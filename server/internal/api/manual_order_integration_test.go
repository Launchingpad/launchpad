package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"devops/internal/tracker"
)

// LAUN-51: manual issue order. Rules: docs/internal/decisions-T14-manual-order.md.

// column lists a column in manual order, as identifiers.
func (e *env) column(token, team, state string) []tracker.Issue {
	e.t.Helper()
	var p page[tracker.Issue]
	e.must(200, e.do("GET", O+"/issues?team="+team+"&state="+url.QueryEscape(state)+"&sort=manual&limit=250", token, nil)).json(e.t, &p)
	return p.Items
}

func (e *env) colIDs(token, team, state string) []string {
	return identifiers(e.column(token, team, state))
}

func (e *env) place(token, issue string, body map[string]any) tracker.Issue {
	e.t.Helper()
	var is tracker.Issue
	e.must(200, e.do("PATCH", O+"/issues/"+issue, token, body)).json(e.t, &is)
	return is
}

// backlog makes n issues in order; each goes on top, so the column reads
// last-made first.
func (e *env) backlog(n int, state string) []tracker.Issue {
	e.t.Helper()
	var out []tracker.Issue
	for i := 0; i < n; i++ {
		out = append(out, e.createIssue(e.chen, map[string]any{"title": fmt.Sprintf("card %d", i+1), "state": state}))
	}
	return out
}

func wantOrder(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("column %v, want %v", got, want)
	}
}

func TestManualOrder_NewIssueGoesOnTop(t *testing.T) {
	e := newEnv(t)
	is := e.backlog(3, "Todo")
	got := e.column(e.chen, "ORB", "Todo")
	wantOrder(t, identifiers(got), is[2].Identifier, is[1].Identifier, is[0].Identifier)
	if !(got[0].SortOrder < got[1].SortOrder && got[1].SortOrder < got[2].SortOrder) {
		t.Fatalf("ranks %v %v %v do not ascend", got[0].SortOrder, got[1].SortOrder, got[2].SortOrder)
	}
	// issue.created carries the rank and the state, so a client can place the card.
	cr := e.eventsOfType(e.chen, "issue.created")
	if len(cr) != 3 || !strings.Contains(string(cr[2].Payload), fmt.Sprintf(`"sortOrder":%v`, got[0].SortOrder)) ||
		!strings.Contains(string(cr[2].Payload), `"stateId":"`+got[0].State.ID.String()+`"`) {
		t.Fatalf("issue.created payloads: %+v", cr)
	}
	// Another column is its own list; the issue read carries the rank too.
	e.backlog(1, "In Progress")
	if len(e.column(e.chen, "ORB", "Todo")) != 3 {
		t.Fatal("a card of another column is in this one")
	}
	var one tracker.Issue
	e.must(200, e.do("GET", O+"/issues/"+is[0].Identifier, e.chen, nil)).json(t, &one)
	if one.SortOrder != got[2].SortOrder {
		t.Fatalf("GET issue sortOrder %v, list %v", one.SortOrder, got[2].SortOrder)
	}
}

