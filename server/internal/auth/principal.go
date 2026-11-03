package auth

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
)

// Scope is a token permission.
type Scope string

// Scopes are cumulative: admin implies write, and write implies read.
const (
	ScopeRead  Scope = "read"
	ScopeWrite Scope = "write"
	ScopeAdmin Scope = "admin"
)

var scopeRank = map[Scope]int{ScopeRead: 1, ScopeWrite: 2, ScopeAdmin: 3}

// ParseScopes validates and de-duplicates a scope list.
func ParseScopes(in []string) ([]Scope, error) {
	if len(in) == 0 {
		return nil, fmt.Errorf("at least one scope is required")
	}
	out := make([]Scope, 0, len(in))
	for _, s := range in {
		sc := Scope(s)
		if _, ok := scopeRank[sc]; !ok {
			return nil, fmt.Errorf("unknown scope %q (want read, write or admin)", s)
		}
		if !slices.Contains(out, sc) {
			out = append(out, sc)
		}
	}
	return out, nil
}

// Principal is the authenticated caller.
type Principal struct {
	UserID   uuid.UUID
	Username string
	Name     string
	Email    *string // the user's login email; nil for most agents
	IsAgent  bool
	// IsPlatformAdmin: the user runs this install (users.is_platform_admin).
	// It is the user's flag, not the token's: RequirePlatformAdmin
	// also wants the admin scope and no org pin.
	IsPlatformAdmin bool
	TokenID         uuid.UUID
	TokenKind       string
	Scopes          []Scope
	PinnedOrgID     *uuid.UUID
	// ExpiresAt is the token's own expiry (nil: none). A token minted by
	// this caller may not outlive it.
	ExpiresAt *time.Time
	// Via names the TokenSource that carried the token ("bearer", "cookie").
	Via string
	// CSRFToken is the session's CSRF token when Via is the cookie, else "".
	CSRFToken string
}

// HasScope reports whether the token grants s (directly or by implication).
func (p *Principal) HasScope(s Scope) bool {
	need := scopeRank[s]
	for _, have := range p.Scopes {
		if scopeRank[have] >= need {
			return true
		}
	}
	return false
}

// Org is the org resolved from the {org} path segment, with the caller's role.
type Org struct {
	ID   uuid.UUID
	Slug string
	Name string
	Role string // admin | member
	// Access is the set of teams the caller may see (T20 #13). It is built
	// from the user, never the token.
	Access TeamAccess
}

// TeamAccess is the visible set of teams. All is true for an org admin
// (whatever the flag) and for a member whose team_access is 'all'. IDs is
// sorted and empty when All.
type TeamAccess struct {
	All bool
	IDs []uuid.UUID
}

// NeedsTeamRows reports whether the member's team_members rows must be read.
// It fails closed: only an admin or the flag 'all' skips the read, so an
// unknown flag is treated as restricted. The HTTP and SSE paths both use it.
func NeedsTeamRows(role, flag string) bool { return role != "admin" && flag != "all" }

// NewTeamAccess builds the access from the role, the team_access flag and
// the member's team rows. An admin is always All. ids need not be sorted;
// the result is a sorted copy.
func NewTeamAccess(role, flag string, ids []uuid.UUID) TeamAccess {
	if role == "admin" || flag == "all" {
		return TeamAccess{All: true}
	}
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	return TeamAccess{IDs: out}
}

// Can reports whether the team is visible.
func (a TeamAccess) Can(teamID uuid.UUID) bool {
	if a.All {
		return true
	}
	_, found := slices.BinarySearchFunc(a.IDs, teamID, func(x, y uuid.UUID) int { return bytes.Compare(x[:], y[:]) })
	return found
}

// Args returns the two arguments of the scope predicate (T20 #15). ids is
// never nil, so it binds as an empty uuid[].
func (a TeamAccess) Args() (all bool, ids []uuid.UUID) {
	if a.All {
		return true, []uuid.UUID{}
	}
	return false, append([]uuid.UUID{}, a.IDs...)
}

// Empty reports a restricted caller with no team.
func (a TeamAccess) Empty() bool { return !a.All && len(a.IDs) == 0 }

type ctxKey int

const (
	principalKey ctxKey = iota
	orgKey
)

// WithPrincipal stores p in ctx.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

// PrincipalFrom returns the caller. It is always set behind Authenticate.
func PrincipalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalKey).(*Principal)
	return p, ok && p != nil
}

// WithOrg stores o in ctx.
func WithOrg(ctx context.Context, o *Org) context.Context {
	return context.WithValue(ctx, orgKey, o)
}

// OrgFrom returns the org. It is always set behind RequireOrgMember.
func OrgFrom(ctx context.Context) (*Org, bool) {
	o, ok := ctx.Value(orgKey).(*Org)
	return o, ok && o != nil
}
