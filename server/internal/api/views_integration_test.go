package api

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"devops/internal/notify"
)

// The remembered issue-page views (LAUN-141, docs/internal/decisions-T23-view-preferences.md).

const vwPath = "/api/v1/orgs/orbit/me/views"

func (e *env) views(token string) []notify.ViewPreference {
	e.t.Helper()
	var l notify.ViewPreferenceList
	e.must(200, e.do("GET", vwPath, token, nil)).json(e.t, &l)
	return l.Items
}

func (e *env) putView(token, place, view, sort string, at int64) notify.ViewPreference {
	e.t.Helper()
	var v notify.ViewPreference
	e.must(200, e.do("PUT", vwPath+"/"+place, token, map[string]any{"view": view, "sort": sort, "at": at})).json(e.t, &v)
	return v
}

func TestViews_StoreAndConflict(t *testing.T) {
	e := newEnv(t)
	if l := e.views(e.chen); l == nil || len(l) != 0 {
		t.Fatalf("empty list = %#v", l)
	}
	base := time.Now().UnixMilli() - 100000
	if v := e.putView(e.chen, "team:orb", "board", "manual", base); v.View != "board" || v.At != base {
		t.Fatalf("put %+v", v)
	}
	// An older write loses and the answer shows the stored value; so does an equal one.
	for _, at := range []int64{base - 1, base} {
		if v := e.putView(e.chen, "team:orb", "list", "created", at); v.View != "board" || v.Sort != "manual" {
			t.Fatalf("older write won: %+v", v)
		}
	}
	// A newer write wins.
	if v := e.putView(e.chen, "team:orb", "list", "priority", base+1); v.View != "list" || v.Sort != "priority" {
		t.Fatalf("newer write lost: %+v", v)
	}
	// A time in the future is set to the server clock, so a later honest write still works.
	v := e.putView(e.chen, "all", "board", "manual", time.Now().Add(48*time.Hour).UnixMilli())
	if v.At > time.Now().UnixMilli() {
		t.Fatalf("future at kept: %d", v.At)
	}
	if l := e.views(e.chen); len(l) != 2 || l[0].Place != "all" || l[1].Place != "team:orb" {
		t.Fatalf("list %+v", l)
	}
	// Own rows only: another person sees nothing.
	pat := e.newHuman("pat", "pat@example.com", "member")
	patTok, _ := e.newToken(pat.ID, tokenOpts{pin: &e.orgID})
	if l := e.views(patTok); len(l) != 0 {
		t.Fatalf("other person sees %+v", l)
	}
	// No event, no secret rows beyond the table.
	if n := e.countRows(`SELECT count(*) FROM events WHERE type LIKE '%view%'`); n != 0 {
		t.Fatalf("events %d", n)
	}
}

func TestViews_Validation(t *testing.T) {
	e := newEnv(t)
	bad := []struct {
		place string
		body  map[string]any
	}{
		{"team:orb", map[string]any{"view": "grid", "sort": "updated", "at": 1}},
		{"team:orb", map[string]any{"view": "list", "sort": "title", "at": 1}},
		{"team:orb", map[string]any{"view": "list", "sort": "updated", "at": -1}},
		{"team:orb", map[string]any{"view": "list", "sort": "updated"}},
		{"Team%20ORB", map[string]any{"view": "list", "sort": "updated", "at": 1}},
		{fmt.Sprintf("%0201d", 0), map[string]any{"view": "list", "sort": "updated", "at": 1}},
	}
	for i, b := range bad {
		if r := e.do("PUT", vwPath+"/"+b.place, e.chen, b.body); r.status != 400 || r.errCode(t) != "validation_failed" {
			t.Fatalf("case %d: %d %s", i, r.status, r.body)
		}
	}
	if r := e.do("PUT", vwPath+"/team:orb", e.chen, map[string]any{"view": "list", "sort": "updated", "at": 1, "extra": 1}); r.status != 400 {
		t.Fatalf("unknown field: %d %s", r.status, r.body)
	}
	if n := e.countRows(`SELECT count(*) FROM user_view_preferences`); n != 0 {
		t.Fatalf("rows %d", n)
	}
}

func TestViews_Cap(t *testing.T) {
	e := newEnv(t)
	base := time.Now().UnixMilli() - 1_000_000
	for i := 0; i < notify.MaxViewPlaces+5; i++ {
		e.putView(e.chen, fmt.Sprintf("cycle:orb:%d", i), "board", "manual", base+int64(i))
	}
	l := e.views(e.chen)
	if len(l) != notify.MaxViewPlaces {
		t.Fatalf("kept %d", len(l))
	}
	for _, v := range l {
		if v.At < base+5 {
			t.Fatalf("an old place stayed: %+v", v)
		}
	}
}

