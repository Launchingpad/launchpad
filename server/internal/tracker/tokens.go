package tracker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/httpx"
)

// Token management covers the tokens PINNED to the org in the path. A token
// made here is always pinned to that org, so an org admin can never mint or
// see a credential for another org. (Unpinned tokens are minted only by a
// future account screen; see docs/internal/decisions-T1.md.)

// TokenInput is the POST /tokens body.
type TokenInput struct {
	Name          string   `json:"name"`
	Scopes        []string `json:"scopes"`
	User          string   `json:"user"`          // username of the caller or an agent; default: the caller
	ExpiresInDays *int     `json:"expiresInDays"` // 1-365 and required for an agent; optional 1-3650 for a human
}

// Token lifetime limits (decisions-T6a #18-19, decisions-T11).
const (
	maxHumanTokenDays = 3650
	maxAgentTokenDays = 365
	// A token from a web session always expires, in 1-365 days (decisions-T11 #2).
	maxSessionTokenDays = 365
)

func toToken(r dbq.ListOrgTokensRow) Token {
	return Token{ID: r.ID, Kind: r.Kind, Name: r.Name, Prefix: r.Prefix, Scopes: r.Scopes,
		User:      UserRef{ID: r.UserID, Username: r.Username, Name: r.UserName},
		CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, LastUsedAt: r.LastUsedAt, RevokedAt: r.RevokedAt}
}

// ListTokens lists the tokens pinned to the org, newest first.
func (s *Service) ListTokens(ctx context.Context, org *auth.Org) ([]Token, error) {
	rows, err := s.q.ListOrgTokens(ctx, &org.ID)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	out := make([]Token, 0, len(rows))
	for _, r := range rows {
		out = append(out, toToken(r))
	}
	return out, nil
}

// CreateToken mints a PAT pinned to org. The secret is returned once.
//
// Rules (decisions-T6a #18-19, decisions-T11): a web session of an org admin
// may mint, but its token always expires (1-365 days) and never has the admin
// scope, so a stolen cookie cannot become a long-lived or admin credential;
// an agent token must expire (1-365 days); and a token made with a bearer
// token never outlives that token.
func (s *Service) CreateToken(ctx context.Context, org *auth.Org, p *auth.Principal, in TokenInput) (CreatedToken, error) {
	fromSession := auth.IsLoginKind(p.TokenKind)
	target := UserRef{ID: p.UserID, Username: p.Username, Name: p.Name}
	targetRole := org.Role
	targetIsAgent := p.IsAgent
	if in.User != "" && in.User != p.Username {
		m, err := s.q.GetOrgMemberByUsername(ctx, dbq.GetOrgMemberByUsernameParams{OrgID: org.ID, Username: strings.ToLower(in.User)})
		if err != nil {
			return CreatedToken{}, invalidRef(err, "user %q is not a member of this org", in.User)
		}
		// No impersonation: an admin may mint for an agent (the reviewer and
		// MCP bots), never for another human, whose secret they would hold.
		if !m.IsAgent {
			return CreatedToken{}, httpx.Errorf(http.StatusForbidden, httpx.CodeForbidden,
				"a token can be minted only for yourself or for an agent user")
		}
		target = UserRef{ID: m.ID, Username: m.Username, Name: m.Name}
		targetRole = m.Role
		targetIsAgent = m.IsAgent
	}
	name, strScopes, expires, err := s.tokenRules(in, targetRole, targetIsAgent, fromSession)
	if err != nil {
		return CreatedToken{}, err
	}
	// A bearer token never mints one that outlives it. A session is no
	// credential to copy (30 days, and the cookie dies at logout), so its
	// expiry does not cap the new token (decisions-T11 #5).
	if !fromSession && p.ExpiresAt != nil && (expires == nil || expires.After(*p.ExpiresAt)) {
		t := *p.ExpiresAt
		expires = &t
	}
	return s.insertPAT(ctx, org.ID, target, name, strScopes, &p.UserID, expires)
}

