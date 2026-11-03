package importjob

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"devops/internal/httpx"
	"devops/internal/importer"
	"devops/internal/importer/linear"
	"devops/internal/testdb"
)

func ctx() context.Context { return context.Background() }

func TestExport_Succeeds(t *testing.T) {
	h := newHarness(t)
	s := h.exportDone()
	if s.State != StateExported {
		t.Fatalf("state %s", s.State)
	}
	if s.ExportCounts["issues"] != 3 {
		t.Fatalf("counts %v", s.ExportCounts)
	}
	n, auth := h.linear.seen()
	if n == 0 {
		t.Fatal("fake Linear saw nothing")
	}
	for _, a := range auth {
		if a != testKey {
			t.Fatalf("unexpected Authorization %q", a)
		}
	}
}

func TestExport_BadKeyMakesNoJob(t *testing.T) {
	h := newHarness(t)
	s := h.newSession()
	h.linear.status.Store(401)
	_, err := h.m.StartExport(ctx(), h.org, s.ID, []byte(testKey))
	h.wantCode(err, httpx.CodeLinearUnauthorized)
	if h.count(`SELECT count(*) FROM import_jobs`) != 0 {
		t.Fatal("a job row exists")
	}
	if h.session().State != StateNew {
		t.Fatalf("state %s", h.session().State)
	}
	h.linear.status.Store(0)
	h.linear.srv.CloseClientConnections()
	h.linear.srv.Close()
	_, err = h.m.StartExport(ctx(), h.org, s.ID, []byte(testKey))
	h.wantCode(err, httpx.CodeLinearUnreachable)
}

func TestExport_NoSpaceAtStart(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.FloorBytes = 1 << 30
		c.FreeBytes = func(string) (uint64, error) { return 10, nil }
	})
	s := h.newSession()
	_, err := h.m.StartExport(ctx(), h.org, s.ID, []byte(testKey))
	h.wantCode(err, httpx.CodeNoSpace)
	if status(err) != 507 {
		t.Fatalf("status %d", status(err))
	}
}

func TestExport_FailedExportFailsSession(t *testing.T) {
	h := newHarness(t)
	s := h.newSession()
	h.m.exportFn = func(context.Context, *linear.Client, *linear.Downloader, linear.Options) (*importer.Export, error) {
		return nil, errors.New("boom for chen@example.com")
	}
	j, err := h.m.StartExport(ctx(), h.org, s.ID, []byte(testKey))
	ok(t, err)
	v := h.wait(j.ID)
	if v.State != JobFailed || v.ErrorCode == nil || *v.ErrorCode != CodeExportFailed {
		t.Fatalf("job %+v", v)
	}
	if h.session().State != StateFailed {
		t.Fatalf("state %s", h.session().State)
	}
	if strings.Contains(h.logs.String(), "chen@example.com") {
		t.Fatal("an email reached the log")
	}
}

func TestUserMapAndDryRunAndRun(t *testing.T) {
	h := newHarness(t)
	s := h.mapped()
	if s.State != StateUserMap {
		t.Fatalf("state %s", s.State)
	}
	// a run without a dry run
	_, err := h.runStart(s)
	h.wantCode(err, httpx.CodeDryRunRequired)

	dv := h.dryDone(s)
	if dv.Report == nil || !dv.Report.DryRun {
		t.Fatalf("report %+v", dv.Report)
	}
	if h.issuesImported() != 0 || h.store.count() != 0 || h.store.puts.Load() != 0 {
		t.Fatal("dry run wrote rows or objects")
	}
	if h.count(`SELECT count(*) FROM issues WHERE org_id = $1`, h.org.ID) != 0 {
		t.Fatal("dry run wrote issues")
	}
	if h.session().State != StateDryRunOK {
		t.Fatalf("state %s", h.session().State)
	}
	// wrong confirmation
	_, err = h.m.StartRun(ctx(), h.org, s.ID, "other")
	h.wantCode(err, httpx.CodeValidationFailed)

	j, err := h.runStart(s)
	ok(t, err)
	rv := h.wait(j.ID)
	if rv.State != JobSucceeded || !rv.Committed {
		t.Fatalf("run %+v %v", rv, rv.ErrorCode)
	}
	if rv.Report == nil || dv.Report.Uploads.Planned != rv.Report.Uploads.Uploaded || rv.Report.Uploads.Uploaded == 0 {
		t.Fatalf("planned %+v uploaded %+v", dv.Report.Uploads, rv.Report.Uploads)
	}
	for k, c := range dv.Report.Counts {
		if c.Create != rv.Report.Counts[k].Create {
			t.Fatalf("%s: dry created %d, real %d", k, c.Create, rv.Report.Counts[k].Create)
		}
	}
	if h.session().State != StateImported || h.issuesImported() != 3 {
		t.Fatalf("state %s issues %d", h.session().State, h.issuesImported())
	}

	// a second run: zero creates, no upload
	puts := h.store.puts.Load()
	j, err = h.runStart(h.session())
	ok(t, err)
	r2 := h.wait(j.ID)
	if r2.State != JobSucceeded {
		t.Fatalf("second run %s", r2.State)
	}
	for k, c := range r2.Report.Counts {
		if c.Create != 0 {
			t.Fatalf("second run created %d %s", c.Create, k)
		}
	}
	if h.store.puts.Load() != puts {
		t.Fatal("second run uploaded again")
	}

	// verify
	vj, err := h.m.StartVerify(ctx(), h.org, s.ID)
	ok(t, err)
	vv := h.wait(vj.ID)
	if vv.State != JobSucceeded || len(vv.Rows) == 0 {
		t.Fatalf("verify %+v", vv)
	}
	if h.session().State != StateVerified {
		t.Fatalf("state %s", h.session().State)
	}
}

