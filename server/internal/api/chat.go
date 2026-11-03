package api

import (
	"net/http"

	"devops/internal/httpx"
	"devops/internal/notify"
)

// The org's chat services (LAUN-204, docs/internal/decisions-T36-chat-services.md).
// Every route is admin scope and refuses an agent token. The webhook URL is a
// secret: the POST and the PATCH never go through the Idempotency-Key store
// (idempotency.Reject), and no response holds the URL.

var errChatOff = httpx.NotFound("chat services")

func (s *Server) listChat(w http.ResponseWriter, r *http.Request) {
	if s.Chat == nil {
		s.fail(w, r, errChatOff)
		return
	}
	o, _ := ctxOrgPrincipal(r)
	out, err := s.Chat.List(r.Context(), o.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) getChat(w http.ResponseWriter, r *http.Request) {
	if s.Chat == nil {
		s.fail(w, r, errChatOff)
		return
	}
	o, _ := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "service")
	if err != nil {
		s.fail(w, r, errChatNotFoundAPI)
		return
	}
	out, err := s.Chat.Get(r.Context(), o.ID, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

var errChatNotFoundAPI = httpx.NotFound("chat service")

func (s *Server) createChat(w http.ResponseWriter, r *http.Request) {
	if s.Chat == nil {
		s.fail(w, r, errChatOff)
		return
	}
	o, p := ctxOrgPrincipal(r)
	if err := requireJSON(r); err != nil {
		s.fail(w, r, err)
		return
	}
	var in notify.ChatCreateInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.Chat.Create(r.Context(), o.ID, p, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, out)
}

func (s *Server) patchChat(w http.ResponseWriter, r *http.Request) {
	if s.Chat == nil {
		s.fail(w, r, errChatOff)
		return
	}
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "service")
	if err != nil {
		s.fail(w, r, errChatNotFoundAPI)
		return
	}
	if err := requireJSON(r); err != nil {
		s.fail(w, r, err)
		return
	}
	var in notify.ChatPatchInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.Chat.Patch(r.Context(), o.ID, id, p, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) deleteChat(w http.ResponseWriter, r *http.Request) {
	if s.Chat == nil {
		s.fail(w, r, errChatOff)
		return
	}
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "service")
	if err != nil {
		s.fail(w, r, errChatNotFoundAPI)
		return
	}
	if err := s.Chat.Delete(r.Context(), o.ID, id, p); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) testChat(w http.ResponseWriter, r *http.Request) {
	if s.Chat == nil {
		s.fail(w, r, errChatOff)
		return
	}
	o, _ := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "service")
	if err != nil {
		s.fail(w, r, errChatNotFoundAPI)
		return
	}
	out, err := s.Chat.Test(r.Context(), o.ID, id)
	if err != nil {
		s.failLogin(w, r, err) // Retry-After on a 429
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
