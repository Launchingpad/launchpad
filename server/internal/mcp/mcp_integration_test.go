package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/api"
	"devops/internal/auth"
	"devops/internal/dbq"
	"devops/internal/events"
	"devops/internal/idempotency"
	"devops/internal/storage/storagetest"
	"devops/internal/testdb"
	"devops/internal/tracker"
)

// liveAPI is the real router on a freshly migrated database with the test fixture.
type liveAPI struct {
	t            *testing.T
	pool         *pgxpool.Pool
	url          string
	chen, claude string // fixture tokens: chen read/write/admin, claude read/write
}

func newLiveAPI(t *testing.T) *liveAPI {
	t.Helper()
	pool, _ := testdb.New(t) // skips without INTEGRATION=1
	fx := testdb.Fixture(t, pool)
	l := &liveAPI{t: t, pool: pool, chen: fx.Chen, claude: fx.Claude}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	q := dbq.New(pool)
	broker := events.NewBroker()
	svc := tracker.New(pool)
	svc.SetStore(storagetest.New(t)) // the real silo; INTEGRATION=1 fails without it
	s := api.NewServer(svc, auth.NewAuthenticator(q, log), broker,
		idempotency.Middleware(q, log, idempotency.Options{}), pool, log)
	srv := httptest.NewServer(s.Routes())
	t.Cleanup(func() { broker.Close(); srv.Close() })
	l.url = srv.URL
	return l
}

// rest calls the API directly, for setup the MCP tools do not cover.
func (l *liveAPI) rest(method, path, token string, body any, out any) {
	l.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, l.url+"/api/v1/orgs/orbit"+path, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		l.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode > 299 {
		l.t.Fatalf("%s %s: %d %s", method, path, resp.StatusCode, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			l.t.Fatal(err)
		}
	}
}

// mint makes a claude token with the given scopes.
func (l *liveAPI) mint(scopes ...string) string {
	var out struct{ Secret string }
	l.rest("POST", "/tokens", l.chen, map[string]any{"name": "mcp-test", "scopes": scopes, "user": "claude", "expiresInDays": 1}, &out)
	if out.Secret == "" {
		l.t.Fatal("no secret")
	}
	return out.Secret
}

func (l *liveAPI) session(token string, profile Profile) *session {
	l.t.Helper()
	return l.sessionWith(token, profile, nil)
}

func (l *liveAPI) sessionWith(token string, profile Profile, cfg func(*ClientConfig)) *session {
	l.t.Helper()
	cc := ClientConfig{BaseURL: l.url, Token: token, Org: "orbit", ReadOnly: profile == ProfileReviewer}
	if cfg != nil {
		cfg(&cc)
	}
	c, err := NewClient(cc)
	if err != nil {
		l.t.Fatal(err)
	}
	tools, err := NewTools(c, "ORB", profile)
	if err != nil {
		l.t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	return &session{t: l.t, tools: tools, logs: logs, srv: NewServer(tools, slog.New(slog.NewTextHandler(logs, nil)), "test")}
}

// counts is the number of rows in every table a write tool can change.
func (l *liveAPI) counts() map[string]int {
	l.t.Helper()
	out := map[string]int{}
	for _, tbl := range []string{"issues", "comments", "documents", "projects", "issue_relations", "issue_labels", "attachments", "events", "products", "product_teams"} {
		var n int
		if err := l.pool.QueryRow(context.Background(), "SELECT count(*) FROM "+tbl).Scan(&n); err != nil {
			l.t.Fatal(err)
		}
		out[tbl] = n
	}
	var v int
	if err := l.pool.QueryRow(context.Background(), "SELECT coalesce(sum(version), 0) FROM issues").Scan(&v); err != nil {
		l.t.Fatal(err)
	}
	out["issue versions"] = v
	return out
}

type liveIssue struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
	State      struct{ Name string }
	Assignee   *struct{ Username string }
	Labels     []struct{ Name string }
	ProjectID  *string `json:"projectId"`
	CycleID    *string `json:"cycleId"`
	Version    int     `json:"version"`
	Relations  *struct {
		Blocks    []struct{ Issue struct{ Identifier string } }
		BlockedBy []struct{ Issue struct{ Identifier string } }
		Related   []struct{ Issue struct{ Identifier string } }
	} `json:"relations"`
}

func labelNames(is liveIssue) string {
	var n []string
	for _, l := range is.Labels {
		n = append(n, l.Name)
	}
	return strings.Join(n, ",")
}

