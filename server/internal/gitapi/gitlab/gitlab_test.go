package gitlab_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"devops/internal/gitapi"
	"devops/internal/gitapi/gitapitest"
	"devops/internal/gitapi/gitlab"
	"devops/internal/netguard"
	"devops/internal/scm"
)

const tok = "glpat-SECRET-token-value"

var js = gitapitest.JSON

func client(t *testing.T, f *gitapitest.Fake) *gitlab.Client {
	t.Helper()
	c, err := gitlab.NewAt(f.Srv.URL+"/api/v4", tok, gitapitest.Options(nil))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCheckTokenScopes(t *testing.T) {
	scopes := []string{"api"}
	selfStatus := 200
	f := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer "+tok {
			t.Error("Authorization")
		}
		switch r.URL.Path {
		case "/api/v4/user":
			js(w, 200, map[string]any{"username": "bot", "name": "Project bot"})
		case "/api/v4/personal_access_tokens/self":
			if selfStatus != 200 {
				w.WriteHeader(selfStatus)
				return true
			}
			js(w, 200, map[string]any{"scopes": scopes})
		default:
			return false
		}
		return true
	})
	c := client(t, f)
	id, err := c.CheckToken(context.Background())
	if err != nil || id.Login != "bot" || id.Name != "Project bot" {
		t.Fatalf("%v %v", id, err)
	}
	scopes = []string{"read_api"} // cannot add a project hook
	if _, err := c.CheckToken(context.Background()); gitapi.CodeOf(err) != gitapi.CodeForbidden {
		t.Errorf("read_api: %v", err)
	}
	selfStatus = 404 // an old host: /user is enough
	if _, err := c.CheckToken(context.Background()); err != nil {
		t.Errorf("no self route: %v", err)
	}
	selfStatus = 401
	if _, err := c.CheckToken(context.Background()); gitapi.CodeOf(err) != gitapi.CodeUnauthorized {
		t.Errorf("%v", err)
	}
}

func TestErrorMatrix(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   gitapi.Code
	}{{401, gitapi.CodeUnauthorized}, {403, gitapi.CodeForbidden}, {404, gitapi.CodeNotFound}, {429, gitapi.CodeRateLimited}, {500, gitapi.CodeError}, {503, gitapi.CodeError}} {
		f := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
			w.WriteHeader(tc.status)
			fmt.Fprintf(w, `{"message":"%s"}`, r.Header.Get("Authorization"))
			return true
		})
		c := client(t, f)
		repo := scm.RepoRef{ID: "12"}
		for name, call := range map[string]func() error{
			"check": func() error { _, e := c.CheckToken(context.Background()); return e },
			"list":  func() error { _, e := c.ListRepos(context.Background(), "q", ""); return e },
			"get":   func() error { _, e := c.GetRepo(context.Background(), "12"); return e },
			"hook": func() error {
				_, e := c.EnsureHook(context.Background(), repo, scm.HookSpec{URL: "https://l/h", Secret: "s3cret"})
				return e
			},
			"del":  func() error { return c.DeleteHook(context.Background(), repo, scm.HookRef{ID: "5"}) },
			"chg":  func() error { _, e := c.GetChange(context.Background(), repo, 3); return e },
			"open": func() error { _, e := c.ListOpenChanges(context.Background(), repo, ""); return e },
		} {
			err := call()
			if name == "del" && tc.status == 404 {
				if err != nil {
					t.Errorf("gone hook: %v", err)
				}
				continue
			}
			if gitapi.CodeOf(err) != tc.want {
				t.Errorf("%d %s: %v", tc.status, name, err)
				continue
			}
			for _, bad := range []string{tok, "Bearer", "s3cret"} {
				if strings.Contains(err.Error(), bad) {
					t.Errorf("%d %s: error holds %q", tc.status, name, bad)
				}
			}
		}
	}
}

func TestOversizeBody(t *testing.T) {
	f := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		_, _ = w.Write([]byte(`[{"iid":1,"title":"` + strings.Repeat("x", gitapi.MaxBody) + `"}]`))
		return true
	})
	if _, err := client(t, f).ListOpenChanges(context.Background(), scm.RepoRef{ID: "1"}, ""); gitapi.CodeOf(err) != gitapi.CodeError {
		t.Errorf("%v", err)
	}
}

