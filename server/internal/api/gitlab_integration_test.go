package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/gitlab"
	"devops/internal/tracker"
)

// T5: the GitLab webhook against a real database, driven by the
// documented-shape fixtures in internal/gitlab/testdata (hand-written, not
// recorded deliveries; see its README.md).

const (
	glSecret    = "glwh-test-secret-0123456789abcdefABCDEF" // 39 printable characters
	glBaseURL   = "https://gitlab.example.com"
	glProjectID = 42 // project.id in every fixture
)

// glFixture loads a fixture and lets the test set the fields that name the
// issues (the rest of the payload stays as written).
func glFixture(t *testing.T, name string, mut func(attrs map[string]any)) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "gitlab", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if mut != nil {
		mut(m["object_attributes"].(map[string]any))
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// naming returns a mutator that makes the MR name ident in its branch and
// title, with description desc.
func naming(ident, desc string) func(map[string]any) {
	return func(a map[string]any) {
		a["source_branch"] = "chen/" + strings.ToLower(ident) + "-fix-login-redirect"
		a["title"] = ident + ": Fix login redirect"
		a["description"] = desc
	}
}

// hook sends one webhook delivery. headers are extra or replacement
// key/value pairs; a value of "" removes that header.
func (e *env) hook(org, secret, eventUUID string, body []byte, headers ...string) resp {
	e.t.Helper()
	req, err := http.NewRequest("POST", e.srv.URL+"/api/v1/orgs/"+org+"/integrations/gitlab/webhook", bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(gitlab.HeaderEvent, gitlab.MergeRequestHook)
	if secret != "" {
		req.Header.Set(gitlab.HeaderToken, secret)
	}
	if eventUUID != "" {
		req.Header.Set(gitlab.HeaderEventUUID, eventUUID)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i+1] == "" {
			req.Header.Del(headers[i])
		} else {
			req.Header.Set(headers[i], headers[i+1])
		}
	}
	return e.sendHook(req)
}

// hookOutcome is a webhook answer's status and what the delivery did: the
// issues it newly linked (a new link, with its link attachment) and those it moved, read from
// the database before and after (the answer itself is a status only,
// decisions-T20 #65).
type hookOutcome struct {
	Status        string
	Linked, Moved []string
}

func (e *env) hookResult(r resp, status int) hookOutcome {
	e.t.Helper()
	var out struct {
		Status string `json:"status"`
	}
	e.must(status, r).json(e.t, &out)
	return hookOutcome{Status: out.Status, Linked: r.linked, Moved: r.moved}
}

// hookIssue is one issue's state and link count, for hookDiff.
type hookIssue struct {
	state string
	links int
}

// hookSnap reads every issue's state and number of mr_links rows. It runs
// outside any lock a test holds (a plain read), and reports an error with
// Errorf, so a sender in a goroutine can call it.
func (e *env) hookSnap() map[string]hookIssue {
	out := map[string]hookIssue{}
	// A new link writes a link attachment with no creator (linkAndMove); the
	// attachments are counted, not mr_links, which a test may rename.
	rows, err := e.pool.Query(context.Background(), `SELECT t.key || '-' || i.number, i.state_id::text,
		(SELECT count(*) FROM attachments a WHERE a.org_id = i.org_id AND a.issue_id = i.id AND a.kind = 'link' AND a.creator_id IS NULL)
		FROM issues i JOIN teams t ON t.org_id = i.org_id AND t.id = i.team_id`)
	if err != nil {
		e.t.Errorf("hook snapshot: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var h hookIssue
		if err := rows.Scan(&id, &h.state, &h.links); err != nil {
			e.t.Errorf("hook snapshot: %v", err)
			return out
		}
		out[id] = h
	}
	if err := rows.Err(); err != nil {
		e.t.Errorf("hook snapshot: %v", err)
	}
	return out
}

// hookDiff is the issues newly linked and moved between two snapshots.
func hookDiff(before, after map[string]hookIssue) (linked, moved []string) {
	linked, moved = []string{}, []string{}
	for id, a := range after {
		b, ok := before[id]
		if a.links > b.links {
			linked = append(linked, id)
		}
		if ok && a.state != b.state {
			moved = append(moved, id)
		}
	}
	sort.Strings(linked)
	sort.Strings(moved)
	return linked, moved
}

// sendHook sends a webhook request and fills linked and moved from the
// database (hookDiff).
func (e *env) sendHook(req *http.Request) resp {
	e.t.Helper()
	before := e.hookSnap()
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Error(err)
		return resp{}
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		e.t.Error(err)
		return resp{}
	}
	linked, moved := hookDiff(before, e.hookSnap())
	return resp{status: res.StatusCode, body: b, header: res.Header, linked: linked, moved: moved}
}

// glSetup adds an integration with glSecret to org (as token, an admin) and
// maps project glProjectID. It returns the integration id.
func (e *env) glSetup(org, token string) uuid.UUID {
	e.t.Helper()
	var gi tracker.GitIntegration
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/"+org+"/integrations", token,
		map[string]any{"provider": "gitlab", "baseUrl": glBaseURL, "secret": glSecret})).json(e.t, &gi)
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/"+org+"/integrations/"+gi.ID.String()+"/repos", token,
		map[string]any{"repoKey": strconv.Itoa(glProjectID), "repoPath": "example-org/example-app"}))
	return gi.ID
}

func (e *env) issueFull(ident string) tracker.Issue {
	e.t.Helper()
	var is tracker.Issue
	e.must(http.StatusOK, e.do("GET", "/api/v1/orgs/orbit/issues/"+ident+"?include=stateHistory,attachments", e.chen, nil)).json(e.t, &is)
	return is
}

func (e *env) setState(ident, state string) {
	e.t.Helper()
	e.must(http.StatusOK, e.do("PATCH", "/api/v1/orgs/orbit/issues/"+ident, e.chen, map[string]any{"state": state}))
}

func (e *env) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", query, err)
	}
	return n
}

func gitlabSources(is tracker.Issue) int {
	n := 0
	for _, sc := range is.StateHistory {
		if sc.Source == "gitlab" {
			n++
		}
	}
	return n
}

