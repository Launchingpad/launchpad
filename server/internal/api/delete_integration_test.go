package api

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"devops/internal/tracker"
)

// LAUN-191 (docs/internal/decisions-T26-delete.md): DELETE for projects and
// teams. A product delete already existed (products_integration_test.go).

func TestDeleteProject(t *testing.T) {
	e := newEnv(t)
	p := e.createProject(e.chen, map[string]any{"name": "Doomed", "teams": []string{"ORB"}})
	keep := e.createProject(e.chen, map[string]any{"name": "Keeper"})
	var ms tracker.Milestone
	e.must(201, e.do("POST", O+"/milestones", e.chen, map[string]any{"projectId": p.ID, "name": "m1"})).json(t, &ms)
	var doc tracker.Document
	e.must(201, e.do("POST", O+"/documents", e.chen, map[string]any{"title": "kept doc", "projectId": p.ID})).json(t, &doc)
	is := e.createIssue(e.chen, map[string]any{"title": "in project", "projectId": p.ID, "milestoneId": ms.ID})
	other := e.createIssue(e.chen, map[string]any{"title": "in other", "projectId": keep.ID})

	// Scope: write (the agent) and read cannot delete.
	expectCode(t, e.do("DELETE", O+"/projects/"+p.ID.String(), e.claude, nil), 403, "insufficient_scope")
	readTok, _ := e.newToken(e.user("chen").ID, tokenOpts{scopes: []string{"read"}})
	expectCode(t, e.do("DELETE", O+"/projects/"+p.ID.String(), readTok, nil), 403, "insufficient_scope")
	expectCode(t, e.do("DELETE", O+"/projects/not-a-uuid", e.chen, nil), 404, "not_found")
	if e.eventCount("project.deleted") != 0 {
		t.Fatal("event before delete")
	}

	e.must(204, e.do("DELETE", O+"/projects/"+p.ID.String(), e.chen, nil))
	expectCode(t, e.do("GET", O+"/projects/"+p.ID.String(), e.chen, nil), 404, "not_found")
	expectCode(t, e.do("DELETE", O+"/projects/"+p.ID.String(), e.chen, nil), 404, "not_found")

	// The issue and the document stay; they lose the project (and the milestone).
	if n := e.countRows(`SELECT count(*) FROM issues WHERE id = $1 AND project_id IS NULL AND milestone_id IS NULL`, is.ID); n != 1 {
		t.Fatal("issue was not detached")
	}
	if n := e.countRows(`SELECT count(*) FROM documents WHERE id = $1 AND project_id IS NULL`, doc.ID); n != 1 {
		t.Fatal("document was not detached")
	}
	if n := e.countRows(`SELECT count(*) FROM milestones WHERE project_id = $1`, p.ID); n != 0 {
		t.Fatal("milestones left")
	}
	if n := e.countRows(`SELECT count(*) FROM project_teams WHERE project_id = $1`, p.ID); n != 0 {
		t.Fatal("project_teams left")
	}
	// The other project is untouched.
	if n := e.countRows(`SELECT count(*) FROM issues WHERE id = $1 AND project_id = $2`, other.ID, keep.ID); n != 1 {
		t.Fatal("another project's issue changed")
	}
	if e.eventCount("project.deleted") != 1 {
		t.Fatal("want one project.deleted")
	}
	// LAUN-221 (T20 #37): no issue count in the payload; it would count
	// issues of teams a reader cannot see.
	if n := e.countRows(`SELECT count(*) FROM events WHERE type = 'project.deleted' AND entity_id = $1 AND NOT payload ? 'detachedIssues'`, p.ID); n != 1 {
		t.Fatal("payload holds detachedIssues")
	}
	// The event is the LAST of the transaction: nothing newer in the feed.
	if n := e.countRows(`SELECT count(*) FROM events WHERE seq > (SELECT seq FROM events WHERE type = 'project.deleted')`); n != 0 {
		t.Fatal("project.deleted is not the last event")
	}
}

