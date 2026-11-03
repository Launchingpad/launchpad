package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewClient(t *testing.T) {
	tests := []struct {
		name    string
		cfg     ClientConfig
		wantErr string
		wantURL string // base, when ok
	}{
		{name: "https", cfg: ClientConfig{BaseURL: "https://tracker.example", Token: testToken, Org: "orbit"},
			wantURL: "https://tracker.example/api/v1/orgs/orbit"},
		{name: "api suffix and slash", cfg: ClientConfig{BaseURL: "https://tracker.example/api/v1/", Token: testToken, Org: "orbit"},
			wantURL: "https://tracker.example/api/v1/orgs/orbit"},
		{name: "path prefix", cfg: ClientConfig{BaseURL: "https://example.com/tracker", Token: testToken, Org: "orbit"},
			wantURL: "https://example.com/tracker/api/v1/orgs/orbit"},
		{name: "loopback http", cfg: ClientConfig{BaseURL: "http://127.0.0.1:8192", Token: testToken, Org: "orbit"},
			wantURL: "http://127.0.0.1:8192/api/v1/orgs/orbit"},
		{name: "localhost http", cfg: ClientConfig{BaseURL: "http://localhost:8192", Token: testToken, Org: "orbit"},
			wantURL: "http://localhost:8192/api/v1/orgs/orbit"},
		{name: "remote http refused", cfg: ClientConfig{BaseURL: "http://tracker.example", Token: testToken, Org: "orbit"},
			wantErr: "plain http"},
		{name: "remote http allowed", cfg: ClientConfig{BaseURL: "http://app:8080", Token: testToken, Org: "orbit", AllowInsecureHTTP: true},
			wantURL: "http://app:8080/api/v1/orgs/orbit"},
		{name: "no url", cfg: ClientConfig{Token: testToken, Org: "orbit"}, wantErr: "LAUNCHPAD_URL is not set"},
		{name: "relative url", cfg: ClientConfig{BaseURL: "tracker.example", Token: testToken, Org: "orbit"}, wantErr: "absolute"},
		{name: "credentials in url", cfg: ClientConfig{BaseURL: "https://u:p@tracker.example", Token: testToken, Org: "orbit"}, wantErr: "credentials"},
		{name: "query in url", cfg: ClientConfig{BaseURL: "https://tracker.example?x=1", Token: testToken, Org: "orbit"}, wantErr: "query"},
		{name: "no token", cfg: ClientConfig{BaseURL: "https://tracker.example", Org: "orbit"}, wantErr: "LAUNCHPAD_TOKEN is not set"},
		{name: "not a tracker token", cfg: ClientConfig{BaseURL: "https://tracker.example", Token: "lin_api_SECRETVALUE", Org: "orbit"},
			wantErr: "not a tracker token"},
		{name: "token with newline", cfg: ClientConfig{BaseURL: "https://tracker.example", Token: "odt_SECRET\nX: y", Org: "orbit"},
			wantErr: "whitespace"},
		{name: "no org", cfg: ClientConfig{BaseURL: "https://tracker.example", Token: testToken}, wantErr: "LAUNCHPAD_ORG"},
		{name: "bad org", cfg: ClientConfig{BaseURL: "https://tracker.example", Token: testToken, Org: "../x"}, wantErr: "slug"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := NewClient(tt.cfg)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), "SECRET") {
					t.Fatalf("the error echoes the token: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := c.base.String(); got != tt.wantURL {
				t.Errorf("base = %s, want %s", got, tt.wantURL)
			}
		})
	}
}

// A caller-supplied id is one escaped path segment: it can never climb to
// another route.
func TestClient_PathSegments(t *testing.T) {
	api := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		writeJSON(w, 200, map[string]any{})
	})
	s := newSession(t, ProfileFull, api)
	s.okJSON("get_issue", map[string]any{"id": "../tokens"}, nil)
	s.okJSON("get_document", map[string]any{"id": "a/b?c#d"}, nil)
	reqs := api.requests()
	if got := reqs[0].Path; got != "/api/v1/orgs/orbit/issues/..%2Ftokens" {
		t.Errorf("path = %s", got)
	}
	if got := reqs[1].Path; got != "/api/v1/orgs/orbit/documents/a%2Fb%3Fc%23d" {
		t.Errorf("path = %s", got)
	}
	for _, id := range []string{"..", "."} {
		r := s.callTool("get_issue", map[string]any{"id": id})
		if !r.IsError || !strings.Contains(r.Text, "invalid id") {
			t.Errorf("id %q: %+v", id, r)
		}
	}
	if len(api.requests()) != 2 {
		t.Errorf("refused ids reached the API: %+v", api.requests()[2:])
	}
}

