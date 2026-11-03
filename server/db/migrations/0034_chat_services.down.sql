-- LAUN-204: reverses 0034. One Slack channel per org can come back: the
-- `slack` service whose blob still has its migrated scope (seal_scope = the
-- org id). It cannot restore (and so loses): Discord and custom services,
-- extra Slack services (sealed under their own scope, which the old column
-- cannot carry), and the posts waiting for them. A disabled service returns
-- with an empty event list (the old table has no off switch).

CREATE TABLE slack_channels (
    org_id       uuid        NOT NULL PRIMARY KEY REFERENCES orgs (id) ON DELETE CASCADE,
    -- AES-256-GCM blob (nonce || ciphertext || tag) of the webhook URL.
    webhook_enc  bytea       NOT NULL,
    -- The event kinds the channel gets. Empty: the channel gets nothing but the test message.
    events       text[]      NOT NULL DEFAULT '{}',
    -- The result of the last real or test post, as a fixed word. Never a reply body.
    last_ok_at   timestamptz,
    last_error   text,
    updated_by   uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT slack_channels_blob_check CHECK (octet_length(webhook_enc) BETWEEN 29 AND 2048),
    CONSTRAINT slack_channels_events_check CHECK (events <@ ARRAY[
        'assigned', 'mention', 'comment', 'state_change', 'new_issue', 'project_update_due']::text[]),
    CONSTRAINT slack_channels_error_check CHECK (last_error IS NULL OR last_error IN (
        'slack_rejected', 'slack_gone', 'slack_rate_limited', 'slack_unavailable',
        'slack_unreachable', 'slack_blocked', 'slack_redirect', 'slack_unreadable'))
);

INSERT INTO slack_channels (org_id, webhook_enc, events, last_ok_at, last_error, updated_by, created_at, updated_at)
SELECT DISTINCT ON (org_id) org_id, webhook_enc, CASE WHEN enabled THEN events ELSE '{}'::text[] END,
       last_ok_at, regexp_replace(last_error, '^chat_', 'slack_'), updated_by, created_at, updated_at
FROM chat_services
WHERE kind = 'slack' AND seal_scope = org_id::text
ORDER BY org_id, created_at, id;

CREATE TABLE slack_outbox (
    id              uuid        NOT NULL PRIMARY KEY DEFAULT uuidv7(),
    org_id          uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
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
    CONSTRAINT slack_outbox_dedupe_key UNIQUE (org_id, dedupe_key),
    CONSTRAINT slack_outbox_issue_fk FOREIGN KEY (org_id, issue_id)
        REFERENCES issues (org_id, id) ON DELETE CASCADE,
    CONSTRAINT slack_outbox_project_fk FOREIGN KEY (org_id, project_id)
        REFERENCES projects (org_id, id) ON DELETE CASCADE,
    CONSTRAINT slack_outbox_one_target CHECK (num_nonnulls(issue_id, project_id) = 1),
    CONSTRAINT slack_outbox_kind_check CHECK (kind IN (
        'assigned', 'mention', 'comment', 'state_change', 'new_issue', 'project_update_due')),
    CONSTRAINT slack_outbox_kind_target CHECK ((kind = 'project_update_due') = (project_id IS NOT NULL)),
    CONSTRAINT slack_outbox_state_check CHECK (state IN ('pending', 'sending', 'sent', 'skipped', 'dead')),
    CONSTRAINT slack_outbox_skip_check CHECK (skip_reason IS NULL OR skip_reason IN (
        'disabled', 'not_selected', 'unreadable', 'no_base_url', 'rate_cap', 'expired', 'gone')),
    CONSTRAINT slack_outbox_error_check CHECK (last_error IS NULL OR last_error IN (
        'slack_rejected', 'slack_gone', 'slack_rate_limited', 'slack_unavailable',
        'slack_unreachable', 'slack_blocked', 'slack_redirect', 'slack_unreadable')),
    CONSTRAINT slack_outbox_sent_check CHECK (state <> 'sent' OR sent_at IS NOT NULL)
);
CREATE INDEX slack_outbox_due_idx ON slack_outbox (next_attempt_at) WHERE state = 'pending';
CREATE INDEX slack_outbox_sent_idx ON slack_outbox (org_id, sent_at) WHERE state = 'sent';
CREATE INDEX slack_outbox_created_idx ON slack_outbox (created_at);

INSERT INTO slack_outbox (id, org_id, kind, issue_id, project_id, dedupe_key, event_seq, state,
                          skip_reason, attempts, next_attempt_at, last_error, created_at, sent_at)
SELECT o.id, o.org_id, o.kind, o.issue_id, o.project_id, o.dedupe_key, o.event_seq, o.state,
       o.skip_reason, o.attempts, o.next_attempt_at, regexp_replace(o.last_error, '^chat_', 'slack_'),
       o.created_at, o.sent_at
FROM chat_outbox o
JOIN slack_channels c ON c.org_id = o.org_id
JOIN chat_services s ON s.id = o.service_id AND s.kind = 'slack' AND s.seal_scope = s.org_id::text;

DROP TABLE chat_outbox;
DROP TABLE chat_services;
