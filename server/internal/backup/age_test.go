package backup

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

func keyPair(t *testing.T) (string, string) {
	t.Helper()
	s, p, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func TestAgeRoundTrip(t *testing.T) {
	secret, public := keyPair(t)
	secret2, public2 := keyPair(t)
	for _, size := range []int{0, 1, 65535, 65536, 65537, 3 * 65536, 200_000} {
		plain := make([]byte, size)
		rand.Read(plain)
		var enc bytes.Buffer
		res, err := Encrypt(&enc, bytes.NewReader(plain), []string{public, public2})
		if err != nil {
			t.Fatal(err)
		}
		if res.PlainSHA256 != sum(plain) || res.PlainBytes != int64(size) ||
			res.CipherSHA256 != sum(enc.Bytes()) || res.CipherBytes != int64(enc.Len()) {
			t.Errorf("size %d: result %+v", size, res)
		}
		if bytes.Contains(enc.Bytes(), plain[:min(size, 32)]) && size >= 32 {
			t.Error("plaintext visible in the ciphertext")
		}
		for _, sk := range []string{secret, secret2} { // either recipient opens it
			var out bytes.Buffer
			n, err := Decrypt(&out, bytes.NewReader(enc.Bytes()), sk, 0)
			if err != nil || n != int64(size) || !bytes.Equal(out.Bytes(), plain) {
				t.Errorf("size %d: decrypt %v n=%d", size, err, n)
			}
		}
	}
}

func TestAgeTwoEncryptionsDiffer(t *testing.T) {
	_, public := keyPair(t)
	var a, b bytes.Buffer
	Encrypt(&a, strings.NewReader("same"), []string{public})
	Encrypt(&b, strings.NewReader("same"), []string{public})
	if bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Error("two encryptions are identical (the retry rule needs the SAME file, so it is kept, not redone)")
	}
}

func TestAgeHostile(t *testing.T) {
	secret, public := keyPair(t)
	wrong, _ := keyPair(t)
	plain := make([]byte, 3*65536+123)
	rand.Read(plain)
	var enc bytes.Buffer
	if _, err := Encrypt(&enc, bytes.NewReader(plain), []string{public}); err != nil {
		t.Fatal(err)
	}
	good := enc.Bytes()
	flip := func(i int) []byte { b := append([]byte(nil), good...); b[i] ^= 1; return b }
	for _, tc := range []struct {
		name string
		data []byte
		key  string
	}{
		{"wrong key", good, wrong},
		{"empty", nil, secret},
		{"not age", []byte("hello world"), secret},
		{"truncated header", good[:20], secret},
		{"truncated at a chunk boundary", good[:len(good)-(len(plain)%65536+16)], secret},
		{"truncated last byte", good[:len(good)-1], secret},
		{"truncated mid chunk", good[:len(good)/2], secret},
		{"flipped header byte", flip(10), secret},
		{"flipped first body byte", flip(len(good) - len(plain) - 3*16 - 10), secret},
		{"flipped middle", flip(len(good) / 2), secret},
		{"flipped last byte", flip(len(good) - 1), secret},
		{"trailing garbage", append(append([]byte(nil), good...), 'x'), secret},
	} {
		var out bytes.Buffer
		_, err := Decrypt(&out, bytes.NewReader(tc.data), tc.key, 0)
		if err == nil {
			t.Errorf("%s: decrypted", tc.name)
		}
		if err != nil && !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
	}
	// Output limit: exactly the size passes, one less is ErrTooLarge.
	if _, err := Decrypt(io.Discard, bytes.NewReader(good), secret, int64(len(plain))); err != nil {
		t.Errorf("at the limit: %v", err)
	}
	if _, err := Decrypt(io.Discard, bytes.NewReader(good), secret, int64(len(plain))-1); !errors.Is(err, ErrTooLarge) {
		t.Errorf("above the limit: %v", err)
	}
	for _, bad := range []string{"", "age1xyz", "AGE-SECRET-KEY-1BAD", public} {
		if _, err := Decrypt(io.Discard, bytes.NewReader(good), bad, 0); !errors.Is(err, ErrBadIdentity) {
			t.Errorf("identity %q: %v", bad, err)
		}
	}
}

