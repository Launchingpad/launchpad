package database_test

import (
	"context"
	"testing"

	"devops/internal/database"
	"devops/internal/testdb"
)

// TestMigration0027_ProjectStatuses: old-style projects (an enum state, no status) get the org's status of the same
// kind; a new org gets the six defaults from the trigger; the composite key keeps state and status equal; the down
// keeps the projects and their state (LAUN-195, decisions-T29).
func TestMigration0027_ProjectStatuses(t *testing.T) {
	dsn := testdb.Fresh(t)
	m := migratorFor(t, dsn)
	migrateTo(t, m, 25)
	c := backupConn(t, dsn)
	ctx := context.Background()

	const orgA = "0192f000-0000-7000-8000-0000000000a1"
	const orgB = "0192f000-0000-7000-8000-0000000000b1"
	mustExec(t, c, `INSERT INTO orgs (id, slug, name) VALUES ($1, 'oa', 'OA'), ($2, 'ob', 'OB')`, orgA, orgB)
	kinds := []string{"backlog", "planned", "started", "paused", "completed", "canceled"}
	for i, k := range kinds {
		mustExec(t, c, `INSERT INTO projects (org_id, name, state) VALUES ($1, $2, $3)`, orgA, "pa-"+k, k)
		if i < 2 {
			mustExec(t, c, `INSERT INTO projects (org_id, name, state) VALUES ($1, $2, $3)`, orgB, "pb-"+k, k)
		}
	}

	migrateTo(t, m, 27)
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := c.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Both existing orgs got the six defaults, one per kind.
	for _, o := range []string{orgA, orgB} {
		if n := count(`SELECT count(DISTINCT kind) FROM project_statuses WHERE org_id = $1`, o); n != 6 {
			t.Fatalf("org %s: %d kinds seeded", o, n)
		}
	}
	// Every project got the status of its own org and kind (the default name shows the same label as before).
	if n := count(`SELECT count(*) FROM projects p JOIN project_statuses s ON s.org_id = p.org_id AND s.id = p.status_id AND s.kind = p.state`); n != 8 {
		t.Fatalf("%d projects backfilled, want 8", n)
	}
	if n := count(`SELECT count(*) FROM projects p JOIN project_statuses s ON s.id = p.status_id WHERE p.state = 'started' AND s.name = 'In progress'`); n != 1 {
		t.Fatal("started project is not on In progress")
	}
	// A new org (any code path) gets the defaults from the trigger.
	mustExec(t, c, `INSERT INTO orgs (slug, name) VALUES ('oc', 'OC')`)
	if n := count(`SELECT count(*) FROM project_statuses s JOIN orgs o ON o.id = s.org_id WHERE o.slug = 'oc'`); n != 6 {
		t.Fatalf("new org has %d statuses", n)
	}
	// The key keeps state and status in step, and a status of another org is refused.
	var startedA, startedB string
	if err := c.QueryRow(ctx, `SELECT id::text FROM project_statuses WHERE org_id = $1 AND kind = 'started'`, orgA).Scan(&startedA); err != nil {
		t.Fatal(err)
	}
	if err := c.QueryRow(ctx, `SELECT id::text FROM project_statuses WHERE org_id = $1 AND kind = 'started'`, orgB).Scan(&startedB); err != nil {
		t.Fatal(err)
	}
	refuse(t, c, "state/status mismatch", database.CodeForeignKeyViolation,
		`UPDATE projects SET status_id = $1 WHERE org_id = $2 AND name = 'pa-backlog'`, startedA, orgA)
	refuse(t, c, "other org's status", database.CodeForeignKeyViolation,
		`UPDATE projects SET status_id = $1, state = 'started' WHERE org_id = $2 AND name = 'pa-backlog'`, startedB, orgA)
	refuse(t, c, "duplicate name, any case", database.CodeUniqueViolation,
		`INSERT INTO project_statuses (org_id, kind, name, color, position) VALUES ($1, 'started', 'IN PROGRESS', '#000000', 9)`, orgA)
	refuse(t, c, "bad kind", database.CodeCheckViolation,
		`INSERT INTO project_statuses (org_id, kind, name, color, position) VALUES ($1, 'done', 'Done', '#000000', 9)`, orgA)
	refuse(t, c, "bad colour", database.CodeCheckViolation,
		`INSERT INTO project_statuses (org_id, kind, name, color, position) VALUES ($1, 'started', 'X', 'red', 9)`, orgA)
	refuse(t, c, "status in use", database.CodeForeignKeyViolation,
		`DELETE FROM project_statuses WHERE id = $1`, startedA)

	// Down: the projects and their state stay; the status table and trigger go. Up again backfills again.
	migrateTo(t, m, 25)
	if n := count(`SELECT count(*) FROM projects WHERE state IN ('started','canceled')`); n != 2 {
		t.Fatalf("down lost projects: %d", n)
	}
	if n := count(`SELECT count(*) FROM pg_tables WHERE tablename = 'project_statuses'`); n != 0 {
		t.Fatal("table left")
	}
	migrateTo(t, m, 27)
	if n := count(`SELECT count(*) FROM projects p JOIN project_statuses s ON s.id = p.status_id AND s.kind = p.state`); n != 8 {
		t.Fatalf("second up backfilled %d, want 8", n)
	}
}
