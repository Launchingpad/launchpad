package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"

	"github.com/google/uuid"
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

// The chat service routes (LAUN-204, docs/internal/decisions-T36-chat-services.md).

const (
	slackSecret = "APISECRETWEBHOOKTOKEN555"
	slackHook   = "https://hooks.slack.com/services/T0001/B0001/" + slackSecret
	chatPath    = O + "/chat-services"
)

type apiChat struct {
	*httptest.Server
	mu     sync.Mutex
	hits   []string
	bodies []string
	status int
}

func (f *apiChat) set(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

func (f *apiChat) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.hits)
}

// newChatEnv is an env with the Slack service wired to a fake Slack through
// the real netguard dialer (test policy: 127.0.0.1 only).
func newChatEnv(t *testing.T, withKey bool) (*env, *apiChat) {
	t.Helper()
	f := &apiChat{}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.hits = append(f.hits, r.URL.Path)
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
		s.Chat = notify.NewChatAdmin(e.pool, log, keys, notify.NewChatPoster(c), "https://launchpad.example.com")
	}})
	return e, f
}

func (e *env) chatList(token string) notify.ChatList {
	e.t.Helper()
	var v notify.ChatList
	e.must(200, e.do("GET", chatPath, token, nil)).json(e.t, &v)
	return v
}

func (e *env) createChat(token string, body map[string]any) notify.ChatView {
	e.t.Helper()
	var v notify.ChatView
	e.must(201, e.do("POST", chatPath, token, body)).json(e.t, &v)
	return v
}

func (e *env) countEvents(typ string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE type = $1`, typ).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

const someID = "0198f0e0-0000-7000-8000-000000000000"

// Every route refuses an anonymous call, a token without admin scope, a member
// (the role), and an agent token even with the admin scope.
func TestChat_Auth(t *testing.T) {
	e, _ := newChatEnv(t, true)
	own := e.createChat(e.chen, map[string]any{"name": "Own", "teamId": e.chatTeam(), "kind": "slack", "webhookUrl": slackHook})
	ann := e.newHuman("ann", "ann@example.com", "member")
	annAdminScope, _ := e.newToken(ann.ID, tokenOpts{})                                        // admin scope, member role
	readOnly, _ := e.newToken(e.user("chen").ID, tokenOpts{scopes: []string{"read", "write"}}) // org admin, no admin scope
	exp := time.Now().Add(time.Hour)
	// An agent that is an org admin and holds every scope: only refuseAgents stops it.
	if _, err := e.pool.Exec(context.Background(), `UPDATE org_members SET role = 'admin' WHERE user_id = $1`, e.user("claude").ID); err != nil {
		t.Fatal(err)
	}
	agentAdmin, _ := e.newToken(e.user("claude").ID, tokenOpts{expires: &exp})
	body := map[string]any{"name": "x", "teamId": e.chatTeam(), "kind": "slack", "webhookUrl": slackHook, "events": []string{"assigned"}}
	one := chatPath + "/" + own.ID.String()
	routes := []struct{ method, path string }{
		{"GET", chatPath}, {"POST", chatPath}, {"GET", one}, {"PATCH", one}, {"DELETE", one}, {"POST", one + "/test"},
	}
	before := e.countEvents("chat_service.updated")
	for _, c := range routes {
		expectCode(t, e.do(c.method, c.path, "", body), 401, "unauthenticated")
		expectCode(t, e.do(c.method, c.path, e.claude, body), 403, "insufficient_scope")
		expectCode(t, e.do(c.method, c.path, readOnly, body), 403, "insufficient_scope")
		expectCode(t, e.do(c.method, c.path, annAdminScope, body), 403, "forbidden")
		// An agent token, even with the admin scope, gets nothing (AGENTS.md: refuseAgents).
		expectCode(t, e.do(c.method, c.path, agentAdmin, body), 403, "forbidden")
	}
	if n := e.countEvents("chat_service.updated"); n != before {
		t.Fatalf("a refused request emitted %d events", n-before)
	}
	var rows int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM chat_services`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("a refused request changed the services: %d rows", rows)
	}
}

