package netguard

import (
	"context"
	"errors"
	"net/netip"
	"testing"
)

func TestLegacyIPv4(t *testing.T) {
	ok := map[string]string{
		"127.1": "127.0.0.1", "2130706433": "127.0.0.1", "0x7f.1": "127.0.0.1", "0177.0.0.1": "127.0.0.1",
		"0x7f000001": "127.0.0.1", "10.1": "10.0.0.1", "134744072": "8.8.8.8", "192.168.257": "192.168.1.1", "127.1.": "127.0.0.1",
	}
	for in, want := range ok {
		a, found := LegacyIPv4(in)
		if !found || a.String() != want {
			t.Errorf("%q = %v %v, want %s", in, a, found, want)
		}
	}
	for _, in := range []string{"", "example.com", "1.2.3.4", "::1", "1.2.3.4.5", "256.1", "1.256.3", "4294967296", "0x", "1..2", "08", "a.1", "1.2.3.300", "0x1ffffffff"} {
		if a, found := LegacyIPv4(in); found {
			t.Errorf("%q parsed as %v", in, a)
		}
	}
}

func TestIsLocalName(t *testing.T) {
	for in, want := range map[string]bool{"localhost": true, "LOCALHOST.": true, "a.localhost": true, "intranet": true, "relay.example.com": false, "notlocalhost.com": false,
		"foo.internal": true, "a.b.LOCAL.": true, "nas.lan": true, "x.home.arpa": true, "home.arpa.example.com": false, "internal.example.com": false, "local.example.com": false, "mylan.com": false} {
		if IsLocalName(in) != want {
			t.Errorf("%q = %v", in, !want)
		}
	}
}

// The dial path never hands an inet_aton spelling to a resolver: "127.1" is the loopback address.
func TestDialRefusesLegacyIPv4Spellings(t *testing.T) {
	d := &Dialer{Resolver: failResolver{}}
	for _, h := range []string{"127.1", "2130706433", "0x7f.1", "0177.0.0.1"} {
		_, err := d.DialContext(context.Background(), "tcp", h+":443")
		if !errors.Is(err, ErrRefused) {
			t.Errorf("%s: %v, want a refusal", h, err)
		}
	}
}

type failResolver struct{}

func (failResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return nil, errors.New("the resolver must not be asked")
}
