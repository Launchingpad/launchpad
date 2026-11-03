package api

import (
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/tracker"
)

// T3: team cadence, projects, milestones, cycles, documents and
// document comments through the HTTP API. The cycle job itself (fake clock,
// DST, catch-up, rollover, races) is tested in
// internal/tracker/cycles_integration_test.go.

const O = "/api/v1/orgs/orbit"

func (e *env) enableCycles(token, team string) tracker.Team {
	e.t.Helper()
	var t tracker.Team
	e.must(200, e.do("PATCH", O+"/teams/"+team, token, map[string]any{"cyclesEnabled": true})).json(e.t, &t)
	return t
}

func (e *env) cyclesOf(token, query string) []tracker.Cycle {
	e.t.Helper()
	var p page[tracker.Cycle]
	e.must(200, e.do("GET", O+"/cycles?"+query, token, nil)).json(e.t, &p)
	return p.Items
}

func (e *env) eventCount(typ string) int {
	e.t.Helper()
	return e.countRows(`SELECT count(*) FROM events WHERE type = $1`, typ)
}

func (e *env) createProject(token string, body map[string]any) tracker.Project {
	e.t.Helper()
	var p tracker.Project
	e.must(201, e.do("POST", O+"/projects", token, body)).json(e.t, &p)
	return p
}

func expectCode(t *testing.T, r resp, status int, code string) {
	t.Helper()
	if r.status != status {
		t.Fatalf("status %d, want %d: %s", r.status, status, r.body)
	}
	if got := r.errCode(t); got != code {
		t.Fatalf("code %s, want %s", got, code)
	}
}

// AC: with the Orbit cadence the next cycle exists before the current one
// ends. Enabling cycles on a team runs the job for it at once.
func TestTeams_UpdateCadenceCreatesCycles(t *testing.T) {
	// The server clock is pinned, so "today" below is the server's today
	// even if the test runs across a London midnight.
	pinned := time.Now()
	e := newEnvWith(t, envOpts{now: func() time.Time { return pinned }})
	// The team PATCH needs the admin scope; claude has write.
	expectCode(t, e.do("PATCH", O+"/teams/ORB", e.claude, map[string]any{"cyclesEnabled": true}), 403, "insufficient_scope")
	for name, body := range map[string]map[string]any{
		"unknown zone":      {"cycleTimezone": "Mars/Olympus"},
		"server zone":       {"cycleTimezone": "Local"},
		"weekday 7":         {"cycleStartWeekday": 7},
		"length 0":          {"cycleLengthDays": 0},
		"length 57":         {"cycleLengthDays": 57},
		"bad time":          {"cycleStartTime": "25:00"},
		"null enabled":      {"cyclesEnabled": nil},
		"null rollover":     {"cycleRollover": nil},
		"empty name":        {"name": " "},
		"unknown field key": {"key": "NEW"},
	} {
		t.Run(name, func(t *testing.T) {
			r := e.do("PATCH", O+"/teams/ORB", e.chen, body)
			if r.status != 400 {
				t.Fatalf("status %d: %s", r.status, r.body)
			}
		})
	}
	if n := e.eventCount("team.updated"); n != 0 {
		t.Fatalf("a refused PATCH wrote %d events", n)
	}

	team := e.enableCycles(e.chen, "ORB")
	if !team.CyclesEnabled || team.CycleLengthDays != 14 || team.CycleStartWeekday != 0 ||
		team.CycleStartTime != "00:00" || team.CycleTimezone != "Europe/London" || !team.CycleRollover {
		t.Fatalf("team after enable: %+v", team)
	}
	cs := e.cyclesOf(e.chen, "team=ORB")
	if len(cs) != 2 || cs[0].Number != 2 || cs[1].Number != 1 {
		t.Fatalf("cycles %+v, want 2 then 1 (latest start first)", cs)
	}
	now := pinned
	cur, next := cs[1], cs[0]
	if now.Before(cur.StartsAt) || !now.Before(cur.EndsAt) || !next.StartsAt.Equal(cur.EndsAt) || !next.StartsAt.After(now) {
		t.Fatalf("current %s..%s, next %s..%s at %s", cur.StartsAt, cur.EndsAt, next.StartsAt, next.EndsAt, now)
	}
	if active := e.cyclesOf(e.chen, "active=true"); len(active) != 1 || active[0].ID != cur.ID {
		t.Fatalf("active=true gave %+v", active)
	}
	if inactive := e.cyclesOf(e.chen, "active=false"); len(inactive) != 1 || inactive[0].ID != next.ID {
		t.Fatalf("active=false gave %+v", inactive)
	}
	if e.eventCount("team.updated") != 1 || e.eventCount("cycle.created") != 2 {
		t.Fatalf("events: team.updated %d, cycle.created %d", e.eventCount("team.updated"), e.eventCount("cycle.created"))
	}
	// The same PATCH again changes nothing and writes nothing.
	e.enableCycles(e.chen, "ORB")
	if e.eventCount("team.updated") != 1 || len(e.cyclesOf(e.chen, "")) != 2 {
		t.Fatal("a no-op PATCH wrote events or cycles")
	}

	// The issue filter cycle=active finds issues of that cycle.
	in := e.createIssue(e.chen, map[string]any{"title": "in the cycle", "cycleId": cur.ID, "estimate": 3})
	e.createIssue(e.chen, map[string]any{"title": "outside"})
	var ip page[tracker.Issue]
	e.must(200, e.do("GET", O+"/issues?cycle=active", e.chen, nil)).json(t, &ip)
	if ids := identifiers(ip.Items); !slices.Equal(ids, []string{in.Identifier}) {
		t.Fatalf("cycle=active lists %v", ids)
	}

	// AC: today's snapshot is visible, and the GET refreshes it.
	reader, _ := e.newToken(e.user("claude").ID, tokenOpts{pin: &e.orgID, scopes: []string{"read"}})
	var sp page[tracker.CycleSnapshot]
	e.must(200, e.do("GET", O+"/cycles/"+cur.ID.String()+"/snapshots", reader, nil)).json(t, &sp)
	loc, _ := time.LoadLocation("Europe/London")
	today := pinned.In(loc).Format(time.DateOnly)
	if len(sp.Items) != 1 || sp.Items[0].Date != today || sp.Items[0].ScopeCount != 1 || sp.Items[0].ScopePoints != 3 ||
		sp.Items[0].BacklogCount != 1 || sp.NextCursor != nil {
		t.Fatalf("snapshots %+v, want one row for %s with the issue", sp, today)
	}
	e.must(200, e.do("PATCH", O+"/issues/"+in.Identifier, e.chen, map[string]any{"state": "Done"}))
	e.must(200, e.do("GET", O+"/cycles/"+cur.ID.String()+"/snapshots", reader, nil)).json(t, &sp)
	if len(sp.Items) != 1 || sp.Items[0].CompletedCount != 1 || sp.Items[0].CompletedPoints != 3 {
		t.Fatalf("snapshot was not refreshed: %+v", sp.Items)
	}
	// An upcoming cycle has no burn-up yet.
	e.must(200, e.do("GET", O+"/cycles/"+next.ID.String()+"/snapshots", reader, nil)).json(t, &sp)
	if len(sp.Items) != 0 {
		t.Fatalf("upcoming cycle has snapshots %+v", sp.Items)
	}

	// Renaming a cycle: write scope, null clears.
	expectCode(t, e.do("PATCH", O+"/cycles/"+cur.ID.String(), reader, map[string]any{"name": "x"}), 403, "insufficient_scope")
	var c tracker.Cycle
	e.must(200, e.do("PATCH", O+"/cycles/"+cur.ID.String(), e.claude, map[string]any{"name": "Sprint A", "description": "d"})).json(t, &c)
	if c.Name == nil || *c.Name != "Sprint A" || c.Description != "d" || c.Team.Key != "ORB" {
		t.Fatalf("renamed cycle %+v", c)
	}
	e.must(200, e.do("PATCH", O+"/cycles/"+cur.ID.String(), e.claude, map[string]any{"name": nil})).json(t, &c)
	if c.Name != nil {
		t.Fatalf("null did not clear the name: %v", *c.Name)
	}
	// The number still belongs to the job. The dates are editable since LAUN-172 (decisions-T27):
	// the start of a running cycle is refused by a rule, not as an unknown field.
	expectCode(t, e.do("PATCH", O+"/cycles/"+cur.ID.String(), e.claude, map[string]any{"number": 9}), 400, "bad_request")
	expectCode(t, e.do("PATCH", O+"/cycles/"+cur.ID.String(), e.claude, map[string]any{"startsAt": now}), 409, "cycle_conflict")
	if e.eventCount("cycle.updated") != 2 {
		t.Fatalf("cycle.updated events %d", e.eventCount("cycle.updated"))
	}
	e.must(200, e.do("GET", O+"/cycles/"+cur.ID.String(), reader, nil)).json(t, &c)
	expectCode(t, e.do("GET", O+"/cycles/"+uuid.NewString(), reader, nil), 404, "not_found")
	expectCode(t, e.do("GET", O+"/cycles/not-an-id", reader, nil), 404, "not_found")
	expectCode(t, e.do("GET", O+"/cycles?active=maybe", reader, nil), 400, "validation_failed")
}

