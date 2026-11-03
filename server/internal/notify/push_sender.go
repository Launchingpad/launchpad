package notify

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/platform"
)

// Push sender constants (decisions-T35). Constants in v1, not settings.
const (
	// PushSendInterval is the ticker of Run.
	PushSendInterval = 15 * time.Second
	// PushRowTTL: a push that waited this long is skipped (expired).
	PushRowTTL = 24 * time.Hour
	// PushRateCap is the most pushes one org sends per PushRateWindow.
	PushRateCap = 300
	// PushRateWindow is the rolling window of PushRateCap.
	PushRateWindow = time.Hour
	// PushPerOrgPerPass is the most pushes one pass sends for one org.
	PushPerOrgPerPass = 20
	// PushDueLimit bounds the rows one pass reads.
	PushDueLimit = 200
	// PushRetention: terminal rows are deleted this long after creation.
	PushRetention = 30 * 24 * time.Hour
	// pushPruneBatch is the most rows one DELETE removes.
	pushPruneBatch = 1000
)

// PushSender sends the push_outbox rows the consumer made. Like the mail and
// Slack senders it is called by no write path; a relay fault cannot fail a write.
type PushSender struct {
	pool   *pgxpool.Pool
	q      *dbq.Queries
	log    *slog.Logger
	keys   *platform.PushKeyring
	poster *PushPoster
	now    func() time.Time
}

// NewPushSender returns a sender. keys may be nil (rows are then skipped as
// unreadable).
func NewPushSender(pool *pgxpool.Pool, log *slog.Logger, keys *platform.PushKeyring, poster *PushPoster) *PushSender {
	return &PushSender{pool: pool, q: dbq.New(pool), log: log, keys: keys, poster: poster, now: time.Now}
}

// SetClock sets the clock of the expiry, the rate window and the retries (tests).
func (s *PushSender) SetClock(now func() time.Time) { s.now = now }

// PushResult counts what one pass did.
type PushResult struct {
	Locked    bool
	Recovered int
	Expired   int
	Sent      int
	Skipped   int
	Retried   int
	Dead      int
	Invalid   int // devices deleted because the relay said the token is invalid
	Pruned    int
	PrunedDev int // devices deleted by the 90-day retention
}

// Run sends at boot and every interval until ctx ends. A panic is recovered
// per pass. It logs counts and fixed words only.
func (s *PushSender) Run(ctx context.Context, interval time.Duration) {
	once := func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.ErrorContext(ctx, "push sender panicked", "panic", fmt.Sprint(r))
			}
		}()
		res, err := s.RunOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.log.ErrorContext(ctx, "push sender failed; retrying on the next tick", "err", err)
		}
		attrs := []any{"sent", res.Sent, "skipped", res.Skipped, "retry", res.Retried, "dead", res.Dead,
			"invalid", res.Invalid, "expired", res.Expired, "recovered", res.Recovered}
		switch {
		case res.Retried+res.Dead > 0:
			s.log.WarnContext(ctx, "push sender: some pushes failed", attrs...)
		case res.Sent+res.Skipped+res.Expired+res.Recovered+res.Invalid > 0:
			s.log.InfoContext(ctx, "push sender", attrs...)
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

// RunOnce is one pass: take the sender lock, return stuck rows to pending,
// expire old rows, prune old terminal rows and stale devices, claim the due
// rows and send them.
func (s *PushSender) RunOnce(ctx context.Context) (PushResult, error) {
	var res PushResult
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return res, fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()
	lockq := dbq.New(conn)
	locked, err := lockq.TryLockPushSender(ctx)
	if err != nil {
		return res, fmt.Errorf("lock: %w", err)
	}
	if !locked {
		return res, nil
	}
	res.Locked = true
	defer func() {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if ok, err := lockq.UnlockPushSender(uctx); err != nil || !ok {
			_ = conn.Hijack().Close(uctx)
		}
	}()

	now := s.now()
	n, err := s.q.RecoverStuckPushOutbox(ctx, now.Add(-StuckAfter))
	if err != nil {
		return res, fmt.Errorf("recover: %w", err)
	}
	res.Recovered = int(n)
	if n, err = s.q.ExpirePushOutbox(ctx, dbq.ExpirePushOutboxParams{Now: now, Cutoff: now.Add(-PushRowTTL)}); err != nil {
		return res, fmt.Errorf("expire: %w", err)
	}
	res.Expired = int(n)
	if n, err = s.q.PrunePushOutbox(ctx, dbq.PrunePushOutboxParams{Cutoff: now.Add(-PushRetention), BatchSize: pushPruneBatch}); err != nil {
		return res, fmt.Errorf("prune: %w", err)
	}
	res.Pruned = int(n)
	if n, err = s.q.PruneStalePushDevices(ctx, dbq.PruneStalePushDevicesParams{Cutoff: now.Add(-PushDeviceRetention), BatchSize: pushPruneBatch}); err != nil {
		return res, fmt.Errorf("prune devices: %w", err)
	}
	res.PrunedDev = int(n)

	rows, err := s.claim(ctx, now)
	if err != nil {
		return res, fmt.Errorf("claim: %w", err)
	}
	var errs []error
	for _, r := range rows {
		if ctx.Err() != nil {
			break
		}
		if err := s.sendRow(ctx, r, &res); err != nil {
			errs = append(errs, err)
		}
	}
	return res, errors.Join(errs...)
}

// claim marks up to PushPerOrgPerPass due rows of each org as sending, in one
// transaction. The rest wait for the next pass.
func (s *PushSender) claim(ctx context.Context, now time.Time) ([]dbq.ListDuePushOutboxRow, error) {
	var out []dbq.ListDuePushOutboxRow
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		out = nil
		q := s.q.WithTx(tx)
		rows, err := q.ListDuePushOutbox(ctx, dbq.ListDuePushOutboxParams{Now: now, PerOrg: PushPerOrgPerPass, MaxRows: PushDueLimit})
		if err != nil {
			return err
		}
		perOrg := map[uuid.UUID]int{}
		for _, r := range rows {
			if perOrg[r.OrgID] >= PushPerOrgPerPass {
				continue
			}
			n, err := q.ClaimPushOutbox(ctx, dbq.ClaimPushOutboxParams{OrgID: r.OrgID, ID: r.ID, Now: now})
			if err != nil {
				return err
			}
			if n == 0 {
				continue // the row left pending since the list: do not send it
			}
			perOrg[r.OrgID]++
			out = append(out, r)
		}
		return nil
	})
	return out, err
}