func TestAgeRecipients(t *testing.T) {
	_, pub := keyPair(t)
	scrypt, _ := age.NewScryptRecipient("pass")
	_ = scrypt
	var many []string
	for i := 0; i < MaxRecipients+1; i++ {
		_, p := keyPair(t)
		many = append(many, p)
	}
	for _, tc := range []struct {
		name string
		keys []string
		want error
	}{
		{"none", nil, ErrNoRecipients},
		{"five", many[:5], nil},
		{"six", many, ErrTooManyRecipient},
		{"one", []string{pub}, nil},
		{"padded", []string{" " + pub + "\n"}, nil},
		{"ssh key", []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGq2 x"}, ErrBadRecipient},
		{"ssh rsa", []string{"ssh-rsa AAAA"}, ErrBadRecipient},
		{"plugin", []string{"age1yubikey1qtest"}, ErrBadRecipient},
		{"private key as recipient", []string{"AGE-SECRET-KEY-1QQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQ"}, ErrBadRecipient},
		{"empty string", []string{""}, ErrBadRecipient},
		{"bad checksum", []string{pub[:len(pub)-1] + otherChar(pub[len(pub)-1])}, ErrBadRecipient},
		{"one bad of two", []string{pub, "age1bad"}, ErrBadRecipient},
	} {
		_, err := ParseRecipients(tc.keys)
		if !errors.Is(err, tc.want) && !(tc.want == nil && err == nil) {
			t.Errorf("%s: %v", tc.name, err)
		}
		if err != nil && len(tc.keys) > 0 && len(tc.keys[0]) > 12 && strings.Contains(err.Error(), strings.TrimSpace(tc.keys[0])[:12]) {
			t.Errorf("%s: error echoes the key: %v", tc.name, err)
		}
	}
	var out bytes.Buffer
	if _, err := Encrypt(&out, strings.NewReader("x"), nil); !errors.Is(err, ErrNoRecipients) || out.Len() != 0 {
		t.Errorf("Encrypt with no recipient: %v, wrote %d bytes", err, out.Len())
	}
}

func TestFingerprint(t *testing.T) {
	const k = "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p"
	full := Fingerprint(k)
	want := sha256.Sum256([]byte(k))
	if full != hex.EncodeToString(want[:]) || len(full) != 64 || ShortFingerprint(k) != full[:12] {
		t.Error("fingerprint")
	}
}

type failWriter struct{ left int }

func (f *failWriter) Write(p []byte) (int, error) {
	if len(p) > f.left {
		return 0, errors.New("disk full")
	}
	f.left -= len(p)
	return len(p), nil
}

func TestEncryptWriteFailure(t *testing.T) {
	_, pub := keyPair(t)
	if _, err := Encrypt(&failWriter{left: 100}, bytes.NewReader(make([]byte, 300000)), []string{pub}); err == nil {
		t.Error("write failure not reported")
	}
}

// The age command, where present, opens what Encrypt wrote (the format is age's own).
func TestAgeCommandInterop(t *testing.T) {
	bin, err := exec.LookPath("age")
	if err != nil {
		t.Skip("age command not installed")
	}
	secret, public := keyPair(t)
	dir := t.TempDir()
	plain := make([]byte, 150000)
	rand.Read(plain)
	var enc bytes.Buffer
	Encrypt(&enc, bytes.NewReader(plain), []string{public})
	encPath, keyPath := filepath.Join(dir, "x.age"), filepath.Join(dir, "key")
	os.WriteFile(encPath, enc.Bytes(), 0o600)
	os.WriteFile(keyPath, []byte(secret+"\n"), 0o600)
	out, err := exec.Command(bin, "-d", "-i", keyPath, encPath).Output()
	if err != nil || !bytes.Equal(out, plain) {
		t.Errorf("age -d: %v", err)
	}
	// And the other way: the command encrypts, Decrypt opens.
	cmd := exec.Command(bin, "-r", public)
	cmd.Stdin = bytes.NewReader(plain)
	cenc, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var back bytes.Buffer
	if _, err := Decrypt(&back, bytes.NewReader(cenc), secret, 0); err != nil || !bytes.Equal(back.Bytes(), plain) {
		t.Errorf("Decrypt of age output: %v", err)
	}
}

// otherChar returns a bech32 character that differs from c, so the changed key
// always has a bad checksum (a fixed replacement is the real last char 1 time in 32).
func otherChar(c byte) string {
	if c == 'q' {
		return "p"
	}
	return "q"
}
