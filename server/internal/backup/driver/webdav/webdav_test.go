package webdav

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

func permitDialer() *netguard.Dialer {
	return &netguard.Dialer{Policy: netguardtest.Permit(netguard.Policy{}, "127.0.0.1"), Timeout: 5 * time.Second}
}

func cfgJSON(t testing.TB, m map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func secretsJSON(pass string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"password": pass})
	return b
}

func code(err error) driver.Code {
	if err == nil {
		return ""
	}
	return driver.CodeOf(err)
}

// startTLS serves f over TLS (the driver is https only) and returns the base URL
// and the leaf's SPKI pin.
func startTLS(t *testing.T, f *fakeDAV) (base, pin string) {
	t.Helper()
	f.srv = httptest.NewTLSServer(f)
	t.Cleanup(f.srv.Close)
	sum := sha256.Sum256(f.srv.Certificate().RawSubjectPublicKeyInfo)
	return f.srv.URL + davBase, hex.EncodeToString(sum[:])
}

func davConfig(t *testing.T, base, pin string, extra map[string]any) map[string]any {
	m := map[string]any{"url": base, "username": davUser, "folder": "backups/main"}
	if pin != "" {
		m["tlsPin"] = pin
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func openDAV(t *testing.T, f *fakeDAV, extra map[string]any) driver.Remote {
	t.Helper()
	base, pin := startTLS(t, f)
	r, err := Driver{}.Open(context.Background(), driver.Config{Config: cfgJSON(t, davConfig(t, base, pin, extra)), Secrets: secretsJSON(davPass), Dialer: permitDialer()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func put(t *testing.T, r driver.Remote, name string, b []byte) {
	t.Helper()
	if err := r.Put(context.Background(), name, int64(len(b)), bytes.NewReader(b)); err != nil {
		t.Fatalf("Put %s: %v", name, err)
	}
}

func TestValidateAndIdentity(t *testing.T) {
	ok := func(m map[string]any) json.RawMessage {
		base := map[string]any{"url": "https://cloud.example.com/remote.php/dav/files/alice/", "username": "alice"}
		for k, v := range m {
			base[k] = v
		}
		return cfgJSON(t, base)
	}
	good := []map[string]any{
		{},
		{"folder": "a/b-c_d.e"},
		{"url": "https://cloud.example.com"}, {"url": "https://cloud.example.com/"},
		{"url": "https://cloud.example.com:8443/dav"},
		{"url": "https://cloud.example.com/remote.php/dav/files/al%40ex.com/"},
		{"url": "https://cloud.example.com/remote.php/dav/files/Alice%20Smith/"},
		{"url": "https://[2001:4860:4860::8888]/dav/"},
		{"tlsPin": strings.Repeat("ab", 32)},
		{"username": "alice@example.com"},
	}
	for i, m := range good {
		if err := (Driver{}).Validate(ok(m)); err != nil {
			t.Errorf("good[%d] %v: %v", i, m, err)
		}
	}
	bad := []map[string]any{
		{"url": ""}, {"url": "http://cloud.example.com/dav/"}, {"url": "ftp://x.example.com"}, {"url": "cloud.example.com"},
		{"url": "https://u:p@cloud.example.com/"}, {"url": "https://cloud.example.com/dav?a=b"}, {"url": "https://cloud.example.com/dav#f"},
		{"url": "https://cloud.example.com/dav?"}, {"url": "https://cloud.example.com\\dav"}, {"url": "https://cloud.example.com/a/../b"},
		{"url": "https://cloud.example.com/a/./b"}, {"url": "https://cloud.example.com//dav"}, {"url": "https://cloud.example.com/a//b"},
		{"url": "https://cloud.example.com/a%2Fb"}, {"url": "https://cloud.example.com/a%2fb"}, {"url": "https://cloud.example.com/a%5Cb"},
		{"url": "https://cloud.example.com/a%00b"}, {"url": "https://cloud.example.com/%2e%2e/b"}, {"url": "https://cloud.example.com/a%2e%2e/b"}, {"url": "https://cloud.example.com/Alice Smith/"}, {"url": "https://cloud.example.com./dav"},
		{"url": "https://cloud.example.com/a b%0A"}, {"url": "https://cloud.example.com/dav/" + strings.Repeat("a/", 250)},
		{"url": "https://cloud.example.com/a\"b"},
		{"username": ""}, {"username": "al:ice"}, {"username": "al\nice"}, {"username": strings.Repeat("a", 256)},
		{"folder": "../x"}, {"folder": "a/../b"}, {"folder": "/a"}, {"folder": "a/"}, {"folder": "a//b"}, {"folder": "a b"}, {"folder": "a..b"},
		{"folder": "a%2fb"}, {"folder": strings.Repeat("a/", 120)},
		{"tlsPin": "ABC"}, {"tlsPin": strings.Repeat("AB", 32)},
		{"unknown": 1}, {"hostKey": "x"}, {"password": "x"},
	}
	for i, m := range bad {
		if err := (Driver{}).Validate(ok(m)); err == nil {
			t.Errorf("bad[%d] %v accepted", i, m)
		}
	}
	if err := (Driver{}).Validate(json.RawMessage(`{"url":"https://x.example.com/","username":"a"} {}`)); err == nil {
		t.Error("trailing JSON accepted")
	}
	if err := (Driver{}).Validate(json.RawMessage(`not json`)); err == nil {
		t.Error("garbage accepted")
	}
	a, err := Driver{}.Identity(ok(map[string]any{"url": "https://CLOUD.Example.com:443/remote.php/dav/files/alice", "folder": "x/y"}))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Driver{}.Identity(ok(map[string]any{"folder": "x/y", "username": "other", "tlsPin": strings.Repeat("0", 64)}))
	if a != b || a.Type != "webdav" || a.Host != "cloud.example.com" || a.Bucket != "remote.php/dav/files/alice" || a.Folder != "x/y" {
		t.Errorf("identity %+v vs %+v", a, b)
	}
	c, _ := Driver{}.Identity(ok(map[string]any{"url": "https://cloud.example.com:8443/remote.php/dav/files/alice/"}))
	if c.Host != "cloud.example.com:8443" || c == a {
		t.Errorf("port identity = %+v", c)
	}
	d, _ := Driver{}.Identity(ok(map[string]any{"url": "https://cloud.example.com/remote.php/dav/files/bob/", "folder": "x/y"}))
	if d == a {
		t.Error("another base path is the same destination")
	}
	if _, err := (Driver{}).Identity(json.RawMessage(`{}`)); err == nil {
		t.Error("identity of an invalid config")
	}
}

func TestRegistered(t *testing.T) {
	d, ok := driver.Lookup(Type)
	if !ok {
		t.Fatal("webdav is not registered")
	}
	if _, is := d.(Driver); !is {
		t.Fatalf("registered %T", d)
	}
}

func TestContract(t *testing.T) {
	f := newFake()
	r := openDAV(t, f, nil)
	ctx := context.Background()
	if es, err := r.List(ctx, "launchpad-"); err != nil || len(es) != 0 {
		t.Fatalf("List of a folder that does not exist yet = %v, %v", es, err)
	}
	put(t, r, objName, []byte("object-bytes"))
	put(t, r, manName, []byte("{}"))
	// Nested folders are made level by level, once.
	reqs := strings.Join(f.requests(), "\n")
	for _, want := range []string{"MKCOL " + davBase + "backups/", "MKCOL " + davBase + "backups/main/", "PUT " + davBase + "backups/main/" + objName} {
		if !strings.Contains(reqs, want) {
			t.Errorf("missing request %q in\n%s", want, reqs)
		}
	}
	if n := strings.Count(reqs, "MKCOL"); n != 2 {
		t.Errorf("MKCOL count = %d, want 2 (the second Put reuses the folder)", n)
	}
	// Files that are not ours or not valid names are not listed.
	f.setFile(davBase+"backups/main/notes.txt", []byte("x"))
	f.setFile(davBase+"backups/main/launchpad-ffffffff-20260101T000000Z-0001.json", []byte("other install"))
	f.mu.Lock()
	f.dirs[davBase+"backups/main/launchpad-0123abcd-20260101T000000Z-0002.json"] = true // a collection with a valid name
	f.mu.Unlock()
	es, err := r.List(ctx, "launchpad-0123abcd-")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, e := range es {
		got[e.Name] = e.Size
	}
	if len(got) != 2 || got[objName] != 12 || got[manName] != 2 {
		t.Fatalf("List = %v", got)
	}
	rc, err := r.Get(ctx, objName)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "object-bytes" {
		t.Fatalf("Get = %q", b)
	}
	put(t, r, objName, []byte("replaced")) // 204 on replace
	if err := r.Delete(ctx, objName); err != nil {
		t.Fatal(err)
	}
	if f.has(davBase + "backups/main/" + objName) {
		t.Fatal("Delete left the file")
	}
	if err := r.Delete(ctx, objName); err != nil {
		t.Fatalf("Delete of a missing file = %v", err)
	}
	if _, err := r.Get(ctx, objName); code(err) != driver.NotFound {
		t.Fatalf("Get missing = %v", err)
	}
	steps := r.Test(ctx, testName)
	if len(steps) != 5 {
		t.Fatalf("steps = %+v", steps)
	}
	for _, s := range steps {
		if !s.OK {
			t.Fatalf("step %+v", s)
		}
	}
	if f.has(davBase + "backups/main/" + testName) {
		t.Fatal("the test file was left behind")
	}
	// Basic auth went to the server on every request.
	for _, a := range f.auths {
		if !strings.HasPrefix(a, "Basic ") {
			t.Fatalf("a request had no Basic auth: %q", a)
		}
	}
}

func TestNoFolderUsesBase(t *testing.T) {
	f := newFake()
	r := openDAV(t, f, map[string]any{"folder": ""})
	put(t, r, objName, []byte("x"))
	if !f.has(davBase + objName) {
		t.Fatal("object not in the base")
	}
	for _, q := range f.requests() {
		if strings.HasPrefix(q, "MKCOL") {
			t.Fatalf("MKCOL with no folder: %s", q)
		}
	}
	es, err := r.List(context.Background(), "launchpad-")
	if err != nil || len(es) != 1 || es[0].Name != objName {
		t.Fatalf("List = %v, %v", es, err)
	}
}

func TestNamesAreChecked(t *testing.T) {
	f := newFake()
	r := openDAV(t, f, nil)
	ctx := context.Background()
	n0 := f.count()
	for _, n := range []string{"", "..", "../x", "a/b", "/etc/passwd", objName + "/../x", "launchpad-0123abcd-20261026T031500Z-abcd.dump.age/..",
		"launchpad-0123abcd-20261026T031500Z-abcd%2f.json", "x.txt", objName + "\n", "http://evil/x"} {
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
	for _, p := range []string{"../", "a/b", "launchpad-%2f", strings.Repeat("a", 129), "a b"} {
		if _, err := r.List(ctx, p); code(err) != driver.NotFound {
			t.Errorf("List(%q) = %v", p, err)
		}
	}
	if f.count() != n0 {
		t.Fatalf("a refused name sent %d request(s)", f.count()-n0)
	}
}

func TestTooLarge(t *testing.T) {
	f := newFake()
	r := openDAV(t, f, nil)
	n0 := f.count()
	for _, size := range []int64{MaxObject + 1, -1} {
		src := &failReader{}
		if err := r.Put(context.Background(), objName, size, src); code(err) != driver.TooLarge {
			t.Fatalf("Put(size %d) = %v", size, err)
		}
		if src.read {
			t.Fatal("the source was read")
		}
	}
	if f.count() != n0 {
		t.Fatal("a request was sent")
	}
}

type failReader struct{ read bool }

func (f *failReader) Read([]byte) (int, error) { f.read = true; return 0, io.ErrUnexpectedEOF }

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) { clear(b); return len(b), nil }

func TestShortAndWrongSizeSource(t *testing.T) {
	f := newFake()
	r := openDAV(t, f, nil)
	if err := r.Put(context.Background(), objName, 10, strings.NewReader("short")); err == nil {
		t.Fatal("a short source succeeded")
	}
	if err := r.Put(context.Background(), objName, 3, strings.NewReader("longer than three")); err == nil {
		t.Fatal("a long source succeeded")
	}
}

func TestPutVerifiesStoredSize(t *testing.T) {
	f := newFake()
	f.sizeLie = -1 // the server reports one byte less than it got
	r := openDAV(t, f, nil)
	err := r.Put(context.Background(), objName, 4, strings.NewReader("abcd"))
	if code(err) != driver.ServerError {
		t.Fatalf("Put = %v", err)
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		want   driver.Code
		st     int
	}{
		{401, driver.AuthFailed, 0}, {403, driver.Forbidden, 0}, {404, driver.NotFound, 0}, {409, driver.NotFound, 0},
		{413, driver.TooLarge, 0}, {423, driver.ServerError, 423}, {405, driver.ServerError, 405}, {507, driver.QuotaExceeded, 0},
		{500, driver.ServerError, 500}, {503, driver.ServerError, 503}, {429, driver.ServerError, 429},
		{301, driver.ServerError, 301}, {302, driver.ServerError, 302}, {307, driver.ServerError, 307}, {308, driver.ServerError, 308},
	}
	for _, c := range cases {
		t.Run(fmt.Sprint(c.status), func(t *testing.T) {
			f := newFake()
			r := openDAV(t, f, map[string]any{"folder": ""})
			f.fault = func(w http.ResponseWriter, req *http.Request) bool {
				if c.status >= 300 && c.status < 400 {
					w.Header().Set("Location", "https://other.invalid/x")
				}
				davErr(w, c.status)
				return true
			}
			check := func(op string, err error) {
				t.Helper()
				if op == "Delete" && c.status == 404 {
					if err != nil {
						t.Errorf("Delete 404 = %v", err)
					}
					return
				}
				if op == "List" && c.status == 404 {
					if err != nil {
						t.Errorf("List 404 = %v", err)
					}
					return
				}
				var de *driver.Error
				if !errors.As(err, &de) || de.Code != c.want || de.Status != c.st {
					t.Errorf("%s %d = %v", op, c.status, err)
				}
				if err != nil && (strings.Contains(err.Error(), leakWords) || strings.Contains(err.Error(), davPass) || strings.Contains(err.Error(), "127.0.0.1")) {
					t.Errorf("%s leaked: %v", op, err)
				}
			}
			ctx := context.Background()
			check("Put", r.Put(ctx, objName, 1, strings.NewReader("x")))
			_, err := r.List(ctx, "launchpad-")
			check("List", err)
			_, err = r.Get(ctx, objName)
			check("Get", err)
			check("Delete", r.Delete(ctx, objName))
			if c.status == 500 || c.status == 503 {
				if !driver.Retryable(&driver.Error{Code: driver.ServerError, Status: c.status}) {
					t.Error("5xx is retried")
				}
			}
		})
	}
}

func TestPutNon2xxSuccessCodes(t *testing.T) {
	f := newFake()
	r := openDAV(t, f, map[string]any{"folder": ""})
	f.fault = func(w http.ResponseWriter, req *http.Request) bool {
		if req.Method == http.MethodPut {
			io.Copy(io.Discard, req.Body)
			w.WriteHeader(202) // accepted, not stored yet: not a success
			return true
		}
		return false
	}
	if err := r.Put(context.Background(), objName, 1, strings.NewReader("x")); code(err) != driver.ServerError {
		t.Fatalf("Put 202 = %v", err)
	}
}

func TestMkcolBaseMissing(t *testing.T) {
	f := newFake()
	f.mu.Lock()
	delete(f.dirs, strings.TrimSuffix(davBase, "/"))
	f.mu.Unlock()
	r := openDAV(t, f, nil)
	if err := r.Put(context.Background(), objName, 1, strings.NewReader("x")); code(err) != driver.NotFound {
		t.Fatalf("Put with no base = %v", err)
	}
	steps := r.Test(context.Background(), testName)
	if len(steps) != 2 || steps[0].Name != "connect" || !steps[0].OK || steps[1].Name != "auth" || steps[1].Code != driver.NotFound {
		t.Fatalf("steps = %+v", steps)
	}
}

func TestRedirectIsNotFollowedAndNoCredentialsForwarded(t *testing.T) {
	other := newFake()
	other.srv = httptest.NewTLSServer(other)
	defer other.srv.Close()
	f := newFake()
	r := openDAV(t, f, map[string]any{"folder": ""})
	f.fault = func(w http.ResponseWriter, req *http.Request) bool {
		w.Header().Set("Location", other.srv.URL+req.URL.Path)
		w.WriteHeader(307)
		return true
	}
	err := r.Put(context.Background(), objName, 1, strings.NewReader("x"))
	var de *driver.Error
	if !errors.As(err, &de) || de.Code != driver.ServerError || de.Status != 307 {
		t.Fatalf("Put = %v", err)
	}
	if driver.Retryable(err) {
		t.Fatal("a redirect is not retried")
	}
	if _, err := r.List(context.Background(), "launchpad-"); err == nil {
		t.Fatal("List followed a redirect")
	}
	if other.count() != 0 {
		t.Fatalf("the redirect target got %d request(s)", other.count())
	}
}

func TestWrongCredentials(t *testing.T) {
	f := newFake()
	base, pin := startTLS(t, f)
	r, err := Driver{}.Open(context.Background(), driver.Config{Config: cfgJSON(t, davConfig(t, base, pin, nil)), Secrets: secretsJSON("wrong"), Dialer: permitDialer()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := r.Put(context.Background(), objName, 1, strings.NewReader("x")); code(err) != driver.AuthFailed {
		t.Fatalf("Put = %v", err)
	}
	steps := r.Test(context.Background(), testName)
	if len(steps) != 2 || !steps[0].OK || steps[0].Name != "connect" || steps[1].Name != "auth" || steps[1].Code != driver.AuthFailed {
		t.Fatalf("steps = %+v", steps)
	}
	if driver.Retryable(driver.Errf(driver.AuthFailed)) {
		t.Fatal("auth failures are not retried")
	}
}

func TestSecretsRequired(t *testing.T) {
	f := newFake()
	base, pin := startTLS(t, f)
	for _, s := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`{"password":""}`), json.RawMessage(`nope`)} {
		_, err := Driver{}.Open(context.Background(), driver.Config{Config: cfgJSON(t, davConfig(t, base, pin, nil)), Secrets: s, Dialer: permitDialer()})
		if code(err) != driver.SecretsUnreadable {
			t.Errorf("secrets %q: %v", s, err)
		}
	}
	if _, err := (Driver{}).Open(context.Background(), driver.Config{Config: cfgJSON(t, davConfig(t, base, pin, nil)), Secrets: secretsJSON("x")}); err == nil {
		t.Error("Open without a dialer")
	}
}

func TestHTTPIsRefused(t *testing.T) {
	cfg := cfgJSON(t, map[string]any{"url": "http://10.0.0.5/remote.php/dav/files/alice/", "username": "alice"})
	if err := (Driver{}).Validate(cfg); err == nil {
		t.Fatal("http:// accepted by Validate")
	}
	if _, err := (Driver{}).Open(context.Background(), driver.Config{Config: cfg, Secrets: secretsJSON("x"), Dialer: permitDialer(), AllowPlainHTTP: true}); err == nil {
		t.Fatal("http:// accepted by Open")
	}
	if _, err := (Driver{}).Probe(context.Background(), driver.Config{Config: cfg, Dialer: permitDialer(), AllowPlainHTTP: true}); err == nil {
		t.Fatal("http:// accepted by Probe")
	}
}

// --- hostile PROPFIND replies ---

func resp(href string, n int) string {
	return fmt.Sprintf(`<d:response><d:href>%s</d:href><d:propstat><d:prop><d:getcontentlength>%d</d:getcontentlength><d:resourcetype/></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, href, n)
}

func TestHostileHrefs(t *testing.T) {
	f := newFake()
	r := openDAV(t, f, nil)
	put(t, r, objName, []byte("good-object"))
	col := davBase + "backups/main/"
	v2 := "launchpad-0123abcd-20260101T000000Z-0002.json"
	v3 := "launchpad-0123abcd-20260101T000000Z-0003.json"
	v4 := "launchpad-0123abcd-20260101T000000Z-0004.json"
	other := newFake()
	other.srv = httptest.NewTLSServer(other)
	defer other.srv.Close()
	host := strings.TrimPrefix(f.srv.URL, "https://")
	f.extra = strings.Join([]string{
		// accepted: a plain path, an absolute URL of THIS host, percent-encoded characters that decode to a plain name
		resp(col+v2, 7),
		resp(f.srv.URL+col+v3, 8),
		resp(strings.Replace(col, "backups", "%62ackups", 1)+strings.Replace(v4, "launchpad", "launch%70ad", 1), 9),
		// refused
		resp(col+"../../../../etc/passwd", 1),
		resp(col+"..", 1),
		resp(davBase+"launchpad-0123abcd-20260101T000000Z-0005.json", 1),      // the parent folder, not the listed one
		resp(col+"sub/launchpad-0123abcd-20260101T000000Z-0006.json", 1),      // nested
		resp(col+"launchpad-0123abcd-20260101T000000Z-0007.json%2F..%2Fx", 1), // encoded slash
		resp(col+"x%2Flaunchpad-0123abcd-20260101T000000Z-0008.json", 1),      // encoded slash in the name
		resp(strings.Replace(col, "backups/main", "backups%2Fmain", 1)+v2, 1), // encoded slash in the folder part
		resp(col+"launchpad-0123abcd-20260101T000000Z-0009.json%5Cx", 1),      // encoded backslash
		resp(col+"launchpad-0123abcd-20260101T000000Z-0010.json%00", 1),       // encoded NUL
		resp("https://evil.example"+col+"launchpad-0123abcd-20260101T000000Z-0011.json", 1),
		resp("https://"+host+".evil.example"+col+"launchpad-0123abcd-20260101T000000Z-0012.json", 1),
		resp("http://"+host+col+"launchpad-0123abcd-20260101T000000Z-0013.json", 1), // wrong scheme
		resp("https://user:pw@"+host+col+"launchpad-0123abcd-20260101T000000Z-0014.json", 1),
		resp(other.srv.URL+col+"launchpad-0123abcd-20260101T000000Z-0015.json", 1), // another host (another port)
		resp("//evil.example"+col+"launchpad-0123abcd-20260101T000000Z-0016.json", 1),
		resp(col+"launchpad-0123abcd-20260101T000000Z-0017.json?x=1", 1),
		resp(col+"launchpad-0123abcd-20260101T000000Z-0018.json#f", 1),
		resp("launchpad-0123abcd-20260101T000000Z-0019.json", 1), // relative
		resp("/", 1),
		resp("", 1),
		resp(col+"notes.txt", 1),
		resp(col+"launchpad-0123abcd-20260101T000000Z-0021.json%0A", 1),
		resp(col+"launchpad-0123abcd-20260101T000000Z-0022.json\\..\\x", 1),
	}, "")
	es, err := r.List(context.Background(), "launchpad-0123abcd-")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range es {
		got[e.Name] = true
	}
	want := map[string]bool{objName: true, v2: true, v3: true, v4: true}
	if len(got) != len(want) {
		t.Fatalf("List = %v", got)
	}
	for k := range want {
		if !got[k] {
			t.Fatalf("List = %v, missing %s", got, k)
		}
	}
	// Reading and deleting every listed name builds the URL from the name, and no
	// request goes to another host or another path.
	n0 := f.count()
	for _, e := range es {
		_ = r.Delete(context.Background(), e.Name)
	}
	for _, q := range f.requests()[n0:] {
		if !strings.HasPrefix(q, "DELETE "+col+"launchpad-0123abcd-") {
			t.Errorf("unexpected request %q", q)
		}
	}
	if other.count() != 0 {
		t.Fatalf("a request went to the other host (%d)", other.count())
	}
	for _, q := range f.requests() {
		if strings.Contains(q, "..") || strings.Contains(q, "etc/passwd") || strings.Contains(q, "evil") {
			t.Errorf("hostile request %q", q)
		}
	}
}

func TestSelfAndCollectionsAreNotListed(t *testing.T) {
	f := newFake()
	r := openDAV(t, f, nil)
	put(t, r, objName, []byte("x"))
	f.extra = strings.Replace(dirResp(davBase+"backups/main/launchpad-0123abcd-20260101T000000Z-0002.json"), "/</d:href>", "</d:href>", 1) // collection, href without slash
	es, _ := r.List(context.Background(), "launchpad-")
	if len(es) != 1 || es[0].Name != objName {
		t.Fatalf("List = %v", es)
	}
}

func TestListBodyRules(t *testing.T) {
	multi := func(s string) func(w http.ResponseWriter, req *http.Request) bool {
		return func(w http.ResponseWriter, req *http.Request) bool {
			if req.Method != "PROPFIND" || req.Header.Get("Depth") != "1" {
				return false
			}
			w.WriteHeader(207)
			_, _ = w.Write([]byte(s))
			return true
		}
	}
	cases := map[string]struct {
		fault func(w http.ResponseWriter, req *http.Request) bool
		want  driver.Code
	}{
		"doctype":             {multi(`<?xml version="1.0"?><!DOCTYPE x [<!ENTITY a "aaaa">]><d:multistatus xmlns:d="DAV:"><d:response><d:href>&a;</d:href></d:response></d:multistatus>`), driver.ServerError},
		"entity":              {multi(`<d:multistatus xmlns:d="DAV:"><!ENTITY a "b"></d:multistatus>`), driver.ServerError},
		"doctype upper-mixed": {multi(`<!DocType x><d:multistatus xmlns:d="DAV:"></d:multistatus>`), driver.ServerError},
		"undefined entity":    {multi(`<d:multistatus xmlns:d="DAV:"><d:response><d:href>&xxe;</d:href></d:response></d:multistatus>`), driver.ServerError},
		"malformed":           {multi(`<d:multistatus xmlns:d="DAV:"><d:response>`), driver.ServerError},
		"not xml":             {multi(`hello`), driver.ServerError},
		"wrong root":          {multi(`<d:other xmlns:d="DAV:"/>`), driver.ServerError},
		"wrong namespace":     {multi(`<multistatus><response><href>/x</href></response></multistatus>`), driver.ServerError},
		"plain 200": {func(w http.ResponseWriter, req *http.Request) bool {
			if req.Method != "PROPFIND" {
				return false
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`<d:multistatus xmlns:d="DAV:"/>`))
			return true
		}, driver.ServerError},
		"huge": {func(w http.ResponseWriter, req *http.Request) bool {
			if req.Method != "PROPFIND" {
				return false
			}
			w.WriteHeader(207)
			_, _ = w.Write([]byte(`<d:multistatus xmlns:d="DAV:">`))
			chunk := bytes.Repeat([]byte(" "), 1<<20)
			for i := 0; i < 17; i++ {
				if _, err := w.Write(chunk); err != nil {
					return true
				}
			}
			return true
		}, driver.TooLarge},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			r := openDAV(t, f, nil)
			put(t, r, objName, []byte("x"))
			f.fault = c.fault
			es, err := r.List(context.Background(), "launchpad-")
			if code(err) != c.want || len(es) != 0 {
				t.Fatalf("List = %v, %v", es, err)
			}
		})
	}
	t.Run("deep nesting", func(t *testing.T) {
		f := newFake()
		r := openDAV(t, f, nil)
		put(t, r, objName, []byte("x"))
		f.fault = multi(`<d:multistatus xmlns:d="DAV:"><d:response>` + strings.Repeat("<a>", 50000) + strings.Repeat("</a>", 50000) + `</d:response></d:multistatus>`)
		es, err := r.List(context.Background(), "launchpad-")
		if len(es) != 0 {
			t.Fatalf("List = %v, %v", es, err)
		}
	})
}

func TestPropstatStatus(t *testing.T) {
	f := newFake()
	r := openDAV(t, f, nil)
	col := davBase + "backups/main/"
	put(t, r, objName, []byte("12345"))
	// A size reported under a 404 propstat is not a size.
	f.extra = `<d:response><d:href>` + col + `launchpad-0123abcd-20260101T000000Z-0002.json</d:href><d:propstat><d:prop><d:getcontentlength>99</d:getcontentlength></d:prop><d:status>HTTP/1.1 404 Not Found</d:status></d:propstat></d:response>` +
		resp(col+"launchpad-0123abcd-20260101T000000Z-0003.json", -5)
	es, _ := r.List(context.Background(), "launchpad-")
	sizes := map[string]int64{}
	for _, e := range es {
		sizes[e.Name] = e.Size
	}
	if sizes[objName] != 5 || sizes["launchpad-0123abcd-20260101T000000Z-0002.json"] != -1 || sizes["launchpad-0123abcd-20260101T000000Z-0003.json"] != -1 {
		t.Fatalf("sizes = %v", sizes)
	}
}

// --- test steps ---

func TestTestStepFailures(t *testing.T) {
	cases := []struct {
		name  string
		fault func(w http.ResponseWriter, r *http.Request) bool
		step  string
		want  driver.Code
	}{
		{"read-only account", func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == http.MethodPut || r.Method == "MKCOL" {
				davErr(w, 403)
				return true
			}
			return false
		}, "write", driver.Forbidden},
		{"quota", func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == http.MethodPut {
				io.Copy(io.Discard, r.Body)
				davErr(w, 507)
				return true
			}
			return false
		}, "write", driver.QuotaExceeded},
		{"locked", func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == http.MethodPut {
				davErr(w, 423)
				return true
			}
			return false
		}, "write", driver.ServerError},
		{"cannot read back", func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == http.MethodGet {
				davErr(w, 403)
				return true
			}
			return false
		}, "read", driver.Forbidden},
		{"wrong content", func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == http.MethodGet {
				w.Header().Set("Content-Length", "3")
				_, _ = w.Write([]byte("bad"))
				return true
			}
			return false
		}, "read", driver.ServerError},
		{"cannot delete", func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == http.MethodDelete {
				davErr(w, 403)
				return true
			}
			return false
		}, "delete", driver.Forbidden},
		{"listing hides the file", func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == "PROPFIND" && r.Header.Get("Depth") == "1" {
				w.WriteHeader(207)
				_, _ = w.Write([]byte(`<d:multistatus xmlns:d="DAV:"/>`))
				return true
			}
			return false
		}, "read", driver.NotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake()
			r := openDAV(t, f, nil)
			f.fault = c.fault
			steps := r.Test(context.Background(), testName)
			last := steps[len(steps)-1]
			if last.OK || last.Name != c.step || last.Code != c.want {
				t.Fatalf("steps = %+v", steps)
			}
			for _, s := range steps[:len(steps)-1] {
				if !s.OK {
					t.Fatalf("an earlier step failed: %+v", steps)
				}
			}
		})
	}
}

// --- timeouts ---

func TestStall(t *testing.T) {
	old := stallAfter
	stallAfter = 300 * time.Millisecond
	defer func() { stallAfter = old }()

	t.Run("server stops reading an upload", func(t *testing.T) {
		f := newFake()
		release := make(chan struct{})
		r := openDAV(t, f, map[string]any{"folder": ""})
		f.fault = func(w http.ResponseWriter, req *http.Request) bool {
			if req.Method == http.MethodPut {
				w.WriteHeader(100) // accepts the body, then never reads it
				<-release
				return true
			}
			return false
		}
		defer close(release)
		const size = 256 << 20
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
		r := openDAV(t, f, nil)
		f.fault = func(w http.ResponseWriter, req *http.Request) bool { <-release; return true }
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
		r := openDAV(t, f, nil)
		f.fault = func(w http.ResponseWriter, req *http.Request) bool {
			w.Header().Set("Content-Length", "1000")
			w.WriteHeader(200)
			_, _ = w.Write([]byte("abc"))
			w.(http.Flusher).Flush()
			<-release
			return true
		}
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
		if _, err = io.ReadAll(rc); code(err) != driver.Stalled {
			t.Fatalf("stalled download = %v", err)
		}
	})
	t.Run("slow but moving is fine", func(t *testing.T) {
		// Fake time (LAUN-175): 5 reads, 100 ms apart, is 500 ms against a 300 ms
		// stall time. Only a touch on every read keeps the upload alive.
		clk := &fakeClock{}
		oldT := newStallTimer
		newStallTimer = clk.newTimer
		defer func() { newStallTimer = oldT }()
		f := newFake()
		r := openDAV(t, f, nil)
		if err := r.Put(context.Background(), objName, 5, &slowReader{n: 5, gap: 100 * time.Millisecond, clk: clk}); err != nil {
			t.Fatalf("a moving upload stalled: %v", err)
		}
		if clk.resets() < 5 {
			t.Fatalf("only %d progress touches for 5 reads", clk.resets())
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
	r := openDAV(t, f, nil)
	f.fault = func(w http.ResponseWriter, req *http.Request) bool {
		c, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			c.Close()
		}
		return true
	}
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
	r := openDAV(t, f, nil)
	f.fault = func(w http.ResponseWriter, req *http.Request) bool { <-release; return true }
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := r.List(ctx, "launchpad-"); code(err) != driver.Timeout {
		t.Fatalf("List = %v", err)
	}
}

// --- network rules ---

func TestPrivateAddressRefusedByPolicy(t *testing.T) {
	f := newFake()
	base, pin := startTLS(t, f)
	pol, err := backup.Policy("", backup.Gateways{Read: func() ([]netip.Addr, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	c := driver.Config{Config: cfgJSON(t, davConfig(t, base, pin, nil)), Secrets: secretsJSON(davPass), Dialer: backup.NewDialer(pol, 2*time.Second)}
	r, err := Driver{}.Open(context.Background(), c)
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
	if _, err := (Driver{}).Probe(context.Background(), c); code(err) != driver.SSRFBlocked {
		t.Fatalf("Probe = %v", err)
	}
	if f.count() != 0 {
		t.Fatal("a request reached the refused address")
	}
	c.Config = cfgJSON(t, map[string]any{"url": "https://postgres:5432/dav/", "username": "alice"})
	r2, _ := Driver{}.Open(context.Background(), c)
	if _, err := r2.List(context.Background(), "launchpad-"); code(err) != driver.SSRFBlocked {
		t.Fatalf("service name = %v", err)
	}
}

func TestProxyEnvironmentIsIgnored(t *testing.T) {
	var hits atomic.Int32
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer px.Close()
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		t.Setenv(k, px.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	f := newFake()
	base, pin := startTLS(t, f)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(f.srv.URL, "https://"))
	res := &netguardtest.MapResolver{Hosts: map[string][]string{"dav.fake.test": {"127.0.0.1"}}}
	d := &netguard.Dialer{Policy: netguardtest.Permit(netguard.Policy{}, "127.0.0.1"), Resolver: res, Timeout: 5 * time.Second}
	_ = base
	cfg := cfgJSON(t, davConfig(t, "https://dav.fake.test:"+port+davBase, pin, nil))
	r, err := Driver{}.Open(context.Background(), driver.Config{Config: cfg, Secrets: secretsJSON(davPass), Dialer: d})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if rm, ok := r.(*remote); !ok || rm.tr.Proxy != nil {
		t.Fatal("the transport has a Proxy function")
	}
	put(t, r, objName, []byte("x"))
	if hits.Load() != 0 {
		t.Fatal("the proxy from the environment was used")
	}
	if !f.has(davBase + "backups/main/" + objName) {
		t.Fatal("the object did not reach the server")
	}
}

// --- TLS ---

func tlsRemote(t *testing.T, base, pin string) driver.Remote {
	t.Helper()
	r, err := Driver{}.Open(context.Background(), driver.Config{Config: cfgJSON(t, davConfig(t, base, pin, nil)), Secrets: secretsJSON(davPass), Dialer: permitDialer()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestTLSPin(t *testing.T) {
	f := newFake()
	base, pin := startTLS(t, f)
	t.Run("matching pin works with a certificate no root trusts", func(t *testing.T) {
		r := tlsRemote(t, base, pin)
		put(t, r, objName, []byte("pinned"))
		for _, s := range r.Test(context.Background(), testName) {
			if !s.OK {
				t.Fatalf("step %+v", s)
			}
		}
	})
	t.Run("wrong pin", func(t *testing.T) {
		n0 := f.count()
		r := tlsRemote(t, base, strings.Repeat("0", 64))
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
		if f.count() != n0 {
			t.Fatal("a request (and its password) reached a server with the wrong certificate")
		}
	})
	t.Run("no pin: the chain is checked", func(t *testing.T) {
		n0 := f.count()
		r := tlsRemote(t, base, "")
		if _, err := r.List(context.Background(), "launchpad-"); code(err) != driver.TLSUntrusted {
			t.Fatalf("List = %v", err)
		}
		if f.count() != n0 {
			t.Fatal("a request reached an untrusted server")
		}
	})
	t.Run("a pin does not skip the expiry date", func(t *testing.T) {
		old := clockNow
		clockNow = func() time.Time { return time.Now().AddDate(100, 0, 0) }
		defer func() { clockNow = old }()
		r := tlsRemote(t, base, pin)
		if _, err := r.List(context.Background(), "launchpad-"); code(err) != driver.TLSUntrusted {
			t.Fatalf("List = %v", err)
		}
	})
	t.Run("probe sends no request", func(t *testing.T) {
		n0 := f.count()
		pr, err := Driver{}.Probe(context.Background(), driver.Config{Config: cfgJSON(t, davConfig(t, base, "", nil)), Dialer: permitDialer()})
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
		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()
		u := "https://" + strings.TrimPrefix(srv.URL, "http://") + davBase
		_, err := Driver{}.Probe(context.Background(), driver.Config{Config: cfgJSON(t, davConfig(t, u, "", nil)), Dialer: permitDialer()})
		if _, ok := err.(*driver.Error); !ok {
			t.Fatalf("Probe = %v (%T)", err, err)
		}
	})
}

func TestProbeConnectionRefused(t *testing.T) {
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	_, err := Driver{}.Probe(context.Background(), driver.Config{Config: cfgJSON(t, davConfig(t, "https://"+addr+davBase, "", nil)), Dialer: permitDialer()})
	if code(err) != driver.ConnectRefused {
		t.Fatalf("Probe = %v", err)
	}
}

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

// A real chain and host name check with a test CA (no pin), including the SNI
// name the server sees.
func TestChainAndHostName(t *testing.T) {
	run := func(t *testing.T, dns string) (driver.Remote, *fakeDAV, string) {
		cert, pool, _ := leafFor(t, dns)
		f := newFake()
		f.srv = httptest.NewUnstartedServer(f)
		f.srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
		f.srv.StartTLS()
		t.Cleanup(f.srv.Close)
		old := testRoots
		testRoots = pool
		t.Cleanup(func() { testRoots = old })
		_, port, _ := net.SplitHostPort(strings.TrimPrefix(f.srv.URL, "https://"))
		res := &netguardtest.MapResolver{Hosts: map[string][]string{"dav.fake.test": {"127.0.0.1"}}}
		d := &netguard.Dialer{Policy: netguardtest.Permit(netguard.Policy{}, "127.0.0.1"), Resolver: res, Timeout: 5 * time.Second}
		u := "https://dav.fake.test:" + port + davBase
		r, err := Driver{}.Open(context.Background(), driver.Config{Config: cfgJSON(t, davConfig(t, u, "", nil)), Secrets: secretsJSON(davPass), Dialer: d})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Close() })
		return r, f, u
	}
	t.Run("right name", func(t *testing.T) {
		r, _, u := run(t, "dav.fake.test")
		put(t, r, objName, []byte("x"))
		res := &netguardtest.MapResolver{Hosts: map[string][]string{"dav.fake.test": {"127.0.0.1"}}}
		d := &netguard.Dialer{Policy: netguardtest.Permit(netguard.Policy{}, "127.0.0.1"), Resolver: res, Timeout: 5 * time.Second}
		pr, err := Driver{}.Probe(context.Background(), driver.Config{Config: cfgJSON(t, davConfig(t, u, "", nil)), Dialer: d})
		if err != nil || !pr.ChainVerified {
			t.Fatalf("Probe = %+v, %v", pr, err)
		}
	})
	t.Run("wrong name", func(t *testing.T) {
		r, f, _ := run(t, "other.fake.test")
		if _, err := r.List(context.Background(), "launchpad-"); code(err) != driver.TLSWrongHost {
			t.Fatalf("List = %v", err)
		}
		if f.count() != 0 {
			t.Fatal("a request reached a server with the wrong name")
		}
	})
}

// --- memory ---

func TestPutStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	const size = 192 << 20
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "MKCOL":
			w.WriteHeader(201)
		case http.MethodPut:
			n, _ := io.Copy(io.Discard, r.Body)
			if n != size {
				w.WriteHeader(400)
				return
			}
			w.WriteHeader(201)
		case "PROPFIND":
			w.WriteHeader(207)
			fmt.Fprint(w, `<d:multistatus xmlns:d="DAV:">`+fileResp(r.URL.Path, size)+`</d:multistatus>`)
		}
	}))
	defer srv.Close()
	sum := sha256.Sum256(srv.Certificate().RawSubjectPublicKeyInfo)
	cfg := cfgJSON(t, davConfig(t, srv.URL+davBase, hex.EncodeToString(sum[:]), nil))
	r, err := Driver{}.Open(context.Background(), driver.Config{Config: cfg, Secrets: secretsJSON(davPass), Dialer: permitDialer()})
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

// An early 507 stops the upload before the body is sent (Expect: 100-continue).
func TestEarlyRejectDoesNotSendBody(t *testing.T) {
	var body atomic.Int64
	f := newFake()
	r := openDAV(t, f, map[string]any{"folder": ""})
	f.fault = func(w http.ResponseWriter, req *http.Request) bool {
		if req.Method == http.MethodPut {
			if req.Header.Get("Expect") != "100-continue" {
				body.Store(-1)
			}
			davErr(w, 507)
			return true
		}
		return false
	}
	const size = 64 << 20
	src := &countReader{n: size}
	err := r.Put(context.Background(), objName, size, src)
	if code(err) != driver.QuotaExceeded {
		t.Fatalf("Put = %v", err)
	}
	if body.Load() == -1 {
		t.Fatal("no Expect header")
	}
	if src.read.Load() >= size {
		t.Fatalf("the whole body (%d bytes) was sent to a server that said 507 first", src.read.Load())
	}
}

type countReader struct {
	n    int64
	read atomic.Int64
}

func (c *countReader) Read(b []byte) (int, error) {
	left := c.n - c.read.Load()
	if left <= 0 {
		return 0, io.EOF
	}
	if int64(len(b)) > left {
		b = b[:left]
	}
	clear(b)
	c.read.Add(int64(len(b)))
	return len(b), nil
}

func TestPathsAreEscaped(t *testing.T) {
	f := newFake()
	base := "/remote.php/dav/files/Alice Smith@example.com/"
	f.dirs["/remote.php/dav/files/Alice Smith@example.com"] = true
	sb, pin := startTLS(t, f)
	_ = sb
	cfg := cfgJSON(t, map[string]any{"url": f.srv.URL + "/remote.php/dav/files/Alice%20Smith%40example.com/", "username": davUser, "tlsPin": pin, "folder": "bk"})
	r, err := Driver{}.Open(context.Background(), driver.Config{Config: cfg, Secrets: secretsJSON(davPass), Dialer: permitDialer()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	put(t, r, objName, []byte("x"))
	if !f.has(base + "bk/" + objName) {
		t.Fatalf("object missing; requests: %v", f.requests())
	}
	es, err := r.List(context.Background(), "launchpad-")
	if err != nil || len(es) != 1 {
		t.Fatalf("List = %v, %v", es, err)
	}
}

// Every backup run opens a new Remote, so its first Put sends MKCOL for folders
// that already exist (405). That is the normal path after the first run.
func TestExistingFolderLevels(t *testing.T) {
	t.Run("whole folder exists", func(t *testing.T) {
		f := newFake()
		f.dirs[davBase+"backups"] = true
		f.dirs[davBase+"backups/main"] = true
		r := openDAV(t, f, nil)
		put(t, r, objName, []byte("x"))
		if !f.has(davBase + "backups/main/" + objName) {
			t.Fatal("object missing")
		}
	})
	t.Run("upper level exists, lower does not", func(t *testing.T) {
		f := newFake()
		f.dirs[davBase+"backups"] = true
		r := openDAV(t, f, nil)
		put(t, r, objName, []byte("x"))
		if !f.has(davBase + "backups/main/" + objName) {
			t.Fatal("object missing")
		}
	})
	t.Run("second Remote on the same server", func(t *testing.T) {
		f := newFake()
		r1 := openDAV(t, f, nil)
		put(t, r1, objName, []byte("x"))
		base, pin := f.srv.URL+davBase, ""
		sum := sha256.Sum256(f.srv.Certificate().RawSubjectPublicKeyInfo)
		pin = hex.EncodeToString(sum[:])
		r2 := tlsRemote(t, base, pin)
		put(t, r2, manName, []byte("{}"))
	})
}

// lateReader gives head, holds the next byte back for a while (so a server that
// answers on the declared bytes alone has time to do it), then gives tail. net/http
// probes for an extra body byte only after it has written the declared bytes, and
// such a server used to win that race: a long source "succeeded" (LAUN-156).
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
	r := openDAV(t, f, nil)
	for i := 0; i < 2; i++ {
		if err := r.Put(context.Background(), objName, 3, &lateReader{head: "abc", tail: "defgh"}); err == nil {
			t.Fatal("a source longer than its declared size succeeded")
		}
	}
	f.mu.Lock()
	n := len(f.files)
	f.mu.Unlock()
	if n != 0 {
		t.Fatalf("a long source left %d stored file(s)", n)
	}
}

func TestSizeZeroSource(t *testing.T) {
	f := newFake()
	r := openDAV(t, f, nil)
	err := r.Put(context.Background(), objName, 0, strings.NewReader("x"))
	if err == nil || code(err) != driver.ServerError {
		t.Fatalf("a non-empty source with declared size 0: %v", err)
	}
	f.mu.Lock()
	n := len(f.files)
	f.mu.Unlock()
	if n != 0 {
		t.Fatalf("a non-empty source was stored as an empty object (%d files)", n)
	}
	if err := r.Put(context.Background(), objName, 0, strings.NewReader("")); err != nil {
		t.Fatalf("an empty source with declared size 0: %v", err)
	}
}
