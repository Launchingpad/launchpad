package database_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"devops/internal/database"
	"devops/internal/testdb"
)

// TestMigration0006_PlatformSettings: 0006 makes exactly one row with the
// defaults, its CHECKs refuse a second row and bad values, and its down and
// a new up give the row back.
func TestMigration0006_PlatformSettings(t *testing.T) {
	dsn := testdb.Fresh(t)
	m := migratorFor(t, dsn)
	migrateTo(t, m, 6)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()

	check := func() {
		t.Helper()
		var n int
		var provider, signup, security string
		var port int
		var tokenNull, keyNull bool
		if err := conn.QueryRow(ctx, `SELECT count(*) OVER (), mail_provider, signup_mode, smtp_security, smtp_port,
			postmark_token_enc IS NULL, key_check IS NULL FROM platform_settings`).
			Scan(&n, &provider, &signup, &security, &port, &tokenNull, &keyNull); err != nil {
			t.Fatal(err)
		}
		if n != 1 || provider != "env" || signup != "invite" || security != "starttls" || port != 587 || !tokenNull || !keyNull {
			t.Fatalf("row: n=%d %s %s %s %d %v %v", n, provider, signup, security, port, tokenNull, keyNull)
		}
	}
	check()

	refused := []struct{ name, sql, code string }{
		{"a second row", `INSERT INTO platform_settings (id) VALUES (true)`, database.CodeUniqueViolation},
		{"id false", `INSERT INTO platform_settings (id) VALUES (false)`, database.CodeCheckViolation},
		{"bad provider", `UPDATE platform_settings SET mail_provider = 'sendgrid'`, database.CodeCheckViolation},
		{"bad signup mode", `UPDATE platform_settings SET signup_mode = 'open'`, database.CodeCheckViolation},
		{"bad security", `UPDATE platform_settings SET smtp_security = 'ssl'`, database.CodeCheckViolation},
		{"bad port", `UPDATE platform_settings SET smtp_port = 70000`, database.CodeCheckViolation},
		{"short blob", `UPDATE platform_settings SET smtp_password_enc = '\x00'::bytea`, database.CodeCheckViolation},
		{"short key check", `UPDATE platform_settings SET key_check = '\x00'::bytea`, database.CodeCheckViolation},
	}
	for _, r := range refused {
		if _, err := conn.Exec(ctx, r.sql); !database.IsPgCode(err, r.code) {
			t.Fatalf("%s: err %v, want SQLSTATE %s", r.name, err, r.code)
		}
	}
	// updated_by is cleared when the user goes.
	if _, err := conn.Exec(ctx, `WITH u AS (INSERT INTO users (username, name) VALUES ('gone', 'Gone') RETURNING id)
		UPDATE platform_settings SET updated_by = (SELECT id FROM u)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `DELETE FROM users WHERE username = 'gone'`); err != nil {
		t.Fatal(err)
	}
	var cleared bool
	if err := conn.QueryRow(ctx, `SELECT updated_by IS NULL FROM platform_settings`).Scan(&cleared); err != nil || !cleared {
		t.Fatalf("updated_by not cleared: %v %v", cleared, err)
	}

	migrateTo(t, m, 5)
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.platform_settings') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("down left the table: %v %v", exists, err)
	}
	migrateTo(t, m, 6)
	check()
}
