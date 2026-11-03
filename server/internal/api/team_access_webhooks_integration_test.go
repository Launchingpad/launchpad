package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"devops/internal/github"
	"devops/internal/testdb"
	"devops/internal/tracker"
)

// Ticket 8 of decisions-T20 (LAUN-224): the webhooks answer a status only
// and a repository's team limits the match (#65); the webhook guard keeps a
// repository with no team from existing next to restricted access (entries 5
// to 7 of #65); and the samples of the last own rules.

func init() {
	for k, v := range map[string]func(f *teamFixture) teamSample{
		// The caller's own remembered views: no team row, nothing of team B.
		"GET /me/views": func(f *teamFixture) teamSample {
			return teamSample{Method: "GET", Path: f.orgPath("/me/views")}
		},
		"PUT /me/views/{place}": func(f *teamFixture) teamSample {
			return teamSample{Method: "PUT", Path: f.orgPath("/me/views/issues:probe"),
				Body: map[string]any{"view": "board", "sort": "manual", "at": 1}}
		},
		// The caller's git handles per integration: no team row.
		"GET /me/git-accounts": func(f *teamFixture) teamSample {
			return teamSample{Method: "GET", Path: f.orgPath("/me/git-accounts")}
		},
	} {
		if _, dup := teamRuleSamples[k]; dup {
			panic("ticket 8 sample " + k + " is in teamRuleSamples twice")
		}
		teamRuleSamples[k] = v
	}
}

// hookTeams is a GitLab env with team HID next to ORB, its repository mapped
// to HID, and every member who is not an admin restricted to ORB: no member
// but the admin sees HID.
type hookTeams struct {
	*env
	integration, repo uuid.UUID
	hid               tracker.Team
	hidden, visible   tracker.Issue // HID-1 and an ORB issue
}

func newHookTeams(t *testing.T) *hookTeams {
	t.Helper()
	e := newEnv(t)
	h := &hookTeams{env: e}
	h.integration = e.glSetup("orbit", e.chen)
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/orbit/teams", e.chen, map[string]any{"key": "HID", "name": "Hidden"})).json(t, &h.hid)
	h.hidden = e.createIssue(e.chen, map[string]any{"team": "HID", "title": "hidden issue"})
	h.visible = e.createIssue(e.chen, map[string]any{"team": testdb.TeamKey, "title": "visible issue"})
	h.repo = h.repoID(h.integration)
	e.must(http.StatusOK, e.do("PATCH", h.repoPath(h.integration, h.repo), e.chen, map[string]any{"teamId": "HID"}))
	orb := e.teamID(testdb.TeamKey)
	for _, name := range []string{"claude"} {
		testdb.MakeRestricted(t, e.pool, e.orgID, e.user(name).ID, orb)
	}
	rid := testdb.AddMember(t, e.pool, e.orgID, "rita", "member")
	testdb.MakeRestricted(t, e.pool, e.orgID, rid, orb)
	return h
}

func (e *env) teamID(key string) uuid.UUID {
	e.t.Helper()
	var id uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM teams WHERE org_id = $1 AND key = $2`, e.orgID, key).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// repoID is the id of the only map row of an integration.
func (e *env) repoID(integration uuid.UUID) uuid.UUID {
	e.t.Helper()
	var id uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM git_repo_map WHERE org_id = $1 AND integration_id = $2`,
		e.orgID, integration).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) repoPath(integration, repo uuid.UUID) string {
	return "/api/v1/orgs/orbit/integrations/" + integration.String() + "/repos/" + repo.String()
}

// glMR names ident in an MR with iid n.
func glMR(ident string, n int) func(map[string]any) {
	return func(a map[string]any) {
		naming(ident, "")(a)
		a["iid"] = n
	}
}