func TestViews_AgentsAndScopes(t *testing.T) {
	e := newEnv(t)
	body := map[string]any{"view": "board", "sort": "manual", "at": 1}
	for _, rt := range []struct{ m, p string }{{"GET", vwPath}, {"PUT", vwPath + "/all"}} {
		r := e.do(rt.m, rt.p, e.claude, body)
		if r.status != 403 || r.errCode(t) != "forbidden" {
			t.Fatalf("agent %s: %d %s", rt.m, r.status, r.body)
		}
	}
	chen := e.user("chen")
	ro, _ := e.newToken(chen.ID, tokenOpts{pin: &e.orgID, scopes: []string{"read"}})
	e.must(200, e.do("GET", vwPath, ro, nil))
	if r := e.do("PUT", vwPath+"/all", ro, body); r.status != 403 || r.errCode(t) != "insufficient_scope" {
		t.Fatalf("read token: %d %s", r.status, r.body)
	}
	e.must(401, e.do("GET", vwPath, "", nil))
}

// A person in two orgs has separate rows per org; a token pinned to one org
// cannot reach the other; a non-member gets 404.
func TestViews_CrossOrg(t *testing.T) {
	e := newEnv(t)
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "member")
	tok, _ := e.newToken(chen.ID, tokenOpts{})
	now := time.Now().UnixMilli()
	e.must(200, e.do("PUT", "/api/v1/orgs/orbit/me/views/all", tok, map[string]any{"view": "board", "sort": "manual", "at": now - 10}))
	var l notify.ViewPreferenceList
	e.must(200, e.do("GET", "/api/v1/orgs/acme/me/views", tok, nil)).json(t, &l)
	if len(l.Items) != 0 {
		t.Fatalf("acme sees orbit's rows: %+v", l.Items)
	}
	e.must(200, e.do("PUT", "/api/v1/orgs/acme/me/views/all", tok, map[string]any{"view": "list", "sort": "created", "at": now - 20}))
	if v := func() []notify.ViewPreference {
		var o notify.ViewPreferenceList
		e.must(200, e.do("GET", vwPath, tok, nil)).json(t, &o)
		return o.Items
	}(); len(v) != 1 || v[0].View != "board" {
		t.Fatalf("orbit rows changed: %+v", v)
	}
	// A token pinned to orbit does not reach acme.
	e.must(404, e.do("GET", "/api/v1/orgs/acme/me/views", e.chen, nil))
	e.must(404, e.do("PUT", "/api/v1/orgs/acme/me/views/all", e.chen, map[string]any{"view": "list", "sort": "created", "at": 1}))
	// An org chen is not in.
	e.newOrg("other", "OTH")
	e.must(404, e.do("GET", "/api/v1/orgs/other/me/views", tok, nil))
	e.must(404, e.do("PUT", "/api/v1/orgs/other/me/views/all", tok, map[string]any{"view": "list", "sort": "created", "at": 1}))
	if n := e.countRows(`SELECT count(*) FROM user_view_preferences`); n != 2 {
		t.Fatalf("rows %d", n)
	}
}

// With 100 newer places stored, a write for a new place with an older time is trimmed at once. The answer must
// say what is stored (the default, no row), and the next GET must agree.
func TestViews_CapTrimsTheNewWrite(t *testing.T) {
	e := newEnv(t)
	base := time.Now().UnixMilli() - 1_000_000
	for i := 0; i < notify.MaxViewPlaces; i++ {
		e.putView(e.chen, fmt.Sprintf("cycle:orb:%d", i), "board", "manual", base+1000+int64(i))
	}
	v := e.putView(e.chen, "team:old", "board", "priority", base)
	if v.Place != "team:old" || v.View != "list" || v.Sort != "updated" {
		t.Fatalf("answer %+v", v)
	}
	l := e.views(e.chen)
	if len(l) != notify.MaxViewPlaces {
		t.Fatalf("kept %d", len(l))
	}
	for _, it := range l {
		if it.Place == "team:old" {
			t.Fatalf("trimmed place listed: %+v", it)
		}
	}
}