func TestListReposPagesAndQuery(t *testing.T) {
	f := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/v4/projects" {
			return false
		}
		q := r.URL.Query()
		if q.Get("membership") != "true" || q.Get("min_access_level") != "40" || q.Get("search") != "lp" {
			t.Errorf("query %v", q)
		}
		page := 1
		fmt.Sscan(q.Get("page"), &page)
		w.Header().Set("X-Next-Page", fmt.Sprint(page+1)) // never ends
		js(w, 200, []map[string]any{{"id": 500 + page, "path_with_namespace": fmt.Sprintf("grp/p%d", page), "web_url": "https://gl/grp/p"}})
		return true
	})
	c := client(t, f)
	cursor, pages := "", 0
	for {
		pg, err := c.ListRepos(context.Background(), "lp", cursor)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if len(pg.Items) != 1 || pg.Items[0].ID != fmt.Sprint(500+pages) || pg.Items[0].Key != pg.Items[0].ID {
			t.Fatalf("%+v", pg)
		}
		if strings.Contains(pg.Next, "http") || strings.ContainsAny(pg.Next, "/:?") {
			t.Errorf("cursor %q", pg.Next)
		}
		if pg.Next == "" {
			break
		}
		cursor = pg.Next
	}
	if pages != gitapi.MaxPages || f.Count("GET /api/v4/projects") != gitapi.MaxPages {
		t.Errorf("pages %d requests %d", pages, f.Count("GET "))
	}
	for _, bad := range []string{"http://evil/", "Ni4x", "x/y"} {
		if _, err := c.ListRepos(context.Background(), "", bad); !errors.Is(err, gitapi.ErrInvalidCursor) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// A Link header and an X-Next-Page that name another origin: no request goes
// there and it sees no Authorization.
func TestNextLinkToOtherOriginIsNeverFollowed(t *testing.T) {
	var hits atomic.Int32
	var auth atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		auth.Store(r.Header.Get("Authorization"))
		js(w, 200, []any{})
	}))
	defer other.Close()
	f := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Link", fmt.Sprintf(`<%s/api/v4/projects?page=2>; rel="next"`, other.URL))
		w.Header().Set("X-Next-Page", other.URL+"/api/v4/projects?page=2")
		js(w, 200, []map[string]any{{"id": 1, "path_with_namespace": "a/b", "web_url": "u"}})
		return true
	})
	c := client(t, f)
	pg, err := c.ListRepos(context.Background(), "", "")
	if err != nil || pg.Next != "" {
		t.Errorf("a URL as the next page was accepted: %+v %v", pg, err)
	}
	if _, err := c.ListOpenChanges(context.Background(), scm.RepoRef{ID: "1"}, ""); err != nil {
		t.Error(err)
	}
	if hits.Load() != 0 || auth.Load() != nil {
		t.Errorf("second origin hit %d times (%v)", hits.Load(), auth.Load())
	}
}

func TestGetRepoAndBadIDs(t *testing.T) {
	f := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/v4/projects/42" {
			return false
		}
		js(w, 200, map[string]any{"id": 42, "path_with_namespace": "grp/sub/app", "web_url": "https://gl/grp/sub/app"})
		return true
	})
	c := client(t, f)
	r, err := c.GetRepo(context.Background(), "42")
	if err != nil || r.ID != "42" || r.Path != "grp/sub/app" {
		t.Fatalf("%+v %v", r, err)
	}
	for _, bad := range []string{"", "grp%2Fapp", "grp/app", "4 2", "../x"} {
		if _, err := c.GetRepo(context.Background(), bad); !errors.Is(err, gitapi.ErrBadRepo) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	n := len(f.Calls())
	if _, err := c.GetChange(context.Background(), scm.RepoRef{ID: "a/b"}, 1); !errors.Is(err, gitapi.ErrBadRepo) {
		t.Error("bad id")
	}
	if len(f.Calls()) != n {
		t.Error("bad id reached the host")
	}
}

