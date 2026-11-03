-- Notifications (LAUN-31, docs/internal/decisions-T13-notifications.md).
-- Basic reads and writes for the later tickets; no handler uses them yet.

-- name: GetNotificationUserSettings :one
-- scope: own
SELECT * FROM notification_user_settings WHERE org_id = $1 AND user_id = $2;

-- name: UpsertNotificationUserSettings :one
-- scope: own
INSERT INTO notification_user_settings (org_id, user_id, email_enabled, include_excerpt)
VALUES ($1, $2, $3, $4)
ON CONFLICT (org_id, user_id) DO UPDATE
SET email_enabled = EXCLUDED.email_enabled, include_excerpt = EXCLUDED.include_excerpt, updated_at = now()
RETURNING *;

-- name: ListNotificationSettings :many
-- scope: team
-- The caller's rows: the global ones and those of teams the caller sees
-- (T20 #64). A row of a hidden team stays stored and is not listed.
SELECT * FROM notification_settings WHERE org_id = $1 AND user_id = $2
  AND (team_id IS NULL OR (sqlc.arg(all_teams)::bool OR team_id = ANY(sqlc.arg(team_ids)::uuid[])))
ORDER BY event, team_id NULLS FIRST;

-- name: ListNotificationSettingsSystem :many
-- scope: system
-- The unscoped copy: the mail sender and the unsubscribe route resolve a
-- recipient's settings with no caller; their team comes from the issue.
SELECT * FROM notification_settings WHERE org_id = $1 AND user_id = $2 ORDER BY event, team_id NULLS FIRST;

-- name: UpsertGlobalNotificationSetting :one
-- scope: own
-- Overwrites both columns: callers read, merge and then write.
INSERT INTO notification_settings (org_id, user_id, event, email, inbox, push)
VALUES ($1, $2, $3, sqlc.narg('email'), sqlc.narg('inbox'), sqlc.narg('push'))
ON CONFLICT (org_id, user_id, event) WHERE team_id IS NULL DO UPDATE
SET email = EXCLUDED.email, inbox = EXCLUDED.inbox, push = EXCLUDED.push, updated_at = now()
RETURNING *;

-- name: UpsertTeamNotificationSetting :one
-- scope: resolved
-- Overwrites both columns: callers read, merge and then write.
INSERT INTO notification_settings (org_id, user_id, team_id, event, email, inbox, push)
VALUES ($1, $2, $3, $4, sqlc.narg('email'), sqlc.narg('inbox'), sqlc.narg('push'))
ON CONFLICT (org_id, user_id, team_id, event) WHERE team_id IS NOT NULL DO UPDATE
SET email = EXCLUDED.email, inbox = EXCLUDED.inbox, push = EXCLUDED.push, updated_at = now()
RETURNING *;

-- name: DeleteEmptyNotificationSettings :exec
-- scope: own
-- A row with both columns NULL is deleted on write (#3).
DELETE FROM notification_settings WHERE org_id = $1 AND user_id = $2 AND email IS NULL AND inbox IS NULL AND push IS NULL;

-- name: ListTeamFollows :many
-- scope: team
-- The follows of teams the caller sees (T20 #64).
SELECT team_id FROM team_follows WHERE org_id = $1 AND user_id = $2
  AND (sqlc.arg(all_teams)::bool OR team_id = ANY(sqlc.arg(team_ids)::uuid[]))
ORDER BY team_id;

-- name: ListTeamFollowers :many
-- scope: system
SELECT user_id FROM team_follows WHERE org_id = $1 AND team_id = $2;

-- name: InsertTeamFollow :exec
-- scope: resolved
INSERT INTO team_follows (org_id, user_id, team_id) VALUES ($1, $2, $3)
ON CONFLICT (org_id, user_id, team_id) DO NOTHING;

-- name: DeleteTeamFollow :exec
-- scope: resolved
DELETE FROM team_follows WHERE org_id = $1 AND user_id = $2 AND team_id = $3;

-- name: GetIssueSubscription :one
-- scope: resolved
SELECT * FROM issue_subscriptions WHERE org_id = $1 AND issue_id = $2 AND user_id = $3;

-- name: ListIssueSubscriptions :many
-- scope: system
SELECT * FROM issue_subscriptions WHERE org_id = $1 AND issue_id = $2;

-- name: UpsertIssueSubscription :one
-- scope: resolved
INSERT INTO issue_subscriptions (org_id, issue_id, user_id, state, reason)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (org_id, issue_id, user_id) DO UPDATE
SET state = EXCLUDED.state, reason = EXCLUDED.reason, updated_at = now()
RETURNING *;

-- name: InsertMentionSubscription :exec
-- scope: system
-- A mention adds a subscribed row only when no row exists (#9).
INSERT INTO issue_subscriptions (org_id, issue_id, user_id, state, reason)
VALUES ($1, $2, $3, 'subscribed', 'mentioned')
ON CONFLICT (org_id, issue_id, user_id) DO NOTHING;

-- name: DeleteIssueSubscription :exec
-- scope: resolved
DELETE FROM issue_subscriptions WHERE org_id = $1 AND issue_id = $2 AND user_id = $3;

-- name: InsertNotification :one
-- scope: system
-- Exactly once per (recipient, dedupe key): a repeat returns no row.
INSERT INTO notifications (org_id, recipient_id, kind, issue_id, project_id, actor_id, data, dedupe_key, event_seq, inbox)
VALUES ($1, $2, $3, sqlc.narg('issue_id'), sqlc.narg('project_id'), sqlc.narg('actor_id'), $4, $5, sqlc.narg('event_seq'), $6)
ON CONFLICT (org_id, recipient_id, dedupe_key) DO NOTHING
RETURNING *;

-- name: InsertNotificationEmail :one
-- scope: system
INSERT INTO notification_emails (org_id, notification_id, recipient_id, issue_id, project_id)
VALUES ($1, $2, $3, sqlc.narg('issue_id'), sqlc.narg('project_id'))
ON CONFLICT (notification_id) DO NOTHING
RETURNING *;

-- name: ListClaimableNotificationEmails :many
-- scope: system
-- No org_id on purpose: the mail worker is install-wide.
SELECT * FROM notification_emails
WHERE state = 'pending' AND next_attempt_at <= now()
ORDER BY next_attempt_at
LIMIT sqlc.arg('max_rows')
FOR UPDATE SKIP LOCKED;

-- name: GetNotificationCursor :one
-- scope: system
SELECT * FROM notification_cursors WHERE org_id = $1;

-- name: InsertNotificationCursor :exec
-- scope: system
INSERT INTO notification_cursors (org_id, last_seq)
VALUES ($1, COALESCE((SELECT max(seq) FROM events WHERE org_id = $1), 0))
ON CONFLICT (org_id) DO NOTHING;

-- name: LockNotificationCursor :one
-- scope: system
SELECT * FROM notification_cursors WHERE org_id = $1 FOR UPDATE;

-- name: AdvanceNotificationCursor :exec
-- scope: system
UPDATE notification_cursors SET last_seq = $2, updated_at = now() WHERE org_id = $1;

-- name: InsertNotificationMailToken :exec
-- scope: system
INSERT INTO notification_mail_tokens (token_hash, org_id, user_id, targets, expires_at)
VALUES ($1, $2, $3, $4, $5);

-- name: GetNotificationMailToken :one
-- scope: system
-- No org_id on purpose (the token is the key). Callers MUST check org_id and user_id on the row.
SELECT * FROM notification_mail_tokens WHERE token_hash = $1;

-- name: MarkNotificationMailTokenUsed :execrows
-- scope: system
-- No org_id on purpose: the token hash is the key.
UPDATE notification_mail_tokens SET used_at = now() WHERE token_hash = $1 AND used_at IS NULL;

-- Retention (decisions-T13 #10). The cutoffs come from internal/notify
-- (retention.go); the job (RunRetention, LAUN-44) called each query in a loop.
-- Each query was one batched DELETE.

-- name: PruneNotifications :execrows
-- scope: system
-- No org_id on purpose: retention is install-wide.
-- Read or archived rows older than read_cutoff, every row older than all_cutoff.
-- Their outbox rows go by CASCADE. At most batch_size rows per statement (LAUN-44).
DELETE FROM notifications
WHERE id IN (
  SELECT n.id FROM notifications n
  WHERE n.created_at < sqlc.arg('read_cutoff')
    AND (n.created_at < sqlc.arg('all_cutoff') OR n.read_at IS NOT NULL OR n.archived_at IS NOT NULL)
  ORDER BY n.created_at
  LIMIT sqlc.arg('batch_size')
);

-- name: PruneNotificationEmails :execrows
-- scope: system
-- No org_id on purpose: retention is install-wide.
-- Sent and skipped rows older than sent_cutoff; dead rows older than dead_cutoff.
-- At most batch_size rows per statement.
DELETE FROM notification_emails
WHERE id IN (
  SELECT e.id FROM notification_emails e
  WHERE (e.state IN ('sent', 'skipped') AND e.created_at < sqlc.arg('sent_cutoff'))
     OR (e.state = 'dead' AND e.created_at < sqlc.arg('dead_cutoff'))
  ORDER BY e.created_at
  LIMIT sqlc.arg('batch_size')
);

-- name: PruneNotificationMailTokens :execrows
-- scope: system
-- No org_id on purpose: retention is install-wide. At most batch_size rows.
DELETE FROM notification_mail_tokens
WHERE token_hash IN (
  SELECT t.token_hash FROM notification_mail_tokens t
  WHERE t.expires_at < sqlc.arg('cutoff')
  ORDER BY t.expires_at
  LIMIT sqlc.arg('batch_size')
);

-- LAUN-32: the preference and subscription API.

-- name: GetIssueSubscriptionFacts :one
-- scope: resolved
-- Why the caller is an implicit subscriber of the issue (#9): creator,
-- assignee, or a commenter. One row, or no row when the issue is not in the org.
SELECT i.creator_id, i.assignee_id,
       EXISTS (SELECT 1 FROM comments c
               WHERE c.org_id = i.org_id AND c.issue_id = i.id AND c.author_id = sqlc.arg('user_id')) AS commenter
FROM issues i WHERE i.org_id = $1 AND i.id = $2;

-- LAUN-33: the consumer (decisions-T13 #15 to #17).

-- name: ListOrgIDs :many
-- scope: none
SELECT id FROM orgs ORDER BY id;

-- name: TryLockNotifyOrg :one
-- scope: none
-- A transaction-level try lock per org: a second instance gets false and skips the org.
SELECT pg_try_advisory_xact_lock(hashtextextended('devops:notify:' || sqlc.arg('org_id')::text, 0)) AS locked;

-- name: GetNotifyIssue :one
-- scope: system
-- The issue as the consumer needs it. No row: the issue is gone (a poison event).
SELECT i.id, i.team_id, i.creator_id, i.assignee_id, i.description,
       i.archived_at AS issue_archived_at, t.archived_at AS team_archived_at
FROM issues i
JOIN teams t ON t.org_id = i.org_id AND t.id = i.team_id
WHERE i.org_id = $1 AND i.id = $2;

-- name: ListIssueCommenterIDs :many
-- scope: system
-- Commenters as of the event: a comment written after it does not make its
-- author a recipient of it, so a replay decides as the first pass did.
SELECT DISTINCT author_id::uuid AS user_id FROM comments
WHERE org_id = $1 AND issue_id = $2 AND author_id IS NOT NULL AND created_at <= sqlc.arg('as_of');

-- name: ListNotifyEligibleUsers :many
-- scope: system
-- Active, non-agent, current members among user_ids (decisions-T13 #14) who
-- see the team (T20 #61): an org admin, an 'all' member, or a team_members
-- row for it. Any other team_access value fails closed.
SELECT u.id FROM users u
JOIN org_members m ON m.user_id = u.id
WHERE m.org_id = $1 AND u.id = ANY(sqlc.arg('user_ids')::uuid[])
  AND u.deactivated_at IS NULL AND NOT u.is_agent
  AND (m.role = 'admin' OR m.team_access = 'all'
       OR EXISTS (SELECT 1 FROM team_members tm
                  WHERE tm.org_id = m.org_id AND tm.user_id = m.user_id AND tm.team_id = sqlc.arg('team_id')::uuid));

-- name: ListNotifyEligibleProjectUsers :many
-- scope: system
-- As ListNotifyEligibleUsers, for a project (T20 #36, #61): a project with no
-- team is org-level; else the user sees one of its teams.
SELECT u.id FROM users u
JOIN org_members m ON m.user_id = u.id
WHERE m.org_id = $1 AND u.id = ANY(sqlc.arg('user_ids')::uuid[])
  AND u.deactivated_at IS NULL AND NOT u.is_agent
  AND (m.role = 'admin' OR m.team_access = 'all'
       OR NOT EXISTS (SELECT 1 FROM project_teams pt
                      WHERE pt.org_id = m.org_id AND pt.project_id = sqlc.arg('project_id')::uuid)
       OR EXISTS (SELECT 1 FROM project_teams pt
                  JOIN team_members tm ON tm.org_id = pt.org_id AND tm.team_id = pt.team_id
                  WHERE pt.org_id = m.org_id AND pt.project_id = sqlc.arg('project_id')::uuid
                    AND tm.user_id = m.user_id));

-- name: ListActiveHumanMembers :many
-- scope: system
-- Active, non-agent, current members among user_ids, whatever their team
-- access. Only the chat services read it (T20: a shared channel is not a
-- person, so its rules do not depend on one member's access).
SELECT u.id FROM users u
JOIN org_members m ON m.user_id = u.id
WHERE m.org_id = $1 AND u.id = ANY(sqlc.arg('user_ids')::uuid[])
  AND u.deactivated_at IS NULL AND NOT u.is_agent;

-- name: ListMentionableUsers :many
-- scope: system
-- The usernames of a text that name an active, non-agent member of THIS org
-- (#17). sees_team: the member sees the team (T20 #45, #61, the rule of
-- ListNotifyEligibleUsers). Only a member who sees it is notified or
-- subscribed; the chat services read every row (a shared channel).
SELECT u.id, u.username,
       (m.role = 'admin' OR m.team_access = 'all'
        OR EXISTS (SELECT 1 FROM team_members tm
                   WHERE tm.org_id = m.org_id AND tm.user_id = m.user_id AND tm.team_id = sqlc.arg('team_id')::uuid))::bool AS sees_team
FROM users u
JOIN org_members m ON m.user_id = u.id
WHERE m.org_id = $1 AND u.username = ANY(sqlc.arg('usernames')::text[])
  AND u.deactivated_at IS NULL AND NOT u.is_agent;

-- name: ListNotificationSettingsForUsers :many
-- scope: system
SELECT * FROM notification_settings
WHERE org_id = $1 AND user_id = ANY(sqlc.arg('user_ids')::uuid[]);

-- name: ListNotificationUserSettingsForUsers :many
-- scope: system
SELECT * FROM notification_user_settings
WHERE org_id = $1 AND user_id = ANY(sqlc.arg('user_ids')::uuid[]);

-- name: GetNotificationEventSeqByKey :one
-- scope: system
-- The event that made a notification (NULL event_seq: none). No row: not made yet.
SELECT event_seq FROM notifications WHERE org_id = $1 AND recipient_id = $2 AND dedupe_key = $3;

-- LAUN-34: the outbox sender and the unsubscribe route (decisions-T13 #19 to #25).
-- The sender is install-wide, so the claim and expiry queries carry no org_id
-- on purpose; every query that touches one row's data does.

-- name: TryLockNotifySender :one
-- scope: none
-- A SESSION-level try lock, held by one connection for the whole pass, so two
-- senders never send the same row.
SELECT pg_try_advisory_lock(hashtextextended('devops:notify-send', 0)) AS locked;

-- name: UnlockNotifySender :one
-- scope: none
SELECT pg_advisory_unlock(hashtextextended('devops:notify-send', 0)) AS unlocked;

-- name: RecoverStuckNotificationEmails :execrows
-- scope: system
-- A row stuck in sending (a crash): the claim time is next_attempt_at.
UPDATE notification_emails SET state = 'pending'
WHERE state = 'sending' AND next_attempt_at <= sqlc.arg('cutoff');

-- name: ExpireNotificationEmails :execrows
-- scope: system
-- Every row expires 24 hours after creation (#20). next_attempt_at holds the
-- time of the decision for a skipped row.
UPDATE notification_emails SET state = 'skipped', skip_reason = 'expired', next_attempt_at = sqlc.arg('now')
WHERE state = 'pending' AND created_at <= sqlc.arg('cutoff');

-- name: ListDueNotificationEmails :many
-- scope: system
SELECT e.id, e.org_id, e.notification_id, e.recipient_id, e.issue_id, e.project_id, e.attempts, e.batch_id, e.created_at,
       n.kind, n.actor_id, n.data
FROM notification_emails e
JOIN notifications n ON n.org_id = e.org_id AND n.id = e.notification_id
WHERE e.state = 'pending' AND e.next_attempt_at <= sqlc.arg('now')
ORDER BY e.org_id, e.recipient_id, e.issue_id NULLS LAST, e.project_id NULLS LAST, e.created_at, e.id
LIMIT sqlc.arg('max_rows')
FOR UPDATE OF e SKIP LOCKED;

-- name: ClaimNotificationEmails :execrows
-- scope: system
UPDATE notification_emails SET state = 'sending', next_attempt_at = sqlc.arg('now'), batch_id = sqlc.arg('batch_id')
WHERE org_id = $1 AND id = ANY(sqlc.arg('ids')::uuid[]) AND state = 'pending';

-- name: ReleaseNotificationEmails :execrows
-- scope: system
-- Mail was turned off while the row was claimed: back to pending, no attempt counted.
UPDATE notification_emails SET state = 'pending', next_attempt_at = sqlc.arg('now')
WHERE org_id = $1 AND id = ANY(sqlc.arg('ids')::uuid[]) AND state = 'sending';

-- name: SkipNotificationEmails :execrows
-- scope: system
UPDATE notification_emails SET state = 'skipped', skip_reason = sqlc.arg('reason'), next_attempt_at = sqlc.arg('now')
WHERE org_id = $1 AND id = ANY(sqlc.arg('ids')::uuid[]) AND state = 'sending';

-- name: MarkNotificationEmailsSent :execrows
-- scope: system
UPDATE notification_emails SET state = 'sent', sent_at = sqlc.arg('now'), batch_id = sqlc.arg('batch_id'), last_error = NULL
WHERE org_id = $1 AND id = ANY(sqlc.arg('ids')::uuid[]) AND state = 'sending';

-- name: FailNotificationEmail :execrows
-- scope: system
UPDATE notification_emails
SET attempts = sqlc.arg('attempts'), state = sqlc.arg('state'), next_attempt_at = sqlc.arg('next_attempt_at'), last_error = sqlc.arg('last_error')
WHERE org_id = $1 AND id = $2 AND state = 'sending';

-- name: CountRecentMailBatches :one
-- scope: system
-- The rate cap counts MAILS: distinct batch_id of the recipient's sent rows (#23).
SELECT count(DISTINCT batch_id) FROM notification_emails
WHERE org_id = $1 AND recipient_id = $2 AND state = 'sent' AND sent_at > sqlc.arg('since');

-- name: CountUnreportedRateCapped :one
-- scope: system
-- Updates the rate cap kept out since the recipient's last mail (#23).
SELECT count(*) FROM notification_emails e
WHERE e.org_id = $1 AND e.recipient_id = $2 AND e.state = 'skipped' AND e.skip_reason = 'rate_cap'
  AND e.next_attempt_at >= COALESCE(
      (SELECT max(s.sent_at) FROM notification_emails s
       WHERE s.org_id = $1 AND s.recipient_id = $2 AND s.state = 'sent'),
      'epoch'::timestamptz);

-- name: CountDeadNotificationEmails :one
-- scope: system
SELECT count(*) FROM notification_emails WHERE state = 'dead';

-- name: GetMailRecipient :one
-- scope: system
-- Active, address, proof and role of the recipient; no row: not a member any more.
SELECT u.email, u.is_agent, u.deactivated_at, u.email_unproven
FROM users u JOIN org_members m ON m.user_id = u.id
WHERE m.org_id = $1 AND u.id = $2;

-- name: GetMailIssue :one
-- scope: system
SELECT i.id, i.title, i.number, i.description, i.team_id, t.key AS team_key, s.name AS state_name, o.slug AS org_slug,
       i.archived_at AS issue_archived_at, t.archived_at AS team_archived_at
FROM issues i
JOIN teams t ON t.org_id = i.org_id AND t.id = i.team_id
JOIN workflow_states s ON s.org_id = i.org_id AND s.id = i.state_id
JOIN orgs o ON o.id = i.org_id
WHERE i.org_id = $1 AND i.id = $2;

-- name: MailRecipientSeesTeam :one
-- scope: system
-- The mail sender's re-check at send time (T20 #63): the recipient still sees
-- the team (the rule of ListNotifyEligibleUsers). false: no membership row.
SELECT EXISTS (SELECT 1 FROM org_members m
               WHERE m.org_id = $1 AND m.user_id = sqlc.arg('user_id')::uuid
                 AND (m.role = 'admin' OR m.team_access = 'all'
                      OR EXISTS (SELECT 1 FROM team_members tm
                                 WHERE tm.org_id = m.org_id AND tm.user_id = m.user_id
                                   AND tm.team_id = sqlc.arg('team_id')::uuid)))::bool AS sees;

-- name: MailRecipientSeesProject :one
-- scope: system
-- The same re-check for a project reminder (T20 #36, #63).
SELECT EXISTS (SELECT 1 FROM org_members m
               WHERE m.org_id = $1 AND m.user_id = sqlc.arg('user_id')::uuid
                 AND (m.role = 'admin' OR m.team_access = 'all'
                      OR NOT EXISTS (SELECT 1 FROM project_teams pt
                                     WHERE pt.org_id = m.org_id AND pt.project_id = sqlc.arg('project_id')::uuid)
                      OR EXISTS (SELECT 1 FROM project_teams pt
                                 JOIN team_members tm ON tm.org_id = pt.org_id AND tm.team_id = pt.team_id
                                 WHERE pt.org_id = m.org_id AND pt.project_id = sqlc.arg('project_id')::uuid
                                   AND tm.user_id = m.user_id)))::bool AS sees;

-- name: GetMailProject :one
-- scope: system
SELECT p.id, p.name, p.archived_at AS project_archived_at, o.slug AS org_slug
FROM projects p JOIN orgs o ON o.id = p.org_id
WHERE p.org_id = $1 AND p.id = $2;

-- name: ListStateNamesByIDs :many
-- scope: system
SELECT id, name FROM workflow_states WHERE org_id = $1 AND id = ANY(sqlc.arg('ids')::uuid[]);

-- name: ListUserNamesByIDs :many
-- scope: none
-- Actor names. The ids come from notification rows of one org.
SELECT id, name FROM users WHERE id = ANY(sqlc.arg('ids')::uuid[]);

-- Inbox API (LAUN-35, decisions-T13 #27-#31). Every query takes the caller's
-- org and user and only reads rows with inbox = true: an email-only row is
-- never returned.

-- name: ListInbox :many
-- scope: team
-- Newest first by (created_at, id); the keyset is the pair. Never ORDER BY id
-- alone (uuidv7 is not monotonic within a millisecond).
SELECT n.id, n.kind, n.created_at, n.read_at, n.archived_at, n.data,
       i.id AS issue_id, COALESCE(t.key || '-' || i.number, '')::text AS identifier, i.title AS issue_title,
       t.key AS team_key, st.name AS state_name,
       pr.id AS project_id, pr.name AS project_name,
       a.id AS actor_id, a.username AS actor_username, a.name AS actor_name,
       fs.name AS from_state_name, ts.name AS to_state_name
FROM notifications n
LEFT JOIN issues i ON i.org_id = n.org_id AND i.id = n.issue_id
LEFT JOIN teams t ON t.org_id = i.org_id AND t.id = i.team_id
LEFT JOIN workflow_states st ON st.org_id = i.org_id AND st.id = i.state_id
LEFT JOIN projects pr ON pr.org_id = n.org_id AND pr.id = n.project_id
LEFT JOIN users a ON a.id = n.actor_id
LEFT JOIN workflow_states fs ON fs.org_id = n.org_id AND fs.id = NULLIF(n.data ->> 'fromStateId', '')::uuid
LEFT JOIN workflow_states ts ON ts.org_id = n.org_id AND ts.id = NULLIF(n.data ->> 'toStateId', '')::uuid
WHERE n.org_id = sqlc.arg(org_id) AND n.recipient_id = sqlc.arg(user_id) AND n.inbox
  AND (n.issue_id IS NULL OR EXISTS (SELECT 1 FROM issues si WHERE si.org_id = n.org_id AND si.id = n.issue_id
        AND (sqlc.arg(all_teams)::bool OR si.team_id = ANY(sqlc.arg(team_ids)::uuid[]))))
  AND (n.project_id IS NULL OR EXISTS (SELECT 1 FROM projects sp WHERE sp.org_id = n.org_id AND sp.id = n.project_id
        AND (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = sp.org_id AND pst.project_id = sp.id)
    OR EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = sp.org_id AND pst.project_id = sp.id
               AND pst.team_id = ANY(sqlc.arg(team_ids)::uuid[])))))
  AND ((sqlc.arg(archived)::bool AND n.archived_at IS NOT NULL) OR (NOT sqlc.arg(archived)::bool AND n.archived_at IS NULL))
  AND (NOT sqlc.arg(unread_only)::bool OR n.read_at IS NULL)
  AND (sqlc.narg(cursor_at)::timestamptz IS NULL
       OR (n.created_at, n.id) < (sqlc.narg(cursor_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
ORDER BY n.created_at DESC, n.id DESC
LIMIT sqlc.arg(row_limit);

-- name: GetInboxItem :one
-- scope: team
SELECT n.id, n.kind, n.created_at, n.read_at, n.archived_at, n.data,
       i.id AS issue_id, COALESCE(t.key || '-' || i.number, '')::text AS identifier, i.title AS issue_title,
       t.key AS team_key, st.name AS state_name,
       pr.id AS project_id, pr.name AS project_name,
       a.id AS actor_id, a.username AS actor_username, a.name AS actor_name,
       fs.name AS from_state_name, ts.name AS to_state_name
FROM notifications n
LEFT JOIN issues i ON i.org_id = n.org_id AND i.id = n.issue_id
LEFT JOIN teams t ON t.org_id = i.org_id AND t.id = i.team_id
LEFT JOIN workflow_states st ON st.org_id = i.org_id AND st.id = i.state_id
LEFT JOIN projects pr ON pr.org_id = n.org_id AND pr.id = n.project_id
LEFT JOIN users a ON a.id = n.actor_id
LEFT JOIN workflow_states fs ON fs.org_id = n.org_id AND fs.id = NULLIF(n.data ->> 'fromStateId', '')::uuid
LEFT JOIN workflow_states ts ON ts.org_id = n.org_id AND ts.id = NULLIF(n.data ->> 'toStateId', '')::uuid
WHERE n.org_id = $1 AND n.recipient_id = $2 AND n.id = $3 AND n.inbox
  AND (n.issue_id IS NULL OR EXISTS (SELECT 1 FROM issues si WHERE si.org_id = n.org_id AND si.id = n.issue_id
        AND (sqlc.arg(all_teams)::bool OR si.team_id = ANY(sqlc.arg(team_ids)::uuid[]))))
  AND (n.project_id IS NULL OR EXISTS (SELECT 1 FROM projects sp WHERE sp.org_id = n.org_id AND sp.id = n.project_id
        AND (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = sp.org_id AND pst.project_id = sp.id)
    OR EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = sp.org_id AND pst.project_id = sp.id
               AND pst.team_id = ANY(sqlc.arg(team_ids)::uuid[])))));

-- name: CountUnreadInbox :one
-- scope: team
-- Counts only rows the caller can see (T20 #62), so the bell matches the list.
SELECT count(*) FROM notifications n
WHERE n.org_id = $1 AND n.recipient_id = $2 AND n.inbox AND n.read_at IS NULL AND n.archived_at IS NULL
  AND (n.issue_id IS NULL OR EXISTS (SELECT 1 FROM issues si WHERE si.org_id = n.org_id AND si.id = n.issue_id
        AND (sqlc.arg(all_teams)::bool OR si.team_id = ANY(sqlc.arg(team_ids)::uuid[]))))
  AND (n.project_id IS NULL OR EXISTS (SELECT 1 FROM projects sp WHERE sp.org_id = n.org_id AND sp.id = n.project_id
        AND (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = sp.org_id AND pst.project_id = sp.id)
    OR EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = sp.org_id AND pst.project_id = sp.id
               AND pst.team_id = ANY(sqlc.arg(team_ids)::uuid[])))));

-- name: SetInboxItemState :one
-- scope: team
-- Sets a state, never toggles: read = true keeps the first read_at, false
-- clears it; archived works the same way. A flag that is not named is left
-- alone. Returns the id, or no row when the caller has no such inbox row.
UPDATE notifications n SET
  read_at = CASE WHEN sqlc.arg(set_read)::bool
                 THEN (CASE WHEN sqlc.arg(read)::bool THEN COALESCE(read_at, now()) ELSE NULL END)
                 ELSE read_at END,
  archived_at = CASE WHEN sqlc.arg(set_archived)::bool
                     THEN (CASE WHEN sqlc.arg(archived)::bool THEN COALESCE(archived_at, now()) ELSE NULL END)
                     ELSE archived_at END
WHERE n.org_id = sqlc.arg(org_id) AND n.recipient_id = sqlc.arg(user_id) AND n.id = sqlc.arg(id) AND n.inbox
  AND (n.issue_id IS NULL OR EXISTS (SELECT 1 FROM issues si WHERE si.org_id = n.org_id AND si.id = n.issue_id
        AND (sqlc.arg(all_teams)::bool OR si.team_id = ANY(sqlc.arg(team_ids)::uuid[]))))
  AND (n.project_id IS NULL OR EXISTS (SELECT 1 FROM projects sp WHERE sp.org_id = n.org_id AND sp.id = n.project_id
        AND (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = sp.org_id AND pst.project_id = sp.id)
    OR EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = sp.org_id AND pst.project_id = sp.id
               AND pst.team_id = ANY(sqlc.arg(team_ids)::uuid[])))))
RETURNING n.id;

-- name: MarkInboxReadBefore :execrows
-- scope: team
-- Only unread, unarchived rows made at or before the cut-off: a retry marks
-- the same rows and never one that arrived later.
UPDATE notifications n SET read_at = now()
WHERE n.org_id = $1 AND n.recipient_id = $2 AND n.inbox AND n.read_at IS NULL AND n.archived_at IS NULL
  AND n.created_at <= $3
  AND (n.issue_id IS NULL OR EXISTS (SELECT 1 FROM issues si WHERE si.org_id = n.org_id AND si.id = n.issue_id
        AND (sqlc.arg(all_teams)::bool OR si.team_id = ANY(sqlc.arg(team_ids)::uuid[]))))
  AND (n.project_id IS NULL OR EXISTS (SELECT 1 FROM projects sp WHERE sp.org_id = n.org_id AND sp.id = n.project_id
        AND (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = sp.org_id AND pst.project_id = sp.id)
    OR EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = sp.org_id AND pst.project_id = sp.id
               AND pst.team_id = ANY(sqlc.arg(team_ids)::uuid[])))));

-- name: ListInboxStreamRows :many
-- scope: team
-- The live stream: new rows after the connection's cursor, in inbox_seq order.
SELECT n.id, n.kind, n.inbox_seq::bigint AS inbox_seq FROM notifications n
WHERE n.org_id = $1 AND n.recipient_id = $2 AND n.inbox AND n.archived_at IS NULL AND n.inbox_seq > sqlc.arg(after_seq)::bigint
  AND (n.issue_id IS NULL OR EXISTS (SELECT 1 FROM issues si WHERE si.org_id = n.org_id AND si.id = n.issue_id
        AND (sqlc.arg(all_teams)::bool OR si.team_id = ANY(sqlc.arg(team_ids)::uuid[]))))
  AND (n.project_id IS NULL OR EXISTS (SELECT 1 FROM projects sp WHERE sp.org_id = n.org_id AND sp.id = n.project_id
        AND (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = sp.org_id AND pst.project_id = sp.id)
    OR EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = sp.org_id AND pst.project_id = sp.id
               AND pst.team_id = ANY(sqlc.arg(team_ids)::uuid[])))))
ORDER BY n.inbox_seq
LIMIT sqlc.arg(row_limit);

-- name: MaxInboxSeq :one
-- scope: own
SELECT COALESCE(max(inbox_seq), 0)::bigint FROM notifications WHERE org_id = $1 AND recipient_id = $2;

-- name: GetNotifyProject :one
-- scope: system
SELECT id, name, archived_at FROM projects WHERE org_id = $1 AND id = $2;

-- T20 #50 (LAUN-223): the cleanup of a member's own notification rows for the
-- teams they lose. The access setter (ticket 9, admin scope) calls them in its
-- transaction through tracker.RemoveLostTeamNotifyRows. Hard deletes; no event
-- (per-user private rows). Inbox rows are kept and hidden at read time (#51).

-- name: DeleteMemberSubscriptionsInTeams :execrows
-- scope: admin
DELETE FROM issue_subscriptions s
USING issues i
WHERE s.org_id = $1 AND s.user_id = sqlc.arg('user_id')::uuid
  AND i.org_id = s.org_id AND i.id = s.issue_id AND i.team_id = ANY(sqlc.arg('lost_team_ids')::uuid[]);

-- name: DeleteMemberTeamFollowsInTeams :execrows
-- scope: admin
DELETE FROM team_follows
WHERE org_id = $1 AND user_id = sqlc.arg('user_id')::uuid AND team_id = ANY(sqlc.arg('lost_team_ids')::uuid[]);

-- name: DeleteMemberTeamSettingsInTeams :execrows
-- scope: admin
DELETE FROM notification_settings
WHERE org_id = $1 AND user_id = sqlc.arg('user_id')::uuid AND team_id = ANY(sqlc.arg('lost_team_ids')::uuid[]);
