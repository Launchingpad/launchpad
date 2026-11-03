package api

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"devops/internal/tracker"
)

type page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

func identifiers(items []tracker.Issue) []string {
	out := make([]string, len(items))
	for i, is := range items {
		out[i] = is.Identifier
	}
	return out
}

// AC: create, list, filter and update issues with a token.
func TestIssues_CreateGetListFilterUpdate(t *testing.T) {
	e := newEnv(t)
	long := strings.Repeat("é", 450)
	a := e.createIssue(e.chen, map[string]any{"title": "Login crash on iPad", "description": long,
		"priority": 1, "labels": []string{"Bug"}, "assignee": "me"})
	b := e.createIssue(e.chen, map[string]any{"title": "Dark mode", "state": "Todo", "labels": []string{"feature"}, "assignee": "claude"})
	c := e.createIssue(e.claude, map[string]any{"title": "Refactor auth", "state": "In Progress", "priority": 3, "parent": a.Identifier})

	// Create response shape.
	if a.Identifier != "ORB-1" || b.Identifier != "ORB-2" || c.Identifier != "ORB-3" {
		t.Fatalf("identifiers %s %s %s", a.Identifier, b.Identifier, c.Identifier)
	}
	if a.State.Name != "Backlog" || a.Assignee == nil || a.Assignee.Username != "chen" || a.Version != 1 {
		t.Fatalf("issue a = %+v", a)
	}
	if a.GitBranchName != "chen/orb-1-login-crash-on-ipad" {
		t.Fatalf("gitBranchName = %q", a.GitBranchName)
	}
	if b.GitBranchName != "claude/orb-2-dark-mode" {
		t.Fatalf("gitBranchName follows the assignee: %q", b.GitBranchName)
	}
	if c.Parent == nil || c.Parent.Identifier != "ORB-1" || c.StartedAt == nil {
		t.Fatalf("issue c = %+v", c)
	}
	if len(a.Labels) != 1 || a.Labels[0].Name != "Bug" || a.Description != long || a.DescriptionTruncated {
		t.Fatalf("issue a labels/description wrong: %+v", a.Labels)
	}

	// Get by identifier (any case) and by UUID.
	for _, ref := range []string{"ORB-1", "orb-1", a.ID.String()} {
		var got tracker.Issue
		e.must(200, e.do("GET", "/api/v1/orgs/orbit/issues/"+ref, e.claude, nil)).json(t, &got)
		if got.ID != a.ID {
			t.Fatalf("GET %s returned %s", ref, got.Identifier)
		}
	}
	if code := e.must(404, e.do("GET", "/api/v1/orgs/orbit/issues/ORB-999", e.chen, nil)).errCode(t); code != "not_found" {
		t.Fatalf("code = %s", code)
	}

	list := func(q string) []string {
		t.Helper()
		var p page[tracker.Issue]
		e.must(200, e.do("GET", "/api/v1/orgs/orbit/issues?"+q, e.chen, nil)).json(t, &p)
		return identifiers(p.Items)
	}
	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"all, newest update first", "", []string{"ORB-3", "ORB-2", "ORB-1"}},
		{"orderBy createdAt", "orderBy=createdAt", []string{"ORB-3", "ORB-2", "ORB-1"}},
		{"team key", "team=ORB", []string{"ORB-3", "ORB-2", "ORB-1"}},
		{"state name", "state=todo", []string{"ORB-2"}},
		{"state list", "state=Todo,Backlog", []string{"ORB-2", "ORB-1"}},
		{"stateType", "stateType=started", []string{"ORB-3"}},
		{"assignee me", "assignee=me", []string{"ORB-1"}},
		{"assignee username", "assignee=claude", []string{"ORB-2"}},
		{"assignee none", "assignee=none", []string{"ORB-3"}},
		{"label", "label=bug", []string{"ORB-1"}},
		{"priority", "priority=3", []string{"ORB-3"}},
		{"parent", "parent=ORB-1", []string{"ORB-3"}},
		{"query title", "query=dark", []string{"ORB-2"}},
		{"query identifier", "query=ORB-3", []string{"ORB-3"}},
		{"query literal percent", "query=" + url.QueryEscape("%"), []string{}},
		{"createdAt duration", "createdAt=-P1D", []string{"ORB-3", "ORB-2", "ORB-1"}},
		{"updatedAt future timestamp", "updatedAt=2999-01-01T00:00:00Z", []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := list(tt.query); !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}

	// List cuts the description; full=true does not.
	var p page[tracker.Issue]
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/issues?query=Login", e.chen, nil)).json(t, &p)
	if got := p.Items[0]; !got.DescriptionTruncated || len([]rune(got.Description)) != 400 {
		t.Fatalf("list description: truncated=%v runes=%d", got.DescriptionTruncated, len([]rune(got.Description)))
	}
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/issues?query=Login&full=true", e.chen, nil)).json(t, &p)
	if got := p.Items[0]; got.DescriptionTruncated || got.Description != long {
		t.Fatal("full=true still cut the description")
	}

	// Keyset pagination: limit=2 then the cursor.
	var p1, p2 page[tracker.Issue]
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/issues?limit=2", e.chen, nil)).json(t, &p1)
	if len(p1.Items) != 2 || p1.NextCursor == nil {
		t.Fatalf("page 1: %d items, cursor %v", len(p1.Items), p1.NextCursor)
	}
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/issues?limit=2&cursor="+url.QueryEscape(*p1.NextCursor), e.chen, nil)).json(t, &p2)
	if got := append(identifiers(p1.Items), identifiers(p2.Items)...); !slices.Equal(got, []string{"ORB-3", "ORB-2", "ORB-1"}) || p2.NextCursor != nil {
		t.Fatalf("pages: %v, last cursor %v", got, p2.NextCursor)
	}

	// Bad filters are 400 validation_failed, not "match everything".
	for _, q := range []string{"limit=251", "stateType=done", "priority=9", "createdAt=yesterday", "asignee=me", "cursor=garbage", "orderBy=title", "parent=ORB-999", "parent=" + url.QueryEscape("01a16d1e-fd0c-797c-8e74-679e26a7276f")} {
		if code := e.must(400, e.do("GET", "/api/v1/orgs/orbit/issues?"+q, e.chen, nil)).errCode(t); code != "validation_failed" {
			t.Fatalf("%s: code %s", q, code)
		}
	}

	// PATCH: several fields; absent fields stay; null clears.
	var upd tracker.Issue
	e.must(200, e.do("PATCH", "/api/v1/orgs/orbit/issues/ORB-2", e.chen, map[string]any{
		"title": "Dark mode everywhere", "priority": 2, "addLabels": []string{"Improvement"}, "assignee": nil, "dueDate": "2026-12-01",
	})).json(t, &upd)
	if upd.Title != "Dark mode everywhere" || upd.Priority != 2 || upd.Assignee != nil || upd.Version != 2 ||
		len(upd.Labels) != 2 || upd.DueDate == nil || *upd.DueDate != "2026-12-01" || upd.State.Name != "Todo" {
		t.Fatalf("after PATCH: %+v", upd)
	}
	e.must(200, e.do("PATCH", "/api/v1/orgs/orbit/issues/ORB-2", e.chen, map[string]any{"labels": []string{}})).json(t, &upd)
	if len(upd.Labels) != 0 || upd.Version != 3 {
		t.Fatalf("labels replace: %+v", upd.Labels)
	}
	// A no-op PATCH does not bump the version.
	e.must(200, e.do("PATCH", "/api/v1/orgs/orbit/issues/ORB-2", e.chen, map[string]any{"title": "Dark mode everywhere"})).json(t, &upd)
	if upd.Version != 3 {
		t.Fatalf("no-op bumped version to %d", upd.Version)
	}
	// Invalid PATCH values.
	for name, body := range map[string]map[string]any{
		"null title":     {"title": nil},
		"bad state":      {"state": "Shipped"},
		"bad assignee":   {"assignee": "nobody"},
		"unknown field":  {"team": "ORB"},
		"self parent":    {"parent": "ORB-2"},
		"parent cycle":   {"parent": "ORB-3"},
		"labels and add": {"labels": []string{}, "addLabels": []string{"Bug"}},
	} {
		ref := "ORB-2"
		if name == "parent cycle" {
			ref = "ORB-1" // ORB-3's parent is ORB-1
		}
		r := e.do("PATCH", "/api/v1/orgs/orbit/issues/"+ref, e.chen, body)
		if r.status != 400 {
			t.Fatalf("%s: status %d body %s", name, r.status, r.body)
		}
	}
}

