package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"devops/internal/gitapi/gitapitest"
	"devops/internal/github"
	"devops/internal/tracker"
)

// Secret rotation and the repo map (T17, LAUN-86).

// hostHook is a webhook the fake host holds.
type hostHook struct {
	id     int
	url    string
	secret string
	events []string
}

// repoHost is a GitHub Enterprise shaped host (API root <base>/api/v3) with
// repositories and hooks, and a status to inject per repository.
type repoHost struct {
	*gitapitest.Fake
	mu          sync.Mutex
	ids         []int
	hooks       map[int][]*hostHook
	seq         int
	failWrite   map[int]int // repo id -> status of POST/PATCH on its hooks
	delStatus   int         // status of DELETE on a hook; 0: 204
	onWrite     func()      // called once, at the next POST/PATCH of a hook
	badURL      map[int]string
	pulls       map[int][]map[string]any    // open pull requests per repo (LAUN-83 backfill), newest first
	reviews     map[string][]map[string]any // reviews per "repo/number" (LAUN-96), oldest first
	failReviews bool                        // answer 500 to a review read
}

var (
	hostRepoRE  = regexp.MustCompile(`^/repositories/(\d+)$`)
	hostHooksRE = regexp.MustCompile(`^/repositories/(\d+)/hooks(?:/(\d+))?$`)
	hostPullsRE = regexp.MustCompile(`^/repositories/(\d+)/pulls$`)
	hostRevsRE  = regexp.MustCompile(`^/repositories/(\d+)/pulls/(\d+)/reviews$`)
)

func newRepoHost(t *testing.T, ids ...int) *repoHost {
	t.Helper()
	h := &repoHost{ids: ids, hooks: map[int][]*hostHook{}, failWrite: map[int]int{}, badURL: map[int]string{}}
	h.Fake = gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		p := strings.TrimPrefix(r.URL.Path, "/api/v3")
		h.mu.Lock()
		defer h.mu.Unlock()
		switch {
		case p == "/user":
			w.Header().Set("X-OAuth-Scopes", "repo, admin:repo_hook")
			gitapitest.JSON(w, 200, map[string]string{"login": "octo"})
			return true
		case p == "/user/repos":
			page := 1
			_, _ = fmt.Sscan(r.URL.Query().Get("page"), &page)
			from, to := (page-1)*50, page*50
			if to < len(h.ids) {
				w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=%d&per_page=50>; rel="next"`, h.Srv.URL, r.URL.Path, page+1))
			}
			var out []map[string]any
			for i := from; i < to && i < len(h.ids); i++ {
				out = append(out, h.repoJSON(h.ids[i]))
			}
			gitapitest.JSON(w, 200, out)
			return true
		}
		if m := hostRepoRE.FindStringSubmatch(p); m != nil {
			id, _ := strconv.Atoi(m[1])
			if !h.has(id) {
				return false
			}
			gitapitest.JSON(w, 200, h.repoJSON(id))
			return true
		}
		if m := hostHooksRE.FindStringSubmatch(p); m != nil {
			repo, _ := strconv.Atoi(m[1])
			if !h.has(repo) {
				return false
			}
			return h.hookCall(w, r, repo, m[2])
		}
		if m := hostRevsRE.FindStringSubmatch(p); m != nil && r.Method == http.MethodGet {
			if h.failReviews {
				w.WriteHeader(http.StatusInternalServerError)
				return true
			}
			out := h.reviews[m[1]+"/"+m[2]]
			if out == nil {
				out = []map[string]any{}
			}
			gitapitest.JSON(w, 200, out)
			return true
		}
		if m := hostPullsRE.FindStringSubmatch(p); m != nil && r.Method == http.MethodGet {
			repo, _ := strconv.Atoi(m[1])
			if !h.has(repo) {
				return false
			}
			page := 1
			_, _ = fmt.Sscan(r.URL.Query().Get("page"), &page)
			from, to := (page-1)*100, page*100
			all := h.pulls[repo]
			if to < len(all) {
				w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=%d&per_page=100>; rel="next"`, h.Srv.URL, r.URL.Path, page+1))
			}
			out := []map[string]any{}
			for i := from; i < to && i < len(all); i++ {
				out = append(out, all[i])
			}
			gitapitest.JSON(w, 200, out)
			return true
		}
		return false
	})
	return h
}

func (h *repoHost) has(id int) bool {
	for _, x := range h.ids {
		if x == id {
			return true
		}
	}
	return false
}

func (h *repoHost) repoJSON(id int) map[string]any {
	u := fmt.Sprintf("https://github.example.com/acme/repo-%d", id)
	if b, ok := h.badURL[id]; ok {
		u = b
	}
	return map[string]any{"id": id, "full_name": fmt.Sprintf("acme/repo-%d", id), "html_url": u}
}

