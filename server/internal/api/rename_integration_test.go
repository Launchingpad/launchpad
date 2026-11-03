package api

import (
	"testing"

	"devops/internal/tracker"
)

// LAUN-99: renaming a team, a product and the org. The key and the slugs of
// teams and the org stay fixed; each rename emits one event and needs admin.
func TestRename_TeamProductOrg(t *testing.T) {
	e := newEnv(t)
	prod := e.mkProduct(map[string]any{"name": "Old Product", "teams": []string{"ORB"}})

	// Team: trimmed, key fixed, one event, a repeat writes nothing.
	var tm tracker.Team
	e.must(200, e.do("PATCH", O+"/teams/ORB", e.chen, map[string]any{"name": "  New Team  "})).json(t, &tm)
	if tm.Name != "New Team" || tm.Key != "ORB" {
		t.Fatalf("team after rename %+v", tm)
	}
	if n := e.eventCount("team.updated"); n != 1 {
		t.Fatalf("team.updated events %d, want 1", n)
	}
	e.must(200, e.do("PATCH", O+"/teams/ORB", e.chen, map[string]any{"name": "New Team"}))
	if n := e.eventCount("team.updated"); n != 1 {
		t.Fatalf("a no-op rename emitted: %d events", n)
	}
	expectCode(t, e.do("PATCH", O+"/teams/ORB", e.chen, map[string]any{"name": nil}), 400, "validation_failed")
	expectCode(t, e.do("PATCH", O+"/teams/ORB", e.chen, map[string]any{"name": " "}), 400, "validation_failed")
	expectCode(t, e.do("PATCH", O+"/teams/ORB", e.claude, map[string]any{"name": "x"}), 403, "insufficient_scope")

	// Product: name changes, slug stays unless sent.
	before := e.eventCount("product.updated")
	var pd tracker.ProductDetail
	e.must(200, e.do("PATCH", P+"/"+prod.ID.String(), e.chen, map[string]any{"name": "New Product"})).json(t, &pd)
	if pd.Name != "New Product" || pd.Slug != prod.Slug {
		t.Fatalf("product after rename %+v", pd)
	}
	if n := e.eventCount("product.updated"); n != before+1 {
		t.Fatalf("product.updated events %d, want %d", n, before+1)
	}
	expectCode(t, e.do("PATCH", P+"/"+prod.ID.String(), e.chen, map[string]any{"name": ""}), 400, "validation_failed")
	expectCode(t, e.do("PATCH", P+"/"+prod.ID.String(), e.claude, map[string]any{"name": "x"}), 403, "insufficient_scope")

	// Org: name changes, slug stays; the slug is an unknown field.
	var o tracker.Org
	e.must(200, e.do("PATCH", O, e.chen, map[string]any{"name": "Orbit Two"})).json(t, &o)
	if o.Name != "Orbit Two" || o.Slug != "orbit" {
		t.Fatalf("org after rename %+v", o)
	}
	expectCode(t, e.do("PATCH", O, e.chen, map[string]any{"slug": "other"}), 400, "bad_request")
	expectCode(t, e.do("PATCH", O, e.claude, map[string]any{"name": "x"}), 403, "insufficient_scope")

	// Another org's admin cannot rename any of them.
	acmeID := e.newOrg("acme", "ACM")
	dave := e.newUser("dave")
	e.addMember(acmeID, dave.ID, "admin")
	tok, _ := e.newToken(dave.ID, tokenOpts{})
	expectCode(t, e.do("PATCH", O+"/teams/ORB", tok, map[string]any{"name": "x"}), 404, "not_found")
	expectCode(t, e.do("PATCH", P+"/"+prod.ID.String(), tok, map[string]any{"name": "x"}), 404, "not_found")
	expectCode(t, e.do("PATCH", O, tok, map[string]any{"name": "x"}), 404, "not_found")
}
