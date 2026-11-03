package importjob

import (
	"context"
	"fmt"
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

// withFiles makes the export step end with the given manifest files.
func (h *harness) withFiles(files []importer.File) {
	h.m.exportFn = func(c context.Context, cl *linear.Client, dl *linear.Downloader, opts linear.Options) (*importer.Export, error) {
		opts.Files = false
		ex, err := linear.Export(c, cl, dl, opts)
		if err != nil {
			return nil, err
		}
		ex.Files = files
		root, err := os.OpenRoot(opts.OutDir)
		if err != nil {
			return nil, err
		}
		defer func() { _ = root.Close() }()
		return ex, importer.WriteExport(root, ex, time.Now())
	}
}

func (h *harness) exportJob() JobView {
	h.t.Helper()
	s := h.newSession()
	j, err := h.m.StartExport(ctx(), h.org, s.ID, []byte(testKey))
	ok(h.t, err)
	return h.wait(j.ID)
}

func TestExport_DownloadLimitIsNoSpace(t *testing.T) {
	h := newHarness(t)
	h.withFiles([]importer.File{{URL: fxUploadA, Error: linear.ErrTotalLimit}})
	v := h.exportJob()
	if v.State != JobFailed || v.ErrorCode == nil || *v.ErrorCode != CodeNoSpace {
		t.Fatalf("job %s %v", v.State, v.ErrorCode)
	}
	if h.session().State != StateFailed {
		t.Fatalf("session %s", h.session().State)
	}
}

func TestExport_OtherFileErrorsSucceedWithNames(t *testing.T) {
	h := newHarness(t)
	var files []importer.File
	for i := 0; i < 25; i++ {
		files = append(files, importer.File{URL: fmt.Sprintf("https://uploads.linear.app/o/%d/f%d.png?token=SECRET", i, i),
			Error: "download failed: status 500"})
	}
	h.withFiles(files)
	v := h.exportJob()
	if v.State != JobSucceeded || v.FailedFiles != 25 || len(v.FailedFileNames) != maxFailedNames {
		t.Fatalf("job %s failed %d names %d", v.State, v.FailedFiles, len(v.FailedFileNames))
	}
	s := h.session()
	if s.FailedFiles != 25 || len(s.FailedFileNames) != 20 || s.State != StateExported {
		t.Fatalf("session %+v", s)
	}
	for _, n := range s.FailedFileNames {
		if strings.Contains(n.Name+n.Reason, "SECRET") {
			t.Fatal("a token reached the file list")
		}
	}
	// the dry run needs the accept flag
	_, err := h.m.PutUserMap(ctx(), h.org.ID, s.ID, []byte(fixtureMap))
	ok(t, err)
	_, err = h.m.StartDryRun(ctx(), h.org, s.ID, false)
	h.wantCode(err, httpx.CodeConflict)
}

func TestUserMap_Validation(t *testing.T) {
	h := newHarness(t)
	s := h.exportDone()
	u, err := h.m.Users(ctx(), h.org.ID, s.ID)
	ok(t, err)
	if len(u.Users) != 2 || len(u.Members) == 0 {
		t.Fatalf("users %+v", u)
	}
	for name, body := range map[string]string{
		"not json":     `{`,
		"unknown user": `{"default":"nobody","users":{}}`,
		"no default":   `{"users":{"lin-user-bot":"claude"}}`,
		"bad member":   `{"default":"chen","users":{"lin-user-bot":"ghost"}}`,
		"flat map":     `{"lin-user-chen":"chen"}`,
	} {
		_, err := h.m.PutUserMap(ctx(), h.org.ID, s.ID, []byte(body))
		if status(err) != 400 {
			t.Errorf("%s: status %d err %v", name, status(err), err)
		}
	}
	_, err = h.m.PutUserMap(ctx(), h.org.ID, s.ID, []byte(`{"users":{"lin-user-chen":"chen","lin-user-bot":"claude"}}`))
	ok(t, err)
	var h1, h2 string
	ok(t, h.pool.QueryRow(ctx(), `SELECT user_map_hash FROM import_sessions WHERE id=$1`, s.ID).Scan(&h1))
	_, err = h.m.PutUserMap(ctx(), h.org.ID, s.ID, []byte(fixtureMap))
	ok(t, err)
	ok(t, h.pool.QueryRow(ctx(), `SELECT user_map_hash FROM import_sessions WHERE id=$1`, s.ID).Scan(&h2))
	if h1 == h2 {
		t.Fatal("the hash did not change with the map")
	}
	// not while a job is active
	testdb.MustExec(t, h.pool, `INSERT INTO import_jobs (org_id, session_id, kind, state, instance_id) VALUES ($1,$2,'verify','running','x')`, h.org.ID, s.ID)
	_, err = h.m.PutUserMap(ctx(), h.org.ID, s.ID, []byte(fixtureMap))
	h.wantCode(err, httpx.CodeJobActive)
	// a deactivated member is refused
	testdb.MustExec(t, h.pool, `UPDATE import_jobs SET state='failed', finished_at=now()`)
	testdb.MustExec(t, h.pool, `UPDATE users SET deactivated_at = now() WHERE username='claude'`)
	_, err = h.m.PutUserMap(ctx(), h.org.ID, s.ID, []byte(fixtureMap))
	if status(err) != 400 {
		t.Fatalf("deactivated member: %v", err)
	}
}

func TestStartRules_ByState(t *testing.T) {
	h := newHarness(t)
	s := h.newSession()
	_, err := h.m.StartDryRun(ctx(), h.org, s.ID, false)
	h.wantCode(err, httpx.CodeConflict)
	_, err = h.m.StartVerify(ctx(), h.org, s.ID)
	h.wantCode(err, httpx.CodeConflict)
	_, err = h.runStart(s)
	h.wantCode(err, httpx.CodeConflict)
	_, err = h.m.PutUserMap(ctx(), h.org.ID, s.ID, []byte(fixtureMap))
	h.wantCode(err, httpx.CodeConflict)
	ok(t, h.m.CloseSession(ctx(), h.org.ID, s.ID))
	_, err = h.m.StartExport(ctx(), h.org, s.ID, []byte(testKey))
	h.wantCode(err, httpx.CodeConflict)
}

func TestCloseSession_Rules(t *testing.T) {
	h := newHarness(t)
	s := h.exportDone()
	var dir string
	ok(t, h.pool.QueryRow(ctx(), `SELECT export_dir FROM import_sessions WHERE id=$1`, s.ID).Scan(&dir))
	if _, err := os.Stat(filepath.Join(h.workDir, dir)); err != nil {
		t.Fatalf("export dir: %v", err)
	}
	testdb.MustExec(t, h.pool, `INSERT INTO import_jobs (org_id, session_id, kind, state, instance_id) VALUES ($1,$2,'verify','running','x')`, h.org.ID, s.ID)
	h.wantCode(h.m.CloseSession(ctx(), h.org.ID, s.ID), httpx.CodeJobActive)
	testdb.MustExec(t, h.pool, `UPDATE import_jobs SET state='failed', finished_at=now()`)
	ok(t, h.m.CloseSession(ctx(), h.org.ID, s.ID))
	if _, err := os.Stat(filepath.Join(h.workDir, dir)); !os.IsNotExist(err) {
		t.Fatalf("dir not removed: %v", err)
	}
	if _, err := h.m.Current(ctx(), h.org.ID); err == nil {
		t.Fatal("closed session is current")
	}
	// the org may start a new session
	h.newSession()
}

func TestSweep_IdleSessionAndOrphanDir(t *testing.T) {
	now := time.Now()
	clock := &now
	h := newHarness(t, func(c *Config) { c.Now = func() time.Time { return *clock }; c.SessionTTL = 7 * 24 * time.Hour })
	s := h.exportDone()
	var dir string
	ok(t, h.pool.QueryRow(ctx(), `SELECT export_dir FROM import_sessions WHERE id=$1`, s.ID).Scan(&dir))
	orphan := filepath.Join(h.workDir, sessionPrefix+"orphan")
	ok(t, os.Mkdir(orphan, 0o700))
	operator := filepath.Join(h.workDir, "operator-export")
	ok(t, os.Mkdir(operator, 0o700))
	old := now.Add(-3 * time.Hour)
	ok(t, os.Chtimes(orphan, old, old))
	ok(t, os.Chtimes(operator, old, old))
	ok(t, h.m.Sweep(ctx()))
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan kept")
	}
	if _, err := os.Stat(operator); err != nil {
		t.Fatal("operator dir removed")
	}
	if _, err := h.m.Current(ctx(), h.org.ID); err != nil {
		t.Fatal("fresh session closed")
	}
	later := now.Add(8 * 24 * time.Hour)
	*clock = later
	testdb.MustExec(t, h.pool, `UPDATE import_sessions SET updated_at = $1`, now.Add(-8*24*time.Hour))
	ok(t, h.m.Sweep(ctx()))
	if _, err := h.m.Current(ctx(), h.org.ID); err == nil {
		t.Fatal("idle session kept")
	}
	if _, err := os.Stat(filepath.Join(h.workDir, dir)); !os.IsNotExist(err) {
		t.Fatal("idle session dir kept")
	}
}

