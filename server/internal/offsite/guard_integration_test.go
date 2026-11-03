//go:build backuptest

package offsite

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/backup/driver"
	"devops/internal/dbq"
	"devops/internal/platform"
)

func (h *harness) queueOK(dest uuid.UUID) dbq.BackupRun {
	h.t.Helper()
	h.addDump(10*time.Minute, nil)
	id := h.queue(dest, "manual")
	h.scan()
	return h.run(id)
}

// First use makes the key; two workers racing make exactly one.
func TestSigning_FirstUseOnce(t *testing.T) {
	h := newHarness(t)
	if h.settings().SigningPublic != "" {
		t.Fatal("the key exists before first use")
	}
	other := h.newWorker(h.pool, h.dsn, nil)
	var wg sync.WaitGroup
	for _, w := range []*Worker{h.w, other, h.w, other} {
		wg.Add(1)
		go func(w *Worker) { defer wg.Done(); _ = w.ensureSigningKey(context.Background()) }(w)
	}
	wg.Wait()
	s := h.settings()
	if s.SigningPublic == "" || s.SigningPrivateEnc == nil {
		t.Fatal("no key made")
	}
	for _, w := range []*Worker{h.w, other} {
		_, pub, err := w.loadSigning(context.Background())
		if err != nil || pub != s.SigningPublic {
			t.Fatalf("loadSigning: %v", err)
		}
	}
	var n int
	_ = h.pool.QueryRow(context.Background(), `SELECT count(*) FROM backup_change_log WHERE kind = 'signing_key_replaced'`).Scan(&n)
	if n != 0 {
		t.Fatal("first use wrote a replace row")
	}
	if h.settings().KeyCheck != nil {
		t.Fatal("offsite wrote key_check")
	}
}

