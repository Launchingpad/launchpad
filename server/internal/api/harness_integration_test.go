package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxuuid "github.com/vgarvardt/pgx-google-uuid/v5"

	"devops/internal/auth"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/idempotency"
	"devops/internal/login"
	"devops/internal/setup"
	"devops/internal/signup"
	"devops/internal/storage"
	"devops/internal/testdb"
	"devops/internal/tracker"
)

// env is one database with the test fixture (or empty) plus a live server on it.
type env struct {
	t      *testing.T
	pool   *pgxpool.Pool
	q      *dbq.Queries
	srv    *httptest.Server
	broker *events.Broker
	orgID  uuid.UUID
	// Fixture tokens (pinned to orbit); "" with envOpts.empty.
	chen, claude string

	// Web login (T6a).
	login    *login.Service
	mail     *mailbox    // codes handed to the transport (unless deliver is overridden)
	logs     *syncBuffer // every server log line
	sessions sync.Map    // session secrets: request() sends these as cookies
	names    auth.CookieNames

	// Sign-up requests and invites.
	signup *signup.Service
	smail  *textMailbox // every sign-up / invite email (sent in-request)
}

// envOpts changes how newEnvWith builds the server.
type envOpts struct {
	ssePoll    time.Duration                             // default 250ms
	idemStore  func(idempotency.Store) idempotency.Store // wrap the store (fault injection)
	noListener bool                                      // do not start LISTEN (mutation checks)
	idemOpts   idempotency.Options

	login    login.Options              // zero rate limits become "unlimited"
	deliver  func(e *env) login.Deliver // default: e.mail; return nil for "login off"
	logLevel slog.Level                 // default Warn; Debug captures everything
	local    bool                       // a LOG_ENV=local server: no Secure, bare cookie names
	now      func() time.Time           // the service clock (default time.Now)

	empty      bool          // no fixture: a first-run install
	setup      setup.Options // the setup service (zero limits become "unlimited")
	store      storage.Store // the attachment store (nil: attachments off)
	publicBase string        // PUBLIC_BASE_URL of the service

	configure func(e *env, s *Server) // last change to the server before Routes

	signup signup.Options // the sign-up service (zero limits become "unlimited")
	// trustedProxies is the server's TRUSTED_PROXY_CIDRS.
	trustedProxies []netip.Prefix
	// tracer sees every statement of the server's pool (T20
	// TestHiddenAndMissingSamePath counts them).
	tracer pgx.QueryTracer
}

// testPepper is the AUTH_OTP_PEPPER of every test server.
var testPepper = []byte("test-otp-pepper-0123456789abcdef-0123456789")

func newEnv(t *testing.T) *env { return newEnvWith(t, envOpts{}) }

func newEnvWith(t *testing.T, o envOpts) *env {
	t.Helper()
	pool, dsn := testdb.New(t)
	ctx := context.Background()
	if o.tracer != nil {
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		cfg.ConnConfig.Tracer = o.tracer
		cfg.AfterConnect = func(_ context.Context, conn *pgx.Conn) error {
			pgxuuid.Register(conn.TypeMap())
			return nil
		}
		if pool, err = pgxpool.NewWithConfig(ctx, cfg); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
	}
	e := &env{t: t, pool: pool, q: dbq.New(pool)}
	if !o.empty {
		fx := testdb.Fixture(t, pool)
		e.chen, e.claude, e.orgID = fx.Chen, fx.Claude, fx.OrgID
	}

	e.logs = &syncBuffer{}
	level := slog.LevelWarn
	if o.logLevel != 0 {
		level = o.logLevel
	}
	log := slog.New(slog.NewTextHandler(io.MultiWriter(tWriter{t}, e.logs), &slog.HandlerOptions{Level: level}))
	e.broker = events.NewBroker()
	lctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	listen := !o.noListener
	wg.Go(func() {
		if !listen {
			return
		}
		events.Listen(lctx, func(c context.Context) (*pgx.Conn, error) {
			conn, err := pgx.Connect(c, dsn)
			if err != nil {
				return nil, err
			}
			pgxuuid.Register(conn.TypeMap())
			return conn, nil
		}, e.broker, log)
	})
	var store idempotency.Store = e.q
	if o.idemStore != nil {
		store = o.idemStore(store)
	}
	svc := tracker.New(pool)
	if o.now != nil {
		svc.SetClock(o.now)
	}
	if o.store != nil {
		svc.SetStore(o.store)
	}
	svc.SetPublicBaseURL(o.publicBase)
	s := NewServer(svc, auth.NewAuthenticator(e.q, log), e.broker,
		idempotency.Middleware(store, log, o.idemOpts), pool, log)
	s.SSEPoll = 250 * time.Millisecond
	if o.ssePoll > 0 {
		s.SSEPoll = o.ssePoll
	}
	s.SSEHeartbeat = time.Second
	e.mail = &mailbox{}
	deliver := e.mail.deliver
	if o.deliver != nil {
		deliver = o.deliver(e)
	}
	lo := o.login
	if lo.RequestsPerMinute == 0 {
		lo.RequestsPerMinute, lo.RequestBurst = 1_000_000, 1_000_000
	}
	if lo.VerifiesPerMinute == 0 {
		lo.VerifiesPerMinute, lo.VerifyBurst = 1_000_000, 1_000_000
	}
	if lo.Pepper == nil {
		lo.Pepper = testPepper
	}
	e.login = login.New(pool, log, deliver, lo)
	s.Login = e.login
	so := o.setup
	if so.PerMinute == 0 {
		so.PerMinute, so.Burst = 1_000_000, 1_000_000
	}
	s.Setup = setup.New(pool, so)
	e.smail = &textMailbox{}
	e.smail.ready.Store(true)
	sg := o.signup
	unlimited := func(v *int) {
		if *v == 0 {
			*v = 1_000_000
		}
	}
	for _, v := range []*int{&sg.SignupPerMinute, &sg.SignupBurst, &sg.ConfirmPerMinute, &sg.ConfirmBurst,
		&sg.AcceptPerMinute, &sg.AcceptBurst, &sg.SignupPerClientPerHour, &sg.ConfirmPerClientPerHour,
		&sg.AcceptPerClientPerHour, &sg.SignupPerEmailPerHour, &sg.ConfirmPerEmailPerHour} {
		unlimited(v)
	}
	if sg.Pepper == nil {
		sg.Pepper = testPepper
	}
	if sg.Mailer == nil {
		sg.Mailer = e.smail
	}
	if sg.PublicBaseURL == "" {
		sg.PublicBaseURL = o.publicBase
	}
	sg.SyncMail = true
	e.signup = signup.New(pool, log, sg)
	s.Signup = e.signup
	s.TrustedProxies = o.trustedProxies
	s.SecureCookies = !o.local
	e.names = auth.CookieNamesFor(s.SecureCookies)
	if o.configure != nil {
		o.configure(e, s)
	}
	e.srv = httptest.NewServer(s.Routes())
	t.Cleanup(func() {
		e.broker.Close()
		e.srv.Close()
		cancel()
		wg.Wait()
	})
	return e
}

