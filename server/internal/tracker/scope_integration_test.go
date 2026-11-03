package tracker_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/auth"
	"devops/internal/dbq"
	"devops/internal/httpx"
	"devops/internal/tracker"
)

// LAUN-216 (T20 #14, #15, #23, #36): every resolver and scoped read takes the
// org and its access. A member with access to team ORB only finds nothing of
// team BBB, in the words for a row that does not exist; an org admin and a
// member with all teams find everything. Nothing in the API can make a
// restricted member yet, so the test builds the org value by hand.
func TestScope_RestrictedAccess(t *testing.T) {
	e := newCycEnv(t)
	ctx := context.Background()
	tb, err := tracker.CreateTeamTx(ctx, e.q, e.org.ID, "BBB", "Team B", "")
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE teams SET cycles_enabled = true WHERE org_id = $1`, e.org.ID)
	e.run(at(e.loc, 2026, 10, 21, 12))

	mkIssue := func(team string) tracker.Issue {
		is, err := e.svc.CreateIssue(ctx, e.org, e.chen, tracker.CreateIssueInput{Team: team, Title: "issue in " + team})
		if err != nil {
			t.Fatal(err)
		}
		return is
	}
	ia, ib := mkIssue("ORB"), mkIssue("BBB")
	mkProject := func(name string, teams ...string) tracker.Project {
		p, err := e.svc.CreateProject(ctx, e.org, e.chen, tracker.CreateProjectInput{Name: name, Teams: teams})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	pa, pb, pshared, porg := mkProject("pa", "ORB"), mkProject("pb", "BBB"), mkProject("pshared", "ORB", "BBB"), mkProject("porg")
	mb, err := e.svc.CreateMilestone(ctx, e.org, e.chen, tracker.CreateMilestoneInput{ProjectID: &pb.ID, Name: "mb"})
	if err != nil {
		t.Fatal(err)
	}
	ma, err := e.svc.CreateMilestone(ctx, e.org, e.chen, tracker.CreateMilestoneInput{ProjectID: &pa.ID, Name: "ma"})
	if err != nil {
		t.Fatal(err)
	}
	mkDoc := func(title string, project *uuid.UUID) tracker.Document {
		d, err := e.svc.CreateDocument(ctx, e.org, e.chen, tracker.CreateDocumentInput{Title: title, Content: "x", ProjectID: project})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	da, db, dorg := mkDoc("da", &pa.ID), mkDoc("db", &pb.ID), mkDoc("dorg", nil)
	var cycA, cycB uuid.UUID
	if err := e.pool.QueryRow(ctx, `SELECT id FROM cycles WHERE team_id = $1 ORDER BY number LIMIT 1`, e.team.ID).Scan(&cycA); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT id FROM cycles WHERE team_id = $1 ORDER BY number LIMIT 1`, tb.ID).Scan(&cycB); err != nil {
		t.Fatal(err)
	}
	var stateB uuid.UUID
	if err := e.pool.QueryRow(ctx, `SELECT id FROM workflow_states WHERE team_id = $1 LIMIT 1`, tb.ID).Scan(&stateB); err != nil {
		t.Fatal(err)
	}

	member := func(access auth.TeamAccess) *auth.Org {
		o := *e.org
		o.Role, o.Access = "member", access
		return &o
	}
	restricted := member(auth.NewTeamAccess("member", "selected", []uuid.UUID{e.team.ID}))
	nobody := member(auth.NewTeamAccess("member", "selected", nil))
	allMember := member(auth.TeamAccess{All: true})

	status := func(err error) int {
		var api *httpx.Error
		if errors.As(err, &api) {
			return api.Status
		}
		if err == nil {
			return 0
		}
		t.Fatalf("not an API error: %v", err)
		return -1
	}
	want := func(name string, err error, code int) {
		t.Helper()
		if got := status(err); got != code {
			t.Fatalf("%s: status %d (%v), want %d", name, got, err, code)
		}
	}

	// Reads by reference: the restricted member finds team ORB and nothing of BBB.
	for _, tc := range []struct {
		name string
		org  *auth.Org
		hide int // the status for a hidden row; 0 means visible
	}{{"restricted", restricted, http.StatusNotFound}, {"no team", nobody, http.StatusNotFound}, {"all member", allMember, 0}, {"admin", e.org, 0}} {
		hidden := tc.hide
		visible := 0
		if tc.org == nobody {
			visible = http.StatusNotFound
		}
		_, err := e.svc.GetIssue(ctx, tc.org, e.chen, ia.Identifier, tracker.Includes{})
		want(tc.name+" issue A by identifier", err, visible)
		_, err = e.svc.GetIssue(ctx, tc.org, e.chen, ib.Identifier, tracker.Includes{})
		want(tc.name+" issue B by identifier", err, hidden)
		_, err = e.svc.GetIssue(ctx, tc.org, e.chen, ib.ID.String(), tracker.Includes{})
		want(tc.name+" issue B by id", err, hidden)
		_, err = e.svc.ResolveTeamID(ctx, tc.org, "BBB")
		want(tc.name+" team B by key", err, hidden)
		_, err = e.svc.ResolveTeamID(ctx, tc.org, tb.ID.String())
		want(tc.name+" team B by id", err, hidden)
		_, err = e.svc.GetCycle(ctx, tc.org, cycB)
		want(tc.name+" cycle B", err, hidden)
		_, err = e.svc.GetCycle(ctx, tc.org, cycA)
		want(tc.name+" cycle A", err, visible)
		_, err = e.svc.GetProject(ctx, tc.org, pb.ID)
		want(tc.name+" project B", err, hidden)
		_, err = e.svc.GetProject(ctx, tc.org, pshared.ID) // the any-of rule (#36)
		want(tc.name+" project shared", err, visible)
		_, err = e.svc.GetProject(ctx, tc.org, pa.ID)
		want(tc.name+" project A", err, visible)
		_, err = e.svc.GetProject(ctx, tc.org, porg.ID) // no team: org-level (#36)
		want(tc.name+" project without team", err, 0)
		_, err = e.svc.GetMilestone(ctx, tc.org, mb.ID)
		want(tc.name+" milestone of project B", err, hidden)
		_, err = e.svc.GetMilestone(ctx, tc.org, ma.ID)
		want(tc.name+" milestone of project A", err, visible)
		_, err = e.svc.GetDocument(ctx, tc.org, db.ID.String())
		want(tc.name+" document of project B", err, hidden)
		_, err = e.svc.GetDocument(ctx, tc.org, db.Slug)
		want(tc.name+" document of project B by slug", err, hidden)
		_, err = e.svc.GetDocument(ctx, tc.org, da.ID.String())
		want(tc.name+" document of project A", err, visible)
		_, err = e.svc.GetDocument(ctx, tc.org, dorg.ID.String()) // no project: org-level (#7)
		want(tc.name+" document without project", err, 0)
	}

	// A body reference to a hidden row is a 400 with the text of an unknown one.
	_, err = e.svc.CreateIssue(ctx, restricted, e.chen, tracker.CreateIssueInput{Team: "BBB", Title: "x"})
	want("create in hidden team", err, http.StatusBadRequest)
	_, err = e.svc.CreateIssue(ctx, restricted, e.chen, tracker.CreateIssueInput{Team: "ORB", Title: "x", State: stateB.String()})
	hiddenState := err
	want("hidden state", err, http.StatusBadRequest)
	_, err = e.svc.CreateIssue(ctx, restricted, e.chen, tracker.CreateIssueInput{Team: "ORB", Title: "x", State: uuid.NewString()})
	want("unknown state", err, http.StatusBadRequest)
	// The text of a state of a hidden team is the text of an unknown one (#23).
	if hiddenState.Error() != strings.Replace(err.Error(), err.Error()[strings.Index(err.Error(), `"`)+1:strings.LastIndex(err.Error(), `"`)], stateB.String(), 1) {
		t.Fatalf("hidden state: %q, unknown state: %q", hiddenState, err)
	}
	_, err = e.svc.CreateIssue(ctx, restricted, e.chen, tracker.CreateIssueInput{Team: "ORB", Title: "x", ProjectID: &pb.ID})
	want("hidden project", err, http.StatusBadRequest)
	_, err = e.svc.CreateIssue(ctx, restricted, e.chen, tracker.CreateIssueInput{Team: "ORB", Title: "x", CycleID: &cycB})
	want("hidden cycle", err, http.StatusBadRequest)
	_, err = e.svc.CreateIssue(ctx, restricted, e.chen, tracker.CreateIssueInput{Team: "ORB", Title: "x", Parent: ib.Identifier})
	want("hidden parent", err, http.StatusBadRequest)
	if ok, err := e.svc.CreateIssue(ctx, restricted, e.chen, tracker.CreateIssueInput{Team: "ORB", Title: "ok", ProjectID: &pshared.ID, CycleID: &cycA}); err != nil || ok.ID == uuid.Nil {
		t.Fatalf("visible refs: %v", err)
	}

	// Lists hold the visible rows only.
	lf, err := tracker.ParseListFilter(url.Values{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	issues, err := e.svc.ListIssues(ctx, restricted, e.chen, lf)
	if err != nil {
		t.Fatal(err)
	}
	for _, is := range issues.Items {
		if is.ID == ib.ID {
			t.Fatal("the issue list holds an issue of a hidden team")
		}
	}
	if len(issues.Items) != 2 { // ia and the one created above
		t.Fatalf("restricted issue list: %d items, want 2", len(issues.Items))
	}
	if all, _ := e.svc.ListIssues(ctx, e.org, e.chen, lf); len(all.Items) != 3 {
		t.Fatalf("admin issue list: %d items, want 3", len(all.Items))
	}
	if none, _ := e.svc.ListIssues(ctx, nobody, e.chen, lf); len(none.Items) != 0 {
		t.Fatalf("no-team issue list: %d items, want 0", len(none.Items))
	}
	pf, _ := tracker.ParseProjectFilter(url.Values{})
	projects, err := e.svc.ListProjects(ctx, restricted, e.chen, pf)
	if err != nil || len(projects.Items) != 3 {
		t.Fatalf("restricted project list: %d items, %v; want 3 (pa, pshared, porg)", len(projects.Items), err)
	}
	if np, _ := e.svc.ListProjects(ctx, nobody, e.chen, pf); len(np.Items) != 1 {
		t.Fatalf("no-team project list: %d items, want 1 (the org-level project)", len(np.Items))
	}
	if ap, _ := e.svc.ListProjects(ctx, e.org, e.chen, pf); len(ap.Items) != 4 {
		t.Fatalf("admin project list: %d items, want 4", len(ap.Items))
	}
	mf, _ := tracker.ParseMilestoneFilter(url.Values{})
	if ms, err := e.svc.ListMilestones(ctx, restricted, mf); err != nil || len(ms.Items) != 1 || ms.Items[0].ID != ma.ID {
		t.Fatalf("restricted milestone list: %+v %v", ms.Items, err)
	}
	df, _ := tracker.ParseDocumentFilter(url.Values{})
	if ds, err := e.svc.ListDocuments(ctx, restricted, df); err != nil || len(ds.Items) != 2 {
		t.Fatalf("restricted document list: %d items, %v; want 2 (da, dorg)", len(ds.Items), err)
	}
	cf, _ := tracker.ParseCycleFilter(url.Values{})
	cs, err := e.svc.ListCycles(ctx, restricted, cf)
	if err != nil || len(cs.Items) == 0 {
		t.Fatalf("restricted cycle list: %v", err)
	}
	for _, c := range cs.Items {
		if c.ID == cycB {
			t.Fatal("the cycle list holds a cycle of a hidden team")
		}
	}
	// A hidden id in a filter is the answer of an unknown one: the id of the
	// hidden team lists nothing.
	cf, _ = tracker.ParseCycleFilter(url.Values{"team": {tb.ID.String()}})
	if cs, err := e.svc.ListCycles(ctx, restricted, cf); err != nil || len(cs.Items) != 0 {
		t.Fatalf("cycles of a hidden team: %d, %v", len(cs.Items), err)
	}
}

// LAUN-216 review: writes, comments, imported ids, relations, project updates
// and avatars for a member with access to team ORB only. A hidden row is the
// 404 (or 400 in a body) of a row that does not exist, and a write changes
// nothing.
func TestScope_RestrictedWritesAndRefs(t *testing.T) {
	e := newCycEnv(t)
	ctx := context.Background()
	tb, err := tracker.CreateTeamTx(ctx, e.q, e.org.ID, "BBB", "Team B", "")
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE teams SET cycles_enabled = true WHERE org_id = $1`, e.org.ID)
	e.run(at(e.loc, 2026, 10, 21, 12))
	restricted := *e.org
	restricted.Role, restricted.Access = "member", auth.NewTeamAccess("member", "selected", []uuid.UUID{e.team.ID})
	r := &restricted

	code := func(err error) int {
		var api *httpx.Error
		if errors.As(err, &api) {
			return api.Status
		}
		if err == nil {
			return 0
		}
		t.Fatalf("not an API error: %v", err)
		return -1
	}
	want := func(name string, err error, status int) {
		t.Helper()
		if got := code(err); got != status {
			t.Fatalf("%s: status %d (%v), want %d", name, got, err, status)
		}
	}
	ia, err := e.svc.CreateIssue(ctx, e.org, e.chen, tracker.CreateIssueInput{Team: "ORB", Title: "a"})
	if err != nil {
		t.Fatal(err)
	}
	ib, err := e.svc.CreateIssue(ctx, e.org, e.chen, tracker.CreateIssueInput{Team: "BBB", Title: "b"})
	if err != nil {
		t.Fatal(err)
	}
	pb, err := e.svc.CreateProject(ctx, e.org, e.chen, tracker.CreateProjectInput{Name: "pb", Teams: []string{"BBB"}})
	if err != nil {
		t.Fatal(err)
	}
	db, err := e.svc.CreateDocument(ctx, e.org, e.chen, tracker.CreateDocumentInput{Title: "db", Content: "x", ProjectID: &pb.ID})
	if err != nil {
		t.Fatal(err)
	}

	// Comments: by id, on a document of a hidden project and on an issue of a hidden team.
	dc, err := e.svc.CreateDocumentComment(ctx, e.org, e.chen, db.ID.String(), tracker.CommentInput{Body: "doc comment"})
	if err != nil {
		t.Fatal(err)
	}
	ic, err := e.svc.CreateComment(ctx, e.org, e.chen, ib.Identifier, tracker.CommentInput{Body: "issue comment"})
	if err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]uuid.UUID{"document comment": dc.ID, "issue comment": ic.ID} {
		_, err := e.svc.UpdateComment(ctx, r, e.chen, id, tracker.CommentInput{Body: "changed"}) // GetCommentForUpdate
		want("edit "+name, err, http.StatusNotFound)
		want("delete "+name, e.svc.DeleteComment(ctx, r, e.chen, id), http.StatusNotFound)
		if _, err := e.svc.UpdateComment(ctx, e.org, e.chen, id, tracker.CommentInput{Body: "by admin"}); err != nil {
			t.Fatalf("admin edit %s: %v", name, err)
		}
	}
	if n := e.count(`SELECT count(*) FROM comments WHERE body = 'changed'`); n != 0 {
		t.Fatal("a hidden comment was changed")
	}
	// A reply to a comment of a hidden target is "unknown parentId" (GetComment).
	_, err = e.svc.CreateComment(ctx, r, e.chen, ia.Identifier, tracker.CommentInput{Body: "reply", ParentID: &ic.ID})
	want("reply to a hidden comment", err, http.StatusBadRequest)

	// Imported Linear ids: the three variants find a hidden row only for a caller who sees its team.
	var cycB uuid.UUID
	if err := e.pool.QueryRow(ctx, `SELECT id FROM cycles WHERE team_id = $1 ORDER BY number LIMIT 1`, tb.ID).Scan(&cycB); err != nil {
		t.Fatal(err)
	}
	linearIssue, linearTeam, linearCycle := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, m := range []struct {
		entity, source string
		target         uuid.UUID
	}{{"issue", linearIssue, ib.ID}, {"team", linearTeam, tb.ID}, {"cycle", linearCycle, cycB}} {
		e.exec(`INSERT INTO import_mappings (org_id, entity_type, source_id, target_id) VALUES ($1, $2, $3, $4)`, e.org.ID, m.entity, m.source, m.target)
	}
	for name, o := range map[string]*auth.Org{"restricted": r, "admin": e.org} {
		hidden := http.StatusNotFound
		if o == e.org {
			hidden = 0
		}
		_, err := e.svc.GetIssue(ctx, o, e.chen, linearIssue, tracker.Includes{})
		want(name+" imported issue", err, hidden)
		_, err = e.svc.ResolveTeamID(ctx, o, linearTeam)
		want(name+" imported team", err, hidden)
		_, err = e.svc.GetCycle(ctx, o, uuid.MustParse(linearCycle))
		want(name+" imported cycle", err, hidden)
	}

	// Relations: an edge to a hidden issue is not in the restricted view.
	if _, err := e.svc.CreateRelation(ctx, e.org, e.chen, ia.Identifier, tracker.RelationInput{Type: "related", Issue: ib.Identifier}); err != nil {
		t.Fatal(err)
	}
	rel, err := e.svc.ListRelations(ctx, r, ia.Identifier)
	if err != nil || len(rel.Related) != 0 {
		t.Fatalf("restricted relations: %+v %v", rel, err)
	}
	if rel, err := e.svc.ListRelations(ctx, e.org, ia.Identifier); err != nil || len(rel.Related) != 1 {
		t.Fatalf("admin relations: %+v %v", rel, err)
	}
	is, err := e.svc.GetIssue(ctx, r, e.chen, ia.Identifier, tracker.Includes{Relations: true})
	if err != nil || is.Relations == nil || len(is.Relations.Related) != 0 {
		t.Fatalf("restricted include=relations: %+v %v", is.Relations, err)
	}

	// Lock paths: a write to a hidden row is a 404 and changes nothing.
	_, err = e.svc.UpdateProject(ctx, r, e.chen, pb.ID, tracker.UpdateProjectInput{Name: httpx.Opt[string]{Set: true, V: "hijacked"}})
	want("update hidden project", err, http.StatusNotFound)
	_, err = e.svc.UpdateDocument(ctx, r, e.chen, db.ID.String(), tracker.UpdateDocumentInput{Title: httpx.Opt[string]{Set: true, V: "hijacked"}})
	want("update hidden document", err, http.StatusNotFound)
	if n := e.count(`SELECT count(*) FROM projects WHERE name = 'hijacked'`) + e.count(`SELECT count(*) FROM documents WHERE title = 'hijacked'`); n != 0 {
		t.Fatal("a write reached a hidden row")
	}

	// Project updates: a hidden project is the 404 of an unknown one, for read and write.
	pu, err := e.svc.CreateProjectUpdate(ctx, e.org, e.chen, pb.ID, tracker.CreateProjectUpdateInput{Health: "on_track", Body: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.GetProjectUpdate(ctx, r, pb.ID, pu.ID)
	hidden := err
	want("hidden project update", err, http.StatusNotFound)
	_, err = e.svc.GetProjectUpdate(ctx, r, uuid.New(), pu.ID)
	if hidden.Error() != err.Error() {
		t.Fatalf("hidden project %q, unknown project %q", hidden, err)
	}
	if _, err := e.svc.GetProjectUpdate(ctx, e.org, pb.ID, pu.ID); err != nil {
		t.Fatalf("admin project update: %v", err)
	}
	_, err = e.svc.CreateProjectUpdate(ctx, r, e.chen, pb.ID, tracker.CreateProjectUpdateInput{Health: "on_track"})
	want("update on a hidden project", err, http.StatusNotFound)

	// Avatars: read and clear of a hidden project are the 404 of an unknown one, before any write.
	e.exec(`UPDATE projects SET avatar_key = 'k', avatar_content_type = 'image/png' WHERE id = $1`, pb.ID)
	_, err = e.svc.AvatarFileOf(ctx, r, &pb.ID)
	hidden = err
	want("hidden project avatar", err, http.StatusNotFound)
	_, err = e.svc.AvatarFileOf(ctx, r, ptr(uuid.New()))
	if hidden.Error() != err.Error() {
		t.Fatalf("hidden avatar %q, unknown %q", hidden, err)
	}
	want("clear a hidden project avatar", e.svc.ClearAvatar(ctx, r, e.chen, &pb.ID), http.StatusNotFound)
	if n := e.count(`SELECT count(*) FROM projects WHERE id = $1 AND avatar_key = 'k' AND avatar_version = 0`, pb.ID); n != 1 {
		t.Fatal("a hidden avatar was changed")
	}
	if _, err := e.svc.AvatarFileOf(ctx, e.org, &pb.ID); err != nil {
		t.Fatalf("admin avatar: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

// A nil team set means NO team (T20 #15): the label list of a restricted member
// holds the org labels and the labels of its teams, nothing of another team.
func TestScope_LabelList(t *testing.T) {
	e := newCycEnv(t)
	ctx := context.Background()
	tb, err := tracker.CreateTeamTx(ctx, e.q, e.org.ID, "BBB", "Team B", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.q.InsertLabel(ctx, dbq.InsertLabelParams{OrgID: e.org.ID, Name: "b-only", Color: "#112233", TeamID: &tb.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.q.InsertLabel(ctx, dbq.InsertLabelParams{OrgID: e.org.ID, Name: "a-only", Color: "#112233", TeamID: &e.team.ID}); err != nil {
		t.Fatal(err)
	}
	names := func(o *auth.Org) map[string]bool {
		ls, err := e.svc.ListLabels(ctx, o, "")
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]bool{}
		for _, l := range ls {
			m[l.Name] = true
		}
		return m
	}
	member := func(ids ...uuid.UUID) *auth.Org {
		o := *e.org
		o.Role, o.Access = "member", auth.NewTeamAccess("member", "selected", ids)
		return &o
	}
	if m := names(e.org); !m["a-only"] || !m["b-only"] {
		t.Fatalf("admin labels: %v", m)
	}
	if m := names(member(e.team.ID)); !m["a-only"] || m["b-only"] {
		t.Fatalf("restricted labels: %v", m)
	}
	if m := names(member()); m["a-only"] || m["b-only"] || len(m) == 0 {
		t.Fatalf("no-team labels: %v (want the org labels only)", m)
	}

	// issueCount of an org label counts only issues of visible teams (T20
	// security section, "Counts"): one issue in each team carries "shared".
	if _, err := e.q.InsertLabel(ctx, dbq.InsertLabelParams{OrgID: e.org.ID, Name: "shared", Color: "#112233"}); err != nil {
		t.Fatal(err)
	}
	for _, team := range []string{e.team.Key, tb.Key} {
		if _, err := e.svc.CreateIssue(ctx, e.org, e.chen, tracker.CreateIssueInput{Team: team, Title: "counted", Labels: []string{"shared"}}); err != nil {
			t.Fatal(err)
		}
	}
	count := func(o *auth.Org) int32 {
		ls, err := e.svc.ListLabels(ctx, o, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range ls {
			if l.Name == "shared" {
				return l.IssueCount
			}
		}
		t.Fatal("no shared label")
		return -1
	}
	for _, c := range []struct {
		who  string
		org  *auth.Org
		want int32
	}{{"admin", e.org, 2}, {"restricted to A", member(e.team.ID), 1}, {"restricted to B", member(tb.ID), 1}, {"no team", member(), 0}} {
		if got := count(c.org); got != c.want {
			t.Errorf("%s: issueCount = %d, want %d", c.who, got, c.want)
		}
	}
}
