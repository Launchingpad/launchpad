package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/github"
	"devops/internal/gitlab"
	"devops/internal/tracker"
)

// LAUN-83 (T17 ticket 11): MR state on the ticket. The Changes block
// (changeRequests), the review events, the refresh of a plain update, the
// stale-event rule, the timeline kinds, the live events, the hook refresh and
// the backfill. The fixtures are hand-written (see internal/github/testdata).

// ghMut makes a GitHub fixture name ident in its branch and title and gives
// it the host time 2026-10-20 09:<minute>. Extra mutators run last.
func ghMut(ident string, minute int, extra ...func(m map[string]any)) func(map[string]any) {
	return func(m map[string]any) {
		ghNaming(ident, "")(m)
		m["pull_request"].(map[string]any)["updated_at"] = fmt.Sprintf("2026-10-20T09:%02d:00Z", minute)
		for _, f := range extra {
			f(m)
		}
	}
}

// ghSend delivers a pull_request fixture (or a review, with event set).
func (g *ghEnv) ghSend(fixture string, mut func(map[string]any), event ...string) hookOutcome {
	g.t.Helper()
	var hdr []string
	if len(event) > 0 {
		hdr = []string{github.HeaderEvent, event[0]}
	}
	return g.ghResult(g.ghHook("orbit", ghSecret, uuid.NewString(), ghFixture(g.t, fixture, mut), hdr...), http.StatusOK)
}

func (g *ghEnv) ghReview(fixture string, mut func(map[string]any)) hookOutcome {
	g.t.Helper()
	return g.ghSend(fixture, mut, github.EventPullRequestReview)
}

func (e *env) changeRequests(ident string) []tracker.ChangeRequest {
	e.t.Helper()
	var is tracker.Issue
	e.must(http.StatusOK, e.do("GET", "/api/v1/orgs/orbit/issues/"+ident+"?include=changeRequests", e.chen, nil)).json(e.t, &is)
	return is.ChangeRequests
}

func (e *env) oneCR(ident string) tracker.ChangeRequest {
	e.t.Helper()
	crs := e.changeRequests(ident)
	if len(crs) != 1 {
		e.t.Fatalf("%s: %d change requests, want 1: %+v", ident, len(crs), crs)
	}
	return crs[0]
}

func (e *env) issueActivity(ident string) []tracker.Activity {
	e.t.Helper()
	return e.activity("/api/v1/orgs/orbit/issues/" + ident + "/activity").Items
}

// mrEvents counts the issue.updated events that mark a change-request change.
func (e *env) mrEvents(issue uuid.UUID) int {
	e.t.Helper()
	return e.count(`SELECT count(*) FROM events WHERE org_id = $1 AND type = 'issue.updated' AND entity_id = $2
		AND payload->'changes' ? 'changeRequests'`, e.orgID, issue)
}

// quiet is a snapshot without the delivery-key count (a stale event still
// records its delivery key).
func (e *env) quiet(ident string) glSnapshot {
	s := e.glSnapshot(ident)
	s.webhookEvents = 0
	return s
}

// AC: the block shows draft, open, approved, merged and closed. An approval
// does not move the ticket. The timeline gets mrLinked, mrMerged and mrClosed.
func TestMRState_ChangesBlockShowsAllFiveStates(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{})
	integ := g.setup()
	a := g.createIssue(g.chen, map[string]any{"title": "five states"})
	g.setState(a.Identifier, "Todo")

	r := g.ghSend("pr_opened_draft.json", ghMut(a.Identifier, 1))
	if len(r.Linked) != 1 || len(r.Moved) != 0 {
		t.Fatalf("draft open = %+v", r)
	}
	cr := g.oneCR(a.Identifier)
	if cr.State != tracker.CRDraft || cr.Provider != "github" || cr.RepoPath != "example-org/example-app" || cr.Number != 17 ||
		cr.Author != "example-dev" || !strings.HasPrefix(cr.Title, a.Identifier) || cr.Checks != nil || cr.ChangesRequested ||
		cr.URL != "https://github.com/example-org/example-app/pull/17" || cr.IntegrationID != integ {
		t.Fatalf("draft block = %+v (integration %s)", cr, integ)
	}

	r = g.ghSend("pr_ready_for_review.json", ghMut(a.Identifier, 2))
	if len(r.Moved) != 1 {
		t.Fatalf("ready = %+v, want a move to In Review", r)
	}
	if cr = g.oneCR(a.Identifier); cr.State != tracker.CRopen {
		t.Fatalf("after ready: %+v", cr)
	}

	// An approval sets the review and moves nothing: put the ticket back first.
	g.setState(a.Identifier, "In Progress")
	before := g.quiet(a.Identifier)
	r = g.ghReview("review_approved.json", ghMut(a.Identifier, 3))
	if r.Status != tracker.WebhookProcessed || len(r.Moved) != 0 || len(r.Linked) != 0 {
		t.Fatalf("approval = %+v", r)
	}
	after := g.quiet(a.Identifier)
	if after.state != "In Progress" || after.stateChanges != before.stateChanges || after.version != before.version || after.attachments != before.attachments {
		t.Fatalf("an approval moved or changed the ticket:\n before %+v\n after  %+v", before, after)
	}
	if cr = g.oneCR(a.Identifier); cr.State != tracker.CRApproved {
		t.Fatalf("after approval: %+v", cr)
	}

	g.ghReview("review_changes_requested.json", ghMut(a.Identifier, 4))
	if cr = g.oneCR(a.Identifier); cr.State != tracker.CRopen || !cr.ChangesRequested {
		t.Fatalf("after changes requested: %+v, want open with the flag", cr)
	}
	g.ghReview("review_dismissed.json", ghMut(a.Identifier, 5))
	if cr = g.oneCR(a.Identifier); cr.State != tracker.CRopen || cr.ChangesRequested {
		t.Fatalf("after dismissal: %+v", cr)
	}
	// A comment review asks for nothing.
	if r = g.ghReview("review_commented.json", ghMut(a.Identifier, 6)); r.Status != tracker.WebhookIgnored {
		t.Fatalf("comment review = %+v, want ignored", r)
	}

	// Draft wins over an approval; ready shows the approval again.
	g.ghReview("review_approved.json", ghMut(a.Identifier, 7))
	g.ghSend("pr_converted_to_draft.json", ghMut(a.Identifier, 8))
	if cr = g.oneCR(a.Identifier); cr.State != tracker.CRDraft {
		t.Fatalf("after converted_to_draft: %+v", cr)
	}
	g.ghSend("pr_ready_for_review.json", ghMut(a.Identifier, 9))
	if cr = g.oneCR(a.Identifier); cr.State != tracker.CRApproved {
		t.Fatalf("after ready with an approval: %+v", cr)
	}

	g.ghSend("pr_closed_merged.json", ghMut(a.Identifier, 10))
	if cr = g.oneCR(a.Identifier); cr.State != tracker.CRMerged {
		t.Fatalf("after merge: %+v", cr)
	}
	if st := g.issueFull(a.Identifier).State.Name; st != "In Testing" {
		t.Fatalf("after merge the ticket is %q, want In Testing", st)
	}

	// The fifth state: closed.
	b := g.createIssue(g.chen, map[string]any{"title": "closed one"})
	g.ghSend("pr_opened.json", ghMut(b.Identifier, 1, func(m map[string]any) { m["number"], m["pull_request"].(map[string]any)["number"] = 18, 18 }))
	g.ghSend("pr_closed_unmerged.json", ghMut(b.Identifier, 2, func(m map[string]any) { m["number"], m["pull_request"].(map[string]any)["number"] = 18, 18 }))
	if cr = g.oneCR(b.Identifier); cr.State != tracker.CRClosed || cr.Number != 18 {
		t.Fatalf("closed block = %+v", cr)
	}

	// The timeline: linked, merged and closed rows from the provider, no actor.
	kinds := actKinds(g.issueActivity(a.Identifier))
	if kinds["mrLinked"] != 1 || kinds["mrMerged"] != 1 || kinds["mrClosed"] != 0 {
		t.Fatalf("A activity kinds = %v", kinds)
	}
	for _, act := range g.issueActivity(a.Identifier) {
		if act.Source != "github" || act.Actor != nil {
			t.Fatalf("activity %+v: want source github and no actor", act)
		}
		var to struct {
			Provider, RepoPath, URL, Title string
			Number                         int64
		}
		if err := json.Unmarshal(act.To, &to); err != nil || to.Provider != "github" || to.Number != 17 || to.URL == "" || to.RepoPath != "example-org/example-app" {
			t.Fatalf("activity to = %s (%v)", act.To, err)
		}
	}
	if kinds = actKinds(g.issueActivity(b.Identifier)); kinds["mrLinked"] != 1 || kinds["mrClosed"] != 1 || kinds["mrMerged"] != 0 {
		t.Fatalf("B activity kinds = %v", kinds)
	}

	// A live event reaches the change feed: one issue.updated per change, with the marker.
	if n := g.mrEvents(a.ID); n < 8 {
		t.Fatalf("issue.updated change-request events for A = %d, want one per change (at least 8)", n)
	}
	if n := g.count(`SELECT count(*) FROM events WHERE org_id = $1 AND type = 'issue.updated' AND entity_id = $2 AND actor_id IS NULL
		AND payload->>'source' = 'github' AND payload->'mergeRequest'->>'number' = '17' AND payload->'changes' ? 'changeRequests'`, g.orgID, a.ID); n < 8 {
		t.Fatalf("events carry source and mergeRequest: %d", n)
	}
	// The comment review, which changed nothing, emitted nothing of its own.
}

