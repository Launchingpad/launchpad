package testdb_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"devops/db/migrations"
	"devops/internal/testdb"
)

// New copies the migrated template: the copy is at the newest migration, not
// dirty, and holds no rows of the fixture (LAUN-135).
func TestNew_CopyIsMigratedAndEmpty(t *testing.T) {
	pool, _ := testdb.New(t)
	ctx := context.Background()
	var version int
	var dirty bool
	if err := pool.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil {
		t.Fatal(err)
	}
	newest := 0
	ents, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if n, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0]); err == nil && n > newest {
			newest = n
		}
	}
	if version != newest || dirty {
		t.Fatalf("schema_migrations = %d dirty=%v, want %d clean", version, dirty, newest)
	}
	var users int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&users); err != nil || users != 0 {
		t.Fatalf("users = %d (%v), want 0", users, err)
	}
}
