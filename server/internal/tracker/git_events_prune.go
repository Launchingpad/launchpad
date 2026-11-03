package tracker

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// GitEventRetention is how long git_webhook_events rows are kept
// (decisions-T17 #8): they are dedupe keys, and the hosts' retry windows are
// far shorter.
const GitEventRetention = 30 * 24 * time.Hour

// PruneGitWebhookEvents deletes the rows older than GitEventRetention as of
// now and returns how many it deleted.
func (s *Service) PruneGitWebhookEvents(ctx context.Context, now time.Time) (int64, error) {
	n, err := s.q.PruneGitWebhookEvents(ctx, now.Add(-GitEventRetention))
	if err != nil {
		return 0, fmt.Errorf("prune git webhook events: %w", err)
	}
	return n, nil
}

// RunGitEventPrune prunes once now (the boot run) and then every interval
// until ctx ends. A failed run is logged and the loop goes on.
func RunGitEventPrune(ctx context.Context, prune func(context.Context, time.Time) (int64, error),
	clock func() time.Time, interval time.Duration, log *slog.Logger) {
	once := func() {
		n, err := prune(ctx, clock())
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.ErrorContext(ctx, "git webhook event prune failed", "err", err)
		} else if n > 0 {
			log.InfoContext(ctx, "git webhook event prune", "deleted", n)
		}
	}
	once()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			once()
		}
	}
}
