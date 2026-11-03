// Package signup is how a person who is not a user joins the install's org
// (docs/internal/decisions-T12-signups-invites.md):
//
//   - sign-up requests (platform_settings.signup_mode = approval): a person
//     asks, proves the email with a confirm code when mail works, and an org
//     admin approves or rejects. NO user, membership or session exists until
//     the approval;
//   - org invites (mode invite or approval): an org admin invites an email;
//     the one-time link makes the user (or adds the membership) and, for a
//     new user, starts a session.
//
// The public routes (POST /signups, /signups/confirm, /invites/accept) have
// the protection shape of POST /auth/otp: JSON only, no Idempotency-Key, a
// 300 ms floor, a global token bucket and per-client and per-email limits,
// and an answer that does not say whether an email or username exists.
//
// Secrets this package handles (confirm codes, invite tokens, sessions) are
// never logged, never put in an event or an idempotency row, and stored
// only as an HMAC (codes, with AUTH_OTP_PEPPER) or a sha256 (invite tokens).
package signup

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/httpx"
	"devops/internal/login"
	"devops/internal/tracker"
)

// Defaults (decisions-T12).
const (
	// DefaultPendingCap is the most pending sign-up requests an org holds
	// in its admin queue (confirmed, or made while mail did not work).
	DefaultPendingCap = 200
	// DefaultUnconfirmedCap is the most requests that wait for their email
	// confirm code (a separate count, so they never fill the admin queue).
	DefaultUnconfirmedCap = 1000
	// DefaultPendingTTL is how long a request in the admin queue stays
	// pending (from when it was made).
	DefaultPendingTTL = 30 * 24 * time.Hour
	// DefaultUnconfirmedTTL is how long a request that waits for its
	// confirm code lives.
	DefaultUnconfirmedTTL = 24 * time.Hour
	// DefaultMaxTotalCodeAttempts is the wrong tries a request has across
	// every code it gets, so asking again does not give more guesses.
	DefaultMaxTotalCodeAttempts = 10
	// DefaultCodeTTL is how long a confirm code works.
	DefaultCodeTTL = time.Hour
	// DefaultInviteTTL is how long an invite link works.
	DefaultInviteTTL = 7 * 24 * time.Hour
	// MaxNoteLen and MaxReasonLen are in characters.
	MaxNoteLen   = 1000
	MaxReasonLen = 1000
)

// Sign-up modes (platform_settings.signup_mode; the platform package
// validates them).
const (
	modeInvite   = "invite"
	modeApproval = "approval"
	modeClosed   = "closed"
)

// Options tune the Service. Zero values take the defaults.
type Options struct {
	// Floor is the least time each public route takes. Zero: none (tests).
	Floor time.Duration
	// Pepper is AUTH_OTP_PEPPER, the HMAC key of stored confirm codes.
	// Required: without it no confirm code is issued.
	Pepper []byte
	// PublicBaseURL is the web app's origin, the base of invite links.
	// Empty: links are bare paths.
	PublicBaseURL string
	// Mailer sends the email. Nil: no email is ever sent.
	Mailer Mailer
	// SyncMail sends each email inside the request instead of the queue
	// (tests only; it changes the timing).
	SyncMail bool

	PendingCap, UnconfirmedCap int
	PendingTTL, CodeTTL        time.Duration
	UnconfirmedTTL             time.Duration
	MaxTotalCodeAttempts       int // wrong tries across every code; default 10
	InviteTTL, SessionTTL      time.Duration
	MaxCodeAttempts            int // wrong tries that lock a code; default 5
	SignupPerMinute            int // POST /signups, global; default 30
	SignupBurst                int // default 10
	ConfirmPerMinute           int // POST /signups/confirm, global; default 60
	ConfirmBurst               int // default 20
	AcceptPerMinute            int // POST /invites/accept, global; default 30
	AcceptBurst                int // default 10
	SignupPerClientPerHour     int // default 10
	ConfirmPerClientPerHour    int // default 30
	AcceptPerClientPerHour     int // default 20
	SignupPerEmailPerHour      int // default 3
	KeyTableSize               int // keys per per-key limiter; default 10000 (tests set it small)
	ConfirmPerEmailPerHour     int // POST /signups/confirm, per email; default 5
	Now                        func() time.Time
}