// AC: an event older than provider_updated_at writes and moves nothing: a
// delayed older closed, a delayed older opened.
func TestMRState_StaleEventsWriteAndMoveNothing(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{})
	g.setup()
	c := g.createIssue(g.chen, map[string]any{"title": "stale events"})
	g.setState(c.Identifier, "Todo")
	g.ghSend("pr_opened.json", ghMut(c.Identifier, 10))
	if cr := g.oneCR(c.Identifier); cr.State != tracker.CRopen {
		t.Fatalf("open = %+v", cr)
	}

	// A delayed OLDER closed: nothing changes.
	before := g.quiet(c.Identifier)
	r := g.ghSend("pr_closed_unmerged.json", ghMut(c.Identifier, 5))
	if r.Status != tracker.WebhookProcessed || len(r.Moved) != 0 {
		t.Fatalf("older closed = %+v", r)
	}
	if after := g.quiet(c.Identifier); after != before {
		t.Fatalf("an older closed wrote something:\n before %+v\n after  %+v", before, after)
	}
	if cr := g.oneCR(c.Identifier); cr.State != tracker.CRopen {
		t.Fatalf("an older closed closed the link: %+v", cr)
	}
	if actKinds(g.issueActivity(c.Identifier))["mrClosed"] != 0 {
		t.Fatal("an older closed wrote a timeline row")
	}
	// An older review and an older update change nothing either.
	g.ghReview("review_approved.json", ghMut(c.Identifier, 6))
	g.ghSend("pr_edited.json", ghMut(c.Identifier, 7, func(m map[string]any) { m["pull_request"].(map[string]any)["title"] = c.Identifier + ": OLD TITLE" }))
	if cr := g.oneCR(c.Identifier); cr.State != tracker.CRopen || strings.Contains(cr.Title, "OLD TITLE") {
		t.Fatalf("an older review or edit changed the block: %+v", cr)
	}

	// The real close, then a delayed older opened and a delayed older reopened.
	g.ghSend("pr_closed_unmerged.json", ghMut(c.Identifier, 20))
	if cr := g.oneCR(c.Identifier); cr.State != tracker.CRClosed {
		t.Fatalf("close = %+v", cr)
	}
	g.setState(c.Identifier, "Todo") // the MR was abandoned; a person moved the ticket back
	before = g.quiet(c.Identifier)
	for _, fx := range []string{"pr_opened.json", "pr_reopened.json", "pr_ready_for_review.json"} {
		r = g.ghSend(fx, ghMut(c.Identifier, 15))
		if len(r.Moved) != 0 {
			t.Fatalf("a delayed older %s moved the ticket: %+v", fx, r)
		}
		if after := g.quiet(c.Identifier); after != before {
			t.Fatalf("a delayed older %s wrote something:\n before %+v\n after  %+v", fx, before, after)
		}
		if cr := g.oneCR(c.Identifier); cr.State != tracker.CRClosed {
			t.Fatalf("a delayed older %s changed the state: %+v", fx, cr)
		}
	}
	// A newer reopen opens it again and moves the ticket.
	r = g.ghSend("pr_reopened.json", ghMut(c.Identifier, 30))
	if len(r.Moved) != 1 {
		t.Fatalf("a newer reopen = %+v", r)
	}
	if cr := g.oneCR(c.Identifier); cr.State != tracker.CRopen {
		t.Fatalf("reopen = %+v", cr)
	}
	// An equal time is accepted (hosts give second resolution).
	g.ghSend("pr_closed_unmerged.json", ghMut(c.Identifier, 30))
	if cr := g.oneCR(c.Identifier); cr.State != tracker.CRClosed {
		t.Fatalf("an equal-time close was refused: %+v", cr)
	}

	// A merge is a fact: delayed past a newer update it still merges (state only).
	d := g.createIssue(g.chen, map[string]any{"title": "late merge"})
	g.setState(d.Identifier, "Todo")
	num := func(m map[string]any) { m["number"], m["pull_request"].(map[string]any)["number"] = 19, 19 }
	g.ghSend("pr_opened.json", ghMut(d.Identifier, 40, num))
	g.ghSend("pr_edited.json", ghMut(d.Identifier, 50, num, func(m map[string]any) { m["pull_request"].(map[string]any)["title"] = d.Identifier + ": newer title" }))
	r = g.ghSend("pr_closed_merged.json", ghMut(d.Identifier, 45, num))
	if len(r.Moved) != 1 {
		t.Fatalf("a delayed merge = %+v, want the move to In Testing", r)
	}
	if cr := g.oneCR(d.Identifier); cr.State != tracker.CRMerged || !strings.Contains(cr.Title, "newer title") {
		t.Fatalf("a delayed merge: %+v, want merged with the newer title kept", cr)
	}
}

