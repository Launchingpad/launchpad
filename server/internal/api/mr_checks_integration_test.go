package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/github"
	"devops/internal/gitlab"
	"devops/internal/tracker"
)

// LAUN-85 (T17 ticket 12): MR checks from provider events, GitHub and GitLab.
// Bitbucket's commit status events come with LAUN-84. The fixtures are
// hand-written (internal/github/testdata, internal/gitlab/testdata).
//
// Since decisions-T20 #65 (LAUN-224) every check event answers `processed`,
// whether it changed a link, was older than the stored check or named a sha no
// link has: the answer never says whether an issue matched. What the event did
// is in the deliveries ring (ringRows, the result column).

const (
	prSHA  = "0123456789abcdef0123456789abcdef01234567" // head.sha of pr_opened.json
	newSHA = "aaaaaaaaaaaabbbbbbbbbbbbccccccccdddddddd"
	mrSHA  = "3f2a9c1e5b7d4a6c8e0f1a2b3c4d5e6f7a8b9c0d" // last_commit.id of mr_open.json
)

// ghCheck delivers a check_suite or status fixture. mut changes the payload.
func (g *ghEnv) ghCheck(org, fixture, event, delivery string, mut func(m map[string]any)) resp {
	g.t.Helper()
	return g.ghHook(org, ghSecret, delivery, ghFixture(g.t, fixture, mut), github.HeaderEvent, event)
}

func suiteAt(sha, app, status, conclusion string, minute int) func(map[string]any) {
	return func(m map[string]any) {
		cs := m["check_suite"].(map[string]any)
		cs["head_sha"], cs["status"], cs["conclusion"] = sha, status, conclusion
		cs["app"] = map[string]any{"slug": app, "name": app}
		cs["updated_at"] = fmt.Sprintf("2026-10-20T10:%02d:00Z", minute)
		if conclusion == "" {
			cs["conclusion"] = nil
		}
	}
}

func statusAt(sha, context, state string, minute int) func(map[string]any) {
	return func(m map[string]any) {
		m["sha"], m["context"], m["state"] = sha, context, state
		m["updated_at"] = fmt.Sprintf("2026-10-20T10:%02d:00Z", minute)
	}
}

func checksOf(t *testing.T, e *env, ident string) string {
	t.Helper()
	cr := e.oneCR(ident)
	if cr.Checks == nil {
		return "null"
	}
	return *cr.Checks
}

func (e *env) detailOf(ident string) string {
	e.t.Helper()
	var d string
	if err := e.pool.QueryRow(context.Background(), `SELECT l.checks_detail::text FROM mr_links l JOIN issues i ON i.id = l.issue_id AND i.org_id = l.org_id
		JOIN teams t ON t.id = i.team_id AND t.org_id = i.org_id WHERE l.org_id = $1 AND t.key || '-' || i.number = $2`, e.orgID, ident).Scan(&d); err != nil {
		e.t.Fatal(err)
	}
	return d
}

