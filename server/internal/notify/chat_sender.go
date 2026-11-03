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

// Chat sender constants (decisions-T33, T36). Constants in v1, not settings.
const (
	// ChatSendInterval is the ticker of Run.
	ChatSendInterval = 15 * time.Second
	// ChatRowTTL: a post that waited this long is skipped (expired).
	ChatRowTTL = 24 * time.Hour
	// ChatRateCap is the most posts one service gets per ChatRateWindow.
	ChatRateCap = 60
	// ChatRateWindow is the rolling window of ChatRateCap.
	ChatRateWindow = time.Hour
	// ChatPerOrgPerPass is the most posts one pass sends for one org, so a burst
	// of events does not trip a service's own limit (Slack allows about one post a second).
	ChatPerOrgPerPass = 10
	// ChatDueLimit bounds the rows one pass reads.
	ChatDueLimit = 200
	// ChatRetention: terminal rows are deleted this long after creation.
	ChatRetention = 30 * 24 * time.Hour
	// chatPruneBatch is the most rows one DELETE removes.
	chatPruneBatch = 1000
	// ChatPassBudget is the wall-clock time one pass may spend posting. When it
	// is over, the claimed rows that are left go back to pending untouched (no
	// attempt used), so one slow host cannot hold the global lock for long.
	ChatPassBudget = 60 * time.Second
)

// ChatSender sends the chat_outbox rows the consumer made. Like the mail
// Sender it is called by no write path; a chat fault cannot fail a write.
type ChatSender struct {
	pool    *pgxpool.Pool
	q       *dbq.Queries
	log     *slog.Logger
	keys    *platform.SlackKeyring
	poster  *ChatPoster
	baseURL string
	now     func() time.Time
	budget  time.Duration
}

// NewChatSender returns a sender. keys may be nil (rows are then skipped as
// unreadable). publicBaseURL empty means no post (a post is a link).
func NewChatSender(pool *pgxpool.Pool, log *slog.Logger, keys *platform.SlackKeyring, poster *ChatPoster, publicBaseURL string) *ChatSender {
	return &ChatSender{pool: pool, q: dbq.New(pool), log: log, keys: keys, poster: poster, baseURL: trimBase(publicBaseURL), now: time.Now, budget: ChatPassBudget}
}

// SetClock sets the clock of the expiry, the rate window and the retries (tests).
func (s *ChatSender) SetClock(now func() time.Time) { s.now = now }

// SetBudget sets the wall-clock budget of one pass (tests).
func (s *ChatSender) SetBudget(d time.Duration) { s.budget = d }

// ChatResult counts what one pass did.
type ChatResult struct {
	Locked    bool
	Recovered int
	Expired   int
	Sent      int
	Skipped   int
	Retried   int
	Dead      int
	Pruned    int
	// Released counts claimed rows put back to pending without an attempt: the
	// service timed out earlier in the pass, or the pass budget ran out.
	Released int
}