// AC: an issue with no MR is unchanged in the response; the block has no
// heading to draw (the key is absent).
func TestMRState_IssueWithoutMRIsUnchanged(t *testing.T) {
	e := newEnv(t)
	is := e.createIssue(e.chen, map[string]any{"title": "no MR"})
	for _, q := range []string{"", "?include=changeRequests", "?include=attachments,changeRequests"} {
		r := e.must(http.StatusOK, e.do("GET", "/api/v1/orgs/orbit/issues/"+is.Identifier+q, e.chen, nil))
		var m map[string]json.RawMessage
		r.json(t, &m)
		if _, ok := m["changeRequests"]; ok {
			t.Fatalf("%q: changeRequests is in the answer of an issue with no MR: %s", q, r.body)
		}
	}
	// Without the include the key is absent even for a linked issue (the web asks for it).
	e.must(http.StatusBadRequest, e.do("GET", "/api/v1/orgs/orbit/issues/"+is.Identifier+"?include=nope", e.chen, nil))
}

// A plain update refreshes the link row (title, URL, branch, head sha, draft)
// and a new head sha resets the checks. It links nothing and moves nothing.
func TestMRState_UpdateRefreshesTheLink(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{})
	g.setup()
	d := g.createIssue(g.chen, map[string]any{"title": "refresh"})
	g.setState(d.Identifier, "Todo")
	g.ghSend("pr_opened.json", ghMut(d.Identifier, 1))
	if n := g.count(`SELECT count(*) FROM mr_links WHERE issue_id = $1 AND head_sha = '0123456789abcdef0123456789abcdef01234567' AND author = 'example-dev'
		AND repo_path = 'example-org/example-app' AND provider_updated_at IS NOT NULL AND review = 'none' AND checks = 'none'`, d.ID); n != 1 {
		t.Fatalf("the open did not fill the new columns (%d rows)", n)
	}
	// Checks arrive with a later ticket; plant some to see the reset.
	if _, err := g.pool.Exec(context.Background(), `UPDATE mr_links SET checks = 'failure', checks_detail = '{"ci":{"state":"failure"}}', checks_updated_at = now() WHERE issue_id = $1`, d.ID); err != nil {
		t.Fatal(err)
	}
	g.setState(d.Identifier, "In Progress")
	same := func(m map[string]any) { m["pull_request"].(map[string]any)["title"] = d.Identifier + ": Fix, reworded" }
	r := g.ghSend("pr_edited.json", ghMut(d.Identifier, 2, same))
	if r.Status != tracker.WebhookProcessed || len(r.Linked) != 0 || len(r.Moved) != 0 {
		t.Fatalf("edited = %+v", r)
	}
	cr := g.oneCR(d.Identifier)
	if !strings.Contains(cr.Title, "reworded") || cr.Checks == nil || *cr.Checks != "failure" {
		t.Fatalf("after an edit with the same head sha: %+v, want the new title and the checks kept", cr)
	}
	g.ghSend("pr_synchronize.json", ghMut(d.Identifier, 3, func(m map[string]any) {
		m["pull_request"].(map[string]any)["head"].(map[string]any)["sha"] = "fedcba9876543210fedcba9876543210fedcba98"
	}))
	cr = g.oneCR(d.Identifier)
	if cr.Checks != nil {
		t.Fatalf("a new head sha must reset the checks: %+v", cr)
	}
	if n := g.count(`SELECT count(*) FROM mr_links WHERE issue_id = $1 AND head_sha = 'fedcba9876543210fedcba9876543210fedcba98' AND checks_detail = '{}'::jsonb AND checks_updated_at IS NULL`, d.ID); n != 1 {
		t.Fatalf("the head sha / checks reset not stored (%d)", n)
	}
	if st := g.issueFull(d.Identifier).State.Name; st != "In Progress" {
		t.Fatalf("an update moved the ticket to %q", st)
	}
	// An update of an MR no issue is linked to: nothing written, and the answer
	// is processed all the same (decisions-T20 #65).
	other := g.createIssue(g.chen, map[string]any{"title": "not linked"})
	_ = other
	if r = g.ghSend("pr_edited.json", func(m map[string]any) {
		m["number"], m["pull_request"].(map[string]any)["number"] = 99, 99
		m["pull_request"].(map[string]any)["title"] = "no ticket here"
	}); r.Status != tracker.WebhookProcessed || len(r.Linked) != 0 || len(r.Moved) != 0 {
		t.Fatalf("edit of an unlinked MR = %+v, want processed and nothing done", r)
	}
	// A fork's review is ignored like a fork's open (open point 16).
	r = g.ghReview("review_approved.json", ghMut(d.Identifier, 9, func(m map[string]any) {
		m["pull_request"].(map[string]any)["head"].(map[string]any)["repo"].(map[string]any)["id"] = 7777
	}))
	if r.Status != tracker.WebhookIgnored {
		t.Fatalf("fork review = %+v", r)
	}
	if cr = g.oneCR(d.Identifier); cr.State != tracker.CRopen {
		t.Fatalf("a fork's approval changed the block: %+v", cr)
	}
	// The same delivery again is a duplicate.
	id := uuid.NewString()
	body := ghFixture(t, "review_approved.json", ghMut(d.Identifier, 10))
	g.ghResult(g.ghHook("orbit", ghSecret, id, body, github.HeaderEvent, github.EventPullRequestReview), http.StatusOK)
	if r = g.ghResult(g.ghHook("orbit", ghSecret, id, body, github.HeaderEvent, github.EventPullRequestReview), http.StatusOK); r.Status != tracker.WebhookDuplicate {
		t.Fatalf("redelivered review = %+v", r)
	}
}