// AC (#65): the answer is a status and nothing else, byte for byte the same
// for an issue of the mapped team that no member but the admin can see, an
// issue of a visible team (another team: not matched) and a missing one; for
// a review of an MR with a link and one with none; and the deliveries ring
// (org admins) still says what each did. The hidden issue is linked and
// moved, its event is scoped to its team, and the visible one is untouched.
func TestWebhookTeamAccess_GitlabStatusOnly(t *testing.T) {
	h := newHookTeams(t)
	e := h.env
	seq := latestSeq(t, e)
	var bodies []string
	for i, ident := range []string{h.hidden.Identifier, h.visible.Identifier, "HID-999"} {
		r := e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_open.json", glMR(ident, 100+i)))
		e.must(http.StatusOK, r)
		bodies = append(bodies, string(r.body))
	}
	for _, b := range bodies {
		if b != bodies[0] || strings.TrimSpace(b) != `{"status":"processed"}` {
			t.Fatalf("answers differ or say more than a status: %q", bodies)
		}
	}
	if got := e.issueFull(h.hidden.Identifier); got.State.Name != "In Review" || len(got.Attachments) != 1 {
		t.Fatalf("the hidden team's issue is not moved and linked: %s, %d attachments", got.State.Name, len(got.Attachments))
	}
	if got := e.issueFull(h.visible.Identifier); got.State.Name == "In Review" || len(got.Attachments) != 0 {
		t.Fatalf("another team's issue was touched by a repository mapped to HID: %s, %d attachments", got.State.Name, len(got.Attachments))
	}
	// Every event of the deliveries is scoped to HID (#54).
	rows, err := e.pool.Query(context.Background(), `SELECT type, scope, team_ids FROM events WHERE org_id = $1 AND seq > $2`, e.orgID, seq)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		var typ, scope string
		var teams []uuid.UUID
		if err := rows.Scan(&typ, &scope, &teams); err != nil {
			t.Fatal(err)
		}
		n++
		if scope != "any" || len(teams) != 1 || teams[0] != h.hid.ID {
			t.Errorf("event %s: scope %s %v, want any [HID]", typ, scope, teams)
		}
	}
	rows.Close()
	if n == 0 {
		t.Fatal("the deliveries made no event")
	}

	// A review of the linked MR (100) and of the MR with no link (101).
	linked := e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_approved.json", glMR(h.hidden.Identifier, 100)))
	unlinked := e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_approved.json", glMR(h.visible.Identifier, 101)))
	e.must(http.StatusOK, linked)
	e.must(http.StatusOK, unlinked)
	if string(linked.body) != string(unlinked.body) || strings.TrimSpace(string(linked.body)) != `{"status":"processed"}` {
		t.Fatalf("a review answers by its link: %s vs %s", linked.body, unlinked.body)
	}
	// The ring keeps the outcome for the org's admins.
	var results []string
	rows, err = e.pool.Query(context.Background(), `SELECT result FROM git_webhook_deliveries WHERE org_id = $1 ORDER BY received_at DESC, id DESC LIMIT 2`, e.orgID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		results = append(results, r)
	}
	rows.Close()
	if strings.Join(results, ",") != tracker.WebhookIgnored+","+tracker.WebhookProcessed {
		t.Fatalf("ring results (newest first) = %v, want ignored then processed", results)
	}
}

