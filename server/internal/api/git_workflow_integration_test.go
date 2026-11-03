package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"devops/internal/bitbucket"
	"devops/internal/tracker"
)

// LAUN-244 (decisions-T44-git-workflow.md): per-team git workflow rules,
// their routes and their effect on the GitLab, GitHub and Bitbucket webhooks,
// driven by the documented-shape fixtures of each host (hand-written, not
// recorded deliveries).

const gwPath = "/api/v1/orgs/orbit/teams/ORB/git-workflow"

// gwStates is the team's state ids by name (archived states are not listed).
func (e *env) gwStates(org, team string) map[string]uuid.UUID {
	e.t.Helper()
	var page struct {
		Items []tracker.WorkflowState `json:"items"`
	}
	e.must(http.StatusOK, e.do("GET", "/api/v1/orgs/"+org+"/teams/"+team+"/states", e.chen, nil)).json(e.t, &page)
	out := map[string]uuid.UUID{}
	for _, s := range page.Items {
		out[s.Name] = s.ID
	}
	return out
}

// gwRule is one rule of a PUT body; from may be nil.
func gwRule(event string, state *uuid.UUID, from ...uuid.UUID) map[string]any {
	r := map[string]any{"event": event, "stateId": state}
	if len(from) > 0 {
		r["fromStateIds"] = from
	}
	return r
}

func idp(id uuid.UUID) *uuid.UUID { return &id }

func (e *env) gwPut(rules ...map[string]any) tracker.GitWorkflow {
	e.t.Helper()
	if rules == nil {
		rules = []map[string]any{}
	}
	var wf tracker.GitWorkflow
	e.must(http.StatusOK, e.do("PUT", gwPath, e.chen, map[string]any{"rules": rules})).json(e.t, &wf)
	return wf
}

func (e *env) gwGet() tracker.GitWorkflow {
	e.t.Helper()
	var wf tracker.GitWorkflow
	e.must(http.StatusOK, e.do("GET", gwPath, e.chen, nil)).json(e.t, &wf)
	return wf
}

func gwFind(wf tracker.GitWorkflow, event string) tracker.GitWorkflowRule {
	for _, r := range wf.Rules {
		if r.Event == event {
			return r
		}
	}
	return tracker.GitWorkflowRule{}
}

func (e *env) gwEvents() int {
	return e.count(`SELECT count(*) FROM events WHERE org_id = $1 AND type = 'team.updated' AND payload->'changes' ? 'gitWorkflow'`, e.orgID)
}

