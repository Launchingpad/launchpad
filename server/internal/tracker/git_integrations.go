package tracker

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/gitapi"
	"devops/internal/httpx"
	"devops/internal/netguard"
	"devops/internal/scm"
)

// Git integration config (T17, LAUN-7): the provider-neutral routes under
// /orgs/{org}/integrations. Rules: docs/internal/decisions-T17-git-connections-import.md
// (#11, #11a, #12, #13, #28, #29, #31). The webhook itself is gitlab_webhook.go
// and git_secrets.go.
//
// The token and the secrets are write-only. The token is sealed (AES-256-GCM,
// AAD = column + row id); the webhook secret is sealed when the server has a
// key and always kept as a sha256 hash. Nothing here logs, emits or returns a
// secret, and the create route is never idempotent.

const (
	// maxEnabledGitIntegrations bounds the HMAC compare of a signed webhook
	// (T17 #14): at most 8 enabled integrations per provider per org.
	maxEnabledGitIntegrations = 8
	// gitCheckTimeout is the budget of one token check in a request (T17 #28).
	gitCheckTimeout = 10 * time.Second

	maxGitNameRunes = 100

	ModeToken       = "token"
	ModeWebhookOnly = "webhook_only"

	colTokenEnc = "token_enc"

	// The attention codes of T17 #11a: our own, never host text.
	AttentionSecretUnreadable = "secret_unreadable"
	AttentionTokenRejected    = "token_rejected"
	AttentionUnreachable      = "attention_unreachable"
	AttentionHookFailed       = "hook_failed"

	// The status of the list route (T17 #11a).
	StatusOK             = "ok"
	StatusNeedsAttention = "needs_attention"
	StatusDisabled       = "disabled"
)

// gitProviders are the providers a create accepts (T17: gitlab, github and,
// since LAUN-84, bitbucket, Cloud and Data Center).
var gitProviders = map[string]bool{"gitlab": true, "github": true, "bitbucket": true}

// GitNet is what the config routes need to reach a git host: the SSRF policy
// (the same one the API clients dial under, for the save-time refusals of
// T17 #29), a resolver for those refusals, a client factory and whether the
// server is a LOG_ENV local or test one. Set it before serving; with none set
// token mode cannot be used.
type GitNet struct {
	Policy   netguard.Policy
	Resolver netguard.Resolver // nil: net.DefaultResolver
	// NewClient builds the guarded API client of one integration. apiRoot is
	// the API root (derived from the base URL or the admin's override).
	NewClient func(provider, apiRoot, token string) (scm.APIClient, error)
	// Local is LOG_ENV local or test: http is allowed, and so is an http
	// PUBLIC_BASE_URL.
	Local bool
}

// SetGitNet sets the git host access. Call it before serving.
func (s *Service) SetGitNet(n *GitNet) { s.gitNet = n }

