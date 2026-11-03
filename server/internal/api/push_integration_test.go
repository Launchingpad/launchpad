package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/netguard"
	"devops/internal/netguard/netguardtest"
	"devops/internal/notify"
	"devops/internal/platform"
)

// Mobile push routes (LAUN-197, docs/internal/decisions-T35-mobile-push.md).

const (
	pushRelaySecret = "APIRELAYSECRET-8842"
	pushRelayToken  = "APIRELAYURLTOKEN-7731"
	pushRelayURL    = "https://relay.example.com/x/" + pushRelayToken
	pushDevToken    = "APIDEVICETOKEN-5520"
	pushPath        = O + "/push"
	pushDevPath     = O + "/me/push-devices"
)

type apiRelay struct {
	*httptest.Server
	mu     sync.Mutex
	hits   []string
	status int
}

func (f *apiRelay) set(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

func (f *apiRelay) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.hits)
}

func newPushEnv(t *testing.T, withKey bool) (*env, *apiRelay) {
	t.Helper()
	f := &apiRelay{}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.hits = append(f.hits, r.URL.Path+"|"+r.Header.Get("Authorization")+"|"+string(b))
		st := f.status
		f.mu.Unlock()
		if st == 0 {
			st = 200
		}
		w.WriteHeader(st)
	}))
	t.Cleanup(f.Close)
	d := &netguard.Dialer{Policy: netguardtest.Loopback()}
	c := notify.NewPushClientFor(d)
	tr := c.Transport.(*http.Transport)
	addr := f.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return d.DialContext(ctx, network, addr)
	}
	pool := x509.NewCertPool()
	pool.AddCert(f.Certificate())
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: "example.com"}
	e := newEnvWith(t, envOpts{configure: func(e *env, s *Server) {
		var keys *platform.PushKeyring
		if withKey {
			var err error
			if keys, err = platform.NewPushKeyring([]byte("0123456789abcdef0123456789abcdef")); err != nil {
				t.Fatal(err)
			}
		}
		log := slog.New(slog.NewTextHandler(io.MultiWriter(tWriter{t}, e.logs), &slog.HandlerOptions{Level: slog.LevelDebug}))
		s.Push = notify.NewPushService(e.pool, log, keys, notify.NewPushPoster(c))
	}})
	return e, f
}

func (e *env) pushView(token string) notify.PushView {
	e.t.Helper()
	var v notify.PushView
	e.must(200, e.do("GET", pushPath, token, nil)).json(e.t, &v)
	return v
}

func pushDev(token string) map[string]any {
	return map[string]any{"platform": "ios", "token": token, "appVersion": "1.0", "label": "Test phone"}
}

func (e *env) setRelay(token string) {
	e.t.Helper()
	e.must(200, e.do("PUT", pushPath, token, map[string]any{"relayUrl": pushRelayURL, "relaySecret": pushRelaySecret}))
}