// The built-in rules, a replace, a no-op replace and a reset.
func TestGitWorkflow_DefaultsReplaceReset(t *testing.T) {
	e := newEnv(t)
	st := e.gwStates("orbit", "ORB")
	wf := e.gwGet()
	if len(wf.Rules) != 5 {
		t.Fatalf("rules = %+v, want five", wf.Rules)
	}
	want := map[string]*uuid.UUID{"opened": idp(st["In Review"]), "ready": idp(st["In Review"]), "reopened": idp(st["In Review"]),
		"merged": idp(st["In Testing"]), "closed": nil}
	for i, ev := range []string{"opened", "ready", "reopened", "merged", "closed"} {
		r := wf.Rules[i]
		if r.Event != ev || r.Custom || r.Problem != nil || len(r.FromStateIDs) != 0 {
			t.Fatalf("rule %d = %+v, want built-in %s", i, r, ev)
		}
		if w := want[ev]; (w == nil) != (r.StateID == nil) || (w != nil && *w != *r.StateID) {
			t.Fatalf("%s target = %v, want %v", ev, r.StateID, w)
		}
	}

	wf = e.gwPut(gwRule("merged", idp(st["Done"])), gwRule("opened", idp(st["In Testing"])), gwRule("closed", nil))
	if r := gwFind(wf, "merged"); !r.Custom || *r.StateID != st["Done"] {
		t.Fatalf("merged = %+v", r)
	}
	if r := gwFind(wf, "opened"); !r.Custom || *r.StateID != st["In Testing"] {
		t.Fatalf("opened = %+v", r)
	}
	if r := gwFind(wf, "closed"); !r.Custom || r.StateID != nil {
		t.Fatalf("closed = %+v", r)
	}
	if r := gwFind(wf, "ready"); r.Custom || *r.StateID != st["In Review"] {
		t.Fatalf("ready = %+v, want built-in", r)
	}
	if n := e.gwEvents(); n != 1 {
		t.Fatalf("team.updated gitWorkflow events = %d, want 1", n)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE org_id = $1 AND type = 'team.updated' AND actor_id IS NOT NULL
		AND payload->>'key' = 'ORB' AND NOT payload ? 'rules'`, e.orgID); n != 1 {
		t.Fatalf("team.updated with an actor, the key and no rules = %d, want 1", n)
	}
	// The same rules again (another order) change nothing and emit nothing.
	e.gwPut(gwRule("closed", nil), gwRule("opened", idp(st["In Testing"])), gwRule("merged", idp(st["Done"])))
	if n := e.gwEvents(); n != 1 {
		t.Fatalf("a same-rules PUT emitted: %d events", n)
	}
	// A from set round-trips.
	wf = e.gwPut(gwRule("merged", idp(st["Done"]), st["In Review"], st["In Testing"]))
	if r := gwFind(wf, "merged"); len(r.FromStateIDs) != 2 {
		t.Fatalf("merged from = %+v", r)
	}
	if r := gwFind(wf, "opened"); r.Custom {
		t.Fatalf("opened left the set but is still custom: %+v", r)
	}
	// Reset: an empty list.
	wf = e.gwPut()
	for _, r := range wf.Rules {
		if r.Custom {
			t.Fatalf("after reset: %+v", r)
		}
	}
	if n := e.count(`SELECT count(*) FROM team_git_rules WHERE org_id = $1`, e.orgID) + e.count(`SELECT count(*) FROM team_git_rule_from_states WHERE org_id = $1`, e.orgID); n != 0 {
		t.Fatalf("rows after reset = %d", n)
	}
	if n := e.gwEvents(); n != 3 {
		t.Fatalf("events = %d, want 3", n)
	}
}

// Members read, admins write, agents neither; another org is 404; a body
// reference to another team's or org's state is 400.
func TestGitWorkflow_AccessAndCrossOrg(t *testing.T) {
	e := newEnv(t)
	st := e.gwStates("orbit", "ORB")
	member := e.newUser("gw-member")
	e.addMember(e.orgID, member.ID, "member")
	mtok, _ := e.newToken(member.ID, tokenOpts{pin: &e.orgID})
	e.must(http.StatusOK, e.do("GET", gwPath, mtok, nil))
	r := e.do("PUT", gwPath, mtok, map[string]any{"rules": []any{}})
	if r.status != http.StatusForbidden {
		t.Fatalf("member PUT = %d %s, want 403", r.status, r.body)
	}
	if r := e.do("GET", gwPath, e.claude, nil); r.status != http.StatusForbidden || r.errCode(t) != "forbidden" ||
		!strings.Contains(string(r.body), "git workflow is not available to agent users") {
		t.Fatalf("agent GET = %d %s, want 403 forbidden with the route's words", r.status, r.body)
	}
	// An agent with the admin role and scope is still refused.
	agent := e.user("claude")
	if _, err := e.pool.Exec(context.Background(), `UPDATE org_members SET role = 'admin' WHERE org_id = $1 AND user_id = $2`, e.orgID, agent.ID); err != nil {
		t.Fatal(err)
	}
	atok, _ := e.newToken(agent.ID, tokenOpts{pin: &e.orgID})
	for _, m := range []string{"GET", "PUT"} {
		if r := e.do(m, gwPath, atok, map[string]any{"rules": []any{}}); r.status != http.StatusForbidden || r.errCode(t) != "forbidden" {
			t.Fatalf("admin agent %s = %d %s, want 403 forbidden", m, r.status, r.body)
		}
	}

	// Another org: its admin cannot reach orbit's team, and orbit's admin
	// cannot reach its team.
	other := e.newOrg("gw-other", "OTH")
	admin := e.newUser("gw-other-admin")
	e.addMember(other, admin.ID, "admin")
	otok, _ := e.newToken(admin.ID, tokenOpts{pin: &other})
	for _, m := range []string{"GET", "PUT"} {
		if r := e.do(m, gwPath, otok, map[string]any{"rules": []any{}}); r.status != http.StatusNotFound {
			t.Fatalf("other org admin %s orbit = %d, want 404", m, r.status)
		}
		if r := e.do(m, "/api/v1/orgs/orbit/teams/OTH/git-workflow", e.chen, map[string]any{"rules": []any{}}); r.status != http.StatusNotFound {
			t.Fatalf("orbit admin %s OTH = %d, want 404", m, r.status)
		}
	}
	e.must(http.StatusOK, e.do("GET", "/api/v1/orgs/gw-other/teams/OTH/git-workflow", otok, nil))
	var otherState uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM workflow_states WHERE org_id = $1 AND name = 'Done'`, other).Scan(&otherState); err != nil {
		t.Fatal(err)
	}
	// A second team of orbit.
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/orbit/teams", e.chen, map[string]any{"key": "GWB", "name": "Second"}))
	second := e.gwStates("orbit", "GWB")
	for name, body := range map[string]any{
		"other org state":       []any{gwRule("merged", idp(otherState))},
		"other team state":      []any{gwRule("merged", idp(second["Done"]))},
		"other org from state":  []any{gwRule("merged", idp(st["Done"]), otherState)},
		"other team from state": []any{gwRule("merged", idp(st["Done"]), second["In Testing"])},
		"unknown event":         []any{gwRule("approved", idp(st["Done"]))},
		"repeated event":        []any{gwRule("merged", idp(st["Done"])), gwRule("merged", nil)},
		"no move with from":     []any{gwRule("merged", nil, st["Done"])},
		"made-up state":         []any{gwRule("merged", idp(uuid.New()))},
		"missing stateId":       []any{map[string]any{"event": "merged"}},
	} {
		r := e.do("PUT", gwPath, e.chen, map[string]any{"rules": body})
		if r.status != http.StatusBadRequest || r.errCode(t) != "validation_failed" {
			t.Fatalf("%s: %d %s, want 400 validation_failed", name, r.status, r.body)
		}
	}
	for name, body := range map[string]any{
		"no rules":      map[string]any{},
		"null rules":    map[string]any{"rules": nil},
		"unknown field": map[string]any{"rules": []any{}, "x": 1},
	} {
		if r := e.do("PUT", gwPath, e.chen, body); r.status != http.StatusBadRequest {
			t.Fatalf("%s: %d %s, want 400", name, r.status, r.body)
		}
	}
	if n := e.gwEvents(); n != 0 {
		t.Fatalf("refused writes emitted %d events", n)
	}
}

