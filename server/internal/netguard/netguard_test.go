package netguard_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"devops/internal/netguard"
	"devops/internal/netguard/netguardtest"
)

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

// fakeAttached is an Attached on fixed readers.
func fakeAttached(t *testing.T, prefixes []string, gateways []string) *netguard.Attached {
	t.Helper()
	a := netguard.NewAttached(
		func() ([]netip.Prefix, error) {
			var out []netip.Prefix
			for _, p := range prefixes {
				out = append(out, netip.MustParsePrefix(p))
			}
			return out, nil
		},
		func() ([]netip.Addr, error) {
			var out []netip.Addr
			for _, g := range gateways {
				out = append(out, addr(g))
			}
			return out, nil
		})
	if err := a.Refresh(); err != nil {
		t.Fatal(err)
	}
	return a
}

var alwaysRefusedSamples = []string{
	// the T16 #34 list, one per range
	"0.0.0.1", "127.0.0.1", "127.255.255.254", "::1", "::", "169.254.1.1", "fe80::1", "fec0::1",
	"192.0.0.9", "192.88.99.1", "198.18.0.1", "198.19.255.255", "240.0.0.1", "255.255.255.255",
	"192.0.2.1", "198.51.100.1", "203.0.113.1", "2001:db8::1", "100::1", "64:ff9b:1::1",
	"224.0.0.1", "239.255.255.250", "ff02::1", "ff00::",
	// the metadata addresses
	"169.254.169.254", "169.254.170.2", "100.100.100.200", "fd00:ec2::254", "168.63.129.16",
	// embedded IPv4: mapped, compatible, NAT64, 6to4, Teredo (server and client)
	"::ffff:127.0.0.1", "::ffff:169.254.169.254", "::127.0.0.1", "64:ff9b::7f00:1", "2002:7f00:1::",
	"2002:a9fe:a9fe::1", "2001:0:7f00:1::1", "2001:0:808:808:0:0:80ff:fffe",
}

// Every always-refused row refuses under the zero policy, under the git policy
// with the switch on, and with a private-range operator list.
func TestAlwaysRefused(t *testing.T) {
	att := fakeAttached(t, []string{"172.20.0.7/16"}, []string{"172.20.0.1"})
	all, err := netguard.GitPolicy(true, "10.0.0.0/8,100.64.0.0/10", att)
	if err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]netguard.Policy{
		"zero": {}, "git switch": all, "private list": {Private: netguard.StandardPrivate},
	} {
		for _, s := range alwaysRefusedSamples {
			err := p.Check(addr(s))
			if !errors.Is(err, netguard.ErrRefused) {
				t.Errorf("%s: %s allowed (err=%v)", name, s, err)
			}
		}
	}
}

func TestPublicAddressesPass(t *testing.T) {
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "2606:4700::1111", "::ffff:8.8.8.8", "64:ff9b::808:808", "2002:808:808::1"} {
		if err := (netguard.Policy{}).Check(addr(s)); err != nil {
			t.Errorf("%s refused: %v", s, err)
		}
	}
}

func TestPrivateAddressesRefusedByDefault(t *testing.T) {
	for _, s := range []string{"10.0.0.1", "172.16.0.1", "172.31.255.255", "192.168.1.1", "fc00::1", "fd12::1", "100.64.0.1", "::ffff:10.0.0.1", "64:ff9b::a00:1"} {
		if err := (netguard.Policy{}).Check(addr(s)); !errors.Is(err, netguard.ErrRefused) {
			t.Errorf("%s allowed under the zero policy", s)
		}
	}
}

