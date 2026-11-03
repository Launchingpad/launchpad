package notify_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"devops/internal/notify"
	"devops/internal/tracker"
)

// Personal webhooks (LAUN-229, decisions-T40), with the real database, the
// real consumer and sender, and a fake host through the REAL netguard dialer.

const (
	pwhToken = "PERSONALWEBHOOKTOKEN4242"
	pwhURL   = "https://hooks.slack.com/services/T0003/B0003/" + pwhToken
)

type pworld struct {
	*swo
	pw *notify.PersonalWebhooks
	ps *notify.PersonalWebhookSender
}

func newPersonalWorld(t *testing.T) *pworld {
	t.Helper()
	w := newChatWorld(t)
	poster := notify.NewChatPoster(w.slack.client(t))
	pw := &pworld{swo: w}
	log := slog.New(slog.NewTextHandler(w.slogs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	pw.pw = notify.NewPersonalWebhooks(w.pool, log, w.keys, poster, testBase)
	pw.ps = notify.NewPersonalWebhookSender(w.pool, log, w.keys, poster, testBase)
	pw.ps.SetClock(w.clk.now)
	return pw
}

// hook makes a webhook of the member with the given choices (nil: both).
func (w *pworld) hook(who, name string, events []string) notify.PersonalView {
	w.t.Helper()
	v, err := w.pw.Create(context.Background(), w.org.ID, w.user[who].UserID,
		notify.PersonalCreateInput{Name: name, Kind: notify.ChatSlack, WebhookURL: pwhURL, Events: events})
	if err != nil {
		w.t.Fatalf("Create: %v", err)
	}
	return v
}

// consume runs the consumer and ages the new rows to the fake clock.
func (w *pworld) consume() {
	w.t.Helper()
	w.world.run()
	w.exec(`UPDATE personal_webhook_outbox SET created_at = $1, next_attempt_at = $1 WHERE created_at > $1`, w.clk.now())
}

func (w *pworld) pass() notify.ChatResult {
	w.t.Helper()
	res, err := w.ps.RunOnce(context.Background())
	if err != nil {
		w.t.Fatalf("personal RunOnce: %v", err)
	}
	return res
}

// prows lists the outbox as "owner:kind:state[:skip]", oldest first.
func (w *pworld) prows() []string {
	w.t.Helper()
	rs, err := w.pool.Query(context.Background(), `
		SELECT u.username, o.kind, o.state, coalesce(o.skip_reason, '')
		FROM personal_webhook_outbox o
		JOIN personal_webhooks h ON h.org_id = o.org_id AND h.id = o.webhook_id
		JOIN users u ON u.id = h.user_id
		ORDER BY o.created_at, o.id`)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rs.Close()
	var out []string
	for rs.Next() {
		var who, kind, state, skip string
		if err := rs.Scan(&who, &kind, &state, &skip); err != nil {
			w.t.Fatal(err)
		}
		s := who + ":" + kind + ":" + state
		if skip != "" {
			s += ":" + skip
		}
		out = append(out, s)
	}
	return out
}

func (w *pworld) wantP(want ...string) {
	w.t.Helper()
	got := w.prows()
	if strings.Join(got, " ") != strings.Join(want, " ") {
		w.t.Fatalf("personal outbox = %v, want %v", got, want)
	}
}

func (w *pworld) clearP() {
	w.t.Helper()
	w.exec(`DELETE FROM personal_webhook_outbox`)
}

func TestPersonal_AssignedPostsFixedSentence(t *testing.T) {
	w := newPersonalWorld(t)
	w.hook("alice", "Mine", nil)
	is := w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice", Title: "TOPSECRETTITLE", Description: "TOPSECRETBODY"})
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "bob"}) // bob has no webhook
	w.consume()
	w.wantP("alice:assigned:pending")
	if res := w.pass(); res.Sent != 1 {
		t.Fatalf("pass %+v", res)
	}
	hits := w.slack.got()
	if len(hits) != 1 || hits[0].Path != "/services/T0003/B0003/"+pwhToken {
		t.Fatalf("hits %+v", hits)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(hits[0].Body), &body); err != nil {
		t.Fatal(err)
	}
	want := "[" + is.Identifier + "] Issue assigned to you: <" + testBase + "/orbit/issue/" + is.Identifier + "|open in Launchpad>"
	if body["text"] != want {
		t.Errorf("text = %q, want %q", body["text"], want)
	}
	for _, leak := range []string{"TOPSECRETTITLE", "TOPSECRETBODY", "alice", "chen"} {
		if strings.Contains(hits[0].Body, leak) {
			t.Errorf("the post holds %q: %s", leak, hits[0].Body)
		}
	}
	w.wantP("alice:assigned:sent")
	// A replay of the feed makes no second row.
	w.exec(`UPDATE notification_cursors SET last_seq = 0`)
	w.consume()
	w.wantP("alice:assigned:sent")
	if res := w.pass(); res.Sent != 0 {
		t.Fatalf("a second pass posted: %+v", res)
	}
	// Nothing secret or personal in the logs.
	if l := w.slogs.String(); strings.Contains(l, pwhToken) || strings.Contains(l, is.Identifier) {
		t.Errorf("logs: %s", l)
	}
}

