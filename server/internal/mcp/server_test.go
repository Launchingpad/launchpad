package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// testToken is a well-formed token the fake API accepts. Tests assert it
// never reaches stdout or the log.
const testToken = "odt_TESTSECRETtestsecret0123456789ABCDEFGHIJK"

// seen is one request the fake API received.
type seen struct {
	Method, Path, Query, Auth string
	Body                      map[string]any
}

// fakeAPI is an httptest server that records every request and answers
// with route(r).
type fakeAPI struct {
	t     *testing.T
	srv   *httptest.Server
	mu    sync.Mutex
	reqs  []seen
	dirN  int // directory (GET /teams, GET /users) requests answered by the fake itself
	route func(w http.ResponseWriter, r *http.Request, body map[string]any)
	// ownDirectory hands GET /teams and GET /users to route too. Otherwise
	// the fake answers them from fakeTeams / fakeUsers and does not record
	// them in reqs (every team argument resolves through GET /teams).
	ownDirectory bool
	teams        []any
}

// The default directory of the fake org.
var (
	fakeTeams = []any{
		map[string]any{"id": "0190f5f0-0000-7000-8000-0000000000e1", "key": "ORB", "name": "Orbit"},
		map[string]any{"id": "0190f5f0-0000-7000-8000-0000000000e2", "key": "WEB", "name": "Web"},
	}
	fakeUsers = []any{
		map[string]any{"username": "chen", "name": "Chen", "email": "chen@example.com"},
		map[string]any{"username": "claude", "name": "Claude", "email": nil},
	}
)

func newFakeAPI(t *testing.T, route func(w http.ResponseWriter, r *http.Request, body map[string]any)) *fakeAPI {
	return newFakeAPIWith(t, false, route)
}

// newFakeAPIOwn is a fake whose route also answers GET /teams and /users.
func newFakeAPIOwn(t *testing.T, route func(w http.ResponseWriter, r *http.Request, body map[string]any)) *fakeAPI {
	return newFakeAPIWith(t, true, route)
}

func newFakeAPIWith(t *testing.T, own bool, route func(w http.ResponseWriter, r *http.Request, body map[string]any)) *fakeAPI {
	t.Helper()
	f := &fakeAPI{t: t, route: route, ownDirectory: own, teams: fakeTeams}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			if err := json.Unmarshal(b, &body); err != nil {
				t.Errorf("request body is not a JSON object: %s", b)
			}
		}
		if !f.ownDirectory && r.Method == http.MethodGet &&
			(r.URL.Path == "/api/v1/orgs/orbit/teams" || r.URL.Path == "/api/v1/orgs/orbit/users") {
			f.mu.Lock()
			f.dirN++
			teams := f.teams
			f.mu.Unlock()
			items := fakeUsers
			if strings.HasSuffix(r.URL.Path, "/teams") {
				items = teams
			}
			writeJSON(w, 200, map[string]any{"items": items, "nextCursor": nil})
			return
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, seen{Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery,
			Auth: r.Header.Get("Authorization"), Body: body})
		f.mu.Unlock()
		if f.route == nil {
			writeJSON(w, 200, map[string]any{})
			return
		}
		f.route(w, r, body)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// total counts every request, the directory ones included.
func (f *fakeAPI) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs) + f.dirN
}

func (f *fakeAPI) requests() []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reqs)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

// session is one MCP server over the fake API.
type session struct {
	t     *testing.T
	api   *fakeAPI
	srv   *Server
	tools *Tools
	logs  *bytes.Buffer
}

func newSession(t *testing.T, profile Profile, api *fakeAPI) *session {
	t.Helper()
	c, err := NewClient(ClientConfig{BaseURL: api.srv.URL, Token: testToken, Org: "orbit",
		ReadOnly: profile == ProfileReviewer})
	if err != nil {
		t.Fatal(err)
	}
	tools, err := NewTools(c, "ORB", profile)
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	return &session{t: t, api: api, tools: tools, logs: logs,
		srv: NewServer(tools, slog.New(slog.NewTextHandler(logs, nil)), "test")}
}

