package backup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SiloDialer is the dedicated dialer for the product's OWN silo (T16 #34,
// built for ticket 14). It is not a netguard.Dialer: the silo address is
// private and the guard would refuse it. It can connect to ONE host and port,
// those of offsite's own S3_ENDPOINT (from the environment), and nothing else:
//
//   - it compares the requested "host:port" NAME with the configured one
//     BEFORE any resolution; a different name, port or an IP literal that is
//     not the configured one is refused without a lookup;
//   - it does not read BACKUP_ALLOW_PRIVATE_CIDRS and has no policy;
//   - user-entered destinations never receive it: only the files code path
//     (ticket 14) constructs it, from the environment endpoint.
type SiloDialer struct {
	host    string
	port    string
	timeout time.Duration
}

// NewSiloDialer reads an endpoint URL such as http://silo:9000. The scheme must
// be http or https; a path, query, fragment or user info is refused (an endpoint
// names a host).
func NewSiloDialer(endpoint string, timeout time.Duration) (*SiloDialer, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, errors.New("backup: S3_ENDPOINT must be an http or https URL")
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("backup: S3_ENDPOINT must be a host only")
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &SiloDialer{host: normHost(u.Hostname()), port: port, timeout: timeout}, nil
}

func normHost(h string) string { return strings.TrimSuffix(strings.ToLower(h), ".") }

// Address is the one host:port it may dial.
func (s *SiloDialer) Address() string { return net.JoinHostPort(s.host, s.port) }

// DialContext has the signature of http.Transport.DialContext.
func (s *SiloDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("backup: network %q is not allowed for silo", network)
	}
	h, p, err := net.SplitHostPort(address)
	if err != nil || normHost(h) != s.host || p != s.port {
		return nil, errors.New("backup: the silo dialer connects to the configured silo only")
	}
	d := net.Dialer{Timeout: s.timeout}
	return d.DialContext(ctx, network, s.Address())
}

// NewHTTPClient builds a client on the silo dialer: Proxy nil, no redirect.
func (s *SiloDialer) NewHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           s.DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   s.timeout,
			ResponseHeaderTimeout: 2 * s.timeout,
			MaxIdleConns:          4,
			IdleConnTimeout:       30 * time.Second,
		},
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
