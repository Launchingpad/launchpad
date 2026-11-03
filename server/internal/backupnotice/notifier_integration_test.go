package backupnotice_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/backupnotice"
	"devops/internal/dbq"
	"devops/internal/testdb"
)

// LAUN-75: the backup notices against a real Postgres (decisions-T16 #41). Time
// comes from an injected clock; no test reads time.Now.

var t0 = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type sent struct{ to, subject, text string }

// fakeMailer records sends. failFor: addresses whose send fails. failAll: every send fails.
type fakeMailer struct {
	mu      sync.Mutex
	ready   bool
	failAll bool
	failFor map[string]bool
	out     []sent
}

func (m *fakeMailer) Ready() bool             { m.mu.Lock(); defer m.mu.Unlock(); return m.ready }
func (m *fakeMailer) set(f func(*fakeMailer)) { m.mu.Lock(); defer m.mu.Unlock(); f(m) }

func (m *fakeMailer) SendText(_ context.Context, to, subject, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failAll || m.failFor[to] {
		return errors.New("smtp: refused " + to + " " + subject) // would leak if logged raw
	}
	m.out = append(m.out, sent{to, subject, text})
	return nil
}

func (m *fakeMailer) all() []sent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]sent(nil), m.out...)
}

func (m *fakeMailer) count(to, subjectPart string) int {
	n := 0
	for _, s := range m.all() {
		if (to == "" || s.to == to) && strings.Contains(s.subject, subjectPart) {
			n++
		}
	}
	return n
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	clk   *clock
	mail  *fakeMailer
	logs  *lockedBuf
	n     *backupnotice.Notifier
	admin map[string]string // username -> email of the platform admins that must get mail
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool, _ := testdb.New(t)
	e := &env{t: t, pool: pool, clk: &clock{t: t0}, mail: &fakeMailer{ready: true}, logs: &lockedBuf{}}
	e.n = e.newNotifier()
	user := func(name, email string, agent, platform bool, deactivated bool) {
		var em any
		if email != "" {
			em = email
		}
		testdb.MustExec(t, pool, `INSERT INTO users (username, name, email, is_agent, is_platform_admin, deactivated_at)
			VALUES ($1, $1, $2, $3, $4, CASE WHEN $5 THEN now() END)`, name, em, agent, platform, deactivated)
	}
	user("ann", "ann@example.com", false, true, false)
	user("bob", "bob@example.com", false, true, false)
	user("robot", "robot@example.com", true, true, false)     // an agent: never mailed
	user("gone", "gone@example.com", false, true, true)       // deactivated: never mailed
	user("nomail", "", false, true, false)                    // no address: skipped
	user("member", "member@example.com", false, false, false) // not a platform admin
	e.admin = map[string]string{"ann": "ann@example.com", "bob": "bob@example.com"}
	return e
}

func (e *env) newNotifier() *backupnotice.Notifier {
	return backupnotice.New(e.pool, slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		backupnotice.Options{Mailer: e.mail, PublicBaseURL: "https://launchpad.example.com", Clock: e.clk.Now})
}

func (e *env) tick() backupnotice.Result {
	e.t.Helper()
	r, err := e.n.TickResult(context.Background())
	if err != nil {
		e.t.Fatalf("tick: %v", err)
	}
	return r
}

func (e *env) dest(name, schedule string, enabled bool, created time.Time, enabledSince *time.Time) uuid.UUID {
	e.t.Helper()
	id := uuid.Must(uuid.NewV7())
	var weekday any
	if schedule == "weekly" {
		weekday = 1
	}
	testdb.MustExec(e.t, e.pool, `INSERT INTO backup_destinations (id, name, type, config, schedule, weekday, enabled, enabled_since, created_at)
		VALUES ($1, $2, 's3', '{}', $3, $4, $5, $6, $7)`, id, name, schedule, weekday, enabled, enabledSince, created)
	return id
}

