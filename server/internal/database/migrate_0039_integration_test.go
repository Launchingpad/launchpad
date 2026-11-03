package database_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"devops/internal/testdb"
)

// TestMigration0039_MailAccessRemoved checks 0039 (LAUN-223, T20 M2b): before
// it the skip reason access_removed is refused, after it a row may carry it,
// and the down rewrites such a row to disabled (the nearest old reason) and
// puts the old CHECK back. Foreign keys are off on this connection
// (session_replication_role), so the rows need no org, member or project;
// the CHECKs still run.
func TestMigration0039_MailAccessRemoved(t *testing.T) {
	dsn := testdb.Fresh(t)
	m := migratorFor(t, dsn)
	migrateTo(t, m, 38)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, `SET session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	insert := func(reason string) error {
		_, err := conn.Exec(ctx, `INSERT INTO notification_emails (org_id, notification_id, recipient_id, project_id, state, skip_reason)
			VALUES (uuidv7(), uuidv7(), uuidv7(), uuidv7(), 'skipped', $1)`, reason)
		return err
	}
	if err := insert("access_removed"); err == nil {
		t.Fatal("access_removed was accepted before 0039")
	}

	migrateTo(t, m, 39)
	if err := insert("access_removed"); err != nil {
		t.Fatalf("access_removed refused after 0039: %v", err)
	}
	if err := insert("rate_cap"); err != nil {
		t.Fatalf("an old reason refused after 0039: %v", err)
	}
	if err := insert("bogus"); err == nil {
		t.Fatal("an unknown reason was accepted after 0039")
	}

	migrateTo(t, m, 38)
	var reasons string
	if err := conn.QueryRow(ctx, `SELECT string_agg(skip_reason, ',' ORDER BY skip_reason) FROM notification_emails`).Scan(&reasons); err != nil {
		t.Fatal(err)
	}
	if reasons != "disabled,rate_cap" {
		t.Fatalf("reasons after down = %q, want disabled,rate_cap (the row is kept, rewritten)", reasons)
	}
	if err := insert("access_removed"); err == nil {
		t.Fatal("access_removed was accepted after the down")
	}
}
