//go:build backuptest

package offsite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/backup"
	"devops/internal/backup/driver"
	"devops/internal/backup/remotename"
)

// A manual run with a ready dump: the object is age-encrypted to the recipient, the
// manifest verifies with the published signing key, the run row is complete, and the
// scratch file is gone.
func TestRun_Success(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	dump := h.addDump(10*time.Minute, nil)
	id := h.queue(dest, "manual")

	h.scan()
	r := h.run(id)
	if r.Status != "succeeded" || r.ErrorCode != "" || r.LockedUntil != nil || r.TestSecretsEnc != nil || r.FinishedAt == nil {
		t.Fatalf("run = %+v\nlog:\n%s", r, h.log.String())
	}
	if r.DumpID == nil || *r.DumpID != dump.id || r.Bytes == nil || r.Sha256 == nil || r.RemoteName == nil || r.StartedAt == nil {
		t.Fatalf("run columns: %+v", r)
	}
	files := h.remoteFiles()
	if len(files) != 2 {
		t.Fatalf("remote files = %v", files)
	}
	s := h.settings()
	if s.SigningPublic == "" || s.SigningPrivateEnc == nil {
		t.Fatal("the signing key was not made at first use")
	}
	obj, err := os.ReadFile(filepath.Join(h.remoteDir, *r.RemoteName))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(obj)
	if hex.EncodeToString(sum[:]) != *r.Sha256 || int64(len(obj)) != *r.Bytes {
		t.Fatal("the run's sha256 and bytes are not the object's")
	}
	// The manifest verifies and binds to the object.
	stem := strings.TrimSuffix(*r.RemoteName, remotename.ObjectSuffix)
	mb, err := os.ReadFile(filepath.Join(h.remoteDir, remotename.ManifestName(stem)))
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := backup.ParsePublicKey(s.SigningPublic)
	m, err := backup.VerifyManifest(pub, mb)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Bind(s.InstallID, stem); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckObject(*r.Sha256, *r.Bytes); err != nil {
		t.Fatal(err)
	}
	if m.SchemaVersion != 20 || m.LaunchpadVersion != "unversioned" {
		t.Fatalf("manifest = %+v", m)
	}
	// It decrypts with the admin's private key to the dump.
	var plain bytes.Buffer
	if _, err := backup.Decrypt(&plain, bytes.NewReader(obj), h.agePriv, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plain.Bytes(), dump.content) {
		t.Fatal("the decrypted object is not the dump")
	}
	if len(h.scratchFiles()) != 0 {
		t.Fatalf("scratch left: %v", h.scratchFiles())
	}
	// The instance file was written (first run on the volume).
	if b, _ := os.ReadFile(filepath.Join(h.workDir, InstanceFile)); strings.TrimSpace(string(b)) != s.InstanceID.String() {
		t.Fatalf("instance file = %q", b)
	}
}

