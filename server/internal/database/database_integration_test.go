package database_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"devops/internal/database"
	"devops/internal/testdb"
)

// schemaFingerprint lists every column, constraint, index and function in
// public, so two schemas can be compared exactly.
func schemaFingerprint(t *testing.T, dsn string) []string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	queries := []string{
		`SELECT 'col ' || table_name || '.' || column_name || ' ' || data_type || ' ' || is_nullable || ' ' || coalesce(column_default, '')
		 FROM information_schema.columns WHERE table_schema = 'public' AND table_name <> 'schema_migrations'`,
		`SELECT 'con ' || conrelid::regclass || ' ' || conname || ' ' || pg_get_constraintdef(oid)
		 FROM pg_constraint WHERE connamespace = 'public'::regnamespace AND conrelid::regclass::text <> 'schema_migrations'`,
		`SELECT 'idx ' || indexdef FROM pg_indexes WHERE schemaname = 'public' AND tablename <> 'schema_migrations'`,
		`SELECT 'fn ' || p.proname || ' ' || md5(pg_get_functiondef(p.oid))
		 FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace`,
	}
	var out []string
	for _, q := range queries {
		rows, err := conn.Query(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		vals, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, vals...)
	}
	sort.Strings(out)
	return out
}

func countTables(t *testing.T, dsn string) int {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tablename <> 'schema_migrations'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestMigrateUpDownUp is the AC "migration up, down, up passes": the down
// leaves nothing behind, and the second up rebuilds exactly the first schema.
func TestMigrateUpDownUp(t *testing.T) {
	dsn := testdb.Fresh(t)

	v, err := database.Migrate(dsn)
	if err != nil {
		t.Fatalf("first up: %v", err)
	}
	if v < 1 {
		t.Fatalf("version after up = %d", v)
	}
	first := schemaFingerprint(t, dsn)
	if n := countTables(t, dsn); n < 28 {
		t.Fatalf("tables after up = %d, want the full baseline (>= 28)", n)
	}

	if err := database.MigrateDown(dsn); err != nil {
		t.Fatalf("down: %v", err)
	}
	if leftover := schemaFingerprint(t, dsn); len(leftover) != 0 {
		t.Fatalf("down left objects behind:\n%s", strings.Join(leftover, "\n"))
	}

	if _, err := database.Migrate(dsn); err != nil {
		t.Fatalf("second up: %v", err)
	}
	second := schemaFingerprint(t, dsn)
	if strings.Join(first, "\n") != strings.Join(second, "\n") {
		t.Fatalf("second up differs from the first (%d vs %d objects)", len(first), len(second))
	}

	// A further up is a no-op.
	if v2, err := database.Migrate(dsn); err != nil || v2 != v {
		t.Fatalf("re-run up: version %d err %v", v2, err)
	}
}

// TestUUIDv7 checks the id default is a version 7 UUID whose time prefix is now.
func TestUUIDv7(t *testing.T) {
	pool, _ := testdb.New(t)
	var version, ageMs int
	err := pool.QueryRow(context.Background(), `
		WITH u AS (SELECT uuidv7() AS id)
		SELECT get_byte(uuid_send(id), 6) >> 4,
		       abs(extract(epoch FROM clock_timestamp()) * 1000
		           - ('x' || substr(replace(id::text, '-', ''), 1, 12))::bit(48)::bigint)::int
		FROM u`).Scan(&version, &ageMs)
	if err != nil {
		t.Fatal(err)
	}
	if version != 7 {
		t.Fatalf("version nibble = %d, want 7", version)
	}
	if ageMs > 5000 {
		t.Fatalf("time prefix is %d ms from now", ageMs)
	}
}

func TestInTxRollsBackOnError(t *testing.T) {
	pool, _ := testdb.New(t)
	ctx := context.Background()
	sentinel := context.Canceled
	err := database.InTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO orgs (slug, name) VALUES ('rollback-me', 'x')`); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM orgs WHERE slug = 'rollback-me'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("row survived the rollback: n=%d err=%v", n, err)
	}
}
