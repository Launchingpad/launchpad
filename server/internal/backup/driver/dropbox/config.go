// Package dropbox is the Dropbox backup driver (T16 ticket 10, LAUN-77). It
// speaks HTTPS and OAuth 2 to Dropbox's two fixed hosts (api.dropboxapi.com and
// content.dropboxapi.com) over the guarded dialer, with the standard library
// only. The app key, app secret and refresh token are the destination's sealed
// secrets; an access token lives in memory and is never stored, logged or put
// in an error. Every Dropbox fact in this package is from Dropbox's published
// documentation: it was NOT run against a real Dropbox account.
package dropbox

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"

	"devops/internal/backup/driver"
)

// Type is the destination type this driver serves.
const Type = "dropbox"

// Access modes of the Dropbox app (what the admin chose when making it).
const (
	// AccessAppFolder: the app sees only its own folder (Apps/<app name>).
	// Paths are relative to it. The default and the recommended mode (T16 #9).
	AccessAppFolder = "app_folder"
	// AccessFull: the app sees the whole Dropbox. A folder is then required, so
	// the driver never lists or writes in the root of a person's Dropbox.
	AccessFull = "full_dropbox"
)

// Config is the typed JSON config of a dropbox destination (no secrets). The
// secrets are appKey, appSecret and refreshToken (T16 ticket 2 note 13); the
// refreshToken is written only by the exchange run.
type Config struct {
	// Access is "app_folder" (default) or "full_dropbox".
	Access string `json:"access,omitempty"`
	// Folder is an optional sub folder without a leading or trailing slash. It
	// is required with full_dropbox. Dropbox paths are not case sensitive.
	Folder string `json:"folder,omitempty"`
	// TLSPin is the lower-case hex sha256 of a leaf's SubjectPublicKeyInfo
	// (T16 #36). It applies to BOTH Dropbox hosts and replaces the chain and host
	// name check (the expiry date is still checked). Dropbox has a public chain,
	// so it is normally empty; it is for an operator behind a TLS-inspecting proxy.
	TLSPin string `json:"tlsPin,omitempty"`
}

var (
	// segmentRe: a folder segment. It may not end in "." (Dropbox refuses a
	// trailing dot or space in a name).
	segmentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	pinRe     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

const maxFolderLen = 200

type parsed struct {
	Config
}

func parse(raw json.RawMessage) (parsed, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return parsed{}, driver.Errf(driver.ServerError)
	}
	if dec.More() {
		return parsed{}, driver.Errf(driver.ServerError)
	}
	if c.Access == "" {
		c.Access = AccessAppFolder
	}
	if c.Access != AccessAppFolder && c.Access != AccessFull {
		return parsed{}, driver.Errf(driver.ServerError)
	}
	if c.TLSPin != "" && !pinRe.MatchString(c.TLSPin) {
		return parsed{}, driver.Errf(driver.ServerError)
	}
	if c.Access == AccessFull && c.Folder == "" {
		return parsed{}, driver.Errf(driver.ServerError)
	}
	if c.Folder != "" {
		if len(c.Folder) > maxFolderLen {
			return parsed{}, driver.Errf(driver.ServerError)
		}
		for _, seg := range strings.Split(c.Folder, "/") {
			if !segmentRe.MatchString(seg) || strings.HasSuffix(seg, ".") || strings.Contains(seg, "..") {
				return parsed{}, driver.Errf(driver.ServerError)
			}
		}
	}
	return parsed{Config: c}, nil
}

// Driver is the dropbox driver. Its zero value is ready.
type Driver struct{}

// Validate implements driver.Driver. The error carries no value from the config.
func (Driver) Validate(config json.RawMessage) error {
	_, err := parse(config)
	return err
}

// Identity implements driver.Driver. Dropbox has one host, so Host is the API
// host; Bucket is the access mode (an app-folder and a full-Dropbox destination
// never collide); Folder is lower case because Dropbox paths are case
// insensitive. The Dropbox app itself (its key) is a secret and not part of the
// identity: two destinations on different apps with the same folder are refused
// as one (a false collision, on the safe side of T16 #25).
func (Driver) Identity(config json.RawMessage) (driver.Identity, error) {
	p, err := parse(config)
	if err != nil {
		return driver.Identity{}, err
	}
	return driver.Identity{Type: Type, Host: "api.dropboxapi.com", Bucket: p.Access, Folder: strings.ToLower(p.Folder)}, nil
}
