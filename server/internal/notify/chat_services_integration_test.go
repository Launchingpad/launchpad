package notify_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // the "pgx" database/sql driver

	"devops/db/migrations"
	"devops/internal/notify"
	"devops/internal/platform"
	"devops/internal/testdb"
	"devops/internal/tracker"
)

// More of the chat services (LAUN-204, decisions-T36): the kinds on the wire,
// the switch, the migration of a Slack channel of before.

const (
	discordHook = "https://discord.com/api/webhooks/123456789012345678/" + secretToken
	customHook  = "https://chat.example.com/hooks/" + secretToken
	fluxerHook  = "https://chat.example.com/api/webhooks/123456789/" + secretToken
)

// Each kind posts its own body shape with the same fixed sentence, identifier
// and link, and nothing else of the issue (decisions-T36 #4, T33 #13).
func TestChat_KindsPostTheirOwnShape(t *testing.T) {
	w := newChatWorld(t)
	w.putService(w.org.ID, notify.ChatSlack, "Slack", hookURL, "assigned")
	w.putService(w.org.ID, notify.ChatDiscord, "Disc", discordHook, "assigned")
	w.putService(w.org.ID, notify.ChatCustom, "Fluxer", customHook, "assigned")
	is := w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice", Title: "TOPSECRETTITLE", Description: "TOPSECRETBODY"})
	w.consume()
	if n := len(w.outRows()); n != 3 {
		t.Fatalf("rows = %d, want one per service", n)
	}
	if res := w.pass(); res.Sent != 3 {
		t.Fatalf("pass %+v", res)
	}
	link := testBase + "/orbit/issue/" + is.Identifier
	byPath := map[string]map[string]any{}
	for _, h := range w.slack.got() {
		for _, leak := range []string{"TOPSECRETTITLE", "TOPSECRETBODY", "alice", "chen"} {
			if strings.Contains(h.Body, leak) {
				t.Errorf("a post holds %q: %s", leak, h.Body)
			}
		}
		var b map[string]any
		if err := json.Unmarshal([]byte(h.Body), &b); err != nil {
			t.Fatal(err)
		}
		byPath[h.Path] = b
	}
	if b := byPath["/services/T0001/B0001/"+secretToken]; b["text"] != "["+is.Identifier+"] Issue assigned: <"+link+"|open in Launchpad>" || b["unfurl_links"] != false {
		t.Errorf("slack body %v", b)
	}
	d := byPath["/api/webhooks/123456789012345678/"+secretToken]
	if d["content"] != "["+is.Identifier+"] Issue assigned: <"+link+">" || d["text"] != nil {
		t.Errorf("discord body %v", d)
	}
	if am, _ := d["allowed_mentions"].(map[string]any); am == nil || len(am["parse"].([]any)) != 0 {
		t.Errorf("discord allowed_mentions %v", d["allowed_mentions"])
	}
	c := byPath["/hooks/"+secretToken]
	if c["text"] != "["+is.Identifier+"] Issue assigned: "+link || c["kind"] != "assigned" || c["id"] != is.Identifier || c["link"] != link || len(c) != 4 {
		t.Errorf("custom body %v", c)
	}
}

