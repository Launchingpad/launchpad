package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/tracker"
)

// LAUN-194 (docs/internal/decisions-T28-project-labels.md): project labels.

func (e *env) mkProjectLabel(token string, body map[string]any) tracker.ProjectLabel {
	e.t.Helper()
	var l tracker.ProjectLabel
	e.must(201, e.do("POST", O+"/project-labels", token, body)).json(e.t, &l)
	return l
}

func labelNames(ls []tracker.ProjectLabelRef) string {
	n := make([]string, len(ls))
	for i, l := range ls {
		n[i] = l.Name
	}
	return strings.Join(n, ",")
}

func projectNames(ps []tracker.Project) string {
	n := make([]string, len(ps))
	for i, p := range ps {
		n[i] = p.Name
	}
	return strings.Join(n, ",")
}

func (e *env) listProjectsQ(token, q string) []tracker.Project {
	e.t.Helper()
	var p page[tracker.Project]
	e.must(200, e.do("GET", O+"/projects?"+q, token, nil)).json(e.t, &p)
	return p.Items
}

func TestProjectLabels_CRUDAndScope(t *testing.T) {
	e := newEnv(t)
	design := e.mkProjectLabel(e.chen, map[string]any{"name": "Design", "color": "#112233"})
	if design.Color != "#112233" || design.ParentID != nil || design.ProjectCount != 0 {
		t.Fatalf("created %+v", design)
	}
	if got := e.mkProjectLabel(e.chen, map[string]any{"name": "Plain"}); got.Color != "#95a2b3" {
		t.Fatalf("default colour %q", got.Color)
	}
	// Unique ignoring case: 409.
	expectCode(t, e.do("POST", O+"/project-labels", e.chen, map[string]any{"name": "dESIGN"}), 409, "conflict")
	// Validation.
	expectCode(t, e.do("POST", O+"/project-labels", e.chen, map[string]any{"name": " "}), 400, "validation_failed")
	expectCode(t, e.do("POST", O+"/project-labels", e.chen, map[string]any{"name": "x", "color": "red"}), 400, "validation_failed")
	expectCode(t, e.do("POST", O+"/project-labels", e.chen, map[string]any{"name": "x", "parentId": "nope"}), 400, "validation_failed")
	expectCode(t, e.do("POST", O+"/project-labels", e.chen, map[string]any{"name": "x", "parentId": uuid.NewString()}), 400, "validation_failed")

	// Scope: write (the agent) reads but cannot write the catalogue; read cannot write either.
	var l page[tracker.ProjectLabel]
	e.must(200, e.do("GET", O+"/project-labels", e.claude, nil)).json(t, &l)
	if len(l.Items) != 2 || l.Items[0].Name != "Design" {
		t.Fatalf("list %+v", l.Items)
	}
	p := O + "/project-labels/" + design.ID.String()
	expectCode(t, e.do("POST", O+"/project-labels", e.claude, map[string]any{"name": "Nope"}), 403, "insufficient_scope")
	expectCode(t, e.do("PATCH", p, e.claude, map[string]any{"name": "Nope"}), 403, "insufficient_scope")
	expectCode(t, e.do("DELETE", p, e.claude, nil), 403, "insufficient_scope")
	expectCode(t, e.do("PATCH", O+"/project-labels/not-a-uuid", e.chen, map[string]any{"name": "x"}), 404, "not_found")

	// Rename and recolour; a clash is 409; a no-op writes no event.
	var up tracker.ProjectLabel
	e.must(200, e.do("PATCH", p, e.chen, map[string]any{"name": "UX", "color": "#abcdef"})).json(t, &up)
	if up.Name != "UX" || up.Color != "#abcdef" {
		t.Fatalf("updated %+v", up)
	}
	expectCode(t, e.do("PATCH", p, e.chen, map[string]any{"name": "plain"}), 409, "conflict")
	expectCode(t, e.do("PATCH", p, e.chen, map[string]any{"color": "blue"}), 400, "validation_failed")
	expectCode(t, e.do("PATCH", p, e.chen, map[string]any{"name": nil}), 400, "validation_failed")
	before := e.eventCount("project_label.updated")
	e.must(200, e.do("PATCH", p, e.chen, map[string]any{"name": "UX"}))
	if e.eventCount("project_label.updated") != before || before != 1 {
		t.Fatalf("updated events %d then %d", before, e.eventCount("project_label.updated"))
	}
	if e.eventCount("project_label.created") != 2 {
		t.Fatal("want 2 project_label.created")
	}
	e.must(204, e.do("DELETE", p, e.chen, nil))
	expectCode(t, e.do("DELETE", p, e.chen, nil), 404, "not_found")
	if e.eventCount("project_label.deleted") != 1 {
		t.Fatal("want one project_label.deleted")
	}
	// The event is the LAST of the transaction.
	if n := e.countRows(`SELECT count(*) FROM events WHERE seq > (SELECT max(seq) FROM events WHERE type = 'project_label.deleted')`); n != 0 {
		t.Fatal("project_label.deleted is not the last event")
	}
}

