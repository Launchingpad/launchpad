package login

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"sync"
	"time"
)

// windowLimiter allows at most limit hits per key in a sliding window. It is
// in memory and per process. Keys are HMACs, so it holds no address.
// It is safe for concurrent use.
type windowLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	maxKeys int
	now     func() time.Time
	hits    map[[32]byte][]time.Time
}

func newWindowLimiter(limit int, window time.Duration, maxKeys int, now func() time.Time) *windowLimiter {
	return &windowLimiter{limit: limit, window: window, maxKeys: maxKeys, now: now, hits: make(map[[32]byte][]time.Time)}
}

// prune drops the hits at or before cut, in place. ts is sorted oldest first.
func prune(ts []time.Time, cut time.Time) []time.Time {
	i := 0
	for i < len(ts) && !ts[i].After(cut) {
		i++
	}
	n := copy(ts, ts[i:])
	return ts[:n]
}

// Allow records a hit for key and reports whether it is within the limit.
// A refused hit is not recorded, so the window frees up on time. When the
// table is full of live keys it refuses (fail closed); the route's own rate
// limit keeps that from happening in practice.
func (l *windowLimiter) Allow(key [32]byte) bool {
	ok, _ := l.Take(key)
	return ok
}

// Take is Allow that also says, on a refusal, how long until a hit would be
// allowed (for Retry-After). When the table is full that is the window.
func (l *windowLimiter) Take(key [32]byte) (bool, time.Duration) {
	ok, wait, _ := l.takeAt(key)
	return ok, wait
}

// has reports whether key had a hit in the window.
func (l *windowLimiter) has(key [32]byte) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	ts, ok := l.hits[key]
	if !ok {
		return false
	}
	ts = prune(ts, l.now().Add(-l.window))
	if len(ts) == 0 {
		delete(l.hits, key)
		return false
	}
	l.hits[key] = ts
	return true
}

// undo takes back the hit recorded at at for key (one of them).
func (l *windowLimiter) undo(key [32]byte, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ts := l.hits[key]
	for i := len(ts) - 1; i >= 0; i-- {
		if ts[i].Equal(at) {
			ts = append(ts[:i], ts[i+1:]...)
			break
		}
	}
	if len(ts) == 0 {
		delete(l.hits, key)
		return
	}
	l.hits[key] = ts
}

// takeAt is Take that also returns the time of the recorded hit.
func (l *windowLimiter) takeAt(key [32]byte) (bool, time.Duration, time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cut := now.Add(-l.window)
	ts, known := l.hits[key]
	ts = prune(ts, cut)
	if len(ts) >= l.limit {
		l.hits[key] = ts
		// The oldest hit in the window leaves it first.
		return false, ts[len(ts)-l.limit].Sub(cut), time.Time{}
	}
	if !known && len(l.hits) >= l.maxKeys {
		for k, v := range l.hits {
			if v = prune(v, cut); len(v) == 0 {
				delete(l.hits, k)
			} else {
				l.hits[k] = v
			}
		}
		if len(l.hits) >= l.maxKeys {
			return false, l.window, time.Time{}
		}
	}
	l.hits[key] = append(ts, now)
	return true, 0, now
}

// bucket is a token bucket: perMinute tokens a minute, up to burst. It is
// the coarse per-process limit on a route. Safe for concurrent use.
type bucket struct {
	mu     sync.Mutex
	rate   float64 // tokens per second
	burst  float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

func newBucket(perMinute, burst int, now func() time.Time) *bucket {
	return &bucket{rate: float64(perMinute) / 60, burst: float64(burst), tokens: float64(burst), last: now(), now: now}
}

// Allow takes one token, or reports false when there is none.
func (b *bucket) Allow() bool {
	ok, _ := b.Take()
	return ok
}

// Take is Allow that also says, on a refusal, when the next token comes.
func (b *bucket) Take() (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens = min(b.burst, b.tokens+el*b.rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false, time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
	}
	b.tokens--
	return true, 0
}

// RouteLimit is the coarse per-process token bucket of a public route, for
// routes outside this package (first-run setup). Safe for concurrent use.
type RouteLimit struct{ b *bucket }

// NewRouteLimit allows perMinute requests a minute, up to burst at once.
func NewRouteLimit(perMinute, burst int, now func() time.Time) *RouteLimit {
	if now == nil {
		now = time.Now
	}
	return &RouteLimit{b: newBucket(perMinute, burst, now)}
}

// Take takes one request, or returns a *RateLimitedError (429 rate_limited,
// with the wait for Retry-After).
func (l *RouteLimit) Take() error { return rateLimited(l.b.Take()) }

// KeyLimit allows at most limit hits per key in a sliding window, for
// routes outside this package (sign-up requests and invites). A
// key (an email or a client address) is held only as an HMAC under a
// random per-process key, never in clear. In memory and per process. Safe
// for concurrent use.
type KeyLimit struct {
	key []byte
	l   *windowLimiter
}

// NewKeyLimit allows limit hits per key in window, for at most maxKeys live
// keys (a full table refuses: fail closed).
func NewKeyLimit(limit int, window time.Duration, maxKeys int, now func() time.Time) *KeyLimit {
	if now == nil {
		now = time.Now
	}
	k := make([]byte, 32)
	_, _ = rand.Read(k) // crypto/rand.Read never fails (Go 1.24+)
	return &KeyLimit{key: k, l: newWindowLimiter(limit, window, maxKeys, now)}
}

func (l *KeyLimit) hash(key string) [32]byte {
	m := hmac.New(sha256.New, l.key)
	m.Write([]byte(key))
	var k [32]byte
	copy(k[:], m.Sum(nil))
	return k
}

// Take records a hit for key, or returns a *RateLimitedError (429
// rate_limited, with the wait for Retry-After).
func (l *KeyLimit) Take(key string) error {
	return rateLimited(l.l.Take(l.hash(key)))
}

// TakeThen is Take in front of a wider limit next (the route's global
// bucket), without letting a flood of NEW keys fill the table
// (decisions-T12 #9):
//
//   - a key that is in the table (it had a hit in the window) is checked
//     first: over its limit it is refused and next is never called, so a
//     refused caller does not use up the shared allowance. Within its
//     limit the hit is recorded, then next runs; if next refuses, the hit
//     is taken back;
//   - a NEW key is recorded only after next lets the request in. So keys
//     enter the table no faster than next allows, and random keys that next
//     refuses leave no trace.
func (l *KeyLimit) TakeThen(key string, next func() error) error {
	k := l.hash(key)
	if l.l.has(k) {
		ok, wait, at := l.l.takeAt(k)
		if !ok {
			return rateLimited(false, wait)
		}
		if err := next(); err != nil {
			l.l.undo(k, at)
			return err
		}
		return nil
	}
	if err := next(); err != nil {
		return err
	}
	return rateLimited(l.l.Take(k))
}
