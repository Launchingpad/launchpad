package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"devops/internal/config"
	"devops/internal/move"
	"devops/internal/testdb"
)

// The move commands against two real databases: export prints the passphrase
// alone on stdout and a 0600 file; import with that passphrase loads it; a
// wrong passphrase and a second import are refused with exit 1.
func TestMoveCommands_RoundTrip(t *testing.T) {
	src, _ := testdb.New(t)
	testdb.Fixture(t, src)
	dst, _ := testdb.New(t)
	ctx := context.Background()
	settings, backup := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	keys, err := moveKeys(config.Config{SettingsKey: settings, BackupKey: backup})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "install.bundle")

	var so, se bytes.Buffer
	env := &moveEnv{pool: src, keys: keys}
	if rc := runMoveExport(ctx, env, moveExportArgs{path: path, user: "Chen", noAttachments: true}, &so, &se); rc != 0 {
		t.Fatalf("export exit %d: %s", rc, se.String())
	}
	pass := strings.TrimSuffix(so.String(), "\n")
	if strings.Count(so.String(), "\n") != 1 || len(pass) < 32 || strings.Contains(se.String(), pass) {
		t.Fatalf("stdout %q is not the passphrase alone (or stderr leaks it)", so.String())
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v %v", st, err)
	}
	// An existing file is never overwritten.
	if rc := runMoveExport(ctx, env, moveExportArgs{path: path, noAttachments: true}, &so, &se); rc != 1 {
		t.Fatalf("overwrite exit %d", rc)
	}
	// Without a store, a full export is refused and leaves no file.
	missing := filepath.Join(t.TempDir(), "none.bundle")
	if rc := runMoveExport(ctx, env, moveExportArgs{path: missing}, &so, &se); rc != 1 {
		t.Fatalf("no-store exit %d", rc)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("a failed export left a file")
	}

	denv := &moveEnv{pool: dst, keys: keys}
	se.Reset()
	if rc := runMoveImport(ctx, denv, path, "AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA", &se); rc != 1 || !strings.Contains(se.String(), move.ErrBadPassphrase.Error()) {
		t.Fatalf("wrong passphrase: exit %d: %s", rc, se.String())
	}
	se.Reset()
	if rc := runMoveImport(ctx, denv, path, pass, &se); rc != 0 {
		t.Fatalf("import exit %d: %s", rc, se.String())
	}
	if strings.Contains(se.String(), pass) {
		t.Fatal("stderr holds the passphrase")
	}
	var users int
	if err := dst.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&users); err != nil || users != 2 {
		t.Fatalf("users: %d %v", users, err)
	}
	se.Reset()
	if rc := runMoveImport(ctx, denv, path, pass, &se); rc != 1 || !strings.Contains(se.String(), "not empty") {
		t.Fatalf("second import: exit %d: %s", rc, se.String())
	}
}
