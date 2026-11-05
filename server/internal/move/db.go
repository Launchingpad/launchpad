package move

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// querier is what pgx.Tx and pgxpool.Pool share.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// migrationVersion reads the applied migration version. A dirty database is
// refused: its schema is in an unknown state.
func migrationVersion(ctx context.Context, q querier) (uint, error) {
	var v int64
	var dirty bool
	if err := q.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&v, &dirty); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("move: the database has no migration version")
		}
		return 0, fmt.Errorf("move: read migration version: %w", err)
	}
	if dirty {
		return 0, fmt.Errorf("move: the database migration state is dirty")
	}
	return uint(v), nil
}

// orderedTables returns the copied tables in foreign key order, read from the
// catalog of the connection's own database.
func orderedTables(ctx context.Context, q querier) ([]Table, error) {
	rows, err := q.Query(ctx, `
		SELECT c.conrelid::regclass::text, c.confrelid::regclass::text
		FROM pg_constraint c
		WHERE c.contype = 'f' AND c.connamespace = 'public'::regnamespace`)
	if err != nil {
		return nil, fmt.Errorf("move: read foreign keys: %w", err)
	}
	defer rows.Close()
	deps := map[string][]string{}
	for rows.Next() {
		var from, to string
		if err := rows.Scan(&from, &to); err != nil {
			return nil, err
		}
		deps[from] = append(deps[from], to)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return sortedCopied(deps)
}

// tableColumns lists the stored columns of a table in order.
func tableColumns(ctx context.Context, q querier, table string) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1 AND is_generated = 'NEVER'
		ORDER BY ordinal_position`, table)
	if err != nil {
		return nil, fmt.Errorf("move: read columns of %s: %w", table, err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("move: table %s has no columns (is it in the schema?)", table)
	}
	return cols, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// dbErr reduces a PostgreSQL error to its code and constraint: the full text
// can quote a row value in its detail, and a value may be a secret.
func dbErr(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		if pe.ConstraintName != "" {
			return fmt.Errorf("database error %s (constraint %s)", pe.Code, pe.ConstraintName)
		}
		return fmt.Errorf("database error %s", pe.Code)
	}
	return err
}
