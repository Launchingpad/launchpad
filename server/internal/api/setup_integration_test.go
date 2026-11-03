package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"devops/internal/auth"
	"devops/internal/dbq"
	"devops/internal/setup"
	"devops/internal/tracker"
)

// First-run setup (docs/internal/decisions-T8-first-run.md).

const setupEmail = "chen@example.com"

// setupBody is a valid POST /setup body for token.
func setupBody(token string) map[string]any {
	return map[string]any{
		"token": token,
		"user":  map[string]any{"username": "chen", "name": "Chen", "email": " Chen@example.com "},
		"org":   map[string]any{"slug": "orbit", "name": "Orbit"},
		"team":  map[string]any{"key": "ORB", "name": "Orbit"},
	}
}

// mintLink makes a setup link the way a boot does and returns its token.
func (e *env) mintLink() string {
	e.t.Helper()
	secret, minted, err := setup.MintIfRequired(context.Background(), e.pool)
	if err != nil || !minted || secret == "" {
		e.t.Fatalf("mint: minted %v err %v", minted, err)
	}
	return secret
}

func (e *env) setupRequired() bool {
	e.t.Helper()
	var st setup.Status
	e.must(200, e.do("GET", "/api/v1/setup", "", nil)).json(e.t, &st)
	return st.SetupRequired
}

// completeSetup runs POST /setup and returns the body and the session.
func (e *env) completeSetup(token string) (setup.Response, string) {
	e.t.Helper()
	r := e.must(http.StatusCreated, e.do("POST", "/api/v1/setup", "", setupBody(token)))
	var body setup.Response
	r.json(e.t, &body)
	c, ok := r.cookies()[e.names.Session]
	if !ok || c.Value == "" {
		e.t.Fatalf("setup set no %s cookie", e.names.Session)
	}
	if cc := r.cookies()[e.names.CSRF]; cc == nil || cc.Value != body.CSRFToken || body.CSRFToken == "" {
		e.t.Fatalf("csrf cookie %v vs body %q", cc, body.CSRFToken)
	}
	return body, e.asSession(c.Value)
}

func (e *env) rowCounts() map[string]int {
	e.t.Helper()
	out := map[string]int{}
	for _, tbl := range []string{"users", "orgs", "org_members", "teams", "workflow_states", "labels", "api_tokens", "events"} {
		out[tbl] = e.countRows("SELECT count(*) FROM " + tbl)
	}
	return out
}

