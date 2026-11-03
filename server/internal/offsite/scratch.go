package offsite

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"syscall"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/dbq"
)

// scratchRe matches the scratch file of a run: run-<run id>.age.
var scratchRe = regexp.MustCompile(`^run-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.age$`)

func (w *Worker) scratchPath(runID uuid.UUID) string {
	return filepath.Join(w.o.WorkDir, "run-"+runID.String()+".age")
}

// cleanScratch removes scratch files left by a crash (#18): any run-<id>.age whose
// run is not running or waiting. The instance file is never touched.
func (w *Worker) cleanScratch(ctx context.Context) {
	des, err := os.ReadDir(w.o.WorkDir)
	if err != nil {
		w.log.Error("offsite: cannot read the scratch directory", "err", err)
		return
	}
	q := dbq.New(w.pool)
	for _, de := range des {
		m := scratchRe.FindStringSubmatch(de.Name())
		if m == nil || de.IsDir() {
			continue
		}
		id, err := uuid.Parse(m[1])
		if err != nil {
			continue
		}
		run, err := q.GetBackupRun(ctx, id)
		if err == nil && (run.Status == "running" || run.Status == "waiting_dump") {
			continue // another worker's live run
		}
		if err != nil && err != pgx.ErrNoRows {
			continue
		}
		_ = os.Remove(filepath.Join(w.o.WorkDir, de.Name()))
	}
}

// freeBytes is the free space a non-root writer has on the file system of dir.
func freeBytes(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil //nolint:unconvert // the field types differ per OS
}
