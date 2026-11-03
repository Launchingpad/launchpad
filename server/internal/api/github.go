package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"devops/internal/github"
	"devops/internal/httpx"
	"devops/internal/scm"
	"devops/internal/tracker"
)

// T17 (LAUN-5): the GitHub webhook. GitHub signs the body (HMAC-SHA256), so
// the body must be read before the check; the order and the limits that keep
// that read cheap are decisions-T17 #14. Fixtures and rules: internal/github.

const (
	// maxSignedWebhookBytes is the most a signed-body webhook may carry (T17
	// #14; ours, not GitHub's 25 MiB).
	maxSignedWebhookBytes = 2 << 20
	// DefaultGitWebhookBudget is the process-wide budget of in-flight unsigned
	// body bytes. A guess, not measured (open point 8).
	DefaultGitWebhookBudget = 32 << 20
	// DefaultGitWebhookReadDeadline is the absolute time a body read may take.
	DefaultGitWebhookReadDeadline = 5 * time.Second
	// gitWebhookRetryAfter is the Retry-After (seconds) of an overloaded answer.
	gitWebhookRetryAfter = "5"
)

// byteBudget is a weighted budget of in-flight bytes. acquire never waits.
type byteBudget struct {
	mu          sync.Mutex
	used, limit int64
}

func newByteBudget(limit int64) *byteBudget { return &byteBudget{limit: limit} }

func (b *byteBudget) acquire(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+n > b.limit {
		return false
	}
	b.used += n
	return true
}

func (b *byteBudget) release(n int64) {
	b.mu.Lock()
	b.used -= n
	b.mu.Unlock()
}

func (b *byteBudget) inUse() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// SetGitWebhookLimits sets the byte budget and the body read deadline of the
// signed-body webhooks. Tests use it; main keeps the defaults. Call it before
// Routes.
func (s *Server) SetGitWebhookLimits(budget int64, readDeadline time.Duration) {
	s.gitBudget = newByteBudget(budget)
	s.gitReadDeadline = readDeadline
}

func ignoredResult(status int) (int, tracker.WebhookResult) {
	return status, tracker.WebhookResult{Status: tracker.WebhookIgnored, Outcome: tracker.WebhookIgnored}
}

// signedWebhook describes one signed-body provider for signedBodyWebhook.
type signedWebhook struct {
	name        string
	prov        scm.Provider
	signatureOK func(http.Header) bool // the header is present and well formed
	eventHeader string
	handles     func(event string) bool // an event the tracker reads
	ping        func(event string) bool // an event answered 200 ignored
	deliveryOK  func(http.Header) bool
	deliveryMsg string
}

var githubSigned = signedWebhook{
	name: github.ProviderName, prov: github.Provider{},
	signatureOK: func(h http.Header) bool { _, ok := github.ParseSignature(h); return ok },
	eventHeader: github.HeaderEvent,
	handles: func(e string) bool {
		switch e {
		case github.EventPullRequest, github.EventPullRequestReview, github.EventCheckSuite, github.EventStatus:
			return true
		}
		return false
	},
	ping:        func(e string) bool { return e == github.EventPing },
	deliveryOK:  func(h http.Header) bool { return github.ValidDelivery(h.Get(github.HeaderDelivery)) },
	deliveryMsg: fmt.Sprintf("%s is required: 1-%d printable characters", github.HeaderDelivery, github.MaxDeliveryBytes),
}

// githubWebhook: POST /orgs/{org}/integrations/github/webhook (signedBodyWebhook).
func (s *Server) githubWebhook(w http.ResponseWriter, r *http.Request) {
	s.signedBodyWebhook(w, r, githubSigned)
}