func TestChat_CreateGetPatchDelete_WriteOnly(t *testing.T) {
	e, _ := newChatEnv(t, true)
	l := e.chatList(e.chen)
	if !l.Available || !l.LinksAvailable || l.Limit != notify.ChatMaxServices || len(l.Items) != 0 {
		t.Fatalf("fresh list %+v", l)
	}
	r := e.must(201, e.do("POST", chatPath, e.chen, map[string]any{"name": "Eng", "teamId": e.chatTeam(), "kind": "slack", "webhookUrl": slackHook, "events": []string{"comment", "assigned"}}))
	if strings.Contains(string(r.body), slackSecret) || strings.Contains(string(r.body), "/services/") {
		t.Fatalf("the POST response holds the URL: %s", r.body)
	}
	var got notify.ChatView
	r.json(t, &got)
	if got.Name != "Eng" || got.Kind != "slack" || got.Host != "hooks.slack.com" || !got.Enabled || strings.Join(got.Events, ",") != "assigned,comment" {
		t.Fatalf("created %+v", got)
	}
	one := chatPath + "/" + got.ID.String()
	for _, u := range []string{chatPath, one} {
		g := e.must(200, e.do("GET", u, e.chen, nil))
		if strings.Contains(string(g.body), slackSecret) || strings.Contains(string(g.body), "/services/") {
			t.Fatalf("GET %s holds the URL: %s", u, g.body)
		}
	}
	// A PATCH without a URL keeps it; the response never holds it.
	var p notify.ChatView
	e.must(200, e.do("PATCH", one, e.chen, map[string]any{"events": []string{"new_issue"}, "enabled": false, "name": "Eng 2"})).json(t, &p)
	if p.Enabled || p.Name != "Eng 2" || p.SecretUnreadable || strings.Join(p.Events, ",") != "new_issue" {
		t.Fatalf("patched %+v", p)
	}
	e.must(200, e.do("PATCH", one, e.chen, map[string]any{"webhookUrl": slackHook}))
	if n := e.countEvents("chat_service.updated"); n != 3 {
		t.Fatalf("events after three writes = %d", n)
	}
	e.must(204, e.do("DELETE", one, e.chen, nil))
	e.must(204, e.do("DELETE", one, e.chen, nil))
	expectCode(t, e.do("GET", one, e.chen, nil), 404, "not_found")
	if n := e.countEvents("chat_service.updated"); n != 4 {
		t.Fatalf("events after remove = %d, want 4 (the second delete emits nothing)", n)
	}
	// The event never holds the URL or the name.
	var leaked int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE payload::text LIKE '%services%' OR payload::text LIKE '%'||$1||'%' OR payload::text LIKE '%Eng%'`, slackSecret).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("event payload leaked: %d %v", leaked, err)
	}
}

func TestChat_Validation(t *testing.T) {
	e, _ := newChatEnv(t, true)
	post := func(body map[string]any, h ...string) resp { return e.do("POST", chatPath, e.chen, body, h...) }
	ev := []string{"assigned"}
	for _, u := range []string{
		"http://hooks.slack.com/services/T1/B1/xyz", "https://evil.example.com/services/T1/B1/" + slackSecret,
		"https://hooks.slack.com@evil.example.com/services/T1/B1/xyz", "https://hooks.slack.com:444/services/T1/B1/xyz",
		"https://127.0.0.1/services/T1/B1/xyz", "https://hooks.slack.com/services/T1/B1/xyz?x=1", "not a url", "",
	} {
		r := post(map[string]any{"name": "x", "teamId": e.chatTeam(), "kind": "slack", "webhookUrl": u, "events": ev})
		expectCode(t, r, 400, "validation_failed")
		if strings.Contains(string(r.body), slackSecret) || (u != "" && strings.Contains(string(r.body), u)) {
			t.Fatalf("the error repeats the value: %s", r.body)
		}
	}
	for _, u := range []string{"https://localhost/h", "https://10.1.2.3/h", "https://2130706433/h", "https://127.1/h", "http://chat.example.com/h", "https://intranet/h", "https://169.254.169.254/h"} {
		expectCode(t, post(map[string]any{"name": "x", "teamId": e.chatTeam(), "kind": "custom", "webhookUrl": u}), 400, "validation_failed")
	}
	expectCode(t, post(map[string]any{"name": "x", "teamId": e.chatTeam(), "kind": "discord", "webhookUrl": slackHook}), 400, "validation_failed")
	expectCode(t, post(map[string]any{"name": "", "teamId": e.chatTeam(), "kind": "slack", "webhookUrl": slackHook}), 400, "validation_failed")
	expectCode(t, post(map[string]any{"name": "x", "teamId": e.chatTeam(), "kind": "teams", "webhookUrl": slackHook}), 400, "validation_failed")
	expectCode(t, post(map[string]any{"name": "x", "teamId": e.chatTeam(), "kind": "slack", "webhookUrl": slackHook, "events": []string{"bogus"}}), 400, "validation_failed")
	expectCode(t, post(map[string]any{"name": "x", "teamId": e.chatTeam(), "kind": "slack", "webhookUrl": slackHook, "extra": 1}), 400, "bad_request")
	// A secret never goes through the Idempotency-Key store.
	r := post(map[string]any{"name": "x", "teamId": e.chatTeam(), "kind": "slack", "webhookUrl": slackHook}, "Idempotency-Key", "k1")
	if r.status != 400 || !strings.Contains(string(r.body), "Idempotency-Key") {
		t.Fatalf("Idempotency-Key on POST: %d %s", r.status, r.body)
	}
	v := e.createChat(e.chen, map[string]any{"name": "Eng", "teamId": e.chatTeam(), "kind": "slack", "webhookUrl": slackHook})
	one := chatPath + "/" + v.ID.String()
	r = e.do("PATCH", one, e.chen, map[string]any{"name": "y"}, "Idempotency-Key", "k3")
	if r.status != 400 || !strings.Contains(string(r.body), "Idempotency-Key") {
		t.Fatalf("Idempotency-Key on PATCH: %d %s", r.status, r.body)
	}
	r = e.do("POST", one+"/test", e.chen, nil, "Idempotency-Key", "k2")
	if r.status != 400 || !strings.Contains(string(r.body), "Idempotency-Key") {
		t.Fatalf("Idempotency-Key on test: %d %s", r.status, r.body)
	}
	if n := len(e.chatList(e.chen).Items); n != 1 {
		t.Fatalf("a refused POST stored a service: %d", n)
	}
	// A duplicate name (ignoring case) is 409; a URL of another kind is 400 on PATCH.
	expectCode(t, post(map[string]any{"name": "ENG", "teamId": e.chatTeam(), "kind": "slack", "webhookUrl": slackHook}), 409, "conflict")
	expectCode(t, e.do("PATCH", one, e.chen, map[string]any{"webhookUrl": "https://discord.com/api/webhooks/1/abc"}), 400, "validation_failed")
	expectCode(t, e.do("PATCH", one, e.chen, map[string]any{"teamId": e.chatTeam(), "kind": "discord"}), 400, "bad_request") // the kind never changes
	expectCode(t, e.do("PATCH", chatPath+"/not-a-uuid", e.chen, map[string]any{"name": "z"}), 404, "not_found")
	expectCode(t, e.do("PATCH", chatPath+"/"+someID, e.chen, map[string]any{"name": "z"}), 404, "not_found")
	// A body that is not JSON is 415.
	rq, _ := http.NewRequest("POST", e.srv.URL+chatPath, strings.NewReader("x"))
	e.authorize(rq, e.chen)
	rq.Header.Set("Content-Type", "text/plain")
	res, err := http.DefaultClient.Do(rq)
	if err != nil || res.StatusCode != 415 {
		t.Fatalf("text/plain: %v %v", res, err)
	}
	_ = res.Body.Close()
}

func TestChat_CapIs409(t *testing.T) {
	e, _ := newChatEnv(t, true)
	for i := 0; i < notify.ChatMaxServices; i++ {
		e.createChat(e.chen, map[string]any{"name": fmt.Sprintf("s%d", i), "teamId": e.chatTeam(), "kind": "custom", "webhookUrl": fmt.Sprintf("https://chat.example.com/h/%d", i)})
	}
	expectCode(t, e.do("POST", chatPath, e.chen, map[string]any{"name": "one more", "teamId": e.chatTeam(), "kind": "slack", "webhookUrl": slackHook}), 409, "integration_limit")
}

func TestChat_NoKeyCannotStore(t *testing.T) {
	e, _ := newChatEnv(t, false)
	if l := e.chatList(e.chen); l.Available {
		t.Fatalf("available without a key: %+v", l)
	}
	expectCode(t, e.do("POST", chatPath, e.chen, map[string]any{"name": "x", "teamId": e.chatTeam(), "kind": "slack", "webhookUrl": slackHook}), 409, "encryption_unavailable")
}

func TestChat_TestMessage(t *testing.T) {
	e, f := newChatEnv(t, true)
	expectCode(t, e.do("POST", chatPath+"/"+someID+"/test", e.chen, nil), 404, "not_found") // none set
	v := e.createChat(e.chen, map[string]any{"name": "Eng", "teamId": e.chatTeam(), "kind": "slack", "webhookUrl": slackHook})
	one := chatPath + "/" + v.ID.String()
	var out notify.ChatTestResult
	e.must(200, e.do("POST", one+"/test", e.chen, nil)).json(t, &out)
	if !out.OK || f.count() != 1 {
		t.Fatalf("ok=%v hits=%d", out.OK, f.count())
	}
	var got notify.ChatView
	e.must(200, e.do("GET", one, e.chen, nil)).json(t, &got)
	if got.LastOkAt == nil || got.LastError != nil {
		t.Fatalf("view after ok %+v", got)
	}
	// A failure is a 502 with a fixed word, never the URL or the reply.
	f.set(404)
	r := e.do("POST", one+"/test", e.chen, nil)
	expectCode(t, r, 502, "chat_send_failed")
	if !strings.Contains(string(r.body), "chat_gone") || strings.Contains(string(r.body), slackSecret) || strings.Contains(string(r.body), "hooks.slack.com") {
		t.Fatalf("502 body: %s", r.body)
	}
	e.must(200, e.do("GET", one, e.chen, nil)).json(t, &got)
	if got.LastError == nil || *got.LastError != "chat_gone" {
		t.Fatalf("view after fail %+v", got)
	}
	// 6 a minute, burst 3: the 404 above, the ok and the failure used the three
	// tokens, so the next is 429 with Retry-After and posts nothing.
	f.set(200)
	r = e.do("POST", one+"/test", e.chen, nil)
	expectCode(t, r, 429, "rate_limited")
	if r.header.Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}
	if n := f.count(); n != 2 {
		t.Fatalf("the service was called %d times, want 2", n)
	}
	if l := e.logs.String(); strings.Contains(l, slackSecret) || strings.Contains(l, "services/T0001") {
		t.Fatalf("log holds the webhook: %s", l)
	}
}

// Discord and custom services get their own body shapes, through the real
// route and the real guarded client; the secret is in no response.
func TestChat_TestMessageKinds(t *testing.T) {
	e, f := newChatEnv(t, true)
	d := e.createChat(e.chen, map[string]any{"name": "Disc", "teamId": e.chatTeam(), "kind": "discord", "webhookUrl": "https://discord.com/api/webhooks/123/" + slackSecret})
	c := e.createChat(e.chen, map[string]any{"name": "Fluxer", "teamId": e.chatTeam(), "kind": "custom", "webhookUrl": "https://chat.example.com/hooks/" + slackSecret})
	if d.Host != "discord.com" || c.Host != "chat.example.com" {
		t.Fatalf("hosts %q %q", d.Host, c.Host)
	}
	e.must(200, e.do("POST", chatPath+"/"+d.ID.String()+"/test", e.chen, nil))
	e.must(200, e.do("POST", chatPath+"/"+c.ID.String()+"/test", e.chen, nil))
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) != 2 || !strings.Contains(f.bodies[0], `"content":"Launchpad test message.`) || !strings.Contains(f.bodies[0], `"allowed_mentions"`) {
		t.Fatalf("discord body: %v", f.bodies)
	}
	if !strings.Contains(f.bodies[1], `"text":"Launchpad test message.`) || !strings.Contains(f.bodies[1], `"kind":"test"`) {
		t.Fatalf("custom body: %v", f.bodies)
	}
	if f.hits[0] != "/api/webhooks/123/"+slackSecret || f.hits[1] != "/hooks/"+slackSecret {
		t.Fatalf("paths %v", f.hits)
	}
}

