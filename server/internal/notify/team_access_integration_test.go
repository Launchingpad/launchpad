package notify_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"devops/internal/auth"
	"devops/internal/notify"
	"devops/internal/testdb"
	"devops/internal/tracker"
)

// Ticket 7 of decisions-T20 (LAUN-223): a member without access to the issue's
// team, or to the project, gets no notification of any kind, no mention
// subscription and no mail (#45, #61, #63). Admins and 'all' members are
// unchanged; a restricted member of the team is notified; the chat services
// keep their rules (a shared channel is not a person).

// restrict makes a member 'selected' with the given teams (none: no team).
func (w *world) restrict(name string, teams ...string) {
	w.t.Helper()
	var ids []any
	for _, k := range teams {
		var id any
		if err := w.pool.QueryRow(context.Background(), `SELECT id FROM teams WHERE org_id = $1 AND key = $2`, w.org.ID, k).Scan(&id); err != nil {
			w.t.Fatal(err)
		}
		ids = append(ids, id)
	}
	w.exec(`UPDATE org_members SET team_access = 'selected' WHERE org_id = $1 AND user_id = $2`, w.org.ID, w.user[name].UserID)
	for _, id := range ids {
		w.exec(`INSERT INTO team_members (org_id, user_id, team_id) VALUES ($1, $2, $3)`, w.org.ID, w.user[name].UserID, id)
	}
}

// TestConsume_TeamAccess: alice is restricted to no team, bob to ORB, carol
// is 'all', dave is an admin whose flag says 'selected' with no team (the
// role decides, #2). Every kind is tried.
func TestConsume_TeamAccess(t *testing.T) {
	w := newWorld(t)
	w.addMember(w.org.ID, "dave")
	w.restrict("alice")
	w.restrict("bob", testdb.TeamKey)
	w.exec(`UPDATE org_members SET role = 'admin', team_access = 'selected' WHERE org_id = $1 AND user_id = $2`, w.org.ID, w.user["dave"].UserID)

	// assigned: alice gets nothing; bob, carol and dave do.
	for _, who := range []string{"alice", "bob", "carol", "dave", "claude"} {
		w.createIssue("chen", tracker.CreateIssueInput{Assignee: who})
	}
	w.run()
	w.wantRows("bob:assigned", "carol:assigned", "dave:assigned")
	w.clear()

	// new_issue: every one of them follows the team.
	for _, who := range []string{"alice", "bob", "carol", "dave"} {
		w.exec(`INSERT INTO team_follows (org_id, user_id, team_id) VALUES ($1, $2, $3)`, w.org.ID, w.user[who].UserID, w.teamID())
	}
	is := w.createIssue("chen", tracker.CreateIssueInput{})
	w.run()
	w.wantRows("bob:new_issue", "carol:new_issue", "dave:new_issue")
	w.clear()
	w.exec(`DELETE FROM team_follows`)

	// comment and state change: alice is the creator, an explicit subscriber
	// and a commenter; bob and carol are subscribers.
	w.exec(`UPDATE issues SET creator_id = $2 WHERE id = $1`, is.ID, w.user["alice"].UserID)
	w.exec(`INSERT INTO comments (org_id, issue_id, author_id, body) VALUES ($1, $2, $3, 'old')`, w.org.ID, is.ID, w.user["alice"].UserID)
	for _, who := range []string{"alice", "bob", "carol"} {
		w.exec(`INSERT INTO issue_subscriptions (org_id, issue_id, user_id, state, reason) VALUES ($1, $2, $3, 'subscribed', 'manual')`,
			w.org.ID, is.ID, w.user[who].UserID)
	}
	w.comment("chen", is.Identifier, "a note")
	w.run()
	w.wantRows("bob:comment", "carol:comment")
	w.clear()
	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{State: str("In Progress")})
	w.run()
	w.wantRows("bob:state_change", "carol:state_change")
	w.clear()

	// mention: a direct mention of alice makes no notification and no
	// mention subscription; dave (admin) and a new mention of bob do.
	w.exec(`DELETE FROM issue_subscriptions`)
	w.comment("chen", is.Identifier, "ping @alice @bob @dave")
	w.run()
	w.wantRows("bob:mention", "dave:mention")
	var n int
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM issue_subscriptions WHERE user_id = $1`, w.user["alice"].UserID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("alice has %d subscription rows (%v), want none", n, err)
	}
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM issue_subscriptions WHERE user_id = $1 AND reason = 'mentioned'`, w.user["bob"].UserID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("bob has %d mention subscriptions (%v), want 1", n, err)
	}

	// Access given back: a new event reaches alice again (nothing was deleted).
	w.clear()
	w.exec(`INSERT INTO team_members (org_id, user_id, team_id) VALUES ($1, $2, $3)`, w.org.ID, w.user["alice"].UserID, w.teamID())
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.run()
	w.wantRows("alice:assigned")

	// The logs name no user or issue text (the notify rule of AGENTS.md).
	if strings.Contains(w.logs.String(), "ping") || strings.Contains(w.logs.String(), "@example.test") {
		t.Fatalf("logs hold text: %s", w.logs.String())
	}
}

