package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/dbq"
	"devops/internal/httpx"
)

// Store is the slice of dbq.Queries the middleware needs.
type Store interface {
	GetTokenByHash(ctx context.Context, tokenHash []byte) (dbq.GetTokenByHashRow, error)
	TouchTokenLastUsed(ctx context.Context, id uuid.UUID) error
	GetOrgBySlug(ctx context.Context, slug string) (dbq.Org, error)
	GetMembership(ctx context.Context, arg dbq.GetMembershipParams) (dbq.GetMembershipRow, error)
	ListMemberTeamIDs(ctx context.Context, arg dbq.ListMemberTeamIDsParams) ([]uuid.UUID, error)
}

// TokenSource pulls a credential out of a request. found=false means "this
// source has nothing here"; a non-empty raw value is then validated.
// Kinds lists the token kinds the source may carry. A non-empty CSRFCookie
// marks a source the browser attaches by itself (a cookie): its writes must
// also pass the double-submit check against that cookie. Bearer requests
// need none.
type TokenSource struct {
	Name       string
	Kinds      []string
	CSRFCookie string
	Extract    func(r *http.Request) (raw string, found bool)
}

// BearerSource reads "Authorization: Bearer odt_...". It carries PATs and
// native tokens (LAUN-122, LAUN-129): a native app gets one from POST
// /auth/otp/verify with client "native" and sends it here. It never carries
// a web session (kind session): that value is for the cookie, which has the
// CSRF check. A bearer request needs no CSRF header (no browser attaches it
// by itself).
var BearerSource = TokenSource{
	Name:  "bearer",
	Kinds: []string{KindPAT, KindNative},
	Extract: func(r *http.Request) (string, bool) {
		h := r.Header.Get("Authorization")
		if h == "" {
			return "", false
		}
		scheme, value, ok := strings.Cut(h, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			return h, true // present but not a bearer token: rejected as invalid
		}
		return strings.TrimSpace(value), true
	},
}

// Authenticator builds the auth middlewares.
type Authenticator struct {
	store   Store
	log     *slog.Logger
	now     func() time.Time
	sources []TokenSource
}

// NewAuthenticator returns an Authenticator. The bearer source comes first,
// so when a request carries both, the bearer token wins and the cookie is
// ignored. The session cookie has the __Host- names (SecureCookieNames);
// a local dev server switches with UseCookieNames.
func NewAuthenticator(store Store, log *slog.Logger) *Authenticator {
	return &Authenticator{store: store, log: log, now: time.Now,
		sources: []TokenSource{BearerSource, CookieSource(SecureCookieNames)}}
}

// UseCookieNames sets the session cookie names. Call it before serving.
func (a *Authenticator) UseCookieNames(n CookieNames) {
	a.sources = []TokenSource{BearerSource, CookieSource(n)}
}

// Authenticate resolves the token to a Principal or answers 401. A cookie
// write without a matching CSRF header is 403 csrf_failed, checked before the
// token is even looked up.
func (a *Authenticator) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, apiErr := a.resolve(r)
		if apiErr != nil {
			if apiErr.Status == http.StatusUnauthorized {
				w.Header().Set("WWW-Authenticate", `Bearer realm="launchpad"`)
			}
			if apiErr.Status == http.StatusInternalServerError {
				a.log.ErrorContext(r.Context(), "token lookup failed", "err", apiErr.Msg)
				apiErr = httpx.Errorf(http.StatusInternalServerError, httpx.CodeInternal, "internal error")
			}
			httpx.WriteError(w, apiErr.Status, apiErr.Code, apiErr.Msg)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

func (a *Authenticator) resolve(r *http.Request) (*Principal, *httpx.Error) {
	for _, src := range a.sources {
		raw, found := src.Extract(r)
		if !found {
			continue
		}
		if src.CSRFCookie != "" && !safeMethod(r.Method) && !csrfOK(r, raw, src.CSRFCookie) {
			return nil, httpx.Errorf(http.StatusForbidden, httpx.CodeCSRFFailed,
				"a cookie-authenticated write needs the %s header (the value of the %s cookie)", CSRFHeader, src.CSRFCookie)
		}
		p, apiErr := a.resolveToken(r.Context(), raw, src)
		if p != nil {
			p.Via = src.Name
			if src.CSRFCookie != "" {
				p.CSRFToken = CSRFToken(raw)
			}
		}
		return p, apiErr
	}
	return nil, httpx.Errorf(http.StatusUnauthorized, httpx.CodeUnauthenticated, "authentication required")
}

func (a *Authenticator) resolveToken(ctx context.Context, raw string, src TokenSource) (*Principal, *httpx.Error) {
	invalid := httpx.Errorf(http.StatusUnauthorized, httpx.CodeInvalidToken, "invalid token")
	if !WellFormed(raw) {
		return nil, invalid
	}
	row, err := a.store.GetTokenByHash(ctx, HashToken(raw))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, invalid
	}
	if err != nil {
		return nil, httpx.Errorf(http.StatusInternalServerError, httpx.CodeInternal, "%v", err)
	}
	if !slices.Contains(src.Kinds, row.Kind) || row.DeactivatedAt != nil {
		return nil, invalid
	}
	if row.RevokedAt != nil {
		return nil, httpx.Errorf(http.StatusUnauthorized, httpx.CodeTokenRevoked, "token has been revoked")
	}
	if row.ExpiresAt != nil && !a.now().Before(*row.ExpiresAt) {
		return nil, httpx.Errorf(http.StatusUnauthorized, httpx.CodeTokenExpired, "token has expired")
	}
	scopes, err := ParseScopes(row.Scopes)
	if err != nil {
		return nil, invalid
	}
	if err := a.store.TouchTokenLastUsed(ctx, row.ID); err != nil {
		// Not fatal: the request is authenticated either way.
		a.log.WarnContext(ctx, "token last_used_at update failed", "token_id", row.ID, "err", err)
	}
	return &Principal{
		UserID:          row.UserID,
		Username:        row.Username,
		Name:            row.UserName,
		Email:           row.UserEmail,
		IsAgent:         row.IsAgent,
		IsPlatformAdmin: row.IsPlatformAdmin,
		TokenID:         row.ID,
		TokenKind:       row.Kind,
		Scopes:          scopes,
		PinnedOrgID:     row.OrgID,
		ExpiresAt:       row.ExpiresAt,
	}, nil
}

