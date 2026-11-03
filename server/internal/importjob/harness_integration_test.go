package importjob

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/auth"
	"devops/internal/dbq"
	"devops/internal/httpx"
	"devops/internal/importer"
	"devops/internal/importer/linear"
	"devops/internal/testdb"
)

// The manager against a real, freshly migrated database with the test fixture
// (INTEGRATION=1), a fake Linear and an in-memory object store.

type harness struct {
	t       *testing.T
	pool    *pgxpool.Pool
	q       *dbq.Queries
	m       *Manager
	org     *auth.Org
	chen    uuid.UUID
	store   *memStore
	linear  *fakeLinear
	workDir string
	logs    *syncBuf
	cfg     Config
}

func newHarness(t *testing.T, mods ...func(*Config)) *harness {
	t.Helper()
	pool, _ := testdb.New(t)
	testdb.Fixture(t, pool)
	q := dbq.New(pool)
	o, err := q.GetOrgBySlug(context.Background(), testdb.OrgSlug)
	if err != nil {
		t.Fatal(err)
	}
	chen, err := q.GetUserByUsername(context.Background(), "chen")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, pool: pool, q: q, org: &auth.Org{ID: o.ID, Slug: o.Slug, Name: o.Name, Role: "admin", Access: auth.TeamAccess{All: true}}, chen: chen.ID,
		store: newMemStore(), linear: newFakeLinear(t, fixtureExport()), logs: &syncBuf{},
		workDir: filepath.Join(t.TempDir(), "work")}
	h.cfg = Config{
		Pool: pool, Store: h.store, WorkDir: h.workDir, PublicBaseURL: "https://tracker.example",
		LinearEndpoint: h.linear.srv.URL, Log: slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Heartbeat: 40 * time.Millisecond, ProgressEvery: 20 * time.Millisecond, StaleAfter: time.Minute,
		FloorBytes: 1, FreeBytes: func(string) (uint64, error) { return 50 << 30, nil },
	}
	for _, mod := range mods {
		mod(&h.cfg)
	}
	h.m = h.newManager()
	return h
}

// newManager is another manager on the same database (a second instance, or
// a restart).
func (h *harness) newManager() *Manager {
	m := New(h.cfg)
	// The exporter downloads nothing from the fake: the files of the fixture
	// are added to the export it wrote, as a download would have.
	m.exportFn = func(ctx context.Context, c *linear.Client, dl *linear.Downloader, opts linear.Options) (*importer.Export, error) {
		opts.Files = false
		ex, err := linear.Export(ctx, c, dl, opts)
		if err != nil {
			return nil, err
		}
		ex.Files = fixtureUploads()
		root, err := os.OpenRoot(opts.OutDir)
		if err != nil {
			return nil, err
		}
		defer func() { _ = root.Close() }()
		for rel, body := range fixtureFiles {
			if err := root.MkdirAll(filepath.Dir(rel), 0o700); err != nil {
				return nil, err
			}
			if err := root.WriteFile(rel, []byte(body), 0o600); err != nil {
				return nil, err
			}
		}
		if err := importer.WriteExport(root, ex, time.Now()); err != nil {
			return nil, err
		}
		return ex, nil
	}
	h.t.Cleanup(m.Close)
	return m
}

// ok fails the test on an error.
func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// code is the API code of an *httpx.Error ("" for any other error).
func code(err error) httpx.Code {
	var he *httpx.Error
	if errors.As(err, &he) {
		return he.Code
	}
	return ""
}

func status(err error) int {
	var he *httpx.Error
	if errors.As(err, &he) {
		return he.Status
	}
	return 0
}

func (h *harness) wantCode(err error, want httpx.Code) {
	h.t.Helper()
	if code(err) != want {
		h.t.Fatalf("error %v (code %q), want code %q", err, code(err), want)
	}
}

