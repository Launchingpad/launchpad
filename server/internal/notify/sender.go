package notify

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/login"
	"devops/internal/notify/mail"
)

// Sender constants (decisions-T13 #19 to #25). They are constants in v1, not
// settings.
const (
	// SendInterval is the ticker of Run.
	SendInterval = 15 * time.Second
	// QuietWindow: a group of rows for one recipient and issue waits this
	// long after its NEWEST row, so five edits make one mail.
	QuietWindow = 2 * time.Minute
	// MaxHold: ... but never longer than this after its OLDEST row.
	MaxHold = 10 * time.Minute
	// MaxMailsPerPass is the most mails (groups) one pass claims.
	MaxMailsPerPass = 50
	// DueRowLimit bounds the rows one pass reads.
	DueRowLimit = 1000
	// RowTTL: every row expires this long after its creation (skipped / expired).
	RowTTL = 24 * time.Hour
	// StuckAfter: a row in sending for longer goes back to pending (a crash).
	StuckAfter = 10 * time.Minute
	// SendTimeout bounds one send.
	SendTimeout = 15 * time.Second
	// RateCap is the most mails (batches) per recipient per RateWindow.
	RateCap = 30
	// RateWindow is the rolling window of RateCap.
	RateWindow = time.Hour
	// TokenTTL is the life of an unsubscribe token.
	TokenTTL = 60 * 24 * time.Hour
	// MaxAttempts: the failure that makes the count reach it is the last; the
	// row is dead (1m, 5m, 30m, 2h, 12h, then dead).
	MaxAttempts = 6
	// maxLastError bounds the stored MailError text.
	maxLastError = 300
)

// SkipAccessRemoved is the skip reason of a row whose recipient no longer
// sees the issue's team or the project at send time (T20 #63, migration 0039).
const SkipAccessRemoved = "access_removed"

// RetryDelays is the wait after the 1st, 2nd, ... 5th failure (#20).
var RetryDelays = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 12 * time.Hour}

// Mailer is the transport the sender needs. login.MailSwitch is one.
type Mailer interface {
	// Ready reports whether a transport is set (provider none: false).
	Ready() bool
	SendMessage(ctx context.Context, m login.Message) error
}

// Sender is the outbox sender (decisions-T13 #19 to #25). It sends
// notification_emails rows, one mail per group of rows of one recipient and
// issue. No write path calls it. A mail fault cannot fail a write, and a
// fault in one group never stops the others.
type Sender struct {
	pool    *pgxpool.Pool
	q       *dbq.Queries
	log     *slog.Logger
	mailer  Mailer
	baseURL string
	now     func() time.Time
	timeout time.Duration
}

// NewSender returns a sender. publicBaseURL is PUBLIC_BASE_URL: empty means
// no email (a mail needs a working unsubscribe and issue link, #21).
func NewSender(pool *pgxpool.Pool, log *slog.Logger, m Mailer, publicBaseURL string) *Sender {
	return &Sender{pool: pool, q: dbq.New(pool), log: log, mailer: m,
		baseURL: strings.TrimRight(publicBaseURL, "/"), now: time.Now, timeout: SendTimeout}
}

// SetClock sets the clock of every window, expiry and retry time (tests).
func (s *Sender) SetClock(now func() time.Time) { s.now = now }

// SendResult counts what one pass did.
type SendResult struct {
	Locked    bool // this pass held the sender lock
	Recovered int  // rows back from sending to pending
	Expired   int  // rows that passed 24 hours
	Mails     int  // mails sent
	Sent      int  // rows sent
	Skipped   int  // rows skipped (any reason but expired)
	Retried   int  // rows that failed and wait for a retry
	Dead      int  // rows that failed the last time
}

