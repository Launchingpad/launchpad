package dropbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// leak is planted in every reply the fake sends; no driver error may contain it.
const leak = "REMOTE-WORDS-MUST-NOT-LEAK"

// dbxHash is Dropbox's content_hash, written again here (independent of the
// driver's code): sha256 of the concatenated sha256 digests of 4 MiB blocks.
func dbxHash(b []byte) string {
	all := sha256.New()
	for i := 0; i < len(b); i += 4 << 20 {
		end := min(i+4<<20, len(b))
		d := sha256.Sum256(b[i:end])
		all.Write(d[:])
	}
	return hex.EncodeToString(all.Sum(nil))
}

type session struct {
	data []byte
}

// fakeDropbox is an in-memory Dropbox for the driver's unit tests: the OAuth
// token endpoint, list_folder with cursors, delete_v2, get_metadata, download
// and upload (single and session). The API host and the content host are two
// servers and a call to the wrong one is a 400.
type fakeDropbox struct {
	mu        sync.Mutex
	appKey    string
	appSecret string
	refresh   string
	code      string
	codeUsed  bool

	noRefreshToken bool  // the exchange answers without a refresh_token
	missingScope   bool  // file calls answer missing_scope
	scopeStatus    int   // its HTTP status: 401 (default; an AuthError) or 403
	expiresIn      int64 // reported lifetime of an access token (seconds)
	tokens         map[string]bool
	tokenSeq       int
	refreshes      int
	exchanges      int

	root     string            // lower-case folder path of the destination, "" or "/backups/main"
	files    map[string][]byte // key: lower-case path (Dropbox paths are not case sensitive)
	names    map[string]string // key -> the name as it was written (Dropbox keeps the case)
	extras   []map[string]any  // extra list entries (folders, deleted, strangers)
	pageSize int
	cursors  map[string]int
	sessions map[string]*session
	sessSeq  int

	calls   map[string]int
	log     []string // every content/sesssion step, with offsets
	maxCall int      // bodies above this are refused 413 (0 = no limit)
	biggest int      // the largest content body seen

	fault     func(ep string, n int, w http.ResponseWriter, r *http.Request) bool
	dropReply map[string]int // ep -> call number whose reply is lost after it was processed

	// download hooks (LAUN-154): the Dropbox-API-Result header and the body sent for the stored bytes.
	dlHeader func(b []byte) (string, bool) // ok=false: send the header as `{"name":"x"}`
	dlBody   func(b []byte) []byte

	api, content *httptest.Server
}

func newFake() *fakeDropbox {
	return &fakeDropbox{
		appKey: "APPKEY-TEST", appSecret: "APPSECRET-TEST", refresh: "REFRESHTOKEN-TEST", code: "AUTHCODE-TEST",
		expiresIn: 14400, tokens: map[string]bool{}, root: "/backups/main", files: map[string][]byte{}, names: map[string]string{},
		pageSize: 2000, cursors: map[string]int{}, sessions: map[string]*session{}, calls: map[string]int{},
		dropReply: map[string]int{},
	}
}

var contentPaths = map[string]bool{
	"/2/files/upload": true, "/2/files/upload_session/start": true, "/2/files/upload_session/append_v2": true,
	"/2/files/upload_session/finish": true, "/2/files/download": true,
}

func (f *fakeDropbox) startServers(newServer func(http.Handler) *httptest.Server) {
	f.api = newServer(f.handler("api"))
	f.content = newServer(f.handler("content"))
}

func (f *fakeDropbox) count(ep string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[ep]
}

func (f *fakeDropbox) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		n += c
	}
	return n
}

func (f *fakeDropbox) steps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

func (f *fakeDropbox) has(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.files[strings.ToLower(path)]
	return ok
}

func (f *fakeDropbox) get(path string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.files[strings.ToLower(path)]
}

func (f *fakeDropbox) put(path string, b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.store(path, b)
}

func (f *fakeDropbox) store(path string, b []byte) {
	k := strings.ToLower(path)
	f.files[k] = b
	f.names[k] = path[strings.LastIndex(path, "/")+1:]
}

func (f *fakeDropbox) revokeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = map[string]bool{}
}

func jsonErr(w http.ResponseWriter, status int, summary string, errObj any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error_summary": summary + "/" + leak, "error": errObj, "user_message": leak})
}

func pathErr(w http.ResponseWriter, field, sub string) {
	jsonErr(w, 409, field+"/"+sub+"/..", map[string]any{".tag": field, field: map[string]any{".tag": sub}})
}

type captured struct {
	hdr  http.Header
	code int
	body []byte
}

func (c *captured) Header() http.Header         { return c.hdr }
func (c *captured) WriteHeader(code int)        { c.code = code }
func (c *captured) Write(b []byte) (int, error) { c.body = append(c.body, b...); return len(b), nil }

