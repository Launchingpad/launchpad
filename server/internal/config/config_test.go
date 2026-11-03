package config

import (
	"encoding/hex"
	"slices"
	"strings"
	"testing"
)

// pepper is a valid AUTH_OTP_PEPPER for tests (32+ bytes).
const pepper = "config-test-pepper-0123456789abcdef"

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
		port    int
	}{
		{name: "minimal", env: map[string]string{"DATABASE_URL": "postgres://x", "LOG_ENV": "local", "AUTH_OTP_PEPPER": pepper}, port: 8080},
		{name: "port", env: map[string]string{"DATABASE_URL": "postgres://x", "LOG_ENV": "test", "PORT": "9000", "AUTH_OTP_PEPPER": pepper}, port: 9000},
		{name: "missing db", env: map[string]string{"LOG_ENV": "local", "AUTH_OTP_PEPPER": pepper}, wantErr: true},
		{name: "missing log env", env: map[string]string{"DATABASE_URL": "postgres://x"}, wantErr: true},
		{name: "bad log env", env: map[string]string{"DATABASE_URL": "postgres://x", "LOG_ENV": "dev", "AUTH_OTP_PEPPER": pepper}, wantErr: true},
		{name: "bad port", env: map[string]string{"DATABASE_URL": "postgres://x", "LOG_ENV": "local", "PORT": "x", "AUTH_OTP_PEPPER": pepper}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := load(func(k string) string { return tt.env[k] })
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", c)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.Port != tt.port {
				t.Fatalf("port = %d, want %d", c.Port, tt.port)
			}
		})
	}
}

func TestLoad_LoginAndWeb(t *testing.T) {
	base := func(kv ...string) map[string]string {
		m := map[string]string{"DATABASE_URL": "postgres://x", "LOG_ENV": "production", "AUTH_OTP_PEPPER": pepper}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
		mode    LoginMode
		secure  bool
		origins []string
	}{
		{name: "production without postmark fails closed", env: base(), mode: LoginDisabled, secure: true},
		{name: "staging without postmark fails closed", env: base("LOG_ENV", "staging"), mode: LoginDisabled, secure: true},
		{name: "production with postmark", env: base("POSTMARK_SERVER_TOKEN", "t", "POSTMARK_FROM", "a@b.co"), mode: LoginPostmark, secure: true},
		{name: "production without postmark still needs a pepper", env: base("AUTH_OTP_PEPPER", ""), wantErr: "AUTH_OTP_PEPPER"},
		{name: "staging without postmark, short pepper", env: base("LOG_ENV", "staging", "AUTH_OTP_PEPPER", pepper[:31]), wantErr: "AUTH_OTP_PEPPER"},
		{name: "production without postmark, with a pepper", env: base(), mode: LoginDisabled, secure: true},
		{name: "postmark without a pepper fails the boot", env: base("POSTMARK_SERVER_TOKEN", "t", "POSTMARK_FROM", "a@b.co", "AUTH_OTP_PEPPER", ""), wantErr: "AUTH_OTP_PEPPER"},
		{name: "local without a pepper fails the boot", env: base("LOG_ENV", "local", "AUTH_OTP_PEPPER", ""), wantErr: "AUTH_OTP_PEPPER"},
		{name: "a short pepper fails the boot", env: base("LOG_ENV", "test", "AUTH_OTP_PEPPER", pepper[:31]), wantErr: "AUTH_OTP_PEPPER"},
		{name: "postmark needs a sender", env: base("POSTMARK_SERVER_TOKEN", "t"), wantErr: "POSTMARK_FROM"},
		{name: "local logs codes, no Secure", env: base("LOG_ENV", "local"), mode: LoginDevLog, secure: false},
		{name: "test logs codes, Secure", env: base("LOG_ENV", "test"), mode: LoginDevLog, secure: true},
		{name: "fixed otp on local", env: base("LOG_ENV", "local", "AUTH_FIXED_OTP", "000000"), mode: LoginDevLog},
		{name: "fixed otp in production is refused", env: base("AUTH_FIXED_OTP", "000000"), wantErr: "AUTH_FIXED_OTP is allowed only"},
		{name: "fixed otp with postmark is refused", env: base("LOG_ENV", "local", "AUTH_FIXED_OTP", "000000", "POSTMARK_SERVER_TOKEN", "t", "POSTMARK_FROM", "a@b.co"), wantErr: "cannot be combined"},
		{name: "fixed otp must be 6 digits", env: base("LOG_ENV", "local", "AUTH_FIXED_OTP", "12345"), wantErr: "6 digits"},
		// the seed is gone, and SEED_CHEN_EMAIL with it. A stale
		// value in an old environment is ignored, not an error.
		{name: "a leftover SEED_CHEN_EMAIL is ignored", env: base("SEED_CHEN_EMAIL", "chen"), mode: LoginDisabled, secure: true},
		{name: "origins", env: base("WEB_ORIGINS", "https://Tracker.example.com, http://localhost:5173/ ,"), mode: LoginDisabled, secure: true,
			origins: []string{"https://tracker.example.com", "http://localhost:5173"}},
		{name: "wildcard origin refused", env: base("WEB_ORIGINS", "*"), wantErr: "wildcards"},
		{name: "subdomain wildcard refused", env: base("WEB_ORIGINS", "https://*.example.com"), wantErr: "wildcards"},
		{name: "origin with a path refused", env: base("WEB_ORIGINS", "https://example.com/app"), wantErr: "scheme://host"},
		{name: "origin without scheme refused", env: base("WEB_ORIGINS", "example.com"), wantErr: "scheme://host"},
		{name: "null origin refused", env: base("WEB_ORIGINS", "null"), wantErr: "scheme://host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := load(func(k string) string { return tt.env[k] })
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err %v, want %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), "000000") || strings.Contains(err.Error(), "12345") || strings.Contains(err.Error(), pepper[:31]) {
					t.Fatal("the error echoes a secret")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.LoginMode() != tt.mode {
				t.Fatalf("mode %s, want %s", c.LoginMode(), tt.mode)
			}
			if c.SecureCookies() != tt.secure {
				t.Fatalf("secure %v, want %v", c.SecureCookies(), tt.secure)
			}
			if !slices.Equal(c.WebOrigins, tt.origins) {
				t.Fatalf("origins %v, want %v", c.WebOrigins, tt.origins)
			}
		})
	}
}

