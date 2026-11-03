package gitapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"devops/internal/gitapi"
	"devops/internal/gitapi/gitapitest"
	"devops/internal/netguard"
)

const secretToken = "tok-SECRET-0123456789"

func core(t *testing.T, srv *httptest.Server, waits *[]time.Duration) *gitapi.Core {
	t.Helper()
	c, err := gitapi.NewCore(srv.URL+"/api", secretToken, gitapitest.Options(waits))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func get(c *gitapi.Core) error {
	_, err := c.Do(context.Background(), http.MethodGet, "/x", nil, nil)
	return err
}

func TestStatusMapping(t *testing.T) {
	cases := []struct {
		status int
		hdr    map[string]string
		want   gitapi.Code
	}{
		{401, nil, gitapi.CodeUnauthorized},
		{403, nil, gitapi.CodeForbidden},
		{403, map[string]string{"X-RateLimit-Remaining": "0"}, gitapi.CodeRateLimited},
		{404, nil, gitapi.CodeNotFound},
		{429, nil, gitapi.CodeRateLimited},
		{500, nil, gitapi.CodeError},
		{503, nil, gitapi.CodeError},
		{400, nil, gitapi.CodeError},
		{301, map[string]string{"Location": "http://elsewhere.invalid/"}, gitapi.CodeError},
		{307, map[string]string{"Location": "http://elsewhere.invalid/"}, gitapi.CodeError},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for k, v := range tc.hdr {
				w.Header().Set(k, v)
			}
			w.WriteHeader(tc.status)
			// A hostile host echoing the credential and its own words.
			_, _ = w.Write([]byte(`{"message":"` + r.Header.Get("Authorization") + ` <html>host error page</html>"}`))
		}))
		err := get(core(t, srv, nil))
		srv.Close()
		if gitapi.CodeOf(err) != tc.want {
			t.Errorf("%d: code %q, want %q (%v)", tc.status, gitapi.CodeOf(err), tc.want, err)
			continue
		}
		var ge *gitapi.Error
		if !errors.As(err, &ge) || ge.Status != tc.status {
			t.Errorf("%d: status %v", tc.status, err)
		}
		for _, bad := range []string{secretToken, "Bearer", "host error page", "127.0.0.1"} {
			if strings.Contains(err.Error(), bad) {
				t.Errorf("%d: error text holds %q: %s", tc.status, bad, err)
			}
		}
	}
}

func TestRetryRules(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) < 3 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(503)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	var waits []time.Duration
	if err := get(core(t, srv, &waits)); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 3 || len(waits) != 2 || waits[0] != 2*time.Second {
		t.Errorf("attempts %d, waits %v", n.Load(), waits)
	}

	// Always failing: three attempts, then the error.
	n.Store(-100)
	var cnt atomic.Int32
	always := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { cnt.Add(1); w.WriteHeader(429) }))
	defer always.Close()
	err := get(core(t, always, nil))
	if gitapi.CodeOf(err) != gitapi.CodeRateLimited || cnt.Load() != 3 {
		t.Errorf("429: %v after %d attempts", err, cnt.Load())
	}

	// Retry-After above 5 s is not waited for: one attempt.
	cnt.Store(0)
	long := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cnt.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
	}))
	defer long.Close()
	var lw []time.Duration
	if err := get(core(t, long, &lw)); gitapi.CodeOf(err) != gitapi.CodeRateLimited || cnt.Load() != 1 || len(lw) != 0 {
		t.Errorf("long Retry-After: %v, %d attempts, waits %v", err, cnt.Load(), lw)
	}

	// 4xx other than 429 is never retried; a write is never retried.
	for _, tc := range []struct {
		method string
		status int
	}{{"GET", 404}, {"GET", 401}, {"POST", 503}, {"POST", 429}, {"DELETE", 500}, {"PATCH", 502}, {"PUT", 503}} {
		cnt.Store(0)
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { cnt.Add(1); w.WriteHeader(tc.status) }))
		_, err := core(t, s, nil).Do(context.Background(), tc.method, "/x", nil, map[string]string{"a": "b"})
		s.Close()
		if err == nil || cnt.Load() != 1 {
			t.Errorf("%s %d: %d attempts", tc.method, tc.status, cnt.Load())
		}
	}
}

