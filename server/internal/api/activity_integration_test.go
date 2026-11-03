package api

import (
	"encoding/json"
	"strings"
	"testing"

	"devops/internal/tracker"
)

func (e *env) activity(path string) page[tracker.Activity] {
	e.t.Helper()
	var p page[tracker.Activity]
	e.must(200, e.do("GET", path, e.chen, nil)).json(e.t, &p)
	return p
}

// kinds counts the kinds of a page.
func actKinds(items []tracker.Activity) map[string]int {
	m := map[string]int{}
	for _, a := range items {
		m[a.Kind]++
	}
	return m
}

// LAUN-49: field edits, label changes and relation removals are recorded
// per issue and listed oldest first.
func TestActivity_FieldsLabelsRelations(t *testing.T) {
	e := newEnv(t)
	e.createIssue(e.chen, map[string]any{"title": "one", "labels": []string{"Bug"}})
	e.createIssue(e.chen, map[string]any{"title": "two"})
	const base = "/api/v1/orgs/orbit/issues/"

	if n := len(e.activity(base + "ORB-1/activity").Items); n != 0 {
		t.Fatalf("a new issue has %d activity rows, want 0", n)
	}
	// A PATCH that changes nothing records nothing.
	e.must(200, e.do("PATCH", base+"ORB-1", e.chen, map[string]any{"title": "one", "priority": 0}))
	if n := len(e.activity(base + "ORB-1/activity").Items); n != 0 {
		t.Fatalf("no-op PATCH recorded %d rows", n)
	}

	e.must(200, e.do("PATCH", base+"ORB-1", e.claude, map[string]any{
		"title": "one, renamed", "description": "new text", "priority": 2, "estimate": 3,
		"assignee": "claude", "dueDate": "2026-12-01", "parent": "ORB-2", "removeLabels": []string{"Bug"}, "addLabels": []string{"Feature"},
	}))
	e.must(200, e.do("PATCH", base+"ORB-1", e.chen, map[string]any{"assignee": nil, "priority": 4}))

	p := e.activity(base + "ORB-1/activity")
	got := actKinds(p.Items)
	for k, n := range map[string]int{
		"title": 1, "description": 1, "priority": 2, "estimate": 1, "assignee": 2, "dueDate": 1, "parent": 1,
		"labelAdded": 1, "labelRemoved": 1,
	} {
		if got[k] != n {
			t.Fatalf("kind %s: %d, want %d (all: %v)", k, got[k], n, got)
		}
	}
	for _, a := range p.Items {
		if a.Source != "api" || a.Actor == nil {
			t.Fatalf("row %s: source %q actor %v", a.Kind, a.Source, a.Actor)
		}
		switch a.Kind {
		case "title":
			if string(a.From) != `"one"` || string(a.To) != `"one, renamed"` {
				t.Fatalf("title: %s -> %s", a.From, a.To)
			}
		case "description":
			if string(a.From) != "null" || string(a.To) != "null" || strings.Contains(string(a.To), "new text") {
				t.Fatalf("description must hold no text: %s -> %s", a.From, a.To)
			}
		case "parent":
			var to struct{ Identifier string }
			if err := json.Unmarshal(a.To, &to); err != nil || to.Identifier != "ORB-2" {
				t.Fatalf("parent to: %s", a.To)
			}
		case "labelAdded":
			if !strings.Contains(string(a.To), `"Feature"`) || string(a.From) != "null" {
				t.Fatalf("labelAdded: %s -> %s", a.From, a.To)
			}
		case "labelRemoved":
			if !strings.Contains(string(a.From), `"Bug"`) || string(a.To) != "null" {
				t.Fatalf("labelRemoved: %s -> %s", a.From, a.To)
			}
		}
	}
	// Last row: the second PATCH, by chen, oldest first.
	if last := p.Items[len(p.Items)-1]; last.Actor.Username != "chen" {
		t.Fatalf("last row by %s, want chen", last.Actor.Username)
	}

	// Paging: limit 2, follow the cursor, same rows in the same order.
	var ids []string
	path := base + "ORB-1/activity?limit=2"
	for pages := 0; ; pages++ {
		pg := e.activity(path)
		if len(pg.Items) > 2 || pages > 20 {
			t.Fatalf("bad page: %d items, page %d", len(pg.Items), pages)
		}
		for _, a := range pg.Items {
			ids = append(ids, a.ID.String())
		}
		if pg.NextCursor == nil {
			break
		}
		path = base + "ORB-1/activity?limit=2&cursor=" + *pg.NextCursor
	}
	if len(ids) != len(p.Items) {
		t.Fatalf("paged %d rows, full list %d", len(ids), len(p.Items))
	}
	for i, a := range p.Items {
		if ids[i] != a.ID.String() {
			t.Fatalf("row %d differs between paged and full list", i)
		}
	}
	e.must(400, e.do("GET", base+"ORB-1/activity?cursor=junk", e.chen, nil))
	e.must(400, e.do("GET", base+"ORB-1/activity?limit=0", e.chen, nil))

	// Relations: added is exposed on the Relation; removed is recorded on both issues.
	var rel tracker.Relation
	e.must(201, e.do("POST", base+"ORB-1/relations", e.claude, map[string]any{"type": "blocks", "issue": "ORB-2"})).json(t, &rel)
	if rel.CreatedBy == nil || rel.CreatedBy.Username != "claude" || rel.CreatedAt.IsZero() {
		t.Fatalf("create response: %+v", rel)
	}
	var rels tracker.Relations
	e.must(200, e.do("GET", base+"ORB-2/relations", e.chen, nil)).json(t, &rels)
	if len(rels.BlockedBy) != 1 || rels.BlockedBy[0].CreatedBy == nil || rels.BlockedBy[0].CreatedBy.Username != "claude" || rels.BlockedBy[0].CreatedAt.IsZero() {
		t.Fatalf("list: %+v", rels)
	}
	e.must(204, e.do("DELETE", base+"ORB-2/relations/"+rel.ID.String(), e.chen, nil))
	for ident, want := range map[string]string{"ORB-1": `"blocks"`, "ORB-2": `"blockedBy"`} {
		var rm []tracker.Activity
		for _, a := range e.activity(base + ident + "/activity").Items {
			if a.Kind == "relationRemoved" {
				rm = append(rm, a)
			}
		}
		if len(rm) != 1 || rm[0].Actor.Username != "chen" || !strings.Contains(string(rm[0].From), want) {
			t.Fatalf("%s relationRemoved: %+v", ident, rm)
		}
	}
	// A failed delete records nothing.
	e.must(404, e.do("DELETE", base+"ORB-1/relations/"+rel.ID.String(), e.chen, nil))
	if n := actKinds(e.activity(base + "ORB-1/activity").Items)["relationRemoved"]; n != 1 {
		t.Fatalf("relationRemoved rows: %d", n)
	}
	// A read token can read it; no token cannot.
	e.must(200, e.do("GET", base+"ORB-1/activity", e.claude, nil))
	e.must(401, e.do("GET", base+"ORB-1/activity", "", nil))
}