// AC: an MR opened from an orb-<n> branch moves the issue to In Review and
// adds the MR link; a merge moves it to In Testing.
func TestGitlabWebhook_OpenThenMerge(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	is := e.createIssue(e.chen, map[string]any{"title": "Fix login redirect"})
	e.setState(is.Identifier, "Todo")

	r := e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_open.json", naming(is.Identifier, ""))), http.StatusOK)
	if r.Status != tracker.WebhookProcessed || len(r.Linked) != 1 || r.Linked[0] != is.Identifier || len(r.Moved) != 1 {
		t.Fatalf("open result = %+v", r)
	}
	got := e.issueFull(is.Identifier)
	if got.State.Name != "In Review" || got.StartedAt == nil {
		t.Fatalf("after open: state %q startedAt %v, want In Review and set", got.State.Name, got.StartedAt)
	}
	last := got.StateHistory[len(got.StateHistory)-1]
	if last.Source != "gitlab" || last.Actor != nil || last.FromStateName == nil || *last.FromStateName != "Todo" {
		t.Fatalf("history row = %+v, want Todo -> In Review from gitlab with no actor", last)
	}
	mrURL := "https://gitlab.example.com/example-org/example-app/-/merge_requests/17"
	if len(got.Attachments) != 1 {
		t.Fatalf("attachments = %+v, want the MR link", got.Attachments)
	}
	a := got.Attachments[0]
	if a.Kind != "link" || a.URL == nil || *a.URL != mrURL || a.Title != "MR !17: "+is.Identifier+": Fix login redirect" ||
		a.Subtitle != "example-org/example-app" || a.Creator != nil {
		t.Fatalf("link attachment = %+v", a)
	}
	if n := e.count(`SELECT count(*) FROM mr_links WHERE org_id = $1 AND issue_id = $2 AND repo_key = '42' AND number = 17 AND state = 'opened' AND url = $3`,
		e.orgID, is.ID, mrURL); n != 1 {
		t.Fatalf("mr_links rows = %d, want 1 opened", n)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE org_id = $1 AND type = 'issue.updated' AND entity_id = $2 AND actor_id IS NULL
		AND payload->>'source' = 'gitlab' AND payload->'mergeRequest'->>'iid' = '17'`, e.orgID, is.ID); n != 1 {
		t.Fatalf("gitlab issue.updated events = %d, want 1", n)
	}
	// The same payload fields as an API update (LAUN-33): the notification
	// consumer reads the team and the assignee from them.
	if n := e.count(`SELECT count(*) FROM events ev JOIN issues i ON i.id = ev.entity_id
		WHERE ev.org_id = $1 AND ev.type = 'issue.updated' AND ev.entity_id = $2 AND ev.payload->>'source' = 'gitlab'
		AND ev.payload->>'teamId' = i.team_id::text AND ev.payload ? 'assigneeId'`, e.orgID, is.ID); n != 1 {
		t.Fatalf("gitlab issue.updated events with teamId and assigneeId = %d, want 1", n)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE org_id = $1 AND type = 'attachment.created' AND payload->>'issueId' = $2`, e.orgID, is.ID.String()); n != 1 {
		t.Fatalf("attachment.created events = %d, want 1", n)
	}

	if n := e.count(`SELECT count(*) FROM attachments WHERE issue_id = $1 AND kind = 'link' AND creator_id IS NULL`, is.ID); n != 1 {
		t.Fatalf("webhook link attachments with no creator = %d, want 1", n)
	}

	r = e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_merge.json", naming(is.Identifier, ""))), http.StatusOK)
	if r.Status != tracker.WebhookProcessed || len(r.Moved) != 1 || len(r.Linked) != 0 {
		t.Fatalf("merge result = %+v, want moved and nothing newly linked", r)
	}
	got = e.issueFull(is.Identifier)
	if got.State.Name != "In Testing" {
		t.Fatalf("after merge: state %q, want In Testing", got.State.Name)
	}
	if gitlabSources(got) != 2 || len(got.Attachments) != 1 {
		t.Fatalf("after merge: %d gitlab history rows, %d attachments; want 2 and 1 (no second link)", gitlabSources(got), len(got.Attachments))
	}
	if n := e.count(`SELECT count(*) FROM mr_links WHERE issue_id = $1 AND state = 'merged'`, is.ID); n != 1 {
		t.Fatalf("merged mr_links = %d, want 1", n)
	}
	if n := e.count(`SELECT count(*) FROM git_webhook_events WHERE org_id = $1 AND status = 'processed' AND processed_at IS NOT NULL`, e.orgID); n != 2 {
		t.Fatalf("processed webhook events = %d, want 2", n)
	}
}

// AC: a Done or Canceled issue never moves (nor Duplicate), on open or
// merge. It is still linked.
func TestGitlabWebhook_ClosedStatesNeverMove(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	for _, state := range []string{"Done", "Canceled", "Duplicate"} {
		for _, fx := range []string{"mr_open.json", "mr_merge.json", "mr_reopen.json"} {
			t.Run(state+"/"+fx, func(t *testing.T) {
				is := e.createIssue(e.chen, map[string]any{"title": "closed " + state})
				e.setState(is.Identifier, state)
				before := e.issueFull(is.Identifier)
				r := e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, fx, naming(is.Identifier, ""))), http.StatusOK)
				if len(r.Moved) != 0 || len(r.Linked) != 1 {
					t.Fatalf("result = %+v, want linked and not moved", r)
				}
				after := e.issueFull(is.Identifier)
				if after.State.Name != state || after.Version != before.Version || gitlabSources(after) != 0 {
					t.Fatalf("issue moved: %q v%d -> %q v%d, %d gitlab history rows", state, before.Version, after.State.Name, after.Version, gitlabSources(after))
				}
				if len(after.Attachments) != 1 {
					t.Fatalf("attachments = %d, want the MR link", len(after.Attachments))
				}
			})
		}
	}
}

// Moves are forward only: a reopen after a merge leaves In Testing alone.
func TestGitlabWebhook_ForwardOnly(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	is := e.createIssue(e.chen, map[string]any{"title": "forward only"})
	e.setState(is.Identifier, "In Testing")
	for _, fx := range []string{"mr_reopen.json", "mr_open.json"} {
		r := e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, fx, naming(is.Identifier, ""))), http.StatusOK)
		if len(r.Moved) != 0 {
			t.Fatalf("%s moved In Testing back: %+v", fx, r)
		}
	}
	if got := e.issueFull(is.Identifier); got.State.Name != "In Testing" || gitlabSources(got) != 0 {
		t.Fatalf("state %q with %d gitlab rows, want In Testing and none", got.State.Name, gitlabSources(got))
	}
	// A backlog issue merged without a review jumps straight to In Testing.
	other := e.createIssue(e.chen, map[string]any{"title": "straight to testing"})
	e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_merge.json", naming(other.Identifier, ""))), http.StatusOK)
	if got := e.issueFull(other.Identifier); got.State.Name != "In Testing" {
		t.Fatalf("backlog issue after merge: %q, want In Testing", got.State.Name)
	}
}

// snapshot is every row a webhook can write, for "changed nothing" checks.
type glSnapshot struct {
	webhookEvents, mrLinks, attachments, stateChanges, events int
	mrLinkUpdated                                             string
	version                                                   int32
	state                                                     string
}

func (e *env) glSnapshot(ident string) glSnapshot {
	e.t.Helper()
	is := e.issueFull(ident)
	s := glSnapshot{
		webhookEvents: e.count(`SELECT count(*) FROM git_webhook_events WHERE org_id = $1`, e.orgID),
		mrLinks:       e.count(`SELECT count(*) FROM mr_links WHERE org_id = $1`, e.orgID),
		attachments:   e.count(`SELECT count(*) FROM attachments WHERE org_id = $1`, e.orgID),
		stateChanges:  e.count(`SELECT count(*) FROM issue_state_changes WHERE org_id = $1`, e.orgID),
		events:        e.count(`SELECT count(*) FROM events WHERE org_id = $1`, e.orgID),
		version:       is.Version, state: is.State.Name,
	}
	_ = e.pool.QueryRow(context.Background(), `SELECT coalesce(max(updated_at)::text, '') FROM mr_links WHERE issue_id = $1`, is.ID).Scan(&s.mrLinkUpdated)
	return s
}

