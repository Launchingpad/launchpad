package mcp

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	dlAttID  = "0190f5f0-0000-7000-8000-0000000000a1"
	dlSecret = "SECRETSIGNATURE123"
)

// dlEnv is a fake tracker API plus a fake file host for download_attachment.
type dlEnv struct {
	t      *testing.T
	api    *fakeAPI
	files  *httptest.Server
	dir    string
	meta   map[string]any
	locate func(filesURL string) string // the Location the API answers with; nil: files/obj?sig=...
	// the file host's reply
	body        []byte
	contentType string
	chunked     bool
	filesHandle func(w http.ResponseWriter, r *http.Request)

	mu        sync.Mutex
	filesReqs []seen
}

func newDLEnv(t *testing.T) *dlEnv {
	t.Helper()
	e := &dlEnv{t: t, dir: filepath.Join(t.TempDir(), "dl"), body: []byte("\x89PNG\r\n\x1a\nhello"), contentType: "image/png"}
	e.meta = map[string]any{"id": dlAttID, "issueId": issueID, "kind": "file", "filename": "shot.png",
		"contentType": "image/png", "sizeBytes": len(e.body)}
	e.files = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.filesReqs = append(e.filesReqs, seen{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization")})
		e.mu.Unlock()
		if e.filesHandle != nil {
			e.filesHandle(w, r)
			return
		}
		w.Header().Set("Content-Type", e.contentType)
		if e.chunked {
			w.WriteHeader(200)
			_, _ = w.Write(e.body)
			w.(http.Flusher).Flush()
			return
		}
		_, _ = w.Write(e.body)
	}))
	t.Cleanup(e.files.Close)
	e.api = newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		p := r.URL.Path
		switch {
		case strings.HasPrefix(p, "/files/"):
			w.Header().Set("Content-Type", e.contentType)
			_, _ = w.Write(e.body)
		case strings.HasSuffix(p, "/issues/ORB-7"):
			writeJSON(w, 200, map[string]any{"id": issueID, "identifier": "ORB-7"})
		case strings.HasSuffix(p, "/attachments/"+dlAttID+"/download"):
			loc := e.files.URL + "/obj?sig=" + dlSecret
			if e.locate != nil {
				loc = e.locate(e.files.URL)
			}
			http.Redirect(w, r, loc, http.StatusFound)
		case strings.HasSuffix(p, "/attachments/"+dlAttID):
			writeJSON(w, 200, e.meta)
		default:
			apiErr(w, 404, "not_found", "attachment not found")
		}
	})
	return e
}

func (e *dlEnv) session(t *testing.T, cfg func(*ClientConfig)) *session {
	t.Helper()
	cc := ClientConfig{BaseURL: e.api.srv.URL, Token: testToken, Org: "orbit", DownloadDir: e.dir, FilesURL: e.files.URL}
	if cfg != nil {
		cfg(&cc)
	}
	c, err := NewClient(cc)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := NewTools(c, "ORB", ProfileFull)
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	return &session{t: t, api: e.api, tools: tools, logs: logs,
		srv: NewServer(tools, slog.New(slog.NewTextHandler(logs, nil)), "test")}
}

func (e *dlEnv) fileReqs() []seen {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]seen(nil), e.filesReqs...)
}

func (e *dlEnv) downloadHits() int {
	n := 0
	for _, r := range e.api.requests() {
		if strings.HasSuffix(r.Path, "/download") {
			n++
		}
	}
	return n
}

// noLeak fails when text holds the token, the signature or the bytes.
func (e *dlEnv) noLeak(label string, texts ...string) {
	e.t.Helper()
	for _, s := range texts {
		for _, bad := range []string{testToken, dlSecret, "hello", "../../x"} {
			if strings.Contains(s, bad) {
				e.t.Errorf("%s leaks %q: %s", label, bad, s)
			}
		}
	}
}

func dlArgs() map[string]any { return map[string]any{"issue": "ORB-7", "attachment": dlAttID} }

func (e *dlEnv) leftovers() []string {
	ents, _ := os.ReadDir(e.dir)
	var out []string
	for _, x := range ents {
		out = append(out, x.Name())
	}
	return out
}

