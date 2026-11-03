-- LAUN-82 (T17, decisions-T17 #8, #17): migration A. The GitLab tables become
-- provider-neutral git_* tables, gain the provider columns, and the stored
-- ids become text. No MR-state columns here (migration D).

-- ---------------------------------------------------------------------------
-- Renames: tables, then every constraint and index that carries the old name.
-- (Renaming a primary key or unique constraint renames its index too.)
-- ---------------------------------------------------------------------------
ALTER TABLE gitlab_integrations   RENAME TO git_integrations;
ALTER TABLE gitlab_project_map    RENAME TO git_repo_map;
ALTER TABLE gitlab_webhook_events RENAME TO git_webhook_events;

ALTER TABLE git_integrations RENAME CONSTRAINT gitlab_integrations_pkey TO git_integrations_pkey;
ALTER TABLE git_integrations RENAME CONSTRAINT gitlab_integrations_org_id_id_key TO git_integrations_org_id_id_key;
ALTER TABLE git_integrations RENAME CONSTRAINT gitlab_integrations_hash_len TO git_integrations_hash_len;
ALTER TABLE git_integrations RENAME CONSTRAINT gitlab_integrations_org_id_fkey TO git_integrations_org_id_fkey;

ALTER TABLE git_repo_map RENAME CONSTRAINT gitlab_project_map_pkey TO git_repo_map_pkey;
ALTER TABLE git_repo_map RENAME CONSTRAINT gitlab_project_map_integration_fk TO git_repo_map_integration_fk;
ALTER TABLE git_repo_map RENAME CONSTRAINT gitlab_project_map_team_fk TO git_repo_map_team_fk;
ALTER TABLE git_repo_map RENAME CONSTRAINT gitlab_project_map_project_key TO git_repo_map_repo_key;
ALTER INDEX gitlab_project_map_org_integration_idx RENAME TO git_repo_map_org_integration_idx;
ALTER INDEX gitlab_project_map_team_idx RENAME TO git_repo_map_team_idx;

ALTER TABLE git_webhook_events RENAME CONSTRAINT gitlab_webhook_events_pkey TO git_webhook_events_pkey;
ALTER TABLE git_webhook_events RENAME CONSTRAINT gitlab_webhook_events_org_id_fkey TO git_webhook_events_org_id_fkey;
ALTER TABLE git_webhook_events RENAME CONSTRAINT gitlab_webhook_events_status_check TO git_webhook_events_status_check;
ALTER INDEX gitlab_webhook_events_received_idx RENAME TO git_webhook_events_received_idx;

-- ---------------------------------------------------------------------------
-- git_integrations: provider columns. Existing rows are GitLab, webhook_only,
-- with NULL sealed columns (the secret was never stored; they verify by hash).
-- The row id is made in the app (uuidv7() in Go): the AAD of a sealed column
-- holds the row id.
-- ---------------------------------------------------------------------------
ALTER TABLE git_integrations
    ADD COLUMN provider                 text  NOT NULL DEFAULT 'gitlab',
    ADD COLUMN flavor                   text,
    ADD COLUMN mode                     text  NOT NULL DEFAULT 'webhook_only',
    ADD COLUMN api_base_url             text,
    ADD COLUMN name                     text,
    ADD COLUMN token_enc                bytea,
    ADD COLUMN webhook_secret_enc       bytea,
    ADD COLUMN webhook_secret_next_enc  bytea,
    ADD COLUMN webhook_secret_next_hash bytea,
    -- Our own codes only (secret_unreadable, token_rejected,
    -- attention_unreachable, hook_failed); NULL is ok. Not a CHECK: LAUN-7
    -- owns the list and a new code must not need a migration.
    ADD COLUMN attention_code           text,
    ADD CONSTRAINT git_integrations_provider_check
        CHECK (provider IN ('gitlab', 'github', 'bitbucket')),
    ADD CONSTRAINT git_integrations_flavor_check
        CHECK (flavor IS NULL OR flavor IN ('cloud', 'self_hosted')),
    ADD CONSTRAINT git_integrations_mode_check
        CHECK (mode IN ('token', 'webhook_only')),
    ADD CONSTRAINT git_integrations_token_mode_check
        CHECK (mode <> 'token' OR token_enc IS NOT NULL),
    ADD CONSTRAINT git_integrations_next_hash_len
        CHECK (webhook_secret_next_hash IS NULL OR octet_length(webhook_secret_next_hash) = 32);

ALTER TABLE git_integrations DROP CONSTRAINT gitlab_integrations_org_base_key;
ALTER TABLE git_integrations
    ADD CONSTRAINT git_integrations_org_provider_base_key UNIQUE (org_id, provider, base_url);

-- ---------------------------------------------------------------------------
-- git_repo_map
-- ---------------------------------------------------------------------------
ALTER TABLE git_repo_map RENAME COLUMN gitlab_project_id TO repo_key;
ALTER TABLE git_repo_map ALTER COLUMN repo_key TYPE text USING repo_key::text;
ALTER TABLE git_repo_map RENAME COLUMN project_path TO repo_path;
ALTER TABLE git_repo_map
    ADD COLUMN hook_id    text,
    ADD COLUMN hook_state text NOT NULL DEFAULT 'none',
    ADD COLUMN hook_error text,
    ADD CONSTRAINT git_repo_map_hook_state_check CHECK (hook_state IN ('none', 'active', 'failed'));

-- ---------------------------------------------------------------------------
-- git_webhook_events
-- ---------------------------------------------------------------------------
ALTER TABLE git_webhook_events RENAME COLUMN gitlab_project_id TO repo_key;
ALTER TABLE git_webhook_events ALTER COLUMN repo_key TYPE text USING repo_key::text;
ALTER TABLE git_webhook_events ADD COLUMN integration_id uuid;

-- Rows of an org with no integration cannot have been written by one.
DELETE FROM git_webhook_events e
WHERE NOT EXISTS (SELECT 1 FROM git_integrations g WHERE g.org_id = e.org_id);

-- Approximate by design (the table is a dedupe key store, never shown): the
-- org's oldest integration. Several integrations of one org can mis-attribute.
UPDATE git_webhook_events e SET integration_id = (
    SELECT g.id FROM git_integrations g WHERE g.org_id = e.org_id
    ORDER BY g.created_at, g.id LIMIT 1);

ALTER TABLE git_webhook_events ALTER COLUMN integration_id SET NOT NULL;
ALTER TABLE git_webhook_events
    ADD CONSTRAINT git_webhook_events_integration_fk FOREIGN KEY (org_id, integration_id)
        REFERENCES git_integrations (org_id, id) ON DELETE CASCADE;

-- One-time rewrite to the gl: form of decisions-T17 #17.
UPDATE git_webhook_events SET event_uuid = 'gl:' || event_uuid;

ALTER TABLE git_webhook_events DROP CONSTRAINT gitlab_webhook_events_uuid_key;
ALTER TABLE git_webhook_events
    ADD CONSTRAINT git_webhook_events_uuid_key UNIQUE (org_id, integration_id, event_uuid);

-- ---------------------------------------------------------------------------
-- mr_links
-- ---------------------------------------------------------------------------
ALTER TABLE mr_links RENAME COLUMN gitlab_project_id TO repo_key;
ALTER TABLE mr_links ALTER COLUMN repo_key TYPE text USING repo_key::text;
ALTER TABLE mr_links RENAME COLUMN mr_iid TO number;
ALTER TABLE mr_links ALTER COLUMN number TYPE bigint USING number::bigint;

-- ---------------------------------------------------------------------------
-- source CHECKs gain github and bitbucket
-- ---------------------------------------------------------------------------
ALTER TABLE issue_state_changes DROP CONSTRAINT issue_state_changes_source_check;
ALTER TABLE issue_state_changes ADD CONSTRAINT issue_state_changes_source_check
    CHECK (source IN ('api', 'gitlab', 'github', 'bitbucket', 'import', 'system'));
ALTER TABLE issue_activity DROP CONSTRAINT issue_activity_source_check;
ALTER TABLE issue_activity ADD CONSTRAINT issue_activity_source_check
    CHECK (source IN ('api', 'gitlab', 'github', 'bitbucket', 'import', 'system'));