// AC: two concurrent creates get different numbers (real Postgres).
func TestIssues_ConcurrentCreatesGetDistinctNumbers(t *testing.T) {
	e := newEnv(t)
	const n = 24
	var wg sync.WaitGroup
	numbers := make(chan int32, n)
	errs := make(chan string, n)
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start // release every request at once
			r := e.doNoFatal("POST", "/api/v1/orgs/orbit/issues", e.chen, map[string]any{"team": "ORB", "title": fmt.Sprintf("race %d", i)})
			if r.status != http.StatusCreated {
				errs <- fmt.Sprintf("status %d: %s", r.status, r.body)
				return
			}
			var is tracker.Issue
			if err := jsonUnmarshal(r.body, &is); err != nil {
				errs <- err.Error()
				return
			}
			numbers <- is.Number
		})
	}
	close(start)
	wg.Wait()
	close(numbers)
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}
	var got []int
	for num := range numbers {
		got = append(got, int(num))
	}
	slices.Sort(got)
	if len(got) != n {
		t.Fatalf("got %d numbers, want %d", len(got), n)
	}
	for i, v := range got {
		if v != i+1 {
			t.Fatalf("numbers are not distinct and contiguous: %v", got)
		}
	}
}

// AC: a state change writes a history row and an event.
func TestIssues_StateChangeWritesHistoryAndEvent(t *testing.T) {
	e := newEnv(t)
	is := e.createIssue(e.chen, map[string]any{"title": "Ship it", "state": "Todo"})
	var after tracker.Issue
	e.must(200, e.do("PATCH", "/api/v1/orgs/orbit/issues/"+is.Identifier, e.claude, map[string]any{"state": "In Progress"})).json(t, &after)
	if after.State.Name != "In Progress" || after.StartedAt == nil || after.CompletedAt != nil {
		t.Fatalf("after start: %+v", after)
	}
	e.must(200, e.do("PATCH", "/api/v1/orgs/orbit/issues/"+is.Identifier, e.claude, map[string]any{"state": "Done"})).json(t, &after)
	if after.CompletedAt == nil || after.CanceledAt != nil {
		t.Fatalf("after done: completedAt=%v canceledAt=%v", after.CompletedAt, after.CanceledAt)
	}
	e.must(200, e.do("PATCH", "/api/v1/orgs/orbit/issues/"+is.Identifier, e.claude, map[string]any{"state": "Todo"})).json(t, &after)
	if after.CompletedAt != nil || after.StartedAt != nil {
		t.Fatalf("reopened issue kept timestamps: %+v", after)
	}

	var full tracker.Issue
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/issues/"+is.Identifier+"?include=stateHistory", e.chen, nil)).json(t, &full)
	var path []string
	for _, h := range full.StateHistory {
		from := "-"
		if h.FromStateName != nil {
			from = *h.FromStateName
		}
		path = append(path, from+">"+h.ToStateName)
	}
	want := []string{"->Todo", "Todo>In Progress", "In Progress>Done", "Done>Todo"}
	if !slices.Equal(path, want) {
		t.Fatalf("history %v, want %v", path, want)
	}
	if full.StateHistory[1].Actor == nil || full.StateHistory[1].Actor.Username != "claude" || full.StateHistory[1].Source != "api" {
		t.Fatalf("history actor/source: %+v", full.StateHistory[1])
	}

	var evs page[tracker.Event]
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/events?after=0", e.chen, nil)).json(t, &evs)
	var updates int
	var lastSeq int64
	for _, ev := range evs.Items {
		if ev.Seq <= lastSeq {
			t.Fatalf("seq not increasing: %d after %d", ev.Seq, lastSeq)
		}
		lastSeq = ev.Seq
		if ev.Type == "issue.updated" && ev.EntityID == is.ID {
			updates++
			if !strings.Contains(string(ev.Payload), `"toStateId"`) || !strings.Contains(string(ev.Payload), `"state"`) {
				t.Fatalf("state event payload: %s", ev.Payload)
			}
		}
	}
	if updates != 3 || evs.Items[0].Type != "issue.created" {
		t.Fatalf("events: %d issue.updated (want 3), first %q", updates, evs.Items[0].Type)
	}
	// after= pages forward.
	var tail page[tracker.Event]
	e.must(200, e.do("GET", fmt.Sprintf("/api/v1/orgs/orbit/events?after=%d", evs.Items[1].Seq), e.chen, nil)).json(t, &tail)
	if len(tail.Items) != len(evs.Items)-2 {
		t.Fatalf("after=%d returned %d items", evs.Items[1].Seq, len(tail.Items))
	}
}