func TestManualOrder_PlaceNeighbours(t *testing.T) {
	e := newEnv(t)
	is := e.backlog(5, "Todo") // column: 5 4 3 2 1
	id := func(n int) string { return is[n-1].Identifier }
	// after the middle one
	moved := e.place(e.chen, id(1), map[string]any{"placeAfter": id(4)})
	wantOrder(t, e.colIDs(e.chen, "ORB", "Todo"), id(5), id(4), id(1), id(3), id(2))
	if moved.SortOrder == is[0].SortOrder {
		t.Fatal("the response has the old sortOrder")
	}
	// before the first: the top
	e.place(e.chen, id(2), map[string]any{"placeBefore": id(5)})
	wantOrder(t, e.colIDs(e.chen, "ORB", "Todo"), id(2), id(5), id(4), id(1), id(3))
	// after the last: the bottom
	e.place(e.chen, id(5), map[string]any{"placeAfter": id(3)})
	wantOrder(t, e.colIDs(e.chen, "ORB", "Todo"), id(2), id(4), id(1), id(3), id(5))
	// before the last, by uuid
	e.place(e.chen, id(2), map[string]any{"placeBefore": is[4].ID.String()})
	wantOrder(t, e.colIDs(e.chen, "ORB", "Todo"), id(4), id(1), id(3), id(2), id(5))
	// a number
	e.place(e.chen, id(5), map[string]any{"sortOrder": -1e6})
	wantOrder(t, e.colIDs(e.chen, "ORB", "Todo"), id(5), id(4), id(1), id(3), id(2))
	// the same place again changes nothing: no version bump, no event
	before := e.eventCount("issue.updated")
	cur := e.place(e.chen, id(5), map[string]any{"sortOrder": -1e6})
	if e.eventCount("issue.updated") != before {
		t.Fatal("an unchanged placement wrote an event")
	}
	again := e.place(e.chen, id(5), map[string]any{"sortOrder": -1e6})
	if again.Version != cur.Version {
		t.Fatal("an unchanged placement bumped the version")
	}
	// the event
	evs := e.eventsOfType(e.chen, "issue.updated")
	last := evs[len(evs)-1]
	var pl struct {
		Changes   []string `json:"changes"`
		SortOrder *float64 `json:"sortOrder"`
		StateID   string   `json:"stateId"`
	}
	_ = json.Unmarshal(last.Payload, &pl)
	if !slices.Equal(pl.Changes, []string{"sortOrder"}) || pl.SortOrder == nil || *pl.SortOrder != -1e6 || pl.StateID != is[0].State.ID.String() {
		t.Fatalf("event payload %s", last.Payload)
	}
}

func TestManualOrder_StateAndPlaceAreOneUpdate(t *testing.T) {
	e := newEnv(t)
	todo := e.backlog(3, "Todo")         // 3 2 1
	doing := e.backlog(2, "In Progress") // 2 1
	card := todo[1]
	r := e.place(e.chen, card.Identifier, map[string]any{"state": "In Progress", "placeAfter": doing[1].Identifier})
	if r.State.Name != "In Progress" {
		t.Fatalf("state %s", r.State.Name)
	}
	wantOrder(t, e.colIDs(e.chen, "ORB", "In Progress"), doing[1].Identifier, card.Identifier, doing[0].Identifier)
	wantOrder(t, e.colIDs(e.chen, "ORB", "Todo"), todo[2].Identifier, todo[0].Identifier)
	evs := e.eventsOfType(e.chen, "issue.updated")
	var pl struct {
		Changes []string `json:"changes"`
	}
	_ = json.Unmarshal(evs[len(evs)-1].Payload, &pl)
	if len(evs) != 1 || !slices.Equal(pl.Changes, []string{"state", "sortOrder"}) {
		t.Fatalf("%d events, last %s", len(evs), evs[len(evs)-1].Payload)
	}
	if n := e.countRows(`SELECT count(*) FROM issue_state_changes WHERE issue_id = $1 AND source = 'api'`, card.ID); n != 2 {
		t.Fatalf("%d history rows, want create + move", n)
	}

	// Atomic: a bad neighbour with a state change changes nothing.
	before := e.eventCount("issue.updated")
	expectCode(t, e.do("PATCH", O+"/issues/"+todo[2].Identifier, e.chen,
		map[string]any{"state": "Done", "placeAfter": doing[0].Identifier}), 409, "stale_placement")
	var cur tracker.Issue
	e.must(200, e.do("GET", O+"/issues/"+todo[2].Identifier, e.chen, nil)).json(t, &cur)
	if cur.State.Name != "Todo" || e.eventCount("issue.updated") != before {
		t.Fatalf("a refused move left state %s, events %d -> %d", cur.State.Name, before, e.eventCount("issue.updated"))
	}

	// A state change alone puts the card on top of the new column.
	r = e.place(e.chen, todo[0].Identifier, map[string]any{"state": "In Progress"})
	wantOrder(t, e.colIDs(e.chen, "ORB", "In Progress"), todo[0].Identifier, doing[1].Identifier, card.Identifier, doing[0].Identifier)
	// Placement in the new column: before the first.
	e.place(e.chen, todo[2].Identifier, map[string]any{"state": "In Progress", "placeBefore": todo[0].Identifier})
	if ids := e.colIDs(e.chen, "ORB", "In Progress"); ids[0] != todo[2].Identifier {
		t.Fatalf("column %v", ids)
	}
	// A version check still works with a placement.
	expectCode(t, e.do("PATCH", O+"/issues/"+card.Identifier, e.chen,
		map[string]any{"version": 1, "placeBefore": doing[0].Identifier}), 409, "version_conflict")
}