// AC: projects: create, read, update, list with filters and cursor pages.
func TestProjects_CRUDListFilters(t *testing.T) {
	e := newEnv(t)
	long := strings.Repeat("ü", 450)
	a := e.createProject(e.chen, map[string]any{"name": "Alpha", "lead": "me", "teams": []string{"orb"}, "state": "started",
		"startDate": "2026-10-01", "targetDate": "2026-12-01", "content": long})
	if a.Lead == nil || a.Lead.Username != "chen" || len(a.Teams) != 1 || a.Teams[0].Key != "ORB" ||
		a.StartedAt == nil || a.Version != 1 || a.Content != long || a.ContentTruncated || *a.StartDate != "2026-10-01" {
		t.Fatalf("project a = %+v", a)
	}
	b := e.createProject(e.claude, map[string]any{"name": "Beta", "lead": "claude"})
	c := e.createProject(e.chen, map[string]any{"name": "Gamma", "state": "completed"})
	if b.State != "planned" || c.CompletedAt == nil || c.StartedAt == nil {
		t.Fatalf("b %+v, c %+v", b, c)
	}
	for name, body := range map[string]map[string]any{
		"lead not a member": {"name": "x", "lead": "nobody"},
		"unknown team":      {"name": "x", "teams": []string{"NOPE"}},
		"bad state":         {"name": "x", "state": "done"},
		"target < start":    {"name": "x", "startDate": "2026-12-01", "targetDate": "2026-11-01"},
		"bad date":          {"name": "x", "startDate": "1st of May"},
		"empty name":        {"name": "  "},
	} {
		t.Run(name, func(t *testing.T) {
			expectCode(t, e.do("POST", O+"/projects", e.chen, body), 400, "validation_failed")
		})
	}
	var got tracker.Project
	e.must(200, e.do("GET", O+"/projects/"+a.ID.String(), e.claude, nil)).json(t, &got)
	if got.ID != a.ID || got.Content != long {
		t.Fatalf("GET project %+v", got)
	}
	expectCode(t, e.do("GET", O+"/projects/"+uuid.NewString(), e.chen, nil), 404, "not_found")

	names := func(q string) []string {
		t.Helper()
		var p page[tracker.Project]
		e.must(200, e.do("GET", O+"/projects?"+q, e.chen, nil)).json(t, &p)
		out := []string{}
		for _, pr := range p.Items {
			out = append(out, pr.Name)
		}
		return out
	}
	for _, tt := range []struct{ q, want string }{
		{"", "[Gamma Beta Alpha]"},
		{"state=started", "[Alpha]"},
		{"state=planned,completed", "[Gamma Beta]"},
		{"lead=claude", "[Beta]"},
		{"lead=me", "[Alpha]"},
		{"lead=none", "[Gamma]"},
		{"team=ORB", "[Alpha]"},
		{"query=ET", "[Beta]"},
	} {
		if g := names(tt.q); strings.Join(g, " ") != strings.Trim(tt.want, "[]") {
			t.Fatalf("?%s = %v, want %s", tt.q, g, tt.want)
		}
	}
	var p1, p2 page[tracker.Project]
	e.must(200, e.do("GET", O+"/projects?limit=2", e.chen, nil)).json(t, &p1)
	e.must(200, e.do("GET", O+"/projects?limit=2&cursor="+url.QueryEscape(*p1.NextCursor), e.chen, nil)).json(t, &p2)
	if len(p1.Items) != 2 || len(p2.Items) != 1 || p2.Items[0].Name != "Alpha" || p2.NextCursor != nil {
		t.Fatalf("pages %d + %d, last cursor %v", len(p1.Items), len(p2.Items), p2.NextCursor)
	}
	if !p2.Items[0].ContentTruncated || len([]rune(p2.Items[0].Content)) != 400 {
		t.Fatal("list did not cut content")
	}
	for _, q := range []string{"limit=251", "state=done", "leader=me", "cursor=nope"} {
		expectCode(t, e.do("GET", O+"/projects?"+q, e.chen, nil), 400, "validation_failed")
	}
	// A cursor from another list is refused.
	var dp page[tracker.Document]
	e.createDocument(e.chen, "d1")
	e.createDocument(e.chen, "d2")
	e.must(200, e.do("GET", O+"/documents?limit=1", e.chen, nil)).json(t, &dp)
	expectCode(t, e.do("GET", O+"/projects?cursor="+url.QueryEscape(*dp.NextCursor), e.chen, nil), 400, "validation_failed")

	// PATCH: CAS, replace teams, clear lead, no-op.
	expectCode(t, e.do("PATCH", O+"/projects/"+a.ID.String(), e.chen, map[string]any{"version": 7, "name": "x"}), 409, "version_conflict")
	var upd tracker.Project
	e.must(200, e.do("PATCH", O+"/projects/"+a.ID.String(), e.chen, map[string]any{
		"version": 1, "name": "Alpha 2", "teams": []string{}, "lead": nil, "state": "paused", "targetDate": nil,
	})).json(t, &upd)
	if upd.Name != "Alpha 2" || len(upd.Teams) != 0 || upd.Lead != nil || upd.Version != 2 || upd.State != "paused" ||
		upd.StartedAt == nil || upd.TargetDate != nil {
		t.Fatalf("patched %+v", upd)
	}
	e.must(200, e.do("PATCH", O+"/projects/"+a.ID.String(), e.chen, map[string]any{"teams": []string{"ORB"}})).json(t, &upd)
	if upd.Version != 3 || len(upd.Teams) != 1 {
		t.Fatalf("teams-only change: %+v", upd)
	}
	e.must(200, e.do("PATCH", O+"/projects/"+a.ID.String(), e.chen, map[string]any{"name": "Alpha 2", "teams": []string{"ORB"}})).json(t, &upd)
	if upd.Version != 3 {
		t.Fatalf("a no-op PATCH bumped the version to %d", upd.Version)
	}
	expectCode(t, e.do("PATCH", O+"/projects/"+a.ID.String(), e.chen, map[string]any{"startDate": "2027-01-01", "targetDate": "2026-12-01"}), 400, "validation_failed")
	if n := e.countRows(`SELECT count(*) FROM events WHERE type = 'project.updated' AND entity_id = $1`, a.ID); n != 2 {
		t.Fatalf("%d project.updated events, want 2", n)
	}
	// Scopes: a read token cannot write.
	reader, _ := e.newToken(e.user("claude").ID, tokenOpts{pin: &e.orgID, scopes: []string{"read"}})
	expectCode(t, e.do("POST", O+"/projects", reader, map[string]any{"name": "x"}), 403, "insufficient_scope")
	expectCode(t, e.do("PATCH", O+"/projects/"+a.ID.String(), reader, map[string]any{"name": "x"}), 403, "insufficient_scope")
}