type runRow struct {
	dest      *uuid.UUID
	name      string
	trigger   string
	status    string
	willRetry bool
	finished  time.Time
	code      string
	text      string
	bytes     *int64
	secrets   []byte
	queued    *time.Time
}

func (e *env) run(r runRow) uuid.UUID {
	e.t.Helper()
	id := uuid.Must(uuid.NewV7())
	q := r.finished
	if r.queued != nil {
		q = *r.queued
	}
	var fin any
	switch r.status {
	case "succeeded", "failed", "interrupted", "canceled":
		fin = r.finished
	}
	testdb.MustExec(e.t, e.pool, `INSERT INTO backup_runs (id, destination_id, destination_name, trigger, status, will_retry,
		queued_at, finished_at, error_code, error_text, bytes, test_secrets_enc)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		id, r.dest, r.name, r.trigger, r.status, r.willRetry, q, fin, r.code, r.text, r.bytes, r.secrets)
	return id
}

func (e *env) change(kind, destName string, at time.Time, detail string) uuid.UUID {
	e.t.Helper()
	id := uuid.Must(uuid.NewV7())
	testdb.MustExec(e.t, e.pool, `INSERT INTO backup_change_log (id, at, kind, destination_name, detail)
		VALUES ($1, $2, $3, $4, $5::jsonb)`, id, at, kind, destName, detail)
	return id
}

func (e *env) failedRun(name, trigger string) uuid.UUID {
	return e.run(runRow{name: name, trigger: trigger, status: "failed", finished: e.clk.Now().Add(-time.Minute),
		code: "auth_failed", text: "the remote refused the credentials"})
}

func TestRunNotice_FailedRunMailsEveryActivePlatformAdminOnce(t *testing.T) {
	e := newEnv(t)
	e.failedRun("nas", "schedule")
	if r := e.tick(); r.RunMails != 1 {
		t.Fatalf("run mails = %d", r.RunMails)
	}
	e.tick()
	e.tick()
	all := e.mail.all()
	if len(all) != 2 {
		t.Fatalf("%d mails, want 2 (ann and bob): %+v", len(all), all)
	}
	for _, m := range all {
		if m.to != "ann@example.com" && m.to != "bob@example.com" {
			t.Errorf("mailed %s", m.to)
		}
		if !strings.Contains(m.subject, "backup failed: nas") || !strings.Contains(m.text, "Code: auth_failed") {
			t.Errorf("mail: %+v", m)
		}
	}
}

func TestRunNotice_TwoInstancesMailEachRunOnce(t *testing.T) {
	e := newEnv(t)
	const runs = 40
	for i := range runs {
		e.failedRun(fmt.Sprintf("dest-%02d", i), "schedule")
	}
	a, b := e.newNotifier(), e.newNotifier()
	var wg sync.WaitGroup
	for _, n := range []*backupnotice.Notifier{a, b, a, b} {
		wg.Go(func() {
			if _, err := n.TickResult(context.Background()); err != nil {
				t.Errorf("tick: %v", err)
			}
		})
	}
	wg.Wait()
	e.tick() // anything left over
	per := map[string]int{}
	for _, m := range e.mail.all() {
		per[m.to+"|"+m.subject]++
	}
	if len(per) != runs*2 {
		t.Fatalf("%d distinct mails, want %d", len(per), runs*2)
	}
	for k, n := range per {
		if n != 1 {
			t.Errorf("%s sent %d times", k, n)
		}
	}
}

func TestRunNotice_WhichRunsMail(t *testing.T) {
	e := newEnv(t)
	fin := e.clk.Now().Add(-time.Minute)
	size := int64(1234)
	rows := []struct {
		name string
		r    runRow
		mail bool
	}{
		{"fail-schedule", runRow{trigger: "schedule", status: "failed", finished: fin}, true},
		{"fail-manual", runRow{trigger: "manual", status: "failed", finished: fin}, true},
		{"fail-catchup", runRow{trigger: "catchup", status: "failed", finished: fin}, true},
		{"fail-retry", runRow{trigger: "retry", status: "failed", finished: fin}, true},
		{"fail-verify", runRow{trigger: "verify", status: "failed", finished: fin}, true},
		{"fail-willretry", runRow{trigger: "schedule", status: "failed", willRetry: true, finished: fin}, false},
		{"fail-test", runRow{trigger: "test", status: "failed", finished: fin}, false},
		{"fail-probe", runRow{trigger: "probe", status: "failed", finished: fin}, false},
		{"fail-exchange", runRow{trigger: "exchange", status: "failed", finished: fin}, false},
		{"interrupted", runRow{trigger: "schedule", status: "interrupted", finished: fin}, false},
		{"canceled", runRow{trigger: "schedule", status: "canceled", finished: fin}, false},
		{"ok-schedule", runRow{trigger: "schedule", status: "succeeded", finished: fin, bytes: &size}, false}, // success mail is off by default
	}
	for _, c := range rows {
		c.r.name = c.name
		e.run(c.r)
	}
	e.tick()
	for _, c := range rows {
		got := e.mail.count("ann@example.com", ": "+c.name) > 0
		if got != c.mail {
			t.Errorf("%s: mailed = %v, want %v", c.name, got, c.mail)
		}
	}
	// The ones that need no mail are marked, so none is looked at twice.
	var open int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM backup_runs WHERE notified_at IS NULL`).Scan(&open); err != nil || open != 0 {
		t.Fatalf("unmarked runs: %d %v", open, err)
	}
}