// tokenRules are the POST /tokens checks that do not depend on who mints:
// the name, the scopes, the admin scope only for an org admin, and the
// lifetime (an agent token must expire, 1-365 days; a human one may, 1-3650).
// A token minted by a web session (fromSession) must expire in 1-365 days
// and cannot have the admin scope (decisions-T11 #2-3).
// `server mint-token` applies exactly these with fromSession false
// (decisions-T10 #6).
func (s *Service) tokenRules(in TokenInput, targetRole string, targetIsAgent, fromSession bool) (string, []string, *time.Time, error) {
	name := strings.TrimSpace(in.Name)
	if n := utf8.RuneCountInString(name); n < 1 || n > 200 {
		return "", nil, nil, httpx.Invalid("name must be 1-200 characters")
	}
	scopes, err := auth.ParseScopes(in.Scopes)
	if err != nil {
		return "", nil, nil, httpx.Invalid("%v", err)
	}
	if fromSession && slices.Contains(scopes, auth.ScopeAdmin) {
		return "", nil, nil, httpx.Errorf(http.StatusForbidden, httpx.CodeForbidden,
			"a web session cannot make a token with the admin scope; use a personal access token or the server command")
	}
	if slices.Contains(scopes, auth.ScopeAdmin) && targetRole != "admin" {
		return "", nil, nil, httpx.Invalid("the admin scope needs a user with the org admin role")
	}
	maxDays := maxHumanTokenDays
	switch {
	case targetIsAgent:
		maxDays = maxAgentTokenDays
		if in.ExpiresInDays == nil {
			return "", nil, nil, httpx.Invalid("expiresInDays (1-%d) is required for an agent token", maxAgentTokenDays)
		}
	case fromSession:
		maxDays = maxSessionTokenDays
		if in.ExpiresInDays == nil {
			return "", nil, nil, httpx.Invalid("expiresInDays (1-%d) is required for a token made in the web app", maxSessionTokenDays)
		}
	}
	if fromSession {
		maxDays = min(maxDays, maxSessionTokenDays)
	}
	var expires *time.Time
	if in.ExpiresInDays != nil {
		if *in.ExpiresInDays < 1 || *in.ExpiresInDays > maxDays {
			return "", nil, nil, httpx.Invalid("expiresInDays must be 1-%d", maxDays)
		}
		t := s.now().Add(time.Duration(*in.ExpiresInDays) * 24 * time.Hour)
		expires = &t
	}
	strScopes := make([]string, len(scopes))
	for i, sc := range scopes {
		strScopes[i] = string(sc)
	}
	return name, strScopes, expires, nil
}

// insertPAT stores a new org-pinned PAT for target: only sha256 of the
// secret and its 12-character prefix are stored. The secret is returned once.
func (s *Service) insertPAT(ctx context.Context, orgID uuid.UUID, target UserRef, name string, scopes []string,
	createdBy *uuid.UUID, expires *time.Time) (CreatedToken, error) {
	nt, err := auth.GenerateToken()
	if err != nil {
		return CreatedToken{}, err
	}
	row, err := s.q.InsertAPIToken(ctx, dbq.InsertAPITokenParams{
		UserID: target.ID, OrgID: &orgID, Kind: auth.KindPAT, Name: name, TokenHash: nt.Hash,
		Prefix: nt.Prefix, Scopes: scopes, CreatedBy: createdBy, ExpiresAt: expires,
	})
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == database.CodeForeignKeyViolation && pg.ConstraintName == "api_tokens_user_id_fkey" {
		// The user was deleted (DELETE /agents/{user}, decisions-T47) after the lookup: not found.
		return CreatedToken{}, httpx.NotFound("user")
	}
	if err != nil {
		return CreatedToken{}, fmt.Errorf("insert token: %w", err)
	}
	return CreatedToken{
		Token: Token{ID: row.ID, Kind: row.Kind, Name: row.Name, Prefix: row.Prefix, Scopes: row.Scopes, User: target,
			CreatedAt: row.CreatedAt, ExpiresAt: row.ExpiresAt},
		Secret: nt.Plaintext,
	}, nil
}

// ErrMintNotFound is MintOperatorToken's answer for an unknown org, or a
// username that is not an active member of it.
var ErrMintNotFound = errors.New("not found")

// MintOperatorToken is `server mint-token` (decisions-T10 #5-8): the operator
// of the install, who can already reach the database, mints an org-pinned PAT
// for any active member of the org, an agent or a human (the rescue when
// every admin token is lost). There is no calling token, so there is no web
// session refusal, no impersonation rule and no "outlive the caller" cap;
// every other rule is tokenRules. created_by is NULL: no user minted it.
func (s *Service) MintOperatorToken(ctx context.Context, orgSlug string, in TokenInput) (CreatedToken, error) {
	org, err := s.q.GetOrgBySlug(ctx, strings.ToLower(strings.TrimSpace(orgSlug)))
	if errors.Is(err, pgx.ErrNoRows) {
		return CreatedToken{}, fmt.Errorf("org %q: %w", orgSlug, ErrMintNotFound)
	}
	if err != nil {
		return CreatedToken{}, fmt.Errorf("org lookup: %w", err)
	}
	username := strings.ToLower(strings.TrimSpace(in.User))
	m, err := s.q.GetOrgMemberByUsername(ctx, dbq.GetOrgMemberByUsernameParams{OrgID: org.ID, Username: username})
	if errors.Is(err, pgx.ErrNoRows) {
		return CreatedToken{}, fmt.Errorf("user %q is not an active member of org %q: %w", username, org.Slug, ErrMintNotFound)
	}
	if err != nil {
		return CreatedToken{}, fmt.Errorf("member lookup: %w", err)
	}
	name, scopes, expires, err := s.tokenRules(in, m.Role, m.IsAgent, false)
	if err != nil {
		return CreatedToken{}, err
	}
	return s.insertPAT(ctx, org.ID, UserRef{ID: m.ID, Username: m.Username, Name: m.Name}, name, scopes, nil, expires)
}

