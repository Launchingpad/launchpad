package tracker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"sync"
	"testing"
	"time"
)

// The job loop: a run at boot, then one per tick, the clock it is given,
// and an exit when ctx ends. No database (the DB side is in
// cycles_integration_test.go).
func TestRunCycleJob_BootRunTicksAndStops(t *testing.T) {
	fake := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	var seen []time.Time
	runs := make(chan struct{}, 16)
	run := func(_ context.Context, now time.Time) (CycleRun, error) {
		mu.Lock()
		seen = append(seen, now)
		n := len(seen)
		mu.Unlock()
		select {
		case runs <- struct{}{}:
		default: // never block the loop (it must stay cancellable)
		}
		if n == 2 {
			return CycleRun{}, errors.New("one bad run") // logged; the loop goes on
		}
		return CycleRun{Teams: 1}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunCycleJob(ctx, run, func() time.Time { return fake }, 5*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	for i := 0; i < 3; i++ {
		select {
		case <-runs:
		case <-time.After(5 * time.Second):
			t.Fatalf("run %d never happened", i+1)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunCycleJob did not return after cancel")
	}
	mu.Lock()
	defer mu.Unlock()
	for i, s := range seen {
		if !s.Equal(fake) {
			t.Fatalf("run %d used %s, not the injected clock", i+1, s)
		}
	}
}

func TestRunCycleJob_FirstRunIsImmediate(t *testing.T) {
	ran := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunCycleJob(ctx, func(context.Context, time.Time) (CycleRun, error) {
		select {
		case ran <- struct{}{}:
		default:
		}
		return CycleRun{}, nil
	}, time.Now, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("no boot run (the interval is an hour)")
	}
}

func TestParseFilters_RejectUnknownAndBadValues(t *testing.T) {
	tests := []struct {
		name  string
		parse func(url.Values) error
		query string
	}{
		{"project: unknown param", func(v url.Values) error { _, err := ParseProjectFilter(v); return err }, "stat=started"},
		{"project: limit 251", func(v url.Values) error { _, err := ParseProjectFilter(v); return err }, "limit=251"},
		{"project: limit 0", func(v url.Values) error { _, err := ParseProjectFilter(v); return err }, "limit=0"},
		{"milestone: project not an id", func(v url.Values) error { _, err := ParseMilestoneFilter(v); return err }, "project=web"},
		{"cycle: active not a bool", func(v url.Values) error { _, err := ParseCycleFilter(v); return err }, "active=yes"},
		{"document: unknown param", func(v url.Values) error { _, err := ParseDocumentFilter(v); return err }, "q=x"},
		{"document: garbage cursor", func(v url.Values) error { _, err := ParseDocumentFilter(v); return err }, "cursor=abc"},
		{"document: a project cursor", func(v url.Values) error { _, err := ParseDocumentFilter(v); return err },
			"cursor=" + encodeCursor(cursor{OrderBy: "projects", At: time.Now(), ID: [16]byte{1}})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := url.ParseQuery(tt.query)
			if err != nil {
				t.Fatal(err)
			}
			if err := tt.parse(v); err == nil {
				t.Fatalf("%s accepted", tt.query)
			}
		})
	}
	v, _ := url.ParseQuery("state=started,paused&lead=me&team=ORB&query=x&full=true&limit=250&includeArchived=true")
	f, err := ParseProjectFilter(v)
	if err != nil || f.Limit != 250 || !f.Full || !f.IncludeArchived || len(f.States) != 2 {
		t.Fatalf("good project filter: %+v, %v", f, err)
	}
	f2, err := ParseProjectFilter(url.Values{})
	if err != nil || f2.Limit != defaultListLimit {
		t.Fatalf("default limit = %d, %v", f2.Limit, err)
	}
}

func TestProjectTimestamps(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	earlier := now.Add(-time.Hour)
	tests := []struct {
		name                         string
		state                        string
		in                           [3]*time.Time
		wantStarted, wantDone, wantX bool
	}{
		{"started sets startedAt", "started", [3]*time.Time{}, true, false, false},
		{"paused keeps startedAt", "paused", [3]*time.Time{&earlier, nil, nil}, true, false, false},
		{"completed sets both", "completed", [3]*time.Time{}, true, true, false},
		{"canceled clears completed", "canceled", [3]*time.Time{&earlier, &earlier, nil}, true, false, true},
		{"planned clears all", "planned", [3]*time.Time{&earlier, &earlier, &earlier}, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, d, x := projectTimestamps(tt.state, now, tt.in[0], tt.in[1], tt.in[2])
			if (s != nil) != tt.wantStarted || (d != nil) != tt.wantDone || (x != nil) != tt.wantX {
				t.Fatalf("got started=%v completed=%v canceled=%v", s, d, x)
			}
		})
	}
}