// tWriter sends server logs to t.Log, which prints only for a failing test.
type tWriter struct{ t *testing.T }

func (w tWriter) Write(b []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(b), "\n"))
	return len(b), nil
}

// resp is a decoded response.
type resp struct {
	status int
	body   []byte
	header http.Header
	// linked and moved are set by the webhook senders (hookDiff): the issues
	// a delivery newly linked and moved, read from the database, since the
	// answer is a status only (decisions-T20 #65).
	linked, moved []string
}

func (r resp) json(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
}

func (r resp) errCode(t *testing.T) string {
	t.Helper()
	var b struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	r.json(t, &b)
	if b.Error == "" {
		t.Fatalf("error envelope has no message: %s", r.body)
	}
	return b.Code
}

func (e *env) do(method, path, token string, body any, headers ...string) resp {
	e.t.Helper()
	r, err := e.request(method, path, token, body, headers...)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// request is do without t.Fatal, so goroutines can use it.
func (e *env) request(method, path, token string, body any, headers ...string) (resp, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return resp{}, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		return resp{}, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	e.authorize(req, token)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return resp{}, err
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		return resp{}, err
	}
	return resp{status: res.StatusCode, body: b, header: res.Header}, nil
}

var jsonUnmarshal = json.Unmarshal

// must asserts the status and returns the response.
func (e *env) must(status int, r resp) resp {
	e.t.Helper()
	if r.status != status {
		e.t.Fatalf("status = %d, want %d; body %s", r.status, status, r.body)
	}
	return r
}

// newOrg makes an org with one team (key) and returns its id.
func (e *env) newOrg(slug, teamKey string) uuid.UUID {
	e.t.Helper()
	ctx := context.Background()
	org, err := e.q.InsertOrg(ctx, dbq.InsertOrgParams{Slug: slug, Name: slug})
	if err != nil {
		e.t.Fatal(err)
	}
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := tracker.CreateTeamTx(ctx, e.q.WithTx(tx), org.ID, teamKey, teamKey, ""); err != nil {
		e.t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		e.t.Fatal(err)
	}
	return org.ID
}

func (e *env) user(username string) dbq.User {
	e.t.Helper()
	u, err := e.q.GetUserByUsername(context.Background(), username)
	if err != nil {
		e.t.Fatal(err)
	}
	return u
}

func (e *env) newUser(username string) dbq.User {
	e.t.Helper()
	u, err := e.q.InsertUser(context.Background(), dbq.InsertUserParams{Username: username, Name: username})
	if err != nil {
		e.t.Fatal(err)
	}
	return u
}

func (e *env) addMember(orgID, userID uuid.UUID, role string) {
	e.t.Helper()
	if err := e.q.InsertOrgMember(context.Background(), dbq.InsertOrgMemberParams{OrgID: orgID, UserID: userID, Role: role}); err != nil {
		e.t.Fatal(err)
	}
}

type tokenOpts struct {
	pin     *uuid.UUID
	scopes  []string
	expires *time.Time
	revoked bool
}

