// Package platform is the install-wide settings a platform admin edits in
// the app (docs/internal/decisions-T9-mail-settings.md): how login email
// is sent (the environment, none, Postmark or SMTP) and the sign-up mode.
//
// The settings live in the one platform_settings row. Secrets are sealed
// with AES-256-GCM under a key derived from SETTINGS_ENCRYPTION_KEY, are
// write-only on the API, and never reach a log, an event, an idempotency
// row or an error message.
//
// The login service sends through a login.MailSwitch. This package sets
// the switch's transport: at boot, after every PUT (at once, on this
// server), and from a refresh every RefreshInterval (so the other replicas
// follow a change within that time).
package platform

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/auth"
	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/httpx"
	"devops/internal/login"
)

// RefreshInterval is how often every server re-reads the settings.
const RefreshInterval = 60 * time.Second

// Column names: the additional data of each sealed blob.
const (
	colPostmarkToken = "postmark_token_enc"
	colSMTPPassword  = "smtp_password_enc"
)

// EnvMail is what provider "env" means on this server: the transport the
// environment configures (POSTMARK_* or the dev log), as before per-install settings existed.
type EnvMail struct {
	Mailer    login.TextMailer // nil: the environment has no transport
	Transport string           // TransportPostmark, TransportDevLog or TransportNone
}

// Options configure the Service. Zero values take the defaults.
type Options struct {
	// Key is SETTINGS_ENCRYPTION_KEY (32 bytes). Nil: no secret can be
	// stored, and stored ones are unreadable.
	Key []byte
	Env EnvMail
	// Switch is the login service's mailer. Required.
	Switch *login.MailSwitch

	// The mail test route's limit: default 6 a minute, burst 3 (per
	// process; only platform admins reach it).
	TestPerMinute, TestBurst int
	// TestTimeout bounds one test send; default 30 seconds.
	TestTimeout time.Duration

	// Test seams for the mailers built from the settings.
	PostmarkEndpoint string
	SMTPRootCAs      *x509.CertPool
	SMTPDial         func(ctx context.Context, network, addr string) (net.Conn, error)
	SMTPTimeout      time.Duration
}

// Service reads, writes and applies the platform settings. It is safe for
// concurrent use.
type Service struct {
	pool *pgxpool.Pool
	log  *slog.Logger
	keys *Keyring // nil: no SETTINGS_ENCRYPTION_KEY
	opts Options

	mu      sync.Mutex // serialises apply; never held across network I/O
	applied *time.Time // updated_at of the row the switch follows

	active     atomic.Pointer[ActiveMail]
	unreadable atomic.Bool
	// loginCheck reports whether login can issue codes now (the login
	// service's Enabled: a transport AND the pepper). Nil: the switch alone.
	loginCheck atomic.Pointer[func() bool]
	testLimit  *login.RouteLimit
}

// New returns a Service. It does not read the database: call Refresh (or
// UseEnvironment when that fails) before serving.
func New(pool *pgxpool.Pool, log *slog.Logger, opts Options) (*Service, error) {
	if opts.Switch == nil {
		return nil, errors.New("platform: Options.Switch is required")
	}
	if opts.Env.Transport == "" {
		opts.Env.Transport = TransportNone
	}
	if opts.TestPerMinute <= 0 {
		opts.TestPerMinute, opts.TestBurst = 6, 3
	}
	if opts.TestBurst <= 0 {
		opts.TestBurst = 1
	}
	if opts.TestTimeout <= 0 {
		opts.TestTimeout = 30 * time.Second
	}
	s := &Service{pool: pool, log: log, opts: opts,
		testLimit: login.NewRouteLimit(opts.TestPerMinute, opts.TestBurst, nil)}
	if opts.Key != nil {
		k, err := NewKeyring(opts.Key)
		if err != nil {
			return nil, fmt.Errorf("platform: %w", err)
		}
		s.keys = k
	}
	s.active.Store(&ActiveMail{Transport: TransportNone, Source: SourceEnvironment})
	return s, nil
}