// A PUT is idempotent by shape: an Idempotency-Key is accepted (the
// middleware sits after the scope check and stores POST only), and the same
// PUT again answers the same body and emits nothing.
func TestGitWorkflow_IdempotencyKey(t *testing.T) {
	e := newEnv(t)
	st := e.gwStates("orbit", "ORB")
	body := map[string]any{"rules": []any{gwRule("merged", idp(st["Done"]))}}
	r1 := e.must(http.StatusOK, e.do("PUT", gwPath, e.chen, body, "Idempotency-Key", "gw-1"))
	r2 := e.must(http.StatusOK, e.do("PUT", gwPath, e.chen, body, "Idempotency-Key", "gw-1"))
	if string(r1.body) != string(r2.body) {
		t.Fatalf("second PUT: %s / %s", r1.body, r2.body)
	}
	if n := e.gwEvents(); n != 1 {
		t.Fatalf("events = %d, want 1", n)
	}
	// The scope check answers before the key is looked at.
	member := e.newUser("gw-idem-member")
	e.addMember(e.orgID, member.ID, "member")
	mtok, _ := e.newToken(member.ID, tokenOpts{pin: &e.orgID})
	if r := e.do("PUT", gwPath, mtok, body, "Idempotency-Key", "gw-1"); r.status != http.StatusForbidden {
		t.Fatalf("member with a key = %d, want 403", r.status)
	}
}

