package backup

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"time"
	"unicode/utf8"

	"devops/internal/backup/remotename"
)

// The files manifest (T16 #38, ticket 14). A files backup writes two files next
// to each other, in the files folder of the destination:
//
//   - the LIST (`...-list-<time>-<4 hex>.list.age`): one JSON line per attachment
//     object (FileEntry), age encrypted to the same recipients as the dumps. It
//     holds silo keys, so it is encrypted. It can be large, so it is a stream, not
//     one JSON value.
//   - the MANIFEST (`....json`): the same envelope as a dump manifest (payload +
//     Ed25519 signature) over a payload that names the list, its sha256 and size,
//     the time and the install id. The signature covers FilesSigDomain plus the
//     payload bytes exactly as stored. The domain differs from SigDomain, so a dump
//     manifest can never pass as a files manifest or the other way round.
//
// The manifest is small whatever the number of objects (the 1 MiB cap of a dump
// manifest holds). The list is uploaded after the objects and the manifest after
// the list.

// FilesSigDomain separates this signature from the dump manifest's.
const FilesSigDomain = "launchpad-backup-files-manifest-v1\n"

// FilesManifestVersion is the payload version this code writes and accepts.
const FilesManifestVersion = 1

// Limits of a list (the memory bound of a run: the previous list is read into a map).
const (
	MaxFileEntries   = 200000
	MaxFileKeyBytes  = 1024
	maxFileLineBytes = 8 << 10
)

// FilesManifest is the signed payload.
type FilesManifest struct {
	Version          int    `json:"version"`
	InstallID        string `json:"installId"`
	List             string `json:"list"`   // the list file name
	SHA256           string `json:"sha256"` // of the encrypted list, 64 lower-case hex
	Size             int64  `json:"size"`   // bytes of the encrypted list
	Time             string `json:"time"`   // UTC, remotename.TimeLayout
	Objects          int    `json:"objects"`
	Bytes            int64  `json:"bytes"` // plain bytes of the objects the list names
	LaunchpadVersion string `json:"launchpadVersion"`
}

// FileEntry is one line of the list.
type FileEntry struct {
	Key          string `json:"key"`
	Size         int64  `json:"size"`     // plain bytes
	ETag         string `json:"etag"`     // of the silo listing, as silo sent it
	Modified     string `json:"modified"` // the silo listing's mtime, UTC RFC 3339
	SHA256       string `json:"sha256"`   // of the plain bytes
	Remote       string `json:"remote"`   // the remote object name
	CipherSize   int64  `json:"cipherSize"`
	CipherSHA256 string `json:"cipherSha256"`
	// LastSeen is the last run that saw the key in the silo listing (UTC RFC 3339).
	// A key that is gone from silo keeps its old value (ticket 15 prunes by it).
	LastSeen string `json:"lastSeen"`
}

// Files manifest errors (fixed texts).
var ErrFileList = errors.New("backup: files list is malformed")

// Validate checks one entry for an install.
func (e FileEntry) Validate(installID string) error {
	if e.Key == "" || len(e.Key) > MaxFileKeyBytes || !utf8.ValidString(e.Key) || e.Size < 0 || e.CipherSize < 0 {
		return ErrFileList
	}
	for _, r := range e.Key {
		if r < 0x20 || r == 0x7f {
			return ErrFileList
		}
	}
	want, err := remotename.FilesObjectName(installID, e.Key)
	if err != nil || e.Remote != want || !sha256Re.MatchString(e.SHA256) || !sha256Re.MatchString(e.CipherSHA256) || len(e.ETag) > 128 {
		return ErrFileList
	}
	for _, ts := range []string{e.Modified, e.LastSeen} {
		if _, err := time.Parse(time.RFC3339, ts); err != nil {
			return ErrFileList
		}
	}
	return nil
}

func (m FilesManifest) validate() error {
	if m.Version != FilesManifestVersion {
		return ErrManifestFormat
	}
	mt, err := remotename.NewFilesMatcher(m.InstallID)
	if err != nil {
		return ErrManifestFormat
	}
	n, ok := mt.Parse(m.List)
	if !ok || n.Manifest {
		return ErrManifestFormat
	}
	if !sha256Re.MatchString(m.SHA256) || m.Size < 0 || m.Objects < 0 || m.Bytes < 0 || !versionRe.MatchString(m.LaunchpadVersion) {
		return ErrManifestFormat
	}
	t, err := time.Parse(remotename.TimeLayout, m.Time)
	if err != nil || !t.Equal(n.Time) {
		return ErrManifestFormat
	}
	return nil
}

