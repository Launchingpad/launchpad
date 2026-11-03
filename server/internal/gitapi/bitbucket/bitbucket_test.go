package bitbucket_test

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
	"devops/internal/gitapi/bitbucket"
	"devops/internal/gitapi/gitapitest"
	"devops/internal/netguard"
	"devops/internal/scm"
)

const tok = "bb-SECRET-access-token"

var js = gitapitest.JSON

const (
	wsUUID   = "{11111111-2222-3333-4444-555555555555}"
	repoUUID = "{aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee}"
	cloudID  = "{11111111-2222-3333-4444-555555555555}/{aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee}"
	cloudPre = "/2.0/repositories/%7B11111111-2222-3333-4444-555555555555%7D/%7Baaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee%7D"
)

func cloud(t *testing.T, f *gitapitest.Fake) *bitbucket.Client {
	t.Helper()
	c, err := bitbucket.NewAt(f.Srv.URL+"/2.0", tok, gitapitest.Options(nil))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func dc(t *testing.T, f *gitapitest.Fake) *bitbucket.Client {
	t.Helper()
	c, err := bitbucket.NewAt(f.Srv.URL+"/rest/api/1.0", tok, gitapitest.Options(nil))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewChoosesFlavourFromRoot(t *testing.T) {
	o := gitapitest.Options(nil)
	for _, base := range []string{"https://bitbucket.org", "https://bb.example.com"} {
		if _, err := bitbucket.New(scm.ClientConfig{BaseURL: base, Token: tok}, o); err != nil {
			t.Error(base, err)
		}
	}
	for _, root := range []string{"https://x.com/api", "https://x.com/2.1", "https://x.com"} {
		if _, err := bitbucket.NewAt(root, tok, o); err == nil {
			t.Errorf("%s accepted", root)
		}
	}
	if _, err := bitbucket.New(scm.ClientConfig{BaseURL: "https://bitbucket.org"}, o); err == nil {
		t.Error("no token")
	}
}

func repoMatrixCalls(c *bitbucket.Client, id string) map[string]func() error {
	repo := scm.RepoRef{ID: id}
	return map[string]func() error{
		"check": func() error { _, e := c.CheckToken(context.Background()); return e },
		"list":  func() error { _, e := c.ListRepos(context.Background(), "q", ""); return e },
		"get":   func() error { _, e := c.GetRepo(context.Background(), id); return e },
		"hook": func() error {
			_, e := c.EnsureHook(context.Background(), repo, scm.HookSpec{URL: "https://l/h", Secret: "s3cret"})
			return e
		},
		"del":  func() error { return c.DeleteHook(context.Background(), repo, scm.HookRef{ID: hookIDFor(id)}) },
		"chg":  func() error { _, e := c.GetChange(context.Background(), repo, 3); return e },
		"open": func() error { _, e := c.ListOpenChanges(context.Background(), repo, ""); return e },
	}
}

func hookIDFor(repoID string) string {
	if strings.Contains(repoID, "#") {
		return "5"
	}
	return "{99999999-8888-7777-6666-555555555555}"
}

func TestErrorMatrixBothFlavours(t *testing.T) {
	for _, flavour := range []string{"cloud", "dc"} {
		for _, tc := range []struct {
			status int
			want   gitapi.Code
		}{{401, gitapi.CodeUnauthorized}, {403, gitapi.CodeForbidden}, {404, gitapi.CodeNotFound}, {429, gitapi.CodeRateLimited}, {500, gitapi.CodeError}, {504, gitapi.CodeError}} {
			f := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
				w.WriteHeader(tc.status)
				fmt.Fprintf(w, `{"error":"%s"}`, r.Header.Get("Authorization"))
				return true
			})
			c, id := cloud(t, f), cloudID
			if flavour == "dc" {
				c, id = dc(t, f), "PRJ/app#12"
			}
			for name, call := range repoMatrixCalls(c, id) {
				err := call()
				if name == "del" && tc.status == 404 {
					if err != nil {
						t.Errorf("%s: gone hook: %v", flavour, err)
					}
					continue
				}
				want := tc.want
				// Cloud /user 403 and 404 fall back to a list that fails the same way: same code.
				if gitapi.CodeOf(err) != want {
					t.Errorf("%s %d %s: %v", flavour, tc.status, name, err)
					continue
				}
				for _, bad := range []string{tok, "Bearer", "s3cret"} {
					if strings.Contains(err.Error(), bad) {
						t.Errorf("%s %d %s: error holds %q", flavour, tc.status, name, bad)
					}
				}
			}
		}
	}
}

