package api

import (
	"bufio"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"devops/internal/httpx"
	"devops/internal/tracker"
)

// Git integration config routes (T17, LAUN-7). Rules:
// docs/internal/decisions-T17-git-connections-import.md.

// The rate limit of T17 #28 (a guess, copied from the mail test: 6 a minute,
// burst 3), per org AND per user, in memory and per process. The routes that
// call a git host use it, because they let an admin session probe addresses.
const (
	gitCallBurst     = 3
	gitCallPerMinute = 6
)

type gitBucket struct {
	tokens float64
	last   time.Time
}

type gitLimiter struct {
	mu      sync.Mutex
	now     func() time.Time
	burst   float64
	perMin  float64
	buckets map[string]*gitBucket
}

func newGitLimiter(now func() time.Time) *gitLimiter {
	return &gitLimiter{now: now, burst: gitCallBurst, perMin: gitCallPerMinute, buckets: make(map[string]*gitBucket)}
}

// take takes one token from key's bucket, or reports false and the wait.
func (l *gitLimiter) take(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &gitBucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	rate := l.perMin / 60
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens = min(l.burst, b.tokens+el*rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false, time.Duration((1 - b.tokens) / rate * float64(time.Second))
	}
	b.tokens--
	return true, 0
}

func (l *gitLimiter) refund(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.buckets[key]; ok {
		b.tokens = min(l.burst, b.tokens+1)
	}
}

// checkGitCall takes one token for the org and one for the user; a refusal is
// the 429 with Retry-After. Only org admins reach it, so the table stays small.
func (s *Server) checkGitCall(w http.ResponseWriter, orgID, userID uuid.UUID) error {
	ok, wait := s.gitLimit.take("org:" + orgID.String())
	if ok {
		var uwait time.Duration
		if ok, uwait = s.gitLimit.take("user:" + userID.String()); ok {
			return nil
		}
		s.gitLimit.refund("org:" + orgID.String())
		wait = uwait
	}
	w.Header().Set("Retry-After", strconv.Itoa(max(int((wait+time.Second-1)/time.Second), 1)))
	return httpx.Errorf(http.StatusTooManyRequests, httpx.CodeRateLimited,
		"too many calls to git hosts; try again later")
}

func (s *Server) listGitIntegrations(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	items, err := s.svc.ListGitIntegrations(r.Context(), o)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page[tracker.GitIntegration]{Items: items})
}

func (s *Server) getGitIntegration(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "integration")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	gi, err := s.svc.GetGitIntegration(r.Context(), o, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, gi)
}

func (s *Server) createGitIntegration(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	var in tracker.CreateGitIntegrationInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	// Only a create that calls the host (token mode) uses the allowance.
	if in.Mode == tracker.ModeToken {
		if err := s.checkGitCall(w, o.ID, p.UserID); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	gi, err := s.svc.CreateGitIntegration(r.Context(), o, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, gi)
}

func (s *Server) checkGitIntegration(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "integration")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.checkGitCall(w, o.ID, p.UserID); err != nil {
		s.fail(w, r, err)
		return
	}
	res, err := s.svc.CheckGitIntegration(r.Context(), o, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (s *Server) updateGitIntegration(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "integration")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var in tracker.UpdateGitIntegrationInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if in.HasToken() {
		if err := s.checkGitCall(w, o.ID, p.UserID); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	gi, err := s.svc.UpdateGitIntegration(r.Context(), o, id, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, gi)
}

// --- secret rotation (T17 #11, LAUN-86) -------------------------------------

// rotateGitSecretBody is the optional body of rotate-secret: a pasted secret,
// for a GitLab webhook-only integration (as at create). Write-only.
type rotateGitSecretBody struct {
	Secret string `json:"secret"`
}

// commitGitSecretBody is the optional body of commit. Force commits in token
// mode although some hooks are not on the pending secret.
type commitGitSecretBody struct {
	Force bool `json:"force"`
}

// decodeOptionalJSON decodes a body that may be absent or empty (also a chunked
// empty one).
func decodeOptionalJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	if r.ContentLength == 0 {
		return nil
	}
	br := bufio.NewReader(r.Body)
	if _, err := br.Peek(1); err == io.EOF {
		return nil // a chunked body with no bytes
	}
	r.Body = struct {
		io.Reader
		io.Closer
	}{br, r.Body}
	return httpx.DecodeJSON(w, r, dst)
}

// deleteGitIntegration: DELETE /orgs/{org}/integrations/{integration} (LAUN-104).
func (s *Server) deleteGitIntegration(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "integration")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.svc.DeleteGitIntegration(r.Context(), o, id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) rotateGitSecret(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "integration")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var in rotateGitSecretBody
	if err := decodeOptionalJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.checkGitCall(w, o.ID, p.UserID); err != nil {
		s.fail(w, r, err)
		return
	}
	res, err := s.svc.RotateGitSecret(r.Context(), o, id, in.Secret)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

func (s *Server) commitGitSecret(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "integration")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var in commitGitSecretBody
	if err := decodeOptionalJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	gi, err := s.svc.CommitGitSecret(r.Context(), o, id, in.Force)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, gi)
}

