package notify

import (
	"context"
	"errors"
	"testing"
	"time"
)

func noPause(context.Context) error { return nil }

func TestBatches_StopsOnShortBatchAndSumsRows(t *testing.T) {
	calls := 0
	seq := []int64{RetentionBatch, RetentionBatch, 5}
	n, err := batches(context.Background(), noPause, func() (int64, error) { calls++; return seq[calls-1], nil })
	if err != nil || n != 2*RetentionBatch+5 || calls != 3 {
		t.Errorf("got n=%d calls=%d err=%v", n, calls, err)
	}
}

func TestBatches_ErrorAndCancel(t *testing.T) {
	boom := errors.New("boom")
	if _, err := batches(context.Background(), noPause, func() (int64, error) { return 3, boom }); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, err := batches(ctx, noPause, func() (int64, error) { calls++; cancel(); return RetentionBatch, nil })
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Errorf("err=%v calls=%d", err, calls)
	}
}

// The pause runs between full batches only: two full batches then a short
// one make two pauses; an error from the pause (cancelled ctx) stops the loop.
func TestBatches_PauseBetweenFullBatches(t *testing.T) {
	pauses, calls := 0, 0
	seq := []int64{RetentionBatch, RetentionBatch, 5}
	_, err := batches(context.Background(), func(context.Context) error { pauses++; return nil },
		func() (int64, error) { calls++; return seq[calls-1], nil })
	if err != nil || pauses != 2 || calls != 3 {
		t.Errorf("pauses=%d calls=%d err=%v", pauses, calls, err)
	}
	stop := errors.New("stop")
	calls = 0
	_, err = batches(context.Background(), func(context.Context) error { return stop },
		func() (int64, error) { calls++; return RetentionBatch, nil })
	if !errors.Is(err, stop) || calls != 1 {
		t.Errorf("err=%v calls=%d", err, calls)
	}
}

func TestSleepCtx(t *testing.T) {
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Error(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}
