// Package smtptest is an in-process fake SMTP server for tests.
// It speaks just enough SMTP for net/smtp: EHLO, STARTTLS, AUTH PLAIN, MAIL,
// RCPT, DATA, RSET, NOOP, QUIT, over plain TCP or implicit TLS, with a
// self-signed certificate for mail.test, localhost, 127.0.0.1 and ::1
// (RootCAs trusts it). It is test-only code: nothing in the server imports
// it.
package smtptest

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"io"
	"math/big"
	"net"
	"net/mail"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Options shape the server.
type Options struct {
	TLS      bool // implicit TLS from the first byte
	StartTLS bool // offer STARTTLS (ignored with TLS)
	// Auth offers AUTH PLAIN and accepts Username / Password only.
	Auth               bool
	Username, Password string
	// AuthFailText is the text after "535 " on a refused login. A test
	// plants a secret here to prove the client never repeats it.
	AuthFailText string
	// RcptFailText, when set, refuses every RCPT with "550 " + it.
	RcptFailText string
	// NoGreeting holds the connection open and never says 220 (a timeout).
	NoGreeting bool
}

// Message is one accepted message.
type Message struct {
	From     string // the MAIL FROM argument
	To       []string
	Data     []byte // the DATA, dot-unstuffed, LF line ends
	AuthUser string // "" without AUTH
	TLS      bool   // the session was encrypted when DATA ran
}

// Parse reads Data as an RFC 5322 message, so a test can check its headers
// (List-Unsubscribe, Message-ID) and walk its MIME parts.
func (m Message) Parse() (*mail.Message, error) {
	return mail.ReadMessage(bytes.NewReader(m.Data))
}

// Server is a running fake.
type Server struct {
	Addr    string // 127.0.0.1:port
	Port    int
	RootCAs *x509.CertPool

	opts Options
	cfg  *tls.Config
	ln   net.Listener
	wg   sync.WaitGroup

	mu    sync.Mutex
	msgs  []Message
	conns map[net.Conn]struct{}
}

// Start runs a server until the test ends.
func Start(t testing.TB, o Options) *Server {
	t.Helper()
	cert, pool := selfSigned(t)
	s := &Server{opts: o, RootCAs: pool, conns: map[net.Conn]struct{}{},
		cfg: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.ln = ln
	s.Addr = ln.Addr().String()
	s.Port = ln.Addr().(*net.TCPAddr).Port
	s.wg.Go(s.accept)
	t.Cleanup(s.Close)
	return s
}

// Close stops the server and waits for every session to end.
func (s *Server) Close() {
	_ = s.ln.Close()
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// Messages returns a copy of every accepted message.
func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.msgs...)
}

// Dial connects any host name to this server (SMTPMailer.Dial), so a test
// can use a name that is not loopback, such as mail.test.
func (s *Server) Dial(ctx context.Context, network, _ string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, s.Addr)
}

func (s *Server) accept() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		s.wg.Go(func() {
			defer func() {
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
				_ = c.Close()
			}()
			s.serve(c)
		})
	}
}

func (s *Server) serve(raw net.Conn) {
	_ = raw.SetDeadline(time.Now().Add(15 * time.Second))
	var c net.Conn = raw
	isTLS := false
	if s.opts.TLS {
		tc := tls.Server(raw, s.cfg)
		if err := tc.Handshake(); err != nil {
			return
		}
		c, isTLS = tc, true
	}
	if s.opts.NoGreeting {
		_, _ = io.Copy(io.Discard, c)
		return
	}
	tp := textproto.NewConn(c)
	_ = tp.PrintfLine("220 fake.test ESMTP")
	var msg Message
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO", "HELO":
			ext := []string{"fake.test"}
			if s.opts.StartTLS && !isTLS {
				ext = append(ext, "STARTTLS")
			}
			if s.opts.Auth {
				ext = append(ext, "AUTH PLAIN")
			}
			for i, e := range ext {
				sep := "-"
				if i == len(ext)-1 {
					sep = " "
				}
				_ = tp.PrintfLine("250%s%s", sep, e)
			}
		case "STARTTLS":
			if !s.opts.StartTLS || isTLS {
				_ = tp.PrintfLine("502 5.5.1 not offered")
				continue
			}
			_ = tp.PrintfLine("220 2.0.0 go ahead")
			tc := tls.Server(c, s.cfg)
			if err := tc.Handshake(); err != nil {
				return
			}
			c, isTLS = tc, true
			tp = textproto.NewConn(c)
		case "AUTH":
			s.auth(tp, arg, &msg)
		case "MAIL":
			msg.From = arg
			_ = tp.PrintfLine("250 2.1.0 ok")
		case "RCPT":
			if s.opts.RcptFailText != "" {
				_ = tp.PrintfLine("550 %s", s.opts.RcptFailText)
				continue
			}
			msg.To = append(msg.To, arg)
			_ = tp.PrintfLine("250 2.1.5 ok")
		case "DATA":
			_ = tp.PrintfLine("354 go ahead")
			data, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			msg.Data, msg.TLS = data, isTLS
			s.mu.Lock()
			s.msgs = append(s.msgs, msg)
			s.mu.Unlock()
			msg = Message{AuthUser: msg.AuthUser}
			_ = tp.PrintfLine("250 2.0.0 queued")
		case "RSET", "NOOP":
			_ = tp.PrintfLine("250 2.0.0 ok")
		case "QUIT":
			_ = tp.PrintfLine("221 2.0.0 bye")
			return
		default:
			_ = tp.PrintfLine("502 5.5.2 unknown command")
		}
	}
}

func (s *Server) auth(tp *textproto.Conn, arg string, msg *Message) {
	mech, resp, _ := strings.Cut(arg, " ")
	if !s.opts.Auth || !strings.EqualFold(mech, "PLAIN") {
		_ = tp.PrintfLine("504 5.5.4 mechanism not supported")
		return
	}
	if resp == "" {
		_ = tp.PrintfLine("334 ")
		l, err := tp.ReadLine()
		if err != nil {
			return
		}
		resp = l
	}
	b, err := base64.StdEncoding.DecodeString(resp)
	parts := strings.Split(string(b), "\x00")
	if err != nil || len(parts) != 3 || parts[1] != s.opts.Username || parts[2] != s.opts.Password {
		text := s.opts.AuthFailText
		if text == "" {
			text = "5.7.8 authentication failed"
		}
		_ = tp.PrintfLine("535 %s", text)
		return
	}
	msg.AuthUser = parts[1]
	_ = tp.PrintfLine("235 2.7.0 authenticated")
}

// selfSigned is a P-256 certificate for mail.test, localhost, 127.0.0.1
// and ::1, valid for an hour, and a pool that trusts it.
func selfSigned(t testing.TB) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "smtptest"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		DNSNames:              []string{"mail.test", "localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// HostPort is "host:port" for a host name and this server's port.
func (s *Server) HostPort(host string) string { return net.JoinHostPort(host, strconv.Itoa(s.Port)) }
