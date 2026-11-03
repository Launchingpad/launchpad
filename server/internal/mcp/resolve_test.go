package mcp

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestTools_ResolveTeam(t *testing.T) {
	api := newFakeAPI(t, nil)
	s := newSession(t, ProfileFull, api)
	ctx := context.Background()
	for ref, want := range map[string]string{
		"Orbit": "ORB", "orbit": "ORB", "ORB": "ORB", "orb": "ORB", " Web ": "WEB",
		"0190f5f0-0000-7000-8000-0000000000e1": "ORB", "0190F5F0-0000-7000-8000-0000000000E2": "WEB",
	} {
		got, err := s.tools.resolveTeam(ctx, ref)
		if err != nil || got != want {
			t.Errorf("resolveTeam(%q) = %q, %v; want %q", ref, got, err, want)
		}
	}
	if n := api.total(); n != 1 {
		t.Errorf("GET /teams ran %d times, want 1 (cached)", n)
	}
	_, err := s.tools.resolveTeam(ctx, "Mobile")
	if err == nil || !strings.Contains(err.Error(), `unknown team "Mobile"`) || !strings.Contains(err.Error(), "ORB (Orbit)") {
		t.Errorf("unknown team: %v", err)
	}
	_, err = s.tools.resolveTeam(ctx, "1bfad36e-7b0a-4e27-8af1-2d10f49b3209")
	if err == nil || !strings.Contains(err.Error(), "Linear team id") {
		t.Errorf("Linear team id: %v", err)
	}
	// A team made after start-up is found by re-reading the list once.
	api.mu.Lock()
	api.teams = append(append([]any{}, fakeTeams...), map[string]any{"id": "t3", "key": "MOB", "name": "Mobile"})
	api.mu.Unlock()
	if got, err := s.tools.resolveTeam(ctx, "mobile"); err != nil || got != "MOB" {
		t.Errorf("new team = %q, %v", got, err)
	}
}

func TestTools_ResolveTeam_AmbiguousName(t *testing.T) {
	api := newFakeAPI(t, nil)
	api.teams = []any{map[string]any{"id": "a", "key": "A", "name": "Core"}, map[string]any{"id": "b", "key": "B", "name": "core"}}
	s := newSession(t, ProfileFull, api)
	if _, err := s.tools.resolveTeam(context.Background(), "Core"); err == nil || !strings.Contains(err.Error(), "2 teams are named") {
		t.Errorf("err = %v", err)
	}
}

func TestTools_ResolveUser(t *testing.T) {
	s := newSession(t, ProfileFull, newFakeAPI(t, nil))
	ctx := context.Background()
	for ref, want := range map[string]string{
		"me": "me", "Me": "me", "none": "none", "chen": "chen", "Chen": "chen", "CLAUDE": "claude",
		"chen@example.com": "chen", "nobody": "nobody",
	} {
		got, err := s.tools.resolveUser(ctx, ref)
		if err != nil || got != want {
			t.Errorf("resolveUser(%q) = %q, %v; want %q", ref, got, err, want)
		}
	}
}

// Every team argument takes the Linear team name, as today's briefs send it.
func TestCatalogue_TeamNames(t *testing.T) {
	api := relationsFake(t, nil)
	s := newSession(t, ProfileFull, api)
	s.okJSON("save_issue", map[string]any{"title": "x", "team": "Orbit", "assignee": "Chen"}, nil)
	s.okJSON("list_issues", map[string]any{"team": "Orbit", "assignee": "Chen", "state": "In Progress"}, nil)
	s.okJSON("list_issue_statuses", map[string]any{"team": "Orbit"}, nil)
	s.okJSON("list_cycles", map[string]any{"teamId": "Orbit"}, nil)
	s.okJSON("list_projects", map[string]any{"team": "orbit", "lead": "Chen"}, nil)
	s.okJSON("save_project", map[string]any{"name": "P", "teams": []string{"Orbit"}, "addTeams": []string{"Web", "ORB"}, "lead": "Claude"}, nil)

	var create, project map[string]any
	var listQ, cyclesQ, projQ url.Values
	var statesPath string
	for _, r := range api.requests() {
		switch {
		case r.Method == http.MethodPost && r.Path == "/api/v1/orgs/orbit/issues":
			create = r.Body
		case r.Method == http.MethodPost && r.Path == "/api/v1/orgs/orbit/projects":
			project = r.Body
		case r.Path == "/api/v1/orgs/orbit/issues":
			listQ = q(t, r)
		case r.Path == "/api/v1/orgs/orbit/cycles":
			cyclesQ = q(t, r)
		case r.Path == "/api/v1/orgs/orbit/projects":
			projQ = q(t, r)
		case strings.HasSuffix(r.Path, "/states"):
			statesPath = r.Path
		}
	}
	if create["team"] != "ORB" || create["assignee"] != "chen" {
		t.Errorf("create = %v", create)
	}
	if listQ.Get("team") != "ORB" || listQ.Get("assignee") != "chen" {
		t.Errorf("list query = %v", listQ)
	}
	if statesPath != "/api/v1/orgs/orbit/teams/ORB/states" {
		t.Errorf("states path = %s", statesPath)
	}
	if cyclesQ.Get("team") != "ORB" {
		t.Errorf("cycles query = %v", cyclesQ)
	}
	if projQ.Get("team") != "ORB" || projQ.Get("lead") != "chen" {
		t.Errorf("projects query = %v", projQ)
	}
	if teams, _ := project["teams"].([]any); len(teams) != 2 || teams[0] != "ORB" || teams[1] != "WEB" || project["lead"] != "claude" {
		t.Errorf("project = %v", project)
	}
	if r := s.callTool("list_issues", map[string]any{"team": "Nope"}); !r.IsError || !strings.Contains(r.Text, `unknown team "Nope"`) {
		t.Errorf("unknown team: %+v", r)
	}
}
