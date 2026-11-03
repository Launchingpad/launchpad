package api

import (
	"net/http"

	"devops/internal/httpx"
	"devops/internal/tracker"
)

// Project statuses (LAUN-195, docs/internal/decisions-T29-project-statuses.md). Thin handlers.

func (s *Server) listProjectStatuses(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	items, err := s.svc.ListProjectStatuses(r.Context(), o)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page[tracker.ProjectStatus]{Items: items})
}

func (s *Server) createProjectStatus(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in tracker.CreateProjectStatusInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	st, err := s.svc.CreateProjectStatus(r.Context(), o, p, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, st)
}

func (s *Server) updateProjectStatus(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "status")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var in tracker.UpdateProjectStatusInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	st, err := s.svc.UpdateProjectStatus(r.Context(), o, p, id, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, st)
}

func (s *Server) deleteProjectStatus(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "status")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.svc.DeleteProjectStatus(r.Context(), o, p, id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) reorderProjectStatuses(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in tracker.ReorderProjectStatusesInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	items, err := s.svc.ReorderProjectStatuses(r.Context(), o, p, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page[tracker.ProjectStatus]{Items: items})
}
