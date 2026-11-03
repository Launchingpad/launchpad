-- First-run setup (docs/internal/decisions-T8-first-run.md). Expiry and
-- consumption use the database clock (now()) in every query, so one clock
-- decides whether a link is live.

-- name: SetupRequired :one
-- scope: none
-- Setup is open while the install has no user at all.
SELECT NOT EXISTS (SELECT 1 FROM users) AS required;

-- name: LockSetup :exec
-- scope: none
-- Serialises minting and completion across processes and instances. Held to
-- the end of the transaction.
SELECT pg_advisory_xact_lock(hashtext('devops-setup'));

-- name: CloseSetupTokens :exec
-- scope: none
-- Minting a new link closes every older one, so only the newest link works.
UPDATE setup_tokens SET consumed_at = now() WHERE consumed_at IS NULL;

-- name: InsertSetupToken :one
-- scope: none
INSERT INTO setup_tokens (token_hash, expires_at)
VALUES (sqlc.arg(token_hash), now() + make_interval(secs => sqlc.arg(ttl_seconds)::double precision))
RETURNING id, expires_at;

-- name: ConsumeSetupToken :one
-- scope: none
-- The race arbiter: of two requests with the same link, one gets the row and
-- the other none (the second waits on the row lock, then sees consumed_at).
UPDATE setup_tokens SET consumed_at = now()
WHERE token_hash = $1 AND consumed_at IS NULL AND expires_at > now()
RETURNING id;

-- name: InsertSetupUser :one
-- scope: none
-- The first user of an install: a human platform admin.
INSERT INTO users (username, name, email, is_agent, is_platform_admin)
VALUES ($1, $2, $3, false, true)
RETURNING id, username, name, email;

-- name: SetPlatformAdmin :execrows
-- scope: none
-- `server grant-platform-admin`: an active human only.
UPDATE users SET is_platform_admin = true, updated_at = now()
WHERE id = $1 AND NOT is_agent AND deactivated_at IS NULL AND NOT is_platform_admin;
