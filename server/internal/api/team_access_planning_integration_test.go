package api

import (
	"bytes"
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"devops/internal/dbq"
	"devops/internal/testdb"
	"devops/internal/tracker"
)

// Ticket 5 of decisions-T20 (LAUN-220): projects, milestones, documents,
// cycles, products and the planning refs of the issue writes, for a
// restricted member. The fixture is Test B's (teamFixture): ORB visible, HID
// hidden, plus addPlanningFixture.

func (f *teamFixture) projectAs(t *testing.T, tok, id string) tracker.Project {
	t.Helper()
	var p tracker.Project
	f.e.must(http.StatusOK, f.e.do("GET", f.orgPath("/projects/"+id), tok, nil)).json(t, &p)
	return p
}

func teamKeysOf(p tracker.Project) []string {
	out := []string{}
	for _, t := range p.Teams {
		out = append(out, t.Key)
	}
	slices.Sort(out)
	return out
}

// storedProjectTeams reads a project's team keys from the database.
func (f *teamFixture) storedProjectTeams(t *testing.T, id string) []string {
	t.Helper()
	rows, err := f.e.pool.Query(context.Background(), `SELECT t.key FROM project_teams pt JOIN teams t ON t.id = pt.team_id
		WHERE pt.org_id = $1 AND pt.project_id = $2 ORDER BY t.key`, f.e.orgID, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		out = append(out, k)
	}
	return out
}

// storedPlanning reads an issue's stored project and milestone ids.
func (f *teamFixture) storedPlanning(t *testing.T, identifier string) (project, milestone *uuid.UUID) {
	t.Helper()
	if err := f.e.pool.QueryRow(context.Background(), `SELECT i.project_id, i.milestone_id FROM issues i JOIN teams t ON t.id = i.team_id
		WHERE i.org_id = $1 AND t.key || '-' || i.number = $2`, f.e.orgID, identifier).Scan(&project, &milestone); err != nil {
		t.Fatal(err)
	}
	return project, milestone
}

// TestPlanningAccess_SharedProject: a project of ORB and HID read by an ORB
// member shows team ORB only, and its issue counts count visible issues only
// (#37). The admin and the all-teams member see both teams and every issue.
func TestPlanningAccess_SharedProject(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	shared := f.plan.shared.String()
	e.createIssue(e.chen, map[string]any{"team": testdb.TeamKey, "title": "visible shared work", "projectId": shared})
	p := f.projectAs(t, f.restricted, shared)
	if got := teamKeysOf(p); !slices.Equal(got, []string{"ORB"}) || p.IssueCounts.Total != 1 {
		t.Errorf("restricted: teams %v, counts %+v; want [ORB] and 1 issue", got, p.IssueCounts)
	}
	for who, tok := range map[string]string{"admin": e.chen, "member": f.member} {
		p := f.projectAs(t, tok, shared)
		if got := teamKeysOf(p); !slices.Equal(got, []string{"HID", "ORB"}) || p.IssueCounts.Total != 2 {
			t.Errorf("%s: teams %v, counts %+v; want [HID ORB] and 2 issues", who, got, p.IssueCounts)
		}
	}
	// The list agrees with the read.
	var page struct {
		Items []tracker.Project `json:"items"`
	}
	e.must(http.StatusOK, e.do("GET", f.orgPath("/projects?team=ORB"), f.restricted, nil)).json(t, &page)
	if len(page.Items) != 1 || page.Items[0].ID != f.plan.shared || page.Items[0].IssueCounts.Total != 1 {
		t.Errorf("restricted ?team=ORB: %+v, want the shared project with 1 issue", page.Items)
	}
	e.must(http.StatusOK, e.do("GET", f.orgPath("/projects?team=HID"), e.chen, nil)).json(t, &page)
	if len(page.Items) != 2 {
		t.Errorf("admin ?team=HID: %d projects, want 2 (hidden and shared)", len(page.Items))
	}
}

