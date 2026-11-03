package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// linksFake answers save_issue's writes and the issue's attachments; the
// issue already has a link to MR !1.
func linksFake(t *testing.T, failURL string) *fakeAPI {
	return newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/attachments"):
			writeJSON(w, 200, map[string]any{"items": []any{
				map[string]any{"kind": "link", "url": "https://gitlab.example/mr/1"},
				map[string]any{"kind": "file", "url": nil},
			}, "nextCursor": nil})
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/attachments"):
			switch body["url"] {
			case failURL:
				apiErr(w, 400, "validation_failed", "url must be http or https")
			case "https://gitlab.example/mr/409":
				apiErr(w, 409, "conflict", "already attached")
			default:
				writeJSON(w, 201, map[string]any{"id": "att"})
			}
		case r.Method == http.MethodPost:
			writeJSON(w, 201, map[string]any{"id": issueID, "identifier": "ORB-12"})
		default:
			writeJSON(w, 200, map[string]any{"id": issueID, "identifier": "ORB-12"})
		}
	})
}

func postedLinks(api *fakeAPI) []string {
	var out []string
	for _, r := range api.requests() {
		if r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/attachments") {
			out = append(out, r.Body["url"].(string)+"|"+asString(r.Body["title"]))
		}
	}
	return out
}

func asString(v any) string { s, _ := v.(string); return s }

// The MR-opened move: state and links in one call. Add-only: the link
// already attached is skipped, a URL given twice is posted once.
func TestSaveIssue_Links(t *testing.T) {
	api := linksFake(t, "")
	s := newSession(t, ProfileFull, api)
	s.okJSON("save_issue", map[string]any{"id": "ORB-12", "state": "In Review", "links": []map[string]any{
		{"url": "https://gitlab.example/mr/1", "title": "example-app!1"},
		{"url": "https://gitlab.example/mr/2", "title": "example-app!2"},
		{"url": "https://gitlab.example/mr/2", "title": "again"},
		{"url": "https://gitlab.example/mr/3"},
		{"url": "https://gitlab.example/mr/409", "title": "raced"},
	}}, nil)
	var patch map[string]any
	for _, r := range api.requests() {
		if r.Method == http.MethodPatch {
			patch = r.Body
		}
	}
	if patch["state"] != "In Review" {
		t.Errorf("patch = %v", patch)
	}
	want := []string{"https://gitlab.example/mr/2|example-app!2", "https://gitlab.example/mr/3|", "https://gitlab.example/mr/409|raced"}
	if got := postedLinks(api); !slices.Equal(got, want) {
		t.Errorf("posted = %v, want %v", got, want)
	}
	last := api.requests()[len(api.requests())-1]
	if last.Method != http.MethodGet || q(t, last).Get("include") != "relations,attachments" {
		t.Errorf("last = %+v, want the issue re-read", last)
	}

	// A repeat with only the existing link writes nothing.
	api2 := linksFake(t, "")
	s2 := newSession(t, ProfileFull, api2)
	s2.okJSON("save_issue", map[string]any{"id": "ORB-12", "links": []map[string]any{{"url": "https://gitlab.example/mr/1", "title": "x"}}}, nil)
	for _, r := range api2.requests() {
		if r.Method != http.MethodGet {
			t.Errorf("a repeat wrote: %s %s", r.Method, r.Path)
		}
	}
}

func TestSaveIssue_LinksPartialFailure(t *testing.T) {
	s := newSession(t, ProfileFull, linksFake(t, "ftp://bad"))
	r := s.callTool("save_issue", map[string]any{"title": "x", "links": []map[string]any{{"url": "ftp://bad"}}})
	if !r.IsError || !strings.Contains(r.Text, "issue ORB-12 was saved, but adding links failed") || !strings.Contains(r.Text, "400") {
		t.Errorf("result = %+v", r)
	}
}

