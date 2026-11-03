package login

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"math/big"
)

// MinPepperLen is the least length of AUTH_OTP_PEPPER, in bytes.
const MinPepperLen = 32

// CodeLen is the number of digits in a login code.
const CodeLen = 6

// saltLen is the per-row salt (login_codes_salt_len).
const saltLen = 16

var codeSpace = big.NewInt(1_000_000)

// newCode draws a uniform 6-digit code from crypto/rand (golang-security:
// never math/rand for a secret).
func newCode() (string, error) {
	n, err := rand.Int(rand.Reader, codeSpace)
	if err != nil {
		return "", fmt.Errorf("draw login code: %w", err)
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// newSalt returns a fresh random salt.
func newSalt() []byte {
	b := make([]byte, saltLen)
	_, _ = rand.Read(b) // crypto/rand.Read never fails (Go 1.24+)
	return b
}

// hashCode is the stored form of a code: HMAC-SHA256(pepper, salt || code).
// The pepper (AUTH_OTP_PEPPER) lives only in the server's environment, never
// in the database. A plain hash would not do: a 6-digit code has only 10^6
// values, so someone who can READ the database could ask for a code, read
// the salt and hash, and try every value in well under a second. Without the
// pepper the stored hash says nothing about the code.
func hashCode(pepper, salt []byte, code string) []byte {
	m := hmac.New(sha256.New, pepper)
	m.Write(salt)
	m.Write([]byte(code))
	return m.Sum(nil)
}

// codeMatches compares in constant time.
func codeMatches(pepper, salt, stored []byte, code string) bool {
	return subtle.ConstantTimeCompare(hashCode(pepper, salt, code), stored) == 1
}

// ValidCodeFormat reports whether s is exactly six ASCII digits.
func ValidCodeFormat(s string) bool {
	if len(s) != CodeLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// The exported forms of the code helpers, for the other one-time codes that
// use the same rules (the sign-up email confirm code). They share
// the pepper, the salt length and the constant-time compare with login.

// NewCode draws a uniform 6-digit code from crypto/rand.
func NewCode() (string, error) { return newCode() }

// NewSalt returns a fresh 16-byte random salt.
func NewSalt() []byte { return newSalt() }

// HashCode is HMAC-SHA256(pepper, salt || code), the stored form of a code.
func HashCode(pepper, salt []byte, code string) []byte { return hashCode(pepper, salt, code) }

// CodeMatches compares code with a stored hash in constant time.
func CodeMatches(pepper, salt, stored []byte, code string) bool {
	return codeMatches(pepper, salt, stored, code)
}