// newToken inserts a token directly and returns its secret.
func (e *env) newToken(userID uuid.UUID, o tokenOpts) (string, uuid.UUID) {
	e.t.Helper()
	nt, err := auth.GenerateToken()
	if err != nil {
		e.t.Fatal(err)
	}
	if o.scopes == nil {
		o.scopes = []string{"read", "write", "admin"}
	}
	row, err := e.q.InsertAPIToken(context.Background(), dbq.InsertAPITokenParams{
		UserID: userID, OrgID: o.pin, Kind: auth.KindPAT, Name: "test", TokenHash: nt.Hash,
		Prefix: nt.Prefix, Scopes: o.scopes, ExpiresAt: o.expires,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	if o.revoked {
		if _, err := e.q.RevokeOrgToken(context.Background(), dbq.RevokeOrgTokenParams{OrgID: o.pin, ID: row.ID}); err != nil {
			e.t.Fatal(err)
		}
	}
	return nt.Plaintext, row.ID
}

// createIssue posts an issue in orbit and returns it.
func (e *env) createIssue(token string, body map[string]any) tracker.Issue {
	e.t.Helper()
	if _, ok := body["team"]; !ok {
		body["team"] = "ORB"
	}
	var is tracker.Issue
	e.must(http.StatusCreated, e.do("POST", "/api/v1/orgs/orbit/issues", token, body)).json(e.t, &is)
	return is
}

// authorize sends token as a bearer header, or, for a session secret from
// e.login (or e.asSession), as the browser would: both cookies plus the
// CSRF header. Explicit headers passed to do() are applied after, so a test
// can blank or change X-CSRF-Token.
func (e *env) authorize(req *http.Request, token string) {
	if token == "" {
		return
	}
	if _, ok := e.sessions.Load(token); ok {
		csrf := auth.CSRFToken(token)
		req.AddCookie(&http.Cookie{Name: e.names.Session, Value: token})
		req.AddCookie(&http.Cookie{Name: e.names.CSRF, Value: csrf})
		req.Header.Set(auth.CSRFHeader, csrf)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
}

// asSession marks a secret as a session, so do() sends it as cookies.
func (e *env) asSession(secret string) string {
	e.sessions.Store(secret, true)
	return secret
}

// mailbox records every code handed to the transport.
type mailbox struct {
	mu   sync.Mutex
	sent []sentCode
}

type sentCode struct{ to, code string }

func (m *mailbox) deliver(_ context.Context, to, code string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, sentCode{to, code})
}

// last is the newest code sent to "to" ("" if none). The code goes to the
// STORED address, so the match ignores case and spaces in what was typed.
func (m *mailbox) last(to string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.sent) - 1; i >= 0; i-- {
		if strings.EqualFold(m.sent[i].to, strings.TrimSpace(to)) {
			return m.sent[i].code
		}
	}
	return ""
}

func (m *mailbox) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

// textMailbox is the sign-up mailer of the test server: Ready follows
// ready, and every message is recorded.
type textMailbox struct {
	ready atomic.Bool
	mu    sync.Mutex
	sent  []sentText
}

type sentText struct{ to, subject, text string }

func (m *textMailbox) Ready() bool { return m.ready.Load() }

func (m *textMailbox) SendText(_ context.Context, to, subject, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, sentText{to, subject, text})
	return nil
}

func (m *textMailbox) all() []sentText {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]sentText(nil), m.sent...)
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// cookies parses the Set-Cookie headers of a response.
func (r resp) cookies() map[string]*http.Cookie {
	out := map[string]*http.Cookie{}
	for _, c := range (&http.Response{Header: r.header}).Cookies() {
		out[c.Name] = c
	}
	return out
}

// requestCode asks for a code for email and returns the one sent ("" when
// none was sent).
func (e *env) requestCode(email string) string {
	e.t.Helper()
	before := e.mail.last(email)
	r := e.must(http.StatusAccepted, e.do("POST", "/api/v1/auth/otp", "", map[string]any{"email": email}))
	if strings.TrimSpace(string(r.body)) != "{}" {
		e.t.Fatalf("otp body = %q, want {}", r.body)
	}
	if c := e.mail.last(email); c != before {
		return c
	}
	return ""
}

// loginAs runs the whole login and returns the session secret (registered,
// so do() sends it as cookies) and the response.
func (e *env) loginAs(email string) (string, resp) {
	e.t.Helper()
	code := e.requestCode(email)
	if code == "" {
		e.t.Fatalf("no code sent to %s", email)
	}
	r := e.must(http.StatusOK, e.do("POST", "/api/v1/auth/otp/verify", "", map[string]any{"email": email, "code": code}))
	c, ok := r.cookies()[e.names.Session]
	if !ok || c.Value == "" {
		e.t.Fatalf("verify set no %s cookie: %v", e.names.Session, r.header)
	}
	return e.asSession(c.Value), r
}

// newHuman makes a human user with a login email, a member of orbit.
func (e *env) newHuman(username, email, role string) dbq.User {
	e.t.Helper()
	u, err := e.q.InsertUser(context.Background(), dbq.InsertUserParams{Username: username, Name: username, Email: &email})
	if err != nil {
		e.t.Fatal(err)
	}
	e.addMember(e.orgID, u.ID, role)
	return u
}
