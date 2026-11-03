package api

import (
	"net/http"
	"time"

	"devops/internal/httpx"
	"devops/internal/login"
	"devops/internal/setup"
)

// The /setup routes (docs/internal/decisions-T8-first-run.md). Both are
// public. POST /setup is padded to a floor, rate limited and needs JSON like
// the login routes (requireJSON closes cross-site form posts).

// getSetup: GET /setup. {"setupRequired": true} while the install has no
// user. It says nothing a caller could not learn from POST /setup.
func (s *Server) getSetup(w http.ResponseWriter, r *http.Request) {
	if s.Setup == nil {
		httpx.WriteJSON(w, http.StatusOK, setup.Status{})
		return
	}
	req, err := s.Setup.Required(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, setup.Status{SetupRequired: req})
}

// completeSetup: POST /setup. 201 sets the session cookies (as a login)
// and returns the login body plus the first admin token, once.
func (s *Server) completeSetup(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if s.Setup == nil {
		s.fail(w, r, setup.ErrComplete)
		return
	}
	err := requireJSON(r)
	var in setup.Request
	if err == nil {
		err = httpx.DecodeJSON(w, r, &in)
	}
	var (
		sess login.Session
		body setup.Response
	)
	if err == nil {
		sess, body, err = s.Setup.Complete(r.Context(), in)
	}
	s.Setup.Pad(r.Context(), start)
	if err != nil {
		s.failLogin(w, r, err) // Retry-After on a 429
		return
	}
	n := s.cookieNames()
	http.SetCookie(w, s.sessionCookie(n.Session, sess.Secret, true, sess.ExpiresAt))
	http.SetCookie(w, s.sessionCookie(n.CSRF, sess.CSRF, false, sess.ExpiresAt))
	httpx.WriteJSON(w, http.StatusCreated, body)
}
