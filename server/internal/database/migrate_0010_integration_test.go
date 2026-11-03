package database_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"devops/internal/testdb"
)

// TestMigration0010_Notifications checks 0010 (LAUN-31): the cursor seed (at
// each org's max event seq, and a trigger for new orgs), composite FKs, both
// partial unique indexes, the dedupe key, the CHECKs, NOT NULL enums, a
// claimable minimal outbox row, cascades, and the down.
func TestMigration0010_Notifications(t *testing.T) {
	dsn := testdb.Fresh(t)
	m := migratorFor(t, dsn)
	migrateTo(t, m, 9)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	one := func(q string, args ...any) string {
		t.Helper()
		var s string
		if err := conn.QueryRow(ctx, q, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return s
	}
	exec := func(q string, args ...any) error {
		_, err := conn.Exec(ctx, q, args...)
		return err
	}
	mustExec := func(q string, args ...any) {
		t.Helper()
		if err := exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustFail := func(what, q string, args ...any) {
		t.Helper()
		if err := exec(q, args...); err == nil {
			t.Errorf("%s: accepted, want a constraint error", what)
		}
	}
	count := func(q string, args ...any) string { return one(`SELECT count(*)::text FROM (`+q+`) x`, args...) }

	orgA := one(`INSERT INTO orgs (slug, name) VALUES ('alpha-org', 'alpha') RETURNING id::text`)
	orgB := one(`INSERT INTO orgs (slug, name) VALUES ('beta-org', 'beta') RETURNING id::text`)
	orgC := one(`INSERT INTO orgs (slug, name) VALUES ('gamma-org', 'gamma') RETURNING id::text`)
	for _, s := range []int{1, 2, 7} {
		mustExec(`INSERT INTO events (org_id, seq, type, entity_type, entity_id) VALUES ($1, $2, 'x', 'issue', uuidv7())`, orgA, s)
	}
	mustExec(`INSERT INTO events (org_id, seq, type, entity_type, entity_id) VALUES ($1, 3, 'x', 'issue', uuidv7())`, orgB)

	migrateTo(t, m, 10)

	for org, want := range map[string]string{orgA: "7", orgB: "3", orgC: "0"} {
		if got := one(`SELECT last_seq::text FROM notification_cursors WHERE org_id = $1`, org); got != want {
			t.Errorf("seeded cursor = %s, want %s", got, want)
		}
	}
	orgD := one(`INSERT INTO orgs (slug, name) VALUES ('delta-org', 'delta') RETURNING id::text`)
	if got := one(`SELECT last_seq::text FROM notification_cursors WHERE org_id = $1`, orgD); got != "0" {
		t.Errorf("new org cursor = %s, want 0 (trigger)", got)
	}

	// Two orgs with a team, an issue, and members.
	user := func(name string) string {
		return one(`INSERT INTO users (username, name) VALUES ($1, $1) RETURNING id::text`, name)
	}
	u1, u2 := user("one"), user("two")
	team := func(o, key string) string {
		return one(`INSERT INTO teams (org_id, key, name) VALUES ($1, $2, $2) RETURNING id::text`, o, key)
	}
	tA, tB := team(orgA, "AAA"), team(orgB, "BBB")
	tA2 := team(orgA, "AAB")
	st := func(o, tm string) string {
		return one(`INSERT INTO workflow_states (org_id, team_id, name, type) VALUES ($1, $2, 'Todo', 'unstarted') RETURNING id::text`, o, tm)
	}
	sA, sB := st(orgA, tA), st(orgB, tB)
	issue := func(o, tm, s string, n int) string {
		return one(`INSERT INTO issues (org_id, team_id, number, title, state_id) VALUES ($1, $2, $3, 'x', $4) RETURNING id::text`, o, tm, n, s)
	}
	iA, iB := issue(orgA, tA, sA, 1), issue(orgB, tB, sB, 1)
	mustExec(`INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member'), ($1, $3, 'member')`, orgA, u1, u2)

	// notification_user_settings
	mustExec(`INSERT INTO notification_user_settings (org_id, user_id) VALUES ($1, $2)`, orgA, u1)
	if got := one(`SELECT email_enabled::text || include_excerpt::text || delivery FROM notification_user_settings`); got != "truefalseimmediate" {
		t.Errorf("user settings defaults = %s", got)
	}
	mustFail("delivery hourly", `INSERT INTO notification_user_settings (org_id, user_id, delivery) VALUES ($1, $2, 'hourly')`, orgA, u2)
	mustFail("user settings for a non-member", `INSERT INTO notification_user_settings (org_id, user_id) VALUES ($1, $2)`, orgB, u1)

	// notification_settings: partial unique indexes, CHECKs, composite team FK.
	mustExec(`INSERT INTO notification_settings (org_id, user_id, event, email) VALUES ($1, $2, 'comment', 'off')`, orgA, u1)
	mustFail("duplicate global row", `INSERT INTO notification_settings (org_id, user_id, event, inbox) VALUES ($1, $2, 'comment', 'on')`, orgA, u1)
	mustExec(`INSERT INTO notification_settings (org_id, user_id, event, email) VALUES ($1, $2, 'mention', 'off')`, orgA, u1)
	mustExec(`INSERT INTO notification_settings (org_id, user_id, team_id, event, email) VALUES ($1, $2, $3, 'comment', 'on')`, orgA, u1, tA)
	mustFail("duplicate team row", `INSERT INTO notification_settings (org_id, user_id, team_id, event, inbox) VALUES ($1, $2, $3, 'comment', 'on')`, orgA, u1, tA)
	mustExec(`INSERT INTO notification_settings (org_id, user_id, team_id, event, email) VALUES ($1, $2, $3, 'comment', 'on')`, orgA, u1, tA2)
	mustFail("team of another org", `INSERT INTO notification_settings (org_id, user_id, team_id, event) VALUES ($1, $2, $3, 'comment')`, orgA, u2, tB)
	mustFail("unknown event", `INSERT INTO notification_settings (org_id, user_id, event) VALUES ($1, $2, 'bogus')`, orgA, u2)
	mustFail("bad email value", `INSERT INTO notification_settings (org_id, user_id, event, email) VALUES ($1, $2, 'comment', 'maybe')`, orgA, u2)
	mustFail("bad inbox value", `INSERT INTO notification_settings (org_id, user_id, event, inbox) VALUES ($1, $2, 'comment', 'maybe')`, orgA, u2)
	mustFail("NULL event", `INSERT INTO notification_settings (org_id, user_id, event) VALUES ($1, $2, NULL)`, orgA, u2)

	// team_follows, issue_subscriptions
	mustExec(`INSERT INTO team_follows (org_id, user_id, team_id) VALUES ($1, $2, $3)`, orgA, u1, tA)
	mustFail("duplicate follow", `INSERT INTO team_follows (org_id, user_id, team_id) VALUES ($1, $2, $3)`, orgA, u1, tA)
	mustFail("follow of another org's team", `INSERT INTO team_follows (org_id, user_id, team_id) VALUES ($1, $2, $3)`, orgA, u1, tB)
	mustExec(`INSERT INTO issue_subscriptions (org_id, issue_id, user_id, state, reason) VALUES ($1, $2, $3, 'muted', 'manual')`, orgA, iA, u1)
	mustFail("subscription state", `INSERT INTO issue_subscriptions (org_id, issue_id, user_id, state, reason) VALUES ($1, $2, $3, 'x', 'manual')`, orgA, iA, u2)
	mustFail("subscription reason", `INSERT INTO issue_subscriptions (org_id, issue_id, user_id, state, reason) VALUES ($1, $2, $3, 'muted', 'x')`, orgA, iA, u2)
	mustFail("subscription to another org's issue", `INSERT INTO issue_subscriptions (org_id, issue_id, user_id, state, reason) VALUES ($1, $2, $3, 'muted', 'manual')`, orgA, iB, u2)

	// notifications
	ins := func(recipient, key string) error {
		return exec(`INSERT INTO notifications (org_id, recipient_id, kind, issue_id, dedupe_key, inbox) VALUES ($1, $2, 'comment', $3, $4, true)`,
			orgA, recipient, iA, key)
	}
	if err := ins(u1, "k1"); err != nil {
		t.Fatal(err)
	}
	if err := ins(u1, "k1"); err == nil {
		t.Error("duplicate dedupe key accepted")
	}
	if err := ins(u2, "k1"); err != nil {
		t.Errorf("same key for another recipient: %v", err)
	}
	mustFail("notification for another org's issue", `INSERT INTO notifications (org_id, recipient_id, kind, issue_id, dedupe_key, inbox) VALUES ($1, $2, 'comment', $3, 'z', true)`, orgA, u1, iB)
	mustFail("unknown kind", `INSERT INTO notifications (org_id, recipient_id, kind, issue_id, dedupe_key, inbox) VALUES ($1, $2, 'bogus', $3, 'z', true)`, orgA, u1, iA)
	mustFail("NULL kind", `INSERT INTO notifications (org_id, recipient_id, kind, issue_id, dedupe_key, inbox) VALUES ($1, $2, NULL, $3, 'z', true)`, orgA, u1, iA)
	mustFail("NULL inbox", `INSERT INTO notifications (org_id, recipient_id, kind, issue_id, dedupe_key, inbox) VALUES ($1, $2, 'comment', $3, 'z', NULL)`, orgA, u1, iA)
	if got := count(`SELECT 1 FROM notifications WHERE inbox_seq IS NOT NULL`); got != "2" {
		t.Errorf("inbox_seq set on %s rows, want 2", got)
	}
	mustFail("inbox_seq insert", `INSERT INTO notifications (org_id, recipient_id, kind, issue_id, dedupe_key, inbox, inbox_seq) VALUES ($1, $2, 'comment', $3, 'q', true, 5)`, orgA, u1, iA)
	if got := one(`SELECT data::text FROM notifications LIMIT 1`); got != "{}" {
		t.Errorf("data default = %s", got)
	}

	// notification_emails: a minimal insert is claimable.
	nid := one(`SELECT id::text FROM notifications WHERE recipient_id = $1`, u1)
	mustExec(`INSERT INTO notification_emails (org_id, notification_id, recipient_id, issue_id) VALUES ($1, $2, $3, $4)`, orgA, nid, u1, iA)
	if got := count(`SELECT 1 FROM notification_emails WHERE state = 'pending' AND next_attempt_at <= now() AND attempts = 0`); got != "1" {
		t.Errorf("minimal outbox row claimable = %s, want 1", got)
	}
	mustFail("second outbox row for one notification", `INSERT INTO notification_emails (org_id, notification_id, recipient_id, issue_id) VALUES ($1, $2, $3, $4)`, orgA, nid, u1, iA)
	nid2 := one(`SELECT id::text FROM notifications WHERE recipient_id = $1`, u2)
	mustFail("sent without sent_at", `INSERT INTO notification_emails (org_id, notification_id, recipient_id, issue_id, state, batch_id) VALUES ($1, $2, $3, $4, 'sent', uuidv7())`, orgA, nid2, u2, iA)
	mustFail("sent without batch_id", `INSERT INTO notification_emails (org_id, notification_id, recipient_id, issue_id, state, sent_at) VALUES ($1, $2, $3, $4, 'sent', now())`, orgA, nid2, u2, iA)
	mustFail("unknown state", `INSERT INTO notification_emails (org_id, notification_id, recipient_id, issue_id, state) VALUES ($1, $2, $3, $4, 'bogus')`, orgA, nid2, u2, iA)
	mustFail("NULL state", `INSERT INTO notification_emails (org_id, notification_id, recipient_id, issue_id, state) VALUES ($1, $2, $3, $4, NULL)`, orgA, nid2, u2, iA)
	mustFail("unknown skip reason", `INSERT INTO notification_emails (org_id, notification_id, recipient_id, issue_id, state, skip_reason) VALUES ($1, $2, $3, $4, 'skipped', 'bogus')`, orgA, nid2, u2, iA)
	mustFail("outbox row of another org", `INSERT INTO notification_emails (org_id, notification_id, recipient_id, issue_id) VALUES ($1, $2, $3, $4)`, orgB, nid2, u2, iB)
	mustExec(`INSERT INTO notification_emails (org_id, notification_id, recipient_id, issue_id, state, sent_at, batch_id) VALUES ($1, $2, $3, $4, 'sent', now(), uuidv7())`, orgA, nid2, u2, iA)

	// notification_mail_tokens
	tok := `INSERT INTO notification_mail_tokens (token_hash, org_id, user_id, targets, expires_at) VALUES ($1, $2, $3, $4::jsonb, now() + interval '1 day')`
	mustExec(tok, []byte("h1"), orgA, u1, `[{"event":"comment","teamId":null}]`)
	mustFail("duplicate token hash", tok, []byte("h1"), orgA, u2, `[{"event":"comment","teamId":null}]`)
	mustFail("empty targets", tok, []byte("h2"), orgA, u1, `[]`)
	mustFail("six targets", tok, []byte("h3"), orgA, u1, `[1,2,3,4,5,6]`)
	mustFail("targets not an array", tok, []byte("h4"), orgA, u1, `{}`)
	mustFail("token for a non-member", tok, []byte("h5"), orgB, u1, `[1]`)

	// No column can hold an IP, a User-Agent or a body.
	if got := count(`SELECT column_name FROM information_schema.columns WHERE table_schema = 'public'
		AND (table_name LIKE 'notification%' OR table_name IN ('team_follows', 'issue_subscriptions'))
		AND (column_name ~ '(^|_)(ip|ua|user_agent|body|title|text|excerpt_text)($|_)' OR column_name LIKE '%address%')`); got != "0" {
		t.Errorf("%s suspect columns", got)
	}

	// Cascades: removing a member and a team.
	mustExec(`DELETE FROM org_members WHERE org_id = $1 AND user_id = $2`, orgA, u1)
	for _, tbl := range []string{"notification_user_settings", "notification_settings", "team_follows", "issue_subscriptions", "notifications", "notification_emails", "notification_mail_tokens"} {
		col := "user_id"
		switch tbl {
		case "notifications", "notification_emails":
			col = "recipient_id"
		}
		if got := count(`SELECT 1 FROM `+tbl+` WHERE `+col+` = $1`, u1); got != "0" {
			t.Errorf("%s: %s rows left after member removal", tbl, got)
		}
	}
	if got := count(`SELECT 1 FROM notifications WHERE recipient_id = $1`, u2); got != "1" {
		t.Errorf("other member's notification: %s rows, want 1", got)
	}
	mustExec(`INSERT INTO notification_settings (org_id, user_id, team_id, event, email) VALUES ($1, $2, $3, 'comment', 'on')`, orgA, u2, tA)
	mustExec(`INSERT INTO team_follows (org_id, user_id, team_id) VALUES ($1, $2, $3)`, orgA, u2, tA)
	mustExec(`DELETE FROM issues WHERE id = $1`, iA)
	mustExec(`DELETE FROM teams WHERE id = $1`, tA)
	if got := count(`SELECT 1 FROM notification_settings WHERE team_id = $1 UNION ALL SELECT 1 FROM team_follows WHERE team_id = $1`, tA); got != "0" {
		t.Errorf("%s team rows left after team removal", got)
	}
	if got := count(`SELECT 1 FROM notifications UNION ALL SELECT 1 FROM notification_emails`); got != "0" {
		t.Errorf("%s notification rows left after issue removal", got)
	}
	// actor_id: SET NULL.
	iA2 := issue(orgA, tA2, st(orgA, tA2), 2)
	mustExec(`INSERT INTO notifications (org_id, recipient_id, kind, issue_id, actor_id, dedupe_key, inbox) VALUES ($1, $2, 'assigned', $3, $4, 'a', true)`, orgA, u2, iA2, u1)
	mustExec(`DELETE FROM users WHERE id = $1`, u1)
	if got := count(`SELECT 1 FROM notifications WHERE actor_id IS NULL`); got != "1" {
		t.Errorf("actor_id set null: %s rows, want 1", got)
	}

	// Down and up again.
	mustExec(`DELETE FROM orgs WHERE id = $1`, orgD)
	if got := count(`SELECT 1 FROM notification_cursors WHERE org_id = $1`, orgD); got != "0" {
		t.Errorf("cursor left after org removal")
	}
	migrateTo(t, m, 9)
	if got := one(`SELECT count(*)::text FROM pg_tables WHERE schemaname = 'public' AND (tablename LIKE 'notification%' OR tablename IN ('team_follows', 'issue_subscriptions'))`); got != "0" {
		t.Errorf("%s tables left after down", got)
	}
	if got := one(`SELECT count(*)::text FROM pg_trigger WHERE tgname = 'orgs_notification_cursor'`); got != "0" {
		t.Error("trigger left after down")
	}
	migrateTo(t, m, 10)
	if got := one(`SELECT count(*)::text FROM notification_cursors`); got != "3" {
		t.Errorf("cursors after re-up = %s, want 3", got)
	}
}
