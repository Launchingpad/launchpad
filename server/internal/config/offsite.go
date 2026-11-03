package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"devops/internal/netguard"
)

// Offsite is the small configuration of `server offsite` (LAUN-71,
// decisions-T16 #10, #46). The worker needs no AUTH_OTP_PEPPER and no mail setting:
// it never holds the mail key. Its S3_* variables (ticket 14, LAUN-80) are all or
// none and name a READ-ONLY silo user made for offsite alone. Secrets come from the
// environment only and are never logged.
type Offsite struct {
	// DatabaseURL is the URL of offsite's own restricted role (#15).
	DatabaseURL string
	LogEnv      string
	// BackupKey is BACKUP_ENCRYPTION_KEY decoded. Unlike the app, offsite
	// REQUIRES it at boot: without it no destination secret or signing key can
	// be opened, so a worker that idled would only hide the fault.
	BackupKey []byte
	// AllowPrivateCIDRs is BACKUP_ALLOW_PRIVATE_CIDRS, validated here (#35).
	AllowPrivateCIDRs string
	// DumpDir (BACKUP_DUMP_DIR, default /backups) is the backups volume, read only.
	DumpDir string
	// WorkDir (OFFSITE_WORK_DIR, default /work; the deploy compose file sets OFFSITE_WORK_DIR=/offsite-work) is the scratch volume: scratch
	// files and the instance-id file (#18, #37).
	WorkDir string
	// HealthFile (OFFSITE_HEALTH_FILE, default /tmp/offsite.alive) is touched
	// while the process lives; `offsite healthcheck` reads its age.
	HealthFile string
	// S3Endpoint, S3Bucket, S3AccessKey, S3SecretKey and S3Region are offsite's
	// own silo access (all four or none; the region defaults to us-east-1). None:
	// no attachment backup.
	S3Endpoint, S3Bucket, S3AccessKey, S3SecretKey, S3Region string
	// Version (LAUNCHPAD_VERSION, default "unversioned") goes into each manifest.
	Version string
}

var versionRe = regexp.MustCompile(`^[0-9A-Za-z._+-]{1,64}$`)

// LoadOffsite reads and validates the offsite environment.
func LoadOffsite(getenv func(string) string) (Offsite, error) {
	c := Offsite{
		DatabaseURL:       getenv("DATABASE_URL"),
		LogEnv:            getenv("LOG_ENV"),
		AllowPrivateCIDRs: strings.TrimSpace(getenv("BACKUP_ALLOW_PRIVATE_CIDRS")),
		DumpDir:           strings.TrimSpace(getenv("BACKUP_DUMP_DIR")),
		WorkDir:           strings.TrimSpace(getenv("OFFSITE_WORK_DIR")),
		HealthFile:        strings.TrimSpace(getenv("OFFSITE_HEALTH_FILE")),
		Version:           strings.TrimSpace(getenv("LAUNCHPAD_VERSION")),
		S3Endpoint:        strings.TrimSpace(getenv("S3_ENDPOINT")),
		S3Bucket:          strings.TrimSpace(getenv("S3_BUCKET")),
		S3AccessKey:       getenv("S3_ACCESS_KEY"),
		S3SecretKey:       getenv("S3_SECRET_KEY"),
		S3Region:          strings.TrimSpace(getenv("S3_REGION")),
	}
	if c.DumpDir == "" {
		c.DumpDir = "/backups"
	}
	if c.WorkDir == "" {
		c.WorkDir = "/work"
	}
	if c.HealthFile == "" {
		c.HealthFile = "/tmp/offsite.alive"
	}
	if c.Version == "" {
		c.Version = "unversioned"
	}
	var errs []error
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if !validLogEnvs[c.LogEnv] {
		errs = append(errs, fmt.Errorf("LOG_ENV must be one of local, test, staging, production (got %q)", c.LogEnv))
	}
	for _, p := range []struct{ name, v string }{
		{"BACKUP_DUMP_DIR", c.DumpDir}, {"OFFSITE_WORK_DIR", c.WorkDir}, {"OFFSITE_HEALTH_FILE", c.HealthFile},
	} {
		if !filepath.IsAbs(p.v) {
			errs = append(errs, fmt.Errorf("%s must be an absolute path (got %q)", p.name, p.v))
		}
	}
	if set := countSet(c.S3Endpoint, c.S3Bucket, c.S3AccessKey, c.S3SecretKey); set != 0 && set != 4 {
		errs = append(errs, errors.New("S3_ENDPOINT, S3_BUCKET, S3_ACCESS_KEY and S3_SECRET_KEY must be set together or not at all"))
	}
	if !versionRe.MatchString(c.Version) {
		errs = append(errs, errors.New("LAUNCHPAD_VERSION must be 1 to 64 letters, digits and . _ + -"))
	}
	dev := c.LogEnv == "local" || c.LogEnv == "test"
	var kerrs []error
	c.BackupKey, kerrs = loadBackupKey(getenv("BACKUP_ENCRYPTION_KEY"), dev, nil)
	errs = append(errs, kerrs...)
	if c.BackupKey == nil && len(kerrs) == 0 {
		errs = append(errs, errors.New("BACKUP_ENCRYPTION_KEY is required by offsite (64 hex characters, e.g. `openssl rand -hex 32`)"))
	}
	if _, err := netguard.ParseCIDRs(c.AllowPrivateCIDRs); err != nil {
		errs = append(errs, fmt.Errorf("BACKUP_ALLOW_PRIVATE_CIDRS: %w", err))
	}
	if err := errors.Join(errs...); err != nil {
		return Offsite{}, err
	}
	return c, nil
}

func countSet(v ...string) int {
	n := 0
	for _, s := range v {
		if s != "" {
			n++
		}
	}
	return n
}