func TestChat_CrossOrg(t *testing.T) {
	e, f := newChatEnv(t, true)
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "admin")
	both, _ := e.newToken(chen.ID, tokenOpts{}) // unpinned: a member of both orgs
	const A = "/api/v1/orgs/acme/chat-services"
	acmeHook := "https://hooks.slack.com/services/T9999/B9999/ACMEONLYTOKEN"
	var av notify.ChatView
	e.must(201, e.do("POST", A, both, map[string]any{"name": "Acme", "teamId": e.teamOfOrg(acmeID), "kind": "slack", "webhookUrl": acmeHook, "events": []string{"comment"}})).json(t, &av)
	// Orbit sees nothing of acme.
	if l := e.chatList(both); len(l.Items) != 0 {
		t.Fatalf("leak %+v", l)
	}
	orbitSvc := e.createChat(both, map[string]any{"name": "Orbit", "teamId": e.chatTeam(), "kind": "slack", "webhookUrl": slackHook})
	// The id of acme's service is 404 under orbit's path (every verb), and
	// orbit's under acme's.
	for _, c := range []struct{ m, p string }{
		{"GET", chatPath + "/" + av.ID.String()}, {"PATCH", chatPath + "/" + av.ID.String()},
		{"DELETE", chatPath + "/" + av.ID.String()}, {"POST", chatPath + "/" + av.ID.String() + "/test"},
	} {
		r := e.do(c.m, c.p, both, map[string]any{"name": "hijack"})
		if c.m == "DELETE" {
			if r.status != 204 { // idempotent by shape: nothing deleted
				t.Fatalf("cross-org DELETE = %d %s", r.status, r.body)
			}
		} else {
			expectCode(t, r, 404, "not_found")
		}
	}
	for _, c := range []struct{ m, p string }{
		{"GET", A + "/" + orbitSvc.ID.String()}, {"PATCH", A + "/" + orbitSvc.ID.String()}, {"POST", A + "/" + orbitSvc.ID.String() + "/test"},
	} {
		expectCode(t, e.do(c.m, c.p, both, map[string]any{"name": "hijack"}), 404, "not_found")
	}
	if f.count() != 0 {
		t.Fatalf("a cross-org test message was posted (%d)", f.count())
	}
	// A token pinned to orbit cannot reach acme's routes: 404 for every verb.
	for _, c := range []struct{ m, p string }{{"GET", A}, {"POST", A}, {"GET", A + "/" + av.ID.String()}, {"DELETE", A + "/" + av.ID.String()}} {
		expectCode(t, e.do(c.m, c.p, e.chen, map[string]any{"name": "x", "teamId": e.chatTeam(), "kind": "slack", "webhookUrl": slackHook}), 404, "not_found")
	}
	// A team of another org in a body is 400, on POST and PATCH, and writes nothing (LAUN-209).
	acmeTeam := e.teamOfOrg(acmeID)
	expectCode(t, e.do("POST", chatPath, both, map[string]any{"name": "X", "teamId": acmeTeam, "kind": "slack", "webhookUrl": slackHook}), 400, "validation_failed")
	expectCode(t, e.do("PATCH", chatPath+"/"+orbitSvc.ID.String(), both, map[string]any{"teamId": acmeTeam}), 400, "validation_failed")
	expectCode(t, e.do("POST", A, both, map[string]any{"name": "Y", "teamId": e.chatTeam(), "kind": "slack", "webhookUrl": acmeHook}), 400, "validation_failed")
	expectCode(t, e.do("PATCH", A+"/"+av.ID.String(), both, map[string]any{"teamId": e.chatTeam()}), 400, "validation_failed")
	expectCode(t, e.do("POST", chatPath, both, map[string]any{"name": "Z", "kind": "slack", "webhookUrl": slackHook}), 400, "validation_failed")
	expectCode(t, e.do("PATCH", chatPath+"/"+orbitSvc.ID.String(), both, map[string]any{"teamId": someID}), 400, "validation_failed")
	var gotTeam string
	if err := e.pool.QueryRow(context.Background(), `SELECT team_id::text FROM chat_services WHERE id = $1`, orbitSvc.ID).Scan(&gotTeam); err != nil || gotTeam != e.chatTeam() {
		t.Fatalf("orbit service team = %q (%v), want unchanged", gotTeam, err)
	}
	// Orbit's delete and the hijack attempts left acme's service and its name.
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM chat_services WHERE org_id = $1 AND name = 'Acme'`, acmeID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("acme row = %d (%v)", n, err)
	}
	// The events are per org; the same name may exist in two orgs.
	e.must(201, e.do("POST", A, both, map[string]any{"name": "Orbit", "teamId": e.teamOfOrg(acmeID), "kind": "slack", "webhookUrl": acmeHook}))
	var orbit int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE org_id = $1 AND type = 'chat_service.updated'`, e.orgID).Scan(&orbit)
	if orbit != 1 {
		t.Fatalf("orbit has %d chat events, want 1", orbit)
	}
}