// AC: a replayed delivery changes nothing, even when processing it again
// would (a human moved the issue back after the first delivery), and even
// if its body differs in fields outside the key (the key is the Event UUID
// plus project, MR iid and action, decisions-T5 #12b). A different action
// under the same Event UUID is another event (TestGitlabWebhook_SameEventUUIDTwoMRs).
func TestGitlabWebhook_ReplayChangesNothing(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	is := e.createIssue(e.chen, map[string]any{"title": "replay"})
	id := uuid.NewString()
	open := glFixture(t, "mr_open.json", naming(is.Identifier, ""))
	e.hookResult(e.hook("orbit", glSecret, id, open), http.StatusOK)
	e.setState(is.Identifier, "In Progress") // a person moves it back

	before := e.glSnapshot(is.Identifier)
	edited := glFixture(t, "mr_open.json", naming(is.Identifier, "edited later"))
	for _, body := range [][]byte{open, edited} {
		r := e.hookResult(e.hook("orbit", glSecret, id, body), http.StatusOK)
		if r.Status != tracker.WebhookDuplicate || len(r.Linked) != 0 || len(r.Moved) != 0 {
			t.Fatalf("replay result = %+v, want duplicate and nothing", r)
		}
		if after := e.glSnapshot(is.Identifier); after != before {
			t.Fatalf("a replay changed something:\n before %+v\n after  %+v", before, after)
		}
	}
	if before.state != "In Progress" {
		t.Fatalf("state = %q, want In Progress", before.state)
	}
	// The merge under the same Event UUID is a new event, and it moves.
	r := e.hookResult(e.hook("orbit", glSecret, id, glFixture(t, "mr_merge.json", naming(is.Identifier, ""))), http.StatusOK)
	if r.Status != tracker.WebhookProcessed || len(r.Moved) != 1 {
		t.Fatalf("merge under the same Event UUID = %+v, want processed", r)
	}
}

// Concurrent deliveries of one UUID: exactly one is processed.
func TestGitlabWebhook_ConcurrentSameUUID(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	is := e.createIssue(e.chen, map[string]any{"title": "race"})
	id := uuid.NewString()
	body := glFixture(t, "mr_open.json", naming(is.Identifier, ""))
	const n = 8
	statuses := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			req, _ := http.NewRequest("POST", e.srv.URL+"/api/v1/orgs/orbit/integrations/gitlab/webhook", bytes.NewReader(body))
			req.Header.Set(gitlab.HeaderToken, glSecret)
			req.Header.Set(gitlab.HeaderEvent, gitlab.MergeRequestHook)
			req.Header.Set(gitlab.HeaderEventUUID, id)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				statuses[i] = err.Error()
				return
			}
			defer func() { _ = res.Body.Close() }()
			var out tracker.WebhookResult
			if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&out) != nil {
				statuses[i] = fmt.Sprintf("status %d", res.StatusCode)
				return
			}
			statuses[i] = out.Status
		})
	}
	wg.Wait()
	processed := 0
	for _, s := range statuses {
		switch s {
		case tracker.WebhookProcessed:
			processed++
		case tracker.WebhookDuplicate:
		default:
			t.Fatalf("a delivery answered %q", s)
		}
	}
	if processed != 1 {
		t.Fatalf("processed %d times, want once: %v", processed, statuses)
	}
	got := e.issueFull(is.Identifier)
	if gitlabSources(got) != 1 || len(got.Attachments) != 1 {
		t.Fatalf("%d gitlab history rows and %d attachments, want 1 and 1", gitlabSources(got), len(got.Attachments))
	}
}

// AC: a bad secret returns 401, before anything is parsed, and also when the
// org has no integration at all.
func TestGitlabWebhook_Auth(t *testing.T) {
	e := newEnvWith(t, envOpts{logLevel: slog.LevelDebug})
	is := e.createIssue(e.chen, map[string]any{"title": "auth"})
	body := glFixture(t, "mr_open.json", naming(is.Identifier, ""))

	unauth := func(name string, r resp) {
		t.Helper()
		if r.status != http.StatusUnauthorized || r.errCode(t) != "unauthenticated" {
			t.Fatalf("%s: %d %s, want 401 unauthenticated", name, r.status, r.body)
		}
	}
	// No integration in the org yet: any token is refused.
	unauth("no integration", e.hook("orbit", glSecret, uuid.NewString(), body))

	integ := e.glSetup("orbit", e.chen)
	unauth("unknown org", e.hook("no-such-org", glSecret, uuid.NewString(), body))
	unauth("wrong secret", e.hook("orbit", glSecret+"x", uuid.NewString(), body))
	unauth("secret prefix", e.hook("orbit", glSecret[:32], uuid.NewString(), body))
	unauth("no secret", e.hook("orbit", "", uuid.NewString(), body))
	unauth("oversized secret", e.hook("orbit", strings.Repeat("a", 300), uuid.NewString(), body))
	// Nothing is parsed before the token check: garbage, a missing UUID, a
	// wrong event type, a bearer token or an Idempotency-Key header all
	// still get the 401.
	unauth("garbage body", e.hook("orbit", "wrong-secret-wrong-secret-wrong-secret", uuid.NewString(), []byte("{not json")))
	unauth("no event uuid", e.hook("orbit", "wrong-secret-wrong-secret-wrong-secret", "", body))
	unauth("push hook", e.hook("orbit", "wrong-secret-wrong-secret-wrong-secret", uuid.NewString(), body, gitlab.HeaderEvent, "Push Hook"))
	unauth("bearer token instead", e.hook("orbit", "", uuid.NewString(), body, "Authorization", "Bearer "+e.chen))
	unauth("idempotency key", e.hook("orbit", "bad", uuid.NewString(), body, "Idempotency-Key", "k1"))
	if n := e.count(`SELECT count(*) FROM git_webhook_events`); n != 0 {
		t.Fatalf("refused deliveries left %d webhook event rows", n)
	}
	if got := e.issueFull(is.Identifier); got.State.Name != "Backlog" {
		t.Fatalf("a refused delivery moved the issue to %q", got.State.Name)
	}

	// Another org's secret does not open this org.
	org2 := e.newOrg("acme", "ACME")
	admin2 := e.newUser("acmeadmin")
	e.addMember(org2, admin2.ID, "admin")
	tok2, _ := e.newToken(admin2.ID, tokenOpts{pin: &org2})
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/acme/integrations", tok2,
		map[string]any{"provider": "gitlab", "baseUrl": glBaseURL, "secret": "acme-secret-0123456789abcdef0123456789"}))
	unauth("another org's secret", e.hook("orbit", "acme-secret-0123456789abcdef0123456789", uuid.NewString(), body))

	// Disabled: refused.
	e.must(http.StatusOK, e.do("PATCH", "/api/v1/orgs/orbit/integrations/"+integ.String(), e.chen, map[string]any{"enabled": false}))
	unauth("disabled integration", e.hook("orbit", glSecret, uuid.NewString(), body))

	// The secrets are never logged or stored in the clear.
	for _, s := range []string{glSecret, "acme-secret-0123456789abcdef0123456789"} {
		if strings.Contains(e.logs.String(), s) {
			t.Fatal("a webhook secret is in the server log")
		}
		if n := e.count(`SELECT count(*) FROM events WHERE payload::text LIKE '%' || $1 || '%'`, s); n != 0 {
			t.Fatal("a webhook secret is in an event payload")
		}
	}
	if n := e.count(`SELECT count(*) FROM idempotency_keys`); n != 0 {
		t.Fatalf("idempotency rows = %d; the secret routes must never store one", n)
	}
}

// AC: an unmapped project returns 202 and is logged without payload content.
func TestGitlabWebhook_UnmappedProject(t *testing.T) {
	e := newEnvWith(t, envOpts{logLevel: slog.LevelDebug})
	e.glSetup("orbit", e.chen)
	is := e.createIssue(e.chen, map[string]any{"title": "unmapped"})
	body := glFixture(t, "mr_open.json", func(a map[string]any) {
		naming(is.Identifier, "zebra-private-description")(a)
		a["title"] = is.Identifier + ": zebra-private-title"
	})
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	m["project"].(map[string]any)["id"] = 999
	body, _ = json.Marshal(m)

	r := e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), body), http.StatusAccepted)
	if r.Status != tracker.WebhookUnmapped || len(r.Linked) != 0 {
		t.Fatalf("result = %+v, want unmapped", r)
	}
	if got := e.issueFull(is.Identifier); got.State.Name != "Backlog" || len(got.Attachments) != 0 {
		t.Fatalf("an unmapped project changed the issue: %q, %d attachments", got.State.Name, len(got.Attachments))
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "unmapped project") || !strings.Contains(logs, "gitlab_project_id=999") {
		t.Fatalf("no unmapped-project log line with the project id:\n%s", logs)
	}
	for _, s := range []string{"zebra-private", strings.ToLower(is.Identifier) + "-fix", "exampledev", "merge_requests/17"} {
		if strings.Contains(logs, s) {
			t.Fatalf("payload content %q is in the log", s)
		}
	}
	if n := e.count(`SELECT count(*) FROM git_webhook_events`); n != 0 {
		t.Fatalf("an unmapped delivery kept %d webhook event rows, want 0 (a resend after mapping must apply)", n)
	}
}