// A rule whose state is archived is flagged and moves nothing; it cannot be
// saved again. A team delete takes its rules with it.
func TestGitWorkflow_ArchivedStateAndTeamDelete(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	st := e.gwStates("orbit", "ORB")
	e.gwPut(gwRule("merged", idp(st["Done"])))
	if _, err := e.pool.Exec(context.Background(), `UPDATE workflow_states SET archived_at = now() WHERE id = $1`, st["Done"]); err != nil {
		t.Fatal(err)
	}
	r := gwFind(e.gwGet(), "merged")
	if r.Problem == nil || *r.Problem != tracker.GitRuleStateArchived || r.StateID == nil || *r.StateID != st["Done"] {
		t.Fatalf("merged = %+v, want flagged state_archived", r)
	}
	if res := e.do("PUT", gwPath, e.chen, map[string]any{"rules": []any{gwRule("merged", idp(st["Done"]))}}); res.status != http.StatusBadRequest {
		t.Fatalf("PUT archived = %d, want 400", res.status)
	}
	is := e.createIssue(e.chen, map[string]any{"title": "archived target"})
	e.setState(is.Identifier, "In Progress")
	out := e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_merge.json", naming(is.Identifier, ""))), http.StatusOK)
	if len(out.Moved) != 0 || len(out.Linked) != 1 {
		t.Fatalf("merge = %+v, want linked, not moved", out)
	}
	if got := e.issueFull(is.Identifier); got.State.Name != "In Progress" {
		t.Fatalf("state = %q, want In Progress", got.State.Name)
	}

	// An archived "only from" state: the rule is flagged, still moves from its
	// other states, and a PUT that keeps the archived state is 400.
	e.gwPut(gwRule("merged", idp(st["In Testing"]), st["In Progress"], st["In Review"]))
	if _, err := e.pool.Exec(context.Background(), `UPDATE workflow_states SET archived_at = now() WHERE id = $1`, st["In Review"]); err != nil {
		t.Fatal(err)
	}
	r = gwFind(e.gwGet(), "merged")
	if r.Problem == nil || *r.Problem != tracker.GitRuleFromStateArchived || len(r.FromStateIDs) != 2 {
		t.Fatalf("merged = %+v, want flagged from_state_archived", r)
	}
	if res := e.do("PUT", gwPath, e.chen, map[string]any{"rules": []any{gwRule("merged", idp(st["In Testing"]), st["In Progress"], st["In Review"])}}); res.status != http.StatusBadRequest {
		t.Fatalf("PUT with an archived from state = %d, want 400", res.status)
	}
	is2 := e.createIssue(e.chen, map[string]any{"title": "archived from"})
	e.setState(is2.Identifier, "In Progress")
	mr := func(m map[string]any) {
		naming(is2.Identifier, "")(m)
		m["iid"] = 50
		m["url"] = "https://gitlab.example.com/example-org/example-app/-/merge_requests/50"
	}
	if out := e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_merge.json", mr)), http.StatusOK); len(out.Moved) != 1 {
		t.Fatalf("merge from a live from state = %+v, want moved", out)
	}
	if got := e.issueFull(is2.Identifier); got.State.Name != "In Testing" {
		t.Fatalf("state = %q, want In Testing", got.State.Name)
	}
	// Dropping the archived state saves, and the flag goes.
	e.gwPut(gwRule("merged", idp(st["In Testing"]), st["In Progress"]))
	if r := gwFind(e.gwGet(), "merged"); r.Problem != nil || len(r.FromStateIDs) != 1 {
		t.Fatalf("after the fix: %+v", r)
	}

	// A team with rules (and a from set) can be deleted: its states and rules go together.
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/orbit/teams", e.chen, map[string]any{"key": "GWD", "name": "Doomed"}))
	d := e.gwStates("orbit", "GWD")
	e.must(http.StatusOK, e.do("PUT", "/api/v1/orgs/orbit/teams/GWD/git-workflow", e.chen,
		map[string]any{"rules": []any{gwRule("merged", idp(d["Done"]), d["In Review"])}}))
	var gwd uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM teams WHERE org_id = $1 AND key = 'GWD'`, e.orgID).Scan(&gwd); err != nil {
		t.Fatal(err)
	}
	e.must(http.StatusNoContent, e.do("DELETE", "/api/v1/orgs/orbit/teams/GWD", e.chen, nil))
	if n := e.count(`SELECT count(*) FROM team_git_rule_from_states WHERE org_id = $1 AND team_id = $2`, e.orgID, gwd) +
		e.count(`SELECT count(*) FROM team_git_rules WHERE org_id = $1 AND team_id = $2`, e.orgID, gwd); n != 0 {
		t.Fatalf("from rows after delete = %d", n)
	}
}

// AC (GitLab): opened -> In Testing and merged -> Done; closed -> Canceled;
// a reopen rule; a rule of no move.
func TestGitWorkflow_GitLab(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	st := e.gwStates("orbit", "ORB")
	e.gwPut(gwRule("opened", idp(st["In Testing"])), gwRule("merged", idp(st["Done"])),
		gwRule("closed", idp(st["Canceled"])), gwRule("reopened", idp(st["In Progress"]), st["Canceled"]))

	a := e.createIssue(e.chen, map[string]any{"title": "rules a"})
	e.setState(a.Identifier, "Todo")
	out := e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_open.json", naming(a.Identifier, ""))), http.StatusOK)
	if len(out.Moved) != 1 {
		t.Fatalf("open = %+v", out)
	}
	if got := e.issueFull(a.Identifier); got.State.Name != "In Testing" {
		t.Fatalf("after open: %q, want In Testing", got.State.Name)
	}
	e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_merge.json", naming(a.Identifier, ""))), http.StatusOK)
	got := e.issueFull(a.Identifier)
	if got.State.Name != "Done" || got.CompletedAt == nil {
		t.Fatalf("after merge: %q completedAt %v, want Done and set", got.State.Name, got.CompletedAt)
	}
	last := got.StateHistory[len(got.StateHistory)-1]
	if last.Source != "gitlab" || last.Actor != nil {
		t.Fatalf("history = %+v, want gitlab with no actor", last)
	}

	// Close (a second MR number keeps the links apart), then reopen.
	b := e.createIssue(e.chen, map[string]any{"title": "rules b"})
	e.setState(b.Identifier, "In Progress")
	mr18 := func(ident string) func(map[string]any) {
		return func(m map[string]any) {
			naming(ident, "")(m)
			m["iid"] = 18
			m["url"] = "https://gitlab.example.com/example-org/example-app/-/merge_requests/18"
		}
	}
	e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_draft_open.json", mr18(b.Identifier))), http.StatusOK)
	if got := e.issueFull(b.Identifier); got.State.Name != "In Progress" {
		t.Fatalf("a draft open moved: %q", got.State.Name)
	}
	out = e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_close.json", mr18(b.Identifier))), http.StatusOK)
	if got := e.issueFull(b.Identifier); got.State.Name != "Canceled" || len(out.Moved) != 1 {
		t.Fatalf("after close: %q (%+v), want Canceled", got.State.Name, out)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE org_id = $1 AND type = 'issue.updated' AND entity_id = $2
		AND payload->'changes' ? 'state' AND payload->'changes' ? 'changeRequests' AND payload->>'source' = 'gitlab'`, e.orgID, b.ID); n != 1 {
		t.Fatalf("close move events = %d, want 1", n)
	}
	// The same close again (a new delivery): the link is closed already, nothing moves.
	e.setState(b.Identifier, "In Progress")
	out = e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_close.json", mr18(b.Identifier))), http.StatusOK)
	if len(out.Moved) != 0 {
		t.Fatalf("a second close moved: %+v", out)
	}
	e.setState(b.Identifier, "Canceled")
	out = e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_reopen.json", mr18(b.Identifier))), http.StatusOK)
	if got := e.issueFull(b.Identifier); got.State.Name != "In Progress" || len(out.Moved) != 1 {
		t.Fatalf("after reopen: %q (%+v), want In Progress (the rule lists Canceled)", got.State.Name, out)
	}

	// A rule of no move: linked, never moved.
	e.gwPut(gwRule("opened", nil))
	c := e.createIssue(e.chen, map[string]any{"title": "rules c"})
	e.setState(c.Identifier, "Todo")
	mr19 := func(m map[string]any) {
		naming(c.Identifier, "")(m)
		m["iid"] = 19
		m["url"] = "https://gitlab.example.com/example-org/example-app/-/merge_requests/19"
	}
	out = e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_open.json", mr19)), http.StatusOK)
	if len(out.Moved) != 0 || len(out.Linked) != 1 {
		t.Fatalf("no-move rule: %+v", out)
	}
}