func (e *env) createDocument(token, title string) tracker.Document {
	e.t.Helper()
	var d tracker.Document
	e.must(201, e.do("POST", O+"/documents", token, map[string]any{"title": title})).json(e.t, &d)
	return d
}

// AC: milestones: create, read, update, list by project.
func TestMilestones_CRUDAndProjectFilter(t *testing.T) {
	e := newEnv(t)
	p := e.createProject(e.chen, map[string]any{"name": "P"})
	q := e.createProject(e.chen, map[string]any{"name": "Q"})
	var m1, m2 tracker.Milestone
	e.must(201, e.do("POST", O+"/milestones", e.claude, map[string]any{"projectId": p.ID, "name": "M1", "targetDate": "2026-12-24", "sortOrder": 2})).json(t, &m1)
	e.must(201, e.do("POST", O+"/milestones", e.claude, map[string]any{"projectId": q.ID, "name": "M2"})).json(t, &m2)
	if m1.ProjectID != p.ID || *m1.TargetDate != "2026-12-24" || m1.SortOrder != 2 {
		t.Fatalf("m1 %+v", m1)
	}
	expectCode(t, e.do("POST", O+"/milestones", e.chen, map[string]any{"name": "x"}), 400, "validation_failed")
	expectCode(t, e.do("POST", O+"/milestones", e.chen, map[string]any{"projectId": uuid.New(), "name": "x"}), 400, "validation_failed")

	var mp page[tracker.Milestone]
	e.must(200, e.do("GET", O+"/milestones?project="+p.ID.String(), e.chen, nil)).json(t, &mp)
	if len(mp.Items) != 1 || mp.Items[0].ID != m1.ID {
		t.Fatalf("project filter %+v", mp.Items)
	}
	e.must(200, e.do("GET", O+"/milestones?limit=1", e.chen, nil)).json(t, &mp)
	if len(mp.Items) != 1 || mp.NextCursor == nil {
		t.Fatalf("limit=1: %+v", mp)
	}
	expectCode(t, e.do("GET", O+"/milestones?project="+uuid.NewString(), e.chen, nil), 400, "validation_failed")
	expectCode(t, e.do("GET", O+"/milestones?project=P", e.chen, nil), 400, "validation_failed")

	var got tracker.Milestone
	e.must(200, e.do("PATCH", O+"/milestones/"+m1.ID.String(), e.chen, map[string]any{"name": "M1b", "targetDate": nil, "sortOrder": 0.5})).json(t, &got)
	if got.Name != "M1b" || got.TargetDate != nil || got.SortOrder != 0.5 {
		t.Fatalf("patched %+v", got)
	}
	expectCode(t, e.do("PATCH", O+"/milestones/"+m1.ID.String(), e.chen, map[string]any{"projectId": q.ID}), 400, "bad_request")
	e.must(200, e.do("GET", O+"/milestones/"+m1.ID.String(), e.chen, nil)).json(t, &got)
	if got.Name != "M1b" {
		t.Fatalf("GET %+v", got)
	}
	if e.eventCount("milestone.created") != 2 || e.eventCount("milestone.updated") != 1 {
		t.Fatal("milestone events")
	}
}

