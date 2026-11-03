package notify_test

import (
	"context"
	"testing"

	"devops/internal/notify"
)

// LAUN-36: settings mail.available is false when PUBLIC_BASE_URL is empty (the sender would skip every mail),
// and true when the transport is ready and the base URL is set.
func TestSettingsMailAvailable_NeedsBaseURL(t *testing.T) {
	w := newWorld(t)
	ready := func() bool { return true }
	cases := []struct {
		name  string
		ready func() bool
		base  string
		want  bool
	}{
		{"ready, base url set", ready, "https://launchpad.example.com", true},
		{"ready, no base url", ready, "", false},
		{"not ready, base url set", func() bool { return false }, "https://launchpad.example.com", false},
		{"ready, base url is only a slash", ready, "/", false},
		{"ready, base url is only spaces", ready, "  ", false},
		{"no transport check", nil, "https://launchpad.example.com", false},
	}
	for _, c := range cases {
		s := notify.New(w.pool)
		s.SetMailReady(notify.MailReady(c.ready, c.base))
		v, err := s.View(context.Background(), w.org, w.user["chen"].UserID)
		if err != nil {
			t.Fatal(err)
		}
		if v.Mail.Available != c.want {
			t.Fatalf("%s: mail.available = %v, want %v", c.name, v.Mail.Available, c.want)
		}
	}
}