// A switched-off service gets nothing and its waiting posts are dropped; a
// deleted service takes its posts with it; turning it on again posts nothing
// old (decisions-T36 #7).
func TestChat_DisabledAndDeletedDropWaitingPosts(t *testing.T) {
	w := newChatWorld(t)
	ctx := context.Background()
	chen := w.user["chen"]
	a := w.putService(w.org.ID, notify.ChatSlack, "A", hookURL, "assigned")
	b := w.putService(w.org.ID, notify.ChatCustom, "B", customHook, "assigned")
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	if n := len(w.outRows()); n != 2 {
		t.Fatalf("rows %d", n)
	}
	count := func(id uuid.UUID) int {
		t.Helper()
		var n int
		if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM chat_outbox WHERE service_id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	off := false
	if _, err := w.svc2.Patch(ctx, w.org.ID, a.ID, chen, notify.ChatPatchInput{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	if count(a.ID) != 0 || count(b.ID) != 1 {
		t.Fatalf("after switching A off: A has %d rows, B has %d; want 0 and 1", count(a.ID), count(b.ID))
	}
	// Events while A is off make no row for A.
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "bob"})
	w.consume()
	if count(a.ID) != 0 || count(b.ID) != 2 {
		t.Fatalf("while A is off: A has %d rows, B has %d; want 0 and 2", count(a.ID), count(b.ID))
	}
	if res := w.pass(); res.Sent != 2 {
		t.Fatalf("pass %+v", res)
	}
	for _, h := range w.slack.got() {
		if strings.HasPrefix(h.Path, "/services/") {
			t.Fatalf("the switched-off service got a post: %v", h)
		}
	}
	// On again: A gets new events only.
	on := true
	if _, err := w.svc2.Patch(ctx, w.org.ID, a.ID, chen, notify.ChatPatchInput{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	if count(a.ID) != 0 {
		t.Fatalf("A got %d old rows when switched on", count(a.ID))
	}
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "carol"})
	w.consume()
	if count(a.ID) != 1 || count(b.ID) != 3 {
		t.Fatalf("after switching A on: A has %d rows, B has %d; want 1 and 3", count(a.ID), count(b.ID))
	}
	// Deleting B takes all its rows (the waiting one too) and posts nothing more to it.
	if err := w.svc2.Delete(ctx, w.org.ID, b.ID, chen); err != nil {
		t.Fatal(err)
	}
	if count(b.ID) != 0 || count(a.ID) != 1 {
		t.Fatalf("after deleting B: B has %d rows, A has %d", count(b.ID), count(a.ID))
	}
	before := len(w.slack.got())
	if res := w.pass(); res.Sent != 1 {
		t.Fatalf("pass %+v", res)
	}
	for _, h := range w.slack.got()[before:] {
		if !strings.HasPrefix(h.Path, "/services/") {
			t.Fatalf("a post went to the deleted service: %v", h)
		}
	}
}

// Each service has its own hourly cap and its own status (decisions-T36 #7).
func TestChat_RateCapAndStatusAreEachServices(t *testing.T) {
	w := newChatWorld(t)
	a := w.putService(w.org.ID, notify.ChatSlack, "A", hookURL, "assigned")
	b := w.putService(w.org.ID, notify.ChatCustom, "B", customHook, "assigned")
	is := w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	w.exec(`DELETE FROM chat_outbox`)
	// A is at its cap; B is not.
	w.exec(`INSERT INTO chat_outbox (org_id, service_id, kind, issue_id, dedupe_key, state, sent_at)
		SELECT $1, $5, 'assigned', $2, 'old:' || g, 'sent', $3 FROM generate_series(1, $4) g`,
		w.org.ID, is.ID, w.clk.now().Add(-time.Minute), notify.ChatRateCap, a.ID)
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "bob"})
	w.consume()
	res := w.pass()
	if res.Sent != 1 {
		t.Fatalf("pass %+v", res)
	}
	got := w.slack.got()
	if len(got) != 1 || !strings.HasPrefix(got[0].Path, "/hooks/") {
		t.Fatalf("hits %+v, want only B's", got)
	}
	var capped, sentB int
	_ = w.pool.QueryRow(context.Background(), `SELECT count(*) FROM chat_outbox WHERE service_id = $1 AND skip_reason = 'rate_cap'`, a.ID).Scan(&capped)
	_ = w.pool.QueryRow(context.Background(), `SELECT count(*) FROM chat_outbox WHERE service_id = $1 AND state = 'sent'`, b.ID).Scan(&sentB)
	if capped != 1 || sentB != 1 {
		t.Fatalf("capped A = %d, sent B = %d", capped, sentB)
	}
	// B's success does not clear or set A's status.
	var errA *string
	_ = w.pool.QueryRow(context.Background(), `SELECT last_error FROM chat_services WHERE id = $1`, a.ID).Scan(&errA)
	var okB *time.Time
	_ = w.pool.QueryRow(context.Background(), `SELECT last_ok_at FROM chat_services WHERE id = $1`, b.ID).Scan(&okB)
	if errA != nil || okB == nil {
		t.Fatalf("status A err=%v B ok=%v", errA, okB)
	}
}