// The full tool set against the real API and database.
func TestIntegration_FullProfile(t *testing.T) {
	l := newLiveAPI(t)
	s := l.session(l.claude, ProfileFull)

	// --- issues
	var a, b, c liveIssue
	// The shapes today's briefs send: the team NAME, a display name, labels
	// as a JSON-encoded string.
	s.okJSON("save_issue", map[string]any{"title": "MCP first", "description": "body", "assignee": "Claude",
		"state": "Todo", "priority": 2, "labels": `["Bug"]`, "team": "Orbit"}, &a)
	s.okJSON("save_issue", map[string]any{"title": "MCP second", "team": "orbit"}, &b)
	s.okJSON("save_issue", map[string]any{"title": "MCP third", "team": "ORB"}, &c)
	if a.Identifier == "" || a.State.Name != "Todo" || a.Assignee == nil || a.Assignee.Username != "claude" || labelNames(a) != "Bug" {
		t.Fatalf("created = %+v", a)
	}

	var got liveIssue
	s.okJSON("get_issue", map[string]any{"id": strings.ToLower(a.Identifier)}, &got)
	if got.ID != a.ID {
		t.Errorf("get_issue = %+v", got)
	}

	// Append-only labels and relations; a repeat changes nothing.
	upd := map[string]any{"id": a.Identifier, "addLabels": []string{"Feature"},
		"blocks": []string{b.Identifier}, "blockedBy": []string{c.Identifier}, "relatedTo": []string{b.ID}}
	var after liveIssue
	s.okJSON("save_issue", upd, &after)
	if labelNames(after) != "Bug,Feature" && labelNames(after) != "Feature,Bug" {
		t.Errorf("labels = %s", labelNames(after))
	}
	if after.Relations == nil || len(after.Relations.Blocks) != 1 || after.Relations.Blocks[0].Issue.Identifier != b.Identifier ||
		len(after.Relations.BlockedBy) != 1 || len(after.Relations.Related) != 1 {
		t.Fatalf("relations = %+v", after.Relations)
	}
	before := l.counts()
	s.okJSON("save_issue", map[string]any{"id": a.Identifier, "blocks": []string{b.Identifier}, "relatedTo": []string{b.Identifier}}, nil)
	// The same relations named another way: an upper-case UUID, a ref twice,
	// and the symmetric related row added from the other issue.
	s.okJSON("save_issue", map[string]any{"id": a.Identifier, "blocks": []string{strings.ToUpper(b.ID), b.Identifier, strings.ToLower(b.Identifier)}}, nil)
	s.okJSON("save_issue", map[string]any{"id": b.Identifier, "relatedTo": []string{a.Identifier}, "blockedBy": []string{a.ID}}, nil)
	if now := l.counts(); fmt.Sprint(now) != fmt.Sprint(before) {
		t.Errorf("a repeated append changed rows: %v -> %v", before, now)
	}
	// A reverse blocks relation is a real conflict.
	if r := s.callTool("save_issue", map[string]any{"id": b.Identifier, "blocks": []string{a.Identifier}}); !r.IsError || !strings.Contains(r.Text, "409") {
		t.Errorf("reverse blocks: %+v", r)
	}
	// removeBlockedBy deletes; removing it again is a no-op.
	s.okJSON("save_issue", map[string]any{"id": a.Identifier, "removeBlockedBy": []string{c.Identifier}}, &after)
	if len(after.Relations.BlockedBy) != 0 {
		t.Errorf("blockedBy after remove = %+v", after.Relations.BlockedBy)
	}
	s.okJSON("save_issue", map[string]any{"id": a.Identifier, "removeBlockedBy": []string{c.Identifier}}, nil)
	s.okJSON("save_issue", map[string]any{"id": a.Identifier, "blockedBy": []string{c.Identifier}}, &after)

	// A description patch, as Linear's save_issue takes it.
	s.okJSON("save_issue", map[string]any{"id": a.Identifier, "patch": []map[string]any{
		{"op": "append", "text": "\n\n## Acceptance criteria\n\n1. one"},
		{"op": "insert_before", "anchor": "## Acceptance criteria", "text": "## Notes\n\n"},
		{"op": "replace", "old_string": "body", "new_string": "Body"},
	}}, nil)
	var full struct {
		Description string `json:"description"`
		Version     int    `json:"version"`
	}
	s.okJSON("get_issue", map[string]any{"id": a.Identifier, "fields": []string{"description", "relations"}}, &full)
	if want := "Body\n\n## Notes\n\n## Acceptance criteria\n\n1. one"; full.Description != want {
		t.Errorf("patched description = %q, want %q", full.Description, want)
	}
	after.Version = full.Version

	// Compare-and-swap and clearing.
	r := s.callTool("save_issue", map[string]any{"id": a.Identifier, "title": "stale", "version": 1})
	if !r.IsError || !strings.Contains(r.Text, "409 version_conflict") {
		t.Errorf("stale version: %+v", r)
	}
	s.okJSON("save_issue", map[string]any{"id": a.Identifier, "assignee": nil, "version": after.Version}, &got)
	if got.Assignee != nil {
		t.Errorf("assignee not cleared")
	}

	// The MR-opened move as the briefs send it: state and the MR links in one
	// call. Add-only: a repeat writes nothing.
	mrLinks := []map[string]any{
		{"url": "https://gitlab.example/example-org/example-app/-/merge_requests/1", "title": "example-app!1"},
		{"url": "https://gitlab.example/example-org/example-kmp/-/merge_requests/2", "title": "example-kmp!2"},
		{"url": "https://gitlab.example/example-org/example-app/-/merge_requests/1", "title": "twice"},
	}
	var moved struct {
		State       struct{ Name string }
		Attachments []struct{ Kind, Title string }
	}
	s.okJSON("save_issue", map[string]any{"id": c.Identifier, "state": "In Review", "links": mrLinks}, &moved)
	if moved.State.Name != "In Review" || len(moved.Attachments) != 2 || moved.Attachments[0].Title != "example-app!1" {
		t.Fatalf("state+links = %+v", moved)
	}
	beforeLinks := l.counts()
	s.okJSON("save_issue", map[string]any{"id": c.Identifier, "state": "In Review", "links": mrLinks}, nil)
	if now := l.counts(); fmt.Sprint(now) != fmt.Sprint(beforeLinks) {
		t.Errorf("a repeated links call changed rows: %v -> %v", beforeLinks, now)
	}

	// A file attachment: prepare, the agent PUTs, create (Linear's flow).
	png := []byte("\x89PNG\r\n\x1a\nnot really a png, but the store checks only type and size")
	var up struct {
		AssetURL      string `json:"assetUrl"`
		UploadRequest struct {
			URL     string            `json:"url"`
			Method  string            `json:"method"`
			Headers map[string]string `json:"headers"`
		} `json:"uploadRequest"`
	}
	s.okJSON("prepare_attachment_upload", map[string]any{"issue": c.Identifier, "filename": "ios-01.png",
		"contentType": "image/png", "size": len(png), "title": "iOS 01"}, &up)
	req, _ := http.NewRequest(up.UploadRequest.Method, up.UploadRequest.URL, bytes.NewReader(png))
	for k, v := range up.UploadRequest.Headers {
		req.Header.Set(k, v)
	}
	put, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = put.Body.Close()
	if put.StatusCode != 200 {
		t.Fatalf("PUT to the store: %d", put.StatusCode)
	}
	var att map[string]any
	s.okJSON("create_attachment_from_upload", map[string]any{"issue": c.Identifier, "assetUrl": up.AssetURL, "title": "iOS 01"}, &att)
	if att["kind"] != "file" || att["title"] != "iOS 01" {
		t.Errorf("attachment = %v", att)
	}
	s.okJSON("get_issue", map[string]any{"id": c.Identifier}, &moved)
	if len(moved.Attachments) != 3 {
		t.Errorf("attachments = %+v", moved.Attachments)
	}
	// LAUN-106: a second create of one upload on the same issue is the same row.
	var again map[string]any
	s.okJSON("create_attachment_from_upload", map[string]any{"issue": c.Identifier, "assetUrl": up.AssetURL}, &again)
	if again["id"] != att["id"] {
		t.Errorf("second create of one upload: %v, want id %v", again, att["id"])
	}
	s.okJSON("get_issue", map[string]any{"id": c.Identifier}, &moved)
	if len(moved.Attachments) != 3 {
		t.Errorf("attachments after the replay = %+v", moved.Attachments)
	}

	// Linear's list_issues `state` takes a state type: In Review is `started`.
	var started Page[liveIssue]
	s.okJSON("list_issues", map[string]any{"team": "Orbit", "state": "started"}, &started)
	if len(started.Items) != 1 || started.Items[0].ID != c.ID {
		t.Errorf("state=started = %+v", started.Items)
	}

	// Lists and their pages.
	var p1, p2 Page[liveIssue]
	s.okJSON("list_issues", map[string]any{"team": "ORB", "query": "MCP", "limit": 2, "orderBy": "createdAt"}, &p1)
	if len(p1.Items) != 2 || p1.NextCursor == nil {
		t.Fatalf("page 1 = %d items, cursor %v", len(p1.Items), p1.NextCursor)
	}
	s.okJSON("list_issues", map[string]any{"team": "ORB", "query": "MCP", "limit": 2, "orderBy": "createdAt", "cursor": *p1.NextCursor}, &p2)
	if len(p2.Items) != 1 || p2.NextCursor != nil {
		t.Fatalf("page 2 = %d items, cursor %v", len(p2.Items), p2.NextCursor)
	}
	// Duration filters, as the briefs send them.
	var recent, created, future Page[liveIssue]
	s.okJSON("list_issues", map[string]any{"team": "Orbit", "updatedAt": "-P1D", "limit": 250}, &recent)
	s.okJSON("list_issues", map[string]any{"createdAt": "-PT1H"}, &created)
	s.okJSON("list_issues", map[string]any{"createdAt": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)}, &future)
	if len(recent.Items) != 3 || len(created.Items) != 3 || len(future.Items) != 0 {
		t.Errorf("duration filters: -P1D %d, -PT1H %d, tomorrow %d; want 3, 3, 0", len(recent.Items), len(created.Items), len(future.Items))
	}
	if r := s.callTool("list_issues", map[string]any{"updatedAt": "yesterday"}); !r.IsError || !strings.Contains(r.Text, "400") {
		t.Errorf("bad duration: %+v", r)
	}
	var mine Page[liveIssue]
	s.okJSON("list_issues", map[string]any{"label": "Feature", "fields": []string{"title"}}, &mine)
	if len(mine.Items) != 1 || mine.Items[0].ID != a.ID {
		t.Errorf("label filter = %+v", mine.Items)
	}
	if r := s.callTool("get_issue", map[string]any{"id": "ORB-999999"}); !r.IsError || !strings.Contains(r.Text, "404 not_found") {
		t.Errorf("missing issue: %+v", r)
	}

	// --- catalogue reads
	var states, teams, labels, users Page[map[string]any]
	s.okJSON("list_issue_statuses", map[string]any{"team": "Orbit"}, &states)
	s.okJSON("list_teams", map[string]any{"query": "orb"}, &teams)
	s.okJSON("list_issue_labels", map[string]any{}, &labels)
	s.okJSON("list_users", map[string]any{"query": "claude"}, &users)
	if len(states.Items) != 9 || len(teams.Items) != 1 || len(labels.Items) != 3 || len(users.Items) != 1 {
		t.Errorf("states %d, teams %d, labels %d, users %d", len(states.Items), len(teams.Items), len(labels.Items), len(users.Items))
	}

	// --- comments
	var cm map[string]any
	s.okJSON("save_comment", map[string]any{"issueId": a.Identifier, "body": "first"}, &cm)
	s.okJSON("save_comment", map[string]any{"issueId": a.Identifier, "body": "reply", "parentId": cm["id"]}, nil)
	s.okJSON("save_comment", map[string]any{"id": cm["id"], "body": "first, edited"}, nil)
	var cms Page[map[string]any]
	s.okJSON("list_comments", map[string]any{"issueId": a.Identifier}, &cms)
	// Newest first, as Linear: the reply, then the first comment.
	if len(cms.Items) != 2 || cms.Items[0]["body"] != "reply" || cms.Items[1]["body"] != "first, edited" {
		t.Errorf("comments = %v", cms.Items)
	}
	var latest Page[map[string]any]
	s.okJSON("list_comments", map[string]any{"issueId": a.Identifier, "orderBy": "createdAt", "limit": 1}, &latest)
	if len(latest.Items) != 1 || latest.Items[0]["body"] != "reply" || latest.NextCursor == nil {
		t.Errorf("latest comment = %+v", latest)
	}

	// --- projects, milestones, documents
	var pr map[string]any
	s.okJSON("save_project", map[string]any{"name": "MCP Launch", "state": "started", "lead": "me", "teams": []string{"Orbit"}}, &pr)
	pid, _ := pr["id"].(string)
	var gp map[string]any
	s.okJSON("get_project", map[string]any{"query": "mcp launch"}, &gp)
	if gp["id"] != pid {
		t.Errorf("get_project by name = %v", gp["id"])
	}
	var prs Page[map[string]any]
	s.okJSON("list_projects", map[string]any{"state": "started", "team": "Orbit"}, &prs)
	if len(prs.Items) != 1 {
		t.Errorf("projects = %d", len(prs.Items))
	}
	// LAUN-195: the project carries its status; `state` takes a custom status name as well as a kind.
	if st, _ := pr["status"].(map[string]any); st == nil || st["name"] != "In progress" || pr["state"] != "started" {
		t.Errorf("project status = %v state %v", pr["status"], pr["state"])
	}
	if _, err := l.pool.Exec(context.Background(), `INSERT INTO project_statuses (org_id, kind, name, color, position)
		SELECT org_id, 'paused', 'On hold', '#112233', 9 FROM projects WHERE id = $1`, pid); err != nil {
		t.Fatal(err)
	}
	var held map[string]any
	s.okJSON("save_project", map[string]any{"id": pid, "state": "on hold"}, &held)
	if st, _ := held["status"].(map[string]any); st == nil || st["name"] != "On hold" || held["state"] != "paused" {
		t.Errorf("custom status = %v state %v", held["status"], held["state"])
	}
	s.okJSON("save_project", map[string]any{"id": pid, "state": "started"}, &held)
	// LAUN-232: save_project sets the emoji, get_project returns it, null clears it.
	var em map[string]any
	s.okJSON("save_project", map[string]any{"id": pid, "emoji": "🧑🏽‍💻"}, &em)
	s.okJSON("get_project", map[string]any{"query": pid}, &gp)
	if em["emoji"] != "🧑🏽‍💻" || gp["emoji"] != "🧑🏽‍💻" {
		t.Errorf("emoji = %v / %v", em["emoji"], gp["emoji"])
	}
	s.okJSON("save_project", map[string]any{"id": pid, "emoji": nil}, &em)
	if v, ok := em["emoji"]; !ok || v != nil {
		t.Errorf("cleared emoji = %v (present %v)", v, ok)
	}
	var onHold Page[map[string]any]
	s.okJSON("list_projects", map[string]any{"state": "On hold"}, &onHold)
	if len(onHold.Items) != 0 {
		t.Errorf("projects on hold = %d, want 0 after moving back", len(onHold.Items))
	}
	var ms map[string]any
	l.rest("POST", "/milestones", l.claude, map[string]any{"projectId": pid, "name": "Beta"}, &ms)
	var mss Page[map[string]any]
	s.okJSON("list_milestones", map[string]any{"project": "MCP Launch"}, &mss)
	if len(mss.Items) != 1 {
		t.Errorf("milestones = %d", len(mss.Items))
	}
	s.okJSON("save_issue", map[string]any{"id": b.Identifier, "project": "MCP Launch", "milestone": "beta"}, &got)
	if got.ProjectID == nil || *got.ProjectID != pid {
		t.Errorf("issue project = %v", got.ProjectID)
	}

	var doc map[string]any
	s.okJSON("save_document", map[string]any{"title": "MCP spec", "content": "# spec", "project": "MCP Launch"}, &doc)
	slug, _ := doc["slug"].(string)
	var gd map[string]any
	s.okJSON("get_document", map[string]any{"id": slug}, &gd)
	if gd["id"] != doc["id"] {
		t.Errorf("get_document by slug = %v", gd)
	}
	s.okJSON("save_document", map[string]any{"id": slug, "content": "# spec v2"}, nil)
	var docs Page[map[string]any]
	s.okJSON("list_documents", map[string]any{"project": pid}, &docs)
	if len(docs.Items) != 1 || docs.Items[0]["content"] != "# spec v2" {
		t.Errorf("documents = %v", docs.Items)
	}
	s.okJSON("save_comment", map[string]any{"documentId": slug, "body": "doc note"}, nil)
	s.okJSON("list_comments", map[string]any{"documentId": slug}, &cms)
	if len(cms.Items) != 1 {
		t.Errorf("document comments = %d", len(cms.Items))
	}

	// --- cycles (an admin turns them on through REST; there is no MCP tool)
	var none Page[map[string]any]
	s.okJSON("list_cycles", map[string]any{}, &none)
	if len(none.Items) != 0 {
		t.Errorf("cycles before enabling = %d", len(none.Items))
	}
	l.rest("PATCH", "/teams/ORB", l.chen, map[string]any{"cyclesEnabled": true}, nil)
	var cur, next, all Page[map[string]any]
	s.okJSON("list_cycles", map[string]any{"teamId": "Orbit", "type": "current"}, &cur)
	s.okJSON("list_cycles", map[string]any{"type": "next"}, &next)
	s.okJSON("list_cycles", map[string]any{}, &all)
	if len(cur.Items) != 1 || len(next.Items) != 1 || len(all.Items) != 2 || cur.Items[0]["id"] == next.Items[0]["id"] {
		t.Fatalf("current %v next %v all %d", cur.Items, next.Items, len(all.Items))
	}
	s.okJSON("save_issue", map[string]any{"id": c.Identifier, "cycle": "active"}, &got)
	if got.CycleID == nil || *got.CycleID != cur.Items[0]["id"] {
		t.Errorf("cycle = %v, want %v", got.CycleID, cur.Items[0]["id"])
	}
	var inCycle Page[liveIssue]
	s.okJSON("list_issues", map[string]any{"cycle": cur.Items[0]["id"]}, &inCycle)
	if len(inCycle.Items) != 1 || inCycle.Items[0].ID != c.ID {
		t.Errorf("cycle filter by id = %+v", inCycle.Items)
	}

	if strings.Contains(s.logs.String(), l.claude) {
		t.Error("the token is in the log")
	}
}

