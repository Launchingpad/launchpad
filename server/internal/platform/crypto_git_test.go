package platform

import (
	"bytes"
	"errors"
	"testing"
)

func mustGitKeyring(t *testing.T, k []byte) *GitKeyring {
	t.Helper()
	kr, err := NewGitKeyring(k)
	if err != nil {
		t.Fatal(err)
	}
	return kr
}

func TestGitKeyring_RoundTripAndBinding(t *testing.T) {
	kr := mustGitKeyring(t, key(1))
	pt := []byte("planted-webhook-secret-77aa")
	blob := kr.SealFor("webhook_secret_enc", "row-1", pt)
	if bytes.Contains(blob, pt) || len(blob) != len(pt)+28 {
		t.Fatalf("blob holds the plaintext or has the wrong length %d", len(blob))
	}
	if bytes.Equal(blob, kr.SealFor("webhook_secret_enc", "row-1", pt)) {
		t.Fatal("two seals are equal: the nonce is not random")
	}
	got, err := kr.OpenFor("webhook_secret_enc", "row-1", blob)
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("open = %q, %v", got, err)
	}
	// A restart (a second keyring from the same key) opens it.
	if got, err := mustGitKeyring(t, key(1)).OpenFor("webhook_secret_enc", "row-1", blob); err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("restart open = %q, %v", got, err)
	}
	// Another row, another column, another key, a changed blob: unreadable.
	for name, f := range map[string]func() error{
		"other row":    func() error { _, e := kr.OpenFor("webhook_secret_enc", "row-2", blob); return e },
		"other column": func() error { _, e := kr.OpenFor("webhook_secret_next_enc", "row-1", blob); return e },
		"other key":    func() error { _, e := mustGitKeyring(t, key(2)).OpenFor("webhook_secret_enc", "row-1", blob); return e },
		"changed blob": func() error {
			b := append([]byte(nil), blob...)
			b[len(b)-1] ^= 1
			_, e := kr.OpenFor("webhook_secret_enc", "row-1", b)
			return e
		},
		"empty blob": func() error { _, e := kr.OpenFor("webhook_secret_enc", "row-1", nil); return e },
	} {
		if err := f(); !errors.Is(err, ErrUnreadable) {
			t.Errorf("%s: err = %v, want ErrUnreadable", name, err)
		}
	}
}

// The two scopes use different keys: a platform_settings blob never opens as
// a git blob (and the other way round), even with the same master key.
func TestGitKeyring_IndependentOfPlatformKeyring(t *testing.T) {
	p, g := mustKeyring(t, key(1)), mustGitKeyring(t, key(1))
	if _, err := g.OpenFor("postmark_token_enc", "x", p.Seal("postmark_token_enc", []byte("v"))); err == nil {
		t.Fatal("a platform blob opened as a git blob")
	}
	if _, err := p.Open("git_integrations.token_enc.x", g.SealFor("token_enc", "x", []byte("v"))); err == nil {
		t.Fatal("a git blob opened as a platform blob")
	}
	if _, err := NewGitKeyring(make([]byte, 31)); err == nil {
		t.Fatal("a short master key was accepted")
	}
}

// The info strings pin the stored data: the existing two never change, the git
// one is new in LAUN-5.
func TestInfoStringsPinned(t *testing.T) {
	if infoSeal != "launchpad platform_settings secret sealing v1" ||
		infoCheck != "launchpad platform_settings key check v1" ||
		infoGitSeal != "launchpad git_integrations sealing v1" {
		t.Fatal("an HKDF info string changed: every stored secret would become unreadable")
	}
}
