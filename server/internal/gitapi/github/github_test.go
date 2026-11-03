package github_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"devops/internal/gitapi"
	"devops/internal/gitapi/gitapitest"
	"devops/internal/gitapi/github"
	"devops/internal/netguard"
	"devops/internal/scm"
)

const tok = "ghp_SECRET_token_value_123"

type fake struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	calls    []string // "METHOD path?query"
	bodies   map[string]string
	handler  func(w http.ResponseWriter, r *http.Request) bool
	authSeen []string
}

func newFake(t *testing.T, h func(w http.ResponseWriter, r *http.Request) bool) *fake {
	f := &fake{t: t, handler: h, bodies: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		f.mu.Lock()
		key := r.Method + " " + r.URL.RequestURI()
		f.calls = append(f.calls, key)
		f.bodies[key] = string(b)
		f.authSeen = append(f.authSeen, r.Header.Get("Authorization"))
		f.mu.Unlock()
		if !f.handler(w, r) {
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) client() *github.Client {
	c, err := github.NewAt(f.srv.URL, tok, gitapitest.Options(nil))
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *fake) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func js(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestCheckToken(t *testing.T) {
	scopes := "repo, admin:repo_hook"
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/user" {
			return false
		}
		if r.Header.Get("Authorization") != "Bearer "+tok || r.Header.Get("X-GitHub-Api-Version") == "" {
			t.Error("headers")
		}
		if scopes != "-" {
			w.Header().Set("X-OAuth-Scopes", scopes)
		}
		js(w, 200, map[string]any{"login": "octo", "name": "Octo Cat"})
		return true
	})
	id, err := f.client().CheckToken(context.Background())
	if err != nil || id.Login != "octo" || id.Name != "Octo Cat" {
		t.Fatalf("%v %v", id, err)
	}
	scopes = "-" // a fine-grained token sends no scope header
	if _, err := f.client().CheckToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	scopes = "" // a classic token with no scope can do nothing
	if _, err := f.client().CheckToken(context.Background()); gitapi.CodeOf(err) != gitapi.CodeForbidden {
		t.Errorf("empty scopes: %v", err)
	}
}

func TestErrorMatrix(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   gitapi.Code
	}{{401, gitapi.CodeUnauthorized}, {403, gitapi.CodeForbidden}, {404, gitapi.CodeNotFound}, {429, gitapi.CodeRateLimited}, {500, gitapi.CodeError}, {502, gitapi.CodeError}} {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request) bool {
			w.WriteHeader(tc.status)
			fmt.Fprintf(w, `{"message":"%s"}`, r.Header.Get("Authorization"))
			return true
		})
		c := f.client()
		for name, call := range map[string]func() error{
			"check": func() error { _, e := c.CheckToken(context.Background()); return e },
			"list":  func() error { _, e := c.ListRepos(context.Background(), "", ""); return e },
			"get":   func() error { _, e := c.GetRepo(context.Background(), "12"); return e },
			"hook": func() error {
				_, e := c.EnsureHook(context.Background(), scm.RepoRef{ID: "12"}, scm.HookSpec{URL: "https://l/h", Secret: "s3cret"})
				return e
			},
			"del":  func() error { return c.DeleteHook(context.Background(), scm.RepoRef{ID: "12"}, scm.HookRef{ID: "5"}) },
			"chg":  func() error { _, e := c.GetChange(context.Background(), scm.RepoRef{ID: "12"}, 3); return e },
			"open": func() error { _, e := c.ListOpenChanges(context.Background(), scm.RepoRef{ID: "12"}, ""); return e },
		} {
			err := call()
			if name == "del" && tc.status == 404 {
				if err != nil {
					t.Errorf("delete of a gone hook: %v", err)
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
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		_, _ = w.Write([]byte(`{"login":"` + strings.Repeat("x", gitapi.MaxBody) + `"}`))
		return true
	})
	if _, err := f.client().CheckToken(context.Background()); gitapi.CodeOf(err) != gitapi.CodeError {
		t.Errorf("%v", err)
	}
}