// The ticket's measurement against the real database: the reviewer profile,
// even holding a WRITE-scoped token, changes nothing, because no write tool
// exists in it. Its three readers work with a read-scoped token.
func TestIntegration_ReviewerProfile(t *testing.T) {
	l := newLiveAPI(t)
	full := l.session(l.claude, ProfileFull)
	var is liveIssue
	full.okJSON("save_issue", map[string]any{"title": "for review"}, &is)
	full.okJSON("save_comment", map[string]any{"issueId": is.Identifier, "body": "context"}, nil)
	var doc map[string]any
	full.okJSON("save_document", map[string]any{"title": "Review notes", "content": "x"}, &doc)

	rev := l.session(l.claude, ProfileReviewer) // write-scoped token on purpose
	before := l.counts()
	attempts := map[string]map[string]any{
		"save_issue":                    {"id": is.Identifier, "title": "changed by a reviewer", "addLabels": []string{"Bug"}},
		"save_comment":                  {"issueId": is.Identifier, "body": "reviewer comment"},
		"save_document":                 {"id": doc["id"], "content": "overwritten"},
		"save_project":                  {"name": "reviewer project"},
		"prepare_attachment_upload":     {"issue": is.Identifier, "filename": "a.png", "contentType": "image/png", "size": 4},
		"create_attachment_from_upload": {"issue": is.Identifier, "assetUrl": "org/x/2026/y/a.png"},
	}
	for name, args := range attempts {
		_, resp := rev.exchange(call(1, name, args))
		if e := resp["1"].Error; e == nil || !strings.Contains(e.Message, "unknown tool") {
			t.Errorf("reviewer %s: %+v", name, resp["1"])
		}
	}
	if after := l.counts(); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("the reviewer changed the database: %v -> %v", before, after)
	}
	// Same attempts in the full profile do change it (the check is not vacuous).
	for name, args := range attempts {
		if strings.Contains(name, "attachment") {
			continue // no upload behind them; the full run is TestIntegration_FullProfile
		}
		full.okJSON(name, args, nil)
	}
	if after := l.counts(); fmt.Sprint(after) == fmt.Sprint(before) {
		t.Fatal("the full profile's writes changed nothing; the reviewer check proves nothing")
	}

	// A read-scoped reviewer token: the three readers work.
	ro := l.session(l.mint("read"), ProfileReviewer)
	var got liveIssue
	ro.okJSON("get_issue", map[string]any{"id": is.Identifier, "includeComments": true}, &got)
	if got.ID != is.ID {
		t.Errorf("get_issue = %+v", got)
	}
	var cms Page[map[string]any]
	ro.okJSON("list_comments", map[string]any{"issueId": is.Identifier}, &cms)
	if len(cms.Items) != 2 {
		t.Errorf("comments = %d", len(cms.Items))
	}
	ro.okJSON("get_document", map[string]any{"id": doc["id"]}, nil)

	// AC4: a read token cannot write even OUTSIDE the reviewer profile: the
	// server's scope check refuses every write tool, and no row changes.
	rofull := l.session(l.mint("read"), ProfileFull)
	beforeRO := l.counts()
	roWrites := map[string]map[string]any{
		"save_issue (create)":           {"title": "read token", "team": "Orbit"},
		"save_issue (update)":           {"id": is.Identifier, "title": "read token"},
		"save_issue (relation)":         {"id": is.Identifier, "relatedTo": []string{"ORB-1"}},
		"save_comment":                  {"issueId": is.Identifier, "body": "x"},
		"save_comment (edit)":           {"id": "0190f5f0-0000-7000-8000-000000000001", "body": "x"},
		"save_document (create)":        {"title": "read token"},
		"save_document (update)":        {"id": doc["id"], "content": "read token"},
		"save_project (create)":         {"name": "read token"},
		"save_project_update":           {"project": "0190f5f0-0000-7000-8000-000000000001", "health": "on_track"},
		"save_issue (links)":            {"id": is.Identifier, "state": "In Review", "links": []map[string]any{{"url": "https://gitlab.example/mr/9"}}},
		"prepare_attachment_upload":     {"issue": is.Identifier, "filename": "a.png", "contentType": "image/png", "size": 4},
		"create_attachment_from_upload": {"issue": is.Identifier, "assetUrl": "org/x/2026/y/a.png"},
	}
	for name, args := range roWrites {
		tool, _, _ := strings.Cut(name, " ")
		r := rofull.callTool(tool, args)
		if !r.IsError || !strings.Contains(r.Text, "403 insufficient_scope") {
			t.Errorf("read token %s: %+v", name, r)
		}
	}
	for _, tl := range catalogue() {
		if tl.ReadOnly || tl.Name == "download_attachment" { // the latter only reads; TestIntegration_DownloadAttachment
			continue
		}
		found := false
		for name := range roWrites {
			if strings.HasPrefix(name, tl.Name) {
				found = true
			}
		}
		if !found {
			t.Errorf("write tool %s is not in the read-token check", tl.Name)
		}
	}
	if after := l.counts(); fmt.Sprint(after) != fmt.Sprint(beforeRO) {
		t.Errorf("a read token changed the database: %v -> %v", beforeRO, after)
	}
}

