package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// Agent briefs written for Linear name a team by its NAME ("Orbit") and a
// person by a display name ("Chen"). The REST API resolves a team only by id
// or key, and a person only by username or `me`. So the MCP layer resolves
// both itself, from GET /teams and GET /users, before calling the API
// (decisions-T2 #19, #20). Both lists are cached for the life of the process
// and re-read once when a name is not found (a team or member added since).

type teamRef struct {
	ID   string `json:"id"`
	Key  string `json:"key"`
	Name string `json:"name"`
}

type userRef struct {
	Username string  `json:"username"`
	Name     string  `json:"name"`
	Email    *string `json:"email"`
}

// directory caches the org's teams and members. Fetches run outside the
// lock, so a slow API never blocks other calls on it.
type directory struct {
	mu     sync.Mutex
	teams  []teamRef
	users  []userRef
	states map[string][]string // team key -> state names
}

func (t *Tools) teamList(ctx context.Context, refresh bool) ([]teamRef, error) {
	t.dir.mu.Lock()
	cached := t.dir.teams
	t.dir.mu.Unlock()
	if cached != nil && !refresh {
		return cached, nil
	}
	var page Page[teamRef]
	if err := t.c.Get(ctx, "teams", nil, &page); err != nil {
		return nil, err
	}
	if page.Items == nil {
		page.Items = []teamRef{}
	}
	t.dir.mu.Lock()
	t.dir.teams = page.Items
	t.dir.mu.Unlock()
	return page.Items, nil
}

func (t *Tools) userList(ctx context.Context, refresh bool) ([]userRef, error) {
	t.dir.mu.Lock()
	cached := t.dir.users
	t.dir.mu.Unlock()
	if cached != nil && !refresh {
		return cached, nil
	}
	var page Page[userRef]
	if err := t.c.Get(ctx, "users", nil, &page); err != nil {
		return nil, err
	}
	if page.Items == nil {
		page.Items = []userRef{}
	}
	t.dir.mu.Lock()
	t.dir.users = page.Items
	t.dir.mu.Unlock()
	return page.Items, nil
}

// resolveTeam turns a team key, id or name (any case) into the team's key.
func (t *Tools) resolveTeam(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("empty team")
	}
	var teams []teamRef
	for attempt := range 2 {
		var err error
		if teams, err = t.teamList(ctx, attempt == 1); err != nil {
			return "", err
		}
		var byName []teamRef
		for _, tm := range teams {
			if strings.EqualFold(tm.ID, ref) || strings.EqualFold(tm.Key, ref) {
				return tm.Key, nil
			}
			if strings.EqualFold(tm.Name, ref) {
				byName = append(byName, tm)
			}
		}
		switch len(byName) {
		case 1:
			return byName[0].Key, nil
		case 0:
		default:
			keys := make([]string, 0, len(byName))
			for _, tm := range byName {
				keys = append(keys, tm.Key)
			}
			return "", fmt.Errorf("%d teams are named %q (%s); pass the key", len(byName), ref, strings.Join(keys, ", "))
		}
	}
	known := make([]string, 0, len(teams))
	for _, tm := range teams {
		known = append(known, fmt.Sprintf("%s (%s)", tm.Key, tm.Name))
	}
	msg := fmt.Sprintf("unknown team %q; teams: %s", ref, strings.Join(known, ", "))
	if _, err := uuid.Parse(ref); err == nil {
		msg += ". A Linear team id is not a team id here: pass the key or the name"
	}
	return "", fmt.Errorf("%s", msg)
}

// teamOr resolves the team argument k, or the configured default team.
func (t *Tools) teamOr(ctx context.Context, a args, k string) (string, error) {
	ref := a.str(k)
	if ref == "" {
		ref = t.team
	}
	if ref == "" {
		return "", fmt.Errorf("argument %q is required (or set LAUNCHPAD_TEAM)", k)
	}
	return t.resolveTeam(ctx, ref)
}

// stateTypes are the workflow state types (decisions-T1 #8).
var stateTypes = map[string]bool{"triage": true, "backlog": true, "unstarted": true, "started": true, "completed": true, "canceled": true}

// stateNames returns the state names of the team with key, cached.
func (t *Tools) stateNames(ctx context.Context, key string) ([]string, error) {
	t.dir.mu.Lock()
	names, ok := t.dir.states[key]
	t.dir.mu.Unlock()
	if ok {
		return names, nil
	}
	p, err := orgPath("teams", key, "states")
	if err != nil {
		return nil, err
	}
	var page Page[struct {
		Name string `json:"name"`
	}]
	if err := t.c.Get(ctx, p, nil, &page); err != nil {
		return nil, err
	}
	names = make([]string, 0, len(page.Items))
	for _, st := range page.Items {
		names = append(names, st.Name)
	}
	t.dir.mu.Lock()
	if t.dir.states == nil {
		t.dir.states = map[string][]string{}
	}
	t.dir.states[key] = names
	t.dir.mu.Unlock()
	return names, nil
}

// stateAsType reports whether list_issues' state value is really a list of
// state TYPES, as Linear allows ("started", "unstarted,backlog"): every part
// is a type and no state of the team (or of any team, with no team) has that
// name. Then it returns the lower-cased types for the API's stateType filter.
func (t *Tools) stateAsType(ctx context.Context, teamKey, state string) (string, bool, error) {
	parts := strings.Split(state, ",")
	types := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if !stateTypes[p] {
			return "", false, nil
		}
		types = append(types, p)
	}
	keys := []string{teamKey}
	if teamKey == "" {
		teams, err := t.teamList(ctx, false)
		if err != nil {
			return "", false, err
		}
		keys = keys[:0]
		for _, tm := range teams {
			keys = append(keys, tm.Key)
		}
	}
	for _, k := range keys {
		names, err := t.stateNames(ctx, k)
		if err != nil {
			return "", false, err
		}
		for _, n := range names {
			for _, ty := range types {
				if strings.EqualFold(n, ty) {
					return "", false, nil // "Backlog" is a name too: keep it a name
				}
			}
		}
	}
	return strings.Join(types, ","), true, nil
}

// resolveUser turns a username, email or display name (any case) into the
// username the API takes. `me` and `none` pass through. A value that matches
// nobody is passed through unchanged, so the API's own error reaches the
// agent.
func (t *Tools) resolveUser(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" || strings.EqualFold(ref, "me") || strings.EqualFold(ref, "none") {
		return strings.ToLower(ref), nil
	}
	for attempt := range 2 {
		users, err := t.userList(ctx, attempt == 1)
		if err != nil {
			return "", err
		}
		var byName []string
		for _, u := range users {
			if strings.EqualFold(u.Username, ref) || (u.Email != nil && strings.EqualFold(*u.Email, ref)) {
				return u.Username, nil
			}
			if strings.EqualFold(u.Name, ref) {
				byName = append(byName, u.Username)
			}
		}
		switch len(byName) {
		case 1:
			return byName[0], nil
		case 0:
		default:
			return "", fmt.Errorf("%d members are named %q (%s); pass the username", len(byName), ref, strings.Join(byName, ", "))
		}
	}
	return ref, nil
}
