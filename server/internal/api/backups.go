package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"devops/internal/auth"
	"devops/internal/backupapi"
	"devops/internal/httpx"
)

// The /platform/backups routes (LAUN-69, docs/internal/decisions-T16-backups.md
// #43). All sit behind Authenticate and auth.RequirePlatformAdmin. Every write
// is JSON-only and never takes an Idempotency-Key (idempotency.Reject): the
// bodies hold secrets or queue work that must not be replayed. They dial
// nothing and emit no event (decisions-T16 #26): the record is the change log.

var errBackupsOff = httpx.NotFound("backups")

// backups returns the service, or answers 404 (as the platform routes do when
// the service is not wired).
func (s *Server) backups(w http.ResponseWriter, r *http.Request) *backupapi.Service {
	if s.Backups == nil {
		s.fail(w, r, errBackupsOff)
		return nil
	}
	return s.Backups
}

// backupBody reads a required JSON body.
func (s *Server) backupBody(w http.ResponseWriter, r *http.Request, dst any) error {
	if err := requireJSON(r); err != nil {
		return err
	}
	return httpx.DecodeJSON(w, r, dst)
}

// backupOptionalBody reads a JSON body when there is one.
func (s *Server) backupOptionalBody(w http.ResponseWriter, r *http.Request, dst any) error {
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return nil
	}
	return s.backupBody(w, r, dst)
}