func TestLoad_S3(t *testing.T) {
	full := func(logEnv string, kv ...string) map[string]string {
		m := map[string]string{
			"DATABASE_URL": "postgres://x", "LOG_ENV": logEnv, "AUTH_OTP_PEPPER": pepper,
			"S3_ENDPOINT": "https://silo.example.com", "S3_BUCKET": "att",
			"S3_ACCESS_KEY": "ak", "S3_SECRET_KEY": "a-real-secret",
		}
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i+1] == "" {
				delete(m, kv[i])
			} else {
				m[kv[i]] = kv[i+1]
			}
		}
		return m
	}
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
		enabled bool
	}{
		{name: "none set: attachments off", env: map[string]string{"DATABASE_URL": "postgres://x", "LOG_ENV": "production", "AUTH_OTP_PEPPER": pepper}},
		{name: "full https production", env: full("production"), enabled: true},
		{name: "partial set fails", env: full("production", "S3_SECRET_KEY", ""), wantErr: "missing S3_SECRET_KEY"},
		{name: "only public endpoint fails", env: map[string]string{"DATABASE_URL": "postgres://x", "LOG_ENV": "local", "AUTH_OTP_PEPPER": pepper, "S3_PUBLIC_ENDPOINT": "http://localhost:9100"}, wantErr: "missing S3_ENDPOINT"},
		{name: "http public endpoint refused in production", env: full("production", "S3_ENDPOINT", "http://silo:9000"), wantErr: "must be https"},
		{name: "internal http with https public ok", env: full("staging", "S3_ENDPOINT", "http://srv-captain--silo:9000", "S3_PUBLIC_ENDPOINT", "https://silo.example.com"), enabled: true},
		{name: "http allowed on local", env: full("local", "S3_ENDPOINT", "http://silo:9000"), enabled: true},
		{name: "dev secret refused in production", env: full("production", "S3_SECRET_KEY", LocalDevS3Secret), wantErr: "public local-dev default"},
		{name: "dev secret allowed on test", env: full("test", "S3_SECRET_KEY", LocalDevS3Secret), enabled: true},
		{name: "endpoint with a path fails", env: full("production", "S3_ENDPOINT", "https://silo.example.com/bucket"), wantErr: "S3_ENDPOINT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := load(func(k string) string { return tt.env[k] })
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.AttachmentsEnabled() != tt.enabled {
				t.Fatalf("AttachmentsEnabled = %v, want %v", c.AttachmentsEnabled(), tt.enabled)
			}
		})
	}
}

