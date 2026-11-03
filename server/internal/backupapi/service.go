// Package backupapi is the domain code of the backups API (LAUN-69,
// docs/internal/decisions-T16-backups.md #43): the notice settings, the age
// recipients and their 24 hour hold, the destinations with their write-only
// sealed secrets, and the rows that tell offsite to probe, test, exchange or
// run. It dials nothing: every action only queues a row (decisions-T16 #10),
// and offsite does the network work.
//
// Rules this package keeps (and its tests pin):
//   - Secrets are sealed with platform.BackupKeyring only, never logged, never
//     put in an event, an error, a change-log row or a run row (except the
//     sealed test_secrets_enc).
//   - No events.Emit (decisions-T16 #26): the record is backup_change_log, in
//     the same transaction as the change.
//   - A route that stores a secret is never idempotent (the handlers use
//     idempotency.Reject).
package backupapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/dbq"
	"devops/internal/httpx"
	"devops/internal/login"
	"devops/internal/platform"
)

// Hold is how long a recipient add or removal waits (decisions-T16 #20).
const Hold = 24 * time.Hour

// Options configure the Service.
type Options struct {
	// Key is BACKUP_ENCRYPTION_KEY (32 bytes). Nil: no secret can be stored or
	// sealed, enabling a destination is 409 and an action is 503 backup_unavailable.
	Key []byte
	// Clock is the clock of recipient times. Nil: the DATABASE clock (decisions-T16
	// #31 "Clock"). Tests inject one.
	Clock func() time.Time
	// ActionPerMinute and ActionBurst limit probe, test, exchange and run
	// together: default 6 a minute, burst 3 (decisions-T16 #43; a guess).
	ActionPerMinute, ActionBurst int
	// MailReady reports whether the platform mail transport can send (the
	// status route's mailReady and its mail_off banner, LAUN-75). Nil: not ready.
	MailReady func() bool
	// FilesStored reports whether this server keeps attachments (it has the object
	// store). True: the status route says "files not backed up" for an enabled
	// destination without a recent good files backup (LAUN-80). Nil: false.
	FilesStored func() bool
}

// Service reads and writes the backup configuration. It is safe for concurrent
// use.
type Service struct {
	pool  *pgxpool.Pool
	log   *slog.Logger
	keys  *platform.BackupKeyring // nil: no BACKUP_ENCRYPTION_KEY
	clock func() time.Time
	limit *login.RouteLimit

	mailReady   func() bool
	filesStored func() bool
}

// New returns a Service.
func New(pool *pgxpool.Pool, log *slog.Logger, o Options) (*Service, error) {
	s := &Service{pool: pool, log: log, clock: o.Clock, mailReady: o.MailReady, filesStored: o.FilesStored}
	if o.Key != nil {
		k, err := platform.NewBackupKeyring(o.Key)
		if err != nil {
			return nil, fmt.Errorf("backupapi: %w", err)
		}
		s.keys = k
	}
	if o.ActionPerMinute <= 0 {
		o.ActionPerMinute, o.ActionBurst = 6, 3
	}
	if o.ActionBurst <= 0 {
		o.ActionBurst = 1
	}
	s.limit = login.NewRouteLimit(o.ActionPerMinute, o.ActionBurst, nil)
	return s, nil
}

// TakeAction takes one token of the shared probe, test, exchange and run
// allowance, or returns the 429 (a *login.RateLimitedError).
func (s *Service) TakeAction() error { return s.limit.Take() }

// Errors with a fixed text. None names a value the caller sent.
var (
	errNoKeyWrite = httpx.Errorf(http.StatusConflict, httpx.CodeConflict,
		"this server has no BACKUP_ENCRYPTION_KEY, so it cannot store a backup secret or enable a destination; set it (docs/deploy.md) and restart the server")
	errNoKeyAction = httpx.Errorf(http.StatusServiceUnavailable, httpx.CodeBackupUnavailable,
		"this server has no BACKUP_ENCRYPTION_KEY, so it cannot start this action; set it (docs/deploy.md) and restart the server")
	errBusy = httpx.Errorf(http.StatusConflict, httpx.CodeDestinationBusy,
		"a backup run of this destination is already queued, waiting or running")
	errNoDestination = httpx.NotFound("destination")
)

// now is the time of a recipient change: the injected clock, else the database's.
func (s *Service) now(ctx context.Context, q *dbq.Queries) (time.Time, error) {
	if s.clock != nil {
		return s.clock().UTC(), nil
	}
	t, err := q.BackupDBNow(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("read the database clock: %w", err)
	}
	return t.UTC(), nil
}

// logChange appends a change-log row in the caller's transaction. detail must
// hold no secret: callers build it from names, ids and canonical identities.
func logChange(ctx context.Context, q *dbq.Queries, actor *uuid.UUID, kind string, dest *uuid.UUID, destName string, detail map[string]any) error {
	if detail == nil {
		detail = map[string]any{}
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("encode change detail: %w", err)
	}
	// The id is made here, not by uuidv7() in the database: google/uuid's NewV7 is
	// strictly increasing within a process, so rows of one transaction (one `at`)
	// read back in the order they were written. The feed is the audit record.
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("make a change id: %w", err)
	}
	if _, err := q.InsertBackupChangeLog(ctx, dbq.InsertBackupChangeLogParams{
		ID: id, ActorID: actor, Kind: kind, DestinationID: dest, DestinationName: destName, Detail: b,
	}); err != nil {
		return fmt.Errorf("write the change log: %w", err)
	}
	return nil
}

// pgUnique reports a unique violation and the constraint (or index) name.
func pgUnique(err error) (constraint string, ok bool) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return pe.ConstraintName, true
	}
	return "", false
}

func invalid(format string, args ...any) error { return httpx.Invalid(format, args...) }

func conflict(format string, args ...any) error {
	return httpx.Errorf(http.StatusConflict, httpx.CodeConflict, format, args...)
}

func unavailable(format string, args ...any) error {
	return httpx.Errorf(http.StatusServiceUnavailable, httpx.CodeBackupUnavailable, format, args...)
}

func intp(v int16) *int {
	i := int(v)
	return &i
}