// GitLab: an approval and an unapproval set the review; a plain update
// refreshes; nothing moves.
func TestMRState_GitLab(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	is := e.createIssue(e.chen, map[string]any{"title": "gitlab states"})
	e.setState(is.Identifier, "Todo")
	e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_open.json", naming(is.Identifier, ""))), http.StatusOK)
	cr := e.oneCR(is.Identifier)
	if cr.State != tracker.CRopen || cr.Provider != "gitlab" || cr.Author != "exampledev" || cr.RepoPath != "example-org/example-app" || cr.Number != 17 {
		t.Fatalf("open block = %+v", cr)
	}
	e.setState(is.Identifier, "In Progress")
	r := e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_approved.json", naming(is.Identifier, ""))), http.StatusOK)
	if r.Status != tracker.WebhookProcessed || len(r.Moved) != 0 {
		t.Fatalf("approved = %+v", r)
	}
	if cr = e.oneCR(is.Identifier); cr.State != tracker.CRApproved || cr.Author != "exampledev" {
		t.Fatalf("approved block = %+v (an approval must keep the author)", cr)
	}
	if st := e.issueFull(is.Identifier).State.Name; st != "In Progress" {
		t.Fatalf("an approval moved the ticket to %q", st)
	}
	e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_unapproved.json", naming(is.Identifier, ""))), http.StatusOK)
	if cr = e.oneCR(is.Identifier); cr.State != tracker.CRopen {
		t.Fatalf("unapproved block = %+v", cr)
	}
	e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_update.json", func(a map[string]any) {
		naming(is.Identifier, "")(a)
		a["title"] = is.Identifier + ": renamed on GitLab"
		a["updated_at"] = "2026-10-24 11:00:00 UTC"
	})), http.StatusOK)
	if cr = e.oneCR(is.Identifier); !strings.Contains(cr.Title, "renamed on GitLab") {
		t.Fatalf("update block = %+v", cr)
	}
	// Stale: a GitLab approval older than the stored time is dropped.
	e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_approved.json", func(a map[string]any) {
		naming(is.Identifier, "")(a)
		a["updated_at"] = "2026-10-24 09:30:00 UTC"
	})), http.StatusOK)
	if cr = e.oneCR(is.Identifier); cr.State != tracker.CRopen {
		t.Fatalf("an older approval changed the block: %+v", cr)
	}
	_ = gitlab.HeaderEvent
}

// Cross-org: a change request is read through the issue, so another org's
// issue (and so its MR links) is 404 on this org's paths.
func TestMRState_CrossOrg(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{})
	g.setup()
	org2 := g.newOrg("acme", "ACME")
	admin2 := g.newUser("acmeadmin")
	g.addMember(org2, admin2.ID, "admin")
	tok2, _ := g.newToken(admin2.ID, tokenOpts{pin: &org2})
	var other tracker.Issue
	g.must(http.StatusCreated, g.do("POST", "/api/v1/orgs/acme/issues", tok2, map[string]any{"team": "ACME", "title": "theirs"})).json(t, &other)
	g.must(http.StatusNotFound, g.do("GET", "/api/v1/orgs/orbit/issues/"+other.Identifier+"?include=changeRequests", g.chen, nil))
	g.must(http.StatusNotFound, g.do("GET", "/api/v1/orgs/orbit/issues/"+other.ID.String()+"?include=changeRequests", g.chen, nil))
	// An orbit PR naming ACME-1 links nothing in the other org.
	mine := g.createIssue(g.chen, map[string]any{"title": "mine"})
	r := g.ghSend("pr_opened.json", ghMut(mine.Identifier, 1, func(m map[string]any) {
		m["pull_request"].(map[string]any)["body"] = "Refs " + other.Identifier
	}))
	if len(r.Linked) != 1 {
		t.Fatalf("result = %+v", r)
	}
	if n := g.count(`SELECT count(*) FROM mr_links WHERE issue_id = $1`, other.ID); n != 0 {
		t.Fatal("another org's issue was linked")
	}
	var got tracker.Issue
	g.must(http.StatusOK, g.do("GET", "/api/v1/orgs/acme/issues/"+other.Identifier+"?include=changeRequests", tok2, nil)).json(t, &got)
	if len(got.ChangeRequests) != 0 {
		t.Fatalf("the other org sees %+v", got.ChangeRequests)
	}
}

// --- hook refresh and backfill (token mode) --------------------------------

// tokenRepoEnv is a token-mode GitHub integration on a fake host with the
// given repository ids, one of them mapped.
func tokenRepoEnv(t *testing.T, ids ...int) (*gitEnv, *repoHost, tracker.GitIntegrationCreated, tracker.GitRepo) {
	t.Helper()
	g := newGitEnv(t, gitEnvOpts{})
	h := newRepoHost(t, ids...)
	gi := g.tokenCreate(&fakeHost{Fake: h.Fake})
	return g, h, gi, g.mapRepo(gi.ID, ids[0])
}

func (h *repoHost) hookEvents(repo int) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.hooks[repo]) == 0 {
		return nil
	}
	return append([]string(nil), h.hooks[repo][0].events...)
}

func (h *repoHost) setHookEvents(repo int, ev ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hooks[repo][0].events = ev
}