// The client never follows a redirect, so the bearer header cannot be sent
// to another host.
func TestClient_NoRedirect(t *testing.T) {
	other := newFakeAPI(t, nil)
	api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		http.Redirect(w, r, other.srv.URL+"/steal", http.StatusFound)
	})
	s := newSession(t, ProfileFull, api)
	r := s.callTool("get_issue", map[string]any{"id": "ORB-1"})
	if !r.IsError || !strings.Contains(r.Text, "302") {
		t.Errorf("result = %+v, want a 302 error", r)
	}
	if n := len(other.requests()); n != 0 {
		t.Fatalf("the redirect was followed (%d requests reached the other host)", n)
	}
}

func TestClient_APIError(t *testing.T) {
	api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		if strings.Contains(r.URL.Path, "plain") {
			w.WriteHeader(502)
			_, _ = w.Write([]byte("<html>bad gateway</html>"))
			return
		}
		apiErr(w, 404, "not_found", "issue not found")
	})
	c, err := NewClient(ClientConfig{BaseURL: api.srv.URL, Token: testToken, Org: "orbit"})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Get(context.Background(), "issues/ORB-404", nil, nil)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 404 || ae.Code != "not_found" || ae.Message != "issue not found" {
		t.Fatalf("err = %#v", err)
	}
	if !IsStatus(err, 404) || err.Error() != "tracker API 404 not_found: issue not found" {
		t.Errorf("err = %v", err)
	}
	err = c.Get(context.Background(), "plain", nil, nil)
	if !errors.As(err, &ae) || ae.Status != 502 || ae.Message != "Bad Gateway" {
		t.Errorf("non-JSON error = %#v", err)
	}
}

func TestClient_ReadOnly(t *testing.T) {
	api := newFakeAPI(t, nil)
	c, err := NewClient(ClientConfig{BaseURL: api.srv.URL, Token: testToken, Org: "orbit", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Post(ctx, "issues", map[string]any{"title": "x"}, nil); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Post: %v", err)
	}
	if err := c.Patch(ctx, "issues/ORB-1", map[string]any{"title": "x"}, nil); !errors.Is(err, ErrReadOnly) {
		t.Errorf("Patch: %v", err)
	}
	if err := c.Get(ctx, "issues/ORB-1", nil, nil); err != nil {
		t.Errorf("Get: %v", err)
	}
	if reqs := api.requests(); len(reqs) != 1 || reqs[0].Method != http.MethodGet {
		t.Errorf("requests = %+v, want the one GET", reqs)
	}
}

func TestClient_ResponseCap(t *testing.T) {
	const limit = 1 << 20
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`"`))
		chunk := strings.Repeat("a", 1<<20)
		for range limit>>20 + 1 {
			_, _ = w.Write([]byte(chunk))
		}
		_, _ = w.Write([]byte(`"`))
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(ClientConfig{BaseURL: srv.URL, Token: testToken, Org: "orbit"})
	if err != nil {
		t.Fatal(err)
	}
	c.maxBody = limit
	var s string
	if err := c.Get(context.Background(), "big", nil, &s); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err = %v", err)
	}
}

func TestCheckCredential_WarnsReviewerWithWriteToken(t *testing.T) {
	api := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		if r.URL.Path != "/api/v1/me" {
			t.Errorf("path = %s", r.URL.Path)
		}
		writeJSON(w, 200, map[string]any{"username": "claude", "scopes": []string{"read", "write"}})
	})
	s := newSession(t, ProfileReviewer, api)
	CheckCredential(context.Background(), s.tools.c, ProfileReviewer, s.srv.log)
	if !strings.Contains(s.logs.String(), "can write") {
		t.Errorf("no warning:\n%s", s.logs.String())
	}
}

// With plain http a proxy from HTTP_PROXY would see the bearer token, so the
// client uses none; over https the proxy (a CONNECT tunnel) is kept.
func TestNewClient_ProxyOnlyOverTLS(t *testing.T) {
	for url, wantProxy := range map[string]bool{"http://localhost:8192": false, "http://app:8080": false, "https://tracker.example": true} {
		c, err := NewClient(ClientConfig{BaseURL: url, Token: testToken, Org: "orbit", AllowInsecureHTTP: true})
		if err != nil {
			t.Fatal(err)
		}
		tr, ok := c.transport().(*http.Transport)
		if !ok {
			t.Fatalf("%s: transport %T", url, c.transport())
		}
		if got := tr.Proxy != nil; got != wantProxy {
			t.Errorf("%s: proxy set = %v, want %v", url, got, wantProxy)
		}
	}
}
