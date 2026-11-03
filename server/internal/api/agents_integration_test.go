package api

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"devops/internal/dbq"
	"devops/internal/login"
	"devops/internal/tracker"
)

// POST /orgs/{org}/agents (docs/internal/decisions-T10-agents.md).

const agentsPath = "/api/v1/orgs/orbit/agents"

// TestAgents_Create: an org admin makes an agent; the row is an agent with
// no email and the member role, it is listed, and a token minted for it
// through POST /tokens works and must expire.
func TestAgents_Create(t *testing.T) {
	e := newEnv(t)
	var m tracker.Member
	e.must(201, e.do("POST", agentsPath, e.chen, map[string]any{"username": "  Claude-Review ", "name": " Claude (review) "})).json(t, &m)
	if m.Username != "claude-review" || m.Name != "Claude (review)" || !m.IsAgent || m.Role != "member" ||
		m.Email != nil || m.DeactivatedAt != nil || m.ID == uuid.Nil || m.CreatedAt.IsZero() {
		t.Fatalf("created %+v", m)
	}
	u := e.user("claude-review")
	if !u.IsAgent || u.Email != nil || u.IsPlatformAdmin || u.ID != m.ID {
		t.Fatalf("stored user %+v", u)
	}
	if n := e.countRows(`SELECT count(*) FROM org_members WHERE org_id = $1 AND user_id = $2 AND role = 'member'`, e.orgID, m.ID); n != 1 {
		t.Fatalf("membership rows %d", n)
	}
	// It is in the member list the MCP server resolves people from.
	var users page[tracker.Member]
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/users", e.chen, nil)).json(t, &users)
	found := false
	for _, it := range users.Items {
		found = found || (it.Username == "claude-review" && it.IsAgent)
	}
	if !found {
		t.Fatalf("GET /users lacks the agent: %+v", users.Items)
	}
	// Its token: POST /tokens for an agent needs expiresInDays, and the
	// member role cannot carry the admin scope.
	e.must(400, e.do("POST", "/api/v1/orgs/orbit/tokens", e.chen, map[string]any{"name": "mcp", "scopes": []string{"read"}, "user": "claude-review"}))
	e.must(400, e.do("POST", "/api/v1/orgs/orbit/tokens", e.chen, map[string]any{"name": "mcp", "scopes": []string{"admin"}, "user": "claude-review", "expiresInDays": 30}))
	var tok tracker.CreatedToken
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/tokens", e.chen, map[string]any{"name": "mcp", "scopes": []string{"read", "write"}, "user": "claude-review", "expiresInDays": 90})).json(t, &tok)
	var me tracker.Me
	e.must(200, e.do("GET", "/api/v1/me", tok.Secret, nil)).json(t, &me)
	if me.Username != "claude-review" || !me.IsAgent || me.Email != nil {
		t.Fatalf("me %+v", me)
	}
	// The agent can work, but cannot make agents (it is no admin).
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues", tok.Secret, map[string]any{"team": "ORB", "title": "by the agent"}))
	e.must(403, e.do("POST", agentsPath, tok.Secret, map[string]any{"username": "other", "name": "x"}))
}

// TestAgents_UsernameTaken: a second call with the same username, any case,
// and any username in use (the fixture's agent claude, the human chen) is
// 409 conflict, and nothing is written.
func TestAgents_UsernameTaken(t *testing.T) {
	e := newEnv(t)
	e.must(201, e.do("POST", agentsPath, e.chen, map[string]any{"username": "bot", "name": "Bot"}))
	before := e.countRows(`SELECT count(*) FROM users`) + e.countRows(`SELECT count(*) FROM org_members`)
	for _, name := range []string{"bot", "BOT", "claude", "chen"} {
		t.Run(name, func(t *testing.T) {
			r := e.must(409, e.do("POST", agentsPath, e.chen, map[string]any{"username": name, "name": "Again"}))
			if r.errCode(t) != "conflict" || !strings.Contains(string(r.body), "exists already") {
				t.Fatalf("body %s", r.body)
			}
		})
	}
	if after := e.countRows(`SELECT count(*) FROM users`) + e.countRows(`SELECT count(*) FROM org_members`); after != before {
		t.Fatalf("rows %d -> %d after refused creates", before, after)
	}
}

