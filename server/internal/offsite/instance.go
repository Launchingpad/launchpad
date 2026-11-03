package offsite

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"devops/internal/dbq"
)

// InstanceFile is the name of the instance guard file on the scratch volume
// (decisions-T16 #37). A restore of the database does not bring it.
const InstanceFile = "instance-id"

// checkInstance applies the instance guard (#37) and reports whether backup runs
// may go on. It rereads the file on EVERY scan, not only at boot.
//
//   - file present and equal to backup_settings.instance_id: ok;
//   - file present and different: not ok (a restored or cloned database);
//   - file missing and the database has no succeeded run: a first deploy; the
//     worker writes the file and it is ok;
//   - file missing and the database already has a succeeded run: not ok;
//   - any other read problem: not ok (fail closed).
func (w *Worker) checkInstance(ctx context.Context) bool {
	q := dbq.New(w.pool)
	s, err := q.GetBackupSettings(ctx)
	if err != nil {
		w.log.Error("offsite: cannot read the backup settings", "err", err)
		return false
	}
	path := filepath.Join(w.o.WorkDir, InstanceFile)
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		return strings.TrimSpace(string(b)) == s.InstanceID.String()
	case !errors.Is(err, fs.ErrNotExist):
		w.log.Error("offsite: cannot read the instance file", "err", err)
		return false
	}
	has, err := q.HasSucceededBackupRun(ctx)
	if err != nil {
		w.log.Error("offsite: cannot read the run history", "err", err)
		return false
	}
	if has {
		return false
	}
	if err := WriteInstanceFile(w.o.WorkDir, s.InstanceID); err != nil {
		w.log.Error("offsite: cannot write the instance file", "err", err)
		return false
	}
	w.log.Info("offsite: first run on this volume; instance file written")
	return true
}

// WriteInstanceFile writes <dir>/instance-id atomically (a temp file in the same
// directory, then a rename), mode 0600. `server offsite resume` uses it too.
func WriteInstanceFile(dir string, id uuid.UUID) error {
	tmp, err := os.CreateTemp(dir, ".instance-id-*")
	if err != nil {
		return fmt.Errorf("offsite: write the instance file: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.WriteString(id.String() + "\n"); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("offsite: write the instance file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("offsite: write the instance file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("offsite: write the instance file: %w", err)
	}
	if err := os.Rename(name, filepath.Join(dir, InstanceFile)); err != nil {
		return fmt.Errorf("offsite: write the instance file: %w", err)
	}
	return nil
}
