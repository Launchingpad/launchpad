-- LAUN-223 (decisions-T20 #45, review): the chat services decide whether a
-- mention is new from their own record, never from the mentioned person's
-- notification rows. Those rows exist only for a person who sees the team, so
-- reading them made the chat post count an access probe. One row per mention
-- key (the target and the mentioned user's id; no text, no address); the
-- event that claimed it first, so a replay decides the same way.
CREATE TABLE chat_mentions (
    org_id     uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    dedupe_key text        NOT NULL,
    event_seq  bigint      NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, dedupe_key)
);
