// Package setup is first-run setup (docs/internal/decisions-T8-first-run.md).
// It replaces the old boot seed: an install with no user prints a one-time
// setup link to its log, and whoever opens it creates the install's one org
// in one transaction: the first user (a platform admin and the org's admin),
// the org, its first team with the default workflow, the default labels, a
// web session and the first admin token.
//
// One instance is one org. More teams come from POST /orgs/{org}/teams.
//
// The link is the only credential this server ever writes to its log. Only
// sha256(link token) is stored; the link lives 24 hours, works once, and a
// new link (a restart while no user exists, or `server setup-link`) closes
// every older one. Mint and completion run under one advisory lock, and the
// conditional UPDATE in ConsumeSetupToken decides a race between two
// completions with the same link.
package setup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/httpx"
	"devops/internal/login"
	"devops/internal/tracker"
)

// LinkTTL is how long a setup link works.
const LinkTTL = 24 * time.Hour

// secretBytes is the random part of a link token: 256 bits.
const secretBytes = 32

// secretLen is the base64url (no padding) length of secretBytes.
var secretLen = base64.RawURLEncoding.EncodedLen(secretBytes)

// LogPrefix starts the one log line that holds a setup link. deploy/lib.sh
// (app_logs_redacted) drops lines with it; keep the two equal.
const LogPrefix = "setup: open "

// The wire errors.
var (
	ErrInvalidToken = httpx.Errorf(http.StatusUnauthorized, httpx.CodeInvalidSetupToken,
		"the setup link is wrong, expired, used or replaced by a newer one; restart the server (or run `server setup-link`) for a new link")
	ErrComplete = httpx.Errorf(http.StatusConflict, httpx.CodeSetupComplete,
		"setup is complete: this install has a user")
)

