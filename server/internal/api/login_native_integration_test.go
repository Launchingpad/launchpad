package api

// LAUN-122: native login mode. POST /auth/otp/verify with "client":"native"
// returns a native-kind bearer token (LAUN-129; kind session before) in the body and sets no cookie and no
// CSRF value. Every other login rule is the web rule.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"devops/internal/auth"
	"devops/internal/httpx"
	"devops/internal/login"
	"devops/internal/tracker"
)

const verifyPath = "/api/v1/auth/otp/verify"

// nativeLogin runs the whole native login and returns the token and the
// raw answer. The token is NOT registered as a cookie session, so do()
// sends it as a bearer token.
func (e *env) nativeLogin(email string) (string, resp) {
	e.t.Helper()
	code := e.requestCode(email)
	if code == "" {
		e.t.Fatalf("no code sent to %s", email)
	}
	r := e.must(200, e.do("POST", verifyPath, "", map[string]any{"email": email, "code": code, "client": "native"}))
	var body login.NativeResponse
	r.json(e.t, &body)
	if body.Token == "" {
		e.t.Fatalf("native login returned no token: %s", r.body)
	}
	return body.Token, r
}

func TestLoginNative_BearerSessionAndLogout(t *testing.T) {
	e := newEnv(t)
	token, r := e.nativeLogin(chenEmail)

	if len(r.cookies()) != 0 || len(r.header.Values("Set-Cookie")) != 0 {
		t.Fatalf("native login set a cookie: %v", r.header.Values("Set-Cookie"))
	}
	if cc := r.header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control %q", cc)
	}
	// The body has a token and an expiry and NO csrfToken key.
	var raw map[string]json.RawMessage
	r.json(t, &raw)
	if _, has := raw["csrfToken"]; has {
		t.Fatalf("native body has a csrfToken: %s", r.body)
	}
	var body login.NativeResponse
	r.json(t, &body)
	if !auth.WellFormed(token) || body.User.Username != "chen" || len(body.Orgs) != 1 || body.Orgs[0].Slug != "orbit" {
		t.Fatalf("native body %+v", body)
	}
	if d := time.Until(body.ExpiresAt); d < 29*24*time.Hour || d > 30*24*time.Hour+time.Minute {
		t.Fatalf("expiresAt in %v, want about 30 days", d)
	}
	if n := e.countRows(`SELECT count(*) FROM api_tokens WHERE kind = 'native' AND org_id IS NULL AND name = 'native session' AND token_hash = $1`, auth.HashToken(token)); n != 1 {
		t.Fatalf("no unpinned native session row (%d)", n)
	}

	// As a bearer: reads, and writes with no CSRF header.
	var me tracker.Me
	e.must(200, e.do("GET", "/api/v1/me", token, nil)).json(t, &me)
	if me.Username != "chen" || me.TokenKind != "native" || strings.Join(me.Scopes, ",") != "read,write,admin" || me.PinnedOrgID != nil {
		t.Fatalf("me: %+v", me)
	}
	is := e.createIssue(token, map[string]any{"title": "made by the native app"})
	if is.Creator == nil || is.Creator.Username != "chen" {
		t.Fatalf("creator %+v", is.Creator)
	}
	var sess login.Response
	e.must(200, e.do("GET", "/api/v1/auth/session", token, nil)).json(t, &sess)
	if sess.CSRFToken != "" || sess.User.Username != "chen" {
		t.Fatalf("auth/session: %+v", sess)
	}
	// Logout revokes it; the token is dead everywhere.
	e.must(204, e.do("POST", "/api/v1/auth/logout", token, nil))
	if n := e.countRows(`SELECT count(*) FROM api_tokens WHERE kind = 'native' AND revoked_at IS NOT NULL AND token_hash = $1`, auth.HashToken(token)); n != 1 {
		t.Fatalf("token not revoked (%d)", n)
	}
	if code := e.must(401, e.do("GET", "/api/v1/me", token, nil)).errCode(t); code != "token_revoked" {
		t.Fatalf("after logout: %s", code)
	}
	// Logout by a PAT is still refused.
	e.must(403, e.do("POST", "/api/v1/auth/logout", e.chen, nil))
}