func TestPush_AdminRoutesAuth(t *testing.T) {
	e, _ := newPushEnv(t, true)
	ann := e.newHuman("ann", "ann@example.com", "member")
	annAdminScope, _ := e.newToken(ann.ID, tokenOpts{})                                         // admin scope, member role
	readWrite, _ := e.newToken(e.user("chen").ID, tokenOpts{scopes: []string{"read", "write"}}) // org admin, no admin scope
	exp := time.Now().Add(time.Hour)
	if _, err := e.pool.Exec(context.Background(), `UPDATE org_members SET role = 'admin' WHERE user_id = $1`, e.user("claude").ID); err != nil {
		t.Fatal(err)
	}
	agentAdmin, _ := e.newToken(e.user("claude").ID, tokenOpts{expires: &exp})
	body := map[string]any{"relayUrl": pushRelayURL, "relaySecret": pushRelaySecret}
	for _, c := range []struct{ method, path string }{
		{"GET", pushPath}, {"PUT", pushPath}, {"DELETE", pushPath}, {"POST", pushPath + "/test"},
	} {
		expectCode(t, e.do(c.method, c.path, "", body), 401, "unauthenticated")
		expectCode(t, e.do(c.method, c.path, e.claude, body), 403, "insufficient_scope")
		expectCode(t, e.do(c.method, c.path, readWrite, body), 403, "insufficient_scope")
		expectCode(t, e.do(c.method, c.path, annAdminScope, body), 403, "forbidden")
		// An agent token, even with the admin scope, gets nothing (AGENTS.md: refuseAgents).
		expectCode(t, e.do(c.method, c.path, agentAdmin, body), 403, "forbidden")
	}
	if n := e.countEvents("push.updated"); n != 0 {
		t.Fatalf("a refused request emitted %d events", n)
	}
	var rows int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM push_relays`).Scan(&rows)
	if rows != 0 {
		t.Fatal("a refused request stored a relay")
	}
}

func TestPush_DeviceRoutesAuth(t *testing.T) {
	e, _ := newPushEnv(t, true)
	exp := time.Now().Add(time.Hour)
	// An agent holding every scope: only refuseAgents stops it.
	agentAll, _ := e.newToken(e.user("claude").ID, tokenOpts{expires: &exp})
	readOnly, _ := e.newToken(e.user("chen").ID, tokenOpts{scopes: []string{"read"}})
	id := uuid.NewString()
	for _, c := range []struct{ method, path string }{
		{"GET", pushDevPath}, {"PUT", pushDevPath + "/" + id}, {"DELETE", pushDevPath + "/" + id},
	} {
		expectCode(t, e.do(c.method, c.path, "", pushDev(pushDevToken)), 401, "unauthenticated")
		expectCode(t, e.do(c.method, c.path, agentAll, pushDev(pushDevToken)), 403, "forbidden")
	}
	// Read scope lists but does not write.
	e.must(200, e.do("GET", pushDevPath, readOnly, nil))
	expectCode(t, e.do("PUT", pushDevPath+"/"+id, readOnly, pushDev(pushDevToken)), 403, "insufficient_scope")
	expectCode(t, e.do("DELETE", pushDevPath+"/"+id, readOnly, nil), 403, "insufficient_scope")
	if n := e.count(`SELECT count(*) FROM push_devices`); n != 0 {
		t.Fatalf("a refused request stored %d devices", n)
	}
}

func TestPush_RelaySetGetRemove_WriteOnly(t *testing.T) {
	e, _ := newPushEnv(t, true)
	v := e.pushView(e.chen)
	if v.Configured || !v.Available || v.Host != nil {
		t.Fatalf("fresh view %+v", v)
	}
	r := e.must(200, e.do("PUT", pushPath, e.chen, map[string]any{"relayUrl": pushRelayURL, "relaySecret": pushRelaySecret}))
	for _, s := range []string{pushRelaySecret, pushRelayToken, "/x/"} {
		if strings.Contains(string(r.body), s) {
			t.Fatalf("the PUT response holds %q: %s", s, r.body)
		}
	}
	var got notify.PushView
	r.json(t, &got)
	if !got.Configured || got.Host == nil || *got.Host != "relay.example.com" {
		t.Fatalf("put view %+v", got)
	}
	g := e.must(200, e.do("GET", pushPath, e.chen, nil))
	for _, s := range []string{pushRelaySecret, pushRelayToken} {
		if strings.Contains(string(g.body), s) {
			t.Fatalf("the GET holds %q: %s", s, g.body)
		}
	}
	// A partial PUT keeps the other value.
	e.must(200, e.do("PUT", pushPath, e.chen, map[string]any{"relaySecret": "ANOTHERSECRET-1"}))
	if v := e.pushView(e.chen); !v.Configured || v.SecretUnreadable {
		t.Fatalf("keep %+v", v)
	}
	if n := e.countEvents("push.updated"); n != 2 {
		t.Fatalf("events after two writes = %d", n)
	}
	e.must(204, e.do("DELETE", pushPath, e.chen, nil))
	e.must(204, e.do("DELETE", pushPath, e.chen, nil))
	if v := e.pushView(e.chen); v.Configured {
		t.Fatalf("still configured %+v", v)
	}
	if n := e.countEvents("push.updated"); n != 3 {
		t.Fatalf("events after remove = %d, want 3 (the second delete emits none)", n)
	}
	var leaked int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE payload::text LIKE '%relay%' OR payload::text LIKE '%'||$1||'%' OR payload::text LIKE '%'||$2||'%'`, pushRelaySecret, pushRelayToken).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("event payload leaked: %d %v", leaked, err)
	}
	evs := e.eventsOfType(e.chen, "push.updated")
	if len(evs) != 3 || evs[0].EntityType != "org" {
		t.Fatalf("events %+v", evs)
	}
}

