package importer

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// UserMap turns Linear users into Launchpad usernames (for example alice and
// claude). The file is JSON:
//
//	{
//	  "default": "alice",
//	  "users": { "<linear user id>": "claude", "someone@example.com": "alice" }
//	}
//
// A key is a Linear user id or an email (case-insensitive). "default" is
// used for every Linear user that no key names; with no default, an
// unmapped user is an error, so nobody is silently folded into someone else.
// Any other top-level field (the export's "_users" listing) is ignored.
type UserMap struct {
	Default string            `json:"default"`
	Users   map[string]string `json:"users"`
}

// LoadUserMap reads a user map file.
func LoadUserMap(path string) (*UserMap, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read user map: %w", err)
	}
	return ParseUserMap(b)
}

// ParseUserMap parses a user map document.
func ParseUserMap(b []byte) (*UserMap, error) {
	var m UserMap
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse user map: %w", err)
	}
	norm := make(map[string]string, len(m.Users))
	for k, v := range m.Users {
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.ToLower(strings.TrimSpace(v))
		if k == "" || v == "" {
			return nil, fmt.Errorf("user map: empty key or username")
		}
		if v == strings.ToLower(ChangeMe) {
			return nil, fmt.Errorf("user map: %q is still %s: set a Launchpad username for it", k, ChangeMe)
		}
		norm[k] = v
	}
	m.Users = norm
	m.Default = strings.ToLower(strings.TrimSpace(m.Default))
	if m.Default == strings.ToLower(ChangeMe) {
		return nil, fmt.Errorf("user map: default is still %s: set a Launchpad username or remove it", ChangeMe)
	}
	if len(m.Users) == 0 && m.Default == "" {
		return nil, fmt.Errorf("user map: no users and no default")
	}
	return &m, nil
}

// Resolve is the username for a Linear user, by id, then email, then the
// default. ok is false when nothing matches.
func (m *UserMap) Resolve(u User) (string, bool) {
	if v, ok := m.Users[strings.ToLower(u.ID)]; ok {
		return v, true
	}
	if u.Email != "" {
		if v, ok := m.Users[strings.ToLower(u.Email)]; ok {
			return v, true
		}
	}
	if m.Default != "" {
		return m.Default, true
	}
	return "", false
}

// Usernames is every dev-ops username the map can produce, sorted.
func (m *UserMap) Usernames() []string {
	set := map[string]bool{}
	for _, v := range m.Users {
		set[v] = true
	}
	if m.Default != "" {
		set[m.Default] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ChangeMe is the placeholder the template writes for a user it cannot guess.
// A map that still holds it is refused, so nobody is mapped by accident.
const ChangeMe = "CHANGE_ME"

// UserMapTemplate is the file the export writes next to the JSONL: every
// Linear user mapped to a guess (an app user to claude, anyone else to
// the placeholder CHANGE_ME), plus an informational "_users" list. An operator checks it and
// passes it to import with --user-map.
func UserMapTemplate(users []User) ([]byte, error) {
	type info struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		App    bool   `json:"app"`
		Active bool   `json:"active"`
	}
	t := struct {
		Default string            `json:"default"`
		Users   map[string]string `json:"users"`
		Info    []info            `json:"_users"`
	}{Default: "", Users: map[string]string{}, Info: []info{}}
	for _, u := range users {
		guess := ChangeMe
		if u.App || strings.Contains(strings.ToLower(u.Name+" "+u.DisplayName), "claude") {
			guess = "claude"
		}
		t.Users[u.ID] = guess
		t.Info = append(t.Info, info{ID: u.ID, Name: u.DisplayName, App: u.App, Active: u.Active})
	}
	sort.Slice(t.Info, func(i, j int) bool { return t.Info[i].ID < t.Info[j].ID })
	return json.MarshalIndent(t, "", "  ")
}