// A blanked, half-empty or unreadable key never produces a new key: the run fails
// secrets_unreadable and the row is not touched.
func TestSigning_NeverReplacedSilently(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	if r := h.queueOK(dest); r.Status != "succeeded" {
		t.Fatalf("first run: %+v", r)
	}
	good := h.settings()

	// Another BACKUP_ENCRYPTION_KEY cannot open the key.
	wrong := h.newWorker(h.pool, h.dsn, nil)
	var err error
	wrong.keys, err = platform.NewBackupKeyring(bytes32(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := wrong.loadSigning(context.Background()); driver.CodeOf(err) != driver.SecretsUnreadable {
		t.Fatalf("wrong key: %v", err)
	}
	if err := wrong.ensureSigningKey(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := h.settings(); s.SigningPublic != good.SigningPublic || string(s.SigningPrivateEnc) != string(good.SigningPrivateEnc) {
		t.Fatal("the signing key changed")
	}

	// A half-empty pair (the guard disabled, as a bug or a manual edit would do).
	h.exec(`ALTER TABLE backup_settings DISABLE TRIGGER backup_settings_signing_guard`)
	h.exec(`UPDATE backup_settings SET signing_public = ''`)
	h.addDump(5*time.Minute, nil)
	id := h.queue(dest, "manual")
	h.scan()
	r := h.run(id)
	if r.Status != "failed" || r.ErrorCode != "secrets_unreadable" || r.WillRetry {
		t.Fatalf("half empty: %+v", r)
	}
	if s := h.settings(); s.SigningPublic != "" || s.SigningPrivateEnc == nil {
		t.Fatal("a half-empty key was repaired by the worker")
	}
	// A private half that does not match the public one.
	h.exec(`UPDATE backup_settings SET signing_public = $1`, "AAAA"+good.SigningPublic[4:])
	id = h.queue(dest, "manual")
	h.scan()
	if r := h.run(id); r.Status != "failed" || r.ErrorCode != "secrets_unreadable" {
		t.Fatalf("mismatch: %+v", r)
	}
}

// rotate-signing: a change-log row and the new key in one transaction; later runs
// sign with the new key. A bare UPDATE without the row is refused by the trigger.
func TestSigning_Rotate(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	if r := h.queueOK(dest); r.Status != "succeeded" {
		t.Fatalf("run: %+v", r)
	}
	old := h.settings().SigningPublic
	pub, err := rotateSigning(context.Background(), h.pool, h.w.keys)
	if err != nil {
		t.Fatal(err)
	}
	if pub == old || h.settings().SigningPublic != pub {
		t.Fatal("the key was not replaced")
	}
	var n int
	_ = h.pool.QueryRow(context.Background(), `SELECT count(*) FROM backup_change_log WHERE kind = 'signing_key_replaced'`).Scan(&n)
	if n != 1 {
		t.Fatalf("change log rows = %d", n)
	}
	if r := h.queueOK(dest); r.Status != "succeeded" {
		t.Fatalf("run after rotate: %+v", r)
	}
	if _, err := h.pool.Exec(context.Background(), `UPDATE backup_settings SET signing_public = 'x'`); err == nil {
		t.Fatal("the trigger allowed a bare replace")
	}
}

// The instance guard (#37).
func TestInstanceGuard(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	file := filepath.Join(h.workDir, InstanceFile)
	ctx := context.Background()

	// First deploy: no file, no succeeded run -> the file is written.
	if !h.w.checkInstance(ctx) {
		t.Fatal("first deploy refused")
	}
	b, _ := os.ReadFile(file)
	if strings.TrimSpace(string(b)) != h.settings().InstanceID.String() {
		t.Fatal("wrong instance file")
	}
	if r := h.queueOK(dest); r.Status != "succeeded" {
		t.Fatalf("run: %+v", r)
	}

	// A restored database: the file is another id. Queued runs fail, instance_changed, no retry.
	_ = os.WriteFile(file, []byte(uuid.NewString()+"\n"), 0o600)
	h.addDump(1*time.Minute, nil)
	id := h.queue(dest, "manual")
	h.scan()
	r := h.run(id)
	if r.Status != "failed" || r.ErrorCode != "instance_changed" || r.WillRetry {
		t.Fatalf("mismatch: %+v", r)
	}
	// A test run still works.
	tid := h.queue(dest, "test")
	h.scan()
	if r := h.run(tid); r.Status != "succeeded" {
		t.Fatalf("test run under a mismatch: %+v", r)
	}
	// The file is reread every scan: resume (the CLI) clears it with no restart.
	if code, err := cmdResume([]string{"--instance-id", h.settings().InstanceID.String()},
		func(k string) string {
			if k == "OFFSITE_WORK_DIR" {
				return h.workDir
			}
			return ""
		}); code != 0 || err != nil {
		t.Fatalf("resume: %d %v", code, err)
	}
	h.addDump(1*time.Minute, nil)
	id = h.queue(dest, "manual")
	h.scan()
	if r := h.run(id); r.Status != "succeeded" {
		t.Fatalf("after resume: %+v", r)
	}

	// A missing file with a succeeded run in the database (a fresh volume + restored db) is a mismatch.
	_ = os.Remove(file)
	if h.w.checkInstance(ctx) {
		t.Fatal("a missing file with history was accepted")
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("the worker wrote the file over history")
	}
}

// Cancel rules (#31): a disabled destination cancels its queued backup runs, a deleted one
// every open run; a running run is aborted and its partial object deleted; an unsaved test is
// never canceled.
func TestCancelRules(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	h.addDump(10*time.Minute, nil)
	started := make(chan struct{})
	var once sync.Once
	h.fake.before = func(ctx context.Context, name string) error {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return ctx.Err()
	}
	run := h.queue(dest, "manual")
	unsaved := h.queueUnsaved("test", map[string]any{"dir": h.remoteDir}, "", nil)
	ctx := context.Background()
	h.w.Scan(ctx)
	<-started
	h.exec(`UPDATE backup_destinations SET enabled = false WHERE id = $1`, dest)
	h.w.Scan(ctx)
	r := h.waitStatus(run, 10*time.Second, "canceled", "failed", "interrupted")
	if r.Status != "canceled" {
		t.Fatalf("running run after disable: %+v", r)
	}
	h.w.Wait()
	if len(h.remoteFiles()) != 0 || len(h.scratchFiles()) != 0 {
		t.Fatalf("leftovers: %v %v", h.remoteFiles(), h.scratchFiles())
	}
	if u := h.run(unsaved); u.Status == "canceled" {
		t.Fatal("an unsaved test was canceled")
	}

	// A queued run, destination deleted.
	h.exec(`UPDATE backup_destinations SET enabled = true WHERE id = $1`, dest)
	q := h.queue(dest, "manual")
	h.exec(`DELETE FROM backup_destinations WHERE id = $1`, dest)
	h.w.Scan(ctx)
	if r := h.run(q); r.Status != "canceled" {
		t.Fatalf("deleted destination: %+v", r)
	}
}

// The scratch file of a killed run goes at the next scan once its lease has run out,
// not only at a restart; the file of a live running run stays.
func TestScan_RemovesScratchOfAKilledRun(t *testing.T) {
	h := newHarness(t)
	mk := func(name, lease string) uuid.UUID {
		dest := h.addDest(name)
		id := uuid.Must(uuid.NewV7())
		h.exec(`INSERT INTO backup_runs (id, destination_id, destination_name, trigger, status, started_at, locked_until)
			VALUES ($1, $2, $4, 'manual', 'running', now(), now() + $3::interval)`, id, dest, lease, name)
		if err := os.WriteFile(h.w.scratchPath(id), []byte("dump sized"), 0o600); err != nil {
			t.Fatal(err)
		}
		return id
	}
	dead := mk("d1", "-1 minute")
	live := mk("d2", "1 hour")
	// Boot sees the dead run still `running`: its file stays (the run may be live elsewhere).
	if err := h.w.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.w.scratchPath(dead)); err != nil {
		t.Fatal("boot removed the file of a run that was still running")
	}
	h.w.Scan(context.Background())
	if r := h.run(dead); r.Status != "interrupted" {
		t.Fatalf("dead run = %+v", r)
	}
	if _, err := os.Stat(h.w.scratchPath(dead)); err == nil {
		t.Fatal("the scan left the scratch file of the killed run")
	}
	if _, err := os.Stat(h.w.scratchPath(live)); err != nil {
		t.Fatal("the scan removed the file of a live run")
	}
}

// With every backup slot busy, a queued backup does not hold back a test run behind it.
func TestDispatch_FullSlotsDoNotBlockOtherRuns(t *testing.T) {
	h := newHarness(t)
	h.w = h.newWorker(h.pool, h.dsn, func(o *Options) { o.MaxBackups = 1 })
	a, b := h.addDest("a"), h.addDest("b")
	h.addDump(10*time.Minute, nil)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.fake.before = func(ctx context.Context, name string) error {
		once.Do(func() { close(started) })
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	first := h.queue(a, "manual")
	ctx := context.Background()
	h.w.Scan(ctx)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the first backup did not start")
	}
	second := h.queue(b, "manual") // no free slot
	probe := h.queue(b, "probe")   // queued AFTER it
	h.w.Scan(ctx)
	if r := h.waitStatus(probe, 5*time.Second, "succeeded", "failed"); r.Status != "succeeded" {
		t.Fatalf("probe = %+v", r)
	}
	if r := h.run(second); r.Status != "queued" {
		t.Fatalf("second backup = %+v", r)
	}
	close(release)
	h.waitStatus(first, 10*time.Second, "succeeded")
	h.w.Wait()
	h.w.Scan(ctx)
	h.w.Wait()
	if r := h.run(second); r.Status != "succeeded" {
		t.Fatalf("second backup after the slot freed = %+v", r)
	}
}
