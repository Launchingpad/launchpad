-- LAUN-199 (T34, docs/internal/decisions-T34-connected-accounts.md): a
-- member's handle on one git connection. A link is a CLAIM the member typed.
-- It is read for display only (who shows as the author of a change request)
-- and never for a permission. No token, no email and no OAuth grant here.
--
-- Rules the schema holds:
--   * one handle per member per connection (git_identities_user_key);
--   * one holder per handle per connection, compared in lower case
--     (git_identities_handle_key): a second claim is a 409, so a claim cannot
--     take over an existing link;
--   * a row goes with its member (org_members) and with its connection.
-- NOTE: numbered 0032 while develop is at 0030 and the Slack ticket takes
-- 0031; renumber on merge if the order changes.

CREATE TABLE git_identities (
    id             uuid        NOT NULL PRIMARY KEY DEFAULT uuidv7(),
    org_id         uuid        NOT NULL,
    user_id        uuid        NOT NULL,
    integration_id uuid        NOT NULL,
    -- handle is what the member typed (shown back); handle_key is its lower case.
    handle         text        NOT NULL,
    handle_key     text        NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT git_identities_member_fk FOREIGN KEY (org_id, user_id)
        REFERENCES org_members (org_id, user_id) ON DELETE CASCADE,
    CONSTRAINT git_identities_integration_fk FOREIGN KEY (org_id, integration_id)
        REFERENCES git_integrations (org_id, id) ON DELETE CASCADE,
    CONSTRAINT git_identities_user_key UNIQUE (org_id, user_id, integration_id),
    CONSTRAINT git_identities_handle_key UNIQUE (integration_id, handle_key),
    CONSTRAINT git_identities_handle_check CHECK (char_length(handle) BETWEEN 1 AND 100),
    CONSTRAINT git_identities_key_check CHECK (handle_key = lower(handle))
);
