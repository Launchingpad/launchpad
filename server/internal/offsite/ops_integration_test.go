//go:build backuptest

package offsite

import (
	"context"
	"encoding/json"
	"errors"
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

// queueUnsaved inserts a queued probe or test run for a form that is not saved,
// with its secrets sealed into the run row (additional data: the run id).
func (h *harness) queueUnsaved(trigger string, cfg map[string]any, confirmed string, secrets map[string]string) uuid.UUID {
	h.t.Helper()
	id := uuid.Must(uuid.NewV7())
	tc := map[string]any{"type": "s3", "config": cfg}
	if confirmed != "" {
		tc["confirmedFingerprint"] = confirmed
	}
	tcb, _ := json.Marshal(tc)
	var blob []byte
	if secrets != nil {
		pt, _ := json.Marshal(secrets)
		var err error
		if blob, err = h.w.keys.Seal(platform.BackupColRunTestSecrets, id.String(), pt); err != nil {
			h.t.Fatal(err)
		}
	}
	h.exec(`INSERT INTO backup_runs (id, trigger, status, test_config, test_secrets_enc) VALUES ($1, $2, 'queued', $3, $4)`,
		id, trigger, tcb, blob)
	return id
}

func stepsOf(t *testing.T, r dbq.BackupRun) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := json.Unmarshal(r.Steps, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// An unsaved test: the sealed secrets reach the driver, the blob is empty after a
// success and after a failure, and nothing planted reaches a log or a row.
func TestTest_UnsavedSecretsAreClearedAndNeverLeak(t *testing.T) {
	h := newHarness(t)
	cfg := map[string]any{"dir": h.remoteDir}
	const planted = "PLANTED-UNSAVED-PASSWORD-9"

	ok := h.queueUnsaved("test", cfg, "", map[string]string{"password": planted})
	h.scan()
	r := h.run(ok)
	if r.Status != "succeeded" || r.TestSecretsEnc != nil || r.DestinationID != nil || r.DestinationName != "" {
		t.Fatalf("success: %+v", r)
	}
	var names []string
	for _, s := range stepsOf(t, r) {
		if s["ok"] != true {
			t.Fatalf("step %v", s)
		}
		names = append(names, s["name"].(string))
	}
	if strings.Join(names, ",") != "connect,auth,write,read,delete" {
		t.Fatalf("steps = %v", names)
	}
	if cs := h.fake.openedConfigs(); len(cs) != 1 || !strings.Contains(string(cs[0].Secrets), planted) {
		t.Fatal("the driver did not get the unsaved secrets")
	}

	// A failure: a driver whose Open returns an error that echoes the secret.
	h.fake.openErr = errors.New("server said: password " + planted + " is wrong")
	bad := h.queueUnsaved("test", cfg, "", map[string]string{"password": planted})
	h.scan()
	r = h.run(bad)
	if r.Status != "failed" || r.TestSecretsEnc != nil || r.ErrorCode != "server_error" || strings.Contains(r.ErrorText, planted) {
		t.Fatalf("failure: %+v", r)
	}
	h.assertNoText(planted)
	if left := h.remoteFiles(); len(left) != 0 {
		t.Fatalf("test files left on the remote: %v", left)
	}
}

// Boot clears the secrets of every run that is not a young queued one; the scan
// sweeps a queued run older than 10 minutes.
func TestBoot_ClearsTestSecretsAndScratch(t *testing.T) {
	h := newHarness(t)
	blob := func(id uuid.UUID) []byte {
		b, err := h.w.keys.Seal(platform.BackupColRunTestSecrets, id.String(), []byte(`{"password":"x"}`))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	mk := func(status, extra string) uuid.UUID {
		id := uuid.Must(uuid.NewV7())
		h.exec(`INSERT INTO backup_runs (id, trigger, status, test_config, test_secrets_enc) VALUES ($1, 'test', 'queued', '{"type":"s3","config":{}}', $2)`, id, blob(id))
		if status != "queued" {
			h.exec(`UPDATE backup_runs SET status = $2, finished_at = now() WHERE id = $1`, id, status)
		}
		if extra != "" {
			h.exec(`UPDATE backup_runs SET `+extra+` WHERE id = $1`, id)
		}
		return id
	}
	failed := mk("failed", "")
	running := mk("queued", "status = 'running', started_at = now(), locked_until = now() + interval '1 hour'")
	young := mk("queued", "")
	old := mk("queued", "queued_at = now() - interval '11 minutes'")

	// A scratch file of a dead run and of a live one, and the instance file.
	dead, live := uuid.Must(uuid.NewV7()), running
	for _, id := range []uuid.UUID{dead, live} {
		if err := os.WriteFile(h.w.scratchPath(id), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.WriteFile(filepath.Join(h.workDir, InstanceFile), []byte("keep"), 0o600)

	if err := h.w.Boot(context.Background()); err != nil {
		t.Fatal(err)
	}
	has := func(id uuid.UUID) bool { return h.run(id).TestSecretsEnc != nil }
	if has(failed) || has(running) || has(old) {
		t.Fatal("boot left a stale test secret")
	}
	if !has(young) {
		t.Fatal("boot cleared the secrets of a young queued test")
	}
	if _, err := os.Stat(h.w.scratchPath(dead)); err == nil {
		t.Fatal("boot left the scratch file of a dead run")
	}
	if _, err := os.Stat(h.w.scratchPath(live)); err != nil {
		t.Fatal("boot removed the scratch file of a live run")
	}
	if _, err := os.Stat(filepath.Join(h.workDir, InstanceFile)); err != nil {
		t.Fatal("boot touched the instance file")
	}
}

// Probe (phase 1): the fingerprint and whether the chain verified go into steps; a
// probe sends no credentials (the driver gets none).
func TestProbe_Steps(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	h.fake.probe = driver.ProbeResult{Kind: "tls", Fingerprint: "abc123", ChainVerified: false}
	saved := h.queue(dest, "probe")
	unsaved := h.queueUnsaved("probe", map[string]any{"dir": h.remoteDir}, "", nil)
	h.scan()
	for _, id := range []uuid.UUID{saved, unsaved} {
		r := h.run(id)
		st := stepsOf(t, r)
		if r.Status != "succeeded" || len(st) != 1 || st[0]["fingerprint"] != "abc123" || st[0]["kind"] != "tls" || st[0]["chainVerified"] != false {
			t.Fatalf("probe = %+v steps %v", r, st)
		}
	}
	h.fake.probeErr = driver.Errf(driver.DNSFailed)
	bad := h.queue(dest, "probe")
	h.scan()
	if r := h.run(bad); r.Status != "failed" || r.ErrorCode != "dns_failed" || r.WillRetry {
		t.Fatalf("failed probe = %+v", r)
	}
	for _, c := range h.fake.openedConfigs() {
		if len(c.Secrets) > 0 {
			t.Fatal("a probe was given secrets")
		}
	}
}

// Phase 2 starts only when trust exists: a verified chain, a stored pin, or the
// admin's confirmed fingerprint equal to the probed one. A confirmed fingerprint never
// overrides a pin the config has.
func TestTest_TrustRules(t *testing.T) {
	h := newHarness(t)
	h.fake.probe = driver.ProbeResult{Kind: "tls", Fingerprint: "fp-probed", ChainVerified: false}
	cfg := func(extra map[string]any) map[string]any {
		m := map[string]any{"dir": h.remoteDir}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	last := func() map[string]any {
		cs := h.fake.openedConfigs()
		var m map[string]any
		_ = json.Unmarshal(cs[len(cs)-1].Config, &m)
		return m
	}

	// Untrusted and not confirmed: tls_untrusted, no connection opened.
	id := h.queueUnsaved("test", cfg(nil), "", nil)
	h.scan()
	r := h.run(id)
	if r.Status != "failed" || r.ErrorCode != "tls_untrusted" || len(h.fake.openedConfigs()) != 0 {
		t.Fatalf("untrusted: %+v", r)
	}
	// A confirmed fingerprint that is not the probed one.
	id = h.queueUnsaved("test", cfg(nil), "fp-other", nil)
	h.scan()
	if r := h.run(id); r.Status != "failed" || r.ErrorCode != "tls_untrusted" {
		t.Fatalf("wrong confirmation: %+v", r)
	}
	// The probed fingerprint, confirmed: it becomes the pin the driver is given.
	id = h.queueUnsaved("test", cfg(nil), "fp-probed", nil)
	h.scan()
	if r := h.run(id); r.Status != "succeeded" {
		t.Fatalf("confirmed: %+v", r)
	}
	if last()["tlsPin"] != "fp-probed" {
		t.Fatalf("the driver was not given the confirmed pin: %v", last())
	}
	// A pin already in the config wins over a confirmed fingerprint.
	id = h.queueUnsaved("test", cfg(map[string]any{"tlsPin": "fp-saved"}), "fp-probed", nil)
	h.scan()
	if r := h.run(id); r.Status != "succeeded" || last()["tlsPin"] != "fp-saved" {
		t.Fatalf("saved pin overridden: %+v %v", r, last())
	}
	// A verified chain needs no confirmation and no pin.
	h.fake.probe = driver.ProbeResult{Kind: "tls", Fingerprint: "x", ChainVerified: true}
	id = h.queueUnsaved("test", cfg(nil), "", nil)
	h.scan()
	if r := h.run(id); r.Status != "succeeded" {
		t.Fatalf("verified chain: %+v", r)
	}
	// SSH: a host key is never trusted without a pin or a confirmation.
	h.fake.probe = driver.ProbeResult{Kind: "ssh", Fingerprint: "ssh-fp"}
	id = h.queueUnsaved("test", cfg(nil), "", nil)
	h.scan()
	if r := h.run(id); r.Status != "failed" || r.ErrorCode != "host_key_changed" {
		t.Fatalf("ssh: %+v", r)
	}
}

// A test is claimed and finishes while a backup run of the same destination holds the lock.
func TestTest_RunsWhileABackupHoldsTheLock(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
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
	backupID := h.queue(dest, "manual")
	ctx := context.Background()
	h.w.Scan(ctx)
	<-started
	if h.lockFree(dest) {
		t.Fatal("the backup run does not hold the lock")
	}
	h.fake.testSteps = nil
	testID := h.queue(dest, "test")
	h.w.Scan(ctx)
	if r := h.waitStatus(testID, 5*time.Second, "succeeded", "failed"); r.Status != "succeeded" {
		t.Fatalf("test = %+v", r)
	}
	if r := h.run(backupID); r.Status != "running" {
		t.Fatalf("backup = %+v", r)
	}
	close(release)
	h.waitStatus(backupID, 5*time.Second, "succeeded")
	h.w.Wait()
}

// The exchange: the code is used once, the refresh token is merged into the sealed
// secrets (other secrets stay), the blob of the run is cleared, key_check is untouched.
func TestExchange_MergesTheRefreshToken(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	h.exec(`UPDATE backup_destinations SET enabled = false WHERE id = $1`, dest)
	pt, _ := json.Marshal(map[string]string{"appKey": "AK", "appSecret": "AS", "extra": "keep"})
	blob, _ := h.w.keys.Seal(platform.BackupColDestinationSecrets, dest.String(), pt)
	h.exec(`UPDATE backup_destinations SET secrets_enc = $2, secrets_set = '{appKey,appSecret,extra}' WHERE id = $1`, dest, blob)

	const code, token = "PLANTED-AUTH-CODE", "PLANTED-REFRESH-TOKEN"
	var gotCode string
	var gotSecrets string
	h.fake.exchange = func(c driver.Config, cd string) (string, error) {
		gotCode, gotSecrets = cd, string(c.Secrets)
		return token, nil
	}
	mk := func() uuid.UUID {
		id := uuid.Must(uuid.NewV7())
		bl, _ := h.w.keys.Seal(platform.BackupColRunTestSecrets, id.String(), mustJSON(map[string]string{"code": code, "appKey": "AK", "appSecret": "AS"}))
		h.exec(`INSERT INTO backup_runs (id, destination_id, destination_name, trigger, status, test_config, test_secrets_enc)
			VALUES ($1, $2, 'main', 'exchange', 'queued', '{"type":"dropbox"}', $3)`, id, dest, bl)
		return id
	}
	id := mk()
	h.scan()
	r := h.run(id)
	if r.Status != "succeeded" || r.TestSecretsEnc != nil {
		t.Fatalf("exchange = %+v\n%s", r, h.log.String())
	}
	if gotCode != code || !strings.Contains(gotSecrets, `"appKey":"AK"`) || strings.Contains(gotSecrets, code) {
		t.Fatalf("the driver got code %q secrets %s", gotCode, gotSecrets)
	}
	d, _ := dbq.New(h.pool).GetBackupDestination(context.Background(), dest)
	open, err := h.w.keys.Open(platform.BackupColDestinationSecrets, dest.String(), d.SecretsEnc)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	_ = json.Unmarshal(open, &m)
	if m["refreshToken"] != token || m["appKey"] != "AK" || m["extra"] != "keep" {
		t.Fatalf("merged secrets = %v", m)
	}
	if strings.Join(d.SecretsSet, ",") != "appKey,appSecret,extra,refreshToken" || d.Enabled {
		t.Fatalf("destination = %+v", d)
	}
	if h.settings().KeyCheck != nil {
		t.Fatal("offsite wrote key_check")
	}
	h.assertNoText(code)
	h.assertNoText(token)

	// A failed exchange: no refresh token change, the destination stays disabled, no leak.
	h.exec(`UPDATE backup_destinations SET secrets_enc = $2, secrets_set = '{appKey,appSecret,extra}' WHERE id = $1`, dest, blob)
	h.fake.exchange = func(c driver.Config, cd string) (string, error) { return "", driver.Errf(driver.AuthFailed) }
	id = mk()
	h.scan()
	r = h.run(id)
	if r.Status != "failed" || r.ErrorCode != "auth_failed" || r.TestSecretsEnc != nil || r.WillRetry {
		t.Fatalf("failed exchange = %+v", r)
	}
	d, _ = dbq.New(h.pool).GetBackupDestination(context.Background(), dest)
	if strings.Contains(strings.Join(d.SecretsSet, ","), "refreshToken") || d.Enabled {
		t.Fatalf("destination after a failed exchange: %+v", d)
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// A refresh by offsite and a PATCH by the app, both read-merge-seal under the row
// lock, lose neither write.
func TestMergeSecrets_TwoWritersLoseNothing(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	ctx := context.Background()
	for round := 0; round < 8; round++ {
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		wg.Add(2)
		go func() { // offsite: a token refresh
			defer wg.Done()
			errs <- h.w.MergeSecrets(ctx, dest, func(m map[string]string) {
				time.Sleep(30 * time.Millisecond)
				m["refreshToken"] = "tok" + string(rune('a'+round))
			})
		}()
		go func() { // the app: a PATCH of another secret, as backupapi does it
			defer wg.Done()
			errs <- appPatch(ctx, h, dest, "appKey", "key"+string(rune('a'+round)))
		}()
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		d, _ := dbq.New(h.pool).GetBackupDestination(ctx, dest)
		pt, err := h.w.keys.Open(platform.BackupColDestinationSecrets, dest.String(), d.SecretsEnc)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]string
		_ = json.Unmarshal(pt, &m)
		if m["refreshToken"] != "tok"+string(rune('a'+round)) || m["appKey"] != "key"+string(rune('a'+round)) {
			t.Fatalf("round %d lost a write: %v", round, m)
		}
	}
}

// appPatch is what backupapi's PATCH does with a secret: lock the row, read, merge,
// seal, write (decisions-T9 #15).
func appPatch(ctx context.Context, h *harness, dest uuid.UUID, name, value string) error {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := dbq.New(tx)
	d, err := q.GetBackupDestinationForUpdate(ctx, dest)
	if err != nil {
		return err
	}
	m := map[string]string{}
	if d.SecretsEnc != nil {
		pt, err := h.w.keys.Open(platform.BackupColDestinationSecrets, dest.String(), d.SecretsEnc)
		if err != nil {
			return err
		}
		_ = json.Unmarshal(pt, &m)
	}
	time.Sleep(30 * time.Millisecond)
	m[name] = value
	blob, err := h.w.keys.Seal(platform.BackupColDestinationSecrets, dest.String(), mustJSON(m))
	if err != nil {
		return err
	}
	if _, err := q.SetBackupDestinationSecrets(ctx, dbq.SetBackupDestinationSecretsParams{ID: dest, SecretsEnc: blob, SecretsSet: []string{"appKey", "refreshToken"}}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// A verify run downloads the newest object and checks its hash and size against the
// DATABASE; it fails object_mismatch for a changed object and not_found for a missing one.
func TestVerify_Run(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	h.addDump(10*time.Minute, nil)
	h.queue(dest, "manual")
	h.scan()
	v := h.queue(dest, "verify")
	h.scan()
	if r := h.run(v); r.Status != "succeeded" || r.Sha256 == nil || r.Bytes == nil || r.RemoteName == nil {
		t.Fatalf("verify = %+v\n%s", r, h.log.String())
	}
	files := h.remoteFiles()
	var obj string
	for _, f := range files {
		if strings.HasSuffix(f, ".dump.age") {
			obj = filepath.Join(h.remoteDir, f)
		}
	}
	b, _ := os.ReadFile(obj)
	b[len(b)-3] ^= 0xff // the remote changed the object
	_ = os.WriteFile(obj, b, 0o600)
	v = h.queue(dest, "verify")
	h.scan()
	if r := h.run(v); r.Status != "failed" || r.ErrorCode != "object_mismatch" || r.WillRetry {
		t.Fatalf("changed object: %+v", r)
	}
	_ = os.Remove(obj)
	v = h.queue(dest, "verify")
	h.scan()
	if r := h.run(v); r.Status != "failed" || r.ErrorCode != "not_found" {
		t.Fatalf("missing object: %+v", r)
	}
}

// The verify schedule: due when the interval has passed since the last verify (or the
// first success), never doubled while one is open, off at 0.
func TestVerify_Schedule(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	ctx := context.Background()
	h.addDump(10*time.Minute, nil)
	h.queue(dest, "manual")
	h.scan()
	due := func() int {
		ids, err := dbq.New(h.pool).ListBackupVerifyDue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(ids)
	}
	if due() != 0 {
		t.Fatal("a verify is due right after the first success")
	}
	h.exec(`UPDATE backup_runs SET finished_at = now() - interval '8 days', queued_at = now() - interval '8 days',
		started_at = now() - interval '8 days'`)
	if due() != 1 {
		t.Fatal("no verify is due 8 days after the first success")
	}
	h.w.scheduleVerifies(ctx)
	h.w.scheduleVerifies(ctx)
	var n int
	_ = h.pool.QueryRow(ctx, `SELECT count(*) FROM backup_runs WHERE trigger = 'verify'`).Scan(&n)
	if n != 1 {
		t.Fatalf("verify runs = %d, want 1", n)
	}
	if due() != 0 {
		t.Fatal("a verify is due while one is open")
	}
	h.scan()
	if due() != 0 {
		t.Fatal("a verify is due right after one ran")
	}
	h.exec(`UPDATE backup_destinations SET verify_every_days = 0`)
	h.exec(`UPDATE backup_runs SET queued_at = now() - interval '30 days' WHERE trigger = 'verify'`)
	if due() != 0 {
		t.Fatal("a verify is due with verify_every_days = 0")
	}
}
