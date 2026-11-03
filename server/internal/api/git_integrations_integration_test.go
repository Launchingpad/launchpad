package api

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/gitapi"
	"devops/internal/gitapi/clients"
	"devops/internal/gitapi/gitapitest"
	"devops/internal/github"
	"devops/internal/netguard"
	"devops/internal/netguard/netguardtest"
	"devops/internal/platform"
	"devops/internal/scm"
	"devops/internal/tracker"
)

// The git integration config routes (T17, LAUN-7).

const (
	gitBase    = "/api/v1/orgs/orbit/integrations"
	gitToken   = "ghp_launchpad_test_token_0123456789"
	gitToken2  = "ghp_launchpad_second_token_0123456789"
	gitPublic  = "https://tracker.example.com"
	gitAcmeKey = "acme-secret-0123456789abcdef0123456789"
)

// fakeHost is a git host on 127.0.0.1 that answers /user (GitHub Enterprise
// shape: API root <base>/api/v3) with a status the test sets.
type fakeHost struct {
	*gitapitest.Fake
	status  atomic.Int32 // HTTP status of GET /api/v3/user
	delay   atomic.Int64 // nanoseconds
	lastTok atomic.Value // the last Authorization header
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	h := &fakeHost{}
	h.status.Store(200)
	h.Fake = gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/v3/user" {
			return false
		}
		h.lastTok.Store(r.Header.Get("Authorization"))
		if d := time.Duration(h.delay.Load()); d > 0 {
			time.Sleep(d)
		}
		st := int(h.status.Load())
		if st == 429 {
			w.Header().Set("Retry-After", "2")
		}
		if st != 200 {
			gitapitest.JSON(w, st, map[string]string{"message": "secret host text must never be echoed"})
			return true
		}
		w.Header().Set("X-OAuth-Scopes", "repo, admin:repo_hook")
		gitapitest.JSON(w, 200, map[string]string{"login": "octo", "name": "Octo"})
		return true
	})
	return h
}

func (h *fakeHost) url() string { return h.Srv.URL }

type gitEnv struct {
	*env
	kr     *platform.GitKeyring
	res    *netguardtest.MapResolver
	server *Server
}

type gitEnvOpts struct {
	noKey      bool
	publicBase string // default gitPublic
	nonLocal   bool   // LOG_ENV is not local or test
	policy     *netguard.Policy
	limited    bool // keep the real rate limit of T17 #28 (default: a test limit of 1000)
	// wrapClient, when set, wraps every API client the server builds (LAUN-92 tests).
	wrapClient func(scm.APIClient) scm.APIClient
}

func newGitEnv(t *testing.T, o gitEnvOpts) *gitEnv {
	t.Helper()
	g := &gitEnv{kr: ghKeyring(t, 1), res: &netguardtest.MapResolver{Hosts: map[string][]string{}}}
	base := o.publicBase
	switch base {
	case "":
		base = gitPublic
	case "-":
		base = "" // no PUBLIC_BASE_URL
	}
	g.env = newEnvWith(t, envOpts{logLevel: slog.LevelDebug, publicBase: base, configure: func(_ *env, s *Server) {
		g.server = s
		if !o.noKey {
			s.svc.SetGitSealer(g.kr)
		}
		if !o.limited {
			s.gitLimit.burst, s.gitLimit.perMin = 1000, 6000
		}
		base, err := netguard.GitPolicy(false, "", nil) // the production policy: names and ranges
		if err != nil {
			t.Fatal(err)
		}
		pol := netguardtest.Permit(base, "127.0.0.1")
		if o.policy != nil {
			pol = *o.policy
		}
		opts := gitapitest.Options(nil)
		opts.RequestTimeout = 400 * time.Millisecond
		opts.HTTP = gitapi.NewHTTPClient(&netguard.Dialer{Policy: pol, Resolver: g.res})
		newClient := clients.Factory(opts)
		if o.wrapClient != nil {
			inner := newClient
			newClient = func(provider, root, token string) (scm.APIClient, error) {
				c, err := inner(provider, root, token)
				if err != nil {
					return nil, err
				}
				return o.wrapClient(c), nil
			}
		}
		s.svc.SetGitNet(&tracker.GitNet{Policy: pol, Resolver: g.res, NewClient: newClient, Local: !o.nonLocal})
	}})
	return g
}

func (g *gitEnv) create(body map[string]any) resp {
	g.t.Helper()
	return g.do("POST", gitBase, g.chen, body)
}

// tokenCreate makes a token-mode GitHub (Enterprise shape) integration on host.
func (g *gitEnv) tokenCreate(h *fakeHost) tracker.GitIntegrationCreated {
	g.t.Helper()
	var gi tracker.GitIntegrationCreated
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "github", "baseUrl": h.url(), "mode": "token", "token": gitToken})).json(g.t, &gi)
	return gi
}

func rawContains(g *gitEnv, query string, args ...any) bool {
	g.t.Helper()
	var n int
	if err := g.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		g.t.Fatal(err)
	}
	return n > 0
}