func TestLoginNative_ClientValue(t *testing.T) {
	e := newEnv(t)
	code := e.requestCode(chenEmail)
	// An unknown client is 400 and spends nothing: the code still works.
	r := e.must(400, e.do("POST", verifyPath, "", map[string]any{"email": chenEmail, "code": code, "client": "tv"}))
	if r.errCode(t) != "validation_failed" || len(r.cookies()) != 0 {
		t.Fatalf("%s", r.body)
	}
	// "web" and absent are the cookie login.
	r = e.must(200, e.do("POST", verifyPath, "", map[string]any{"email": chenEmail, "code": code, "client": "web"}))
	if r.cookies()[e.names.Session] == nil || r.cookies()[e.names.CSRF] == nil {
		t.Fatalf("client web set no cookies: %v", r.header)
	}
	var web login.Response
	r.json(t, &web)
	if web.CSRFToken == "" {
		t.Fatal("web body has no csrfToken")
	}
	var raw map[string]json.RawMessage
	r.json(t, &raw)
	if _, has := raw["token"]; has {
		t.Fatalf("a web body must not carry the token: %s", r.body)
	}
}

// The web login is byte for byte what it was: cookies, csrfToken, no token.
func TestLoginNative_WebLoginUnchanged(t *testing.T) {
	e := newEnv(t)
	session, r := e.loginAs(chenEmail)
	var raw map[string]json.RawMessage
	r.json(t, &raw)
	if len(raw) != 3 || raw["user"] == nil || raw["orgs"] == nil || raw["csrfToken"] == nil {
		t.Fatalf("web body keys changed: %s", r.body)
	}
	if n := e.countRows(`SELECT count(*) FROM api_tokens WHERE kind = 'session' AND name = 'web session' AND token_hash = $1`, auth.HashToken(session)); n != 1 {
		t.Fatal("web session row changed")
	}
	// Cookie writes still need the CSRF header.
	e.must(403, e.do("POST", "/api/v1/orgs/orbit/issues", session, map[string]any{"team": "ORB", "title": "x"}, auth.CSRFHeader, "bad"))
	e.createIssue(session, map[string]any{"title": "web still works"})
}

// The failure rules are the web rules: one 401 for every cause, the 300 ms
// floor, the five-try lock, no answer that tells if an email exists.
func TestLoginNative_SameRulesAsWeb(t *testing.T) {
	e := newEnvWith(t, envOpts{login: login.Options{Floor: 300 * time.Millisecond}})
	native := func(email, code string) (resp, time.Duration) {
		start := time.Now()
		r := e.do("POST", verifyPath, "", map[string]any{"email": email, "code": code, "client": "native"})
		return r, time.Since(start)
	}
	web := func(email, code string) resp {
		return e.do("POST", verifyPath, "", map[string]any{"email": email, "code": code})
	}

	// Unknown email: same answer as web, and the floor holds.
	nr, el := native("nobody@example.com", "123456")
	wr := web("nobody@example.com", "123456")
	if nr.status != 401 || string(nr.body) != string(wr.body) || len(nr.cookies()) != 0 {
		t.Fatalf("unknown email: native %d %s, web %d %s", nr.status, nr.body, wr.status, wr.body)
	}
	if el < 290*time.Millisecond {
		t.Fatalf("native failure took %v, floor is 300 ms", el)
	}

	// Five wrong tries lock the code; the right code then fails.
	code := e.requestCode(chenEmail)
	for range 5 {
		r, _ := native(chenEmail, wrongCode(code))
		if r.status != 401 || r.errCode(t) != "invalid_code" {
			t.Fatalf("wrong try: %d %s", r.status, r.body)
		}
	}
	if r, _ := native(chenEmail, code); r.status != 401 || r.errCode(t) != "invalid_code" {
		t.Fatalf("locked code gave %d %s", r.status, r.body)
	}
	if n := e.countRows(`SELECT count(*) FROM api_tokens WHERE kind IN ('session', 'native') AND name = 'native session'`); n != 0 {
		t.Fatalf("%d native sessions after a locked code", n)
	}

	// Success also takes the floor.
	code = e.requestCode(chenEmail)
	r, el := native(chenEmail, code)
	if r.status != 200 || el < 290*time.Millisecond {
		t.Fatalf("native success: %d in %v", r.status, el)
	}
	// And the code is single use.
	if r, _ := native(chenEmail, code); r.status != 401 {
		t.Fatalf("a used code gave %d", r.status)
	}
}

