-- LAUN-8 (T17, decisions-T17 #36 to #45): migration C. The Linear import as
-- a background job in the API process: one session per org at a time, and one
-- active job per org. The Linear API key is in NO column here (#38).

-- #37: the session. `state` moves only as the table of #43 says; the app
-- enforces it under a row lock, the CHECK only keeps the set closed.
CREATE TABLE import_sessions (
    id                uuid        NOT NULL PRIMARY KEY DEFAULT uuidv7(),
    org_id            uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    -- Who started it. SET NULL keeps the history when the user is deleted.
    created_by        uuid        REFERENCES users (id) ON DELETE SET NULL,
    state             text        NOT NULL DEFAULT 'new'
                      CONSTRAINT import_sessions_state_check CHECK (state IN
                          ('new', 'exporting', 'exported', 'user_map', 'dry_run_ok', 'importing',
                           'imported', 'verified', 'failed', 'closed')),
    -- A directory NAME under IMPORT_WORK_DIR, made by the server (or an
    -- adopted CLI export, #44). Never a path from a request.
    export_dir        text        NOT NULL
                      CONSTRAINT import_sessions_export_dir_check CHECK (export_dir ~ '^[A-Za-z0-9._-]{1,64}$' AND export_dir NOT IN ('.', '..')),
    -- The importer's own shape, {default, users}. Linear emails can be keys
    -- or values: personal data, kept for the session's life, cleared on close.
    user_map          jsonb,
    failed_files      integer     NOT NULL DEFAULT 0
                      CONSTRAINT import_sessions_failed_files_check CHECK (failed_files >= 0),
    -- At most 20 entries {name, reason} (#39).
    failed_file_names jsonb       NOT NULL DEFAULT '[]'
                      CONSTRAINT import_sessions_failed_names_check CHECK (jsonb_typeof(failed_file_names) = 'array' AND jsonb_array_length(failed_file_names) <= 20),
    -- sha256 (hex) of the export's manifest and of the stored map (#37): a
    -- dry run is "current" for `run` only when both match.
    export_hash       text,
    user_map_hash     text,
    -- Records per export file (the manifest counts).
    export_counts     jsonb       NOT NULL DEFAULT '{}',
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    closed_at         timestamptz,
    CONSTRAINT import_sessions_closed_check CHECK ((state = 'closed') = (closed_at IS NOT NULL)),
    CONSTRAINT import_sessions_org_id_id_key UNIQUE (org_id, id)
);
-- At most one open session per org.
CREATE UNIQUE INDEX import_sessions_open_key ON import_sessions (org_id) WHERE state <> 'closed';
-- The adopt check (#44): an export directory is in use by one open session.
CREATE INDEX import_sessions_dir_idx ON import_sessions (export_dir) WHERE state <> 'closed';

-- #37: the job. `queued` and `running` are the active states.
CREATE TABLE import_jobs (
    id               uuid        NOT NULL PRIMARY KEY DEFAULT uuidv7(),
    org_id           uuid        NOT NULL,
    session_id       uuid        NOT NULL,
    kind             text        NOT NULL
                     CONSTRAINT import_jobs_kind_check CHECK (kind IN ('export', 'dry_run', 'import', 'verify')),
    state            text        NOT NULL DEFAULT 'queued'
                     CONSTRAINT import_jobs_state_check CHECK (state IN ('queued', 'running', 'succeeded', 'failed', 'canceled')),
    -- The entity being worked on, and the progress in it (the files phase
    -- counts bytes). total 0 means unknown.
    phase            text        NOT NULL DEFAULT '',
    done             bigint      NOT NULL DEFAULT 0,
    total            bigint      NOT NULL DEFAULT 0,
    -- The importer's report without the verify rows and with the exclusions
    -- capped (counts, exclusions, uploads), and the verify rows (capped at
    -- 500; `rows_truncated` says so).
    counts           jsonb       NOT NULL DEFAULT '{}',
    rows             jsonb       NOT NULL DEFAULT '[]'
                     CONSTRAINT import_jobs_rows_check CHECK (jsonb_typeof(rows) = 'array' AND jsonb_array_length(rows) <= 500),
    rows_truncated   boolean     NOT NULL DEFAULT false,
    -- #42c: true when the import transaction committed. A verify mismatch
    -- before the commit keeps no rows; one after it keeps them.
    committed        boolean     NOT NULL DEFAULT false,
    -- Copied from the session when the job starts.
    export_hash      text,
    user_map_hash    text,
    -- error_text is a short message of ours, chosen by error_code (#37): the
    -- importer's own errors can name a user email or a ticket title.
    error_code       text,
    error_text       text,
    cancel_requested boolean     NOT NULL DEFAULT false,
    -- Random at process start; the heartbeat (#36) tells a dead owner.
    instance_id      text        NOT NULL,
    heartbeat_at     timestamptz NOT NULL DEFAULT now(),
    created_at       timestamptz NOT NULL DEFAULT now(),
    started_at       timestamptz,
    finished_at      timestamptz,
    CONSTRAINT import_jobs_session_fk FOREIGN KEY (org_id, session_id)
        REFERENCES import_sessions (org_id, id) ON DELETE CASCADE,
    CONSTRAINT import_jobs_org_id_id_key UNIQUE (org_id, id),
    CONSTRAINT import_jobs_finished_check CHECK ((state IN ('queued', 'running')) = (finished_at IS NULL))
);
-- One active job per org, across instances (#36 guard a).
CREATE UNIQUE INDEX import_jobs_active_key ON import_jobs (org_id) WHERE state IN ('queued', 'running');
CREATE INDEX import_jobs_session_idx ON import_jobs (org_id, session_id, created_at DESC);
-- The heartbeat sweep reads the active rows only.
CREATE INDEX import_jobs_heartbeat_idx ON import_jobs (heartbeat_at) WHERE state IN ('queued', 'running');
