package netguard

import (
	"fmt"
	"net/netip"
	"strings"
)

// ParseCIDRs parses an operator list of CIDRs (T16 #35): a comma list, blanks
// ignored. It rejects a prefix of /0, a range that overlaps an always-refused
// range, a range shorter than /8 (IPv4) or /32 (IPv6), and a bad entry. The
// metadata addresses are not rejected here: they stay refused inside a valid
// range. The error names the entry and never anything else.
func ParseCIDRs(list string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, raw := range strings.Split(list, ",") {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("netguard: %q is not a CIDR", s)
		}
		p = p.Masked()
		if p.Bits() == 0 {
			return nil, fmt.Errorf("netguard: %q is a /0 range", s)
		}
		if p.Addr().Is4() && p.Bits() < 8 {
			return nil, fmt.Errorf("netguard: %q is shorter than /8", s)
		}
		if p.Addr().Is6() && p.Bits() < 32 {
			return nil, fmt.Errorf("netguard: %q is shorter than /32", s)
		}
		for _, r := range alwaysRefused {
			if p.Overlaps(r) {
				return nil, fmt.Errorf("netguard: %q overlaps the always-refused range %s", s, r)
			}
		}
		out = append(out, p)
	}
	return out, nil
}
