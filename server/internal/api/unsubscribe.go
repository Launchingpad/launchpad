package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"devops/internal/httpx"
	"devops/internal/moveapi"
)

// The public one-click unsubscribe (LAUN-34, decisions-T13 #25).
// POST /notifications/unsubscribe/{token}: the token is the credential, so the
// route sits outside Authenticate, reads no cookie, never stores an
// Idempotency-Key, and answers 204 for a valid, used, expired or unknown
// token. GET of the same path is NOT an action (mail scanners fetch links);
// the router answers 405 to it.

const (
	unsubscribePrefix = "/api/v1/notifications/unsubscribe/"

	// DefaultUnsubscribeFloor is the least time the route takes.
	DefaultUnsubscribeFloor = 300 * time.Millisecond

	unsubPerClientPerMinute = 20
	unsubGlobalPerMinute    = 600
	unsubMaxKeys            = 10000
	unsubMaxBody            = 4 << 10
)

// logPath is the path of a request as the logs may hold it: the unsubscribe
// token (the credential) is replaced.
func logPath(r *http.Request) string {
	if p := r.URL.Path; strings.HasPrefix(p, unsubscribePrefix) {
		return unsubscribePrefix + "{token}"
	}
	if p := r.URL.Path; strings.HasPrefix(p, moveapi.DownloadPrefix) {
		return moveapi.DownloadPrefix + "{ticket}" // LAUN-249: the export ticket
	}
	return r.URL.Path
}

// unsubLimiter is a per-client and a global sliding-window limit on the
// unsubscribe route, in memory and per process. Client keys are hashed with a
// random per-process salt, so the table holds no address. A request with no
// client key (a proxied request with no trusted proxy configured, see
// clientip.go) only meets the global limit.
type unsubLimiter struct {
	mu     sync.Mutex
	now    func() time.Time
	salt   [16]byte
	client map[[32]byte][]time.Time
	global []time.Time
}

func newUnsubLimiter(now func() time.Time) *unsubLimiter {
	l := &unsubLimiter{now: now, client: map[[32]byte][]time.Time{}}
	_, _ = rand.Read(l.salt[:])
	return l
}

func pruneTimes(ts []time.Time, cut time.Time) []time.Time {
	i := 0
	for i < len(ts) && !ts[i].After(cut) {
		i++
	}
	return ts[i:]
}

// allow records a hit and reports whether it is within both limits; when it
// is not, the wait until it would be.
func (l *unsubLimiter) allow(clientKey string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cut := now.Add(-time.Minute)
	l.global = pruneTimes(l.global, cut)
	if len(l.global) >= unsubGlobalPerMinute {
		return false, l.global[0].Sub(cut)
	}
	if clientKey != "" {
		k := sha256.Sum256(append(l.salt[:], clientKey...))
		ts, known := l.client[k]
		ts = pruneTimes(ts, cut)
		if len(ts) >= unsubPerClientPerMinute {
			l.client[k] = ts
			return false, ts[0].Sub(cut)
		}
		if !known && len(l.client) >= unsubMaxKeys {
			for key, v := range l.client {
				if v = pruneTimes(v, cut); len(v) == 0 {
					delete(l.client, key)
				} else {
					l.client[key] = v
				}
			}
			if len(l.client) >= unsubMaxKeys {
				return false, time.Minute // fail closed on a full table
			}
		}
		l.client[k] = append(ts, now)
	}
	l.global = append(l.global, now)
	return true, 0
}

// padTo sleeps until start+floor, or ctx ends.
func padTo(ctx context.Context, start time.Time, floor time.Duration) {
	d := floor - time.Since(start)
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// unsubscribeNotifications: POST /notifications/unsubscribe/{token}.
func (s *Server) unsubscribeNotifications(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer padTo(r.Context(), start, s.UnsubscribeFloor)
	if ok, wait := s.unsub.allow(s.clientKey(r)); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
		httpx.WriteError(w, http.StatusTooManyRequests, httpx.CodeRateLimited, "too many requests")
		return
	}
	// RFC 8058 posts "List-Unsubscribe=One-Click"; the body is not read.
	r.Body = http.MaxBytesReader(w, r.Body, unsubMaxBody)
	if err := s.notify.Unsubscribe(r.Context(), chi.URLParam(r, "token")); err != nil {
		s.fail(w, r, err) // a database error: 500, never the token
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
