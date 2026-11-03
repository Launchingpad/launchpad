// Package database owns the pgx pool, the boot-time migrations and the
// transaction helper every write path uses.
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for golang-migrate
	pgxuuid "github.com/vgarvardt/pgx-google-uuid/v5"

	"devops/db/migrations"
	"devops/internal/httpx"
)

// Open makes a pool and checks that the database answers. The pool is sized
// for a small single-instance service; statement_timeout bounds any runaway
// query so a lock is never held for minutes (supabase lock-short-transactions).
func Open(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if !strings.Contains(databaseURL, "pool_max_conns") {
		cfg.MaxConns = 20
	}
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	if _, ok := cfg.ConnConfig.RuntimeParams["statement_timeout"]; !ok {
		cfg.ConnConfig.RuntimeParams["statement_timeout"] = "30s"
	}
	cfg.AfterConnect = func(_ context.Context, conn *pgx.Conn) error {
		pgxuuid.Register(conn.TypeMap())
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// Migrate applies every pending up migration. It is a no-op when the schema
// is current. It returns the version the database is at afterwards.
func Migrate(databaseURL string) (uint, error) {
	m, closeFn, err := newMigrator(databaseURL)
	if err != nil {
		return 0, err
	}
	defer closeFn()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return 0, fmt.Errorf("migrate up: %w", err)
	}
	v, _, err := m.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return 0, fmt.Errorf("migrate version: %w", err)
	}
	return v, nil
}

// MigrateDown rolls back every migration. Tests and the migrate CLI use it;
// the server never does.
func MigrateDown(databaseURL string) error {
	m, closeFn, err := newMigrator(databaseURL)
	if err != nil {
		return err
	}
	defer closeFn()
	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate down: %w", err)
	}
	return nil
}

func newMigrator(databaseURL string) (*migrate.Migrate, func(), error) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, nil, fmt.Errorf("migrate source: %w", err)
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, nil, fmt.Errorf("migrate open: %w", err)
	}
	driver, err := migratepg.WithInstance(db, &migratepg.Config{})
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("migrate driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", driver)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("migrate instance: %w", err)
	}
	return m, func() { _, _ = m.Close() }, nil
}

// Beginner is satisfied by *pgxpool.Pool and pgx.Tx.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

type claimKey struct{}

// WithIdempotencyClaim marks ctx as running under an Idempotency-Key claim.
// InTx then records the claim as committed in the SAME transaction as the
// request's write, so "did this request's write commit?" has an exact,
// durable answer even if the process dies before the response is stored.
func WithIdempotencyClaim(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, claimKey{}, id)
}

func idempotencyClaim(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(claimKey{}).(uuid.UUID)
	return id, ok
}

// InTx runs fn inside one READ COMMITTED transaction. It commits when fn
// returns nil and rolls back otherwise (golang-database transactions.md).
func InTx(ctx context.Context, db Beginner, fn func(tx pgx.Tx) error) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit
	if err := fn(tx); err != nil {
		return Retryable(err)
	}
	if id, ok := idempotencyClaim(ctx); ok {
		tag, err := tx.Exec(ctx, `UPDATE idempotency_keys SET committed_at = now() WHERE id = $1 AND committed_at IS NULL`, id)
		if err != nil {
			return fmt.Errorf("mark idempotency claim committed: %w", err)
		}
		// 0 rows: our claim was taken over as abandoned while we ran, and a
		// retry now owns the key. Roll back, so the write happens at most once.
		// The UPDATE row-locks the claim, so a takeover cannot slip in between
		// this check and COMMIT (its DELETE waits, then sees committed_at).
		if tag.RowsAffected() != 1 {
			return ErrIdempotencyClaimLost
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Retryable(fmt.Errorf("commit: %w", err))
	}
	return nil
}

// Retryable maps a failure that a retry or a fresh lookup can cure to a clean
// answer, never a 500 (LAUN-191):
//   - a deadlock (40P01) or serialization failure (40001) is 503 overloaded
//     with Retry-After. A 5xx is never stored by the idempotency middleware,
//     so a retry with the same Idempotency-Key runs the write again;
//   - a foreign key (23503) on a team, a project or a milestone that fails
//     means the row was deleted after the caller looked it up: *GoneError,
//     400 validation_failed (a body reference to a missing row). A path
//     handler may turn it into 404 with errors.As.
func Retryable(err error) error {
	if IsPgCode(err, "40P01") || IsPgCode(err, "40001") {
		e := httpx.Errorf(http.StatusServiceUnavailable, httpx.CodeOverloaded, "a concurrent change collided with this one; retry the request")
		e.RetryAfter = 1
		return e
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == CodeForeignKeyViolation {
		for suffix, ref := range goneFKs {
			if strings.HasSuffix(pg.ConstraintName, suffix) {
				return &GoneError{Ref: ref, api: httpx.Invalid("the %s no longer exists", ref)}
			}
		}
	}
	return err
}

// goneFKs: foreign key name suffix -> what the key points at. Every writer
// that names a team or a project can lose a race with its delete.
var goneFKs = map[string]string{
	"_team_fk":            "team",
	"_project_fk":         "project",
	"issues_milestone_fk": "milestone",
	"projects_status_fk":  "project status", // LAUN-195: a project write that loses to a status delete
	"issues_cycle_fk":     "cycle",          // LAUN-172: an issue write that loses to a cycle delete
	// LAUN-201: a comment, attachment, label, relation or re-parent that loses to an issue delete.
	"_issue_fk":                  "issue",
	"issues_parent_fk":           "issue",
	"issue_relations_related_fk": "issue",
}

// GoneError is a write that lost a race with a delete of a team, a project or
// a milestone or cycle it named. It unwraps to the 400 *httpx.Error.
type GoneError struct {
	Ref string // "team", "project", "milestone" or "cycle"
	api *httpx.Error
}

func (e *GoneError) Error() string { return e.api.Error() }
func (e *GoneError) Unwrap() error { return e.api }

// ErrIdempotencyClaimLost aborts a transaction whose Idempotency-Key claim
// was taken over by a retry. It is a 409: the retry is doing the work.
var ErrIdempotencyClaimLost = httpx.Errorf(http.StatusConflict, httpx.CodeIdempotencyInProgress,
	"this request's Idempotency-Key was taken over by a retry; nothing was written")

// Postgres error codes the service maps to API errors.
const (
	CodeUniqueViolation     = "23505"
	CodeForeignKeyViolation = "23503"
	CodeCheckViolation      = "23514"
)

// IsPgCode reports whether err carries the given SQLSTATE.
func IsPgCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// ConstraintName returns the violated constraint, or "".
func ConstraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}
