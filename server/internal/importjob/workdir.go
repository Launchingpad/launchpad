package importjob

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"devops/internal/httpx"
	"devops/internal/importer"
	"devops/internal/importer/linear"
)

// The work directory (decisions-T17 #39). Files are 0600 and directories 0700:
// an export holds every ticket's text and the users' emails. Nothing from a
// request becomes a path, except an adopt name that is matched, Lstat'd and
// opened with os.Root.

// sessionPrefix starts the directory name of every session the server makes.
const sessionPrefix = "s-"

func sessionDirName(id uuid.UUID) string { return sessionPrefix + hex.EncodeToString(id[:]) }

// ensureWorkDir makes the work directory (0700) and proves it is private and
// writable. A directory that others can read or write is refused: the volume
// is root-owned until the image's directory (owned by uid 10001, mode 0700)
// or deploy's volume-init gives it to the app.
func (m *Manager) ensureWorkDir() error {
	dir := m.cfg.WorkDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		m.log.Error("import: cannot make the work directory", "dir", dir, "err", err)
		return fmt.Errorf("import work directory: %w", err)
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return fmt.Errorf("import work directory %s is not a directory", dir)
	}
	if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil { // not ours to change: refused below
			m.log.Warn("import: cannot set the work directory to 0700", "dir", dir, "err", err)
		}
		if fi, err = os.Stat(dir); err != nil || fi.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("import work directory %s must not be accessible to others (mode 0700)", dir)
		}
	}
	probe, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		m.log.Error("import: the work directory is not writable", "dir", dir, "err", err)
		return fmt.Errorf("import work directory is not writable: %w", err)
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())
	return nil
}

// EnsureWorkDir is the boot check: it logs and returns the error, and the
// caller goes on (a storage fault must not stop the tracker).
func (m *Manager) EnsureWorkDir() error { return m.ensureWorkDir() }

func (m *Manager) makeSessionDir(name string) error {
	root, err := os.OpenRoot(m.cfg.WorkDir)
	if err != nil {
		return fmt.Errorf("open work dir: %w", err)
	}
	defer func() { _ = root.Close() }()
	if err := root.Mkdir(name, 0o700); err != nil {
		return fmt.Errorf("make session dir: %w", err)
	}
	return nil
}

// openExport opens a session's export directory as an os.Root.
func (m *Manager) openExport(name string) (*os.Root, error) {
	root, err := os.OpenRoot(m.cfg.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("open work dir: %w", err)
	}
	defer func() { _ = root.Close() }()
	sub, err := root.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("open export dir: %w", err)
	}
	return sub, nil
}

// exportPath is the path of a session's export directory.
func (m *Manager) exportPath(name string) string { return filepath.Join(m.cfg.WorkDir, name) }

// removeDir deletes a session directory. A failure is logged: the sweep
// removes an orphaned server-made directory later.
func (m *Manager) removeDir(name string) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return
	}
	root, err := os.OpenRoot(m.cfg.WorkDir)
	if err != nil {
		m.log.Warn("import: cannot open the work directory to remove a session directory", "err", err)
		return
	}
	defer func() { _ = root.Close() }()
	if err := root.RemoveAll(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		m.log.Warn("import: cannot remove a session directory", "err", err)
	}
}

// sweepOrphans removes the server-made directories no open session names,
// older than orphanAge. A directory an operator placed for adopt (no "s-"
// prefix) is never touched here.
func (m *Manager) sweepOrphans(ctx context.Context) error {
	ents, err := os.ReadDir(m.cfg.WorkDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read work dir: %w", err)
	}
	open, err := m.q.ImportSessionOpenDirs(ctx)
	if err != nil {
		return fmt.Errorf("open session dirs: %w", err)
	}
	keep := map[string]bool{}
	for _, d := range open {
		keep[d] = true
	}
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), sessionPrefix) || keep[e.Name()] {
			continue
		}
		info, err := e.Info()
		if err != nil || m.cfg.Now().Sub(info.ModTime()) < orphanAge {
			continue
		}
		m.removeDir(e.Name())
		m.log.Info("import: swept an orphaned session directory")
	}
	return nil
}

// fileProblems is the manifest check of the files (decisions-T17 #39).
type fileProblems struct {
	noSpace bool
	count   int
	names   []FailedFile
}

// checkFiles inspects the files of an export's manifest. A file the download
// limit skipped means the export is incomplete (no_space); any other file
// error is counted, and the first maxFailedNames are named.
func checkFiles(files []importer.File) fileProblems {
	pr := fileProblems{names: []FailedFile{}}
	for _, f := range files {
		if f.Error == "" {
			continue
		}
		if f.Error == linear.ErrTotalLimit {
			pr.noSpace = true
			continue
		}
		pr.count++
		if len(pr.names) < maxFailedNames {
			pr.names = append(pr.names, FailedFile{Name: fileLabel(f.URL), Reason: fileReason(f.Error)})
		}
	}
	return pr
}

// fileLabel is the file name of an upload URL, made safe to show.
func fileLabel(raw string) string {
	name := raw
	if i := strings.IndexAny(name, "?#"); i >= 0 {
		name = name[:i]
	}
	return linear.SafeFilename(path.Base(name))
}

// fileReason maps a download error to a fixed text: the raw error can name a
// network address.
func fileReason(e string) string {
	switch {
	case strings.HasPrefix(e, "download failed: status "):
		return "the download failed (" + strings.TrimPrefix(e, "download failed: ") + ")"
	case strings.Contains(e, "limit"):
		return "the file is larger than the limit"
	case strings.Contains(e, "not an uploads.linear.app"):
		return "not a Linear upload address"
	default:
		return "the download failed"
	}
}

func errNoSpace() *httpx.Error {
	return httpx.Errorf(http.StatusInsufficientStorage, httpx.CodeNoSpace,
		"the import work directory has too little free space for an export (or the export hit the download limit)")
}
