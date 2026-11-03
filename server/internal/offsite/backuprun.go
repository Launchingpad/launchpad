package offsite

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"devops/internal/backup"
	"devops/internal/backup/driver"
	"devops/internal/backup/remotename"
	"devops/internal/dbq"
	"devops/internal/platform"
)

// execBackup owns a claimed backup run from its claim to its end. lock is the
// destination lock of a run claimed as `running`; a `waiting_dump` run was claimed
// with the lock and gives it back, then takes it again before it runs (#31
// "Advisory lock and states").
func (w *Worker) execBackup(parent context.Context, run dbq.BackupRun, lock *destLock) {
	sc := w.begin(parent, run.ID)
	holdsSem := true
	defer func() {
		sc.stop()
		lock.release()
		if holdsSem {
			release(w.backupSem)
		}
	}()
	if run.Status == "waiting_dump" {
		lock.release()
		lock = nil
		release(w.backupSem)
		holdsSem = false
		var dump dbq.BackupDump
		var err error
		dump, run, err = w.waitForDump(sc.ctx, run)
		if err != nil {
			w.finish(parent, sc, run, result{}, err)
			return
		}
		select { // the run phase needs a slot and the lock
		case w.backupSem <- struct{}{}:
			holdsSem = true
		case <-sc.ctx.Done():
			return
		}
		lock, err = w.lockUntil(sc.ctx, *run.DestinationID)
		if err != nil {
			return // cancelled while waiting for the lock
		}
		run, err = dbq.New(w.pool).MarkBackupRunRunning(sc.ctx, dbq.MarkBackupRunRunningParams{ID: run.ID, DumpID: &dump.ID})
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				w.log.Error("offsite: cannot move a run to running", "err", err)
			}
			return
		}
	}
	timeout := w.o.RunTimeout
	if w.silo != nil {
		timeout += w.o.FilesTimeout
	}
	runCtx, cancel := context.WithTimeoutCause(sc.ctx, timeout, errRunTimeout)
	defer cancel()
	res, err := w.doBackup(runCtx, run)
	w.finish(parent, sc, run, res, err)
}

// lockUntil waits for the destination lock (the previous holder is finishing).
func (w *Worker) lockUntil(ctx context.Context, dest uuidT) (*destLock, error) {
	for {
		l, ok, err := w.tryLock(ctx, dest)
		if err == nil && ok {
			return l, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(w.o.WaitPoll):
		}
	}
}

// waitForDump polls a waiting_dump run until its dump is ready, or fails it
// dump_unavailable (#23, #28). The lease is renewed by the run's heartbeat.
func (w *Worker) waitForDump(ctx context.Context, run dbq.BackupRun) (dbq.BackupDump, dbq.BackupRun, error) {
	for {
		dec, err := w.evaluate(ctx, run)
		switch {
		case err != nil && ctx.Err() == nil:
			w.log.Error("offsite: cannot look at the dumps", "err", err)
		case err != nil:
			return dbq.BackupDump{}, run, ctx.Err()
		case dec.dump != nil:
			return *dec.dump, run, nil
		case dec.fail != nil:
			return dbq.BackupDump{}, run, dec.fail
		}
		select {
		case <-ctx.Done():
			return dbq.BackupDump{}, run, context.Cause(ctx)
		case <-time.After(w.o.WaitPoll):
		}
	}
}

// progress counts the bytes that pass and the time of the last one: the stall
// watchdog reads it (#40: no progress for StallAfter is `stalled`).
type progress struct {
	r    io.Reader
	last atomic.Int64 // unix nanoseconds
}

func newProgress(r io.Reader) *progress {
	p := &progress{r: r}
	p.last.Store(time.Now().UnixNano())
	return p
}

func (p *progress) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.last.Store(time.Now().UnixNano())
	}
	return n, err
}

