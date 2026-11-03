package importer

import (
	"context"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"devops/db/migrations"
)

// LatestMigration is the highest migration number this binary embeds.
func LatestMigration() (uint, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return 0, fmt.Errorf("read migrations: %w", err)
	}
	var latest uint
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		n, err := strconv.ParseUint(strings.SplitN(name, "_", 2)[0], 10, 32)
		if err != nil {
			continue
		}
		latest = max(latest, uint(n))
	}
	return latest, nil
}

// CheckSchema refuses a database that is not at this binary's newest
// migration (or is dirty). The importer never migrates: the server does
// that at boot, and a dry run must change nothing.
func CheckSchema(ctx context.Context, pool *pgxpool.Pool) error {
	want, err := LatestMigration()
	if err != nil {
		return err
	}
	var v int64
	var dirty bool
	if err := pool.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&v, &dirty); err != nil {
		return fmt.Errorf("read schema version (has the server booted on this database?): %w", err)
	}
	if dirty || v != int64(want) {
		return fmt.Errorf("database schema is at version %d (dirty %v), this importer needs %d: boot the matching server first", v, dirty, want)
	}
	return nil
}