func TestChecks_GitHub(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{})
	g.setup()
	a := g.createIssue(g.chen, map[string]any{"title": "checks"})
	g.setState(a.Identifier, "Todo")
	if r := g.ghSend("pr_opened.json", ghMut(a.Identifier, 1)); len(r.Linked) != 1 {
		t.Fatalf("open = %+v", r)
	}
	if c := checksOf(t, g.env, a.Identifier); c != "null" {
		t.Fatalf("checks before any event = %s", c)
	}
	issueID := g.issueFull(a.Identifier).ID
	state0 := g.issueFull(a.Identifier).State.Name

	send := func(fixture, event string, mut func(map[string]any)) hookOutcome {
		t.Helper()
		return g.ghResult(g.ghCheck("orbit", fixture, event, uuid.NewString(), mut), http.StatusOK)
	}

	// A suite is queued: pending, and one change event for the Changes block.
	ev0 := g.mrEvents(issueID)
	if r := send("check_suite_requested.json", github.EventCheckSuite, suiteAt(prSHA, "example-ci", "queued", "", 1)); r.Status != tracker.WebhookProcessed || len(r.Linked) != 0 || len(r.Moved) != 0 {
		t.Fatalf("requested = %+v", r)
	}
	if c := checksOf(t, g.env, a.Identifier); c != "pending" {
		t.Fatalf("after requested = %s", c)
	}
	if g.mrEvents(issueID) != ev0+1 {
		t.Fatal("a checks change must emit one issue.updated with changeRequests")
	}
	// A status of another context: still pending; the aggregate did not change, so no event.
	send("status_success.json", github.EventStatus, statusAt(prSHA, "ci/build", "pending", 2))
	if g.mrEvents(issueID) != ev0+1 {
		t.Fatal("an event that leaves the worst state alone must emit nothing")
	}
	// Both finish: success.
	send("check_suite_completed_success.json", github.EventCheckSuite, suiteAt(prSHA, "example-ci", "completed", "success", 5))
	send("status_success.json", github.EventStatus, statusAt(prSHA, "ci/build", "success", 6))
	if c := checksOf(t, g.env, a.Identifier); c != "success" {
		t.Fatalf("both succeeded = %s", c)
	}
	// One failing app plus succeeding apps is failure.
	send("check_suite_completed_failure.json", github.EventCheckSuite, suiteAt(prSHA, "example-lint", "completed", "failure", 8))
	if c := checksOf(t, g.env, a.Identifier); c != "failure" {
		t.Fatalf("one failing app = %s", c)
	}
	// Out of order: an older success of the failing app is dropped (the later one stays).
	before := g.count(`SELECT count(*) FROM events WHERE org_id = $1`, g.orgID)
	if r := send("check_suite_completed_success.json", github.EventCheckSuite, suiteAt(prSHA, "example-lint", "completed", "success", 7)); r.Status != tracker.WebhookProcessed {
		t.Fatalf("older result = %+v, want processed", r)
	}
	if c := checksOf(t, g.env, a.Identifier); c != "failure" {
		t.Fatalf("an older success hid a failure: %s", c)
	}
	if g.count(`SELECT count(*) FROM events WHERE org_id = $1`, g.orgID) != before {
		t.Fatal("a stale check event emitted an event")
	}
	// A later success of that app clears the failure.
	send("check_suite_completed_success.json", github.EventCheckSuite, suiteAt(prSHA, "example-lint", "completed", "success", 9))
	if c := checksOf(t, g.env, a.Identifier); c != "success" {
		t.Fatalf("fixed = %s", c)
	}
	// A status error is a failure.
	send("status_error.json", github.EventStatus, statusAt(prSHA, "ci/build", "error", 10))
	if c := checksOf(t, g.env, a.Identifier); c != "failure" {
		t.Fatalf("status error = %s", c)
	}
	// The ticket was never touched by any of it.
	if g.issueFull(a.Identifier).State.Name != state0 {
		t.Fatal("a check event moved the ticket")
	}
	var checkedAt *string
	if err := g.pool.QueryRow(context.Background(), `SELECT to_char(checks_updated_at AT TIME ZONE 'UTC', 'HH24:MI') FROM mr_links WHERE org_id = $1`, g.orgID).Scan(&checkedAt); err != nil || checkedAt == nil || *checkedAt != "10:10" {
		t.Fatalf("checks_updated_at = %v, %v", checkedAt, err)
	}

	// The delivery key has no PR number; a redelivery changes nothing.
	d := "delivery-checks-1"
	if r := g.ghResult(g.ghCheck("orbit", "status_success.json", github.EventStatus, d, statusAt(prSHA, "ci/build", "success", 11)), http.StatusOK); r.Status != tracker.WebhookProcessed {
		t.Fatalf("first = %+v", r)
	}
	if n := g.count(`SELECT count(*) FROM git_webhook_events WHERE org_id = $1 AND event_uuid = $2 AND event_type = 'checks'`, g.orgID,
		"gh:"+d+":r4242:sha0123456789ab:checks"); n != 1 {
		t.Fatalf("the check delivery key is missing (%d)", n)
	}
	if r := g.ghResult(g.ghCheck("orbit", "status_success.json", github.EventStatus, d, statusAt(prSHA, "ci/build", "failure", 12)), http.StatusOK); r.Status != tracker.WebhookProcessed || g.lastRingResult() != tracker.WebhookDuplicate {
		t.Fatalf("redelivery = %+v, ring %s; want processed, ring duplicate", r, g.lastRingResult())
	}
	if c := checksOf(t, g.env, a.Identifier); c != "success" {
		t.Fatalf("a duplicate changed the checks: %s", c)
	}
	if n := g.count(`SELECT count(*) FROM git_webhook_events WHERE org_id = $1 AND event_uuid LIKE '%:pr%checks'`, g.orgID); n != 0 {
		t.Fatal("a check key carries a PR number")
	}

	// A sha with no link is 200 ignored and keeps nothing (no delivery row, no event).
	rows, evs := g.count(`SELECT count(*) FROM git_webhook_events WHERE org_id = $1`, g.orgID), g.count(`SELECT count(*) FROM events WHERE org_id = $1`, g.orgID)
	if r := send("status_success.json", github.EventStatus, statusAt(newSHA, "ci/build", "failure", 13)); r.Status != tracker.WebhookProcessed {
		t.Fatalf("no link = %+v, want processed", r)
	}
	if g.count(`SELECT count(*) FROM git_webhook_events WHERE org_id = $1`, g.orgID) != rows || g.count(`SELECT count(*) FROM events WHERE org_id = $1`, g.orgID) != evs {
		t.Fatal("an event for a sha with no link kept something")
	}
	if c := checksOf(t, g.env, a.Identifier); c != "success" {
		t.Fatalf("checks = %s", c)
	}

	// A new head sha (a push) resets the checks; the old sha no longer matches.
	g.ghSend("pr_synchronize.json", ghMut(a.Identifier, 14, func(m map[string]any) {
		m["pull_request"].(map[string]any)["head"].(map[string]any)["sha"] = newSHA
	}))
	if c := checksOf(t, g.env, a.Identifier); c != "null" {
		t.Fatalf("after a push the checks are %s, want none", c)
	}
	if d := g.detailOf(a.Identifier); d != "{}" {
		t.Fatalf("checks_detail after a push = %s", d)
	}
	if r := send("status_success.json", github.EventStatus, statusAt(prSHA, "ci/build", "failure", 15)); r.Status != tracker.WebhookProcessed {
		t.Fatalf("old sha = %+v", r)
	}
	send("status_success.json", github.EventStatus, statusAt(newSHA, "ci/build", "pending", 16))
	if c := checksOf(t, g.env, a.Identifier); c != "pending" {
		t.Fatalf("new sha = %s", c)
	}

	// An unknown state asks for nothing; a bad payload is 400; an unmapped repo is 202.
	if r := send("status_success.json", github.EventStatus, statusAt(newSHA, "ci/build", "weird", 17)); r.Status != tracker.WebhookIgnored {
		t.Fatalf("unknown state = %+v", r)
	}
	g.must(http.StatusBadRequest, g.ghCheck("orbit", "status_success.json", github.EventStatus, uuid.NewString(), func(m map[string]any) { delete(m, "sha") }))
	g.must(http.StatusAccepted, g.ghCheck("orbit", "status_success.json", github.EventStatus, uuid.NewString(), func(m map[string]any) {
		m["repository"].(map[string]any)["id"] = 9999
	})) // unmapped: 202 like the other events
	if n := g.count(`SELECT count(*) FROM git_webhook_events WHERE org_id = $1 AND repo_key = '9999'`, g.orgID); n != 0 {
		t.Fatal("an unmapped check event kept a row")
	}
	// A bad signature is 401 and writes no check.
	g.must(http.StatusUnauthorized, g.ghHook("orbit", "wrong-secret", uuid.NewString(), ghFixture(t, "status_success.json", statusAt(newSHA, "ci/build", "failure", 20)), github.HeaderEvent, github.EventStatus))
	if c := checksOf(t, g.env, a.Identifier); c != "pending" {
		t.Fatalf("checks = %s", c)
	}
	// Host text is untrusted: a long, odd check name is cut and stored as plain text.
	send("status_success.json", github.EventStatus, func(m map[string]any) {
		statusAt(newSHA, "evil\n"+strings.Repeat("x", 300)+"<script>", "success", 21)(m)
	})
	if d := g.detailOf(a.Identifier); strings.Contains(d, "\\n") || strings.Contains(d, strings.Repeat("x", 101)) {
		t.Fatalf("check name not cleaned: %s", d)
	}
}