// AC: a mapped repo's hook gets the new events after a refresh.
func TestMRState_HookRefresh(t *testing.T) {
	g, h, gi, repo := tokenRepoEnv(t, 4242)
	refresh := g.path(gi.ID, "/repos/"+repo.ID.String()+"/hook/refresh")
	wantEvents := "[pull_request pull_request_review check_suite status]"
	if got := fmt.Sprint(h.hookEvents(4242)); got != wantEvents {
		t.Fatalf("a hook made now subscribes to %v, want %s", got, wantEvents)
	}
	// A hook made before LAUN-83 knows only pull_request; one made before LAUN-85
	// has no check events. Both learn the full set after one refresh.
	h.setHookEvents(4242, "pull_request")
	secret := h.secret(4242)
	var out tracker.HookRefreshResult
	g.must(http.StatusOK, g.do("POST", refresh, g.chen, nil)).json(t, &out)
	if out.HookState != "active" {
		t.Fatalf("refresh = %+v", out)
	}
	if got := fmt.Sprint(h.hookEvents(4242)); got != wantEvents {
		t.Fatalf("after the refresh the hook has %v, want %s", got, wantEvents)
	}
	h.setHookEvents(4242, "pull_request", "pull_request_review")
	g.must(http.StatusOK, g.do("POST", refresh, g.chen, nil))
	if got := fmt.Sprint(h.hookEvents(4242)); got != wantEvents {
		t.Fatalf("a LAUN-83 hook has %v after the refresh, want %s", got, wantEvents)
	}
	if h.hookCount(4242) != 1 || h.secret(4242) != secret || g.pingStatus(secret) != 200 {
		t.Fatal("a refresh must update the same hook and keep its secret")
	}
	// Repeating it is safe.
	g.must(http.StatusOK, g.do("POST", refresh, g.chen, nil))
	if h.hookCount(4242) != 1 {
		t.Fatal("a second refresh made a second hook")
	}

	// A host failure: 502 with our code, the repo is marked failed, no host words.
	h.fail(4242, 500)
	r := g.do("POST", refresh, g.chen, nil)
	if r.status != http.StatusBadGateway || r.errCode(t) != "provider_error" || strings.Contains(string(r.body), "host words") {
		t.Fatalf("host failure: %d %s", r.status, r.body)
	}
	if st := g.hookStates(gi.ID)["4242"]; st != "failed:provider_error" {
		t.Fatalf("hook state = %q", st)
	}
	h.fail(4242, 0)
	g.must(http.StatusOK, g.do("POST", refresh, g.chen, nil))
	if st := g.hookStates(gi.ID)["4242"]; st != "active" {
		t.Fatalf("hook state after a good refresh = %q", st)
	}

	// A pending rotation owns the hooks: refresh is 409.
	h.fail(4242, 500)
	rot := g.rotate(gi.ID, http.StatusOK, nil)
	if !rot.SecretPending {
		t.Fatalf("rotation = %+v, want pending", rot)
	}
	h.fail(4242, 0)
	if r = g.do("POST", refresh, g.chen, nil); r.status != http.StatusConflict {
		t.Fatalf("refresh during a rotation: %d %s", r.status, r.body)
	}
	g.must(http.StatusOK, g.do("DELETE", g.path(gi.ID, "/rotate-secret"), g.chen, nil))

	// Not an admin, not found, another org's integration.
	g.must(http.StatusForbidden, g.do("POST", refresh, g.claude, nil))
	g.must(http.StatusNotFound, g.do("POST", g.path(gi.ID, "/repos/"+uuid.NewString()+"/hook/refresh"), g.chen, nil))
	g.must(http.StatusNotFound, g.do("POST", g.path(uuid.New(), "/repos/"+repo.ID.String()+"/hook/refresh"), g.chen, nil))
}

func TestMRState_HookRefreshAndBackfillNeedToken(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	var gi tracker.GitIntegration
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "github", "baseUrl": "https://github.com"})).json(t, &gi)
	var repo tracker.GitRepo
	g.must(http.StatusCreated, g.do("POST", g.path(gi.ID, "/repos"), g.chen, map[string]any{"repoKey": "4242", "repoPath": "acme/x"})).json(t, &repo)
	for _, tail := range []string{"/hook/refresh", "/backfill"} {
		r := g.do("POST", g.path(gi.ID, "/repos/"+repo.ID.String()+tail), g.chen, nil)
		if r.status != http.StatusConflict || r.errCode(t) != "conflict" {
			t.Fatalf("webhook-only %s: %d %s", tail, r.status, r.body)
		}
	}
	// Another org's integration is 404 on both routes.
	org2 := g.newOrg("acme", "ACME")
	admin2 := g.newUser("acmeadmin")
	g.addMember(org2, admin2.ID, "admin")
	tok2, _ := g.newToken(admin2.ID, tokenOpts{pin: &org2})
	for _, tail := range []string{"/hook/refresh", "/backfill"} {
		g.must(http.StatusNotFound, g.do("POST", "/api/v1/orgs/acme/integrations/"+gi.ID.String()+"/repos/"+repo.ID.String()+tail, tok2, nil))
	}
}

func hostPR(repo, n int, ident, title string, draft bool, minute int) map[string]any {
	branch := "feature/x" + strconv.Itoa(n)
	if ident != "" {
		branch = "chen/" + strings.ToLower(ident) + "-x"
		title = ident + ": " + title
	}
	return map[string]any{
		"number": n, "html_url": fmt.Sprintf("https://github.example.com/acme/repo-%d/pull/%d", repo, n),
		"title": title, "body": "", "state": "open", "draft": draft, "merged": false,
		"updated_at": fmt.Sprintf("2026-10-20T09:%02d:00Z", minute),
		"user":       map[string]any{"login": "example-dev"},
		"head":       map[string]any{"ref": branch, "sha": fmt.Sprintf("%040d", n), "repo": map[string]any{"id": repo}},
		"base":       map[string]any{"repo": map[string]any{"id": repo}},
	}
}

