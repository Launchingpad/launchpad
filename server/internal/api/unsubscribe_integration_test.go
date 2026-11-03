package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/notify"
)

// The public one-click unsubscribe (LAUN-34, decisions-T13 #25).

const unsubPath = "/api/v1/notifications/unsubscribe/"

// mintUnsub stores an unsubscribe token for user (hash only), as the sender does.
func (e *env) mintUnsub(token string, user uuid.UUID, targets string, expires time.Time) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO notification_mail_tokens (token_hash, org_id, user_id, targets, expires_at) VALUES ($1, $2, $3, $4::jsonb, $5)`,
		notify.HashToken(token), e.orgID, user, targets, expires); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) emailOf(user uuid.UUID, event string) string {
	e.t.Helper()
	var v *string
	err := e.pool.QueryRow(context.Background(), `SELECT email FROM notification_settings WHERE user_id = $1 AND event = $2 AND team_id IS NULL`, user, event).Scan(&v)
	if err != nil {
		return "<no row>"
	}
	if v == nil {
		return "<null>"
	}
	return *v
}

func TestUnsubscribe_AnyTokenIs204(t *testing.T) {
	e := newEnvWith(t, envOpts{logLevel: slog.LevelDebug})
	start := time.Now()
	r := e.do("POST", unsubPath+"no-such-token", "", nil)
	if r.status != http.StatusNoContent || len(r.body) != 0 {
		t.Fatalf("status %d body %q", r.status, r.body)
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Fatalf("answered in %v, under the 300 ms floor", time.Since(start))
	}
	// The one-click form body, a junk bearer token and a junk cookie change nothing.
	r = e.do("POST", unsubPath+strings.Repeat("z", 150), "", nil,
		"Content-Type", "application/x-www-form-urlencoded", "Authorization", "Bearer junk", "Cookie", "odt_session=junk")
	if r.status != http.StatusNoContent {
		t.Fatalf("status %d body %s", r.status, r.body)
	}
	// An over-long token is still 204 (and does nothing).
	if r := e.do("POST", unsubPath+strings.Repeat("z", 5000), "", nil); r.status != http.StatusNoContent {
		t.Fatalf("long token: status %d", r.status)
	}
}

func TestUnsubscribe_ValidTokenTurnsOffExactlyThatEvent(t *testing.T) {
	e := newEnvWith(t, envOpts{})
	alice := e.newHuman("alice", "alice@example.test", "member")
	e.mintUnsub("tok-valid-1", alice.ID, `[{"event":"assigned","teamId":null}]`, time.Now().Add(time.Hour))
	if r := e.do("POST", unsubPath+"tok-valid-1", "", nil, "Content-Type", "application/x-www-form-urlencoded"); r.status != http.StatusNoContent {
		t.Fatalf("status %d body %s", r.status, r.body)
	}
	if got := e.emailOf(alice.ID, "assigned"); got != "off" {
		t.Fatalf("assigned email = %s", got)
	}
	var rows int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM notification_settings WHERE user_id = $1`, alice.ID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("settings rows %d (%v), want 1", rows, err)
	}
	// Again: 204 and no change.
	if r := e.do("POST", unsubPath+"tok-valid-1", "", nil); r.status != http.StatusNoContent {
		t.Fatalf("second post: %d", r.status)
	}
}

func TestUnsubscribe_GetIsNotAnAction(t *testing.T) {
	e := newEnvWith(t, envOpts{})
	alice := e.newHuman("alice", "alice@example.test", "member")
	e.mintUnsub("tok-get-1", alice.ID, `[{"event":"assigned","teamId":null}]`, time.Now().Add(time.Hour))
	for _, m := range []string{"GET", "HEAD", "PUT", "DELETE", "PATCH"} {
		if r := e.do(m, unsubPath+"tok-get-1", "", nil); r.status != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status %d, want 405", m, r.status)
		}
	}
	if got := e.emailOf(alice.ID, "assigned"); got != "<no row>" {
		t.Fatalf("a non-POST changed a setting: %s", got)
	}
	var used *time.Time
	if err := e.pool.QueryRow(context.Background(), `SELECT used_at FROM notification_mail_tokens`).Scan(&used); err != nil || used != nil {
		t.Fatalf("the token was used by a non-POST: %v (%v)", used, err)
	}
}