// The one rule of GIT_ALLOW_PRIVATE_*: four combinations.
func TestGitSwitchesFourCombinations(t *testing.T) {
	att := fakeAttached(t, []string{"172.20.0.7/16"}, nil)
	std := []string{"10.1.2.3", "172.31.0.1", "192.168.5.5", "100.64.0.9", "fc00::1", "fd12::7"}
	cases := []struct {
		name      string
		providers bool
		cidrs     string
		allowed   map[string]bool
	}{
		{"unset and unset", false, "", map[string]bool{}},
		{"switch only", true, "", map[string]bool{"10.1.2.3": true, "172.31.0.1": true, "192.168.5.5": true, "100.64.0.9": true, "fc00::1": true, "fd12::7": true}},
		{"list only", false, "192.168.0.0/16, fd12::/32", map[string]bool{"192.168.5.5": true, "fd12::7": true}},
		{"switch and list (union)", true, "192.168.0.0/16", map[string]bool{"10.1.2.3": true, "172.31.0.1": true, "192.168.5.5": true, "100.64.0.9": true, "fc00::1": true, "fd12::7": true}},
	}
	for _, c := range cases {
		p, err := netguard.GitPolicy(c.providers, c.cidrs, att)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		for _, s := range std {
			err := p.Check(addr(s))
			if c.allowed[s] && err != nil {
				t.Errorf("%s: %s refused: %v", c.name, s, err)
			}
			if !c.allowed[s] && err == nil {
				t.Errorf("%s: %s allowed", c.name, s)
			}
		}
		if err := p.Check(addr("8.8.8.8")); err != nil {
			t.Errorf("%s: public refused: %v", c.name, err)
		}
	}
}