func TestPush_RelayValidation(t *testing.T) {
	e, _ := newPushEnv(t, true)
	put := func(body map[string]any, h ...string) resp { return e.do("PUT", pushPath, e.chen, body, h...) }
	for _, u := range []string{
		"http://relay.example.com/" + pushRelayToken, "https://u:p@relay.example.com/" + pushRelayToken,
		"https://relay.example.com/?k=" + pushRelayToken, "not a url", "", "https://relay.example.com/#" + pushRelayToken,
	} {
		r := put(map[string]any{"relayUrl": u, "relaySecret": pushRelaySecret})
		expectCode(t, r, 400, "validation_failed")
		if strings.Contains(string(r.body), pushRelaySecret) || (u != "" && strings.Contains(string(r.body), u)) {
			t.Fatalf("the error repeats the value: %s", r.body)
		}
	}
	expectCode(t, put(map[string]any{}), 400, "validation_failed")
	expectCode(t, put(map[string]any{"relayUrl": pushRelayURL}), 400, "validation_failed") // a new relay needs both
	expectCode(t, put(map[string]any{"relayUrl": pushRelayURL, "relaySecret": "has space"}), 400, "validation_failed")
	expectCode(t, put(map[string]any{"relayUrl": pushRelayURL, "relaySecret": pushRelaySecret, "extra": 1}), 400, "bad_request")
	r := put(map[string]any{"relayUrl": pushRelayURL, "relaySecret": pushRelaySecret}, "Idempotency-Key", "k1")
	if r.status != 400 || !strings.Contains(string(r.body), "Idempotency-Key") {
		t.Fatalf("Idempotency-Key on PUT: %d %s", r.status, r.body)
	}
	r = e.do("POST", pushPath+"/test", e.chen, nil, "Idempotency-Key", "k2")
	if r.status != 400 || !strings.Contains(string(r.body), "Idempotency-Key") {
		t.Fatalf("Idempotency-Key on test: %d %s", r.status, r.body)
	}
	if v := e.pushView(e.chen); v.Configured {
		t.Fatal("a refused PUT stored a relay")
	}
	rq, _ := http.NewRequest("PUT", e.srv.URL+pushPath, strings.NewReader("x"))
	e.authorize(rq, e.chen)
	rq.Header.Set("Content-Type", "text/plain")
	res, err := http.DefaultClient.Do(rq)
	if err != nil || res.StatusCode != 415 {
		t.Fatalf("text/plain: %v %v", res, err)
	}
	_ = res.Body.Close()
}

func TestPush_NoKeyCannotStore(t *testing.T) {
	e, _ := newPushEnv(t, false)
	if v := e.pushView(e.chen); v.Available {
		t.Fatalf("available without a key: %+v", v)
	}
	expectCode(t, e.do("PUT", pushPath, e.chen, map[string]any{"relayUrl": pushRelayURL, "relaySecret": pushRelaySecret}), 409, "encryption_unavailable")
	expectCode(t, e.do("PUT", pushDevPath+"/"+uuid.NewString(), e.chen, pushDev(pushDevToken)), 409, "encryption_unavailable")
}

