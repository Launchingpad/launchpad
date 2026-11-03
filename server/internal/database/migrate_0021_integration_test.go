package database_test

import (
	"context"
	"testing"

	"devops/internal/database"
	"devops/internal/testdb"
)

// TestMigration0021_NativeKind: rows named 'native session' (made by the
// native login since LAUN-122) become kind native; web sessions and PATs are
// untouched; the CHECK admits native; the down turns native back into
// session (LAUN-129, decisions-T6a #34).
func TestMigration0021_NativeKind(t *testing.T) {
	dsn := testdb.Fresh(t)
	m := migratorFor(t, dsn)
	migrateTo(t, m, 20)
	c := backupConn(t, dsn)
	ctx := context.Background()

	mustExec(t, c, `INSERT INTO users (id, username, name) VALUES ('0192f000-0000-7000-8000-000000000001', 'u21', 'U')`)
	ins := func(kind, name, hash string) {
		t.Helper()
		mustExec(t, c, `INSERT INTO api_tokens (user_id, kind, name, token_hash, prefix, scopes)
			VALUES ('0192f000-0000-7000-8000-000000000001', $1, $2, decode($3, 'hex'), 'odt_aaaaaaaa', ARRAY['read'])`,
			kind, name, hash)
	}
	h := func(c string) string {
		s := ""
		for range 32 {
			s += c + c
		}
		return s
	}
	ins("session", "native session", h("a"))
	ins("session", "web session", h("b"))
	ins("pat", "native session", h("c")) // a PAT with that name stays a PAT
	refuse(t, c, "native before 0021", database.CodeCheckViolation,
		`INSERT INTO api_tokens (user_id, kind, name, token_hash, prefix, scopes)
		 VALUES ('0192f000-0000-7000-8000-000000000001', 'native', 'x', decode($1, 'hex'), 'odt_aaaaaaaa', ARRAY['read'])`, h("d"))

	migrateTo(t, m, 21)
	kinds := func() map[string]string {
		t.Helper()
		rows, err := c.Query(ctx, `SELECT encode(token_hash, 'hex'), kind FROM api_tokens`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var hash, kind string
			if err := rows.Scan(&hash, &kind); err != nil {
				t.Fatal(err)
			}
			out[hash[:2]] = kind
		}
		return out
	}
	if got := kinds(); got["aa"] != "native" || got["bb"] != "session" || got["cc"] != "pat" {
		t.Fatalf("after up: %v", got)
	}
	ins("native", "n", h("d"))
	refuse(t, c, "unknown kind", database.CodeCheckViolation,
		`INSERT INTO api_tokens (user_id, kind, name, token_hash, prefix, scopes)
		 VALUES ('0192f000-0000-7000-8000-000000000001', 'other', 'x', decode($1, 'hex'), 'odt_aaaaaaaa', ARRAY['read'])`, h("e"))

	if err := m.Steps(-1); err != nil {
		t.Fatal(err)
	}
	if got := kinds(); got["aa"] != "session" || got["dd"] != "session" || got["cc"] != "pat" {
		t.Fatalf("after down: %v", got)
	}
}