// TestConsume_TeamAccess_ProjectDue: the reminder goes to a lead who sees the
// project (#36, #61): a project of a team the lead cannot see gives nothing,
// a project with no team is org-level, and an 'all' lead is unchanged.
func TestConsume_TeamAccess_ProjectDue(t *testing.T) {
	w := newWorld(t)
	w.restrict("alice")
	w.restrict("bob", testdb.TeamKey)
	at := utc(2026, 10, 28, 12, 0)
	teamed := w.project("Teamed", "alice", "weekly", at)
	w.exec(`INSERT INTO project_teams (org_id, project_id, team_id) VALUES ($1, $2, $3)`, w.org.ID, teamed.ID, w.teamID())
	byBob := w.project("Bob's", "bob", "weekly", at)
	w.exec(`INSERT INTO project_teams (org_id, project_id, team_id) VALUES ($1, $2, $3)`, w.org.ID, byBob.ID, w.teamID())
	w.project("Open", "alice", "weekly", at) // no team: org-level
	w.project("Carol's", "carol", "weekly", at)
	w.job(utc(2026, 10, 30, 9, 0))
	w.run()
	got := w.rows(w.org.ID)
	if len(got) != 3 {
		t.Fatalf("rows = %v, want alice (open project), bob and carol", got)
	}
	var keys []string
	if err := w.pool.QueryRow(context.Background(), `SELECT array_agg(p.name ORDER BY p.name) FROM notifications n JOIN projects p ON p.id = n.project_id`).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if strings.Join(keys, ",") != "Bob's,Carol's,Open" {
		t.Fatalf("projects reminded = %v, want Bob's, Carol's and Open (not Teamed)", keys)
	}
}

// TestSender_AccessRemoved (#63): access removed after the row was made skips
// the mail with access_removed, for an issue and for a project; nothing is
// sent, and the recipient who still sees it gets the mail.
func TestSender_AccessRemoved(t *testing.T) {
	sw := newSWorld(t)
	sw.restrict("bob", testdb.TeamKey)
	sw.assign("a secret title", "alice")
	sw.assign("bob's issue", "bob")
	at := utc(2026, 10, 28, 12, 0)
	p := sw.project("Secret plan", "carol", "weekly", at)
	sw.exec(`INSERT INTO project_teams (org_id, project_id, team_id) VALUES ($1, $2, $3)`, sw.org.ID, p.ID, sw.teamID())
	sw.job(utc(2026, 10, 30, 9, 0))
	sw.run()
	// Access goes after the rows were made.
	sw.restrict("alice")
	sw.restrict("carol")
	sw.clk.advance(3 * time.Minute)
	res := sw.pass()
	if res.Skipped != 2 || res.Mails != 1 {
		t.Fatalf("result %+v, want two skipped and one mail", res)
	}
	if n := sw.countOutbox("skipped", "access_removed"); n != 2 {
		t.Fatalf("outbox %+v, want two rows skipped with access_removed", sw.outbox())
	}
	ms := sw.m.sent()
	if len(ms) != 1 || !strings.Contains(ms[0].Text, "bob's issue") {
		t.Fatalf("sent %d mails, want bob's only", len(ms))
	}
	for _, m := range ms {
		if strings.Contains(m.Text+m.HTML+m.Subject, "secret") || strings.Contains(m.Text+m.HTML+m.Subject, "Secret") {
			t.Fatalf("a mail names a hidden issue or project: %s", m.Subject)
		}
	}
	// The inbox rows stay (#51); the inbox routes hide them (ticket 7 api tests).
	var n int
	if err := sw.pool.QueryRow(context.Background(), `SELECT count(*) FROM notifications`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("notifications %d (%v), want 3", n, err)
	}
	if strings.Contains(sw.slog.String(), "secret") || strings.Contains(sw.slog.String(), "@example.test") {
		t.Fatalf("sender log holds text: %s", sw.slog.String())
	}
}

