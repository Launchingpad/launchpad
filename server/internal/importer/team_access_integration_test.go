package importer

import (
	"context"
	"net/url"
	"testing"

	"github.com/google/uuid"

	"devops/internal/auth"
	"devops/internal/dbq"
	"devops/internal/testdb"
	"devops/internal/tracker"
)

// decisions-T20 #67 (ticket 8): the importer writes with system authority. A
// team it creates is visible to members with all teams and to nobody
// restricted until an admin adds it; a mapped member keeps the access an
// admin gave them (the import never writes team_access or team_members).
func TestRun_aRestrictedMemberDoesNotSeeTheImportedTeam(t *testing.T) {
	e := newImpEnv(t)
	ctx := context.Background()
	// The fixture's team takes another key, so the import makes a new ORB.
	testdb.MustExec(t, e.pool, `UPDATE teams SET key = 'OLD' WHERE org_id = $1 AND key = 'ORB'`, e.org.ID)
	var old uuid.UUID
	if err := e.pool.QueryRow(ctx, `SELECT id FROM teams WHERE org_id = $1 AND key = 'OLD'`, e.org.ID).Scan(&old); err != nil {
		t.Fatal(err)
	}
	// claude is in the user map (the Linear bot); rita is not.
	claude, err := e.q.GetUserByUsername(ctx, "claude")
	if err != nil {
		t.Fatal(err)
	}
	testdb.MakeRestricted(t, e.pool, e.org.ID, claude.ID, old)
	rita := testdb.AddMember(t, e.pool, e.org.ID, "rita", "member")
	testdb.MakeRestricted(t, e.pool, e.org.ID, rita, old)
	access := func(user uuid.UUID) string {
		var out string
		if err := e.pool.QueryRow(ctx, `SELECT m.team_access || ':' || coalesce((SELECT string_agg(team_id::text, ',' ORDER BY team_id)
			FROM team_members tm WHERE tm.org_id = m.org_id AND tm.user_id = m.user_id), '')
			FROM org_members m WHERE m.org_id = $1 AND m.user_id = $2`, e.org.ID, user).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	beforeClaude, beforeRita := access(claude.ID), access(rita)

	rep := e.mustRun(fixtureExport(), false)
	if got := *rep.Counts[EntTeam]; got.Create != 1 {
		t.Fatalf("team counts %+v, want the import to create ORB", got)
	}
	if access(claude.ID) != beforeClaude || access(rita) != beforeRita {
		t.Fatalf("the import changed a member's access: claude %s -> %s, rita %s -> %s",
			beforeClaude, access(claude.ID), beforeRita, access(rita))
	}
	orb, err := e.q.GetTeamByKeySystem(ctx, dbq.GetTeamByKeySystemParams{OrgID: e.org.ID, Key: "ORB"})
	if err != nil {
		t.Fatal(err)
	}

	view := func(user uuid.UUID, a auth.TeamAccess) (teams []string, issues int, projects int, gotIssue error) {
		org := &auth.Org{ID: e.org.ID, Slug: e.org.Slug, Name: e.org.Name, Role: "member", Access: a}
		p := &auth.Principal{UserID: user, Scopes: []auth.Scope{auth.ScopeRead}}
		ts, err := e.svc.ListTeams(ctx, org)
		if err != nil {
			t.Fatal(err)
		}
		for _, tm := range ts {
			teams = append(teams, tm.Key)
		}
		page, err := e.svc.ListIssues(ctx, org, p, tracker.ListFilter{Limit: 250})
		if err != nil {
			t.Fatal(err)
		}
		pf, err := tracker.ParseProjectFilter(url.Values{"limit": {"250"}})
		if err != nil {
			t.Fatal(err)
		}
		pr, err := e.svc.ListProjects(ctx, org, p, pf)
		if err != nil {
			t.Fatal(err)
		}
		_, gotIssue = e.svc.GetIssue(ctx, org, p, "ORB-2557", tracker.Includes{})
		return teams, len(page.Items), len(pr.Items), gotIssue
	}
	for _, who := range []struct {
		name string
		user uuid.UUID
	}{{"claude (mapped)", claude.ID}, {"rita", rita}} {
		teams, issues, projects, err := view(who.user, auth.NewTeamAccess("member", "selected", []uuid.UUID{old}))
		if len(teams) != 1 || teams[0] != "OLD" || issues != 0 || projects != 0 || err == nil {
			t.Errorf("%s sees teams %v, %d issues, %d projects, ORB-2557 err %v; want OLD only and nothing imported", who.name, teams, issues, projects, err)
		}
	}
	// A member with all teams sees the new team and its rows.
	teams, issues, projects, err := view(rita, auth.TeamAccess{All: true})
	if len(teams) != 2 || issues == 0 || projects == 0 || err != nil {
		t.Errorf("an all-teams member sees teams %v, %d issues, %d projects, ORB-2557 err %v", teams, issues, projects, err)
	}
	// And once an admin adds ORB, the restricted member sees it.
	testdb.PutInTeams(t, e.pool, e.org.ID, rita, orb.ID)
	teams, issues, _, err = view(rita, auth.NewTeamAccess("member", "selected", []uuid.UUID{old, orb.ID}))
	if len(teams) != 2 || issues == 0 || err != nil {
		t.Errorf("after an admin adds ORB: teams %v, %d issues, err %v", teams, issues, err)
	}
}