func TestDryRunRequired_AfterChange(t *testing.T) {
	h := newHarness(t)
	s := h.mapped()
	h.dryDone(s)
	// a changed user map
	_, err := h.m.PutUserMap(ctx(), h.org.ID, s.ID, []byte(`{"default":"claude","users":{"lin-user-bot":"claude"}}`))
	ok(t, err)
	_, err = h.runStart(h.session())
	h.wantCode(err, httpx.CodeDryRunRequired)
}

func TestVerify_Mismatch(t *testing.T) {
	h := newHarness(t)
	s := h.mapped()
	h.dryDone(s)
	j, err := h.runStart(s)
	ok(t, err)
	if v := h.wait(j.ID); v.State != JobSucceeded {
		t.Fatalf("run %s", v.State)
	}
	testdb.MustExec(t, h.pool, `DELETE FROM issues WHERE id = (SELECT id FROM issues WHERE org_id = $1 LIMIT 1)`, h.org.ID)
	vj, err := h.m.StartVerify(ctx(), h.org, s.ID)
	ok(t, err)
	vv := h.wait(vj.ID)
	if vv.State != JobFailed || vv.ErrorCode == nil || *vv.ErrorCode != CodeVerifyMismatch {
		t.Fatalf("verify %+v %v", vv.State, vv.ErrorCode)
	}
	found := false
	for _, r := range vv.Rows {
		if r.Expected != r.Actual {
			found = true
		}
	}
	if !found {
		t.Fatal("no mismatching row")
	}
	if h.session().State != StateFailed {
		t.Fatalf("state %s", h.session().State)
	}
}

// A mismatch before the commit keeps no rows; after it, the rows stay.
func TestRun_MismatchBeforeAndAfterCommit(t *testing.T) {
	h := newHarness(t)
	s := h.mapped()
	h.dryDone(s)
	// the object vanishes from the store between the upload and the
	// in-transaction check: the head fails once files are uploaded
	h.store.failHead = errors.New("store down")
	j, err := h.runStart(s)
	ok(t, err)
	v := h.wait(j.ID)
	if v.State != JobFailed || v.Committed {
		t.Fatalf("job %s committed %v", v.State, v.Committed)
	}
	if h.issuesImported() != 0 {
		t.Fatal("rows kept after a failure before commit")
	}
	if h.session().State != StateFailed {
		t.Fatalf("state %s", h.session().State)
	}
}

func TestCancel_DuringImport(t *testing.T) {
	h := newHarness(t)
	s := h.mapped()
	h.dryDone(s)
	reached, release := h.blockAt(KindImport, "issues")
	j, err := h.runStart(s)
	ok(t, err)
	recv(t, reached, "the issues phase")
	_, err = h.m.Cancel(ctx(), h.org.ID, j.ID)
	ok(t, err)
	release()
	v := h.wait(j.ID)
	if v.State != JobCanceled {
		t.Fatalf("job %s %v", v.State, v.ErrorCode)
	}
	if h.issuesImported() != 0 || v.Committed {
		t.Fatal("rows kept after cancel")
	}
	if h.session().State != StateFailed {
		t.Fatalf("state %s", h.session().State)
	}
	// the files stay in the store; the second run uploads nothing
	if h.store.count() == 0 {
		t.Fatal("files were not uploaded before the transaction")
	}
	h.m.progressHook = nil
	h.dryDone(h.session())
	j, err = h.runStart(h.session())
	ok(t, err)
	r := h.wait(j.ID)
	if r.State != JobSucceeded || r.Report.Uploads.Uploaded != 0 || h.issuesImported() != 3 {
		t.Fatalf("second run %s uploaded %d", r.State, r.Report.Uploads.Uploaded)
	}
}

