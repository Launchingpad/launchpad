package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"devops/internal/gitapi/gitapitest"
	"devops/internal/tracker"
)

// LAUN-97: a GitLab hook refresh turns the pipeline events on (LAUN-85), on the
// same hook, through the real handler and the real client.

type glHost struct {
	*gitapitest.Fake
	mu    sync.Mutex
	hooks []map[string]any
}

func newGLHost(t *testing.T) *glHost {
	t.Helper()
	h := &glHost{}
	h.Fake = gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		const hooks = "/api/v4/projects/9/hooks"
		switch {
		case r.URL.Path == "/api/v4/user":
			gitapitest.JSON(w, 200, map[string]string{"username": "gl-user"})
		case r.URL.Path == "/api/v4/projects/9":
			gitapitest.JSON(w, 200, map[string]any{"id": 9, "path_with_namespace": "example-org/example-app", "web_url": "https://gitlab.example.test/example-org/example-app"})
		case r.URL.Path == hooks && r.Method == http.MethodGet:
			gitapitest.JSON(w, 200, h.hooks)
		case r.URL.Path == hooks && r.Method == http.MethodPost:
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			b["id"] = 500
			h.hooks = append(h.hooks, b)
			gitapitest.JSON(w, 201, b)
		case r.URL.Path == hooks+"/500" && r.Method == http.MethodPut:
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			for k, v := range b {
				h.hooks[0][k] = v
			}
			gitapitest.JSON(w, 200, h.hooks[0])
		default:
			return false
		}
		return true
	})
	return h
}

func (h *glHost) hook() (map[string]any, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.hooks) == 0 {
		return nil, 0
	}
	c := map[string]any{}
	for k, v := range h.hooks[0] {
		c[k] = v
	}
	return c, len(h.hooks)
}

func TestGitLabHooks_RefreshTurnsPipelineEventsOn(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	h := newGLHost(t)
	var gi tracker.GitIntegrationCreated
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "gitlab", "baseUrl": h.Srv.URL, "mode": "token", "token": gitToken})).json(t, &gi)
	repo := g.mapRepo(gi.ID, 9)
	hook, n := h.hook()
	if n != 1 || hook["pipeline_events"] != true || hook["merge_requests_events"] != true {
		t.Fatalf("a hook made now has %d hooks, %v", n, hook)
	}
	// A hook made before LAUN-85 has no pipeline events.
	h.mu.Lock()
	h.hooks[0]["pipeline_events"] = false
	h.mu.Unlock()
	var out tracker.HookRefreshResult
	g.must(http.StatusOK, g.do("POST", g.path(gi.ID, "/repos/"+repo.ID.String()+"/hook/refresh"), g.chen, nil)).json(t, &out)
	hook, n = h.hook()
	if out.HookState != "active" || n != 1 || hook["pipeline_events"] != true || hook["merge_requests_events"] != true {
		t.Fatalf("after the refresh: %+v, %d hooks, %v", out, n, hook)
	}
	if strings.Contains(string(mustJSON(t, out)), gitToken) {
		t.Fatal("the token is in the answer")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
