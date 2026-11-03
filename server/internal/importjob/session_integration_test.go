package importjob

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/httpx"
	"devops/internal/importer"
	"devops/internal/importer/linear"
	"devops/internal/storage"
	"devops/internal/testdb"
)

func TestStartSession_PreflightErrors(t *testing.T) {
	testdb.Require(t)
	ctx := context.Background()
	t.Run("no store is 503 attachments_unavailable", func(t *testing.T) {
		h := newHarness(t, func(c *Config) { c.Store = nil })
		_, _, err := h.m.StartSession(ctx, h.org.ID, h.chen, "")
		h.wantCode(err, httpx.CodeAttachmentsUnavailable)
		if status(err) != 503 {
			t.Fatalf("status %d", status(err))
		}
	})
	t.Run("an unreadable store is 503 attachments_unavailable", func(t *testing.T) {
		h := newHarness(t)
		h.store.failHead = storage.ErrNotFound // not found is fine ...
		_, _, err := h.m.StartSession(ctx, h.org.ID, h.chen, "")
		ok(t, err)
		h2 := newHarness(t)
		h2.store.failHead = os.ErrPermission // ... anything else is not
		_, _, err = h2.m.StartSession(ctx, h2.org.ID, h2.chen, "")
		h2.wantCode(err, httpx.CodeAttachmentsUnavailable)
	})
	t.Run("an empty PUBLIC_BASE_URL is 409 public_url_required", func(t *testing.T) {
		h := newHarness(t, func(c *Config) { c.PublicBaseURL = "" })
		_, _, err := h.m.StartSession(ctx, h.org.ID, h.chen, "")
		h.wantCode(err, httpx.CodePublicURLRequired)
		if status(err) != 409 {
			t.Fatalf("status %d", status(err))
		}
		if n := h.count(`SELECT count(*) FROM import_sessions`); n != 0 {
			t.Fatalf("a refused start made %d session(s)", n)
		}
	})
}

