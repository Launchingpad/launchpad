package notify_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/auth"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/httpx"
	"devops/internal/notify"
	"devops/internal/testdb"
	"devops/internal/tracker"
)

// world is one org with people. chen is the org admin (fixture), claude the
// fixture agent; the rest are made here.
type world struct {
	t    *testing.T
	pool *pgxpool.Pool
	q    *dbq.Queries
	svc  *tracker.Service
	org  *auth.Org
	logs *bytes.Buffer
	c    *notify.Consumer
	user map[string]*auth.Principal
}

func newWorld(t *testing.T) *world {
	t.Helper()
	pool, _ := testdb.New(t)
	d := testdb.Fixture(t, pool)
	w := &world{t: t, pool: pool, q: dbq.New(pool), svc: tracker.New(pool), logs: &bytes.Buffer{}, user: map[string]*auth.Principal{}}
	w.org = &auth.Org{ID: d.OrgID, Slug: testdb.OrgSlug, Name: testdb.OrgName, Role: "admin", Access: auth.TeamAccess{All: true}}
	w.c = notify.NewConsumer(pool, slog.New(slog.NewTextHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	for _, name := range []string{"chen", "claude"} {
		var id uuid.UUID
		var agent bool
		if err := pool.QueryRow(context.Background(), `SELECT id, is_agent FROM users WHERE username = $1`, name).Scan(&id, &agent); err != nil {
			t.Fatal(err)
		}
		w.user[name] = &auth.Principal{UserID: id, Username: name, Name: name, IsAgent: agent}
	}
	for _, name := range []string{"alice", "bob", "carol"} {
		w.addMember(w.org.ID, name)
	}
	return w
}

// addMember makes (or finds) a human user and adds it to org.
func (w *world) addMember(org uuid.UUID, name string) *auth.Principal {
	w.t.Helper()
	ctx := context.Background()
	p, ok := w.user[name]
	if !ok {
		email := name + "@example.test"
		u, err := w.q.InsertUser(ctx, dbq.InsertUserParams{Username: name, Name: name, Email: &email})
		if err != nil {
			w.t.Fatal(err)
		}
		p = &auth.Principal{UserID: u.ID, Username: name, Name: name}
		w.user[name] = p
	}
	if err := w.q.InsertOrgMember(ctx, dbq.InsertOrgMemberParams{OrgID: org, UserID: p.UserID, Role: "member"}); err != nil {
		w.t.Fatal(err)
	}
	return p
}

func (w *world) exec(sql string, args ...any) { testdb.MustExec(w.t, w.pool, sql, args...) }

func (w *world) run() notify.Result {
	w.t.Helper()
	res, err := w.c.RunOnce(context.Background())
	if err != nil {
		w.t.Fatalf("RunOnce: %v", err)
	}
	return res
}

func (w *world) createIssue(by string, in tracker.CreateIssueInput) tracker.Issue {
	w.t.Helper()
	if in.Team == "" {
		in.Team = testdb.TeamKey
	}
	if in.Title == "" {
		in.Title = "an issue"
	}
	is, err := w.svc.CreateIssue(context.Background(), w.org, w.user[by], in)
	if err != nil {
		w.t.Fatalf("CreateIssue: %v", err)
	}
	return is
}

func (w *world) updateIssue(by, ref string, in tracker.UpdateIssueInput) {
	w.t.Helper()
	if _, err := w.svc.UpdateIssue(context.Background(), w.org, w.user[by], ref, in); err != nil {
		w.t.Fatalf("UpdateIssue: %v", err)
	}
}

func (w *world) comment(by, issue, body string) tracker.Comment {
	w.t.Helper()
	c, err := w.svc.CreateComment(context.Background(), w.org, w.user[by], issue, tracker.CommentInput{Body: body})
	if err != nil {
		w.t.Fatalf("CreateComment: %v", err)
	}
	return c
}

func (w *world) editComment(by string, id uuid.UUID, body string) {
	w.t.Helper()
	if _, err := w.svc.UpdateComment(context.Background(), w.org, w.user[by], id, tracker.CommentInput{Body: body}); err != nil {
		w.t.Fatalf("UpdateComment: %v", err)
	}
}

func str(s string) httpx.Opt[string] { return httpx.Opt[string]{Set: true, V: s} }

// row is one notification as the tests compare it.
type row struct {
	To    string
	Kind  string
	Inbox bool
	Email bool // an outbox row exists
	Key   string
}

func (r row) String() string {
	return fmt.Sprintf("%s:%s inbox=%v email=%v", r.To, r.Kind, r.Inbox, r.Email)
}

// rows lists every notification of org, ordered by recipient, kind, key. The
// key has the event seq replaced by "#" so a test need not know it.
func (w *world) rows(org uuid.UUID) []row {
	w.t.Helper()
	rs, err := w.pool.Query(context.Background(), `
		SELECT u.username, n.kind, n.inbox, EXISTS (SELECT 1 FROM notification_emails e WHERE e.notification_id = n.id), n.dedupe_key
		FROM notifications n JOIN users u ON u.id = n.recipient_id
		WHERE n.org_id = $1 ORDER BY u.username, n.kind, n.dedupe_key`, org)
	if err != nil {
		w.t.Fatal(err)
	}
	defer rs.Close()
	var out []row
	for rs.Next() {
		var r row
		if err := rs.Scan(&r.To, &r.Kind, &r.Inbox, &r.Email, &r.Key); err != nil {
			w.t.Fatal(err)
		}
		if strings.HasPrefix(r.Key, "evt:") {
			parts := strings.Split(r.Key, ":")
			r.Key = "evt:#:" + parts[2]
		}
		out = append(out, r)
	}
	return out
}

// got returns "to:kind" labels, for short assertions.
func (w *world) got() []string {
	var out []string
	for _, r := range w.rows(w.org.ID) {
		out = append(out, r.To+":"+r.Kind)
	}
	return out
}

func (w *world) wantRows(want ...string) {
	w.t.Helper()
	got := w.got()
	slices.Sort(want)
	if !slices.Equal(got, want) {
		w.t.Errorf("notifications = %v, want %v", got, want)
	}
}

func (w *world) clear() {
	w.exec(`DELETE FROM notifications`)
}

// --- the mapping table (decisions-T13 #12): one test per row -----------------

func TestConsume_IssueCreatedWithAssignee(t *testing.T) {
	w := newWorld(t)
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.run()
	rs := w.rows(w.org.ID)
	want := []row{{"alice", "assigned", true, true, "evt:#:assigned"}}
	if fmt.Sprint(rs) != fmt.Sprint(want) {
		t.Errorf("rows = %v, want %v", rs, want)
	}
	// The key is the event seq and the kind.
	var key string
	var seq int64
	if err := w.pool.QueryRow(context.Background(), `SELECT dedupe_key, event_seq FROM notifications`).Scan(&key, &seq); err != nil {
		t.Fatal(err)
	}
	if key != fmt.Sprintf("evt:%d:assigned", seq) {
		t.Errorf("dedupe key = %q, event_seq = %d", key, seq)
	}
}

func TestConsume_IssueCreatedNewIssueForFollowers(t *testing.T) {
	w := newWorld(t)
	team := w.teamID()
	for _, u := range []string{"alice", "bob"} {
		w.exec(`INSERT INTO team_follows (org_id, user_id, team_id) VALUES ($1, $2, $3)`, w.org.ID, w.user[u].UserID, team)
	}
	// carol follows nothing; alice is the actor.
	w.createIssue("alice", tracker.CreateIssueInput{})
	w.run()
	rs := w.rows(w.org.ID)
	want := []row{{"bob", "new_issue", true, false, "evt:#:new_issue"}}
	if fmt.Sprint(rs) != fmt.Sprint(want) {
		t.Errorf("rows = %v, want %v", rs, want)
	}
}

func TestConsume_NewIssueNeedsAFollow(t *testing.T) {
	w := newWorld(t)
	// An explicit "on" without a follow still makes nothing.
	w.exec(`INSERT INTO notification_settings (org_id, user_id, event, email, inbox) VALUES ($1, $2, 'new_issue', 'on', 'on')`,
		w.org.ID, w.user["bob"].UserID)
	w.createIssue("chen", tracker.CreateIssueInput{})
	w.run()
	w.wantRows()
}

func TestConsume_IssueUpdatedAssignee(t *testing.T) {
	w := newWorld(t)
	is := w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.run()
	w.clear()
	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{Assignee: str("bob")})
	w.run()
	// The NEW assignee only. alice (the old one) got nothing: no state change.
	rs := w.rows(w.org.ID)
	want := []row{{"bob", "assigned", true, true, "evt:#:assigned"}}
	if fmt.Sprint(rs) != fmt.Sprint(want) {
		t.Errorf("rows = %v, want %v", rs, want)
	}
}

// Two quick reassignments: each event names its own assignee, so the first
// one notifies the first person even though the issue has moved on.
func TestConsume_ReassignedTwiceBeforeTheConsumerRuns(t *testing.T) {
	w := newWorld(t)
	is := w.createIssue("chen", tracker.CreateIssueInput{})
	w.run()
	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{Assignee: str("alice")})
	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{Assignee: str("bob")})
	w.run()
	w.wantRows("alice:assigned", "bob:assigned")
}

