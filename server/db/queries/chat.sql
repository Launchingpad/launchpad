-- LAUN-204 (T36): the org's chat services and their post outbox. Every query
-- filters by org_id, except the sender's install-wide ones.

-- name: GetChatService :one
-- scope: system
SELECT * FROM chat_services WHERE org_id = $1 AND id = $2;

-- name: GetChatServiceForUpdate :one
-- scope: admin
SELECT * FROM chat_services WHERE org_id = $1 AND id = $2 FOR UPDATE;

-- name: ListChatServices :many
-- scope: admin
SELECT * FROM chat_services WHERE org_id = $1 ORDER BY created_at, id;

-- name: CountChatServices :one
-- scope: admin
SELECT count(*) FROM chat_services WHERE org_id = $1;

-- name: LockOrgForChat :one
-- scope: none
-- Serialises creates of one org, so the cap holds under concurrent POSTs.
SELECT id FROM orgs WHERE id = $1 FOR NO KEY UPDATE;

-- name: ListEnabledChatServiceEvents :many
-- scope: system
-- The consumer reads the event lists only, never the sealed blob. Only the
-- services of the given teams (LAUN-209): an event posts to its team's services.
SELECT id, events FROM chat_services
WHERE org_id = $1 AND enabled AND team_id = ANY(sqlc.arg('team_ids')::uuid[])
ORDER BY created_at, id;

-- name: CountTeamChatServices :one
-- scope: admin
SELECT count(*) FROM chat_services WHERE org_id = $1 AND team_id = $2;

-- name: ListProjectTeamIDs :many
-- scope: system
SELECT team_id FROM project_teams WHERE org_id = $1 AND project_id = $2;

-- name: GetChatTeamExists :one
-- scope: admin
-- A team of this org (the body check of a team id). FOR KEY SHARE: a team
-- delete (FOR UPDATE) cannot run between this check and the write.
SELECT id, archived_at FROM teams WHERE org_id = $1 AND id = $2 FOR KEY SHARE;

-- name: InsertChatService :one
-- scope: admin
INSERT INTO chat_services (id, org_id, name, kind, host, webhook_enc, seal_scope, events, enabled, updated_by, team_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, sqlc.arg('events')::text[], $8, $9, $10)
RETURNING *;

-- name: UpdateChatService :one
-- scope: admin
-- name, team, events, enabled keep their value when the argument is NULL. A new
-- webhook (blob and host together) clears the status.
UPDATE chat_services SET
  name        = COALESCE(sqlc.narg('name'), name),
  team_id     = COALESCE(sqlc.narg('team_id'), team_id),
  events      = COALESCE(sqlc.narg('events')::text[], events),
  enabled     = COALESCE(sqlc.narg('enabled'), enabled),
  webhook_enc = COALESCE(sqlc.narg('webhook_enc'), webhook_enc),
  host        = COALESCE(sqlc.narg('host'), host),
  last_ok_at  = CASE WHEN sqlc.narg('webhook_enc')::bytea IS NOT NULL THEN NULL ELSE last_ok_at END,
  last_error  = CASE WHEN sqlc.narg('webhook_enc')::bytea IS NOT NULL THEN NULL ELSE last_error END,
  updated_by  = sqlc.arg('updated_by'), updated_at = now()
WHERE org_id = $1 AND id = $2
RETURNING *;

-- name: DeleteChatService :execrows
-- scope: admin
DELETE FROM chat_services WHERE org_id = $1 AND id = $2;

-- name: SetChatServiceResult :execrows
-- scope: admin
-- The outcome of the last post: ok clears the error. last_error is a fixed word.
UPDATE chat_services
SET last_ok_at = CASE WHEN sqlc.arg('ok')::bool THEN sqlc.arg('at')::timestamptz ELSE last_ok_at END,
    last_error = sqlc.narg('last_error')
WHERE org_id = $1 AND id = $2;

-- name: InsertChatOutbox :one
-- scope: system
-- ON CONFLICT: a replayed event makes no second post.
INSERT INTO chat_outbox (org_id, service_id, kind, issue_id, project_id, dedupe_key, event_seq)
VALUES ($1, $2, $3, sqlc.narg('issue_id'), sqlc.narg('project_id'), $4, sqlc.narg('event_seq'))
ON CONFLICT (service_id, dedupe_key) DO NOTHING
RETURNING id;

-- name: TryLockChatSender :one
-- scope: none
SELECT pg_try_advisory_lock(hashtextextended('devops:chat-send', 0)) AS locked;

-- name: UnlockChatSender :one
-- scope: none
SELECT pg_advisory_unlock(hashtextextended('devops:chat-send', 0)) AS unlocked;

-- name: RecoverStuckChatOutbox :execrows
-- scope: system
-- A row stuck in sending (a crash): the claim time is next_attempt_at.
UPDATE chat_outbox SET state = 'pending'
WHERE state = 'sending' AND next_attempt_at <= sqlc.arg('cutoff');

-- name: ExpireChatOutbox :execrows
-- scope: system
UPDATE chat_outbox SET state = 'skipped', skip_reason = 'expired', next_attempt_at = sqlc.arg('now')
WHERE state = 'pending' AND created_at <= sqlc.arg('cutoff');

