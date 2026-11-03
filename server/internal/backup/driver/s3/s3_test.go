package s3

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"devops/internal/backup"
	"devops/internal/backup/driver"
	"devops/internal/netguard"
	"devops/internal/netguard/netguardtest"
)

const (
	objName  = "launchpad-0123abcd-20261026T031500Z-abcd.dump.age"
	manName  = "launchpad-0123abcd-20261026T031500Z-abcd.json"
	testName = "launchpad-0123abcd-test-0011223344556677.tmp"
)

func plainPolicy() netguard.Policy {
	p := netguardtest.Permit(netguard.Policy{}, "127.0.0.1")
	p.Private = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	return p
}

func dialer(p netguard.Policy) *netguard.Dialer {
	return &netguard.Dialer{Policy: p, Timeout: 5 * time.Second}
}

func cfgJSON(t testing.TB, m map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func secretsJSON(f *fakeS3) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"accessKey": f.access, "secretKey": f.secret})
	return b
}

// start serves f over http and returns its endpoint.
func start(t *testing.T, f *fakeS3) string {
	t.Helper()
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f.srv.URL
}

func openPlain(t *testing.T, f *fakeS3, extra map[string]any) driver.Remote {
	t.Helper()
	ep := start(t, f)
	m := map[string]any{"endpoint": ep, "bucket": f.bucket, "folder": "backups/main"}
	for k, v := range extra {
		m[k] = v
	}
	r, err := Driver{}.Open(context.Background(), driver.Config{Config: cfgJSON(t, m), Secrets: secretsJSON(f), Dialer: dialer(plainPolicy()), AllowPlainHTTP: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func code(err error) driver.Code {
	if err == nil {
		return ""
	}
	return driver.CodeOf(err)
}

func put(t *testing.T, r driver.Remote, name string, b []byte) {
	t.Helper()
	if err := r.Put(context.Background(), name, int64(len(b)), bytes.NewReader(b)); err != nil {
		t.Fatalf("Put %s: %v", name, err)
	}
}

func TestValidateAndIdentity(t *testing.T) {
	ok := func(m map[string]any) json.RawMessage {
		base := map[string]any{"endpoint": "https://s3.example.com", "bucket": "my-bucket"}
		for k, v := range m {
			base[k] = v
		}
		return cfgJSON(t, base)
	}
	good := []map[string]any{
		{},
		{"folder": "a/b-c_d.e", "region": "eu-west-2", "addressing": "virtual"},
		{"endpoint": "http://10.0.0.5:9000"},
		{"tlsPin": strings.Repeat("ab", 32)},
		{"endpoint": "https://s3.example.com/"},
	}
	for i, m := range good {
		if err := (Driver{}).Validate(ok(m)); err != nil {
			t.Errorf("good[%d] %v: %v", i, m, err)
		}
	}
	bad := []map[string]any{
		{"endpoint": ""}, {"endpoint": "ftp://x.example.com"}, {"endpoint": "https://u:p@x.example.com"},
		{"endpoint": "https://x.example.com/path"}, {"endpoint": "https://x.example.com?a=b"}, {"endpoint": "https://x.example.com#f"},
		{"endpoint": "https://x.example.com\\y"}, {"endpoint": "s3.example.com"},
		{"bucket": ""}, {"bucket": "Up"}, {"bucket": "a"}, {"bucket": "-ab"}, {"bucket": "a/b"},
		{"folder": "../x"}, {"folder": "a/../b"}, {"folder": "/a"}, {"folder": "a/"}, {"folder": "a//b"}, {"folder": "a b"}, {"folder": "a..b"},
		{"folder": strings.Repeat("a/", 120)},
		{"region": "US East"}, {"addressing": "dns"}, {"tlsPin": "ABC"}, {"tlsPin": strings.Repeat("AB", 32)},
		{"unknown": 1}, {"hostKey": "x"},
	}
	for i, m := range bad {
		if err := (Driver{}).Validate(ok(m)); err == nil {
			t.Errorf("bad[%d] %v accepted", i, m)
		}
	}
	if err := (Driver{}).Validate(json.RawMessage(`{"endpoint":"https://x.example.com","bucket":"abc"} {}`)); err == nil {
		t.Error("trailing JSON accepted")
	}
	if err := (Driver{}).Validate(json.RawMessage(`not json`)); err == nil {
		t.Error("garbage accepted")
	}
	// Identity: case and a default port do not make a different destination.
	a, err := Driver{}.Identity(ok(map[string]any{"endpoint": "https://S3.Example.com:443", "folder": "x/y"}))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Driver{}.Identity(ok(map[string]any{"endpoint": "https://s3.example.com", "folder": "x/y", "region": "eu-west-1", "tlsPin": strings.Repeat("0", 64)}))
	if a != b || a.Type != "s3" || a.Host != "s3.example.com" || a.Bucket != "my-bucket" || a.Folder != "x/y" {
		t.Errorf("identity %+v vs %+v", a, b)
	}
	c, _ := Driver{}.Identity(ok(map[string]any{"endpoint": "https://s3.example.com:8443"}))
	if c.Host != "s3.example.com:8443" {
		t.Errorf("port identity = %q", c.Host)
	}
	if _, err := (Driver{}).Identity(json.RawMessage(`{}`)); err == nil {
		t.Error("identity of an invalid config")
	}
}

func TestRegistered(t *testing.T) {
	d, ok := driver.Lookup(Type)
	if !ok {
		t.Fatal("s3 is not registered")
	}
	if _, isS3 := d.(Driver); !isS3 {
		t.Fatalf("registered %T", d)
	}
}

func TestContract(t *testing.T) {
	f := newFake()
	f.pageSize = 2
	r := openPlain(t, f, nil)
	ctx := context.Background()
	obj := bytes.Repeat([]byte("0123456789"), 5000)
	put(t, r, objName, obj)
	put(t, r, manName, []byte(`{"m":1}`))
	put(t, r, "launchpad-0123abcd-20261027T031500Z-aaaa.dump.age", []byte("a"))
	put(t, r, "launchpad-0123abcd-20261028T031500Z-bbbb.dump.age", []byte("b"))
	// Keys that are not ours: a stranger's file in the folder, a nested key, an
	// object of another folder and a bad-month name.
	f.put("backups/main/notes.txt", []byte("n"))
	f.put("backups/main/sub/launchpad-0123abcd-20261029T031500Z-cccc.dump.age", []byte("n"))
	f.put("backups/other/launchpad-0123abcd-20261030T031500Z-dddd.dump.age", []byte("n"))
	f.put("backups/main/launchpad-0123abcd-20261030T031500Z-dddd.dump.age.bak", []byte("n"))
	f.put("backups/mainlaunchpad-0123abcd-20261030T031500Z-eeee.dump.age", []byte("n"))

	es, err := r.List(ctx, "launchpad-0123abcd-")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, e := range es {
		got[e.Name] = e.Size
	}
	if len(es) != 4 || got[objName] != int64(len(obj)) || got[manName] != 7 {
		t.Fatalf("List = %v", es)
	}
	if f.count() < 3 {
		t.Fatalf("pagination did not happen: %d requests", f.count())
	}
	es, _ = r.List(ctx, "launchpad-ffffffff-")
	if len(es) != 0 {
		t.Fatalf("other install listed %v", es)
	}

	rc, err := r.Get(ctx, objName)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(b, obj) {
		t.Fatalf("Get: %v, %d bytes", err, len(b))
	}
	if _, err := r.Get(ctx, "launchpad-0123abcd-20261101T031500Z-ffff.dump.age"); code(err) != driver.NotFound {
		t.Fatalf("Get missing = %v", err)
	}
	if err := r.Delete(ctx, objName); err != nil || f.has("backups/main/"+objName) {
		t.Fatalf("Delete: %v", err)
	}
	if err := r.Delete(ctx, objName); err != nil {
		t.Fatalf("Delete of a missing name must not fail: %v", err)
	}
	// An overwrite of the same name with other bytes.
	put(t, r, manName, []byte(`{"m":2}`))
	if string(f.objects["backups/main/"+manName]) != `{"m":2}` {
		t.Fatal("overwrite lost")
	}
	// Zero bytes.
	put(t, r, "launchpad-0123abcd-20261102T031500Z-0000.json", nil)

	steps := r.Test(ctx, testName)
	want := []string{"connect", "auth", "write", "read", "delete"}
	if len(steps) != len(want) {
		t.Fatalf("steps = %+v", steps)
	}
	for i, s := range steps {
		if s.Name != want[i] || !s.OK || s.Code != "" {
			t.Fatalf("step %d = %+v", i, s)
		}
	}
	if f.has("backups/main/" + testName) {
		t.Fatal("the test file was left behind")
	}
}

func TestNamesAreChecked(t *testing.T) {
	f := newFake()
	r := openPlain(t, f, nil)
	ctx := context.Background()
	bad := []string{"", "../x", "a/b", "notes.txt", "launchpad-0123abcd-20261026T031500Z-abcd.dump.age/..", "launchpad-0123abcd-test-zz.tmp",
		"%2e%2e", "launchpad-0123abcd-20261026T031500Z-abcd.dump.age\n", strings.Repeat("a", 200)}
	n0 := f.count()
	for _, n := range bad {
		if err := r.Put(ctx, n, 1, strings.NewReader("x")); code(err) != driver.NotFound {
			t.Errorf("Put(%q) = %v", n, err)
		}
		if _, err := r.Get(ctx, n); code(err) != driver.NotFound {
			t.Errorf("Get(%q) = %v", n, err)
		}
		if err := r.Delete(ctx, n); code(err) != driver.NotFound {
			t.Errorf("Delete(%q) = %v", n, err)
		}
	}
	for _, p := range []string{"../", "a/b", "a b", "x%00", strings.Repeat("a", 129)} {
		if _, err := r.List(ctx, p); code(err) != driver.NotFound {
			t.Errorf("List(%q) = %v", p, err)
		}
	}
	if f.count() != n0 {
		t.Fatalf("%d requests were sent for refused names", f.count()-n0)
	}
}

func TestTooLarge(t *testing.T) {
	f := newFake()
	r := openPlain(t, f, nil)
	n0 := f.count()
	rd := &failReader{}
	if err := r.Put(context.Background(), objName, MaxObject+1, rd); code(err) != driver.TooLarge {
		t.Fatalf("Put 5 GiB + 1 = %v", err)
	}
	if err := r.Put(context.Background(), objName, -1, rd); code(err) != driver.TooLarge {
		t.Fatalf("Put -1 = %v", err)
	}
	if rd.read || f.count() != n0 {
		t.Fatal("a refused size read the source or sent a request")
	}
	// Exactly 5 GiB is allowed by the check: it must not be refused as too large.
	// (A fake answers at once with 413 so no 5 GiB is sent.)
	f.fault = func(w http.ResponseWriter, r *http.Request) bool { s3Err(w, 400, "EntityTooLarge", "x"); return true }
	if err := r.Put(context.Background(), objName, MaxObject, io.LimitReader(zeroReader{}, MaxObject)); code(err) != driver.TooLarge {
		t.Fatalf("EntityTooLarge = %v", err)
	}
}

type failReader struct{ read bool }

func (f *failReader) Read([]byte) (int, error) {
	f.read = true
	return 0, errors.New("must not be read")
}

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) { clear(b); return len(b), nil }

func TestShortAndWrongSizeSource(t *testing.T) {
	f := newFake()
	r := openPlain(t, f, nil)
	err := r.Put(context.Background(), objName, 100, strings.NewReader("short"))
	if err == nil {
		t.Fatal("a short source was accepted")
	}
	if f.has("backups/main/" + objName) {
		t.Fatal("a short upload was stored")
	}
	// A source longer than the size (the dump changed while it was read) is an
	// error too: the upload is exactly size bytes or it fails.
	if err := r.Put(context.Background(), objName, 3, strings.NewReader("abcdef")); err == nil {
		t.Fatal("a source longer than the declared size was accepted")
	}
}

func TestPutVerifiesStoredSize(t *testing.T) {
	f := newFake()
	r := openPlain(t, f, nil)
	f.fault = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "1")
			w.WriteHeader(200)
			return true
		}
		return false
	}
	if err := r.Put(context.Background(), objName, 5, strings.NewReader("hello")); code(err) != driver.ServerError {
		t.Fatalf("size mismatch = %v", err)
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   string
		hdr    map[string]string
		want   driver.Code
		stat   int
	}{
		{"forbidden", 403, "AccessDenied", nil, driver.Forbidden, 0},
		{"bad key", 403, "InvalidAccessKeyId", nil, driver.AuthFailed, 0},
		{"bad secret", 403, "SignatureDoesNotMatch", nil, driver.AuthFailed, 0},
		{"unauthorized", 401, "", nil, driver.AuthFailed, 0},
		{"skew code", 403, "RequestTimeTooSkewed", nil, driver.ClockSkew, 0},
		{"skew by date", 403, "SignatureDoesNotMatch", map[string]string{"Date": time.Now().Add(-2 * time.Hour).UTC().Format(http.TimeFormat)}, driver.ClockSkew, 0},
		{"expired by date", 403, "AccessDenied", map[string]string{"Date": time.Now().Add(3 * time.Hour).UTC().Format(http.TimeFormat)}, driver.ClockSkew, 0},
		{"expired code", 403, "RequestExpired", nil, driver.ClockSkew, 0},
		{"close clock", 403, "AccessDenied", map[string]string{"Date": time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat)}, driver.Forbidden, 0},
		{"insufficient", 507, "", nil, driver.QuotaExceeded, 0},
		{"quota code", 403, "QuotaExceeded", nil, driver.QuotaExceeded, 0},
		{"no bucket", 404, "NoSuchBucket", nil, driver.NotFound, 0},
		{"no key", 404, "NoSuchKey", nil, driver.NotFound, 0},
		{"bare 404", 404, "", nil, driver.NotFound, 0},
		{"unavailable", 503, "SlowDown", nil, driver.ServerError, 503},
		{"internal", 500, "InternalError", nil, driver.ServerError, 500},
		{"too big", 413, "", nil, driver.TooLarge, 0},
		{"redirect", 301, "PermanentRedirect", map[string]string{"Location": "https://elsewhere.invalid/"}, driver.ServerError, 301},
		{"teapot", 418, "", nil, driver.ServerError, 418},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			r := openPlain(t, f, nil)
			f.fault = func(w http.ResponseWriter, _ *http.Request) bool {
				for k, v := range tc.hdr {
					w.Header().Set(k, v)
				}
				s3Err(w, tc.status, tc.code, "REMOTE-WORDS-MUST-NOT-LEAK secret=SECRETTESTKEY")
				return true
			}
			ctx := context.Background()
			for opName, op := range map[string]func() error{
				"put":    func() error { return r.Put(ctx, objName, 1, strings.NewReader("x")) },
				"list":   func() error { _, err := r.List(ctx, "launchpad-"); return err },
				"get":    func() error { _, err := r.Get(ctx, objName); return err },
				"delete": func() error { return r.Delete(ctx, objName) },
			} {
				err := op()
				if opName == "delete" && tc.want == driver.NotFound && tc.code != "NoSuchBucket" {
					if err != nil {
						t.Errorf("delete: a 404 is not an error: %v", err)
					}
					continue
				}
				var de *driver.Error
				if !errors.As(err, &de) || de.Code != tc.want || de.Status != tc.stat {
					t.Errorf("%s: got %v, want %s/%d", opName, err, tc.want, tc.stat)
					continue
				}
				if s := err.Error(); strings.Contains(s, "REMOTE") || strings.Contains(s, "SECRET") || strings.Contains(s, "127.0.0.1") {
					t.Errorf("%s: error text leaks: %q", opName, s)
				}
			}
		})
	}
	// 5xx is retryable, a redirect and a 4xx are not.
	if !driver.Retryable(driver.ServerErr(503)) || driver.Retryable(driver.ServerErr(301)) || driver.Retryable(driver.Errf(driver.ClockSkew)) {
		t.Fatal("retry classification")
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer other.Close()
	f := newFake()
	f.fault = func(w http.ResponseWriter, r *http.Request) bool {
		http.Redirect(w, r, other.URL+"/x", http.StatusTemporaryRedirect)
		return true
	}
	r := openPlain(t, f, nil)
	err := r.Put(context.Background(), objName, 1, strings.NewReader("x"))
	var de *driver.Error
	if !errors.As(err, &de) || de.Code != driver.ServerError || de.Status != 307 {
		t.Fatalf("redirect = %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("the redirect was followed")
	}
}

func TestStall(t *testing.T) {
	old := stallAfter
	stallAfter = 300 * time.Millisecond
	defer func() { stallAfter = old }()

	t.Run("server stops reading an upload", func(t *testing.T) {
		f := newFake()
		release := make(chan struct{})
		f.fault = func(w http.ResponseWriter, r *http.Request) bool { <-release; return true } // never reads the body
		r := openPlain(t, f, nil)
		defer close(release)
		const size = 256 << 20 // far more than the socket buffers hold
		err := r.Put(context.Background(), objName, size, io.LimitReader(zeroReader{}, size))
		if code(err) != driver.Stalled {
			t.Fatalf("stalled upload = %v", err)
		}
		if !driver.Retryable(err) {
			t.Fatal("a stall is retried")
		}
	})
	t.Run("server never answers", func(t *testing.T) {
		f := newFake()
		release := make(chan struct{})
		f.fault = func(w http.ResponseWriter, r *http.Request) bool { <-release; return true }
		r := openPlain(t, f, nil)
		defer close(release)
		if _, err := r.List(context.Background(), "launchpad-"); code(err) != driver.Stalled {
			t.Fatalf("silent server = %v", err)
		}
	})
	t.Run("download stops", func(t *testing.T) {
		// Fake time (LAUN-175): Get must finish before the stall time starts to
		// count, which a real 300 ms window cannot promise on a loaded host.
		clk := &fakeClock{}
		oldT := newStallTimer
		newStallTimer = clk.newTimer
		defer func() { newStallTimer = oldT }()
		f := newFake()
		release := make(chan struct{})
		f.fault = func(w http.ResponseWriter, r *http.Request) bool {
			w.Header().Set("Content-Length", "1000")
			w.WriteHeader(200)
			_, _ = w.Write([]byte("abc"))
			w.(http.Flusher).Flush()
			<-release
			return true
		}
		r := openPlain(t, f, nil)
		defer close(release)
		rc, err := r.Get(context.Background(), objName)
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		done := make(chan struct{})
		defer close(done)
		step := stallAfter // read here: the goroutine below outlives the subtest, the test restores stallAfter
		go func() {        // time passes, with no byte, until the read gives up
			for {
				select {
				case <-done:
					return
				default:
					clk.advance(step)
					runtime.Gosched()
				}
			}
		}()
		_, err = io.ReadAll(rc)
		if code(err) != driver.Stalled {
			t.Fatalf("stalled download = %v", err)
		}
	})
	t.Run("slow but moving is fine", func(t *testing.T) {
		// Fake time (LAUN-175): 10 reads, 100 ms apart, is 1 s against a 300 ms
		// stall time. Only a touch on every read keeps the upload alive.
		clk := &fakeClock{}
		oldT := newStallTimer
		newStallTimer = clk.newTimer
		defer func() { newStallTimer = oldT }()
		f := newFake()
		r := openPlain(t, f, nil)
		src := &slowReader{n: 10, gap: 100 * time.Millisecond, clk: clk}
		if err := r.Put(context.Background(), objName, 10, src); err != nil {
			t.Fatalf("a moving upload stalled: %v", err)
		}
		if clk.resets() < 10 {
			t.Fatalf("only %d progress touches for 10 reads", clk.resets())
		}
	})
}

// fakeClock drives the stall timers by hand (LAUN-175). A test moves time with
// advance; no wall-clock wait, so a loaded host cannot turn "moving" into "stalled".
type fakeClock struct {
	mu     sync.Mutex
	now    time.Duration
	timers []*fakeTimer
}

type fakeTimer struct {
	c        *fakeClock
	deadline time.Duration
	active   bool
	f        func()
	resets   int
}

func (c *fakeClock) newTimer(d time.Duration, f func()) stallTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, deadline: c.now + d, active: true, f: f}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.deadline, t.active = t.c.now+d, true
	t.resets++
	return was
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.active = false
	return was
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now += d
	var fire []func()
	for _, t := range c.timers {
		if t.active && t.deadline <= c.now {
			t.active = false
			fire = append(fire, t.f)
		}
	}
	c.mu.Unlock()
	for _, f := range fire {
		f()
	}
}

