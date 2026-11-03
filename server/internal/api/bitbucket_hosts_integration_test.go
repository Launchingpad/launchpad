package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"devops/internal/bitbucket"
	"devops/internal/gitapi/gitapitest"
	"devops/internal/tracker"
)

// LAUN-84: token mode against a fake Bitbucket (Cloud and Data Center): the
// connect and check routes, available-repos, the repo map and its hook, the
// hook refresh, the backfill and the unmap. The fake answers the documented
// REST shapes the clients in internal/gitapi/bitbucket expect; no live host was
// called (decisions-T17, LAUN-84 "UNVERIFIED").

// bbHost is a fake Bitbucket Cloud (flavour "cloud", API root <url>/2.0) or Data
// Center (API root <url>/rest/api/1.0) holding one repository.
type bbHost struct {
	*gitapitest.Fake
	cloud bool
	mu    sync.Mutex
	hooks []map[string]any
	pulls []map[string]any
	seq   int
	// refuseBuildStatus answers a Data Center hook write that names the build
	// status event with 400 (the event name is UNVERIFIED).
	refuseBuildStatus bool
	fail              int // status of a hook write, 0: none
}

func (h *bbHost) repoJSON() map[string]any {
	if h.cloud {
		return map[string]any{"uuid": bbRepoUUID, "full_name": bbCloudPath, "workspace": map[string]any{"uuid": bbWS},
			"links": map[string]any{"html": map[string]any{"href": "https://bitbucket.example.test/" + bbCloudPath}}}
	}
	return map[string]any{"id": 4242, "slug": "example-app", "project": map[string]any{"key": "PROJ"},
		"links": map[string]any{"self": []any{map[string]any{"href": "https://bitbucket.example.test/projects/PROJ/repos/example-app/browse"}}}}
}

func newBBHost(t *testing.T, cloud bool) *bbHost {
	t.Helper()
	h := &bbHost{cloud: cloud}
	root, hooks, pulls, repoP := "/rest/api/1.0", "/webhooks", "/pull-requests", "/projects/PROJ/repos/example-app"
	if cloud {
		root, hooks, pulls, repoP = "/2.0", "/hooks", "/pullrequests", "/repositories/"+bbWS+"/"+bbRepoUUID
	}
	js := gitapitest.JSON
	h.Fake = gitapitest.NewFake(t, func(w http.ResponseWriter, r *http.Request) bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		p := strings.TrimPrefix(r.URL.Path, root)
		if p == r.URL.Path {
			return false
		}
		switch {
		case cloud && p == "/user":
			js(w, 200, map[string]any{"nickname": "bb-user", "display_name": "BB User"})
		case !cloud && p == "/projects":
			js(w, 200, map[string]any{"values": []any{map[string]any{"key": "PROJ"}}, "isLastPage": true})
		case p == "/repositories" && cloud, p == "/repos" && !cloud:
			js(w, 200, map[string]any{"values": []any{h.repoJSON()}, "isLastPage": true})
		case p == repoP:
			js(w, 200, h.repoJSON())
		case p == repoP+pulls && r.Method == http.MethodGet:
			js(w, 200, map[string]any{"values": h.pulls, "isLastPage": true})
		case p == repoP+hooks && r.Method == http.MethodGet:
			js(w, 200, map[string]any{"values": h.hooks, "isLastPage": true})
		case p == repoP+hooks && r.Method == http.MethodPost, strings.HasPrefix(p, repoP+hooks+"/") && r.Method == http.MethodPut:
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			if h.fail != 0 {
				js(w, h.fail, map[string]any{"error": "host words that must never be echoed"})
				return true
			}
			if h.refuseBuildStatus && strings.Contains(fmt.Sprint(b["events"]), "repo:build_status") {
				js(w, 400, map[string]any{"errors": []any{map[string]any{"message": "unknown event"}}})
				return true
			}
			if r.Method == http.MethodPost {
				h.seq++
				if cloud {
					b["uuid"] = fmt.Sprintf("{00000000-0000-0000-0000-%012d}", h.seq)
				} else {
					b["id"] = h.seq
				}
				h.hooks = append(h.hooks, b)
				js(w, 201, b)
				return true
			}
			for k, v := range b {
				h.hooks[0][k] = v
			}
			js(w, 200, h.hooks[0])
		case strings.HasPrefix(p, repoP+hooks+"/") && r.Method == http.MethodDelete:
			h.hooks = nil
			w.WriteHeader(204)
		default:
			return false
		}
		return true
	})
	return h
}

