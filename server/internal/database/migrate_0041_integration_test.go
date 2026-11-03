package database_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"devops/internal/testdb"
)

// TestMigration0041_InviteTeamAccess checks 0041 (LAUN-226, T20 M3): after
// it an existing invite is 'all', a 'selected' invite holds team rows of its
// own org only, an admin invite cannot be 'selected', and a deleted team
// leaves the invite. The down FAILS while an open 'selected' invite exists
// and changes nothing; after the operator revokes it (the runbook's step 2)
// the down passes, and up again works.
func TestMigration0041_InviteTeamAccess(t *testing.T) {
	dsn := testdb.Fresh(t)
	m := migratorFor(t, dsn)
	migrateTo(t, m, 40)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	one := func(q string, args ...any) string {
		t.Helper()
		var s string
		if err := conn.QueryRow(ctx, q, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return s
	}
	exec := func(q string, args ...any) error {
		_, err := conn.Exec(ctx, q, args...)
		return err
	}
	org := one(`INSERT INTO orgs (slug, name) VALUES ('alpha-org', 'alpha') RETURNING id::text`)
	other := one(`INSERT INTO orgs (slug, name) VALUES ('beta-org', 'beta') RETURNING id::text`)
	team := one(`INSERT INTO teams (org_id, key, name) VALUES ($1, 'AAA', 'a') RETURNING id::text`, org)
	team2 := one(`INSERT INTO teams (org_id, key, name) VALUES ($1, 'BBB', 'b') RETURNING id::text`, org)
	foreign := one(`INSERT INTO teams (org_id, key, name) VALUES ($1, 'CCC', 'c') RETURNING id::text`, other)
	invite := func(email, role string) string {
		return one(`INSERT INTO org_invites (org_id, email, role, token_hash, expires_at)
			VALUES ($1, $2, $3, sha256(convert_to($2::text, 'UTF8')), now() + interval '7 days') RETURNING id::text`, org, email, role)
	}
	old := invite("old@example.com", "member")

	migrateTo(t, m, 41)
	if got := one(`SELECT team_access FROM org_invites WHERE id = $1`, old); got != "all" {
		t.Fatalf("an existing invite is %q after up, want all", got)
	}
	sel := invite("sel@example.com", "member")
	if err := exec(`UPDATE org_invites SET team_access = 'selected' WHERE id = $1`, sel); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO org_invite_teams (org_id, invite_id, team_id) VALUES ($1, $2, $3), ($1, $2, $4)`, org, sel, team, team2); err != nil {
		t.Fatal(err)
	}
	if err := exec(`INSERT INTO org_invite_teams (org_id, invite_id, team_id) VALUES ($1, $2, $3)`, org, sel, foreign); err == nil {
		t.Error("another org's team was accepted")
	}
	if err := exec(`UPDATE org_invites SET team_access = 'bogus' WHERE id = $1`, old); err == nil {
		t.Error("a bogus team_access was accepted")
	}
	adm := invite("adm@example.com", "admin")
	if err := exec(`UPDATE org_invites SET team_access = 'selected' WHERE id = $1`, adm); err == nil {
		t.Error("an admin invite became selected")
	}
	if err := exec(`DELETE FROM teams WHERE id = $1`, team2); err != nil {
		t.Fatal(err)
	}
	if got := one(`SELECT count(*)::text FROM org_invite_teams WHERE invite_id = $1`, sel); got != "1" {
		t.Fatalf("%s team rows after a team delete, want 1 (the cascade)", got)
	}

	// The down refuses while the selected invite is open, and changes nothing.
	err = m.Migrate(40)
	if err == nil || !strings.Contains(err.Error(), "revoke them first") {
		t.Fatalf("down with an open selected invite: %v, want the refusal", err)
	}
	if err := m.Force(41); err != nil {
		t.Fatal(err)
	}
	if got := one(`SELECT team_access || ':' || (SELECT count(*) FROM org_invite_teams)::text FROM org_invites WHERE id = $1`, sel); got != "selected:1" {
		t.Fatalf("the refused down changed the invite: %q", got)
	}
	// The runbook's step 2, then the down passes.
	if err := exec(`UPDATE org_invites SET revoked_at = now() WHERE team_access = 'selected' AND accepted_at IS NULL AND revoked_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	migrateTo(t, m, 40)
	if got := one(`SELECT count(*)::text FROM information_schema.columns WHERE table_name = 'org_invites' AND column_name = 'team_access'`); got != "0" {
		t.Error("column still there after down")
	}
	if got := one(`SELECT count(*)::text FROM information_schema.tables WHERE table_name = 'org_invite_teams'`); got != "0" {
		t.Error("table still there after down")
	}
	if got := one(`SELECT count(*)::text FROM org_invites`); got != "3" {
		t.Errorf("%s invites after down, want 3 (the down deletes none)", got)
	}
	migrateTo(t, m, 41)
	if got := one(`SELECT string_agg(team_access, ',') FROM org_invites`); got != "all,all,all" {
		t.Errorf("after up again: %q", got)
	}
}
