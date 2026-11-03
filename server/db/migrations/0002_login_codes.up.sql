-- 0002_login_codes: one-time email login codes for the web app (T6a,
-- ORB-3414). A code is 6 digits, lives 10 minutes and is used once.
--
-- The code itself is never stored: code_hash =
-- HMAC-SHA256(pepper, code_salt || code), with a random 16-byte salt per row
-- and a server-side pepper (AUTH_OTP_PEPPER) that is never in the database. A person has at most one live code: a
-- new request closes the older ones (consumed_at), and five wrong tries lock
-- the row (attempts >= 5). Sessions minted from a code are ordinary
-- api_tokens rows of kind 'session'.
CREATE TABLE login_codes (
    id           uuid        PRIMARY KEY DEFAULT uuidv7(),
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_salt    bytea       NOT NULL,
    code_hash    bytea       NOT NULL,
    attempts     integer     NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    -- Set when the code is used, or replaced by a newer code.
    consumed_at  timestamptz,
    CONSTRAINT login_codes_salt_len CHECK (octet_length(code_salt) = 16),
    CONSTRAINT login_codes_hash_len CHECK (octet_length(code_hash) = 32),
    CONSTRAINT login_codes_attempts_check CHECK (attempts >= 0),
    CONSTRAINT login_codes_expiry_check CHECK (expires_at > created_at)
);
-- Serves the user_id foreign key, the per-hour count and "newest live code".
CREATE INDEX login_codes_user_created_idx ON login_codes (user_id, created_at DESC);
