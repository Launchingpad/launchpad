package notify_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"devops/internal/auth"
	"devops/internal/dbq"
	"devops/internal/notify"
	"devops/internal/tracker"
)

// LAUN-231 (decisions-T20, decisions-T38 superseded line): a post queued for
// one team never reaches the channel of another. A service that moves to a
// new team or gets a new webhook URL drops its waiting posts in the same
// transaction, and the sender skips ("not_selected", no retry) a row whose
// issue or project is not of the service's current team.

const secondToken = "SECONDTEAMTOKEN"

// waiting counts the service's pending or sending rows.
func (w *swo) waiting(id uuid.UUID) int {
	w.t.Helper()
	var n int
	if err := w.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM chat_outbox WHERE service_id = $1 AND state IN ('pending', 'sending')`, id).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// A move to another team (with or without a new URL) drops the waiting post:
// the new team's channel never gets the old team's post. A post of the new
// team still goes.
func TestChat_MoveDropsWaitingPosts(t *testing.T) {
	for _, withURL := range []bool{false, true} {
		t.Run(map[bool]string{false: "team only", true: "team and url"}[withURL], func(t *testing.T) {
			w := newChatWorld(t)
			ctx := context.Background()
			sec := w.secondTeam()
			v := w.serviceInTeam("ORB", "Mover", hookURL, "new_issue")
			w.createIssue("chen", tracker.CreateIssueInput{Team: "ORB"})
			w.consume()
			if n := w.waiting(v.ID); n != 1 {
				t.Fatalf("waiting = %d, want 1", n)
			}
			st := w.teamIDByKey(sec)
			in := notify.ChatPatchInput{TeamID: &st}
			if withURL {
				in.WebhookURL = strp(hookURL2)
			}
			if _, err := w.svc2.Patch(ctx, w.org.ID, v.ID, w.user["chen"], in); err != nil {
				t.Fatal(err)
			}
			if n := w.rowsFor("Mover"); n != 0 {
				t.Fatalf("rows after the move = %d, want 0", n)
			}
			if res := w.pass(); res.Sent != 0 {
				t.Fatalf("pass after the move %+v", res)
			}
			if hits := w.slack.got(); len(hits) != 0 {
				t.Fatalf("posted after the move: %+v", hits)
			}
			// A post of the new team goes, to the current URL.
			w.createIssue("chen", tracker.CreateIssueInput{Team: sec})
			w.consume()
			if res := w.pass(); res.Sent != 1 {
				t.Fatalf("new team's post %+v", res)
			}
			hits := w.slack.got()
			if len(hits) != 1 || !strings.Contains(hits[0].Body, sec+"-1") || strings.Contains(hits[0].Body, "ORB-") {
				t.Fatalf("hits %+v", hits)
			}
			if withURL && !strings.HasSuffix(hits[0].Path, secondToken) {
				t.Fatalf("post went to %s, want the new URL", hits[0].Path)
			}
			w.noSecretAnywhere(secretToken, secondToken)
		})
	}
}

// A new URL alone drops the waiting posts too; nothing reaches the new URL.
// A change of the name or the events alone keeps them.
func TestChat_NewURLDropsWaitingPosts(t *testing.T) {
	w := newChatWorld(t)
	ctx := context.Background()
	v := w.serviceInTeam("ORB", "Chan", hookURL, "new_issue", "assigned")
	w.createIssue("chen", tracker.CreateIssueInput{Team: "ORB"})
	w.consume()
	if n := w.waiting(v.ID); n != 1 {
		t.Fatalf("waiting = %d, want 1", n)
	}
	if _, err := w.svc2.Patch(ctx, w.org.ID, v.ID, w.user["chen"], notify.ChatPatchInput{
		Name: strp("Renamed"), Events: []string{"new_issue"}}); err != nil {
		t.Fatal(err)
	}
	if n := w.waiting(v.ID); n != 1 {
		t.Fatalf("a rename and an events change dropped the post: waiting = %d", n)
	}
	// Same team, given again: not a move.
	own := w.teamIDByKey("ORB")
	if _, err := w.svc2.Patch(ctx, w.org.ID, v.ID, w.user["chen"], notify.ChatPatchInput{TeamID: &own}); err != nil {
		t.Fatal(err)
	}
	if n := w.waiting(v.ID); n != 1 {
		t.Fatalf("the same team dropped the post: waiting = %d", n)
	}
	if _, err := w.svc2.Patch(ctx, w.org.ID, v.ID, w.user["chen"], notify.ChatPatchInput{WebhookURL: strp(hookURL2)}); err != nil {
		t.Fatal(err)
	}
	if n := w.waiting(v.ID); n != 0 {
		t.Fatalf("waiting after a new URL = %d, want 0", n)
	}
	w.pass()
	if hits := w.slack.got(); len(hits) != 0 {
		t.Fatalf("the new URL got the old post: %+v", hits)
	}
	w.noSecretAnywhere(secretToken, secondToken)
}

// The send-time check: a waiting row whose issue or project is not of the
// service's current team is skipped with "not_selected", no attempt, no post.
// (The service's team is changed in SQL here: the Patch path drops the rows
// first, so this is the backstop for any other way the two can differ.)
func TestChat_SendTimeTeamMismatchSkips(t *testing.T) {
	w := newChatWorld(t)
	sec := w.secondTeam()
	v := w.serviceInTeam("ORB", "Chan", hookURL, "new_issue", "project_update_due")
	w.createIssue("chen", tracker.CreateIssueInput{Team: "ORB"})
	p := w.project("Linked", "alice", "weekly", utc(2026, 10, 28, 12, 0))
	w.exec(`INSERT INTO project_teams (org_id, project_id, team_id) VALUES ($1, $2, $3)`, w.org.ID, p.ID, w.teamIDByKey("ORB"))
	w.job(utc(2026, 10, 30, 9, 0))
	w.consume()
	if n := w.waiting(v.ID); n != 2 {
		t.Fatalf("waiting = %d, want 2 (the issue and the reminder)", n)
	}
	w.exec(`UPDATE chat_services SET team_id = $1 WHERE id = $2`, w.teamIDByKey(sec), v.ID)
	res := w.pass()
	if res.Sent != 0 || res.Skipped != 2 {
		t.Fatalf("pass %+v, want 0 sent and 2 skipped", res)
	}
	for _, r := range w.outRows() {
		if r.State != "skipped" || r.Skip != "not_selected" || r.Attempts != 0 {
			t.Errorf("row %+v, want skipped/not_selected with no attempt", r)
		}
	}
	if hits := w.slack.got(); len(hits) != 0 {
		t.Fatalf("posted across teams: %+v", hits)
	}
	// Back in its team, a new post goes as before.
	w.exec(`UPDATE chat_services SET team_id = $1 WHERE id = $2`, w.teamIDByKey("ORB"), v.ID)
	w.createIssue("chen", tracker.CreateIssueInput{Team: "ORB"})
	w.consume()
	if res := w.pass(); res.Sent != 1 {
		t.Fatalf("a normal post %+v", res)
	}
}

// A move in one org leaves the waiting posts of another org's service alone.
func TestChat_MoveDropIsPerOrg(t *testing.T) {
	w := newChatWorld(t)
	ctx := context.Background()
	org2, err := w.q.InsertOrg(ctx, dbq.InsertOrgParams{Slug: "other", Name: "Other"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tracker.CreateTeamTx(ctx, w.q, org2.ID, "OTH", "Other", ""); err != nil {
		t.Fatal(err)
	}
	w.addMember(org2.ID, "alice")
	w.addMember(org2.ID, "chen")
	mine := w.putService(w.org.ID, notify.ChatSlack, "Slack", hookURL, "assigned")
	theirs := w.putService(org2.ID, notify.ChatSlack, "Slack", "https://hooks.slack.com/services/T0003/B0003/OTHERORGTOKEN", "assigned")
	w.createIssue("chen", tracker.CreateIssueInput{Assignee: "alice"})
	o2 := &auth.Org{ID: org2.ID, Slug: "other", Name: "Other", Role: "admin", Access: auth.TeamAccess{All: true}}
	if _, err := w.svc.CreateIssue(ctx, o2, w.user["chen"], tracker.CreateIssueInput{Team: "OTH", Title: "t", Assignee: "alice"}); err != nil {
		t.Fatal(err)
	}
	w.consume()
	if a, b := w.waiting(mine.ID), w.waiting(theirs.ID); a != 1 || b != 1 {
		t.Fatalf("waiting %d and %d, want 1 and 1", a, b)
	}
	if _, err := w.svc2.Patch(ctx, w.org.ID, mine.ID, w.user["chen"], notify.ChatPatchInput{WebhookURL: strp(hookURL2)}); err != nil {
		t.Fatal(err)
	}
	if a, b := w.waiting(mine.ID), w.waiting(theirs.ID); a != 0 || b != 1 {
		t.Fatalf("after org 1's new URL: waiting %d and %d, want 0 and 1", a, b)
	}
	// Org 1 cannot patch org 2's service.
	if _, err := w.svc2.Patch(ctx, w.org.ID, theirs.ID, w.user["chen"], notify.ChatPatchInput{WebhookURL: strp(hookURL2)}); err == nil {
		t.Fatal("org 1 patched org 2's service")
	}
	if n := w.waiting(theirs.ID); n != 1 {
		t.Fatalf("org 2's row went: waiting %d", n)
	}
}

// noSecretAnywhere: no webhook token in a log, an event payload or an outbox row.
func (w *swo) noSecretAnywhere(tokens ...string) {
	w.t.Helper()
	for name, text := range map[string]string{"sender log": w.slogs.String(), "consumer log": w.logs.String()} {
		for _, tok := range tokens {
			if strings.Contains(text, tok) || strings.Contains(text, "hooks.slack.com/services") {
				w.t.Errorf("%s holds a webhook", name)
			}
		}
	}
	for _, q := range []string{
		`SELECT coalesce(string_agg(payload::text, ' '), '') FROM events`,
		`SELECT coalesce(string_agg(chat_outbox::text, ' '), '') FROM chat_outbox`,
	} {
		var text string
		if err := w.pool.QueryRow(context.Background(), q).Scan(&text); err != nil {
			w.t.Fatalf("%s: %v", q, err)
		}
		for _, tok := range tokens {
			if strings.Contains(text, tok) || strings.Contains(text, "hooks.slack.com") {
				w.t.Errorf("stored in clear (%s)", q)
			}
		}
	}
}