func TestDeleteTeam(t *testing.T) {
	e := newEnv(t)
	e.mkTeam("AAA")
	e.mkTeam("BBB")
	prod := e.mkProduct(map[string]any{"name": "Grp", "teams": []string{"AAA", "BBB"}})
	full := e.createIssue(e.chen, map[string]any{"title": "stays", "team": "AAA"})

	expectCode(t, e.do("DELETE", O+"/teams/BBB", e.claude, nil), 403, "insufficient_scope")
	expectCode(t, e.do("DELETE", O+"/teams/NOPE", e.chen, nil), 404, "not_found")

	// 409 while the team holds an issue; nothing is deleted.
	r := e.do("DELETE", O+"/teams/AAA", e.chen, nil)
	expectCode(t, r, 409, "team_in_use")
	if !strings.Contains(string(r.body), "1 issue") {
		t.Fatalf("message %s", r.body)
	}
	if n := e.countRows(`SELECT count(*) FROM issues WHERE id = $1`, full.ID); n != 1 {
		t.Fatal("issue deleted")
	}
	// An archived issue counts too.
	if _, err := e.pool.Exec(t.Context(), `UPDATE issues SET archived_at = now() WHERE id = $1`, full.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, e.do("DELETE", O+"/teams/AAA", e.chen, nil), 409, "team_in_use")
	if e.eventCount("team.deleted") != 0 {
		t.Fatal("event on a refused delete")
	}

	// An empty team goes: states, product link; the product and other team stay.
	e.must(204, e.do("DELETE", O+"/teams/BBB", e.chen, nil))
	expectCode(t, e.do("GET", O+"/teams/BBB", e.chen, nil), 404, "not_found")
	if n := e.countRows(`SELECT count(*) FROM workflow_states ws JOIN teams t ON t.id = ws.team_id WHERE t.key = 'BBB'`); n != 0 {
		t.Fatal("states left")
	}
	if got := e.joinedKeys(prod.ID); len(got) != 1 || got[0] != "AAA" {
		t.Fatalf("product teams %v", got)
	}
	e.must(200, e.do("GET", P+"/grp", e.chen, nil))
	expectCode(t, e.do("DELETE", O+"/teams/BBB", e.chen, nil), 404, "not_found")
	if e.eventCount("team.deleted") != 1 {
		t.Fatal("want one team.deleted")
	}
	if n := e.countRows(`SELECT count(*) FROM events WHERE seq > (SELECT seq FROM events WHERE type = 'team.deleted')`); n != 0 {
		t.Fatal("team.deleted is not the last event")
	}
}

func TestDeleteTeam_DefaultAndLast(t *testing.T) {
	e := newEnv(t)
	// The fixture has one team: the last team is never deleted.
	expectCode(t, e.do("DELETE", O+"/teams/ORB", e.chen, nil), 409, "team_in_use")

	e.mkTeam("AAA")
	e.must(200, e.do("PATCH", O, e.chen, map[string]any{"defaultTeam": "AAA"}))
	r := e.do("DELETE", O+"/teams/AAA", e.chen, nil)
	expectCode(t, r, 409, "team_in_use")
	if !strings.Contains(string(r.body), "default") {
		t.Fatalf("message %s", r.body)
	}
	// ORB is empty and not the default: it can go; AAA is then the last team.
	e.must(204, e.do("DELETE", O+"/teams/ORB", e.chen, nil))
	expectCode(t, e.do("DELETE", O+"/teams/AAA", e.chen, nil), 409, "team_in_use")
	if n := e.countRows(`SELECT count(*) FROM teams WHERE org_id = $1`, e.orgID); n != 1 {
		t.Fatalf("%d teams", n)
	}
}

func TestDeleteProjectAndTeam_CrossOrg(t *testing.T) {
	e := newEnv(t)
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "admin")
	e.mkTeam("ZZZ")
	tok, _ := e.newToken(chen.ID, tokenOpts{}) // unpinned: a member of both orgs
	const A = "/api/v1/orgs/acme"
	var ap tracker.Project
	e.must(201, e.do("POST", A+"/projects", tok, map[string]any{"name": "acme project"})).json(t, &ap)
	op := e.createProject(tok, map[string]any{"name": "orbit project"})

	// Another org's project or team is 404 through this org's path.
	expectCode(t, e.do("DELETE", O+"/projects/"+ap.ID.String(), tok, nil), 404, "not_found")
	expectCode(t, e.do("DELETE", O+"/teams/ACM", tok, nil), 404, "not_found")
	expectCode(t, e.do("DELETE", A+"/projects/"+op.ID.String(), tok, nil), 404, "not_found")
	expectCode(t, e.do("DELETE", A+"/teams/ZZZ", tok, nil), 404, "not_found")
	if n := e.countRows(`SELECT count(*) FROM projects`); n != 2 {
		t.Fatalf("%d projects", n)
	}
	if n := e.countRows(`SELECT count(*) FROM teams WHERE key IN ('ACM','ZZZ')`); n != 2 {
		t.Fatal("a team was deleted")
	}
}