// A custom host the admin typed that resolves to a private address is refused
// at dial time, with the fixed word, and the resolver answer is not followed
// (decisions-T36 #5).
func TestChat_CustomHostResolvingPrivateIsBlocked(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "10.0.0.5", "169.254.169.254", "192.168.0.9", "::1", "fd00::1"} {
		res := &netguardMap{Hosts: map[string][]string{"chat.example.com": {addr}}}
		poster := notify.NewChatPoster(res.client())
		err := poster.Post(context.Background(), notify.ChatCustom, customHook, notify.ChatMessage{Event: notify.EventTest})
		var ce *notify.ChatError
		if !asChatErr(err, &ce) || ce.Word != notify.ChatBlocked {
			t.Errorf("%s: Post = %v, want chat_blocked", addr, err)
		}
	}
	// A custom URL written to the database by another way is checked again at post time.
	f := newFakeChat(t)
	poster := notify.NewChatPoster(f.client(t))
	for _, u := range []string{"https://127.0.0.1/h", "https://localhost/h", "http://chat.example.com/h", "https://2130706433/h", "https://10.1.1.1/h"} {
		err := poster.Post(context.Background(), notify.ChatCustom, u, notify.ChatMessage{Event: notify.EventTest})
		var ce *notify.ChatError
		if !asChatErr(err, &ce) || ce.Word != notify.ChatBlocked {
			t.Errorf("%q: %v", u, err)
		}
	}
	if n := len(f.got()); n != 0 {
		t.Errorf("fake called %d times", n)
	}
}

// Discord answers 204 on success; a 2xx is success for every kind.
func TestChat_Discord204IsSuccess(t *testing.T) {
	w := newChatWorld(t)
	w.putService(w.org.ID, notify.ChatDiscord, "Disc", discordHook, "assigned")
	w.slack.set(204, "")
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	if res := w.pass(); res.Sent != 1 {
		t.Fatalf("pass %+v", res)
	}
	// And a 429 with a decimal Retry-After is honoured, in seconds.
	w.slack.set(429, "1.5")
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "bob"})
	w.consume()
	w.pass()
	r := w.outRows()[1]
	if r.State != "pending" || r.LastErr != "chat_rate_limited" {
		t.Fatalf("row %+v", r)
	}
	// The default retry (1m) is longer than 1.5s, so it wins.
	if got := r.Next.Sub(w.clk.now()); got != notify.RetryDelays[0] {
		t.Errorf("next attempt in %v", got)
	}
}

