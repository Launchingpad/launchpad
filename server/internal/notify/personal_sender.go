package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/platform"
)

// PersonalWebhookSender sends the personal_webhook_outbox rows the consumer
// made (LAUN-229, decisions-T40 #13 to #18). It has the policy of the chat
// sender (the same constants, the same poster and guarded client) on its own
// table and lock. No write path calls it.
type PersonalWebhookSender struct {
	pool    *pgxpool.Pool
	q       *dbq.Queries
	log     *slog.Logger
	keys    *platform.SlackKeyring
	poster  *ChatPoster
	baseURL string
	now     func() time.Time
	budget  time.Duration
}

// NewPersonalWebhookSender returns a sender. keys may be nil (rows are then
// skipped as unreadable). publicBaseURL empty means no post.
func NewPersonalWebhookSender(pool *pgxpool.Pool, log *slog.Logger, keys *platform.SlackKeyring, poster *ChatPoster, publicBaseURL string) *PersonalWebhookSender {
	return &PersonalWebhookSender{pool: pool, q: dbq.New(pool), log: log, keys: keys, poster: poster,
		baseURL: trimBase(publicBaseURL), now: time.Now, budget: ChatPassBudget}
}

// SetClock sets the clock (tests).
func (s *PersonalWebhookSender) SetClock(now func() time.Time) { s.now = now }

// SetBudget sets the wall-clock budget of one pass (tests).
func (s *PersonalWebhookSender) SetBudget(d time.Duration) { s.budget = d }

// Run sends at boot and every interval until ctx ends. It logs counts only.
func (s *PersonalWebhookSender) Run(ctx context.Context, interval time.Duration) {
	once := func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.ErrorContext(ctx, "personal webhook sender panicked", "panic", fmt.Sprint(r))
			}
		}()
		res, err := s.RunOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.log.ErrorContext(ctx, "personal webhook sender failed; retrying on the next tick", "err", err)
		}
		attrs := []any{"sent", res.Sent, "skipped", res.Skipped, "retry", res.Retried, "dead", res.Dead,
			"expired", res.Expired, "recovered", res.Recovered}
		switch {
		case res.Retried+res.Dead > 0:
			s.log.WarnContext(ctx, "personal webhook sender: some posts failed", attrs...)
		case res.Sent+res.Skipped+res.Expired+res.Recovered > 0:
			s.log.InfoContext(ctx, "personal webhook sender", attrs...)
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

// RunOnce is one pass: lock, recover, expire, prune, claim, post.
func (s *PersonalWebhookSender) RunOnce(ctx context.Context) (ChatResult, error) {
	var res ChatResult
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return res, fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()
	lockq := dbq.New(conn)
	locked, err := lockq.TryLockPersonalWebhookSender(ctx)
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
		if ok, err := lockq.UnlockPersonalWebhookSender(uctx); err != nil || !ok {
			_ = conn.Hijack().Close(uctx)
		}
	}()

	now := s.now()
	n, err := s.q.RecoverStuckPersonalWebhookOutbox(ctx, now.Add(-StuckAfter))
	if err != nil {
		return res, fmt.Errorf("recover: %w", err)
	}
	res.Recovered = int(n)
	if n, err = s.q.ExpirePersonalWebhookOutbox(ctx, dbq.ExpirePersonalWebhookOutboxParams{Now: now, Cutoff: now.Add(-ChatRowTTL)}); err != nil {
		return res, fmt.Errorf("expire: %w", err)
	}
	res.Expired = int(n)
	if n, err = s.q.PrunePersonalWebhookOutbox(ctx, dbq.PrunePersonalWebhookOutboxParams{Cutoff: now.Add(-ChatRetention), BatchSize: chatPruneBatch}); err != nil {
		return res, fmt.Errorf("prune: %w", err)
	}
	res.Pruned = int(n)

	rows, err := s.claim(ctx, now)
	if err != nil {
		return res, fmt.Errorf("claim: %w", err)
	}
	var errs []error
	deadline := time.Now().Add(s.budget)
	down := map[uuid.UUID]bool{} // webhooks that were unreachable in this pass
	for _, r := range rows {
		if ctx.Err() != nil {
			break
		}
		if down[r.WebhookID] || !time.Now().Before(deadline) {
			rctx, cancel := recordCtx(ctx)
			n, err := s.q.ReleasePersonalWebhookOutbox(rctx, dbq.ReleasePersonalWebhookOutboxParams{OrgID: r.OrgID, ID: r.ID})
			cancel()
			res.Released += int(n)
			if err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if err := s.sendRow(ctx, r, &res, down); err != nil {
			errs = append(errs, err)
		}
	}
	return res, errors.Join(errs...)
}

func (s *PersonalWebhookSender) claim(ctx context.Context, now time.Time) ([]dbq.ListDuePersonalWebhookOutboxRow, error) {
	var out []dbq.ListDuePersonalWebhookOutboxRow
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		out = nil
		q := s.q.WithTx(tx)
		rows, err := q.ListDuePersonalWebhookOutbox(ctx, dbq.ListDuePersonalWebhookOutboxParams{Now: now, PerOrg: ChatPerOrgPerPass, MaxRows: ChatDueLimit})
		if err != nil {
			return err
		}
		perOrg := map[uuid.UUID]int{}
		for _, r := range rows {
			if perOrg[r.OrgID] >= ChatPerOrgPerPass {
				continue
			}
			n, err := q.ClaimPersonalWebhookOutbox(ctx, dbq.ClaimPersonalWebhookOutboxParams{OrgID: r.OrgID, ID: r.ID, Now: now})
			if err != nil {
				return err
			}
			if n == 0 {
				continue
			}
			perOrg[r.OrgID]++
			out = append(out, r)
		}
		return nil
	})
	return out, err
}