func (c *fakeClock) resets() (n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.timers {
		n += t.resets
	}
	return n
}

// slowReader returns one byte per Read, and lets gap of fake time pass before each.
type slowReader struct {
	n   int
	gap time.Duration
	clk *fakeClock
}

func (s *slowReader) Read(b []byte) (int, error) {
	if s.n == 0 {
		return 0, io.EOF
	}
	s.clk.advance(s.gap)
	s.n--
	b[0] = 'x'
	return 1, nil
}

func TestConnectionReset(t *testing.T) {
	f := newFake()
	f.fault = func(w http.ResponseWriter, r *http.Request) bool {
		c, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			c.Close()
		}
		return true
	}
	r := openPlain(t, f, nil)
	err := r.Put(context.Background(), objName, 3, strings.NewReader("abc"))
	if err == nil {
		t.Fatal("a reset was a success")
	}
	if de := (*driver.Error)(nil); !errors.As(err, &de) {
		t.Fatalf("not a safe error: %T", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("the address leaked: %v", err)
	}
}

func TestContextCancel(t *testing.T) {
	f := newFake()
	release := make(chan struct{})
	f.fault = func(w http.ResponseWriter, r *http.Request) bool { <-release; return true }
	r := openPlain(t, f, nil)
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := r.List(ctx, "launchpad-")
	if code(err) != driver.Timeout {
		t.Fatalf("deadline = %v", err)
	}
}