func (o *Options) defaults() {
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	defD := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&o.PendingCap, DefaultPendingCap)
	def(&o.UnconfirmedCap, DefaultUnconfirmedCap)
	defD(&o.UnconfirmedTTL, DefaultUnconfirmedTTL)
	def(&o.MaxTotalCodeAttempts, DefaultMaxTotalCodeAttempts)
	defD(&o.PendingTTL, DefaultPendingTTL)
	defD(&o.CodeTTL, DefaultCodeTTL)
	defD(&o.InviteTTL, DefaultInviteTTL)
	defD(&o.SessionTTL, 30*24*time.Hour)
	def(&o.MaxCodeAttempts, 5)
	def(&o.SignupPerMinute, 30)
	def(&o.SignupBurst, 10)
	def(&o.ConfirmPerMinute, 60)
	def(&o.ConfirmBurst, 20)
	def(&o.AcceptPerMinute, 30)
	def(&o.AcceptBurst, 10)
	def(&o.SignupPerClientPerHour, 10)
	def(&o.ConfirmPerClientPerHour, 30)
	def(&o.AcceptPerClientPerHour, 20)
	def(&o.SignupPerEmailPerHour, 3)
	def(&o.KeyTableSize, DefaultKeyTableSize)
	def(&o.ConfirmPerEmailPerHour, 5)
	if o.Now == nil {
		o.Now = time.Now
	}
}

// Service runs sign-up requests and invites. It is safe for concurrent use.
type Service struct {
	pool *pgxpool.Pool
	log  *slog.Logger
	opts Options
	q    *queue // nil: no mailer

	signupLimit, confirmLimit, acceptLimit *login.RouteLimit
	signupClient, confirmClient            *login.KeyLimit
	acceptClient, signupEmail              *login.KeyLimit
	confirmEmail                           *login.KeyLimit
	dummySalt                              []byte
}

// DefaultKeyTableSize bounds each per-key limiter. A NEW key enters a table
// only after the route's global bucket let the request in (KeyLimit.
// TakeThen), so a table holds at most the keys the global bucket admits in
// one window: 1800 an hour for POST /signups, 3600 for the confirm. A
// flood of random emails or addresses cannot fill it (decisions-T12 #9).
const DefaultKeyTableSize = 10000

// New returns a Service. Call Run on a goroutine you own to send email.
func New(pool *pgxpool.Pool, log *slog.Logger, opts Options) *Service {
	opts.defaults()
	s := &Service{
		pool: pool, log: log, opts: opts,
		signupLimit:   login.NewRouteLimit(opts.SignupPerMinute, opts.SignupBurst, opts.Now),
		confirmLimit:  login.NewRouteLimit(opts.ConfirmPerMinute, opts.ConfirmBurst, opts.Now),
		acceptLimit:   login.NewRouteLimit(opts.AcceptPerMinute, opts.AcceptBurst, opts.Now),
		signupClient:  login.NewKeyLimit(opts.SignupPerClientPerHour, time.Hour, opts.KeyTableSize, opts.Now),
		confirmClient: login.NewKeyLimit(opts.ConfirmPerClientPerHour, time.Hour, opts.KeyTableSize, opts.Now),
		acceptClient:  login.NewKeyLimit(opts.AcceptPerClientPerHour, time.Hour, opts.KeyTableSize, opts.Now),
		signupEmail:   login.NewKeyLimit(opts.SignupPerEmailPerHour, time.Hour, opts.KeyTableSize, opts.Now),
		confirmEmail:  login.NewKeyLimit(opts.ConfirmPerEmailPerHour, time.Hour, opts.KeyTableSize, opts.Now),
		dummySalt:     login.NewSalt(),
	}
	if opts.Mailer != nil {
		s.q = newQueue(opts.Mailer, log, 64)
	}
	return s
}