func TestOversizeBody(t *testing.T) {
	f := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		_, _ = w.Write([]byte(`{"values":[{"id":1,"title":"` + strings.Repeat("x", gitapi.MaxBody) + `"}]}`))
		return true
	})
	if _, err := cloud(t, f).ListOpenChanges(context.Background(), scm.RepoRef{ID: cloudID}, ""); gitapi.CodeOf(err) != gitapi.CodeError {
		t.Errorf("%v", err)
	}
}

// ---- Cloud

func cloudRepo(i int) map[string]any {
	return map[string]any{
		"uuid": repoUUID, "full_name": fmt.Sprintf("acme/app%d", i),
		"workspace": map[string]any{"uuid": strings.ToUpper(wsUUID)},
		"links":     map[string]any{"html": map[string]any{"href": "https://bitbucket.org/acme/app"}},
	}
}

func TestCloudCheckToken(t *testing.T) {
	userStatus, listN := 200, 1
	f := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/2.0/user":
			if userStatus != 200 {
				w.WriteHeader(userStatus)
				return true
			}
			js(w, 200, map[string]any{"nickname": "dev", "display_name": "Dev Eloper"})
		case "/2.0/repositories":
			vals := []any{}
			for i := 0; i < listN; i++ {
				vals = append(vals, cloudRepo(i))
			}
			js(w, 200, map[string]any{"values": vals})
		default:
			return false
		}
		return true
	})
	c := cloud(t, f)
	id, err := c.CheckToken(context.Background())
	if err != nil || id.Login != "dev" || id.Name != "Dev Eloper" {
		t.Fatalf("%v %v", id, err)
	}
	userStatus = 403 // a repository access token has no user
	if _, err := c.CheckToken(context.Background()); err != nil {
		t.Errorf("fallback: %v", err)
	}
	listN = 0 // sees nothing: can do nothing
	if _, err := c.CheckToken(context.Background()); gitapi.CodeOf(err) != gitapi.CodeForbidden {
		t.Errorf("empty: %v", err)
	}
	userStatus = 401
	if _, err := c.CheckToken(context.Background()); gitapi.CodeOf(err) != gitapi.CodeUnauthorized {
		t.Errorf("401: %v", err)
	}
}

