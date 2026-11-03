package login

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"testing"
	"time"

	"devops/internal/login/smtptest"
)

const (
	smtpUser = "relay-user"
	// A planted password: no error, message or log may ever hold it.
	smtpPass = "planted-smtp-password-7f3a9c"
)

func smtpMailer(srv *smtptest.Server, host, security string) *SMTPMailer {
	return &SMTPMailer{
		Host: host, Port: srv.Port, Security: security,
		FromAddress: "tracker@example.com", FromName: "Orbit Tracker",
		RootCAs: srv.RootCAs, Dial: srv.Dial, Timeout: 5 * time.Second,
	}
}

func TestSMTPMailer_Modes(t *testing.T) {
	tests := []struct {
		name     string
		opts     smtptest.Options
		host     string
		security string
		auth     bool
		wantTLS  bool
	}{
		{name: "starttls with auth", opts: smtptest.Options{StartTLS: true, Auth: true}, host: "mail.test", security: SMTPStartTLS, auth: true, wantTLS: true},
		{name: "starttls without auth", opts: smtptest.Options{StartTLS: true}, host: "mail.test", security: SMTPStartTLS, wantTLS: true},
		{name: "implicit tls with auth", opts: smtptest.Options{TLS: true, Auth: true}, host: "mail.test", security: SMTPTLS, auth: true, wantTLS: true},
		{name: "none, no auth, any host", opts: smtptest.Options{}, host: "mail.test", security: SMTPNone},
		{name: "none with auth to loopback", opts: smtptest.Options{Auth: true}, host: "127.0.0.1", security: SMTPNone, auth: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.opts.Username, tt.opts.Password = smtpUser, smtpPass
			srv := smtptest.Start(t, tt.opts)
			m := smtpMailer(srv, tt.host, tt.security)
			if tt.auth {
				m.Username, m.Password = smtpUser, smtpPass
			}
			if err := m.SendLoginCode(context.Background(), "chen@example.com", "042917"); err != nil {
				t.Fatalf("send: %v", err)
			}
			msgs := srv.Messages()
			if len(msgs) != 1 {
				t.Fatalf("messages = %d, want 1", len(msgs))
			}
			got := msgs[0]
			if got.TLS != tt.wantTLS {
				t.Fatalf("TLS = %v, want %v", got.TLS, tt.wantTLS)
			}
			if tt.auth && got.AuthUser != smtpUser {
				t.Fatalf("auth user = %q", got.AuthUser)
			}
			if got.From != "FROM:<tracker@example.com>" || len(got.To) != 1 || got.To[0] != "TO:<chen@example.com>" {
				t.Fatalf("envelope from %q to %v", got.From, got.To)
			}
			pm, err := mail.ReadMessage(bytes.NewReader(got.Data))
			if err != nil {
				t.Fatalf("parse message: %v", err)
			}
			if pm.Header.Get("Content-Type") != "text/plain; charset=UTF-8" ||
				pm.Header.Get("Subject") != "Your Launchpad login code" ||
				pm.Header.Get("From") != `"Orbit Tracker" <tracker@example.com>` {
				t.Fatalf("headers %v", pm.Header)
			}
			if !strings.Contains(string(got.Data), "Your login code is 042917") {
				t.Fatalf("body lacks the code:\n%s", got.Data)
			}
			if strings.Contains(string(got.Data), smtpPass) {
				t.Fatal("the password is in the message")
			}
		})
	}
}

// TestSMTPMailer_Errors: every failure is a *MailError naming the step and
// the reply code, never the server's text (a planted secret) or the password.
func TestSMTPMailer_Errors(t *testing.T) {
	const planted = "planted-server-text-91d2"
	tests := []struct {
		name     string
		opts     smtptest.Options
		host     string
		security string
		user     string
		pass     string
		timeout  time.Duration
		want     string
	}{
		{name: "auth refused", opts: smtptest.Options{StartTLS: true, Auth: true, AuthFailText: "5.7.8 bad password " + smtpPass + " " + planted},
			host: "mail.test", security: SMTPStartTLS, user: smtpUser, pass: "wrong-" + smtpPass, want: "refused the login (reply code 535)"},
		{name: "recipient refused", opts: smtptest.Options{StartTLS: true, RcptFailText: "5.1.1 " + planted},
			host: "mail.test", security: SMTPStartTLS, want: "refused RCPT TO (the recipient) (reply code 550)"},
		{name: "starttls not offered", opts: smtptest.Options{},
			host: "mail.test", security: SMTPStartTLS, want: "does not offer STARTTLS"},
		{name: "auth not offered", opts: smtptest.Options{StartTLS: true},
			host: "mail.test", security: SMTPStartTLS, user: smtpUser, pass: smtpPass, want: "does not offer AUTH"},
		{name: "certificate for another host", opts: smtptest.Options{TLS: true},
			host: "other.test", security: SMTPTLS, want: "not valid for other.test"},
		{name: "tls against a plain server", opts: smtptest.Options{},
			host: "mail.test", security: SMTPTLS, timeout: 2 * time.Second, want: "TLS handshake"},
		{name: "no greeting: timeout", opts: smtptest.Options{NoGreeting: true},
			host: "mail.test", security: SMTPNone, timeout: 300 * time.Millisecond, want: "did not answer in time"},
		{name: "plain auth without tls to a remote host", opts: smtptest.Options{Auth: true},
			host: "mail.test", security: SMTPNone, user: smtpUser, pass: smtpPass, want: "without TLS to a host that is not loopback"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.opts.Username, tt.opts.Password = smtpUser, smtpPass
			srv := smtptest.Start(t, tt.opts)
			m := smtpMailer(srv, tt.host, tt.security)
			m.Username, m.Password = tt.user, tt.pass
			if tt.timeout > 0 {
				m.Timeout = tt.timeout
			}
			err := m.SendText(context.Background(), "chen@example.com", "s", "t")
			var me *MailError
			if !errors.As(err, &me) {
				t.Fatalf("err = %v (%T), want *MailError", err, err)
			}
			msg := err.Error()
			if !strings.Contains(msg, tt.want) {
				t.Fatalf("err = %q, want %q", msg, tt.want)
			}
			if pub := PublicMailError(err); pub != msg {
				t.Fatalf("PublicMailError = %q, want %q", pub, msg)
			}
			for _, secret := range []string{planted, smtpPass, "wrong-" + smtpPass} {
				if strings.Contains(msg, secret) {
					t.Fatalf("the error repeats a secret: %q", msg)
				}
			}
			if len(srv.Messages()) != 0 {
				t.Fatal("a message was accepted")
			}
		})
	}
}

