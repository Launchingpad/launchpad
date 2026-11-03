package notify_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"devops/internal/dbq"
	"devops/internal/tracker"
)

// LAUN-208, decisions-T37: a chat service posts every ticked kind whoever the
// issue belongs to and whoever acted. Mute still wins. Email, inbox and push
// keep their rules (decisions-T13 #14).

func (w *swo) pushRows() int {
	w.t.Helper()
	var n int
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM push_outbox`).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func (w *swo) kinds() []string {
	var out []string
	for _, r := range w.outRows() {
		out = append(out, r.Kind)
	}
	slices.Sort(out)
	return out
}

// The ticket's case: an agent moves an issue nobody owns or follows.
func TestChat_AgentStateChangeOnUnassignedIssuePostsOnce(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "state_change")
	is := w.createIssue("claude", tracker.CreateIssueInput{}) // agent creator, no assignee
	w.consume()
	w.exec(`DELETE FROM chat_outbox`)
	w.updateIssue("claude", is.Identifier, tracker.UpdateIssueInput{State: str("In Progress")})
	w.consume()
	if r := w.onlyRow(); r.Kind != "state_change" || r.State != "pending" {
		t.Fatalf("row %+v", r)
	}
	if res := w.pass(); res.Sent != 1 {
		t.Fatalf("pass %+v", res)
	}
	if res := w.pass(); res.Sent != 0 || len(w.slack.got()) != 1 {
		t.Fatalf("a second pass posted again: %+v", res)
	}
	// Email, inbox and push are unchanged: nobody is eligible, so nobody is told.
	if got := w.got(); len(got) != 0 {
		t.Errorf("notifications = %v, want none", got)
	}
	if o := w.outbox(); len(o) != 0 {
		t.Errorf("mail outbox = %+v, want none", o)
	}
	if n := w.pushRows(); n != 0 {
		t.Errorf("push rows = %d, want 0", n)
	}
	// A replay of the feed makes no second row.
	w.exec(`UPDATE notification_cursors SET last_seq = 0`)
	w.consume()
	var n int
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM chat_outbox WHERE kind = 'state_change'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("state_change rows after replay = %d, want 1", n)
	}
}

func TestChat_HumanStateChangeOnUnassignedIssuePosts(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "state_change")
	is := w.createIssue("chen", tracker.CreateIssueInput{})
	w.consume()
	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{State: str("In Progress")}) // the actor is the only person
	w.consume()
	if r := w.onlyRow(); r.Kind != "state_change" {
		t.Fatalf("row %+v", r)
	}
	if got := w.got(); len(got) != 0 { // chen made it: the actor is not told (unchanged)
		t.Errorf("notifications = %v, want none", got)
	}
}

// Every kind an event makes is posted, with nobody to tell.
func TestChat_NewIssueAndAssignedNeedNoFollower(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "new_issue", "assigned")
	w.createIssue("chen", tracker.CreateIssueInput{}) // no follower, no assignee
	w.consume()
	if r := w.onlyRow(); r.Kind != "new_issue" {
		t.Fatalf("row %+v", r)
	}
	w.exec(`DELETE FROM chat_outbox`)
	is := w.createIssue("claude", tracker.CreateIssueInput{Assignee: "claude"})
	w.consume()
	if k := w.kinds(); !slices.Equal(k, []string{"assigned", "new_issue"}) {
		t.Fatalf("kinds %v", k)
	}
	w.exec(`DELETE FROM chat_outbox`)
	w.updateIssue("claude", is.Identifier, tracker.UpdateIssueInput{Assignee: str("alice")}) // assigned only when set
	w.consume()
	if r := w.onlyRow(); r.Kind != "assigned" {
		t.Fatalf("row %+v", r)
	}
	if got := w.got(); !slices.Equal(got, []string{"alice:assigned"}) {
		t.Errorf("notifications = %v (unchanged rule)", got)
	}
}

func TestChat_MutedIssuePostsNothing(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "state_change", "comment", "assigned", "mention")
	is := w.createIssue("claude", tracker.CreateIssueInput{})
	w.consume()
	w.exec(`DELETE FROM chat_outbox`)
	w.exec(`INSERT INTO issue_subscriptions (org_id, issue_id, user_id, state, reason) VALUES ($1, $2, $3, 'muted', 'manual')`,
		w.org.ID, is.ID, w.user["bob"].UserID)
	w.updateIssue("claude", is.Identifier, tracker.UpdateIssueInput{State: str("In Progress")})
	w.comment("claude", is.Identifier, "hello @alice") // even a mention
	w.updateIssue("claude", is.Identifier, tracker.UpdateIssueInput{Assignee: str("carol")})
	w.consume()
	if rows := w.outRows(); len(rows) != 0 {
		t.Fatalf("a muted issue posted: %+v", rows)
	}
	// The personal rules are unchanged: alice is mentioned and carol assigned; bob muted.
	got := w.got()
	for _, want := range []string{"alice:mention", "carol:assigned"} {
		if !slices.Contains(got, want) {
			t.Errorf("notifications = %v, want %s", got, want)
		}
	}
	for _, g := range got {
		if strings.HasPrefix(g, "bob:") {
			t.Errorf("muted bob was told: %s", g)
		}
	}
	// Unmuted, the issue posts again.
	w.exec(`DELETE FROM issue_subscriptions WHERE issue_id = $1`, is.ID)
	w.updateIssue("claude", is.Identifier, tracker.UpdateIssueInput{State: str("Done")})
	w.consume()
	if r := w.onlyRow(); r.Kind != "state_change" {
		t.Fatalf("row %+v", r)
	}
}

// The mute of a person who is no longer an active member, or an agent, does
// not silence the channel.
func TestChat_MuteOfInactiveMemberOrAgentIsIgnored(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "state_change")
	is := w.createIssue("claude", tracker.CreateIssueInput{})
	w.consume()
	for _, u := range []string{"bob", "claude"} {
		w.exec(`INSERT INTO issue_subscriptions (org_id, issue_id, user_id, state, reason) VALUES ($1, $2, $3, 'muted', 'manual')`,
			w.org.ID, is.ID, w.user[u].UserID)
	}
	w.exec(`UPDATE users SET deactivated_at = now() WHERE id = $1`, w.user["bob"].UserID)
	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{State: str("In Progress")})
	w.consume()
	if r := w.onlyRow(); r.Kind != "state_change" {
		t.Fatalf("row %+v", r)
	}
}

func TestChat_UntickedKindPostsNothing(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "comment")
	is := w.createIssue("claude", tracker.CreateIssueInput{Assignee: "alice"})
	w.updateIssue("claude", is.Identifier, tracker.UpdateIssueInput{State: str("In Progress")})
	w.consume()
	if rows := w.outRows(); len(rows) != 0 {
		t.Fatalf("unticked kinds posted: %+v", rows)
	}
	w.putChannel(w.org.ID, hookURL) // none ticked
	w.comment("claude", is.Identifier, "x")
	w.consume()
	if rows := w.outRows(); len(rows) != 0 {
		t.Fatalf("an empty list posted: %+v", rows)
	}
}

// A mention keeps its meaning: it posts only for an eligible person.
func TestChat_MentionNeedsAnEligiblePerson(t *testing.T) {
	w := newChatWorld(t)
	w.putChannel(w.org.ID, hookURL, "mention")
	is := w.createIssue("claude", tracker.CreateIssueInput{})
	w.consume()
	w.comment("claude", is.Identifier, "@claude @chen_nobody")
	w.consume()
	if rows := w.outRows(); len(rows) != 0 {
		t.Fatalf("a mention of no eligible person posted: %+v", rows)
	}
	w.comment("claude", is.Identifier, "@alice")
	w.consume()
	if r := w.onlyRow(); r.Kind != "mention" {
		t.Fatalf("row %+v", r)
	}
}

// Another org's issue, mute and services never reach this org's channel.
func TestChat_EveryEventIsolatedByOrg(t *testing.T) {
	w := newChatWorld(t)
	ctx := context.Background()
	org2, err := w.q.InsertOrg(ctx, dbq.InsertOrgParams{Slug: "other", Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tracker.CreateTeamTx(ctx, w.q, org2.ID, "OTH", "Other", ""); err != nil {
		t.Fatal(err)
	}
	w.addMember(org2.ID, "chen")
	w.addMember(org2.ID, "claude")
	w.putChannel(w.org.ID, hookURL, "new_issue")
	w.putService(org2.ID, "slack", "Slack", "https://hooks.slack.com/services/T0002/B0002/OTHERORGTOKEN", "new_issue")
	o2 := *w.org
	o2.ID, o2.Slug = org2.ID, "other"
	if _, err := w.svc.CreateIssue(ctx, &o2, w.user["claude"], tracker.CreateIssueInput{Team: "OTH", Title: "t"}); err != nil {
		t.Fatal(err)
	}
	w.consume()
	var n1, n2 int
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM chat_outbox WHERE org_id = $1`, w.org.ID).Scan(&n1); err != nil {
		t.Fatal(err)
	}
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM chat_outbox WHERE org_id = $1`, org2.ID).Scan(&n2); err != nil {
		t.Fatal(err)
	}
	if n1 != 0 || n2 != 1 {
		t.Fatalf("org 1 rows = %d (want 0), org 2 rows = %d (want 1)", n1, n2)
	}
}
