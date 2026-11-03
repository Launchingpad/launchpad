-- LAUN-229 (T40): a member's own webhooks and their post outbox. The member
-- routes read and write the caller's own rows (org_id AND user_id); the
-- consumer and the sender have no caller.

-- name: ListMyPersonalWebhooks :many
-- scope: own
SELECT * FROM personal_webhooks WHERE org_id = $1 AND user_id = $2 ORDER BY created_at, id;

-- name: GetMyPersonalWebhook :one
-- scope: own
SELECT * FROM personal_webhooks WHERE org_id = $1 AND user_id = $2 AND id = $3;

-- name: GetMyPersonalWebhookForUpdate :one
-- scope: own
SELECT * FROM personal_webhooks WHERE org_id = $1 AND user_id = $2 AND id = $3 FOR UPDATE;

-- name: CountMyPersonalWebhooks :one
-- scope: own
SELECT count(*) FROM personal_webhooks WHERE org_id = $1 AND user_id = $2;

-- name: LockMemberForPersonalWebhook :one
-- scope: own
-- Serialises the creates of one member, so the cap holds under concurrent
-- POSTs. No row: the caller is not a member (any more).
SELECT user_id FROM org_members WHERE org_id = $1 AND user_id = $2 FOR NO KEY UPDATE;

-- name: InsertPersonalWebhook :one
-- scope: own
INSERT INTO personal_webhooks (id, org_id, user_id, name, kind, host, webhook_enc, seal_scope, events, enabled)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, sqlc.arg('events')::text[], $9)
RETURNING *;

-- name: UpdateMyPersonalWebhook :one
-- scope: own
-- name, events, enabled keep their value when the argument is NULL. A new
-- webhook (blob and host together) clears the status.
UPDATE personal_webhooks SET
  name        = COALESCE(sqlc.narg('name'), name),
  events      = COALESCE(sqlc.narg('events')::text[], events),
  enabled     = COALESCE(sqlc.narg('enabled'), enabled),
  webhook_enc = COALESCE(sqlc.narg('webhook_enc'), webhook_enc),
  host        = COALESCE(sqlc.narg('host'), host),
  last_ok_at  = CASE WHEN sqlc.narg('webhook_enc')::bytea IS NOT NULL THEN NULL ELSE last_ok_at END,
  last_error  = CASE WHEN sqlc.narg('webhook_enc')::bytea IS NOT NULL THEN NULL ELSE last_error END,
  updated_at  = now()
WHERE org_id = $1 AND user_id = $2 AND id = $3
RETURNING *;

-- name: DeleteMyPersonalWebhook :execrows
-- scope: own
DELETE FROM personal_webhooks WHERE org_id = $1 AND user_id = $2 AND id = $3;

-- name: DeleteOpenPersonalWebhookOutbox :execrows
-- scope: own
-- A webhook switched off drops its waiting posts (turning it on later must not
-- send them). Terminal rows stay for the prune. The caller owns the webhook.
DELETE FROM personal_webhook_outbox WHERE org_id = $1 AND webhook_id = $2 AND state IN ('pending', 'sending');

-- name: SetPersonalWebhookResult :execrows
-- scope: system
-- The outcome of the last post: ok clears the error. last_error is a fixed word.
UPDATE personal_webhooks
SET last_ok_at = CASE WHEN sqlc.arg('ok')::bool THEN sqlc.arg('at')::timestamptz ELSE last_ok_at END,
    last_error = sqlc.narg('last_error')
WHERE org_id = $1 AND id = $2;

-- name: GetPersonalWebhook :one
-- scope: system
SELECT * FROM personal_webhooks WHERE org_id = $1 AND id = $2;

-- name: ListEnabledPersonalWebhooksForUsers :many
-- scope: system
-- The consumer reads the event choices only, never the sealed blob.
SELECT id, user_id, events FROM personal_webhooks
WHERE org_id = $1 AND enabled AND user_id = ANY(sqlc.arg('user_ids')::uuid[])
ORDER BY user_id, created_at, id;

-- name: InsertPersonalWebhookOutbox :one
-- scope: system
-- ON CONFLICT: a replayed event makes no second post.
INSERT INTO personal_webhook_outbox (org_id, webhook_id, kind, issue_id, dedupe_key, event_seq)
VALUES ($1, $2, $3, $4, $5, sqlc.narg('event_seq'))
ON CONFLICT (webhook_id, dedupe_key) DO NOTHING
RETURNING id;

