// Package auth implements token authentication and the per-org middleware
// chain: bearer token -> user -> RequireOrgMember({org}) -> token org pin ->
// RequireScope per route.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"strings"
)

// Token format: "odt_" + 43 base62 characters (about 256 bits of entropy).
const (
	TokenPrefix      = "odt_"
	tokenBodyLen     = 43
	TokenLen         = len(TokenPrefix) + tokenBodyLen
	DisplayPrefixLen = 12 // stored in api_tokens.prefix for listing
	base62Alphabet   = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// Kinds of token. Session tokens arrive with the web login.
// KindNative (LAUN-129) is the token of a native app's login: a bearer
// credential like a PAT, made by the same login as a session. The web
// cookie carries KindSession only; the bearer header carries KindPAT and
// KindNative only, so a cookie value never works as a bearer token.
const (
	KindPAT     = "pat"
	KindSession = "session"
	KindNative  = "native"
)

// IsLoginKind reports whether kind is made by a login (a web session or a
// native token) and not minted as a PAT. The session rules (admin scope,
// no admin token minted, mint rate limit, logout) follow it.
func IsLoginKind(kind string) bool { return kind == KindSession || kind == KindNative }

// NewToken is a freshly minted secret. Plaintext is shown to the caller once
// and never stored; Hash and Prefix go to the database.
type NewToken struct {
	Plaintext string
	Hash      []byte
	Prefix    string
}

// GenerateToken mints a token from crypto/rand (golang-security: never
// math/rand for secrets). Bytes >= 248 are rejected so every base62 digit is
// uniform (248 = 4 * 62).
func GenerateToken() (NewToken, error) {
	var sb strings.Builder
	sb.Grow(TokenLen)
	sb.WriteString(TokenPrefix)
	buf := make([]byte, 64)
	for sb.Len() < TokenLen {
		if _, err := rand.Read(buf); err != nil {
			return NewToken{}, fmt.Errorf("read random: %w", err)
		}
		for _, b := range buf {
			if b >= 248 {
				continue
			}
			sb.WriteByte(base62Alphabet[b%62])
			if sb.Len() == TokenLen {
				break
			}
		}
	}
	plain := sb.String()
	return NewToken{Plaintext: plain, Hash: HashToken(plain), Prefix: plain[:DisplayPrefixLen]}, nil
}

// HashToken is the stored form of a token.
func HashToken(plain string) []byte {
	sum := sha256.Sum256([]byte(plain))
	return sum[:]
}

// WellFormed reports whether s has the token shape. It runs before any
// database lookup so junk input never reaches the query.
func WellFormed(s string) bool {
	if len(s) != TokenLen || !strings.HasPrefix(s, TokenPrefix) {
		return false
	}
	for i := len(TokenPrefix); i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') {
			return false
		}
	}
	return true
}
