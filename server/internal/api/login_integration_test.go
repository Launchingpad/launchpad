package api

// T6a: email login, the cookie session, CSRF, token lifetime,
// SSE with a cookie, and no secret in logs, events or idempotency rows.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"devops/internal/auth"
	"devops/internal/login"
	"devops/internal/tracker"
)

const chenEmail = "chen@example.com"

// clock is a settable time source for login.Options.Now.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Now()} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (e *env) countRows(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func wrongCode(code string) string {
	var n int
	_, _ = fmt.Sscanf(code, "%d", &n)
	return fmt.Sprintf("%06d", (n+1)%1_000_000)
}

// AC: a code gives a session that survives (cookies alone work) and ends on
// logout (the token is revoked, and the old cookie is then 401).
func TestLogin_SessionSurvivesAndEndsOnLogout(t *testing.T) {
	e := newEnv(t)
	session, r := e.loginAs("Chen@example.com ") // any case, trimmed

	// Response body.
	var body login.Response
	r.json(t, &body)
	if body.User.Username != "chen" || body.User.Email != chenEmail || len(body.Orgs) != 1 ||
		body.Orgs[0].Slug != "orbit" || body.Orgs[0].Role != "admin" {
		t.Fatalf("login body %+v", body)
	}
	// Cookies: attributes, and csrfToken equals the CSRF cookie.
	cs := r.cookies()
	sc, cc := cs[e.names.Session], cs[e.names.CSRF]
	if sc == nil || cc == nil {
		t.Fatalf("cookies %v", cs)
	}
	month := 30 * 24 * 60 * 60
	for _, c := range []*http.Cookie{sc, cc} {
		if c.Path != "/" || c.SameSite != http.SameSiteLaxMode || !c.Secure || c.MaxAge < month-60 || c.MaxAge > month {
			t.Fatalf("cookie %s attributes: %+v", c.Name, c)
		}
	}
	if sc.Name != "__Host-odt_session" || cc.Name != "__Host-odt_csrf" || sc.Domain != "" || cc.Domain != "" {
		t.Fatalf("a Secure server must use host-only __Host- cookies: %s %q, %s %q", sc.Name, sc.Domain, cc.Name, cc.Domain)
	}
	if !sc.HttpOnly || cc.HttpOnly {
		t.Fatalf("HttpOnly: session=%v csrf=%v (want true, false)", sc.HttpOnly, cc.HttpOnly)
	}
	if cc.Value != body.CSRFToken || cc.Value != auth.CSRFToken(session) || !auth.WellFormed(session) {
		t.Fatalf("csrf cookie %q, body %q, derived %q, session well formed %v", cc.Value, body.CSRFToken, auth.CSRFToken(session), auth.WellFormed(session))
	}

	// A second request with only the cookies works, as a session.
	var me tracker.Me
	e.must(200, e.do("GET", "/api/v1/me", session, nil)).json(t, &me)
	if me.Username != "chen" || me.TokenKind != "session" || me.PinnedOrgID != nil ||
		strings.Join(me.Scopes, ",") != "read,write,admin" {
		t.Fatalf("me via cookie: %+v", me)
	}
	// A write with cookie + CSRF works.
	is := e.createIssue(session, map[string]any{"title": "made in the browser"})
	if is.Creator == nil || is.Creator.Username != "chen" {
		t.Fatalf("issue creator %+v", is.Creator)
	}

	// Logout: 204, both cookies cleared, token revoked.
	lr := e.must(204, e.do("POST", "/api/v1/auth/logout", session, nil))
	cleared := lr.cookies()
	for _, name := range []string{e.names.Session, e.names.CSRF} {
		c := cleared[name]
		if c == nil || c.Value != "" || c.MaxAge >= 0 {
			t.Fatalf("logout did not clear %s: %+v", name, c)
		}
	}
	if n := e.countRows(`SELECT count(*) FROM api_tokens WHERE kind = 'session' AND revoked_at IS NOT NULL AND token_hash = $1`, auth.HashToken(session)); n != 1 {
		t.Fatalf("session not revoked (%d rows)", n)
	}
	if code := e.must(401, e.do("GET", "/api/v1/me", session, nil)).errCode(t); code != "token_revoked" {
		t.Fatalf("old cookie after logout: %s", code)
	}
}

