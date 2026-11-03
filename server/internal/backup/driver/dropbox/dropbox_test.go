package dropbox

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"devops/internal/backup/driver"
	"devops/internal/netguard"
	"devops/internal/netguard/netguardtest"
)

const (
	objName  = "launchpad-0123abcd-20261026T031500Z-abcd.dump.age"
	manName  = "launchpad-0123abcd-20261026T031500Z-abcd.json"
	testName = "launchpad-0123abcd-test-0011223344556677.tmp"
)

func permitPolicy() netguard.Policy {
	p := netguardtest.Permit(netguard.Policy{}, "127.0.0.1")
	p.Private = []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}
	return p
}

func dialer(p netguard.Policy) *netguard.Dialer {
	return &netguard.Dialer{Policy: p, Timeout: 5 * time.Second}
}

func code(err error) driver.Code {
	if err == nil {
		return ""
	}
	return driver.CodeOf(err)
}

// env starts the fake over TLS, points the driver's two hosts at it and records
// the waits the driver asks for (no real sleeping).
type env struct {
	f      *fakeDropbox
	waits  []time.Duration
	now    atomic.Int64 // seconds added to the driver's clock
	pool   *x509.CertPool
	secret map[string]string
}

func setup(t *testing.T, f *fakeDropbox) *env {
	t.Helper()
	f.startServers(httptest.NewTLSServer)
	t.Cleanup(f.api.Close)
	t.Cleanup(f.content.Close)
	e := &env{f: f, pool: x509.NewCertPool()}
	e.pool.AddCert(f.api.Certificate())
	oldAPI, oldContent, oldRoots, oldSleep, oldNow, oldChunk, oldStall := apiBase, contentBase, testRoots, sleep, clockNow, chunkSize, stallAfter
	apiBase, contentBase, testRoots = f.api.URL, f.content.URL, e.pool
	sleep = func(ctx context.Context, d time.Duration) error {
		e.waits = append(e.waits, d)
		return ctx.Err()
	}
	clockNow = func() time.Time { return time.Now().Add(time.Duration(e.now.Load()) * time.Second) }
	t.Cleanup(func() {
		apiBase, contentBase, testRoots, sleep, clockNow, chunkSize, stallAfter = oldAPI, oldContent, oldRoots, oldSleep, oldNow, oldChunk, oldStall
	})
	return e
}

func (e *env) secrets() json.RawMessage {
	b, _ := json.Marshal(map[string]string{"appKey": e.f.appKey, "appSecret": e.f.appSecret, "refreshToken": e.f.refresh})
	return b
}

func (e *env) cfg(t testing.TB, extra map[string]any) driver.Config {
	t.Helper()
	m := map[string]any{"folder": "backups/main"}
	for k, v := range extra {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return driver.Config{Config: b, Secrets: e.secrets(), Dialer: dialer(permitPolicy())}
}

func (e *env) open(t *testing.T, extra map[string]any) driver.Remote {
	t.Helper()
	r, err := Driver{}.Open(context.Background(), e.cfg(t, extra))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func fresh(t *testing.T) (*env, driver.Remote) {
	t.Helper()
	e := setup(t, newFake())
	return e, e.open(t, nil)
}

func put(t *testing.T, r driver.Remote, name string, b []byte) {
	t.Helper()
	if err := r.Put(context.Background(), name, int64(len(b)), bytes.NewReader(b)); err != nil {
		t.Fatalf("Put %s: %v", name, err)
	}
}

func payload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i>>8)
	}
	return b
}

func TestRegistered(t *testing.T) {
	d, ok := driver.Lookup(Type)
	if !ok {
		t.Fatal("dropbox is not registered")
	}
	if _, ok := d.(driver.Exchanger); !ok {
		t.Fatal("the dropbox driver must implement driver.Exchanger")
	}
}

func TestValidateAndIdentity(t *testing.T) {
	good := []string{
		`{}`, `{"folder":"a/b-c_d.e"}`, `{"access":"app_folder"}`, `{"access":"full_dropbox","folder":"launchpad"}`,
		`{"tlsPin":"` + strings.Repeat("ab", 32) + `"}`,
	}
	for _, g := range good {
		if err := (Driver{}).Validate(json.RawMessage(g)); err != nil {
			t.Errorf("good %s: %v", g, err)
		}
	}
	bad := []string{
		`{"access":"full_dropbox"}`, `{"access":"team"}`, `{"folder":"../x"}`, `{"folder":"a/../b"}`, `{"folder":"/a"}`,
		`{"folder":"a/"}`, `{"folder":"a//b"}`, `{"folder":"a b"}`, `{"folder":"a."}`, `{"folder":"a..b"}`,
		`{"folder":"` + strings.Repeat("a/", 120) + `"}`, `{"tlsPin":"ABC"}`, `{"tlsPin":"` + strings.Repeat("AB", 32) + `"}`,
		`{"endpoint":"https://evil.example.com"}`, `{"unknown":1}`, `{"hostKey":"x"}`, `{} {}`, ``, `[]`,
	}
	for _, b := range bad {
		if err := (Driver{}).Validate(json.RawMessage(b)); err == nil {
			t.Errorf("bad %s accepted", b)
		}
	}
	a, _ := Driver{}.Identity(json.RawMessage(`{"folder":"Backups/Main"}`))
	b, _ := Driver{}.Identity(json.RawMessage(`{"access":"app_folder","folder":"backups/main"}`))
	c, _ := Driver{}.Identity(json.RawMessage(`{"access":"full_dropbox","folder":"backups/main"}`))
	if a != b {
		t.Errorf("identity differs only in case: %+v %+v", a, b)
	}
	if a == c || a.Type != "dropbox" {
		t.Errorf("app folder and full dropbox must differ: %+v %+v", a, c)
	}
	if _, err := (Driver{}).Identity(json.RawMessage(`{"access":"x"}`)); err == nil {
		t.Error("identity of a bad config")
	}
}

func TestSecretsRequired(t *testing.T) {
	e := setup(t, newFake())
	for _, s := range []string{``, `{}`, `{"appKey":"a","appSecret":"b"}`, `{"appKey":"a","refreshToken":"c"}`,
		`{"appKey":"a b","appSecret":"b","refreshToken":"c"}`, `{"appKey":"a","appSecret":"b","refreshToken":"c\nd"}`} {
		c := e.cfg(t, nil)
		c.Secrets = json.RawMessage(s)
		if _, err := (Driver{}).Open(context.Background(), c); code(err) != driver.SecretsUnreadable {
			t.Errorf("secrets %q: %v", s, err)
		}
	}
	c := e.cfg(t, nil)
	c.Dialer = nil
	if _, err := (Driver{}).Open(context.Background(), c); err == nil {
		t.Error("Open without a dialer")
	}
}

func TestContract(t *testing.T) {
	e, r := fresh(t)
	ctx := context.Background()
	if es, err := r.List(ctx, "launchpad-"); err != nil || len(es) != 0 {
		t.Fatalf("empty list: %v %v", es, err)
	}
	obj := payload(1500)
	put(t, r, objName, obj)
	put(t, r, manName, []byte(`{"m":1}`))
	if got := e.f.get("/backups/main/" + objName); !bytes.Equal(got, obj) {
		t.Fatal("stored bytes differ")
	}
	es, err := r.List(ctx, "launchpad-0123abcd-")
	if err != nil || len(es) != 2 {
		t.Fatalf("list: %v %v", es, err)
	}
	for _, en := range es {
		if en.Name == objName && en.Size != int64(len(obj)) {
			t.Errorf("size %d", en.Size)
		}
	}
	if es, _ := r.List(ctx, "launchpad-ffffffff-"); len(es) != 0 {
		t.Errorf("other install's prefix: %v", es)
	}
	rc, err := r.Get(ctx, objName)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, obj) {
		t.Fatalf("get: %v", err)
	}
	if _, err := r.Get(ctx, "launchpad-0123abcd-20261026T031500Z-eeee.dump.age"); code(err) != driver.NotFound {
		t.Errorf("get missing: %v", err)
	}
	if err := r.Delete(ctx, objName); err != nil {
		t.Fatal(err)
	}
	if e.f.has("/backups/main/" + objName) {
		t.Error("not deleted")
	}
	if err := r.Delete(ctx, objName); err != nil {
		t.Errorf("delete of a missing file must not fail: %v", err)
	}
	// overwrite: the same name again replaces it
	put(t, r, manName, []byte(`{"m":2}`))
	if got := e.f.get("/backups/main/" + manName); string(got) != `{"m":2}` {
		t.Errorf("overwrite: %q", got)
	}
	// zero bytes
	put(t, r, "launchpad-0123abcd-20261026T031500Z-0000.json", nil)
}