// The webhook is in no log line, event, idempotency row or response.
func TestChat_SecretNeverLeaks(t *testing.T) {
	e, f := newChatEnv(t, true)
	var bodies []string
	add := func(r resp) { bodies = append(bodies, string(r.body)) }
	r := e.do("POST", chatPath, e.chen, map[string]any{"name": "Eng", "teamId": e.chatTeam(), "kind": "slack", "webhookUrl": slackHook, "events": []string{"assigned"}})
	add(r)
	var v notify.ChatView
	r.json(t, &v)
	one := chatPath + "/" + v.ID.String()
	add(e.do("POST", chatPath, e.chen, map[string]any{"name": "Bad", "teamId": e.chatTeam(), "kind": "slack", "webhookUrl": slackHook + "?bad", "events": []string{}}))
	add(e.do("PATCH", one, e.chen, map[string]any{"webhookUrl": slackHook + "?bad"}))
	add(e.do("PATCH", one, e.chen, map[string]any{"webhookUrl": slackHook, "events": []string{"comment"}}))
	add(e.do("GET", chatPath, e.chen, nil))
	add(e.do("GET", one, e.chen, nil))
	add(e.do("POST", one+"/test", e.chen, nil))
	f.set(500)
	add(e.do("POST", one+"/test", e.chen, nil))
	f.Close()
	add(e.do("POST", one+"/test", e.chen, nil))
	add(e.do("DELETE", one, e.chen, nil))
	for _, b := range bodies {
		if strings.Contains(b, slackSecret) {
			t.Errorf("a response holds the secret: %s", b)
		}
	}
	if l := e.logs.String(); strings.Contains(l, slackSecret) || strings.Contains(l, "/services/T0001") {
		t.Errorf("log holds the webhook: %s", l)
	}
	for _, q := range []string{
		`SELECT coalesce(string_agg(payload::text, ' '), '') FROM events`,
		`SELECT coalesce(string_agg(t::text, ' '), '') FROM idempotency_keys t`,
		`SELECT coalesce(string_agg(s::text, ' '), '') FROM chat_outbox s`,
	} {
		var text string
		if err := e.pool.QueryRow(context.Background(), q).Scan(&text); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(text, slackSecret) {
			t.Errorf("stored in clear (%s)", q)
		}
	}
}

