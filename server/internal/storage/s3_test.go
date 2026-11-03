package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestNew_Validation(t *testing.T) {
	good := Config{Endpoint: "http://silo:9000", Bucket: "b", AccessKey: "a", SecretKey: "s"}
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "ok", mutate: func(*Config) {}},
		{name: "endpoint with path", mutate: func(c *Config) { c.Endpoint = "http://silo:9000/x" }, wantErr: "S3_ENDPOINT"},
		{name: "endpoint ftp", mutate: func(c *Config) { c.Endpoint = "ftp://silo" }, wantErr: "S3_ENDPOINT"},
		{name: "public with user info", mutate: func(c *Config) { c.PublicEndpoint = "https://u:p@silo" }, wantErr: "S3_PUBLIC_ENDPOINT"},
		{name: "no bucket", mutate: func(c *Config) { c.Bucket = "" }, wantErr: "S3_BUCKET"},
		{name: "no secret", mutate: func(c *Config) { c.SecretKey = "" }, wantErr: "S3_SECRET_KEY"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := good
			tc.mutate(&c)
			_, err := New(c)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("New err = %v, want it to mention %s", err, tc.wantErr)
			}
		})
	}
}

// TestPresignPut_SignsTypeAndLength: the store must refuse another type or
// size, so both are in X-Amz-SignedHeaders; the client is told to send the
// type (a browser sets Content-Length itself). The URL uses the PUBLIC
// endpoint, path-style.
func TestPresignPut_SignsTypeAndLength(t *testing.T) {
	s, err := New(Config{Endpoint: "http://silo:9000", PublicEndpoint: "https://files.example.com",
		Bucket: "att", AccessKey: "ak", SecretKey: "sk"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	up := s.PresignPut("org/o/2026/u/a.png", "image/png", 42, 15*time.Minute)
	u, err := url.Parse(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "files.example.com" || u.Path != "/att/org/o/2026/u/a.png" {
		t.Fatalf("url = %s", up.URL)
	}
	q := u.Query()
	if q.Get("X-Amz-SignedHeaders") != "content-length;content-type;host" {
		t.Fatalf("signed headers = %q", q.Get("X-Amz-SignedHeaders"))
	}
	if q.Get("X-Amz-Expires") != "900" || q.Get("X-Amz-Signature") == "" {
		t.Fatalf("query = %v", q)
	}
	if up.Method != "PUT" || up.Headers["Content-Type"] != "image/png" || len(up.Headers) != 1 {
		t.Fatalf("upload = %+v", up)
	}
	if !up.Expires.Equal(now.Add(15 * time.Minute)) {
		t.Fatalf("expires = %v", up.Expires)
	}
	g, _ := url.Parse(s.PresignGet("org/o/2026/u/a.png", "image/png", `inline; filename="a.png"`, time.Minute))
	if g.Query().Get("response-content-disposition") != `inline; filename="a.png"` || g.Host != "files.example.com" {
		t.Fatalf("get url = %s", g)
	}
}

// TestNew_DropsTheDefaultPort: net/http sends "Host: host" for a URL on the
// scheme's default port, so the signed host must not carry ":443" / ":80".
func TestNew_DropsTheDefaultPort(t *testing.T) {
	tests := []struct{ endpoint, wantHost string }{
		{"https://files.example.com:443", "files.example.com"},
		{"http://silo:80", "silo"},
		{"http://silo:9000", "silo:9000"},
		{"https://files.example.com:8443", "files.example.com:8443"},
		{"https://[2001:db8::1]:443", "[2001:db8::1]"},
		{"http://[::1]:9000", "[::1]:9000"},
	}
	for _, tc := range tests {
		t.Run(tc.endpoint, func(t *testing.T) {
			s, err := New(Config{Endpoint: tc.endpoint, Bucket: "b", AccessKey: "a", SecretKey: "s"})
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(s.PresignGet("k", "", "", time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if u.Host != tc.wantHost {
				t.Fatalf("signed URL host = %q, want %q", u.Host, tc.wantHost)
			}
		})
	}
}

// TestStripURL: an error about a signed URL must not print the URL (its
// query holds the signature).
func TestStripURL(t *testing.T) {
	signed := "https://silo/b/k?X-Amz-Signature=deadbeefsecret"
	err := stripURL(&url.Error{Op: "Put", URL: signed, Err: errors.New("connection refused")})
	if strings.Contains(err.Error(), "deadbeef") || err.Error() != "connection refused" {
		t.Fatalf("stripped error = %q", err)
	}
	plain := errors.New("x")
	if stripURL(plain) != plain {
		t.Fatal("a non-url error must pass through")
	}
}

// TestPut_SignsAndSendsTheSameHost: the server-side PUT signs content-type
// and content-length and sends exactly those, on the host it signed.
func TestPut_SignsAndSendsTheSameHost(t *testing.T) {
	var got struct {
		host, signed, ct string
		length           int64
		body             string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.host, got.signed, got.ct, got.length, got.body = r.Host, r.URL.Query().Get("X-Amz-SignedHeaders"),
			r.Header.Get("Content-Type"), r.ContentLength, string(b)
	}))
	defer srv.Close()
	s, err := New(Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(t.Context(), "org/o/2026/u/a.png", "image/png", 3, strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	if got.host != strings.TrimPrefix(srv.URL, "http://") || got.signed != "content-length;content-type;host" ||
		got.ct != "image/png" || got.length != 3 || got.body != "abc" {
		t.Fatalf("received %+v", got)
	}
}

// slowReader hands out its bytes with a pause before each read.
type slowReader struct {
	data  []byte
	pause time.Duration
}

func (r *slowReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	time.Sleep(r.pause)
	n := copy(p[:1], r.data)
	r.data = r.data[n:]
	return n, nil
}

// TestPut_HasNoTotalTimeout: an upload that takes longer than the
// HEAD/DELETE client's total timeout still completes. A client Timeout
// covers sending the whole body, so a fixed one would fail every large file
// on a slow uplink; Put relies on its ctx instead.
func TestPut_HasNoTotalTimeout(t *testing.T) {
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()
	s, err := New(Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	s.http.Timeout = 100 * time.Millisecond                                  // stands for the 30 s of the small calls
	body := &slowReader{data: []byte("slow!"), pause: 60 * time.Millisecond} // 5 x 60 ms > 100 ms
	if err := s.Put(t.Context(), "org/o/2026/u/a.png", "image/png", 5, body); err != nil {
		t.Fatalf("a slow upload failed: %v", err)
	}
	if string(got) != "slow!" {
		t.Fatalf("server got %q", got)
	}
	// The small calls keep their timeout.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	defer slow.Close()
	s2, _ := New(Config{Endpoint: slow.URL, Bucket: "b", AccessKey: "a", SecretKey: "s"})
	s2.http.Timeout = 100 * time.Millisecond
	if _, err := s2.Head(t.Context(), "k"); err == nil {
		t.Fatal("HEAD must still time out")
	}
}

// TestPut_StalledStoreHitsTheDeadline: a store that stops reading the body
// fails the Put at its size deadline instead of blocking it for ever.
func TestPut_StalledStoreHitsTheDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = r.Body.Read(make([]byte, 1)) // one byte, then nothing
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()
	s, err := New(Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.putDeadline(100 << 20); got != PutSlack+800*time.Second {
		t.Fatalf("deadline of 100 MiB = %s", got)
	}
	s.putMinRate, s.putSlack = 1<<40, 300*time.Millisecond
	body := &slowReader{data: make([]byte, 1<<20), pause: 0}
	start := time.Now()
	err = s.Put(t.Context(), "org/o/2026/u/a.png", "image/png", 1<<20, body)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Put to a stalled store: %v, want a deadline error", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the deadline took %s", took)
	}
	if strings.Contains(err.Error(), "X-Amz-Signature") {
		t.Fatal("the error carries the signed URL")
	}
}
