-- Scheduled backups to remote storage (LAUN-68, docs/internal/decisions-T16-backups.md).
-- The tables are install-level: no org_id. No query here returns a secret in
-- clear; the sealed columns come back as bytea and are opened in Go.

-- name: GetBackupSettings :one
-- scope: none
SELECT * FROM backup_settings WHERE id;

-- name: GetBackupSettingsForUpdate :one
-- scope: none
-- A change of the recipients or the notice settings reads the row under this lock.
SELECT * FROM backup_settings WHERE id FOR UPDATE;

-- name: ClaimBackupSigningKey :execrows
-- scope: none
-- First use is compare-and-set (#22): of two writers one changes the row, the
-- other changes none and reads the stored key.
UPDATE backup_settings SET
    signing_public      = sqlc.arg(signing_public),
    signing_private_enc = sqlc.arg(signing_private_enc)
WHERE id AND signing_public = '' AND signing_private_enc IS NULL;

-- name: SetBackupKeyCheck :execrows
-- scope: none
-- The app only (#12): stores the check of the current key on the first
-- successful seal, and replaces a different one in the same transaction.
-- Changes no row when the stored check is already this one.
UPDATE backup_settings SET key_check = sqlc.arg(key_check)
WHERE id AND key_check IS DISTINCT FROM sqlc.arg(key_check)::bytea;

-- name: InsertBackupDestination :one
-- scope: none
INSERT INTO backup_destinations (id, name, type, config, secrets_enc, secrets_set, schedule, at_utc, weekday,
    keep_last, verify_every_days, updated_by)
VALUES (sqlc.arg(id), sqlc.arg(name), sqlc.arg(type), sqlc.arg(config), sqlc.narg(secrets_enc), sqlc.arg(secrets_set),
    sqlc.arg(schedule), sqlc.arg(at_utc), sqlc.narg(weekday), sqlc.arg(keep_last), sqlc.arg(verify_every_days),
    sqlc.narg(updated_by))
RETURNING *;

-- name: GetBackupDestination :one
-- scope: none
SELECT * FROM backup_destinations WHERE id = $1;

-- name: GetBackupDestinationForUpdate :one
-- scope: none
-- A secret update reads, merges and seals under this lock (T9 #15).
SELECT * FROM backup_destinations WHERE id = $1 FOR UPDATE;

-- name: ListBackupDestinations :many
-- scope: none
SELECT * FROM backup_destinations ORDER BY created_at, id;

-- name: SetBackupDestinationSecrets :one
-- scope: none
UPDATE backup_destinations SET
    secrets_enc = sqlc.narg(secrets_enc),
    secrets_set = sqlc.arg(secrets_set),
    updated_at  = now(),
    updated_by  = sqlc.narg(updated_by)
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: DeleteBackupDestination :execrows
-- scope: none
-- Its runs and change-log rows stay, with destination_name (ON DELETE SET NULL).
DELETE FROM backup_destinations WHERE id = $1;

-- name: InsertBackupRun :one
-- scope: none
-- destination_name is copied from the destination here and kept after a delete;
-- a run with no destination (an unsaved-form probe or test) gets ''.
INSERT INTO backup_runs (id, destination_id, destination_name, dump_id, request_id, trigger, status, attempt,
    retry_at, test_config, test_secrets_enc)
VALUES (sqlc.arg(id), sqlc.narg(destination_id),
    COALESCE((SELECT d.name FROM backup_destinations d WHERE d.id = sqlc.narg(destination_id)::uuid), ''),
    sqlc.narg(dump_id), sqlc.narg(request_id), sqlc.arg(trigger), sqlc.arg(status), sqlc.arg(attempt),
    sqlc.narg(retry_at), sqlc.narg(test_config), sqlc.narg(test_secrets_enc))
RETURNING *;

-- name: GetBackupRun :one
-- scope: none
SELECT * FROM backup_runs WHERE id = $1;

-- name: GetBackupRunForUpdate :one
-- scope: none
SELECT * FROM backup_runs WHERE id = $1 FOR UPDATE;

-- name: ListBackupRuns :many
-- scope: none
SELECT * FROM backup_runs ORDER BY queued_at DESC, id DESC LIMIT sqlc.arg(row_limit);

-- name: InsertBackupDump :one
-- scope: none
INSERT INTO backup_dumps (file_name, status, request_id) VALUES ($1, $2, sqlc.narg(request_id)) RETURNING *;

-- name: InsertBackupRequest :one
-- scope: none
INSERT INTO backup_requests (state, requested_by, requested_by_user)
VALUES ('queued', sqlc.arg(requested_by), sqlc.narg(requested_by_user)) RETURNING *;

-- name: InsertBackupChangeLog :one
-- scope: none
INSERT INTO backup_change_log (id, actor_id, kind, destination_id, destination_name, detail)
VALUES (sqlc.arg(id), sqlc.narg(actor_id), sqlc.arg(kind), sqlc.narg(destination_id), sqlc.arg(destination_name), sqlc.arg(detail))
RETURNING *;

-- LAUN-69: the backups API (decisions-T16 #43).

-- name: UpdateBackupRecipients :one
-- scope: none
-- The caller holds GetBackupSettingsForUpdate. updated_by is NULL for a host command.
UPDATE backup_settings SET
    recipients         = sqlc.arg(recipients),
    ever_had_recipient = sqlc.arg(ever_had_recipient),
    updated_at         = now(),
    updated_by         = sqlc.narg(updated_by)
WHERE id
RETURNING *;

-- name: UpdateBackupNotice :one
-- scope: none
UPDATE backup_settings SET
    notify_on_failure = sqlc.arg(notify_on_failure),
    notify_on_success = sqlc.arg(notify_on_success),
    stale_alert_hours = sqlc.arg(stale_alert_hours),
    updated_at        = now(),
    updated_by        = sqlc.narg(updated_by)
WHERE id
RETURNING *;

-- name: UpdateBackupDestination :one
-- scope: none
-- Every field except the secrets (SetBackupDestinationSecrets) and the type. enabled_since is
-- stamped each time the destination goes from disabled to enabled.
UPDATE backup_destinations SET
    name              = sqlc.arg(name),
    config            = sqlc.arg(config),
    schedule          = sqlc.arg(schedule),
    at_utc            = sqlc.arg(at_utc),
    weekday           = sqlc.narg(weekday),
    keep_last         = sqlc.arg(keep_last),
    verify_every_days = sqlc.arg(verify_every_days),
    enabled_since     = CASE WHEN sqlc.arg(enabled)::boolean AND NOT enabled THEN now() ELSE enabled_since END,
    enabled           = sqlc.arg(enabled),
    updated_at        = now(),
    updated_by        = sqlc.narg(updated_by)
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: CountEnabledBackupDestinations :one
-- scope: none
SELECT count(*) FROM backup_destinations WHERE enabled;

-- name: ListBackupDestinationLastRuns :many
-- scope: none
-- The newest backup run (schedule, manual, catchup, retry) of each destination.
SELECT DISTINCT ON (destination_id) * FROM backup_runs
WHERE destination_id IS NOT NULL AND trigger IN ('schedule', 'manual', 'catchup', 'retry')
ORDER BY destination_id, queued_at DESC, id DESC;

-- name: ListBackupFilesLastOk :many
-- scope: none
-- The newest run of each destination whose `files` step succeeded (LAUN-80, the
-- attachment backup is a step of a backup run).
SELECT DISTINCT ON (destination_id) id, destination_id, finished_at FROM backup_runs
WHERE destination_id IS NOT NULL AND status = 'succeeded'
  AND trigger IN ('schedule', 'manual', 'catchup', 'retry')
  AND steps @> '[{"name":"files","ok":true}]'::jsonb
ORDER BY destination_id, finished_at DESC, id DESC;

-- name: ListBackupRunsPage :many
-- scope: none
-- Newest first, keyset on (queued_at, id). destination_id and the cursor are optional.
SELECT * FROM backup_runs
WHERE (sqlc.narg(destination_id)::uuid IS NULL OR destination_id = sqlc.narg(destination_id)::uuid)
  AND (sqlc.narg(after_at)::timestamptz IS NULL
       OR (queued_at, id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY queued_at DESC, id DESC
LIMIT sqlc.arg(row_limit);

-- name: ListBackupChangeLogPage :many
-- scope: none
SELECT c.id, c.at, c.actor_id, u.username AS actor_username, c.kind, c.destination_id, c.destination_name, c.detail
FROM backup_change_log c LEFT JOIN users u ON u.id = c.actor_id
WHERE sqlc.narg(after_at)::timestamptz IS NULL
   OR (c.at, c.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid)
ORDER BY c.at DESC, c.id DESC
LIMIT sqlc.arg(row_limit);

-- name: LockBackupDestinationRuns :many
-- scope: none
-- The open backup runs of a destination (the busy rule, decisions-T16 #31), locked.
SELECT id, trigger, status FROM backup_runs
WHERE destination_id = $1 AND status IN ('queued', 'waiting_dump', 'running')
  AND trigger IN ('schedule', 'manual', 'catchup', 'retry')
FOR UPDATE;

-- name: CancelQueuedBackupRetry :execrows
-- scope: none
-- Changes no row when the retry was just claimed (the manual run then answers 409 destination_busy).
UPDATE backup_runs SET status = 'canceled', finished_at = now()
WHERE id = $1 AND status = 'queued' AND trigger = 'retry';

-- name: GetNewestBackupDump :one
-- scope: none
SELECT * FROM backup_dumps ORDER BY started_at DESC, id DESC LIMIT 1;

-- name: HasRecentOkBackupDump :one
-- scope: none
-- An ok dump finished in the last 36 hours (database clock).
SELECT EXISTS (SELECT 1 FROM backup_dumps WHERE status = 'ok' AND finished_at > now() - interval '36 hours');

-- name: BackupDBNow :one
-- scope: none
SELECT now()::timestamptz;

-- name: NotifyBackupWork :exec
-- scope: none
-- Wakes offsite (decisions-T16 #30). Sent in the transaction that inserts a run or request.
SELECT pg_notify('backup_work', '');

-- LAUN-71: the offsite worker (decisions-T16 #28 to #33). The worker runs as a
-- restricted database role (#15): SELECT on the backup tables, INSERT and UPDATE on
-- backup_runs, INSERT on backup_requests and backup_change_log, and an UPDATE on
-- (secrets_enc, secrets_set) of backup_destinations and on the two signing columns of
-- backup_settings. A query here touches nothing else, and none uses INSERT ...
-- RETURNING on backup_change_log (the role has no SELECT right on it).

-- name: ListEnabledBackupDestinations :many
-- scope: none
SELECT * FROM backup_destinations WHERE enabled ORDER BY created_at, id;

-- name: GetBackupDestinationByName :one
-- scope: none
SELECT * FROM backup_destinations WHERE name = $1;

-- name: HasSucceededBackupRun :one
-- scope: none
-- The instance guard (#37): a database that already has a succeeded run, with no
-- instance-id file on the scratch volume, is a restored or cloned one.
SELECT EXISTS (SELECT 1 FROM backup_runs WHERE status = 'succeeded');

-- name: InterruptExpiredBackupRuns :many
-- scope: none
-- A running or waiting run whose lease ran out (the worker died): final status interrupted.
UPDATE backup_runs SET status = 'interrupted', finished_at = now(), locked_until = NULL,
    test_secrets_enc = NULL, error_text = 'the worker stopped before the run ended'
WHERE status IN ('running', 'waiting_dump') AND (locked_until IS NULL OR locked_until < now())
RETURNING id;

-- name: ClearStaleBackupTestSecrets :execrows
-- scope: none
-- The sealed secrets of an unsaved test live for the length of the test (#36): any run
-- that is not queued, and any queued one older than 10 minutes, loses them.
UPDATE backup_runs SET test_secrets_enc = NULL
WHERE test_secrets_enc IS NOT NULL
  AND (status <> 'queued' OR queued_at < now() - interval '10 minutes');

-- name: CancelOrphanedBackupRuns :many
-- scope: none
-- #31 "Deleted or disabled destinations". A deleted destination (id NULL, name kept)
-- cancels every open run; a disabled one cancels its BACKUP runs only. An unsaved
-- probe or test has destination_name '' and is never matched.
UPDATE backup_runs r SET status = 'canceled', finished_at = now(), locked_until = NULL, test_secrets_enc = NULL
WHERE r.status IN ('queued', 'waiting_dump', 'running')
  AND ((r.destination_id IS NULL AND r.destination_name <> '')
       OR (r.trigger IN ('schedule', 'manual', 'catchup', 'retry')
           AND EXISTS (SELECT 1 FROM backup_destinations d WHERE d.id = r.destination_id AND NOT d.enabled)))
RETURNING r.id;

-- name: FailBackupRunsInstanceChanged :execrows
-- scope: none
-- #37: while the instance guard fails, every queued or waiting backup run fails
-- instance_changed with no retry (which also frees the one-run index).
UPDATE backup_runs SET status = 'failed', finished_at = now(), locked_until = NULL, will_retry = false,
    error_code = 'instance_changed', error_text = sqlc.arg(error_text)
WHERE status IN ('queued', 'waiting_dump') AND trigger IN ('schedule', 'manual', 'catchup', 'retry');

-- name: BackupSlotHasRun :one
-- scope: none
-- #31 "Missed runs": a slot has a run when a backup run queued at or after it is
-- queued, waiting, running, succeeded or failed (not interrupted, not canceled).
SELECT EXISTS (
    SELECT 1 FROM backup_runs
    WHERE destination_id = sqlc.arg(destination_id) AND trigger IN ('schedule', 'catchup', 'retry', 'manual')
      AND status IN ('queued', 'waiting_dump', 'running', 'succeeded', 'failed')
      AND queued_at >= sqlc.arg(slot)::timestamptz);

-- name: InsertScheduledBackupRun :execrows
-- scope: none
-- ON CONFLICT DO NOTHING: the one-run index (#31) turns a second open backup run into no row.
INSERT INTO backup_runs (id, destination_id, destination_name, trigger, status, attempt)
SELECT sqlc.arg(id), d.id, d.name, sqlc.arg(trigger), 'queued', 1
FROM backup_destinations d WHERE d.id = sqlc.arg(destination_id) AND d.enabled
ON CONFLICT DO NOTHING;

-- name: ListBackupVerifyDue :many
-- scope: none
-- Destinations whose verify interval has passed: the newest non-canceled verify run (or,
-- with none, the first success) is older than verify_every_days, none is open.
SELECT d.id FROM backup_destinations d
WHERE d.enabled AND d.verify_every_days > 0
  AND EXISTS (SELECT 1 FROM backup_runs r WHERE r.destination_id = d.id AND r.status = 'succeeded'
              AND r.trigger IN ('schedule', 'manual', 'catchup', 'retry')
              AND r.remote_name IS NOT NULL AND r.sha256 IS NOT NULL)
  AND NOT EXISTS (SELECT 1 FROM backup_runs v WHERE v.destination_id = d.id AND v.trigger = 'verify'
                  AND v.status IN ('queued', 'running'))
  AND COALESCE(
        (SELECT max(v.queued_at) FROM backup_runs v WHERE v.destination_id = d.id AND v.trigger = 'verify'
           AND v.status <> 'canceled'),
        (SELECT min(r.finished_at) FROM backup_runs r WHERE r.destination_id = d.id AND r.status = 'succeeded'
           AND r.trigger IN ('schedule', 'manual', 'catchup', 'retry')))
      + make_interval(days => d.verify_every_days) <= now();

-- name: GetNewestSucceededBackupRun :one
-- scope: none
SELECT * FROM backup_runs
WHERE destination_id = $1 AND status = 'succeeded' AND trigger IN ('schedule', 'manual', 'catchup', 'retry')
  AND remote_name IS NOT NULL AND sha256 IS NOT NULL
ORDER BY finished_at DESC, id DESC LIMIT 1;

-- name: NextQueuedBackupRun :one
-- scope: none
-- The next queued run that is due (a retry waits for its retry_at, database clock),
-- skipping runs this pass could not lock. backup_ok false (the instance guard failed)
-- leaves out the four backup triggers.
SELECT * FROM backup_runs
WHERE status = 'queued' AND (retry_at IS NULL OR retry_at <= now())
  AND NOT (id = ANY(sqlc.arg(skip)::uuid[]))
  AND (sqlc.arg(backup_ok)::boolean OR trigger NOT IN ('schedule', 'manual', 'catchup', 'retry'))
ORDER BY queued_at, id LIMIT 1;

-- name: ClaimBackupRun :one
-- scope: none
-- The claim of #31 for a backup run: ONE statement, filtered by the destination whose
-- advisory lock the caller holds. new_status is running (a dump is ready) or waiting_dump.
UPDATE backup_runs SET status = sqlc.arg(new_status),
    request_id   = COALESCE(sqlc.narg(request_id), request_id),
    dump_id      = sqlc.narg(dump_id),
    started_at   = CASE WHEN sqlc.arg(new_status)::text = 'running' THEN now() ELSE started_at END,
    locked_until = now() + interval '60 seconds'
WHERE id = (SELECT b.id FROM backup_runs b
            WHERE b.id = sqlc.arg(id) AND b.status = 'queued' AND b.destination_id = sqlc.arg(destination_id)
              AND b.trigger IN ('schedule', 'manual', 'catchup', 'retry')
              AND (b.retry_at IS NULL OR b.retry_at <= now())
            FOR UPDATE SKIP LOCKED)
RETURNING *;

-- name: ClaimUnlockedBackupRun :one
-- scope: none
-- test, probe, exchange and verify runs (and every unsaved run) are claimed WITHOUT
-- the destination lock, so a test is allowed while a backup runs (#31).
UPDATE backup_runs SET status = 'running', started_at = now(), locked_until = now() + interval '60 seconds'
WHERE id = (SELECT b.id FROM backup_runs b
            WHERE b.id = sqlc.arg(id) AND b.status = 'queued'
              AND b.trigger IN ('test', 'probe', 'exchange', 'verify')
            FOR UPDATE SKIP LOCKED)
RETURNING *;

-- name: MarkBackupRunRunning :one
-- scope: none
-- waiting_dump to running, with the destination lock held.
UPDATE backup_runs SET status = 'running', started_at = now(), dump_id = sqlc.arg(dump_id),
    locked_until = now() + interval '60 seconds'
WHERE id = sqlc.arg(id) AND status = 'waiting_dump'
RETURNING *;

-- name: RenewBackupRunLease :execrows
-- scope: none
-- 0 rows: the run was cancelled or interrupted under us; the caller aborts.
UPDATE backup_runs SET locked_until = now() + interval '60 seconds'
WHERE id = $1 AND status IN ('running', 'waiting_dump');

-- name: SetBackupRunRemoteName :execrows
-- scope: none
UPDATE backup_runs SET remote_name = $2 WHERE id = $1 AND status = 'running';

-- name: FinishBackupRun :one
-- scope: none
-- The only way a run ends. It matches only a running or waiting run, so a run that was
-- cancelled meanwhile is never overwritten. A retryable failure sets will_retry and a
-- retry_at 30 minutes ahead on ITS OWN row (#31); the queued retry row copies it.
UPDATE backup_runs SET status = sqlc.arg(status), finished_at = now(), locked_until = NULL, test_secrets_enc = NULL,
    bytes       = COALESCE(sqlc.narg(bytes), bytes),
    sha256      = COALESCE(sqlc.narg(sha256), sha256),
    remote_name = COALESCE(sqlc.narg(remote_name), remote_name),
    steps       = COALESCE(sqlc.narg(steps), steps),
    error_code  = sqlc.arg(error_code),
    error_text  = sqlc.arg(error_text),
    will_retry  = sqlc.arg(will_retry),
    retry_at    = CASE WHEN sqlc.arg(will_retry)::boolean THEN now() + interval '30 minutes' ELSE NULL END
WHERE id = sqlc.arg(id) AND status IN ('running', 'waiting_dump')
RETURNING *;

-- name: InsertBackupRetryRun :execrows
-- scope: none
-- The queued retry of a failed run: same retry_at, will_retry false. Inserted after
-- the parent is final, so it does not collide with the one-run index.
INSERT INTO backup_runs (id, destination_id, destination_name, trigger, status, attempt, retry_at)
SELECT sqlc.arg(id), d.id, d.name, 'retry', 'queued', sqlc.arg(attempt), sqlc.arg(retry_at)::timestamptz
FROM backup_destinations d WHERE d.id = sqlc.arg(destination_id)
ON CONFLICT DO NOTHING;

-- name: ListUsableBackupDumps :many
-- scope: none
-- Candidate dumps, newest first: ok, not removed, with a sha256 (an ok dump without
-- a hash cannot be checked, so it is not used). The caller also checks the file.
SELECT * FROM backup_dumps
WHERE status = 'ok' AND file_removed_at IS NULL AND sha256 IS NOT NULL AND bytes IS NOT NULL
ORDER BY started_at DESC, id DESC LIMIT 20;

-- name: ListBackupDumpsSince :many
-- scope: none
-- Every dump that STARTED at or after the request's time (#23 rule), oldest first.
SELECT * FROM backup_dumps WHERE started_at >= $1 ORDER BY started_at, id LIMIT 50;

-- name: GetBackupDump :one
-- scope: none
SELECT * FROM backup_dumps WHERE id = $1;

-- name: GetBackupRequest :one
-- scope: none
SELECT * FROM backup_requests WHERE id = $1;

-- name: GetOpenBackupRequest :one
-- scope: none
-- The newest request still queued or taken: two destinations share one extra dump.
SELECT * FROM backup_requests WHERE state IN ('queued', 'taken') ORDER BY requested_at DESC, id DESC LIMIT 1;

-- name: LockBackupRequests :exec
-- scope: none
-- Serialises "is a request open? else make one" across workers, until the transaction ends.
SELECT pg_advisory_xact_lock(7368106633251971);

-- name: ListEarlierRunningBackupDumpBytes :many
-- scope: none
-- #18: the dump sizes of the backup runs that are running and started before this one.
SELECT r.id, COALESCE(d.bytes, 0)::bigint AS dump_bytes
FROM backup_runs r LEFT JOIN backup_dumps d ON d.id = r.dump_id
WHERE r.status = 'running' AND r.trigger IN ('schedule', 'manual', 'catchup', 'retry')
  AND r.id <> sqlc.arg(id)
  AND (r.started_at, r.id) < (sqlc.arg(started_at)::timestamptz, sqlc.arg(id)::uuid);

-- name: ListRunningBackupRemoteNames :many
-- scope: none
-- Names a running run has chosen: retention never treats them as orphans (#33).
SELECT remote_name FROM backup_runs WHERE status = 'running' AND remote_name IS NOT NULL AND id <> $1;

-- name: SetBackupDestinationSecretsByWorker :exec
-- scope: none
-- offsite's column grant (#15): secrets_enc and secrets_set only. Run under
-- GetBackupDestinationForUpdate (the merge of a Dropbox refresh and an app PATCH).
UPDATE backup_destinations SET secrets_enc = sqlc.narg(secrets_enc), secrets_set = sqlc.arg(secrets_set)
WHERE id = sqlc.arg(id);

-- name: InsertBackupChangeLogNoReturn :exec
-- scope: none
-- For the restricted role: no RETURNING (it has no SELECT right on the table).
INSERT INTO backup_change_log (id, actor_id, kind, destination_id, destination_name, detail)
VALUES (sqlc.arg(id), sqlc.narg(actor_id), sqlc.arg(kind), sqlc.narg(destination_id), sqlc.arg(destination_name), sqlc.arg(detail));

-- name: ReplaceBackupSigningKey :exec
-- scope: none
-- rotate-signing: the trigger (#22) allows it only in the transaction of a
-- signing_key_replaced change-log row, with no savepoint.
UPDATE backup_settings SET signing_public = sqlc.arg(signing_public), signing_private_enc = sqlc.arg(signing_private_enc)
WHERE id;

-- LAUN-75: notices, change notices, the stale alert and the app's row cleanup
-- (decisions-T16 #29, #41). These run in the APP (the table owner), not as the
-- offsite role. Every time is a parameter: the caller passes its clock (the
-- database's now(), or the test clock), so no query reads now().

-- name: ListBackupNoticeAdmins :many
-- scope: none
-- Everyone a notice goes to: every active human platform admin who has an address.
SELECT id, username, email FROM users
WHERE is_platform_admin AND NOT is_agent AND deactivated_at IS NULL
  AND email IS NOT NULL AND email <> ''
ORDER BY username, id;

-- name: LockUnnotifiedBackupRuns :many
-- scope: none
-- Final runs of the last 24 hours that nobody has looked at yet, locked (SKIP LOCKED: a
-- second app instance gets other rows or none). No sealed column is selected.
SELECT id, destination_name, trigger, status, will_retry, started_at, finished_at, bytes, error_code, error_text
FROM backup_runs
WHERE notified_at IS NULL AND finished_at IS NOT NULL
  AND status IN ('succeeded', 'failed', 'interrupted', 'canceled')
  AND finished_at > sqlc.arg(now)::timestamptz - interval '24 hours'
ORDER BY finished_at, id
LIMIT 50
FOR UPDATE SKIP LOCKED;

-- name: StampBackupRuns :execrows
-- scope: none
UPDATE backup_runs SET notified_at = sqlc.arg(now) WHERE id = ANY(sqlc.arg(ids)::uuid[]) AND notified_at IS NULL;

-- name: LockUnnotifiedBackupChanges :many
-- scope: none
-- Change-log rows of the last 7 days that no mail has covered, locked. The actor's name is
-- read here; detail is read as bytes and only a whitelist of its fields reaches a mail.
SELECT c.id, c.at, c.kind, c.destination_name, c.detail, u.username AS actor_username
FROM backup_change_log c LEFT JOIN users u ON u.id = c.actor_id
WHERE c.notified_at IS NULL AND c.at > sqlc.arg(now)::timestamptz - interval '7 days'
ORDER BY c.at, c.id
LIMIT 20
FOR UPDATE OF c SKIP LOCKED;

-- name: StampBackupChange :execrows
-- scope: none
UPDATE backup_change_log SET notified_at = sqlc.arg(now) WHERE id = sqlc.arg(id) AND notified_at IS NULL;

-- name: ListBackupStaleInputs :many
-- scope: none
-- Per enabled destination: what the stale rule (#41) needs. last_success is the newest
-- FINISH of a schedule, catchup, retry or manual run that succeeded (test, probe, verify
-- and exchange runs do not count).
SELECT d.id, d.name, d.schedule, d.created_at, d.enabled_since, d.last_stale_alert_at,
       COALESCE(s.last_success, 'epoch'::timestamptz)::timestamptz AS last_success,
       (s.last_success IS NOT NULL)::boolean AS has_success
FROM backup_destinations d
LEFT JOIN (SELECT r.destination_id, max(r.finished_at)::timestamptz AS last_success FROM backup_runs r
           WHERE r.status = 'succeeded' AND r.trigger IN ('schedule', 'catchup', 'retry', 'manual')
           GROUP BY r.destination_id) s ON s.destination_id = d.id
WHERE d.enabled
ORDER BY d.created_at, d.id;

-- name: ClaimBackupStaleAlert :execrows
-- scope: none
-- At most once per 24 hours per destination, across app instances: of two callers one
-- changes the row. updated_at is not touched (this is not an edit of the destination).
UPDATE backup_destinations SET last_stale_alert_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND enabled
  AND (last_stale_alert_at IS NULL OR last_stale_alert_at <= sqlc.arg(now)::timestamptz - interval '24 hours');

-- name: SweepBackupTestSecrets :execrows
-- scope: none
-- #29: a sealed unsaved-test secret older than 10 minutes is cleared, whatever the run state.
UPDATE backup_runs SET test_secrets_enc = NULL
WHERE test_secrets_enc IS NOT NULL AND queued_at < sqlc.arg(now)::timestamptz - interval '10 minutes';

-- name: PruneBackupRuns :execrows
-- scope: none
-- #29: final runs older than 180 days, in batches.
DELETE FROM backup_runs WHERE id IN (
    SELECT r.id FROM backup_runs r
    WHERE r.finished_at IS NOT NULL AND r.finished_at < sqlc.arg(cutoff)::timestamptz
    ORDER BY r.finished_at, r.id LIMIT sqlc.arg(batch_size)::int);

-- name: PruneBackupDumps :execrows
-- scope: none
-- #29: rows whose file was removed more than 180 days ago, and failed rows that finished
-- more than 180 days ago.
DELETE FROM backup_dumps WHERE id IN (
    SELECT d.id FROM backup_dumps d
    WHERE (d.file_removed_at IS NOT NULL AND d.file_removed_at < sqlc.arg(cutoff)::timestamptz)
       OR (d.status = 'failed' AND d.finished_at IS NOT NULL AND d.finished_at < sqlc.arg(cutoff)::timestamptz)
    ORDER BY d.started_at, d.id LIMIT sqlc.arg(batch_size)::int);

-- name: PruneBackupRequests :execrows
-- scope: none
-- #29: requests in a final state older than 7 days.
DELETE FROM backup_requests WHERE id IN (
    SELECT q.id FROM backup_requests q
    WHERE q.state IN ('done', 'failed') AND COALESCE(q.finished_at, q.requested_at) < sqlc.arg(cutoff)::timestamptz
    ORDER BY q.requested_at, q.id LIMIT sqlc.arg(batch_size)::int);

-- name: PruneBackupChangeLog :execrows
-- scope: none
-- #29: change-log rows older than 730 days.
DELETE FROM backup_change_log WHERE id IN (
    SELECT c.id FROM backup_change_log c
    WHERE c.at < sqlc.arg(cutoff)::timestamptz
    ORDER BY c.at, c.id LIMIT sqlc.arg(batch_size)::int);

-- name: FailStuckBackupDumps :execrows
-- scope: none
-- #29: a dump left running for more than 6 hours (a killed backup container, or a restored
-- database that carries another host's running row) is failed. The text is fixed.
UPDATE backup_dumps SET status = 'failed', finished_at = sqlc.arg(now),
    error_text = 'the dump did not finish in 6 hours'
WHERE status = 'running' AND started_at < sqlc.arg(now)::timestamptz - interval '6 hours';

-- name: FailStuckBackupRequests :execrows
-- scope: none
-- #29: a request left taken for more than 2 hours whose dump is not running is failed. A
-- request whose dump is still running is left alone (FailStuckBackupDumps ends that dump
-- after 6 hours). Only state = 'taken' changes, so backup.sh's own done/failed never flips.
UPDATE backup_requests SET state = 'failed', finished_at = sqlc.arg(now)
WHERE state = 'taken' AND taken_at < sqlc.arg(now)::timestamptz - interval '2 hours'
  AND NOT EXISTS (SELECT 1 FROM backup_dumps d
                  WHERE d.status = 'running' AND (d.request_id = backup_requests.id OR d.id = backup_requests.dump_id));
