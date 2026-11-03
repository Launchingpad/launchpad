package backupapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"devops/internal/testdb"
)

// LAUN-75: the hourly row cleanup (decisions-T16 #29) against a real Postgres.
// The clock is injected; no test reads time.Now.

type cleanClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *cleanClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }

var cleanNow = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func cleanEnv(t *testing.T) (*Service, *pgxpool.Pool) {
	t.Helper()
	pool, _ := testdb.New(t)
	svc, err := New(pool, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{Clock: (&cleanClock{t: cleanNow}).Now})
	if err != nil {
		t.Fatal(err)
	}
	return svc, pool
}

func ago(d time.Duration) time.Time { return cleanNow.Add(-d) }

const day = 24 * time.Hour

func count(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func TestCleanup_RunsOnlyPastTheirAge(t *testing.T) {
	svc, pool := cleanEnv(t)
	ctx := context.Background()
	mk := func(name, status string, finished *time.Time, queued time.Time) {
		testdb.MustExec(t, pool, `INSERT INTO backup_runs (destination_name, trigger, status, queued_at, finished_at)
			VALUES ($1, 'schedule', $2, $3, $4)`, name, status, queued, finished)
	}
	f181, f179 := ago(181*day), ago(179*day)
	mk("old-failed", "failed", &f181, f181)
	mk("old-succeeded", "succeeded", &f181, f181)
	mk("old-interrupted", "interrupted", &f181, f181)
	mk("young-failed", "failed", &f179, f179)
	// A run that never finished is not deleted by age (it is a stuck run, not an old one).
	mk("old-queued", "queued", nil, f181)
	res, err := svc.Cleanup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Runs != 3 {
		t.Fatalf("deleted %d runs, want 3", res.Runs)
	}
	for name, want := range map[string]int{"young-failed": 1, "old-queued": 1, "old-failed": 0, "old-succeeded": 0, "old-interrupted": 0} {
		if got := count(t, pool, `SELECT count(*) FROM backup_runs WHERE destination_name = $1`, name); got != want {
			t.Errorf("%s: %d rows, want %d", name, got, want)
		}
	}
	res, err = svc.Cleanup(ctx)
	if err != nil || res.total() != 0 {
		t.Fatalf("a second pass changed %d rows: %v", res.total(), err)
	}
}

func TestCleanup_ManyRowsGoInBatches(t *testing.T) {
	svc, pool := cleanEnv(t)
	testdb.MustExec(t, pool, `INSERT INTO backup_runs (destination_name, trigger, status, queued_at, finished_at)
		SELECT 'bulk', 'schedule', 'failed', $1::timestamptz, $1::timestamptz FROM generate_series(1, 2600)`, ago(200*day))
	res, err := svc.Cleanup(context.Background())
	if err != nil || res.Runs != 2600 {
		t.Fatalf("deleted %d: %v", res.Runs, err)
	}
	if n := count(t, pool, `SELECT count(*) FROM backup_runs`); n != 0 {
		t.Fatalf("%d left", n)
	}
}

func TestCleanup_Dumps(t *testing.T) {
	svc, pool := cleanEnv(t)
	mk := func(name, status string, started time.Time, finished, removed *time.Time) {
		testdb.MustExec(t, pool, `INSERT INTO backup_dumps (file_name, status, started_at, finished_at, file_removed_at)
			VALUES ($1, $2, $3, $4, $5)`, name, status, started, finished, removed)
	}
	r181, r179 := ago(181*day), ago(179*day)
	mk("removed-181", "ok", r181, &r181, &r181)
	mk("removed-179", "ok", r179, &r179, &r179)
	mk("failed-181", "failed", r181, &r181, nil)
	mk("failed-179", "failed", r179, &r179, nil)
	recent := ago(time.Hour)
	mk("ok-recent", "ok", recent, &recent, nil)
	mk("ok-old-file-kept", "ok", r181, &r181, nil) // an ok dump whose file still exists is never deleted
	res, err := svc.Cleanup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Dumps != 2 {
		t.Fatalf("deleted %d dumps, want 2", res.Dumps)
	}
	for name, want := range map[string]int{"removed-181": 0, "failed-181": 0, "removed-179": 1, "failed-179": 1, "ok-recent": 1, "ok-old-file-kept": 1} {
		if got := count(t, pool, `SELECT count(*) FROM backup_dumps WHERE file_name = $1`, name); got != want {
			t.Errorf("%s: %d, want %d", name, got, want)
		}
	}
}

func TestCleanup_StuckDumpsAndRequests(t *testing.T) {
	svc, pool := cleanEnv(t)
	ctx := context.Background()
	state := func(sql string, args ...any) string {
		var s string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	// A dump running for 7 hours is failed; one running for 5 hours is left.
	testdb.MustExec(t, pool, `INSERT INTO backup_dumps (file_name, status, started_at) VALUES ('stuck', 'running', $1), ('slow', 'running', $2)`,
		ago(7*time.Hour), ago(5*time.Hour))
	// A request taken 3 hours ago with no running dump: failed. With a running dump: left.
	var slowDump, stuckDump uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM backup_dumps WHERE file_name = 'slow'`).Scan(&slowDump); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM backup_dumps WHERE file_name = 'stuck'`).Scan(&stuckDump); err != nil {
		t.Fatal(err)
	}
	mkReq := func(id uuid.UUID, st string, takenAgo time.Duration, dump *uuid.UUID) {
		testdb.MustExec(t, pool, `INSERT INTO backup_requests (id, state, requested_by, requested_at, taken_at, dump_id) VALUES ($1, $2, 'app', $3, $4, $5)`,
			id, st, ago(takenAgo+time.Minute), ago(takenAgo), dump)
	}
	noDump, withSlow, withStuck, fresh := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	mkReq(noDump, "taken", 3*time.Hour, nil)
	mkReq(withSlow, "taken", 3*time.Hour, &slowDump)
	mkReq(withStuck, "taken", 8*time.Hour, &stuckDump)
	mkReq(fresh, "taken", time.Hour, nil)
	res, err := svc.Cleanup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.DumpsFailed != 1 || res.RequestsFailed != 2 {
		t.Fatalf("failed %d dumps, %d requests; want 1 and 2", res.DumpsFailed, res.RequestsFailed)
	}
	if s := state(`SELECT status FROM backup_dumps WHERE file_name = 'stuck'`); s != "failed" {
		t.Errorf("stuck dump: %s", s)
	}
	if s := state(`SELECT status FROM backup_dumps WHERE file_name = 'slow'`); s != "running" {
		t.Errorf("slow dump: %s", s)
	}
	for id, want := range map[uuid.UUID]string{noDump: "failed", withSlow: "taken", withStuck: "failed", fresh: "taken"} {
		if s := state(`SELECT state FROM backup_requests WHERE id = $1`, id); s != want {
			t.Errorf("request %s: %s, want %s", id, s, want)
		}
	}
	if s := state(`SELECT error_text FROM backup_dumps WHERE file_name = 'stuck'`); s != "the dump did not finish in 6 hours" {
		t.Errorf("error_text: %q", s)
	}
	// A request that backup.sh already finished is never flipped back.
	done := uuid.Must(uuid.NewV7())
	testdb.MustExec(t, pool, `INSERT INTO backup_requests (id, state, requested_by, requested_at, taken_at, finished_at) VALUES ($1, 'done', 'app', $2, $2, $2)`, done, ago(5*time.Hour))
	if _, err := svc.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if s := state(`SELECT state FROM backup_requests WHERE id = $1`, done); s != "done" {
		t.Errorf("a done request became %s", s)
	}
}

func TestCleanup_RequestsAndChangeLogPastTheirAge(t *testing.T) {
	svc, pool := cleanEnv(t)
	mk := func(st string, a time.Duration) {
		testdb.MustExec(t, pool, `INSERT INTO backup_requests (state, requested_by, requested_at, finished_at) VALUES ($1, 'offsite', $2,
			CASE WHEN $1 IN ('done','failed') THEN $2::timestamptz END)`, st, ago(a))
	}
	mk("done", 8*day)
	mk("failed", 8*day)
	mk("done", 6*day)
	mk("queued", 30*day) // not final: never deleted by age
	chg := func(name string, a time.Duration) {
		testdb.MustExec(t, pool, `INSERT INTO backup_change_log (at, kind, destination_name) VALUES ($1, 'destination_added', $2)`, ago(a), name)
	}
	chg("c731", 731*day)
	chg("c729", 729*day)
	res, err := svc.Cleanup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Requests != 2 || res.ChangeLog != 1 {
		t.Fatalf("deleted %d requests and %d change rows; want 2 and 1", res.Requests, res.ChangeLog)
	}
	if n := count(t, pool, `SELECT count(*) FROM backup_requests`); n != 2 {
		t.Errorf("%d requests left", n)
	}
	if n := count(t, pool, `SELECT count(*) FROM backup_change_log WHERE destination_name = 'c729'`); n != 1 {
		t.Errorf("the 729 day row is gone")
	}
}

func TestCleanup_TestSecretsAndPassedRecipients(t *testing.T) {
	svc, pool := cleanEnv(t)
	ctx := context.Background()
	blob := bytes.Repeat([]byte{9}, 40)
	testdb.MustExec(t, pool, `INSERT INTO backup_runs (destination_name, trigger, status, queued_at, test_secrets_enc) VALUES
		('old', 'test', 'queued', $1, $3), ('young', 'test', 'queued', $2, $3)`, ago(11*time.Minute), ago(9*time.Minute), blob)
	mkr := func(fp string, effective time.Time, removeAt *time.Time) storedRecipient {
		return storedRecipient{PublicKey: "age1" + fp, Fingerprint: fp, AddedAt: ago(40 * day), EffectiveAt: effective, RemoveAt: removeAt}
	}
	past, future := ago(time.Hour), cleanNow.Add(time.Hour)
	pad := func(c string) string { return fmt.Sprintf("%064s", c) }
	list := []storedRecipient{
		mkr(pad("a"), ago(30*day), nil),             // active
		mkr(pad("b"), ago(30*day), &past),           // removal passed: dropped
		mkr(pad("c"), ago(30*day), &future),         // removal pending: kept
		mkr(pad("d"), cleanNow.Add(time.Hour), nil), // add pending: kept
	}
	raw, _ := json.Marshal(list)
	testdb.MustExec(t, pool, `UPDATE backup_settings SET recipients = $1, ever_had_recipient = true`, raw)
	res, err := svc.Cleanup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.TestSecrets != 1 || res.Recipients != 1 {
		t.Fatalf("cleared %d secrets, dropped %d recipients; want 1 and 1", res.TestSecrets, res.Recipients)
	}
	var got []storedRecipient
	var b []byte
	if err := pool.QueryRow(ctx, `SELECT recipients FROM backup_settings`).Scan(&b); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Fingerprint != pad("a") || got[1].Fingerprint != pad("c") || got[2].Fingerprint != pad("d") {
		t.Fatalf("recipients after cleanup: %+v", got)
	}
	if n := count(t, pool, `SELECT count(*) FROM backup_runs WHERE test_secrets_enc IS NOT NULL AND destination_name = 'young'`); n != 1 {
		t.Errorf("a young secret was cleared")
	}
	// ever_had_recipient survives, and a cleanup writes no change-log row.
	if n := count(t, pool, `SELECT count(*) FROM backup_settings WHERE ever_had_recipient`); n != 1 {
		t.Errorf("ever_had_recipient was reset")
	}
	if n := count(t, pool, `SELECT count(*) FROM backup_change_log`); n != 0 {
		t.Errorf("%d change-log rows written", n)
	}
}

func TestCleanupOnce_TwoInstancesDoNotRunTogether(t *testing.T) {
	svc, pool := cleanEnv(t)
	ctx := context.Background()
	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	var got bool
	if err := holder.QueryRow(ctx, cleanupLockSQL).Scan(&got); err != nil || !got {
		t.Fatalf("take the lock: %v %v", got, err)
	}
	_, locked, err := svc.CleanupOnce(ctx)
	if err != nil || locked {
		t.Fatalf("a second instance ran: locked=%v err=%v", locked, err)
	}
	var ok bool
	if err := holder.QueryRow(ctx, cleanupUnlockSQL).Scan(&ok); err != nil || !ok {
		t.Fatal("unlock")
	}
	if _, locked, err = svc.CleanupOnce(ctx); err != nil || !locked {
		t.Fatalf("after the other finished: locked=%v err=%v", locked, err)
	}
}
