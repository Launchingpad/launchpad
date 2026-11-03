package tracker_test

import (
	"context"
	"testing"
	"time"

	"devops/internal/testdb"
	"devops/internal/tracker"
)

// TestPruneGitWebhookEvents: rows older than 30 days go, newer ones stay,
// in every org (LAUN-82).
func TestPruneGitWebhookEvents(t *testing.T) {
	pool, _ := testdb.New(t)
	d := testdb.Fixture(t, pool)
	ctx := context.Background()
	var integ string
	if err := pool.QueryRow(ctx, `INSERT INTO git_integrations (org_id, base_url, webhook_secret_hash)
		VALUES ($1, 'https://gl.example', decode(repeat('aa', 32), 'hex')) RETURNING id::text`, d.OrgID).Scan(&integ); err != nil {
		t.Fatal(err)
	}
	add := func(key, age string) {
		if _, err := pool.Exec(ctx, `INSERT INTO git_webhook_events (org_id, integration_id, event_uuid, event_type, received_at)
			VALUES ($1, $2, $3, 'merge_request', now() - $4::interval)`, d.OrgID, integ, key, age); err != nil {
			t.Fatal(err)
		}
	}
	add("gl:ev:old", "31 days")
	add("gl:ev:older", "200 days")
	add("gl:ev:edge", "29 days")
	add("gl:ev:new", "1 hour")

	svc := tracker.New(pool)
	n, err := svc.PruneGitWebhookEvents(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("deleted %d, want 2", n)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM git_webhook_events WHERE event_uuid IN ('gl:ev:edge', 'gl:ev:new')`).Scan(&left); err != nil || left != 2 {
		t.Fatalf("kept %d of the 2 recent rows (%v)", left, err)
	}
}