// exchange sends msgs (one JSON line each) and returns stdout and the
// responses by id.
func (s *session) exchange(msgs ...any) (string, map[string]response) {
	s.t.Helper()
	var in bytes.Buffer
	for _, m := range msgs {
		switch v := m.(type) {
		case string:
			in.WriteString(v)
		default:
			b, err := json.Marshal(v)
			if err != nil {
				s.t.Fatal(err)
			}
			in.Write(b)
		}
		in.WriteByte('\n')
	}
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.srv.Serve(ctx, &in, &out); err != nil {
		s.t.Fatalf("serve: %v", err)
	}
	byID := map[string]response{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var r struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Result  json.RawMessage `json:"result"`
			Error   *rpcError       `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			s.t.Fatalf("stdout line is not JSON-RPC: %q", line)
		}
		if r.JSONRPC != "2.0" {
			s.t.Fatalf("jsonrpc = %q", r.JSONRPC)
		}
		byID[string(r.ID)] = response{ID: r.ID, Result: r.Result, Error: r.Error}
	}
	return out.String(), byID
}

func call(id int, name string, args any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args}}
}

// result is a decoded tools/call result.
type result struct {
	Text    string
	IsError bool
}

// callTool runs one tool call and returns its result. A protocol error fails
// the test.
func (s *session) callTool(name string, args any) result {
	s.t.Helper()
	_, resp := s.exchange(call(1, name, args))
	r, ok := resp["1"]
	if !ok {
		s.t.Fatalf("no response to %s", name)
	}
	if r.Error != nil {
		s.t.Fatalf("%s: protocol error %d %s", name, r.Error.Code, r.Error.Message)
	}
	var tr struct {
		Content []struct {
			Type, Text string
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(r.Result.(json.RawMessage), &tr); err != nil {
		s.t.Fatalf("%s: result: %v", name, err)
	}
	if len(tr.Content) != 1 || tr.Content[0].Type != "text" {
		s.t.Fatalf("%s: content = %+v", name, tr.Content)
	}
	return result{Text: tr.Content[0].Text, IsError: tr.IsError}
}

// okJSON calls a tool, requires success and decodes its result.
func (s *session) okJSON(name string, args any, out any) {
	s.t.Helper()
	r := s.callTool(name, args)
	if r.IsError {
		s.t.Fatalf("%s: tool error: %s", name, r.Text)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(r.Text), out); err != nil {
			s.t.Fatalf("%s: decode %q: %v", name, r.Text, err)
		}
	}
}

func TestServer_Serve_Protocol(t *testing.T) {
	api := newFakeAPI(t, nil)
	s := newSession(t, ProfileFull, api)
	out, resp := s.exchange(
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
			"params": map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}}},
		map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"},
		map[string]any{"jsonrpc": "2.0", "id": 2, "method": "ping"},
		map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/list"},
		map[string]any{"jsonrpc": "2.0", "id": 4, "method": "resources/list"},
		`{not json`,
		map[string]any{"jsonrpc": "1.0", "id": 5, "method": "ping"},
		map[string]any{"jsonrpc": "2.0", "id": "six", "method": "tools/call", "params": map[string]any{"name": "nope"}},
	)
	if n := strings.Count(strings.TrimSpace(out), "\n") + 1; n != 7 {
		t.Fatalf("got %d response lines, want 7 (the notification gets none):\n%s", n, out)
	}

	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools map[string]any `json:"tools"`
		} `json:"capabilities"`
		ServerInfo struct{ Name string } `json:"serverInfo"`
	}
	mustDecode(t, resp["1"], &init)
	if init.ProtocolVersion != "2025-03-26" || init.Capabilities.Tools == nil || init.ServerInfo.Name != "launchpad" {
		t.Errorf("initialize = %+v", init)
	}
	if resp["2"].Error != nil {
		t.Errorf("ping: %+v", resp["2"].Error)
	}
	var list struct {
		Tools []struct {
			Name        string         `json:"name"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	mustDecode(t, resp["3"], &list)
	var names []string
	for _, tl := range list.Tools {
		names = append(names, tl.Name)
		if tl.InputSchema["type"] != "object" || tl.InputSchema["additionalProperties"] != false {
			t.Errorf("%s: schema %v", tl.Name, tl.InputSchema)
		}
	}
	want := []string{"get_issue", "list_issues", "save_issue", "list_issue_statuses", "list_teams",
		"get_guidance", "list_products", "list_issue_labels", "list_users", "list_cycles", "list_milestones", "save_comment", "list_comments",
		"get_document", "list_documents", "save_document", "list_projects", "get_project", "save_project",
		"list_project_updates", "save_project_update",
		"prepare_attachment_upload", "create_attachment_from_upload", "download_attachment"}
	if !slices.Equal(names, want) {
		t.Errorf("tools = %v\nwant    %v", names, want)
	}
	for id, code := range map[string]int{"4": codeMethodNotFound, "null": codeParseError, "5": codeInvalidRequest, `"six"`: codeInvalidParams} {
		if resp[id].Error == nil || resp[id].Error.Code != code {
			t.Errorf("id %s: error %+v, want code %d", id, resp[id].Error, code)
		}
	}
	if len(api.requests()) != 0 {
		t.Errorf("protocol messages called the API: %+v", api.requests())
	}
}

func TestServer_Serve_UnknownVersionGetsNewest(t *testing.T) {
	s := newSession(t, ProfileFull, newFakeAPI(t, nil))
	_, resp := s.exchange(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": "1999-01-01"}})
	var init struct{ ProtocolVersion string }
	mustDecode(t, resp["1"], &init)
	if init.ProtocolVersion != protocolVersions[0] {
		t.Errorf("protocolVersion = %q, want %q", init.ProtocolVersion, protocolVersions[0])
	}
}

// A notifications/cancelled for an in-flight call cancels its API request,
// and the cancelled call gets no response (MCP: SHOULD NOT answer).
func TestServer_Serve_Cancel(t *testing.T) {
	started := make(chan struct{})
	aborted := make(chan struct{})
	api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		if r.URL.Path == "/api/v1/orgs/orbit/issues/ORB-1" {
			close(started)
			<-r.Context().Done() // hangs until the client gives up
			close(aborted)
			return
		}
		writeJSON(w, 200, map[string]any{"id": "x"})
	})
	s := newSession(t, ProfileFull, api)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- s.srv.Serve(context.Background(), inR, outW); _ = outW.Close() }()
	lines := make(chan string, 4)
	go func() {
		defer close(lines)
		b := make([]byte, 0, 4096)
		buf := make([]byte, 4096)
		for {
			n, err := outR.Read(buf)
			b = append(b, buf[:n]...)
			for {
				i := bytes.IndexByte(b, '\n')
				if i < 0 {
					break
				}
				lines <- string(b[:i])
				b = b[i+1:]
			}
			if err != nil {
				return
			}
		}
	}()
	write := func(v any) {
		b, _ := json.Marshal(v)
		if _, err := inW.Write(append(b, '\n')); err != nil {
			t.Error(err)
		}
	}
	write(call(7, "get_issue", map[string]any{"id": "ORB-1"}))
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the call never reached the API")
	}
	// stdin stays open: only the notification can end the call.
	write(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": 7}})
	select {
	case <-aborted:
	case <-time.After(10 * time.Second):
		t.Fatal("the cancel did not abort the API request")
	}
	// The next answer is the ping's: the cancelled call sent none.
	write(map[string]any{"jsonrpc": "2.0", "id": 8, "method": "ping"})
	select {
	case line := <-lines:
		if !strings.Contains(line, `"id":8`) {
			t.Errorf("want the ping's answer, got %s", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no answer to the ping")
	}
	_ = inW.Close()
	for line := range lines {
		if strings.Contains(line, `"id":7`) {
			t.Errorf("the cancelled call was answered: %s", line)
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return at end of input")
	}
}

// A signal (ctx cancelled) ends Serve even while stdin stays open, and
// cancels the call in flight.
func TestServer_Serve_SignalWithOpenStdin(t *testing.T) {
	started := make(chan struct{})
	api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		close(started)
		<-r.Context().Done()
	})
	s := newSession(t, ProfileFull, api)
	inR, inW := io.Pipe()
	t.Cleanup(func() { _ = inW.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.srv.Serve(ctx, inR, io.Discard) }()
	b, _ := json.Marshal(call(1, "get_issue", map[string]any{"id": "ORB-1"}))
	if _, err := inW.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "canceled") {
			t.Errorf("Serve = %v, want context canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve kept running after the signal")
	}
}

// blockingWriter blocks every Write until release is closed.
type blockingWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingWriter) Write(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return len(p), nil
}

