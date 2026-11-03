//go:build backuptest

package offsite

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/backup"
	"devops/internal/backup/driver"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/netguard"
	"devops/internal/testdb"
)

// syncBuf is a log sink the tests can read while the worker writes.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// fake is a driver for the type `s3` in tests. It stores objects in a directory
// like the local driver, and lets a test inject faults and see what it was given.
type fake struct {
	mu   sync.Mutex
	dir  string
	puts atomic.Int32
	// before runs first in Put; an error fails the upload; it may block.
	before func(ctx context.Context, name string) error
	// put replaces the upload when set.
	put       func(ctx context.Context, name string, size int64, r io.Reader) error
	del       func(name string) error
	deleted   []string
	probe     driver.ProbeResult
	probeErr  error
	openErr   error
	opened    []driver.Config
	testSteps func() []driver.Step
	exchange  func(c driver.Config, code string) (string, error)
}

type fakeDriver struct{ f *fake }

func (d fakeDriver) Validate(raw json.RawMessage) error { _, err := d.dir(raw); return err }

func (d fakeDriver) dir(raw json.RawMessage) (string, error) {
	var m struct {
		Dir string `json:"dir"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.Dir == "" {
		return "", driver.Errf(driver.ServerError)
	}
	return m.Dir, nil
}

func (d fakeDriver) Identity(raw json.RawMessage) (driver.Identity, error) {
	dir, err := d.dir(raw)
	return driver.Identity{Type: "s3", Folder: strings.Trim(dir, "/")}, err
}

func (d fakeDriver) Probe(context.Context, driver.Config) (driver.ProbeResult, error) {
	return d.f.probe, d.f.probeErr
}

func (d fakeDriver) Open(ctx context.Context, c driver.Config) (driver.Remote, error) {
	d.f.mu.Lock()
	cc := c
	cc.Config = append(json.RawMessage(nil), c.Config...)
	cc.Secrets = append(json.RawMessage(nil), c.Secrets...)
	d.f.opened = append(d.f.opened, cc)
	d.f.mu.Unlock()
	if d.f.openErr != nil {
		return nil, d.f.openErr
	}
	dir, err := d.dir(c.Config)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { // a files sub folder (ticket 14)
		return nil, driver.Errf(driver.ServerError)
	}
	cfg, _ := json.Marshal(map[string]string{"dir": dir})
	lr, err := driver.Local{}.Open(ctx, driver.Config{Config: cfg})
	if err != nil {
		return nil, err
	}
	return &fakeRemote{Remote: lr, f: d.f}, nil
}

func (d fakeDriver) Exchange(_ context.Context, c driver.Config, code string) (string, error) {
	if d.f.exchange == nil {
		return "", driver.Errf(driver.ServerError)
	}
	return d.f.exchange(c, code)
}

type fakeRemote struct {
	driver.Remote
	f *fake
}

func (r *fakeRemote) Put(ctx context.Context, name string, size int64, rd io.Reader) error {
	r.f.puts.Add(1)
	if r.f.before != nil {
		if err := r.f.before(ctx, name); err != nil {
			return err
		}
	}
	if r.f.put != nil {
		return r.f.put(ctx, name, size, rd) // a hook replaces the upload
	}
	return r.Remote.Put(ctx, name, size, rd)
}

func (r *fakeRemote) Delete(ctx context.Context, name string) error {
	r.f.mu.Lock()
	r.f.deleted = append(r.f.deleted, name)
	r.f.mu.Unlock()
	if r.f.del != nil {
		if err := r.f.del(name); err != nil {
			return err
		}
	}
	return r.Remote.Delete(ctx, name)
}

func (r *fakeRemote) Test(ctx context.Context, name string) []driver.Step {
	if r.f.testSteps != nil {
		return r.f.testSteps()
	}
	return r.Remote.Test(ctx, name)
}

func (f *fake) openedConfigs() []driver.Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]driver.Config(nil), f.opened...)
}

type harness struct {
	t         *testing.T
	pool      *pgxpool.Pool
	dsn       string
	dumpDir   string
	workDir   string
	remoteDir string
	key       []byte
	log       *syncBuf
	fake      *fake
	agePriv   string
	agePub    string
	w         *Worker
	dumpN     int
	base      time.Time // fixed once: dump names never depend on the wall clock
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	pool, dsn := testdb.New(t)
	h := &harness{t: t, pool: pool, dsn: dsn, log: &syncBuf{},
		dumpDir: t.TempDir(), workDir: t.TempDir(), remoteDir: t.TempDir(), key: bytes32(t), base: time.Now().UTC().Truncate(time.Second)}
	h.fake = &fake{dir: h.remoteDir}
	h.fake.probe = driver.ProbeResult{Kind: "tls", Fingerprint: "fp-probed", ChainVerified: true}
	driver.RegisterForTest(t, "s3", fakeDriver{f: h.fake})
	var err error
	if h.agePriv, h.agePub, err = backup.GenerateKeyPair(); err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE backup_settings SET recipients = $1::jsonb, ever_had_recipient = true`, h.recipientsJSON(h.agePub))
	h.w = h.newWorker(h.pool, h.dsn, nil)
	return h
}