func TestListLimits(t *testing.T) {
	t.Run("body above the cap", func(t *testing.T) {
		f := newFake()
		f.fault = func(w http.ResponseWriter, r *http.Request) bool {
			w.WriteHeader(200)
			_, _ = w.Write([]byte("<ListBucketResult>"))
			chunk := bytes.Repeat([]byte(" "), 1<<20)
			for i := 0; i < 17; i++ {
				_, _ = w.Write(chunk)
			}
			return true
		}
		r := openPlain(t, f, nil)
		if _, err := r.List(context.Background(), "launchpad-"); code(err) != driver.TooLarge {
			t.Fatalf("huge list = %v", err)
		}
	})
	t.Run("not xml", func(t *testing.T) {
		f := newFake()
		f.fault = func(w http.ResponseWriter, r *http.Request) bool { _, _ = w.Write([]byte("<html>")); return true }
		r := openPlain(t, f, nil)
		if _, err := r.List(context.Background(), "launchpad-"); code(err) != driver.ServerError {
			t.Fatalf("html list = %v", err)
		}
	})
	t.Run("truncated with no token", func(t *testing.T) {
		f := newFake()
		f.fault = func(w http.ResponseWriter, r *http.Request) bool {
			_, _ = w.Write([]byte("<ListBucketResult><IsTruncated>true</IsTruncated></ListBucketResult>"))
			return true
		}
		r := openPlain(t, f, nil)
		if _, err := r.List(context.Background(), "launchpad-"); code(err) != driver.ServerError {
			t.Fatalf("no token = %v", err)
		}
	})
	t.Run("in-folder valid keys without the prefix are dropped", func(t *testing.T) {
		f := newFake()
		f.fault = func(w http.ResponseWriter, r *http.Request) bool {
			_, _ = w.Write([]byte("<ListBucketResult><IsTruncated>false</IsTruncated>" +
				"<Contents><Key>backups/main/" + manName + "</Key><Size>1</Size></Contents>" +
				"<Contents><Key>backups/main/launchpad-ffffffff-20261026T031500Z-abcd.json</Key><Size>2</Size></Contents>" +
				"</ListBucketResult>"))
			return true
		}
		r := openPlain(t, f, nil)
		es, err := r.List(context.Background(), "launchpad-0123abcd-")
		if err != nil || len(es) != 1 || es[0].Name != manName {
			t.Fatalf("List = %v, %v", es, err)
		}
	})
	t.Run("keys outside the folder are dropped", func(t *testing.T) {
		f := newFake()
		f.fault = func(w http.ResponseWriter, r *http.Request) bool {
			// Both keys are 13 characters of prefix + a valid name: without the folder
			// membership check, cutting 13 characters would yield a valid name.
			_, _ = w.Write([]byte("<ListBucketResult><IsTruncated>false</IsTruncated>" +
				"<Contents><Key>backups/other" + objName + "</Key><Size>1</Size></Contents>" +
				"<Contents><Key>backups/mainX" + objName + "</Key><Size>2</Size></Contents>" +
				"<Contents><Key>backups/main/" + manName + "</Key><Size>3</Size></Contents>" +
				"</ListBucketResult>"))
			return true
		}
		r := openPlain(t, f, nil)
		es, err := r.List(context.Background(), "launchpad-")
		if err != nil || len(es) != 1 || es[0].Name != manName || es[0].Size != 3 {
			t.Fatalf("List = %v, %v", es, err)
		}
	})
	t.Run("hostile keys are dropped", func(t *testing.T) {
		f := newFake()
		f.fault = func(w http.ResponseWriter, r *http.Request) bool {
			_, _ = w.Write([]byte(`<ListBucketResult><IsTruncated>false</IsTruncated>
<Contents><Key>backups/main/../../etc/passwd</Key><Size>1</Size></Contents>
<Contents><Key>backups/main/launchpad-0123abcd-20261026T031500Z-abcd.dump.age</Key><Size>7</Size></Contents>
<Contents><Key>backups/main/launchpad-0123abcd-20261026T031500Z-abcd.dump.age/../x</Key><Size>7</Size></Contents>
</ListBucketResult>`))
			return true
		}
		r := openPlain(t, f, nil)
		es, err := r.List(context.Background(), "launchpad-")
		if err != nil || len(es) != 1 || es[0].Name != objName || es[0].Size != 7 {
			t.Fatalf("List = %v, %v", es, err)
		}
	})
}

