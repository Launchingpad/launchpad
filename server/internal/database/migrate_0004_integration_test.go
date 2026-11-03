package database_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib" // the "pgx" database/sql driver

	"devops/db/migrations"
	"devops/internal/testdb"
)

func migratorFor(t *testing.T, dsn string) *migrate.Migrate {
	t.Helper()
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	drv, err := migratepg.WithInstance(db, &migratepg.Config{})
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", drv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.Close() })
	return m
}

func migrateTo(t *testing.T, m *migrate.Migrate, v uint) {
	t.Helper()
	if err := m.Migrate(v); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to %d: %v", v, err)
	}
}

// TestMigration0004_MRLinksIntegration checks 0004's backfill rule, its new
// key, and that its down restores the old key (deleting only the rows the
// old key cannot hold).
func TestMigration0004_MRLinksIntegration(t *testing.T) {
	dsn := testdb.Fresh(t)
	m := migratorFor(t, dsn)
	migrateTo(t, m, 3)

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	one := func(q string, args ...any) string {
		t.Helper()
		var s string
		if err := conn.QueryRow(ctx, q, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return s
	}

	// At version 3: org1 with two integrations (A older) that both map
	// project 42; org2 with no integration. Links: L1 (org1, project 42),
	// L2 (org1, project 99, mapped by nobody), L3 (org2).
	newIssue := func(slug string) (org, issue string) {
		org = one(`INSERT INTO orgs (slug, name) VALUES ($1, $1) RETURNING id::text`, slug)
		team := one(`INSERT INTO teams (org_id, key, name) VALUES ($1, 'ORB', 'Orbit') RETURNING id::text`, org)
		state := one(`INSERT INTO workflow_states (org_id, team_id, name, type) VALUES ($1, $2, 'Todo', 'unstarted') RETURNING id::text`, org, team)
		issue = one(`INSERT INTO issues (org_id, team_id, number, title, state_id) VALUES ($1, $2, 1, 'x', $3) RETURNING id::text`, org, team, state)
		return org, issue
	}
	org1, issue1 := newIssue("one")
	org2, issue2 := newIssue("two")
	intA := one(`INSERT INTO gitlab_integrations (org_id, base_url, webhook_secret_hash, created_at)
		VALUES ($1, 'https://a.example', decode(repeat('aa', 32), 'hex'), now() - interval '1 day') RETURNING id::text`, org1)
	intB := one(`INSERT INTO gitlab_integrations (org_id, base_url, webhook_secret_hash)
		VALUES ($1, 'https://b.example', decode(repeat('bb', 32), 'hex')) RETURNING id::text`, org1)
	for _, in := range []string{intB, intA} {
		exec(`INSERT INTO gitlab_project_map (org_id, integration_id, gitlab_project_id, project_path) VALUES ($1, $2, 42, 'p')`, org1, in)
	}
	link := func(org, issue string, project int) string {
		return one(`INSERT INTO mr_links (org_id, issue_id, gitlab_project_id, mr_iid, url, state)
			VALUES ($1, $2, $3, 17, 'https://x/17', 'opened') RETURNING id::text`, org, issue, project)
	}
	l1 := link(org1, issue1, 42)
	l2 := link(org1, issue1, 99)
	link(org2, issue2, 42)

	migrateTo(t, m, 4)
	if got := one(`SELECT integration_id::text FROM mr_links WHERE id = $1`, l1); got != intA {
		t.Fatalf("L1 backfilled to %s, want the oldest mapping integration %s", got, intA)
	}
	if got := one(`SELECT integration_id::text FROM mr_links WHERE id = $1`, l2); got != intA {
		t.Fatalf("L2 (unmapped project) backfilled to %s, want the org's oldest integration %s", got, intA)
	}
	if got := one(`SELECT count(*)::text FROM mr_links WHERE org_id = $1`, org2); got != "0" {
		t.Fatalf("org2's unattributable link was kept (%s rows)", got)
	}
	if got := one(`SELECT is_nullable FROM information_schema.columns WHERE table_name = 'mr_links' AND column_name = 'integration_id'`); got != "NO" {
		t.Fatalf("integration_id nullable = %s", got)
	}
	// The new key: the same issue, project and iid under the other integration.
	newer := one(`INSERT INTO mr_links (org_id, issue_id, integration_id, gitlab_project_id, mr_iid, url, state)
		VALUES ($1, $2, $3, 42, 17, 'https://b/17', 'opened') RETURNING id::text`, org1, issue1, intB)
	if _, err := conn.Exec(ctx, `INSERT INTO mr_links (org_id, issue_id, integration_id, gitlab_project_id, mr_iid, url, state)
		VALUES ($1, $2, $3, 42, 17, 'https://b/17', 'opened')`, org1, issue1, intB); err == nil {
		t.Fatal("a second link for the same integration, project and iid was accepted")
	}

	// Down: the old key cannot hold both L1 and the newer row; the oldest stays.
	migrateTo(t, m, 3)
	if got := one(`SELECT count(*)::text FROM mr_links WHERE id = $1`, newer); got != "0" {
		t.Fatal("down kept the row the old key cannot hold")
	}
	if got := one(`SELECT count(*)::text FROM mr_links WHERE id IN ($1, $2)`, l1, l2); got != "2" {
		t.Fatalf("down lost other rows: %s of 2 left", got)
	}
	if got := one(`SELECT count(*)::text FROM information_schema.columns WHERE table_name = 'mr_links' AND column_name = 'integration_id'`); got != "0" {
		t.Fatal("down left integration_id")
	}
	if _, err := conn.Exec(ctx, `INSERT INTO mr_links (org_id, issue_id, gitlab_project_id, mr_iid, url, state)
		VALUES ($1, $2, 42, 17, 'https://x/17', 'opened')`, org1, issue1); err == nil {
		t.Fatal("after down, the old unique key is missing")
	}
	migrateTo(t, m, 4) // and up again
}