func TestDownload_OK(t *testing.T) {
	e := newDLEnv(t)
	e.meta["filename"] = "../../x.png" // hostile user file name
	s := e.session(t, nil)
	var out struct {
		Path, ContentType, AttachmentID string
		SizeBytes                       int64
	}
	s.okJSON("download_attachment", dlArgs(), &out)
	want := filepath.Join(e.dir, dlAttID+".png")
	if out.Path != want || out.ContentType != "image/png" || out.SizeBytes != int64(len(e.body)) || out.AttachmentID != dlAttID {
		t.Fatalf("result = %+v, want path %s", out, want)
	}
	got, err := os.ReadFile(want)
	if err != nil || !bytes.Equal(got, e.body) {
		t.Fatalf("file = %q, %v", got, err)
	}
	if fi, _ := os.Stat(want); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	if l := e.leftovers(); len(l) != 1 {
		t.Errorf("folder = %v", l)
	}
	// The user file name never touched a path.
	if _, err := os.Stat(filepath.Join(e.dir, "..", "..", "x.png")); err == nil {
		t.Error("traversal wrote outside")
	}
	// Token to the API only; the file host got one GET with NO Authorization.
	fr := e.fileReqs()
	if len(fr) != 1 || fr[0].Auth != "" || fr[0].Method != "GET" {
		t.Errorf("file host requests = %+v", fr)
	}
	for _, r := range e.api.requests() {
		if r.Auth != "Bearer "+testToken {
			t.Errorf("API request without the token: %+v", r)
		}
	}
	e.noLeak("logs", s.logs.String())
}

func TestDownload_AttachmentAsURL(t *testing.T) {
	e := newDLEnv(t)
	s := e.session(t, nil)
	a := dlArgs()
	a["attachment"] = e.api.srv.URL + "/api/v1/orgs/orbit/attachments/" + strings.ToUpper(dlAttID) + "/download"
	s.okJSON("download_attachment", a, nil)
	a["attachment"] = "../../etc/passwd"
	if r := s.callTool("download_attachment", a); !r.IsError || !strings.Contains(r.Text, "bad_attachment") {
		t.Errorf("bad id: %+v", r)
	}
}

func TestDownload_TextPlainWithCharset(t *testing.T) {
	e := newDLEnv(t)
	e.body, e.contentType = []byte("note"), "text/plain; charset=utf-8"
	e.meta["contentType"], e.meta["sizeBytes"] = "text/plain", 4
	s := e.session(t, nil)
	var out struct{ Path string }
	s.okJSON("download_attachment", dlArgs(), &out)
	if filepath.Base(out.Path) != dlAttID+".txt" {
		t.Errorf("path = %s", out.Path)
	}
}

// A redirect to a host that is neither the API nor LAUNCHPAD_FILES_URL
// is refused, and nothing is requested from it.
func TestDownload_ForeignHostRefused(t *testing.T) {
	e := newDLEnv(t)
	s := e.session(t, func(c *ClientConfig) { c.FilesURL = "" }) // files host not configured
	r := s.callTool("download_attachment", dlArgs())
	if !r.IsError || !strings.Contains(r.Text, "host_not_allowed") {
		t.Fatalf("result = %+v", r)
	}
	if n := len(e.fileReqs()); n != 0 {
		t.Errorf("the foreign host got %d requests", n)
	}
	e.noLeak("error", r.Text, s.logs.String())
	if l := e.leftovers(); len(l) != 0 {
		t.Errorf("folder = %v", l)
	}
}

func TestDownload_RedirectTargets(t *testing.T) {
	c, err := NewClient(ClientConfig{BaseURL: "https://tracker.example", Token: testToken, Org: "orbit", FilesURL: "https://files.example:9443"})
	if err != nil {
		t.Fatal(err)
	}
	for loc, ok := range map[string]bool{
		"https://tracker.example/x?sig=1":          true,
		"https://files.example:9443/o?sig=1":       true,
		"https://FILES.example:9443/o":             true,
		"https://files.example/o":                  false, // other port
		"https://evil.example/o":                   false,
		"https://tracker.example.evil.example/o":   false,
		"http://files.example:9443/o":              false, // plain http, not loopback
		"https://user:pw@files.example:9443/o":     false,
		"https://files.example:9443@evil.example/": false,
		"//evil.example/o":                         false,
		"file:///etc/passwd":                       false,
	} {
		_, err := c.checkRedirect(loc)
		if (err == nil) != ok {
			t.Errorf("checkRedirect(%q) = %v, want ok=%v", loc, err, ok)
		}
		if err != nil && strings.Contains(err.Error(), "evil") {
			t.Errorf("error echoes the address: %v", err)
		}
	}
}

