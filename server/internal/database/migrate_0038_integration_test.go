package database_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"devops/internal/testdb"
)

// TestMigration0038_EventsScope checks 0038 (LAUN-221, T20 M2): an event made
// before the upgrade is 'legacy' with no teams after it (the feed hides it
// from a restricted member), the scope is CHECKed, a team scope needs a team,
// and the down drops both columns and keeps the row.
func TestMigration0038_EventsScope(t *testing.T) {
	dsn := testdb.Fresh(t)
	m := migratorFor(t, dsn)
	migrateTo(t, m, 37)
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
	if _, err := conn.Exec(ctx, `INSERT INTO events (org_id, seq, type, entity_type, entity_id) VALUES ($1, 1, 'issue.created', 'issue', uuidv7())`, org); err != nil {
		t.Fatal(err)
	}

	migrateTo(t, m, 38)

	if got := one(`SELECT scope || ':' || cardinality(team_ids) FROM events WHERE org_id = $1 AND seq = 1`, org); got != "legacy:0" {
		t.Errorf("an old event after up = %q, want legacy:0", got)
	}
	for _, bad := range []string{
		`INSERT INTO events (org_id, seq, type, entity_type, entity_id, scope) VALUES ($1, 2, 'x', 'issue', uuidv7(), 'bogus')`,
		`INSERT INTO events (org_id, seq, type, entity_type, entity_id, scope) VALUES ($1, 3, 'x', 'issue', uuidv7(), 'any')`,
		`INSERT INTO events (org_id, seq, type, entity_type, entity_id, scope) VALUES ($1, 4, 'x', 'issue', uuidv7(), 'all')`,
	} {
		if _, err := conn.Exec(ctx, bad, org); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
	if _, err := conn.Exec(ctx, `INSERT INTO events (org_id, seq, type, entity_type, entity_id, scope, team_ids)
		VALUES ($1, 5, 'x', 'issue', uuidv7(), 'any', ARRAY[uuidv7()])`, org); err != nil {
		t.Errorf("a team scope with a team was refused: %v", err)
	}

	migrateTo(t, m, 37)
	if got := one(`SELECT count(*)::text FROM information_schema.columns WHERE table_name = 'events' AND column_name IN ('scope', 'team_ids')`); got != "0" {
		t.Errorf("columns still there after down")
	}
	if got := one(`SELECT count(*)::text FROM events WHERE org_id = $1`, org); got != "2" {
		t.Errorf("events after down = %s, want 2 (rows are kept)", got)
	}
}