-- name: ListDueChatOutbox :many
-- scope: system
-- Install-wide: the sender is. Fair across orgs AND across the services of one
-- org: rs ranks a service's rows (oldest first, id as the tie-break); rn ranks
-- an org's rows by rs first, so the first row of every service comes before the
-- second row of any (a dead service with many rows cannot fill the org's slots
-- and starve its healthy services). At most per_org rows of each org, taken
-- round-robin across orgs (the first row of every org before the second of any),
-- then max_rows in all.
-- No FOR UPDATE: one sender holds an advisory lock, and the claim only moves
-- rows that are still pending.
SELECT id, org_id, service_id, kind, issue_id, project_id, event_seq, attempts, created_at
FROM (
  SELECT a.id, a.org_id, a.service_id, a.kind, a.issue_id, a.project_id, a.event_seq, a.attempts, a.created_at,
         row_number() OVER (PARTITION BY a.org_id ORDER BY a.rs, a.created_at, a.id) AS rn
  FROM (
    SELECT o.id, o.org_id, o.service_id, o.kind, o.issue_id, o.project_id, o.event_seq, o.attempts, o.created_at,
           row_number() OVER (PARTITION BY o.org_id, o.service_id ORDER BY o.created_at, o.id) AS rs
    FROM chat_outbox o
    WHERE o.state = 'pending' AND o.next_attempt_at <= sqlc.arg('now')
  ) a
) t
WHERE rn <= sqlc.arg('per_org')::int
ORDER BY rn, created_at, id
LIMIT sqlc.arg('max_rows');

-- name: ClaimChatOutbox :execrows
-- scope: system
UPDATE chat_outbox SET state = 'sending', next_attempt_at = sqlc.arg('now')
WHERE org_id = $1 AND id = $2 AND state = 'pending';

-- name: SkipChatOutbox :execrows
-- scope: system
UPDATE chat_outbox SET state = 'skipped', skip_reason = sqlc.arg('reason'), next_attempt_at = sqlc.arg('now')
WHERE org_id = $1 AND id = $2 AND state = 'sending';

-- name: MarkChatOutboxSent :execrows
-- scope: system
UPDATE chat_outbox SET state = 'sent', sent_at = sqlc.arg('now'), last_error = NULL
WHERE org_id = $1 AND id = $2 AND state = 'sending';

-- name: ReleaseChatOutbox :execrows
-- scope: system
-- A claimed row the pass did not try (the service timed out, or the pass budget
-- ended): back to pending, attempts unchanged, still due.
UPDATE chat_outbox SET state = 'pending'
WHERE org_id = $1 AND id = $2 AND state = 'sending';

-- name: FailChatOutbox :execrows
-- scope: system
UPDATE chat_outbox
SET attempts = sqlc.arg('attempts'), state = sqlc.arg('state'), next_attempt_at = sqlc.arg('next_attempt_at'),
    last_error = sqlc.arg('last_error')
WHERE org_id = $1 AND id = $2 AND state = 'sending';

-- name: CountRecentChatPosts :one
-- scope: system
-- The rate cap is per service: each webhook has its own limit.
SELECT count(*) FROM chat_outbox WHERE org_id = $1 AND service_id = $2 AND state = 'sent' AND sent_at > sqlc.arg('since');

-- name: CountDeadChatOutbox :one
-- scope: system
SELECT count(*) FROM chat_outbox WHERE state = 'dead';

-- name: PruneChatOutbox :execrows
-- scope: system
-- Install-wide. Terminal rows older than the cutoff, at most batch_size.
DELETE FROM chat_outbox
WHERE id IN (
  SELECT o.id FROM chat_outbox o
  WHERE o.state IN ('sent', 'skipped', 'dead') AND o.created_at < sqlc.arg('cutoff')
  ORDER BY o.created_at
  LIMIT sqlc.arg('batch_size')
);

-- name: DeleteOpenChatOutbox :execrows
-- scope: admin
-- A removed or disabled service takes its waiting posts with it (a webhook set
-- later must not receive them). Terminal rows stay for the prune.
DELETE FROM chat_outbox WHERE org_id = $1 AND service_id = $2 AND state IN ('pending', 'sending');

-- name: CountChatRateCapped :one
-- scope: admin
-- Posts the rate cap dropped since a time (next_attempt_at holds the decision time).
SELECT count(*) FROM chat_outbox
WHERE org_id = $1 AND service_id = $2 AND state = 'skipped' AND skip_reason = 'rate_cap' AND next_attempt_at > sqlc.arg('since');

-- name: LastChatRateCapped :one
-- scope: admin
SELECT next_attempt_at FROM chat_outbox
WHERE org_id = $1 AND service_id = $2 AND state = 'skipped' AND skip_reason = 'rate_cap' AND next_attempt_at > sqlc.arg('since')
ORDER BY next_attempt_at DESC LIMIT 1;

-- name: GetChatEventStateIDs :one
-- scope: system
-- The from and to state ids a state-change event carried (LAUN-214,
-- decisions-T39). Ids only; the name is read in the same org at post time.
SELECT COALESCE(payload->>'fromStateId', '')::text AS from_state_id, COALESCE(payload->>'toStateId', '')::text AS to_state_id
FROM events
WHERE org_id = $1 AND seq = $2 AND type = 'issue.updated';

-- name: ListChatStateNames :many
-- scope: system
-- Current names of workflow states, org-scoped: an id of another org, or a
-- deleted one, returns no row.
SELECT id, name FROM workflow_states
WHERE org_id = $1 AND id = ANY(sqlc.arg('ids')::uuid[]);

-- name: ClaimChatMention :one
-- scope: system
-- The chat side of a mention (decisions-T20 #45, LAUN-223): the first event
-- that names the key claims it; the row keeps that event's seq, so a replay of
-- it gets the same answer and a later event gets an older seq (not new).
INSERT INTO chat_mentions (org_id, dedupe_key, event_seq) VALUES ($1, $2, $3)
ON CONFLICT (org_id, dedupe_key) DO UPDATE SET event_seq = chat_mentions.event_seq
RETURNING event_seq;
