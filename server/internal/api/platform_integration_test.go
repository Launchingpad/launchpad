package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"devops/internal/auth"
	"devops/internal/dbq"
	"devops/internal/login"
	"devops/internal/login/smtptest"
	"devops/internal/platform"
	"devops/internal/setup"
)

// the platform settings routes, against the real router and
// Postgres (docs/internal/decisions-T9-mail-settings.md).

const (
	platToken    = "planted-pm-token-a1b2c3"
	platPassword = "planted-smtp-password-d4e5f6"
	platUser     = "relay-user"
)

var (
	platKey1 = bytes.Repeat([]byte{0x11}, 32)
	platKey2 = bytes.Repeat([]byte{0x22}, 32)
)

// recordingMailer is the "environment" transport of the test server.
type recordingMailer struct {
	mu    sync.Mutex
	codes []string
	texts []string
	fail  error // SendText returns it when set
}

func (m *recordingMailer) SendLoginCode(_ context.Context, to, code string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.codes = append(m.codes, to+":"+code)
	return nil
}

func (m *recordingMailer) SendText(_ context.Context, to, subject, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.texts = append(m.texts, to+":"+subject)
	return nil
}

func (m *recordingMailer) textCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.texts)
}

type platEnv struct {
	*env
	plat    *platform.Service
	sw      *login.MailSwitch
	envMail *recordingMailer
	smtp    *smtptest.Server
	chen    dbq.User
	// admin is an unpinned admin PAT of chen, a platform admin; session
	// is chen's web session.
	admin, session string
}

type platOpts struct {
	key       []byte
	smtp      smtptest.Options
	testLimit int // mail test per minute (and burst); default unlimited
}

