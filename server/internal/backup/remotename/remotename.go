// Package remotename is the one place that knows the names of remote backup
// files (T16 #31 and #33). It is pure: no I/O. Every Get and Delete on a
// remote uses a name that Parse or Valid accepted, never a path or href that a
// remote's reply returned.
package remotename

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

// TimeLayout is the dump time in a name: 20261026T031500Z, always UTC.
const TimeLayout = "20060102T150405Z"

// Suffixes of the two files of one backup.
const (
	ObjectSuffix   = ".dump.age"
	ManifestSuffix = ".json"
)

// Kind says which file of a pair a name is.
type Kind int

const (
	KindObject Kind = iota + 1
	KindManifest
)

var installIDRe = regexp.MustCompile(`^[0-9a-f]{8}$`)

// ValidInstallID reports whether s is an install id (8 lower-case hex).
func ValidInstallID(s string) bool { return installIDRe.MatchString(s) }

// Name is a parsed backup file name.
type Name struct {
	Stem string    // launchpad-<install>-<time>-<4 hex>
	Kind Kind      // object or manifest
	Time time.Time // the dump time in the name, UTC
}

// Full returns the file name.
func (n Name) Full() string {
	if n.Kind == KindManifest {
		return n.Stem + ManifestSuffix
	}
	return n.Stem + ObjectSuffix
}

// matcher holds the compiled expression of one install id.
type matcher struct{ re *regexp.Regexp }

// Matcher parses the names of ONE install. The install id is checked before it
// is put in an expression, so it can never add syntax to it.
type Matcher struct{ m matcher }

// NewMatcher returns the matcher of an install id.
func NewMatcher(installID string) (*Matcher, error) {
	if !ValidInstallID(installID) {
		return nil, errors.New("remotename: install id must be 8 lower-case hex characters")
	}
	re := regexp.MustCompile(`^(launchpad-` + installID + `-(\d{8}T\d{6}Z)-[0-9a-f]{4})\.(dump\.age|json)$`)
	return &Matcher{m: matcher{re: re}}, nil
}

// Parse returns the parsed name, and false for any name that does not match
// the T16 #33 expression exactly or whose time is not a real UTC time (a
// month 13 is a foreign file, never deleted). The check is on the whole
// string: no path separator, dot segment, space or newline can pass.
func (m *Matcher) Parse(name string) (Name, bool) {
	sub := m.m.re.FindStringSubmatch(name)
	if sub == nil {
		return Name{}, false
	}
	t, err := time.Parse(TimeLayout, sub[2])
	if err != nil || t.Format(TimeLayout) != sub[2] {
		return Name{}, false
	}
	k := KindObject
	if sub[3] == "json" {
		k = KindManifest
	}
	return Name{Stem: sub[1], Kind: k, Time: t}, true
}

// ParseStem parses a stem (a name without its extension).
func (m *Matcher) ParseStem(stem string) (time.Time, bool) {
	n, ok := m.Parse(stem + ObjectSuffix)
	return n.Time, ok
}

// NewStem makes a stem for a dump time. r supplies the 4 random hex
// characters (nil: crypto/rand). A retry or a second run of the same dump
// calls it again and gets a NEW stem.
func NewStem(installID string, dumpTime time.Time, r io.Reader) (string, error) {
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
	return fmt.Sprintf("launchpad-%s-%s-%02x%02x", installID, dumpTime.UTC().Format(TimeLayout), b[0], b[1]), nil
}

// ObjectName and ManifestName add the two suffixes to a stem.
func ObjectName(stem string) string   { return stem + ObjectSuffix }
func ManifestName(stem string) string { return stem + ManifestSuffix }

// validRe is the set of names any driver may be asked to Put, Get or Delete
// whatever the install: a backup file of ANY install id, or a test file
// (T16 #36: launchpad-<install>-test-<random>.tmp). Drivers use it as a last
// line of defence against path traversal; retention uses the stricter Matcher.
var validRe = regexp.MustCompile(`^launchpad-[0-9a-f]{8}-(\d{8}T\d{6}Z-[0-9a-f]{4}\.(dump\.age|json)|test-[0-9a-f]{8,64}\.tmp)$`)

// Valid also accepts the names of the files backup (files.go).

// Valid reports whether name is safe to use as a remote file name.
func Valid(name string) bool {
	return len(name) <= 128 && (validRe.MatchString(name) || validFilesName(name))
}

// TestName returns a test file name for the install; random is lower-case hex.
func TestName(installID, randomHex string) (string, error) {
	n := "launchpad-" + installID + "-test-" + randomHex + ".tmp"
	if !ValidInstallID(installID) || !Valid(n) {
		return "", errors.New("remotename: bad test file name")
	}
	return n, nil
}