func TestRunNotice_SuccessMailOnlyWhenSet_AndFailureMailCanBeOff(t *testing.T) {
	e := newEnv(t)
	size := int64(4096)
	e.run(runRow{name: "ok1", trigger: "schedule", status: "succeeded", finished: e.clk.Now().Add(-time.Minute), bytes: &size})
	e.tick()
	if n := len(e.mail.all()); n != 0 {
		t.Fatalf("success mailed with notify_on_success off: %d", n)
	}
	testdb.MustExec(t, e.pool, `UPDATE backup_settings SET notify_on_success = true, notify_on_failure = false`)
	e.run(runRow{name: "ok2", trigger: "manual", status: "succeeded", finished: e.clk.Now().Add(-time.Minute), bytes: &size})
	e.failedRun("bad2", "schedule")
	e.tick()
	if e.mail.count("", "succeeded: ok2") != 2 {
		t.Fatalf("success mails: %+v", e.mail.all())
	}
	if e.mail.count("", "failed") != 0 {
		t.Fatalf("a failure mailed with notify_on_failure off: %+v", e.mail.all())
	}
	if m := e.mail.all()[0]; !strings.Contains(m.text, "Size sent: 4096 bytes") {
		t.Errorf("text:\n%s", m.text)
	}
	// Turning failure mail back on does not resend an earlier failure (it was looked at).
	testdb.MustExec(t, e.pool, `UPDATE backup_settings SET notify_on_failure = true`)
	e.tick()
	if e.mail.count("", "failed") != 0 {
		t.Fatalf("an old failure was mailed later")
	}
}

func TestRunNotice_AtMostOnce_ACrashBetweenCommitAndSendLosesTheMailAndDoesNotResend(t *testing.T) {
	e := newEnv(t)
	e.failedRun("nas", "schedule")
	e.mail.set(func(m *fakeMailer) { m.failAll = true }) // the send after the commit fails, as a crash would
	e.tick()
	if len(e.mail.all()) != 0 {
		t.Fatal("a send went through")
	}
	e.mail.set(func(m *fakeMailer) { m.failAll = false })
	e.tick()
	e.tick()
	if n := len(e.mail.all()); n != 0 {
		t.Fatalf("a run notice was resent after a lost send: %d", n)
	}
}

