package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"devops/internal/api"
	"devops/internal/auth"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/idempotency"
	"devops/internal/testdb"
	"devops/internal/tracker"
)

// `server mint-token` against a real database (INTEGRATION=1).

func mintArgs(org, user, name string, scopes []string, days *int) mintTokenArgs {
	return mintTokenArgs{org: org, in: tracker.TokenInput{Name: name, User: user, Scopes: scopes, ExpiresInDays: days}}
}

// TestMintToken_PrintsTheSecretOnceAndStoresTheHash: stdout is the secret
// alone; stderr describes it without it; the row holds only sha256 of it,
// is an org-pinned PAT with no creator, and the token authenticates as the
// agent.
func TestMintToken_PrintsTheSecretOnceAndStoresTheHash(t *testing.T) {
	pool, _ := testdb.New(t)
	fx := testdb.Fixture(t, pool)
	ctx := context.Background()
	var stdout, stderr bytes.Buffer
	before := time.Now()
	if rc := mintToken(ctx, pool, mintArgs(" Orbit ", "Claude", "mcp", []string{"read", "write"}, new(90)), &stdout, &stderr); rc != 0 {
		t.Fatalf("exit %d: %s", rc, stderr.String())
	}
	secret := strings.TrimSuffix(stdout.String(), "\n")
	if strings.Count(stdout.String(), "\n") != 1 || !strings.HasPrefix(secret, "odt_") || strings.ContainsAny(secret, " \t") {
		t.Fatalf("stdout %q is not the secret alone", stdout.String())
	}
	if strings.Contains(stderr.String(), secret) {
		t.Fatal("stderr holds the secret")
	}
	for _, want := range []string{`token "mcp"`, "for claude in org orbit", "scopes read,write", "only copy"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr lacks %q: %s", want, stderr.String())
		}
	}
	var (
		user, kind string
		pinned     bool
		createdBy  *string
		expires    time.Time
		n          int
	)
	if err := pool.QueryRow(ctx, `SELECT u.username, t.kind, t.org_id = $2, t.created_by::text, t.expires_at
		FROM api_tokens t JOIN users u ON u.id = t.user_id WHERE t.token_hash = $1`, auth.HashToken(secret), fx.OrgID).
		Scan(&user, &kind, &pinned, &createdBy, &expires); err != nil {
		t.Fatalf("no row for sha256(secret): %v", err)
	}
	if user != "claude" || kind != auth.KindPAT || !pinned || createdBy != nil {
		t.Fatalf("row user %s kind %s pinned %v created_by %v", user, kind, pinned, createdBy)
	}
	if d := expires.Sub(before); d < 90*24*time.Hour-time.Minute || d > 90*24*time.Hour+time.Minute {
		t.Fatalf("expires in %v, want 90 days", d)
	}
	// The plaintext is in no column.
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM api_tokens WHERE position($1 in name || prefix || encode(token_hash, 'escape')) > 0`, secret).Scan(&n); err != nil || n != 0 {
		t.Fatalf("plaintext stored (%d rows, %v)", n, err)
	}

	// It works against the real router, pinned to orbit.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	q := dbq.New(pool)
	srv := api.NewServer(tracker.New(pool), auth.NewAuthenticator(q, log), events.NewBroker(),
		idempotency.Middleware(q, log, idempotency.Options{}), pool, log)
	hs := httptest.NewServer(srv.Routes())
	t.Cleanup(hs.Close)
	get := func(path string) int {
		req, _ := http.NewRequest("GET", hs.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	if c := get("/api/v1/orgs/orbit/issues"); c != 200 {
		t.Fatalf("the minted token gets %d on orbit", c)
	}
	if c := get("/api/v1/orgs/orbit/tokens"); c != 403 {
		t.Fatalf("a read/write agent token gets %d on an admin route", c)
	}
}

// TestMintToken_Rules: the tracker.CreateToken rules hold, and every refusal
// prints nothing on stdout and stores nothing.
func TestMintToken_Rules(t *testing.T) {
	pool, _ := testdb.New(t)
	testdb.Fixture(t, pool)
	ctx := context.Background()
	testdb.MustExec(t, pool, `INSERT INTO users (username, name, is_agent, deactivated_at) VALUES ('gone', 'Gone', true, now())`)
	testdb.MustExec(t, pool, `INSERT INTO org_members (org_id, user_id, role) SELECT o.id, u.id, 'member' FROM orgs o, users u WHERE o.slug = 'orbit' AND u.username = 'gone'`)
	testdb.MustExec(t, pool, `INSERT INTO users (username, name, is_agent) VALUES ('outsider', 'Outsider', true)`)
	tests := []struct {
		name string
		a    mintTokenArgs
		rc   int
		want string // on stderr
	}{
		{"agent needs an expiry", mintArgs("orbit", "claude", "mcp", []string{"read"}, nil), 1, "expiresInDays (1-365) is required"},
		{"agent at most 365 days", mintArgs("orbit", "claude", "mcp", []string{"read"}, new(400)), 1, "expiresInDays must be 1-365"},
		{"admin scope for a member", mintArgs("orbit", "claude", "mcp", []string{"read", "admin"}, new(30)), 1, "the admin scope needs a user with the org admin role"},
		{"unknown scope", mintArgs("orbit", "claude", "mcp", []string{"root"}, new(30)), 1, "unknown scope"},
		{"blank name", mintArgs("orbit", "claude", "  ", []string{"read"}, new(30)), 1, "name must be 1-200"},
		{"unknown org", mintArgs("nope", "claude", "mcp", []string{"read"}, new(30)), 1, `org "nope"`},
		{"unknown user", mintArgs("orbit", "nobody", "mcp", []string{"read"}, new(30)), 1, `"nobody" is not an active member`},
		{"deactivated user", mintArgs("orbit", "gone", "mcp", []string{"read"}, new(30)), 1, `"gone" is not an active member`},
		{"user of no org", mintArgs("orbit", "outsider", "mcp", []string{"read"}, new(30)), 1, `"outsider" is not an active member`},
	}
	count := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM api_tokens`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if rc := mintToken(ctx, pool, tt.a, &stdout, &stderr); rc != tt.rc {
				t.Fatalf("exit %d, want %d: %s", rc, tt.rc, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout %q on a refusal", stdout.String())
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Fatalf("stderr %q lacks %q", stderr.String(), tt.want)
			}
		})
	}
	if after := count(); after != before {
		t.Fatalf("tokens %d -> %d after refusals", before, after)
	}

	// The rescue: the org admin (a human) gets an admin token with no expiry.
	var stdout, stderr bytes.Buffer
	if rc := mintToken(ctx, pool, mintArgs("orbit", "chen", "rescue", []string{"read", "write", "admin"}, nil), &stdout, &stderr); rc != 0 {
		t.Fatalf("rescue exit %d: %s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "expires never") {
		t.Fatalf("stderr %q", stderr.String())
	}
}
