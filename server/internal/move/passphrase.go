package move

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
)

// passphraseBytes is the entropy of a generated passphrase: 160 bits.
const passphraseBytes = 20

// minPassphraseLen is the shortest passphrase Export takes. A generated one is
// 39 characters (32 base32 characters in 8 groups of 4, with 7 dashes).
const minPassphraseLen = 32

// GeneratePassphrase returns a fresh passphrase of 160 random bits (crypto/rand),
// as eight groups of four base32 characters: ABCD-EFGH-... It is easy to read
// out and to type. The caller shows it once and never logs or stores it.
func GeneratePassphrase() (string, error) {
	b := make([]byte, passphraseBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("move: read random: %w", err)
	}
	s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b) // 32 characters
	var sb strings.Builder
	for i := 0; i < len(s); i += 4 {
		if i > 0 {
			sb.WriteByte('-')
		}
		sb.WriteString(s[i : i+4])
	}
	return sb.String(), nil
}

// NormalizePassphrase makes a typed passphrase comparable: trims space and a
// trailing newline, and upper-cases a generated one (base32 is upper-case).
// Import applies it, so a pasted or lower-cased value still opens the bundle.
func NormalizePassphrase(s string) string {
	s = strings.TrimSpace(s)
	if len(s) == 39 && strings.Count(s, "-") == 7 {
		return strings.ToUpper(s)
	}
	return s
}
