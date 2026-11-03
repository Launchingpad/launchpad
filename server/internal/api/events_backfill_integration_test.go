package api

import (
	"context"
	"slices"
	"sort"
	"testing"

	"devops/internal/events"
	"devops/internal/testdb"
)

// TestEventsBackfill_RoundTrip checks the events backfill job (decisions-T20
// #57, ticket 6b) against Emit: the Test D writes make every kind through the
// routes; their rows are turned back into legacy rows (as before migration
// 0038); the job places them again. Each row gets the scope Emit gave it, or
// stays legacy when its subject is gone (never another scope, so the job never
// shows a row to more readers than Emit did). Before the job a restricted
// member reads none of the rows; after it, exactly the rows the #55 rule
// gives. The admin and the all-teams member read the same rows throughout.
func TestEventsBackfill_RoundTrip(t *testing.T) {
	f := newTeamFixture(t)
	e := f.e
	ctx := context.Background()
	f.makeEventKinds(t)
	emitted := f.eventRows()

	testdb.MustExec(t, e.pool, `UPDATE events SET scope = 'legacy', team_ids = '{}' WHERE org_id = $1`, e.orgID)
	admin, member := f.feedSeqs(e.chen), f.feedSeqs(f.member)
	if len(admin) != len(emitted) || !slices.Equal(admin, member) {
		t.Fatalf("before the job: the admin reads %d rows, the all-teams member %d, want %d each", len(admin), len(member), len(emitted))
	}
	if got := f.feedSeqs(f.restricted); len(got) != 0 {
		t.Fatalf("before the job the restricted member reads legacy rows %v", got)
	}

	res, locked, err := events.Backfill(ctx, e.pool, events.BackfillOptions{})
	if err != nil || !locked {
		t.Fatalf("backfill: locked %v, err %v", locked, err)
	}
	placed := f.eventRows()
	if len(placed) != len(emitted) || res.Scanned != int64(len(emitted)) {
		t.Fatalf("rows %d -> %d, scanned %d: the job must neither add nor drop a row", len(emitted), len(placed), res.Scanned)
	}

	same := map[string]bool{} // kinds with a row placed as Emit placed it, with HID
	legacyKinds := map[string]int{}
	var legacySeqs []int64
	for i, em := range emitted {
		pl := placed[i]
		if pl.seq != em.seq || pl.typ != em.typ {
			t.Fatalf("row %d: order changed", i)
		}
		if pl.scope == em.scope && slices.Equal(pl.teams, em.teams) {
			if slices.Contains(pl.teams, f.hidID) {
				same[pl.typ] = true
			}
			continue
		}
		if pl.scope != events.ScopeKindLegacy {
			t.Errorf("event %d %s placed %s %v, Emit gave %s %v", pl.seq, pl.typ, pl.scope, pl.teams, em.scope, em.teams)
			continue
		}
		legacyKinds[pl.typ]++
		legacySeqs = append(legacySeqs, pl.seq)
	}
	if res.Legacy != int64(len(legacySeqs)) || res.Placed != res.Scanned-res.Legacy {
		t.Errorf("result %+v, but %d rows stayed legacy", res, len(legacySeqs))
	}

	// The rows whose subject is gone, and why each stays legacy. A change here
	// is a change of the rules of #57: update the build notes.
	wantLegacy := map[string]string{
		events.TeamCreated:          "team GON was deleted: its team is gone",
		events.TeamDeleted:          "team GON was deleted: its team is gone",
		events.ProjectCreated:       "project 'Hidden plan two' was deleted: its teams are unknown",
		events.ProjectDeleted:       "the project is gone: its teams are unknown",
		events.RelationCreated:      "the edge to the deleted issue 'hidden two' went with it; one side's team is unknown",
		events.ProductUpdated:       "product three was deleted after its archive: its teams now are unknown",
		events.AgentGuidanceUpdated: "a team guidance event names a team key, not an id",
	}
	var gotKinds []string
	for k := range legacyKinds {
		gotKinds = append(gotKinds, k)
		if _, ok := wantLegacy[k]; !ok {
			t.Errorf("%d %s rows stayed legacy; the job should place them", legacyKinds[k], k)
		}
	}
	sort.Strings(gotKinds)
	for k, why := range wantLegacy {
		if legacyKinds[k] == 0 {
			t.Errorf("no %s row stayed legacy, want one (%s); kinds left legacy: %v", k, why, gotKinds)
		}
	}
	// Every kind made in HID whose subject still exists is placed as Emit placed it.
	for _, k := range hiddenKinds {
		switch k {
		case events.TeamDeleted, events.ProjectDeleted, events.AgentGuidanceUpdated:
			continue // only gone subjects, or a team key (above)
		}
		if !same[k] {
			t.Errorf("no %s row with HID was placed as Emit placed it", k)
		}
	}

	// The feed after the job.
	if got := f.feedSeqs(e.chen); !slices.Equal(got, admin) {
		t.Errorf("the admin's feed changed: %d rows, want %d", len(got), len(admin))
	}
	if got := f.feedSeqs(f.member); !slices.Equal(got, member) {
		t.Errorf("the all-teams member's feed changed: %d rows, want %d", len(got), len(member))
	}
	var want []int64
	for _, r := range placed {
		if r.visibleTo(f.orbID) {
			want = append(want, r.seq)
		}
	}
	got := f.feedSeqs(f.restricted)
	if !slices.Equal(got, want) {
		t.Errorf("the restricted member reads %v, want %v", got, want)
	}
	for _, s := range legacySeqs {
		if slices.Contains(got, s) {
			t.Errorf("the restricted member reads legacy row %d", s)
		}
	}
	if len(want) == 0 || len(want) == len(placed) {
		t.Fatalf("the restricted member should read some rows and not all: %d of %d", len(want), len(placed))
	}
	// A member of both teams reads the cross-team relation rows too, never a legacy one.
	testdb.PutInTeams(t, e.pool, e.orgID, f.restrictedID, f.hidID)
	both := f.feedSeqs(f.restricted)
	for _, r := range placed {
		if r.visibleTo(f.orbID, f.hidID) != slices.Contains(both, r.seq) {
			t.Errorf("a member of ORB and HID: event %d %s (%s %v) read %v", r.seq, r.typ, r.scope, r.teams, slices.Contains(both, r.seq))
		}
	}
}
