package sftp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gosftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"devops/internal/backup"
	"devops/internal/backup/driver"
	"devops/internal/netguard"
	"devops/internal/netguard/netguardtest"
)

const (
	testPass   = "s3cret"
	validName  = "launchpad-0a1b2c3d-20260102T030405Z-abcd.dump.age"
	validName2 = "launchpad-0a1b2c3d-20260103T030405Z-abce.json"
	testFile   = "launchpad-0a1b2c3d-test-0123456789abcdef.tmp"
)

func loopDialer() *netguard.Dialer {
	return &netguard.Dialer{Policy: netguardtest.Loopback(), Timeout: 5 * time.Second}
}

func cfgJSON(t testing.TB, host string, port int, pin, folder string) json.RawMessage {
	t.Helper()
	m := map[string]any{"host": host, "port": port, "username": "backup"}
	if pin != "" {
		m["hostKey"] = pin
	}
	if folder != "" {
		m["folder"] = folder
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func secJSON(pw, key string) json.RawMessage {
	m := map[string]string{}
	if pw != "" {
		m["password"] = pw
	}
	if key != "" {
		m["privateKey"] = key
	}
	b, _ := json.Marshal(m)
	return b
}

func code(err error) driver.Code {
	if err == nil {
		return ""
	}
	return driver.CodeOf(err)
}

// open returns a Remote pinned to the server's current host key.
func open(t *testing.T, s *testServer, folder string) driver.Remote {
	t.Helper()
	h, p := s.addr()
	r, err := Driver{}.Open(context.Background(), driver.Config{
		Config: cfgJSON(t, h, p, s.fingerprint(), folder), Secrets: secJSON(testPass, ""), Dialer: loopDialer()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func put(t *testing.T, r driver.Remote, name string, body []byte) {
	t.Helper()
	if err := r.Put(context.Background(), name, int64(len(body)), bytes.NewReader(body)); err != nil {
		t.Fatalf("put %s: %v", name, err)
	}
}

func readAll(t *testing.T, r driver.Remote, name string) []byte {
	t.Helper()
	rc, err := r.Get(context.Background(), name)
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestValidateAndIdentity(t *testing.T) {
	ok := []string{
		`{"host":"nas.example.com","username":"backup"}`,
		`{"host":"NAS.example.com","port":2222,"username":"u","folder":"a/b-1/c_d.e","hostKey":"SHA256:` + strings.Repeat("A", 43) + `"}`,
		`{"host":"192.0.2.7","username":"u","folder":"/volume1/backups"}`,
		`{"host":"2001:db8::1","username":"u"}`,
	}
	for _, c := range ok {
		if err := (Driver{}).Validate(json.RawMessage(c)); err != nil {
			t.Errorf("%s: %v", c, err)
		}
	}
	bad := []string{
		``, `{}`, `[]`, `null`,
		`{"host":"h","username":"u"} {}`,
		`{"host":"h","username":"u","extra":1}`,
		`{"host":"h:22","username":"u"}`,
		`{"host":"[::1]","username":"u"}`,
		`{"host":"fe80::1%eth0","username":"u"}`,
		`{"host":"h h","username":"u"}`,
		`{"host":"-h","username":"u"}`,
		`{"host":"h","username":""}`,
		`{"host":"h","username":"u v"}`,
		`{"host":"h","username":"u","port":0,"folder":"x/../y"}`,
		`{"host":"h","username":"u","port":70000}`,
		`{"host":"h","username":"u","port":-1}`,
		`{"host":"h","username":"u","folder":"a//b"}`,
		`{"host":"h","username":"u","folder":"a/"}`,
		`{"host":"h","username":"u","folder":"/"}`,
		`{"host":"h","username":"u","folder":"//a"}`,
		`{"host":"h","username":"u","folder":"../a"}`,
		`{"host":"h","username":"u","folder":"a/./b"}`,
		`{"host":"h","username":"u","folder":"a..b"}`,
		`{"host":"h","username":"u","folder":"a b"}`,
		`{"host":"h","username":"u","folder":"` + strings.Repeat("a/", 101) + `a"}`,
		`{"host":"h","username":"u","hostKey":"SHA256:short"}`,
		`{"host":"h","username":"u","hostKey":"MD5:` + strings.Repeat("A", 43) + `"}`,
		`{"host":"h","username":"u","hostKey":"` + strings.Repeat("a", 64) + `"}`,
	}
	for _, c := range bad {
		if err := (Driver{}).Validate(json.RawMessage(c)); err == nil {
			t.Errorf("accepted %q", c)
		}
	}
	id, err := (Driver{}).Identity(json.RawMessage(`{"host":"NAS.Example.com","username":"Bob","folder":"/x/y"}`))
	if err != nil || id != (driver.Identity{Type: "sftp", Host: "nas.example.com", Bucket: "Bob", Folder: "/x/y"}) {
		t.Fatalf("%+v %v", id, err)
	}
	id, _ = (Driver{}).Identity(json.RawMessage(`{"host":"nas.example.com","port":22,"username":"b"}`))
	id2, _ := (Driver{}).Identity(json.RawMessage(`{"host":"NAS.example.com","username":"b","hostKey":"SHA256:` + strings.Repeat("B", 43) + `"}`))
	if id != id2 {
		t.Fatalf("default port or pin changed the identity: %+v %+v", id, id2)
	}
	id3, _ := (Driver{}).Identity(json.RawMessage(`{"host":"2001:db8::1","port":2200,"username":"b"}`))
	if id3.Host != "[2001:db8::1]:2200" {
		t.Fatalf("%q", id3.Host)
	}
	if _, err := (Driver{}).Identity(json.RawMessage(`{`)); err == nil {
		t.Fatal("identity of a bad config")
	}
	if d, ok := driver.Lookup(Type); !ok || d == nil {
		t.Fatal("not registered")
	}
}

func TestProbeSendsNoCredentialsAndShowsTheFingerprint(t *testing.T) {
	s := newTestServer(t)
	h, p := s.addr()
	res, err := Driver{}.Probe(context.Background(), driver.Config{Config: cfgJSON(t, h, p, "", ""), Dialer: loopDialer()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != "ssh" || res.Fingerprint != s.fingerprint() || res.ChainVerified {
		t.Fatalf("%+v want %s", res, s.fingerprint())
	}
	s.settle() // the server has finished with the probe's connection
	if n := s.authAttempts.Load(); n != 0 {
		t.Fatalf("the probe sent %d auth requests (not even none is allowed)", n)
	}
	// The probe works with a pin that does not match: it only shows the key.
	res2, err := Driver{}.Probe(context.Background(), driver.Config{Config: cfgJSON(t, h, p, "SHA256:"+strings.Repeat("Z", 43), ""), Dialer: loopDialer()})
	if err != nil || res2.Fingerprint != res.Fingerprint {
		t.Fatalf("%+v %v", res2, err)
	}
	// A real call does authenticate.
	put(t, open(t, s, ""), validName, []byte("x"))
	if s.authAttempts.Load() == 0 {
		t.Fatal("the auth log does not work, the test above proves nothing")
	}
}

func TestProbeErrors(t *testing.T) {
	// Nothing listens.
	s := newTestServer(t)
	h, p := s.addr()
	_ = s.ln.Close()
	if _, err := (Driver{}).Probe(context.Background(), driver.Config{Config: cfgJSON(t, h, p, "", ""), Dialer: loopDialer()}); code(err) != driver.ConnectRefused {
		t.Fatalf("refused: %v", err)
	}
	// Not an SSH server.
	srv := newTestServer(t)
	srv.hangHandshake.Store(false)
	// Bad config and no dialer.
	if _, err := (Driver{}).Probe(context.Background(), driver.Config{Config: json.RawMessage(`{}`), Dialer: loopDialer()}); err == nil {
		t.Fatal("bad config")
	}
	h2, p2 := srv.addr()
	if _, err := (Driver{}).Probe(context.Background(), driver.Config{Config: cfgJSON(t, h2, p2, "", "")}); err == nil {
		t.Fatal("no dialer")
	}
}

func TestProbeHangingServerTimesOut(t *testing.T) {
	old := handshakeTimeout
	handshakeTimeout = 400 * time.Millisecond
	defer func() { handshakeTimeout = old }()
	s := newTestServer(t)
	s.hangHandshake.Store(true)
	h, p := s.addr()
	start := time.Now()
	_, err := Driver{}.Probe(context.Background(), driver.Config{Config: cfgJSON(t, h, p, "", ""), Dialer: loopDialer()})
	if code(err) != driver.Timeout || time.Since(start) > 5*time.Second {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
}

func TestOpenNeedsAPinAndASecret(t *testing.T) {
	s := newTestServer(t)
	h, p := s.addr()
	if _, err := (Driver{}).Open(context.Background(), driver.Config{Config: cfgJSON(t, h, p, "", ""), Secrets: secJSON("x", ""), Dialer: loopDialer()}); code(err) != driver.HostKeyChanged {
		t.Fatalf("no pin: %v", err)
	}
	pin := s.fingerprint()
	for _, sec := range []string{``, `{}`, `{"password":""}`, `[`, `{"privateKey":"not a key"}`} {
		_, err := (Driver{}).Open(context.Background(), driver.Config{Config: cfgJSON(t, h, p, pin, ""), Secrets: json.RawMessage(sec), Dialer: loopDialer()})
		if code(err) != driver.SecretsUnreadable {
			t.Errorf("secrets %q: %v", sec, err)
		}
	}
	if _, err := (Driver{}).Open(context.Background(), driver.Config{Config: cfgJSON(t, h, p, pin, ""), Secrets: secJSON("x", "")}); err == nil {
		t.Fatal("no dialer")
	}
	if _, err := (Driver{}).Open(context.Background(), driver.Config{Config: json.RawMessage(`{}`), Secrets: secJSON("x", ""), Dialer: loopDialer()}); err == nil {
		t.Fatal("bad config")
	}
	s.settle()
	if s.conns.Load() != 0 {
		t.Fatalf("Open dialed (%d)", s.conns.Load())
	}
}

func TestContract(t *testing.T) {
	s := newTestServer(t)
	r := open(t, s, "backups/db")
	ctx := context.Background()

	// A folder that does not exist lists as empty (nothing was uploaded yet).
	if es, err := r.List(ctx, "launchpad-"); err != nil || len(es) != 0 {
		t.Fatalf("empty: %v %v", es, err)
	}
	a := bytes.Repeat([]byte("0123456789abcdef"), 40000) // 640 KB: several packets
	put(t, r, validName, a)
	put(t, r, validName2, []byte(`{"m":1}`))
	es, err := r.List(ctx, "launchpad-0a1b2c3d-")
	if err != nil || len(es) != 2 {
		t.Fatalf("%v %v", es, err)
	}
	sizes := map[string]int64{}
	for _, e := range es {
		sizes[e.Name] = e.Size
	}
	if sizes[validName] != int64(len(a)) || sizes[validName2] != 7 {
		t.Fatalf("%v", sizes)
	}
	if es, _ := r.List(ctx, "launchpad-ffffffff-"); len(es) != 0 {
		t.Fatalf("prefix filter: %v", es)
	}
	if got := readAll(t, r, validName); !bytes.Equal(got, a) {
		t.Fatal("download differs")
	}
	// A second Put of the same name replaces it (no posix-rename on this server).
	put(t, r, validName, []byte("second"))
	if got := readAll(t, r, validName); string(got) != "second" {
		t.Fatalf("%q", got)
	}
	// No temporary file stays.
	for _, f := range memNames(t, s) {
		if strings.Contains(f, partPrefix) {
			t.Fatalf("leftover %s", f)
		}
	}
	if err := r.Delete(ctx, validName); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, validName); err != nil {
		t.Fatalf("deleting a missing name must not fail: %v", err)
	}
	if _, err := r.Get(ctx, validName); code(err) != driver.NotFound {
		t.Fatalf("get missing: %v", err)
	}
	// A zero-byte object.
	put(t, r, validName, nil)
	if got := readAll(t, r, validName); len(got) != 0 {
		t.Fatal("empty object")
	}
}

// memNames lists every file of the in-memory server through a plain client.
func memNames(t *testing.T, s *testServer) []string {
	t.Helper()
	h, p := s.addr()
	r := &remote{p: mustParse(t, cfgJSON(t, h, p, s.fingerprint(), "")), dialer: loopDialer()}
	auth, _ := secrets{Password: testPass}.authMethods()
	r.auth = auth
	ss, se := r.connect(context.Background())
	if se != nil {
		t.Fatal(se.err)
	}
	defer ss.shut()
	var out []string
	var walk func(string)
	walk = func(dir string) {
		fis, err := ss.c.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, fi := range fis {
			p := strings.TrimSuffix(dir, "/") + "/" + fi.Name()
			out = append(out, p)
			if fi.IsDir() {
				walk(p)
			}
		}
	}
	walk("/")
	return out
}

func mustParse(t *testing.T, raw json.RawMessage) parsed {
	t.Helper()
	p, err := parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNamesAreCheckedBeforeAnyConnection(t *testing.T) {
	s := newTestServer(t)
	r := open(t, s, "")
	ctx := context.Background()
	for _, n := range []string{"", "../etc/passwd", "a/b", validName + "/x", ".", "..", strings.Repeat("a", 300), partPrefix + "0123"} {
		if err := r.Put(ctx, n, 1, bytes.NewReader([]byte("x"))); code(err) != driver.NotFound {
			t.Errorf("put %q: %v", n, err)
		}
		if _, err := r.Get(ctx, n); code(err) != driver.NotFound {
			t.Errorf("get %q: %v", n, err)
		}
		if err := r.Delete(ctx, n); code(err) != driver.NotFound {
			t.Errorf("delete %q: %v", n, err)
		}
	}
	for _, pfx := range []string{"../", "a/b", "launchpad- ", strings.Repeat("a", 129), "é"} {
		if _, err := r.List(ctx, pfx); code(err) != driver.NotFound {
			t.Errorf("list %q: %v", pfx, err)
		}
	}
	if err := r.Put(ctx, validName, -1, bytes.NewReader(nil)); code(err) != driver.TooLarge {
		t.Errorf("negative size: %v", err)
	}
	s.settle()
	if s.conns.Load() != 0 {
		t.Fatalf("a refused name dialed (%d connections)", s.conns.Load())
	}
}

func TestTestSteps(t *testing.T) {
	s := newTestServer(t)
	r := open(t, s, "bk")
	steps := r.Test(context.Background(), testFile)
	want := []string{"connect", "auth", "write", "read", "delete"}
	if len(steps) != 5 {
		t.Fatalf("%+v", steps)
	}
	for i, st := range steps {
		if st.Name != want[i] || !st.OK || st.Code != "" {
			t.Fatalf("%+v", steps)
		}
	}
	for _, f := range memNames(t, s) {
		if strings.Contains(f, "test") || strings.Contains(f, partPrefix) {
			t.Fatalf("the test left %s", f)
		}
	}
	if steps := r.Test(context.Background(), "../x"); len(steps) != 3 || steps[2].Name != "write" || steps[2].Code != driver.NotFound {
		t.Fatalf("%+v", steps)
	}
}

func TestTestStepsNameTheFailingStage(t *testing.T) {
	s := newTestServer(t)
	h, p := s.addr()
	pin := s.fingerprint()
	steps := func(sec json.RawMessage, pin string) []driver.Step {
		r, err := Driver{}.Open(context.Background(), driver.Config{Config: cfgJSON(t, h, p, pin, ""), Secrets: sec, Dialer: loopDialer()})
		if err != nil {
			t.Fatal(err)
		}
		return r.Test(context.Background(), testFile)
	}
	// Wrong password: connect ok, auth auth_failed.
	st := steps(secJSON("wrong", ""), pin)
	if len(st) != 2 || !st[0].OK || st[1].Name != "auth" || st[1].Code != driver.AuthFailed {
		t.Fatalf("%+v", st)
	}
	// Wrong host key: connect host_key_changed.
	st = steps(secJSON(testPass, ""), "SHA256:"+strings.Repeat("Q", 43))
	if len(st) != 1 || st[0].Name != "connect" || st[0].Code != driver.HostKeyChanged {
		t.Fatalf("%+v", st)
	}
	// No sftp on the server: auth step, server_error.
	s.noSFTP.Store(true)
	st = steps(secJSON(testPass, ""), pin)
	if len(st) != 2 || !st[0].OK || st[1].Name != "auth" || st[1].Code != driver.ServerError {
		t.Fatalf("%+v", st)
	}
	// Nothing listening: connect connect_refused.
	_ = s.ln.Close()
	st = steps(secJSON(testPass, ""), pin)
	if len(st) != 1 || st[0].Name != "connect" || st[0].Code != driver.ConnectRefused {
		t.Fatalf("%+v", st)
	}
}

func TestAuth(t *testing.T) {
	s := newTestServer(t)
	h, p := s.addr()
	pin := s.fingerprint()
	do := func(sec json.RawMessage) error {
		r, err := Driver{}.Open(context.Background(), driver.Config{Config: cfgJSON(t, h, p, pin, ""), Secrets: sec, Dialer: loopDialer()})
		if err != nil {
			return err
		}
		return r.Put(context.Background(), validName, 1, strings.NewReader("x"))
	}
	if err := do(secJSON(testPass, "")); err != nil {
		t.Fatalf("password: %v", err)
	}
	if err := do(secJSON("nope", "")); code(err) != driver.AuthFailed {
		t.Fatalf("bad password: %v", err)
	}

	// Key auth (ed25519 and RSA are both accepted by ParsePrivateKey).
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	blk, _ := ssh.MarshalPrivateKey(priv, "")
	keyPEM := string(pem.EncodeToMemory(blk))
	sp, _ := ssh.NewPublicKey(pub)
	s.mu.Lock()
	s.userKey = sp
	s.mu.Unlock()
	if err := do(secJSON("", keyPEM)); err != nil {
		t.Fatalf("key: %v", err)
	}
	// Both given: the key works even when the password is wrong, and the other way round.
	if err := do(secJSON("wrong", keyPEM)); err != nil {
		t.Fatalf("key+bad password: %v", err)
	}
	// A key the server does not know.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	ob, _ := ssh.MarshalPrivateKey(other, "")
	if err := do(secJSON("", string(pem.EncodeToMemory(ob)))); code(err) != driver.AuthFailed {
		t.Fatalf("unknown key: %v", err)
	}
	if err := do(secJSON(testPass, string(pem.EncodeToMemory(ob)))); err != nil {
		t.Fatalf("unknown key + good password: %v", err)
	}
	// A key with a passphrase cannot be used (no passphrase secret exists).
	eb, _ := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("pp"))
	if err := do(secJSON("", string(pem.EncodeToMemory(eb)))); code(err) != driver.SecretsUnreadable {
		t.Fatalf("passphrase key: %v", err)
	}
	// Right user key, wrong user name: the config's user is what is sent.
	// Keyboard-interactive-only servers accept the password too.
	s.mu.Lock()
	s.kbdOnly = true
	s.mu.Unlock()
	if err := do(secJSON(testPass, "")); err != nil {
		t.Fatalf("keyboard-interactive: %v", err)
	}
	if err := do(secJSON("bad", "")); code(err) != driver.AuthFailed {
		t.Fatalf("keyboard-interactive bad: %v", err)
	}
}

func TestHostKeyChangeNeverSendsCredentialsAndNeverUpdates(t *testing.T) {
	s := newTestServer(t)
	r := open(t, s, "")
	put(t, r, validName, []byte("x"))
	before := s.authAttempts.Load()
	s.setHostKey(newSigner(t)) // the server's key changes (or someone else answers)
	ctx := context.Background()
	if err := r.Put(ctx, validName2, 1, strings.NewReader("y")); code(err) != driver.HostKeyChanged {
		t.Fatalf("put: %v", err)
	}
	if _, err := r.List(ctx, ""); code(err) != driver.HostKeyChanged {
		t.Fatalf("list: %v", err)
	}
	if _, err := r.Get(ctx, validName); code(err) != driver.HostKeyChanged {
		t.Fatalf("get: %v", err)
	}
	if err := r.Delete(ctx, validName); code(err) != driver.HostKeyChanged {
		t.Fatalf("delete: %v", err)
	}
	if st := r.Test(ctx, testFile); len(st) != 1 || st[0].Code != driver.HostKeyChanged {
		t.Fatalf("%+v", st)
	}
	s.settle()
	if after := s.authAttempts.Load(); after != before {
		t.Fatalf("credentials went to a server with another host key (%d -> %d auth requests)", before, after)
	}
	// Every call still fails the same way: nothing updated the pin.
	if err := r.Put(ctx, validName2, 1, strings.NewReader("y")); code(err) != driver.HostKeyChanged {
		t.Fatalf("second try: %v", err)
	}
}

func TestSSRFRefusals(t *testing.T) {
	s := newTestServer(t)
	h, p := s.addr()
	pol, err := backup.Policy("", backup.Gateways{})
	if err != nil {
		t.Fatal(err)
	}
	d := backup.NewDialer(pol, 2*time.Second)
	if _, err := (Driver{}).Probe(context.Background(), driver.Config{Config: cfgJSON(t, h, p, "", ""), Dialer: d}); code(err) != driver.SSRFBlocked {
		t.Fatalf("probe of loopback: %v", err)
	}
	r, err := Driver{}.Open(context.Background(), driver.Config{Config: cfgJSON(t, h, p, s.fingerprint(), ""), Secrets: secJSON(testPass, ""), Dialer: d})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Put(context.Background(), validName, 1, strings.NewReader("x")); code(err) != driver.SSRFBlocked {
		t.Fatalf("put to loopback: %v", err)
	}
	if st := r.Test(context.Background(), testFile); len(st) != 1 || st[0].Code != driver.SSRFBlocked {
		t.Fatalf("%+v", st)
	}
	// A name that resolves to loopback; then to a public address and a private one (rebinding).
	res := &netguardtest.MapResolver{Hosts: map[string][]string{"sftp.example.test": {"127.0.0.1"}}}
	d2 := &netguard.Dialer{Policy: pol, Resolver: res, Timeout: 2 * time.Second}
	cfg := cfgJSON(t, "sftp.example.test", p, "", "")
	if _, err := (Driver{}).Probe(context.Background(), driver.Config{Config: cfg, Dialer: d2}); code(err) != driver.SSRFBlocked {
		t.Fatalf("name resolving to loopback: %v", err)
	}
	for _, ip := range []string{"169.254.169.254", "10.0.0.5", "::1"} {
		res.Hosts["sftp.example.test"] = []string{ip}
		if _, err := (Driver{}).Probe(context.Background(), driver.Config{Config: cfg, Dialer: d2}); code(err) != driver.SSRFBlocked {
			t.Fatalf("%s: %v", ip, err)
		}
	}
	s.settle()
	if s.conns.Load() != 0 {
		t.Fatalf("a refused address still connected (%d)", s.conns.Load())
	}
	// Unresolvable.
	res.Hosts = map[string][]string{}
	if _, err := (Driver{}).Probe(context.Background(), driver.Config{Config: cfg, Dialer: d2}); err == nil {
		t.Fatalf("unresolvable: %v", err)
	}
}

// ---- hostile server behaviour ---------------------------------------------------------

// hookFS wraps the in-memory handlers; each hook may take over a request.
type hookFS struct {
	// delay is added to every ReadAt and WriteAt; inflight / maxRead / maxWrite count the
	// requests the server is serving at once (a client that waits one round trip per
	// packet never has more than one).
	delay    time.Duration
	inflight atomic.Int64
	maxRead  atomic.Int64
	maxWrite atomic.Int64
	base     gosftp.Handlers
	read     func(*gosftp.Request) (io.ReaderAt, error, bool)
	write    func(*gosftp.Request) (io.WriterAt, error, bool)
	cmd      func(*gosftp.Request) (error, bool)
	list     func(*gosftp.Request) (gosftp.ListerAt, error, bool)
}

func (h *hookFS) Fileread(r *gosftp.Request) (io.ReaderAt, error) {
	if h.read != nil {
		if v, err, ok := h.read(r); ok {
			return v, err
		}
	}
	ra, err := h.base.FileGet.Fileread(r)
	if err != nil || h.delay == 0 {
		return ra, err
	}
	return slowReader{h, ra}, nil
}

func (h *hookFS) Filewrite(r *gosftp.Request) (io.WriterAt, error) {
	if h.write != nil {
		if v, err, ok := h.write(r); ok {
			return v, err
		}
	}
	wa, err := h.base.FilePut.Filewrite(r)
	if err != nil || h.delay == 0 {
		return wa, err
	}
	return slowWriter{h, wa}, nil
}

func (h *hookFS) enter(max *atomic.Int64) {
	n := h.inflight.Add(1)
	for {
		m := max.Load()
		if n <= m || max.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(h.delay)
}

type slowReader struct {
	h *hookFS
	r io.ReaderAt
}

func (s slowReader) ReadAt(p []byte, off int64) (int, error) {
	s.h.enter(&s.h.maxRead)
	defer s.h.inflight.Add(-1)
	return s.r.ReadAt(p, off)
}

type slowWriter struct {
	h *hookFS
	w io.WriterAt
}

func (s slowWriter) WriteAt(p []byte, off int64) (int, error) {
	s.h.enter(&s.h.maxWrite)
	defer s.h.inflight.Add(-1)
	return s.w.WriteAt(p, off)
}

func (h *hookFS) Filecmd(r *gosftp.Request) error {
	if h.cmd != nil {
		if err, ok := h.cmd(r); ok {
			return err
		}
	}
	return h.base.FileCmd.Filecmd(r)
}

func (h *hookFS) Filelist(r *gosftp.Request) (gosftp.ListerAt, error) {
	if h.list != nil {
		if v, err, ok := h.list(r); ok {
			return v, err
		}
	}
	return h.base.FileList.Filelist(r)
}

// Lstat must not follow symlinks (the library falls back to Stat, which does, when the
// handler has no Lstat).
func (h *hookFS) Lstat(r *gosftp.Request) (gosftp.ListerAt, error) {
	return h.base.FileList.(gosftp.LstatFileLister).Lstat(r)
}

func hooked(s *testServer) *hookFS {
	base := gosftp.InMemHandler()
	h := &hookFS{base: base}
	s.handlers = func() gosftp.Handlers { return gosftp.Handlers{FileGet: h, FilePut: h, FileCmd: h, FileList: h} }
	return h
}

type fakeInfo struct {
	name string
	mode os.FileMode
	size int64
}

func (f fakeInfo) Name() string       { return f.name }
func (f fakeInfo) Size() int64        { return f.size }
func (f fakeInfo) Mode() os.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Unix(1700000000, 0) }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return nil }

type fakeLister struct {
	infos func(i int) os.FileInfo
	n     int
}

func (l fakeLister) ListAt(ls []os.FileInfo, off int64) (int, error) {
	if off >= int64(l.n) {
		return 0, io.EOF
	}
	n := 0
	for n < len(ls) && off+int64(n) < int64(l.n) {
		ls[n] = l.infos(int(off) + n)
		n++
	}
	return n, nil
}

func TestListDropsEverythingThatIsNotOurRegularFile(t *testing.T) {
	s := newTestServer(t)
	h := hooked(s)
	good := "launchpad-0a1b2c3d-20260105T030405Z-0001.dump.age"
	names := []fakeInfo{
		{good, 0o644, 11},
		{"launchpad-0a1b2c3d-20260105T030405Z-0002.dump.age", os.ModeSymlink | 0o777, 5},   // a symlink under a valid name
		{"launchpad-0a1b2c3d-20260105T030405Z-0003.dump.age", os.ModeDir | 0o755, 0},       // a directory under a valid name
		{"launchpad-0a1b2c3d-20260105T030405Z-0004.dump.age", os.ModeNamedPipe | 0o644, 0}, // a pipe
		{"launchpad-0a1b2c3d-20260105T030405Z-0005.dump.age", os.ModeDevice | 0o644, 0},    // a device
		{"../../etc/passwd", 0o644, 1},                                          // traversal
		{"/etc/passwd", 0o644, 1},                                               // absolute
		{"launchpad-0a1b2c3d-20260105T030405Z-0007.dump.age.part", 0o644, 1},    // stranger
		{"launchpad-0a1b2c3d-20260105T030405Z-000a.dump.age\n", 0o644, 1},       // newline
		{"launchpad-0a1b2c3d-20260105T030405Z-000b.dump.age\x00evil", 0o644, 1}, // NUL
		{"launchpad-0a1b2c3d-20260105T030405Z-000c.dump.age ", 0o644, 1},        // space
		{"LAUNCHPAD-0a1b2c3d-20260105T030405Z-000d.dump.age", 0o644, 1},         // case
		{"launchpad-0a1b2c3d-test-0123456789abcdef.tmp", 0o644, 4},              // the test file
		{partPrefix + "0123456789abcdef", 0o644, 1},                             // our own temp
		{"launchpad-0a1b2c3d-20260105T030405Z-000e.json", 0o644, 2},             // the manifest
	}
	h.list = func(r *gosftp.Request) (gosftp.ListerAt, error, bool) {
		if r.Method != "List" {
			return nil, nil, false
		}
		return fakeLister{n: len(names), infos: func(i int) os.FileInfo { return names[i] }}, nil, true
	}
	r := open(t, s, "")
	es, err := r.List(context.Background(), "launchpad-")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, e := range es {
		got[e.Name] = e.Size
	}
	want := map[string]int64{good: 11, "launchpad-0a1b2c3d-test-0123456789abcdef.tmp": 4, "launchpad-0a1b2c3d-20260105T030405Z-000e.json": 2}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestHugeListingIsCutAtTheCap(t *testing.T) {
	s := newTestServer(t)
	h := hooked(s)
	long := strings.Repeat("x", 220)
	h.list = func(r *gosftp.Request) (gosftp.ListerAt, error, bool) {
		if r.Method != "List" {
			return nil, nil, false
		}
		// 100 million entries: far above anything that fits 16 MiB.
		return fakeLister{n: 100_000_000, infos: func(i int) os.FileInfo {
			return fakeInfo{name: fmt.Sprintf("%s%d", long, i), mode: 0o644, size: 1}
		}}, nil, true
	}
	r := open(t, s, "")
	start := time.Now()
	es, err := r.List(context.Background(), "launchpad-")
	if code(err) != driver.TooLarge || es != nil {
		t.Fatalf("%d entries, %v", len(es), err)
	}
	if time.Since(start) > 30*time.Second {
		t.Fatalf("took %v", time.Since(start))
	}
}

func TestListFolderErrors(t *testing.T) {
	s := newTestServer(t)
	h := hooked(s)
	h.list = func(r *gosftp.Request) (gosftp.ListerAt, error, bool) {
		if r.Method == "List" {
			return nil, gosftp.ErrSSHFxPermissionDenied, true
		}
		return nil, nil, false
	}
	r := open(t, s, "")
	if _, err := r.List(context.Background(), ""); code(err) != driver.Forbidden {
		t.Fatalf("%v", err)
	}
}

func TestPutRefusedAndFullDisk(t *testing.T) {
	s := newTestServer(t)
	h := hooked(s)
	r := open(t, s, "")
	ctx := context.Background()
	h.write = func(*gosftp.Request) (io.WriterAt, error, bool) { return nil, gosftp.ErrSSHFxPermissionDenied, true }
	if err := r.Put(ctx, validName, 1, strings.NewReader("x")); code(err) != driver.Forbidden {
		t.Fatalf("refused write: %v", err)
	}
	h.write = func(*gosftp.Request) (io.WriterAt, error, bool) { return full{}, nil, true }
	err := r.Put(ctx, validName, 100000, bytes.NewReader(make([]byte, 100000)))
	if code(err) != driver.QuotaExceeded {
		t.Fatalf("full disk: %v", err)
	}
	// Nothing is left behind and nothing is a backup.
	h.write = nil
	for _, f := range memNames(t, s) {
		t.Fatalf("leftover after a failed upload: %s", f)
	}
	h.write = func(*gosftp.Request) (io.WriterAt, error, bool) { return full{text: "Disk quota exceeded"}, nil, true }
	if err := r.Put(ctx, validName, 10, strings.NewReader("0123456789")); code(err) != driver.QuotaExceeded {
		t.Fatalf("quota: %v", err)
	}
}

type full struct{ text string }

func (f full) WriteAt(p []byte, off int64) (int, error) {
	t := f.text
	if t == "" {
		t = "write: no space left on device"
	}
	return 0, errors.New(t)
}

func TestPutSourceMustMatchTheDeclaredSize(t *testing.T) {
	s := newTestServer(t)
	r := open(t, s, "")
	ctx := context.Background()
	if err := r.Put(ctx, validName, 100, strings.NewReader("short")); code(err) != driver.DumpMismatch {
		t.Fatalf("short source: %v", err)
	}
	if err := r.Put(ctx, validName, 3, strings.NewReader("longer than declared")); code(err) != driver.DumpMismatch {
		t.Fatalf("long source: %v", err)
	}
	if err := r.Put(ctx, validName, 10, errReader{}); err == nil || code(err) == driver.DumpMismatch {
		t.Fatalf("source read error: %v", err)
	}
	if fs := memNames(t, s); len(fs) != 0 {
		t.Fatalf("a failed upload left %v", fs)
	}
	if es, _ := r.List(ctx, ""); len(es) != 0 {
		t.Fatalf("%v", es)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("disk read failed: /secret/path") }

func TestStoredSizeIsChecked(t *testing.T) {
	// A server that stores fewer bytes than it was sent (silently truncates).
	s := newTestServer(t)
	h := hooked(s)
	h.write = func(r *gosftp.Request) (io.WriterAt, error, bool) {
		w, err := h.base.FilePut.Filewrite(r)
		if err != nil {
			return nil, err, true
		}
		return dropTail{w}, nil, true
	}
	r := open(t, s, "")
	if err := r.Put(context.Background(), validName, 10, strings.NewReader("0123456789")); code(err) != driver.ServerError {
		t.Fatalf("%v", err)
	}
	if es, _ := r.List(context.Background(), ""); len(es) != 0 {
		t.Fatalf("a truncated upload became a backup: %v", es)
	}
}

type dropTail struct{ w io.WriterAt }

func (d dropTail) WriteAt(p []byte, off int64) (int, error) {
	if len(p) > 1 {
		if _, err := d.w.WriteAt(p[:len(p)-1], off); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	return len(p), nil // swallow a one-byte write
}

func TestRenameFailureRemovesTheTemporaryFile(t *testing.T) {
	s := newTestServer(t)
	h := hooked(s)
	h.cmd = func(r *gosftp.Request) (error, bool) {
		if r.Method == "Rename" || r.Method == "PosixRename" {
			return gosftp.ErrSSHFxPermissionDenied, true
		}
		return nil, false
	}
	r := open(t, s, "")
	if err := r.Put(context.Background(), validName, 3, strings.NewReader("abc")); code(err) != driver.Forbidden {
		t.Fatalf("%v", err)
	}
	if fs := memNames(t, s); len(fs) != 0 {
		t.Fatalf("left %v", fs)
	}
}

func TestServerWordsAndSecretsNeverReachAnError(t *testing.T) {
	const words = "SERVER-WORDS-do-not-echo-9f3a"
	s := newTestServer(t)
	h := hooked(s)
	s.password = "PLANTED-PASSWORD-77"
	h.write = func(*gosftp.Request) (io.WriterAt, error, bool) {
		return nil, errors.New(words + " /srv/secret/path"), true
	}
	h.read = func(*gosftp.Request) (io.ReaderAt, error, bool) { return nil, errors.New(words), true }
	h.cmd = func(*gosftp.Request) (error, bool) { return errors.New(words), true }
	h.list = func(r *gosftp.Request) (gosftp.ListerAt, error, bool) {
		if r.Method == "List" {
			return nil, errors.New(words), true
		}
		return nil, nil, false
	}
	hh, p := s.addr()
	pin := s.fingerprint()
	var errs []error
	mk := func(pw string) driver.Remote {
		r, err := Driver{}.Open(context.Background(), driver.Config{Config: cfgJSON(t, hh, p, pin, ""), Secrets: secJSON(pw, ""), Dialer: loopDialer()})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := mk(s.password)
	ctx := context.Background()
	errs = append(errs, r.Put(ctx, validName, 1, strings.NewReader("x")), r.Delete(ctx, validName))
	_, e := r.List(ctx, "")
	errs = append(errs, e)
	_, e = r.Get(ctx, validName)
	errs = append(errs, e)
	// A wrong password and a wrong pin.
	errs = append(errs, mk("PLANTED-WRONG-PASSWORD").Put(ctx, validName, 1, strings.NewReader("x")))
	bad, _ := Driver{}.Open(ctx, driver.Config{Config: cfgJSON(t, hh, p, "SHA256:"+strings.Repeat("Y", 43), ""), Secrets: secJSON(s.password, ""), Dialer: loopDialer()})
	errs = append(errs, bad.Put(ctx, validName, 1, strings.NewReader("x")))
	for _, st := range r.Test(ctx, testFile) {
		if st.Code != "" {
			errs = append(errs, driver.Errf(st.Code))
		}
	}
	nonNil := 0
	for _, err := range errs {
		if err == nil {
			continue
		}
		nonNil++
		var de *driver.Error
		if !errors.As(err, &de) {
			t.Errorf("not a *driver.Error: %T", err)
		}
		for _, bad := range []string{words, "/srv", "PLANTED", pin, hh, "secret", "backup"} {
			if strings.Contains(err.Error(), bad) {
				t.Errorf("error %q contains %q", err, bad)
			}
		}
	}
	if nonNil < 5 {
		t.Fatalf("only %d errors, the hooks did not fire", nonNil)
	}
}

// ---- stalls and timeouts ------------------------------------------------------------------

func fastStall(t *testing.T) {
	old := stallAfter
	stallAfter = 400 * time.Millisecond
	t.Cleanup(func() { stallAfter = old })
}

func TestServerThatNeverStartsSFTPIsStalled(t *testing.T) {
	fastStall(t)
	s := newTestServer(t)
	s.hangSFTP.Store(true)
	r := open(t, s, "")
	start := time.Now()
	err := r.Put(context.Background(), validName, 1, strings.NewReader("x"))
	if code(err) != driver.Stalled || time.Since(start) > 10*time.Second {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
	st := r.Test(context.Background(), testFile)
	if len(st) == 0 || st[len(st)-1].Code != driver.Stalled {
		t.Fatalf("%+v", st)
	}
}

func TestServerThatNeverAnswersTheSSHBanner(t *testing.T) {
	old := handshakeTimeout
	handshakeTimeout = 400 * time.Millisecond
	defer func() { handshakeTimeout = old }()
	s := newTestServer(t)
	r := open(t, s, "") // pinned while the server was fine
	s.hangHandshake.Store(true)
	start := time.Now()
	if err := r.Delete(context.Background(), validName); code(err) != driver.Timeout || time.Since(start) > 5*time.Second {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
}

func TestDownloadStallsMidFile(t *testing.T) {
	fastStall(t)
	s := newTestServer(t)
	h := hooked(s)
	r := open(t, s, "")
	body := bytes.Repeat([]byte("z"), 300000)
	put(t, r, validName, body)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	h.read = func(rq *gosftp.Request) (io.ReaderAt, error, bool) {
		w, err := h.base.FileGet.Fileread(rq)
		if err != nil {
			return nil, err, true
		}
		return stallAt{w, release}, nil, true
	}
	rc, err := r.Get(context.Background(), validName)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	start := time.Now()
	_, err = io.ReadAll(rc)
	if code(err) != driver.Stalled || time.Since(start) > 10*time.Second {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
}

type stallAt struct {
	r       io.ReaderAt
	release chan struct{}
}

func (s stallAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= 32768 {
		<-s.release
		return 0, io.EOF
	}
	return s.r.ReadAt(p, off)
}

func TestUploadSourceThatStalls(t *testing.T) {
	fastStall(t)
	s := newTestServer(t)
	r := open(t, s, "")
	pr, pw := io.Pipe()
	go func() { _, _ = pw.Write([]byte("first bytes")) }() // then nothing
	t.Cleanup(func() { _ = pw.Close() })
	start := time.Now()
	err := r.Put(context.Background(), validName, 1000, pr)
	if code(err) != driver.Stalled || time.Since(start) > 10*time.Second {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
	// The connection was cut mid-copy; a fresh connection removed the temp file.
	if fs := memNames(t, s); len(fs) != 0 {
		t.Fatalf("an orphaned temp file stayed: %v", fs)
	}
}

func TestContextCancelAndDeadline(t *testing.T) {
	s := newTestServer(t)
	r := open(t, s, "")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })
	start := time.Now()
	err := r.Put(ctx, validName, 1000, pr)
	if code(err) != driver.Timeout || time.Since(start) > 5*time.Second {
		t.Fatalf("deadline: %v after %v", err, time.Since(start))
	}
	// The expired context could not clean up; a fresh one did, and the error is unchanged.
	if fs := memNames(t, s); len(fs) != 0 {
		t.Fatalf("left %v", fs)
	}
	cctx, ccancel := context.WithCancel(context.Background())
	ccancel()
	if _, err := r.List(cctx, ""); err == nil {
		t.Fatal("a cancelled context must fail")
	}
}

func TestGetReaderEndsWithTheConnection(t *testing.T) {
	s := newTestServer(t)
	r := open(t, s, "")
	put(t, r, validName, []byte("hello"))
	rc, err := r.Get(context.Background(), validName)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 2)
	if _, err := io.ReadFull(rc, b); err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if _, err := rc.Read(b); err == nil {
		t.Fatal("read after close")
	}
	// The caller's cap works on the reader. (The in-memory server keeps a sticky error on a
	// file whose transfer was cut, so the object is written again first.)
	put(t, r, validName, []byte("hello"))
	rc, _ = r.Get(context.Background(), validName)
	defer rc.Close()
	if _, err := io.ReadAll(driver.CapReader(rc, 2)); code(err) != driver.TooLarge {
		t.Fatalf("%v", err)
	}
}

func TestMapSFTP(t *testing.T) {
	cases := []struct {
		err  error
		want driver.Code
		st   int
	}{
		{os.ErrNotExist, driver.NotFound, 0},
		{os.ErrPermission, driver.Forbidden, 0},
		{&gosftp.StatusError{Code: 2}, driver.NotFound, 0},
		{&gosftp.StatusError{Code: 3}, driver.Forbidden, 0},
		{&gosftp.StatusError{Code: 14}, driver.QuotaExceeded, 0},
		{&gosftp.StatusError{Code: 15}, driver.QuotaExceeded, 0},
		{&gosftp.StatusError{Code: 4}, driver.ServerError, 4},
		{&gosftp.StatusError{Code: 8}, driver.ServerError, 8},
		{io.EOF, driver.ServerError, 0},
		{errors.New("anything else"), driver.ServerError, 0},
	}
	for _, c := range cases {
		e := mapSFTP(c.err)
		if e.Code != c.want || e.Status != c.st {
			t.Errorf("%v: %+v", c.err, e)
		}
	}
}

// ---- the real file system (posix-rename, symlinks) -----------------------------------

func TestRealServerPosixRenameAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t)
	s.realDir = dir
	if err := os.MkdirAll(filepath.Join(dir, "bk"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dir, "outside.txt")
	if err := os.WriteFile(outside, []byte("TARGET"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink sits at a valid backup name.
	linkName := "launchpad-0a1b2c3d-20260106T030405Z-0001.dump.age"
	if err := os.Symlink(outside, filepath.Join(dir, "bk", linkName)); err != nil {
		t.Fatal(err)
	}
	r := open(t, s, "bk")
	ctx := context.Background()
	if es, err := r.List(ctx, "launchpad-"); err != nil || len(es) != 0 {
		t.Fatalf("a symlink was listed: %v %v", es, err)
	}
	if _, err := r.Get(ctx, linkName); code(err) != driver.NotFound {
		t.Fatalf("a symlink was read: %v", err)
	}
	// Put over the link replaces the link, never writes through it.
	put(t, r, linkName, []byte("NEW"))
	if b, _ := os.ReadFile(outside); string(b) != "TARGET" {
		t.Fatalf("wrote through the symlink: %q", b)
	}
	fi, err := os.Lstat(filepath.Join(dir, "bk", linkName))
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("%v %v", fi, err)
	}
	if got := readAll(t, r, linkName); string(got) != "NEW" {
		t.Fatalf("%q", got)
	}
	// A dangling temp symlink guess cannot be followed: O_EXCL. (Names are random; nothing to plant.)
	ents, _ := os.ReadDir(filepath.Join(dir, "bk"))
	if len(ents) != 1 {
		t.Fatalf("%v", ents)
	}
	// Replace again (posix-rename replaces the existing regular file).
	put(t, r, linkName, []byte("NEWER"))
	if got := readAll(t, r, linkName); string(got) != "NEWER" {
		t.Fatalf("%q", got)
	}
	// Test steps and folder creation on the real fs.
	r2 := open(t, s, "a/b/c")
	for _, st := range r2.Test(ctx, testFile) {
		if !st.OK {
			t.Fatalf("%+v", st)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "a/b/c")); err != nil {
		t.Fatal(err)
	}
	// A folder that is a file.
	if err := os.WriteFile(filepath.Join(dir, "afile"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r3 := open(t, s, "afile")
	if err := r3.Put(ctx, validName, 1, strings.NewReader("x")); code(err) != driver.NotFound {
		t.Fatalf("folder is a file: %v", err)
	}
	// Delete.
	if err := r.Delete(ctx, linkName); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "bk", linkName)); !os.IsNotExist(err) {
		t.Fatal("not deleted")
	}
}

func TestConcurrentCallsAreIndependent(t *testing.T) {
	s := newTestServer(t)
	// A real file system: the library's in-memory server has unsynchronised renames and
	// listings (a data race of the TEST server, which the sweep after each Put now exercises).
	s.realDir = t.TempDir()
	if err := os.Mkdir(filepath.Join(s.realDir, "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := open(t, s, "c")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("launchpad-0a1b2c3d-20260107T0304%02dZ-%04x.dump.age", i, i)
			body := bytes.Repeat([]byte{byte(i)}, 50000+i)
			if err := r.Put(context.Background(), name, int64(len(body)), bytes.NewReader(body)); err != nil {
				t.Error(err)
				return
			}
			rc, err := r.Get(context.Background(), name)
			if err != nil {
				t.Error(err)
				return
			}
			got, _ := io.ReadAll(rc)
			_ = rc.Close()
			if !bytes.Equal(got, body) {
				t.Errorf("%s differs", name)
			}
		}(i)
	}
	wg.Wait()
	if es, err := r.List(context.Background(), "launchpad-"); err != nil || len(es) != 8 {
		t.Fatalf("%v %v", es, err)
	}
}

func TestSweepNeverChangesTheError(t *testing.T) {
	// The server dies mid-upload: the sweep cannot connect and the error is still the transport's.
	fastStall(t)
	s := newTestServer(t)
	r := open(t, s, "")
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("abc")) // Put reads this only after it is connected and its sftp session has started
		for s.sessions.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		_ = s.ln.Close()
	}()
	t.Cleanup(func() { _ = pw.Close() })
	err := r.Put(context.Background(), validName, 1000, pr)
	if code(err) != driver.Stalled {
		t.Fatalf("%v", err)
	}
}

func TestTransfersKeepRequestsInFlight(t *testing.T) {
	s := newTestServer(t)
	h := hooked(s)
	h.delay = 10 * time.Millisecond
	r := open(t, s, "")
	body := bytes.Repeat([]byte("0123456789abcdef"), 131072) // 2 MiB = 64 packets of 32 KiB
	if err := r.Put(context.Background(), validName, int64(len(body)), bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if m := h.maxWrite.Load(); m < 4 {
		t.Fatalf("an upload had at most %d writes in flight: one round trip per packet", m)
	}
	got := readAll(t, r, validName)
	if !bytes.Equal(got, body) {
		t.Fatal("download differs")
	}
	if m := h.maxRead.Load(); m < 4 {
		t.Fatalf("a download had at most %d reads in flight: one round trip per packet", m)
	}
	// The size check still works with concurrent writes.
	if err := r.Put(context.Background(), validName2, int64(len(body))+5, bytes.NewReader(body)); code(err) != driver.DumpMismatch {
		t.Fatalf("short source: %v", err)
	}
	if err := r.Put(context.Background(), validName2, 100, bytes.NewReader(body)); code(err) != driver.DumpMismatch {
		t.Fatalf("long source: %v", err)
	}
	if fs := memNames(t, s); len(fs) != 1 {
		t.Fatalf("%v", fs)
	}
}

func TestListEntryCap(t *testing.T) {
	old := maxEntries
	maxEntries = 100
	defer func() { maxEntries = old }()
	s := newTestServer(t)
	h := hooked(s)
	h.list = func(r *gosftp.Request) (gosftp.ListerAt, error, bool) {
		if r.Method != "List" {
			return nil, nil, false
		}
		return fakeLister{n: 500, infos: func(i int) os.FileInfo {
			return fakeInfo{name: fmt.Sprintf("launchpad-0a1b2c3d-20260105T030405Z-%04x.json", i), mode: 0o644, size: 1}
		}}, nil, true
	}
	r := open(t, s, "")
	if es, err := r.List(context.Background(), ""); code(err) != driver.TooLarge || es != nil {
		t.Fatalf("%d %v", len(es), err)
	}
	maxEntries = 500
	if es, err := r.List(context.Background(), ""); err != nil || len(es) != 500 {
		t.Fatalf("%d %v", len(es), err)
	}
}

// A caller's deadline that ends in any phase of a call is a timeout, never a
// server error, a host key or an auth error.
func TestDeadlineInAnyPhaseIsTimeout(t *testing.T) {
	s := newTestServer(t)
	r := open(t, s, "")
	put(t, r, validName, []byte("x"))
	timeouts := 0
	for d := 200 * time.Microsecond; d <= 6*time.Millisecond; d += 150 * time.Microsecond {
		for i := 0; i < 3; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), d)
			_, err := r.List(ctx, "launchpad-")
			expired := ctx.Err() != nil
			cancel()
			if err != nil && expired {
				if code(err) != driver.Timeout {
					t.Fatalf("deadline %v: %v", d, err)
				}
				timeouts++
			}
		}
	}
	_ = timeouts // how many calls hit the deadline depends on the machine; the next test is deterministic
}

// The server holds the auth request until the test lets it go; the deadline falls inside that
// window. LAUN-176: a held gate replaces a 2 s delay and an elapsed-time limit, so a slow host
// cannot fail the test: the call must come back WHILE the server is still holding the request.
func TestDeadlineInAuthWindowIsTimeoutWithTheRightPin(t *testing.T) {
	s := newTestServer(t)
	gate := make(chan struct{})
	s.mu.Lock()
	s.authGate = gate
	s.mu.Unlock()
	t.Cleanup(func() { close(gate) })
	r := open(t, s, "")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- r.Put(ctx, validName, 1, strings.NewReader("x")) }()
	select {
	case err := <-errc:
		if code(err) != driver.Timeout {
			t.Fatalf("%v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the call stayed behind the server's auth window after its deadline")
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	stc := make(chan []driver.Step, 1)
	go func() { stc <- r.Test(ctx2, testFile) }()
	select {
	case st := <-stc:
		if len(st) != 2 || !st[0].OK || st[1].Name != "auth" || st[1].Code != driver.Timeout {
			t.Fatalf("%+v", st)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Test stayed behind the server's auth window after its deadline")
	}
}

// A wrong host key is reported as such even when the context ended in the same moment.
func TestWrongPinIsNotHiddenByAnExpiredContext(t *testing.T) {
	s := newTestServer(t)
	h, p := s.addr()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	hostKeyHook = func() { <-ctx.Done() } // the callback runs after the deadline
	defer func() { hostKeyHook = nil }()
	wrong, err := Driver{}.Open(context.Background(), driver.Config{
		Config: cfgJSON(t, h, p, "SHA256:"+strings.Repeat("Q", 43), ""), Secrets: secJSON(testPass, ""), Dialer: loopDialer()})
	if err != nil {
		t.Fatal(err)
	}
	if err := wrong.Put(ctx, validName, 1, strings.NewReader("x")); code(err) != driver.HostKeyChanged {
		t.Fatalf("wrong pin, expired context: %v", err)
	}
	// The right pin with the same expiry: a timeout, in the auth stage.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	hostKeyHook = func() { <-ctx2.Done() }
	right := open(t, s, "")
	st := right.Test(ctx2, testFile)
	if len(st) != 2 || !st[0].OK || st[1].Name != "auth" || st[1].Code != driver.Timeout {
		t.Fatalf("%+v", st)
	}
}

// ---- the fallback gates ---------------------------------------------------------------

// refuseRenames makes every plain rename fail with err (posix-rename is turned off).
func refuseRenames(t *testing.T, h *hookFS, err error) {
	allowPosixRename = false
	t.Cleanup(func() { allowPosixRename = true })
	h.cmd = func(rq *gosftp.Request) (error, bool) {
		if rq.Method == "Rename" || rq.Method == "PosixRename" {
			return err, true
		}
		return nil, false
	}
}

// Status codes other than the generic failure never start the remove-and-retry. (Status 14
// cannot be sent by the library's server, so it is covered by TestMapSFTP only.)
func TestRenameOtherStatusCodesKeepTheFinalFile(t *testing.T) {
	for _, e := range []error{gosftp.ErrSSHFxOpUnsupported, gosftp.ErrSSHFxBadMessage} {
		s := newTestServer(t)
		h := hooked(s)
		r := open(t, s, "")
		put(t, r, validName, []byte("old bytes"))
		refuseRenames(t, h, e)
		err := r.Put(context.Background(), validName, 3, strings.NewReader("new"))
		if code(err) != driver.ServerError {
			t.Fatalf("%v: %v", e, err)
		}
		h.cmd = nil
		if got := readAll(t, r, validName); string(got) != "old bytes" {
			t.Fatalf("%v: the final file was touched: %q", e, got)
		}
		if fs := memNames(t, s); len(fs) != 1 {
			t.Fatalf("%v: the temp file stayed: %v", e, fs)
		}
	}
}

// A directory or a symlink at the final name is never removed by the fallback.
func TestRenameFallbackNeverRemovesADirectoryOrASymlink(t *testing.T) {
	for _, kind := range []string{"dir", "symlink"} {
		s := newTestServer(t)
		h := hooked(s)
		r := open(t, s, "")
		rr := r.(*remote)
		ss, se := rr.connect(context.Background())
		if se != nil {
			t.Fatal(se.err)
		}
		if kind == "dir" {
			if err := ss.c.Mkdir(validName); err != nil {
				t.Fatal(err)
			}
		} else {
			f, err := ss.c.Create(validName2)
			if err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
			if err := ss.c.Symlink("/target.txt", validName); err != nil {
				t.Fatal(err)
			}
		}
		ss.shut()
		refuseRenames(t, h, gosftp.ErrSSHFxFailure)
		if err := r.Put(context.Background(), validName, 3, strings.NewReader("new")); err == nil {
			t.Fatalf("%s: expected the rename error", kind)
		}
		ss, se = rr.connect(context.Background())
		if se != nil {
			t.Fatal(se.err)
		}
		fi, err := ss.c.Lstat(validName)
		ss.shut()
		if err != nil {
			t.Fatalf("%s: removed: %v", kind, err)
		}
		if kind == "dir" && !fi.IsDir() || kind == "symlink" && fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s: replaced by %v", kind, fi.Mode())
		}
	}
}

// A server that returns fewer bytes than its own Lstat size must not look like a clean stream.
func TestShortDownloadIsAnError(t *testing.T) {
	s := newTestServer(t)
	h := hooked(s)
	r := open(t, s, "")
	body := bytes.Repeat([]byte("q"), 100000)
	put(t, r, validName, body)
	h.read = func(rq *gosftp.Request) (io.ReaderAt, error, bool) {
		ra, err := h.base.FileGet.Fileread(rq)
		if err != nil {
			return nil, err, true
		}
		return cutAt{ra, 40000}, nil, true
	}
	rc, err := r.Get(context.Background(), validName)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if code(err) != driver.ServerError || len(got) > 40000 {
		t.Fatalf("%d bytes, %v", len(got), err)
	}
	if st := r.Test(context.Background(), testFile); len(st) != 5 || !st[4].OK {
		t.Fatalf("%+v", st) // a server that behaves still passes the test
	}
}

type cutAt struct {
	r   io.ReaderAt
	max int64
}

func (c cutAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= c.max {
		return 0, io.EOF
	}
	if off+int64(len(p)) > c.max {
		p = p[:c.max-off]
		n, _ := c.r.ReadAt(p, off)
		return n, io.EOF
	}
	return c.r.ReadAt(p, off)
}

// ---- the plain-rename fallback ----------------------------------------------------------

// noReplace makes a server without posix-rename that refuses to replace a file.
type noReplace struct {
	mu     sync.Mutex
	exists map[string]bool
	// afterFirst, when set, runs after the first refused rename (to break the temp file or later renames).
	onRefuse func(h *hookFS, r *gosftp.Request) (error, bool)
	refused  int
}

func replaceRefuser(t *testing.T, h *hookFS, nr *noReplace) {
	allowPosixRename = false // the in-memory server advertises posix-rename but its version refuses to replace
	t.Cleanup(func() { allowPosixRename = true })
	nr.exists = map[string]bool{}
	h.write = func(r *gosftp.Request) (io.WriterAt, error, bool) {
		nr.mu.Lock()
		nr.exists[r.Filepath] = true
		nr.mu.Unlock()
		return nil, nil, false
	}
	h.cmd = func(r *gosftp.Request) (error, bool) {
		nr.mu.Lock()
		defer nr.mu.Unlock()
		switch r.Method {
		case "Remove":
			delete(nr.exists, r.Filepath)
		case "Rename":
			if nr.onRefuse != nil && nr.refused > 0 {
				return nr.onRefuse(h, r)
			}
			if nr.exists[r.Target] {
				nr.refused++
				if nr.onRefuse != nil {
					return gosftp.ErrSSHFxFailure, true
				}
				return gosftp.ErrSSHFxFailure, true
			}
			delete(nr.exists, r.Filepath)
			nr.exists[r.Target] = true
		}
		return nil, false
	}
}

func TestRenameFallbackReplacesAnExistingFile(t *testing.T) {
	s := newTestServer(t)
	h := hooked(s)
	nr := &noReplace{}
	replaceRefuser(t, h, nr)
	r := open(t, s, "")
	put(t, r, validName, []byte("first"))
	put(t, r, validName, []byte("second")) // the first plain rename is refused; the fallback runs
	if nr.refused != 1 {
		t.Fatalf("the fallback did not run (%d refusals)", nr.refused)
	}
	if got := readAll(t, r, validName); string(got) != "second" {
		t.Fatalf("%q", got)
	}
	if fs := memNames(t, s); len(fs) != 1 {
		t.Fatalf("%v", fs)
	}
}

func TestRenameFallbackNeverRunsWhenTheTempFileIsGone(t *testing.T) {
	s := newTestServer(t)
	h := hooked(s)
	nr := &noReplace{}
	replaceRefuser(t, h, nr)
	r := open(t, s, "")
	put(t, r, validName, []byte("keep me"))
	// The refused rename also made the temp file disappear: the old file must stay untouched.
	nr.onRefuse = func(h *hookFS, rq *gosftp.Request) (error, bool) {
		_ = h.base.FileCmd.Filecmd(gosftp.NewRequest("Remove", rq.Filepath))
		return gosftp.ErrSSHFxFailure, true
	}
	nr.refused = 0
	nr.exists[validName] = true
	// Make the FIRST rename refuse through onRefuse as well.
	h.cmd = func(rq *gosftp.Request) (error, bool) {
		if rq.Method == "Rename" {
			_ = h.base.FileCmd.Filecmd(gosftp.NewRequest("Remove", rq.Filepath))
			return gosftp.ErrSSHFxFailure, true
		}
		return nil, false
	}
	if err := r.Put(context.Background(), validName, 3, strings.NewReader("new")); err == nil {
		t.Fatal("expected the rename error")
	}
	h.cmd = nil
	if got := readAll(t, r, validName); string(got) != "keep me" {
		t.Fatalf("the old file was touched: %q", got)
	}
}

func TestRenameFallbackSecondRenameFailsKeepsTheOldFile(t *testing.T) {
	s := newTestServer(t)
	h := hooked(s)
	nr := &noReplace{}
	replaceRefuser(t, h, nr)
	r := open(t, s, "")
	put(t, r, validName, []byte("old"))
	// The first rename is refused (target exists); every rename after it fails (LAUN-154: the
	// old file is NOT removed first any more).
	nr.onRefuse = func(h *hookFS, rq *gosftp.Request) (error, bool) { return gosftp.ErrSSHFxFailure, true }
	err := r.Put(context.Background(), validName, 3, strings.NewReader("new"))
	if code(err) != driver.ServerError {
		t.Fatalf("%v", err)
	}
	h.cmd = nil
	if got := readAll(t, r, validName); string(got) != "old" {
		t.Fatalf("the old file was lost: %q", got)
	}
	if fs := memNames(t, s); len(fs) != 1 {
		t.Fatalf("%v", fs)
	}
}

func TestRenameFailureOtherThanFailureCodeKeepsTheFinalFile(t *testing.T) {
	s := newTestServer(t)
	h := hooked(s)
	r := open(t, s, "")
	put(t, r, validName, []byte("old"))
	h.cmd = func(rq *gosftp.Request) (error, bool) {
		if rq.Method == "Rename" || rq.Method == "PosixRename" {
			return gosftp.ErrSSHFxPermissionDenied, true
		}
		return nil, false
	}
	if err := r.Put(context.Background(), validName, 3, strings.NewReader("new")); code(err) != driver.Forbidden {
		t.Fatalf("%v", err)
	}
	h.cmd = nil
	if got := readAll(t, r, validName); string(got) != "old" {
		t.Fatalf("%q", got)
	}
}

// ---- file mode and O_EXCL on the real file system -------------------------------------------

func TestTempFileIsPrivateAndExclusive(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t)
	s.realDir = dir
	bk := filepath.Join(dir, "bk")
	if err := os.MkdirAll(bk, 0o755); err != nil {
		t.Fatal(err)
	}
	r := open(t, s, "bk")
	ctx := context.Background()
	put(t, r, validName, []byte("secret dump"))
	fi, err := os.Stat(filepath.Join(bk, validName))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
	}

	old := randToken
	randToken = func() (string, error) { return "0123456789abcdef", nil }
	defer func() { randToken = old }()
	planted := filepath.Join(bk, partPrefix+"0123456789abcdef")
	// A planted file at the temp name: the upload fails and leaves it alone.
	if err := os.WriteFile(planted, []byte("PLANTED"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Put(ctx, validName2, 3, strings.NewReader("new")); err == nil {
		t.Fatal("O_EXCL did not refuse an existing temp file")
	}
	if b, _ := os.ReadFile(planted); string(b) != "PLANTED" {
		t.Fatalf("the planted file was touched: %q", b)
	}
	// A planted symlink at the temp name: not followed, target untouched.
	_ = os.Remove(planted)
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("VICTIM"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, planted); err != nil {
		t.Fatal(err)
	}
	if err := r.Put(ctx, validName2, 3, strings.NewReader("new")); err == nil {
		t.Fatal("O_EXCL followed or replaced a symlink")
	}
	if b, _ := os.ReadFile(victim); string(b) != "VICTIM" {
		t.Fatalf("wrote through the symlink: %q", b)
	}
	if _, err := os.Lstat(planted); err != nil {
		t.Fatalf("the planted symlink was removed: %v", err)
	}
}

// lateCtx is a context whose deadline has passed but whose timer has not set Err yet
// (a socket deadline can fire a few microseconds before the context's own).
type lateCtx struct {
	context.Context
	dl time.Time
}

func (l lateCtx) Deadline() (time.Time, bool) { return l.dl, true }

// This is a smoke test: with a passed deadline the socket itself times out, which already maps to
// timeout, so it does NOT guard the deadline clause of ctxEnded (a mutant that disables it passes).
// No deterministic test reaches the microsecond gap that clause covers.
func TestPassedDeadlineWithoutErrIsTimeout(t *testing.T) {
	s := newTestServer(t)
	s.mu.Lock()
	s.authDelay = 300 * time.Millisecond
	s.mu.Unlock()
	r := open(t, s, "")
	ctx := lateCtx{context.Background(), time.Now().Add(-time.Second)}
	if err := r.Delete(ctx, validName); code(err) != driver.Timeout {
		t.Fatalf("%v", err)
	}
	h, p := s.addr()
	if _, err := (Driver{}).Probe(ctx, driver.Config{Config: cfgJSON(t, h, p, "", ""), Dialer: loopDialer()}); code(err) != driver.Timeout {
		t.Fatalf("probe: %v", err)
	}
}

// A download that returns MORE bytes than the server's own Lstat size is an error too.
func TestLongDownloadIsAnError(t *testing.T) {
	s := newTestServer(t)
	h := hooked(s)
	r := open(t, s, "")
	put(t, r, validName, []byte("hello"))
	h.read = func(rq *gosftp.Request) (io.ReaderAt, error, bool) {
		ra, err := h.base.FileGet.Fileread(rq)
		if err != nil {
			return nil, err, true
		}
		return padded{ra, 5, []byte("EXTRA")}, nil, true
	}
	rc, err := r.Get(context.Background(), validName)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); code(err) != driver.ServerError {
		t.Fatalf("%v", err)
	}
}

// padded serves a file of `size` bytes followed by extra bytes, then ends.
type padded struct {
	r     io.ReaderAt
	size  int64
	extra []byte
}

func (p padded) ReadAt(b []byte, off int64) (int, error) {
	if off < p.size {
		if int64(len(b)) > p.size-off {
			b = b[:p.size-off]
		}
		return p.r.ReadAt(b, off)
	}
	i := off - p.size
	if i >= int64(len(p.extra)) {
		return 0, io.EOF
	}
	n := copy(b, p.extra[i:])
	if i+int64(n) >= int64(len(p.extra)) {
		return n, io.EOF
	}
	return n, nil
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
	s := newTestServer(t)
	r := open(t, s, "")
	for i := 0; i < 2; i++ {
		if err := r.Put(context.Background(), validName, 3, &lateReader{head: "abc", tail: "defgh"}); code(err) != driver.DumpMismatch {
			t.Fatalf("long source: %v", err)
		}
	}
	if fs := memNames(t, s); len(fs) != 0 {
		t.Fatalf("a long source left %v", fs)
	}
}