// A webhook-only create shows the secret exactly once; the secret then opens
// the webhook; nothing else ever returns it.
func TestGitIntegrations_WebhookOnlyShowsSecretOnce(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	r := g.must(http.StatusCreated, g.create(map[string]any{"provider": "github", "baseUrl": "https://github.com", "name": "GitHub"}))
	var gi tracker.GitIntegrationCreated
	r.json(t, &gi)
	if len(gi.WebhookSecret) < 32 {
		t.Fatalf("webhookSecret = %q, want a server-made secret", gi.WebhookSecret)
	}
	if gi.Provider != "github" || gi.Mode != "webhook_only" || gi.Status != "ok" || gi.TokenSet || !gi.WebhookSecretSet ||
		gi.WebhookURL != gitPublic+"/api/v1/orgs/orbit/integrations/github/webhook" || gi.Name != "GitHub" || gi.RepoCount != 0 ||
		gi.Flavor == nil || *gi.Flavor != "cloud" {
		t.Fatalf("integration = %+v", gi)
	}
	// The secret is in no other response.
	for _, r := range []resp{
		g.must(http.StatusOK, g.do("GET", gitBase, g.chen, nil)),
		g.must(http.StatusOK, g.do("GET", gitBase+"/"+gi.ID.String(), g.chen, nil)),
		g.must(http.StatusOK, g.do("PATCH", gitBase+"/"+gi.ID.String(), g.chen, map[string]any{"name": "GH"})),
	} {
		if strings.Contains(string(r.body), gi.WebhookSecret) || strings.Contains(string(r.body), `"webhookSecret"`) {
			t.Fatalf("a later response shows the secret: %s", r.body)
		}
	}
	// It is the secret of the webhook: a signed ping is accepted, a wrong key is 401.
	gh := &ghEnv{env: g.env}
	gh.must(http.StatusOK, gh.ghHook("orbit", gi.WebhookSecret, uuid.NewString(), []byte("{}"), github.HeaderEvent, "ping"))
	gh.must(http.StatusUnauthorized, gh.ghHook("orbit", gi.WebhookSecret+"x", uuid.NewString(), []byte("{}"), github.HeaderEvent, "ping"))
	// Sealed at rest, bound to the row (AAD); the hash is kept.
	var blob []byte
	if err := g.pool.QueryRow(context.Background(), `SELECT webhook_secret_enc FROM git_integrations WHERE id = $1`, gi.ID).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte(gi.WebhookSecret)) {
		t.Fatal("the secret is stored in the clear")
	}
	if pt, err := g.kr.OpenFor("webhook_secret_enc", gi.ID.String(), blob); err != nil || string(pt) != gi.WebhookSecret {
		t.Fatalf("sealed secret does not open under its row: %v", err)
	}
	if _, err := g.kr.OpenFor("webhook_secret_enc", uuid.NewString(), blob); err == nil {
		t.Fatal("the sealed secret opens under another row id")
	}
	// GitHub takes no pasted secret; a GitLab one is accepted and not returned.
	g.must(http.StatusBadRequest, g.create(map[string]any{"provider": "github", "baseUrl": "https://ghe.example.com", "secret": glSecret}))
	r = g.must(http.StatusCreated, g.create(map[string]any{"provider": "gitlab", "baseUrl": glBaseURL, "secret": glSecret}))
	if strings.Contains(string(r.body), glSecret) || strings.Contains(string(r.body), `"webhookSecret"`) {
		t.Fatalf("a pasted secret came back: %s", r.body)
	}
	// A GitLab integration with a server-made secret: shown once, works as X-Gitlab-Token.
	var gl tracker.GitIntegrationCreated
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "gitlab", "baseUrl": "https://gitlab.other.example"})).json(t, &gl)
	if len(gl.WebhookSecret) < 32 {
		t.Fatalf("gitlab webhookSecret = %q", gl.WebhookSecret)
	}
	if r := g.hook("orbit", gl.WebhookSecret, uuid.NewString(), []byte("{}")); r.status == http.StatusUnauthorized { // past the token check (the empty body is a 400)
		t.Fatalf("the made GitLab secret does not open the webhook: %d %s", r.status, r.body)
	}
}

func TestGitIntegrations_Validation(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	for name, body := range map[string]map[string]any{
		"no provider":       {"baseUrl": glBaseURL},
		"bitbucket bad url": {"provider": "bitbucket", "baseUrl": "bitbucket.org"},
		"unknown":           {"provider": "gitea", "baseUrl": glBaseURL},
		"bad mode":          {"provider": "gitlab", "baseUrl": glBaseURL, "mode": "magic"},
		"bad url":           {"provider": "gitlab", "baseUrl": "gitlab.example.com"},
		"url with query":    {"provider": "gitlab", "baseUrl": glBaseURL + "/?a=1"},
		"url with user":     {"provider": "gitlab", "baseUrl": "https://u:p@gitlab.example.com"},
		"short secret":      {"provider": "gitlab", "baseUrl": glBaseURL, "secret": "short"},
		"spaced secret":     {"provider": "gitlab", "baseUrl": glBaseURL, "secret": strings.Repeat("ab ", 12)},
		"token no mode":     {"provider": "gitlab", "baseUrl": glBaseURL, "token": "x"},
		"token mode empty":  {"provider": "gitlab", "baseUrl": glBaseURL, "mode": "token"},
		"token + secret":    {"provider": "gitlab", "baseUrl": glBaseURL, "mode": "token", "token": "x", "secret": glSecret},
		"bad api url":       {"provider": "gitlab", "baseUrl": glBaseURL, "apiBaseUrl": "nope"},
		"long name":         {"provider": "gitlab", "baseUrl": glBaseURL, "name": strings.Repeat("n", 101)},
	} {
		r := g.create(body)
		if r.status != http.StatusBadRequest || r.errCode(t) != "validation_failed" {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
		if strings.Contains(string(r.body), glSecret) {
			t.Fatalf("%s: the error echoes the secret", name)
		}
	}
	g.must(http.StatusBadRequest, g.do("POST", gitBase, g.chen, map[string]any{"provider": "gitlab", "baseUrl": glBaseURL, "secret": glSecret}, "Idempotency-Key", "k1"))
	// Unknown fields are refused (a typo must not silently drop a token).
	g.must(http.StatusBadRequest, g.create(map[string]any{"provider": "gitlab", "baseUrl": glBaseURL, "tokn": "x"}))
	if n := g.count(`SELECT count(*) FROM git_integrations`); n != 0 {
		t.Fatalf("%d rows after refused creates", n)
	}
	// The same provider and base URL (in any spelling) is 409 conflict; another provider is fine.
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "gitlab", "baseUrl": "https://GitLab.Example.com:443/"}))
	if code := g.must(http.StatusConflict, g.create(map[string]any{"provider": "gitlab", "baseUrl": glBaseURL})).errCode(t); code != "integration_exists" {
		t.Fatalf("duplicate code = %s", code)
	}
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "github", "baseUrl": glBaseURL}))
}