// opened is a row with its secrets opened.
type opened struct {
	row             dbq.PlatformSetting
	token, password Secret
	// unreadable: the row holds secrets this server cannot open (no key,
	// another key, or a blob that fails its check). Its secrets are then
	// treated as absent.
	unreadable bool
	why        string // the reason, for the log; never a secret
}

func (o opened) tokenSet() bool    { return o.token != "" }
func (o opened) passwordSet() bool { return o.password != "" }

func (s *Service) open(row dbq.PlatformSetting) opened {
	o := opened{row: row}
	if row.PostmarkTokenEnc == nil && row.SmtpPasswordEnc == nil {
		return o
	}
	switch {
	case s.keys == nil:
		o.unreadable, o.why = true, "SETTINGS_ENCRYPTION_KEY is not set on this server"
		return o
	case !s.keys.Matches(row.KeyCheck):
		o.unreadable, o.why = true, "SETTINGS_ENCRYPTION_KEY is not the key the secrets were stored with (key_check mismatch)"
		return o
	}
	for _, c := range []struct {
		col  string
		blob []byte
		out  *Secret
	}{{colPostmarkToken, row.PostmarkTokenEnc, &o.token}, {colSMTPPassword, row.SmtpPasswordEnc, &o.password}} {
		if c.blob == nil {
			continue
		}
		pt, err := s.keys.Open(c.col, c.blob)
		if err != nil {
			o.unreadable, o.why = true, "a stored secret failed its integrity check ("+c.col+")"
			o.token, o.password = "", ""
			return o
		}
		*c.out = Secret(pt)
	}
	return o
}

// needsSecret reports whether the row's provider needs a stored secret:
// postmark its token, smtp with a username its password.
func needsSecret(r dbq.PlatformSetting) bool {
	switch r.MailProvider {
	case ProviderPostmark:
		return true
	case ProviderSMTP:
		return r.SmtpUsername != ""
	default:
		return false
	}
}

// fallsBack: the provider needs a secret this server cannot open, so mail
// goes through the environment instead.
func fallsBack(o opened) bool { return o.unreadable && needsSecret(o.row) }

// mailerFor builds the transport of o. A provider that needs a secret this
// server cannot open falls back to the environment, so a lost key never
// stops login that the environment can still serve. A provider that needs
// no secret (none, env, smtp without a username) is used as it is.
func (s *Service) mailerFor(o opened) (login.TextMailer, ActiveMail) {
	r := o.row
	provider := r.MailProvider
	if fallsBack(o) {
		provider = ProviderEnv
	}
	switch provider {
	case ProviderNone:
		return nil, ActiveMail{Transport: TransportNone, Source: SourceSettings}
	case ProviderPostmark:
		if !o.tokenSet() || r.PostmarkFrom == "" {
			return nil, ActiveMail{Transport: TransportNone, Source: SourceSettings}
		}
		m := login.NewPostmarkMailer(o.token.Reveal(), r.PostmarkFrom, r.PostmarkStream)
		if s.opts.PostmarkEndpoint != "" {
			m.Endpoint = s.opts.PostmarkEndpoint
		}
		return m, ActiveMail{Transport: TransportPostmark, Source: SourceSettings}
	case ProviderSMTP:
		if r.SmtpHost == "" || r.SmtpFromAddress == "" || (r.SmtpUsername != "" && !o.passwordSet()) {
			return nil, ActiveMail{Transport: TransportNone, Source: SourceSettings}
		}
		return &login.SMTPMailer{
			Host: r.SmtpHost, Port: int(r.SmtpPort), Security: r.SmtpSecurity,
			Username: r.SmtpUsername, Password: o.password.Reveal(),
			FromAddress: r.SmtpFromAddress, FromName: r.SmtpFromName,
			Timeout: s.opts.SMTPTimeout, RootCAs: s.opts.SMTPRootCAs, Dial: s.opts.SMTPDial,
		}, ActiveMail{Transport: TransportSMTP, Source: SourceSettings}
	default:
		return s.opts.Env.Mailer, ActiveMail{Transport: s.opts.Env.Transport, Source: SourceEnvironment}
	}
}

