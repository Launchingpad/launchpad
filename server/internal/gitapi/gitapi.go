// Package gitapi is ALL outbound HTTP to git hosts (T17 #32): the guarded
// client (response caps, timeouts, retry rules, the token's origin) shared by
// the three provider API clients in gitapi/github, gitapi/gitlab and
// gitapi/bitbucket. The provider parser packages never dial.
//
// The package takes an *http.Client built on internal/netguard (see
// NewHTTPClient); it never builds a transport of its own, so the SSRF guard
// cannot be left out by accident.
package gitapi

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// The limits of T17 #32.
const (
	MaxBody        = 1 << 20          // bytes of a JSON response
	MaxPages       = 5                // pages of one list
	RequestTimeout = 10 * time.Second // one HTTP request
	RouteTimeout   = 30 * time.Second // one call of a client method, retries included
	MaxAttempts    = 3                // GET only, 5xx and 429 only
	MaxRetryWait   = 5 * time.Second  // the longest Retry-After honoured
)

// WithRouteTimeout bounds one client method (T17 #32: 30 s per route).
func WithRouteTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, RouteTimeout)
}

// Code is our own error code for a failed provider call (T17 #28). No host
// text is ever part of it.
type Code string

// The codes. provider_not_found is added to #28's list for a 404 (a repo or a
// hook that is gone).
const (
	CodeUnreachable  Code = "provider_unreachable"
	CodeUnauthorized Code = "provider_unauthorized"
	CodeForbidden    Code = "provider_forbidden"
	CodeRateLimited  Code = "provider_rate_limited"
	CodeError        Code = "provider_error"
	CodeNotFound     Code = "provider_not_found"
)

// Error is a failed provider call. Its text is the code and the HTTP status
// only: never a token, a header, a URL or the host's own words. Unwrap gives
// the transport error (for errors.Is(err, netguard.ErrRefused)); do not print it.
type Error struct {
	Code   Code
	Status int // 0 when there was no response
	cause  error
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("gitapi: %s (HTTP %d)", e.Code, e.Status)
	}
	return "gitapi: " + string(e.Code)
}

// Unwrap returns the underlying transport error, if any.
func (e *Error) Unwrap() error { return e.cause }

func newErr(code Code, status int) *Error { return &Error{Code: code, Status: status} }

// CodeOf returns the Code of err, or "" when err is not a gitapi *Error.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// ErrInvalidCursor is returned for a cursor this package did not make (or one
// that names a sixth page). Callers answer 400 validation_failed.
var ErrInvalidCursor = errors.New("gitapi: invalid cursor")

// ErrBadRepo is returned for a repository id this package did not make.
var ErrBadRepo = errors.New("gitapi: invalid repository id")

// APIRoot derives the API root from a host's base URL (T17 #29): GitHub
// github.com -> https://api.github.com, Enterprise https://HOST/api/v3; GitLab
// https://HOST/api/v4; Bitbucket bitbucket.org -> https://api.bitbucket.org/2.0,
// Data Center https://HOST/rest/api/1.0. A base path is kept. It checks only
// the shape; the scheme and address policy is the caller's (#29) and the
// guard's (#31).
func APIRoot(provider, baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", errors.New("gitapi: base URL must be an http(s) URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Contains(baseURL, "?") || strings.Contains(baseURL, "#") {
		return "", errors.New("gitapi: base URL has user info, a query or a fragment")
	}
	base := strings.TrimRight(u.Scheme+"://"+u.Host+u.EscapedPath(), "/")
	host := strings.ToLower(u.Hostname())
	switch provider {
	case "github":
		if host == "github.com" {
			return "https://api.github.com", nil
		}
		return base + "/api/v3", nil
	case "gitlab":
		return base + "/api/v4", nil
	case "bitbucket":
		if host == "bitbucket.org" {
			return "https://api.bitbucket.org/2.0", nil
		}
		return base + "/rest/api/1.0", nil
	}
	return "", fmt.Errorf("gitapi: unknown provider %q", provider)
}