// Cross-org: a check event of one org never touches another org's link, even
// with the same repository id and head sha.
func TestChecks_CrossOrg(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{})
	g.setup()
	org2 := g.newOrg("acme", "ACME")
	g.ghInsert(ghRow{org: org2, secret: "acme-secret-0123456789abcdef0123456789", enabled: true})
	admin2 := g.newUser("acmeadmin")
	g.addMember(org2, admin2.ID, "admin")
	tok2, _ := g.newToken(admin2.ID, tokenOpts{pin: &org2})
	var theirs tracker.Issue
	g.must(http.StatusCreated, g.do("POST", "/api/v1/orgs/acme/issues", tok2, map[string]any{"team": "ACME", "title": "theirs"})).json(t, &theirs)
	mine := g.createIssue(g.chen, map[string]any{"title": "mine"})
	g.ghSend("pr_opened.json", ghMut(mine.Identifier, 1))
	body := ghFixture(t, "pr_opened.json", ghMut(theirs.Identifier, 1))
	g.must(http.StatusOK, g.ghHook("acme", "acme-secret-0123456789abcdef0123456789", uuid.NewString(), body))

	g.ghResult(g.ghCheck("orbit", "status_success.json", github.EventStatus, uuid.NewString(), statusAt(prSHA, "ci/build", "failure", 3)), http.StatusOK)
	if c := checksOf(t, g.env, mine.Identifier); c != "failure" {
		t.Fatalf("own link = %s", c)
	}
	var other string
	if err := g.pool.QueryRow(context.Background(), `SELECT checks FROM mr_links WHERE org_id = $1`, org2).Scan(&other); err != nil || other != "none" {
		t.Fatalf("another org's link: %q, %v", other, err)
	}
	// And the other org's secret cannot reach this org's route.
	g.must(http.StatusUnauthorized, g.ghHook("orbit", "acme-secret-0123456789abcdef0123456789", uuid.NewString(),
		ghFixture(t, "status_success.json", statusAt(prSHA, "ci/build", "success", 4)), github.HeaderEvent, github.EventStatus))
}

