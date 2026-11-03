package database_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"devops/internal/database"
	"devops/internal/testdb"
)

// TestMigration0007_SignupsInvites: 0007 makes signup_requests and
// org_invites; their CHECKs and unique indexes refuse bad rows (two pending
// requests or two open invites for one email, a plaintext-length code, an
// approved row without a decision); the down drops both tables and leaves
// users alone; a new up makes them again.
func TestMigration0007_SignupsInvites(t *testing.T) {
	dsn := testdb.Fresh(t)
	m := migratorFor(t, dsn)
	migrateTo(t, m, 7)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()

	setupRows := func() {
		t.Helper()
		for _, q := range []string{
			`INSERT INTO orgs (slug, name) VALUES ('o1', 'O1') ON CONFLICT DO NOTHING`,
			`INSERT INTO users (username, name, email) VALUES ('kept', 'Kept', 'kept@example.com') ON CONFLICT DO NOTHING`,
			`INSERT INTO signup_requests (org_id, email, name, username, expires_at)
			 SELECT id, 'a@example.com', 'A', 'anna', now() + interval '30 days' FROM orgs WHERE slug = 'o1'`,
			`INSERT INTO org_invites (org_id, email, token_hash, expires_at)
			 SELECT id, 'b@example.com', sha256('t1'), now() + interval '7 days' FROM orgs WHERE slug = 'o1'`,
		} {
			if _, err := conn.Exec(ctx, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
	}
	setupRows()

	refused := []struct{ name, sql, code string }{
		{"a second pending request for one email", `INSERT INTO signup_requests (org_id, email, name, username, expires_at)
			SELECT id, 'a@example.com', 'A2', 'anna2', now() + interval '1 day' FROM orgs WHERE slug = 'o1'`, database.CodeUniqueViolation},
		{"an upper-case email", `INSERT INTO signup_requests (org_id, email, name, username, expires_at)
			SELECT id, 'C@example.com', 'C', 'cee', now() + interval '1 day' FROM orgs WHERE slug = 'o1'`, database.CodeCheckViolation},
		{"a bad status", `UPDATE signup_requests SET status = 'open'`, database.CodeCheckViolation},
		{"approved with no decision", `UPDATE signup_requests SET status = 'approved'`, database.CodeCheckViolation},
		{"a code hash that is not 32 bytes", `UPDATE signup_requests SET code_salt = '\x00000000000000000000000000000000'::bytea,
			code_hash = convert_to('123456', 'UTF8'), code_expires_at = now()`, database.CodeCheckViolation},
		{"half a code", `UPDATE signup_requests SET code_hash = sha256('x')`, database.CodeCheckViolation},
		{"a block on a pending row", `UPDATE signup_requests SET blocked = true`, database.CodeCheckViolation},
		{"a bad username", `INSERT INTO signup_requests (org_id, email, name, username, expires_at)
			SELECT id, 'd@example.com', 'D', 'Bad Name', now() + interval '1 day' FROM orgs WHERE slug = 'o1'`, database.CodeCheckViolation},
		{"a second open invite for one email", `INSERT INTO org_invites (org_id, email, token_hash, expires_at)
			SELECT id, 'b@example.com', sha256('t2'), now() + interval '1 day' FROM orgs WHERE slug = 'o1'`, database.CodeUniqueViolation},
		{"a reused token hash", `INSERT INTO org_invites (org_id, email, token_hash, expires_at)
			SELECT id, 'e@example.com', sha256('t1'), now() + interval '1 day' FROM orgs WHERE slug = 'o1'`, database.CodeUniqueViolation},
		{"a short token hash", `INSERT INTO org_invites (org_id, email, token_hash, expires_at)
			SELECT id, 'f@example.com', '\x00'::bytea, now() + interval '1 day' FROM orgs WHERE slug = 'o1'`, database.CodeCheckViolation},
		{"a bad role", `UPDATE org_invites SET role = 'owner'`, database.CodeCheckViolation},
		{"accepted and revoked", `UPDATE org_invites SET accepted_at = now(), revoked_at = now()`, database.CodeCheckViolation},
		{"a total under the code's attempts", `UPDATE signup_requests SET code_attempts = 3, code_attempts_total = 2`, database.CodeCheckViolation},
	}
	for _, r := range refused {
		if _, err := conn.Exec(ctx, r.sql); !database.IsPgCode(err, r.code) {
			t.Fatalf("%s: err %v, want SQLSTATE %s", r.name, err, r.code)
		}
	}
	// A revoked invite frees the address; a rejected request does too.
	for _, q := range []string{
		`UPDATE org_invites SET revoked_at = now()`,
		`INSERT INTO org_invites (org_id, email, token_hash, expires_at)
		 SELECT id, 'b@example.com', sha256('t3'), now() + interval '1 day' FROM orgs WHERE slug = 'o1'`,
		`UPDATE signup_requests SET status = 'rejected', decided_at = now(), blocked = true`,
		`INSERT INTO signup_requests (org_id, email, name, username, expires_at)
		 SELECT id, 'a@example.com', 'A', 'anna', now() + interval '30 days' FROM orgs WHERE slug = 'o1'`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	var unproven, emailed bool
	if err := conn.QueryRow(ctx, `SELECT u.email_unproven, (SELECT emailed FROM org_invites LIMIT 1) FROM users u WHERE username = 'kept'`).Scan(&unproven, &emailed); err != nil || unproven || emailed {
		t.Fatalf("defaults: email_unproven %v emailed %v %v", unproven, emailed, err)
	}

	migrateTo(t, m, 6)
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.signup_requests') IS NOT NULL OR to_regclass('public.org_invites') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("down left a table: %v %v", exists, err)
	}
	var col bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'email_unproven')`).Scan(&col); err != nil || col {
		t.Fatalf("down left users.email_unproven: %v %v", col, err)
	}
	var kept int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM users WHERE username = 'kept'`).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("down touched users: %d %v", kept, err)
	}
	migrateTo(t, m, 7)
	setupRows()
}