// Run sends at boot and every interval until ctx ends. A panic is recovered
// per pass and logged (decisions-T13 #37). It logs the dead letters once.
func (s *Sender) Run(ctx context.Context, interval time.Duration) {
	if n, err := s.q.CountDeadNotificationEmails(ctx); err == nil && n > 0 {
		s.log.WarnContext(ctx, "notification mail: dead letters are waiting (docs/deploy.md 'Notifications')", "dead", n)
	}
	once := func() {
		defer func() {
			if r := recover(); r != nil {
				s.log.ErrorContext(ctx, "notification sender panicked", "panic", fmt.Sprint(r))
			}
		}()
		res, err := s.RunOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.log.ErrorContext(ctx, "notification sender failed; retrying on the next tick", "err", err)
		}
		// One line per pass with counts: never an address, a subject or a body.
		attrs := []any{"mails", res.Mails, "rows_sent", res.Sent, "skipped", res.Skipped,
			"retry", res.Retried, "dead", res.Dead, "expired", res.Expired, "recovered", res.Recovered}
		switch {
		case res.Retried+res.Dead > 0:
			s.log.WarnContext(ctx, "notification sender: some mail failed", attrs...)
		case res.Mails+res.Skipped+res.Expired+res.Recovered > 0:
			s.log.InfoContext(ctx, "notification sender", attrs...)
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

// group is the rows of one recipient and one issue that go out as one mail.
type group struct {
	org, recipient, issue uuid.UUID
	project               uuid.UUID // set for a project notification (issue is Nil then)
	rows                  []dbq.ListDueNotificationEmailsRow
	batch                 uuid.UUID
}

func (g *group) ids() []uuid.UUID {
	ids := make([]uuid.UUID, len(g.rows))
	for i, r := range g.rows {
		ids[i] = r.ID
	}
	return ids
}

// RunOnce is one pass: take the sender lock (a second instance returns at
// once), return stuck rows to pending, expire old rows, and, when mail is
// on, claim the groups that are ready and send them.
func (s *Sender) RunOnce(ctx context.Context) (SendResult, error) {
	var res SendResult
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return res, fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()
	lockq := dbq.New(conn)
	locked, err := lockq.TryLockNotifySender(ctx)
	if err != nil {
		return res, fmt.Errorf("lock: %w", err)
	}
	if !locked {
		return res, nil
	}
	res.Locked = true
	defer func() {
		// A session lock must never go back to the pool held.
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if ok, err := lockq.UnlockNotifySender(uctx); err != nil || !ok {
			_ = conn.Hijack().Close(uctx)
		}
	}()

	now := s.now()
	n, err := s.q.RecoverStuckNotificationEmails(ctx, now.Add(-StuckAfter))
	if err != nil {
		return res, fmt.Errorf("recover: %w", err)
	}
	res.Recovered = int(n)
	n, err = s.q.ExpireNotificationEmails(ctx, dbq.ExpireNotificationEmailsParams{Now: now, Cutoff: now.Add(-RowTTL)})
	if err != nil {
		return res, fmt.Errorf("expire: %w", err)
	}
	res.Expired = int(n)

	// Provider none, or env with nothing set: rows stay pending and nothing
	// is attempted or failed; the 24 hour expiry above ends them (#21).
	if !s.mailer.Ready() {
		return res, nil
	}
	groups, err := s.claim(ctx, now)
	if err != nil {
		return res, fmt.Errorf("claim: %w", err)
	}
	var errs []error
	for i := range groups {
		if ctx.Err() != nil {
			break
		}
		if err := s.sendGroup(ctx, &groups[i], &res); err != nil {
			errs = append(errs, err)
		}
	}
	return res, errors.Join(errs...)
}

// claim finds the groups whose quiet window is over, and marks their rows
// sending, in one transaction. A group with a retry that is due is ready at
// once. A group keeps the batch id its rows already have (a retry sends
// under the same Message-ID), else gets a new one.
func (s *Sender) claim(ctx context.Context, now time.Time) ([]group, error) {
	var out []group
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		out = nil
		q := s.q.WithTx(tx)
		rows, err := q.ListDueNotificationEmails(ctx, dbq.ListDueNotificationEmailsParams{Now: now, MaxRows: DueRowLimit})
		if err != nil {
			return err
		}
		var groups []group
		for _, r := range rows {
			if n := len(groups); n > 0 {
				last := &groups[n-1]
				if last.org == r.OrgID && last.recipient == r.RecipientID && last.issue == derefID(r.IssueID) && last.project == derefID(r.ProjectID) {
					last.rows = append(last.rows, r)
					continue
				}
			}
			groups = append(groups, group{org: r.OrgID, recipient: r.RecipientID, issue: derefID(r.IssueID), project: derefID(r.ProjectID), rows: []dbq.ListDueNotificationEmailsRow{r}})
		}
		for _, g := range groups {
			if len(out) >= MaxMailsPerPass {
				break
			}
			if !ready(g.rows, now) {
				continue
			}
			g.batch = uuid.New()
			for _, r := range g.rows {
				if r.BatchID != nil {
					g.batch = *r.BatchID
					break
				}
			}
			b := g.batch
			if _, err := q.ClaimNotificationEmails(ctx, dbq.ClaimNotificationEmailsParams{OrgID: g.org, Now: now, BatchID: &b, Ids: g.ids()}); err != nil {
				return err
			}
			out = append(out, g)
		}
		return nil
	})
	return out, err
}