// AC: documents: create, read (id or slug), update with CAS, list with
// project and query; documents take comments.
func TestDocuments_CRUDAndComments(t *testing.T) {
	e := newEnv(t)
	p := e.createProject(e.chen, map[string]any{"name": "P"})
	var d tracker.Document
	e.must(201, e.do("POST", O+"/documents", e.chen, map[string]any{"title": "Design Doc", "content": "the burn-up plan", "projectId": p.ID})).json(t, &d)
	if !strings.HasPrefix(d.Slug, "design-doc-") || d.Creator == nil || d.Creator.Username != "chen" || d.Version != 1 || *d.ProjectID != p.ID {
		t.Fatalf("doc %+v", d)
	}
	other := e.createDocument(e.claude, "Notes")
	for _, ref := range []string{d.ID.String(), d.Slug} {
		var g tracker.Document
		e.must(200, e.do("GET", O+"/documents/"+ref, e.claude, nil)).json(t, &g)
		if g.ID != d.ID {
			t.Fatalf("GET %s = %s", ref, g.ID)
		}
	}
	expectCode(t, e.do("GET", O+"/documents/no-such-slug", e.chen, nil), 404, "not_found")
	expectCode(t, e.do("POST", O+"/documents", e.chen, map[string]any{"title": ""}), 400, "validation_failed")
	expectCode(t, e.do("POST", O+"/documents", e.chen, map[string]any{"title": "x", "projectId": uuid.New()}), 400, "validation_failed")

	docs := func(q string) []uuid.UUID {
		t.Helper()
		var dp page[tracker.Document]
		e.must(200, e.do("GET", O+"/documents?"+q, e.chen, nil)).json(t, &dp)
		out := []uuid.UUID{}
		for _, x := range dp.Items {
			out = append(out, x.ID)
		}
		return out
	}
	if g := docs("query=BURN-UP"); !slices.Equal(g, []uuid.UUID{d.ID}) {
		t.Fatalf("query on content: %v", g)
	}
	if g := docs("project=" + p.ID.String()); !slices.Equal(g, []uuid.UUID{d.ID}) {
		t.Fatalf("project filter: %v", g)
	}
	if g := docs(""); !slices.Equal(g, []uuid.UUID{other.ID, d.ID}) {
		t.Fatalf("all: %v", g)
	}
	expectCode(t, e.do("GET", O+"/documents?project="+uuid.NewString(), e.chen, nil), 400, "validation_failed")

	expectCode(t, e.do("PATCH", O+"/documents/"+d.Slug, e.chen, map[string]any{"version": 9, "title": "x"}), 409, "version_conflict")
	var u tracker.Document
	e.must(200, e.do("PATCH", O+"/documents/"+d.Slug, e.claude, map[string]any{"version": 1, "title": "Renamed", "projectId": nil})).json(t, &u)
	if u.Title != "Renamed" || u.Slug != d.Slug || u.Version != 2 || u.ProjectID != nil {
		t.Fatalf("patched %+v", u)
	}
	e.must(200, e.do("PATCH", O+"/documents/"+d.ID.String(), e.claude, map[string]any{"title": "Renamed"})).json(t, &u)
	if u.Version != 2 {
		t.Fatal("no-op PATCH bumped the version")
	}

	// Comments on a document.
	base := O + "/documents/" + d.Slug + "/comments"
	var c1, c2 tracker.Comment
	e.must(201, e.do("POST", base, e.chen, map[string]any{"body": "first"})).json(t, &c1)
	if c1.DocumentID == nil || *c1.DocumentID != d.ID || c1.IssueID != nil {
		t.Fatalf("doc comment %+v", c1)
	}
	e.must(201, e.do("POST", base, e.claude, map[string]any{"body": "reply", "parentId": c1.ID})).json(t, &c2)
	// A reply must be to a comment on the same document.
	is := e.createIssue(e.chen, map[string]any{"title": "an issue"})
	var ic tracker.Comment
	e.must(201, e.do("POST", O+"/issues/"+is.Identifier+"/comments", e.chen, map[string]any{"body": "on the issue"})).json(t, &ic)
	if ic.IssueID == nil || *ic.IssueID != is.ID || ic.DocumentID != nil {
		t.Fatalf("issue comment %+v", ic)
	}
	expectCode(t, e.do("POST", base, e.chen, map[string]any{"body": "x", "parentId": ic.ID}), 400, "validation_failed")
	expectCode(t, e.do("POST", O+"/issues/"+is.Identifier+"/comments", e.chen, map[string]any{"body": "x", "parentId": c1.ID}), 400, "validation_failed")
	expectCode(t, e.do("POST", O+"/documents/"+other.ID.String()+"/comments", e.chen, map[string]any{"body": "x", "parentId": c1.ID}), 400, "validation_failed")
	var cl page[tracker.Comment]
	e.must(200, e.do("GET", base, e.claude, nil)).json(t, &cl)
	if len(cl.Items) != 2 || cl.Items[0].ID != c1.ID || cl.Items[1].ParentID == nil || *cl.Items[1].ParentID != c1.ID {
		t.Fatalf("doc comments %+v", cl.Items)
	}
	// The issue's comment list does not show document comments.
	e.must(200, e.do("GET", O+"/issues/"+is.Identifier+"/comments", e.claude, nil)).json(t, &cl)
	if len(cl.Items) != 1 {
		t.Fatalf("issue comments %+v", cl.Items)
	}
	// Edit and delete through the shared comment routes.
	var ed tracker.Comment
	e.must(200, e.do("PATCH", O+"/comments/"+c2.ID.String(), e.claude, map[string]any{"body": "edited"})).json(t, &ed)
	if ed.Body != "edited" || ed.DocumentID == nil || *ed.DocumentID != d.ID {
		t.Fatalf("edited %+v", ed)
	}
	expectCode(t, e.do("DELETE", O+"/comments/"+c1.ID.String(), e.claude, nil), 403, "forbidden") // not the author
	e.must(204, e.do("DELETE", O+"/comments/"+c2.ID.String(), e.claude, nil))
	if n := e.countRows(`SELECT count(*) FROM events WHERE type = 'comment.deleted' AND (payload->>'documentId')::uuid = $1`, d.ID); n != 1 {
		t.Fatalf("comment.deleted with documentId: %d", n)
	}
	if e.eventCount("document.created") != 2 || e.eventCount("document.updated") != 1 {
		t.Fatal("document events")
	}
}