// GitIntegration is one git host an org takes webhooks from. No secret is
// part of it: tokenSet and webhookSecretSet say only that one is stored.
type GitIntegration struct {
	ID               uuid.UUID `json:"id"`
	Provider         string    `json:"provider"`
	Flavor           *string   `json:"flavor"`
	Name             string    `json:"name"`
	BaseURL          string    `json:"baseUrl"`
	APIBaseURL       *string   `json:"apiBaseUrl"`
	Mode             string    `json:"mode"`
	Enabled          bool      `json:"enabled"`
	Status           string    `json:"status"`
	AttentionCode    *string   `json:"attentionCode"`
	TokenSet         bool      `json:"tokenSet"`
	WebhookSecretSet bool      `json:"webhookSecretSet"`
	// SecretPending is true while a rotation holds a pending secret (T17 #11).
	SecretPending bool `json:"secretPending"`
	// WebhookURL is what to paste into the host's webhook form (the secret
	// goes in the host's secret field, never in the URL).
	WebhookURL string    `json:"webhookUrl"`
	RepoCount  int       `json:"repoCount"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	// LastDelivery is the newest row of the deliveries ring; null when the
	// integration has had none (LAUN-87).
	LastDelivery *GitDeliverySummary `json:"lastDelivery"`
}

// GitIntegrationCreated is the 201 of a create. WebhookSecret is set only for
// a webhook-only integration whose secret the server made, and only here.
type GitIntegrationCreated struct {
	GitIntegration
	WebhookSecret string `json:"webhookSecret,omitempty"`
}

// GitIntegrationCheck is the answer of a token check. Login can be empty
// (Bitbucket tokens may not name a user).
type GitIntegrationCheck struct {
	GitIntegration
	Login string `json:"login"`
}

// CreateGitIntegrationInput is the POST body. Enabled defaults to true and
// Mode to webhook_only.
type CreateGitIntegrationInput struct {
	Provider   string `json:"provider"`
	BaseURL    string `json:"baseUrl"`
	APIBaseURL string `json:"apiBaseUrl"`
	Name       string `json:"name"`
	Mode       string `json:"mode"`
	Token      string `json:"token"`
	Secret     string `json:"secret"`
	Enabled    *bool  `json:"enabled"`
}

// UpdateGitIntegrationInput is the PATCH body. Absent keeps a value. For the
// token, null also keeps it and "" clears it (T17 #11).
type UpdateGitIntegrationInput struct {
	Name       httpx.Opt[string] `json:"name"`
	BaseURL    httpx.Opt[string] `json:"baseUrl"`
	APIBaseURL httpx.Opt[string] `json:"apiBaseUrl"`
	Mode       httpx.Opt[string] `json:"mode"`
	Token      httpx.Opt[string] `json:"token"`
	Enabled    httpx.Opt[bool]   `json:"enabled"`
}

// HasToken reports whether the PATCH sends a token to check (the route rate
// limits those).
func (in UpdateGitIntegrationInput) HasToken() bool {
	return in.Token.Set && !in.Token.Null && in.Token.V != ""
}

func (s *Service) gitWebhookURL(provider, orgSlug string) string {
	return s.publicBaseURL + "/api/v1/orgs/" + url.PathEscape(orgSlug) + "/integrations/" + provider + "/webhook"
}

func gitStatus(enabled bool, attention *string) string {
	switch {
	case !enabled:
		return StatusDisabled
	case attention != nil && *attention != "":
		return StatusNeedsAttention
	}
	return StatusOK
}

func (s *Service) toGitIntegration(orgSlug string, r dbq.ListGitIntegrationsRow) GitIntegration {
	return GitIntegration{
		ID: r.ID, Provider: r.Provider, Flavor: r.Flavor, Name: derefOr(r.Name, hostOf(r.BaseUrl)),
		BaseURL: r.BaseUrl, APIBaseURL: r.ApiBaseUrl, Mode: r.Mode, Enabled: r.Enabled,
		Status: gitStatus(r.Enabled, r.AttentionCode), AttentionCode: r.AttentionCode,
		TokenSet: r.TokenSet, WebhookSecretSet: true, SecretPending: r.SecretPending,
		WebhookURL: s.gitWebhookURL(r.Provider, orgSlug), RepoCount: int(r.RepoCount),
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, LastDelivery: lastDelivery(r),
	}
}

func derefOr(p *string, def string) string {
	if p != nil && *p != "" {
		return *p
	}
	return def
}

func hostOf(base string) string {
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		return u.Host
	}
	return base
}

// flavorOf is the label of the UI: the vendor's cloud host or a self-hosted one.
func flavorOf(provider, base string) string {
	h := strings.ToLower(hostOf(base))
	if (provider == "github" && h == "github.com") || (provider == "gitlab" && h == "gitlab.com") ||
		(provider == "bitbucket" && h == "bitbucket.org") {
		return "cloud"
	}
	return "self_hosted"
}

// ListGitIntegrations lists the org's integrations of every provider, oldest first.
func (s *Service) ListGitIntegrations(ctx context.Context, org *auth.Org) ([]GitIntegration, error) {
	rows, err := s.q.ListGitIntegrations(ctx, dbq.ListGitIntegrationsParams{OrgID: org.ID})
	if err != nil {
		return nil, fmt.Errorf("list git integrations: %w", err)
	}
	out := make([]GitIntegration, 0, len(rows))
	for _, r := range rows {
		out = append(out, s.toGitIntegration(org.Slug, r))
	}
	return out, nil
}

// GetGitIntegration reads one integration of the org.
func (s *Service) GetGitIntegration(ctx context.Context, org *auth.Org, id uuid.UUID) (GitIntegration, error) {
	return s.getGitIntegration(ctx, s.q, org, id)
}

func (s *Service) getGitIntegration(ctx context.Context, q *dbq.Queries, org *auth.Org, id uuid.UUID) (GitIntegration, error) {
	r, err := s.getGitRow(ctx, q, org.ID, id)
	if err != nil {
		return GitIntegration{}, err
	}
	return s.toGitIntegration(org.Slug, r), nil
}

func (s *Service) getGitRow(ctx context.Context, q *dbq.Queries, orgID, id uuid.UUID) (dbq.ListGitIntegrationsRow, error) {
	rows, err := q.ListGitIntegrations(ctx, dbq.ListGitIntegrationsParams{OrgID: orgID, ID: &id})
	if err != nil {
		return dbq.ListGitIntegrationsRow{}, fmt.Errorf("get git integration: %w", err)
	}
	if len(rows) == 0 {
		return dbq.ListGitIntegrationsRow{}, httpx.NotFound("integration")
	}
	return rows[0], nil
}

// --- save-time rules (T17 #29, #31) ---------------------------------------

// isPrivateAddr is true for the addresses an http git host may have: the five
// standard private ranges.
func isPrivateAddr(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range netguard.StandardPrivate {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// resolveHost returns the addresses of host: the literal itself, or the
// resolver's answer. An error (no such host) returns no addresses.
func (s *Service) resolveHost(ctx context.Context, host string) []netip.Addr {
	if a, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		return []netip.Addr{a}
	}
	var res netguard.Resolver = net.DefaultResolver
	if s.gitNet != nil && s.gitNet.Resolver != nil {
		res = s.gitNet.Resolver
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := res.LookupNetIP(rctx, "ip", host)
	if err != nil {
		return nil
	}
	return addrs
}

// checkGitScheme is the scheme rule of T17 #29: https everywhere; http only on
// a LOG_ENV local or test server, or for a host whose addresses are all private
// AND allowed by the git policy (GIT_ALLOW_PRIVATE_PROVIDERS or a listed CIDR).
func (s *Service) checkGitScheme(ctx context.Context, field, normalized string) error {
	u, err := url.Parse(normalized)
	if err != nil {
		return httpx.Invalid("%s is not a valid URL", field)
	}
	if u.Scheme == "https" {
		return nil
	}
	if s.gitNet != nil && s.gitNet.Local {
		return nil
	}
	refused := httpx.Invalid("%s must be https; http is allowed only for a private address that GIT_ALLOW_PRIVATE_PROVIDERS or GIT_ALLOW_PRIVATE_CIDRS permits", field)
	if s.gitNet == nil {
		return refused
	}
	addrs := s.resolveHost(ctx, u.Hostname())
	if len(addrs) == 0 {
		return refused
	}
	for _, a := range addrs {
		if !isPrivateAddr(a) || s.gitNet.Policy.Check(a) != nil {
			return refused
		}
	}
	return nil
}

// checkGitHostAllowed is the save-time SSRF refusal for a token-mode host
// (T17 #31): a compose service name, or an address (a literal, or what the
// name resolves to now) that the git policy refuses. The dial-time guard still
// checks every connection (this does not stop rebinding; that is its job).
func (s *Service) checkGitHostAllowed(ctx context.Context, field, normalized string) error {
	u, err := url.Parse(normalized)
	if err != nil {
		return httpx.Invalid("%s is not a valid URL", field)
	}
	refused := httpx.Invalid("%s points at an address this server may not call", field)
	var pol netguard.Policy
	if s.gitNet != nil {
		pol = s.gitNet.Policy
	}
	if pol.RefusesName(u.Hostname()) {
		return refused
	}
	for _, a := range s.resolveHost(ctx, u.Hostname()) {
		if pol.Check(a) != nil {
			return refused
		}
	}
	return nil
}

func (s *Service) gitPublicURLOK() bool {
	b := strings.ToLower(s.publicBaseURL)
	if strings.HasPrefix(b, "https://") && len(b) > len("https://") {
		return true
	}
	return s.gitNet != nil && s.gitNet.Local && strings.HasPrefix(b, "http://") && len(b) > len("http://")
}

func errEncryptionUnavailable() error {
	return httpx.Errorf(http.StatusConflict, httpx.CodeEncryptionUnavailable,
		"this server has no SETTINGS_ENCRYPTION_KEY, so it cannot store a token or a webhook secret for this integration")
}

func errPublicURLRequired() error {
	return httpx.Errorf(http.StatusConflict, httpx.CodePublicURLRequired,
		"token mode needs an https PUBLIC_BASE_URL so the host can reach the webhook; set it, or use webhook-only mode")
}

// --- provider errors (T17 #12, #28) ---------------------------------------

// providerError maps a failed host call to our code. No host text is echoed.
func providerError(err error) error {
	switch gitapi.CodeOf(err) {
	case gitapi.CodeUnauthorized:
		return httpx.Errorf(http.StatusBadGateway, httpx.CodeProviderUnauthorized, "the git host refused the token")
	case gitapi.CodeForbidden:
		return httpx.Errorf(http.StatusBadGateway, httpx.CodeProviderForbidden, "the token lacks a scope the host requires")
	case gitapi.CodeRateLimited:
		e := httpx.Errorf(http.StatusServiceUnavailable, httpx.CodeProviderRateLimited, "the git host is rate limiting this server; try again later")
		e.RetryAfter = 30
		return e
	case gitapi.CodeUnreachable:
		return httpx.Errorf(http.StatusBadGateway, httpx.CodeProviderUnreachable, "the git host could not be reached")
	case "":
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, netguard.ErrRefused) {
			return httpx.Errorf(http.StatusBadGateway, httpx.CodeProviderUnreachable, "the git host could not be reached")
		}
	}
	return httpx.Errorf(http.StatusBadGateway, httpx.CodeProviderError, "the git host answered with an error")
}

// hostCallError maps the error of a client call: an *httpx.Error (our own
// validation) and a server fault pass through; a host or guard failure becomes
// a provider_* code.
func hostCallError(err error) error {
	var he *httpx.Error
	if errors.As(err, &he) {
		return err
	}
	if gitapi.CodeOf(err) == "" && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, netguard.ErrRefused) {
		return err
	}
	return providerError(err)
}

// attentionFor is the attention code a failed host call leaves (T17 #11a); ""
// leaves it as it is.
func attentionFor(err error) string {
	switch gitapi.CodeOf(err) {
	case gitapi.CodeUnauthorized, gitapi.CodeForbidden:
		return AttentionTokenRejected
	case gitapi.CodeUnreachable:
		return AttentionUnreachable
	}
	return ""
}

// newGitClient builds the guarded API client of one integration.
func (s *Service) newGitClient(provider, base, apiOverride, token string) (scm.APIClient, error) {
	if s.gitNet == nil || s.gitNet.NewClient == nil {
		return nil, fmt.Errorf("git hosts are not configured on this server")
	}
	root := apiOverride
	if root == "" {
		var err error
		if root, err = gitapi.APIRoot(provider, base); err != nil {
			return nil, httpx.Invalid("baseUrl is not usable for %s", provider)
		}
	}
	c, err := s.gitNet.NewClient(provider, root, token)
	if err != nil {
		return nil, httpx.Invalid("baseUrl or apiBaseUrl is not usable for %s", provider)
	}
	return c, nil
}

// checkToken runs CheckToken through the guarded client with the 10 s budget.
func (s *Service) checkToken(ctx context.Context, provider, base, apiOverride, token string) (scm.Identity, error) {
	c, err := s.newGitClient(provider, base, apiOverride, token)
	if err != nil {
		return scm.Identity{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, gitCheckTimeout)
	defer cancel()
	id, err := c.CheckToken(cctx)
	if err != nil {
		return scm.Identity{}, err
	}
	return id, nil
}

// --- secrets --------------------------------------------------------------

// newGitSecret makes a webhook secret: 32 random bytes, base64url (43
// printable characters, inside the 32-256 rule).
func newGitSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// checkGitSecretFree refuses (409) a secret that another enabled integration
// of the org and provider already uses, as its current or its pending secret:
// the signature or token check could not tell the two apart (T17 #14). One
// advisory lock per (org, provider) makes the check and the write atomic.
func checkGitSecretFree(ctx context.Context, tx pgx.Tx, q *dbq.Queries, orgID uuid.UUID, provider string, id uuid.UUID, hashes ...[]byte) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('devops:git-secrets:' || $1::text || ':' || $2, 0))`, orgID, provider); err != nil {
		return fmt.Errorf("git secret lock: %w", err)
	}
	for _, h := range hashes {
		if len(h) == 0 {
			continue
		}
		inUse, err := q.GitSecretInUse(ctx, dbq.GitSecretInUseParams{OrgID: orgID, Provider: provider, ID: id, WebhookSecretHash: h})
		if err != nil {
			return fmt.Errorf("git secret check: %w", err)
		}
		if inUse {
			return httpx.Errorf(http.StatusConflict, httpx.CodeSecretInUse, "another enabled %s integration of this org uses this secret; choose a different one", provider)
		}
	}
	return nil
}

