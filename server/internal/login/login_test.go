package login

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNormalizeEmail(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"chen@example.com", "chen@example.com", true},
		{"  Chen@example.com\n", "chen@example.com", true},
		{"a.b+tag@sub.example.co.uk", "a.b+tag@sub.example.co.uk", true},
		{"", "", false},
		{"no-at-sign", "", false},
		{"two@@example.com", "", false},
		{"a@b@example.com", "", false},
		{"@example.com", "", false},
		{"a@", "", false},
		{"a@localhost", "", false},
		{"a@.example.com", "", false},
		{"Chen <chen@example.com>", "", false},
		{"a b@example.com", "", false},
		{"a@example.com (comment)", "", false},
		{strings.Repeat("a", 250) + "@b.co", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := NormalizeEmail(tt.in)
			if (err == nil) != tt.ok || got != tt.want {
				t.Fatalf("NormalizeEmail(%q) = %q, %v", tt.in, got, err)
			}
		})
	}
}

func TestCodes(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		c, err := newCode()
		if err != nil {
			t.Fatal(err)
		}
		if !ValidCodeFormat(c) {
			t.Fatalf("bad code %q", c)
		}
		seen[c] = true
	}
	if len(seen) < 190 {
		t.Fatalf("only %d distinct codes in 200", len(seen))
	}
	for _, s := range []string{"12345", "1234567", "12345a", "١٢٣٤٥٦", " 12345", ""} {
		if ValidCodeFormat(s) {
			t.Errorf("ValidCodeFormat(%q) = true", s)
		}
	}
	s1, s2 := newSalt(), newSalt()
	if len(s1) != saltLen || bytes.Equal(s1, s2) {
		t.Fatal("salts are not random 16-byte values")
	}
	pep, other := bytes.Repeat([]byte("p"), MinPepperLen), bytes.Repeat([]byte("q"), MinPepperLen)
	h := hashCode(pep, s1, "123456")
	if len(h) != 32 || !codeMatches(pep, s1, h, "123456") || codeMatches(pep, s1, h, "123457") || codeMatches(pep, s2, h, "123456") {
		t.Fatal("hash/compare wrong")
	}
	if bytes.Equal(hashCode(pep, s1, "123456"), hashCode(pep, s2, "123456")) {
		t.Fatal("the salt does not change the hash")
	}
	// The pepper is the secret: another pepper does not verify, and a DB
	// reader who knows salt and hash cannot brute-force the code with a
	// plain hash.
	if codeMatches(other, s1, h, "123456") {
		t.Fatal("the hash verifies without the right pepper")
	}
	plain := sha256.Sum256(append(append([]byte{}, s1...), "123456"...))
	if bytes.Equal(plain[:], h) {
		t.Fatal("the stored hash is a plain sha256(salt || code), which a DB reader can brute-force")
	}
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func TestWindowLimiter(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := newWindowLimiter(5, time.Hour, 3, clk.now)
	a, b, c, d := [32]byte{1}, [32]byte{2}, [32]byte{3}, [32]byte{4}
	for i := range 5 {
		if !l.Allow(a) {
			t.Fatalf("hit %d refused", i+1)
		}
		clk.t = clk.t.Add(time.Minute)
	}
	if l.Allow(a) {
		t.Fatal("sixth hit in the hour allowed")
	}
	if !l.Allow(b) {
		t.Fatal("another key was limited")
	}
	// A refused hit is not recorded: once the first hit is an hour old, one
	// more is allowed.
	clk.t = time.Unix(1_700_000_000, 0).Add(time.Hour + time.Second)
	if !l.Allow(a) {
		t.Fatal("window did not slide")
	}
	if l.Allow(a) {
		t.Fatal("window slid too far")
	}
	// Full table of live keys: fail closed, but a key already known still works.
	if !l.Allow(c) || l.Allow(d) {
		t.Fatal("maxKeys not enforced")
	}
	// After everything ages out, the table sweeps and accepts new keys.
	clk.t = clk.t.Add(2 * time.Hour)
	if !l.Allow(d) {
		t.Fatal("sweep did not free the table")
	}
}

