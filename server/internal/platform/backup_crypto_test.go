package platform

import (
	"bytes"
	"errors"
	"testing"
)

func mustBackupKeyring(t *testing.T, k []byte) *BackupKeyring {
	t.Helper()
	kr, err := NewBackupKeyring(k)
	if err != nil {
		t.Fatal(err)
	}
	return kr
}

func mustSeal(t *testing.T, kr *BackupKeyring, col, owner string, pt []byte) []byte {
	t.Helper()
	b, err := kr.Seal(col, owner, pt)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBackupKeyring_RoundTripAndBinding(t *testing.T) {
	kr := mustBackupKeyring(t, key(1))
	pt := []byte("planted-s3-secret-91fe")
	for _, f := range []struct{ col, owner string }{
		{BackupColDestinationSecrets, "dest-1"},
		{BackupColRunTestSecrets, "run-1"},
		{BackupColSigningKey, BackupSettingsOwner},
	} {
		blob := mustSeal(t, kr, f.col, f.owner, pt)
		if bytes.Contains(blob, pt) || len(blob) != len(pt)+28 {
			t.Fatalf("%s: blob holds the plaintext or has length %d", f.col, len(blob))
		}
		if bytes.Equal(blob, mustSeal(t, kr, f.col, f.owner, pt)) {
			t.Fatalf("%s: two seals are equal: the nonce is not random", f.col)
		}
		got, err := kr.Open(f.col, f.owner, blob)
		if err != nil || !bytes.Equal(got, pt) {
			t.Fatalf("%s: open = %q, %v", f.col, got, err)
		}
		// A restart (a second keyring from the same key) opens it.
		if got, err := mustBackupKeyring(t, key(1)).Open(f.col, f.owner, blob); err != nil || !bytes.Equal(got, pt) {
			t.Fatalf("%s: restart open = %q, %v", f.col, got, err)
		}
	}
}

func TestBackupKeyring_BlobDoesNotOpenElsewhere(t *testing.T) {
	kr := mustBackupKeyring(t, key(1))
	dest := mustSeal(t, kr, BackupColDestinationSecrets, "dest-1", []byte("v"))
	run := mustSeal(t, kr, BackupColRunTestSecrets, "run-1", []byte("v"))
	sig := mustSeal(t, kr, BackupColSigningKey, BackupSettingsOwner, []byte("v"))
	changed := append([]byte(nil), dest...)
	changed[len(changed)-1] ^= 1
	for name, f := range map[string]func() error{
		"other destination":      func() error { _, e := kr.Open(BackupColDestinationSecrets, "dest-2", dest); return e },
		"destination as run":     func() error { _, e := kr.Open(BackupColRunTestSecrets, "dest-1", dest); return e },
		"run as destination":     func() error { _, e := kr.Open(BackupColDestinationSecrets, "run-1", run); return e },
		"other run":              func() error { _, e := kr.Open(BackupColRunTestSecrets, "run-2", run); return e },
		"signing as destination": func() error { _, e := kr.Open(BackupColDestinationSecrets, BackupSettingsOwner, sig); return e },
		"signing wrong owner":    func() error { _, e := kr.Open(BackupColSigningKey, "dest-1", sig); return e },
		"destination as signing": func() error { _, e := kr.Open(BackupColSigningKey, BackupSettingsOwner, dest); return e },
		"other key": func() error {
			_, e := mustBackupKeyring(t, key(2)).Open(BackupColDestinationSecrets, "dest-1", dest)
			return e
		},
		"changed blob":   func() error { _, e := kr.Open(BackupColDestinationSecrets, "dest-1", changed); return e },
		"empty blob":     func() error { _, e := kr.Open(BackupColDestinationSecrets, "dest-1", nil); return e },
		"unknown column": func() error { _, e := kr.Open("backup_runs.error_text", "run-1", run); return e },
		"empty owner":    func() error { _, e := kr.Open(BackupColDestinationSecrets, "", dest); return e },
	} {
		if err := f(); !errors.Is(err, ErrUnreadable) {
			t.Errorf("%s: err = %v, want ErrUnreadable", name, err)
		}
	}
	// A misuse on the way in is an error, never a seal under a loose binding.
	for name, f := range map[string]func() error{
		"unknown column":        func() error { _, e := kr.Seal("x", "y", []byte("v")); return e },
		"empty owner":           func() error { _, e := kr.Seal(BackupColDestinationSecrets, "", []byte("v")); return e },
		"settings as dest id":   func() error { _, e := kr.Seal(BackupColDestinationSecrets, BackupSettingsOwner, []byte("v")); return e },
		"signing, other owner":  func() error { _, e := kr.Seal(BackupColSigningKey, "dest-1", []byte("v")); return e },
		"platform column given": func() error { _, e := kr.Seal("platform_settings.postmark_token_enc", "x", []byte("v")); return e },
	} {
		if err := f(); err == nil {
			t.Errorf("seal %s: want an error", name)
		}
	}
}

// The backup scope is independent of the platform and git scopes: the same
// master key does not open a blob across scopes.
func TestBackupKeyring_IndependentOfOtherScopes(t *testing.T) {
	b, p, g := mustBackupKeyring(t, key(1)), mustKeyring(t, key(1)), mustGitKeyring(t, key(1))
	blob := mustSeal(t, b, BackupColDestinationSecrets, "dest-1", []byte("v"))
	if _, err := p.Open(BackupColDestinationSecrets, blob); err == nil {
		t.Fatal("a backup blob opened as a platform blob")
	}
	if _, err := g.OpenFor(BackupColDestinationSecrets, "dest-1", blob); err == nil {
		t.Fatal("a backup blob opened as a git blob")
	}
	if _, err := b.Open("postmark_token_enc", "x", p.Seal("postmark_token_enc", []byte("v"))); err == nil {
		t.Fatal("a platform blob opened as a backup blob")
	}
	if bytes.Equal(b.KeyCheck(), p.KeyCheck()) {
		t.Fatal("the backup key check equals the platform one for the same master key")
	}
}

func TestBackupKeyring_KeyCheck(t *testing.T) {
	kr := mustBackupKeyring(t, key(1))
	if len(kr.KeyCheck()) != 32 || !kr.Matches(kr.KeyCheck()) || kr.Matches(mustBackupKeyring(t, key(2)).KeyCheck()) || kr.Matches(nil) {
		t.Fatal("key check")
	}
	if kr.BackupKeyMismatch(nil) || kr.BackupKeyMismatch(kr.KeyCheck()) || !kr.BackupKeyMismatch(mustBackupKeyring(t, key(2)).KeyCheck()) {
		t.Fatal("mismatch flag")
	}
	kc := kr.KeyCheck()
	kc[0] ^= 1
	if !kr.Matches(kr.KeyCheck()) {
		t.Fatal("KeyCheck returned the internal slice")
	}
	if _, err := NewBackupKeyring(make([]byte, 31)); err == nil {
		t.Fatal("a short key was accepted")
	}
}
