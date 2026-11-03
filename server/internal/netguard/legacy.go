package netguard

import (
	"net/netip"
	"strconv"
	"strings"
)

// LegacyIPv4 parses the old inet_aton spellings of an IPv4 address that
// netip.ParseAddr refuses and a system resolver may accept: "127.1",
// "2130706433", "0x7f.1", "0177.0.0.1". One to four parts, each decimal, 0x
// hex or 0 octal; the last part fills the remaining bytes. A plain dotted quad
// is not "legacy" (ok is false: ParseAddr reads it). Callers check the
// returned address against a Policy.
func LegacyIPv4(host string) (netip.Addr, bool) {
	h := strings.TrimSuffix(host, ".")
	if h == "" {
		return netip.Addr{}, false
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return netip.Addr{}, false
	}
	parts := strings.Split(h, ".")
	if len(parts) > 4 {
		return netip.Addr{}, false
	}
	vals := make([]uint64, len(parts))
	for i, p := range parts {
		if p == "" {
			return netip.Addr{}, false
		}
		base := 10
		switch {
		case len(p) > 2 && (p[:2] == "0x" || p[:2] == "0X"):
			base, p = 16, p[2:]
		case len(p) > 1 && p[0] == '0':
			base, p = 8, p[1:]
		}
		v, err := strconv.ParseUint(p, base, 32)
		if err != nil {
			return netip.Addr{}, false
		}
		vals[i] = v
	}
	var n uint64
	last := len(vals) - 1
	for i := 0; i < last; i++ {
		if vals[i] > 255 {
			return netip.Addr{}, false
		}
		n |= vals[i] << (8 * uint(3-i))
	}
	if vals[last] >= 1<<(8*uint(4-last)) {
		return netip.Addr{}, false
	}
	n |= vals[last]
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}), true
}

// localSuffixes are the name endings that mean a private network: .localhost,
// .internal and .local (mDNS), .lan, and .home.arpa (RFC 8375).
var localSuffixes = []string{".localhost", ".internal", ".local", ".lan", ".home.arpa"}

// IsLocalName reports a host name that is never a public relay: "localhost",
// a name that ends in one of localSuffixes, or a single label with no dot
// ("intranet"). Trailing dot and case are ignored. It is used only when an
// admin saves a push relay or a chat webhook; the git hosts do not use it.
func IsLocalName(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if h == "localhost" || !strings.Contains(h, ".") {
		return true
	}
	for _, suf := range localSuffixes {
		if strings.HasSuffix(h, suf) {
			return true
		}
	}
	return false
}
