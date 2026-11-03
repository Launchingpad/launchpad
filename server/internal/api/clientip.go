package api

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// The client address of the per-client sign-up limits
// (docs/internal/decisions-T12-signups-invites.md #9). It is a limiter key only:
// never logged, never stored (the limiter keeps an HMAC of it).
//
// The rules, in order:
//
//   - No trusted proxy is configured (TRUSTED_PROXY_CIDRS empty): a request
//     that carries no forwarding header is a direct connection and is keyed
//     on its peer. A request WITH a forwarding header came through a proxy
//     the server cannot judge: one address may stand for every caller, so
//     it gets NO per-client key (the global bucket, the per-email limit and
//     the token entropy still hold). This is the default of the production
//     stack, so ~20 strangers cannot lock out everyone.
//   - Trusted proxies are configured, and the peer is NOT one of them: the
//     peer is the key, and every forwarding header is ignored (a client
//     cannot choose its own key by forging X-Real-IP).
//   - The peer IS a trusted proxy: X-Forwarded-For is read from the right;
//     trusted hops are skipped, and the first address that is not trusted
//     is the client. With no X-Forwarded-For, X-Real-IP is that one hop. A
//     malformed hop, or a chain of trusted hops only, gives no key.

// forwardingHeaders are the headers a proxy adds.
var forwardingHeaders = []string{"X-Forwarded-For", "X-Real-IP", "Forwarded"}

func peerAddr(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

// keyOf is the limiter key of one address: the address itself for IPv4,
// its /64 for IPv6 (one host usually holds a whole /64, so a key per
// address would give it 2^64 keys; decisions-T12 #9).
func keyOf(a netip.Addr) string {
	if a.Is6() {
		return netip.PrefixFrom(a, 64).Masked().String()
	}
	return a.String()
}

func trusted(a netip.Addr, proxies []netip.Prefix) bool {
	for _, p := range proxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// clientKeyFor returns the per-client limiter key of r, or "" when the
// caller's own address cannot be known (see the rules above).
func clientKeyFor(r *http.Request, proxies []netip.Prefix) string {
	peer, ok := peerAddr(r)
	if !ok {
		return ""
	}
	if len(proxies) == 0 {
		for _, h := range forwardingHeaders {
			if r.Header.Get(h) != "" {
				return ""
			}
		}
		return keyOf(peer)
	}
	if !trusted(peer, proxies) {
		return keyOf(peer)
	}
	var hops []string
	for _, v := range r.Header.Values("X-Forwarded-For") {
		for h := range strings.SplitSeq(v, ",") {
			hops = append(hops, strings.TrimSpace(h))
		}
	}
	if len(hops) == 0 {
		if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
			hops = []string{v}
		}
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(hops[i])
		if err != nil {
			return ""
		}
		a = a.Unmap()
		if !trusted(a, proxies) {
			return keyOf(a)
		}
	}
	return ""
}

// clientKey is clientKeyFor with this server's trusted proxies.
func (s *Server) clientKey(r *http.Request) string { return clientKeyFor(r, s.TrustedProxies) }
