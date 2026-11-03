package api

import (
	"context"
	"strings"
	"testing"

	"devops/internal/notify"
)

// The caller's display preferences (LAUN-235, docs/internal/decisions-T42-themes.md).

const prefPath = "/api/v1/me/preferences"

func (e *env) prefs(token string) notify.Preferences {
	e.t.Helper()
	var p notify.Preferences
	e.must(200, e.do("GET", prefPath, token, nil)).json(e.t, &p)
	return p
}

func TestPreferences_StoreAndDefault(t *testing.T) {
	e := newEnv(t)
	if p := e.prefs(e.chen); p.Theme != "linear" {
		t.Fatalf("default = %+v", p)
	}
	for _, th := range []string{"paper", "terminal", "linear", "terminal"} {
		var p notify.Preferences
		e.must(200, e.do("PUT", prefPath, e.chen, map[string]any{"theme": th})).json(t, &p)
		if p.Theme != th {
			t.Fatalf("put %s answered %+v", th, p)
		}
		if g := e.prefs(e.chen); g.Theme != th {
			t.Fatalf("get after %s = %+v", th, g)
		}
	}
	// A repeat is a no-op.
	e.must(200, e.do("PUT", prefPath, e.chen, map[string]any{"theme": "terminal"}))
	if n := e.countRows(`SELECT count(*) FROM user_preferences`); n != 1 {
		t.Fatalf("rows %d", n)
	}
	// Own row only: another person still has the default.
	pat := e.newHuman("pat", "pat@example.com", "member")
	patTok, _ := e.newToken(pat.ID, tokenOpts{pin: &e.orgID})
	if p := e.prefs(patTok); p.Theme != "linear" {
		t.Fatalf("other person sees %+v", p)
	}
	// No event.
	if n := e.countRows(`SELECT count(*) FROM events WHERE type LIKE '%pref%' OR type LIKE '%theme%'`); n != 0 {
		t.Fatalf("events %d", n)
	}
}

func TestPreferences_Validation(t *testing.T) {
	e := newEnv(t)
	for _, body := range []any{
		map[string]any{"theme": "studio"},
		map[string]any{"theme": "Linear"},
		map[string]any{"theme": ""},
		map[string]any{"theme": nil},
		map[string]any{},
	} {
		r := e.do("PUT", prefPath, e.chen, body)
		if r.status != 400 || r.errCode(t) != "validation_failed" {
			t.Fatalf("%v: %d %s", body, r.status, r.body)
		}
	}
	// A body of the wrong shape is refused by the decoder.
	for _, body := range []any{map[string]any{"theme": 3}, map[string]any{"theme": "paper", "extra": true}} {
		r := e.do("PUT", prefPath, e.chen, body)
		if r.status != 400 || r.errCode(t) != "bad_request" {
			t.Fatalf("%v: %d %s", body, r.status, r.body)
		}
	}
	if p := e.prefs(e.chen); p.Theme != "linear" {
		t.Fatalf("a refused write was stored: %+v", p)
	}
	if n := e.countRows(`SELECT count(*) FROM user_preferences`); n != 0 {
		t.Fatalf("rows %d", n)
	}
	// The CHECK is the backstop.
	chen := e.user("chen")
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO user_preferences (user_id, theme) VALUES ($1, 'studio')`, chen.ID); err == nil {
		t.Fatal("CHECK let studio in")
	}
}

func TestPreferences_AgentsAndScopes(t *testing.T) {
	e := newEnv(t)
	for _, m := range []string{"GET", "PUT"} {
		r := e.do(m, prefPath, e.claude, map[string]any{"theme": "paper"})
		if r.status != 403 || r.errCode(t) != "forbidden" || !strings.Contains(string(r.body), "display preferences are not available to agent users") {
			t.Fatalf("agent %s: %d %s", m, r.status, r.body)
		}
	}
	if n := e.countRows(`SELECT count(*) FROM user_preferences`); n != 0 {
		t.Fatalf("agent stored a row: %d", n)
	}
	chen := e.user("chen")
	ro, _ := e.newToken(chen.ID, tokenOpts{pin: &e.orgID, scopes: []string{"read"}})
	e.must(200, e.do("GET", prefPath, ro, nil))
	if r := e.do("PUT", prefPath, ro, map[string]any{"theme": "paper"}); r.status != 403 || r.errCode(t) != "insufficient_scope" {
		t.Fatalf("read token: %d %s", r.status, r.body)
	}
	e.must(401, e.do("GET", prefPath, "", nil))
	e.must(401, e.do("PUT", prefPath, "", map[string]any{"theme": "paper"}))
}

// The theme is per person and install-wide: the same value through a token of
// either org, and a person in another org never sees or changes it.
func TestPreferences_CrossOrg(t *testing.T) {
	e := newEnv(t)
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "member")
	acmeTok, _ := e.newToken(chen.ID, tokenOpts{pin: &acmeID})
	e.must(200, e.do("PUT", prefPath, e.chen, map[string]any{"theme": "paper"}))
	if p := e.prefs(acmeTok); p.Theme != "paper" {
		t.Fatalf("acme token sees %+v", p)
	}
	// Another person.
	sam := e.newHuman("sam", "sam@example.com", "member")
	samTok, _ := e.newToken(sam.ID, tokenOpts{pin: &e.orgID})
	e.must(200, e.do("PUT", prefPath, samTok, map[string]any{"theme": "terminal"}))
	if p := e.prefs(e.chen); p.Theme != "paper" {
		t.Fatalf("another person changed chen's theme: %+v", p)
	}
	if p := e.prefs(samTok); p.Theme != "terminal" {
		t.Fatalf("sam = %+v", p)
	}
}