// apply points the switch at row's transport, unless the switch already
// follows this row or a newer one (a refresh that read the row before a
// PUT committed must not undo the PUT).
func (s *Service) apply(ctx context.Context, row dbq.PlatformSetting) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.applied != nil && !row.UpdatedAt.After(*s.applied) {
		return
	}
	o := s.open(row)
	m, active := s.mailerFor(o)
	s.opts.Switch.Set(m)
	s.active.Store(&active)
	s.unreadable.Store(o.unreadable)
	at := row.UpdatedAt
	s.applied = &at
	switch {
	case fallsBack(o):
		s.log.ErrorContext(ctx, "mail settings: the stored mail secrets cannot be opened, so login email falls back to the environment's mail settings. "+
			"A platform admin must enter the secrets again (PUT /api/v1/platform/settings; docs/deploy.md 'Mail settings')",
			"reason", o.why, "mail_provider", row.MailProvider)
	case o.unreadable:
		s.log.ErrorContext(ctx, "mail settings: the stored mail secrets cannot be opened. The mail provider needs none of them, so it is used as it is; "+
			"enter them again before a provider needs them (PUT /api/v1/platform/settings; docs/deploy.md 'Mail settings')",
			"reason", o.why, "mail_provider", row.MailProvider)
	}
	switch {
	case m != nil:
		s.log.InfoContext(ctx, "mail: login email goes through "+active.Transport, "source", active.Source, "mail_provider", row.MailProvider)
	case active.Source == SourceSettings:
		s.log.WarnContext(ctx, "login: OFF. The platform mail settings send no email (provider "+row.MailProvider+
			"), so POST /auth/otp and /auth/otp/verify answer 503 login_unavailable")
	default:
		s.log.ErrorContext(ctx, "login: OFF. LOG_ENV is not local or test and POSTMARK_SERVER_TOKEN is empty, and the platform mail settings say env, "+
			"so no code can be delivered; POST /auth/otp and /auth/otp/verify answer 503 login_unavailable")
	}
}

// Refresh re-reads the row and applies it if it changed. On an error the
// switch keeps its transport.
func (s *Service) Refresh(ctx context.Context) error {
	row, err := dbq.New(s.pool).GetPlatformSettings(ctx)
	if err != nil {
		return fmt.Errorf("read platform settings: %w", err)
	}
	s.apply(ctx, row)
	return nil
}

// UseEnvironment points the switch at the environment's transport. main
// calls it when the boot Refresh fails, so login works as it did before the settings existed.
func (s *Service) UseEnvironment() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts.Switch.Set(s.opts.Env.Mailer)
	s.active.Store(&ActiveMail{Transport: s.opts.Env.Transport, Source: SourceEnvironment})
}

// Run refreshes every interval until ctx ends. It is the one goroutine of
// this package; main owns it (golang-concurrency: an owner and an exit).
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Refresh(ctx); err != nil && ctx.Err() == nil {
				s.log.WarnContext(ctx, "mail settings: refresh failed; keeping the current transport", "err", err)
			}
		}
	}
}

// Active is this server's transport now.
func (s *Service) Active() ActiveMail { return *s.active.Load() }

// SetLoginCheck makes LoginAvailable ask f (main passes the login
// service's Enabled, which also needs AUTH_OTP_PEPPER). Call it before
// serving.
func (s *Service) SetLoginCheck(f func() bool) { s.loginCheck.Store(&f) }

// MailWorking reports whether mail really goes out now: login can issue codes
// and the active transport sends (Postmark or SMTP). The dev log only writes
// codes to the log, so it is not working mail (LAUN-56).
func (s *Service) MailWorking() bool {
	switch s.Active().Transport {
	case TransportPostmark, TransportSMTP:
		return s.LoginAvailable()
	}
	return false
}

// LoginAvailable reports whether login can issue codes on this server now.
func (s *Service) LoginAvailable() bool {
	if f := s.loginCheck.Load(); f != nil && *f != nil {
		return (*f)()
	}
	return s.opts.Switch.Ready()
}