func TestPush_DeviceLifecycle(t *testing.T) {
	e, _ := newPushEnv(t, true)
	id := uuid.NewString()
	// The list is empty at first and says whether a relay exists.
	var l notify.DeviceList
	e.must(200, e.do("GET", pushDevPath, e.chen, nil)).json(t, &l)
	if l.RelayConfigured || l.MaxDevices != notify.PushMaxDevices || len(l.Items) != 0 {
		t.Fatalf("list %+v", l)
	}
	r := e.must(200, e.do("PUT", pushDevPath+"/"+id, e.chen, pushDev(pushDevToken)))
	if strings.Contains(string(r.body), pushDevToken) {
		t.Fatalf("the PUT response holds the token: %s", r.body)
	}
	var d notify.DeviceView
	r.json(t, &d)
	if d.ID.String() != id || d.Platform != "ios" || d.Label != "Test phone" || d.AppVersion != "1.0" {
		t.Fatalf("device %+v", d)
	}
	// A refresh keeps one row.
	e.must(200, e.do("PUT", pushDevPath+"/"+id, e.chen, pushDev(pushDevToken)))
	g := e.must(200, e.do("GET", pushDevPath, e.chen, nil))
	if strings.Contains(string(g.body), pushDevToken) {
		t.Fatalf("the list holds the token: %s", g.body)
	}
	g.json(t, &l)
	if len(l.Items) != 1 {
		t.Fatalf("list %+v", l)
	}
	e.setRelay(e.chen)
	e.must(200, e.do("GET", pushDevPath, e.chen, nil)).json(t, &l)
	if !l.RelayConfigured {
		t.Fatal("relayConfigured false after the admin set a relay")
	}
	// Another member sees none of it, and cannot touch it.
	ann := e.newHuman("ann", "ann@example.com", "member")
	annTok, _ := e.newToken(ann.ID, tokenOpts{})
	e.must(200, e.do("GET", pushDevPath, annTok, nil)).json(t, &l)
	if len(l.Items) != 0 {
		t.Fatalf("ann sees %d devices", len(l.Items))
	}
	expectCode(t, e.do("PUT", pushDevPath+"/"+id, annTok, pushDev("ANN-TOKEN-1")), 404, "not_found")
	expectCode(t, e.do("DELETE", pushDevPath+"/"+id, annTok, nil), 404, "not_found")
	// Validation.
	for _, body := range []map[string]any{
		{"platform": "web", "token": "T"}, {"platform": "ios"}, {"platform": "ios", "token": "has space"},
		{"platform": "ios", "token": strings.Repeat("a", 4097)}, {"platform": "ios", "token": "T", "extra": 1},
	} {
		r := e.do("PUT", pushDevPath+"/"+uuid.NewString(), e.chen, body)
		if r.status != 400 {
			t.Errorf("body %v: status %d %s", body, r.status, r.body)
		}
	}
	expectCode(t, e.do("PUT", pushDevPath+"/not-a-uuid", e.chen, pushDev("T")), 404, "not_found")
	r = e.do("PUT", pushDevPath+"/"+id, e.chen, pushDev(pushDevToken), "Idempotency-Key", "k1")
	if r.status != 400 || !strings.Contains(string(r.body), "Idempotency-Key") {
		t.Fatalf("Idempotency-Key on the device PUT: %d %s", r.status, r.body)
	}
	// No event for any of it (a per-user private row).
	if n := e.countEvents("push.updated"); n != 1 { // the admin's setRelay
		t.Fatalf("push events = %d, want 1", n)
	}
	e.must(204, e.do("DELETE", pushDevPath+"/"+id, e.chen, nil))
	expectCode(t, e.do("DELETE", pushDevPath+"/"+id, e.chen, nil), 404, "not_found")
}

func TestPush_DeviceCapIs409(t *testing.T) {
	e, _ := newPushEnv(t, true)
	for i := 0; i < notify.PushMaxDevices; i++ {
		e.must(200, e.do("PUT", pushDevPath+"/"+uuid.NewString(), e.chen, pushDev("CAP-"+uuid.NewString())))
	}
	expectCode(t, e.do("PUT", pushDevPath+"/"+uuid.NewString(), e.chen, pushDev("ONE-MORE")), 409, "conflict")
}