// The binary's start-up check against the real API.
func TestIntegration_CheckCredential(t *testing.T) {
	l := newLiveAPI(t)
	s := l.session(l.claude, ProfileReviewer)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	CheckCredential(ctx, s.tools.c, ProfileReviewer, s.srv.log)
	if !strings.Contains(s.logs.String(), "user=claude") || !strings.Contains(s.logs.String(), "can write") {
		t.Errorf("log:\n%s", s.logs.String())
	}
	bad := l.session("odt_"+strings.Repeat("x", 43), ProfileFull)
	CheckCredential(ctx, bad.tools.c, ProfileFull, bad.srv.log)
	if !strings.Contains(bad.logs.String(), "401") {
		t.Errorf("log:\n%s", bad.logs.String())
	}
}

// list_products, productSlugs on list_teams and the product filter on
// list_issues (LAUN-63): reads only, no admin token, with a read token and
// a read,write agent token.
func TestIntegration_Products(t *testing.T) {
	l := newLiveAPI(t)
	var prod, arch map[string]any
	l.rest("POST", "/products", l.chen, map[string]any{"name": "Launchpad", "slug": "launchpad", "teams": []string{"ORB"}}, &prod)
	l.rest("POST", "/products", l.chen, map[string]any{"name": "Old thing", "slug": "old-thing"}, &arch)
	l.rest("PATCH", "/products/old-thing", l.chen, map[string]any{"archived": true}, nil)

	agent := l.session(l.claude, ProfileFull)
	var is liveIssue
	agent.okJSON("save_issue", map[string]any{"title": "in a product", "team": "Orbit"}, &is)
	before := l.counts()

	for name, s := range map[string]*session{
		"read,write agent": agent,
		"read token":       l.session(l.mint("read"), ProfileFull),
	} {
		var ps Page[map[string]any]
		s.okJSON("list_products", map[string]any{}, &ps)
		if len(ps.Items) != 1 || ps.Items[0]["slug"] != "launchpad" || ps.NextCursor != nil {
			t.Errorf("%s: list_products = %+v", name, ps)
		}
		if keys, _ := ps.Items[0]["teamKeys"].([]any); len(keys) != 1 || keys[0] != "ORB" {
			t.Errorf("%s: teamKeys = %v", name, ps.Items[0]["teamKeys"])
		}
		var all Page[map[string]any]
		s.okJSON("list_products", map[string]any{"includeArchived": true}, &all)
		if len(all.Items) != 2 {
			t.Errorf("%s: includeArchived = %d items", name, len(all.Items))
		}
		var q Page[map[string]any]
		s.okJSON("list_products", map[string]any{"query": "LAUNCH"}, &q)
		if len(q.Items) != 1 {
			t.Errorf("%s: query = %d items", name, len(q.Items))
		}
		var teams Page[map[string]any]
		s.okJSON("list_teams", map[string]any{}, &teams)
		if len(teams.Items) != 1 {
			t.Fatalf("%s: teams = %d", name, len(teams.Items))
		}
		if sl, _ := teams.Items[0]["productSlugs"].([]any); len(sl) != 1 || sl[0] != "launchpad" {
			t.Errorf("%s: productSlugs = %v", name, teams.Items[0]["productSlugs"])
		}
		var byProduct, none Page[liveIssue]
		s.okJSON("list_issues", map[string]any{"product": "launchpad"}, &byProduct)
		if len(byProduct.Items) != 1 || byProduct.Items[0].ID != is.ID {
			t.Errorf("%s: product filter = %+v", name, byProduct.Items)
		}
		s.okJSON("list_issues", map[string]any{"product": "old-thing"}, &none)
		if len(none.Items) != 0 {
			t.Errorf("%s: product with no teams = %+v", name, none.Items)
		}
	}
	if after := l.counts(); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("the product reads changed the database: %v -> %v", before, after)
	}
}