// An unmapped delivery keeps no event row, so after an admin maps the
// project, a resend of the same delivery (same key) is applied.
func TestGitlabWebhook_MapThenResendApplies(t *testing.T) {
	e := newEnv(t)
	integ := e.glSetup("orbit", e.chen)
	is := e.createIssue(e.chen, map[string]any{"title": "map then resend"})
	body := glFixture(t, "mr_open.json", naming(is.Identifier, ""))
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	m["project"].(map[string]any)["id"] = 77
	body, _ = json.Marshal(m)
	id := uuid.NewString()

	for range 2 { // unmapped twice: still 202, never "duplicate"
		if r := e.hookResult(e.hook("orbit", glSecret, id, body), http.StatusAccepted); r.Status != tracker.WebhookUnmapped {
			t.Fatalf("unmapped = %+v", r)
		}
	}
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/orbit/integrations/"+integ.String()+"/repos", e.chen,
		map[string]any{"repoKey": "77", "repoPath": "example-org/late"}))
	r := e.hookResult(e.hook("orbit", glSecret, id, body), http.StatusOK)
	if r.Status != tracker.WebhookProcessed || len(r.Moved) != 1 {
		t.Fatalf("resend after mapping = %+v, want processed and moved", r)
	}
	if got := e.issueFull(is.Identifier); got.State.Name != "In Review" {
		t.Fatalf("state = %q, want In Review", got.State.Name)
	}
	if r := e.hookResult(e.hook("orbit", glSecret, id, body), http.StatusOK); r.Status != tracker.WebhookDuplicate {
		t.Fatalf("a second resend = %+v, want duplicate", r)
	}
}

func TestGitlabWebhook_RequestChecks(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	is := e.createIssue(e.chen, map[string]any{"title": "checks"})
	body := glFixture(t, "mr_open.json", naming(is.Identifier, ""))

	// Not a merge_request event: 202, never read.
	r := e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), []byte("not json"), gitlab.HeaderEvent, "Push Hook"), http.StatusAccepted)
	if r.Status != tracker.WebhookIgnored {
		t.Fatalf("push hook = %+v", r)
	}
	if code := e.must(http.StatusBadRequest, e.hook("orbit", glSecret, "", body)).errCode(t); code != "bad_request" {
		t.Fatalf("missing event uuid code = %s", code)
	}
	e.must(http.StatusBadRequest, e.hook("orbit", glSecret, strings.Repeat("u", 101), body))
	e.must(http.StatusBadRequest, e.hook("orbit", glSecret, uuid.NewString(), []byte("{not json")))
	e.must(http.StatusBadRequest, e.hook("orbit", glSecret, uuid.NewString(), []byte(`{"object_kind":"push","project":{"id":42}}`)))
	badURL := glFixture(t, "mr_open.json", func(a map[string]any) { naming(is.Identifier, "")(a); a["url"] = "javascript:alert(1)" })
	e.must(http.StatusBadRequest, e.hook("orbit", glSecret, uuid.NewString(), badURL))

	big := append([]byte(`{"object_kind":"merge_request","x":"`), bytes.Repeat([]byte("a"), maxWebhookBodyBytes)...)
	big = append(big, []byte(`"}`)...)
	if code := e.must(http.StatusRequestEntityTooLarge, e.hook("orbit", glSecret, uuid.NewString(), big)).errCode(t); code != "payload_too_large" {
		t.Fatalf("oversized body code = %s", code)
	}
	if n := e.count(`SELECT count(*) FROM git_webhook_events`); n != 0 {
		t.Fatalf("refused requests left %d webhook event rows", n)
	}
	if got := e.issueFull(is.Identifier); got.State.Name != "Backlog" {
		t.Fatalf("a refused request moved the issue to %q", got.State.Name)
	}
}

// update / draft / close: only "marked ready" moves; a title edit and a
// close move nothing.
func TestGitlabWebhook_UpdateDraftClose(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	is := e.createIssue(e.chen, map[string]any{"title": "draft flow"})

	r := e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_draft_open.json", naming(is.Identifier, ""))), http.StatusOK)
	if len(r.Linked) != 1 || len(r.Moved) != 0 {
		t.Fatalf("draft open = %+v, want linked and not moved", r)
	}
	if got := e.issueFull(is.Identifier); got.State.Name != "Backlog" || len(got.Attachments) != 1 {
		t.Fatalf("after draft open: %q with %d attachments", got.State.Name, len(got.Attachments))
	}

	before := e.glSnapshot(is.Identifier)
	r = e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_update.json", naming(is.Identifier, ""))), http.StatusOK)
	// The answer says processed (decisions-T20 #65); the event row says ignored.
	if r.Status != tracker.WebhookProcessed {
		t.Fatalf("title update = %+v, want processed", r)
	}
	after := e.glSnapshot(is.Identifier)
	before.webhookEvents++ // the update is recorded (status ignored) and nothing else
	if after != before {
		t.Fatalf("a title update changed something:\n before %+v\n after  %+v", before, after)
	}

	r = e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_update_ready.json", naming(is.Identifier, ""))), http.StatusOK)
	if len(r.Moved) != 1 {
		t.Fatalf("marked ready = %+v, want moved", r)
	}
	if got := e.issueFull(is.Identifier); got.State.Name != "In Review" || len(got.Attachments) != 1 {
		t.Fatalf("after ready: %q with %d attachments, want In Review and 1", got.State.Name, len(got.Attachments))
	}

	// Close of the ready MR (iid 18): no move, its link records closed.
	r = e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_close.json", func(a map[string]any) {
		naming(is.Identifier, "")(a)
		a["iid"] = 18
	})), http.StatusOK)
	if r.Status != tracker.WebhookProcessed || len(r.Moved) != 0 {
		t.Fatalf("close = %+v", r)
	}
	got := e.issueFull(is.Identifier)
	if got.State.Name != "In Review" || gitlabSources(got) != 1 {
		t.Fatalf("after close: %q with %d gitlab rows, want In Review and 1", got.State.Name, gitlabSources(got))
	}
	if n := e.count(`SELECT count(*) FROM mr_links WHERE issue_id = $1 AND number = 18 AND state = 'closed'`, is.ID); n != 1 {
		t.Fatalf("closed mr_links = %d, want 1", n)
	}
}