func TestPush_CrossOrg(t *testing.T) {
	e, _ := newPushEnv(t, true)
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "admin")
	both, _ := e.newToken(chen.ID, tokenOpts{}) // unpinned: a member of both orgs
	const A = "/api/v1/orgs/acme"
	acmeDev := uuid.NewString()
	e.must(200, e.do("PUT", A+"/me/push-devices/"+acmeDev, both, pushDev("ACME-ONLY-TOKEN")))
	e.must(200, e.do("PUT", A+"/push", both, map[string]any{"relayUrl": "https://relay.example.com/acme", "relaySecret": "ACMESECRET-1"}))
	// Orbit sees nothing of acme.
	if v := e.pushView(both); v.Configured {
		t.Fatalf("leak %+v", v)
	}
	var l notify.DeviceList
	e.must(200, e.do("GET", pushDevPath, both, nil)).json(t, &l)
	if len(l.Items) != 0 || l.RelayConfigured {
		t.Fatalf("orbit lists %+v", l)
	}
	// The id is chosen by the client and is unique per org: the same id in orbit is a new
	// orbit row, not a denial and not an overwrite of acme's.
	e.must(200, e.do("PUT", pushDevPath+"/"+acmeDev, both, pushDev("OTHER")))
	if n := e.count(`SELECT count(*) FROM push_devices WHERE id = $1`, acmeDev); n != 2 {
		t.Fatalf("rows with the id = %d, want one per org", n)
	}
	if n := e.count(`SELECT count(*) FROM push_devices WHERE id = $1 AND org_id = $2 AND platform = 'ios'`, acmeDev, acmeID); n != 1 {
		t.Fatalf("acme's device changed (%d)", n)
	}
	e.must(204, e.do("DELETE", pushDevPath+"/"+acmeDev, both, nil))
	if n := e.count(`SELECT count(*) FROM push_devices WHERE id = $1 AND org_id = $2`, acmeDev, acmeID); n != 1 {
		t.Fatalf("orbit's delete removed acme's device (%d)", n)
	}
	// A token pinned to orbit cannot reach acme's routes: 404 for every verb.
	for _, c := range []struct{ m, p string }{
		{"GET", A + "/push"}, {"PUT", A + "/push"}, {"DELETE", A + "/push"}, {"POST", A + "/push/test"},
		{"GET", A + "/me/push-devices"}, {"PUT", A + "/me/push-devices/" + acmeDev}, {"DELETE", A + "/me/push-devices/" + acmeDev},
	} {
		expectCode(t, e.do(c.m, c.p, e.chen, pushDev("X")), 404, "not_found")
	}
	// A delete in orbit leaves acme's relay.
	e.must(204, e.do("DELETE", pushPath, both, nil))
	if n := e.count(`SELECT count(*) FROM push_relays WHERE org_id = $1`, acmeID); n != 1 {
		t.Fatalf("acme relay rows = %d", n)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE org_id = $1 AND type = 'push.updated'`, e.orgID); n != 0 {
		t.Fatalf("orbit has %d push events", n)
	}
}

func TestPush_TestPush(t *testing.T) {
	e, f := newPushEnv(t, true)
	expectCode(t, e.do("POST", pushPath+"/test", e.chen, nil), 404, "not_found") // no relay
	e.setRelay(e.chen)
	// Two calls so far inside the burst of 3. The admin has no device: 409.
	expectCode(t, e.do("POST", pushPath+"/test", e.chen, nil), 409, "conflict")
	e.must(200, e.do("PUT", pushDevPath+"/"+uuid.NewString(), e.chen, pushDev(pushDevToken)))
	var out notify.PushTestResult
	e.must(200, e.do("POST", pushPath+"/test", e.chen, nil)).json(t, &out)
	if out.Sent != 1 || f.count() != 1 {
		t.Fatalf("out %+v hits %d", out, f.count())
	}
	if v := e.pushView(e.chen); v.LastOkAt == nil || v.LastError != nil {
		t.Fatalf("view after ok %+v", v)
	}
	// The burst of 3 is used: the next call is 429 with Retry-After and sends nothing.
	f.set(401)
	r := e.do("POST", pushPath+"/test", e.chen, nil)
	expectCode(t, r, 429, "rate_limited")
	if r.header.Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	if f.count() != 1 {
		t.Errorf("the relay was called %d times, want 1", f.count())
	}
	f.mu.Lock()
	hit := f.hits[0]
	f.mu.Unlock()
	if !strings.HasPrefix(hit, "/x/"+pushRelayToken+"/v1/wake|Bearer "+pushRelaySecret+"|") || !strings.Contains(hit, pushDevToken) {
		t.Errorf("the relay got %q", hit)
	}
	if l := e.logs.String(); strings.Contains(l, pushRelaySecret) || strings.Contains(l, pushRelayToken) || strings.Contains(l, pushDevToken) {
		t.Fatalf("log holds a secret: %s", l)
	}
}

func TestPush_TestPushFailureIs502(t *testing.T) {
	e, f := newPushEnv(t, true)
	e.setRelay(e.chen)
	e.must(200, e.do("PUT", pushDevPath+"/"+uuid.NewString(), e.chen, pushDev(pushDevToken)))
	f.set(403)
	r := e.do("POST", pushPath+"/test", e.chen, nil)
	expectCode(t, r, 502, "push_send_failed")
	for _, s := range []string{pushRelaySecret, pushRelayToken, pushDevToken, "relay.example.com"} {
		if strings.Contains(string(r.body), s) {
			t.Fatalf("502 body holds %q: %s", s, r.body)
		}
	}
	if !strings.Contains(string(r.body), "push_unauthorized") {
		t.Fatalf("502 body: %s", r.body)
	}
	if v := e.pushView(e.chen); v.LastError == nil || *v.LastError != "push_unauthorized" {
		t.Fatalf("view after fail %+v", v)
	}
}

// The notification settings carry the push channel and say whether to show it.
func TestPush_NotificationSettings(t *testing.T) {
	e, _ := newPushEnv(t, true)
	const path = O + "/me/notification-settings"
	var v notify.View
	e.must(200, e.do("GET", path, e.chen, nil)).json(t, &v)
	if v.Push.Available {
		t.Fatal("push available without a relay")
	}
	e.setRelay(e.chen)
	e.must(200, e.do("PUT", path, e.chen, map[string]any{"events": map[string]any{"comment": map[string]any{"push": "on"}}})).json(t, &v)
	if !v.Push.Available {
		t.Fatal("push not available with a relay")
	}
	for _, ev := range v.Events {
		want := "off"
		if ev.Event == "comment" {
			want = "on"
		}
		if ev.Default.Push != "off" || ev.Resolved.Push.Value != want {
			t.Errorf("%s: %+v", ev.Event, ev)
		}
	}
	r := e.do("PUT", path, e.chen, map[string]any{"events": map[string]any{"comment": map[string]any{"sms": "on"}}})
	expectCode(t, r, 400, "validation_failed")
	if !strings.Contains(string(r.body), "email, inbox or push") {
		t.Errorf("message %s", r.body)
	}
	expectCode(t, e.do("PUT", path, e.chen, map[string]any{"events": map[string]any{"comment": map[string]any{"push": "yes"}}}), 400, "validation_failed")
}

// No secret in a response, a log line, an event or an idempotency row.
func TestPush_SecretsNeverLeak(t *testing.T) {
	e, f := newPushEnv(t, true)
	secrets := []string{pushRelaySecret, pushRelayToken, pushDevToken}
	var bodies []string
	add := func(r resp) { bodies = append(bodies, string(r.body)) }
	add(e.do("PUT", pushPath, e.chen, map[string]any{"relayUrl": pushRelayURL, "relaySecret": pushRelaySecret}))
	add(e.do("PUT", pushPath, e.chen, map[string]any{"relayUrl": pushRelayURL + "?bad", "relaySecret": pushRelaySecret}))
	add(e.do("PUT", pushDevPath+"/"+uuid.NewString(), e.chen, pushDev(pushDevToken)))
	add(e.do("PUT", pushDevPath+"/"+uuid.NewString(), e.chen, map[string]any{"platform": "nope", "token": pushDevToken}))
	add(e.do("GET", pushPath, e.chen, nil))
	add(e.do("GET", pushDevPath, e.chen, nil))
	add(e.do("POST", pushPath+"/test", e.chen, nil))
	f.set(500)
	add(e.do("POST", pushPath+"/test", e.chen, nil))
	f.Close()
	add(e.do("POST", pushPath+"/test", e.chen, nil)) // 429 or 502; either way no secret
	add(e.do("DELETE", pushPath, e.chen, nil))
	for _, b := range bodies {
		for _, s := range secrets {
			if strings.Contains(b, s) {
				t.Errorf("a response holds %q: %s", s, b)
			}
		}
	}
	if l := e.logs.String(); true {
		for _, s := range secrets {
			if strings.Contains(l, s) {
				t.Errorf("log holds %q", s)
			}
		}
	}
	for _, q := range []string{
		`SELECT coalesce(string_agg(payload::text, ' '), '') FROM events`,
		`SELECT coalesce(string_agg(t::text, ' '), '') FROM idempotency_keys t`,
		`SELECT coalesce(string_agg(s::text, ' '), '') FROM push_outbox s`,
		`SELECT coalesce(string_agg(coalesce(last_error, ''), ' '), '') FROM push_relays`,
	} {
		var text string
		if err := e.pool.QueryRow(context.Background(), q).Scan(&text); err != nil {
			t.Fatal(err)
		}
		for _, s := range secrets {
			if strings.Contains(text, s) {
				t.Errorf("%q stored in clear (%s)", s, q)
			}
		}
	}
	_ = json.Marshal
}