func TestManualOrder_PlacementIsRefused(t *testing.T) {
	e := newEnv(t)
	is := e.backlog(2, "Todo")
	other := e.backlog(1, "In Progress")[0]
	// A second team in orbit, and a second org.
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tracker.CreateTeamTx(ctx, e.q.WithTx(tx), e.orgID, "XYZ", "Xyz", ""); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	xyz := e.createIssue(e.chen, map[string]any{"team": "XYZ", "title": "other team", "state": "Todo"})
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "admin")
	tok, _ := e.newToken(chen.ID, tokenOpts{})
	var acme tracker.Issue
	e.must(201, e.do("POST", "/api/v1/orgs/acme/issues", tok, map[string]any{"team": "ACM", "title": "acme card"})).json(t, &acme)

	a := is[0].Identifier
	// Malformed input: validation_failed.
	for name, body := range map[string]map[string]any{
		"other team":         {"placeAfter": xyz.Identifier},
		"other team by id":   {"placeBefore": xyz.ID.String()},
		"junk":               {"placeBefore": "not an issue"},
		"itself":             {"placeAfter": a},
		"two neighbours":     {"placeAfter": is[1].Identifier, "placeBefore": a},
		"rank and neighbour": {"sortOrder": 5, "placeAfter": is[1].Identifier},
		"huge rank":          {"sortOrder": 1e15},
	} {
		t.Run(name, func(t *testing.T) {
			expectCode(t, e.do("PATCH", O+"/issues/"+a, e.chen, body), 400, "validation_failed")
		})
	}
	// A neighbour that is unknown, of another org (not told apart from an
	// unknown one) or in another column is stale_placement, 409.
	for name, body := range map[string]map[string]any{
		"other org by id":         {"placeAfter": acme.ID.String()},
		"other org by identifier": {"placeAfter": acme.Identifier},
		"unknown":                 {"placeAfter": uuid.NewString()},
		"unknown identifier":      {"placeBefore": "ORB-9999"},
		"another column":          {"placeAfter": other.Identifier},
		"another column + state":  {"state": "Done", "placeAfter": other.Identifier},
	} {
		t.Run(name, func(t *testing.T) {
			expectCode(t, e.do("PATCH", O+"/issues/"+a, e.chen, body), 409, "stale_placement")
		})
	}
	expectCode(t, e.do("PATCH", O+"/issues/"+a, e.chen, map[string]any{"sortOrder": "high"}), 400, "bad_request")
	// An issue of another org is 404 to the PATCH itself, whatever it names.
	expectCode(t, e.do("PATCH", "/api/v1/orgs/acme/issues/"+a, tok, map[string]any{"placeAfter": acme.Identifier}), 404, "not_found")
	expectCode(t, e.do("PATCH", "/api/v1/orgs/acme/issues/"+is[0].ID.String(), tok, map[string]any{"sortOrder": 1}), 404, "not_found")
	// And acme's PATCH cannot name an orbit neighbour.
	expectCode(t, e.do("PATCH", "/api/v1/orgs/acme/issues/"+acme.Identifier, tok,
		map[string]any{"placeAfter": is[0].ID.String()}), 409, "stale_placement")
	// A reader cannot place.
	ro, _ := e.newToken(chen.ID, tokenOpts{scopes: []string{"read"}})
	expectCode(t, e.do("PATCH", O+"/issues/"+a, ro, map[string]any{"sortOrder": 1}), 403, "insufficient_scope")
	// Nothing changed.
	wantOrder(t, e.colIDs(e.chen, "ORB", "Todo"), is[1].Identifier, is[0].Identifier)
}