func TestFolderRootAndFullAccess(t *testing.T) {
	f := newFake()
	f.root = ""
	e := setup(t, f)
	r, err := Driver{}.Open(context.Background(), driver.Config{Config: json.RawMessage(`{}`), Secrets: e.secrets(), Dialer: dialer(permitPolicy())})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	put(t, r, manName, []byte("x"))
	if !f.has("/" + manName) {
		t.Fatalf("root path: %v", f.files)
	}
	es, err := r.List(context.Background(), "launchpad-")
	if err != nil || len(es) != 1 {
		t.Fatalf("list root: %v %v", es, err)
	}
	// full access with a folder
	f2 := newFake()
	e2 := setup(t, f2)
	r2 := e2.open(t, map[string]any{"access": "full_dropbox", "folder": "deep/er"})
	f2.root = "/deep/er"
	put(t, r2, manName, []byte("y"))
	if !f2.has("/deep/er/" + manName) {
		t.Fatalf("full path: %v", f2.files)
	}
}

func TestNamesAreChecked(t *testing.T) {
	e, r := fresh(t)
	ctx := context.Background()
	bad := []string{"", "../x", "a/b", "/etc/passwd", "launchpad-0123abcd-20261026T031500Z-abcd.dump.age/../../x", "x.dump.age", "launchpad-0123abcd-2026"}
	before := e.f.total()
	for _, n := range bad {
		if err := r.Put(ctx, n, 1, strings.NewReader("x")); code(err) != driver.NotFound {
			t.Errorf("Put %q: %v", n, err)
		}
		if _, err := r.Get(ctx, n); code(err) != driver.NotFound {
			t.Errorf("Get %q: %v", n, err)
		}
		if err := r.Delete(ctx, n); code(err) != driver.NotFound {
			t.Errorf("Delete %q: %v", n, err)
		}
	}
	for _, p := range []string{"a/b", "../", "x y", strings.Repeat("a", 200), "%2e"} {
		if _, err := r.List(ctx, p); code(err) != driver.NotFound {
			t.Errorf("List prefix %q: %v", p, err)
		}
	}
	if e.f.total() != before {
		t.Errorf("a refused name made %d requests", e.f.total()-before)
	}
}

func TestListFiltersForeignAndPaginates(t *testing.T) {
	f := newFake()
	f.pageSize = 2
	e := setup(t, f)
	r := e.open(t, nil)
	mine := []string{}
	for i := 0; i < 7; i++ {
		n := fmt.Sprintf("launchpad-0123abcd-20261026T03%02d00Z-abcd.dump.age", i)
		mine = append(mine, n)
		f.put("/backups/main/"+n, []byte("x"))
	}
	f.put("/backups/main/notes.txt", []byte("x"))
	for _, n := range []string{
		"launchpad-0123abcd-20261026T031500Z-abcd.dump.age.exe", "launchpad-0123abcd-zzz",
		"launchpad-0123abcd-20261026T031500Z-abcd.dump.age x", "launchpad-0123abcd-20261026T031500Z-ABCD.dump.age",
		"launchpad-0123abcd-test-zz.tmp",
	} {
		f.put("/backups/main/"+n, []byte("x")) // match the prefix, fail remotename.Valid
	}
	f.extras = []map[string]any{
		{".tag": "folder", "name": "launchpad-0123abcd-20261026T031500Z-ffff.dump.age"},             // a folder named like a file
		{".tag": "deleted", "name": "launchpad-0123abcd-20261026T031500Z-eeee.dump.age"},            // deleted
		{".tag": "file", "name": "../launchpad-0123abcd-20261026T031500Z-dddd.dump.age", "size": 1}, // hostile
		{".tag": "file", "name": "a/launchpad-0123abcd-20261026T031500Z-cccc.dump.age", "size": 1},
	}
	es, err := r.List(context.Background(), "launchpad-0123abcd-")
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != len(mine) {
		t.Fatalf("got %d entries %v", len(es), es)
	}
	seen := map[string]bool{}
	for _, en := range es {
		seen[en.Name] = true
	}
	for _, n := range mine {
		if !seen[n] {
			t.Errorf("missing %s", n)
		}
	}
	// 7 files + 6 extras/foreign = 13 entries over pages of 2: the continue call is used with cursors
	if f.count("/2/files/list_folder") != 1 || f.count("/2/files/list_folder/continue") < 5 {
		t.Errorf("calls: list=%d continue=%d", f.count("/2/files/list_folder"), f.count("/2/files/list_folder/continue"))
	}
}

func TestListLimits(t *testing.T) {
	t.Run("a cursor that never ends", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		r := e.open(t, nil)
		n := 0
		f.fault = func(ep string, _ int, w http.ResponseWriter, _ *http.Request) bool {
			if ep != "/2/files/list_folder" && ep != "/2/files/list_folder/continue" {
				return false
			}
			n++
			fmt.Fprintf(w, `{"entries":[],"cursor":"c%d","has_more":true}`, n)
			return true
		}
		// authenticate first so the fault does not hide the token call
		if _, err := r.List(context.Background(), "launchpad-"); code(err) != driver.TooLarge {
			t.Errorf("endless cursor: %v", err)
		}
		if n != maxPages {
			t.Errorf("pages = %d", n)
		}
	})
	t.Run("a repeated cursor", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		r := e.open(t, nil)
		f.fault = func(ep string, _ int, w http.ResponseWriter, _ *http.Request) bool {
			if ep == "/2/files/list_folder" || ep == "/2/files/list_folder/continue" {
				fmt.Fprint(w, `{"entries":[],"cursor":"same","has_more":true}`)
				return true
			}
			return false
		}
		if _, err := r.List(context.Background(), "launchpad-"); code(err) != driver.ServerError {
			t.Errorf("repeated cursor: %v", err)
		}
	})
	t.Run("has_more without a cursor, a huge cursor, bad JSON", func(t *testing.T) {
		for _, body := range []string{`{"entries":[],"has_more":true}`, `{"entries":[],"cursor":"` + strings.Repeat("x", maxCursorLen+1) + `","has_more":true}`, `not json`} {
			f := newFake()
			e := setup(t, f)
			r := e.open(t, nil)
			f.fault = func(ep string, _ int, w http.ResponseWriter, _ *http.Request) bool {
				if ep == "/2/files/list_folder" {
					fmt.Fprint(w, body)
					return true
				}
				return false
			}
			if _, err := r.List(context.Background(), "launchpad-"); code(err) != driver.ServerError {
				t.Errorf("%.30s: %v", body, err)
			}
		}
	})
	t.Run("a reply over 16 MiB", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		r := e.open(t, nil)
		f.fault = func(ep string, _ int, w http.ResponseWriter, _ *http.Request) bool {
			if ep == "/2/files/list_folder" {
				fmt.Fprint(w, `{"entries":[{".tag":"file","name":"`)
				_, _ = w.Write(bytes.Repeat([]byte("a"), driver.MaxListBody+10))
				fmt.Fprint(w, `"}]}`)
				return true
			}
			return false
		}
		if _, err := r.List(context.Background(), "launchpad-"); code(err) != driver.TooLarge {
			t.Errorf("huge list: %v", err)
		}
	})
	t.Run("a missing folder is empty", func(t *testing.T) {
		f := newFake()
		f.root = "/somewhere/else"
		e := setup(t, f)
		r := e.open(t, nil)
		es, err := r.List(context.Background(), "launchpad-")
		if err != nil || len(es) != 0 {
			t.Errorf("missing folder: %v %v", es, err)
		}
	})
}

