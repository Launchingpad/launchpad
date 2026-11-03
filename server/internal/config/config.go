// Package config reads the server's environment once at boot.
package config

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"devops/internal/login"
	"devops/internal/netguard"
	"devops/internal/storage"
)

// Config is the runtime configuration. Secrets come only from the
// environment, never from a file in the repo.
type Config struct {
	DatabaseURL   string
	Port          int
	LogEnv        string // local | test | staging | production
	PublicBaseURL string

	// Login (T6a). With a Postmark token, codes are emailed. Without one,
	// login works only on a local/test server, where the code is logged
	// (FixedOTP, when set, is the code every request gets).
	PostmarkToken  string
	PostmarkFrom   string
	PostmarkStream string
	FixedOTP       string
	// OTPPepper keys the HMAC of stored login codes. Required (at least
	// login.MinPepperLen bytes) at every boot. Never stored in the
	// database and never logged.
	OTPPepper string

	// WebOrigins are the exact origins that get CORS headers (with
	// credentials). Empty means no CORS headers at all.
	WebOrigins []string

	// TrustedProxies are the proxy addresses (TRUSTED_PROXY_CIDRS) whose
	// X-Forwarded-For / X-Real-IP the server believes when it picks the
	// client address of the per-client sign-up limits
	// (decisions-T12 #9). Empty: no proxy is trusted.
	TrustedProxies []netip.Prefix

	// SettingsKey is SETTINGS_ENCRYPTION_KEY decoded: 32 bytes that seal the
	// secrets of the platform mail settings. Nil when unset: the
	// server then cannot store a mail secret, and settings that hold one
	// fall back to the environment's mail. Never logged.
	SettingsKey []byte

	// BackupKey is BACKUP_ENCRYPTION_KEY decoded: 32 bytes that seal the
	// secrets of backup destinations, the backup signing key and the secrets
	// of an unsaved test (LAUN-68, decisions-T16 #12). Nil when unset: the
	// boot goes on, and a write that stores a backup secret is refused later.
	// Never logged.
	BackupKey []byte

	// GitAllowPrivateProviders (GIT_ALLOW_PRIVATE_PROVIDERS=1) and
	// GitAllowPrivateCIDRs (GIT_ALLOW_PRIVATE_CIDRS, a comma list) are the two
	// operator switches that let the git API clients reach private
	// addresses (decisions-T17 #31). Both are validated at boot; the policy
	// is netguard.GitPolicy(GitAllowPrivateProviders, GitAllowPrivateCIDRs,
	// the app's attached subnets). Neither is a UI setting.
	GitAllowPrivateProviders bool
	GitAllowPrivateCIDRs     string

	// ImportWorkDir (IMPORT_WORK_DIR, default /work/import) holds the exports
	// of the web Linear import: ticket text and user emails, so mode 0700
	// (LAUN-8, decisions-T17 #39).
	ImportWorkDir string

	// S3 is the attachment store. Zero (no S3_* set) means
	// attachments are off: file routes answer 503 attachments_unavailable.
	// A partial set fails the boot.
	S3 storage.Config
}

// LocalDevS3Secret is the PUBLIC silo password in docker-compose.yml. It is
// refused outside LOG_ENV local/test.
const LocalDevS3Secret = "devops-local-s3-secret"

// LocalDevSettingsKey is the PUBLIC SETTINGS_ENCRYPTION_KEY in
// docker-compose.yml (hex of "launchpad-local-dev-settings-key"). It is
// refused outside LOG_ENV local/test, and so is a key of all zeros.
const LocalDevSettingsKey = "6c61756e63687061642d6c6f63616c2d6465762d73657474696e67732d6b6579"

// SettingsKeyLen is the decoded length of SETTINGS_ENCRYPTION_KEY.
const SettingsKeyLen = 32

// LocalDevBackupKey is the PUBLIC BACKUP_ENCRYPTION_KEY in docker-compose.yml
// (hex of "launchpad-local-dev-backup-key-0"). Like the settings key it is
// refused outside LOG_ENV local/test, and so is a key of all zeros.
const LocalDevBackupKey = "6c61756e63687061642d6c6f63616c2d6465762d6261636b75702d6b65792d30"