func (h *bbHost) hook() (events []string, secret, url string, n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n = len(h.hooks)
	if n == 0 {
		return
	}
	k := h.hooks[0]
	if raw, ok := k["events"].([]any); ok {
		for _, e := range raw {
			events = append(events, fmt.Sprint(e))
		}
	}
	if h.cloud {
		secret = fmt.Sprint(k["secret"])
	} else if c, ok := k["configuration"].(map[string]any); ok {
		secret = fmt.Sprint(c["secret"])
	}
	url = fmt.Sprint(k["url"])
	return
}

func (h *bbHost) setEvents(ev ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hooks[0]["events"] = ev
}

// cloudPull and dcPull are open pull requests in the host's own shape.
func cloudPull(n int, ident, title string, draft bool, forkUUID string) map[string]any {
	branch := fmt.Sprintf("feature/x%d", n)
	if ident != "" {
		branch, title = "chen/"+strings.ToLower(ident)+"-x", ident+": "+title
	}
	src := bbRepoUUID
	if forkUUID != "" {
		src = forkUUID
	}
	return map[string]any{
		"id": n, "title": title, "description": "", "state": "OPEN", "draft": draft, "updated_on": fmt.Sprintf("2026-10-20T09:%02d:00.000000+00:00", n),
		"author":      map[string]any{"nickname": "example-dev"},
		"source":      map[string]any{"branch": map[string]any{"name": branch}, "commit": map[string]any{"hash": fmt.Sprintf("%040d", n)}, "repository": map[string]any{"uuid": src}},
		"destination": map[string]any{"repository": map[string]any{"uuid": bbRepoUUID}},
		"links":       map[string]any{"html": map[string]any{"href": fmt.Sprintf("https://bitbucket.example.test/%s/pull-requests/%d", bbCloudPath, n)}},
	}
}

func dcPull(n int, ident, title string, draft bool, fromRepo int) map[string]any {
	branch := fmt.Sprintf("feature/x%d", n)
	if ident != "" {
		branch, title = "chen/"+strings.ToLower(ident)+"-x", ident+": "+title
	}
	return map[string]any{
		"id": n, "title": title, "description": "", "state": "OPEN", "draft": draft, "updatedDate": 1793000000000 + int64(n)*60000,
		"author":  map[string]any{"user": map[string]any{"slug": "example-dev"}},
		"fromRef": map[string]any{"displayId": branch, "latestCommit": fmt.Sprintf("%040d", n), "repository": map[string]any{"id": fromRepo}},
		"toRef":   map[string]any{"repository": map[string]any{"id": 4242}},
		"links":   map[string]any{"self": []any{map[string]any{"href": fmt.Sprintf("https://bitbucket.example.test/projects/PROJ/repos/example-app/pull-requests/%d", n)}}},
	}
}

func (g *gitEnv) bbCreate(h *bbHost) tracker.GitIntegrationCreated {
	g.t.Helper()
	body := map[string]any{"provider": "bitbucket", "baseUrl": h.Srv.URL, "mode": "token", "token": "bb-SECRET-access-token"}
	if h.cloud {
		body["apiBaseUrl"] = h.Srv.URL + "/2.0"
	}
	var gi tracker.GitIntegrationCreated
	g.must(http.StatusCreated, g.create(body)).json(g.t, &gi)
	return gi
}

// bb returns a Bitbucket test driver on the same server.
func (g *gitEnv) bb() *bbEnv { return &bbEnv{ghEnv: &ghEnv{env: g.env, kr: g.kr}} }