func lockGitProvider(ctx context.Context, tx pgx.Tx, orgID uuid.UUID, provider string) error {
	// The same lock checkGitSecretFree takes: the limit count and the insert
	// are atomic against a concurrent create or enable.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('devops:git-secrets:' || $1::text || ':' || $2, 0))`, orgID, provider); err != nil {
		return fmt.Errorf("git provider lock: %w", err)
	}
	return nil
}

func checkGitLimit(ctx context.Context, q *dbq.Queries, orgID uuid.UUID, provider string, except uuid.UUID) error {
	n, err := q.CountEnabledGitIntegrations(ctx, dbq.CountEnabledGitIntegrationsParams{OrgID: orgID, Provider: provider, ID: except})
	if err != nil {
		return fmt.Errorf("count git integrations: %w", err)
	}
	if n >= maxEnabledGitIntegrations {
		return httpx.Errorf(http.StatusConflict, httpx.CodeIntegrationLimit,
			"an org can have at most %d enabled %s integrations", maxEnabledGitIntegrations, provider)
	}
	return nil
}

// normalizeGitName trims a display name; empty is the host.
func normalizeGitName(raw string) (string, error) {
	n := strings.TrimSpace(raw)
	if hasControl(n) || len([]rune(n)) > maxGitNameRunes {
		return "", httpx.Invalid("name must be at most %d characters with no control characters", maxGitNameRunes)
	}
	return n, nil
}