func TestGitIntegrations_EncryptionUnavailable(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{noKey: true})
	h := newFakeHost(t)
	// A GitHub secret and every token need the key.
	for name, body := range map[string]map[string]any{
		"github webhook-only": {"provider": "github", "baseUrl": "https://github.com"},
		"gitlab token":        {"provider": "gitlab", "baseUrl": h.url(), "mode": "token", "token": gitToken},
	} {
		if code := g.must(http.StatusConflict, g.create(body)).errCode(t); code != "encryption_unavailable" {
			t.Fatalf("%s: code %s", name, code)
		}
	}
	if h.Count("") != 0 {
		t.Fatal("the host was called before the key was checked")
	}
	// A GitLab webhook-only integration still works, hash only.
	var gl tracker.GitIntegrationCreated
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "gitlab", "baseUrl": glBaseURL})).json(t, &gl)
	if rawContains(g, `SELECT count(*) FROM git_integrations WHERE id = $1 AND webhook_secret_enc IS NOT NULL`, gl.ID) {
		t.Fatal("a sealed secret without a key")
	}
	if r := g.hook("orbit", gl.WebhookSecret, uuid.NewString(), []byte("{}")); r.status == http.StatusUnauthorized {
		t.Fatalf("hash-only GitLab secret refused: %s", r.body)
	}
	// A token PATCH needs the key too.
	g.must(http.StatusBadRequest, g.do("PATCH", gitBase+"/"+gl.ID.String(), g.chen, map[string]any{"mode": "token"})) // no token yet
	if code := g.must(http.StatusConflict, g.do("PATCH", gitBase+"/"+gl.ID.String(), g.chen, map[string]any{"token": gitToken})).errCode(t); code != "secret_hash_only" && code != "encryption_unavailable" {
		t.Fatalf("token PATCH code = %s", code)
	}
}

func TestGitIntegrations_Limit(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	var ids []string
	for i := 0; i < 8; i++ {
		var gi tracker.GitIntegration
		g.must(http.StatusCreated, g.create(map[string]any{"provider": "gitlab", "baseUrl": fmt.Sprintf("https://gl%d.example.com", i)})).json(t, &gi)
		ids = append(ids, gi.ID.String())
	}
	if code := g.must(http.StatusConflict, g.create(map[string]any{"provider": "gitlab", "baseUrl": "https://gl9.example.com"})).errCode(t); code != "integration_limit" {
		t.Fatalf("ninth create code = %s", code)
	}
	// The limit is per provider; a disabled one does not count.
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "github", "baseUrl": "https://github.com"}))
	var off tracker.GitIntegration
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "gitlab", "baseUrl": "https://gl9.example.com", "enabled": false})).json(t, &off)
	if code := g.must(http.StatusConflict, g.do("PATCH", gitBase+"/"+off.ID.String(), g.chen, map[string]any{"enabled": true})).errCode(t); code != "integration_limit" {
		t.Fatalf("enable code = %s", code)
	}
	g.must(http.StatusOK, g.do("PATCH", gitBase+"/"+ids[0], g.chen, map[string]any{"enabled": false}))
	g.must(http.StatusOK, g.do("PATCH", gitBase+"/"+off.ID.String(), g.chen, map[string]any{"enabled": true}))
}

// Token mode: the token is checked, sealed, never returned; the secret is
// made by the server and never returned.
func TestGitIntegrations_TokenMode(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	h := newFakeHost(t)
	r := g.must(http.StatusCreated, g.create(map[string]any{"provider": "github", "baseUrl": h.url(), "mode": "token", "token": gitToken}))
	for _, s := range []string{gitToken, `"webhookSecret"`} {
		if strings.Contains(string(r.body), s) {
			t.Fatalf("token mode response shows %q: %s", s, r.body)
		}
	}
	var gi tracker.GitIntegrationCreated
	r.json(t, &gi)
	if gi.Mode != "token" || !gi.TokenSet || gi.Status != "ok" || gi.WebhookSecret != "" {
		t.Fatalf("integration = %+v", gi)
	}
	if h.Count("GET /api/v3/user") != 1 {
		t.Fatalf("host calls = %v", h.Calls())
	}
	if got, _ := h.lastTok.Load().(string); got != "Bearer "+gitToken && got != "token "+gitToken {
		t.Fatalf("the host did not get the token: %q", got)
	}
	var blob []byte
	if err := g.pool.QueryRow(context.Background(), `SELECT token_enc FROM git_integrations WHERE id = $1`, gi.ID).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte(gitToken)) {
		t.Fatal("the token is stored in the clear")
	}
	if pt, err := g.kr.OpenFor("token_enc", gi.ID.String(), blob); err != nil || string(pt) != gitToken {
		t.Fatalf("token does not open under its row: %v", err)
	}
	if _, err := g.kr.OpenFor("webhook_secret_enc", gi.ID.String(), blob); err == nil {
		t.Fatal("the token blob opens as another column")
	}
	if !rawContains(g, `SELECT count(*) FROM git_integrations WHERE id = $1 AND webhook_secret_enc IS NOT NULL`, gi.ID) {
		t.Fatal("token mode stored no sealed secret")
	}
	// The list and get show tokenSet only.
	r = g.must(http.StatusOK, g.do("GET", gitBase+"/"+gi.ID.String(), g.chen, nil))
	if strings.Contains(string(r.body), gitToken) || !strings.Contains(string(r.body), `"tokenSet":true`) {
		t.Fatalf("get = %s", r.body)
	}
}

func TestGitIntegrations_PublicURLRequired(t *testing.T) {
	tokenBody := func(base string) map[string]any {
		return map[string]any{"provider": "gitlab", "baseUrl": base, "mode": "token", "token": gitToken}
	}
	// A non-local server needs an absolute https PUBLIC_BASE_URL for token mode.
	for name, pub := range map[string]string{"empty": "-", "http": "http://tracker.example.com", "relative": "/tracker"} {
		g := newGitEnv(t, gitEnvOpts{publicBase: pub, nonLocal: true})
		if pub == "-" {
			pub = ""
		}
		g.res.Hosts["gitlab.pub"] = []string{"93.184.216.34"}
		r := g.create(tokenBody("https://gitlab.pub"))
		if r.status != http.StatusConflict || r.errCode(t) != "public_url_required" {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
		// Webhook-only still works, and shows the path for a missing public URL.
		var gi tracker.GitIntegrationCreated
		g.must(http.StatusCreated, g.create(map[string]any{"provider": "gitlab", "baseUrl": "https://gitlab.pub"})).json(t, &gi)
		if want := pub + "/api/v1/orgs/orbit/integrations/gitlab/webhook"; gi.WebhookURL != want {
			t.Fatalf("%s: webhookUrl = %q, want %q", name, gi.WebhookURL, want)
		}
	}
	// A local server may use an http one.
	h := newFakeHost(t)
	g := newGitEnv(t, gitEnvOpts{publicBase: "http://localhost:8090"})
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "github", "baseUrl": h.url(), "mode": "token", "token": gitToken}))
}

