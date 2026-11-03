package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"devops/internal/importjob"
	"devops/internal/storage/storagetest"
)

const importTestKey = "lin_api_APITESTKEY_0123456789abcdefghij"

// importEnv is a server with the web import on: the real manager, the real
// silo, and a Linear that refuses every key.
func importEnv(t *testing.T) (*env, *importjob.Manager) {
	t.Helper()
	lin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"errors":[{"message":"Authentication required"}]}`))
	}))
	t.Cleanup(lin.Close)
	var m *importjob.Manager
	st := storagetest.New(t)
	e := newEnvWith(t, envOpts{store: st, publicBase: "https://tracker.example", logLevel: slog.LevelDebug,
		configure: func(e *env, s *Server) {
			m = importjob.New(importjob.Config{Pool: e.pool, Store: st, WorkDir: t.TempDir() + "/work",
				PublicBaseURL: "https://tracker.example", LinearEndpoint: lin.URL, Log: slog.New(slog.NewTextHandler(e.logs, nil)),
				FloorBytes: 1, FreeBytes: func(string) (uint64, error) { return 50 << 30, nil }})
			s.Import = m
		}})
	t.Cleanup(m.Close)
	return e, m
}

func (e *env) idStr(r resp) string {
	e.t.Helper()
	var v struct {
		ID string `json:"id"`
	}
	r.json(e.t, &v)
	return v.ID
}

func TestImport_RoutesOffWithoutManager(t *testing.T) {
	e := newEnv(t)
	for _, c := range [][2]string{{"GET", "/import/sessions/current"}, {"POST", "/import/sessions"}} {
		if r := e.do(c[0], "/api/v1/orgs/orbit"+c[1], e.chen, nil); r.status != 404 {
			t.Fatalf("%v: %d", c, r.status)
		}
	}
}

func TestImport_SessionLifecycleAndStateErrors(t *testing.T) {
	e, _ := importEnv(t)
	base := "/api/v1/orgs/orbit/import"
	e.must(404, e.do("GET", base+"/sessions/current", e.chen, nil))
	r := e.must(201, e.do("POST", base+"/sessions", e.chen, nil))
	sid := e.idStr(r)
	e.must(200, e.do("POST", base+"/sessions", e.chen, nil)) // the open one comes back
	e.must(200, e.do("GET", base+"/sessions/current", e.chen, nil))
	// a state that does not allow the step
	for _, p := range []string{"dry-run", "run", "verify"} {
		var body any
		if p == "run" {
			body = map[string]string{"confirmOrg": "orbit"}
		}
		if r := e.do("POST", fmt.Sprintf("%s/sessions/%s/%s", base, sid, p), e.chen, body); r.status != 409 {
			t.Fatalf("%s: %d %s", p, r.status, r.body)
		}
	}
	if r := e.do("PUT", fmt.Sprintf("%s/sessions/%s/usermap", base, sid), e.chen, map[string]any{"default": "chen", "users": map[string]string{}}); r.status != 409 {
		t.Fatalf("usermap: %d %s", r.status, r.body)
	}
	e.must(409, e.do("GET", fmt.Sprintf("%s/sessions/%s/users", base, sid), e.chen, nil))
	e.must(204, e.do("DELETE", fmt.Sprintf("%s/sessions/%s", base, sid), e.chen, nil))
	e.must(404, e.do("GET", base+"/sessions/current", e.chen, nil))
}

func TestImport_AuthAndCrossOrg(t *testing.T) {
	e, _ := importEnv(t)
	org2 := e.newOrg("acme", "ACME")
	ctx := context.Background()
	sid, jid := uuid.New(), uuid.New()
	if _, err := e.pool.Exec(ctx, `INSERT INTO import_sessions (id, org_id, state, export_dir, export_counts) VALUES ($1,$2,'exported','s-other','{}')`, sid, org2); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO import_jobs (id, org_id, session_id, kind, state, instance_id, finished_at) VALUES ($1,$2,$3,'export','succeeded','x', now())`, jid, org2, sid); err != nil {
		t.Fatal(err)
	}
	base := "/api/v1/orgs/orbit/import"
	key := map[string]string{"linearApiKey": importTestKey}
	type call struct {
		method, path string
		body         any
	}
	calls := []call{
		{"GET", fmt.Sprintf("/sessions/%s/users", sid), nil},
		{"PUT", fmt.Sprintf("/sessions/%s/usermap", sid), map[string]any{"default": "chen"}},
		{"POST", fmt.Sprintf("/sessions/%s/export", sid), key},
		{"POST", fmt.Sprintf("/sessions/%s/dry-run", sid), nil},
		{"POST", fmt.Sprintf("/sessions/%s/run", sid), map[string]string{"confirmOrg": "orbit"}},
		{"POST", fmt.Sprintf("/sessions/%s/verify", sid), nil},
		{"DELETE", fmt.Sprintf("/sessions/%s", sid), nil},
		{"GET", fmt.Sprintf("/jobs/%s", jid), nil},
		{"POST", fmt.Sprintf("/jobs/%s/cancel", jid), nil},
	}
	for _, c := range calls {
		if r := e.do(c.method, base+c.path, e.chen, c.body); r.status != 404 {
			t.Errorf("cross-org %s %s: %d %s", c.method, c.path, r.status, r.body)
		}
		// a member (not an admin) of the org is refused before anything else
		if r := e.do(c.method, base+c.path, e.claude, c.body); r.status != 403 {
			t.Errorf("member %s %s: %d", c.method, c.path, r.status)
		}
		if r := e.do(c.method, base+c.path, "", c.body); r.status != 401 {
			t.Errorf("anonymous %s %s: %d", c.method, c.path, r.status)
		}
	}
	// the org itself in the path is not ours
	if r := e.do("GET", "/api/v1/orgs/acme/import/sessions/current", e.chen, nil); r.status != 404 {
		t.Errorf("other org's path: %d", r.status)
	}
	var state string
	if err := e.pool.QueryRow(ctx, `SELECT state FROM import_sessions WHERE id=$1`, sid).Scan(&state); err != nil || state != "exported" {
		t.Fatalf("the other org's session changed: %v %s", err, state)
	}
}