// AC: Cloud and Data Center in token mode: connect, check, list, map (the hook
// gets the full event set and the integration's secret), refresh, backfill, unmap.
func TestBitbucketHosts_TokenMode(t *testing.T) {
	for _, cloud := range []bool{true, false} {
		name, key, path, repoID, openEv := "datacenter", bbDCKey, bbDCPath, "PROJ/example-app#4242", bitbucket.EventDCOpened
		wantEvents := []string{"pr:opened", "pr:from_ref_updated", "pr:modified", "pr:merged", "pr:declined",
			"pr:reviewer:approved", "pr:reviewer:unapproved", "pr:reviewer:needs_work", "repo:build_status"}
		fx := "dc_pr_opened.json"
		if cloud {
			name, key, path, repoID, openEv = "cloud", bbCloudKey, bbCloudPath, bbCloudKey, bitbucket.EventCloudCreated
			wantEvents = []string{"pullrequest:created", "pullrequest:updated", "pullrequest:fulfilled", "pullrequest:rejected",
				"pullrequest:approved", "pullrequest:unapproved", "repo:commit_status_created", "repo:commit_status_updated"}
			fx = "cloud_pr_created.json"
		}
		t.Run(name, func(t *testing.T) {
			g := newGitEnv(t, gitEnvOpts{})
			h := newBBHost(t, cloud)
			gi := g.bbCreate(h)
			if gi.Provider != "bitbucket" || gi.Mode != "token" || !gi.TokenSet || gi.WebhookURL != gitPublic+"/api/v1/orgs/orbit/integrations/bitbucket/webhook" {
				t.Fatalf("integration = %+v", gi)
			}
			// Check: Cloud names the user; Data Center has no who-am-I and answers an empty login.
			var chk tracker.GitIntegrationCheck
			g.must(http.StatusOK, g.do("POST", g.path(gi.ID, "/check"), g.chen, nil)).json(t, &chk)
			if wantLogin := map[bool]string{true: "bb-user", false: ""}[cloud]; chk.Login != wantLogin || chk.Status != "ok" {
				t.Fatalf("check = %+v", chk)
			}
			if got := h.Fake.Count("GET "); got == 0 {
				t.Fatal("the host was not called")
			}
			// The token went to the host as a Bearer token, to its origin only.
			for _, a := range h.Fake.Auths() {
				if a != "Bearer bb-SECRET-access-token" {
					t.Fatalf("Authorization = %q", a)
				}
			}

			var page struct {
				Items []tracker.AvailableRepo `json:"items"`
			}
			g.must(http.StatusOK, g.do("GET", g.path(gi.ID, "/available-repos"), g.chen, nil)).json(t, &page)
			if len(page.Items) != 1 || page.Items[0].RepoID != repoID || page.Items[0].RepoKey != key || page.Items[0].RepoPath != path || page.Items[0].Mapped {
				t.Fatalf("available repos = %+v", page.Items)
			}

			// Map: the hook is made FIRST, with the base, review and check events and the secret.
			var repo tracker.GitRepo
			g.must(http.StatusCreated, g.do("POST", g.path(gi.ID, "/repos"), g.chen, map[string]any{"repoId": repoID})).json(t, &repo)
			if repo.RepoKey != key || repo.RepoPath != path || repo.HookState != "active" {
				t.Fatalf("repo = %+v", repo)
			}
			events, secret, hookURL, n := h.hook()
			if n != 1 || strings.Join(events, " ") != strings.Join(wantEvents, " ") || secret == "" || hookURL != gitPublic+"/api/v1/orgs/orbit/integrations/bitbucket/webhook" {
				t.Fatalf("host hook: %d %v %q %q", n, events, secret, hookURL)
			}
			// That secret opens the webhook (an event for the mapped repo links the issue).
			b := g.bb()
			is := g.createIssue(g.chen, map[string]any{"title": "live"})
			g.setState(is.Identifier, "Todo")
			r := b.result(b.hook("orbit", secret, openEv, uuid.NewString(), bbFixture(t, fx, bbNaming(is.Identifier, ""))), http.StatusOK)
			if len(r.Linked) != 1 || len(r.Moved) != 1 {
				t.Fatalf("live event = %+v", r)
			}
			if b.hook("orbit", secret+"x", openEv, uuid.NewString(), []byte("{}")).status != http.StatusUnauthorized {
				t.Fatal("a wrong secret passed")
			}
			// The repo is now mapped.
			g.must(http.StatusOK, g.do("GET", g.path(gi.ID, "/available-repos"), g.chen, nil)).json(t, &page)
			if !page.Items[0].Mapped {
				t.Fatal("available-repos does not show the repo as mapped")
			}
			g.must(http.StatusConflict, g.do("POST", g.path(gi.ID, "/repos"), g.chen, map[string]any{"repoId": repoID}))

			// Refresh: a hook from before the review and check events learns them; same hook, same secret.
			refresh := g.path(gi.ID, "/repos/"+repo.ID.String()+"/hook/refresh")
			h.setEvents(wantEvents[:4]...)
			var out tracker.HookRefreshResult
			g.must(http.StatusOK, g.do("POST", refresh, g.chen, nil)).json(t, &out)
			events2, secret2, _, n2 := h.hook()
			if out.HookState != "active" || n2 != 1 || secret2 != secret || strings.Join(events2, " ") != strings.Join(wantEvents, " ") {
				t.Fatalf("refresh: %+v, %d hooks, events %v", out, n2, events2)
			}
			// A host failure: our provider code, no host words, the repo is marked failed.
			h.fail = 500
			if r := g.do("POST", refresh, g.chen, nil); r.status != http.StatusBadGateway || r.errCode(t) != "provider_error" || strings.Contains(string(r.body), "host words") {
				t.Fatalf("host failure: %d %s", r.status, r.body)
			}
			h.fail = 0
			g.must(http.StatusOK, g.do("POST", refresh, g.chen, nil))

			// Backfill: open pull requests link like live events; a fork is read and skipped; forward only.
			mk := func(title, state string) tracker.Issue {
				i := g.createIssue(g.chen, map[string]any{"title": title})
				g.setState(i.Identifier, state)
				return i
			}
			i1, i2, i3, i4 := mk("open one", "Todo"), mk("draft one", "Todo"), mk("already done", "Done"), mk("fork one", "Todo")
			if cloud {
				h.pulls = []map[string]any{cloudPull(1, i1.Identifier, "open", false, ""), cloudPull(2, i2.Identifier, "draft", true, ""),
					cloudPull(3, i3.Identifier, "done", false, ""), cloudPull(4, i4.Identifier, "fork", false, "{99999999-8888-7777-6666-555555555555}")}
			} else {
				h.pulls = []map[string]any{dcPull(1, i1.Identifier, "open", false, 4242), dcPull(2, i2.Identifier, "draft", true, 4242),
					dcPull(3, i3.Identifier, "done", false, 4242), dcPull(4, i4.Identifier, "fork", false, 7777)}
			}
			var bf tracker.BackfillResult
			g.must(http.StatusOK, g.do("POST", g.path(gi.ID, "/repos/"+repo.ID.String()+"/backfill"), g.chen, nil)).json(t, &bf)
			if bf.Seen != 4 || bf.Linked != 3 || bf.Moved != 1 || bf.Truncated {
				t.Fatalf("backfill = %+v, want seen 4, linked 3 (not the fork), moved 1", bf)
			}
			if g.issueFull(i1.Identifier).State.Name != "In Review" || g.issueFull(i2.Identifier).State.Name != "Todo" ||
				g.issueFull(i3.Identifier).State.Name != "Done" || g.issueFull(i4.Identifier).State.Name != "Todo" {
				t.Fatal("backfill moved the wrong issues")
			}
			if cr := g.oneCR(i2.Identifier); cr.State != tracker.CRDraft || cr.Provider != "bitbucket" || cr.RepoPath != path {
				t.Fatalf("backfilled draft block = %+v", cr)
			}
			g.must(http.StatusOK, g.do("POST", g.path(gi.ID, "/repos/"+repo.ID.String()+"/backfill"), g.chen, nil)).json(t, &bf)
			if bf.Linked != 0 || bf.Moved != 0 {
				t.Fatalf("second backfill = %+v", bf)
			}

			// Unmap: the hook is deleted on the host by its stored id.
			var rm tracker.GitRepoRemoved
			g.must(http.StatusOK, g.do("DELETE", g.path(gi.ID, "/repos/"+repo.ID.String()), g.chen, nil)).json(t, &rm)
			if _, _, _, n := h.hook(); !rm.HookRemoved || n != 0 {
				t.Fatalf("unmap: %+v, %d hooks left", rm, n)
			}
			if strings.Contains(g.logs.String(), "bb-SECRET-access-token") || strings.Contains(g.logs.String(), secret) {
				t.Fatal("a token or a secret is in the log")
			}
		})
	}
}

