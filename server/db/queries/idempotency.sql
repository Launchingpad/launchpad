-- name: ClaimIdempotencyKey :one
-- scope: none
-- Returns a row only when this request is the first to use the key.
INSERT INTO idempotency_keys (user_id, key, method, path, request_hash, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (user_id, key) DO NOTHING
RETURNING id;

-- name: GetIdempotencyKey :one
-- scope: none
SELECT * FROM idempotency_keys WHERE user_id = $1 AND key = $2;

-- name: CompleteIdempotencyKey :exec
-- scope: none
UPDATE idempotency_keys SET status_code = $2, response_body = $3 WHERE id = $1;

-- name: ReleaseUncommittedIdempotencyKey :exec
-- scope: none
-- Frees a claim for a retry, but only if the request's write never
-- committed; a committed claim is kept so a retry cannot run it twice.
DELETE FROM idempotency_keys WHERE id = $1 AND committed_at IS NULL AND status_code IS NULL;

-- name: TakeOverAbandonedIdempotencyKey :execrows
-- scope: none
-- Deletes a claim that is still in flight, never committed, and older than
-- the stale window: its request died before writing anything. Uses the
-- DATABASE clock, the same one that stamped created_at.
DELETE FROM idempotency_keys
WHERE id = $1 AND status_code IS NULL AND committed_at IS NULL
  AND created_at < now() - make_interval(secs => sqlc.arg(stale_seconds)::float8);

-- name: DeleteExpiredIdempotencyKey :exec
-- scope: none
DELETE FROM idempotency_keys WHERE user_id = $1 AND key = $2 AND expires_at <= now();

-- name: PurgeExpiredIdempotencyKeys :execrows
-- scope: none
DELETE FROM idempotency_keys WHERE expires_at <= now();