func TestNextState_Table(t *testing.T) {
	type c struct {
		kind string
		end  endKind
		cur  string
		want string
		ok   bool
	}
	for _, tc := range []c{
		{KindExport, endOK, StateExporting, StateExported, true},
		{KindExport, endFailed, StateExporting, StateFailed, true},
		{KindExport, endOK, StateNew, "", false},
		{KindDryRun, endOK, StateUserMap, StateDryRunOK, true},
		{KindDryRun, endOK, StateFailed, StateDryRunOK, true},
		{KindDryRun, endFailed, StateDryRunOK, StateFailed, true},
		{KindImport, endOK, StateImporting, StateImported, true},
		{KindImport, endFailed, StateImporting, StateFailed, true},
		{KindVerify, endOK, StateImported, StateVerified, true},
		{KindVerify, endMismatch, StateVerified, StateFailed, true},
		{KindVerify, endFailed, StateImported, "", false},
		{KindVerify, endOK, StateClosed, "", false},
	} {
		got, okk := nextState(tc.kind, tc.end, tc.cur)
		if got != tc.want || okk != tc.ok {
			t.Errorf("%v: got %q %v", tc, got, okk)
		}
	}
}

// The Linear key and the emails of the export never reach a log, a row,
// an event or an idempotency record.
func TestNoSecrets(t *testing.T) {
	h := newHarness(t)
	s := h.mapped()
	h.dryDone(s)
	j, err := h.runStart(s)
	ok(t, err)
	h.wait(j.ID)
	vj, err := h.m.StartVerify(ctx(), h.org, s.ID)
	ok(t, err)
	h.wait(vj.ID)
	// a failing export, too
	h.linear.status.Store(401)
	_, _ = h.m.StartExport(ctx(), h.org, s.ID, []byte(testKey))
	h.linear.status.Store(0)

	if strings.Contains(h.logs.String(), testKey) || strings.Contains(h.logs.String(), "TESTKEY") {
		t.Fatal("the key is in the log")
	}
	for _, q := range []string{
		`SELECT coalesce(string_agg(i::text, ' '), '') FROM import_sessions i`,
		`SELECT coalesce(string_agg(i::text, ' '), '') FROM import_jobs i`,
		`SELECT coalesce(string_agg(i::text, ' '), '') FROM events i`,
		`SELECT coalesce(string_agg(i::text, ' '), '') FROM idempotency_keys i`,
	} {
		var all string
		if err := h.pool.QueryRow(ctx(), q).Scan(&all); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if strings.Contains(all, "TESTKEY") {
			t.Fatalf("the key is in %s", q)
		}
	}
	if n := h.count(`SELECT count(*) FROM events WHERE entity_type LIKE 'import%' OR type LIKE 'import%'`); n != 0 {
		t.Fatalf("%d import events", n)
	}
}