// Run sends queued email until ctx ends. main owns the goroutine
// (golang-concurrency: an owner and an exit). Without a mailer it returns
// at once.
func (s *Service) Run(ctx context.Context) {
	if s.q == nil {
		return
	}
	s.q.run(ctx)
}

// Pad sleeps until start+Floor (or ctx ends), so every answer of a public
// route takes at least Floor.
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

// mailReady reports whether an email would go out now.
func (s *Service) mailReady() bool { return s.opts.Mailer != nil && s.opts.Mailer.Ready() }

// send queues j for to (or sends it now with SyncMail).
func (s *Service) send(ctx context.Context, to string, j mailJob) {
	if s.q == nil {
		return
	}
	j.to = to
	if s.opts.SyncMail {
		s.q.sendOne(ctx, j)
		return
	}
	s.q.enqueue(j)
}

// The wire errors.
var (
	// ErrSignupUnavailable is the one answer of the public sign-up routes
	// when the mode is not approval (and when the install has no org).
	ErrSignupUnavailable = httpx.Errorf(http.StatusForbidden, httpx.CodeSignupUnavailable,
		"sign-up requests are not open on this server; ask an org admin for an invite")
	// ErrInvitesClosed is the answer of the invite routes in mode closed.
	ErrInvitesClosed = httpx.Errorf(http.StatusForbidden, httpx.CodeSignupUnavailable,
		"invites are closed on this server (sign-up mode closed)")
	errApproveNeedsApproval = httpx.Errorf(http.StatusForbidden, httpx.CodeSignupUnavailable,
		"a sign-up request can be approved only while the sign-up mode is approval")
	// ErrInvalidCode: wrong, expired, used, locked or no request; not told
	// apart (the code of POST /auth/otp/verify).
	ErrInvalidCode = httpx.Errorf(http.StatusUnauthorized, httpx.CodeInvalidCode,
		"the code is wrong, expired or already used; ask for a new one by sending the sign-up request again")
	errQueueFull = &login.RateLimitedError{Wait: time.Hour}
	errBadEmail  = httpx.Invalid("email must be a valid email address")
)

// mode reads the install's sign-up mode.
func (s *Service) mode(ctx context.Context) (string, error) {
	m, err := dbq.New(s.pool).GetSignupMode(ctx)
	if err != nil {
		return "", fmt.Errorf("read sign-up mode: %w", err)
	}
	return m, nil
}

// requireApproval answers ErrSignupUnavailable unless the mode is approval.
func (s *Service) requireApproval(ctx context.Context) error {
	m, err := s.mode(ctx)
	if err != nil {
		return err
	}
	if m != modeApproval {
		return ErrSignupUnavailable
	}
	return nil
}

// validText trims s and checks it is at most max characters with no control
// character other than a newline or a tab.
func validText(field, s string, max int) (string, error) {
	s = strings.TrimSpace(s)
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > max {
		return "", httpx.Invalid("%s must be at most %d characters", field, max)
	}
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return "", httpx.Invalid("%s must not hold control characters", field)
		}
	}
	return s, nil
}

// validateCreate checks and normalises a POST /signups body.
func validateCreate(in CreateRequest) (CreateRequest, error) {
	email, err := login.NormalizeEmail(in.Email)
	if err != nil {
		return in, errBadEmail
	}
	in.Email = email
	if in.Username, in.Name, err = tracker.ValidateUser(in.Username, in.Name); err != nil {
		return in, err
	}
	if in.Note, err = validText("note", in.Note, MaxNoteLen); err != nil {
		return in, err
	}
	return in, nil
}

