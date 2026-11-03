package signup

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/dbq"
	"devops/internal/httpx"
)

func TestInviteSecret_ShapeAndHash(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		s, err := newInviteSecret()
		if err != nil {
			t.Fatal(err)
		}
		if len(s) != 43 || !inviteWellFormed(s) || seen[s] {
			t.Fatalf("secret %q: len %d, well formed %v, repeat %v", s, len(s), inviteWellFormed(s), seen[s])
		}
		seen[s] = true
		if h := hashInvite(s); len(h) != 32 || bytes.Contains(h, []byte(s)) {
			t.Fatalf("hash of %q is not a 32-byte digest", s)
		}
	}
	for _, bad := range []string{"", "short", strings.Repeat("A", 42), strings.Repeat("A", 44), strings.Repeat("!", 43)} {
		if inviteWellFormed(bad) {
			t.Fatalf("%q is well formed", bad)
		}
	}
}

func TestInviteLink(t *testing.T) {
	cases := []struct{ name, base, want string }{
		{"no base", "", "/invite#tok"},
		{"base", "https://launchpad.example", "https://launchpad.example/invite#tok"},
		{"trailing slash", "https://launchpad.example/", "https://launchpad.example/invite#tok"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := InviteLink(c.base, "tok"); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestInviteStatus(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	exp := now.Add(time.Hour)
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	admin := uuid.New()
	cases := []struct {
		name string
		row  dbq.OrgInvite
		want string
	}{
		{"open", dbq.OrgInvite{ExpiresAt: exp}, "pending"},
		{"past expiry", dbq.OrgInvite{ExpiresAt: now.Add(-time.Second)}, "expired"},
		{"at expiry", dbq.OrgInvite{ExpiresAt: now}, "expired"},
		{"accepted", dbq.OrgInvite{ExpiresAt: exp, AcceptedAt: at(-time.Minute)}, "accepted"},
		{"accepted, later expired", dbq.OrgInvite{ExpiresAt: now.Add(-time.Second), AcceptedAt: at(-time.Hour)}, "accepted"},
		{"revoked before expiry", dbq.OrgInvite{ExpiresAt: exp, RevokedAt: at(-time.Minute)}, "revoked"},
		{"closed by a new invite after expiry", dbq.OrgInvite{ExpiresAt: now.Add(-time.Hour), RevokedAt: at(-time.Minute)}, "expired"},
		{"revoked by an admin after expiry", dbq.OrgInvite{ExpiresAt: now.Add(-time.Hour), RevokedAt: at(-time.Minute), RevokedBy: &admin}, "revoked"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := inviteStatus(c.row, now); got != c.want {
				t.Fatalf("got %s, want %s", got, c.want)
			}
		})
	}
}

func TestValidateCreate(t *testing.T) {
	ok := CreateRequest{Email: " Ann@Example.COM ", Name: " Ann ", Username: "Ann", Note: " hi\nthere "}
	got, err := validateCreate(ok)
	if err != nil || got.Email != "ann@example.com" || got.Name != "Ann" || got.Username != "ann" || got.Note != "hi\nthere" {
		t.Fatalf("got %+v, %v", got, err)
	}
	bad := []struct {
		name string
		mod  func(*CreateRequest)
		want string
	}{
		{"bad email", func(r *CreateRequest) { r.Email = "not-an-email" }, "email"},
		{"display name in email", func(r *CreateRequest) { r.Email = "Ann <ann@example.com>" }, "email"},
		{"bad username", func(r *CreateRequest) { r.Username = "1abc" }, "username"},
		{"no name", func(r *CreateRequest) { r.Name = "  " }, "name"},
		{"long note", func(r *CreateRequest) { r.Note = strings.Repeat("x", MaxNoteLen+1) }, "note"},
		{"control character in note", func(r *CreateRequest) { r.Note = "a\x00b" }, "note"},
		{"escape in note", func(r *CreateRequest) { r.Note = "a\x1b[31mb" }, "note"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			in := ok
			c.mod(&in)
			_, err := validateCreate(in)
			var he *httpx.Error
			if !errors.As(err, &he) || he.Status != 400 || !strings.Contains(he.Msg, c.want) {
				t.Fatalf("err %v, want a 400 naming %s", err, c.want)
			}
		})
	}
	// A note of exactly the limit, in multi-byte characters, is fine.
	in := ok
	in.Note = strings.Repeat("é", MaxNoteLen)
	if _, err := validateCreate(in); err != nil {
		t.Fatalf("a %d-character note: %v", MaxNoteLen, err)
	}
}

func TestParseLimit(t *testing.T) {
	cases := []struct {
		raw  string
		want int32
		ok   bool
	}{
		{"", 100, true}, {"1", 1, true}, {"500", 500, true},
		{"0", 0, false}, {"501", 0, false}, {"-1", 0, false}, {"5x", 0, false}, {" 5", 0, false}, {"05", 0, false},
	}
	for _, c := range cases {
		t.Run("limit="+c.raw, func(t *testing.T) {
			got, err := ParseLimit(c.raw)
			if (err == nil) != c.ok || got != c.want {
				t.Fatalf("got %d, %v", got, err)
			}
		})
	}
}

// TestMailTexts: the secret is in the body only, never in the subject (the
// dev log mailer writes the subject to the log).
func TestMailTexts(t *testing.T) {
	c := confirmMail("123456")
	if !strings.Contains(c.text, "123456") || strings.Contains(c.subject, "123456") {
		t.Fatalf("confirm mail %+v", c)
	}
	link := "https://launchpad.example/invite#SECRETSECRET"
	i := inviteMail("Chen", "Orbit\r\nBcc: x@evil.example", link, time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC))
	if !strings.Contains(i.text, link) || strings.Contains(i.subject, "SECRET") {
		t.Fatalf("invite mail %+v", i)
	}
	if strings.ContainsAny(i.subject, "\r\n") {
		t.Fatalf("subject has a line break: %q", i.subject)
	}
	if x := existingAccountMail("https://launchpad.example"); !strings.Contains(x.text, "https://launchpad.example/login") || x.subject == confirmMail("1").subject {
		t.Fatalf("existing-account mail %+v", x)
	}
	a := approvedMail("https://launchpad.example/", "Orbit")
	if !strings.Contains(a.text, "https://launchpad.example/login") {
		t.Fatalf("approved mail %+v", a)
	}
	if b := approvedMail("", "Orbit"); !strings.Contains(b.text, "the Launchpad web app") {
		t.Fatalf("approved mail with no base %+v", b)
	}
}