func glPipeline(e *env, fixture, uuidv string, mut func(a map[string]any)) resp {
	e.t.Helper()
	return e.hook("orbit", glSecret, uuidv, glFixture(e.t, fixture, mut), gitlab.HeaderEvent, gitlab.PipelineHook)
}

func pipelineAt(sha, name, status string, created, finished string) func(map[string]any) {
	return func(a map[string]any) {
		a["sha"], a["status"], a["created_at"] = sha, status, created
		if name == "" {
			delete(a, "name")
		} else {
			a["name"] = name
		}
		if finished == "" {
			a["finished_at"] = nil
		} else {
			a["finished_at"] = finished
		}
	}
}

func TestChecks_GitLab(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	is := e.createIssue(e.chen, map[string]any{"title": "gitlab checks"})
	e.setState(is.Identifier, "Todo")
	e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_open.json", naming(is.Identifier, ""))), http.StatusOK)
	if c := checksOf(t, e, is.Identifier); c != "null" {
		t.Fatalf("checks before = %s", c)
	}
	state0 := e.issueFull(is.Identifier).State.Name
	issueID := e.issueFull(is.Identifier).ID
	send := func(uuidv string, mut func(map[string]any)) hookOutcome {
		t.Helper()
		return e.hookResult(glPipeline(e, "pipeline_success.json", uuidv, mut), http.StatusOK)
	}
	ev0 := e.mrEvents(issueID)
	send(uuid.NewString(), pipelineAt(mrSHA, "", "running", "2026-10-24 10:00:00 UTC", ""))
	if c := checksOf(t, e, is.Identifier); c != "pending" {
		t.Fatalf("running = %s", c)
	}
	if e.mrEvents(issueID) != ev0+1 {
		t.Fatal("a checks change must emit one issue.updated")
	}
	send(uuid.NewString(), pipelineAt(mrSHA, "", "success", "2026-10-24 10:00:00 UTC", "2026-10-24 10:05:00 UTC"))
	if c := checksOf(t, e, is.Identifier); c != "success" {
		t.Fatalf("success = %s", c)
	}
	// A second pipeline (another name) fails: failure wins.
	send(uuid.NewString(), pipelineAt(mrSHA, "Deploy review", "failed", "2026-10-24 10:01:00 UTC", "2026-10-24 10:06:00 UTC"))
	if c := checksOf(t, e, is.Identifier); c != "failure" {
		t.Fatalf("one failing pipeline = %s", c)
	}
	// Out of order: the running event of the first pipeline arrives late; and an older success of the failing one.
	if r := send(uuid.NewString(), pipelineAt(mrSHA, "", "running", "2026-10-24 10:00:00 UTC", "")); r.Status != tracker.WebhookProcessed {
		t.Fatalf("late running event = %+v, want processed", r)
	}
	if r := send(uuid.NewString(), pipelineAt(mrSHA, "Deploy review", "success", "2026-10-24 09:00:00 UTC", "2026-10-24 10:02:00 UTC")); r.Status != tracker.WebhookProcessed {
		t.Fatalf("older result = %+v, want processed", r)
	}
	if c := checksOf(t, e, is.Identifier); c != "failure" {
		t.Fatalf("a late event hid the failure: %s", c)
	}
	// Retry of the failing pipeline later: success.
	send(uuid.NewString(), pipelineAt(mrSHA, "Deploy review", "success", "2026-10-24 10:10:00 UTC", "2026-10-24 10:12:00 UTC"))
	if c := checksOf(t, e, is.Identifier); c != "success" {
		t.Fatalf("retry = %s", c)
	}
	if e.issueFull(is.Identifier).State.Name != state0 {
		t.Fatal("a pipeline moved the ticket")
	}

	// The key has no MR iid; both header forms record a key; a repeat is a duplicate.
	u := "uuid-pipe-1"
	r := e.hookResult(e.hook("orbit", glSecret, u, glFixture(t, "pipeline_success.json", pipelineAt(mrSHA, "", "failed", "2026-10-24 10:20:00 UTC", "2026-10-24 10:21:00 UTC")),
		gitlab.HeaderEvent, gitlab.PipelineHook, gitlab.HeaderIdempotencyKey, "ik-pipe-1"), http.StatusOK)
	if r.Status != tracker.WebhookProcessed {
		t.Fatalf("first = %+v", r)
	}
	for _, k := range []string{"gl:ik:ik-pipe-1:p42:sha3f2a9c1e5b7d:checks", "gl:ev:" + u + ":p42:sha3f2a9c1e5b7d:checks"} {
		if n := e.count(`SELECT count(*) FROM git_webhook_events WHERE org_id = $1 AND event_uuid = $2`, e.orgID, k); n != 1 {
			t.Fatalf("key %s missing", k)
		}
	}
	r = e.hookResult(e.hook("orbit", glSecret, u, glFixture(t, "pipeline_success.json", pipelineAt(mrSHA, "", "success", "2026-10-24 10:30:00 UTC", "2026-10-24 10:31:00 UTC")),
		gitlab.HeaderEvent, gitlab.PipelineHook), http.StatusOK)
	if r.Status != tracker.WebhookProcessed || e.lastRingResult() != tracker.WebhookDuplicate {
		t.Fatalf("repeat = %+v, ring %s; want processed, ring duplicate", r, e.lastRingResult())
	}
	if c := checksOf(t, e, is.Identifier); c != "failure" {
		t.Fatalf("a duplicate changed the checks: %s", c)
	}

	// A sha with no link: 200 ignored, keeps nothing.
	rows, evs := e.count(`SELECT count(*) FROM git_webhook_events WHERE org_id = $1`, e.orgID), e.count(`SELECT count(*) FROM events WHERE org_id = $1`, e.orgID)
	if r := send(uuid.NewString(), pipelineAt(newSHA, "", "failed", "2026-10-24 11:00:00 UTC", "2026-10-24 11:01:00 UTC")); r.Status != tracker.WebhookProcessed {
		t.Fatalf("no link = %+v", r)
	}
	if e.count(`SELECT count(*) FROM git_webhook_events WHERE org_id = $1`, e.orgID) != rows || e.count(`SELECT count(*) FROM events WHERE org_id = $1`, e.orgID) != evs {
		t.Fatal("a pipeline for a sha with no link kept something")
	}
	// An MR update with a new head sha resets the checks, and says so on the
	// change feed (LAUN-97: the web needs no extra event for it).
	evPush := e.mrEvents(issueID)
	e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_update.json", func(a map[string]any) {
		naming(is.Identifier, "")(a)
		a["updated_at"] = "2026-10-24 12:00:00 UTC"
		a["last_commit"] = map[string]any{"id": newSHA}
	})), http.StatusOK)
	if c := checksOf(t, e, is.Identifier); c != "null" {
		t.Fatalf("after a push = %s", c)
	}
	if e.mrEvents(issueID) != evPush+1 {
		t.Fatal("a new head sha that clears the checks must emit one issue.updated")
	}
	if r := send(uuid.NewString(), pipelineAt(mrSHA, "", "failed", "2026-10-24 12:05:00 UTC", "2026-10-24 12:06:00 UTC")); r.Status != tracker.WebhookProcessed {
		t.Fatalf("old sha = %+v", r)
	}
	// An unknown status asks for nothing; a wrong token is 401 before the body is read.
	if r := send(uuid.NewString(), pipelineAt(newSHA, "", "surprise", "2026-10-24 12:07:00 UTC", "")); r.Status != tracker.WebhookIgnored {
		t.Fatalf("unknown status = %+v", r)
	}
	e.must(http.StatusUnauthorized, e.hook("orbit", "wrong", uuid.NewString(), []byte("{not json"), gitlab.HeaderEvent, gitlab.PipelineHook))
	e.must(http.StatusBadRequest, glPipeline(e, "pipeline_success.json", uuid.NewString(), func(a map[string]any) { delete(a, "sha") }))
}