// TestPlanningAccess_ProjectTeams: the team rules of #39 and the lead rule of #46.
func TestPlanningAccess_ProjectTeams(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	shared := f.plan.shared.String()
	expect := func(r resp, status int, text string) {
		t.Helper()
		if r.status != status || !strings.Contains(string(r.body), text) {
			t.Fatalf("got %d %s, want %d with %q", r.status, r.body, status, text)
		}
	}
	// Create: a restricted member must name one of their teams.
	expect(e.do("POST", f.orgPath("/projects"), f.restricted, map[string]any{"name": "No team"}), http.StatusBadRequest, "teams must name at least one team")
	expect(e.do("POST", f.orgPath("/projects"), f.restricted, map[string]any{"name": "No team", "teams": []string{}}), http.StatusBadRequest, "teams must name at least one team")
	expect(e.do("POST", f.orgPath("/projects"), f.restricted, map[string]any{"name": "Hid", "teams": []string{"HID"}}), http.StatusBadRequest, `unknown team \"HID\"`)
	// Any member may lead (#46), even one who cannot see the project.
	lid := testdb.AddMember(t, e.pool, e.orgID, "lena", "member")
	testdb.MakeRestricted(t, e.pool, e.orgID, lid)
	var mine tracker.Project
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/projects"), f.restricted,
		map[string]any{"name": "Mine", "teams": []string{"ORB"}, "lead": "lena"})).json(t, &mine)
	if mine.Lead == nil || mine.Lead.Username != "lena" || !slices.Equal(teamKeysOf(mine), []string{"ORB"}) {
		t.Errorf("created %+v, want lead lena and team ORB", mine)
	}
	lena, _ := e.newToken(lid, tokenOpts{})
	e.must(http.StatusNotFound, e.do("GET", f.orgPath("/projects/"+mine.ID.String()), lena, nil))
	// An all-teams member may still create an org-wide project (today's rule).
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/projects"), f.member, map[string]any{"name": "Org wide"}))

	// Edit: the last visible team cannot go; a hidden team cannot be named.
	before := f.storedProjectTeams(t, shared)
	expect(e.do("PATCH", f.orgPath("/projects/"+shared), f.restricted, map[string]any{"teams": []string{}}), http.StatusBadRequest, "at least one of your teams")
	expect(e.do("PATCH", f.orgPath("/projects/"+shared), f.restricted, map[string]any{"teams": nil}), http.StatusBadRequest, "at least one of your teams")
	if got := f.storedProjectTeams(t, shared); !slices.Equal(got, before) {
		t.Fatalf("a refused teams edit changed the rows: %v -> %v", before, got)
	}
	// A field edit keeps every team row.
	e.must(http.StatusOK, e.do("PATCH", f.orgPath("/projects/"+shared), f.restricted, map[string]any{"name": "Shared plan 2"}))
	// The hidden team row survives a replace of the visible ones (#39).
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/teams"), e.chen, map[string]any{"key": "VIS", "name": "Visible too"}))
	testdb.PutInTeams(t, e.pool, e.orgID, f.userID(t, "rita"), f.teamID(t, "VIS"))
	var p tracker.Project
	e.must(http.StatusOK, e.do("PATCH", f.orgPath("/projects/"+shared), f.restricted, map[string]any{"teams": []string{"VIS"}})).json(t, &p)
	if got := teamKeysOf(p); !slices.Equal(got, []string{"VIS"}) {
		t.Errorf("restricted reads teams %v after the replace, want [VIS]", got)
	}
	if got := f.storedProjectTeams(t, shared); !slices.Equal(got, []string{"HID", "VIS"}) {
		t.Errorf("stored teams after the replace = %v, want [HID VIS] (HID kept)", got)
	}
	// The admin's replace is whole, as before.
	e.must(http.StatusOK, e.do("PATCH", f.orgPath("/projects/"+shared), e.chen, map[string]any{"teams": []string{"ORB"}}))
	if got := f.storedProjectTeams(t, shared); !slices.Equal(got, []string{"ORB"}) {
		t.Errorf("stored teams after the admin's replace = %v, want [ORB]", got)
	}

	// A project with no team: only a caller who sees every team sets its teams.
	open := e.createProject(e.chen, map[string]any{"name": "Open plan"})
	op := f.orgPath("/projects/" + open.ID.String())
	expect(e.do("PATCH", op, f.restricted, map[string]any{"teams": []string{"ORB"}}), http.StatusBadRequest, "project with no team")
	e.must(http.StatusOK, e.do("PATCH", op, f.restricted, map[string]any{"teams": []string{}, "name": "Open plan 2"}))
	if got := f.storedProjectTeams(t, open.ID.String()); len(got) != 0 {
		t.Errorf("open project teams = %v, want none", got)
	}
	e.must(http.StatusOK, e.do("PATCH", op, f.member, map[string]any{"teams": []string{"ORB"}}))
	if got := f.storedProjectTeams(t, open.ID.String()); !slices.Equal(got, []string{"ORB"}) {
		t.Errorf("open project teams after the all-teams member = %v, want [ORB]", got)
	}
}

