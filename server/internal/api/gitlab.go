package api

import (
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"devops/internal/gitlab"
	"devops/internal/httpx"
	"devops/internal/scm"
	"devops/internal/tracker"
)

// T5: the GitLab webhook and its config routes. Rules:
// docs/internal/decisions-T5.md.

// maxWebhookBodyBytes bounds a webhook body. It is read only AFTER the token
// check, so only a holder of the secret can make the server buffer it.
// GitLab allows a 1,000,000-character description, and an update event
// carries the old and the new one.
const maxWebhookBodyBytes = 8 << 20

// gitlabWebhook: POST /orgs/{org}/integrations/gitlab/webhook. Public (no
// bearer token, no session, no org membership): GitLab proves itself with
// X-Gitlab-Token. The order matters:
//
//  1. The token is checked first, before the body is read or anything is
//     parsed: a wrong or missing token, an unknown org and an org with no
//     enabled integration are all the same 401.
//  2. A non-merge_request event is 202 and is never read.
//  3. The body (at most 8 MiB, else 413) is parsed; a bad one is 400.
//  4. One transaction applies the event. A database error is 500 and keeps
//     nothing, and processing the event again later is safe; a delivery
//     seen before (either of gitlab.DeliveryKeys) is 200 "duplicate" and
//     changes nothing; an unmapped project is 202.
//
// It never logs the token or anything from the payload except, for an
// unmapped project, the numeric GitLab project id an admin needs to map it.
func (s *Server) gitlabWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d := s.newGitDelivery("gitlab")
	defer d.commit(ctx) // the ring row (LAUN-87), after the answer
	hook, err := s.svc.AuthenticateGitlabWebhook(ctx, chi.URLParam(r, "org"), r.Header.Get(gitlab.HeaderToken))
	if err != nil {
		d.org(hook.OrgID) // set when the org has integrations; zero writes nothing
		d.fail(w, r, err)
		return
	}
	d.bind(hook)
	d.event(printableEvent(r.Header.Get(gitlab.HeaderEvent)))
	if h := r.Header.Get(gitlab.HeaderEvent); h != gitlab.MergeRequestHook && h != gitlab.PipelineHook {
		d.reply(w, http.StatusAccepted, tracker.WebhookResult{Status: tracker.WebhookIgnored, Outcome: tracker.WebhookIgnored})
		return
	}
	eventUUID := r.Header.Get(gitlab.HeaderEventUUID)
	if eventUUID == "" || len(eventUUID) > gitlab.MaxEventUUIDBytes || !printableASCII(eventUUID) {
		d.fail(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest,
			"%s is required: 1-%d printable characters", gitlab.HeaderEventUUID, gitlab.MaxEventUUIDBytes))
		return
	}
	idemKey := r.Header.Get(gitlab.HeaderIdempotencyKey)
	if idemKey != "" && (len(idemKey) > gitlab.MaxEventUUIDBytes || !printableASCII(idemKey)) {
		d.fail(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest,
			"%s must be 1-%d printable characters", gitlab.HeaderIdempotencyKey, gitlab.MaxEventUUIDBytes))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			d.fail(w, r, httpx.Errorf(http.StatusRequestEntityTooLarge, httpx.CodePayloadTooLarge,
				"webhook body is larger than %d bytes", maxWebhookBodyBytes))
			return
		}
		d.fail(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest, "could not read the webhook body"))
		return
	}
	var prov gitlab.Provider
	ev, parsed, err := prov.Parse(r.Header, body)
	if err != nil {
		d.fail(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest, "not a usable %s payload: %v", r.Header.Get(gitlab.HeaderEvent), err))
		return
	}
	if parsed == scm.ParseIgnored { // not reached: the event header was checked above
		d.reply(w, http.StatusAccepted, tracker.WebhookResult{Status: tracker.WebhookIgnored, Outcome: tracker.WebhookIgnored})
		return
	}
	d.repo(ev.RepoKey)
	if ev.Kind == scm.KindChecks && ev.Plan().Noop() { // a pipeline state the tracker does not know
		d.reply(w, http.StatusOK, tracker.WebhookResult{Status: tracker.WebhookIgnored, Outcome: tracker.WebhookIgnored})
		return
	}
	// The dedupe keys: Idempotency-Key (when sent) and the Event UUID, each
	// plus the MR (one GitLab request can fire several hooks with one header
	// value). Either one seen before makes the delivery a duplicate.
	keys, err := prov.DeliveryKeys(r.Header, ev)
	if err != nil {
		d.fail(w, r, err)
		return
	}
	res, err := s.svc.HandleSCMEvent(ctx, hook, keys, ev)
	if err != nil {
		d.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if res.Status == tracker.WebhookUnmapped {
		status = http.StatusAccepted
		s.log.InfoContext(ctx, "gitlab webhook from an unmapped project",
			"org", hook.OrgSlug, "integration_id", hook.IntegrationID, "gitlab_project_id", ev.RepoKey)
	}
	d.reply(w, status, res)
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// printableEvent is a provider's event header when it is a short printable
// name, else "" (the ring keeps the event name only).
func printableEvent(v string) string {
	if len(v) > 64 {
		return ""
	}
	for i := 0; i < len(v); i++ { // GitLab's names have spaces ("Merge Request Hook")
		if v[i] < 0x20 || v[i] > 0x7e {
			return ""
		}
	}
	return v
}