func (s *Service) view(o opened) Settings {
	r := o.row
	return Settings{
		MailProvider: r.MailProvider, PostmarkFrom: r.PostmarkFrom, PostmarkMessageStream: r.PostmarkStream,
		PostmarkTokenSet: o.tokenSet(),
		SMTPHost:         r.SmtpHost, SMTPPort: int(r.SmtpPort), SMTPSecurity: r.SmtpSecurity, SMTPUsername: r.SmtpUsername,
		SMTPPasswordSet: o.passwordSet(), SMTPFromAddress: r.SmtpFromAddress, SMTPFromName: r.SmtpFromName,
		SignupMode: r.SignupMode, SignupModeChosen: r.SignupModeChosenAt != nil, ActiveMail: s.Active(), LoginAvailable: s.LoginAvailable(), EncryptionKeySet: s.keys != nil,
		SecretsUnreadable: o.unreadable, UpdatedAt: r.UpdatedAt, UpdatedBy: r.UpdatedBy,
	}
}

// Get is GET /platform/settings.
func (s *Service) Get(ctx context.Context) (Settings, error) {
	row, err := dbq.New(s.pool).GetPlatformSettings(ctx)
	if err != nil {
		return Settings{}, fmt.Errorf("read platform settings: %w", err)
	}
	return s.view(s.open(row)), nil
}

var errNoKey = httpx.Errorf(http.StatusConflict, httpx.CodeConflict,
	"this server has no SETTINGS_ENCRYPTION_KEY, so it cannot store a secret; set it (docs/deploy.md 'Mail settings') and restart the server")

// nextSecret is the blob to store for one secret field. Absent or null
// keeps the stored blob, "" clears it, a value is sealed. A stored blob that
// this server cannot open is dropped even when "kept": it is useless, and
// the row's key_check moves to this server's key. dropped reports that.
func (s *Service) nextSecret(col string, in httpx.Opt[Secret], stored []byte, unreadable bool) (blob []byte, dropped bool, err error) {
	switch {
	case !in.Set || in.Null:
		if stored != nil && unreadable {
			return nil, true, nil
		}
		return stored, false, nil
	case in.V == "":
		return nil, false, nil
	case s.keys == nil:
		return nil, false, errNoKey
	default:
		return s.keys.Seal(col, []byte(in.V.Reveal())), false, nil
	}
}

func invalid(problems []string) error {
	return httpx.Invalid("%s", strings.Join(problems, "; "))
}