// Data Center's build status event name is UNVERIFIED: a host that refuses the
// hook because of it still gets the hook, without that event.
func TestBitbucketHosts_DataCenterRefusesBuildStatus(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	h := newBBHost(t, false)
	h.refuseBuildStatus = true
	gi := g.bbCreate(h)
	var repo tracker.GitRepo
	g.must(http.StatusCreated, g.do("POST", g.path(gi.ID, "/repos"), g.chen, map[string]any{"repoId": "PROJ/example-app#4242"})).json(t, &repo)
	events, _, _, n := h.hook()
	if repo.HookState != "active" || n != 1 || strings.Contains(strings.Join(events, " "), "build_status") || !strings.Contains(strings.Join(events, " "), "pr:reviewer:approved") {
		t.Fatalf("hook state %s, %d hooks, events %v", repo.HookState, n, events)
	}
}

// A cross-org check: another org's admin cannot see or touch this integration's
// repo map, and a repoId that is not a Bitbucket id is a 400.
func TestBitbucketHosts_CrossOrgAndBadIDs(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	h := newBBHost(t, true)
	gi := g.bbCreate(h)
	for _, bad := range []string{"4242", "x/y", bbWS, "../etc/passwd", "{11111111-2222-3333-4444-555555555555}/../x"} {
		if r := g.do("POST", g.path(gi.ID, "/repos"), g.chen, map[string]any{"repoId": bad}); r.status != http.StatusBadRequest {
			t.Fatalf("repoId %q: %d %s, want 400", bad, r.status, r.body)
		}
	}
	// Another org: 404 on the integration.
	g.newOrg("acme", "ACME")
	if r := g.do("GET", "/api/v1/orgs/acme/integrations/"+gi.ID.String(), g.chen, nil); r.status != http.StatusNotFound && r.status != http.StatusForbidden {
		t.Fatalf("cross-org read: %d", r.status)
	}
}