// Keep the issue API compatible, and check its planning references: the
// project and the milestone in the org (and the milestone in the issue's
// project), and the cycle in the issue's team.
func TestIssues_PlanningRefsAreChecked(t *testing.T) {
	e := newEnv(t)
	e.enableCycles(e.chen, "ORB")
	e.must(201, e.do("POST", O+"/teams", e.chen, map[string]any{"key": "WEB", "name": "Web"}))
	e.enableCycles(e.chen, "WEB")
	orb := e.cyclesOf(e.chen, "team=ORB&active=true")[0]
	web := e.cyclesOf(e.chen, "team=WEB&active=true")[0]
	p := e.createProject(e.chen, map[string]any{"name": "P"})
	q := e.createProject(e.chen, map[string]any{"name": "Q"})
	var mp, mq tracker.Milestone
	e.must(201, e.do("POST", O+"/milestones", e.chen, map[string]any{"projectId": p.ID, "name": "MP"})).json(t, &mp)
	e.must(201, e.do("POST", O+"/milestones", e.chen, map[string]any{"projectId": q.ID, "name": "MQ"})).json(t, &mq)

	// Create.
	expectCode(t, e.do("POST", O+"/issues", e.chen, map[string]any{"team": "ORB", "title": "x", "cycleId": web.ID}), 400, "validation_failed")
	expectCode(t, e.do("POST", O+"/issues", e.chen, map[string]any{"team": "ORB", "title": "x", "milestoneId": mp.ID}), 400, "validation_failed")
	expectCode(t, e.do("POST", O+"/issues", e.chen, map[string]any{"team": "ORB", "title": "x", "projectId": q.ID, "milestoneId": mp.ID}), 400, "validation_failed")
	expectCode(t, e.do("POST", O+"/issues", e.chen, map[string]any{"team": "ORB", "title": "x", "projectId": uuid.New()}), 400, "validation_failed")
	is := e.createIssue(e.chen, map[string]any{"title": "planned", "cycleId": orb.ID, "projectId": p.ID, "milestoneId": mp.ID})
	if *is.CycleID != orb.ID || *is.ProjectID != p.ID || *is.MilestoneID != mp.ID {
		t.Fatalf("created %+v", is)
	}

	path := O + "/issues/" + is.Identifier
	patch := func(body map[string]any) resp { return e.do("PATCH", path, e.chen, body) }
	expectCode(t, patch(map[string]any{"cycleId": web.ID}), 400, "validation_failed")
	expectCode(t, patch(map[string]any{"cycleId": uuid.New()}), 400, "validation_failed")
	// Moving to project Q while keeping MP (a milestone of P) is refused.
	expectCode(t, patch(map[string]any{"projectId": q.ID}), 400, "validation_failed")
	expectCode(t, patch(map[string]any{"milestoneId": mq.ID}), 400, "validation_failed")
	var got tracker.Issue
	e.must(200, patch(map[string]any{"projectId": q.ID, "milestoneId": mq.ID})).json(t, &got)
	if *got.ProjectID != q.ID || *got.MilestoneID != mq.ID {
		t.Fatalf("moved %+v", got)
	}
	expectCode(t, patch(map[string]any{"projectId": nil}), 400, "validation_failed") // MQ would be orphaned
	e.must(200, patch(map[string]any{"projectId": nil, "milestoneId": nil, "cycleId": nil})).json(t, &got)
	if got.ProjectID != nil || got.MilestoneID != nil || got.CycleID != nil {
		t.Fatalf("cleared %+v", got)
	}
	// Unchanged refs are not re-checked: a PATCH of the title still works.
	e.must(200, patch(map[string]any{"cycleId": orb.ID}))
	e.must(200, patch(map[string]any{"title": "still planned"}))
}