// RequireOrgMember resolves {org} and checks the caller is a member and the
// token is not pinned to another org. Every failure is the same 404, so a
// caller cannot learn whether an org it cannot see exists.
func (a *Authenticator) RequireOrgMember(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		p, ok := PrincipalFrom(ctx)
		if !ok {
			httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthenticated, "authentication required")
			return
		}
		notFound := func() { httpx.WriteError(w, http.StatusNotFound, httpx.CodeNotFound, "org not found") }
		org, err := a.store.GetOrgBySlug(ctx, chi.URLParam(r, "org"))
		if errors.Is(err, pgx.ErrNoRows) {
			notFound()
			return
		}
		if err != nil {
			a.log.ErrorContext(ctx, "org lookup failed", "err", err)
			httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
			return
		}
		if p.PinnedOrgID != nil && *p.PinnedOrgID != org.ID {
			notFound()
			return
		}
		m, err := a.store.GetMembership(ctx, dbq.GetMembershipParams{OrgID: org.ID, UserID: p.UserID})
		if errors.Is(err, pgx.ErrNoRows) {
			notFound()
			return
		}
		if err != nil {
			a.log.ErrorContext(ctx, "membership lookup failed", "err", err)
			httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
			return
		}
		// The visible set (T20 #13): one more read, only for a restricted
		// member. Built from the user each request; nothing filters by it yet.
		var teamIDs []uuid.UUID
		if NeedsTeamRows(m.Role, m.TeamAccess) {
			teamIDs, err = a.store.ListMemberTeamIDs(ctx, dbq.ListMemberTeamIDsParams{OrgID: org.ID, UserID: p.UserID})
			if err != nil {
				a.log.ErrorContext(ctx, "team access lookup failed", "err", err)
				httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
				return
			}
		}
		o := &Org{ID: org.ID, Slug: org.Slug, Name: org.Name, Role: m.Role,
			Access: NewTeamAccess(m.Role, m.TeamAccess, teamIDs)}
		next.ServeHTTP(w, r.WithContext(WithOrg(ctx, o)))
	})
}

// RequireScope answers 403 unless the token grants s. For the admin scope
// inside an org, the caller must also be an org admin: a token never grants
// more than its owner has.
func RequireScope(s Scope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := PrincipalFrom(r.Context())
			if !ok {
				httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthenticated, "authentication required")
				return
			}
			if !p.HasScope(s) {
				httpx.WriteError(w, http.StatusForbidden, httpx.CodeInsufficientScope, "token lacks the "+string(s)+" scope")
				return
			}
			if s == ScopeAdmin {
				if o, ok := OrgFrom(r.Context()); ok && o.Role != "admin" {
					httpx.WriteError(w, http.StatusForbidden, httpx.CodeForbidden, "org admin role required")
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequirePlatformAdmin answers 403 forbidden unless the caller is a platform
// admin (users.is_platform_admin) AND the token has the admin scope AND the
// token is not pinned to an org. An org-pinned token is a credential for one
// org, so it never acts on the whole install, even when its owner could
// (docs/internal/decisions-T8-first-run.md). It needs no {org} and reads no
// org role: the platform-admin routes sit outside RequireOrgMember.
func RequirePlatformAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFrom(r.Context())
		if !ok {
			httpx.WriteError(w, http.StatusUnauthorized, httpx.CodeUnauthenticated, "authentication required")
			return
		}
		if !p.IsPlatformAdmin || !p.HasScope(ScopeAdmin) || p.PinnedOrgID != nil {
			httpx.WriteError(w, http.StatusForbidden, httpx.CodeForbidden,
				"platform admin required: a platform admin's token with the admin scope and no org pin")
			return
		}
		next.ServeHTTP(w, r)
	})
}
