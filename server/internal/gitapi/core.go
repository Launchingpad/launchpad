package gitapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Options builds a Core. HTTP must be a client on the netguard dialer
// (NewHTTPClient); it is required.
type Options struct {
	HTTP *http.Client
	// Headers are sent on every request besides Authorization (GitHub's
	// Accept and API version).
	Headers http.Header
	// Sleep waits between retries; nil is time.Sleep (a test passes a stub).
	Sleep func(time.Duration)
	// RequestTimeout overrides the 10 s of one request (tests only).
	RequestTimeout time.Duration
}

// Core is the guarded client of one integration: one API root, one token. The
// token is sent only to that root's origin (T17 #32 rule d).
type Core struct {
	root  *url.URL
	hc    *http.Client
	hdr   http.Header
	sleep func(time.Duration)
}

type origin struct{ scheme, host, port string }

func originOf(u *url.URL) origin {
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return origin{strings.ToLower(u.Scheme), strings.ToLower(u.Hostname()), port}
}

// authTransport sets the Authorization header only on a request to the
// integration's own origin, and strips it from any other: a code path that
// forgot rules a to c still cannot send the token away.
type authTransport struct {
	next  http.RoundTripper
	org   origin
	token string
}

func (a *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context()) // a RoundTripper must not change the request it is given
	if originOf(r.URL) == a.org {
		r.Header.Set("Authorization", "Bearer "+a.token)
	} else {
		r.Header.Del("Authorization")
	}
	return a.next.RoundTrip(r)
}

// NewCore builds the client of one integration. apiRoot is the API root (see
// APIRoot); token is sent as a Bearer header and nowhere else.
func NewCore(apiRoot, token string, o Options) (*Core, error) {
	if o.HTTP == nil {
		return nil, errors.New("gitapi: an http client on the netguard dialer is required")
	}
	if o.HTTP.Transport == nil {
		return nil, errors.New("gitapi: the http client must carry the guarded transport (no default transport)")
	}
	if token == "" {
		return nil, errors.New("gitapi: empty token")
	}
	u, err := url.Parse(apiRoot)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("gitapi: bad API root")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = strings.TrimRight(u.RawPath, "/")
	hc := *o.HTTP // a copy: the caller's client is not changed
	hc.Transport = &authTransport{next: hc.Transport, org: originOf(u), token: token}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	hc.Timeout = RequestTimeout
	if o.RequestTimeout > 0 {
		hc.Timeout = o.RequestTimeout
	}
	sleep := o.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	return &Core{root: u, hc: &hc, hdr: o.Headers, sleep: sleep}, nil
}

// Root returns the API root without its trailing slash.
func (c *Core) Root() string { return c.root.String() }

// SameOrigin reports whether a URL from the host (a next link) has the API
// root's scheme, host and port (T17 #32 rule c).
func (c *Core) SameOrigin(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.IsAbs() && originOf(u) == originOf(c.root)
}

// NextParam reads the page parameter `name` from a next link a host sent. The
// link is never requested: its origin must equal the API root's (else
// provider_error), and only the parameter, which must match the cursor
// charset, is used. ok is false when the link has no such parameter.
func (c *Core) NextParam(link, name string) (param string, ok bool, err error) {
	if link == "" {
		return "", false, nil
	}
	if !c.SameOrigin(link) {
		return "", false, newErr(CodeError, 0)
	}
	u, _ := url.Parse(link)
	v := u.Query().Get(name)
	if v == "" {
		return "", false, nil
	}
	if !ValidParam(v) {
		return "", false, newErr(CodeError, 0)
	}
	return v, true, nil
}

// Response is a successful (2xx) answer.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// JSON decodes the body into v.
func (r *Response) JSON(v any) error {
	if err := json.Unmarshal(r.Body, v); err != nil {
		return newErr(CodeError, r.Status)
	}
	return nil
}

// Do sends one request to the API root. path is an already-escaped path that
// starts with "/" (the client builds it from ids it made); query is added to
// it. body, if not nil, is sent as JSON. A GET is tried up to MaxAttempts
// times on a 5xx or a 429, waiting for Retry-After up to 5 s; every other
// method is sent once. Any non-2xx answer is a *Error with our code; a 3xx is
// provider_error and is not followed.
func (c *Core) Do(ctx context.Context, method, path string, query url.Values, body any) (*Response, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, newErr(CodeError, 0)
	}
	full := c.root.String() + path
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, newErr(CodeError, 0)
		}
	}
	attempts := 1
	if method == http.MethodGet {
		attempts = MaxAttempts
	}
	for attempt := 1; ; attempt++ {
		resp, retryAfter, err := c.once(ctx, method, full, payload)
		if err == nil {
			return resp, nil
		}
		var pe *Error
		retryable := errors.As(err, &pe) && (pe.Status == http.StatusTooManyRequests || pe.Status >= 500 && pe.Status <= 599)
		if !retryable || attempt >= attempts || ctx.Err() != nil {
			return nil, err
		}
		wait := time.Duration(attempt) * 250 * time.Millisecond
		if retryAfter > 0 {
			if retryAfter > MaxRetryWait {
				return nil, err // too long to wait inside a request
			}
			wait = retryAfter
		}
		c.sleep(wait)
	}
}

func (c *Core) once(ctx context.Context, method, full string, payload []byte) (*Response, time.Duration, error) {
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, full, rd)
	if err != nil {
		return nil, 0, newErr(CodeError, 0)
	}
	for k, vs := range c.hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set("User-Agent", "launchpad")
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		// The text of err can carry the URL and the dial error; it is kept as
		// the cause only and never printed. The request, with its header, is
		// not referenced by the error.
		return nil, 0, &Error{Code: CodeUnreachable, cause: errors.Unwrap(err)}
	}
	defer resp.Body.Close()
	data, rerr := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if rerr != nil {
		return nil, 0, &Error{Code: CodeUnreachable, Status: resp.StatusCode}
	}
	if len(data) > MaxBody {
		return nil, 0, newErr(CodeError, resp.StatusCode)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return &Response{Status: resp.StatusCode, Header: resp.Header, Body: data}, 0, nil
	}
	return nil, retryAfter(resp.Header), statusError(resp)
}

func retryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
		if n == 0 {
			return time.Millisecond
		}
		return time.Duration(n) * time.Second
	}
	return 0
}

func statusError(resp *http.Response) *Error {
	s := resp.StatusCode
	switch {
	case s == http.StatusUnauthorized:
		return newErr(CodeUnauthorized, s)
	case s == http.StatusForbidden:
		if resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != "" {
			return newErr(CodeRateLimited, s)
		}
		return newErr(CodeForbidden, s)
	case s == http.StatusNotFound:
		return newErr(CodeNotFound, s)
	case s == http.StatusTooManyRequests:
		return newErr(CodeRateLimited, s)
	}
	return newErr(CodeError, s) // 3xx (not followed), other 4xx, 5xx
}

// Is404 reports whether err is a provider_not_found.
func Is404(err error) bool { return CodeOf(err) == CodeNotFound }