// "My issues": creator or assignee; every change kind; never my own change.
func TestPersonal_ChangesOnMyIssues(t *testing.T) {
	w := newPersonalWorld(t)
	w.hook("alice", "Mine", nil)
	w.hook("bob", "Bob", nil)
	is := w.createIssue("alice", tracker.CreateIssueInput{}) // alice creates: her own act
	w.consume()
	w.wantP()

	w.comment("chen", is.Identifier, "a note")
	w.consume()
	w.wantP("alice:comment:pending")
	w.clearP()

	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{State: str("In Progress")})
	w.consume()
	w.wantP("alice:state_change:pending")
	if res := w.pass(); res.Sent != 1 {
		t.Fatalf("pass %+v", res)
	}
	if b := w.slack.got()[0].Body; !strings.Contains(b, "Your issue changed state") || !strings.Contains(b, "In Progress") {
		t.Fatalf("state post %s", b)
	}
	w.clearP()

	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{Title: str("TOPSECRETTITLE")})
	w.consume()
	w.wantP("alice:issue_updated:pending")
	w.clearP()

	// Assigned to bob: bob gets "assigned", alice (creator) gets the change.
	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{Assignee: str("bob")})
	w.consume()
	got := strings.Join(w.prows(), " ")
	if !strings.Contains(got, "bob:assigned:pending") || !strings.Contains(got, "alice:issue_updated:pending") || len(w.prows()) != 2 {
		t.Fatalf("rows %s", got)
	}
	w.clearP()

	// bob's own change on the issue: alice hears of it, bob does not.
	w.comment("bob", is.Identifier, "mine")
	w.consume()
	w.wantP("alice:comment:pending")
	w.clearP()

	// A comment edit is no change; a board move alone is no change.
	c := w.comment("alice", is.Identifier, "first")
	w.consume()
	w.clearP()
	w.editComment("alice", c.ID, "edited by alice")
	w.consume()
	w.wantP()
}

