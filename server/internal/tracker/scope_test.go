package tracker

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"devops/internal/auth"
)

func scopeArgs() (argFunc, *[]any) {
	var args []any
	return func(v any) string {
		args = append(args, v)
		return "$" + string(rune('0'+len(args)))
	}, &args
}

func TestTeamScopeAllTeams(t *testing.T) {
	org := &auth.Org{Access: auth.TeamAccess{All: true}}
	arg, args := scopeArgs()
	got := teamScope(org, "i.team_id", arg)
	if got != "($1::bool OR i.team_id = ANY($2::uuid[]))" {
		t.Fatalf("text = %q", got)
	}
	if len(*args) != 2 || (*args)[0] != true {
		t.Fatalf("args = %v; want all_teams true", *args)
	}
	if ids := (*args)[1].([]uuid.UUID); ids == nil || len(ids) != 0 {
		t.Fatalf("team_ids = %#v; want an empty non-nil slice", ids)
	}
}

func TestTeamScopeRestricted(t *testing.T) {
	a, b := uuid.MustParse("00000000-0000-0000-0000-00000000000a"), uuid.MustParse("00000000-0000-0000-0000-00000000000b")
	org := &auth.Org{Access: auth.NewTeamAccess("member", "selected", []uuid.UUID{b, a})}
	arg, args := scopeArgs()
	got := teamScope(org, "c.team_id", arg)
	if !strings.Contains(got, "c.team_id = ANY($2::uuid[])") {
		t.Fatalf("text = %q", got)
	}
	if (*args)[0] != false {
		t.Fatalf("all_teams = %v; want false", (*args)[0])
	}
	ids := (*args)[1].([]uuid.UUID)
	if len(ids) != 2 || ids[0] != a || ids[1] != b {
		t.Fatalf("team_ids = %v; want [a b]", ids)
	}
	// A restricted member with no team still gets a clause that matches nothing.
	arg, args = scopeArgs()
	teamScope(&auth.Org{Access: auth.NewTeamAccess("member", "selected", nil)}, "c.team_id", arg)
	if (*args)[0] != false || len((*args)[1].([]uuid.UUID)) != 0 {
		t.Fatalf("empty access args = %v", *args)
	}
}

func TestTeamScopeAdminIsAll(t *testing.T) {
	org := &auth.Org{Access: auth.NewTeamAccess("admin", "selected", []uuid.UUID{uuid.New()})}
	arg, args := scopeArgs()
	teamScope(org, "x", arg)
	if (*args)[0] != true {
		t.Fatalf("an admin binds all_teams = %v", (*args)[0])
	}
}

func TestProjectScope(t *testing.T) {
	a := uuid.MustParse("00000000-0000-0000-0000-00000000000a")
	for _, tc := range []struct {
		name string
		org  *auth.Org
		all  bool
		ids  int
	}{
		{"all", &auth.Org{Access: auth.TeamAccess{All: true}}, true, 0},
		{"restricted", &auth.Org{Access: auth.NewTeamAccess("member", "selected", []uuid.UUID{a})}, false, 1},
		{"none", &auth.Org{Access: auth.NewTeamAccess("member", "selected", nil)}, false, 0},
	} {
		arg, args := scopeArgs()
		got := projectScope(tc.org, "p", arg)
		if len(*args) != 2 || (*args)[0] != tc.all || len((*args)[1].([]uuid.UUID)) != tc.ids {
			t.Errorf("%s: args = %v", tc.name, *args)
		}
		// One text for every caller: the any-of rule with the org-level case.
		for _, want := range []string{"$1::bool", "NOT EXISTS (SELECT 1 FROM project_teams pst", "pst.team_id = ANY($2::uuid[])", "p.id"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: %q missing from %q", tc.name, want, got)
			}
		}
	}
}

func TestDocumentScope(t *testing.T) {
	a := uuid.MustParse("00000000-0000-0000-0000-00000000000a")
	for _, tc := range []struct {
		name string
		org  *auth.Org
		all  bool
	}{
		{"all", &auth.Org{Access: auth.TeamAccess{All: true}}, true},
		{"restricted", &auth.Org{Access: auth.NewTeamAccess("member", "selected", []uuid.UUID{a})}, false},
	} {
		arg, args := scopeArgs()
		got := documentScope(tc.org, "d", arg)
		if len(*args) != 2 || (*args)[0] != tc.all {
			t.Errorf("%s: args = %v", tc.name, *args)
		}
		// A document with no project is org-level; otherwise it follows its project.
		for _, want := range []string{"d.project_id IS NULL OR EXISTS", "dp.id = d.project_id", "$1::bool", "pst.team_id = ANY($2::uuid[])"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: %q missing from %q", tc.name, want, got)
			}
		}
	}
}
