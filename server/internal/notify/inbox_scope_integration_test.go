package notify_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/auth"
	"devops/internal/dbq"
	"devops/internal/notify"
	"devops/internal/tracker"
)

// LAUN-216 (T20 #62): the inbox reads take the org and its access. A caller
// with access to team ORB only sees the rows of ORB issues, and the list, the
// unread count, the stream, one row and read-all agree. A project row has no
// team and stays. All teams: no change.
func TestInbox_Scope(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tb, err := tracker.CreateTeamTx(ctx, w.q, w.org.ID, "BBB", "Team B", "")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(team string) tracker.Issue {
		is, err := w.svc.CreateIssue(ctx, w.org, w.user["chen"], tracker.CreateIssueInput{Team: team, Title: "in " + team})
		if err != nil {
			t.Fatal(err)
		}
		return is
	}
	ia, ib := mk("ORB"), mk("BBB")
	alice := w.user["alice"].UserID
	ins := func(key string, issue uuid.UUID) uuid.UUID {
		n, err := w.q.InsertNotification(ctx, dbq.InsertNotificationParams{OrgID: w.org.ID, RecipientID: alice, Kind: "assigned",
			IssueID: &issue, Data: []byte(`{}`), DedupeKey: key, Inbox: true})
		if err != nil {
			t.Fatal(err)
		}
		return n.ID
	}
	na, nb := ins("a", ia.ID), ins("b", ib.ID)
	// Project-only rows: one of a project of ORB (visible), one of a project of BBB (hidden), one of no team.
	mkProject := func(name string, teams ...string) uuid.UUID {
		p, err := w.svc.CreateProject(ctx, w.org, w.user["chen"], tracker.CreateProjectInput{Name: name, Teams: teams})
		if err != nil {
			t.Fatal(err)
		}
		return p.ID
	}
	insProject := func(key string, project uuid.UUID) uuid.UUID {
		n, err := w.q.InsertNotification(ctx, dbq.InsertNotificationParams{OrgID: w.org.ID, RecipientID: alice, Kind: "project_update_due",
			ProjectID: &project, Data: []byte(`{}`), DedupeKey: key, Inbox: true})
		if err != nil {
			t.Fatal(err)
		}
		return n.ID
	}
	npa, npb, npo := insProject("pa", mkProject("pa", "ORB")), insProject("pb", mkProject("pb", "BBB")), insProject("po", mkProject("po"))
	var orbTeam uuid.UUID
	if err := w.pool.QueryRow(ctx, `SELECT id FROM teams WHERE key = 'ORB'`).Scan(&orbTeam); err != nil {
		t.Fatal(err)
	}
	_ = tb

	restricted := *w.org
	restricted.Role, restricted.Access = "member", auth.NewTeamAccess("member", "selected", []uuid.UUID{orbTeam})
	svc := notify.New(w.pool)

	ids := func(o *auth.Org) []uuid.UUID {
		page, err := svc.ListInbox(ctx, o, alice, notify.InboxQuery{})
		if err != nil {
			t.Fatal(err)
		}
		var out []uuid.UUID
		for _, it := range page.Items {
			out = append(out, it.ID)
		}
		return out
	}
	if got := ids(w.org); len(got) != 5 {
		t.Fatalf("all teams: %v, want both rows", got)
	}
	got := ids(&restricted)
	if len(got) != 3 || !slices.Contains(got, na) || !slices.Contains(got, npa) || !slices.Contains(got, npo) || slices.Contains(got, nb) || slices.Contains(got, npb) {
		t.Fatalf("restricted list: %v, want the ORB issue row, the ORB project row and the org-level project row", got)
	}
	for name, o := range map[string]*auth.Org{"all": w.org, "restricted": &restricted} {
		want := int64(5)
		if o == &restricted {
			want = 3
		}
		if n, err := svc.UnreadCount(ctx, o, alice); err != nil || n != want {
			t.Fatalf("%s unread count: %d %v, want %d", name, n, err, want)
		}
		rows, err := svc.StreamRows(ctx, o, alice, 0, 50)
		if err != nil || int64(len(rows)) != want {
			t.Fatalf("%s stream: %d rows %v, want %d", name, len(rows), err, want)
		}
	}
	// One row of a hidden team or a hidden project is not found.
	read := true
	if _, err := svc.PatchInbox(ctx, &restricted, alice, npb, notify.InboxPatch{Read: &read}); err == nil {
		t.Fatal("patched a row of a hidden project")
	}
	if _, err := svc.PatchInbox(ctx, &restricted, alice, npa, notify.InboxPatch{Read: &read}); err != nil {
		t.Fatalf("patch a visible project row: %v", err)
	}
	if _, err := svc.PatchInbox(ctx, &restricted, alice, npo, notify.InboxPatch{Read: &read}); err != nil {
		t.Fatalf("patch an org-level project row: %v", err)
	}
	if _, err := svc.PatchInbox(ctx, &restricted, alice, nb, notify.InboxPatch{Read: &read}); err == nil {
		t.Fatal("patched a row of a hidden team")
	}
	if _, err := svc.PatchInbox(ctx, &restricted, alice, na, notify.InboxPatch{Read: &read}); err != nil {
		t.Fatalf("patch a visible row: %v", err)
	}
	// Read-all marks the visible rows only.
	before := time.Now().Add(time.Minute)
	if res, err := svc.ReadAll(ctx, &restricted, alice, notify.ReadAllInput{Before: &before}); err != nil || res.Marked != 0 {
		t.Fatalf("restricted read-all after the visible row was read: %+v %v", res, err)
	}
	for _, hidden := range []uuid.UUID{nb, npb} {
		var readAt *time.Time
		if err := w.pool.QueryRow(ctx, `SELECT read_at FROM notifications WHERE id = $1`, hidden).Scan(&readAt); err != nil || readAt != nil {
			t.Fatalf("a hidden row was marked: %v %v", readAt, err)
		}
	}
	if res, err := svc.ReadAll(ctx, w.org, alice, notify.ReadAllInput{Before: &before}); err != nil || res.Marked != 2 {
		t.Fatalf("all-teams read-all: %+v %v, want 2", res, err)
	}
}
