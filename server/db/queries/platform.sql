-- Platform settings (docs/internal/decisions-T9-mail-settings.md). The
-- table has exactly one row (id = true), made by migration 0006.

-- name: GetPlatformSettings :one
-- scope: none
SELECT * FROM platform_settings WHERE id;

-- name: GetPlatformSettingsForUpdate :one
-- scope: none
-- PUT reads the stored secrets under this lock, so a secret it keeps is the
-- one in the row it writes, even with two writers at once.
SELECT * FROM platform_settings WHERE id FOR UPDATE;

-- name: UpdatePlatformSettings :one
-- scope: none
UPDATE platform_settings SET
    mail_provider      = sqlc.arg(mail_provider),
    postmark_token_enc = sqlc.narg(postmark_token_enc),
    postmark_from      = sqlc.arg(postmark_from),
    postmark_stream    = sqlc.arg(postmark_stream),
    smtp_host          = sqlc.arg(smtp_host),
    smtp_port          = sqlc.arg(smtp_port),
    smtp_security      = sqlc.arg(smtp_security),
    smtp_username      = sqlc.arg(smtp_username),
    smtp_password_enc  = sqlc.narg(smtp_password_enc),
    smtp_from_address  = sqlc.arg(smtp_from_address),
    smtp_from_name     = sqlc.arg(smtp_from_name),
    key_check          = sqlc.narg(key_check),
    signup_mode        = sqlc.arg(signup_mode),
    -- Set once, by a change of the mode or a confirm; no other save touches it (LAUN-56).
    signup_mode_chosen_at = CASE WHEN sqlc.arg(signup_chosen)::boolean
        THEN COALESCE(signup_mode_chosen_at, now()) ELSE signup_mode_chosen_at END,
    -- Strictly later than the row it replaces, and taken at the UPDATE (not
    -- at transaction start, as now() is): under the row lock, so the order of
    -- updated_at is the commit order, and every server's "apply only a newer
    -- row" check follows the last write (decisions-T9 #21).
    updated_at         = GREATEST(clock_timestamp(), updated_at + interval '1 microsecond'),
    updated_by         = sqlc.narg(updated_by)
WHERE id
RETURNING *;