// TestDeleteTeam_ConcurrentCreate races POST /issues against DELETE /teams/{key}
// on a fresh team, many rounds, in both start orders. The first version took
// the team row before the issue counter and deadlocked with a create (500
// both ways). Now: no 5xx, and the end state is consistent: either the issue
// exists (create 201, delete 409 team_in_use) or the team is gone with no
// issue (delete 204, create 400 validation_failed: one status for both
// places the create can lose). Mutant: remove rankLock from DeleteTeam and
// this test fails (500).
func TestDeleteTeam_ConcurrentCreate(t *testing.T) {
	e := newEnv(t)
	const rounds = 40
	for i := 0; i < rounds; i++ {
		key := fmt.Sprintf("R%d", i)
		e.mkTeam(key)
		var cr, dl resp
		var cerr, derr error
		var wg sync.WaitGroup
		wg.Add(2)
		create := func() {
			defer wg.Done()
			cr, cerr = e.request("POST", O+"/issues", e.chen, map[string]any{"title": "race " + key, "team": key})
		}
		del := func() {
			defer wg.Done()
			dl, derr = e.request("DELETE", O+"/teams/"+key, e.chen, nil)
		}
		if i%2 == 0 {
			go create()
			go del()
		} else {
			go del()
			time.Sleep(time.Duration(i%5) * time.Millisecond)
			go create()
		}
		wg.Wait()
		if cerr != nil || derr != nil {
			t.Fatalf("round %d transport: %v %v", i, cerr, derr)
		}
		if cr.status >= 500 || dl.status >= 500 {
			t.Fatalf("round %d: create %d %s; delete %d %s", i, cr.status, cr.body, dl.status, dl.body)
		}
		issues := e.countRows(`SELECT count(*) FROM issues WHERE org_id = $1 AND title = $2`, e.orgID, "race "+key)
		teams := e.countRows(`SELECT count(*) FROM teams WHERE org_id = $1 AND key = $2`, e.orgID, key)
		switch {
		case cr.status == 201 && dl.status == 409:
			if issues != 1 || teams != 1 {
				t.Fatalf("round %d: issue %d team %d", i, issues, teams)
			}
		case cr.status == 400 && dl.status == 204: // 400: the team was gone at the lookup or at the lock
			if issues != 0 || teams != 0 {
				t.Fatalf("round %d: orphan issue %d team %d", i, issues, teams)
			}
		default:
			t.Fatalf("round %d: inconsistent create %d %s, delete %d %s", i, cr.status, cr.body, dl.status, dl.body)
		}
	}
}

// raceOnce runs a and b at once. Start order and gap vary with i, so both
// orders and a spread of interleavings are hit.
func (e *env) raceOnce(i int, a, b func() (resp, error)) (ra, rb resp) {
	e.t.Helper()
	var ea, eb error
	var wg sync.WaitGroup
	wg.Add(2)
	run := func(f func() (resp, error), r *resp, er *error) {
		defer wg.Done()
		*r, *er = f()
	}
	gaps := []time.Duration{0, 0, time.Millisecond, 3 * time.Millisecond}
	if i%2 == 0 {
		go run(a, &ra, &ea)
		time.Sleep(gaps[i%len(gaps)])
		go run(b, &rb, &eb)
	} else {
		go run(b, &rb, &eb)
		time.Sleep(gaps[i%len(gaps)])
		go run(a, &ra, &ea)
	}
	wg.Wait()
	if ea != nil || eb != nil {
		e.t.Fatalf("transport: %v %v", ea, eb)
	}
	return ra, rb
}

func noCollision(t *testing.T, name string, rs ...resp) {
	t.Helper()
	for _, r := range rs {
		if r.status >= 500 || strings.Contains(string(r.body), "collided") {
			t.Fatalf("%s: status %d %s", name, r.status, r.body)
		}
	}
}