// BackupKeyLen is the decoded length of BACKUP_ENCRYPTION_KEY.
const BackupKeyLen = 32

// AttachmentsEnabled reports whether an object store is configured.
func (c Config) AttachmentsEnabled() bool { return c.S3.Endpoint != "" }

// LoginMode is how login codes are delivered.
type LoginMode string

// The login modes.
const (
	LoginPostmark LoginMode = "postmark" // emailed
	LoginDevLog   LoginMode = "dev-log"  // written to the log; local/test only
	LoginDisabled LoginMode = "disabled" // fail closed: no transport
)

// validLogEnvs: LOG_ENV is required and has no default,
// so a deployment that forgot it fails loudly at boot.
var validLogEnvs = map[string]bool{"local": true, "test": true, "staging": true, "production": true}

var sixDigits = regexp.MustCompile(`^[0-9]{6}$`)

// DevSeams reports whether this is a local or test server. The check is
// POSITIVE (a fail-closed rule): only an explicit local/test turns
// a dev seam on; anything else keeps it off.
func (c Config) DevSeams() bool { return c.LogEnv == "local" || c.LogEnv == "test" }

// LoginMode picks the code transport. It fails closed: a server that is not
// local/test and has no Postmark token has no login at all.
func (c Config) LoginMode() LoginMode {
	switch {
	case c.PostmarkToken != "":
		return LoginPostmark
	case c.DevSeams():
		return LoginDevLog
	default:
		return LoginDisabled
	}
}

// SecureCookies reports whether session cookies carry Secure. Only a local
// dev server (plain http on localhost) goes without.
func (c Config) SecureCookies() bool { return c.LogEnv != "local" }

// Load reads and validates the environment.
func Load() (Config, error) {
	return load(os.Getenv)
}

func load(getenv func(string) string) (Config, error) {
	c := Config{
		DatabaseURL:    getenv("DATABASE_URL"),
		LogEnv:         getenv("LOG_ENV"),
		PublicBaseURL:  getenv("PUBLIC_BASE_URL"),
		Port:           8080,
		PostmarkToken:  strings.TrimSpace(getenv("POSTMARK_SERVER_TOKEN")),
		PostmarkFrom:   strings.TrimSpace(getenv("POSTMARK_FROM")),
		PostmarkStream: strings.TrimSpace(getenv("POSTMARK_MESSAGE_STREAM")),
		FixedOTP:       strings.TrimSpace(getenv("AUTH_FIXED_OTP")),
		OTPPepper:      getenv("AUTH_OTP_PEPPER"),
		ImportWorkDir:  strings.TrimSpace(getenv("IMPORT_WORK_DIR")),
	}
	if c.ImportWorkDir == "" {
		c.ImportWorkDir = "/work/import"
	}
	var errs []error
	if !filepath.IsAbs(c.ImportWorkDir) {
		errs = append(errs, fmt.Errorf("IMPORT_WORK_DIR must be an absolute path (got %q)", c.ImportWorkDir))
	}
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if !validLogEnvs[c.LogEnv] {
		errs = append(errs, fmt.Errorf("LOG_ENV must be one of local, test, staging, production (got %q)", c.LogEnv))
	}
	if p := getenv("PORT"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			errs = append(errs, fmt.Errorf("PORT must be 1-65535 (got %q)", p))
		} else {
			c.Port = n
		}
	}
	if c.PostmarkToken != "" && c.PostmarkFrom == "" {
		errs = append(errs, errors.New("POSTMARK_FROM is required when POSTMARK_SERVER_TOKEN is set"))
	}
	if c.FixedOTP != "" {
		// Never echo the value: it is a credential on a dev server.
		switch {
		case !sixDigits.MatchString(c.FixedOTP):
			errs = append(errs, errors.New("AUTH_FIXED_OTP must be exactly 6 digits"))
		case !c.DevSeams():
			errs = append(errs, errors.New("AUTH_FIXED_OTP is allowed only with LOG_ENV local or test"))
		case c.PostmarkToken != "":
			errs = append(errs, errors.New("AUTH_FIXED_OTP cannot be combined with POSTMARK_SERVER_TOKEN"))
		}
	}
	// Fail closed: every server must be able to pepper codes. Since
	// a platform admin can turn login on at run time (the mail settings), so
	// the pepper is required at every boot, not only when the environment
	// has a mail transport (decisions-T9 #20).
	if len(c.OTPPepper) < login.MinPepperLen {
		errs = append(errs, fmt.Errorf("AUTH_OTP_PEPPER (a random secret of at least %d bytes) is required at every boot: login can be turned on at run time from the platform mail settings", login.MinPepperLen))
	}
	c.SettingsKey, errs = loadSettingsKey(getenv("SETTINGS_ENCRYPTION_KEY"), c.DevSeams(), errs)
	c.BackupKey, errs = loadBackupKey(getenv("BACKUP_ENCRYPTION_KEY"), c.DevSeams(), errs)
	c.S3, errs = loadS3(getenv, c.DevSeams(), errs)
	origins, err := parseOrigins(getenv("WEB_ORIGINS"))
	if err != nil {
		errs = append(errs, fmt.Errorf("WEB_ORIGINS: %w", err))
	}
	c.WebOrigins = origins
	proxies, err := ParseTrustedProxies(getenv("TRUSTED_PROXY_CIDRS"))
	if err != nil {
		errs = append(errs, fmt.Errorf("TRUSTED_PROXY_CIDRS: %w", err))
	}
	c.TrustedProxies = proxies
	c.GitAllowPrivateCIDRs = strings.TrimSpace(getenv("GIT_ALLOW_PRIVATE_CIDRS"))
	on, err := netguard.ParseSwitch("GIT_ALLOW_PRIVATE_PROVIDERS", getenv("GIT_ALLOW_PRIVATE_PROVIDERS"))
	if err != nil {
		errs = append(errs, err)
	}
	c.GitAllowPrivateProviders = on
	if _, err := netguard.GitPolicy(on, c.GitAllowPrivateCIDRs, nil); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return c, nil
}

