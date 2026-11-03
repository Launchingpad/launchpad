package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/dbq"
	"devops/internal/testdb"
	"devops/internal/tracker"
)

// Ticket 3 of decisions-T20 (LAUN-218): teams, states, the org's default team
// key, agent guidance, labels and the issue list for a restricted member. The
// fixture is Test B's (teamFixture): ORB visible, HID hidden.

// orgBody is the part of GET / and GET /orgs these tests read.
type orgBody struct {
	Slug        string  `json:"slug"`
	DefaultTeam *string `json:"defaultTeam"`
}

func (f *teamFixture) orgAs(t *testing.T, tok string) orgBody {
	t.Helper()
	var o orgBody
	f.e.must(http.StatusOK, f.e.do("GET", f.orgPath(""), tok, nil)).json(t, &o)
	return o
}

func (f *teamFixture) orgsAs(t *testing.T, tok string) []orgBody {
	t.Helper()
	var page struct {
		Items []orgBody `json:"items"`
	}
	f.e.must(http.StatusOK, f.e.do("GET", "/api/v1/orgs", tok, nil)).json(t, &page)
	return page.Items
}

// TestTeamAccess_DefaultTeamKey: GET / and GET /orgs name the default team
// only to a caller who sees it (#9). Admins and all-teams members are as before.
func TestTeamAccess_DefaultTeamKey(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	if o := f.orgAs(t, f.restricted); o.DefaultTeam != nil {
		t.Errorf("GET / as restricted: defaultTeam = %q, want null", *o.DefaultTeam)
	}
	if os := f.orgsAs(t, f.restricted); len(os) != 1 || os[0].DefaultTeam != nil {
		t.Errorf("GET /orgs as restricted: %+v, want one org with defaultTeam null", os)
	}
	for who, tok := range map[string]string{"admin": e.chen, "member": f.member} {
		if o := f.orgAs(t, tok); o.DefaultTeam == nil || *o.DefaultTeam != "HID" {
			t.Errorf("GET / as %s: defaultTeam = %v, want HID", who, o.DefaultTeam)
		}
		if os := f.orgsAs(t, tok); len(os) != 1 || os[0].DefaultTeam == nil || *os[0].DefaultTeam != "HID" {
			t.Errorf("GET /orgs as %s: %+v, want defaultTeam HID", who, os)
		}
	}
	// A restricted member who is in the default team sees its key.
	hid := f.teamID(t, "HID")
	sid := testdb.AddMember(t, e.pool, e.orgID, "sara", "member")
	testdb.MakeRestricted(t, e.pool, e.orgID, sid, hid)
	sara, _ := e.newToken(sid, tokenOpts{})
	if o := f.orgAs(t, sara); o.DefaultTeam == nil || *o.DefaultTeam != "HID" {
		t.Errorf("GET / as a member of HID: defaultTeam = %v, want HID", o.DefaultTeam)
	}
	if os := f.orgsAs(t, sara); len(os) != 1 || os[0].DefaultTeam == nil || *os[0].DefaultTeam != "HID" {
		t.Errorf("GET /orgs as a member of HID: %+v", os)
	}
	// GET /orgs reads each membership's own access: in another org where the
	// restricted user is an all-teams member, that org's key is named.
	other := e.newOrg("second", "SEC")
	ruser := f.userID(t, "rita")
	testdb.MustExec(t, e.pool, `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, other, ruser)
	testdb.MustExec(t, e.pool, `UPDATE orgs SET default_team_id = (SELECT id FROM teams WHERE org_id = $1) WHERE id = $1`, other)
	got := map[string]*string{}
	for _, o := range f.orgsAs(t, f.restricted) {
		got[o.Slug] = o.DefaultTeam
	}
	if got["orbit"] != nil || got["second"] == nil || *got["second"] != "SEC" {
		t.Errorf("GET /orgs per org: orbit %v (want null), second %v (want SEC)", got["orbit"], got["second"])
	}
}

func (f *teamFixture) teamID(t *testing.T, key string) uuid.UUID {
	t.Helper()
	tm, err := f.e.q.GetTeamByKeySystem(context.Background(), dbq.GetTeamByKeySystemParams{OrgID: f.e.orgID, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	return tm.ID
}

func (f *teamFixture) userID(t *testing.T, username string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.e.pool.QueryRow(context.Background(), `SELECT id FROM users WHERE username = $1`, username).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// issueAs reads one issue (GET /issues/{issue} shares the list's select).
func (f *teamFixture) issueAs(t *testing.T, tok, ref string) tracker.Issue {
	t.Helper()
	var is tracker.Issue
	f.e.must(http.StatusOK, f.e.do("GET", f.orgPath("/issues/"+ref), tok, nil)).json(t, &is)
	return is
}

// listAs reads GET /issues with a query string and returns the identifiers.
func (f *teamFixture) listAs(t *testing.T, tok, query string) ([]string, *string) {
	t.Helper()
	var page struct {
		Items      []tracker.Issue `json:"items"`
		NextCursor *string         `json:"nextCursor"`
	}
	f.e.must(http.StatusOK, f.e.do("GET", f.orgPath("/issues?"+query), tok, nil)).json(t, &page)
	out := make([]string, 0, len(page.Items))
	for _, is := range page.Items {
		out = append(out, is.Identifier)
	}
	return out, page.NextCursor
}

// TestTeamAccess_IssueRedaction: an issue whose parent, project and milestone
// are hidden is read with parent, projectId and milestoneId null by the
// restricted member (#40, #42), in the list and by id; the stored row is
// unchanged and the admin and the all-teams member read it whole.
func TestTeamAccess_IssueRedaction(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	before := f.issueRow(f.child)()
	is := f.issueAs(t, f.restricted, f.child)
	if is.Parent != nil || is.ProjectID != nil || is.MilestoneID != nil {
		t.Errorf("restricted reads parent %+v, projectId %v, milestoneId %v; want all null", is.Parent, is.ProjectID, is.MilestoneID)
	}
	var page struct {
		Items []tracker.Issue `json:"items"`
	}
	e.must(http.StatusOK, e.do("GET", f.orgPath("/issues?limit=200&full=true"), f.restricted, nil)).json(t, &page)
	found := false
	for _, it := range page.Items {
		if it.Identifier == f.child {
			found = true
			if it.Parent != nil || it.ProjectID != nil || it.MilestoneID != nil {
				t.Errorf("list: restricted reads parent %+v, projectId %v, milestoneId %v", it.Parent, it.ProjectID, it.MilestoneID)
			}
		}
	}
	if !found {
		t.Fatalf("the restricted member does not list the visible child %s", f.child)
	}
	for who, tok := range map[string]string{"admin": e.chen, "member": f.member} {
		is := f.issueAs(t, tok, f.child)
		if is.Parent == nil || is.Parent.Identifier != f.hidden["issue"] || is.ProjectID == nil || is.ProjectID.String() != f.hidden["project"] ||
			is.MilestoneID == nil || is.MilestoneID.String() != f.hidden["milestone"] {
			t.Errorf("%s reads parent %+v, projectId %v, milestoneId %v; want the stored values", who, is.Parent, is.ProjectID, is.MilestoneID)
		}
	}
	if after := f.issueRow(f.child)(); after != before {
		t.Errorf("a read changed the row:\nbefore %s\nafter  %s", before, after)
	}
	// A visible parent, project and milestone stay for the restricted member.
	var proj, ms struct {
		ID uuid.UUID `json:"id"`
	}
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/projects"), e.chen, map[string]any{"name": "Shared plan", "teams": []string{"ORB", "HID"}})).json(t, &proj)
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/milestones"), e.chen, map[string]any{"projectId": proj.ID, "name": "Shared step"})).json(t, &ms)
	e.must(http.StatusOK, e.do("PATCH", f.orgPath("/issues/"+f.child), e.chen,
		map[string]any{"parent": f.visible, "projectId": proj.ID, "milestoneId": ms.ID}))
	is = f.issueAs(t, f.restricted, f.child)
	if is.Parent == nil || is.Parent.Identifier != f.visible || is.ProjectID == nil || *is.ProjectID != proj.ID ||
		is.MilestoneID == nil || *is.MilestoneID != ms.ID {
		t.Errorf("visible refs: parent %+v, projectId %v, milestoneId %v", is.Parent, is.ProjectID, is.MilestoneID)
	}
}

// TestTeamAccess_ProjectFilter: ?project= by id or by name matches a visible
// project only (#40); a hidden one is the empty list of an unknown one, so
// the name cannot be probed. All-teams members and admins are as before.
func TestTeamAccess_ProjectFilter(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	for _, ref := range []string{f.hidden["project"], "Hidden plan", "hidden PLAN"} {
		q := "project=" + url.QueryEscape(ref)
		got, _ := f.listAs(t, f.restricted, q)
		if len(got) != 0 {
			t.Errorf("restricted ?%s = %v, want empty", q, got)
		}
		for who, tok := range map[string]string{"admin": e.chen, "member": f.member} {
			got, _ := f.listAs(t, tok, q)
			if !strings.Contains(strings.Join(got, ","), f.child) {
				t.Errorf("%s ?%s = %v, want %s in it", who, q, got, f.child)
			}
		}
	}
	// A project with no team is org-level: its filter works for everyone.
	var open struct {
		ID uuid.UUID `json:"id"`
	}
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/projects"), e.chen, map[string]any{"name": "Open plan"})).json(t, &open)
	e.must(http.StatusOK, e.do("PATCH", f.orgPath("/issues/"+f.visible), e.chen, map[string]any{"projectId": open.ID}))
	for _, ref := range []string{open.ID.String(), "Open plan"} {
		got, _ := f.listAs(t, f.restricted, "project="+url.QueryEscape(ref))
		if len(got) != 1 || got[0] != f.visible {
			t.Errorf("restricted ?project=%s = %v, want [%s]", ref, got, f.visible)
		}
	}
	// project=none is unchanged.
	if got, _ := f.listAs(t, f.restricted, "project=none&limit=200"); strings.Contains(strings.Join(got, ","), f.visible) {
		t.Errorf("project=none holds %s, which has a project: %v", f.visible, got)
	}
}

// TestTeamAccess_FullPages: the scope is in the WHERE, before LIMIT (#24): a
// page is full when that many visible rows exist, and nextCursor says "more"
// only for visible rows.
func TestTeamAccess_FullPages(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	// Interleave hidden and visible issues in the updatedAt order.
	for i := range 6 {
		e.createIssue(e.chen, map[string]any{"team": "HID", "title": fmt.Sprintf("noise %d", i)})
		e.createIssue(e.chen, map[string]any{"team": "ORB", "title": fmt.Sprintf("page row %d", i)})
	}
	all, next := f.listAs(t, f.restricted, "limit=250")
	if next != nil {
		t.Fatalf("one page of 250 has a next cursor")
	}
	n := len(all)
	for _, id := range all {
		if strings.HasPrefix(id, "HID-") {
			t.Fatalf("the restricted list holds %s", id)
		}
	}
	// A page of exactly n is full and is the last page.
	got, next := f.listAs(t, f.restricted, fmt.Sprintf("limit=%d", n))
	if len(got) != n || next != nil {
		t.Errorf("limit=%d: %d rows, next %v; want %d rows and no next", n, len(got), next, n)
	}
	// A page of n-1 is full and has a next page of one row.
	got, next = f.listAs(t, f.restricted, fmt.Sprintf("limit=%d", n-1))
	if len(got) != n-1 || next == nil {
		t.Fatalf("limit=%d: %d rows, next %v; want a full page and a cursor", n-1, len(got), next)
	}
	rest, next2 := f.listAs(t, f.restricted, fmt.Sprintf("limit=%d&cursor=%s", n-1, url.QueryEscape(*next)))
	if len(rest) != 1 || next2 != nil {
		t.Errorf("page 2: %v, next %v; want one row and no next", rest, next2)
	}
	// The admin and the all-teams member list every row, as before.
	for who, tok := range map[string]string{"admin": e.chen, "member": f.member} {
		rows, _ := f.listAs(t, tok, "limit=250")
		if len(rows) <= n {
			t.Errorf("%s lists %d rows, want more than the restricted %d", who, len(rows), n)
		}
	}
}

// TestTeamAccess_ParentCycleText: the cycle walk of a re-parent may pass
// through hidden issues; a restricted caller is told "unknown parent", the
// text of a parent that does not exist (#23). An admin keeps the plain text.
func TestTeamAccess_ParentCycleText(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	a := e.createIssue(e.chen, map[string]any{"team": "ORB", "title": "top"}).Identifier
	h := e.createIssue(e.chen, map[string]any{"team": "HID", "title": "middle"}).Identifier
	b := e.createIssue(e.chen, map[string]any{"team": "ORB", "title": "bottom"}).Identifier
	// a -> h -> b (a is the parent of h, h of b).
	e.must(http.StatusOK, e.do("PATCH", f.orgPath("/issues/"+h), e.chen, map[string]any{"parent": a}))
	e.must(http.StatusOK, e.do("PATCH", f.orgPath("/issues/"+b), e.chen, map[string]any{"parent": h}))
	// Making b the parent of a closes the loop through the hidden h.
	before := f.issueRow(a)()
	got := e.do("PATCH", f.orgPath("/issues/"+a), f.restricted, map[string]any{"parent": b})
	want := e.do("PATCH", f.orgPath("/issues/"+a), f.restricted, map[string]any{"parent": "ORB-999"})
	if got.status != http.StatusBadRequest || want.status != http.StatusBadRequest {
		t.Fatalf("restricted cycle: %d %s; unknown: %d %s; want 400 both", got.status, got.body, want.status, want.body)
	}
	gb := bytes.ReplaceAll(got.body, []byte(b), []byte("<ref>"))
	wb := bytes.ReplaceAll(want.body, []byte("ORB-999"), []byte("<ref>"))
	if !bytes.Equal(gb, wb) {
		t.Errorf("the cycle answer differs from an unknown parent:\ncycle   %s\nunknown %s", gb, wb)
	}
	if after := f.issueRow(a)(); after != before {
		t.Errorf("a refused PATCH changed the row")
	}
	res := e.do("PATCH", f.orgPath("/issues/"+a), e.chen, map[string]any{"parent": b})
	if res.status != http.StatusBadRequest || !strings.Contains(string(res.body), "that parent would make a cycle") {
		t.Errorf("admin cycle: %d %s, want the plain cycle text", res.status, res.body)
	}
}

// TestTeamAccess_AssigneeWithoutAccess: any member may be assigned (#46); an
// assignee who cannot see the team is accepted, and the person who assigns is
// not told.
func TestTeamAccess_AssigneeWithoutAccess(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	hid := f.teamID(t, "HID")
	uid := testdb.AddMember(t, e.pool, e.orgID, "hilda", "member")
	testdb.MakeRestricted(t, e.pool, e.orgID, uid, hid)
	var is tracker.Issue
	e.must(http.StatusOK, e.do("PATCH", f.orgPath("/issues/"+f.visible), f.restricted, map[string]any{"assignee": "hilda"})).json(t, &is)
	if is.Assignee == nil || is.Assignee.Username != "hilda" {
		t.Fatalf("assignee = %+v, want hilda", is.Assignee)
	}
	// hilda cannot read the issue she was given.
	tok, _ := e.newToken(uid, tokenOpts{})
	if res := e.do("GET", f.orgPath("/issues/"+f.visible), tok, nil); res.status != http.StatusNotFound {
		t.Errorf("hilda reads the ORB issue: %d", res.status)
	}
}

// TestTeamAccess_TeamsGuidanceLabels: GET /teams lists visible teams; agent
// guidance and labels leave the hidden team's rows out; the all-teams member
// and the admin read every row.
func TestTeamAccess_TeamsGuidanceLabels(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	e.must(http.StatusOK, e.do("PUT", f.orgPath("/agent-guidance"), e.chen, map[string]any{"text": "org guidance"}))
	keys := func(tok string) []string {
		var page struct {
			Items []tracker.Team `json:"items"`
		}
		e.must(http.StatusOK, e.do("GET", f.orgPath("/teams"), tok, nil)).json(t, &page)
		var out []string
		for _, tm := range page.Items {
			out = append(out, tm.Key)
		}
		return out
	}
	if got := strings.Join(keys(f.restricted), ","); got != "ORB" {
		t.Errorf("restricted GET /teams = %s, want ORB", got)
	}
	for who, tok := range map[string]string{"admin": e.chen, "member": f.member} {
		if got := strings.Join(keys(tok), ","); got != "HID,ORB" {
			t.Errorf("%s GET /teams = %s, want HID,ORB", who, got)
		}
	}
	guidance := func(tok, q string) string {
		return string(e.must(http.StatusOK, e.do("GET", f.orgPath("/agent-guidance"+q), tok, nil)).body)
	}
	if g := guidance(f.restricted, ""); !strings.Contains(g, "org guidance") || strings.Contains(g, "hidden guidance") {
		t.Errorf("restricted guidance: %s", g)
	}
	if g := guidance(f.restricted, "?team=ORB"); !strings.Contains(g, "org guidance") || strings.Contains(g, "hidden guidance") {
		t.Errorf("restricted guidance ?team=ORB: %s", g)
	}
	for who, tok := range map[string]string{"admin": e.chen, "member": f.member} {
		if g := guidance(tok, ""); !strings.Contains(g, "org guidance") || !strings.Contains(g, "hidden guidance") {
			t.Errorf("%s guidance: %s", who, g)
		}
	}
	// A visible team label: the restricted member makes one in ORB.
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/labels"), f.restricted, map[string]any{"name": "orb-only", "team": "ORB"}))
	labels := string(e.must(http.StatusOK, e.do("GET", f.orgPath("/labels"), f.restricted, nil)).body)
	if !strings.Contains(labels, "orb-only") || strings.Contains(labels, "hidden-label") {
		t.Errorf("restricted labels: %s", labels)
	}
}

// TestTeamAccess_LinearIDs: a Linear id of a hidden team, issue or cycle
// (import_mappings) is not a back door (#14, #67): each answers as a made-up
// Linear id. The admin resolves them.
func TestTeamAccess_LinearIDs(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	ctx := context.Background()
	var issueID uuid.UUID
	if err := e.pool.QueryRow(ctx, `SELECT i.id FROM issues i JOIN teams t ON t.org_id = i.org_id AND t.id = i.team_id
		WHERE i.org_id = $1 AND t.key || '-' || i.number = $2`, e.orgID, f.hidden["issue"]).Scan(&issueID); err != nil {
		t.Fatal(err)
	}
	lin := map[string]string{"team": uuid.NewString(), "issue": uuid.NewString(), "cycle": uuid.NewString()}
	for kind, target := range map[string]string{"team": f.teamID(t, "HID").String(), "issue": issueID.String(), "cycle": f.hidden["cycle"]} {
		testdb.MustExec(t, e.pool, `INSERT INTO import_mappings (org_id, entity_type, source_id, target_id) VALUES ($1, $2, $3, $4)`,
			e.orgID, kind, lin[kind], target)
	}
	same := func(name, hiddenPath, madePath, hiddenRef, madeRef string) {
		t.Helper()
		got := e.do("GET", hiddenPath, f.restricted, nil)
		want := e.do("GET", madePath, f.restricted, nil)
		gb := bytes.ReplaceAll(got.body, []byte(hiddenRef), []byte("<ref>"))
		wb := bytes.ReplaceAll(want.body, []byte(madeRef), []byte("<ref>"))
		if got.status != want.status || !bytes.Equal(gb, wb) {
			t.Errorf("%s: hidden %d %s; made-up %d %s", name, got.status, gb, want.status, wb)
		}
	}
	made := uuid.NewString()
	same("GET /teams/{linear}", f.orgPath("/teams/"+lin["team"]), f.orgPath("/teams/"+made), lin["team"], made)
	same("GET /teams/{linear}/states", f.orgPath("/teams/"+lin["team"]+"/states"), f.orgPath("/teams/"+made+"/states"), lin["team"], made)
	same("?team=<linear>", f.orgPath("/issues?team="+lin["team"]), f.orgPath("/issues?team="+made), lin["team"], made)
	same("?cycle=<linear>", f.orgPath("/issues?cycle="+lin["cycle"]), f.orgPath("/issues?cycle="+made), lin["cycle"], made)
	same("?parent=<linear>", f.orgPath("/issues?parent="+lin["issue"]), f.orgPath("/issues?parent="+made), lin["issue"], made)
	if res := e.do("GET", f.orgPath("/issues?team="+lin["team"]), f.restricted, nil); res.status != http.StatusOK || bytes.Contains(res.body, []byte("HID")) {
		t.Errorf("?team=<linear> as restricted: %d %s", res.status, res.body)
	}
	// The admin reads them.
	e.must(http.StatusOK, e.do("GET", f.orgPath("/teams/"+lin["team"]), e.chen, nil))
	for _, q := range []string{"team=" + lin["team"], "cycle=" + lin["cycle"], "parent=" + lin["issue"]} {
		e.must(http.StatusOK, e.do("GET", f.orgPath("/issues?"+q), e.chen, nil))
	}
	if got, _ := f.listAs(t, e.chen, "team="+lin["team"]+"&limit=200"); !strings.Contains(strings.Join(got, ","), f.hidden["issue"]) {
		t.Errorf("admin ?team=<linear> = %v, want %s in it", got, f.hidden["issue"])
	}
}

// TestTeamAccess_CrossOrg: a reference to another org's team, project or
// label in the routes of ticket 3 answers as an unknown one.
func TestTeamAccess_CrossOrg(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	var far struct {
		ID uuid.UUID `json:"id"`
	}
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/elsewhere/projects", f.outsider, map[string]any{"name": "Far plan"})).json(t, &far)
	ptID := far.ID
	same := func(name, method, p1, p2 string, b1, b2 any, r1, r2 string) {
		t.Helper()
		got := e.do(method, f.orgPath(p1), e.chen, b1)
		want := e.do(method, f.orgPath(p2), e.chen, b2)
		gb := bytes.ReplaceAll(got.body, []byte(r1), []byte("<ref>"))
		wb := bytes.ReplaceAll(want.body, []byte(r2), []byte("<ref>"))
		if got.status != want.status || !bytes.Equal(gb, wb) {
			t.Errorf("%s: other org %d %s; unknown %d %s", name, got.status, gb, want.status, wb)
		}
	}
	made := uuid.NewString()
	same("?project=<id>", "GET", "/issues?project="+ptID.String(), "/issues?project="+made, nil, nil, ptID.String(), made)
	same("?project=<name>", "GET", "/issues?project=Far%20plan", "/issues?project=No%20plan", nil, nil, "Far%20plan", "No%20plan")
	same("?team=ELS", "GET", "/issues?team=ELS", "/issues?team=ZZZ", nil, nil, "ELS", "ZZZ")
	same("/teams/ELS/states", "GET", "/teams/ELS/states", "/teams/ZZZ/states", nil, nil, "ELS", "ZZZ")
	same("/agent-guidance?team=ELS", "GET", "/agent-guidance?team=ELS", "/agent-guidance?team=ZZZ", nil, nil, "ELS", "ZZZ")
	same("/labels?team=ELS", "GET", "/labels?team=ELS", "/labels?team=ZZZ", nil, nil, "ELS", "ZZZ")
	same("POST /labels team ELS", "POST", "/labels", "/labels", map[string]any{"name": "x", "team": "ELS"}, map[string]any{"name": "x", "team": "ZZZ"}, "ELS", "ZZZ")
}

// queryLog records the SQL of every statement while on.
type queryLog struct {
	on  atomic.Bool
	mu  sync.Mutex
	sql []string
}

func (l *queryLog) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if l.on.Load() {
		l.mu.Lock()
		l.sql = append(l.sql, d.SQL)
		l.mu.Unlock()
	}
	return ctx
}

func (l *queryLog) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// run records the statements of one request.
func (l *queryLog) run(fn func() resp) (resp, []string) {
	l.mu.Lock()
	l.sql = nil
	l.mu.Unlock()
	l.on.Store(true)
	r := fn()
	l.on.Store(false)
	l.mu.Lock()
	defer l.mu.Unlock()
	return r, append([]string(nil), l.sql...)
}

// TestHiddenAndMissingSamePath (ticket 3 AC): a hidden issue and a missing one
// give the same status, body and headers, and run the same statements (the
// predicate is part of the lookup, #11). Each id form: identifier, issue id,
// Linear id; and the issue list's ?parent= filter.
func TestHiddenAndMissingSamePath(t *testing.T) {
	log := &queryLog{}
	e := newEnvWith(t, envOpts{tracer: log})
	ctx := context.Background()
	var hid tracker.Team
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/orbit/teams", e.chen, map[string]any{"key": "HID", "name": "Hidden"})).json(t, &hid)
	hidden := e.createIssue(e.chen, map[string]any{"team": "HID", "title": "hidden issue"})
	linear := uuid.NewString()
	testdb.MustExec(t, e.pool, `INSERT INTO import_mappings (org_id, entity_type, source_id, target_id) VALUES ($1, 'issue', $2, $3)`,
		e.orgID, linear, hidden.ID)
	orb, err := e.q.GetTeamByKeySystem(ctx, dbq.GetTeamByKeySystemParams{OrgID: e.orgID, Key: testdb.TeamKey})
	if err != nil {
		t.Fatal(err)
	}
	rid := testdb.AddMember(t, e.pool, e.orgID, "rita", "member")
	testdb.MakeRestricted(t, e.pool, e.orgID, rid, orb.ID)
	tok, _ := e.newToken(rid, tokenOpts{})
	// Ticket 4: the hidden issue's children, and an edge from a visible
	// issue to the hidden one.
	var com, rel struct {
		ID uuid.UUID `json:"id"`
	}
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/orbit/issues/"+hidden.Identifier+"/comments", e.chen, map[string]any{"body": "hidden"})).json(t, &com)
	key, fname, ctype, size := "org/"+e.orgID.String()+"/2026/"+uuid.NewString()+"/h.txt", "h.txt", "text/plain", int64(1)
	att, err := e.q.InsertAttachment(ctx, dbq.InsertAttachmentParams{OrgID: e.orgID, IssueID: hidden.ID, Kind: "file", Title: "h",
		ObjectKey: &key, Filename: &fname, ContentType: &ctype, SizeBytes: &size})
	if err != nil {
		t.Fatal(err)
	}
	visible := e.createIssue(e.chen, map[string]any{"team": testdb.TeamKey, "title": "visible"}).Identifier
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/orbit/issues/"+visible+"/relations", e.chen,
		map[string]any{"type": "related", "issue": hidden.Identifier})).json(t, &rel)
	// Warm up: the first request of a token may differ (last_used_at).
	e.do("GET", "/api/v1/orgs/orbit/teams", tok, nil)
	made := uuid.NewString()
	cases := []struct {
		name, method, hidden, missing string
		// hr and mr are the echoed references; empty: the last path segment.
		hr, mr string
		hb, mb any
	}{
		{name: "identifier", hidden: "/issues/" + hidden.Identifier, missing: "/issues/HID-999"},
		{name: "issue id", hidden: "/issues/" + hidden.ID.String(), missing: "/issues/" + uuid.NewString()},
		{name: "linear id", hidden: "/issues/" + linear, missing: "/issues/" + uuid.NewString()},
		{name: "parent filter", hidden: "/issues?parent=" + hidden.Identifier, missing: "/issues?parent=HID-999"},
		{name: "include", hidden: "/issues/" + hidden.Identifier + "?include=relations,attachments,stateHistory,comments",
			missing: "/issues/HID-999?include=relations,attachments,stateHistory,comments", hr: hidden.Identifier, mr: "HID-999"},
		{name: "activity", hidden: "/issues/" + hidden.Identifier + "/activity", missing: "/issues/HID-999/activity", hr: hidden.Identifier, mr: "HID-999"},
		{name: "comments", hidden: "/issues/" + hidden.Identifier + "/comments", missing: "/issues/HID-999/comments", hr: hidden.Identifier, mr: "HID-999"},
		{name: "attachments", hidden: "/issues/" + hidden.Identifier + "/attachments", missing: "/issues/HID-999/attachments", hr: hidden.Identifier, mr: "HID-999"},
		{name: "relations", hidden: "/issues/" + hidden.Identifier + "/relations", missing: "/issues/HID-999/relations", hr: hidden.Identifier, mr: "HID-999"},
		{name: "attachment", hidden: "/attachments/" + att.ID.String(), missing: "/attachments/" + made},
		{name: "download", hidden: "/attachments/" + att.ID.String() + "/download", missing: "/attachments/" + made + "/download", hr: att.ID.String(), mr: made},
		{name: "comment patch", method: "PATCH", hidden: "/comments/" + com.ID.String(), missing: "/comments/" + made,
			hb: map[string]any{"body": "x"}, mb: map[string]any{"body": "x"}},
		{name: "comment delete", method: "DELETE", hidden: "/comments/" + com.ID.String(), missing: "/comments/" + made},
		{name: "reply parentId", method: "POST", hidden: "/issues/" + visible + "/comments", missing: "/issues/" + visible + "/comments",
			hr: com.ID.String(), mr: made, hb: map[string]any{"body": "x", "parentId": com.ID}, mb: map[string]any{"body": "x", "parentId": made}},
		{name: "relation delete", method: "DELETE", hidden: "/issues/" + visible + "/relations/" + rel.ID.String(), missing: "/issues/" + visible + "/relations/" + made},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			method := c.method
			if method == "" {
				method = "GET"
			}
			hp, mp := "/api/v1/orgs/orbit"+c.hidden, "/api/v1/orgs/orbit"+c.missing
			got, gq := log.run(func() resp { return e.do(method, hp, tok, c.hb) })
			want, wq := log.run(func() resp { return e.do(method, mp, tok, c.mb) })
			if got.status != want.status || (got.status != http.StatusNotFound && got.status != http.StatusBadRequest) {
				t.Fatalf("hidden %d %s, missing %d %s", got.status, got.body, want.status, want.body)
			}
			hr, mr := c.hr, c.mr
			if hr == "" {
				hr = c.hidden[strings.LastIndexAny(c.hidden, "/=")+1:]
				mr = c.missing[strings.LastIndexAny(c.missing, "/=")+1:]
			}
			gb := bytes.ReplaceAll(got.body, []byte(hr), []byte("<ref>"))
			wb := bytes.ReplaceAll(want.body, []byte(mr), []byte("<ref>"))
			if !bytes.Equal(gb, wb) {
				t.Errorf("bodies differ:\nhidden  %s\nmissing %s", gb, wb)
			}
			for _, h := range []string{"Content-Type", "Location", "Cache-Control"} {
				if got.header.Get(h) != want.header.Get(h) {
					t.Errorf("header %s: %q vs %q", h, got.header.Get(h), want.header.Get(h))
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
	// The refused writes left the hidden rows: the comment and the edge.
	for _, q := range []string{`SELECT count(*) FROM comments WHERE org_id = $1 AND id = $2`, `SELECT count(*) FROM issue_relations WHERE org_id = $1 AND id = $2`} {
		id := com.ID
		if strings.Contains(q, "issue_relations") {
			id = rel.ID
		}
		var n int
		if err := e.pool.QueryRow(ctx, q, e.orgID, id).Scan(&n); err != nil || n != 1 {
			t.Errorf("%s: %d rows, %v; want 1", q, n, err)
		}
	}
	// The admin reads the hidden issue: the test's ids are real.
	e.must(http.StatusOK, e.do("GET", "/api/v1/orgs/orbit/issues/"+linear, e.chen, nil))
}

// short is the first line of each statement, for a failure message.
func short(sql []string) string {
	out := make([]string, len(sql))
	for i, s := range sql {
		s = strings.TrimSpace(s)
		if j := strings.IndexByte(s, '\n'); j > 0 {
			s = s[:j]
		}
		out[i] = s
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// TestTeamAccess_ProjectNone: project=none holds an issue whose project the
// caller cannot see, in agreement with its redacted projectId (#40). For the
// all-teams member and the admin it is in a project, as before.
func TestTeamAccess_ProjectNone(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	got, _ := f.listAs(t, f.restricted, "project=none&limit=250")
	if !strings.Contains(","+strings.Join(got, ",")+",", ","+f.child+",") {
		t.Errorf("restricted project=none = %v, want %s in it", got, f.child)
	}
	if is := f.issueAs(t, f.restricted, f.child); is.ProjectID != nil {
		t.Errorf("restricted reads projectId %v, want null (the filter and the field must agree)", is.ProjectID)
	}
	for who, tok := range map[string]string{"admin": e.chen, "member": f.member} {
		got, _ := f.listAs(t, tok, "project=none&limit=250")
		if strings.Contains(","+strings.Join(got, ",")+",", ","+f.child+",") {
			t.Errorf("%s project=none = %v, holds %s, which is in a project", who, got, f.child)
		}
	}
	// A visible project keeps its issue out of project=none.
	e.must(http.StatusOK, e.do("PATCH", f.orgPath("/issues/"+f.child), e.chen, map[string]any{"projectId": nil, "milestoneId": nil}))
	var open struct {
		ID uuid.UUID `json:"id"`
	}
	e.must(http.StatusCreated, e.do("POST", f.orgPath("/projects"), e.chen, map[string]any{"name": "Open plan"})).json(t, &open)
	e.must(http.StatusOK, e.do("PATCH", f.orgPath("/issues/"+f.child), e.chen, map[string]any{"projectId": open.ID}))
	if got, _ := f.listAs(t, f.restricted, "project=none&limit=250"); strings.Contains(","+strings.Join(got, ",")+",", ","+f.child+",") {
		t.Errorf("restricted project=none = %v holds %s, which is in a visible project", got, f.child)
	}
}

// TestTeamAccess_LabelCycleFilterDepth: the ?label= and ?cycle=<number>
// predicates are defence in depth. The API never puts a hidden team's label
// or cycle on a visible issue, so the test makes that row by hand: the filter
// still does not match it for the restricted member, and does for the admin.
func TestTeamAccess_LabelCycleFilterDepth(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	var issueID uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT i.id FROM issues i JOIN teams t ON t.org_id = i.org_id AND t.id = i.team_id
		WHERE i.org_id = $1 AND t.key || '-' || i.number = $2`, e.orgID, f.visible).Scan(&issueID); err != nil {
		t.Fatal(err)
	}
	testdb.MustExec(t, e.pool, `INSERT INTO issue_labels (org_id, issue_id, label_id) VALUES ($1, $2, $3)`, e.orgID, issueID, f.hiddenLabel)
	testdb.MustExec(t, e.pool, `UPDATE issues SET cycle_id = $3 WHERE org_id = $1 AND id = $2`, e.orgID, issueID, f.hidden["cycle"])
	for _, q := range []string{"label=hidden-label", "cycle=1"} {
		if got, _ := f.listAs(t, f.restricted, q); len(got) != 0 {
			t.Errorf("restricted ?%s = %v, want empty", q, got)
		}
		if got, _ := f.listAs(t, e.chen, q); !strings.Contains(strings.Join(got, ","), f.visible) {
			t.Errorf("admin ?%s = %v, want %s in it", q, got, f.visible)
		}
	}
}
