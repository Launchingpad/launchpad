package api

import (
	"bufio"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"devops/internal/auth"
	"devops/internal/httpx"
	"devops/internal/setup"
)

// The move routes (LAUN-249, docs/internal/decisions-T46-move-server.md).
//
//	POST /platform/move/export          platform admin, human: passphrase + ticket, once
//	GET  /platform/move/export/{ticket} streams the .lpmove file
//	POST /setup/restore                 public, the setup link is the credential
//
// Install-level: no event. The passphrase and the ticket are never logged,
// emitted or stored (the ticket path is hidden by logPath).

// HeaderSetupToken carries the setup link token of POST /setup/restore. HeaderPassphrase
// carries the bundle passphrase (or send it as the FIRST multipart part,
// named "passphrase"). Neither is ever a query parameter.
const (
	HeaderSetupToken = "X-Launchpad-Setup-Token"
	HeaderPassphrase = "X-Launchpad-Passphrase"
)

// maxPassphraseBytes bounds a passphrase form part.
const maxPassphraseBytes = 256

// startMoveExportInput is the POST /platform/move/export body (optional).
type startMoveExportInput struct {
	IncludeAttachments *bool `json:"includeAttachments"`
}

func (s *Server) startMoveExport(w http.ResponseWriter, r *http.Request) {
	if s.Move == nil {
		s.fail(w, r, httpx.NotFound("move"))
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	var in startMoveExportInput
	if r.ContentLength != 0 { // an empty body means the defaults
		if err := requireJSON(r); err != nil {
			s.fail(w, r, err)
			return
		}
		if err := httpx.DecodeJSON(w, r, &in); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	res, err := s.Move.StartExport(p.UserID, in.IncludeAttachments)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusCreated, res)
}

// lazyResponse sends the download headers on the first byte, so an error
// before any byte is a normal JSON error.
type lazyResponse struct {
	w       http.ResponseWriter
	name    string
	started bool
}

func (l *lazyResponse) Write(b []byte) (int, error) {
	if !l.started {
		l.started = true
		h := l.w.Header()
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": l.name}))
		h.Set("Cache-Control", "no-store")
		l.w.WriteHeader(http.StatusOK)
	}
	return l.w.Write(b)
}

func (s *Server) downloadMoveExport(w http.ResponseWriter, r *http.Request) {
	if s.Move == nil {
		s.fail(w, r, httpx.NotFound("move"))
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	tk := chi.URLParam(r, "ticket")
	name, err := s.Move.Filename(tk, p.UserID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	clearDeadlines(w)
	lw := &lazyResponse{w: w, name: name}
	bw := bufio.NewWriterSize(lw, 256<<10)
	_, err = s.Move.Export(r.Context(), tk, p.UserID, bw)
	if err == nil {
		err = bw.Flush()
	}
	if err != nil {
		if !lw.started {
			s.fail(w, r, err)
			return
		}
		// Bytes are out: the status cannot change. Cut the connection so the
		// client sees a failed download, never a clean short file.
		s.log.ErrorContext(r.Context(), "move export failed while streaming", "err", err)
		panic(http.ErrAbortHandler)
	}
}

// clearDeadlines lifts the server's read and write deadlines for one
// long-running stream. It does nothing when the writer does not support it.
func clearDeadlines(w http.ResponseWriter) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
}

// restoreMove: POST /setup/restore. Public. The setup link (header) is
// checked BEFORE the body is touched; nothing else answers first.
func (s *Server) restoreMove(w http.ResponseWriter, r *http.Request) {
	// net/http reads up to 256 KiB of an unread request body before it sends
	// the first byte of a reply. Full duplex stops that, so a refusal never
	// waits for the upload.
	_ = http.NewResponseController(w).EnableFullDuplex()
	if s.Setup == nil {
		s.fail(w, r, setup.ErrComplete)
		return
	}
	if err := setup.CheckLink(r.Context(), s.pool, r.Header.Get(HeaderSetupToken)); err != nil {
		s.fail(w, r, err)
		return
	}
	if s.Move == nil {
		s.fail(w, r, httpx.NotFound("move"))
		return
	}
	clearDeadlines(w)
	pass := r.Header.Get(HeaderPassphrase)
	var body io.Reader = r.Body
	if mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err == nil && strings.HasPrefix(mt, "multipart/") {
		mr, err := r.MultipartReader()
		if err != nil || params["boundary"] == "" {
			s.fail(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest, "the multipart body is malformed"))
			return
		}
		part, err := mr.NextPart()
		if err != nil {
			s.fail(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest, "the multipart body is empty"))
			return
		}
		if part.FormName() == "passphrase" {
			if pass == "" {
				b, err := io.ReadAll(io.LimitReader(part, maxPassphraseBytes+1))
				if err != nil || len(b) > maxPassphraseBytes {
					s.fail(w, r, httpx.Invalid("passphrase is too long"))
					return
				}
				pass = string(b)
			}
			if part, err = mr.NextPart(); err != nil {
				s.fail(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest, "the multipart body has no file part after the passphrase"))
				return
			}
		}
		if part.FormName() != "file" {
			s.fail(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest, "the bundle must be the multipart part named \"file\""))
			return
		}
		body = part
	}
	if strings.TrimSpace(pass) == "" {
		s.fail(w, r, httpx.Invalid("the passphrase is required: send it in the %s header, or as a first multipart part named \"passphrase\"", HeaderPassphrase))
		return
	}
	sess, out, err := s.Move.Restore(r.Context(), bufio.NewReaderSize(body, 1<<20), pass, r.Header.Get(HeaderSetupToken))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	n := s.cookieNames()
	http.SetCookie(w, s.sessionCookie(n.Session, sess.Secret, true, sess.ExpiresAt))
	http.SetCookie(w, s.sessionCookie(n.CSRF, sess.CSRF, false, sess.ExpiresAt))
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusCreated, out)
}
