package api

import (
	"context"
	"strings"
	"testing"

	"devops/internal/tracker"
)

// LAUN-200: agent guidance (docs/internal/decisions-T31-agent-guidance.md).

func (e *env) guidance(token, query string) tracker.Guidance {
	e.t.Helper()
	var g tracker.Guidance
	path := O + "/agent-guidance"
	if query != "" {
		path += "?" + query
	}
	e.must(200, e.do("GET", path, token, nil)).json(e.t, &g)
	return g
}

func (e *env) guidanceEvents() int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE org_id = $1 AND type = 'agent_guidance.updated'`, e.orgID).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestAgentGuidance_Auth(t *testing.T) {
	e := newEnv(t)
	ann := e.newHuman("ann", "ann@example.com", "member")
	annAdminScope, _ := e.newToken(ann.ID, tokenOpts{}) // admin scope, member role
	body := map[string]any{"text": "x"}
	for _, m := range []string{"PUT", "DELETE"} {
		expectCode(t, e.do(m, O+"/agent-guidance", "", body), 401, "unauthenticated")
		// The agent token has read and write scope: it cannot write guidance.
		expectCode(t, e.do(m, O+"/agent-guidance", e.claude, body), 403, "insufficient_scope")
		expectCode(t, e.do(m, O+"/agent-guidance", annAdminScope, body), 403, "forbidden")
	}
	expectCode(t, e.do("GET", O+"/agent-guidance", "", nil), 401, "unauthenticated")
	if e.guidanceEvents() != 0 {
		t.Fatal("a refused write emitted")
	}
}

func TestAgentGuidance_WriteReadClear(t *testing.T) {
	e := newEnv(t)
	if g := e.guidance(e.claude, ""); len(g.Items) != 0 {
		t.Fatalf("fresh org: %+v", g)
	}
	var in tracker.GuidanceEntry
	e.must(200, e.do("PUT", O+"/agent-guidance", e.chen, map[string]any{"text": "  Be brief.\n"})).json(t, &in)
	if in.Team != nil || in.Text != "Be brief." {
		t.Fatalf("org entry %+v", in)
	}
	e.must(200, e.do("PUT", O+"/agent-guidance", e.chen, map[string]any{"text": "Be brief.", "team": nil}))
	if e.guidanceEvents() != 1 {
		t.Fatalf("a same-text write emitted: %d events", e.guidanceEvents())
	}
	e.must(200, e.do("PUT", O+"/agent-guidance", e.chen, map[string]any{"text": "Use ORB labels.", "team": "orb"}))
	e.must(200, e.do("PUT", O+"/agent-guidance", e.chen, map[string]any{"text": "Be very brief."}))
	// An agent token reads all of it; the org entry comes first.
	g := e.guidance(e.claude, "")
	if len(g.Items) != 2 || g.Items[0].Team != nil || g.Items[0].Text != "Be very brief." ||
		g.Items[1].Team == nil || *g.Items[1].Team != "ORB" {
		t.Fatalf("list %+v", g)
	}
	if g := e.guidance(e.claude, "team=ORB"); len(g.Items) != 2 {
		t.Fatalf("team filter %+v", g)
	}
	// Delete is idempotent; a missing entry emits nothing.
	before := e.guidanceEvents()
	e.must(204, e.do("DELETE", O+"/agent-guidance?team=ORB", e.chen, nil))
	e.must(204, e.do("DELETE", O+"/agent-guidance?team=ORB", e.chen, nil))
	if e.guidanceEvents() != before+1 {
		t.Fatalf("events %d, want %d", e.guidanceEvents(), before+1)
	}
	e.must(204, e.do("DELETE", O+"/agent-guidance", e.chen, nil))
	if g := e.guidance(e.claude, ""); len(g.Items) != 0 {
		t.Fatalf("after delete: %+v", g)
	}
	// The event never holds the text.
	var leaked int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE type = 'agent_guidance.updated' AND payload::text ILIKE '%brief%'`).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("payload leaked text: %d %v", leaked, err)
	}
}

