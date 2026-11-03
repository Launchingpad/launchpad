-- Provider-neutral git integration reads (T17, LAUN-5). Every query filters
-- by org_id.

-- The signed-body webhook check (GitHub, later Bitbucket): the sealed secrets
-- of every ENABLED integration of the org and provider. The caller opens them
-- (GitKeyring.OpenFor) and compares the HMACs of all of them in constant time.
-- updated_at is the key of the in-memory secret cache. This is, with
-- ListGitlabSecretHashes, one of the two queries that read a webhook secret.
-- name: ListGitSealedSecrets :many
-- scope: system
SELECT id, updated_at, webhook_secret_enc, webhook_secret_next_enc
FROM git_integrations
WHERE org_id = $1 AND provider = $2 AND enabled
ORDER BY id;

-- Integration config (T17, LAUN-7). The sealed columns and the hashes are
-- never selected here: token_set says only that a token is stored.
-- id NULL lists the org's integrations, oldest first; an id reads one.
-- name: ListGitIntegrations :many
-- scope: admin
SELECT g.id, g.provider, g.flavor, g.mode, g.name, g.base_url, g.api_base_url, g.enabled,
       g.attention_code, (g.token_enc IS NOT NULL)::boolean AS token_set,
       (g.webhook_secret_enc IS NOT NULL)::boolean AS secret_sealed,
       (g.webhook_secret_next_hash IS NOT NULL)::boolean AS secret_pending,
       g.created_at, g.updated_at,
       (SELECT count(*) FROM git_repo_map m WHERE m.org_id = g.org_id AND m.integration_id = g.id)::int AS repo_count,
       ld.received_at AS last_delivery_at, ld.result AS last_delivery_result,
       ld.http_status AS last_delivery_status, ld.error_code AS last_delivery_error
FROM git_integrations g
LEFT JOIN git_webhook_deliveries ld ON ld.org_id = g.org_id AND ld.id = (
    SELECT d.id FROM git_webhook_deliveries d WHERE d.org_id = g.org_id AND d.integration_id = g.id
    ORDER BY d.received_at DESC, d.id DESC LIMIT 1)
WHERE g.org_id = $1 AND (sqlc.narg(id)::uuid IS NULL OR g.id = sqlc.narg(id))
ORDER BY g.created_at, g.id;

-- Locks the row for a PATCH (the transaction then reads it with ListGitIntegrations).
-- name: LockGitIntegration :one
-- scope: admin
SELECT id FROM git_integrations WHERE org_id = $1 AND id = $2 FOR UPDATE;

-- LAUN-104: removes the row; the composite foreign keys cascade this
-- integration's own repo map, webhook events, deliveries and mr_links.
-- name: DeleteGitIntegration :execrows
-- scope: admin
DELETE FROM git_integrations WHERE org_id = $1 AND id = $2;

-- The one query that reads the sealed token, for a check.
-- name: GetGitIntegrationToken :one
-- scope: admin
SELECT provider, mode, base_url, api_base_url, token_enc, updated_at
FROM git_integrations WHERE org_id = $1 AND id = $2;

-- The id is made by the app: the AAD of a sealed column holds it.
-- name: InsertGitIntegration :exec
-- scope: admin
INSERT INTO git_integrations (id, org_id, provider, flavor, mode, name, base_url, api_base_url,
                              enabled, webhook_secret_hash, webhook_secret_enc, token_enc)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12);

-- set_token false keeps token_enc; true replaces it (NULL clears it). A change
-- that makes the old attention code moot (a checked token, a cleared token)
-- clears the codes of the token and the hooks, never secret_unreadable.
-- name: UpdateGitIntegration :execrows
-- scope: admin
UPDATE git_integrations SET
    name = $3, flavor = $4, base_url = $5, api_base_url = $6, mode = $7, enabled = $8,
    token_enc = CASE WHEN sqlc.arg(set_token)::boolean THEN sqlc.narg(token_enc)::bytea ELSE token_enc END,
    attention_code = CASE WHEN sqlc.arg(clear_attention)::boolean
                          AND attention_code IN ('token_rejected', 'attention_unreachable', 'hook_failed')
                          THEN NULL ELSE attention_code END,
    updated_at = now()