// watchStall cancels the run (cause errStalled) when p shows no progress for d.
func watchStall(ctx context.Context, cancel context.CancelCauseFunc, p *progress, d time.Duration) (stop func()) {
	done := make(chan struct{})
	tick := d / 4
	if tick < 10*time.Millisecond {
		tick = 10 * time.Millisecond
	}
	go func() {
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if time.Since(time.Unix(0, p.last.Load())) > d {
					cancel(errStalled)
					return
				}
			}
		}
	}()
	return func() { close(done) }
}

// readTracker remembers a read error of the source so a read failure of the dump
// is not mistaken for a write failure of the scratch file.
type readTracker struct {
	r   io.Reader
	err error
}

func (t *readTracker) Read(b []byte) (int, error) {
	n, err := t.r.Read(b)
	if err != nil && err != io.EOF {
		t.err = err
	}
	return n, err
}

// doBackup is the pipeline of #31 steps 2 to 4: pick the dump, check the space,
// encrypt into scratch (hashing in the same pass), upload the object and then the
// signed manifest, list and compare, prune. On a failure after the upload began it
// deletes its own partial remote objects, best effort (#18).
func (w *Worker) doBackup(ctx context.Context, run dbq.BackupRun) (res result, err error) {
	q := dbq.New(w.pool)
	if run.DestinationID == nil || run.DumpID == nil {
		return res, internal(errors.New("a backup run without a destination or a dump"))
	}
	dest, err := q.GetBackupDestination(ctx, *run.DestinationID)
	if err != nil {
		return res, internal(err)
	}
	settings, err := q.GetBackupSettings(ctx)
	if err != nil {
		return res, internal(err)
	}
	dump, err := q.GetBackupDump(ctx, *run.DumpID)
	if errors.Is(err, pgx.ErrNoRows) {
		return res, driver.Errf(driver.DumpUnavailable)
	}
	if err != nil {
		return res, internal(err)
	}
	now, err := q.BackupDBNow(ctx)
	if err != nil {
		return res, internal(err)
	}
	recipients, err := backup.ActiveRecipientKeys(settings.Recipients, now)
	if err != nil {
		return res, internal(err)
	}
	if len(recipients) == 0 {
		return res, driver.Errf(driver.NoRecipient)
	}
	priv, _, err := w.loadSigning(ctx)
	if err != nil {
		return res, err
	}
	secrets, err := w.openDestinationSecrets(dest)
	if err != nil {
		return res, err
	}
	drv, ok := driver.Lookup(dest.Type)
	if !ok {
		return res, driver.Errf(driver.ServerError)
	}

	path, size, ok := w.dumpFile(dump)
	if !ok {
		return res, driver.Errf(driver.DumpUnavailable)
	}
	if dump.Bytes != nil && *dump.Bytes != size {
		return res, driver.Errf(driver.DumpMismatch)
	}
	if err := w.checkSpace(ctx, run, size); err != nil {
		return res, err
	}
	dt, ok := dumpTime(dump.FileName)
	if !ok {
		return res, driver.Errf(driver.DumpUnavailable)
	}
	stem, err := remotename.NewStem(settings.InstallID, dt, nil)
	if err != nil {
		return res, internal(err)
	}
	objName, manName := remotename.ObjectName(stem), remotename.ManifestName(stem)

	// Encrypt to scratch; the dump's sha256 comes from the same pass (#22).
	scratch := w.scratchPath(run.ID)
	defer os.Remove(scratch)
	enc, err := w.encryptDump(path, scratch, recipients)
	if err != nil {
		return res, err
	}
	if dump.Sha256 == nil || enc.PlainSHA256 != *dump.Sha256 || enc.PlainBytes != size {
		return res, driver.Errf(driver.DumpMismatch) // before any upload; scratch is deleted by the defer
	}

	cfg := driver.Config{Config: dest.Config, Secrets: secrets, Dialer: w.dialer,
		AllowPlainHTTP: w.plainHTTPAllowed(ctx, drv, dest.Config)}
	remote, err := drv.Open(ctx, cfg)
	if err != nil {
		return res, err
	}
	defer remote.Close()

	if _, err := q.SetBackupRunRemoteName(ctx, dbq.SetBackupRunRemoteNameParams{ID: run.ID, RemoteName: &objName}); err != nil {
		return res, internal(err)
	}
	uploaded := false
	defer func() {
		if err != nil && uploaded {
			w.deletePartial(remote, objName, manName)
		}
	}()

	f, err := os.Open(scratch)
	if err != nil {
		return res, internal(err)
	}
	defer f.Close()
	sctx, scancel := context.WithCancelCause(ctx)
	defer scancel(nil)
	prog := newProgress(f)
	stopWatch := watchStall(sctx, scancel, prog, w.o.StallAfter)
	uploaded = true
	perr := remote.Put(sctx, objName, enc.CipherBytes, prog)
	stopWatch()
	if perr != nil {
		if errors.Is(context.Cause(sctx), errStalled) {
			return res, driver.Errf(driver.Stalled)
		}
		return res, perr
	}

	manifest, err := backup.Sign(priv, backup.Manifest{
		Version: backup.ManifestVersion, InstallID: settings.InstallID, Object: objName,
		SHA256: enc.CipherSHA256, Size: enc.CipherBytes, DumpTime: dt.UTC().Format(remotename.TimeLayout),
		SchemaVersion: schemaVersion(dump), LaunchpadVersion: w.o.Version,
	})
	if err != nil {
		return res, internal(err)
	}
	if err := remote.Put(ctx, manName, int64(len(manifest)), bytes.NewReader(manifest)); err != nil {
		return res, err
	}
	if err := w.verifyListing(ctx, remote, stem, objName, manName, enc.CipherBytes, int64(len(manifest))); err != nil {
		return res, err
	}

	cb, sha := enc.CipherBytes, enc.CipherSHA256
	res = result{bytes: &cb, sha: &sha, remote: &objName}
	// The dump is on the destination and verified: its scratch file is not needed
	// again, and the files phase reuses the path.
	_ = os.Remove(scratch)
	// Dump retention runs BEFORE the files phase: the dump is verified, and a long first
	// files copy must not spend the deadline that prune needs (LAUN-80 review).
	res.warn = w.prune(ctx, remote, run, settings.InstallID, int(dest.KeepLast), stem)
	if w.silo != nil {
		fnow, nerr := q.BackupDBNow(ctx)
		if nerr != nil {
			fnow = now
		}
		// A backup run's steps hold only `files`: the dump steps were never recorded in steps.
		var filesWarn string
		// The window of the files retention: from the oldest dump still on the destination
		// (after the dump prune above). A listing that fails means no window: no pruning.
		var window time.Duration
		if entries, lerr := remote.List(ctx, "launchpad-"+settings.InstallID+"-"); lerr == nil {
			names := make([]string, len(entries))
			for i, e := range entries {
				names[i] = e.Name
			}
			oldest, have := backup.OldestDumpTime(settings.InstallID, names)
			window = backup.FilesWindow(fnow, oldest, have)
		}
		res.steps, filesWarn = w.filesPhase(ctx, run, dest, drv, cfg, settings.InstallID, recipients, priv, fnow, window)
		if res.warn == "" {
			res.warn = filesWarn
		}
	}
	return res, nil
}