// ---- tokens --------------------------------------------------------------

func TestTokenRefreshAndReuse(t *testing.T) {
	e, r := fresh(t)
	for i := 0; i < 3; i++ {
		if _, err := r.List(context.Background(), "launchpad-"); err != nil {
			t.Fatal(err)
		}
	}
	if e.f.refreshes != 1 {
		t.Fatalf("refreshes = %d, want 1 (the access token is reused)", e.f.refreshes)
	}
}

func TestTokenRefreshesOnExpiry(t *testing.T) {
	f := newFake()
	f.expiresIn = 3600
	e := setup(t, f)
	r := e.open(t, nil)
	if _, err := r.List(context.Background(), "launchpad-"); err != nil {
		t.Fatal(err)
	}
	e.now.Store(3001) // past expiry minus the 10 minute slack of a 1 hour token (3000 s)
	if _, err := r.List(context.Background(), "launchpad-"); err != nil {
		t.Fatal(err)
	}
	if f.refreshes != 2 {
		t.Fatalf("refreshes = %d, want 2 after the token ran out", f.refreshes)
	}
	if _, err := r.List(context.Background(), "launchpad-"); err != nil || f.refreshes != 2 {
		t.Fatalf("the second token is reused: %d %v", f.refreshes, err)
	}
}

func TestTokenRejectedByServerIsRefreshedOnce(t *testing.T) {
	e, r := fresh(t)
	ctx := context.Background()
	if _, err := r.List(ctx, "launchpad-"); err != nil {
		t.Fatal(err)
	}
	e.f.revokeAll() // Dropbox expired the token early
	if _, err := r.List(ctx, "launchpad-"); err != nil {
		t.Fatalf("after a server-side expiry: %v", err)
	}
	if e.f.refreshes != 2 || e.f.count("/2/files/list_folder") != 3 {
		t.Errorf("refreshes=%d list calls=%d", e.f.refreshes, e.f.count("/2/files/list_folder"))
	}
	// a server that keeps saying 401: one refresh, then auth_failed, no loop
	e.f.fault = func(ep string, _ int, w http.ResponseWriter, _ *http.Request) bool {
		if ep == "/2/files/list_folder" {
			jsonErr(w, 401, "invalid_access_token", map[string]any{".tag": "invalid_access_token"})
			return true
		}
		return false
	}
	before := e.f.refreshes
	if _, err := r.List(ctx, "launchpad-"); code(err) != driver.AuthFailed {
		t.Fatalf("persistent 401: %v", err)
	}
	if e.f.refreshes-before != 1 {
		t.Errorf("refreshes in the 401 loop: %d", e.f.refreshes-before)
	}
}

func TestBadCredentials(t *testing.T) {
	cases := map[string]func(f *fakeDropbox){
		"revoked refresh token": func(f *fakeDropbox) { f.refresh = "OTHER" },
		"wrong app secret":      func(f *fakeDropbox) { f.appSecret = "OTHER" },
		"wrong app key":         func(f *fakeDropbox) { f.appKey = "OTHER" },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			e := setup(t, f)
			c := e.cfg(t, nil)
			mod(f) // after the secrets were taken from the old values
			r, err := Driver{}.Open(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			_, err = r.List(context.Background(), "launchpad-")
			if code(err) != driver.AuthFailed {
				t.Fatalf("list: %v", err)
			}
			steps := r.Test(context.Background(), testName)
			if len(steps) != 2 || !steps[0].OK || steps[1].Name != "auth" || steps[1].Code != driver.AuthFailed {
				t.Errorf("test steps: %+v", steps)
			}
			if b, _ := json.Marshal(steps); strings.Contains(string(b), leak) || strings.Contains(err.Error(), leak) {
				t.Error("the remote's words leaked")
			}
			if f.total() > 4 {
				t.Errorf("%d requests: bad credentials must not loop", f.total())
			}
		})
	}
}

// ---- upload --------------------------------------------------------------

func TestSingleCallUploadVerifies(t *testing.T) {
	e, r := fresh(t)
	put(t, r, objName, payload(5000))
	if got := e.f.steps(); len(got) != 1 || got[0] != "upload" {
		t.Fatalf("steps = %v", got)
	}
}

func TestUploadSessionOffsets(t *testing.T) {
	f := newFake()
	e := setup(t, f)
	chunkSize = 4 << 20
	f.maxCall = int(chunkSize)
	r := e.open(t, nil)
	data := payload(3*int(chunkSize) + 1000)
	put(t, r, objName, data)
	want := []string{"start", "append@" + itoa(chunkSize), "append@" + itoa(2*chunkSize), "finish@" + itoa(3*chunkSize)}
	if got := f.steps(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	if !bytes.Equal(f.get("/backups/main/"+objName), data) {
		t.Fatal("stored object differs")
	}
	if f.biggest > int(chunkSize) {
		t.Errorf("a request body of %d bytes is above the chunk size", f.biggest)
	}
	rc, err := r.Get(context.Background(), objName)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("download differs")
	}
}

func itoa(n int64) string { return fmt.Sprint(n) }

func TestUploadExactChunkMultiples(t *testing.T) {
	for _, mult := range []int{1, 2, 3} {
		f := newFake()
		e := setup(t, f)
		chunkSize = 4 << 20
		r := e.open(t, nil)
		data := payload(mult * int(chunkSize))
		put(t, r, objName, data)
		if !bytes.Equal(f.get("/backups/main/"+objName), data) {
			t.Fatalf("x%d: stored differs", mult)
		}
		got := f.steps()
		if mult == 1 && (len(got) != 1 || got[0] != "upload") {
			t.Errorf("one chunk is one call: %v", got)
		}
		if mult > 1 && !strings.HasPrefix(got[len(got)-1], "finish@"+itoa(int64(mult-1)*chunkSize)) {
			t.Errorf("x%d: last step %v (the final chunk is never empty)", mult, got)
		}
	}
}

func TestContentHashIsDropboxsAlgorithm(t *testing.T) {
	// the driver's hasher against the independent one in the fake, on odd sizes
	for _, n := range []int{0, 1, 4<<20 - 1, 4 << 20, 4<<20 + 1, 9 << 20} {
		d := payload(n)
		h := newHasher()
		for i := 0; i < len(d); i += 1000003 { // uneven writes across block edges
			h.Write(d[i:min(i+1000003, len(d))])
		}
		if got, want := h.Final(), dbxHash(d); got != want {
			t.Errorf("n=%d hash %s want %s", n, got, want)
		}
	}
}