func TestProjectLabels_Groups(t *testing.T) {
	e := newEnv(t)
	grp := e.mkProjectLabel(e.chen, map[string]any{"name": "Area"})
	kid := e.mkProjectLabel(e.chen, map[string]any{"name": "Backend", "parentId": grp.ID})
	if kid.ParentID == nil || *kid.ParentID != grp.ID {
		t.Fatalf("kid %+v", kid)
	}
	k := O + "/project-labels/" + kid.ID.String()
	g := O + "/project-labels/" + grp.ID.String()
	// One level only.
	expectCode(t, e.do("POST", O+"/project-labels", e.chen, map[string]any{"name": "Deep", "parentId": kid.ID}), 400, "validation_failed")
	expectCode(t, e.do("PATCH", g, e.chen, map[string]any{"parentId": grp.ID}), 400, "validation_failed")
	other := e.mkProjectLabel(e.chen, map[string]any{"name": "Other"})
	expectCode(t, e.do("PATCH", g, e.chen, map[string]any{"parentId": other.ID}), 400, "validation_failed") // a group with children
	// Regroup and ungroup.
	var up tracker.ProjectLabel
	e.must(200, e.do("PATCH", k, e.chen, map[string]any{"parentId": other.ID})).json(t, &up)
	if up.ParentID == nil || *up.ParentID != other.ID {
		t.Fatalf("regrouped %+v", up)
	}
	e.must(200, e.do("PATCH", k, e.chen, map[string]any{"parentId": nil})).json(t, &up)
	if up.ParentID != nil {
		t.Fatalf("ungrouped %+v", up)
	}
	// Deleting a group keeps its labels, with no group.
	e.must(200, e.do("PATCH", k, e.chen, map[string]any{"parentId": grp.ID}))
	e.must(204, e.do("DELETE", g, e.chen, nil))
	var l page[tracker.ProjectLabel]
	e.must(200, e.do("GET", O+"/project-labels", e.chen, nil)).json(t, &l)
	for _, x := range l.Items {
		if x.ID == kid.ID && x.ParentID != nil {
			t.Fatal("child kept a deleted group")
		}
	}
	if len(l.Items) != 2 {
		t.Fatalf("labels %+v", l.Items)
	}
}

