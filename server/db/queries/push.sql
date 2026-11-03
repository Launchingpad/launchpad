-- LAUN-197 (T35): the org's push relay, the members' devices and the push
-- outbox. Every query filters by org_id, except the sender's install-wide ones.

-- name: GetPushRelay :one
-- scope: system
SELECT * FROM push_relays WHERE org_id = $1;

-- name: GetPushRelayForUpdate :one
-- scope: admin
SELECT * FROM push_relays WHERE org_id = $1 FOR UPDATE;

-- name: PushRelayConfigured :one
-- scope: system
-- The consumer and the settings view ask only whether a relay exists; never for the blobs.
SELECT EXISTS (SELECT 1 FROM push_relays WHERE org_id = $1);

-- name: UpsertPushRelay :one
-- scope: admin
INSERT INTO push_relays (org_id, url_enc, secret_enc, host, updated_by)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (org_id) DO UPDATE
SET url_enc = EXCLUDED.url_enc, secret_enc = EXCLUDED.secret_enc, host = EXCLUDED.host,
    updated_by = EXCLUDED.updated_by, last_ok_at = NULL, last_error = NULL, updated_at = now()
RETURNING *;

-- name: DeletePushRelay :execrows
-- scope: admin
DELETE FROM push_relays WHERE org_id = $1;

-- name: SetPushRelayResult :execrows
-- scope: admin
-- The outcome of the last push: ok clears the error. last_error is a fixed word.
UPDATE push_relays
SET last_ok_at = CASE WHEN sqlc.arg('ok')::bool THEN sqlc.arg('at')::timestamptz ELSE last_ok_at END,
    last_error = sqlc.narg('last_error')
WHERE org_id = $1;

-- name: GetPushDevice :one
-- scope: own
SELECT * FROM push_devices WHERE org_id = $1 AND id = $2;

-- name: ListPushDevices :many
-- scope: own
SELECT * FROM push_devices WHERE org_id = $1 AND user_id = $2 ORDER BY created_at, id;

-- name: CountPushDevices :one
-- scope: own
SELECT count(*) FROM push_devices WHERE org_id = $1 AND user_id = $2;

-- name: ListPushDevicesForUsers :many
-- scope: system
-- The consumer: the devices of the recipients. Ids only (no blob).
SELECT id, user_id FROM push_devices WHERE org_id = $1 AND user_id = ANY(sqlc.arg('user_ids')::uuid[]) ORDER BY user_id, id;

-- name: UpsertPushDevice :one
-- scope: own
-- The caller has checked that an existing row with this id belongs to the same org and user.
INSERT INTO push_devices (id, org_id, user_id, platform, token_enc, token_hash, app_version, label, last_seen_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, sqlc.arg('now')::timestamptz)
ON CONFLICT (org_id, id) DO UPDATE
SET platform = EXCLUDED.platform, token_enc = EXCLUDED.token_enc, token_hash = EXCLUDED.token_hash,
    app_version = EXCLUDED.app_version, label = EXCLUDED.label, last_seen_at = EXCLUDED.last_seen_at
WHERE push_devices.org_id = EXCLUDED.org_id AND push_devices.user_id = EXCLUDED.user_id
RETURNING *;

-- name: DeletePushDevicesByTokenHash :execrows
-- scope: own
-- A token belongs to one device row: a re-registration under another id (or by
-- another member who now holds the phone) takes it over.
DELETE FROM push_devices WHERE org_id = $1 AND token_hash = $2 AND id <> sqlc.arg('keep_id');

-- name: DeleteMyPushDevice :execrows
-- scope: own
DELETE FROM push_devices WHERE org_id = $1 AND user_id = $2 AND id = $3;

-- name: DeleteStalePushDevicesOfUser :execrows
-- scope: own
DELETE FROM push_devices WHERE org_id = $1 AND user_id = $2 AND last_seen_at < sqlc.arg('cutoff');

-- name: DeletePushDeviceIfToken :execrows
-- scope: system
-- After the relay said a token is invalid: delete the device only while it still
-- holds THAT token. A token the app refreshed since is not deleted.
DELETE FROM push_devices WHERE org_id = $1 AND id = $2 AND token_hash = sqlc.arg('token_hash');

-- name: PruneStalePushDevices :execrows
-- scope: system
-- Install-wide retention: devices not seen since the cutoff, at most batch_size.
DELETE FROM push_devices
WHERE (org_id, id) IN (
  SELECT d.org_id, d.id FROM push_devices d WHERE d.last_seen_at < sqlc.arg('cutoff') ORDER BY d.last_seen_at LIMIT sqlc.arg('batch_size')
);

-- name: InsertPushOutbox :one
-- scope: system
-- ON CONFLICT: a replayed event makes no second push.
INSERT INTO push_outbox (org_id, notification_id, device_id)
VALUES ($1, $2, $3)
ON CONFLICT (notification_id, device_id) DO NOTHING
RETURNING id;