func TestFailedChunkIsResumed(t *testing.T) {
	data := payload(3*(4<<20) + 77)
	t.Run("the chunk was refused", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		chunkSize = 4 << 20
		r := e.open(t, nil)
		f.fault = func(ep string, n int, w http.ResponseWriter, _ *http.Request) bool {
			if ep == "/2/files/upload_session/append_v2" && n == 2 {
				jsonErr(w, 500, "internal", map[string]any{".tag": "internal"})
				return true
			}
			return false
		}
		put(t, r, objName, data)
		if !bytes.Equal(f.get("/backups/main/"+objName), data) {
			t.Fatal("object differs after the resend")
		}
		if got := strings.Join(f.steps(), ","); got != "start,append@4194304,append@8388608,finish@12582912" {
			t.Errorf("steps %s (a refused chunk is sent again at the same offset)", got)
		}
		if len(e.waits) != 1 {
			t.Errorf("waits %v", e.waits)
		}
	})
	t.Run("the chunk landed and the answer was lost", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		chunkSize = 4 << 20
		r := e.open(t, nil)
		f.dropReply["/2/files/upload_session/append_v2"] = 1
		put(t, r, objName, data)
		if !bytes.Equal(f.get("/backups/main/"+objName), data) {
			t.Fatal("object differs: a landed chunk must not be added twice")
		}
		if f.count("/2/files/upload_session/append_v2") != 3 { // 1 lost + 1 resend (incorrect_offset says done) + 1 next
			t.Errorf("append calls %d", f.count("/2/files/upload_session/append_v2"))
		}
	})
	t.Run("the final commit landed and the answer was lost", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		chunkSize = 4 << 20
		r := e.open(t, nil)
		f.dropReply["/2/files/upload_session/finish"] = 1
		put(t, r, objName, data)
		if !bytes.Equal(f.get("/backups/main/"+objName), data) {
			t.Fatal("object differs")
		}
		if f.count("/2/files/get_metadata") != 1 {
			t.Errorf("the resend must be settled by looking at the file: %d", f.count("/2/files/get_metadata"))
		}
	})
	t.Run("the first call of a small object fails once", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		r := e.open(t, nil)
		f.fault = func(ep string, n int, w http.ResponseWriter, _ *http.Request) bool {
			if ep == "/2/files/upload" && n == 1 {
				w.WriteHeader(503)
				return true
			}
			return false
		}
		put(t, r, objName, payload(100))
		if f.count("/2/files/upload") != 2 {
			t.Errorf("upload calls %d", f.count("/2/files/upload"))
		}
	})
	t.Run("persistent failure gives up", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		chunkSize = 4 << 20
		r := e.open(t, nil)
		f.fault = func(ep string, _ int, w http.ResponseWriter, _ *http.Request) bool {
			if ep == "/2/files/upload_session/append_v2" {
				w.WriteHeader(502)
				return true
			}
			return false
		}
		err := r.Put(context.Background(), objName, int64(len(data)), bytes.NewReader(data))
		var de *driver.Error
		if !errors.As(err, &de) || de.Code != driver.ServerError || de.Status != 502 {
			t.Fatalf("persistent 502: %v", err)
		}
		if f.count("/2/files/upload_session/append_v2") != transientTries {
			t.Errorf("tries %d", f.count("/2/files/upload_session/append_v2"))
		}
		if !driver.Retryable(err) {
			t.Error("a 5xx must stay retryable at run level")
		}
	})
	t.Run("a wrong offset that is not ours fails", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		chunkSize = 4 << 20
		r := e.open(t, nil)
		f.fault = func(ep string, n int, w http.ResponseWriter, _ *http.Request) bool {
			if ep == "/2/files/upload_session/append_v2" && n == 1 {
				jsonErr(w, 409, "incorrect_offset", map[string]any{".tag": "incorrect_offset", "correct_offset": 1})
				return true
			}
			return false
		}
		err := r.Put(context.Background(), objName, int64(len(data)), bytes.NewReader(data))
		if code(err) != driver.ServerError {
			t.Fatalf("foreign offset: %v", err)
		}
		if n := f.count("/2/files/upload_session/append_v2"); n != 1 {
			t.Errorf("append calls = %d: a foreign offset must stop the upload at that append", n)
		}
		for _, s := range f.steps() {
			if strings.HasPrefix(s, "finish") {
				t.Error("finish was sent after a foreign offset")
			}
		}
	})
}

func TestUploadReplyChecks(t *testing.T) {
	cases := map[string]string{
		"wrong size":  `{".tag":"file","name":"x","size":5,"content_hash":"%s"}`,
		"wrong hash":  `{".tag":"file","name":"x","size":100,"content_hash":"00"}`,
		"not json":    `<html>`,
		"empty reply": ``,
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake()
			e := setup(t, f)
			r := e.open(t, nil)
			f.fault = func(ep string, _ int, w http.ResponseWriter, _ *http.Request) bool {
				if ep == "/2/files/upload" {
					fmt.Fprint(w, strings.ReplaceAll(reply, "%s", dbxHash(payload(100))))
					return true
				}
				return false
			}
			err := r.Put(context.Background(), objName, 100, bytes.NewReader(payload(100)))
			if code(err) != driver.ServerError {
				t.Fatalf("%s: %v", name, err)
			}
		})
	}
	t.Run("a reply without a hash is checked by size", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		r := e.open(t, nil)
		f.fault = func(ep string, _ int, w http.ResponseWriter, _ *http.Request) bool {
			if ep == "/2/files/upload" {
				fmt.Fprint(w, `{".tag":"file","name":"x","size":100}`)
				return true
			}
			return false
		}
		if err := r.Put(context.Background(), objName, 100, bytes.NewReader(payload(100))); err != nil {
			t.Fatal(err)
		}
	})
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

func TestSourceSizeIsExact(t *testing.T) {
	for _, size := range []int{100, 3*(4<<20) + 5} {
		f := newFake()
		e := setup(t, f)
		chunkSize = 4 << 20
		r := e.open(t, nil)
		ctx := context.Background()
		short := payload(size - 1)
		if err := r.Put(ctx, objName, int64(size), bytes.NewReader(short)); code(err) != driver.DumpMismatch {
			t.Errorf("size %d short source: %v", size, err)
		}
		long := payload(size + 1)
		err := r.Put(ctx, objName, int64(size), bytes.NewReader(long))
		if code(err) != driver.DumpMismatch {
			t.Errorf("size %d long source: %v", size, err)
		}
		if f.has("/backups/main/" + objName) {
			t.Errorf("size %d: a source of the wrong length was committed", size)
		}
		if err := r.Put(ctx, objName, int64(size), errReader{errors.New("disk " + leak)}); err == nil || strings.Contains(err.Error(), leak) {
			t.Errorf("size %d reader error: %v", size, err)
		}
	}
}

func TestTooLarge(t *testing.T) {
	e, r := fresh(t)
	before := e.f.total()
	for _, size := range []int64{MaxObject + 1, -1} {
		src := &failReader{}
		if err := r.Put(context.Background(), objName, size, src); code(err) != driver.TooLarge {
			t.Errorf("size %d: %v", size, err)
		}
		if src.read {
			t.Error("the source was read before the size check")
		}
	}
	if e.f.total() != before {
		t.Error("a request was sent for a too large object")
	}
}

type failReader struct{ read bool }

func (f *failReader) Read([]byte) (int, error) { f.read = true; return 0, io.EOF }

func TestDefaultChunkIsBelowTheSingleCallLimit(t *testing.T) {
	// chunkSize is a var only so tests can shrink it; the shipped value must keep
	// every content request under Dropbox's documented 150 MB, and be a multiple
	// of the 4 MiB hash block.
	if chunkSize > maxCall || chunkSize%hashBlock != 0 || chunkSize <= 0 {
		t.Fatalf("chunkSize %d", chunkSize)
	}
	// Put clamps a bigger seam value to the limit (the buffer is never bigger).
	if got := min(int64(1)<<40, int64(maxCall)); got != maxCall {
		t.Fatal(got)
	}
}

// ---- rate limits ---------------------------------------------------------

