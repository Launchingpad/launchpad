package tracker

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"devops/internal/httpx"
)

// TestTokenRules pins the rules POST /tokens and `server mint-token` share.
func TestTokenRules(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	s := &Service{now: func() time.Time { return now }}
	days := func(n int) *int { return &n }
	tests := []struct {
		name     string
		in       TokenInput
		role     string
		agent    bool
		wantErr  string
		wantName string
		scopes   []string
		expires  *time.Time
	}{
		{"agent read write 90 days", TokenInput{Name: " mcp ", Scopes: []string{"read", "write"}, ExpiresInDays: days(90)}, "member", true,
			"", "mcp", []string{"read", "write"}, new(now.Add(90 * 24 * time.Hour))},
		{"agent needs an expiry", TokenInput{Name: "mcp", Scopes: []string{"read"}}, "member", true, "expiresInDays (1-365) is required", "", nil, nil},
		{"agent at most 365 days", TokenInput{Name: "mcp", Scopes: []string{"read"}, ExpiresInDays: days(366)}, "member", true, "expiresInDays must be 1-365", "", nil, nil},
		{"zero days", TokenInput{Name: "mcp", Scopes: []string{"read"}, ExpiresInDays: days(0)}, "member", true, "expiresInDays must be 1-365", "", nil, nil},
		{"human no expiry", TokenInput{Name: "rescue", Scopes: []string{"read", "write", "admin"}}, "admin", false, "", "rescue", []string{"read", "write", "admin"}, nil},
		{"human 3650 days", TokenInput{Name: "x", Scopes: []string{"read"}, ExpiresInDays: days(3650)}, "member", false, "", "x", []string{"read"}, new(now.Add(3650 * 24 * time.Hour))},
		{"human 3651 days", TokenInput{Name: "x", Scopes: []string{"read"}, ExpiresInDays: days(3651)}, "member", false, "expiresInDays must be 1-3650", "", nil, nil},
		{"admin scope for a member", TokenInput{Name: "x", Scopes: []string{"admin"}, ExpiresInDays: days(1)}, "member", true, "the admin scope needs", "", nil, nil},
		{"unknown scope", TokenInput{Name: "x", Scopes: []string{"root"}}, "admin", false, "", "", nil, nil},
		{"no scopes", TokenInput{Name: "x"}, "admin", false, "", "", nil, nil},
		{"blank name", TokenInput{Name: "  ", Scopes: []string{"read"}}, "admin", false, "name must be 1-200", "", nil, nil},
		{"name 201", TokenInput{Name: strings.Repeat("n", 201), Scopes: []string{"read"}}, "admin", false, "name must be 1-200", "", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, scopes, exp, err := s.tokenRules(tt.in, tt.role, tt.agent, false)
			if tt.scopes == nil { // a refusal
				var he *httpx.Error
				if !errors.As(err, &he) || he.Code != httpx.CodeValidationFailed || !strings.Contains(he.Msg, tt.wantErr) {
					t.Fatalf("err = %v, want a validation_failed with %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || name != tt.wantName || !slices.Equal(scopes, tt.scopes) {
				t.Fatalf("tokenRules = %q %v %v", name, scopes, err)
			}
			if (exp == nil) != (tt.expires == nil) || (exp != nil && !exp.Equal(*tt.expires)) {
				t.Fatalf("expires %v, want %v", exp, tt.expires)
			}
		})
	}
}

// TestTokenRules_Session pins the extra rules for a token made by a web
// session (decisions-T11 #2-3): it must expire in 1-365 days and cannot have
// the admin scope, for the caller's own token and for an agent's.
func TestTokenRules_Session(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	s := &Service{now: func() time.Time { return now }}
	days := func(n int) *int { return &n }
	cases := []struct {
		name    string
		in      TokenInput
		agent   bool
		status  int
		wantErr string
	}{
		{"own token 90 days", TokenInput{Name: "x", Scopes: []string{"read", "write"}, ExpiresInDays: days(90)}, false, 0, ""},
		{"own token 365 days", TokenInput{Name: "x", Scopes: []string{"read"}, ExpiresInDays: days(365)}, false, 0, ""},
		{"own token needs an expiry", TokenInput{Name: "x", Scopes: []string{"read"}}, false, 400, "expiresInDays (1-365) is required"},
		{"own token 366 days", TokenInput{Name: "x", Scopes: []string{"read"}, ExpiresInDays: days(366)}, false, 400, "expiresInDays must be 1-365"},
		{"own token 0 days", TokenInput{Name: "x", Scopes: []string{"read"}, ExpiresInDays: days(0)}, false, 400, "expiresInDays must be 1-365"},
		{"admin scope is refused", TokenInput{Name: "x", Scopes: []string{"read", "admin"}, ExpiresInDays: days(30)}, false, 403, "admin scope"},
		{"admin scope is refused before the expiry check", TokenInput{Name: "x", Scopes: []string{"admin"}}, false, 403, "admin scope"},
		{"agent needs an expiry", TokenInput{Name: "x", Scopes: []string{"read"}}, true, 400, "required for an agent token"},
		{"agent 365 days", TokenInput{Name: "x", Scopes: []string{"read", "write"}, ExpiresInDays: days(365)}, true, 0, ""},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, _, exp, err := s.tokenRules(tt.in, "admin", tt.agent, true)
			if tt.status == 0 {
				if err != nil || exp == nil {
					t.Fatalf("tokenRules = %v %v", exp, err)
				}
				return
			}
			var he *httpx.Error
			if !errors.As(err, &he) || he.Status != tt.status || !strings.Contains(he.Msg, tt.wantErr) {
				t.Fatalf("err = %v, want %d with %q", err, tt.status, tt.wantErr)
			}
		})
	}
}