// TestAgents_Gates: the auth gate, the admin scope and role, JSON only, no
// Idempotency-Key, the shared validation, and no email field.
func TestAgents_Gates(t *testing.T) {
	e := newEnv(t)
	chen := e.user("chen")
	readWrite, _ := e.newToken(chen.ID, tokenOpts{pin: &e.orgID, scopes: []string{"read", "write"}})
	ok := map[string]any{"username": "gate-bot", "name": "Gate"}
	long := strings.Repeat("a", 40)
	tests := []struct {
		name    string
		token   string
		body    any
		headers []string
		status  int
		code    string
	}{
		{"no token", "", ok, nil, 401, "unauthenticated"},
		{"bad token", "odt_nope", ok, nil, 401, "invalid_token"},
		{"agent member token", e.claude, ok, nil, 403, "insufficient_scope"},
		{"admin without the admin scope", readWrite, ok, nil, 403, "insufficient_scope"},
		{"idempotency key", e.chen, ok, []string{"Idempotency-Key", "k1"}, 400, "bad_request"},
		{"text/plain", e.chen, ok, []string{"Content-Type", "text/plain"}, 415, "unsupported_media_type"},
		{"no content type", e.chen, ok, []string{"Content-Type", ""}, 415, "unsupported_media_type"},
		{"empty body", e.chen, nil, []string{"Content-Type", "application/json"}, 400, "bad_request"},
		{"email is not a field", e.chen, map[string]any{"username": "mail-bot", "name": "M", "email": "bot@example.com"}, nil, 400, "bad_request"},
		{"role is not a field", e.chen, map[string]any{"username": "role-bot", "name": "R", "role": "admin"}, nil, 400, "bad_request"},
		{"username missing", e.chen, map[string]any{"name": "x"}, nil, 400, "validation_failed"},
		{"username starts with a digit", e.chen, map[string]any{"username": "1bot", "name": "x"}, nil, 400, "validation_failed"},
		{"username too long", e.chen, map[string]any{"username": long, "name": "x"}, nil, 400, "validation_failed"},
		{"username with a dot", e.chen, map[string]any{"username": "a.b", "name": "x"}, nil, 400, "validation_failed"},
		{"name blank", e.chen, map[string]any{"username": "blank-bot", "name": "   "}, nil, 400, "validation_failed"},
		{"name too long", e.chen, map[string]any{"username": "long-bot", "name": strings.Repeat("n", 201)}, nil, 400, "validation_failed"},
	}
	before := e.countRows(`SELECT count(*) FROM users`)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := e.do("POST", agentsPath, tt.token, tt.body, tt.headers...)
			if r.status != tt.status {
				t.Fatalf("status %d, want %d: %s", r.status, tt.status, r.body)
			}
			if c := r.errCode(t); c != tt.code {
				t.Fatalf("code %q, want %q: %s", c, tt.code, r.body)
			}
		})
	}
	if after := e.countRows(`SELECT count(*) FROM users`); after != before {
		t.Fatalf("users %d -> %d after refused requests", before, after)
	}
	if n := e.countRows(`SELECT count(*) FROM idempotency_keys`); n != 0 {
		t.Fatalf("%d idempotency rows; the route must store none", n)
	}
}

// TestAgents_CrossOrg: an org-pinned token cannot reach another org's route
// (404), and an agent made in acme is a member of acme only.
func TestAgents_CrossOrg(t *testing.T) {
	e := newEnv(t)
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "admin")
	// The fixture token is pinned to orbit.
	r := e.must(404, e.do("POST", "/api/v1/orgs/acme/agents", e.chen, map[string]any{"username": "acme-bot", "name": "A"}))
	r.errCode(t)
	// An org chen is not in at all.
	e.newOrg("other", "OTH")
	unpinned, _ := e.newToken(chen.ID, tokenOpts{})
	e.must(404, e.do("POST", "/api/v1/orgs/other/agents", unpinned, map[string]any{"username": "other-bot", "name": "O"}))
	// The unpinned admin token makes one in acme: acme only.
	var m tracker.Member
	e.must(201, e.do("POST", "/api/v1/orgs/acme/agents", unpinned, map[string]any{"username": "acme-bot", "name": "A"})).json(t, &m)
	if n := e.countRows(`SELECT count(*) FROM org_members WHERE user_id = $1`, m.ID); n != 1 {
		t.Fatalf("acme-bot is in %d orgs", n)
	}
	if n := e.countRows(`SELECT count(*) FROM org_members WHERE user_id = $1 AND org_id = $2`, m.ID, e.orgID); n != 0 {
		t.Fatal("acme-bot is a member of orbit")
	}
	// Orbit cannot mint for acme's agent.
	e.must(400, e.do("POST", "/api/v1/orgs/orbit/tokens", e.chen, map[string]any{"name": "x", "scopes": []string{"read"}, "user": "acme-bot", "expiresInDays": 1}))
}