// AC: a non-member email gets the same 202 and no code is created or sent.
func TestLogin_NonMemberGetsTheSameAnswer(t *testing.T) {
	e := newEnv(t)
	member := e.do("POST", "/api/v1/auth/otp", "", map[string]any{"email": chenEmail})
	if member.status != 202 || e.mail.count() != 1 {
		t.Fatalf("member: %d, %d sent", member.status, e.mail.count())
	}
	codesBefore := e.countRows(`SELECT count(*) FROM login_codes`)

	// Users who exist but may not log in look exactly like strangers.
	lonely := e.newUser("lonely") // no org
	mustExec(t, e, `UPDATE users SET email = 'lonely@example.com' WHERE id = $1`, lonely.ID)
	gone := e.newHuman("gone", "gone@example.com", "member")
	mustExec(t, e, `UPDATE users SET deactivated_at = now() WHERE id = $1`, gone.ID)
	mustExec(t, e, `UPDATE users SET email = 'claude@example.com' WHERE username = 'claude'`) // an agent

	for _, email := range []string{"nobody@example.com", "lonely@example.com", "gone@example.com", "claude@example.com"} {
		t.Run(email, func(t *testing.T) {
			r := e.do("POST", "/api/v1/auth/otp", "", map[string]any{"email": email})
			if r.status != member.status || string(r.body) != string(member.body) ||
				r.header.Get("Content-Type") != member.header.Get("Content-Type") {
				t.Fatalf("answer differs from a member's: %d %q vs %d %q", r.status, r.body, member.status, member.body)
			}
			if len(r.cookies()) != 0 {
				t.Fatal("otp set a cookie")
			}
		})
	}
	if n := e.countRows(`SELECT count(*) FROM login_codes`); n != codesBefore {
		t.Fatalf("a code was created for a non-member: %d -> %d rows", codesBefore, n)
	}
	if e.mail.count() != 1 {
		t.Fatalf("%d codes sent, want only the member's", e.mail.count())
	}
	// And verify cannot tell them apart either.
	for _, email := range []string{"nobody@example.com", "gone@example.com"} {
		r := e.must(401, e.do("POST", "/api/v1/auth/otp/verify", "", map[string]any{"email": email, "code": "123456"}))
		if r.errCode(t) != "invalid_code" {
			t.Fatalf("verify %s: %s", email, r.body)
		}
	}
}

// AC: five wrong codes lock that code; a later right code for the same row
// fails. A new code works again.
func TestLogin_FiveWrongTriesLockTheCode(t *testing.T) {
	e := newEnv(t)
	code := e.requestCode(chenEmail)
	for i := range 5 {
		r := e.must(401, e.do("POST", "/api/v1/auth/otp/verify", "", map[string]any{"email": chenEmail, "code": wrongCode(code)}))
		if r.errCode(t) != "invalid_code" {
			t.Fatalf("try %d: %s", i+1, r.body)
		}
	}
	r := e.must(401, e.do("POST", "/api/v1/auth/otp/verify", "", map[string]any{"email": chenEmail, "code": code}))
	if r.errCode(t) != "invalid_code" || len(r.cookies()) != 0 {
		t.Fatalf("right code after lock: %s", r.body)
	}
	if n := e.countRows(`SELECT count(*) FROM login_codes WHERE attempts = 5 AND consumed_at IS NULL`); n != 1 {
		t.Fatalf("locked rows = %d", n)
	}
	if n := e.countRows(`SELECT count(*) FROM api_tokens WHERE kind = 'session'`); n != 0 {
		t.Fatalf("%d sessions minted", n)
	}
	// A new code replaces the locked one and works.
	fresh := e.requestCode(chenEmail)
	e.must(200, e.do("POST", "/api/v1/auth/otp/verify", "", map[string]any{"email": chenEmail, "code": fresh}))
}

// Concurrent wrong guesses are counted exactly (the row is locked), so a
// burst cannot get more than five tries at one code.
func TestLogin_ConcurrentGuessesCountExactly(t *testing.T) {
	e := newEnv(t)
	code := e.requestCode(chenEmail)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			_, _ = e.request("POST", "/api/v1/auth/otp/verify", "", map[string]any{"email": chenEmail, "code": wrongCode(code)})
		})
	}
	wg.Wait()
	// Exactly 5: the first five count, the rest find the code locked.
	if n := e.countRows(`SELECT attempts FROM login_codes WHERE consumed_at IS NULL`); n != 5 {
		t.Fatalf("attempts = %d after 12 wrong guesses, want 5", n)
	}
	e.must(401, e.do("POST", "/api/v1/auth/otp/verify", "", map[string]any{"email": chenEmail, "code": code}))
}

// AC: a sixth code request within the hour is refused (429), for a member
// and a non-member alike; after the hour it works again. The cap also holds
// across a restart (the in-memory limiter is gone; the database one is not).
func TestLogin_SixthRequestInAnHourIsRefused(t *testing.T) {
	clk := newClock()
	e := newEnvWith(t, envOpts{login: login.Options{Now: clk.Now}})
	for _, email := range []string{chenEmail, "stranger@example.com"} {
		for i := range 5 {
			e.must(202, e.do("POST", "/api/v1/auth/otp", "", map[string]any{"email": email}))
			_ = i
		}
		r := e.must(429, e.do("POST", "/api/v1/auth/otp", "", map[string]any{"email": email}))
		if r.errCode(t) != "rate_limited" {
			t.Fatalf("%s: %s", email, r.body)
		}
		// All five fell at the same instant, so the first leaves the
		// window in exactly one hour.
		if ra := r.header.Get("Retry-After"); ra != "3600" {
			t.Fatalf("%s: Retry-After %q, want 3600", email, ra)
		}
	}
	if e.mail.count() != 5 {
		t.Fatalf("%d codes sent, want 5", e.mail.count())
	}
	// Any case of the address is the same address.
	e.must(429, e.do("POST", "/api/v1/auth/otp", "", map[string]any{"email": "CHEN@example.com"}))

	// "Restart": a new service on the same database, empty limiter.
	restarted := login.New(e.pool, slog.New(slog.DiscardHandler), e.mail.deliver, login.Options{Now: clk.Now, Pepper: testPepper})
	if err := restarted.RequestCode(context.Background(), chenEmail); err != nil {
		t.Fatalf("after restart: %v", err) // 202 to the caller
	}
	if e.mail.count() != 5 || e.countRows(`SELECT count(*) FROM login_codes`) != 5 {
		t.Fatalf("the durable cap let a sixth code out: %d sent", e.mail.count())
	}

	clk.Advance(61 * time.Minute)
	if c := e.requestCode(chenEmail); c == "" {
		t.Fatal("no code an hour later")
	}
}