// The close of an issue named only in the description moves nothing, even
// under a close rule (decisions-T5 #21).
func TestGitWorkflow_CloseNamesDescriptionOnly(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	st := e.gwStates("orbit", "ORB")
	e.gwPut(gwRule("closed", idp(st["Canceled"])))
	named := e.createIssue(e.chen, map[string]any{"title": "named"})
	mentioned := e.createIssue(e.chen, map[string]any{"title": "mentioned"})
	for _, is := range []tracker.Issue{named, mentioned} {
		e.setState(is.Identifier, "In Progress")
	}
	e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_open.json", naming(named.Identifier, "Refs "+mentioned.Identifier))), http.StatusOK)
	e.setState(named.Identifier, "In Progress")
	out := e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_close.json", naming(named.Identifier, "Refs "+mentioned.Identifier))), http.StatusOK)
	if len(out.Moved) != 1 || out.Moved[0] != named.Identifier {
		t.Fatalf("close moved %v, want only %s", out.Moved, named.Identifier)
	}
}

// AC (GitHub): ready -> In Testing and merged -> Done.
func TestGitWorkflow_GitHub(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{})
	g.setup()
	st := g.gwStates("orbit", "ORB")
	g.gwPut(gwRule("ready", idp(st["In Testing"])), gwRule("merged", idp(st["Done"])))
	is := g.createIssue(g.chen, map[string]any{"title": "gh rules"})
	g.setState(is.Identifier, "Todo")
	g.ghResult(g.ghHook("orbit", ghSecret, uuid.NewString(), ghFixture(t, "pr_opened_draft.json", ghNaming(is.Identifier, ""))), http.StatusOK)
	if got := g.issueFull(is.Identifier); got.State.Name != "Todo" {
		t.Fatalf("draft opened moved: %q", got.State.Name)
	}
	g.ghResult(g.ghHook("orbit", ghSecret, uuid.NewString(), ghFixture(t, "pr_ready_for_review.json", ghNaming(is.Identifier, ""))), http.StatusOK)
	if got := g.issueFull(is.Identifier); got.State.Name != "In Testing" {
		t.Fatalf("after ready: %q, want In Testing", got.State.Name)
	}
	g.ghResult(g.ghHook("orbit", ghSecret, uuid.NewString(), ghFixture(t, "pr_closed_merged.json", ghNaming(is.Identifier, ""))), http.StatusOK)
	if got := g.issueFull(is.Identifier); got.State.Name != "Done" || githubSources(got) != 2 {
		t.Fatalf("after merge: %q, %d github rows; want Done, 2", got.State.Name, githubSources(got))
	}

	// A redelivery (the same GUID) is a duplicate and moves nothing.
	other := g.createIssue(g.chen, map[string]any{"title": "gh replay"})
	g.setState(other.Identifier, "In Progress")
	body := ghFixture(t, "pr_closed_merged.json", func(m map[string]any) {
		ghNaming(other.Identifier, "")(m)
		m["number"] = 18
		m["pull_request"].(map[string]any)["number"] = 18
		m["pull_request"].(map[string]any)["html_url"] = "https://github.com/example-org/example-app/pull/18"
	})
	g.ghResult(g.ghHook("orbit", ghSecret, ghDelivery, body), http.StatusOK)
	g.setState(other.Identifier, "In Progress")
	r := g.ghResult(g.ghHook("orbit", ghSecret, ghDelivery, body), http.StatusOK)
	if r.Status != tracker.WebhookDuplicate || len(r.Moved) != 0 {
		t.Fatalf("redelivery = %+v, want duplicate, nothing moved", r)
	}
	// The same merge under a NEW delivery id: the link is merged already, so
	// the rule does not move the issue again (once per event).
	r = g.ghResult(g.ghHook("orbit", ghSecret, uuid.NewString(), body), http.StatusOK)
	if len(r.Moved) != 0 {
		t.Fatalf("second merge under a rule moved: %+v", r)
	}
	if got := g.issueFull(other.Identifier); got.State.Name != "In Progress" {
		t.Fatalf("state = %q, want In Progress", got.State.Name)
	}
}

