-- LAUN-82 (T17 #8): down of migration A. DOWN POLICY: this down deletes or
-- rewrites what the old schema cannot hold; it never refuses.
--   * integrations of a provider other than gitlab are deleted, and with them
--     (cascade) their repo map rows, events and MR links;
--   * GitLab map rows, event rows and links whose repo_key is not all digits
--     (or whose number does not fit an integer) are deleted;
--   * event rows that would collide on the old (org_id, event_uuid) key are
--     deleted, keeping the oldest; the gl: prefix is stripped;
--   * source values github and bitbucket are rewritten to system (history
--     is kept);
--   * GitLab integrations with mode = 'token' lose token_enc and mode (the old
--     schema has neither; they become plain hash-verified integrations);
--   * the sealed columns and every other new column are dropped.

ALTER TABLE issue_activity DROP CONSTRAINT issue_activity_source_check;
UPDATE issue_activity SET source = 'system' WHERE source IN ('github', 'bitbucket');
ALTER TABLE issue_activity ADD CONSTRAINT issue_activity_source_check
    CHECK (source IN ('api', 'gitlab', 'import', 'system'));
ALTER TABLE issue_state_changes DROP CONSTRAINT issue_state_changes_source_check;
UPDATE issue_state_changes SET source = 'system' WHERE source IN ('github', 'bitbucket');
ALTER TABLE issue_state_changes ADD CONSTRAINT issue_state_changes_source_check
    CHECK (source IN ('api', 'gitlab', 'import', 'system'));

DELETE FROM git_integrations WHERE provider <> 'gitlab';

-- mr_links
DELETE FROM mr_links WHERE repo_key !~ '^[0-9]{1,18}$' OR number > 2147483647 OR number < -2147483648;
ALTER TABLE mr_links ALTER COLUMN number TYPE integer USING number::integer;
ALTER TABLE mr_links RENAME COLUMN number TO mr_iid;
ALTER TABLE mr_links ALTER COLUMN repo_key TYPE bigint USING repo_key::bigint;
ALTER TABLE mr_links RENAME COLUMN repo_key TO gitlab_project_id;

-- git_webhook_events
ALTER TABLE git_webhook_events DROP CONSTRAINT git_webhook_events_uuid_key;
ALTER TABLE git_webhook_events DROP CONSTRAINT git_webhook_events_integration_fk;
UPDATE git_webhook_events SET event_uuid = substr(event_uuid, 4) WHERE event_uuid LIKE 'gl:%';
DELETE FROM git_webhook_events e
WHERE EXISTS (
    SELECT 1 FROM git_webhook_events o
    WHERE o.org_id = e.org_id AND o.event_uuid = e.event_uuid AND o.id <> e.id
      AND (o.received_at, o.id) < (e.received_at, e.id));
DELETE FROM git_webhook_events WHERE repo_key IS NOT NULL AND repo_key !~ '^[0-9]{1,18}$';
ALTER TABLE git_webhook_events DROP COLUMN integration_id;
ALTER TABLE git_webhook_events ALTER COLUMN repo_key TYPE bigint USING repo_key::bigint;
ALTER TABLE git_webhook_events RENAME COLUMN repo_key TO gitlab_project_id;
ALTER TABLE git_webhook_events
    ADD CONSTRAINT gitlab_webhook_events_uuid_key UNIQUE (org_id, event_uuid);

-- git_repo_map
DELETE FROM git_repo_map WHERE repo_key !~ '^[0-9]{1,18}$';
ALTER TABLE git_repo_map
    DROP CONSTRAINT git_repo_map_hook_state_check,
    DROP COLUMN hook_id,
    DROP COLUMN hook_state,
    DROP COLUMN hook_error;
ALTER TABLE git_repo_map RENAME COLUMN repo_path TO project_path;
ALTER TABLE git_repo_map ALTER COLUMN repo_key TYPE bigint USING repo_key::bigint;
ALTER TABLE git_repo_map RENAME COLUMN repo_key TO gitlab_project_id;

-- git_integrations
ALTER TABLE git_integrations DROP CONSTRAINT git_integrations_org_provider_base_key;
ALTER TABLE git_integrations
    DROP CONSTRAINT git_integrations_provider_check,
    DROP CONSTRAINT git_integrations_flavor_check,
    DROP CONSTRAINT git_integrations_mode_check,
    DROP CONSTRAINT git_integrations_token_mode_check,
    DROP CONSTRAINT git_integrations_next_hash_len,
    DROP COLUMN provider,
    DROP COLUMN flavor,
    DROP COLUMN mode,
    DROP COLUMN api_base_url,
    DROP COLUMN name,
    DROP COLUMN token_enc,
    DROP COLUMN webhook_secret_enc,
    DROP COLUMN webhook_secret_next_enc,
    DROP COLUMN webhook_secret_next_hash,
    DROP COLUMN attention_code;
ALTER TABLE git_integrations
    ADD CONSTRAINT gitlab_integrations_org_base_key UNIQUE (org_id, base_url);

-- names back
ALTER INDEX git_webhook_events_received_idx RENAME TO gitlab_webhook_events_received_idx;
ALTER TABLE git_webhook_events RENAME CONSTRAINT git_webhook_events_status_check TO gitlab_webhook_events_status_check;
ALTER TABLE git_webhook_events RENAME CONSTRAINT git_webhook_events_org_id_fkey TO gitlab_webhook_events_org_id_fkey;
ALTER TABLE git_webhook_events RENAME CONSTRAINT git_webhook_events_pkey TO gitlab_webhook_events_pkey;

ALTER INDEX git_repo_map_team_idx RENAME TO gitlab_project_map_team_idx;
ALTER INDEX git_repo_map_org_integration_idx RENAME TO gitlab_project_map_org_integration_idx;
ALTER TABLE git_repo_map RENAME CONSTRAINT git_repo_map_repo_key TO gitlab_project_map_project_key;
ALTER TABLE git_repo_map RENAME CONSTRAINT git_repo_map_team_fk TO gitlab_project_map_team_fk;
ALTER TABLE git_repo_map RENAME CONSTRAINT git_repo_map_integration_fk TO gitlab_project_map_integration_fk;
ALTER TABLE git_repo_map RENAME CONSTRAINT git_repo_map_pkey TO gitlab_project_map_pkey;

ALTER TABLE git_integrations RENAME CONSTRAINT git_integrations_org_id_fkey TO gitlab_integrations_org_id_fkey;
ALTER TABLE git_integrations RENAME CONSTRAINT git_integrations_hash_len TO gitlab_integrations_hash_len;
ALTER TABLE git_integrations RENAME CONSTRAINT git_integrations_org_id_id_key TO gitlab_integrations_org_id_id_key;
ALTER TABLE git_integrations RENAME CONSTRAINT git_integrations_pkey TO gitlab_integrations_pkey;

ALTER TABLE git_webhook_events RENAME TO gitlab_webhook_events;
ALTER TABLE git_repo_map RENAME TO gitlab_project_map;
ALTER TABLE git_integrations RENAME TO gitlab_integrations;