// AC: backfill makes the same links as live events, moves forward only, and
// is safe to repeat.
func TestMRState_Backfill(t *testing.T) {
	g, h, gi, repo := tokenRepoEnv(t, 4242)
	mk := func(title, state string) tracker.Issue {
		is := g.createIssue(g.chen, map[string]any{"title": title})
		g.setState(is.Identifier, state)
		return is
	}
	i1, i2, i3, i4, i5, i6 := mk("open one", "Todo"), mk("draft one", "Todo"), mk("already done", "Done"), mk("fork one", "Todo"), mk("described only", "Todo"), mk("live twin", "Todo")
	fork := hostPR(4242, 4, i4.Identifier, "fork", false, 4)
	fork["head"].(map[string]any)["repo"] = map[string]any{"id": 9999}
	desc := hostPR(4242, 5, "", "described", false, 5)
	desc["body"] = "Refs " + i5.Identifier
	h.pulls = map[int][]map[string]any{4242: {
		hostPR(4242, 1, i1.Identifier, "open", false, 1),
		hostPR(4242, 2, i2.Identifier, "draft", true, 2),
		hostPR(4242, 3, i3.Identifier, "done", false, 3),
		fork, desc,
	}}
	path := g.path(gi.ID, "/repos/"+repo.ID.String()+"/backfill")
	var out tracker.BackfillResult
	g.must(http.StatusOK, g.do("POST", path, g.chen, nil)).json(t, &out)
	if out.Seen != 5 || out.Linked != 4 || out.Moved != 1 || out.Truncated {
		t.Fatalf("backfill = %+v, want seen 5, linked 4 (not the fork), moved 1", out)
	}
	for ident, want := range map[string]string{i1.Identifier: "In Review", i2.Identifier: "Todo", i3.Identifier: "Done", i4.Identifier: "Todo", i5.Identifier: "Todo"} {
		if st := g.issueFull(ident).State.Name; st != want {
			t.Fatalf("%s is %q, want %q (forward only; a draft, a done ticket and a description-only reference stay)", ident, st, want)
		}
	}
	if cr := g.oneCR(i2.Identifier); cr.State != tracker.CRDraft || cr.Provider != "github" || cr.RepoPath != "acme/repo-4242" || cr.Author != "example-dev" {
		t.Fatalf("backfilled draft block = %+v", cr)
	}
	if n := len(g.changeRequests(i4.Identifier)); n != 0 {
		t.Fatalf("a fork change was linked (%d)", n)
	}
	if k := actKinds(g.issueActivity(i1.Identifier)); k["mrLinked"] != 1 {
		t.Fatalf("backfill timeline = %v", k)
	}
	if g.count(`SELECT count(*) FROM events WHERE org_id = $1 AND type = 'attachment.created' AND payload->>'issueId' = $2`, g.orgID, i1.ID.String()) != 1 ||
		g.mrEvents(i1.ID) != 1 {
		t.Fatal("the backfill must emit like a live event (attachment.created, and issue.updated with the move)")
	}

	// The same change through a LIVE event makes the same row.
	secret := h.secret(4242)
	live := ghFixture(t, "pr_opened.json", func(m map[string]any) {
		pr := m["pull_request"].(map[string]any)
		m["number"], pr["number"] = 1, 1 // same MR number: another issue, so another row
		pr["title"], pr["body"] = i6.Identifier+": open", ""
		pr["head"].(map[string]any)["ref"] = "chen/" + strings.ToLower(i6.Identifier) + "-x"
		pr["head"].(map[string]any)["sha"] = fmt.Sprintf("%040d", 1)
		pr["html_url"] = "https://github.example.com/acme/repo-4242/pull/1"
		pr["updated_at"] = "2026-10-20T09:01:00Z"
		m["repository"].(map[string]any)["full_name"] = "acme/repo-4242"
	})
	gh := &ghEnv{env: g.env}
	gh.ghResult(gh.ghHook("orbit", secret, uuid.NewString(), live), http.StatusOK)
	cols := func(id uuid.UUID) string {
		var s string
		if err := g.pool.QueryRow(context.Background(), `SELECT state||'|'||draft||'|'||review||'|'||checks||'|'||author||'|'||repo_path||'|'||head_sha||'|'||url||'|'||provider_updated_at::text
			FROM mr_links WHERE issue_id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if a, b := cols(i1.ID), cols(i6.ID); a != b {
		t.Fatalf("backfill and a live event made different rows:\n backfill %s\n live     %s", a, b)
	}

	// A second run links and moves nothing.
	g.must(http.StatusOK, g.do("POST", path, g.chen, nil)).json(t, &out)
	if out.Seen != 5 || out.Linked != 0 || out.Moved != 0 {
		t.Fatalf("second backfill = %+v", out)
	}
	if g.count(`SELECT count(*) FROM mr_links WHERE issue_id = $1`, i1.ID) != 1 || g.count(`SELECT count(*) FROM attachments WHERE issue_id = $1`, i1.ID) != 1 {
		t.Fatal("a second backfill made a second link")
	}

	// Access: admin only; unknown mapping is 404.
	g.must(http.StatusForbidden, g.do("POST", path, g.claude, nil))
	g.must(http.StatusNotFound, g.do("POST", g.path(gi.ID, "/repos/"+uuid.NewString()+"/backfill"), g.chen, nil))
}

// AC: backfill caps at 200 and answers truncated.
func TestMRState_BackfillCap(t *testing.T) {
	g, h, gi, repo := tokenRepoEnv(t, 4242, 4243)
	r2 := g.mapRepo(gi.ID, 4243)
	many := func(repo, n int) []map[string]any {
		var out []map[string]any
		for i := 1; i <= n; i++ {
			out = append(out, hostPR(repo, i, "", "chore", false, i%60))
		}
		return out
	}
	h.pulls = map[int][]map[string]any{4242: many(4242, 205), 4243: many(4243, 200)}
	var out tracker.BackfillResult
	g.must(http.StatusOK, g.do("POST", g.path(gi.ID, "/repos/"+repo.ID.String()+"/backfill"), g.chen, nil)).json(t, &out)
	if out.Seen != 200 || !out.Truncated || out.Linked != 0 {
		t.Fatalf("205 open changes: %+v, want seen 200 and truncated", out)
	}
	g.must(http.StatusOK, g.do("POST", g.path(gi.ID, "/repos/"+r2.ID.String()+"/backfill"), g.chen, nil)).json(t, &out)
	if out.Seen != 200 || out.Truncated {
		t.Fatalf("exactly 200 open changes: %+v, want seen 200 and not truncated", out)
	}
}

// --- LAUN-96: lock order, backfill review, backfill deadline ----------------

// setReviews sets the reviews the fake host answers for one pull request.
func (h *repoHost) setReviews(repo, number int, rs ...map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.reviews == nil {
		h.reviews = map[string][]map[string]any{}
	}
	h.reviews[fmt.Sprintf("%d/%d", repo, number)] = rs
}

func hostReview(user, state string) map[string]any {
	return map[string]any{"state": state, "user": map[string]any{"login": user}}
}

// waitForLockWaiter blocks until some backend waits on a lock.
func waitForLockWaiter(t *testing.T, e *env) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if e.count(`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("nothing waited on a lock")
}

// LAUN-96 #1: ONE lock order, the issue row before the link row. linkAndMove
// (a reopen, ready or merged event) locks the issue and then the link. The test
// is that writer: it holds the issue row and then asks for the link row, while
// a close event is in flight. Before the fix the close held the link and waited
// for the issue (its timeline row takes a key-share lock on it): a deadlock,
// and Postgres aborted one of the two. Now the close waits for the issue first,
// so the other writer gets the link, commits, and the close goes through.
func TestMRState_CloseRacingAnIssueFirstWriterDoesNotDeadlock(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{})
	g.setup()
	a := g.createIssue(g.chen, map[string]any{"title": "lock order"})
	g.setState(a.Identifier, "Todo")
	g.ghSend("pr_opened.json", ghMut(a.Identifier, 10))

	ctx := context.Background()
	tx, err := g.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM issues WHERE id = $1 FOR UPDATE`, a.ID); err != nil {
		t.Fatal(err)
	}

	closeBody := ghFixture(t, "pr_closed_unmerged.json", ghMut(a.Identifier, 20))
	done := make(chan resp, 1)
	go func() { done <- g.ghHook("orbit", ghSecret, uuid.NewString(), closeBody) }()
	waitForLockWaiter(t, g.env)

	// The issue-first writer now takes the link row, as LockMRLink does.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM mr_links WHERE issue_id = $1 FOR UPDATE`, a.ID); err != nil {
		t.Fatalf("the link row could not be locked while a close waits (deadlock): %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.status != http.StatusOK {
			t.Fatalf("the close answered %d: %s", r.status, r.body)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the close never finished")
	}
	if cr := g.oneCR(a.Identifier); cr.State != tracker.CRClosed {
		t.Fatalf("the close was lost: %+v", cr)
	}
	if actKinds(g.issueActivity(a.Identifier))["mrClosed"] != 1 {
		t.Fatal("the close wrote no timeline row")
	}
}

// LAUN-96 #1, real concurrency: closes, reopens and readies of one MR, sent at
// once many times, all answer 200 (no deadlock abort, no 500).
func TestMRState_ConcurrentCloseAndReopenAllAnswer200(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{})
	g.setup()
	a := g.createIssue(g.chen, map[string]any{"title": "race"})
	g.setState(a.Identifier, "Todo")
	g.ghSend("pr_opened.json", ghMut(a.Identifier, 1))
	var wg sync.WaitGroup
	statuses := make(chan int, 80)
	for round := 0; round < 10; round++ {
		for i, fx := range []string{"pr_closed_unmerged.json", "pr_reopened.json", "pr_closed_unmerged.json", "pr_ready_for_review.json"} {
			body := ghFixture(t, fx, ghMut(a.Identifier, 2+round*4+i))
			wg.Add(1)
			go func() {
				defer wg.Done()
				statuses <- g.ghHook("orbit", ghSecret, uuid.NewString(), body).status
			}()
		}
	}
	wg.Wait()
	close(statuses)
	for s := range statuses {
		if s != http.StatusOK {
			t.Fatalf("a concurrent event answered %d, want 200", s)
		}
	}
}

