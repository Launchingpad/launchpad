// Package linear reads a Linear workspace through the GraphQL API
// and writes it as an importer export.
//
// It is READ-ONLY by construction: Client.Query refuses any document that
// is not a query (a mutation or a subscription), before a request is made.
// The API key comes from the LINEAR_API_KEY environment variable only; it
// is sent in the Authorization header to api.linear.app and to
// uploads.linear.app, and never appears in a log line or an error.
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Endpoint is the Linear GraphQL API.
const Endpoint = "https://api.linear.app/graphql"

// Client is a rate-limit-aware GraphQL client. It is not safe for
// concurrent use; the exporter runs one request at a time.
type Client struct {
	endpoint string
	key      string
	http     *http.Client
	log      *slog.Logger
	sleep    func(context.Context, time.Duration) error
	// MaxRetries is the number of retries after a rate limit or a server
	// error (default 8).
	MaxRetries int
	// MaxWait caps one back-off sleep (default 15 minutes: Linear's limit
	// window is an hour, and its reset header can point far ahead).
	MaxWait time.Duration
	// MinRemaining: when Linear's X-RateLimit-Requests-Remaining falls
	// to this, the client waits for the reset before the next request.
	MinRemaining int

	waitUntil time.Time
	requests  int
}

// ErrNoKey is returned when LINEAR_API_KEY is empty.
var ErrNoKey = errors.New("LINEAR_API_KEY is not set")

// NewClient makes a client for endpoint with key. Pass Endpoint for the
// real API; tests pass an httptest server.
func NewClient(endpoint, key string, log *slog.Logger) (*Client, error) {
	if strings.TrimSpace(key) == "" {
		return nil, ErrNoKey
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Client{
		endpoint: endpoint, key: strings.TrimSpace(key), log: log,
		http:       &http.Client{Timeout: 90 * time.Second},
		sleep:      sleepCtx,
		MaxRetries: 8, MaxWait: 15 * time.Minute, MinRemaining: 5,
	}, nil
}

// Requests is how many HTTP requests the client has sent.
func (c *Client) Requests() int { return c.requests }

// readOnlyRe matches a document that is a query: either the shorthand
// `{ ... }` or `query Name(...) { ... }`. Anything else is refused.
var readOnlyRe = regexp.MustCompile(`^\s*(\{|query\b)`)

// mutationRe finds an operation keyword that is not a query anywhere in
// the document, so a second operation cannot hide behind a first query.
var mutationRe = regexp.MustCompile(`(?i)\b(mutation|subscription)\b`)

// ErrNotReadOnly is returned for a document that could change Linear.
var ErrNotReadOnly = errors.New("linear client: only read-only queries are allowed")

// CheckReadOnly refuses a GraphQL document that is not a single query.
func CheckReadOnly(doc string) error {
	if !readOnlyRe.MatchString(doc) || mutationRe.MatchString(doc) {
		return ErrNotReadOnly
	}
	return nil
}

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type gqlError struct {
	Message    string         `json:"message"`
	Extensions map[string]any `json:"extensions"`
}

type gqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []gqlError      `json:"errors"`
}

// APIError is a GraphQL or HTTP error from Linear. Its text is Linear's
// message and status only (no request header is ever part of it).
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("linear api: status %d: %s", e.Status, e.Message)
}

