package offsite

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/database"
	"devops/internal/dbq"
)

// schedule makes the runs that are due (decisions-T16 #31 "Missed runs"). For each
// enabled destination: if the last passed slot has no run, make ONE run. Only slots
// after max(created_at, enabled_since) count, so a new or re-enabled destination makes
// no run for a slot that passed before it existed or was enabled. The slot time and
// "now" are the DATABASE's.
func (w *Worker) schedule(ctx context.Context) {
	q := dbq.New(w.pool)
	now, err := q.BackupDBNow(ctx)
	if err != nil {
		w.log.Error("offsite: cannot read the database clock", "err", err)
		return
	}
	dests, err := q.ListEnabledBackupDestinations(ctx)
	if err != nil {
		w.log.Error("offsite: cannot list the destinations", "err", err)
		return
	}
	for _, d := range dests {
		var wd *int
		if d.Weekday != nil {
			v := int(*d.Weekday)
			wd = &v
		}
		slot, err := LastSlot(now, d.Schedule, d.AtUtc, wd)
		if err != nil {
			w.log.Error("offsite: bad schedule", "destination", d.ID.String())
			continue
		}
		floor := d.CreatedAt
		if d.EnabledSince != nil && d.EnabledSince.After(floor) {
			floor = *d.EnabledSince
		}
		if !slot.After(floor) {
			continue
		}
		did := d.ID
		has, err := q.BackupSlotHasRun(ctx, dbq.BackupSlotHasRunParams{DestinationID: &did, Slot: slot})
		if err != nil {
			w.log.Error("offsite: cannot read the runs of a slot", "err", err)
			continue
		}
		if has {
			continue
		}
		trigger := "catchup"
		if now.Sub(slot) < scheduleGrace {
			trigger = "schedule"
		}
		if err := w.insertRun(ctx, d.ID, trigger); err != nil {
			w.log.Error("offsite: cannot queue a scheduled run", "err", err)
		}
	}
}

// insertRun inserts a scheduled backup run (ON CONFLICT DO NOTHING against the
// one-run index) and wakes the workers.
func (w *Worker) insertRun(ctx context.Context, dest uuid.UUID, trigger string) error {
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	return database.InTx(ctx, w.pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		n, err := q.InsertScheduledBackupRun(ctx, dbq.InsertScheduledBackupRunParams{ID: id, Trigger: trigger, DestinationID: dest})
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		return q.NotifyBackupWork(ctx)
	})
}

// scheduleVerifies queues a verify run for each destination whose interval passed (#42).
func (w *Worker) scheduleVerifies(ctx context.Context) {
	q := dbq.New(w.pool)
	ids, err := q.ListBackupVerifyDue(ctx)
	if err != nil {
		w.log.Error("offsite: cannot list the verifies that are due", "err", err)
		return
	}
	for _, d := range ids {
		id, err := uuid.NewV7()
		if err != nil {
			continue
		}
		did := d
		err = database.InTx(ctx, w.pool, func(tx pgx.Tx) error {
			tq := dbq.New(tx)
			if _, err := tq.InsertBackupRun(ctx, dbq.InsertBackupRunParams{
				ID: id, DestinationID: &did, Trigger: "verify", Status: "queued", Attempt: 1,
			}); err != nil {
				return err
			}
			return tq.NotifyBackupWork(ctx)
		})
		if err != nil {
			w.log.Error("offsite: cannot queue a verify run", "err", err)
		}
	}
}