// TestSetup_CreatesTheOrgAndLogsIn: the whole first run, then the first
// token in use, and a second team through the existing route.
func TestSetup_CreatesTheOrgAndLogsIn(t *testing.T) {
	e := newEnvWith(t, envOpts{empty: true})
	if !e.setupRequired() {
		t.Fatal("an empty install says setupRequired false")
	}
	tok := e.mintLink()
	before := time.Now()
	body, session := e.completeSetup(tok)

	if body.User.Username != "chen" || body.User.Email != "chen@example.com" || !body.User.IsPlatformAdmin {
		t.Fatalf("user %+v", body.User)
	}
	if len(body.Orgs) != 1 || body.Orgs[0].Slug != "orbit" || body.Orgs[0].Role != "admin" {
		t.Fatalf("orgs %+v", body.Orgs)
	}
	ft := body.Token
	if !auth.WellFormed(ft.Secret) || ft.Token.Kind != auth.KindPAT || !slices.Equal(ft.Token.Scopes, []string{"read", "write", "admin"}) {
		t.Fatalf("first token %+v", ft.Token)
	}
	// No expiry: a token minted with it may not outlive it (T6a #19).
	if ft.Token.ExpiresAt != nil || ft.Token.CreatedAt.Before(before.Add(-time.Minute)) {
		t.Fatalf("first token expiry %v created %v", ft.Token.ExpiresAt, ft.Token.CreatedAt)
	}
	want := map[string]int{"users": 1, "orgs": 1, "org_members": 1, "teams": 1, "workflow_states": 9, "labels": 3, "api_tokens": 2, "events": 1}
	if got := e.rowCounts(); !mapsEqual(got, want) {
		t.Fatalf("rows %v, want %v", got, want)
	}
	if e.setupRequired() {
		t.Fatal("setupRequired after setup")
	}

	// The first token: pinned to the org (the PAT rule), admin there.
	var me tracker.Me
	e.must(200, e.do("GET", "/api/v1/me", ft.Secret, nil)).json(t, &me)
	if !me.IsPlatformAdmin || me.PinnedOrgID == nil || me.TokenKind != "pat" || !slices.Contains(me.Scopes, "admin") {
		t.Fatalf("me with the first token %+v", me)
	}
	var orgs struct{ Items []tracker.Org }
	e.must(200, e.do("GET", "/api/v1/orgs", ft.Secret, nil)).json(t, &orgs)
	if len(orgs.Items) != 1 || *me.PinnedOrgID != orgs.Items[0].ID {
		t.Fatalf("orgs %+v pin %v", orgs.Items, me.PinnedOrgID)
	}
	// The session: unpinned, admin scope, platform admin.
	var sess struct {
		User struct {
			IsPlatformAdmin bool `json:"isPlatformAdmin"`
		} `json:"user"`
	}
	e.must(200, e.do("GET", "/api/v1/auth/session", session, nil)).json(t, &sess)
	e.must(200, e.do("GET", "/api/v1/me", session, nil)).json(t, &me)
	if !sess.User.IsPlatformAdmin || !me.IsPlatformAdmin || me.PinnedOrgID != nil || !slices.Contains(me.Scopes, "admin") {
		t.Fatalf("session %+v me %+v", sess, me)
	}
	// A web session mints only with an expiry and no admin scope (decisions-T11).
	e.must(400, e.do("POST", "/api/v1/orgs/orbit/tokens", session, map[string]any{"name": "x", "scopes": []string{"read"}}))
	e.must(403, e.do("POST", "/api/v1/orgs/orbit/tokens", session, map[string]any{"name": "x", "scopes": []string{"admin"}, "expiresInDays": 7}))

	// The first team works, with its states and the default labels.
	is := e.createIssue(ft.Secret, map[string]any{"title": "first"})
	if is.Identifier != "ORB-1" {
		t.Fatalf("first issue %s", is.Identifier)
	}
	var labels struct{ Items []tracker.Label }
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/labels", ft.Secret, nil)).json(t, &labels)
	if len(labels.Items) != 3 {
		t.Fatalf("labels %+v", labels.Items)
	}
	// More teams: the existing POST /orgs/{org}/teams.
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/teams", ft.Secret, map[string]any{"key": "OPS", "name": "Ops"}))
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/teams", ft.Secret, map[string]any{"key": "WEB", "name": "Web"}))
	var teams struct{ Items []tracker.Team }
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/teams", ft.Secret, nil)).json(t, &teams)
	if len(teams.Items) != 3 {
		t.Fatalf("teams after setup %+v", teams.Items)
	}
	e.createIssue(ft.Secret, map[string]any{"title": "ops", "team": "OPS"})

	// The same link again, and any link at all: 409.
	if r := e.do("POST", "/api/v1/setup", "", setupBody(tok)); r.status != 409 || r.errCode(t) != "setup_complete" {
		t.Fatalf("second setup %d %s", r.status, r.body)
	}
	if _, minted, err := setup.MintIfRequired(context.Background(), e.pool); err != nil || minted {
		t.Fatalf("a boot after setup minted a link: %v %v", minted, err)
	}
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestSetup_NewLinkClosesTheOlder: every mint (a restart, setup-link)
// closes the links before it.
func TestSetup_NewLinkClosesTheOlder(t *testing.T) {
	e := newEnvWith(t, envOpts{empty: true})
	first := e.mintLink()
	second := e.mintLink()
	if first == second {
		t.Fatal("two mints gave the same link")
	}
	if n := e.countRows(`SELECT count(*) FROM setup_tokens WHERE consumed_at IS NULL`); n != 1 {
		t.Fatalf("%d live links, want 1", n)
	}
	if r := e.do("POST", "/api/v1/setup", "", setupBody(first)); r.status != 401 || r.errCode(t) != "invalid_setup_token" {
		t.Fatalf("older link: %d %s", r.status, r.body)
	}
	e.completeSetup(second)
}

// TestSetup_LinkExpires after 24 hours, and an expired link creates nothing.
func TestSetup_LinkExpires(t *testing.T) {
	e := newEnvWith(t, envOpts{empty: true})
	tok := e.mintLink()
	var ttl float64
	if err := e.pool.QueryRow(context.Background(), `SELECT extract(epoch FROM expires_at - created_at) FROM setup_tokens`).Scan(&ttl); err != nil {
		t.Fatal(err)
	}
	if ttl < 24*3600-5 || ttl > 24*3600+5 {
		t.Fatalf("link lifetime %vs, want 24h", ttl)
	}
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE setup_tokens SET created_at = now() - interval '25 hours', expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if r := e.do("POST", "/api/v1/setup", "", setupBody(tok)); r.status != 401 || r.errCode(t) != "invalid_setup_token" {
		t.Fatalf("expired link: %d %s", r.status, r.body)
	}
	if n := e.countRows(`SELECT count(*) FROM users`) + e.countRows(`SELECT count(*) FROM orgs`); n != 0 {
		t.Fatalf("an expired link created %d rows", n)
	}
	if !e.setupRequired() {
		t.Fatal("setup closed after an expired link")
	}
}

// TestSetup_LinkIsSingleUse: a used link stays used even when the install
// has no user again (its users deleted), so only consumed_at protects it.
func TestSetup_LinkIsSingleUse(t *testing.T) {
	e := newEnvWith(t, envOpts{empty: true})
	tok := e.mintLink()
	e.completeSetup(tok)
	if _, err := e.pool.Exec(context.Background(), `DELETE FROM orgs; DELETE FROM users;`); err != nil {
		t.Fatal(err)
	}
	if !e.setupRequired() {
		t.Fatal("setup is not open again with no user")
	}
	if r := e.do("POST", "/api/v1/setup", "", setupBody(tok)); r.status != 401 || r.errCode(t) != "invalid_setup_token" {
		t.Fatalf("used link reused: %d %s", r.status, r.body)
	}
	if n := e.countRows(`SELECT count(*) FROM users`); n != 0 {
		t.Fatalf("a used link created %d users", n)
	}
}

// TestSetup_ConcurrentCompletion: many requests with the one link at once.
// Exactly one succeeds; the others fail with nothing half-made.
func TestSetup_ConcurrentCompletion(t *testing.T) {
	e := newEnvWith(t, envOpts{empty: true})
	tok := e.mintLink()
	const n = 8
	statuses := make([]int, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			b := setupBody(tok)
			b["user"] = map[string]any{"username": "user" + string(rune('a'+i)), "name": "U", "email": "u" + string(rune('a'+i)) + "@example.com"}
			r, err := e.request("POST", "/api/v1/setup", "", b)
			statuses[i], errs[i] = r.status, err
		})
	}
	close(start)
	wg.Wait()
	ok := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		switch statuses[i] {
		case 201:
			ok++
		case 401, 409:
		default:
			t.Fatalf("request %d: status %d", i, statuses[i])
		}
	}
	if ok != 1 {
		t.Fatalf("%d successes, want exactly 1 (%v)", ok, statuses)
	}
	want := map[string]int{"users": 1, "orgs": 1, "org_members": 1, "teams": 1, "workflow_states": 9, "labels": 3, "api_tokens": 2, "events": 1}
	if got := e.rowCounts(); !mapsEqual(got, want) {
		t.Fatalf("rows %v, want %v", got, want)
	}
}

