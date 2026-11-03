package backup

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"devops/internal/netguard"
	"devops/internal/netguard/netguardtest"
)

func sshServer(t *testing.T) (addr string, hostKey ssh.PublicKey) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					c.Close()
					return
				}
				go ssh.DiscardRequests(reqs)
				go func() {
					for ch := range chans {
						ch.Reject(ssh.Prohibited, "no")
					}
				}()
				_ = sc.Wait()
			}()
		}
	}()
	return l.Addr().String(), signer.PublicKey()
}

func TestDialSSH(t *testing.T) {
	addr, hk := sshServer(t)
	cfg := &ssh.ClientConfig{User: "u", HostKeyCallback: ssh.FixedHostKey(hk), Timeout: 2 * time.Second}

	// The guard refuses loopback: an SFTP host that resolves to loopback never gets a byte.
	refusing := NewDialer(mustPolicy(t, "", gw()), time.Second)
	refusing.Resolver = &netguardtest.MapResolver{Hosts: map[string][]string{"sftp.example.test": {"127.0.0.1"}}}
	_, port, _ := net.SplitHostPort(addr)
	if _, err := DialSSH(context.Background(), refusing, "sftp.example.test:"+port, cfg); !errors.Is(err, netguard.ErrRefused) {
		t.Errorf("host resolving to loopback: %v", err)
	}
	if _, err := DialSSH(context.Background(), refusing, addr, cfg); !errors.Is(err, netguard.ErrRefused) {
		t.Errorf("loopback literal: %v", err)
	}

	// With the test seam the handshake works and checks the host key.
	allow := NewDialer(netguardtest.Permit(mustPolicy(t, "", gw()), netip.MustParseAddr("127.0.0.1").String()), time.Second)
	c, err := DialSSH(context.Background(), allow, addr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()

	_, other, _ := ed25519.GenerateKey(rand.Reader)
	otherSigner, _ := ssh.NewSignerFromKey(other)
	bad := &ssh.ClientConfig{User: "u", HostKeyCallback: ssh.FixedHostKey(otherSigner.PublicKey()), Timeout: 2 * time.Second}
	if _, err := DialSSH(context.Background(), allow, addr, bad); err == nil {
		t.Error("wrong host key accepted")
	}
	if _, err := DialSSH(context.Background(), allow, addr, &ssh.ClientConfig{User: "u"}); err == nil {
		t.Error("config without a host key callback accepted")
	}
	if _, err := DialSSH(context.Background(), allow, addr, nil); err == nil {
		t.Error("nil config accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DialSSH(ctx, allow, addr, cfg); err == nil {
		t.Error("canceled context accepted")
	}
}

// A silent server: the handshake ends at cfg.Timeout even when the context allows far longer.
func TestDialSSHHandshakeTimeoutIsMinOfContextAndConfig(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			defer c.Close() // never speaks
		}
	}()
	allow := NewDialer(netguardtest.Permit(mustPolicy(t, "", gw()), "127.0.0.1"), time.Second)
	_, hk := sshServer(t)
	cfg := &ssh.ClientConfig{User: "u", HostKeyCallback: ssh.FixedHostKey(hk), Timeout: 300 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := time.Now()
	if _, err := DialSSH(ctx, allow, l.Addr().String(), cfg); err == nil {
		t.Fatal("handshake with a silent server succeeded")
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("took %v: the config timeout was not applied", el)
	}
	// And the context still wins when it is earlier.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	cfg.Timeout = time.Minute
	start = time.Now()
	if _, err := DialSSH(ctx2, allow, l.Addr().String(), cfg); err == nil || time.Since(start) > 5*time.Second {
		t.Errorf("context deadline not applied: %v after %v", err, time.Since(start))
	}
}