// Pinned: with no rule, a second merge (a new delivery id) after a person
// moved the issue back moves it forward again, as before LAUN-244.
func TestGitWorkflow_DefaultSecondMergeMovesForward(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{})
	g.setup()
	is := g.createIssue(g.chen, map[string]any{"title": "default replay"})
	g.setState(is.Identifier, "In Progress")
	body := ghFixture(t, "pr_closed_merged.json", ghNaming(is.Identifier, ""))
	g.ghResult(g.ghHook("orbit", ghSecret, uuid.NewString(), body), http.StatusOK)
	g.setState(is.Identifier, "In Progress")
	r := g.ghResult(g.ghHook("orbit", ghSecret, uuid.NewString(), body), http.StatusOK)
	if got := g.issueFull(is.Identifier); got.State.Name != "In Testing" || len(r.Moved) != 1 {
		t.Fatalf("default second merge: %q (%+v), want In Testing", got.State.Name, r)
	}
}

// AC (Bitbucket Cloud and Data Center): created -> In Testing, fulfilled /
// merged -> Done, declined -> Canceled.
func TestGitWorkflow_Bitbucket(t *testing.T) {
	b := newBBEnv(t, envOpts{})
	b.setup()
	st := b.gwStates("orbit", "ORB")
	b.gwPut(gwRule("opened", idp(st["In Testing"])), gwRule("merged", idp(st["Done"])), gwRule("closed", idp(st["Canceled"])))

	cloud := b.createIssue(b.chen, map[string]any{"title": "bb cloud"})
	b.setState(cloud.Identifier, "Todo")
	b.send(bitbucket.EventCloudCreated, "cloud_pr_created.json", bbNaming(cloud.Identifier, ""))
	if got := b.issueFull(cloud.Identifier); got.State.Name != "In Testing" {
		t.Fatalf("cloud created: %q, want In Testing", got.State.Name)
	}
	b.send(bitbucket.EventCloudFulfilled, "cloud_pr_fulfilled.json", bbNaming(cloud.Identifier, ""))
	if got := b.issueFull(cloud.Identifier); got.State.Name != "Done" || bbSources(got) != 2 {
		t.Fatalf("cloud fulfilled: %q, %d rows; want Done, 2", got.State.Name, bbSources(got))
	}

	dc := b.createIssue(b.chen, map[string]any{"title": "bb dc"})
	b.setState(dc.Identifier, "Todo")
	b.send(bitbucket.EventDCOpened, "dc_pr_opened.json", bbNaming(dc.Identifier, ""))
	if got := b.issueFull(dc.Identifier); got.State.Name != "In Testing" {
		t.Fatalf("dc opened: %q, want In Testing", got.State.Name)
	}
	b.send(bitbucket.EventDCDeclined, "dc_pr_declined.json", bbNaming(dc.Identifier, ""))
	if got := b.issueFull(dc.Identifier); got.State.Name != "Canceled" {
		t.Fatalf("dc declined: %q, want Canceled", got.State.Name)
	}
}