// limited runs the limits of a public route: the per-client limit (when
// the client is known), then the per-email limit (when email is not ""),
// then the global bucket. Each per-key limit uses KeyLimit.TakeThen: a key
// it knows is checked BEFORE the wider limits, so a refused caller does
// not use up the shared allowance; a NEW key is recorded only after the
// wider limits let the request in, so random keys cannot fill a table
// (decisions-T12 #9).
func limited(client *login.KeyLimit, clientKey string, email *login.KeyLimit, emailKey string, global func() error) error {
	next := global
	if email != nil && emailKey != "" {
		inner := next
		next = func() error { return email.TakeThen(emailKey, inner) }
	}
	if clientKey == "" {
		// Behind a proxy that is not trusted, one address may stand for
		// many people: no per-client key.
		return next()
	}
	return client.TakeThen(clientKey, next)
}

// codeReady reports whether a confirm code can be made and sent now.
func (s *Service) codeReady() bool {
	return s.mailReady() && len(s.opts.Pepper) >= login.MinPepperLen
}

// RequestSignup is POST /signups. It answers nil (202) whether or not the
// email exists, has a request, or is blocked: the answer never says. A
// taken username is not checked here (approval refuses it), so the confirm
// email goes out for it like for any new request. The errors are a bad body
// (400), the mode (403), a rate limit (429) and a full queue (429, the same
// for every email). client is the caller's address, used only as a limiter
// key ("" when unknown).
func (s *Service) RequestSignup(ctx context.Context, client string, in CreateRequest) error {
	if err := s.requireApproval(ctx); err != nil {
		return err
	}
	in, err := validateCreate(in)
	if err != nil {
		return err
	}
	if err := limited(s.signupClient, client, s.signupEmail, in.Email, s.signupLimit.Take); err != nil {
		return err
	}
	ready := s.codeReady()
	var (
		code       string
		hasAccount bool
	)
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		code, hasAccount = "", false
		q := dbq.New(tx)
		org, err := q.GetSignupOrg(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSignupUnavailable // no org yet: setup is not done
		}
		if err != nil {
			return fmt.Errorf("sign-up org: %w", err)
		}
		if err := q.LockSignups(ctx); err != nil {
			return fmt.Errorf("sign-up lock: %w", err)
		}
		if _, err := q.ExpireSignupRequests(ctx); err != nil {
			return fmt.Errorf("expire sign-up requests: %w", err)
		}
		// The cap a new row would use comes first, so a full queue is the
		// same answer for an email that is a user, blocked or new. While
		// mail works a new row waits for its code, in its own larger cap.
		limit := s.opts.PendingCap
		if ready {
			limit = s.opts.UnconfirmedCap
		}
		n, err := q.CountPendingSignups(ctx, dbq.CountPendingSignupsParams{OrgID: org.ID, Awaiting: ready})
		if err != nil {
			return fmt.Errorf("count sign-up requests: %w", err)
		}
		if n >= int64(limit) {
			return errQueueFull
		}
		isUser, err := q.SignupEmailIsUser(ctx, in.Email)
		if err != nil {
			return fmt.Errorf("sign-up check: %w", err)
		}
		if isUser {
			// Only the owner of the address sees this email, and it says
			// nothing they do not know.
			hasAccount = ready
			return nil
		}
		blocked, err := q.SignupEmailBlocked(ctx, dbq.SignupEmailBlockedParams{OrgID: org.ID, Email: in.Email})
		if err != nil {
			return fmt.Errorf("sign-up check: %w", err)
		}
		if blocked {
			return nil // same answer, nothing stored, no email
		}
		var id uuid.UUID
		row, err := q.GetPendingSignupByEmailForUpdate(ctx, dbq.GetPendingSignupByEmailForUpdateParams{OrgID: org.ID, Email: in.Email})
		switch {
		case err == nil:
			// A request is pending: keep it as it is (a second request
			// does not change its name or username). An unconfirmed one
			// gets a fresh code, so a lost email can be sent again; the
			// wrong tries of the request add up across its codes.
			if row.EmailVerifiedAt != nil || int(row.CodeAttemptsTotal) >= s.opts.MaxTotalCodeAttempts {
				return nil
			}
			id = row.ID
		case errors.Is(err, pgx.ErrNoRows):
			ttl := s.opts.PendingTTL
			if ready {
				ttl = s.opts.UnconfirmedTTL
			}
			nr, err := q.InsertSignupRequest(ctx, dbq.InsertSignupRequestParams{
				OrgID: org.ID, Email: in.Email, Name: in.Name, Username: in.Username, Note: in.Note,
				TtlSeconds: ttl.Seconds(),
			})
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // the one-pending index: cannot happen under the lock
			}
			if err != nil {
				return fmt.Errorf("insert sign-up request: %w", err)
			}
			id = nr.ID
		default:
			return fmt.Errorf("sign-up lookup: %w", err)
		}
		if !ready {
			return nil // shows to admins as email-unverified
		}
		c, err := login.NewCode()
		if err != nil {
			return err
		}
		salt := login.NewSalt()
		if err := q.SetSignupCode(ctx, dbq.SetSignupCodeParams{
			ID: id, CodeSalt: salt, CodeHash: login.HashCode(s.opts.Pepper, salt, c),
			TtlSeconds: s.opts.CodeTTL.Seconds(), UnconfirmedTtlSeconds: s.opts.UnconfirmedTTL.Seconds(),
		}); err != nil {
			return fmt.Errorf("set sign-up code: %w", err)
		}
		code = c
		return nil
	})
	if err != nil {
		return err
	}
	switch {
	case code != "":
		s.send(ctx, in.Email, confirmMail(code))
	case hasAccount:
		s.send(ctx, in.Email, existingAccountMail(s.opts.PublicBaseURL))
	}
	return nil
}

