package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"devops/internal/auth"
	"devops/internal/login"
)

// newBareServer is a Server with no database: only routes that never reach
// a store can be exercised.
func newBareServer(log *slog.Logger) *Server {
	return NewServer(nil, auth.NewAuthenticator(nil, log), nil, func(h http.Handler) http.Handler { return h }, nil, log)
}

// requestStatuses returns the status of every access-log line in buf.
func requestStatuses(t *testing.T, buf *bytes.Buffer) []int {
	t.Helper()
	var out []int
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	for sc.Scan() {
		var line struct {
			Msg    string `json:"msg"`
			Status int    `json:"status"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err == nil && line.Msg == "request" {
			out = append(out, line.Status)
		}
	}
	return out
}

// T1 follow-up: a panic is written through the access log's
// status writer (logged as 500, not 0), and never appended to a response
// the handler had already started.
func TestRecoverer_ThroughTheAccessLog(t *testing.T) {
	chain := func(s *Server, h http.Handler) http.Handler {
		mws := s.baseMiddleware()
		for i := len(mws) - 1; i >= 0; i-- {
			h = mws[i](h)
		}
		return h
	}
	t.Run("panic before any write", func(t *testing.T) {
		var buf bytes.Buffer
		s := newBareServer(slog.New(slog.NewJSONHandler(&buf, nil)))
		rec := httptest.NewRecorder()
		chain(s, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })).
			ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != 500 || body["code"] != "internal_error" {
			t.Fatalf("response %d %s", rec.Code, rec.Body)
		}
		if got := requestStatuses(t, &buf); len(got) != 1 || got[0] != 500 {
			t.Fatalf("access log statuses %v, want [500]\n%s", got, buf.String())
		}
		if !strings.Contains(buf.String(), `"msg":"panic"`) {
			t.Fatal("the panic was not logged")
		}
	})
	t.Run("panic after a partial write", func(t *testing.T) {
		var buf bytes.Buffer
		s := newBareServer(slog.New(slog.NewJSONHandler(&buf, nil)))
		rec := httptest.NewRecorder()
		h := chain(s, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("partial"))
			panic("boom")
		}))
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/x", nil))
		}()
		if recovered != http.ErrAbortHandler {
			t.Fatalf("want the connection aborted (http.ErrAbortHandler), got %v", recovered)
		}
		if rec.Body.String() != "partial" {
			t.Fatalf("an error body was appended to a started response: %q", rec.Body.String())
		}
		if got := requestStatuses(t, &buf); len(got) != 1 || got[0] != 500 {
			t.Fatalf("access log statuses %v, want [500]", got)
		}
	})
	t.Run("a normal request logs its status", func(t *testing.T) {
		var buf bytes.Buffer
		s := newBareServer(slog.New(slog.NewJSONHandler(&buf, nil)))
		chain(s, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })).
			ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
		if got := requestStatuses(t, &buf); len(got) != 1 || got[0] != http.StatusTeapot {
			t.Fatalf("statuses %v", got)
		}
	})
}

func TestCORS(t *testing.T) {
	const web = "https://tracker.example.com"
	routes := func(origins []string) http.Handler {
		s := newBareServer(slog.New(slog.DiscardHandler))
		s.CORSOrigins = origins
		return s.Routes()
	}
	do := func(h http.Handler, method, path, origin string, preflight bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if preflight {
			r.Header.Set("Access-Control-Request-Method", "POST")
			r.Header.Set("Access-Control-Request-Headers", "content-type,x-csrf-token")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	h := routes([]string{web, "http://localhost:5173"})

	t.Run("preflight from an allowed origin, before auth", func(t *testing.T) {
		// The store is nil: reaching auth would panic, so this also proves
		// the preflight is answered first.
		rec := do(h, "OPTIONS", "/api/v1/orgs/orbit/issues", web, true)
		hd := rec.Header()
		if rec.Code != 204 || hd.Get("Access-Control-Allow-Origin") != web || hd.Get("Access-Control-Allow-Credentials") != "true" {
			t.Fatalf("%d %v", rec.Code, hd)
		}
		for _, want := range []string{"Authorization", "Content-Type", "X-CSRF-Token", "Idempotency-Key", "Last-Event-ID"} {
			if !strings.Contains(hd.Get("Access-Control-Allow-Headers"), want) {
				t.Errorf("Allow-Headers %q lacks %s", hd.Get("Access-Control-Allow-Headers"), want)
			}
		}
		for _, want := range []string{"GET", "POST", "PATCH", "DELETE"} {
			if !strings.Contains(hd.Get("Access-Control-Allow-Methods"), want) {
				t.Errorf("Allow-Methods lacks %s", want)
			}
		}
		if !strings.Contains(strings.Join(hd.Values("Vary"), ","), "Origin") {
			t.Error("no Vary: Origin")
		}
	})
	t.Run("allowed origin on a real request", func(t *testing.T) {
		rec := do(h, "GET", "/api/v1/openapi.yaml", "http://localhost:5173", false)
		hd := rec.Header()
		if rec.Code != 200 || hd.Get("Access-Control-Allow-Origin") != "http://localhost:5173" ||
			hd.Get("Access-Control-Allow-Credentials") != "true" || hd.Get("Access-Control-Expose-Headers") != "Idempotent-Replayed, Retry-After" {
			t.Fatalf("%d %v", rec.Code, hd)
		}
		// An error answer carries the headers too, so the web app can read it.
		rec = do(h, "GET", "/api/v1/me", web, false)
		if rec.Code != 401 || rec.Header().Get("Access-Control-Allow-Origin") != web {
			t.Fatalf("401 without CORS: %d %v", rec.Code, rec.Header())
		}
	})
	for _, origin := range []string{"https://evil.example", "http://tracker.example.com", "https://tracker.example.com.evil.example", "null"} {
		t.Run("other origin "+origin, func(t *testing.T) {
			for _, preflight := range []bool{true, false} {
				method := "GET"
				if preflight {
					method = "OPTIONS"
				}
				rec := do(h, method, "/api/v1/openapi.yaml", origin, preflight)
				for k := range rec.Header() {
					if strings.HasPrefix(k, "Access-Control-") {
						t.Fatalf("preflight=%v: CORS header %s for %s", preflight, k, origin)
					}
				}
				if preflight && rec.Code == 204 {
					t.Fatal("preflight from another origin was approved")
				}
			}
		})
	}
	t.Run("no WEB_ORIGINS, no CORS at all", func(t *testing.T) {
		h := routes(nil)
		for _, preflight := range []bool{true, false} {
			method := "GET"
			if preflight {
				method = "OPTIONS"
			}
			rec := do(h, method, "/api/v1/openapi.yaml", web, preflight)
			for k := range rec.Header() {
				if strings.HasPrefix(k, "Access-Control-") {
					t.Fatalf("CORS header %s with no origins configured", k)
				}
			}
		}
	})
}

// With no login service, or one with no transport, the public routes answer
// 503 login_unavailable without touching a database.
func TestAuthRoutes_LoginOff(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	for name, l := range map[string]*login.Service{"nil service": nil, "no transport": login.New(nil, log, nil, login.Options{})} {
		t.Run(name, func(t *testing.T) {
			s := newBareServer(log)
			s.Login = l
			h := s.Routes()
			for _, path := range []string{"/api/v1/auth/otp", "/api/v1/auth/otp/verify"} {
				r := httptest.NewRequest("POST", path, strings.NewReader(`{"email":"a@b.co","code":"123456"}`))
				r.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, r)
				var body map[string]string
				_ = json.Unmarshal(rec.Body.Bytes(), &body)
				if rec.Code != 503 || body["code"] != "login_unavailable" {
					t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
				}
			}
			// Logout needs a credential first.
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/v1/auth/logout", nil))
			if rec.Code != 401 {
				t.Fatalf("logout without credentials: %d", rec.Code)
			}
		})
	}
}