func TestAgentGuidance_Validation(t *testing.T) {
	e := newEnv(t)
	put := func(body map[string]any) resp { return e.do("PUT", O+"/agent-guidance", e.chen, body) }
	expectCode(t, put(map[string]any{"text": ""}), 400, "validation_failed")
	expectCode(t, put(map[string]any{"text": " \n "}), 400, "validation_failed")
	expectCode(t, put(map[string]any{"text": "a\u0000b"}), 400, "validation_failed")
	expectCode(t, put(map[string]any{"text": strings.Repeat("a", tracker.MaxGuidanceBytes+1)}), 400, "validation_failed")
	// Bytes, not characters: 2731 three-byte runes are 8193 bytes.
	expectCode(t, put(map[string]any{"text": strings.Repeat("€", 2731)}), 400, "validation_failed")
	e.must(200, put(map[string]any{"text": strings.Repeat("a", tracker.MaxGuidanceBytes)}))
	// Bidi controls and zero-width characters are refused, each one, with a fixed sentence.
	for _, r := range []rune{0x202A, 0x202B, 0x202C, 0x202D, 0x202E, 0x2066, 0x2067, 0x2068, 0x2069,
		0x200E, 0x200F, 0x200B, 0x200C, 0x200D, 0x2060, 0xFEFF} {
		r := put(map[string]any{"text": "ok " + string(r) + " hidden"})
		expectCode(t, r, 400, "validation_failed")
		if !strings.Contains(string(r.body), tracker.GuidanceHiddenText) {
			t.Fatalf("body %s", r.body)
		}
	}
	expectCode(t, put(map[string]any{"text": "x", "team": "NOPE"}), 404, "not_found")
	expectCode(t, put(map[string]any{"text": "x", "bogus": 1}), 400, "bad_request")
	expectCode(t, e.do("GET", O+"/agent-guidance?team=NOPE", e.chen, nil), 404, "not_found")
}

func TestAgentGuidance_CrossOrg(t *testing.T) {
	e := newEnv(t)
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "admin")
	tok, _ := e.newToken(chen.ID, tokenOpts{}) // unpinned: a member of both orgs
	const A = "/api/v1/orgs/acme"
	e.must(200, e.do("PUT", A+"/agent-guidance", tok, map[string]any{"text": "acme secret plan"}))
	e.must(200, e.do("PUT", A+"/agent-guidance", tok, map[string]any{"text": "acme team", "team": "ACM"}))
	// The orbit side sees nothing of acme.
	if g := e.guidance(tok, ""); len(g.Items) != 0 {
		t.Fatalf("leak %+v", g)
	}
	// A team of the other org is 404 in the query and the body, for every verb.
	expectCode(t, e.do("GET", O+"/agent-guidance?team=ACM", tok, nil), 404, "not_found")
	expectCode(t, e.do("PUT", O+"/agent-guidance", tok, map[string]any{"text": "x", "team": "ACM"}), 404, "not_found")
	expectCode(t, e.do("DELETE", O+"/agent-guidance?team=ACM", tok, nil), 404, "not_found")
	if g := e.guidance(tok, ""); len(g.Items) != 0 {
		t.Fatalf("write leaked across: %+v", g)
	}
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_guidance WHERE org_id = $1`, acmeID).Scan(&n); err != nil || n != 2 {
		t.Fatalf("acme rows %d %v", n, err)
	}
	// Composite FK: a row cannot name a team of another org.
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO agent_guidance (org_id, team_id, body) SELECT $1, id, 'x' FROM teams WHERE org_id = $2 LIMIT 1`, e.orgID, acmeID); err == nil {
		t.Fatal("a cross-org guidance row was accepted")
	}
}

// A member-role user on a session cookie (CSRF sent as usual) reads, but cannot write.
func TestAgentGuidance_MemberSession(t *testing.T) {
	e := newEnv(t)
	e.newHuman("mem", "mem@example.com", "member")
	web, _ := e.loginAs("mem@example.com")
	e.must(200, e.do("GET", O+"/agent-guidance", web, nil))
	expectCode(t, e.do("PUT", O+"/agent-guidance", web, map[string]any{"text": "x"}), 403, "insufficient_scope")
	expectCode(t, e.do("DELETE", O+"/agent-guidance", web, nil), 403, "insufficient_scope")
	if e.guidanceEvents() != 0 {
		t.Fatal("a refused write emitted")
	}
}

// Deleting a team through the API removes its guidance; the org entry stays.
func TestAgentGuidance_TeamDeleteCascade(t *testing.T) {
	e := newEnv(t)
	e.mkTeam("DEL")
	e.must(200, e.do("PUT", O+"/agent-guidance", e.chen, map[string]any{"text": "org text"}))
	e.must(200, e.do("PUT", O+"/agent-guidance", e.chen, map[string]any{"text": "del text", "team": "DEL"}))
	if g := e.guidance(e.chen, ""); len(g.Items) != 2 {
		t.Fatalf("before: %+v", g)
	}
	e.must(204, e.do("DELETE", O+"/teams/DEL", e.chen, nil))
	g := e.guidance(e.chen, "")
	if len(g.Items) != 1 || g.Items[0].Team != nil {
		t.Fatalf("after: %+v", g)
	}
	if n := e.countRows(`SELECT count(*) FROM agent_guidance WHERE team_id IS NOT NULL`); n != 0 {
		t.Fatalf("%d team rows left", n)
	}
}