func TestRunNotice_MailOffSendsNothingAndMarksNothing(t *testing.T) {
	e := newEnv(t)
	id := e.failedRun("nas", "schedule")
	e.mail.set(func(m *fakeMailer) { m.ready = false })
	e.tick()
	e.clk.Advance(time.Hour)
	e.tick()
	if len(e.mail.all()) != 0 {
		t.Fatal("sent with mail off")
	}
	var marked bool
	if err := e.pool.QueryRow(context.Background(), `SELECT notified_at IS NOT NULL FROM backup_runs WHERE id = $1`, id).Scan(&marked); err != nil || marked {
		t.Fatalf("marked with mail off: %v %v", marked, err)
	}
	// Mail comes back inside the 24 hour window: the notice goes out then.
	e.mail.set(func(m *fakeMailer) { m.ready = true })
	e.tick()
	if e.mail.count("", "failed: nas") != 2 {
		t.Fatalf("mails after mail came back: %+v", e.mail.all())
	}
}

func TestRunNotice_OldRunIsNotMailedWhenMailComesBack(t *testing.T) {
	e := newEnv(t)
	e.failedRun("nas", "schedule")
	e.mail.set(func(m *fakeMailer) { m.ready = false })
	e.tick()
	e.clk.Advance(25 * time.Hour)
	e.mail.set(func(m *fakeMailer) { m.ready = true })
	e.tick()
	if len(e.mail.all()) != 0 {
		t.Fatalf("a day-old run was mailed: %+v", e.mail.all())
	}
}

var allKinds = []string{
	"destination_added", "destination_deleted", "destination_renamed", "destination_config_changed",
	"destination_secrets_changed", "destination_schedule_changed", "destination_retention_changed",
	"destination_enabled", "destination_disabled", "destination_pin_set", "destination_host_key_set",
	"notice_settings_changed", "recipient_added", "recipient_removal_scheduled", "recipient_add_cancelled",
	"recipient_removal_cancelled", "recipient_removed_by_host", "recipient_cancelled_by_host", "signing_key_replaced", "instance_resumed",
}

func TestChangeNotice_EveryKindMailsAllPlatformAdmins(t *testing.T) {
	e := newEnv(t)
	for i, k := range allKinds {
		if !backupnotice.KnownChangeKind(k) {
			t.Errorf("kind %s has no sentence", k)
		}
		e.change(k, "nas", e.clk.Now().Add(-time.Duration(len(allKinds)-i)*time.Second), `{}`)
	}
	// 19 rows, 20 per pass: one tick covers them.
	if r := e.tick(); r.ChangeMails != len(allKinds) {
		t.Fatalf("change mails = %d, want %d", r.ChangeMails, len(allKinds))
	}
	e.tick()
	for to := range map[string]bool{"ann@example.com": true, "bob@example.com": true} {
		if n := e.mail.count(to, "setting changed"); n != len(allKinds) {
			t.Errorf("%s got %d change mails, want %d", to, n, len(allKinds))
		}
	}
	if n := len(e.mail.all()); n != 2*len(allKinds) {
		t.Errorf("%d mails in all, want %d", n, 2*len(allKinds))
	}
}

func TestChangeNotice_AtLeastOnce_ACrashBeforeNotifiedAtResends(t *testing.T) {
	e := newEnv(t)
	id := e.change("recipient_added", "", e.clk.Now().Add(-time.Minute), `{}`)
	e.mail.set(func(m *fakeMailer) { m.failAll = true })
	e.tick()
	var marked bool
	q := func() {
		if err := e.pool.QueryRow(context.Background(), `SELECT notified_at IS NOT NULL FROM backup_change_log WHERE id = $1`, id).Scan(&marked); err != nil {
			t.Fatal(err)
		}
	}
	q()
	if marked {
		t.Fatal("marked although no admin got the mail")
	}
	e.mail.set(func(m *fakeMailer) { m.failAll = false })
	e.tick()
	q()
	if !marked || len(e.mail.all()) != 2 {
		t.Fatalf("after the retry: marked=%v mails=%d", marked, len(e.mail.all()))
	}
	e.tick()
	if len(e.mail.all()) != 2 {
		t.Fatal("sent again after the row was marked")
	}
}

