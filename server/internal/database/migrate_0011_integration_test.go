package database_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"devops/internal/testdb"
)

// TestMigration0011_TriageState checks that every team without one gets a
// "Triage" state (type triage) first in its order, in its own org only; that
// a team with a triage-type state or a state already named Triage is left
// alone; and that the down moves issues to the default state and removes only
// the Triage states.
func TestMigration0011_TriageState(t *testing.T) {
	dsn := testdb.Fresh(t)
	m := migratorFor(t, dsn)
	migrateTo(t, m, 10)

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
	org := one(`INSERT INTO orgs (slug, name) VALUES ('alpha-org', 'alpha') RETURNING id::text`)
	org2 := one(`INSERT INTO orgs (slug, name) VALUES ('beta-org', 'beta') RETURNING id::text`)
	team := func(o, key string) string {
		return one(`INSERT INTO teams (org_id, key, name) VALUES ($1, $2, $2) RETURNING id::text`, o, key)
	}
	state := func(o, tm, name, typ string, pos float64) string {
		return one(`INSERT INTO workflow_states (org_id, team_id, name, type, position) VALUES ($1, $2, $3, $4, $5) RETURNING id::text`,
			o, tm, name, typ, pos)
	}
	plain, hasType, hasName, other := team(org, "AAA"), team(org, "BBB"), team(org, "CCC"), team(org2, "DDD")
	backlog := state(org, plain, "Backlog", "backlog", 0)
	state(org, plain, "Todo", "unstarted", 1)
	state(org, hasType, "Intake", "triage", 5)
	state(org, hasType, "Backlog", "backlog", 6)
	state(org, hasName, "Triage", "backlog", 0) // taken name, other type
	state(org2, other, "Backlog", "backlog", 3)
	if _, err := conn.Exec(ctx, `UPDATE teams SET default_state_id = $1 WHERE id = $2`, backlog, plain); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO issues (org_id, team_id, number, title, state_id) VALUES ($1, $2, 1, 'x', $3)`,
		org, plain, backlog); err != nil {
		t.Fatal(err)
	}

	migrateTo(t, m, 11)
	triage := func(tm string) string {
		return one(`SELECT COALESCE(string_agg(type || ':' || position::text, ','), '') FROM workflow_states WHERE team_id = $1 AND name = 'Triage'`, tm)
	}
	if got := triage(plain); got != "triage:-1" {
		t.Errorf("plain team: %q, want triage:-1 (first)", got)
	}
	if got := triage(other); got != "triage:2" {
		t.Errorf("other org's team: %q, want triage:2", got)
	}
	if got := triage(hasName); got != "backlog:0" {
		t.Errorf("team with a state named Triage: %q, want it untouched", got)
	}
	if got := one(`SELECT count(*)::text FROM workflow_states WHERE team_id = $1 AND type = 'triage'`, hasType); got != "1" {
		t.Errorf("team with a triage-type state has %s, want 1", got)
	}
	if got := one(`SELECT count(*)::text FROM workflow_states WHERE team_id = $1 AND name = 'Triage'`, hasType); got != "0" {
		t.Errorf("team with a triage-type state got a second one")
	}

	// An issue in Triage and a history row that names it.
	tri := one(`SELECT id::text FROM workflow_states WHERE team_id = $1 AND name = 'Triage'`, plain)
	iss := one(`SELECT id::text FROM issues WHERE team_id = $1`, plain)
	if _, err := conn.Exec(ctx, `UPDATE issues SET state_id = $1 WHERE id = $2`, tri, iss); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO issue_state_changes (org_id, issue_id, from_state_id, to_state_id, source)
		VALUES ($1, $2, $3, $4, 'api')`, org, iss, backlog, tri); err != nil {
		t.Fatal(err)
	}

	migrateTo(t, m, 10)
	if got := one(`SELECT state_id::text FROM issues WHERE id = $1`, iss); got != backlog {
		t.Errorf("down left the issue in %s, want the default state", got)
	}
	if got := one(`SELECT count(*)::text FROM workflow_states WHERE name = 'Triage' AND type = 'triage'`); got != "0" {
		t.Errorf("down left %s Triage states", got)
	}
	if got := one(`SELECT count(*)::text FROM workflow_states WHERE name = 'Triage' AND type = 'backlog'`); got != "1" {
		t.Errorf("down removed a state it did not make")
	}
	if got := one(`SELECT count(*)::text FROM workflow_states WHERE name = 'Intake'`); got != "1" {
		t.Errorf("down removed a triage-type state it did not make")
	}
}