// AC (#65) for GitHub: the same body for a hidden, a visible (other team) and
// a missing issue, and a repository mapped to a team never links or moves an
// issue of another team.
func TestWebhookTeamAccess_GithubStatusOnly(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{})
	gi := g.setup()
	g.must(http.StatusCreated, g.do("POST", "/api/v1/orgs/orbit/teams", g.chen, map[string]any{"key": "HID", "name": "Hidden"}))
	hidden := g.createIssue(g.chen, map[string]any{"team": "HID", "title": "hidden issue"})
	visible := g.createIssue(g.chen, map[string]any{"team": testdb.TeamKey, "title": "visible issue"})
	g.must(http.StatusOK, g.do("PATCH", g.repoPath(gi, g.repoID(gi)), g.chen, map[string]any{"teamId": "HID"}))
	rid := testdb.AddMember(t, g.pool, g.orgID, "rita", "member")
	testdb.MakeRestricted(t, g.pool, g.orgID, rid, g.teamID(testdb.TeamKey))

	var bodies []string
	for i, ident := range []string{hidden.Identifier, visible.Identifier, "HID-999"} {
		body := ghFixture(t, "pr_opened.json", func(m map[string]any) {
			ghNaming(ident, "")(m)
			m["number"] = 200 + i
			m["pull_request"].(map[string]any)["number"] = 200 + i
		})
		r := g.ghHook("orbit", ghSecret, uuid.NewString(), body)
		g.must(http.StatusOK, r)
		bodies = append(bodies, string(r.body))
		switch ident {
		case hidden.Identifier:
			if len(r.linked) != 1 || r.linked[0] != hidden.Identifier || len(r.moved) != 1 {
				t.Fatalf("the mapped team's issue: linked %v moved %v", r.linked, r.moved)
			}
		default:
			if len(r.linked) != 0 || len(r.moved) != 0 {
				t.Fatalf("%s: linked %v moved %v, want nothing (another team or no issue)", ident, r.linked, r.moved)
			}
		}
	}
	for _, b := range bodies {
		if b != bodies[0] || strings.TrimSpace(b) != `{"status":"processed"}` {
			t.Fatalf("answers differ or say more than a status: %q", bodies)
		}
	}
}

// guardRefused checks a 409 conflict of the webhook guard that names path.
func guardRefused(t *testing.T, r resp, path string) {
	t.Helper()
	if r.status != http.StatusConflict || r.errCode(t) != "conflict" || !strings.Contains(string(r.body), "need") ||
		(path != "" && !strings.Contains(string(r.body), path)) {
		t.Fatalf("want the guard's 409 conflict naming %q, got %d %s", path, r.status, r.body)
	}
}