func TestChecks_GitLabCrossOrg(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	org2 := e.newOrg("acme", "ACME")
	admin2 := e.newUser("acmeadmin")
	e.addMember(org2, admin2.ID, "admin")
	tok2, _ := e.newToken(admin2.ID, tokenOpts{pin: &org2})
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/acme/integrations", tok2,
		map[string]any{"provider": "gitlab", "baseUrl": glBaseURL, "secret": "acme-gl-secret-0123456789abcdefABCDEF"}))
	mine := e.createIssue(e.chen, map[string]any{"title": "mine"})
	e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_open.json", naming(mine.Identifier, ""))), http.StatusOK)
	e.hookResult(glPipeline(e, "pipeline_success.json", uuid.NewString(), pipelineAt(mrSHA, "", "failed", "2026-10-24 10:00:00 UTC", "2026-10-24 10:01:00 UTC")), http.StatusOK)
	if c := checksOf(t, e, mine.Identifier); c != "failure" {
		t.Fatalf("own = %s", c)
	}
	// The other org's token does not open this org's route, and its own org has no link.
	e.must(http.StatusUnauthorized, e.hook("orbit", "acme-gl-secret-0123456789abcdefABCDEF", uuid.NewString(),
		glFixture(t, "pipeline_success.json", nil), gitlab.HeaderEvent, gitlab.PipelineHook))
}

