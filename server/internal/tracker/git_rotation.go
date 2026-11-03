package tracker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/gitapi"
	"devops/internal/httpx"
	"devops/internal/scm"
)

// Webhook secret rotation (T17 #11, LAUN-86). Hooks first, then the swap:
//
//   - rotate-secret stores a PENDING secret (sealed, with its hash). From then
//     on the webhook accepts the current AND the pending secret (GitHub: the
//     HMAC loop; GitLab: the hash compare), so no delivery is lost.
//   - webhook-only: the secret is shown once; the admin updates the host, then
//     calls commit.
//   - token mode: the server pushes the pending secret to every mapped repo's
//     hook, and commits itself when all of them took it. A hook that fails
//     leaves both secrets accepted; a retry reuses the pending secret and
//     pushes only to the repos that are not yet active.
//   - discard drops the pending secret.
//
// Every statement that changes a secret sets updated_at (the key of the verify
// cache) and calls ForgetGitSecrets. No secret is logged, emitted or stored in
// an idempotency row; the routes use idempotency.Reject.

// GitRotateResult is the answer of rotate-secret. WebhookSecret is set only in
// webhook-only mode (the secret is shown once); token mode never returns it.
// Committed is true when the swap is done (token mode, every hook updated).
// FailedRepos lists the repos whose hook is not on the pending secret yet.
type GitRotateResult struct {
	GitIntegration
	WebhookSecret string    `json:"webhookSecret,omitempty"`
	Committed     bool      `json:"committed"`
	FailedRepos   []GitRepo `json:"failedRepos"`
}

// gitWriteTimeout bounds the state writes that follow a host push. The post-push
// block shares ONE such budget, including the commit with repoCheckSilent, so a
// slow host or database can use it up. The push-lock wait uses gitapi.RouteTimeout
// as its own, separate timer.
const gitWriteTimeout = 15 * time.Second

// gitWriteCtx is the context of the writes after a push (LAUN-92). The hooks
// are already on the host, so a client that disconnects must not leave the
// rows behind: the writes outlive the request, with a short timeout.
func gitWriteCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), gitWriteTimeout)
}

// lockGitPush serialises the token-mode rotations of ONE integration for the
// whole push loop (LAUN-92): a late write of a stale run can never reach the
// host after a newer run. The lock is a transaction-level advisory lock on a
// connection the caller holds until release. Waiting is bounded by the route
// budget and by ctx.
func (s *Service) lockGitPush(ctx context.Context, id uuid.UUID) (release func(), err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("git push lock: %w", err)
	}
	release = func() {
		rctx, cancel := gitWriteCtx(ctx)
		defer cancel()
		_ = tx.Rollback(rctx)
	}
	wctx, cancel := context.WithTimeout(ctx, gitapi.RouteTimeout)
	defer cancel()
	if _, err := tx.Exec(wctx, `SELECT pg_advisory_xact_lock(hashtextextended('launchpad:git-push:' || $1::text, 0))`, id); err != nil {
		release()
		if ctx.Err() == nil && errors.Is(wctx.Err(), context.DeadlineExceeded) {
			return nil, conflict("another rotation of this integration is still running; try again")
		}
		return nil, fmt.Errorf("git push lock: %w", err)
	}
	return release, nil
}

// isErrCode reports whether err is an *httpx.Error with code.
func isErrCode(err error, code httpx.Code) bool {
	var he *httpx.Error
	return errors.As(err, &he) && he.Code == code
}

func errSecretUnreadable() error {
	return httpx.Errorf(http.StatusConflict, httpx.CodeEncryptionUnavailable,
		"a stored secret cannot be read (the encryption key changed); rotate or send the secret again")
}

// openGitBlob opens a sealed column of the row id; ok is false when there is no
// key or the blob does not open.
func (s *Service) openGitBlob(col string, id uuid.UUID, blob []byte) ([]byte, bool) {
	if s.gitSealer == nil || len(blob) == 0 {
		return nil, false
	}
	pt, err := s.gitSealer.OpenFor(col, id.String(), blob)
	if err != nil || len(pt) == 0 {
		return nil, false
	}
	return pt, true
}

