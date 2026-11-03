package api

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"devops/internal/tracker"
)

// LAUN-56: the first-run checklist and the org settings, against the real
// router and Postgres (docs/internal/decisions-T17-onboarding.md).

func (e *env) onboarding(token string) tracker.Onboarding {
	e.t.Helper()
	var o tracker.Onboarding
	e.must(200, e.do("GET", O+"/onboarding", token, nil)).json(e.t, &o)
	return o
}

func stepOf(t *testing.T, o tracker.Onboarding, id string) tracker.OnboardingStep {
	t.Helper()
	for _, s := range o.Steps {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no step %s in %+v", id, o.Steps)
	return tracker.OnboardingStep{}
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatal(err)
	}
}

func TestOnboarding_Auth(t *testing.T) {
	e := newEnv(t)
	ann := e.newHuman("ann", "ann@example.com", "member")
	annAdminScope, _ := e.newToken(ann.ID, tokenOpts{}) // admin scope, member role
	chen := e.user("chen")
	readOnly, _ := e.newToken(chen.ID, tokenOpts{scopes: []string{"read", "write"}}) // org admin, no admin scope
	routes := []struct {
		method, path string
		body         any
	}{
		{"GET", O + "/onboarding", nil},
		{"PUT", O + "/onboarding/skip", map[string]any{"step": "mail", "skipped": true}},
		{"PATCH", O, map[string]any{"name": "X"}},
	}
	for _, rt := range routes {
		expectCode(t, e.do(rt.method, rt.path, "", rt.body), 401, "unauthenticated")
		expectCode(t, e.do(rt.method, rt.path, e.claude, rt.body), 403, "insufficient_scope")
		expectCode(t, e.do(rt.method, rt.path, readOnly, rt.body), 403, "insufficient_scope")
		expectCode(t, e.do(rt.method, rt.path, annAdminScope, rt.body), 403, "forbidden")
	}
	if n := e.eventCount("org.updated") + e.eventCount("onboarding.step_changed"); n != 0 {
		t.Fatalf("a refused write emitted %d events", n)
	}
}

// The done flags come from the data.
func TestOnboarding_DoneFlags(t *testing.T) {
	e := newEnv(t)
	done := func(o tracker.Onboarding) map[string]bool {
		m := map[string]bool{}
		for _, s := range o.Steps {
			m[s.ID] = s.Done
		}
		return m
	}
	o := e.onboarding(e.chen)
	var ids []string
	for _, s := range o.Steps {
		ids = append(ids, s.ID)
	}
	// One team: the optional "products" step (LAUN-67) is not listed.
	if want := []string{"organisation", "mail", "signups", "team", "agent", "import", "githost", "invite"}; !slices.Equal(ids, want) {
		t.Fatalf("steps %v", ids)
	}
	d := done(o)
	// The fixture: org made, claude (agent) has a token; no mail, no cycles, one human.
	if !d["organisation"] || d["mail"] || d["signups"] || d["team"] || !d["agent"] || d["import"] || d["githost"] || d["invite"] {
		t.Fatalf("fixture flags %v", d)
	}

	// team: cycles on for a team.
	e.must(200, e.do("PATCH", O+"/teams/ORB", e.chen, map[string]any{"cyclesEnabled": true}))
	// invite: a second human; a deactivated one and an agent do not count.
	bob := e.newHuman("bob", "bob@example.com", "member")
	gone := e.newHuman("gone", "gone@example.com", "member")
	e.exec(`UPDATE users SET deactivated_at = now() WHERE id = $1`, gone.ID)
	d = done(e.onboarding(e.chen))
	if !d["team"] || !d["invite"] {
		t.Fatalf("after cycles and a member: %v", d)
	}
	e.exec(`UPDATE users SET deactivated_at = now() WHERE id = $1`, bob.ID)
	if done(e.onboarding(e.chen))["invite"] {
		t.Fatal("a deactivated member counted")
	}

	// agent: revoked and expired tokens do not count; a live one does.
	e.exec(`UPDATE api_tokens SET revoked_at = now() WHERE user_id = $1`, e.user("claude").ID)
	if done(e.onboarding(e.chen))["agent"] {
		t.Fatal("a revoked agent token counted")
	}
	past := time.Now().Add(-time.Hour)
	e.newToken(e.user("claude").ID, tokenOpts{expires: &past})
	if done(e.onboarding(e.chen))["agent"] {
		t.Fatal("an expired agent token counted")
	}
	future := time.Now().Add(24 * time.Hour)
	e.newToken(e.user("claude").ID, tokenOpts{expires: &future})
	if !done(e.onboarding(e.chen))["agent"] {
		t.Fatal("a live agent token did not count")
	}
	// A human's token is not an agent token.
	e.exec(`UPDATE api_tokens SET revoked_at = now() WHERE user_id = $1`, e.user("claude").ID)
	e.newToken(e.user("chen").ID, tokenOpts{})
	if done(e.onboarding(e.chen))["agent"] {
		t.Fatal("a human token counted as an agent token")
	}

	// githost: an enabled integration; import: a mapping.
	e.exec(`INSERT INTO git_integrations (org_id, provider, base_url, webhook_secret_hash) VALUES ($1, 'gitlab', 'https://git.example.com', $2)`,
		e.orgID, make([]byte, 32))
	e.exec(`INSERT INTO import_mappings (org_id, entity_type, source_id, target_id) VALUES ($1, 'team', 'x', $1)`, e.orgID)
	d = done(e.onboarding(e.chen))
	if !d["githost"] || !d["import"] {
		t.Fatalf("githost/import %v", d)
	}
}