func TestConsume_IssueUpdatedStateChange(t *testing.T) {
	w := newWorld(t)
	is := w.createIssue("alice", tracker.CreateIssueInput{Assignee: "bob"}) // alice creator, bob assignee
	w.run()
	w.clear()
	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{State: str("In Progress")})
	w.run()
	rs := w.rows(w.org.ID)
	want := []row{
		{"alice", "state_change", true, false, "evt:#:state_change"},
		{"bob", "state_change", true, false, "evt:#:state_change"},
	}
	if fmt.Sprint(rs) != fmt.Sprint(want) {
		t.Errorf("rows = %v, want %v", rs, want)
	}
	var data string
	if err := w.pool.QueryRow(context.Background(), `SELECT data::text FROM notifications LIMIT 1`).Scan(&data); err != nil {
		t.Fatal(err)
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(data), &d); err != nil || d["fromStateId"] == nil || d["toStateId"] == nil {
		t.Errorf("data = %s (%v), want fromStateId and toStateId", data, err)
	}
}

func TestConsume_CommentCreated(t *testing.T) {
	w := newWorld(t)
	is := w.createIssue("alice", tracker.CreateIssueInput{Assignee: "bob"})
	w.run()
	w.clear()
	cm := w.comment("chen", is.Identifier, "looks fine")
	w.run()
	rs := w.rows(w.org.ID)
	want := []row{
		{"alice", "comment", true, true, "evt:#:comment"},
		{"bob", "comment", true, true, "evt:#:comment"},
	}
	if fmt.Sprint(rs) != fmt.Sprint(want) {
		t.Errorf("rows = %v, want %v", rs, want)
	}
	var data string
	if err := w.pool.QueryRow(context.Background(), `SELECT data->>'commentId' FROM notifications LIMIT 1`).Scan(&data); err != nil || data != cm.ID.String() {
		t.Errorf("commentId = %q (%v), want %s", data, err, cm.ID)
	}
	// A commenter becomes an implicit subscriber: carol comments, then chen
	// comments again and carol hears it.
	w.comment("carol", is.Identifier, "me too")
	w.run()
	w.clear()
	w.comment("chen", is.Identifier, "again")
	w.run()
	w.wantRows("alice:comment", "bob:comment", "carol:comment")
}

