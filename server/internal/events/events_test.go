package events

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestBroker_PublishWakesOnlyThatOrg(t *testing.T) {
	b := NewBroker()
	orgA, orgB := uuid.New(), uuid.New()
	chA, cancelA := b.Subscribe(orgA)
	defer cancelA()
	chB, cancelB := b.Subscribe(orgB)
	defer cancelB()

	b.Publish(orgA)
	b.Publish(orgA) // collapses into the one pending wake-up; must not block

	select {
	case <-chA:
	default:
		t.Fatal("org A subscriber was not woken")
	}
	select {
	case <-chA:
		t.Fatal("two wake-ups were queued; want them collapsed into one")
	default:
	}
	select {
	case <-chB:
		t.Fatal("org B subscriber was woken by an org A event")
	default:
	}
}

func TestBroker_CancelRemovesSubscriber(t *testing.T) {
	b := NewBroker()
	org := uuid.New()
	_, cancel := b.Subscribe(org)
	if b.Subscribers() != 1 {
		t.Fatalf("subscribers = %d", b.Subscribers())
	}
	cancel()
	cancel() // idempotent
	if b.Subscribers() != 0 {
		t.Fatalf("subscribers after cancel = %d", b.Subscribers())
	}
	b.Publish(org) // no subscribers: must not panic
}

func TestBroker_PublishAllAndClose(t *testing.T) {
	b := NewBroker()
	ch1, c1 := b.Subscribe(uuid.New())
	defer c1()
	ch2, c2 := b.Subscribe(uuid.New())
	defer c2()
	b.PublishAll()
	<-ch1
	<-ch2
	b.Close()
	b.Close() // idempotent
	<-b.Done()
}

func TestBroker_Concurrent(t *testing.T) {
	b := NewBroker()
	org := uuid.New()
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			ch, cancel := b.Subscribe(org)
			b.Publish(org)
			<-ch
			cancel()
		})
	}
	wg.Wait()
	if b.Subscribers() != 0 {
		t.Fatalf("leaked subscribers: %d", b.Subscribers())
	}
}

// T1 follow-up: after a connection that worked, the reconnect
// backoff starts again from 1s; failures before any connection still double.
func TestListenLoop_BackoffResetsAfterAConnection(t *testing.T) {
	// The attempts: fail, fail, connect-then-drop, fail, fail, then stop.
	outcomes := []bool{false, false, true, false, false}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	i := 0
	once := func(context.Context) (bool, error) {
		connected := outcomes[i]
		i++
		return connected, errors.New("down")
	}
	var waits []time.Duration
	wait := func(_ context.Context, d time.Duration) bool {
		waits = append(waits, d)
		if len(waits) == len(outcomes) {
			cancel()
			return false
		}
		return true
	}
	listenLoop(ctx, once, wait, slog.New(slog.DiscardHandler))
	want := []time.Duration{time.Second, 2 * time.Second, time.Second, 2 * time.Second, 4 * time.Second}
	if !slices.Equal(waits, want) {
		t.Fatalf("waits %v, want %v", waits, want)
	}
	// The cap.
	waits, i = nil, 0
	outcomes = make([]bool, 8)
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	listenLoop(ctx, once, wait, slog.New(slog.DiscardHandler))
	if got := waits[len(waits)-1]; got != 30*time.Second {
		t.Fatalf("backoff not capped: %v", waits)
	}
}

func TestBroker_SubscribeAllHearsEveryOrg(t *testing.T) {
	b := NewBroker()
	all, cancel := b.SubscribeAll()
	defer cancel()
	one, cancelOne := b.Subscribe(uuid.New())
	defer cancelOne()
	b.Publish(uuid.New())
	select {
	case <-all:
	case <-time.After(time.Second):
		t.Fatal("SubscribeAll was not woken by another org's Publish")
	}
	select {
	case <-one:
		t.Fatal("an org subscriber was woken by another org")
	default:
	}
	b.PublishAll()
	select {
	case <-all:
	case <-time.After(time.Second):
		t.Fatal("SubscribeAll was not woken by PublishAll")
	}
}