WHERE org_id = $1 AND id = $2;

-- Sets or clears the attention code. It does not touch updated_at, which keys
-- the in-memory secret cache.
-- name: SetGitAttention :exec
-- scope: admin
UPDATE git_integrations SET attention_code = sqlc.narg(attention_code)
WHERE org_id = $1 AND id = $2;

-- Does another ENABLED integration of the org and provider use this secret
-- (as its current or its pending hash)? The caller, checkGitSecretFree in
-- internal/tracker/git_integrations.go, takes the advisory lock first.
-- name: GitSecretInUse :one
-- scope: system
SELECT EXISTS (
    SELECT 1 FROM git_integrations o
    WHERE o.org_id = $1 AND o.provider = $2 AND o.enabled AND o.id <> $3
      AND (o.webhook_secret_hash = $4 OR o.webhook_secret_next_hash = $4)
) AS in_use;

-- The hash an integration has, for an enable that keeps its secret (current and
-- pending).
-- name: GetGitSecretHashes :one
-- scope: system
SELECT webhook_secret_hash, webhook_secret_next_hash
FROM git_integrations WHERE org_id = $1 AND id = $2;

-- name: CountEnabledGitIntegrations :one
-- scope: admin
SELECT count(*)::int FROM git_integrations
WHERE org_id = $1 AND provider = $2 AND enabled AND id <> $3;