func TestLoginNative_AgentsCannotLogIn(t *testing.T) {
	e := newEnvWith(t, envOpts{login: login.Options{FixedCode: "000000"}})
	var m tracker.Member
	e.must(201, e.do("POST", agentsPath, e.chen, map[string]any{"username": "native-bot", "name": "N"})).json(t, &m)
	mustExec(t, e, `UPDATE users SET email = 'native-bot@example.com' WHERE id = $1`, m.ID)
	r := e.must(401, e.do("POST", verifyPath, "", map[string]any{"email": "native-bot@example.com", "code": "000000", "client": "native"}))
	if r.errCode(t) != "invalid_code" || len(r.cookies()) != 0 || strings.Contains(string(r.body), `"token"`) {
		t.Fatalf("%s", r.body)
	}
	if n := e.countRows(`SELECT count(*) FROM api_tokens WHERE user_id = $1 AND kind IN ('session', 'native')`, m.ID); n != 0 {
		t.Fatalf("%d sessions for the agent", n)
	}
}

// An email_unproven account: the first native login proves the email,
// clears the flag and revokes every older token, exactly as the web login.
func TestLoginNative_EmailUnproven(t *testing.T) {
	e := newEnv(t)
	u := e.newHuman("unproven", "unproven@example.com", "member")
	mustExec(t, e, `UPDATE users SET email_unproven = true WHERE id = $1`, u.ID)
	earlier, _ := e.newToken(u.ID, tokenOpts{pin: &e.orgID, scopes: []string{"read"}})
	e.must(200, e.do("GET", "/api/v1/me", earlier, nil))

	token, _ := e.nativeLogin("unproven@example.com")
	if r := e.do("GET", "/api/v1/me", earlier, nil); r.status != 401 {
		t.Fatalf("a token from before the first login still works: %d", r.status)
	}
	e.must(200, e.do("GET", "/api/v1/me", token, nil))
	if e.user("unproven").EmailUnproven {
		t.Fatal("the flag is not cleared")
	}
}

// Cross-org: a native session is unpinned, so org access is membership. A
// member of another org only is 404 on this org, and its login body lists
// only its own orgs. Chen's token sees both of his.
func TestLoginNative_CrossOrg(t *testing.T) {
	e := newEnv(t)
	acme := e.newOrg("acme", "ACM")
	ann := e.newHuman("ann", "ann@example.com", "member")
	mustExec(t, e, `DELETE FROM org_members WHERE user_id = $1 AND org_id = $2`, ann.ID, e.orgID)
	e.addMember(acme, ann.ID, "admin")

	token, r := e.nativeLogin("ann@example.com")
	var body login.NativeResponse
	r.json(t, &body)
	if len(body.Orgs) != 1 || body.Orgs[0].Slug != "acme" {
		t.Fatalf("ann's orgs: %+v", body.Orgs)
	}
	e.must(200, e.do("GET", "/api/v1/orgs/acme/issues", token, nil))
	for _, path := range []string{"/api/v1/orgs/orbit/issues", "/api/v1/orgs/orbit/teams", "/api/v1/orgs/orbit/tokens"} {
		if code := e.must(404, e.do("GET", path, token, nil)).errCode(t); code != "not_found" {
			t.Fatalf("%s: %s", path, code)
		}
	}
	// An issue of orbit is invisible to ann's session by id as well.
	is := e.createIssue(e.chen, map[string]any{"title": "orbit only"})
	e.must(404, e.do("GET", "/api/v1/orgs/acme/issues/"+is.Identifier, token, nil))
	// Her token is hers: it never acts as chen.
	var me tracker.Me
	e.must(200, e.do("GET", "/api/v1/me", token, nil)).json(t, &me)
	if me.Username != "ann" {
		t.Fatalf("me %+v", me)
	}
	// A session cannot mint a token with the admin scope (same rule as web).
	r = e.do("POST", "/api/v1/orgs/acme/tokens", token, map[string]any{"name": "x", "scopes": []string{"read", "admin"}, "expiresInDays": 5})
	if r.status != http.StatusBadRequest && r.status != http.StatusForbidden {
		t.Fatalf("a native session minted an admin token: %d %s", r.status, r.body)
	}
}

