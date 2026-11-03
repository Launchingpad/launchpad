//go:build backuptest

package offsite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/backup/driver"
)

func (h *harness) request(age time.Duration) uuid.UUID {
	h.t.Helper()
	id := uuid.Must(uuid.NewV7())
	h.exec(`INSERT INTO backup_requests (id, state, requested_by, requested_at) VALUES ($1, 'taken', 'app', now() - $2::interval)`,
		id, age.String())
	return id
}

func (h *harness) queueWithRequest(dest uuid.UUID, trigger string, req uuid.UUID) uuid.UUID {
	h.t.Helper()
	id := h.queue(dest, trigger)
	h.exec(`UPDATE backup_runs SET request_id = $2 WHERE id = $1`, id, req)
	return id
}

// A waiting_dump run fails dump_unavailable when no dump started within 30 minutes of
// the request; the failure is not retried.
func TestWaitingDump_NoDumpStartedIn30Minutes(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	id := h.queueWithRequest(dest, "manual", h.request(31*time.Minute))
	h.scan()
	r := h.run(id)
	if r.Status != "failed" || r.ErrorCode != "dump_unavailable" || r.WillRetry {
		t.Fatalf("run = %+v", r)
	}
	var n int
	_ = h.pool.QueryRow(context.Background(), `SELECT count(*) FROM backup_runs WHERE trigger = 'retry'`).Scan(&n)
	if n != 0 {
		t.Fatal("dump_unavailable made a retry")
	}
}

// A dump that started after the request and ended failed fails the run at once, not
// after 30 minutes.
func TestWaitingDump_FailedDumpFailsAtOnce(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	req := h.request(time.Minute)
	h.exec(`INSERT INTO backup_dumps (file_name, status, started_at, finished_at, error_text)
		VALUES ('devops-20260101T000000Z.dump', 'failed', now() - interval '30 seconds', now(), 'pg_dump failed')`)
	id := h.queueWithRequest(dest, "manual", req)
	h.scan()
	if r := h.run(id); r.Status != "failed" || r.ErrorCode != "dump_unavailable" {
		t.Fatalf("run = %+v", r)
	}
}

// A started dump still running after 6 hours fails the run; a slow one does not.
func TestWaitingDump_SlowDumpWaitsAndStuckDumpFails(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	req := h.request(40 * time.Minute)
	h.exec(`INSERT INTO backup_dumps (file_name, status, started_at) VALUES ('devops-20260101T000000Z.dump', 'running', now() - interval '35 minutes')`)
	id := h.queueWithRequest(dest, "manual", req)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.w.Scan(ctx)
	h.waitFor("the run waits for the dump", 5*time.Second, func() bool {
		r := h.run(id)
		return r.Status == "waiting_dump" && r.LockedUntil != nil
	})
	if r := h.run(id); r.Status != "waiting_dump" || r.LockedUntil == nil || r.StartedAt != nil {
		t.Fatalf("a slow dump must not fail the run: %+v", r)
	}
	// The same dump, running for 7 hours: the run fails.
	h.exec(`UPDATE backup_dumps SET started_at = now() - interval '7 hours' WHERE status = 'running'`)
	h.exec(`UPDATE backup_requests SET requested_at = now() - interval '7 hours 5 minutes'`)
	if r := h.waitStatus(id, 5*time.Second, "failed"); r.ErrorCode != "dump_unavailable" {
		t.Fatalf("run = %+v", r)
	}
	h.w.Wait()
}

