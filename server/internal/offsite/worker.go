package offsite

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/backup"
	"devops/internal/backup/driver"
	"devops/internal/dbq"
	"devops/internal/netguard"
	"devops/internal/platform"
)

// Options configure a Worker. The zero value of a knob means its default; tests
// shorten the intervals and inject faults through the hooks.
type Options struct {
	Pool *pgxpool.Pool
	// DSN opens the dedicated connections (the LISTEN connection and one per
	// destination lock). They are not pool connections, so a lock can never be
	// lost or leaked by a pool checkout (decisions-T16 #31 step 1).
	DSN    string
	Log    *slog.Logger
	Key    []byte // BACKUP_ENCRYPTION_KEY, 32 bytes
	Policy netguard.Policy

	DumpDir    string // the backups volume (read only)
	WorkDir    string // the scratch volume
	HealthFile string
	Version    string // goes into each manifest

	// Silo is offsite's read-only access to the product's silo (ticket 14, LAUN-80).
	// Empty: no attachment backup. FilesTimeout is added to RunTimeout for a run when
	// silo is set (default 12h): the first copy of a large bucket takes hours.
	Silo         SiloConfig
	FilesTimeout time.Duration

	ScanEvery  time.Duration // default 30s (#30)
	WaitPoll   time.Duration // a waiting_dump run looks again every WaitPoll; default 5s
	LeaseEvery time.Duration // default 15s (#23)
	StallAfter time.Duration // no progress this long is `stalled`; default 120s (#40)
	RunTimeout time.Duration // a whole run; default 4h (#40)
	MaxBackups int           // backup and verify runs at once; default 2 (#40)

	// Test hooks. Production leaves them nil.
	FreeBytes   func(dir string) (uint64, error)
	ScratchWrap func(io.Writer) io.Writer
	Lookup      func(ctx context.Context, host string) ([]netip.Addr, error)
}

// Worker is the offsite worker. Safe for one Run; Scan may be called by tests.
type Worker struct {
	o      Options
	pool   *pgxpool.Pool
	log    *slog.Logger
	keys   *platform.BackupKeyring
	dialer *netguard.Dialer
	silo   *siloSource // nil: no attachment backup

	backupSem chan struct{}
	otherSem  chan struct{}
	wg        sync.WaitGroup

	mu     sync.Mutex
	active map[uuid.UUID]context.CancelCauseFunc
}

// Constants of #23, #31, #40. The numbers marked in the design as guesses are guesses.
const (
	leaseSeconds     = 60
	dumpMaxAge       = 6 * time.Hour    // #28: an older newest dump makes the run ask for an extra one
	dumpStartDelay   = 30 * time.Minute // #23: no dump started this long after the request
	dumpRunningLimit = 6 * time.Hour    // #23: a started dump still running this long
	otherRunLimit    = 4                // probe, test and exchange runs at once (a guess)
	dialTimeout      = 15 * time.Second // #40
)

// New checks the options and builds a Worker.
func New(o Options) (*Worker, error) {
	if o.Pool == nil || o.DSN == "" {
		return nil, errors.New("offsite: a pool and a DSN are required")
	}
	if o.DumpDir == "" || o.WorkDir == "" {
		return nil, errors.New("offsite: DumpDir and WorkDir are required")
	}
	keys, err := platform.NewBackupKeyring(o.Key)
	if err != nil {
		return nil, fmt.Errorf("offsite: %w", err)
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.ScanEvery, 30*time.Second)
	def(&o.WaitPoll, 5*time.Second)
	def(&o.LeaseEvery, 15*time.Second)
	def(&o.StallAfter, 120*time.Second)
	def(&o.RunTimeout, 4*time.Hour)
	def(&o.FilesTimeout, 12*time.Hour)
	if o.MaxBackups <= 0 {
		o.MaxBackups = 2
	}
	if o.Version == "" {
		o.Version = "unversioned"
	}
	if o.FreeBytes == nil {
		o.FreeBytes = freeBytes
	}
	var silo *siloSource
	if o.Silo.Enabled() {
		var err error
		if silo, err = newSiloSource(o.Silo, dialTimeout); err != nil {
			return nil, err
		}
	}
	return &Worker{
		silo: silo,
		o:    o, pool: o.Pool, log: o.Log, keys: keys,
		dialer:    backup.NewDialer(o.Policy, dialTimeout),
		backupSem: make(chan struct{}, o.MaxBackups),
		otherSem:  make(chan struct{}, otherRunLimit),
		active:    map[uuid.UUID]context.CancelCauseFunc{},
	}, nil
}

// Wait blocks until every run goroutine has ended.
func (w *Worker) Wait() { w.wg.Wait() }