// TestSetup_CompleteOnceAUserExists: 409 for any link once a user exists,
// and no link is minted. An existing install (the fixture) is one.
func TestSetup_CompleteOnceAUserExists(t *testing.T) {
	e := newEnv(t) // chen and claude exist
	if e.setupRequired() {
		t.Fatal("an install with users says setupRequired")
	}
	if _, minted, err := setup.MintIfRequired(context.Background(), e.pool); err != nil || minted {
		t.Fatalf("minted %v err %v", minted, err)
	}
	// A live link that predates the users (a link minted, then a user made
	// some other way) does not open setup either.
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO setup_tokens (token_hash, expires_at)
		VALUES (sha256(convert_to('AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA', 'UTF8')), now() + interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	r := e.do("POST", "/api/v1/setup", "", setupBody("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"))
	if r.status != 409 || r.errCode(t) != "setup_complete" {
		t.Fatalf("setup with users: %d %s", r.status, r.body)
	}
	if n := e.countRows(`SELECT count(*) FROM users WHERE is_platform_admin`); n != 0 {
		t.Fatalf("%d platform admins made", n)
	}
	// Only an agent exists: still complete (any user closes setup).
	e2 := newEnvWith(t, envOpts{empty: true})
	tok := e2.mintLink()
	if _, err := e2.q.InsertUser(context.Background(), dbq.InsertUserParams{Username: "bot", Name: "Bot", IsAgent: true}); err != nil {
		t.Fatal(err)
	}
	if r := e2.do("POST", "/api/v1/setup", "", setupBody(tok)); r.status != 409 {
		t.Fatalf("agent-only install: %d %s", r.status, r.body)
	}
}

// TestSetup_Inputs: a bad body is refused before the link is touched, so
// the link still works afterwards.
func TestSetup_Inputs(t *testing.T) {
	e := newEnvWith(t, envOpts{empty: true})
	tok := e.mintLink()
	mod := func(f func(b map[string]any)) map[string]any {
		b := setupBody(tok)
		f(b)
		return b
	}
	cases := []struct {
		name   string
		body   map[string]any
		status int
		code   string
	}{
		{"malformed token", mod(func(b map[string]any) { b["token"] = "short" }), 401, "invalid_setup_token"},
		{"unknown token", mod(func(b map[string]any) { b["token"] = strings.Repeat("A", 43) }), 401, "invalid_setup_token"},
		{"bad username", mod(func(b map[string]any) { b["user"].(map[string]any)["username"] = "9chen" }), 400, "validation_failed"},
		{"no name", mod(func(b map[string]any) { b["user"].(map[string]any)["name"] = " " }), 400, "validation_failed"},
		{"bad email", mod(func(b map[string]any) { b["user"].(map[string]any)["email"] = "chen" }), 400, "validation_failed"},
		{"bad slug", mod(func(b map[string]any) { b["org"].(map[string]any)["slug"] = "Orbit" }), 400, "validation_failed"},
		{"one-letter slug", mod(func(b map[string]any) { b["org"].(map[string]any)["slug"] = "o" }), 400, "validation_failed"},
		{"bad team key", mod(func(b map[string]any) { b["team"].(map[string]any)["key"] = "orb" }), 400, "validation_failed"},
		{"no team name", mod(func(b map[string]any) { b["team"].(map[string]any)["name"] = "" }), 400, "validation_failed"},
		{"unknown field", mod(func(b map[string]any) { b["admin"] = true }), 400, "bad_request"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r := e.do("POST", "/api/v1/setup", "", tt.body)
			if r.status != tt.status || r.errCode(t) != tt.code {
				t.Fatalf("%d %s, want %d %s", r.status, r.body, tt.status, tt.code)
			}
		})
	}
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/v1/setup", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "text/plain")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 415 {
		t.Fatalf("text/plain: %d", res.StatusCode)
	}
	if r := e.do("POST", "/api/v1/setup", "", setupBody(tok), "Idempotency-Key", "k"); r.status != 400 {
		t.Fatalf("Idempotency-Key accepted: %d", r.status)
	}
	if n := e.countRows(`SELECT count(*) FROM users`); n != 0 {
		t.Fatalf("a refused body made %d users", n)
	}
	e.completeSetup(tok) // still live
}

