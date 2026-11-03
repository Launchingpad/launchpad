package tracker

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"devops/internal/httpx"
)

func TestValidateUser(t *testing.T) {
	tests := []struct {
		name, username, display string
		wantUser, wantName      string
		wantErr                 string
	}{
		{"plain", "claude", "Claude", "claude", "Claude", ""},
		{"trimmed and lower-cased", "  Claude-Review_2 ", "  Claude  ", "claude-review_2", "Claude", ""},
		{"one letter", "c", "C", "c", "C", ""},
		{"39 characters", "a" + strings.Repeat("b", 38), "x", "a" + strings.Repeat("b", 38), "x", ""},
		{"40 characters", "a" + strings.Repeat("b", 39), "x", "", "", "username must be"},
		{"empty", "", "x", "", "", "username must be"},
		{"starts with a digit", "1bot", "x", "", "", "username must be"},
		{"starts with a dash", "-bot", "x", "", "", "username must be"},
		{"a dot", "a.b", "x", "", "", "username must be"},
		{"an at sign", "bot@example.com", "x", "", "", "username must be"},
		{"inner space", "a b", "x", "", "", "username must be"},
		{"name blank", "bot", "  ", "", "", "name must be 1-200"},
		{"name 200 runes", "bot", strings.Repeat("é", 200), "bot", strings.Repeat("é", 200), ""},
		{"name 201 runes", "bot", strings.Repeat("é", 201), "", "", "name must be 1-200"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, n, err := ValidateUser(tt.username, tt.display)
			if tt.wantErr == "" {
				if err != nil || u != tt.wantUser || n != tt.wantName {
					t.Fatalf("ValidateUser = %q, %q, %v; want %q, %q", u, n, err, tt.wantUser, tt.wantName)
				}
				return
			}
			var he *httpx.Error
			if !errors.As(err, &he) || he.Status != http.StatusBadRequest || he.Code != httpx.CodeValidationFailed ||
				!strings.HasPrefix(he.Msg, tt.wantErr) {
				t.Fatalf("err = %v, want a 400 starting %q", err, tt.wantErr)
			}
		})
	}
}
