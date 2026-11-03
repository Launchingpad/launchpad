package notify_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/dbq"
	"devops/internal/login"
	"devops/internal/login/smtptest"
	"devops/internal/notify"
	"devops/internal/tracker"
)

// The outbox sender (LAUN-34, decisions-T13 #19 to #25), with the real
// database, the real consumer and a fake transport.

const testBase = "https://launchpad.example.com"

// lockedBuf is a goroutine-safe log sink.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// fakeMailer records every message. It is a notify.Mailer.
type fakeMailer struct {
	mu      sync.Mutex
	ready   atomic.Bool
	msgs    []login.Message
	err     error         // returned by every send when set
	entered chan struct{} // when set: signals the first send, then waits on release
	release chan struct{}
}

func newFakeMailer() *fakeMailer {
	f := &fakeMailer{}
	f.ready.Store(true)
	return f
}

func (f *fakeMailer) Ready() bool { return f.ready.Load() }

func (f *fakeMailer) SendMessage(_ context.Context, m login.Message) error {
	f.mu.Lock()
	entered, release := f.entered, f.release
	f.mu.Unlock()
	if entered != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, m)
	return f.err
}

func (f *fakeMailer) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeMailer) sent() []login.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.msgs)
}

// clock is the injected clock of the sender.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// sworld is a world with a sender on a fake transport and a fake clock.
type sworld struct {
	*world
	s    *notify.Sender
	m    *fakeMailer
	clk  *clock
	slog *lockedBuf
}

func newSWorld(t *testing.T) *sworld {
	t.Helper()
	return newSWorldBase(t, testBase)
}

func newSWorldBase(t *testing.T, base string) *sworld {
	t.Helper()
	w := newWorld(t)
	sw := &sworld{world: w, m: newFakeMailer(), clk: &clock{t: time.Now().Truncate(time.Microsecond)}, slog: &lockedBuf{}}
	sw.s = sw.newSender(base, sw.m)
	return sw
}

func (sw *sworld) newSender(base string, m notify.Mailer) *notify.Sender {
	s := notify.NewSender(sw.pool, slog.New(slog.NewTextHandler(sw.slog, &slog.HandlerOptions{Level: slog.LevelDebug})), m, base)
	s.SetClock(sw.clk.now)
	return s
}

// run runs the consumer and then puts the rows it made on the fake clock.
// notification_emails.created_at is the database's real now(), while every
// window in the sender reads the fake clock, which starts when the world is
// made. Rows made more than a second after that start were "in the future" of
// the fake clock, so the 2 minute quiet window (clock advanced by 2 min + 1 s)
// had not passed for them: a slow host failed the tests that make many rows
// (LAUN-135). A row cannot be newer than "now", so the row takes the fake now.
func (sw *sworld) run() notify.Result {
	sw.t.Helper()
	res := sw.world.run()
	sw.exec(`UPDATE notification_emails SET created_at = $1 WHERE created_at > $1`, sw.clk.now())
	return res
}

// send runs the consumer, moves the clock past the quiet window and runs one
// sender pass.
func (sw *sworld) send() notify.SendResult {
	sw.t.Helper()
	sw.run()
	sw.clk.advance(notify.QuietWindow + time.Second)
	return sw.pass()
}

// pass runs one sender pass at the current clock.
func (sw *sworld) pass() notify.SendResult {
	sw.t.Helper()
	res, err := sw.s.RunOnce(context.Background())
	if err != nil {
		sw.t.Fatalf("RunOnce: %v", err)
	}
	return res
}

// outRow is one outbox row as the tests compare it.
type outRow struct {
	To       string
	Kind     string
	State    string
	Skip     string
	Attempts int32
	Batch    *uuid.UUID
	LastErr  string
	Next     time.Time
}