// A change is seen on the org's change feed, so another admin's page refreshes
// (the web client listens for chat_service.updated).
func TestChat_EventOnFeed(t *testing.T) {
	e, _ := newChatEnv(t, true)
	v := e.createChat(e.chen, map[string]any{"name": "Eng", "teamId": e.chatTeam(), "kind": "slack", "webhookUrl": slackHook, "events": []string{"comment"}})
	evs := e.eventsOfType(e.chen, "chat_service.updated")
	if len(evs) != 1 || evs[0].EntityType != "org" || !strings.Contains(string(evs[0].Payload), v.ID.String()) {
		t.Fatalf("events %+v", evs)
	}
}

// Fluxer (LAUN-206): accepted on a public host with a webhook path, refused on a
// private or .lan host or a wrong path, posts the Discord body, is org-scoped,
// and the URL is never returned.
func TestChat_Fluxer(t *testing.T) {
	e, f := newChatEnv(t, true)
	hook := "https://chat.example.com/api/webhooks/123/" + slackSecret
	for name, u := range map[string]string{
		"lan": "https://fluxer.lan/api/webhooks/1/a", "private": "https://10.0.0.5/api/webhooks/1/a",
		"path": "https://chat.example.com/hooks/a", "http": "http://chat.example.com/api/webhooks/1/a",
	} {
		r := e.do("POST", chatPath, e.chen, map[string]any{"name": "F-" + name, "teamId": e.chatTeam(), "kind": "fluxer", "webhookUrl": u})
		expectCode(t, r, 400, "validation_failed")
		if strings.Contains(string(r.body), u) {
			t.Errorf("%s: the error repeats the URL", name)
		}
	}
	r := e.do("POST", chatPath, e.chen, map[string]any{"name": "Fluxer", "teamId": e.chatTeam(), "kind": "fluxer", "webhookUrl": hook})
	if r.status != 201 {
		t.Fatalf("create = %d %s", r.status, r.body)
	}
	var v notify.ChatView
	r.json(t, &v)
	if v.Kind != "fluxer" || v.Host != "chat.example.com" || strings.Contains(string(r.body), slackSecret) {
		t.Fatalf("view %+v / %s", v, r.body)
	}
	// The kind never changes: an unknown "kind" field is refused, and a GET still says fluxer.
	expectCode(t, e.do("PATCH", chatPath+"/"+v.ID.String(), e.chen, map[string]any{"teamId": e.chatTeam(), "kind": "custom"}), 400, "bad_request")
	var after notify.ChatView
	e.must(200, e.do("GET", chatPath+"/"+v.ID.String(), e.chen, nil)).json(t, &after)
	if after.Kind != "fluxer" {
		t.Fatalf("kind changed to %q", after.Kind)
	}
	// A Fluxer URL cannot be saved on a service of another kind's rules, and a PATCH is checked for the kind.
	expectCode(t, e.do("PATCH", chatPath+"/"+v.ID.String(), e.chen, map[string]any{"webhookUrl": "https://chat.example.com/hooks/a"}), 400, "validation_failed")
	if g := e.do("GET", chatPath+"/"+v.ID.String(), e.chen, nil); strings.Contains(string(g.body), slackSecret) {
		t.Fatalf("GET returns the URL: %s", g.body)
	}
	e.must(200, e.do("POST", chatPath+"/"+v.ID.String()+"/test", e.chen, nil))
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) != 1 || !strings.Contains(f.bodies[0], `"content":"Launchpad test message.`) || !strings.Contains(f.bodies[0], `"allowed_mentions"`) || strings.Contains(f.bodies[0], `"kind"`) {
		t.Fatalf("fluxer body: %v", f.bodies)
	}
	if f.hits[0] != "/api/webhooks/123/"+slackSecret {
		t.Fatalf("path %v", f.hits)
	}
	if l := e.logs.String(); strings.Contains(l, slackSecret) {
		t.Errorf("log holds the webhook")
	}
}

