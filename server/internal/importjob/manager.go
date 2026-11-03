package importjob

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/dbq"
	"devops/internal/httpx"
	"devops/internal/importer"
	"devops/internal/importer/linear"
)

// Defaults of Config.
const (
	DefaultWorkDir       = "/work/import"
	DefaultHeartbeat     = 5 * time.Second
	DefaultProgressEvery = time.Second
	DefaultStaleAfter    = 60 * time.Second
	DefaultSweepEvery    = time.Minute
	DefaultSessionTTL    = 7 * 24 * time.Hour
	// DefaultFloorBytes is the free space below which an export is refused
	// with no_space (decisions-T17 #39, proposed).
	DefaultFloorBytes uint64 = 1 << 30
	// maxTotalCap is DownloadAll's own default; the run never asks for more.
	maxTotalCap int64 = 10 << 30
	// orphanAge is how old an unreferenced session directory must be before
	// the sweep removes it (a session makes its directory, then its row).
	orphanAge = time.Hour
)

// Config wires a Manager.
type Config struct {
	Pool *pgxpool.Pool
	// Store is the attachment store the files go to. Nil: the web import is
	// off and a session start answers 503 attachments_unavailable.
	Store importer.ObjectStore
	// WorkDir is IMPORT_WORK_DIR: the directory of the exports (mode 0700).
	WorkDir string
	// PublicBaseURL is PUBLIC_BASE_URL. Empty: a session start answers 409
	// public_url_required (the texts would get path-only download links).
	PublicBaseURL string
	// LinearEndpoint is the GraphQL endpoint (linear.Endpoint). Tests point it
	// at a fake; there is no environment variable for it.
	LinearEndpoint string
	Log            *slog.Logger
	Now            func() time.Time

	Heartbeat     time.Duration // the owner's beat and cancel check (5 s)
	ProgressEvery time.Duration // the progress write, at most (1 s)
	StaleAfter    time.Duration // a heartbeat older than this is a dead owner (60 s)
	SweepEvery    time.Duration // the sweep loop (1 minute)
	SessionTTL    time.Duration // an idle session is closed after this (7 days)

	// FloorBytes is the least free space for an export; MaxTotalBytes, when
	// set, replaces the limit derived from the free space (tests).
	FloorBytes    uint64
	MaxTotalBytes int64
	// FreeBytes reports the free space under a directory (tests replace it).
	FreeBytes func(dir string) (uint64, error)
}

// Manager runs import sessions and jobs. It is safe for concurrent use.
type Manager struct {
	cfg      Config
	q        *dbq.Queries
	log      *slog.Logger
	instance string

	base       context.Context
	baseCancel context.CancelCauseFunc
	wg         sync.WaitGroup

	mu      sync.Mutex
	running map[uuid.UUID]*jobRun

	// exportFn is linear.Export; the tests replace it (an in-package seam,
	// not a flag or a variable of the production binary).
	exportFn func(ctx context.Context, c *linear.Client, dl *linear.Downloader, opts linear.Options) (*importer.Export, error)
	// progressHook, when set by a test, runs in the job's goroutine at every
	// progress report, before it is recorded (a test blocks a job in the
	// middle of the import transaction with it).
	progressHook func(kind, phase string)
}

// New returns a Manager. Call Run (the sweep loop) or Close to stop it.
func New(cfg Config) *Manager {
	if cfg.WorkDir == "" {
		cfg.WorkDir = DefaultWorkDir
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.LinearEndpoint == "" {
		cfg.LinearEndpoint = linear.Endpoint
	}
	cfg.Heartbeat = orDur(cfg.Heartbeat, DefaultHeartbeat)
	cfg.ProgressEvery = orDur(cfg.ProgressEvery, DefaultProgressEvery)
	cfg.StaleAfter = orDur(cfg.StaleAfter, DefaultStaleAfter)
	cfg.SweepEvery = orDur(cfg.SweepEvery, DefaultSweepEvery)
	cfg.SessionTTL = orDur(cfg.SessionTTL, DefaultSessionTTL)
	if cfg.FloorBytes == 0 {
		cfg.FloorBytes = DefaultFloorBytes
	}
	if cfg.FreeBytes == nil {
		cfg.FreeBytes = freeBytes
	}
	var id [8]byte
	_, _ = rand.Read(id[:])
	base, cancel := context.WithCancelCause(context.Background())
	return &Manager{cfg: cfg, q: dbq.New(cfg.Pool), log: cfg.Log, instance: hex.EncodeToString(id[:]),
		base: base, baseCancel: cancel, running: map[uuid.UUID]*jobRun{}, exportFn: linear.Export}
}

func orDur(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}

// freeBytes is the space a non-root user can still use under dir.
func freeBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", dir, err)
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil //nolint:gosec // both are non-negative
}

// Close stops every running job (each rolls back and is marked interrupted)
// and waits for them. It is idempotent.
func (m *Manager) Close() {
	m.baseCancel(errShutdown)
	m.wg.Wait()
}

// Run sweeps once now and then every SweepEvery until ctx ends, then stops
// the jobs (Close). A failed sweep is logged and the loop goes on.
func (m *Manager) Run(ctx context.Context) {
	defer m.Close()
	once := func() {
		if err := m.Sweep(ctx); err != nil && ctx.Err() == nil {
			m.log.ErrorContext(ctx, "import sweep failed", "err", err)
		}
	}
	once()
	t := time.NewTicker(m.cfg.SweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			once()
		}
	}
}

// Cancel causes of a job context.
var (
	errUserCanceled = errors.New("canceled by an admin")
	errShutdown     = errors.New("the server is shutting down")
	errLost         = errors.New("the job row was taken over by the sweep")
)

// Errors of the API (decisions-T17 #12).
func errConflict(format string, args ...any) *httpx.Error {
	return httpx.Errorf(http.StatusConflict, httpx.CodeConflict, format, args...)
}

func errJobActive() *httpx.Error {
	return httpx.Errorf(http.StatusConflict, httpx.CodeJobActive, "an import job is already running; wait for it or cancel it")
}

func isUnique(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

func notFound(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return httpx.NotFound(what)
	}
	return fmt.Errorf("%s lookup: %w", what, err)
}
