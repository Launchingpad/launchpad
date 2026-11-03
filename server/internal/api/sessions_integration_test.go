package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/auth"
	"devops/internal/dbq"
	"devops/internal/tracker"
)

// GET /me/sessions, DELETE /me/sessions/{id}, POST /me/sessions/revoke-others
// (LAUN-130, decisions-T6a "Sessions list").

const sessionsPath = "/api/v1/me/sessions"

func (e *env) sessionList(token string) []tracker.Session {
	e.t.Helper()
	var l page[tracker.Session]
	e.must(200, e.do("GET", sessionsPath, token, nil)).json(e.t, &l)
	return l.Items
}

// sessionID is the row id of a login secret.
func (e *env) sessionID(secret string) uuid.UUID {
	e.t.Helper()
	var id uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM api_tokens WHERE token_hash = $1`, auth.HashToken(secret)).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func hasSession(l []tracker.Session, id uuid.UUID) bool {
	for _, s := range l {
		if s.ID == id {
			return true
		}
	}
	return false
}

func TestSessions_ListOwnOnly(t *testing.T) {
	e := newEnv(t)
	e.newHuman("dana", "dana@example.com", "member")
	web, _ := e.loginAs(chenEmail)
	native, _ := e.nativeLogin(chenEmail)
	pat, patID := e.newToken(e.user("chen").ID, tokenOpts{})
	danaWeb, _ := e.loginAs("dana@example.com")
	danaNative, _ := e.nativeLogin("dana@example.com")

	// Cross-org: a person who belongs to another org only.
	otherOrg := e.newOrg("other", "OTH")
	em := "eve@example.com"
	eve, err := e.q.InsertUser(context.Background(), dbq.InsertUserParams{Username: "eve", Name: "eve", Email: &em})
	if err != nil {
		t.Fatal(err)
	}
	e.addMember(otherOrg, eve.ID, "admin")
	eveWeb, _ := e.loginAs(em)

	webID, nativeID := e.sessionID(web), e.sessionID(native)
	list := e.sessionList(web)
	if len(list) != 2 || !hasSession(list, webID) || !hasSession(list, nativeID) {
		t.Fatalf("chen sees %+v", list)
	}
	for _, s := range list {
		if s.ID == webID && (!s.Current || s.Kind != "session" || s.Name != "web session") {
			t.Fatalf("web row %+v", s)
		}
		if s.ID == nativeID && (s.Current || s.Kind != "native" || s.Name != "native session") {
			t.Fatalf("native row %+v", s)
		}
		if s.ExpiresAt == nil || time.Until(*s.ExpiresAt) < 29*24*time.Hour {
			t.Fatalf("expiry %+v", s)
		}
	}
	// From the native bearer the "current" mark moves; a PAT caller has no current row.
	for _, s := range e.sessionList(native) {
		if s.Current != (s.ID == nativeID) {
			t.Fatalf("current on native: %+v", s)
		}
	}
	for _, s := range e.sessionList(pat) {
		if s.Current {
			t.Fatalf("a PAT caller has no current session: %+v", s)
		}
	}
	// The PAT is never listed; other people's sessions are not listed.
	if hasSession(list, patID) || hasSession(list, e.sessionID(danaWeb)) || hasSession(list, e.sessionID(danaNative)) || hasSession(list, e.sessionID(eveWeb)) {
		t.Fatalf("foreign row in %+v", list)
	}
	if l := e.sessionList(eveWeb); len(l) != 1 || l[0].ID != e.sessionID(eveWeb) {
		t.Fatalf("eve sees %+v", l)
	}
	// Revoked and expired rows are not listed.
	e.must(204, e.do("DELETE", sessionsPath+"/"+nativeID.String(), web, nil))
	if _, err := e.pool.Exec(context.Background(), `UPDATE api_tokens SET expires_at = now() - interval '1 minute' WHERE id = $1`, webID); err != nil {
		t.Fatal(err)
	}
	if l := e.sessionList(pat); len(l) != 0 {
		t.Fatalf("revoked or expired listed: %+v", l)
	}
}

func TestSessions_ResponseHasNoSecret(t *testing.T) {
	e := newEnv(t)
	web, _ := e.loginAs(chenEmail)
	native, _ := e.nativeLogin(chenEmail)
	r := e.must(200, e.do("GET", sessionsPath, web, nil))
	body := string(r.body)
	for _, bad := range []string{web, native, "odt_", "token", "hash", "prefix", "ip", "agent"} {
		if bad == "ip" || bad == "agent" || bad == "odt_" || bad == "token" || bad == "hash" || bad == "prefix" {
			if strings.Contains(strings.ToLower(body), `"`+bad) {
				t.Fatalf("response has a %q field: %s", bad, body)
			}
			continue
		}
		if strings.Contains(body, bad) {
			t.Fatalf("response holds a secret: %s", body)
		}
	}
	if !strings.Contains(r.header.Get("Cache-Control"), "no-store") {
		t.Fatalf("Cache-Control %q", r.header.Get("Cache-Control"))
	}
}