func TestPersonal_EventChoices(t *testing.T) {
	w := newPersonalWorld(t)
	w.hook("alice", "Assigned only", []string{"assigned"})
	w.hook("bob", "Mine only", []string{"my_issues"})
	is := w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "bob"}) // created: no change, and bob did not tick assigned
	w.consume()
	w.wantP("alice:assigned:pending")
	w.clearP()
	// A change on alice's issue: she ticked only "assigned".
	w.comment("chen", is.Identifier, "hello")
	w.consume()
	w.wantP()
	// An update that assigns bob (only my_issues): the change kind.
	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{Assignee: str("bob")})
	w.consume()
	w.wantP("bob:issue_updated:pending")
	// Taking the choice off skips the waiting row at send time.
	var id uuid.UUID
	if err := w.pool.QueryRow(context.Background(), `SELECT id FROM personal_webhooks WHERE name = 'Mine only'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := w.pw.Patch(context.Background(), w.org.ID, w.user["bob"].UserID, id, notify.PersonalPatchInput{Events: []string{"assigned"}}); err != nil {
		t.Fatal(err)
	}
	w.pass()
	w.wantP("bob:issue_updated:skipped:not_selected")
	if len(w.slack.got()) != 0 {
		t.Fatal("a post was sent")
	}
}

// T20 #61 and #63: a member who cannot see the team gets no row; access
// removed after the row was made skips it at send time (access_removed).
func TestPersonal_TeamAccess(t *testing.T) {
	w := newPersonalWorld(t)
	w.hook("alice", "Mine", nil)
	w.hook("bob", "Bob", nil)
	w.restrict("alice")
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	w.wantP()

	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "bob"})
	w.consume()
	w.wantP("bob:assigned:pending")
	w.restrict("bob") // no team any more
	w.pass()
	w.wantP("bob:assigned:skipped:access_removed")
	if len(w.slack.got()) != 0 {
		t.Fatal("a post for a hidden issue was sent")
	}
}

func TestPersonal_MutedDisabledAndDeactivated(t *testing.T) {
	w := newPersonalWorld(t)
	v := w.hook("alice", "Mine", nil)
	is := w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	w.clearP()
	// alice mutes the issue: nothing.
	w.exec(`INSERT INTO issue_subscriptions (org_id, issue_id, user_id, state, reason) VALUES ($1, $2, $3, 'muted', 'manual')`,
		w.org.ID, is.ID, w.user["alice"].UserID)
	w.comment("chen", is.Identifier, "x")
	w.consume()
	w.wantP()
	w.exec(`DELETE FROM issue_subscriptions`)

	// A row waits; switching the webhook off deletes it; while off nothing is made.
	w.comment("chen", is.Identifier, "y")
	w.consume()
	w.wantP("alice:comment:pending")
	off := false
	if _, err := w.pw.Patch(context.Background(), w.org.ID, w.user["alice"].UserID, v.ID, notify.PersonalPatchInput{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	w.wantP()
	w.comment("chen", is.Identifier, "z")
	w.consume()
	w.wantP()
	on := true
	if _, err := w.pw.Patch(context.Background(), w.org.ID, w.user["alice"].UserID, v.ID, notify.PersonalPatchInput{Enabled: &on}); err != nil {
		t.Fatal(err)
	}

	// The owner deactivated between the row and the post: skipped.
	w.comment("chen", is.Identifier, "w")
	w.consume()
	w.exec(`UPDATE users SET deactivated_at = now() WHERE id = $1`, w.user["alice"].UserID)
	w.pass()
	w.wantP("alice:comment:skipped:disabled")
}

// The hourly cap per webhook (the chat cap) and the failure policy.
func TestPersonal_CapAndFailures(t *testing.T) {
	w := newPersonalWorld(t)
	v := w.hook("alice", "Mine", nil)
	is := w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.consume()
	w.clearP()
	for i := 0; i < notify.ChatRateCap; i++ {
		w.exec(`INSERT INTO personal_webhook_outbox (org_id, webhook_id, kind, issue_id, dedupe_key, state, sent_at)
			VALUES ($1, $2, 'comment', $3, $4, 'sent', $5)`, w.org.ID, v.ID, is.ID, "old:"+uuid.NewString(), w.clk.now())
	}
	w.comment("chen", is.Identifier, "capped")
	w.consume()
	w.pass()
	var skip string
	if err := w.pool.QueryRow(context.Background(), `SELECT skip_reason FROM personal_webhook_outbox WHERE state = 'skipped'`).Scan(&skip); err != nil || skip != "rate_cap" {
		t.Fatalf("skip = %q (%v), want rate_cap", skip, err)
	}
	w.clearP()

	// A 500 retries; a 410 is dead at once with a fixed word on the webhook.
	w.slack.set(500, "")
	w.comment("chen", is.Identifier, "retry")
	w.consume()
	if res := w.pass(); res.Retried != 1 {
		t.Fatalf("pass %+v", res)
	}
	w.wantP("alice:comment:pending")
	w.clearP()
	w.slack.set(410, "")
	w.comment("chen", is.Identifier, "gone")
	w.consume()
	if res := w.pass(); res.Dead != 1 {
		t.Fatalf("pass %+v", res)
	}
	l, err := w.pw.List(context.Background(), w.org.ID, w.user["alice"].UserID)
	if err != nil || len(l.Items) != 1 || l.Items[0].LastError == nil || *l.Items[0].LastError != notify.ChatGone {
		t.Fatalf("list %+v (%v)", l, err)
	}
}

// The sealed URL is bound to its row: a blob copied to another webhook (of
// another member) does not open, and the agent cannot be a recipient.
func TestPersonal_SealScopeAndAgents(t *testing.T) {
	w := newPersonalWorld(t)
	a := w.hook("alice", "Mine", nil)
	b := w.hook("bob", "Bob", nil)
	w.exec(`UPDATE personal_webhooks SET webhook_enc = (SELECT webhook_enc FROM personal_webhooks WHERE id = $1) WHERE id = $2`, a.ID, b.ID)
	l, err := w.pw.List(context.Background(), w.org.ID, w.user["bob"].UserID)
	if err != nil || len(l.Items) != 1 || !l.Items[0].SecretUnreadable {
		t.Fatalf("a copied blob opens: %+v (%v)", l, err)
	}
	// The stored blob does not hold the URL in clear.
	var n int
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM personal_webhooks WHERE position($1::bytea in webhook_enc) > 0`, []byte(pwhToken)).Scan(&n); err != nil || n != 0 {
		t.Fatalf("clear text in blob: %d (%v)", n, err)
	}
	// An agent assigned an issue gets no row even if a row were stored for it:
	// the eligibility filter is the one of inbox and mail (agents never).
	w.exec(`INSERT INTO personal_webhooks (org_id, user_id, name, kind, host, webhook_enc, seal_scope)
		SELECT org_id, $2, 'Agent', kind, host, webhook_enc, 'x' FROM personal_webhooks WHERE id = $1`, a.ID, w.user["claude"].UserID)
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "claude"})
	w.consume()
	w.wantP()
	// No event was emitted for any of the personal rows above.
	var evs int
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE type NOT LIKE 'issue.%'`).Scan(&evs); err != nil || evs != 0 {
		t.Fatalf("events %d (%v)", evs, err)
	}
}
