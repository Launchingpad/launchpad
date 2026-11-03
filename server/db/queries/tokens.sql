-- name: InsertAPIToken :one
-- scope: system
INSERT INTO api_tokens (user_id, org_id, kind, name, token_hash, prefix, scopes, created_by, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetTokenByHash :one
-- scope: system
SELECT t.id, t.user_id, t.org_id, t.kind, t.scopes, t.expires_at, t.revoked_at, t.last_used_at,
       u.username, u.name AS user_name, u.email AS user_email, u.is_agent, u.deactivated_at,
       u.is_platform_admin
FROM api_tokens t
JOIN users u ON u.id = t.user_id
WHERE t.token_hash = $1;

-- name: TouchTokenLastUsed :exec
-- scope: system
-- Throttled to once a minute so a busy client does not write on every call.
UPDATE api_tokens SET last_used_at = now()
WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- name: ListOrgTokens :many
-- scope: admin
SELECT t.id, t.kind, t.name, t.prefix, t.scopes, t.created_at, t.expires_at, t.last_used_at, t.revoked_at,
       u.id AS user_id, u.username, u.name AS user_name
FROM api_tokens t
JOIN users u ON u.id = t.user_id
WHERE t.org_id = $1
ORDER BY t.created_at DESC, t.id;

-- name: RevokeOrgToken :one
-- scope: admin
-- o is the row as it was before this statement, so newly_revoked is true only
-- for the call that revoked it (a repeat revoke is a no-op).
UPDATE api_tokens t SET revoked_at = COALESCE(t.revoked_at, now())
FROM api_tokens o JOIN users u ON u.id = o.user_id
WHERE t.org_id = $1 AND t.id = $2 AND o.id = t.id
RETURNING t.id, t.name, t.scopes, t.expires_at, t.revoked_at, u.username AS owner_username,
          (o.revoked_at IS NULL)::boolean AS newly_revoked;

-- name: RevokeSessionToken :execrows
-- scope: own
-- Logout. Only the owner's own session; a PAT is never revoked this way.
UPDATE api_tokens SET revoked_at = COALESCE(revoked_at, now())
WHERE id = $1 AND user_id = $2 AND kind IN ('session', 'native');

-- name: GetTokenAuthState :one
-- scope: system
-- Re-checked by long-lived requests (the SSE stream).
SELECT t.revoked_at, t.expires_at, u.deactivated_at
FROM api_tokens t
JOIN users u ON u.id = t.user_id
WHERE t.id = $1;

-- name: ListMySessions :many
-- scope: own
-- LAUN-130. The caller's OWN live login sessions (web and native). user_id is the
-- ownership filter; kind keeps PATs out. No hash, no prefix.
SELECT id, kind, name, created_at, last_used_at, expires_at
FROM api_tokens
WHERE user_id = $1 AND kind IN ('session', 'native')
  AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > @now::timestamptz)
ORDER BY created_at DESC, id
LIMIT 200;

-- name: RevokeMySession :one
-- scope: own
-- LAUN-130. user_id and kind are the ownership filter: another person's id, a PAT's id
-- and an unknown id all match no row (404). o is the row before this statement, so
-- newly_revoked is true only for the call that revoked it.
UPDATE api_tokens t SET revoked_at = COALESCE(t.revoked_at, now())
FROM api_tokens o
WHERE t.id = $1 AND t.user_id = $2 AND t.kind IN ('session', 'native') AND o.id = t.id
RETURNING t.id, t.kind, t.name, (o.revoked_at IS NULL)::boolean AS newly_revoked;

-- name: RevokeMyOtherSessions :execrows
-- scope: own
-- LAUN-130. "Log out everywhere": every live session of the caller except id $2.
UPDATE api_tokens SET revoked_at = now()
WHERE user_id = $1 AND kind IN ('session', 'native') AND id <> $2 AND revoked_at IS NULL;
