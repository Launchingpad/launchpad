package api

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"devops/internal/auth"
	"devops/internal/tracker"
)

// POST /orgs/{org}/tokens from a web session (decisions-T11).

const tokensPath = "/api/v1/orgs/orbit/tokens"

func TestSessionTokens_MintRevokeAndRules(t *testing.T) {
	e := newEnv(t)
	e.newHuman("dana", "dana@example.com", "member")
	session, _ := e.loginAs(chenEmail)
	e.must(201, e.do("POST", agentsPath, session, map[string]any{"username": "claude-web", "name": "Claude (web)"}))

	t.Run("read and write with an expiry, for me and for an agent", func(t *testing.T) {
		var mine, bot tracker.CreatedToken
		e.must(201, e.do("POST", tokensPath, session, map[string]any{"name": "mine", "scopes": []string{"read", "write"}, "expiresInDays": 90})).json(t, &mine)
		e.must(201, e.do("POST", tokensPath, session, map[string]any{"name": "mcp", "scopes": []string{"read", "write"}, "user": "claude-web", "expiresInDays": 365})).json(t, &bot)
		if mine.Token.User.Username != "chen" || bot.Token.User.Username != "claude-web" {
			t.Fatalf("owners %q %q", mine.Token.User.Username, bot.Token.User.Username)
		}
		// 90 and 365 days are NOT cut to the session's 30 days (T11 #5).
		if mine.Token.ExpiresAt == nil || time.Until(*mine.Token.ExpiresAt) < 89*24*time.Hour ||
			bot.Token.ExpiresAt == nil || time.Until(*bot.Token.ExpiresAt) < 364*24*time.Hour {
			t.Fatalf("expiry %v %v", mine.Token.ExpiresAt, bot.Token.ExpiresAt)
		}
		// The new secret works as a bearer token, pinned to the org.
		var me tracker.Me
		e.must(200, e.do("GET", "/api/v1/me", bot.Secret, nil)).json(t, &me)
		if me.Username != "claude-web" || !me.IsAgent || strings.Join(me.Scopes, ",") != "read,write" {
			t.Fatalf("me %+v", me)
		}
		e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues", bot.Secret, map[string]any{"team": "ORB", "title": "by the web agent"}))
		// It is in the list; revoke by session works, and the token stops.
		var list page[tracker.Token]
		e.must(200, e.do("GET", tokensPath, session, nil)).json(t, &list)
		found := false
		for _, it := range list.Items {
			found = found || it.ID == bot.Token.ID
		}
		if !found {
			t.Fatalf("token %s not in the list", bot.Token.ID)
		}
		e.must(204, e.do("DELETE", tokensPath+"/"+bot.Token.ID.String(), session, nil))
		e.must(401, e.do("GET", "/api/v1/me", bot.Secret, nil))
		e.must(204, e.do("DELETE", tokensPath+"/"+bot.Token.ID.String(), session, nil)) // twice: still 204
	})

	t.Run("an expiry is required, 1 to 365 days", func(t *testing.T) {
		for _, days := range []any{nil, 0, -1, 366, 3650} {
			body := map[string]any{"name": "x", "scopes": []string{"read"}}
			if days != nil {
				body["expiresInDays"] = days
			}
			r := e.must(400, e.do("POST", tokensPath, session, body))
			if r.errCode(t) != "validation_failed" || !strings.Contains(string(r.body), "expiresInDays") {
				t.Fatalf("days %v: %s", days, r.body)
			}
		}
		// The same for an agent's token (T6a #18 holds).
		e.must(400, e.do("POST", tokensPath, session, map[string]any{"name": "x", "scopes": []string{"read"}, "user": "claude-web"}))
	})

	t.Run("the admin scope is refused with a clear message", func(t *testing.T) {
		for _, body := range []map[string]any{
			{"name": "x", "scopes": []string{"admin"}, "expiresInDays": 30},
			{"name": "x", "scopes": []string{"read", "write", "admin"}, "expiresInDays": 30},
			{"name": "x", "scopes": []string{"admin"}},
			{"name": "x", "scopes": []string{"admin"}, "user": "claude-web", "expiresInDays": 30},
		} {
			r := e.must(403, e.do("POST", tokensPath, session, body))
			if r.errCode(t) != "forbidden" || !strings.Contains(string(r.body), "admin scope") {
				t.Fatalf("%s", r.body)
			}
		}
	})

	t.Run("strict validation", func(t *testing.T) {
		e.must(400, e.do("POST", tokensPath, session, map[string]any{"name": "  ", "scopes": []string{"read"}, "expiresInDays": 30}))
		e.must(400, e.do("POST", tokensPath, session, map[string]any{"name": "x", "scopes": []string{"root"}, "expiresInDays": 30}))
		e.must(400, e.do("POST", tokensPath, session, map[string]any{"name": "x", "scopes": []string{"read"}, "expiresInDays": 30, "extra": 1}))
		e.must(400, e.do("POST", tokensPath, session, map[string]any{"name": "x", "scopes": []string{"read"}, "expiresInDays": 30, "user": "nobody"}))
		// A token for another human is refused, as for a bearer token.
		e.must(403, e.do("POST", tokensPath, session, map[string]any{"name": "x", "scopes": []string{"read"}, "expiresInDays": 30, "user": "dana"}))
	})

	t.Run("Idempotency-Key is refused, nothing is stored", func(t *testing.T) {
		body := map[string]any{"name": "x", "scopes": []string{"read"}, "expiresInDays": 30}
		e.must(400, e.do("POST", tokensPath, session, body, "Idempotency-Key", "k-1"))
		if n := e.countRows(`SELECT count(*) FROM idempotency_keys`); n != 0 {
			t.Fatalf("%d idempotency rows", n)
		}
	})
}

