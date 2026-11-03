package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"devops/internal/netguard"
	"devops/internal/netguard/netguardtest"
	"devops/internal/notify"
	"devops/internal/platform"
)

// A member's own webhooks (LAUN-229, docs/internal/decisions-T40-personal-webhooks.md).

const (
	pwhSecret = "APIPERSONALHOOKTOKEN777"
	pwhHook   = "https://hooks.slack.com/services/T0002/B0002/" + pwhSecret
	pwhPath   = O + "/me/webhooks"
)

type apiHook struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []string
	status int
}

func (f *apiHook) set(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

func (f *apiHook) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

// newPersonalEnv is an env with the personal webhooks wired to a fake host
// through the real netguard dialer (test policy: 127.0.0.1 only).
func newPersonalEnv(t *testing.T, withKey bool) (*env, *apiHook) {
	t.Helper()
	f := &apiHook{}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, string(b))
		st := f.status
		f.mu.Unlock()
		if st == 0 {
			st = 200
		}
		w.WriteHeader(st)
	}))
	t.Cleanup(f.Close)
	d := &netguard.Dialer{Policy: netguardtest.Loopback()}
	c := notify.NewChatClientFor(d)
	tr := c.Transport.(*http.Transport)
	addr := f.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return d.DialContext(ctx, network, addr)
	}
	pool := x509.NewCertPool()
	pool.AddCert(f.Certificate())
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: "example.com"}
	e := newEnvWith(t, envOpts{configure: func(e *env, s *Server) {
		var keys *platform.SlackKeyring
		if withKey {
			var err error
			if keys, err = platform.NewSlackKeyring([]byte("0123456789abcdef0123456789abcdef")); err != nil {
				t.Fatal(err)
			}
		}
		log := slog.New(slog.NewTextHandler(io.MultiWriter(tWriter{t}, e.logs), &slog.HandlerOptions{Level: slog.LevelDebug}))
		s.Personal = notify.NewPersonalWebhooks(e.pool, log, keys, notify.NewChatPoster(c), "https://launchpad.example.com")
	}})
	return e, f
}

func (e *env) pwhList(token string) notify.PersonalList {
	e.t.Helper()
	var v notify.PersonalList
	e.must(200, e.do("GET", pwhPath, token, nil)).json(e.t, &v)
	return v
}

func (e *env) pwhCreate(token string, body map[string]any) notify.PersonalView {
	e.t.Helper()
	var v notify.PersonalView
	e.must(201, e.do("POST", pwhPath, token, body)).json(e.t, &v)
	return v
}

func (e *env) countAllEvents() int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM events`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// Every route refuses an anonymous call, a token without the scope, and an
// agent token (refuseAgents), even an admin agent with every scope.
func TestPersonalWebhooks_Auth(t *testing.T) {
	e, f := newPersonalEnv(t, true)
	own := e.pwhCreate(e.chen, map[string]any{"name": "Mine", "kind": "slack", "webhookUrl": pwhHook})
	readOnly, _ := e.newToken(e.user("chen").ID, tokenOpts{scopes: []string{"read"}})
	exp := time.Now().Add(time.Hour)
	if _, err := e.pool.Exec(context.Background(), `UPDATE org_members SET role = 'admin' WHERE user_id = $1`, e.user("claude").ID); err != nil {
		t.Fatal(err)
	}
	agentAdmin, _ := e.newToken(e.user("claude").ID, tokenOpts{expires: &exp})
	body := map[string]any{"name": "x", "kind": "slack", "webhookUrl": pwhHook}
	one := pwhPath + "/" + own.ID.String()
	routes := []struct{ method, path string }{
		{"GET", pwhPath}, {"POST", pwhPath}, {"PATCH", one}, {"DELETE", one}, {"POST", one + "/test"},
	}
	before := e.countAllEvents()
	for _, c := range routes {
		expectCode(t, e.do(c.method, c.path, "", body), 401, "unauthenticated")
		expectCode(t, e.do(c.method, c.path, e.claude, body), 403, "forbidden")
		expectCode(t, e.do(c.method, c.path, agentAdmin, body), 403, "forbidden")
		if c.method != "GET" {
			expectCode(t, e.do(c.method, c.path, readOnly, body), 403, "insufficient_scope")
		}
	}
	var rows int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM personal_webhooks`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("a refused request changed the webhooks: %d rows", rows)
	}
	if n := e.countAllEvents(); n != before {
		t.Fatalf("a refused request emitted %d events", n-before)
	}
	if f.count() != 0 {
		t.Fatalf("a refused test posted")
	}
}