// rowsOf reads every row of a table in the org, to show a refused write
// changed nothing.
func (e *env) rowsOf(table string) string {
	e.t.Helper()
	var out string
	if err := e.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(row_to_json(x)::text, ',' ORDER BY x.id), '')
		FROM `+table+` x WHERE x.org_id = $1`, e.orgID).Scan(&out); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// AC (#65, entries 5 to 7): while restricted access exists, a repository with
// no team on an enabled integration cannot come to exist: not by a map, not
// by clearing a team, not by enabling an integration, and not by deleting the
// team of a repository. Each refusal writes nothing. Without restricted
// access every one of them works (the control).
func TestWebhookGuard_Entries(t *testing.T) {
	for _, restricted := range []bool{true, false} {
		name := "restricted"
		if !restricted {
			name = "no restricted member"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			if restricted {
				rid := testdb.AddMember(t, e.pool, e.orgID, "rita", "member")
				testdb.MakeRestricted(t, e.pool, e.orgID, rid, e.teamID(testdb.TeamKey))
			}
			base := "/api/v1/orgs/orbit/integrations"
			// Entry 5: a new integration has no repository; the create passes.
			var gi tracker.GitIntegration
			e.must(http.StatusCreated, e.do("POST", base, e.chen,
				map[string]any{"provider": "gitlab", "baseUrl": glBaseURL, "secret": glSecret})).json(t, &gi)
			repos := base + "/" + gi.ID.String() + "/repos"

			// Entry 6: a map with no team.
			before := e.rowsOf("git_repo_map")
			r := e.do("POST", repos, e.chen, map[string]any{"repoKey": "42", "repoPath": "example-org/no-team"})
			if restricted {
				guardRefused(t, r, "example-org/no-team")
				if e.rowsOf("git_repo_map") != before {
					t.Fatal("a refused map wrote a row")
				}
			} else {
				e.must(http.StatusCreated, r)
				e.must(http.StatusOK, e.do("DELETE", e.repoPath(gi.ID, e.repoID(gi.ID)), e.chen, nil))
			}
			// A map with a team passes either way.
			var repo tracker.GitRepo
			e.must(http.StatusCreated, e.do("POST", repos, e.chen,
				map[string]any{"repoKey": "42", "repoPath": "example-org/app", "teamId": testdb.TeamKey})).json(t, &repo)

			// Entry 6: clearing the team.
			before = e.rowsOf("git_repo_map")
			r = e.do("PATCH", e.repoPath(gi.ID, repo.ID), e.chen, map[string]any{"teamId": nil})
			if restricted {
				guardRefused(t, r, "example-org/app")
				if e.rowsOf("git_repo_map") != before {
					t.Fatal("a refused PATCH changed the map")
				}
			} else {
				e.must(http.StatusOK, r)
				e.must(http.StatusOK, e.do("PATCH", e.repoPath(gi.ID, repo.ID), e.chen, map[string]any{"teamId": testdb.TeamKey}))
			}

			// Entry 5: enabling an integration that has a repository with no
			// team. While it is disabled the map is allowed (it matches nothing).
			e.must(http.StatusOK, e.do("PATCH", base+"/"+gi.ID.String(), e.chen, map[string]any{"enabled": false}))
			e.must(http.StatusCreated, e.do("POST", repos, e.chen, map[string]any{"repoKey": "43", "repoPath": "example-org/later"}))
			before = e.rowsOf("git_integrations")
			r = e.do("PATCH", base+"/"+gi.ID.String(), e.chen, map[string]any{"enabled": true})
			if restricted {
				guardRefused(t, r, "example-org/later")
				if e.rowsOf("git_integrations") != before {
					t.Fatal("a refused enable changed the integration")
				}
			} else {
				e.must(http.StatusOK, r)
			}

			// Entry 7: deleting the team of a repository on an enabled
			// integration (ON DELETE SET NULL would leave it with no team).
			e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/orbit/teams", e.chen, map[string]any{"key": "GON", "name": "Gone"}))
			var other tracker.GitIntegration
			e.must(http.StatusCreated, e.do("POST", base, e.chen,
				map[string]any{"provider": "gitlab", "baseUrl": "https://gitlab2.example.com", "secret": glSecret + "-two"})).json(t, &other)
			e.must(http.StatusCreated, e.do("POST", base+"/"+other.ID.String()+"/repos", e.chen,
				map[string]any{"repoKey": "77", "repoPath": "example-org/gone", "teamId": "GON"}))
			beforeTeams, beforeMap := e.rowsOf("teams"), e.rowsOf("git_repo_map")
			r = e.do("DELETE", "/api/v1/orgs/orbit/teams/GON", e.chen, nil)
			if restricted {
				guardRefused(t, r, "example-org/gone")
				if e.rowsOf("teams") != beforeTeams || e.rowsOf("git_repo_map") != beforeMap {
					t.Fatal("a refused team delete changed the teams or the map")
				}
			} else {
				e.must(http.StatusNoContent, r)
			}
		})
	}
}

// AC (#65, entry 6, token mode): the refusal comes before the host call, so a
// refused map makes no hook on the host.
func TestWebhookGuard_TokenModeMakesNoHook(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	h := newRepoHost(t, 4242)
	gi := g.tokenCreate(&fakeHost{Fake: h.Fake})
	rid := testdb.AddMember(t, g.pool, g.orgID, "rita", "member")
	testdb.MakeRestricted(t, g.pool, g.orgID, rid, g.teamID(testdb.TeamKey))
	r := g.do("POST", g.path(gi.ID, "/repos"), g.chen, map[string]any{"repoId": strconv.Itoa(4242)})
	guardRefused(t, r, "")
	h.mu.Lock()
	hooks := len(h.hooks[4242])
	h.mu.Unlock()
	if hooks != 0 {
		t.Fatalf("a refused map made %d hook(s) on the host", hooks)
	}
	g.must(http.StatusCreated, g.do("POST", g.path(gi.ID, "/repos"), g.chen, map[string]any{"repoId": strconv.Itoa(4242), "teamId": testdb.TeamKey}))
}

// Cross-org (AGENTS.md): another org's team in a repository map is 400
// unknown team, and the guard reads only its own org: a restricted member in
// another org does not block this org's map with no team.
func TestWebhookGuard_CrossOrg(t *testing.T) {
	e := newEnv(t)
	other := e.newOrg("elsewhere", "ELS")
	oid := testdb.AddMember(t, e.pool, other, "otto", "member")
	var els uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM teams WHERE org_id = $1 AND key = 'ELS'`, other).Scan(&els); err != nil {
		t.Fatal(err)
	}
	testdb.MakeRestricted(t, e.pool, other, oid, els)
	gi := e.glSetup("orbit", e.chen) // maps a repository with no team: passes
	r := e.do("PATCH", e.repoPath(gi, e.repoID(gi)), e.chen, map[string]any{"teamId": els.String()})
	if r.status != http.StatusBadRequest || r.errCode(t) != "validation_failed" {
		t.Fatalf("another org's team in the map: %d %s, want 400", r.status, r.body)
	}
}