func repoPages(w http.ResponseWriter, r *http.Request, total int, base string) {
	page := 1
	fmt.Sscan(r.URL.Query().Get("page"), &page)
	if page < total {
		w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=%d&per_page=100>; rel="next"`, base, r.URL.Path, page+1))
	}
	js(w, 200, []map[string]any{{"id": 1000 + page, "full_name": fmt.Sprintf("acme/repo-%d", page), "html_url": fmt.Sprintf("https://github.com/acme/repo-%d", page)}})
}

func TestListReposCursorsAndPageLimit(t *testing.T) {
	var f *fake
	f = newFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/user/repos" {
			return false
		}
		repoPages(w, r, 99, f.srv.URL)
		return true
	})
	c := f.client()
	cursor := ""
	var paths []string
	pages := 0
	for {
		page, err := c.ListRepos(context.Background(), "", cursor)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, it := range page.Items {
			paths = append(paths, it.Path)
			if it.ID != it.Key || it.URL == "" {
				t.Errorf("%+v", it)
			}
		}
		if strings.Contains(page.Next, "http") || strings.ContainsAny(page.Next, "/:?") {
			t.Errorf("cursor %q is not opaque", page.Next)
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	if pages != gitapi.MaxPages || len(paths) != gitapi.MaxPages || f.count("GET /user/repos") != gitapi.MaxPages {
		t.Errorf("pages %d, items %d, requests %d", pages, len(paths), f.count("GET /user/repos"))
	}
	// The page parameters came from the headers: page=2.. in order.
	if f.calls[1] == f.calls[0] || !strings.Contains(f.calls[1], "page=2") {
		t.Errorf("%v", f.calls)
	}
	// A cursor for a sixth page, or any URL, never reaches the host.
	before := len(f.calls)
	for _, bad := range []string{"https://evil.example/x", "Ni4x", "../x"} {
		if _, err := c.ListRepos(context.Background(), "", bad); !errors.Is(err, gitapi.ErrInvalidCursor) {
			t.Errorf("%q: %v", bad, err)
		}
		if _, err := c.ListOpenChanges(context.Background(), scm.RepoRef{ID: "1"}, bad); !errors.Is(err, gitapi.ErrInvalidCursor) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if len(f.calls) != before {
		t.Error("an invalid cursor caused a request")
	}
}

func TestListReposQueryFilter(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		js(w, 200, []map[string]any{{"id": 1, "full_name": "acme/Alpha", "html_url": "u1"}, {"id": 2, "full_name": "acme/beta", "html_url": "u2"}})
		return true
	})
	page, err := f.client().ListRepos(context.Background(), " ALPHA ", "")
	if err != nil || len(page.Items) != 1 || page.Items[0].Path != "acme/Alpha" || page.Items[0].ID != "1" {
		t.Fatalf("%+v %v", page, err)
	}
}

// Rules a to d: a next link naming another origin is an error, is never
// requested, and the second origin sees no Authorization header.
func TestNextLinkToOtherOriginIsNeverFollowed(t *testing.T) {
	var second atomic.Int32
	var secondAuth atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		second.Add(1)
		secondAuth.Store(r.Header.Get("Authorization"))
		js(w, 200, []any{})
	}))
	defer other.Close()
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Link", fmt.Sprintf(`<%s/user/repos?page=2>; rel="next"`, other.URL))
		js(w, 200, []map[string]any{{"id": 1, "full_name": "a/b", "html_url": "u"}})
		return true
	})
	_, err := f.client().ListRepos(context.Background(), "", "")
	if gitapi.CodeOf(err) != gitapi.CodeError {
		t.Errorf("%v", err)
	}
	// Hook listing walks next links too.
	_, err = f.client().EnsureHook(context.Background(), scm.RepoRef{ID: "1"}, scm.HookSpec{URL: "https://l/h"})
	if err == nil {
		t.Error("hook listing followed a foreign next link")
	}
	if second.Load() != 0 || secondAuth.Load() != nil {
		t.Errorf("second origin hit %d times, Authorization %v", second.Load(), secondAuth.Load())
	}
}

func TestGetRepoAndBadIDs(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/repositories/4242" {
			return false
		}
		js(w, 200, map[string]any{"id": 4242, "full_name": "acme/new-name", "html_url": "https://github.com/acme/new-name"})
		return true
	})
	c := f.client()
	r, err := c.GetRepo(context.Background(), "4242")
	if err != nil || r.ID != "4242" || r.Key != "4242" || r.Path != "acme/new-name" {
		t.Fatalf("%+v %v", r, err)
	}
	for _, bad := range []string{"", "acme/repo", "12/../../x", "1 2", "-1"} {
		if _, err := c.GetRepo(context.Background(), bad); !errors.Is(err, gitapi.ErrBadRepo) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	n := len(f.calls)
	if _, err := c.GetChange(context.Background(), scm.RepoRef{ID: "x/y"}, 1); !errors.Is(err, gitapi.ErrBadRepo) {
		t.Error("bad repo id in GetChange")
	}
	if len(f.calls) != n {
		t.Error("bad id reached the host")
	}
}

type hookState struct {
	mu    sync.Mutex
	hooks []map[string]any
	next  int
}

func hookHandler(st *hookState, idRoute bool, lose bool) func(w http.ResponseWriter, r *http.Request) bool {
	prefix := "/repositories/7/hooks"
	if !idRoute {
		prefix = "/repos/acme/app/hooks"
	}
	return func(w http.ResponseWriter, r *http.Request) bool {
		st.mu.Lock()
		defer st.mu.Unlock()
		switch {
		case r.URL.Path == "/repositories/7" && !idRoute:
			js(w, 200, map[string]any{"id": 7, "full_name": "acme/app", "html_url": "u"})
		case !idRoute && strings.HasPrefix(r.URL.Path, "/repositories/7/"):
			http.NotFound(w, r)
		case r.URL.Path == prefix && r.Method == "GET":
			js(w, 200, st.hooks)
		case r.URL.Path == prefix && r.Method == "POST":
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			st.next++
			h := map[string]any{"id": 100 + st.next, "config": b["config"], "events": b["events"]}
			st.hooks = append(st.hooks, h)
			if lose { // the host made the hook, the answer is lost
				w.WriteHeader(502)
				return true
			}
			js(w, 201, h)
		case strings.HasPrefix(r.URL.Path, prefix+"/") && r.Method == "PATCH":
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			for _, h := range st.hooks {
				if r.URL.Path == fmt.Sprintf("%s/%v", prefix, h["id"]) {
					h["config"] = b["config"]
					h["events"] = b["events"]
					js(w, 200, h)
					return true
				}
			}
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, prefix+"/") && r.Method == "DELETE":
			w.WriteHeader(204)
		default:
			return false
		}
		return true
	}
}

func TestEnsureHookCreateThenUpdate(t *testing.T) {
	for _, idRoute := range []bool{true, false} {
		st := &hookState{}
		f := newFake(t, hookHandler(st, idRoute, false))
		c := f.client()
		repo := scm.RepoRef{ID: "7"}
		spec := scm.HookSpec{URL: "https://launchpad.example/api/v1/orgs/orb/integrations/github/webhook", Secret: "whsec-1"}
		h1, err := c.EnsureHook(context.Background(), repo, spec)
		if err != nil || h1.ID != "101" {
			t.Fatalf("idRoute=%v: %v %v", idRoute, h1, err)
		}
		spec.Secret = "whsec-2"
		h2, err := c.EnsureHook(context.Background(), repo, spec)
		if err != nil || h2.ID != h1.ID || len(st.hooks) != 1 {
			t.Fatalf("second ensure: %v %v, %d hooks", h2, err, len(st.hooks))
		}
		cfg := st.hooks[0]["config"].(map[string]any)
		if cfg["secret"] != "whsec-2" || cfg["insecure_ssl"] != "0" || cfg["content_type"] != "json" {
			t.Errorf("config %v", cfg)
		}
		ev := st.hooks[0]["events"].([]any)
		if len(ev) != 1 || ev[0] != "pull_request" {
			t.Errorf("events %v", ev)
		}
		// LAUN-83: a refresh with reviews on updates the SAME hook with the new events.
		spec.Reviews = true
		h4, err := c.EnsureHook(context.Background(), repo, spec)
		if err != nil || h4.ID != h1.ID || len(st.hooks) != 1 {
			t.Fatalf("refresh: %v %v, %d hooks", h4, err, len(st.hooks))
		}
		if ev := fmt.Sprint(st.hooks[0]["events"]); ev != "[pull_request pull_request_review]" {
			t.Errorf("events after refresh = %v", ev)
		}
		// LAUN-85: a refresh with checks on adds the check events to the SAME hook.
		spec.Checks = true
		h5, err := c.EnsureHook(context.Background(), repo, spec)
		if err != nil || h5.ID != h1.ID || len(st.hooks) != 1 {
			t.Fatalf("checks refresh: %v %v, %d hooks", h5, err, len(st.hooks))
		}
		if ev := fmt.Sprint(st.hooks[0]["events"]); ev != "[pull_request pull_request_review check_suite status]" {
			t.Errorf("events after the checks refresh = %v", ev)
		}
		spec.Checks = false
		spec.Reviews = false
		if _, err := c.EnsureHook(context.Background(), repo, spec); err != nil {
			t.Fatal(err)
		}
		// Another URL makes a second hook.
		if h3, err := c.EnsureHook(context.Background(), repo, scm.HookSpec{URL: "https://other/h", Secret: "x"}); err != nil || h3.ID == h1.ID {
			t.Errorf("other URL: %v %v", h3, err)
		}
		if err := c.DeleteHook(context.Background(), repo, h1); err != nil {
			t.Error(err)
		}
		if idRoute && f.count("GET /repos/") != 0 {
			t.Error("the id route needed no owner/name path")
		}
		if !idRoute && f.count("GET /repos/acme/app/hooks") == 0 {
			t.Error("owner/name fallback not used")
		}
	}
}

func TestEnsureHookLostAnswerMakesNoSecondHook(t *testing.T) {
	st := &hookState{}
	f := newFake(t, hookHandler(st, true, true))
	h, err := f.client().EnsureHook(context.Background(), scm.RepoRef{ID: "7"}, scm.HookSpec{URL: "https://l/h", Secret: "s"})
	if err != nil || h.ID != "101" || len(st.hooks) != 1 {
		t.Fatalf("%v %v %d", h, err, len(st.hooks))
	}
	if f.count("POST ") != 1 {
		t.Errorf("POST sent %d times", f.count("POST "))
	}
}

func prJSON(n int, draft bool, headRepo, baseRepo int) map[string]any {
	head := map[string]any{"ref": "LAUN-5-fix", "sha": "abc123"}
	if headRepo != 0 {
		head["repo"] = map[string]any{"id": headRepo}
	}
	return map[string]any{
		"number": n, "html_url": fmt.Sprintf("https://github.com/acme/app/pull/%d", n), "title": "Fix LAUN-5",
		"body": nil, "state": "open", "draft": draft, "merged": false, "merged_at": nil,
		"updated_at": "2026-10-20T10:00:00Z", "user": map[string]any{"login": "octo"},
		"head": head, "base": map[string]any{"repo": map[string]any{"id": baseRepo}},
	}
}

func TestGetChange(t *testing.T) {
	merged := prJSON(3, false, 7, 7)
	merged["state"] = "closed"
	merged["merged_at"] = "2026-10-21T10:00:00Z"
	closed := prJSON(4, false, 7, 7)
	closed["state"] = "closed"
	prs := map[string]map[string]any{"/repositories/7/pulls/2": prJSON(2, true, 8, 7), "/repositories/7/pulls/3": merged, "/repositories/7/pulls/4": closed, "/repositories/7/pulls/5": prJSON(5, false, 0, 7)}
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		if p, ok := prs[r.URL.Path]; ok {
			js(w, 200, p)
			return true
		}
		return false
	})
	c := f.client()
	repo := scm.RepoRef{ID: "7"}
	ch, err := c.GetChange(context.Background(), repo, 2)
	if err != nil || ch.Number != 2 || ch.State != "opened" || !ch.Draft || !ch.Fork || ch.SourceBranch != "LAUN-5-fix" ||
		ch.Author != "octo" || ch.HeadSHA != "abc123" || ch.Description != "" || ch.Title != "Fix LAUN-5" ||
		ch.Review != scm.ReviewNone || ch.Checks != scm.ChecksNone || ch.UpdatedAt.IsZero() || !strings.HasSuffix(ch.URL, "/pull/2") {
		t.Fatalf("%+v %v", ch, err)
	}
	if ch, _ = c.GetChange(context.Background(), repo, 3); ch.State != "merged" || ch.Fork {
		t.Errorf("merged: %+v", ch)
	}
	if ch, _ = c.GetChange(context.Background(), repo, 4); ch.State != "closed" {
		t.Errorf("closed: %+v", ch)
	}
	if ch, _ = c.GetChange(context.Background(), repo, 5); !ch.Fork { // the fork was deleted
		t.Errorf("deleted head repo: %+v", ch)
	}
	if _, err := c.GetChange(context.Background(), repo, 99); gitapi.CodeOf(err) != gitapi.CodeNotFound {
		t.Errorf("missing: %v", err)
	}
	if _, err := c.GetChange(context.Background(), repo, 0); err == nil {
		t.Error("number 0")
	}
}

func TestListOpenChangesNewestFirstAndCursor(t *testing.T) {
	var f *fake
	f = newFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/repositories/7/pulls" {
			return false
		}
		q := r.URL.Query()
		if q.Get("state") != "open" || q.Get("sort") != "created" || q.Get("direction") != "desc" {
			t.Errorf("query %v", q)
		}
		page := 1
		fmt.Sscan(q.Get("page"), &page)
		if page < 2 {
			w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=2>; rel="next"`, f.srv.URL, r.URL.Path))
		}
		js(w, 200, []any{prJSON(10-page, false, 7, 7)})
		return true
	})
	c := f.client()
	p1, err := c.ListOpenChanges(context.Background(), scm.RepoRef{ID: "7"}, "")
	if err != nil || len(p1.Items) != 1 || p1.Items[0].Number != 9 || p1.Next == "" {
		t.Fatalf("%+v %v", p1, err)
	}
	p2, err := c.ListOpenChanges(context.Background(), scm.RepoRef{ID: "7"}, p1.Next)
	if err != nil || p2.Items[0].Number != 8 || p2.Next != "" {
		t.Fatalf("%+v %v", p2, err)
	}
}