// AC: PATCH with a stale version returns 409 version_conflict.
func TestIssues_PatchStaleVersionConflict(t *testing.T) {
	e := newEnv(t)
	is := e.createIssue(e.chen, map[string]any{"title": "CAS"})
	var v2 tracker.Issue
	e.must(200, e.do("PATCH", "/api/v1/orgs/orbit/issues/ORB-1", e.chen, map[string]any{"version": 1, "title": "CAS 2"})).json(t, &v2)
	if v2.Version != 2 {
		t.Fatalf("version = %d", v2.Version)
	}
	r := e.must(409, e.do("PATCH", "/api/v1/orgs/orbit/issues/ORB-1", e.claude, map[string]any{"version": 1, "title": "lost update"}))
	if code := r.errCode(t); code != "version_conflict" {
		t.Fatalf("code = %s", code)
	}
	var now tracker.Issue
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/issues/"+is.ID.String(), e.chen, nil)).json(t, &now)
	if now.Title != "CAS 2" || now.Version != 2 {
		t.Fatalf("stale write landed: %+v", now)
	}
	// Without version, last write wins.
	e.must(200, e.do("PATCH", "/api/v1/orgs/orbit/issues/ORB-1", e.claude, map[string]any{"title": "CAS 3"}))
}

// AC: a read-scoped token cannot write; scope and role checks.
func TestAuth_Scopes(t *testing.T) {
	e := newEnv(t)
	chen := e.user("chen")
	claude := e.user("claude")
	readTok, _ := e.newToken(claude.ID, tokenOpts{pin: &e.orgID, scopes: []string{"read"}})
	writeTok, _ := e.newToken(claude.ID, tokenOpts{pin: &e.orgID, scopes: []string{"write"}})
	// claude is a member, not an admin: an admin-scoped token still cannot manage tokens.
	memberAdminTok, _ := e.newToken(claude.ID, tokenOpts{pin: &e.orgID, scopes: []string{"admin"}})
	adminTok, _ := e.newToken(chen.ID, tokenOpts{pin: &e.orgID, scopes: []string{"admin"}})

	e.createIssue(e.chen, map[string]any{"title": "x"})
	tests := []struct {
		name   string
		method string
		path   string
		token  string
		body   any
		status int
		code   string
	}{
		{"read can list", "GET", "/api/v1/orgs/orbit/issues", readTok, nil, 200, ""},
		{"read cannot create issue", "POST", "/api/v1/orgs/orbit/issues", readTok, map[string]any{"team": "ORB", "title": "no"}, 403, "insufficient_scope"},
		{"read cannot patch", "PATCH", "/api/v1/orgs/orbit/issues/ORB-1", readTok, map[string]any{"title": "no"}, 403, "insufficient_scope"},
		{"read cannot comment", "POST", "/api/v1/orgs/orbit/issues/ORB-1/comments", readTok, map[string]any{"body": "no"}, 403, "insufficient_scope"},
		{"read cannot add label", "POST", "/api/v1/orgs/orbit/labels", readTok, map[string]any{"name": "no"}, 403, "insufficient_scope"},
		{"write can read (scopes are cumulative)", "GET", "/api/v1/orgs/orbit/labels", writeTok, nil, 200, ""},
		{"write can create", "POST", "/api/v1/orgs/orbit/issues", writeTok, map[string]any{"team": "ORB", "title": "yes"}, 201, ""},
		{"write cannot list tokens", "GET", "/api/v1/orgs/orbit/tokens", writeTok, nil, 403, "insufficient_scope"},
		{"write cannot create team", "POST", "/api/v1/orgs/orbit/teams", writeTok, map[string]any{"key": "NO", "name": "no"}, 403, "insufficient_scope"},
		{"admin scope needs admin role", "GET", "/api/v1/orgs/orbit/tokens", memberAdminTok, nil, 403, "forbidden"},
		{"org admin with admin scope", "GET", "/api/v1/orgs/orbit/tokens", adminTok, nil, 200, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := e.do(tt.method, tt.path, tt.token, tt.body)
			if r.status != tt.status {
				t.Fatalf("status %d, want %d: %s", r.status, tt.status, r.body)
			}
			if tt.code != "" {
				if code := r.errCode(t); code != tt.code {
					t.Fatalf("code %s, want %s", code, tt.code)
				}
			}
		})
	}
}

// AC: a revoked or expired token is rejected.
func TestAuth_RejectsBadTokens(t *testing.T) {
	e := newEnv(t)
	claude := e.user("claude")
	past := time.Now().Add(-time.Minute)
	future := time.Now().Add(time.Hour)
	revoked, _ := e.newToken(claude.ID, tokenOpts{pin: &e.orgID, revoked: true})
	expired, _ := e.newToken(claude.ID, tokenOpts{pin: &e.orgID, expires: &past})
	live, _ := e.newToken(claude.ID, tokenOpts{pin: &e.orgID, expires: &future})
	unknown := "odt_" + strings.Repeat("A", 43)

	tests := []struct {
		name   string
		header string
		status int
		code   string
	}{
		{"missing", "", 401, "unauthenticated"},
		{"wrong scheme", "Basic Zm9vOmJhcg==", 401, "invalid_token"},
		{"malformed", "Bearer odt_short", 401, "invalid_token"},
		{"unknown", "Bearer " + unknown, 401, "invalid_token"},
		{"revoked", "Bearer " + revoked, 401, "token_revoked"},
		{"expired", "Bearer " + expired, 401, "token_expired"},
		{"live with expiry", "Bearer " + live, 200, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hdr []string
			if tt.header != "" {
				hdr = []string{"Authorization", tt.header}
			}
			r := e.do("GET", "/api/v1/orgs/orbit/issues", "", nil, hdr...)
			if r.status != tt.status {
				t.Fatalf("status %d, want %d: %s", r.status, tt.status, r.body)
			}
			if tt.code != "" {
				if code := r.errCode(t); code != tt.code {
					t.Fatalf("code %s, want %s", code, tt.code)
				}
				if r.header.Get("WWW-Authenticate") == "" {
					t.Fatal("401 without WWW-Authenticate")
				}
			}
		})
	}

	// Revoking through the API takes effect on the next request.
	var created tracker.CreatedToken
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/tokens", e.chen, map[string]any{"name": "reviewer", "scopes": []string{"read"}, "user": "claude", "expiresInDays": 30})).json(t, &created)
	if !strings.HasPrefix(created.Secret, "odt_") || len(created.Secret) != 47 || created.Token.Prefix != created.Secret[:12] {
		t.Fatalf("created token: %+v", created.Token)
	}
	e.must(200, e.do("GET", "/api/v1/me", created.Secret, nil))
	e.must(204, e.do("DELETE", "/api/v1/orgs/orbit/tokens/"+created.Token.ID.String(), e.chen, nil))
	if code := e.must(401, e.do("GET", "/api/v1/me", created.Secret, nil)).errCode(t); code != "token_revoked" {
		t.Fatalf("code %s", code)
	}
	var list page[tracker.Token]
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/tokens", e.chen, nil)).json(t, &list)
	if strings.Contains(string(e.do("GET", "/api/v1/orgs/orbit/tokens", e.chen, nil).body), created.Secret) {
		t.Fatal("token list leaks a secret")
	}
	// Admin scope cannot be granted to a non-admin member.
	e.must(400, e.do("POST", "/api/v1/orgs/orbit/tokens", e.chen, map[string]any{"name": "x", "scopes": []string{"admin"}, "user": "claude", "expiresInDays": 30}))
}