// wait polls a job until it ends.
func (h *harness) wait(id uuid.UUID) JobView {
	h.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		v, err := h.m.GetJob(context.Background(), h.org.ID, id)
		if err != nil {
			h.t.Fatalf("GetJob: %v", err)
		}
		if v.State != JobQueued && v.State != JobRunning {
			return v
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("job %s did not end", id)
	return JobView{}
}

func (h *harness) session() SessionView {
	h.t.Helper()
	v, err := h.m.Current(context.Background(), h.org.ID)
	ok(h.t, err)
	return v
}

func (h *harness) newSession() SessionView {
	h.t.Helper()
	v, _, err := h.m.StartSession(context.Background(), h.org.ID, h.chen, "")
	ok(h.t, err)
	return v
}

// exportDone makes a session and runs the export to the end.
func (h *harness) exportDone() SessionView {
	h.t.Helper()
	s := h.newSession()
	j, err := h.m.StartExport(context.Background(), h.org, s.ID, []byte(testKey))
	if err != nil {
		h.t.Fatalf("StartExport: %v", err)
	}
	if v := h.wait(j.ID); v.State != JobSucceeded {
		h.t.Fatalf("export ended %s (%v %v)", v.State, v.ErrorCode, v.ErrorText)
	}
	return h.session()
}

const fixtureMap = `{"default":"chen","users":{"lin-user-bot":"claude"}}`

// mapped is exportDone plus a saved user map.
func (h *harness) mapped() SessionView {
	h.t.Helper()
	s := h.exportDone()
	_, err := h.m.PutUserMap(context.Background(), h.org.ID, s.ID, []byte(fixtureMap))
	ok(h.t, err)
	return h.session()
}

// dryDone runs a dry run to the end and requires it to succeed.
func (h *harness) dryDone(s SessionView) JobView {
	h.t.Helper()
	j, err := h.m.StartDryRun(context.Background(), h.org, s.ID, false)
	if err != nil {
		h.t.Fatalf("StartDryRun: %v", err)
	}
	v := h.wait(j.ID)
	if v.State != JobSucceeded {
		h.t.Fatalf("dry run ended %s (%v)", v.State, v.ErrorCode)
	}
	return v
}

// runDone runs the real import to the end.
func (h *harness) runStart(s SessionView) (JobView, error) {
	return h.m.StartRun(context.Background(), h.org, s.ID, h.org.Slug)
}

func (h *harness) count(sql string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		h.t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// issuesImported is the number of issues the import wrote (mapped rows).
func (h *harness) issuesImported() int {
	return h.count(`SELECT count(*) FROM import_mappings WHERE org_id = $1 AND entity_type = 'issue'`, h.org.ID)
}

func (h *harness) sessionState(id uuid.UUID) string {
	h.t.Helper()
	s, err := h.q.ImportSessionGet(context.Background(), dbq.ImportSessionGetParams{OrgID: h.org.ID, ID: id})
	if err != nil {
		h.t.Fatal(err)
	}
	return s.State
}

// setSessionState forces a state (the tests of the cycles guard and of the
// transition table start from every state).
func (h *harness) setSessionState(id uuid.UUID, state string) {
	h.t.Helper()
	testdb.MustExec(h.t, h.pool, `UPDATE import_sessions SET state = $1 WHERE id = $2`, state, id)
}

// blockAt makes jobs of kind stop at phase until the returned release is
// called (or the test ends). reached closes when a job gets there.
func (h *harness) blockAt(kind, phase string) (reached <-chan struct{}, release func()) {
	r := make(chan struct{})
	gate := make(chan struct{})
	once, rel := new(onceFlag), new(onceFlag)
	h.m.progressHook = func(k, p string) {
		if k == kind && p == phase && once.first() {
			close(r)
			<-gate
		}
	}
	release = func() {
		if rel.first() {
			close(gate)
		}
	}
	h.t.Cleanup(release)
	return r, release
}

type onceFlag struct{ used atomic.Bool }

func (o *onceFlag) first() bool { return o.used.CompareAndSwap(false, true) }

func recv(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}