// A signal must end Serve even when the reader stopped on ctx.Done (and so
// never reported a read error) at the same moment the loop saw the closed
// line channel. Before the fix the loop then waited on readErr for ever.
// The race is 50/50 per run, so it is driven 20 times.
func TestServer_Serve_SignalWhileReaderStops(t *testing.T) {
	for i := range 20 {
		s := newSession(t, ProfileFull, newFakeAPI(t, nil))
		w := &blockingWriter{entered: make(chan struct{}), release: make(chan struct{})}
		// Line 1 keeps the loop busy in reply (the writer blocks); line 2
		// leaves the reader blocked on its send until ctx is cancelled.
		in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n" + `{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.srv.Serve(ctx, in, w) }()
		<-w.entered
		time.Sleep(20 * time.Millisecond) // the reader is now blocked sending line 2
		cancel()
		time.Sleep(20 * time.Millisecond) // the reader saw ctx.Done and closed lines
		close(w.release)
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "canceled") {
				t.Fatalf("run %d: Serve = %v, want context canceled", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("run %d: Serve hung after the signal", i)
		}
	}
}

// A second tools/call with the id of one still running is refused; the
// first is not disturbed.
func TestServer_Serve_DuplicateID(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		close(started)
		<-release
		writeJSON(w, 200, map[string]any{"id": "x", "identifier": "ORB-1"})
	})
	s := newSession(t, ProfileFull, api)
	inR, inW := io.Pipe()
	var out syncBuf
	done := make(chan error, 1)
	go func() { done <- s.srv.Serve(context.Background(), inR, &out) }()
	b, _ := json.Marshal(call(5, "get_issue", map[string]any{"id": "ORB-1"}))
	_, _ = inW.Write(append(b, '\n'))
	<-started
	_, _ = inW.Write(append(b, '\n'))
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "already in flight") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	close(release)
	_ = inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "already in flight") || !strings.Contains(got, "ORB-1") {
		t.Errorf("output:\n%s", got)
	}
	if n := len(api.requests()); n != 1 {
		t.Errorf("API requests = %d, want 1", n)
	}
}

