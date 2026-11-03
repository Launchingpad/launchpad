package login

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// SMTP security modes (docs/internal/decisions-T9-mail-settings.md).
const (
	SMTPStartTLS = "starttls" // plain connect, then STARTTLS (required, never skipped)
	SMTPTLS      = "tls"      // implicit TLS from the first byte (port 465)
	SMTPNone     = "none"     // no TLS: a loopback relay only, if it needs a password
)

// DefaultSMTPTimeout bounds one whole SMTP session.
const DefaultSMTPTimeout = 20 * time.Second

// SMTPMailer sends through an SMTP server with the standard library
// (net/smtp). Auth is PLAIN, and only over TLS or to a loopback host, the
// same rule as net/smtp's PlainAuth. No DKIM and no OAuth.
//
// Every error is a *MailError whose text names the step and the server's
// numeric reply code, never the server's reply text (a server may echo the
// username or worse), the password or the message.
type SMTPMailer struct {
	Host        string
	Port        int
	Security    string // SMTPStartTLS, SMTPTLS or SMTPNone
	Username    string // empty: no AUTH
	Password    string
	FromAddress string // addr-spec, e.g. tracker@example.com
	FromName    string // display name; may be empty

	Timeout time.Duration    // whole session; default DefaultSMTPTimeout
	RootCAs *x509.CertPool   // nil: the system roots (tests set a pool)
	Now     func() time.Time // the Date header; default time.Now
	// Dial opens the TCP connection; nil uses net.Dialer. Tests point a
	// host name at a local listener with it.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// String hides the password from %v and %+v, of a value or a pointer
// (a value receiver is in both method sets).
func (m SMTPMailer) String() string {
	return "smtp mailer " + net.JoinHostPort(m.Host, strconv.Itoa(m.Port)) + " (" + m.Security + ")"
}

// GoString hides the password from %#v, of a value or a pointer.
func (m SMTPMailer) GoString() string { return m.String() }

// SendLoginCode sends the login code email (the Postmark path's text).
func (m *SMTPMailer) SendLoginCode(ctx context.Context, to, code string) error {
	subject, text := message(code)
	return m.SendText(ctx, to, subject, text)
}

// IsLoopbackHost is net/smtp's own test for "PLAIN without TLS is fine":
// exactly localhost, 127.0.0.1 or ::1.
func IsLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// SendText runs one SMTP session: connect, (STARTTLS), AUTH PLAIN when a
// username is set, MAIL, RCPT, DATA, QUIT.
func (m *SMTPMailer) SendText(ctx context.Context, to, subject, text string) error {
	return m.SendMessage(ctx, Message{To: to, Subject: subject, Text: text})
}

// SendMessage runs one SMTP session for a Message: multipart/alternative
// when it has an HTML body, plain text otherwise, with its extra headers.
func (m *SMTPMailer) SendMessage(ctx context.Context, in Message) error {
	to := in.To
	msg, err := buildFullMessage(m.FromAddress, m.FromName, in, m.now())
	if err != nil {
		return err
	}
	if m.Username != "" && m.Security == SMTPNone && !IsLoopbackHost(m.Host) {
		return &MailError{Msg: "refused to send the SMTP password over a connection without TLS to a host that is not loopback; use security starttls or tls"}
	}
	timeout := m.Timeout
	if timeout <= 0 {
		timeout = DefaultSMTPTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	addr := net.JoinHostPort(m.Host, strconv.Itoa(m.Port))
	tlsCfg := &tls.Config{ServerName: m.Host, MinVersion: tls.VersionTLS12, RootCAs: m.RootCAs}

	conn, err := m.dial(ctx, addr)
	if err != nil {
		return dialError(err, addr)
	}
	// A cancelled ctx closes the connection, so a stuck read returns.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if m.Security == SMTPTLS {
		tc := tls.Client(conn, tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return tlsError(err, m.Host)
		}
		conn = tc
	}
	c, err := smtp.NewClient(conn, m.Host)
	if err != nil {
		_ = conn.Close()
		return stepError(ctx, "the greeting", err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Hello("localhost"); err != nil {
		return stepError(ctx, "EHLO", err)
	}
	if m.Security == SMTPStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return &MailError{Msg: "the SMTP server does not offer STARTTLS; choose security tls (port 465) or ask the provider"}
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			var pe *textproto.Error
			if errors.As(err, &pe) {
				return stepError(ctx, "STARTTLS", err)
			}
			return tlsError(err, m.Host)
		}
	}
	if m.Username != "" {
		if ok, _ := c.Extension("AUTH"); !ok {
			return &MailError{Msg: "the SMTP server does not offer AUTH, but a username is set"}
		}
		if err := c.Auth(smtp.PlainAuth("", m.Username, m.Password, m.Host)); err != nil {
			var pe *textproto.Error
			if errors.As(err, &pe) {
				return &MailError{Msg: fmt.Sprintf("the SMTP server refused the login (reply code %d); check the username and password", pe.Code), Cause: errors.New("smtp auth refused")}
			}
			return stepError(ctx, "AUTH", err)
		}
	}
	if err := c.Mail(m.FromAddress); err != nil {
		return stepError(ctx, "MAIL FROM (the sender address)", err)
	}
	if err := c.Rcpt(to); err != nil {
		return stepError(ctx, "RCPT TO (the recipient)", err)
	}
	w, err := c.Data()
	if err != nil {
		return stepError(ctx, "DATA", err)
	}
	if _, err := w.Write(msg); err != nil {
		_ = w.Close()
		return stepError(ctx, "the message body", err)
	}
	if err := w.Close(); err != nil {
		return stepError(ctx, "the end of the message", err)
	}
	// The message is accepted once DATA ends with 250; a failed QUIT
	// changes nothing.
	_ = c.Quit()
	return nil
}

func (m *SMTPMailer) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *SMTPMailer) dial(ctx context.Context, addr string) (net.Conn, error) {
	if m.Dial != nil {
		return m.Dial(ctx, "tcp", addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

// dialError: only the configured address, which is not a secret.
func dialError(err error, addr string) error {
	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		return &MailError{Msg: "timed out connecting to the SMTP server " + addr, Cause: err}
	}
	return &MailError{Msg: "could not connect to the SMTP server " + addr, Cause: err}
}

// tlsError names the kind of TLS failure, not the certificate's text.
func tlsError(err error, host string) error {
	var (
		unknown  x509.UnknownAuthorityError
		hostname x509.HostnameError
		invalid  x509.CertificateInvalidError
	)
	switch {
	case errors.As(err, &unknown):
		return &MailError{Msg: "the SMTP server's TLS certificate is not signed by a trusted authority", Cause: err}
	case errors.As(err, &hostname):
		return &MailError{Msg: "the SMTP server's TLS certificate is not valid for " + host, Cause: err}
	case errors.As(err, &invalid):
		return &MailError{Msg: "the SMTP server's TLS certificate is not valid (expired or not for this use)", Cause: err}
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		return &MailError{Msg: "the TLS handshake with the SMTP server timed out", Cause: err}
	default:
		return &MailError{Msg: "the TLS handshake with the SMTP server failed (is the security mode right for this port?)", Cause: err}
	}
}

// stepError is a failure at one step: the server's reply code when it sent
// one (never its text), else a timeout or a dropped connection.
func stepError(ctx context.Context, step string, err error) error {
	var pe *textproto.Error
	switch {
	case errors.As(err, &pe):
		return &MailError{Msg: fmt.Sprintf("the SMTP server refused %s (reply code %d)", step, pe.Code), Cause: errors.New("smtp reply")}
	case ctx.Err() != nil || isTimeout(err):
		return &MailError{Msg: "the SMTP server did not answer in time at " + step, Cause: context.DeadlineExceeded}
	default:
		return &MailError{Msg: "the connection to the SMTP server failed at " + step, Cause: errors.New("smtp connection")}
	}
}

// headerSafe drops every control character (CR and LF above all), so a
// value can never start a new header or end the header block.
func headerSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// plainAddress parses one addr-spec and refuses anything that is not a
// bare, control-free address.
func plainAddress(raw, what string) (string, error) {
	if raw == "" || raw != headerSafe(raw) {
		return "", &MailError{Msg: "the " + what + " address is not valid"}
	}
	a, err := mail.ParseAddress(raw)
	if err != nil || a.Name != "" || a.Address != raw {
		return "", &MailError{Msg: "the " + what + " address is not valid"}
	}
	return a.Address, nil
}

// buildMessage is the plain-text message of SendText.
func buildMessage(fromAddr, fromName, to, subject, text string, now time.Time) ([]byte, error) {
	return buildFullMessage(fromAddr, fromName, Message{To: to, Subject: subject, Text: text}, now)
}

// writeQP writes one QP-encoded body part.
func writeQP(b *bytes.Buffer, text string) error {
	qp := quotedprintable.NewWriter(b)
	if _, err := qp.Write([]byte(strings.ReplaceAll(text, "\r\n", "\n"))); err != nil {
		return &MailError{Msg: "could not build the message", Cause: err}
	}
	if err := qp.Close(); err != nil {
		return &MailError{Msg: "could not build the message", Cause: err}
	}
	if !bytes.HasSuffix(b.Bytes(), []byte("\r\n")) {
		b.WriteString("\r\n")
	}
	return nil
}

// buildFullMessage is the RFC 5322 message: text/plain, or
// multipart/alternative (text, then html) when in.HTML is set; UTF-8,
// quoted-printable, CRLF line ends. Every header value is control-free
// (headerSafe) and non-ASCII text is RFC 2047 encoded, so no input can add
// a header (header injection). Custom headers are checked by checkHeaders
// (a CR or LF is refused, not stripped) and written sorted.
func buildFullMessage(fromAddr, fromName string, in Message, now time.Time) ([]byte, error) {
	from, err := plainAddress(fromAddr, "sender")
	if err != nil {
		return nil, err
	}
	rcpt, err := plainAddress(in.To, "recipient")
	if err != nil {
		return nil, err
	}
	if err := checkHeaders(in.Headers); err != nil {
		return nil, err
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, &MailError{Msg: "could not build the message", Cause: err}
	}
	domain := from[strings.LastIndexByte(from, '@')+1:]
	var b bytes.Buffer
	hdr := func(k, v string) { b.WriteString(k + ": " + headerSafe(v) + "\r\n") }
	hdr("From", (&mail.Address{Name: headerSafe(fromName), Address: from}).String())
	hdr("To", "<"+rcpt+">")
	hdr("Subject", mime.QEncoding.Encode("utf-8", headerSafe(in.Subject)))
	hdr("Date", now.Format(time.RFC1123Z))
	msgID, custom := lookupHeader(in.Headers, "Message-ID")
	if !custom {
		msgID = "<" + hex.EncodeToString(id) + "@" + domain + ">"
	}
	hdr("Message-ID", msgID)
	for _, k := range sortedKeys(in.Headers) {
		if strings.EqualFold(k, "Message-ID") {
			continue
		}
		hdr(textproto.CanonicalMIMEHeaderKey(k), in.Headers[k])
	}
	hdr("MIME-Version", "1.0")
	if in.HTML == "" {
		hdr("Content-Type", "text/plain; charset=UTF-8")
		hdr("Content-Transfer-Encoding", "quoted-printable")
		b.WriteString("\r\n")
		if err := writeQP(&b, in.Text); err != nil {
			return nil, err
		}
		return b.Bytes(), nil
	}
	boundary := "lp-" + hex.EncodeToString(id)
	hdr("Content-Type", "multipart/alternative; boundary="+boundary)
	b.WriteString("\r\n")
	for _, part := range []struct{ ctype, body string }{
		{"text/plain; charset=UTF-8", in.Text}, {"text/html; charset=UTF-8", in.HTML}} {
		b.WriteString("--" + boundary + "\r\n")
		b.WriteString("Content-Type: " + part.ctype + "\r\n")
		b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		if err := writeQP(&b, part.body); err != nil {
			return nil, err
		}
	}
	b.WriteString("--" + boundary + "--\r\n")
	return b.Bytes(), nil
}
