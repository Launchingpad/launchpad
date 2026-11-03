package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"devops/internal/httpx"
	"devops/internal/tracker"
)

// Attachment handlers: upload (prepare, then create from the
// upload), links, reads and the download redirect. Thin: parse, call the
// service, write.

func (s *Server) prepareAttachmentUpload(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	var in tracker.PrepareUploadInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	up, err := s.svc.PrepareUpload(r.Context(), o, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// The upload URL is a credential for 15 minutes: never cache it.
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, up)
}

func (s *Server) listAttachments(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	as, err := s.svc.ListAttachments(r.Context(), o, chi.URLParam(r, "issue"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page[tracker.Attachment]{Items: as})
}

func (s *Server) createAttachment(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in tracker.CreateAttachmentInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	a, created, err := s.svc.CreateAttachment(r.Context(), o, p, chi.URLParam(r, "issue"), in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !created {
		// The same upload was already attached to this issue: the retry gets the same row (LAUN-106).
		httpx.WriteJSON(w, http.StatusOK, a)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, a)
}

func (s *Server) getAttachment(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "attachment")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	a, err := s.svc.GetAttachment(r.Context(), o, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, a)
}

// downloadAttachment is a 302 to a presigned GET (5 minutes). The Location
// is a short-lived credential, so it is never cached and never sent on as a
// Referer.
func (s *Server) downloadAttachment(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "attachment")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	u, err := s.svc.DownloadURL(r.Context(), o, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, u, http.StatusFound)
}