// AC: every resource returns 404 across orgs. chen is a member of BOTH orgs
// here and uses an unpinned token, so membership is not what stops him: the
// org in the path is. Body references to another org's rows are 400.
//
// It runs twice: with an unpinned bearer PAT, and with a web session cookie
// (T6a), which is unpinned too and must be stopped the same way.
func TestCrossOrgIsolation(t *testing.T) {
	t.Run("bearer", func(t *testing.T) {
		e := newEnv(t)
		chen := e.user("chen")
		acmeID := e.newOrg("acme", "ACM")
		e.addMember(acmeID, chen.ID, "admin")
		tok, _ := e.newToken(chen.ID, tokenOpts{}) // unpinned, all scopes
		crossOrgIsolation(t, e, tok, tok)
	})
	t.Run("cookie session", func(t *testing.T) {
		e := newEnv(t)
		chen := e.user("chen")
		acmeID := e.newOrg("acme", "ACM")
		e.addMember(acmeID, chen.ID, "admin")
		session, _ := e.loginAs("chen@example.com")
		// The orbit-only token is minted with a PAT (it has no expiry).
		minter, _ := e.newToken(chen.ID, tokenOpts{})
		crossOrgIsolation(t, e, session, minter)
	})
}

// crossOrgIsolation is the body of TestCrossOrgIsolation. tok is chen's
// credential under test (a member of orbit and acme); minter mints the
// orbit-pinned token the "tokens" case needs.
func crossOrgIsolation(t *testing.T, e *env, tok, minter string) {

	// Orbit resources.
	orb := e.createIssue(tok, map[string]any{"title": "orbit secret", "labels": []string{"Bug"}})
	orb2 := e.createIssue(tok, map[string]any{"title": "orbit other"})
	var cm tracker.Comment
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues/ORB-1/comments", tok, map[string]any{"body": "orbit comment"})).json(t, &cm)
	var rel tracker.Relation
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues/ORB-1/relations", tok, map[string]any{"type": "blocks", "issue": "ORB-2"})).json(t, &rel)
	var orbTeam tracker.Team
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/teams/ORB", tok, nil)).json(t, &orbTeam)
	var orbStates page[tracker.WorkflowState]
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/teams/ORB/states", tok, nil)).json(t, &orbStates)
	var orbLabels page[tracker.Label]
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/labels", tok, nil)).json(t, &orbLabels)
	var orbTok tracker.CreatedToken
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/tokens", minter, map[string]any{"name": "orbit-only", "scopes": []string{"read"}})).json(t, &orbTok)

	// An acme issue to aim body references from.
	var acm tracker.Issue
	e.must(201, e.do("POST", "/api/v1/orgs/acme/issues", tok, map[string]any{"team": "ACM", "title": "acme issue"})).json(t, &acm)
	if acm.Identifier != "ACM-1" {
		t.Fatalf("acme numbering starts at %s", acm.Identifier)
	}

	expect := func(t *testing.T, status int, method, path string, body any) {
		t.Helper()
		r := e.do(method, path, tok, body)
		if r.status != status {
			t.Fatalf("%s %s: status %d, want %d: %s", method, path, r.status, status, r.body)
		}
		if status >= 400 {
			r.errCode(t)
		}
	}
	A := "/api/v1/orgs/acme"

	t.Run("issues", func(t *testing.T) {
		expect(t, 404, "GET", A+"/issues/ORB-1", nil)
		expect(t, 404, "GET", A+"/issues/"+orb.ID.String(), nil)
		expect(t, 404, "PATCH", A+"/issues/"+orb.ID.String(), map[string]any{"title": "pwned"})
		var p page[tracker.Issue]
		e.must(200, e.do("GET", A+"/issues?includeArchived=true", tok, nil)).json(t, &p)
		if ids := identifiers(p.Items); !slices.Equal(ids, []string{"ACM-1"}) {
			t.Fatalf("acme list shows %v", ids)
		}
		expect(t, 400, "PATCH", A+"/issues/ACM-1", map[string]any{"parent": orb.ID.String()})
		expect(t, 400, "POST", A+"/issues", map[string]any{"team": orbTeam.ID.String(), "title": "x"})
	})
	t.Run("comments", func(t *testing.T) {
		expect(t, 404, "GET", A+"/issues/"+orb.ID.String()+"/comments", nil)
		expect(t, 404, "POST", A+"/issues/"+orb.ID.String()+"/comments", map[string]any{"body": "x"})
		expect(t, 404, "PATCH", A+"/comments/"+cm.ID.String(), map[string]any{"body": "pwned"})
		expect(t, 404, "DELETE", A+"/comments/"+cm.ID.String(), nil)
		expect(t, 400, "POST", A+"/issues/ACM-1/comments", map[string]any{"body": "x", "parentId": cm.ID})
	})
	t.Run("activity", func(t *testing.T) {
		expect(t, 404, "GET", A+"/issues/"+orb.ID.String()+"/activity", nil)
		expect(t, 404, "GET", A+"/issues/ORB-1/activity", nil)
	})
	t.Run("relations", func(t *testing.T) {
		expect(t, 404, "GET", A+"/issues/"+orb.ID.String()+"/relations", nil)
		expect(t, 404, "DELETE", A+"/issues/"+orb.ID.String()+"/relations/"+rel.ID.String(), nil)
		expect(t, 404, "DELETE", A+"/issues/ACM-1/relations/"+rel.ID.String(), nil)
		expect(t, 400, "POST", A+"/issues/ACM-1/relations", map[string]any{"type": "blocks", "issue": orb2.ID.String()})
	})
	t.Run("labels", func(t *testing.T) {
		var p page[tracker.Label]
		e.must(200, e.do("GET", A+"/labels", tok, nil)).json(t, &p)
		if len(p.Items) != 0 {
			t.Fatalf("acme sees labels %v", p.Items)
		}
		expect(t, 400, "PATCH", A+"/issues/ACM-1", map[string]any{"addLabels": []string{orbLabels.Items[0].ID.String()}})
		expect(t, 400, "POST", A+"/issues", map[string]any{"team": "ACM", "title": "x", "labels": []string{"Bug"}})
	})
	t.Run("teams", func(t *testing.T) {
		expect(t, 404, "GET", A+"/teams/ORB", nil)
		expect(t, 404, "GET", A+"/teams/"+orbTeam.ID.String(), nil)
		var p page[tracker.Team]
		e.must(200, e.do("GET", A+"/teams", tok, nil)).json(t, &p)
		if len(p.Items) != 1 || p.Items[0].Key != "ACM" {
			t.Fatalf("acme teams %v", p.Items)
		}
	})
	t.Run("states", func(t *testing.T) {
		expect(t, 404, "GET", A+"/teams/ORB/states", nil)
		expect(t, 404, "GET", A+"/teams/"+orbTeam.ID.String()+"/states", nil)
		expect(t, 400, "PATCH", A+"/issues/ACM-1", map[string]any{"state": orbStates.Items[0].ID.String()})
	})
	t.Run("tokens", func(t *testing.T) {
		expect(t, 404, "DELETE", A+"/tokens/"+orbTok.Token.ID.String(), nil)
		var p page[tracker.Token]
		e.must(200, e.do("GET", A+"/tokens", tok, nil)).json(t, &p)
		for _, tk := range p.Items {
			if tk.ID == orbTok.Token.ID {
				t.Fatal("acme lists an orbit token")
			}
		}
		// The orbit-pinned token cannot reach acme at all.
		r := e.do("GET", A+"/issues", orbTok.Secret, nil)
		if r.status != 404 {
			t.Fatalf("pinned token reached acme: %d", r.status)
		}
		// And the revoke in acme did not revoke it.
		e.must(200, e.do("GET", "/api/v1/orgs/orbit/issues", orbTok.Secret, nil))
	})
	t.Run("events", func(t *testing.T) {
		var p page[tracker.Event]
		e.must(200, e.do("GET", A+"/events?after=0", tok, nil)).json(t, &p)
		for _, ev := range p.Items {
			if ev.EntityID == orb.ID || ev.EntityID == cm.ID || ev.EntityID == rel.ID {
				t.Fatalf("acme feed has an orbit event: %+v", ev)
			}
		}
		if len(p.Items) != 1 || p.Items[0].EntityID != acm.ID || p.Items[0].Seq != 1 {
			t.Fatalf("acme feed: %+v", p.Items)
		}
	})
	t.Run("users", func(t *testing.T) {
		var p page[tracker.Member]
		e.must(200, e.do("GET", A+"/users", tok, nil)).json(t, &p)
		if len(p.Items) != 1 || p.Items[0].Username != "chen" {
			t.Fatalf("acme members %v (claude is only in orbit)", p.Items)
		}
		r := e.do("GET", A+"/users", e.claude, nil)
		if r.status != 404 {
			t.Fatalf("non-member listed acme users: %d", r.status)
		}
	})
	t.Run("events stream", func(t *testing.T) {
		br, closeFn := openStream(t, e, "acme", tok, "", 10*time.Second)
		defer closeFn()
		// An orbit write first, then an acme one: the first event the acme
		// stream sees must be the acme one.
		e.createIssue(tok, map[string]any{"title": "orbit, invisible to acme"})
		var acm2 tracker.Issue
		e.must(201, e.do("POST", A+"/issues", tok, map[string]any{"team": "ACM", "title": "acme two"})).json(t, &acm2)
		_, _, data, err := nextEvent(br)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(data, acm2.ID.String()) {
			t.Fatalf("acme stream delivered %s", data)
		}
		r := e.do("GET", A+"/events/stream", e.claude, nil)
		if r.status != 404 {
			t.Fatalf("non-member opened acme stream: %d", r.status)
		}
	})
	t.Run("org itself", func(t *testing.T) {
		// claude is not in acme; a non-member and an unknown org look the same.
		for _, path := range []string{A + "/issues", "/api/v1/orgs/nope/issues"} {
			r := e.do("GET", path, e.claude, nil)
			if r.status != 404 || r.errCode(t) != "not_found" {
				t.Fatalf("%s: %d %s", path, r.status, r.body)
			}
		}
		var orgs page[tracker.Org]
		e.must(200, e.do("GET", "/api/v1/orgs", e.chen, nil)).json(t, &orgs) // pinned to orbit
		if len(orgs.Items) != 1 || orgs.Items[0].Slug != "orbit" {
			t.Fatalf("pinned token lists %v", orgs.Items)
		}
		e.must(200, e.do("GET", "/api/v1/orgs", tok, nil)).json(t, &orgs)
		if len(orgs.Items) != 2 {
			t.Fatalf("unpinned token lists %v", orgs.Items)
		}
	})
}

