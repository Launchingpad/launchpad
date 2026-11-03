package api

import "net/http"

// CORS for the web app (T6a). Only the exact origins in WEB_ORIGINS get CORS
// headers, with credentials allowed (the session cookie). There is no
// wildcard: with credentials a wildcard would hand the session to any site.
const (
	corsAllowMethods  = "GET, POST, PATCH, DELETE"
	corsAllowHeaders  = "Authorization, Content-Type, X-CSRF-Token, Idempotency-Key, Last-Event-ID"
	corsExposeHeaders = "Idempotent-Replayed, Retry-After"
	corsMaxAge        = "600"
)

// cors returns the middleware. With no origins it adds nothing at all. A
// preflight from an allowed origin is answered here, before auth (a
// preflight carries no credentials). A preflight from any other origin
// falls through to the router and gets no CORS headers, so the browser
// blocks the real request.
func cors(origins []string) func(http.Handler) http.Handler {
	if len(origins) == 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	allowed := make(map[string]bool, len(origins))
	for _, o := range origins {
		allowed[o] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Add("Vary", "Origin") // the answer depends on Origin; caches must key on it
			origin := r.Header.Get("Origin")
			if origin == "" || !allowed[origin] {
				next.ServeHTTP(w, r)
				return
			}
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				h.Add("Vary", "Access-Control-Request-Method")
				h.Add("Vary", "Access-Control-Request-Headers")
				h.Set("Access-Control-Allow-Methods", corsAllowMethods)
				h.Set("Access-Control-Allow-Headers", corsAllowHeaders)
				h.Set("Access-Control-Max-Age", corsMaxAge)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			h.Set("Access-Control-Expose-Headers", corsExposeHeaders)
			next.ServeHTTP(w, r)
		})
	}
}
