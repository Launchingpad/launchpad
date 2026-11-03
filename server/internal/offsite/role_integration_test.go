//go:build backuptest

package offsite

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The worker runs under the restricted role of decisions-T16 #15: the whole flow
// (schedule, dispatch, success, retention, rotate-signing, secrets merge) works with
// only those grants, and other tables stay closed.
func TestRestrictedRole(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	role := "offsite_t_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	h.exec(`CREATE ROLE ` + role + ` LOGIN PASSWORD 'pw'`)
	t.Cleanup(func() {
		h.pool.Close()
	})
	var db string
	if err := h.pool.QueryRow(ctx, `SELECT current_database()`).Scan(&db); err != nil {
		t.Fatal(err)
	}
	h.exec(`GRANT CONNECT ON DATABASE ` + db + ` TO ` + role)
	h.exec(`GRANT USAGE ON SCHEMA public TO ` + role)
	h.exec(`GRANT SELECT ON backup_settings, backup_destinations, backup_dumps, backup_requests, backup_runs, schema_migrations TO ` + role)
	h.exec(`GRANT INSERT, UPDATE ON backup_runs TO ` + role)
	h.exec(`GRANT INSERT ON backup_requests, backup_change_log TO ` + role)
	h.exec(`GRANT UPDATE (secrets_enc, secrets_set) ON backup_destinations TO ` + role)
	h.exec(`GRANT UPDATE (signing_public, signing_private_enc) ON backup_settings TO ` + role)

	u, err := url.Parse(h.dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(role, "pw")
	dsn := u.String()
	pool, err := dbOpen(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	h.w = h.newWorker(pool, dsn, nil)
	dest := h.addDest("main")
	h.addDump(10*time.Minute, nil)
	// The app queues a run and a request; the worker needs the role's grants only.
	id := h.queue(dest, "manual")
	h.scan()
	if r := h.run(id); r.Status != "succeeded" {
		t.Fatalf("run under the restricted role: %+v\n%s", r, h.log.String())
	}
	// An old dump makes the worker file an extra dump request.
	h.exec(`UPDATE backup_dumps SET started_at = now() - interval '7 hours'`)
	id = h.queue(dest, "manual")
	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	h.w.Scan(wctx)
	h.waitStatus(id, 10*time.Second, "waiting_dump")
	var n int
	_ = h.pool.QueryRow(ctx, `SELECT count(*) FROM backup_requests`).Scan(&n)
	if n != 1 || h.run(id).Status != "waiting_dump" {
		t.Fatalf("extra dump request: %d %+v\n%s", n, h.run(id), h.log.String())
	}
	if _, err := rotateSigning(ctx, pool, h.w.keys); err != nil {
		t.Fatalf("rotate-signing under the role: %v", err)
	}
	if err := h.w.MergeSecrets(ctx, dest, func(m map[string]string) { m["refreshToken"] = "t" }); err != nil {
		t.Fatalf("merge secrets under the role: %v", err)
	}
	for _, q := range []string{
		`SELECT 1 FROM users LIMIT 1`,
		`SELECT 1 FROM platform_settings LIMIT 1`,
		`DELETE FROM backup_runs`,
		`UPDATE backup_destinations SET name = 'x'`,
		`UPDATE backup_settings SET recipients = '[]'`,
	} {
		if _, err := pool.Exec(ctx, q); err == nil {
			t.Fatalf("the role may run %q", q)
		}
	}
}

// A NOTIFY on backup_work wakes the loop at once (the scan interval is an hour).
func TestListen_WakesTheLoop(t *testing.T) {
	h := newHarness(t)
	dest := h.addDest("main")
	h.addDump(10*time.Minute, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.w.Run(ctx) }()
	h.waitFor("the first scan and the LISTEN", 10*time.Second, func() bool {
		var n int
		_ = h.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity WHERE query = 'LISTEN backup_work' OR query LIKE 'LISTEN backup_work%'`).Scan(&n)
		return n > 0
	})
	id := h.queue(dest, "manual")
	h.exec(`SELECT pg_notify('backup_work', '')`)
	h.waitStatus(id, 10*time.Second, "succeeded")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Boot waits for the migration: an unmigrated database is not ready, the context ends the wait.
func TestWaitReady_Cancel(t *testing.T) {
	h := newHarness(t)
	h.exec(`UPDATE schema_migrations SET version = 18`)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := h.w.waitReady(ctx); err == nil {
		t.Fatal("ready on an old schema")
	}
	h.exec(`UPDATE schema_migrations SET version = 20`)
	if err := h.w.waitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
}
