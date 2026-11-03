package setup

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestNewSecretShape(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		s, err := newSecret()
		if err != nil {
			t.Fatal(err)
		}
		if len(s) != 43 || !wellFormed(s) || strings.ContainsAny(s, "+/=#?&") {
			t.Fatalf("secret %q", s)
		}
		if len(hashSecret(s)) != 32 {
			t.Fatal("hash is not 32 bytes")
		}
		if seen[s] {
			t.Fatal("duplicate secret")
		}
		seen[s] = true
	}
	for _, bad := range []string{"", "short", strings.Repeat("A", 42), strings.Repeat("A", 44), strings.Repeat("!", 43)} {
		if wellFormed(bad) {
			t.Errorf("wellFormed(%q)", bad)
		}
	}
}

func TestLinkAndLogLine(t *testing.T) {
	tests := []struct{ name, base, want string }{
		{"base", "https://launchpad.example", "https://launchpad.example/setup#tok"},
		{"trailing slash", "https://launchpad.example/", "https://launchpad.example/setup#tok"},
		{"no base", "", "/setup#tok"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Link(tt.base, "tok"); got != tt.want {
				t.Fatalf("Link = %q, want %q", got, tt.want)
			}
			var buf bytes.Buffer
			LogLink(slog.New(slog.NewJSONHandler(&buf, nil)), tt.base, "tok")
			line := buf.String()
			if strings.Count(line, "\n") != 1 || !strings.Contains(line, `"msg":"`+LogPrefix+tt.want) {
				t.Fatalf("log line %q", line)
			}
			if !strings.Contains(line, "valid 24h") || (tt.base == "" && !strings.Contains(line, "PUBLIC_BASE_URL is unset")) {
				t.Fatalf("log line lacks the hint: %q", line)
			}
		})
	}
}