// TestChat_TeamAccessDoesNotApply: a shared channel keeps its rules. A
// mention of a member without access still posts the mention kind, and the
// reminder of a project led by a member without access still posts, while
// the person gets nothing.
func TestChat_TeamAccessDoesNotApply(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "mention", "project_update_due")
	w.restrict("alice")
	is := w.createIssue("chen", tracker.CreateIssueInput{})
	w.consume()
	w.exec(`DELETE FROM chat_outbox`)
	w.comment("chen", is.Identifier, "ping @alice")
	w.consume()
	if r := w.onlyRow(); r.Kind != "mention" {
		t.Fatalf("row %+v, want a mention post", r)
	}
	if got := w.got(); len(got) != 0 {
		t.Fatalf("alice has notifications: %v", got)
	}
	w.exec(`DELETE FROM chat_outbox`)
	p := w.project("Zeta", "alice", "weekly", utc(2026, 10, 28, 12, 0))
	w.exec(`INSERT INTO project_teams (org_id, project_id, team_id) VALUES ($1, $2, $3)`, w.org.ID, p.ID, w.teamOf(w.org.ID))
	w.job(utc(2026, 10, 30, 9, 0))
	w.consume()
	if r := w.onlyRow(); r.Kind != "project_update_due" {
		t.Fatalf("row %+v, want the reminder post", r)
	}
	if got := w.got(); len(got) != 0 {
		t.Fatalf("alice has notifications: %v", got)
	}
}

// TestChat_MentionPostsDoNotDependOnAccess (review of LAUN-223): the chat
// decides that a mention is new from its own record, so the same edits post
// the same number of mentions whether the mentioned member sees the team or
// not (a post count is no access probe, #45). A member who sees it still gets
// one notification.
func TestChat_MentionPostsDoNotDependOnAccess(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "mention")
	w.restrict("alice")               // no team
	w.restrict("bob", testdb.TeamKey) // sees ORB
	posts := map[string]int{}
	for _, who := range []string{"alice", "bob"} {
		is := w.createIssue("chen", tracker.CreateIssueInput{})
		w.consume()
		w.exec(`DELETE FROM chat_outbox`)
		cm := w.comment("chen", is.Identifier, "ping @"+who)
		w.consume()
		w.editComment("chen", cm.ID, "ping @"+who+" again")
		w.consume()
		w.editComment("chen", cm.ID, "ping @"+who+" once more")
		w.consume()
		posts[who] = len(w.outRows())
	}
	if posts["alice"] != 1 || posts["bob"] != 1 {
		t.Fatalf("mention posts = %v, want 1 for each (the same for access and no access)", posts)
	}
	w.wantRows("bob:mention")
	// The chat record holds the key only: ids, no text.
	var keys string
	if err := w.pool.QueryRow(context.Background(), `SELECT string_agg(dedupe_key, ',') FROM chat_mentions`).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(keys, "ping") || strings.Contains(keys, "alice") || strings.Contains(keys, "@") {
		t.Fatalf("chat_mentions holds text: %s", keys)
	}
}

// TestSettings_TeamWriteAfterAccessRemoved (review of LAUN-223): the team was
// resolved before the write's transaction; access gone in between must not
// clear the stored column the body did not name.
func TestSettings_TeamWriteAfterAccessRemoved(t *testing.T) {
	w := newWorld(t)
	w.restrict("alice")
	alice := w.user["alice"].UserID
	w.exec(`INSERT INTO notification_settings (org_id, user_id, team_id, event, email, inbox) VALUES ($1, $2, $3, 'comment', 'off', 'on')`,
		w.org.ID, alice, w.teamID())
	org := &auth.Org{ID: w.org.ID, Slug: w.org.Slug, Name: w.org.Name, Role: "member"} // sees no team now
	if _, err := notify.New(w.pool).UpdateTeam(context.Background(), org, alice, w.teamID(), notify.TeamSettingsInput{
		Events: map[string]notify.EventInput{"comment": {Email: str("on")}}}); err != nil {
		t.Fatal(err)
	}
	var email, inbox string
	if err := w.pool.QueryRow(context.Background(), `SELECT coalesce(email, 'null'), coalesce(inbox, 'null') FROM notification_settings
		WHERE user_id = $1 AND team_id = $2 AND event = 'comment'`, alice, w.teamID()).Scan(&email, &inbox); err != nil {
		t.Fatal(err)
	}
	if email != "on" || inbox != "on" {
		t.Fatalf("row email=%s inbox=%s, want on and on (inbox kept)", email, inbox)
	}
}