func TestSessionTokens_NonAdminAndCSRF(t *testing.T) {
	e := newEnv(t)
	e.newHuman("dana", "dana@example.com", "member")
	dana, _ := e.loginAs("dana@example.com")
	chen, _ := e.loginAs(chenEmail)
	body := map[string]any{"name": "x", "scopes": []string{"read"}, "expiresInDays": 30}

	// A member's session has no admin scope: 403 insufficient_scope.
	if code := e.must(403, e.do("POST", tokensPath, dana, body)).errCode(t); code != "insufficient_scope" {
		t.Fatalf("member session: %s", code)
	}
	// A write without the CSRF header fails, with the right or no cookie.
	if code := e.must(403, e.do("POST", tokensPath, chen, body, auth.CSRFHeader, "")).errCode(t); code != "csrf_failed" {
		t.Fatalf("no CSRF header: %s", code)
	}
	if code := e.must(403, e.do("POST", tokensPath, chen, body, auth.CSRFHeader, "wrong")).errCode(t); code != "csrf_failed" {
		t.Fatalf("wrong CSRF header: %s", code)
	}
	if code := e.must(403, e.do("DELETE", tokensPath+"/00000000-0000-0000-0000-000000000001", chen, nil, auth.CSRFHeader, "")).errCode(t); code != "csrf_failed" {
		t.Fatalf("revoke without CSRF header: %s", code)
	}
	if n := e.countRows(`SELECT count(*) FROM api_tokens WHERE kind = 'pat' AND name = 'x'`); n != 0 {
		t.Fatalf("%d tokens made by refused requests", n)
	}
}

// The bearer-admin path is unchanged: no expiry needed, the admin scope is
// allowed, and the token is cut to the minting token's expiry.
func TestSessionTokens_BearerPathUnchanged(t *testing.T) {
	e := newEnv(t)
	var ct tracker.CreatedToken
	e.must(201, e.do("POST", tokensPath, e.chen, map[string]any{"name": "forever", "scopes": []string{"read", "write", "admin"}})).json(t, &ct)
	if ct.Token.ExpiresAt != nil || len(ct.Token.Scopes) != 3 {
		t.Fatalf("token %+v", ct.Token)
	}
	e.must(400, e.do("POST", tokensPath, e.chen, map[string]any{"name": "bot", "scopes": []string{"read"}, "user": "claude"}))
	e.must(201, e.do("POST", tokensPath, e.chen, map[string]any{"name": "bot", "scopes": []string{"read"}, "user": "claude", "expiresInDays": 365}))
	// A bearer token with 400 days: 3650 is still allowed for a human.
	e.must(201, e.do("POST", tokensPath, e.chen, map[string]any{"name": "long", "scopes": []string{"read"}, "expiresInDays": 3650}))
}

