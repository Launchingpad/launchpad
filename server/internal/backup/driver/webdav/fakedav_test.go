package webdav

import (
	"encoding/xml"
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

const (
	davBase = "/remote.php/dav/files/alice/"
	davUser = "alice"
	davPass = "app-pass-SECRET-123"
	// leakWords is what the fake puts in every error reply; no error may carry it.
	leakWords = "REMOTE-WORDS-MUST-NOT-LEAK"
)

// fakeDAV is a small in-memory WebDAV server (PUT, MKCOL, PROPFIND, GET,
// DELETE, Basic auth, 207 multi-status) for the driver's unit tests.
type fakeDAV struct {
	mu    sync.Mutex
	dirs  map[string]bool   // decoded collection paths without a trailing slash
	files map[string][]byte // decoded file paths
	reqs  []string          // "METHOD decoded-path"
	auths []string          // Authorization header of every request
	// fault runs first; true means it answered.
	fault func(w http.ResponseWriter, r *http.Request) bool
	// extra is raw XML <d:response> elements added to every Depth 1 PROPFIND.
	extra string
	// sizeLie is added to the content length a PROPFIND reports for a file.
	sizeLie int64
	srv     *httptest.Server
}

func newFake() *fakeDAV {
	f := &fakeDAV{dirs: map[string]bool{}, files: map[string][]byte{}}
	f.dirs[strings.TrimSuffix(davBase, "/")] = true
	return f
}

func (f *fakeDAV) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func (f *fakeDAV) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

func (f *fakeDAV) has(p string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.files[p]
	return ok
}

func (f *fakeDAV) setFile(p string, b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[p] = b
}

func davErr(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0"?><d:error xmlns:d="DAV:"><s:message xmlns:s="x">%s alice %s</s:message></d:error>`, leakWords, davPass)
}

func (f *fakeDAV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r.Method+" "+r.URL.Path)
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	f.mu.Unlock()
	if f.fault != nil && f.fault(w, r) {
		return
	}
	u, p, ok := r.BasicAuth()
	if !ok || u != davUser || p != davPass {
		w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
		davErr(w, 401)
		return
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch r.Method {
	case "MKCOL":
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.dirs[path] || f.files[path] != nil {
			davErr(w, 405)
			return
		}
		if !f.dirs[parent(path)] {
			davErr(w, 409)
			return
		}
		f.dirs[path] = true
		w.WriteHeader(201)
	case http.MethodPut:
		b, err := io.ReadAll(r.Body)
		if err != nil || int64(len(b)) != r.ContentLength {
			davErr(w, 400)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.dirs[parent(path)] {
			davErr(w, 409)
			return
		}
		_, had := f.files[path]
		f.files[path] = b
		if had {
			w.WriteHeader(204)
		} else {
			w.WriteHeader(201)
		}
	case "PROPFIND":
		f.propfind(w, r, path)
	case http.MethodGet:
		f.mu.Lock()
		b, ok := f.files[path]
		f.mu.Unlock()
		if !ok {
			davErr(w, 404)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		_, _ = w.Write(b)
	case http.MethodDelete:
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.files[path]; !ok {
			davErr(w, 404)
			return
		}
		delete(f.files, path)
		w.WriteHeader(204)
	default:
		davErr(w, 405)
	}
}

func parent(p string) string {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return ""
	}
	return p[:i]
}

func escPath(p string) string { return (&url.URL{Path: p}).EscapedPath() }

func fileResp(p string, n int) string {
	return fmt.Sprintf(`<d:response><d:href>%s</d:href><d:propstat><d:prop><d:getcontentlength>%d</d:getcontentlength><d:resourcetype/></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, escPath(p), n)
}

func dirResp(p string) string {
	return fmt.Sprintf(`<d:response><d:href>%s/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`, escPath(p))
}

func (f *fakeDAV) propfind(w http.ResponseWriter, r *http.Request, path string) {
	io.Copy(io.Discard, r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="utf-8"?><d:multistatus xmlns:d="DAV:">`)
	if b, ok := f.files[path]; ok {
		sb.WriteString(fileResp(path, len(b)+int(f.sizeLie)))
	} else if f.dirs[path] {
		sb.WriteString(dirResp(path))
		if r.Header.Get("Depth") == "1" {
			var names []string
			for p := range f.files {
				if parent(p) == path {
					names = append(names, p)
				}
			}
			for p := range f.dirs {
				if parent(p) == path && p != path {
					names = append(names, p)
				}
			}
			sort.Strings(names)
			for _, p := range names {
				if f.dirs[p] {
					sb.WriteString(dirResp(p))
				} else {
					sb.WriteString(fileResp(p, len(f.files[p])+int(f.sizeLie)))
				}
			}
			sb.WriteString(f.extra)
		}
	} else {
		davErr(w, 404)
		return
	}
	sb.WriteString(`</d:multistatus>`)
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(207)
	_, _ = w.Write([]byte(sb.String()))
}

var _ = xml.Name{}
