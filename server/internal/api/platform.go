package api

import (
	"net/http"

	"devops/internal/auth"
	"devops/internal/httpx"
	"devops/internal/platform"
)

// The /platform routes (docs/internal/decisions-T9-mail-settings.md). All
// sit behind Authenticate and auth.RequirePlatformAdmin: a platform admin's
// session, or a platform admin's unpinned token with the admin scope. The
// writes need JSON and never take an Idempotency-Key (idempotency.Reject):
// the PUT body holds secrets, which must never be hashed into a stored row.

var errPlatformOff = httpx.NotFound("platform settings")

// getPlatformSettings: GET /platform/settings. Secrets are write-only.
func (s *Server) getPlatformSettings(w http.ResponseWriter, r *http.Request) {
	if s.Platform == nil {
		s.fail(w, r, errPlatformOff)
		return
	}
	out, err := s.Platform.Get(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// updatePlatformSettings: PUT /platform/settings, the whole settings.
func (s *Server) updatePlatformSettings(w http.ResponseWriter, r *http.Request) {
	if s.Platform == nil {
		s.fail(w, r, errPlatformOff)
		return
	}
	if err := requireJSON(r); err != nil {
		s.fail(w, r, err)
		return
	}
	var in platform.SettingsInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	out, err := s.Platform.Update(r.Context(), p, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// testPlatformMail: POST /platform/mail/test. One email to the caller's own
// address through the transport login uses now; rate limited.
func (s *Server) testPlatformMail(w http.ResponseWriter, r *http.Request) {
	if s.Platform == nil {
		s.fail(w, r, errPlatformOff)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	out, err := s.Platform.SendTest(r.Context(), p)
	if err != nil {
		s.failLogin(w, r, err) // Retry-After on a 429
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
