package api

import (
	"net/http"

	"devops/internal/httpx"
	"devops/internal/tracker"
)

// Agent guidance (LAUN-200; decisions-T31-agent-guidance.md). GET is read
// scope so an agent token can fetch it; PUT and DELETE are admin scope.

func (s *Server) getAgentGuidance(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	out, err := s.svc.GetGuidance(r.Context(), o, r.URL.Query().Get("team"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) putAgentGuidance(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	if err := requireJSON(r); err != nil {
		s.fail(w, r, err)
		return
	}
	var in tracker.GuidanceInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.svc.SetGuidance(r.Context(), o, p, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) deleteAgentGuidance(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	if err := s.svc.DeleteGuidance(r.Context(), o, p, r.URL.Query().Get("team")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