func TestHooks(t *testing.T) {
	hooks := []map[string]any{}
	lose := false
	f := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		const p = "/api/v4/projects/9/hooks"
		switch {
		case r.URL.Path == p && r.Method == "GET":
			js(w, 200, hooks)
		case r.URL.Path == p && r.Method == "POST":
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			b["id"] = 200 + len(hooks) + 1
			hooks = append(hooks, b)
			if lose {
				w.WriteHeader(502)
				return true
			}
			js(w, 201, b)
		case strings.HasPrefix(r.URL.Path, p+"/") && r.Method == "PUT":
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			for _, h := range hooks {
				if r.URL.Path == fmt.Sprintf("%s/%v", p, h["id"]) {
					b["id"] = h["id"]
					for k, v := range b {
						h[k] = v
					}
					js(w, 200, h)
					return true
				}
			}
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, p+"/") && r.Method == "DELETE":
			w.WriteHeader(204)
		default:
			return false
		}
		return true
	})
	c := client(t, f)
	repo := scm.RepoRef{ID: "9"}
	spec := scm.HookSpec{URL: "https://lp.example/api/v1/orgs/orb/integrations/gitlab/webhook", Secret: "tok1"}
	h1, err := c.EnsureHook(context.Background(), repo, spec)
	if err != nil || h1.ID != "201" {
		t.Fatalf("%v %v", h1, err)
	}
	if hooks[0]["merge_requests_events"] != true || hooks[0]["push_events"] != false || hooks[0]["enable_ssl_verification"] != true {
		t.Errorf("events: %v", hooks[0])
	}
	if hooks[0]["pipeline_events"] != false {
		t.Errorf("a spec without checks must not subscribe to pipelines: %v", hooks[0])
	}
	// LAUN-85: a refresh with checks on turns the pipeline events on, on the SAME hook.
	spec.Checks = true
	if h, err := c.EnsureHook(context.Background(), repo, spec); err != nil || h.ID != h1.ID || len(hooks) != 1 || hooks[0]["pipeline_events"] != true || hooks[0]["merge_requests_events"] != true {
		t.Fatalf("checks refresh: %v %v %v", h, err, hooks)
	}
	spec.Secret = "tok2"
	h2, err := c.EnsureHook(context.Background(), repo, spec)
	if err != nil || h2.ID != h1.ID || len(hooks) != 1 || hooks[0]["token"] != "tok2" {
		t.Fatalf("update: %v %v %v", h2, err, hooks)
	}
	lose = true
	h3, err := c.EnsureHook(context.Background(), repo, scm.HookSpec{URL: "https://other/h", Secret: "s"})
	if err != nil || h3.ID != "202" || len(hooks) != 2 || f.Count("POST ") != 2 {
		t.Fatalf("lost answer: %v %v %d hooks %d POSTs", h3, err, len(hooks), f.Count("POST "))
	}
	if err := c.DeleteHook(context.Background(), repo, h1); err != nil {
		t.Error(err)
	}
	if err := c.DeleteHook(context.Background(), repo, scm.HookRef{ID: "../x"}); !errors.Is(err, gitapi.ErrBadRepo) {
		t.Error("bad hook id")
	}
}

func mr(iid int, state string, src, tgt int) map[string]any {
	return map[string]any{
		"iid": iid, "web_url": fmt.Sprintf("https://gl/grp/app/-/merge_requests/%d", iid), "title": "LAUN-5 fix",
		"description": "text", "source_branch": "LAUN-5-fix", "sha": "deadbeef", "state": state, "draft": false,
		"work_in_progress": false, "source_project_id": src, "target_project_id": tgt,
		"updated_at": "2026-10-20T10:00:00.123Z", "author": map[string]any{"username": "dev"},
	}
}

