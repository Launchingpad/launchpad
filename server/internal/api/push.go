package api

import (
	"net/http"

	"devops/internal/httpx"
	"devops/internal/notify"
)

// Mobile push (LAUN-197, docs/internal/decisions-T35-mobile-push.md). The
// org's relay routes are admin scope; the device routes are the caller's own
// rows. Every route refuses an agent token. The relay URL, the relay secret
// and a device token are secrets: the PUTs never go through the
// Idempotency-Key store (idempotency.Reject), and no response holds one.

var errPushOff = httpx.NotFound("push")

func (s *Server) getPush(w http.ResponseWriter, r *http.Request) {
	if s.Push == nil {
		s.fail(w, r, errPushOff)
		return
	}
	o, _ := ctxOrgPrincipal(r)
	out, err := s.Push.Get(r.Context(), o.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) putPush(w http.ResponseWriter, r *http.Request) {
	if s.Push == nil {
		s.fail(w, r, errPushOff)
		return
	}
	o, p := ctxOrgPrincipal(r)
	if err := requireJSON(r); err != nil {
		s.fail(w, r, err)
		return
	}
	var in notify.PushInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.Push.Put(r.Context(), o.ID, p, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) deletePush(w http.ResponseWriter, r *http.Request) {
	if s.Push == nil {
		s.fail(w, r, errPushOff)
		return
	}
	o, p := ctxOrgPrincipal(r)
	if err := s.Push.Delete(r.Context(), o.ID, p); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) testPush(w http.ResponseWriter, r *http.Request) {
	if s.Push == nil {
		s.fail(w, r, errPushOff)
		return
	}
	o, p := ctxOrgPrincipal(r)
	out, err := s.Push.Test(r.Context(), o.ID, p)
	if err != nil {
		s.failLogin(w, r, err) // Retry-After on a 429
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) listPushDevices(w http.ResponseWriter, r *http.Request) {
	if s.Push == nil {
		s.fail(w, r, errPushOff)
		return
	}
	o, p := ctxOrgPrincipal(r)
	out, err := s.Push.ListDevices(r.Context(), o.ID, p.UserID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) putPushDevice(w http.ResponseWriter, r *http.Request) {
	if s.Push == nil {
		s.fail(w, r, errPushOff)
		return
	}
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "device")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := requireJSON(r); err != nil {
		s.fail(w, r, err)
		return
	}
	var in notify.DeviceInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := s.Push.PutDevice(r.Context(), o.ID, p.UserID, id, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) deletePushDevice(w http.ResponseWriter, r *http.Request) {
	if s.Push == nil {
		s.fail(w, r, errPushOff)
		return
	}
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "device")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Push.DeleteDevice(r.Context(), o.ID, p.UserID, id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