// signedBodyWebhook is the public webhook of a provider that signs the body
// (GitHub, Bitbucket). No bearer token, no session, no org membership, no rate
// limiter, no Idempotency-Key. The order is decisions-T17 #14:
//
//  1. The org and its enabled integrations of the provider (two indexed
//     reads). None: 401, before anything is read.
//  2. The signature header must be well formed, else 401.
//  3. Content-Length must be present (411) and at most 2 MiB (413).
//  4. The body bytes are taken from the in-flight budget (503 with
//     Retry-After when it is spent).
//  5. The body is read under an absolute deadline; a slow reader is closed.
//  6. HMAC-SHA256 under every secret of every enabled integration (current and
//     pending), constant time, no early exit; the match picks the
//     integration. A miss is 401.
//  7. Only then the event header, the delivery header and the payload.
//
// It logs no signature, secret or payload field. For an unmapped repository it
// logs the repository key; for a sealed value that does not open, our code
// secret_unreadable (T17 #33).
func (s *Server) signedBodyWebhook(w http.ResponseWriter, r *http.Request, sw signedWebhook) {
	ctx := r.Context()
	d := s.newGitDelivery(sw.name)
	defer d.commit(ctx) // the ring row (LAUN-87), after the answer
	targets, err := s.svc.GitWebhookTargets(ctx, sw.name, chi.URLParam(r, "org"))
	if err != nil {
		d.fail(w, r, err) // no org id yet: nothing is written
		return
	}
	d.org(targets.OrgID)
	if !sw.signatureOK(r.Header) {
		d.fail(w, r, tracker.ErrWebhookSignature)
		return
	}
	n := r.ContentLength
	if n < 0 {
		d.fail(w, r, httpx.Errorf(http.StatusLengthRequired, httpx.CodeLengthRequired, "Content-Length is required"))
		return
	}
	if n > maxSignedWebhookBytes {
		d.fail(w, r, httpx.Errorf(http.StatusRequestEntityTooLarge, httpx.CodePayloadTooLarge,
			"webhook body is larger than %d bytes", maxSignedWebhookBytes))
		return
	}
	if !s.gitBudget.acquire(n) {
		w.Header().Set("Retry-After", gitWebhookRetryAfter)
		d.fail(w, r, httpx.Errorf(http.StatusServiceUnavailable, httpx.CodeOverloaded, "too many webhook bodies in flight, retry later"))
		return
	}
	var once sync.Once
	release := func() { once.Do(func() { s.gitBudget.release(n) }) }
	defer release()

	body, err := s.readBodyBefore(w, r, n)
	if errors.Is(err, errNoReadDeadline) {
		// Fail closed: without our own deadline a slow sender would hold the
		// bytes until the server-wide timeout.
		release()
		s.warnOnce(ctx, sw.name+"-deadline", sw.name+" webhook: the connection cannot take a read deadline, so the body was refused", "org", targets.OrgSlug)
		d.fail(w, r, httpx.Errorf(http.StatusServiceUnavailable, httpx.CodeInternal, "webhook cannot be received on this connection"))
		return
	}
	if err != nil {
		release()
		s.log.WarnContext(ctx, sw.name+" webhook: body not read", "org", targets.OrgSlug, "timeout", isTimeout(err))
		w.Header().Set("Connection", "close")
		d.fail(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest, "could not read the webhook body"))
		return
	}

	hook, unreadable, err := s.svc.VerifyGitWebhook(targets, sw.prov, r.Header, body)
	release()
	for _, id := range unreadable {
		s.warnOnce(ctx, id.String(), "git webhook: a sealed secret could not be opened",
			"code", "secret_unreadable", "org", targets.OrgSlug, "integration_id", id, "provider", sw.name)
	}
	if err != nil {
		d.fail(w, r, err)
		return
	}
	d.bind(hook)
	event := r.Header.Get(sw.eventHeader)
	d.event(printableEvent(event))

	switch {
	case sw.handles(event):
	case sw.ping(event): // a hook was made or tested: nothing to do
		d.reply(w, http.StatusOK, tracker.WebhookResult{Status: tracker.WebhookIgnored, Outcome: tracker.WebhookIgnored})
		return
	default:
		code, res := ignoredResult(http.StatusAccepted)
		d.reply(w, code, res)
		return
	}
	if !sw.deliveryOK(r.Header) {
		d.fail(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest, "%s", sw.deliveryMsg))
		return
	}
	ev, parsed, err := sw.prov.Parse(r.Header, body)
	if err != nil {
		d.fail(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest, "not a usable %s payload: %v", printableEvent(event), err))
		return
	}
	if parsed == scm.ParseIgnored { // not reached: the event header was checked above
		code, res := ignoredResult(http.StatusAccepted)
		d.reply(w, code, res)
		return
	}
	d.repo(ev.RepoKey)
	// A pull request from a fork (or a deleted fork) may name any ticket:
	// the tracker acts on same-repo pull requests only (open point 16). It
	// writes nothing. Likewise an action that asks for nothing (a label, a
	// push, a comment review).
	if ev.Fork || ev.Plan().Noop() {
		code, res := ignoredResult(http.StatusOK)
		d.reply(w, code, res)
		return
	}
	keys, err := sw.prov.DeliveryKeys(r.Header, ev)
	if err != nil {
		d.fail(w, r, httpx.Errorf(http.StatusBadRequest, httpx.CodeBadRequest, "bad delivery headers"))
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
		s.log.InfoContext(ctx, sw.name+" webhook from an unmapped repository",
			"org", hook.OrgSlug, "integration_id", hook.IntegrationID, "repo_key", ev.RepoKey)
	}
	d.reply(w, status, res)
}

// readBodyBefore reads exactly n bytes of the request body within the
// handler's read deadline (an absolute time from now). A slow reader gets a
// read error at the deadline. The connection's read deadline is cleared
// after a complete read: the net/http server sets its own again before it
// reads the next request on the connection (the previous value cannot be read
// back). After a failed read it is left to expire, which closes the connection.
func (s *Server) readBodyBefore(w http.ResponseWriter, r *http.Request, n int64) ([]byte, error) {
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(s.gitReadDeadline)); err != nil {
		return nil, errNoReadDeadline
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r.Body, body); err != nil {
		// The deadline stays as it is: when it has passed, the server's own
		// attempt to drain the rest of the body after the answer fails at
		// once and the connection closes, instead of waiting for the sender.
		return nil, err
	}
	_ = rc.SetReadDeadline(time.Time{})
	return body, nil
}

var errNoReadDeadline = errors.New("no read deadline")

// warnOnce logs a WARN at most once a minute per key (a webhook that keeps
// failing must not fill the log).
func (s *Server) warnOnce(ctx context.Context, key, msg string, args ...any) {
	now := s.gitWarnNow()
	s.gitWarnMu.Lock()
	last, ok := s.gitWarnAt[key]
	if ok && now.Sub(last) < time.Minute {
		s.gitWarnMu.Unlock()
		return
	}
	if len(s.gitWarnAt) > 1024 {
		clear(s.gitWarnAt)
	}
	s.gitWarnAt[key] = now
	s.gitWarnMu.Unlock()
	s.log.WarnContext(ctx, msg, args...)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}
