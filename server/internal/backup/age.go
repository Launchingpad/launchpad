package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"

	"filippo.io/age"
)

// MaxRecipients is the most live public keys the install allows (T16 #19).
const MaxRecipients = 5

// Errors of this file. They hold no key text.
var (
	ErrNoRecipients     = errors.New("backup: no recipient")
	ErrTooManyRecipient = errors.New("backup: too many recipients")
	ErrBadRecipient     = errors.New("backup: not an X25519 age public key (age1...)")
	ErrBadIdentity      = errors.New("backup: not an age private key (AGE-SECRET-KEY-1...)")
	ErrDecrypt          = errors.New("backup: cannot decrypt (wrong key, or the data is damaged)")
	ErrTooLarge         = errors.New("backup: data above the limit")
)

// ParseRecipient accepts an X25519 recipient only (`age1...`, T16 #17). The
// scrypt, SSH and plugin forms are refused: they need a passphrase on the host
// or add parsers.
func ParseRecipient(s string) (*age.X25519Recipient, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "age1") {
		return nil, ErrBadRecipient
	}
	r, err := age.ParseX25519Recipient(s)
	if err != nil {
		return nil, ErrBadRecipient
	}
	return r, nil
}

// ParseRecipients parses 1 to MaxRecipients keys.
func ParseRecipients(keys []string) ([]age.Recipient, error) {
	if len(keys) == 0 {
		return nil, ErrNoRecipients
	}
	if len(keys) > MaxRecipients {
		return nil, ErrTooManyRecipient
	}
	out := make([]age.Recipient, 0, len(keys))
	for _, k := range keys {
		r, err := ParseRecipient(k)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// Fingerprint is the FULL sha256 (64 hex) of the `age1...` string: the route key
// and command argument of T16 #20. The page shows ShortFingerprint.
func Fingerprint(recipient string) string {
	sum := sha256.Sum256([]byte(recipient))
	return hex.EncodeToString(sum[:])
}

// ShortFingerprint is the first 12 hex characters.
func ShortFingerprint(recipient string) string { return Fingerprint(recipient)[:12] }

// GenerateKeyPair makes an age X25519 pair. It is for keygen and tests; the
// private key must be printed once by the caller and never logged or stored.
func GenerateKeyPair() (secret, public string, err error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", "", err
	}
	return id.String(), id.Recipient().String(), nil
}

// EncryptResult describes one encryption pass.
type EncryptResult struct {
	PlainSHA256  string // sha256 of the plaintext read (the dump file)
	PlainBytes   int64
	CipherSHA256 string // sha256 of the encrypted bytes written to dst
	CipherBytes  int64
}

type hashCounter struct {
	w io.Writer
	h interface {
		io.Writer
		Sum([]byte) []byte
	}
	n int64
}

func (c *hashCounter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.h.Write(p[:n])
	c.n += int64(n)
	return n, err
}

// Encrypt reads src once and writes the age file to dst, hashing both the
// plaintext and the ciphertext in the same pass (T16 #18, #22). It never
// buffers the stream: memory is one age chunk. The recipients are encrypted
// to as given; the caller passes ACTIVE recipients only.
func Encrypt(dst io.Writer, src io.Reader, recipients []string) (EncryptResult, error) {
	rs, err := ParseRecipients(recipients)
	if err != nil {
		return EncryptResult{}, err
	}
	cipher := &hashCounter{w: dst, h: sha256.New()}
	plain := &hashCounter{w: io.Discard, h: sha256.New()}
	w, err := age.Encrypt(cipher, rs...)
	if err != nil {
		return EncryptResult{}, err
	}
	if _, err := io.Copy(w, io.TeeReader(src, plain)); err != nil {
		return EncryptResult{}, err // the writer is abandoned: dst holds a partial file
	}
	if err := w.Close(); err != nil {
		return EncryptResult{}, err
	}
	return EncryptResult{
		PlainSHA256:  hex.EncodeToString(plain.h.Sum(nil)),
		PlainBytes:   plain.n,
		CipherSHA256: hex.EncodeToString(cipher.h.Sum(nil)),
		CipherBytes:  cipher.n,
	}, nil
}

// Decrypt reads an age file and writes the plaintext to dst, at most
// maxPlain bytes (0: no limit). Any failure, whatever its cause (wrong key,
// truncated, a changed byte), is ErrDecrypt, with one exception: more output
// than maxPlain is ErrTooLarge. age authenticates every 64 KiB chunk, so dst
// may already hold earlier, authentic chunks of a truncated file; the caller
// must discard dst on an error. Truncation at a chunk boundary is detected by
// age's final-chunk flag.
func Decrypt(dst io.Writer, src io.Reader, identity string, maxPlain int64) (int64, error) {
	id, err := age.ParseX25519Identity(strings.TrimSpace(identity))
	if err != nil {
		return 0, ErrBadIdentity
	}
	r, err := age.Decrypt(src, id)
	if err != nil {
		return 0, ErrDecrypt
	}
	var in io.Reader = r
	if maxPlain > 0 {
		in = io.LimitReader(r, maxPlain+1)
	}
	n, err := io.Copy(dst, in)
	if err != nil {
		return n, ErrDecrypt
	}
	if maxPlain > 0 && n > maxPlain {
		return n, ErrTooLarge
	}
	return n, nil
}
