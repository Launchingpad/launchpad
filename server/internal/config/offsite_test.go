package config

import (
	"strings"
	"testing"
)

func offsiteEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadOffsite(t *testing.T) {
	good := strings.Repeat("ab", 32)
	base := func() map[string]string {
		return map[string]string{"DATABASE_URL": "postgres://x", "LOG_ENV": "production", "BACKUP_ENCRYPTION_KEY": good}
	}
	// No AUTH_OTP_PEPPER, no S3_*: the worker starts without them.
	c, err := LoadOffsite(offsiteEnv(base()))
	if err != nil {
		t.Fatal(err)
	}
	if c.DumpDir != "/backups" || c.WorkDir != "/work" || c.HealthFile != "/tmp/offsite.alive" || c.Version != "unversioned" || len(c.BackupKey) != 32 {
		t.Fatalf("defaults: %+v", c)
	}
	for name, mut := range map[string]func(map[string]string){
		"no key":          func(m map[string]string) { delete(m, "BACKUP_ENCRYPTION_KEY") },
		"dev key in prod": func(m map[string]string) { m["BACKUP_ENCRYPTION_KEY"] = LocalDevBackupKey },
		"no db":           func(m map[string]string) { delete(m, "DATABASE_URL") },
		"bad cidr":        func(m map[string]string) { m["BACKUP_ALLOW_PRIVATE_CIDRS"] = "nonsense" },
		"relative dir":    func(m map[string]string) { m["OFFSITE_WORK_DIR"] = "work" },
		"bad version":     func(m map[string]string) { m["LAUNCHPAD_VERSION"] = "a b" },
		"bad log env":     func(m map[string]string) { m["LOG_ENV"] = "x" },
	} {
		m := base()
		mut(m)
		if _, err := LoadOffsite(offsiteEnv(m)); err == nil {
			t.Errorf("%s: no error", name)
		} else if strings.Contains(err.Error(), good) {
			t.Errorf("%s: the error holds the key", name)
		}
	}
	m := base()
	m["LOG_ENV"], m["BACKUP_ENCRYPTION_KEY"] = "local", LocalDevBackupKey
	if _, err := LoadOffsite(offsiteEnv(m)); err != nil {
		t.Fatalf("dev key on local: %v", err)
	}
}

// The offsite worker's own silo access (ticket 14) is all four variables or none.
func TestLoadOffsite_SiloAccess(t *testing.T) {
	m := map[string]string{"DATABASE_URL": "postgres://x", "LOG_ENV": "production", "BACKUP_ENCRYPTION_KEY": strings.Repeat("ab", 32)}
	c, err := LoadOffsite(offsiteEnv(m))
	if err != nil || c.S3Endpoint != "" {
		t.Fatalf("none: %v %+v", err, c)
	}
	all := map[string]string{"S3_ENDPOINT": "http://silo:9000", "S3_BUCKET": "b", "S3_ACCESS_KEY": "ro-key", "S3_SECRET_KEY": "ro-secret-value", "S3_REGION": "eu-west-1"}
	for k, v := range all {
		m[k] = v
	}
	c, err = LoadOffsite(offsiteEnv(m))
	if err != nil || c.S3Endpoint != "http://silo:9000" || c.S3Bucket != "b" || c.S3AccessKey != "ro-key" || c.S3SecretKey != "ro-secret-value" || c.S3Region != "eu-west-1" {
		t.Fatalf("all: %v %+v", err, c)
	}
	for k := range all {
		if k == "S3_REGION" {
			continue
		}
		p := map[string]string{}
		for kk, vv := range m {
			p[kk] = vv
		}
		delete(p, k)
		_, err := LoadOffsite(offsiteEnv(p))
		if err == nil {
			t.Errorf("without %s: no error", k)
		} else if strings.Contains(err.Error(), "ro-secret-value") || strings.Contains(err.Error(), "ro-key") {
			t.Errorf("without %s: the error holds a credential", k)
		}
	}
}
