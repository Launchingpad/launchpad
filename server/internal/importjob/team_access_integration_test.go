package importjob

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"devops/internal/auth"
	"devops/internal/testdb"
	"devops/internal/tracker"
)

// decisions-T20 #67 (ticket 8): the web import job runs the importer with
// system authority. In an org with a restricted member, the team it creates
// is not visible to that member, the member's access is not changed, and an
// all-teams member sees the team.
func TestRun_RestrictedMemberDoesNotSeeImportedTeam(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	testdb.MustExec(t, h.pool, `UPDATE teams SET key = 'OLD' WHERE org_id = $1 AND key = 'ORB'`, h.org.ID)
	var old uuid.UUID
	if err := h.pool.QueryRow(ctx, `SELECT id FROM teams WHERE org_id = $1 AND key = 'OLD'`, h.org.ID).Scan(&old); err != nil {
		t.Fatal(err)
	}
	rita := testdb.AddMember(t, h.pool, h.org.ID, "rita", "member")
	testdb.MakeRestricted(t, h.pool, h.org.ID, rita, old)
	rows := func() string {
		var out string
		if err := h.pool.QueryRow(ctx, `SELECT coalesce(string_agg(m.user_id::text || m.team_access, ',' ORDER BY m.user_id), '') ||
			'|' || coalesce((SELECT string_agg(user_id::text || team_id::text, ',' ORDER BY user_id, team_id) FROM team_members WHERE org_id = $1), '')
			FROM org_members m WHERE m.org_id = $1`, h.org.ID).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := rows()

	s := h.mapped()
	h.dryDone(s)
	j, err := h.runStart(s)
	ok(t, err)
	if v := h.wait(j.ID); v.State != JobSucceeded || !v.Committed {
		t.Fatalf("run %+v", v)
	}
	if h.count(`SELECT count(*) FROM teams WHERE org_id = $1 AND key = 'ORB'`, h.org.ID) != 1 || h.issuesImported() == 0 {
		t.Fatal("the import made no team ORB or no issue")
	}
	if after := rows(); after != before {
		t.Fatalf("the import changed the members' access:\nbefore %s\nafter  %s", before, after)
	}

	svc := tracker.New(h.pool)
	keys := func(a auth.TeamAccess) (out []string, issues int) {
		org := &auth.Org{ID: h.org.ID, Slug: h.org.Slug, Name: h.org.Name, Role: "member", Access: a}
		ts, err := svc.ListTeams(ctx, org)
		if err != nil {
			t.Fatal(err)
		}
		for _, tm := range ts {
			out = append(out, tm.Key)
		}
		page, err := svc.ListIssues(ctx, org, &auth.Principal{UserID: rita, Scopes: []auth.Scope{auth.ScopeRead}}, tracker.ListFilter{Limit: 250})
		if err != nil {
			t.Fatal(err)
		}
		return out, len(page.Items)
	}
	if ks, n := keys(auth.NewTeamAccess("member", "selected", []uuid.UUID{old})); len(ks) != 1 || ks[0] != "OLD" || n != 0 {
		t.Errorf("the restricted member sees teams %v and %d issues; want OLD and none", ks, n)
	}
	if ks, n := keys(auth.TeamAccess{All: true}); len(ks) != 2 || n == 0 {
		t.Errorf("an all-teams member sees teams %v and %d issues", ks, n)
	}
}