func TestCloudListReposCursorsAndPageLimit(t *testing.T) {
	var f *gitapitest.Fake
	f = gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/2.0/repositories" {
			return false
		}
		q := r.URL.Query()
		if q.Get("role") != "admin" || q.Get("q") != `name ~ "ap"` {
			t.Errorf("query %v", q)
		}
		page := 1
		fmt.Sscan(q.Get("page"), &page)
		js(w, 200, map[string]any{"values": []any{cloudRepo(page)}, "next": fmt.Sprintf("%s/2.0/repositories?page=%d", f.Srv.URL, page+1)})
		return true
	})
	c := cloud(t, f)
	cursor, pages := "", 0
	for {
		// a query with quotes and backslashes cannot break out of the filter
		pg, err := c.ListRepos(context.Background(), `a"p\`, cursor)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		it := pg.Items[0]
		if it.ID != cloudID || it.Key != cloudID || it.Path == "" || it.URL == "" {
			t.Fatalf("%+v", it)
		}
		if strings.Contains(pg.Next, "http") || strings.ContainsAny(pg.Next, "/:?") {
			t.Errorf("cursor %q", pg.Next)
		}
		if pg.Next == "" {
			break
		}
		cursor = pg.Next
	}
	if pages != gitapi.MaxPages {
		t.Errorf("pages %d", pages)
	}
	for _, bad := range []string{"https://evil.example/", "Ni4x"} {
		if _, err := c.ListRepos(context.Background(), "", bad); !errors.Is(err, gitapi.ErrInvalidCursor) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// The AC fake: a `next` link and a hook list that name another origin.
func TestNextLinkToOtherOriginIsNeverFollowed(t *testing.T) {
	var hits atomic.Int32
	var auth atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		auth.Store(r.Header.Get("Authorization"))
		js(w, 200, map[string]any{"values": []any{}})
	}))
	defer other.Close()
	f := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		js(w, 200, map[string]any{"values": []any{}, "next": other.URL + "/2.0/anything?page=2"})
		return true
	})
	c := cloud(t, f)
	if _, err := c.ListRepos(context.Background(), "", ""); gitapi.CodeOf(err) != gitapi.CodeError {
		t.Errorf("repos: %v", err)
	}
	if _, err := c.EnsureHook(context.Background(), scm.RepoRef{ID: cloudID}, scm.HookSpec{URL: "https://l/h"}); err == nil {
		t.Error("hook listing followed a foreign next link")
	}
	if _, err := c.ListOpenChanges(context.Background(), scm.RepoRef{ID: cloudID}, ""); gitapi.CodeOf(err) != gitapi.CodeError {
		t.Errorf("changes: %v", err)
	}
	if hits.Load() != 0 || auth.Load() != nil {
		t.Errorf("second origin hit %d times (%v)", hits.Load(), auth.Load())
	}
}

func TestCloudPathsEncodeBracesAndRejectBadIDs(t *testing.T) {
	f := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.EscapedPath() != cloudPre {
			return false
		}
		js(w, 200, cloudRepo(1))
		return true
	})
	c := cloud(t, f)
	ref, err := c.GetRepo(context.Background(), cloudID)
	if err != nil || ref.ID != cloudID {
		t.Fatalf("%+v %v (calls %v)", ref, err, f.Calls())
	}
	n := len(f.Calls())
	for _, bad := range []string{"", "acme", "a/b/c", "{x}/{y}", "ws/../repo", "ws/repo name", "./repo", "ws/..", "../" + repoUUID, "ws/{aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee}/x", "PRJ/app#12"} {
		if _, err := c.GetRepo(context.Background(), bad); !errors.Is(err, gitapi.ErrBadRepo) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	// A slug workspace is allowed.
	if _, err := c.GetRepo(context.Background(), "acme/"+repoUUID); err == nil || errors.Is(err, gitapi.ErrBadRepo) {
		t.Errorf("slug form: %v", err)
	}
	if len(f.Calls()) != n+1 {
		t.Errorf("a bad id reached the host: %v", f.Calls())
	}
}

func TestCloudHooks(t *testing.T) {
	type hk = map[string]any
	hooks := []hk{}
	lose := false
	const base = cloudPre + "/hooks"
	f := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		p := r.URL.EscapedPath()
		switch {
		case p == base && r.Method == "GET":
			js(w, 200, map[string]any{"values": hooks})
		case p == base && r.Method == "POST":
			var b hk
			_ = json.NewDecoder(r.Body).Decode(&b)
			b["uuid"] = fmt.Sprintf("{00000000-0000-0000-0000-%012d}", len(hooks)+1)
			hooks = append(hooks, b)
			if lose {
				w.WriteHeader(502)
				return true
			}
			js(w, 201, b)
		case strings.HasPrefix(p, base+"/") && r.Method == "PUT":
			var b hk
			_ = json.NewDecoder(r.Body).Decode(&b)
			for _, h := range hooks {
				if p == base+"/"+strings.NewReplacer("{", "%7B", "}", "%7D").Replace(h["uuid"].(string)) {
					for k, v := range b {
						h[k] = v
					}
					js(w, 200, h)
					return true
				}
			}
			http.NotFound(w, r)
		case strings.HasPrefix(p, base+"/") && r.Method == "DELETE":
			w.WriteHeader(204)
		default:
			return false
		}
		return true
	})
	c := cloud(t, f)
	repo := scm.RepoRef{ID: cloudID}
	spec := scm.HookSpec{URL: "https://lp.example/api/v1/orgs/orb/integrations/bitbucket/webhook", Secret: "one"}
	h1, err := c.EnsureHook(context.Background(), repo, spec)
	if err != nil || h1.ID != "{00000000-0000-0000-0000-000000000001}" {
		t.Fatalf("%v %v", h1, err)
	}
	if hooks[0]["skip_cert_verification"] != false || hooks[0]["active"] != true || hooks[0]["secret"] != "one" {
		t.Errorf("%v", hooks[0])
	}
	ev := fmt.Sprint(hooks[0]["events"])
	if !strings.Contains(ev, "pullrequest:created") || !strings.Contains(ev, "pullrequest:fulfilled") {
		t.Errorf("events %s", ev)
	}
	spec.Secret = "two"
	h2, err := c.EnsureHook(context.Background(), repo, spec)
	if err != nil || h2.ID != h1.ID || len(hooks) != 1 || hooks[0]["secret"] != "two" {
		t.Fatalf("update: %v %v %v", h2, err, hooks)
	}
	lose = true
	h3, err := c.EnsureHook(context.Background(), repo, scm.HookSpec{URL: "https://other/h", Secret: "s"})
	if err != nil || h3.ID == "" || len(hooks) != 2 || f.Count("POST ") != 2 {
		t.Fatalf("lost answer: %v %v %d hooks, %d POSTs", h3, err, len(hooks), f.Count("POST "))
	}
	if err := c.DeleteHook(context.Background(), repo, h1); err != nil {
		t.Error(err)
	}
	if err := c.DeleteHook(context.Background(), repo, scm.HookRef{ID: "../../x"}); !errors.Is(err, gitapi.ErrBadRepo) {
		t.Error("bad hook id")
	}
}

func TestCloudChanges(t *testing.T) {
	pr := func(id int, state string, srcRepo string) map[string]any {
		return map[string]any{
			"id": id, "title": "LAUN-5 fix", "description": "d", "state": state, "draft": id == 2,
			"updated_on": "2026-10-20T10:00:00.123456+00:00",
			"author":     map[string]any{"nickname": "dev", "account_id": "557058:abc"},
			"source": map[string]any{"branch": map[string]any{"name": "LAUN-5-fix"}, "commit": map[string]any{"hash": "abc"},
				"repository": map[string]any{"uuid": srcRepo}},
			"destination": map[string]any{"repository": map[string]any{"uuid": strings.ToUpper(repoUUID)}},
			"links":       map[string]any{"html": map[string]any{"href": fmt.Sprintf("https://bitbucket.org/acme/app/pull-requests/%d", id)}},
		}
	}
	f := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.EscapedPath() {
		case cloudPre + "/pullrequests/1":
			js(w, 200, pr(1, "MERGED", repoUUID))
		case cloudPre + "/pullrequests/2":
			js(w, 200, pr(2, "OPEN", repoUUID))
		case cloudPre + "/pullrequests/3":
			js(w, 200, pr(3, "DECLINED", "{99999999-9999-9999-9999-999999999999}"))
		case cloudPre + "/pullrequests":
			q := r.URL.Query()
			if q.Get("state") != "OPEN" || q.Get("sort") != "-created_on" || q.Get("pagelen") != "50" {
				t.Errorf("query %v", q)
			}
			js(w, 200, map[string]any{"values": []any{pr(2, "OPEN", repoUUID)}, "next": "https://" + r.Host + "/x?page=2"})
		default:
			return false
		}
		return true
	})
	c := cloud(t, f)
	repo := scm.RepoRef{ID: cloudID}
	ch, err := c.GetChange(context.Background(), repo, 1)
	if err != nil || ch.State != "merged" || ch.Fork || ch.Draft || ch.Author != "dev" || ch.SourceBranch != "LAUN-5-fix" || ch.HeadSHA != "abc" ||
		ch.UpdatedAt.IsZero() || ch.Review != scm.ReviewNone || !strings.HasSuffix(ch.URL, "/pull-requests/1") {
		t.Fatalf("%+v %v", ch, err)
	}
	if ch, _ = c.GetChange(context.Background(), repo, 2); ch.State != "opened" || !ch.Draft {
		t.Errorf("%+v", ch)
	}
	if ch, _ = c.GetChange(context.Background(), repo, 3); ch.State != "closed" || !ch.Fork {
		t.Errorf("%+v", ch)
	}
	// the list's next link has another scheme/host: provider_error, never requested
	if _, err := c.ListOpenChanges(context.Background(), repo, ""); gitapi.CodeOf(err) != gitapi.CodeError {
		t.Errorf("foreign next: %v", err)
	}
}

// ---- Data Center

func dcRepo(id int, slug string) map[string]any {
	return map[string]any{"id": id, "slug": slug, "project": map[string]any{"key": "PRJ"},
		"links": map[string]any{"self": []any{map[string]any{"href": "https://bb.example.com/projects/PRJ/repos/" + slug + "/browse"}}}}
}

func TestDCCheckToken(t *testing.T) {
	n := 1
	f := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/rest/api/1.0/projects" || r.URL.Query().Get("limit") != "1" {
			return false
		}
		vals := []any{}
		for i := 0; i < n; i++ {
			vals = append(vals, map[string]any{"key": "PRJ"})
		}
		js(w, 200, map[string]any{"values": vals, "isLastPage": true})
		return true
	})
	c := dc(t, f)
	if _, err := c.CheckToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	n = 0
	if _, err := c.CheckToken(context.Background()); gitapi.CodeOf(err) != gitapi.CodeForbidden {
		t.Errorf("empty: %v", err)
	}
}

func TestDCListRepos(t *testing.T) {
	f := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/rest/api/1.0/repos" {
			return false
		}
		q := r.URL.Query()
		if q.Get("permission") != "REPO_ADMIN" || q.Get("name") != "app" {
			t.Errorf("query %v", q)
		}
		start := 0
		fmt.Sscan(q.Get("start"), &start)
		js(w, 200, map[string]any{"values": []any{dcRepo(10+start, "app"), map[string]any{"id": 0, "slug": "", "project": map[string]any{}}},
			"isLastPage": false, "nextPageStart": start + 100})
		return true
	})
	c := dc(t, f)
	cursor, pages := "", 0
	for {
		pg, err := c.ListRepos(context.Background(), "app", cursor)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if len(pg.Items) != 1 { // the malformed row is dropped
			t.Fatalf("%+v", pg)
		}
		it := pg.Items[0]
		if it.ID != fmt.Sprintf("PRJ/app#%d", 10+(pages-1)*100) || it.Key != fmt.Sprint(10+(pages-1)*100) || it.Path != "PRJ/app" || !strings.Contains(it.URL, "/projects/PRJ/repos/app") {
			t.Fatalf("%+v", it)
		}
		if pg.Next == "" {
			break
		}
		cursor = pg.Next
	}
	if pages != gitapi.MaxPages {
		t.Errorf("pages %d", pages)
	}
	if _, err := c.ListRepos(context.Background(), "", "http://evil/"); !errors.Is(err, gitapi.ErrInvalidCursor) {
		t.Error("URL cursor")
	}
	// the last page ends the list
	last := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		js(w, 200, map[string]any{"values": []any{dcRepo(1, "a")}, "isLastPage": true})
		return true
	})
	if pg, err := dc(t, last).ListRepos(context.Background(), "", ""); err != nil || pg.Next != "" {
		t.Errorf("%+v %v", pg, err)
	}
}

func TestDCRenamedSlugIsFoundByID(t *testing.T) {
	f := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == "/rest/api/1.0/projects/PRJ/repos/old-slug" || strings.HasPrefix(r.URL.Path, "/rest/api/1.0/projects/PRJ/repos/old-slug/"):
			http.NotFound(w, r)
		case r.URL.Path == "/rest/api/1.0/projects/PRJ/repos" && r.URL.Query().Get("start") == "":
			js(w, 200, map[string]any{"values": []any{dcRepo(11, "other")}, "isLastPage": false, "nextPageStart": 100})
		case r.URL.Path == "/rest/api/1.0/projects/PRJ/repos":
			js(w, 200, map[string]any{"values": []any{dcRepo(12, "new-slug")}, "isLastPage": true})
		case r.URL.Path == "/rest/api/1.0/projects/PRJ/repos/new-slug":
			js(w, 200, dcRepo(12, "new-slug"))
		case r.URL.Path == "/rest/api/1.0/projects/PRJ/repos/new-slug/webhooks" && r.Method == "GET":
			js(w, 200, map[string]any{"values": []any{map[string]any{"id": 7, "url": "https://l/h"}}, "isLastPage": true})
		case r.URL.Path == "/rest/api/1.0/projects/PRJ/repos/new-slug/webhooks/7" && r.Method == "PUT":
			js(w, 200, map[string]any{"id": 7})
		case r.URL.Path == "/rest/api/1.0/projects/PRJ/repos/new-slug/pull-requests/4":
			js(w, 200, dcPR(4, "MERGED", 12, 12))
		default:
			return false
		}
		return true
	})
	c := dc(t, f)
	ref, err := c.GetRepo(context.Background(), "PRJ/old-slug#12")
	if err != nil || ref.ID != "PRJ/new-slug#12" || ref.Path != "PRJ/new-slug" || ref.Key != "12" {
		t.Fatalf("%+v %v", ref, err)
	}
	h, err := c.EnsureHook(context.Background(), scm.RepoRef{ID: "PRJ/old-slug#12"}, scm.HookSpec{URL: "https://l/h", Secret: "s"})
	if err != nil || h.ID != "7" {
		t.Fatalf("hook after rename: %v %v", h, err)
	}
	ch, err := c.GetChange(context.Background(), scm.RepoRef{ID: "PRJ/old-slug#12"}, 4)
	if err != nil || ch.Number != 4 || ch.State != "merged" {
		t.Fatalf("change after rename: %+v %v", ch, err)
	}
	// A repo that is really gone: the id is not in the project.
	if _, err := c.GetRepo(context.Background(), "PRJ/old-slug#999"); gitapi.CodeOf(err) != gitapi.CodeNotFound {
		t.Errorf("gone: %v", err)
	}
	for _, bad := range []string{"", "PRJ/app", "PRJ/app#x", "PRJ/../app#1", "PRJ/..#1", "../app#1", "./app#1", "PRJ/app#1/x", cloudID} {
		if _, err := c.GetRepo(context.Background(), bad); !errors.Is(err, gitapi.ErrBadRepo) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func dcPR(id int, state string, fromRepo, toRepo int) map[string]any {
	return map[string]any{
		"id": id, "title": "LAUN-5 fix", "description": "d", "state": state, "updatedDate": 1792490400000,
		"fromRef": map[string]any{"displayId": "LAUN-5-fix", "latestCommit": "abc", "repository": map[string]any{"id": fromRepo}},
		"toRef":   map[string]any{"repository": map[string]any{"id": toRepo}},
		"author":  map[string]any{"user": map[string]any{"slug": "dev", "name": "Dev Eloper"}},
		"links":   map[string]any{"self": []any{map[string]any{"href": fmt.Sprintf("https://bb.example.com/projects/PRJ/repos/app/pull-requests/%d", id)}}},
	}
}

func TestDCChangesAndHooks(t *testing.T) {
	var hooks []map[string]any
	const repoP = "/rest/api/1.0/projects/PRJ/repos/app"
	f := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == repoP+"/pull-requests/1":
			js(w, 200, dcPR(1, "OPEN", 5, 5))
		case r.URL.Path == repoP+"/pull-requests/2":
			js(w, 200, dcPR(2, "DECLINED", 6, 5))
		case r.URL.Path == repoP+"/pull-requests":
			q := r.URL.Query()
			if q.Get("state") != "OPEN" || q.Get("order") != "NEWEST" || q.Get("limit") != "100" {
				t.Errorf("query %v", q)
			}
			js(w, 200, map[string]any{"values": []any{dcPR(1, "OPEN", 5, 5)}, "isLastPage": false, "nextPageStart": 100})
		case r.URL.Path == repoP+"/webhooks" && r.Method == "GET":
			js(w, 200, map[string]any{"values": hooks, "isLastPage": true})
		case r.URL.Path == repoP+"/webhooks" && r.Method == "POST":
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			b["id"] = len(hooks) + 1
			hooks = append(hooks, b)
			js(w, 201, b)
		case strings.HasPrefix(r.URL.Path, repoP+"/webhooks/") && r.Method == "PUT":
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			for k, v := range b {
				hooks[0][k] = v
			}
			js(w, 200, hooks[0])
		case strings.HasPrefix(r.URL.Path, repoP+"/webhooks/") && r.Method == "DELETE":
			w.WriteHeader(204)
		default:
			return false
		}
		return true
	})
	c := dc(t, f)
	repo := scm.RepoRef{ID: "PRJ/app#5"}
	ch, err := c.GetChange(context.Background(), repo, 1)
	if err != nil || ch.State != "opened" || ch.Fork || ch.Author != "dev" || ch.SourceBranch != "LAUN-5-fix" || ch.HeadSHA != "abc" ||
		ch.UpdatedAt.Year() != 2026 || ch.Draft || !strings.HasSuffix(ch.URL, "/pull-requests/1") {
		t.Fatalf("%+v %v", ch, err)
	}
	if ch, _ = c.GetChange(context.Background(), repo, 2); ch.State != "closed" || !ch.Fork {
		t.Errorf("%+v", ch)
	}
	pg, err := c.ListOpenChanges(context.Background(), repo, "")
	if err != nil || len(pg.Items) != 1 || pg.Next == "" {
		t.Fatalf("%+v %v", pg, err)
	}
	spec := scm.HookSpec{URL: "https://lp.example/hook", Secret: "one"}
	h1, err := c.EnsureHook(context.Background(), repo, spec)
	if err != nil || h1.ID != "1" {
		t.Fatalf("%v %v", h1, err)
	}
	cfg := hooks[0]["configuration"].(map[string]any)
	if cfg["secret"] != "one" || hooks[0]["sslVerificationRequired"] != true || hooks[0]["active"] != true {
		t.Errorf("%v", hooks[0])
	}
	spec.Secret = "two"
	h2, err := c.EnsureHook(context.Background(), repo, spec)
	if err != nil || h2.ID != "1" || len(hooks) != 1 || hooks[0]["configuration"].(map[string]any)["secret"] != "two" {
		t.Fatalf("update %v %v %v", h2, err, hooks)
	}
	if err := c.DeleteHook(context.Background(), repo, h1); err != nil {
		t.Error(err)
	}
	if err := c.DeleteHook(context.Background(), repo, scm.HookRef{ID: "{x}"}); !errors.Is(err, gitapi.ErrBadRepo) {
		t.Error("bad hook id")
	}
}

func TestGuardedByDefault(t *testing.T) {
	f := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool { t.Error("reached"); return true })
	c, _ := bitbucket.NewAt(f.Srv.URL+"/2.0", tok, gitapi.Options{HTTP: gitapi.NewHTTPClient(&netguard.Dialer{})})
	if _, err := c.CheckToken(context.Background()); !errors.Is(err, netguard.ErrRefused) || gitapi.CodeOf(err) != gitapi.CodeUnreachable {
		t.Errorf("%v", err)
	}
}

// LAUN-84: the event list of a hook follows the spec (base, reviews, checks).
func TestHookEventsFollowTheSpec(t *testing.T) {
	has := func(events any, want ...string) bool {
		got := map[string]bool{}
		for _, e := range events.([]any) {
			got[e.(string)] = true
		}
		for _, w := range want {
			if !got[w] {
				return false
			}
		}
		return true
	}
	count := func(events any) int { return len(events.([]any)) }
	run := func(c *bitbucket.Client, repo scm.RepoRef, spec scm.HookSpec) {
		t.Helper()
		if _, err := c.EnsureHook(context.Background(), repo, spec); err != nil {
			t.Fatal(err)
		}
	}
	for _, flavour := range []string{"cloud", "dc"} {
		var last map[string]any
		f := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
			switch {
			case r.Method == "GET":
				js(w, 200, map[string]any{"values": []any{}, "isLastPage": true})
			case r.Method == "POST":
				last = nil
				_ = json.NewDecoder(r.Body).Decode(&last)
				last["uuid"], last["id"] = "{00000000-0000-0000-0000-000000000001}", 1
				js(w, 201, last)
			default:
				return false
			}
			return true
		})
		var c *bitbucket.Client
		var repo scm.RepoRef
		var base, reviews, checks []string
		if flavour == "cloud" {
			c, repo = cloud(t, f), scm.RepoRef{ID: cloudID}
			base, reviews, checks = bitbucket.CloudEvents, bitbucket.CloudReviewEvents, bitbucket.CloudCheckEvents
		} else {
			c, repo = dc(t, f), scm.RepoRef{ID: "PRJ/app#5"}
			base, reviews, checks = bitbucket.DCEvents, bitbucket.DCReviewEvents, bitbucket.DCCheckEvents
		}
		spec := scm.HookSpec{URL: "https://lp.example/hook", Secret: "s"}
		run(c, repo, spec)
		if !has(last["events"], base...) || count(last["events"]) != len(base) {
			t.Errorf("%s base events %v", flavour, last["events"])
		}
		spec.Reviews = true
		run(c, repo, spec)
		if !has(last["events"], append(append([]string{}, base...), reviews...)...) || count(last["events"]) != len(base)+len(reviews) {
			t.Errorf("%s review events %v", flavour, last["events"])
		}
		spec.Checks = true
		run(c, repo, spec)
		if !has(last["events"], append(append(append([]string{}, base...), reviews...), checks...)...) ||
			count(last["events"]) != len(base)+len(reviews)+len(checks) {
			t.Errorf("%s all events %v", flavour, last["events"])
		}
	}
	if !has(anyList(bitbucket.CloudCheckEvents), "repo:commit_status_created", "repo:commit_status_updated") ||
		!has(anyList(bitbucket.DCReviewEvents), "pr:reviewer:approved", "pr:reviewer:unapproved", "pr:reviewer:needs_work") {
		t.Error("event names changed")
	}
}

func anyList(s []string) any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// Data Center's build status event name is UNVERIFIED: a host that refuses the
// hook with a 400 gets it again without that event. Cloud's names are
// documented, so Cloud never retries.
func TestDCHookRetriesWithoutTheBuildStatusEvent(t *testing.T) {
	const repoP = "/rest/api/1.0/projects/PRJ/repos/app"
	var posts []map[string]any
	f := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == repoP+"/webhooks" && r.Method == "GET":
			js(w, 200, map[string]any{"values": []any{}, "isLastPage": true})
		case r.URL.Path == repoP+"/webhooks" && r.Method == "POST":
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			posts = append(posts, b)
			if strings.Contains(fmt.Sprint(b["events"]), "repo:build_status") {
				js(w, 400, map[string]any{"errors": []any{map[string]any{"message": "host text"}}})
				return true
			}
			b["id"] = 7
			js(w, 201, b)
		default:
			return false
		}
		return true
	})
	c := dc(t, f)
	h, err := c.EnsureHook(context.Background(), scm.RepoRef{ID: "PRJ/app#5"}, scm.HookSpec{URL: "https://lp.example/hook", Secret: "s", Reviews: true, Checks: true})
	if err != nil || h.ID != "7" || len(posts) != 2 {
		t.Fatalf("hook %v err %v posts %d", h, err, len(posts))
	}
	if strings.Contains(fmt.Sprint(posts[1]["events"]), "build_status") || !strings.Contains(fmt.Sprint(posts[1]["events"]), "pr:reviewer:approved") {
		t.Errorf("second request events %v", posts[1]["events"])
	}

	// A 400 with no check events asked for is an error, not a retry.
	posts = nil
	f2 := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == repoP+"/webhooks" && r.Method == "GET":
			js(w, 200, map[string]any{"values": []any{}, "isLastPage": true})
		case r.URL.Path == repoP+"/webhooks" && r.Method == "POST":
			posts = append(posts, nil)
			js(w, 400, map[string]any{})
		default:
			return false
		}
		return true
	})
	if _, err := dc(t, f2).EnsureHook(context.Background(), scm.RepoRef{ID: "PRJ/app#5"}, scm.HookSpec{URL: "https://lp.example/hook", Secret: "s"}); err == nil || len(posts) != 1 {
		t.Errorf("err %v posts %d", err, len(posts))
	}
}

// LAUN-96: the list answer carries the review, so the backfill reads it with
// no extra call. A payload without the field leaves the review unknown.
func TestChangesCarryTheReview(t *testing.T) {
	type rv = map[string]any
	cloudPR := func(id int, participants any) rv {
		m := rv{"id": id, "title": "t", "state": "OPEN", "updated_on": "2026-10-20T10:00:00+00:00",
			"source":      rv{"branch": rv{"name": "b"}, "commit": rv{"hash": "abc"}, "repository": rv{"uuid": repoUUID}},
			"destination": rv{"repository": rv{"uuid": repoUUID}},
			"links":       rv{"html": rv{"href": "https://bitbucket.org/acme/app/pull-requests/1"}}}
		if participants != nil {
			m["participants"] = participants
		}
		return m
	}
	fc := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.EscapedPath() != cloudPre+"/pullrequests" {
			return false
		}
		js(w, 200, rv{"values": []any{
			cloudPR(1, []any{rv{"approved": true, "state": "approved"}, rv{"approved": false, "state": nil}}),
			cloudPR(2, []any{rv{"approved": true, "state": "approved"}, rv{"approved": false, "state": "changes_requested"}}),
			cloudPR(3, []any{}),
			cloudPR(4, nil),
		}})
		return true
	})
	pg, err := cloud(t, fc).ListOpenChanges(context.Background(), scm.RepoRef{ID: cloudID}, "")
	if err != nil || len(pg.Items) != 4 {
		t.Fatalf("%+v %v", pg, err)
	}
	for i, w := range []struct {
		known  bool
		review scm.Review
	}{{true, scm.ReviewApproved}, {true, scm.ReviewChangesRequested}, {true, scm.ReviewNone}, {false, scm.ReviewNone}} {
		if it := pg.Items[i]; it.ReviewKnown != w.known || it.Review != w.review {
			t.Errorf("cloud %d: known %v review %q, want %v %q", i+1, it.ReviewKnown, it.Review, w.known, w.review)
		}
	}

	const repoP = "/rest/api/1.0/projects/PRJ/repos/app"
	withReviewers := func(id int, reviewers any) rv {
		m := dcPR(id, "OPEN", 5, 5)
		if reviewers != nil {
			m["reviewers"] = reviewers
		}
		return m
	}
	fd := gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != repoP+"/pull-requests" {
			return false
		}
		js(w, 200, rv{"isLastPage": true, "values": []any{
			withReviewers(1, []any{rv{"approved": true, "status": "APPROVED"}, rv{"approved": false, "status": "UNAPPROVED"}}),
			withReviewers(2, []any{rv{"approved": true, "status": "APPROVED"}, rv{"approved": false, "status": "NEEDS_WORK"}}),
			withReviewers(3, []any{rv{"approved": false, "status": "UNAPPROVED"}}),
			withReviewers(4, nil),
		}})
		return true
	})
	pg, err = dc(t, fd).ListOpenChanges(context.Background(), scm.RepoRef{ID: "PRJ/app#5"}, "")
	if err != nil || len(pg.Items) != 4 {
		t.Fatalf("%+v %v", pg, err)
	}
	for i, w := range []struct {
		known  bool
		review scm.Review
	}{{true, scm.ReviewApproved}, {true, scm.ReviewChangesRequested}, {true, scm.ReviewNone}, {false, scm.ReviewNone}} {
		if it := pg.Items[i]; it.ReviewKnown != w.known || it.Review != w.review {
			t.Errorf("dc %d: known %v review %q, want %v %q", i+1, it.ReviewKnown, it.Review, w.known, w.review)
		}
	}
}