func TestImport_ExportKeyRules(t *testing.T) {
	e, _ := importEnv(t)
	base := "/api/v1/orgs/orbit/import"
	sid := e.idStr(e.must(201, e.do("POST", base+"/sessions", e.chen, nil)))
	path := fmt.Sprintf("%s/sessions/%s/export", base, sid)

	// the Idempotency-Key header is refused: the body holds a secret
	if r := e.do("POST", path, e.chen, map[string]string{"linearApiKey": importTestKey}, "Idempotency-Key", "k-1"); r.status != 400 {
		t.Fatalf("idempotency key: %d %s", r.status, r.body)
	}
	for name, body := range map[string]any{
		"empty":       map[string]string{},
		"extra field": map[string]string{"linearApiKey": importTestKey, "x": "y"},
		"short":       map[string]string{"linearApiKey": "abc"},
		"space":       map[string]string{"linearApiKey": "lin_api key with spaces 01234"},
		"unicode":     map[string]string{"linearApiKey": "lin_api_ééééééé"},
		"number":      map[string]int{"linearApiKey": 12345678901},
	} {
		r := e.do("POST", path, e.chen, body)
		if r.status != 400 {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
		if bytes.Contains(r.body, []byte("lin_api")) {
			t.Errorf("%s: the key is in the error", name)
		}
	}
	// a wrong key: the fake Linear says 401 and the API says so, with no job
	r := e.do("POST", path, e.chen, map[string]string{"linearApiKey": importTestKey})
	if r.status != 502 || e.codeOf(r) != "linear_unauthorized" {
		t.Fatalf("wrong key: %d %s", r.status, r.body)
	}
	if bytes.Contains(r.body, []byte("APITESTKEY")) || strings.Contains(e.logs.String(), "APITESTKEY") {
		t.Fatal("the key is in a response or a log")
	}
	for _, q := range []string{
		`SELECT coalesce(string_agg(t::text, ' '), '') FROM import_jobs t`,
		`SELECT coalesce(string_agg(t::text, ' '), '') FROM import_sessions t`,
		`SELECT coalesce(string_agg(t::text, ' '), '') FROM idempotency_keys t`,
		`SELECT coalesce(string_agg(t::text, ' '), '') FROM events t`,
	} {
		var all string
		if err := e.pool.QueryRow(context.Background(), q).Scan(&all); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(all, "APITESTKEY") {
			t.Fatalf("the key is stored: %s", q)
		}
	}
	if n := e.count(`SELECT count(*) FROM import_jobs`); n != 0 {
		t.Fatalf("%d job rows after a refused key", n)
	}
}

func (e *env) codeOf(r resp) string {
	var b struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(r.body, &b)
	return b.Code
}

// The cycles guard (decisions-T17 #43): turning cycles ON is refused while a
// session is in a state that is not settled; turning them off never is.
func TestImport_CyclesGuard(t *testing.T) {
	e, _ := importEnv(t)
	patch := func(on bool) resp {
		return e.do("PATCH", "/api/v1/orgs/orbit/teams/ORB", e.chen, map[string]any{"cyclesEnabled": on})
	}
	if r := patch(true); r.status != 200 {
		t.Fatalf("no session: %d %s", r.status, r.body)
	}
	if r := patch(false); r.status != 200 {
		t.Fatalf("off: %d", r.status)
	}
	sid := e.idStr(e.must(201, e.do("POST", "/api/v1/orgs/orbit/import/sessions", e.chen, nil)))
	for state, blocked := range map[string]bool{
		"new": false, "exporting": true, "exported": true, "user_map": true, "dry_run_ok": true,
		"importing": true, "imported": false, "verified": false, "failed": false,
	} {
		if _, err := e.pool.Exec(context.Background(), `UPDATE import_sessions SET state=$1 WHERE id=$2`, state, sid); err != nil {
			t.Fatal(err)
		}
		r := patch(true)
		if blocked {
			if r.status != 409 || e.codeOf(r) != "import_in_progress" {
				t.Errorf("%s: %d %s", state, r.status, r.body)
			}
			// off is never blocked, and the same body with cyclesEnabled false works
		} else if r.status != 200 {
			t.Errorf("%s: %d %s", state, r.status, r.body)
		}
		if r := patch(false); r.status != 200 {
			t.Errorf("%s off: %d %s", state, r.status, r.body)
		}
	}
	if _, err := e.pool.Exec(context.Background(), `UPDATE import_sessions SET state='closed', closed_at=now() WHERE id=$1`, sid); err != nil {
		t.Fatal(err)
	}
	if r := patch(true); r.status != 200 {
		t.Errorf("closed: %d", r.status)
	}
}