// AC: cross-org access is 404 for every new resource, and a body reference
// to another org's row is 400 validation_failed.
func TestPlanning_CrossOrgIsolation(t *testing.T) {
	e := newEnv(t)
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "admin")
	tok, _ := e.newToken(chen.ID, tokenOpts{}) // unpinned, a member of both

	e.enableCycles(tok, "ORB")
	orbCycle := e.cyclesOf(tok, "active=true")[0]
	orbTeam := orbCycle.Team
	op := e.createProject(tok, map[string]any{"name": "orbit secret"})
	var om tracker.Milestone
	e.must(201, e.do("POST", O+"/milestones", tok, map[string]any{"projectId": op.ID, "name": "om"})).json(t, &om)
	var od tracker.Document
	e.must(201, e.do("POST", O+"/documents", tok, map[string]any{"title": "orbit doc", "projectId": op.ID})).json(t, &od)
	var oc tracker.Comment
	e.must(201, e.do("POST", O+"/documents/"+od.Slug+"/comments", tok, map[string]any{"body": "orbit"})).json(t, &oc)

	A := "/api/v1/orgs/acme"
	var ap tracker.Project
	e.must(201, e.do("POST", A+"/projects", tok, map[string]any{"name": "acme project"})).json(t, &ap)
	var ad tracker.Document
	e.must(201, e.do("POST", A+"/documents", tok, map[string]any{"title": "acme doc"})).json(t, &ad)

	t.Run("projects", func(t *testing.T) {
		expectCode(t, e.do("GET", A+"/projects/"+op.ID.String(), tok, nil), 404, "not_found")
		expectCode(t, e.do("PATCH", A+"/projects/"+op.ID.String(), tok, map[string]any{"name": "pwned"}), 404, "not_found")
		var p page[tracker.Project]
		e.must(200, e.do("GET", A+"/projects?includeArchived=true", tok, nil)).json(t, &p)
		if len(p.Items) != 1 || p.Items[0].ID != ap.ID {
			t.Fatalf("acme lists %+v", p.Items)
		}
		expectCode(t, e.do("POST", A+"/projects", tok, map[string]any{"name": "x", "teams": []string{orbTeam.ID.String()}}), 400, "validation_failed")
	})
	t.Run("milestones", func(t *testing.T) {
		expectCode(t, e.do("GET", A+"/milestones/"+om.ID.String(), tok, nil), 404, "not_found")
		expectCode(t, e.do("PATCH", A+"/milestones/"+om.ID.String(), tok, map[string]any{"name": "pwned"}), 404, "not_found")
		expectCode(t, e.do("POST", A+"/milestones", tok, map[string]any{"projectId": op.ID, "name": "x"}), 400, "validation_failed")
		expectCode(t, e.do("GET", A+"/milestones?project="+op.ID.String(), tok, nil), 400, "validation_failed")
		var p page[tracker.Milestone]
		e.must(200, e.do("GET", A+"/milestones", tok, nil)).json(t, &p)
		if len(p.Items) != 0 {
			t.Fatalf("acme lists %+v", p.Items)
		}
	})
	t.Run("cycles", func(t *testing.T) {
		expectCode(t, e.do("GET", A+"/cycles/"+orbCycle.ID.String(), tok, nil), 404, "not_found")
		expectCode(t, e.do("PATCH", A+"/cycles/"+orbCycle.ID.String(), tok, map[string]any{"name": "pwned"}), 404, "not_found")
		expectCode(t, e.do("GET", A+"/cycles/"+orbCycle.ID.String()+"/snapshots", tok, nil), 404, "not_found")
		var p page[tracker.Cycle]
		e.must(200, e.do("GET", A+"/cycles?team="+orbTeam.ID.String(), tok, nil)).json(t, &p)
		if len(p.Items) != 0 {
			t.Fatalf("acme lists orbit cycles %+v", p.Items)
		}
		expectCode(t, e.do("PATCH", A+"/teams/"+orbTeam.ID.String(), tok, map[string]any{"cyclesEnabled": false}), 404, "not_found")
	})
	t.Run("documents", func(t *testing.T) {
		for _, ref := range []string{od.ID.String(), od.Slug} {
			expectCode(t, e.do("GET", A+"/documents/"+ref, tok, nil), 404, "not_found")
			expectCode(t, e.do("PATCH", A+"/documents/"+ref, tok, map[string]any{"title": "pwned"}), 404, "not_found")
			expectCode(t, e.do("GET", A+"/documents/"+ref+"/comments", tok, nil), 404, "not_found")
			expectCode(t, e.do("POST", A+"/documents/"+ref+"/comments", tok, map[string]any{"body": "x"}), 404, "not_found")
		}
		expectCode(t, e.do("POST", A+"/documents", tok, map[string]any{"title": "x", "projectId": op.ID}), 400, "validation_failed")
		expectCode(t, e.do("PATCH", A+"/documents/"+ad.ID.String(), tok, map[string]any{"projectId": op.ID}), 400, "validation_failed")
		expectCode(t, e.do("GET", A+"/documents?project="+op.ID.String(), tok, nil), 400, "validation_failed")
		expectCode(t, e.do("POST", A+"/documents/"+ad.ID.String()+"/comments", tok, map[string]any{"body": "x", "parentId": oc.ID}), 400, "validation_failed")
		expectCode(t, e.do("PATCH", A+"/comments/"+oc.ID.String(), tok, map[string]any{"body": "pwned"}), 404, "not_found")
		var p page[tracker.Document]
		e.must(200, e.do("GET", A+"/documents?query=orbit", tok, nil)).json(t, &p)
		if len(p.Items) != 0 {
			t.Fatalf("acme finds %+v", p.Items)
		}
	})
	t.Run("issue references", func(t *testing.T) {
		for name, ref := range map[string]map[string]any{
			// The milestone case sends acme's OWN project, so only the
			// milestone's org check can refuse it.
			"project": {"projectId": op.ID}, "milestone": {"projectId": ap.ID, "milestoneId": om.ID}, "cycle": {"cycleId": orbCycle.ID},
		} {
			t.Run(name, func(t *testing.T) {
				body := map[string]any{"team": "ACM", "title": "x"}
				for k, v := range ref {
					body[k] = v
				}
				expectCode(t, e.do("POST", A+"/issues", tok, body), 400, "validation_failed")
			})
		}
		var acm tracker.Issue
		e.must(201, e.do("POST", A+"/issues", tok, map[string]any{"team": "ACM", "title": "acme"})).json(t, &acm)
		expectCode(t, e.do("PATCH", A+"/issues/"+acm.Identifier, tok, map[string]any{"cycleId": orbCycle.ID}), 400, "validation_failed")
		expectCode(t, e.do("PATCH", A+"/issues/"+acm.Identifier, tok, map[string]any{"projectId": op.ID}), 400, "validation_failed")
	})
	t.Run("events", func(t *testing.T) {
		var p page[tracker.Event]
		e.must(200, e.do("GET", A+"/events?after=0", tok, nil)).json(t, &p)
		for _, ev := range p.Items {
			if ev.EntityID == op.ID || ev.EntityID == od.ID || ev.EntityID == om.ID || ev.EntityID == orbCycle.ID {
				t.Fatalf("acme feed has an orbit event %+v", ev)
			}
		}
	})
}