func TestNewFromConfig(t *testing.T) {
	if _, err := github.New(scm.ClientConfig{BaseURL: "https://github.com", Token: tok}, gitapitest.Options(nil)); err != nil {
		t.Error(err)
	}
	if _, err := github.New(scm.ClientConfig{BaseURL: "nonsense", Token: tok}, gitapitest.Options(nil)); err == nil {
		t.Error("bad base")
	}
	if _, err := github.New(scm.ClientConfig{BaseURL: "https://github.com"}, gitapitest.Options(nil)); err == nil {
		t.Error("no token")
	}
}

// With the real guard and no test seam, a loopback API host is refused.
func TestGuardedByDefault(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) bool { t.Error("reached"); return true })
	c, _ := github.NewAt(f.srv.URL, tok, gitapi.Options{HTTP: gitapi.NewHTTPClient(&netguard.Dialer{})})
	_, err := c.CheckToken(context.Background())
	if !errors.Is(err, netguard.ErrRefused) || gitapi.CodeOf(err) != gitapi.CodeUnreachable {
		t.Errorf("%v", err)
	}
}

// A host that answers a dot segment as the full name cannot climb the path.
func TestFallbackRejectsDotSegments(t *testing.T) {
	for _, name := range []string{"../x", "x/..", "./x"} {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == "/repositories/7" {
				js(w, 200, map[string]any{"id": 7, "full_name": name, "html_url": "u"})
				return true
			}
			return false
		})
		_, err := f.client().GetChange(context.Background(), scm.RepoRef{ID: "7"}, 1)
		if gitapi.CodeOf(err) != gitapi.CodeError {
			t.Errorf("%s: %v", name, err)
		}
		if f.count("GET /repos/") != 0 {
			t.Errorf("%s: owner/name path requested", name)
		}
	}
}