// The waiting run holds neither the lock nor a slot; it takes the lock when the dump
// is ready and moves to running; a dump that started BEFORE the request is not used.
func TestWaitingDump_LockHandoverAndOlderDumpNotUsed(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	old := h.addDump(10*time.Minute, nil) // started before the request: never used for it
	req := h.request(0)
	id := h.queueWithRequest(dest, "manual", req)
	var heldDuringPut atomic.Bool
	h.fake.before = func(ctx context.Context, name string) error {
		heldDuringPut.Store(!h.lockFree(dest))
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.w.Scan(ctx)
	h.waitFor("the run waits and holds neither the lock nor a slot", 5*time.Second, func() bool {
		return h.run(id).Status == "waiting_dump" && h.lockFree(dest) && len(h.w.backupSem) == 0
	})
	r := h.run(id)
	if r.Status != "waiting_dump" || r.DumpID != nil {
		t.Fatalf("the older dump was used: %+v (dump %s)", r, old.id)
	}
	if !h.lockFree(dest) {
		t.Fatal("a waiting_dump run holds the destination lock")
	}
	if len(h.w.backupSem) != 0 {
		t.Fatal("a waiting_dump run holds a slot")
	}
	fresh := h.addDump(0, nil) // started after the request
	r = h.waitStatus(id, 5*time.Second, "succeeded", "failed")
	if r.Status != "succeeded" || r.DumpID == nil || *r.DumpID != fresh.id {
		t.Fatalf("run = %+v\n%s", r, h.log.String())
	}
	if !heldDuringPut.Load() {
		t.Fatal("the run did not hold the lock while it ran")
	}
	h.w.Wait()
	h.waitFor("the lock to be released", 5*time.Second, func() bool { return h.lockFree(dest) })
}

// A run whose newest dump is older than 6 hours asks for an extra dump (offsite as the
// requester); two destinations share ONE request; the run uploads the new dump.
func TestExtraDump_OneRequestForTwoDestinations(t *testing.T) {
	h := newHarness(t)
	a, b := h.addDest("a"), h.addDest("b")
	h.addDump(7*time.Hour, nil)
	ra, rb := h.queue(a, "schedule"), h.queue(b, "schedule")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.w.Scan(ctx)
	var n int
	var by string
	_ = h.pool.QueryRow(ctx, `SELECT count(*), coalesce(max(requested_by), '') FROM backup_requests`).Scan(&n, &by)
	if n != 1 || by != "offsite" {
		t.Fatalf("requests = %d by %q, want one by offsite", n, by)
	}
	runA, runB := h.run(ra), h.run(rb)
	if runA.Status != "waiting_dump" || runB.Status != "waiting_dump" || runA.RequestID == nil || runB.RequestID == nil || *runA.RequestID != *runB.RequestID {
		t.Fatalf("runs = %+v %+v", runA, runB)
	}
	h.addDump(0, nil)
	h.waitStatus(ra, 5*time.Second, "succeeded", "failed")
	h.waitStatus(rb, 5*time.Second, "succeeded", "failed")
	if h.run(ra).Status != "succeeded" || h.run(rb).Status != "succeeded" {
		t.Fatalf("log:\n%s", h.log.String())
	}
	h.w.Wait()
}

// A changed dump file fails dump_mismatch BEFORE any upload, with the scratch file
// deleted; so does a file of another size.
func TestRun_DumpMismatch(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	dump := h.addDump(10*time.Minute, nil)
	changed := append([]byte(nil), dump.content...)
	changed[10] ^= 0xff // same size, different bytes
	if err := os.WriteFile(dump.path, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	id := h.queue(dest, "manual")
	h.scan()
	r := h.run(id)
	if r.Status != "failed" || r.ErrorCode != "dump_mismatch" || r.WillRetry {
		t.Fatalf("same size: %+v", r)
	}
	if h.fake.puts.Load() != 0 || len(h.remoteFiles()) != 0 || len(h.scratchFiles()) != 0 {
		t.Fatalf("puts=%d remote=%v scratch=%v", h.fake.puts.Load(), h.remoteFiles(), h.scratchFiles())
	}
	// A different size fails before the encryption is even started.
	if err := os.WriteFile(dump.path, changed[:len(changed)-1], 0o600); err != nil {
		t.Fatal(err)
	}
	id = h.queue(dest, "manual")
	h.scan()
	if r := h.run(id); r.Status != "failed" || r.ErrorCode != "dump_mismatch" {
		t.Fatalf("other size: %+v", r)
	}
}

// A dump that is gone, a name that is not a dump name and an ok dump with no hash are
// not used.
func TestRun_UnusableDumps(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	dump := h.addDump(10*time.Minute, nil)
	if err := os.Remove(dump.path); err != nil {
		t.Fatal(err)
	}
	req := h.request(31 * time.Minute)
	id := h.queueWithRequest(dest, "manual", req)
	h.scan()
	if r := h.run(id); r.Status != "failed" || r.ErrorCode != "dump_unavailable" {
		t.Fatalf("missing file: %+v", r)
	}
	// A symlink in the backups volume is not a regular file.
	h.exec(`DELETE FROM backup_runs`)
	target := h.dumpDir + "/real"
	_ = os.WriteFile(target, dump.content, 0o600)
	_ = os.Symlink(target, dump.path)
	id = h.queueWithRequest(dest, "manual", req)
	h.scan()
	if r := h.run(id); r.Status != "failed" || r.ErrorCode != "dump_unavailable" {
		t.Fatalf("symlink: %+v", r)
	}
}

// no_space is reported before any work: no scratch file, no upload.
func TestRun_NoSpaceBeforeWork(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	dump := h.addDump(10*time.Minute, nil)
	var wrapped atomic.Bool
	h.w = h.newWorker(h.pool, h.dsn, func(o *Options) {
		o.FreeBytes = func(string) (uint64, error) { return uint64(len(dump.content)) - 1, nil }
		o.ScratchWrap = func(w io.Writer) io.Writer { wrapped.Store(true); return w }
	})
	id := h.queue(dest, "manual")
	h.scan()
	r := h.run(id)
	if r.Status != "failed" || r.ErrorCode != "no_space" || r.WillRetry {
		t.Fatalf("run = %+v", r)
	}
	if wrapped.Load() || h.fake.puts.Load() != 0 || len(h.scratchFiles()) != 0 {
		t.Fatal("work was done before the space check failed")
	}
}

// Two concurrent runs on a volume sized for one: the second fails no_space (it counts
// the dump of the run that is already running).
func TestRun_TwoConcurrentRunsOnAVolumeForOne(t *testing.T) {
	h := newHarness(t)
	a, b := h.addDest("a"), h.addDest("b")
	dump := h.addDump(10*time.Minute, nil)
	h.w = h.newWorker(h.pool, h.dsn, func(o *Options) {
		o.FreeBytes = func(string) (uint64, error) { return uint64(len(dump.content)) * 3 / 2, nil }
	})
	started, release := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	h.fake.before = func(ctx context.Context, name string) error {
		if once.CompareAndSwap(false, true) {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	ra := h.queue(a, "manual")
	ctx := context.Background()
	h.w.Scan(ctx)
	<-started
	rb := h.queue(b, "manual")
	h.w.Scan(ctx)
	if r := h.waitStatus(rb, 5*time.Second, "failed", "succeeded"); r.Status != "failed" || r.ErrorCode != "no_space" {
		t.Fatalf("second run = %+v", r)
	}
	close(release)
	if r := h.waitStatus(ra, 5*time.Second, "succeeded", "failed"); r.Status != "succeeded" {
		t.Fatalf("first run = %+v", r)
	}
	h.w.Wait()
}

// ENOSPC in the middle of the encryption is scratch_full, not retried, and the scratch
// file is removed. A failing read of the dump is dump_unavailable instead.
func TestRun_ScratchFullMidEncryption(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	h.addDump(10*time.Minute, make([]byte, 400_000))
	h.w = h.newWorker(h.pool, h.dsn, func(o *Options) {
		o.ScratchWrap = func(w io.Writer) io.Writer { return &failAfter{w: w, left: 70_000} }
	})
	id := h.queue(dest, "manual")
	h.scan()
	r := h.run(id)
	if r.Status != "failed" || r.ErrorCode != "scratch_full" || r.WillRetry {
		t.Fatalf("run = %+v\n%s", r, h.log.String())
	}
	if len(h.scratchFiles()) != 0 || h.fake.puts.Load() != 0 {
		t.Fatalf("scratch=%v puts=%d", h.scratchFiles(), h.fake.puts.Load())
	}
}

type failAfter struct {
	w    io.Writer
	left int
}

func (f *failAfter) Write(p []byte) (int, error) {
	if len(p) > f.left {
		return 0, syscall.ENOSPC
	}
	f.left -= len(p)
	return f.w.Write(p)
}

// No progress for StallAfter is `stalled`, which is retried once.
func TestRun_Stalled(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	h.addDump(10*time.Minute, nil)
	h.w = h.newWorker(h.pool, h.dsn, func(o *Options) { o.StallAfter = 300 * time.Millisecond })
	h.fake.put = func(ctx context.Context, name string, size int64, r io.Reader) error {
		buf := make([]byte, 1024)
		_, _ = r.Read(buf)
		<-ctx.Done()
		return ctx.Err()
	}
	id := h.queue(dest, "manual")
	h.scan()
	r := h.run(id)
	if r.Status != "failed" || r.ErrorCode != "stalled" || !r.WillRetry {
		t.Fatalf("run = %+v\n%s", r, h.log.String())
	}
	if len(h.scratchFiles()) != 0 {
		t.Fatal("scratch left")
	}
}

// A failed upload deletes its own partial remote objects, best effort (#18).
func TestRun_FailedUploadDeletesItsPartialObject(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	h.addDump(10*time.Minute, nil)
	h.fake.put = func(ctx context.Context, name string, size int64, r io.Reader) error {
		return driver.Errf(driver.AuthFailed)
	}
	id := h.queue(dest, "manual")
	h.scan()
	r := h.run(id)
	if r.Status != "failed" || r.RemoteName == nil {
		t.Fatalf("run = %+v", r)
	}
	h.fake.mu.Lock()
	deleted := append([]string(nil), h.fake.deleted...)
	h.fake.mu.Unlock()
	if len(deleted) != 2 {
		t.Fatalf("deleted = %v, want the object and the manifest names", deleted)
	}
}

// Retention keeps the newest N pairs after a verified upload, and a failing delete is
// a warning: the run stays succeeded with prune_failed.
func TestRun_PruneAndPruneFailed(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	h.exec(`UPDATE backup_destinations SET keep_last = 2 WHERE id = $1`, dest)
	for i, age := range []time.Duration{50 * time.Minute, 40 * time.Minute, 30 * time.Minute} {
		h.addDump(age, nil) // each run sees a newer dump, so the newest names sort last
		id := h.queue(dest, "manual")
		h.scan()
		if r := h.run(id); r.Status != "succeeded" || r.ErrorText != "" {
			t.Fatalf("run %d = %+v\n%s", i, r, h.log.String())
		}
	}
	if files := h.remoteFiles(); len(files) != 4 {
		t.Fatalf("after 3 runs with keep_last 2 the remote holds %v", files)
	}
	h.fake.del = func(string) error { return driver.ServerErr(500) }
	h.addDump(20*time.Minute, nil)
	id := h.queue(dest, "manual")
	h.scan()
	if r := h.run(id); r.Status != "succeeded" || r.ErrorText != "prune_failed" {
		t.Fatalf("prune failure: %+v", r)
	}
}

// A large dump goes through with bounded memory: the pipeline streams file -> age ->
// scratch -> upload and never holds the dump (decisions-T16 #18).
func TestLargeDump_BoundedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	h := newHarness(t)
	dest := h.addDest("main")
	const size = 256 << 20
	d := h.addDump(10*time.Minute, []byte("x"))
	f, err := os.OpenFile(d.path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	h2 := sha256.New()
	for i := 0; i < size>>20; i++ {
		chunk[0] = byte(i)
		_, _ = f.Write(chunk)
		_, _ = h2.Write(chunk)
	}
	_ = f.Close()
	h.exec(`UPDATE backup_dumps SET bytes = $2, sha256 = $3 WHERE id = $1`, d.id, int64(size), hex.EncodeToString(h2.Sum(nil)))

	var peak atomic.Uint64
	h.fake.put = func(ctx context.Context, name string, sz int64, r io.Reader) error {
		out, err := os.Create(filepath.Join(h.remoteDir, name))
		if err != nil {
			return err
		}
		defer out.Close()
		buf := make([]byte, 1<<20)
		for {
			n, err := r.Read(buf)
			_, _ = out.Write(buf[:n])
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if m.HeapInuse > peak.Load() {
				peak.Store(m.HeapInuse)
			}
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}
	id := h.queue(dest, "manual")
	h.scan()
	r := h.run(id)
	if r.Status != "succeeded" {
		t.Fatalf("run = %+v\n%s", r, h.log.String())
	}
	if p := peak.Load(); p > 128<<20 {
		t.Fatalf("heap in use peaked at %d MiB for a %d MiB dump", p>>20, size>>20)
	}
}
