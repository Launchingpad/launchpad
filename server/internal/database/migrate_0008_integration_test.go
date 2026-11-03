package database_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"devops/internal/testdb"
)

// TestMigration0008_ManualOrderBackfill checks the backfill of
// issues.sort_order: per column, 1024 steps; Linear's order kept; equal
// ranks newest first; columns, teams and orgs do not mix. And the down.
func TestMigration0008_ManualOrderBackfill(t *testing.T) {
	dsn := testdb.Fresh(t)
	m := migratorFor(t, dsn)
	migrateTo(t, m, 7)

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	one := func(q string, args ...any) string {
		t.Helper()
		var s string
		if err := conn.QueryRow(ctx, q, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return s
	}
	org := one(`INSERT INTO orgs (slug, name) VALUES ('alpha-org', 'alpha') RETURNING id::text`)
	org2 := one(`INSERT INTO orgs (slug, name) VALUES ('beta-org', 'beta') RETURNING id::text`)
	team := func(o, key string) string {
		return one(`INSERT INTO teams (org_id, key, name) VALUES ($1, $2, $2) RETURNING id::text`, o, key)
	}
	state := func(o, tm, name string) string {
		return one(`INSERT INTO workflow_states (org_id, team_id, name, type) VALUES ($1, $2, $3, 'unstarted') RETURNING id::text`, o, tm, name)
	}
	n := 0
	issue := func(o, tm, st string, rank float64, createdDay int) string {
		n++
		return one(`INSERT INTO issues (org_id, team_id, number, title, state_id, sort_order, created_at)
			VALUES ($1, $2, $3, 'x', $4, $5, '2026-01-01'::timestamptz + make_interval(days => $6))
			RETURNING id::text`, o, tm, n, st, rank, createdDay)
	}
	t1, t2, t3 := team(org, "AAA"), team(org, "BBB"), team(org2, "CCC")
	todo, doing := state(org, t1, "Todo"), state(org, t1, "Doing")
	b1 := state(org, t2, "Todo")
	c1 := state(org2, t3, "Todo")

	// Column t1/todo: three never-ordered issues (0) made on days 1..3, and
	// two imported ones with Linear's ranks.
	old, mid, newest := issue(org, t1, todo, 0, 1), issue(org, t1, todo, 0, 2), issue(org, t1, todo, 0, 3)
	linA, linB := issue(org, t1, todo, -7.25, 0), issue(org, t1, todo, 300.5, 0)
	other := issue(org, t1, doing, 0, 1)
	otherTeam := issue(org, t2, b1, 99, 1)
	otherOrg := issue(org2, t3, c1, 0, 1)

	migrateTo(t, m, 8)
	rank := func(id string) string { return one(`SELECT sort_order::text FROM issues WHERE id = $1`, id) }
	// Ascending old ranks: -7.25, then the three zeros newest first, then 300.5.
	want := []struct{ id, rank string }{{linA, "1024"}, {newest, "2048"}, {mid, "3072"}, {old, "4096"}, {linB, "5120"}}
	for _, w := range want {
		if got := rank(w.id); got != w.rank {
			t.Errorf("rank of %s = %s, want %s", w.id, got, w.rank)
		}
	}
	for name, id := range map[string]string{"other state": other, "other team": otherTeam, "other org": otherOrg} {
		if got := rank(id); got != "1024" {
			t.Errorf("%s: rank %s, want 1024 (own column, first)", name, got)
		}
	}
	if got := one(`SELECT count(*)::text FROM pg_indexes WHERE indexname = 'issues_manual_order_idx'`); got != "1" {
		t.Fatal("index missing after up")
	}

	// Down drops the index and keeps the column (and the new ranks).
	migrateTo(t, m, 7)
	if got := one(`SELECT count(*)::text FROM pg_indexes WHERE indexname = 'issues_manual_order_idx'`); got != "0" {
		t.Fatal("index left after down")
	}
	if got := one(`SELECT count(*)::text FROM information_schema.columns WHERE table_name = 'issues' AND column_name = 'sort_order'`); got != "1" {
		t.Fatal("down dropped the 0001 column")
	}
	migrateTo(t, m, 8) // and up again: renumbering twice is stable
	if got := rank(linA) + "," + rank(linB); !strings.HasPrefix(got, "1024,") || got != "1024,5120" {
		t.Fatalf("second up changed ranks: %s", got)
	}
}