func schemaVersion(d dbq.BackupDump) int {
	if d.MigrationVersion != nil && *d.MigrationVersion >= 0 {
		return int(*d.MigrationVersion)
	}
	return 0
}

// openDestinationSecrets opens the sealed secrets of a destination and returns the
// plaintext JSON a driver reads. An unreadable blob is secrets_unreadable.
func (w *Worker) openDestinationSecrets(d dbq.BackupDestination) (json.RawMessage, error) {
	if d.SecretsEnc == nil {
		return json.RawMessage(`{}`), nil
	}
	pt, err := w.keys.Open(platform.BackupColDestinationSecrets, d.ID.String(), d.SecretsEnc)
	if err != nil {
		return nil, driver.Errf(driver.SecretsUnreadable)
	}
	return pt, nil
}

// checkSpace is the free-space rule of #18: the scratch volume must hold this run's
// dump plus the dumps of the backup runs that are running and started before it. A
// run that cannot fails no_space BEFORE it does any work.
func (w *Worker) checkSpace(ctx context.Context, run dbq.BackupRun, size int64) error {
	need := uint64(size)
	if run.StartedAt != nil {
		rows, err := dbq.New(w.pool).ListEarlierRunningBackupDumpBytes(ctx, dbq.ListEarlierRunningBackupDumpBytesParams{ID: run.ID, StartedAt: *run.StartedAt})
		if err != nil {
			return internal(err)
		}
		for _, r := range rows {
			need += uint64(r.DumpBytes)
		}
	}
	free, err := w.o.FreeBytes(w.o.WorkDir)
	if err != nil {
		return internal(err)
	}
	if free < need {
		return driver.Errf(driver.NoSpace)
	}
	return nil
}