// AC: a non-platform org admin sees correct mail and sign-up flags, and the
// body holds nothing else of the platform.
func TestOnboarding_PlatformFlagsForOrgAdmin(t *testing.T) {
	pe := newPlatEnv(t, platOpts{key: platKey1})
	bob := pe.newHuman("bob", "bob@example.com", "admin")
	bobTok, _ := pe.newToken(bob.ID, tokenOpts{pin: &pe.orgID})
	if r := pe.do("GET", "/api/v1/platform/settings", bobTok, nil); r.status != 403 {
		t.Fatalf("bob reads platform settings: %d", r.status)
	}
	flags := func() (mail, signups bool) {
		o := pe.onboarding(bobTok)
		return stepOf(t, o, "mail").Done, stepOf(t, o, "signups").Done
	}
	if m, s := flags(); m || s {
		t.Fatalf("start: mail %v signups %v", m, s)
	}
	// SMTP with a password: mail works. The sign-up mode is not chosen by a mail save.
	pe.must(200, pe.put(pe.admin, smtpBody("smtpPassword", platPassword)))
	if m, s := flags(); !m || s {
		t.Fatalf("after mail: mail %v signups %v", m, s)
	}
	if pe.settings(pe.admin).SignupModeChosen {
		t.Fatal("a mail save marked the mode as chosen")
	}
	// A mail save that keeps the mode and sends no confirm: still not chosen.
	pe.must(200, pe.put(pe.admin, smtpBody("smtpFromName", "Other")))
	if _, s := flags(); s {
		t.Fatal("a second mail save marked the mode as chosen")
	}
	// A confirm of the same mode is a choice.
	pe.must(200, pe.put(pe.admin, smtpBody("signupModeConfirmed", true)))
	if _, s := flags(); !s || !pe.settings(pe.admin).SignupModeChosen {
		t.Fatal("a confirm did not mark the mode as chosen")
	}
	// A later plain save does not clear it.
	pe.must(200, pe.put(pe.admin, smtpBody("signupModeConfirmed", false)))
	if _, s := flags(); !s {
		t.Fatal("a plain save cleared the marker")
	}
	// Mail off again: the flag follows.
	pe.must(200, pe.put(pe.admin, platBody("mailProvider", "none")))
	if m, _ := flags(); m {
		t.Fatal("mail none still counts as working")
	}
}

func TestOnboarding_SignupModeChangeIsAChoice(t *testing.T) {
	pe := newPlatEnv(t, platOpts{key: platKey1})
	pe.must(200, pe.put(pe.admin, platBody("signupMode", "approval")))
	if !stepOf(t, pe.onboarding(pe.env.chen), "signups").Done {
		t.Fatal("changing the mode did not count as a choice")
	}
}