// LAUN-96 #2: the backfill reads the host's review state, also for a link that
// exists, and a later run corrects it (a dismissal, a request for changes).
func TestMRState_BackfillRefreshesTheReviewOfAnExistingLink(t *testing.T) {
	g, h, gi, repo := tokenRepoEnv(t, 4242)
	is := g.createIssue(g.chen, map[string]any{"title": "review sync"})
	g.setState(is.Identifier, "Todo")
	h.pulls = map[int][]map[string]any{4242: {hostPR(4242, 1, is.Identifier, "open", false, 1)}}
	path := g.path(gi.ID, "/repos/"+repo.ID.String()+"/backfill")
	run := func() tracker.BackfillResult {
		var out tracker.BackfillResult
		g.must(http.StatusOK, g.do("POST", path, g.chen, nil)).json(t, &out)
		return out
	}
	if out := run(); out.Linked != 1 {
		t.Fatalf("first backfill = %+v", out)
	}
	if cr := g.oneCR(is.Identifier); cr.State != tracker.CRopen {
		t.Fatalf("no reviews: %+v", cr)
	}
	events := g.mrEvents(is.ID)

	h.setReviews(4242, 1, hostReview("alice", "APPROVED"))
	run()
	if cr := g.oneCR(is.Identifier); cr.State != tracker.CRApproved {
		t.Fatalf("the host's approval was not read into the existing link: %+v", cr)
	}
	if g.mrEvents(is.ID) != events+1 {
		t.Fatal("the review change must emit one issue.updated")
	}
	run()
	if g.mrEvents(is.ID) != events+1 {
		t.Fatal("a repeat with no change emitted an event")
	}

	h.setReviews(4242, 1, hostReview("alice", "APPROVED"), hostReview("bob", "CHANGES_REQUESTED"))
	run()
	if cr := g.oneCR(is.Identifier); cr.State != tracker.CRopen || !cr.ChangesRequested {
		t.Fatalf("a standing request for changes: %+v", cr)
	}
	// bob's review is dismissed: alice's approval still stands.
	h.setReviews(4242, 1, hostReview("alice", "APPROVED"), hostReview("bob", "CHANGES_REQUESTED"), hostReview("bob", "DISMISSED"))
	run()
	if cr := g.oneCR(is.Identifier); cr.State != tracker.CRApproved || cr.ChangesRequested {
		t.Fatalf("after bob's dismissal: %+v", cr)
	}
	h.setReviews(4242, 1)
	run()
	if cr := g.oneCR(is.Identifier); cr.State != tracker.CRopen {
		t.Fatalf("no reviews left: %+v", cr)
	}

	// A review the host cannot answer keeps the stored one.
	h.setReviews(4242, 1, hostReview("alice", "APPROVED"))
	run()
	h.mu.Lock()
	delete(h.reviews, "4242/1")
	h.mu.Unlock()
	h.mu.Lock()
	h.failReviews = true
	h.mu.Unlock()
	run()
	if cr := g.oneCR(is.Identifier); cr.State != tracker.CRApproved {
		t.Fatalf("a failed review read changed the review: %+v", cr)
	}
}

// LAUN-96 #2: a live opened / ready / merged event never wipes the review a
// review event set (its own review field is always none).
func TestMRState_LiveChangeEventKeepsTheReview(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{})
	g.setup()
	a := g.createIssue(g.chen, map[string]any{"title": "keep review"})
	g.setState(a.Identifier, "Todo")
	g.ghSend("pr_opened.json", ghMut(a.Identifier, 1))
	g.ghReview("review_approved.json", ghMut(a.Identifier, 2))
	g.ghSend("pr_ready_for_review.json", ghMut(a.Identifier, 3))
	if cr := g.oneCR(a.Identifier); cr.State != tracker.CRApproved {
		t.Fatalf("a ready event wiped the approval: %+v", cr)
	}
}

