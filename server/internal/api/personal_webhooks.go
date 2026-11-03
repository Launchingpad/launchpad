package api

import (
	"errors"
	"net/http"
	"strconv"

	"devops/internal/httpx"
	"devops/internal/notify"
)

// A member's own webhooks (LAUN-229, docs/internal/decisions-T40-personal-webhooks.md).
// Every route acts on the caller's own rows and refuses an agent token. The
// webhook URL is a secret: the POST, the PATCH and the test never go through
// the Idempotency-Key store (idempotency.Reject), and no response holds it.
// Another member's webhook, or another org's, is 404.

var (
	errPersonalOff      = httpx.NotFound("personal webhooks")
	errPersonalNotFound = httpx.NotFound("webhook")
)

func (s *Server) listPersonalWebhooks(w http.ResponseWriter, r *http.Request) {
	if s.Personal == nil {
		s.fail(w, r, errPersonalOff)
		return
	}
	o, p := ctxOrgPrincipal(r)
	out, err := s.Personal.List(r.Context(), o.ID, p.UserID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) createPersonalWebhook(w http.ResponseWriter, r *http.Request) {
	if s.Personal == nil {
		s.fail(w, r, errPersonalOff)
		return
	}
	o, p := ctxOrgPrincipal(r)
	if err := requireJSON(r); err != nil {
		s.fail(w, r, err)
		return
	}
	var in notify.PersonalCreateInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.Personal.Create(r.Context(), o.ID, p.UserID, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, out)
}

func (s *Server) patchPersonalWebhook(w http.ResponseWriter, r *http.Request) {
	if s.Personal == nil {
		s.fail(w, r, errPersonalOff)
		return
	}
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "webhook")
	if err != nil {
		s.fail(w, r, errPersonalNotFound)
		return
	}
	if err := requireJSON(r); err != nil {
		s.fail(w, r, err)
		return
	}
	var in notify.PersonalPatchInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.Personal.Patch(r.Context(), o.ID, p.UserID, id, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) deletePersonalWebhook(w http.ResponseWriter, r *http.Request) {
	if s.Personal == nil {
		s.fail(w, r, errPersonalOff)
		return
	}
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "webhook")
	if err != nil {
		s.fail(w, r, errPersonalNotFound)
		return
	}
	if err := s.Personal.Delete(r.Context(), o.ID, p.UserID, id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) testPersonalWebhook(w http.ResponseWriter, r *http.Request) {
	if s.Personal == nil {
		s.fail(w, r, errPersonalOff)
		return
	}
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "webhook")
	if err != nil {
		s.fail(w, r, errPersonalNotFound)
		return
	}
	out, err := s.Personal.Test(r.Context(), o.ID, p.UserID, id)
	if err != nil {
		var rl *notify.PersonalTestLimitedError
		if errors.As(err, &rl) {
			w.Header().Set("Retry-After", strconv.Itoa(rl.RetryAfterSeconds()))
		}
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
