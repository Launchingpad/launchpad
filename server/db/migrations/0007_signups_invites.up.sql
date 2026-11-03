-- 0007_signups_invites: sign-up requests and org invites (ORB-3436,
-- docs/decisions-T12-signups-invites.md). platform_settings.signup_mode
-- (0006) decides which of the two flows is open.
--
-- signup_requests: a person asks to join an org (signup_mode approval). NO
-- user, membership or session exists until an org admin approves the row.
-- The email confirm code is stored only as HMAC-SHA256(AUTH_OTP_PEPPER,
-- salt || code), as login codes are (0002). A pending row expires after 30
-- days (expires_at); while mail works, a request whose email is not
-- confirmed yet expires after 24 hours instead, and confirming it moves
-- expires_at to created_at + 30 days. The server marks rows expired lazily,
-- before each read and write of the table. A rejected row with blocked =
-- true stops new requests from that email in that org. code_attempts
-- counts the wrong tries of the open code; code_attempts_total counts them
-- across every code the request had, so asking again does not give more
-- guesses.
CREATE TABLE signup_requests (
    id                 uuid        PRIMARY KEY DEFAULT uuidv7(),
    org_id             uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    email              text        NOT NULL,
    name               text        NOT NULL,
    username           text        NOT NULL,
    note               text        NOT NULL DEFAULT '',
    status             text        NOT NULL DEFAULT 'pending',
    email_verified_at  timestamptz,
    code_salt          bytea,
    code_hash          bytea,
    code_attempts      integer     NOT NULL DEFAULT 0,
    code_attempts_total integer    NOT NULL DEFAULT 0,
    code_expires_at    timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    expires_at         timestamptz NOT NULL,
    decided_at         timestamptz,
    decided_by         uuid        REFERENCES users (id) ON DELETE SET NULL,
    user_id            uuid        REFERENCES users (id) ON DELETE SET NULL,
    reject_reason      text        NOT NULL DEFAULT '',
    blocked            boolean     NOT NULL DEFAULT false,
    CONSTRAINT signup_requests_status_check
        CHECK (status IN ('pending', 'approved', 'rejected', 'expired')),
    -- Stored normalised (login.NormalizeEmail), so lookups need no lower().
    CONSTRAINT signup_requests_email_lower CHECK (email = lower(email)),
    CONSTRAINT signup_requests_email_len CHECK (char_length(email) BETWEEN 3 AND 254),
    CONSTRAINT signup_requests_username_format CHECK (username ~ '^[a-z][a-z0-9_-]{0,38}$'),
    CONSTRAINT signup_requests_name_len CHECK (char_length(name) BETWEEN 1 AND 200),
    CONSTRAINT signup_requests_note_len CHECK (char_length(note) <= 1000),
    CONSTRAINT signup_requests_reason_len CHECK (char_length(reject_reason) <= 1000),
    CONSTRAINT signup_requests_expiry_check CHECK (expires_at > created_at),
    -- The code is all three columns or none of them.
    CONSTRAINT signup_requests_code_whole CHECK (
        (code_salt IS NULL) = (code_hash IS NULL) AND (code_hash IS NULL) = (code_expires_at IS NULL)),
    CONSTRAINT signup_requests_code_salt_len CHECK (octet_length(code_salt) = 16),
    CONSTRAINT signup_requests_code_hash_len CHECK (octet_length(code_hash) = 32),
    CONSTRAINT signup_requests_attempts_check CHECK (code_attempts >= 0 AND code_attempts_total >= code_attempts),
    -- Only a pending row can hold a live code.
    CONSTRAINT signup_requests_code_pending CHECK (code_hash IS NULL OR status = 'pending'),
    CONSTRAINT signup_requests_decided_check
        CHECK ((status IN ('approved', 'rejected')) = (decided_at IS NOT NULL)),
    CONSTRAINT signup_requests_user_check CHECK (user_id IS NULL OR status = 'approved'),
    CONSTRAINT signup_requests_blocked_check CHECK (NOT blocked OR status = 'rejected')
);

-- One live (pending) request per email per org: the race arbiter of two
-- requests for one address, and the lookup of POST /signups/confirm.
CREATE UNIQUE INDEX IF NOT EXISTS signup_requests_one_pending
    ON signup_requests (org_id, email) WHERE status = 'pending';