func TestChangeNotice_RolledBackTransactionSendsTheSameMailAgain(t *testing.T) {
	// A mail goes out inside the transaction that holds the row; if the process dies before the
	// commit the row is unmarked and the next tick sends again. Simulate the death with a context
	// that is cancelled by the mailer after its first send.
	e := newEnv(t)
	e.change("destination_added", "nas", e.clk.Now().Add(-time.Minute), `{}`)
	ctx, cancel := context.WithCancel(context.Background())
	e.n = backupnotice.New(e.pool, slog.New(slog.NewTextHandler(e.logs, nil)), backupnotice.Options{
		Mailer: cancelAfterFirst{fakeMailer: e.mail, cancel: cancel}, Clock: e.clk.Now})
	_ = e.n.Tick(ctx) // the commit fails: the context is gone
	first := len(e.mail.all())
	if first == 0 {
		t.Fatal("nothing was sent before the crash")
	}
	e.n = e.newNotifier()
	e.tick()
	if len(e.mail.all()) <= first {
		t.Fatalf("the notice was not sent again after the crash: %d then %d", first, len(e.mail.all()))
	}
	e.tick()
	total := len(e.mail.all())
	e.tick()
	if len(e.mail.all()) != total {
		t.Fatal("sent after the row was marked")
	}
}

type cancelAfterFirst struct {
	*fakeMailer
	cancel func()
}

func (c cancelAfterFirst) SendText(ctx context.Context, to, s, t string) error {
	err := c.fakeMailer.SendText(context.Background(), to, s, t)
	c.cancel()
	return err
}

func TestChangeNotice_OneAdminFailingDoesNotRepeatTheMailForTheOthers(t *testing.T) {
	e := newEnv(t)
	e.mail.set(func(m *fakeMailer) { m.failFor = map[string]bool{"bob@example.com": true} })
	e.change("destination_enabled", "nas", e.clk.Now().Add(-time.Minute), `{}`)
	for range 3 {
		e.tick()
	}
	if e.mail.count("ann@example.com", "setting changed") != 1 {
		t.Fatalf("ann's mails: %+v", e.mail.all())
	}
	if !strings.Contains(e.logs.String(), "backup notice not sent") {
		t.Errorf("a failed send was not logged")
	}
}

func TestChangeNotice_OldRowsAndNoAdminWithAnAddress(t *testing.T) {
	e := newEnv(t)
	e.change("destination_added", "old", e.clk.Now().Add(-8*24*time.Hour), `{}`)
	e.tick()
	if len(e.mail.all()) != 0 {
		t.Fatalf("an 8 day old change was mailed")
	}
	// No platform admin has an address: the row waits (it is not marked), and goes out when one has.
	testdb.MustExec(t, e.pool, `UPDATE users SET email = NULL WHERE is_platform_admin`)
	id := e.change("destination_added", "new", e.clk.Now().Add(-time.Minute), `{}`)
	e.tick()
	var marked bool
	if err := e.pool.QueryRow(context.Background(), `SELECT notified_at IS NOT NULL FROM backup_change_log WHERE id = $1`, id).Scan(&marked); err != nil || marked {
		t.Fatalf("marked with nobody to mail: %v %v", marked, err)
	}
	testdb.MustExec(t, e.pool, `UPDATE users SET email = 'ann@example.com' WHERE username = 'ann'`)
	e.tick()
	if e.mail.count("ann@example.com", "setting changed") != 1 {
		t.Fatalf("mails: %+v", e.mail.all())
	}
}

func TestChangeNotice_TextHasNoHostNoSecret(t *testing.T) {
	e := newEnv(t)
	e.change("destination_config_changed", "nas", e.clk.Now().Add(-time.Minute),
		`{"identity":{"host":"secret-host.internal.example","bucket":"planted-bucket"},"password":"planted-pw-1","url":"https://u:p@h/x"}`)
	e.tick()
	for _, m := range e.mail.all() {
		for _, bad := range []string{"secret-host", "planted-bucket", "planted-pw-1", "u:p@"} {
			if strings.Contains(m.subject+m.text, bad) {
				t.Errorf("mail to %s holds %q", m.to, bad)
			}
		}
	}
}