func TestStartSession_ReturnsTheOpenOne(t *testing.T) {
	testdb.Require(t)
	h := newHarness(t)
	ctx := context.Background()
	a, created, err := h.m.StartSession(ctx, h.org.ID, h.chen, "")
	if err != nil || !created || a.State != StateNew {
		t.Fatalf("first start: %+v created=%v err=%v", a, created, err)
	}
	b, created, err := h.m.StartSession(ctx, h.org.ID, h.chen, "")
	if err != nil || created || b.ID != a.ID {
		t.Fatalf("second start: %+v created=%v err=%v", b, created, err)
	}
	// Closing it lets a new one start.
	if err := h.m.CloseSession(ctx, h.org.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	c, created, err := h.m.StartSession(ctx, h.org.ID, h.chen, "")
	if err != nil || !created || c.ID == a.ID {
		t.Fatalf("after close: %+v created=%v err=%v", c, created, err)
	}
	// Closed is final.
	err = h.m.CloseSession(ctx, h.org.ID, a.ID)
	h.wantCode(err, httpx.CodeConflict)
}

// decisions-T17 #39: the work directory and the session directory are 0700.
func TestWorkDir_Modes(t *testing.T) {
	testdb.Require(t)
	h := newHarness(t)
	// A directory that exists with loose modes is tightened.
	if err := os.MkdirAll(h.workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := h.newSession()
	mode := func(p string) os.FileMode {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Mode().Perm()
	}
	if m := mode(h.workDir); m != 0o700 {
		t.Fatalf("work dir mode %o, want 700", m)
	}
	var dir string
	if err := h.pool.QueryRow(context.Background(), `SELECT export_dir FROM import_sessions WHERE id = $1`, s.ID).Scan(&dir); err != nil {
		t.Fatal(err)
	}
	if m := mode(filepath.Join(h.workDir, dir)); m != 0o700 {
		t.Fatalf("session dir mode %o, want 700", m)
	}
	// The export: every directory 0700 and every file 0600.
	s2 := h.exportDone()
	_ = s2
	err := filepath.WalkDir(filepath.Join(h.workDir, dir), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		want := os.FileMode(0o600)
		if d.IsDir() {
			want = 0o700
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s has mode %o, want %o", strings.TrimPrefix(p, h.workDir), fi.Mode().Perm(), want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A work directory others can reach, and that the process cannot tighten, is refused.
func TestWorkDir_RefusedWhenNotPrivate(t *testing.T) {
	testdb.Require(t)
	if os.Geteuid() == 0 {
		t.Skip("root can chmod anything")
	}
	h := newHarness(t)
	parent := t.TempDir()
	// A directory owned by someone else cannot be built in a test; a file
	// where the directory should be is the refusal this test can reach.
	f := filepath.Join(parent, "work")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.m.cfg.WorkDir = f
	_, _, err := h.m.StartSession(context.Background(), h.org.ID, h.chen, "")
	if err == nil {
		t.Fatal("a work dir that is a file was accepted")
	}
}

// writeCLIExport places a CLI export (as `importer export` writes it) in the
// work directory under name.
func (h *harness) writeCLIExport(name string, files []importer.File) {
	h.t.Helper()
	if err := os.MkdirAll(h.workDir, 0o700); err != nil {
		h.t.Fatal(err)
	}
	dir := filepath.Join(h.workDir, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		h.t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	ex := fixtureExport()
	ex.Files = files
	if err := importer.WriteExport(root, ex, time.Now()); err != nil {
		h.t.Fatal(err)
	}
}

func TestAdopt(t *testing.T) {
	testdb.Require(t)
	ctx := context.Background()
	t.Run("a valid export starts the session as exported", func(t *testing.T) {
		h := newHarness(t)
		h.writeCLIExport("cli-1", fixtureUploads())
		s, created, err := h.m.StartSession(ctx, h.org.ID, h.chen, "cli-1")
		if err != nil || !created {
			t.Fatalf("adopt: %v created=%v", err, created)
		}
		if s.State != StateExported || s.ExportCounts["issues"] != 3 || s.FailedFiles != 0 {
			t.Fatalf("session %+v", s)
		}
		// The users are readable and the wizard goes on from here.
		_, err = h.m.PutUserMap(ctx, h.org.ID, s.ID, []byte(fixtureMap))
		ok(t, err)
		// The session's directory is the adopted one, and it is swept on close.
		if err := h.m.CloseSession(ctx, h.org.ID, s.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(h.workDir, "cli-1")); !os.IsNotExist(err) {
			t.Fatalf("the adopted directory was not swept: %v", err)
		}
	})
	t.Run("refusals", func(t *testing.T) {
		h := newHarness(t)
		h.writeCLIExport("cli-1", nil)
		if err := os.WriteFile(filepath.Join(h.workDir, "afile"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(h.workDir, "cli-1"), filepath.Join(h.workDir, "link")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(h.workDir, "empty"), 0o700); err != nil {
			t.Fatal(err)
		}
		for name, want := range map[string]httpx.Code{
			"..": httpx.CodeValidationFailed, ".": httpx.CodeValidationFailed, "../x": httpx.CodeValidationFailed,
			"a/b": httpx.CodeValidationFailed, "": "", strings.Repeat("a", 65): httpx.CodeValidationFailed,
			"missing": httpx.CodeValidationFailed, "afile": httpx.CodeValidationFailed, "link": httpx.CodeValidationFailed,
			"empty": httpx.CodeValidationFailed, "s-0123456789abcdef0123456789abcdef": httpx.CodeValidationFailed,
		} {
			if name == "" {
				continue
			}
			_, _, err := h.m.StartSession(ctx, h.org.ID, h.chen, name)
			if code(err) != want {
				t.Errorf("adopt %q: %v (code %q), want %q", name, err, code(err), want)
			}
		}
		if n := h.count(`SELECT count(*) FROM import_sessions`); n != 0 {
			t.Fatalf("a refused adopt made %d session(s)", n)
		}
		// An open session blocks an adopt.
		h.newSession()
		_, _, err := h.m.StartSession(ctx, h.org.ID, h.chen, "cli-1")
		h.wantCode(err, httpx.CodeConflict)
	})
	t.Run("a directory in use by an open session is refused", func(t *testing.T) {
		h := newHarness(t)
		h.writeCLIExport("cli-1", nil)
		_, _, err := h.m.StartSession(ctx, h.org.ID, h.chen, "cli-1")
		ok(t, err)
		// A second org cannot adopt the same directory.
		testdb.MustExec(t, h.pool, `INSERT INTO orgs (slug, name) VALUES ('other', 'Other')`)
		var otherID uuid.UUID
		if err := h.pool.QueryRow(ctx, `SELECT id FROM orgs WHERE slug = 'other'`).Scan(&otherID); err != nil {
			t.Fatal(err)
		}
		_, _, err = h.m.StartSession(ctx, otherID, h.chen, "cli-1")
		h.wantCode(err, httpx.CodeConflict)
	})
	t.Run("the manifest check: the download limit is no_space", func(t *testing.T) {
		h := newHarness(t)
		files := fixtureUploads()
		files[1] = importer.File{URL: files[1].URL, Error: linear.ErrTotalLimit}
		h.writeCLIExport("cli-1", files)
		_, _, err := h.m.StartSession(ctx, h.org.ID, h.chen, "cli-1")
		h.wantCode(err, httpx.CodeNoSpace)
		if status(err) != 507 {
			t.Fatalf("status %d", status(err))
		}
		if n := h.count(`SELECT count(*) FROM import_sessions`); n != 0 {
			t.Fatalf("a refused adopt made %d session(s)", n)
		}
	})
	t.Run("the manifest check: another file error sets failed_files", func(t *testing.T) {
		h := newHarness(t)
		files := fixtureUploads()
		files[1] = importer.File{URL: files[1].URL + "?sig=SECRETSIG", Error: "download failed: status 404"}
		h.writeCLIExport("cli-1", files)
		s, _, err := h.m.StartSession(ctx, h.org.ID, h.chen, "cli-1")
		if err != nil {
			t.Fatal(err)
		}
		if s.FailedFiles != 1 || len(s.FailedFileNames) != 1 || s.FailedFileNames[0].Name != "ffff" ||
			!strings.Contains(s.FailedFileNames[0].Reason, "404") {
			t.Fatalf("failed files %d %+v", s.FailedFiles, s.FailedFileNames)
		}
		if strings.Contains(s.FailedFileNames[0].Name+s.FailedFileNames[0].Reason, "SECRETSIG") {
			t.Fatal("a query string reached the failed-file list")
		}
	})
}
