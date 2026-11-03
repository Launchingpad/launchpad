package backup

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"devops/internal/backup/driver"
	"devops/internal/netguard"
	"devops/internal/netguard/netguardtest"
)

func gw(addrs ...string) Gateways {
	return Gateways{Read: func() ([]netip.Addr, error) {
		var out []netip.Addr
		for _, a := range addrs {
			out = append(out, netip.MustParseAddr(a))
		}
		return out, nil
	}}
}

func mustPolicy(t *testing.T, cidrs string, g Gateways) netguard.Policy {
	t.Helper()
	p, err := Policy(cidrs, g)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The SSRF table of T16 #34 and the CIDR parser tests run in internal/netguard
// (LAUN-6). These rows are the BACKUP policy: the operator list, the service
// names, the gateway exception.
func TestPolicyOperatorList(t *testing.T) {
	for _, tc := range []struct {
		cidrs string
		ok    bool
	}{
		{"", true}, {"192.168.1.0/24", true}, {"192.168.1.0/24, 10.0.0.0/8 ,,", true}, {"fd00::/32", true},
		{"0.0.0.0/0", false}, {"::/0", false}, {"0.0.0.0/4", false}, {"240.0.0.0/4", false}, {"224.0.0.0/4", false},
		{"128.0.0.0/1", false}, {"127.0.0.0/8", false}, {"127.0.0.1/32", false}, {"169.254.0.0/16", false},
		{"169.254.169.254/32", false}, {"::1/128", false}, {"fe80::/10", false}, {"10.0.0.0/7", false},
		{"nonsense", false}, {"10.0.0.1", false},
	} {
		_, err := Policy(tc.cidrs, gw())
		if (err == nil) != tc.ok {
			t.Errorf("Policy(%q) err=%v", tc.cidrs, err)
		}
		if err != nil && !strings.HasPrefix(err.Error(), "BACKUP_ALLOW_PRIVATE_CIDRS:") {
			t.Errorf("error does not name the variable: %v", err)
		}
	}
	env := map[string]string{"BACKUP_ALLOW_PRIVATE_CIDRS": "10.9.0.0/16"}
	p, err := PolicyFromEnv(func(k string) string { return env[k] }, gw())
	if err != nil || p.Check(netip.MustParseAddr("10.9.1.1")) != nil || p.Check(netip.MustParseAddr("10.8.1.1")) == nil {
		t.Errorf("PolicyFromEnv: %v", err)
	}
	env["BACKUP_ALLOW_PRIVATE_CIDRS"] = "0.0.0.0/0"
	if _, err := PolicyFromEnv(func(k string) string { return env[k] }, gw()); err == nil {
		t.Error("/0 accepted from the environment")
	}
}

func TestPolicyCheckRows(t *testing.T) {
	const gate = "172.17.0.1"
	for _, tc := range []struct {
		name  string
		cidrs string
		addr  string
		ok    bool
	}{
		{"public", "", "93.184.216.34", true},
		{"public v6", "", "2606:2800:220:1::1", true},
		{"private by default", "", "192.168.1.5", false},
		{"private 10/8 by default", "", "10.1.2.3", false},
		{"cgnat by default", "", "100.64.0.9", false},
		{"ula by default", "", "fd12::1", false},
		{"listed range opens", "192.168.1.0/24", "192.168.1.5", true},
		{"unlisted neighbour stays closed", "192.168.1.0/24", "192.168.2.5", false},
		{"loopback never", "10.0.0.0/8", "127.0.0.1", false},
		{"link-local never", "10.0.0.0/8", "169.254.1.1", false},
		{"aws metadata, range listed", "10.0.0.0/8", "169.254.169.254", false},
		{"ecs metadata", "10.0.0.0/8", "169.254.170.2", false},
		{"alibaba metadata inside listed 100.64/10", "100.64.0.0/10", "100.100.100.200", false},
		{"alibaba neighbour inside listed 100.64/10", "100.64.0.0/10", "100.100.100.201", true},
		{"aws v6 metadata inside listed fc00::/7", "fd00:ec2::/32", "fd00:ec2::254", false},
		{"azure wireserver, listed as /32", "168.63.129.16/32", "168.63.129.16", false},
		{"azure wireserver, listed /24", "168.63.129.0/24", "168.63.129.16", false},
		{"v4-mapped private, not listed", "", "::ffff:192.168.1.5", false},
		{"v4-mapped private, listed", "192.168.1.0/24", "::ffff:192.168.1.5", true},
		{"v4-mapped metadata", "10.0.0.0/8", "::ffff:169.254.169.254", false},
		{"nat64 of loopback", "", "64:ff9b::7f00:1", false},
		{"6to4 of private, not listed", "", "2002:c0a8:105::1", false},
		{"teredo", "", "2001:0:c0a8:105::1", false},
		{"gateway refused, range containing it listed", "172.17.0.0/16", gate, false},
		{"gateway allowed when listed as exact /32", "172.17.0.1/32", gate, true},
		{"other host in the gateway's range, listed", "172.17.0.0/16", "172.17.0.7", true},
		{"gateway unlisted", "", gate, false},
		{"gateway as v4-mapped", "172.17.0.0/16", "::ffff:" + gate, false},
		{"documentation range", "", "203.0.113.5", false},
		{"multicast", "", "224.0.0.1", false},
		{"broadcast", "", "255.255.255.255", false},
		{"unspecified", "", "0.0.0.0", false},
	} {
		p := mustPolicy(t, tc.cidrs, gw(gate))
		err := p.Check(netip.MustParseAddr(tc.addr))
		if (err == nil) != tc.ok {
			t.Errorf("%s: Check(%s) = %v", tc.name, tc.addr, err)
		}
		if err != nil && !errors.Is(err, netguard.ErrRefused) {
			t.Errorf("%s: error is not ErrRefused: %v", tc.name, err)
		}
	}
}

func TestGatewayReadFailureFailsClosed(t *testing.T) {
	boom := Gateways{Read: func() ([]netip.Addr, error) { return nil, errors.New("read failed") }}
	p := mustPolicy(t, "192.168.1.0/24", boom)
	if p.Check(netip.MustParseAddr("192.168.1.5")) == nil {
		t.Error("a listed private address passed with an unreadable route table")
	}
	if p.Check(netip.MustParseAddr("93.184.216.34")) != nil {
		t.Error("a public address was refused")
	}
	// Not Linux: no route file means no gateway is known; nothing is refused for it.
	none := Gateways{Read: func() ([]netip.Addr, error) { return nil, fs.ErrNotExist }}
	p = mustPolicy(t, "192.168.1.0/24", none)
	if p.Check(netip.MustParseAddr("192.168.1.5")) != nil {
		t.Error("missing route file refused a listed range")
	}
}

type countResolver struct {
	answers [][]string
	n       atomic.Int32
}

func (r *countResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	i := int(r.n.Add(1)) - 1
	if i >= len(r.answers) {
		i = len(r.answers) - 1
	}
	var out []netip.Addr
	for _, s := range r.answers[i] {
		out = append(out, netip.MustParseAddr(s))
	}
	return out, nil
}

func TestDialerRefusals(t *testing.T) {
	p := mustPolicy(t, "", gw("172.17.0.1"))
	res := &netguardtest.MapResolver{Hosts: map[string][]string{
		"loop.example":    {"127.0.0.1"},
		"private.example": {"192.168.1.5"},
		"mixed.example":   {"93.184.216.34", "10.0.0.5"},
		"postgres":        {"10.0.0.2"},
	}}
	d := NewDialer(p, time.Second)
	d.Resolver = res
	for _, host := range []string{
		"loop.example:443", "private.example:443", "mixed.example:443", // one refused answer refuses the host
		"postgres:5432", "app:80", "silo:9000", "backup:80", "Silo.:9000", "host.docker.internal:80", "gateway.docker.internal:80",
		"127.0.0.1:22", "[::1]:22", "169.254.169.254:80", "172.17.0.1:80", "[::ffff:127.0.0.1]:80",
	} {
		_, err := d.DialContext(context.Background(), "tcp", host)
		if !errors.Is(err, netguard.ErrRefused) {
			t.Errorf("%s: %v", host, err)
		}
		if got := driver.MapNetError(err).Code; got != driver.SSRFBlocked {
			t.Errorf("%s maps to %s", host, got)
		}
	}
	if _, err := d.DialContext(context.Background(), "udp", "93.184.216.34:53"); err == nil {
		t.Error("udp accepted")
	}
}

func TestDialerRebinding(t *testing.T) {
	// The first answer is public, the second private: each dial resolves once
	// and checks what it dials, so the second dial is refused.
	p := mustPolicy(t, "", gw())
	res := &countResolver{answers: [][]string{{"93.184.216.34"}, {"192.168.1.5"}}}
	d := NewDialer(p, 50*time.Millisecond)
	d.Resolver = res
	// First dial: the public address is allowed by the guard; the connect itself fails (no route
	// or timeout), which is not a refusal.
	_, err := d.DialContext(context.Background(), "tcp", "rebind.example:9")
	if errors.Is(err, netguard.ErrRefused) {
		t.Fatalf("first dial refused: %v", err)
	}
	if _, err := d.DialContext(context.Background(), "tcp", "rebind.example:9"); !errors.Is(err, netguard.ErrRefused) {
		t.Errorf("second dial not refused: %v", err)
	}
}

func TestHTTPClientNoRedirectNoProxy(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/go" {
			http.Redirect(w, r, "http://10.0.0.1/secret", http.StatusFound)
			return
		}
		w.WriteHeader(204)
	}))
	defer target.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(target.URL, "http://"))

	// A proxy that records any connection.
	var proxied atomic.Int32
	pl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pl.Close()
	go func() {
		for {
			c, err := pl.Accept()
			if err != nil {
				return
			}
			proxied.Add(1)
			c.Close()
		}
	}()
	t.Setenv("HTTP_PROXY", "http://"+pl.Addr().String())
	t.Setenv("http_proxy", "http://"+pl.Addr().String())
	t.Setenv("HTTPS_PROXY", "http://"+pl.Addr().String())
	t.Setenv("NO_PROXY", "")

	p := netguardtest.Permit(mustPolicy(t, "", gw()), "127.0.0.1")
	d := NewDialer(p, time.Second)
	d.Resolver = &netguardtest.MapResolver{Hosts: map[string][]string{"files.example.test": {"127.0.0.1"}}}
	c := NewHTTPClient(d, 5*time.Second)

	resp, err := c.Get("http://files.example.test:" + port + "/ok")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 || proxied.Load() != 0 {
		t.Errorf("status %d, proxy saw %d connections", resp.StatusCode, proxied.Load())
	}
	// A redirect to a private address is a 302 response, not followed.
	resp, err = c.Get("http://files.example.test:" + port + "/go")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 302 || hits.Load() != 2 {
		t.Errorf("status %d, hits %d", resp.StatusCode, hits.Load())
	}
	if e := driver.StatusError(resp.StatusCode); e.Code != driver.ServerError || e.Status != 302 || driver.Retryable(e) {
		t.Errorf("redirect maps to %v", e)
	}
	// The default policy (no permit) refuses the loopback server.
	d2 := NewDialer(mustPolicy(t, "", gw()), time.Second)
	d2.Resolver = d.Resolver
	if _, err := NewHTTPClient(d2, time.Second).Get("http://files.example.test:" + port + "/ok"); !errors.Is(err, netguard.ErrRefused) {
		t.Errorf("unpermitted loopback: %v", err)
	}
}

func TestOperatorListed(t *testing.T) {
	p := mustPolicy(t, "192.168.1.0/24", gw())
	if !OperatorListed(p, netip.MustParseAddr("192.168.1.9")) || !OperatorListed(p, netip.MustParseAddr("::ffff:192.168.1.9")) {
		t.Error("listed address not listed")
	}
	if OperatorListed(p, netip.MustParseAddr("93.184.216.34")) || OperatorListed(p, netip.MustParseAddr("192.168.2.9")) {
		t.Error("unlisted address listed")
	}
}