-- name: TryLockPersonalWebhookSender :one
-- scope: none
SELECT pg_try_advisory_lock(hashtextextended('devops:personal-webhook-send', 0)) AS locked;

-- name: UnlockPersonalWebhookSender :one
-- scope: none
SELECT pg_advisory_unlock(hashtextextended('devops:personal-webhook-send', 0)) AS unlocked;

-- name: RecoverStuckPersonalWebhookOutbox :execrows
-- scope: system
UPDATE personal_webhook_outbox SET state = 'pending'
WHERE state = 'sending' AND next_attempt_at <= sqlc.arg('cutoff');

-- name: ExpirePersonalWebhookOutbox :execrows
-- scope: system
UPDATE personal_webhook_outbox SET state = 'skipped', skip_reason = 'expired', next_attempt_at = sqlc.arg('now')
WHERE state = 'pending' AND created_at <= sqlc.arg('cutoff');

-- name: ListDuePersonalWebhookOutbox :many
-- scope: system
-- Install-wide, fair as ListDueChatOutbox: the first row of every webhook of an
-- org before the second row of any, at most per_org rows of each org, taken
-- round-robin across orgs, then max_rows in all.
SELECT id, org_id, webhook_id, kind, issue_id, event_seq, attempts, created_at
FROM (
  SELECT a.id, a.org_id, a.webhook_id, a.kind, a.issue_id, a.event_seq, a.attempts, a.created_at,
         row_number() OVER (PARTITION BY a.org_id ORDER BY a.rs, a.created_at, a.id) AS rn
  FROM (
    SELECT o.id, o.org_id, o.webhook_id, o.kind, o.issue_id, o.event_seq, o.attempts, o.created_at,
           row_number() OVER (PARTITION BY o.org_id, o.webhook_id ORDER BY o.created_at, o.id) AS rs
    FROM personal_webhook_outbox o
    WHERE o.state = 'pending' AND o.next_attempt_at <= sqlc.arg('now')
  ) a
) t
WHERE rn <= sqlc.arg('per_org')::int
ORDER BY rn, created_at, id
LIMIT sqlc.arg('max_rows');

-- name: ClaimPersonalWebhookOutbox :execrows
-- scope: system
UPDATE personal_webhook_outbox SET state = 'sending', next_attempt_at = sqlc.arg('now')
WHERE org_id = $1 AND id = $2 AND state = 'pending';

-- name: SkipPersonalWebhookOutbox :execrows
-- scope: system
UPDATE personal_webhook_outbox SET state = 'skipped', skip_reason = sqlc.arg('reason'), next_attempt_at = sqlc.arg('now')
WHERE org_id = $1 AND id = $2 AND state = 'sending';

-- name: MarkPersonalWebhookOutboxSent :execrows
-- scope: system
UPDATE personal_webhook_outbox SET state = 'sent', sent_at = sqlc.arg('now'), last_error = NULL
WHERE org_id = $1 AND id = $2 AND state = 'sending';

-- name: ReleasePersonalWebhookOutbox :execrows
-- scope: system
UPDATE personal_webhook_outbox SET state = 'pending'
WHERE org_id = $1 AND id = $2 AND state = 'sending';

-- name: FailPersonalWebhookOutbox :execrows
-- scope: system
UPDATE personal_webhook_outbox
SET attempts = sqlc.arg('attempts'), state = sqlc.arg('state'), next_attempt_at = sqlc.arg('next_attempt_at'),
    last_error = sqlc.arg('last_error')
WHERE org_id = $1 AND id = $2 AND state = 'sending';

-- name: CountRecentPersonalWebhookPosts :one
-- scope: system
-- The rate cap is per webhook.
SELECT count(*) FROM personal_webhook_outbox
WHERE org_id = $1 AND webhook_id = $2 AND state = 'sent' AND sent_at > sqlc.arg('since');

-- name: PrunePersonalWebhookOutbox :execrows
-- scope: system
-- Install-wide. Terminal rows older than the cutoff, at most batch_size.
DELETE FROM personal_webhook_outbox
WHERE id IN (
  SELECT o.id FROM personal_webhook_outbox o
  WHERE o.state IN ('sent', 'skipped', 'dead') AND o.created_at < sqlc.arg('cutoff')
  ORDER BY o.created_at
  LIMIT sqlc.arg('batch_size')
);