// The scheme must be the one configured for the host: allowing plain http
// does not let an https files host be reached over http.
func TestDownload_SchemeMustMatchConfiguredHost(t *testing.T) {
	c, err := NewClient(ClientConfig{BaseURL: "https://tracker.example", Token: testToken, Org: "orbit",
		FilesURL: "https://files.example", AllowInsecureHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	for loc, ok := range map[string]bool{
		"https://files.example/o":   true,
		"http://files.example/o":    false,
		"http://tracker.example/o":  false,
		"https://tracker.example/o": true,
	} {
		if _, err := c.checkRedirect(loc); (err == nil) != ok {
			t.Errorf("checkRedirect(%q) = %v, want ok=%v", loc, err, ok)
		}
	}
	// A plain-http files host stays usable when it was configured as http.
	c, _ = NewClient(ClientConfig{BaseURL: "https://tracker.example", Token: testToken, Org: "orbit",
		FilesURL: "http://files.internal:9000", AllowInsecureHTTP: true})
	if _, err := c.checkRedirect("http://files.internal:9000/o"); err != nil {
		t.Errorf("configured http host: %v", err)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, io.ErrShortWrite }

// A write error (a full disk) is "write", not "read"; an endless body stops at
// max+1 bytes written.
func TestDownload_WriteErrorAndEndlessBody(t *testing.T) {
	e := newDLEnv(t)
	e.filesHandle = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		buf := bytes.Repeat([]byte("A"), 4096)
		for r.Context().Err() == nil {
			if _, err := w.Write(buf); err != nil {
				return
			}
		}
	}
	c, err := NewClient(ClientConfig{BaseURL: e.api.srv.URL, Token: testToken, Org: "orbit", FilesURL: e.files.URL})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(e.files.URL + "/obj?sig=" + dlSecret)
	var cw countWriter
	_, err = c.fetchPresigned(context.Background(), u, &cw, "image/png", 1000, 1000)
	if err == nil || !strings.Contains(err.Error(), "too_large") {
		t.Fatalf("err = %v", err)
	}
	if cw.n != 1001 {
		t.Errorf("wrote %d bytes, want exactly max+1 = 1001", cw.n)
	}
	e.filesHandle = nil
	_, err = c.fetchPresigned(context.Background(), u, failWriter{}, "image/png", int64(len(e.body)), 1000)
	if err == nil || !strings.Contains(err.Error(), "(write)") {
		t.Errorf("write failure: %v", err)
	}
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// A relative Location resolves to the API host, and the follow-up carries no
// Authorization.
func TestDownload_RelativeLocation(t *testing.T) {
	e := newDLEnv(t)
	e.locate = func(string) string { return "/files/obj?sig=" + dlSecret }
	s := e.session(t, nil)
	var out struct{ Path string }
	s.okJSON("download_attachment", dlArgs(), &out)
	if len(e.fileReqs()) != 0 {
		t.Error("a relative address went to the files host")
	}
	found := false
	for _, r := range e.api.requests() {
		if strings.HasPrefix(r.Path, "/files/") {
			found = true
			if r.Auth != "" {
				t.Errorf("Authorization on the follow-up: %q", r.Auth)
			}
		}
	}
	if !found {
		t.Error("the follow-up did not go to the API host")
	}
}

// The file host answering with a redirect is not followed (no hop to an
// internal address, and the token is not forwarded).
func TestDownload_SecondHopNotFollowed(t *testing.T) {
	e := newDLEnv(t)
	e.filesHandle = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, e.api.srv.URL+"/api/v1/orgs/orbit/attachments/"+dlAttID+"/download", http.StatusFound)
	}
	s := e.session(t, nil)
	r := s.callTool("download_attachment", dlArgs())
	if !r.IsError || !strings.Contains(r.Text, "file_host_status") {
		t.Fatalf("result = %+v", r)
	}
	if n := e.downloadHits(); n != 1 {
		t.Errorf("download route hit %d times, want 1", n)
	}
	e.noLeak("error", r.Text, s.logs.String())
}

func TestDownload_SizeCaps(t *testing.T) {
	// Record says too large: refused before the download route is called.
	e := newDLEnv(t)
	e.meta["sizeBytes"] = 30 << 20
	s := e.session(t, nil)
	if r := s.callTool("download_attachment", dlArgs()); !r.IsError || !strings.Contains(r.Text, "too_large") {
		t.Errorf("record size: %+v", r)
	}
	if e.downloadHits() != 0 || len(e.fileReqs()) != 0 {
		t.Error("a request went out for a file over the cap")
	}
	// A configured cap above the hard cap is clamped.
	c, _ := NewClient(ClientConfig{BaseURL: e.api.srv.URL, Token: testToken, Org: "orbit", DownloadMaxBytes: 1 << 40})
	if c.dlMax != hardDownloadBytes {
		t.Errorf("dlMax = %d", c.dlMax)
	}
	c, _ = NewClient(ClientConfig{BaseURL: e.api.srv.URL, Token: testToken, Org: "orbit"})
	if c.dlMax != 25<<20 {
		t.Errorf("default dlMax = %d", c.dlMax)
	}

	// By header: the host says it has more than the record.
	e = newDLEnv(t)
	e.meta["sizeBytes"] = 5
	s = e.session(t, nil)
	if r := s.callTool("download_attachment", dlArgs()); !r.IsError || !strings.Contains(r.Text, "too_large") {
		t.Errorf("by header: %+v", r)
	}
	if l := e.leftovers(); len(l) != 0 {
		t.Errorf("folder = %v", l)
	}

	// By header alone: the host announces 1000 bytes and sends 3. The
	// answer is "too_large" before a byte is read, not a cut-off read.
	e = newDLEnv(t)
	e.meta["sizeBytes"] = 5
	e.filesHandle = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("abc"))
	}
	s = e.session(t, nil)
	if r := s.callTool("download_attachment", dlArgs()); !r.IsError || !strings.Contains(r.Text, "too_large") {
		t.Errorf("announced length: %+v", r)
	}

	// By streaming: no Content-Length, more bytes than the cap.
	e = newDLEnv(t)
	e.body = bytes.Repeat([]byte("A"), 1000)
	e.chunked = true
	e.meta["sizeBytes"] = 10
	s = e.session(t, func(c *ClientConfig) { c.DownloadMaxBytes = 10 })
	r := s.callTool("download_attachment", dlArgs())
	if !r.IsError || !strings.Contains(r.Text, "too_large") {
		t.Errorf("by streaming: %+v", r)
	}
	if l := e.leftovers(); len(l) != 0 {
		t.Errorf("folder after an aborted copy = %v", l)
	}
	// Fewer bytes than the record: refused too.
	e = newDLEnv(t)
	e.chunked = true
	e.meta["sizeBytes"] = len(e.body) + 5
	s = e.session(t, nil)
	if r := s.callTool("download_attachment", dlArgs()); !r.IsError || !strings.Contains(r.Text, "size_mismatch") {
		t.Errorf("short body: %+v", r)
	}
}