// A code is single use, expires after 10 minutes, and is replaced by a
// newer one.
func TestLogin_CodeIsSingleUseExpiresAndIsReplaced(t *testing.T) {
	clk := newClock()
	e := newEnvWith(t, envOpts{login: login.Options{Now: clk.Now}})
	verify := func(code string) resp {
		return e.do("POST", "/api/v1/auth/otp/verify", "", map[string]any{"email": chenEmail, "code": code})
	}
	c1 := e.requestCode(chenEmail)
	e.must(200, verify(c1))
	if code := e.must(401, verify(c1)).errCode(t); code != "invalid_code" {
		t.Fatalf("reuse: %s", code)
	}

	c2 := e.requestCode(chenEmail)
	c3 := e.requestCode(chenEmail)
	if c2 != c3 {
		e.must(401, verify(c2)) // replaced
	}
	clk.Advance(10*time.Minute + time.Second)
	e.must(401, verify(c3)) // expired

	c4 := e.requestCode(chenEmail)
	clk.Advance(9 * time.Minute)
	e.must(200, verify(c4))
}

// Input rules of the public routes: JSON only (a cross-site "simple"
// request cannot reach them), a bad email or code is 400, and a session
// cookie is no way around the code.
func TestLogin_PublicRouteInputs(t *testing.T) {
	e := newEnv(t)
	session, _ := e.loginAs(chenEmail)
	post := func(path, ct, body string, cookies bool) resp {
		t.Helper()
		req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(body))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		if cookies {
			e.authorize(req, session)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		b, _ := io.ReadAll(res.Body)
		return resp{status: res.StatusCode, body: b, header: res.Header}
	}
	cases := []struct {
		name, path, ct, body string
		cookies              bool
		status               int
		code                 string
	}{
		{"otp text/plain", "/api/v1/auth/otp", "text/plain", `{"email":"chen@example.com"}`, false, 415, "unsupported_media_type"},
		{"otp no content type", "/api/v1/auth/otp", "", `{"email":"chen@example.com"}`, false, 415, "unsupported_media_type"},
		{"verify form", "/api/v1/auth/otp/verify", "application/x-www-form-urlencoded", `email=x&code=1`, false, 415, "unsupported_media_type"},
		{"otp bad email", "/api/v1/auth/otp", "application/json", `{"email":"not an email"}`, false, 400, "validation_failed"},
		{"otp unknown field", "/api/v1/auth/otp", "application/json", `{"email":"a@b.co","x":1}`, false, 400, "bad_request"},
		{"verify short code", "/api/v1/auth/otp/verify", "application/json", `{"email":"chen@example.com","code":"123"}`, false, 400, "validation_failed"},
		{"verify with a session cookie still needs the code", "/api/v1/auth/otp/verify", "application/json", `{"email":"chen@example.com","code":"000000"}`, true, 401, "invalid_code"},
		{"verify with a session cookie and no code", "/api/v1/auth/otp/verify", "application/json", `{"email":"chen@example.com"}`, true, 400, "validation_failed"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r := post(tt.path, tt.ct, tt.body, tt.cookies)
			if r.status != tt.status || r.errCode(t) != tt.code {
				t.Fatalf("%d %s, want %d %s", r.status, r.body, tt.status, tt.code)
			}
		})
	}
	// Idempotency-Key is refused on the login routes (the response is a secret).
	r := e.do("POST", "/api/v1/auth/otp/verify", "", map[string]any{"email": chenEmail, "code": "000000"}, "Idempotency-Key", "k")
	if r.status != 400 {
		t.Fatalf("Idempotency-Key on verify: %d", r.status)
	}
}

