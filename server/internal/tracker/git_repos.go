package tracker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/gitapi"
	"devops/internal/httpx"
	"devops/internal/scm"
)

// The repo map of a git integration (T17 routes table, LAUN-86): which host
// repositories send webhooks for the org, and (token mode) the hook Launchpad
// made on each. Rules: docs/internal/decisions-T17-git-connections-import.md
// (#8, #11, #12, #24, #28).
//
// Config writes emit no event (decisions-T17 #52). No route here returns a
// secret.

const (
	hookNone   = "none"
	hookActive = "active"
	hookFailed = "failed"

	maxRepoPathRunes = 500
	// maxAvailableRepos is the page size of available-repos (T17 routes table).
	maxAvailableRepos = 50
	maxRepoQueryRunes = 100
)

// repoKeyRule is what the API says about a repo key it refuses (T17 #8: the API
// validates it, in webhook-only mode too; scm.CanonicalRepoKey is the rule).
func repoKeyRule(provider string) string {
	if provider == "bitbucket" {
		return "repoKey must be the repository key the host sends in its webhook: the numeric repository id (Data Center) or {workspace uuid}/{repository uuid} with the braces (Cloud)"
	}
	return "repoKey must be the numeric repository id the host sends in its webhook"
}

// dcPathRE is a Data Center repo label, "PROJECTKEY/slug".
var dcPathRE = regexp.MustCompile(`^(~?[A-Za-z0-9_.-]{1,100})/([A-Za-z0-9_.-]{1,100})$`)

// gitRepoRef is the scm.RepoRef a client call takes for a stored repo. The
// stored key is the whole id for every host but Data Center, whose REST paths
// take the project key and slug: its id is the opaque "KEY/slug#id" that
// available-repos returned (T17 #15). A Data Center row whose label is not
// "KEY/slug" gets a ref the client refuses (ErrBadRepo).
func gitRepoRef(provider, key, path string) scm.RepoRef {
	id := key
	if provider == "bitbucket" && !scm.IsCloudRepoKey(key) && dcPathRE.MatchString(path) {
		id = path + "#" + key
	}
	return scm.RepoRef{ID: id, Key: key, Path: path}
}

// GitRepo is one mapped repository. Team, when set, limits linking to that
// team's issues. HookError is our own short code, never host text.
type GitRepo struct {
	ID            uuid.UUID `json:"id"`
	IntegrationID uuid.UUID `json:"integrationId"`
	RepoKey       string    `json:"repoKey"`
	RepoPath      string    `json:"repoPath"`
	Team          *TeamRef  `json:"team"`
	HookState     string    `json:"hookState"`
	HookError     *string   `json:"hookError"`
	CreatedAt     time.Time `json:"createdAt"`
}

// CreateGitRepoInput is the POST body. Token mode takes RepoID (from
// available-repos) and derives the key and the path from the host; webhook-only
// takes RepoKey and RepoPath. TeamID is a team id or key, or empty for "any
// team of the org".
type CreateGitRepoInput struct {
	RepoID   string `json:"repoId"`
	RepoKey  string `json:"repoKey"`
	RepoPath string `json:"repoPath"`
	TeamID   string `json:"teamId"`
}

// UpdateGitRepoInput is the PATCH body: the team only (null clears it).
type UpdateGitRepoInput struct {
	TeamID httpx.Opt[string] `json:"teamId"`
}

// GitRepoRemoved is the answer of an unmap. HookRemoved is true when the host
// confirmed the hook is gone (a host 404 counts); false means the admin must
// remove the hook by hand (webhook-only, or the host failed).
type GitRepoRemoved struct {
	HookRemoved bool `json:"hookRemoved"`
}

// AvailableRepo is one repository the token can see.
type AvailableRepo struct {
	RepoID   string `json:"repoId"`
	RepoKey  string `json:"repoKey"`
	RepoPath string `json:"repoPath"`
	URL      string `json:"url"`
	Mapped   bool   `json:"mapped"`
}

