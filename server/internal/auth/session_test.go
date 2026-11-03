package auth

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/dbq"
)

func TestCSRFToken(t *testing.T) {
	a, b := CSRFToken("odt_one"), CSRFToken("odt_two")
	if a == b || a != CSRFToken("odt_one") || len(a) != 43 {
		t.Fatalf("CSRFToken not a stable per-session value: %q %q", a, b)
	}
	if a == string(HashToken("odt_one")) {
		t.Fatal("CSRF token equals the stored token hash")
	}
}

// The cookie source, the bearer-wins rule and the double-submit check.
func TestAuthenticate_CookieAndCSRF(t *testing.T) {
	sessionUser, patUser, nativeUser := uuid.New(), uuid.New(), uuid.New()
	exp := time.Now().Add(time.Hour)
	mk := func() string { tok, _ := GenerateToken(); return tok.Plaintext }
	session, pat, native := mk(), mk(), mk()
	store := &fakeStore{tokens: map[string]dbq.GetTokenByHashRow{
		string(HashToken(session)): {ID: uuid.New(), UserID: sessionUser, Kind: KindSession, Scopes: []string{"read", "write"}, Username: "chen", ExpiresAt: &exp},
		string(HashToken(pat)):     {ID: uuid.New(), UserID: patUser, Kind: KindPAT, Scopes: []string{"read", "write"}, Username: "claude"},
		string(HashToken(native)):  {ID: uuid.New(), UserID: nativeUser, Kind: KindNative, Scopes: []string{"read", "write"}, Username: "ann", ExpiresAt: &exp},
	}}
	a := NewAuthenticator(store, slog.New(slog.DiscardHandler))
	var seen *Principal
	h := a.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = PrincipalFrom(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	csrf := CSRFToken(session)
	type req struct {
		method     string
		bearer     string
		session    string
		csrfCookie string
		csrfHeader string
	}
	tests := []struct {
		name   string
		in     req
		status int
		code   string
		user   uuid.UUID
		via    string
	}{
		{"cookie GET needs no header", req{method: "GET", session: session}, 204, "", sessionUser, "cookie"},
		{"cookie HEAD needs no header", req{method: "HEAD", session: session}, 204, "", sessionUser, "cookie"},
		{"cookie POST with the header", req{method: "POST", session: session, csrfCookie: csrf, csrfHeader: csrf}, 204, "", sessionUser, "cookie"},
		{"cookie DELETE with the header", req{method: "DELETE", session: session, csrfCookie: csrf, csrfHeader: csrf}, 204, "", sessionUser, "cookie"},
		{"cookie POST without the header", req{method: "POST", session: session, csrfCookie: csrf}, 403, "csrf_failed", uuid.Nil, ""},
		{"cookie PATCH with a wrong header", req{method: "PATCH", session: session, csrfCookie: csrf, csrfHeader: csrf + "x"}, 403, "csrf_failed", uuid.Nil, ""},
		{"cookie POST without the csrf cookie", req{method: "POST", session: session, csrfHeader: csrf}, 403, "csrf_failed", uuid.Nil, ""},
		{"cookie POST, header equals a planted cookie", req{method: "POST", session: session, csrfCookie: "planted", csrfHeader: "planted"}, 403, "csrf_failed", uuid.Nil, ""},
		{"bearer POST needs no header", req{method: "POST", bearer: pat}, 204, "", patUser, "bearer"},
		{"bearer wins over a cookie", req{method: "POST", bearer: pat, session: session}, 204, "", patUser, "bearer"},
		{"a PAT in the cookie is refused", req{method: "GET", session: pat}, 401, "invalid_token", uuid.Nil, ""},
		{"a native token as bearer works with no CSRF header (LAUN-122)", req{method: "POST", bearer: native}, 204, "", nativeUser, "bearer"},
		{"a web session value as bearer is refused, GET (LAUN-129)", req{method: "GET", bearer: session}, 401, "invalid_token", uuid.Nil, ""},
		{"a web session value as bearer is refused, POST with no CSRF header (LAUN-129)", req{method: "POST", bearer: session}, 401, "invalid_token", uuid.Nil, ""},
		{"a native token in the cookie is refused, GET (LAUN-129)", req{method: "GET", session: native}, 401, "invalid_token", uuid.Nil, ""},
		{"a native token in the cookie is refused, POST with a valid CSRF pair (LAUN-129)", req{method: "POST", session: native, csrfCookie: CSRFToken(native), csrfHeader: CSRFToken(native)}, 401, "invalid_token", uuid.Nil, ""},
		{"a native token in the cookie, POST with no CSRF header: CSRF fails first", req{method: "POST", session: native}, 403, "csrf_failed", uuid.Nil, ""},
		{"an empty cookie is no credential", req{method: "GET", session: ""}, 401, "unauthenticated", uuid.Nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seen = nil
			r := httptest.NewRequest(tt.in.method, "/", nil)
			if tt.in.bearer != "" {
				r.Header.Set("Authorization", "Bearer "+tt.in.bearer)
			}
			r.AddCookie(&http.Cookie{Name: SecureCookieNames.Session, Value: tt.in.session})
			if tt.in.csrfCookie != "" {
				r.AddCookie(&http.Cookie{Name: SecureCookieNames.CSRF, Value: tt.in.csrfCookie})
			}
			if tt.in.csrfHeader != "" {
				r.Header.Set(CSRFHeader, tt.in.csrfHeader)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != tt.status {
				t.Fatalf("status %d, want %d (%s)", rec.Code, tt.status, rec.Body)
			}
			if tt.code == "" {
				if seen == nil || seen.UserID != tt.user || seen.Via != tt.via {
					t.Fatalf("principal %+v, want user %s via %s", seen, tt.user, tt.via)
				}
				if tt.via == "cookie" && (seen.ExpiresAt == nil || !seen.ExpiresAt.Equal(exp)) {
					t.Fatalf("ExpiresAt %v", seen.ExpiresAt)
				}
				return
			}
			var body map[string]string
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if body["code"] != tt.code || body["error"] == "" {
				t.Fatalf("body %v, want code %s", body, tt.code)
			}
			if (tt.status == 401) != (rec.Header().Get("WWW-Authenticate") != "") {
				t.Fatalf("WWW-Authenticate on %d: %q", tt.status, rec.Header().Get("WWW-Authenticate"))
			}
		})
	}
}

// The CSRF check runs BEFORE the token lookup: a forged cookie write is
// 403 csrf_failed without a database call, even when the store is down or
// the session is revoked.
func TestAuthenticate_CSRFBeforeLookup(t *testing.T) {
	tok, _ := GenerateToken()
	past := time.Now().Add(-time.Minute)
	store := &fakeStore{tokens: map[string]dbq.GetTokenByHashRow{
		string(tok.Hash): {ID: uuid.New(), UserID: uuid.New(), Kind: KindSession, Scopes: []string{"read"}, RevokedAt: &past},
	}}
	a := NewAuthenticator(store, slog.New(slog.DiscardHandler))
	h := a.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	call := func(withHeader bool) (int, string) {
		r := httptest.NewRequest("POST", "/", nil)
		r.AddCookie(&http.Cookie{Name: SecureCookieNames.Session, Value: tok.Plaintext})
		r.AddCookie(&http.Cookie{Name: SecureCookieNames.CSRF, Value: CSRFToken(tok.Plaintext)})
		if withHeader {
			r.Header.Set(CSRFHeader, CSRFToken(tok.Plaintext))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body["code"]
	}
	if st, code := call(false); st != 403 || code != "csrf_failed" {
		t.Fatalf("revoked session, no header: %d %s, want 403 csrf_failed", st, code)
	}
	if st, code := call(true); st != 401 || code != "token_revoked" {
		t.Fatalf("revoked session, with header: %d %s", st, code)
	}
	store.fail = true
	if st, code := call(false); st != 403 || code != "csrf_failed" {
		t.Fatalf("store down, no header: %d %s (the lookup ran first)", st, code)
	}
}

// A Secure server reads only the __Host- names; a local one only the bare
// names. A cookie under the other name is no credential.
func TestCookieNames(t *testing.T) {
	if CookieNamesFor(true) != SecureCookieNames || CookieNamesFor(false) != LocalCookieNames ||
		SecureCookieNames.Session != "__Host-odt_session" || SecureCookieNames.CSRF != "__Host-odt_csrf" ||
		LocalCookieNames.Session != "odt_session" || LocalCookieNames.CSRF != "odt_csrf" {
		t.Fatal("cookie names changed")
	}
	tok, _ := GenerateToken()
	store := &fakeStore{tokens: map[string]dbq.GetTokenByHashRow{
		string(tok.Hash): {ID: uuid.New(), UserID: uuid.New(), Kind: KindSession, Scopes: []string{"read"}},
	}}
	get := func(a *Authenticator, n CookieNames) int {
		h := a.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
		r := httptest.NewRequest("GET", "/", nil)
		r.AddCookie(&http.Cookie{Name: n.Session, Value: tok.Plaintext})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	secure := NewAuthenticator(store, slog.New(slog.DiscardHandler))
	local := NewAuthenticator(store, slog.New(slog.DiscardHandler))
	local.UseCookieNames(LocalCookieNames)
	if get(secure, SecureCookieNames) != 204 || get(secure, LocalCookieNames) != 401 {
		t.Fatal("a Secure server must read only the __Host- cookie")
	}
	if get(local, LocalCookieNames) != 204 || get(local, SecureCookieNames) != 401 {
		t.Fatal("a local server must read only the bare cookie")
	}
}