-- The lazy expiry reads only pending rows; the two pending caps count
-- them per org.
CREATE INDEX IF NOT EXISTS signup_requests_pending_expiry
    ON signup_requests (expires_at) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS signup_requests_pending_org
    ON signup_requests (org_id, (email_verified_at IS NULL AND code_hash IS NOT NULL)) WHERE status = 'pending';
-- "Is this email blocked in this org?"
CREATE INDEX IF NOT EXISTS signup_requests_blocked
    ON signup_requests (org_id, email) WHERE blocked;
-- The admin list: one org, one status, newest first.
CREATE INDEX IF NOT EXISTS signup_requests_org_status
    ON signup_requests (org_id, status, created_at DESC, id DESC);
-- ON DELETE SET NULL needs these to find the rows of a deleted user.
CREATE INDEX IF NOT EXISTS signup_requests_decided_by
    ON signup_requests (decided_by) WHERE decided_by IS NOT NULL;
CREATE INDEX IF NOT EXISTS signup_requests_user_id
    ON signup_requests (user_id) WHERE user_id IS NOT NULL;

-- org_invites: an org admin invites an email address. Only sha256(token) is
-- stored; the token is emailed to the address when mail works, and shown
-- once to the admin only when it is not. An invite works once (accepted_at), until expires_at (7 days by
-- default), and not after revoked_at. An open invite that expired is
-- closed (revoked_at set, revoked_by NULL) when a new invite for the same
-- email is made, so the unique index below lets the new one in.
CREATE TABLE org_invites (
    id           uuid        PRIMARY KEY DEFAULT uuidv7(),
    org_id       uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    email        text        NOT NULL,
    role         text        NOT NULL DEFAULT 'member',
    token_hash   bytea       NOT NULL,
    created_by   uuid        REFERENCES users (id) ON DELETE SET NULL,
    -- The link went to the address by email, so its token was NOT shown to
    -- the admin. false: the admin copied the link (no working mail).
    emailed      boolean     NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    accepted_at  timestamptz,
    accepted_by  uuid        REFERENCES users (id) ON DELETE SET NULL,
    revoked_at   timestamptz,
    revoked_by   uuid        REFERENCES users (id) ON DELETE SET NULL,
    CONSTRAINT org_invites_hash_key UNIQUE (token_hash),
    CONSTRAINT org_invites_hash_len CHECK (octet_length(token_hash) = 32),
    CONSTRAINT org_invites_role_check CHECK (role IN ('admin', 'member')),
    CONSTRAINT org_invites_email_lower CHECK (email = lower(email)),
    CONSTRAINT org_invites_email_len CHECK (char_length(email) BETWEEN 3 AND 254),
    CONSTRAINT org_invites_expiry_check CHECK (expires_at > created_at),
    CONSTRAINT org_invites_one_end CHECK (accepted_at IS NULL OR revoked_at IS NULL),
    CONSTRAINT org_invites_accepted_by_check CHECK (accepted_by IS NULL OR accepted_at IS NOT NULL)
);

-- One open invite per email per org.
CREATE UNIQUE INDEX IF NOT EXISTS org_invites_one_open
    ON org_invites (org_id, email) WHERE accepted_at IS NULL AND revoked_at IS NULL;
-- The admin list, newest first.
CREATE INDEX IF NOT EXISTS org_invites_org_created
    ON org_invites (org_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS org_invites_created_by
    ON org_invites (created_by) WHERE created_by IS NOT NULL;
CREATE INDEX IF NOT EXISTS org_invites_accepted_by
    ON org_invites (accepted_by) WHERE accepted_by IS NOT NULL;
CREATE INDEX IF NOT EXISTS org_invites_revoked_by
    ON org_invites (revoked_by) WHERE revoked_by IS NOT NULL;

-- users.email_unproven: nobody has proved the email of the account yet. It
-- was made from an invite link that an admin copied (the link was not
-- emailed), or from a sign-up request approved without a confirmed email
-- (mail was off). The first email-code login of the user revokes every token and
-- session the account had before, then clears the flag (decisions-T12 #21).
ALTER TABLE users ADD COLUMN email_unproven boolean NOT NULL DEFAULT false;
