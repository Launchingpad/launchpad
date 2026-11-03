-- 0006_platform_settings: install-wide settings that a platform admin edits
-- in the app (ORB-3435, docs/decisions-T9-mail-settings.md): how login
-- email is sent, and the sign-up mode (stored here; ORB-3436 acts on it).
--
-- One row for the whole install. The primary key is a boolean that the CHECK
-- holds at true, so a second row cannot exist; the row is made here, so a
-- read always finds it.
--
-- Secrets (the Postmark server token, the SMTP password) are AES-256-GCM
-- blobs (nonce || ciphertext || tag), sealed with a key derived from
-- SETTINGS_ENCRYPTION_KEY. They are never stored in clear and never
-- returned by the API. key_check is a value derived from the same key with
-- another HKDF purpose: when it does not match the server's key, the stored
-- secrets cannot be opened, and the server falls back to the environment's
-- mail settings. NULL until the first secret is stored.
--
-- Non-secret text fields are NOT NULL DEFAULT '' (empty = not set); the API
-- validates them. The table has one row, so updated_by needs no index.
CREATE TABLE platform_settings (
    id                    boolean     PRIMARY KEY DEFAULT true,
    mail_provider         text        NOT NULL DEFAULT 'env',
    postmark_token_enc    bytea,
    postmark_from         text        NOT NULL DEFAULT '',
    postmark_stream       text        NOT NULL DEFAULT '',
    smtp_host             text        NOT NULL DEFAULT '',
    smtp_port             integer     NOT NULL DEFAULT 587,
    smtp_security         text        NOT NULL DEFAULT 'starttls',
    smtp_username         text        NOT NULL DEFAULT '',
    smtp_password_enc     bytea,
    smtp_from_address     text        NOT NULL DEFAULT '',
    smtp_from_name        text        NOT NULL DEFAULT '',
    key_check             bytea,
    signup_mode           text        NOT NULL DEFAULT 'invite',
    updated_at            timestamptz NOT NULL DEFAULT now(),
    updated_by            uuid        REFERENCES users (id) ON DELETE SET NULL,
    CONSTRAINT platform_settings_one_row CHECK (id),
    CONSTRAINT platform_settings_mail_provider_check
        CHECK (mail_provider IN ('env', 'none', 'postmark', 'smtp')),
    CONSTRAINT platform_settings_smtp_security_check
        CHECK (smtp_security IN ('starttls', 'tls', 'none')),
    CONSTRAINT platform_settings_smtp_port_check CHECK (smtp_port BETWEEN 0 AND 65535),
    CONSTRAINT platform_settings_signup_mode_check
        CHECK (signup_mode IN ('invite', 'approval', 'closed')),
    -- 12-byte nonce + 16-byte tag at least.
    CONSTRAINT platform_settings_postmark_token_len CHECK (octet_length(postmark_token_enc) >= 28),
    CONSTRAINT platform_settings_smtp_password_len CHECK (octet_length(smtp_password_enc) >= 28),
    CONSTRAINT platform_settings_key_check_len CHECK (octet_length(key_check) = 32)
);

INSERT INTO platform_settings (id) VALUES (true);