// TestMigration0011_DownDefaults: the down works when a team's default is
// the Triage state, when it is NULL, and leaves a triage-type state with
// another name (and its issues) alone.
func TestMigration0011_DownDefaults(t *testing.T) {
	dsn := testdb.Fresh(t)
	m := migratorFor(t, dsn)
	migrateTo(t, m, 10)

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
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	org := one(`INSERT INTO orgs (slug, name) VALUES ('alpha-org', 'alpha') RETURNING id::text`)
	team := func(key string) string {
		return one(`INSERT INTO teams (org_id, key, name) VALUES ($1, $2, $2) RETURNING id::text`, org, key)
	}
	state := func(tm, name, typ string, pos float64) string {
		return one(`INSERT INTO workflow_states (org_id, team_id, name, type, position) VALUES ($1, $2, $3, $4, $5) RETURNING id::text`,
			org, tm, name, typ, pos)
	}
	issue := func(tm, st string, n int) string {
		return one(`INSERT INTO issues (org_id, team_id, number, title, state_id) VALUES ($1, $2, $3, 'x', $4) RETURNING id::text`,
			org, tm, n, st)
	}
	tDef, tNull, tImp := team("AAA"), team("BBB"), team("CCC")
	todoDef := state(tDef, "Todo", "unstarted", 1)
	backDef := state(tDef, "Backlog", "backlog", 2)
	_ = todoDef
	backNull := state(tNull, "Backlog", "backlog", 5)
	state(tNull, "Todo", "unstarted", 1)
	intake := state(tImp, "Intake", "triage", 0)
	backImp := state(tImp, "Backlog", "backlog", 1)
	exec(`UPDATE teams SET default_state_id = $1 WHERE id = $2`, backImp, tImp)

	migrateTo(t, m, 11)
	triDef := one(`SELECT id::text FROM workflow_states WHERE team_id = $1 AND name = 'Triage'`, tDef)
	triNull := one(`SELECT id::text FROM workflow_states WHERE team_id = $1 AND name = 'Triage'`, tNull)
	exec(`UPDATE teams SET default_state_id = $1 WHERE id = $2`, triDef, tDef) // default = Triage
	// tNull keeps a NULL default.
	iDef, iNull, iImp := issue(tDef, triDef, 1), issue(tNull, triNull, 1), issue(tImp, intake, 1)

	migrateTo(t, m, 10)
	if got := one(`SELECT default_state_id::text FROM teams WHERE id = $1`, tDef); got != backDef {
		t.Errorf("default was Triage: now %s, want the backlog state", got)
	}
	if got := one(`SELECT state_id::text FROM issues WHERE id = $1`, iDef); got != backDef {
		t.Errorf("issue of the team whose default was Triage is in %s, want Backlog", got)
	}
	if got := one(`SELECT state_id::text FROM issues WHERE id = $1`, iNull); got != backNull {
		t.Errorf("issue of the team with a NULL default is in %s, want its backlog state", got)
	}
	if got := one(`SELECT state_id::text FROM issues WHERE id = $1`, iImp); got != intake {
		t.Errorf("issue in an imported triage-type state moved to %s", got)
	}
	if got := one(`SELECT count(*)::text FROM workflow_states WHERE type = 'triage'`); got != "1" {
		t.Errorf("%s triage-type states left, want only the imported one", got)
	}
}