func TestAttachmentTools(t *testing.T) {
	api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		switch {
		case r.URL.Path == "/api/v1/orgs/orbit/attachments/prepare":
			writeJSON(w, 200, map[string]any{"uploadKey": "org/o/2026/u/shot.png", "uploadUrl": "https://silo.example/put?sig=x",
				"method": "PUT", "headers": map[string]string{"Content-Type": "image/png"}, "contentType": "image/png",
				"sizeBytes": body["sizeBytes"], "expiresAt": "2026-10-24T20:00:00Z"})
		case strings.HasSuffix(r.URL.Path, "/attachments") && r.Method == http.MethodPost:
			if body["uploadKey"] != "org/o/2026/u/shot.png" {
				apiErr(w, 400, "validation_failed", "uploadKey is not an upload of this org")
				return
			}
			writeJSON(w, 201, map[string]any{"id": "a1", "kind": "file", "title": body["title"]})
		default:
			writeJSON(w, 200, map[string]any{"id": issueID, "identifier": "ORB-7"})
		}
	})
	s := newSession(t, ProfileFull, api)
	var up struct {
		AssetURL      string `json:"assetUrl"`
		Issue         string `json:"issue"`
		UploadURL     string `json:"uploadUrl"`
		UploadRequest struct {
			URL     string            `json:"url"`
			Method  string            `json:"method"`
			Headers map[string]string `json:"headers"`
		} `json:"uploadRequest"`
	}
	s.okJSON("prepare_attachment_upload", map[string]any{"issue": "orb-7", "filename": "shot.png", "contentType": "image/png",
		"size": 216316, "title": "iOS 01"}, &up)
	if up.AssetURL != "org/o/2026/u/shot.png" || up.Issue != "ORB-7" || up.UploadRequest.URL != up.UploadURL ||
		up.UploadRequest.Method != "PUT" || up.UploadRequest.Headers["Content-Type"] != "image/png" {
		t.Errorf("prepare = %+v", up)
	}
	reqs := api.requests()
	if reqs[0].Path != "/api/v1/orgs/orbit/issues/orb-7" || reqs[1].Method != http.MethodPost {
		t.Fatalf("requests = %+v", reqs)
	}
	if want := map[string]any{"filename": "shot.png", "contentType": "image/png", "sizeBytes": float64(216316)}; !jsonEqual(reqs[1].Body, want) {
		t.Errorf("prepare body = %v", reqs[1].Body)
	}
	s.okJSON("create_attachment_from_upload", map[string]any{"issue": "ORB-7", "assetUrl": up.AssetURL, "title": "iOS 01"}, nil)
	last := api.requests()[len(api.requests())-1]
	if last.Path != "/api/v1/orgs/orbit/issues/ORB-7/attachments" ||
		!jsonEqual(last.Body, map[string]any{"kind": "file", "uploadKey": up.AssetURL, "title": "iOS 01"}) {
		t.Errorf("create = %+v", last)
	}
	r := s.callTool("create_attachment_from_upload", map[string]any{"issue": "ORB-7",
		"assetUrl": "https://uploads.linear.app/0ca9/264c/e6bb"})
	if !r.IsError || !strings.Contains(r.Text, "Linear assetUrl") {
		t.Errorf("Linear assetUrl: %+v", r)
	}
	// A wrong issue fails before the prepare.
	api404 := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		apiErr(w, 404, "not_found", "issue not found")
	})
	s404 := newSession(t, ProfileFull, api404)
	if r := s404.callTool("prepare_attachment_upload", map[string]any{"issue": "ORB-1", "filename": "a.png", "contentType": "image/png", "size": 1}); !r.IsError {
		t.Errorf("unknown issue: %+v", r)
	}
	if n := len(api404.requests()); n != 1 {
		t.Errorf("requests after a 404 issue = %d, want 1 (no prepare)", n)
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// Linear's list_comments returns the newest first (measured by the round-3
// reviewers); the API returns the oldest first.
func TestListComments_NewestFirst(t *testing.T) {
	api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		writeJSON(w, 200, map[string]any{"items": []any{
			map[string]any{"id": "c1", "createdAt": "2026-10-01T10:00:00Z", "updatedAt": "2026-10-05T10:00:00Z"},
			map[string]any{"id": "c2", "createdAt": "2026-10-02T10:00:00Z", "updatedAt": "2026-10-02T10:00:00Z"},
			map[string]any{"id": "c3", "createdAt": "2026-10-03T10:00:00Z", "updatedAt": "2026-10-03T10:00:00Z"},
		}, "nextCursor": nil})
	})
	s := newSession(t, ProfileFull, api)
	ids := func(args map[string]any) []string {
		var p Page[map[string]any]
		s.okJSON("list_comments", args, &p)
		var out []string
		for _, c := range p.Items {
			out = append(out, c["id"].(string))
		}
		return out
	}
	if got := ids(map[string]any{"issueId": "ORB-1"}); !slices.Equal(got, []string{"c3", "c2", "c1"}) {
		t.Errorf("default = %v", got)
	}
	if got := ids(map[string]any{"issueId": "ORB-1", "orderBy": "createdAt", "limit": 2}); !slices.Equal(got, []string{"c3", "c2"}) {
		t.Errorf("createdAt limit 2 = %v", got)
	}
	if got := ids(map[string]any{"issueId": "ORB-1", "orderBy": "updatedAt"}); !slices.Equal(got, []string{"c1", "c3", "c2"}) {
		t.Errorf("updatedAt = %v", got)
	}
}