func TestConsume_MentionInComment(t *testing.T) {
	w := newWorld(t)
	is := w.createIssue("chen", tracker.CreateIssueInput{})
	w.run()
	w.clear()
	cm := w.comment("chen", is.Identifier, "ping @bob")
	w.run()
	rs := w.rows(w.org.ID)
	want := []row{{"bob", "mention", true, true, "mention:c:" + cm.ID.String() + ":" + w.user["bob"].UserID.String()}}
	if fmt.Sprint(rs) != fmt.Sprint(want) {
		t.Errorf("rows = %v, want %v", rs, want)
	}
	// A mention adds a subscribed row (#9).
	var n int
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM issue_subscriptions WHERE user_id = $1 AND state = 'subscribed' AND reason = 'mentioned'`,
		w.user["bob"].UserID).Scan(&n); err != nil || n != 1 {
		t.Errorf("mentioned subscription rows = %d (%v), want 1", n, err)
	}
}

// An edit that adds a name notifies only the new name; an edit that changes
// the text but not the names notifies nobody.
func TestConsume_CommentEditNotifiesOnlyNewMentions(t *testing.T) {
	w := newWorld(t)
	is := w.createIssue("chen", tracker.CreateIssueInput{})
	cm := w.comment("chen", is.Identifier, "ping @bob")
	w.run()
	w.wantRows("bob:mention")
	w.editComment("chen", cm.ID, "ping @bob and @carol")
	w.run()
	w.wantRows("bob:mention", "carol:mention")
	w.editComment("chen", cm.ID, "different words for @bob and @carol")
	w.run()
	w.wantRows("bob:mention", "carol:mention")
}

func TestConsume_MentionInIssueDescription(t *testing.T) {
	w := newWorld(t)
	is := w.createIssue("chen", tracker.CreateIssueInput{Description: "cc @alice"})
	w.run()
	rs := w.rows(w.org.ID)
	want := []row{{"alice", "mention", true, true, "mention:i:" + is.ID.String() + ":" + w.user["alice"].UserID.String()}}
	if fmt.Sprint(rs) != fmt.Sprint(want) {
		t.Errorf("rows = %v, want %v", rs, want)
	}
	// A description edit that adds a name notifies only the new name.
	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{Description: str("cc @alice and @bob")})
	w.run()
	w.wantRows("alice:mention", "bob:mention")
	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{Description: str("other words, @alice and @bob")})
	w.run()
	w.wantRows("alice:mention", "bob:mention")
}

func TestConsume_NotMentions(t *testing.T) {
	w := newWorld(t)
	is := w.createIssue("chen", tracker.CreateIssueInput{})
	w.comment("chen", is.Identifier, "mail a@bob.example, `@alice`, @nobody\n> @carol\n```\n@alice\n```")
	w.run()
	w.wantRows()
}

// One kind per recipient per event, in the order mention, assigned,
// comment (#13).
func TestConsume_PrecedenceOneKindPerEvent(t *testing.T) {
	w := newWorld(t)
	is := w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.run()
	w.clear()
	// alice is an issue recipient and is mentioned: one row, the mention.
	w.comment("chen", is.Identifier, "@alice please")
	w.run()
	w.wantRows("alice:mention")

	// With mention off but comment on, she hears about the comment instead.
	w.clear()
	w.exec(`INSERT INTO notification_settings (org_id, user_id, event, email, inbox) VALUES ($1, $2, 'mention', 'off', 'off')`,
		w.org.ID, w.user["alice"].UserID)
	w.comment("chen", is.Identifier, "@alice again")
	w.run()
	w.wantRows("alice:comment")

	// All kinds off for both channels: no row.
	w.clear()
	w.exec(`INSERT INTO notification_settings (org_id, user_id, event, email, inbox) VALUES ($1, $2, 'comment', 'off', 'off')`,
		w.org.ID, w.user["alice"].UserID)
	w.comment("chen", is.Identifier, "@alice third")
	w.run()
	w.wantRows()
}

// The new assignee gets `assigned` only for an event that changes both the
// assignee and the state; the others get state_change.
func TestConsume_AssignedBeatsStateChangeForTheSameEvent(t *testing.T) {
	w := newWorld(t)
	is := w.createIssue("carol", tracker.CreateIssueInput{Assignee: "alice"})
	w.run()
	w.clear()
	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{Assignee: str("bob"), State: str("In Progress")})
	w.run()
	// carol: creator, alice: old assignee is no longer a recipient.
	w.wantRows("bob:assigned", "carol:state_change")
}

func TestConsume_ChannelsFollowSettings(t *testing.T) {
	w := newWorld(t)
	team := w.teamID()
	is := w.createIssue("alice", tracker.CreateIssueInput{})
	w.run()
	w.clear()
	// Inbox off and email on: an email-only row.
	w.exec(`INSERT INTO notification_settings (org_id, user_id, event, email, inbox) VALUES ($1, $2, 'state_change', 'on', 'off')`,
		w.org.ID, w.user["alice"].UserID)
	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{State: str("In Progress")})
	w.run()
	if rs := w.rows(w.org.ID); len(rs) != 1 || rs[0].Inbox || !rs[0].Email {
		t.Errorf("rows = %v, want one email-only row", rs)
	}
	// The master switch turns email off for every event; inbox stays.
	w.clear()
	w.exec(`INSERT INTO notification_user_settings (org_id, user_id, email_enabled) VALUES ($1, $2, false)`, w.org.ID, w.user["alice"].UserID)
	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{State: str("In Review")})
	w.run()
	// Inbox is off by the stored row and email is off by the master switch.
	if rs := w.rows(w.org.ID); len(rs) != 0 {
		t.Errorf("rows = %v, want none", rs)
	}
	// A team row beats the global row.
	w.clear()
	w.exec(`INSERT INTO notification_settings (org_id, user_id, team_id, event, inbox) VALUES ($1, $2, $3, 'state_change', 'on')`,
		w.org.ID, w.user["alice"].UserID, team)
	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{State: str("Done")})
	w.run()
	if rs := w.rows(w.org.ID); len(rs) != 1 || !rs[0].Inbox || rs[0].Email {
		t.Errorf("rows = %v, want one inbox-only row", rs)
	}
}

// --- who gets nothing -----------------------------------------------------

func TestConsume_RecipientFilters(t *testing.T) {
	w := newWorld(t)
	dave := w.addMember(w.org.ID, "dave") // will be deactivated
	erin := w.addMember(w.org.ID, "erin") // will leave the org
	frank := w.addMember(w.org.ID, "frank")
	_ = frank
	// A stranger: a user of the install who is not a member of this org.
	stranger, err := w.q.InsertUser(context.Background(), dbq.InsertUserParams{Username: "zed", Name: "zed"})
	if err != nil {
		t.Fatal(err)
	}
	_ = stranger
	is := w.createIssue("alice", tracker.CreateIssueInput{Assignee: "bob"})
	w.run()
	w.clear()

	// Everyone is mentioned, plus the agent, the stranger and the actor.
	// bob (muted) is also an issue recipient: only his mention passes.
	w.exec(`INSERT INTO issue_subscriptions (org_id, issue_id, user_id, state, reason) VALUES ($1, $2, $3, 'muted', 'manual')`,
		w.org.ID, is.ID, w.user["bob"].UserID)
	w.exec(`UPDATE users SET deactivated_at = now() WHERE id = $1`, dave.UserID)
	w.exec(`DELETE FROM org_members WHERE org_id = $1 AND user_id = $2`, w.org.ID, erin.UserID)
	w.comment("chen", is.Identifier, "@chen @claude @dave @erin @zed @bob @frank")
	w.run()
	// chen is the actor, claude an agent, dave deactivated, erin and zed
	// not members. bob is muted but mentioned directly. alice (creator)
	// gets the comment; frank the mention. bob's comment row is muted.
	w.wantRows("alice:comment", "bob:mention", "frank:mention")
}

func TestConsume_MutedUserGetsNoOtherKind(t *testing.T) {
	w := newWorld(t)
	team := w.teamID()
	is := w.createIssue("chen", tracker.CreateIssueInput{Assignee: "bob"})
	w.exec(`INSERT INTO team_follows (org_id, user_id, team_id) VALUES ($1, $2, $3)`, w.org.ID, w.user["alice"].UserID, team)
	w.run()
	w.clear()
	for _, u := range []string{"bob", "alice"} {
		w.exec(`INSERT INTO issue_subscriptions (org_id, issue_id, user_id, state, reason) VALUES ($1, $2, $3, 'muted', 'manual')`,
			w.org.ID, is.ID, w.user[u].UserID)
	}
	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{State: str("In Progress")}) // state_change
	w.comment("claude", is.Identifier, "a comment")                                           // comment
	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{Assignee: str("alice")})    // assigned
	w.run()
	// Only chen, the creator, who is not muted, hears claude's comment.
	w.wantRows("chen:comment")
}

func TestConsume_ArchivedIssueAndTeam(t *testing.T) {
	w := newWorld(t)
	is := w.createIssue("alice", tracker.CreateIssueInput{Assignee: "bob"})
	w.run()
	w.clear()
	w.exec(`UPDATE issues SET archived_at = now() WHERE id = $1`, is.ID)
	w.comment("chen", is.Identifier, "@carol hello")
	w.run()
	w.wantRows()

	w.exec(`UPDATE issues SET archived_at = NULL WHERE id = $1`, is.ID)
	w.exec(`UPDATE teams SET archived_at = now() WHERE id = $1`, w.teamID())
	w.comment("chen", is.Identifier, "@carol hello again")
	w.run()
	w.wantRows()
}

func TestConsume_AgentsGetNothing(t *testing.T) {
	w := newWorld(t)
	// An agent is assignee, creator and mentioned.
	is := w.createIssue("claude", tracker.CreateIssueInput{Assignee: "claude", Description: "@claude"})
	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{State: str("In Progress")})
	w.comment("chen", is.Identifier, "@claude look")
	w.exec(`INSERT INTO team_follows (org_id, user_id, team_id) VALUES ($1, $2, $3)`, w.org.ID, w.user["claude"].UserID, w.teamID())
	w.createIssue("chen", tracker.CreateIssueInput{})
	w.run()
	w.wantRows()
}

// --- exactly once, first run, stale, poison ---------------------------------

func TestConsume_ReplayMakesNoDuplicates(t *testing.T) {
	w := newWorld(t)
	team := w.teamID()
	w.exec(`INSERT INTO team_follows (org_id, user_id, team_id) VALUES ($1, $2, $3)`, w.org.ID, w.user["carol"].UserID, team)
	is := w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice", Description: "@bob"})
	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{State: str("In Progress")})
	w.comment("chen", is.Identifier, "@bob and @carol")
	first := w.run()
	before := w.rows(w.org.ID)
	if first.Notifications == 0 {
		t.Fatal("no notifications on the first pass")
	}
	// Reset the cursor and run again: the same rows, no more.
	w.exec(`UPDATE notification_cursors SET last_seq = 0`)
	again := w.run()
	if again.Notifications != 0 || again.Emails != 0 {
		t.Errorf("replay made %d notifications and %d emails, want none", again.Notifications, again.Emails)
	}
	if after := w.rows(w.org.ID); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("rows changed on replay:\n%v\n%v", before, after)
	}
	var emails int
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM notification_emails`).Scan(&emails); err != nil {
		t.Fatal(err)
	}
	var withEmail int
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM notifications n WHERE EXISTS (SELECT 1 FROM notification_emails e WHERE e.notification_id = n.id)`).Scan(&withEmail); err != nil || withEmail != emails {
		t.Errorf("outbox rows = %d, notifications with an outbox row = %d (%v)", emails, withEmail, err)
	}
}

// Two instances on one org make each row once.
func TestConsume_TwoInstancesMakeEachRowOnce(t *testing.T) {
	w := newWorld(t)
	for i := 0; i < 30; i++ {
		is := w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
		w.comment("chen", is.Identifier, "@bob")
	}
	c2 := notify.NewConsumer(w.pool, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	var wg sync.WaitGroup
	for _, c := range []*notify.Consumer{w.c, c2, w.c, c2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				if _, err := c.RunOnce(context.Background()); err != nil {
					t.Errorf("RunOnce: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	w.run()
	var n int
	// 30 issues x (assigned alice + mention bob + comment alice? no: alice is
	// assignee, so she gets the comment) = alice: assigned + comment, bob: mention.
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM notifications`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 30*3 {
		t.Errorf("notifications = %d, want %d", n, 30*3)
	}
	var dup int
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM (SELECT 1 FROM notifications GROUP BY recipient_id, dedupe_key HAVING count(*) > 1) x`).Scan(&dup); err != nil || dup != 0 {
		t.Errorf("duplicate keys = %d (%v)", dup, err)
	}
}

// A held lock makes the second instance skip the org, not wait.
func TestConsume_SecondInstanceSkipsALockedOrg(t *testing.T) {
	w := newWorld(t)
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	ctx := context.Background()
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	locked, err := dbq.New(tx).TryLockNotifyOrg(ctx, w.org.ID.String())
	if err != nil || !locked {
		t.Fatalf("test lock: %v %v", locked, err)
	}
	done := make(chan notify.Result, 1)
	go func() {
		res, _, err := w.c.ConsumeOrg(ctx, w.org.ID)
		if err != nil {
			t.Errorf("ConsumeOrg: %v", err)
		}
		done <- res
	}()
	select {
	case res := <-done:
		if res.Orgs != 0 || res.Events != 0 {
			t.Errorf("result = %+v, want the org skipped", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second instance waited on the lock")
	}
	w.wantRows()
	_ = tx.Rollback(ctx)
	w.run()
	w.wantRows("alice:assigned")
}

func TestConsume_NoHistoryAtFirstRun(t *testing.T) {
	w := newWorld(t)
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	// An org without a cursor starts at its current max seq.
	w.exec(`DELETE FROM notification_cursors`)
	res := w.run()
	w.wantRows()
	if res.Events != 0 {
		t.Errorf("events read = %d, want 0 (no history)", res.Events)
	}
	var last, max int64
	if err := w.pool.QueryRow(context.Background(), `SELECT (SELECT last_seq FROM notification_cursors), (SELECT max(seq) FROM events)`).Scan(&last, &max); err != nil || last != max {
		t.Errorf("cursor = %d, max seq = %d (%v)", last, max, err)
	}
	// New events after that are consumed.
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "bob"})
	w.run()
	w.wantRows("bob:assigned")
}

func TestConsume_StaleEventsAreSkipped(t *testing.T) {
	w := newWorld(t)
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.exec(`UPDATE events SET created_at = now() - interval '25 hours'`)
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "bob"})
	res := w.run()
	w.wantRows("bob:assigned")
	if res.Stale == 0 {
		t.Errorf("stale = %d, want events counted as stale", res.Stale)
	}
	var last, max int64
	if err := w.pool.QueryRow(context.Background(), `SELECT (SELECT last_seq FROM notification_cursors), (SELECT max(seq) FROM events)`).Scan(&last, &max); err != nil || last != max {
		t.Errorf("cursor = %d, max seq = %d (%v)", last, max, err)
	}
	// An injected clock: events 23 hours old are fresh, 25 hours old are stale.
	w2 := newWorld(t)
	w2.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w2.c.SetClock(func() time.Time { return time.Now().Add(23 * time.Hour) })
	w2.run()
	w2.wantRows("alice:assigned")
}

func TestConsume_PoisonEventIsSkipped(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	// A missing issue, a payload that is not an object, an unreadable id.
	for _, ev := range []struct {
		typ, entity, payload string
	}{
		{events.IssueUpdated, uuid.NewString(), `{"changes":["state"]}`},
		{events.IssueCreated, uuid.NewString(), `[1,2]`},
		{events.CommentCreated, uuid.NewString(), `{"issueId":"` + uuid.NewString() + `"}`},
		{events.IssueUpdated, uuid.NewString(), `{"teamId":"not-a-uuid"}`},
	} {
		if _, err := events.Emit(ctx, w.q, w.org.ID, events.Event{Type: ev.typ, EntityType: "x", EntityID: uuid.MustParse(ev.entity), Payload: json.RawMessage(ev.payload)},
			events.ScopeTeams(uuid.New())); err != nil {
			t.Fatal(err)
		}
	}
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	res := w.run()
	w.wantRows("alice:assigned")
	if res.Poison != 4 {
		t.Errorf("poison = %d, want 4", res.Poison)
	}
	var last, max int64
	if err := w.pool.QueryRow(ctx, `SELECT (SELECT last_seq FROM notification_cursors), (SELECT max(seq) FROM events)`).Scan(&last, &max); err != nil || last != max {
		t.Errorf("cursor = %d, max seq = %d (%v)", last, max, err)
	}
	if !strings.Contains(w.logs.String(), "skipped an event") {
		t.Errorf("no WARN for the poison events:\n%s", w.logs)
	}
}

// A database error rolls the batch back and keeps the cursor, so the next
// tick retries it.
func TestConsume_DatabaseErrorRetriesOnTheNextRun(t *testing.T) {
	w := newWorld(t)
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	// A trigger that fails every insert while fail_flag has a row.
	w.exec(`CREATE TABLE fail_flag (x int)`)
	w.exec(`INSERT INTO fail_flag VALUES (1)`)
	w.exec(`CREATE FUNCTION fail_while_flagged() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF EXISTS (SELECT 1 FROM fail_flag) THEN RAISE EXCEPTION 'injected'; END IF;
			RETURN NEW;
		END $$`)
	w.exec(`CREATE TRIGGER fail_while_flagged BEFORE INSERT ON notifications FOR EACH ROW EXECUTE FUNCTION fail_while_flagged()`)
	if _, err := w.c.RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce: want the injected error")
	}
	var last int64
	if err := w.pool.QueryRow(context.Background(), `SELECT last_seq FROM notification_cursors`).Scan(&last); err != nil || last != 0 {
		t.Errorf("cursor after the failed run = %d (%v), want 0", last, err)
	}
	w.wantRows()
	w.exec(`DELETE FROM fail_flag`)
	w.run()
	w.wantRows("alice:assigned")
}

// --- the write path, the wake-up, the org wall, the logs -----------------

// Writes work with the consumer stopped, and make no notification row.
func TestWrites_SucceedWithTheConsumerStopped(t *testing.T) {
	w := newWorld(t)
	is := w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice", Description: "@bob"})
	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{State: str("In Progress")})
	w.comment("chen", is.Identifier, "@carol")
	w.wantRows()
	var n int
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM notification_emails`).Scan(&n); err != nil || n != 0 {
		t.Errorf("outbox rows = %d (%v), want 0 before the consumer runs", n, err)
	}
}

