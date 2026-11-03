package notify_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/dbq"
	"devops/internal/notify"
	"devops/internal/tracker"
)

// One-click unsubscribe (LAUN-34, decisions-T13 #25).

func tokenOf(t *testing.T, headerValue string) string {
	t.Helper()
	i := strings.LastIndex(headerValue, "/")
	if i < 0 || !strings.HasSuffix(headerValue, ">") {
		t.Fatalf("List-Unsubscribe %q", headerValue)
	}
	return strings.TrimSuffix(headerValue[i+1:], ">")
}

func (sw *sworld) lastToken() string {
	sw.t.Helper()
	ms := sw.m.sent()
	if len(ms) == 0 {
		sw.t.Fatal("no mail was sent")
	}
	return tokenOf(sw.t, ms[len(ms)-1].Headers["List-Unsubscribe"])
}

func (w *world) settingsOf(org, user uuid.UUID) []dbq.NotificationSetting {
	w.t.Helper()
	rows, err := w.q.ListNotificationSettingsSystem(context.Background(), dbq.ListNotificationSettingsSystemParams{OrgID: org, UserID: user})
	if err != nil {
		w.t.Fatal(err)
	}
	return rows
}

func sv(p *string) string {
	if p == nil {
		return "<null>"
	}
	return *p
}

func (sw *sworld) unsubscribe(token string) {
	sw.t.Helper()
	if err := notify.New(sw.pool).Unsubscribe(context.Background(), token); err != nil {
		sw.t.Fatalf("Unsubscribe: %v", err)
	}
}

// A mail with one kind whose value came from the default: a stored global
// off row for that event, nothing else.
func TestUnsubscribe_SingleKindDefaultGetsAGlobalOffRow(t *testing.T) {
	sw := newSWorld(t)
	alice := sw.user["alice"].UserID
	sw.assign("first", "alice")
	sw.send()
	token := sw.lastToken()

	// A second row waits while the person unsubscribes.
	sw.assign("second", "alice")
	sw.run()
	sw.unsubscribe(token)

	rows := sw.settingsOf(sw.org.ID, alice)
	if len(rows) != 1 || rows[0].Event != "assigned" || rows[0].TeamID != nil || sv(rows[0].Email) != "off" || rows[0].Inbox != nil {
		t.Fatalf("settings %+v, want one global assigned row with email off", rows)
	}
	var master int
	if err := sw.pool.QueryRow(context.Background(), `SELECT count(*) FROM notification_user_settings`).Scan(&master); err != nil || master != 0 {
		t.Fatalf("the master switch row was touched: %d (%v)", master, err)
	}
	// Other events: unchanged.
	for _, e := range []notify.Event{notify.EventComment, notify.EventMention} {
		if r := notify.Resolve(notify.DefaultUserPrefs, nil, e, rows); !r.Email.On || !r.Inbox.On {
			t.Fatalf("%s resolves to %+v after the unsubscribe", e, r)
		}
	}
	if r := notify.Resolve(notify.DefaultUserPrefs, nil, notify.EventAssigned, rows); r.Email.On || !r.Inbox.On {
		t.Fatalf("assigned resolves to %+v: email must be off, inbox on", r)
	}
	// The pending row stops (AC: skipped / disabled), and its inbox row stays.
	sw.clk.advance(3 * time.Minute)
	sw.pass()
	var skipped int
	if err := sw.pool.QueryRow(context.Background(), `SELECT count(*) FROM notification_emails WHERE state = 'skipped' AND skip_reason = 'disabled'`).Scan(&skipped); err != nil || skipped != 1 {
		t.Fatalf("disabled rows %d (%v); outbox %+v", skipped, err, sw.outbox())
	}
	if len(sw.m.sent()) != 1 {
		t.Fatalf("mails %d, want only the first", len(sw.m.sent()))
	}
	// The token is used up.
	var used *time.Time
	if err := sw.pool.QueryRow(context.Background(), `SELECT used_at FROM notification_mail_tokens WHERE token_hash = $1`, notify.HashToken(token)).Scan(&used); err != nil || used == nil {
		t.Fatalf("used_at %v (%v)", used, err)
	}
}