// encryptDump reads the dump once, hashing it and encrypting it into scratch.
func (w *Worker) encryptDump(src, scratch string, recipients []string) (backup.EncryptResult, error) {
	in, err := os.Open(src)
	if err != nil {
		return backup.EncryptResult{}, driver.Errf(driver.DumpUnavailable)
	}
	defer in.Close()
	out, err := os.OpenFile(scratch, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return backup.EncryptResult{}, scratchError(err)
	}
	var dst io.Writer = out
	if w.o.ScratchWrap != nil {
		dst = w.o.ScratchWrap(out)
	}
	bw := bufio.NewWriterSize(dst, 256<<10)
	rt := &readTracker{r: in}
	enc, err := backup.Encrypt(bw, rt, recipients)
	if err == nil {
		err = bw.Flush()
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if rt.err != nil {
			return backup.EncryptResult{}, driver.Errf(driver.DumpUnavailable)
		}
		return backup.EncryptResult{}, scratchError(err)
	}
	return enc, nil
}

// scratchError: a full volume is scratch_full (not retried); anything else is internal.
func scratchError(err error) error {
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return driver.Errf(driver.ScratchFull)
	}
	return internal(err)
}

// verifyListing lists the stem and compares names and sizes ("list and compare size").
func (w *Worker) verifyListing(ctx context.Context, r driver.Remote, stem, obj, man string, objSize, manSize int64) error {
	entries, err := r.List(ctx, stem)
	if err != nil {
		return err
	}
	var gotObj, gotMan bool
	for _, e := range entries {
		switch e.Name {
		case obj:
			if e.Size >= 0 && e.Size != objSize {
				return driver.Errf(driver.ServerError)
			}
			gotObj = true
		case man:
			if e.Size >= 0 && e.Size != manSize {
				return driver.Errf(driver.ServerError)
			}
			gotMan = true
		}
	}
	if !gotObj || !gotMan {
		return driver.Errf(driver.NotFound)
	}
	return nil
}

// prune applies retention after a verified upload (#33). A failure is a warning:
// the run stays succeeded and its error_text says prune_failed.
func (w *Worker) prune(ctx context.Context, r driver.Remote, run dbq.BackupRun, installID string, keep int, stem string) string {
	q := dbq.New(w.pool)
	now, err := q.BackupDBNow(ctx)
	if err != nil {
		return "prune_failed"
	}
	running := map[string]bool{}
	if names, err := q.ListRunningBackupRemoteNames(ctx, run.ID); err == nil {
		for _, n := range names {
			if n != nil {
				running[strings.TrimSuffix(*n, remotename.ObjectSuffix)] = true
			}
		}
	}
	res, err := backup.Prune(ctx, r, installID, keep, now, running, stem)
	if err != nil || res.Failed > 0 {
		return "prune_failed"
	}
	return ""
}

// deletePartial removes the objects a failed run chose, best effort, with its own
// short deadline (the run's context may be the reason it failed).
func (w *Worker) deletePartial(r driver.Remote, names ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, n := range names {
		_ = r.Delete(ctx, n)
	}
}
