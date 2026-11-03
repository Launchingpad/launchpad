package backup

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSiloDialerEndpoint(t *testing.T) {
	for _, tc := range []struct {
		ep   string
		addr string
		ok   bool
	}{
		{"http://silo:9000", "silo:9000", true},
		{"http://Silo.:9000/", "silo:9000", true},
		{"https://s3.example.com", "s3.example.com:443", true},
		{"http://silo", "silo:80", true},
		{"http://10.1.2.3:9000", "10.1.2.3:9000", true},
		{"", "", false}, {"silo:9000", "", false}, {"ftp://silo", "", false}, {"http://", "", false},
		{"http://u:p@silo:9000", "", false}, {"http://silo:9000/bucket", "", false}, {"http://silo:9000?x=1", "", false},
		{"http://silo:9000#f", "", false},
	} {
		d, err := NewSiloDialer(tc.ep, 0)
		if (err == nil) != tc.ok {
			t.Errorf("%q: err=%v", tc.ep, err)
			continue
		}
		if err == nil && d.Address() != tc.addr {
			t.Errorf("%q: address %s", tc.ep, d.Address())
		}
	}
}

func TestSiloDialerOnlyTheConfiguredAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "silo") }))
	defer srv.Close()
	hostport := strings.TrimPrefix(srv.URL, "http://")
	_, port, _ := net.SplitHostPort(hostport)

	// The endpoint is the loopback address: a private-range address the user-entered guard would
	// refuse. The silo dialer connects, because the NAME matches the configuration, with no policy.
	d, err := NewSiloDialer("http://127.0.0.1:"+port, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.NewHTTPClient(2 * time.Second).Get("http://127.0.0.1:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(c.Body)
	c.Body.Close()
	if string(b) != "silo" {
		t.Errorf("body %q", b)
	}

	for _, addr := range []string{
		"127.0.0.2:" + port,       // another address
		"localhost:" + port,       // another name for the same host: refused BEFORE resolution
		"127.0.0.1:" + port + "0", // another port
		"127.0.0.1:1",             // another port
		"169.254.169.254:80",      // metadata
		"10.0.0.1:" + port,        // private
		"[::1]:" + port,           // loopback v6
		"127.0.0.1",               // no port
		"example.com:443",         // public
		"127.0.0.1.evil.example:" + port,
	} {
		if _, err := d.DialContext(context.Background(), "tcp", addr); err == nil {
			t.Errorf("dialed %s", addr)
		}
	}
	if _, err := d.DialContext(context.Background(), "udp", "127.0.0.1:"+port); err == nil {
		t.Error("udp accepted")
	}
	// No redirect, no proxy: a redirect comes back as a 3xx.
	rs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "http://127.0.0.2/", 302) }))
	defer rs.Close()
	_, rport, _ := net.SplitHostPort(strings.TrimPrefix(rs.URL, "http://"))
	d2, _ := NewSiloDialer("http://127.0.0.1:"+rport, time.Second)
	resp, err := d2.NewHTTPClient(time.Second).Get(rs.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 302 {
		t.Errorf("status %d", resp.StatusCode)
	}
}
