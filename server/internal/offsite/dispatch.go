package offsite

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/database"
	"devops/internal/dbq"
)

func isBackupTrigger(t string) bool {
	return t == "schedule" || t == "manual" || t == "catchup" || t == "retry"
}

// dispatch claims every queued run that is due and starts a goroutine for it
// (decisions-T16 #31 "Claim order"):
//   - a backup run: pick its destination, try-lock it on a dedicated connection,
//     THEN claim a row filtered by that destination. If the lock is busy nothing
//     changes and the next scan tries again;
//   - test, probe, exchange and verify runs (and every unsaved run) are claimed
//     WITHOUT the lock, so a test is allowed while a backup runs.
//
// instanceOK false leaves the four backup triggers out (#37).
func (w *Worker) dispatch(ctx context.Context, instanceOK bool) {
	q := dbq.New(w.pool)
	skip := []uuid.UUID{}
	for ctx.Err() == nil {
		run, err := q.NextQueuedBackupRun(ctx, dbq.NextQueuedBackupRunParams{Skip: skip, BackupOk: instanceOK})
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		if err != nil {
			w.log.Error("offsite: cannot read the queue", "err", err)
			return
		}
		var started, full bool
		if isBackupTrigger(run.Trigger) {
			started, full = w.startBackup(ctx, run)
		} else {
			started, full = w.startOther(ctx, run)
		}
		if full {
			// No free slot for this class: the run stays queued for the next pass, and
			// the scan goes on, so a probe or test behind a capped backup still starts.
			skip = append(skip, run.ID)
			continue
		}
		if !started {
			skip = append(skip, run.ID)
		}
	}
}

func tryAcquire(sem chan struct{}) bool {
	select {
	case sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func release(sem chan struct{}) { <-sem }

// startBackup claims a queued backup run and starts its goroutine.
func (w *Worker) startBackup(ctx context.Context, run dbq.BackupRun) (started, full bool) {
	if run.DestinationID == nil {
		return false, false // a deleted destination: the orphan sweep cancels it
	}
	dest := *run.DestinationID
	if !tryAcquire(w.backupSem) {
		return false, true
	}
	lock, ok, err := w.tryLock(ctx, dest)
	if err != nil {
		release(w.backupSem)
		w.log.Error("offsite: cannot take the destination lock", "err", err)
		return false, false
	}
	if !ok {
		release(w.backupSem)
		return false, false
	}
	q := dbq.New(w.pool)
	params := dbq.ClaimBackupRunParams{ID: run.ID, DestinationID: &dest, NewStatus: "running"}
	var dump *dbq.BackupDump
	if run.RequestID == nil {
		dump, err = w.newestFresh(ctx, q)
		if err == nil && dump == nil {
			var rid uuid.UUID
			if rid, err = w.ensureRequest(ctx); err == nil {
				params.RequestID = &rid
				params.NewStatus = "waiting_dump"
			}
		}
	} else {
		var dec dumpDecision
		if dec, err = w.evaluate(ctx, run); err == nil {
			dump = dec.dump
			if dump == nil {
				params.NewStatus = "waiting_dump"
			}
		}
	}
	if err != nil {
		lock.release()
		release(w.backupSem)
		w.log.Error("offsite: cannot decide the dump of a run", "err", err)
		return false, false
	}
	if dump != nil {
		params.DumpID = &dump.ID
	}
	claimed, err := q.ClaimBackupRun(ctx, params)
	if err != nil {
		lock.release()
		release(w.backupSem)
		if !errors.Is(err, pgx.ErrNoRows) {
			w.log.Error("offsite: cannot claim a run", "err", err)
		}
		return false, false
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.execBackup(ctx, claimed, lock)
	}()
	return true, false
}

// startOther claims a test, probe, exchange or verify run.
func (w *Worker) startOther(ctx context.Context, run dbq.BackupRun) (started, full bool) {
	sem := w.otherSem
	if run.Trigger == "verify" {
		sem = w.backupSem
	}
	if !tryAcquire(sem) {
		return false, true
	}
	claimed, err := dbq.New(w.pool).ClaimUnlockedBackupRun(ctx, run.ID)
	if err != nil {
		release(sem)
		if !errors.Is(err, pgx.ErrNoRows) {
			w.log.Error("offsite: cannot claim a run", "err", err)
		}
		return false, false
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer release(sem)
		w.execOther(ctx, claimed)
	}()
	return true, false
}

// runScope is the context and bookkeeping of one run goroutine.
type runScope struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	stop   func()
}

// begin gives a run its cancellable context, registers it for the scan's cancel
// and starts the lease heartbeat (every LeaseEvery; 0 rows renewed means the run
// was cancelled or interrupted, and the goroutine aborts).
func (w *Worker) begin(parent context.Context, id uuid.UUID) *runScope {
	ctx, cancel := context.WithCancelCause(parent)
	w.register(id, cancel)
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(w.o.LeaseEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				n, err := dbq.New(w.pool).RenewBackupRunLease(ctx, id)
				if err == nil && n == 0 {
					cancel(errLost)
					return
				}
			}
		}
	}()
	return &runScope{ctx: ctx, cancel: cancel, stop: func() {
		close(done)
		cancel(nil)
		w.unregister(id)
	}}
}