// LAUN-96: the checks path takes the issue rows before the link rows too. An
// MR that names two issues whose link ids run the other way from their issue
// ids: linkAndMove holds issue A and link La and then wants issue B and link
// Lb (issue-id order); the check event used to lock the links in link-id order
// (Lb, then La) and no issue row, so the two crossed. This test is
// linkAndMove, step by step, with a check event in flight.
func TestChecks_LockOrderWithLinkAndMoveOnTwoIssues(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{})
	g.setup()
	x := g.createIssue(g.chen, map[string]any{"title": "first"})
	y := g.createIssue(g.chen, map[string]any{"title": "second"})
	a, b := x, y // a has the lower issue id
	if strings.Compare(a.ID.String(), b.ID.String()) > 0 {
		a, b = b, a
	}
	g.setState(a.Identifier, "Todo")
	g.setState(b.Identifier, "Todo")
	// b is linked first, then the title names both: b's link id is lower than a's.
	g.ghSend("pr_opened.json", ghMut(b.Identifier, 1))
	g.ghSend("pr_reopened.json", ghMut(b.Identifier, 2, func(m map[string]any) {
		m["pull_request"].(map[string]any)["title"] = a.Identifier + " " + b.Identifier + ": both"
	}))
	var la, lb uuid.UUID
	for ident, dst := range map[uuid.UUID]*uuid.UUID{a.ID: &la, b.ID: &lb} {
		if err := g.pool.QueryRow(context.Background(), `SELECT id FROM mr_links WHERE issue_id = $1`, ident).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Compare(lb.String(), la.String()) >= 0 {
		t.Fatalf("setup: link of b (%s) must have the lower id than the link of a (%s)", lb, la)
	}

	ctx := context.Background()
	tx, err := g.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, q := range []struct {
		sql string
		id  uuid.UUID
	}{
		{`SELECT 1 FROM issues WHERE id = $1 FOR UPDATE`, a.ID},
		{`SELECT 1 FROM mr_links WHERE id = $1 FOR UPDATE`, la},
	} {
		if _, err := tx.Exec(ctx, q.sql, q.id); err != nil {
			t.Fatal(err)
		}
	}
	body := ghFixture(t, "check_suite_requested.json", suiteAt(prSHA, "example-ci", "queued", "", 5))
	done := make(chan resp, 1)
	go func() {
		done <- g.ghHook("orbit", ghSecret, uuid.NewString(), body, github.HeaderEvent, github.EventCheckSuite)
	}()
	waitForLockWaiter(t, g.env)
	for _, q := range []struct {
		sql string
		id  uuid.UUID
	}{
		{`SELECT 1 FROM issues WHERE id = $1 FOR UPDATE`, b.ID},
		{`SELECT 1 FROM mr_links WHERE id = $1 FOR UPDATE`, lb},
	} {
		if _, err := tx.Exec(ctx, q.sql, q.id); err != nil {
			t.Fatalf("linkAndMove's second step failed while a check waits (deadlock): %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.status != http.StatusOK {
		t.Fatalf("the check answered %d: %s", r.status, r.body)
	}
	for _, id := range []string{a.Identifier, b.Identifier} {
		if c := checksOf(t, g.env, id); c != "pending" {
			t.Fatalf("%s checks = %s, want pending", id, c)
		}
	}
}

// --- LAUN-97 ---------------------------------------------------------------

func (e *env) ringRows() int {
	e.t.Helper()
	return e.count(`SELECT count(*) FROM git_webhook_deliveries WHERE org_id = $1`, e.orgID)
}

func glTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04:05 UTC") }

// A check event for a commit no link has writes no ring row (the newest 100
// would fill with ignored rows); a check event that reaches a link does, and so
// does a stale one.
func TestChecks_NoLinkWritesNoRingRow(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	is := e.createIssue(e.chen, map[string]any{"title": "ring spam"})
	e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_open.json", naming(is.Identifier, ""))), http.StatusOK)
	send := func(sha, status, created, finished string) hookOutcome {
		t.Helper()
		return e.hookResult(glPipeline(e, "pipeline_success.json", uuid.NewString(), pipelineAt(sha, "", status, created, finished)), http.StatusOK)
	}
	before := e.ringRows()
	for i := 0; i < 5; i++ {
		if r := send(newSHA, "failed", "2026-10-24 11:00:00 UTC", "2026-10-24 11:01:00 UTC"); r.Status != tracker.WebhookProcessed {
			t.Fatalf("no link = %+v", r)
		}
	}
	if n := e.ringRows(); n != before {
		t.Fatalf("5 events for a sha with no link wrote %d ring rows", n-before)
	}
	if r := send(mrSHA, "running", "2026-10-24 10:00:00 UTC", ""); r.Status != tracker.WebhookProcessed {
		t.Fatalf("linked = %+v", r)
	}
	if n := e.ringRows(); n != before+1 {
		t.Fatalf("a linked event wrote %d ring rows, want 1", n-before)
	}
	if r := send(mrSHA, "running", "2026-10-24 09:00:00 UTC", ""); r.Status != tracker.WebhookProcessed {
		t.Fatalf("stale = %+v", r)
	}
	if n := e.ringRows(); n != before+2 {
		t.Fatalf("a stale event on a link wrote %d ring rows, want 1", n-before-1)
	}
	var got string
	if err := e.pool.QueryRow(context.Background(), `SELECT result FROM git_webhook_deliveries WHERE org_id = $1 ORDER BY received_at DESC, id DESC LIMIT 1`, e.orgID).Scan(&got); err != nil || got != tracker.WebhookIgnored {
		t.Fatalf("last ring row = %q %v", got, err)
	}
}

func TestChecks_GitHubNoLinkWritesNoRingRow(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{})
	g.setup()
	a := g.createIssue(g.chen, map[string]any{"title": "gh ring"})
	g.ghSend("pr_opened.json", ghMut(a.Identifier, 1))
	before := g.ringRows()
	if r := g.ghResult(g.ghCheck("orbit", "status_success.json", github.EventStatus, uuid.NewString(), statusAt(newSHA, "ci/build", "failure", 13)), http.StatusOK); r.Status != tracker.WebhookProcessed {
		t.Fatalf("no link = %+v", r)
	}
	if g.ringRows() != before {
		t.Fatal("a GitHub check event for a sha with no link wrote a ring row")
	}
	g.ghResult(g.ghCheck("orbit", "status_success.json", github.EventStatus, uuid.NewString(), statusAt(prSHA, "ci/build", "failure", 14)), http.StatusOK)
	if g.ringRows() != before+1 {
		t.Fatal("a linked check event wrote no ring row")
	}
}

// A host time far in the future cannot block later results of that check name.
func TestChecks_FutureHostTimeIsClamped(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	is := e.createIssue(e.chen, map[string]any{"title": "clock"})
	e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_open.json", naming(is.Identifier, ""))), http.StatusOK)
	now := time.Now()
	send := func(status string, finished time.Time) hookOutcome {
		t.Helper()
		return e.hookResult(glPipeline(e, "pipeline_success.json", uuid.NewString(),
			pipelineAt(mrSHA, "", status, glTime(now.Add(-time.Hour)), glTime(finished))), http.StatusOK)
	}
	if r := send("failed", time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)); r.Status != tracker.WebhookProcessed {
		t.Fatalf("future failure = %+v", r)
	}
	if c := checksOf(t, e, is.Identifier); c != "failure" {
		t.Fatalf("checks = %s", c)
	}
	var at time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT (checks_detail->'pipeline'->>'at')::timestamptz FROM mr_links WHERE org_id = $1`, e.orgID).Scan(&at); err != nil {
		t.Fatal(err)
	}
	if at.After(time.Now().Add(time.Minute)) {
		t.Fatalf("stored time %v is in the future", at)
	}
	// The retry's real time, within the skew, is newer than the clamped one.
	if r := send("success", time.Now().Add(2*time.Minute)); r.Status != tracker.WebhookProcessed {
		t.Fatalf("retry = %+v, a future time blocked it", r)
	}
	if c := checksOf(t, e, is.Identifier); c != "success" {
		t.Fatalf("checks after the retry = %s", c)
	}
}