// A two-kind mail turns off both; a value that came from a team row is turned
// off at the team; an inbox value stays as it was.
func TestUnsubscribe_TwoKindMailTurnsOffBothAtTheirLevels(t *testing.T) {
	sw := newSWorld(t)
	ctx := context.Background()
	alice, org, team := sw.user["alice"].UserID, sw.org.ID, sw.teamID()
	// state_change: email on, inbox off, at the global level.
	sw.exec(`INSERT INTO notification_settings (org_id, user_id, event, email, inbox) VALUES ($1, $2, 'state_change', 'on', 'off')`, org, alice)
	// comment: email on at the TEAM level only.
	sw.exec(`INSERT INTO notification_settings (org_id, user_id, team_id, event, email) VALUES ($1, $2, $3, 'comment', 'on')`, org, alice, team)
	is := sw.assign("an issue", "alice")
	sw.comment("chen", is.Identifier, "hello")
	sw.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{State: str("In Progress")})
	sw.send()
	if len(sw.m.sent()) != 1 {
		t.Fatalf("mails %d", len(sw.m.sent()))
	}
	var targets []map[string]any
	var raw []byte
	if err := sw.pool.QueryRow(ctx, `SELECT targets FROM notification_mail_tokens`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &targets); err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, tg := range targets {
		got[tg["event"].(string)] = tg["teamId"]
	}
	if len(got) != 3 || got["assigned"] != nil || got["state_change"] != nil || got["comment"] != team.String() {
		t.Fatalf("targets %v, want assigned and state_change global, comment at the team", got)
	}

	sw.unsubscribe(sw.lastToken())
	rows := sw.settingsOf(org, alice)
	by := map[string]dbq.NotificationSetting{}
	for _, r := range rows {
		key := r.Event
		if r.TeamID != nil {
			key += "@team"
		}
		by[key] = r
	}
	if len(by) != 3 {
		t.Fatalf("settings %+v, want exactly assigned, state_change and comment@team", by)
	}
	if sv(by["assigned"].Email) != "off" || by["assigned"].Inbox != nil {
		t.Fatalf("assigned %+v", by["assigned"])
	}
	if sv(by["state_change"].Email) != "off" || sv(by["state_change"].Inbox) != "off" {
		t.Fatalf("state_change %+v: email off, inbox kept at off", by["state_change"])
	}
	if sv(by["comment@team"].Email) != "off" {
		t.Fatalf("comment@team %+v", by["comment@team"])
	}
	// No global comment row: the level was the team.
	if _, ok := by["comment"]; ok {
		t.Fatalf("a global comment row was made: %+v", by["comment"])
	}
}

func TestUnsubscribe_BadUsedAndExpiredTokensDoNothing(t *testing.T) {
	sw := newSWorld(t)
	ctx := context.Background()
	alice := sw.user["alice"].UserID
	svc := notify.New(sw.pool)
	for _, tok := range []string{"", "nope", strings.Repeat("a", 43), strings.Repeat("a", 500), "../../etc", "%00"} {
		if err := svc.Unsubscribe(ctx, tok); err != nil {
			t.Fatalf("token %q: %v", tok, err)
		}
	}
	if n := len(sw.settingsOf(sw.org.ID, alice)); n != 0 {
		t.Fatalf("a bad token wrote %d settings", n)
	}

	mint := func(token string, expires time.Time, targets string) {
		sw.exec(`INSERT INTO notification_mail_tokens (token_hash, org_id, user_id, targets, expires_at) VALUES ($1, $2, $3, $4::jsonb, $5)`,
			notify.HashToken(token), sw.org.ID, alice, targets, expires)
	}
	// Expired.
	mint("expired-token", time.Now().Add(-time.Minute), `[{"event":"comment","teamId":null}]`)
	if err := svc.Unsubscribe(ctx, "expired-token"); err != nil || len(sw.settingsOf(sw.org.ID, alice)) != 0 {
		t.Fatalf("an expired token changed settings (%v)", err)
	}
	// Used: it works once. The person turns the email back on; the token does not turn it off again.
	mint("good-token", time.Now().Add(time.Hour), `[{"event":"comment","teamId":null}]`)
	if err := svc.Unsubscribe(ctx, "good-token"); err != nil {
		t.Fatal(err)
	}
	if rows := sw.settingsOf(sw.org.ID, alice); len(rows) != 1 || sv(rows[0].Email) != "off" {
		t.Fatalf("settings %+v", rows)
	}
	sw.exec(`UPDATE notification_settings SET email = 'on'`)
	if err := svc.Unsubscribe(ctx, "good-token"); err != nil {
		t.Fatal(err)
	}
	if rows := sw.settingsOf(sw.org.ID, alice); sv(rows[0].Email) != "on" {
		t.Fatalf("a used token applied again: %+v", rows)
	}
	// Unknown events and unreadable targets do nothing and do not fail.
	mint("odd-token", time.Now().Add(time.Hour), `[{"event":"made_up","teamId":null}]`)
	if err := svc.Unsubscribe(ctx, "odd-token"); err != nil {
		t.Fatal(err)
	}
	mint("odd-token-2", time.Now().Add(time.Hour), `[{"event":123}]`)
	if err := svc.Unsubscribe(ctx, "odd-token-2"); err != nil {
		t.Fatal(err)
	}
	if rows := sw.settingsOf(sw.org.ID, alice); len(rows) != 1 {
		t.Fatalf("settings %+v", rows)
	}
}

