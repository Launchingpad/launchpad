// Package login is the web app's email one-time-code login (T6a):
// it issues 6-digit codes to existing members, checks them, and exchanges a
// good code for a session token (api_tokens kind 'session', or 'native' for a native client). The HTTP layer
// (internal/api) turns a session into the session and CSRF cookies
// (__Host-odt_session / __Host-odt_csrf; odt_session / odt_csrf on a local
// server).
//
// Secrets this package handles (codes, session tokens, CSRF tokens) are
// never logged, never put in an event and never stored in plaintext. The one
// exception is DevLogMailer, which writes a code to the log and can only be
// built for a local or test server.
package login

import (
	"errors"
	"net/mail"
	"strings"
)

// MaxEmailLen is the longest address accepted (RFC 5321 path limit).
const MaxEmailLen = 254

var errBadEmail = errors.New("not a valid email address")

// NormalizeEmail trims and lower-cases an address and checks it is a plain
// addr-spec ("a@b.c", no display name, no comments). Lookups compare
// lower(email), so the case of the stored address does not matter.
func NormalizeEmail(raw string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" || len(s) > MaxEmailLen {
		return "", errBadEmail
	}
	for _, c := range s {
		if c <= ' ' || c == 0x7f {
			return "", errBadEmail
		}
	}
	local, domain, ok := strings.Cut(s, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "@") ||
		!strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return "", errBadEmail
	}
	a, err := mail.ParseAddress(s)
	if err != nil || a.Name != "" || a.Address != s {
		return "", errBadEmail
	}
	return s, nil
}