// A Fluxer service of another org is 404 for every verb.
func TestChat_FluxerCrossOrg(t *testing.T) {
	e, f := newChatEnv(t, true)
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "admin")
	both, _ := e.newToken(chen.ID, tokenOpts{})
	var av notify.ChatView
	e.must(201, e.do("POST", "/api/v1/orgs/acme/chat-services", both, map[string]any{"name": "Acme", "teamId": e.teamOfOrg(acmeID), "kind": "fluxer",
		"webhookUrl": "https://chat.example.com/api/webhooks/9/ACMEONLYTOKEN"})).json(t, &av)
	if l := e.chatList(both); len(l.Items) != 0 {
		t.Fatalf("leak %+v", l)
	}
	for _, m := range []string{"GET", "PATCH", "POST"} {
		p := chatPath + "/" + av.ID.String()
		if m == "POST" {
			p += "/test"
		}
		expectCode(t, e.do(m, p, both, map[string]any{"name": "x"}), 404, "not_found")
	}
	if f.count() != 0 {
		t.Fatal("a cross-org message was posted")
	}
}

// chatTeam is the id of the org's first team, the team a test service joins.
func (e *env) chatTeam() string { return e.teamOfOrg(e.orgID) }

func (e *env) teamOfOrg(org uuid.UUID) string {
	e.t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), `SELECT id::text FROM teams WHERE org_id = $1 ORDER BY created_at, id LIMIT 1`, org).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// A service has a team: POST needs it, PATCH moves it, every body returns it