// newSecret returns a fresh link token: 32 bytes from crypto/rand,
// base64url without padding (43 characters, safe in a URL fragment).
func newSecret() (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// hashSecret is the stored form of a link token.
func hashSecret(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// wellFormed reports whether s has the link-token shape. It runs before the
// lookup so junk never reaches the query.
func wellFormed(s string) bool {
	if len(s) != secretLen {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil && len(b) == secretBytes
}

// Link is the setup URL for secret: the token is in the fragment, so a
// browser never sends it to a server or a proxy log. With no base it is the
// bare path.
func Link(base, secret string) string {
	return strings.TrimRight(base, "/") + "/setup#" + secret
}

// LogLink writes the one setup line. It is the only credential the server
// logs (decisions-T8 #3).
func LogLink(log *slog.Logger, base, secret string) {
	if base == "" {
		log.Warn(LogPrefix + Link("", secret) + " on this server's web address (valid 24h; restart the app for a new link). " +
			"PUBLIC_BASE_URL is unset, so the link has no host: put the address you open the web app on in front of it")
		return
	}
	log.Warn(LogPrefix + Link(base, secret) + " (valid 24h; restart the app for a new link)")
}

// Required reports whether setup is open: the install has no user.
func Required(ctx context.Context, q *dbq.Queries) (bool, error) {
	req, err := q.SetupRequired(ctx)
	if err != nil {
		return false, fmt.Errorf("setup required: %w", err)
	}
	return req, nil
}

// MintIfRequired makes a new setup link when the install has no user, and
// closes every older link. minted is false (and secret "") once a user
// exists. It is safe with several instances: the advisory lock serialises
// them, and each mint closes the links before it, so only the newest works.
func MintIfRequired(ctx context.Context, pool *pgxpool.Pool) (secret string, minted bool, err error) {
	err = database.InTx(ctx, pool, func(tx pgx.Tx) error {
		secret, minted = "", false
		q := dbq.New(tx)
		if err := q.LockSetup(ctx); err != nil {
			return fmt.Errorf("setup lock: %w", err)
		}
		req, err := Required(ctx, q)
		if err != nil || !req {
			return err
		}
		if err := q.CloseSetupTokens(ctx); err != nil {
			return fmt.Errorf("close setup links: %w", err)
		}
		s, err := newSecret()
		if err != nil {
			return err
		}
		if _, err := q.InsertSetupToken(ctx, dbq.InsertSetupTokenParams{
			TokenHash: hashSecret(s), TtlSeconds: LinkTTL.Seconds(),
		}); err != nil {
			return fmt.Errorf("insert setup link: %w", err)
		}
		secret, minted = s, true
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return secret, minted, nil
}

// CheckLink answers whether token is a live setup link, without using it
// (LAUN-249: POST /setup/restore checks the link before it reads the upload).
// A malformed, wrong, expired, used or replaced link is ErrInvalidToken; a
// live link on an install that has a user is ErrComplete.
func CheckLink(ctx context.Context, pool *pgxpool.Pool, token string) error {
	if !wellFormed(token) {
		return ErrInvalidToken
	}
	q := dbq.New(pool)
	ok, err := q.PeekSetupToken(ctx, hashSecret(token))
	if err != nil {
		return fmt.Errorf("check setup link: %w", err)
	}
	if !ok {
		return ErrInvalidToken
	}
	req, err := Required(ctx, q)
	if err != nil {
		return err
	}
	if !req {
		return ErrComplete
	}
	return nil
}

// ConsumeLinkTx uses token inside the caller's transaction (which holds the
// setup lock). ErrInvalidToken when it is not live.
func ConsumeLinkTx(ctx context.Context, q *dbq.Queries, token string) error {
	if !wellFormed(token) {
		return ErrInvalidToken
	}
	if _, err := q.ConsumeSetupToken(ctx, hashSecret(token)); errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidToken
	} else if err != nil {
		return fmt.Errorf("consume setup link: %w", err)
	}
	return nil
}

// ErrUserNotFound and ErrNotEligible are GrantPlatformAdmin's refusals.
var (
	ErrUserNotFound = errors.New("no user with that username")
	ErrNotEligible  = errors.New("only an active human user can be a platform admin (not an agent, not a deactivated user)")
)

// GrantPlatformAdmin sets users.is_platform_admin for username: the upgrade
// step of an install that existed before first-run setup. already is true
// when the user was one before.
func GrantPlatformAdmin(ctx context.Context, pool *pgxpool.Pool, username string) (already bool, err error) {
	q := dbq.New(pool)
	u, err := q.GetUserByUsername(ctx, strings.ToLower(strings.TrimSpace(username)))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrUserNotFound
	}
	if err != nil {
		return false, fmt.Errorf("user lookup: %w", err)
	}
	if u.IsAgent || u.DeactivatedAt != nil {
		return false, ErrNotEligible
	}
	if u.IsPlatformAdmin {
		return true, nil
	}
	n, err := q.SetPlatformAdmin(ctx, u.ID)
	if err != nil {
		return false, fmt.Errorf("grant platform admin: %w", err)
	}
	if n != 1 {
		// Changed between the read and the write: say so rather than guess.
		return false, fmt.Errorf("the user changed while granting; run the command again")
	}
	return false, nil
}

// Options tune the Service. Zero values take the defaults.
type Options struct {
	// Floor is the least time POST /setup takes. Zero: none (tests).
	Floor time.Duration
	// The coarse per-process limit on POST /setup; default 30 a minute,
	// burst 10 (the /auth/otp values).
	PerMinute, Burst int
	SessionTTL       time.Duration // default 30 days, as a login
	Now              func() time.Time
}

// Service completes setup. It is safe for concurrent use.
type Service struct {
	pool  *pgxpool.Pool
	opts  Options
	limit *login.RouteLimit
}

// New returns a Service.
func New(pool *pgxpool.Pool, opts Options) *Service {
	if opts.PerMinute <= 0 {
		opts.PerMinute = 30
	}
	if opts.Burst <= 0 {
		opts.Burst = 10
	}
	if opts.SessionTTL <= 0 {
		opts.SessionTTL = 30 * 24 * time.Hour
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{pool: pool, opts: opts, limit: login.NewRouteLimit(opts.PerMinute, opts.Burst, opts.Now)}
}

// Required reports whether setup is open.
func (s *Service) Required(ctx context.Context) (bool, error) {
	return Required(ctx, dbq.New(s.pool))
}

// Pad sleeps until start+Floor (or ctx ends).
func (s *Service) Pad(ctx context.Context, start time.Time) {
	d := s.opts.Floor - time.Since(start)
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// validate checks and normalises the body. It runs before the transaction,
// so a typo never uses up the link.
func validate(in Request) (Request, error) {
	var err error
	// The users CHECKs, shared with POST /agents (tracker.ValidateUser).
	if in.User.Username, in.User.Name, err = tracker.ValidateUser(in.User.Username, in.User.Name); err != nil {
		var he *httpx.Error
		if errors.As(err, &he) {
			return in, httpx.Invalid("user.%s", he.Msg)
		}
		return in, err
	}
	email, err := login.NormalizeEmail(in.User.Email)
	if err != nil {
		return in, httpx.Invalid("user.email must be a valid email address")
	}
	in.User.Email = email
	if in.Org.Slug, in.Org.Name, err = tracker.ValidateOrg(in.Org.Slug, in.Org.Name); err != nil {
		return in, err
	}
	if in.Team.Key, in.Team.Name, err = tracker.ValidateTeam(in.Team.Key, in.Team.Name); err != nil {
		var he *httpx.Error
		if errors.As(err, &he) {
			return in, httpx.Invalid("team.%s", he.Msg)
		}
		return in, err
	}
	return in, nil
}

// Complete runs setup. Every failure leaves nothing behind: one
// transaction holds the link consumption and every row.
func (s *Service) Complete(ctx context.Context, in Request) (login.Session, Response, error) {
	if !wellFormed(in.Token) {
		return login.Session{}, Response{}, ErrInvalidToken
	}
	in, err := validate(in)
	if err != nil {
		return login.Session{}, Response{}, err
	}
	if err := s.limit.Take(); err != nil {
		return login.Session{}, Response{}, err
	}
	var (
		sess login.Session
		body Response
	)
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		if err := q.LockSetup(ctx); err != nil {
			return fmt.Errorf("setup lock: %w", err)
		}
		req, err := Required(ctx, q)
		if err != nil {
			return err
		}
		if !req {
			return ErrComplete
		}
		if _, err := q.ConsumeSetupToken(ctx, hashSecret(in.Token)); errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidToken
		} else if err != nil {
			return fmt.Errorf("consume setup link: %w", err)
		}
		email := in.User.Email
		u, err := q.InsertSetupUser(ctx, dbq.InsertSetupUserParams{Username: in.User.Username, Name: in.User.Name, Email: &email})
		if err != nil {
			return fmt.Errorf("insert user: %w", err)
		}
		org, err := q.InsertOrg(ctx, dbq.InsertOrgParams{Slug: in.Org.Slug, Name: in.Org.Name})
		if errors.Is(err, pgx.ErrNoRows) {
			// An org survives its users (no cascade): an install whose
			// users were all deleted can still hold one.
			return httpx.Errorf(http.StatusConflict, httpx.CodeConflict, "an org with slug %q exists already", in.Org.Slug)
		}
		if err != nil {
			return fmt.Errorf("insert org: %w", err)
		}
		if err := q.InsertOrgMember(ctx, dbq.InsertOrgMemberParams{OrgID: org.ID, UserID: u.ID, Role: "admin"}); err != nil {
			return fmt.Errorf("insert member: %w", err)
		}
		team, err := tracker.CreateTeamTx(ctx, q, org.ID, in.Team.Key, in.Team.Name, "")
		if err != nil {
			return err
		}
		if err := tracker.InsertDefaultLabelsTx(ctx, q, org.ID); err != nil {
			return err
		}
		now := s.opts.Now()
		if sess, err = login.InsertSessionTx(ctx, q, u.ID, login.SessionScopes(true), now.Add(s.opts.SessionTTL)); err != nil {
			return err
		}
		tok, err := insertFirstToken(ctx, q, u, org.ID)
		if err != nil {
			return err
		}
		body = Response{
			User:      login.User{ID: u.ID, Username: u.Username, Name: u.Name, Email: email, IsPlatformAdmin: true},
			Orgs:      []login.Org{{ID: org.ID, Slug: org.Slug, Name: org.Name, Role: "admin"}},
			CSRFToken: sess.CSRF,
			Token:     tok,
		}
		// Last write of the transaction (AGENTS.md conventions).
		_, err = events.Emit(ctx, q, org.ID, events.Event{
			Type: events.TeamCreated, EntityType: "team", EntityID: team.ID, ActorID: &u.ID,
			Payload: map[string]any{"id": team.ID, "key": team.Key},
		}, events.ScopeTeams(team.ID))
		return err
	})
	if err != nil {
		return login.Session{}, Response{}, err
	}
	return sess, body, nil
}

// firstTokenScopes are the scopes of the first admin token.
var firstTokenScopes = []string{string(auth.ScopeRead), string(auth.ScopeWrite), string(auth.ScopeAdmin)}

// insertFirstToken mints the first admin token by the existing PAT rules: a
// PAT is pinned to the org it was minted in (POST /orgs/{org}/tokens always
// pins), the admin scope needs the org admin role (the user has it), and a
// human PAT has no expiry unless asked for (decisions-T6a #18). It must not
// expire: no token outlives its minter (decisions-T6a #19), so an expiry
// here would cap every token minted with it (decisions-T8 #14).
func insertFirstToken(ctx context.Context, q *dbq.Queries, u dbq.InsertSetupUserRow, orgID uuid.UUID) (tracker.CreatedToken, error) {
	nt, err := auth.GenerateToken()
	if err != nil {
		return tracker.CreatedToken{}, err
	}
	row, err := q.InsertAPIToken(ctx, dbq.InsertAPITokenParams{
		UserID: u.ID, OrgID: &orgID, Kind: auth.KindPAT, Name: "first admin token", TokenHash: nt.Hash,
		Prefix: nt.Prefix, Scopes: slices.Clone(firstTokenScopes), CreatedBy: &u.ID,
	})
	if err != nil {
		return tracker.CreatedToken{}, fmt.Errorf("insert first token: %w", err)
	}
	return tracker.CreatedToken{
		Token: tracker.Token{ID: row.ID, Kind: row.Kind, Name: row.Name, Prefix: row.Prefix, Scopes: row.Scopes,
			User: tracker.UserRef{ID: u.ID, Username: u.Username, Name: u.Name}, CreatedAt: row.CreatedAt, ExpiresAt: row.ExpiresAt},
		Secret: nt.Plaintext,
	}, nil
}