func TestDownload_WrongType(t *testing.T) {
	e := newDLEnv(t)
	e.contentType = "text/html"
	s := e.session(t, nil)
	r := s.callTool("download_attachment", dlArgs())
	if !r.IsError || !strings.Contains(r.Text, "type_mismatch") {
		t.Fatalf("host type: %+v", r)
	}
	if l := e.leftovers(); len(l) != 0 {
		t.Errorf("folder = %v", l)
	}
	// A record with a type outside the allowlist.
	e = newDLEnv(t)
	e.meta["contentType"] = "text/html"
	s = e.session(t, nil)
	if r := s.callTool("download_attachment", dlArgs()); !r.IsError || !strings.Contains(r.Text, "type_not_allowed") {
		t.Errorf("record type: %+v", r)
	}
	if e.downloadHits() != 0 {
		t.Error("download route called for a type outside the allowlist")
	}
}

func TestDownload_LinkRefused(t *testing.T) {
	e := newDLEnv(t)
	e.meta = map[string]any{"id": dlAttID, "issueId": issueID, "kind": "link", "url": "https://gitlab.example/mr/1",
		"contentType": nil, "sizeBytes": nil, "filename": nil}
	s := e.session(t, nil)
	r := s.callTool("download_attachment", dlArgs())
	if !r.IsError || !strings.Contains(r.Text, "not_a_file") || e.downloadHits() != 0 {
		t.Errorf("result = %+v, hits %d", r, e.downloadHits())
	}
}

func TestDownload_NotFound(t *testing.T) {
	e := newDLEnv(t)
	s := e.session(t, nil)
	a := dlArgs()
	a["attachment"] = "0190f5f0-0000-7000-8000-0000000000ff"
	r := s.callTool("download_attachment", a)
	if !r.IsError || !strings.Contains(r.Text, "404") {
		t.Errorf("unknown attachment: %+v", r)
	}
	// An attachment of another issue in this org looks the same.
	e.meta["issueId"] = "0190f5f0-0000-7000-8000-0000000000d2"
	r = s.callTool("download_attachment", dlArgs())
	if !r.IsError || !strings.Contains(r.Text, "404") || e.downloadHits() != 0 {
		t.Errorf("other issue: %+v", r)
	}
	a = dlArgs()
	a["issue"] = "ORB-999"
	if r := s.callTool("download_attachment", a); !r.IsError || !strings.Contains(r.Text, "404") {
		t.Errorf("unknown issue: %+v", r)
	}
}