func TestVirtualAddressing(t *testing.T) {
	f := newFake()
	f.virtual = true
	ep := start(t, f)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(ep, "http://"))
	res := &netguardtest.MapResolver{Hosts: map[string][]string{"bkt-test.s3.fake.test": {"127.0.0.1"}, "s3.fake.test": {"127.0.0.1"}}}
	d := &netguard.Dialer{Policy: plainPolicy(), Resolver: res, Timeout: 5 * time.Second}
	cfg := cfgJSON(t, map[string]any{"endpoint": "http://s3.fake.test:" + port, "bucket": f.bucket, "addressing": "virtual", "folder": "v"})
	r, err := Driver{}.Open(context.Background(), driver.Config{Config: cfg, Secrets: secretsJSON(f), Dialer: d, AllowPlainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	put(t, r, objName, []byte("virtual"))
	if !f.has("v/" + objName) {
		t.Fatalf("objects = %v", f.objects)
	}
	for _, s := range r.Test(context.Background(), testName) {
		if !s.OK {
			t.Fatalf("step %+v", s)
		}
	}
}

func TestWrongCredentials(t *testing.T) {
	f := newFake()
	ep := start(t, f)
	cfg := cfgJSON(t, map[string]any{"endpoint": ep, "bucket": f.bucket})
	for name, sec := range map[string]string{
		"wrong secret": `{"accessKey":"AKTEST","secretKey":"nope"}`,
		"wrong key":    `{"accessKey":"AKNOPE","secretKey":"SECRETTESTKEY"}`,
	} {
		r, err := Driver{}.Open(context.Background(), driver.Config{Config: cfg, Secrets: json.RawMessage(sec), Dialer: dialer(plainPolicy()), AllowPlainHTTP: true})
		if err != nil {
			t.Fatal(err)
		}
		steps := r.Test(context.Background(), testName)
		r.Close()
		if len(steps) != 2 || !steps[0].OK || steps[0].Name != "connect" || steps[1].Name != "auth" || steps[1].OK || steps[1].Code != driver.AuthFailed {
			t.Errorf("%s: steps = %+v", name, steps)
		}
	}
}

func TestSecretsRequired(t *testing.T) {
	cfg := cfgJSON(t, map[string]any{"endpoint": "https://s3.example.com", "bucket": "abc"})
	for _, s := range []string{"", "{}", `{"accessKey":"a"}`, `{"secretKey":"a"}`, "null", "garbage"} {
		_, err := Driver{}.Open(context.Background(), driver.Config{Config: cfg, Secrets: json.RawMessage(s), Dialer: dialer(netguard.Policy{})})
		if code(err) != driver.SecretsUnreadable {
			t.Errorf("secrets %q: %v", s, err)
		}
	}
}

func TestTestStepFailures(t *testing.T) {
	cases := []struct {
		name  string
		fault func(w http.ResponseWriter, r *http.Request) bool
		steps string
	}{
		{"read-only key", func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == http.MethodPut {
				s3Err(w, 403, "AccessDenied", "x")
				return true
			}
			return false
		}, "connect+ auth+ write:forbidden"},
		{"no space", func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == http.MethodPut {
				s3Err(w, 507, "", "x")
				return true
			}
			return false
		}, "connect+ auth+ write:quota_exceeded"},
		{"cannot delete", func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == http.MethodDelete {
				s3Err(w, 403, "AccessDenied", "x")
				return true
			}
			return false
		}, "connect+ auth+ write+ read+ delete:forbidden"},
		{"altered bytes", func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "" {
				w.Header().Set("Content-Length", "3")
				_, _ = w.Write([]byte("bad"))
				return true
			}
			return false
		}, "connect+ auth+ write+ read:server_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			r := openPlain(t, f, nil)
			f.fault = tc.fault
			var parts []string
			for _, s := range r.Test(context.Background(), testName) {
				p := s.Name
				if s.OK {
					p += "+"
				} else {
					p += ":" + string(s.Code)
				}
				parts = append(parts, p)
			}
			if got := strings.Join(parts, " "); got != tc.steps {
				t.Fatalf("steps = %q, want %q", got, tc.steps)
			}
		})
	}
}

