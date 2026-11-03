package platform

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func mustKeyring(t *testing.T, k []byte) *Keyring {
	t.Helper()
	kr, err := NewKeyring(k)
	if err != nil {
		t.Fatal(err)
	}
	return kr
}

func TestKeyring_RoundTrip(t *testing.T) {
	kr := mustKeyring(t, key(1))
	pt := []byte("planted-postmark-token-3c1e")
	a, b := kr.Seal(colPostmarkToken, pt), kr.Seal(colPostmarkToken, pt)
	if bytes.Equal(a, b) {
		t.Fatal("two seals of one value are equal: the nonce is not random")
	}
	if bytes.Contains(a, pt) {
		t.Fatal("the blob holds the plaintext")
	}
	if len(a) != len(pt)+28 {
		t.Fatalf("blob len %d, want %d (12-byte nonce + 16-byte tag)", len(a), len(pt)+28)
	}
	for _, blob := range [][]byte{a, b} {
		got, err := kr.Open(colPostmarkToken, blob)
		if err != nil || !bytes.Equal(got, pt) {
			t.Fatalf("open = %q, %v", got, err)
		}
	}
	// A second keyring from the same key opens it (a restart).
	if got, err := mustKeyring(t, key(1)).Open(colPostmarkToken, a); err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("restart open = %q, %v", got, err)
	}
}

func TestKeyring_WrongKeyAndKeyCheck(t *testing.T) {
	a, b := mustKeyring(t, key(1)), mustKeyring(t, key(2))
	blob := a.Seal(colSMTPPassword, []byte("pw"))
	if _, err := b.Open(colSMTPPassword, blob); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("wrong key opened the blob: %v", err)
	}
	if !a.Matches(a.KeyCheck()) || b.Matches(a.KeyCheck()) || a.Matches(nil) || a.Matches([]byte("short")) {
		t.Fatal("Matches is wrong")
	}
	if len(a.KeyCheck()) != 32 {
		t.Fatalf("key check len %d (the column CHECK wants 32)", len(a.KeyCheck()))
	}
	// The key check is not the sealing key and not the master key.
	if bytes.Equal(a.KeyCheck(), key(1)) {
		t.Fatal("the key check is the master key")
	}
	if _, err := NewKeyring(make([]byte, 16)); err == nil {
		t.Fatal("a 16-byte key was accepted")
	}
}

func TestKeyring_TamperAndColumnBinding(t *testing.T) {
	kr := mustKeyring(t, key(1))
	blob := kr.Seal(colPostmarkToken, []byte("token"))
	for i := range blob {
		bad := append([]byte(nil), blob...)
		bad[i] ^= 0x01
		if _, err := kr.Open(colPostmarkToken, bad); !errors.Is(err, ErrUnreadable) {
			t.Fatalf("a flipped byte %d opened", i)
		}
	}
	if _, err := kr.Open(colPostmarkToken, blob[:len(blob)-1]); !errors.Is(err, ErrUnreadable) {
		t.Fatal("a truncated blob opened")
	}
	if _, err := kr.Open(colSMTPPassword, blob); !errors.Is(err, ErrUnreadable) {
		t.Fatal("a token blob opened as the password: the column is not bound")
	}
}

// TestSecret_Redacted: no ordinary way of printing a Secret shows it.
func TestSecret_Redacted(t *testing.T) {
	const v = "planted-secret-value-8d2b"
	s := Secret(v)
	in := SettingsInput{}
	in.PostmarkToken.Set, in.PostmarkToken.V = true, s
	var logs bytes.Buffer
	slog.New(slog.NewTextHandler(&logs, nil)).Info("x", "s", s, "in", in)
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("x", "s", s, "in", in)
	js, _ := json.Marshal(struct{ S Secret }{s})
	for _, out := range []string{
		fmt.Sprint(s), fmt.Sprintf("%s|%v|%+v|%#v|%q|%x|%X", s, s, s, s, s, s, s),
		fmt.Sprintf("%v|%+v|%#v", in, in, in), logs.String(), string(js),
	} {
		if strings.Contains(out, v) || strings.Contains(out, fmt.Sprintf("%x", v)) {
			t.Fatalf("the secret shows: %s", out)
		}
	}
	if s.Reveal() != v {
		t.Fatal("Reveal")
	}
}