// The scheme rule of T17 #29 and the save-time SSRF refusal of #31.
func TestGitIntegrations_SchemeAndSSRFAtSave(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{nonLocal: true})
	g.res.Hosts["gitlab.lan"] = []string{"10.1.2.3"}
	g.res.Hosts["gitlab.pub"] = []string{"93.184.216.34"}
	// https is fine; http is refused unless the address is private and permitted.
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "gitlab", "baseUrl": "https://gitlab.pub"}))
	for _, u := range []string{"http://gitlab.pub", "http://gitlab.lan", "http://10.1.2.3", "http://127.0.0.1:9", "http://unknown.host"} {
		r := g.create(map[string]any{"provider": "gitlab", "baseUrl": u})
		if r.status != http.StatusBadRequest || r.errCode(t) != "validation_failed" {
			t.Fatalf("%s: %d %s", u, r.status, r.body)
		}
	}
	// apiBaseUrl is held to the same rule.
	g.must(http.StatusBadRequest, g.create(map[string]any{"provider": "gitlab", "baseUrl": "https://gl2.example.com", "apiBaseUrl": "http://gitlab.pub/api/v4"}))

	// With the private switch on for 10.0.0.0/8, http to a private host is allowed.
	pol, err := netguard.GitPolicy(false, "10.0.0.0/8", nil)
	if err != nil {
		t.Fatal(err)
	}
	g2 := newGitEnv(t, gitEnvOpts{nonLocal: true, policy: &pol})
	g2.res.Hosts["gitlab.lan"] = []string{"10.1.2.3"}
	g2.res.Hosts["gitlab.other"] = []string{"192.168.1.5"}
	g2.must(http.StatusCreated, g2.create(map[string]any{"provider": "gitlab", "baseUrl": "http://gitlab.lan"}))
	g2.must(http.StatusBadRequest, g2.create(map[string]any{"provider": "gitlab", "baseUrl": "http://gitlab.other"})) // private but not listed

	// Token mode refuses addresses the policy refuses, before any call.
	g3 := newGitEnv(t, gitEnvOpts{})
	g3.res.Hosts["evil.example.com"] = []string{"169.254.169.254"}
	g3.res.Hosts["lan.example.com"] = []string{"10.9.9.9"}
	g3.res.Hosts["app.example.com"] = []string{"93.184.216.34"}
	for _, u := range []string{"https://169.254.169.254", "https://[fd00:ec2::254]", "https://10.0.0.5", "https://evil.example.com", "https://lan.example.com", "https://silo:9000", "https://postgres", "https://host.docker.internal"} {
		r := g3.create(map[string]any{"provider": "gitlab", "baseUrl": u, "mode": "token", "token": gitToken})
		if r.status != http.StatusBadRequest || r.errCode(t) != "validation_failed" {
			t.Fatalf("token mode %s: %d %s", u, r.status, r.body)
		}
		if strings.Contains(string(r.body), gitToken) {
			t.Fatal("the error echoes the token")
		}
	}
	// A PATCH to such a host with a token is refused the same way.
	var gi tracker.GitIntegration
	g3.must(http.StatusCreated, g3.create(map[string]any{"provider": "gitlab", "baseUrl": "https://app.example.com"})).json(t, &gi)
	g3.must(http.StatusBadRequest, g3.do("PATCH", gitBase+"/"+gi.ID.String(), g3.chen, map[string]any{"baseUrl": "https://lan.example.com", "token": gitToken}))
	if n := g3.count(`SELECT count(*) FROM git_integrations`); n != 1 {
		t.Fatalf("%d rows", n)
	}
}

// The provider_* mapping (T17 #12, #28): 401, 403, 429, 5xx and a timeout; the
// host's own words are never echoed.
func TestGitIntegrations_ProviderErrors(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	h := newFakeHost(t)
	cases := []struct {
		name   string
		status int
		delay  time.Duration
		http   int
		code   string
	}{
		{"unauthorized", 401, 0, http.StatusBadGateway, "provider_unauthorized"},
		{"forbidden", 403, 0, http.StatusBadGateway, "provider_forbidden"},
		{"rate limited", 429, 0, http.StatusServiceUnavailable, "provider_rate_limited"},
		{"server error", 500, 0, http.StatusBadGateway, "provider_error"},
		{"bad gateway", 502, 0, http.StatusBadGateway, "provider_error"},
		{"timeout", 200, 3 * time.Second, http.StatusBadGateway, "provider_unreachable"},
	}
	for _, c := range cases {
		g2 := newGitEnv(t, gitEnvOpts{})
		h.status.Store(int32(c.status))
		h.delay.Store(int64(c.delay))
		r := g2.create(map[string]any{"provider": "github", "baseUrl": h.url(), "mode": "token", "token": gitToken})
		if r.status != c.http || r.errCode(t) != c.code {
			t.Fatalf("%s: %d %s", c.name, r.status, r.body)
		}
		if strings.Contains(string(r.body), "secret host text") || strings.Contains(string(r.body), gitToken) {
			t.Fatalf("%s: the error echoes host text or the token: %s", c.name, r.body)
		}
		if c.status == 429 && r.header.Get("Retry-After") == "" {
			t.Fatal("429 mapped without Retry-After")
		}
		if n := g2.count(`SELECT count(*) FROM git_integrations`); n != 0 {
			t.Fatalf("%s: a failed check left a row", c.name)
		}
	}
	// An unreachable host (nothing listens).
	dead := newFakeHost(t)
	dead.Srv.Close()
	r := g.create(map[string]any{"provider": "github", "baseUrl": dead.url(), "mode": "token", "token": gitToken})
	if r.status != http.StatusBadGateway || r.errCode(t) != "provider_unreachable" {
		t.Fatalf("dead host: %d %s", r.status, r.body)
	}
}