// AC: a cookie request without the CSRF header, or with a wrong one, is
// 403; a bearer request is not affected.
func TestCSRF_CookieWritesNeedTheHeader(t *testing.T) {
	e := newEnv(t)
	session, _ := e.loginAs(chenEmail)
	is := e.createIssue(e.chen, map[string]any{"title": "target"})
	csrf := auth.CSRFToken(session)

	writes := []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/v1/orgs/orbit/issues", map[string]any{"team": "ORB", "title": "forged"}},
		{"PATCH", "/api/v1/orgs/orbit/issues/" + is.Identifier, map[string]any{"title": "forged"}},
		{"POST", "/api/v1/orgs/orbit/issues/" + is.Identifier + "/comments", map[string]any{"body": "forged"}},
		{"DELETE", "/api/v1/orgs/orbit/tokens/00000000-0000-7000-8000-000000000000", nil},
		{"POST", "/api/v1/auth/logout", nil},
	}
	forgeries := []struct {
		name   string
		header []string // explicit headers applied after the cookies
		cookie *http.Cookie
	}{
		{"no header", []string{auth.CSRFHeader, ""}, nil},
		{"wrong header", []string{auth.CSRFHeader, "nope"}, nil},
		{"header is the session token", []string{auth.CSRFHeader, session}, nil},
		// Double submit alone would pass this: header == cookie. The cookie is
		// not this session's token, so it fails (a planted cookie is useless).
		{"planted csrf cookie that matches the header", []string{auth.CSRFHeader, "planted"}, &http.Cookie{Name: e.names.CSRF, Value: "planted"}},
	}
	for _, w := range writes {
		for _, f := range forgeries {
			t.Run(w.method+" "+w.path+" "+f.name, func(t *testing.T) {
				var body io.Reader
				if w.body != nil {
					b, _ := json.Marshal(w.body)
					body = strings.NewReader(string(b))
				}
				req, _ := http.NewRequest(w.method, e.srv.URL+w.path, body)
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(&http.Cookie{Name: e.names.Session, Value: session})
				if f.cookie != nil {
					req.AddCookie(f.cookie)
				} else {
					req.AddCookie(&http.Cookie{Name: e.names.CSRF, Value: csrf})
				}
				req.Header.Set(f.header[0], f.header[1])
				res, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = res.Body.Close() }()
				b, _ := io.ReadAll(res.Body)
				r := resp{status: res.StatusCode, body: b, header: res.Header}
				if r.status != 403 || r.errCode(t) != "csrf_failed" {
					t.Fatalf("%d %s, want 403 csrf_failed", r.status, r.body)
				}
			})
		}
	}
	// Nothing changed.
	var got tracker.Issue
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/issues/"+is.Identifier, e.chen, nil)).json(t, &got)
	if got.Title != "target" || e.countRows(`SELECT count(*) FROM issues`) != 1 {
		t.Fatalf("a forged write landed: %+v", got)
	}
	// The session survived the forged logouts.
	e.must(200, e.do("GET", "/api/v1/me", session, nil))

	// Reads need no header.
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/issues", session, nil, auth.CSRFHeader, ""))
	// With the header a cookie write works.
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues", session, map[string]any{"team": "ORB", "title": "real"}))

	// Bearer: no header needed, even with a stale session cookie alongside;
	// the bearer token wins.
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues", e.claude, map[string]any{"team": "ORB", "title": "bearer"}))
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/v1/orgs/orbit/issues", strings.NewReader(`{"team":"ORB","title":"bearer wins"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.claude)
	req.AddCookie(&http.Cookie{Name: e.names.Session, Value: session})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var created tracker.Issue
	_ = json.NewDecoder(res.Body).Decode(&created)
	_ = res.Body.Close()
	if res.StatusCode != 201 || created.Creator == nil || created.Creator.Username != "claude" {
		t.Fatalf("bearer + cookie: %d, creator %+v (want claude's bearer to win)", res.StatusCode, created.Creator)
	}

	// A web session value is no bearer token (LAUN-129; a native login token
	// is, see login_native_integration_test.go). A PAT is not a session cookie.
	req, _ = http.NewRequest("GET", e.srv.URL+"/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+session)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("web session as bearer: %d, want 401", res.StatusCode)
	}
	pat := e.asSession(e.claude) // send the PAT as a cookie
	if code := e.must(401, e.do("GET", "/api/v1/me", pat, nil)).errCode(t); code != "invalid_token" {
		t.Fatalf("PAT as cookie: %s", code)
	}
}

// AC: a token cannot mint a token that outlives itself; an agent token
// without expiresInDays is 400; a cookie session mints only with an expiry
// and without the admin scope (decisions-T11).
func TestTokens_LifetimeRules(t *testing.T) {
	e := newEnv(t)
	chen := e.user("chen")
	session, _ := e.loginAs(chenEmail)
	mint := func(token string, body map[string]any) resp {
		return e.do("POST", "/api/v1/orgs/orbit/tokens", token, body)
	}

	t.Run("a session mints only with an expiry and no admin scope", func(t *testing.T) {
		// decisions-T11 supersedes T6a #17; the full set is in
		// tokens_session_integration_test.go.
		e.must(400, mint(session, map[string]any{"name": "x", "scopes": []string{"read"}}))
		e.must(403, mint(session, map[string]any{"name": "x", "scopes": []string{"admin"}, "expiresInDays": 30}))
		if n := e.countRows(`SELECT count(*) FROM api_tokens WHERE created_by = $1 AND kind = 'pat'`, chen.ID); n != 0 {
			t.Fatalf("a refused session request minted %d tokens", n)
		}
		e.must(200, e.do("GET", "/api/v1/orgs/orbit/tokens", session, nil))
	})
	t.Run("agent tokens must expire", func(t *testing.T) {
		for _, days := range []any{nil, 0, 366, 3650} {
			body := map[string]any{"name": "bot", "scopes": []string{"read"}, "user": "claude"}
			if days != nil {
				body["expiresInDays"] = days
			}
			if code := e.must(400, mint(e.chen, body)).errCode(t); code != "validation_failed" {
				t.Fatalf("days %v: %s", days, code)
			}
		}
		var ct tracker.CreatedToken
		e.must(201, mint(e.chen, map[string]any{"name": "bot", "scopes": []string{"read"}, "user": "claude", "expiresInDays": 365})).json(t, &ct)
		if ct.Token.ExpiresAt == nil || time.Until(*ct.Token.ExpiresAt) < 364*24*time.Hour {
			t.Fatalf("agent expiry %v", ct.Token.ExpiresAt)
		}
		// Human PATs keep the optional expiry.
		e.must(201, mint(e.chen, map[string]any{"name": "me", "scopes": []string{"read"}}))
		e.must(201, mint(e.chen, map[string]any{"name": "me", "scopes": []string{"read"}, "expiresInDays": 3650}))
	})
	t.Run("never outlives the minting token", func(t *testing.T) {
		soon := time.Now().Add(48 * time.Hour).Truncate(time.Microsecond)
		short, _ := e.newToken(chen.ID, tokenOpts{pin: &e.orgID, expires: &soon})
		for _, body := range []map[string]any{
			{"name": "no expiry asked", "scopes": []string{"read"}},
			{"name": "longer asked", "scopes": []string{"read"}, "expiresInDays": 30},
			{"name": "agent longer", "scopes": []string{"read"}, "user": "claude", "expiresInDays": 365},
		} {
			var ct tracker.CreatedToken
			e.must(201, mint(short, body)).json(t, &ct)
			if ct.Token.ExpiresAt == nil || !ct.Token.ExpiresAt.Equal(soon) {
				t.Fatalf("%s: expiresAt %v, want the caller's %v", body["name"], ct.Token.ExpiresAt, soon)
			}
			var stored time.Time
			if err := e.pool.QueryRow(context.Background(), `SELECT expires_at FROM api_tokens WHERE id = $1`, ct.Token.ID).Scan(&stored); err != nil || !stored.Equal(soon) {
				t.Fatalf("stored expires_at %v %v", stored, err)
			}
		}
		// A shorter request is kept.
		var ct tracker.CreatedToken
		e.must(201, mint(short, map[string]any{"name": "shorter", "scopes": []string{"read"}, "expiresInDays": 1})).json(t, &ct)
		if ct.Token.ExpiresAt == nil || !ct.Token.ExpiresAt.Before(soon) {
			t.Fatalf("shorter expiry not kept: %v", ct.Token.ExpiresAt)
		}
		// A caller with no expiry mints a token with none.
		e.must(201, mint(e.chen, map[string]any{"name": "forever", "scopes": []string{"read"}})).json(t, &ct)
		if ct.Token.ExpiresAt != nil {
			t.Fatalf("unexpected expiry %v", ct.Token.ExpiresAt)
		}
	})
}

