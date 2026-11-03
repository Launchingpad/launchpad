package offsite

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/backup/driver"
	"devops/internal/backup/remotename"
	"devops/internal/database"
	"devops/internal/dbq"
)

// dumpNameRe is the only file name offsite opens in the backups volume (#28).
var dumpNameRe = regexp.MustCompile(`^devops-(\d{8}T\d{6}Z)\.dump$`)

// dumpFile checks a dump row against the volume and returns its path and size. The
// name must match the pattern, and the file must be a regular file (not a link).
func (w *Worker) dumpFile(d dbq.BackupDump) (string, int64, bool) {
	if !dumpNameRe.MatchString(d.FileName) {
		return "", 0, false
	}
	p := filepath.Join(w.o.DumpDir, d.FileName)
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return "", 0, false
	}
	return p, fi.Size(), true
}

// usable: an ok dump, not removed, with a hash and a size, whose file is there.
func (w *Worker) usable(d dbq.BackupDump) bool {
	if d.Status != "ok" || d.FileRemovedAt != nil || d.Sha256 == nil || d.Bytes == nil {
		return false
	}
	_, _, ok := w.dumpFile(d)
	return ok
}

// dumpDecision is where a backup run stands with its dump.
type dumpDecision struct {
	dump *dbq.BackupDump
	wait bool
	fail *driver.Error
}

// newestFresh is the dump a run WITHOUT a request uses: the newest usable one,
// when it started at most 6 hours ago (database clock). Nil: ask for an extra dump (#28).
func (w *Worker) newestFresh(ctx context.Context, q *dbq.Queries) (*dbq.BackupDump, error) {
	now, err := q.BackupDBNow(ctx)
	if err != nil {
		return nil, err
	}
	dumps, err := q.ListUsableBackupDumps(ctx)
	if err != nil {
		return nil, err
	}
	for i := range dumps {
		if w.usable(dumps[i]) {
			if now.Sub(dumps[i].StartedAt) <= dumpMaxAge {
				return &dumps[i], nil
			}
			return nil, nil // the newest usable one is too old
		}
	}
	return nil, nil
}

// ensureRequest returns an open dump request, making one when none is queued or
// taken: two destinations that both need an extra dump share ONE request (#28).
func (w *Worker) ensureRequest(ctx context.Context) (uuid.UUID, error) {
	var id uuid.UUID
	err := database.InTx(ctx, w.pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if err := q.LockBackupRequests(ctx); err != nil {
			return err
		}
		open, err := q.GetOpenBackupRequest(ctx)
		if err == nil {
			id = open.ID
			return nil
		}
		if err != pgx.ErrNoRows {
			return err
		}
		r, err := q.InsertBackupRequest(ctx, dbq.InsertBackupRequestParams{RequestedBy: "offsite"})
		if err != nil {
			return err
		}
		id = r.ID
		return q.NotifyBackupWork(ctx)
	})
	return id, err
}

// evaluate decides, for a run that names a request, whether a dump is ready, the
// run still waits, or it fails dump_unavailable (decisions-T16 #23, #28):
//   - ready: an ok, usable dump that STARTED at or after the request's time;
//   - wait: a dump that started after the request is still running (and for less
//     than 6 hours), or none has started yet and 30 minutes have not passed;
//   - fail: no dump started within 30 minutes of the request; a started dump is
//     still running after 6 hours; or the dumps that started after the request have
//     all ended failed (or are ok but unusable) and none is running or ok.
func (w *Worker) evaluate(ctx context.Context, run dbq.BackupRun) (dumpDecision, error) {
	q := dbq.New(w.pool)
	if run.RequestID == nil {
		return dumpDecision{fail: driver.Errf(driver.DumpUnavailable)}, nil
	}
	req, err := q.GetBackupRequest(ctx, *run.RequestID)
	if err == pgx.ErrNoRows {
		return dumpDecision{fail: driver.Errf(driver.DumpUnavailable)}, nil
	}
	if err != nil {
		return dumpDecision{}, err
	}
	now, err := q.BackupDBNow(ctx)
	if err != nil {
		return dumpDecision{}, err
	}
	dumps, err := q.ListBackupDumpsSince(ctx, req.RequestedAt)
	if err != nil {
		return dumpDecision{}, err
	}
	running := false
	for i := len(dumps) - 1; i >= 0; i-- { // newest first
		d := dumps[i]
		switch {
		case d.Status == "ok" && w.usable(d):
			return dumpDecision{dump: &dumps[i]}, nil
		case d.Status == "running" && now.Sub(d.StartedAt) <= dumpRunningLimit:
			running = true
		}
	}
	switch {
	case running:
		return dumpDecision{wait: true}, nil
	case len(dumps) == 0 && now.Sub(req.RequestedAt) <= dumpStartDelay:
		return dumpDecision{wait: true}, nil
	}
	return dumpDecision{fail: driver.Errf(driver.DumpUnavailable)}, nil
}

// dumpTime reads the UTC time out of a dump's file name (devops-<time>.dump).
func dumpTime(name string) (time.Time, bool) {
	m := dumpNameRe.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.Parse(remotename.TimeLayout, m[1])
	return t, err == nil
}
