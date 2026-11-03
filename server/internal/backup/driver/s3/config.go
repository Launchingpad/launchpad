// Package s3 is the S3-compatible backup driver (T16 ticket 6, LAUN-73). It
// talks to AWS S3, MinIO, silo, Backblaze B2's S3 API, Wasabi and any service
// with the S3 REST API, over the guarded dialer only. Signing is the SigV4
// signer of internal/storage (standard library, no SDK). An object is one
// PUT (above 5 GiB is too_large, T16 #40): there is no multipart in v1.
package s3

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
const Type = "s3"

// Config is the typed JSON config of an s3 destination (no secrets). The
// secrets are accessKey and secretKey (T16 ticket 2 note 13).
type Config struct {
	// Endpoint is scheme://host[:port] with no path, query or user info.
	Endpoint string `json:"endpoint"`
	// Region signs the request; default us-east-1.
	Region string `json:"region,omitempty"`
	Bucket string `json:"bucket"`
	// Folder is an optional key prefix without a leading or trailing slash.
	Folder string `json:"folder,omitempty"`
	// Addressing is "path" (default: /bucket/key, what MinIO, silo and B2
	// accept) or "virtual" (bucket.host/key).
	Addressing string `json:"addressing,omitempty"`
	// TLSPin is the lower-case hex sha256 of the leaf's SubjectPublicKeyInfo
	// (T16 #36). A pin bypasses the chain and host name checks; the expiry
	// date is still checked.
	TLSPin string `json:"tlsPin,omitempty"`
}

var (
	bucketRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	regionRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	segmentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	pinRe     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

const (
	defaultRegion = "us-east-1"
	maxFolderLen  = 200
)

// parsed is a validated Config with the endpoint taken apart.
type parsed struct {
	Config
	scheme string
	host   string // lower case, no default port, as sent in the Host header
	hostIP string // endpoint host without port
	// reqHost is the host name requests really go to: the endpoint host, or
	// bucket.host for virtual addressing. The dial, the TLS server name, the
	// pin and the Probe all use it.
	reqHost string
	port    string // effective port
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
	if c.Region == "" {
		c.Region = defaultRegion
	}
	if c.Addressing == "" {
		c.Addressing = "path"
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" ||
		strings.ContainsAny(c.Endpoint, " \t\r\n\\") {
		return parsed{}, driver.Errf(driver.ServerError)
	}
	hostname := strings.ToLower(u.Hostname())
	port := u.Port()
	if hostname == "" || strings.HasSuffix(hostname, ".") {
		return parsed{}, driver.Errf(driver.ServerError)
	}
	def := "443"
	if u.Scheme == "http" {
		def = "80"
	}
	if port == "" {
		port = def
	}
	host := hostname
	if strings.Contains(hostname, ":") { // IPv6 literal
		host = "[" + hostname + "]"
	}
	if port != def {
		host = net.JoinHostPort(hostname, port)
	}
	switch {
	case !bucketRe.MatchString(c.Bucket), !regionRe.MatchString(c.Region),
		c.Addressing != "path" && c.Addressing != "virtual",
		c.TLSPin != "" && !pinRe.MatchString(c.TLSPin):
		return parsed{}, driver.Errf(driver.ServerError)
	}
	if c.Addressing == "virtual" && strings.Contains(hostname, ":") {
		return parsed{}, driver.Errf(driver.ServerError)
	}
	if c.Folder != "" {
		if len(c.Folder) > maxFolderLen {
			return parsed{}, driver.Errf(driver.ServerError)
		}
		for _, seg := range strings.Split(c.Folder, "/") {
			if !segmentRe.MatchString(seg) || seg == "." || seg == ".." || strings.Contains(seg, "..") {
				return parsed{}, driver.Errf(driver.ServerError)
			}
		}
	}
	req := hostname
	if c.Addressing == "virtual" {
		req = c.Bucket + "." + hostname
	}
	return parsed{Config: c, scheme: u.Scheme, host: host, hostIP: hostname, reqHost: req, port: port}, nil
}

// Driver is the s3 driver. Its zero value is ready.
type Driver struct{}

// Validate implements driver.Driver. The error carries no value from the
// config (the caller drops its text anyway).
func (Driver) Validate(config json.RawMessage) error {
	_, err := parse(config)
	return err
}

// Identity implements driver.Driver: the host without a default port, the
// bucket and the folder (T16 #25). Two endpoints that differ only in case or a
// default port are the same destination.
func (Driver) Identity(config json.RawMessage) (driver.Identity, error) {
	p, err := parse(config)
	if err != nil {
		return driver.Identity{}, err
	}
	return driver.Identity{Type: Type, Host: p.host, Bucket: p.Bucket, Folder: p.Folder}, nil
}