func TestBucket(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	b := newBucket(60, 3, clk.now) // one a second, burst 3
	for i := range 3 {
		if !b.Allow() {
			t.Fatalf("burst %d refused", i)
		}
	}
	if b.Allow() {
		t.Fatal("over burst")
	}
	clk.t = clk.t.Add(time.Second)
	if !b.Allow() || b.Allow() {
		t.Fatal("refill is not one a second")
	}
	clk.t = clk.t.Add(time.Hour)
	for range 3 {
		if !b.Allow() {
			t.Fatal("did not refill to burst")
		}
	}
	if b.Allow() {
		t.Fatal("refilled past burst")
	}
}

func TestPostmarkMailer(t *testing.T) {
	var got struct {
		header http.Header
		body   map[string]string
	}
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.header = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got.body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"ErrorCode":300,"Message":"Invalid email request"}`))
	}))
	defer srv.Close()
	m := NewPostmarkMailer("server-token", "tracker@example.com", "")
	m.Endpoint = srv.URL
	if err := m.SendLoginCode(context.Background(), "chen@example.com", "042917"); err != nil {
		t.Fatal(err)
	}
	if got.header.Get("X-Postmark-Server-Token") != "server-token" || got.header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers %v", got.header)
	}
	if got.body["From"] != "tracker@example.com" || got.body["To"] != "chen@example.com" ||
		got.body["MessageStream"] != "outbound" || !strings.Contains(got.body["TextBody"], "042917") ||
		strings.Contains(got.body["Subject"], "042917") {
		t.Fatalf("body %v", got.body)
	}
	status = http.StatusUnprocessableEntity
	err := m.SendLoginCode(context.Background(), "chen@example.com", "042917")
	if err == nil || !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "300") ||
		strings.Contains(err.Error(), "042917") || strings.Contains(err.Error(), "server-token") {
		t.Fatalf("error %v", err)
	}
}

func TestDevLogMailerOnlyOnDevServers(t *testing.T) {
	if _, err := NewDevLogMailer(slog.New(slog.DiscardHandler), false); !errors.Is(err, ErrNotDevServer) {
		t.Fatalf("built outside a dev server: %v", err)
	}
	var buf bytes.Buffer
	m, err := NewDevLogMailer(slog.New(slog.NewTextHandler(&buf, nil)), true)
	if err != nil {
		t.Fatal(err)
	}
	_ = m.SendLoginCode(context.Background(), "a@b.co", "123456")
	if !strings.Contains(buf.String(), "123456") {
		t.Fatal("the dev mailer did not log the code")
	}
}

type recordMailer struct {
	mu    sync.Mutex
	sent  []string
	block chan struct{}
	fail  bool
}

func (r *recordMailer) SendLoginCode(ctx context.Context, to, code string) error {
	if r.block != nil {
		select {
		case <-r.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, to+":"+code)
	if r.fail {
		return errors.New("transport down")
	}
	return nil
}

func (r *recordMailer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sent)
}

func TestQueue(t *testing.T) {
	t.Run("delivers in the background and stops", func(t *testing.T) {
		m := &recordMailer{}
		var logs bytes.Buffer
		q := NewQueue(m, slog.New(slog.NewTextHandler(&logs, nil)), 4)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { q.Run(ctx); close(done) }()
		q.Deliver(context.Background(), "a@b.co", "111111")
		deadline := time.Now().Add(2 * time.Second)
		for m.count() == 0 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if m.count() != 1 {
			t.Fatal("not delivered")
		}
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not return after cancel")
		}
	})
	t.Run("a full queue drops without blocking", func(t *testing.T) {
		m := &recordMailer{}
		var logs bytes.Buffer
		q := NewQueue(m, slog.New(slog.NewTextHandler(&logs, nil)), 1)
		// No Run: the buffer of one fills, the next Deliver must return.
		returned := make(chan struct{})
		go func() {
			q.Deliver(context.Background(), "a@b.co", "111111")
			q.Deliver(context.Background(), "a@b.co", "222222")
			close(returned)
		}()
		select {
		case <-returned:
		case <-time.After(2 * time.Second):
			t.Fatal("Deliver blocked on a full queue")
		}
		if !strings.Contains(logs.String(), "queue is full") || strings.Contains(logs.String(), "222222") {
			t.Fatalf("drop log %q", logs.String())
		}
	})
	t.Run("a send error is logged without the code", func(t *testing.T) {
		m := &recordMailer{fail: true}
		var mu sync.Mutex
		var logs bytes.Buffer
		w := writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return logs.Write(p) })
		q := NewQueue(m, slog.New(slog.NewTextHandler(w, nil)), 1)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { q.Run(ctx); close(done) }()
		q.Deliver(context.Background(), "a@b.co", "333333")
		deadline := time.Now().Add(2 * time.Second)
		for {
			mu.Lock()
			s := logs.String()
			mu.Unlock()
			if strings.Contains(s, "email failed") {
				if strings.Contains(s, "333333") {
					t.Fatal("the code is in the error log")
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("no error logged")
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
		<-done
	})
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// With no transport the service refuses before touching the database (the
// nil pool would panic otherwise).
func TestServiceDisabled(t *testing.T) {
	s := New(nil, slog.New(slog.DiscardHandler), nil, Options{})
	if s.Enabled() {
		t.Fatal("enabled without a transport")
	}
	if err := s.RequestCode(context.Background(), "a@b.co"); err != errUnavailable {
		t.Fatalf("RequestCode: %v", err)
	}
	if _, err := s.Verify(context.Background(), "a@b.co", "123456", false); err != errUnavailable {
		t.Fatalf("Verify: %v", err)
	}
}

func TestPad(t *testing.T) {
	s := New(nil, slog.New(slog.DiscardHandler), nil, Options{Floor: 80 * time.Millisecond})
	start := time.Now()
	s.Pad(context.Background(), start)
	if el := time.Since(start); el < 80*time.Millisecond {
		t.Fatalf("Pad returned after %v", el)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start = time.Now()
	s.Pad(ctx, start)
	if el := time.Since(start); el > 50*time.Millisecond {
		t.Fatalf("Pad ignored a cancelled context (%v)", el)
	}
}

func TestRetryAfter(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	l := newWindowLimiter(2, time.Hour, 10, clk.now)
	k := [32]byte{9}
	l.Allow(k)
	clk.t = clk.t.Add(10 * time.Minute)
	l.Allow(k)
	ok, wait := l.Take(k)
	if ok || wait != 50*time.Minute {
		t.Fatalf("window: ok=%v wait=%v, want 50m (the first hit leaves then)", ok, wait)
	}
	b := newBucket(30, 1, clk.now) // one token every 2 s
	b.Allow()
	ok, wait = b.Take()
	if ok || wait != 2*time.Second {
		t.Fatalf("bucket: ok=%v wait=%v, want 2s", ok, wait)
	}
	for _, tt := range []struct {
		wait time.Duration
		want int
	}{{0, 1}, {300 * time.Millisecond, 1}, {time.Second, 1}, {1001 * time.Millisecond, 2}, {time.Hour, 3600}} {
		if got := (&RateLimitedError{Wait: tt.wait}).RetryAfterSeconds(); got != tt.want {
			t.Errorf("RetryAfterSeconds(%v) = %d, want %d", tt.wait, got, tt.want)
		}
	}
	var e error = &RateLimitedError{Wait: time.Second}
	if !errors.Is(e, errRateLimited) {
		t.Fatal("RateLimitedError does not unwrap to the rate_limited wire error")
	}
}

// Without a long enough pepper the service refuses to issue codes (fail
// closed), even with a transport.
func TestServiceNeedsAPepper(t *testing.T) {
	deliver := func(context.Context, string, string) {}
	for _, pep := range [][]byte{nil, bytes.Repeat([]byte("x"), MinPepperLen-1)} {
		if New(nil, slog.New(slog.DiscardHandler), deliver, Options{Pepper: pep}).Enabled() {
			t.Fatalf("enabled with a %d-byte pepper", len(pep))
		}
	}
	if !New(nil, slog.New(slog.DiscardHandler), deliver, Options{Pepper: bytes.Repeat([]byte("x"), MinPepperLen)}).Enabled() {
		t.Fatal("disabled with a good pepper")
	}
}

// TestPostmarkMailer_SafeErrors: every Postmark failure is a
// *MailError whose text holds no token and no Postmark message text.
func TestPostmarkMailer_SafeErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ErrorCode":10,"Message":"Bad token pm-planted-token"}`))
	}))
	defer srv.Close()
	m := NewPostmarkMailer("pm-planted-token", "tracker@example.com", "")
	m.Endpoint = srv.URL
	err := m.SendText(context.Background(), "chen@example.com", "s", "t")
	var me *MailError
	if !errors.As(err, &me) || err.Error() != "postmark status 401, error code 10" {
		t.Fatalf("err = %v", err)
	}
	srv.Close() // now unreachable
	err = m.SendText(context.Background(), "chen@example.com", "s", "t")
	if !errors.As(err, &me) || err.Error() != "could not reach Postmark" || strings.Contains(PublicMailError(err), "pm-planted-token") {
		t.Fatalf("err = %v", err)
	}
	if PublicMailError(errors.New("raw detail pm-planted-token")) == "raw detail pm-planted-token" {
		t.Fatal("PublicMailError passed a foreign error through")
	}
}