// --- the network rules ---

func TestPrivateAddressRefusedByPolicy(t *testing.T) {
	f := newFake()
	ep := start(t, f)
	cfg := cfgJSON(t, map[string]any{"endpoint": ep, "bucket": f.bucket})
	// The default backup policy: loopback is always refused.
	pol, err := backup.Policy("", backup.Gateways{Read: func() ([]netip.Addr, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	r, err := Driver{}.Open(context.Background(), driver.Config{Config: cfg, Secrets: secretsJSON(f), Dialer: backup.NewDialer(pol, 2*time.Second), AllowPlainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.List(context.Background(), "launchpad-"); code(err) != driver.SSRFBlocked {
		t.Fatalf("List = %v", err)
	}
	steps := r.Test(context.Background(), testName)
	if len(steps) != 1 || steps[0].Name != "connect" || steps[0].Code != driver.SSRFBlocked {
		t.Fatalf("steps = %+v", steps)
	}
	if _, err := (Driver{}).Probe(context.Background(), driver.Config{Config: cfg, Dialer: backup.NewDialer(pol, 2*time.Second), AllowPlainHTTP: true}); code(err) != driver.SSRFBlocked {
		t.Fatalf("Probe = %v", err)
	}
	if f.count() != 0 {
		t.Fatal("a request reached the refused address")
	}
	// A service name is refused before it is resolved.
	cfg = cfgJSON(t, map[string]any{"endpoint": "http://postgres:5432", "bucket": f.bucket})
	r2, _ := Driver{}.Open(context.Background(), driver.Config{Config: cfg, Secrets: secretsJSON(f), Dialer: backup.NewDialer(pol, 2*time.Second), AllowPlainHTTP: true})
	if _, err := r2.List(context.Background(), "launchpad-"); code(err) != driver.SSRFBlocked {
		t.Fatalf("service name = %v", err)
	}
}

func TestPlainHTTPRules(t *testing.T) {
	f := newFake()
	ep := start(t, f)
	cfg := cfgJSON(t, map[string]any{"endpoint": ep, "bucket": f.bucket})
	// The caller did not allow plain http.
	if _, err := (Driver{}).Open(context.Background(), driver.Config{Config: cfg, Secrets: secretsJSON(f), Dialer: dialer(plainPolicy())}); code(err) != driver.SSRFBlocked {
		t.Fatalf("Open without AllowPlainHTTP = %v", err)
	}
	if _, err := (Driver{}).Probe(context.Background(), driver.Config{Config: cfg, Dialer: dialer(plainPolicy())}); code(err) != driver.SSRFBlocked {
		t.Fatalf("Probe without AllowPlainHTTP = %v", err)
	}
	// The caller allowed it, but the address the socket connected to is not in a
	// listed range: refused at dial time, before a byte is sent.
	unlisted := netguardtest.Permit(netguard.Policy{}, "127.0.0.1")
	r, err := Driver{}.Open(context.Background(), driver.Config{Config: cfg, Secrets: secretsJSON(f), Dialer: dialer(unlisted), AllowPlainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.List(context.Background(), "launchpad-"); code(err) != driver.SSRFBlocked {
		t.Fatalf("unlisted address = %v", err)
	}
	if _, err := (Driver{}).Probe(context.Background(), driver.Config{Config: cfg, Dialer: dialer(unlisted), AllowPlainHTTP: true}); code(err) != driver.SSRFBlocked {
		t.Fatalf("Probe of an unlisted address = %v", err)
	}
	if f.count() != 0 {
		t.Fatal("a request was sent")
	}
	// Listed: works, and the probe says "plain".
	pr, err := Driver{}.Probe(context.Background(), driver.Config{Config: cfg, Dialer: dialer(plainPolicy()), AllowPlainHTTP: true})
	if err != nil || pr.Kind != "plain" || pr.Fingerprint != "" {
		t.Fatalf("Probe = %+v, %v", pr, err)
	}
}

func TestRebindingToUnlistedAddress(t *testing.T) {
	f := newFake()
	ep := start(t, f)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(ep, "http://"))
	// The name resolves to 127.0.0.1, which is permitted by the test seam but not
	// listed: plain http is refused even though the pre-check said yes.
	res := &netguardtest.MapResolver{Hosts: map[string][]string{"s3.fake.test": {"127.0.0.1"}}}
	d := &netguard.Dialer{Policy: netguardtest.Permit(netguard.Policy{}, "127.0.0.1"), Resolver: res, Timeout: 2 * time.Second}
	cfg := cfgJSON(t, map[string]any{"endpoint": "http://s3.fake.test:" + port, "bucket": f.bucket})
	r, _ := Driver{}.Open(context.Background(), driver.Config{Config: cfg, Secrets: secretsJSON(f), Dialer: d, AllowPlainHTTP: true})
	defer r.Close()
	if _, err := r.List(context.Background(), "launchpad-"); code(err) != driver.SSRFBlocked {
		t.Fatalf("= %v", err)
	}
}

func TestProxyEnvironmentIsIgnored(t *testing.T) {
	var hits atomic.Int32
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer px.Close()
	t.Setenv("HTTP_PROXY", px.URL)
	t.Setenv("http_proxy", px.URL)
	t.Setenv("HTTPS_PROXY", px.URL)
	t.Setenv("https_proxy", px.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	f := newFake()
	ep := start(t, f)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(ep, "http://"))
	// Go never proxies a loopback address, so the endpoint is a real-looking host
	// name that the test resolver maps to 127.0.0.1. With
	// tr.Proxy = http.ProxyFromEnvironment this request would go to px.
	res := &netguardtest.MapResolver{Hosts: map[string][]string{"s3.fake.test": {"127.0.0.1"}}}
	d := &netguard.Dialer{Policy: plainPolicy(), Resolver: res, Timeout: 5 * time.Second}
	cfg := cfgJSON(t, map[string]any{"endpoint": "http://s3.fake.test:" + port, "bucket": f.bucket})
	r, err := Driver{}.Open(context.Background(), driver.Config{Config: cfg, Secrets: secretsJSON(f), Dialer: d, AllowPlainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// Direct and order-independent: Go caches the proxy environment once per
	// process, so also assert that the transport has no proxy function at all.
	if rm, ok := r.(*remote); !ok || rm.tr.Proxy != nil {
		t.Fatal("the transport has a Proxy function")
	}
	put(t, r, objName, []byte("x"))
	if hits.Load() != 0 {
		t.Fatal("the proxy from the environment was used")
	}
	if !f.has(objName) {
		t.Fatal("the object did not reach the server")
	}
}

// --- TLS ---

func startTLS(t *testing.T, f *fakeS3) (endpoint, pin string) {
	t.Helper()
	f.srv = httptest.NewTLSServer(f)
	t.Cleanup(f.srv.Close)
	leaf := f.srv.Certificate()
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return f.srv.URL, hex.EncodeToString(sum[:])
}

func tlsRemote(t *testing.T, f *fakeS3, ep, pin string) (driver.Remote, error) {
	m := map[string]any{"endpoint": ep, "bucket": f.bucket}
	if pin != "" {
		m["tlsPin"] = pin
	}
	return Driver{}.Open(context.Background(), driver.Config{Config: cfgJSON(t, m), Secrets: secretsJSON(f),
		Dialer: dialer(netguardtest.Permit(netguard.Policy{}, "127.0.0.1"))})
}

func TestTLSPin(t *testing.T) {
	f := newFake()
	ep, pin := startTLS(t, f)

	t.Run("matching pin works with a certificate no root trusts", func(t *testing.T) {
		r, err := tlsRemote(t, f, ep, pin)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		put(t, r, objName, []byte("pinned"))
		for _, s := range r.Test(context.Background(), testName) {
			if !s.OK {
				t.Fatalf("step %+v", s)
			}
		}
	})
	t.Run("wrong pin", func(t *testing.T) {
		r, _ := tlsRemote(t, f, ep, strings.Repeat("0", 64))
		defer r.Close()
		err := r.Put(context.Background(), objName, 1, strings.NewReader("x"))
		if code(err) != driver.TLSPinMismatch {
			t.Fatalf("Put = %v", err)
		}
		if driver.Retryable(err) {
			t.Fatal("a pin mismatch is not retried")
		}
		steps := r.Test(context.Background(), testName)
		if len(steps) != 1 || steps[0].Name != "connect" || steps[0].Code != driver.TLSPinMismatch {
			t.Fatalf("steps = %+v", steps)
		}
	})
	t.Run("no pin: the chain is checked", func(t *testing.T) {
		r, _ := tlsRemote(t, f, ep, "")
		defer r.Close()
		if _, err := r.List(context.Background(), "launchpad-"); code(err) != driver.TLSUntrusted {
			t.Fatalf("List = %v", err)
		}
	})
	t.Run("a pin does not skip the expiry date", func(t *testing.T) {
		old := clockNow
		clockNow = func() time.Time { return time.Now().AddDate(100, 0, 0) }
		defer func() { clockNow = old }()
		r, _ := tlsRemote(t, f, ep, pin)
		defer r.Close()
		if _, err := r.List(context.Background(), "launchpad-"); code(err) != driver.TLSUntrusted {
			t.Fatalf("List = %v", err)
		}
	})
	t.Run("probe", func(t *testing.T) {
		n0 := f.count()
		pr, err := Driver{}.Probe(context.Background(), driver.Config{Config: cfgJSON(t, map[string]any{"endpoint": ep, "bucket": f.bucket}),
			Dialer: dialer(netguardtest.Permit(netguard.Policy{}, "127.0.0.1"))})
		if err != nil {
			t.Fatal(err)
		}
		if pr.Kind != "tls" || pr.Fingerprint != pin || pr.ChainVerified {
			t.Fatalf("Probe = %+v", pr)
		}
		if f.count() != n0 {
			t.Fatal("the probe sent an HTTP request")
		}
	})
	t.Run("probe of a plain-text port", func(t *testing.T) {
		pf := newFake()
		pep := start(t, pf)
		pep = "https://" + strings.TrimPrefix(pep, "http://")
		_, err := Driver{}.Probe(context.Background(), driver.Config{Config: cfgJSON(t, map[string]any{"endpoint": pep, "bucket": pf.bucket}),
			Dialer: dialer(netguardtest.Permit(netguard.Policy{}, "127.0.0.1"))})
		if err == nil {
			t.Fatal("a non-TLS server probed as TLS")
		}
		if _, ok := err.(*driver.Error); !ok {
			t.Fatalf("not a safe error: %T", err)
		}
	})
}

func TestProbeConnectionRefused(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	_, err := Driver{}.Probe(context.Background(), driver.Config{Config: cfgJSON(t, map[string]any{"endpoint": "https://" + addr, "bucket": "abc"}),
		Dialer: dialer(netguardtest.Permit(netguard.Policy{}, "127.0.0.1"))})
	if code(err) != driver.ConnectRefused {
		t.Fatalf("Probe = %v", err)
	}
}

// --- memory ---

// A large object streams: the heap stays far below the object size.
func TestPutStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	const size = 192 << 20
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			n, _ := io.Copy(io.Discard, r.Body)
			if n != size {
				w.WriteHeader(400)
				return
			}
			w.WriteHeader(200)
		case http.MethodHead:
			w.Header().Set("Content-Length", fmt.Sprint(size))
			w.WriteHeader(200)
		}
	}))
	defer srv.Close()
	cfg := cfgJSON(t, map[string]any{"endpoint": srv.URL, "bucket": "abc"})
	r, err := Driver{}.Open(context.Background(), driver.Config{Config: cfg, Secrets: json.RawMessage(`{"accessKey":"a","secretKey":"b"}`),
		Dialer: dialer(plainPolicy()), AllowPlainHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	runtime.GC()
	var peak atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				return
			default:
			}
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > peak.Load() {
				peak.Store(ms.HeapAlloc)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	err = r.Put(context.Background(), objName, size, io.LimitReader(zeroReader{}, size))
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if peak.Load() > 64<<20 {
		t.Fatalf("heap peaked at %d MiB for a %d MiB object", peak.Load()>>20, size>>20)
	}
}

// --- virtual addressing over TLS: the certificate is for bucket.host ---

// leafFor makes a CA and a leaf for the given DNS names; it returns the leaf
// certificate chain for a server, the CA pool and the leaf's SPKI sha256 (hex).
func leafFor(t *testing.T, dns ...string) (tls.Certificate, *x509.CertPool, string) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "leaf"}, DNSNames: dns,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool, hex.EncodeToString(sum[:])
}

// virtualTLS serves the fake over TLS with cert and records the SNI names.
func virtualTLS(t *testing.T, f *fakeS3, cert tls.Certificate) (cfg func(extra map[string]any) driver.Config, sni *sniLog) {
	t.Helper()
	f.virtual = true
	sni = &sniLog{}
	srv := httptest.NewUnstartedServer(f)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		sni.add(h.ServerName)
		return nil, nil
	}}
	srv.StartTLS()
	f.srv = srv
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	res := &netguardtest.MapResolver{Hosts: map[string][]string{"bkt-test.s3.fake.test": {"127.0.0.1"}, "s3.fake.test": {"127.0.0.1"}}}
	d := &netguard.Dialer{Policy: netguardtest.Permit(netguard.Policy{}, "127.0.0.1"), Resolver: res, Timeout: 5 * time.Second}
	return func(extra map[string]any) driver.Config {
		m := map[string]any{"endpoint": "https://s3.fake.test:" + port, "bucket": f.bucket, "addressing": "virtual", "folder": "v"}
		for k, v := range extra {
			m[k] = v
		}
		return driver.Config{Config: cfgJSON(t, m), Secrets: secretsJSON(f), Dialer: d}
	}, sni
}

type sniLog struct {
	mu    sync.Mutex
	names []string
}

func (s *sniLog) add(n string) { s.mu.Lock(); s.names = append(s.names, n); s.mu.Unlock() }
func (s *sniLog) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.names...)
}

