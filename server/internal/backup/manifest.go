package backup

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"

	"devops/internal/backup/remotename"
)

// The manifest (T16 #22) sits next to each uploaded object. It names the
// object, its sha256 and size, the dump's UTC time, the schema version, the
// Launchpad version and the install id; it holds no host, URL or secret.
//
// File format (the `.json` file): an envelope of two fields,
//
//	{"payload":"<base64 of the payload JSON>","signature":"<base64 Ed25519>"}
//
// The signature covers SigDomain + the payload BYTES exactly as stored, so no
// canonical-JSON rule is needed. Verification checks the signature first and
// parses the payload only after it.

// SigDomain separates this signature from any other use of the key.
const SigDomain = "launchpad-backup-manifest-v1\n"

// ManifestVersion is the payload version this code writes and accepts.
const ManifestVersion = 1

// MaxManifestBytes is the size above which a manifest file is refused (T16 #39).
const MaxManifestBytes = 1 << 20

// Manifest is the signed payload.
type Manifest struct {
	Version          int    `json:"version"`
	InstallID        string `json:"installId"`
	Object           string `json:"object"`   // the object's file name
	SHA256           string `json:"sha256"`   // of the encrypted object, 64 lower-case hex
	Size             int64  `json:"size"`     // bytes of the encrypted object
	DumpTime         string `json:"dumpTime"` // UTC, remotename.TimeLayout
	SchemaVersion    int    `json:"schemaVersion"`
	LaunchpadVersion string `json:"launchpadVersion"`
}

type envelope struct {
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

// Manifest errors. Each is a fixed text.
var (
	ErrManifestTooLarge  = errors.New("backup: manifest is too large")
	ErrManifestFormat    = errors.New("backup: manifest is malformed")
	ErrManifestSignature = errors.New("backup: manifest signature is not valid")
	ErrManifestBinding   = errors.New("backup: manifest does not belong to this object")
	ErrObjectMismatch    = errors.New("backup: object does not match its manifest")
	ErrBadKey            = errors.New("backup: bad signing key")
)

var (
	sha256Re  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	versionRe = regexp.MustCompile(`^[0-9A-Za-z._+-]{1,64}$`)
)

// GenerateSigningKey makes an Ed25519 pair with crypto/rand. The caller seals
// the private key (platform.BackupKeyring) and never logs it.
func GenerateSigningKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(nil)
}

// EncodePublicKey is the text form of a public key: standard base64 of the 32
// raw bytes. It is what the page shows and `verify --public-key FILE` reads.
func EncodePublicKey(k ed25519.PublicKey) string { return base64.StdEncoding.EncodeToString(k) }

// ParsePublicKey reads the text form (surrounding white space ignored).
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, ErrBadKey
	}
	return ed25519.PublicKey(b), nil
}

// validate checks every field. It is the same check for Sign and Verify, so a
// manifest this code would not accept is never written.
func (m Manifest) validate() error {
	if m.Version != ManifestVersion {
		return ErrManifestFormat
	}
	mt, err := remotename.NewMatcher(m.InstallID)
	if err != nil {
		return ErrManifestFormat
	}
	n, ok := mt.Parse(m.Object)
	if !ok || n.Kind != remotename.KindObject {
		return ErrManifestFormat
	}
	if !sha256Re.MatchString(m.SHA256) || m.Size < 0 || m.SchemaVersion < 0 || !versionRe.MatchString(m.LaunchpadVersion) {
		return ErrManifestFormat
	}
	t, err := time.Parse(remotename.TimeLayout, m.DumpTime)
	if err != nil || !t.Equal(n.Time) {
		return ErrManifestFormat
	}
	return nil
}

// Sign returns the manifest file bytes.
func Sign(priv ed25519.PrivateKey, m Manifest) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, ErrBadKey
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(priv, signedBytes(payload))
	return json.Marshal(envelope{
		Payload:   base64.StdEncoding.EncodeToString(payload),
		Signature: base64.StdEncoding.EncodeToString(sig),
	})
}

func signedBytes(payload []byte) []byte {
	b := make([]byte, 0, len(SigDomain)+len(payload))
	b = append(b, SigDomain...)
	return append(b, payload...)
}

// strictDecode decodes one JSON value, refusing unknown fields and any
// trailing bytes.
func strictDecode(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return ErrManifestFormat
	}
	if _, err := dec.Token(); err != io.EOF {
		return ErrManifestFormat
	}
	return nil
}

// VerifyManifest checks the size cap, the envelope, the signature and then the
// payload, and returns the payload. It proves the manifest was made by the
// holder of the private key. It does NOT prove freshness, and it does not bind
// the manifest to an object: call Bind and CheckObject (T16 #22, #39).
func VerifyManifest(pub ed25519.PublicKey, file []byte) (Manifest, error) {
	if len(pub) != ed25519.PublicKeySize {
		return Manifest{}, ErrBadKey
	}
	if len(file) > MaxManifestBytes {
		return Manifest{}, ErrManifestTooLarge
	}
	var env envelope
	if err := strictDecode(file, &env); err != nil {
		return Manifest{}, err
	}
	payload, err1 := base64.StdEncoding.DecodeString(env.Payload)
	sig, err2 := base64.StdEncoding.DecodeString(env.Signature)
	if err1 != nil || err2 != nil || len(sig) != ed25519.SignatureSize {
		return Manifest{}, ErrManifestFormat
	}
	if !ed25519.Verify(pub, signedBytes(payload), sig) {
		return Manifest{}, ErrManifestSignature
	}
	var m Manifest
	if err := strictDecode(payload, &m); err != nil {
		return Manifest{}, err
	}
	if err := m.validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// Bind fails when the manifest's object name, dump time or install id disagree
// with the stem that was fetched and the destination's install id (T16 #39). A
// renamed, swapped or replayed pair fails here: an old manifest names an old
// stem, and a stem includes its time and 4 random characters.
func (m Manifest) Bind(installID, stem string) error {
	if m.InstallID != installID || m.Object != remotename.ObjectName(stem) {
		return ErrManifestBinding
	}
	mt, err := remotename.NewMatcher(installID)
	if err != nil {
		return ErrManifestBinding
	}
	t, ok := mt.ParseStem(stem)
	if !ok {
		return ErrManifestBinding
	}
	if dt, err := time.Parse(remotename.TimeLayout, m.DumpTime); err != nil || !dt.Equal(t) {
		return ErrManifestBinding
	}
	return nil
}

// CheckObject compares the object's size and sha256 (hex, of the encrypted
// bytes) with the manifest.
func (m Manifest) CheckObject(sha256hex string, size int64) error {
	if size != m.Size || sha256hex != m.SHA256 {
		return ErrObjectMismatch
	}
	return nil
}