func TestProjectLabels_OnProjects(t *testing.T) {
	e := newEnv(t)
	area := e.mkProjectLabel(e.chen, map[string]any{"name": "Area"})
	api := e.mkProjectLabel(e.chen, map[string]any{"name": "API", "parentId": area.ID})
	web := e.mkProjectLabel(e.chen, map[string]any{"name": "Web", "parentId": area.ID})
	risk := e.mkProjectLabel(e.chen, map[string]any{"name": "Risk"})

	a := e.createProject(e.chen, map[string]any{"name": "Alpha", "labelIds": []string{api.ID.String(), risk.ID.String(), api.ID.String()}})
	if labelNames(a.Labels) != "API,Risk" {
		t.Fatalf("created labels %q", labelNames(a.Labels))
	}
	b := e.createProject(e.chen, map[string]any{"name": "Beta"})
	if b.Labels == nil || len(b.Labels) != 0 {
		t.Fatalf("no labels must be [], got %#v", b.Labels)
	}
	c := e.createProject(e.chen, map[string]any{"name": "Gamma", "labelIds": []string{web.ID.String()}})

	// The write scope may set labels on a project (it may edit the project).
	pa := O + "/projects/" + a.ID.String()
	var got tracker.Project
	e.must(200, e.do("PATCH", pa, e.claude, map[string]any{"labelIds": []string{risk.ID.String(), web.ID.String()}})).json(t, &got)
	if labelNames(got.Labels) != "Risk,Web" || got.Version != a.Version+1 {
		t.Fatalf("patched %q v%d", labelNames(got.Labels), got.Version)
	}
	evs := e.eventsOfType(e.chen, "project.updated")
	last := evs[len(evs)-1]
	var pl struct{ Changes []string }
	if err := json.Unmarshal(last.Payload, &pl); err != nil || len(pl.Changes) != 1 || pl.Changes[0] != "labels" {
		t.Fatalf("payload %s", last.Payload)
	}
	// The same set again changes nothing: no version bump, no event.
	n := e.eventCount("project.updated")
	e.must(200, e.do("PATCH", pa, e.claude, map[string]any{"labelIds": []string{web.ID.String(), risk.ID.String()}})).json(t, &got)
	if got.Version != a.Version+1 || e.eventCount("project.updated") != n {
		t.Fatal("a same-set PATCH wrote")
	}
	// A name change with a label change is one project.updated with both.
	e.must(200, e.do("PATCH", pa, e.claude, map[string]any{"name": "Alpha2", "labelIds": []string{api.ID.String()}})).json(t, &got)
	if got.Name != "Alpha2" || labelNames(got.Labels) != "API" {
		t.Fatalf("both %+v", got)
	}

	// The list shows the labels; GET too.
	if ps := e.listProjectsQ(e.chen, "label=Risk"); len(ps) != 0 {
		t.Fatalf("Risk was replaced, got %s", projectNames(ps))
	}
	e.must(200, e.do("PATCH", pa, e.chen, map[string]any{"labelIds": []string{api.ID.String(), risk.ID.String()}}))
	if ps := e.listProjectsQ(e.chen, "label=risk"); projectNames(ps) != "Alpha2" || labelNames(ps[0].Labels) != "API,Risk" {
		t.Fatalf("by name: %s", projectNames(ps))
	}
	if ps := e.listProjectsQ(e.chen, "label="+api.ID.String()); projectNames(ps) != "Alpha2" {
		t.Fatalf("by id: %s", projectNames(ps))
	}
	// A group matches the projects of its labels, by id and by name.
	for _, q := range []string{"label=" + area.ID.String(), "label=area"} {
		ps := e.listProjectsQ(e.chen, q)
		if len(ps) != 2 || !strings.Contains(projectNames(ps), "Alpha2") || !strings.Contains(projectNames(ps), "Gamma") {
			t.Fatalf("%s: %s", q, projectNames(ps))
		}
	}
	if ps := e.listProjectsQ(e.chen, "label=nothing"); len(ps) != 0 {
		t.Fatalf("unknown label matched %s", projectNames(ps))
	}
	if ps := e.listProjectsQ(e.chen, "label=Risk&state=planned"); projectNames(ps) != "Alpha2" {
		t.Fatalf("combined: %s", projectNames(ps))
	}
	_ = c

	// Counts, then a delete takes the label off every project and leaves them alone.
	var l page[tracker.ProjectLabel]
	e.must(200, e.do("GET", O+"/project-labels", e.chen, nil)).json(t, &l)
	counts := map[string]int{}
	for _, x := range l.Items {
		counts[x.Name] = x.ProjectCount
	}
	if counts["API"] != 1 || counts["Risk"] != 1 || counts["Web"] != 1 || counts["Area"] != 0 {
		t.Fatalf("counts %v", counts)
	}
	pn := e.eventCount("project.updated")
	e.must(204, e.do("DELETE", O+"/project-labels/"+risk.ID.String(), e.chen, nil))
	if n := e.countRows(`SELECT count(*) FROM project_label_assignments WHERE label_id = $1`, risk.ID); n != 0 {
		t.Fatalf("%d assignments left", n)
	}
	e.must(200, e.do("GET", pa, e.chen, nil)).json(t, &got)
	if labelNames(got.Labels) != "API" || e.eventCount("project.updated") != pn {
		t.Fatalf("after delete %q", labelNames(got.Labels))
	}
	// Deleting a project removes its assignments; the label stays.
	e.must(204, e.do("DELETE", pa, e.chen, nil))
	if n := e.countRows(`SELECT count(*) FROM project_label_assignments WHERE project_id = $1`, a.ID); n != 0 {
		t.Fatal("assignments left after the project went")
	}
	e.must(200, e.do("GET", O+"/project-labels", e.chen, nil))

	// Clearing: null and [].
	pc := O + "/projects/" + c.ID.String()
	e.must(200, e.do("PATCH", pc, e.chen, map[string]any{"labelIds": nil})).json(t, &got)
	if len(got.Labels) != 0 {
		t.Fatalf("null did not clear: %+v", got.Labels)
	}
	// Bad input.
	expectCode(t, e.do("PATCH", pc, e.chen, map[string]any{"labelIds": []string{"nope"}}), 400, "validation_failed")
	expectCode(t, e.do("POST", O+"/projects", e.chen, map[string]any{"name": "Z", "labelIds": []string{uuid.NewString()}}), 400, "validation_failed")
	if n := e.countRows(`SELECT count(*) FROM projects WHERE name = 'Z'`); n != 0 {
		t.Fatal("a refused create left a project")
	}
	expectCode(t, e.do("GET", O+"/projects?labels=x", e.chen, nil), 400, "validation_failed")
}

