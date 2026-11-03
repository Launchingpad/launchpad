package testdb_test

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/auth"
	"devops/internal/dbq"
	"devops/internal/testdb"
	"devops/internal/tracker"
)

// LAUN-215 (T20 #1): the schema and the access reads. Nothing filters yet.
func TestTeamAccess(t *testing.T) {
	pool, _ := testdb.New(t)
	d := testdb.Fixture(t, pool)
	ctx := context.Background()
	q := dbq.New(pool)
	mk := func(key string) dbq.Team {
		tm, err := tracker.CreateTeamTx(ctx, q, d.OrgID, key, key, "")
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	tb, tc := mk("BBB"), mk("CCC")
	count := func(sql string, args ...any) int {
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Existing members are 'all' after up: the fixture ones, and a new one.
	if n := count(`SELECT count(*) FROM org_members WHERE team_access <> 'all'`); n != 0 {
		t.Fatalf("%d members are not all", n)
	}
	alice := testdb.AddMember(t, pool, d.OrgID, "alice", "member")
	m, err := q.GetMembership(ctx, dbq.GetMembershipParams{OrgID: d.OrgID, UserID: alice})
	if err != nil || m.Role != "member" || m.TeamAccess != "all" {
		t.Fatalf("membership %+v %v", m, err)
	}
	if got := auth.NewTeamAccess(m.Role, m.TeamAccess, nil); !got.All {
		t.Fatalf("all member: %+v", got)
	}

	// A selected member with two rows has those two ids, sorted.
	testdb.MakeRestricted(t, pool, d.OrgID, alice, tc.ID, tb.ID)
	ids, err := q.ListMemberTeamIDs(ctx, dbq.ListMemberTeamIDsParams{OrgID: d.OrgID, UserID: alice})
	if err != nil {
		t.Fatal(err)
	}
	want := []uuid.UUID{tb.ID, tc.ID}
	slices.SortFunc(want, func(x, y uuid.UUID) int { return slices.Compare(x[:], y[:]) })
	if !slices.Equal(ids, want) {
		t.Fatalf("ids %v, want %v", ids, want)
	}
	m, _ = q.GetMembership(ctx, dbq.GetMembershipParams{OrgID: d.OrgID, UserID: alice})
	acc := auth.NewTeamAccess(m.Role, m.TeamAccess, ids)
	if acc.All || !slices.Equal(acc.IDs, want) || !acc.Can(tb.ID) || acc.Can(uuid.New()) {
		t.Fatalf("selected: %+v", acc)
	}

	// An admin is All whatever the flag.
	chen := userID(t, pool, "chen")
	testdb.MakeRestricted(t, pool, d.OrgID, chen, tb.ID)
	cm, _ := q.GetMembership(ctx, dbq.GetMembershipParams{OrgID: d.OrgID, UserID: chen})
	if cm.TeamAccess != "selected" || !auth.NewTeamAccess(cm.Role, cm.TeamAccess, []uuid.UUID{tb.ID}).All {
		t.Fatalf("admin %+v", cm)
	}

	// StillAuthorized returns the access.
	svc := tracker.New(pool)
	tok := tokenID(t, q, alice, d.OrgID)
	got, ok, err := svc.StillAuthorized(ctx, &auth.Principal{UserID: alice, TokenID: tok}, d.OrgID)
	if err != nil || !ok || got.Access.All || !slices.Equal(got.Access.IDs, want) || got.Role != "member" {
		t.Fatalf("StillAuthorized %+v %v %v", got, ok, err)
	}

	// A deleted team removes its rows.
	testdb.MustExec(t, pool, `DELETE FROM teams WHERE id = $1`, tc.ID)
	ids, _ = q.ListMemberTeamIDs(ctx, dbq.ListMemberTeamIDsParams{OrgID: d.OrgID, UserID: alice})
	if !slices.Equal(ids, []uuid.UUID{tb.ID}) {
		t.Fatalf("after team delete: %v", ids)
	}
	// A deleted member removes its rows.
	testdb.MustExec(t, pool, `DELETE FROM org_members WHERE org_id = $1 AND user_id = $2`, d.OrgID, alice)
	if n := count(`SELECT count(*) FROM team_members WHERE user_id = $1`, alice); n != 0 {
		t.Fatalf("%d rows left", n)
	}
}

// Cross-org: a team_members row cannot join a member of one org to a team or
// member of another (composite FKs).
func TestTeamMembersCrossOrg(t *testing.T) {
	pool, _ := testdb.New(t)
	d := testdb.Fixture(t, pool)
	ctx := context.Background()
	q := dbq.New(pool)
	other, err := q.InsertOrg(ctx, dbq.InsertOrgParams{Slug: "acme", Name: "Acme"})
	if err != nil {
		t.Fatal(err)
	}
	otherTeam, err := tracker.CreateTeamTx(ctx, q, other.ID, "ACM", "Acme", "")
	if err != nil {
		t.Fatal(err)
	}
	mine := testdb.AddMember(t, pool, d.OrgID, "bob", "member")
	theirs := testdb.AddMember(t, pool, other.ID, "carol", "member")
	var team uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM teams WHERE org_id = $1`, d.OrgID).Scan(&team); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO team_members (org_id, user_id, team_id) VALUES ($1, $2, $3)`
	// org A member, org B team (row stamped with either org)
	for _, org := range []uuid.UUID{d.OrgID, other.ID} {
		if _, err := pool.Exec(ctx, ins, org, mine, otherTeam.ID); err == nil {
			t.Fatalf("org %s: member of one org joined to another org's team", org)
		}
	}
	// org B member, org A team
	for _, org := range []uuid.UUID{d.OrgID, other.ID} {
		if _, err := pool.Exec(ctx, ins, org, theirs, team); err == nil {
			t.Fatalf("org %s: another org's member joined to this org's team", org)
		}
	}
	// the same org works
	if _, err := pool.Exec(ctx, ins, d.OrgID, mine, team); err != nil {
		t.Fatalf("same-org insert: %v", err)
	}
}

func userID(t *testing.T, pool *pgxpool.Pool, username string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `SELECT id FROM users WHERE username = $1`, username).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func tokenID(t *testing.T, q *dbq.Queries, userID, orgID uuid.UUID) uuid.UUID {
	t.Helper()
	nt, err := auth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	row, err := q.InsertAPIToken(context.Background(), dbq.InsertAPITokenParams{
		UserID: userID, OrgID: &orgID, Kind: auth.KindPAT, Name: "t", TokenHash: nt.Hash,
		Prefix: nt.Prefix, Scopes: []string{"read"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return row.ID
}
