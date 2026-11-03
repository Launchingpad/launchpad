package tracker

import (
	"strings"
	"testing"
)

func TestNormalizeGitlabBaseURL(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"https://gitlab.example.com", "https://gitlab.example.com", true},
		{"https://GitLab.Example.com/", "https://gitlab.example.com", true},
		{"HTTPS://gitlab.example.com:8443/gitlab/", "https://gitlab.example.com:8443/gitlab", true},
		{"http://localhost:8929", "http://localhost:8929", true},
		{"https://gitlab.example.com:443", "https://gitlab.example.com", true},
		{"HTTPS://GitLab.Example.com:443/", "https://gitlab.example.com", true},
		{"http://gitlab.example.com:80/gitlab/", "http://gitlab.example.com/gitlab", true},
		{"https://gitlab.example.com:80", "https://gitlab.example.com:80", true},
		{"http://gitlab.example.com:443", "http://gitlab.example.com:443", true},
		{"https://gitlab.example.com:", "https://gitlab.example.com", true},
		{"https://[2001:db8::1]:443/", "https://[2001:db8::1]", true},
		{"https://[2001:db8::1]:8443", "https://[2001:db8::1]:8443", true},
		{"  https://gitlab.example.com  ", "https://gitlab.example.com", true},
		{"ftp://gitlab.example.com", "", false},
		{"gitlab.example.com", "", false},
		{"https://user:pw@gitlab.example.com", "", false},
		{"https://gitlab.example.com/?x=1", "", false},
		{"https://gitlab.example.com/#frag", "", false},
		{"https://gitlab.example.com/\x00", "", false},
		{"", "", false},
		{"https://" + strings.Repeat("a", 2050) + ".com", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := normalizeGitlabBaseURL(tt.in)
			if (err == nil) != tt.ok || got != tt.want {
				t.Fatalf("normalizeGitlabBaseURL(%q) = %q, %v; want %q, ok=%v", tt.in, got, err, tt.want, tt.ok)
			}
		})
	}
}

func TestValidateGitlabSecret(t *testing.T) {
	good := strings.Repeat("aB3$", 8) // 32
	tests := []struct {
		name, secret string
		ok           bool
	}{
		{"32 printable", good, true},
		{"256", strings.Repeat("x", 256), true},
		{"31 is too short", good[:31], false},
		{"257 is too long", strings.Repeat("x", 257), false},
		{"a space", good[:16] + " " + good[17:], false},
		{"non-ASCII", good[:30] + "é", false},
		{"a newline", good + "\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateGitlabSecret(tt.secret)
			if (err == nil) != tt.ok {
				t.Fatalf("err = %v, want ok=%v", err, tt.ok)
			}
			if err != nil && strings.Contains(err.Error(), tt.secret) {
				t.Fatal("the error message echoes the secret")
			}
		})
	}
	if len(hashGitlabSecret(good)) != 32 {
		t.Fatal("the stored hash must be 32 bytes (git_integrations_hash_len)")
	}
}