// Query runs a read-only GraphQL document and decodes its data into out.
// It retries a rate limit (HTTP 429 or a RATELIMITED error) and a server
// error with back-off, honouring Linear's reset headers.
func (c *Client) Query(ctx context.Context, doc string, vars map[string]any, out any) error {
	if err := CheckReadOnly(doc); err != nil {
		return err
	}
	body, err := json.Marshal(gqlRequest{Query: doc, Variables: vars})
	if err != nil {
		return fmt.Errorf("encode query: %w", err)
	}
	backoff := 2 * time.Second
	for attempt := 0; ; attempt++ {
		if wait := time.Until(c.waitUntil); wait > 0 {
			c.log.Info("linear rate limit: waiting for the reset", "wait", wait.Round(time.Second).String())
			if err := c.sleep(ctx, min(wait, c.MaxWait)); err != nil {
				return err
			}
		}
		data, retryAfter, err := c.do(ctx, body)
		if err == nil {
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("decode linear response: %w", err)
			}
			return nil
		}
		var re *retryable
		if !errors.As(err, &re) || attempt >= c.MaxRetries {
			if errors.As(err, &re) {
				return re.err
			}
			return err
		}
		wait := backoff + time.Duration(rand.Int64N(int64(time.Second)))
		if retryAfter > 0 {
			wait = retryAfter
		}
		wait = min(wait, c.MaxWait)
		c.log.Warn("linear request failed, retrying", "attempt", attempt+1, "wait", wait.Round(time.Second).String(),
			"reason", re.err.Error())
		if err := c.sleep(ctx, wait); err != nil {
			return err
		}
		backoff = min(backoff*2, 2*time.Minute)
	}
}

type retryable struct{ err error }

func (r *retryable) Error() string { return r.err.Error() }

// do sends one request. A retryable failure is wrapped in *retryable, with
// the server's suggested wait when it gave one.
func (c *Client) do(ctx context.Context, body []byte) (json.RawMessage, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.key)
	c.requests++
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		// A transport error (reset, timeout). url.Error names the URL, never
		// a header, so the message is safe.
		return nil, 0, &retryable{err: fmt.Errorf("linear request: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, 0, &retryable{err: fmt.Errorf("read linear response: %w", err)}
	}
	c.noteRateLimit(resp.Header)
	retryAfter := retryAfterOf(resp.Header, time.Now())

	var g gqlResponse
	_ = json.Unmarshal(raw, &g) // a non-JSON body is handled by the status checks below
	if resp.StatusCode == http.StatusTooManyRequests || isRateLimited(g.Errors) {
		return nil, retryAfter, &retryable{err: &APIError{Status: resp.StatusCode, Message: "rate limited"}}
	}
	if resp.StatusCode >= 500 {
		return nil, retryAfter, &retryable{err: &APIError{Status: resp.StatusCode, Message: http.StatusText(resp.StatusCode)}}
	}
	if resp.StatusCode != http.StatusOK {
		msg := http.StatusText(resp.StatusCode)
		if len(g.Errors) > 0 {
			msg = g.Errors[0].Message
		}
		return nil, 0, &APIError{Status: resp.StatusCode, Message: msg}
	}
	if len(g.Errors) > 0 {
		msgs := make([]string, 0, len(g.Errors))
		for _, e := range g.Errors {
			msgs = append(msgs, e.Message)
		}
		return nil, 0, &APIError{Status: resp.StatusCode, Message: strings.Join(msgs, "; ")}
	}
	return g.Data, 0, nil
}

func isRateLimited(errs []gqlError) bool {
	for _, e := range errs {
		if code, _ := e.Extensions["code"].(string); strings.EqualFold(code, "RATELIMITED") {
			return true
		}
	}
	return false
}

// noteRateLimit reads Linear's request budget headers. When the budget is
// nearly gone, the next request waits for the reset instead of hitting 429.
func (c *Client) noteRateLimit(h http.Header) {
	rem, err := strconv.Atoi(h.Get("X-RateLimit-Requests-Remaining"))
	if err != nil || rem > c.MinRemaining {
		return
	}
	if reset := resetOf(h.Get("X-RateLimit-Requests-Reset")); !reset.IsZero() {
		c.waitUntil = reset
	}
}

// resetOf reads a reset header: Linear sends epoch milliseconds.
func resetOf(v string) time.Time {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	if n < 1e12 { // seconds, not milliseconds
		return time.Unix(n, 0)
	}
	return time.UnixMilli(n)
}

// retryAfterOf is how long the server asked us to wait: Retry-After
// (seconds), else the requests reset header. 0 when it said nothing.
func retryAfterOf(h http.Header, now time.Time) time.Duration {
	if s, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After"))); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	if reset := resetOf(h.Get("X-RateLimit-Requests-Reset")); !reset.IsZero() && reset.After(now) {
		return reset.Sub(now)
	}
	return 0
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