func TestIdempotencyKey(t *testing.T) {
	e := newEnv(t)
	body := map[string]any{"team": "ORB", "title": "once"}
	r1 := e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues", e.chen, body, "Idempotency-Key", "k-1"))
	r2 := e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues", e.chen, body, "Idempotency-Key", "k-1"))
	if string(r1.body) != string(r2.body) || r2.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay differs:\n%s\n%s", r1.body, r2.body)
	}
	var p page[tracker.Issue]
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/issues", e.chen, nil)).json(t, &p)
	if len(p.Items) != 1 {
		t.Fatalf("%d issues after a replayed create", len(p.Items))
	}
	// Same key, different body.
	r3 := e.must(409, e.do("POST", "/api/v1/orgs/orbit/issues", e.chen, map[string]any{"team": "ORB", "title": "twice"}, "Idempotency-Key", "k-1"))
	if code := r3.errCode(t); code != "idempotency_key_reused" {
		t.Fatalf("code %s", code)
	}
	// Keys are per user: claude may use the same key for his own request.
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues", e.claude, map[string]any{"team": "ORB", "title": "claude's"}, "Idempotency-Key", "k-1"))
	// A 4xx is remembered too, so a retry gets the same answer.
	bad := map[string]any{"team": "ORB", "title": ""}
	e.must(400, e.do("POST", "/api/v1/orgs/orbit/issues", e.chen, bad, "Idempotency-Key", "k-bad"))
	if r := e.do("POST", "/api/v1/orgs/orbit/issues", e.chen, bad, "Idempotency-Key", "k-bad"); r.status != 400 || r.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("4xx replay: %d %v", r.status, r.header)
	}
	e.must(400, e.do("POST", "/api/v1/orgs/orbit/issues", e.chen, body, "Idempotency-Key", strings.Repeat("x", 256)))
	// A 403 from a read token is not remembered: the same user's write token
	// may then use the key.
	chenRead, _ := e.newToken(e.user("chen").ID, tokenOpts{pin: &e.orgID, scopes: []string{"read"}})
	other := map[string]any{"team": "ORB", "title": "scoped"}
	e.must(403, e.do("POST", "/api/v1/orgs/orbit/issues", chenRead, other, "Idempotency-Key", "k-scope"))
	if r := e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues", e.chen, other, "Idempotency-Key", "k-scope")); r.header.Get("Idempotent-Replayed") != "" {
		t.Fatal("a 403 was replayed")
	}
	// An expired key is forgotten.
	if _, err := e.pool.Exec(context.Background(), `UPDATE idempotency_keys SET expires_at = now() - interval '1 second' WHERE key = 'k-1'`); err != nil {
		t.Fatal(err)
	}
	r4 := e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues", e.chen, body, "Idempotency-Key", "k-1"))
	if r4.header.Get("Idempotent-Replayed") != "" {
		t.Fatal("expired key was replayed")
	}
}