func TestUnsubscribe_IdempotencyKeyIsRefusedAndNothingIsStored(t *testing.T) {
	e := newEnvWith(t, envOpts{})
	if r := e.do("POST", unsubPath+"tok-x", "", nil, "Idempotency-Key", "k-1"); r.status != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", r.status)
	}
	e.do("POST", unsubPath+"tok-x", "", nil)
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM idempotency_keys`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("idempotency rows %d (%v)", n, err)
	}
}

// The access log and the error log never hold the token.
func TestUnsubscribe_TokenIsNotInTheLogs(t *testing.T) {
	e := newEnvWith(t, envOpts{logLevel: slog.LevelDebug})
	alice := e.newHuman("alice", "alice@example.test", "member")
	const tok = "TOKEN-PLANTED-b91f6c2d"
	e.mintUnsub(tok, alice.ID, `[{"event":"assigned","teamId":null}]`, time.Now().Add(time.Hour))
	e.do("POST", unsubPath+tok, "", nil)
	e.do("GET", unsubPath+tok, "", nil)
	e.do("POST", unsubPath+tok+"-other", "", nil)
	out := e.logs.String()
	if strings.Contains(out, "TOKEN-PLANTED") {
		t.Fatalf("the log holds the token:\n%s", out)
	}
	if !strings.Contains(out, "unsubscribe/{token}") {
		t.Fatalf("the access log does not name the route:\n%s", out)
	}
}

func TestLogPath(t *testing.T) {
	for in, want := range map[string]string{
		"/api/v1/notifications/unsubscribe/abc":    "/api/v1/notifications/unsubscribe/{token}",
		"/api/v1/notifications/unsubscribe/":       "/api/v1/notifications/unsubscribe/{token}",
		"/api/v1/orgs/orbit/issues/ORB-1":          "/api/v1/orgs/orbit/issues/ORB-1",
		"/api/v1/notifications/unsubscribeX/token": "/api/v1/notifications/unsubscribeX/token",
	} {
		r, _ := http.NewRequest("POST", "http://x"+in, nil)
		if got := logPath(r); got != want {
			t.Errorf("logPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnsubscribe_RateLimitPerClient(t *testing.T) {
	e := newEnvWith(t, envOpts{configure: func(_ *env, s *Server) { s.UnsubscribeFloor = 0 }})
	var last resp
	n := 0
	for i := 0; i < unsubPerClientPerMinute+5; i++ {
		last = e.do("POST", unsubPath+"x", "", nil)
		if last.status == http.StatusTooManyRequests {
			break
		}
		n++
	}
	if n != unsubPerClientPerMinute || last.status != http.StatusTooManyRequests {
		t.Fatalf("allowed %d requests, last status %d; want %d then 429", n, last.status, unsubPerClientPerMinute)
	}
	if last.header.Get("Retry-After") == "" || last.errCode(t) != "rate_limited" {
		t.Fatalf("429 without Retry-After or code: %v %s", last.header, last.body)
	}
}

// A proxied request, with no trusted proxy configured, has no client key:
// it is not locked out by the per-client limit.
func TestUnsubscribe_ProxiedRequestsHaveNoPerClientKey(t *testing.T) {
	e := newEnvWith(t, envOpts{configure: func(_ *env, s *Server) { s.UnsubscribeFloor = 0 }})
	for i := range unsubPerClientPerMinute * 2 {
		if r := e.do("POST", unsubPath+"x", "", nil, "X-Forwarded-For", "203.0.113.9"); r.status != http.StatusNoContent {
			t.Fatalf("request %d: status %d", i, r.status)
		}
	}
}

func TestUnsubLimiter_WindowAndGlobal(t *testing.T) {
	now := time.Now()
	l := newUnsubLimiter(func() time.Time { return now })
	for range unsubPerClientPerMinute {
		if ok, _ := l.allow("a"); !ok {
			t.Fatal("refused inside the limit")
		}
	}
	if ok, wait := l.allow("a"); ok || wait <= 0 || wait > time.Minute {
		t.Fatalf("allowed past the limit (wait %v)", wait)
	}
	if ok, _ := l.allow("b"); !ok {
		t.Fatal("another client was refused")
	}
	now = now.Add(61 * time.Second)
	if ok, _ := l.allow("a"); !ok {
		t.Fatal("not allowed after the window")
	}
	// The global limit counts every client, and a request with no key.
	g := newUnsubLimiter(func() time.Time { return now })
	for range unsubGlobalPerMinute {
		if ok, _ := g.allow(""); !ok {
			t.Fatal("global limit refused early")
		}
	}
	if ok, _ := g.allow(""); ok {
		t.Fatal("global limit did not hold")
	}
}
