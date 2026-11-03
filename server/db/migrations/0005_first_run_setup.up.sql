-- 0005_first_run_setup: first-run setup replaces the boot seed (ORB-3432,
-- docs/decisions-T8-first-run.md).
--
-- users.is_platform_admin marks the person who runs this install. It is not
-- an org role: a platform admin may belong to no org at all. Setup makes the
-- first one; `server grant-platform-admin --user <name>` makes one on an
-- install that existed before this migration (every existing user starts
-- false, so an upgrade grants nothing by itself).
ALTER TABLE users ADD COLUMN is_platform_admin boolean NOT NULL DEFAULT false;

-- setup_tokens: the one-time link the server logs at boot while no user
-- exists. Only sha256(token) is stored. A link lives 24 hours and is used
-- once: completing setup sets consumed_at, and minting a new link (a restart,
-- or `server setup-link`) closes every older one. The table holds a few rows
-- per install, so it needs no index beyond the unique hash.
CREATE TABLE setup_tokens (
    id           uuid        PRIMARY KEY DEFAULT uuidv7(),
    token_hash   bytea       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    consumed_at  timestamptz,
    CONSTRAINT setup_tokens_hash_key UNIQUE (token_hash),
    CONSTRAINT setup_tokens_hash_len CHECK (octet_length(token_hash) = 32),
    CONSTRAINT setup_tokens_expiry_check CHECK (expires_at > created_at)
);