// TestLoad_BackupKey: BACKUP_ENCRYPTION_KEY is 64 hex characters or unset (the
// boot goes on); the public dev key and an all-zero key work on local/test
// only; an error never echoes the value (LAUN-68, decisions-T16 #12).
func TestLoad_BackupKey(t *testing.T) {
	const good = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"
	zero := strings.Repeat("0", 64)
	tests := []struct {
		name, logEnv, key string
		wantErr           string
		wantKey           bool
	}{
		{name: "unset boots", logEnv: "production", key: "", wantKey: false},
		{name: "good", logEnv: "production", key: good, wantKey: true},
		{name: "upper-case hex", logEnv: "production", key: strings.ToUpper(good), wantKey: true},
		{name: "too short", logEnv: "local", key: good[:62], wantErr: "BACKUP_ENCRYPTION_KEY must be exactly 64 hex characters"},
		{name: "not hex", logEnv: "local", key: strings.Repeat("zz", 32), wantErr: "exactly 64 hex characters"},
		{name: "dev key on production", logEnv: "production", key: LocalDevBackupKey, wantErr: "public local-dev default"},
		{name: "dev key on staging", logEnv: "staging", key: LocalDevBackupKey, wantErr: "public local-dev default"},
		{name: "dev key on local", logEnv: "local", key: LocalDevBackupKey, wantKey: true},
		{name: "dev key on test", logEnv: "test", key: LocalDevBackupKey, wantKey: true},
		{name: "zeros on production", logEnv: "production", key: zero, wantErr: "all zeros"},
		{name: "zeros on local", logEnv: "local", key: zero, wantKey: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{"DATABASE_URL": "postgres://x", "LOG_ENV": tt.logEnv, "AUTH_OTP_PEPPER": pepper, "BACKUP_ENCRYPTION_KEY": tt.key}
			c, err := load(func(k string) string { return env[k] })
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				if tt.key != "" && strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.key)) {
					t.Fatal("the error echoes the key")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := len(c.BackupKey) == BackupKeyLen; got != tt.wantKey {
				t.Fatalf("key loaded = %v (len %d), want %v", got, len(c.BackupKey), tt.wantKey)
			}
			if c.SettingsKey != nil {
				t.Fatal("the settings key was read from the backup variable")
			}
		})
	}
	if string(mustHex(t, LocalDevBackupKey)) != "launchpad-local-dev-backup-key-0" {
		t.Fatal("LocalDevBackupKey is not the hex of its documented text")
	}
}