// TestPlanningAccess_IssueProjectNull: #40 on PATCH /issues. A null for a
// hidden project or milestone leaves it; the same hidden id again is the 400
// of a made-up id; a null for a visible one clears it; a visible project
// replacing a hidden one takes the hidden milestone with it.
func TestPlanningAccess_IssueProjectNull(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	child := f.orgPath("/issues/" + f.child)
	hp, hm := f.storedPlanning(t, f.child)
	if hp == nil || hm == nil {
		t.Fatal("the fixture child has no project or milestone")
	}
	seq := f.eventSeq()
	for _, body := range []map[string]any{{"projectId": nil}, {"milestoneId": nil}, {"projectId": nil, "milestoneId": nil}} {
		var is tracker.Issue
		e.must(http.StatusOK, e.do("PATCH", child, f.restricted, body)).json(t, &is)
		if is.ProjectID != nil || is.MilestoneID != nil {
			t.Errorf("PATCH %v: the response shows project %v milestone %v, want both null", body, is.ProjectID, is.MilestoneID)
		}
		if p, m := f.storedPlanning(t, f.child); !eqUUID(p, hp) || !eqUUID(m, hm) {
			t.Fatalf("PATCH %v cleared a hidden value: project %v milestone %v", body, p, m)
		}
	}
	if n := f.eventSeq(); n != seq {
		t.Errorf("a no-op PATCH emitted %d events", n-seq)
	}
	// The same hidden id again answers as a made-up one.
	for field, hidden := range map[string]string{"projectId": hp.String(), "milestoneId": hm.String()} {
		made := uuid.NewString()
		got := e.do("PATCH", child, f.restricted, map[string]any{field: hidden})
		want := e.do("PATCH", child, f.restricted, map[string]any{field: made})
		gb := bytes.ReplaceAll(got.body, []byte(hidden), []byte("<ref>"))
		wb := bytes.ReplaceAll(want.body, []byte(made), []byte("<ref>"))
		if got.status != http.StatusBadRequest || got.status != want.status || !bytes.Equal(gb, wb) {
			t.Errorf("%s = the hidden id: %d %s; made-up: %d %s", field, got.status, gb, want.status, wb)
		}
	}
	// A visible project with the hidden milestone named again: the 400 of a
	// made-up milestone, and the milestone is not cleared.
	r400 := e.do("PATCH", child, f.restricted, map[string]any{"projectId": f.plan.shared, "milestoneId": hm.String()})
	if r400.status != http.StatusBadRequest || !strings.Contains(string(r400.body), "unknown milestoneId") {
		t.Errorf("visible project with the hidden milestone: %d %s", r400.status, r400.body)
	}
	if p, m := f.storedPlanning(t, f.child); !eqUUID(p, hp) || !eqUUID(m, hm) {
		t.Fatalf("a refused PATCH changed the planning: project %v milestone %v", p, m)
	}
	// A visible project replaces the hidden one; the hidden milestone goes with it.
	shared := f.plan.shared
	var is tracker.Issue
	e.must(http.StatusOK, e.do("PATCH", child, f.restricted, map[string]any{"projectId": shared})).json(t, &is)
	if p, m := f.storedPlanning(t, f.child); !eqUUID(p, &shared) || m != nil {
		t.Errorf("after the visible project: project %v milestone %v, want %s and null", p, m, shared)
	}
	if is.ProjectID == nil || *is.ProjectID != shared {
		t.Errorf("the response shows project %v, want %s", is.ProjectID, shared)
	}
	// A null for a visible project clears it.
	e.must(http.StatusOK, e.do("PATCH", child, f.restricted, map[string]any{"projectId": nil}))
	if p, _ := f.storedPlanning(t, f.child); p != nil {
		t.Errorf("a null for a visible project left %v", p)
	}
	// The admin keeps today's rule: a project change with a milestone of
	// another project is a 400 that names it.
	other := e.createIssue(e.chen, map[string]any{"team": "ORB", "title": "admin case", "projectId": hp.String(), "milestoneId": hm.String()})
	r := e.do("PATCH", f.orgPath("/issues/"+other.Identifier), e.chen, map[string]any{"projectId": shared})
	if r.status != http.StatusBadRequest || !strings.Contains(string(r.body), "milestoneId is a milestone of project") {
		t.Errorf("admin project change with a stale milestone: %d %s", r.status, r.body)
	}
}