func toGitRepo(r dbq.ListGitReposRow) GitRepo {
	g := GitRepo{ID: r.ID, IntegrationID: r.IntegrationID, RepoKey: r.RepoKey, RepoPath: r.RepoPath,
		HookState: r.HookState, HookError: r.HookError, CreatedAt: r.CreatedAt}
	if r.TeamID != nil && r.TeamKey != nil && r.TeamName != nil {
		g.Team = &TeamRef{ID: *r.TeamID, Key: *r.TeamKey, Name: *r.TeamName}
	}
	return g
}

func (s *Service) integrationExists(ctx context.Context, orgID, id uuid.UUID) error {
	_, err := s.q.GetGitRotation(ctx, dbq.GetGitRotationParams{OrgID: orgID, ID: id})
	return notFound(err, "integration")
}

// ListGitRepos lists the mapped repos of an integration, oldest first.
func (s *Service) ListGitRepos(ctx context.Context, org *auth.Org, integrationID uuid.UUID) ([]GitRepo, error) {
	if err := s.integrationExists(ctx, org.ID, integrationID); err != nil {
		return nil, err
	}
	rows, err := s.q.ListGitRepos(ctx, dbq.ListGitReposParams{OrgID: org.ID, IntegrationID: integrationID})
	if err != nil {
		return nil, fmt.Errorf("list git repos: %w", err)
	}
	out := make([]GitRepo, 0, len(rows))
	for _, r := range rows {
		out = append(out, toGitRepo(r))
	}
	return out, nil
}

func (s *Service) getGitRepo(ctx context.Context, q *dbq.Queries, orgID, integrationID, id uuid.UUID) (dbq.ListGitReposRow, error) {
	rows, err := q.ListGitRepos(ctx, dbq.ListGitReposParams{OrgID: orgID, IntegrationID: integrationID, ID: &id})
	if err != nil {
		return dbq.ListGitReposRow{}, fmt.Errorf("get git repo: %w", err)
	}
	if len(rows) == 0 {
		return dbq.ListGitReposRow{}, httpx.NotFound("repository mapping")
	}
	return rows[0], nil
}

func validRepoPath(p string) bool {
	return p != "" && utf8.ValidString(p) && utf8.RuneCountInString(p) <= maxRepoPathRunes && !hasControl(p)
}

// resolveRepoTeam turns a team id or key into the team (400 for an unknown one).
func resolveRepoTeam(ctx context.Context, q *dbq.Queries, org *auth.Org, ref string) (*dbq.Team, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, nil
	}
	t, err := resolveTeam(ctx, q, org, ref)
	if err != nil {
		return nil, invalidRef(err, "unknown team %q", ref)
	}
	return &t, nil
}

// CreateGitRepo maps a repository. Token mode makes the hook first and the row
// second: a hook that fails leaves no row. A repo that is already mapped is 409.
func (s *Service) CreateGitRepo(ctx context.Context, org *auth.Org, integrationID uuid.UUID, in CreateGitRepoInput) (GitRepo, error) {
	pre, err := s.q.GetGitRotation(ctx, dbq.GetGitRotationParams{OrgID: org.ID, ID: integrationID})
	if err != nil {
		return GitRepo{}, notFound(err, "integration")
	}
	if pre.Mode == ModeToken {
		return s.createTokenRepo(ctx, org, integrationID, pre, in)
	}
	if in.RepoID != "" {
		return GitRepo{}, httpx.Invalid("repoId is for token mode; send repoKey and repoPath")
	}
	path := strings.TrimSpace(in.RepoPath)
	key, ok := scm.CanonicalRepoKey(pre.Provider, in.RepoKey)
	if !ok {
		return GitRepo{}, httpx.Invalid("%s", repoKeyRule(pre.Provider))
	}
	if !validRepoPath(path) {
		return GitRepo{}, httpx.Invalid("repoPath must be 1-%d characters with no control characters", maxRepoPathRunes)
	}
	return s.insertGitRepo(ctx, org, integrationID, nil, key, path, in.TeamID, nil)
}