// ready: a retry is due, or the newest row is older than the quiet window,
// or the oldest row is older than the hold limit (#22).
func ready(rows []dbq.ListDueNotificationEmailsRow, now time.Time) bool {
	newest, oldest := rows[0].CreatedAt, rows[0].CreatedAt
	for _, r := range rows {
		if r.Attempts > 0 {
			return true
		}
		if r.CreatedAt.After(newest) {
			newest = r.CreatedAt
		}
		if r.CreatedAt.Before(oldest) {
			oldest = r.CreatedAt
		}
	}
	return !newest.After(now.Add(-QuietWindow)) || !oldest.After(now.Add(-MaxHold))
}

// recordCtx is the context of the writes that record a result: a send that
// has happened must be recorded even when the server is shutting down.
func recordCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

type rowDecision struct {
	row    dbq.ListDueNotificationEmailsRow
	source Source
	team   uuid.UUID
}

// sendGroup decides, builds and sends one mail, then records the result for
// every row of the group. A database error leaves the rows in sending (the
// next pass returns them to pending after StuckAfter): at-least-once.
func (s *Sender) sendGroup(ctx context.Context, g *group, res *SendResult) error {
	rctx, cancel := recordCtx(ctx)
	defer cancel()
	now := s.now()
	skip := func(ids []uuid.UUID, reason string) error {
		if len(ids) == 0 {
			return nil
		}
		n, err := s.q.SkipNotificationEmails(rctx, dbq.SkipNotificationEmailsParams{OrgID: g.org, Reason: &reason, Now: now, Ids: ids})
		res.Skipped += int(n)
		return err
	}
	all := g.ids()

	rec, err := s.q.GetMailRecipient(ctx, dbq.GetMailRecipientParams{OrgID: g.org, ID: g.recipient})
	if errors.Is(err, pgx.ErrNoRows) {
		return skip(all, "disabled") // not a member any more
	}
	if err != nil {
		return fmt.Errorf("recipient: %w", err)
	}
	switch {
	case rec.IsAgent || rec.DeactivatedAt != nil:
		return skip(all, "disabled")
	case rec.Email == nil || *rec.Email == "":
		return skip(all, "no_address")
	case rec.EmailUnproven:
		return skip(all, "unproven")
	case s.baseURL == "":
		return skip(all, "no_base_url")
	}
	if g.project != uuid.Nil {
		return s.sendProjectGroup(ctx, g, res, *rec.Email, skip)
	}
	issue, err := s.q.GetMailIssue(ctx, dbq.GetMailIssueParams{OrgID: g.org, ID: g.issue})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (issue.IssueArchivedAt != nil || issue.TeamArchivedAt != nil)) {
		return skip(all, "disabled")
	}
	if err != nil {
		return fmt.Errorf("issue: %w", err)
	}
	// T20 #63: the recipient must still see the issue's team at send time.
	// Access removed after the notification was made: no mail, so no title or
	// text of an issue the person cannot see is ever sent.
	sees, err := s.q.MailRecipientSeesTeam(ctx, dbq.MailRecipientSeesTeamParams{OrgID: g.org, UserID: g.recipient, TeamID: issue.TeamID})
	if err != nil {
		return fmt.Errorf("access: %w", err)
	}
	if !sees {
		return skip(all, SkipAccessRemoved)
	}

	// Re-resolve the PREFERENCE from the rows as they are now (#21): a person
	// who turned email off, turned the event off or unsubscribed stops the
	// mail that is still waiting.
	prefs, settings, err := s.loadPrefs(ctx, g)
	if err != nil {
		return err
	}
	var keep []rowDecision
	var off []uuid.UUID
	for _, r := range g.rows {
		team := issue.TeamID
		got := Resolve(prefs, &team, Event(r.Kind), settings)
		if !got.Email.On {
			off = append(off, r.ID)
			continue
		}
		keep = append(keep, rowDecision{row: r, source: got.Email.Source, team: team})
	}
	if err := skip(off, "disabled"); err != nil {
		return err
	}
	if len(keep) == 0 {
		return nil
	}
	keepIDs := make([]uuid.UUID, len(keep))
	for i, k := range keep {
		keepIDs[i] = k.row.ID
	}

	// The rate cap counts MAILS: distinct batch ids sent in the last hour (#23).
	since := now.Add(-RateWindow)
	sent, err := s.q.CountRecentMailBatches(ctx, dbq.CountRecentMailBatchesParams{OrgID: g.org, RecipientID: g.recipient, Since: &since})
	if err != nil {
		return fmt.Errorf("rate count: %w", err)
	}
	if sent >= RateCap {
		return skip(keepIDs, "rate_cap")
	}
	notEmailed, err := s.q.CountUnreportedRateCapped(ctx, dbq.CountUnreportedRateCappedParams{OrgID: g.org, RecipientID: g.recipient})
	if err != nil {
		return fmt.Errorf("rate capped count: %w", err)
	}

	msg, err := s.build(ctx, g, keep, issue, prefs, int(notEmailed), *rec.Email, now)
	if err != nil {
		// Our own fault (a bad row): release nothing; the rows return after
		// StuckAfter and the error is logged once per pass.
		return fmt.Errorf("build: %w", err)
	}

	return s.transmit(ctx, rctx, g, keep, msg, now, res)
}