func (s *Service) markUnreadable(ctx context.Context, orgID, id uuid.UUID) {
	_ = s.q.SetGitAttention(ctx, dbq.SetGitAttentionParams{OrgID: orgID, ID: id, AttentionCode: strp(AttentionSecretUnreadable)})
}

// openGitClient opens the stored token and builds the client of a token-mode
// integration. A webhook-only integration is 409; an unreadable token sets
// secret_unreadable and is 409 encryption_unavailable.
func (s *Service) openGitClient(ctx context.Context, orgID, id uuid.UUID) (scm.APIClient, dbq.GetGitIntegrationTokenRow, error) {
	row, err := s.q.GetGitIntegrationToken(ctx, dbq.GetGitIntegrationTokenParams{OrgID: orgID, ID: id})
	if err != nil {
		return nil, row, notFound(err, "integration")
	}
	if row.Mode != ModeToken || len(row.TokenEnc) == 0 {
		return nil, row, conflict("this integration has no token; this route works in token mode only")
	}
	if s.gitSealer == nil {
		return nil, row, errEncryptionUnavailable()
	}
	tok, ok := s.openGitBlob(colTokenEnc, id, row.TokenEnc)
	if !ok {
		s.markUnreadable(ctx, orgID, id)
		return nil, row, httpx.Errorf(http.StatusConflict, httpx.CodeEncryptionUnavailable,
			"the stored token cannot be read (the encryption key changed); send the token again")
	}
	c, err := s.newGitClient(row.Provider, row.BaseUrl, derefOr(row.ApiBaseUrl, ""), string(tok))
	wipe(tok)
	if err != nil {
		return nil, row, err
	}
	return c, row, nil
}

// hookSecret is the plaintext a NEW hook gets: the pending secret while a
// rotation is pending (so a repo mapped meanwhile is never left on the old
// one), else the current one. A hash-only row has none: 409 (T17 #11).
func (s *Service) hookSecret(ctx context.Context, orgID uuid.UUID, row dbq.GetGitRotationRow) (string, error) {
	col, blob := colWebhookSecretEnc, row.WebhookSecretEnc
	if len(row.WebhookSecretNextHash) > 0 {
		col, blob = colWebhookSecretNextEnc, row.WebhookSecretNextEnc
	}
	if len(blob) == 0 {
		return "", httpx.Errorf(http.StatusConflict, httpx.CodeSecretHashOnly, "this integration keeps its webhook secret as a hash only; rotate the secret before using token mode")
	}
	if s.gitSealer == nil {
		return "", errEncryptionUnavailable()
	}
	pt, ok := s.openGitBlob(col, row.ID, blob)
	if !ok {
		s.markUnreadable(ctx, orgID, row.ID)
		return "", errSecretUnreadable()
	}
	out := string(pt)
	wipe(pt)
	return out, nil
}

// hookErrCode is the short code stored for a failed hook (ours, never host text).
func hookErrCode(err error) string {
	if c := gitapi.CodeOf(err); c != "" {
		return string(c)
	}
	return string(gitapi.CodeUnreachable)
}

// setHookAttention records why a hook call failed (T17 #11a): the token was
// refused, the host was unreachable, or the hook itself failed. A rate limit
// says nothing about the integration and sets nothing.
func (s *Service) setHookAttention(ctx context.Context, orgID, id uuid.UUID, err error) {
	if gitapi.CodeOf(err) == gitapi.CodeRateLimited {
		return
	}
	code := attentionFor(err)
	if code == "" {
		code = AttentionHookFailed
	}
	_ = s.q.SetGitAttention(ctx, dbq.SetGitAttentionParams{OrgID: orgID, ID: id, AttentionCode: strp(code)})
}

// RotateGitSecret starts or retries a rotation. pasted is the optional secret
// of a GitLab webhook-only integration (as at create); everything else is made
// by the server.
func (s *Service) RotateGitSecret(ctx context.Context, org *auth.Org, id uuid.UUID, pasted string) (GitRotateResult, error) {
	pre, err := s.q.GetGitRotation(ctx, dbq.GetGitRotationParams{OrgID: org.ID, ID: id})
	if err != nil {
		return GitRotateResult{}, notFound(err, "integration")
	}
	if pre.Mode == ModeToken {
		if pasted != "" {
			return GitRotateResult{}, httpx.Invalid("secret is made by the server in token mode")
		}
		return s.rotateToken(ctx, org, id, pre)
	}
	return s.rotateWebhookOnly(ctx, org, id, pre, pasted)
}

