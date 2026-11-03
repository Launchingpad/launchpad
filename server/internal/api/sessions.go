package api

import (
	"net/http"

	"devops/internal/auth"
	"devops/internal/httpx"
	"devops/internal/tracker"
)

// The caller's own login sessions (LAUN-130, decisions-T6a "Sessions list"). Human only
// (refuseAgents); user-private rows, so no change-feed event (decisions-T13 #32). The
// audit line holds ids and fixed words, never a token.

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	items, err := s.svc.ListSessions(r.Context(), p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.WriteJSON(w, http.StatusOK, httpx.Page[tracker.Session]{Items: items})
}

func (s *Server) revokeSession(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	id, err := pathUUID(r, "id")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rs, err := s.svc.RevokeSession(r.Context(), p, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if rs.Newly {
		s.log.InfoContext(r.Context(), "audit: session revoked", "actor", p.Username, "via", p.TokenKind,
			"session_id", rs.ID, "kind", rs.Kind, "self", rs.ID == p.TokenID)
	}
	// Revoking the session this very request used is a logout: drop the cookies too.
	if rs.ID == p.TokenID && p.Via == auth.CookieSourceName {
		n := s.cookieNames()
		http.SetCookie(w, s.clearedCookie(n.Session, true))
		http.SetCookie(w, s.clearedCookie(n.CSRF, false))
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) revokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	res, err := s.svc.RevokeOtherSessions(r.Context(), p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.InfoContext(r.Context(), "audit: other sessions revoked", "actor", p.Username, "via", p.TokenKind, "count", res.Revoked)
	httpx.WriteJSON(w, http.StatusOK, res)
}
