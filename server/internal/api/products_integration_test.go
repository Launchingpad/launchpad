package api

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"devops/internal/tracker"
)

// LAUN-62 (T18, docs/internal/decisions-T18-products.md): products through
// the HTTP API. Every rule of the P1 ticket's AC list has a test here.

const P = O + "/products"

func (e *env) product(token, ref string) tracker.ProductDetail {
	e.t.Helper()
	var p tracker.ProductDetail
	e.must(200, e.do("GET", P+"/"+ref, token, nil)).json(e.t, &p)
	return p
}

func (e *env) mkProduct(body map[string]any) tracker.ProductDetail {
	e.t.Helper()
	var p tracker.ProductDetail
	e.must(201, e.do("POST", P, e.chen, body)).json(e.t, &p)
	return p
}

func (e *env) mkTeam(key string) tracker.Team {
	e.t.Helper()
	var tm tracker.Team
	e.must(201, e.do("POST", O+"/teams", e.chen, map[string]any{"key": key, "name": key})).json(e.t, &tm)
	return tm
}

func (e *env) archiveTeam(key string) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE teams SET archived_at = now() WHERE org_id = $1 AND key = $2`, e.orgID, key); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) joinedKeys(productID uuid.UUID) []string {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(),
		`SELECT t.key FROM product_teams pt JOIN teams t ON t.id = pt.team_id WHERE pt.product_id = $1 ORDER BY t.key`, productID)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, k)
	}
	return out
}

func (e *env) productList(token, query string) page[tracker.Product] {
	e.t.Helper()
	var p page[tracker.Product]
	e.must(200, e.do("GET", P+query, token, nil)).json(e.t, &p)
	return p
}

func (e *env) issuesOf(token, query string) []string {
	e.t.Helper()
	var p page[tracker.Issue]
	e.must(200, e.do("GET", O+"/issues?"+query, token, nil)).json(e.t, &p)
	out := []string{}
	for _, i := range p.Items {
		out = append(out, i.Identifier)
	}
	slices.Sort(out)
	return out
}

func TestProducts_CreateListGetAndScopes(t *testing.T) {
	e := newEnv(t)
	e.mkTeam("AAA")
	p := e.mkProduct(map[string]any{"name": "Launchpad", "teams": []string{"ORB", "aaa"}, "icon": "L", "color": "#112233"})
	if p.Slug != "launchpad" || p.Version != 1 || p.ArchivedAt != nil || !slices.Equal(p.TeamKeys, []string{"AAA", "ORB"}) ||
		len(p.Teams) != 2 || p.Icon == nil || *p.Icon != "L" || *p.Color != "#112233" {
		t.Fatalf("created %+v", p)
	}
	// The list is {items, nextCursor: null}, whole, with team keys.
	r := e.must(200, e.do("GET", P, e.claude, nil))
	var raw map[string]json.RawMessage
	r.json(t, &raw)
	if string(raw["nextCursor"]) != "null" || len(raw) != 2 {
		t.Fatalf("list body %s", r.body)
	}
	l := e.productList(e.claude, "")
	if len(l.Items) != 1 || l.Items[0].ID != p.ID || !slices.Equal(l.Items[0].TeamKeys, []string{"AAA", "ORB"}) {
		t.Fatalf("list %+v", l.Items)
	}
	expectCode(t, e.do("GET", P+"?limit=5", e.claude, nil), 400, "validation_failed")
	// Get by slug and by id, either case of the id.
	if g := e.product(e.claude, "launchpad"); g.ID != p.ID {
		t.Fatal("get by slug")
	}
	if g := e.product(e.claude, strings.ToUpper(p.ID.String())); g.ID != p.ID {
		t.Fatal("get by upper-case id")
	}
	expectCode(t, e.do("GET", P+"/nope", e.claude, nil), 404, "not_found")
	// Team.productIds is always present, and lists the product.
	var tp page[tracker.Team]
	e.must(200, e.do("GET", O+"/teams", e.chen, nil)).json(t, &tp)
	for _, tm := range tp.Items {
		if tm.ProductIDs == nil {
			t.Fatalf("team %s: productIds null", tm.Key)
		}
		if !slices.Equal(tm.ProductIDs, []uuid.UUID{p.ID}) {
			t.Fatalf("team %s productIds %v", tm.Key, tm.ProductIDs)
		}
	}
	e.mkTeam("LONE")
	var lone tracker.Team
	e.must(200, e.do("GET", O+"/teams/LONE", e.chen, nil)).json(t, &lone)
	if !strings.Contains(string(e.do("GET", O+"/teams/LONE", e.chen, nil).body), `"productIds":[]`) || len(lone.ProductIDs) != 0 {
		t.Fatal("a team with no product must have productIds []")
	}
	var tpr page[tracker.Product]
	e.must(200, e.do("GET", O+"/teams/AAA/products", e.claude, nil)).json(t, &tpr)
	if len(tpr.Items) != 1 || tpr.Items[0].ID != p.ID {
		t.Fatalf("team products %+v", tpr.Items)
	}
	// Scopes: write is not enough (admin writes); a read token reads only.
	for _, c := range []struct{ m, path string }{
		{"POST", P}, {"PATCH", P + "/launchpad"}, {"DELETE", P + "/launchpad"},
		{"PUT", P + "/launchpad/teams"}, {"PUT", P + "/launchpad/teams/ORB"}, {"DELETE", P + "/launchpad/teams/ORB"},
	} {
		expectCode(t, e.do(c.m, c.path, e.claude, map[string]any{"name": "x", "teams": []string{}}), 403, "insufficient_scope")
	}
	readTok, _ := e.newToken(e.user("chen").ID, tokenOpts{scopes: []string{"read"}})
	expectCode(t, e.do("POST", P, readTok, map[string]any{"name": "Nope"}), 403, "insufficient_scope")
	e.must(200, e.do("GET", P, readTok, nil))
	// Body validation.
	for name, body := range map[string]map[string]any{
		"empty name":    {"name": " "},
		"long name":     {"name": strings.Repeat("n", 201)},
		"bad color":     {"name": "C", "color": "red"},
		"long icon":     {"name": "I", "icon": "123456789"},
		"unknown field": {"name": "U", "key": "X"},
		"unknown team":  {"name": "T", "teams": []string{"NOPE"}},
	} {
		expectCode(t, e.do("POST", P, e.chen, body), 400, map[bool]string{true: "bad_request", false: "validation_failed"}[name == "unknown field"])
	}
	if n := e.eventCount("product.created"); n != 1 {
		t.Fatalf("%d product.created events", n)
	}
}

func TestProducts_SlugRules(t *testing.T) {
	e := newEnv(t)
	for name, c := range map[string]struct{ name, want string }{
		"ascii":    {"Launchpad", "launchpad"},
		"accented": {"Café Ünit", "cafe-unit"},
		"symbols":  {"  Hello,   World!! ", "hello-world"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := e.mkProduct(map[string]any{"name": c.name}).Slug; got != c.want {
				t.Fatalf("slug %q, want %q", got, c.want)
			}
		})
	}
	// A slug that is not derivable, or does not start with a letter: send one.
	for _, n := range []string{"3D Tools", "2024", "日本語", "!!!"} {
		r := e.do("POST", P, e.chen, map[string]any{"name": n})
		expectCode(t, r, 400, "validation_failed")
		if !strings.Contains(string(r.body), "send a slug") {
			t.Fatalf("%q: %s", n, r.body)
		}
	}
	e.mkProduct(map[string]any{"name": "3D Tools", "slug": "tools-3d"})
	// A collision is 409, derived or sent; no numeric suffix.
	expectCode(t, e.do("POST", P, e.chen, map[string]any{"name": "launchpad"}), 409, "conflict")
	expectCode(t, e.do("POST", P, e.chen, map[string]any{"name": "Other", "slug": "launchpad"}), 409, "conflict")
	// Sent slugs that break the rules are 400 before the database.
	for _, s := range []string{"Upper", "1abc", "a--b", "a-", "-a", "a_b", "x" + strings.Repeat("y", 40),
		"a1234567-1234-1234-1234-123456789abc", "A1234567-1234-1234-1234-123456789ABC"} {
		expectCode(t, e.do("POST", P, e.chen, map[string]any{"name": "S", "slug": s}), 400, "validation_failed")
	}
	// The CHECK constraints refuse the same values when written by hand.
	for _, s := range []string{"a1234567-1234-1234-1234-123456789abc", "a-", "a--b", "1a", "A", ""} {
		if _, err := e.pool.Exec(context.Background(),
			`INSERT INTO products (org_id, slug, name) VALUES ($1, $2, 'x')`, e.orgID, s); err == nil {
			t.Fatalf("slug %q passed the CHECK", s)
		}
	}
	// PATCH uses the same rules, and a changed slug works.
	expectCode(t, e.do("PATCH", P+"/launchpad", e.chen, map[string]any{"slug": "A--b"}), 400, "validation_failed")
	expectCode(t, e.do("PATCH", P+"/launchpad", e.chen, map[string]any{"slug": "tools-3d"}), 409, "conflict")
	if g := e.must(200, e.do("PATCH", P+"/launchpad", e.chen, map[string]any{"slug": "lp"})); !strings.Contains(string(g.body), `"slug":"lp"`) {
		t.Fatal(string(g.body))
	}
	expectCode(t, e.do("GET", P+"/launchpad", e.chen, nil), 404, "not_found")
}

func TestProducts_RefLookup(t *testing.T) {
	e := newEnv(t)
	hex := "a123456789abcdef0123456789abcdef" // a legal slug that looks like a 32-hex UUID
	p := e.mkProduct(map[string]any{"name": "Hex", "slug": hex})
	if g := e.product(e.chen, hex); g.ID != p.ID {
		t.Fatal("a 32-hex slug must be looked up as a slug")
	}
	// Braced and urn forms of a real id are slugs, so they are not found.
	for _, ref := range []string{"%7B" + p.ID.String() + "%7D", "urn:uuid:" + p.ID.String(), strings.ReplaceAll(p.ID.String(), "-", "")} {
		expectCode(t, e.do("GET", P+"/"+ref, e.chen, nil), 404, "not_found")
	}
	e.must(200, e.do("GET", P+"/"+p.ID.String(), e.chen, nil))
	// A canonical id that exists nowhere is 404, and never a slug lookup.
	expectCode(t, e.do("GET", P+"/"+uuid.NewString(), e.chen, nil), 404, "not_found")
}

func TestProducts_ArchiveRestoreDelete(t *testing.T) {
	e := newEnv(t)
	e.mkTeam("AAA")
	e.mkProduct(map[string]any{"name": "Alpha", "teams": []string{"ORB", "AAA"}})
	in := e.createIssue(e.chen, map[string]any{"title": "keep me"})
	// Delete needs archive first.
	expectCode(t, e.do("DELETE", P+"/alpha", e.chen, nil), 409, "conflict")
	// Stale version.
	expectCode(t, e.do("PATCH", P+"/alpha", e.chen, map[string]any{"name": "B", "version": 9}), 409, "version_conflict")
	// A PATCH that changes nothing writes nothing.
	e.must(200, e.do("PATCH", P+"/alpha", e.chen, map[string]any{"name": "Alpha", "version": 1}))
	if e.eventCount("product.updated") != 0 || e.product(e.chen, "alpha").Version != 1 {
		t.Fatal("a no-op PATCH wrote")
	}
	// Archive: version bumps, one product.updated.
	var a tracker.ProductDetail
	e.must(200, e.do("PATCH", P+"/alpha", e.chen, map[string]any{"archived": true, "version": 1})).json(t, &a)
	if a.ArchivedAt == nil || a.Version != 2 || e.eventCount("product.updated") != 1 {
		t.Fatalf("archived %+v, events %d", a, e.eventCount("product.updated"))
	}
	// Archiving again is a no-op.
	e.must(200, e.do("PATCH", P+"/alpha", e.chen, map[string]any{"archived": true}))
	if e.eventCount("product.updated") != 1 || e.product(e.chen, "alpha").Version != 2 {
		t.Fatal("archive twice wrote")
	}
	// Archived: other fields and team changes are 409; reads work.
	for _, b := range []map[string]any{{"name": "Beta"}, {"slug": "beta"}, {"description": "d"}, {"icon": nil},
		{"color": nil}, {"archived": false, "name": "Beta"}} {
		expectCode(t, e.do("PATCH", P+"/alpha", e.chen, b), 409, "conflict")
	}
	expectCode(t, e.do("PUT", P+"/alpha/teams", e.chen, map[string]any{"teams": []string{}}), 409, "conflict")
	expectCode(t, e.do("PUT", P+"/alpha/teams/ORB", e.chen, nil), 409, "conflict")
	expectCode(t, e.do("DELETE", P+"/alpha/teams/ORB", e.chen, nil), 409, "conflict")
	expectCode(t, e.do("PATCH", P+"/alpha", e.chen, map[string]any{"archived": nil}), 400, "validation_failed")
	if g := e.product(e.claude, "alpha"); g.ArchivedAt == nil || !slices.Equal(g.TeamKeys, []string{"AAA", "ORB"}) {
		t.Fatalf("get archived %+v", g)
	}
	if n := len(e.productList(e.claude, "").Items); n != 0 {
		t.Fatalf("list shows %d archived", n)
	}
	if n := len(e.productList(e.claude, "?includeArchived=true").Items); n != 1 {
		t.Fatalf("includeArchived shows %d", n)
	}
	// An archived product leaves Team.productIds and /teams/{t}/products.
	var tm tracker.Team
	e.must(200, e.do("GET", O+"/teams/ORB", e.chen, nil)).json(t, &tm)
	if len(tm.ProductIDs) != 0 || !strings.Contains(string(e.do("GET", O+"/teams/ORB", e.chen, nil).body), `"productIds":[]`) {
		t.Fatalf("productIds %v", tm.ProductIDs)
	}
	var tpr page[tracker.Product]
	e.must(200, e.do("GET", O+"/teams/ORB/products", e.chen, nil)).json(t, &tpr)
	if len(tpr.Items) != 0 {
		t.Fatalf("team products %+v", tpr.Items)
	}
	// ?product= on an archived product is the union; an unknown one is empty.
	if got := e.issuesOf(e.chen, "product=alpha"); !slices.Equal(got, []string{in.Identifier}) {
		t.Fatalf("archived filter %v", got)
	}
	if got := e.issuesOf(e.chen, "product=nothing-here"); len(got) != 0 {
		t.Fatalf("unknown filter %v", got)
	}
	if got := e.issuesOf(e.chen, "product="+uuid.NewString()); len(got) != 0 {
		t.Fatalf("unknown id filter %v", got)
	}
	// Restore with the stale check, then archive again and delete.
	expectCode(t, e.do("PATCH", P+"/alpha", e.chen, map[string]any{"archived": false, "version": 1}), 409, "version_conflict")
	var r tracker.ProductDetail
	e.must(200, e.do("PATCH", P+"/alpha", e.chen, map[string]any{"archived": false, "version": 2})).json(t, &r)
	if r.ArchivedAt != nil || r.Version != 3 || e.eventCount("product.updated") != 2 {
		t.Fatalf("restored %+v", r)
	}
	e.must(200, e.do("PATCH", P+"/alpha", e.chen, map[string]any{"archived": true}))
	e.must(204, e.do("DELETE", P+"/alpha", e.chen, nil))
	if e.eventCount("product.deleted") != 1 {
		t.Fatal("no product.deleted")
	}
	expectCode(t, e.do("GET", P+"/alpha", e.chen, nil), 404, "not_found")
	if n := e.countRows(`SELECT count(*) FROM product_teams`); n != 0 {
		t.Fatalf("%d join rows left", n)
	}
	if n := e.countRows(`SELECT count(*) FROM teams WHERE org_id = $1 AND key IN ('ORB','AAA')`, e.orgID); n != 2 {
		t.Fatal("delete removed a team")
	}
	e.must(200, e.do("GET", O+"/issues/"+in.Identifier, e.chen, nil))
	expectCode(t, e.do("DELETE", P+"/alpha", e.chen, nil), 404, "not_found")
}

func TestProducts_ArchivedTeams(t *testing.T) {
	e := newEnv(t)
	for _, k := range []string{"AAA", "BBB", "CCC"} {
		e.mkTeam(k)
	}
	p := e.mkProduct(map[string]any{"name": "Mix", "teams": []string{"AAA", "BBB"}})
	e.archiveTeam("AAA")
	// An archived member is absent from teamKeys and from the teams of GET.
	g := e.product(e.chen, "mix")
	if !slices.Equal(g.TeamKeys, []string{"BBB"}) || len(g.Teams) != 1 || g.Teams[0].Key != "BBB" {
		t.Fatalf("get %+v", g)
	}
	if l := e.productList(e.chen, ""); !slices.Equal(l.Items[0].TeamKeys, []string{"BBB"}) {
		t.Fatalf("list %+v", l.Items)
	}
	// The same product with its row for A still stored.
	if k := e.joinedKeys(p.ID); !slices.Equal(k, []string{"AAA", "BBB"}) {
		t.Fatalf("join %v", k)
	}
	// Naming an archived team is 400 on create, replace and add-one.
	expectCode(t, e.do("POST", P, e.chen, map[string]any{"name": "N", "teams": []string{"AAA"}}), 400, "validation_failed")
	expectCode(t, e.do("PUT", P+"/mix/teams", e.chen, map[string]any{"teams": []string{"AAA", "CCC"}}), 400, "validation_failed")
	expectCode(t, e.do("PUT", P+"/mix/teams/AAA", e.chen, nil), 400, "validation_failed")
	if k := e.joinedKeys(p.ID); !slices.Equal(k, []string{"AAA", "BBB"}) {
		t.Fatalf("a refused call changed the join: %v", k)
	}
	// Replace [C] leaves A (archived), adds C, removes B.
	e.must(204, e.do("PUT", P+"/mix/teams", e.chen, map[string]any{"teams": []string{"CCC"}}))
	if k := e.joinedKeys(p.ID); !slices.Equal(k, []string{"AAA", "CCC"}) {
		t.Fatalf("replace gave %v", k)
	}
	// The filter is the union of ALL joined teams, an archived one too.
	ia := e.createIssue(e.chen, map[string]any{"title": "in A", "team": "AAA"})
	ic := e.createIssue(e.chen, map[string]any{"title": "in C", "team": "CCC"})
	e.createIssue(e.chen, map[string]any{"title": "in B", "team": "BBB"})
	e.createIssue(e.chen, map[string]any{"title": "in ORB"})
	want := []string{ia.Identifier, ic.Identifier}
	slices.Sort(want)
	if got := e.issuesOf(e.chen, "product=mix"); !slices.Equal(got, want) {
		t.Fatalf("filter %v, want %v", got, want)
	}
	if got := e.issuesOf(e.chen, "product="+p.ID.String()+"&team=CCC"); !slices.Equal(got, []string{ic.Identifier}) {
		t.Fatalf("product AND team: %v", got)
	}
	if got := e.issuesOf(e.chen, "product=mix&team=BBB"); len(got) != 0 {
		t.Fatalf("product AND other team: %v", got)
	}
	// DELETE of an archived member removes it (and 204 for one that is absent).
	e.must(204, e.do("DELETE", P+"/mix/teams/AAA", e.chen, nil))
	e.must(204, e.do("DELETE", P+"/mix/teams/AAA", e.chen, nil))
	if k := e.joinedKeys(p.ID); !slices.Equal(k, []string{"CCC"}) {
		t.Fatalf("delete gave %v", k)
	}
	// Unarchiving the team brings it back into its products.
	if _, err := e.pool.Exec(context.Background(), `UPDATE teams SET archived_at = NULL WHERE key = 'AAA'`); err != nil {
		t.Fatal(err)
	}
	e.must(204, e.do("PUT", P+"/mix/teams/AAA", e.chen, nil))
	if g := e.product(e.chen, "mix"); !slices.Equal(g.TeamKeys, []string{"AAA", "CCC"}) {
		t.Fatalf("after unarchive %+v", g.TeamKeys)
	}
}

func TestProducts_MembershipEvents(t *testing.T) {
	e := newEnv(t)
	e.mkTeam("AAA")
	e.mkTeam("BBB")
	// Create with teams: ONE product.created, no product.teams_changed.
	e.mkProduct(map[string]any{"name": "Ev", "teams": []string{"ORB"}})
	if e.eventCount("product.created") != 1 || e.eventCount("product.teams_changed") != 0 {
		t.Fatalf("created %d, changed %d", e.eventCount("product.created"), e.eventCount("product.teams_changed"))
	}
	changed := func() int { return e.eventCount("product.teams_changed") }
	// Add one: first writes and emits, the replay writes nothing.
	e.must(204, e.do("PUT", P+"/ev/teams/AAA", e.chen, nil))
	e.must(204, e.do("PUT", P+"/ev/teams/aaa", e.chen, nil))
	if changed() != 1 {
		t.Fatalf("add emitted %d", changed())
	}
	// Remove one that is not in: nothing.
	e.must(204, e.do("DELETE", P+"/ev/teams/BBB", e.chen, nil))
	if changed() != 1 {
		t.Fatalf("absent remove emitted: %d", changed())
	}
	// Replace with the same set (any order, duplicates): nothing.
	e.must(204, e.do("PUT", P+"/ev/teams", e.chen, map[string]any{"teams": []string{"AAA", "ORB", "ORB"}}))
	if changed() != 1 {
		t.Fatalf("unchanged replace emitted: %d", changed())
	}
	// A real replace: one event with the payload.
	e.must(204, e.do("PUT", P+"/ev/teams", e.chen, map[string]any{"teams": []string{"BBB"}}))
	if changed() != 2 {
		t.Fatalf("replace emitted %d", changed())
	}
	e.must(204, e.do("DELETE", P+"/ev/teams/BBB", e.chen, nil))
	if changed() != 3 {
		t.Fatalf("remove emitted %d", changed())
	}
	// Unknown teams: 404 in a path, 400 in a body. Replace needs the field.
	expectCode(t, e.do("PUT", P+"/ev/teams/NOPE", e.chen, nil), 404, "not_found")
	expectCode(t, e.do("DELETE", P+"/ev/teams/NOPE", e.chen, nil), 404, "not_found")
	expectCode(t, e.do("PUT", P+"/ev/teams", e.chen, map[string]any{"teams": []string{"NOPE"}}), 400, "validation_failed")
	expectCode(t, e.do("PUT", P+"/ev/teams", e.chen, map[string]any{}), 400, "validation_failed")
	expectCode(t, e.do("PUT", P+"/nope/teams/ORB", e.chen, nil), 404, "not_found")
	// Events are on the org feed with entityType product and the team ids.
	evs := e.eventsOfType(e.chen, "product.teams_changed")
	if len(evs) != 3 || evs[0].EntityType != "product" {
		t.Fatalf("feed %+v", evs)
	}
	var pl struct {
		Slug    string      `json:"slug"`
		TeamIDs []uuid.UUID `json:"teamIds"`
	}
	b, _ := json.Marshal(evs[0].Payload)
	_ = json.Unmarshal(b, &pl)
	if pl.Slug != "ev" || len(pl.TeamIDs) == 0 {
		t.Fatalf("payload %s", b)
	}
}

func TestProducts_Cap(t *testing.T) {
	e := newEnv(t)
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO products (org_id, slug, name) SELECT $1, 'p' || g, 'P' || g FROM generate_series(1, 198) g`, e.orgID); err != nil {
		t.Fatal(err)
	}
	// 198 + 2 racing creates at 199 would be wrong: add one more so 199 exist.
	e.mkProduct(map[string]any{"name": "Number 199"})
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Go(func() {
			r, err := e.request("POST", P, e.chen, map[string]any{"name": fmt.Sprintf("Racer %c", 'a'+i)})
			if err != nil {
				t.Error(err)
				return
			}
			codes[i] = r.status
		})
	}
	wg.Wait()
	slices.Sort(codes)
	if !slices.Equal(codes, []int{201, 409}) {
		t.Fatalf("racing creates at 199: %v", codes)
	}
	if n := e.countRows(`SELECT count(*) FROM products WHERE org_id = $1`, e.orgID); n != 200 {
		t.Fatalf("%d products", n)
	}
	// The cap counts archived products too.
	e.must(200, e.do("PATCH", P+"/p1", e.chen, map[string]any{"archived": true}))
	expectCode(t, e.do("POST", P, e.chen, map[string]any{"name": "Too many"}), 409, "conflict")
	// Delete frees a place.
	e.must(204, e.do("DELETE", P+"/p1", e.chen, nil))
	e.mkProduct(map[string]any{"name": "Fits again"})
}

