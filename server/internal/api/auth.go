package api

import (
	"errors"
	"mime"
	"net/http"
	"strconv"
	"time"

	"devops/internal/auth"
	"devops/internal/httpx"
	"devops/internal/login"
)

// The /auth routes (T6a): email one-time-code login and the web
// session cookies. POST /auth/otp and /auth/otp/verify are public and never
// read a cookie; POST /auth/logout sits behind Authenticate, so it needs the
// session cookie AND the CSRF header like any other cookie write.

var errLoginUnavailable = httpx.Errorf(http.StatusServiceUnavailable, httpx.CodeLoginUnavailable,
	"login is not available on this server")

// requireJSON insists on Content-Type application/json. A cross-site page
// can send a "simple" request (form or text/plain, or no type at all)
// without a CORS preflight; JSON forces the preflight, which an origin
// outside WEB_ORIGINS fails. That closes login CSRF on the public routes.
func requireJSON(r *http.Request) error {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return httpx.Errorf(http.StatusUnsupportedMediaType, httpx.CodeUnsupportedMediaType, "content type must be application/json")
	}
	return nil
}

// failLogin is s.fail plus Retry-After (whole seconds) on a 429.
func (s *Server) failLogin(w http.ResponseWriter, r *http.Request, err error) {
	var rl *login.RateLimitedError
	if errors.As(err, &rl) {
		w.Header().Set("Retry-After", strconv.Itoa(rl.RetryAfterSeconds()))
	}
	s.fail(w, r, err)
}

// requestLoginCode: POST /auth/otp. Always 202 {} unless the request itself
// is bad or rate-limited, so the answer never says whether the email can
// log in. Every answer is padded to the same floor.
func (s *Server) requestLoginCode(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if s.Login == nil || !s.Login.Enabled() {
		s.fail(w, r, errLoginUnavailable)
		return
	}
	err := requireJSON(r)
	var in login.CodeRequest
	if err == nil {
		err = httpx.DecodeJSON(w, r, &in)
	}
	if err == nil {
		err = s.Login.RequestCode(r.Context(), in.Email)
	}
	s.Login.Pad(r.Context(), start)
	if err != nil {
		s.failLogin(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, struct{}{})
}

// fromBrowser reports whether the request carries a header that only a
// browser sets (LAUN-137): Origin (every browser sends it on a POST), or
// Sec-Fetch-Site / Sec-Fetch-Mode (forbidden header names: a script cannot
// set, change or remove any of the three). Presence counts, whatever the
// value, also "null" and the empty value. A native app is not a browser and
// sends none of them.
func fromBrowser(r *http.Request) bool { return browserHeader(r) != "" }

// browserHeader returns the NAME (a fixed word, never the value) of the first
// browser-only header the request carries, or "".
func browserHeader(r *http.Request) string {
	for _, h := range []string{"Origin", "Sec-Fetch-Site", "Sec-Fetch-Mode"} {
		if _, ok := r.Header[h]; ok {
			return h
		}
	}
	return ""
}

// verifyLoginCode: POST /auth/otp/verify. On success it sets the session
// and CSRF cookies and returns the person, their orgs and the CSRF token.
// With client "native" (LAUN-122) it sets no cookie and returns the session
// token in the body instead.
func (s *Server) verifyLoginCode(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if s.Login == nil || !s.Login.Enabled() {
		s.fail(w, r, errLoginUnavailable)
		return
	}
	err := requireJSON(r)
	var in login.VerifyRequest
	if err == nil {
		err = httpx.DecodeJSON(w, r, &in)
	}
	native := in.Client == login.ClientNative
	if err == nil && in.Client != "" && in.Client != login.ClientWeb && !native {
		err = httpx.Errorf(http.StatusBadRequest, httpx.CodeValidationFailed, "client must be \"web\" or \"native\"")
	}
	var sess login.Session
	if h := browserHeader(r); err == nil && native && h != "" {
		// LAUN-137: a browser (a script on the web origin) may not take the
		// native token. The same plain 401 a wrong code gets, with no hint,
		// answered BEFORE the code is looked at: no try, no code and no
		// limiter token is spent, so it cannot burn the person's code.
		// LAUN-145: log the refusal, so an operator can find a native client
		// that sends a browser header. Fixed words and the header NAME only:
		// no email, code, token or header value, no IP, no User-Agent. The
		// answer is not changed.
		s.log.WarnContext(r.Context(), "native login refused", "reason", "browser_header", "header", h)
		err = login.InvalidCodeError()
	}
	if err == nil {
		sess, err = s.Login.Verify(r.Context(), in.Email, in.Code, native)
	}
	s.Login.Pad(r.Context(), start)
	if err != nil {
		s.failLogin(w, r, err)
		return
	}
	if native {
		// LAUN-122: the token is in the body only. No cookie, no CSRF value.
		w.Header().Set("Cache-Control", "no-store")
		httpx.WriteJSON(w, http.StatusOK, login.NativeResponse{
			User: sess.Body.User, Orgs: sess.Body.Orgs, Token: sess.Secret, ExpiresAt: sess.ExpiresAt})
		return
	}
	n := s.cookieNames()
	http.SetCookie(w, s.sessionCookie(n.Session, sess.Secret, true, sess.ExpiresAt))
	http.SetCookie(w, s.sessionCookie(n.CSRF, sess.CSRF, false, sess.ExpiresAt))
	httpx.WriteJSON(w, http.StatusOK, sess.Body)
}

// logout: POST /auth/logout. Revokes the session and clears both cookies.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if s.Login == nil {
		s.fail(w, r, errLoginUnavailable)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	if err := s.Login.Logout(r.Context(), p); err != nil {
		s.fail(w, r, err)
		return
	}
	n := s.cookieNames()
	http.SetCookie(w, s.clearedCookie(n.Session, true))
	http.SetCookie(w, s.clearedCookie(n.CSRF, false))
	w.WriteHeader(http.StatusNoContent)
}

