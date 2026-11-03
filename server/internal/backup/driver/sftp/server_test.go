package sftp

import (
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gosftp "github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// testServer is an in-process SSH server with the pkg/sftp server on the
// "sftp" subsystem. It is hostile on request: every flag below is a way a real
// or a broken server can behave.
type testServer struct {
	t  *testing.T
	ln net.Listener

	mu        sync.Mutex
	hostKey   ssh.Signer
	user      string
	password  string
	userKey   ssh.PublicKey
	kbdOnly   bool
	authDelay time.Duration // the server waits this long before it answers an auth request
	authGate  chan struct{} // non-nil: the server holds every password auth request until it is closed

	// What it records.
	authAttempts atomic.Int64 // every auth request, "none" included
	conns        atomic.Int64
	served       atomic.Int64 // connections whose serve loop has returned
	seenMu       sync.Mutex
	seen         map[string]bool // remote addresses the accept loop has taken
	sessions     atomic.Int64    // sftp subsystems started

	// Behaviour.
	hangHandshake atomic.Bool // accept TCP and say nothing at all
	noSFTP        atomic.Bool // refuse the sftp subsystem
	hangSFTP      atomic.Bool // accept the subsystem and never answer
	realDir       string      // non-empty: pkg/sftp's real-filesystem server rooted here
	handlers      func() gosftp.Handlers
	reqOpts       []gosftp.RequestServerOption
}

func newSigner(t testing.TB) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &testServer{t: t, ln: ln, hostKey: newSigner(t), user: "backup", password: "s3cret"}
	shared := gosftp.InMemHandler() // one file system for every connection
	s.handlers = func() gosftp.Handlers { return shared }
	t.Cleanup(func() { _ = ln.Close() })
	go s.accept()
	return s
}

func (s *testServer) setHostKey(k ssh.Signer) { s.mu.Lock(); s.hostKey = k; s.mu.Unlock() }
func (s *testServer) fingerprint() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ssh.FingerprintSHA256(s.hostKey.PublicKey())
}

func (s *testServer) addr() (string, int) {
	a := s.ln.Addr().(*net.TCPAddr)
	return "127.0.0.1", a.Port
}

func (s *testServer) config() *ssh.ServerConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := &ssh.ServerConfig{
		AuthLogCallback: func(ssh.ConnMetadata, string, error) { s.authAttempts.Add(1) },
	}
	if !s.kbdOnly {
		cfg.PasswordCallback = func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			s.mu.Lock()
			d, gate := s.authDelay, s.authGate
			s.mu.Unlock()
			time.Sleep(d)
			if gate != nil {
				<-gate
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.password != "" && c.User() == s.user && string(pw) == s.password {
				return nil, nil
			}
			return nil, os.ErrPermission
		}
	} else {
		cfg.KeyboardInteractiveCallback = func(c ssh.ConnMetadata, ch ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
			ans, err := ch(c.User(), "", []string{"Password: "}, []bool{false})
			s.mu.Lock()
			defer s.mu.Unlock()
			if err == nil && len(ans) == 1 && s.password != "" && c.User() == s.user && ans[0] == s.password {
				return nil, nil
			}
			return nil, os.ErrPermission
		}
	}
	cfg.PublicKeyCallback = func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.userKey != nil && c.User() == s.user && string(k.Marshal()) == string(s.userKey.Marshal()) {
			return nil, nil
		}
		return nil, os.ErrPermission
	}
	cfg.AddHostKey(s.hostKey)
	return cfg
}

func (s *testServer) accept() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		// conns is counted BEFORE the address is recorded: settle() waits for the
		// canary's address and then compares conns with served, so a connection that is
		// recorded but not yet counted would let it read a short count.
		s.conns.Add(1)
		s.seenMu.Lock()
		if s.seen == nil {
			s.seen = map[string]bool{}
		}
		s.seen[c.RemoteAddr().String()] = true
		s.seenMu.Unlock()
		go s.serve(c)
	}
}

// settle waits until the server has finished with every connection that was made
// before the call, so a test can then assert what the server did NOT see (LAUN-176:
// this replaced sleeps that guessed how long a late connection or auth request
// would take). It dials a canary: the accept loop takes connections in order, so
// when the canary is taken every earlier one has been counted; then it waits for
// every serve loop to return (the client has closed its connections by then).
// The canary is counted by the accept loop like any connection (conns, and served
// when its serve loop returns). The wait compares those totals WITH the canary, so an
// earlier connection that is not yet served keeps served below conns. The two
// decrements at the end then remove the canary again, so the counts a test reads
// after settle are its own connections only. That is safe because tests are
// synchronous: no real connection is in flight when settle returns. A connection
// that a broken driver makes between the wait and the decrements is still visible:
// it is counted in conns, and the decrements remove one each, never that connection.
func (s *testServer) settle() {
	s.t.Helper()
	c, err := net.Dial("tcp", s.ln.Addr().String())
	if err != nil {
		s.t.Fatal(err)
	}
	addr := c.LocalAddr().String()
	deadline := time.Now().Add(60 * time.Second)
	for {
		s.seenMu.Lock()
		ok := s.seen[addr]
		s.seenMu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			s.t.Fatal("settle: the canary was never accepted")
		}
		time.Sleep(time.Millisecond)
	}
	_ = c.Close()
	for s.served.Load() != s.conns.Load() {
		if time.Now().After(deadline) {
			s.t.Fatalf("settle: %d connections, %d served", s.conns.Load(), s.served.Load())
		}
		time.Sleep(time.Millisecond)
	}
	s.conns.Add(-1)
	s.served.Add(-1)
}

func (s *testServer) serve(c net.Conn) {
	defer s.served.Add(1)
	if s.hangHandshake.Load() {
		go func() { time.Sleep(20 * time.Second); _ = c.Close() }()
		return
	}
	sc, chans, reqs, err := ssh.NewServerConn(c, s.config())
	if err != nil {
		_ = c.Close()
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "no")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go s.session(ch, creqs)
	}
}

func (s *testServer) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	for r := range reqs {
		if r.Type != "subsystem" || len(r.Payload) < 4 || string(r.Payload[4:]) != "sftp" || s.noSFTP.Load() {
			_ = r.Reply(false, nil)
			continue
		}
		_ = r.Reply(true, nil)
		s.sessions.Add(1)
		if s.hangSFTP.Load() {
			go func() { time.Sleep(30 * time.Second); _ = ch.Close() }()
			continue
		}
		go func() {
			defer ch.Close()
			if s.realDir != "" {
				srv, err := gosftp.NewServer(ch, gosftp.WithServerWorkingDirectory(s.realDir))
				if err == nil {
					_ = srv.Serve()
				}
				return
			}
			srv := gosftp.NewRequestServer(ch, s.handlers(), s.reqOpts...)
			_ = srv.Serve()
		}()
	}
}

var _ = io.EOF
