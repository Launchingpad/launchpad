package netguard

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"
)

// InterfacePrefixes returns the subnet of every address on the host's network
// interfaces: for the app container, every network it is attached to.
func InterfacePrefixes() ([]netip.Prefix, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []netip.Prefix
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(ipn.IP)
		if !ok {
			continue
		}
		ones, _ := ipn.Mask.Size()
		ip = ip.Unmap()
		if len(ipn.IP) == net.IPv6len && ip.Is4() { // a mask of an unmapped v4 address
			if ones >= 96 {
				ones -= 96
			}
		}
		p, err := ip.Prefix(ones)
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// ReadDefaultGateways parses /proc/net/route text and returns the gateway of
// every default route (destination 0.0.0.0, mask 0.0.0.0).
func ReadDefaultGateways(r io.Reader) ([]netip.Addr, error) {
	sc := bufio.NewScanner(r)
	var out []netip.Addr
	first := true
	for sc.Scan() {
		if first { // the header
			first = false
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		v, err := parseHex32(f[2])
		if err != nil || v == 0 {
			continue
		}
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], v) // the kernel prints it in host (little-endian) order
		out = append(out, netip.AddrFrom4(b))
	}
	return out, sc.Err()
}

func parseHex32(s string) (uint32, error) {
	if len(s) != 8 {
		return 0, errors.New("bad hex")
	}
	var v uint32
	for _, c := range s {
		var d uint32
		switch {
		case c >= '0' && c <= '9':
			d = uint32(c - '0')
		case c >= 'a' && c <= 'f':
			d = uint32(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = uint32(c-'A') + 10
		default:
			return 0, errors.New("bad hex")
		}
		v = v<<4 | d
	}
	return v, nil
}

// ProcNetRouteGateways reads the default gateways from /proc/net/route.
func ProcNetRouteGateways() ([]netip.Addr, error) {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadDefaultGateways(f)
}

// Attached is the Dynamic of the git policy (T17 #31): every subnet attached
// to the app container, and its default gateway, read at boot and again each
// hour. If a read of the interface addresses fails or finds none, the guard
// FAILS CLOSED: every private address is refused until a read succeeds.
type Attached struct {
	readPrefixes func() ([]netip.Prefix, error)
	readGateways func() ([]netip.Addr, error)

	mu       sync.RWMutex
	prefixes []netip.Prefix
	ok       bool
}

// NewAttached builds an Attached from two readers (nil: the real ones). It is
// closed until Refresh succeeds; call Refresh once at boot.
func NewAttached(prefixes func() ([]netip.Prefix, error), gateways func() ([]netip.Addr, error)) *Attached {
	if prefixes == nil {
		prefixes = InterfacePrefixes
	}
	if gateways == nil {
		gateways = ProcNetRouteGateways
	}
	return &Attached{readPrefixes: prefixes, readGateways: gateways}
}

// Refresh re-reads the interface addresses. A failure (or an empty answer)
// closes the guard; the error says only that the read failed.
func (a *Attached) Refresh() error {
	ps, err := a.readPrefixes()
	if err == nil && len(ps) == 0 {
		err = errors.New("no interface addresses")
	}
	if err != nil {
		a.mu.Lock()
		a.prefixes, a.ok = nil, false
		a.mu.Unlock()
		return errors.New("netguard: cannot read the interface addresses: " + err.Error())
	}
	out := append([]netip.Prefix(nil), ps...)
	if gws, gerr := a.readGateways(); gerr == nil { // the gateway lies in an attached subnet; this is belt and braces
		for _, g := range gws {
			out = append(out, netip.PrefixFrom(g, g.BitLen()))
		}
	}
	a.mu.Lock()
	a.prefixes, a.ok = out, true
	a.mu.Unlock()
	return nil
}

// Run refreshes every interval until ctx ends. onErr (may be nil) is called
// with each failed refresh.
func (a *Attached) Run(ctx context.Context, every time.Duration, onErr func(error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := a.Refresh(); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}

// Denied implements Dynamic.
func (a *Attached) Denied() ([]netip.Prefix, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.prefixes, !a.ok
}