func TestSessions_RevokeOwnAndForeign(t *testing.T) {
	e := newEnv(t)
	e.newHuman("dana", "dana@example.com", "member")
	web, _ := e.loginAs(chenEmail)
	native, _ := e.nativeLogin(chenEmail)
	pat, patID := e.newToken(e.user("chen").ID, tokenOpts{})
	danaWeb, _ := e.loginAs("dana@example.com")
	danaID := e.sessionID(danaWeb)
	nativeID := e.sessionID(native)

	// Another person's session id (any org): 404, and it keeps working.
	r := e.must(404, e.do("DELETE", sessionsPath+"/"+danaID.String(), web, nil))
	if r.errCode(t) != "not_found" {
		t.Fatalf("code %s", r.body)
	}
	e.must(200, e.do("GET", "/api/v1/me", danaWeb, nil))
	// A PAT id (even my own), an unknown id and a malformed id: 404.
	e.must(404, e.do("DELETE", sessionsPath+"/"+patID.String(), web, nil))
	e.must(404, e.do("DELETE", sessionsPath+"/"+uuid.NewString(), web, nil))
	e.must(404, e.do("DELETE", sessionsPath+"/not-a-uuid", web, nil))
	e.must(200, e.do("GET", "/api/v1/me", pat, nil))
	// The 404 for a foreign id is the 404 of an unknown id.
	if a, b := e.do("DELETE", sessionsPath+"/"+danaID.String(), web, nil), e.do("DELETE", sessionsPath+"/"+uuid.NewString(), web, nil); a.status != b.status || a.errCode(t) != b.errCode(t) {
		t.Fatalf("foreign %d %s vs unknown %d %s", a.status, a.body, b.status, b.body)
	}

	// My own: revoked at once, repeat is 204, the token is dead.
	e.must(200, e.do("GET", "/api/v1/me", native, nil))
	e.must(204, e.do("DELETE", sessionsPath+"/"+nativeID.String(), web, nil))
	e.must(401, e.do("GET", "/api/v1/me", native, nil))
	e.must(204, e.do("DELETE", sessionsPath+"/"+nativeID.String(), web, nil))

	// A PAT with the write scope may revoke a session of its own user, not another's.
	n2, _ := e.nativeLogin(chenEmail)
	e.must(204, e.do("DELETE", sessionsPath+"/"+e.sessionID(n2).String(), pat, nil))
	e.must(401, e.do("GET", "/api/v1/me", n2, nil))
	e.must(404, e.do("DELETE", sessionsPath+"/"+danaID.String(), pat, nil))
	// A read-only token cannot revoke.
	ro, _ := e.newToken(e.user("chen").ID, tokenOpts{scopes: []string{"read"}})
	if c := e.must(403, e.do("DELETE", sessionsPath+"/"+e.sessionID(web).String(), ro, nil)).errCode(t); c != "insufficient_scope" {
		t.Fatalf("read token: %s", c)
	}
	e.must(403, e.do("POST", sessionsPath+"/revoke-others", ro, nil))
	e.must(200, e.do("GET", "/api/v1/me", web, nil))
}

func TestSessions_RevokeCurrentCookieSession(t *testing.T) {
	e := newEnv(t)
	web, _ := e.loginAs(chenEmail)
	r := e.must(204, e.do("DELETE", sessionsPath+"/"+e.sessionID(web).String(), web, nil))
	if c, ok := r.cookies()[e.names.Session]; !ok || c.MaxAge >= 0 {
		t.Fatalf("session cookie not cleared: %v", r.header.Values("Set-Cookie"))
	}
	e.must(401, e.do("GET", "/api/v1/me", web, nil))
}

func TestSessions_RevokeOthers(t *testing.T) {
	e := newEnv(t)
	e.newHuman("dana", "dana@example.com", "member")
	web, _ := e.loginAs(chenEmail)
	n1, _ := e.nativeLogin(chenEmail)
	n2, _ := e.nativeLogin(chenEmail)
	pat, _ := e.newToken(e.user("chen").ID, tokenOpts{})
	danaWeb, _ := e.loginAs("dana@example.com")
	danaNative, _ := e.nativeLogin("dana@example.com")

	var out tracker.RevokedSessions
	e.must(200, e.do("POST", sessionsPath+"/revoke-others", n1, nil)).json(t, &out)
	if out.Revoked != 2 { // the web session and n2; not n1 (current), not the PAT
		t.Fatalf("revoked %d", out.Revoked)
	}
	e.must(200, e.do("GET", "/api/v1/me", n1, nil))
	e.must(401, e.do("GET", "/api/v1/me", web, nil))
	e.must(401, e.do("GET", "/api/v1/me", n2, nil))
	e.must(200, e.do("GET", "/api/v1/me", pat, nil))     // a PAT is not a session
	e.must(200, e.do("GET", "/api/v1/me", danaWeb, nil)) // another person is untouched
	e.must(200, e.do("GET", "/api/v1/me", danaNative, nil))
	// Again: nothing left to revoke.
	e.must(200, e.do("POST", sessionsPath+"/revoke-others", n1, nil)).json(t, &out)
	if out.Revoked != 0 {
		t.Fatalf("second call revoked %d", out.Revoked)
	}
	// From a PAT, every session goes, including mine; Dana's stay.
	e.must(200, e.do("POST", sessionsPath+"/revoke-others", pat, nil)).json(t, &out)
	if out.Revoked != 1 {
		t.Fatalf("pat call revoked %d", out.Revoked)
	}
	e.must(401, e.do("GET", "/api/v1/me", n1, nil))
	e.must(200, e.do("GET", "/api/v1/me", danaWeb, nil))
	e.must(200, e.do("GET", "/api/v1/me", danaNative, nil))
}

