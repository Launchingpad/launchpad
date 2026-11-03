package api

import (
	"strings"
	"testing"
	"time"

	"devops/internal/tracker"
)

// LAUN-172 (decisions-T27): the cycle options over HTTP. The rules and the
// race with the job are in internal/tracker/cycle_options_integration_test.go.

// optionsEnv: cycles on for ORB with the clock fixed on Wed 2026-10-21 12:00
// UTC: cycle 1 is 10-18 .. 11-01 (current), cycle 2 is 11-01 .. 11-15.
func optionsEnv(t *testing.T) (*env, []tracker.Cycle) {
	t.Helper()
	e := newEnvWith(t, envOpts{now: func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) }})
	e.enableCycles(e.chen, "ORB")
	cs := e.cyclesOf(e.chen, "team=ORB")
	if len(cs) != 2 {
		t.Fatalf("%d cycles", len(cs))
	}
	return e, []tracker.Cycle{cs[1], cs[0]} // current first
}

func TestCycleOptions_HTTP_Dates(t *testing.T) {
	e, c := optionsEnv(t)
	P := O + "/cycles/"
	var got tracker.Cycle
	e.must(200, e.do("PATCH", P+c[0].ID.String(), e.chen, map[string]any{"endsAt": "2026-11-05", "name": "Sprint"})).json(t, &got)
	if got.EndsAt.Format("2006-01-02") != "2026-11-05" || got.Name == nil || *got.Name != "Sprint" {
		t.Fatalf("cycle %+v", got)
	}
	e.must(200, e.do("GET", P+c[1].ID.String(), e.chen, nil)).json(t, &got)
	if got.StartsAt.Format("2006-01-02") != "2026-11-05" {
		t.Fatalf("next cycle starts %s", got.StartsAt)
	}
	// Refusals carry the code and a message.
	r := e.do("PATCH", P+c[0].ID.String(), e.chen, map[string]any{"startsAt": "2026-10-19"})
	expectCode(t, r, 409, "cycle_conflict")
	if !strings.Contains(string(r.body), "has started") {
		t.Fatalf("message %s", r.body)
	}
	expectCode(t, e.do("PATCH", P+c[0].ID.String(), e.chen, map[string]any{"endsAt": "soon"}), 400, "validation_failed")
	expectCode(t, e.do("PATCH", P+c[0].ID.String(), e.chen, map[string]any{"endsAt": nil}), 400, "validation_failed")
	expectCode(t, e.do("PATCH", P+c[0].ID.String(), e.chen, map[string]any{"endsAt": "2026-10-20"}), 400, "validation_failed")
	if e.eventCount("cycle.updated") != 2 {
		t.Fatalf("%d cycle.updated events, want 2 (the cycle and its neighbour)", e.eventCount("cycle.updated"))
	}
}

func TestCycleOptions_HTTP_CompleteAndDelete(t *testing.T) {
	e, c := optionsEnv(t)
	P := O + "/cycles/"
	// Upcoming: not completable.
	expectCode(t, e.do("POST", P+c[1].ID.String()+"/complete", e.chen, nil), 409, "cycle_conflict")

	r1 := e.must(200, e.do("POST", P+c[0].ID.String()+"/complete", e.chen, nil, "Idempotency-Key", "complete-1"))
	r2 := e.must(200, e.do("POST", P+c[0].ID.String()+"/complete", e.chen, nil, "Idempotency-Key", "complete-1"))
	if string(r1.body) != string(r2.body) {
		t.Fatalf("a retry with the same key must replay:\n%s\n%s", r1.body, r2.body)
	}
	var done tracker.Cycle
	r1.json(t, &done)
	if done.CompletedAt == nil || !done.CompletedAt.Equal(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("completedAt %v", done.CompletedAt)
	}
	// Without the key, a second call is a plain 409.
	expectCode(t, e.do("POST", P+c[0].ID.String()+"/complete", e.chen, nil), 409, "cycle_conflict")

	// Delete: the next cycle (now current) is the first of two; make a third to delete it.
	cs := e.cyclesOf(e.chen, "team=ORB")
	if len(cs) != 3 {
		t.Fatalf("%d cycles after complete, want 3", len(cs))
	}
	is := e.createIssue(e.chen, map[string]any{"title": "in the upcoming cycle", "team": "ORB", "cycleId": cs[0].ID})
	r := e.do("DELETE", P+cs[0].ID.String(), e.chen, nil)
	expectCode(t, r, 409, "cycle_in_use")
	if !strings.Contains(string(r.body), "including archived issues") {
		t.Fatalf("message %s", r.body)
	}
	e.must(200, e.do("PATCH", O+"/issues/"+is.Identifier, e.chen, map[string]any{"cycleId": nil}))
	// cs[0] is the last cycle: the job would make it again.
	expectCode(t, e.do("DELETE", P+cs[0].ID.String(), e.chen, nil), 409, "cycle_conflict")
	e.must(204, e.do("DELETE", P+cs[1].ID.String(), e.chen, nil)) // the current one: the upcoming takes over
	expectCode(t, e.do("GET", P+cs[1].ID.String(), e.chen, nil), 404, "not_found")
	expectCode(t, e.do("DELETE", P+cs[1].ID.String(), e.chen, nil), 404, "not_found")
	if e.eventCount("cycle.deleted") != 1 {
		t.Fatal("want one cycle.deleted")
	}
}