func (s *Service) rotateWebhookOnly(ctx context.Context, org *auth.Org, id uuid.UUID, pre dbq.GetGitRotationRow, pasted string) (GitRotateResult, error) {
	secret := pasted
	if secret != "" {
		if pre.Provider != "gitlab" {
			return GitRotateResult{}, httpx.Invalid("only a GitLab integration takes a pasted secret")
		}
		if err := validateGitlabSecret(secret); err != nil {
			return GitRotateResult{}, err
		}
	}
	// GitHub and Bitbucket verify with the plaintext, so they need the key; a
	// GitLab secret is hashed and sealed only when the server has one.
	if pre.Provider != "gitlab" && s.gitSealer == nil {
		return GitRotateResult{}, errEncryptionUnavailable()
	}
	if secret == "" {
		var err error
		if secret, err = newGitSecret(); err != nil {
			return GitRotateResult{}, err
		}
	}
	hash := hashGitlabSecret(secret)
	var enc []byte
	if s.gitSealer != nil {
		enc = s.gitSealer.SealFor(colWebhookSecretNextEnc, id.String(), []byte(secret))
	}
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if err := lockGitProvider(ctx, tx, org.ID, pre.Provider); err != nil {
			return err
		}
		row, err := q.LockGitRotation(ctx, dbq.LockGitRotationParams{OrgID: org.ID, ID: id})
		if err != nil {
			return notFound(err, "integration")
		}
		if row.Mode != ModeWebhookOnly {
			return conflict("the integration changed mode while this request ran; try again")
		}
		if bytes.Equal(hash, row.WebhookSecretHash) {
			return conflict("this is the current secret; choose a different one")
		}
		if err := checkGitSecretFree(ctx, tx, q, org.ID, row.Provider, id, hash); err != nil {
			return err
		}
		// A second rotate-secret replaces a pending secret: in webhook-only mode
		// the server has pushed nothing it could forget (T17 #11).
		return q.SetGitPendingSecret(ctx, dbq.SetGitPendingSecretParams{OrgID: org.ID, ID: id, WebhookSecretNextHash: hash, NextEnc: enc})
	})
	if err != nil {
		return GitRotateResult{}, err
	}
	s.ForgetGitSecrets(id)
	gi, err := s.GetGitIntegration(ctx, org, id)
	if err != nil {
		return GitRotateResult{}, err
	}
	// A pasted secret is the caller's own: it is never sent back (as at create).
	shown := secret
	if pasted != "" {
		shown = ""
	}
	return GitRotateResult{GitIntegration: gi, WebhookSecret: shown, FailedRepos: []GitRepo{}}, nil
}