// check sets and clears the attention code (T17 #11a).
func TestGitIntegrations_CheckAndAttention(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	h := newFakeHost(t)
	gi := g.tokenCreate(h)
	path := gitBase + "/" + gi.ID.String() + "/check"
	var chk tracker.GitIntegrationCheck
	g.must(http.StatusOK, g.do("POST", path, g.chen, nil)).json(t, &chk)
	if chk.Login != "octo" || chk.Status != "ok" {
		t.Fatalf("check = %+v", chk.GitIntegration)
	}
	attention := func() (status string, code *string) {
		var got tracker.GitIntegration
		g.must(http.StatusOK, g.do("GET", gitBase+"/"+gi.ID.String(), g.chen, nil)).json(t, &got)
		return got.Status, got.AttentionCode
	}
	h.status.Store(401)
	if c := g.must(http.StatusBadGateway, g.do("POST", path, g.chen, nil)).errCode(t); c != "provider_unauthorized" {
		t.Fatalf("code = %s", c)
	}
	if st, code := attention(); st != "needs_attention" || code == nil || *code != "token_rejected" {
		t.Fatalf("after 401: %s %v", st, code)
	}
	h.status.Store(200)
	g.must(http.StatusOK, g.do("POST", path, g.chen, nil))
	if st, code := attention(); st != "ok" || code != nil {
		t.Fatalf("after a good check: %s %v", st, code)
	}
	h.status.Store(403)
	g.must(http.StatusBadGateway, g.do("POST", path, g.chen, nil))
	if _, code := attention(); code == nil || *code != "token_rejected" {
		t.Fatalf("after 403: %v", code)
	}
	// A new token that checks well clears it.
	h.status.Store(200)
	g.must(http.StatusOK, g.do("PATCH", gitBase+"/"+gi.ID.String(), g.chen, map[string]any{"token": gitToken2}))
	if st, code := attention(); st != "ok" || code != nil {
		t.Fatalf("after a token replace: %s %v", st, code)
	}
	// An unreachable host.
	h.delay.Store(int64(3 * time.Second))
	g.must(http.StatusBadGateway, g.do("POST", path, g.chen, nil))
	if _, code := attention(); code == nil || *code != "attention_unreachable" {
		t.Fatalf("after a timeout: %v", code)
	}
	h.delay.Store(0)
	// Disabled beats needs_attention.
	g.must(http.StatusOK, g.do("PATCH", gitBase+"/"+gi.ID.String(), g.chen, map[string]any{"enabled": false}))
	if st, _ := attention(); st != "disabled" {
		t.Fatalf("status = %s, want disabled", st)
	}
	// A token that cannot be read (the key changed) is secret_unreadable and says so.
	g.must(http.StatusOK, g.do("PATCH", gitBase+"/"+gi.ID.String(), g.chen, map[string]any{"enabled": true}))
	if _, err := g.pool.Exec(context.Background(), `UPDATE git_integrations SET token_enc = $2 WHERE id = $1`, gi.ID,
		ghKeyring(t, 9).SealFor("token_enc", gi.ID.String(), []byte("x"))); err != nil {
		t.Fatal(err)
	}
	if c := g.must(http.StatusConflict, g.do("POST", path, g.chen, nil)).errCode(t); c != "encryption_unavailable" {
		t.Fatalf("unreadable token code = %s", c)
	}
	if _, code := attention(); code == nil || *code != "secret_unreadable" {
		t.Fatalf("unreadable: %v", code)
	}
}

// A webhook-only integration has no token: check is 409 and sends nothing.
func TestGitIntegrations_CheckWebhookOnly(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	var gi tracker.GitIntegration
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "github", "baseUrl": "https://github.com"})).json(t, &gi)
	if c := g.must(http.StatusConflict, g.do("POST", gitBase+"/"+gi.ID.String()+"/check", g.chen, nil)).errCode(t); c != "conflict" {
		t.Fatalf("code = %s", c)
	}
}

// A URL change clears the token, and a following check sends nothing to the
// new host. A URL change with a repo mapped is 409 (T17 #11).
func TestGitIntegrations_URLChangeClearsToken(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	old, fresh := newFakeHost(t), newFakeHost(t)
	gi := g.tokenCreate(old)
	path := gitBase + "/" + gi.ID.String()

	var got tracker.GitIntegration
	g.must(http.StatusOK, g.do("PATCH", path, g.chen, map[string]any{"baseUrl": fresh.url()})).json(t, &got)
	if got.TokenSet || got.Mode != "webhook_only" || got.BaseURL != fresh.url() {
		t.Fatalf("after the URL change: %+v", got)
	}
	if rawContains(g, `SELECT count(*) FROM git_integrations WHERE id = $1 AND token_enc IS NOT NULL`, gi.ID) {
		t.Fatal("the token blob is still stored")
	}
	g.must(http.StatusConflict, g.do("POST", path+"/check", g.chen, nil))
	if fresh.Count("") != 0 {
		t.Fatalf("the new host got %v", fresh.Calls())
	}
	// apiBaseUrl is the same: set it on a token integration.
	gi2 := g.tokenCreate(newFakeHost(t))
	g.must(http.StatusOK, g.do("PATCH", gitBase+"/"+gi2.ID.String(), g.chen, map[string]any{"apiBaseUrl": fresh.url() + "/api/v3"})).json(t, &got)
	if got.TokenSet || got.APIBaseURL == nil {
		t.Fatalf("after the apiBaseUrl change: %+v", got)
	}
	if fresh.Count("") != 0 {
		t.Fatalf("the new host got %v", fresh.Calls())
	}
	// A URL change that carries a new token is allowed and checks it on the new host.
	g.must(http.StatusOK, g.do("PATCH", path, g.chen, map[string]any{"baseUrl": old.url(), "token": gitToken2})).json(t, &got)
	if !got.TokenSet || got.Mode != "token" {
		t.Fatalf("URL + token: %+v", got)
	}
	if old.Count("GET /api/v3/user") != 2 { // create + this check
		t.Fatalf("old host calls = %v", old.Calls())
	}
	// With a repository mapped, the URL cannot change at all.
	if _, err := g.pool.Exec(context.Background(), `INSERT INTO git_repo_map (org_id, integration_id, repo_key, repo_path) VALUES ($1, $2, '7', 'a/b')`, g.orgID, gi.ID); err != nil {
		t.Fatal(err)
	}
	if c := g.must(http.StatusConflict, g.do("PATCH", path, g.chen, map[string]any{"baseUrl": fresh.url()})).errCode(t); c != "repos_mapped" {
		t.Fatalf("code = %s", c)
	}
	g.must(http.StatusConflict, g.do("PATCH", path, g.chen, map[string]any{"apiBaseUrl": fresh.url() + "/api/v3"}))
	g.must(http.StatusOK, g.do("GET", path, g.chen, nil)).json(t, &got)
	if got.BaseURL != old.url() || got.RepoCount != 1 || !got.TokenSet {
		t.Fatalf("a refused change moved the row: %+v", got)
	}
	// Name and enabled still change with a repo mapped.
	g.must(http.StatusOK, g.do("PATCH", path, g.chen, map[string]any{"name": "mine"}))
}