func TestCommentsAndRelations(t *testing.T) {
	e := newEnv(t)
	e.createIssue(e.chen, map[string]any{"title": "one"})
	e.createIssue(e.chen, map[string]any{"title": "two"})
	e.createIssue(e.chen, map[string]any{"title": "three"})

	// Comments.
	var c1, c2 tracker.Comment
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues/ORB-1/comments", e.chen, map[string]any{"body": "first"})).json(t, &c1)
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues/ORB-1/comments", e.claude, map[string]any{"body": "reply", "parentId": c1.ID})).json(t, &c2)
	if c2.ParentID == nil || *c2.ParentID != c1.ID || c2.Author == nil || c2.Author.Username != "claude" {
		t.Fatalf("reply: %+v", c2)
	}
	e.must(400, e.do("POST", "/api/v1/orgs/orbit/issues/ORB-2/comments", e.chen, map[string]any{"body": "x", "parentId": c1.ID}))
	e.must(400, e.do("POST", "/api/v1/orgs/orbit/issues/ORB-1/comments", e.chen, map[string]any{"body": "  "}))
	e.must(403, e.do("PATCH", "/api/v1/orgs/orbit/comments/"+c1.ID.String(), e.claude, map[string]any{"body": "not mine"}))
	var edited tracker.Comment
	e.must(200, e.do("PATCH", "/api/v1/orgs/orbit/comments/"+c1.ID.String(), e.chen, map[string]any{"body": "first, edited"})).json(t, &edited)
	if edited.EditedAt == nil || edited.Body != "first, edited" {
		t.Fatalf("edit: %+v", edited)
	}
	var cs page[tracker.Comment]
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/issues/ORB-1/comments", e.claude, nil)).json(t, &cs)
	if len(cs.Items) != 2 || cs.Items[0].ID != c1.ID {
		t.Fatalf("comments: %+v", cs.Items)
	}
	e.must(403, e.do("DELETE", "/api/v1/orgs/orbit/comments/"+c1.ID.String(), e.claude, nil))
	e.must(204, e.do("DELETE", "/api/v1/orgs/orbit/comments/"+c2.ID.String(), e.chen, nil)) // admin may delete
	e.must(404, e.do("DELETE", "/api/v1/orgs/orbit/comments/"+c2.ID.String(), e.chen, nil))
	e.must(404, e.do("PATCH", "/api/v1/orgs/orbit/comments/not-a-uuid", e.chen, map[string]any{"body": "x"}))

	// Relations: ORB-1 blocks ORB-2; ORB-3 is blockedBy ORB-2; ORB-1 related ORB-3; ORB-3 duplicateOf ORB-1.
	var r1, r2, r3, r4 tracker.Relation
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues/ORB-1/relations", e.chen, map[string]any{"type": "blocks", "issue": "ORB-2"})).json(t, &r1)
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues/ORB-3/relations", e.chen, map[string]any{"type": "blockedBy", "issue": "ORB-2"})).json(t, &r2)
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues/ORB-1/relations", e.chen, map[string]any{"type": "related", "issue": "ORB-3"})).json(t, &r3)
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/issues/ORB-3/relations", e.chen, map[string]any{"type": "duplicateOf", "issue": "ORB-1"})).json(t, &r4)
	if r2.Type != "blockedBy" || r2.Issue.Identifier != "ORB-2" {
		t.Fatalf("blockedBy create: %+v", r2)
	}
	e.must(409, e.do("POST", "/api/v1/orgs/orbit/issues/ORB-1/relations", e.chen, map[string]any{"type": "blocks", "issue": "ORB-2"}))
	e.must(409, e.do("POST", "/api/v1/orgs/orbit/issues/ORB-2/relations", e.chen, map[string]any{"type": "blocks", "issue": "ORB-1"}))
	e.must(409, e.do("POST", "/api/v1/orgs/orbit/issues/ORB-3/relations", e.chen, map[string]any{"type": "related", "issue": "ORB-1"}))
	e.must(400, e.do("POST", "/api/v1/orgs/orbit/issues/ORB-1/relations", e.chen, map[string]any{"type": "blocks", "issue": "ORB-1"}))
	e.must(400, e.do("POST", "/api/v1/orgs/orbit/issues/ORB-1/relations", e.chen, map[string]any{"type": "parent", "issue": "ORB-2"}))

	var two tracker.Relations
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/issues/ORB-2/relations", e.chen, nil)).json(t, &two)
	if len(two.BlockedBy) != 1 || two.BlockedBy[0].Issue.Identifier != "ORB-1" || len(two.Blocks) != 1 || two.Blocks[0].Issue.Identifier != "ORB-3" {
		t.Fatalf("ORB-2 relations: %+v", two)
	}
	var one tracker.Issue
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/issues/ORB-1?include=relations,comments,attachments,stateHistory", e.chen, nil)).json(t, &one)
	if one.Relations == nil || len(one.Relations.Related) != 1 || len(one.Relations.Duplicates) != 1 || one.Relations.Duplicates[0].Issue.Identifier != "ORB-3" {
		t.Fatalf("ORB-1 include=relations: %+v", one.Relations)
	}
	if len(one.Comments) != 1 || one.Attachments == nil || len(one.StateHistory) != 1 {
		t.Fatalf("includes: comments=%d attachments=%v history=%d", len(one.Comments), one.Attachments, len(one.StateHistory))
	}
	// Without include, the expansion keys are absent.
	plain := e.must(200, e.do("GET", "/api/v1/orgs/orbit/issues/ORB-1", e.chen, nil))
	for _, k := range []string{`"relations"`, `"comments"`, `"attachments"`, `"stateHistory"`} {
		if strings.Contains(string(plain.body), k) {
			t.Fatalf("%s present without include", k)
		}
	}
	e.must(400, e.do("GET", "/api/v1/orgs/orbit/issues/ORB-1?include=everything", e.chen, nil))

	e.must(204, e.do("DELETE", "/api/v1/orgs/orbit/issues/ORB-2/relations/"+r1.ID.String(), e.chen, nil)) // from the other side
	e.must(404, e.do("DELETE", "/api/v1/orgs/orbit/issues/ORB-1/relations/"+r1.ID.String(), e.chen, nil))
	e.must(404, e.do("DELETE", "/api/v1/orgs/orbit/issues/ORB-1/relations/"+r2.ID.String(), e.chen, nil)) // does not touch ORB-1
}