func eqUUID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// TestPlanningAccess_Documents: a document of a hidden project is 404 by id
// and slug; a document with no project is org-level and visible; a restricted
// member moves it into a visible project.
func TestPlanningAccess_Documents(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	e.must(http.StatusNotFound, e.do("GET", f.orgPath("/documents/"+f.hidden["document"]), f.restricted, nil))
	e.must(http.StatusNotFound, e.do("GET", f.orgPath("/documents/"+f.plan.hiddenDocSlug), f.restricted, nil))
	e.must(http.StatusOK, e.do("GET", f.orgPath("/documents/"+f.plan.hiddenDocSlug), f.member, nil))
	open := f.orgPath("/documents/" + f.plan.openDoc.String())
	e.must(http.StatusOK, e.do("GET", open, f.restricted, nil))
	e.must(http.StatusCreated, e.do("POST", open+"/comments", f.restricted, map[string]any{"body": "hello"}))
	e.must(http.StatusOK, e.do("PATCH", open, f.restricted, map[string]any{"projectId": f.plan.shared}))
	// It now follows the shared project: still visible to the restricted
	// member, and hidden from a member who sees neither ORB nor HID.
	e.must(http.StatusOK, e.do("GET", open, f.restricted, nil))
	nid := testdb.AddMember(t, e.pool, e.orgID, "nora", "member")
	testdb.MakeRestricted(t, e.pool, e.orgID, nid)
	nora, _ := e.newToken(nid, tokenOpts{})
	e.must(http.StatusNotFound, e.do("GET", open, nora, nil))
	var page struct {
		Items []tracker.Document `json:"items"`
	}
	e.must(http.StatusOK, e.do("GET", f.orgPath("/documents"), nora, nil)).json(t, &page)
	if len(page.Items) != 0 {
		t.Errorf("a member with no team lists %d documents, want 0: %+v", len(page.Items), page.Items)
	}
}

// TestPlanningAccess_CycleSnapshotsNoWrite: the lazy refresh of today's
// snapshot row runs only after the check (a row count), and the admin's read
// does write it (so the count can move).
func TestPlanningAccess_CycleSnapshotsNoWrite(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	count := func() int {
		return e.countRows(`SELECT count(*) FROM cycle_snapshots WHERE cycle_id = $1`, f.hidden["cycle"])
	}
	p := f.orgPath("/cycles/" + f.hidden["cycle"] + "/snapshots")
	n := count()
	e.must(http.StatusNotFound, e.do("GET", p, f.restricted, nil))
	e.must(http.StatusNotFound, e.do("GET", f.orgPath("/cycles/"+f.plan.cycleLinear+"/snapshots"), f.restricted, nil))
	if got := count(); got != n {
		t.Fatalf("a restricted read of a hidden cycle wrote %d snapshot rows", got-n)
	}
	e.must(http.StatusOK, e.do("GET", p, e.chen, nil))
	if got := count(); got != n+1 {
		t.Fatalf("the admin's read wrote %d rows, want 1 (the cycle is running)", got-n)
	}
}

