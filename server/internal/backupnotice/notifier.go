package backupnotice

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/dbq"
	"devops/internal/login"
)

// Mailer is the transport: login.MailSwitch in production. Ready is false when
// the platform mail provider is "none" (or "env" with nothing set); nothing is
// then sent and nothing is marked as sent.
type Mailer interface {
	Ready() bool
	SendText(ctx context.Context, to, subject, text string) error
}

// Windows (decisions-T16 ticket 8 note 4): a run that ended longer ago than
// RunWindow, or a change older than ChangeWindow, gets no mail when mail comes
// back; the page and the table still show it.
const (
	RunWindow    = 24 * time.Hour
	ChangeWindow = 7 * 24 * time.Hour
	// Interval is how often Run ticks (#41: every minute).
	Interval = time.Minute
	// SendTimeout bounds one mail.
	SendTimeout = 15 * time.Second
	// ChangeBudget bounds the change-notice sends of one tick.
	ChangeBudget = 45 * time.Second
)

// Options configure a Notifier.
type Options struct {
	Mailer Mailer
	// PublicBaseURL is the web origin of the link in a mail; empty: no link.
	PublicBaseURL string
	// Clock is the clock of every window and of the stale rule. Nil: the
	// DATABASE clock (#31 "Clock"). Tests inject one.
	Clock func() time.Time
	// SendTimeout overrides SendTimeout (tests).
	SendTimeout time.Duration
	// ChangeBudget is the most time one tick spends sending change notices
	// (default 45 s). Mono is the clock of that budget (default time.Now); tests inject one.
	ChangeBudget time.Duration
	Mono         func() time.Time
}

// Notifier sends the backup mails. It is safe to run on several app instances:
// rows are taken with FOR UPDATE SKIP LOCKED and the stale alert is claimed with
// one UPDATE.
type Notifier struct {
	pool *pgxpool.Pool
	log  *slog.Logger
	o    Options
}

// New returns a Notifier.
func New(pool *pgxpool.Pool, log *slog.Logger, o Options) *Notifier {
	if o.SendTimeout <= 0 {
		o.SendTimeout = SendTimeout
	}
	if o.ChangeBudget <= 0 {
		o.ChangeBudget = ChangeBudget
	}
	if o.Mono == nil {
		o.Mono = time.Now
	}
	return &Notifier{pool: pool, log: log, o: o}
}

func (n *Notifier) mono() time.Time { return n.o.Mono() }

// MailReady reports whether a mail can go out now.
func (n *Notifier) MailReady() bool { return n.o.Mailer != nil && n.o.Mailer.Ready() }

func (n *Notifier) now(ctx context.Context) (time.Time, error) {
	if n.o.Clock != nil {
		return n.o.Clock().UTC(), nil
	}
	t, err := dbq.New(n.pool).BackupDBNow(ctx)
	if err != nil {
		return time.Time{}, fmt.Errorf("read the database clock: %w", err)
	}
	return t.UTC(), nil
}

