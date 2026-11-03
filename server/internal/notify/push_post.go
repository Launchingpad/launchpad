package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"devops/internal/netguard"
)

// Mobile push (LAUN-197, docs/internal/decisions-T35-mobile-push.md). This file
// is the one place that talks to a push relay: the relay URL check and the
// POST. The POST goes through a *http.Client built by netguard.NewClient (the
// dial-time guard, no redirect, no proxy). Nothing here logs or returns the
// relay URL, the relay secret or a device token: an error is a fixed word.

// MaxRelayURLBytes bounds the relay URL an admin may enter.
const MaxRelayURLBytes = 512

// MaxRelaySecretBytes bounds the relay bearer secret.
const MaxRelaySecretBytes = 512

// PushPostTimeout bounds one request to the relay.
const PushPostTimeout = 10 * time.Second

// The fixed words of a failed push (the CHECK of push_relays.last_error and
// push_outbox.last_error). They hold no part of the relay's reply.
const (
	PushRejected     = "push_rejected"      // 4xx not named below: the relay refused the request
	PushUnauthorized = "push_unauthorized"  // 401, 403: the relay refused the bearer secret
	PushTokenInvalid = "push_token_invalid" // 410: the device token is no longer valid (not stored as a row error)
	PushRateLimited  = "push_rate_limited"  // 429
	PushUnavailable  = "push_unavailable"   // 5xx
	PushUnreachable  = "push_unreachable"   // the connection or the TLS check failed
	PushBlocked      = "push_blocked"       // the dial guard or the URL check refused the host
	PushRedirect     = "push_redirect"      // a 3xx: never followed
	PushUnreadable   = "push_unreadable"    // a stored blob does not open with this server's key
)

// PushError is a failed request. Error is the fixed word and nothing else, so
// the error is safe in a log line and in a response.
type PushError struct {
	Word       string
	RetryAfter time.Duration // set for push_rate_limited when the relay said how long
}

func (e *PushError) Error() string { return e.Word }

// permanent reports whether a retry cannot help until the admin acts.
func (e *PushError) permanent() bool {
	switch e.Word {
	case PushRejected, PushUnauthorized, PushBlocked, PushRedirect, PushTokenInvalid:
		return true
	}
	return false
}

