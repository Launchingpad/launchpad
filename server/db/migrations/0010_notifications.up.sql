-- Notifications (LAUN-31, docs/internal/decisions-T13-notifications.md #1 to #8).
-- Eight tables: the user's master settings, sparse per-event settings, team
-- follows, per-issue subscriptions, the inbox rows, the email outbox, one
-- consumer cursor per org and the one-click unsubscribe tokens.
-- Every column is NOT NULL unless it says NULL. Every enum column is NOT NULL
-- with a CHECK. Every reference to an org row is a composite (org_id, x) key.
-- Every key to org_members, teams, issues or notifications cascades; the only
-- SET NULL is notifications.actor_id. No IP, no User-Agent, no body text.

-- #2: one row per member; no row means all defaults. A later migration widens
-- the delivery CHECK to hourly and daily.
CREATE TABLE notification_user_settings (
    org_id          uuid        NOT NULL,
    user_id         uuid        NOT NULL,
    email_enabled   boolean     NOT NULL DEFAULT true,
    include_excerpt boolean     NOT NULL DEFAULT false,
    delivery        text        NOT NULL DEFAULT 'immediate'
                    CONSTRAINT notification_user_settings_delivery_check CHECK (delivery IN ('immediate')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id),
    CONSTRAINT notification_user_settings_member_fk FOREIGN KEY (org_id, user_id)
        REFERENCES org_members (org_id, user_id) ON DELETE CASCADE
);

-- #3: sparse rows; a NULL column inherits. team_id NULL is the global level.
CREATE TABLE notification_settings (
    id      uuid NOT NULL PRIMARY KEY DEFAULT uuidv7(),
    org_id  uuid NOT NULL,
    user_id uuid NOT NULL,
    team_id uuid,
    event   text NOT NULL
            CONSTRAINT notification_settings_event_check CHECK (event IN ('assigned', 'state_change', 'comment', 'mention', 'new_issue')),
    email   text CONSTRAINT notification_settings_email_check CHECK (email IN ('on', 'off')),
    inbox   text CONSTRAINT notification_settings_inbox_check CHECK (inbox IN ('on', 'off')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT notification_settings_member_fk FOREIGN KEY (org_id, user_id)
        REFERENCES org_members (org_id, user_id) ON DELETE CASCADE,
    -- A NULL team_id skips this check (MATCH SIMPLE).
    CONSTRAINT notification_settings_team_fk FOREIGN KEY (org_id, team_id)
        REFERENCES teams (org_id, id) ON DELETE CASCADE
);
-- A NULL team_id is never equal to another NULL: two partial unique indexes.
-- Serves the cascade scan when a team is deleted.
CREATE INDEX notification_settings_team_idx ON notification_settings (org_id, team_id) WHERE team_id IS NOT NULL;
CREATE UNIQUE INDEX notification_settings_global_key
    ON notification_settings (org_id, user_id, event) WHERE team_id IS NULL;
CREATE UNIQUE INDEX notification_settings_team_key
    ON notification_settings (org_id, user_id, team_id, event) WHERE team_id IS NOT NULL;

-- #4
CREATE TABLE team_follows (
    org_id     uuid        NOT NULL,
    user_id    uuid        NOT NULL,
    team_id    uuid        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id, team_id),
    CONSTRAINT team_follows_member_fk FOREIGN KEY (org_id, user_id)
        REFERENCES org_members (org_id, user_id) ON DELETE CASCADE,
    CONSTRAINT team_follows_team_fk FOREIGN KEY (org_id, team_id)
        REFERENCES teams (org_id, id) ON DELETE CASCADE
);
CREATE INDEX team_follows_team_idx ON team_follows (org_id, team_id);

-- #5
CREATE TABLE issue_subscriptions (
    org_id     uuid        NOT NULL,
    issue_id   uuid        NOT NULL,
    user_id    uuid        NOT NULL,
    state      text        NOT NULL
               CONSTRAINT issue_subscriptions_state_check CHECK (state IN ('subscribed', 'muted')),
    reason     text        NOT NULL
               CONSTRAINT issue_subscriptions_reason_check CHECK (reason IN ('manual', 'mentioned')),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, issue_id, user_id),
    CONSTRAINT issue_subscriptions_issue_fk FOREIGN KEY (org_id, issue_id)
        REFERENCES issues (org_id, id) ON DELETE CASCADE,
    CONSTRAINT issue_subscriptions_member_fk FOREIGN KEY (org_id, user_id)
        REFERENCES org_members (org_id, user_id) ON DELETE CASCADE
);
CREATE INDEX issue_subscriptions_user_idx ON issue_subscriptions (org_id, user_id);

-- #6: the inbox rows; email reads them too. data holds ids only.
CREATE TABLE notifications (
    id           uuid        NOT NULL PRIMARY KEY DEFAULT uuidv7(),
    org_id       uuid        NOT NULL,
    recipient_id uuid        NOT NULL,
    kind         text        NOT NULL
                 CONSTRAINT notifications_kind_check CHECK (kind IN ('assigned', 'state_change', 'comment', 'mention', 'new_issue')),
    issue_id     uuid        NOT NULL,
    actor_id     uuid        REFERENCES users (id) ON DELETE SET NULL,
    data         jsonb       NOT NULL DEFAULT '{}',
    dedupe_key   text        NOT NULL,
    event_seq    bigint,
    inbox_seq    bigint      GENERATED ALWAYS AS IDENTITY,
    inbox        boolean     NOT NULL,
    read_at      timestamptz,
    archived_at  timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT notifications_org_id_id_key UNIQUE (org_id, id),
    -- Exactly once: inserts use ON CONFLICT DO NOTHING.
    CONSTRAINT notifications_dedupe_key UNIQUE (org_id, recipient_id, dedupe_key),
    CONSTRAINT notifications_recipient_fk FOREIGN KEY (org_id, recipient_id)
        REFERENCES org_members (org_id, user_id) ON DELETE CASCADE,
    CONSTRAINT notifications_issue_fk FOREIGN KEY (org_id, issue_id)
        REFERENCES issues (org_id, id) ON DELETE CASCADE
);
CREATE INDEX notifications_list_idx ON notifications (org_id, recipient_id, created_at DESC, id DESC)
    WHERE archived_at IS NULL AND inbox;