func TestVirtualAddressingTLS(t *testing.T) {
	ctx := context.Background()
	const vhost = "bkt-test.s3.fake.test"
	for name, dns := range map[string][]string{"san": {vhost}, "wildcard": {"*.s3.fake.test"}} {
		t.Run(name+" cert, not valid for the bare host", func(t *testing.T) {
			old := testRoots
			defer func() { testRoots = old }()
			cert, roots, pin := leafFor(t, dns...)
			testRoots = roots
			f := newFake()
			cfg, sni := virtualTLS(t, f, cert)

			// No pin: the chain and the host name are checked against bucket.host.
			r, err := Driver{}.Open(ctx, cfg(nil))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			put(t, r, objName, []byte("virtual tls"))
			rc, err := r.Get(ctx, objName)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(rc)
			rc.Close()
			if string(b) != "virtual tls" {
				t.Fatalf("Get = %q", b)
			}
			if es, err := r.List(ctx, "launchpad-"); err != nil || len(es) != 1 {
				t.Fatalf("List = %v, %v", es, err)
			}

			// Probe sees the SAME leaf the requests use, and verifies it for bucket.host.
			pr, err := Driver{}.Probe(ctx, cfg(nil))
			if err != nil {
				t.Fatal(err)
			}
			if pr.Kind != "tls" || pr.Fingerprint != pin || !pr.ChainVerified {
				t.Fatalf("Probe = %+v (pin %s)", pr, pin)
			}

			// A pin saved from the probe works; a wrong one does not.
			pr2, err := Driver{}.Open(ctx, cfg(map[string]any{"tlsPin": pr.Fingerprint}))
			if err != nil {
				t.Fatal(err)
			}
			defer pr2.Close()
			testRoots = nil // the pin alone must carry the connection
			put(t, pr2, manName, []byte("pinned"))
			bad, _ := Driver{}.Open(ctx, cfg(map[string]any{"tlsPin": strings.Repeat("0", 64)}))
			defer bad.Close()
			if err := bad.Put(ctx, manName, 1, strings.NewReader("x")); code(err) != driver.TLSPinMismatch {
				t.Fatalf("wrong pin = %v", err)
			}
			// Every handshake (requests and probe) asked for bucket.host, never the bare host.
			for _, n := range sni.all() {
				if n != vhost {
					t.Fatalf("SNI %q, want %q (all %v)", n, vhost, sni.all())
				}
			}
			if len(sni.all()) == 0 {
				t.Fatal("no handshake seen")
			}
		})
	}
	t.Run("a cert for the bare host only is the wrong host", func(t *testing.T) {
		old := testRoots
		defer func() { testRoots = old }()
		cert, roots, _ := leafFor(t, "s3.fake.test")
		testRoots = roots
		f := newFake()
		cfg, _ := virtualTLS(t, f, cert)
		r, _ := Driver{}.Open(ctx, cfg(nil))
		defer r.Close()
		if err := r.Put(ctx, objName, 1, strings.NewReader("x")); code(err) != driver.TLSWrongHost {
			t.Fatalf("Put = %v", err)
		}
		pr, err := Driver{}.Probe(ctx, cfg(nil))
		if err != nil || pr.ChainVerified {
			t.Fatalf("Probe = %+v, %v (the chain must not verify for bucket.host)", pr, err)
		}
	})
}