// TestSetup_RateLimited: the route bucket answers 429 with Retry-After.
func TestSetup_RateLimited(t *testing.T) {
	e := newEnvWith(t, envOpts{empty: true, setup: setup.Options{PerMinute: 1, Burst: 1}})
	bad := setupBody(strings.Repeat("A", 43))
	e.must(401, e.do("POST", "/api/v1/setup", "", bad))
	r := e.do("POST", "/api/v1/setup", "", bad)
	if r.status != 429 || r.errCode(t) != "rate_limited" || r.header.Get("Retry-After") == "" {
		t.Fatalf("second request: %d %s %v", r.status, r.body, r.header)
	}
}

// TestRequirePlatformAdmin_RealTokens: the middleware matrix against real
// token rows (GetTokenByHash must carry the user's flag).
func TestRequirePlatformAdmin_RealTokens(t *testing.T) {
	e := newEnvWith(t, envOpts{empty: true})
	body, session := e.completeSetup(e.mintLink())
	ctx := context.Background()
	admin := e.user("chen")
	orgID := body.Orgs[0].ID
	e.orgID = orgID // newHuman adds members to e.orgID
	member := e.newHuman("ann", "ann@example.com", "member")
	orgAdmin := e.newHuman("bob", "bob@example.com", "admin")
	unpinned, _ := e.newToken(admin.ID, tokenOpts{})
	readWrite, _ := e.newToken(admin.ID, tokenOpts{scopes: []string{"read", "write"}})
	memberTok, _ := e.newToken(member.ID, tokenOpts{pin: &orgID, scopes: []string{"read", "write"}})
	orgAdminTok, _ := e.newToken(orgAdmin.ID, tokenOpts{pin: &orgID})
	orgAdminUnpinned, _ := e.newToken(orgAdmin.ID, tokenOpts{})
	// A platform admin's session without the admin scope.
	nt, _ := auth.GenerateToken()
	if _, err := e.q.InsertAPIToken(ctx, dbq.InsertAPITokenParams{UserID: admin.ID, Kind: auth.KindSession, Name: "s",
		TokenHash: nt.Hash, Prefix: nt.Prefix, Scopes: []string{"read", "write"}}); err != nil {
		t.Fatal(err)
	}
	weakSession := e.asSession(nt.Plaintext)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := auth.NewAuthenticator(e.q, log)
	a.UseCookieNames(e.names)
	srv := httptest.NewServer(a.Authenticate(auth.RequirePlatformAdmin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))))
	defer srv.Close()
	cases := []struct {
		name, token string
		status      int
	}{
		{"member", memberTok, 403},
		{"org admin (pinned)", orgAdminTok, 403},
		{"org admin (unpinned, admin scope, no flag)", orgAdminUnpinned, 403},
		{"platform admin, unpinned admin PAT", unpinned, 204},
		{"platform admin, the org-pinned first token", body.Token.Secret, 403},
		{"platform admin, PAT without admin scope", readWrite, 403},
		{"platform admin, session without admin scope", weakSession, 403},
		{"platform admin, the setup session", session, 204},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", srv.URL, nil)
			e.authorize(req, tt.token)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
			if res.StatusCode != tt.status {
				t.Fatalf("status %d, want %d", res.StatusCode, tt.status)
			}
		})
	}
}

