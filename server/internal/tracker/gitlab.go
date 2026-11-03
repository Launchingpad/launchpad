package tracker

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/httpx"
)

// GitLab webhook token check and shared secret helpers (T5). The
// integration config and the repo map are git_integrations.go and git_repos.go;
// the webhook itself is gitlab_webhook.go. Rules: docs/internal/decisions-T5.md.
//
// The secret is write-only. It is stored as sha256 only (0001's
// git_integrations.webhook_secret_hash), never returned, logged, emitted
// or put in an idempotency row (the create route uses idempotency.Reject).

const (
	// A webhook secret is 32-256 printable ASCII characters with no space.
	// 32 random characters are far beyond brute force, which is what makes a
	// plain (unsalted, fast) sha256 a safe store for it, as for api_tokens.
	minGitlabSecretBytes = 32
	maxGitlabSecretBytes = 256

	maxGitlabBaseURLBytes = 2048
)

// GitHook is a webhook request whose token or signature matched an enabled
// integration of the org in its path.
type GitHook struct {
	OrgID         uuid.UUID
	OrgSlug       string
	IntegrationID uuid.UUID
}

// GitlabHook is GitHook, the name it had before GitHub (LAUN-5).
type GitlabHook = GitHook

// errWebhookToken is every webhook authentication failure: an unknown org,
// an org with no enabled integration, a missing token and a wrong one look
// the same.
var errWebhookToken = httpx.Errorf(http.StatusUnauthorized, httpx.CodeUnauthenticated, "invalid or missing X-Gitlab-Token")

// errWebhookSignature is every signed-body (GitHub) authentication failure:
// an unknown org, an org with no enabled integration of the provider, a
// missing or malformed signature header, a wrong signature and sealed secrets
// that cannot be opened look the same.
var errWebhookSignature = httpx.Errorf(http.StatusUnauthorized, httpx.CodeUnauthenticated, "invalid or missing webhook signature")

// ErrWebhookSignature is errWebhookSignature for the handler, which checks
// the signature header before it reads the body.
var ErrWebhookSignature error = errWebhookSignature

// validateGitlabSecret checks a secret's shape. The message never echoes it.
func validateGitlabSecret(s string) error {
	if len(s) < minGitlabSecretBytes || len(s) > maxGitlabSecretBytes {
		return httpx.Invalid("secret must be %d-%d characters", minGitlabSecretBytes, maxGitlabSecretBytes)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return httpx.Invalid("secret must be printable ASCII with no spaces")
		}
	}
	return nil
}

func hashGitlabSecret(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// normalizeGitlabBaseURL accepts an absolute http(s) URL with no user info,
// query or fragment, and returns it with a lower-case scheme and host, no
// default port (443 for https, 80 for http) and no trailing slash, so one
// instance cannot be added twice under two spellings.
func normalizeGitlabBaseURL(raw string) (string, error) {
	bad := httpx.Invalid("baseUrl must be an absolute http or https URL with no user info, query or fragment, of at most %d bytes", maxGitlabBaseURLBytes)
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || len(raw) > maxGitlabBaseURLBytes || hasControl(raw) || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || u.Opaque != "" {
		return "", bad
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", bad
	}
	host := strings.ToLower(u.Host)
	if p := u.Port(); (scheme == "https" && p == "443") || (scheme == "http" && p == "80") {
		host = strings.TrimSuffix(host, ":"+p)
	}
	if strings.HasSuffix(host, ":") { // "https://x:" has an empty port
		host = strings.TrimSuffix(host, ":")
	}
	return scheme + "://" + host + strings.TrimRight(u.EscapedPath(), "/"), nil
}

// AuthenticateGitlabWebhook checks X-Gitlab-Token against every enabled
// integration of the org in constant time (sha256 of the header, compared
// with subtle.ConstantTimeCompare against each stored hash; no early exit).
// It reads no request body. Every failure is the same 401.
func (s *Service) AuthenticateGitlabWebhook(ctx context.Context, orgSlug, token string) (GitlabHook, error) {
	if token == "" || len(token) > maxGitlabSecretBytes {
		return GitlabHook{}, errWebhookToken
	}
	org, err := s.q.GetOrgBySlug(ctx, orgSlug)
	if errors.Is(err, pgx.ErrNoRows) {
		return GitlabHook{}, errWebhookToken
	}
	if err != nil {
		return GitlabHook{}, fmt.Errorf("webhook org lookup: %w", err)
	}
	rows, err := s.q.ListGitlabSecretHashes(ctx, org.ID)
	if err != nil {
		return GitlabHook{}, fmt.Errorf("webhook secret lookup: %w", err)
	}
	sum := hashGitlabSecret(token)
	var match uuid.UUID
	found := 0
	for _, r := range rows {
		// The current and, during a rotation, the pending hash (T17 #11): both
		// are compared, with no early exit, and one integration counts once.
		eq := subtle.ConstantTimeCompare(sum, r.WebhookSecretHash) | subtle.ConstantTimeCompare(sum, r.WebhookSecretNextHash)
		if eq == 1 && found == 0 {
			match = r.ID
		}
		found |= eq
	}
	if found != 1 {
		// The org is returned (with the error) when it has enabled
		// integrations, so the deliveries ring can note the bad token (LAUN-87).
		if len(rows) > 0 {
			return GitlabHook{OrgID: org.ID, OrgSlug: org.Slug}, errWebhookToken
		}
		return GitlabHook{}, errWebhookToken
	}
	return GitlabHook{OrgID: org.ID, OrgSlug: org.Slug, IntegrationID: match}, nil
}