// TestAgents_WebSessionCanCreate: a cookie session with the CSRF header may
// make an agent; without the header it is refused.
func TestAgents_WebSessionCanCreate(t *testing.T) {
	e := newEnv(t)
	session, _ := e.loginAs(chenEmail)
	e.must(403, e.do("POST", agentsPath, session, map[string]any{"username": "web-bot", "name": "W"}, "X-CSRF-Token", ""))
	e.must(201, e.do("POST", agentsPath, session, map[string]any{"username": "web-bot", "name": "W"}))
}

// TestAgents_CannotLogIn is the tested property of decisions-T10 #2: an
// agent can never be sent or verify a login code. It has no email; and even
// with an email written in by hand, /auth/otp sends nothing and stores no
// code, and /auth/otp/verify refuses the server's fixed code.
func TestAgents_CannotLogIn(t *testing.T) {
	e := newEnvWith(t, envOpts{login: login.Options{FixedCode: "000000"}})
	var m tracker.Member
	e.must(201, e.do("POST", agentsPath, e.chen, map[string]any{"username": "login-bot", "name": "L"})).json(t, &m)
	if n := e.countRows(`SELECT count(*) FROM users WHERE id = $1 AND email IS NULL AND is_agent`, m.ID); n != 1 {
		t.Fatal("the agent has an email or is not an agent")
	}
	// The human control: the same server sends chen a code.
	e.must(202, e.do("POST", "/api/v1/auth/otp", "", map[string]any{"email": chenEmail}))
	if e.mail.count() != 1 {
		t.Fatalf("control: %d codes sent", e.mail.count())
	}
	// Give the agent an email by hand (no route can) and try it.
	mustExec(t, e, `UPDATE users SET email = 'login-bot@example.com' WHERE id = $1`, m.ID)
	e.must(202, e.do("POST", "/api/v1/auth/otp", "", map[string]any{"email": "login-bot@example.com"}))
	if e.mail.count() != 1 || e.mail.last("login-bot@example.com") != "" {
		t.Fatalf("a code was sent to the agent (%d sent)", e.mail.count())
	}
	if n := e.countRows(`SELECT count(*) FROM login_codes WHERE user_id = $1`, m.ID); n != 0 {
		t.Fatalf("%d login codes stored for the agent", n)
	}
	r := e.must(401, e.do("POST", "/api/v1/auth/otp/verify", "", map[string]any{"email": "login-bot@example.com", "code": "000000"}))
	if r.errCode(t) != "invalid_code" || len(r.cookies()) != 0 {
		t.Fatalf("verify: %s %v", r.body, r.cookies())
	}
	// LAUN-122: the native client mode is the same 401.
	r = e.must(401, e.do("POST", "/api/v1/auth/otp/verify", "", map[string]any{"email": "login-bot@example.com", "code": "000000", "client": "native"}))
	if r.errCode(t) != "invalid_code" || strings.Contains(string(r.body), `"token"`) {
		t.Fatalf("native verify: %s", r.body)
	}
	if n := e.countRows(`SELECT count(*) FROM api_tokens WHERE user_id = $1 AND kind = 'session'`, m.ID); n != 0 {
		t.Fatalf("%d sessions for the agent", n)
	}
}

// TestAgents_AfterFirstRunSetup: on an install made by first-run setup
// (no fixture), the operator's first admin token makes `claude`, and the
// row is exactly what the Linear importer looks up for its user map
// (GetOrgMemberByUsername: an active member of the org).
func TestAgents_AfterFirstRunSetup(t *testing.T) {
	e := newEnvWith(t, envOpts{empty: true})
	body, _ := e.completeSetup(e.mintLink())
	first := body.Token.Secret
	e.must(201, e.do("POST", agentsPath, first, map[string]any{"username": "claude", "name": "Claude"}))
	org, err := e.q.GetOrgBySlug(context.Background(), "orbit")
	if err != nil {
		t.Fatal(err)
	}
	m, err := e.q.GetOrgMemberByUsername(context.Background(), dbq.GetOrgMemberByUsernameParams{OrgID: org.ID, Username: "claude"})
	if err != nil {
		t.Fatalf("the importer's lookup does not find claude: %v", err)
	}
	if !m.IsAgent || m.Role != "member" {
		t.Fatalf("claude %+v", m)
	}
	// The agent closes nothing new: setup was complete already.
	if e.setupRequired() {
		t.Fatal("setup is open again")
	}
}