// Linking keeps only team keys of this org (and of the mapped team), in
// branch, title, description order.
func TestGitlabWebhook_LinkingRules(t *testing.T) {
	e := newEnv(t)
	integ := e.glSetup("orbit", e.chen)
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/orbit/teams", e.chen, map[string]any{"key": "WEB", "name": "Web"}))
	a := e.createIssue(e.chen, map[string]any{"title": "a"})
	b := e.createIssue(e.chen, map[string]any{"title": "b", "team": "WEB"})

	// A second org with team ACME and an issue ACME-1 that orbit must never touch.
	org2 := e.newOrg("acme", "ACME")
	admin2 := e.newUser("acmeadmin")
	e.addMember(org2, admin2.ID, "admin")
	tok2, _ := e.newToken(admin2.ID, tokenOpts{pin: &org2})
	var acme tracker.Issue
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/acme/issues", tok2, map[string]any{"team": "ACME", "title": "acme"})).json(t, &acme)

	desc := "Also " + b.Identifier + " and " + acme.Identifier + ", FOO-1, UTF-8, sha-256."
	r := e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_open.json", naming(a.Identifier, desc))), http.StatusOK)
	if strings.Join(r.Linked, ",") != a.Identifier+","+b.Identifier && strings.Join(r.Linked, ",") != b.Identifier+","+a.Identifier {
		t.Fatalf("linked = %v, want %s and %s only", r.Linked, a.Identifier, b.Identifier)
	}
	var acmeState string
	if err := e.pool.QueryRow(context.Background(), `SELECT s.name FROM issues i JOIN workflow_states s ON s.id = i.state_id WHERE i.id = $1`, acme.ID).Scan(&acmeState); err != nil {
		t.Fatal(err)
	}
	if acmeState != "Backlog" || e.count(`SELECT count(*) FROM mr_links WHERE org_id = $1`, org2) != 0 {
		t.Fatalf("another org's issue was touched: %s", acmeState)
	}

	// A mapping with a team links only that team's issues.
	var p tracker.GitRepo
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/orbit/integrations/"+integ.String()+"/repos", e.chen,
		map[string]any{"repoKey": "43", "repoPath": "example-org/web", "teamId": "web"})).json(t, &p)
	if p.Team == nil || p.Team.Key != "WEB" {
		t.Fatalf("mapping team = %+v", p.Team)
	}
	c := e.createIssue(e.chen, map[string]any{"title": "c"})
	d := e.createIssue(e.chen, map[string]any{"title": "d", "team": "WEB"})
	body := glFixture(t, "mr_open.json", naming(c.Identifier, "and "+d.Identifier))
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	m["project"].(map[string]any)["id"] = 43
	body, _ = json.Marshal(m)
	r = e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), body), http.StatusOK)
	if len(r.Linked) != 1 || r.Linked[0] != d.Identifier {
		t.Fatalf("team-limited mapping linked %v, want only %s", r.Linked, d.Identifier)
	}
}

// A database error is a 500 that keeps nothing (no webhook row, no move),
// so the same delivery processed again afterwards runs in full. Whether GitLab
// sends it again by itself is not tested or relied on.
func TestGitlabWebhook_DBErrorIs500AndRetryWorks(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	is := e.createIssue(e.chen, map[string]any{"title": "db error"})
	id := uuid.NewString()
	body := glFixture(t, "mr_open.json", naming(is.Identifier, ""))
	ctx := context.Background()

	if _, err := e.pool.Exec(ctx, `ALTER TABLE mr_links RENAME TO mr_links_gone`); err != nil {
		t.Fatal(err)
	}
	if code := e.must(http.StatusInternalServerError, e.hook("orbit", glSecret, id, body)).errCode(t); code != "internal_error" {
		t.Fatalf("code = %s", code)
	}
	if n := e.count(`SELECT count(*) FROM git_webhook_events`); n != 0 {
		t.Fatalf("a failed event kept %d webhook rows; the retry would be a duplicate", n)
	}
	if got := e.issueFull(is.Identifier); got.State.Name != "Backlog" {
		t.Fatalf("a failed event moved the issue to %q", got.State.Name)
	}
	if _, err := e.pool.Exec(ctx, `ALTER TABLE mr_links_gone RENAME TO mr_links`); err != nil {
		t.Fatal(err)
	}
	r := e.hookResult(e.hook("orbit", glSecret, id, body), http.StatusOK)
	if r.Status != tracker.WebhookProcessed || len(r.Moved) != 1 {
		t.Fatalf("retry = %+v, want processed and moved", r)
	}
}

// GitLab can deliver out of order. Merged is terminal for a link: a delayed
// open (with an older title) or close after the merge changes neither the
// link nor the issue.
func TestGitlabWebhook_OutOfOrderAfterMerge(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	is := e.createIssue(e.chen, map[string]any{"title": "out of order"})
	e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_merge.json", naming(is.Identifier, ""))), http.StatusOK)

	linkRow := func() (state, title, branch string) {
		t.Helper()
		if err := e.pool.QueryRow(context.Background(),
			`SELECT state, title, source_branch FROM mr_links WHERE issue_id = $1 AND number = 17`, is.ID).Scan(&state, &title, &branch); err != nil {
			t.Fatal(err)
		}
		return
	}
	st, title, branch := linkRow()
	if st != "merged" {
		t.Fatalf("after merge: link state %q", st)
	}
	before := e.issueFull(is.Identifier)

	stale := glFixture(t, "mr_open.json", func(a map[string]any) {
		naming(is.Identifier, "")(a)
		a["title"] = is.Identifier + ": an older title"
		a["source_branch"] = "chen/" + strings.ToLower(is.Identifier) + "-older-branch"
	})
	r := e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), stale), http.StatusOK)
	if len(r.Moved) != 0 {
		t.Fatalf("a delayed open moved the issue: %+v", r)
	}
	if st2, title2, branch2 := linkRow(); st2 != "merged" || title2 != title || branch2 != branch {
		t.Fatalf("a delayed open changed the merged link: %q %q %q, want merged %q %q", st2, title2, branch2, title, branch)
	}
	e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_close.json", naming(is.Identifier, ""))), http.StatusOK)
	if st3, _, _ := linkRow(); st3 != "merged" {
		t.Fatalf("a delayed close made the merged link %q", st3)
	}
	after := e.issueFull(is.Identifier)
	if after.State.Name != "In Testing" || after.Version != before.Version || gitlabSources(after) != 1 || len(after.Attachments) != 1 {
		t.Fatalf("the issue changed: %q v%d -> %q v%d, %d gitlab rows, %d attachments",
			before.State.Name, before.Version, after.State.Name, after.Version, gitlabSources(after), len(after.Attachments))
	}
}

// The MR URL is checked only after the duplicate and unmapped answers: an
// unmapped project with an odd URL is still 202, a replay still duplicate,
// and a mapped project's bad URL is a 400 that keeps nothing.
func TestGitlabWebhook_BadURLOrder(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	is := e.createIssue(e.chen, map[string]any{"title": "bad url"})
	badURL := func(project int) []byte {
		b := glFixture(t, "mr_open.json", func(a map[string]any) { naming(is.Identifier, "")(a); a["url"] = "javascript:alert(1)" })
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		m["project"].(map[string]any)["id"] = project
		out, _ := json.Marshal(m)
		return out
	}
	if r := e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), badURL(999)), http.StatusAccepted); r.Status != tracker.WebhookUnmapped {
		t.Fatalf("unmapped with a bad url = %+v, want unmapped", r)
	}
	id := uuid.NewString()
	e.hookResult(e.hook("orbit", glSecret, id, glFixture(t, "mr_open.json", naming(is.Identifier, ""))), http.StatusOK)
	if r := e.hookResult(e.hook("orbit", glSecret, id, badURL(glProjectID)), http.StatusOK); r.Status != tracker.WebhookDuplicate {
		t.Fatalf("replay with a bad url = %+v, want duplicate", r)
	}
	rows := e.count(`SELECT count(*) FROM git_webhook_events`)
	e.must(http.StatusBadRequest, e.hook("orbit", glSecret, uuid.NewString(), badURL(glProjectID)))
	if n := e.count(`SELECT count(*) FROM git_webhook_events`); n != rows {
		t.Fatalf("a 400 kept a webhook event row (%d -> %d)", rows, n)
	}
}