func TestManualOrder_RebalanceKeepsTheOrder(t *testing.T) {
	e := newEnv(t)
	is := e.backlog(4, "Todo") // 4 3 2 1
	id := func(n int) string { return is[n-1].Identifier }
	// Drop card 1 right after card 4, then each next card right after card 4
	// again: every drop halves the same gap. After 25 drops a rebalance must
	// have happened and the order must be exactly the reverse drop order.
	want := []string{id(4)}
	var rebalanced int
	for i := 0; i < 25; i++ {
		n := e.createIssue(e.chen, map[string]any{"title": fmt.Sprintf("drop %d", i), "state": "Todo"})
		e.place(e.chen, n.Identifier, map[string]any{"placeAfter": id(4)})
		want = append([]string{id(4)}, append([]string{n.Identifier}, want[1:]...)...)
		for _, ev := range e.eventsOfType(e.chen, "issue.updated") {
			if strings.Contains(string(ev.Payload), `"rebalanced":true`) {
				rebalanced++
				break
			}
		}
	}
	got := e.colIDs(e.chen, "ORB", "Todo")
	wantAll := append(append([]string{}, want...), id(3), id(2), id(1))
	wantOrder(t, got, wantAll...)
	col := e.column(e.chen, "ORB", "Todo")
	for i := 1; i < len(col); i++ {
		if !(col[i-1].SortOrder < col[i].SortOrder) {
			t.Fatalf("ranks %v and %v at %d are not strictly ascending", col[i-1].SortOrder, col[i].SortOrder, i)
		}
	}
	if rebalanced == 0 {
		t.Fatal("25 drops into one gap never renumbered the column")
	}
	// A rebalance leaves version and updated_at of the other cards alone.
	var v int32
	if err := e.pool.QueryRow(context.Background(), `SELECT version FROM issues WHERE org_id = $1 AND id = $2`, e.orgID, is[0].ID).Scan(&v); err != nil || v != 1 {
		t.Fatalf("an untouched card has version %d (%v)", v, err)
	}
	// The same drop into the front: before the first, 25 times.
	for i := 0; i < 25; i++ {
		n := e.createIssue(e.chen, map[string]any{"title": fmt.Sprintf("front %d", i), "state": "Todo"})
		e.place(e.chen, n.Identifier, map[string]any{"placeBefore": col[1].Identifier})
	}
	col = e.column(e.chen, "ORB", "Todo")
	for i := 1; i < len(col); i++ {
		if !(col[i-1].SortOrder < col[i].SortOrder) {
			t.Fatalf("after front drops: ranks %v and %v at %d", col[i-1].SortOrder, col[i].SortOrder, i)
		}
	}
}

func TestManualOrder_EqualRanksAreRepaired(t *testing.T) {
	e := newEnv(t)
	is := e.backlog(3, "Todo")
	// Force a tie by hand (an old import could have one).
	if _, err := e.pool.Exec(context.Background(), `UPDATE issues SET sort_order = 7 WHERE org_id = $1`, e.orgID); err != nil {
		t.Fatal(err)
	}
	e.place(e.chen, is[0].Identifier, map[string]any{"placeAfter": is[1].Identifier})
	ids := e.colIDs(e.chen, "ORB", "Todo")
	pos := slices.Index(ids, is[0].Identifier)
	if pos != slices.Index(ids, is[1].Identifier)+1 {
		t.Fatalf("column %v: %s is not right after %s", ids, is[0].Identifier, is[1].Identifier)
	}
}