// bearerOnly sends secret as an Authorization header, whatever kind it is,
// with no cookie and no CSRF value.
func (e *env) bearerOnly(method, path, secret string, body any) resp {
	e.t.Helper()
	return e.do(method, path, "", body, "Authorization", "Bearer "+secret)
}

// cookieOnly sends secret as the session cookie with a VALID CSRF pair, so
// only the token kind can refuse it.
func (e *env) cookieOnly(method, path, secret string, body any) resp {
	e.t.Helper()
	csrf := auth.CSRFToken(secret)
	return e.do(method, path, "", body, "Cookie", e.names.Session+"="+secret+"; "+e.names.CSRF+"="+csrf, auth.CSRFHeader, csrf)
}

// LAUN-129: the value of the web cookie is no bearer token. The refusal is
// the answer an unknown token gets, byte for byte, so it does not say that
// the value is a real session.
func TestLoginNative_WebValueAsBearerRefused(t *testing.T) {
	e := newEnv(t)
	session, _ := e.loginAs(chenEmail)
	unknown, _ := auth.GenerateToken()

	for _, m := range []struct{ method, path string }{
		{"GET", "/api/v1/me"}, {"GET", "/api/v1/orgs/orbit/issues"}, {"POST", "/api/v1/orgs/orbit/issues"}, {"POST", "/api/v1/auth/logout"},
	} {
		var body any
		if m.method == "POST" && strings.HasSuffix(m.path, "/issues") {
			body = map[string]any{"team": "ORB", "title": "no"}
		}
		got := e.bearerOnly(m.method, m.path, session, body)
		want := e.bearerOnly(m.method, m.path, unknown.Plaintext, body)
		if got.status != 401 || got.errCode(t) != "invalid_token" || got.status != want.status || string(got.body) != string(want.body) ||
			got.header.Get("WWW-Authenticate") != want.header.Get("WWW-Authenticate") {
			t.Fatalf("%s %s: web value as bearer %d %s, unknown token %d %s", m.method, m.path, got.status, got.body, want.status, want.body)
		}
	}
	// Nothing was made and nothing was revoked by the refused calls.
	if n := e.countRows(`SELECT count(*) FROM issues WHERE title = 'no'`); n != 0 {
		t.Fatalf("a refused bearer call made %d issues", n)
	}
	if n := e.countRows(`SELECT count(*) FROM api_tokens WHERE kind = 'session' AND revoked_at IS NULL AND token_hash = $1`, auth.HashToken(session)); n != 1 {
		t.Fatal("the refused logout revoked the web session")
	}
	// The same value as the cookie, with the CSRF pair, still works.
	e.must(200, e.cookieOnly("GET", "/api/v1/me", session, nil))
	e.createIssue(session, map[string]any{"title": "cookie still works"})
}

// LAUN-129: a native token is a bearer credential only. As the session
// cookie, even with a valid CSRF pair, it is refused like an unknown token.
func TestLoginNative_NativeTokenAsCookieRefused(t *testing.T) {
	e := newEnv(t)
	token, _ := e.nativeLogin(chenEmail)
	unknown, _ := auth.GenerateToken()
	for _, m := range []string{"GET", "POST"} {
		path := "/api/v1/me"
		if m == "POST" {
			path = "/api/v1/auth/logout"
		}
		got := e.cookieOnly(m, path, token, nil)
		want := e.cookieOnly(m, path, unknown.Plaintext, nil)
		if got.status != 401 || got.errCode(t) != "invalid_token" || string(got.body) != string(want.body) {
			t.Fatalf("%s native as cookie: %d %s (unknown: %d %s)", m, got.status, got.body, want.status, want.body)
		}
	}
	// A cookie write without the CSRF pair is still 403 csrf_failed first.
	e.must(403, e.do("POST", "/api/v1/auth/logout", "", nil, "Cookie", e.names.Session+"="+token))
	// The refused calls did not log it out: the bearer still works.
	e.must(200, e.bearerOnly("GET", "/api/v1/me", token, nil))
	// With both a native bearer and a stale cookie, the bearer wins.
	e.must(200, e.do("GET", "/api/v1/me", token, nil, "Cookie", e.names.Session+"=junk"))
}

