package api

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

// TestClientKeyFor is the table of decisions-T12 #9.
func TestClientKeyFor(t *testing.T) {
	edge := []netip.Prefix{netip.MustParsePrefix("172.18.0.0/16"), netip.MustParsePrefix("192.0.2.10/32")}
	cases := []struct {
		name    string
		peer    string
		proxies []netip.Prefix
		headers map[string]string
		want    string
	}{
		{"no proxies, direct", "198.51.100.1:5000", nil, nil, "198.51.100.1"},
		{"no proxies, through a proxy: no key", "172.18.0.3:5000", nil, map[string]string{"X-Forwarded-For": "203.0.113.5"}, ""},
		{"no proxies, X-Real-IP: no key", "172.18.0.3:5000", nil, map[string]string{"X-Real-IP": "203.0.113.5"}, ""},
		{"no proxies, Forwarded: no key", "172.18.0.3:5000", nil, map[string]string{"Forwarded": "for=203.0.113.5"}, ""},
		{"untrusted peer, forged headers ignored", "198.51.100.1:5000", edge, map[string]string{"X-Forwarded-For": "203.0.113.5", "X-Real-IP": "203.0.113.6"}, "198.51.100.1"},
		{"trusted peer, one hop", "172.18.0.3:5000", edge, map[string]string{"X-Forwarded-For": "203.0.113.5"}, "203.0.113.5"},
		{"trusted peer, outer proxy skipped", "172.18.0.3:5000", edge, map[string]string{"X-Forwarded-For": "203.0.113.5, 192.0.2.10"}, "203.0.113.5"},
		{"trusted peer, spoofed left part", "172.18.0.3:5000", edge, map[string]string{"X-Forwarded-For": "1.2.3.4, 203.0.113.5, 192.0.2.10"}, "203.0.113.5"},
		{"trusted peer, outer proxy NOT listed: the outer proxy is the key", "172.18.0.3:5000", edge[:1], map[string]string{"X-Forwarded-For": "203.0.113.5, 192.0.2.10"}, "192.0.2.10"},
		{"trusted peer, X-Real-IP only", "172.18.0.3:5000", edge, map[string]string{"X-Real-IP": "203.0.113.7"}, "203.0.113.7"},
		{"trusted peer, no header: no key", "172.18.0.3:5000", edge, nil, ""},
		{"trusted peer, only trusted hops: no key", "172.18.0.3:5000", edge, map[string]string{"X-Forwarded-For": "192.0.2.10"}, ""},
		{"trusted peer, junk hop: no key", "172.18.0.3:5000", edge, map[string]string{"X-Forwarded-For": "not-an-ip"}, ""},
		{"IPv6 peer direct: its /64", "[2001:db8::1]:5000", nil, nil, "2001:db8::/64"},
		{"IPv6 hop: its /64", "172.18.0.3:5000", edge, map[string]string{"X-Forwarded-For": "2001:db8:0:0:abcd::7"}, "2001:db8::/64"},
		{"IPv4-mapped hop", "172.18.0.3:5000", edge, map[string]string{"X-Forwarded-For": "::ffff:203.0.113.5"}, "203.0.113.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/", nil)
			r.RemoteAddr = c.peer
			for k, v := range c.headers {
				r.Header.Set(k, v)
			}
			if got := clientKeyFor(r, c.proxies); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}