// Idempotency-Key on the new POSTs: a retry replays, it does not re-create.
func TestPlanning_IdempotentCreates(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct {
		path string
		body map[string]any
	}{
		{O + "/projects", map[string]any{"name": "once"}},
		{O + "/documents", map[string]any{"title": "once"}},
	} {
		r1 := e.must(201, e.do("POST", tc.path, e.chen, tc.body, "Idempotency-Key", "k-"+tc.path))
		r2 := e.must(201, e.do("POST", tc.path, e.chen, tc.body, "Idempotency-Key", "k-"+tc.path))
		if string(r1.body) != string(r2.body) || r2.header.Get("Idempotent-Replayed") != "true" {
			t.Fatalf("%s: replay differs or is not marked", tc.path)
		}
	}
	if e.eventCount("project.created") != 1 || e.eventCount("document.created") != 1 {
		t.Fatal("a replay wrote a second create event")
	}
}

// Review: estimates are bounded (0-1000) on create and PATCH, so a cycle's
// point sums stay small.
func TestIssues_EstimateIsBounded(t *testing.T) {
	e := newEnv(t)
	expectCode(t, e.do("POST", O+"/issues", e.chen, map[string]any{"team": "ORB", "title": "x", "estimate": 1001}), 400, "validation_failed")
	expectCode(t, e.do("POST", O+"/issues", e.chen, map[string]any{"team": "ORB", "title": "x", "estimate": 2147483647}), 400, "validation_failed")
	is := e.createIssue(e.chen, map[string]any{"title": "max", "estimate": 1000})
	if is.Estimate == nil || *is.Estimate != 1000 {
		t.Fatalf("estimate %v", is.Estimate)
	}
	expectCode(t, e.do("PATCH", O+"/issues/"+is.Identifier, e.chen, map[string]any{"estimate": 1001}), 400, "validation_failed")
	expectCode(t, e.do("PATCH", O+"/issues/"+is.Identifier, e.chen, map[string]any{"estimate": -1}), 400, "validation_failed")
	e.must(200, e.do("PATCH", O+"/issues/"+is.Identifier, e.chen, map[string]any{"estimate": 0}))
}

// Review nit: a slug that happens to parse as a UUID still resolves.
func TestDocuments_UUIDShapedSlugResolves(t *testing.T) {
	e := newEnv(t)
	d := e.createDocument(e.chen, "deadbeef cafe babe face")
	if _, err := uuid.Parse(d.Slug); err != nil {
		t.Fatalf("slug %q is not UUID-shaped; the test needs one", d.Slug)
	}
	var g tracker.Document
	e.must(200, e.do("GET", O+"/documents/"+d.Slug, e.chen, nil)).json(t, &g)
	if g.ID != d.ID {
		t.Fatalf("GET by slug found %s", g.ID)
	}
	e.must(201, e.do("POST", O+"/documents/"+d.Slug+"/comments", e.chen, map[string]any{"body": "ok"}))
}