// loadSettingsKey reads SETTINGS_ENCRYPTION_KEY: exactly 64 hex characters
// (32 random bytes, e.g. `openssl rand -hex 32`), or empty. The value is
// never echoed. Outside local/test the public dev key and an all-zero key
// are refused.
func loadSettingsKey(raw string, devSeams bool, errs []error) ([]byte, []error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errs
	}
	k, err := hex.DecodeString(raw)
	if err != nil || len(k) != SettingsKeyLen {
		return nil, append(errs, fmt.Errorf("SETTINGS_ENCRYPTION_KEY must be exactly %d hex characters (%d random bytes, e.g. `openssl rand -hex 32`)", 2*SettingsKeyLen, SettingsKeyLen))
	}
	if !devSeams {
		if strings.EqualFold(raw, LocalDevSettingsKey) {
			return nil, append(errs, errors.New("SETTINGS_ENCRYPTION_KEY is the public local-dev default; set a random key (openssl rand -hex 32)"))
		}
		if bytes.Count(k, []byte{0}) == len(k) {
			return nil, append(errs, errors.New("SETTINGS_ENCRYPTION_KEY is all zeros; set a random key (openssl rand -hex 32)"))
		}
	}
	return k, errs
}

// loadBackupKey reads BACKUP_ENCRYPTION_KEY with the rules of
// SETTINGS_ENCRYPTION_KEY (decisions-T16 #12, T9 #7): exactly 64 hex
// characters or empty; outside local/test the public dev key and an all-zero
// key are refused. The value is never echoed.
func loadBackupKey(raw string, devSeams bool, errs []error) ([]byte, []error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errs
	}
	k, err := hex.DecodeString(raw)
	if err != nil || len(k) != BackupKeyLen {
		return nil, append(errs, fmt.Errorf("BACKUP_ENCRYPTION_KEY must be exactly %d hex characters (%d random bytes, e.g. `openssl rand -hex 32`)", 2*BackupKeyLen, BackupKeyLen))
	}
	if !devSeams {
		if strings.EqualFold(raw, LocalDevBackupKey) {
			return nil, append(errs, errors.New("BACKUP_ENCRYPTION_KEY is the public local-dev default; set a random key (openssl rand -hex 32)"))
		}
		if bytes.Count(k, []byte{0}) == len(k) {
			return nil, append(errs, errors.New("BACKUP_ENCRYPTION_KEY is all zeros; set a random key (openssl rand -hex 32)"))
		}
	}
	return k, errs
}

