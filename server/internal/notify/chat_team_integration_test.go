package notify_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"devops/internal/dbq"
	"devops/internal/httpx"
	"devops/internal/notify"
	"devops/internal/tracker"
)

// LAUN-209, decisions-T38: a chat service belongs to one team and posts only
// the events of that team's issues.

// secondTeam makes a second team (key SEC) in the world's org.
func (w *swo) secondTeam() string {
	w.t.Helper()
	if _, err := tracker.CreateTeamTx(context.Background(), w.q, w.org.ID, "SEC", "Second", ""); err != nil {
		w.t.Fatal(err)
	}
	return "SEC"
}

func (w *swo) teamIDByKey(key string) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.pool.QueryRow(context.Background(), `SELECT id FROM teams WHERE org_id = $1 AND key = $2`, w.org.ID, key).Scan(&id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

func (w *swo) serviceInTeam(team, name, hook string, events ...string) notify.ChatView {
	w.t.Helper()
	v, err := w.svc2.Create(context.Background(), w.org.ID, w.user["chen"], notify.ChatCreateInput{
		Name: name, TeamID: w.teamIDByKey(team), Kind: notify.ChatSlack, WebhookURL: hook, Events: events})
	if err != nil {
		w.t.Fatalf("Create: %v", err)
	}
	return v
}

func (w *swo) rowsFor(svcName string) int {
	w.t.Helper()
	var n int
	if err := w.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM chat_outbox o JOIN chat_services s ON s.id = o.service_id WHERE s.name = $1`, svcName).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

const hookURL2 = "https://hooks.slack.com/services/T0002/B0002/SECONDTEAMTOKEN"

// A team service gets only its team's events; another team's events post
// nothing to it; a team with no service gets no post.
func TestChat_TeamRoutesEvents(t *testing.T) {
	w := newChatWorld(t)
	sec := w.secondTeam()
	third := "THR"
	if _, err := tracker.CreateTeamTx(context.Background(), w.q, w.org.ID, third, "Third", ""); err != nil {
		t.Fatal(err)
	}
	w.serviceInTeam("ORB", "Orb", hookURL, "new_issue")
	w.serviceInTeam(sec, "Sec", hookURL2, "new_issue")
	w.createIssue("chen", tracker.CreateIssueInput{Team: "ORB"})
	w.consume()
	if a, b := w.rowsFor("Orb"), w.rowsFor("Sec"); a != 1 || b != 0 {
		t.Fatalf("ORB issue: Orb %d, Sec %d rows; want 1 and 0", a, b)
	}
	w.createIssue("chen", tracker.CreateIssueInput{Team: sec})
	w.consume()
	if a, b := w.rowsFor("Orb"), w.rowsFor("Sec"); a != 1 || b != 1 {
		t.Fatalf("SEC issue: Orb %d, Sec %d rows; want 1 and 1", a, b)
	}
	// A team with no service posts nowhere.
	w.createIssue("chen", tracker.CreateIssueInput{Team: third})
	w.consume()
	if n := len(w.outRows()); n != 2 {
		t.Fatalf("rows = %d after an issue in a team with no service, want 2", n)
	}
}

// Changing the team of a service moves the future posts only; the waiting
// posts of the old team are dropped (LAUN-231).
func TestChat_TeamChangeMovesFuturePosts(t *testing.T) {
	w := newChatWorld(t)
	sec := w.secondTeam()
	v := w.serviceInTeam("ORB", "Mover", hookURL, "new_issue")
	w.createIssue("chen", tracker.CreateIssueInput{Team: "ORB"})
	w.consume()
	if n := w.rowsFor("Mover"); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}
	st := w.teamIDByKey(sec)
	if _, err := w.svc2.Patch(context.Background(), w.org.ID, v.ID, w.user["chen"], notify.ChatPatchInput{TeamID: &st}); err != nil {
		t.Fatal(err)
	}
	if n := w.rowsFor("Mover"); n != 0 {
		t.Fatalf("the waiting ORB post survived the move: rows = %d, want 0", n)
	}
	w.createIssue("chen", tracker.CreateIssueInput{Team: "ORB"})
	w.consume()
	if n := w.rowsFor("Mover"); n != 0 {
		t.Fatalf("an ORB issue after the move posted: rows = %d, want 0", n)
	}
	w.createIssue("chen", tracker.CreateIssueInput{Team: sec})
	w.consume()
	if n := w.rowsFor("Mover"); n != 1 {
		t.Fatalf("a SEC issue after the move: rows = %d, want 1", n)
	}
}

// A project reminder posts to the services of the project's teams; a project
// with no team posts nowhere.
func TestChat_ProjectDueUsesProjectTeams(t *testing.T) {
	w := newChatWorld(t)
	sec := w.secondTeam()
	w.serviceInTeam("ORB", "Orb", hookURL, "project_update_due")
	w.serviceInTeam(sec, "Sec", hookURL2, "project_update_due")
	linked := w.project("Linked", "alice", "weekly", utc(2026, 10, 28, 12, 0))
	w.exec(`INSERT INTO project_teams (org_id, project_id, team_id) VALUES ($1, $2, $3)`, w.org.ID, linked.ID, w.teamIDByKey(sec))
	w.project("Orphan", "alice", "weekly", utc(2026, 10, 28, 12, 0)) // no team
	w.job(utc(2026, 10, 30, 9, 0))
	w.consume()
	if a, b := w.rowsFor("Orb"), w.rowsFor("Sec"); a != 0 || b != 1 {
		t.Fatalf("project reminders: Orb %d, Sec %d; want 0 and 1 (the orphan posts nowhere)", a, b)
	}
}

// A team cannot be given a service across orgs, and a missing team is refused.
func TestChat_TeamMustBeOfTheOrg(t *testing.T) {
	w := newChatWorld(t)
	ctx := context.Background()
	other := w.otherOrgTeam()
	chen := w.user["chen"]
	var he *httpx.Error
	_, err := w.svc2.Create(ctx, w.org.ID, chen, notify.ChatCreateInput{Name: "x", TeamID: other, Kind: notify.ChatSlack, WebhookURL: hookURL})
	if !errors.As(err, &he) || he.Status != 400 {
		t.Fatalf("another org's team on create: %v", err)
	}
	_, err = w.svc2.Create(ctx, w.org.ID, chen, notify.ChatCreateInput{Name: "x", Kind: notify.ChatSlack, WebhookURL: hookURL})
	if !errors.As(err, &he) || he.Status != 400 {
		t.Fatalf("no team on create: %v", err)
	}
	v := w.serviceInTeam("ORB", "Mine", hookURL)
	_, err = w.svc2.Patch(ctx, w.org.ID, v.ID, chen, notify.ChatPatchInput{TeamID: &other})
	if !errors.As(err, &he) || he.Status != 400 {
		t.Fatalf("another org's team on patch: %v", err)
	}
	got, err := w.svc2.Get(ctx, w.org.ID, v.ID)
	if err != nil || got.TeamID != w.teamIDByKey("ORB") {
		t.Fatalf("team after refused patch: %v %v", got.TeamID, err)
	}
}

func (w *swo) otherOrgTeam() uuid.UUID {
	w.t.Helper()
	ctx := context.Background()
	org2, err := w.q.InsertOrg(ctx, dbq.InsertOrgParams{Slug: "elsewhere", Name: "Elsewhere"})
	if err != nil {
		w.t.Fatal(err)
	}
	tm, err := tracker.CreateTeamTx(ctx, w.q, org2.ID, "ELS", "Elsewhere", "")
	if err != nil {
		w.t.Fatal(err)
	}
	return tm.ID
}

// An archived team is refused as a service's team on create and on a change;
// a service that already sits in a team keeps it when the team is archived.
func TestChat_ArchivedTeamRefused(t *testing.T) {
	w := newChatWorld(t)
	ctx := context.Background()
	chen := w.user["chen"]
	sec := w.secondTeam()
	w.exec(`UPDATE teams SET archived_at = now() WHERE org_id = $1 AND key = $2`, w.org.ID, sec)
	st := w.teamIDByKey(sec)
	var he *httpx.Error
	_, err := w.svc2.Create(ctx, w.org.ID, chen, notify.ChatCreateInput{Name: "x", TeamID: st, Kind: notify.ChatSlack, WebhookURL: hookURL})
	if !errors.As(err, &he) || he.Status != 400 {
		t.Fatalf("archived team on create: %v", err)
	}
	v := w.serviceInTeam("ORB", "Mine", hookURL)
	_, err = w.svc2.Patch(ctx, w.org.ID, v.ID, chen, notify.ChatPatchInput{TeamID: &st})
	if !errors.As(err, &he) || he.Status != 400 {
		t.Fatalf("archived team on patch: %v", err)
	}
	// Archive the service's own team: a patch that keeps it still works.
	own := w.teamIDByKey("ORB")
	w.exec(`UPDATE teams SET archived_at = now() WHERE id = $1`, own)
	if _, err := w.svc2.Patch(ctx, w.org.ID, v.ID, chen, notify.ChatPatchInput{TeamID: &own}); err != nil {
		t.Fatalf("keeping the team: %v", err)
	}
}