// loadPrefs reads the recipient's master switches and event settings as they are now.
func (s *Sender) loadPrefs(ctx context.Context, g *group) (UserPrefs, []dbq.NotificationSetting, error) {
	prefs := DefaultUserPrefs
	up, err := s.q.GetNotificationUserSettings(ctx, dbq.GetNotificationUserSettingsParams{OrgID: g.org, UserID: g.recipient})
	switch {
	case err == nil:
		prefs = UserPrefs{EmailEnabled: up.EmailEnabled, IncludeExcerpt: up.IncludeExcerpt}
	case !errors.Is(err, pgx.ErrNoRows):
		return prefs, nil, fmt.Errorf("user settings: %w", err)
	}
	settings, err := s.q.ListNotificationSettingsSystem(ctx, dbq.ListNotificationSettingsSystemParams{OrgID: g.org, UserID: g.recipient})
	if err != nil {
		return prefs, nil, fmt.Errorf("settings: %w", err)
	}
	return prefs, settings, nil
}

func derefID(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

// transmit sends msg and records the result for the kept rows of the group.
func (s *Sender) transmit(ctx, rctx context.Context, g *group, keep []rowDecision, msg login.Message, now time.Time, res *SendResult) error {
	keepIDs := make([]uuid.UUID, len(keep))
	for i, k := range keep {
		keepIDs[i] = k.row.ID
	}
	sctx, scancel := context.WithTimeout(ctx, s.timeout)
	sendErr := s.mailer.SendMessage(sctx, msg)
	scancel()

	switch {
	case sendErr == nil:
		b := g.batch
		at := s.now()
		if _, err := s.q.MarkNotificationEmailsSent(rctx, dbq.MarkNotificationEmailsSentParams{OrgID: g.org, Now: &at, BatchID: &b, Ids: keepIDs}); err != nil {
			return fmt.Errorf("record sent: %w", err)
		}
		res.Mails++
		res.Sent += len(keepIDs)
		return nil
	case errors.Is(sendErr, login.ErrNoMailer):
		// Mail was turned off during the pass: no attempt counts.
		_, err := s.q.ReleaseNotificationEmails(rctx, dbq.ReleaseNotificationEmailsParams{OrgID: g.org, Now: now, Ids: keepIDs})
		return err
	}
	text := login.PublicMailError(sendErr)
	if len(text) > maxLastError {
		text = text[:maxLastError]
	}
	for _, k := range keep {
		attempts := k.row.Attempts + 1
		state, next := "pending", now.Add(RetryDelays[min(int(attempts), len(RetryDelays))-1])
		if int(attempts) >= MaxAttempts {
			state, next = "dead", now
			res.Dead++
		} else {
			res.Retried++
		}
		if _, err := s.q.FailNotificationEmail(rctx, dbq.FailNotificationEmailParams{
			OrgID: g.org, ID: k.row.ID, Attempts: attempts, State: state, NextAttemptAt: next, LastError: &text,
		}); err != nil {
			return fmt.Errorf("record failure: %w", err)
		}
	}
	return nil
}

// rowData is the ids a notification row holds (decisions-T13 #6).
type rowData struct {
	From    *uuid.UUID `json:"fromStateId"`
	To      *uuid.UUID `json:"toStateId"`
	Comment *uuid.UUID `json:"commentId"`
}

// target is one {event, teamId} of an unsubscribe token (#8, #25).
type target struct {
	Event  string     `json:"event"`
	TeamID *uuid.UUID `json:"teamId"`
}

// build makes the message of one group and stores its unsubscribe token.
func (s *Sender) build(ctx context.Context, g *group, keep []rowDecision, issue dbq.GetMailIssueRow,
	prefs UserPrefs, notEmailed int, to string, now time.Time) (login.Message, error) {
	// Names: the actors and the states named in the rows' data.
	var actorIDs, stateIDs []uuid.UUID
	datas := make([]rowData, len(keep))
	for i, k := range keep {
		var d rowData
		if err := json.Unmarshal(k.row.Data, &d); err != nil {
			return login.Message{}, errors.New("row data is not readable")
		}
		datas[i] = d
		if k.row.ActorID != nil {
			actorIDs = append(actorIDs, *k.row.ActorID)
		}
		for _, id := range []*uuid.UUID{d.From, d.To} {
			if id != nil {
				stateIDs = append(stateIDs, *id)
			}
		}
	}
	actors := map[uuid.UUID]string{}
	if len(actorIDs) > 0 {
		us, err := s.q.ListUserNamesByIDs(ctx, actorIDs)
		if err != nil {
			return login.Message{}, err
		}
		for _, u := range us {
			actors[u.ID] = u.Name
		}
	}
	states := map[uuid.UUID]string{}
	if len(stateIDs) > 0 {
		ss, err := s.q.ListStateNamesByIDs(ctx, dbq.ListStateNamesByIDsParams{OrgID: g.org, Ids: stateIDs})
		if err != nil {
			return login.Message{}, err
		}
		for _, st := range ss {
			states[st.ID] = st.Name
		}
	}

	items := make([]mail.Item, len(keep))
	kinds := map[Event]bool{}
	for i, k := range keep {
		it := mail.Item{Kind: k.row.Kind}
		if k.row.ActorID != nil {
			it.Actor = actors[*k.row.ActorID]
		}
		if d := datas[i]; d.From != nil || d.To != nil {
			if d.From != nil {
				it.FromState = states[*d.From]
			}
			if d.To != nil {
				it.ToState = states[*d.To]
			}
		}
		if prefs.IncludeExcerpt && (k.row.Kind == string(EventComment) || k.row.Kind == string(EventMention)) {
			switch c := datas[i].Comment; {
			case c != nil:
				cm, err := s.q.GetCommentSystem(ctx, dbq.GetCommentSystemParams{OrgID: g.org, ID: *c})
				if err == nil {
					it.Excerpt = cm.Body
				} else if !errors.Is(err, pgx.ErrNoRows) {
					return login.Message{}, err
				}
			case k.row.Kind == string(EventMention):
				it.Excerpt = issue.Description // a mention in the description
			}
		}
		items[i] = it
		kinds[Event(k.row.Kind)] = true
	}

	// The primary kind: the first by precedence (#22).
	var targets []target
	primary := ""
	for _, e := range []Event{EventMention, EventAssigned, EventComment, EventStateChange, EventNewIssue} {
		if !kinds[e] {
			continue
		}
		if primary == "" {
			primary = string(e)
		}
		t := target{Event: string(e)}
		for _, k := range keep {
			if Event(k.row.Kind) == e && k.source == SourceTeam {
				team := k.team
				t.TeamID = &team // the value came from a team row: that level
				break
			}
		}
		targets = append(targets, t) // default and global both land at the global level
	}

	token, err := s.newToken(ctx, g, targets, now)
	if err != nil {
		return login.Message{}, err
	}

	ident := fmt.Sprintf("%s-%d", issue.TeamKey, issue.Number)
	slug := url.PathEscape(issue.OrgSlug)
	out, err := mail.Build(mail.Input{
		Primary: primary, Identifier: ident, Title: issue.Title, TeamKey: issue.TeamKey, StateName: issue.StateName,
		IssueURL:       s.baseURL + "/" + slug + "/issue/" + url.PathEscape(ident),
		SettingsURL:    s.baseURL + "/" + slug + "/settings/notifications",
		UnsubscribeURL: s.baseURL + "/unsubscribe/" + token,
		Items:          items, NotEmailed: notEmailed,
	})
	if err != nil {
		return login.Message{}, err
	}
	host := "launchpad.invalid"
	if u, err := url.Parse(s.baseURL); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	return login.Message{
		To: to, Subject: out.Subject, Text: out.Text, HTML: out.HTML,
		Headers: map[string]string{
			"Message-ID":            "<" + g.batch.String() + "@" + host + ">",
			"List-Unsubscribe":      "<" + s.baseURL + "/api/v1/notifications/unsubscribe/" + token + ">",
			"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
		},
	}, nil
}

// HashToken is the stored form of an unsubscribe token (#25).
func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// newToken makes one random 32-byte token for a mail and stores only its
// sha256 hash and the targets (#25).
func (s *Sender) newToken(ctx context.Context, g *group, targets []target, now time.Time) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", errors.New("no randomness for the unsubscribe token")
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	tj, err := json.Marshal(slices.Clone(targets))
	if err != nil {
		return "", err
	}
	if err := s.q.InsertNotificationMailToken(ctx, dbq.InsertNotificationMailTokenParams{
		TokenHash: HashToken(token), OrgID: g.org, UserID: g.recipient, Targets: tj, ExpiresAt: now.Add(TokenTTL),
	}); err != nil {
		return "", fmt.Errorf("store token: %w", err)
	}
	return token, nil
}