// The secret is in the one 201 body and nowhere else: not in the logs (at
// debug level), the events, the idempotency rows, or any error body.
func TestSessionTokens_SecretIsNeverStored(t *testing.T) {
	e := newEnvWith(t, envOpts{logLevel: -4}) // slog.LevelDebug
	session, _ := e.loginAs(chenEmail)
	var ct tracker.CreatedToken
	e.must(201, e.do("POST", tokensPath, session, map[string]any{"name": "audit me", "scopes": []string{"read"}, "expiresInDays": 30})).json(t, &ct)
	e.must(400, e.do("POST", tokensPath, session, map[string]any{"name": "again", "scopes": []string{"read"}, "expiresInDays": 30}, "Idempotency-Key", "k-2"))
	e.must(204, e.do("DELETE", tokensPath+"/"+ct.Token.ID.String(), session, nil))
	refused := e.must(400, e.do("POST", tokensPath, session, map[string]any{"name": ct.Secret, "scopes": []string{"read"}}))

	secret := ct.Secret
	if !strings.HasPrefix(secret, "odt_") || len(secret) < 20 {
		t.Fatalf("unexpected secret shape")
	}
	logs := e.logs.String()
	if strings.Contains(logs, secret) {
		t.Fatal("the secret is in the server log")
	}
	// The audit lines exist and name the token, not the secret.
	if !strings.Contains(logs, "audit: token created") || !strings.Contains(logs, "audit: token revoked") ||
		!strings.Contains(logs, ct.Token.ID.String()) || !strings.Contains(logs, `name="audit me"`) {
		t.Fatalf("audit lines are missing:\n%s", logs)
	}
	for _, q := range []string{
		`SELECT count(*) FROM events WHERE payload::text LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM idempotency_keys WHERE key LIKE '%' || $1 || '%' OR encode(response_body, 'escape') LIKE '%' || $1 || '%'`,
	} {
		if n := e.countRows(q, secret); n != 0 {
			t.Fatalf("the secret is in the database (%s)", q)
		}
	}
	if n := e.countRows(`SELECT count(*) FROM idempotency_keys`); n != 0 {
		t.Fatalf("%d idempotency rows", n)
	}
	if strings.Contains(string(refused.body), secret) {
		t.Fatal("an error body repeats a planted secret")
	}
	// No event for token create or revoke (T11 #7): the feed has no token type.
	if n := e.countRows(`SELECT count(*) FROM events WHERE type LIKE 'token.%'`); n != 0 {
		t.Fatalf("%d token events", n)
	}
}

// A hijacked session cannot mint in a loop (T11 #6): 5 at once, then 429
// with Retry-After. A bearer token is not limited.
func TestSessionTokens_RateLimit(t *testing.T) {
	e := newEnv(t)
	session, _ := e.loginAs(chenEmail)
	body := map[string]any{"name": "x", "scopes": []string{"read"}, "expiresInDays": 30}
	for i := range sessionMintBurst {
		if r := e.do("POST", tokensPath, session, body); r.status != 201 {
			t.Fatalf("mint %d: %d %s", i, r.status, r.body)
		}
	}
	r := e.must(429, e.do("POST", tokensPath, session, body))
	if r.errCode(t) != "rate_limited" || r.header.Get("Retry-After") == "" {
		t.Fatalf("%s %v", r.body, r.header)
	}
	// Bearer calls are not rate limited: more than the burst all succeed.
	for range sessionMintBurst + 2 {
		e.must(201, e.do("POST", tokensPath, e.chen, body))
	}
}

func TestMintLimiter_Refill(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	l := newMintLimiter(func() time.Time { return now })
	u := uuid.New()
	for range sessionMintBurst {
		if ok, _ := l.take(u); !ok {
			t.Fatal("refused within the burst")
		}
	}
	ok, wait := l.take(u)
	if ok || wait <= 0 || wait > 6*time.Minute+time.Second {
		t.Fatalf("ok %v wait %v", ok, wait)
	}
	now = now.Add(wait)
	if ok, _ := l.take(u); !ok {
		t.Fatal("still refused after the wait")
	}
}

// A refused request (a typo in the form) does not use the allowance.
func TestSessionTokens_RefusedRequestsAreNotCounted(t *testing.T) {
	e := newEnv(t)
	session, _ := e.loginAs(chenEmail)
	for range sessionMintBurst * 3 {
		e.must(400, e.do("POST", tokensPath, session, map[string]any{"name": "x", "scopes": []string{"read"}}))
	}
	e.must(201, e.do("POST", tokensPath, session, map[string]any{"name": "x", "scopes": []string{"read"}, "expiresInDays": 30}))
}

// The revoke audit line names the token (name, owner, scopes, expiry), never
// the secret, and a repeat revoke logs nothing.
func TestSessionTokens_RevokeAudit(t *testing.T) {
	e := newEnvWith(t, envOpts{logLevel: -4})
	session, _ := e.loginAs(chenEmail)
	var ct tracker.CreatedToken
	e.must(201, e.do("POST", tokensPath, session, map[string]any{"name": "audit-me", "scopes": []string{"read", "write"}, "expiresInDays": 30})).json(t, &ct)
	path := tokensPath + "/" + ct.Token.ID.String()
	e.must(204, e.do("DELETE", path, session, nil))
	e.must(204, e.do("DELETE", path, session, nil))
	logs := e.logs.String()
	if n := strings.Count(logs, "audit: token revoked"); n != 1 {
		t.Fatalf("%d revoke lines, want 1:\n%s", n, logs)
	}
	for _, want := range []string{`name=audit-me`, `owner=chen`, `scopes="[read write]"`, "expires_at=", ct.Token.ID.String(), "actor=chen"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("revoke line lacks %q:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, ct.Secret) {
		t.Fatal("the secret is in the log")
	}
}
