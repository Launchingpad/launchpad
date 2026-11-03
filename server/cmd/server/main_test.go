package main

import (
	"bytes"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"devops/internal/config"
	"devops/internal/netguard"
)

func TestParseMintTokenArgs(t *testing.T) {
	base := []string{"--org", "orbit", "--user", "claude", "--name", "mcp", "--scopes", "read, write"}
	tests := []struct {
		name     string
		args     []string
		ok       bool
		wantDays *int
	}{
		{"no expiry flag", base, true, nil},
		{"expiry given", append(slices.Clone(base), "--expires-days", "90"), true, new(90)},
		{"expiry zero is kept, so the rules refuse it", append(slices.Clone(base), "--expires-days", "0"), true, new(0)},
		{"no args", nil, false, nil},
		{"no org", []string{"--user", "claude", "--name", "mcp", "--scopes", "read"}, false, nil},
		{"no user", []string{"--org", "orbit", "--name", "mcp", "--scopes", "read"}, false, nil},
		{"no name", []string{"--org", "orbit", "--user", "claude", "--scopes", "read"}, false, nil},
		{"no scopes", []string{"--org", "orbit", "--user", "claude", "--name", "mcp"}, false, nil},
		{"a stray argument", append(slices.Clone(base), "extra"), false, nil},
		{"an unknown flag", append(slices.Clone(base), "--secret", "x"), false, nil},
		{"days not a number", append(slices.Clone(base), "--expires-days", "ninety"), false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			a, ok := parseMintTokenArgs(tt.args, &stderr)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v (stderr %q)", ok, tt.ok, stderr.String())
			}
			if !ok {
				if !strings.Contains(stderr.String(), mintTokenUsage) {
					t.Fatalf("stderr lacks the usage line: %q", stderr.String())
				}
				return
			}
			if a.org != "orbit" || a.in.User != "claude" || a.in.Name != "mcp" || !slices.Equal(a.in.Scopes, []string{"read", "write"}) {
				t.Fatalf("parsed %+v", a)
			}
			if (a.in.ExpiresInDays == nil) != (tt.wantDays == nil) ||
				(tt.wantDays != nil && *a.in.ExpiresInDays != *tt.wantDays) {
				t.Fatalf("ExpiresInDays %v, want %v", a.in.ExpiresInDays, tt.wantDays)
			}
		})
	}
}

// TestMintTokenCmd_UsageExitCode: bad usage is exit 2 and touches no database.
func TestMintTokenCmd_UsageExitCode(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if rc := mintTokenCmd([]string{"--org", "orbit"}, &stdout, &stderr); rc != 2 {
		t.Fatalf("exit %d, want 2", rc)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout %q on a usage error", stdout.String())
	}
}

// The git policy the server boots with (T17 #31, LAUN-7): the app's own
// subnets are refused even with the private switch on, and until the first
// read of them succeeds every private address is refused.
func TestGitHostAccess_AttachedSubnets(t *testing.T) {
	ps := []netip.Prefix{netip.MustParsePrefix("172.18.0.0/16")}
	gws := []netip.Addr{netip.MustParseAddr("172.18.0.1")}
	var readErr error
	att := netguard.NewAttached(
		func() ([]netip.Prefix, error) { return ps, readErr },
		func() ([]netip.Addr, error) { return gws, nil })
	n, err := gitHostAccess(config.Config{LogEnv: "production", GitAllowPrivateProviders: true}, att)
	if err != nil {
		t.Fatal(err)
	}
	if n.Local || n.NewClient == nil {
		t.Fatalf("production GitNet = %+v", n)
	}
	lan, own, gw, meta := netip.MustParseAddr("10.1.2.3"), netip.MustParseAddr("172.18.0.5"), netip.MustParseAddr("172.18.0.1"), netip.MustParseAddr("169.254.169.254")
	if n.Policy.Check(lan) == nil {
		t.Fatal("before the first read of the attached subnets the guard must fail closed")
	}
	if err := att.Refresh(); err != nil {
		t.Fatal(err)
	}
	if err := n.Policy.Check(lan); err != nil {
		t.Fatalf("a LAN host with the switch on: %v", err)
	}
	for name, a := range map[string]netip.Addr{"own subnet": own, "gateway": gw, "metadata": meta} {
		if n.Policy.Check(a) == nil {
			t.Fatalf("%s is allowed", name)
		}
	}
	// A failed re-read closes the guard again.
	readErr = errors.New("boom")
	if att.Refresh() == nil {
		t.Fatal("Refresh did not report the failure")
	}
	if n.Policy.Check(lan) == nil {
		t.Fatal("after a failed read the guard stays open")
	}
	if _, err := gitHostAccess(config.Config{GitAllowPrivateCIDRs: "0.0.0.0/0"}, att); err == nil {
		t.Fatal("a /0 allow-list was accepted")
	}
	if n, _ := gitHostAccess(config.Config{LogEnv: "local"}, att); !n.Local {
		t.Fatal("a local server is not Local")
	}
}