// TestPlanningAccess_Products: #69. A product whose teams are all hidden is
// not listed; a product of a visible and a hidden team shows the visible
// team only; ?product= returns visible issues only and a hidden product
// answers as an unknown one.
func TestPlanningAccess_Products(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	type product struct {
		ID       uuid.UUID `json:"id"`
		Slug     string    `json:"slug"`
		TeamKeys []string  `json:"teamKeys"`
		Teams    []struct {
			Key string `json:"key"`
		} `json:"teams"`
	}
	list := func(tok, path string) map[string][]string {
		t.Helper()
		var page struct {
			Items []product `json:"items"`
		}
		e.must(http.StatusOK, e.do("GET", f.orgPath(path), tok, nil)).json(t, &page)
		out := map[string][]string{}
		for _, p := range page.Items {
			out[p.Slug] = p.TeamKeys
		}
		return out
	}
	if got := list(f.restricted, "/products"); len(got) != 1 || !slices.Equal(got["mixed"], []string{"ORB"}) {
		t.Errorf("restricted products = %v, want only mixed with [ORB]", got)
	}
	if got := list(f.restricted, "/teams/ORB/products"); len(got) != 1 || !slices.Equal(got["mixed"], []string{"ORB"}) {
		t.Errorf("restricted ORB products = %v, want mixed with [ORB]", got)
	}
	for who, tok := range map[string]string{"admin": e.chen, "member": f.member} {
		if got := list(tok, "/products"); len(got) != 2 || !slices.Equal(got["mixed"], []string{"HID", "ORB"}) || !slices.Equal(got["secret-product"], []string{"HID"}) {
			t.Errorf("%s products = %v", who, got)
		}
	}
	var d product
	e.must(http.StatusOK, e.do("GET", f.orgPath("/products/mixed"), f.restricted, nil)).json(t, &d)
	if len(d.Teams) != 1 || d.Teams[0].Key != "ORB" || !slices.Equal(d.TeamKeys, []string{"ORB"}) {
		t.Errorf("restricted GET /products/mixed = %+v, want team ORB only", d)
	}
	// ?product=: the hidden product answers as an unknown one; mixed lists
	// visible issues only.
	hidden := e.do("GET", f.orgPath("/issues?product=secret-product"), f.restricted, nil)
	made := e.do("GET", f.orgPath("/issues?product=made-product"), f.restricted, nil)
	if hidden.status != http.StatusOK || !bytes.Equal(hidden.body, made.body) {
		t.Errorf("?product=<hidden>: %d %s; unknown: %d %s", hidden.status, hidden.body, made.status, made.body)
	}
	got, _ := f.listAs(t, f.restricted, "product=mixed&limit=200")
	if len(got) == 0 || slices.ContainsFunc(got, func(id string) bool { return strings.HasPrefix(id, "HID-") }) {
		t.Errorf("restricted ?product=mixed = %v, want ORB issues only", got)
	}
	got, _ = f.listAs(t, e.chen, "product=secret-product&limit=200")
	if !slices.Contains(got, f.hidden["issue"]) {
		t.Errorf("admin ?product=secret-product = %v, want %s", got, f.hidden["issue"])
	}
}

