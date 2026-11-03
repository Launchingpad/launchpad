package backup

import (
	"context"
	"errors"
	"time"

	"golang.org/x/crypto/ssh"

	"devops/internal/netguard"
)

// DialSSH connects to an SFTP host through the guarded dialer and hands the
// connection to ssh.NewClientConn (T16 #34: ssh.Dial would dial on its own).
// cfg must carry a HostKeyCallback; the SSH handshake is bounded by the
// earlier of the context's deadline and cfg.Timeout (30 s when unset). A refused address is a
// *netguard.RefusedError before any byte is sent.
func DialSSH(ctx context.Context, d *netguard.Dialer, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	if cfg == nil || cfg.HostKeyCallback == nil {
		return nil, errors.New("backup: ssh config needs a host key callback")
	}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	// The handshake deadline is the EARLIER of the context's and now+cfg.Timeout
	// (30 s when unset): a long context deadline must not give a silent server
	// more time than the config allows.
	t := cfg.Timeout
	if t <= 0 {
		t = 30 * time.Second
	}
	deadline := time.Now().Add(t)
	if cd, ok := ctx.Deadline(); ok && cd.Before(deadline) {
		deadline = cd
	}
	_ = conn.SetDeadline(deadline)
	// Close the socket if the context ends during the handshake.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if !stop() && err == nil {
		err = ctx.Err()
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}