func TestTeamsStatesLabelsUsers(t *testing.T) {
	e := newEnv(t)
	var states page[tracker.WorkflowState]
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/teams/ORB/states", e.claude, nil)).json(t, &states)
	var names []string
	for _, s := range states.Items {
		names = append(names, s.Name+":"+s.Type)
	}
	want := []string{"Triage:triage", "Backlog:backlog", "Todo:unstarted", "In Progress:started", "In Review:started",
		"In Testing:started", "Done:completed", "Canceled:canceled", "Duplicate:canceled"}
	if !slices.Equal(names, want) {
		t.Fatalf("states %v", names)
	}
	var team tracker.Team
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/teams/orb", e.claude, nil)).json(t, &team)
	if team.ReviewStateID == nil || *team.ReviewStateID != states.Items[4].ID || team.TestingStateID == nil || *team.TestingStateID != states.Items[5].ID {
		t.Fatalf("anchor states: %+v", team)
	}

	var made tracker.Team
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/teams", e.chen, map[string]any{"key": "WEB", "name": "Web"})).json(t, &made)
	e.must(409, e.do("POST", "/api/v1/orgs/orbit/teams", e.chen, map[string]any{"key": "WEB", "name": "Web again"}))
	e.must(400, e.do("POST", "/api/v1/orgs/orbit/teams", e.chen, map[string]any{"key": "web-1", "name": "x"}))
	is := e.createIssue(e.chen, map[string]any{"team": "WEB", "title": "first web issue"})
	if is.Identifier != "WEB-1" || is.State.Name != "Backlog" {
		t.Fatalf("WEB issue: %s %s", is.Identifier, is.State.Name)
	}

	var labels page[tracker.Label]
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/labels", e.claude, nil)).json(t, &labels)
	if len(labels.Items) != 3 {
		t.Fatalf("seed labels: %+v", labels.Items)
	}
	e.must(201, e.do("POST", "/api/v1/orgs/orbit/labels", e.claude, map[string]any{"name": "Design", "color": "#123abc"}))
	e.must(409, e.do("POST", "/api/v1/orgs/orbit/labels", e.claude, map[string]any{"name": "design"}))
	e.must(400, e.do("POST", "/api/v1/orgs/orbit/labels", e.claude, map[string]any{"name": "x", "color": "red"}))

	var users page[tracker.Member]
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/users", e.claude, nil)).json(t, &users)
	if len(users.Items) != 2 || users.Items[0].Username != "chen" || users.Items[0].Role != "admin" || !users.Items[1].IsAgent {
		t.Fatalf("users: %+v", users.Items)
	}
	var me tracker.Me
	e.must(200, e.do("GET", "/api/v1/me", e.claude, nil)).json(t, &me)
	if me.Username != "claude" || !me.IsAgent || me.PinnedOrgID == nil || *me.PinnedOrgID != e.orgID {
		t.Fatalf("me: %+v", me)
	}
	var org tracker.Org
	e.must(200, e.do("GET", "/api/v1/orgs/orbit", e.claude, nil)).json(t, &org)
	if org.Slug != "orbit" || org.Role != "member" {
		t.Fatalf("org: %+v", org)
	}
}

func TestErrorEnvelopeAndPublicRoutes(t *testing.T) {
	e := newEnv(t)
	if code := e.must(404, e.do("GET", "/api/v1/orgs/orbit/nothing-here", e.chen, nil)).errCode(t); code != "not_found" {
		t.Fatalf("code %s", code)
	}
	if code := e.must(405, e.do("PUT", "/api/v1/orgs/orbit/issues", e.chen, nil)).errCode(t); code != "method_not_allowed" {
		t.Fatalf("code %s", code)
	}
	r := e.do("POST", "/api/v1/orgs/orbit/issues", e.chen, nil, "Content-Type", "text/plain")
	if r.status != 415 || r.errCode(t) != "unsupported_media_type" {
		t.Fatalf("415: %d %s", r.status, r.body)
	}
	e.must(200, e.do("GET", "/healthz", "", nil))
	spec := e.must(200, e.do("GET", "/api/v1/openapi.yaml", "", nil))
	if !strings.HasPrefix(string(spec.body), "openapi: 3") {
		t.Fatalf("openapi body starts %q", string(spec.body[:20]))
	}
}

