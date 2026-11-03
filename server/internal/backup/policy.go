// Package backup is the library of the scheduled off-site backups (T16,
// LAUN-28; this is ticket 3, LAUN-70): the backup network policy and the
// dedicated silo dialer, the guarded HTTP and SSH dial helpers, age
// encryption, the signed manifest, and the retention filter. It has no
// database code and starts no goroutine.
//
// The SSRF guard itself lives in internal/netguard (LAUN-6). This package
// holds only the backup policy on top of it. No file here builds a net.Dialer
// or an http.Client for a user-entered host except through netguard.
package backup

import (
	"errors"
	"io/fs"
	"net/netip"
	"time"

	"devops/internal/netguard"
)

// ServiceNames are the compose service names and Docker DNS names the backup
// policy refuses before resolution (T16 #34's test table). `web` is not in the
// list: it is not on offsite's networks (#13).
var ServiceNames = []string{
	"postgres", "app", "silo", "backup",
	"host.docker.internal", "gateway.docker.internal",
}

// Gateways reads the container's default gateways. The backup policy refuses
// each one unless the operator listed it as an exact /32 (T16 #34).
type Gateways struct {
	// Read returns the gateways; nil means netguard.ProcNetRouteGateways.
	Read func() ([]netip.Addr, error)
}

func (g Gateways) read() ([]netip.Addr, error) {
	if g.Read != nil {
		return g.Read()
	}
	return netguard.ProcNetRouteGateways()
}

// Addrs is Policy.Gateways. It reads at every call (a small file).
func (g Gateways) Addrs() []netip.Addr {
	a, err := g.read()
	if err != nil {
		return nil
	}
	return a
}

// Denied is netguard.Dynamic. A missing route file (not Linux) means no
// gateway is known. Any other read error FAILS CLOSED: every private address
// is refused, because the gateway can no longer be told apart.
func (g Gateways) Denied() ([]netip.Prefix, bool) {
	_, err := g.read()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, true
	}
	return nil, false
}

// Policy builds the backup network policy (T16 #34, #35). cidrs is
// BACKUP_ALLOW_PRIVATE_CIDRS: an operator list, validated by
// netguard.ParseCIDRs (/0, a /4, a loopback overlap are errors). The opened
// private ranges are exactly that list: unlike the git policy there is no
// "all standard ranges" switch. gw supplies the default-gateway rule.
func Policy(cidrs string, gw Gateways) (netguard.Policy, error) {
	listed, err := netguard.ParseCIDRs(cidrs)
	if err != nil {
		return netguard.Policy{}, errors.New("BACKUP_ALLOW_PRIVATE_CIDRS: " + err.Error())
	}
	return netguard.Policy{
		Private:  listed,
		Dynamic:  gw,
		Gateways: gw.Addrs,
		Names:    append([]string(nil), ServiceNames...),
	}, nil
}

// PolicyFromEnv reads BACKUP_ALLOW_PRIVATE_CIDRS through getenv.
func PolicyFromEnv(getenv func(string) string, gw Gateways) (netguard.Policy, error) {
	return Policy(getenv("BACKUP_ALLOW_PRIVATE_CIDRS"), gw)
}

// NewDialer is the guarded dialer of every user-entered destination. Drivers
// build their HTTP clients with NewHTTPClient(d) and their SSH connections
// with DialSSH(d).
func NewDialer(p netguard.Policy, timeout time.Duration) *netguard.Dialer {
	return &netguard.Dialer{Policy: p, Timeout: timeout}
}

// OperatorListed reports whether a is inside a range the operator listed. A
// driver allows an http:// URL only then (T16 #34: "not encrypted in
// transit"). An always-refused or metadata address is never listed-through:
// the dial still refuses it.
func OperatorListed(p netguard.Policy, a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	for _, r := range p.Private {
		if r.Contains(a) {
			return true
		}
	}
	return false
}