// ParsePushRelayURL checks a relay base URL and returns it. The error names
// the problem and never the value. The rules: https only, a host, no user info,
// no query or fragment, no encoded path. Unlike the Slack webhook the host
// cannot be on an allowlist: the admin enters it. The dial-time guard
// (public addresses only, checked on the resolved address) is the control.
func ParsePushRelayURL(raw string) (*url.URL, error) {
	if raw == "" || len(raw) > MaxRelayURLBytes || !utf8.ValidString(raw) {
		return nil, errors.New("must be an https URL of at most 512 bytes")
	}
	for _, r := range raw {
		if r <= ' ' || r == 0x7f {
			return nil, errors.New("must not contain spaces or control characters")
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("must be a valid URL")
	}
	switch {
	case u.Scheme != "https":
		return nil, errors.New("must start with https://")
	case u.User != nil:
		return nil, errors.New("must not hold a user name or password")
	case u.Hostname() == "":
		return nil, errors.New("must name a host")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "":
		return nil, errors.New("must not have a query or fragment")
	case u.RawPath != "":
		return nil, errors.New("must not have an encoded path")
	}
	return u, nil
}

// CheckRelayHostLiteral refuses, at save time, a relay host that is an IP
// literal in a range the dial guard refuses (private, loopback, link-local,
// metadata, ...), or a service name the guard refuses. It uses the same policy
// as the dial, so there is no second range list. A name that resolves to a
// private address passes here and is refused at dial time (push_blocked).
func CheckRelayHostLiteral(u *url.URL) error {
	pol := PushPolicy(nil)
	h := u.Hostname()
	if a, err := netip.ParseAddr(h); err == nil {
		if pol.Check(a.Unmap()) != nil {
			return errors.New("must not be a private, loopback or link-local address")
		}
		return nil
	}
	if _, ok := netguard.LegacyIPv4(h); ok {
		return errors.New("must not be a numeric address in an old spelling; write a normal host name")
	}
	if netguard.IsLocalName(h) || pol.RefusesName(h) {
		return errors.New("must not be a local or internal host name")
	}
	return nil
}

// relayHost is the host name alone, lower case, for the admin page.
func relayHost(u *url.URL) string { return strings.ToLower(u.Hostname()) }

// ValidRelaySecret checks the bearer secret: printable ASCII without spaces.
func ValidRelaySecret(s string) error {
	if s == "" || len(s) > MaxRelaySecretBytes {
		return errors.New("must be 1 to 512 characters")
	}
	for _, r := range s {
		if r <= ' ' || r >= 0x7f {
			return errors.New("must be printable ASCII without spaces")
		}
	}
	return nil
}

// PushWake is the whole request body of one push: no text of an issue, no
// identifier, no title, no name. The app wakes and pulls the data with its own
// token (decisions-T22 #47).
type PushWake struct {
	Token    string // the device token (a credential-like identifier)
	Platform string // ios or android
	ID       string // an opaque outbox id
}

// PushPoster posts to a relay. Build it with NewPushPoster.
type PushPoster struct {
	client *http.Client
}

// NewPushPoster returns a poster on the guarded client. client must come from
// netguard.NewClient. A nil client posts nothing and returns push_blocked.
func NewPushPoster(client *http.Client) *PushPoster { return &PushPoster{client: client} }

// PushPolicy is the dial policy of the relay client: public addresses only
// (the zero Policy), the service names the git policy refuses, and the app's
// own subnets when dyn is given. No setting opens a private range here.
func PushPolicy(dyn netguard.Dynamic) netguard.Policy {
	return netguard.Policy{Dynamic: dyn, Names: append([]string(nil), netguard.GitServiceNames...)}
}

// NewPushClient builds the guarded client of the server.
func NewPushClient(dyn netguard.Dynamic) *http.Client {
	return NewPushClientFor(&netguard.Dialer{Policy: PushPolicy(dyn)})
}

// NewPushClientFor builds the client on a given dialer: netguard.NewClient (no
// redirect, no proxy, TLS 1.2 or newer). Tests pass a dialer with a fake resolver.
func NewPushClientFor(d *netguard.Dialer) *http.Client {
	return netguard.NewClient(d, PushPostTimeout)
}

// pushBody is the JSON the relay gets.
type pushBody struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Platform string `json:"platform"`
	Token    string `json:"token"`
}

// Wake sends one wake to the relay: POST <base>/v1/wake with the bearer
// secret. relayURL is checked again here. The returned error is nil or a
// *PushError.
func (p *PushPoster) Wake(ctx context.Context, relayURL, secret string, w PushWake) error {
	if p == nil || p.client == nil {
		return &PushError{Word: PushBlocked}
	}
	u, err := ParsePushRelayURL(relayURL)
	if err != nil {
		return &PushError{Word: PushBlocked}
	}
	target := *u
	target.Path = strings.TrimRight(u.Path, "/") + "/v1/wake"
	body, err := json.Marshal(pushBody{Kind: "wake", ID: w.ID, Platform: w.Platform, Token: w.Token})
	if err != nil {
		return &PushError{Word: PushRejected}
	}
	ctx, cancel := context.WithTimeout(ctx, PushPostTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return &PushError{Word: PushBlocked}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+secret)
	resp, err := p.client.Do(req)
	if err != nil {
		// err is a *url.Error that holds the URL: map it to a word, drop it.
		if errors.Is(err, netguard.ErrRefused) {
			return &PushError{Word: PushBlocked}
		}
		return &PushError{Word: PushUnreachable}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch c := resp.StatusCode; {
	case c == http.StatusOK || c == http.StatusAccepted || c == http.StatusNoContent:
		return nil
	case c >= 300 && c < 400:
		return &PushError{Word: PushRedirect}
	case c == http.StatusUnauthorized || c == http.StatusForbidden:
		return &PushError{Word: PushUnauthorized}
	case c == http.StatusGone:
		return &PushError{Word: PushTokenInvalid}
	case c == http.StatusTooManyRequests:
		return &PushError{Word: PushRateLimited, RetryAfter: retryAfter(resp.Header.Get("Retry-After"))}
	case c >= 400 && c < 500:
		return &PushError{Word: PushRejected}
	default:
		return &PushError{Word: PushUnavailable}
	}
}
