package api

import (
	"net/http"

	"devops/internal/httpx"
	"devops/internal/tracker"
)

// Connected accounts (LAUN-199; decisions-T34). The /me routes are the
// caller's own rows: human only (refuseAgents), user-private, no event. The
// admin routes list and remove links.

func (s *Server) listMyGitAccounts(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	out, err := s.svc.ListMyGitAccounts(r.Context(), o.ID, p.UserID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) putMyGitAccount(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "integration")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var in tracker.GitAccountInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.svc.SetMyGitAccount(r.Context(), o.ID, p.UserID, id, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) deleteMyGitAccount(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "integration")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.svc.DeleteMyGitAccount(r.Context(), o.ID, p.UserID, id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listGitIdentities(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	out, err := s.svc.ListGitIdentities(r.Context(), o)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) deleteGitIdentity(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "identity")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.svc.DeleteGitIdentity(r.Context(), o, p, id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