// Run ticks at boot and then every interval until ctx ends. A failed tick is
// logged (a fixed word and the error, which holds no address or body) and the
// loop goes on.
func (n *Notifier) Run(ctx context.Context, interval time.Duration) {
	tick := func() {
		if err := n.Tick(ctx); err != nil && ctx.Err() == nil {
			n.log.ErrorContext(ctx, "backup notices failed", "err", err)
		}
	}
	tick()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// Result counts what one tick did.
type Result struct {
	RunMails, ChangeMails, StaleMails int
	SecretsCleared                    int64
}

// Tick is one pass: the test-secret sweep, run notices, change notices and the
// stale alerts. The first error stops the pass; the next tick tries again.
func (n *Notifier) Tick(ctx context.Context) error {
	_, err := n.TickResult(ctx)
	return err
}

// TickResult is Tick with counts (tests).
func (n *Notifier) TickResult(ctx context.Context) (Result, error) {
	var r Result
	now, err := n.now(ctx)
	if err != nil {
		return r, err
	}
	if r.SecretsCleared, err = dbq.New(n.pool).SweepBackupTestSecrets(ctx, now); err != nil {
		return r, fmt.Errorf("sweep test secrets: %w", err)
	}
	if !n.MailReady() {
		return r, nil // nothing is sent and nothing is marked: mail may come back
	}
	if r.RunMails, err = n.noticeRuns(ctx, now); err != nil {
		return r, fmt.Errorf("run notices: %w", err)
	}
	if r.ChangeMails, err = n.noticeChanges(ctx, now); err != nil {
		return r, fmt.Errorf("change notices: %w", err)
	}
	if r.StaleMails, err = n.staleAlerts(ctx, now); err != nil {
		return r, fmt.Errorf("stale alerts: %w", err)
	}
	return r, nil
}

// triggers whose result is mailed (#41): not test, probe or exchange.
func mailTrigger(t string) bool {
	switch t {
	case "schedule", "manual", "catchup", "retry", "verify":
		return true
	}
	return false
}

// link is the Backups page address for this install's org (one instance is one
// org, decisions-T8 #1), or "" when there is no base URL or no org.
func (n *Notifier) link(ctx context.Context, q *dbq.Queries) string {
	if strings.TrimSpace(n.o.PublicBaseURL) == "" {
		return ""
	}
	org, err := q.GetSignupOrg(ctx)
	if err != nil {
		return ""
	}
	return PageLink(n.o.PublicBaseURL, org.Slug)
}

type outMail struct{ subject, text string }

// noticeRuns: AT-MOST-ONCE (#41). The rows are marked in the transaction and the
// mail goes after the commit, so a crash between the two loses that mail and
// never repeats it.
func (n *Notifier) noticeRuns(ctx context.Context, now time.Time) (int, error) {
	var mails []outMail
	err := pgx.BeginTxFunc(ctx, n.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		link := n.link(ctx, q)
		st, err := q.GetBackupSettings(ctx)
		if err != nil {
			return fmt.Errorf("read the backup settings: %w", err)
		}
		rows, err := q.LockUnnotifiedBackupRuns(ctx, now)
		if err != nil {
			return fmt.Errorf("lock the runs: %w", err)
		}
		if len(rows) == 0 {
			return nil
		}
		ids := make([]uuid.UUID, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.ID)
			if !mailTrigger(r.Trigger) || r.FinishedAt == nil {
				continue
			}
			switch {
			case r.Status == "failed" && !r.WillRetry && st.NotifyOnFailure,
				r.Status == "succeeded" && st.NotifyOnSuccess:
				s, t := RunMail(RunNotice{DestinationName: r.DestinationName, Trigger: r.Trigger, Status: r.Status,
					FinishedAt: *r.FinishedAt, Bytes: r.Bytes, ErrorCode: r.ErrorCode, ErrorText: r.ErrorText}, link)
				mails = append(mails, outMail{s, t})
			}
		}
		// Every row looked at is marked, mailed or not, so it is never looked at twice.
		if _, err := q.StampBackupRuns(ctx, dbq.StampBackupRunsParams{Now: &now, Ids: ids}); err != nil {
			return fmt.Errorf("mark the runs: %w", err)
		}
		return nil
	})
	if err != nil || len(mails) == 0 {
		return 0, err
	}
	admins, err := dbq.New(n.pool).ListBackupNoticeAdmins(ctx)
	if err != nil {
		return 0, fmt.Errorf("list the platform admins: %w", err)
	}
	sent := 0
	for _, m := range mails {
		if n.sendAll(ctx, admins, m) > 0 {
			sent++
		}
	}
	return sent, nil
}

// noticeChanges: AT-LEAST-ONCE (#41). The mail goes first, inside the transaction
// that holds the rows; the row is marked after at least one admin got it. A crash
// before the commit sends the same mail again. A row no admin could be mailed for
// (no admin has an address, or every send failed) stays and is tried next tick,
// until the ChangeWindow ends.
func (n *Notifier) noticeChanges(ctx context.Context, now time.Time) (int, error) {
	sent := 0
	err := pgx.BeginTxFunc(ctx, n.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		rows, err := q.LockUnnotifiedBackupChanges(ctx, now)
		if err != nil {
			return fmt.Errorf("lock the changes: %w", err)
		}
		if len(rows) == 0 {
			return nil
		}
		admins, err := q.ListBackupNoticeAdmins(ctx)
		if err != nil {
			return fmt.Errorf("list the platform admins: %w", err)
		}
		if len(admins) == 0 {
			return nil
		}
		link := n.link(ctx, q)
		start := n.mono()
		for _, r := range rows {
			// A dead mail server must not hold this transaction (and the rest of the tick)
			// for 20 rows x admins x SendTimeout: past the budget the remaining rows stay
			// unmarked for the next tick (still at-least-once).
			if n.mono().Sub(start) >= n.o.ChangeBudget {
				break
			}
			actor := ""
			if r.ActorUsername != nil {
				actor = *r.ActorUsername
			}
			s, t := ChangeMail(ChangeNotice{Kind: r.Kind, At: r.At, DestinationName: r.DestinationName, Actor: actor, Detail: r.Detail}, link)
			if n.sendAll(ctx, admins, outMail{s, t}) == 0 {
				continue
			}
			if _, err := q.StampBackupChange(ctx, dbq.StampBackupChangeParams{Now: &now, ID: r.ID}); err != nil {
				return fmt.Errorf("mark a change: %w", err)
			}
			sent++
		}
		return nil
	})
	return sent, err
}

