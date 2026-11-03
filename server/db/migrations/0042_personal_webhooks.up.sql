-- 0042_personal_webhooks: LAUN-229 (decisions-T40-personal-webhooks.md). A
-- member's own webhooks: they post when an issue is assigned to the member or
-- when an issue the member owns changes. Private rows of one member of one org:
-- no admin reads them and no event names them. The webhook URL is sealed and
-- never read back (the chat keyring, its own column name and scope).

CREATE TABLE personal_webhooks (
    id           uuid        NOT NULL PRIMARY KEY DEFAULT uuidv7(),
    org_id       uuid        NOT NULL,
    user_id      uuid        NOT NULL,
    name         text        NOT NULL,
    kind         text        NOT NULL,
    -- The host name alone, for the member's page. Not secret.
    host         text        NOT NULL,
    -- AES-256-GCM blob (nonce || ciphertext || tag) of the webhook URL.
    webhook_enc  bytea       NOT NULL,
    -- The additional data of the blob: 'pwh.<org id>.<user id>.<id>'. Checked in code.
    seal_scope   text        NOT NULL,
    -- assigned: an issue is assigned to the member; my_issues: an issue the
    -- member owns (assignee or creator) changes.
    events       text[]      NOT NULL DEFAULT ARRAY['assigned', 'my_issues']::text[],
    enabled      boolean     NOT NULL DEFAULT true,
    -- The result of the last real or test post, as a fixed word. Never a reply body.
    last_ok_at   timestamptz,
    last_error   text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT personal_webhooks_org_id_key UNIQUE (org_id, id),
    -- A member who leaves the org takes the webhooks along.
    CONSTRAINT personal_webhooks_member_fk FOREIGN KEY (org_id, user_id)
        REFERENCES org_members (org_id, user_id) ON DELETE CASCADE,
    CONSTRAINT personal_webhooks_name_check CHECK (char_length(name) BETWEEN 1 AND 40),
    CONSTRAINT personal_webhooks_kind_check CHECK (kind IN ('slack', 'discord', 'fluxer', 'custom')),
    CONSTRAINT personal_webhooks_host_check CHECK (char_length(host) BETWEEN 1 AND 255),
    CONSTRAINT personal_webhooks_blob_check CHECK (octet_length(webhook_enc) BETWEEN 29 AND 2048),
    CONSTRAINT personal_webhooks_events_check CHECK (events <@ ARRAY['assigned', 'my_issues']::text[]),
    CONSTRAINT personal_webhooks_error_check CHECK (last_error IS NULL OR last_error IN (
        'chat_rejected', 'chat_gone', 'chat_rate_limited', 'chat_unavailable',
        'chat_unreachable', 'chat_blocked', 'chat_redirect', 'chat_unreadable'))
);
CREATE UNIQUE INDEX personal_webhooks_name_idx ON personal_webhooks (org_id, user_id, lower(name));

-- The outbox, one row per post per webhook. It holds ids and a kind only.
CREATE TABLE personal_webhook_outbox (
    id              uuid        NOT NULL PRIMARY KEY DEFAULT uuidv7(),
    org_id          uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    webhook_id      uuid        NOT NULL,
    kind            text        NOT NULL,
    issue_id        uuid        NOT NULL,
    dedupe_key      text        NOT NULL,
    event_seq       bigint,
    state           text        NOT NULL DEFAULT 'pending',
    skip_reason     text,
    attempts        integer     NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    sent_at         timestamptz,
    CONSTRAINT personal_webhook_outbox_dedupe_key UNIQUE (webhook_id, dedupe_key),
    CONSTRAINT personal_webhook_outbox_webhook_fk FOREIGN KEY (org_id, webhook_id)
        REFERENCES personal_webhooks (org_id, id) ON DELETE CASCADE,
    CONSTRAINT personal_webhook_outbox_issue_fk FOREIGN KEY (org_id, issue_id)
        REFERENCES issues (org_id, id) ON DELETE CASCADE,
    CONSTRAINT personal_webhook_outbox_kind_check CHECK (kind IN (
        'assigned', 'state_change', 'comment', 'issue_updated')),
    CONSTRAINT personal_webhook_outbox_state_check CHECK (state IN ('pending', 'sending', 'sent', 'skipped', 'dead')),
    CONSTRAINT personal_webhook_outbox_skip_check CHECK (skip_reason IS NULL OR skip_reason IN (
        'disabled', 'not_selected', 'unreadable', 'no_base_url', 'rate_cap', 'expired', 'gone', 'access_removed')),
    CONSTRAINT personal_webhook_outbox_error_check CHECK (last_error IS NULL OR last_error IN (
        'chat_rejected', 'chat_gone', 'chat_rate_limited', 'chat_unavailable',
        'chat_unreachable', 'chat_blocked', 'chat_redirect', 'chat_unreadable')),
    CONSTRAINT personal_webhook_outbox_sent_check CHECK (state <> 'sent' OR sent_at IS NOT NULL)
);
CREATE INDEX personal_webhook_outbox_due_idx ON personal_webhook_outbox (next_attempt_at) WHERE state = 'pending';
CREATE INDEX personal_webhook_outbox_sent_idx ON personal_webhook_outbox (webhook_id, sent_at) WHERE state = 'sent';
CREATE INDEX personal_webhook_outbox_created_idx ON personal_webhook_outbox (created_at);
CREATE INDEX personal_webhook_outbox_issue_idx ON personal_webhook_outbox (org_id, issue_id);
