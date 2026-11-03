package importer

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestParseUserMap(t *testing.T) {
	tests := []struct {
		name, doc, err string
	}{
		{"ok", `{"default":"Chen","users":{"ID-1":" Claude "},"_users":[{"id":"x"}]}`, ""},
		{"no users and no default", `{}`, "no users and no default"},
		{"empty username", `{"users":{"a":""}}`, "empty key or username"},
		{"placeholder left in", `{"users":{"a":"CHANGE_ME"}}`, "still CHANGE_ME"},
		{"placeholder default", `{"default":"change_me","users":{"a":"claude"}}`, "default is still CHANGE_ME"},
		{"not json", `{`, "parse user map"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := ParseUserMap([]byte(tt.doc))
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if m.Default != "chen" || m.Users["id-1"] != "claude" {
				t.Errorf("map not normalised: %+v", m)
			}
		})
	}
}

func TestUserMap_Resolve(t *testing.T) {
	m, err := ParseUserMap([]byte(`{"users":{"u1":"claude","Chen@Example.com":"chen"}}`))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		u    User
		want string
		ok   bool
	}{
		{"by id", User{ID: "U1"}, "claude", true},
		{"by email, any case", User{ID: "u9", Email: "chen@example.COM"}, "chen", true},
		{"unmapped with no default", User{ID: "u9", Email: "x@y"}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := m.Resolve(tt.u)
			if got != tt.want || ok != tt.ok {
				t.Errorf("Resolve = %q, %v", got, ok)
			}
		})
	}
	m.Default = "chen"
	if got, ok := m.Resolve(User{ID: "u9"}); !ok || got != "chen" {
		t.Errorf("default not used: %q %v", got, ok)
	}
	if got := m.Usernames(); !slices.Equal(got, []string{"chen", "claude"}) {
		t.Errorf("Usernames = %v", got)
	}
}

func TestUserMapTemplate(t *testing.T) {
	b, err := UserMapTemplate(fixtureExport().Users)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(b)), "example.com") {
		t.Error("the template must not carry email addresses")
	}
	if _, err := ParseUserMap(b); err == nil || !strings.Contains(err.Error(), ChangeMe) {
		t.Fatalf("an unedited template must be refused and name %s: %v", ChangeMe, err)
	}
	m, err := ParseUserMap([]byte(strings.ReplaceAll(string(b), ChangeMe, "chen")))
	if err != nil {
		t.Fatalf("an edited template must parse as a user map: %v", err)
	}
	if m.Default != "" {
		t.Error("the template has no default: every user is listed on purpose")
	}
	if m.Users[strings.ToLower(fxUBot)] != "claude" || m.Users[strings.ToLower(fxUChen)] != "chen" {
		t.Errorf("guesses %v", m.Users)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil || raw["_users"] == nil {
		t.Errorf("the template lists the users under _users: %v", err)
	}
}