func (s *Service) createTokenRepo(ctx context.Context, org *auth.Org, integrationID uuid.UUID, pre dbq.GetGitRotationRow, in CreateGitRepoInput) (GitRepo, error) {
	if in.RepoKey != "" || in.RepoPath != "" {
		return GitRepo{}, httpx.Invalid("in token mode the key and the path come from the host; send repoId only")
	}
	repoID := strings.TrimSpace(in.RepoID)
	if repoID == "" || len(repoID) > 200 {
		return GitRepo{}, httpx.Invalid("repoId is required in token mode (from available-repos)")
	}
	if s.gitSealer == nil {
		return GitRepo{}, errEncryptionUnavailable()
	}
	if !s.gitPublicURLOK() {
		return GitRepo{}, errPublicURLRequired()
	}
	secret, err := s.hookSecret(ctx, org.ID, pre)
	if err != nil {
		return GitRepo{}, err
	}
	client, _, err := s.openGitClient(ctx, org.ID, integrationID)
	if err != nil {
		return GitRepo{}, err
	}
	ref, err := client.GetRepo(ctx, repoID)
	if err != nil {
		if errors.Is(err, gitapi.ErrBadRepo) {
			return GitRepo{}, httpx.Invalid("repoId is not a repository id from available-repos")
		}
		if gitapi.CodeOf(err) == gitapi.CodeNotFound {
			return GitRepo{}, httpx.Invalid("the host does not know this repository for this token")
		}
		return GitRepo{}, s.repoCallError(ctx, org.ID, integrationID, err)
	}
	path := ref.Path
	key, ok := scm.CanonicalRepoKey(pre.Provider, ref.Key)
	if !ok || !validRepoPath(path) {
		return GitRepo{}, providerError(&gitapi.Error{Code: gitapi.CodeError})
	}
	mapped, err := s.q.GitRepoKeyMapped(ctx, dbq.GitRepoKeyMappedParams{OrgID: org.ID, IntegrationID: integrationID, RepoKey: key})
	if err != nil {
		return GitRepo{}, fmt.Errorf("git repo mapped: %w", err)
	}
	if mapped {
		return GitRepo{}, conflict("this repository is already mapped for this integration")
	}
	team, err := resolveRepoTeam(ctx, s.q, org, in.TeamID)
	if err != nil {
		return GitRepo{}, err
	}
	// The webhook guard before the host call, so a refused map makes no hook
	// (decisions-T20 #65, entry 6). insertGitRepo checks again under the lock.
	if team == nil && pre.Enabled {
		if err := s.precheckRepoWithoutTeam(ctx, org.ID); err != nil {
			return GitRepo{}, err
		}
	}
	h, err := client.EnsureHook(ctx, scm.RepoRef{ID: ref.ID, Key: key, Path: path, URL: ref.URL},
		scm.HookSpec{URL: s.gitWebhookURL(pre.Provider, org.Slug), Secret: secret, Reviews: true, Checks: true})
	if err != nil {
		s.setHookAttention(ctx, org.ID, integrationID, err)
		return GitRepo{}, hostCallError(err)
	}
	hid := h.ID
	// If the insert below fails the hook stays on the host: a retry adopts it
	// (list and match by URL). It is not deleted here, because a concurrent map
	// of the same repo shares that hook.
	return s.insertGitRepo(ctx, org, integrationID, &pre.UpdatedAt, key, path, in.TeamID, &hid)
}

// repoCallError maps a failed client call of a repo route to our code and
// leaves the attention code of #11a.
func (s *Service) repoCallError(ctx context.Context, orgID, id uuid.UUID, err error) error {
	if code := attentionFor(err); code != "" {
		_ = s.q.SetGitAttention(ctx, dbq.SetGitAttentionParams{OrgID: orgID, ID: id, AttentionCode: strp(code)})
	}
	return hostCallError(err)
}

