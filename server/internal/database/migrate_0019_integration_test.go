package database_test

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"devops/internal/database"
	"devops/internal/dbq"
	"devops/internal/testdb"
)

func backupConn(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func mustExec(t *testing.T, c *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := c.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func refuse(t *testing.T, c *pgx.Conn, name, code, sql string, args ...any) {
	t.Helper()
	if _, err := c.Exec(context.Background(), sql, args...); !database.IsPgCode(err, code) {
		t.Fatalf("%s: err %v, want SQLSTATE %s", name, err, code)
	}
}

// TestMigration0019_BackupTables: the single settings row, the CHECKs, the
// circular keys, SET NULL on delete and the down (LAUN-68, decisions-T16 #23).
func TestMigration0019_BackupTables(t *testing.T) {
	dsn := testdb.Fresh(t)
	m := migratorFor(t, dsn)
	migrateTo(t, m, 19)
	c := backupConn(t, dsn)
	ctx := context.Background()

	checkRow := func() {
		t.Helper()
		var n int
		var install, recipients string
		var ever, signingEmpty, keyNull bool
		var stale int
		var inst uuid.UUID
		if err := c.QueryRow(ctx, `SELECT count(*) OVER (), install_id, instance_id, recipients::text, ever_had_recipient,
			signing_public = '' AND signing_private_enc IS NULL, key_check IS NULL, stale_alert_hours FROM backup_settings`).
			Scan(&n, &install, &inst, &recipients, &ever, &signingEmpty, &keyNull, &stale); err != nil {
			t.Fatal(err)
		}
		if n != 1 || !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(install) || inst == uuid.Nil ||
			recipients != "[]" || ever || !signingEmpty || !keyNull || stale != 48 {
			t.Fatalf("settings row: n=%d install=%q instance=%s recipients=%s ever=%v empty=%v keyNull=%v stale=%d",
				n, install, inst, recipients, ever, signingEmpty, keyNull, stale)
		}
	}
	checkRow()

	cv := database.CodeCheckViolation
	refuse(t, c, "a second settings row", database.CodeUniqueViolation, `INSERT INTO backup_settings (id) VALUES (true)`)
	refuse(t, c, "id false", cv, `INSERT INTO backup_settings (id) VALUES (false)`)
	refuse(t, c, "short key check", cv, `UPDATE backup_settings SET key_check = '\x00'::bytea`)
	refuse(t, c, "long key check", cv, `UPDATE backup_settings SET key_check = decode(repeat('00', 33), 'hex')`)
	refuse(t, c, "stale alert too high", cv, `UPDATE backup_settings SET stale_alert_hours = 8761`)
	refuse(t, c, "stale alert negative", cv, `UPDATE backup_settings SET stale_alert_hours = -1`)
	refuse(t, c, "recipients not an array", cv, `UPDATE backup_settings SET recipients = '{}'`)
	refuse(t, c, "bad install id", cv, `UPDATE backup_settings SET install_id = 'ABCDEFGH'`)
	mustExec(t, c, `UPDATE backup_settings SET key_check = decode(repeat('ab', 32), 'hex')`)
	mustExec(t, c, `UPDATE backup_settings SET key_check = NULL`)
	mustExec(t, c, `UPDATE backup_settings SET stale_alert_hours = 0`)

	// Destinations.
	ins := `INSERT INTO backup_destinations (name, type, schedule, weekday, at_utc, keep_last, verify_every_days)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`
	mustExec(t, c, ins, "a", "s3", "daily", nil, "04:15", 14, 7)
	mustExec(t, c, ins, "b", "dropbox", "weekly", 6, "23:59", 365, 0)
	mustExec(t, c, ins, "c", "webdav", "weekly", 0, "00:00", 1, 90)
	mustExec(t, c, ins, "d", "sftp", "daily", nil, "04:15", 14, 7)
	refuse(t, c, "duplicate name", database.CodeUniqueViolation, ins, "a", "s3", "daily", nil, "04:15", 14, 7)
	refuse(t, c, "unknown type", cv, ins, "e", "ftp", "daily", nil, "04:15", 14, 7)
	refuse(t, c, "unknown schedule", cv, ins, "e", "s3", "hourly", nil, "04:15", 14, 7)
	refuse(t, c, "keep_last 0", cv, ins, "e", "s3", "daily", nil, "04:15", 0, 7)
	refuse(t, c, "keep_last 366", cv, ins, "e", "s3", "daily", nil, "04:15", 366, 7)
	refuse(t, c, "verify 91", cv, ins, "e", "s3", "daily", nil, "04:15", 14, 91)
	refuse(t, c, "verify -1", cv, ins, "e", "s3", "daily", nil, "04:15", 14, -1)
	refuse(t, c, "weekly without weekday", cv, ins, "e", "s3", "weekly", nil, "04:15", 14, 7)
	refuse(t, c, "daily with weekday", cv, ins, "e", "s3", "daily", 3, "04:15", 14, 7)
	refuse(t, c, "weekday 7", cv, ins, "e", "s3", "weekly", 7, "04:15", 14, 7)
	refuse(t, c, "at_utc 24:00", cv, ins, "e", "s3", "daily", nil, "24:00", 14, 7)
	refuse(t, c, "at_utc 4:15", cv, ins, "e", "s3", "daily", nil, "4:15", 14, 7)
	refuse(t, c, "name empty", cv, ins, "", "s3", "daily", nil, "04:15", 14, 7)
	var longName [61]byte
	for i := range longName {
		longName[i] = 'x'
	}
	refuse(t, c, "name 61", cv, ins, string(longName[:]), "s3", "daily", nil, "04:15", 14, 7)
	refuse(t, c, "config not an object", cv, `UPDATE backup_destinations SET config = '[]' WHERE name = 'a'`)
	// Weekday CHECK also holds on UPDATE, in both directions.
	refuse(t, c, "update to weekly without weekday", cv, `UPDATE backup_destinations SET schedule = 'weekly' WHERE name = 'a'`)
	refuse(t, c, "update weekday on daily", cv, `UPDATE backup_destinations SET weekday = 1 WHERE name = 'a'`)

	// Runs.
	var destA uuid.UUID
	if err := c.QueryRow(ctx, `SELECT id FROM backup_destinations WHERE name = 'a'`).Scan(&destA); err != nil {
		t.Fatal(err)
	}
	run := `INSERT INTO backup_runs (destination_id, trigger, status, finished_at, will_retry, locked_until)
		VALUES ($1, $2, $3, $4, $5, $6)`
	now := time.Now()
	refuse(t, c, "unknown trigger", cv, run, nil, "cron", "queued", nil, false, nil)
	refuse(t, c, "unknown status", cv, run, nil, "manual", "paused", nil, false, nil)
	refuse(t, c, "final without finished_at", cv, run, nil, "test", "failed", nil, false, nil)
	refuse(t, c, "queued with finished_at", cv, run, nil, "test", "queued", now, false, nil)
	refuse(t, c, "will_retry on succeeded", cv, run, nil, "test", "succeeded", now, true, nil)
	refuse(t, c, "will_retry on queued", cv, run, nil, "test", "queued", nil, true, nil)
	refuse(t, c, "lease on a queued run", cv, run, nil, "test", "queued", nil, false, now)
	refuse(t, c, "lease on a final run", cv, run, nil, "test", "canceled", now, false, now)
	for _, st := range []string{"succeeded", "failed", "interrupted", "canceled"} {
		mustExec(t, c, run, nil, "test", st, now, false, nil)
	}
	mustExec(t, c, run, nil, "test", "failed", now, true, nil)
	mustExec(t, c, run, nil, "probe", "running", nil, false, now.Add(time.Minute))
	mustExec(t, c, run, nil, "exchange", "waiting_dump", nil, false, now.Add(time.Minute))

	// The circular keys: a dump names its request, the request names its dump,
	// and a waiting_dump run names the request.
	var reqID, dumpID, runID uuid.UUID
	if err := c.QueryRow(ctx, `INSERT INTO backup_requests (state, requested_by) VALUES ('queued', 'app') RETURNING id`).Scan(&reqID); err != nil {
		t.Fatal(err)
	}
	if err := c.QueryRow(ctx, `INSERT INTO backup_dumps (file_name, status, request_id) VALUES ('d1.dump', 'running', $1) RETURNING id`, reqID).Scan(&dumpID); err != nil {
		t.Fatal(err)
	}
	mustExec(t, c, `UPDATE backup_requests SET dump_id = $1, state = 'taken' WHERE id = $2`, dumpID, reqID)
	if err := c.QueryRow(ctx, `INSERT INTO backup_runs (destination_id, trigger, status, request_id, dump_id)
		VALUES ($1, 'manual', 'waiting_dump', $2, NULL) RETURNING id`, destA, reqID).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	refuse(t, c, "unknown request state", cv, `INSERT INTO backup_requests (state, requested_by) VALUES ('open', 'app')`)
	refuse(t, c, "unknown requester", cv, `INSERT INTO backup_requests (state, requested_by) VALUES ('queued', 'cron')`)
	refuse(t, c, "unknown dump status", cv, `INSERT INTO backup_dumps (file_name, status) VALUES ('d2.dump', 'done')`)
	refuse(t, c, "duplicate dump file", database.CodeUniqueViolation, `INSERT INTO backup_dumps (file_name, status) VALUES ('d1.dump', 'ok')`)
	refuse(t, c, "dangling request", database.CodeForeignKeyViolation, `INSERT INTO backup_dumps (file_name, status, request_id) VALUES ('d3.dump', 'ok', gen_random_uuid())`)
	// Deleting the request clears both pointers; deleting the dump clears the others.
	mustExec(t, c, `DELETE FROM backup_requests WHERE id = $1`, reqID)
	var reqNull bool
	if err := c.QueryRow(ctx, `SELECT (SELECT request_id IS NULL FROM backup_dumps WHERE id = $1) AND
		(SELECT request_id IS NULL FROM backup_runs WHERE id = $2)`, dumpID, runID).Scan(&reqNull); err != nil || !reqNull {
		t.Fatalf("request delete did not clear the keys: %v %v", reqNull, err)
	}

	// key_check / change-log kind length.
	refuse(t, c, "empty change kind", cv, `INSERT INTO backup_change_log (kind) VALUES ('')`)

	// A destination delete keeps its runs and change-log rows with the name.
	q := dbq.New(c)
	r, err := q.InsertBackupRun(ctx, dbq.InsertBackupRunParams{ID: uuid.Must(uuid.NewV7()), DestinationID: &destA, Trigger: "test", Status: "queued", Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if r.DestinationName != "a" {
		t.Fatalf("destination_name = %q, want a", r.DestinationName)
	}
	free, err := q.InsertBackupRun(ctx, dbq.InsertBackupRunParams{ID: uuid.Must(uuid.NewV7()), Trigger: "probe", Status: "queued", Attempt: 1})
	if err != nil || free.DestinationName != "" {
		t.Fatalf("a run with no destination: %q %v", free.DestinationName, err)
	}
	mustExec(t, c, `INSERT INTO backup_change_log (kind, destination_id, destination_name) VALUES ('destination_added', $1, 'a')`, destA)
	if n, err := q.DeleteBackupDestination(ctx, destA); err != nil || n != 1 {
		t.Fatalf("delete: %d %v", n, err)
	}
	var kept int
	var name string
	var idNull bool
	if err := c.QueryRow(ctx, `SELECT count(*), max(destination_name), bool_and(destination_id IS NULL) FROM backup_runs WHERE id = $1`, r.ID).Scan(&kept, &name, &idNull); err != nil {
		t.Fatal(err)
	}
	if kept != 1 || name != "a" || !idNull {
		t.Fatalf("run after destination delete: kept=%d name=%q idNull=%v", kept, name, idNull)
	}
	var logKept int
	if err := c.QueryRow(ctx, `SELECT count(*) FROM backup_change_log WHERE kind = 'destination_added' AND destination_id IS NULL AND destination_name = 'a'`).Scan(&logKept); err != nil || logKept != 1 {
		t.Fatalf("change log after destination delete: %d %v", logKept, err)
	}

	// updated_by is cleared when the user goes.
	mustExec(t, c, `WITH u AS (INSERT INTO users (username, name) VALUES ('gone', 'Gone') RETURNING id)
		UPDATE backup_settings SET updated_by = (SELECT id FROM u)`)
	mustExec(t, c, `DELETE FROM users WHERE username = 'gone'`)
	var cleared bool
	if err := c.QueryRow(ctx, `SELECT updated_by IS NULL FROM backup_settings`).Scan(&cleared); err != nil || !cleared {
		t.Fatalf("updated_by not cleared: %v %v", cleared, err)
	}

	// Down removes every table and the function; up gives the row back.
	_ = c.Close(ctx)
	migrateTo(t, m, 18)
	c = backupConn(t, dsn)
	var left int
	if err := c.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tablename LIKE 'backup\_%')
		+ (SELECT count(*) FROM pg_proc WHERE proname = 'backup_settings_signing_guard')`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("down left %d objects: %v", left, err)
	}
	migrateTo(t, m, 19)
	checkRow()
}

// TestMigration0019_SigningKeyGuard: the trigger of decisions-T16 #22 and the
// compare-and-set of the first use.
func TestMigration0019_SigningKeyGuard(t *testing.T) {
	dsn := testdb.Fresh(t)
	migrateTo(t, migratorFor(t, dsn), 19)
	c := backupConn(t, dsn)
	ctx := context.Background()
	cv := database.CodeCheckViolation
	blob := `decode(repeat('ab', 40), 'hex')`

	// The empty state: no-op updates, key_check and other columns are free.
	mustExec(t, c, `UPDATE backup_settings SET signing_public = '', signing_private_enc = NULL`)
	mustExec(t, c, `UPDATE backup_settings SET key_check = decode(repeat('01', 32), 'hex'), notify_on_success = true`)
	// A half-empty first write and a blank are refused.
	refuse(t, c, "public only", cv, `UPDATE backup_settings SET signing_public = 'PUB1'`)
	refuse(t, c, "private only", cv, `UPDATE backup_settings SET signing_private_enc = `+blob)

	// Two concurrent first writers: one row changes, the other none.
	cas := `UPDATE backup_settings SET signing_public = $1, signing_private_enc = ` + blob + `
		WHERE signing_public = '' AND signing_private_enc IS NULL`
	c2 := backupConn(t, dsn)
	tx1, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tag1, err := tx1.Exec(ctx, cas, "PUB1")
	if err != nil || tag1.RowsAffected() != 1 {
		t.Fatalf("first writer: %v %v", tag1, err)
	}
	type res struct {
		n   int64
		err error
	}
	done := make(chan res, 1)
	go func() {
		tag, err := c2.Exec(ctx, cas, "PUB2")
		done <- res{tag.RowsAffected(), err}
	}()
	select {
	case r := <-done:
		t.Fatalf("the second writer did not wait for the first: %+v", r)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.err != nil || r.n != 0 {
		t.Fatalf("second writer: rows=%d err=%v, want 0 rows", r.n, r.err)
	}
	var pub string
	if err := c.QueryRow(ctx, `SELECT signing_public FROM backup_settings`).Scan(&pub); err != nil || pub != "PUB1" {
		t.Fatalf("stored key %q %v", pub, err)
	}

	// From a non-empty key: refused without a change-log row.
	refuse(t, c, "replace without a log row", cv, `UPDATE backup_settings SET signing_public = 'PUB3', signing_private_enc = `+blob)
	refuse(t, c, "blank public", cv, `UPDATE backup_settings SET signing_public = ''`)
	refuse(t, c, "blank private", cv, `UPDATE backup_settings SET signing_private_enc = NULL`)
	refuse(t, c, "blank both", cv, `UPDATE backup_settings SET signing_public = '', signing_private_enc = NULL`)
	// A log row of another kind, or from an earlier transaction, does not count.
	mustExec(t, c, `INSERT INTO backup_change_log (kind) VALUES ('signing_key_replaced')`)
	mustExec(t, c, `INSERT INTO backup_change_log (kind) VALUES ('destination_added')`)
	refuse(t, c, "replace with an old log row", cv, `UPDATE backup_settings SET signing_public = 'PUB3', signing_private_enc = `+blob)
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO backup_change_log (kind) VALUES ('destination_added')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE backup_settings SET signing_public = 'PUB3', signing_private_enc = `+blob); !database.IsPgCode(err, cv) {
		t.Fatalf("replace with a log row of another kind: %v", err)
	}
	_ = tx.Rollback(ctx)
	// The same transaction as the row: allowed. Even then a blank is not.
	tx, err = c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO backup_change_log (kind) VALUES ('signing_key_replaced')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE backup_settings SET signing_public = '', signing_private_enc = NULL`); !database.IsPgCode(err, cv) {
		t.Fatalf("blank with a log row: %v", err)
	}
	_ = tx.Rollback(ctx)
	tx, err = c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO backup_change_log (kind) VALUES ('signing_key_replaced')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE backup_settings SET signing_public = 'PUB3', signing_private_enc = `+blob); err != nil {
		t.Fatalf("replace with a log row in the same transaction: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.QueryRow(ctx, `SELECT signing_public FROM backup_settings`).Scan(&pub); err != nil || pub != "PUB3" {
		t.Fatalf("stored key after replace %q %v", pub, err)
	}
	// key_check alone does not fire the trigger, with a key present and no log row in this transaction.
	mustExec(t, c, `UPDATE backup_settings SET key_check = decode(repeat('02', 32), 'hex')`)
	mustExec(t, c, `UPDATE backup_settings SET key_check = NULL`)
	// An UPDATE that names the columns but changes nothing is a no-op.
	mustExec(t, c, `UPDATE backup_settings SET signing_public = signing_public, signing_private_enc = signing_private_enc`)

	// The function is SECURITY DEFINER with a fixed search_path.
	var secdef bool
	var cfg []string
	if err := c.QueryRow(ctx, `SELECT prosecdef, proconfig FROM pg_proc WHERE proname = 'backup_settings_signing_guard'`).Scan(&secdef, &cfg); err != nil {
		t.Fatal(err)
	}
	if !secdef || len(cfg) != 1 || cfg[0] != "search_path=pg_catalog, pg_temp" {
		t.Fatalf("function: secdef=%v config=%v", secdef, cfg)
	}
}

// TestMigration0019_OneActiveRunPerDestination: the busy rule of decisions-T16
// #31, as a partial unique index.
func TestMigration0019_OneActiveRunPerDestination(t *testing.T) {
	dsn := testdb.Fresh(t)
	migrateTo(t, migratorFor(t, dsn), 19)
	c := backupConn(t, dsn)
	ctx := context.Background()
	var dest, other uuid.UUID
	if err := c.QueryRow(ctx, `INSERT INTO backup_destinations (name, type) VALUES ('a', 's3') RETURNING id`).Scan(&dest); err != nil {
		t.Fatal(err)
	}
	if err := c.QueryRow(ctx, `INSERT INTO backup_destinations (name, type) VALUES ('b', 's3') RETURNING id`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO backup_runs (destination_id, trigger, status, finished_at) VALUES ($1, $2, $3,
		CASE WHEN $3 IN ('succeeded', 'failed', 'interrupted', 'canceled') THEN now() END)`
	insNothing := ins + ` ON CONFLICT DO NOTHING`

	// Two concurrent inserts leave one row.
	c2 := backupConn(t, dsn)
	tx1, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx1.Exec(ctx, insNothing, dest, "manual", "queued"); err != nil {
		t.Fatal(err)
	}
	type res struct {
		n   int64
		err error
	}
	done := make(chan res, 1)
	go func() {
		tag, err := c2.Exec(ctx, insNothing, dest, "schedule", "queued")
		done <- res{tag.RowsAffected(), err}
	}()
	select {
	case r := <-done:
		t.Fatalf("the second insert did not wait: %+v", r)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx1.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.err != nil || r.n != 0 {
		t.Fatalf("second insert: rows=%d err=%v, want 0 rows", r.n, r.err)
	}
	var n int
	if err := c.QueryRow(ctx, `SELECT count(*) FROM backup_runs WHERE destination_id = $1`, dest).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d %v, want 1", n, err)
	}
	// Without ON CONFLICT it is a unique violation, for every backup trigger and active status.
	for _, trg := range []string{"schedule", "manual", "catchup", "retry"} {
		for _, st := range []string{"queued", "waiting_dump", "running"} {
			refuse(t, c, trg+"/"+st, database.CodeUniqueViolation, ins, dest, trg, st)
		}
	}
	// Another destination, and runs that are not backup runs, are not limited.
	mustExec(t, c, ins, other, "manual", "queued")
	for _, trg := range []string{"probe", "test", "exchange", "verify"} {
		mustExec(t, c, ins, dest, trg, "queued")
		mustExec(t, c, ins, dest, trg, "running")
	}
	// A destination-less probe is not limited either.
	mustExec(t, c, ins, nil, "probe", "queued")
	mustExec(t, c, ins, nil, "probe", "queued")

	// A failed run with its queued retry: the parent is final, so the retry fits.
	mustExec(t, c, `UPDATE backup_runs SET status = 'failed', finished_at = now(), will_retry = true
		WHERE destination_id = $1 AND trigger = 'manual'`, dest)
	mustExec(t, c, ins, dest, "retry", "queued")
	// A final run frees the slot.
	mustExec(t, c, `UPDATE backup_runs SET status = 'canceled', finished_at = now() WHERE destination_id = $1 AND trigger = 'retry'`, dest)
	mustExec(t, c, ins, dest, "schedule", "queued")
}