-- name: TryLockPushSender :one
-- scope: none
SELECT pg_try_advisory_lock(hashtextextended('devops:push-send', 0)) AS locked;

-- name: UnlockPushSender :one
-- scope: none
SELECT pg_advisory_unlock(hashtextextended('devops:push-send', 0)) AS unlocked;

-- name: RecoverStuckPushOutbox :execrows
-- scope: system
UPDATE push_outbox SET state = 'pending'
WHERE state = 'sending' AND next_attempt_at <= sqlc.arg('cutoff');

-- name: ExpirePushOutbox :execrows
-- scope: system
UPDATE push_outbox SET state = 'skipped', skip_reason = 'expired', next_attempt_at = sqlc.arg('now')
WHERE state = 'pending' AND created_at <= sqlc.arg('cutoff');

-- name: ListDuePushOutbox :many
-- scope: system
-- Install-wide: the sender is. Fair across orgs: at most per_org rows of each
-- org, taken round-robin, then max_rows in all. No FOR UPDATE: one sender holds
-- an advisory lock, and the claim only moves rows that are still pending.
SELECT id, org_id, notification_id, device_id, attempts, created_at
FROM (
  SELECT o.id, o.org_id, o.notification_id, o.device_id, o.attempts, o.created_at,
         row_number() OVER (PARTITION BY o.org_id ORDER BY o.created_at, o.id) AS rn
  FROM push_outbox o
  WHERE o.state = 'pending' AND o.next_attempt_at <= sqlc.arg('now')
) t
WHERE rn <= sqlc.arg('per_org')::int
ORDER BY rn, created_at, id
LIMIT sqlc.arg('max_rows');

-- name: ClaimPushOutbox :execrows
-- scope: system
UPDATE push_outbox SET state = 'sending', next_attempt_at = sqlc.arg('now')
WHERE org_id = $1 AND id = $2 AND state = 'pending';

-- name: SkipPushOutbox :execrows
-- scope: system
UPDATE push_outbox SET state = 'skipped', skip_reason = sqlc.arg('reason'), next_attempt_at = sqlc.arg('now')
WHERE org_id = $1 AND id = $2 AND state = 'sending';

-- name: MarkPushOutboxSent :execrows
-- scope: system
UPDATE push_outbox SET state = 'sent', sent_at = sqlc.arg('now'), last_error = NULL
WHERE org_id = $1 AND id = $2 AND state = 'sending';

-- name: FailPushOutbox :execrows
-- scope: system
UPDATE push_outbox
SET attempts = sqlc.arg('attempts'), state = sqlc.arg('state'), next_attempt_at = sqlc.arg('next_attempt_at'),
    last_error = sqlc.arg('last_error')
WHERE org_id = $1 AND id = $2 AND state = 'sending';

-- name: GetPushSendInfo :one
-- scope: system
-- What the sender needs for one row: the device (sealed token) and whether the
-- notification still wants a wake. Org-bound by both joins.
SELECT d.id AS device_id, d.platform, d.token_enc, d.user_id,
       n.inbox, n.read_at, n.archived_at
FROM push_outbox o
JOIN push_devices d ON d.org_id = o.org_id AND d.id = o.device_id
JOIN notifications n ON n.org_id = o.org_id AND n.id = o.notification_id
WHERE o.org_id = $1 AND o.id = $2;

-- name: CountRecentPushes :one
-- scope: system
SELECT count(*) FROM push_outbox WHERE org_id = $1 AND state = 'sent' AND sent_at > sqlc.arg('since');

-- name: CountDeadPushOutbox :one
-- scope: admin
SELECT count(*) FROM push_outbox WHERE org_id = $1 AND state = 'dead';

-- name: PrunePushOutbox :execrows
-- scope: system
DELETE FROM push_outbox
WHERE id IN (
  SELECT o.id FROM push_outbox o
  WHERE o.state IN ('sent', 'skipped', 'dead') AND o.created_at < sqlc.arg('cutoff')
  ORDER BY o.created_at
  LIMIT sqlc.arg('batch_size')
);

-- name: DeleteOpenPushOutbox :execrows
-- scope: admin
-- A removed relay takes its waiting pushes with it.
DELETE FROM push_outbox WHERE org_id = $1 AND state IN ('pending', 'sending');

-- name: CountPushRateCapped :one
-- scope: admin
SELECT count(*) FROM push_outbox
WHERE org_id = $1 AND state = 'skipped' AND skip_reason = 'rate_cap' AND next_attempt_at > sqlc.arg('since');

-- name: CountPushDevicesOfOrg :one
-- scope: admin
SELECT count(*) FROM push_devices WHERE org_id = $1;