// insertGitRepo writes the row. seen, when set, is the updated_at the secret
// was read at: if a rotation or a PATCH moved it since, the hook may hold a
// stale secret, so the map is refused (409) and the admin retries.
func (s *Service) insertGitRepo(ctx context.Context, org *auth.Org, integrationID uuid.UUID, seen *time.Time, key, path, teamRef string, hookID *string) (GitRepo, error) {
	var out GitRepo
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		// The webhook guard's lock first (decisions-T20 #65, entry 6).
		if err := LockTeamAccessGuard(ctx, q, org.ID); err != nil {
			return err
		}
		// The share lock keeps a URL change (which holds FOR UPDATE and counts
		// the mapped repos) from racing this insert.
		lk, err := q.LockGitIntegrationShared(ctx, dbq.LockGitIntegrationSharedParams{OrgID: org.ID, ID: integrationID})
		if err != nil {
			return notFound(err, "integration")
		}
		if seen != nil && !lk.UpdatedAt.Equal(*seen) {
			return httpx.Errorf(http.StatusConflict, httpx.CodeIntegrationChanged, "the integration changed while this request ran; try again")
		}
		team, err := resolveRepoTeam(ctx, q, org, teamRef)
		if err != nil {
			return err
		}
		row := dbq.InsertGitRepoParams{OrgID: org.ID, IntegrationID: integrationID, RepoKey: key, RepoPath: path,
			HookState: hookNone, HookID: hookID}
		if hookID != nil {
			row.HookState = hookActive
		}
		if team != nil {
			row.TeamID = &team.ID
		}
		id, err := q.InsertGitRepo(ctx, row)
		if err != nil {
			if database.IsPgCode(err, database.CodeUniqueViolation) {
				return conflict("this repository is already mapped for this integration")
			}
			return fmt.Errorf("insert git repo: %w", err)
		}
		if err := CheckWebhookGuard(ctx, q, org.ID); err != nil {
			return err
		}
		r, err := s.getGitRepo(ctx, q, org.ID, integrationID, id)
		if err != nil {
			return err
		}
		out = toGitRepo(r)
		return nil
	})
	return out, err
}

// UpdateGitRepo changes the team of a mapping (T5 #23).
func (s *Service) UpdateGitRepo(ctx context.Context, org *auth.Org, integrationID, id uuid.UUID, in UpdateGitRepoInput) (GitRepo, error) {
	if !in.TeamID.Set {
		return GitRepo{}, httpx.Invalid("teamId is required (null clears the team)")
	}
	var out GitRepo
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		// The webhook guard's lock first (decisions-T20 #65, entry 6).
		if err := LockTeamAccessGuard(ctx, q, org.ID); err != nil {
			return err
		}
		if _, err := s.getGitRepo(ctx, q, org.ID, integrationID, id); err != nil {
			return err
		}
		var teamID *uuid.UUID
		if !in.TeamID.Null {
			team, err := resolveRepoTeam(ctx, q, org, in.TeamID.V)
			if err != nil {
				return err
			}
			if team != nil {
				teamID = &team.ID
			}
		}
		n, err := q.UpdateGitRepoTeam(ctx, dbq.UpdateGitRepoTeamParams{OrgID: org.ID, IntegrationID: integrationID, ID: id, TeamID: teamID})
		if err != nil {
			return fmt.Errorf("update git repo: %w", err)
		}
		if n == 0 {
			return httpx.NotFound("repository mapping")
		}
		if err := CheckWebhookGuard(ctx, q, org.ID); err != nil {
			return err
		}
		r, err := s.getGitRepo(ctx, q, org.ID, integrationID, id)
		if err != nil {
			return err
		}
		out = toGitRepo(r)
		return nil
	})
	return out, err
}

