package notify

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"devops/internal/platform"
)

func TestParsePushRelayURL(t *testing.T) {
	good := []string{
		"https://relay.example.com", "https://relay.example.com/", "https://relay.example.com/base/path",
		"https://relay.example.com:8443/x", "https://203.0.113.9/relay", "https://[2001:db8::1]/relay",
	}
	for _, u := range good {
		if _, err := ParsePushRelayURL(u); err != nil {
			t.Errorf("%q refused: %v", u, err)
		}
	}
	bad := map[string]string{
		"http":      "http://relay.example.com/",
		"no scheme": "relay.example.com",
		"ftp":       "ftp://relay.example.com/",
		"user info": "https://user:pass@relay.example.com/",
		"user only": "https://user@relay.example.com/",
		"query":     "https://relay.example.com/?a=1",
		"empty q":   "https://relay.example.com/?",
		"fragment":  "https://relay.example.com/#x",
		"no host":   "https:///path",
		"opaque":    "https:relay.example.com",
		"space":     "https://relay.example.com/a b",
		"control":   "https://relay.example.com/a\x01",
		"encoded":   "https://relay.example.com/a%2Fb",
		"empty":     "",
		"too long":  "https://relay.example.com/" + strings.Repeat("a", MaxRelayURLBytes),
		"bad utf8":  "https://relay.example.com/\xff",
	}
	for name, u := range bad {
		_, err := ParsePushRelayURL(u)
		if err == nil {
			t.Errorf("%s: %q accepted", name, u)
			continue
		}
		if u != "" && len(u) > 8 && strings.Contains(err.Error(), u) {
			t.Errorf("%s: the error repeats the URL", name)
		}
	}
}