// The destination's secrets are opened and handed to the driver, and nothing of them
// is stored or logged.
func TestRun_SecretsReachTheDriverAndNothingElse(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	h.addDump(10*time.Minute, nil)
	const planted = "PLANTED-ACCESS-KEY-123"
	blob, err := h.w.keys.Seal("backup_destinations.secrets_enc", dest.String(), []byte(`{"accessKey":"`+planted+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE backup_destinations SET secrets_enc = $2, secrets_set = '{accessKey}' WHERE id = $1`, dest, blob)
	h.fake.put = func(ctx context.Context, name string, size int64, r io.Reader) error {
		_, _ = io.Copy(io.Discard, r)
		return driver.ServerErr(500) // a reply that "echoes" nothing
	}
	id := h.queue(dest, "manual")
	h.scan()
	r := h.run(id)
	if r.Status != "failed" {
		t.Fatalf("run = %+v", r)
	}
	cfgs := h.fake.openedConfigs()
	if len(cfgs) == 0 || !strings.Contains(string(cfgs[0].Secrets), planted) {
		t.Fatal("the driver did not get the opened secrets")
	}
	h.assertNoText(planted)
}

// assertNoText greps the log and every text of every backup table for s.
func (h *harness) assertNoText(s string) {
	h.t.Helper()
	if strings.Contains(h.log.String(), s) {
		h.t.Errorf("the log holds %q", s)
	}
	for _, q := range []string{
		`SELECT coalesce(string_agg(r::text, ' '), '') FROM backup_runs r`,
		`SELECT coalesce(string_agg(r::text, ' '), '') FROM backup_change_log r`,
		`SELECT coalesce(string_agg(r::text, ' '), '') FROM backup_settings r`,
		`SELECT coalesce(string_agg(r::text, ' '), '') FROM backup_requests r`,
		`SELECT coalesce(string_agg(r::text, ' '), '') FROM backup_dumps r`,
	} {
		var all string
		if err := h.pool.QueryRow(context.Background(), q).Scan(&all); err != nil {
			h.t.Fatal(err)
		}
		if strings.Contains(all, s) {
			h.t.Errorf("a backup row holds %q (%s)", s, q[:60])
		}
	}
}

// A run is claimed once even when two workers (two pools) scan at the same time.
func TestRun_TwoWorkersOneRun(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	h.addDump(10*time.Minute, nil)
	h.queue(dest, "manual")
	pool2 := h.cloneWorker()
	var wg sync.WaitGroup
	for _, w := range []*Worker{h.w, pool2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Scan(context.Background())
			w.Wait()
		}()
	}
	wg.Wait()
	if n := h.fake.puts.Load(); n != 2 { // one object and one manifest, from ONE run
		t.Fatalf("puts = %d, want 2 (one run)\nlog:\n%s", n, h.log.String())
	}
}

// cloneWorker is a second worker with its own pool on the same database.
func (h *harness) cloneWorker() *Worker {
	h.t.Helper()
	p2, err := dbOpen(h.dsn)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(p2.Close)
	return h.newWorker(p2, h.dsn, nil)
}

// While another session holds the destination lock the run is not claimed (nothing
// changes), and it runs when the lock is free. The lock lives on a dedicated
// connection, so heavy use of the pool does not lose it.
func TestLock_HeldElsewhereAndSurvivesPoolUse(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	h.addDump(10*time.Minute, nil)
	id := h.queue(dest, "manual")

	lock, ok, err := h.w.tryLock(context.Background(), dest)
	if err != nil || !ok {
		t.Fatalf("tryLock: %v %v", ok, err)
	}
	// Churn the pool: many checkouts and returns.
	for i := 0; i < 100; i++ {
		c, err := h.pool.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var one int
		_ = c.QueryRow(context.Background(), `SELECT 1`).Scan(&one)
		c.Release()
	}
	if h.lockFree(dest) {
		t.Fatal("the lock was lost after pool checkouts and returns")
	}
	// A second try-lock (another worker) fails.
	if l2, ok, _ := h.cloneWorker().tryLock(context.Background(), dest); ok {
		l2.release()
		t.Fatal("two workers hold one destination lock")
	}
	h.scan()
	if r := h.run(id); r.Status != "queued" || r.StartedAt != nil {
		t.Fatalf("a run was claimed while the lock was held: %+v", r)
	}
	lock.release()
	h.waitFor("the lock to be released", 5*time.Second, func() bool { return h.lockFree(dest) })
	h.scan()
	if r := h.run(id); r.Status != "succeeded" {
		t.Fatalf("run = %+v", r)
	}
}

// A run whose lease ran out (a killed worker) is marked interrupted; its lock is
// free; and it counts as a missed slot, so the next scan makes ONE catch-up run.
func TestRun_KilledRunIsInterruptedAndCaughtUp(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	h.addDump(10*time.Minute, nil)
	h.exec(`UPDATE backup_destinations SET created_at = now() - interval '3 days', enabled_since = now() - interval '3 days' WHERE id = $1`, dest)
	id := h.queue(dest, "schedule")
	h.exec(`UPDATE backup_runs SET status = 'running', started_at = now() - interval '10 minutes',
		locked_until = now() - interval '1 minute' WHERE id = $1`, id)

	ctx := context.Background()
	h.w.Scan(ctx)
	r := h.run(id)
	if r.Status != "interrupted" || r.LockedUntil != nil || r.FinishedAt == nil {
		t.Fatalf("run = %+v", r)
	}
	h.w.Wait()
	// The interrupted run does not satisfy the slot: exactly one catch-up run exists now.
	var n int
	_ = h.pool.QueryRow(ctx, `SELECT count(*) FROM backup_runs WHERE trigger = 'catchup'`).Scan(&n)
	if n != 1 {
		t.Fatalf("catch-up runs = %d, want 1", n)
	}
	h.scan() // and a further scan makes no second one
	_ = h.pool.QueryRow(ctx, `SELECT count(*) FROM backup_runs WHERE trigger IN ('catchup','schedule') AND status <> 'interrupted'`).Scan(&n)
	if n != 1 {
		t.Fatalf("runs after a second scan = %d, want 1", n)
	}
}

// A destination with a missed slot gets ONE catch-up run, not one per missed slot; a
// destination made after the slot passed gets none; a slot passed a moment ago is a
// `schedule` run.
func TestSchedule_CatchupAndFloor(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	old := h.addDest("old")
	h.exec(`UPDATE backup_destinations SET created_at = now() - interval '5 days', enabled_since = now() - interval '5 days',
		at_utc = to_char((now() at time zone 'utc') - interval '3 hours', 'HH24:MI') WHERE id = $1`, old)
	fresh := h.addDest("fresh") // created now: no run for a slot that passed before
	h.exec(`UPDATE backup_destinations SET at_utc = to_char((now() at time zone 'utc') - interval '3 hours', 'HH24:MI') WHERE id = $1`, fresh)
	recent := h.addDest("recent")
	h.exec(`UPDATE backup_destinations SET created_at = now() - interval '5 days', enabled_since = now() - interval '5 days',
		at_utc = to_char((now() at time zone 'utc') - interval '1 minute', 'HH24:MI') WHERE id = $1`, recent)

	h.w.schedule(ctx)
	h.w.schedule(ctx) // idempotent
	count := func(dest uuid.UUID) (int, string) {
		var n int
		var tr string
		_ = h.pool.QueryRow(ctx, `SELECT count(*), coalesce(max(trigger), '') FROM backup_runs WHERE destination_id = $1`, dest).Scan(&n, &tr)
		return n, tr
	}
	if n, tr := count(old); n != 1 || tr != "catchup" {
		t.Fatalf("old: %d %s", n, tr)
	}
	if n, _ := count(fresh); n != 0 {
		t.Fatalf("a new destination got %d runs for a slot before it existed", n)
	}
	if n, tr := count(recent); n != 1 || tr != "schedule" {
		t.Fatalf("recent: %d %s", n, tr)
	}
	// A disabled destination gets none.
	h.exec(`UPDATE backup_destinations SET enabled = false WHERE id = $1`, old)
	h.exec(`DELETE FROM backup_runs WHERE destination_id = $1`, old)
	h.w.schedule(ctx)
	if n, _ := count(old); n != 0 {
		t.Fatalf("disabled destination got %d runs", n)
	}
}

// The retry rule through the real run path: a retryable code makes will_retry and ONE
// queued retry row 30 minutes ahead; every other code, a retry run, a probe and a
// test make none.
func TestRetry_Table(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	h.addDump(10*time.Minute, nil)
	cases := []struct {
		err   error
		retry bool
	}{
		{driver.Errf(driver.Timeout), true},
		{driver.Errf(driver.ConnectRefused), true},
		{driver.Errf(driver.DNSFailed), true},
		{driver.Errf(driver.Stalled), true},
		{driver.ServerErr(503), true},
		{driver.ServerErr(302), false},
		{driver.ServerErr(404), false},
		{driver.Errf(driver.NotFound), false},
		{driver.Errf(driver.AuthFailed), false},
		{driver.Errf(driver.Forbidden), false},
		{driver.Errf(driver.QuotaExceeded), false},
		{driver.Errf(driver.TLSPinMismatch), false},
		{driver.Errf(driver.TLSUntrusted), false},
		{driver.Errf(driver.HostKeyChanged), false},
		{driver.Errf(driver.SSRFBlocked), false},
		{driver.Errf(driver.TooLarge), false},
		{driver.Errf(driver.ClockSkew), false},
	}
	for _, c := range cases {
		h.exec(`DELETE FROM backup_runs`)
		h.fake.put = func(ctx context.Context, name string, size int64, r io.Reader) error { return c.err }
		id := h.queue(dest, "manual")
		h.scan()
		r := h.run(id)
		if r.Status != "failed" || r.ErrorCode != string(driver.CodeOf(c.err)) || r.WillRetry != c.retry {
			t.Fatalf("%v: run = %+v", c.err, r)
		}
		var n int
		_ = h.pool.QueryRow(context.Background(), `SELECT count(*) FROM backup_runs WHERE trigger = 'retry' AND status = 'queued'`).Scan(&n)
		if (n == 1) != c.retry || n > 1 {
			t.Fatalf("%v: queued retries = %d", c.err, n)
		}
		if c.retry {
			var at, rat time.Time
			var att int
			var wr bool
			if err := h.pool.QueryRow(context.Background(), `SELECT retry_at, attempt, will_retry FROM backup_runs WHERE trigger = 'retry'`).Scan(&rat, &att, &wr); err != nil {
				t.Fatal(err)
			}
			at = *r.RetryAt
			if !rat.Equal(at) || att != 2 || wr {
				t.Fatalf("retry row = %v %d %v, parent retry_at %v", rat, att, wr, at)
			}
			if d := time.Until(rat); d < 29*time.Minute || d > 31*time.Minute {
				t.Fatalf("retry_at is %v ahead", d)
			}
			// It does not run before retry_at...
			h.scan()
			var st string
			_ = h.pool.QueryRow(context.Background(), `SELECT status FROM backup_runs WHERE trigger = 'retry'`).Scan(&st)
			if st != "queued" {
				t.Fatalf("a retry ran before its time: %s", st)
			}
			// ...and a retry that fails again is not retried a second time.
			h.exec(`UPDATE backup_runs SET retry_at = now() - interval '1 second' WHERE trigger = 'retry'`)
			h.scan()
			var failed bool
			var wr2 bool
			_ = h.pool.QueryRow(context.Background(), `SELECT status = 'failed', will_retry FROM backup_runs WHERE trigger = 'retry'`).Scan(&failed, &wr2)
			_ = h.pool.QueryRow(context.Background(), `SELECT count(*) FROM backup_runs WHERE status = 'queued'`).Scan(&n)
			if !failed || wr2 || n != 0 {
				t.Fatalf("the retry's failure: failed=%v will_retry=%v queued=%d", failed, wr2, n)
			}
		}
	}
	// A failed test run is never retried.
	h.exec(`DELETE FROM backup_runs`)
	h.fake.testSteps = func() []driver.Step {
		return []driver.Step{{Name: "connect", OK: true}, {Name: "auth", Code: driver.Timeout}}
	}
	tid := h.queue(dest, "test")
	h.scan()
	if r := h.run(tid); r.Status != "failed" || r.WillRetry || r.ErrorCode != "timeout" {
		t.Fatalf("test run = %+v", r)
	}
}
