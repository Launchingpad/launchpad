package login

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Mailer sends a login code to an address. The code goes in the message
// body only: an implementation must never log it or put it in an error.
type Mailer interface {
	SendLoginCode(ctx context.Context, to, code string) error
}

// TextMailer is a Mailer that also sends one plain-text message (the
// platform mail test). Every mailer in this package is one.
type TextMailer interface {
	Mailer
	SendText(ctx context.Context, to, subject, text string) error
}

// Message is one email with both bodies and extra headers (LAUN-34,
// decisions-T13 #26). HTML may be empty (a plain-text mail). Headers are
// extra RFC 5322 headers, such as List-Unsubscribe; a mailer refuses a
// name or value that could start a new header. Message-ID, when set,
// replaces the one the mailer would make.
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
	Headers map[string]string
}

// MessageMailer sends a Message. The transports in this package are all
// one; MailSwitch forwards to the current one. Like every mailer here it
// returns only *MailError (or ErrNoMailer) and never puts the body, an
// address or a secret in an error or a log line.
type MessageMailer interface {
	SendMessage(ctx context.Context, m Message) error
}

// MailError is a send failure whose text is safe to show to a platform
// admin and to log: it names the step and the server's numeric reply code,
// never the server's reply text, a secret or the message body. Cause is
// kept for errors.Is (timeouts) and is never printed.
type MailError struct {
	Msg   string
	Cause error
}

func (e *MailError) Error() string { return e.Msg }

// Unwrap exposes the cause to errors.Is / errors.As only.
func (e *MailError) Unwrap() error { return e.Cause }

// PublicMailError is the text of a send failure that the mail test route
// may return: a *MailError's own text, else a fixed message. A mailer in
// this package returns only *MailError (or ErrNoMailer), so the fixed
// message is a second line of defence.
func PublicMailError(err error) string {
	var me *MailError
	switch {
	case errors.As(err, &me):
		return me.Msg
	case errors.Is(err, ErrNoMailer):
		return ErrNoMailer.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "the mail server did not answer in time"
	default:
		return "the mail transport failed (the server log has the step)"
	}
}

// message is the email text. Plain text only.
func message(code string) (subject, text string) {
	return "Your Launchpad login code",
		fmt.Sprintf("Your login code is %s\n\n"+
			"It works once and expires in 10 minutes.\n"+
			"If you did not ask for it, you can ignore this email.\n", code)
}

// PostmarkEndpoint is Postmark's single-email API.
const PostmarkEndpoint = "https://api.postmarkapp.com/email"

// PostmarkMailer sends through the Postmark API, in the same shape as
// the usual Postmark transactional email call.
type PostmarkMailer struct {
	Token    string
	From     string
	Stream   string // MessageStream; default "outbound"
	Endpoint string // default PostmarkEndpoint; tests point it at a fake
	Client   *http.Client
}

// NewPostmarkMailer returns a mailer with a 10-second HTTP timeout.
func NewPostmarkMailer(token, from, stream string) *PostmarkMailer {
	if stream == "" {
		stream = "outbound"
	}
	return &PostmarkMailer{Token: token, From: from, Stream: stream, Endpoint: PostmarkEndpoint,
		Client: &http.Client{Timeout: 10 * time.Second}}
}

type postmarkHeader struct {
	Name  string `json:"Name"`
	Value string `json:"Value"`
}

type postmarkRequest struct {
	From          string           `json:"From"`
	To            string           `json:"To"`
	Subject       string           `json:"Subject"`
	TextBody      string           `json:"TextBody"`
	HTMLBody      string           `json:"HtmlBody,omitempty"`
	Headers       []postmarkHeader `json:"Headers,omitempty"`
	MessageStream string           `json:"MessageStream"`
}

type postmarkError struct {
	ErrorCode int    `json:"ErrorCode"`
	Message   string `json:"Message"`
}

// String hides the token from %v and %+v, of a value or a pointer.
func (p PostmarkMailer) String() string { return "postmark mailer (from " + p.From + ")" }

// GoString hides the token from %#v, of a value or a pointer.
func (p PostmarkMailer) GoString() string { return p.String() }

// SendLoginCode posts one email. On failure the error names Postmark's
// status and error code, never the message body.
func (p *PostmarkMailer) SendLoginCode(ctx context.Context, to, code string) error {
	subject, text := message(code)
	return p.SendText(ctx, to, subject, text)
}

// SendText posts one plain-text email. Every error is a *MailError: the
// status and Postmark's numeric error code, never its message text or the
// token.
func (p *PostmarkMailer) SendText(ctx context.Context, to, subject, text string) error {
	return p.SendMessage(ctx, Message{To: to, Subject: subject, Text: text})
}