// Logout works in both modes and kills only the caller's own login.
func TestLoginNative_LogoutBothModes(t *testing.T) {
	e := newEnv(t)
	native, _ := e.nativeLogin(chenEmail)
	native2, _ := e.nativeLogin(chenEmail)
	web, _ := e.loginAs(chenEmail)

	e.must(204, e.bearerOnly("POST", "/api/v1/auth/logout", native, nil))
	e.must(401, e.bearerOnly("GET", "/api/v1/me", native, nil))
	e.must(200, e.bearerOnly("GET", "/api/v1/me", native2, nil)) // another native login lives
	e.must(200, e.cookieOnly("GET", "/api/v1/me", web, nil))     // so does the web one

	// Web logout needs the CSRF pair and revokes the web session only.
	e.must(403, e.do("POST", "/api/v1/auth/logout", "", nil, "Cookie", e.names.Session+"="+web))
	r := e.must(204, e.cookieOnly("POST", "/api/v1/auth/logout", web, nil))
	if len(r.header.Values("Set-Cookie")) != 2 {
		t.Fatalf("web logout did not clear both cookies: %v", r.header.Values("Set-Cookie"))
	}
	e.must(401, e.cookieOnly("GET", "/api/v1/me", web, nil))
	e.must(200, e.bearerOnly("GET", "/api/v1/me", native2, nil))
	// A second logout of a dead native token is the plain 401.
	e.must(401, e.bearerOnly("POST", "/api/v1/auth/logout", native, nil))
}

// LAUN-129: a native token keeps every session rule: it mints only a token
// with an expiry and without the admin scope, minting is rate limited, and
// the minted PAT is the one listed. The native login itself is not listed.
func TestLoginNative_MintRulesAndTokenList(t *testing.T) {
	e := newEnv(t)
	token, _ := e.nativeLogin(chenEmail)
	tokens := "/api/v1/orgs/orbit/tokens"

	var before httpx.Page[tracker.Token]
	e.must(200, e.bearerOnly("GET", tokens, token, nil)).json(t, &before)
	for _, it := range before.Items {
		if it.Kind == "native" || it.Kind == "session" {
			t.Fatalf("an unpinned login token is listed: %+v", it)
		}
	}
	// No admin scope, no token without an expiry.
	for _, body := range []map[string]any{
		{"name": "x", "scopes": []string{"read", "admin"}, "expiresInDays": 5},
		{"name": "x", "scopes": []string{"read"}},
	} {
		r := e.bearerOnly("POST", tokens, token, body)
		if r.status != 400 && r.status != 403 {
			t.Fatalf("native minted %v: %d %s", body, r.status, r.body)
		}
	}
	body := map[string]any{"name": "from-native", "scopes": []string{"read"}, "expiresInDays": 30}
	for i := range sessionMintBurst {
		if r := e.bearerOnly("POST", tokens, token, body); r.status != 201 {
			t.Fatalf("mint %d: %d %s", i, r.status, r.body)
		}
	}
	r := e.must(429, e.bearerOnly("POST", tokens, token, body))
	if r.errCode(t) != "rate_limited" || r.header.Get("Retry-After") == "" {
		t.Fatalf("%s %v", r.body, r.header)
	}
	// A PAT bearer is still not limited.
	e.must(201, e.do("POST", tokens, e.chen, body))
	var after httpx.Page[tracker.Token]
	e.must(200, e.do("GET", tokens, e.chen, nil)).json(t, &after)
	if len(after.Items) != len(before.Items)+sessionMintBurst+1 {
		t.Fatalf("token list: %d before, %d after", len(before.Items), len(after.Items))
	}
}