func TestSwitchParsing(t *testing.T) {
	for in, want := range map[string]bool{"1": true, "true": true, "TRUE": true, "": false, "0": false, "false": false} {
		got, err := netguard.ParseSwitch("X", in)
		if err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	if _, err := netguard.ParseSwitch("X", "yes"); err == nil {
		t.Error("yes accepted")
	}
	env := map[string]string{"GIT_ALLOW_PRIVATE_PROVIDERS": "maybe"}
	if _, err := netguard.GitPolicyFromEnv(func(k string) string { return env[k] }, nil); err == nil {
		t.Error("bad switch accepted")
	}
	env = map[string]string{"GIT_ALLOW_PRIVATE_CIDRS": "0.0.0.0/0"}
	if _, err := netguard.GitPolicyFromEnv(func(k string) string { return env[k] }, nil); err == nil {
		t.Error("/0 accepted")
	}
	env = map[string]string{"GIT_ALLOW_PRIVATE_PROVIDERS": "1", "GIT_ALLOW_PRIVATE_CIDRS": "192.168.4.0/24"}
	p, err := netguard.GitPolicyFromEnv(func(k string) string { return env[k] }, nil)
	if err != nil || p.Check(addr("10.0.0.1")) != nil {
		t.Errorf("env policy: %v", err)
	}
}

// fc00::/7 is built in for the switch and so is not run through the parser,
// whose rule would reject it (T16 #35).
func TestParseCIDRs(t *testing.T) {
	for _, bad := range []string{
		"0.0.0.0/0", "::/0", "127.0.0.0/8", "127.0.0.1/32", "10.0.0.0/7", "fc00::/7", "169.254.0.0/16", "169.254.169.254/32",
		"0.0.0.0/8", "224.0.0.0/4", "240.0.0.0/4", "2001:db8::/32", "fe80::/10", "198.18.0.0/15", "nonsense", "10.0.0.0/33",
		"8.0.0.0/5",
	} {
		if _, err := netguard.ParseCIDRs(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	got, err := netguard.ParseCIDRs(" 10.0.0.0/8 , ,192.168.1.5/32,fd00::/32,")
	if err != nil || len(got) != 3 {
		t.Fatalf("%v %v", got, err)
	}
	if got, err := netguard.ParseCIDRs(""); err != nil || len(got) != 0 {
		t.Errorf("empty: %v %v", got, err)
	}
}

// Metadata addresses stay refused inside an operator range.
func TestMetadataInsideOperatorRange(t *testing.T) {
	p, err := netguard.GitPolicy(false, "100.64.0.0/10,fd00::/32", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Check(addr("100.64.1.1")) != nil {
		t.Fatal("control: the range does not open")
	}
	for _, s := range []string{"100.100.100.200", "fd00:ec2::254"} {
		if err := p.Check(addr(s)); !errors.Is(err, netguard.ErrRefused) {
			t.Errorf("%s allowed inside the range", s)
		}
	}
}

// The app's own network: refused with the switch on, and with a listed CIDR
// that contains it. The gateway is refused even when listed as a /32.
func TestGitRefusesAppNetwork(t *testing.T) {
	att := fakeAttached(t, []string{"172.20.0.7/16", "172.21.0.3/24", "fd00:dead::5/64"}, []string{"172.20.0.1"})
	own := []string{"172.20.0.9", "172.20.255.254", "172.21.0.200", "172.20.0.1", "fd00:dead::99", "172.20.0.7"}
	for name, p := range map[string]struct {
		providers bool
		cidrs     string
	}{
		"switch":        {true, ""},
		"listed range":  {false, "172.16.0.0/12,fd00::/32"},
		"exact gateway": {false, "172.20.0.1/32,172.16.0.0/12"},
		"both":          {true, "172.20.0.0/16,fd00:dead::/32"},
	} {
		pol, err := netguard.GitPolicy(p.providers, p.cidrs, att)
		if err != nil {
			t.Fatal(name, err)
		}
		for _, s := range own {
			if err := pol.Check(addr(s)); !errors.Is(err, netguard.ErrRefused) {
				t.Errorf("%s: %s allowed", name, s)
			}
		}
		// a private address OUTSIDE the app's subnets still opens
		if err := pol.Check(addr("172.22.0.9")); (p.providers || strings.Contains(p.cidrs, "172.16.0.0/12")) && err != nil {
			t.Errorf("%s: outside address refused: %v", name, err)
		}
	}
}

// A gateway outside every attached subnet is still refused.
func TestGitRefusesGatewayOutsideSubnets(t *testing.T) {
	att := fakeAttached(t, []string{"172.20.0.7/16"}, []string{"10.9.9.1"})
	p, _ := netguard.GitPolicy(true, "10.9.9.1/32", att)
	if err := p.Check(addr("10.9.9.1")); !errors.Is(err, netguard.ErrRefused) {
		t.Error("gateway allowed")
	}
}

func TestAttachedFailsClosed(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	empty := atomic.Bool{}
	a := netguard.NewAttached(func() ([]netip.Prefix, error) {
		if fail.Load() {
			return nil, errors.New("boom")
		}
		if empty.Load() {
			return nil, nil
		}
		return []netip.Prefix{netip.MustParsePrefix("172.20.0.7/16")}, nil
	}, func() ([]netip.Addr, error) { return nil, errors.New("no route file") })
	p, err := netguard.GitPolicy(true, "", a)
	if err != nil {
		t.Fatal(err)
	}
	// Before any refresh, and after a failed one: every private address refused.
	for _, stage := range []string{"before refresh", "failed refresh"} {
		if stage == "failed refresh" {
			if err := a.Refresh(); err == nil {
				t.Fatal("refresh should fail")
			}
		}
		for _, s := range []string{"10.1.1.1", "192.168.1.1", "172.22.0.1", "fc00::1", "100.64.0.1"} {
			if err := p.Check(addr(s)); !errors.Is(err, netguard.ErrRefused) {
				t.Errorf("%s: %s allowed", stage, s)
			}
		}
		if err := p.Check(addr("8.8.8.8")); err != nil {
			t.Errorf("%s: public refused", stage)
		}
	}
	fail.Store(false)
	if err := a.Refresh(); err != nil { // a missing gateway file does not close it
		t.Fatal(err)
	}
	if err := p.Check(addr("10.1.1.1")); err != nil {
		t.Errorf("after a good read: %v", err)
	}
	if err := p.Check(addr("172.20.3.3")); err == nil {
		t.Error("own subnet allowed after a good read")
	}
	// A later failure closes it again, and so does an empty answer.
	fail.Store(true)
	_ = a.Refresh()
	if err := p.Check(addr("10.1.1.1")); err == nil {
		t.Error("allowed after a later failed read")
	}
	fail.Store(false)
	empty.Store(true)
	if err := a.Refresh(); err == nil {
		t.Error("an empty read must fail")
	}
	if err := p.Check(addr("10.1.1.1")); err == nil {
		t.Error("allowed after an empty read")
	}
}

// With the interface read failed, an operator-listed PUBLIC range is refused
// too (everything in Private), not only the standard private ranges.
func TestFailClosedDeniesListedRanges(t *testing.T) {
	a := netguard.NewAttached(func() ([]netip.Prefix, error) { return nil, errors.New("boom") }, func() ([]netip.Addr, error) { return nil, nil })
	_ = a.Refresh()
	p, err := netguard.GitPolicy(false, "11.0.0.0/8", a)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Check(addr("11.2.3.4")); !errors.Is(err, netguard.ErrRefused) {
		t.Error("listed range allowed while closed")
	}
	if err := p.Check(addr("8.8.8.8")); err != nil {
		t.Error("public refused")
	}
}

func TestReadDefaultGateways(t *testing.T) {
	route := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t00000000\t0100A8C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
		"eth0\t0000A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" +
		"eth1\t000014AC\t00000000\t0001\t0\t0\t0\t0000FFFF\t0\t0\t0\n"
	got, err := netguard.ReadDefaultGateways(strings.NewReader(route))
	if err != nil || len(got) != 1 || got[0] != addr("192.168.0.1") {
		t.Fatalf("%v %v", got, err)
	}
}

// The backup-style gateway rule: refused unless listed as an exact /32.
func TestGatewayExactException(t *testing.T) {
	gw := func() []netip.Addr { return []netip.Addr{addr("172.17.0.1")} }
	p := netguard.Policy{Private: netguard.StandardPrivate, Gateways: gw}
	if err := p.Check(addr("172.17.0.1")); !errors.Is(err, netguard.ErrRefused) {
		t.Error("gateway allowed inside a range that merely contains it")
	}
	p.Private = []netip.Prefix{netip.MustParsePrefix("172.17.0.1/32")}
	if err := p.Check(addr("172.17.0.1")); err != nil {
		t.Errorf("exact /32 refused: %v", err)
	}
	if err := p.Check(addr("172.17.0.2")); err == nil {
		t.Error("neighbour of an exact /32 allowed")
	}
}

type countResolver struct {
	mu    sync.Mutex
	n     int
	reply func(n int) []string
}

func (r *countResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	r.n++
	n := r.n
	r.mu.Unlock()
	var out []netip.Addr
	for _, s := range r.reply(n) {
		out = append(out, addr(s))
	}
	return out, nil
}

func (r *countResolver) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

func TestServiceNamesRefusedBeforeResolution(t *testing.T) {
	p, _ := netguard.GitPolicy(true, "", fakeAttached(t, []string{"172.20.0.7/16"}, nil))
	res := &countResolver{reply: func(int) []string { return []string{"8.8.8.8"} }}
	d := &netguard.Dialer{Policy: p, Resolver: res}
	for _, name := range []string{"postgres", "app", "silo", "backup", "web", "host.docker.internal", "gateway.docker.internal", "SILO", "Postgres.", "silo"} {
		_, err := d.DialContext(context.Background(), "tcp", name+":9000")
		if !errors.Is(err, netguard.ErrRefused) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if res.calls() != 0 {
		t.Errorf("the resolver was asked %d times", res.calls())
	}
}

func listen(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	return srv, port
}

func TestDialerResolvesOnceAndRefusesRebinding(t *testing.T) {
	_, port := listen(t)
	pol := netguardtest.Loopback()

	// Public, then private: the first answer is used and checked; the second is never asked for.
	res := &countResolver{reply: func(n int) []string {
		if n == 1 {
			return []string{"127.0.0.1"}
		}
		return []string{"10.0.0.5"}
	}}
	d := &netguard.Dialer{Policy: pol, Resolver: res}
	c, err := d.DialContext(context.Background(), "tcp", "rebind.test:"+port)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if res.calls() != 1 {
		t.Errorf("resolved %d times", res.calls())
	}

	// One answer refused refuses the host, and nothing is dialed.
	res2 := &countResolver{reply: func(int) []string { return []string{"127.0.0.1", "10.0.0.5"} }}
	d2 := &netguard.Dialer{Policy: pol, Resolver: res2}
	if _, err := d2.DialContext(context.Background(), "tcp", "mixed.test:"+port); !errors.Is(err, netguard.ErrRefused) {
		t.Errorf("mixed answer: %v", err)
	}
	// An answer that is private from the start.
	res3 := &countResolver{reply: func(int) []string { return []string{"192.168.1.9"} }}
	d3 := &netguard.Dialer{Policy: pol, Resolver: res3}
	if _, err := d3.DialContext(context.Background(), "tcp", "lan.test:80"); !errors.Is(err, netguard.ErrRefused) {
		t.Errorf("private answer: %v", err)
	}
	// Literal addresses are checked too, and loopback is refused without the seam.
	d4 := &netguard.Dialer{}
	for _, a := range []string{"127.0.0.1:" + port, "[::1]:" + port, "10.0.0.1:80", "169.254.169.254:80", "[::ffff:127.0.0.1]:" + port} {
		if _, err := d4.DialContext(context.Background(), "tcp", a); !errors.Is(err, netguard.ErrRefused) {
			t.Errorf("%s: %v", a, err)
		}
	}
	if _, err := d4.DialContext(context.Background(), "udp", "8.8.8.8:53"); err == nil {
		t.Error("udp allowed")
	}
}

func TestClientIgnoresProxyEnvironment(t *testing.T) {
	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxyHits.Add(1) }))
	defer proxy.Close()
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY"} {
		t.Setenv(k, proxy.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	srv, port := listen(t)
	_ = srv
	res := &netguardtest.MapResolver{Hosts: map[string][]string{"git.test": {"127.0.0.1"}}}
	c := netguard.NewClient(&netguard.Dialer{Policy: netguardtest.Loopback(), Resolver: res}, 5*time.Second)
	resp, err := c.Get("http://git.test:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if proxyHits.Load() != 0 {
		t.Error("the request went through the proxy")
	}
	// And a proxy-only world: with the target refused, nothing leaves via the proxy either.
	c2 := netguard.NewClient(&netguard.Dialer{Resolver: res}, 5*time.Second)
	if _, err := c2.Get("http://git.test:" + port + "/"); err == nil {
		t.Error("loopback host allowed without the seam")
	}
	if proxyHits.Load() != 0 {
		t.Error("the refused request went through the proxy")
	}
}

func TestClientFollowsNoRedirect(t *testing.T) {
	var second atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { second.Add(1) }))
	defer other.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusFound)
	}))
	defer first.Close()
	c := netguard.NewClient(&netguard.Dialer{Policy: netguardtest.Loopback()}, 5*time.Second)
	resp, err := c.Get(first.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || second.Load() != 0 {
		t.Errorf("status %d, second host hit %d times", resp.StatusCode, second.Load())
	}
	// A redirect to a private address is not followed, and a dial there is refused anyway.
	priv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://10.0.0.1/x", http.StatusFound)
	}))
	defer priv.Close()
	resp, err = c.Get(priv.URL)
	if err != nil || resp.StatusCode != http.StatusFound {
		t.Fatalf("%v", err)
	}
	resp.Body.Close()
	if _, err := c.Get("http://10.0.0.1/x"); !errors.Is(err, netguard.ErrRefused) {
		t.Errorf("private dial: %v", err)
	}
}