func TestSessions_CSRFAndAgents(t *testing.T) {
	e := newEnv(t)
	web, _ := e.loginAs(chenEmail)
	id := e.sessionID(web)
	for _, c := range []struct{ method, path string }{
		{"DELETE", sessionsPath + "/" + id.String()},
		{"POST", sessionsPath + "/revoke-others"},
	} {
		if code := e.must(403, e.do(c.method, c.path, web, nil, auth.CSRFHeader, "")).errCode(t); code != "csrf_failed" {
			t.Fatalf("%s no CSRF: %s", c.method, code)
		}
		if code := e.must(403, e.do(c.method, c.path, web, nil, auth.CSRFHeader, "wrong")).errCode(t); code != "csrf_failed" {
			t.Fatalf("%s wrong CSRF: %s", c.method, code)
		}
	}
	e.must(200, e.do("GET", "/api/v1/me", web, nil)) // still alive after the refusals
	// No credential: 401.
	e.must(401, e.do("GET", sessionsPath, "", nil))
	// Agents: 403 forbidden on all three routes.
	bot, _ := e.newToken(e.user("claude").ID, tokenOpts{})
	for _, c := range []struct{ method, path string }{
		{"GET", sessionsPath}, {"DELETE", sessionsPath + "/" + id.String()}, {"POST", sessionsPath + "/revoke-others"},
	} {
		if code := e.must(403, e.do(c.method, c.path, bot, nil)).errCode(t); code != "forbidden" {
			t.Fatalf("agent %s %s: %s", c.method, c.path, code)
		}
	}
	e.must(200, e.do("GET", "/api/v1/me", web, nil))
}

func TestSessions_NoSecretsNoEventsNoIdempotency(t *testing.T) {
	e := newEnvWith(t, envOpts{logLevel: -4})
	web, _ := e.loginAs(chenEmail)
	native, _ := e.nativeLogin(chenEmail)
	before := e.countRows(`SELECT count(*) FROM events`)
	e.sessionList(web)
	e.must(204, e.do("DELETE", sessionsPath+"/"+e.sessionID(native).String(), web, nil, "Idempotency-Key", "k-1"))
	e.must(200, e.do("POST", sessionsPath+"/revoke-others", web, nil))
	logs := e.logs.String()
	if strings.Contains(logs, native) || strings.Contains(logs, web) {
		t.Fatal("a session secret is in the log")
	}
	if !strings.Contains(logs, "audit: session revoked") || !strings.Contains(logs, "audit: other sessions revoked") {
		t.Fatalf("audit lines are missing:\n%s", logs)
	}
	if after := e.countRows(`SELECT count(*) FROM events`); after != before {
		t.Fatalf("%d events written", after-before)
	}
	if n := e.countRows(`SELECT count(*) FROM idempotency_keys`); n != 0 {
		t.Fatalf("%d idempotency rows", n)
	}
}

func TestSessions_ListNewestFirst(t *testing.T) {
	e := newEnv(t)
	web, _ := e.loginAs(chenEmail)
	n1, _ := e.nativeLogin(chenEmail)
	n2, _ := e.nativeLogin(chenEmail)
	ids := []uuid.UUID{e.sessionID(web), e.sessionID(n1), e.sessionID(n2)}
	// Fixed, distinct creation times: n1 oldest, then n2, then web newest.
	for i, id := range []uuid.UUID{ids[1], ids[2], ids[0]} {
		if _, err := e.pool.Exec(context.Background(), `UPDATE api_tokens SET created_at = now() - make_interval(hours => $2) WHERE id = $1`, id, 3-i); err != nil {
			t.Fatal(err)
		}
	}
	l := e.sessionList(web)
	if len(l) != 3 || l[0].ID != ids[0] || l[1].ID != ids[2] || l[2].ID != ids[1] {
		t.Fatalf("order %+v, want web, n2, n1", l)
	}
}