func (s *Server) getBackupSettings(w http.ResponseWriter, r *http.Request) {
	b := s.backups(w, r)
	if b == nil {
		return
	}
	out, err := b.GetSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) updateBackupSettings(w http.ResponseWriter, r *http.Request) {
	b := s.backups(w, r)
	if b == nil {
		return
	}
	var in backupapi.SettingsInput
	if err := s.backupBody(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	out, err := b.UpdateSettings(r.Context(), p, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) getBackupStatus(w http.ResponseWriter, r *http.Request) {
	b := s.backups(w, r)
	if b == nil {
		return
	}
	out, err := b.GetStatus(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) listBackupRecipients(w http.ResponseWriter, r *http.Request) {
	b := s.backups(w, r)
	if b == nil {
		return
	}
	out, err := b.ListRecipients(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) addBackupRecipient(w http.ResponseWriter, r *http.Request) {
	b := s.backups(w, r)
	if b == nil {
		return
	}
	var in backupapi.AddRecipientInput
	if err := s.backupBody(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	out, err := b.AddRecipient(r.Context(), p, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, out)
}

func (s *Server) removeBackupRecipient(w http.ResponseWriter, r *http.Request) {
	b := s.backups(w, r)
	if b == nil {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	out, err := b.RemoveRecipient(r.Context(), p, chi.URLParam(r, "fingerprint"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) cancelBackupRecipient(w http.ResponseWriter, r *http.Request) {
	b := s.backups(w, r)
	if b == nil {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	out, err := b.CancelRecipient(r.Context(), p, chi.URLParam(r, "fingerprint"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) listBackupDestinations(w http.ResponseWriter, r *http.Request) {
	b := s.backups(w, r)
	if b == nil {
		return
	}
	out, err := b.ListDestinations(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) createBackupDestination(w http.ResponseWriter, r *http.Request) {
	b := s.backups(w, r)
	if b == nil {
		return
	}
	var in backupapi.CreateDestinationInput
	if err := s.backupBody(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	out, err := b.CreateDestination(r.Context(), p, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, out)
}

func (s *Server) getBackupDestination(w http.ResponseWriter, r *http.Request) {
	b := s.backups(w, r)
	if b == nil {
		return
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		s.fail(w, r, httpx.NotFound("destination"))
		return
	}
	out, err := b.GetDestination(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) updateBackupDestination(w http.ResponseWriter, r *http.Request) {
	b := s.backups(w, r)
	if b == nil {
		return
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		s.fail(w, r, httpx.NotFound("destination"))
		return
	}
	var in backupapi.UpdateDestinationInput
	if err := s.backupBody(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	out, err := b.UpdateDestination(r.Context(), p, id, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) deleteBackupDestination(w http.ResponseWriter, r *http.Request) {
	b := s.backups(w, r)
	if b == nil {
		return
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		s.fail(w, r, httpx.NotFound("destination"))
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	if err := b.DeleteDestination(r.Context(), p, id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// backupAction runs one of the queueing actions. The rate limit is taken inside
// the service, after the request checks (a bad request spends no token). The
// answer is 202 with the queued run; a 429 carries Retry-After.
func (s *Server) backupAction(w http.ResponseWriter, r *http.Request, fn func(b *backupapi.Service, p *auth.Principal) (backupapi.Run, error)) {
	b := s.backups(w, r)
	if b == nil {
		return
	}
	p, _ := auth.PrincipalFrom(r.Context())
	out, err := fn(b, p)
	if err != nil {
		s.failLogin(w, r, err) // Retry-After on the 429
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, out)
}

func (s *Server) probeBackupDestination(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		s.fail(w, r, httpx.NotFound("destination"))
		return
	}
	s.backupAction(w, r, func(b *backupapi.Service, _ *auth.Principal) (backupapi.Run, error) {
		return b.QueueProbe(r.Context(), id)
	})
}

func (s *Server) probeUnsavedBackupDestination(w http.ResponseWriter, r *http.Request) {
	var in backupapi.ProbeInput
	if err := s.backupBody(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	s.backupAction(w, r, func(b *backupapi.Service, _ *auth.Principal) (backupapi.Run, error) {
		return b.QueueProbeUnsaved(r.Context(), in)
	})
}

func (s *Server) testBackupDestination(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		s.fail(w, r, httpx.NotFound("destination"))
		return
	}
	var in backupapi.TestInput
	if err := s.backupOptionalBody(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	s.backupAction(w, r, func(b *backupapi.Service, p *auth.Principal) (backupapi.Run, error) {
		return b.QueueTest(r.Context(), p, id, in)
	})
}

func (s *Server) testUnsavedBackupDestination(w http.ResponseWriter, r *http.Request) {
	var in backupapi.UnsavedTestInput
	if err := s.backupBody(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	s.backupAction(w, r, func(b *backupapi.Service, _ *auth.Principal) (backupapi.Run, error) {
		return b.QueueTestUnsaved(r.Context(), in)
	})
}

func (s *Server) exchangeBackupDestination(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		s.fail(w, r, httpx.NotFound("destination"))
		return
	}
	var in backupapi.ExchangeInput
	if err := s.backupBody(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	s.backupAction(w, r, func(b *backupapi.Service, _ *auth.Principal) (backupapi.Run, error) {
		return b.QueueExchange(r.Context(), id, in)
	})
}

func (s *Server) runBackupDestination(w http.ResponseWriter, r *http.Request) {
	id, err := pathUUID(r, "id")
	if err != nil {
		s.fail(w, r, httpx.NotFound("destination"))
		return
	}
	s.backupAction(w, r, func(b *backupapi.Service, p *auth.Principal) (backupapi.Run, error) {
		return b.QueueRun(r.Context(), p, id)
	})
}

func (s *Server) listBackupRuns(w http.ResponseWriter, r *http.Request) {
	b := s.backups(w, r)
	if b == nil {
		return
	}
	pq, err := backupapi.ParsePageQuery(r.URL.Query(), true)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := b.ListRuns(r.Context(), pq)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) getBackupRun(w http.ResponseWriter, r *http.Request) {
	b := s.backups(w, r)
	if b == nil {
		return
	}
	id, err := pathUUID(r, "id")
	if err != nil {
		s.fail(w, r, httpx.NotFound("run"))
		return
	}
	out, err := b.GetRun(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *Server) listBackupChanges(w http.ResponseWriter, r *http.Request) {
	b := s.backups(w, r)
	if b == nil {
		return
	}
	pq, err := backupapi.ParsePageQuery(r.URL.Query(), false)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out, err := b.ListChanges(r.Context(), pq)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