// SSE: a new event arrives on the stream, and Last-Event-ID resumes.
func TestEventStream(t *testing.T) {
	e := newEnv(t)
	first := e.createIssue(e.chen, map[string]any{"title": "before the stream"})

	open := func(t *testing.T, lastEventID string) (*bufio.Reader, func()) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/api/v1/orgs/orbit/events/stream", nil)
		req.Header.Set("Authorization", "Bearer "+e.claude)
		if lastEventID != "" {
			req.Header.Set("Last-Event-ID", lastEventID)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/event-stream" || res.Header.Get("X-Accel-Buffering") != "no" {
			cancel()
			t.Fatalf("stream response %d %v", res.StatusCode, res.Header)
		}
		return bufio.NewReader(res.Body), func() { cancel(); _ = res.Body.Close() }
	}
	// next reads lines until a complete event; returns its id and type.
	next := func(t *testing.T, br *bufio.Reader) (id, typ, data string) {
		t.Helper()
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("read stream: %v", err)
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case strings.HasPrefix(line, "id: "):
				id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				typ = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			case line == "" && id != "":
				return id, typ, data
			}
		}
	}

	t.Run("live event", func(t *testing.T) {
		br, closeFn := open(t, "")
		defer closeFn()
		second := e.createIssue(e.chen, map[string]any{"title": "during the stream"})
		id, typ, data := next(t, br)
		if typ != "issue.created" || !strings.Contains(data, second.ID.String()) || id != "2" {
			t.Fatalf("got id=%s type=%s data=%s", id, typ, data)
		}
	})
	t.Run("resume with Last-Event-ID", func(t *testing.T) {
		br, closeFn := open(t, "0")
		defer closeFn()
		id, _, data := next(t, br)
		if id != "1" || !strings.Contains(data, first.ID.String()) {
			t.Fatalf("resume from 0 gave id=%s", id)
		}
		id, _, _ = next(t, br)
		if id != "2" {
			t.Fatalf("second event id=%s", id)
		}
	})
	t.Run("bad Last-Event-ID", func(t *testing.T) {
		r := e.do("GET", "/api/v1/orgs/orbit/events/stream", e.claude, nil, "Last-Event-ID", "abc")
		if r.status != 400 {
			t.Fatalf("status %d", r.status)
		}
	})
	if n := e.broker.Subscribers(); n != 0 {
		// Closed streams must unsubscribe (allow the handler a moment to exit).
		deadline := time.Now().Add(2 * time.Second)
		for e.broker.Subscribers() != 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if n := e.broker.Subscribers(); n != 0 {
			t.Fatalf("%d stream subscriptions leaked", n)
		}
	}
}

// doNoFatal is do() for goroutines, where t.Fatal is not allowed.
func (e *env) doNoFatal(method, path, token string, body any) resp {
	r, err := e.request(method, path, token, body)
	if err != nil {
		return resp{status: -1, body: []byte(err.Error())}
	}
	return r
}

// LAUN-20: every team has a Triage state (type triage), first in its order.
// A new issue still starts in Backlog; Triage is chosen by name. The
// stateType=triage filter finds only that org's issues, and another org's
// Triage state is a different row that this org cannot use.
func TestTriageState(t *testing.T) {
	e := newEnv(t)
	chen := e.user("chen")
	acmeID := e.newOrg("acme", "ACM")
	e.addMember(acmeID, chen.ID, "admin")
	tok, _ := e.newToken(chen.ID, tokenOpts{})

	def := e.createIssue(tok, map[string]any{"title": "plain"})
	if def.State.Name != "Backlog" {
		t.Fatalf("a new issue starts in %q, want Backlog", def.State.Name)
	}
	tri := e.createIssue(tok, map[string]any{"title": "needs a look", "state": "Triage"})
	if tri.State.Name != "Triage" || tri.State.Type != "triage" || tri.StartedAt != nil {
		t.Fatalf("triage issue state %+v", tri.State)
	}
	var acm tracker.Issue
	e.must(201, e.do("POST", "/api/v1/orgs/acme/issues", tok, map[string]any{"team": "ACM", "title": "acme triage", "state": "Triage"})).json(t, &acm)

	ids := func(path string) []string {
		t.Helper()
		var p page[tracker.Issue]
		e.must(200, e.do("GET", path, tok, nil)).json(t, &p)
		return identifiers(p.Items)
	}
	if got := ids("/api/v1/orgs/orbit/issues?stateType=triage"); !slices.Equal(got, []string{tri.Identifier}) {
		t.Fatalf("orbit stateType=triage: %v", got)
	}
	if got := ids("/api/v1/orgs/acme/issues?stateType=triage"); !slices.Equal(got, []string{"ACM-1"}) {
		t.Fatalf("acme stateType=triage: %v", got)
	}
	if got := ids("/api/v1/orgs/orbit/issues?state=triage"); !slices.Equal(got, []string{tri.Identifier}) {
		t.Fatalf("state=triage (any case): %v", got)
	}

	var orbStates, acmStates page[tracker.WorkflowState]
	e.must(200, e.do("GET", "/api/v1/orgs/orbit/teams/ORB/states", tok, nil)).json(t, &orbStates)
	e.must(200, e.do("GET", "/api/v1/orgs/acme/teams/ACM/states", tok, nil)).json(t, &acmStates)
	if len(orbStates.Items) != 9 || orbStates.Items[0].Name != "Triage" || orbStates.Items[0].Type != "triage" {
		t.Fatalf("orbit states %+v", orbStates.Items)
	}
	if acmStates.Items[0].Name != "Triage" || acmStates.Items[0].ID == orbStates.Items[0].ID {
		t.Fatalf("acme states %+v", acmStates.Items)
	}
	// Another org's Triage state is a body reference to another org's row.
	r := e.do("PATCH", "/api/v1/orgs/acme/issues/ACM-1", tok, map[string]any{"state": orbStates.Items[0].ID.String()})
	if r.status != 400 {
		t.Fatalf("PATCH with orbit's state id: %d %s", r.status, r.body)
	}
	// Moving out of Triage works and the issue leaves the filter.
	e.must(200, e.do("PATCH", "/api/v1/orgs/orbit/issues/"+tri.Identifier, tok, map[string]any{"state": "Todo"}))
	if got := ids("/api/v1/orgs/orbit/issues?stateType=triage"); len(got) != 0 {
		t.Fatalf("after the move: %v", got)
	}
}