// A line over the limit is refused on its own; the session goes on.
func TestServer_Serve_LineTooLong(t *testing.T) {
	s := newSession(t, ProfileFull, newFakeAPI(t, nil))
	s.srv.maxLine = 200
	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"x":"` + strings.Repeat("a", 100<<10) + `"}}`
	out, resp := s.exchange(big, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "ping"})
	if e := resp["null"].Error; e == nil || !strings.Contains(e.Message, "larger than 200 bytes") {
		t.Errorf("long line: %+v\n%s", resp["null"], out)
	}
	if resp["2"].Error != nil || resp["2"].Result == nil {
		t.Errorf("the session did not go on: %s", out)
	}
}

// syncBuf is a bytes.Buffer safe for one writer and one reader.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// At the end of stdin, a call still running finishes and answers.
func TestServer_Serve_EOFLetsCallsFinish(t *testing.T) {
	api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		time.Sleep(200 * time.Millisecond)
		writeJSON(w, 200, map[string]any{"id": "x", "identifier": "ORB-1"})
	})
	s := newSession(t, ProfileFull, api)
	r := s.callTool("get_issue", map[string]any{"id": "ORB-1"}) // exchange closes stdin at once
	if r.IsError || !strings.Contains(r.Text, "ORB-1") {
		t.Errorf("result = %+v", r)
	}
}

// Calls run concurrently: each fake request waits until both have arrived,
// so a server that ran them one after the other would time both out.
func TestServer_Serve_Concurrent(t *testing.T) {
	var mu sync.Mutex
	arrived := 0
	both := make(chan struct{})
	api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		mu.Lock()
		arrived++
		if arrived == 2 {
			close(both)
		}
		mu.Unlock()
		select {
		case <-both:
			writeJSON(w, 200, map[string]any{"id": "x"})
		case <-time.After(5 * time.Second):
			apiErr(w, 500, "internal_error", "the other call never arrived")
		}
	})
	s := newSession(t, ProfileFull, api)
	_, resp := s.exchange(call(1, "get_issue", map[string]any{"id": "ORB-1"}), call(2, "get_issue", map[string]any{"id": "ORB-2"}))
	if len(resp) != 2 {
		t.Fatalf("responses = %d", len(resp))
	}
	for id, r := range resp {
		if b, _ := json.Marshal(r.Result); strings.Contains(string(b), "never arrived") || strings.Contains(string(b), `"isError":true`) {
			t.Errorf("call %s: %s", id, b)
		}
	}
}

// --- the reviewer profile ---------------------------------------------------

// writeCalls is a valid call of every tool that is not in the reviewer
// profile. Under the full profile each one reaches the API; under the
// reviewer profile none may.
var writeCalls = map[string]map[string]any{
	"list_issues":                   {},
	"save_issue":                    {"title": "x", "team": "ORB"},
	"list_issue_statuses":           {"team": "ORB"},
	"list_teams":                    {},
	"get_guidance":                  {},
	"list_products":                 {},
	"list_issue_labels":             {},
	"list_users":                    {},
	"list_cycles":                   {"teamId": "ORB"},
	"list_milestones":               {"project": "0190f5f0-0000-7000-8000-000000000001"},
	"save_comment":                  {"issueId": "ORB-1", "body": "x"},
	"list_documents":                {},
	"save_document":                 {"title": "x"},
	"list_projects":                 {},
	"get_project":                   {"query": "0190f5f0-0000-7000-8000-000000000001"},
	"save_project":                  {"name": "x"},
	"list_project_updates":          {"project": "0190f5f0-0000-7000-8000-000000000001"},
	"save_project_update":           {"project": "0190f5f0-0000-7000-8000-000000000001", "health": "on_track"},
	"prepare_attachment_upload":     {"issue": "ORB-1", "filename": "a.png", "contentType": "image/png", "size": 10},
	"create_attachment_from_upload": {"issue": "ORB-1", "assetUrl": "org/x/2026/y/a.png"},
	"download_attachment":           {"issue": "ORB-1", "attachment": "0190f5f0-0000-7000-8000-0000000000a1"},
}

var writeTools = []string{"save_issue", "save_comment", "save_document", "save_project", "save_project_update",
	"prepare_attachment_upload", "create_attachment_from_upload"}

func TestServer_ReviewerProfile_ListsOnlyReaders(t *testing.T) {
	s := newSession(t, ProfileReviewer, newFakeAPI(t, nil))
	_, resp := s.exchange(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	var list struct {
		Tools []struct {
			Name        string         `json:"name"`
			Annotations map[string]any `json:"annotations"`
		} `json:"tools"`
	}
	mustDecode(t, resp["1"], &list)
	var names []string
	for _, tl := range list.Tools {
		names = append(names, tl.Name)
		if tl.Annotations["readOnlyHint"] != true {
			t.Errorf("%s is not marked read-only", tl.Name)
		}
	}
	if !slices.Equal(names, []string{"get_issue", "list_comments", "get_document"}) {
		t.Errorf("reviewer tools = %v", names)
	}
}

// The measurement the ticket asks for: under the reviewer profile, no tool
// outside the three readers can be called, and the API sees no request at
// all. The same calls under the full profile DO reach the API (so the test
// is not passing because the calls were broken).
func TestServer_ReviewerProfile_CannotCallWriteTools(t *testing.T) {
	if len(writeCalls)+len(reviewerTools) != len(catalogue()) {
		t.Fatalf("writeCalls covers %d tools; the catalogue has %d besides the reviewer's %d",
			len(writeCalls), len(catalogue())-len(reviewerTools), len(reviewerTools))
	}
	for name, a := range writeCalls {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
				writeJSON(w, 200, map[string]any{"id": "0190f5f0-0000-7000-8000-000000000009", "identifier": "ORB-9",
					"items": []any{}, "nextCursor": nil})
			})
			rev := newSession(t, ProfileReviewer, api)
			_, resp := rev.exchange(call(1, name, a))
			if e := resp["1"].Error; e == nil || e.Code != codeInvalidParams || !strings.Contains(e.Message, "unknown tool") {
				t.Fatalf("reviewer %s: response %+v, want unknown tool", name, resp["1"])
			}
			if n := api.total(); n != 0 {
				t.Fatalf("reviewer %s reached the API %d times", name, n)
			}

			full := newSession(t, ProfileFull, api)
			_, resp = full.exchange(call(1, name, a))
			if resp["1"].Error != nil {
				t.Fatalf("full %s: %+v", name, resp["1"].Error)
			}
			reqs := api.requests()
			if api.total() == 0 {
				t.Fatalf("full %s did not reach the API; the reviewer check proves nothing", name)
			}
			if slices.Contains(writeTools, name) && !slices.ContainsFunc(reqs, func(r seen) bool { return r.Method != http.MethodGet }) {
				t.Errorf("full %s sent no write: %+v", name, reqs)
			}
		})
	}
}

// The reviewer's three tools still work, and only GET.
func TestServer_ReviewerProfile_ReadersWork(t *testing.T) {
	api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		writeJSON(w, 200, map[string]any{"id": "x", "items": []any{}, "nextCursor": nil})
	})
	s := newSession(t, ProfileReviewer, api)
	s.okJSON("get_issue", map[string]any{"id": "ORB-1"}, nil)
	s.okJSON("list_comments", map[string]any{"issueId": "ORB-1"}, nil)
	s.okJSON("get_document", map[string]any{"id": "spec-abc"}, nil)
	for _, r := range api.requests() {
		if r.Method != http.MethodGet {
			t.Errorf("reviewer sent %s %s", r.Method, r.Path)
		}
	}
	if len(api.requests()) != 3 {
		t.Errorf("requests = %d, want 3", len(api.requests()))
	}
}

// Second layer: even if a write handler were wired into the reviewer
// profile, its read-only client would refuse the request before sending it.
func TestServer_ReviewerProfile_ClientRefusesWrites(t *testing.T) {
	api := newFakeAPI(t, nil)
	s := newSession(t, ProfileReviewer, api)
	for _, tl := range catalogue() {
		if tl.ReadOnly || tl.Name == "download_attachment" { // the latter sends no API write; it is not in the reviewer profile
			continue
		}
		a, err := tl.parseArgs(mustJSON(t, writeCalls[tl.Name]))
		if err != nil {
			t.Fatalf("%s: %v", tl.Name, err)
		}
		_, err = tl.run(context.Background(), s.tools, a)
		if err == nil || !strings.Contains(err.Error(), ErrReadOnly.Error()) {
			t.Errorf("%s through the reviewer client: err = %v, want %v", tl.Name, err, ErrReadOnly)
		}
	}
	for _, r := range api.requests() {
		if r.Method != http.MethodGet {
			t.Errorf("a write reached the API: %s %s", r.Method, r.Path)
		}
	}
}

func TestNewTools_ReviewerNeedsReadOnlyClient(t *testing.T) {
	c, err := NewClient(ClientConfig{BaseURL: "http://localhost:1", Token: testToken, Org: "orbit"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewTools(c, "", ProfileReviewer); err == nil {
		t.Fatal("the reviewer profile accepted a client that can write")
	}
}

// The token never reaches stdout or the log, on success or on any failure.
func TestServer_TokenNeverEchoed(t *testing.T) {
	api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			t.Errorf("Authorization header = %q", r.Header.Get("Authorization"))
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/ORB-1"):
			writeJSON(w, 200, map[string]any{"id": "x"})
		case strings.HasSuffix(r.URL.Path, "/ORB-2"):
			apiErr(w, 401, "invalid_token", "invalid token")
		default:
			w.WriteHeader(500)
		}
	})
	s := newSession(t, ProfileFull, api)
	out, _ := s.exchange(
		map[string]any{"jsonrpc": "2.0", "id": 0, "method": "initialize", "params": map[string]any{}},
		call(1, "get_issue", map[string]any{"id": "ORB-1"}),
		call(2, "get_issue", map[string]any{"id": "ORB-2"}),
		call(3, "get_issue", map[string]any{"id": "ORB-3"}),
		call(4, "save_issue", map[string]any{"title": "t", "links": "not a list of objects"}),
	)
	api.srv.Close() // a transport error too
	out2, _ := s.exchange(call(5, "get_issue", map[string]any{"id": "ORB-1"}))
	all := out + out2 + s.logs.String()
	if strings.Contains(all, testToken) || strings.Contains(all, "TESTSECRET") {
		t.Fatalf("the token leaked:\n%s", all)
	}
	if !strings.Contains(out2, "connect") && !strings.Contains(out2, "refused") && !strings.Contains(out2, "EOF") {
		t.Logf("transport error text: %s", out2)
	}
}

func mustDecode(t *testing.T, r response, out any) {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("error %+v", r.Error)
	}
	raw, ok := r.Result.(json.RawMessage)
	if !ok {
		t.Fatalf("result %T", r.Result)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