// finish writes the end of a run. A run whose context was cancelled because the
// row changed under it (errLost), or because the process is stopping, writes
// nothing: the row is already final, or the lease will make it interrupted. A
// retryable failure of a schedule, catchup or manual run also queues ONE retry
// row 30 minutes ahead, in the same transaction (#31 "Retry").
func (w *Worker) finish(parent context.Context, sc *runScope, run dbq.BackupRun, res result, ferr error) {
	if parent.Err() != nil || errors.Is(context.Cause(sc.ctx), errLost) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 30*time.Second)
	defer cancel()
	p := dbq.FinishBackupRunParams{ID: run.ID, Status: "succeeded", Bytes: res.bytes, Sha256: res.sha, RemoteName: res.remote, Steps: res.steps}
	var f failure
	if ferr == nil {
		p.ErrorText = res.warn
	} else {
		f = classify(sc.ctx, ferr)
		p.Status, p.ErrorCode, p.ErrorText = "failed", string(f.code), f.text
		p.WillRetry = (run.Trigger == "schedule" || run.Trigger == "catchup" || run.Trigger == "manual") && f.retryable()
		if f.internalErr != nil {
			w.log.Error("offsite: a run failed on an internal error", "run", run.ID.String(), "err", f.internalErr)
		} else {
			w.log.Warn("offsite: a run failed", "run", run.ID.String(), "trigger", run.Trigger, "code", p.ErrorCode, "will_retry", p.WillRetry)
		}
	}
	err := database.InTx(ctx, w.pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		row, err := q.FinishBackupRun(ctx, p)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // cancelled or interrupted meanwhile: not overwritten
		}
		if err != nil {
			return err
		}
		if row.WillRetry && row.RetryAt != nil && run.DestinationID != nil {
			id, err := uuid.NewV7()
			if err != nil {
				return err
			}
			if _, err := q.InsertBackupRetryRun(ctx, dbq.InsertBackupRetryRunParams{
				ID: id, Attempt: run.Attempt + 1, RetryAt: *row.RetryAt, DestinationID: *run.DestinationID,
			}); err != nil {
				return err
			}
			return q.NotifyBackupWork(ctx)
		}
		return nil
	})
	if err != nil {
		w.log.Error("offsite: cannot write the end of a run", "run", run.ID.String(), "err", err)
	}
}

// result is what a run reports on success.
type result struct {
	bytes  *int64
	sha    *string
	remote *string
	steps  []byte
	warn   string // a fixed text, e.g. prune_failed
}
