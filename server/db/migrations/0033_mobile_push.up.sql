-- 0033_mobile_push: LAUN-197 (docs/internal/decisions-T35-mobile-push.md).
-- Mobile push through a RELAY. This server never holds an APNs or FCM key: a
-- relay service run by the app's publisher does. An org admin enters the relay
-- URL and a bearer secret (both sealed, write-only). A member registers the
-- device tokens of their own phones. A third channel, push, sits next to email
-- and inbox in the member's notification settings (default off). A push is a
-- WAKE: the relay body holds a device token, a platform, an opaque outbox id
-- and the word "wake", and no text of the issue.

-- The org's relay. One row per org. url_enc and secret_enc are AES-256-GCM
-- blobs (nonce || ciphertext || tag) and are never read back. host is the
-- host name alone, kept in clear for the admin page.
CREATE TABLE push_relays (
    org_id      uuid        NOT NULL PRIMARY KEY REFERENCES orgs (id) ON DELETE CASCADE,
    url_enc     bytea       NOT NULL,
    secret_enc  bytea       NOT NULL,
    host        text        NOT NULL,
    -- The result of the last real or test push, as a fixed word. Never a reply body.
    last_ok_at  timestamptz,
    last_error  text,
    updated_by  uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT push_relays_url_blob_check CHECK (octet_length(url_enc) BETWEEN 29 AND 2048),
    CONSTRAINT push_relays_secret_blob_check CHECK (octet_length(secret_enc) BETWEEN 29 AND 2048),
    CONSTRAINT push_relays_host_check CHECK (char_length(host) BETWEEN 1 AND 253),
    CONSTRAINT push_relays_error_check CHECK (last_error IS NULL OR last_error IN (
        'push_rejected', 'push_unauthorized', 'push_rate_limited',
        'push_unavailable', 'push_unreachable', 'push_blocked', 'push_redirect', 'push_unreadable'))
);

-- A member's device. The id is chosen by the app (a UUID in the path). The
-- token is a credential-like identifier: it is sealed (the sender must send
-- it), never returned, never logged, never in an event. token_hash (sha256)
-- only keeps one row per token in an org.
CREATE TABLE push_devices (
    id            uuid        NOT NULL,
    org_id        uuid        NOT NULL,
    user_id       uuid        NOT NULL,
    platform      text        NOT NULL,
    token_enc     bytea       NOT NULL,
    token_hash    bytea       NOT NULL,
    app_version   text        NOT NULL DEFAULT '',
    label         text        NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT push_devices_member_fk FOREIGN KEY (org_id, user_id)
        REFERENCES org_members (org_id, user_id) ON DELETE CASCADE,
    -- The id is chosen by the client, so it is unique per org only: another org's
    -- device with the same id is no denial.
    CONSTRAINT push_devices_pkey PRIMARY KEY (org_id, id),
    CONSTRAINT push_devices_token_key UNIQUE (org_id, token_hash),
    CONSTRAINT push_devices_platform_check CHECK (platform IN ('ios', 'android')),
    CONSTRAINT push_devices_blob_check CHECK (octet_length(token_enc) BETWEEN 29 AND 8400),
    CONSTRAINT push_devices_hash_check CHECK (octet_length(token_hash) = 32),
    CONSTRAINT push_devices_version_check CHECK (char_length(app_version) <= 64),
    CONSTRAINT push_devices_label_check CHECK (char_length(label) <= 80)
);
CREATE INDEX push_devices_user_idx ON push_devices (org_id, user_id);
CREATE INDEX push_devices_seen_idx ON push_devices (last_seen_at);

-- The third channel of a member's settings. NULL inherits, like email and inbox.
ALTER TABLE notification_settings ADD COLUMN push text;
ALTER TABLE notification_settings ADD CONSTRAINT notification_settings_push_check CHECK (push IN ('on', 'off'));

-- The outbox: one row per (notification, device). Ids and a state only: no
-- title, no name, no text, no token.
CREATE TABLE push_outbox (
    id              uuid        NOT NULL PRIMARY KEY DEFAULT uuidv7(),
    org_id          uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    notification_id uuid        NOT NULL,
    device_id       uuid        NOT NULL,
    state           text        NOT NULL DEFAULT 'pending',
    skip_reason     text,
    attempts        integer     NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    sent_at         timestamptz,
    CONSTRAINT push_outbox_notification_fk FOREIGN KEY (org_id, notification_id)
        REFERENCES notifications (org_id, id) ON DELETE CASCADE,
    CONSTRAINT push_outbox_device_fk FOREIGN KEY (org_id, device_id)
        REFERENCES push_devices (org_id, id) ON DELETE CASCADE,
    CONSTRAINT push_outbox_pair_key UNIQUE (notification_id, device_id),
    CONSTRAINT push_outbox_state_check CHECK (state IN ('pending', 'sending', 'sent', 'skipped', 'dead')),
    CONSTRAINT push_outbox_skip_check CHECK (skip_reason IS NULL OR skip_reason IN (
        'disabled', 'unreadable', 'rate_cap', 'expired', 'gone', 'read')),
    CONSTRAINT push_outbox_error_check CHECK (last_error IS NULL OR last_error IN (
        'push_rejected', 'push_unauthorized', 'push_rate_limited',
        'push_unavailable', 'push_unreachable', 'push_blocked', 'push_redirect', 'push_unreadable')),
    CONSTRAINT push_outbox_sent_check CHECK (state <> 'sent' OR sent_at IS NOT NULL)
);
CREATE INDEX push_outbox_due_idx ON push_outbox (next_attempt_at) WHERE state = 'pending';
CREATE INDEX push_outbox_sent_idx ON push_outbox (org_id, sent_at) WHERE state = 'sent';
CREATE INDEX push_outbox_created_idx ON push_outbox (created_at);