// Two people using one token at the same time: applied once.
func TestUnsubscribe_ConcurrentUseAppliesOnce(t *testing.T) {
	sw := newSWorld(t)
	alice := sw.user["alice"].UserID
	sw.exec(`INSERT INTO notification_mail_tokens (token_hash, org_id, user_id, targets, expires_at) VALUES ($1, $2, $3, '[{"event":"assigned","teamId":null}]', now() + interval '1 hour')`,
		notify.HashToken("race-token"), sw.org.ID, alice)
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			if err := notify.New(sw.pool).Unsubscribe(context.Background(), "race-token"); err != nil {
				t.Errorf("Unsubscribe: %v", err)
			}
		})
	}
	wg.Wait()
	if rows := sw.settingsOf(sw.org.ID, alice); len(rows) != 1 || sv(rows[0].Email) != "off" {
		t.Fatalf("settings %+v", rows)
	}
}

// Cross-org: a token acts in its own org only, even for a user who is in
// both, and a team of another org in its targets is skipped.
func TestUnsubscribe_CrossOrg(t *testing.T) {
	sw := newSWorld(t)
	ctx := context.Background()
	org2, err := sw.q.InsertOrg(ctx, dbq.InsertOrgParams{Slug: "other", Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	team2, err := tracker.CreateTeamTx(ctx, sw.q, org2.ID, "OTH", "Other", "")
	if err != nil {
		t.Fatal(err)
	}
	alice := sw.addMember(org2.ID, "alice").UserID
	// Alice has her own setting in org 2.
	sw.exec(`INSERT INTO notification_settings (org_id, user_id, event, email) VALUES ($1, $2, 'assigned', 'on')`, org2.ID, alice)

	sw.assign("home issue", "alice")
	sw.send()
	sw.unsubscribe(sw.lastToken())
	if rows := sw.settingsOf(sw.org.ID, alice); len(rows) != 1 || sv(rows[0].Email) != "off" {
		t.Fatalf("org 1 settings %+v", rows)
	}
	if rows := sw.settingsOf(org2.ID, alice); len(rows) != 1 || sv(rows[0].Email) != "on" {
		t.Fatalf("org 2 settings changed: %+v", rows)
	}

	// A token of org 1 that names a team of org 2.
	crossTeam := `[{"event":"comment","teamId":"` + team2.ID.String() + `"}]`
	sw.exec(`INSERT INTO notification_mail_tokens (token_hash, org_id, user_id, targets, expires_at) VALUES ($1, $2, $3, $4::jsonb, now() + interval '1 hour')`,
		notify.HashToken("cross-token"), sw.org.ID, alice, crossTeam)
	sw.unsubscribe("cross-token")
	var n int
	if err := sw.pool.QueryRow(ctx, `SELECT count(*) FROM notification_settings WHERE team_id = $1`, team2.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a team of another org got %d settings (%v)", n, err)
	}
}
