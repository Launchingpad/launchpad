package platform

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"devops/internal/dbq"
	"devops/internal/httpx"
	"devops/internal/login"
)

const (
	plantedToken    = "planted-pm-token-55aa"
	plantedPassword = "planted-smtp-pw-66bb"
)

// envMailer stands in for the environment's transport.
type envMailer struct{}

func (envMailer) SendLoginCode(context.Context, string, string) error    { return nil }
func (envMailer) SendText(context.Context, string, string, string) error { return nil }

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func newTestService(t *testing.T, k []byte) (*Service, *login.MailSwitch, *lockedBuf) {
	t.Helper()
	var logs lockedBuf
	sw := login.NewMailSwitch()
	s, err := New(nil, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Options{Key: k, Switch: sw, Env: EnvMail{Mailer: envMailer{}, Transport: TransportPostmark}})
	if err != nil {
		t.Fatal(err)
	}
	return s, sw, &logs
}

// sealedRow is an smtp row with both secrets sealed under k.
func sealedRow(t *testing.T, k []byte, at time.Time) dbq.PlatformSetting {
	t.Helper()
	kr := mustKeyring(t, k)
	return dbq.PlatformSetting{
		ID: true, MailProvider: ProviderSMTP, SignupMode: SignupInvite,
		PostmarkTokenEnc: kr.Seal(colPostmarkToken, []byte(plantedToken)), PostmarkFrom: "tracker@example.com",
		SmtpHost: "mail.test", SmtpPort: 587, SmtpSecurity: login.SMTPStartTLS, SmtpUsername: "u",
		SmtpPasswordEnc: kr.Seal(colSMTPPassword, []byte(plantedPassword)), SmtpFromAddress: "tracker@example.com",
		KeyCheck: kr.KeyCheck(), UpdatedAt: at,
	}
}

func noSecrets(t *testing.T, where, s string) {
	t.Helper()
	for _, v := range []string{plantedToken, plantedPassword} {
		if strings.Contains(s, v) {
			t.Fatalf("%s holds a secret:\n%s", where, s)
		}
	}
}

func TestApply_Providers(t *testing.T) {
	ctx := context.Background()
	at := time.Now()
	s, sw, logs := newTestService(t, key(1))

	row := sealedRow(t, key(1), at)
	s.apply(ctx, row)
	if a := s.Active(); a != (ActiveMail{TransportSMTP, SourceSettings}) || !sw.Ready() {
		t.Fatalf("smtp: active %+v ready %v", a, sw.Ready())
	}
	v := s.view(s.open(row))
	if !v.PostmarkTokenSet || !v.SMTPPasswordSet || v.SecretsUnreadable || !v.EncryptionKeySet {
		t.Fatalf("view %+v", v)
	}

	row.MailProvider, row.UpdatedAt = ProviderPostmark, at.Add(time.Second)
	s.apply(ctx, row)
	if a := s.Active(); a != (ActiveMail{TransportPostmark, SourceSettings}) {
		t.Fatalf("postmark: %+v", a)
	}

	row.MailProvider, row.UpdatedAt = ProviderNone, at.Add(2*time.Second)
	s.apply(ctx, row)
	if a := s.Active(); a != (ActiveMail{TransportNone, SourceSettings}) || sw.Ready() {
		t.Fatalf("none: active %+v ready %v", a, sw.Ready())
	}

	row.MailProvider, row.UpdatedAt = ProviderEnv, at.Add(3*time.Second)
	s.apply(ctx, row)
	if a := s.Active(); a != (ActiveMail{TransportPostmark, SourceEnvironment}) || !sw.Ready() {
		t.Fatalf("env: active %+v ready %v", a, sw.Ready())
	}

	// An older row (a refresh that read before a PUT) changes nothing.
	row.MailProvider, row.UpdatedAt = ProviderNone, at.Add(time.Second)
	s.apply(ctx, row)
	if !sw.Ready() {
		t.Fatal("an older row was applied")
	}
	noSecrets(t, "the log", logs.String())
}