// TestPlanningAccess_CrossOrg: a reference to another org's planning object
// or product answers as an unknown one, also for an admin. (Every path under
// /orgs/{org} is 404 to another org's admin: TestTeamRules_CrossOrg.)
func TestPlanningAccess_CrossOrg(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	far := "/api/v1/orgs/elsewhere"
	var fp struct {
		ID uuid.UUID `json:"id"`
	}
	e.must(http.StatusCreated, e.do("POST", far+"/projects", f.outsider, map[string]any{"name": "Far plan", "teams": []string{"ELS"}})).json(t, &fp)
	var prod struct {
		ID uuid.UUID `json:"id"`
	}
	e.must(http.StatusCreated, e.do("POST", far+"/products", f.outsider, map[string]any{"name": "Far", "slug": "far", "teams": []string{"ELS"}})).json(t, &prod)
	for who, tok := range map[string]string{"admin": e.chen, "restricted": f.restricted} {
		e.must(http.StatusNotFound, e.do("GET", f.orgPath("/products/far"), tok, nil))
		e.must(http.StatusNotFound, e.do("GET", f.orgPath("/products/"+prod.ID.String()), tok, nil))
		if r := e.do("GET", f.orgPath("/issues?product=far"), tok, nil); r.status != http.StatusOK || bytes.Contains(r.body, []byte("ELS")) || !bytes.Contains(r.body, []byte(`"items":[]`)) {
			t.Errorf("%s ?product=far: %d %s", who, r.status, r.body)
		}
		for _, c := range []struct {
			method, path string
			body         map[string]any
			text         string
		}{
			{"POST", "/projects", map[string]any{"name": "x", "teams": []string{"ELS"}}, `unknown team \"ELS\"`},
			{"PATCH", "/projects/" + f.plan.shared.String(), map[string]any{"teams": []string{"ORB", "ELS"}}, `unknown team \"ELS\"`},
			{"POST", "/issues", map[string]any{"team": "ORB", "title": "x", "projectId": fp.ID}, "unknown projectId"},
			{"PATCH", "/issues/" + f.visible, map[string]any{"projectId": fp.ID}, "unknown projectId"},
			{"POST", "/milestones", map[string]any{"name": "x", "projectId": fp.ID}, "unknown projectId"},
			{"POST", "/documents", map[string]any{"title": "x", "projectId": fp.ID}, "unknown projectId"},
			{"PATCH", "/documents/" + f.plan.openDoc.String(), map[string]any{"projectId": fp.ID}, "unknown projectId"},
		} {
			r := e.do(c.method, f.orgPath(c.path), tok, c.body)
			if r.status != http.StatusBadRequest || !strings.Contains(string(r.body), c.text) {
				t.Errorf("%s %s %s %v: %d %s, want 400 %q", who, c.method, c.path, c.body, r.status, r.body, c.text)
			}
		}
		for _, q := range []string{"/milestones?project=", "/documents?project="} {
			r := e.do("GET", f.orgPath(q+fp.ID.String()), tok, nil)
			if r.status != http.StatusBadRequest || !strings.Contains(string(r.body), "unknown project") {
				t.Errorf("%s %s<other org>: %d %s", who, q, r.status, r.body)
			}
		}
		if r := e.do("GET", f.orgPath("/projects?team=ELS"), tok, nil); r.status != http.StatusOK || bytes.Contains(r.body, []byte("Far plan")) {
			t.Errorf("%s /projects?team=ELS: %d %s", who, r.status, r.body)
		}
	}
}

