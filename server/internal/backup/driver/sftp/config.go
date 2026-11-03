// Package sftp is the SFTP backup driver (T16 ticket 9, LAUN-76). It talks to
// any SSH server with the sftp subsystem (a Linux host, a NAS) over the guarded
// dialer only (backup.DialSSH). The host key is pinned (T16 #36): the first pin
// is trust on first use, shown by Probe and saved by the app on confirmation.
// Auth is a private key and/or a password; both are sealed secrets.
package sftp

import (
	"bytes"
	"encoding/json"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"devops/internal/backup/driver"
)

// Type is the destination type this driver serves.
const Type = "sftp"

// Config is the typed JSON config of an sftp destination (no secrets). The
// secrets are password and/or privateKey (T16 ticket 2 note 13).
type Config struct {
	// Host is a DNS name or an IP literal (no port, no brackets).
	Host string `json:"host"`
	// Port defaults to 22.
	Port     int    `json:"port,omitempty"`
	Username string `json:"username"`
	// Folder is optional. It is relative to the login directory (or the chroot),
	// or absolute with ONE leading slash. Empty: the login directory itself.
	Folder string `json:"folder,omitempty"`
	// HostKey is the OpenSSH SHA256 fingerprint of the server's host key
	// ("SHA256:<43 base64 characters>", what `ssh-keygen -l` prints). Empty until
	// the admin confirmed the probed one (T16 #36).
	HostKey string `json:"hostKey,omitempty"`
}

var (
	hostRe    = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	userRe    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._@-]{0,63}$`)
	segmentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	hostKeyRe = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)
)

const (
	defaultPort  = 22
	maxFolderLen = 200
)

type parsed struct {
	Config
	host string // lower case
	port int
	addr string // host:port for the dialer
}

func bad() error { return driver.Errf(driver.ServerError) }

func parse(raw json.RawMessage) (parsed, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil || dec.More() {
		return parsed{}, bad()
	}
	host := strings.ToLower(c.Host)
	if a, err := netip.ParseAddr(host); err == nil {
		if a.Zone() != "" {
			return parsed{}, bad()
		}
		host = a.String()
	} else if !hostRe.MatchString(host) {
		return parsed{}, bad()
	}
	port := c.Port
	if port == 0 {
		port = defaultPort
	}
	if port < 1 || port > 65535 || !userRe.MatchString(c.Username) {
		return parsed{}, bad()
	}
	if c.HostKey != "" && !hostKeyRe.MatchString(c.HostKey) {
		return parsed{}, bad()
	}
	if c.Folder != "" {
		if len(c.Folder) > maxFolderLen {
			return parsed{}, bad()
		}
		f := strings.TrimPrefix(c.Folder, "/")
		for _, seg := range strings.Split(f, "/") {
			if !segmentRe.MatchString(seg) || seg == "." || seg == ".." || strings.Contains(seg, "..") {
				return parsed{}, bad()
			}
		}
	}
	return parsed{Config: c, host: host, port: port, addr: net.JoinHostPort(host, strconv.Itoa(port))}, nil
}

// Driver is the sftp driver. Its zero value is ready.
type Driver struct{}

// Validate implements driver.Driver. The error carries no value from the config.
func (Driver) Validate(config json.RawMessage) error {
	_, err := parse(config)
	return err
}

// Identity implements driver.Driver: host (with the port when it is not 22),
// the user name as the "bucket" (two users of one host may use the same relative
// folder name in different home directories) and the folder. The folder keeps a
// leading slash: an absolute and a relative folder are different places.
func (Driver) Identity(config json.RawMessage) (driver.Identity, error) {
	p, err := parse(config)
	if err != nil {
		return driver.Identity{}, err
	}
	h := p.host
	if strings.Contains(h, ":") {
		h = "[" + h + "]"
	}
	if p.port != defaultPort {
		h = p.addr
	}
	return driver.Identity{Type: Type, Host: h, Bucket: p.Username, Folder: p.Folder}, nil
}