// newPlatEnv: the fixture, chen made a platform admin, and a server whose
// login sends through a MailSwitch that the platform service drives (as in
// cmd/server). Login also records each code in e.mail, so loginAs works.
func newPlatEnv(t *testing.T, o platOpts) *platEnv {
	t.Helper()
	pe := &platEnv{envMail: &recordingMailer{}, sw: login.NewMailSwitch()}
	o.smtp.Username, o.smtp.Password = platUser, platPassword
	pe.smtp = smtptest.Start(t, o.smtp)
	if o.testLimit == 0 {
		o.testLimit = 1_000_000
	}
	pe.env = newEnvWith(t, envOpts{logLevel: slog.LevelDebug, configure: func(e *env, s *Server) {
		log := slog.New(slog.NewTextHandler(io.MultiWriter(tWriter{t}, e.logs), &slog.HandlerOptions{Level: slog.LevelDebug}))
		p, err := platform.New(e.pool, log, platform.Options{
			Key: o.key, Switch: pe.sw,
			Env:           platform.EnvMail{Mailer: pe.envMail, Transport: platform.TransportDevLog},
			TestPerMinute: o.testLimit, TestBurst: o.testLimit,
			SMTPRootCAs: pe.smtp.RootCAs, SMTPDial: pe.smtp.Dial, SMTPTimeout: 5 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		pe.plat = p
		s.Platform = p
		deliver := func(ctx context.Context, to, code string) {
			e.mail.deliver(ctx, to, code)
			if err := pe.sw.SendLoginCode(ctx, to, code); err != nil {
				log.ErrorContext(ctx, "login code email failed", "err", err)
			}
		}
		s.Login = login.New(e.pool, log, deliver, login.Options{
			Pepper: testPepper, Available: pe.sw.Ready,
			RequestsPerMinute: 1_000_000, RequestBurst: 1_000_000, VerifiesPerMinute: 1_000_000, VerifyBurst: 1_000_000,
		})
		e.login = s.Login
		p.SetLoginCheck(s.Login.Enabled)
	}})
	ctx := context.Background()
	if _, err := setup.GrantPlatformAdmin(ctx, pe.pool, "chen"); err != nil {
		t.Fatal(err)
	}
	pe.chen = pe.user("chen")
	pe.admin, _ = pe.newToken(pe.chen.ID, tokenOpts{})
	pe.session, _ = pe.loginAs(chenEmail)
	return pe
}

// body is a complete PUT body (env provider), with changes.
func platBody(kv ...any) map[string]any {
	b := map[string]any{
		"mailProvider": "env", "signupMode": "invite",
		"postmarkFrom": "", "postmarkMessageStream": "",
		"smtpHost": "", "smtpPort": 587, "smtpSecurity": "starttls",
		"smtpUsername": "", "smtpFromAddress": "", "smtpFromName": "",
	}
	for i := 0; i+1 < len(kv); i += 2 {
		k := kv[i].(string)
		if kv[i+1] == nil {
			b[k] = nil
			continue
		}
		b[k] = kv[i+1]
	}
	return b
}

func smtpBody(kv ...any) map[string]any {
	b := platBody("mailProvider", "smtp", "smtpHost", "mail.test", "smtpSecurity", "starttls",
		"smtpUsername", platUser, "smtpFromAddress", "tracker@example.com", "smtpFromName", "Orbit")
	for i := 0; i+1 < len(kv); i += 2 {
		b[kv[i].(string)] = kv[i+1]
	}
	return b
}

func (pe *platEnv) settings(token string) platform.Settings {
	pe.t.Helper()
	var s platform.Settings
	pe.must(200, pe.do("GET", "/api/v1/platform/settings", token, nil)).json(pe.t, &s)
	return s
}

func (pe *platEnv) put(token string, body map[string]any) resp {
	pe.t.Helper()
	return pe.do("PUT", "/api/v1/platform/settings", token, body)
}

// noPlanted fails when any planted secret is in s.
func noPlanted(t *testing.T, where, s string) {
	t.Helper()
	for _, v := range []string{platToken, platPassword} {
		if strings.Contains(s, v) {
			t.Fatalf("%s holds a planted secret:\n%s", where, s)
		}
	}
}

// TestPlatformSettings_Auth: 401 without credentials; 403 for anything but
// a platform admin's session or unpinned admin token; on all three routes.
func TestPlatformSettings_Auth(t *testing.T) {
	pe := newPlatEnv(t, platOpts{key: platKey1})
	member := pe.newHuman("ann", "ann@example.com", "member")
	memberTok, _ := pe.newToken(member.ID, tokenOpts{pin: &pe.orgID, scopes: []string{"read", "write"}})
	orgAdmin := pe.newHuman("bob", "bob@example.com", "admin")
	orgAdminUnpinned, _ := pe.newToken(orgAdmin.ID, tokenOpts{})
	readWrite, _ := pe.newToken(pe.chen.ID, tokenOpts{scopes: []string{"read", "write"}})
	routes := []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/v1/platform/settings", nil},
		{"PUT", "/api/v1/platform/settings", platBody()},
		{"POST", "/api/v1/platform/mail/test", nil},
	}
	cases := []struct {
		name, token string
		status      int
		code        string
	}{
		{"no credentials", "", 401, "unauthenticated"},
		{"bad token", "odt_" + strings.Repeat("x", 43), 401, "invalid_token"},
		{"member", memberTok, 403, "forbidden"},
		{"org admin, unpinned admin PAT, not a platform admin", orgAdminUnpinned, 403, "forbidden"},
		{"platform admin, org-pinned fixture token", pe.env.chen, 403, "forbidden"},
		{"platform admin, PAT without admin scope", readWrite, 403, "forbidden"},
		{"agent", pe.claude, 403, "forbidden"},
	}
	for _, rt := range routes {
		for _, tt := range cases {
			t.Run(rt.method+" "+rt.path+" "+tt.name, func(t *testing.T) {
				r := pe.do(rt.method, rt.path, tt.token, rt.body)
				if r.status != tt.status || r.errCode(t) != tt.code {
					t.Fatalf("%d %s, want %d %s", r.status, r.body, tt.status, tt.code)
				}
			})
		}
		for _, ok := range []string{pe.admin, pe.session} {
			if r := pe.do(rt.method, rt.path, ok, rt.body); r.status != 200 {
				t.Fatalf("%s %s as a platform admin: %d %s", rt.method, rt.path, r.status, r.body)
			}
		}
	}
	// A cookie write still needs the CSRF header.
	r := pe.do("PUT", "/api/v1/platform/settings", pe.session, platBody(), auth.CSRFHeader, "wrong")
	if r.status != 403 || r.errCode(t) != "csrf_failed" {
		t.Fatalf("PUT with a bad CSRF header: %d %s", r.status, r.body)
	}
}

func TestPlatformSettings_GetDefaults(t *testing.T) {
	pe := newPlatEnv(t, platOpts{key: platKey1})
	s := pe.settings(pe.admin)
	want := platform.Settings{
		MailProvider: "env", SMTPPort: 587, SMTPSecurity: "starttls", SignupMode: "invite",
		ActiveMail:       platform.ActiveMail{Transport: "dev-log", Source: "environment"},
		LoginAvailable:   true,
		EncryptionKeySet: true, UpdatedAt: s.UpdatedAt,
	}
	if s != want {
		t.Fatalf("defaults\n got %+v\nwant %+v", s, want)
	}
}

// TestPlatformSettings_Validation: the PUT matrix through HTTP. Every bad
// body is a 4xx that names the field and stores nothing.
func TestPlatformSettings_Validation(t *testing.T) {
	pe := newPlatEnv(t, platOpts{key: platKey1})
	before := pe.settings(pe.admin)
	cases := []struct {
		name   string
		body   map[string]any
		status int
		code   string
		want   string
	}{
		{"missing field", func() map[string]any { b := platBody(); delete(b, "smtpHost"); return b }(), 400, "validation_failed", "smtpHost is required"},
		{"unknown field", platBody("smtpTls", true), 400, "bad_request", "smtpTls"},
		{"port as a string", platBody("smtpPort", "587"), 400, "bad_request", "invalid JSON"},
		{"bad provider", platBody("mailProvider", "sendgrid"), 400, "validation_failed", "mailProvider must be"},
		{"bad signup mode", platBody("signupMode", "open"), 400, "validation_failed", "signupMode must be"},
		{"postmark without a token", platBody("mailProvider", "postmark", "postmarkFrom", "tracker@example.com"), 400, "validation_failed", "postmarkToken is required"},
		{"postmark without from", platBody("mailProvider", "postmark", "postmarkToken", platToken), 400, "validation_failed", "postmarkFrom is required"},
		{"smtp without host", smtpBody("smtpHost", "", "smtpPassword", platPassword), 400, "validation_failed", "smtpHost is required"},
		{"smtp port out of range", smtpBody("smtpPort", 70000, "smtpPassword", platPassword), 400, "validation_failed", "smtpPort must be 0 or 1-65535 (0 stores the default 587)"},
		{"smtp username without password", smtpBody(), 400, "validation_failed", "smtpPassword is required with smtpUsername"},
		{"plain auth without tls to a remote host", smtpBody("smtpSecurity", "none", "smtpPassword", platPassword), 400, "validation_failed", "would send the password without TLS"},
		{"header injection in the from name", smtpBody("smtpFromName", "Orbit\r\nBcc: x@example.com", "smtpPassword", platPassword), 400, "validation_failed", "smtpFromName"},
		{"token with a space", platBody("postmarkToken", platToken+" x"), 400, "validation_failed", "postmarkToken holds a space"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r := pe.put(pe.admin, tt.body)
			if r.status != tt.status || r.errCode(t) != tt.code || !strings.Contains(string(r.body), tt.want) {
				t.Fatalf("%d %s, want %d %s %q", r.status, r.body, tt.status, tt.code, tt.want)
			}
			noPlanted(t, "the error", string(r.body))
		})
	}
	// Not JSON; an Idempotency-Key.
	req, _ := http.NewRequest("PUT", pe.srv.URL+"/api/v1/platform/settings", strings.NewReader("mailProvider=none"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	pe.authorize(req, pe.admin)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 415 {
		t.Fatalf("form body: %d", res.StatusCode)
	}
	r := pe.do("PUT", "/api/v1/platform/settings", pe.admin, platBody("postmarkToken", platToken), "Idempotency-Key", "k1")
	if r.status != 400 || !strings.Contains(string(r.body), "Idempotency-Key") {
		t.Fatalf("Idempotency-Key: %d %s", r.status, r.body)
	}
	r = pe.do("POST", "/api/v1/platform/mail/test", pe.admin, nil, "Idempotency-Key", "k2")
	if r.status != 400 {
		t.Fatalf("Idempotency-Key on the test: %d %s", r.status, r.body)
	}
	if after := pe.settings(pe.admin); after != before {
		t.Fatalf("a refused PUT changed the settings:\n%+v\n%+v", before, after)
	}
}

// TestPlatformSettings_SecretSemantics: absent or null keeps, "" clears, a
// value replaces; GET never shows a value; the row holds only sealed blobs.
func TestPlatformSettings_SecretSemantics(t *testing.T) {
	pe := newPlatEnv(t, platOpts{key: platKey1})
	ctx := context.Background()
	blobs := func() (tok, pw []byte) {
		row, err := pe.q.GetPlatformSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return row.PostmarkTokenEnc, row.SmtpPasswordEnc
	}

	r := pe.must(200, pe.put(pe.admin, platBody("mailProvider", "postmark", "postmarkFrom", "Launchpad <tracker@example.com>",
		"postmarkToken", platToken, "smtpUsername", platUser, "smtpPassword", platPassword)))
	noPlanted(t, "the PUT response", string(r.body))
	s := pe.settings(pe.admin)
	if !s.PostmarkTokenSet || !s.SMTPPasswordSet || s.MailProvider != "postmark" || s.UpdatedBy == nil || *s.UpdatedBy != pe.chen.ID ||
		s.ActiveMail != (platform.ActiveMail{Transport: "postmark", Source: "settings"}) {
		t.Fatalf("after set: %+v", s)
	}
	tok1, pw1 := blobs()
	if tok1 == nil || pw1 == nil || bytes.Contains(tok1, []byte(platToken)) || bytes.Contains(pw1, []byte(platPassword)) {
		t.Fatal("the blobs are missing or hold the plaintext")
	}
	if n := pe.countRows(`SELECT count(*) FROM platform_settings WHERE key_check IS NOT NULL`); n != 1 {
		t.Fatal("no key_check stored")
	}

	// Absent keeps.
	pe.must(200, pe.put(pe.admin, platBody("mailProvider", "postmark", "postmarkFrom", "tracker@example.com", "smtpUsername", platUser)))
	if tok, pw := blobs(); !bytes.Equal(tok, tok1) || !bytes.Equal(pw, pw1) {
		t.Fatal("an absent secret was not kept")
	}
	// Null keeps.
	pe.must(200, pe.put(pe.admin, platBody("mailProvider", "postmark", "postmarkFrom", "tracker@example.com", "postmarkToken", nil,
		"smtpUsername", platUser, "smtpPassword", nil)))
	if tok, pw := blobs(); !bytes.Equal(tok, tok1) || !bytes.Equal(pw, pw1) {
		t.Fatal("a null secret was not kept")
	}
	// A new value replaces.
	pe.must(200, pe.put(pe.admin, platBody("mailProvider", "postmark", "postmarkFrom", "tracker@example.com", "postmarkToken", platToken+"-2",
		"smtpUsername", platUser)))
	if tok, _ := blobs(); bytes.Equal(tok, tok1) {
		t.Fatal("a new token did not replace the blob")
	}
	// "" clears: refused while postmark needs it, done once it does not.
	r = pe.put(pe.admin, platBody("mailProvider", "postmark", "postmarkFrom", "tracker@example.com", "postmarkToken", ""))
	if r.status != 400 || !strings.Contains(string(r.body), "postmarkToken is required") {
		t.Fatalf("clearing the token postmark needs: %d %s", r.status, r.body)
	}
	pe.must(200, pe.put(pe.admin, platBody("postmarkToken", "", "smtpPassword", "")))
	if tok, pw := blobs(); tok != nil || pw != nil {
		t.Fatal(`"" did not clear`)
	}
	if n := pe.countRows(`SELECT count(*) FROM platform_settings WHERE key_check IS NULL`); n != 1 {
		t.Fatal("key_check kept with no secret stored")
	}
	if s := pe.settings(pe.admin); s.PostmarkTokenSet || s.SMTPPasswordSet || s.ActiveMail.Source != "environment" {
		t.Fatalf("after clear: %+v", s)
	}
}

// TestPlatformSettings_NoKey: with no SETTINGS_ENCRYPTION_KEY, a secret is
// refused (409) and the settings without one still save.
func TestPlatformSettings_NoKey(t *testing.T) {
	pe := newPlatEnv(t, platOpts{})
	r := pe.put(pe.admin, platBody("postmarkToken", platToken))
	if r.status != 409 || r.errCode(t) != "conflict" || !strings.Contains(string(r.body), "SETTINGS_ENCRYPTION_KEY") {
		t.Fatalf("%d %s", r.status, r.body)
	}
	noPlanted(t, "the error", string(r.body))
	pe.must(200, pe.put(pe.admin, platBody("signupMode", "closed")))
	if s := pe.settings(pe.admin); s.EncryptionKeySet || s.SignupMode != "closed" {
		t.Fatalf("%+v", s)
	}
}

// TestPlatformSettings_SwitchesLogin: the provider decides login. none is
// 503; env is today's behaviour; smtp sends the code through SMTP.
func TestPlatformSettings_SwitchesLogin(t *testing.T) {
	pe := newPlatEnv(t, platOpts{key: platKey1, smtp: smtptest.Options{StartTLS: true, Auth: true}})
	otp := func() resp {
		return pe.do("POST", "/api/v1/auth/otp", "", map[string]any{"email": "nobody@example.com"})
	}
	pe.must(202, otp())

	pe.must(200, pe.put(pe.admin, platBody("mailProvider", "none")))
	for _, p := range []string{"/api/v1/auth/otp", "/api/v1/auth/otp/verify"} {
		r := pe.do("POST", p, "", map[string]any{"email": chenEmail, "code": "123456"})
		if r.status != 503 || r.errCode(t) != "login_unavailable" {
			t.Fatalf("%s with provider none: %d %s", p, r.status, r.body)
		}
	}
	if s := pe.settings(pe.admin); s.ActiveMail != (platform.ActiveMail{Transport: "none", Source: "settings"}) || s.LoginAvailable {
		t.Fatalf("none: active %+v loginAvailable %v", s.ActiveMail, s.LoginAvailable)
	}

	pe.must(200, pe.put(pe.admin, smtpBody("smtpPassword", platPassword)))
	if s := pe.settings(pe.admin); !s.LoginAvailable || s.ActiveMail.Transport != "smtp" {
		t.Fatalf("smtp: %+v", s)
	}
	before := len(pe.smtp.Messages())
	code := pe.requestCode(chenEmail)
	if code == "" {
		t.Fatal("no code issued with provider smtp")
	}
	msgs := pe.smtp.Messages()
	if len(msgs) != before+1 || !strings.Contains(string(msgs[len(msgs)-1].Data), "Your login code is "+code) ||
		msgs[len(msgs)-1].AuthUser != platUser || !msgs[len(msgs)-1].TLS {
		t.Fatalf("the code did not go through SMTP: %d messages", len(msgs))
	}

	pe.must(200, pe.put(pe.admin, platBody("mailProvider", "env", "smtpUsername", platUser)))
	pe.must(202, otp())
	noPlanted(t, "the log", pe.logs.String())
}

// TestPlatformMailTest: the test email goes to the caller through the
// current transport; a failure is a sanitised 502.
func TestPlatformMailTest(t *testing.T) {
	const plantedServerText = "planted-server-reply-0099"
	pe := newPlatEnv(t, platOpts{key: platKey1, smtp: smtptest.Options{StartTLS: true, Auth: true,
		AuthFailText: "5.7.8 login for " + platUser + " with " + platPassword + " " + plantedServerText}})

	// env: the environment's transport.
	var res platform.MailTestResult
	pe.must(200, pe.do("POST", "/api/v1/platform/mail/test", pe.admin, nil)).json(t, &res)
	if !res.OK || res.To != chenEmail || res.ActiveMail.Source != "environment" || pe.envMail.textCount() != 1 {
		t.Fatalf("env test: %+v (%d texts)", res, pe.envMail.textCount())
	}

	// smtp, right password: sent.
	pe.must(200, pe.put(pe.admin, smtpBody("smtpPassword", platPassword)))
	pe.must(200, pe.do("POST", "/api/v1/platform/mail/test", pe.session, nil)).json(t, &res)
	msgs := pe.smtp.Messages()
	if !res.OK || res.ActiveMail.Transport != "smtp" || len(msgs) != 1 ||
		!strings.Contains(string(msgs[0].Data), "Subject: Launchpad test email") || msgs[0].To[0] != "TO:<"+chenEmail+">" {
		t.Fatalf("smtp test: %+v %d messages", res, len(msgs))
	}

	// smtp, wrong password: 502 with the step and the code only.
	pe.must(200, pe.put(pe.admin, smtpBody("smtpPassword", "wrong-"+platPassword)))
	r := pe.do("POST", "/api/v1/platform/mail/test", pe.admin, nil)
	if r.status != 502 || r.errCode(t) != "mail_send_failed" || !strings.Contains(string(r.body), "refused the login (reply code 535)") {
		t.Fatalf("auth failure: %d %s", r.status, r.body)
	}
	for _, v := range []string{plantedServerText, platPassword} {
		if strings.Contains(string(r.body), v) || strings.Contains(pe.logs.String(), v) {
			t.Fatalf("%q leaked into the response or the log", v)
		}
	}

	// none: 503.
	pe.must(200, pe.put(pe.admin, platBody("mailProvider", "none")))
	r = pe.do("POST", "/api/v1/platform/mail/test", pe.admin, nil)
	if r.status != 503 || r.errCode(t) != "login_unavailable" {
		t.Fatalf("none: %d %s", r.status, r.body)
	}

	// A platform admin with no email address: 400.
	ctx := context.Background()
	ops, err := pe.q.InsertUser(ctx, dbq.InsertUserParams{Username: "ops", Name: "Ops"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setup.GrantPlatformAdmin(ctx, pe.pool, "ops"); err != nil {
		t.Fatal(err)
	}
	opsTok, _ := pe.newToken(ops.ID, tokenOpts{})
	pe.must(200, pe.put(pe.admin, platBody()))
	r = pe.do("POST", "/api/v1/platform/mail/test", opsTok, nil)
	if r.status != 400 || !strings.Contains(string(r.body), "no email address") {
		t.Fatalf("no email: %d %s", r.status, r.body)
	}
}

func TestPlatformMailTest_RateLimited(t *testing.T) {
	pe := newPlatEnv(t, platOpts{key: platKey1, testLimit: 2})
	for range 2 {
		pe.must(200, pe.do("POST", "/api/v1/platform/mail/test", pe.admin, nil))
	}
	r := pe.do("POST", "/api/v1/platform/mail/test", pe.admin, nil)
	if r.status != 429 || r.errCode(t) != "rate_limited" || r.header.Get("Retry-After") == "" {
		t.Fatalf("%d %s %v", r.status, r.body, r.header)
	}
}

// TestPlatformSettings_NoSecretsInLogsEventsOrIdempotency: planted secrets
// go through every route, a failed send and refused requests, and appear in
// no log line, event, idempotency row or response (decisions-T9).
func TestPlatformSettings_NoSecretsInLogsEventsOrIdempotency(t *testing.T) {
	pe := newPlatEnv(t, platOpts{key: platKey1, smtp: smtptest.Options{StartTLS: true, Auth: true,
		AuthFailText: "5.7.8 echo " + platPassword}})
	var bodies []string
	keep := func(r resp) { bodies = append(bodies, string(r.body)) }
	keep(pe.put(pe.admin, platBody("mailProvider", "postmark", "postmarkFrom", "tracker@example.com", "postmarkToken", platToken)))
	keep(pe.put(pe.session, smtpBody("smtpPassword", "wrong"+platPassword, "postmarkToken", platToken)))
	keep(pe.do("GET", "/api/v1/platform/settings", pe.admin, nil))
	keep(pe.do("POST", "/api/v1/platform/mail/test", pe.admin, nil)) // fails: wrong password
	keep(pe.put(pe.admin, smtpBody("smtpSecurity", "none", "smtpPassword", platPassword)))
	keep(pe.do("PUT", "/api/v1/platform/settings", pe.admin, platBody("postmarkToken", platToken), "Idempotency-Key", "plat-1"))
	keep(pe.put(pe.admin, platBody("postmarkToken", platToken+" bad")))
	// Writes that do store idempotency rows and events, so the check is not vacuous.
	pe.createIssue(pe.admin, map[string]any{"title": "x"})
	pe.must(201, pe.do("POST", "/api/v1/orgs/orbit/labels", pe.env.chen, map[string]any{"name": "plat-label", "color": "#112233"}, "Idempotency-Key", "plat-2"))

	logs := pe.logs.String()
	if !strings.Contains(logs, "/api/v1/platform/settings") || !strings.Contains(logs, "mail test failed") {
		t.Fatalf("the log capture saw no platform lines; the check would be vacuous:\n%s", logs)
	}
	noPlanted(t, "the log", logs)
	for _, b := range bodies {
		noPlanted(t, "a response", b)
	}
	var events, idem string
	if err := pe.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(type || ' ' || payload::text, E'\n'), '') FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := pe.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(key || ' ' || path || ' ' || coalesce(convert_from(response_body, 'UTF8'), ''), E'\n'), '') FROM idempotency_keys`).Scan(&idem); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(idem, "plat-2") || events == "" {
		t.Fatalf("expected rows are missing; the check would be vacuous:\nevents %s\nidem %s", events, idem)
	}
	if strings.Contains(idem, "plat-1") {
		t.Fatal("the refused Idempotency-Key was stored")
	}
	noPlanted(t, "an event", events)
	noPlanted(t, "an idempotency row", idem)
	// The row holds no plaintext anywhere.
	if n := pe.countRows(`SELECT count(*) FROM platform_settings WHERE position(convert_to($1, 'UTF8') in coalesce(postmark_token_enc, '')) > 0
		OR position(convert_to($2, 'UTF8') in coalesce(smtp_password_enc, '')) > 0 OR row_to_json(platform_settings)::text LIKE '%' || $1 || '%'`,
		platToken, platPassword); n != 0 {
		t.Fatal("a secret is in platform_settings in plaintext")
	}
}

// TestPlatformSettings_KeyChanged: a server restarted with another key
// cannot open the stored secrets: it boots, says so in the log, falls back
// to the environment, and a PUT asks for the secrets again.
func TestPlatformSettings_KeyChanged(t *testing.T) {
	pe := newPlatEnv(t, platOpts{key: platKey1, smtp: smtptest.Options{StartTLS: true, Auth: true}})
	pe.must(200, pe.put(pe.admin, smtpBody("smtpPassword", platPassword, "postmarkToken", platToken)))
	ctx := context.Background()

	var logs syncBuffer
	sw := login.NewMailSwitch()
	other, err := platform.New(pe.pool, slog.New(slog.NewTextHandler(&logs, nil)), platform.Options{
		Key: platKey2, Switch: sw, Env: platform.EnvMail{Mailer: pe.envMail, Transport: platform.TransportDevLog},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if a := other.Active(); a != (platform.ActiveMail{Transport: "dev-log", Source: "environment"}) || !sw.Ready() {
		t.Fatalf("active %+v", a)
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "key_check mismatch") {
		t.Fatalf("no loud log:\n%s", logs.String())
	}
	noPlanted(t, "the log", logs.String())
	s, err := other.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !s.SecretsUnreadable || s.SMTPPasswordSet || s.PostmarkTokenSet || s.MailProvider != "smtp" {
		t.Fatalf("%+v", s)
	}
	p := &auth.Principal{UserID: pe.chen.ID}
	// Keeping the unreadable password is refused for smtp.
	_, err = other.Update(ctx, p, settingsInput(t, smtpBody()))
	if err == nil || !strings.Contains(err.Error(), "enter it again") {
		t.Fatalf("keep with another key: %v", err)
	}
	// Re-entered: works, and the old token blob is dropped.
	s, err = other.Update(ctx, p, settingsInput(t, smtpBody("smtpPassword", platPassword)))
	if err != nil {
		t.Fatal(err)
	}
	if s.SecretsUnreadable || !s.SMTPPasswordSet || s.PostmarkTokenSet || other.Active().Transport != "smtp" {
		t.Fatalf("after re-entry: %+v", s)
	}
}

// settingsInput decodes a body the way the handler does.
func settingsInput(t *testing.T, body map[string]any) platform.SettingsInput {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var in platform.SettingsInput
	if err := jsonUnmarshal(b, &in); err != nil {
		t.Fatal(err)
	}
	return in
}

// TestPlatformSettings_OtherServerFollows: a second server on the same
// database follows a PUT on its next refresh.
func TestPlatformSettings_OtherServerFollows(t *testing.T) {
	pe := newPlatEnv(t, platOpts{key: platKey1})
	sw := login.NewMailSwitch()
	other, err := platform.New(pe.pool, slog.New(slog.DiscardHandler), platform.Options{
		Key: platKey1, Switch: sw, Env: platform.EnvMail{Mailer: pe.envMail, Transport: platform.TransportDevLog},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := other.Refresh(ctx); err != nil || !sw.Ready() {
		t.Fatalf("refresh %v ready %v", err, sw.Ready())
	}
	pe.must(200, pe.put(pe.admin, platBody("mailProvider", "none")))
	if !sw.Ready() {
		t.Fatal("the other server changed before its refresh")
	}
	runCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { other.Run(runCtx, 20*time.Millisecond) })
	deadline := time.Now().Add(5 * time.Second)
	for sw.Ready() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	if sw.Ready() {
		t.Fatal("the other server never followed the PUT")
	}
}

// TestPlatformSettings_SwapUnderLoad: PUTs switch the provider while login
// requests and mail tests run (go test -race). Every answer is one of the
// expected ones; nothing is a 500.
func TestPlatformSettings_SwapUnderLoad(t *testing.T) {
	pe := newPlatEnv(t, platOpts{key: platKey1, smtp: smtptest.Options{StartTLS: true, Auth: true}})
	bodies := []map[string]any{
		platBody("mailProvider", "none", "smtpUsername", platUser),
		smtpBody("smtpPassword", platPassword),
		platBody("mailProvider", "env", "smtpUsername", platUser),
	}
	var stop atomic.Bool
	var bad atomic.Int64
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() {
			for n := 0; !stop.Load(); n++ {
				var r resp
				var err error
				if i%3 == 0 {
					r, err = pe.request("POST", "/api/v1/platform/mail/test", pe.admin, nil)
					if err == nil && r.status != 200 && r.status != 503 {
						bad.Add(1)
						t.Errorf("mail test: %d %s", r.status, r.body)
					}
					continue
				}
				r, err = pe.request("POST", "/api/v1/auth/otp", "", map[string]any{"email": fmt.Sprintf("load-%d-%d@example.com", i, n)})
				if err == nil && r.status != 202 && r.status != 503 {
					bad.Add(1)
					t.Errorf("otp: %d %s", r.status, r.body)
				}
			}
		})
	}
	for n := range 30 {
		pe.must(200, pe.put(pe.admin, bodies[n%len(bodies)]))
	}
	stop.Store(true)
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d unexpected answers", bad.Load())
	}
	// The last PUT (env) is in force.
	if a := pe.settings(pe.admin).ActiveMail; a.Source != "environment" {
		t.Fatalf("active %+v", a)
	}
}

// TestPlatformSettings_PortZeroStoresDefault: smtpPort 0 stores 587, also
// for provider smtp.
func TestPlatformSettings_PortZeroStoresDefault(t *testing.T) {
	pe := newPlatEnv(t, platOpts{key: platKey1})
	var s platform.Settings
	pe.must(200, pe.put(pe.admin, smtpBody("smtpPort", 0, "smtpPassword", platPassword))).json(t, &s)
	if s.SMTPPort != 587 || s.MailProvider != "smtp" {
		t.Fatalf("%+v", s)
	}
}

// TestPlatformMailTest_NoMailerAtSendIs503: the transport going away
// between the Ready check and the send (a PUT to none) is the same 503 as
// no transport, never a 502.
func TestPlatformMailTest_NoMailerAtSendIs503(t *testing.T) {
	pe := newPlatEnv(t, platOpts{key: platKey1})
	pe.envMail.mu.Lock()
	pe.envMail.fail = fmt.Errorf("switch emptied: %w", login.ErrNoMailer)
	pe.envMail.mu.Unlock()
	r := pe.do("POST", "/api/v1/platform/mail/test", pe.admin, nil)
	if r.status != 503 || r.errCode(t) != "login_unavailable" {
		t.Fatalf("%d %s", r.status, r.body)
	}
}

// TestPlatformSettings_CommitOrderWins (review of dff6f56): a write whose
// transaction STARTED first but committed last must still be the newest
// row, so every server, the one that took the other PUT included, follows
// it. With updated_at = now() (transaction start) it was older, and a
// server that had applied the other write skipped it for good.
func TestPlatformSettings_CommitOrderWins(t *testing.T) {
	pe := newPlatEnv(t, platOpts{key: platKey1})
	ctx := context.Background()

	// A starts first and holds its transaction-start clock.
	txA, err := pe.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txA.Rollback(ctx) }()
	var startA time.Time
	if err := txA.QueryRow(ctx, `SELECT now()`).Scan(&startA); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)

	// B: a PUT through this server, applied here at once.
	var b platform.Settings
	pe.must(200, pe.put(pe.admin, platBody("mailProvider", "none"))).json(t, &b)
	if !b.UpdatedAt.After(startA) || pe.sw.Ready() {
		t.Fatalf("B: updated %v (A started %v), ready %v", b.UpdatedAt, startA, pe.sw.Ready())
	}

	// A commits last, with the same statements a PUT runs.
	qa := dbq.New(txA)
	row, err := qa.GetPlatformSettingsForUpdate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a, err := qa.UpdatePlatformSettings(ctx, dbq.UpdatePlatformSettingsParams{
		MailProvider: "env", SmtpPort: row.SmtpPort, SmtpSecurity: row.SmtpSecurity, SignupMode: row.SignupMode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := txA.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if !a.UpdatedAt.After(b.UpdatedAt) {
		t.Fatalf("the last commit is not the newest row: A %v, B %v", a.UpdatedAt, b.UpdatedAt)
	}

	// This server (it applied B) follows A on its refresh, and so does a
	// fresh one: both match the database.
	if err := pe.plat.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := pe.plat.Active(); got.Source != "environment" || !pe.sw.Ready() {
		t.Fatalf("this server stayed on B: %+v", got)
	}
	sw := login.NewMailSwitch()
	other, err := platform.New(pe.pool, slog.New(slog.DiscardHandler), platform.Options{
		Key: platKey1, Switch: sw, Env: platform.EnvMail{Mailer: pe.envMail, Transport: platform.TransportDevLog},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Refresh(ctx); err != nil || other.Active().Source != "environment" {
		t.Fatalf("a fresh server: %+v %v", other.Active(), err)
	}

	// Two PUTs at once: whatever the order, the stored timestamps are
	// distinct and this server ends on the stored row.
	var wg sync.WaitGroup
	for _, body := range []map[string]any{platBody("mailProvider", "none"), platBody("mailProvider", "env")} {
		wg.Go(func() {
			if r, err := pe.request("PUT", "/api/v1/platform/settings", pe.admin, body); err != nil || r.status != 200 {
				t.Errorf("concurrent PUT: %v %d %s", err, r.status, r.body)
			}
		})
	}
	wg.Wait()
	final, err := pe.q.GetPlatformSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantReady := final.MailProvider == "env"
	if pe.sw.Ready() != wantReady {
		t.Fatalf("this server does not match the stored row (provider %s): ready %v", final.MailProvider, pe.sw.Ready())
	}
}
