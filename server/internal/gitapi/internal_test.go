package gitapi

import (
	"net/http"
	"net/url"
	"testing"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// T17 #32 rule d: the one RoundTripper that sets Authorization compares the
// request's origin with the integration's and drops the header otherwise.
func TestAuthTransportDropsHeaderForOtherOrigin(t *testing.T) {
	root, _ := url.Parse("https://git.example.com/api/v4")
	var seen http.Header
	at := &authTransport{
		next: rtFunc(func(r *http.Request) (*http.Response, error) {
			seen = r.Header.Clone()
			return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
		}),
		org:   originOf(root),
		token: "tok-secret",
	}
	for _, tc := range []struct {
		url  string
		want string
	}{
		{"https://git.example.com/api/v4/user", "Bearer tok-secret"},
		{"https://git.example.com:443/api/v4/user", "Bearer tok-secret"}, // default port
		{"https://GIT.example.com/x", "Bearer tok-secret"},               // host case
		{"https://git.example.com:8443/x", ""},
		{"http://git.example.com/x", ""},
		{"https://evil.example.com/x", ""},
		{"https://git.example.com.evil.com/x", ""},
	} {
		req, _ := http.NewRequest("GET", tc.url, nil)
		req.Header.Set("Authorization", "Bearer smuggled") // a stray one is replaced or removed too
		if _, err := at.RoundTrip(req); err != nil {
			t.Fatal(err)
		}
		if got := seen.Get("Authorization"); got != tc.want {
			t.Errorf("%s: Authorization %q, want %q", tc.url, got, tc.want)
		}
		if req.Header.Get("Authorization") != "Bearer smuggled" {
			t.Errorf("%s: the caller's request was changed", tc.url)
		}
	}
}