// DeleteGitRepo unmaps a repository by its map row id. In token mode it deletes
// the hook the map made, by the stored hook id; a host 404 counts as done, any
// other host failure leaves the hook and answers hookRemoved=false (the row is
// deleted either way). allow, when set, is called before the host call (the rate
// limit). Links already made stay.
func (s *Service) DeleteGitRepo(ctx context.Context, org *auth.Org, integrationID, id uuid.UUID, allow func() error) (GitRepoRemoved, error) {
	row, err := s.getGitRepo(ctx, s.q, org.ID, integrationID, id)
	if err != nil {
		return GitRepoRemoved{}, err
	}
	removed := false
	if row.HookID != nil && *row.HookID != "" {
		// The host call takes the allowance of T17 #28; a webhook-only unmap makes none.
		if allow != nil {
			if err := allow(); err != nil {
				return GitRepoRemoved{}, err
			}
		}
		// A webhook-only integration made no hook (hook_id is NULL); an
		// integration switched to webhook-only keeps its ids but has no token.
		if client, integ, cerr := s.openGitClient(ctx, org.ID, integrationID); cerr == nil {
			derr := client.DeleteHook(ctx, gitRepoRef(integ.Provider, row.RepoKey, row.RepoPath), scm.HookRef{ID: *row.HookID})
			if derr == nil {
				removed = true
			} else if code := attentionFor(derr); code != "" {
				_ = s.q.SetGitAttention(ctx, dbq.SetGitAttentionParams{OrgID: org.ID, ID: integrationID, AttentionCode: strp(code)})
			}
		}
	}
	n, err := s.q.DeleteGitRepo(ctx, dbq.DeleteGitRepoParams{OrgID: org.ID, IntegrationID: integrationID, ID: id})
	if err != nil {
		return GitRepoRemoved{}, fmt.Errorf("delete git repo: %w", err)
	}
	if n == 0 {
		return GitRepoRemoved{}, httpx.NotFound("repository mapping")
	}
	return GitRepoRemoved{HookRemoved: removed}, nil
}

// AvailableGitRepos is one page (at most 50) of the repositories the token can
// see, through the guarded client. Token mode only. The cursor is opaque.
func (s *Service) AvailableGitRepos(ctx context.Context, org *auth.Org, integrationID uuid.UUID, query, cursor string) (httpx.Page[AvailableRepo], error) {
	if utf8.RuneCountInString(query) > maxRepoQueryRunes || hasControl(query) {
		return httpx.Page[AvailableRepo]{}, httpx.Invalid("q must be at most %d characters with no control characters", maxRepoQueryRunes)
	}
	client, integ, err := s.openGitClient(ctx, org.ID, integrationID)
	if err != nil {
		return httpx.Page[AvailableRepo]{}, err
	}
	provider := integ.Provider
	page, err := client.ListRepos(ctx, strings.TrimSpace(query), cursor)
	if err != nil {
		if errors.Is(err, gitapi.ErrInvalidCursor) {
			return httpx.Page[AvailableRepo]{}, httpx.Invalid("cursor is not valid")
		}
		return httpx.Page[AvailableRepo]{}, s.repoCallError(ctx, org.ID, integrationID, err)
	}
	keys, err := s.q.ListGitRepoKeys(ctx, dbq.ListGitRepoKeysParams{OrgID: org.ID, IntegrationID: integrationID})
	if err != nil {
		return httpx.Page[AvailableRepo]{}, fmt.Errorf("list git repo keys: %w", err)
	}
	mapped := make(map[string]bool, len(keys))
	for _, k := range keys {
		mapped[k] = true
	}
	items := make([]AvailableRepo, 0, len(page.Items))
	for _, r := range page.Items {
		if len(items) == maxAvailableRepos {
			break
		}
		key, ok := scm.CanonicalRepoKey(provider, r.Key)
		if !ok || !validRepoPath(r.Path) {
			continue // a host answer we would not store
		}
		items = append(items, AvailableRepo{RepoID: r.ID, RepoKey: key, RepoPath: r.Path, URL: safeWebURL(r.URL), Mapped: mapped[key]})
	}
	out := httpx.Page[AvailableRepo]{Items: items}
	if page.Next != "" {
		n := page.Next
		out.NextCursor = &n
	}
	return out, nil
}

// safeWebURL returns u when it is an http or https URL, else "": the web app
// links it, and a host must not hand it a javascript: URL.
func safeWebURL(u string) string {
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
		return ""
	}
	return u
}

// precheckRepoWithoutTeam answers the guard's 409 for a repository with no
// team on an enabled integration while restricted access exists, before a
// host call is made. It reads without the lock; the write checks again.
func (s *Service) precheckRepoWithoutTeam(ctx context.Context, orgID uuid.UUID) error {
	restricted, err := s.q.RestrictedAccessExists(ctx, orgID)
	if err != nil {
		return fmt.Errorf("restricted access: %w", err)
	}
	if !restricted {
		return nil
	}
	return httpx.Errorf(http.StatusConflict, httpx.CodeConflict,
		"some members see only selected teams, so every repository of an enabled integration needs a team; choose a team for this one")
}
