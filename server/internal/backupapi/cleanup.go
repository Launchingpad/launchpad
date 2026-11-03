package backupapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/database"
	"devops/internal/dbq"
)

// Row cleanup of the backup tables (LAUN-75, decisions-T16 #29). offsite has no
// DELETE right (#15), so the app does it, hourly.
const (
	// RunRetention: a final backup_runs row is deleted this long after it finished.
	RunRetention = 180 * 24 * time.Hour
	// DumpRetention: a backup_dumps row whose file was removed, or a failed one,
	// is deleted this long after file_removed_at / finished_at.
	DumpRetention = 180 * 24 * time.Hour
	// RequestRetention: a backup_requests row in a final state.
	RequestRetention = 7 * 24 * time.Hour
	// ChangeLogRetention: the audit trail (a guess, #29).
	ChangeLogRetention = 730 * 24 * time.Hour
	// StuckDump: a dump left running longer than this is marked failed.
	StuckDump = 6 * time.Hour
	// StuckRequest: a request left taken longer than this, whose dump is not
	// running, is marked failed.
	StuckRequest = 2 * time.Hour
	// CleanupBatch: the most rows one DELETE removes.
	CleanupBatch = 1000
	// CleanupInterval: the job runs at boot and then this often.
	CleanupInterval = time.Hour
	cleanupPause    = 50 * time.Millisecond
)

const (
	cleanupLockSQL   = `SELECT pg_try_advisory_lock(hashtextextended('devops:backup:cleanup', 0))`
	cleanupUnlockSQL = `SELECT pg_advisory_unlock(hashtextextended('devops:backup:cleanup', 0))`
)

// CleanupResult counts what one pass changed.
type CleanupResult struct {
	Runs, Dumps, Requests, ChangeLog int64
	DumpsFailed, RequestsFailed      int64
	TestSecrets                      int64
	Recipients                       int
}

// total is the number of rows the pass touched.
func (r CleanupResult) total() int64 {
	return r.Runs + r.Dumps + r.Requests + r.ChangeLog + r.DumpsFailed + r.RequestsFailed + r.TestSecrets + int64(r.Recipients)
}

func batched(ctx context.Context, del func(batch int32) (int64, error)) (int64, error) {
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := del(CleanupBatch)
		total += n
		if err != nil || n < CleanupBatch {
			return total, err
		}
		t := time.NewTimer(cleanupPause)
		select {
		case <-ctx.Done():
			t.Stop()
			return total, ctx.Err()
		case <-t.C:
		}
	}
}

// Cleanup is one pass as of now (the service clock, else the database's). It
// takes no lock: RunCleanup does. It deletes only rows past the ages above, ends
// a stuck dump or request, clears old sealed test secrets and drops recipient
// elements whose removal has passed.
func (s *Service) Cleanup(ctx context.Context) (CleanupResult, error) {
	return s.cleanup(ctx, s.pool)
}

func (s *Service) cleanup(ctx context.Context, db dbq.DBTX) (CleanupResult, error) {
	var r CleanupResult
	q := dbq.New(db)
	now, err := s.now(ctx, q)
	if err != nil {
		return r, err
	}
	// 1. End what is stuck first: a dump that ends failed frees its request.
	if r.DumpsFailed, err = q.FailStuckBackupDumps(ctx, &now); err != nil {
		return r, fmt.Errorf("fail stuck dumps: %w", err)
	}
	if r.RequestsFailed, err = q.FailStuckBackupRequests(ctx, &now); err != nil {
		return r, fmt.Errorf("fail stuck requests: %w", err)
	}
	if r.TestSecrets, err = q.SweepBackupTestSecrets(ctx, now); err != nil {
		return r, fmt.Errorf("sweep test secrets: %w", err)
	}
	// 2. Delete what is old.
	if r.Runs, err = batched(ctx, func(b int32) (int64, error) {
		return q.PruneBackupRuns(ctx, dbq.PruneBackupRunsParams{Cutoff: now.Add(-RunRetention), BatchSize: b})
	}); err != nil {
		return r, fmt.Errorf("prune runs: %w", err)
	}
	if r.Dumps, err = batched(ctx, func(b int32) (int64, error) {
		return q.PruneBackupDumps(ctx, dbq.PruneBackupDumpsParams{Cutoff: now.Add(-DumpRetention), BatchSize: b})
	}); err != nil {
		return r, fmt.Errorf("prune dumps: %w", err)
	}
	if r.Requests, err = batched(ctx, func(b int32) (int64, error) {
		return q.PruneBackupRequests(ctx, dbq.PruneBackupRequestsParams{Cutoff: now.Add(-RequestRetention), BatchSize: b})
	}); err != nil {
		return r, fmt.Errorf("prune requests: %w", err)
	}
	if r.ChangeLog, err = batched(ctx, func(b int32) (int64, error) {
		return q.PruneBackupChangeLog(ctx, dbq.PruneBackupChangeLogParams{Cutoff: now.Add(-ChangeLogRetention), BatchSize: b})
	}); err != nil {
		return r, fmt.Errorf("prune the change log: %w", err)
	}
	if r.Recipients, err = s.dropPassedRecipients(ctx, now); err != nil {
		return r, err
	}
	return r, nil
}