// Only the owner sees, changes, tests or removes a webhook. An admin gets
// nothing of a member's webhooks; the URL is never in a response; no event.
func TestPersonalWebhooks_OwnRowsOnly_WriteOnly(t *testing.T) {
	e, f := newPersonalEnv(t, true)
	ann := e.newHuman("ann", "ann@example.com", "member")
	annTok, _ := e.newToken(ann.ID, tokenOpts{})
	before := e.countAllEvents()
	v := e.pwhCreate(annTok, map[string]any{"name": "Ann's", "kind": "slack", "webhookUrl": pwhHook})
	if v.Host != "hooks.slack.com" || strings.Join(v.Events, ",") != "assigned,my_issues" || !v.Enabled {
		t.Fatalf("created %+v", v)
	}
	one := pwhPath + "/" + v.ID.String()
	// The admin (chen) sees an empty list and gets 404 on the member's row.
	if l := e.pwhList(e.chen); len(l.Items) != 0 || l.Limit != 3 || !l.Available || !l.LinksAvailable {
		t.Fatalf("the admin sees %+v", l)
	}
	expectCode(t, e.do("PATCH", one, e.chen, map[string]any{"name": "hijack"}), 404, "not_found")
	expectCode(t, e.do("POST", one+"/test", e.chen, nil), 404, "not_found")
	e.must(204, e.do("DELETE", one, e.chen, nil)) // idempotent by shape: removes nothing
	if l := e.pwhList(annTok); len(l.Items) != 1 || l.Items[0].Name != "Ann's" {
		t.Fatalf("the member's webhook changed: %+v", l)
	}
	if f.count() != 0 {
		t.Fatal("another member's test posted")
	}
	// The owner changes it: name, choices, switch, a new URL.
	var pv notify.PersonalView
	e.must(200, e.do("PATCH", one, annTok, map[string]any{"name": "Renamed", "events": []string{"assigned"}, "enabled": false, "webhookUrl": pwhHook + "X"})).json(t, &pv)
	if pv.Name != "Renamed" || strings.Join(pv.Events, ",") != "assigned" || pv.Enabled {
		t.Fatalf("patched %+v", pv)
	}
	var tr notify.ChatTestResult
	e.must(200, e.do("POST", one+"/test", annTok, nil)).json(t, &tr)
	if !tr.OK || f.count() != 1 {
		t.Fatalf("test %+v, posts %d", tr, f.count())
	}
	if !strings.Contains(f.bodies[0], "Launchpad test message") {
		t.Fatalf("test body %s", f.bodies[0])
	}
	f.set(410)
	expectCode(t, e.do("POST", one+"/test", annTok, nil), 502, "chat_send_failed")
	if l := e.pwhList(annTok); l.Items[0].LastError == nil || *l.Items[0].LastError != "chat_gone" {
		t.Fatalf("status %+v", l.Items[0])
	}
	e.must(204, e.do("DELETE", one, annTok, nil))
	e.must(204, e.do("DELETE", one, annTok, nil))
	if l := e.pwhList(annTok); len(l.Items) != 0 {
		t.Fatalf("after delete %+v", l)
	}
	if n := e.countAllEvents(); n != before {
		t.Fatalf("personal webhook writes emitted %d events, want none", n-before)
	}
}

