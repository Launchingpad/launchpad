// Package webdav is the WebDAV backup driver (T16 ticket 7, LAUN-74). It serves
// Nextcloud, ownCloud and any WebDAV server, over the guarded dialer only,
// with HTTP Basic auth (a Nextcloud app password) over TLS only. An object is
// one PUT; the folder is made with MKCOL; a listing is PROPFIND depth 1.
// Request URLs are built from the configured base, the folder and a checked
// file name, never from a reply.
package webdav

import (
	"bytes"
	"encoding/json"
	"net"
	"net/url"
	"regexp"
	"strings"

	"devops/internal/backup/driver"
)

// Type is the destination type this driver serves.
const Type = "webdav"

// Config is the typed JSON config of a webdav destination (no secrets). The
// only secret is password (T16 ticket 2 note 13; the --remote-config file of #39
// uses the same names).
type Config struct {
	// URL is the https base of the WebDAV tree: for Nextcloud
	// https://host/remote.php/dav/files/<user>/. The base must exist.
	URL string `json:"url"`
	// Username is the Basic auth user (a Nextcloud login name).
	Username string `json:"username"`
	// Folder is an optional sub-folder under URL; every level is made with MKCOL.
	Folder string `json:"folder,omitempty"`
	// TLSPin is the lower-case hex sha256 of the leaf's SubjectPublicKeyInfo
	// (T16 #36). A pin bypasses the chain and host name checks; the expiry date
	// is still checked.
	TLSPin string `json:"tlsPin,omitempty"`
}

var (
	// baseSegRe is one decoded segment of the base path. Nextcloud login names
	// may hold @ . _ - + and (rarely) a space.
	baseSegRe = regexp.MustCompile(`^[A-Za-z0-9@._~+ -]{1,100}$`)
	segmentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	pinRe     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

const (
	maxFolderLen = 200
	maxBaseLen   = 400
	maxUserLen   = 255
)

// parsed is a validated Config with the URL taken apart.
type parsed struct {
	Config
	host    string   // lower case, no default port, as sent in the Host header
	reqHost string   // host name without port: the dial, SNI, host check and pin use it
	port    string   // effective port
	base    []string // decoded base path segments
	folder  []string // folder segments
}

func parse(raw json.RawMessage) (parsed, error) {
	bad := driver.Errf(driver.ServerError)
	var c Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil || dec.More() {
		return parsed{}, bad
	}
	low := strings.ToLower(c.URL)
	if strings.ContainsAny(c.URL, " \t\r\n\\") || strings.Contains(low, "%2f") || strings.Contains(low, "%5c") ||
		strings.Contains(low, "%00") || len(c.URL) > 1024 {
		return parsed{}, bad
	}
	u, err := url.Parse(c.URL)
	// Basic auth goes over TLS only (ticket 7 scope): http:// is refused.
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || u.Opaque != "" {
		return parsed{}, bad
	}
	hostname := strings.ToLower(u.Hostname())
	if hostname == "" || strings.HasSuffix(hostname, ".") {
		return parsed{}, bad
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	host := canonHost(hostname, port)
	var base []string
	if p := strings.Trim(u.Path, "/"); p != "" {
		if len(p) > maxBaseLen || strings.HasPrefix(u.Path, "//") || strings.Contains(u.Path, "//") {
			return parsed{}, bad
		}
		base = strings.Split(p, "/")
		for _, s := range base {
			if !baseSegRe.MatchString(s) || s == "." || strings.Contains(s, "..") {
				return parsed{}, bad
			}
		}
	}
	if c.Username == "" || len(c.Username) > maxUserLen || strings.Contains(c.Username, ":") {
		return parsed{}, bad
	}
	for _, r := range c.Username {
		if r < 0x20 || r == 0x7f {
			return parsed{}, bad
		}
	}
	if c.TLSPin != "" && !pinRe.MatchString(c.TLSPin) {
		return parsed{}, bad
	}
	var folder []string
	if c.Folder != "" {
		if len(c.Folder) > maxFolderLen {
			return parsed{}, bad
		}
		folder = strings.Split(c.Folder, "/")
		for _, seg := range folder {
			if !segmentRe.MatchString(seg) || strings.Contains(seg, "..") {
				return parsed{}, bad
			}
		}
	}
	return parsed{Config: c, host: host, reqHost: hostname, port: port, base: base, folder: folder}, nil
}

// canonHost is the lower-case host without a default (443) port.
func canonHost(hostname, port string) string {
	host := hostname
	if strings.Contains(hostname, ":") { // IPv6 literal
		host = "[" + hostname + "]"
	}
	if port != "443" {
		host = net.JoinHostPort(hostname, port)
	}
	return host
}

// Driver is the webdav driver. Its zero value is ready.
type Driver struct{}

// Validate implements driver.Driver. The error carries no value from the config.
func (Driver) Validate(config json.RawMessage) error {
	_, err := parse(config)
	return err
}

// Identity implements driver.Driver: the host without a default port, the base
// path (in Bucket, decoded, no outer slashes) and the folder (T16 #25). The
// user name is not part of it: the same path is the same destination.
func (Driver) Identity(config json.RawMessage) (driver.Identity, error) {
	p, err := parse(config)
	if err != nil {
		return driver.Identity{}, err
	}
	return driver.Identity{Type: Type, Host: p.host, Bucket: strings.Join(p.base, "/"), Folder: strings.Join(p.folder, "/")}, nil
}
