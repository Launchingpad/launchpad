package tracker_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/database"
	"devops/internal/testdb"
	"devops/internal/tracker"
)

// TestRemoveLostTeamNotifyRows (T20 #50, LAUN-223): for the member and the
// lost team only, the subscriptions on that team's issues, the follow and the
// team settings go; the rows of the kept team, the global settings, the inbox
// rows and another member's rows stay. A second call deletes nothing.
func TestRemoveLostTeamNotifyRows(t *testing.T) {
	e := newCycEnv(t)
	ctx := context.Background()
	tb, err := tracker.CreateTeamTx(ctx, e.q, e.org.ID, "BBB", "Team B", "")
	if err != nil {
		t.Fatal(err)
	}
	ia, err := e.svc.CreateIssue(ctx, e.org, e.chen, tracker.CreateIssueInput{Team: testdb.TeamKey, Title: "a"})
	if err != nil {
		t.Fatal(err)
	}
	ib, err := e.svc.CreateIssue(ctx, e.org, e.chen, tracker.CreateIssueInput{Team: "BBB", Title: "b"})
	if err != nil {
		t.Fatal(err)
	}
	rita := testdb.AddMember(t, e.pool, e.org.ID, "rita", "member")
	mona := testdb.AddMember(t, e.pool, e.org.ID, "mona", "member")
	for _, u := range []uuid.UUID{rita, mona} {
		for _, is := range []uuid.UUID{ia.ID, ib.ID} {
			e.exec(`INSERT INTO issue_subscriptions (org_id, issue_id, user_id, state, reason) VALUES ($1, $2, $3, 'subscribed', 'manual')`, e.org.ID, is, u)
			e.exec(`INSERT INTO notifications (org_id, recipient_id, kind, issue_id, dedupe_key, inbox) VALUES ($1, $2, 'comment', $3, $4, true)`,
				e.org.ID, u, is, "k:"+is.String())
		}
		for _, team := range []uuid.UUID{e.team.ID, tb.ID} {
			e.exec(`INSERT INTO team_follows (org_id, user_id, team_id) VALUES ($1, $2, $3)`, e.org.ID, u, team)
			e.exec(`INSERT INTO notification_settings (org_id, user_id, team_id, event, email) VALUES ($1, $2, $3, 'comment', 'off')`, e.org.ID, u, team)
		}
		e.exec(`INSERT INTO notification_settings (org_id, user_id, event, email) VALUES ($1, $2, 'comment', 'on')`, e.org.ID, u)
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := e.pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	seqBefore := count(`SELECT coalesce(max(seq), 0) FROM events WHERE org_id = $1`, e.org.ID)

	var got tracker.LostTeamNotifyRows
	if err := database.InTx(ctx, e.pool, func(tx pgx.Tx) error {
		var err error
		got, err = tracker.RemoveLostTeamNotifyRows(ctx, e.q.WithTx(tx), e.org.ID, rita, []uuid.UUID{tb.ID})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got != (tracker.LostTeamNotifyRows{Subscriptions: 1, Follows: 1, Settings: 1}) {
		t.Fatalf("deleted %+v, want one of each", got)
	}
	for _, c := range []struct {
		name string
		sql  string
		args []any
		want int
	}{
		{"rita's subscriptions", `SELECT count(*) FROM issue_subscriptions WHERE user_id = $1`, []any{rita}, 1},
		{"rita's subscription of team B", `SELECT count(*) FROM issue_subscriptions WHERE user_id = $1 AND issue_id = $2`, []any{rita, ib.ID}, 0},
		{"rita's follows", `SELECT count(*) FROM team_follows WHERE user_id = $1 AND team_id = $2`, []any{rita, e.team.ID}, 1},
		{"rita's follow of team B", `SELECT count(*) FROM team_follows WHERE user_id = $1 AND team_id = $2`, []any{rita, tb.ID}, 0},
		{"rita's settings (global and ORB)", `SELECT count(*) FROM notification_settings WHERE user_id = $1`, []any{rita}, 2},
		{"rita's global setting", `SELECT count(*) FROM notification_settings WHERE user_id = $1 AND team_id IS NULL`, []any{rita}, 1},
		{"rita's inbox rows (kept, #51)", `SELECT count(*) FROM notifications WHERE recipient_id = $1`, []any{rita}, 2},
		{"mona's subscriptions", `SELECT count(*) FROM issue_subscriptions WHERE user_id = $1`, []any{mona}, 2},
		{"mona's follows", `SELECT count(*) FROM team_follows WHERE user_id = $1`, []any{mona}, 2},
		{"mona's settings", `SELECT count(*) FROM notification_settings WHERE user_id = $1`, []any{mona}, 3},
	} {
		if n := count(c.sql, c.args...); n != c.want {
			t.Errorf("%s = %d, want %d", c.name, n, c.want)
		}
	}
	if n := count(`SELECT coalesce(max(seq), 0) FROM events WHERE org_id = $1`, e.org.ID); n != seqBefore {
		t.Errorf("the cleanup emitted %d events", n-seqBefore)
	}
	again, err := tracker.RemoveLostTeamNotifyRows(ctx, e.q, e.org.ID, rita, []uuid.UUID{tb.ID})
	if err != nil || again != (tracker.LostTeamNotifyRows{}) {
		t.Fatalf("second call deleted %+v (%v), want nothing", again, err)
	}
	if none, err := tracker.RemoveLostTeamNotifyRows(ctx, e.q, e.org.ID, rita, nil); err != nil || none != (tracker.LostTeamNotifyRows{}) {
		t.Fatalf("no lost team deleted %+v (%v)", none, err)
	}
}
