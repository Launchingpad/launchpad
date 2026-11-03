// Package notify holds the notifications code (LAUN-23): the consumer, the
// sender, the inbox API support and the retention job.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"devops/internal/dbq"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Retention periods (docs/internal/decisions-T13-notifications.md #10).
const (
	// ReadRetention: a read or archived notification is deleted this long
	// after created_at.
	ReadRetention = 90 * 24 * time.Hour
	// AllRetention: every notification is deleted this long after created_at.
	AllRetention = 180 * 24 * time.Hour
	// SentEmailRetention: outbox rows in state sent or skipped.
	SentEmailRetention = 30 * 24 * time.Hour
	// DeadEmailRetention: outbox rows in state dead, kept for the operator.
	DeadEmailRetention = 90 * 24 * time.Hour
	// TokenRetention: a mail token is deleted this long after expires_at.
	TokenRetention = 7 * 24 * time.Hour
	// RetentionBatch: the most rows one DELETE removes, so no statement holds
	// a long lock.
	RetentionBatch = 1000
	// RetentionInterval: the job runs at boot and then this often.
	RetentionInterval = time.Hour
	// RetentionPause: the wait between two full batches, so a large first
	// backlog does not make a burst of WAL.
	RetentionPause = 50 * time.Millisecond
)

// The job holds a session advisory lock (a try lock; the key is a hash of a fixed name).
const retentionLockSQL = `SELECT pg_try_advisory_lock(hashtextextended('devops:notify:retention', 0))`
const retentionUnlockSQL = `SELECT pg_advisory_unlock(hashtextextended('devops:notify:retention', 0))`

// PruneResult counts the rows one Prune call deleted.
type PruneResult struct {
	Notifications int64
	Emails        int64
	Tokens        int64
}

// sleepCtx waits d, or returns the context's error when ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// batches runs del until a statement removes fewer than RetentionBatch rows
// and returns the total. It calls pause between two full batches. A
// cancelled ctx stops it between statements.
func batches(ctx context.Context, pause func(context.Context) error, del func() (int64, error)) (int64, error) {
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, err := del()
		total += n
		if err != nil {
			return total, err
		}
		if n < RetentionBatch {
			return total, nil
		}
		if err := pause(ctx); err != nil {
			return total, err
		}
	}
}

// Prune deletes what retention allows as of now, in batches of at most
// RetentionBatch rows per statement. The three kinds are independent. It
// takes no lock: RunRetention does (use PruneOnce for one locked run).
func Prune(ctx context.Context, q *dbq.Queries, now time.Time) (PruneResult, error) {
	return prune(ctx, q, now, func(ctx context.Context) error { return sleepCtx(ctx, RetentionPause) })
}

// prune is Prune with the pause between full batches injected (tests).
func prune(ctx context.Context, q *dbq.Queries, now time.Time, pause func(context.Context) error) (PruneResult, error) {
	var r PruneResult
	var err error
	if r.Notifications, err = batches(ctx, pause, func() (int64, error) {
		return q.PruneNotifications(ctx, dbq.PruneNotificationsParams{
			ReadCutoff: now.Add(-ReadRetention), AllCutoff: now.Add(-AllRetention), BatchSize: RetentionBatch,
		})
	}); err != nil {
		return r, fmt.Errorf("prune notifications: %w", err)
	}
	if r.Emails, err = batches(ctx, pause, func() (int64, error) {
		return q.PruneNotificationEmails(ctx, dbq.PruneNotificationEmailsParams{
			SentCutoff: now.Add(-SentEmailRetention), DeadCutoff: now.Add(-DeadEmailRetention), BatchSize: RetentionBatch,
		})
	}); err != nil {
		return r, fmt.Errorf("prune notification emails: %w", err)
	}
	if r.Tokens, err = batches(ctx, pause, func() (int64, error) {
		return q.PruneNotificationMailTokens(ctx, dbq.PruneNotificationMailTokensParams{
			Cutoff: now.Add(-TokenRetention), BatchSize: RetentionBatch,
		})
	}); err != nil {
		return r, fmt.Errorf("prune notification mail tokens: %w", err)
	}
	return r, nil
}

// PruneOnce is one locked run: it takes a session try advisory lock on its
// own connection, so a second instance gets locked=false and does nothing.
func PruneOnce(ctx context.Context, pool *pgxpool.Pool, now time.Time) (res PruneResult, locked bool, err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return res, false, fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()
	if err := conn.QueryRow(ctx, retentionLockSQL).Scan(&locked); err != nil {
		return res, false, fmt.Errorf("lock: %w", err)
	}
	if !locked {
		return res, false, nil
	}
	defer func() {
		// The caller's ctx may be done; the unlock must still run, or the
		// connection is destroyed (which also frees a session lock).
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		var ok bool
		if uerr := conn.QueryRow(uctx, retentionUnlockSQL).Scan(&ok); uerr != nil || !ok {
			_ = conn.Conn().Close(uctx)
		}
	}()
	res, err = Prune(ctx, dbq.New(conn), now)
	return res, true, err
}

// RunRetention prunes once at boot and then every interval until ctx ends.
// One failed run is logged and the loop goes on. It logs counts only.
func RunRetention(ctx context.Context, pool *pgxpool.Pool, clock func() time.Time, interval time.Duration, log *slog.Logger) {
	once := func() {
		res, locked, err := PruneOnce(ctx, pool, clock())
		if ctx.Err() != nil {
			return
		}
		switch {
		case err != nil:
			log.ErrorContext(ctx, "notifications retention failed", "err", err,
				"notifications", res.Notifications, "emails", res.Emails, "tokens", res.Tokens)
		case locked && (res.Notifications+res.Emails+res.Tokens) > 0:
			log.InfoContext(ctx, "notifications retention", "notifications", res.Notifications,
				"emails", res.Emails, "tokens", res.Tokens)
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