// After COMMIT the consumer wakes the broker's channel with the org id.
func TestConsume_NotifiesAfterCommit(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	conn, err := w.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+events.Channel); err != nil {
		t.Fatal(err)
	}
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	// Drain the wake-ups of the write itself.
	drain := func() {
		for {
			c, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
			_, err := conn.Conn().WaitForNotification(c)
			cancel()
			if err != nil {
				return
			}
		}
	}
	drain()
	w.run()
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	n, err := conn.Conn().WaitForNotification(c)
	if err != nil {
		t.Fatalf("no wake-up after the consumer committed: %v", err)
	}
	if n.Payload != w.org.ID.String() {
		t.Errorf("payload = %q, want the org id", n.Payload)
	}
	// A run that makes no row sends none.
	drain()
	w.run()
	c2, cancel2 := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel2()
	if _, err := conn.Conn().WaitForNotification(c2); err == nil {
		t.Error("a run with nothing to do sent a wake-up")
	}
}

// Two orgs, one user in both: a mention in one org makes a row in that org
// only, and a name that is a member of the other org only is plain text.
func TestConsume_CrossOrg(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	org2, err := w.q.InsertOrg(ctx, dbq.InsertOrgParams{Slug: "other", Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tracker.CreateTeamTx(ctx, w.q, org2.ID, "OTH", "Other", ""); err != nil {
		t.Fatal(err)
	}
	// alice is in both orgs; zed only in org 2; bob only in org 1.
	w.addMember(org2.ID, "alice")
	zed := w.addMember(org2.ID, "zed")
	w.addMember(org2.ID, "chen")
	w2 := *w
	w2.org = &auth.Org{ID: org2.ID, Slug: "other", Name: "Other", Role: "admin", Access: auth.TeamAccess{All: true}}
	is2, err := w.svc.CreateIssue(ctx, w2.org, w.user["chen"], tracker.CreateIssueInput{Team: "OTH", Title: "t", Description: "@alice @bob @zed"})
	if err != nil {
		t.Fatal(err)
	}
	is1 := w.createIssue("chen", tracker.CreateIssueInput{Description: "@alice @bob @zed"})
	_ = is1
	w.run()
	got := func(org uuid.UUID) []string {
		var out []string
		for _, r := range w.rows(org) {
			out = append(out, r.To+":"+r.Kind)
		}
		return out
	}
	if g := got(org2.ID); !slices.Equal(g, []string{"alice:mention", "zed:mention"}) {
		t.Errorf("org 2 rows = %v, want alice and zed", g)
	}
	if g := got(w.org.ID); !slices.Equal(g, []string{"alice:mention", "bob:mention"}) {
		t.Errorf("org 1 rows = %v, want alice and bob", g)
	}
	// Every row points at an issue of its own org.
	var bad int
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM notifications n JOIN issues i ON i.id = n.issue_id WHERE i.org_id <> n.org_id`).Scan(&bad); err != nil || bad != 0 {
		t.Errorf("rows pointing at another org's issue = %d (%v)", bad, err)
	}
	_ = is2
	_ = zed
	// Each org has its own cursor.
	var cursors int
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM notification_cursors`).Scan(&cursors); err != nil || cursors != 2 {
		t.Errorf("cursors = %d (%v), want 2", cursors, err)
	}
}

// No body text, address, token or secret in the logs, the events, the
// notification data, or the idempotency rows.
func TestConsume_NoSecretsOrBodiesInLogsEventsOrRows(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	const secret = "TOPSECRETBODY-7731"
	is := w.createIssue("chen", tracker.CreateIssueInput{Title: "t " + secret, Assignee: "alice", Description: secret + " @bob"})
	w.comment("chen", is.Identifier, secret+" @carol")
	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{State: str("In Progress")})
	// A poison event logs a line too.
	if _, err := events.Emit(ctx, w.q, w.org.ID, events.Event{Type: events.IssueUpdated, EntityType: "issue", EntityID: uuid.New(), Payload: map[string]any{"x": secret}},
		events.ScopeTeams(uuid.New())); err != nil {
		t.Fatal(err)
	}
	w.run()
	needles := []string{secret, "@example.test", "alice@", "bob@", "carol@"}
	for _, n := range needles {
		if strings.Contains(w.logs.String(), n) {
			t.Errorf("the log holds %q:\n%s", n, w.logs)
		}
	}
	for _, q := range []string{
		`SELECT coalesce(string_agg(data::text, ' '), '') FROM notifications`,
		`SELECT coalesce(string_agg(payload::text, ' '), '') FROM events`,
		`SELECT coalesce(string_agg(last_error, ' '), '') FROM notification_emails`,
		`SELECT coalesce(string_agg(key || path || encode(coalesce(response_body, ''::bytea), 'escape'), ' '), '') FROM idempotency_keys`,
	} {
		var s string
		if err := w.pool.QueryRow(ctx, q).Scan(&s); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		// The test's own poison event carries the secret in its payload by design.
		if strings.Contains(q, "FROM events") {
			s = strings.ReplaceAll(s, `{"x": "`+secret+`"}`, "")
		}
		for _, n := range needles {
			if strings.Contains(s, n) {
				t.Errorf("%s holds %q", q, n)
			}
		}
	}
}