// download_attachment against the real router and silo (LAUN-178): a PNG goes
// up through prepare / PUT / create and comes back through the tool.
func TestIntegration_DownloadAttachment(t *testing.T) {
	l := newLiveAPI(t)
	full := l.session(l.claude, ProfileFull)
	var is, other liveIssue
	full.okJSON("save_issue", map[string]any{"title": "has a screenshot"}, &is)
	full.okJSON("save_issue", map[string]any{"title": "another issue"}, &other)

	png := []byte("\x89PNG\r\n\x1a\nscreenshot bytes")
	var up struct {
		AssetURL      string `json:"assetUrl"`
		UploadRequest struct {
			URL     string            `json:"url"`
			Method  string            `json:"method"`
			Headers map[string]string `json:"headers"`
		} `json:"uploadRequest"`
	}
	full.okJSON("prepare_attachment_upload", map[string]any{"issue": is.Identifier, "filename": "..evil name.png",
		"contentType": "image/png", "size": len(png)}, &up)
	req, _ := http.NewRequest(up.UploadRequest.Method, up.UploadRequest.URL, bytes.NewReader(png))
	for k, v := range up.UploadRequest.Headers {
		req.Header.Set(k, v)
	}
	put, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = put.Body.Close()
	if put.StatusCode != 200 {
		t.Fatalf("PUT = %d", put.StatusCode)
	}
	var att struct {
		ID string `json:"id"`
	}
	full.okJSON("create_attachment_from_upload", map[string]any{"issue": is.Identifier, "assetUrl": up.AssetURL}, &att)
	full.okJSON("save_issue", map[string]any{"id": is.Identifier, "links": []map[string]any{{"url": "https://gitlab.example/mr/5"}}}, nil)
	var withAtt struct {
		Attachments []struct {
			ID, Kind string
		}
	}
	full.okJSON("get_issue", map[string]any{"id": is.Identifier}, &withAtt)
	linkID := ""
	for _, a := range withAtt.Attachments {
		if a.Kind == "link" {
			linkID = a.ID
		}
	}
	if linkID == "" {
		t.Fatalf("no link attachment in %+v", withAtt)
	}

	// The presigned URL's host is the store's public endpoint.
	su, err := url.Parse(up.UploadRequest.URL)
	if err != nil {
		t.Fatal(err)
	}
	filesURL := su.Scheme + "://" + su.Host

	// A read-scoped token in the full profile downloads the bytes.
	dir := filepath.Join(t.TempDir(), "dl")
	ro := l.sessionWith(l.mint("read"), ProfileFull, func(c *ClientConfig) {
		c.DownloadDir, c.FilesURL, c.AllowInsecureHTTP = dir, filesURL, true
	})
	before := l.counts()
	var out struct {
		Path, ContentType, AttachmentID string
		SizeBytes                       int64
	}
	ro.okJSON("download_attachment", map[string]any{"issue": is.Identifier, "attachment": att.ID}, &out)
	if out.Path != filepath.Join(dir, att.ID+".png") || out.ContentType != "image/png" || out.SizeBytes != int64(len(png)) {
		t.Fatalf("result = %+v", out)
	}
	if got, err := os.ReadFile(out.Path); err != nil || !bytes.Equal(got, png) {
		t.Fatalf("saved bytes = %q, %v", got, err)
	}
	if fi, _ := os.Stat(out.Path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	if after := l.counts(); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("a download changed the database: %v -> %v", before, after)
	}

	// The URL form from get_issue works.
	ro.okJSON("download_attachment", map[string]any{"issue": is.Identifier,
		"attachment": l.url + "/api/v1/orgs/orbit/attachments/" + att.ID + "/download"}, nil)

	// Without the files host configured the presigned host is refused.
	noFiles := l.sessionWith(l.claude, ProfileFull, func(c *ClientConfig) { c.DownloadDir, c.AllowInsecureHTTP = dir, true })
	if r := noFiles.callTool("download_attachment", map[string]any{"issue": is.Identifier, "attachment": att.ID}); !r.IsError ||
		!strings.Contains(r.Text, "host_not_allowed") {
		t.Errorf("unconfigured files host: %+v", r)
	}

	// A link, another issue's attachment and an unknown id.
	if r := ro.callTool("download_attachment", map[string]any{"issue": is.Identifier, "attachment": linkID}); !r.IsError ||
		!strings.Contains(r.Text, "not_a_file") {
		t.Errorf("link: %+v", r)
	}
	if r := ro.callTool("download_attachment", map[string]any{"issue": other.Identifier, "attachment": att.ID}); !r.IsError ||
		!strings.Contains(r.Text, "404") {
		t.Errorf("other issue: %+v", r)
	}
	if r := ro.callTool("download_attachment", map[string]any{"issue": is.Identifier,
		"attachment": "0190f5f0-0000-7000-8000-0000000000ff"}); !r.IsError || !strings.Contains(r.Text, "404") {
		t.Errorf("unknown id: %+v", r)
	}

	// The reviewer profile has no such tool (cross-org is 404 on the REST
	// route: TestAttachments_CrossOrg).
	rev := l.session(l.mint("read"), ProfileReviewer)
	_, resp := rev.exchange(call(1, "download_attachment", map[string]any{"issue": is.Identifier, "attachment": att.ID}))
	if e := resp["1"].Error; e == nil || !strings.Contains(e.Message, "unknown tool") {
		t.Errorf("reviewer: %+v", resp["1"])
	}
	for _, s := range []string{ro.logs.String(), full.logs.String()} {
		if strings.Contains(s, "X-Amz-Signature") {
			t.Error("a presigned signature is in the logs")
		}
	}
}