// Linear's list_issues `state` also takes a state type.
func TestListIssues_StateType(t *testing.T) {
	api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		if strings.HasSuffix(r.URL.Path, "/states") {
			writeJSON(w, 200, map[string]any{"items": []any{
				map[string]any{"name": "Backlog"}, map[string]any{"name": "Todo"}, map[string]any{"name": "In Progress"},
				map[string]any{"name": "Done"}, map[string]any{"name": "Canceled"},
			}, "nextCursor": nil})
			return
		}
		writeJSON(w, 200, map[string]any{"items": []any{}, "nextCursor": nil})
	})
	s := newSession(t, ProfileFull, api)
	for _, tt := range []struct {
		args             map[string]any
		state, stateType string
	}{
		{map[string]any{"team": "Orbit", "state": "started"}, "", "started"},
		{map[string]any{"state": "Unstarted, Started"}, "", "unstarted,started"},
		{map[string]any{"team": "Orbit", "state": "backlog"}, "backlog", ""},         // a state is named Backlog
		{map[string]any{"team": "Orbit", "state": "In Progress"}, "In Progress", ""}, // a name
		{map[string]any{"team": "Orbit", "state": "started,Todo"}, "started,Todo", ""},
		{map[string]any{"team": "Orbit", "state": "started", "stateType": "completed"}, "started", "completed"},
	} {
		before := len(api.requests())
		s.okJSON("list_issues", tt.args, nil)
		var list seen
		for _, r := range api.requests()[before:] {
			if r.Path == "/api/v1/orgs/orbit/issues" {
				list = r
			}
		}
		got := q(t, list)
		if got.Get("state") != tt.state || got.Get("stateType") != tt.stateType {
			t.Errorf("%v: state=%q stateType=%q, want %q %q", tt.args, got.Get("state"), got.Get("stateType"), tt.state, tt.stateType)
		}
	}
}

// A cycle UUID this tracker does not have (a Linear id, today) gets a hint.
func TestCycleUUID_Hint(t *testing.T) {
	const linearCycle = "2160b4bf-62fe-44d8-bee6-79f75939771e"
	api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		if strings.Contains(r.URL.Path, "/cycles/") {
			apiErr(w, 404, "not_found", "cycle not found")
			return
		}
		writeJSON(w, 200, map[string]any{"id": issueID, "identifier": "ORB-12", "team": map[string]any{"key": "ORB"}})
	})
	s := newSession(t, ProfileFull, api)
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"save_issue", map[string]any{"id": "ORB-12", "cycle": linearCycle}},
		{"save_issue", map[string]any{"title": "x", "cycle": linearCycle}},
		{"list_issues", map[string]any{"cycle": linearCycle}},
	} {
		r := s.callTool(tc.tool, tc.args)
		if !r.IsError || !strings.Contains(r.Text, "A Linear cycle id is not a cycle id here") {
			t.Errorf("%s %v: %+v", tc.tool, tc.args, r)
		}
	}
	for _, r := range api.requests() {
		if r.Method != http.MethodGet {
			t.Errorf("wrote after a bad cycle: %s %s", r.Method, r.Path)
		}
	}
}

// With every slot busy, a ping is still answered and a queued call can be
// cancelled before it ever reaches the API.
func TestServer_Serve_BusySlots(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	arrived := 0
	api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		mu.Lock()
		arrived++
		mu.Unlock()
		<-release
		writeJSON(w, 200, map[string]any{"id": "x"})
	})
	s := newSession(t, ProfileFull, api)
	inR, inW := io.Pipe()
	var out syncBuf
	done := make(chan error, 1)
	go func() { done <- s.srv.Serve(context.Background(), inR, &out) }()
	send := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = inW.Write(append(b, '\n'))
	}
	for i := range maxInFlight {
		send(call(100+i, "get_issue", map[string]any{"id": "ORB-1"}))
	}
	// Only after every slot is taken: goroutines take slots in no fixed order.
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return arrived == maxInFlight })
	send(call(100+maxInFlight, "get_issue", map[string]any{"id": "ORB-QUEUED"})) // waits for a slot
	send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"})
	waitFor(t, func() bool { return strings.Contains(out.String(), `"id":1,`) })
	send(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": 100 + maxInFlight}})
	time.Sleep(50 * time.Millisecond)
	close(release)
	_ = inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, `"id":108`) {
		t.Errorf("the cancelled queued call was answered:\n%s", got)
	}
	if strings.Count(got, `"isError":false`) != maxInFlight {
		t.Errorf("want %d answered calls:\n%s", maxInFlight, got)
	}
	for _, r := range api.requests() {
		if strings.HasSuffix(r.Path, "/ORB-QUEUED") {
			t.Errorf("the cancelled queued call reached the API")
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