func TestDownload_FolderRules(t *testing.T) {
	// A symlinked folder is refused.
	e := newDLEnv(t)
	real := t.TempDir()
	if err := os.Symlink(real, e.dir); err != nil {
		t.Fatal(err)
	}
	s := e.session(t, nil)
	if r := s.callTool("download_attachment", dlArgs()); !r.IsError || !strings.Contains(r.Text, "download_dir") {
		t.Errorf("symlink dir: %+v", r)
	}
	if ents, _ := os.ReadDir(real); len(ents) != 0 {
		t.Error("wrote through the symlink")
	}
	if len(e.fileReqs()) != 0 {
		t.Error("downloaded before the folder check")
	}
	// A folder with mode 0755 is refused.
	e = newDLEnv(t)
	if err := os.Mkdir(e.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(e.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s = e.session(t, nil)
	if r := s.callTool("download_attachment", dlArgs()); !r.IsError || !strings.Contains(r.Text, "download_dir") {
		t.Errorf("0755 dir: %+v", r)
	}
	// A folder that cannot be created is refused.
	e = newDLEnv(t)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s = e.session(t, func(c *ClientConfig) { c.DownloadDir = filepath.Join(blocker, "sub") })
	if r := s.callTool("download_attachment", dlArgs()); !r.IsError || !strings.Contains(r.Text, "download_dir") {
		t.Errorf("uncreatable dir: %+v", r)
	}
}

func TestDownload_DefaultFolder(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	e := newDLEnv(t)
	s := e.session(t, func(c *ClientConfig) { c.DownloadDir = "" })
	var out struct{ Path string }
	s.okJSON("download_attachment", dlArgs(), &out)
	if filepath.Dir(out.Path) != filepath.Join(os.TempDir(), "launchpad-mcp") {
		t.Errorf("path = %s", out.Path)
	}
	if fi, err := os.Stat(filepath.Dir(out.Path)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("folder = %v, %v", fi, err)
	}
}

// A symlink at the final name is refused (not written through, not replaced);
// the file it points to stays as it was.
func TestDownload_FinalNameSymlink(t *testing.T) {
	e := newDLEnv(t)
	if err := os.Mkdir(e.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(e.dir, dlAttID+".png")); err != nil {
		t.Fatal(err)
	}
	s := e.session(t, nil)
	r := s.callTool("download_attachment", dlArgs())
	if !r.IsError || !strings.Contains(r.Text, "target_not_regular") {
		t.Errorf("result = %+v", r)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Errorf("victim = %q", b)
	}
}

func TestDownload_UnreachableHostHidesURL(t *testing.T) {
	e := newDLEnv(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	e.locate = func(string) string { return deadURL + "/obj?sig=" + dlSecret }
	s := e.session(t, func(c *ClientConfig) { c.FilesURL = deadURL })
	r := s.callTool("download_attachment", dlArgs())
	if !r.IsError || !strings.Contains(r.Text, "unreachable") {
		t.Fatalf("result = %+v", r)
	}
	e.noLeak("error", r.Text, s.logs.String())
	if strings.Contains(r.Text, "obj") {
		t.Errorf("error holds the path: %s", r.Text)
	}
}

func TestDownload_FileHostErrorBodyNotShown(t *testing.T) {
	e := newDLEnv(t)
	e.filesHandle = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = io.WriteString(w, "<Error>hello "+dlSecret+"</Error>")
	}
	s := e.session(t, nil)
	r := s.callTool("download_attachment", dlArgs())
	if !r.IsError || !strings.Contains(r.Text, "403") {
		t.Fatalf("result = %+v", r)
	}
	e.noLeak("error", r.Text, s.logs.String())
}

// Not in the reviewer profile; its client stays GET-only.
func TestDownload_NotInReviewerProfile(t *testing.T) {
	api := newFakeAPI(t, nil)
	s := newSession(t, ProfileReviewer, api)
	for _, n := range s.tools.Names() {
		if n == "download_attachment" {
			t.Fatal("download_attachment is in the reviewer profile")
		}
	}
	_, resp := s.exchange(call(1, "download_attachment", dlArgs()))
	if e := resp["1"].Error; e == nil || !strings.Contains(e.Message, "unknown tool") {
		t.Errorf("reviewer call: %+v", resp["1"])
	}
}