func TestProjectLabels_CrossOrg(t *testing.T) {
	e := newEnv(t)
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "admin")
	tok, _ := e.newToken(chen.ID, tokenOpts{}) // unpinned: a member of both orgs
	const A = "/api/v1/orgs/acme"
	var theirs, theirGroup tracker.ProjectLabel
	e.must(201, e.do("POST", A+"/project-labels", tok, map[string]any{"name": "Theirs"})).json(t, &theirs)
	e.must(201, e.do("POST", A+"/project-labels", tok, map[string]any{"name": "TheirGroup"})).json(t, &theirGroup)
	var theirProject tracker.Project
	e.must(201, e.do("POST", A+"/projects", tok, map[string]any{"name": "Acme P", "labelIds": []string{theirs.ID.String()}})).json(t, &theirProject)
	mine := e.mkProjectLabel(tok, map[string]any{"name": "Mine"})
	myProject := e.createProject(tok, map[string]any{"name": "Orbit P", "labelIds": []string{mine.ID.String()}})

	// Another org's label in a path is 404.
	p := O + "/project-labels/" + theirs.ID.String()
	expectCode(t, e.do("PATCH", p, tok, map[string]any{"name": "x"}), 404, "not_found")
	expectCode(t, e.do("DELETE", p, tok, nil), 404, "not_found")
	// ... and in a body is 400, for a project and for a label's group.
	pm := O + "/projects/" + myProject.ID.String()
	expectCode(t, e.do("PATCH", pm, tok, map[string]any{"labelIds": []string{theirs.ID.String()}}), 400, "validation_failed")
	expectCode(t, e.do("POST", O+"/projects", tok, map[string]any{"name": "Y", "labelIds": []string{theirs.ID.String()}}), 400, "validation_failed")
	expectCode(t, e.do("POST", O+"/project-labels", tok, map[string]any{"name": "Kid", "parentId": theirGroup.ID}), 400, "validation_failed")
	expectCode(t, e.do("PATCH", O+"/project-labels/"+mine.ID.String(), tok, map[string]any{"parentId": theirGroup.ID}), 400, "validation_failed")
	// Another org's project cannot take our label through its path.
	expectCode(t, e.do("PATCH", O+"/projects/"+theirProject.ID.String(), tok, map[string]any{"labelIds": []string{mine.ID.String()}}), 404, "not_found")
	// Lists never mix; a filter with another org's label matches nothing.
	var l page[tracker.ProjectLabel]
	e.must(200, e.do("GET", O+"/project-labels", tok, nil)).json(t, &l)
	if len(l.Items) != 1 || l.Items[0].ID != mine.ID {
		t.Fatalf("orbit labels %+v", l.Items)
	}
	if ps := e.listProjectsQ(tok, "label="+theirs.ID.String()); len(ps) != 0 {
		t.Fatalf("filter crossed orgs: %s", projectNames(ps))
	}
	if ps := e.listProjectsQ(tok, "label=Theirs"); len(ps) != 0 {
		t.Fatalf("name filter crossed orgs: %s", projectNames(ps))
	}
	// Nothing changed on their side.
	if n := e.countRows(`SELECT count(*) FROM project_label_assignments WHERE org_id = $1`, acmeID); n != 1 {
		t.Fatalf("acme assignments %d", n)
	}
}