func (f *fakeDropbox) handler(kind string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ep := r.URL.Path
		f.mu.Lock()
		f.calls[ep]++
		n := f.calls[ep]
		fault := f.fault
		drop := f.dropReply[ep] == n && n > 0
		f.mu.Unlock()
		if (kind == "content") != contentPaths[ep] && ep != "/oauth2/token" {
			http.Error(w, "WRONG-HOST", 400)
			return
		}
		if ep == "/oauth2/token" && kind != "api" {
			http.Error(w, "WRONG-HOST", 400)
			return
		}
		if fault != nil && fault(ep, n, w, r) {
			return
		}
		if !drop {
			f.serve(ep, w, r)
			return
		}
		c := &captured{hdr: http.Header{}, code: 200}
		f.serve(ep, c, r) // processed, then the reply is lost
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
	})
}

func (f *fakeDropbox) authOK(r *http.Request) bool {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	defer f.mu.Unlock()
	return tok != "" && f.tokens[tok]
}

func (f *fakeDropbox) newToken() string {
	f.tokenSeq++
	t := "ACCESS-" + strconv.Itoa(f.tokenSeq)
	f.tokens[t] = true
	return t
}

func (f *fakeDropbox) serve(ep string, w http.ResponseWriter, r *http.Request) {
	if ep == "/oauth2/token" {
		f.token(w, r)
		return
	}
	if !f.authOK(r) {
		jsonErr(w, 401, "expired_access_token", map[string]any{".tag": "expired_access_token"})
		return
	}
	if f.missingScope {
		st := f.scopeStatus
		if st == 0 {
			st = 401
		}
		jsonErr(w, st, "missing_scope", map[string]any{".tag": "missing_scope", "required_scope": "files.content.write"})
		return
	}
	switch ep {
	case "/2/files/list_folder":
		f.list(w, r, false)
	case "/2/files/list_folder/continue":
		f.list(w, r, true)
	case "/2/files/delete_v2":
		var a struct{ Path string }
		_ = json.NewDecoder(r.Body).Decode(&a)
		f.mu.Lock()
		_, ok := f.files[strings.ToLower(a.Path)]
		delete(f.files, strings.ToLower(a.Path))
		f.mu.Unlock()
		if !ok {
			pathErr(w, "path_lookup", "not_found")
			return
		}
		fmt.Fprint(w, `{"metadata":{".tag":"file"}}`)
	case "/2/files/get_metadata":
		var a struct{ Path string }
		_ = json.NewDecoder(r.Body).Decode(&a)
		f.mu.Lock()
		b, ok := f.files[strings.ToLower(a.Path)]
		f.mu.Unlock()
		if !ok {
			pathErr(w, "path", "not_found")
			return
		}
		f.writeMeta(w, a.Path, b)
	case "/2/files/download":
		var a struct{ Path string }
		_ = json.Unmarshal([]byte(r.Header.Get("Dropbox-API-Arg")), &a)
		f.mu.Lock()
		b, ok := f.files[strings.ToLower(a.Path)]
		f.mu.Unlock()
		if !ok {
			pathErr(w, "path", "not_found")
			return
		}
		hdr, _ := json.Marshal(map[string]any{"name": "x", "size": len(b), "content_hash": dbxHash(b)})
		hv := string(hdr)
		if f.dlHeader != nil {
			hv, _ = f.dlHeader(b)
		}
		if hv != "-" {
			w.Header().Set("Dropbox-API-Result", hv)
		}
		if f.dlBody != nil {
			b = f.dlBody(b)
		}
		_, _ = w.Write(b)
	default:
		f.upload(ep, w, r)
	}
}

func (f *fakeDropbox) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		http.Error(w, "bad", 400)
		return
	}
	b, _ := io.ReadAll(r.Body)
	if r.URL.RawQuery != "" {
		jsonErr(w, 400, "secrets in URL", "invalid_request") // a secret must never be in the URL
		return
	}
	v, _ := url.ParseQuery(string(b))
	f.mu.Lock()
	defer f.mu.Unlock()
	if v.Get("client_id") != f.appKey || v.Get("client_secret") != f.appSecret {
		jsonErr(w, 401, "invalid_client", "invalid_client")
		return
	}
	reply := map[string]any{"token_type": "bearer", "expires_in": f.expiresIn}
	switch v.Get("grant_type") {
	case "refresh_token":
		if v.Get("refresh_token") != f.refresh {
			jsonErr(w, 400, "invalid_grant", "invalid_grant")
			return
		}
		f.refreshes++
	case "authorization_code":
		f.exchanges++
		if v.Get("code") != f.code || f.codeUsed {
			jsonErr(w, 400, "invalid_grant", "invalid_grant")
			return
		}
		f.codeUsed = true
		if !f.noRefreshToken {
			reply["refresh_token"] = f.refresh
		}
	default:
		jsonErr(w, 400, "unsupported_grant_type", "unsupported_grant_type")
		return
	}
	reply["access_token"] = f.newToken()
	_ = json.NewEncoder(w).Encode(reply)
}

