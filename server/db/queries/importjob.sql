-- The web Linear import (LAUN-8, decisions-T17 #36 to #45). Every query
-- filters by org_id. Neither table holds the Linear API key.

-- name: ImportSessionInsert :one
-- scope: admin
INSERT INTO import_sessions (id, org_id, created_by, state, export_dir, export_hash, export_counts, failed_files, failed_file_names)
VALUES (@id, @org_id, @created_by, @state, @export_dir, @export_hash, @export_counts, @failed_files, @failed_file_names)
RETURNING *;

-- name: ImportSessionGetOpen :one
-- scope: admin
SELECT * FROM import_sessions WHERE org_id = @org_id AND state <> 'closed';

-- name: ImportSessionGet :one
-- scope: admin
SELECT * FROM import_sessions WHERE org_id = @org_id AND id = @id;

-- name: ImportSessionLock :one
-- scope: admin
-- The row lock every session transition takes first.
SELECT * FROM import_sessions WHERE org_id = @org_id AND id = @id FOR UPDATE;

-- name: ImportSessionSetState :exec
-- scope: system
UPDATE import_sessions SET state = @state, updated_at = now() WHERE org_id = @org_id AND id = @id;

-- name: ImportSessionClearExport :exec
-- scope: system
-- A new export starts: the files on disk are about to change, so no earlier
-- hash (and no dry run bound to it) may stand for them (LAUN-8 review).
UPDATE import_sessions
SET export_hash = NULL, export_counts = '{}', failed_files = 0, failed_file_names = '[]', updated_at = now()
WHERE org_id = @org_id AND id = @id;

-- name: ImportSessionSetExport :exec
-- scope: system
-- A finished export (or an adopted one): the state, the manifest hash and
-- counts, and the file problems of the manifest check (#39).
UPDATE import_sessions
SET state = @state, export_hash = @export_hash, export_counts = @export_counts,
    failed_files = @failed_files, failed_file_names = @failed_file_names, updated_at = now()
WHERE org_id = @org_id AND id = @id;

-- name: ImportSessionSetUserMap :exec
-- scope: admin
UPDATE import_sessions
SET state = @state, user_map = @user_map, user_map_hash = @user_map_hash, updated_at = now()
WHERE org_id = @org_id AND id = @id;

-- name: ImportSessionClose :exec
-- scope: admin
-- The map holds Linear emails: it goes with the session.
UPDATE import_sessions
SET state = 'closed', closed_at = now(), updated_at = now(), user_map = NULL
WHERE org_id = @org_id AND id = @id;

-- name: ImportSessionDirInUse :one
-- scope: admin
SELECT EXISTS (SELECT 1 FROM import_sessions WHERE export_dir = @export_dir AND state <> 'closed');

-- name: ImportSessionOpenDirs :many
-- scope: system
SELECT export_dir FROM import_sessions WHERE state <> 'closed';

-- name: ImportSessionsExpired :many
-- scope: system
-- Open sessions with no active job whose last job ended before @cutoff, or
-- that never started a job and were made before it (#39, #43).
SELECT s.org_id, s.id, s.export_dir
FROM import_sessions s
WHERE s.state <> 'closed'
  AND NOT EXISTS (SELECT 1 FROM import_jobs j WHERE j.org_id = s.org_id AND j.session_id = s.id AND j.state IN ('queued', 'running'))
  AND COALESCE((SELECT max(j.finished_at) FROM import_jobs j WHERE j.org_id = s.org_id AND j.session_id = s.id), s.created_at) < @cutoff::timestamptz;

-- name: ImportSessionBlocksCycles :one
-- scope: admin
-- decisions-T17 #43: the states in which turning cycles on is refused.
SELECT EXISTS (
    SELECT 1 FROM import_sessions
    WHERE org_id = @org_id AND state IN ('exporting', 'exported', 'user_map', 'dry_run_ok', 'importing')
);

-- name: ImportJobInsert :one
-- scope: admin
INSERT INTO import_jobs (org_id, session_id, kind, state, export_hash, user_map_hash, instance_id)
VALUES (@org_id, @session_id, @kind, 'queued', @export_hash, @user_map_hash, @instance_id)
RETURNING *;

-- name: ImportJobGet :one
-- scope: admin
SELECT * FROM import_jobs WHERE org_id = @org_id AND id = @id;

-- name: ImportJobActive :one
-- scope: admin
SELECT * FROM import_jobs WHERE org_id = @org_id AND state IN ('queued', 'running');

-- name: ImportJobMarkRunning :execrows
-- scope: system
UPDATE import_jobs SET state = 'running', started_at = now(), heartbeat_at = now()
WHERE org_id = @org_id AND id = @id AND state = 'queued';

-- name: ImportJobHeartbeat :one
-- scope: system
-- The owner's beat. No row: the job is no longer active (the sweep took it).
UPDATE import_jobs SET heartbeat_at = now()
WHERE org_id = @org_id AND id = @id AND state IN ('queued', 'running')
RETURNING cancel_requested;

-- name: ImportJobProgress :exec
-- scope: system
UPDATE import_jobs SET phase = @phase, done = @done, total = @total
WHERE org_id = @org_id AND id = @id AND state IN ('queued', 'running');

-- name: ImportJobFinish :execrows
-- scope: system
-- The end of a job. It only changes a job that is still active, so a job the
-- sweep marked interrupted is not overwritten.
UPDATE import_jobs
SET state = @state, counts = @counts, rows = @rows, rows_truncated = @rows_truncated, committed = @committed,
    error_code = @error_code, error_text = @error_text, finished_at = now()
WHERE org_id = @org_id AND id = @id AND state IN ('queued', 'running');

-- name: ImportJobRequestCancel :one
-- scope: admin
UPDATE import_jobs SET cancel_requested = true
WHERE org_id = @org_id AND id = @id AND state IN ('queued', 'running')
RETURNING *;

-- name: ImportJobsLastByKind :many
-- scope: admin
SELECT DISTINCT ON (kind) * FROM import_jobs
WHERE org_id = @org_id AND session_id = @session_id
ORDER BY kind, created_at DESC, id DESC;

-- name: ImportJobsSweepStale :many
-- scope: system
-- decisions-T17 #36: a heartbeat older than @stale_secs (database time, so
-- two instances agree) is a dead owner.
UPDATE import_jobs
SET state = 'failed', error_code = 'interrupted', error_text = @error_text, finished_at = now()
WHERE state IN ('queued', 'running') AND heartbeat_at < now() - make_interval(secs => @stale_secs::double precision)
RETURNING id, org_id, session_id, kind;

-- name: ImportDryRunCurrent :one
-- scope: system
-- A succeeded dry run on the session's CURRENT export and user map (#41).
SELECT EXISTS (
    SELECT 1 FROM import_jobs
    WHERE org_id = @org_id AND session_id = @session_id AND kind = 'dry_run' AND state = 'succeeded'
      AND export_hash IS NOT DISTINCT FROM @export_hash::text
      AND user_map_hash IS NOT DISTINCT FROM @user_map_hash::text
      AND export_hash IS NOT NULL AND user_map_hash IS NOT NULL
);

-- name: ImportCommittedExists :one
-- scope: system
-- Rows of an import of this session were committed (#43: verify stays allowed).
SELECT EXISTS (
    SELECT 1 FROM import_jobs
    WHERE org_id = @org_id AND session_id = @session_id AND kind = 'import' AND committed
);