// normalizeAPIBase normalises the admin's API root override: the same rules as
// the base URL (T17 #29). Empty is "no override".
func normalizeAPIBase(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	n, err := normalizeGitlabBaseURL(raw)
	if err != nil {
		return "", httpx.Invalid("apiBaseUrl must be an absolute http or https URL with no user info, query or fragment")
	}
	return n, nil
}

// CreateGitIntegration adds a git host with its webhook secret (T17 routes
// table). No event is emitted: credential changes are not broadcast (decisions-T1
// #28). The secret is returned once, for a webhook-only integration whose
// secret the server made.
func (s *Service) CreateGitIntegration(ctx context.Context, org *auth.Org, in CreateGitIntegrationInput) (GitIntegrationCreated, error) {
	if !gitProviders[in.Provider] {
		return GitIntegrationCreated{}, httpx.Invalid("provider must be one of gitlab, github, bitbucket")
	}
	mode := in.Mode
	if mode == "" {
		mode = ModeWebhookOnly
	}
	if mode != ModeToken && mode != ModeWebhookOnly {
		return GitIntegrationCreated{}, httpx.Invalid("mode must be token or webhook_only")
	}
	base, err := normalizeGitlabBaseURL(in.BaseURL)
	if err != nil {
		return GitIntegrationCreated{}, err
	}
	apiBase, err := normalizeAPIBase(in.APIBaseURL)
	if err != nil {
		return GitIntegrationCreated{}, err
	}
	name, err := normalizeGitName(in.Name)
	if err != nil {
		return GitIntegrationCreated{}, err
	}
	if err := s.checkGitScheme(ctx, "baseUrl", base); err != nil {
		return GitIntegrationCreated{}, err
	}
	if apiBase != "" {
		if err := s.checkGitScheme(ctx, "apiBaseUrl", apiBase); err != nil {
			return GitIntegrationCreated{}, err
		}
	}
	secret := in.Secret
	generated := false
	switch mode {
	case ModeToken:
		if in.Token == "" {
			return GitIntegrationCreated{}, httpx.Invalid("token is required in token mode")
		}
		if in.Secret != "" {
			return GitIntegrationCreated{}, httpx.Invalid("secret is made by the server in token mode")
		}
	case ModeWebhookOnly:
		if in.Token != "" {
			return GitIntegrationCreated{}, httpx.Invalid("token is for token mode; set mode to token")
		}
		if secret != "" {
			if in.Provider != "gitlab" {
				return GitIntegrationCreated{}, httpx.Invalid("only a GitLab integration takes a pasted secret")
			}
			if err := validateGitlabSecret(secret); err != nil {
				return GitIntegrationCreated{}, err
			}
		}
	}
	if secret == "" {
		if secret, err = newGitSecret(); err != nil {
			return GitIntegrationCreated{}, err
		}
		generated = true
	}
	// A GitHub secret must be sealed (HMAC needs the plaintext); a token always
	// is. A GitLab secret is hashed and sealed only when the server has a key.
	needSeal := mode == ModeToken || in.Provider != "gitlab"
	if needSeal && s.gitSealer == nil {
		return GitIntegrationCreated{}, errEncryptionUnavailable()
	}
	if mode == ModeToken {
		if !s.gitPublicURLOK() {
			return GitIntegrationCreated{}, errPublicURLRequired()
		}
		if err := s.checkGitHostAllowed(ctx, "baseUrl", base); err != nil {
			return GitIntegrationCreated{}, err
		}
		if apiBase != "" {
			if err := s.checkGitHostAllowed(ctx, "apiBaseUrl", apiBase); err != nil {
				return GitIntegrationCreated{}, err
			}
		}
		if _, err := s.checkToken(ctx, in.Provider, base, apiBase, in.Token); err != nil {
			return GitIntegrationCreated{}, hostCallError(err)
		}
	}
	enabled := in.Enabled == nil || *in.Enabled
	id, err := uuid.NewV7()
	if err != nil {
		return GitIntegrationCreated{}, fmt.Errorf("uuid: %w", err)
	}
	hash := hashGitlabSecret(secret)
	var secretEnc, tokenEnc []byte
	if s.gitSealer != nil {
		secretEnc = s.gitSealer.SealFor(colWebhookSecretEnc, id.String(), []byte(secret))
		if mode == ModeToken {
			tokenEnc = s.gitSealer.SealFor(colTokenEnc, id.String(), []byte(in.Token))
		}
	}
	var out GitIntegrationCreated
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		// The webhook guard's lock first (decisions-T20 #65, entry 5).
		if err := LockTeamAccessGuard(ctx, q, org.ID); err != nil {
			return err
		}
		if err := lockGitProvider(ctx, tx, org.ID, in.Provider); err != nil {
			return err
		}
		if enabled {
			if err := checkGitLimit(ctx, q, org.ID, in.Provider, uuid.Nil); err != nil {
				return err
			}
			if err := checkGitSecretFree(ctx, tx, q, org.ID, in.Provider, uuid.Nil, hash); err != nil {
				return err
			}
		}
		flavor := flavorOf(in.Provider, base)
		var namep, apip *string
		if name != "" {
			namep = &name
		}
		if apiBase != "" {
			apip = &apiBase
		}
		if err := q.InsertGitIntegration(ctx, dbq.InsertGitIntegrationParams{
			ID: id, OrgID: org.ID, Provider: in.Provider, Flavor: &flavor, Mode: mode, Name: namep,
			BaseUrl: base, ApiBaseUrl: apip, Enabled: enabled, WebhookSecretHash: hash,
			WebhookSecretEnc: secretEnc, TokenEnc: tokenEnc,
		}); err != nil {
			if database.IsPgCode(err, database.CodeUniqueViolation) {
				return httpx.Errorf(http.StatusConflict, httpx.CodeIntegrationExists, "a %s integration for %s already exists", in.Provider, base)
			}
			return fmt.Errorf("insert git integration: %w", err)
		}
		// A new integration has no repository yet, so this passes today; it
		// is checked so the rule holds whatever the create learns to do.
		if err := CheckWebhookGuard(ctx, q, org.ID); err != nil {
			return err
		}
		gi, err := s.getGitIntegration(ctx, q, org, id)
		if err != nil {
			return err
		}
		out = GitIntegrationCreated{GitIntegration: gi}
		return nil
	})
	if err != nil {
		return GitIntegrationCreated{}, err
	}
	if generated && mode == ModeWebhookOnly {
		out.WebhookSecret = secret
	}
	return out, nil
}

