// Package netguardtest is the test seam of internal/netguard (T17 #31):
// httptest servers bind 127.0.0.1, which the guard always refuses, so a test
// builds a policy that permits its own listener. Only _test.go files import
// this package; TestNoProductionBypass checks it.
package netguardtest

import (
	"context"
	"net/netip"
	"sync"

	"devops/internal/netguard"
)

// Permit returns p with the given addresses let through every check.
func Permit(p netguard.Policy, addrs ...string) netguard.Policy {
	for _, a := range addrs {
		ip := netip.MustParseAddr(a)
		p.TestPermit = append(p.TestPermit, netip.PrefixFrom(ip, ip.BitLen()))
	}
	return p
}

// Loopback returns a policy that permits 127.0.0.1 and nothing else beyond the
// zero policy (public addresses only).
func Loopback() netguard.Policy { return Permit(netguard.Policy{}, "127.0.0.1") }

// MapResolver answers from a fixed map of host to addresses and counts calls.
type MapResolver struct {
	Hosts map[string][]string
	mu    sync.Mutex
	calls int
}

// LookupNetIP implements netguard.Resolver.
func (r *MapResolver) LookupNetIP(_ context.Context, _ string, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	var out []netip.Addr
	for _, s := range r.Hosts[host] {
		out = append(out, netip.MustParseAddr(s))
	}
	if len(out) == 0 {
		return nil, &notFound{host}
	}
	return out, nil
}

type notFound struct{ host string }

func (e *notFound) Error() string { return "no such host in the test resolver" }

// Calls is how many lookups were made.
func (r *MapResolver) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}
