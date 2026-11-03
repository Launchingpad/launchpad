// Package netguard is the one dial-time SSRF guard of the server (T17 #31,
// T16 #34 and #35). Callers that dial a host an admin typed (the git API
// clients, the backup destinations) build a Policy, wrap it in a Dialer and
// use NewClient. The check runs on the RESOLVED address at dial time, so a
// DNS answer that changes between save and use (rebinding) gains nothing.
//
// The package holds the mechanism and the always-refused lists. A caller
// chooses only its private-range policy: GitPolicy here, the backup policy in
// internal/backup. Nothing in a Policy can open an always-refused range or a
// metadata address.
package netguard

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// ErrRefused is wrapped by every error the guard raises for an address or a
// name it refuses.
var ErrRefused = errors.New("netguard: refused")

// RefusedError says what was refused and why. It never holds a credential.
type RefusedError struct {
	Target string // an address or a name
	Reason string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("netguard: %s refused (%s)", e.Target, e.Reason)
}

// Is makes errors.Is(err, ErrRefused) true.
func (e *RefusedError) Is(target error) bool { return target == ErrRefused }

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// alwaysRefused is T16 #34's list. No setting opens it.
var alwaysRefused = mustPrefixes(
	"0.0.0.0/8", "127.0.0.0/8", "::1/128", "::/128",
	"169.254.0.0/16", "fe80::/10", "fec0::/10",
	"192.0.0.0/24", "192.88.99.0/24", "198.18.0.0/15", "240.0.0.0/4",
	"192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24", "2001:db8::/32",
	"100::/64", "64:ff9b:1::/48",
	"224.0.0.0/4", "ff00::/8",
	"255.255.255.255/32",
)

// metadataAddrs are refused even inside an operator-listed range.
var metadataAddrs = []netip.Addr{
	netip.MustParseAddr("169.254.169.254"),
	netip.MustParseAddr("169.254.170.2"),
	netip.MustParseAddr("100.100.100.200"),
	netip.MustParseAddr("fd00:ec2::254"),
	netip.MustParseAddr("168.63.129.16"),
}

// StandardPrivate is the set of private ranges a policy may open: the five
// ranges of T16 #34 and T17 #31.
var StandardPrivate = mustPrefixes("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7", "100.64.0.0/10")

// Dynamic gives a policy ranges that can change while the process runs (the
// app's own subnets). Denied is read at every dial.
type Dynamic interface {
	// Denied returns ranges to refuse, and whether EVERY private address
	// must be refused (the read of the interface addresses failed: fail
	// closed).
	Denied() (prefixes []netip.Prefix, denyAllPrivate bool)
}

// Policy is a caller's choice on top of the always-refused lists. The zero
// Policy allows public addresses only.
type Policy struct {
	// Private are the private ranges the operator opened. An address inside
	// StandardPrivate is allowed only if one of these contains it.
	Private []netip.Prefix
	// Dynamic ranges are refused after the always-refused lists and before
	// Private, so an operator range cannot open them.
	Dynamic Dynamic
	// Gateways returns default-gateway addresses. Each is refused unless an
	// entry of Private is exactly that address (/32 or /128): T16 #34's
	// exception, for the backup policy only. The git policy has none and
	// refuses the gateway through Dynamic.
	Gateways func() []netip.Addr
	// Names are host names refused before resolution, as a second defence.
	Names []string
	// TestPermit is the TEST SEAM (T17 #31): addresses it contains pass every
	// check. Only netguardtest sets it. Production code has no flag, variable
	// or build tag that does this; TestNoProductionBypass checks it.
	TestPermit []netip.Prefix
}

func inAny(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// embedded returns the IPv4 addresses an IPv6 address carries, and ok=false
// when the address is not of an embedding form. IPv4-mapped, IPv4-compatible,
// NAT64, 6to4 and Teredo are unwrapped (T16 #34).
func embedded(a netip.Addr) (v4s []netip.Addr, ok bool) {
	if a.Is4() {
		return nil, false
	}
	if a.Is4In6() {
		return []netip.Addr{a.Unmap()}, true
	}
	b := a.As16()
	zero12 := true
	for _, x := range b[:12] {
		if x != 0 {
			zero12 = false
			break
		}
	}
	switch {
	case zero12: // ::/96, IPv4-compatible
		return []netip.Addr{netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})}, true
	case b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b && allZero(b[4:12]): // 64:ff9b::/96
		return []netip.Addr{netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})}, true
	case b[0] == 0x20 && b[1] == 0x02: // 2002::/16, 6to4
		return []netip.Addr{netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]})}, true
	case b[0] == 0x20 && b[1] == 0x01 && b[2] == 0 && b[3] == 0: // 2001::/32, Teredo
		server := netip.AddrFrom4([4]byte{b[4], b[5], b[6], b[7]})
		client := netip.AddrFrom4([4]byte{^b[12], ^b[13], ^b[14], ^b[15]})
		return []netip.Addr{server, client}, true
	}
	return nil, false
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

func refuse(a netip.Addr, why string) error {
	return &RefusedError{Target: a.String(), Reason: why}
}

// Check returns nil when the policy lets the address through, else a
// *RefusedError.
func (p Policy) Check(a netip.Addr) error {
	a = a.WithZone("")
	if !a.IsValid() {
		return &RefusedError{Target: "invalid address", Reason: "not an address"}
	}
	if inAny(p.TestPermit, a) {
		return nil
	}
	if err := p.checkOne(a); err != nil {
		return err
	}
	if v4s, ok := embedded(a); ok {
		for _, v := range v4s {
			if err := p.checkOne(v); err != nil {
				return refuse(a, "embeds "+v.String()+": "+err.(*RefusedError).Reason)
			}
		}
	}
	return nil
}

func (p Policy) checkOne(a netip.Addr) error {
	if inAny(alwaysRefused, a) {
		return refuse(a, "always-refused range")
	}
	for _, m := range metadataAddrs {
		if a == m {
			return refuse(a, "metadata address")
		}
	}
	private := inAny(StandardPrivate, a)
	if p.Dynamic != nil {
		denied, all := p.Dynamic.Denied()
		if inAny(denied, a) {
			return refuse(a, "the app's own network")
		}
		if all && (private || inAny(p.Private, a)) {
			return refuse(a, "private range, interface addresses unreadable")
		}
	}
	if p.Gateways != nil {
		for _, g := range p.Gateways() {
			if g == a && !hasExact(p.Private, a) {
				return refuse(a, "default gateway")
			}
		}
	}
	if private && !inAny(p.Private, a) {
		return refuse(a, "private range not enabled")
	}
	return nil
}

func hasExact(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Addr() == a && p.Bits() == a.BitLen() {
			return true
		}
	}
	return false
}

// RefusesName reports whether a host name is on the policy's name list
// (compared lower-case, without a trailing dot).
func (p Policy) RefusesName(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	for _, n := range p.Names {
		if h == n {
			return true
		}
	}
	return false
}