func TestGitIntegrations_PatchRules(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	h := newFakeHost(t)
	gi := g.tokenCreate(h)
	path := gitBase + "/" + gi.ID.String()
	for name, body := range map[string]map[string]any{
		"null base":   {"baseUrl": nil},
		"null mode":   {"mode": nil},
		"null enable": {"enabled": nil},
		"null name":   {"name": nil},
		"bad mode":    {"mode": "magic"},
		"unknown":     {"secret": glSecret},
		"bad base":    {"baseUrl": "nope"},
	} {
		if r := g.do("PATCH", path, g.chen, body); r.status != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
	}
	g.must(http.StatusBadRequest, g.do("PATCH", path, g.chen, map[string]any{"enabled": true}, "Idempotency-Key", "k"))
	// A null token keeps it; "" clears it and forces webhook_only in the same statement.
	var got tracker.GitIntegration
	g.must(http.StatusOK, g.do("PATCH", path, g.chen, map[string]any{"token": nil})).json(t, &got)
	if !got.TokenSet || got.Mode != "token" {
		t.Fatalf("null token: %+v", got)
	}
	g.must(http.StatusOK, g.do("PATCH", path, g.chen, map[string]any{"token": ""})).json(t, &got)
	if got.TokenSet || got.Mode != "webhook_only" {
		t.Fatalf("cleared token: %+v", got)
	}
	// mode token with no stored token is 400; with a token it checks and switches.
	g.must(http.StatusBadRequest, g.do("PATCH", path, g.chen, map[string]any{"mode": "token"}))
	g.must(http.StatusOK, g.do("PATCH", path, g.chen, map[string]any{"mode": "token", "token": gitToken2})).json(t, &got)
	if !got.TokenSet || got.Mode != "token" {
		t.Fatalf("mode token + token: %+v", got)
	}
	// token -> webhook_only clears the token.
	g.must(http.StatusOK, g.do("PATCH", path, g.chen, map[string]any{"mode": "webhook_only"})).json(t, &got)
	if got.TokenSet || got.Mode != "webhook_only" {
		t.Fatalf("to webhook_only: %+v", got)
	}
	// A token the host rejects is not stored.
	h.status.Store(401)
	g.must(http.StatusBadGateway, g.do("PATCH", path, g.chen, map[string]any{"token": "bad"}))
	g.must(http.StatusOK, g.do("GET", path, g.chen, nil)).json(t, &got)
	if got.TokenSet {
		t.Fatal("a rejected token was stored")
	}
	// A GitLab row that keeps only a hash cannot go to token mode until a rotation seals a secret.
	h.status.Store(200)
	hashOnly := uuid.Must(uuid.NewV7())
	if _, err := g.pool.Exec(context.Background(), `INSERT INTO git_integrations (id, org_id, provider, base_url, webhook_secret_hash) VALUES ($1, $2, 'gitlab', $3, $4)`,
		hashOnly, g.orgID, h.url(), bytes.Repeat([]byte{7}, 32)); err != nil {
		t.Fatal(err)
	}
	g.must(http.StatusNotFound, g.do("PATCH", gitBase+"/"+uuid.NewString(), g.chen, map[string]any{"name": "x"}))
	if c := g.must(http.StatusConflict, g.do("PATCH", gitBase+"/"+hashOnly.String(), g.chen, map[string]any{"token": gitToken})).errCode(t); c != "secret_hash_only" {
		t.Fatalf("hash-only to token mode: %s", c)
	}
	// Disabling stops the webhook (401); the secret cache learns of it.
	gh := &ghEnv{env: g.env}
	var web tracker.GitIntegrationCreated
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "github", "baseUrl": "https://github.com"})).json(t, &web)
	gh.must(http.StatusOK, gh.ghHook("orbit", web.WebhookSecret, uuid.NewString(), []byte("{}"), github.HeaderEvent, "ping"))
	g.must(http.StatusOK, g.do("PATCH", gitBase+"/"+web.ID.String(), g.chen, map[string]any{"enabled": false}))
	gh.must(http.StatusUnauthorized, gh.ghHook("orbit", web.WebhookSecret, uuid.NewString(), []byte("{}"), github.HeaderEvent, "ping"))
	g.must(http.StatusOK, g.do("PATCH", gitBase+"/"+web.ID.String(), g.chen, map[string]any{"enabled": true}))
	gh.must(http.StatusOK, gh.ghHook("orbit", web.WebhookSecret, uuid.NewString(), []byte("{}"), github.HeaderEvent, "ping"))
}