func TestPersonalWebhooks_Validation(t *testing.T) {
	e, _ := newPersonalEnv(t, true)
	bad := []map[string]any{
		{"name": "", "kind": "slack", "webhookUrl": pwhHook},
		{"name": strings.Repeat("x", 41), "kind": "slack", "webhookUrl": pwhHook},
		{"name": "a", "kind": "teams", "webhookUrl": pwhHook},
		{"name": "a", "kind": "slack", "webhookUrl": "https://evil.example.com/services/T/B/x"},
		{"name": "a", "kind": "slack", "webhookUrl": pwhHook, "events": []string{"mention"}},
		// The SSRF refusals are the chat ones (one function): http, localhost,
		// private and old numeric literals, local names.
		{"name": "a", "kind": "custom", "webhookUrl": "http://hooks.example.com/x"},
		{"name": "a", "kind": "custom", "webhookUrl": "https://localhost/x"},
		{"name": "a", "kind": "custom", "webhookUrl": "https://10.0.0.1/x"},
		{"name": "a", "kind": "custom", "webhookUrl": "https://127.1/x"},
		{"name": "a", "kind": "custom", "webhookUrl": "https://printer.lan/x"},
		{"name": "a", "kind": "custom", "webhookUrl": "https://user:pw@hooks.example.com/x"},
	}
	for _, b := range bad {
		r := e.do("POST", pwhPath, e.chen, b)
		expectCode(t, r, 400, "validation_failed")
		if u, _ := b["webhookUrl"].(string); strings.Contains(string(r.body), u) {
			t.Errorf("the error echoes the URL: %s", r.body)
		}
	}
	// The secret-bearing routes refuse an Idempotency-Key.
	expectCode(t, e.do("POST", pwhPath, e.chen, map[string]any{"name": "a", "kind": "slack", "webhookUrl": pwhHook}, "Idempotency-Key", "k1"), 400, "bad_request")
	v := e.pwhCreate(e.chen, map[string]any{"name": "Custom", "kind": "custom", "webhookUrl": "https://hooks.example.com/in?token=" + pwhSecret, "events": []string{"my_issues"}})
	if v.Host != "hooks.example.com" || strings.Join(v.Events, ",") != "my_issues" {
		t.Fatalf("custom %+v", v)
	}
	expectCode(t, e.do("PATCH", pwhPath+"/"+v.ID.String(), e.chen, map[string]any{"name": "x"}, "Idempotency-Key", "k2"), 400, "bad_request")
	expectCode(t, e.do("POST", pwhPath+"/"+v.ID.String()+"/test", e.chen, nil, "Idempotency-Key", "k3"), 400, "bad_request")
	// The kind never changes: a Slack URL on a custom webhook is checked as custom (allowed),
	// a custom URL on a Slack webhook is refused.
	s := e.pwhCreate(e.chen, map[string]any{"name": "Slack", "kind": "slack", "webhookUrl": pwhHook})
	expectCode(t, e.do("PATCH", pwhPath+"/"+s.ID.String(), e.chen, map[string]any{"webhookUrl": "https://hooks.example.com/x"}), 400, "validation_failed")
	// A taken name (ignoring case) is 409; the cap of 3 is 409 integration_limit.
	expectCode(t, e.do("POST", pwhPath, e.chen, map[string]any{"name": "slack", "kind": "slack", "webhookUrl": pwhHook}), 409, "conflict")
	e.pwhCreate(e.chen, map[string]any{"name": "Third", "kind": "slack", "webhookUrl": pwhHook})
	expectCode(t, e.do("POST", pwhPath, e.chen, map[string]any{"name": "Fourth", "kind": "slack", "webhookUrl": pwhHook}), 409, "integration_limit")
	// The same name for another member is fine (the cap and names are per member).
	ann := e.newHuman("ann", "ann@example.com", "member")
	annTok, _ := e.newToken(ann.ID, tokenOpts{})
	e.pwhCreate(annTok, map[string]any{"name": "Slack", "kind": "slack", "webhookUrl": pwhHook})
}

func TestPersonalWebhooks_NoKeyCannotStore(t *testing.T) {
	e, _ := newPersonalEnv(t, false)
	if l := e.pwhList(e.chen); l.Available {
		t.Fatalf("available without a key: %+v", l)
	}
	expectCode(t, e.do("POST", pwhPath, e.chen, map[string]any{"name": "a", "kind": "slack", "webhookUrl": pwhHook}), 409, "encryption_unavailable")
}