func (s *Service) rotateToken(ctx context.Context, org *auth.Org, id uuid.UUID, pre dbq.GetGitRotationRow) (GitRotateResult, error) {
	if s.gitSealer == nil {
		return GitRotateResult{}, errEncryptionUnavailable()
	}
	if !s.gitPublicURLOK() {
		return GitRotateResult{}, errPublicURLRequired()
	}
	// One rotation at a time pushes to the host for this integration.
	release, err := s.lockGitPush(ctx, id)
	if err != nil {
		return GitRotateResult{}, err
	}
	defer release()
	var secret []byte
	var hash []byte
	fresh := false
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if err := lockGitProvider(ctx, tx, org.ID, pre.Provider); err != nil {
			return err
		}
		row, err := q.LockGitRotation(ctx, dbq.LockGitRotationParams{OrgID: org.ID, ID: id})
		if err != nil {
			return notFound(err, "integration")
		}
		if row.Mode != ModeToken {
			return conflict("the integration changed mode while this request ran; try again")
		}
		if len(row.WebhookSecretNextHash) > 0 {
			// A retry reuses the pending secret: a hook already moved to it must
			// never hold a secret the server forgot.
			pt, ok := s.openGitBlob(colWebhookSecretNextEnc, id, row.WebhookSecretNextEnc)
			if !ok {
				return errSecretUnreadable()
			}
			secret, hash = pt, row.WebhookSecretNextHash
			return nil
		}
		str, err := newGitSecret()
		if err != nil {
			return err
		}
		secret, hash, fresh = []byte(str), hashGitlabSecret(str), true
		if err := checkGitSecretFree(ctx, tx, q, org.ID, row.Provider, id, hash); err != nil {
			return err
		}
		enc := s.gitSealer.SealFor(colWebhookSecretNextEnc, id.String(), secret)
		if err := q.SetGitPendingSecret(ctx, dbq.SetGitPendingSecretParams{OrgID: org.ID, ID: id, WebhookSecretNextHash: hash, NextEnc: enc}); err != nil {
			return err
		}
		return q.MarkGitHooksNotUpdated(ctx, dbq.MarkGitHooksNotUpdatedParams{OrgID: org.ID, IntegrationID: id})
	})
	defer func() { wipe(secret) }()
	if err != nil {
		if isErrCode(err, httpx.CodeEncryptionUnavailable) {
			s.markUnreadable(ctx, org.ID, id)
		}
		return GitRotateResult{}, err
	}
	s.ForgetGitSecrets(id)

	client, _, err := s.openGitClient(ctx, org.ID, id)
	if err != nil {
		return GitRotateResult{}, err
	}
	repos, err := s.q.ListGitRepos(ctx, dbq.ListGitReposParams{OrgID: org.ID, IntegrationID: id})
	if err != nil {
		return GitRotateResult{}, fmt.Errorf("list git repos: %w", err)
	}
	spec := scm.HookSpec{URL: s.gitWebhookURL(pre.Provider, org.Slug), Secret: string(secret), Reviews: true, Checks: true}
	hctx, cancel := context.WithTimeout(ctx, gitapi.RouteTimeout)
	defer cancel()
	var firstErr error
	var pushed []uuid.UUID // repos marked active in this run
	for _, r := range repos {
		if !fresh && r.HookState == hookActive {
			continue // already on the pending secret (a retry)
		}
		if hctx.Err() != nil {
			break // the rest stay "not_updated"; a retry covers them
		}
		ref := gitRepoRef(pre.Provider, r.RepoKey, r.RepoPath)
		h, herr := client.EnsureHook(hctx, ref, spec)
		upd := dbq.UpdateGitRepoHookParams{OrgID: org.ID, IntegrationID: id, ID: r.ID}
		if herr != nil {
			code := hookErrCode(herr)
			upd.HookState, upd.HookError, upd.HookID = hookFailed, &code, r.HookID
			if uerr := s.updateHookDetached(ctx, upd); uerr != nil {
				return GitRotateResult{}, fmt.Errorf("update git repo hook: %w", uerr)
			}
			if firstErr == nil {
				firstErr = herr
			}
			if c := gitapi.CodeOf(herr); c == gitapi.CodeUnauthorized || c == gitapi.CodeForbidden || c == gitapi.CodeRateLimited {
				break // every further call would fail the same way
			}
			continue
		}
		hid := h.ID
		upd.HookState, upd.HookID = hookActive, &hid
		if uerr := s.updateHookDetached(ctx, upd); uerr != nil {
			return GitRotateResult{}, fmt.Errorf("update git repo hook: %w", uerr)
		}
		pushed = append(pushed, r.ID)
	}
	// A discard or a newer rotation during the push makes this run's secret
	// stale: the hooks pushed here would hold a secret nobody accepts. The only
	// safe cases are "my hash is still pending" and "my hash is current and
	// nothing newer is pending". A failed read fails closed.
	// These writes outlive a client that disconnected during the push.
	wctx, wcancel := gitWriteCtx(ctx)
	defer wcancel()
	cur, cerr := s.q.GetGitRotation(wctx, dbq.GetGitRotationParams{OrgID: org.ID, ID: id})
	stillMine := cerr == nil && (bytes.Equal(cur.WebhookSecretNextHash, hash) ||
		(len(cur.WebhookSecretNextHash) == 0 && bytes.Equal(cur.WebhookSecretHash, hash)))
	if !stillMine {
		if len(pushed) > 0 {
			if merr := s.q.MarkGitHooksDiscarded(wctx, dbq.MarkGitHooksDiscardedParams{OrgID: org.ID, IntegrationID: id, Ids: pushed}); merr != nil {
				return GitRotateResult{}, fmt.Errorf("mark git hooks: %w", merr)
			}
		}
		if cerr != nil {
			return GitRotateResult{}, fmt.Errorf("re-read git rotation: %w", cerr)
		}
		return s.rotateResult(wctx, org, id, "", false)
	}
	if firstErr != nil {
		s.setHookAttention(wctx, org.ID, id, firstErr)
	}
	committed := false
	if firstErr == nil {
		if committed, err = s.commitRotation(wctx, org, id, hash, repoCheckSilent); err != nil {
			return GitRotateResult{}, err
		}
	}
	return s.rotateResult(wctx, org, id, "", committed)
}