// ConfirmEmail is POST /signups/confirm. Every failure (no request, no
// code, expired, locked, wrong) is the same 401 invalid_code. A wrong code
// counts toward MaxCodeAttempts (this code) and MaxTotalCodeAttempts (the
// request). A confirmed request joins the admin queue: when that queue is
// full the answer is the cap's 429 and the code stays usable.
func (s *Service) ConfirmEmail(ctx context.Context, client string, in ConfirmRequest) error {
	if err := s.requireApproval(ctx); err != nil {
		return err
	}
	email, err := login.NormalizeEmail(in.Email)
	if err != nil {
		return errBadEmail
	}
	if !login.ValidCodeFormat(in.Code) {
		return httpx.Invalid("code must be %d digits", login.CodeLen)
	}
	if err := limited(s.confirmClient, client, s.confirmEmail, email, s.confirmLimit.Take); err != nil {
		return err
	}
	ok := false
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		ok = false
		q := dbq.New(tx)
		org, err := q.GetSignupOrg(ctx)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("sign-up org: %w", err)
		}
		if err := q.LockSignups(ctx); err != nil {
			return fmt.Errorf("sign-up lock: %w", err)
		}
		if _, err := q.ExpireSignupRequests(ctx); err != nil {
			return fmt.Errorf("expire sign-up requests: %w", err)
		}
		row, err := q.GetPendingSignupByEmailForUpdate(ctx, dbq.GetPendingSignupByEmailForUpdateParams{OrgID: org.ID, Email: email})
		if errors.Is(err, pgx.ErrNoRows) {
			_ = login.CodeMatches(s.opts.Pepper, s.dummySalt, make([]byte, sha256.Size), in.Code) // same work
			return nil
		}
		if err != nil {
			return fmt.Errorf("sign-up lookup: %w", err)
		}
		if row.CodeHash == nil || !row.CodeLive || int(row.CodeAttempts) >= s.opts.MaxCodeAttempts ||
			int(row.CodeAttemptsTotal) >= s.opts.MaxTotalCodeAttempts {
			return nil
		}
		if !login.CodeMatches(s.opts.Pepper, row.CodeSalt, row.CodeHash, in.Code) {
			// Committed with the 401 (fn returns nil), so the count sticks.
			if err := q.RecordSignupCodeAttempt(ctx, row.ID); err != nil {
				return fmt.Errorf("record sign-up code attempt: %w", err)
			}
			return nil
		}
		n, err := q.CountPendingSignups(ctx, dbq.CountPendingSignupsParams{OrgID: org.ID, Awaiting: false})
		if err != nil {
			return fmt.Errorf("count sign-up requests: %w", err)
		}
		if n >= int64(s.opts.PendingCap) {
			return errQueueFull // rolled back: the code still works later
		}
		c, err := q.ConfirmSignupEmail(ctx, dbq.ConfirmSignupEmailParams{ID: row.ID, PendingTtlSeconds: s.opts.PendingTTL.Seconds()})
		if err != nil {
			return fmt.Errorf("confirm sign-up email: %w", err)
		}
		ok = c == 1
		return nil
	})
	if err != nil {
		return err
	}
	if !ok {
		return ErrInvalidCode
	}
	return nil
}

