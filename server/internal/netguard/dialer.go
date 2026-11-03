package netguard

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// Resolver turns a host name into addresses. A test passes its own; the
// default is the system resolver.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Dialer dials only addresses its Policy allows. The zero Timeout is 10 s.
type Dialer struct {
	Policy   Policy
	Resolver Resolver // nil: net.DefaultResolver
	Timeout  time.Duration
}

func (d *Dialer) timeout() time.Duration {
	if d.Timeout > 0 {
		return d.Timeout
	}
	return 10 * time.Second
}

func (d *Dialer) resolver() Resolver {
	if d.Resolver != nil {
		return d.Resolver
	}
	return net.DefaultResolver
}

// control is the last line: it checks the address the socket is about to
// connect to, whatever path chose it.
func (d *Dialer) control(network, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return &RefusedError{Target: "unparseable address", Reason: "not an address"}
	}
	return d.Policy.Check(ap.Addr())
}

// DialContext resolves the host ONCE, checks every answer against the policy
// (one refused answer refuses the host), then dials the checked addresses. It
// has the signature of http.Transport.DialContext.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("netguard: network %q is not allowed", network)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("netguard: bad address: %w", err)
	}
	var addrs []netip.Addr
	if ip, perr := netip.ParseAddr(host); perr == nil {
		addrs = []netip.Addr{ip}
	} else if ip, ok := LegacyIPv4(host); ok {
		// "127.1" and "2130706433" are addresses, not names: never hand them to a resolver.
		addrs = []netip.Addr{ip}
	} else {
		if d.Policy.RefusesName(host) {
			return nil, &RefusedError{Target: host, Reason: "service name"}
		}
		addrs, err = d.resolver().LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("netguard: cannot resolve host: %w", err)
		}
		if len(addrs) == 0 {
			return nil, errors.New("netguard: host has no address")
		}
	}
	for _, a := range addrs {
		if err := d.Policy.Check(a); err != nil {
			return nil, err
		}
	}
	nd := net.Dialer{Timeout: d.timeout(), Control: d.control}
	var last error
	for _, a := range addrs {
		if network == "tcp4" && !a.Unmap().Is4() || network == "tcp6" && a.Unmap().Is4() {
			continue
		}
		c, derr := nd.DialContext(ctx, network, net.JoinHostPort(a.WithZone("").String(), port))
		if derr == nil {
			return c, nil
		}
		last = derr
		if ctx.Err() != nil {
			break
		}
	}
	if last == nil {
		last = errors.New("netguard: no usable address")
	}
	return nil, last
}

// NewTransport builds an HTTP transport on the guarded dialer: no proxy (the
// environment's proxy variables are ignored), TLS 1.2 or newer with the system
// roots, no "skip verify".
func NewTransport(d *Dialer) *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DialContext:           d.DialContext,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   d.timeout(),
		ResponseHeaderTimeout: 2 * d.timeout(),
		MaxIdleConns:          10,
		IdleConnTimeout:       30 * time.Second,
	}
}

// NewClient builds an HTTP client on NewTransport that follows no redirect: a
// 3xx comes back as a response for the caller to treat as an error (a
// redirect is a common way to send credentials to a second host).
func NewClient(d *Dialer, timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: NewTransport(d),
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