// Run is the worker loop: boot, then a scan on every wake-up and every ScanEvery,
// until ctx ends (decisions-T16 #30).
func (w *Worker) Run(ctx context.Context) error {
	w.touch()
	go w.healthLoop(ctx)
	if err := w.Boot(ctx); err != nil {
		return err
	}
	wake := make(chan struct{}, 1)
	go w.listen(ctx, wake)
	t := time.NewTicker(w.o.ScanEvery)
	defer t.Stop()
	for {
		w.Scan(ctx)
		select {
		case <-ctx.Done():
			w.wg.Wait()
			return nil
		case <-wake:
		case <-t.C:
		}
	}
}

// Boot waits for the role and the migration, then clears what a crash left:
// stale test secrets and scratch files, and makes the instance file and the signing key.
func (w *Worker) Boot(ctx context.Context) error {
	if err := w.waitReady(ctx); err != nil {
		return err
	}
	q := dbq.New(w.pool)
	if _, err := q.ClearStaleBackupTestSecrets(ctx); err != nil {
		w.log.Error("offsite: cannot clear stale test secrets", "err", err)
	}
	w.cleanScratch(ctx)
	w.checkInstance(ctx)
	if err := w.ensureSigningKey(ctx); err != nil {
		w.log.Error("offsite: signing key", "code", string(driver.CodeOf(err)))
	}
	return nil
}

// minMigration is the migration of ticket 1 (backups, 0019). The worker waits
// until it is applied (decisions-T16 #15).
const minMigration = 19

// waitReady blocks until the role can connect and schema_migrations shows
// migration 0019 and no dirty state. It touches the health file while it waits,
// so a first deploy cannot deadlock on the health check (#46).
func (w *Worker) waitReady(ctx context.Context) error {
	for {
		w.touch()
		var version int64
		var dirty bool
		err := w.pool.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&version, &dirty)
		if err == nil && !dirty && version >= minMigration {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			w.log.Warn("offsite: waiting for the database role and the migration")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// Scan is one pass of the worker (#30, #31). Errors are logged and the pass goes on:
// one failing step must not stop the others.
func (w *Worker) Scan(ctx context.Context) {
	w.touch()
	q := dbq.New(w.pool)
	if err := w.ensureSigningKey(ctx); err != nil {
		w.log.Error("offsite: signing key", "code", string(driver.CodeOf(err)))
	}
	instanceOK := w.checkInstance(ctx)

	if ids, err := q.InterruptExpiredBackupRuns(ctx); err != nil {
		w.log.Error("offsite: interrupt expired runs failed", "err", err)
	} else if len(ids) > 0 {
		w.log.Warn("offsite: runs interrupted (their lease ran out)", "count", len(ids))
	}
	// A killed run's lease is now expired and the run is `interrupted`: its scratch
	// file (dump sized) goes now, not at the next restart.
	w.cleanScratch(ctx)
	if _, err := q.ClearStaleBackupTestSecrets(ctx); err != nil {
		w.log.Error("offsite: cannot clear stale test secrets", "err", err)
	}
	if ids, err := q.CancelOrphanedBackupRuns(ctx); err != nil {
		w.log.Error("offsite: cancel runs of a deleted or disabled destination failed", "err", err)
	} else {
		w.abort(ids...)
	}
	if !instanceOK {
		if n, err := q.FailBackupRunsInstanceChanged(ctx, fixedText(driver.InstanceChanged)); err != nil {
			w.log.Error("offsite: instance_changed failure", "err", err)
		} else if n > 0 {
			w.log.Error("offsite: this database is not the one that made the backups; run deploy.sh --backup-resume", "failed_runs", n)
		}
	} else {
		w.schedule(ctx)
		w.scheduleVerifies(ctx)
	}
	w.dispatch(ctx, instanceOK)
}

// abort cancels the contexts of runs this worker is executing.
func (w *Worker) abort(ids ...uuid.UUID) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, id := range ids {
		if c, ok := w.active[id]; ok {
			c(errLost)
		}
	}
}

func (w *Worker) register(id uuid.UUID, c context.CancelCauseFunc) {
	w.mu.Lock()
	w.active[id] = c
	w.mu.Unlock()
}

func (w *Worker) unregister(id uuid.UUID) {
	w.mu.Lock()
	delete(w.active, id)
	w.mu.Unlock()
}

// touch refreshes the health file: the process is alive.
func (w *Worker) touch() {
	if w.o.HealthFile == "" {
		return
	}
	now := time.Now()
	if err := os.Chtimes(w.o.HealthFile, now, now); err != nil {
		if f, err := os.OpenFile(w.o.HealthFile, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			_ = f.Close()
		}
	}
}

func (w *Worker) healthLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.touch()
		}
	}
}

// uuidT keeps the files short.
type uuidT = uuid.UUID