func (s *Server) discardGitSecret(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "integration")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	gi, err := s.svc.DiscardGitSecret(r.Context(), o, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, gi)
}

// --- repos (T17 routes table, LAUN-86) ---------------------------------------

func (s *Server) availableGitRepos(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "integration")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.checkGitCall(w, o.ID, p.UserID); err != nil {
		s.fail(w, r, err)
		return
	}
	page, err := s.svc.AvailableGitRepos(r.Context(), o, id, r.URL.Query().Get("q"), r.URL.Query().Get("cursor"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, page)
}

func (s *Server) listGitRepos(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "integration")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	items, err := s.svc.ListGitRepos(r.Context(), o, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page[tracker.GitRepo]{Items: items})
}

func (s *Server) createGitRepo(w http.ResponseWriter, r *http.Request) {
	o, p := ctxOrgPrincipal(r)
	id, err := pathUUID(r, "integration")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var in tracker.CreateGitRepoInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	// Only a map that calls the host (token mode sends a repoId) uses the allowance.
	if in.RepoID != "" {
		if err := s.checkGitCall(w, o.ID, p.UserID); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	repo, err := s.svc.CreateGitRepo(r.Context(), o, id, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, repo)
}

func (s *Server) updateGitRepo(w http.ResponseWriter, r *http.Request) {
	o, _ := ctxOrgPrincipal(r)
	iid, err := pathUUID(r, "integration")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rid, err := pathUUID(r, "repo")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var in tracker.UpdateGitRepoInput
	if err := httpx.DecodeJSON(w, r, &in); err != nil {
		s.fail(w, r, err)
		return
	}
	repo, err := s.svc.UpdateGitRepo(r.Context(), o, iid, rid, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, repo)
}

func (s *Server) deleteGitRepo(w http.ResponseWriter, r *http.Request) {
	iid, err := pathUUID(r, "integration")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rid, err := pathUUID(r, "repo")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	o, p := ctxOrgPrincipal(r)
	res, err := s.svc.DeleteGitRepo(r.Context(), o, iid, rid, func() error { return s.checkGitCall(w, o.ID, p.UserID) })
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// --- MR state actions on a mapped repo (LAUN-83, decisions-T17 #21, #24) ----

// refreshGitHook: POST .../repos/{repo}/hook/refresh. Token mode.
func (s *Server) refreshGitHook(w http.ResponseWriter, r *http.Request) {
	iid, err := pathUUID(r, "integration")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rid, err := pathUUID(r, "repo")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	o, p := ctxOrgPrincipal(r)
	res, err := s.svc.RefreshGitHook(r.Context(), o, iid, rid, func() error { return s.checkGitCall(w, o.ID, p.UserID) })
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}

// backfillGitRepo: POST .../repos/{repo}/backfill. Token mode, synchronous,
// capped at 200 open changes.
func (s *Server) backfillGitRepo(w http.ResponseWriter, r *http.Request) {
	iid, err := pathUUID(r, "integration")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rid, err := pathUUID(r, "repo")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	o, p := ctxOrgPrincipal(r)
	res, err := s.svc.BackfillGitRepo(r.Context(), o, iid, rid, func() error { return s.checkGitCall(w, o.ID, p.UserID) })
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, res)
}