// TestApply_KeyMismatchFallsBack: secrets sealed under another key (or no
// key at all, or a changed blob) never stop the boot: mail falls back to
// the environment, the log says so loudly, and no secret is logged.
func TestApply_KeyMismatchFallsBack(t *testing.T) {
	ctx := context.Background()
	tamper := func(r dbq.PlatformSetting) dbq.PlatformSetting {
		r.SmtpPasswordEnc = append([]byte(nil), r.SmtpPasswordEnc...)
		r.SmtpPasswordEnc[5] ^= 1
		return r
	}
	tests := []struct {
		name string
		key  []byte
		row  func(dbq.PlatformSetting) dbq.PlatformSetting
		why  string
	}{
		{name: "another key", key: key(2), row: func(r dbq.PlatformSetting) dbq.PlatformSetting { return r }, why: "key_check mismatch"},
		{name: "no key", key: nil, row: func(r dbq.PlatformSetting) dbq.PlatformSetting { return r }, why: "not set on this server"},
		{name: "tampered blob", key: key(1), row: tamper, why: "integrity check"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, sw, logs := newTestService(t, tt.key)
			row := tt.row(sealedRow(t, key(1), time.Now()))
			s.apply(ctx, row)
			if a := s.Active(); a != (ActiveMail{TransportPostmark, SourceEnvironment}) || !sw.Ready() {
				t.Fatalf("active %+v ready %v, want the environment's postmark", a, sw.Ready())
			}
			v := s.view(s.open(row))
			if !v.SecretsUnreadable || v.PostmarkTokenSet || v.SMTPPasswordSet {
				t.Fatalf("view %+v", v)
			}
			out := logs.String()
			if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "cannot be opened") || !strings.Contains(out, tt.why) {
				t.Fatalf("no loud log naming %q:\n%s", tt.why, out)
			}
			noSecrets(t, "the log", out)

			// Provider none stays none: a lost key never turns mail on, and
			// the log does not claim a fallback.
			before := len(logs.String())
			row.MailProvider, row.UpdatedAt = ProviderNone, row.UpdatedAt.Add(time.Second)
			s.apply(ctx, row)
			if sw.Ready() {
				t.Fatal("provider none sent mail after a key mismatch")
			}
			if out := logs.String()[before:]; strings.Contains(out, "falls back") || !strings.Contains(out, "needs none of them") {
				t.Fatalf("provider none log:\n%s", out)
			}

			// smtp WITHOUT a username needs no secret: it is used as it is.
			row.MailProvider, row.SmtpUsername, row.UpdatedAt = ProviderSMTP, "", row.UpdatedAt.Add(time.Second)
			s.apply(ctx, row)
			if a := s.Active(); a != (ActiveMail{TransportSMTP, SourceSettings}) {
				t.Fatalf("smtp without auth fell back: %+v", a)
			}
		})
	}
}

func TestUseEnvironment(t *testing.T) {
	s, sw, _ := newTestService(t, nil)
	if sw.Ready() {
		t.Fatal("ready before anything is applied")
	}
	s.UseEnvironment()
	if a := s.Active(); a != (ActiveMail{TransportPostmark, SourceEnvironment}) || !sw.Ready() {
		t.Fatalf("active %+v", a)
	}
}

func optAbsent() httpx.Opt[Secret] { return httpx.Opt[Secret]{} }
func optNull() httpx.Opt[Secret]   { return httpx.Opt[Secret]{Set: true, Null: true} }

func TestNextSecret(t *testing.T) {
	s, _, _ := newTestService(t, key(1))
	stored := []byte("old-blob-0123456789012345678901")
	blob, dropped, err := s.nextSecret(colPostmarkToken, optAbsent(), stored, false)
	if err != nil || dropped || !bytes.Equal(blob, stored) {
		t.Fatal("absent did not keep")
	}
	blob, _, _ = s.nextSecret(colPostmarkToken, optNull(), stored, false)
	if !bytes.Equal(blob, stored) {
		t.Fatal("null did not keep")
	}
	blob, _, _ = s.nextSecret(colPostmarkToken, withSecret(""), stored, false)
	if blob != nil {
		t.Fatal("empty did not clear")
	}
	blob, dropped, _ = s.nextSecret(colPostmarkToken, optAbsent(), stored, true)
	if blob != nil || !dropped {
		t.Fatal("an unreadable blob was kept")
	}
	blob, _, _ = s.nextSecret(colPostmarkToken, withSecret(plantedToken), stored, false)
	if pt, err := s.keys.Open(colPostmarkToken, blob); err != nil || string(pt) != plantedToken {
		t.Fatalf("set: %q %v", pt, err)
	}
	nokey, _, _ := newTestService(t, nil)
	if _, _, err := nokey.nextSecret(colPostmarkToken, withSecret(plantedToken), nil, false); err != errNoKey {
		t.Fatalf("no key: %v", err)
	}
}

// TestLoginAvailable: GET's loginAvailable follows the login service, so a
// server without AUTH_OTP_PEPPER says false even with a transport.
func TestLoginAvailable(t *testing.T) {
	s, sw, _ := newTestService(t, key(1))
	if s.LoginAvailable() {
		t.Fatal("available with an empty switch")
	}
	sw.Set(envMailer{})
	if !s.LoginAvailable() {
		t.Fatal("not available with a transport and no login check")
	}
	deliver := func(context.Context, string, string) {}
	noPepper := login.New(nil, slog.New(slog.DiscardHandler), deliver, login.Options{Available: sw.Ready})
	s.SetLoginCheck(noPepper.Enabled)
	if s.LoginAvailable() {
		t.Fatal("available without a pepper")
	}
	withPepper := login.New(nil, slog.New(slog.DiscardHandler), deliver, login.Options{Available: sw.Ready, Pepper: make([]byte, login.MinPepperLen)})
	s.SetLoginCheck(withPepper.Enabled)
	if !s.LoginAvailable() || !s.view(s.open(sealedRow(t, key(1), time.Now()))).LoginAvailable {
		t.Fatal("not available with a pepper and a transport")
	}
}
