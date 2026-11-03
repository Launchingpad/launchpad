package api

import (
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"devops/internal/httpx"
	"devops/internal/tracker"
)

// LAUN-22 avatars. Thin handlers over tracker/avatars.go. The GET streams the
// image from the store (decisions-T24 #1 to #4).

func (s *Server) prepareOrgAvatar(w http.ResponseWriter, r *http.Request) {
	s.prepareAvatar(w, r, false)
}
func (s *Server) setOrgAvatar(w http.ResponseWriter, r *http.Request)   { s.setAvatar(w, r, false) }
func (s *Server) clearOrgAvatar(w http.ResponseWriter, r *http.Request) { s.clearAvatar(w, r, false) }
func (s *Server) getOrgAvatar(w http.ResponseWriter, r *http.Request)   { s.getAvatar(w, r, false) }
func (s *Server) prepareProjectAvatar(w http.ResponseWriter, r *http.Request) {
	s.prepareAvatar(w, r, true)
}
func (s *Server) setProjectAvatar(w http.ResponseWriter, r *http.Request) { s.setAvatar(w, r, true) }
func (s *Server) clearProjectAvatar(w http.ResponseWriter, r *http.Request) {
	s.clearAvatar(w, r, true)
}
func (s *Server) getProjectAvatar(w http.ResponseWriter, r *http.Request) { s.getAvatar(w, r, true) }

// avatarTarget is nil for the org, the project id for a project.
func (s *Server) avatarTarget(w http.ResponseWriter, r *http.Request, project bool) (*uuid.UUID, bool) {
	if !project {
		return nil, true
	}
	id, err := pathUUID(r, "project")
	if err != nil {
		s.fail(w, r, err)
		return nil, false
	}
	return &id, true
}

func (s *Server) prepareAvatar(w http.ResponseWriter, r *http.Request, project bool) {
	o, _ := ctxOrgPrincipal(r)
	target, ok := s.avatarTarget(w, r, project)
	if !ok {
		return
	}
	var in tracker.PrepareAvatarInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	up, err := s.svc.PrepareAvatar(r.Context(), o, target, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// The upload URL is a credential for 15 minutes: never cache it.
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, up)
}

// writeAvatarOwner answers a set or clear with the owner as the API shows it.
func (s *Server) writeAvatarOwner(w http.ResponseWriter, r *http.Request, target *uuid.UUID) {
	o, _ := ctxOrgPrincipal(r)
	if target == nil {
		out, err := s.svc.GetOrg(r.Context(), o)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, out)
		return
	}
	out, err := s.svc.GetProject(r.Context(), o, *target)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) setAvatar(w http.ResponseWriter, r *http.Request, project bool) {
	o, p := ctxOrgPrincipal(r)
	target, ok := s.avatarTarget(w, r, project)
	if !ok {
		return
	}
	if err := requireJSON(r); err != nil {
		s.fail(w, r, err)
		return
	}
	var in tracker.SetAvatarInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.svc.SetAvatar(r.Context(), o, p, target, in); err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeAvatarOwner(w, r, target)
}

func (s *Server) clearAvatar(w http.ResponseWriter, r *http.Request, project bool) {
	o, p := ctxOrgPrincipal(r)
	target, ok := s.avatarTarget(w, r, project)
	if !ok {
		return
	}
	if err := s.svc.ClearAvatar(r.Context(), o, p, target); err != nil {
		s.fail(w, r, err)
		return
	}
	s.writeAvatarOwner(w, r, target)
}

// etagMatches reports whether an If-None-Match header names etag (or is *).
func etagMatches(header, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimPrefix(strings.TrimSpace(part), "W/")
		if part == "*" || part == etag {
			return true
		}
	}
	return false
}

// getAvatar streams the image. The type is the stored one, never sniffed; the
// response cannot run script even if the bytes were not an image.
func (s *Server) getAvatar(w http.ResponseWriter, r *http.Request, project bool) {
	o, _ := ctxOrgPrincipal(r)
	target, ok := s.avatarTarget(w, r, project)
	if !ok {
		return
	}
	f, err := s.svc.AvatarFileOf(r.Context(), o, target)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	h := w.Header()
	etag := `"av` + strconv.Itoa(int(f.Version)) + `"`
	h.Set("ETag", etag)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Referrer-Policy", "no-referrer")
	// The URL in the JSON carries ?v=<version>: that one never changes its
	// bytes, so it may be kept for a year. Any other URL is revalidated.
	if r.URL.Query().Get("v") == strconv.Itoa(int(f.Version)) {
		h.Set("Cache-Control", "private, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "private, no-cache")
	}
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body, size, err := s.svc.OpenAvatar(r.Context(), f)
	if err != nil {
		h.Del("ETag")
		h.Set("Cache-Control", "no-store")
		s.fail(w, r, err)
		return
	}
	defer body.Close()
	h.Set("Content-Type", f.ContentType)
	h.Set("Content-Disposition", "inline")
	if size > 0 && size <= tracker.MaxAvatarBytes {
		h.Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.Copy(w, io.LimitReader(body, tracker.MaxAvatarBytes))
}