func (w *world) outbox() []outRow {
	w.t.Helper()
	rs, err := w.pool.Query(context.Background(), `
		SELECT u.username, n.kind, e.state, coalesce(e.skip_reason, ''), e.attempts, e.batch_id, coalesce(e.last_error, ''), e.next_attempt_at
		FROM notification_emails e
		JOIN notifications n ON n.id = e.notification_id
		JOIN users u ON u.id = e.recipient_id
		ORDER BY e.created_at, e.id`)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rs.Close()
	var out []outRow
	for rs.Next() {
		var r outRow
		if err := rs.Scan(&r.To, &r.Kind, &r.State, &r.Skip, &r.Attempts, &r.Batch, &r.LastErr, &r.Next); err != nil {
			w.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (w *world) countOutbox(state, skip string) int {
	w.t.Helper()
	n := 0
	for _, r := range w.outbox() {
		if r.State == state && r.Skip == skip {
			n++
		}
	}
	return n
}

func (w *world) onlyOutbox() outRow {
	w.t.Helper()
	o := w.outbox()
	if len(o) != 1 {
		w.t.Fatalf("outbox = %+v, want one row", o)
	}
	return o[0]
}

func (w *world) assign(title, to string) tracker.Issue {
	return w.createIssue("chen", tracker.CreateIssueInput{Title: title, Assignee: to})
}

// --- provider none -----------------------------------------------------------

func TestSender_ProviderNoneSendsNothingFailsNothingAndRowsExpire(t *testing.T) {
	sw := newSWorld(t)
	sw.m.ready.Store(false)
	sw.assign("an issue", "alice")
	res := sw.send()
	if res.Mails != 0 || res.Sent != 0 || res.Retried != 0 || res.Dead != 0 || res.Skipped != 0 {
		t.Fatalf("result %+v", res)
	}
	if len(sw.m.sent()) != 0 {
		t.Fatal("a mail was sent with no provider")
	}
	if r := sw.onlyOutbox(); r.State != "pending" || r.Attempts != 0 || r.LastErr != "" {
		t.Fatalf("row %+v, want pending with no attempt", r)
	}
	// 23 hours: still waiting.
	sw.clk.advance(22 * time.Hour)
	sw.pass()
	if r := sw.onlyOutbox(); r.State != "pending" {
		t.Fatalf("row %+v after 23h", r)
	}
	// Past 24 hours: expired, and still nothing sent.
	sw.clk.advance(2 * time.Hour)
	res = sw.pass()
	if res.Expired != 1 {
		t.Fatalf("result %+v", res)
	}
	if r := sw.onlyOutbox(); r.State != "skipped" || r.Skip != "expired" || r.Attempts != 0 {
		t.Fatalf("row %+v, want skipped/expired", r)
	}
	if len(sw.m.sent()) != 0 {
		t.Fatal("a mail was sent")
	}
	// The inbox row is untouched.
	var inbox int
	if err := sw.pool.QueryRow(context.Background(), `SELECT count(*) FROM notifications WHERE inbox`).Scan(&inbox); err != nil || inbox != 1 {
		t.Fatalf("inbox rows %d (%v)", inbox, err)
	}
}

// Mail turned on within the day delivers the recent rows.
func TestSender_MailTurnedOnWithinTheDayDelivers(t *testing.T) {
	sw := newSWorld(t)
	sw.m.ready.Store(false)
	sw.assign("an issue", "alice")
	sw.send()
	sw.m.ready.Store(true)
	sw.clk.advance(time.Hour)
	if res := sw.pass(); res.Mails != 1 {
		t.Fatalf("result %+v", res)
	}
	if r := sw.onlyOutbox(); r.State != "sent" {
		t.Fatalf("row %+v", r)
	}
}

// --- retry and dead letter ---------------------------------------------------

func TestSender_RetryScheduleAndDeadLetter(t *testing.T) {
	sw := newSWorld(t)
	sw.m.setErr(&login.MailError{Msg: "the SMTP server refused RCPT TO (the recipient) (reply code 550)", Cause: errors.New("alice@example.test planted")})
	sw.assign("an issue", "alice")
	sw.run()
	sw.clk.advance(notify.QuietWindow + time.Second)

	var batches []string
	sentCount := func() int { return len(sw.m.sent()) }
	for i, delay := range notify.RetryDelays {
		res := sw.pass()
		if res.Retried != 1 || res.Dead != 0 {
			t.Fatalf("attempt %d: result %+v", i+1, res)
		}
		r := sw.onlyOutbox()
		wantNext := sw.clk.now().Add(delay)
		if r.State != "pending" || int(r.Attempts) != i+1 || !r.Next.Equal(wantNext) {
			t.Fatalf("attempt %d: row %+v, want pending, attempts %d, next %v", i+1, r, i+1, wantNext)
		}
		if r.LastErr != "the SMTP server refused RCPT TO (the recipient) (reply code 550)" {
			t.Fatalf("last_error %q: only the MailError text may be stored", r.LastErr)
		}
		// Not due yet: a pass a second early sends nothing.
		before := sentCount()
		sw.clk.advance(delay - time.Second)
		sw.pass()
		if sentCount() != before {
			t.Fatalf("attempt %d: sent before the retry time", i+1)
		}
		sw.clk.advance(time.Second)
		batches = append(batches, sw.m.sent()[len(sw.m.sent())-1].Headers["Message-ID"])
	}
	// The sixth failure is the last.
	res := sw.pass()
	if res.Dead != 1 || res.Retried != 0 {
		t.Fatalf("last attempt: result %+v", res)
	}
	r := sw.onlyOutbox()
	if r.State != "dead" || r.Attempts != 6 {
		t.Fatalf("row %+v, want dead after 6 attempts", r)
	}
	if sentCount() != 6 {
		t.Fatalf("send attempts = %d, want 6", sentCount())
	}
	// One Message-ID for every attempt: a mail client can fold a copy.
	for _, id := range batches {
		if id != sw.m.sent()[0].Headers["Message-ID"] {
			t.Fatalf("Message-ID changed between attempts: %v", batches)
		}
	}
	// A dead row is never tried again.
	sw.clk.advance(3 * time.Hour)
	sw.pass()
	if sentCount() != 6 {
		t.Fatal("a dead row was sent")
	}
}

func TestSender_FailureThenSuccess(t *testing.T) {
	sw := newSWorld(t)
	sw.m.setErr(&login.MailError{Msg: "could not reach Postmark"})
	sw.assign("an issue", "alice")
	sw.send()
	sw.m.setErr(nil)
	sw.clk.advance(time.Minute)
	if res := sw.pass(); res.Mails != 1 {
		t.Fatalf("result %+v", res)
	}
	r := sw.onlyOutbox()
	if r.State != "sent" || r.Batch == nil || r.LastErr != "" {
		t.Fatalf("row %+v", r)
	}
}

// A non-MailError stores a fixed text, never its own (it may hold an address).
func TestSender_UnknownErrorStoresAFixedText(t *testing.T) {
	sw := newSWorld(t)
	sw.m.setErr(errors.New("dial alice@example.test: refused"))
	sw.assign("an issue", "alice")
	sw.send()
	r := sw.onlyOutbox()
	if r.LastErr == "" || strings.Contains(r.LastErr, "alice") || strings.Contains(r.LastErr, "@") {
		t.Fatalf("last_error %q", r.LastErr)
	}
}

// Mail turned off while a row is claimed: no attempt counts, nothing fails.
func TestSender_NoMailerDuringSendCountsNoAttempt(t *testing.T) {
	sw := newSWorld(t)
	sw.m.setErr(login.ErrNoMailer)
	sw.assign("an issue", "alice")
	res := sw.send()
	if res.Retried != 0 || res.Dead != 0 {
		t.Fatalf("result %+v", res)
	}
	if r := sw.onlyOutbox(); r.State != "pending" || r.Attempts != 0 {
		t.Fatalf("row %+v", r)
	}
	sw.m.setErr(nil)
	if res := sw.pass(); res.Mails != 1 {
		t.Fatalf("result %+v", res)
	}
}

// --- burst collapse and the quiet window -------------------------------------

func TestSender_FiveRowsInTwoMinutesMakeOneMail(t *testing.T) {
	sw := newSWorld(t)
	is := sw.assign("a busy issue", "alice") // 1 row
	for i := range 4 {
		sw.comment("chen", is.Identifier, fmt.Sprintf("c%d", i)) // 4 rows
	}
	sw.run()
	rows := sw.outbox()
	if len(rows) != 5 {
		t.Fatalf("outbox = %d rows, want 5", len(rows))
	}
	// Inside the quiet window: nothing goes out.
	sw.clk.advance(notify.QuietWindow - 10*time.Second)
	if res := sw.pass(); res.Mails != 0 || len(sw.m.sent()) != 0 {
		t.Fatalf("sent inside the quiet window: %+v", res)
	}
	sw.clk.advance(20 * time.Second)
	res := sw.pass()
	if res.Mails != 1 || res.Sent != 5 {
		t.Fatalf("result %+v, want 1 mail of 5 rows", res)
	}
	var batch *uuid.UUID
	for _, r := range sw.outbox() {
		if r.State != "sent" || r.Batch == nil {
			t.Fatalf("row %+v", r)
		}
		if batch != nil && *batch != *r.Batch {
			t.Fatal("rows of one mail have different batch ids")
		}
		batch = r.Batch
	}
	msgs := sw.m.sent()
	if len(msgs) != 1 {
		t.Fatalf("mails = %d", len(msgs))
	}
	// The mail lists the changes in order.
	if !strings.Contains(msgs[0].Text, "assigned this to you") || strings.Count(msgs[0].Text, "New comment") != 4 {
		t.Fatalf("text:\n%s", msgs[0].Text)
	}
	if msgs[0].Headers["Message-ID"] != "<"+batch.String()+"@launchpad.example.com>" {
		t.Fatalf("Message-ID %q is not the batch id", msgs[0].Headers["Message-ID"])
	}
	// The first kind by precedence picks the subject: assigned beats comment.
	if !strings.Contains(msgs[0].Subject, "Assigned to you") {
		t.Fatalf("subject %q", msgs[0].Subject)
	}
}

// A newer row keeps the group waiting, until the oldest is 10 minutes old.
func TestSender_QuietWindowRestartsAndMaxHoldEnds(t *testing.T) {
	sw := newSWorld(t)
	is := sw.assign("an issue", "alice")
	sw.run()
	// Make the first row look old, then a fresh comment arrives.
	sw.exec(`UPDATE notification_emails SET created_at = $1`, sw.clk.now().Add(-9*time.Minute))
	sw.comment("chen", is.Identifier, "fresh")
	sw.run()
	if res := sw.pass(); res.Mails != 0 {
		t.Fatalf("sent while the newest row is inside the window: %+v", res)
	}
	// Now the oldest row is past the 10 minute hold: the group goes out.
	sw.clk.advance(90 * time.Second)
	res := sw.pass()
	if res.Mails != 1 || res.Sent != 2 {
		t.Fatalf("result %+v, want one mail of two rows after MaxHold", res)
	}
}

// Rows of two issues, or of two people, are separate mails.
func TestSender_GroupsAreRecipientAndIssue(t *testing.T) {
	sw := newSWorld(t)
	sw.assign("one", "alice")
	sw.assign("two", "alice")
	sw.assign("three", "bob")
	res := sw.send()
	if res.Mails != 3 {
		t.Fatalf("result %+v, want 3 mails", res)
	}
	to := map[string]int{}
	for _, m := range sw.m.sent() {
		to[m.To]++
	}
	if to["alice@example.test"] != 2 || to["bob@example.test"] != 1 {
		t.Fatalf("recipients %v", to)
	}
}

// --- the rate cap ------------------------------------------------------------

func TestSender_RateCapCountsMailsNotRows(t *testing.T) {
	sw := newSWorld(t)
	// 29 one-row mails and one five-row mail: 30 mails, 34 rows.
	for i := range 29 {
		sw.assign(fmt.Sprintf("issue %d", i), "alice")
	}
	busy := sw.assign("busy", "alice")
	for range 4 {
		sw.comment("chen", busy.Identifier, "c")
	}
	sw.assign("the 31st", "alice")
	res := sw.send()
	if res.Mails != 30 || res.Sent != 34 || res.Skipped != 1 {
		t.Fatalf("result %+v, want 30 mails (34 rows) sent and 1 row skipped", res)
	}
	if sw.countOutbox("skipped", "rate_cap") != 1 || sw.countOutbox("sent", "") != 34 {
		t.Fatalf("outbox states: %+v", sw.outbox())
	}
	// The skipped one still has its inbox row.
	var inbox int
	if err := sw.pool.QueryRow(context.Background(), `SELECT count(*) FROM notifications WHERE inbox AND recipient_id = $1`, sw.user["alice"].UserID).Scan(&inbox); err != nil || inbox != 35 {
		t.Fatalf("inbox rows %d (%v), want 35", inbox, err)
	}
	// Later, under the cap again: the next mail says how many were not emailed,
	// and the one after it does not.
	sw.clk.advance(61 * time.Minute)
	sw.assign("after the hour", "alice")
	if res := sw.send(); res.Mails != 1 {
		t.Fatalf("result %+v", res)
	}
	last := sw.m.sent()[len(sw.m.sent())-1]
	if !strings.Contains(last.Text, "1 more update was not emailed. See your inbox.") {
		t.Fatalf("the next mail lacks the not-emailed line:\n%s", last.Text)
	}
	sw.assign("and again", "alice")
	if res := sw.send(); res.Mails != 1 {
		t.Fatalf("result %+v", res)
	}
	last = sw.m.sent()[len(sw.m.sent())-1]
	if strings.Contains(last.Text, "not emailed") {
		t.Fatalf("the line was repeated:\n%s", last.Text)
	}
}

// --- who is skipped ----------------------------------------------------------

func TestSender_RecipientChecks(t *testing.T) {
	for name, tc := range map[string]struct {
		setup  string
		reason string
	}{
		"unproven":    {`UPDATE users SET email_unproven = true WHERE username = 'alice'`, "unproven"},
		"no address":  {`UPDATE users SET email = NULL WHERE username = 'alice'`, "no_address"},
		"empty email": {`UPDATE users SET email = '' WHERE username = 'alice'`, "no_address"},
		"deactivated": {`UPDATE users SET deactivated_at = now() WHERE username = 'alice'`, "disabled"},
		"archived":    {`UPDATE issues SET archived_at = now()`, "disabled"},
	} {
		t.Run(name, func(t *testing.T) {
			sw := newSWorld(t)
			sw.assign("an issue", "alice")
			sw.run()
			sw.exec(tc.setup) // after the consumer: the sender must check again
			sw.clk.advance(3 * time.Minute)
			res := sw.pass()
			if res.Skipped != 1 || res.Mails != 0 || len(sw.m.sent()) != 0 {
				t.Fatalf("result %+v", res)
			}
			if r := sw.onlyOutbox(); r.State != "skipped" || r.Skip != tc.reason {
				t.Fatalf("row %+v, want skipped/%s", r, tc.reason)
			}
		})
	}
}

func TestSender_AgentRecipientIsSkipped(t *testing.T) {
	sw := newSWorld(t)
	sw.assign("an issue", "alice")
	sw.run()
	sw.exec(`UPDATE users SET is_agent = true WHERE username = 'alice'`)
	sw.clk.advance(3 * time.Minute)
	sw.pass()
	if r := sw.onlyOutbox(); r.State != "skipped" || r.Skip != "disabled" || len(sw.m.sent()) != 0 {
		t.Fatalf("row %+v", r)
	}
}

func TestSender_NoPublicBaseURLMeansNoEmail(t *testing.T) {
	sw := newSWorldBase(t, "")
	sw.assign("an issue", "alice")
	res := sw.send()
	if res.Skipped != 1 || len(sw.m.sent()) != 0 {
		t.Fatalf("result %+v", res)
	}
	if r := sw.onlyOutbox(); r.State != "skipped" || r.Skip != "no_base_url" {
		t.Fatalf("row %+v", r)
	}
}

// --- the preference is checked again ------------------------------------------

func TestSender_PreferenceRecheck(t *testing.T) {
	for name, off := range map[string]string{
		"master switch off": `INSERT INTO notification_user_settings (org_id, user_id, email_enabled) SELECT org_id, user_id, false FROM org_members WHERE user_id = (SELECT id FROM users WHERE username = 'alice')`,
		"event off":         `INSERT INTO notification_settings (org_id, user_id, event, email) SELECT org_id, user_id, 'assigned', 'off' FROM org_members WHERE user_id = (SELECT id FROM users WHERE username = 'alice')`,
		"event off at team": `INSERT INTO notification_settings (org_id, user_id, team_id, event, email) SELECT m.org_id, m.user_id, t.id, 'assigned', 'off' FROM org_members m JOIN teams t ON t.org_id = m.org_id WHERE m.user_id = (SELECT id FROM users WHERE username = 'alice')`,
	} {
		t.Run(name, func(t *testing.T) {
			sw := newSWorld(t)
			sw.assign("an issue", "alice")
			sw.run() // the row is made while email is on
			sw.exec(off)
			sw.clk.advance(3 * time.Minute)
			res := sw.pass()
			if res.Skipped != 1 || len(sw.m.sent()) != 0 {
				t.Fatalf("result %+v", res)
			}
			if r := sw.onlyOutbox(); r.State != "skipped" || r.Skip != "disabled" {
				t.Fatalf("row %+v, want skipped/disabled", r)
			}
			// The inbox row is unaffected.
			var n int
			if err := sw.pool.QueryRow(context.Background(), `SELECT count(*) FROM notifications WHERE inbox`).Scan(&n); err != nil || n != 1 {
				t.Fatalf("inbox rows %d (%v)", n, err)
			}
		})
	}
}

// One kind of a group is off, the other is on: the mail holds the other only.
func TestSender_PartlyDisabledGroupSendsTheRest(t *testing.T) {
	sw := newSWorld(t)
	is := sw.assign("an issue", "alice")
	sw.comment("chen", is.Identifier, "hello")
	sw.run()
	sw.exec(`INSERT INTO notification_settings (org_id, user_id, event, email) SELECT org_id, user_id, 'assigned', 'off' FROM org_members WHERE user_id = (SELECT id FROM users WHERE username = 'alice')`)
	sw.clk.advance(3 * time.Minute)
	res := sw.pass()
	if res.Mails != 1 || res.Sent != 1 || res.Skipped != 1 {
		t.Fatalf("result %+v", res)
	}
	m := sw.m.sent()[0]
	if strings.Contains(m.Text, "assigned this to you") || !strings.Contains(m.Text, "New comment") {
		t.Fatalf("text:\n%s", m.Text)
	}
}

// --- what the mail holds -----------------------------------------------------

func TestSender_MailHeadersBodiesAndNoDescriptionByDefault(t *testing.T) {
	sw := newSWorld(t)
	const desc, body = "DESCRIPTION-SECRET-123", "COMMENT-SECRET-456"
	is := sw.createIssue("chen", tracker.CreateIssueInput{Title: "The <title>", Assignee: "alice", Description: desc})
	sw.comment("chen", is.Identifier, body)
	sw.send()
	msgs := sw.m.sent()
	if len(msgs) != 1 {
		t.Fatalf("mails %d", len(msgs))
	}
	m := msgs[0]
	if m.To != "alice@example.test" || m.Text == "" || m.HTML == "" {
		t.Fatalf("message to %q, text %d bytes, html %d bytes", m.To, len(m.Text), len(m.HTML))
	}
	for _, s := range []string{m.Subject, m.Text, m.HTML} {
		if strings.Contains(s, desc) || strings.Contains(s, body) {
			t.Fatalf("the mail holds the description or a comment:\n%s", s)
		}
	}
	if !strings.Contains(m.Text, is.Identifier) || !strings.Contains(m.Text, "The <title>") ||
		!strings.Contains(m.Text, testBase+"/orbit/issue/"+is.Identifier) || !strings.Contains(m.Text, "Chen") {
		t.Fatalf("text:\n%s", m.Text)
	}
	if strings.Contains(m.HTML, "The <title>") || !strings.Contains(m.HTML, "The &lt;title&gt;") {
		t.Fatalf("the html title is not escaped:\n%s", m.HTML)
	}
	// The headers.
	lu := m.Headers["List-Unsubscribe"]
	if !strings.HasPrefix(lu, "<"+testBase+"/api/v1/notifications/unsubscribe/") || !strings.HasSuffix(lu, ">") {
		t.Fatalf("List-Unsubscribe %q", lu)
	}
	if m.Headers["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" {
		t.Fatalf("List-Unsubscribe-Post %q", m.Headers["List-Unsubscribe-Post"])
	}
	token := strings.TrimSuffix(strings.TrimPrefix(lu, "<"+testBase+"/api/v1/notifications/unsubscribe/"), ">")
	if len(token) != 43 {
		t.Fatalf("token length %d, want 43 (32 random bytes, base64url)", len(token))
	}
	if !strings.Contains(m.Text, testBase+"/unsubscribe/"+token) {
		t.Fatalf("the footer lacks the unsubscribe page link:\n%s", m.Text)
	}
	// Only the hash is stored.
	h := sha256.Sum256([]byte(token))
	var n int
	if err := sw.pool.QueryRow(context.Background(), `SELECT count(*) FROM notification_mail_tokens WHERE token_hash = $1`, h[:]).Scan(&n); err != nil || n != 1 {
		t.Fatalf("hash rows %d (%v)", n, err)
	}
	var leaked int
	if err := sw.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM notification_mail_tokens
		WHERE token_hash = convert_to($1, 'UTF8') OR targets::text LIKE '%' || $1 || '%'`, token).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("the token itself is stored: %d (%v)", leaked, err)
	}
	// The token holds the targets of the mail: assigned (default: global level).
	var targets string
	if err := sw.pool.QueryRow(context.Background(), `SELECT targets::text FROM notification_mail_tokens`).Scan(&targets); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(targets, `"assigned"`) || !strings.Contains(targets, `"comment"`) || strings.Contains(targets, "state_change") {
		t.Fatalf("targets %s", targets)
	}
	var exp time.Time
	if err := sw.pool.QueryRow(context.Background(), `SELECT expires_at FROM notification_mail_tokens`).Scan(&exp); err != nil {
		t.Fatal(err)
	}
	if want := sw.clk.now().Add(notify.TokenTTL); exp.Sub(want).Abs() > time.Second {
		t.Fatalf("token expires %v, want about %v", exp, want)
	}
}

func TestSender_IncludeExcerptAddsAShortExcerpt(t *testing.T) {
	sw := newSWorld(t)
	sw.exec(`INSERT INTO notification_user_settings (org_id, user_id, include_excerpt) SELECT org_id, user_id, true FROM org_members WHERE user_id = (SELECT id FROM users WHERE username = 'alice')`)
	is := sw.assign("an issue", "alice")
	sw.comment("chen", is.Identifier, "EXCERPT-TEXT <b>bold</b> "+strings.Repeat("x", 400))
	sw.send()
	m := sw.m.sent()[0]
	if !strings.Contains(m.Text, "EXCERPT-TEXT") || strings.Contains(m.Text, strings.Repeat("x", 201)) {
		t.Fatalf("text:\n%s", m.Text)
	}
	if strings.Contains(m.HTML, "<b>bold</b>") || !strings.Contains(m.HTML, "&lt;b&gt;bold&lt;/b&gt;") {
		t.Fatalf("the excerpt is not escaped in the html:\n%s", m.HTML)
	}
}

func TestSender_StateChangeLineNamesBothStates(t *testing.T) {
	sw := newSWorld(t)
	sw.exec(`INSERT INTO notification_settings (org_id, user_id, event, email) SELECT org_id, user_id, 'state_change', 'on' FROM org_members WHERE user_id = (SELECT id FROM users WHERE username = 'alice')`)
	is := sw.assign("an issue", "alice")
	sw.run()
	sw.exec(`DELETE FROM notification_emails`) // keep the state change alone
	sw.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{State: str("In Progress")})
	sw.send()
	m := sw.m.sent()[0]
	if !strings.Contains(m.Text, "State changed: ") || !strings.Contains(m.Text, " to In Progress") || !strings.Contains(m.Subject, "State changed") {
		t.Fatalf("subject %q text:\n%s", m.Subject, m.Text)
	}
}

// --- crash recovery, two senders ---------------------------------------------

func TestSender_StuckSendingRowReturnsAfterTenMinutes(t *testing.T) {
	sw := newSWorld(t)
	sw.assign("an issue", "alice")
	sw.run()
	now := sw.clk.advanceTo(notify.QuietWindow + time.Second)
	sw.exec(`UPDATE notification_emails SET state = 'sending', next_attempt_at = $1`, now.Add(-notify.StuckAfter+time.Minute))
	if res := sw.pass(); res.Recovered != 0 || res.Mails != 0 {
		t.Fatalf("a claim younger than 10 minutes was taken back: %+v", res)
	}
	sw.clk.advance(2 * time.Minute)
	res := sw.pass()
	if res.Recovered != 1 || res.Mails != 1 {
		t.Fatalf("result %+v, want the stuck row recovered and sent", res)
	}
}

func (c *clock) advanceTo(d time.Duration) time.Time {
	c.advance(d)
	return c.now()
}

// A second sender on the same database does nothing while the first holds
// the lock, and each row is sent once.
func TestSender_TwoSendersSendEachRowOnce(t *testing.T) {
	sw := newSWorld(t)
	for i := range 12 {
		sw.assign(fmt.Sprintf("issue %d", i), []string{"alice", "bob", "carol"}[i%3])
	}
	sw.run()
	sw.clk.advance(3 * time.Minute)

	// First, deterministically: A is inside a send when B runs.
	sw.m.entered, sw.m.release = make(chan struct{}, 1), make(chan struct{})
	b := sw.newSender(testBase, sw.m)
	done := make(chan notify.SendResult, 1)
	go func() {
		res, err := sw.s.RunOnce(context.Background())
		if err != nil {
			t.Errorf("A: %v", err)
		}
		done <- res
	}()
	<-sw.m.entered
	resB, err := b.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resB.Locked || resB.Mails != 0 {
		t.Fatalf("B ran while A held the lock: %+v", resB)
	}
	close(sw.m.release)
	resA := <-done
	if !resA.Locked || resA.Mails != 12 {
		t.Fatalf("A: %+v", resA)
	}
	if n := len(sw.m.sent()); n != 12 {
		t.Fatalf("mails = %d, want 12", n)
	}
	// The lock is free again.
	if res := sw.pass(); !res.Locked {
		t.Fatalf("the lock was not released: %+v", res)
	}
}

func TestSender_ManySendersInARaceSendEachRowOnce(t *testing.T) {
	sw := newSWorld(t)
	for i := range 20 {
		sw.assign(fmt.Sprintf("issue %d", i), []string{"alice", "bob", "carol"}[i%3])
	}
	sw.run()
	sw.clk.advance(3 * time.Minute)
	var wg sync.WaitGroup
	for range 4 {
		s := sw.newSender(testBase, sw.m)
		wg.Go(func() {
			for range 3 {
				if _, err := s.RunOnce(context.Background()); err != nil {
					t.Errorf("RunOnce: %v", err)
				}
			}
		})
	}
	wg.Wait()
	ids := map[string]int{}
	for _, m := range sw.m.sent() {
		ids[m.Headers["Message-ID"]]++
	}
	if len(sw.m.sent()) != 20 || len(ids) != 20 {
		t.Fatalf("mails %d, distinct Message-IDs %d, want 20 and 20", len(sw.m.sent()), len(ids))
	}
	if n := sw.countOutbox("sent", ""); n != 20 {
		t.Fatalf("sent rows %d", n)
	}
}

// --- the real transports -----------------------------------------------------

func TestSender_ThroughSMTP(t *testing.T) {
	srv := smtptest.Start(t, smtptest.Options{})
	mailer := &login.SMTPMailer{Host: "mail.test", Port: srv.Port, Security: login.SMTPNone,
		FromAddress: "tracker@example.com", FromName: "Launchpad", Dial: srv.Dial, Timeout: 5 * time.Second}
	sw := newSWorld(t)
	sw.s = sw.newSender(testBase, switchOf(mailer))
	sw.assign("an issue", "alice")
	res := sw.send()
	if res.Mails != 1 {
		t.Fatalf("result %+v", res)
	}
	msgs := srv.Messages()
	if len(msgs) != 1 {
		t.Fatalf("smtp messages %d", len(msgs))
	}
	if msgs[0].To[0] != "TO:<alice@example.test>" {
		t.Fatalf("rcpt %v", msgs[0].To)
	}
	pm, err := msgs[0].Parse()
	if err != nil {
		t.Fatal(err)
	}
	batch := sw.onlyOutbox().Batch
	if got := pm.Header.Get("Message-ID"); got != "<"+batch.String()+"@launchpad.example.com>" {
		t.Fatalf("Message-ID %q", got)
	}
	if pm.Header.Get("List-Unsubscribe-Post") != "List-Unsubscribe=One-Click" ||
		!strings.HasPrefix(pm.Header.Get("List-Unsubscribe"), "<"+testBase+"/api/v1/notifications/unsubscribe/") {
		t.Fatalf("headers %v", pm.Header)
	}
	if ct := pm.Header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/alternative") {
		t.Fatalf("content type %q", ct)
	}
	if !bytes.Contains(msgs[0].Data, []byte("text/html")) || !bytes.Contains(msgs[0].Data, []byte("text/plain")) {
		t.Fatal("the mail is not text and html")
	}
}

func switchOf(m login.TextMailer) *login.MailSwitch {
	s := login.NewMailSwitch()
	s.Set(m)
	return s
}

// --- cross-org ---------------------------------------------------------------

// Two orgs, alice in both: each org's mail names its own issue and link, and
// the sender's queries never reach across.
func TestSender_CrossOrg(t *testing.T) {
	sw := newSWorld(t)
	ctx := context.Background()
	org2, err := sw.q.InsertOrg(ctx, dbq.InsertOrgParams{Slug: "other", Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tracker.CreateTeamTx(ctx, sw.q, org2.ID, "OTH", "Other", ""); err != nil {
		t.Fatal(err)
	}
	sw.addMember(org2.ID, "alice")
	sw.addMember(org2.ID, "chen")
	org2Auth := *sw.org
	org2Auth.ID, org2Auth.Slug = org2.ID, "other"
	if _, err := sw.svc.CreateIssue(ctx, &org2Auth, sw.user["chen"], tracker.CreateIssueInput{Team: "OTH", Title: "other issue", Assignee: "alice"}); err != nil {
		t.Fatal(err)
	}
	sw.assign("home issue", "alice")
	sw.run()
	// Alice turns email off in the OTHER org only, after the rows were made.
	sw.exec(`INSERT INTO notification_user_settings (org_id, user_id, email_enabled) VALUES ($1, $2, false)`, org2.ID, sw.user["alice"].UserID)
	sw.clk.advance(3 * time.Minute)
	res := sw.pass()
	if res.Mails != 1 || res.Skipped != 1 {
		t.Fatalf("result %+v, want one mail (home) and one row skipped (other)", res)
	}
	m := sw.m.sent()[0]
	if !strings.Contains(m.Text, "home issue") || strings.Contains(m.Text, "other issue") || !strings.Contains(m.Text, "/orbit/issue/") {
		t.Fatalf("text:\n%s", m.Text)
	}
	var skippedOrg uuid.UUID
	if err := sw.pool.QueryRow(ctx, `SELECT org_id FROM notification_emails WHERE state = 'skipped'`).Scan(&skippedOrg); err != nil || skippedOrg != org2.ID {
		t.Fatalf("skipped row in org %v (%v), want the other org", skippedOrg, err)
	}
	// The token of the mail belongs to the home org.
	var tokOrg uuid.UUID
	if err := sw.pool.QueryRow(ctx, `SELECT org_id FROM notification_mail_tokens`).Scan(&tokOrg); err != nil || tokOrg != sw.org.ID {
		t.Fatalf("token org %v (%v)", tokOrg, err)
	}
}

// --- no secrets ---------------------------------------------------------------

func TestSender_NoTokenAddressOrBodyInLogsEventsOrRows(t *testing.T) {
	sw := newSWorld(t)
	ctx := context.Background()
	const secret = "TOPSECRETBODY-9917"
	sw.m.setErr(&login.MailError{Msg: "could not reach Postmark", Cause: errors.New("alice@example.test " + secret)})
	is := sw.createIssue("chen", tracker.CreateIssueInput{Title: "t " + secret, Assignee: "alice", Description: secret})
	sw.comment("chen", is.Identifier, secret)
	sw.send() // a failed send: logs a line
	sw.m.setErr(nil)
	sw.clk.advance(2 * time.Minute)
	sw.pass() // the retry succeeds

	msgs := sw.m.sent()
	token := strings.TrimSuffix(strings.TrimPrefix(msgs[len(msgs)-1].Headers["List-Unsubscribe"], "<"+testBase+"/api/v1/notifications/unsubscribe/"), ">")
	needles := []string{secret, "@example.test", token, "alice@", "Assigned to you"}
	for _, n := range needles {
		for name, s := range map[string]string{"sender log": sw.slog.String(), "consumer log": sw.logs.String()} {
			if strings.Contains(s, n) {
				t.Errorf("the %s holds %q:\n%s", name, n, s)
			}
		}
	}
	for _, q := range []string{
		`SELECT coalesce(string_agg(payload::text, ' '), '') FROM events`,
		`SELECT coalesce(string_agg(data::text, ' '), '') FROM notifications`,
		`SELECT coalesce(string_agg(coalesce(last_error, ''), ' '), '') FROM notification_emails`,
		`SELECT coalesce(string_agg(key || path || encode(coalesce(response_body, ''::bytea), 'escape'), ' '), '') FROM idempotency_keys`,
	} {
		var s string
		if err := sw.pool.QueryRow(ctx, q).Scan(&s); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		for _, n := range []string{token, "@example.test", "alice@"} {
			if strings.Contains(s, n) {
				t.Errorf("%s holds %q", q, n)
			}
		}
		if !strings.Contains(q, "FROM events") && !strings.Contains(q, "FROM notifications") && strings.Contains(s, secret) {
			t.Errorf("%s holds the secret body text", q)
		}
	}
	// The sender emitted no event of its own.
	var n int
	if err := sw.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE type NOT LIKE 'issue.%' AND type NOT LIKE 'comment.%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("unexpected events %d (%v)", n, err)
	}
}

// LAUN-94: every web link in a notification mail must be a route of the web app.
// testdata/web_routes.txt lists those routes; web/src/mailRoutes.test.tsx checks
// the same file against App.tsx, so server link and web route are pinned together.
func TestSender_MailLinksMatchWebRoutes(t *testing.T) {
	raw, err := os.ReadFile("testdata/web_routes.txt")
	if err != nil {
		t.Fatal(err)
	}
	routes := map[string][]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || strings.HasPrefix(f[0], "#") {
			continue
		}
		routes[f[0]] = strings.Split(strings.Trim(f[1], "/"), "/")
	}
	if len(routes) != 3 {
		t.Fatalf("routes %v", routes)
	}
	matches := func(pattern []string, path string) bool {
		segs := strings.Split(strings.Trim(path, "/"), "/")
		if len(segs) != len(pattern) {
			return false
		}
		for i, p := range pattern {
			if segs[i] == "" || (!strings.HasPrefix(p, ":") && p != segs[i]) {
				return false
			}
		}
		return true
	}

	sw := newSWorld(t)
	is := sw.assign("a link check", "alice")
	sw.send()
	msgs := sw.m.sent()
	if len(msgs) != 1 {
		t.Fatalf("mails %d", len(msgs))
	}
	found := map[string]bool{}
	hrefs := regexp.MustCompile(`href="([^"]+)"`)
	var links []string
	for _, m := range hrefs.FindAllStringSubmatch(msgs[0].HTML, -1) {
		links = append(links, html.UnescapeString(m[1]))
	}
	links = append(links, regexp.MustCompile(`https?://[^\s<>"]+`).FindAllString(msgs[0].Text, -1)...)
	for _, l := range links {
		u, err := url.Parse(l)
		if err != nil || !strings.HasPrefix(l, testBase+"/") {
			t.Fatalf("link %q is not on the public base URL", l)
		}
		hit := ""
		for name, pat := range routes {
			if matches(pat, u.Path) {
				hit = name
			}
		}
		if hit == "" {
			t.Errorf("link %q matches no web route in testdata/web_routes.txt", l)
		}
		found[hit] = true
		if hit == "issue" && !strings.HasSuffix(u.Path, "/"+is.Identifier) {
			t.Errorf("issue link %q does not end in %s", l, is.Identifier)
		}
	}
	for name := range routes {
		if !found[name] {
			t.Errorf("the mail has no %s link", name)
		}
	}
}

// LAUN-135: real time that passes between the start of the world and the
// writes must not change what the fake clock says about the quiet window.
func TestSender_SlowWritesStayInsideTheFakeClock(t *testing.T) {
	sw := newSWorld(t)
	time.Sleep(1500 * time.Millisecond) // a slow host, made certain
	sw.assign("an issue", "alice")
	if res := sw.send(); res.Mails != 1 || res.Sent != 1 {
		t.Fatalf("result %+v, want one mail", res)
	}
}
