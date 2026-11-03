// Package storagetest gives integration tests a real S3 store: the compose
// silo. Like internal/testdb it is gated by INTEGRATION=1, and under it a
// missing silo FAILS the test instead of skipping it, so a green integration
// run always exercised the store.
//
// TEST_S3_ENDPOINT defaults to the compose silo as seen from the host
// (http://localhost:9100). The keys default to the public local-dev values in
// docker-compose.yml; TEST_S3_ACCESS_KEY / TEST_S3_SECRET_KEY override them.
// Every test shares the bucket TEST_S3_BUCKET (default devops-test); keys are
// unique per test because each test database has its own org id.
package storagetest

import (
	"context"
	"os"
	"testing"
	"time"

	"devops/internal/storage"
)

// DefaultEndpoint is the compose silo as seen from the host.
const DefaultEndpoint = "http://localhost:9100"

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// Config is the test store's configuration.
func Config() storage.Config {
	return storage.Config{
		Endpoint:  env("TEST_S3_ENDPOINT", DefaultEndpoint),
		Region:    "us-east-1",
		Bucket:    env("TEST_S3_BUCKET", "devops-test"),
		AccessKey: env("TEST_S3_ACCESS_KEY", "devops"),
		SecretKey: env("TEST_S3_SECRET_KEY", "devops-local-s3-secret"),
	}
}

// New returns the test store with its bucket created. It skips t unless
// INTEGRATION=1.
func New(t testing.TB) *storage.S3 {
	t.Helper()
	if os.Getenv("INTEGRATION") != "1" {
		t.Skip("requires INTEGRATION=1 and a running silo (TEST_S3_ENDPOINT)")
	}
	cfg := Config()
	s, err := storage.New(cfg)
	if err != nil {
		t.Fatalf("storage config: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.EnsureBucket(ctx); err != nil {
		t.Fatalf("ensure bucket (is silo up? TEST_S3_ENDPOINT=%s): %v", cfg.Endpoint, err)
	}
	return s
}