// sendProjectGroup sends the reminder of one project (LAUN-196). The rows of
// the group are the reminders of that project for one recipient. The settings
// are the person's global ones, read again now (#21).
func (s *Sender) sendProjectGroup(ctx context.Context, g *group, res *SendResult, to string,
	skip func([]uuid.UUID, string) error) error {
	rctx, cancel := recordCtx(ctx)
	defer cancel()
	now := s.now()
	all := g.ids()
	proj, err := s.q.GetMailProject(ctx, dbq.GetMailProjectParams{OrgID: g.org, ID: g.project})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && proj.ProjectArchivedAt != nil) {
		return skip(all, "disabled")
	}
	if err != nil {
		return fmt.Errorf("project: %w", err)
	}
	// T20 #63: the recipient must still see the project at send time.
	sees, err := s.q.MailRecipientSeesProject(ctx, dbq.MailRecipientSeesProjectParams{OrgID: g.org, UserID: g.recipient, ProjectID: g.project})
	if err != nil {
		return fmt.Errorf("access: %w", err)
	}
	if !sees {
		return skip(all, SkipAccessRemoved)
	}
	prefs, settings, err := s.loadPrefs(ctx, g)
	if err != nil {
		return err
	}
	var keep []rowDecision
	var off []uuid.UUID
	for _, r := range g.rows {
		got := Resolve(prefs, nil, Event(r.Kind), settings)
		if !got.Email.On {
			off = append(off, r.ID)
			continue
		}
		keep = append(keep, rowDecision{row: r, source: got.Email.Source})
	}
	if err := skip(off, "disabled"); err != nil {
		return err
	}
	if len(keep) == 0 {
		return nil
	}
	keepIDs := make([]uuid.UUID, len(keep))
	for i, k := range keep {
		keepIDs[i] = k.row.ID
	}
	since := now.Add(-RateWindow)
	sent, err := s.q.CountRecentMailBatches(ctx, dbq.CountRecentMailBatchesParams{OrgID: g.org, RecipientID: g.recipient, Since: &since})
	if err != nil {
		return fmt.Errorf("rate count: %w", err)
	}
	if sent >= RateCap {
		return skip(keepIDs, "rate_cap")
	}
	notEmailed, err := s.q.CountUnreportedRateCapped(ctx, dbq.CountUnreportedRateCappedParams{OrgID: g.org, RecipientID: g.recipient})
	if err != nil {
		return fmt.Errorf("rate capped count: %w", err)
	}
	token, err := s.newToken(ctx, g, []target{{Event: string(EventProjectUpdateDue)}}, now)
	if err != nil {
		return fmt.Errorf("build: %w", err)
	}
	slug := url.PathEscape(proj.OrgSlug)
	out := mail.BuildProjectDue(mail.ProjectDueInput{
		ProjectName:    proj.Name,
		ProjectURL:     s.baseURL + "/" + slug + "/projects/" + g.project.String(),
		SettingsURL:    s.baseURL + "/" + slug + "/settings/notifications",
		UnsubscribeURL: s.baseURL + "/unsubscribe/" + token,
		NotEmailed:     int(notEmailed),
	})
	host := "launchpad.invalid"
	if u, err := url.Parse(s.baseURL); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	msg := login.Message{
		To: to, Subject: out.Subject, Text: out.Text, HTML: out.HTML,
		Headers: map[string]string{
			"Message-ID":            "<" + g.batch.String() + "@" + host + ">",
			"List-Unsubscribe":      "<" + s.baseURL + "/api/v1/notifications/unsubscribe/" + token + ">",
			"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
		},
	}
	return s.transmit(ctx, rctx, g, keep, msg, now, res)
}