// CheckGitIntegration re-checks the stored token through the guarded client
// and sets or clears the attention code (T17 #11a). A webhook-only integration
// has no token: 409.
func (s *Service) CheckGitIntegration(ctx context.Context, org *auth.Org, id uuid.UUID) (GitIntegrationCheck, error) {
	row, err := s.q.GetGitIntegrationToken(ctx, dbq.GetGitIntegrationTokenParams{OrgID: org.ID, ID: id})
	if err != nil {
		return GitIntegrationCheck{}, notFound(err, "integration")
	}
	if row.Mode != ModeToken || len(row.TokenEnc) == 0 {
		return GitIntegrationCheck{}, conflict("this integration has no token; check works in token mode only")
	}
	if s.gitSealer == nil {
		return GitIntegrationCheck{}, errEncryptionUnavailable()
	}
	tok, oerr := s.gitSealer.OpenFor(colTokenEnc, id.String(), row.TokenEnc)
	if oerr != nil || len(tok) == 0 {
		_ = s.q.SetGitAttention(ctx, dbq.SetGitAttentionParams{OrgID: org.ID, ID: id, AttentionCode: strp(AttentionSecretUnreadable)})
		return GitIntegrationCheck{}, httpx.Errorf(http.StatusConflict, httpx.CodeEncryptionUnavailable,
			"the stored token cannot be read (the encryption key changed); send the token again")
	}
	api := ""
	if row.ApiBaseUrl != nil {
		api = *row.ApiBaseUrl
	}
	ident, cerr := s.checkToken(ctx, row.Provider, row.BaseUrl, api, string(tok))
	wipe(tok)
	if cerr != nil {
		if code := attentionFor(cerr); code != "" {
			_ = s.q.SetGitAttention(ctx, dbq.SetGitAttentionParams{OrgID: org.ID, ID: id, AttentionCode: strp(code)})
		}
		return GitIntegrationCheck{}, hostCallError(cerr)
	}
	// A good check clears every code but secret_unreadable (that one is about
	// the webhook secret, which a token check does not prove).
	_, err = s.pool.Exec(ctx, `/* scope: admin */ UPDATE git_integrations SET attention_code = NULL
		WHERE org_id = $1 AND id = $2 AND attention_code IS DISTINCT FROM $3`, org.ID, id, AttentionSecretUnreadable)
	if err != nil {
		return GitIntegrationCheck{}, fmt.Errorf("clear attention: %w", err)
	}
	gi, err := s.GetGitIntegration(ctx, org, id)
	return GitIntegrationCheck{GitIntegration: gi, Login: ident.Login}, err
}

