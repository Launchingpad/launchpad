package platform

import (
	"net"
	"net/mail"
	"regexp"
	"strings"
	"unicode/utf8"

	"devops/internal/login"
)

// Field limits. Generous for real values, small enough that nothing odd
// reaches a mail server.
const (
	maxHost     = 253
	maxUsername = 256
	maxSecret   = 512
	maxFromName = 100
	maxStream   = 100

	defaultSMTPPort = 587
)

var (
	hostRe   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)
	streamRe = regexp.MustCompile(`^[A-Za-z0-9_-]*$`)
)

// fields is a validated SettingsInput without its secrets.
type fields struct {
	provider, signup                    string
	pmFrom, pmStream                    string
	host, security, username, from, nme string
	port                                int
}

// hasControl reports a control character (CR, LF, NUL, ...).
func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// normalize checks every field of a PUT body that does not depend on the
// stored row: presence, enums, formats and limits. problems has one entry
// per bad field, named as in the JSON body. A secret's value is never put
// in a problem.
func normalize(in SettingsInput) (f fields, problems []string) {
	req := func(name string, p *string) string {
		if p == nil {
			problems = append(problems, name+" is required")
			return ""
		}
		return strings.TrimSpace(*p)
	}
	f.provider = req("mailProvider", in.MailProvider)
	f.signup = req("signupMode", in.SignupMode)
	f.pmFrom = req("postmarkFrom", in.PostmarkFrom)
	f.pmStream = req("postmarkMessageStream", in.PostmarkMessageStream)
	f.host = req("smtpHost", in.SMTPHost)
	f.security = req("smtpSecurity", in.SMTPSecurity)
	f.username = req("smtpUsername", in.SMTPUsername)
	f.from = req("smtpFromAddress", in.SMTPFromAddress)
	f.nme = req("smtpFromName", in.SMTPFromName)
	if in.SMTPPort == nil {
		problems = append(problems, "smtpPort is required")
	} else {
		f.port = *in.SMTPPort
	}
	if in.MailProvider != nil {
		switch f.provider {
		case ProviderEnv, ProviderNone, ProviderPostmark, ProviderSMTP:
		default:
			problems = append(problems, "mailProvider must be env, none, postmark or smtp")
		}
	}
	if in.SignupMode != nil {
		switch f.signup {
		case SignupInvite, SignupApproval, SignupClosed:
		default:
			problems = append(problems, "signupMode must be invite, approval or closed")
		}
	}
	if in.SMTPSecurity != nil {
		switch f.security {
		case login.SMTPStartTLS, login.SMTPTLS, login.SMTPNone:
		default:
			problems = append(problems, "smtpSecurity must be starttls, tls or none")
		}
	}
	if f.pmFrom != "" {
		if a, err := mail.ParseAddress(f.pmFrom); err != nil || hasControl(f.pmFrom) || len(f.pmFrom) > 320 {
			problems = append(problems, `postmarkFrom must be an address or "Name <address>", e.g. Launchpad <tracker@example.com>`)
		} else if _, err := login.NormalizeEmail(a.Address); err != nil {
			problems = append(problems, "postmarkFrom must hold a valid email address")
		}
	}
	if len(f.pmStream) > maxStream || !streamRe.MatchString(f.pmStream) {
		problems = append(problems, "postmarkMessageStream must be a Postmark stream id (letters, digits, - and _; empty means outbound)")
	}
	if f.host != "" && (len(f.host) > maxHost || (net.ParseIP(f.host) == nil && !hostRe.MatchString(f.host))) {
		problems = append(problems, "smtpHost must be a host name or an IP address (no scheme, no port), e.g. smtp.example.com")
	}
	if in.SMTPPort != nil && (f.port < 0 || f.port > 65535) {
		problems = append(problems, "smtpPort must be 0 or 1-65535 (0 stores the default 587)")
	}
	if f.port == 0 {
		f.port = defaultSMTPPort
	}
	if len(f.username) > maxUsername || hasControl(f.username) {
		problems = append(problems, "smtpUsername must be at most 256 bytes with no control characters")
	}
	if f.from != "" {
		if _, err := login.NormalizeEmail(f.from); err != nil {
			problems = append(problems, "smtpFromAddress must be a plain email address, e.g. tracker@example.com (the name goes in smtpFromName)")
		}
	}
	if utf8.RuneCountInString(f.nme) > maxFromName || hasControl(f.nme) {
		problems = append(problems, "smtpFromName must be at most 100 characters with no control characters")
	}
	problems = append(problems, secretProblems("postmarkToken", in.PostmarkToken.V, true)...)
	problems = append(problems, secretProblems("smtpPassword", in.SMTPPassword.V, false)...)
	if in.SMTPPassword.V != "" && f.username == "" {
		problems = append(problems, "smtpPassword needs smtpUsername")
	}
	return f, problems
}

// secretProblems checks a new secret without ever quoting it. A token is
// printable ASCII with no space; a password may hold spaces but no control
// character (AUTH PLAIN separates its parts with NUL).
func secretProblems(name string, s Secret, token bool) []string {
	v := s.Reveal()
	if v == "" {
		return nil
	}
	if len(v) > maxSecret {
		return []string{name + " is longer than 512 bytes"}
	}
	if hasControl(v) || !utf8.ValidString(v) {
		return []string{name + " holds a control character"}
	}
	if token && strings.ContainsAny(v, " \t") {
		return []string{name + " holds a space"}
	}
	return nil
}
