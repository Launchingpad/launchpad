-- LAUN-199 (T34): a member's handle on a git connection. Every query filters
-- by org_id. The handle is a claim, shown for attribution only.

-- name: ListMyGitAccounts :many
-- scope: own
-- The org's enabled connections (and any disabled one the member still has a
-- link on), each with the member's handle or NULL.
SELECT g.id AS integration_id, g.provider, g.name, g.base_url, g.enabled,
       i.handle AS handle, i.updated_at AS linked_at
FROM git_integrations g
LEFT JOIN git_identities i
       ON i.org_id = g.org_id AND i.integration_id = g.id AND i.user_id = sqlc.arg(user_id)
WHERE g.org_id = sqlc.arg(org_id) AND (g.enabled OR i.id IS NOT NULL)
ORDER BY g.created_at, g.id;

-- name: GetGitIntegrationProvider :one
-- scope: org
SELECT provider, enabled FROM git_integrations WHERE org_id = $1 AND id = $2;

-- name: UpsertGitIdentity :one
-- scope: own
INSERT INTO git_identities (org_id, user_id, integration_id, handle, handle_key)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (org_id, user_id, integration_id)
DO UPDATE SET handle = EXCLUDED.handle, handle_key = EXCLUDED.handle_key,
              updated_at = CASE WHEN git_identities.handle = EXCLUDED.handle
                                THEN git_identities.updated_at ELSE now() END
RETURNING handle, updated_at;

-- name: DeleteMyGitIdentity :execrows
-- scope: own
DELETE FROM git_identities WHERE org_id = $1 AND user_id = $2 AND integration_id = $3;

-- name: ListGitIdentities :many
-- scope: admin
SELECT i.id, i.integration_id, g.provider, g.name AS integration_name, g.base_url,
       i.handle, i.updated_at, u.id AS user_id, u.username, u.name AS user_name
FROM git_identities i
JOIN git_integrations g ON g.org_id = i.org_id AND g.id = i.integration_id
JOIN users u ON u.id = i.user_id
WHERE i.org_id = $1
ORDER BY u.username, g.created_at, i.id;

-- name: DeleteGitIdentity :one
-- scope: admin
-- An admin unlink. Returns the owner and the connection for the event (ids).
DELETE FROM git_identities WHERE org_id = $1 AND id = $2
RETURNING user_id, integration_id;