// view is the admin view of a row: never the code or its hash.
func view(r dbq.SignupRequest) Request {
	return Request{
		ID: r.ID, Email: r.Email, Name: r.Name, Username: r.Username, Note: r.Note, Status: r.Status,
		EmailVerified: r.EmailVerifiedAt != nil, EmailVerifiedAt: r.EmailVerifiedAt,
		CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt, DecidedAt: r.DecidedAt, DecidedBy: r.DecidedBy,
		UserID: r.UserID, RejectReason: r.RejectReason, Blocked: r.Blocked,
	}
}

// ListLimit bounds a list (the newest rows first).
const (
	defaultListLimit = 100
	maxListLimit     = 500
)

// ParseLimit reads a ?limit= value: empty is the default, else 1-500.
func ParseLimit(raw string) (int32, error) {
	if raw == "" {
		return defaultListLimit, nil
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil || fmt.Sprint(n) != raw || n < 1 || n > maxListLimit {
		return 0, httpx.Invalid("limit must be a whole number from 1 to %d", maxListLimit)
	}
	return int32(n), nil
}

// ListRequests is GET /orgs/{org}/signups?status=&limit=. status is
// pending (the default), approved, rejected, expired or all. While mail
// works, a pending request shows only after its email is confirmed
// (decisions-T12 #8); without mail it shows with emailVerified false.
func (s *Service) ListRequests(ctx context.Context, org *auth.Org, status string, limit int32) ([]Request, error) {
	var st *string
	switch status {
	case "":
		p := StatusPending
		st = &p
	case "all":
	case StatusPending, StatusApproved, StatusRejected, StatusExpired:
		st = &status
	default:
		return nil, httpx.Invalid("status must be pending, approved, rejected, expired or all")
	}
	q := dbq.New(s.pool)
	if _, err := q.ExpireSignupRequests(ctx); err != nil {
		return nil, fmt.Errorf("expire sign-up requests: %w", err)
	}
	rows, err := q.ListSignupRequests(ctx, dbq.ListSignupRequestsParams{OrgID: org.ID, Status: st, Limit: limit, HideUnverified: s.mailReady()})
	if err != nil {
		return nil, fmt.Errorf("list sign-up requests: %w", err)
	}
	out := make([]Request, 0, len(rows))
	for _, r := range rows {
		out = append(out, view(r))
	}
	return out, nil
}

var errNoRequest = httpx.NotFound("sign-up request")

// errNotConfirmed: while mail works, only a request whose email was proved
// can be approved (decisions-T12 #8).
var errNotConfirmed = httpx.Errorf(http.StatusConflict, httpx.CodeConflict,
	"the email of this sign-up request is not confirmed yet; it can be approved after the person enters the code from the email")

// Approve is POST /orgs/{org}/signups/{id}/approve: in ONE transaction, the
// user (the request's username, name and email), the membership (role
// member) and the decision. A request approved already answers its view
// again and makes nothing (replay-safe); the row lock serialises two
// approvals at once. Rejected and expired requests are 409.
func (s *Service) Approve(ctx context.Context, org *auth.Org, p *auth.Principal, id uuid.UUID, in ApproveInput) (Request, error) {
	access, err := tracker.NewMemberAccess(in.TeamAccess, in.Teams)
	if err != nil {
		return Request{}, err
	}
	m, err := s.mode(ctx)
	if err != nil {
		return Request{}, err
	}
	if m != modeApproval {
		return Request{}, errApproveNeedsApproval
	}
	if _, err := dbq.New(s.pool).ExpireSignupRequests(ctx); err != nil {
		return Request{}, fmt.Errorf("expire sign-up requests: %w", err)
	}
	var (
		out     dbq.SignupRequest
		created bool
	)
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		created = false
		q := dbq.New(tx)
		if access == tracker.TeamAccessSelected {
			// The webhook guard's lock first (decisions-T20 #65, entry 3).
			if err := tracker.LockTeamAccessGuard(ctx, q, org.ID); err != nil {
				return err
			}
		}
		row, err := q.GetSignupRequestForUpdate(ctx, dbq.GetSignupRequestForUpdateParams{OrgID: org.ID, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return errNoRequest
		}
		if err != nil {
			return fmt.Errorf("lock sign-up request: %w", err)
		}
		switch row.Status {
		case StatusApproved:
			out = row
			return nil
		case StatusPending:
		default:
			return httpx.Errorf(http.StatusConflict, httpx.CodeConflict, "the sign-up request is %s; only a pending request can be approved", row.Status)
		}
		// A request waiting for its code (unverified, with a code) is hidden
		// and refused while mail works. A request made while mail was off
		// (no code) stays visible and approvable (decisions-T12 #8).
		if row.EmailVerifiedAt == nil && row.CodeHash != nil && s.mailReady() {
			return errNotConfirmed
		}
		var teams []uuid.UUID
		if access == tracker.TeamAccessSelected {
			if teams, err = tracker.ResolveAccessTeams(ctx, q, org, in.Teams); err != nil {
				return err
			}
		}
		isUser, err := q.SignupEmailIsUser(ctx, row.Email)
		if err != nil {
			return fmt.Errorf("sign-up email check: %w", err)
		}
		if isUser {
			return httpx.Errorf(http.StatusConflict, httpx.CodeConflict, "a user with this email exists already; reject the request")
		}
		email := row.Email
		// An email nobody proved (approved while mail was off) marks the
		// account email_unproven: its first email-code login revokes what
		// it had before (decisions-T12 #21).
		u, err := q.InsertInvitedUser(ctx, dbq.InsertInvitedUserParams{Username: row.Username, Name: row.Name, Email: &email,
			EmailUnproven: row.EmailVerifiedAt == nil})
		if errors.Is(err, pgx.ErrNoRows) { // ON CONFLICT (username) DO NOTHING
			return httpx.Errorf(http.StatusConflict, httpx.CodeConflict,
				"a user with username %q exists already; reject the request and ask the person to send it again with another username", row.Username)
		}
		if database.IsPgCode(err, database.CodeUniqueViolation) {
			return httpx.Errorf(http.StatusConflict, httpx.CodeConflict, "a user with this email exists already; reject the request")
		}
		if err != nil {
			return fmt.Errorf("insert user: %w", err)
		}
		if err := q.InsertOrgMember(ctx, dbq.InsertOrgMemberParams{OrgID: org.ID, UserID: u.ID, Role: "member"}); err != nil {
			return fmt.Errorf("insert member: %w", err)
		}
		if access == tracker.TeamAccessSelected {
			if err := tracker.GrantNewMemberTeams(ctx, q, org.ID, u.ID, teams); err != nil {
				return err
			}
		}
		uid := p.UserID
		if out, err = q.ApproveSignupRequest(ctx, dbq.ApproveSignupRequestParams{ID: row.ID, DecidedBy: &uid, UserID: &u.ID}); err != nil {
			return fmt.Errorf("approve sign-up request: %w", err)
		}
		if access == tracker.TeamAccessSelected {
			// A restricted member now: no enabled repository may lack a team
			// (#65, entry 3). An admin route: the refusal names them.
			if err := tracker.CheckWebhookGuard(ctx, q, org.ID); err != nil {
				return err
			}
		}
		created = true
		return nil
	})
	if err != nil {
		return Request{}, err
	}
	if created {
		s.log.InfoContext(ctx, "audit: sign-up approved", "org", org.Slug, "actor", p.Username,
			"request", out.ID, "user", out.UserID, "team_access", access, "teams", len(in.Teams))
		if s.mailReady() {
			s.send(ctx, out.Email, approvedMail(s.opts.PublicBaseURL, org.Name))
		}
	}
	return view(out), nil
}