// Another org's webhook is 404 under this org's path, and the reverse.
func TestPersonalWebhooks_CrossOrg(t *testing.T) {
	e, f := newPersonalEnv(t, true)
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "member")
	both, _ := e.newToken(chen.ID, tokenOpts{})
	const A = "/api/v1/orgs/acme/me/webhooks"
	var av notify.PersonalView
	e.must(201, e.do("POST", A, both, map[string]any{"name": "Acme", "kind": "slack", "webhookUrl": pwhHook})).json(t, &av)
	if l := e.pwhList(both); len(l.Items) != 0 {
		t.Fatalf("orbit lists acme's webhook: %+v", l)
	}
	ov := e.pwhCreate(both, map[string]any{"name": "Orbit", "kind": "slack", "webhookUrl": pwhHook})
	for _, c := range []struct{ m, p string }{
		{"PATCH", pwhPath + "/" + av.ID.String()}, {"POST", pwhPath + "/" + av.ID.String() + "/test"},
		{"PATCH", A + "/" + ov.ID.String()}, {"POST", A + "/" + ov.ID.String() + "/test"},
	} {
		expectCode(t, e.do(c.m, c.p, both, map[string]any{"name": "hijack"}), 404, "not_found")
	}
	e.must(204, e.do("DELETE", pwhPath+"/"+av.ID.String(), both, nil))
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM personal_webhooks WHERE org_id = $1 AND name = 'Acme'`, acmeID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("acme row = %d (%v)", n, err)
	}
	if f.count() != 0 {
		t.Fatal("a cross-org test posted")
	}
	// A token pinned to orbit cannot reach acme's routes.
	expectCode(t, e.do("GET", A, e.chen, nil), 404, "not_found")
}

// The webhook URL is in no log line, event, idempotency row or response.
func TestPersonalWebhooks_SecretNeverLeaks(t *testing.T) {
	e, f := newPersonalEnv(t, true)
	var bodies []string
	add := func(r resp) { bodies = append(bodies, string(r.body)) }
	r := e.do("POST", pwhPath, e.chen, map[string]any{"name": "Mine", "kind": "slack", "webhookUrl": pwhHook})
	add(r)
	var v notify.PersonalView
	r.json(t, &v)
	one := pwhPath + "/" + v.ID.String()
	add(e.do("POST", pwhPath, e.chen, map[string]any{"name": "Bad", "kind": "slack", "webhookUrl": pwhHook + "?bad"}))
	add(e.do("PATCH", one, e.chen, map[string]any{"webhookUrl": pwhHook + "?bad"}))
	add(e.do("PATCH", one, e.chen, map[string]any{"webhookUrl": pwhHook}))
	add(e.do("GET", pwhPath, e.chen, nil))
	add(e.do("POST", one+"/test", e.chen, nil))
	f.set(500)
	add(e.do("POST", one+"/test", e.chen, nil))
	f.Close()
	add(e.do("POST", one+"/test", e.chen, nil))
	add(e.do("DELETE", one, e.chen, nil))
	for _, b := range bodies {
		if strings.Contains(b, pwhSecret) {
			t.Errorf("a response holds the secret: %s", b)
		}
	}
	if l := e.logs.String(); strings.Contains(l, pwhSecret) || strings.Contains(l, "/services/T0002") {
		t.Errorf("log holds the webhook: %s", l)
	}
	for _, q := range []string{
		`SELECT coalesce(string_agg(payload::text, ' '), '') FROM events`,
		`SELECT coalesce(string_agg(t::text, ' '), '') FROM idempotency_keys t`,
	} {
		var text string
		if err := e.pool.QueryRow(context.Background(), q).Scan(&text); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(text, pwhSecret) {
			t.Errorf("stored in clear (%s)", q)
		}
	}
}

// The test limiter is per member: one member who uses it up does not stop
// another, and a probe of an id the member does not own uses no token.
func TestPersonalWebhooks_TestLimitPerMember(t *testing.T) {
	e, f := newPersonalEnv(t, true)
	ann := e.newHuman("ann", "ann@example.com", "member")
	annTok, _ := e.newToken(ann.ID, tokenOpts{})
	bob := e.newHuman("bob", "bob@example.com", "member")
	bobTok, _ := e.newToken(bob.ID, tokenOpts{})
	a := e.pwhCreate(annTok, map[string]any{"name": "Ann", "kind": "slack", "webhookUrl": pwhHook})
	b := e.pwhCreate(bobTok, map[string]any{"name": "Bob", "kind": "slack", "webhookUrl": pwhHook})
	// bob probes ann's id and made-up ids: 404, and no token is used.
	for i := 0; i < notify.PersonalTestPerMinute+2; i++ {
		expectCode(t, e.do("POST", pwhPath+"/"+a.ID.String()+"/test", bobTok, nil), 404, "not_found")
		expectCode(t, e.do("POST", pwhPath+"/"+someID+"/test", bobTok, nil), 404, "not_found")
	}
	for i := 0; i < notify.PersonalTestPerMinute; i++ {
		e.must(200, e.do("POST", pwhPath+"/"+a.ID.String()+"/test", annTok, nil))
	}
	r := e.do("POST", pwhPath+"/"+a.ID.String()+"/test", annTok, nil)
	expectCode(t, r, 429, "rate_limited")
	if r.header.Get("Retry-After") == "" || !strings.Contains(string(r.body), "test messages") || strings.Contains(string(r.body), "login") {
		t.Fatalf("429 answer: %v %s", r.header, r.body)
	}
	// bob's own test still works.
	e.must(200, e.do("POST", pwhPath+"/"+b.ID.String()+"/test", bobTok, nil))
	if f.count() != notify.PersonalTestPerMinute+1 {
		t.Fatalf("posts = %d", f.count())
	}
}

// A host longer than a DNS name is 400 validation_failed, not a 500, for a
// personal webhook and for a chat service (the shared parser).
func TestPersonalWebhooks_LongHostIs400(t *testing.T) {
	long := "https://" + strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + "." + strings.Repeat("d", 60) + ".example.com/x"
	e, _ := newPersonalEnv(t, true)
	expectCode(t, e.do("POST", pwhPath, e.chen, map[string]any{"name": "Long", "kind": "custom", "webhookUrl": long}), 400, "validation_failed")
	v := e.pwhCreate(e.chen, map[string]any{"name": "Short", "kind": "custom", "webhookUrl": "https://hooks.example.com/x"})
	expectCode(t, e.do("PATCH", pwhPath+"/"+v.ID.String(), e.chen, map[string]any{"webhookUrl": long}), 400, "validation_failed")
	ce, _ := newChatEnv(t, true)
	expectCode(t, ce.do("POST", chatPath, ce.chen, map[string]any{"name": "Long", "teamId": ce.chatTeam(), "kind": "custom", "webhookUrl": long}), 400, "validation_failed")
}