func TestManualOrder_Pagination(t *testing.T) {
	e := newEnv(t)
	e.backlog(9, "Todo")
	full := e.colIDs(e.chen, "ORB", "Todo")
	// Ties: five cards share a rank, so only the id keeps pages stable.
	if _, err := e.pool.Exec(context.Background(), `UPDATE issues SET sort_order = 42 WHERE org_id = $1
		AND number IN (2, 3, 4, 5, 6)`, e.orgID); err != nil {
		t.Fatal(err)
	}
	full = e.colIDs(e.chen, "ORB", "Todo")
	var got []string
	cursor := ""
	pages := 0
	for {
		path := O + "/issues?team=ORB&state=Todo&sort=manual&limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		var p page[tracker.Issue]
		e.must(200, e.do("GET", path, e.chen, nil)).json(t, &p)
		got = append(got, identifiers(p.Items)...)
		pages++
		if p.NextCursor == nil {
			break
		}
		cursor = *p.NextCursor
	}
	if pages != 5 || !slices.Equal(got, full) {
		t.Fatalf("%d pages gave %v, want %v", pages, got, full)
	}
	// A card moved between pages does not break the cursor.
	var p page[tracker.Issue]
	e.must(200, e.do("GET", O+"/issues?sort=manual&team=ORB&state=Todo&limit=3", e.chen, nil)).json(t, &p)
	e.place(e.chen, p.Items[0].Identifier, map[string]any{"placeAfter": full[8]})
	var p2 page[tracker.Issue]
	e.must(200, e.do("GET", O+"/issues?sort=manual&team=ORB&state=Todo&limit=3&cursor="+*p.NextCursor, e.chen, nil)).json(t, &p2)
	wantOrder(t, identifiers(p2.Items), full[3], full[4], full[5])
	// The cursor is tied to the order.
	expectCode(t, e.do("GET", O+"/issues?team=ORB&cursor="+*p.NextCursor, e.chen, nil), 400, "validation_failed")
	var u page[tracker.Issue]
	e.must(200, e.do("GET", O+"/issues?team=ORB&limit=2", e.chen, nil)).json(t, &u)
	expectCode(t, e.do("GET", O+"/issues?sort=manual&team=ORB&cursor="+*u.NextCursor, e.chen, nil), 400, "validation_failed")
	expectCode(t, e.do("GET", O+"/issues?sort=newest", e.chen, nil), 400, "validation_failed")
	expectCode(t, e.do("GET", O+"/issues?sort=manual&orderBy=createdAt", e.chen, nil), 400, "validation_failed")
	// orderBy=manual is the same list.
	wantOrder(t, identifiers(func() []tracker.Issue {
		var q page[tracker.Issue]
		e.must(200, e.do("GET", O+"/issues?orderBy=manual&team=ORB&state=Todo&limit=250", e.chen, nil)).json(t, &q)
		return q.Items
	}()), e.colIDs(e.chen, "ORB", "Todo")...)
}

// PATCH has no Idempotency-Key middleware (as for every PATCH). A retried
// drag is still harmless: the same placement twice changes nothing.
func TestManualOrder_RepeatedPlacementIsANoOp(t *testing.T) {
	e := newEnv(t)
	is := e.backlog(4, "Todo") // 4 3 2 1
	first := e.place(e.chen, is[0].Identifier, map[string]any{"placeAfter": is[3].Identifier})
	events := e.eventCount("issue.updated")
	for _, body := range []map[string]any{{"placeAfter": is[3].Identifier}, {"placeBefore": is[2].Identifier}} {
		again := e.place(e.chen, is[0].Identifier, body)
		if again.SortOrder != first.SortOrder || again.Version != first.Version {
			t.Fatalf("%v moved the card: rank %v -> %v, version %d -> %d", body, first.SortOrder, again.SortOrder, first.Version, again.Version)
		}
	}
	if e.eventCount("issue.updated") != events {
		t.Fatal("a repeated placement wrote an event")
	}
	wantOrder(t, e.colIDs(e.chen, "ORB", "Todo"), is[3].Identifier, is[0].Identifier, is[2].Identifier, is[1].Identifier)
}

func TestManualOrder_ConcurrentDrags(t *testing.T) {
	e := newEnv(t)
	is := e.backlog(6, "Todo") // 6 5 4 3 2 1
	var wg sync.WaitGroup
	// Three people drop three different cards after the same neighbour.
	for _, n := range []int{1, 2, 3} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := e.request("PATCH", O+"/issues/"+is[n-1].Identifier, e.chen,
				map[string]any{"placeAfter": is[5].Identifier})
			if err != nil || r.status != 200 {
				t.Errorf("drag %d: %v %d %s", n, err, r.status, r.body)
			}
		}()
	}
	wg.Wait()
	col := e.column(e.chen, "ORB", "Todo")
	if len(col) != 6 || col[0].Identifier != is[5].Identifier {
		t.Fatalf("column %v", identifiers(col))
	}
	seen := map[float64]bool{}
	for _, c := range col {
		if seen[c.SortOrder] {
			t.Fatalf("two cards share rank %v: %v", c.SortOrder, identifiers(col))
		}
		seen[c.SortOrder] = true
	}
	// The three dropped cards sit directly under card 6; card 5 and 4 follow.
	tail := identifiers(col)[4:]
	wantOrder(t, tail, is[4].Identifier, is[3].Identifier)
}