// Parallel writes of one person must not leave more than the cap (the advisory lock in PutView).
func TestViews_CapHoldsUnderConcurrency(t *testing.T) {
	e := newEnv(t)
	base := time.Now().UnixMilli() - 1_000_000
	for i := 0; i < notify.MaxViewPlaces-2; i++ {
		e.putView(e.chen, fmt.Sprintf("cycle:orb:%d", i), "board", "manual", base+int64(i))
	}
	var wg sync.WaitGroup
	codes := make([]int, 40)
	start := make(chan struct{})
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r := e.doNoFatal("PUT", fmt.Sprintf("%s/team:new%d", vwPath, i), e.chen,
				map[string]any{"view": "board", "sort": "manual", "at": base + 10_000 + int64(i)})
			codes[i] = r.status
		}()
	}
	close(start)
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("write %d: status %d", i, c)
		}
	}
	if n := e.countRows(`SELECT count(*) FROM user_view_preferences`); n > notify.MaxViewPlaces {
		t.Fatalf("%d rows, cap %d", n, notify.MaxViewPlaces)
	}
}

// LAUN-163: the board's column choices are stored with the place, replace as a whole, and old rows read as defaults.
func TestViews_Columns(t *testing.T) {
	e := newEnv(t)
	base := time.Now().UnixMilli() - 100000
	put := func(at int64, body map[string]any) notify.ViewPreference {
		e.t.Helper()
		body["at"] = at
		var v notify.ViewPreference
		e.must(200, e.do("PUT", vwPath+"/team:orb", e.chen, body)).json(e.t, &v)
		return v
	}
	v := put(base, map[string]any{"view": "board", "sort": "manual", "hideEmpty": true, "hiddenColumns": []string{"Backlog", "Done", "Backlog"}})
	if !v.HideEmpty || len(v.HiddenColumns) != 2 || v.HiddenColumns[0] != "Backlog" || v.HiddenColumns[1] != "Done" {
		t.Fatalf("stored %+v", v)
	}
	// An older write loses and does not change the columns.
	if v := put(base-1, map[string]any{"view": "board", "sort": "manual"}); !v.HideEmpty || len(v.HiddenColumns) != 2 {
		t.Fatalf("older write changed columns: %+v", v)
	}
	// A newer write without the fields is the defaults, and the list shows [] and not null.
	v = put(base+1, map[string]any{"view": "board", "sort": "manual"})
	if v.HideEmpty || v.HiddenColumns == nil || len(v.HiddenColumns) != 0 {
		t.Fatalf("absent fields: %+v", v)
	}
	if r := e.do("GET", vwPath, e.chen, nil); !strings.Contains(string(r.body), `"hiddenColumns":[]`) {
		t.Fatalf("list: %s", r.body)
	}
	// A row from before the migration (no columns set) reads as the defaults.
	e.exec(`INSERT INTO user_view_preferences (org_id, user_id, place, view, sort, changed_at)
	        SELECT org_id, user_id, 'all', 'list', 'created', 5 FROM org_members LIMIT 1`)
	for _, v := range e.views(e.chen) {
		if v.Place == "all" && (v.HideEmpty || v.HiddenColumns == nil || len(v.HiddenColumns) != 0) {
			t.Fatalf("old row: %+v", v)
		}
	}
	// Two places are independent.
	e.putView(e.chen, "mine", "board", "manual", base)
	for _, v := range e.views(e.chen) {
		if v.Place == "mine" && (v.HideEmpty || len(v.HiddenColumns) != 0) {
			t.Fatalf("mine picked up another place's columns: %+v", v)
		}
	}
	// Validation.
	long := strings.Repeat("x", 101)
	many := make([]string, 31)
	for i := range many {
		many[i] = fmt.Sprintf("s%d", i)
	}
	for i, hc := range [][]string{{""}, {long}, {"a,b"}, {"a\nb"}, {"a\u0085b"}, many} {
		r := e.do("PUT", vwPath+"/team:orb", e.chen, map[string]any{"view": "board", "sort": "manual", "at": base + 5, "hiddenColumns": hc})
		if r.status != 400 || r.errCode(t) != "validation_failed" {
			t.Fatalf("case %d: %d %s", i, r.status, r.body)
		}
	}
	if r := e.do("PUT", vwPath+"/team:orb", e.chen, map[string]any{"view": "board", "sort": "manual", "at": base + 5, "hiddenColumns": nil}); r.status != 200 {
		t.Fatalf("null list: %d %s", r.status, r.body)
	}
	// Multibyte: 30 names of 100 CJK runes (the byte-heavy worst case) store and round-trip; 31 distinct are refused.
	cjk := make([]string, 30)
	for i := range cjk {
		cjk[i] = strings.Repeat("漢", 99) + string(rune('一'+i))
	}
	r := e.do("PUT", vwPath+"/team:orb", e.chen, map[string]any{"view": "board", "sort": "manual", "at": base + 10, "hiddenColumns": cjk})
	if r.status != 200 {
		t.Fatalf("30 x 100 CJK: %d %s", r.status, r.body)
	}
	for _, v := range e.views(e.chen) {
		if v.Place == "team:orb" && (len(v.HiddenColumns) != 30 || v.HiddenColumns[29] != cjk[29]) {
			t.Fatalf("CJK round trip: %d names", len(v.HiddenColumns))
		}
	}
	over := append(append([]string{}, cjk...), "extra")
	if r := e.do("PUT", vwPath+"/team:orb", e.chen, map[string]any{"view": "board", "sort": "manual", "at": base + 11, "hiddenColumns": over}); r.status != 400 {
		t.Fatalf("31 names: %d %s", r.status, r.body)
	}
	// Another person's columns are theirs: pat sees nothing of chen's choices, and a write by pat leaves chen's alone.
	pat := e.newHuman("pat", "pat@example.com", "member")
	patTok, _ := e.newToken(pat.ID, tokenOpts{pin: &e.orgID})
	if l := e.views(patTok); len(l) != 0 {
		t.Fatalf("pat sees %+v", l)
	}
	if v := e.do("PUT", vwPath+"/team:orb", patTok, map[string]any{"view": "board", "sort": "manual", "at": base + 20, "hideEmpty": false, "hiddenColumns": []string{"Mine"}}); v.status != 200 {
		t.Fatalf("pat put: %d", v.status)
	}
	for _, v := range e.views(e.chen) {
		if v.Place == "team:orb" && (len(v.HiddenColumns) != 30 || v.HideEmpty) {
			t.Fatalf("pat's write changed chen's row: %+v", v)
		}
	}
	if l := e.views(patTok); len(l) != 1 || len(l[0].HiddenColumns) != 1 || l[0].HiddenColumns[0] != "Mine" {
		t.Fatalf("pat's row %+v", l)
	}
}