// TestDeleteTeam_RacesWithOtherWriters races DELETE /teams/{key} against every
// writer that can name the team or emit an event in the org. No answer may be
// a 5xx or a deadlock ("collided"). Mutants: LockOrgDefaultTeam with FOR
// UPDATE fails the patch-issue-OTH pair (deadlock with events.Emit); removing
// the retryable FK mapping fails the label, project, product and follow pairs.
func TestDeleteTeam_RacesWithOtherWriters(t *testing.T) {
	e := newEnv(t)
	e.mkTeam("OTH")
	oth := e.createIssue(e.chen, map[string]any{"title": "o", "team": "OTH"})
	prod := e.mkProduct(map[string]any{"name": "Racers", "teams": []string{"OTH"}})
	req := func(method, path string, body any) func() (resp, error) {
		return func() (resp, error) { return e.request(method, O+path, e.chen, body) }
	}
	pairs := []struct {
		name string
		mk   func(k string, i int) func() (resp, error)
	}{
		{"patch-issue-OTH", func(k string, i int) func() (resp, error) {
			return req("PATCH", "/issues/"+oth.Identifier, map[string]any{"state": []string{"Done", "Todo", "Backlog"}[i%3]})
		}},
		{"create-in-OTH", func(k string, i int) func() (resp, error) {
			return req("POST", "/issues", map[string]any{"title": "c" + k, "team": "OTH"})
		}},
		{"label-in-T", func(k string, i int) func() (resp, error) {
			return req("POST", "/labels", map[string]any{"name": "l" + k, "team": k})
		}},
		{"project-with-T", func(k string, i int) func() (resp, error) {
			return req("POST", "/projects", map[string]any{"name": "p" + k, "teams": []string{k}})
		}},
		{"product-add-T", func(k string, i int) func() (resp, error) {
			return req("PUT", "/products/"+prod.Slug+"/teams/"+k, nil)
		}},
		{"product-set-T", func(k string, i int) func() (resp, error) {
			return req("PUT", "/products/"+prod.Slug+"/teams", map[string]any{"teams": []string{"OTH", k}})
		}},
		{"notification-settings-T", func(k string, i int) func() (resp, error) {
			return req("PUT", "/me/notification-settings/teams/"+k, map[string]any{"events": map[string]any{"comment": map[string]any{"email": "on"}}})
		}},
		{"follow-T", func(k string, i int) func() (resp, error) {
			return req("PUT", "/me/teams/"+k+"/follow", nil)
		}},
	}
	for pi, p := range pairs {
		for i := 0; i < 30; i++ {
			k := fmt.Sprintf("Q%c%d", rune('A'+pi), i)
			e.mkTeam(k)
			d, o := e.raceOnce(i, req("DELETE", "/teams/"+k, nil), p.mk(k, i))
			noCollision(t, p.name+" "+k, d, o)
			if d.status != 204 && d.status != 409 && d.status != 404 {
				t.Fatalf("%s %s: delete answered %d %s", p.name, k, d.status, d.body)
			}
		}
	}
}

// TestDeleteProject_RacesWithWriters races DELETE /projects/{id} against every
// writer that names the project or one of its milestones. No answer may be a
// 500 (the documented 503 for a deadlock is allowed). Mutant: remove the
// "_project_fk" / "issues_milestone_fk" entries of database.goneFKs and the
// issue, patch, milestone and document pairs fail with 500.
func TestDeleteProject_RacesWithWriters(t *testing.T) {
	e := newEnv(t)
	iss := e.createIssue(e.chen, map[string]any{"title": "mover"})
	req := func(method, path string, body any) func() (resp, error) {
		return func() (resp, error) { return e.request(method, O+path, e.chen, body) }
	}
	pairs := []struct {
		name string
		mk   func(id string) func() (resp, error)
	}{
		{"issue-in-project", func(id string) func() (resp, error) {
			return req("POST", "/issues", map[string]any{"title": "x", "projectId": id})
		}},
		{"patch-issue-project", func(id string) func() (resp, error) {
			return req("PATCH", "/issues/"+iss.Identifier, map[string]any{"projectId": id})
		}},
		{"milestone-in-project", func(id string) func() (resp, error) {
			return req("POST", "/milestones", map[string]any{"projectId": id, "name": "m"})
		}},
		{"document-in-project", func(id string) func() (resp, error) {
			return req("POST", "/documents", map[string]any{"title": "d", "projectId": id})
		}},
		{"patch-project-teams", func(id string) func() (resp, error) {
			return req("PATCH", "/projects/"+id, map[string]any{"teams": []string{"ORB"}})
		}},
	}
	for pi, p := range pairs {
		for i := 0; i < 30; i++ {
			pr := e.createProject(e.chen, map[string]any{"name": fmt.Sprintf("P%d-%d", pi, i)})
			d, o := e.raceOnce(i, req("DELETE", "/projects/"+pr.ID.String(), nil), p.mk(pr.ID.String()))
			if d.status != 204 && d.status != 404 && d.status != 503 {
				t.Fatalf("%s: delete answered %d %s", p.name, d.status, d.body)
			}
			if o.status >= 500 && o.status != 503 || d.status >= 500 && d.status != 503 {
				t.Fatalf("%s round %d: %d %s / %d %s", p.name, i, d.status, d.body, o.status, o.body)
			}
		}
	}
}