func strp(s string) *string { return &s }

// UpdateGitIntegration changes the name, the URLs, the mode, the token or the
// enabled flag (T17 routes table, #11, #13). Rotation of the webhook secret is
// its own route.
func (s *Service) UpdateGitIntegration(ctx context.Context, org *auth.Org, id uuid.UUID, in UpdateGitIntegrationInput) (GitIntegration, error) {
	for name, set := range map[string]bool{"baseUrl": in.BaseURL.Null, "mode": in.Mode.Null, "enabled": in.Enabled.Null, "name": in.Name.Null} {
		if set {
			return GitIntegration{}, httpx.Invalid("%s cannot be null", name)
		}
	}
	pre, err := s.getGitRow(ctx, s.q, org.ID, id)
	if err != nil {
		return GitIntegration{}, err
	}
	// Plan the new values from the row read now; the transaction below
	// re-reads under a lock and refuses (409) if the row moved meanwhile.
	next := pre
	urlChanged := false
	if in.BaseURL.Set {
		n, err := normalizeGitlabBaseURL(in.BaseURL.V)
		if err != nil {
			return GitIntegration{}, err
		}
		if n != pre.BaseUrl {
			if err := s.checkGitScheme(ctx, "baseUrl", n); err != nil {
				return GitIntegration{}, err
			}
			next.BaseUrl, urlChanged = n, true
		}
	}
	if in.APIBaseURL.Set {
		n := ""
		if !in.APIBaseURL.Null {
			if n, err = normalizeAPIBase(in.APIBaseURL.V); err != nil {
				return GitIntegration{}, err
			}
		}
		if n != derefOr(pre.ApiBaseUrl, "") {
			if n != "" {
				if err := s.checkGitScheme(ctx, "apiBaseUrl", n); err != nil {
					return GitIntegration{}, err
				}
				next.ApiBaseUrl = &n
			} else {
				next.ApiBaseUrl = nil
			}
			urlChanged = true
		}
	}
	if urlChanged && pre.RepoCount > 0 {
		return GitIntegration{}, httpx.Errorf(http.StatusConflict, httpx.CodeReposMapped, "the URL cannot change while a repository is mapped; unmap them first")
	}
	if in.Name.Set {
		n, err := normalizeGitName(in.Name.V)
		if err != nil {
			return GitIntegration{}, err
		}
		if n == "" {
			next.Name = nil
		} else {
			next.Name = &n
		}
	}
	if in.Enabled.Set {
		next.Enabled = in.Enabled.V
	}
	// The token: a URL change clears it; "" clears it; a new one replaces it.
	setToken, tokenCleared := false, false
	var newToken string
	switch {
	case in.Token.Set && !in.Token.Null && in.Token.V == "":
		setToken, tokenCleared = true, true
	case in.HasToken():
		setToken, newToken = true, in.Token.V
	}
	if urlChanged && newToken == "" {
		if pre.TokenSet {
			setToken, tokenCleared = true, true
		}
	}
	next.TokenSet = pre.TokenSet
	if setToken {
		next.TokenSet = newToken != ""
	}
	if tokenCleared && newToken == "" {
		next.TokenSet = false
	}
	mode := pre.Mode
	if urlChanged && !next.TokenSet {
		mode = ModeWebhookOnly
	}
	if in.Mode.Set {
		switch in.Mode.V {
		case ModeToken:
			if !next.TokenSet {
				return GitIntegration{}, httpx.Invalid("mode token needs a stored token; send the token as well")
			}
			mode = ModeToken
		case ModeWebhookOnly:
			if newToken != "" {
				return GitIntegration{}, httpx.Invalid("a token is for token mode")
			}
			mode = ModeWebhookOnly
			if next.TokenSet {
				setToken, tokenCleared, next.TokenSet = true, true, false
			}
		default:
			return GitIntegration{}, httpx.Invalid("mode must be token or webhook_only")
		}
	} else if newToken != "" {
		mode = ModeToken
	}
	// Clearing the token alone forces webhook_only (the CHECK of the table).
	if !next.TokenSet {
		mode = ModeWebhookOnly
	}
	if mode == ModeToken && (pre.Mode != ModeToken || newToken != "") {
		if !pre.SecretSealed {
			return GitIntegration{}, httpx.Errorf(http.StatusConflict, httpx.CodeSecretHashOnly, "this integration keeps its webhook secret as a hash only; rotate the secret before using token mode")
		}
		if !s.gitPublicURLOK() {
			return GitIntegration{}, errPublicURLRequired()
		}
	}
	var tokenEnc []byte
	if newToken != "" {
		if s.gitSealer == nil {
			return GitIntegration{}, errEncryptionUnavailable()
		}
		if err := s.checkGitHostAllowed(ctx, "baseUrl", next.BaseUrl); err != nil {
			return GitIntegration{}, err
		}
		if next.ApiBaseUrl != nil {
			if err := s.checkGitHostAllowed(ctx, "apiBaseUrl", *next.ApiBaseUrl); err != nil {
				return GitIntegration{}, err
			}
		}
		if _, err := s.checkToken(ctx, pre.Provider, next.BaseUrl, derefOr(next.ApiBaseUrl, ""), newToken); err != nil {
			return GitIntegration{}, hostCallError(err)
		}
		tokenEnc = s.gitSealer.SealFor(colTokenEnc, id.String(), []byte(newToken))
	}
	flavor := pre.Flavor
	if urlChanged {
		f := flavorOf(pre.Provider, next.BaseUrl)
		flavor = &f
	}
	var out GitIntegration
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		// The webhook guard's lock first (decisions-T20 #65, entry 5).
		if err := LockTeamAccessGuard(ctx, q, org.ID); err != nil {
			return err
		}
		if err := lockGitProvider(ctx, tx, org.ID, pre.Provider); err != nil {
			return err
		}
		if _, err := q.LockGitIntegration(ctx, dbq.LockGitIntegrationParams{OrgID: org.ID, ID: id}); err != nil {
			return notFound(err, "integration")
		}
		cur, err := s.getGitRow(ctx, q, org.ID, id)
		if err != nil {
			return err
		}
		if !cur.UpdatedAt.Equal(pre.UpdatedAt) {
			return httpx.Errorf(http.StatusConflict, httpx.CodeIntegrationChanged, "the integration changed while this request ran; try again")
		}
		// Read again under the row lock: a repo map insert takes a SHARE lock on
		// this row, so it cannot slip in between this count and the update.
		if urlChanged && cur.RepoCount > 0 {
			return httpx.Errorf(http.StatusConflict, httpx.CodeReposMapped, "the URL cannot change while a repository is mapped; unmap them first")
		}
		if next.Enabled && !cur.Enabled {
			if err := checkGitLimit(ctx, q, org.ID, cur.Provider, id); err != nil {
				return err
			}
			h, err := q.GetGitSecretHashes(ctx, dbq.GetGitSecretHashesParams{OrgID: org.ID, ID: id})
			if err != nil {
				return fmt.Errorf("git secret hashes: %w", err)
			}
			if err := checkGitSecretFree(ctx, tx, q, org.ID, cur.Provider, id, h.WebhookSecretHash, h.WebhookSecretNextHash); err != nil {
				return err
			}
		}
		n, err := q.UpdateGitIntegration(ctx, dbq.UpdateGitIntegrationParams{
			OrgID: org.ID, ID: id, Name: next.Name, Flavor: flavor, BaseUrl: next.BaseUrl, ApiBaseUrl: next.ApiBaseUrl,
			Mode: mode, Enabled: next.Enabled, SetToken: setToken, TokenEnc: tokenEnc,
			ClearAttention: newToken != "" || tokenCleared,
		})
		if err != nil {
			if database.IsPgCode(err, database.CodeUniqueViolation) {
				return httpx.Errorf(http.StatusConflict, httpx.CodeIntegrationExists, "a %s integration for %s already exists", cur.Provider, next.BaseUrl)
			}
			return fmt.Errorf("update git integration: %w", err)
		}
		if n == 0 {
			return httpx.NotFound("integration")
		}
		// Enabling an integration turns its repositories with no team on
		// (decisions-T20 #65, entry 5).
		if next.Enabled && !cur.Enabled {
			if err := CheckWebhookGuard(ctx, q, org.ID); err != nil {
				return err
			}
		}
		out, err = s.getGitIntegration(ctx, q, org, id)
		return err
	})
	if err != nil {
		return GitIntegration{}, err
	}
	s.ForgetGitSecrets(id)
	return out, nil
}