func bytes32(t *testing.T) []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func (h *harness) recipientsJSON(keys ...string) string {
	var out []map[string]any
	for _, k := range keys {
		out = append(out, map[string]any{"publicKey": k, "fingerprint": backup.Fingerprint(k), "addedAt": "2026-01-01T00:00:00Z",
			"addedBy": nil, "effectiveAt": "2026-01-01T00:00:00Z", "removeAt": nil, "removeBy": nil})
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// newWorker builds a Worker with fast intervals on a pool; mut may change the options.
func (h *harness) newWorker(pool *pgxpool.Pool, dsn string, mut func(*Options)) *Worker {
	h.t.Helper()
	o := Options{Pool: pool, DSN: dsn, Log: slog.New(slog.NewJSONHandler(h.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Key: h.key, Policy: netguard.Policy{}, DumpDir: h.dumpDir, WorkDir: h.workDir,
		HealthFile: filepath.Join(h.workDir, "..", "alive-"+uuid.NewString()),
		ScanEvery:  time.Hour, WaitPoll: 20 * time.Millisecond, LeaseEvery: 100 * time.Millisecond,
		StallAfter: 10 * time.Second, FreeBytes: func(string) (uint64, error) { return 1 << 50, nil }}
	if mut != nil {
		mut(&o)
	}
	w, err := New(o)
	if err != nil {
		h.t.Fatal(err)
	}
	return w
}

func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	testdb.MustExec(h.t, h.pool, sql, args...)
}

// addDest makes an enabled destination of type s3 (the fake) created now.
func (h *harness) addDest(name string) uuid.UUID {
	h.t.Helper()
	id := uuid.Must(uuid.NewV7())
	cfg, _ := json.Marshal(map[string]string{"dir": h.remoteDir})
	h.exec(`INSERT INTO backup_destinations (id, name, type, config, enabled, enabled_since) VALUES ($1, $2, 's3', $3, true, now())`,
		id, name, cfg)
	return id
}

type dumpInfo struct {
	id      uuid.UUID
	path    string
	content []byte
	name    string
}

// addDump writes a dump file and an ok row that started `age` ago.
func (h *harness) addDump(age time.Duration, content []byte) dumpInfo {
	h.t.Helper()
	h.dumpN++
	if content == nil {
		content = make([]byte, 100_000)
		_, _ = rand.Read(content)
	}
	ts := h.base.Add(-age).Add(-time.Duration(h.dumpN) * time.Second)
	name := "devops-" + ts.Format("20060102T150405Z") + ".dump"
	p := filepath.Join(h.dumpDir, name)
	if err := os.WriteFile(p, content, 0o600); err != nil {
		h.t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	id := uuid.Must(uuid.NewV7())
	h.exec(`INSERT INTO backup_dumps (id, file_name, status, started_at, finished_at, bytes, sha256, migration_version)
		VALUES ($1, $2, 'ok', now() - $3::interval, now() - $3::interval, $4, $5, 20)`,
		id, name, fmt.Sprintf("%d seconds", int(age.Seconds())), int64(len(content)), hex.EncodeToString(sum[:]))
	return dumpInfo{id: id, path: p, content: content, name: name}
}

// queue inserts a queued run for a destination.
func (h *harness) queue(dest uuid.UUID, trigger string) uuid.UUID {
	h.t.Helper()
	id := uuid.Must(uuid.NewV7())
	h.exec(`INSERT INTO backup_runs (id, destination_id, destination_name, trigger, status)
		SELECT $1, d.id, d.name, $3, 'queued' FROM backup_destinations d WHERE d.id = $2`, id, dest, trigger)
	return id
}

func (h *harness) run(id uuid.UUID) dbq.BackupRun {
	h.t.Helper()
	r, err := dbq.New(h.pool).GetBackupRun(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func (h *harness) scan() {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	h.w.Scan(ctx)
	h.w.Wait()
}

// waitFor polls until cond is true.
func (h *harness) waitFor(what string, d time.Duration, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s\nlog:\n%s", what, h.log.String())
}

func (h *harness) waitStatus(id uuid.UUID, d time.Duration, statuses ...string) dbq.BackupRun {
	h.t.Helper()
	h.waitFor(fmt.Sprintf("run %s to be %v", id, statuses), d, func() bool {
		s := h.run(id).Status
		for _, w := range statuses {
			if s == w {
				return true
			}
		}
		return false
	})
	return h.run(id)
}

func (h *harness) remoteFiles() []string {
	des, _ := os.ReadDir(h.remoteDir)
	var out []string
	for _, de := range des {
		if !strings.HasPrefix(de.Name(), ".put-") {
			out = append(out, de.Name())
		}
	}
	return out
}

func (h *harness) scratchFiles() []string {
	des, _ := os.ReadDir(h.workDir)
	var out []string
	for _, de := range des {
		if scratchRe.MatchString(de.Name()) {
			out = append(out, de.Name())
		}
	}
	return out
}

func (h *harness) settings() dbq.BackupSetting {
	s, err := dbq.New(h.pool).GetBackupSettings(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

// lockFree reports whether nobody holds the destination lock. It tries on a
// connection it releases at once, and unlocks if it got the lock.
func (h *harness) lockFree(dest uuid.UUID) bool {
	h.t.Helper()
	ctx := context.Background()
	c, err := h.pool.Acquire(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	defer c.Release()
	var got bool
	if err := c.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockKey(dest)).Scan(&got); err != nil {
		h.t.Fatal(err)
	}
	if got {
		if _, err := c.Exec(ctx, `SELECT pg_advisory_unlock($1)`, lockKey(dest)); err != nil {
			h.t.Fatal(err)
		}
	}
	return got
}

func dbOpen(dsn string) (*pgxpool.Pool, error) { return database.Open(context.Background(), dsn) }

var _ = io.Discard