// LAUN-170: the project answers carry live issue counts by state type, from one grouped query.
func TestProjects_IssueCounts(t *testing.T) {
	e := newEnv(t)
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "admin")
	tok, _ := e.newToken(chen.ID, tokenOpts{})
	A := "/api/v1/orgs/acme"

	empty := e.createProject(tok, map[string]any{"name": "empty"})
	if empty.IssueCounts != (tracker.IssueCounts{}) {
		t.Fatalf("empty project: %+v", empty.IssueCounts)
	}
	p := e.createProject(tok, map[string]any{"name": "counted"})
	mk := func(body map[string]any) tracker.Issue {
		body["projectId"] = p.ID
		body["title"] = "t"
		return e.createIssue(tok, body)
	}
	todo := mk(map[string]any{})
	done := mk(map[string]any{})
	e.setState(done.Identifier, "Done")
	canceled := mk(map[string]any{})
	e.setState(canceled.Identifier, "Canceled")
	started := mk(map[string]any{})
	e.setState(started.Identifier, "In Progress")
	sub := mk(map[string]any{"parent": todo.Identifier})
	e.setState(sub.Identifier, "Done")
	archived := mk(map[string]any{})
	e.setState(archived.Identifier, "Done")
	e.exec(`UPDATE issues SET archived_at = now() WHERE id = $1`, archived.ID)
	// unstarted and triage states are in scope but never "started"
	e.setState(mk(map[string]any{}).Identifier, "Todo")
	e.setState(mk(map[string]any{}).Identifier, "Triage")
	// an issue of another project never counts
	other := e.createProject(tok, map[string]any{"name": "other"})
	e.createIssue(tok, map[string]any{"title": "o", "projectId": other.ID})
	// a project of the other org
	var ap tracker.Project
	e.must(201, e.do("POST", A+"/projects", tok, map[string]any{"name": "acme project"})).json(t, &ap)
	// an issue in the other org's project: counted there, never in orbit
	e.must(201, e.do("POST", A+"/issues", tok, map[string]any{"team": "ACM", "title": "a", "projectId": ap.ID}))

	want := tracker.IssueCounts{Total: 7, Scope: 6, Done: 2, Started: 1, Canceled: 1}
	var got tracker.Project
	e.must(200, e.do("GET", O+"/projects/"+p.ID.String(), tok, nil)).json(t, &got)
	if got.IssueCounts != want {
		t.Fatalf("single: %+v, want %+v", got.IssueCounts, want)
	}
	var list page[tracker.Project]
	e.must(200, e.do("GET", O+"/projects", tok, nil)).json(t, &list)
	byID := map[uuid.UUID]tracker.IssueCounts{}
	for _, it := range list.Items {
		byID[it.ID] = it.IssueCounts
	}
	if byID[p.ID] != want || byID[empty.ID] != (tracker.IssueCounts{}) || byID[other.ID].Total != 1 {
		t.Fatalf("list: %+v", byID)
	}
	// a moved issue changes the next read: no cache
	e.setState(todo.Identifier, "Done")
	e.must(200, e.do("GET", O+"/projects/"+p.ID.String(), tok, nil)).json(t, &got)
	if got.IssueCounts.Done != 3 {
		t.Fatalf("after move: %+v", got.IssueCounts)
	}

	// cross org: orbit's project is 404 in acme and acme's list holds only its own, with zero counts
	expectCode(t, e.do("GET", A+"/projects/"+p.ID.String(), tok, nil), 404, "not_found")
	var al page[tracker.Project]
	e.must(200, e.do("GET", A+"/projects", tok, nil)).json(t, &al)
	if len(al.Items) != 1 || al.Items[0].ID != ap.ID || al.Items[0].IssueCounts != (tracker.IssueCounts{Total: 1, Scope: 1}) {
		t.Fatalf("acme lists %+v", al.Items)
	}
}

// LAUN-179: GET /issues?project= takes an id, a name or `none`.
func TestIssueList_ProjectFilter(t *testing.T) {
	e := newEnv(t)
	p := e.createProject(e.chen, map[string]any{"name": "Web"})
	in := e.createIssue(e.chen, map[string]any{"title": "in project", "projectId": p.ID})
	out := e.createIssue(e.chen, map[string]any{"title": "no project"})
	list := func(q string) []string {
		t.Helper()
		var pg page[tracker.Issue]
		e.must(200, e.do("GET", O+"/issues?"+q, e.chen, nil)).json(t, &pg)
		return identifiers(pg.Items)
	}
	// Another org's projects never match: one has a name of its own, one shares orbit's name "Web",
	// and each holds an issue. The filter answers with orbit's rows only, and never an error.
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, e.user("chen").ID, "admin")
	tok, _ := e.newToken(e.user("chen").ID, tokenOpts{})
	A := "/api/v1/orgs/acme"
	var apOnly, apWeb tracker.Project
	e.must(201, e.do("POST", A+"/projects", tok, map[string]any{"name": "Acme only"})).json(t, &apOnly)
	e.must(201, e.do("POST", A+"/projects", tok, map[string]any{"name": "WEB"})).json(t, &apWeb)
	for _, pid := range []uuid.UUID{apOnly.ID, apWeb.ID} {
		e.must(201, e.do("POST", A+"/issues", tok, map[string]any{"team": "ACM", "title": "acme issue", "projectId": pid}))
	}
	for _, q := range []string{
		"project=" + apOnly.ID.String(),
		"project=" + apWeb.ID.String(),
		"project=" + url.QueryEscape("acme only"),
	} {
		if got := list(q); len(got) != 0 {
			t.Fatalf("%s: got %v from another org", q, got)
		}
	}
	// The shared name finds orbit's issue only, never acme's (whose identifier would start with ACM).
	if got := list("project=web"); !slices.Equal(got, []string{in.Identifier}) {
		t.Fatalf("shared name: got %v, want only %s", got, in.Identifier)
	}
	for q, want := range map[string][]string{
		"project=" + p.ID.String():    {in.Identifier},
		"project=web":                 {in.Identifier},
		"project=none":                {out.Identifier},
		"project=" + uuid.NewString(): {},
	} {
		if got := list(q); !slices.Equal(got, want) {
			t.Fatalf("%s: got %v, want %v", q, got, want)
		}
	}
}
