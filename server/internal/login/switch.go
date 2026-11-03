package login

import (
	"context"
	"errors"
	"sync/atomic"
)

// ErrNoMailer: the switch holds no transport (mail provider "none", or
// "env" with nothing configured in the environment).
var ErrNoMailer = errors.New("no mail transport is configured")

// MailSwitch is the login service's mailer. It forwards to the
// transport set last, and Set swaps it atomically: a send that has already
// picked a transport finishes on it, and every later send uses the new one.
// The zero value holds no transport. It is safe for concurrent use.
type MailSwitch struct {
	cur atomic.Pointer[switchState]
}

type switchState struct{ m TextMailer }

// NewMailSwitch returns a switch with no transport.
func NewMailSwitch() *MailSwitch { return &MailSwitch{} }

// Set makes m the transport. A nil m turns mail off (Ready reports false,
// and the login routes answer 503 login_unavailable).
func (s *MailSwitch) Set(m TextMailer) {
	s.cur.Store(&switchState{m: m})
}

func (s *MailSwitch) current() TextMailer {
	if st := s.cur.Load(); st != nil {
		return st.m
	}
	return nil
}

// Ready reports whether a transport is set. login.Options.Available uses
// it, so Service.Enabled follows the switch.
func (s *MailSwitch) Ready() bool { return s.current() != nil }

// SendLoginCode sends through the current transport.
func (s *MailSwitch) SendLoginCode(ctx context.Context, to, code string) error {
	m := s.current()
	if m == nil {
		return ErrNoMailer
	}
	return m.SendLoginCode(ctx, to, code)
}

// SendText sends through the current transport.
func (s *MailSwitch) SendText(ctx context.Context, to, subject, text string) error {
	m := s.current()
	if m == nil {
		return ErrNoMailer
	}
	return m.SendText(ctx, to, subject, text)
}

// SendMessage sends a Message through the current transport. A transport
// that cannot send one (it implements TextMailer only) is a *MailError, so a
// mail with headers is never sent without them.
func (s *MailSwitch) SendMessage(ctx context.Context, msg Message) error {
	m := s.current()
	if m == nil {
		return ErrNoMailer
	}
	mm, ok := m.(MessageMailer)
	if !ok {
		return &MailError{Msg: "the mail transport cannot send an HTML message with headers"}
	}
	return mm.SendMessage(ctx, msg)
}