// tied makes cards x and y share rank r by hand (an old import could have
// ties). It returns them in column order (smaller id first).
func (e *env) tied(r float64, x, y tracker.Issue) (first, second tracker.Issue) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE issues SET sort_order = $2 WHERE org_id = $1 AND id = ANY($3)`,
		e.orgID, r, []uuid.UUID{x.ID, y.ID}); err != nil {
		e.t.Fatal(err)
	}
	if x.ID.String() < y.ID.String() {
		return x, y
	}
	return y, x
}

func (e *env) setRank(is tracker.Issue, r float64) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE issues SET sort_order = $3 WHERE org_id = $1 AND id = $2`, e.orgID, is.ID, r); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) assertAscending(token, team, state string) {
	e.t.Helper()
	col := e.column(token, team, state)
	for i := 1; i < len(col); i++ {
		if !(col[i-1].SortOrder < col[i].SortOrder) {
			e.t.Fatalf("ranks %v and %v at %d are not strictly ascending", col[i-1].SortOrder, col[i].SortOrder, i)
		}
	}
}

// A rebalance whose own result equals the old rank still writes the row,
// bumps the version and emits the event (review of LAUN-51, finding 1).
func TestManualOrder_RebalanceWithTheSameRankStillEmits(t *testing.T) {
	e := newEnv(t)
	is := e.backlog(3, "Todo")
	x, y := e.tied(7, is[0], is[1])
	cur := is[2]
	e.setRank(cur, 1536) // the rank the placement will compute after the renumber
	before := e.eventCount("issue.updated")
	r := e.place(e.chen, cur.Identifier, map[string]any{"placeAfter": x.Identifier})
	if !r.Rebalanced || r.SortOrder != 1536 || r.Version != 2 {
		t.Fatalf("response: rebalanced %v sortOrder %v version %d", r.Rebalanced, r.SortOrder, r.Version)
	}
	if e.eventCount("issue.updated") != before+1 {
		t.Fatal("a rebalance that kept this card's rank wrote no event")
	}
	evs := e.eventsOfType(e.chen, "issue.updated")
	var pl struct {
		Changes    []string `json:"changes"`
		Rebalanced bool     `json:"rebalanced"`
		StateID    string   `json:"stateId"`
		SortOrder  *float64 `json:"sortOrder"`
	}
	_ = json.Unmarshal(evs[len(evs)-1].Payload, &pl)
	if !pl.Rebalanced || pl.StateID != cur.State.ID.String() || pl.SortOrder == nil || !slices.Contains(pl.Changes, "sortOrder") {
		t.Fatalf("payload %s", evs[len(evs)-1].Payload)
	}
	wantOrder(t, e.colIDs(e.chen, "ORB", "Todo"), x.Identifier, cur.Identifier, y.Identifier)
	e.assertAscending(e.chen, "ORB", "Todo")
	// A plain placement has no signal.
	if again := e.place(e.chen, cur.Identifier, map[string]any{"placeBefore": x.Identifier}); again.Rebalanced {
		t.Fatal("a placement with room says rebalanced")
	}
}

// A state change and a rebalance of the destination column in one update.
func TestManualOrder_StateChangeWithRebalance(t *testing.T) {
	e := newEnv(t)
	doing := e.backlog(2, "In Progress")
	todo := e.backlog(2, "Todo")
	x, y := e.tied(7, doing[0], doing[1])
	r := e.place(e.chen, todo[0].Identifier, map[string]any{"state": "In Progress", "placeAfter": x.Identifier})
	if !r.Rebalanced || r.State.Name != "In Progress" {
		t.Fatalf("rebalanced %v state %s", r.Rebalanced, r.State.Name)
	}
	wantOrder(t, e.colIDs(e.chen, "ORB", "In Progress"), x.Identifier, todo[0].Identifier, y.Identifier)
	e.assertAscending(e.chen, "ORB", "In Progress")
	wantOrder(t, e.colIDs(e.chen, "ORB", "Todo"), todo[1].Identifier)
	evs := e.eventsOfType(e.chen, "issue.updated")
	last := string(evs[len(evs)-1].Payload)
	if !strings.Contains(last, `"rebalanced":true`) || !strings.Contains(last, `"stateId":"`+r.State.ID.String()+`"`) {
		t.Fatalf("payload %s", last)
	}
}

