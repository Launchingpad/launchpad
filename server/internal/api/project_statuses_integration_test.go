package api

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"devops/internal/httpx"
	"devops/internal/tracker"
)

// LAUN-195 (docs/internal/decisions-T29-project-statuses.md): custom project statuses.

func (e *env) statuses(token, base string) []tracker.ProjectStatus {
	e.t.Helper()
	var page struct {
		Items []tracker.ProjectStatus `json:"items"`
	}
	e.must(200, e.do("GET", base+"/project-statuses", token, nil)).json(e.t, &page)
	return page.Items
}

func statusByKind(t *testing.T, sts []tracker.ProjectStatus, kind string) tracker.ProjectStatus {
	t.Helper()
	for _, s := range sts {
		if s.Kind == kind {
			return s
		}
	}
	t.Fatalf("no %s status", kind)
	return tracker.ProjectStatus{}
}

func (e *env) newStatus(token, kind, name string) tracker.ProjectStatus {
	e.t.Helper()
	var st tracker.ProjectStatus
	e.must(201, e.do("POST", O+"/project-statuses", token, map[string]any{"kind": kind, "name": name, "color": "#112233"})).json(e.t, &st)
	return st
}

func TestProjectStatuses_DefaultsAndScopes(t *testing.T) {
	e := newEnv(t)
	sts := e.statuses(e.chen, O)
	if len(sts) != 6 {
		t.Fatalf("%d default statuses", len(sts))
	}
	want := []string{"Backlog", "Planned", "In progress", "Paused", "Completed", "Canceled"}
	for i, s := range sts {
		if s.Name != want[i] || s.Position != int32(i) {
			t.Fatalf("status %d = %+v", i, s)
		}
	}
	// A project made with only a kind lands on the default status of that kind.
	p := e.createProject(e.chen, map[string]any{"name": "P", "state": "started"})
	if p.State != "started" || p.Status.Name != "In progress" || p.Status.Kind != "started" {
		t.Fatalf("project %+v", p)
	}

	readTok, _ := e.newToken(e.user("chen").ID, tokenOpts{scopes: []string{"read"}})
	e.statuses(readTok, O) // read may list
	for _, c := range []struct{ m, path string }{
		{"POST", "/project-statuses"}, {"PUT", "/project-statuses/order"},
		{"PATCH", "/project-statuses/" + sts[0].ID.String()}, {"DELETE", "/project-statuses/" + sts[0].ID.String()},
	} {
		for _, tok := range []string{e.claude, readTok} { // write and read scope
			expectCode(t, e.do(c.m, O+c.path, tok, map[string]any{}), 403, "insufficient_scope")
		}
	}
	if n := e.eventCount("project_status.created"); n != 0 {
		t.Fatal("event from a refused write")
	}
}

func TestProjectStatuses_CreateUpdateValidation(t *testing.T) {
	e := newEnv(t)
	st := e.newStatus(e.chen, "started", "Active")
	if st.Position != 6 || st.Kind != "started" || st.Color != "#112233" {
		t.Fatalf("created %+v", st)
	}
	// The list is in kind order, so a new Started status sits with its kind, not after Canceled.
	var names []string
	for _, s := range e.statuses(e.chen, O) {
		names = append(names, s.Name)
	}
	if got := strings.Join(names, ","); got != "Backlog,Planned,In progress,Active,Paused,Completed,Canceled" {
		t.Fatalf("list order %s", got)
	}
	for name, body := range map[string]map[string]any{
		"bad kind":         {"kind": "done", "name": "X", "color": "#112233"},
		"empty name":       {"kind": "started", "name": "  ", "color": "#112233"},
		"long name":        {"kind": "started", "name": strings.Repeat("n", 51), "color": "#112233"},
		"another kind":     {"kind": "started", "name": "Paused", "color": "#112233"},
		"bad colour":       {"kind": "started", "name": "Y", "color": "red"},
		"unknown field":    {"kind": "started", "name": "Y", "color": "#112233", "position": 1},
		"missing colour":   {"kind": "started", "name": "Y"},
		"same kind word":   nil, // allowed: checked below
		"duplicate (case)": {"kind": "paused", "name": "ACTIVE", "color": "#112233"},
		"comma in name":    {"kind": "paused", "name": "On, hold", "color": "#112233"},
	} {
		r := e.do("POST", O+"/project-statuses", e.chen, body)
		want := 400
		if strings.HasPrefix(name, "duplicate") {
			want = 409
		}
		if r.status != want {
			t.Fatalf("%s: status %d: %s", name, r.status, r.body)
		}
	}
	// The kind's own word is a fine name for a status of that kind (a case change of its own name).
	paused := statusByKind(t, e.statuses(e.chen, O), "paused")
	e.must(200, e.do("PATCH", O+"/project-statuses/"+paused.ID.String(), e.chen, map[string]any{"name": "paused"}))

	before := e.eventCount("project_status.updated")
	var up tracker.ProjectStatus
	e.must(200, e.do("PATCH", O+"/project-statuses/"+st.ID.String(), e.chen, map[string]any{"name": "Doing", "color": "#00FF00"})).json(t, &up)
	if up.Name != "Doing" || up.Color != "#00ff00" || up.Kind != "started" {
		t.Fatalf("updated %+v", up)
	}
	// A no-op writes no event; the kind cannot change; a clash is 409.
	e.must(200, e.do("PATCH", O+"/project-statuses/"+st.ID.String(), e.chen, map[string]any{"name": "Doing"}))
	if n := e.eventCount("project_status.updated"); n != before+1 {
		t.Fatalf("%d update events", n-before)
	}
	expectCode(t, e.do("PATCH", O+"/project-statuses/"+st.ID.String(), e.chen, map[string]any{"kind": "paused"}), 400, "bad_request")
	expectCode(t, e.do("PATCH", O+"/project-statuses/"+st.ID.String(), e.chen, map[string]any{"name": "planned"}), 400, "validation_failed")
	expectCode(t, e.do("PATCH", O+"/project-statuses/"+st.ID.String(), e.chen, map[string]any{"name": "in progress"}), 409, "conflict")
	expectCode(t, e.do("PATCH", O+"/project-statuses/"+uuid.NewString(), e.chen, map[string]any{"name": "Z"}), 404, "not_found")
	expectCode(t, e.do("PATCH", O+"/project-statuses/nope", e.chen, map[string]any{"name": "Z"}), 404, "not_found")
}