// The rate limit of T17 #28: a fourth immediate request is 429 rate_limited.
func TestGitIntegrations_RateLimit(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{limited: true})
	h := newFakeHost(t)
	var gi tracker.GitIntegration
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "github", "baseUrl": h.url()})).json(t, &gi)
	// Not token mode, so no allowance used: the create did not call the host.
	g.must(http.StatusOK, g.do("PATCH", gitBase+"/"+gi.ID.String(), g.chen, map[string]any{"token": gitToken, "mode": "token"}))
	path := gitBase + "/" + gi.ID.String() + "/check"
	g.must(http.StatusOK, g.do("POST", path, g.chen, nil)) // second use (PATCH was the first)
	g.must(http.StatusOK, g.do("POST", path, g.chen, nil)) // third
	r := g.do("POST", path, g.chen, nil)                   // fourth
	if r.status != http.StatusTooManyRequests || r.errCode(t) != "rate_limited" || r.header.Get("Retry-After") == "" {
		t.Fatalf("fourth call: %d %s (Retry-After %q)", r.status, r.body, r.header.Get("Retry-After"))
	}
	// The limit is per org: another org's admin is not slowed ...
	org2 := g.newOrg("acme", "ACME")
	admin2 := g.newUser("acmeadmin")
	g.addMember(org2, admin2.ID, "admin")
	tok2, _ := g.newToken(admin2.ID, tokenOpts{pin: &org2})
	h2 := newFakeHost(t)
	g.must(http.StatusCreated, g.do("POST", "/api/v1/orgs/acme/integrations", tok2, map[string]any{"provider": "github", "baseUrl": h2.url(), "mode": "token", "token": gitToken}))
	// ... and the limit also stops the create and the token PATCH.
	for i := 0; i < 4; i++ {
		r = g.create(map[string]any{"provider": "github", "baseUrl": fmt.Sprintf("%s/%d", h.url(), i), "mode": "token", "token": gitToken})
	}
	if r.status != http.StatusTooManyRequests {
		t.Fatalf("create past the limit: %d %s", r.status, r.body)
	}
	if h.Count("GET") > 5 {
		t.Fatalf("the host was called %d times despite the limit", h.Count("GET"))
	}
}

// Every route needs the admin scope and role; another org's integration is 404.
func TestGitIntegrations_AccessAndCrossOrg(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	h := newFakeHost(t)
	mine := g.tokenCreate(h)

	org2 := g.newOrg("acme", "ACME")
	admin2 := g.newUser("acmeadmin")
	g.addMember(org2, admin2.ID, "admin")
	tok2, _ := g.newToken(admin2.ID, tokenOpts{pin: &org2})
	var theirs tracker.GitIntegration
	g.must(http.StatusCreated, g.do("POST", "/api/v1/orgs/acme/integrations", tok2, map[string]any{"provider": "gitlab", "baseUrl": glBaseURL, "secret": gitAcmeKey})).json(t, &theirs)

	id := mine.ID.String()
	// A write-scope token of a member (claude) is 403 on every route, GETs included.
	for _, c := range []struct{ method, path string }{
		{"GET", gitBase}, {"POST", gitBase}, {"GET", gitBase + "/" + id}, {"DELETE", gitBase + "/" + id}, {"PATCH", gitBase + "/" + id}, {"POST", gitBase + "/" + id + "/check"},
	} {
		if code := g.must(http.StatusForbidden, g.do(c.method, c.path, g.claude, map[string]any{})).errCode(t); code != "insufficient_scope" {
			t.Fatalf("%s %s: code %s", c.method, c.path, code)
		}
	}
	// Cross-org: acme's integration through orbit's path is 404 on every route.
	tid := theirs.ID.String()
	g.must(http.StatusNotFound, g.do("GET", gitBase+"/"+tid, g.chen, nil))
	g.must(http.StatusNotFound, g.do("PATCH", gitBase+"/"+tid, g.chen, map[string]any{"enabled": false}))
	g.must(http.StatusNotFound, g.do("POST", gitBase+"/"+tid+"/check", g.chen, nil))
	g.must(http.StatusNotFound, g.do("DELETE", gitBase+"/"+tid, g.chen, nil))
	// ... and orbit's through acme's.
	g.must(http.StatusNotFound, g.do("GET", "/api/v1/orgs/acme/integrations/"+id, tok2, nil))
	g.must(http.StatusNotFound, g.do("PATCH", "/api/v1/orgs/acme/integrations/"+id, tok2, map[string]any{"enabled": false}))
	g.must(http.StatusNotFound, g.do("POST", "/api/v1/orgs/acme/integrations/"+id+"/check", tok2, nil))
	g.must(http.StatusNotFound, g.do("DELETE", "/api/v1/orgs/acme/integrations/"+id, tok2, nil))
	if g.count(`SELECT count(*) FROM git_integrations`) != 2 {
		t.Fatal("a cross-org delete removed a row")
	}
	// A token pinned to acme cannot read orbit's list at all.
	g.must(http.StatusNotFound, g.do("GET", gitBase, tok2, nil))
	g.must(http.StatusNotFound, g.do("POST", gitBase, tok2, map[string]any{"provider": "gitlab", "baseUrl": glBaseURL}))
	// Each org lists only its own.
	var l1, l2 struct {
		Items []tracker.GitIntegration `json:"items"`
	}
	g.must(http.StatusOK, g.do("GET", gitBase, g.chen, nil)).json(t, &l1)
	g.must(http.StatusOK, g.do("GET", "/api/v1/orgs/acme/integrations", tok2, nil)).json(t, &l2)
	if len(l1.Items) != 1 || l1.Items[0].ID != mine.ID || len(l2.Items) != 1 || l2.Items[0].ID != theirs.ID {
		t.Fatalf("lists: %+v / %+v", l1.Items, l2.Items)
	}
	if g.count(`SELECT count(*) FROM git_integrations WHERE id = $1 AND enabled`, theirs.ID) != 1 {
		t.Fatal("orbit's admin changed acme's integration")
	}
	// The same base URL in two orgs is fine; so is the same secret.
	g.must(http.StatusCreated, g.do("POST", "/api/v1/orgs/acme/integrations", tok2, map[string]any{"provider": "github", "baseUrl": h.url()}))
	// A malformed id is 404.
	g.must(http.StatusNotFound, g.do("GET", gitBase+"/not-a-uuid", g.chen, nil))
}