// TestLoad_SettingsKey: SETTINGS_ENCRYPTION_KEY is 64 hex characters or
// unset; the public dev key and an all-zero key work on local/test only;
// an error never echoes the value.
func TestLoad_SettingsKey(t *testing.T) {
	const good = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"
	zero := strings.Repeat("0", 64)
	tests := []struct {
		name, logEnv, key string
		wantErr           string // "" = no error
		wantKey           bool
	}{
		{name: "unset", logEnv: "production", key: "", wantKey: false},
		{name: "good", logEnv: "production", key: good, wantKey: true},
		{name: "upper-case hex", logEnv: "production", key: strings.ToUpper(good), wantKey: true},
		{name: "too short", logEnv: "local", key: good[:62], wantErr: "exactly 64 hex characters"},
		{name: "not hex", logEnv: "local", key: strings.Repeat("zz", 32), wantErr: "exactly 64 hex characters"},
		{name: "base64 refused", logEnv: "local", key: "DxotPEtaaXiHlqW0w9Lh8A8eLTxLWml4h5altMPS4fA=", wantErr: "exactly 64 hex characters"},
		{name: "dev key on production", logEnv: "production", key: LocalDevSettingsKey, wantErr: "public local-dev default"},
		{name: "dev key on staging", logEnv: "staging", key: LocalDevSettingsKey, wantErr: "public local-dev default"},
		{name: "dev key on local", logEnv: "local", key: LocalDevSettingsKey, wantKey: true},
		{name: "dev key on test", logEnv: "test", key: LocalDevSettingsKey, wantKey: true},
		{name: "zeros on production", logEnv: "production", key: zero, wantErr: "all zeros"},
		{name: "zeros on local", logEnv: "local", key: zero, wantKey: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{"DATABASE_URL": "postgres://x", "LOG_ENV": tt.logEnv, "AUTH_OTP_PEPPER": pepper, "SETTINGS_ENCRYPTION_KEY": tt.key}
			c, err := load(func(k string) string { return env[k] })
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				if tt.key != "" && strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.key)) {
					t.Fatal("the error echoes the key")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := len(c.SettingsKey) == SettingsKeyLen; got != tt.wantKey {
				t.Fatalf("key loaded = %v (len %d), want %v", got, len(c.SettingsKey), tt.wantKey)
			}
		})
	}
	// The constant is the hex of its documented text.
	if string(mustHex(t, LocalDevSettingsKey)) != "launchpad-local-dev-settings-key" {
		t.Fatal("LocalDevSettingsKey is not the hex of launchpad-local-dev-settings-key")
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseTrustedProxies(t *testing.T) {
	cases := []struct {
		name, raw string
		want      []string
		wantErr   string
	}{
		{name: "empty", raw: "", want: nil},
		{name: "list", raw: " 172.18.0.0/16, 192.0.2.10 ,, 2001:db8::/32", want: []string{"172.18.0.0/16", "192.0.2.10/32", "2001:db8::/32"}},
		{name: "masked", raw: "10.1.2.3/8", want: []string{"10.0.0.0/8"}},
		{name: "everyone refused", raw: "0.0.0.0/0", wantErr: "trusts every address"},
		{name: "everyone v6 refused", raw: "::/0", wantErr: "trusts every address"},
		{name: "junk", raw: "proxy.local", wantErr: "not a CIDR"},
		{name: "bad prefix", raw: "10.0.0.0/40", wantErr: "not a CIDR"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseTrustedProxies(c.raw)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var s []string
			for _, p := range got {
				s = append(s, p.String())
			}
			if !slices.Equal(s, c.want) {
				t.Fatalf("got %v, want %v", s, c.want)
			}
		})
	}
}

func TestLoad_GitPrivateSwitches(t *testing.T) {
	base := func(kv ...string) map[string]string {
		m := map[string]string{"DATABASE_URL": "postgres://x", "LOG_ENV": "production", "AUTH_OTP_PEPPER": pepper}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	c, err := load(func(k string) string { return base()[k] })
	if err != nil || c.GitAllowPrivateProviders || c.GitAllowPrivateCIDRs != "" {
		t.Fatalf("unset: %+v %v", c, err)
	}
	env := base("GIT_ALLOW_PRIVATE_PROVIDERS", "1", "GIT_ALLOW_PRIVATE_CIDRS", " 192.168.4.0/24 ")
	c, err = load(func(k string) string { return env[k] })
	if err != nil || !c.GitAllowPrivateProviders || c.GitAllowPrivateCIDRs != "192.168.4.0/24" {
		t.Fatalf("set: %+v %v", c, err)
	}
	for _, bad := range []map[string]string{
		base("GIT_ALLOW_PRIVATE_PROVIDERS", "yes"),
		base("GIT_ALLOW_PRIVATE_CIDRS", "0.0.0.0/0"),
		base("GIT_ALLOW_PRIVATE_CIDRS", "127.0.0.0/8"),
		base("GIT_ALLOW_PRIVATE_CIDRS", "nonsense"),
	} {
		if _, err := load(func(k string) string { return bad[k] }); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

func TestLoad_ImportWorkDir(t *testing.T) {
	env := func(dir string) func(string) string {
		m := map[string]string{"DATABASE_URL": "postgres://x", "LOG_ENV": "production", "AUTH_OTP_PEPPER": pepper, "IMPORT_WORK_DIR": dir}
		return func(k string) string { return m[k] }
	}
	c, err := load(env(""))
	if err != nil || c.ImportWorkDir != "/work/import" {
		t.Fatalf("default: %q %v", c.ImportWorkDir, err)
	}
	c, err = load(env(" /data/imp "))
	if err != nil || c.ImportWorkDir != "/data/imp" {
		t.Fatalf("set: %q %v", c.ImportWorkDir, err)
	}
	if _, err := load(env("relative/dir")); err == nil {
		t.Fatal("a relative path was accepted")
	}
}
