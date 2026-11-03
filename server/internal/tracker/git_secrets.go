package tracker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/dbq"
	"devops/internal/scm"
)

// The signed-body webhook check of GitHub (and later Bitbucket), T17 #11 and
// #14. The signature selects the integration: every enabled integration of
// the provider in the org has its sealed secret opened (from a small
// in-process cache), and the HMAC of the body under each secret (the current
// one and, during a rotation, the pending one) is compared in constant time.

// Sealed column names, the AAD of a blob (platform.GitKeyring.OpenFor).
const (
	colWebhookSecretEnc     = "webhook_secret_enc"
	colWebhookSecretNextEnc = "webhook_secret_next_enc"
)

// GitSealer seals and opens the sealed columns of git_integrations. It is
// *platform.GitKeyring; the tracker takes the interface so it does not import
// platform.
type GitSealer interface {
	SealFor(column, rowID string, plaintext []byte) []byte
	OpenFor(column, rowID string, blob []byte) ([]byte, error)
}

// SetGitSealer sets the opener of the sealed git secrets. Nil (the default,
// no SETTINGS_ENCRYPTION_KEY) leaves every sealed secret unreadable. Set it
// before serving.
func (s *Service) SetGitSealer(k GitSealer) { s.gitSealer = k }

// ForgetGitSecrets drops the cached secrets of an integration. A PATCH,
// rotate, commit, discard or disable calls it; another instance learns of a
// change through updated_at.
func (s *Service) ForgetGitSecrets(id uuid.UUID) { s.gitCache.forget(id) }

// maxGitSecretCache bounds the cache: 8 enabled integrations per provider
// per org, one org per install, with room to spare.
const maxGitSecretCache = 256

// gitSecretCache keeps opened secrets as []byte (never a string, so they can
// be overwritten) keyed by integration id and updated_at. get returns copies
// that the caller wipes, so an eviction never wipes a secret in use.
type gitSecretCache struct {
	mu sync.Mutex
	m  map[uuid.UUID]gitSecretEntry
}

type gitSecretEntry struct {
	updatedAt time.Time
	secrets   [][]byte
}

func wipe(b []byte) { clear(b) }

func (c *gitSecretCache) get(id uuid.UUID, updatedAt time.Time) ([][]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[id]
	if !ok || !e.updatedAt.Equal(updatedAt) {
		return nil, false
	}
	out := make([][]byte, len(e.secrets))
	for i, s := range e.secrets {
		out[i] = append([]byte(nil), s...)
	}
	return out, true
}

func (c *gitSecretCache) put(id uuid.UUID, updatedAt time.Time, secrets [][]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[uuid.UUID]gitSecretEntry{}
	}
	c.dropLocked(id)
	for len(c.m) >= maxGitSecretCache {
		for k := range c.m {
			c.dropLocked(k)
			break
		}
	}
	keep := make([][]byte, len(secrets))
	for i, s := range secrets {
		keep[i] = append([]byte(nil), s...)
	}
	c.m[id] = gitSecretEntry{updatedAt: updatedAt, secrets: keep}
}

func (c *gitSecretCache) forget(id uuid.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropLocked(id)
}

func (c *gitSecretCache) dropLocked(id uuid.UUID) {
	if e, ok := c.m[id]; ok {
		for _, s := range e.secrets {
			wipe(s)
		}
		delete(c.m, id)
	}
}

// GitTargets is the first step of a signed-body webhook: the org and the
// enabled integrations of one provider in it.
type GitTargets struct {
	OrgID    uuid.UUID
	OrgSlug  string
	provider string
	rows     []dbq.ListGitSealedSecretsRow
}

// GitWebhookTargets looks up the org and its enabled integrations of provider
// (two indexed reads, nothing from the request body). An unknown org or no
// enabled integration is the same 401 as a bad signature.
func (s *Service) GitWebhookTargets(ctx context.Context, provider, orgSlug string) (GitTargets, error) {
	org, err := s.q.GetOrgBySlug(ctx, orgSlug)
	if errors.Is(err, pgx.ErrNoRows) {
		return GitTargets{}, errWebhookSignature
	}
	if err != nil {
		return GitTargets{}, fmt.Errorf("webhook org lookup: %w", err)
	}
	rows, err := s.q.ListGitSealedSecrets(ctx, dbq.ListGitSealedSecretsParams{OrgID: org.ID, Provider: provider})
	if err != nil {
		return GitTargets{}, fmt.Errorf("webhook secret lookup: %w", err)
	}
	if len(rows) == 0 {
		return GitTargets{}, errWebhookSignature
	}
	return GitTargets{OrgID: org.ID, OrgSlug: org.Slug, provider: provider, rows: rows}, nil
}

// secretsOf opens the current and the pending secret of one integration.
// unreadable is true when the current secret cannot be opened (or a pending
// one is stored and cannot be); the readable ones are still returned, so one
// bad blob never stops the others. The caller wipes the secrets.
func (s *Service) secretsOf(r dbq.ListGitSealedSecretsRow) (secrets [][]byte, unreadable bool) {
	if cached, ok := s.gitCache.get(r.ID, r.UpdatedAt); ok {
		return cached, false
	}
	var opened [][]byte
	open := func(col string, blob []byte) bool {
		if s.gitSealer == nil || len(blob) == 0 {
			return false
		}
		pt, err := s.gitSealer.OpenFor(col, r.ID.String(), blob)
		if err != nil || len(pt) == 0 {
			return false
		}
		opened = append(opened, pt)
		return true
	}
	curOK := open(colWebhookSecretEnc, r.WebhookSecretEnc)
	nextOK := len(r.WebhookSecretNextEnc) == 0 || open(colWebhookSecretNextEnc, r.WebhookSecretNextEnc)
	if curOK && nextOK {
		s.gitCache.put(r.ID, r.UpdatedAt, opened)
		return opened, false
	}
	return opened, true
}

// VerifyGitWebhook checks the signature of a delivery (header and body) against
// every enabled integration of the targets' provider and returns the one that
// matches. The HMAC of all secrets is computed with no early exit. A miss, or
// a signature that matches two integrations, is the same 401 as any other
// failure. unreadable lists the integrations with a sealed value that did not
// open (the handler logs our code secret_unreadable, T17 #33); they are
// skipped and the others still count.
func (s *Service) VerifyGitWebhook(t GitTargets, prov scm.Provider, h http.Header, body []byte) (hook GitHook, unreadable []uuid.UUID, err error) {
	var match uuid.UUID
	found := 0
	for _, r := range t.rows {
		secrets, bad := s.secretsOf(r)
		if bad {
			unreadable = append(unreadable, r.ID)
		}
		hit := false
		for _, sec := range secrets {
			if prov.Verify(h, body, sec) {
				hit = true
			}
			wipe(sec)
		}
		if hit {
			found++
			match = r.ID
		}
	}
	if found != 1 {
		return GitHook{}, unreadable, errWebhookSignature
	}
	return GitHook{OrgID: t.OrgID, OrgSlug: t.OrgSlug, IntegrationID: match}, unreadable, nil
}