// Run sends at boot and every interval until ctx ends. A panic is recovered
// per pass. It logs counts and fixed words only.
func (s *ChatSender) Run(ctx context.Context, interval time.Duration) {
	once := func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.ErrorContext(ctx, "chat sender panicked", "panic", fmt.Sprint(r))
			}
		}()
		res, err := s.RunOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.log.ErrorContext(ctx, "chat sender failed; retrying on the next tick", "err", err)
		}
		attrs := []any{"sent", res.Sent, "skipped", res.Skipped, "retry", res.Retried, "dead", res.Dead,
			"expired", res.Expired, "recovered", res.Recovered}
		switch {
		case res.Retried+res.Dead > 0:
			s.log.WarnContext(ctx, "chat sender: some posts failed", attrs...)
		case res.Sent+res.Skipped+res.Expired+res.Recovered > 0:
			s.log.InfoContext(ctx, "chat sender", attrs...)
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
// expire old rows, prune old terminal rows, claim the due rows and post them.
func (s *ChatSender) RunOnce(ctx context.Context) (ChatResult, error) {
	var res ChatResult
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return res, fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()
	lockq := dbq.New(conn)
	locked, err := lockq.TryLockChatSender(ctx)
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
		if ok, err := lockq.UnlockChatSender(uctx); err != nil || !ok {
			_ = conn.Hijack().Close(uctx)
		}
	}()

	now := s.now()
	n, err := s.q.RecoverStuckChatOutbox(ctx, now.Add(-StuckAfter))
	if err != nil {
		return res, fmt.Errorf("recover: %w", err)
	}
	res.Recovered = int(n)
	if n, err = s.q.ExpireChatOutbox(ctx, dbq.ExpireChatOutboxParams{Now: now, Cutoff: now.Add(-ChatRowTTL)}); err != nil {
		return res, fmt.Errorf("expire: %w", err)
	}
	res.Expired = int(n)
	if n, err = s.q.PruneChatOutbox(ctx, dbq.PruneChatOutboxParams{Cutoff: now.Add(-ChatRetention), BatchSize: chatPruneBatch}); err != nil {
		return res, fmt.Errorf("prune: %w", err)
	}
	res.Pruned = int(n)

	rows, err := s.claim(ctx, now)
	if err != nil {
		return res, fmt.Errorf("claim: %w", err)
	}
	var errs []error
	deadline := time.Now().Add(s.budget)
	down := map[uuid.UUID]bool{} // services that were unreachable in this pass
	for _, r := range rows {
		if ctx.Err() != nil {
			break
		}
		if down[r.ServiceID] || !time.Now().Before(deadline) {
			if err := s.release(ctx, r, &res); err != nil {
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

// claim marks up to ChatPerOrgPerPass due rows of each org as sending, in one
// transaction. The rest wait for the next pass.
func (s *ChatSender) claim(ctx context.Context, now time.Time) ([]dbq.ListDueChatOutboxRow, error) {
	var out []dbq.ListDueChatOutboxRow
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		out = nil
		q := s.q.WithTx(tx)
		rows, err := q.ListDueChatOutbox(ctx, dbq.ListDueChatOutboxParams{Now: now, PerOrg: ChatPerOrgPerPass, MaxRows: ChatDueLimit})
		if err != nil {
			return err
		}
		perOrg := map[uuid.UUID]int{}
		for _, r := range rows {
			if perOrg[r.OrgID] >= ChatPerOrgPerPass {
				continue
			}
			n, err := q.ClaimChatOutbox(ctx, dbq.ClaimChatOutboxParams{OrgID: r.OrgID, ID: r.ID, Now: now})
			if err != nil {
				return err
			}
			if n == 0 {
				continue // the row left pending since the list (a purge, another writer): do not send it
			}
			perOrg[r.OrgID]++
			out = append(out, r)
		}
		return nil
	})
	return out, err
}

// release puts a claimed row back to pending. It uses no attempt and the row
// stays due, so the next pass picks it up in the usual fair order.
func (s *ChatSender) release(ctx context.Context, r dbq.ListDueChatOutboxRow, res *ChatResult) error {
	rctx, cancel := recordCtx(ctx)
	defer cancel()
	n, err := s.q.ReleaseChatOutbox(rctx, dbq.ReleaseChatOutboxParams{OrgID: r.OrgID, ID: r.ID})
	res.Released += int(n)
	return err
}

// sendRow decides, builds and posts one row, then records the result. A
// database error leaves the row in sending (it returns to pending after
// StuckAfter): at-least-once.
func (s *ChatSender) sendRow(ctx context.Context, r dbq.ListDueChatOutboxRow, res *ChatResult, down map[uuid.UUID]bool) error {
	rctx, cancel := recordCtx(ctx)
	defer cancel()
	now := s.now()
	skip := func(reason string) error {
		n, err := s.q.SkipChatOutbox(rctx, dbq.SkipChatOutboxParams{OrgID: r.OrgID, ID: r.ID, Reason: &reason, Now: now})
		res.Skipped += int(n)
		return err
	}

	svc, err := s.q.GetChatService(ctx, dbq.GetChatServiceParams{OrgID: r.OrgID, ID: r.ServiceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return skip("disabled") // the admin removed the service
	}
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}
	if !svc.Enabled {
		return skip("disabled")
	}
	if !slices.Contains(svc.Events, r.Kind) {
		return skip("not_selected") // the admin took the kind off the list
	}
	if s.baseURL == "" {
		return skip("no_base_url")
	}
	hook, err := openChatSecret(s.keys, svc)
	if err != nil {
		if rerr := recordChatResult(rctx, s.q, r.OrgID, r.ServiceID, now, ChatUnreadable); rerr != nil {
			return fmt.Errorf("record result: %w", rerr)
		}
		return skip("unreadable")
	}

	msg, why, err := s.message(ctx, r, svc.TeamID)
	if err != nil {
		return err
	}
	if why != "" {
		return skip(why)
	}

	since := now.Add(-ChatRateWindow)
	sent, err := s.q.CountRecentChatPosts(ctx, dbq.CountRecentChatPostsParams{OrgID: r.OrgID, ServiceID: r.ServiceID, Since: &since})
	if err != nil {
		return fmt.Errorf("rate count: %w", err)
	}
	if sent >= ChatRateCap {
		return skip("rate_cap")
	}

	perr := s.poster.Post(ctx, svc.Kind, string(hook), msg)
	if perr == nil {
		at := s.now()
		n, err := s.q.MarkChatOutboxSent(rctx, dbq.MarkChatOutboxSentParams{OrgID: r.OrgID, ID: r.ID, Now: &at})
		if err != nil {
			return fmt.Errorf("record sent: %w", err)
		}
		if n > 0 { // 0: the admin switched the service off or removed it mid-flight
			res.Sent++
		}
		if err := recordChatResult(rctx, s.q, r.OrgID, r.ServiceID, at, ""); err != nil {
			return fmt.Errorf("record result: %w", err)
		}
		return nil
	}
	se := &ChatError{Word: ChatUnreachable}
	errors.As(perr, &se)
	if se.Word == ChatUnreachable || se.Word == ChatRateLimited || se.RetryAfter > 0 {
		down[r.ServiceID] = true // a timeout, a dead host or a 429: no more posts to it in this pass
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
	if _, err := s.q.FailChatOutbox(rctx, dbq.FailChatOutboxParams{
		OrgID: r.OrgID, ID: r.ID, Attempts: attempts, State: state, NextAttemptAt: next, LastError: &se.Word,
	}); err != nil {
		return fmt.Errorf("record failure: %w", err)
	}
	if err := recordChatResult(rctx, s.q, r.OrgID, r.ServiceID, now, se.Word); err != nil {
		return fmt.Errorf("record result: %w", err)
	}
	return nil
}

// message builds the post of a row from ids: the event kind, the identifier of
// an issue, and a link. why is a skip word when there is no post: "gone" when
// the target is gone or archived, "not_selected" when the target is not (or no
// longer) of the service's current team (LAUN-231: the service moved, or the
// issue did, after the row was queued; the channel of one team never gets a
// post of another).
func (s *ChatSender) message(ctx context.Context, r dbq.ListDueChatOutboxRow, team uuid.UUID) (ChatMessage, string, error) {
	kind := Event(r.Kind)
	if r.ProjectID != nil {
		p, err := s.q.GetMailProject(ctx, dbq.GetMailProjectParams{OrgID: r.OrgID, ID: *r.ProjectID})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && p.ProjectArchivedAt != nil) {
			return ChatMessage{}, "gone", nil
		}
		if err != nil {
			return ChatMessage{}, "", fmt.Errorf("project: %w", err)
		}
		teams, err := s.q.ListProjectTeamIDs(ctx, dbq.ListProjectTeamIDsParams{OrgID: r.OrgID, ProjectID: *r.ProjectID})
		if err != nil {
			return ChatMessage{}, "", fmt.Errorf("project teams: %w", err)
		}
		if !slices.Contains(teams, team) {
			return ChatMessage{}, "not_selected", nil
		}
		return ChatMessage{Event: kind, Link: s.baseURL + "/" + url.PathEscape(p.OrgSlug) + "/projects/" + r.ProjectID.String()}, "", nil
	}
	if r.IssueID == nil {
		return ChatMessage{}, "gone", nil
	}
	issue, err := s.q.GetMailIssue(ctx, dbq.GetMailIssueParams{OrgID: r.OrgID, ID: *r.IssueID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (issue.IssueArchivedAt != nil || issue.TeamArchivedAt != nil)) {
		return ChatMessage{}, "gone", nil
	}
	if err != nil {
		return ChatMessage{}, "", fmt.Errorf("issue: %w", err)
	}
	if issue.TeamID != team {
		return ChatMessage{}, "not_selected", nil
	}
	ident := fmt.Sprintf("%s-%d", issue.TeamKey, issue.Number)
	msg := ChatMessage{Event: kind, Identifier: ident,
		Link: s.baseURL + "/" + url.PathEscape(issue.OrgSlug) + "/issue/" + url.PathEscape(ident)}
	if kind == EventStateChange && r.EventSeq != nil {
		var err error
		if msg.FromState, msg.ToState, err = s.stateNames(ctx, r.OrgID, *r.EventSeq); err != nil {
			return ChatMessage{}, "", err
		}
	}
	return msg, "", nil
}

// stateNames reads the state ids the event carried and returns the CURRENT
// names of both, in the org of the row (decisions-T39). Both or neither: an
// event that is gone, an id that is missing, deleted or of another org gives
// "", "" and the post keeps its fixed sentence. A name is never logged.
func (s *ChatSender) stateNames(ctx context.Context, org uuid.UUID, seq int64) (from, to string, err error) {
	return chatStateNames(ctx, s.q, org, seq)
}

// chatStateNames is stateNames for any sender (the chat services and the
// personal webhooks, decisions-T40 #14).
func chatStateNames(ctx context.Context, q *dbq.Queries, org uuid.UUID, seq int64) (from, to string, err error) {
	ids, err := q.GetChatEventStateIDs(ctx, dbq.GetChatEventStateIDsParams{OrgID: org, Seq: seq})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("event: %w", err)
	}
	f, errF := uuid.Parse(ids.FromStateID)
	t, errT := uuid.Parse(ids.ToStateID)
	if errF != nil || errT != nil {
		return "", "", nil
	}
	rows, err := q.ListChatStateNames(ctx, dbq.ListChatStateNamesParams{OrgID: org, Ids: []uuid.UUID{f, t}})
	if err != nil {
		return "", "", fmt.Errorf("states: %w", err)
	}
	names := map[uuid.UUID]string{}
	for _, r := range rows {
		names[r.ID] = r.Name
	}
	if names[f] == "" || names[t] == "" {
		return "", "", nil
	}
	return names[f], names[t], nil
}