// A secret or token is never logged, emitted or stored by the idempotency layer.
func TestGitIntegrations_NoSecretsInLogsEventsOrIdempotency(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	h := newFakeHost(t)
	events := g.count(`SELECT count(*) FROM events`)
	var web tracker.GitIntegrationCreated
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "github", "baseUrl": "https://github.com"})).json(t, &web)
	var gl tracker.GitIntegration
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "gitlab", "baseUrl": glBaseURL, "secret": glSecret})).json(t, &gl)
	tok := g.tokenCreate(h)
	path := gitBase + "/" + tok.ID.String()
	g.must(http.StatusOK, g.do("PATCH", path, g.chen, map[string]any{"token": gitToken2}))
	g.must(http.StatusOK, g.do("POST", path+"/check", g.chen, nil))
	h.status.Store(401)
	g.do("POST", path+"/check", g.chen, nil)
	g.do("PATCH", path, g.chen, map[string]any{"token": "rejected-token-0123456789"})
	g.do("POST", gitBase, g.chen, map[string]any{"provider": "github", "baseUrl": h.url() + "/x", "mode": "token", "token": "never-stored-token-0123456789"})

	logs := g.logs.String()
	for _, s := range []string{web.WebhookSecret, glSecret, gitToken, gitToken2, "rejected-token-0123456789", "never-stored-token-0123456789", "secret host text"} {
		if strings.Contains(logs, s) {
			t.Fatalf("a secret or host text is in the server log: %q", s)
		}
	}
	if n := g.count(`SELECT count(*) FROM events`); n != events {
		t.Fatalf("config routes emitted %d events (install-level credentials emit none)", n-events)
	}
	if n := g.count(`SELECT count(*) FROM idempotency_keys`); n != 0 {
		t.Fatalf("idempotency rows = %d after the secret routes", n)
	}
	// The hash column holds sha256 only: no plaintext anywhere in the table.
	for _, s := range []string{web.WebhookSecret, glSecret, gitToken, gitToken2} {
		if rawContains(g, `SELECT count(*) FROM git_integrations g WHERE g::text LIKE '%' || $1 || '%'`, s) {
			t.Fatalf("plaintext %q is stored", s)
		}
	}
}

// A repo map insert takes a SHARE lock on the integration row, so it waits for
// a PATCH that holds the row (which re-counts the mapped repos under the lock):
// a URL change cannot race a concurrent map.
func TestGitIntegrations_RepoMapWaitsForRowLock(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	var gi tracker.GitIntegration
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "gitlab", "baseUrl": glBaseURL})).json(t, &gi)
	ctx := context.Background()
	tx, err := g.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM git_integrations WHERE id = $1 FOR UPDATE`, gi.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan resp, 1)
	go func() {
		done <- g.do("POST", gitBase+"/"+gi.ID.String()+"/repos", g.chen, map[string]any{"repoKey": "5", "repoPath": "a/b"})
	}()
	select {
	case r := <-done:
		t.Fatalf("the map insert did not wait for the row lock: %d %s", r.status, r.body)
	case <-time.After(400 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	g.must(http.StatusCreated, <-done)
}

// LAUN-104: removing a connection. Refused while a repository is mapped; then
// it deletes this integration's own rows and nothing else.
func TestGitIntegrations_Delete(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	var a, b tracker.GitIntegrationCreated
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "gitlab", "baseUrl": glBaseURL})).json(t, &a)
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "github", "baseUrl": "https://github.com"})).json(t, &b)
	ctx := context.Background()
	for _, id := range []uuid.UUID{a.ID, b.ID} {
		if _, err := g.pool.Exec(ctx, `INSERT INTO git_webhook_deliveries (org_id, integration_id, provider, result, http_status) VALUES ($1, $2, 'gitlab', 'processed', 200)`, g.orgID, id); err != nil {
			t.Fatal(err)
		}
		if _, err := g.pool.Exec(ctx, `INSERT INTO git_webhook_events (org_id, integration_id, event_uuid, event_type) VALUES ($1, $2, $3, 'x')`, g.orgID, id, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
	}
	// A mapped repository refuses with its own code, and removes nothing.
	if _, err := g.pool.Exec(ctx, `INSERT INTO git_repo_map (org_id, integration_id, repo_key, repo_path) VALUES ($1, $2, '7', 'a/b')`, g.orgID, a.ID); err != nil {
		t.Fatal(err)
	}
	if c := g.must(http.StatusConflict, g.do("DELETE", gitBase+"/"+a.ID.String(), g.chen, nil)).errCode(t); c != "repos_mapped" {
		t.Fatalf("code = %s", c)
	}
	if g.count(`SELECT count(*) FROM git_integrations WHERE id = $1`, a.ID) != 1 {
		t.Fatal("a refused delete removed the row")
	}
	if _, err := g.pool.Exec(ctx, `DELETE FROM git_repo_map WHERE integration_id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	if r := g.do("DELETE", gitBase+"/"+a.ID.String(), g.chen, nil); r.status != http.StatusNoContent {
		t.Fatalf("delete: %d %s", r.status, r.body)
	}
	g.must(http.StatusNotFound, g.do("GET", gitBase+"/"+a.ID.String(), g.chen, nil))
	g.must(http.StatusNotFound, g.do("DELETE", gitBase+"/"+a.ID.String(), g.chen, nil))
	for _, tbl := range []string{"git_webhook_deliveries", "git_webhook_events"} {
		if g.count(`SELECT count(*) FROM `+tbl+` WHERE integration_id = $1`, a.ID) != 0 {
			t.Fatalf("%s rows of the removed integration remain", tbl)
		}
		if g.count(`SELECT count(*) FROM `+tbl+` WHERE integration_id = $1`, b.ID) != 1 {
			t.Fatalf("%s: the other integration's row changed", tbl)
		}
	}
	// The same host can be connected again at once.
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "gitlab", "baseUrl": glBaseURL}))
	// The 409 reasons have their own codes.
	if c := g.must(http.StatusConflict, g.create(map[string]any{"provider": "github", "baseUrl": "https://github.com"})).errCode(t); c != "integration_exists" {
		t.Fatalf("duplicate: %s", c)
	}
}