// LAUN-171: the board's group-by field.
func TestViews_GroupBy(t *testing.T) {
	e := newEnv(t)
	base := time.Now().UnixMilli() - 100000
	put := func(at int64, body map[string]any) notify.ViewPreference {
		e.t.Helper()
		body["at"] = at
		var v notify.ViewPreference
		e.must(200, e.do("PUT", vwPath+"/team:orb", e.chen, body)).json(e.t, &v)
		return v
	}
	// Every value of the enum stores and reads back, with the column choices.
	for i, g := range []string{"assignee", "priority", "label", "project", "state"} {
		v := put(base+int64(i), map[string]any{"view": "board", "sort": "manual", "groupBy": g, "hiddenColumns": []string{g + ":x"}})
		if v.GroupBy != g || len(v.HiddenColumns) != 1 {
			t.Fatalf("%s: stored %+v", g, v)
		}
	}
	// An older write loses and does not change the grouping.
	put(base+10, map[string]any{"view": "board", "sort": "manual", "groupBy": "assignee"})
	if v := put(base+9, map[string]any{"view": "board", "sort": "manual", "groupBy": "label"}); v.GroupBy != "assignee" {
		t.Fatalf("older write changed groupBy: %+v", v)
	}
	// A newer write without the field is "state" (a PUT replaces the whole row).
	if v := put(base+11, map[string]any{"view": "board", "sort": "manual"}); v.GroupBy != "state" {
		t.Fatalf("absent groupBy = %q", v.GroupBy)
	}
	// A row from before migration 0025 reads as "state".
	e.exec(`INSERT INTO user_view_preferences (org_id, user_id, place, view, sort, changed_at)
	        SELECT org_id, user_id, 'all', 'board', 'manual', 5 FROM org_members LIMIT 1`)
	for _, v := range e.views(e.chen) {
		if v.GroupBy != "state" {
			t.Fatalf("old row %q: groupBy %q", v.Place, v.GroupBy)
		}
	}
	// The server validates the enum; a bad value changes nothing.
	for _, g := range []string{"team", "STATE", "state,assignee", " state"} {
		r := e.do("PUT", vwPath+"/team:orb", e.chen, map[string]any{"view": "board", "sort": "manual", "at": base + 20, "groupBy": g})
		if r.status != 400 || r.errCode(t) != "validation_failed" {
			t.Fatalf("groupBy %q: %d %s", g, r.status, r.body)
		}
	}
	// The column CHECK is the backstop.
	if _, err := e.pool.Exec(context.Background(), `UPDATE user_view_preferences SET group_by = 'cycle'`); err == nil {
		t.Fatal("the database accepted group_by = cycle")
	}
}