// A service sealed under the scheme of the Slack channel (the org id as the
// additional data) still opens with the same key (decisions-T36 #3), and a
// blob cannot be moved to another service or org.
func TestChat_SealScopes(t *testing.T) {
	w := newChatWorld(t)
	ctx := context.Background()
	v := w.putService(w.org.ID, notify.ChatSlack, "Slack", hookURL, "assigned")
	if l, _ := w.svc2.List(ctx, w.org.ID); l.Items[0].SecretUnreadable {
		t.Fatal("a fresh service is unreadable")
	}
	var blob []byte
	var scope string
	if err := w.pool.QueryRow(ctx, `SELECT webhook_enc, seal_scope FROM chat_services WHERE id = $1`, v.ID).Scan(&blob, &scope); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(scope, "svc."+w.org.ID.String()+".") || !strings.HasSuffix(scope, v.ID.String()) {
		t.Fatalf("scope %q", scope)
	}
	// Another service of the same org cannot use this blob (a copy in the database).
	o := w.putService(w.org.ID, notify.ChatSlack, "Other", "https://hooks.slack.com/services/T0002/B0002/OTHERTOKEN", "assigned")
	w.exec(`UPDATE chat_services SET webhook_enc = $1, seal_scope = $2 WHERE id = $3`, blob, scope, o.ID)
	l, _ := w.svc2.List(ctx, w.org.ID)
	for _, it := range l.Items {
		if it.ID == o.ID && !it.SecretUnreadable {
			t.Error("a blob copied to another service opens")
		}
	}
	// A scope that names another org is refused even with a matching blob.
	w.exec(`UPDATE chat_services SET seal_scope = 'svc.' || $1 || '.' || $2 WHERE id = $3`, uuid.New(), v.ID, v.ID)
	l, _ = w.svc2.List(ctx, w.org.ID)
	for _, it := range l.Items {
		if it.ID == v.ID && !it.SecretUnreadable {
			t.Error("a foreign scope opens")
		}
	}
	// A blob sealed under the OLD scheme (the org id) opens for a Slack service
	// of that org, and only for a Slack service.
	oldBlob := w.keys.SealFor(platform.SlackColWebhook, w.org.ID.String(), []byte(hookURL))
	w.exec(`UPDATE chat_services SET webhook_enc = $1, seal_scope = org_id::text WHERE id = $2`, oldBlob, v.ID)
	l, _ = w.svc2.List(ctx, w.org.ID)
	for _, it := range l.Items {
		if it.ID == v.ID && it.SecretUnreadable {
			t.Error("an old-scheme blob does not open for a Slack service")
		}
	}
	w.exec(`UPDATE chat_services SET kind = 'custom' WHERE id = $1`, v.ID)
	l, _ = w.svc2.List(ctx, w.org.ID)
	for _, it := range l.Items {
		if it.ID == v.ID && !it.SecretUnreadable {
			t.Error("a legacy scope opens for a non-Slack kind")
		}
	}
}