// (LAUN-209, decisions-T38). A team with services cannot be deleted (409
// team_in_use); once the services are moved or removed it can.
func TestChat_ServiceTeamAndTeamDelete(t *testing.T) {
	e, _ := newChatEnv(t, true)
	orbit := e.chatTeam()
	svc := e.createChat(e.chen, map[string]any{"name": "Eng", "teamId": orbit, "kind": "slack", "webhookUrl": slackHook})
	if svc.TeamID.String() != orbit {
		t.Fatalf("created teamId = %s, want %s", svc.TeamID, orbit)
	}
	var second string
	if err := e.pool.QueryRow(context.Background(), `INSERT INTO teams (org_id, key, name) VALUES ($1, 'SEC', 'Second') RETURNING id::text`, e.orgID).Scan(&second); err != nil {
		t.Fatal(err)
	}
	// The cap counts per org, not per team.
	for i := 0; i < notify.ChatMaxServices-1; i++ {
		tm := orbit
		if i%2 == 0 {
			tm = second
		}
		e.createChat(e.chen, map[string]any{"name": fmt.Sprintf("n%d", i), "teamId": tm, "kind": "custom", "webhookUrl": fmt.Sprintf("https://chat.example.com/h/%d", i)})
	}
	expectCode(t, e.do("POST", chatPath, e.chen, map[string]any{"name": "over", "teamId": second, "kind": "slack", "webhookUrl": slackHook}), 409, "integration_limit")
	// The second team has services: 409 team_in_use.
	teamPath := O + "/teams/SEC"
	expectCode(t, e.do("DELETE", teamPath, e.chen, nil), 409, "team_in_use")
	// Move them all to the first team (PATCH returns the new team); then it deletes.
	for _, it := range e.chatList(e.chen).Items {
		if it.TeamID.String() == second {
			var v notify.ChatView
			e.must(200, e.do("PATCH", chatPath+"/"+it.ID.String(), e.chen, map[string]any{"teamId": orbit})).json(t, &v)
			if v.TeamID.String() != orbit {
				t.Fatalf("patched teamId = %s", v.TeamID)
			}
		}
	}
	if r := e.do("DELETE", teamPath, e.chen, nil); r.status != 204 {
		t.Fatalf("team delete = %d %s", r.status, r.body)
	}
	// The events carry the team id and nothing secret.
	if n := e.countEvents("chat_service.updated"); n == 0 {
		t.Fatal("no chat_service.updated event")
	}
}