// dropPassedRecipients removes the elements of backup_settings.recipients whose
// removal time has passed (#29). They are already inactive; the removal was logged
// when it was scheduled, so this writes no change-log row. It keeps the order and
// the last editor of the row.
func (s *Service) dropPassedRecipients(ctx context.Context, now time.Time) (int, error) {
	dropped := 0
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		row, err := q.GetBackupSettingsForUpdate(ctx)
		if err != nil {
			return fmt.Errorf("lock the backup settings: %w", err)
		}
		var all []storedRecipient
		if err := json.Unmarshal(row.Recipients, &all); err != nil {
			return fmt.Errorf("read the recipients: %w", err)
		}
		keep := make([]storedRecipient, 0, len(all))
		for _, r := range all {
			if r.RemoveAt != nil && !r.RemoveAt.After(now) {
				continue
			}
			keep = append(keep, r)
		}
		if len(keep) == len(all) {
			return nil
		}
		enc, err := json.Marshal(keep)
		if err != nil {
			return fmt.Errorf("encode the recipients: %w", err)
		}
		if _, err := q.UpdateBackupRecipients(ctx, dbq.UpdateBackupRecipientsParams{
			Recipients: enc, EverHadRecipient: row.EverHadRecipient, UpdatedBy: row.UpdatedBy,
		}); err != nil {
			return fmt.Errorf("store the recipients: %w", err)
		}
		dropped = len(all) - len(keep)
		return nil
	})
	return dropped, err
}

// CleanupOnce is one locked pass: a session try advisory lock on its own
// connection, so a second app instance gets locked=false and does nothing.
func (s *Service) CleanupOnce(ctx context.Context) (res CleanupResult, locked bool, err error) {
	return cleanupLocked(ctx, s.pool, func(ctx context.Context) (CleanupResult, error) { return s.Cleanup(ctx) })
}

func cleanupLocked(ctx context.Context, pool *pgxpool.Pool, run func(context.Context) (CleanupResult, error)) (res CleanupResult, locked bool, err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return res, false, fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()
	if err := conn.QueryRow(ctx, cleanupLockSQL).Scan(&locked); err != nil {
		return res, false, fmt.Errorf("lock: %w", err)
	}
	if !locked {
		return res, false, nil
	}
	defer func() {
		// The caller's ctx may be done; the unlock must still run, or the
		// connection is closed (which also frees a session lock).
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		var ok bool
		if uerr := conn.QueryRow(uctx, cleanupUnlockSQL).Scan(&ok); uerr != nil || !ok {
			_ = conn.Conn().Close(uctx)
		}
	}()
	res, err = run(ctx)
	return res, true, err
}

// RunCleanup cleans at boot and then every interval until ctx ends. One failed
// pass is logged and the loop goes on. It logs counts only.
func (s *Service) RunCleanup(ctx context.Context, interval time.Duration, log *slog.Logger) {
	once := func() {
		res, locked, err := s.CleanupOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		switch {
		case err != nil:
			log.ErrorContext(ctx, "backup cleanup failed", "err", err, "rows", res.total())
		case locked && res.total() > 0:
			log.InfoContext(ctx, "backup cleanup", "runs", res.Runs, "dumps", res.Dumps, "requests", res.Requests,
				"change_log", res.ChangeLog, "dumps_failed", res.DumpsFailed, "requests_failed", res.RequestsFailed,
				"test_secrets", res.TestSecrets, "recipients", res.Recipients)
		}
	}
	once()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			once()
		}
	}
}