// TestLogin_PlatformAdminGetsTheAdminScope: a platform admin's login
// session has admin even with no org admin role, and a platform admin in
// no org can log in at all.
func TestLogin_PlatformAdminGetsTheAdminScope(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	email := "ops@example.com"
	u, err := e.q.InsertUser(ctx, dbq.InsertUserParams{Username: "ops", Name: "Ops", Email: &email})
	if err != nil {
		t.Fatal(err)
	}
	e.requestCode(email)
	if e.mail.last(email) != "" {
		t.Fatal("a user in no org got a code before the grant")
	}
	if _, err := setup.GrantPlatformAdmin(ctx, e.pool, "ops"); err != nil {
		t.Fatal(err)
	}
	session, r := e.loginAs(email)
	var body struct {
		User struct {
			IsPlatformAdmin bool `json:"isPlatformAdmin"`
		} `json:"user"`
		Orgs []any `json:"orgs"`
	}
	r.json(t, &body)
	var me tracker.Me
	e.must(200, e.do("GET", "/api/v1/me", session, nil)).json(t, &me)
	if !body.User.IsPlatformAdmin || len(body.Orgs) != 0 || !me.IsPlatformAdmin || !slices.Contains(me.Scopes, "admin") {
		t.Fatalf("login %+v me %+v", body, me)
	}
	// An org member (not admin) who is a platform admin: admin scope, and
	// RequireScope still refuses org-admin routes in that org.
	e.addMember(e.orgID, u.ID, "member")
	session, _ = e.loginAs(email)
	e.must(200, e.do("GET", "/api/v1/me", session, nil)).json(t, &me)
	if !slices.Contains(me.Scopes, "admin") {
		t.Fatalf("scopes %v", me.Scopes)
	}
	e.must(403, e.do("GET", "/api/v1/orgs/orbit/tokens", session, nil))
	// A plain member's session has no admin scope (unchanged).
	e.newHuman("pat", "pat@example.com", "member")
	s2, _ := e.loginAs("pat@example.com")
	e.must(200, e.do("GET", "/api/v1/me", s2, nil)).json(t, &me)
	if slices.Contains(me.Scopes, "admin") || me.IsPlatformAdmin {
		t.Fatalf("member %+v", me)
	}
}