// A session carries read and write, plus admin only for an org admin; it
// goes through RequireOrgMember and RequireScope like a PAT. Member emails
// are visible only to an org admin (and each person sees their own).
func TestSession_ScopesMembershipAndEmails(t *testing.T) {
	e := newEnv(t)
	e.newHuman("dana", "dana@example.com", "member")
	acmeID := e.newOrg("acme", "ACM")
	_ = acmeID
	dana, r := e.loginAs("dana@example.com")
	var body login.Response
	r.json(t, &body)
	if len(body.Orgs) != 1 || body.Orgs[0].Role != "member" {
		t.Fatalf("dana's orgs %+v", body.Orgs)
	}
	var me tracker.Me
	e.must(200, e.do("GET", "/api/v1/me", dana, nil)).json(t, &me)
	if strings.Join(me.Scopes, ",") != "read,write" {
		t.Fatalf("member session scopes %v", me.Scopes)
	}
	if code := e.must(403, e.do("GET", "/api/v1/orgs/orbit/tokens", dana, nil)).errCode(t); code != "insufficient_scope" {
		t.Fatalf("member session on tokens: %s", code)
	}
	e.must(403, e.do("POST", "/api/v1/orgs/orbit/teams", dana, map[string]any{"key": "NO", "name": "no"}))
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues", dana, map[string]any{"team": "ORB", "title": "dana's"}))
	// Cross-org with a cookie: an org the session's user is not in is 404,
	// for reads and writes.
	for _, m := range []struct{ method, path string }{
		{"GET", "/api/v1/orgs/acme/issues"}, {"POST", "/api/v1/orgs/acme/issues"}, {"GET", "/api/v1/orgs/acme/events/stream"},
	} {
		r := e.do(m.method, m.path, dana, map[string]any{"team": "ACM", "title": "x"})
		if r.status != 404 || r.errCode(t) != "not_found" {
			t.Fatalf("%s %s: %d %s", m.method, m.path, r.status, r.body)
		}
	}
	var orgs page[tracker.Org]
	e.must(200, e.do("GET", "/api/v1/orgs", dana, nil)).json(t, &orgs)
	if len(orgs.Items) != 1 || orgs.Items[0].Slug != "orbit" {
		t.Fatalf("dana's orgs %v", orgs.Items)
	}

	emails := func(token string) map[string]string {
		var p page[tracker.Member]
		e.must(200, e.do("GET", "/api/v1/orgs/orbit/users", token, nil)).json(t, &p)
		out := map[string]string{}
		for _, m := range p.Items {
			if m.Email != nil {
				out[m.Username] = *m.Email
			}
		}
		return out
	}
	if got := emails(dana); len(got) != 1 || got["dana"] != "dana@example.com" {
		t.Fatalf("a member sees emails %v (want only their own)", got)
	}
	if got := emails(e.claude); len(got) != 0 {
		t.Fatalf("the agent sees emails %v", got)
	}
	if got := emails(e.chen); got["chen"] != chenEmail || got["dana"] != "dana@example.com" {
		t.Fatalf("the admin sees %v", got)
	}
}

// AC: SSE works with a cookie session and ends when the session is revoked
// (logout).
func TestEventStream_CookieSession(t *testing.T) {
	e := newEnv(t)
	session, _ := e.loginAs(chenEmail)
	br, closeFn := openStream(t, e, "orbit", session, "", 10*time.Second)
	defer closeFn()

	is := e.createIssue(e.claude, map[string]any{"title": "seen by the browser"})
	_, typ, data, err := nextEvent(br)
	if err != nil || typ != "issue.created" || !strings.Contains(data, is.ID.String()) {
		t.Fatalf("event %s %s %v", typ, data, err)
	}

	e.must(204, e.do("POST", "/api/v1/auth/logout", session, nil))
	start := time.Now()
	rest, err := io.ReadAll(br)
	if err != nil || time.Since(start) > 5*time.Second {
		t.Fatalf("stream did not end after logout: err=%v after %v", err, time.Since(start))
	}
	if !strings.Contains(string(rest), ": credentials no longer valid") {
		t.Fatalf("stream tail %q", rest)
	}
}