func (h *repoHost) hookCall(w http.ResponseWriter, r *http.Request, repo int, hid string) bool {
	hookJSON := func(k *hostHook) map[string]any {
		return map[string]any{"id": k.id, "config": map[string]any{"url": k.url}}
	}
	switch {
	case r.Method == http.MethodGet && hid == "":
		var out []map[string]any
		for _, k := range h.hooks[repo] {
			out = append(out, hookJSON(k))
		}
		gitapitest.JSON(w, 200, out)
		return true
	case r.Method == http.MethodDelete:
		if h.delStatus != 0 {
			w.WriteHeader(h.delStatus)
			return true
		}
		id, _ := strconv.Atoi(hid)
		kept := h.hooks[repo][:0]
		for _, k := range h.hooks[repo] {
			if k.id != id {
				kept = append(kept, k)
			}
		}
		h.hooks[repo] = kept
		w.WriteHeader(204)
		return true
	case r.Method == http.MethodPost || r.Method == http.MethodPatch:
		if f := h.onWrite; f != nil {
			h.onWrite = nil
			f()
		}
		if st := h.failWrite[repo]; st != 0 {
			if st == 429 {
				w.Header().Set("Retry-After", "1")
			}
			gitapitest.JSON(w, st, map[string]string{"message": "host words that must never be echoed"})
			return true
		}
		var body struct {
			Config struct {
				URL    string `json:"url"`
				Secret string `json:"secret"`
			} `json:"config"`
			Events []string `json:"events"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		if r.Method == http.MethodPost {
			h.seq++
			k := &hostHook{id: 9000 + h.seq, url: body.Config.URL, secret: body.Config.Secret, events: body.Events}
			h.hooks[repo] = append(h.hooks[repo], k)
			gitapitest.JSON(w, 201, hookJSON(k))
			return true
		}
		id, _ := strconv.Atoi(hid)
		for _, k := range h.hooks[repo] {
			if k.id == id {
				k.url, k.secret, k.events = body.Config.URL, body.Config.Secret, body.Events
				gitapitest.JSON(w, 200, hookJSON(k))
				return true
			}
		}
	}
	return false
}

func (h *repoHost) secret(repo int) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.hooks[repo]) == 0 {
		return ""
	}
	return h.hooks[repo][0].secret
}

func (h *repoHost) hookCount(repo int) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.hooks[repo])
}

func (h *repoHost) fail(repo, status int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if status == 0 {
		delete(h.failWrite, repo)
		return
	}
	h.failWrite[repo] = status
}

// pingStatus sends a signed GitHub ping: 200 when the secret opens the webhook.
func (g *gitEnv) pingStatus(secret string) int {
	g.t.Helper()
	gh := &ghEnv{env: g.env}
	return gh.ghHook("orbit", secret, uuid.NewString(), []byte("{}"), github.HeaderEvent, "ping").status
}

// glAccepts is true when the GitLab token passes the token check (the empty
// body is then a 400, not a 401).
func (g *gitEnv) glAccepts(secret string) bool {
	g.t.Helper()
	return g.hook("orbit", secret, uuid.NewString(), []byte("{}")).status != http.StatusUnauthorized
}

func (g *gitEnv) path(id uuid.UUID, tail string) string { return gitBase + "/" + id.String() + tail }

func (g *gitEnv) rotate(id uuid.UUID, status int, body any) tracker.GitRotateResult {
	g.t.Helper()
	var out tracker.GitRotateResult
	g.must(status, g.do("POST", g.path(id, "/rotate-secret"), g.chen, body)).json(g.t, &out)
	return out
}

func (g *gitEnv) repos(id uuid.UUID) []tracker.GitRepo {
	g.t.Helper()
	var l struct {
		Items []tracker.GitRepo `json:"items"`
	}
	g.must(http.StatusOK, g.do("GET", g.path(id, "/repos"), g.chen, nil)).json(g.t, &l)
	return l.Items
}

func (g *gitEnv) mapRepo(id uuid.UUID, repoID int) tracker.GitRepo {
	g.t.Helper()
	var r tracker.GitRepo
	g.must(http.StatusCreated, g.do("POST", g.path(id, "/repos"), g.chen, map[string]any{"repoId": strconv.Itoa(repoID)})).json(g.t, &r)
	return r
}

func (g *gitEnv) integration(id uuid.UUID) tracker.GitIntegration {
	g.t.Helper()
	var gi tracker.GitIntegration
	g.must(http.StatusOK, g.do("GET", g.path(id, ""), g.chen, nil)).json(g.t, &gi)
	return gi
}

func (g *gitEnv) hookStates(id uuid.UUID) map[string]string {
	out := map[string]string{}
	for _, r := range g.repos(id) {
		s := r.HookState
		if r.HookError != nil {
			s += ":" + *r.HookError
		}
		out[r.RepoKey] = s
	}
	return out
}

// A webhook-only rotation: both secrets verify until commit, only the new one
// after it; after a discard only the old one. GitHub: either HMAC.
func TestGitRotate_WebhookOnlyGitHub(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	var gi tracker.GitIntegrationCreated
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "github", "baseUrl": "https://github.com"})).json(t, &gi)
	s0 := gi.WebhookSecret
	g.must(http.StatusCreated, g.do("POST", g.path(gi.ID, "/repos"), g.chen, map[string]any{"repoKey": ghRepoKey, "repoPath": "acme/x"}))

	rot := g.rotate(gi.ID, http.StatusOK, nil)
	s1 := rot.WebhookSecret
	if len(s1) < 32 || s1 == s0 || !rot.SecretPending || rot.Committed || len(rot.FailedRepos) != 0 || rot.Mode != "webhook_only" {
		t.Fatalf("rotate = %+v", rot)
	}
	if g.pingStatus(s0) != 200 || g.pingStatus(s1) != 200 || g.pingStatus(s1+"x") != 401 {
		t.Fatal("while pending the old and the pending secret must both verify, nothing else")
	}
	// The secret is shown once: no later response carries it.
	for _, r := range []resp{
		g.must(http.StatusOK, g.do("GET", g.path(gi.ID, ""), g.chen, nil)),
		g.must(http.StatusOK, g.do("GET", gitBase, g.chen, nil)),
	} {
		if strings.Contains(string(r.body), s1) || strings.Contains(string(r.body), `"webhookSecret"`) {
			t.Fatalf("a later response shows the secret: %s", r.body)
		}
	}
	if !g.integration(gi.ID).SecretPending {
		t.Fatal("secretPending false while pending")
	}
	// Sealed under the NEXT column, bound to the row.
	var next []byte
	if err := g.pool.QueryRow(context.Background(), `SELECT webhook_secret_next_enc FROM git_integrations WHERE id = $1`, gi.ID).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if pt, err := g.kr.OpenFor("webhook_secret_next_enc", gi.ID.String(), next); err != nil || string(pt) != s1 {
		t.Fatalf("pending blob: %v", err)
	}

	// Commit: only the new one.
	var after tracker.GitIntegration
	g.must(http.StatusOK, g.do("POST", g.path(gi.ID, "/rotate-secret/commit"), g.chen, nil)).json(t, &after)
	if after.SecretPending || g.pingStatus(s0) != 401 || g.pingStatus(s1) != 200 {
		t.Fatal("after commit only the new secret may verify")
	}
	var cur, nx []byte
	var nxHash []byte
	if err := g.pool.QueryRow(context.Background(), `SELECT webhook_secret_enc, webhook_secret_next_enc, webhook_secret_next_hash FROM git_integrations WHERE id = $1`, gi.ID).Scan(&cur, &nx, &nxHash); err != nil {
		t.Fatal(err)
	}
	if pt, err := g.kr.OpenFor("webhook_secret_enc", gi.ID.String(), cur); err != nil || string(pt) != s1 || nx != nil || nxHash != nil {
		t.Fatalf("after commit: current opens=%v, next=%v/%v", err, nx, nxHash)
	}
	// A commit with nothing pending is 409.
	if r := g.do("POST", g.path(gi.ID, "/rotate-secret/commit"), g.chen, nil); r.status != http.StatusConflict || r.errCode(t) != "conflict" {
		t.Fatalf("commit with nothing pending: %d %s", r.status, r.body)
	}

	// Discard: only the old one. A second rotate replaces a pending secret in
	// webhook-only mode. Discard is safe to repeat.
	s2 := g.rotate(gi.ID, http.StatusOK, nil).WebhookSecret
	s3 := g.rotate(gi.ID, http.StatusOK, nil).WebhookSecret
	if s3 == s2 || g.pingStatus(s2) != 401 || g.pingStatus(s3) != 200 || g.pingStatus(s1) != 200 {
		t.Fatal("webhook-only: a second rotate must replace the pending secret")
	}
	for range 2 {
		var d tracker.GitIntegration
		g.must(http.StatusOK, g.do("DELETE", g.path(gi.ID, "/rotate-secret"), g.chen, nil)).json(t, &d)
		if d.SecretPending {
			t.Fatal("secretPending after discard")
		}
	}
	if g.pingStatus(s3) != 401 || g.pingStatus(s1) != 200 {
		t.Fatal("after discard only the old secret may verify")
	}
	// The cache learns of every change: the old secret still works after a rotate+commit round trip.
	s4 := g.rotate(gi.ID, http.StatusOK, nil).WebhookSecret
	g.must(http.StatusOK, g.do("POST", g.path(gi.ID, "/rotate-secret/commit"), g.chen, nil))
	if g.pingStatus(s1) != 401 || g.pingStatus(s4) != 200 {
		t.Fatal("second commit")
	}
	// The rotate body takes no pasted secret for GitHub.
	g.must(http.StatusBadRequest, g.do("POST", g.path(gi.ID, "/rotate-secret"), g.chen, map[string]any{"secret": glSecret}))
}

// The GitLab hash path accepts the current and the pending token; a row that
// keeps its secret as a hash only (no sealed copy) gets one on its first
// rotate-and-commit with a key, and rotates by hash alone without one.
func TestGitRotate_GitLabHashPath(t *testing.T) {
	for _, withKey := range []bool{true, false} {
		t.Run(fmt.Sprintf("key=%v", withKey), func(t *testing.T) {
			g := newGitEnv(t, gitEnvOpts{noKey: !withKey})
			// A row from before migration A: hash only, no sealed copy.
			id := uuid.Must(uuid.NewV7())
			h := hashHex(glSecret)
			if _, err := g.pool.Exec(context.Background(), `INSERT INTO git_integrations (id, org_id, provider, base_url, webhook_secret_hash)
				VALUES ($1, $2, 'gitlab', $3, decode($4, 'hex'))`, id, g.orgID, glBaseURL, h); err != nil {
				t.Fatal(err)
			}
			rot := g.rotate(id, http.StatusOK, nil)
			s1 := rot.WebhookSecret
			if len(s1) < 32 || !rot.SecretPending {
				t.Fatalf("rotate = %+v", rot)
			}
			if !g.glAccepts(glSecret) || !g.glAccepts(s1) || g.glAccepts(s1+"x") {
				t.Fatal("while pending both tokens must pass, nothing else")
			}
			g.must(http.StatusOK, g.do("POST", g.path(id, "/rotate-secret/commit"), g.chen, nil))
			if g.glAccepts(glSecret) || !g.glAccepts(s1) {
				t.Fatal("after commit only the new token may pass")
			}
			var enc []byte
			if err := g.pool.QueryRow(context.Background(), `SELECT webhook_secret_enc FROM git_integrations WHERE id = $1`, id).Scan(&enc); err != nil {
				t.Fatal(err)
			}
			if withKey {
				if pt, err := g.kr.OpenFor("webhook_secret_enc", id.String(), enc); err != nil || string(pt) != s1 {
					t.Fatalf("the first rotate must store a sealed copy: %v", err)
				}
			} else if enc != nil {
				t.Fatal("without a key the commit stores no sealed copy")
			}
			// A pasted secret: accepted, never returned.
			pasted := "pasted-gitlab-secret-0123456789abcdef0123"
			r := g.do("POST", g.path(id, "/rotate-secret"), g.chen, map[string]any{"secret": pasted})
			if r.status != http.StatusOK || strings.Contains(string(r.body), pasted) || strings.Contains(string(r.body), `"webhookSecret"`) {
				t.Fatalf("pasted: %d %s", r.status, r.body)
			}
			if !g.glAccepts(pasted) || !g.glAccepts(s1) {
				t.Fatal("pasted pending token")
			}
			g.must(http.StatusBadRequest, g.do("POST", g.path(id, "/rotate-secret"), g.chen, map[string]any{"secret": "short"}))
			g.must(http.StatusOK, g.do("DELETE", g.path(id, "/rotate-secret"), g.chen, nil))
			if g.glAccepts(pasted) || !g.glAccepts(s1) {
				t.Fatal("after a discard only the current token may pass")
			}
		})
	}
}

// GitHub and token mode need the key.
func TestGitRotate_EncryptionUnavailable(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	h := newRepoHost(t, 1001)
	var web tracker.GitIntegrationCreated
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "github", "baseUrl": "https://github.com"})).json(t, &web)
	tok := g.tokenCreate(&fakeHost{Fake: h.Fake})
	g.server.svc.SetGitSealer(nil) // the key is gone

	for _, id := range []uuid.UUID{web.ID, tok.ID} {
		if r := g.do("POST", g.path(id, "/rotate-secret"), g.chen, nil); r.status != http.StatusConflict || r.errCode(t) != "encryption_unavailable" {
			t.Fatalf("rotate without a key: %d %s", r.status, r.body)
		}
	}
	// A token-mode map without the key.
	if r := g.do("POST", g.path(tok.ID, "/repos"), g.chen, map[string]any{"repoId": "1001"}); r.status != http.StatusConflict || r.errCode(t) != "encryption_unavailable" {
		t.Fatalf("map without a key: %d %s", r.status, r.body)
	}
	if r := g.do("GET", g.path(tok.ID, "/available-repos"), g.chen, nil); r.status != http.StatusConflict || r.errCode(t) != "encryption_unavailable" {
		t.Fatalf("available-repos without a key: %d %s", r.status, r.body)
	}
	// A webhook-only GitLab row still rotates (hash only).
	var gl tracker.GitIntegrationCreated
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "gitlab", "baseUrl": glBaseURL})).json(t, &gl)
	g.rotate(gl.ID, http.StatusOK, nil)
}

// Token mode needs an https PUBLIC_BASE_URL: the host must call the webhook.
func TestGitRotate_PublicURLRequired(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{publicBase: "-"})
	h := newRepoHost(t, 1001)
	if r := g.create(map[string]any{"provider": "github", "baseUrl": h.Srv.URL, "mode": "token", "token": gitToken}); r.status != http.StatusConflict || r.errCode(t) != "public_url_required" {
		t.Fatalf("token create: %d %s", r.status, r.body)
	}
	var web tracker.GitIntegration
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "github", "baseUrl": h.Srv.URL})).json(t, &web)
	if r := g.do("PATCH", g.path(web.ID, ""), g.chen, map[string]any{"token": gitToken}); r.status != http.StatusConflict || r.errCode(t) != "public_url_required" {
		t.Fatalf("token patch: %d %s", r.status, r.body)
	}
}

// Token mode: one hook per repo, the pending secret reaches every hook, a
// failed hook keeps both secrets, a retry reuses the pending secret.
func TestGitRotate_TokenMode(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	h := newRepoHost(t, 1001, 1002, 1003)
	gi := g.tokenCreate(&fakeHost{Fake: h.Fake})
	if gi.WebhookSecret != "" {
		t.Fatal("token mode returns no secret")
	}
	r1, r2, r3 := g.mapRepo(gi.ID, 1001), g.mapRepo(gi.ID, 1002), g.mapRepo(gi.ID, 1003)
	if r1.HookState != "active" || r1.RepoKey != "1001" || r1.RepoPath != "acme/repo-1001" || h.hookCount(1001) != 1 {
		t.Fatalf("map = %+v, hooks %d", r1, h.hookCount(1001))
	}
	s0 := h.secret(1001)
	if len(s0) < 32 || h.secret(1002) != s0 || h.secret(1003) != s0 || g.pingStatus(s0) != 200 {
		t.Fatalf("the hooks must hold the integration's secret (%d)", len(s0))
	}
	// Mapping the same repo again is 409, and makes no second hook.
	if r := g.do("POST", g.path(gi.ID, "/repos"), g.chen, map[string]any{"repoId": "1001"}); r.status != http.StatusConflict || r.errCode(t) != "conflict" {
		t.Fatalf("second map: %d %s", r.status, r.body)
	}
	if h.hookCount(1001) != 1 {
		t.Fatal("a second hook was made")
	}

	// A rotation with one failing hook keeps both secrets accepted.
	h.fail(1002, 500)
	rot := g.rotate(gi.ID, http.StatusOK, nil)
	p := h.secret(1001)
	if rot.Committed || !rot.SecretPending || rot.WebhookSecret != "" || p == s0 || h.secret(1003) != p || h.secret(1002) != s0 {
		t.Fatalf("rotate = %+v (host secrets %q %q %q)", rot, p, h.secret(1002), h.secret(1003))
	}
	if len(rot.FailedRepos) != 1 || rot.FailedRepos[0].ID != r2.ID || rot.FailedRepos[0].HookState != "failed" ||
		rot.FailedRepos[0].HookError == nil || *rot.FailedRepos[0].HookError != "provider_error" || rot.Status != "needs_attention" ||
		rot.AttentionCode == nil || *rot.AttentionCode != "hook_failed" {
		t.Fatalf("failed repos = %+v, status %s", rot.FailedRepos, rot.Status)
	}
	if g.pingStatus(s0) != 200 || g.pingStatus(p) != 200 {
		t.Fatal("with a failed hook both secrets must stay accepted (no delivery lost)")
	}
	if strings.Contains(g.logs.String(), p) || strings.Contains(g.logs.String(), "host words") {
		t.Fatal("a secret or host text is in the log")
	}
	st := g.hookStates(gi.ID)
	if st["1001"] != "active" || st["1002"] != "failed:provider_error" || st["1003"] != "active" {
		t.Fatalf("hook states = %v", st)
	}

	// A retry reuses the pending secret and calls only the failed repo.
	before1, before3 := h.Count("PATCH /api/v3/repositories/1001/"), h.Count("PATCH /api/v3/repositories/1003/")
	rot = g.rotate(gi.ID, http.StatusOK, nil)
	if rot.Committed || h.secret(1001) != p || h.secret(1003) != p || h.Count("PATCH /api/v3/repositories/1001/") != before1 || h.Count("PATCH /api/v3/repositories/1003/") != before3 {
		t.Fatal("a retry must reuse the pending secret and touch only the repos that failed")
	}
	// Mapped while pending: the new hook gets the pending secret. (1002 is failing; use a fourth repo.)
	h.mu.Lock()
	h.ids = append(h.ids, 1004)
	h.mu.Unlock()
	g.mapRepo(gi.ID, 1004)
	if h.secret(1004) != p {
		t.Fatal("a repo mapped during a rotation must get the pending secret")
	}

	// The host recovers: the retry commits.
	h.fail(1002, 0)
	rot = g.rotate(gi.ID, http.StatusOK, nil)
	if !rot.Committed || rot.SecretPending || len(rot.FailedRepos) != 0 || rot.AttentionCode != nil || rot.Status != "ok" {
		t.Fatalf("final rotate = %+v", rot)
	}
	if h.secret(1002) != p || g.pingStatus(s0) != 401 || g.pingStatus(p) != 200 {
		t.Fatal("after the commit only the new secret may verify, and every hook holds it")
	}
	for k, v := range g.hookStates(gi.ID) {
		if v != "active" {
			t.Fatalf("repo %s is %s", k, v)
		}
	}
	// The new secret is the sealed current one.
	var cur []byte
	if err := g.pool.QueryRow(context.Background(), `SELECT webhook_secret_enc FROM git_integrations WHERE id = $1`, gi.ID).Scan(&cur); err != nil {
		t.Fatal(err)
	}
	if pt, err := g.kr.OpenFor("webhook_secret_enc", gi.ID.String(), cur); err != nil || string(pt) != p {
		t.Fatalf("committed secret: %v", err)
	}

	// Discard in token mode: hooks that moved are marked; a repo that failed
	// for another reason stays failed.
	h.fail(1002, 500)
	g.rotate(gi.ID, http.StatusOK, nil)
	p2 := h.secret(1001)
	g.must(http.StatusOK, g.do("DELETE", g.path(gi.ID, "/rotate-secret"), g.chen, nil))
	st = g.hookStates(gi.ID)
	if st["1001"] != "failed:secret_discarded" || st["1002"] != "failed:provider_error" || g.pingStatus(p2) != 401 || g.pingStatus(p) != 200 {
		t.Fatalf("after discard: %v", st)
	}
	// A repo the loop never reached ("not_updated") is not trusted back to
	// active (LAUN-92): the host may hold the dropped secret, so it is failed.
	h.fail(1002, 0)
	h.fail(1001, 401)
	g.rotate(gi.ID, http.StatusOK, nil)
	if st = g.hookStates(gi.ID); st["1001"] != "failed:provider_unauthorized" || st["1002"] != "failed:not_updated" {
		t.Fatalf("after a 401 stop: %v", st)
	}
	g.must(http.StatusOK, g.do("DELETE", g.path(gi.ID, "/rotate-secret"), g.chen, nil))
	if st = g.hookStates(gi.ID); st["1001"] != "failed:provider_unauthorized" || st["1002"] != "failed:secret_discarded" || st["1003"] != "failed:secret_discarded" {
		t.Fatalf("after discarding a stopped rotation: %v", st)
	}
	_ = r3
}

// Hook errors map to the provider_* codes, on map and on rotate; an unmap
// deletes the hook by the map row id and tells whether it is gone.
func TestGitRepos_TokenModeHostErrorsAndUnmap(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	h := newRepoHost(t, 1001, 1002, 1003, 1004)
	gi := g.tokenCreate(&fakeHost{Fake: h.Fake})

	for _, c := range []struct {
		status int
		want   int
		code   string
	}{{401, 502, "provider_unauthorized"}, {403, 502, "provider_forbidden"}, {429, 503, "provider_rate_limited"}, {500, 502, "provider_error"}} {
		h.fail(1001, c.status)
		r := g.do("POST", g.path(gi.ID, "/repos"), g.chen, map[string]any{"repoId": "1001"})
		if r.status != c.want || r.errCode(t) != c.code || strings.Contains(string(r.body), "host words") {
			t.Fatalf("host %d: %d %s", c.status, r.status, r.body)
		}
		if c.status == 429 && r.header.Get("Retry-After") == "" {
			t.Fatal("no Retry-After")
		}
		if len(g.repos(gi.ID)) != 0 {
			t.Fatalf("a hook that fails leaves no row (host %d)", c.status)
		}
	}
	h.fail(1001, 0)
	// A bad or unknown repoId is a 400.
	g.must(http.StatusBadRequest, g.do("POST", g.path(gi.ID, "/repos"), g.chen, map[string]any{"repoId": "not-a-number"}))
	g.must(http.StatusBadRequest, g.do("POST", g.path(gi.ID, "/repos"), g.chen, map[string]any{"repoId": "9999"}))
	// The key and the path come from the host, never from the body.
	g.must(http.StatusBadRequest, g.do("POST", g.path(gi.ID, "/repos"), g.chen, map[string]any{"repoId": "1001", "repoKey": "7", "repoPath": "evil/x"}))
	g.must(http.StatusBadRequest, g.do("POST", g.path(gi.ID, "/repos"), g.chen, map[string]any{}))

	a := g.mapRepo(gi.ID, 1001)
	if h.hookCount(1001) != 1 {
		t.Fatal("one hook")
	}
	g.must(http.StatusOK, g.do("DELETE", g.path(gi.ID, "/repos/"+a.ID.String()), g.chen, nil))
	if h.hookCount(1001) != 0 {
		t.Fatal("unmap must delete the hook")
	}
	b := g.mapRepo(gi.ID, 1001)
	if h.hookCount(1001) != 1 {
		t.Fatalf("remap: %d hooks", h.hookCount(1001))
	}

	// Unmap deletes by the stored hook id.
	var hookID string
	if err := g.pool.QueryRow(context.Background(), `SELECT hook_id FROM git_repo_map WHERE id = $1`, b.ID).Scan(&hookID); err != nil {
		t.Fatal(err)
	}
	var rm tracker.GitRepoRemoved
	g.must(http.StatusOK, g.do("DELETE", g.path(gi.ID, "/repos/"+b.ID.String()), g.chen, nil)).json(t, &rm)
	if !rm.HookRemoved || h.Count("DELETE /api/v3/repositories/1001/hooks/"+hookID) != 1 {
		t.Fatalf("removed = %+v, calls %v", rm, h.Calls())
	}
	g.must(http.StatusNotFound, g.do("DELETE", g.path(gi.ID, "/repos/"+b.ID.String()), g.chen, nil))

	// A host 404 counts as done; a host 5xx leaves the hook and the row is gone.
	for _, c := range []struct {
		status  int
		removed bool
		repo    int
	}{{404, true, 1002}, {500, false, 1003}, {403, false, 1004}} {
		m := g.mapRepo(gi.ID, c.repo)
		h.mu.Lock()
		h.delStatus = c.status
		h.mu.Unlock()
		g.must(http.StatusOK, g.do("DELETE", g.path(gi.ID, "/repos/"+m.ID.String()), g.chen, nil)).json(t, &rm)
		if rm.HookRemoved != c.removed {
			t.Fatalf("host %d: hookRemoved = %v", c.status, rm.HookRemoved)
		}
		h.mu.Lock()
		h.delStatus = 0
		h.mu.Unlock()
		if n := g.count(`SELECT count(*) FROM git_repo_map WHERE id = $1`, m.ID); n != 0 {
			t.Fatalf("host %d: the row is still there", c.status)
		}
	}
	// A 403 from the host sets token_rejected.
	if gi2 := g.integration(gi.ID); gi2.AttentionCode == nil || *gi2.AttentionCode != "token_rejected" {
		t.Fatalf("attention = %v", gi2.AttentionCode)
	}
}

// available-repos: one page of at most 50, an opaque cursor, the mapped flag.
func TestGitRepos_AvailableRepos(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	ids := make([]int, 0, 120)
	for i := 1; i <= 120; i++ {
		ids = append(ids, 2000+i)
	}
	h := newRepoHost(t, ids...)
	h.badURL[2005] = "javascript:alert(1)"
	gi := g.tokenCreate(&fakeHost{Fake: h.Fake})
	g.mapRepo(gi.ID, 2003)

	var p1 httpPage
	g.must(http.StatusOK, g.do("GET", g.path(gi.ID, "/available-repos"), g.chen, nil)).json(t, &p1)
	if len(p1.Items) != 50 || p1.NextCursor == nil || strings.Contains(*p1.NextCursor, "http") {
		t.Fatalf("page 1: %d items, cursor %v", len(p1.Items), p1.NextCursor)
	}
	mapped := 0
	for _, it := range p1.Items {
		if (it.RepoKey == "2005") != (it.URL == "") {
			t.Fatalf("url %q of repo %s: only a non-http URL may be blanked", it.URL, it.RepoKey)
		}
		if it.Mapped {
			mapped++
			if it.RepoKey != "2003" {
				t.Fatalf("mapped = %+v", it)
			}
		}
	}
	if mapped != 1 {
		t.Fatalf("mapped flags = %d", mapped)
	}
	var p2 httpPage
	g.must(http.StatusOK, g.do("GET", g.path(gi.ID, "/available-repos?cursor="+*p1.NextCursor), g.chen, nil)).json(t, &p2)
	if len(p2.Items) != 50 || p2.Items[0].RepoKey == p1.Items[0].RepoKey {
		t.Fatalf("page 2: %d items", len(p2.Items))
	}
	g.must(http.StatusBadRequest, g.do("GET", g.path(gi.ID, "/available-repos?cursor=https://evil.example/x"), g.chen, nil))
	g.must(http.StatusBadRequest, g.do("GET", g.path(gi.ID, "/available-repos?cursor=bm90LWEtY3Vyc29y"), g.chen, nil))
	// Webhook-only has no token: 409.
	var web tracker.GitIntegration
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "github", "baseUrl": "https://github.com"})).json(t, &web)
	g.must(http.StatusConflict, g.do("GET", g.path(web.ID, "/available-repos"), g.chen, nil))
	if r := g.do("GET", g.path(gi.ID, "/available-repos"), g.claude, nil); r.status != http.StatusForbidden {
		t.Fatalf("a non-admin: %d", r.status)
	}
}

type httpPage struct {
	Items      []tracker.AvailableRepo `json:"items"`
	NextCursor *string                 `json:"nextCursor"`
}

// The webhook-only repo map: validation, 409 on a second map, team patch, and
// a cross-org and a non-admin check on every route.
func TestGitRepos_WebhookOnlyAndCrossOrg(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	var gi tracker.GitIntegration
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "gitlab", "baseUrl": glBaseURL})).json(t, &gi)
	repos := g.path(gi.ID, "/repos")
	for _, body := range []map[string]any{
		{"repoKey": "abc", "repoPath": "x"}, {"repoKey": "0x1", "repoPath": "x"}, {"repoKey": "42", "repoPath": ""},
		{"repoKey": "42", "repoPath": "x", "teamId": "NOPE"}, {"repoKey": "42", "repoPath": "x", "repoId": "42"}, {"repoPath": "x"},
		{"repoKey": "42", "repoPath": "bad\x07path"},
	} {
		g.must(http.StatusBadRequest, g.do("POST", repos, g.chen, body))
	}
	var r tracker.GitRepo
	g.must(http.StatusCreated, g.do("POST", repos, g.chen, map[string]any{"repoKey": "42", "repoPath": "example-org/example-app"})).json(t, &r)
	if r.RepoKey != "42" || r.HookState != "none" || r.Team != nil || r.IntegrationID != gi.ID {
		t.Fatalf("repo = %+v", r)
	}
	if x := g.do("POST", repos, g.chen, map[string]any{"repoKey": "42", "repoPath": "again"}); x.status != http.StatusConflict || x.errCode(t) != "conflict" {
		t.Fatalf("second map: %d %s", x.status, x.body)
	}
	// The team: set by key, set by id, cleared.
	var withTeam tracker.GitRepo
	g.must(http.StatusOK, g.do("PATCH", repos+"/"+r.ID.String(), g.chen, map[string]any{"teamId": "orb"})).json(t, &withTeam)
	if withTeam.Team == nil || withTeam.Team.Key != "ORB" {
		t.Fatalf("team = %+v", withTeam.Team)
	}
	g.must(http.StatusOK, g.do("PATCH", repos+"/"+r.ID.String(), g.chen, map[string]any{"teamId": withTeam.Team.ID.String()}))
	g.must(http.StatusOK, g.do("PATCH", repos+"/"+r.ID.String(), g.chen, map[string]any{"teamId": nil})).json(t, &withTeam)
	if withTeam.Team != nil {
		t.Fatal("team not cleared")
	}
	g.must(http.StatusBadRequest, g.do("PATCH", repos+"/"+r.ID.String(), g.chen, map[string]any{"teamId": "NOPE"}))
	g.must(http.StatusBadRequest, g.do("PATCH", repos+"/"+r.ID.String(), g.chen, map[string]any{}))
	g.must(http.StatusNotFound, g.do("PATCH", repos+"/"+uuid.NewString(), g.chen, map[string]any{"teamId": nil}))
	if got := g.repos(gi.ID); len(got) != 1 || got[0].ID != r.ID {
		t.Fatalf("list = %+v", got)
	}
	// A webhook-only integration makes no hook: unmap says so.
	// (kept below, after the cross-org checks)

	// Another org: its integration through orbit's paths is 404 on every route.
	org2 := g.newOrg("acme", "ACME")
	admin2 := g.newUser("acmeadmin")
	g.addMember(org2, admin2.ID, "admin")
	tok2, _ := g.newToken(admin2.ID, tokenOpts{pin: &org2})
	var gi2 tracker.GitIntegration
	g.must(http.StatusCreated, g.do("POST", "/api/v1/orgs/acme/integrations", tok2, map[string]any{"provider": "gitlab", "baseUrl": glBaseURL, "secret": glSecret})).json(t, &gi2)
	var r2 tracker.GitRepo
	g.must(http.StatusCreated, g.do("POST", "/api/v1/orgs/acme/integrations/"+gi2.ID.String()+"/repos", tok2, map[string]any{"repoKey": "42", "repoPath": "acme/x"})).json(t, &r2)
	other := gitBase + "/" + gi2.ID.String()
	for _, c := range []struct{ method, path string }{
		{"GET", other + "/repos"}, {"POST", other + "/repos"}, {"PATCH", other + "/repos/" + r2.ID.String()},
		{"DELETE", other + "/repos/" + r2.ID.String()}, {"GET", other + "/available-repos"},
		{"POST", other + "/rotate-secret"}, {"DELETE", other + "/rotate-secret"}, {"POST", other + "/rotate-secret/commit"},
		{"PATCH", repos + "/" + r2.ID.String()}, {"DELETE", repos + "/" + r2.ID.String()},
	} {
		var body any
		switch {
		case c.method == "POST" && strings.HasSuffix(c.path, "/repos"):
			body = map[string]any{"repoKey": "7", "repoPath": "x"}
		case c.method == "PATCH":
			body = map[string]any{"teamId": nil}
		}
		g.must(http.StatusNotFound, g.do(c.method, c.path, g.chen, body))
	}
	if g.count(`SELECT count(*) FROM git_repo_map WHERE org_id = $1`, org2) != 1 {
		t.Fatal("another org's mapping was touched")
	}
	if g.count(`SELECT count(*) FROM git_integrations WHERE id = $1 AND webhook_secret_next_hash IS NOT NULL`, gi2.ID) != 0 {
		t.Fatal("another org's secret was rotated")
	}

	// Non-admin tokens (a read-scoped and a member agent) are refused everywhere.
	for _, c := range []struct{ method, path string }{
		{"GET", repos}, {"POST", repos}, {"PATCH", repos + "/" + r.ID.String()}, {"DELETE", repos + "/" + r.ID.String()},
		{"GET", g.path(gi.ID, "/available-repos")}, {"POST", g.path(gi.ID, "/rotate-secret")},
		{"DELETE", g.path(gi.ID, "/rotate-secret")}, {"POST", g.path(gi.ID, "/rotate-secret/commit")},
	} {
		g.must(http.StatusForbidden, g.do(c.method, c.path, g.claude, map[string]any{}))
	}

	var rm tracker.GitRepoRemoved
	g.must(http.StatusOK, g.do("DELETE", repos+"/"+r.ID.String(), g.chen, nil)).json(t, &rm)
	if rm.HookRemoved {
		t.Fatal("webhook-only: hookRemoved must be false (the admin owns the hook)")
	}
}

// Two integrations of one provider in an org may not share a secret, as the
// current or the pending one.
func TestGitRotate_DuplicateSecret(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	var a, b tracker.GitIntegration
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "gitlab", "baseUrl": glBaseURL, "secret": glSecret})).json(t, &a)
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "gitlab", "baseUrl": "https://gitlab.other.example"})).json(t, &b)
	if r := g.do("POST", g.path(b.ID, "/rotate-secret"), g.chen, map[string]any{"secret": glSecret}); r.status != http.StatusConflict || r.errCode(t) != "secret_in_use" {
		t.Fatalf("duplicate current secret: %d %s", r.status, r.body)
	}
	// A's pending secret is taken too.
	pending := "pending-of-a-0123456789abcdef0123456789"
	g.rotate(a.ID, http.StatusOK, map[string]any{"secret": pending})
	if r := g.do("POST", g.path(b.ID, "/rotate-secret"), g.chen, map[string]any{"secret": pending}); r.status != http.StatusConflict || r.errCode(t) != "secret_in_use" {
		t.Fatalf("duplicate pending secret: %d %s", r.status, r.body)
	}
	// The same integration may rotate to a new secret, but not to its own current one.
	if r := g.do("POST", g.path(a.ID, "/rotate-secret"), g.chen, map[string]any{"secret": glSecret}); r.status != http.StatusConflict {
		t.Fatalf("rotate to the current secret: %d %s", r.status, r.body)
	}
	// Another provider may use the same secret value (the check is per provider).
	var gh tracker.GitIntegration
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "github", "baseUrl": "https://github.com"})).json(t, &gh)
}

// The rate limit of #28 covers rotate, available-repos and a token-mode map.
func TestGitRotate_RateLimit(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{limited: true})
	h := newRepoHost(t, 1001)
	gi := g.tokenCreate(&fakeHost{Fake: h.Fake})
	for _, c := range []struct {
		name, method, path string
		body               any
	}{
		{"rotate", "POST", g.path(gi.ID, "/rotate-secret"), nil},
		{"available-repos", "GET", g.path(gi.ID, "/available-repos"), nil},
		{"map", "POST", g.path(gi.ID, "/repos"), map[string]any{"repoId": "1001"}},
	} {
		g.server.gitLimit.mu.Lock()
		g.server.gitLimit.buckets = map[string]*gitBucket{}
		g.server.gitLimit.mu.Unlock()
		var last resp
		for range 4 {
			last = g.do(c.method, c.path, g.chen, c.body)
		}
		if last.status != http.StatusTooManyRequests || last.errCode(t) != "rate_limited" || last.header.Get("Retry-After") == "" {
			t.Fatalf("%s: fourth call = %d %s", c.name, last.status, last.body)
		}
	}
	// Unmap takes the allowance before its hook delete; the row survives a refusal.
	var rows []tracker.GitRepo
	h.mu.Lock()
	h.ids = append(h.ids, 1002, 1003, 1004)
	h.mu.Unlock()
	for _, id := range []int{1001, 1002, 1003, 1004} {
		g.server.gitLimit.mu.Lock()
		g.server.gitLimit.buckets = map[string]*gitBucket{}
		g.server.gitLimit.mu.Unlock()
		if id == 1001 && len(g.repos(gi.ID)) == 1 {
			continue // mapped by the loop above
		}
		rows = append(rows, g.mapRepo(gi.ID, id))
	}
	rows = g.repos(gi.ID)
	g.server.gitLimit.mu.Lock()
	g.server.gitLimit.buckets = map[string]*gitBucket{}
	g.server.gitLimit.mu.Unlock()
	var last resp
	for _, r := range rows {
		last = g.do("DELETE", g.path(gi.ID, "/repos/"+r.ID.String()), g.chen, nil)
	}
	if last.status != http.StatusTooManyRequests || last.errCode(t) != "rate_limited" || len(g.repos(gi.ID)) != 1 {
		t.Fatalf("fourth unmap = %d %s, %d rows left", last.status, last.body, len(g.repos(gi.ID)))
	}
}

// A discard while the push loop runs: the repos pushed meanwhile hold a secret
// nobody accepts, so they are marked, and nothing is committed.
func TestGitRotate_DiscardDuringPush(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	h := newRepoHost(t, 1001, 1002)
	gi := g.tokenCreate(&fakeHost{Fake: h.Fake})
	g.mapRepo(gi.ID, 1001)
	g.mapRepo(gi.ID, 1002)
	s0 := h.secret(1001)
	h.onWrite = func() { g.must(http.StatusOK, g.do("DELETE", g.path(gi.ID, "/rotate-secret"), g.chen, nil)) }
	rot := g.rotate(gi.ID, http.StatusOK, nil)
	st := g.hookStates(gi.ID)
	if rot.Committed || rot.SecretPending || len(rot.FailedRepos) != 2 || st["1001"] != "failed:secret_discarded" || st["1002"] != "failed:secret_discarded" {
		t.Fatalf("rotate = %+v, states %v", rot, st)
	}
	if p := h.secret(1001); p == s0 || g.pingStatus(p) != 401 || g.pingStatus(s0) != 200 {
		t.Fatal("only the current secret may verify after the discard")
	}
}

// A manual commit in token mode needs every hook on the pending secret unless
// forced; webhook-only commit is not checked.
func TestGitRotate_ManualCommitTokenMode(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	h := newRepoHost(t, 1001, 1002)
	gi := g.tokenCreate(&fakeHost{Fake: h.Fake})
	g.mapRepo(gi.ID, 1001)
	g.mapRepo(gi.ID, 1002)
	s0 := h.secret(1001)
	h.fail(1002, 500)
	g.rotate(gi.ID, http.StatusOK, nil)
	p := h.secret(1001)
	commit := g.path(gi.ID, "/rotate-secret/commit")
	if r := g.do("POST", commit, g.chen, nil); r.status != http.StatusConflict || r.errCode(t) != "conflict" || !strings.Contains(string(r.body), "acme/repo-1002") {
		t.Fatalf("commit with a stale hook: %d %s", r.status, r.body)
	}
	if g.pingStatus(s0) != 200 || !g.integration(gi.ID).SecretPending {
		t.Fatal("a refused commit must change nothing")
	}
	g.must(http.StatusBadRequest, g.do("POST", commit, g.chen, map[string]any{"force": "yes"}))
	var after tracker.GitIntegration
	g.must(http.StatusOK, g.do("POST", commit, g.chen, map[string]any{"force": true})).json(t, &after)
	if after.SecretPending || g.pingStatus(s0) != 401 || g.pingStatus(p) != 200 {
		t.Fatal("force must commit")
	}
}

// No secret reaches a log line, an event or an idempotency row; the config
// routes emit no event.
func TestGitRotate_NoSecretsInLogsEventsOrIdempotency(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	h := newRepoHost(t, 1001)
	events := g.count(`SELECT count(*) FROM events`)
	gi := g.tokenCreate(&fakeHost{Fake: h.Fake})
	g.mapRepo(gi.ID, 1001)
	s0 := h.secret(1001)
	rot := g.rotate(gi.ID, http.StatusOK, nil)
	p := h.secret(1001)
	if !rot.Committed {
		t.Fatalf("rotate = %+v", rot)
	}
	var web tracker.GitIntegrationCreated
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "github", "baseUrl": "https://github.com"})).json(t, &web)
	w1 := g.rotate(web.ID, http.StatusOK, nil).WebhookSecret
	g.must(http.StatusOK, g.do("POST", g.path(web.ID, "/rotate-secret/commit"), g.chen, nil))
	g.do("POST", g.path(web.ID, "/rotate-secret"), g.chen, nil, "Idempotency-Key", "k1")
	g.do("POST", g.path(gi.ID, "/repos"), g.chen, map[string]any{"repoId": "1001"})

	logs := g.logs.String()
	for _, s := range []string{s0, p, w1, web.WebhookSecret, gitToken, "host words"} {
		if s != "" && strings.Contains(logs, s) {
			t.Fatalf("a secret or host text is in the server log: %.8s", s)
		}
	}
	if n := g.count(`SELECT count(*) FROM events`); n != events {
		t.Fatalf("the routes emitted %d events (credentials and the repo map emit none)", n-events)
	}
	if n := g.count(`SELECT count(*) FROM idempotency_keys`); n != 0 {
		t.Fatalf("idempotency rows = %d (rotate-secret rejects the key)", n)
	}
	for _, s := range []string{s0, p, w1, web.WebhookSecret} {
		if rawContains(g, `SELECT count(*) FROM git_integrations g WHERE g::text LIKE '%' || $1 || '%'`, s) ||
			rawContains(g, `SELECT count(*) FROM git_repo_map g WHERE g::text LIKE '%' || $1 || '%'`, s) {
			t.Fatalf("plaintext %.8s is stored", s)
		}
	}
}

func hashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum)
}

// A newer rotation during the push: this run's secret is current but another
// secret is pending, so the hooks pushed here must not stay "active".
func TestGitRotate_SupersededDuringPush(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	h := newRepoHost(t, 1001, 1002)
	gi := g.tokenCreate(&fakeHost{Fake: h.Fake})
	g.mapRepo(gi.ID, 1001)
	g.mapRepo(gi.ID, 1002)
	h.onWrite = func() {
		// Commit this run's secret and start a newer one (as the routes would).
		if _, err := g.pool.Exec(context.Background(), `UPDATE git_integrations SET webhook_secret_hash = webhook_secret_next_hash,
			webhook_secret_next_hash = sha256('newer'::bytea), updated_at = now() WHERE id = $1`, gi.ID); err != nil {
			t.Error(err)
		}
	}
	rot := g.rotate(gi.ID, http.StatusOK, nil)
	st := g.hookStates(gi.ID)
	if rot.Committed || st["1001"] != "failed:secret_discarded" || st["1002"] != "failed:secret_discarded" {
		t.Fatalf("rotate = %+v, states %v", rot, st)
	}
}

// An empty chunked body (no Content-Length) is an empty body, not a 400.
func TestGitRotate_EmptyChunkedBody(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	var gi tracker.GitIntegrationCreated
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "github", "baseUrl": "https://github.com"})).json(t, &gi)
	send := func(path string) int {
		req, err := http.NewRequest("POST", g.srv.URL+g.path(gi.ID, path), io.NopCloser(strings.NewReader("")))
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = -1 // sent chunked, no Content-Length
		req.Header.Set("Authorization", "Bearer "+g.chen)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	if st := send("/rotate-secret"); st != http.StatusOK {
		t.Fatalf("rotate: %d", st)
	}
	if st := send("/rotate-secret/commit"); st != http.StatusOK {
		t.Fatalf("commit: %d", st)
	}
}