func (f *fakeDropbox) writeMeta(w http.ResponseWriter, path string, b []byte) {
	name := path[strings.LastIndex(path, "/")+1:]
	_ = json.NewEncoder(w).Encode(map[string]any{".tag": "file", "name": name, "size": len(b), "content_hash": dbxHash(b)})
}

func (f *fakeDropbox) list(w http.ResponseWriter, r *http.Request, cont bool) {
	var a struct {
		Path   string `json:"path"`
		Cursor string `json:"cursor"`
	}
	_ = json.NewDecoder(r.Body).Decode(&a)
	f.mu.Lock()
	defer f.mu.Unlock()
	start := 0
	if cont {
		i, ok := f.cursors[a.Cursor]
		if !ok {
			pathErr(w, "reset", "x")
			return
		}
		start = i
	} else if strings.ToLower(a.Path) != f.root {
		pathErr(w, "path", "not_found")
		return
	}
	var entries []map[string]any
	for p, b := range f.files {
		i := strings.LastIndex(p, "/")
		if p[:i] == f.root {
			entries = append(entries, map[string]any{".tag": "file", "name": f.names[p], "size": len(b)})
		}
	}
	entries = append(entries, f.extras...)
	sort.Slice(entries, func(i, j int) bool { return entries[i]["name"].(string) < entries[j]["name"].(string) })
	end := min(start+f.pageSize, len(entries))
	more := end < len(entries)
	cur := ""
	if more {
		cur = "a+b/c==" + strconv.Itoa(end) + "-" + strconv.Itoa(len(f.cursors))
		f.cursors[cur] = end
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"entries": entries[start:end], "cursor": cur, "has_more": more})
}

func (f *fakeDropbox) upload(ep string, w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	if len(b) > f.biggest {
		f.biggest = len(b)
	}
	tooBig := f.maxCall > 0 && len(b) > f.maxCall
	f.mu.Unlock()
	if tooBig {
		jsonErr(w, 413, "too_large", map[string]any{".tag": "too_large"})
		return
	}
	var arg struct {
		Path   string `json:"path"`
		Close  bool   `json:"close"`
		Mode   string `json:"mode"`
		Cursor struct {
			SessionID string `json:"session_id"`
			Offset    int64  `json:"offset"`
		} `json:"cursor"`
		Commit struct {
			Path string `json:"path"`
			Mode string `json:"mode"`
		} `json:"commit"`
	}
	if json.Unmarshal([]byte(r.Header.Get("Dropbox-API-Arg")), &arg) != nil || r.Header.Get("Content-Type") != "application/octet-stream" {
		http.Error(w, "bad arg", 400)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch ep {
	case "/2/files/upload":
		if arg.Mode != "overwrite" {
			http.Error(w, "bad mode", 400)
			return
		}
		f.log = append(f.log, "upload")
		f.store(arg.Path, b)
		f.writeMeta(w, arg.Path, b)
	case "/2/files/upload_session/start":
		f.sessSeq++
		id := "SESSION-" + strconv.Itoa(f.sessSeq)
		f.sessions[id] = &session{data: b}
		f.log = append(f.log, "start")
		_ = json.NewEncoder(w).Encode(map[string]any{"session_id": id})
	case "/2/files/upload_session/append_v2":
		s, ok := f.sessions[arg.Cursor.SessionID]
		if !ok {
			jsonErr(w, 409, "not_found", map[string]any{".tag": "not_found"})
			return
		}
		if arg.Cursor.Offset != int64(len(s.data)) {
			jsonErr(w, 409, "incorrect_offset", map[string]any{".tag": "incorrect_offset", "correct_offset": len(s.data)})
			return
		}
		s.data = append(s.data, b...)
		f.log = append(f.log, "append@"+strconv.FormatInt(arg.Cursor.Offset, 10))
		fmt.Fprint(w, "null")
	case "/2/files/upload_session/finish":
		s, ok := f.sessions[arg.Cursor.SessionID]
		if !ok {
			jsonErr(w, 409, "lookup_failed", map[string]any{".tag": "lookup_failed", "lookup_failed": map[string]any{".tag": "not_found"}})
			return
		}
		if arg.Cursor.Offset != int64(len(s.data)) {
			jsonErr(w, 409, "lookup_failed", map[string]any{".tag": "lookup_failed",
				"lookup_failed": map[string]any{".tag": "incorrect_offset", "correct_offset": len(s.data)}})
			return
		}
		if arg.Commit.Mode != "overwrite" {
			http.Error(w, "bad mode", 400)
			return
		}
		s.data = append(s.data, b...)
		f.log = append(f.log, "finish@"+strconv.FormatInt(arg.Cursor.Offset, 10))
		f.store(arg.Commit.Path, s.data)
		delete(f.sessions, arg.Cursor.SessionID)
		f.writeMeta(w, arg.Commit.Path, s.data)
	}
}
