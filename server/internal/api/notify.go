package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/httpx"
	"devops/internal/notify"
)

// Notification preferences, team follows and issue subscriptions (LAUN-32,
// docs/internal/decisions-T13-notifications.md #32). Every route works on the
// caller's own rows; an agent token gets 403 forbidden (#14).

// refuseAgents answers 403 forbidden for an agent user. It sits after the
// scope check on every notification route.
func refuseAgents(next http.Handler) http.Handler {
	return refuseAgentsWith("notifications are not available to agent users")(next)
}

// refuseAgentsWith is refuseAgents with the route's own words (LAUN-235: the
// display preferences say "display preferences", not "notifications").
func refuseAgentsWith(msg string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if p, ok := auth.PrincipalFrom(r.Context()); ok && p.IsAgent {
				httpx.WriteError(w, http.StatusForbidden, httpx.CodeForbidden, msg)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (s *Server) getNotificationSettings(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	v, err := s.notify.View(r.Context(), o, p.UserID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

func (s *Server) updateNotificationSettings(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in notify.SettingsInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.notify.UpdateGlobal(r.Context(), o, p.UserID, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

func (s *Server) updateTeamNotificationSettings(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	teamID, err := s.svc.ResolveTeamID(r.Context(), o, chi.URLParam(r, "team"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var in notify.TeamSettingsInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.notify.UpdateTeam(r.Context(), o, p.UserID, teamID, in)
	if err != nil {
		s.fail(w, r, pathGone(err))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

func (s *Server) followTeam(w http.ResponseWriter, r *http.Request) {
	s.setFollow(w, r, s.notify.Follow)
}

func (s *Server) unfollowTeam(w http.ResponseWriter, r *http.Request) {
	s.setFollow(w, r, s.notify.Unfollow)
}

func (s *Server) setFollow(w http.ResponseWriter, r *http.Request, f func(ctx context.Context, org, user, team uuid.UUID) error) {
	o, p := ctxOrgPrincipal(r)
	teamID, err := s.svc.ResolveTeamID(r.Context(), o, chi.URLParam(r, "team"))
	if err == nil {
		err = f(r.Context(), o.ID, p.UserID, teamID)
	}
	if err != nil {
		s.fail(w, r, pathGone(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// pathGone turns a write that lost a race with a delete into 404: the row is
// named in the path, not in a body (LAUN-191).
func pathGone(err error) error {
	var gone *database.GoneError
	if errors.As(err, &gone) {
		return httpx.NotFound(gone.Ref)
	}
	return err
}

func (s *Server) getIssueSubscription(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	issueID, err := s.svc.ResolveIssueID(r.Context(), o, chi.URLParam(r, "issue"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sub, err := s.notify.GetSubscription(r.Context(), o.ID, p.UserID, issueID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, sub)
}

func (s *Server) putIssueSubscription(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	issueID, err := s.svc.ResolveIssueID(r.Context(), o, chi.URLParam(r, "issue"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var in notify.SubscriptionInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if in.Subscribed == nil {
		s.fail(w, r, httpx.Invalid("subscribed is required (true or false)"))
		return
	}
	sub, err := s.notify.SetSubscription(r.Context(), o.ID, p.UserID, issueID, *in.Subscribed)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, sub)
}

func (s *Server) deleteIssueSubscription(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	issueID, err := s.svc.ResolveIssueID(r.Context(), o, chi.URLParam(r, "issue"))
	if err == nil {
		err = s.notify.DeleteSubscription(r.Context(), o.ID, p.UserID, issueID)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Notify returns the notification settings service, so main can give it the
// mail check (MailSwitch.Ready) that the settings view reports.
func (s *Server) Notify() *notify.Service { return s.notify }

// The remembered issue-page views (LAUN-141): own rows, agents refused, no event.
func (s *Server) listViews(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	v, err := s.notify.ListViews(r.Context(), o.ID, p.UserID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

func (s *Server) putView(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in notify.ViewPreferenceInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.notify.PutView(r.Context(), o.ID, p.UserID, chi.URLParam(r, "place"), in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

// refuseAgentsPrefs refuses agent tokens on the display preference routes.
var refuseAgentsPrefs = refuseAgentsWith("display preferences are not available to agent users")

// The caller's display preferences (LAUN-235, decisions-T42): one row per user,
// install-wide, own row only, agents refused, no event.
func (s *Server) getPreferences(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	v, err := s.notify.GetPreferences(r.Context(), p.UserID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

func (s *Server) putPreferences(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	var in notify.PreferencesInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.notify.PutPreferences(r.Context(), p.UserID, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}
