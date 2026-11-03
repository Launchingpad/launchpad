package gitapitest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Fake is an httptest host that records every request.
type Fake struct {
	Srv   *httptest.Server
	mu    sync.Mutex
	calls []string
	auth  []string
}

// NewFake starts a host. h answers; returning false answers 404.
func NewFake(t *testing.T, h func(w http.ResponseWriter, r *http.Request) bool) *Fake {
	f := &Fake{}
	f.Srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		if !h(w, r) {
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Srv.Close)
	return f
}

// Calls returns "METHOD request-uri" of every request so far.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// Auths returns the Authorization header of every request so far.
func (f *Fake) Auths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auth...)
}

// Count counts requests whose "METHOD request-uri" starts with prefix.
func (f *Fake) Count(prefix string) int {
	n := 0
	for _, c := range f.Calls() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// JSON writes v as a JSON answer.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