// Owner decision (2026-10-24): an issue named only in the MR description is
// linked but keeps its state; only branch and title references move. An
// issue named in the title and the description moves once.
func TestGitlabWebhook_DescriptionOnlyLinksButDoesNotMove(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	a := e.createIssue(e.chen, map[string]any{"title": "in the branch"})
	b := e.createIssue(e.chen, map[string]any{"title": "in the description only"})
	c := e.createIssue(e.chen, map[string]any{"title": "in the title and the description"})
	mut := func(attrs map[string]any) {
		attrs["source_branch"] = "chen/" + strings.ToLower(a.Identifier) + "-fix"
		attrs["title"] = c.Identifier + ": Fix login redirect"
		attrs["description"] = "Refs " + b.Identifier + ". Also closes " + c.Identifier + "."
	}
	bBefore := e.issueFull(b.Identifier)
	for _, step := range []struct{ fixture, state string }{{"mr_open.json", "In Review"}, {"mr_merge.json", "In Testing"}} {
		r := e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, step.fixture, mut)), http.StatusOK)
		if want := map[string]int{"mr_open.json": 3, "mr_merge.json": 0}[step.fixture]; len(r.Linked) != want {
			t.Fatalf("%s: newly linked %v, want %d (linked means new links of this event)", step.fixture, r.Linked, want)
		}
		for _, m := range r.Moved {
			if m == b.Identifier {
				t.Fatalf("%s moved the description-only issue %s: %+v", step.fixture, b.Identifier, r)
			}
		}
		if len(r.Moved) != 2 {
			t.Fatalf("%s: moved %v, want %s and %s", step.fixture, r.Moved, a.Identifier, c.Identifier)
		}
		for _, is := range []tracker.Issue{a, c} {
			if got := e.issueFull(is.Identifier); got.State.Name != step.state {
				t.Fatalf("%s: %s is %q, want %q", step.fixture, is.Identifier, got.State.Name, step.state)
			}
		}
	}
	bAfter := e.issueFull(b.Identifier)
	if bAfter.State.Name != "Backlog" || bAfter.Version != bBefore.Version || len(bAfter.StateHistory) != len(bBefore.StateHistory) || gitlabSources(bAfter) != 0 {
		t.Fatalf("description-only issue changed: %q v%d (%d history rows) -> %q v%d (%d rows)",
			bBefore.State.Name, bBefore.Version, len(bBefore.StateHistory), bAfter.State.Name, bAfter.Version, len(bAfter.StateHistory))
	}
	if len(bAfter.Attachments) != 1 || e.count(`SELECT count(*) FROM mr_links WHERE issue_id = $1 AND state = 'merged'`, b.ID) != 1 {
		t.Fatalf("description-only issue is not linked: %d attachments", len(bAfter.Attachments))
	}
	if got := e.issueFull(c.Identifier); gitlabSources(got) != 2 {
		t.Fatalf("%s (title and description) has %d gitlab history rows, want 2 (one per event)", c.Identifier, gitlabSources(got))
	}
}

// One GitLab request (a push that merges two MRs) can give both hooks the
// same X-Gitlab-Event-UUID. Both MRs must be processed; the same delivery
// sent again is still a duplicate.
func TestGitlabWebhook_SameEventUUIDTwoMRs(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	a := e.createIssue(e.chen, map[string]any{"title": "first mr"})
	b := e.createIssue(e.chen, map[string]any{"title": "second mr"})
	shared := uuid.NewString()
	first := glFixture(t, "mr_merge.json", naming(a.Identifier, ""))
	second := glFixture(t, "mr_merge.json", func(attrs map[string]any) {
		naming(b.Identifier, "")(attrs)
		attrs["iid"] = 18
		attrs["url"] = "https://gitlab.example.com/example-org/example-app/-/merge_requests/18"
	})
	for _, d := range []struct {
		body  []byte
		ident string
	}{{first, a.Identifier}, {second, b.Identifier}} {
		r := e.hookResult(e.hook("orbit", glSecret, shared, d.body), http.StatusOK)
		if r.Status != tracker.WebhookProcessed || len(r.Moved) != 1 || r.Moved[0] != d.ident {
			t.Fatalf("MR of %s under a shared Event UUID = %+v, want processed and moved", d.ident, r)
		}
		if got := e.issueFull(d.ident); got.State.Name != "In Testing" {
			t.Fatalf("%s is %q, want In Testing", d.ident, got.State.Name)
		}
	}
	before := e.glSnapshot(a.Identifier)
	if r := e.hookResult(e.hook("orbit", glSecret, shared, first), http.StatusOK); r.Status != tracker.WebhookDuplicate {
		t.Fatalf("the same delivery again = %+v, want duplicate", r)
	}
	if after := e.glSnapshot(a.Identifier); after != before {
		t.Fatalf("a duplicate changed something:\n before %+v\n after  %+v", before, after)
	}
}

// GitLab's Idempotency-Key, when sent, is the dedupe key: the same key under
// another Event UUID is a duplicate; another key under the same Event UUID
// and MR is a new delivery.
func TestGitlabWebhook_IdempotencyKeyHeader(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	is := e.createIssue(e.chen, map[string]any{"title": "idempotency key"})
	body := glFixture(t, "mr_open.json", naming(is.Identifier, ""))
	u1 := uuid.NewString()
	key := "a0b1c2d3-e4f5-4678-9abc-def012345678"
	if r := e.hookResult(e.hook("orbit", glSecret, u1, body, "Idempotency-Key", key), http.StatusOK); r.Status != tracker.WebhookProcessed {
		t.Fatalf("first = %+v", r)
	}
	if r := e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), body, "Idempotency-Key", key), http.StatusOK); r.Status != tracker.WebhookDuplicate {
		t.Fatalf("same Idempotency-Key, other Event UUID = %+v, want duplicate", r)
	}
	// Both headers are recorded: a resend that changes or drops the
	// Idempotency-Key but keeps the Event UUID is still a duplicate.
	if r := e.hookResult(e.hook("orbit", glSecret, u1, body, "Idempotency-Key", "another-key"), http.StatusOK); r.Status != tracker.WebhookDuplicate {
		t.Fatalf("other Idempotency-Key, same Event UUID = %+v, want duplicate", r)
	}
	if r := e.hookResult(e.hook("orbit", glSecret, u1, body), http.StatusOK); r.Status != tracker.WebhookDuplicate {
		t.Fatalf("no Idempotency-Key, same Event UUID = %+v, want duplicate", r)
	}
	if n := e.count(`SELECT count(*) FROM git_webhook_events WHERE event_uuid = $1 AND status = 'processed'`, "gl:ev:"+u1+":p42:mr17:open"); n != 1 {
		t.Fatalf("rows keyed by the Event UUID = %d, want 1 (both keys are recorded)", n)
	}
	if n := e.count(`SELECT count(*) FROM git_webhook_events WHERE event_uuid LIKE 'gl:ik:another-key%'`); n != 0 {
		t.Fatalf("a duplicate kept its new Idempotency-Key row (%d)", n)
	}
	if n := e.count(`SELECT count(*) FROM git_webhook_events WHERE event_uuid = $1`, "gl:ik:"+key+":p42:mr17:open"); n != 1 {
		t.Fatalf("rows keyed by the Idempotency-Key = %d, want 1", n)
	}
	rows := e.count(`SELECT count(*) FROM git_webhook_events`)
	e.must(http.StatusBadRequest, e.hook("orbit", glSecret, uuid.NewString(), body, "Idempotency-Key", strings.Repeat("k", 101)))
	e.must(http.StatusBadRequest, e.hook("orbit", glSecret, uuid.NewString(), body, "Idempotency-Key", "has space"))
	if n := e.count(`SELECT count(*) FROM git_webhook_events`); n != rows {
		t.Fatalf("a bad Idempotency-Key kept a row")
	}
}