// updateHookDetached writes one repo's hook state after its host call.
func (s *Service) updateHookDetached(ctx context.Context, upd dbq.UpdateGitRepoHookParams) error {
	wctx, cancel := gitWriteCtx(ctx)
	defer cancel()
	return s.q.UpdateGitRepoHook(wctx, upd)
}

func (s *Service) rotateResult(ctx context.Context, org *auth.Org, id uuid.UUID, secret string, committed bool) (GitRotateResult, error) {
	gi, err := s.GetGitIntegration(ctx, org, id)
	if err != nil {
		return GitRotateResult{}, err
	}
	rows, err := s.q.ListGitRepos(ctx, dbq.ListGitReposParams{OrgID: org.ID, IntegrationID: id})
	if err != nil {
		return GitRotateResult{}, fmt.Errorf("list git repos: %w", err)
	}
	failed := []GitRepo{}
	if !committed {
		for _, r := range rows {
			if r.HookState != hookActive {
				failed = append(failed, toGitRepo(r))
			}
		}
	}
	return GitRotateResult{GitIntegration: gi, WebhookSecret: secret, Committed: committed, FailedRepos: failed}, nil
}

// repoCheck says how commitRotation treats a repo whose hook is not active.
type repoCheck int

const (
	repoCheckNone   repoCheck = iota // webhook-only, or forced
	repoCheckSilent                  // token-mode rotate: not committed, no error
	repoCheckStrict                  // manual commit in token mode: 409 naming the repos
)

// commitRotation swaps the pending secret in (T17 #11): it opens the pending
// blob, seals it under the current column's AAD and, in ONE transaction, sets
// the current secret and hash and clears the pending ones and the attention
// code. expected, when set, makes it a compare and swap on the pending hash;
// check requires every mapped repo's hook to be active. It returns
// false (nothing changed) when there is nothing to commit or a check fails.
func (s *Service) commitRotation(ctx context.Context, org *auth.Org, id uuid.UUID, expected []byte, check repoCheck) (bool, error) {
	pre, err := s.q.GetGitRotation(ctx, dbq.GetGitRotationParams{OrgID: org.ID, ID: id})
	if err != nil {
		return false, notFound(err, "integration")
	}
	committed := false
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if err := lockGitProvider(ctx, tx, org.ID, pre.Provider); err != nil {
			return err
		}
		row, err := q.LockGitRotation(ctx, dbq.LockGitRotationParams{OrgID: org.ID, ID: id})
		if err != nil {
			return notFound(err, "integration")
		}
		if len(row.WebhookSecretNextHash) == 0 && expected != nil && bytes.Equal(expected, row.WebhookSecretHash) {
			// A force commit took this run's pending secret first: the swap
			// happened, and it was this secret (LAUN-92).
			committed = true
			return nil
		}
		if len(row.WebhookSecretNextHash) == 0 || (expected != nil && !bytes.Equal(expected, row.WebhookSecretNextHash)) {
			return nil
		}
		if check != repoCheckNone {
			// A repo mapped meanwhile is checked here, under the row lock.
			repos, err := q.ListGitRepos(ctx, dbq.ListGitReposParams{OrgID: org.ID, IntegrationID: id})
			if err != nil {
				return fmt.Errorf("list git repos: %w", err)
			}
			var bad []string
			for _, r := range repos {
				if r.HookState != hookActive {
					bad = append(bad, r.RepoPath)
				}
			}
			if len(bad) > 0 {
				if check == repoCheckStrict {
					return conflict("%d repositories have a hook that is not on the pending secret (%s); call rotate-secret to retry them, or send force to commit anyway",
						len(bad), strings.Join(bad[:min(len(bad), 5)], ", "))
				}
				return nil
			}
		}
		var enc []byte
		if len(row.WebhookSecretNextEnc) > 0 {
			if pt, ok := s.openGitBlob(colWebhookSecretNextEnc, id, row.WebhookSecretNextEnc); ok {
				if s.gitSealer != nil {
					enc = s.gitSealer.SealFor(colWebhookSecretEnc, id.String(), pt)
				}
				wipe(pt)
			} else if row.Provider != "gitlab" {
				return errSecretUnreadable()
			}
			// A GitLab row whose blob is unreadable swaps by hash alone: the hash
			// path needs nothing else and the old sealed copy no longer matches.
		}
		n, err := q.CommitGitSecret(ctx, dbq.CommitGitSecretParams{OrgID: org.ID, ID: id, SecretEnc: enc, ExpectedHash: row.WebhookSecretNextHash})
		if err != nil {
			return fmt.Errorf("commit git secret: %w", err)
		}
		committed = n == 1
		return nil
	})
	if err != nil {
		if isErrCode(err, httpx.CodeEncryptionUnavailable) {
			s.markUnreadable(ctx, org.ID, id)
		}
		return false, err
	}
	s.ForgetGitSecrets(id)
	return committed, nil
}