func TestProjectStatuses_ProjectsUseThem(t *testing.T) {
	e := newEnv(t)
	sts := e.statuses(e.chen, O)
	active := e.newStatus(e.chen, "started", "Active")
	// By name (any case), by id, and the kind word picks the first status of the kind.
	byName := e.createProject(e.chen, map[string]any{"name": "n", "state": "active"})
	if byName.Status.ID != active.ID || byName.State != "started" || byName.StartedAt == nil {
		t.Fatalf("by name %+v", byName)
	}
	byID := e.createProject(e.chen, map[string]any{"name": "i", "statusId": active.ID})
	if byID.Status.ID != active.ID {
		t.Fatalf("by id %+v", byID)
	}
	kind := e.createProject(e.chen, map[string]any{"name": "k", "state": "started"})
	if kind.Status.ID != statusByKind(t, sts, "started").ID {
		t.Fatalf("by kind %+v", kind.Status)
	}
	// A mismatch between state and statusId, an unknown id and a bad word are 400.
	expectCode(t, e.do("POST", O+"/projects", e.chen, map[string]any{"name": "x", "state": "paused", "statusId": active.ID}), 400, "validation_failed")
	expectCode(t, e.do("POST", O+"/projects", e.chen, map[string]any{"name": "x", "statusId": uuid.NewString()}), 400, "validation_failed")
	expectCode(t, e.do("POST", O+"/projects", e.chen, map[string]any{"name": "x", "state": "nonsense"}), 400, "validation_failed")

	// PATCH: another status of the same kind keeps the timestamps; another kind moves them.
	startedAt := *byName.StartedAt
	var moved tracker.Project
	e.must(200, e.do("PATCH", O+"/projects/"+byName.ID.String(), e.chen, map[string]any{"statusId": kind.Status.ID})).json(t, &moved)
	if moved.Status.ID != kind.Status.ID || moved.StartedAt == nil || !moved.StartedAt.Equal(startedAt) || moved.Version != byName.Version+1 {
		t.Fatalf("same-kind move %+v", moved)
	}
	done := statusByKind(t, sts, "completed")
	e.must(200, e.do("PATCH", O+"/projects/"+byName.ID.String(), e.chen, map[string]any{"state": done.Name})).json(t, &moved)
	if moved.State != "completed" || moved.CompletedAt == nil {
		t.Fatalf("kind move %+v", moved)
	}
	// Same status again writes nothing.
	var same tracker.Project
	e.must(200, e.do("PATCH", O+"/projects/"+byName.ID.String(), e.chen, map[string]any{"state": "completed"})).json(t, &same)
	if same.Version != moved.Version {
		t.Fatal("a PATCH to the same status bumped the version")
	}
	expectCode(t, e.do("PATCH", O+"/projects/"+byName.ID.String(), e.chen, map[string]any{"state": nil}), 400, "validation_failed")

	// The filter takes a kind, a name or an id; a kind matches every status of it.
	list := func(q string) int {
		var page httpx.Page[tracker.Project]
		e.must(200, e.do("GET", O+"/projects?"+q, e.chen, nil)).json(t, &page)
		return len(page.Items)
	}
	if n := list("state=started"); n != 2 { // byID (Active) + kind (In progress)
		t.Fatalf("kind filter: %d", n)
	}
	if n := list("state=Active"); n != 1 {
		t.Fatalf("name filter: %d", n)
	}
	if n := list("state=" + active.ID.String()); n != 1 {
		t.Fatalf("id filter: %d", n)
	}
	if n := list("state=Active,completed"); n != 2 {
		t.Fatalf("mixed filter: %d", n)
	}
	expectCode(t, e.do("GET", O+"/projects?state=nonsense", e.chen, nil), 400, "validation_failed")
	expectCode(t, e.do("GET", O+"/projects?state="+uuid.NewString(), e.chen, nil), 400, "validation_failed")
}

