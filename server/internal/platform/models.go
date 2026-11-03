package platform

import (
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"devops/internal/httpx"
)

// Secret is a write-only value (the Postmark server token, the SMTP
// password). Every way of printing one shows "[redacted]": fmt (any verb),
// slog and encoding/json. Reveal is the only way to the value, and only a
// mailer and the sealing code call it.
type Secret string

const redacted = "[redacted]"

// Reveal returns the value.
func (s Secret) Reveal() string { return string(s) }

// String hides the value.
func (Secret) String() string { return redacted }

// GoString hides the value from %#v.
func (Secret) GoString() string { return `"` + redacted + `"` }

// Format hides the value from every fmt verb (%s, %q, %x, %v ...).
func (Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// LogValue hides the value from slog.
func (Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON hides the value from encoding/json.
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// Mail providers (platform_settings.mail_provider).
const (
	ProviderEnv      = "env"      // the environment's mail (POSTMARK_*, else the dev log, else off)
	ProviderNone     = "none"     // no mail: login answers 503 login_unavailable
	ProviderPostmark = "postmark" // Postmark with the stored token
	ProviderSMTP     = "smtp"     // SMTP with the stored settings
)

// Transports in ActiveMail.
const (
	TransportPostmark = "postmark"
	TransportSMTP     = "smtp"
	TransportDevLog   = "dev-log"
	TransportNone     = "none"
)

// Sources in ActiveMail.
const (
	SourceSettings    = "settings"
	SourceEnvironment = "environment"
)

// Sign-up modes (platform_settings.signup_mode). Stored and validated here;
//
//	makes sign-up follow them.
const (
	SignupInvite   = "invite"
	SignupApproval = "approval"
	SignupClosed   = "closed"
)

// ActiveMail is what THIS server sends login email with right now. It
// differs from mailProvider when the settings fall back to the environment
// (provider env, or stored secrets this server cannot open).
type ActiveMail struct {
	Transport string `json:"transport"` // postmark | smtp | dev-log | none
	Source    string `json:"source"`    // settings | environment
}

// Settings is the GET /platform/settings body. Secrets are write-only:
// only whether one is stored (and readable) is shown.
type Settings struct {
	MailProvider          string `json:"mailProvider"`
	PostmarkFrom          string `json:"postmarkFrom"`
	PostmarkMessageStream string `json:"postmarkMessageStream"`
	PostmarkTokenSet      bool   `json:"postmarkTokenSet"`
	SMTPHost              string `json:"smtpHost"`
	SMTPPort              int    `json:"smtpPort"`
	SMTPSecurity          string `json:"smtpSecurity"`
	SMTPUsername          string `json:"smtpUsername"`
	SMTPPasswordSet       bool   `json:"smtpPasswordSet"`
	SMTPFromAddress       string `json:"smtpFromAddress"`
	SMTPFromName          string `json:"smtpFromName"`
	SignupMode            string `json:"signupMode"`
	// SignupModeChosen: a platform admin changed or confirmed the mode (LAUN-56).
	SignupModeChosen bool `json:"signupModeChosen"`
	// ActiveMail is this server's transport now.
	ActiveMail ActiveMail `json:"activeMail"`
	// LoginAvailable: login can issue codes on this server now (a transport
	// AND AUTH_OTP_PEPPER). False means /auth/otp answers 503.
	LoginAvailable bool `json:"loginAvailable"`
	// EncryptionKeySet: SETTINGS_ENCRYPTION_KEY is set, so a secret can be
	// stored.
	EncryptionKeySet bool `json:"encryptionKeySet"`
	// SecretsUnreadable: stored secrets were sealed with another key (or the
	// key is unset), so mail falls back to the environment until a platform
	// admin enters them again.
	SecretsUnreadable bool       `json:"secretsUnreadable"`
	UpdatedAt         time.Time  `json:"updatedAt"`
	UpdatedBy         *uuid.UUID `json:"updatedBy"`
}

// SettingsInput is the PUT /platform/settings body: the whole settings.
// Every non-secret field is required (an empty string clears a text). A
// secret absent or null keeps the stored one; "" clears it; a value
// replaces it.
type SettingsInput struct {
	MailProvider          *string           `json:"mailProvider"`
	PostmarkFrom          *string           `json:"postmarkFrom"`
	PostmarkMessageStream *string           `json:"postmarkMessageStream"`
	PostmarkToken         httpx.Opt[Secret] `json:"postmarkToken"`
	SMTPHost              *string           `json:"smtpHost"`
	SMTPPort              *int              `json:"smtpPort"`
	SMTPSecurity          *string           `json:"smtpSecurity"`
	SMTPUsername          *string           `json:"smtpUsername"`
	SMTPPassword          httpx.Opt[Secret] `json:"smtpPassword"`
	SMTPFromAddress       *string           `json:"smtpFromAddress"`
	SMTPFromName          *string           `json:"smtpFromName"`
	SignupMode            *string           `json:"signupMode"`
	// SignupModeConfirmed true records the mode as chosen even when it did not
	// change. Absent or false never clears the marker (LAUN-56).
	SignupModeConfirmed *bool `json:"signupModeConfirmed"`
}

// MailTestResult is the POST /platform/mail/test 200 body.
type MailTestResult struct {
	OK         bool       `json:"ok"`
	To         string     `json:"to"`
	ActiveMail ActiveMail `json:"activeMail"`
}
