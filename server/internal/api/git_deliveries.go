package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"devops/internal/httpx"
	"devops/internal/tracker"
)

// The deliveries ring (T17 #9, LAUN-87): both webhook handlers write one
// summary row per request, AFTER they have answered in their own code path.
// The answer is decided first and is never changed by the ring: a failed or
// slow write only logs our own code. No payload field, secret, signature,
// caller IP or User-Agent goes into a row.

// gitDelivery collects what one webhook request tells the ring. The zero org
// means nothing is written (an unknown org, or an org with no enabled
// integration of the provider: there is no ring to put it in).
type gitDelivery struct {
	s        *Server
	provider string
	rec      tracker.GitDeliveryRecord
	done     bool
}

func (s *Server) newGitDelivery(provider string) *gitDelivery {
	return &gitDelivery{s: s, provider: provider, rec: tracker.GitDeliveryRecord{Provider: provider}}
}

// org names the org once the request has reached one (still unbound: no
// integration matched yet).
func (d *gitDelivery) org(id uuid.UUID) { d.rec.OrgID = id }

// bind ties the request to the integration whose secret matched.
func (d *gitDelivery) bind(hook tracker.GitHook) {
	d.rec.OrgID = hook.OrgID
	id := hook.IntegrationID
	d.rec.IntegrationID = &id
}

// event sets the provider's event name and the repo key. Call it only after
// the secret matched: both come from the request.
func (d *gitDelivery) event(name string) { d.rec.Event = name }

func (d *gitDelivery) repo(key string) { d.rec.RepoKey = key }

// fail answers err as s.fail does, and notes the result.
func (d *gitDelivery) fail(w http.ResponseWriter, r *http.Request, err error) {
	d.s.fail(w, r, err)
	status, code := http.StatusInternalServerError, string(httpx.CodeInternal)
	var apiErr *httpx.Error
	if errors.As(err, &apiErr) {
		status, code = apiErr.Status, string(apiErr.Code)
	}
	d.rec.HTTPStatus, d.rec.ErrorCode = status, code
	switch status {
	case http.StatusUnauthorized:
		d.rec.Result = tracker.DeliveryBadSignature
	case http.StatusBadRequest, http.StatusLengthRequired:
		d.rec.Result = tracker.DeliveryBadRequest
	default:
		d.rec.Result = tracker.DeliveryError
	}
	d.done = true
}

// reply writes the JSON answer (a status only, decisions-T20 #65) and notes
// the result: what the event did (processed, ignored, duplicate or
// unmapped). The ring keeps it for the org's admins; the answer does not say.
func (d *gitDelivery) reply(w http.ResponseWriter, status int, res tracker.WebhookResult) {
	httpx.WriteJSON(w, status, res)
	outcome := res.Outcome
	if outcome == "" {
		outcome = res.Status
	}
	d.rec.HTTPStatus, d.rec.Result = status, outcome
	// A check event for a commit no link has writes no ring row (LAUN-97): a
	// busy CI repo would push every useful row out of the newest 100.
	d.done = !res.NoLink
}

// commit writes the row. Deferred at the top of a handler, it runs after the
// answer has been written. It never panics and never changes the answer.
func (d *gitDelivery) commit(ctx context.Context) {
	if !d.done || d.rec.OrgID == uuid.Nil {
		return
	}
	defer func() {
		if recover() != nil {
			d.s.warnOnce(ctx, "git-ring-panic-"+d.provider, "git webhook: delivery row not written", "code", "delivery_write_failed", "provider", d.provider)
		}
	}()
	if err := d.s.svc.RecordGitDelivery(ctx, d.rec); err != nil {
		// Our own code only: the error text can carry database detail.
		d.s.warnOnce(ctx, "git-ring-"+d.provider, "git webhook: delivery row not written",
			"code", "delivery_write_failed", "provider", d.provider, "org_id", d.rec.OrgID)
	}
}

// listGitDeliveries: GET /orgs/{org}/integrations/{integration}/deliveries.
func (s *Server) listGitDeliveries(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "integration")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	items, err := s.svc.ListGitDeliveries(r.Context(), o, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page[tracker.GitDelivery]{Items: items})
}