func TestDevLogMailer_SendText(t *testing.T) {
	var buf bytes.Buffer
	m, err := NewDevLogMailer(slog.New(slog.NewTextHandler(&buf, nil)), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SendText(context.Background(), "a@b.co", "Launchpad test email", "body-not-logged"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "Launchpad test email") || strings.Contains(buf.String(), "body-not-logged") {
		t.Fatalf("log %q", buf.String())
	}
}

// TestKeyLimit: limit hits per key per window, keys apart, a refusal
// carries the wait, and the window frees on time.
func TestKeyLimit(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	l := NewKeyLimit(2, time.Hour, 10, clock)
	for i := range 2 {
		if err := l.Take("a"); err != nil {
			t.Fatalf("hit %d: %v", i, err)
		}
	}
	err := l.Take("a")
	var rl *RateLimitedError
	if !errors.As(err, &rl) || rl.RetryAfterSeconds() != 3600 {
		t.Fatalf("third hit: %v", err)
	}
	if err := l.Take("b"); err != nil {
		t.Fatalf("another key: %v", err)
	}
	now = now.Add(time.Hour + time.Second)
	if err := l.Take("a"); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}

// TestExportedCodeHelpers match the package's own.
func TestExportedCodeHelpers(t *testing.T) {
	c, err := NewCode()
	if err != nil || !ValidCodeFormat(c) {
		t.Fatalf("code %q %v", c, err)
	}
	salt := NewSalt()
	pepper := []byte("0123456789abcdef0123456789abcdef")
	h := HashCode(pepper, salt, c)
	if !CodeMatches(pepper, salt, h, c) || CodeMatches(pepper, salt, h, "000000x") || CodeMatches([]byte("other-pepper-0123456789abcdefghij"), salt, h, c) {
		t.Fatal("the exported helpers disagree")
	}
}

// TestKeyLimitTakeThen: a refused NEW key leaves no trace; a known key over
// its limit never reaches next; a hit that next refuses is taken back.
func TestKeyLimitTakeThen(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	l := NewKeyLimit(2, time.Hour, 3, func() time.Time { return now })
	refuse := func() error { return &RateLimitedError{Wait: time.Second} }
	calls := 0
	allow := func() error { calls++; return nil }
	// A flood of new keys refused by next fills nothing.
	for i := range 100 {
		if err := l.TakeThen(fmt.Sprint("flood", i), refuse); err == nil {
			t.Fatal("next refused, TakeThen allowed")
		}
	}
	if err := l.TakeThen("real", allow); err != nil || calls != 1 {
		t.Fatalf("a real key after the flood: %v (calls %d)", err, calls)
	}
	// A hit that next refuses is taken back: "real" still has 1 left.
	if err := l.TakeThen("real", refuse); err == nil {
		t.Fatal("next refused, TakeThen allowed")
	}
	if err := l.TakeThen("real", allow); err != nil {
		t.Fatalf("the undone hit was kept: %v", err)
	}
	// Over its limit: next is never called.
	calls = 0
	var rl *RateLimitedError
	if err := l.TakeThen("real", allow); !errors.As(err, &rl) || calls != 0 {
		t.Fatalf("over the limit: %v, next called %d times", err, calls)
	}
}
