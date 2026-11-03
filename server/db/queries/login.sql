-- Email login codes (T6a). Every time in these queries comes from
-- the application clock, so one clock decides expiry and the hourly limit.

-- name: GetLoginUserByEmail :one
-- scope: system
-- Who may log in: an active, human user who is a member of at least one org,
-- or a platform admin (who may belong to no org yet).
SELECT u.id, u.username, u.name, u.email, u.is_platform_admin, u.email_unproven
FROM users u
WHERE lower(u.email) = lower(sqlc.arg(email)::text)
  AND u.deactivated_at IS NULL
  AND NOT u.is_agent
  AND (u.is_platform_admin OR EXISTS (SELECT 1 FROM org_members m WHERE m.user_id = u.id));

-- name: LockUserForLogin :one
-- scope: none
-- Serialises code requests and checks for one person, so the hourly count
-- and "one live code" hold under concurrent requests.
SELECT id FROM users WHERE id = $1 FOR UPDATE;

-- name: CountLoginCodesSince :one
-- scope: none
SELECT count(*) FROM login_codes WHERE user_id = $1 AND created_at > $2;

-- name: DeleteLoginCodesBefore :exec
-- scope: none
DELETE FROM login_codes WHERE user_id = $1 AND created_at < $2;

-- name: CloseLiveLoginCodes :exec
-- scope: none
UPDATE login_codes SET consumed_at = $2 WHERE user_id = $1 AND consumed_at IS NULL;

-- name: InsertLoginCode :one
-- scope: none
INSERT INTO login_codes (user_id, code_salt, code_hash, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING id;

-- name: GetLiveLoginCodeForUpdate :one
-- scope: none
-- The person's newest open code, row-locked so concurrent guesses count
-- exactly (golang-database: SELECT ... FOR UPDATE before a modify).
SELECT id, code_salt, code_hash, attempts, expires_at
FROM login_codes
WHERE user_id = $1 AND consumed_at IS NULL
ORDER BY created_at DESC, id DESC
LIMIT 1
FOR UPDATE;

-- name: RecordFailedLoginAttempt :one
-- scope: none
UPDATE login_codes SET attempts = attempts + 1 WHERE id = $1 RETURNING attempts;

-- name: ConsumeLoginCode :execrows
-- scope: none
UPDATE login_codes SET consumed_at = $2 WHERE id = $1 AND consumed_at IS NULL;

-- name: RevokeTokensOfUnprovenUser :execrows
-- scope: system
-- The first email-code login of an account made from a copied invite link
-- (users.email_unproven): every token and session the account
-- had, and every token it minted for another user (an agent), stop
-- working. Whoever held the copied link before the owner keeps nothing.
UPDATE api_tokens SET revoked_at = now()
WHERE (user_id = $1 OR created_by = $1) AND revoked_at IS NULL;

-- name: ClearEmailUnproven :exec
-- scope: none
UPDATE users SET email_unproven = false, updated_at = now() WHERE id = $1 AND email_unproven;