CREATE INDEX notifications_unread_idx ON notifications (org_id, recipient_id)
    WHERE read_at IS NULL AND archived_at IS NULL AND inbox;
CREATE INDEX notifications_stream_idx ON notifications (org_id, recipient_id, inbox_seq);
CREATE INDEX notifications_created_idx ON notifications (created_at);
-- Serves the cascade scan when an issue is deleted.
CREATE INDEX notifications_issue_idx ON notifications (org_id, issue_id);

-- #7: the email outbox. A minimal insert (the ids) is claimable.
CREATE TABLE notification_emails (
    id              uuid        NOT NULL PRIMARY KEY DEFAULT uuidv7(),
    org_id          uuid        NOT NULL,
    notification_id uuid        NOT NULL,
    recipient_id    uuid        NOT NULL,
    issue_id        uuid        NOT NULL,
    state           text        NOT NULL DEFAULT 'pending'
                    CONSTRAINT notification_emails_state_check CHECK (state IN ('pending', 'sending', 'sent', 'skipped', 'dead')),
    skip_reason     text
                    CONSTRAINT notification_emails_skip_reason_check CHECK (skip_reason IN (
                        'no_mail', 'no_address', 'unproven', 'no_base_url', 'rate_cap', 'expired', 'disabled')),
    attempts        int         NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    batch_id        uuid,
    last_error      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    sent_at         timestamptz,
    CONSTRAINT notification_emails_notification_key UNIQUE (notification_id),
    CONSTRAINT notification_emails_sent_check CHECK (state <> 'sent' OR (sent_at IS NOT NULL AND batch_id IS NOT NULL)),
    CONSTRAINT notification_emails_notification_fk FOREIGN KEY (org_id, notification_id)
        REFERENCES notifications (org_id, id) ON DELETE CASCADE,
    CONSTRAINT notification_emails_member_fk FOREIGN KEY (org_id, recipient_id)
        REFERENCES org_members (org_id, user_id) ON DELETE CASCADE,
    CONSTRAINT notification_emails_issue_fk FOREIGN KEY (org_id, issue_id)
        REFERENCES issues (org_id, id) ON DELETE CASCADE
);
CREATE INDEX notification_emails_claim_idx ON notification_emails (next_attempt_at) WHERE state = 'pending';
CREATE INDEX notification_emails_pending_idx ON notification_emails (org_id, recipient_id, issue_id) WHERE state = 'pending';
CREATE INDEX notification_emails_created_idx ON notification_emails (created_at);
-- Serve the cascade scans for a member and an issue.
CREATE INDEX notification_emails_member_idx ON notification_emails (org_id, recipient_id);
CREATE INDEX notification_emails_issue_idx ON notification_emails (org_id, issue_id);

-- #8: the last event seq the consumer processed, per org.
CREATE TABLE notification_cursors (
    org_id     uuid        NOT NULL PRIMARY KEY REFERENCES orgs (id) ON DELETE CASCADE,
    last_seq   bigint      NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- No history on first run: a cursor per existing org at its current max seq.
INSERT INTO notification_cursors (org_id, last_seq)
SELECT o.id, COALESCE((SELECT max(e.seq) FROM events e WHERE e.org_id = o.id), 0)
FROM orgs o;

-- A new org gets its cursor (at 0: it has no events) in the same statement,
-- whichever code path makes the org (setup, tests, a later org-create route).
CREATE FUNCTION notification_cursor_for_new_org() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO notification_cursors (org_id) VALUES (NEW.id) ON CONFLICT DO NOTHING;
    RETURN NEW;
END
$$;
CREATE TRIGGER orgs_notification_cursor AFTER INSERT ON orgs
    FOR EACH ROW EXECUTE FUNCTION notification_cursor_for_new_org();

-- #8: one-click unsubscribe tokens; only the hash is stored. targets is a list
-- of 1 to 5 {event, teamId} objects (teamId null: the global level).
CREATE TABLE notification_mail_tokens (
    token_hash bytea       NOT NULL PRIMARY KEY,
    org_id     uuid        NOT NULL,
    user_id    uuid        NOT NULL,
    targets    jsonb       NOT NULL
               CONSTRAINT notification_mail_tokens_targets_check CHECK (
                   jsonb_typeof(targets) = 'array' AND jsonb_array_length(targets) BETWEEN 1 AND 5),
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    CONSTRAINT notification_mail_tokens_member_fk FOREIGN KEY (org_id, user_id)
        REFERENCES org_members (org_id, user_id) ON DELETE CASCADE
);
CREATE INDEX notification_mail_tokens_expires_idx ON notification_mail_tokens (expires_at);
CREATE INDEX notification_mail_tokens_member_idx ON notification_mail_tokens (org_id, user_id);