// lostRace forces a write to lose its race with a delete, every time. An open
// transaction deletes the row; the writer's lookup (a plain read) still sees
// the row, its insert then waits on the deleted row's lock; once a backend is
// seen waiting, the delete commits and the writer's foreign key fails.
func (e *env) lostRace(deleteSQL string, args []any, method, path string, body any) resp {
	e.t.Helper()
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, deleteSQL, args...); err != nil {
		e.t.Fatal(err)
	}
	type out struct {
		r   resp
		err error
	}
	ch := make(chan out, 1)
	go func() {
		r, err := e.request(method, path, e.chen, body)
		ch <- out{r, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for e.countRows(`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`) == 0 {
		if time.Now().After(deadline) {
			e.t.Fatalf("%s %s never waited on the deleted row", method, path)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := tx.Commit(ctx); err != nil {
		e.t.Fatal(err)
	}
	o := <-ch
	if o.err != nil {
		e.t.Fatal(o.err)
	}
	return o.r
}

// TestLostRace_IsCleanNotFound: a write that names a team or a project that a
// delete removed under it answers 400 (named in the body) or 404 (named in the
// path), never 500.
func TestLostRace_IsCleanNotFound(t *testing.T) {
	e := newEnv(t)
	e.mkTeam("LRA")
	prod := e.mkProduct(map[string]any{"name": "Lost", "teams": []string{"ORB"}})
	delTeam := func(key string) (string, []any) {
		return `DELETE FROM teams WHERE org_id = $1 AND key = $2`, []any{e.orgID, key}
	}
	cases := []struct {
		name         string
		team         string
		method, path string
		body         any
		want         int
		code         string
	}{
		{"follow (path)", "LRB", "PUT", O + "/me/teams/LRB/follow", nil, 404, "not_found"},
		{"notification settings (path)", "LRC", "PUT", O + "/me/notification-settings/teams/LRC",
			map[string]any{"events": map[string]any{"comment": map[string]any{"email": "on"}}}, 404, "not_found"},
		{"product add team (path)", "LRD", "PUT", O + "/products/" + prod.Slug + "/teams/LRD", nil, 404, "not_found"},
		{"label (body)", "LRE", "POST", O + "/labels", map[string]any{"name": "lost", "team": "LRE"}, 400, "validation_failed"},
		{"project teams (body)", "LRF", "POST", O + "/projects", map[string]any{"name": "lost", "teams": []string{"LRF"}}, 400, "validation_failed"},
	}
	for _, c := range cases {
		e.mkTeam(c.team)
		sql, args := delTeam(c.team)
		r := e.lostRace(sql, args, c.method, c.path, c.body)
		if r.status != c.want {
			t.Fatalf("%s: %d %s, want %d", c.name, r.status, r.body, c.want)
		}
		expectCode(t, r, c.want, c.code)
	}

	// Projects: the same, for the project FKs.
	pcases := []struct {
		name         string
		method, path string
		body         func(id string) any
	}{
		{"issue (body)", "POST", O + "/issues", func(id string) any { return map[string]any{"title": "x", "team": "ORB", "projectId": id} }},
		{"milestone (body)", "POST", O + "/milestones", func(id string) any { return map[string]any{"projectId": id, "name": "m"} }},
		{"document (body)", "POST", O + "/documents", func(id string) any { return map[string]any{"title": "d", "projectId": id} }},
	}
	for _, c := range pcases {
		pr := e.createProject(e.chen, map[string]any{"name": "lost " + c.name})
		r := e.lostRace(`DELETE FROM projects WHERE org_id = $1 AND id = $2`, []any{e.orgID, pr.ID}, c.method, c.path, c.body(pr.ID.String()))
		if r.status != 400 {
			t.Fatalf("%s: %d %s, want 400", c.name, r.status, r.body)
		}
		expectCode(t, r, 400, "validation_failed")
	}
}