func TestRateLimitRetryAfter(t *testing.T) {
	t.Run("a 429 with Retry-After is waited out", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		r := e.open(t, nil)
		f.fault = func(ep string, n int, w http.ResponseWriter, _ *http.Request) bool {
			if ep == "/2/files/upload" && n <= 2 {
				w.Header().Set("Retry-After", "7")
				jsonErr(w, 429, "too_many_requests", map[string]any{".tag": "too_many_requests", "retry_after": 99})
				return true
			}
			return false
		}
		put(t, r, objName, payload(10))
		if len(e.waits) != 2 || e.waits[0] != 7*time.Second || e.waits[1] != 7*time.Second {
			t.Fatalf("waits %v (the header wins over the body)", e.waits)
		}
	})
	t.Run("the body's retry_after is used without a header; a floor and a cap apply", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		r := e.open(t, nil)
		vals := map[int]string{1: `{"error":{".tag":"too_many_write_operations"}}`, 2: `{"error":{"reason":{".tag":"too_many_requests"},"retry_after":3}}`}
		f.fault = func(ep string, n int, w http.ResponseWriter, _ *http.Request) bool {
			if ep == "/2/files/delete_v2" && n <= 2 {
				w.WriteHeader(429)
				fmt.Fprint(w, vals[n])
				return true
			}
			return false
		}
		put(t, r, objName, payload(10))
		if err := r.Delete(context.Background(), objName); err != nil {
			t.Fatal(err)
		}
		if len(e.waits) != 2 || e.waits[0] != minRetryAfter || e.waits[1] != 3*time.Second {
			t.Fatalf("waits %v", e.waits)
		}
	})
	t.Run("too_many_write_operations on a 409 is also a wait", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		r := e.open(t, nil)
		f.fault = func(ep string, n int, w http.ResponseWriter, _ *http.Request) bool {
			if ep == "/2/files/upload" && n == 1 {
				jsonErr(w, 409, "too_many_write_operations", map[string]any{".tag": "path_write", "path_write": map[string]any{".tag": "too_many_write_operations"}})
				return true
			}
			return false
		}
		put(t, r, objName, payload(10))
		if len(e.waits) != 1 {
			t.Errorf("waits %v", e.waits)
		}
	})
	t.Run("a huge Retry-After is capped and the retries are bounded", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		r := e.open(t, nil)
		f.fault = func(ep string, _ int, w http.ResponseWriter, _ *http.Request) bool {
			if ep == "/2/files/list_folder" {
				w.Header().Set("Retry-After", "86400")
				w.WriteHeader(429)
				return true
			}
			return false
		}
		_, err := r.List(context.Background(), "launchpad-")
		var de *driver.Error
		if !errors.As(err, &de) || de.Code != driver.ServerError || de.Status != 429 {
			t.Fatalf("exhausted 429: %v", err)
		}
		if len(e.waits) != maxRateWaits {
			t.Fatalf("waits %d", len(e.waits))
		}
		for _, w := range e.waits {
			if w != maxRetryAfter {
				t.Errorf("wait %v is not capped", w)
			}
		}
	})
	t.Run("the token endpoint is rate limited too", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		r := e.open(t, nil)
		f.fault = func(ep string, n int, w http.ResponseWriter, _ *http.Request) bool {
			if ep == "/oauth2/token" && n == 1 {
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(429)
				return true
			}
			return false
		}
		if _, err := r.List(context.Background(), "launchpad-"); err != nil {
			t.Fatal(err)
		}
		if len(e.waits) != 1 || e.waits[0] != 2*time.Second {
			t.Errorf("waits %v", e.waits)
		}
	})
	t.Run("a cancelled wait stops", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		r := e.open(t, nil)
		ctx, cancel := context.WithCancel(context.Background())
		sleep = func(c context.Context, d time.Duration) error { cancel(); return c.Err() }
		f.fault = func(ep string, _ int, w http.ResponseWriter, _ *http.Request) bool {
			if ep == "/2/files/list_folder" {
				w.WriteHeader(429)
				return true
			}
			return false
		}
		if _, err := r.List(ctx, "launchpad-"); err == nil {
			t.Fatal("no error")
		}
		_ = e
	})
}

// ---- error mapping -------------------------------------------------------

func TestErrorMapping(t *testing.T) {
	type tc struct {
		name   string
		status int
		obj    any
		want   driver.Code
		st     int
	}
	cases := []tc{
		{"403 forbidden", 403, map[string]any{".tag": "other"}, driver.Forbidden, 0},
		{"missing scope", 403, map[string]any{".tag": "missing_scope"}, driver.Forbidden, 0},
		{"no write permission", 409, map[string]any{".tag": "path", "path": map[string]any{".tag": "no_write_permission"}}, driver.Forbidden, 0},
		{"disallowed name", 409, map[string]any{".tag": "path", "path": map[string]any{".tag": "disallowed_name"}}, driver.Forbidden, 0},
		{"insufficient space", 409, map[string]any{".tag": "path", "path": map[string]any{".tag": "insufficient_space"}}, driver.QuotaExceeded, 0},
		{"too large tag", 409, map[string]any{".tag": "too_large"}, driver.TooLarge, 0},
		{"413", 413, nil, driver.TooLarge, 0},
		{"conflict", 409, map[string]any{".tag": "path", "path": map[string]any{".tag": "conflict"}}, driver.ServerError, 409},
		{"malformed path", 409, map[string]any{".tag": "path", "path": map[string]any{".tag": "malformed_path"}}, driver.ServerError, 409},
		{"400", 400, nil, driver.ServerError, 400},
		{"500", 500, nil, driver.ServerError, 500},
		{"503", 503, nil, driver.ServerError, 503},
		{"redirect", 302, nil, driver.ServerError, 302},
		{"408", 408, nil, driver.Timeout, 0},
		{"507", 507, nil, driver.QuotaExceeded, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake()
			e := setup(t, f)
			r := e.open(t, nil)
			f.fault = func(ep string, _ int, w http.ResponseWriter, _ *http.Request) bool {
				if ep == "/2/files/list_folder" {
					if c.status == 302 {
						w.Header().Set("Location", "https://evil.example.com/"+leak)
					}
					jsonErr(w, c.status, "x", c.obj)
					return true
				}
				return false
			}
			_, err := r.List(context.Background(), "launchpad-")
			var de *driver.Error
			if !errors.As(err, &de) || de.Code != c.want || de.Status != c.st {
				t.Fatalf("got %v, want %s status %d", err, c.want, c.st)
			}
			if strings.Contains(err.Error(), leak) || strings.Contains(err.Error(), "evil") {
				t.Errorf("error text leaks the remote's words: %v", err)
			}
		})
	}
}

func TestRemoteWordsNeverLeak(t *testing.T) {
	f := newFake()
	e := setup(t, f)
	r := e.open(t, nil)
	ctx := context.Background()
	f.missingScope = true
	steps := r.Test(ctx, testName)
	b, _ := json.Marshal(steps)
	for _, bad := range []string{leak, f.refresh, f.appSecret, f.appKey, "ACCESS-"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("steps contain %q: %s", bad, b)
		}
	}
	_, err1 := r.List(ctx, "launchpad-")
	err2 := r.Put(ctx, objName, 1, strings.NewReader("x"))
	_, err3 := r.Get(ctx, objName)
	err4 := r.Delete(ctx, objName)
	for _, err := range []error{err1, err2, err3, err4} {
		if err == nil {
			t.Fatal("expected errors with a missing scope")
		}
		for _, bad := range []string{leak, f.refresh, f.appSecret, f.appKey, "ACCESS-"} {
			if strings.Contains(err.Error(), bad) {
				t.Errorf("error %q contains %q", err, bad)
			}
		}
	}
}

func TestDeleteOnlySwallowsNotFound(t *testing.T) {
	f := newFake()
	e := setup(t, f)
	r := e.open(t, nil)
	f.fault = func(ep string, _ int, w http.ResponseWriter, _ *http.Request) bool {
		if ep == "/2/files/delete_v2" {
			pathErr(w, "path_write", "no_write_permission")
			return true
		}
		return false
	}
	if err := r.Delete(context.Background(), objName); code(err) != driver.Forbidden {
		t.Errorf("delete refused: %v", err)
	}
}