// A label DELETE racing a project PATCH that names the label: the share lock
// on the label rows makes the PATCH wait (then 400, the label is gone) or win
// (then the delete removes the assignment). Never a 5xx.
func TestProjectLabels_DeleteRacesProjectPatch(t *testing.T) {
	e := newEnv(t)
	for round := 0; round < 30; round++ {
		l := e.mkProjectLabel(e.chen, map[string]any{"name": fmt.Sprintf("Race %d", round)})
		p := e.createProject(e.chen, map[string]any{"name": fmt.Sprintf("P%d", round)})
		del := func() resp {
			r, err := e.request("DELETE", O+"/project-labels/"+l.ID.String(), e.chen, nil)
			if err != nil {
				t.Error(err)
			}
			return r
		}
		patch := func() resp {
			r, err := e.request("PATCH", O+"/projects/"+p.ID.String(), e.chen, map[string]any{"labelIds": []string{l.ID.String()}})
			if err != nil {
				t.Error(err)
			}
			return r
		}
		var rd, rp resp
		var wg sync.WaitGroup
		first, second := func() { rd = del() }, func() { rp = patch() }
		if round%2 == 1 { // both start orders
			first, second = second, first
		}
		wg.Add(2)
		go func() { defer wg.Done(); first() }()
		go func() { defer wg.Done(); time.Sleep(time.Duration(round%3) * time.Millisecond); second() }()
		wg.Wait()
		if rd.status != 204 || (rp.status != 200 && rp.status != 400) {
			t.Fatalf("round %d: delete %d, patch %d: %s", round, rd.status, rp.status, rp.body)
		}
		if n := e.countRows(`SELECT count(*) FROM project_label_assignments WHERE label_id = $1`, l.ID); n != 0 {
			t.Fatalf("round %d: %d assignments left for a deleted label", round, n)
		}
	}
}

// Two label edits that move labels under each other's group never deadlock.
func TestProjectLabels_OppositeRegroupNoDeadlock(t *testing.T) {
	e := newEnv(t)
	for round := 0; round < 20; round++ {
		a := e.mkProjectLabel(e.chen, map[string]any{"name": fmt.Sprintf("A%d", round)})
		b := e.mkProjectLabel(e.chen, map[string]any{"name": fmt.Sprintf("B%d", round)})
		var ra, rb resp
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			ra, _ = e.request("PATCH", O+"/project-labels/"+a.ID.String(), e.chen, map[string]any{"parentId": b.ID})
		}()
		go func() {
			defer wg.Done()
			rb, _ = e.request("PATCH", O+"/project-labels/"+b.ID.String(), e.chen, map[string]any{"parentId": a.ID})
		}()
		wg.Wait()
		if ra.status >= 500 || rb.status >= 500 {
			t.Fatalf("round %d: %d %d: %s %s", round, ra.status, rb.status, ra.body, rb.body)
		}
	}
}

func TestProjectLabels_Idempotency(t *testing.T) {
	e := newEnv(t)
	body := map[string]any{"name": "Once"}
	r1 := e.must(201, e.do("POST", O+"/project-labels", e.chen, body, "Idempotency-Key", "pl-1"))
	r2 := e.must(201, e.do("POST", O+"/project-labels", e.chen, body, "Idempotency-Key", "pl-1"))
	if string(r1.body) != string(r2.body) || r2.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay differs:\n%s\n%s", r1.body, r2.body)
	}
	if n := e.countRows(`SELECT count(*) FROM project_labels`); n != 1 {
		t.Fatalf("%d labels after a replayed create", n)
	}
	// Same key, another body: refused.
	expectCode(t, e.do("POST", O+"/project-labels", e.chen, map[string]any{"name": "Other"}, "Idempotency-Key", "pl-1"), 409, "idempotency_key_reused")
	// A write-scope token is refused by the scope check BEFORE the key is stored.
	before := e.countRows(`SELECT count(*) FROM idempotency_keys`)
	expectCode(t, e.do("POST", O+"/project-labels", e.claude, map[string]any{"name": "Nope"}, "Idempotency-Key", "pl-2"), 403, "insufficient_scope")
	if after := e.countRows(`SELECT count(*) FROM idempotency_keys`); after != before {
		t.Fatalf("a 403 stored an idempotency key (%d -> %d)", before, after)
	}
}
