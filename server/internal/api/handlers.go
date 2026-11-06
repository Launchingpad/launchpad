package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"devops/internal/auth"
	"devops/internal/httpx"
	"devops/internal/tracker"
)

func (s *Server) getMe(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	httpx.WriteJSON(w, http.StatusOK, tracker.MeFrom(p))
}

func (s *Server) listOrgs(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	orgs, err := s.svc.ListOrgs(r.Context(), p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page[tracker.Org]{Items: orgs})
}

func (s *Server) getOrg(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	out, err := s.svc.GetOrg(r.Context(), o)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// --- teams and states ------------------------------------------------------

func (s *Server) listTeams(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	teams, err := s.svc.ListTeams(r.Context(), o)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page[tracker.Team]{Items: teams})
}

func (s *Server) createTeam(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in tracker.TeamInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	t, err := s.svc.CreateTeam(r.Context(), o, p, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, t)
}

func (s *Server) getTeam(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	t, err := s.svc.GetTeam(r.Context(), o, chi.URLParam(r, "team"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, t)
}

func (s *Server) listStates(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	states, err := s.svc.ListStates(r.Context(), o, chi.URLParam(r, "team"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page[tracker.WorkflowState]{Items: states})
}

// --- labels ----------------------------------------------------------------

func (s *Server) listLabels(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	labels, err := s.svc.ListLabels(r.Context(), o, r.URL.Query().Get("team"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page[tracker.Label]{Items: labels})
}

func (s *Server) createLabel(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in tracker.LabelInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	l, err := s.svc.CreateLabel(r.Context(), o, p, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, l)
}

// --- issues ----------------------------------------------------------------

func (s *Server) listIssues(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	f, err := tracker.ParseListFilter(r.URL.Query(), timeNow())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	page, err := s.svc.ListIssues(r.Context(), o, p, f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

func (s *Server) createIssue(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in tracker.CreateIssueInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	is, err := s.svc.CreateIssue(r.Context(), o, p, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, is)
}

func (s *Server) getIssue(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	inc, err := tracker.ParseIncludes(r.URL.Query().Get("include"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	is, err := s.svc.GetIssue(r.Context(), o, p, chi.URLParam(r, "issue"), inc)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, is)
}

func (s *Server) updateIssue(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in tracker.UpdateIssueInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	is, err := s.svc.UpdateIssue(r.Context(), o, p, chi.URLParam(r, "issue"), in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, is)
}

func (s *Server) deleteIssue(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	if err := s.svc.DeleteIssue(r.Context(), o, p, chi.URLParam(r, "issue")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- comments --------------------------------------------------------------

func (s *Server) listComments(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	cs, err := s.svc.ListComments(r.Context(), o, chi.URLParam(r, "issue"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page[tracker.Comment]{Items: cs})
}

func (s *Server) listActivity(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	lq, err := tracker.ParseActivityQuery(r.URL.Query())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	page, err := s.svc.ListActivity(r.Context(), o, chi.URLParam(r, "issue"), lq)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

func (s *Server) createComment(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in tracker.CommentInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	c, err := s.svc.CreateComment(r.Context(), o, p, chi.URLParam(r, "issue"), in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, c)
}

func pathUUID(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		return uuid.Nil, httpx.NotFound(name)
	}
	return id, nil
}

func (s *Server) updateComment(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "comment")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var in tracker.CommentInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	c, err := s.svc.UpdateComment(r.Context(), o, p, id, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, c)
}

func (s *Server) deleteComment(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "comment")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.svc.DeleteComment(r.Context(), o, p, id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- relations -------------------------------------------------------------

func (s *Server) listRelations(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	rel, err := s.svc.ListRelations(r.Context(), o, chi.URLParam(r, "issue"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rel)
}

func (s *Server) createRelation(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in tracker.RelationInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	rel, err := s.svc.CreateRelation(r.Context(), o, p, chi.URLParam(r, "issue"), in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, rel)
}

func (s *Server) deleteRelation(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "relation")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.svc.DeleteRelation(r.Context(), o, p, chi.URLParam(r, "issue"), id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- users and tokens ------------------------------------------------------

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	ms, err := s.svc.ListMembers(r.Context(), o, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page[tracker.Member]{Items: ms})
}

// setTeamAccess: PUT /orgs/{org}/users/{user}/team-access (admin scope,
// decisions-T20 #48). ?dryRun=true answers what would change and writes
// nothing. A change that is written logs one audit line: ids and counts, no
// IP and no User-Agent.
func (s *Server) setTeamAccess(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var dryRun bool
	switch v := r.URL.Query().Get("dryRun"); v {
	case "", "false":
	case "true":
		dryRun = true
	default:
		s.fail(w, r, httpx.Invalid("dryRun must be true or false"))
		return
	}
	err := requireJSON(r)
	var in tracker.TeamAccessInput
	if err == nil {
		err = httpx.DecodeJSON(w, r, &in)
	}
	var res tracker.TeamAccessResult
	if err == nil {
		res, err = s.svc.SetTeamAccess(r.Context(), o, p, chi.URLParam(r, "user"), in, dryRun)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if res.Changed && !res.DryRun {
		s.log.InfoContext(r.Context(), "audit: team access changed", "org", o.Slug, "actor", p.Username, "via", p.TokenKind,
			"user_id", res.UserID, "team_access", res.TeamAccess, "teams", len(res.TeamIDs),
			"added", len(res.AddedTeamIDs), "lost", len(res.LostTeamIDs), "unassigned", res.Unassigned)
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	ts, err := s.svc.ListTokens(r.Context(), o)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page[tracker.Token]{Items: ts})
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in tracker.TokenInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	// Only a web session is rate limited; a bearer token is not (T11 #6).
	if auth.IsLoginKind(p.TokenKind) {
		if err := s.checkSessionMint(w, p.UserID); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	t, err := s.svc.CreateToken(r.Context(), o, p, in)
	if err != nil {
		if auth.IsLoginKind(p.TokenKind) {
			s.mintLimit.refund(p.UserID)
		}
		s.fail(w, r, err)
		return
	}
	// The audit line (decisions-T11 #7): who, what, never the secret.
	s.log.InfoContext(r.Context(), "audit: token created", "org", o.Slug, "actor", p.Username, "via", p.TokenKind,
		"token_id", t.Token.ID, "owner", t.Token.User.Username, "name", t.Token.Name, "scopes", t.Token.Scopes, "expires_at", t.Token.ExpiresAt)
	httpx.WriteJSON(w, http.StatusCreated, t)
}

// createAgent: POST /orgs/{org}/agents (admin scope). 201 with the new
// member; 409 conflict when the username is taken.
func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	err := requireJSON(r)
	var in tracker.AgentInput
	if err == nil {
		err = httpx.DecodeJSON(w, r, &in)
	}
	var m tracker.Member
	if err == nil {
		m, err = s.svc.CreateAgent(r.Context(), o, in)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, m)
}

// deleteAgent: DELETE /orgs/{org}/agents/{user} (admin scope, decisions-T47).
// 204; 404 for a human, an unknown user or another org's user. A repeat is a
// 404 (no Idempotency-Key).
func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	d, err := s.svc.DeleteAgent(r.Context(), o, chi.URLParam(r, "user"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.log.InfoContext(r.Context(), "audit: agent deleted", "org", o.Slug, "actor", p.Username, "via", p.TokenKind,
		"user_id", d.ID, "username", d.Username)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "token")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rt, err := s.svc.RevokeToken(r.Context(), o, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Only the call that revoked it is logged, not a repeat.
	if rt.Newly {
		s.log.InfoContext(r.Context(), "audit: token revoked", "org", o.Slug, "actor", p.Username, "via", p.TokenKind,
			"token_id", rt.ID, "owner", rt.Owner, "name", rt.Name, "scopes", rt.Scopes, "expires_at", rt.ExpiresAt)
	}
	w.WriteHeader(http.StatusNoContent)
}