// Reject is POST /orgs/{org}/signups/{id}/reject: an optional reason (kept
// for admins; it is never sent to the person) and an optional block, which
// stops new requests from the email in this org. A rejected request can be
// rejected again to add the block. Approved and expired requests are 409.
func (s *Service) Reject(ctx context.Context, org *auth.Org, p *auth.Principal, id uuid.UUID, in RejectInput) (Request, error) {
	reason, err := validText("reason", in.Reason, MaxReasonLen)
	if err != nil {
		return Request{}, err
	}
	q := dbq.New(s.pool)
	if _, err := q.ExpireSignupRequests(ctx); err != nil {
		return Request{}, fmt.Errorf("expire sign-up requests: %w", err)
	}
	var out dbq.SignupRequest
	changed := false
	err = database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		changed = false
		q := dbq.New(tx)
		row, err := q.GetSignupRequestForUpdate(ctx, dbq.GetSignupRequestForUpdateParams{OrgID: org.ID, ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			return errNoRequest
		}
		if err != nil {
			return fmt.Errorf("lock sign-up request: %w", err)
		}
		if row.Status != StatusPending && row.Status != StatusRejected {
			return httpx.Errorf(http.StatusConflict, httpx.CodeConflict, "the sign-up request is %s; only a pending request can be rejected", row.Status)
		}
		uid := p.UserID
		if out, err = q.RejectSignupRequest(ctx, dbq.RejectSignupRequestParams{ID: row.ID, DecidedBy: &uid, Reason: reason, Block: in.Block}); err != nil {
			return fmt.Errorf("reject sign-up request: %w", err)
		}
		changed = row.Status != out.Status || row.Blocked != out.Blocked
		return nil
	})
	if err != nil {
		return Request{}, err
	}
	if changed {
		s.log.InfoContext(ctx, "audit: sign-up rejected", "org", org.Slug, "actor", p.Username,
			"request", out.ID, "blocked", out.Blocked)
	}
	return view(out), nil
}

// PurgeUnconfirmed is DELETE /orgs/{org}/signups/unconfirmed: it deletes
// every request that waits for its confirm code (unverified, with a code).
// It is the admin's way out when a flood holds the unconfirmed cap; the
// 24-hour expiry frees it by itself too (decisions-T12 #10).
func (s *Service) PurgeUnconfirmed(ctx context.Context, org *auth.Org, p *auth.Principal) (PurgeResult, error) {
	n, err := dbq.New(s.pool).PurgeUnconfirmedSignups(ctx, org.ID)
	if err != nil {
		return PurgeResult{}, fmt.Errorf("purge unconfirmed sign-up requests: %w", err)
	}
	s.log.InfoContext(ctx, "audit: unconfirmed sign-up requests purged", "org", org.Slug, "actor", p.Username, "deleted", n)
	return PurgeResult{Deleted: n}, nil
}