// One open session per org, enforced by the index, not only by the code.
func TestOneOpenSessionIndex(t *testing.T) {
	h := newHarness(t)
	h.newSession()
	_, err := h.pool.Exec(ctx(), `INSERT INTO import_sessions (org_id, state, export_dir, export_counts) VALUES ($1,'new','s-two','{}')`, h.org.ID)
	if err == nil {
		t.Fatal("a second open session was inserted")
	}
}

// A failed re-export leaves files that no dry run saw: the old hash and the
// old dry run must not stand for them (LAUN-8 review).
func TestReExportFailed_RunNeedsDryRun(t *testing.T) {
	h := newHarness(t)
	s := h.mapped()
	h.dryDone(s)
	h.withFiles([]importer.File{{URL: fxUploadA, Error: linear.ErrTotalLimit}})
	j, err := h.m.StartExport(ctx(), h.org, s.ID, []byte(testKey))
	ok(t, err)
	if v := h.wait(j.ID); v.State != JobFailed {
		t.Fatalf("re-export %s", v.State)
	}
	if h.session().State != StateFailed {
		t.Fatalf("state %s", h.session().State)
	}
	var hash *string
	ok(t, h.pool.QueryRow(ctx(), `SELECT export_hash FROM import_sessions WHERE id=$1`, s.ID).Scan(&hash))
	if hash != nil {
		t.Fatal("the old export hash is still on the session")
	}
	_, err = h.runStart(s)
	h.wantCode(err, httpx.CodeDryRunRequired)
	_, err = h.m.StartDryRun(ctx(), h.org, s.ID, true)
	h.wantCode(err, httpx.CodeConflict)
	if h.issuesImported() != 0 {
		t.Fatal("rows written")
	}
}

// The manifest changed on disk after the dry run: the run is refused.
func TestManifestChangedOnDisk_Refused(t *testing.T) {
	h := newHarness(t)
	s := h.mapped()
	var dir string
	ok(t, h.pool.QueryRow(ctx(), `SELECT export_dir FROM import_sessions WHERE id=$1`, s.ID).Scan(&dir))
	mf := filepath.Join(h.workDir, dir, "manifest.json")
	// before the dry run
	b, err := os.ReadFile(mf)
	ok(t, err)
	ok(t, os.WriteFile(mf, append(b, '\n'), 0o600))
	j, err := h.m.StartDryRun(ctx(), h.org, s.ID, false)
	ok(t, err)
	v := h.wait(j.ID)
	if v.State != JobFailed || v.ErrorCode == nil || *v.ErrorCode != CodeExportChanged {
		t.Fatalf("dry run %s %v", v.State, v.ErrorCode)
	}
	ok(t, os.WriteFile(mf, b, 0o600))
	h.dryDone(h.session())
	// between the dry run and the run
	ok(t, os.WriteFile(mf, append(b, '\n'), 0o600))
	rj, err := h.runStart(h.session())
	ok(t, err)
	rv := h.wait(rj.ID)
	if rv.State != JobFailed || rv.ErrorCode == nil || *rv.ErrorCode != CodeExportChanged || rv.Committed {
		t.Fatalf("run %s %v", rv.State, rv.ErrorCode)
	}
	if h.issuesImported() != 0 {
		t.Fatal("rows written")
	}
}