func filesSignedBytes(payload []byte) []byte {
	b := make([]byte, 0, len(FilesSigDomain)+len(payload))
	b = append(b, FilesSigDomain...)
	return append(b, payload...)
}

// SignFiles returns the files manifest file bytes.
func SignFiles(priv ed25519.PrivateKey, m FilesManifest) ([]byte, error) {
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
	sig := ed25519.Sign(priv, filesSignedBytes(payload))
	return json.Marshal(envelope{
		Payload:   base64.StdEncoding.EncodeToString(payload),
		Signature: base64.StdEncoding.EncodeToString(sig),
	})
}

// VerifyFilesManifest checks the size cap, the envelope, the signature and then
// the payload. Like VerifyManifest it proves the signer, not freshness: call
// Bind and CheckList.
func VerifyFilesManifest(pub ed25519.PublicKey, file []byte) (FilesManifest, error) {
	if len(pub) != ed25519.PublicKeySize {
		return FilesManifest{}, ErrBadKey
	}
	if len(file) > MaxManifestBytes {
		return FilesManifest{}, ErrManifestTooLarge
	}
	var env envelope
	if err := strictDecode(file, &env); err != nil {
		return FilesManifest{}, err
	}
	payload, err1 := base64.StdEncoding.DecodeString(env.Payload)
	sig, err2 := base64.StdEncoding.DecodeString(env.Signature)
	if err1 != nil || err2 != nil || len(sig) != ed25519.SignatureSize {
		return FilesManifest{}, ErrManifestFormat
	}
	if !ed25519.Verify(pub, filesSignedBytes(payload), sig) {
		return FilesManifest{}, ErrManifestSignature
	}
	var m FilesManifest
	if err := strictDecode(payload, &m); err != nil {
		return FilesManifest{}, err
	}
	if err := m.validate(); err != nil {
		return FilesManifest{}, err
	}
	return m, nil
}

// Bind fails when the manifest's list name, time or install id disagree with
// the stem that was fetched and the destination's install id.
func (m FilesManifest) Bind(installID, stem string) error {
	if m.InstallID != installID || m.List != remotename.FilesListName(stem) {
		return ErrManifestBinding
	}
	mt, err := remotename.NewFilesMatcher(installID)
	if err != nil {
		return ErrManifestBinding
	}
	t, ok := mt.ParseStem(stem)
	if !ok {
		return ErrManifestBinding
	}
	if mtm, err := time.Parse(remotename.TimeLayout, m.Time); err != nil || !mtm.Equal(t) {
		return ErrManifestBinding
	}
	return nil
}

// CheckList compares the size and sha256 (hex, of the encrypted bytes) of the list file.
func (m FilesManifest) CheckList(sha256hex string, size int64) error {
	if size != m.Size || sha256hex != m.SHA256 {
		return ErrObjectMismatch
	}
	return nil
}

// EncodeFileEntry appends one JSON line.
func EncodeFileEntry(w io.Writer, e FileEntry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// ReadFileList reads a plain list (after decryption): strict JSON lines, every
// entry validated, no duplicate key, at most MaxFileEntries. fn is called for
// each entry; a false return stops with no error.
func ReadFileList(r io.Reader, installID string, fn func(FileEntry) bool) (int, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	seen := map[string]bool{}
	n := 0
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return n, ErrFileList
		}
		if err != nil && err != io.EOF {
			return n, ErrFileList
		}
		if len(line) > maxFileLineBytes {
			return n, ErrFileList
		}
		if t := bytes.TrimRight(line, "\n"); len(t) > 0 {
			if err == io.EOF { // the last line has no newline: truncated
				return n, ErrFileList
			}
			var e FileEntry
			if derr := strictDecode(t, &e); derr != nil || e.Validate(installID) != nil || seen[e.Key] {
				return n, ErrFileList
			}
			seen[e.Key] = true
			n++
			if n > MaxFileEntries {
				return n, ErrFileList
			}
			if !fn(e) {
				return n, nil
			}
		} else if err == nil {
			return n, ErrFileList // an empty line
		}
		if err == io.EOF {
			return n, nil
		}
	}
}