// If GitLab gives the hooks of one request the same Idempotency-Key, two MRs
// in one push must still both be processed; a resend of one is a duplicate.
func TestGitlabWebhook_SameIdempotencyKeyTwoMRs(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	a := e.createIssue(e.chen, map[string]any{"title": "first mr"})
	b := e.createIssue(e.chen, map[string]any{"title": "second mr"})
	key := "shared-idempotency-key-0001"
	first := glFixture(t, "mr_merge.json", naming(a.Identifier, ""))
	second := glFixture(t, "mr_merge.json", func(attrs map[string]any) {
		naming(b.Identifier, "")(attrs)
		attrs["iid"] = 18
		attrs["url"] = "https://gitlab.example.com/example-org/example-app/-/merge_requests/18"
	})
	for _, d := range []struct {
		body  []byte
		ident string
	}{{first, a.Identifier}, {second, b.Identifier}} {
		r := e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), d.body, "Idempotency-Key", key), http.StatusOK)
		if r.Status != tracker.WebhookProcessed || len(r.Moved) != 1 || r.Moved[0] != d.ident {
			t.Fatalf("MR of %s under a shared Idempotency-Key = %+v, want processed and moved", d.ident, r)
		}
	}
	if r := e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), first, "Idempotency-Key", key), http.StatusOK); r.Status != tracker.WebhookDuplicate {
		t.Fatalf("a resend of the first = %+v, want duplicate", r)
	}
}

// Two enabled integrations of one org may not share a secret (the token
// check could not tell them apart). Another org may use the same one.
func TestGitlabConfig_SecretReuse(t *testing.T) {
	e := newEnv(t)
	base := "/api/v1/orgs/orbit/integrations"
	var first tracker.GitIntegration
	e.must(http.StatusCreated, e.do("POST", base, e.chen, map[string]any{"provider": "gitlab", "baseUrl": glBaseURL, "secret": glSecret})).json(t, &first)
	if code := e.must(http.StatusConflict, e.do("POST", base, e.chen,
		map[string]any{"provider": "gitlab", "baseUrl": "https://gitlab.other.example", "secret": glSecret})).errCode(t); code != "secret_in_use" {
		t.Fatalf("reused secret on create: code %s", code)
	}
	// A disabled one may hold it, but cannot be enabled while the first is.
	var second tracker.GitIntegration
	e.must(http.StatusCreated, e.do("POST", base, e.chen,
		map[string]any{"provider": "gitlab", "baseUrl": "https://gitlab.other.example", "secret": glSecret, "enabled": false})).json(t, &second)
	e.must(http.StatusConflict, e.do("PATCH", base+"/"+second.ID.String(), e.chen, map[string]any{"enabled": true}))
	// Once the first is off, the second can be enabled.
	e.must(http.StatusOK, e.do("PATCH", base+"/"+first.ID.String(), e.chen, map[string]any{"enabled": false}))
	e.must(http.StatusOK, e.do("PATCH", base+"/"+second.ID.String(), e.chen, map[string]any{"enabled": true}))

	org2 := e.newOrg("acme", "ACME")
	admin2 := e.newUser("acmeadmin")
	e.addMember(org2, admin2.ID, "admin")
	tok2, _ := e.newToken(admin2.ID, tokenOpts{pin: &org2})
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/acme/integrations", tok2, map[string]any{"provider": "gitlab", "baseUrl": glBaseURL, "secret": glSecret}))
}

// Two GitLab instances of one org whose projects share a numeric id and an
// MR iid get separate links (migration 0004): a close on one leaves the
// other's link alone.
func TestGitlabWebhook_TwoIntegrationsSameProjectAndIID(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen) // gitlab.example.com, glSecret, project 42
	otherSecret := "other-instance-secret-0123456789abcdef01"
	var other tracker.GitIntegration
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/orbit/integrations", e.chen,
		map[string]any{"provider": "gitlab", "baseUrl": "https://gitlab.other.example", "secret": otherSecret})).json(t, &other)
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/orbit/integrations/"+other.ID.String()+"/repos", e.chen,
		map[string]any{"repoKey": strconv.Itoa(glProjectID), "repoPath": "other/example-app"}))
	is := e.createIssue(e.chen, map[string]any{"title": "two instances"})
	open := glFixture(t, "mr_open.json", naming(is.Identifier, ""))

	for _, secret := range []string{glSecret, otherSecret} {
		r := e.hookResult(e.hook("orbit", secret, uuid.NewString(), open), http.StatusOK)
		if len(r.Linked) != 1 || r.Linked[0] != is.Identifier {
			t.Fatalf("open from the instance with secret %.6s...: %+v, want a new link", secret, r)
		}
	}
	if n := e.count(`SELECT count(*) FROM mr_links WHERE issue_id = $1 AND repo_key = '42' AND number = 17`, is.ID); n != 2 {
		t.Fatalf("mr_links rows = %d, want one per integration", n)
	}
	if got := e.issueFull(is.Identifier); len(got.Attachments) != 2 {
		t.Fatalf("attachments = %d, want one per integration", len(got.Attachments))
	}
	e.hookResult(e.hook("orbit", otherSecret, uuid.NewString(), glFixture(t, "mr_close.json", naming(is.Identifier, ""))), http.StatusOK)
	if n := e.count(`SELECT count(*) FROM mr_links WHERE issue_id = $1 AND integration_id = $2 AND state = 'closed'`, is.ID, other.ID); n != 1 {
		t.Fatalf("the closing instance's link is not closed (%d)", n)
	}
	if n := e.count(`SELECT count(*) FROM mr_links WHERE issue_id = $1 AND integration_id <> $2 AND state = 'opened'`, is.ID, other.ID); n != 1 {
		t.Fatalf("a close on one instance changed the other's link (%d opened left)", n)
	}
}

// A closed link stays closed against a delayed open or update, and such a
// stale event does not move the issue; an explicit reopen opens it and moves.
func TestGitlabWebhook_StaleOpenAfterClose(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	is := e.createIssue(e.chen, map[string]any{"title": "closed then stale open"})
	e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_open.json", naming(is.Identifier, ""))), http.StatusOK)
	e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_close.json", naming(is.Identifier, ""))), http.StatusOK)
	e.setState(is.Identifier, "Todo") // the MR was abandoned; a person moves the ticket back

	linkState := func() string {
		t.Helper()
		var s string
		if err := e.pool.QueryRow(context.Background(), `SELECT state FROM mr_links WHERE issue_id = $1 AND number = 17`, is.ID).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if linkState() != "closed" {
		t.Fatalf("after close: link %q", linkState())
	}
	// A delayed open, and a delayed "marked ready" update, after the close.
	readyAfterClose := glFixture(t, "mr_update_ready.json", func(a map[string]any) { naming(is.Identifier, "")(a); a["iid"] = 17 })
	for _, body := range [][]byte{glFixture(t, "mr_open.json", naming(is.Identifier, "")), readyAfterClose} {
		r := e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), body), http.StatusOK)
		if len(r.Moved) != 0 {
			t.Fatalf("a stale event after the close moved the issue: %+v", r)
		}
		if linkState() != "closed" {
			t.Fatalf("a stale event reopened the closed link: %q", linkState())
		}
	}
	if got := e.issueFull(is.Identifier); got.State.Name != "Todo" {
		t.Fatalf("issue = %q, want Todo", got.State.Name)
	}
	r := e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_reopen.json", naming(is.Identifier, ""))), http.StatusOK)
	if len(r.Moved) != 1 || linkState() != "opened" {
		t.Fatalf("an explicit reopen = %+v, link %q; want moved and opened", r, linkState())
	}
	if got := e.issueFull(is.Identifier); got.State.Name != "In Review" {
		t.Fatalf("after reopen = %q, want In Review", got.State.Name)
	}
	// A merge after a close (closed -> merged) is never a step back.
	e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_close.json", naming(is.Identifier, ""))), http.StatusOK)
	r = e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_merge.json", naming(is.Identifier, ""))), http.StatusOK)
	if len(r.Moved) != 1 || linkState() != "merged" {
		t.Fatalf("merge after close = %+v, link %q; want moved and merged", r, linkState())
	}
}

