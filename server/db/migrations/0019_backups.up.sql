-- LAUN-68 (docs/internal/decisions-T16-backups.md #22 to #24, #31): the six tables
-- of scheduled, encrypted backups to remote storage. Install-level (no org_id).
-- Sealed columns (signing_private_enc, secrets_enc, test_secrets_enc) hold
-- AES-256-GCM blobs under BACKUP_ENCRYPTION_KEY; the database never sees a secret in clear.

-- 1. backup_settings: one row, like platform_settings.
CREATE TABLE backup_settings (
    id                  boolean PRIMARY KEY DEFAULT true CHECK (id),
    -- 8 random lowercase hex characters; names remote files. No pgcrypto needed.
    install_id          text NOT NULL DEFAULT substr(replace(gen_random_uuid()::text, '-', ''), 1, 8)
                        CHECK (install_id ~ '^[0-9a-f]{8}$'),
    instance_id         uuid NOT NULL DEFAULT gen_random_uuid(),
    -- Each element: {publicKey, fingerprint, addedAt, addedBy, effectiveAt, removeAt, removeBy} (#20).
    recipients          jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(recipients) = 'array'),
    ever_had_recipient  boolean NOT NULL DEFAULT false,
    -- The empty state is signing_public = '' AND signing_private_enc IS NULL (#22).
    signing_public      text NOT NULL DEFAULT '',
    signing_private_enc bytea CHECK (signing_private_enc IS NULL OR octet_length(signing_private_enc) >= 28),
    -- NULL until the app first seals a secret, then the 32-byte key check (#12).
    key_check           bytea CHECK (key_check IS NULL OR octet_length(key_check) = 32),
    notify_on_failure   boolean NOT NULL DEFAULT true,
    notify_on_success   boolean NOT NULL DEFAULT false,
    stale_alert_hours   integer NOT NULL DEFAULT 48 CHECK (stale_alert_hours BETWEEN 0 AND 8760),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    updated_by          uuid REFERENCES users (id) ON DELETE SET NULL
);
INSERT INTO backup_settings (id) VALUES (true);

-- 2. backup_destinations.
CREATE TABLE backup_destinations (
    id                  uuid PRIMARY KEY DEFAULT uuidv7(),
    name                text NOT NULL UNIQUE CHECK (char_length(name) BETWEEN 1 AND 60),
    type                text NOT NULL CHECK (type IN ('s3', 'webdav', 'sftp', 'dropbox')),
    enabled             boolean NOT NULL DEFAULT false,
    enabled_since       timestamptz,
    -- Non-secret: URL, bucket, region, folder, username, SFTP host key, TLS pin.
    config              jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(config) = 'object'),
    secrets_enc         bytea CHECK (secrets_enc IS NULL OR octet_length(secrets_enc) >= 28),
    secrets_set         text[] NOT NULL DEFAULT '{}',
    schedule            text NOT NULL DEFAULT 'daily' CHECK (schedule IN ('daily', 'weekly')),
    at_utc              text NOT NULL DEFAULT '04:15' CHECK (at_utc ~ '^([01][0-9]|2[0-3]):[0-5][0-9]$'),
    weekday             smallint CHECK (weekday BETWEEN 0 AND 6),
    keep_last           integer NOT NULL DEFAULT 14 CHECK (keep_last BETWEEN 1 AND 365),
    verify_every_days   integer NOT NULL DEFAULT 7 CHECK (verify_every_days BETWEEN 0 AND 90),
    last_stale_alert_at timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    updated_by          uuid REFERENCES users (id) ON DELETE SET NULL,
    -- weekday is set if and only if the schedule is weekly.
    CONSTRAINT backup_destinations_weekday_schedule_check CHECK ((weekday IS NOT NULL) = (schedule = 'weekly'))
);

-- 3. backup_dumps, written by backup.sh. Its request_id foreign key is added
-- below: backup_requests.dump_id points back at this table.
CREATE TABLE backup_dumps (
    id                uuid PRIMARY KEY DEFAULT uuidv7(),
    file_name         text NOT NULL UNIQUE CHECK (char_length(file_name) BETWEEN 1 AND 255),
    status            text NOT NULL CHECK (status IN ('running', 'ok', 'failed')),
    request_id        uuid,
    started_at        timestamptz NOT NULL DEFAULT now(),
    finished_at       timestamptz,
    bytes             bigint CHECK (bytes IS NULL OR bytes >= 0),
    sha256            text CHECK (sha256 IS NULL OR sha256 ~ '^[0-9a-f]{64}$'),
    migration_version bigint,
    file_removed_at   timestamptz,
    error_text        text NOT NULL DEFAULT ''
);

-- 4. backup_requests: a request for a fresh dump.
CREATE TABLE backup_requests (
    id                uuid PRIMARY KEY DEFAULT uuidv7(),
    state             text NOT NULL CHECK (state IN ('queued', 'taken', 'done', 'failed')),
    requested_by      text NOT NULL CHECK (requested_by IN ('app', 'offsite')),
    requested_by_user uuid REFERENCES users (id) ON DELETE SET NULL,
    -- The DATABASE clock: a dump counts for this request only if it started at or after it.
    requested_at      timestamptz NOT NULL DEFAULT now(),
    taken_at          timestamptz,
    finished_at       timestamptz,
    dump_id           uuid REFERENCES backup_dumps (id) ON DELETE SET NULL
);

ALTER TABLE backup_dumps
    ADD CONSTRAINT backup_dumps_request_id_fkey FOREIGN KEY (request_id)
    REFERENCES backup_requests (id) ON DELETE SET NULL;

-- 5. backup_runs.
CREATE TABLE backup_runs (
    id                uuid PRIMARY KEY DEFAULT uuidv7(),
    -- A deleted destination keeps its runs: destination_name stays (copied at insert).
    destination_id    uuid REFERENCES backup_destinations (id) ON DELETE SET NULL,
    destination_name  text NOT NULL DEFAULT '',
    dump_id           uuid REFERENCES backup_dumps (id) ON DELETE SET NULL,
    request_id        uuid REFERENCES backup_requests (id) ON DELETE SET NULL,
    trigger           text NOT NULL CHECK (trigger IN
        ('schedule', 'manual', 'catchup', 'retry', 'test', 'probe', 'verify', 'exchange')),
    status            text NOT NULL CHECK (status IN
        ('queued', 'waiting_dump', 'running', 'succeeded', 'failed', 'interrupted', 'canceled')),
    attempt           integer NOT NULL DEFAULT 1 CHECK (attempt >= 1),
    will_retry        boolean NOT NULL DEFAULT false,
    retry_at          timestamptz,
    queued_at         timestamptz NOT NULL DEFAULT now(),
    started_at        timestamptz,
    finished_at       timestamptz,
    bytes             bigint CHECK (bytes IS NULL OR bytes >= 0),
    -- Of the uploaded (encrypted) object; the reference for verify (#22).
    sha256            text CHECK (sha256 IS NULL OR sha256 ~ '^[0-9a-f]{64}$'),
    remote_name       text,
    steps             jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(steps) = 'array'),
    error_code        text NOT NULL DEFAULT '',
    error_text        text NOT NULL DEFAULT '',
    locked_until      timestamptz,
    notified_at       timestamptz,
    -- Only an unsaved-config probe or test or a Dropbox exchange sets these (#36).
    test_config       jsonb,
    test_secrets_enc  bytea CHECK (test_secrets_enc IS NULL OR octet_length(test_secrets_enc) >= 28),
    -- finished_at is set if and only if the status is final.
    CONSTRAINT backup_runs_finished_check CHECK
        ((finished_at IS NOT NULL) = (status IN ('succeeded', 'failed', 'interrupted', 'canceled'))),
    CONSTRAINT backup_runs_will_retry_check CHECK (NOT will_retry OR status = 'failed'),
    -- The lease exists only while the run is running or waits for a dump.
    CONSTRAINT backup_runs_lease_check CHECK
        (locked_until IS NULL OR status IN ('running', 'waiting_dump'))
);

-- The busy rule (#31): one backup run per destination queued, waiting or running.
-- probe, test, verify and exchange runs are not limited.
CREATE UNIQUE INDEX backup_runs_one_active_idx ON backup_runs (destination_id)
    WHERE status IN ('queued', 'waiting_dump', 'running')
      AND trigger IN ('schedule', 'manual', 'catchup', 'retry');
-- The run list, newest first, overall and per destination.
CREATE INDEX backup_runs_list_idx ON backup_runs (queued_at DESC, id DESC);
CREATE INDEX backup_runs_dest_list_idx ON backup_runs (destination_id, queued_at DESC, id DESC)
    WHERE destination_id IS NOT NULL;
-- The job scans: open runs to claim or recover, and runs still to be reported.
CREATE INDEX backup_runs_open_idx ON backup_runs (status, queued_at)
    WHERE status IN ('queued', 'waiting_dump', 'running');
CREATE INDEX backup_runs_unnotified_idx ON backup_runs (finished_at)
    WHERE notified_at IS NULL AND finished_at IS NOT NULL;
-- Foreign keys that are set NULL on delete.
CREATE INDEX backup_runs_dump_idx ON backup_runs (dump_id) WHERE dump_id IS NOT NULL;
CREATE INDEX backup_runs_request_idx ON backup_runs (request_id) WHERE request_id IS NOT NULL;

CREATE INDEX backup_dumps_status_idx ON backup_dumps (status, started_at DESC);
CREATE INDEX backup_dumps_request_idx ON backup_dumps (request_id) WHERE request_id IS NOT NULL;
CREATE INDEX backup_requests_open_idx ON backup_requests (requested_at)
    WHERE state IN ('queued', 'taken');
CREATE INDEX backup_requests_dump_idx ON backup_requests (dump_id) WHERE dump_id IS NOT NULL;
CREATE INDEX backup_destinations_enabled_idx ON backup_destinations (id) WHERE enabled;

-- 6. backup_change_log: the audit table against a stolen admin session (#27).
CREATE TABLE backup_change_log (
    id               uuid PRIMARY KEY DEFAULT uuidv7(),
    at               timestamptz NOT NULL DEFAULT now(),
    -- NULL for a deploy command.
    actor_id         uuid REFERENCES users (id) ON DELETE SET NULL,
    kind             text NOT NULL CHECK (char_length(kind) BETWEEN 1 AND 64),
    destination_id   uuid REFERENCES backup_destinations (id) ON DELETE SET NULL,
    destination_name text NOT NULL DEFAULT '',
    detail           jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(detail) = 'object'),
    notified_at      timestamptz
);
CREATE INDEX backup_change_log_at_idx ON backup_change_log (at DESC, id DESC);
CREATE INDEX backup_change_log_unnotified_idx ON backup_change_log (at) WHERE notified_at IS NULL;
CREATE INDEX backup_change_log_dest_idx ON backup_change_log (destination_id) WHERE destination_id IS NOT NULL;
CREATE INDEX backup_change_log_actor_idx ON backup_change_log (actor_id) WHERE actor_id IS NOT NULL;
CREATE INDEX backup_requests_user_idx ON backup_requests (requested_by_user) WHERE requested_by_user IS NOT NULL;

-- 7. The signing key guard (#22). A change of the two signing columns is allowed
-- only from the empty state (first use), or in the same transaction as a
-- backup_change_log row of kind signing_key_replaced. The result must be a full
-- pair: a blanked or half-empty key is refused. SECURITY DEFINER because the
-- offsite role cannot read backup_change_log; search_path is fixed and every
-- name is schema-qualified. This guards bugs, not a hostile offsite (#22).
CREATE FUNCTION backup_settings_signing_guard() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
BEGIN
    IF NEW.signing_public IS NOT DISTINCT FROM OLD.signing_public
       AND NEW.signing_private_enc IS NOT DISTINCT FROM OLD.signing_private_enc THEN
        RETURN NEW; -- nothing changed
    END IF;
    IF NEW.signing_public = '' OR NEW.signing_private_enc IS NULL THEN
        RAISE EXCEPTION 'the backup signing key cannot be blanked or left half empty'
            USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.signing_public = '' AND OLD.signing_private_enc IS NULL THEN
        RETURN NEW; -- first use
    END IF;
    -- xmin of a row of THIS transaction is the transaction's own id.
    IF EXISTS (SELECT 1 FROM public.backup_change_log
               WHERE kind = 'signing_key_replaced'
                 AND xmin = pg_catalog.pg_current_xact_id()::pg_catalog.xid) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'the backup signing key is replaced only with a signing_key_replaced change-log row in the same transaction'
        USING ERRCODE = 'check_violation';
END
$$;
REVOKE ALL ON FUNCTION backup_settings_signing_guard() FROM PUBLIC;

CREATE TRIGGER backup_settings_signing_guard
    BEFORE UPDATE OF signing_public, signing_private_enc ON backup_settings
    FOR EACH ROW EXECUTE FUNCTION backup_settings_signing_guard();