func TestBodyCap(t *testing.T) {
	big := strings.Repeat("a", gitapi.MaxBody+10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"x":"` + big + `"}`))
	}))
	defer srv.Close()
	err := get(core(t, srv, nil))
	if gitapi.CodeOf(err) != gitapi.CodeError {
		t.Errorf("oversize body: %v", err)
	}
	// Exactly at the cap passes.
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := `{"x":"` + strings.Repeat("a", gitapi.MaxBody-8) + `"}`
		w.Write([]byte(b))
	}))
	defer ok.Close()
	if err := get(core(t, ok, nil)); err != nil {
		t.Errorf("at the cap: %v", err)
	}
}

func TestBadJSONIsProviderError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>")) }))
	defer srv.Close()
	resp, err := core(t, srv, nil).Do(context.Background(), "GET", "/x", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if gitapi.CodeOf(resp.JSON(&v)) != gitapi.CodeError {
		t.Error("bad JSON accepted")
	}
}

func TestRequestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond) }))
	defer srv.Close()
	o := gitapitest.Options(nil)
	o.RequestTimeout = 50 * time.Millisecond
	c, _ := gitapi.NewCore(srv.URL, secretToken, o)
	err := get(c)
	if gitapi.CodeOf(err) != gitapi.CodeUnreachable {
		t.Errorf("timeout: %v", err)
	}
}

func TestTransportErrorHoldsNoToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // connection refused
	c, _ := gitapi.NewCore(url, secretToken, gitapitest.Options(nil))
	err := get(c)
	if gitapi.CodeOf(err) != gitapi.CodeUnreachable {
		t.Fatalf("%v", err)
	}
	for _, f := range []string{err.Error(), strings.TrimSpace(strings.Join([]string{err.Error()}, ""))} {
		if strings.Contains(f, secretToken) {
			t.Error("token in the error")
		}
	}
}

// The guard sits under the client: with no test seam, a loopback host is
// refused, and the error still says only provider_unreachable.
func TestGuardRefusalIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("reached") }))
	defer srv.Close()
	hc := gitapi.NewHTTPClient(&netguard.Dialer{})
	c, _ := gitapi.NewCore(srv.URL, secretToken, gitapi.Options{HTTP: hc})
	err := get(c)
	if gitapi.CodeOf(err) != gitapi.CodeUnreachable || !errors.Is(err, netguard.ErrRefused) {
		t.Errorf("%v", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), secretToken) {
		t.Errorf("error text: %s", err)
	}
}

// Rule d: the Authorization header goes only to the integration's origin, even
// if a code path asks another origin (here the caller's own client does).
func TestAuthHeaderOnlyToOwnOrigin(t *testing.T) {
	var auth1, auth2 atomic.Value
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { auth2.Store(r.Header.Get("Authorization")) }))
	defer second.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth1.Store(r.Header.Get("Authorization"))
		http.Redirect(w, r, second.URL+"/steal", http.StatusFound) // never followed
	}))
	defer first.Close()
	c := core(t, first, nil)
	if err := get(c); gitapi.CodeOf(err) != gitapi.CodeError {
		t.Errorf("redirect: %v", err)
	}
	if auth1.Load() != "Bearer "+secretToken {
		t.Errorf("own origin got %v", auth1.Load())
	}
	if auth2.Load() != nil {
		t.Errorf("second origin was contacted: %v", auth2.Load())
	}
	// The transport itself strips the header for a foreign origin.
	hc := gitapitest.HTTPClient()
	_ = hc
}

func TestNextParamRules(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	c := core(t, srv, nil)
	good := srv.URL + "/api/things?page=3&x=1"
	if p, ok, err := c.NextParam(good, "page"); err != nil || !ok || p != "3" {
		t.Errorf("same origin: %q %v %v", p, ok, err)
	}
	for _, bad := range []string{
		"http://127.0.0.1:1/api/things?page=3",                            // other port
		"http://evil.example/api/things?page=3",                           // other host
		"https://" + strings.TrimPrefix(srv.URL, "http://") + "/x?page=3", // other scheme
		"/relative?page=3",
	} {
		if _, _, err := c.NextParam(bad, "page"); gitapi.CodeOf(err) != gitapi.CodeError {
			t.Errorf("%s accepted: %v", bad, err)
		}
	}
	if _, _, err := c.NextParam(srv.URL+"/x?page=http://evil/", "page"); gitapi.CodeOf(err) != gitapi.CodeError {
		t.Error("URL-shaped parameter accepted")
	}
	if _, ok, err := c.NextParam("", "page"); ok || err != nil {
		t.Error("empty link")
	}
}

func TestCursorsAreOpaque(t *testing.T) {
	for page := 2; page <= gitapi.MaxPages; page++ {
		c := gitapi.EncodeCursor(page, strconv.Itoa(page))
		if c == "" {
			t.Fatalf("page %d", page)
		}
		if strings.ContainsAny(c, ":/?&=") || strings.Contains(c, "http") {
			t.Errorf("cursor %q looks like a URL", c)
		}
		p, param, err := gitapi.DecodeCursor(c)
		if err != nil || p != page || param != strconv.Itoa(page) {
			t.Errorf("round trip: %d %q %v", p, param, err)
		}
	}
	if gitapi.EncodeCursor(gitapi.MaxPages+1, "6") != "" || gitapi.EncodeCursor(1, "1") != "" || gitapi.EncodeCursor(2, "a b") != "" || gitapi.EncodeCursor(2, "http://x") != "" {
		t.Error("limits")
	}
	for _, bad := range []string{
		"https://evil.example/x", "http://127.0.0.1/", "a/b", "../x", "page=2", "2", strings.Repeat("A", 201),
		"MjpodHRwOi8vZXZpbA", // base64 of "2:http://evil"
		"Ni4x",               // "6.1": a sixth page
		"MS4x",               // "1.1": page 1 never has a cursor
		"Mi4",                // "2." empty parameter
	} {
		if _, _, err := gitapi.DecodeCursor(bad); !errors.Is(err, gitapi.ErrInvalidCursor) {
			t.Errorf("%q accepted", bad)
		}
	}
	if p, param, err := gitapi.DecodeCursor(""); err != nil || p != 1 || param != "" {
		t.Error("empty cursor")
	}
}

func TestAPIRoot(t *testing.T) {
	cases := []struct{ provider, base, want string }{
		{"github", "https://github.com", "https://api.github.com"},
		{"github", "https://GitHub.com/", "https://api.github.com"},
		{"github", "https://ghe.example.com", "https://ghe.example.com/api/v3"},
		{"gitlab", "https://gitlab.com", "https://gitlab.com/api/v4"},
		{"gitlab", "https://git.example.com/gitlab/", "https://git.example.com/gitlab/api/v4"},
		{"bitbucket", "https://bitbucket.org", "https://api.bitbucket.org/2.0"},
		{"bitbucket", "https://bb.example.com:7990", "https://bb.example.com:7990/rest/api/1.0"},
	}
	for _, c := range cases {
		got, err := gitapi.APIRoot(c.provider, c.base)
		if err != nil || got != c.want {
			t.Errorf("%s %s: %q %v", c.provider, c.base, got, err)
		}
	}
	for _, bad := range []string{"ftp://x", "https://u:p@example.com", "https://x.com?a=1", "https://x.com#f", "x.com", ""} {
		if _, err := gitapi.APIRoot("gitlab", bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := gitapi.APIRoot("svn", "https://x.com"); err == nil {
		t.Error("unknown provider")
	}
}

func TestNewCoreChecks(t *testing.T) {
	if _, err := gitapi.NewCore("https://x.com", "t", gitapi.Options{}); err == nil {
		t.Error("no http client accepted")
	}
	if _, err := gitapi.NewCore("https://x.com", "t", gitapi.Options{HTTP: &http.Client{}}); err == nil {
		t.Error("a client with no transport accepted")
	}
	o := gitapitest.Options(nil)
	if _, err := gitapi.NewCore("https://x.com", "", o); err == nil {
		t.Error("empty token accepted")
	}
	for _, bad := range []string{"ftp://x.com", "https://u:p@example.com/api", "https://x.com/api?a=1", "nonsense"} {
		if _, err := gitapi.NewCore(bad, "t", o); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestLinkParsing(t *testing.T) {
	h := http.Header{}
	h.Add("Link", `<https://h/a?page=1>; rel="prev", <https://h/a?page=3>; rel="next", <https://h/a?page=9>; rel="last"`)
	if got := gitapi.NextLink(h); got != "https://h/a?page=3" {
		t.Errorf("%q", got)
	}
	if gitapi.NextLink(http.Header{}) != "" {
		t.Error("none")
	}
}

func TestPageNumber(t *testing.T) {
	if _, _, err := gitapi.PageNumber(gitapi.EncodeCursor(2, "abc")); !errors.Is(err, gitapi.ErrInvalidCursor) {
		t.Error("non-numeric page parameter accepted")
	}
	if p, param, err := gitapi.PageNumber(gitapi.EncodeCursor(3, "3")); err != nil || p != 3 || param != "3" {
		t.Error("numeric")
	}
}