func TestValidRelaySecret(t *testing.T) {
	for _, s := range []string{"a", "Abc-123_x.y~z", strings.Repeat("s", MaxRelaySecretBytes)} {
		if err := ValidRelaySecret(s); err != nil {
			t.Errorf("%q refused: %v", s, err)
		}
	}
	for _, s := range []string{"", "a b", "tab\t", "é", "nl\n", strings.Repeat("s", MaxRelaySecretBytes+1)} {
		if err := ValidRelaySecret(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestPushError_IsAFixedWord(t *testing.T) {
	for _, w := range []string{PushRejected, PushUnauthorized, PushRateLimited, PushUnavailable, PushUnreachable, PushBlocked, PushRedirect, PushUnreadable} {
		e := &PushError{Word: w}
		if e.Error() != w || !strings.HasPrefix(w, "push_") {
			t.Errorf("%q", w)
		}
	}
	permanent := map[string]bool{PushRejected: true, PushUnauthorized: true, PushBlocked: true, PushRedirect: true, PushTokenInvalid: true}
	for _, w := range []string{PushRejected, PushUnauthorized, PushRateLimited, PushUnavailable, PushUnreachable, PushBlocked, PushRedirect, PushUnreadable, PushTokenInvalid} {
		if got := (&PushError{Word: w}).permanent(); got != permanent[w] {
			t.Errorf("%s permanent = %v", w, got)
		}
	}
}

// The push keyring is a fourth scope: a blob of another scope never opens, and
// a blob is bound to its org and its column.
func TestPushKeyring_BoundAndSeparate(t *testing.T) {
	master := []byte("0123456789abcdef0123456789abcdef")
	k, err := platform.NewPushKeyring(master)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := platform.NewPushKeyring([]byte("short")); err == nil {
		t.Error("a short key was accepted")
	}
	org, other := uuid.NewString(), uuid.NewString()
	blob := k.SealFor(platform.PushColRelaySecret, org, []byte("secret"))
	if got, err := k.OpenFor(platform.PushColRelaySecret, org, blob); err != nil || string(got) != "secret" {
		t.Fatalf("open: %q %v", got, err)
	}
	if _, err := k.OpenFor(platform.PushColRelaySecret, other, blob); err == nil {
		t.Error("opens for another org")
	}
	if _, err := k.OpenFor(platform.PushColRelayURL, org, blob); err == nil {
		t.Error("opens as another column")
	}
	if string(blob) == string(k.SealFor(platform.PushColRelaySecret, org, []byte("secret"))) {
		t.Error("two seals are equal (the nonce is not random)")
	}
	s, _ := platform.NewSlackKeyring(master)
	g, _ := platform.NewGitKeyring(master)
	if _, err := k.OpenFor("webhook_enc", org, s.SealFor("webhook_enc", org, []byte("x"))); err == nil {
		t.Error("a Slack blob opens as push")
	}
	if _, err := s.OpenFor(platform.PushColRelaySecret, org, blob); err == nil {
		t.Error("a push blob opens as Slack")
	}
	if _, err := g.OpenFor(platform.PushColRelaySecret, org, blob); err == nil {
		t.Error("a push blob opens as git")
	}
	k2, _ := platform.NewPushKeyring([]byte("ffffffffffffffffffffffffffffffff"))
	if _, err := k2.OpenFor(platform.PushColRelaySecret, org, blob); err == nil {
		t.Error("another master key opens it")
	}
}

func TestDeviceScopeAndCheck(t *testing.T) {
	o, d := uuid.New(), uuid.New()
	if got := deviceScope(o, d); got != o.String()+"."+d.String() {
		t.Errorf("scope %q", got)
	}
	if err := checkDevice(DeviceInput{Platform: "ios", Token: "abc", Label: "Anna's iPhone"}); err != nil {
		t.Error(err)
	}
	if err := checkDevice(DeviceInput{Platform: "ios", Token: "abc", Label: "éè中"}); err != nil {
		t.Errorf("a non-ASCII label: %v", err)
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A transport error holds the URL (a *url.Error). The poster maps it to a fixed
// word and drops it: no URL, host, secret or token in the error it returns.
func TestPushPosterTransportErrorHoldsNoValue(t *testing.T) {
	const raw, secret, tok = "https://relay.example.com/t/URLPATHTOKEN", "SECRETVALUE-1", "DEVICETOKEN-1"
	c := &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Post", URL: r.URL.String(), Err: errors.New("connection reset")}
	})}
	err := NewPushPoster(c).Wake(context.Background(), raw, secret, PushWake{Token: tok, Platform: "ios", ID: "x"})
	var pe *PushError
	if !errors.As(err, &pe) || pe.Word != PushUnreachable {
		t.Fatalf("Wake = %v, want push_unreachable", err)
	}
	for _, v := range []string{"relay.example.com", "URLPATHTOKEN", secret, tok} {
		if strings.Contains(err.Error(), v) {
			t.Errorf("the error holds %q", v)
		}
	}
}

func TestCheckRelayHostLiteral(t *testing.T) {
	for raw, wantErr := range map[string]bool{
		"https://relay.example.com": false, "https://8.8.8.8": false, "https://[2606:4700:4700::1111]/x": false,
		"https://127.0.0.1": true, "https://10.0.0.1:8443": true, "https://169.254.169.254": true, "https://[::1]": true,
		"https://[::ffff:192.168.1.1]": true, "https://100.64.0.1": true,
		"https://localhost": true, "https://localhost./x": true, "https://api.localhost": true, "https://LOCALHOST:8443": true,
		"https://intranet": true, "https://postgres": true, "https://127.1": true, "https://2130706433": true,
		"https://0x7f.1": true, "https://0177.0.0.1": true, "https://0x7f000001": true, "https://10.1": true, "https://134744072": true,
		"https://relay.example.co": false, "https://a.b.c.example.com": false,
	} {
		u, err := ParsePushRelayURL(raw)
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if got := CheckRelayHostLiteral(u) != nil; got != wantErr {
			t.Errorf("%s refused = %v, want %v", raw, got, wantErr)
		}
	}
}