// sendRow re-checks, builds and posts one row (decisions-T40 #13), then
// records the result. A database error leaves the row in sending.
func (s *PersonalWebhookSender) sendRow(ctx context.Context, r dbq.ListDuePersonalWebhookOutboxRow, res *ChatResult, down map[uuid.UUID]bool) error {
	rctx, cancel := recordCtx(ctx)
	defer cancel()
	now := s.now()
	skip := func(reason string) error {
		n, err := s.q.SkipPersonalWebhookOutbox(rctx, dbq.SkipPersonalWebhookOutboxParams{OrgID: r.OrgID, ID: r.ID, Reason: &reason, Now: now})
		res.Skipped += int(n)
		return err
	}

	hook, err := s.q.GetPersonalWebhook(ctx, dbq.GetPersonalWebhookParams{OrgID: r.OrgID, ID: r.WebhookID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !hook.Enabled) {
		return skip("disabled")
	}
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	if !slices.Contains(hook.Events, personalChoice(Event(r.Kind))) {
		return skip("not_selected")
	}
	if s.baseURL == "" {
		return skip("no_base_url")
	}
	active, err := s.q.ListActiveHumanMembers(ctx, dbq.ListActiveHumanMembersParams{OrgID: r.OrgID, UserIds: []uuid.UUID{hook.UserID}})
	if err != nil {
		return fmt.Errorf("owner: %w", err)
	}
	if len(active) == 0 {
		return skip("disabled") // the owner left, was deactivated, or is an agent
	}
	secret, err := openPersonalSecret(s.keys, hook)
	if err != nil {
		if rerr := recordPersonalResult(rctx, s.q, r.OrgID, r.WebhookID, now, ChatUnreadable); rerr != nil {
			return fmt.Errorf("record result: %w", rerr)
		}
		return skip("unreadable")
	}
	issue, err := s.q.GetMailIssue(ctx, dbq.GetMailIssueParams{OrgID: r.OrgID, ID: r.IssueID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (issue.IssueArchivedAt != nil || issue.TeamArchivedAt != nil)) {
		return skip("gone")
	}
	if err != nil {
		return fmt.Errorf("issue: %w", err)
	}
	// T20 #63: the owner must still see the issue's team now.
	sees, err := s.q.MailRecipientSeesTeam(ctx, dbq.MailRecipientSeesTeamParams{OrgID: r.OrgID, UserID: hook.UserID, TeamID: issue.TeamID})
	if err != nil {
		return fmt.Errorf("access: %w", err)
	}
	if !sees {
		return skip("access_removed")
	}
	since := now.Add(-ChatRateWindow)
	sent, err := s.q.CountRecentPersonalWebhookPosts(ctx, dbq.CountRecentPersonalWebhookPostsParams{OrgID: r.OrgID, WebhookID: r.WebhookID, Since: &since})
	if err != nil {
		return fmt.Errorf("rate count: %w", err)
	}
	if sent >= ChatRateCap {
		return skip("rate_cap")
	}

	kind := Event(r.Kind)
	ident := fmt.Sprintf("%s-%d", issue.TeamKey, issue.Number)
	msg := ChatMessage{Event: kind, Identifier: ident, Personal: true,
		Link: s.baseURL + "/" + url.PathEscape(issue.OrgSlug) + "/issue/" + url.PathEscape(ident)}
	if kind == EventStateChange && r.EventSeq != nil {
		if msg.FromState, msg.ToState, err = chatStateNames(ctx, s.q, r.OrgID, *r.EventSeq); err != nil {
			return err
		}
	}

	perr := s.poster.Post(ctx, hook.Kind, string(secret), msg)
	if perr == nil {
		at := s.now()
		n, err := s.q.MarkPersonalWebhookOutboxSent(rctx, dbq.MarkPersonalWebhookOutboxSentParams{OrgID: r.OrgID, ID: r.ID, Now: &at})
		if err != nil {
			return fmt.Errorf("record sent: %w", err)
		}
		if n > 0 {
			res.Sent++
		}
		if err := recordPersonalResult(rctx, s.q, r.OrgID, r.WebhookID, at, ""); err != nil {
			return fmt.Errorf("record result: %w", err)
		}
		return nil
	}
	se := &ChatError{Word: ChatUnreachable}
	errors.As(perr, &se)
	if se.Word == ChatUnreachable || se.Word == ChatRateLimited || se.RetryAfter > 0 {
		down[r.WebhookID] = true
	}
	attempts := r.Attempts + 1
	state, next := "pending", now.Add(RetryDelays[min(int(attempts), len(RetryDelays))-1])
	if se.RetryAfter > 0 {
		next = now.Add(max(se.RetryAfter, next.Sub(now)))
	}
	if int(attempts) >= MaxAttempts || se.permanent() {
		state, next = "dead", now
		res.Dead++
	} else {
		res.Retried++
	}
	if _, err := s.q.FailPersonalWebhookOutbox(rctx, dbq.FailPersonalWebhookOutboxParams{
		OrgID: r.OrgID, ID: r.ID, Attempts: attempts, State: state, NextAttemptAt: next, LastError: &se.Word,
	}); err != nil {
		return fmt.Errorf("record failure: %w", err)
	}
	if err := recordPersonalResult(rctx, s.q, r.OrgID, r.WebhookID, now, se.Word); err != nil {
		return fmt.Errorf("record result: %w", err)
	}
	return nil
}
