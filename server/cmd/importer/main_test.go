package main

import (
	"errors"
	"fmt"
	"testing"

	"devops/internal/importer"
)

func TestRun_usageAndConfigErrors(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "")
	t.Setenv("DATABASE_URL", "")
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"no command", nil, exitUsage},
		{"unknown command", []string{"sync"}, exitUsage},
		{"unknown flag", []string{"import", "--nope"}, exitUsage},
		{"import needs a user map", []string{"import", "--in", t.TempDir(), "--org", "o"}, exitUsage},
		{"import needs an org", []string{"import", "--in", "x", "--user-map", "m.json"}, exitUsage},
		{"import needs an export dir", []string{"import", "--org", "o", "--user-map", "m.json"}, exitUsage},
		{"export needs --out", []string{"export"}, exitUsage},
		{"import needs DATABASE_URL", []string{"import", "--in", "x", "--org", "o", "--user-map", "m.json"}, exitUsage},
		{"a bad counter floor", []string{"import", "--counter-floor", "ORB"}, exitUsage},
		{"export needs the key", []string{"export", "--out", t.TempDir()}, exitError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := run(t.Context(), tt.args); got != tt.want {
				t.Errorf("run(%v) = %d, want %d", tt.args, got, tt.want)
			}
		})
	}
}

func TestRun_importNeedsAStoreOrSkipFiles(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://nowhere.invalid/x")
	for _, v := range []string{"S3_ENDPOINT", "S3_BUCKET", "S3_ACCESS_KEY", "S3_SECRET_KEY"} {
		t.Setenv(v, "")
	}
	if got := run(t.Context(), []string{"import", "--in", "x", "--org", "o", "--user-map", "m.json"}); got != exitUsage {
		t.Errorf("import with no S3_* = %d, want %d", got, exitUsage)
	}
	t.Setenv("S3_ENDPOINT", "http://silo:9000")
	if got := run(t.Context(), []string{"import", "--in", "x", "--org", "o", "--user-map", "m.json"}); got != exitUsage {
		t.Errorf("import with a partial S3_* = %d, want %d", got, exitUsage)
	}
}

func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"ok", nil, 0},
		{"usage", usageError{"x"}, exitUsage},
		{"a mismatch, wrapped", fmt.Errorf("import: %w", importer.ErrMismatch), exitMismatch},
		{"anything else", errors.New("boom"), exitError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exitCode(tt.err); got != tt.want {
				t.Errorf("exitCode = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFloors(t *testing.T) {
	f := floors{}
	if err := f.Set("orb=3405"); err != nil || f["ORB"] != 3405 {
		t.Errorf("Set: %v %v", err, f)
	}
	for _, bad := range []string{"ORB", "=3", "ORB=0", "ORB=x", "ORB=99999999999"} {
		if err := (floors{}).Set(bad); err == nil {
			t.Errorf("Set(%q): want an error", bad)
		}
	}
}