// cookieNames are the session cookie names for this server: the __Host-
// names whenever cookies are Secure, the bare ones only on a local server.
func (s *Server) cookieNames() auth.CookieNames { return auth.CookieNamesFor(s.SecureCookies) }

// sessionCookie is one of the two login cookies: Path=/, no Domain (both
// required by the __Host- prefix), SameSite=Lax, Secure unless a local dev
// server, expiring with the session. Only the CSRF cookie is readable by
// JavaScript.
func (s *Server) sessionCookie(name, value string, httpOnly bool, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: value, Path: "/",
		Expires: expires, MaxAge: int(time.Until(expires).Seconds()),
		HttpOnly: httpOnly, Secure: s.SecureCookies, SameSite: http.SameSiteLaxMode,
	}
}

// clearedCookie deletes a login cookie (same name, path and flags).
func (s *Server) clearedCookie(name string, httpOnly bool) *http.Cookie {
	return &http.Cookie{
		Name: name, Value: "", Path: "/", Expires: time.Unix(0, 0), MaxAge: -1,
		HttpOnly: httpOnly, Secure: s.SecureCookies, SameSite: http.SameSiteLaxMode,
	}
}

// getSession: GET /auth/session. The web app calls it after a reload to
// rebuild its state: the same body as a login, for the CURRENT session
// (nothing is rotated). csrfToken is the session's own CSRF token, which
// is the value of a valid CSRF cookie; it is derived from the session
// rather than echoed from the cookie, so a planted cookie is never
// reflected. With a bearer token the route works too, csrfToken is "".
func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	orgs, err := s.svc.ListOrgs(r.Context(), p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	body := login.Response{
		User: login.User{ID: p.UserID, Username: p.Username, Name: p.Name, IsPlatformAdmin: p.IsPlatformAdmin},
		Orgs: make([]login.Org, 0, len(orgs)),
	}
	if p.Email != nil {
		body.User.Email = *p.Email
	}
	for _, o := range orgs {
		body.Orgs = append(body.Orgs, login.Org{ID: o.ID, Slug: o.Slug, Name: o.Name, Role: o.Role})
	}
	body.CSRFToken = p.CSRFToken // "" unless the session cookie authenticated
	httpx.WriteJSON(w, http.StatusOK, body)
}