// The body is booleans and ids: no secret, no setting, no provider name.
func TestOnboarding_NoSecretsOrPlatformSettings(t *testing.T) {
	pe := newPlatEnv(t, platOpts{key: platKey1})
	pe.must(200, pe.put(pe.admin, smtpBody("smtpPassword", platPassword)))
	r := pe.must(200, pe.do("GET", O+"/onboarding", pe.env.chen, nil))
	pe.must(200, pe.do("PUT", O+"/onboarding/skip", pe.env.chen, map[string]any{"step": "mail", "skipped": true}))
	r2 := pe.do("GET", O+"/onboarding", pe.env.chen, nil)
	for _, s := range []string{string(r.body), string(r2.body), pe.logs.String()} {
		noPlanted(t, "onboarding", s)
		for _, bad := range []string{platUser, "mail.test", "tracker@example.com", "smtp", "postmark", "Provider", "signupMode"} {
			if strings.Contains(s, bad) && s != pe.logs.String() {
				t.Fatalf("onboarding body holds %q: %s", bad, s)
			}
		}
	}
	noPlanted(t, "logs", pe.logs.String())
	// The events hold the step and the flag only.
	var payload string
	if err := pe.pool.QueryRow(context.Background(), `SELECT payload::text FROM events WHERE type = 'onboarding.step_changed'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if payload != `{"step": "mail", "skipped": true}` {
		t.Fatalf("event payload %s", payload)
	}
}

func TestOnboarding_SkipSharedByAdmins(t *testing.T) {
	e := newEnv(t)
	bob := e.newHuman("bob", "bob@example.com", "admin")
	bobTok, _ := e.newToken(bob.ID, tokenOpts{pin: &e.orgID})
	skip := func(tok, step string, v bool) resp {
		return e.do("PUT", O+"/onboarding/skip", tok, map[string]any{"step": step, "skipped": v})
	}
	var o tracker.Onboarding
	before := e.onboarding(e.chen).Remaining
	e.must(200, skip(e.chen, "mail", true)).json(t, &o)
	st := stepOf(t, o, "mail")
	if !st.Skipped || st.SkippedBy == nil || st.SkippedBy.Username != "chen" || st.SkippedAt == nil || o.Remaining != before-1 {
		t.Fatalf("after skip %+v remaining %d (was %d)", st, o.Remaining, before)
	}
	// Another admin sees it.
	if got := stepOf(t, e.onboarding(bobTok), "mail"); !got.Skipped || got.SkippedBy == nil || got.SkippedBy.Username != "chen" {
		t.Fatalf("bob sees %+v", got)
	}
	// A repeat changes nothing and emits nothing; so does the undo of an unskipped step.
	e.must(200, skip(bobTok, "mail", true))
	e.must(200, skip(bobTok, "signups", false))
	if n := e.eventCount("onboarding.step_changed"); n != 1 {
		t.Fatalf("%d events after a repeat", n)
	}
	if got := stepOf(t, e.onboarding(e.chen), "mail"); got.SkippedBy.Username != "chen" {
		t.Fatalf("a repeat changed the skipper: %+v", got)
	}
	// Undo by the other admin.
	e.must(200, skip(bobTok, "mail", false)).json(t, &o)
	if stepOf(t, o, "mail").Skipped || e.eventCount("onboarding.step_changed") != 2 {
		t.Fatalf("undo: %+v events %d", o.Steps, e.eventCount("onboarding.step_changed"))
	}
	// A done step can be skipped and stays done.
	e.must(200, skip(e.chen, "organisation", true)).json(t, &o)
	if s := stepOf(t, o, "organisation"); !s.Skipped || !s.Done {
		t.Fatalf("%+v", s)
	}
	// Bad bodies.
	expectCode(t, skip(e.chen, "nope", true), 400, "validation_failed")
	expectCode(t, skip(e.chen, "", true), 400, "validation_failed")
	expectCode(t, e.do("PUT", O+"/onboarding/skip", e.chen, map[string]any{"step": "mail"}), 400, "validation_failed")
	expectCode(t, e.do("PUT", O+"/onboarding/skip", e.chen, map[string]any{"step": "mail", "skipped": true, "x": 1}), 400, "bad_request")
	r, err := e.request("PUT", O+"/onboarding/skip", e.chen, "step=mail", "Content-Type", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	expectCode(t, r, 415, "unsupported_media_type")
}

func TestOnboarding_SkipIdempotencyKey(t *testing.T) {
	e := newEnv(t)
	body := map[string]any{"step": "team", "skipped": true}
	a := e.must(200, e.do("PUT", O+"/onboarding/skip", e.chen, body, "Idempotency-Key", "k-skip-1"))
	b := e.must(200, e.do("PUT", O+"/onboarding/skip", e.chen, body, "Idempotency-Key", "k-skip-1"))
	if string(a.body) != string(b.body) || e.eventCount("onboarding.step_changed") != 1 {
		t.Fatalf("replay differs or emitted twice: %s / %s", a.body, b.body)
	}
}

func TestOnboarding_Capabilities(t *testing.T) {
	e := newEnv(t)
	caps := e.onboarding(e.chen).Capabilities
	// No import manager and no attachment store in the plain harness.
	if !slices.Equal(caps, []string{"gitHosts"}) {
		t.Fatalf("capabilities %v", caps)
	}
	// An import step with no capability is not counted in remaining.
	o := e.onboarding(e.chen)
	open := 0
	for _, s := range o.Steps {
		if !s.Done && !s.Skipped && s.ID != "import" {
			open++
		}
	}
	if o.Remaining != open {
		t.Fatalf("remaining %d, want %d", o.Remaining, open)
	}
}

func TestOnboarding_CrossOrg(t *testing.T) {
	e := newEnv(t)
	acmeID := e.newOrg("acme", "ACM")
	dave := e.newUser("dave")
	e.addMember(acmeID, dave.ID, "admin")
	daveTok, _ := e.newToken(dave.ID, tokenOpts{})
	const A = "/api/v1/orgs/acme"
	// Dave is an admin of acme only: orbit is 404 on every route.
	expectCode(t, e.do("GET", O+"/onboarding", daveTok, nil), 404, "not_found")
	expectCode(t, e.do("PUT", O+"/onboarding/skip", daveTok, map[string]any{"step": "mail", "skipped": true}), 404, "not_found")
	expectCode(t, e.do("PATCH", O, daveTok, map[string]any{"name": "Hacked"}), 404, "not_found")
	// A skip in acme is not a skip in orbit, and the facts are acme's own.
	e.must(200, e.do("PUT", A+"/onboarding/skip", daveTok, map[string]any{"step": "mail", "skipped": true}))
	if stepOf(t, e.onboarding(e.chen), "mail").Skipped {
		t.Fatal("acme's skip shows in orbit")
	}
	acme := func() tracker.Onboarding {
		var o tracker.Onboarding
		e.must(200, e.do("GET", A+"/onboarding", daveTok, nil)).json(t, &o)
		return o
	}()
	if stepOf(t, acme, "agent").Done || stepOf(t, acme, "team").Done || stepOf(t, acme, "invite").Done {
		t.Fatalf("acme sees orbit's data: %+v", acme.Steps)
	}
	// Orbit's agent token does not make acme's agent step done, and a team of
	// acme cannot be orbit's default.
	expectCode(t, e.do("PATCH", O, e.chen, map[string]any{"defaultTeam": "ACM"}), 400, "validation_failed")
	var acmeTeam tracker.Team
	e.must(200, e.do("GET", A+"/teams/ACM", daveTok, nil)).json(t, &acmeTeam)
	expectCode(t, e.do("PATCH", O, e.chen, map[string]any{"defaultTeam": acmeTeam.ID.String()}), 400, "validation_failed")
	if e.eventCount("org.updated") != 0 {
		t.Fatal("a refused PATCH emitted an event")
	}
}

func TestOrgPatch(t *testing.T) {
	e := newEnv(t)
	var o tracker.Org
	e.must(200, e.do("GET", O, e.chen, nil)).json(t, &o)
	if o.TimeZone != "UTC" || o.DefaultTeam != nil || o.Slug != "orbit" || o.CreatedAt.IsZero() {
		t.Fatalf("defaults %+v", o)
	}
	e.must(200, e.do("PATCH", O, e.chen, map[string]any{"name": "  Orbit Labs ", "timeZone": "Europe/London", "defaultTeam": "orb"})).json(t, &o)
	if o.Name != "Orbit Labs" || o.TimeZone != "Europe/London" || o.DefaultTeam == nil || *o.DefaultTeam != "ORB" || o.Slug != "orbit" || o.Role != "admin" {
		t.Fatalf("after patch %+v", o)
	}
	if n := e.eventCount("org.updated"); n != 1 {
		t.Fatalf("%d org.updated events", n)
	}
	var payload string
	if err := e.pool.QueryRow(context.Background(), `SELECT payload::text FROM events WHERE type = 'org.updated'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, `"changes": ["name", "timeZone", "defaultTeam"]`) {
		t.Fatalf("payload %s", payload)
	}
	// The same values change nothing and emit nothing.
	e.must(200, e.do("PATCH", O, e.chen, map[string]any{"name": "Orbit Labs", "timeZone": "Europe/London", "defaultTeam": "ORB"}))
	if n := e.eventCount("org.updated"); n != 1 {
		t.Fatalf("a no-op PATCH emitted: %d", n)
	}
	// The list shows the settings too.
	var l struct{ Items []tracker.Org }
	e.must(200, e.do("GET", "/api/v1/orgs", e.chen, nil)).json(t, &l)
	if len(l.Items) != 1 || l.Items[0].TimeZone != "Europe/London" || *l.Items[0].DefaultTeam != "ORB" {
		t.Fatalf("list %+v", l.Items)
	}
	// Clear the default team.
	e.must(200, e.do("PATCH", O, e.chen, map[string]any{"defaultTeam": nil})).json(t, &o)
	if o.DefaultTeam != nil {
		t.Fatalf("default team not cleared %+v", o)
	}
	// Rejected.
	for name, body := range map[string]map[string]any{
		"slug":         {"slug": "other"},
		"empty":        {},
		"name empty":   {"name": "   "},
		"name null":    {"name": nil},
		"name long":    {"name": strings.Repeat("x", 201)},
		"zone unknown": {"timeZone": "Mars/Base"},
		"zone Local":   {"timeZone": "Local"},
		"zone empty":   {"timeZone": ""},
		"zone null":    {"timeZone": nil},
		"zone path":    {"timeZone": "../etc/passwd"},
		"team unknown": {"defaultTeam": "NOPE"},
	} {
		want := "validation_failed"
		if name == "slug" {
			want = "bad_request" // an unknown field
		}
		if r := e.do("PATCH", O, e.chen, body); r.status != 400 || r.errCode(t) != want {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
	}
	r, err := e.request("PATCH", O, e.chen, "name=x", "Content-Type", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	expectCode(t, r, 415, "unsupported_media_type")
	if n := e.eventCount("org.updated"); n != 2 { // the create and the clear
		t.Fatalf("%d org.updated events at the end", n)
	}
}

// A deleted default team clears the default; it never blocks.
func TestOrgPatch_DefaultTeamDeleted(t *testing.T) {
	e := newEnv(t)
	e.must(200, e.do("PATCH", O, e.chen, map[string]any{"defaultTeam": "ORB"}))
	e.exec(`DELETE FROM teams WHERE org_id = $1`, e.orgID)
	var o tracker.Org
	e.must(200, e.do("GET", O, e.chen, nil)).json(t, &o)
	if o.DefaultTeam != nil {
		t.Fatalf("%+v", o)
	}
}

// LAUN-67: the optional "products" step shows only for two or more teams and no
// product, never counts in remaining, and can be skipped.
func TestOnboarding_ProductsStep(t *testing.T) {
	e := newEnv(t)
	has := func(o tracker.Onboarding) bool {
		for _, s := range o.Steps {
			if s.ID == "products" {
				return true
			}
		}
		return false
	}
	o := e.onboarding(e.chen)
	if has(o) {
		t.Fatal("one team: the products step is listed")
	}
	before := o.Remaining
	e.must(201, e.do("POST", O+"/teams", e.chen, map[string]any{"key": "WEB", "name": "Web"}))
	o = e.onboarding(e.chen)
	if !has(o) {
		t.Fatal("two teams, no product: the step is missing")
	}
	if st := stepOf(t, o, "products"); st.Done || st.Skipped {
		t.Fatalf("step %+v", st)
	}
	if o.Remaining != before {
		t.Fatalf("remaining %d, want %d: an optional step counted", o.Remaining, before)
	}
	// A skip is stored (the CHECK of migration 0020) and shared.
	e.must(200, e.do("PUT", O+"/onboarding/skip", e.chen, map[string]any{"step": "products", "skipped": true}))
	if !stepOf(t, e.onboarding(e.chen), "products").Skipped {
		t.Fatal("skip not stored")
	}
	// A product ends the step.
	e.must(201, e.do("POST", O+"/products", e.chen, map[string]any{"name": "Prod", "teams": []string{"WEB"}}))
	if has(e.onboarding(e.chen)) {
		t.Fatal("a product exists: the step is still listed")
	}
}

func TestOnboarding_ProductsStep_CrossOrg(t *testing.T) {
	e := newEnv(t)
	acmeID := e.newOrg("acme", "ACM")
	dave := e.newUser("dave")
	e.addMember(acmeID, dave.ID, "admin")
	daveTok, _ := e.newToken(dave.ID, tokenOpts{})
	// Orbit has two teams and acme has one: only orbit lists the step.
	e.must(201, e.do("POST", O+"/teams", e.chen, map[string]any{"key": "WEB", "name": "Web"}))
	var acme tracker.Onboarding
	e.must(200, e.do("GET", "/api/v1/orgs/acme/onboarding", daveTok, nil)).json(t, &acme)
	for _, s := range acme.Steps {
		if s.ID == "products" {
			t.Fatal("acme sees orbit's teams")
		}
	}
	// Acme's product does not end orbit's step.
	e.must(201, e.do("POST", "/api/v1/orgs/acme/products", daveTok, map[string]any{"name": "A", "teams": []string{"ACM"}}))
	stepOf(t, e.onboarding(e.chen), "products")
}