// TestBuildMessage_HeaderInjection: CR/LF in any header input cannot add a
// header or end the header block.
func TestBuildMessage_HeaderInjection(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	msg, err := buildMessage("tracker@example.com", "Orbit\r\nBcc: victim@example.com",
		"chen@example.com", "Hello\r\nBcc: victim@example.com\r\n\r\nInjected body", "line one\nline two\n", now)
	if err != nil {
		t.Fatal(err)
	}
	head, _, ok := strings.Cut(string(msg), "\r\n\r\n")
	if !ok {
		t.Fatalf("no header block end:\n%s", msg)
	}
	for _, l := range strings.Split(head, "\r\n") {
		if strings.HasPrefix(strings.ToLower(l), "bcc:") {
			t.Fatalf("an injected header got through:\n%s", head)
		}
	}
	pm, err := mail.ReadMessage(bytes.NewReader(msg))
	if err != nil {
		t.Fatal(err)
	}
	if len(pm.Header["Bcc"]) != 0 || len(pm.Header) != 8 {
		t.Fatalf("headers %v", pm.Header)
	}
	if got := pm.Header.Get("Subject"); got != "HelloBcc: victim@example.com Injected body" && got != "HelloBcc: victim@example.comInjected body" {
		t.Fatalf("subject %q", got)
	}
	if !strings.Contains(string(msg), "line one\r\nline two\r\n") {
		t.Fatalf("body line ends are not CRLF:\n%q", msg)
	}

	// Addresses with CR/LF, a display name or junk are refused outright.
	for _, bad := range []struct{ from, to string }{
		{"tracker@example.com\r\nBcc: x@y.z", "chen@example.com"},
		{"tracker@example.com", "chen@example.com\r\nBcc: x@y.z"},
		{"Launchpad <tracker@example.com>", "chen@example.com"},
		{"tracker@example.com", "not an address"},
		{"", "chen@example.com"},
	} {
		if _, err := buildMessage(bad.from, "", bad.to, "s", "t", now); err == nil {
			t.Fatalf("accepted from %q to %q", bad.from, bad.to)
		}
	}
}

// TestBuildMessage_UTF8: non-ASCII subject and name are RFC 2047 encoded,
// the body is quoted-printable UTF-8.
func TestBuildMessage_UTF8(t *testing.T) {
	msg, err := buildMessage("tracker@example.com", "Ørbit", "chen@example.com", "Prüfung", "Grüße\n", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	head, body, _ := strings.Cut(string(msg), "\r\n\r\n")
	for _, r := range head {
		if r > 0x7e {
			t.Fatalf("raw non-ASCII in the header block:\n%s", head)
		}
	}
	if !strings.Contains(head, "Subject: =?utf-8?q?Pr=C3=BCfung?=") {
		t.Fatalf("subject not encoded:\n%s", head)
	}
	if !strings.Contains(body, "Gr=C3=BC=C3=9Fe") {
		t.Fatalf("body not quoted-printable:\n%s", body)
	}
}

func TestSMTPMailer_StringHidesPassword(t *testing.T) {
	m := &SMTPMailer{Host: "mail.test", Port: 587, Security: SMTPStartTLS, Username: smtpUser, Password: smtpPass}
	for _, s := range []string{m.String(), m.GoString(),
		fmt.Sprintf("%v|%+v|%#v|%s", m, m, m, m), fmt.Sprintf("%v|%+v|%#v|%s", *m, *m, *m, *m)} {
		if strings.Contains(s, smtpPass) {
			t.Fatalf("the password shows: %q", s)
		}
	}
	p := NewPostmarkMailer("pm-planted-token", "tracker@example.com", "")
	for _, s := range []string{fmt.Sprintf("%v|%+v|%#v", p, p, p), fmt.Sprintf("%v|%+v|%#v", *p, *p, *p)} {
		if strings.Contains(s, "pm-planted-token") {
			t.Fatalf("the token shows: %q", s)
		}
	}
}