// RevokedToken is what RevokeToken tells the audit line. Never the secret.
type RevokedToken struct {
	ID        uuid.UUID
	Name      string
	Owner     string
	Scopes    []string
	ExpiresAt *time.Time
	// Newly is true for the call that revoked the token, false for a repeat.
	Newly bool
}

// RevokeToken revokes a token pinned to org. Revoking twice is a no-op
// (Newly is false the second time).
func (s *Service) RevokeToken(ctx context.Context, org *auth.Org, id uuid.UUID) (RevokedToken, error) {
	r, err := s.q.RevokeOrgToken(ctx, dbq.RevokeOrgTokenParams{OrgID: &org.ID, ID: id})
	if err != nil {
		return RevokedToken{}, notFound(err, "token")
	}
	return RevokedToken{ID: r.ID, Name: r.Name, Owner: r.OwnerUsername, Scopes: r.Scopes, ExpiresAt: r.ExpiresAt, Newly: r.NewlyRevoked}, nil
}

// Standing is a member's role and team access as read now.
type Standing struct {
	Role   string
	Access auth.TeamAccess
}

// StillAuthorized re-checks the credentials of a long-lived request (the
// SSE stream): the token must still be live, its user active, and the user
// still a member of the org. A stream ends at the first false. It also
// returns the member's current role and team access (T20 #13), read again on
// every call, so the event stream follows an access change (#56). A missing
// membership row is false, never an empty restricted set.
func (s *Service) StillAuthorized(ctx context.Context, p *auth.Principal, orgID uuid.UUID) (Standing, bool, error) {
	st, err := s.q.GetTokenAuthState(ctx, p.TokenID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Standing{}, false, nil
	}
	if err != nil {
		return Standing{}, false, fmt.Errorf("token state: %w", err)
	}
	if st.RevokedAt != nil || st.DeactivatedAt != nil || (st.ExpiresAt != nil && !s.now().Before(*st.ExpiresAt)) {
		return Standing{}, false, nil
	}
	if p.PinnedOrgID != nil && *p.PinnedOrgID != orgID {
		return Standing{}, false, nil
	}
	m, err := s.q.GetMembership(ctx, dbq.GetMembershipParams{OrgID: orgID, UserID: p.UserID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Standing{}, false, nil
		}
		return Standing{}, false, fmt.Errorf("membership: %w", err)
	}
	var ids []uuid.UUID
	if auth.NeedsTeamRows(m.Role, m.TeamAccess) {
		ids, err = s.q.ListMemberTeamIDs(ctx, dbq.ListMemberTeamIDsParams{OrgID: orgID, UserID: p.UserID})
		if err != nil {
			return Standing{}, false, fmt.Errorf("team access: %w", err)
		}
	}
	return Standing{Role: m.Role, Access: auth.NewTeamAccess(m.Role, m.TeamAccess, ids)}, true, nil
}

// ListEvents reads the caller's feed after seq, oldest first (T20 #55): an
// org admin reads every event, an unrestricted member every event but the
// admin ones, a restricted member only the events of teams they see and the
// org-level ones. The filter is in the query, before the limit.
func (s *Service) ListEvents(ctx context.Context, org *auth.Org, after int64, limit int32) ([]Event, error) {
	all, ids := org.Access.Args()
	rows, err := s.q.ListEventsAfter(ctx, dbq.ListEventsAfterParams{OrgID: org.ID, Seq: after, Limit: limit,
		IsAdmin: org.Role == "admin", AllTeams: all, TeamIds: ids})
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	out := make([]Event, 0, len(rows))
	for _, r := range rows {
		out = append(out, Event{Seq: r.Seq, Type: r.Type, EntityType: r.EntityType, EntityID: r.EntityID,
			Actor: userRef(r.ActorID, r.ActorUsername, r.ActorName), Payload: r.Payload, CreatedAt: r.CreatedAt})
	}
	return out, nil
}

// LatestEventSeq is the org's newest seq, or 0.
func (s *Service) LatestEventSeq(ctx context.Context, orgID uuid.UUID) (int64, error) {
	seq, err := s.q.LatestEventSeq(ctx, orgID)
	if err != nil {
		return 0, fmt.Errorf("latest event seq: %w", err)
	}
	return seq, nil
}
