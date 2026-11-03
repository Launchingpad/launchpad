package events

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// BackfillWithHook is Backfill with a hook after each batch, for the
// crash-and-resume test (package events_test).
func BackfillWithHook(ctx context.Context, pool *pgxpool.Pool, o BackfillOptions, hook func(batch int) error) (BackfillResult, bool, error) {
	o.afterBatch = hook
	return Backfill(ctx, pool, o)
}