// LAUN-96: the review of a pull request is each reviewer's latest approval or
// request for changes; a dismissal removes that reviewer; a comment is ignored.
func TestReadReview(t *testing.T) {
	rv := func(user, state string) map[string]any {
		return map[string]any{"state": state, "user": map[string]any{"login": user}}
	}
	cases := map[int][]map[string]any{
		1: {rv("alice", "APPROVED")},
		2: {rv("alice", "APPROVED"), rv("bob", "CHANGES_REQUESTED")},
		3: {rv("alice", "APPROVED"), rv("bob", "CHANGES_REQUESTED"), rv("Bob", "DISMISSED")},
		4: {rv("alice", "CHANGES_REQUESTED"), rv("alice", "APPROVED")},
		5: {rv("alice", "APPROVED"), rv("alice", "COMMENTED")},
		6: {rv("alice", "APPROVED"), rv("alice", "DISMISSED")},
		7: {},
	}
	want := map[int]scm.Review{1: scm.ReviewApproved, 2: scm.ReviewChangesRequested, 3: scm.ReviewApproved,
		4: scm.ReviewApproved, 5: scm.ReviewApproved, 6: scm.ReviewNone, 7: scm.ReviewNone}
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		var n int
		if _, err := fmt.Sscanf(r.URL.Path, "/repositories/12/pulls/%d/reviews", &n); err != nil || cases[n] == nil {
			return false
		}
		js(w, 200, cases[n])
		return true
	})
	c := f.client()
	for n, w := range want {
		if got, err := c.ReadReview(context.Background(), scm.RepoRef{ID: "12"}, int64(n)); err != nil || got != w {
			t.Errorf("pull %d: %q %v, want %q", n, got, err, w)
		}
	}
	if _, err := c.ReadReview(context.Background(), scm.RepoRef{ID: "12"}, 99); err == nil {
		t.Error("a missing pull request must be an error, not none")
	}
}

// A review list longer than the page limit is an error: the newest reviews
// are at the end, so a guess could be wrong.
func TestReadReviewTooManyPagesIsAnError(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		page := 1
		fmt.Sscan(r.URL.Query().Get("page"), &page)
		w.Header().Set("Link", fmt.Sprintf(`<%s%s?page=%d&per_page=100>; rel="next"`, "http://"+r.Host, r.URL.Path, page+1))
		js(w, 200, []map[string]any{})
		return true
	})
	if _, err := f.client().ReadReview(context.Background(), scm.RepoRef{ID: "12"}, 1); err == nil {
		t.Error("endless pages must be an error")
	}
}