// TestHiddenAndMissingSamePath_PlanningPatch extends TestHiddenAndMissingSamePath
// to the #40 cases of PATCH /issues: a body that names the issue's current
// hidden project or milestone and the same body with a made-up id give the
// same status, body and headers, and run the same statements (a pgx tracer).
func TestHiddenAndMissingSamePath_PlanningPatch(t *testing.T) {
	log := &queryLog{}
	e := newEnvWith(t, envOpts{tracer: log})
	const o = "/api/v1/orgs/orbit"
	e.must(http.StatusCreated, e.do("POST", o+"/teams", e.chen, map[string]any{"key": "HID", "name": "Hidden"}))
	hp := e.createProject(e.chen, map[string]any{"name": "Hidden plan", "teams": []string{"HID"}})
	vp := e.createProject(e.chen, map[string]any{"name": "Visible plan", "teams": []string{testdb.TeamKey}})
	var hm struct {
		ID uuid.UUID `json:"id"`
	}
	e.must(http.StatusCreated, e.do("POST", o+"/milestones", e.chen, map[string]any{"projectId": hp.ID, "name": "Hidden step"})).json(t, &hm)
	is := e.createIssue(e.chen, map[string]any{"team": testdb.TeamKey, "title": "orb", "projectId": hp.ID, "milestoneId": hm.ID})
	orb, err := e.q.GetTeamByKeySystem(context.Background(), dbq.GetTeamByKeySystemParams{OrgID: e.orgID, Key: testdb.TeamKey})
	if err != nil {
		t.Fatal(err)
	}
	rid := testdb.AddMember(t, e.pool, e.orgID, "rita", "member")
	testdb.MakeRestricted(t, e.pool, e.orgID, rid, orb.ID)
	tok, _ := e.newToken(rid, tokenOpts{})
	e.do("GET", o+"/teams", tok, nil) // warm up (last_used_at)
	path := o + "/issues/" + is.Identifier
	h, m, x := hp.ID.String(), hm.ID.String(), uuid.NewString()
	cases := []struct {
		name            string
		hidden, missing map[string]any
		ref             string // the hidden id, replaced by x in missing
	}{
		{"project", map[string]any{"projectId": h}, map[string]any{"projectId": x}, h},
		{"milestone", map[string]any{"milestoneId": m}, map[string]any{"milestoneId": x}, m},
		{"made-up project, hidden milestone", map[string]any{"projectId": x, "milestoneId": m}, map[string]any{"projectId": x, "milestoneId": x}, m},
		{"hidden project, made-up milestone", map[string]any{"projectId": h, "milestoneId": x}, map[string]any{"projectId": x, "milestoneId": x}, h},
		{"visible project, hidden milestone", map[string]any{"projectId": vp.ID, "milestoneId": m}, map[string]any{"projectId": vp.ID, "milestoneId": x}, m},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, gq := log.run(func() resp { return e.do("PATCH", path, tok, c.hidden) })
			want, wq := log.run(func() resp { return e.do("PATCH", path, tok, c.missing) })
			if got.status != http.StatusBadRequest || want.status != got.status {
				t.Fatalf("hidden %d %s, missing %d %s", got.status, got.body, want.status, want.body)
			}
			gb := bytes.ReplaceAll(got.body, []byte(c.ref), []byte("<ref>"))
			wb := bytes.ReplaceAll(want.body, []byte(x), []byte("<ref>"))
			if !bytes.Equal(gb, wb) {
				t.Errorf("bodies differ:\nhidden  %s\nmissing %s", gb, wb)
			}
			for _, hd := range []string{"Content-Type", "Location", "Cache-Control"} {
				if got.header.Get(hd) != want.header.Get(hd) {
					t.Errorf("header %s: %q vs %q", hd, got.header.Get(hd), want.header.Get(hd))
				}
			}
			if len(gq) == 0 {
				t.Fatal("the tracer saw no statement")
			}
			if strings.Join(gq, "\n;\n") != strings.Join(wq, "\n;\n") {
				t.Errorf("statements differ:\nhidden  %s\nmissing %s", short(gq), short(wq))
			}
		})
	}
	// Nothing moved: the stored project and milestone are the hidden ones.
	var p, ms uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT project_id, milestone_id FROM issues WHERE id = $1`, is.ID).Scan(&p, &ms); err != nil {
		t.Fatal(err)
	}
	if p != hp.ID || ms != hm.ID {
		t.Errorf("stored project %s milestone %s, want %s and %s", p, ms, hp.ID, hm.ID)
	}
}

// TestPlanningAccess_MilestoneClearActivity: the activity row that the
// implicit milestone clear writes (a visible project replacing a hidden one,
// #40) is redacted at read time for the restricted member (from null) and
// whole for the admin (#43).
func TestPlanningAccess_MilestoneClearActivity(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	e.must(http.StatusOK, e.do("PATCH", f.orgPath("/issues/"+f.child), f.restricted, map[string]any{"projectId": f.plan.shared}))
	rowBy := func(tok string) tracker.Activity {
		t.Helper()
		var page struct {
			Items []tracker.Activity `json:"items"`
		}
		e.must(http.StatusOK, e.do("GET", f.orgPath("/issues/"+f.child+"/activity?limit=200"), tok, nil)).json(t, &page)
		var found []tracker.Activity
		for _, a := range page.Items {
			if a.Kind == tracker.ActMilestone && a.Actor != nil && a.Actor.Username == "rita" {
				found = append(found, a)
			}
		}
		if len(found) != 1 {
			t.Fatalf("want one milestone row by rita, got %d: %+v", len(found), page.Items)
		}
		return found[0]
	}
	if a := rowBy(f.restricted); string(a.From) != "null" || string(a.To) != "null" {
		t.Errorf("restricted reads the clear as from %s to %s, want null to null", a.From, a.To)
	}
	if a := rowBy(e.chen); !strings.Contains(string(a.From), f.hidden["milestone"]) || string(a.To) != "null" {
		t.Errorf("admin reads the clear as from %s to %s, want the hidden milestone to null", a.From, a.To)
	}
}