// latestSeq is the org's newest event seq.
func latestSeq(t *testing.T, e *env) int64 {
	t.Helper()
	var n int64
	if err := e.pool.QueryRow(context.Background(), `SELECT coalesce(max(seq), 0) FROM events WHERE org_id = $1`, e.orgID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// glWithSHA sets the head sha of a GitLab MR (last_commit.id).
func glWithSHA(sha string, mut func(map[string]any)) func(map[string]any) {
	return func(a map[string]any) {
		mut(a)
		a["last_commit"].(map[string]any)["id"] = sha
	}
}

const missingSHA = "cccccccccccccccccccccccccccccccccccccccc"

// AC (#65, review of LAUN-224): a check delivery and its resend (the host's
// Redeliver or Resend keeps the delivery key) answer the same for a sha whose
// MR linked a hidden issue of the mapped team, a sha whose MR named a visible
// issue of another team (no link) and a sha whose MR named a missing issue.
// The ring keeps the truth (duplicate for the linked resend).
func TestWebhookTeamAccess_GitlabResendSameAnswer(t *testing.T) {
	h := newHookTeams(t)
	e := h.env
	e.must(http.StatusOK, e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_open.json", glMR(h.hidden.Identifier, 100))))
	e.must(http.StatusOK, e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_open.json", glWithSHA(newSHA, glMR(h.visible.Identifier, 101)))))
	e.must(http.StatusOK, e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_open.json", glWithSHA(missingSHA, glMR("HID-999", 102)))))
	var answers []string
	for _, sha := range []string{mrSHA, newSHA, missingSHA} {
		u := uuid.NewString()
		for range 2 {
			r := glPipeline(e, "pipeline_success.json", u, pipelineAt(sha, "", "failed", "2026-10-24 10:00:00 UTC", "2026-10-24 10:01:00 UTC"))
			answers = append(answers, strconv.Itoa(r.status)+" "+strings.TrimSpace(string(r.body)))
		}
	}
	for _, a := range answers {
		if a != `200 {"status":"processed"}` {
			t.Fatalf("a check delivery or its resend answers by its link: %q", answers)
		}
	}
	if c := checksOf(t, e, h.hidden.Identifier); c != "failure" {
		t.Fatalf("the linked check was not applied: %s", c)
	}
	var dup int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM git_webhook_deliveries WHERE org_id = $1 AND result = 'duplicate'`, e.orgID).Scan(&dup); err != nil || dup != 1 {
		t.Fatalf("ring duplicates = %d %v, want 1 (the linked resend)", dup, err)
	}
}

// The same for GitHub's status events.
func TestWebhookTeamAccess_GithubResendSameAnswer(t *testing.T) {
	g := newGHEnv(t, 0, 0, true, envOpts{})
	gi := g.setup()
	g.must(http.StatusCreated, g.do("POST", "/api/v1/orgs/orbit/teams", g.chen, map[string]any{"key": "HID", "name": "Hidden"}))
	hidden := g.createIssue(g.chen, map[string]any{"team": "HID", "title": "hidden issue"})
	visible := g.createIssue(g.chen, map[string]any{"team": testdb.TeamKey, "title": "visible issue"})
	g.must(http.StatusOK, g.do("PATCH", g.repoPath(gi, g.repoID(gi)), g.chen, map[string]any{"teamId": "HID"}))
	rid := testdb.AddMember(t, g.pool, g.orgID, "rita", "member")
	testdb.MakeRestricted(t, g.pool, g.orgID, rid, g.teamID(testdb.TeamKey))
	for i, c := range []struct{ ident, sha string }{{hidden.Identifier, prSHA}, {visible.Identifier, newSHA}, {"HID-999", missingSHA}} {
		g.ghSend("pr_opened.json", func(m map[string]any) {
			ghNaming(c.ident, "")(m)
			m["number"], m["pull_request"].(map[string]any)["number"] = 300+i, 300+i
			m["pull_request"].(map[string]any)["head"].(map[string]any)["sha"] = c.sha
		})
	}
	var answers []string
	for _, sha := range []string{prSHA, newSHA, missingSHA} {
		u := uuid.NewString()
		for range 2 {
			r := g.ghCheck("orbit", "status_success.json", github.EventStatus, u, statusAt(sha, "ci/build", "failure", 13))
			answers = append(answers, strconv.Itoa(r.status)+" "+strings.TrimSpace(string(r.body)))
		}
	}
	for _, a := range answers {
		if a != `200 {"status":"processed"}` {
			t.Fatalf("a check delivery or its resend answers by its link: %q", answers)
		}
	}
	if c := checksOf(t, g.env, hidden.Identifier); c != "failure" {
		t.Fatalf("the linked check was not applied: %s", c)
	}
}

// AC (#65, review of LAUN-224): a close, a review or a check of an MR changes
// only links of the repository's mapped team. A link made before the map moved
// to another team is left as it is, and moves again once the map comes back.
func TestWebhookTeamAccess_RemapLeavesOldLinks(t *testing.T) {
	e := newEnv(t)
	gi := e.glSetup("orbit", e.chen)
	repo := e.repoID(gi)
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/orbit/teams", e.chen, map[string]any{"key": "HID", "name": "Hidden"}))
	is := e.createIssue(e.chen, map[string]any{"team": testdb.TeamKey, "title": "linked before the remap"})
	e.must(http.StatusOK, e.do("PATCH", e.repoPath(gi, repo), e.chen, map[string]any{"teamId": testdb.TeamKey}))
	e.must(http.StatusOK, e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_open.json", glMR(is.Identifier, 7))))
	link := func() string {
		var state, checks string
		if err := e.pool.QueryRow(context.Background(), `SELECT state, coalesce(checks, '') FROM mr_links WHERE org_id = $1 AND issue_id = $2`,
			e.orgID, is.ID).Scan(&state, &checks); err != nil {
			t.Fatal(err)
		}
		return state + "/" + checks
	}
	before, seq := link(), latestSeq(t, e)
	e.must(http.StatusOK, e.do("PATCH", e.repoPath(gi, repo), e.chen, map[string]any{"teamId": "HID"}))
	e.must(http.StatusOK, glPipeline(e, "pipeline_success.json", uuid.NewString(), pipelineAt(mrSHA, "", "failed", "2026-10-24 10:00:00 UTC", "2026-10-24 10:01:00 UTC")))
	e.must(http.StatusOK, e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_approved.json", glMR(is.Identifier, 7))))
	e.must(http.StatusOK, e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_close.json", glMR(is.Identifier, 7))))
	if got := link(); got != before {
		t.Fatalf("an event of a repository mapped to HID changed an ORB link: %s -> %s", before, got)
	}
	if n := latestSeq(t, e); n != seq {
		t.Fatalf("an event of a repository mapped to HID emitted %d events about an ORB issue", n-seq)
	}
	e.must(http.StatusOK, e.do("PATCH", e.repoPath(gi, repo), e.chen, map[string]any{"teamId": testdb.TeamKey}))
	e.must(http.StatusOK, e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_close.json", glMR(is.Identifier, 7))))
	if got := link(); !strings.HasPrefix(got, "closed/") {
		t.Fatalf("after the map came back the close did not apply: %s", got)
	}
}

// AC (#65, review of LAUN-224): a backfill acts with system authority, so it
// is refused on a disabled integration, and on a repository with no team while
// a member is restricted; either way it links and moves nothing and asks the
// host nothing.
func TestWebhookGuard_Backfill(t *testing.T) {
	for _, disabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "disabled integration", false: "repository with no team"}[disabled], func(t *testing.T) {
			g, h, gi, repo := tokenRepoEnv(t, 4242)
			g.must(http.StatusCreated, g.do("POST", "/api/v1/orgs/orbit/teams", g.chen, map[string]any{"key": "HID", "name": "Hidden"}))
			hidden := g.createIssue(g.chen, map[string]any{"team": "HID", "title": "hidden issue"})
			g.setState(hidden.Identifier, "Todo")
			h.pulls = map[int][]map[string]any{4242: {hostPR(4242, 1, hidden.Identifier, "open", false, 1)}}
			if disabled {
				g.must(http.StatusOK, g.do("PATCH", g.path(gi.ID, ""), g.chen, map[string]any{"enabled": false}))
			}
			rid := testdb.AddMember(t, g.pool, g.orgID, "rita", "member")
			testdb.MakeRestricted(t, g.pool, g.orgID, rid, g.teamID(testdb.TeamKey))
			before, links, seq := g.issueFull(hidden.Identifier).State.Name, g.rowsOf("mr_links"), latestSeq(t, g.env)
			r := g.do("POST", g.path(gi.ID, "/repos/"+repo.ID.String()+"/backfill"), g.chen, nil)
			if r.status != http.StatusConflict || r.errCode(t) != "conflict" {
				t.Fatalf("backfill = %d %s, want 409 conflict", r.status, r.body)
			}
			if g.issueFull(hidden.Identifier).State.Name != before || g.rowsOf("mr_links") != links || latestSeq(t, g.env) != seq {
				t.Fatal("a refused backfill linked, moved or emitted")
			}
			// With a team the backfill runs (on an enabled integration).
			if disabled {
				// Enabling is refused while the repository has no team.
				guardRefused(t, g.do("PATCH", g.path(gi.ID, ""), g.chen, map[string]any{"enabled": true}), "")
				return
			}
			g.must(http.StatusOK, g.do("PATCH", g.repoPath(gi.ID, repo.ID), g.chen, map[string]any{"teamId": "HID"}))
			var out tracker.BackfillResult
			g.must(http.StatusOK, g.do("POST", g.path(gi.ID, "/repos/"+repo.ID.String()+"/backfill"), g.chen, nil)).json(t, &out)
			if out.Linked != 1 {
				t.Fatalf("backfill with a team = %+v, want the hidden team's issue linked", out)
			}
		})
	}
}

// The nameless form of the guard (for a caller who is not an org admin,
// ticket 10) names no repository; the named form does.
func TestWebhookGuard_Nameless(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen) // a repository with no team
	rid := testdb.AddMember(t, e.pool, e.orgID, "rita", "member")
	testdb.MakeRestricted(t, e.pool, e.orgID, rid, e.teamID(testdb.TeamKey))
	ctx := context.Background()
	named := tracker.CheckWebhookGuard(ctx, e.q, e.orgID)
	nameless := tracker.CheckWebhookGuardNameless(ctx, e.q, e.orgID)
	if named == nil || !strings.Contains(named.Error(), "example-org/example-app") {
		t.Fatalf("named form = %v", named)
	}
	if nameless == nil || strings.Contains(nameless.Error(), "example-org") {
		t.Fatalf("nameless form = %v", nameless)
	}
}

// lastRingResult is the result of the org's newest deliveries ring row.
func (e *env) lastRingResult() string {
	e.t.Helper()
	var r string
	if err := e.pool.QueryRow(context.Background(), `SELECT result FROM git_webhook_deliveries WHERE org_id = $1 ORDER BY received_at DESC, id DESC LIMIT 1`,
		e.orgID).Scan(&r); err != nil {
		e.t.Fatal(err)
	}
	return r
}