// An https:// endpoint on a loopback or private address under the default
// backup policy is refused by the guarded dialer: nothing is sent.
func TestHTTPSLoopbackRefusedByDefaultPolicy(t *testing.T) {
	f := newFake()
	ep, _ := startTLS(t, f)
	pol, err := backup.Policy("", backup.Gateways{Read: func() ([]netip.Addr, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	c := driver.Config{Config: cfgJSON(t, map[string]any{"endpoint": ep, "bucket": f.bucket}), Secrets: secretsJSON(f),
		Dialer: backup.NewDialer(pol, 2*time.Second)}
	r, err := Driver{}.Open(context.Background(), c)
	if err != nil {
		t.Fatalf("Open = %v", err) // Open dials nothing; the first request is refused
	}
	defer r.Close()
	if _, err := r.List(context.Background(), "launchpad-"); code(err) != driver.SSRFBlocked {
		t.Fatalf("List = %v", err)
	}
	if _, err := (Driver{}).Probe(context.Background(), c); code(err) != driver.SSRFBlocked {
		t.Fatalf("Probe = %v", err)
	}
	if f.count() != 0 {
		t.Fatal("a request reached the refused address")
	}
}

// lateReader gives head, holds the next byte back for a while (so a server that
// answers on the declared bytes alone has time to do it), then gives tail. net/http
// probes for an extra body byte only after it has written the declared bytes, and
// such a server used to win that race: a long source "succeeded" (LAUN-158).
type lateReader struct {
	head, tail string
	pos        int
	waited     bool
}

func (g *lateReader) Read(b []byte) (int, error) {
	if g.pos < len(g.head) {
		n := copy(b, g.head[g.pos:])
		g.pos += n
		return n, nil
	}
	if !g.waited {
		g.waited = true
		time.Sleep(200 * time.Millisecond)
	}
	if g.pos-len(g.head) >= len(g.tail) {
		return 0, io.EOF
	}
	n := copy(b, g.tail[g.pos-len(g.head):])
	g.pos += n
	return n, nil
}

func TestLongSourceNeverSucceedsEvenIfServerAnswersFirst(t *testing.T) {
	f := newFake()
	r := openPlain(t, f, nil)
	for i := 0; i < 2; i++ {
		err := r.Put(context.Background(), objName, 3, &lateReader{head: "abc", tail: "defgh"})
		if err == nil {
			t.Fatal("a source longer than its declared size succeeded")
		}
		if code(err) != driver.ServerError || strings.Contains(err.Error(), "source") {
			t.Fatalf("error %v", err)
		}
	}
	if f.has("backups/main/" + objName) {
		t.Fatal("a long source left a stored object")
	}
}

func TestSizeZeroSource(t *testing.T) {
	f := newFake()
	r := openPlain(t, f, nil)
	err := r.Put(context.Background(), objName, 0, strings.NewReader("x"))
	if err == nil || code(err) != driver.ServerError {
		t.Fatalf("a non-empty source with declared size 0: %v", err)
	}
	if f.has("backups/main/" + objName) {
		t.Fatal("a non-empty source was stored as an empty object")
	}
	if err := r.Put(context.Background(), objName, 0, strings.NewReader("")); err != nil {
		t.Fatalf("an empty source with declared size 0: %v", err)
	}
	if !f.has("backups/main/" + objName) {
		t.Fatal("the empty object was not stored")
	}
}