// The big one: an org that ran the Slack channel of LAUN-198 (migration 0031,
// a webhook sealed under the OLD scheme, a waiting post and a sent one) is
// migrated to 0034. The webhook still opens, the test message reaches it, the
// waiting post is sent to it, and the cap and status work (decisions-T36 #5).
func TestChat_MigratedSlackChannelStillUnsealsAndSends(t *testing.T) {
	dsn := testdb.Fresh(t)
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	drv, err := migratepg.WithInstance(db, &migratepg.Config{})
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", drv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.Close() })
	mig := func(v uint) {
		t.Helper()
		if err := m.Migrate(v); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			t.Fatalf("migrate to %d: %v", v, err)
		}
	}
	mig(33)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	one := func(q string, args ...any) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, q, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return s
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	org := one(`INSERT INTO orgs (slug, name) VALUES ('orbit', 'Orbit') RETURNING id::text`)
	org2 := one(`INSERT INTO orgs (slug, name) VALUES ('other', 'Other') RETURNING id::text`)
	team := one(`INSERT INTO teams (org_id, key, name) VALUES ($1, 'ORB', 'Orbit') RETURNING id::text`, org)
	state := one(`INSERT INTO workflow_states (org_id, team_id, name, type, position) VALUES ($1, $2, 'Todo', 'unstarted', 1) RETURNING id::text`, org, team)
	issue := one(`INSERT INTO issues (org_id, team_id, number, title, state_id) VALUES ($1, $2, 7, 'x', $3) RETURNING id::text`, org, team, state)

	// The OLD scheme: SlackKeyring.SealFor("webhook_enc", <org id>), as LAUN-198 did.
	keys, err := platform.NewSlackKeyring(slackKey)
	if err != nil {
		t.Fatal(err)
	}
	old := keys.SealFor(platform.SlackColWebhook, org, []byte(hookURL))
	exec(`INSERT INTO slack_channels (org_id, webhook_enc, events, last_error) VALUES ($1, $2, '{assigned,comment}', 'slack_unavailable')`, org, old)
	exec(`INSERT INTO slack_outbox (org_id, kind, issue_id, dedupe_key, state) VALUES ($1, 'assigned', $2, 'slack:evt:1:assigned', 'pending')`, org, issue)
	exec(`INSERT INTO slack_outbox (org_id, kind, issue_id, dedupe_key, state, sent_at, last_error) VALUES ($1, 'comment', $2, 'slack:evt:2:comment', 'sent', now(), NULL)`, org, issue)
	exec(`INSERT INTO slack_outbox (org_id, kind, issue_id, dedupe_key, state, attempts, last_error) VALUES ($1, 'comment', $2, 'slack:evt:3:comment', 'dead', 6, 'slack_gone')`, org, issue)
	// An org with no channel and a waiting row: it can only have been skipped.
	team2 := one(`INSERT INTO teams (org_id, key, name) VALUES ($1, 'OTH', 'Other') RETURNING id::text`, org2)
	state2 := one(`INSERT INTO workflow_states (org_id, team_id, name, type, position) VALUES ($1, $2, 'Todo', 'unstarted', 1) RETURNING id::text`, org2, team2)
	issue2 := one(`INSERT INTO issues (org_id, team_id, number, title, state_id) VALUES ($1, $2, 1, 'x', $3) RETURNING id::text`, org2, team2, state2)
	exec(`INSERT INTO slack_outbox (org_id, kind, issue_id, dedupe_key) VALUES ($1, 'assigned', $2, 'slack:evt:9:assigned')`, org2, issue2)

	mig(34)

	// The channel became one `slack` service named Slack, with its events.
	var name, kind, host, scope string
	var events []string
	var enabled bool
	var lastErr *string
	if err := pool.QueryRow(ctx, `SELECT name, kind, host, seal_scope, events, enabled, last_error FROM chat_services WHERE org_id = $1`, org).
		Scan(&name, &kind, &host, &scope, &events, &enabled, &lastErr); err != nil {
		t.Fatal(err)
	}
	if name != "Slack" || kind != "slack" || host != "hooks.slack.com" || scope != org || !enabled || strings.Join(events, ",") != "assigned,comment" {
		t.Fatalf("migrated service %q %q %q %q %v %v", name, kind, host, scope, events, enabled)
	}
	if lastErr == nil || *lastErr != "chat_unavailable" {
		t.Fatalf("last_error %v, want the fixed word mapped to chat_unavailable", lastErr)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM chat_services WHERE org_id = $1`, org2).Scan(&n)
	if n != 0 {
		t.Fatalf("an org without a channel got %d services", n)
	}
	rows := map[string]string{}
	rs, err := pool.Query(ctx, `SELECT dedupe_key, state || '/' || coalesce(last_error, '') FROM chat_outbox`)
	if err != nil {
		t.Fatal(err)
	}
	for rs.Next() {
		var k, v string
		_ = rs.Scan(&k, &v)
		rows[k] = v
	}
	rs.Close()
	if len(rows) != 3 || rows["slack:evt:1:assigned"] != "pending/" || rows["slack:evt:2:comment"] != "sent/" || rows["slack:evt:3:comment"] != "dead/chat_gone" {
		t.Fatalf("outbox after migration %v (the row of the org without a channel must be gone)", rows)
	}
	var gone bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('slack_outbox') IS NULL AND to_regclass('slack_channels') IS NULL`).Scan(&gone); err != nil || !gone {
		t.Fatalf("the old tables are still there (%v)", err)
	}

	// 0036 (LAUN-209) gives the migrated service its org's team (no default team: the oldest).
	mig(36)
	if got := one(`SELECT team_id::text FROM chat_services WHERE org_id = $1`, org); got != team {
		t.Fatalf("migrated service team = %s, want %s", got, team)
	}

	// The service opens, and the test message and the waiting post reach the webhook.
	f := newFakeChat(t)
	var logs lockedBuf
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	poster := notify.NewChatPoster(f.client(t))
	admin := notify.NewChatAdmin(pool, log, keys, poster, testBase)
	orgID := uuid.MustParse(org)
	list, err := admin.List(ctx, orgID)
	if err != nil || len(list.Items) != 1 || list.Items[0].SecretUnreadable || list.Items[0].Host != "hooks.slack.com" {
		t.Fatalf("list %+v %v", list, err)
	}
	if _, err := admin.Test(ctx, orgID, list.Items[0].ID); err != nil {
		t.Fatalf("test message: %v", err)
	}
	if h := f.got(); len(h) != 1 || h[0].Path != "/services/T0001/B0001/"+secretToken {
		t.Fatalf("test hits %+v", h)
	}
	sender := notify.NewChatSender(pool, log, keys, poster, testBase)
	res, err := sender.RunOnce(ctx)
	if err != nil || res.Sent != 1 {
		t.Fatalf("pass %+v %v", res, err)
	}
	h := f.got()
	if len(h) != 2 || !strings.Contains(h[1].Body, "[ORB-7] Issue assigned: <"+testBase+"/orbit/issue/ORB-7|open in Launchpad>") {
		t.Fatalf("hits %+v", h)
	}
	if strings.Contains(logs.String(), secretToken) {
		t.Error("the log holds the webhook")
	}

	// The same blob does NOT open under the scope of a new service: the old
	// scheme is only for the migrated row.
	if _, err := keys.OpenFor(platform.SlackColWebhook, "svc."+org+"."+list.Items[0].ID.String(), old); err == nil {
		t.Error("an old blob opened under a new scope")
	}
}

