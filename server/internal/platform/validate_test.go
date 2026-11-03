package platform

import (
	"strings"
	"testing"

	"devops/internal/httpx"
)

func sp(s string) *string { return &s }
func ip(i int) *int       { return &i }

// validInput is a complete env-provider body.
func validInput() SettingsInput {
	return SettingsInput{
		MailProvider: sp("env"), SignupMode: sp("invite"),
		PostmarkFrom: sp(""), PostmarkMessageStream: sp(""),
		SMTPHost: sp(""), SMTPPort: ip(587), SMTPSecurity: sp("starttls"),
		SMTPUsername: sp(""), SMTPFromAddress: sp(""), SMTPFromName: sp(""),
	}
}

func withSecret(v string) httpx.Opt[Secret] { return httpx.Opt[Secret]{Set: true, V: Secret(v)} }

// TestNormalize is the PUT validation matrix for the checks that do not
// need the stored row.
func TestNormalize(t *testing.T) {
	const planted = "planted-token-value"
	tests := []struct {
		name string
		mod  func(*SettingsInput)
		want string // "" = valid; else a substring of the problems
	}{
		{name: "valid env", mod: func(*SettingsInput) {}},
		{name: "valid smtp fields", mod: func(in *SettingsInput) {
			in.MailProvider, in.SMTPHost, in.SMTPFromAddress, in.SMTPFromName = sp("smtp"), sp("smtp.example.com"), sp("tracker@example.com"), sp("Orbit")
		}},
		{name: "ip host", mod: func(in *SettingsInput) { in.SMTPHost = sp("192.0.2.10") }},
		{name: "valid postmark from with a name", mod: func(in *SettingsInput) { in.PostmarkFrom = sp("Launchpad <tracker@example.com>") }},
		{name: "missing mailProvider", mod: func(in *SettingsInput) { in.MailProvider = nil }, want: "mailProvider is required"},
		{name: "missing signupMode", mod: func(in *SettingsInput) { in.SignupMode = nil }, want: "signupMode is required"},
		{name: "missing smtpPort", mod: func(in *SettingsInput) { in.SMTPPort = nil }, want: "smtpPort is required"},
		{name: "missing smtpHost", mod: func(in *SettingsInput) { in.SMTPHost = nil }, want: "smtpHost is required"},
		{name: "missing postmarkFrom", mod: func(in *SettingsInput) { in.PostmarkFrom = nil }, want: "postmarkFrom is required"},
		{name: "bad provider", mod: func(in *SettingsInput) { in.MailProvider = sp("sendgrid") }, want: "mailProvider must be"},
		{name: "bad signup", mod: func(in *SettingsInput) { in.SignupMode = sp("open") }, want: "signupMode must be"},
		{name: "bad security", mod: func(in *SettingsInput) { in.SMTPSecurity = sp("ssl") }, want: "smtpSecurity must be"},
		{name: "host with scheme", mod: func(in *SettingsInput) { in.SMTPHost = sp("smtp://mail.example.com") }, want: "smtpHost must be"},
		{name: "host with port", mod: func(in *SettingsInput) { in.SMTPHost = sp("mail.example.com:25") }, want: "smtpHost must be"},
		{name: "host with CRLF", mod: func(in *SettingsInput) { in.SMTPHost = sp("mail.example.com\r\nRCPT") }, want: "smtpHost must be"},
		{name: "port too big", mod: func(in *SettingsInput) { in.SMTPPort = ip(70000) }, want: "smtpPort must be"},
		{name: "port negative", mod: func(in *SettingsInput) { in.SMTPPort = ip(-1) }, want: "smtpPort must be"},
		{name: "from with a name", mod: func(in *SettingsInput) { in.SMTPFromAddress = sp("Orbit <t@example.com>") }, want: "smtpFromAddress must be"},
		{name: "from junk", mod: func(in *SettingsInput) { in.SMTPFromAddress = sp("nope") }, want: "smtpFromAddress must be"},
		{name: "from name with CRLF", mod: func(in *SettingsInput) { in.SMTPFromName = sp("Orbit\r\nBcc: x@y.z") }, want: "smtpFromName must be"},
		{name: "from name too long", mod: func(in *SettingsInput) { in.SMTPFromName = sp(strings.Repeat("ø", 101)) }, want: "smtpFromName must be"},
		{name: "username with NUL", mod: func(in *SettingsInput) { in.SMTPUsername = sp("a\x00b") }, want: "smtpUsername must be"},
		{name: "postmark from junk", mod: func(in *SettingsInput) { in.PostmarkFrom = sp("not an address") }, want: "postmarkFrom must be"},
		{name: "bad stream", mod: func(in *SettingsInput) { in.PostmarkMessageStream = sp("out bound") }, want: "postmarkMessageStream must be"},
		{name: "token with a space", mod: func(in *SettingsInput) { in.PostmarkToken = withSecret(planted + " x") }, want: "postmarkToken holds a space"},
		{name: "token with CRLF", mod: func(in *SettingsInput) { in.PostmarkToken = withSecret(planted + "\r\n") }, want: "postmarkToken holds a control character"},
		{name: "token too long", mod: func(in *SettingsInput) { in.PostmarkToken = withSecret(planted + strings.Repeat("x", 600)) }, want: "postmarkToken is longer"},
		{name: "password with NUL", mod: func(in *SettingsInput) {
			in.SMTPUsername, in.SMTPPassword = sp("u"), withSecret(planted+"\x00")
		}, want: "smtpPassword holds a control character"},
		{name: "password without username", mod: func(in *SettingsInput) { in.SMTPPassword = withSecret(planted) }, want: "smtpPassword needs smtpUsername"},
		{name: "password with a space is fine", mod: func(in *SettingsInput) {
			in.SMTPUsername, in.SMTPPassword = sp("u"), withSecret("correct horse battery")
		}},
		{name: "several problems are all named", mod: func(in *SettingsInput) {
			in.MailProvider, in.SMTPPort = sp("x"), ip(99999)
		}, want: "mailProvider must be"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validInput()
			tt.mod(&in)
			_, problems := normalize(in)
			joined := strings.Join(problems, "; ")
			if tt.want == "" {
				if len(problems) != 0 {
					t.Fatalf("problems: %s", joined)
				}
				return
			}
			if !strings.Contains(joined, tt.want) {
				t.Fatalf("problems %q, want %q", joined, tt.want)
			}
			if strings.Contains(joined, planted) {
				t.Fatalf("a problem quotes the secret: %s", joined)
			}
		})
	}
	if f, p := normalize(func() SettingsInput { in := validInput(); in.SMTPPort = ip(0); return in }()); len(p) != 0 || f.port != 587 {
		t.Fatalf("port 0: %d %v", f.port, p)
	}
	in := validInput()
	in.MailProvider, in.SMTPPort = sp("x"), ip(99999)
	if _, p := normalize(in); len(p) != 2 {
		t.Fatalf("want both problems, got %v", p)
	}
}