// A plain state change (the MCP / status picker path) and a GitLab move both
// land on top of a column that has just been renumbered; and hooks racing
// with rebalancing drags finish without a deadlock.
func TestManualOrder_TopPlacementAfterRebalanceAndGitlab(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	review := e.backlog(2, "In Review")
	x, _ := e.tied(7, review[0], review[1])
	mover := e.createIssue(e.chen, map[string]any{"title": "Fix login redirect", "state": "Todo"})
	other := e.createIssue(e.chen, map[string]any{"title": "other", "state": "Todo"})
	e.place(e.chen, other.Identifier, map[string]any{"state": "In Review", "placeAfter": x.Identifier}) // rebalances
	e.assertAscending(e.chen, "ORB", "In Review")
	// plain state change
	plain := e.createIssue(e.chen, map[string]any{"title": "plain", "state": "Todo"})
	e.place(e.chen, plain.Identifier, map[string]any{"state": "In Review"})
	if ids := e.colIDs(e.chen, "ORB", "In Review"); ids[0] != plain.Identifier || len(ids) != 4 {
		t.Fatalf("column %v", ids)
	}
	// GitLab: MR opened moves Todo -> In Review, on top
	r := e.hookResult(e.hook("orbit", glSecret, uuid.NewString(), glFixture(t, "mr_open.json", naming(mover.Identifier, ""))), 200)
	if len(r.Moved) != 1 {
		t.Fatalf("result %+v", r)
	}
	if ids := e.colIDs(e.chen, "ORB", "In Review"); ids[0] != mover.Identifier || len(ids) != 5 {
		t.Fatalf("column after the gitlab move %v", ids)
	}
	e.assertAscending(e.chen, "ORB", "In Review")
}

// The GitLab move holds the team's rank lock, so hooks that move cards into
// one column at the same time get DIFFERENT top ranks (without the lock they
// read the same minimum and tie), while rebalancing drags run in the column
// the cards leave. The ties of the drag column are set up BEFORE any
// goroutine starts (no concurrent raw writes); the rebalance itself locks
// its rows in id order. Any error, a 40P01 deadlock included, fails the test.
func TestManualOrder_HooksTakeDistinctTopRanksAndDoNotDeadlock(t *testing.T) {
	e := newEnv(t)
	e.glSetup("orbit", e.chen)
	const n = 8
	var movers []tracker.Issue
	for i := 0; i < n; i++ {
		movers = append(movers, e.createIssue(e.chen, map[string]any{"title": fmt.Sprintf("m%d", i), "state": "Todo"}))
	}
	cards := e.backlog(4, "Todo")
	a, _ := e.tied(5, cards[0], cards[1])
	bodies := make([][]byte, n)
	for i, m := range movers {
		bodies[i] = glFixture(t, "mr_open.json", naming(m.Identifier, ""))
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range movers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if r := e.hook("orbit", glSecret, uuid.NewString(), bodies[i]); r.status != 200 {
				t.Errorf("hook: %d %s", r.status, r.body)
			}
		}(i)
	}
	for _, c := range []tracker.Issue{cards[2], cards[3]} {
		wg.Add(1)
		go func(c tracker.Issue) {
			defer wg.Done()
			<-start
			r, err := e.request("PATCH", O+"/issues/"+c.Identifier, e.chen, map[string]any{"placeAfter": a.Identifier})
			if err != nil || r.status != 200 {
				t.Errorf("drag: %v %d %s", err, r.status, r.body)
			}
		}(c)
	}
	close(start)
	wg.Wait()
	col := e.column(e.chen, "ORB", "In Review")
	if len(col) != n {
		t.Fatalf("%d cards in In Review, want %d", len(col), n)
	}
	seen := map[float64]bool{}
	for _, c := range col {
		if seen[c.SortOrder] {
			t.Fatalf("two moved cards share the top rank %v", c.SortOrder)
		}
		seen[c.SortOrder] = true
	}
	// Strictly ascending in both columns: distinct ranks, no ties at all.
	e.assertAscending(e.chen, "ORB", "In Review")
	e.assertAscending(e.chen, "ORB", "Todo")
}
