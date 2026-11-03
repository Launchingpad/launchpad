package platform

import (
	"bytes"
	"errors"
	"testing"
)

// LAUN-197: a push blob opens only for its own column and row scope, and only
// with the push key (not the settings, git, Slack or backup key).
func TestPushKeyringBindsColumnAndScope(t *testing.T) {
	master := bytes.Repeat([]byte{7}, 32)
	k, err := NewPushKeyring(master)
	if err != nil {
		t.Fatal(err)
	}
	blob := k.SealFor(PushColRelaySecret, "org-1", []byte("s3cret"))
	if pt, err := k.OpenFor(PushColRelaySecret, "org-1", blob); err != nil || string(pt) != "s3cret" {
		t.Fatalf("round trip = %q, %v", pt, err)
	}
	for name, c := range map[string][2]string{
		"other scope":  {PushColRelaySecret, "org-2"},
		"other column": {PushColRelayURL, "org-1"},
	} {
		if _, err := k.OpenFor(c[0], c[1], blob); !errors.Is(err, ErrUnreadable) {
			t.Errorf("%s opened: %v", name, err)
		}
	}
	other, _ := NewPushKeyring(bytes.Repeat([]byte{8}, 32))
	if _, err := other.OpenFor(PushColRelaySecret, "org-1", blob); !errors.Is(err, ErrUnreadable) {
		t.Errorf("another master key opened it: %v", err)
	}
	if _, err := NewPushKeyring(master[:16]); err == nil {
		t.Error("a short key was accepted")
	}
}