// --- the stale alert ---------------------------------------------------------

func (e *env) setStale(h int) {
	testdb.MustExec(e.t, e.pool, `UPDATE backup_settings SET stale_alert_hours = $1`, h)
}

func (e *env) succeeded(dest uuid.UUID, name, trigger string, ago time.Duration) {
	e.run(runRow{dest: &dest, name: name, trigger: trigger, status: "succeeded", finished: e.clk.Now().Add(-ago)})
}

func (e *env) staleMails() int { return e.mail.count("ann@example.com", "overdue") }

func TestStale_ScheduleRule(t *testing.T) {
	type tc struct {
		name     string
		schedule string
		hours    int
		ago      time.Duration
		trigger  string
		alert    bool
	}
	for _, c := range []tc{
		{"daily 47h", "daily", 48, 47 * time.Hour, "schedule", false},
		{"daily 49h", "daily", 49 - 1, 49 * time.Hour, "schedule", true},
		{"weekly 60h: no alert at 48 hours", "weekly", 48, 60 * time.Hour, "schedule", false},
		{"weekly after two missed slots", "weekly", 48, 337 * time.Hour, "schedule", true},
		{"long setting wins over the schedule", "daily", 400, 399 * time.Hour, "schedule", false},
		{"manual counts", "daily", 48, 10 * time.Hour, "manual", false},
		{"catchup counts", "daily", 48, 10 * time.Hour, "catchup", false},
		{"retry counts", "daily", 48, 10 * time.Hour, "retry", false},
		{"off", "daily", 0, 5000 * time.Hour, "schedule", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.setStale(c.hours)
			created := e.clk.Now().Add(-10000 * time.Hour)
			id := e.dest("nas", c.schedule, true, created, &created)
			e.succeeded(id, "nas", c.trigger, c.ago)
			e.tick()
			if got := e.staleMails() == 1; got != c.alert {
				t.Fatalf("alert = %v, want %v (mails %+v)", got, c.alert, e.mail.all())
			}
		})
	}
}

func TestStale_TestVerifyExchangeAndProbeRunsDoNotCount(t *testing.T) {
	e := newEnv(t)
	created := e.clk.Now().Add(-10000 * time.Hour)
	id := e.dest("nas", "daily", true, created, &created)
	e.succeeded(id, "nas", "schedule", 100*time.Hour)
	for _, trig := range []string{"test", "probe", "verify", "exchange"} {
		e.succeeded(id, "nas", trig, time.Hour)
	}
	e.tick()
	if e.staleMails() != 1 {
		t.Fatalf("stale mails: %+v", e.mail.all())
	}
}

func TestStale_OncePer24Hours(t *testing.T) {
	e := newEnv(t)
	created := e.clk.Now().Add(-10000 * time.Hour)
	id := e.dest("nas", "daily", true, created, &created)
	e.succeeded(id, "nas", "schedule", 100*time.Hour)
	e.tick()
	e.tick()
	if e.staleMails() != 1 {
		t.Fatalf("first day: %d", e.staleMails())
	}
	e.clk.Advance(23*time.Hour + 59*time.Minute)
	e.tick()
	if e.staleMails() != 1 {
		t.Fatalf("at 23h59: %d", e.staleMails())
	}
	e.clk.Advance(2 * time.Minute)
	e.tick()
	e.tick()
	if e.staleMails() != 2 {
		t.Fatalf("after 24h: %d", e.staleMails())
	}
	if e.mail.count("bob@example.com", "overdue") != 2 {
		t.Fatalf("bob: %+v", e.mail.all())
	}
}