// sendRow decides and sends one row, then records the result. A database
// error leaves the row in sending (it returns to pending after StuckAfter):
// at-least-once.
func (s *PushSender) sendRow(ctx context.Context, r dbq.ListDuePushOutboxRow, res *PushResult) error {
	rctx, cancel := recordCtx(ctx)
	defer cancel()
	now := s.now()
	skip := func(reason string) error {
		n, err := s.q.SkipPushOutbox(rctx, dbq.SkipPushOutboxParams{OrgID: r.OrgID, ID: r.ID, Reason: &reason, Now: now})
		res.Skipped += int(n)
		return err
	}

	relay, err := s.q.GetPushRelay(ctx, r.OrgID)
	if errors.Is(err, pgx.ErrNoRows) {
		return skip("disabled") // the admin removed the relay
	}
	if err != nil {
		return fmt.Errorf("relay: %w", err)
	}
	info, err := s.q.GetPushSendInfo(ctx, dbq.GetPushSendInfoParams{OrgID: r.OrgID, ID: r.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return skip("gone") // the device or the notification is gone
	}
	if err != nil {
		return fmt.Errorf("send info: %w", err)
	}
	if info.Inbox && (info.ReadAt != nil || info.ArchivedAt != nil) {
		return skip("read") // the person already saw it
	}
	var relayURL, secret, token string
	if s.keys != nil {
		u, e1 := s.keys.OpenFor(platform.PushColRelayURL, r.OrgID.String(), relay.UrlEnc)
		sec, e2 := s.keys.OpenFor(platform.PushColRelaySecret, r.OrgID.String(), relay.SecretEnc)
		tok, e3 := s.keys.OpenFor(platform.PushColDeviceToken, deviceScope(r.OrgID, r.DeviceID), info.TokenEnc)
		err = errors.Join(e1, e2, e3)
		relayURL, secret, token = string(u), string(sec), string(tok)
	}
	if s.keys == nil || err != nil {
		if rerr := recordPushResult(rctx, s.q, r.OrgID, now, PushUnreadable); rerr != nil {
			return fmt.Errorf("record result: %w", rerr)
		}
		return skip("unreadable")
	}

	since := now.Add(-PushRateWindow)
	sent, err := s.q.CountRecentPushes(ctx, dbq.CountRecentPushesParams{OrgID: r.OrgID, Since: &since})
	if err != nil {
		return fmt.Errorf("rate count: %w", err)
	}
	if sent >= PushRateCap {
		return skip("rate_cap")
	}

	perr := s.poster.Wake(ctx, relayURL, secret, PushWake{Token: token, Platform: info.Platform, ID: r.ID.String()})
	if perr == nil {
		at := s.now()
		if _, err := s.q.MarkPushOutboxSent(rctx, dbq.MarkPushOutboxSentParams{OrgID: r.OrgID, ID: r.ID, Now: &at}); err != nil {
			return fmt.Errorf("record sent: %w", err)
		}
		res.Sent++
		if err := recordPushResult(rctx, s.q, r.OrgID, at, ""); err != nil {
			return fmt.Errorf("record result: %w", err)
		}
		return nil
	}
	pe := &PushError{Word: PushUnreachable}
	errors.As(perr, &pe)
	if pe.Word == PushTokenInvalid {
		// The relay says the token is dead (the app was removed or the token
		// rotated): delete the device. Its outbox rows go with it. The relay
		// itself answered, so this is not a relay fault.
		sum := sha256.Sum256([]byte(token))
		n, err := s.q.DeletePushDeviceIfToken(rctx, dbq.DeletePushDeviceIfTokenParams{OrgID: r.OrgID, ID: info.DeviceID, TokenHash: sum[:]})
		if err != nil {
			return fmt.Errorf("delete invalid device: %w", err)
		}
		if n == 0 {
			// The app refreshed the token while this wake was in flight: the device stays, and this wake is dropped.
			return skip("gone")
		}
		res.Invalid++
		return nil
	}
	attempts := r.Attempts + 1
	state, next := "pending", now.Add(RetryDelays[min(int(attempts), len(RetryDelays))-1])
	if pe.RetryAfter > 0 {
		next = now.Add(max(pe.RetryAfter, next.Sub(now)))
	}
	if int(attempts) >= MaxAttempts || pe.permanent() {
		state, next = "dead", now
		res.Dead++
	} else {
		res.Retried++
	}
	if _, err := s.q.FailPushOutbox(rctx, dbq.FailPushOutboxParams{
		OrgID: r.OrgID, ID: r.ID, Attempts: attempts, State: state, NextAttemptAt: next, LastError: &pe.Word,
	}); err != nil {
		return fmt.Errorf("record failure: %w", err)
	}
	if err := recordPushResult(rctx, s.q, r.OrgID, now, pe.Word); err != nil {
		return fmt.Errorf("record result: %w", err)
	}
	return nil
}