// A Fluxer service posts the Discord body; on a name that resolves to a private
// address, or at a private host written to the database by another way, it is
// blocked at dial and at post time (LAUN-206).
func TestChat_FluxerPostsDiscordBodyAndIsGuarded(t *testing.T) {
	f := newFakeChat(t)
	poster := notify.NewChatPoster(f.client(t))
	m := notify.ChatMessage{Event: notify.EventNewIssue, Identifier: "ORB-1", Link: "https://x.example/o/issue/ORB-1"}
	if err := poster.Post(context.Background(), notify.ChatFluxer, fluxerHook, m); err != nil {
		t.Fatalf("post: %v", err)
	}
	if err := poster.Post(context.Background(), notify.ChatDiscord, discordHook, m); err != nil {
		t.Fatalf("post: %v", err)
	}
	got := f.got()
	if len(got) != 2 || got[0].Body != got[1].Body || got[0].Path != "/api/webhooks/123456789/"+secretToken {
		t.Fatalf("hits %+v", got)
	}
	for _, addr := range []string{"127.0.0.1", "10.0.0.5", "192.168.0.9", "::1"} {
		res := &netguardMap{Hosts: map[string][]string{"chat.example.com": {addr}}}
		err := notify.NewChatPoster(res.client()).Post(context.Background(), notify.ChatFluxer, fluxerHook, notify.ChatMessage{Event: notify.EventTest})
		var ce *notify.ChatError
		if !asChatErr(err, &ce) || ce.Word != notify.ChatBlocked {
			t.Errorf("%s: Post = %v, want chat_blocked", addr, err)
		}
	}
	n := len(f.got())
	for _, u := range []string{"https://fluxer.lan/api/webhooks/1/a", "https://10.1.1.1/api/webhooks/1/a", "http://chat.example.com/api/webhooks/1/a"} {
		err := poster.Post(context.Background(), notify.ChatFluxer, u, notify.ChatMessage{Event: notify.EventTest})
		var ce *notify.ChatError
		if !asChatErr(err, &ce) || ce.Word != notify.ChatBlocked {
			t.Errorf("%q: %v", u, err)
		}
	}
	if len(f.got()) != n {
		t.Error("the fake was called for a refused URL")
	}
}