func TestTestStepFailures(t *testing.T) {
	steps := func(f *fakeDropbox) []driver.Step {
		e := setup(t, f)
		r := e.open(t, nil)
		return r.Test(context.Background(), testName)
	}
	t.Run("ok", func(t *testing.T) {
		s := steps(newFake())
		if len(s) != 5 {
			t.Fatalf("%+v", s)
		}
		for i, n := range []string{"connect", "auth", "write", "read", "delete"} {
			if s[i].Name != n || !s[i].OK {
				t.Errorf("step %d %+v", i, s[i])
			}
		}
	})
	for _, st := range []int{401, 403} {
		t.Run(fmt.Sprintf("missing scope (%d) stops at auth", st), func(t *testing.T) {
			f := newFake()
			f.missingScope, f.scopeStatus = true, st
			s := steps(f)
			if len(s) != 2 || s[1].Name != "auth" || s[1].Code != driver.Forbidden {
				t.Errorf("%+v", s)
			}
			if f.refreshes != 1 {
				t.Errorf("refreshes = %d: a missing scope is not cured by a new token", f.refreshes)
			}
		})
	}
	t.Run("write refused", func(t *testing.T) {
		f := newFake()
		f.fault = func(ep string, _ int, w http.ResponseWriter, _ *http.Request) bool {
			if ep == "/2/files/upload" {
				pathErr(w, "path", "insufficient_space")
				return true
			}
			return false
		}
		s := steps(f)
		if len(s) != 3 || s[2].Name != "write" || s[2].Code != driver.QuotaExceeded {
			t.Errorf("%+v", s)
		}
	})
	t.Run("download differs", func(t *testing.T) {
		f := newFake()
		f.fault = func(ep string, _ int, w http.ResponseWriter, _ *http.Request) bool {
			if ep == "/2/files/download" {
				fmt.Fprint(w, "other bytes")
				return true
			}
			return false
		}
		s := steps(f)
		if len(s) != 4 || s[3].Name != "read" || s[3].Code != driver.ServerError {
			t.Errorf("%+v", s)
		}
	})
	t.Run("delete refused", func(t *testing.T) {
		f := newFake()
		f.fault = func(ep string, _ int, w http.ResponseWriter, _ *http.Request) bool {
			if ep == "/2/files/delete_v2" {
				pathErr(w, "path_write", "no_write_permission")
				return true
			}
			return false
		}
		s := steps(f)
		if len(s) != 5 || s[4].Name != "delete" || s[4].Code != driver.Forbidden {
			t.Errorf("%+v", s)
		}
	})
	t.Run("unreachable stops at connect", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		r := e.open(t, nil)
		f.api.Close()
		s := r.Test(context.Background(), testName)
		if len(s) != 1 || s[0].Name != "connect" || s[0].OK || s[0].Code != driver.ConnectRefused {
			t.Errorf("%+v", s)
		}
	})
}

// ---- transport -----------------------------------------------------------

func TestStall(t *testing.T) {
	f := newFake()
	e := setup(t, f)
	r := e.open(t, nil)
	if _, err := r.List(context.Background(), "launchpad-"); err != nil { // token first
		t.Fatal(err)
	}
	stallAfter = 150 * time.Millisecond
	f.fault = func(ep string, _ int, w http.ResponseWriter, req *http.Request) bool {
		if ep == "/2/files/upload" {
			_, _ = io.Copy(io.Discard, req.Body)
			<-req.Context().Done()
			return true
		}
		return false
	}
	err := r.Put(context.Background(), objName, 10, bytes.NewReader(payload(10)))
	if code(err) != driver.Stalled {
		t.Fatalf("stalled upload: %v", err)
	}
	if f.count("/2/files/upload") != transientTries {
		t.Errorf("a stall is transient: %d tries", f.count("/2/files/upload"))
	}
	_ = e
}

func TestConnectionResetAndContext(t *testing.T) {
	f := newFake()
	e := setup(t, f)
	r := e.open(t, nil)
	f.fault = func(ep string, _ int, w http.ResponseWriter, _ *http.Request) bool {
		if ep == "/2/files/delete_v2" {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return true
		}
		return false
	}
	err := r.Delete(context.Background(), objName)
	var de *driver.Error
	if !errors.As(err, &de) || de.Code != driver.ServerError || de.Status != 0 {
		t.Errorf("reset: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.List(ctx, "launchpad-"); err == nil {
		t.Error("cancelled context")
	}
	_ = e
}

func TestRedirectIsNotFollowed(t *testing.T) {
	f := newFake()
	e := setup(t, f)
	r := e.open(t, nil)
	hit := false
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hit = true }))
	defer other.Close()
	f.fault = func(ep string, _ int, w http.ResponseWriter, req *http.Request) bool {
		if ep == "/2/files/download" {
			http.Redirect(w, req, other.URL+"/x", http.StatusFound)
			return true
		}
		return false
	}
	_, err := r.Get(context.Background(), objName)
	var de *driver.Error
	if !errors.As(err, &de) || de.Code != driver.ServerError || de.Status != 302 {
		t.Errorf("redirect: %v", err)
	}
	if hit {
		t.Error("a redirect was followed")
	}
	_ = e
}

func TestAuthorizationHeaderAndHosts(t *testing.T) {
	f := newFake()
	e := setup(t, f)
	r := e.open(t, nil)
	var seen []string
	f.fault = func(ep string, _ int, w http.ResponseWriter, req *http.Request) bool {
		if ep == "/oauth2/token" {
			if req.Header.Get("Authorization") != "" {
				seen = append(seen, "token call carried a bearer")
			}
		} else if !strings.HasPrefix(req.Header.Get("Authorization"), "Bearer ACCESS-") {
			seen = append(seen, ep+" without a bearer")
		}
		return false
	}
	put(t, r, objName, payload(10))
	rc, _ := r.Get(context.Background(), objName)
	rc.Close()
	_, _ = r.List(context.Background(), "")
	_ = r.Delete(context.Background(), objName)
	if len(seen) != 0 {
		t.Errorf("%v", seen)
	}
	_ = e
}

func TestGetBodyIsNotCappedByDriver(t *testing.T) {
	// The caller caps downloads (driver.CapReader); the driver streams.
	e, r := fresh(t)
	put(t, r, objName, payload(2000))
	rc, err := r.Get(context.Background(), objName)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(driver.CapReader(rc, 10)); code(err) != driver.TooLarge {
		t.Errorf("caller cap: %v", err)
	}
	_ = e
}

// ---- TLS, SSRF, probe ----------------------------------------------------

func TestTLSPin(t *testing.T) {
	f := newFake()
	e := setup(t, f)
	pin := spki(f.api.Certificate())
	ctx := context.Background()
	// the right pin works even with no trusted roots
	testRoots = nil
	r := e.open(t, map[string]any{"tlsPin": pin})
	if _, err := r.List(ctx, "launchpad-"); err != nil {
		t.Fatalf("pinned: %v", err)
	}
	// a wrong pin
	r2 := e.open(t, map[string]any{"tlsPin": strings.Repeat("0", 64)})
	if _, err := r2.List(ctx, "launchpad-"); code(err) != driver.TLSPinMismatch {
		t.Errorf("wrong pin: %v", err)
	}
	// no pin and no trust
	r3 := e.open(t, nil)
	if _, err := r3.List(ctx, "launchpad-"); code(err) != driver.TLSUntrusted {
		t.Errorf("untrusted: %v", err)
	}
	// the pin covers the content host too: a pin that matches only the API host is not enough
	other := httptest.NewUnstartedServer(f.handler("content"))
	other.StartTLS()
	defer other.Close()
	contentBase = other.URL
	r4 := e.open(t, map[string]any{"tlsPin": pin})
	_ = r4 // same httptest certificate: documents that both hosts are pinned to one SPKI
	// an expired leaf is refused even when pinned
	clockNow = func() time.Time { return time.Now().Add(100 * 365 * 24 * time.Hour) }
	r5 := e.open(t, map[string]any{"tlsPin": pin})
	if _, err := r5.List(ctx, "launchpad-"); code(err) != driver.TLSUntrusted {
		t.Errorf("expired pinned leaf: %v", err)
	}
}