// AC: no code, no secret and no cookie value in any log line, event
// payload or idempotency row. This runs the real Postmark path (a fake
// Postmark server, the real queue and mailer) with every log level on.
func TestLogin_NoSecretsInLogsEventsOrIdempotency(t *testing.T) {
	var mu sync.Mutex
	var mails []string
	pm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		mails = append(mails, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ErrorCode":0,"Message":"OK"}`))
	}))
	defer pm.Close()

	e := newEnvWith(t, envOpts{logLevel: slog.LevelDebug, deliver: func(e *env) login.Deliver {
		m := login.NewPostmarkMailer("pm-server-token", "tracker@example.com", "")
		m.Endpoint = pm.URL
		q := login.NewQueue(m, slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})), 8)
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Go(func() { q.Run(ctx) })
		t.Cleanup(func() { cancel(); wg.Wait() })
		return q.Deliver
	}})

	e.must(202, e.do("POST", "/api/v1/auth/otp", "", map[string]any{"email": chenEmail}))
	var code string
	deadline := time.Now().Add(5 * time.Second)
	for code == "" && time.Now().Before(deadline) {
		mu.Lock()
		if len(mails) > 0 {
			var m struct{ TextBody string }
			_ = json.Unmarshal([]byte(mails[0]), &m)
			if i := strings.Index(m.TextBody, "code is "); i >= 0 {
				code = m.TextBody[i+8 : i+14]
			}
		}
		mu.Unlock()
		if code == "" {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !login.ValidCodeFormat(code) {
		t.Fatalf("no code reached Postmark: %v", mails)
	}
	// One wrong try, then the right one.
	e.must(401, e.do("POST", "/api/v1/auth/otp/verify", "", map[string]any{"email": chenEmail, "code": wrongCode(code)}))
	r := e.must(200, e.do("POST", "/api/v1/auth/otp/verify", "", map[string]any{"email": chenEmail, "code": code}))
	session := e.asSession(r.cookies()[e.names.Session].Value)
	csrf := auth.CSRFToken(session)
	// Cookie writes that emit events and store an idempotency row.
	is := e.createIssue(session, map[string]any{"title": "idempotent"})
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues/"+is.Identifier+"/comments", session, map[string]any{"body": "hi"}, "Idempotency-Key", "cookie-key-1"))
	e.must(403, e.do("POST", "/api/v1/orgs/orbit/issues", session, map[string]any{"team": "ORB", "title": "x"}, auth.CSRFHeader, "bad"))
	e.must(204, e.do("POST", "/api/v1/auth/logout", session, nil))
	e.must(401, e.do("GET", "/api/v1/me", session, nil))

	// LAUN-122: a native login too. Its token and code must stay out of the
	// logs, the events and the idempotency rows, through bearer writes that
	// emit events and store a key, and through logout.
	e.must(202, e.do("POST", "/api/v1/auth/otp", "", map[string]any{"email": chenEmail}))
	var code2 string
	deadline = time.Now().Add(5 * time.Second)
	for code2 == "" && time.Now().Before(deadline) {
		mu.Lock()
		if len(mails) > 1 {
			var m struct{ TextBody string }
			_ = json.Unmarshal([]byte(mails[1]), &m)
			if i := strings.Index(m.TextBody, "code is "); i >= 0 {
				code2 = m.TextBody[i+8 : i+14]
			}
		}
		mu.Unlock()
		if code2 == "" {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !login.ValidCodeFormat(code2) {
		t.Fatalf("no second code reached Postmark: %v", mails)
	}
	// LAUN-137: a browser's native request is refused; the code must stay out
	// of the logs, and it is not spent (the next call works).
	e.must(401, e.do("POST", "/api/v1/auth/otp/verify", "", map[string]any{"email": chenEmail, "code": code2, "client": "native"}, "Origin", "https://web.example"))
	if !strings.Contains(e.logs.String(), "native login refused") {
		t.Fatal("the refusal was not logged; the check would be vacuous")
	}
	nr := e.must(200, e.do("POST", "/api/v1/auth/otp/verify", "", map[string]any{"email": chenEmail, "code": code2, "client": "native"}))
	var nbody login.NativeResponse
	nr.json(t, &nbody)
	native := nbody.Token
	if native == "" || len(nr.cookies()) != 0 {
		t.Fatalf("native login: token %q, cookies %v", native, nr.cookies())
	}
	nis := e.createIssue(native, map[string]any{"title": "native idempotent"})
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues/"+nis.Identifier+"/comments", native, map[string]any{"body": "hi"}, "Idempotency-Key", "native-key-1"))
	// LAUN-129: the web session value sent as a bearer is refused too; the
	// refused value must stay out of the logs like any other secret.
	e.must(401, e.do("GET", "/api/v1/me", "", nil, "Authorization", "Bearer "+session))
	e.must(204, e.do("POST", "/api/v1/auth/logout", native, nil))
	e.must(401, e.do("GET", "/api/v1/me", native, nil))

	secrets := map[string]string{"code": code, "session": session, "csrf": csrf, "postmark token": "pm-server-token",
		"native code": code2, "browser origin value": "https://web.example", "native token": native, "native csrf-style hash": auth.CSRFToken(native)}
	logs := e.logs.String()
	if !strings.Contains(logs, `msg=request`) || !strings.Contains(logs, "/api/v1/auth/otp/verify") {
		t.Fatalf("the log capture saw no request lines; the check would be vacuous:\n%s", logs)
	}
	for name, v := range secrets {
		if strings.Contains(logs, v) {
			t.Errorf("the %s is in the logs", name)
		}
	}
	if strings.Contains(logs, "odt_") {
		t.Error("a token-shaped secret is in the logs")
	}
	var events, idem string
	if err := e.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(type || ' ' || payload::text, E'\n'), '') FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(key || ' ' || path || ' ' || coalesce(convert_from(response_body, 'UTF8'), ''), E'\n'), '') FROM idempotency_keys`).Scan(&idem); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(events, "issue.created") || !strings.Contains(idem, "cookie-key-1") || !strings.Contains(idem, "native-key-1") {
		t.Fatalf("expected rows are missing; the check would be vacuous:\nevents %s\nidem %s", events, idem)
	}
	for name, v := range secrets {
		if strings.Contains(events, v) {
			t.Errorf("the %s is in an event payload", name)
		}
		if strings.Contains(idem, v) {
			t.Errorf("the %s is in an idempotency row", name)
		}
	}
	// The code is stored only as a salted hash.
	if n := e.countRows(`SELECT count(*) FROM login_codes WHERE position(convert_to($1, 'UTF8') in code_hash) > 0 OR position(convert_to($1, 'UTF8') in code_salt) > 0`, code); n != 0 {
		t.Error("the code is in login_codes in plaintext")
	}
}

// With no transport the public routes answer 503, and the rest of the API
// is unaffected.
func TestLogin_OffWithoutTransport(t *testing.T) {
	e := newEnvWith(t, envOpts{deliver: func(*env) login.Deliver { return nil }})
	for _, path := range []string{"/api/v1/auth/otp", "/api/v1/auth/otp/verify"} {
		r := e.do("POST", path, "", map[string]any{"email": chenEmail, "code": "123456"})
		if r.status != 503 || r.errCode(t) != "login_unavailable" {
			t.Fatalf("%s: %d %s", path, r.status, r.body)
		}
	}
	if n := e.countRows(`SELECT count(*) FROM login_codes`); n != 0 {
		t.Fatalf("%d codes written with login off", n)
	}
	e.must(200, e.do("GET", "/api/v1/me", e.chen, nil))
}

// The coarse route limit also answers 429 rate_limited with Retry-After.
func TestLogin_RouteLimitHasRetryAfter(t *testing.T) {
	e := newEnvWith(t, envOpts{login: login.Options{
		RequestsPerMinute: 1, RequestBurst: 1, VerifiesPerMinute: 2, VerifyBurst: 1}})
	e.must(202, e.do("POST", "/api/v1/auth/otp", "", map[string]any{"email": "a@example.com"}))
	r := e.must(429, e.do("POST", "/api/v1/auth/otp", "", map[string]any{"email": "b@example.com"}))
	if r.errCode(t) != "rate_limited" || r.header.Get("Retry-After") == "" {
		t.Fatalf("otp: %s %v", r.body, r.header)
	}
	if n, err := strconv.Atoi(r.header.Get("Retry-After")); err != nil || n < 1 || n > 60 { // 1 a minute
		t.Fatalf("otp Retry-After %q", r.header.Get("Retry-After"))
	}
	e.must(401, e.do("POST", "/api/v1/auth/otp/verify", "", map[string]any{"email": "a@example.com", "code": "123456"}))
	r = e.must(429, e.do("POST", "/api/v1/auth/otp/verify", "", map[string]any{"email": "a@example.com", "code": "123456"}))
	if r.errCode(t) != "rate_limited" || r.header.Get("Retry-After") == "" {
		t.Fatalf("verify: %s %v", r.body, r.header)
	}
}

// GET /auth/session rebuilds the web app's state after a reload: the login
// body for the current credential, with nothing rotated.
func TestAuthSession(t *testing.T) {
	e := newEnv(t)
	session, lr := e.loginAs(chenEmail)
	var login1 login.Response
	lr.json(t, &login1)

	r := e.must(200, e.do("GET", "/api/v1/auth/session", session, nil))
	var got login.Response
	r.json(t, &got)
	if got.User != login1.User || len(got.Orgs) != 1 || got.Orgs[0] != login1.Orgs[0] || got.CSRFToken != login1.CSRFToken {
		t.Fatalf("session %+v\nlogin   %+v", got, login1)
	}
	if len(r.cookies()) != 0 {
		t.Fatal("GET /auth/session set a cookie")
	}
	// A planted CSRF cookie is not echoed back.
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/v1/auth/session", nil)
	req.AddCookie(&http.Cookie{Name: e.names.Session, Value: session})
	req.AddCookie(&http.Cookie{Name: e.names.CSRF, Value: "planted"})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var planted login.Response
	_ = json.NewDecoder(res.Body).Decode(&planted)
	_ = res.Body.Close()
	if res.StatusCode != 200 || planted.CSRFToken != auth.CSRFToken(session) {
		t.Fatalf("planted cookie: %d csrf %q", res.StatusCode, planted.CSRFToken)
	}

	// Bearer works, with an empty csrfToken; a pinned token sees its org only.
	chen := e.user("chen")
	e.addMember(e.newOrg("acme", "ACM"), chen.ID, "member")
	e.must(200, e.do("GET", "/api/v1/auth/session", e.chen, nil)).json(t, &got)
	if got.CSRFToken != "" || got.User.Email != chenEmail || len(got.Orgs) != 1 || got.Orgs[0].Slug != "orbit" {
		t.Fatalf("pinned bearer: %+v", got)
	}
	unpinned, _ := e.newToken(chen.ID, tokenOpts{})
	e.must(200, e.do("GET", "/api/v1/auth/session", unpinned, nil)).json(t, &got)
	if len(got.Orgs) != 2 || got.CSRFToken != "" {
		t.Fatalf("unpinned bearer: %+v", got)
	}
	e.must(200, e.do("GET", "/api/v1/auth/session", session, nil)).json(t, &got)
	if len(got.Orgs) != 2 {
		t.Fatalf("the session sees %d orgs, want 2", len(got.Orgs))
	}

	// No credential, or a dead session: 401.
	e.must(401, e.do("GET", "/api/v1/auth/session", "", nil))
	e.must(204, e.do("POST", "/api/v1/auth/logout", session, nil))
	if code := e.must(401, e.do("GET", "/api/v1/auth/session", session, nil)).errCode(t); code != "token_revoked" {
		t.Fatalf("after logout: %s", code)
	}
}

// GET /me carries displayName and the caller's own email; name stays.
func TestMe_DisplayNameAndEmail(t *testing.T) {
	e := newEnv(t)
	session, _ := e.loginAs(chenEmail)
	for _, tok := range []string{e.chen, session} {
		var me tracker.Me
		e.must(200, e.do("GET", "/api/v1/me", tok, nil)).json(t, &me)
		if me.DisplayName != "Chen" || me.Name != "Chen" || me.Email == nil || *me.Email != chenEmail {
			t.Fatalf("me %+v", me)
		}
	}
	var me tracker.Me
	e.must(200, e.do("GET", "/api/v1/me", e.claude, nil)).json(t, &me)
	if me.DisplayName != "Claude" || me.Email != nil {
		t.Fatalf("claude me %+v", me)
	}
	// The raw JSON has the keys even when email is null.
	r := e.must(200, e.do("GET", "/api/v1/me", e.claude, nil))
	if !strings.Contains(string(r.body), `"email":null`) || !strings.Contains(string(r.body), `"displayName":"Claude"`) {
		t.Fatalf("me body %s", r.body)
	}
}

// A LOG_ENV=local server (no Secure) uses the bare names; the browser would
// refuse a __Host- cookie without Secure.
func TestLogin_LocalServerUsesBareCookieNames(t *testing.T) {
	e := newEnvWith(t, envOpts{local: true})
	session, r := e.loginAs(chenEmail)
	cs := r.cookies()
	for _, name := range []string{"odt_session", "odt_csrf"} {
		c := cs[name]
		if c == nil || c.Secure {
			t.Fatalf("local cookie %s: %+v", name, c)
		}
	}
	if _, ok := cs["__Host-odt_session"]; ok {
		t.Fatal("a local server set a __Host- cookie")
	}
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues", session, map[string]any{"team": "ORB", "title": "local"}))
	// The __Host- name means nothing here.
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/v1/me", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-odt_session", Value: session})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatalf("__Host- cookie on a local server: %d", res.StatusCode)
	}
}

// The CSRF check runs before the token lookup: a revoked session's cookie
// with no CSRF header is 403 csrf_failed, not 401.
func TestCSRF_CheckedBeforeTheTokenLookup(t *testing.T) {
	e := newEnv(t)
	session, _ := e.loginAs(chenEmail)
	e.must(204, e.do("POST", "/api/v1/auth/logout", session, nil))
	body := map[string]any{"team": "ORB", "title": "x"}
	if code := e.must(403, e.do("POST", "/api/v1/orgs/orbit/issues", session, body, auth.CSRFHeader, "")).errCode(t); code != "csrf_failed" {
		t.Fatalf("revoked session without CSRF: %s", code)
	}
	if code := e.must(401, e.do("POST", "/api/v1/orgs/orbit/issues", session, body)).errCode(t); code != "token_revoked" {
		t.Fatalf("revoked session with CSRF: %s", code)
	}
}

// The stored code is an HMAC with the server pepper: the salt and hash a
// database reader sees do not let them brute-force the code.
func TestLogin_StoredCodeNeedsThePepper(t *testing.T) {
	e := newEnv(t)
	code := e.requestCode(chenEmail)
	var salt, hash []byte
	if err := e.pool.QueryRow(context.Background(), `SELECT code_salt, code_hash FROM login_codes WHERE consumed_at IS NULL`).Scan(&salt, &hash); err != nil {
		t.Fatal(err)
	}
	m := hmac.New(sha256.New, testPepper)
	m.Write(salt)
	m.Write([]byte(code))
	if !hmac.Equal(m.Sum(nil), hash) {
		t.Fatal("code_hash is not HMAC-SHA256(pepper, salt || code)")
	}
	plain := sha256.Sum256(append(append([]byte{}, salt...), code...))
	if hmac.Equal(plain[:], hash) {
		t.Fatal("code_hash is a plain sha256 a DB reader could brute-force")
	}
}