func TestProducts_Idempotency(t *testing.T) {
	e := newEnv(t)
	body := map[string]any{"name": "Once"}
	a := e.must(201, e.do("POST", P, e.chen, body, "Idempotency-Key", "k-1"))
	b := e.must(201, e.do("POST", P, e.chen, body, "Idempotency-Key", "k-1"))
	if string(a.body) != string(b.body) || e.countRows(`SELECT count(*) FROM products`) != 1 || e.eventCount("product.created") != 1 {
		t.Fatal("a replayed create wrote twice")
	}
}

func TestProducts_CrossOrg(t *testing.T) {
	e := newEnv(t)
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "admin")
	tok, _ := e.newToken(chen.ID, tokenOpts{}) // unpinned: a member of both orgs
	const A = "/api/v1/orgs/acme"
	var acme tracker.ProductDetail
	e.must(201, e.do("POST", A+"/products", tok, map[string]any{"name": "Acme Prod", "teams": []string{"ACM"}})).json(t, &acme)
	acmeIssue := func() tracker.Issue {
		var is tracker.Issue
		e.must(201, e.do("POST", A+"/issues", tok, map[string]any{"title": "acme issue", "team": "ACM"})).json(t, &is)
		return is
	}()
	_ = acmeIssue
	orbit := e.mkProduct(map[string]any{"name": "Orbit Prod", "teams": []string{"ORB"}})

	// Product paths of another org are 404, by slug and by id.
	for _, ref := range []string{"acme-prod", acme.ID.String()} {
		expectCode(t, e.do("GET", P+"/"+ref, tok, nil), 404, "not_found")
		expectCode(t, e.do("PATCH", P+"/"+ref, tok, map[string]any{"name": "x"}), 404, "not_found")
		expectCode(t, e.do("DELETE", P+"/"+ref, tok, nil), 404, "not_found")
		expectCode(t, e.do("PUT", P+"/"+ref+"/teams", tok, map[string]any{"teams": []string{}}), 404, "not_found")
		expectCode(t, e.do("PUT", P+"/"+ref+"/teams/ORB", tok, nil), 404, "not_found")
		expectCode(t, e.do("DELETE", P+"/"+ref+"/teams/ORB", tok, nil), 404, "not_found")
	}
	// A team of another org: 400 in a body, 404 in a path (team not found).
	expectCode(t, e.do("POST", P, tok, map[string]any{"name": "X", "teams": []string{"ACM"}}), 400, "validation_failed")
	expectCode(t, e.do("PUT", P+"/orbit-prod/teams", tok, map[string]any{"teams": []string{"ACM"}}), 400, "validation_failed")
	expectCode(t, e.do("PUT", P+"/orbit-prod/teams/ACM", tok, nil), 404, "not_found")
	expectCode(t, e.do("PUT", P+"/orbit-prod/teams/"+acme.Teams[0].ID.String(), tok, nil), 404, "not_found")
	expectCode(t, e.do("GET", O+"/teams/ACM/products", tok, nil), 404, "not_found")
	// Lists show only the caller's org.
	if l := e.productList(tok, ""); len(l.Items) != 1 || l.Items[0].ID != orbit.ID {
		t.Fatalf("orbit list %+v", l.Items)
	}
	// ?product= with another org's product matches nothing (and no error).
	for _, ref := range []string{"acme-prod", acme.ID.String()} {
		if got := e.issuesOf(tok, "product="+ref); len(got) != 0 {
			t.Fatalf("filter %s leaked %v", ref, got)
		}
	}
	// The slug is unique per org, not globally.
	e.must(201, e.do("POST", A+"/products", tok, map[string]any{"name": "Orbit Prod"}))
	// product_teams cannot join across orgs (composite FKs).
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO product_teams (org_id, product_id, team_id) VALUES ($1, $2, $3)`,
		e.orgID, orbit.ID, acme.Teams[0].ID); err == nil {
		t.Fatal("a cross-org product_teams row was accepted")
	}
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO product_teams (org_id, product_id, team_id) VALUES ($1, $2, $3)`,
		acmeID, orbit.ID, acme.Teams[0].ID); err == nil {
		t.Fatal("a product_teams row with the wrong org was accepted")
	}
}

func TestProducts_CreateWithTeamIDs(t *testing.T) {
	e := newEnv(t)
	aaa := e.mkTeam("AAA")
	p := e.mkProduct(map[string]any{"name": "ByID", "teams": []string{aaa.ID.String(), "ORB", aaa.ID.String()}})
	if !slices.Equal(p.TeamKeys, []string{"AAA", "ORB"}) {
		t.Fatalf("team keys %v", p.TeamKeys)
	}
}