// Completed and canceled issues never move under a rule with no from set; a
// rule that lists Done moves an issue out of Done.
func TestGitWorkflow_CompletedProtection(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	st := e.gwStates("orbit", "ORB")
	e.gwPut(gwRule("opened", idp(st["In Review"])), gwRule("merged", idp(st["In Review"])))
	for i, state := range []string{"Done", "Canceled", "Duplicate", "In Testing"} {
		is := e.createIssue(e.chen, map[string]any{"title": "protected " + state})
		e.setState(is.Identifier, state)
		mr := func(m map[string]any) {
			naming(is.Identifier, "")(m)
			m["iid"] = 30 + i
			m["url"] = "https://gitlab.example.com/example-org/example-app/-/merge_requests/" + uuid.NewString()[:8]
		}
		out := e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_open.json", mr)), http.StatusOK)
		if len(out.Moved) != 0 {
			t.Fatalf("%s moved backwards or out of a closed state: %+v", state, out)
		}
	}
	// A rule that lists Done and In Testing moves out of them.
	e.gwPut(gwRule("merged", idp(st["In Review"]), st["Done"], st["In Testing"]))
	is := e.createIssue(e.chen, map[string]any{"title": "listed done"})
	e.setState(is.Identifier, "Done")
	mr := func(m map[string]any) {
		naming(is.Identifier, "")(m)
		m["iid"] = 40
		m["url"] = "https://gitlab.example.com/example-org/example-app/-/merge_requests/40"
	}
	e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_merge.json", mr)), http.StatusOK)
	got := e.issueFull(is.Identifier)
	if got.State.Name != "In Review" || got.CompletedAt != nil {
		t.Fatalf("listed Done: %q completedAt %v, want In Review and cleared", got.State.Name, got.CompletedAt)
	}
	// A state not in the from set does not move.
	is2 := e.createIssue(e.chen, map[string]any{"title": "not listed"})
	e.setState(is2.Identifier, "Todo")
	mr2 := func(m map[string]any) {
		naming(is2.Identifier, "")(m)
		m["iid"] = 41
		m["url"] = "https://gitlab.example.com/example-org/example-app/-/merge_requests/41"
	}
	if out := e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_merge.json", mr2)), http.StatusOK); len(out.Moved) != 0 {
		t.Fatalf("a state outside the from set moved: %+v", out)
	}
}