type fakeMailer struct {
	mu    sync.Mutex
	sent  []mailJob
	ready bool
	fail  error
}

func (f *fakeMailer) Ready() bool { return f.ready }

func (f *fakeMailer) SendText(_ context.Context, to, subject, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, mailJob{to, subject, text})
	return f.fail
}

// TestQueue: a full queue drops and logs without the address or the text;
// a failed send logs the error and the subject only; Run stops with ctx.
func TestQueue(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := &fakeMailer{ready: true, fail: errors.New("refused (reply code 550)")}
	q := newQueue(m, log, 1)
	q.enqueue(mailJob{to: "a@example.com", subject: "s", text: "SECRET-1"})
	q.enqueue(mailJob{to: "b@example.com", subject: "s", text: "SECRET-2"}) // full: dropped
	if !strings.Contains(buf.String(), "queue is full") {
		t.Fatalf("no drop line: %s", buf.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { q.run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		n := len(m.sent)
		m.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the queued email was not sent")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	for _, s := range []string{"SECRET-1", "SECRET-2", "a@example.com", "b@example.com"} {
		if strings.Contains(buf.String(), s) {
			t.Fatalf("the log holds %q:\n%s", s, buf.String())
		}
	}
	if !strings.Contains(buf.String(), "sign-up email failed") {
		t.Fatalf("no failure line: %s", buf.String())
	}
}

// TestRunWithoutMailer returns at once and send is a no-op.
func TestRunWithoutMailer(t *testing.T) {
	s := New(nil, slog.New(slog.DiscardHandler), Options{})
	done := make(chan struct{})
	go func() { s.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run without a mailer did not return")
	}
	s.send(context.Background(), "a@example.com", confirmMail("1")) // must not panic
	if s.mailReady() {
		t.Fatal("mail is ready with no mailer")
	}
}

func TestOptionsDefaults(t *testing.T) {
	var o Options
	o.defaults()
	if o.PendingCap != 200 || o.PendingTTL != 30*24*time.Hour || o.InviteTTL != 7*24*time.Hour || o.CodeTTL != time.Hour ||
		o.MaxCodeAttempts != 5 || o.SignupPerEmailPerHour != 3 || o.Now == nil ||
		o.UnconfirmedCap != 1000 || o.UnconfirmedTTL != 24*time.Hour || o.MaxTotalCodeAttempts != 10 {
		t.Fatalf("defaults %+v", o)
	}
}