func TestRestart_InterruptsImport(t *testing.T) {
	h := newHarness(t)
	s := h.mapped()
	h.dryDone(s)
	reached, release := h.blockAt(KindImport, "issues")
	j, err := h.runStart(s)
	ok(t, err)
	recv(t, reached, "the issues phase")
	go func() { time.Sleep(50 * time.Millisecond); release() }()
	h.m.Close()
	v, err := h.m.GetJob(ctx(), h.org.ID, j.ID)
	ok(t, err)
	if v.State != JobFailed || v.ErrorCode == nil || *v.ErrorCode != CodeInterrupted {
		t.Fatalf("job %s %v", v.State, v.ErrorCode)
	}
	if h.issuesImported() != 0 {
		t.Fatal("rows kept")
	}
	if h.session().State != StateFailed {
		t.Fatalf("state %s", h.session().State)
	}
	// the index is free and a new manager completes the work
	h.m = h.newManager()
	h.dryDone(h.session())
	j, err = h.runStart(h.session())
	ok(t, err)
	if v := h.wait(j.ID); v.State != JobSucceeded {
		t.Fatalf("after restart %s", v.State)
	}
}

func TestParallelStarts_OneJobActive(t *testing.T) {
	h := newHarness(t)
	s := h.mapped()
	h.dryDone(s)
	reached, release := h.blockAt(KindImport, "issues")
	j, err := h.runStart(s)
	ok(t, err)
	recv(t, reached, "the issues phase")
	_, err = h.runStart(s)
	h.wantCode(err, httpx.CodeJobActive)
	_, err = h.m.StartVerify(ctx(), h.org, s.ID)
	h.wantCode(err, httpx.CodeJobActive)
	if status(err) != 409 {
		t.Fatalf("status %d", status(err))
	}
	// the unique index itself
	_, ierr := h.pool.Exec(ctx(), `INSERT INTO import_jobs (org_id, session_id, kind, state, instance_id) VALUES ($1,$2,'verify','queued','x')`, h.org.ID, s.ID)
	if ierr == nil {
		t.Fatal("second active job inserted")
	}
	release()
	h.wait(j.ID)
}

func TestCancel_AcrossInstances(t *testing.T) {
	h := newHarness(t)
	s := h.mapped()
	h.dryDone(s)
	reached, release := h.blockAt(KindImport, "issues")
	j, err := h.runStart(s)
	ok(t, err)
	recv(t, reached, "the issues phase")
	other := h.newManager()
	_, err = other.Cancel(ctx(), h.org.ID, j.ID)
	ok(t, err)
	// the owner reads the flag at its next heartbeat (40 ms here); the
	// block is released after that, so the job sees its context end
	time.Sleep(400 * time.Millisecond)
	release()
	if v := h.wait(j.ID); v.State != JobCanceled {
		t.Fatalf("job %s", v.State)
	}
}

func TestCancel_WaitingOnOrgLock(t *testing.T) {
	h := newHarness(t)
	s := h.mapped()
	h.dryDone(s)
	tx, err := h.pool.Begin(ctx())
	ok(t, err)
	defer func() { _ = tx.Rollback(ctx()) }()
	_, err = tx.Exec(ctx(), `SELECT pg_advisory_xact_lock(hashtextextended('devops:import:'||$1::text, 0))`, h.org.ID.String())
	ok(t, err)
	j, err := h.runStart(s)
	ok(t, err)
	time.Sleep(300 * time.Millisecond)
	if v, _ := h.m.GetJob(ctx(), h.org.ID, j.ID); v.State != JobRunning {
		t.Fatalf("job %s, want running while the lock is held", v.State)
	}
	_, err = h.m.Cancel(ctx(), h.org.ID, j.ID)
	ok(t, err)
	if v := h.wait(j.ID); v.State != JobCanceled {
		t.Fatalf("job %s", v.State)
	}
	if h.issuesImported() != 0 {
		t.Fatal("rows")
	}
}

