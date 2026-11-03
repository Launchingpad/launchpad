package database_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"devops/internal/testdb"
)

// TestMigration0037_TeamAccess checks 0037 (LAUN-215): a member who exists
// before the upgrade is 'all' after it, the flag is CHECKed, and the down
// drops the column and the table.
func TestMigration0037_TeamAccess(t *testing.T) {
	dsn := testdb.Fresh(t)
	m := migratorFor(t, dsn)
	migrateTo(t, m, 36)
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
	u := one(`INSERT INTO users (username, name) VALUES ('one', 'one') RETURNING id::text`)
	if _, err := conn.Exec(ctx, `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, org, u); err != nil {
		t.Fatal(err)
	}

	migrateTo(t, m, 37)

	if got := one(`SELECT team_access FROM org_members WHERE org_id = $1 AND user_id = $2`, org, u); got != "all" {
		t.Errorf("team_access after up = %q, want all", got)
	}
	if _, err := conn.Exec(ctx, `UPDATE org_members SET team_access = 'bogus' WHERE org_id = $1`, org); err == nil {
		t.Error("bogus team_access accepted")
	}

	migrateTo(t, m, 36)
	if got := one(`SELECT count(*)::text FROM information_schema.columns WHERE table_name = 'org_members' AND column_name = 'team_access'`); got != "0" {
		t.Errorf("column still there after down")
	}
	if got := one(`SELECT count(*)::text FROM information_schema.tables WHERE table_name = 'team_members'`); got != "0" {
		t.Errorf("table still there after down")
	}
}