// Update is PUT /platform/settings: the whole settings, validated, in one
// transaction under the row lock. The switch follows at once on this
// server; other servers follow within RefreshInterval.
func (s *Service) Update(ctx context.Context, p *auth.Principal, in SettingsInput) (Settings, error) {
	f, problems := normalize(in)
	if len(problems) > 0 {
		return Settings{}, invalid(problems)
	}
	var out dbq.PlatformSetting
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := dbq.New(tx)
		row, err := q.GetPlatformSettingsForUpdate(ctx)
		if err != nil {
			return fmt.Errorf("lock platform settings: %w", err)
		}
		cur := s.open(row)
		tokenEnc, tokenDropped, err := s.nextSecret(colPostmarkToken, in.PostmarkToken, row.PostmarkTokenEnc, cur.unreadable)
		if err != nil {
			return err
		}
		pwEnc, pwDropped, err := s.nextSecret(colSMTPPassword, in.SMTPPassword, row.SmtpPasswordEnc, cur.unreadable)
		if err != nil {
			return err
		}
		var probs []string
		switch f.provider {
		case ProviderPostmark:
			if tokenEnc == nil {
				if tokenDropped {
					probs = append(probs, "postmarkToken: the stored token cannot be opened with this server's SETTINGS_ENCRYPTION_KEY; enter it again")
				} else {
					probs = append(probs, "postmarkToken is required for mail provider postmark")
				}
			}
			if f.pmFrom == "" {
				probs = append(probs, "postmarkFrom is required for mail provider postmark")
			}
		case ProviderSMTP:
			if f.host == "" {
				probs = append(probs, "smtpHost is required for mail provider smtp")
			}
			if f.from == "" {
				probs = append(probs, "smtpFromAddress is required for mail provider smtp")
			}
			if f.username != "" && pwEnc == nil {
				if pwDropped {
					probs = append(probs, "smtpPassword: the stored password cannot be opened with this server's SETTINGS_ENCRYPTION_KEY; enter it again")
				} else {
					probs = append(probs, "smtpPassword is required with smtpUsername")
				}
			}
			if f.username != "" && f.security == login.SMTPNone && !login.IsLoopbackHost(f.host) {
				probs = append(probs, "smtpSecurity none would send the password without TLS; use starttls or tls (none with a username is allowed only for localhost, 127.0.0.1 or ::1)")
			}
		}
		if len(probs) > 0 {
			return invalid(probs)
		}
		var keyCheck []byte
		if tokenEnc != nil || pwEnc != nil {
			keyCheck = s.keys.KeyCheck() // non-nil: a blob kept or sealed means a readable key
		}
		port := f.port // normalize maps 0 to the default 587
		uid := p.UserID
		out, err = q.UpdatePlatformSettings(ctx, dbq.UpdatePlatformSettingsParams{
			MailProvider: f.provider, PostmarkTokenEnc: tokenEnc, PostmarkFrom: f.pmFrom, PostmarkStream: f.pmStream,
			SmtpHost: f.host, SmtpPort: int32(port), SmtpSecurity: f.security, SmtpUsername: f.username,
			SmtpPasswordEnc: pwEnc, SmtpFromAddress: f.from, SmtpFromName: f.nme,
			KeyCheck: keyCheck, SignupMode: f.signup, UpdatedBy: &uid,
			// A change of the mode, or a confirm, is a choice (LAUN-56); no other save is.
			SignupChosen: row.SignupMode != f.signup || (in.SignupModeConfirmed != nil && *in.SignupModeConfirmed),
		})
		if err != nil {
			return fmt.Errorf("update platform settings: %w", err)
		}
		return nil
	})
	if err != nil {
		return Settings{}, err
	}
	s.apply(ctx, out)
	return s.view(s.open(out)), nil
}

// The test email.
const (
	testSubject = "Launchpad test email"
	testText    = "This is a test email from Launchpad.\n\n" +
		"A platform admin sent it to check the mail settings. Login codes go out the same way.\n" +
		"You can ignore this email.\n"
)

var (
	errNoEmail = httpx.Invalid("your user has no email address, so there is nowhere to send the test email")
	errNoMail  = httpx.Errorf(http.StatusServiceUnavailable, httpx.CodeLoginUnavailable,
		"no mail transport is configured (mail provider none, or env with no transport in the environment)")
)

// SendTest is POST /platform/mail/test: one email to the caller's own
// address through the transport login uses now. A failure is 502
// mail_send_failed with the mailer's safe text (login.PublicMailError).
func (s *Service) SendTest(ctx context.Context, p *auth.Principal) (MailTestResult, error) {
	if err := s.testLimit.Take(); err != nil {
		return MailTestResult{}, err
	}
	if p.Email == nil || *p.Email == "" {
		return MailTestResult{}, errNoEmail
	}
	// Pick up a change another server made in the last minute.
	if err := s.Refresh(ctx); err != nil {
		s.log.WarnContext(ctx, "mail settings: refresh before the mail test failed; testing the current transport", "err", err)
	}
	active := s.Active()
	if !s.opts.Switch.Ready() {
		return MailTestResult{}, errNoMail
	}
	sctx, cancel := context.WithTimeout(ctx, s.opts.TestTimeout)
	defer cancel()
	if err := s.opts.Switch.SendText(sctx, *p.Email, testSubject, testText); err != nil {
		// A PUT to none between the Ready check and the send: the same 503.
		if errors.Is(err, login.ErrNoMailer) {
			return MailTestResult{}, errNoMail
		}
		msg := login.PublicMailError(err)
		s.log.WarnContext(ctx, "mail test failed", "transport", active.Transport, "source", active.Source, "reason", msg)
		return MailTestResult{}, httpx.Errorf(http.StatusBadGateway, httpx.CodeMailSendFailed, "the test email was not sent: %s", msg)
	}
	return MailTestResult{OK: true, To: *p.Email, ActiveMail: active}, nil
}
