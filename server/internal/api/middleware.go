package api

import (
	"context"
	"net/http"
	"runtime/debug"
	"time"

	"devops/internal/httpx"
)

// statusWriter records the status for the access log. Unwrap lets
// http.ResponseController reach the real writer (Flush, write deadlines), so
// SSE works through this wrapper.
type statusWriter struct {
	http.ResponseWriter
	status   int
	panicked bool // the handler panicked; the access log reports 500
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// baseMiddleware is the chain every request passes, outermost first. The
// order matters: requestLog wraps recoverer, so the 500 a panic produces is
// written through the access log's statusWriter and logged as 500; CORS
// answers a preflight before any auth runs.
func (s *Server) baseMiddleware() []func(http.Handler) http.Handler {
	return []func(http.Handler) http.Handler{s.requestLog, s.recoverer, securityHeaders, cors(s.CORSOrigins)}
}

// requestLog writes one structured line per request. It logs no caller IP,
// no User-Agent (house rule: never store or keep either), no query string,
// no header and no cookie. The line is written in a defer, so a request
// whose handler aborts still gets one.
func (s *Server) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			status := sw.status
			if sw.panicked {
				status = http.StatusInternalServerError
			}
			s.log.InfoContext(r.Context(), "request",
				"method", r.Method, "path", logPath(r), "status", status,
				"duration_ms", time.Since(start).Milliseconds())
		}()
		next.ServeHTTP(sw, r)
	})
}

// findStatusWriter unwraps w down to the access log's statusWriter.
func findStatusWriter(w http.ResponseWriter) *statusWriter {
	for w != nil {
		if sw, ok := w.(*statusWriter); ok {
			return sw
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return nil
		}
		w = u.Unwrap()
	}
	return nil
}

// recoverer turns a handler panic into a logged 500 with the error envelope.
// When the handler had already started its response, it does NOT append an
// error body to it (that would corrupt the response): it aborts the
// connection instead, so the client sees a broken response, never a
// truncated one that looks whole.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			s.log.ErrorContext(r.Context(), "panic", "value", v, "stack", string(debug.Stack()))
			sw := findStatusWriter(w)
			if sw != nil {
				sw.panicked = true
				if sw.status != 0 {
					panic(http.ErrAbortHandler)
				}
			}
			httpx.WriteError(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
		}()
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.pool.Ping(ctx); err != nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, httpx.CodeInternal, "database unavailable")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