// The event payloads carry what the consumer needs (decisions-T13 #14).
func TestEventPayloads_TeamAndAssignee(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	is := w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{Assignee: str("bob")})
	w.updateIssue("chen", is.Identifier, tracker.UpdateIssueInput{State: str("In Progress")})
	evs, err := w.q.ListEventsAfterSystem(ctx, dbq.ListEventsAfterSystemParams{OrgID: w.org.ID, Seq: 0, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	team := w.teamID().String()
	var created, assign, state map[string]any
	for _, e := range evs {
		var m map[string]any
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			t.Fatal(err)
		}
		switch {
		case e.Type == events.IssueCreated:
			created = m
		case e.Type == events.IssueUpdated && fmt.Sprint(m["changes"]) == "[assignee]":
			assign = m
		case e.Type == events.IssueUpdated && fmt.Sprint(m["changes"]) == "[state sortOrder]", e.Type == events.IssueUpdated && fmt.Sprint(m["changes"]) == "[state]":
			state = m
		}
	}
	alice, bob := w.user["alice"].UserID.String(), w.user["bob"].UserID.String()
	if created["teamId"] != team || created["assigneeId"] != alice {
		t.Errorf("issue.created payload = %v, want teamId %s and assigneeId %s", created, team, alice)
	}
	if assign["teamId"] != team || assign["assigneeId"] != bob || assign["fromAssigneeId"] != alice {
		t.Errorf("assignee change payload = %v, want assigneeId %s fromAssigneeId %s", assign, bob, alice)
	}
	if state["teamId"] != team || state["assigneeId"] != bob {
		t.Errorf("state change payload = %v", state)
	}
	if _, ok := state["fromAssigneeId"]; ok {
		t.Errorf("state change payload has fromAssigneeId: %v", state)
	}
}

func (w *world) teamID() uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.pool.QueryRow(context.Background(), `SELECT id FROM teams WHERE org_id = $1 AND key = $2`, w.org.ID, testdb.TeamKey).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}
