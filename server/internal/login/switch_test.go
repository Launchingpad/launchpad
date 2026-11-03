package login

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
)

// countMailer counts sends; it is a TextMailer.
type countMailer struct{ n atomic.Int64 }

func (c *countMailer) SendLoginCode(context.Context, string, string) error {
	c.n.Add(1)
	return nil
}

func (c *countMailer) SendText(context.Context, string, string, string) error {
	c.n.Add(1)
	return nil
}

func TestMailSwitch(t *testing.T) {
	s := NewMailSwitch()
	if s.Ready() {
		t.Fatal("a new switch is ready")
	}
	if err := s.SendLoginCode(context.Background(), "a@b.co", "123456"); !errors.Is(err, ErrNoMailer) {
		t.Fatalf("send with no transport: %v", err)
	}
	if PublicMailError(ErrNoMailer) != ErrNoMailer.Error() {
		t.Fatal("PublicMailError hides ErrNoMailer")
	}
	a := &countMailer{}
	s.Set(a)
	if !s.Ready() {
		t.Fatal("not ready after Set")
	}
	if err := s.SendText(context.Background(), "a@b.co", "s", "t"); err != nil || a.n.Load() != 1 {
		t.Fatalf("send: %v, count %d", err, a.n.Load())
	}
	s.Set(nil)
	if s.Ready() {
		t.Fatal("ready after Set(nil)")
	}
}

// TestMailSwitch_SwapUnderLoad swaps the transport while many goroutines
// send (run with -race). Every send lands on exactly one transport or
// sees ErrNoMailer; none is lost or doubled.
func TestMailSwitch_SwapUnderLoad(t *testing.T) {
	s := NewMailSwitch()
	a, b := &countMailer{}, &countMailer{}
	s.Set(a)
	var none atomic.Int64
	var wg sync.WaitGroup
	const senders, each = 8, 500
	for range senders {
		wg.Go(func() {
			for range each {
				if err := s.SendLoginCode(context.Background(), "a@b.co", "123456"); errors.Is(err, ErrNoMailer) {
					none.Add(1)
				} else if err != nil {
					t.Errorf("send: %v", err)
				}
			}
		})
	}
	wg.Go(func() {
		for i := range 300 {
			switch i % 3 {
			case 0:
				s.Set(b)
			case 1:
				s.Set(nil)
			default:
				s.Set(a)
			}
		}
	})
	wg.Wait()
	if got := a.n.Load() + b.n.Load() + none.Load(); got != senders*each {
		t.Fatalf("sends accounted = %d, want %d", got, senders*each)
	}
}

// TestServiceEnabledFollowsAvailable: with Options.Available, login is on
// only while it says so (the switch's Ready, in the server).
func TestServiceEnabledFollowsAvailable(t *testing.T) {
	s := NewMailSwitch()
	deliver := func(context.Context, string, string) {}
	svc := New(nil, slog.New(slog.DiscardHandler), deliver, Options{Pepper: make([]byte, MinPepperLen), Available: s.Ready})
	if svc.Enabled() {
		t.Fatal("enabled with an empty switch")
	}
	if err := svc.RequestCode(context.Background(), "a@b.co"); err != errUnavailable {
		t.Fatalf("RequestCode with no transport: %v", err)
	}
	s.Set(&countMailer{})
	if !svc.Enabled() {
		t.Fatal("disabled with a transport")
	}
	s.Set(nil)
	if svc.Enabled() {
		t.Fatal("still enabled after the transport went")
	}
}