func TestCycleOptions_HTTP_Scopes(t *testing.T) {
	e, c := optionsEnv(t)
	readTok, _ := e.newToken(e.user("chen").ID, tokenOpts{scopes: []string{"read"}})
	P := O + "/cycles/" + c[1].ID.String()
	expectCode(t, e.do("PATCH", P, readTok, map[string]any{"endsAt": "2026-11-20"}), 403, "insufficient_scope")
	expectCode(t, e.do("POST", P+"/complete", readTok, nil), 403, "insufficient_scope")
	expectCode(t, e.do("DELETE", P, readTok, nil), 403, "insufficient_scope")
	// Complete and delete are admin scope; a write token (and the agent) can still rename and re-date.
	writeTok, _ := e.newToken(e.user("chen").ID, tokenOpts{scopes: []string{"read", "write"}})
	expectCode(t, e.do("POST", P+"/complete", writeTok, nil), 403, "insufficient_scope")
	expectCode(t, e.do("DELETE", P, writeTok, nil), 403, "insufficient_scope")
	expectCode(t, e.do("POST", P+"/complete", e.claude, nil), 403, "insufficient_scope")
	expectCode(t, e.do("DELETE", P, e.claude, nil), 403, "insufficient_scope")
	e.must(200, e.do("PATCH", P, writeTok, map[string]any{"name": "by a write token"}))
	expectCode(t, e.do("PATCH", O+"/cycles/not-a-uuid", e.chen, map[string]any{"endsAt": "2026-11-20"}), 404, "not_found")
	if e.eventCount("cycle.deleted") != 0 || e.eventCount("cycle.completed") != 0 {
		t.Fatal("a refused call wrote an event")
	}
}

func TestCycleOptions_HTTP_CrossOrg(t *testing.T) {
	e, c := optionsEnv(t)
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "admin")
	tok, _ := e.newToken(chen.ID, tokenOpts{}) // unpinned: a member of both orgs
	const A = "/api/v1/orgs/acme"
	e.must(200, e.do("PATCH", A+"/teams/ACM", tok, map[string]any{"cyclesEnabled": true}))
	var p page[tracker.Cycle]
	e.must(200, e.do("GET", A+"/cycles?team=ACM", tok, nil)).json(t, &p)
	if len(p.Items) != 2 {
		t.Fatalf("%d acme cycles", len(p.Items))
	}
	ac := p.Items[0]

	// An acme cycle through the orbit path, and an orbit cycle through the acme path: 404, nothing changes.
	for _, tc := range []struct{ method, path string }{
		{"PATCH", O + "/cycles/" + ac.ID.String()},
		{"POST", O + "/cycles/" + ac.ID.String() + "/complete"},
		{"DELETE", O + "/cycles/" + ac.ID.String()},
		{"PATCH", A + "/cycles/" + c[0].ID.String()},
		{"POST", A + "/cycles/" + c[0].ID.String() + "/complete"},
		{"DELETE", A + "/cycles/" + c[1].ID.String()},
	} {
		var body any
		if tc.method == "PATCH" {
			body = map[string]any{"endsAt": "2026-11-20"}
		}
		expectCode(t, e.do(tc.method, tc.path, tok, body), 404, "not_found")
	}
	if n := e.countRows(`SELECT count(*) FROM cycles`); n != 4 {
		t.Fatalf("%d cycles, want 4", n)
	}
	if n := e.countRows(`SELECT count(*) FROM cycles WHERE completed_at IS NOT NULL`); n != 0 {
		t.Fatal("a cross-org call completed a cycle")
	}
}