func TestStale_NeverSucceededCountsFromCreationOrEnabling_DisabledNeverAlerts(t *testing.T) {
	e := newEnv(t)
	old := e.clk.Now().Add(-1000 * time.Hour)
	recent := e.clk.Now().Add(-10 * time.Hour)
	e.dest("reenabled", "daily", true, old, &recent)   // enabled 10h ago: not stale
	e.dest("neversucceeded", "daily", true, old, &old) // enabled 1000h ago: stale
	e.dest("disabled", "daily", false, old, &old)      // never alerts
	e.tick()
	if e.mail.count("ann@example.com", "overdue: neversucceeded") != 1 || e.staleMails() != 1 {
		t.Fatalf("mails: %+v", e.mail.all())
	}
	var at *time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT last_stale_alert_at FROM backup_destinations WHERE name = 'disabled'`).Scan(&at); err != nil || at != nil {
		t.Fatalf("disabled destination was claimed: %v %v", at, err)
	}
}

func TestStale_MailOffSendsNothingAndKeepsTheAlertForLater(t *testing.T) {
	e := newEnv(t)
	created := e.clk.Now().Add(-10000 * time.Hour)
	e.dest("nas", "daily", true, created, &created)
	e.mail.set(func(m *fakeMailer) { m.ready = false })
	e.tick()
	var at *time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT last_stale_alert_at FROM backup_destinations`).Scan(&at); err != nil || at != nil {
		t.Fatalf("claimed with mail off: %v %v", at, err)
	}
	e.mail.set(func(m *fakeMailer) { m.ready = true })
	e.tick()
	if e.staleMails() != 1 {
		t.Fatalf("mails: %+v", e.mail.all())
	}
}

func TestStale_TwoInstancesSendOneAlert(t *testing.T) {
	e := newEnv(t)
	created := e.clk.Now().Add(-10000 * time.Hour)
	for i := range 10 {
		e.dest(fmt.Sprintf("nas%d", i), "daily", true, created, &created)
	}
	a, b := e.newNotifier(), e.newNotifier()
	var wg sync.WaitGroup
	for _, n := range []*backupnotice.Notifier{a, b, a, b} {
		wg.Go(func() {
			if _, err := n.TickResult(context.Background()); err != nil {
				t.Errorf("tick: %v", err)
			}
		})
	}
	wg.Wait()
	if n := e.mail.count("ann@example.com", "overdue"); n != 10 {
		t.Fatalf("ann got %d stale mails, want 10", n)
	}
}

// --- safety -----------------------------------------------------------------

func TestMail_NoSecretNoHostNoAddressInLogs(t *testing.T) {
	e := newEnv(t)
	secret := []byte("planted-test-secret-blob-0123456789abcdef0123456789abcdef")
	id := uuid.Must(uuid.NewV7())
	testdb.MustExec(t, e.pool, `UPDATE backup_settings SET stale_alert_hours = 48`)
	testdb.MustExec(t, e.pool, `INSERT INTO backup_runs (id, destination_name, trigger, status, finished_at, queued_at, error_code, error_text,
		test_config, test_secrets_enc, remote_name, steps)
		VALUES ($1, E'nas\r\nBcc: evil@example.com', 'schedule', 'failed', $2, $2, 'auth_failed', 'refused', '{"config":{"url":"https://planted-host.example"}}', $3,
		'launchpad-planted-remote-name', '[{"detail":"planted-step"}]')`, id, e.clk.Now().Add(-time.Minute), secret)
	e.mail.set(func(m *fakeMailer) { m.failFor = map[string]bool{"bob@example.com": true} })
	e.tick()
	for _, m := range e.mail.all() {
		for _, bad := range []string{"planted-host", "planted-step", "planted-remote-name", "planted-test-secret", "\nBcc:", "\r"} {
			if strings.Contains(m.subject+m.text, bad) {
				t.Errorf("mail holds %q:\n%s\n%s", bad, m.subject, m.text)
			}
		}
		if strings.ContainsAny(m.subject, "\r\n") {
			t.Errorf("subject: %q", m.subject)
		}
	}
	log := e.logs.String()
	for _, bad := range []string{"ann@example.com", "bob@example.com", "member@example.com", "refused", "nas", "planted", "auth_failed"} {
		if strings.Contains(log, bad) {
			t.Errorf("log holds %q:\n%s", bad, log)
		}
	}
}