// CommitGitSecret is the commit route: the admin has updated the host (or, in
// token mode, wants the swap now). No pending secret is 409.
func (s *Service) CommitGitSecret(ctx context.Context, org *auth.Org, id uuid.UUID, force bool) (GitIntegration, error) {
	pre, err := s.q.GetGitRotation(ctx, dbq.GetGitRotationParams{OrgID: org.ID, ID: id})
	if err != nil {
		return GitIntegration{}, notFound(err, "integration")
	}
	check := repoCheckNone
	if pre.Mode == ModeToken && !force {
		// Hooks that did not move keep the old secret and would be refused.
		check = repoCheckStrict
	}
	ok, err := s.commitRotation(ctx, org, id, nil, check)
	if err != nil {
		return GitIntegration{}, err
	}
	if !ok {
		return GitIntegration{}, conflict("this integration has no pending secret; call rotate-secret first")
	}
	return s.GetGitIntegration(ctx, org, id)
}

// DiscardGitSecret drops a pending secret. It is idempotent. In token mode a
// hook that was already moved to the dropped secret is marked failed
// (secret_discarded): it answers 401 until the repo is refreshed or the secret
// is rotated again.
func (s *Service) DiscardGitSecret(ctx context.Context, org *auth.Org, id uuid.UUID) (GitIntegration, error) {
	pre, err := s.q.GetGitRotation(ctx, dbq.GetGitRotationParams{OrgID: org.ID, ID: id})
	if err != nil {
		return GitIntegration{}, notFound(err, "integration")
	}
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if err := lockGitProvider(ctx, tx, org.ID, pre.Provider); err != nil {
			return err
		}
		row, err := q.LockGitRotation(ctx, dbq.LockGitRotationParams{OrgID: org.ID, ID: id})
		if err != nil {
			return notFound(err, "integration")
		}
		n, err := q.DiscardGitPendingSecret(ctx, dbq.DiscardGitPendingSecretParams{OrgID: org.ID, ID: id})
		if err != nil {
			return fmt.Errorf("discard git secret: %w", err)
		}
		if n > 0 && row.Mode == ModeToken {
			return q.SettleGitHooksAfterDiscard(ctx, dbq.SettleGitHooksAfterDiscardParams{OrgID: org.ID, IntegrationID: id})
		}
		return nil
	})
	if err != nil {
		return GitIntegration{}, err
	}
	s.ForgetGitSecrets(id)
	return s.GetGitIntegration(ctx, org, id)
}
