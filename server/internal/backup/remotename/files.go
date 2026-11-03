package remotename

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

// The names of the attachment (files) backup (T16 #38, ticket 14). They live in
// their OWN folders on the destination (FilesListFolder, FilesObjectFolder), never
// in the folder of the dumps: a dump listing of a WebDAV, SFTP or Dropbox folder
// lists the whole folder, and one name per attachment would push it past the list
// cap. The folder names do not start with "launchpad-<install>-", so the prefix
// list of the dump retention and of `fetch` (S3 lists by prefix, recursively)
// never walks them either.
//
//	object   launchpad-files-<install>-obj-<32 hex>.age       one attachment, age encrypted
//	list     launchpad-files-<install>-list-<time>-<4 hex>.list.age   the encrypted file list
//	manifest launchpad-files-<install>-list-<time>-<4 hex>.json       the signed manifest of that list
//
// The 32 hex of an object name are the first 16 bytes of sha256(key), the name
// design #38 asks for ("named by a hash of the key"). None of these names match
// Matcher (the dump expression), so dump retention treats them as foreign files
// and never deletes them.

// Suffixes of the files backup.
const (
	FilesObjectSuffix   = ".age"
	FilesListSuffix     = ".list.age"
	FilesManifestSuffix = ".json"
)

// FilesListPrefix is the List prefix that finds the manifests and lists of an install.
func FilesListPrefix(installID string) string { return "launchpad-files-" + installID + "-list-" }

var filesRe = regexp.MustCompile(`^launchpad-files-[0-9a-f]{8}-(obj-[0-9a-f]{32}\.age|list-\d{8}T\d{6}Z-[0-9a-f]{4}\.(list\.age|json))$`)

func validFilesName(name string) bool { return filesRe.MatchString(name) }

// FilesListFolder is the sub folder that holds the lists and manifests of an
// install; FilesObjectFolder holds the attachment objects. They are two folders so
// that finding the newest manifest (`List`) never has to list one name per
// attachment, whatever the destination type. Neither name starts with
// "launchpad-<install>-" and neither is a prefix of the other's path (the first
// has no "-objects" suffix and S3 lists by "folder/" prefix).
func FilesListFolder(installID string) (string, error) {
	if !ValidInstallID(installID) {
		return "", errors.New("remotename: install id must be 8 lower-case hex characters")
	}
	return "launchpad-files-" + installID, nil
}

// FilesObjectFolder: see FilesListFolder.
func FilesObjectFolder(installID string) (string, error) {
	if !ValidInstallID(installID) {
		return "", errors.New("remotename: install id must be 8 lower-case hex characters")
	}
	return "launchpad-files-" + installID + "-objects", nil
}

// FilesObjectPrefix is the List prefix of the objects whose key hash starts with
// shard (0 to 2 hex characters).
func FilesObjectPrefix(installID, shard string) string {
	return "launchpad-files-" + installID + "-obj-" + shard
}

// FilesObjectName is the remote name of the object with this silo key.
func FilesObjectName(installID, key string) (string, error) {
	if !ValidInstallID(installID) {
		return "", errors.New("remotename: install id must be 8 lower-case hex characters")
	}
	sum := sha256.Sum256([]byte(key))
	return "launchpad-files-" + installID + "-obj-" + hex.EncodeToString(sum[:16]) + FilesObjectSuffix, nil
}

// NewFilesStem makes the stem of one files manifest: launchpad-files-<install>-list-<time>-<4 hex>.
func NewFilesStem(installID string, t time.Time, r io.Reader) (string, error) {
	if !ValidInstallID(installID) {
		return "", errors.New("remotename: install id must be 8 lower-case hex characters")
	}
	if r == nil {
		r = rand.Reader
	}
	var b [2]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return "", fmt.Errorf("remotename: random: %w", err)
	}
	return fmt.Sprintf("launchpad-files-%s-list-%s-%02x%02x", installID, t.UTC().Format(TimeLayout), b[0], b[1]), nil
}

// FilesListName and FilesManifestName add the two suffixes to a files stem.
func FilesListName(stem string) string     { return stem + FilesListSuffix }
func FilesManifestName(stem string) string { return stem + FilesManifestSuffix }

// FilesName is a parsed files manifest or list name.
type FilesName struct {
	Stem     string
	Time     time.Time
	Manifest bool // the .json file (else the .list.age file)
}

// FilesMatcher parses the list and manifest names of ONE install.
type FilesMatcher struct{ re *regexp.Regexp }

// NewFilesMatcher returns the matcher of an install id (checked before it is
// put in an expression).
func NewFilesMatcher(installID string) (*FilesMatcher, error) {
	if !ValidInstallID(installID) {
		return nil, errors.New("remotename: install id must be 8 lower-case hex characters")
	}
	return &FilesMatcher{re: regexp.MustCompile(`^(launchpad-files-` + installID + `-list-(\d{8}T\d{6}Z)-[0-9a-f]{4})\.(list\.age|json)$`)}, nil
}

// Parse accepts only a whole name of this install with a real UTC time.
func (m *FilesMatcher) Parse(name string) (FilesName, bool) {
	sub := m.re.FindStringSubmatch(name)
	if sub == nil {
		return FilesName{}, false
	}
	t, err := time.Parse(TimeLayout, sub[2])
	if err != nil || t.Format(TimeLayout) != sub[2] {
		return FilesName{}, false
	}
	return FilesName{Stem: sub[1], Time: t, Manifest: sub[3] == "json"}, true
}

// ParseStem parses a stem.
func (m *FilesMatcher) ParseStem(stem string) (time.Time, bool) {
	n, ok := m.Parse(stem + FilesManifestSuffix)
	return n.Time, ok
}