func TestStaleJobSweep(t *testing.T) {
	for _, tc := range []struct {
		kind, from, wantSession string
	}{
		{KindExport, StateExporting, StateFailed},
		{KindDryRun, StateDryRunOK, StateFailed},
		{KindImport, StateImporting, StateFailed},
		{KindVerify, StateImported, StateImported},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			h := newHarness(t)
			s := h.newSession()
			h.setSessionState(s.ID, tc.from)
			testdb.MustExec(t, h.pool, `INSERT INTO import_jobs (org_id, session_id, kind, state, heartbeat_at, started_at, instance_id)
				VALUES ($1,$2,$3,'running', now() - interval '5 minutes', now() - interval '5 minutes', 'gone')`, h.org.ID, s.ID, tc.kind)
			ok(t, h.m.Sweep(ctx()))
			if got := h.sessionState(s.ID); got != tc.wantSession {
				t.Fatalf("session %s, want %s", got, tc.wantSession)
			}
			var st, ec string
			if err := h.pool.QueryRow(ctx(), `SELECT state, error_code FROM import_jobs WHERE session_id=$1`, s.ID).Scan(&st, &ec); err != nil {
				t.Fatal(err)
			}
			if st != JobFailed || ec != CodeInterrupted {
				t.Fatalf("job %s %s", st, ec)
			}
			// the index is free
			testdb.MustExec(t, h.pool, `INSERT INTO import_jobs (org_id, session_id, kind, state, instance_id) VALUES ($1,$2,'verify','queued','x')`, h.org.ID, s.ID)
		})
	}
}

func TestFailedFiles_DryRunNeedsAccept(t *testing.T) {
	h := newHarness(t)
	s := h.mapped()
	testdb.MustExec(t, h.pool, `UPDATE import_sessions SET failed_files = 1, failed_file_names = '[{"name":"x.png","reason":"http 500"}]' WHERE id = $1`, s.ID)
	_, err := h.m.StartDryRun(ctx(), h.org, s.ID, false)
	h.wantCode(err, httpx.CodeConflict)
	j, err := h.m.StartDryRun(ctx(), h.org, s.ID, true)
	ok(t, err)
	h.wait(j.ID)
}

func TestSessionClose_RemovesDir(t *testing.T) {
	h := newHarness(t)
	s := h.exportDone()
	var dir string
	if err := h.pool.QueryRow(ctx(), `SELECT export_dir FROM import_sessions WHERE id=$1`, s.ID).Scan(&dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.workDir, dir)); err != nil {
		t.Fatalf("export dir: %v", err)
	}
	ok(t, h.m.CloseSession(ctx(), h.org.ID, s.ID))
	if h.session2() {
		t.Fatal("session still current")
	}
	if _, err := os.Stat(filepath.Join(h.workDir, dir)); !os.IsNotExist(err) {
		t.Fatalf("dir not removed: %v", err)
	}
}

func (h *harness) session2() bool {
	_, err := h.m.Current(ctx(), h.org.ID)
	return err == nil
}

// A mismatch after the commit keeps the rows and says so (committed).
func TestRun_MismatchAfterCommit(t *testing.T) {
	// measure the Head calls of a clean run
	h0 := newHarness(t)
	s0 := h0.mapped()
	h0.dryDone(s0)
	before := h0.store.heads.Load()
	j0, err := h0.runStart(s0)
	ok(t, err)
	h0.wait(j0.ID)
	total := h0.store.heads.Load() - before

	h := newHarness(t)
	s := h.mapped()
	h.dryDone(s)
	h.store.failHeadFrom.Store(h.store.heads.Load() + total)
	j, err := h.runStart(s)
	ok(t, err)
	v := h.wait(j.ID)
	if v.State != JobFailed || v.ErrorCode == nil || *v.ErrorCode != CodeVerifyMismatch || !v.Committed {
		t.Fatalf("job %s %v committed %v", v.State, v.ErrorCode, v.Committed)
	}
	if h.issuesImported() != 3 {
		t.Fatal("rows lost after the commit")
	}
	mism := false
	for _, r := range v.Rows {
		if r.Expected != r.Actual {
			mism = true
		}
	}
	if !mism {
		t.Fatal("no mismatch row")
	}
	// a verify can run on the kept rows
	h.store.failHeadFrom.Store(0)
	vj, err := h.m.StartVerify(ctx(), h.org, s.ID)
	ok(t, err)
	if vv := h.wait(vj.ID); vv.State != JobSucceeded {
		t.Fatalf("verify %s", vv.State)
	}
}