func TestProbe(t *testing.T) {
	f := newFake()
	e := setup(t, f)
	c := e.cfg(t, nil)
	c.Secrets = nil
	pr, err := Driver{}.Probe(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Kind != "tls" || pr.Fingerprint != spki(f.api.Certificate()) || !pr.ChainVerified {
		t.Errorf("probe %+v", pr)
	}
	if f.total() != 0 {
		t.Errorf("the probe sent %d requests", f.total())
	}
	testRoots = nil
	pr, err = Driver{}.Probe(context.Background(), c)
	if err != nil || pr.ChainVerified {
		t.Errorf("untrusted chain: %+v %v", pr, err)
	}
	f.api.Close()
	if _, err := (Driver{}).Probe(context.Background(), c); code(err) != driver.ConnectRefused {
		t.Errorf("refused: %v", err)
	}
	if _, err := (Driver{}).Probe(context.Background(), driver.Config{Config: json.RawMessage(`{"x":1}`), Dialer: c.Dialer}); err == nil {
		t.Error("probe of a bad config")
	}
	c.Dialer = nil
	if _, err := (Driver{}).Probe(context.Background(), c); err == nil {
		t.Error("probe without a dialer")
	}
}

func TestLoopbackRefusedByDefaultPolicy(t *testing.T) {
	f := newFake()
	e := setup(t, f)
	c := e.cfg(t, nil)
	c.Dialer = dialer(netguard.Policy{}) // the real default: loopback is refused
	r, err := Driver{}.Open(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.List(context.Background(), "launchpad-"); code(err) != driver.SSRFBlocked {
		t.Errorf("list: %v", err)
	}
	if _, err := (Driver{}).Probe(context.Background(), c); code(err) != driver.SSRFBlocked {
		t.Errorf("probe: %v", err)
	}
	if f.total() != 0 {
		t.Error("the guarded dialer let a request through")
	}
}

func TestPlainHTTPBaseIsRefused(t *testing.T) {
	e := setup(t, newFake())
	apiBase = "http://127.0.0.1:1"
	if _, err := (Driver{}).Open(context.Background(), e.cfg(t, nil)); err == nil {
		t.Error("an http:// API base was accepted: TLS only")
	}
	apiBase = "https://127.0.0.1:1"
	contentBase = "http://127.0.0.1:1"
	if _, err := (Driver{}).Open(context.Background(), e.cfg(t, nil)); err == nil {
		t.Error("an http:// content base was accepted: TLS only")
	}
}

func TestProxyEnvironmentIsIgnored(t *testing.T) {
	var proxied atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { proxied.Store(true) }))
	defer proxy.Close()
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("ALL_PROXY", proxy.URL)
	e, r := fresh(t)
	if _, err := r.List(context.Background(), "launchpad-"); err != nil {
		t.Fatal(err)
	}
	if proxied.Load() {
		t.Error("the proxy variables were used")
	}
	rm := r.(*remote)
	if len(rm.trs) != 2 {
		t.Fatalf("transports: %d", len(rm.trs))
	}
	for i, tr := range rm.trs {
		if tr.Proxy != nil {
			t.Errorf("transport %d has a Proxy func", i)
		}
	}
	_ = e
}

// ---- exchange ------------------------------------------------------------

func exchangeCfg(e *env, t *testing.T) driver.Config {
	c := e.cfg(t, nil)
	b, _ := json.Marshal(map[string]string{"appKey": e.f.appKey, "appSecret": e.f.appSecret})
	c.Secrets = b
	return c
}

func TestExchange(t *testing.T) {
	f := newFake()
	e := setup(t, f)
	ctx := context.Background()
	tok, err := Driver{}.Exchange(ctx, exchangeCfg(e, t), f.code)
	if err != nil || tok != f.refresh {
		t.Fatalf("exchange: %q %v", tok, err)
	}
	if f.count("/2/files/list_folder") != 1 {
		t.Errorf("the exchange lists the folder once: %d", f.count("/2/files/list_folder"))
	}
	// the code is single use: a second exchange fails and returns nothing
	tok, err = Driver{}.Exchange(ctx, exchangeCfg(e, t), f.code)
	if code(err) != driver.AuthFailed || tok != "" {
		t.Fatalf("second exchange: %q %v", tok, err)
	}
	// the refresh token it made opens the destination
	r := e.open(t, nil)
	if s := r.Test(ctx, testName); len(s) != 5 || !s[4].OK {
		t.Fatalf("test after exchange: %+v", s)
	}
}

func TestExchangeFailures(t *testing.T) {
	t.Run("wrong code", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		tok, err := Driver{}.Exchange(context.Background(), exchangeCfg(e, t), "WRONG-CODE")
		if code(err) != driver.AuthFailed || tok != "" {
			t.Errorf("%q %v", tok, err)
		}
		for _, bad := range []string{"WRONG-CODE", f.appSecret, leak} {
			if strings.Contains(err.Error(), bad) {
				t.Errorf("error contains %q", bad)
			}
		}
	})
	t.Run("codes that cannot be sent", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		for _, c := range []string{"", "a b", "a\nb", "é", strings.Repeat("a", 513)} {
			if _, err := (Driver{}).Exchange(context.Background(), exchangeCfg(e, t), c); code(err) != driver.AuthFailed {
				t.Errorf("code %q: %v", c, err)
			}
		}
		if f.total() != 0 {
			t.Error("an unusable code reached Dropbox")
		}
	})
	t.Run("wrong app secret", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		c := exchangeCfg(e, t)
		f.appSecret = "OTHER"
		if _, err := (Driver{}).Exchange(context.Background(), c, f.code); code(err) != driver.AuthFailed {
			t.Errorf("%v", err)
		}
	})
	t.Run("no refresh token in the reply (an online authorization)", func(t *testing.T) {
		f := newFake()
		f.noRefreshToken = true
		e := setup(t, f)
		tok, err := Driver{}.Exchange(context.Background(), exchangeCfg(e, t), f.code)
		if code(err) != driver.AuthFailed || tok != "" {
			t.Errorf("%q %v", tok, err)
		}
	})
	for _, st := range []int{401, 403} {
		t.Run(fmt.Sprintf("an app without the file scopes (%d)", st), func(t *testing.T) {
			f := newFake()
			f.missingScope, f.scopeStatus = true, st
			e := setup(t, f)
			tok, err := Driver{}.Exchange(context.Background(), exchangeCfg(e, t), f.code)
			if code(err) != driver.Forbidden || tok != "" {
				t.Errorf("the token must not be returned: %q %v", tok, err)
			}
			if f.count("/oauth2/token") != 1 {
				t.Errorf("token calls = %d: the exchange must not try to refresh (it has no refresh token)", f.count("/oauth2/token"))
			}
		})
	}
	t.Run("a plain 401 after the exchange is final and returns no token", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		f.fault = func(ep string, _ int, w http.ResponseWriter, _ *http.Request) bool {
			if ep == "/2/files/list_folder" {
				jsonErr(w, 401, "invalid_access_token", map[string]any{".tag": "invalid_access_token"})
				return true
			}
			return false
		}
		tok, err := Driver{}.Exchange(context.Background(), exchangeCfg(e, t), f.code)
		if code(err) != driver.AuthFailed || tok != "" || f.count("/oauth2/token") != 1 {
			t.Errorf("%q %v token calls %d", tok, err, f.count("/oauth2/token"))
		}
	})
	t.Run("another listing failure does not lose the token", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		f.fault = func(ep string, _ int, w http.ResponseWriter, _ *http.Request) bool {
			if ep == "/2/files/list_folder" {
				w.WriteHeader(500)
				return true
			}
			return false
		}
		tok, err := Driver{}.Exchange(context.Background(), exchangeCfg(e, t), f.code)
		if err != nil || tok != f.refresh {
			t.Errorf("the used code must not be wasted: %q %v", tok, err)
		}
	})
	t.Run("secrets", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		for _, s := range []string{``, `{}`, `{"appKey":"a"}`, `{"appSecret":"a"}`} {
			c := e.cfg(t, nil)
			c.Secrets = json.RawMessage(s)
			if _, err := (Driver{}).Exchange(context.Background(), c, "x"); code(err) != driver.SecretsUnreadable {
				t.Errorf("secrets %q: %v", s, err)
			}
		}
	})
	t.Run("no secret in the URL or in an error", func(t *testing.T) {
		// the fake answers 400 "secrets in URL" if a query string is ever used
		f := newFake()
		e := setup(t, f)
		if _, err := (Driver{}).Exchange(context.Background(), exchangeCfg(e, t), f.code); err != nil {
			t.Fatal(err)
		}
	})
}