// TestSetup_NoSecretsInLogsEventsOrIdempotency: the setup link line is the
// only credential in the logs; the link, the first token, the session and
// the CSRF token are in no event, idempotency row or log line besides it.
func TestSetup_NoSecretsInLogsEventsOrIdempotency(t *testing.T) {
	e := newEnvWith(t, envOpts{empty: true, logLevel: slog.LevelDebug})
	log := slog.New(slog.NewJSONHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tok := e.mintLink()
	setup.LogLink(log, "https://launchpad.example", tok)
	e.must(401, e.do("POST", "/api/v1/setup", "", setupBody(strings.Repeat("B", 43))))
	body, session := e.completeSetup(tok)
	pat := body.Token.Secret
	csrf := auth.CSRFToken(session)
	// Writes that emit events and store an idempotency row, with both.
	is := e.createIssue(pat, map[string]any{"title": "x"})
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues/"+is.Identifier+"/comments", pat, map[string]any{"body": "hi"}, "Idempotency-Key", "setup-key-1"))
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues/"+is.Identifier+"/comments", session, map[string]any{"body": "hi"}, "Idempotency-Key", "setup-key-2"))

	logs := e.logs.String()
	if !strings.Contains(logs, "/api/v1/setup") {
		t.Fatalf("the log capture saw no setup request; the check would be vacuous:\n%s", logs)
	}
	var linkLines []string
	for _, l := range strings.Split(logs, "\n") {
		if strings.Contains(l, tok) {
			linkLines = append(linkLines, l)
		}
	}
	if len(linkLines) != 1 || !strings.Contains(linkLines[0], setup.LogPrefix+"https://launchpad.example/setup#"+tok) {
		t.Fatalf("the setup token is in %d log lines, want only the link line: %q", len(linkLines), linkLines)
	}
	for name, v := range map[string]string{"first token": pat, "session": session, "csrf": csrf} {
		if strings.Contains(logs, v) {
			t.Errorf("the %s is in the logs", name)
		}
	}
	if strings.Contains(logs, "odt_") {
		t.Error("a token-shaped secret is in the logs")
	}
	var events, idem string
	if err := e.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(type || ' ' || payload::text, E'\n'), '') FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(context.Background(), `SELECT coalesce(string_agg(key || ' ' || path || ' ' || coalesce(convert_from(response_body, 'UTF8'), ''), E'\n'), '') FROM idempotency_keys`).Scan(&idem); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(events, "team.created") || !strings.Contains(idem, "setup-key-1") {
		t.Fatalf("expected rows are missing; the check would be vacuous:\nevents %s\nidem %s", events, idem)
	}
	for name, v := range map[string]string{"setup token": tok, "first token": pat, "session": session, "csrf": csrf} {
		if strings.Contains(events, v) {
			t.Errorf("the %s is in an event payload", name)
		}
		if strings.Contains(idem, v) {
			t.Errorf("the %s is in an idempotency row", name)
		}
	}
	if strings.Contains(idem, "/api/v1/setup") {
		t.Error("POST /setup left an idempotency row")
	}
	// The link is stored only as sha256.
	if n := e.countRows(`SELECT count(*) FROM setup_tokens WHERE position(convert_to($1, 'UTF8') in token_hash) > 0`, tok); n != 0 {
		t.Error("the setup token is stored in plaintext")
	}
	if n := e.countRows(`SELECT count(*) FROM setup_tokens WHERE token_hash = sha256(convert_to($1, 'UTF8'))`, tok); n != 1 {
		t.Errorf("setup_tokens holds %d rows with sha256(token), want 1", n)
	}
}