// DeleteGitIntegration removes one connection (LAUN-104, decisions-T17 "LAUN-104").
// It is refused (409 repos_mapped) while a repository is mapped: unmapping
// deletes the token-mode hooks on the host, and this route makes no host call.
// The row goes with its own rows only, by the composite (org_id, id) foreign
// keys: repo map (empty by now), webhook events, deliveries ring and mr_links.
// No events.Emit: integration config is an exception (decisions-T17 #52).
func (s *Service) DeleteGitIntegration(ctx context.Context, org *auth.Org, id uuid.UUID) error {
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		// Same lock order as a PATCH: the provider lock, then the row. A repo
		// map insert takes a SHARE lock on the row, so none can slip in
		// between the count and the delete.
		row, err := s.getGitRow(ctx, q, org.ID, id)
		if err != nil {
			return err
		}
		if err := lockGitProvider(ctx, tx, org.ID, row.Provider); err != nil {
			return err
		}
		if _, err := q.LockGitIntegration(ctx, dbq.LockGitIntegrationParams{OrgID: org.ID, ID: id}); err != nil {
			return notFound(err, "integration")
		}
		cur, err := s.getGitRow(ctx, q, org.ID, id)
		if err != nil {
			return err
		}
		if cur.RepoCount > 0 {
			return httpx.Errorf(http.StatusConflict, httpx.CodeReposMapped, "repositories are mapped; unmap them first")
		}
		n, err := q.DeleteGitIntegration(ctx, dbq.DeleteGitIntegrationParams{OrgID: org.ID, ID: id})
		if err != nil {
			return fmt.Errorf("delete git integration: %w", err)
		}
		if n == 0 {
			return httpx.NotFound("integration")
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.ForgetGitSecrets(id)
	return nil
}