-- Secret rotation (T17 #11, LAUN-86). The rows below read or write the sealed
-- secrets; nothing returns them to a client.

-- The rotation state of one integration, under the row lock (the caller holds
-- the advisory lock of the (org, provider) too).
-- name: LockGitRotation :one
-- scope: admin
SELECT id, provider, mode, enabled, base_url, api_base_url, token_enc,
       webhook_secret_enc, webhook_secret_hash, webhook_secret_next_enc, webhook_secret_next_hash, updated_at
FROM git_integrations WHERE org_id = $1 AND id = $2 FOR UPDATE;

-- The same, without the lock (the pending secret that a hook push uses).
-- name: GetGitRotation :one
-- scope: admin
SELECT id, provider, mode, enabled, base_url, api_base_url, token_enc,
       webhook_secret_enc, webhook_secret_hash, webhook_secret_next_enc, webhook_secret_next_hash, updated_at
FROM git_integrations WHERE org_id = $1 AND id = $2;

-- Stores the pending secret (and its hash). A NULL blob is a pending secret
-- that was made with no key (GitLab, hash only).
-- name: SetGitPendingSecret :exec
-- scope: admin
UPDATE git_integrations SET webhook_secret_next_enc = sqlc.narg(next_enc)::bytea,
    webhook_secret_next_hash = $3, updated_at = now()
WHERE org_id = $1 AND id = $2;

-- The commit: the pending secret becomes the current one in ONE statement.
-- sqlc.narg(secret_enc) is the pending secret sealed under the current column
-- (NULL when there is no sealed copy). expected_hash makes it a compare and
-- swap: nothing changes if the pending secret is not the one the caller pushed.
-- name: CommitGitSecret :execrows
-- scope: admin
UPDATE git_integrations SET webhook_secret_enc = sqlc.narg(secret_enc)::bytea,
    webhook_secret_hash = webhook_secret_next_hash,
    webhook_secret_next_enc = NULL, webhook_secret_next_hash = NULL,
    attention_code = NULL, updated_at = now()
WHERE org_id = $1 AND id = $2 AND webhook_secret_next_hash = sqlc.arg(expected_hash)::bytea;

-- name: DiscardGitPendingSecret :execrows
-- scope: admin
UPDATE git_integrations SET webhook_secret_next_enc = NULL, webhook_secret_next_hash = NULL, updated_at = now()
WHERE org_id = $1 AND id = $2 AND webhook_secret_next_hash IS NOT NULL;

-- The repo map (T17 #8, LAUN-86).
-- name: ListGitRepos :many
-- scope: admin
SELECT m.id, m.integration_id, m.repo_key, m.repo_path, m.team_id, m.hook_id, m.hook_state, m.hook_error, m.created_at,
       t.key AS team_key, t.name AS team_name
FROM git_repo_map m
LEFT JOIN teams t ON t.org_id = m.org_id AND t.id = m.team_id
WHERE m.org_id = $1 AND m.integration_id = $2 AND (sqlc.narg(id)::uuid IS NULL OR m.id = sqlc.narg(id))
ORDER BY m.created_at, m.id;

-- The share lock keeps a URL change (which holds FOR UPDATE and counts the
-- mapped repos) from racing a map insert. updated_at is returned so the caller
-- can see that a rotation moved the secret since it read it.
-- name: LockGitIntegrationShared :one
-- scope: admin
SELECT id, updated_at, mode FROM git_integrations
WHERE org_id = $1 AND id = $2 FOR SHARE;

-- name: InsertGitRepo :one
-- scope: admin
INSERT INTO git_repo_map (org_id, integration_id, repo_key, repo_path, team_id, hook_id, hook_state)
VALUES ($1, $2, $3, $4, $5, sqlc.narg(hook_id), $6)
RETURNING id;

-- name: UpdateGitRepoTeam :execrows
-- scope: admin
UPDATE git_repo_map SET team_id = sqlc.narg(team_id)
WHERE org_id = $1 AND integration_id = $2 AND id = $3;

-- name: UpdateGitRepoHook :exec
-- scope: admin
UPDATE git_repo_map SET hook_id = sqlc.narg(hook_id), hook_state = $4, hook_error = sqlc.narg(hook_error)
WHERE org_id = $1 AND integration_id = $2 AND id = $3;

-- name: DeleteGitRepo :execrows
-- scope: admin
DELETE FROM git_repo_map WHERE org_id = $1 AND integration_id = $2 AND id = $3;

-- Token-mode rotation (T17 #11): a NEW pending secret means no hook holds it
-- yet, so every mapped repo is "not updated" until a push succeeds. A retry
-- reuses the pending secret and pushes only to the repos that are not active.
-- name: MarkGitHooksNotUpdated :exec
-- scope: admin
UPDATE git_repo_map SET hook_state = 'failed', hook_error = 'not_updated'
WHERE org_id = $1 AND integration_id = $2;

-- A discard in token mode: a hook that was moved to the dropped secret
-- (active) must be refreshed; one that never moved still holds the current
-- secret and is fine again. A not_updated row with a hook id may or may not
-- have taken a host write before a crash (LAUN-92): it is failed, and the
-- next rotate or refresh pushes to it again. Only a row with no hook is none.
-- name: SettleGitHooksAfterDiscard :exec
-- scope: admin
UPDATE git_repo_map SET
    hook_state = CASE WHEN hook_state = 'active' THEN 'failed'
                      WHEN hook_state = 'failed' AND hook_error = 'not_updated' THEN (CASE WHEN hook_id IS NOT NULL THEN 'failed' ELSE 'none' END)
                      ELSE hook_state END,
    hook_error = CASE WHEN hook_state = 'active' THEN 'secret_discarded'
                      WHEN hook_state = 'failed' AND hook_error = 'not_updated' THEN (CASE WHEN hook_id IS NOT NULL THEN 'secret_discarded' ELSE NULL END)
                      ELSE hook_error END
WHERE org_id = $1 AND integration_id = $2;

-- Repos pushed in a rotation run whose pending secret was dropped meanwhile.
-- name: MarkGitHooksDiscarded :exec
-- scope: admin
UPDATE git_repo_map SET hook_state = 'failed', hook_error = 'secret_discarded'
WHERE org_id = $1 AND integration_id = $2 AND id = ANY(sqlc.arg(ids)::uuid[]);

-- name: GitRepoKeyMapped :one
-- scope: admin
SELECT EXISTS (
    SELECT 1 FROM git_repo_map WHERE org_id = $1 AND integration_id = $2 AND repo_key = $3
) AS mapped;

-- The repo keys of an integration, for the "mapped" flag of available-repos.
-- name: ListGitRepoKeys :many
-- scope: admin
SELECT repo_key FROM git_repo_map WHERE org_id = $1 AND integration_id = $2;

-- The deliveries ring (T17 #9, LAUN-87). The ring is written by the two
-- webhook handlers and holds a summary only. One advisory lock per ring
-- (integration, or org and provider for the unbound rows) makes the 100-row
-- bound exact when two deliveries arrive together.
-- name: LockGitDeliveryRing :exec
-- scope: none
SELECT pg_advisory_xact_lock(hashtextextended('launchpad:git-ring:' || sqlc.arg(ring_key)::text, 0));

-- Inserts one row and trims the ring to the newest 100 in the same statement
-- (the data-modifying CTE does not see its own row, so 99 old rows stay).
-- integration_id NULL is the ring of the org and provider.
-- name: InsertGitDelivery :exec
-- scope: system
WITH ins AS (
    INSERT INTO git_webhook_deliveries (org_id, integration_id, provider, result, event, repo_key, http_status, error_code)
    VALUES (sqlc.arg(org_id), sqlc.narg(integration_id)::uuid, sqlc.arg(provider), sqlc.arg(result),
            sqlc.arg(event), sqlc.narg(repo_key), sqlc.arg(http_status), sqlc.narg(error_code))
    RETURNING id
)
DELETE FROM git_webhook_deliveries d
WHERE d.org_id = sqlc.arg(org_id) AND d.provider = sqlc.arg(provider)
  AND d.integration_id IS NOT DISTINCT FROM sqlc.narg(integration_id)::uuid
  AND d.id NOT IN (
      SELECT k.id FROM git_webhook_deliveries k
      WHERE k.org_id = sqlc.arg(org_id) AND k.provider = sqlc.arg(provider)
        AND k.integration_id IS NOT DISTINCT FROM sqlc.narg(integration_id)::uuid
      ORDER BY k.received_at DESC, k.id DESC LIMIT 99);

-- The ring of one integration, newest first, with the unbound rows (a bad
-- signature) of its org and provider.
-- name: ListGitDeliveries :many
-- scope: admin
SELECT d.id, d.integration_id, d.provider, d.received_at, d.result, d.event, d.repo_key, d.http_status, d.error_code
FROM git_webhook_deliveries d
WHERE d.org_id = sqlc.arg(org_id)
  AND (d.integration_id = sqlc.arg(integration_id)::uuid
       OR (d.integration_id IS NULL AND d.provider = sqlc.arg(provider)))
ORDER BY d.received_at DESC, d.id DESC
LIMIT 100;

-- name: LockTeamAccessGuard :exec
-- scope: none
-- The webhook guard of decisions-T20 #65 (open point M): one advisory lock
-- per org that every entry takes first, so a write that adds restricted
-- access and a write that adds a repository with no team cannot both pass.
SELECT pg_advisory_xact_lock(hashtextextended('launchpad-team-access:' || sqlc.arg(org_id)::uuid::text, 0));

-- name: RestrictedAccessExists :one
-- scope: admin
-- A member with team_access 'selected', whatever the role (fail closed), or
-- an open invite that would make one (not accepted, not revoked, not expired;
-- an expired invite can never be used, ConsumeOrgInvite).
SELECT (EXISTS (SELECT 1 FROM org_members m WHERE m.org_id = sqlc.arg(org_id)::uuid AND m.team_access = 'selected')
     OR EXISTS (SELECT 1 FROM org_invites i
                WHERE i.org_id = sqlc.arg(org_id)::uuid AND i.team_access = 'selected'
                  AND i.accepted_at IS NULL AND i.revoked_at IS NULL AND i.expires_at > now()))::boolean AS restricted;

-- name: ListEnabledReposWithoutTeam :many
-- scope: admin
-- The map rows of enabled integrations whose team_id is NULL: they match
-- issues of every team (#65).
SELECT m.repo_path
FROM git_repo_map m
JOIN git_integrations g ON g.org_id = m.org_id AND g.id = m.integration_id
WHERE m.org_id = $1 AND g.enabled AND m.team_id IS NULL
ORDER BY m.repo_path, m.id
LIMIT 21;