// https://x and https://x:443 are one GitLab instance.
func TestGitlabConfig_DefaultPortIsTheSameInstance(t *testing.T) {
	e := newEnv(t)
	base := "/api/v1/orgs/orbit/integrations"
	e.must(http.StatusCreated, e.do("POST", base, e.chen, map[string]any{"provider": "gitlab", "baseUrl": "https://gitlab.example.com", "secret": glSecret}))
	if code := e.must(http.StatusConflict, e.do("POST", base, e.chen,
		map[string]any{"provider": "gitlab", "baseUrl": "https://gitlab.example.com:443/", "secret": "another-secret-0123456789abcdef0123456789"})).errCode(t); code != "integration_exists" {
		t.Fatalf("default-port duplicate code = %s", code)
	}
}

// A close that commits while a stale open waits on the link's row lock must
// win: the open re-reads the committed close (it is stale), the link stays
// closed and the issue does not move. A transaction of the test plays the
// in-flight close: it locks the link row (SELECT ... FOR UPDATE), waits until
// the webhook's LockMRLink is blocked on that lock, then writes the close
// and commits. (A plain row lock, not an early UPDATE, so the webhook's
// INSERT ... ON CONFLICT passes and the wait happens in LockMRLink itself.)
func TestGitlabWebhook_StaleOpenDuringConcurrentClose(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	is := e.createIssue(e.chen, map[string]any{"title": "close races a stale open"})
	e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_open.json", naming(is.Identifier, ""))), http.StatusOK)
	e.setState(is.Identifier, "Todo") // the MR is being abandoned; a person moved the ticket back
	ctx := context.Background()
	linkRow := func() (state, title, url, branch string) {
		t.Helper()
		if err := e.pool.QueryRow(ctx, `SELECT state, title, url, source_branch FROM mr_links WHERE issue_id = $1 AND number = 17`, is.ID).
			Scan(&state, &title, &url, &branch); err != nil {
			t.Fatal(err)
		}
		return
	}
	_, wantTitle, wantURL, wantBranch := linkRow()

	// The in-flight close: it holds the row lock until commit.
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if tag, err := tx.Exec(ctx, `SELECT 1 FROM mr_links WHERE issue_id = $1 AND number = 17 FOR UPDATE`, is.ID); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("lock the link: %v (%d rows)", err, tag.RowsAffected())
	}

	// The stale open (a delayed delivery of an earlier open), with another
	// title, URL and branch: none of them may land on the closed link.
	stale := glFixture(t, "mr_open.json", func(a map[string]any) {
		naming(is.Identifier, "")(a)
		a["title"] = is.Identifier + ": a stale title"
		a["url"] = "https://gitlab.example.com/example-org/example-app/-/merge_requests/17?stale=1"
		a["source_branch"] = "chen/" + strings.ToLower(is.Identifier) + "-stale-branch"
	})
	type result struct {
		status int
		body   []byte
		err    error
	}
	done := make(chan result, 1)
	go func() {
		req, err := http.NewRequest("POST", e.srv.URL+"/api/v1/orgs/orbit/integrations/gitlab/webhook", bytes.NewReader(stale))
		if err != nil {
			done <- result{err: err}
			return
		}
		req.Header.Set(gitlab.HeaderToken, glSecret)
		req.Header.Set(gitlab.HeaderEvent, gitlab.MergeRequestHook)
		req.Header.Set(gitlab.HeaderEventUUID, uuid.NewString())
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer func() { _ = res.Body.Close() }()
		b, err := io.ReadAll(res.Body)
		done <- result{status: res.StatusCode, body: b, err: err}
	}()

	// Wait until the webhook is blocked on a lock held by the close.
	waiting := false
	for range 200 {
		var n int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND query LIKE '%name: LockMRLink%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			waiting = true
			break
		}
		select {
		case r := <-done:
			t.Fatalf("the stale open finished without waiting on the close's lock: %d %s %v", r.status, r.body, r.err)
		case <-time.After(25 * time.Millisecond):
		}
	}
	if !waiting {
		t.Fatal("the stale open never waited on the close's row lock; the race was not set up")
	}
	if _, err := tx.Exec(ctx, `UPDATE mr_links SET state = 'closed', updated_at = now() WHERE issue_id = $1 AND number = 17`, is.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var r result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the stale open did not finish after the close committed")
	}
	if r.err != nil || r.status != http.StatusOK {
		t.Fatalf("stale open = %d %s %v", r.status, r.body, r.err)
	}
	// The answer is a status only (decisions-T20 #65); the state check below
	// shows the stale open moved nothing.
	var out tracker.WebhookResult
	if err := json.Unmarshal(r.body, &out); err != nil || out.Status != tracker.WebhookProcessed {
		t.Fatalf("stale open answer %s: %v", r.body, err)
	}
	state, title, url, branch := linkRow()
	if state != "closed" {
		t.Fatalf("the stale open overwrote the concurrent close: link %q", state)
	}
	if title != wantTitle || url != wantURL || branch != wantBranch {
		t.Fatalf("the stale open changed the closed link's row: %q %q %q, want %q %q %q", title, url, branch, wantTitle, wantURL, wantBranch)
	}
	if got := e.issueFull(is.Identifier); got.State.Name != "Todo" {
		t.Fatalf("issue = %q, want Todo", got.State.Name)
	}
}

// LAUN-82: a GitHub integration row (inserted directly; there is no create
// route yet) is never a GitLab token candidate and is invisible to the old
// GitLab config routes; another org's GitLab integration is a 404.
func TestGitlabRoutes_IgnoreOtherProviders(t *testing.T) {
	e := newEnv(t)
	is := e.createIssue(e.chen, map[string]any{"title": "provider"})
	body := glFixture(t, "mr_open.json", naming(is.Identifier, ""))
	sum := sha256.Sum256([]byte(glSecret))
	var gh uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO git_integrations (id, org_id, provider, base_url, webhook_secret_hash)
		VALUES ($1, $2, 'github', 'https://github.com', $3) RETURNING id`, uuid.Must(uuid.NewV7()), e.orgID, sum[:]).Scan(&gh); err != nil {
		t.Fatal(err)
	}

	// The GitHub row holds the hash of glSecret, yet the GitLab webhook refuses it.
	r := e.hook("orbit", glSecret, uuid.NewString(), body)
	if r.status != http.StatusUnauthorized || r.errCode(t) != "unauthenticated" {
		t.Fatalf("webhook with a GitHub row's hash: %d %s, want 401", r.status, r.body)
	}
	// The GitHub row is in the provider-neutral list, with its provider.
	var list struct {
		Items []tracker.GitIntegration `json:"items"`
	}
	e.must(http.StatusOK, e.do("GET", "/api/v1/orgs/orbit/integrations", e.chen, nil)).json(t, &list)
	if len(list.Items) != 1 || list.Items[0].Provider != "github" {
		t.Fatalf("the list = %+v, want the one GitHub row", list.Items)
	}
	// The same secret may be used by a real GitLab integration (the duplicate check is per org and provider).
	e.glSetup("orbit", e.chen)
	e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), body), http.StatusOK)
}