func TestEveryRequestCarriesNoSecretInItsURL(t *testing.T) {
	f := newFake()
	e := setup(t, f)
	r := e.open(t, nil)
	var urls []string
	f.fault = func(ep string, _ int, _ http.ResponseWriter, req *http.Request) bool {
		urls = append(urls, req.URL.String())
		return false
	}
	put(t, r, objName, payload(10))
	_, _ = r.List(context.Background(), "")
	for _, u := range urls {
		for _, s := range []string{f.refresh, f.appSecret, f.appKey, "ACCESS-"} {
			if strings.Contains(u, s) {
				t.Errorf("URL %q contains a secret", u)
			}
		}
		if strings.Contains(u, "?") {
			t.Errorf("URL %q has a query", u)
		}
	}
}

// ---- review round: gaps ---------------------------------------------------

// within fails fast if fn does not return (an unbounded retry would hang).
func within(t *testing.T, d time.Duration, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("did not return within %v (an unbounded loop?)", d)
	}
}

var realSleep = sleep // the default, before setup replaces it

func TestListContinueNotFoundIsAnError(t *testing.T) {
	f := newFake()
	f.pageSize = 1
	e := setup(t, f)
	r := e.open(t, nil)
	f.put("/backups/main/"+objName, []byte("x"))
	f.put("/backups/main/"+manName, []byte("x"))
	f.fault = func(ep string, _ int, w http.ResponseWriter, _ *http.Request) bool {
		if ep == "/2/files/list_folder/continue" {
			pathErr(w, "path", "not_found")
			return true
		}
		return false
	}
	if es, err := r.List(context.Background(), "launchpad-"); code(err) != driver.NotFound {
		t.Fatalf("a not_found on a later page must fail, got %v %v", es, err)
	}
}

func TestCancelDuringRetryAfterWait(t *testing.T) {
	f := newFake()
	e := setup(t, f)
	r := e.open(t, nil)
	sleep = realSleep
	// The context is cancelled 150 ms after the fake server has SEEN the list
	// call, never on a clock that starts earlier: token refresh and the TLS
	// handshake on a loaded runner could otherwise cancel before any call
	// was made (a flake, "list calls 0").
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var once sync.Once
	f.fault = func(ep string, _ int, w http.ResponseWriter, _ *http.Request) bool {
		if ep == "/2/files/list_folder" {
			once.Do(func() { time.AfterFunc(150*time.Millisecond, cancel) })
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(429)
			return true
		}
		return false
	}
	var err error
	start := time.Now()
	within(t, 5*time.Second, func() { _, err = r.List(ctx, "launchpad-") })
	if err == nil || time.Since(start) > 4*time.Second {
		t.Fatalf("a cancelled wait must return the context error quickly: %v after %v", err, time.Since(start))
	}
	if f.count("/2/files/list_folder") != 1 {
		t.Errorf("list calls %d", f.count("/2/files/list_folder"))
	}
}

func TestRetryCeilings(t *testing.T) {
	t.Run("401 refreshes once", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		r := e.open(t, nil)
		f.fault = func(ep string, _ int, w http.ResponseWriter, _ *http.Request) bool {
			if ep == "/2/files/list_folder" {
				jsonErr(w, 401, "invalid_access_token", map[string]any{".tag": "invalid_access_token"})
				return true
			}
			return false
		}
		within(t, 10*time.Second, func() { _, _ = r.List(context.Background(), "launchpad-") })
		if f.count("/2/files/list_folder") != 2 || f.refreshes != 2 {
			t.Errorf("list calls %d refreshes %d (want 2 and 2)", f.count("/2/files/list_folder"), f.refreshes)
		}
	})
	t.Run("429 waits are bounded", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		r := e.open(t, nil)
		f.fault = func(ep string, _ int, w http.ResponseWriter, _ *http.Request) bool {
			if ep == "/2/files/list_folder" {
				w.WriteHeader(429)
				return true
			}
			return false
		}
		within(t, 10*time.Second, func() { _, _ = r.List(context.Background(), "launchpad-") })
		if n := f.count("/2/files/list_folder"); n != maxRateWaits+1 {
			t.Errorf("calls %d want %d", n, maxRateWaits+1)
		}
	})
	t.Run("transient tries are bounded for every chunk call", func(t *testing.T) {
		for _, ep := range []string{"/2/files/upload", "/2/files/upload_session/start"} {
			f := newFake()
			e := setup(t, f)
			chunkSize = 4 << 20
			r := e.open(t, nil)
			f.fault = func(p string, _ int, w http.ResponseWriter, _ *http.Request) bool {
				if p == ep {
					w.WriteHeader(503)
					return true
				}
				return false
			}
			size := 100
			if strings.HasSuffix(ep, "start") {
				size = 5 << 20
			}
			within(t, 10*time.Second, func() { _ = r.Put(context.Background(), objName, int64(size), bytes.NewReader(payload(size))) })
			if n := f.count(ep); n != transientTries {
				t.Errorf("%s calls %d want %d", ep, n, transientTries)
			}
		}
	})
	t.Run("a non-transient chunk failure is sent once", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		r := e.open(t, nil)
		f.fault = func(p string, _ int, w http.ResponseWriter, _ *http.Request) bool {
			if p == "/2/files/upload" {
				w.WriteHeader(400)
				return true
			}
			return false
		}
		_ = r.Put(context.Background(), objName, 10, bytes.NewReader(payload(10)))
		if f.count("/2/files/upload") != 1 {
			t.Errorf("calls %d", f.count("/2/files/upload"))
		}
	})
}

func TestTokenCallIsNeverResentOnTransientFailure(t *testing.T) {
	t.Run("5xx", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		r := e.open(t, nil)
		f.fault = func(ep string, _ int, w http.ResponseWriter, _ *http.Request) bool {
			if ep == "/oauth2/token" {
				w.WriteHeader(503)
				return true
			}
			return false
		}
		_, err := r.List(context.Background(), "launchpad-")
		var de *driver.Error
		if !errors.As(err, &de) || de.Status != 503 {
			t.Fatalf("%v", err)
		}
		if f.count("/oauth2/token") != 1 {
			t.Errorf("token calls %d", f.count("/oauth2/token"))
		}
	})
	t.Run("stall", func(t *testing.T) {
		f := newFake()
		e := setup(t, f)
		r := e.open(t, nil)
		stallAfter = 100 * time.Millisecond
		f.fault = func(ep string, _ int, w http.ResponseWriter, req *http.Request) bool {
			if ep == "/oauth2/token" {
				_, _ = io.Copy(io.Discard, req.Body)
				<-req.Context().Done()
				return true
			}
			return false
		}
		// the watch reads stallAfter when a request starts: open a new remote after setting it
		r2 := e.open(t, nil)
		_, err := r2.List(context.Background(), "launchpad-")
		if code(err) != driver.Stalled || f.count("/oauth2/token") != 1 {
			t.Errorf("%v token calls %d", err, f.count("/oauth2/token"))
		}
		_ = r
	})
}

// lateReader holds the byte after head back for a while (LAUN-158): a long source
// must fail closed even when the extra byte arrives late.
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

func TestLongSourceNeverSucceedsEvenIfExtraByteIsLate(t *testing.T) {
	for _, size := range []int{100, 3 * (4 << 20)} {
		f := newFake()
		e := setup(t, f)
		chunkSize = 4 << 20
		r := e.open(t, nil)
		p := string(payload(size))
		err := r.Put(context.Background(), objName, int64(size), &lateReader{head: p, tail: "xyz"})
		if code(err) != driver.DumpMismatch {
			t.Errorf("size %d long source: %v", size, err)
		}
		if f.has("/backups/main/" + objName) {
			t.Errorf("size %d: a long source was committed", size)
		}
	}
}
