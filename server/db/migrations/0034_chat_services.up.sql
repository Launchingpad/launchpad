-- 0034_chat_services: LAUN-204 (decisions-T36-chat-services.md). The org's one
-- Slack channel (0031) becomes a list of chat services: Slack, Discord and a
-- custom webhook. The Slack channel of an org moves into one `slack` service
-- named 'Slack' and the waiting posts move with it (mapped to that service).
-- The webhook URL is still sealed and never read back. A migrated blob keeps
-- its old additional data (seal_scope = the org id), so it still opens with
-- the same key; a new service is sealed under its own scope.

CREATE TABLE chat_services (
    id           uuid        NOT NULL PRIMARY KEY DEFAULT uuidv7(),
    org_id       uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    name         text        NOT NULL,
    kind         text        NOT NULL,
    -- The host name alone, for the admin page. Not secret (the path holds the token).
    host         text        NOT NULL,
    -- AES-256-GCM blob (nonce || ciphertext || tag) of the webhook URL.
    webhook_enc  bytea       NOT NULL,
    -- The additional data of the blob: the org id (a blob migrated from
    -- slack_channels) or 'svc.<org id>.<service id>'. Checked in code.
    seal_scope   text        NOT NULL,
    -- The event kinds the service gets. Empty: nothing but the test message.
    events       text[]      NOT NULL DEFAULT '{}',
    enabled      boolean     NOT NULL DEFAULT true,
    -- The result of the last real or test post, as a fixed word. Never a reply body.
    last_ok_at   timestamptz,
    last_error   text,
    updated_by   uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT chat_services_org_id_key UNIQUE (org_id, id),
    CONSTRAINT chat_services_name_check CHECK (char_length(name) BETWEEN 1 AND 40),
    CONSTRAINT chat_services_kind_check CHECK (kind IN ('slack', 'discord', 'custom')),
    CONSTRAINT chat_services_host_check CHECK (char_length(host) BETWEEN 1 AND 255),
    CONSTRAINT chat_services_blob_check CHECK (octet_length(webhook_enc) BETWEEN 29 AND 2048),
    CONSTRAINT chat_services_events_check CHECK (events <@ ARRAY[
        'assigned', 'mention', 'comment', 'state_change', 'new_issue', 'project_update_due']::text[]),
    CONSTRAINT chat_services_error_check CHECK (last_error IS NULL OR last_error IN (
        'chat_rejected', 'chat_gone', 'chat_rate_limited', 'chat_unavailable',
        'chat_unreachable', 'chat_blocked', 'chat_redirect', 'chat_unreadable'))
);
CREATE UNIQUE INDEX chat_services_name_idx ON chat_services (org_id, lower(name));

INSERT INTO chat_services (org_id, name, kind, host, webhook_enc, seal_scope, events, enabled,
                           last_ok_at, last_error, updated_by, created_at, updated_at)
SELECT org_id, 'Slack', 'slack', 'hooks.slack.com', webhook_enc, org_id::text, events, true,
       last_ok_at, regexp_replace(last_error, '^slack_', 'chat_'), updated_by, created_at, updated_at
FROM slack_channels;

-- The outbox, one row per post per service. It holds ids and a kind only.
CREATE TABLE chat_outbox (
    id              uuid        NOT NULL PRIMARY KEY DEFAULT uuidv7(),
    org_id          uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    service_id      uuid        NOT NULL,
    kind            text        NOT NULL,
    issue_id        uuid,
    project_id      uuid,
    dedupe_key      text        NOT NULL,
    event_seq       bigint,
    state           text        NOT NULL DEFAULT 'pending',
    skip_reason     text,
    attempts        integer     NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    sent_at         timestamptz,
    CONSTRAINT chat_outbox_dedupe_key UNIQUE (service_id, dedupe_key),
    CONSTRAINT chat_outbox_service_fk FOREIGN KEY (org_id, service_id)
        REFERENCES chat_services (org_id, id) ON DELETE CASCADE,
    CONSTRAINT chat_outbox_issue_fk FOREIGN KEY (org_id, issue_id)
        REFERENCES issues (org_id, id) ON DELETE CASCADE,
    CONSTRAINT chat_outbox_project_fk FOREIGN KEY (org_id, project_id)
        REFERENCES projects (org_id, id) ON DELETE CASCADE,
    CONSTRAINT chat_outbox_one_target CHECK (num_nonnulls(issue_id, project_id) = 1),
    CONSTRAINT chat_outbox_kind_check CHECK (kind IN (
        'assigned', 'mention', 'comment', 'state_change', 'new_issue', 'project_update_due')),
    CONSTRAINT chat_outbox_kind_target CHECK ((kind = 'project_update_due') = (project_id IS NOT NULL)),
    CONSTRAINT chat_outbox_state_check CHECK (state IN ('pending', 'sending', 'sent', 'skipped', 'dead')),
    CONSTRAINT chat_outbox_skip_check CHECK (skip_reason IS NULL OR skip_reason IN (
        'disabled', 'not_selected', 'unreadable', 'no_base_url', 'rate_cap', 'expired', 'gone')),
    CONSTRAINT chat_outbox_error_check CHECK (last_error IS NULL OR last_error IN (
        'chat_rejected', 'chat_gone', 'chat_rate_limited', 'chat_unavailable',
        'chat_unreachable', 'chat_blocked', 'chat_redirect', 'chat_unreadable')),
    CONSTRAINT chat_outbox_sent_check CHECK (state <> 'sent' OR sent_at IS NOT NULL)
);
CREATE INDEX chat_outbox_due_idx ON chat_outbox (next_attempt_at) WHERE state = 'pending';
CREATE INDEX chat_outbox_sent_idx ON chat_outbox (service_id, sent_at) WHERE state = 'sent';
CREATE INDEX chat_outbox_created_idx ON chat_outbox (created_at);

-- Every row of an org with a Slack channel moves to that org's one service.
-- Rows of an org with no channel were only ever going to be skipped.
INSERT INTO chat_outbox (id, org_id, service_id, kind, issue_id, project_id, dedupe_key, event_seq, state,
                         skip_reason, attempts, next_attempt_at, last_error, created_at, sent_at)
SELECT o.id, o.org_id, s.id, o.kind, o.issue_id, o.project_id, o.dedupe_key, o.event_seq, o.state,
       o.skip_reason, o.attempts, o.next_attempt_at, regexp_replace(o.last_error, '^slack_', 'chat_'),
       o.created_at, o.sent_at
FROM slack_outbox o
JOIN chat_services s ON s.org_id = o.org_id AND s.kind = 'slack';

DROP TABLE slack_outbox;
DROP TABLE slack_channels;