// Rules of one team never touch another team's issues.
func TestGitWorkflow_PerTeam(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/orbit/teams", e.chen, map[string]any{"key": "GWT", "name": "Two"}))
	two := e.gwStates("orbit", "GWT")
	e.must(http.StatusOK, e.do("PUT", "/api/v1/orgs/orbit/teams/GWT/git-workflow", e.chen,
		map[string]any{"rules": []any{gwRule("merged", idp(two["Done"]))}}))
	orb := e.createIssue(e.chen, map[string]any{"title": "orb"})
	gwt := e.createIssue(e.chen, map[string]any{"title": "gwt", "team": "GWT"})
	for _, is := range []tracker.Issue{orb, gwt} {
		e.setState(is.Identifier, "In Progress")
	}
	both := func(m map[string]any) {
		m["source_branch"] = "chen/" + orb.Identifier + "-" + gwt.Identifier
		m["title"] = orb.Identifier + " " + gwt.Identifier
		m["description"] = ""
	}
	e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_merge.json", both)), http.StatusOK)
	if got := e.issueFull(orb.Identifier); got.State.Name != "In Testing" {
		t.Fatalf("ORB (no rules): %q, want In Testing", got.State.Name)
	}
	if got := e.issueFull(gwt.Identifier); got.State.Name != "Done" {
		t.Fatalf("GWT (merged -> Done): %q, want Done", got.State.Name)
	}
}