// SendMessage posts one email with a text body, an optional HTML body and
// custom headers (sorted by name, so the request is stable). Errors are as
// SendText's.
func (p *PostmarkMailer) SendMessage(ctx context.Context, m Message) error {
	if err := checkHeaders(m.Headers); err != nil {
		return err
	}
	req0 := postmarkRequest{From: p.From, To: m.To, Subject: m.Subject, TextBody: m.Text, HTMLBody: m.HTML, MessageStream: p.Stream}
	for _, k := range sortedKeys(m.Headers) {
		req0.Headers = append(req0.Headers, postmarkHeader{Name: k, Value: m.Headers[k]})
	}
	buf, err := json.Marshal(req0)
	if err != nil {
		return &MailError{Msg: "could not build the Postmark request", Cause: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Endpoint, bytes.NewReader(buf))
	if err != nil {
		return &MailError{Msg: "could not build the Postmark request", Cause: err}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Postmark-Server-Token", p.Token)
	resp, err := p.Client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
			return &MailError{Msg: "Postmark did not answer in time", Cause: err}
		}
		return &MailError{Msg: "could not reach Postmark", Cause: err}
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 400 {
		var pe postmarkError
		_ = json.Unmarshal(b, &pe)
		return &MailError{Msg: fmt.Sprintf("postmark status %d, error code %d", resp.StatusCode, pe.ErrorCode)}
	}
	return nil
}

// isTimeout reports a network timeout anywhere in err's chain.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// DevLogMailer writes the code to the log instead of sending it. It exists
// for a local or test server with no Postmark token, and NewDevLogMailer
// refuses to build one anywhere else.
type DevLogMailer struct {
	log *slog.Logger
}

// ErrNotDevServer is returned when a dev-only seam is asked for outside a
// local or test server.
var ErrNotDevServer = errors.New("the dev log mailer is allowed only with LOG_ENV local or test")

// NewDevLogMailer builds the dev mailer. devServer must come from a
// positive local/test check (config.DevSeams), so an unset or unknown
// environment never logs a code.
func NewDevLogMailer(log *slog.Logger, devServer bool) (*DevLogMailer, error) {
	if !devServer {
		return nil, ErrNotDevServer
	}
	return &DevLogMailer{log: log}, nil
}

// SendLoginCode logs the code. Dev servers only (see NewDevLogMailer).
func (d *DevLogMailer) SendLoginCode(ctx context.Context, to, code string) error {
	d.log.WarnContext(ctx, "login code (dev server only; never logged in staging or production)", "to", to, "code", code)
	return nil
}

// SendText logs that a message would go out, with its subject only. Dev
// servers only (see NewDevLogMailer).
func (d *DevLogMailer) SendText(ctx context.Context, to, subject, _ string) error {
	d.log.WarnContext(ctx, "email (dev server only: written to this log, not sent)", "to", to, "subject", subject)
	return nil
}

// SendMessage logs that a message would go out, with its subject only: no
// address, no body and no header (a header holds an unsubscribe token). Dev
// servers only (see NewDevLogMailer).
func (d *DevLogMailer) SendMessage(ctx context.Context, m Message) error {
	d.log.WarnContext(ctx, "email (dev server only: written to this log, not sent)", "subject", m.Subject)
	return nil
}

// Queue sends login emails on a background goroutine, so POST /auth/otp
// answers in the same time whether or not an email goes out (the Postmark
// round trip would otherwise tell members from non-members). It is
// bounded: when full, a code is dropped (and the person asks again).
type Queue struct {
	mailer  Mailer
	log     *slog.Logger
	jobs    chan mailJob
	timeout time.Duration
}

type mailJob struct{ to, code string }

// NewQueue returns a queue of the given size. Call Run to start sending.
func NewQueue(m Mailer, log *slog.Logger, size int) *Queue {
	return &Queue{mailer: m, log: log, jobs: make(chan mailJob, size), timeout: 15 * time.Second}
}

// Deliver enqueues a send without blocking. The request's context is not
// used: the send outlives the request, and runs under Run's context.
func (q *Queue) Deliver(_ context.Context, to, code string) {
	select {
	case q.jobs <- mailJob{to: to, code: code}:
	default:
		q.log.Error("login email queue is full; a login code was not sent")
	}
}

// Run sends queued emails until ctx is done. Jobs still queued then are
// dropped. It is the queue's only consumer.
func (q *Queue) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-q.jobs:
			sctx, cancel := context.WithTimeout(ctx, q.timeout)
			err := q.mailer.SendLoginCode(sctx, j.to, j.code)
			cancel()
			if err != nil {
				q.log.ErrorContext(ctx, "login code email failed", "err", err)
			}
		}
	}
}