// staleAlerts sends one alert per stale destination per 24 hours (#41). The alert
// is claimed first (one UPDATE; of two instances one wins), then sent: a crash
// loses that alert rather than repeating it every minute.
func (n *Notifier) staleAlerts(ctx context.Context, now time.Time) (int, error) {
	q := dbq.New(n.pool)
	st, err := q.GetBackupSettings(ctx)
	if err != nil {
		return 0, fmt.Errorf("read the backup settings: %w", err)
	}
	if st.StaleAlertHours <= 0 {
		return 0, nil // 0 turns the stale alert off, checked first
	}
	rows, err := q.ListBackupStaleInputs(ctx)
	if err != nil {
		return 0, fmt.Errorf("list the destinations: %w", err)
	}
	link := n.link(ctx, q)
	var due []dbq.ListBackupStaleInputsRow
	for _, r := range rows {
		in := inputOf(r)
		stale, _, _ := Stale(in, int(st.StaleAlertHours), now)
		if !stale {
			continue
		}
		if r.LastStaleAlertAt != nil && now.Sub(*r.LastStaleAlertAt) < AlertEvery {
			continue
		}
		due = append(due, r)
	}
	if len(due) == 0 {
		return 0, nil
	}
	admins, err := q.ListBackupNoticeAdmins(ctx)
	if err != nil {
		return 0, fmt.Errorf("list the platform admins: %w", err)
	}
	if len(admins) == 0 {
		return 0, nil
	}
	sent := 0
	for _, r := range due {
		won, err := q.ClaimBackupStaleAlert(ctx, dbq.ClaimBackupStaleAlertParams{Now: &now, ID: r.ID})
		if err != nil {
			return sent, fmt.Errorf("claim a stale alert: %w", err)
		}
		if won == 0 {
			continue // another instance sent it, or the destination changed
		}
		in := inputOf(r)
		_, since, th := Stale(in, int(st.StaleAlertHours), now)
		s, t := StaleMail(StaleNotice{DestinationName: r.Name, LastSuccess: in.LastSuccess, Since: since, Threshold: th, Now: now}, link)
		if n.sendAll(ctx, admins, outMail{s, t}) > 0 {
			sent++
		}
	}
	return sent, nil
}

// InputOf converts a stale-input row.
func inputOf(r dbq.ListBackupStaleInputsRow) StaleInput {
	in := StaleInput{Schedule: r.Schedule, CreatedAt: r.CreatedAt, EnabledSince: r.EnabledSince}
	if r.HasSuccess {
		t := r.LastSuccess
		in.LastSuccess = &t
	}
	return in
}

// StaleInputOf is the exported form for the status route.
func StaleInputOf(r dbq.ListBackupStaleInputsRow) StaleInput { return inputOf(r) }

// sendAll mails every admin and returns how many sends worked. A failure is
// logged with the fixed public text only: never the address, the subject or the body.
func (n *Notifier) sendAll(ctx context.Context, admins []dbq.ListBackupNoticeAdminsRow, m outMail) int {
	ok := 0
	for _, a := range admins {
		if a.Email == nil || *a.Email == "" {
			continue
		}
		sctx, cancel := context.WithTimeout(ctx, n.o.SendTimeout)
		err := n.o.Mailer.SendText(sctx, *a.Email, m.subject, m.text)
		cancel()
		if err != nil {
			n.log.WarnContext(ctx, "backup notice not sent", "reason", login.PublicMailError(err))
			continue
		}
		ok++
	}
	return ok
}