func TestProjectStatuses_Delete(t *testing.T) {
	e := newEnv(t)
	sts := e.statuses(e.chen, O)
	active := e.newStatus(e.chen, "started", "Active")
	p := e.createProject(e.chen, map[string]any{"name": "p", "statusId": active.ID})

	// In use: 409 status_in_use, and the status stays. An archived project counts.
	r := e.do("DELETE", O+"/project-statuses/"+active.ID.String(), e.chen, nil)
	expectCode(t, r, 409, "status_in_use")
	if !strings.Contains(string(r.body), "1 project") {
		t.Fatalf("message %s", r.body)
	}
	if _, err := e.pool.Exec(t.Context(), `UPDATE projects SET archived_at = now() WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	expectCode(t, e.do("DELETE", O+"/project-statuses/"+active.ID.String(), e.chen, nil), 409, "status_in_use")
	if e.eventCount("project_status.deleted") != 0 {
		t.Fatal("event on a refused delete")
	}
	// Move the project away, then it goes.
	e.must(200, e.do("PATCH", O+"/projects/"+p.ID.String(), e.chen, map[string]any{"statusId": statusByKind(t, sts, "started").ID}))
	e.must(204, e.do("DELETE", O+"/project-statuses/"+active.ID.String(), e.chen, nil))
	expectCode(t, e.do("DELETE", O+"/project-statuses/"+active.ID.String(), e.chen, nil), 404, "not_found")
	if e.eventCount("project_status.deleted") != 1 {
		t.Fatal("want one project_status.deleted")
	}
	if n := e.countRows(`SELECT count(*) FROM events WHERE seq > (SELECT seq FROM events WHERE type = 'project_status.deleted')`); n != 0 {
		t.Fatal("project_status.deleted is not the last event")
	}
	// The last status of a kind stays, in use or not.
	for _, s := range sts {
		expectCode(t, e.do("DELETE", O+"/project-statuses/"+s.ID.String(), e.chen, nil), 409, "status_last_of_kind")
	}
	if n := len(e.statuses(e.chen, O)); n != 6 {
		t.Fatalf("%d statuses left", n)
	}
}

func TestProjectStatuses_Reorder(t *testing.T) {
	e := newEnv(t)
	sts := e.statuses(e.chen, O)
	extra := e.newStatus(e.chen, "started", "Active")
	all := append(append([]tracker.ProjectStatus{}, sts...), extra)
	ids := func(l []tracker.ProjectStatus) []string {
		out := make([]string, len(l))
		for i, s := range l {
			out[i] = s.ID.String()
		}
		return out
	}
	// Active before In progress: a project made by kind now lands on it (first of the kind in the order).
	// The list stays in kind order, so the order is judged inside a kind.
	order := ids(all)
	order[2], order[6] = order[6], order[2]
	want := []string{order[0], order[1], order[2], order[6], order[3], order[4], order[5]}
	var page struct {
		Items []tracker.ProjectStatus `json:"items"`
	}
	e.must(200, e.do("PUT", O+"/project-statuses/order", e.chen, map[string]any{"ids": order})).json(t, &page)
	if got := ids(page.Items); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order %v, want %v", got, want)
	}
	if got := ids(e.statuses(e.chen, O)); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatal("GET does not show the new order")
	}
	if p := e.createProject(e.chen, map[string]any{"name": "r", "state": "started"}); p.Status.ID != extra.ID {
		t.Fatalf("kind word chose %s, not the first of the kind", p.Status.Name)
	}
	if e.eventCount("project_status.reordered") != 1 {
		t.Fatal("want one reordered event")
	}
	// The same order again writes no event.
	e.must(200, e.do("PUT", O+"/project-statuses/order", e.chen, map[string]any{"ids": order}))
	if e.eventCount("project_status.reordered") != 1 {
		t.Fatal("a repeat emitted an event")
	}
	// A missing, repeated, unknown or foreign id is 400; the order is unchanged.
	acmeID := e.newOrg("acme", "ACM")
	var foreign uuid.UUID
	if err := e.pool.QueryRow(t.Context(), `SELECT id FROM project_statuses WHERE org_id = $1 LIMIT 1`, acmeID).Scan(&foreign); err != nil {
		t.Fatal(err)
	}
	dup := append(append([]string{}, order[:6]...), order[0])
	for name, bad := range map[string][]string{
		"missing": order[:6], "repeated": dup, "unknown": append(append([]string{}, order[:6]...), uuid.NewString()),
		"foreign": append(append([]string{}, order[:6]...), foreign.String()), "empty": {},
	} {
		if r := e.do("PUT", O+"/project-statuses/order", e.chen, map[string]any{"ids": bad}); r.status != 400 {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
	}
	if got := ids(e.statuses(e.chen, O)); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatal("a refused reorder changed the order")
	}
}

func TestProjectStatuses_CrossOrg(t *testing.T) {
	e := newEnv(t)
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "admin")
	tok, _ := e.newToken(chen.ID, tokenOpts{}) // a member of both orgs
	const A = "/api/v1/orgs/acme"
	acme := e.statuses(tok, A)
	orbit := e.statuses(tok, O)
	if len(acme) != 6 {
		t.Fatalf("a new org has %d statuses", len(acme))
	}
	ax := statusByKind(t, acme, "started")
	ox := statusByKind(t, orbit, "started")

	// Another org's status through this org's path is 404.
	expectCode(t, e.do("PATCH", O+"/project-statuses/"+ax.ID.String(), tok, map[string]any{"name": "Hacked"}), 404, "not_found")
	expectCode(t, e.do("DELETE", A+"/project-statuses/"+ox.ID.String(), tok, nil), 404, "not_found")
	// A body reference to another org's status is 400, and so is a filter that names one.
	expectCode(t, e.do("POST", O+"/projects", tok, map[string]any{"name": "x", "statusId": ax.ID}), 400, "validation_failed")
	p := e.createProject(tok, map[string]any{"name": "mine"})
	expectCode(t, e.do("PATCH", O+"/projects/"+p.ID.String(), tok, map[string]any{"statusId": ax.ID}), 400, "validation_failed")
	expectCode(t, e.do("GET", O+"/projects?state="+ax.ID.String(), tok, nil), 400, "validation_failed")
	// A custom name of one org is unknown to the other; both orgs may use the same name.
	e.must(201, e.do("POST", A+"/project-statuses", tok, map[string]any{"kind": "started", "name": "Active", "color": "#112233"}))
	e.must(201, e.do("POST", O+"/project-statuses", tok, map[string]any{"kind": "paused", "name": "Active", "color": "#112233"}))
	if got := e.statuses(tok, A); statusByKind(t, got, "started").Name != "In progress" {
		t.Fatal("order of acme changed")
	}
	if n := e.countRows(`SELECT count(*) FROM project_statuses WHERE org_id = $1`, acmeID); n != 7 {
		t.Fatalf("acme has %d statuses", n)
	}
}

// A bare kind word that is the project's own kind is a no-op: it keeps a custom status (LAUN-195 review).
func TestProjectStatuses_KindWordKeepsCustomStatus(t *testing.T) {
	e := newEnv(t)
	sts := e.statuses(e.chen, O)
	active := e.newStatus(e.chen, "started", "Active")
	p := e.createProject(e.chen, map[string]any{"name": "p", "statusId": active.ID})
	for _, word := range []string{"started", "STARTED", " started "} {
		var got tracker.Project
		e.must(200, e.do("PATCH", O+"/projects/"+p.ID.String(), e.chen, map[string]any{"state": word})).json(t, &got)
		if got.Status.ID != active.ID || got.Version != p.Version {
			t.Fatalf("%q moved the project: status %s version %d", word, got.Status.Name, got.Version)
		}
	}
	if n := e.eventCount("project.updated"); n != 0 {
		t.Fatalf("%d project.updated events for a no-op", n)
	}
	// A kind word of ANOTHER kind moves to the first status of that kind.
	var moved tracker.Project
	e.must(200, e.do("PATCH", O+"/projects/"+p.ID.String(), e.chen, map[string]any{"state": "paused"})).json(t, &moved)
	if moved.Status.ID != statusByKind(t, sts, "paused").ID || moved.State != "paused" {
		t.Fatalf("moved to %+v", moved.Status)
	}
	// The default status of a kind keeps working by its kind word too (its name is the same word).
	b := e.createProject(e.chen, map[string]any{"name": "b", "state": "backlog"})
	var same tracker.Project
	e.must(200, e.do("PATCH", O+"/projects/"+b.ID.String(), e.chen, map[string]any{"state": "backlog"})).json(t, &same)
	if same.Version != b.Version {
		t.Fatal("backlog word bumped the version")
	}
}
