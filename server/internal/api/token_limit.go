package api

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"devops/internal/httpx"
)

// Token creation by a web session is rate limited per user (decisions-T11
// #6): a token bucket of 5 at once, refilled at 10 an hour. Only a request that
// makes a token uses the allowance. An org admin who
// makes an agent and its token needs 1 or 2; a hijacked session cannot
// mint tokens in a loop. In memory and per process, like the login limits.
const (
	sessionMintBurst   = 5
	sessionMintPerHour = 10
)

type mintBucket struct {
	tokens float64
	last   time.Time
}

// mintLimiter is a per-user token bucket. Safe for concurrent use.
type mintLimiter struct {
	mu      sync.Mutex
	now     func() time.Time
	buckets map[uuid.UUID]*mintBucket
}

func newMintLimiter(now func() time.Time) *mintLimiter {
	return &mintLimiter{now: now, buckets: make(map[uuid.UUID]*mintBucket)}
}

// take takes one token for user, or reports false and the wait for the next.
func (l *mintLimiter) take(user uuid.UUID) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[user]
	if !ok {
		// Only org admins reach this (the route needs the admin scope), so
		// the table stays small; a full bucket is the same as no entry.
		b = &mintBucket{tokens: sessionMintBurst, last: now}
		l.buckets[user] = b
	}
	rate := float64(sessionMintPerHour) / 3600
	if el := now.Sub(b.last).Seconds(); el > 0 {
		b.tokens = min(float64(sessionMintBurst), b.tokens+el*rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false, time.Duration((1 - b.tokens) / rate * float64(time.Second))
	}
	b.tokens--
	return true, 0
}

// refund gives back the token take took, for a request that made no token
// (a typo in the form must not use up the allowance).
func (l *mintLimiter) refund(user uuid.UUID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.buckets[user]; ok {
		b.tokens = min(float64(sessionMintBurst), b.tokens+1)
	}
}

// checkSessionMint is nil, or the 429 (with Retry-After) for w.
func (s *Server) checkSessionMint(w http.ResponseWriter, user uuid.UUID) error {
	ok, wait := s.mintLimit.take(user)
	if ok {
		return nil
	}
	w.Header().Set("Retry-After", strconv.Itoa(max(int((wait+time.Second-1)/time.Second), 1)))
	return httpx.Errorf(http.StatusTooManyRequests, httpx.CodeRateLimited,
		"too many tokens made in the web app; try again later")
}
