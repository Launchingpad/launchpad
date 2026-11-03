package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
)

// CSRFHeader must equal the CSRF cookie on every cookie-authenticated
// write (T6a).
const CSRFHeader = "X-CSRF-Token"

// CookieNames are the names of the two web-session cookies: Session holds
// the session token (HttpOnly), CSRF the CSRF token (readable by the web
// app, which echoes it in CSRFHeader).
type CookieNames struct {
	Session, CSRF string
}

// SecureCookieNames are used whenever the cookies are Secure, i.e. on every
// server but a local dev one. The __Host- prefix makes the browser accept
// such a cookie only when it is Secure, has Path=/ and NO Domain, and was
// set by this exact host over HTTPS. So a sibling subdomain, or a plain
// http page, cannot plant or overwrite either cookie.
var SecureCookieNames = CookieNames{Session: "__Host-odt_session", CSRF: "__Host-odt_csrf"}

// LocalCookieNames are used only on a LOG_ENV=local server, where cookies
// are not Secure (plain http on localhost) and a __Host- cookie would be
// refused by the browser.
var LocalCookieNames = CookieNames{Session: "odt_session", CSRF: "odt_csrf"}

// CookieNamesFor picks the names for a server whose cookies are (or are not)
// Secure.
func CookieNamesFor(secure bool) CookieNames {
	if secure {
		return SecureCookieNames
	}
	return LocalCookieNames
}

// CookieSourceName is the TokenSource.Name of the session cookie.
const CookieSourceName = "cookie"

// CookieSource reads the session cookie named in n. It carries session
// tokens only, and its writes need the CSRF header (double submit against
// the n.CSRF cookie). A cookie under any other name is ignored.
func CookieSource(n CookieNames) TokenSource {
	return TokenSource{
		Name:       CookieSourceName,
		Kinds:      []string{KindSession},
		CSRFCookie: n.CSRF,
		Extract: func(r *http.Request) (string, bool) {
			c, err := r.Cookie(n.Session)
			if err != nil || c.Value == "" {
				return "", false
			}
			return c.Value, true
		},
	}
}

// CSRFToken derives the CSRF token of a session. It is bound to the session
// (a "signed" double submit): a planted CSRF cookie on its own cannot match
// the victim's session. (Planting is already blocked on a Secure server by
// the __Host- names; see SecureCookieNames.) sha256 is one-way, so the CSRF
// token, which JavaScript can read, reveals nothing about the HttpOnly
// session token. The domain prefix keeps it different from the stored
// token hash.
func CSRFToken(session string) string {
	sum := sha256.Sum256([]byte("odt-csrf-v1\x00" + session))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// safeMethod is a method that must not change state, so it needs no CSRF
// header.
func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// csrfOK checks the double submit for a cookie request carrying session:
// the header equals the cookie, and the cookie is the session's own token.
// Both comparisons are constant time (golang-security: never == on secrets).
func csrfOK(r *http.Request, session, csrfCookie string) bool {
	header := r.Header.Get(CSRFHeader)
	c, err := r.Cookie(csrfCookie)
	if header == "" || err != nil || c.Value == "" {
		return false
	}
	want := CSRFToken(session)
	headerMatches := subtle.ConstantTimeCompare([]byte(header), []byte(c.Value)) == 1
	bound := subtle.ConstantTimeCompare([]byte(c.Value), []byte(want)) == 1
	return headerMatches && bound
}