// LAUN-96 #3: GitLab's "approval" / "unapproval" are ONE person's actions
// (the whole-MR words are "approved" / "unapproved"): they are ignored, so a
// single person's removal cannot clear an approval another person gave.
func TestMRState_GitLabSinglePersonApprovalActionsAreIgnored(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	is := e.createIssue(e.chen, map[string]any{"title": "one person"})
	e.setState(is.Identifier, "Todo")
	e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_open.json", naming(is.Identifier, ""))), http.StatusOK)
	e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_approved.json", func(a map[string]any) {
		naming(is.Identifier, "")(a)
		a["updated_at"] = "2026-10-24 11:00:00 UTC"
	})), http.StatusOK)
	e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_unapproved.json", func(a map[string]any) {
		naming(is.Identifier, "")(a)
		a["action"], a["updated_at"] = "unapproval", "2026-10-24 12:00:00 UTC"
	})), http.StatusOK)
	if cr := e.oneCR(is.Identifier); cr.State != tracker.CRApproved {
		t.Fatalf("one person's unapproval cleared the approval: %+v", cr)
	}
}

// LAUN-96 #4: the 30 s deadline hitting a database write is a truncated
// answer with the counts of the work already done, not a 500.
func TestMRState_BackfillDeadlineDuringAWriteIsTruncated(t *testing.T) {
	g, h, gi, repo := tokenRepoEnv(t, 4242)
	done := g.createIssue(g.chen, map[string]any{"title": "written first"})
	held := g.createIssue(g.chen, map[string]any{"title": "held by someone"})
	g.setState(done.Identifier, "Todo")
	g.setState(held.Identifier, "Todo")
	h.pulls = map[int][]map[string]any{4242: {
		hostPR(4242, 1, done.Identifier, "first", false, 1),
		hostPR(4242, 2, held.Identifier, "second", false, 2),
	}}
	g.server.svc.SetBackfillTimeout(1500 * time.Millisecond)

	ctx := context.Background()
	tx, err := g.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM issues WHERE id = $1 FOR UPDATE`, held.ID); err != nil {
		t.Fatal(err)
	}
	path := g.path(gi.ID, "/repos/"+repo.ID.String()+"/backfill")
	var out tracker.BackfillResult
	g.must(http.StatusOK, g.do("POST", path, g.chen, nil)).json(t, &out)
	if !out.Truncated || out.Linked != 1 || out.Moved != 1 || out.Seen < 2 {
		t.Fatalf("backfill with a write that cannot finish = %+v, want truncated with the first change linked and moved", out)
	}
	if n := len(g.changeRequests(done.Identifier)); n != 1 {
		t.Fatalf("the first change was lost (%d links)", n)
	}
	if n := len(g.changeRequests(held.Identifier)); n != 0 {
		t.Fatalf("the interrupted change left a link (%d)", n)
	}

	// Run it again once the lock is gone: the rest is done.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	g.server.svc.SetBackfillTimeout(0)
	g.must(http.StatusOK, g.do("POST", path, g.chen, nil)).json(t, &out)
	if out.Truncated || out.Linked != 1 || out.Seen != 2 {
		t.Fatalf("second backfill = %+v", out)
	}
	if n := len(g.changeRequests(held.Identifier)); n != 1 {
		t.Fatalf("the repeat did not link the held change (%d)", n)
	}
}

// LAUN-96: the event paths that lock issue rows (update, review, close,
// checks) take the team rank lock first, like linkAndMove. A PATCH that
// rebalances a column holds the rank lock and one issue row (A) and then asks
// for the other rows of the column (B). An MR naming B and A (B has the lower
// id): the event locked B, then waited for A; the PATCH held A and waited for
// B. The test is that PATCH, step by step, with an update and a check event in
// flight.
func TestMRState_RankLockBeforeIssueLocksOnEventPaths(t *testing.T) {
	for _, kind := range []string{"update", "checks"} {
		t.Run(kind, func(t *testing.T) {
			g := newGHEnv(t, 0, 0, true, envOpts{})
			g.setup()
			x := g.createIssue(g.chen, map[string]any{"title": "one"})
			y := g.createIssue(g.chen, map[string]any{"title": "two"})
			b, a := x, y // b has the lower issue id
			if strings.Compare(b.ID.String(), a.ID.String()) > 0 {
				b, a = a, b
			}
			g.setState(a.Identifier, "Todo")
			g.setState(b.Identifier, "Todo")
			both := func(m map[string]any) {
				m["pull_request"].(map[string]any)["title"] = a.Identifier + " " + b.Identifier + ": both"
			}
			g.ghSend("pr_opened.json", ghMut(b.Identifier, 1, both))
			if n := g.count(`SELECT count(*) FROM mr_links WHERE issue_id IN ($1, $2)`, a.ID, b.ID); n != 2 {
				t.Fatalf("%d links, want 2", n)
			}
			var teamID uuid.UUID
			if err := g.pool.QueryRow(context.Background(), `SELECT team_id FROM issues WHERE id = $1`, a.ID).Scan(&teamID); err != nil {
				t.Fatal(err)
			}

			ctx := context.Background()
			tx, err := g.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			// The PATCH-rebalance: rank lock, issue A, (then every other row of the column).
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('devops:issue-rank:' || $1::text, 0))`, teamID); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `SELECT 1 FROM issues WHERE id = $1 FOR UPDATE`, a.ID); err != nil {
				t.Fatal(err)
			}
			done := make(chan resp, 1)
			if kind == "update" {
				body := ghFixture(t, "pr_edited.json", ghMut(b.Identifier, 5, both))
				go func() { done <- g.ghHook("orbit", ghSecret, uuid.NewString(), body) }()
			} else {
				body := ghFixture(t, "check_suite_requested.json", suiteAt(prSHA, "example-ci", "queued", "", 5))
				go func() {
					done <- g.ghHook("orbit", ghSecret, uuid.NewString(), body, github.HeaderEvent, github.EventCheckSuite)
				}()
			}
			waitForLockWaiter(t, g.env)
			if _, err := tx.Exec(ctx, `SELECT 1 FROM issues WHERE id = $1 FOR UPDATE`, b.ID); err != nil {
				t.Fatalf("the rebalance could not lock the other row of the column while an event waits (deadlock): %v", err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if r := <-done; r.status != http.StatusOK {
				t.Fatalf("the %s event answered %d: %s", kind, r.status, r.body)
			}
		})
	}
}
