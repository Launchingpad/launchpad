package notify_test

import (
	"context"
	"testing"
	"time"

	"devops/internal/dbq"
	"devops/internal/notify"
	"devops/internal/testdb"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRetentionConstants(t *testing.T) {
	day := 24 * time.Hour
	for name, c := range map[string][2]time.Duration{
		"read":  {notify.ReadRetention, 90 * day},
		"all":   {notify.AllRetention, 180 * day},
		"sent":  {notify.SentEmailRetention, 30 * day},
		"dead":  {notify.DeadEmailRetention, 90 * day},
		"token": {notify.TokenRetention, 7 * day},
	} {
		if c[0] != c[1] {
			t.Errorf("%s retention = %v, want %v (decisions-T13 #10)", name, c[0], c[1])
		}
	}
}

// TestPrune: read or archived rows go after 90 days, every row after 180,
// sent and skipped outbox rows after 30, dead ones after 90, tokens 7 days
// after expiry. Fresh rows stay.
func TestPrune(t *testing.T) {
	pool, _ := testdb.New(t)
	d := testdb.Fixture(t, pool)
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var team, state, user string
	if err := pool.QueryRow(ctx, `SELECT t.id::text, (SELECT id::text FROM workflow_states WHERE team_id = t.id LIMIT 1), (SELECT user_id::text FROM org_members LIMIT 1) FROM teams t`).Scan(&team, &state, &user); err != nil {
		t.Fatal(err)
	}
	org := d.OrgID.String()
	var issue string
	if err := pool.QueryRow(ctx, `INSERT INTO issues (org_id, team_id, number, title, state_id) VALUES ($1, $2, 99, 'x', $3) RETURNING id::text`, org, team, state).Scan(&issue); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// name -> (age in days, read, archived)
	type row struct {
		key       string
		age       int
		read      bool
		archived  bool
		wantAlive bool
	}
	rows := []row{
		{"fresh-unread", 1, false, false, true},
		{"old-unread-100", 100, false, false, true},
		{"old-read-100", 100, true, false, false},
		{"old-archived-100", 100, false, true, false},
		{"young-read-80", 80, true, false, true},
		{"old-unread-200", 200, false, false, false},
	}
	for _, r := range rows {
		exec(`INSERT INTO notifications (org_id, recipient_id, kind, issue_id, dedupe_key, inbox, created_at, read_at, archived_at)
			VALUES ($1, $2, 'comment', $3, $4, true, $5, CASE WHEN $6 THEN now() END, CASE WHEN $7 THEN now() END)`,
			org, user, issue, r.key, now.Add(-time.Duration(r.age)*24*time.Hour), r.read, r.archived)
	}
	// Outbox rows hang on their own notifications (fresh, kept).
	mk := func(key, state string, ageDays int) {
		exec(`INSERT INTO notifications (org_id, recipient_id, kind, issue_id, dedupe_key, inbox) VALUES ($1, $2, 'comment', $3, $4, false)`, org, user, issue, key)
		exec(`INSERT INTO notification_emails (org_id, notification_id, recipient_id, issue_id, state, sent_at, batch_id, created_at)
			SELECT $1, id, $2, $3, $5, CASE WHEN $5 = 'sent' THEN now() END, CASE WHEN $5 = 'sent' THEN uuidv7() END, $6
			FROM notifications WHERE dedupe_key = $4`, org, user, issue, key, state, now.Add(-time.Duration(ageDays)*24*time.Hour))
	}
	mk("e-sent-40", "sent", 40)
	mk("e-sent-10", "sent", 10)
	mk("e-skipped-40", "skipped", 40)
	mk("e-dead-40", "dead", 40)
	mk("e-dead-100", "dead", 100)
	mk("e-pending-100", "pending", 100)
	tok := func(h string, expiredDaysAgo int) {
		exec(`INSERT INTO notification_mail_tokens (token_hash, org_id, user_id, targets, expires_at) VALUES ($1, $2, $3, '[1]', $4)`,
			[]byte(h), org, user, now.Add(-time.Duration(expiredDaysAgo)*24*time.Hour))
	}
	tok("t-8", 8)
	tok("t-6", 6)

	res, err := notify.Prune(ctx, dbq.New(pool), now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Notifications != 3 || res.Emails != 3 || res.Tokens != 1 {
		t.Errorf("pruned %+v, want 3 notifications, 3 emails, 1 token", res)
	}
	alive := func(q string, args ...any) bool {
		var n int
		if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	for _, r := range rows {
		if got := alive(`SELECT count(*) FROM notifications WHERE dedupe_key = $1`, r.key); got != r.wantAlive {
			t.Errorf("%s alive = %v, want %v", r.key, got, r.wantAlive)
		}
	}
	for key, want := range map[string]bool{"e-sent-40": false, "e-sent-10": true, "e-skipped-40": false, "e-dead-40": true, "e-dead-100": false, "e-pending-100": true} {
		if got := alive(`SELECT count(*) FROM notification_emails e JOIN notifications n ON n.id = e.notification_id WHERE n.dedupe_key = $1`, key); got != want {
			t.Errorf("outbox %s alive = %v, want %v", key, got, want)
		}
	}
	// An outbox row goes with its pruned notification by cascade.
	exec(`INSERT INTO notifications (org_id, recipient_id, kind, issue_id, dedupe_key, inbox, created_at, read_at) VALUES ($1, $2, 'comment', $3, 'cascade', false, $4, now())`, org, user, issue, now.Add(-100*24*time.Hour))
	exec(`INSERT INTO notification_emails (org_id, notification_id, recipient_id, issue_id) SELECT $1, id, $2, $3 FROM notifications WHERE dedupe_key = 'cascade'`, org, user, issue)
	if _, err := notify.Prune(ctx, dbq.New(pool), now); err != nil {
		t.Fatal(err)
	}
	if alive(`SELECT count(*) FROM notification_emails e WHERE NOT EXISTS (SELECT 1 FROM notifications n WHERE n.id = e.notification_id)`) {
		t.Error("orphan outbox row")
	}
	if alive(`SELECT count(*) FROM notifications WHERE dedupe_key = 'cascade'`) || alive(`SELECT count(*) FROM notification_emails WHERE issue_id = $1 AND created_at > now() - interval '1 minute' AND state = 'pending' AND notification_id IN (SELECT id FROM notifications WHERE dedupe_key = 'cascade')`, issue) {
		t.Error("cascade row survived")
	}
	if alive(`SELECT count(*) FROM notification_mail_tokens WHERE token_hash = 't-8'::bytea`) || !alive(`SELECT count(*) FROM notification_mail_tokens WHERE token_hash = 't-6'::bytea`) {
		t.Error("token retention wrong")
	}
}

// retentionFixture returns the pool, org, user and an issue to hang rows on.
func retentionFixture(t *testing.T) (*pgxpool.Pool, string, string, string) {
	t.Helper()
	pool, _ := testdb.New(t)
	d := testdb.Fixture(t, pool)
	ctx := context.Background()
	var team, state, user, issue string
	if err := pool.QueryRow(ctx, `SELECT t.id::text, (SELECT id::text FROM workflow_states WHERE team_id = t.id LIMIT 1), (SELECT user_id::text FROM org_members LIMIT 1) FROM teams t`).Scan(&team, &state, &user); err != nil {
		t.Fatal(err)
	}
	org := d.OrgID.String()
	if err := pool.QueryRow(ctx, `INSERT INTO issues (org_id, team_id, number, title, state_id) VALUES ($1, $2, 99, 'x', $3) RETURNING id::text`, org, team, state).Scan(&issue); err != nil {
		t.Fatal(err)
	}
	return pool, org, user, issue
}

func count(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestPrune_WindowEdges: a row one minute inside each window stays, one
// minute outside goes, with an injected clock.
func TestPrune_WindowEdges(t *testing.T) {
	pool, org, user, issue := retentionFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	m := time.Minute
	ins := func(key string, age time.Duration, read bool) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO notifications (org_id, recipient_id, kind, issue_id, dedupe_key, inbox, created_at, read_at)
			VALUES ($1, $2, 'comment', $3, $4, true, $5::timestamptz, CASE WHEN $6::boolean THEN $5::timestamptz END)`, org, user, issue, key, now.Add(-age), read); err != nil {
			t.Fatal(err)
		}
	}
	ins("read-in", notify.ReadRetention-m, true)
	ins("read-out", notify.ReadRetention+m, true)
	ins("unread-in", notify.AllRetention-m, false)
	ins("unread-out", notify.AllRetention+m, false)
	ins("unread-100d", 100*24*time.Hour, false) // unread and younger than 180 days: stays
	mkEmail := func(key, st string, age time.Duration) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO notifications (org_id, recipient_id, kind, issue_id, dedupe_key, inbox, created_at) VALUES ($1, $2, 'comment', $3, $4, false, $5)`, org, user, issue, key, now); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO notification_emails (org_id, notification_id, recipient_id, issue_id, state, sent_at, batch_id, created_at)
			SELECT $1, id, $2, $3, $5, CASE WHEN $5 = 'sent' THEN $6::timestamptz END, CASE WHEN $5 = 'sent' THEN uuidv7() END, $6
			FROM notifications WHERE dedupe_key = $4`, org, user, issue, key, st, now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	mkEmail("sent-in", "sent", notify.SentEmailRetention-m)
	mkEmail("sent-out", "sent", notify.SentEmailRetention+m)
	mkEmail("skipped-out", "skipped", notify.SentEmailRetention+m)
	mkEmail("dead-in", "dead", notify.DeadEmailRetention-m)
	mkEmail("dead-out", "dead", notify.DeadEmailRetention+m)
	tok := func(h string, age time.Duration) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO notification_mail_tokens (token_hash, org_id, user_id, targets, expires_at) VALUES ($1, $2, $3, '[1]', $4)`,
			[]byte(h), org, user, now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	tok("tok-in", notify.TokenRetention-m)
	tok("tok-out", notify.TokenRetention+m)

	if _, locked, err := notify.PruneOnce(ctx, pool, now); err != nil || !locked {
		t.Fatalf("locked=%v err=%v", locked, err)
	}
	for key, want := range map[string]int{"read-in": 1, "read-out": 0, "unread-in": 1, "unread-out": 0, "unread-100d": 1} {
		if got := count(t, pool, `SELECT count(*) FROM notifications WHERE dedupe_key = $1`, key); got != want {
			t.Errorf("notification %s: %d rows, want %d", key, got, want)
		}
	}
	for key, want := range map[string]int{"sent-in": 1, "sent-out": 0, "skipped-out": 0, "dead-in": 1, "dead-out": 0} {
		if got := count(t, pool, `SELECT count(*) FROM notification_emails e JOIN notifications n ON n.id = e.notification_id WHERE n.dedupe_key = $1`, key); got != want {
			t.Errorf("outbox %s: %d rows, want %d", key, got, want)
		}
	}
	for h, want := range map[string]int{"tok-in": 1, "tok-out": 0} {
		if got := count(t, pool, `SELECT count(*) FROM notification_mail_tokens WHERE token_hash = $1`, []byte(h)); got != want {
			t.Errorf("token %s: %d rows, want %d", h, got, want)
		}
	}
	// A second run changes nothing (idempotent).
	res, _, err := notify.PruneOnce(ctx, pool, now)
	if err != nil || res != (notify.PruneResult{}) {
		t.Errorf("second run = %+v, %v; want nothing", res, err)
	}
}

// TestPrune_TenThousandRowsInBatches: 10 000 old rows all go, and the
// statement is bounded by RetentionBatch (so there are several).
func TestPrune_TenThousandRowsInBatches(t *testing.T) {
	pool, org, user, issue := retentionFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `INSERT INTO notifications (org_id, recipient_id, kind, issue_id, dedupe_key, inbox, created_at)
		SELECT $1, $2, 'comment', $3, 'old-' || g, true, $4 FROM generate_series(1, 10000) g`, org, user, issue, now.Add(-200*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO notifications (org_id, recipient_id, kind, issue_id, dedupe_key, inbox, created_at) VALUES ($1, $2, 'comment', $3, 'young', true, $4)`, org, user, issue, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	q := dbq.New(pool)
	n, err := q.PruneNotifications(ctx, dbq.PruneNotificationsParams{
		ReadCutoff: now.Add(-notify.ReadRetention), AllCutoff: now.Add(-notify.AllRetention), BatchSize: notify.RetentionBatch,
	})
	if err != nil || n != notify.RetentionBatch {
		t.Fatalf("one statement deleted %d (err %v), want exactly %d", n, err, notify.RetentionBatch)
	}
	res, err := notify.Prune(ctx, q, now)
	if err != nil || res.Notifications != 10000-notify.RetentionBatch {
		t.Fatalf("prune = %+v, %v", res, err)
	}
	if got := count(t, pool, `SELECT count(*) FROM notifications`); got != 1 {
		t.Errorf("%d rows left, want only the young one", got)
	}
}

// TestPruneOnce_SecondInstanceSkips: while another session holds the lock,
// a run deletes nothing and reports locked=false; after release it runs.
func TestPruneOnce_SecondInstanceSkips(t *testing.T) {
	pool, org, user, issue := retentionFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `INSERT INTO notifications (org_id, recipient_id, kind, issue_id, dedupe_key, inbox, created_at) VALUES ($1, $2, 'comment', $3, 'old', true, $4)`, org, user, issue, now.Add(-200*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	var got bool
	if err := holder.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended('devops:notify:retention', 0))`).Scan(&got); err != nil || !got {
		t.Fatalf("holder lock = %v, %v", got, err)
	}
	res, locked, err := notify.PruneOnce(ctx, pool, now)
	if err != nil || locked || res != (notify.PruneResult{}) {
		t.Fatalf("while held: %+v locked=%v err=%v", res, locked, err)
	}
	if count(t, pool, `SELECT count(*) FROM notifications`) != 1 {
		t.Fatal("a run without the lock deleted a row")
	}
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_unlock(hashtextextended('devops:notify:retention', 0))`); err != nil {
		t.Fatal(err)
	}
	res, locked, err = notify.PruneOnce(ctx, pool, now)
	if err != nil || !locked || res.Notifications != 1 {
		t.Fatalf("after release: %+v locked=%v err=%v", res, locked, err)
	}
	// The lock is free again after a run (no leaked session lock).
	if err := holder.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended('devops:notify:retention', 0))`).Scan(&got); err != nil || !got {
		t.Errorf("lock leaked after a run: %v, %v", got, err)
	}
}
