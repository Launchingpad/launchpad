package login

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/httpx"
)

// Options tune the service. Zero values take the defaults below.
type Options struct {
	CodeTTL      time.Duration // default 10 minutes
	SessionTTL   time.Duration // default 30 days
	MaxAttempts  int           // wrong tries that lock a code; default 5
	CodesPerHour int           // per email; default 5

	// Coarse per-process limits on the two public routes.
	RequestsPerMinute, RequestBurst int // POST /auth/otp; default 30, 10
	VerifiesPerMinute, VerifyBurst  int // POST /auth/otp/verify; default 60, 20

	// Floor is the least time POST /auth/otp and /verify take, so a member
	// and a non-member answer alike. Zero: no floor (tests).
	Floor time.Duration

	// FixedCode, when set, is the code every request gets. Dev servers
	// only; config.Load refuses it anywhere else.
	FixedCode string

	// Pepper is the HMAC key of stored codes (AUTH_OTP_PEPPER), at least
	// MinPepperLen bytes. It is required: New turns login off without it.
	Pepper []byte

	// Available, when set, must also report true for login to be on. The
	// server passes MailSwitch.Ready, so login follows the mail
	// provider a platform admin picks. Nil: deliver alone decides.
	Available func() bool

	Now func() time.Time
}

func (o *Options) defaults() {
	if o.CodeTTL <= 0 {
		o.CodeTTL = 10 * time.Minute
	}
	if o.SessionTTL <= 0 {
		o.SessionTTL = 30 * 24 * time.Hour
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 5
	}
	if o.CodesPerHour <= 0 {
		o.CodesPerHour = 5
	}
	if o.RequestsPerMinute <= 0 {
		o.RequestsPerMinute = 30
	}
	if o.RequestBurst <= 0 {
		o.RequestBurst = 10
	}
	if o.VerifiesPerMinute <= 0 {
		o.VerifiesPerMinute = 60
	}
	if o.VerifyBurst <= 0 {
		o.VerifyBurst = 20
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// Deliver hands a code to the transport. It must not block for long: the
// production one enqueues on Queue.
type Deliver func(ctx context.Context, to, code string)

// Service issues and checks login codes. It is safe for concurrent use.
type Service struct {
	pool    *pgxpool.Pool
	log     *slog.Logger
	deliver Deliver // nil: login is off (no transport, or no pepper)
	opts    Options
	pepper  []byte

	emailKey  []byte // per-process HMAC key for the per-email limiter
	perEmail  *windowLimiter
	requests  *bucket
	verifies  *bucket
	dummySalt []byte
}

// New returns a Service. A nil deliver turns login off: the two public
// routes answer 503 login_unavailable, and logout still works. A pepper
// shorter than MinPepperLen also turns login off (fail closed; config.Load
// already refuses to boot without one, so this is a second line).
func New(pool *pgxpool.Pool, log *slog.Logger, deliver Deliver, opts Options) *Service {
	opts.defaults()
	if deliver != nil && len(opts.Pepper) < MinPepperLen {
		log.Error("login: OFF, the code pepper is missing or shorter than the minimum")
		deliver = nil
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	return &Service{
		pool: pool, log: log, deliver: deliver, opts: opts, pepper: opts.Pepper,
		emailKey: key,
		// 10000 live keys is far above what the route limit lets in per hour.
		perEmail:  newWindowLimiter(opts.CodesPerHour, time.Hour, 10000, opts.Now),
		requests:  newBucket(opts.RequestsPerMinute, opts.RequestBurst, opts.Now),
		verifies:  newBucket(opts.VerifiesPerMinute, opts.VerifyBurst, opts.Now),
		dummySalt: newSalt(),
	}
}

// InvalidCodeError is the one 401 invalid_code (a wrong, expired or used code).
// The route returns it for a refused native login from a browser (LAUN-137).
// A function, not an exported variable, so no importer can replace it.
func InvalidCodeError() error { return errInvalidCode }

// Errors the routes answer with.
var (
	errUnavailable = httpx.Errorf(http.StatusServiceUnavailable, httpx.CodeLoginUnavailable,
		"login is not available on this server (no mail transport configured)")
	errInvalidCode = httpx.Errorf(http.StatusUnauthorized, httpx.CodeInvalidCode,
		"the code is wrong, expired or already used; ask for a new one")
	errBadEmail400 = httpx.Invalid("email must be a valid email address")
	errBadCode400  = httpx.Invalid("code must be %d digits", CodeLen)
)

// errRateLimited is the wire error of every login 429 (code rate_limited).
var errRateLimited = httpx.Errorf(http.StatusTooManyRequests, httpx.CodeRateLimited, "too many login requests; try again later")

// RateLimitedError is a 429 with the time to wait. It unwraps to the
// rate_limited *httpx.Error, so httpx.WriteErr writes the usual envelope;
// the handler adds Retry-After from RetryAfterSeconds.
type RateLimitedError struct {
	Wait time.Duration
}

func (e *RateLimitedError) Error() string { return errRateLimited.Error() }

// Unwrap exposes the wire error.
func (e *RateLimitedError) Unwrap() error { return errRateLimited }

// RetryAfterSeconds is Wait rounded up to whole seconds, at least 1.
func (e *RateLimitedError) RetryAfterSeconds() int {
	s := int((e.Wait + time.Second - 1) / time.Second)
	return max(s, 1)
}

func rateLimited(ok bool, wait time.Duration) error {
	if ok {
		return nil
	}
	return &RateLimitedError{Wait: wait}
}

// Enabled reports whether codes can be issued: a transport, a pepper, and
// (when Options.Available is set) a mail provider that can send.
func (s *Service) Enabled() bool {
	return s.deliver != nil && (s.opts.Available == nil || s.opts.Available())
}

// Pad sleeps until start+Floor (or ctx ends), so every answer of a public
// login route takes at least Floor.
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

func (s *Service) limiterKey(email string) [32]byte {
	m := hmac.New(sha256.New, s.emailKey)
	m.Write([]byte(email))
	var k [32]byte
	copy(k[:], m.Sum(nil))
	return k
}

// RequestCode issues a code when email belongs to someone who may log in,
// and does nothing otherwise. The answer is the same either way: nil, or a
// rate-limit error that does not depend on membership.
func (s *Service) RequestCode(ctx context.Context, rawEmail string) error {
	if !s.Enabled() {
		return errUnavailable
	}
	email, err := NormalizeEmail(rawEmail)
	if err != nil {
		return errBadEmail400
	}
	if err := rateLimited(s.requests.Take()); err != nil {
		return err
	}
	// Per email, for EVERY email, member or not: a 429 says nothing.
	if err := rateLimited(s.perEmail.Take(s.limiterKey(email))); err != nil {
		return err
	}
	q := dbq.New(s.pool)
	u, err := q.GetLoginUserByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // invite-only: no code, no email, same answer
	}
	if err != nil {
		return fmt.Errorf("login user lookup: %w", err)
	}
	code, issued, err := s.issue(ctx, u.ID)
	if err != nil {
		return err
	}
	if issued && u.Email != nil {
		s.deliver(ctx, *u.Email, code)
	}
	return nil
}

// issue writes a new code for user, replacing any open one. issued is false
// when the durable hourly cap is reached: the caller still answers 202 (the
// per-email limiter normally refuses first; this cap also holds across
// restarts).
func (s *Service) issue(ctx context.Context, userID uuid.UUID) (code string, issued bool, err error) {
	if s.opts.FixedCode != "" {
		code = s.opts.FixedCode
	} else if code, err = newCode(); err != nil {
		return "", false, err
	}
	salt := newSalt()
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		issued = false
		q := dbq.New(tx)
		if _, err := q.LockUserForLogin(ctx, userID); err != nil {
			return fmt.Errorf("lock login user: %w", err)
		}
		now := s.opts.Now()
		if err := q.DeleteLoginCodesBefore(ctx, dbq.DeleteLoginCodesBeforeParams{UserID: userID, CreatedAt: now.Add(-24 * time.Hour)}); err != nil {
			return fmt.Errorf("delete old login codes: %w", err)
		}
		n, err := q.CountLoginCodesSince(ctx, dbq.CountLoginCodesSinceParams{UserID: userID, CreatedAt: now.Add(-time.Hour)})
		if err != nil {
			return fmt.Errorf("count login codes: %w", err)
		}
		if n >= int64(s.opts.CodesPerHour) {
			return nil
		}
		if err := q.CloseLiveLoginCodes(ctx, dbq.CloseLiveLoginCodesParams{UserID: userID, ConsumedAt: &now}); err != nil {
			return fmt.Errorf("close login codes: %w", err)
		}
		if _, err := q.InsertLoginCode(ctx, dbq.InsertLoginCodeParams{
			UserID: userID, CodeSalt: salt, CodeHash: hashCode(s.pepper, salt, code),
			CreatedAt: now, ExpiresAt: now.Add(s.opts.CodeTTL),
		}); err != nil {
			return fmt.Errorf("insert login code: %w", err)
		}
		issued = true
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return code, issued, nil
}

// Session is a successful login. Secret and CSRF go into cookies; Body is
// the response.
type Session struct {
	Secret    string
	CSRF      string
	ExpiresAt time.Time
	Body      Response
}

// Verify checks a code and, when it is good, mints a session. Every failure
// (unknown email, no open code, expired, locked, wrong) is the same 401.
func (s *Service) Verify(ctx context.Context, rawEmail, code string, native bool) (Session, error) {
	if !s.Enabled() {
		return Session{}, errUnavailable
	}
	email, err := NormalizeEmail(rawEmail)
	if err != nil {
		return Session{}, errBadEmail400
	}
	if !ValidCodeFormat(code) {
		return Session{}, errBadCode400
	}
	if err := rateLimited(s.verifies.Take()); err != nil {
		return Session{}, err
	}
	q := dbq.New(s.pool)
	u, err := q.GetLoginUserByEmail(ctx, email)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = codeMatches(s.pepper, s.dummySalt, make([]byte, sha256.Size), code) // same work as a real check
		return Session{}, errInvalidCode
	}
	if err != nil {
		return Session{}, fmt.Errorf("login user lookup: %w", err)
	}

	var sess Session
	ok := false
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		ok = false
		q := dbq.New(tx)
		now := s.opts.Now()
		row, err := q.GetLiveLoginCodeForUpdate(ctx, u.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("login code lookup: %w", err)
		}
		if int(row.Attempts) >= s.opts.MaxAttempts || !now.Before(row.ExpiresAt) {
			return nil // locked or expired: even the right code fails
		}
		if !codeMatches(s.pepper, row.CodeSalt, row.CodeHash, code) {
			// Committed with the 401 (fn returns nil), so the count sticks.
			if _, err := q.RecordFailedLoginAttempt(ctx, row.ID); err != nil {
				return fmt.Errorf("record login attempt: %w", err)
			}
			return nil
		}
		n, err := q.ConsumeLoginCode(ctx, dbq.ConsumeLoginCodeParams{ID: row.ID, ConsumedAt: &now})
		if err != nil {
			return fmt.Errorf("consume login code: %w", err)
		}
		if n != 1 {
			return nil // cannot happen under the row lock; fail closed
		}
		// The first proof of the email of an account made from a copied
		// invite link (decisions-T12 #21): whoever held the
		// link before the owner loses every session and token now.
		if u.EmailUnproven {
			if _, err := q.RevokeTokensOfUnprovenUser(ctx, u.ID); err != nil {
				return fmt.Errorf("revoke tokens of an unproven account: %w", err)
			}
			if err := q.ClearEmailUnproven(ctx, u.ID); err != nil {
				return fmt.Errorf("clear email_unproven: %w", err)
			}
		}
		orgs, err := q.ListOrgsForUser(ctx, u.ID)
		if err != nil {
			return fmt.Errorf("list orgs: %w", err)
		}
		// Same rule as a PAT: the admin scope only for someone who is an
		// org admin, or a platform admin, who may be in no org.
		// RequireScope still checks the role in each org, and
		// RequirePlatformAdmin checks the user's flag.
		body := Response{User: User{ID: u.ID, Username: u.Username, Name: u.Name, Email: email,
			IsPlatformAdmin: u.IsPlatformAdmin}, Orgs: make([]Org, 0, len(orgs))}
		admin := u.IsPlatformAdmin
		for _, o := range orgs {
			body.Orgs = append(body.Orgs, Org{ID: o.ID, Slug: o.Slug, Name: o.Name, Role: o.Role})
			admin = admin || o.Role == "admin"
		}
		if u.Email != nil {
			body.User.Email = *u.Email
		}
		kind, name := auth.KindSession, webSessionName
		if native {
			kind, name = auth.KindNative, nativeSessionName
		}
		ns, err := insertSessionTx(ctx, q, u.ID, kind, name, SessionScopes(admin), now.Add(s.opts.SessionTTL))
		if err != nil {
			return err
		}
		if native {
			ns.CSRF = "" // a bearer client has no cookie, so no CSRF value
		}
		body.CSRFToken = ns.CSRF
		ns.Body = body
		sess = ns
		ok = true
		return nil
	})
	if err != nil {
		return Session{}, err
	}
	if !ok {
		return Session{}, errInvalidCode
	}
	return sess, nil
}

