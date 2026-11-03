package database_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"devops/internal/testdb"
)

// TestMigration0036_ChatServicesTeam checks 0036 (LAUN-209, decisions-T38 #17)
// on a database that has rows: a service gets its org's default team, else the
// org's oldest team; a service of an org with no team is removed; NOT NULL and
// the composite FK hold afterwards; the down drops the column; then up again.
func TestMigration0036_ChatServicesTeam(t *testing.T) {
	dsn := testdb.Fresh(t)
	m := migratorFor(t, dsn)
	migrateTo(t, m, 35)
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
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	svc := func(org, name string) {
		exec(`INSERT INTO chat_services (org_id, name, kind, host, webhook_enc, seal_scope, events, enabled)
			VALUES ($1, $2, 'slack', 'hooks.slack.com', $3, 'svc.x.y', '{}', true)`, org, name, []byte("0123456789abcdef0123456789abcdef-x"))
	}
	// org a: two teams, the default is the SECOND one.
	a := one(`INSERT INTO orgs (slug, name) VALUES ('org-a', 'a') RETURNING id::text`)
	ta1 := one(`INSERT INTO teams (org_id, key, name) VALUES ($1, 'ONE', 'one') RETURNING id::text`, a)
	ta2 := one(`INSERT INTO teams (org_id, key, name) VALUES ($1, 'TWO', 'two') RETURNING id::text`, a)
	exec(`UPDATE orgs SET default_team_id = $2 WHERE id = $1`, a, ta2)
	svc(a, "in-a")
	// org b: no default team; the oldest team wins.
	b := one(`INSERT INTO orgs (slug, name) VALUES ('org-b', 'b') RETURNING id::text`)
	tb1 := one(`INSERT INTO teams (org_id, key, name) VALUES ($1, 'OLD', 'old') RETURNING id::text`, b)
	exec(`INSERT INTO teams (org_id, key, name) VALUES ($1, 'NEW', 'new')`, b)
	svc(b, "in-b")
	// org c: no team at all.
	c := one(`INSERT INTO orgs (slug, name) VALUES ('org-c', 'c') RETURNING id::text`)
	svc(c, "in-c")

	migrateTo(t, m, 36)
	if got := one(`SELECT team_id::text FROM chat_services WHERE name = 'in-a'`); got != ta2 {
		t.Errorf("org a service team = %s, want the default team %s (not %s)", got, ta2, ta1)
	}
	if got := one(`SELECT team_id::text FROM chat_services WHERE name = 'in-b'`); got != tb1 {
		t.Errorf("org b service team = %s, want the oldest team %s", got, tb1)
	}
	if n := one(`SELECT count(*)::text FROM chat_services WHERE name = 'in-c'`); n != "0" {
		t.Errorf("a service of an org with no team stayed (%s rows)", n)
	}
	// NOT NULL and the composite FK.
	if _, err := conn.Exec(ctx, `INSERT INTO chat_services (org_id, name, kind, host, webhook_enc, seal_scope)
		VALUES ($1, 'nt', 'slack', 'h', $2, 's')`, a, []byte("0123456789abcdef0123456789abcdef-x")); err == nil {
		t.Error("a service without a team was accepted")
	}
	if _, err := conn.Exec(ctx, `INSERT INTO chat_services (org_id, name, kind, host, webhook_enc, seal_scope, team_id)
		VALUES ($1, 'xo', 'slack', 'h', $2, 's', $3)`, a, []byte("0123456789abcdef0123456789abcdef-x"), tb1); err == nil {
		t.Error("a service in another org's team was accepted")
	}
	if _, err := conn.Exec(ctx, `DELETE FROM teams WHERE id = $1`, ta2); err == nil {
		t.Error("a team with a service was deleted by SQL")
	}

	migrateTo(t, m, 35)
	if n := one(`SELECT count(*)::text FROM chat_services`); n != "2" {
		t.Errorf("after down: %s services, want 2", n)
	}
	migrateTo(t, m, 36)
	if got := one(`SELECT team_id::text FROM chat_services WHERE name = 'in-a'`); got != ta2 {
		t.Errorf("after up again: team %s, want %s", got, ta2)
	}
}