func TestSweep_TestSecretsOlderThanTenMinutesAreCleared(t *testing.T) {
	e := newEnv(t)
	blob := bytes.Repeat([]byte{1}, 40)
	old := e.clk.Now().Add(-11 * time.Minute)
	young := e.clk.Now().Add(-9 * time.Minute)
	mk := func(name string, q time.Time) uuid.UUID {
		return e.run(runRow{name: name, trigger: "test", status: "queued", finished: q, queued: &q, secrets: blob})
	}
	oldID, youngID := mk("old", old), mk("young", young)
	if r := e.tick(); r.SecretsCleared != 1 {
		t.Fatalf("cleared %d", r.SecretsCleared)
	}
	has := func(id uuid.UUID) bool {
		var b []byte
		if err := e.pool.QueryRow(context.Background(), `SELECT test_secrets_enc FROM backup_runs WHERE id = $1`, id).Scan(&b); err != nil {
			t.Fatal(err)
		}
		return b != nil
	}
	if has(oldID) || !has(youngID) {
		t.Fatalf("old kept=%v young kept=%v", has(oldID), has(youngID))
	}
	// The sweep runs even with mail off (it is not a mail).
	e.mail.set(func(m *fakeMailer) { m.ready = false })
	e.clk.Advance(2 * time.Minute)
	if r := e.tick(); r.SecretsCleared != 1 || has(youngID) {
		t.Fatalf("second sweep: %d", r.SecretsCleared)
	}
}

var _ = dbq.New

func TestChangeNotice_TimeBudgetLeavesTheRestForTheNextTick(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	cur := t0
	e.n = backupnotice.New(e.pool, slog.New(slog.NewTextHandler(e.logs, nil)), backupnotice.Options{
		Mailer: e.mail, Clock: e.clk.Now, ChangeBudget: 45 * time.Second,
		Mono: func() time.Time { mu.Lock(); defer mu.Unlock(); cur = cur.Add(20 * time.Second); return cur }, // 20 s per look
	})
	for i := range 5 {
		e.change("destination_added", fmt.Sprintf("d%d", i), e.clk.Now().Add(-time.Duration(10-i)*time.Minute), `{}`)
	}
	if r := e.tick(); r.ChangeMails != 2 {
		t.Fatalf("first tick sent %d change notices, want 2 (budget 45 s, 20 s per look)", r.ChangeMails)
	}
	var open int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM backup_change_log WHERE notified_at IS NULL`).Scan(&open); err != nil || open != 3 {
		t.Fatalf("unmarked rows: %d %v", open, err)
	}
	e.tick()
	e.tick()
	if open := e.mail.count("ann@example.com", "setting changed"); open != 5 {
		t.Fatalf("ann got %d change mails after three ticks, want 5 (once each)", open)
	}
}

func TestMail_LinkHasTheOrgSlugOrNone(t *testing.T) {
	e := newEnv(t)
	e.failedRun("nas", "schedule")
	e.tick()
	for _, m := range e.mail.all() {
		if strings.Contains(m.text, "https://") || !strings.Contains(m.text, "Open Settings, Platform, Backups") {
			t.Fatalf("a link without an org:\n%s", m.text)
		}
	}
	testdb.Fixture(t, e.pool) // org "orbit"
	e.failedRun("nas2", "schedule")
	e.tick()
	found := false
	for _, m := range e.mail.all() {
		if strings.Contains(m.subject, "nas2") {
			found = true
			if !strings.Contains(m.text, "https://launchpad.example.com/orbit/settings/platform") {
				t.Fatalf("link:\n%s", m.text)
			}
		}
	}
	if !found {
		t.Fatal("no mail for nas2")
	}
}
