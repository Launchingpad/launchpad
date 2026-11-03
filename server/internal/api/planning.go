package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"devops/internal/httpx"
	"devops/internal/tracker"
)

// T3 handlers: team settings, projects, milestones, cycles,
// documents and document comments. Thin, like handlers.go: parse, call the
// service, write.

func (s *Server) updateTeam(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in tracker.UpdateTeamInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	t, err := s.svc.UpdateTeam(r.Context(), o, p, chi.URLParam(r, "team"), in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, t)
}

// refuseGitWorkflowAgents: agents get no git workflow route (decisions-T44 #9).
var refuseGitWorkflowAgents = refuseAgentsWith("the git workflow is not available to agent users")

// getGitWorkflow and putGitWorkflow: a team's git workflow rules (LAUN-244,
// decisions-T44). Members read, org admins write; agents are refused.
func (s *Server) getGitWorkflow(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	wf, err := s.svc.GetGitWorkflow(r.Context(), o, chi.URLParam(r, "team"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, wf)
}

func (s *Server) putGitWorkflow(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in tracker.GitWorkflowInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	wf, err := s.svc.SetGitWorkflow(r.Context(), o, p, chi.URLParam(r, "team"), in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, wf)
}

func (s *Server) deleteTeam(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	if err := s.svc.DeleteTeam(r.Context(), o, p, chi.URLParam(r, "team")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- projects --------------------------------------------------------------

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	f, err := tracker.ParseProjectFilter(r.URL.Query())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	page, err := s.svc.ListProjects(r.Context(), o, p, f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in tracker.CreateProjectInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	pr, err := s.svc.CreateProject(r.Context(), o, p, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, pr)
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "project")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pr, err := s.svc.GetProject(r.Context(), o, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, pr)
}

func (s *Server) updateProject(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "project")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var in tracker.UpdateProjectInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	pr, err := s.svc.UpdateProject(r.Context(), o, p, id, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, pr)
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "project")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.svc.DeleteProject(r.Context(), o, p, id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- milestones ------------------------------------------------------------

func (s *Server) listMilestones(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	f, err := tracker.ParseMilestoneFilter(r.URL.Query())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	page, err := s.svc.ListMilestones(r.Context(), o, f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

func (s *Server) createMilestone(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in tracker.CreateMilestoneInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	m, err := s.svc.CreateMilestone(r.Context(), o, p, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, m)
}

func (s *Server) getMilestone(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "milestone")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	m, err := s.svc.GetMilestone(r.Context(), o, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, m)
}

func (s *Server) updateMilestone(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "milestone")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var in tracker.UpdateMilestoneInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	m, err := s.svc.UpdateMilestone(r.Context(), o, p, id, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, m)
}

// --- cycles ----------------------------------------------------------------

func (s *Server) listCycles(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	f, err := tracker.ParseCycleFilter(r.URL.Query())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	page, err := s.svc.ListCycles(r.Context(), o, f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

func (s *Server) getCycle(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "cycle")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	c, err := s.svc.GetCycle(r.Context(), o, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, c)
}

func (s *Server) updateCycle(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "cycle")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var in tracker.UpdateCycleInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	c, err := s.svc.UpdateCycle(r.Context(), o, p, id, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, c)
}

func (s *Server) completeCycle(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "cycle")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	c, err := s.svc.CompleteCycle(r.Context(), o, p, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, c)
}

func (s *Server) deleteCycle(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "cycle")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.svc.DeleteCycle(r.Context(), o, p, id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listCycleSnapshots(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "cycle")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	snaps, err := s.svc.CycleSnapshots(r.Context(), o, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page[tracker.CycleSnapshot]{Items: snaps})
}

// --- documents -------------------------------------------------------------

func (s *Server) listDocuments(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	f, err := tracker.ParseDocumentFilter(r.URL.Query())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	page, err := s.svc.ListDocuments(r.Context(), o, f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

func (s *Server) createDocument(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in tracker.CreateDocumentInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	d, err := s.svc.CreateDocument(r.Context(), o, p, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, d)
}

func (s *Server) getDocument(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	d, err := s.svc.GetDocument(r.Context(), o, chi.URLParam(r, "document"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, d)
}

func (s *Server) updateDocument(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in tracker.UpdateDocumentInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	d, err := s.svc.UpdateDocument(r.Context(), o, p, chi.URLParam(r, "document"), in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, d)
}

func (s *Server) listDocumentComments(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	cs, err := s.svc.ListDocumentComments(r.Context(), o, chi.URLParam(r, "document"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page[tracker.Comment]{Items: cs})
}

func (s *Server) createDocumentComment(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in tracker.CommentInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	c, err := s.svc.CreateDocumentComment(r.Context(), o, p, chi.URLParam(r, "document"), in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, c)
}

// --- project labels (LAUN-194, decisions-T28) --------------------------------

func (s *Server) listProjectLabels(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	labels, err := s.svc.ListProjectLabels(r.Context(), o)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page[tracker.ProjectLabel]{Items: labels})
}

func (s *Server) createProjectLabel(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in tracker.ProjectLabelInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	l, err := s.svc.CreateProjectLabel(r.Context(), o, p, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, l)
}

func (s *Server) updateProjectLabel(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "label")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var in tracker.UpdateProjectLabelInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	l, err := s.svc.UpdateProjectLabel(r.Context(), o, p, id, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, l)
}

func (s *Server) deleteProjectLabel(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "label")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.svc.DeleteProjectLabel(r.Context(), o, p, id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- project updates (LAUN-196) ---------------------------------------------

func (s *Server) listProjectUpdates(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	pid, err := pathUUID(r, "project")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	f, err := tracker.ParseProjectUpdateFilter(r.URL.Query())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	page, err := s.svc.ListProjectUpdates(r.Context(), o, pid, f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

func (s *Server) createProjectUpdate(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	pid, err := pathUUID(r, "project")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var in tracker.CreateProjectUpdateInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	u, err := s.svc.CreateProjectUpdate(r.Context(), o, p, pid, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, u)
}

func (s *Server) getProjectUpdate(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	pid, err := pathUUID(r, "project")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	id, err := pathUUID(r, "update")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	u, err := s.svc.GetProjectUpdate(r.Context(), o, pid, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, u)
}