// Webhook-only: Bitbucket integrations make a server secret shown once; the repo
// key is validated per provider (a numeric id for Data Center, the uuid pair
// for Cloud, stored in lower case); a Cloud key is refused for GitLab/GitHub.
func TestBitbucketHosts_WebhookOnlyRepoKeys(t *testing.T) {
	g := newGitEnv(t, gitEnvOpts{})
	var gi tracker.GitIntegrationCreated
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "bitbucket", "baseUrl": "https://bitbucket.org", "name": "Bitbucket"})).json(t, &gi)
	if len(gi.WebhookSecret) < 32 || gi.Mode != "webhook_only" || gi.Flavor == nil || *gi.Flavor != "cloud" ||
		gi.WebhookURL != gitPublic+"/api/v1/orgs/orbit/integrations/bitbucket/webhook" {
		t.Fatalf("integration = %+v", gi)
	}
	// A pasted secret is for GitLab only.
	g.must(http.StatusBadRequest, g.create(map[string]any{"provider": "bitbucket", "baseUrl": "https://bb.example.test", "secret": strings.Repeat("a", 40)}))
	// The secret opens the webhook, and a self-hosted host is a "self_hosted" flavour.
	b := g.bb()
	if b.hook("orbit", gi.WebhookSecret, "diagnostics:ping", "x", []byte("{}")).status != http.StatusOK {
		t.Fatal("the generated secret does not open the webhook")
	}
	var dc tracker.GitIntegrationCreated
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "bitbucket", "baseUrl": "https://bitbucket.example.test"})).json(t, &dc)
	if dc.Flavor == nil || *dc.Flavor != "self_hosted" {
		t.Fatalf("flavor = %v", dc.Flavor)
	}

	post := func(id uuid.UUID, key, path string) resp {
		return g.do("POST", g.path(id, "/repos"), g.chen, map[string]any{"repoKey": key, "repoPath": path})
	}
	var repo tracker.GitRepo
	g.must(http.StatusCreated, post(gi.ID, strings.ToUpper(bbCloudKey), bbCloudPath)).json(t, &repo)
	if repo.RepoKey != bbCloudKey {
		t.Fatalf("Cloud key stored as %q, want lower case", repo.RepoKey)
	}
	g.must(http.StatusConflict, post(gi.ID, bbCloudKey, "same repo again"))
	g.must(http.StatusCreated, post(dc.ID, "4242", bbDCPath))
	for _, bad := range []string{"", "abc", "0", "PROJ/example-app", "{x}/{y}", bbWS, bbCloudKey + "/x", strings.Repeat("9", 19)} {
		if r := post(dc.ID, bad, "x/y"); r.status != http.StatusBadRequest {
			t.Fatalf("repoKey %q: %d, want 400", bad, r.status)
		}
	}
	// The webhook now finds the Cloud repo under the lower-case key.
	is := g.createIssue(g.chen, map[string]any{"title": "keys"})
	res := b.result(b.hook("orbit", gi.WebhookSecret, bitbucket.EventCloudCreated, "k1", bbFixture(t, "cloud_pr_created.json", bbNaming(is.Identifier, ""))), http.StatusOK)
	if len(res.Linked) != 1 {
		t.Fatalf("webhook for a mapped Cloud repo = %+v", res)
	}

	// GitHub / GitLab keep the numeric rule.
	var gh tracker.GitIntegrationCreated
	g.must(http.StatusCreated, g.create(map[string]any{"provider": "github", "baseUrl": "https://github.com"})).json(t, &gh)
	g.must(http.StatusBadRequest, post(gh.ID, bbCloudKey, "a/b"))
	g.must(http.StatusCreated, post(gh.ID, "0042", "a/b"))
}