// SessionScopes are the scopes of a web session: read and write, plus admin
// for an org admin or a platform admin.
func SessionScopes(admin bool) []string {
	scopes := []string{string(auth.ScopeRead), string(auth.ScopeWrite)}
	if admin {
		scopes = append(scopes, string(auth.ScopeAdmin))
	}
	return scopes
}

// InsertSessionTx mints a web session for user inside the caller's
// transaction: an unpinned api_tokens row of kind session. The secret is in
// the returned Session only (Body is the caller's to fill).
func InsertSessionTx(ctx context.Context, q *dbq.Queries, userID uuid.UUID, scopes []string, exp time.Time) (Session, error) {
	return insertSessionTx(ctx, q, userID, auth.KindSession, webSessionName, scopes, exp)
}

// Token names of the two kinds of login. The web login makes kind `session`
// (cookie only); the native login makes kind `native` (bearer only, LAUN-129).
const (
	webSessionName    = "web session"
	nativeSessionName = "native session" // LAUN-122
)

func insertSessionTx(ctx context.Context, q *dbq.Queries, userID uuid.UUID, kind, name string, scopes []string, exp time.Time) (Session, error) {
	nt, err := auth.GenerateToken()
	if err != nil {
		return Session{}, err
	}
	if _, err := q.InsertAPIToken(ctx, dbq.InsertAPITokenParams{
		UserID: userID, OrgID: nil, Kind: kind, Name: name,
		TokenHash: nt.Hash, Prefix: nt.Prefix, Scopes: scopes, CreatedBy: &userID, ExpiresAt: &exp,
	}); err != nil {
		return Session{}, fmt.Errorf("insert session: %w", err)
	}
	return Session{Secret: nt.Plaintext, CSRF: auth.CSRFToken(nt.Plaintext), ExpiresAt: exp}, nil
}

// Logout revokes the caller's session. A PAT cannot log out (403): it is
// revoked through DELETE /orgs/{org}/tokens/{id}.
func (s *Service) Logout(ctx context.Context, p *auth.Principal) error {
	if !auth.IsLoginKind(p.TokenKind) {
		return httpx.Errorf(http.StatusForbidden, httpx.CodeForbidden,
			"logout ends a login session; revoke a personal access token with DELETE /orgs/{org}/tokens/{id}")
	}
	if _, err := dbq.New(s.pool).RevokeSessionToken(ctx, dbq.RevokeSessionTokenParams{ID: p.TokenID, UserID: p.UserID}); err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}