func TestChanges(t *testing.T) {
	draft := mr(2, "opened", 9, 9)
	draft["draft"] = true
	f := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/api/v4/projects/9/merge_requests/1":
			js(w, 200, mr(1, "merged", 9, 9))
		case "/api/v4/projects/9/merge_requests/2":
			js(w, 200, draft)
		case "/api/v4/projects/9/merge_requests/3":
			js(w, 200, mr(3, "opened", 77, 9))
		case "/api/v4/projects/9/merge_requests/4":
			js(w, 200, mr(4, "locked", 9, 9))
		case "/api/v4/projects/9/merge_requests/5":
			js(w, 200, mr(5, "closed", 9, 9))
		case "/api/v4/projects/9/merge_requests":
			q := r.URL.Query()
			if q.Get("state") != "opened" || q.Get("order_by") != "created_at" || q.Get("sort") != "desc" {
				t.Errorf("query %v", q)
			}
			w.Header().Set("X-Next-Page", "2")
			js(w, 200, []any{mr(9, "opened", 9, 9)})
		default:
			return false
		}
		return true
	})
	c := client(t, f)
	repo := scm.RepoRef{ID: "9"}
	want := map[int64]struct {
		state       string
		draft, fork bool
	}{1: {"merged", false, false}, 2: {"opened", true, false}, 3: {"opened", false, true}, 4: {"opened", false, false}, 5: {"closed", false, false}}
	for n, w := range want {
		ch, err := c.GetChange(context.Background(), repo, n)
		if err != nil || ch.State != w.state || ch.Draft != w.draft || ch.Fork != w.fork || ch.Number != n {
			t.Errorf("%d: %+v %v", n, ch, err)
		}
	}
	ch, _ := c.GetChange(context.Background(), repo, 1)
	if ch.SourceBranch != "LAUN-5-fix" || ch.Author != "dev" || ch.HeadSHA != "deadbeef" || ch.Description != "text" || ch.UpdatedAt.IsZero() ||
		ch.Review != scm.ReviewNone || ch.Checks != scm.ChecksNone || !strings.HasSuffix(ch.URL, "/merge_requests/1") {
		t.Errorf("%+v", ch)
	}
	pg, err := c.ListOpenChanges(context.Background(), repo, "")
	if err != nil || len(pg.Items) != 1 || pg.Items[0].Number != 9 || pg.Next == "" {
		t.Fatalf("%+v %v", pg, err)
	}
	if _, err := c.GetChange(context.Background(), repo, 404); gitapi.CodeOf(err) != gitapi.CodeNotFound {
		t.Error("missing")
	}
}

func TestNewFromConfigAndGuard(t *testing.T) {
	if _, err := gitlab.New(scm.ClientConfig{BaseURL: "https://gitlab.com", Token: tok}, gitapitest.Options(nil)); err != nil {
		t.Error(err)
	}
	if _, err := gitlab.New(scm.ClientConfig{BaseURL: "https://gitlab.com"}, gitapitest.Options(nil)); err == nil {
		t.Error("no token")
	}
	f := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool { t.Error("reached"); return true })
	c, _ := gitlab.NewAt(f.Srv.URL+"/api/v4", tok, gitapi.Options{HTTP: gitapi.NewHTTPClient(&netguard.Dialer{})})
	if _, err := c.CheckToken(context.Background()); !errors.Is(err, netguard.ErrRefused) || gitapi.CodeOf(err) != gitapi.CodeUnreachable {
		t.Errorf("%v", err)
	}
}

// LAUN-96: the backfill reads the approvals of a merge request.
func TestReadReview(t *testing.T) {
	f := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/api/v4/projects/9/merge_requests/1/approvals":
			js(w, 200, map[string]any{"approved": true, "approved_by": []any{map[string]any{"user": map[string]any{"username": "a"}}}})
		case "/api/v4/projects/9/merge_requests/2/approvals":
			js(w, 200, map[string]any{"approved": false, "approved_by": []any{}})
		case "/api/v4/projects/9/merge_requests/4/approvals":
			// One of two required approvers approved: the MR is not "approved".
			js(w, 200, map[string]any{"approved": false, "approvals_left": 1, "approved_by": []any{map[string]any{"user": map[string]any{"username": "a"}}}})
		case "/api/v4/projects/9/merge_requests/5/approvals":
			js(w, 200, map[string]any{"approved_by": []any{map[string]any{"user": map[string]any{"username": "a"}}}}) // no flag
		default:
			return false
		}
		return true
	})
	c := client(t, f)
	repo := scm.RepoRef{ID: "9"}
	for n, want := range map[int64]scm.Review{1: scm.ReviewApproved, 2: scm.ReviewNone, 4: scm.ReviewNone, 5: scm.ReviewApproved} {
		if got, err := c.ReadReview(context.Background(), repo, n); err != nil || got != want {
			t.Errorf("%d: %q %v, want %q", n, got, err, want)
		}
	}
	if _, err := c.ReadReview(context.Background(), repo, 3); gitapi.CodeOf(err) != gitapi.CodeNotFound {
		t.Errorf("missing: %v", err)
	}
}