// loadS3 reads S3_*. All of endpoint, bucket and both keys, or none. Outside
// local/test the client-facing endpoint must be https (a presigned URL is a
// credential for its lifetime) and the public dev secret is refused.
func loadS3(getenv func(string) string, devSeams bool, errs []error) (storage.Config, []error) {
	s := storage.Config{
		Endpoint:       strings.TrimSpace(getenv("S3_ENDPOINT")),
		PublicEndpoint: strings.TrimSpace(getenv("S3_PUBLIC_ENDPOINT")),
		Region:         strings.TrimSpace(getenv("S3_REGION")),
		Bucket:         strings.TrimSpace(getenv("S3_BUCKET")),
		AccessKey:      strings.TrimSpace(getenv("S3_ACCESS_KEY")),
		SecretKey:      getenv("S3_SECRET_KEY"),
	}
	if s.Endpoint == "" && s.Bucket == "" && s.AccessKey == "" && s.SecretKey == "" && s.PublicEndpoint == "" {
		return storage.Config{}, errs
	}
	var missing []string
	for _, f := range []struct{ name, v string }{
		{"S3_ENDPOINT", s.Endpoint}, {"S3_BUCKET", s.Bucket}, {"S3_ACCESS_KEY", s.AccessKey}, {"S3_SECRET_KEY", s.SecretKey},
	} {
		if f.v == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return s, append(errs, fmt.Errorf("attachments: set all of S3_ENDPOINT, S3_BUCKET, S3_ACCESS_KEY, S3_SECRET_KEY, or none (missing %s)", strings.Join(missing, ", ")))
	}
	st, err := storage.New(s)
	if err != nil {
		return s, append(errs, fmt.Errorf("attachments: %w", err))
	}
	if !devSeams {
		if !st.PublicIsHTTPS() {
			errs = append(errs, errors.New("attachments: S3_PUBLIC_ENDPOINT (or S3_ENDPOINT) must be https outside LOG_ENV local or test"))
		}
		if s.SecretKey == LocalDevS3Secret {
			errs = append(errs, errors.New("attachments: S3_SECRET_KEY is the public local-dev default; set a real secret"))
		}
	}
	return s, errs
}

// parseOrigins reads a comma list of exact origins (scheme://host[:port]).
// A wildcard, a path, a query or user info is refused, so a typo cannot
// widen CORS.
func parseOrigins(raw string) ([]string, error) {
	var out []string
	for part := range strings.SplitSeq(raw, ",") {
		o := strings.TrimSpace(part)
		if o == "" {
			continue
		}
		if strings.Contains(o, "*") {
			return nil, fmt.Errorf("%q: wildcards are not allowed; list each origin", o)
		}
		u, err := url.Parse(o)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", o, err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("%q: want scheme://host[:port], e.g. https://tracker.example.com", o)
		}
		out = append(out, strings.ToLower(u.Scheme+"://"+u.Host))
	}
	return out, nil
}

// ParseTrustedProxies reads TRUSTED_PROXY_CIDRS: a comma list of CIDRs
// (10.0.0.0/8) or single addresses (172.18.0.5). Empty parts are skipped.
// A prefix that covers every address (0.0.0.0/0, ::/0) is refused: it
// would let any client choose its own limiter key.
func ParseTrustedProxies(raw string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for part := range strings.SplitSeq(raw, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		var pfx netip.Prefix
		if strings.Contains(p, "/") {
			v, err := netip.ParsePrefix(p)
			if err != nil {
				return nil, fmt.Errorf("%q is not a CIDR or an IP address", p)
			}
			pfx = v.Masked()
		} else {
			a, err := netip.ParseAddr(p)
			if err != nil {
				return nil, fmt.Errorf("%q is not a CIDR or an IP address", p)
			}
			pfx = netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen())
		}
		if pfx.Bits() == 0 {
			return nil, fmt.Errorf("%q trusts every address; list only your proxies", p)
		}
		out = append(out, pfx)
	}
	return out, nil
}
