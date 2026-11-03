package database_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"devops/internal/testdb"
)

// TestMigration0035_ChatServicesFluxer checks that 0035 (LAUN-206) widens the
// kind CHECK to 'fluxer', and that its down turns a Fluxer service into a
// custom one (and restores the old CHECK). Then up again.
func TestMigration0035_ChatServicesFluxer(t *testing.T) {
	dsn := testdb.Fresh(t)
	m := migratorFor(t, dsn)
	migrateTo(t, m, 34)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var org string
	if err := conn.QueryRow(ctx, `INSERT INTO orgs (slug, name) VALUES ('alpha-org', 'alpha') RETURNING id::text`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	ins := func(kind string) error {
		_, err := conn.Exec(ctx, `INSERT INTO chat_services (org_id, name, kind, host, webhook_enc, seal_scope, events, enabled)
			VALUES ($1, $2, $2, 'h.example.com', $3, 'svc.x.y', '{}', true)`, org, kind, []byte("0123456789abcdef0123456789abcdef-x"))
		return err
	}
	if err := ins("fluxer"); err == nil {
		t.Fatal("fluxer accepted before 0035")
	}
	migrateTo(t, m, 35)
	if err := ins("fluxer"); err != nil {
		t.Fatalf("fluxer refused after 0035: %v", err)
	}
	if err := ins("teams"); err == nil {
		t.Fatal("an unknown kind accepted")
	}
	migrateTo(t, m, 34)
	var kind string
	if err := conn.QueryRow(ctx, `SELECT kind FROM chat_services WHERE name = 'fluxer'`).Scan(&kind); err != nil || kind != "custom" {
		t.Fatalf("after down: kind %q err %v", kind, err)
	}
	if err := ins("fluxer"); err == nil {
		t.Fatal("fluxer accepted after the down")
	}
	migrateTo(t, m, 35)
}
