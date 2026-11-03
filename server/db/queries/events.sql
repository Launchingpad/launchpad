-- name: NextEventSeq :one
-- scope: system
-- The upsert takes the org's counter row lock and holds it to COMMIT, so per
-- org the seq order is the commit order. Call it as the LAST write in a
-- transaction to keep the lock short.
INSERT INTO org_event_counters (org_id, next_seq) VALUES ($1, 2)
ON CONFLICT (org_id) DO UPDATE SET next_seq = org_event_counters.next_seq + 1
RETURNING (next_seq - 1)::bigint AS seq;

-- name: InsertEvent :exec
-- scope: system
-- scope and team_ids say who may read the row (T20 #53); events.Emit checks
-- them before the insert.
INSERT INTO events (org_id, seq, type, entity_type, entity_id, actor_id, payload, scope, team_ids)
VALUES ($1, $2, $3, $4, $5, $6, $7, sqlc.arg(scope), sqlc.arg(team_ids)::uuid[]);

-- name: NotifyEvents :exec
-- scope: none
-- Delivered at COMMIT, and not at all on rollback.
SELECT pg_notify('devops_events', sqlc.arg(org_id)::text);

-- name: ListEventsAfter :many
-- scope: feed
-- The feed of a viewer (T20 #55): GET /events and GET /events/stream. An org
-- admin (is_admin, which implies all_teams) reads every row. An unrestricted
-- member (all_teams) reads every row but the admin ones, legacy rows
-- included. A restricted member reads org rows, 'any' rows that share a team
-- with the visible set and 'all' rows whose every team is visible; never a
-- legacy or an admin row (fail closed). A legacy row of an admin-only type
-- (member.access_changed, LAUN-225: one can come back as legacy after a
-- migration down past 0038 and up) is read by admins only, before and after
-- the backfill places it. The filter is in the WHERE, before the LIMIT, so a
-- page is honest (#24).
SELECT e.seq, e.type, e.entity_type, e.entity_id, e.actor_id, u.username AS actor_username, u.name AS actor_name,
       e.payload, e.created_at
FROM events e
LEFT JOIN users u ON u.id = e.actor_id
WHERE e.org_id = $1 AND e.seq > $2
  AND (
    sqlc.arg(is_admin)::bool
    OR (sqlc.arg(all_teams)::bool AND e.scope <> 'admin')
    OR e.scope = 'org'
    OR (e.scope = 'any' AND e.team_ids && sqlc.arg(team_ids)::uuid[])
    OR (e.scope = 'all' AND cardinality(e.team_ids) > 0 AND e.team_ids <@ sqlc.arg(team_ids)::uuid[])
  )
  AND (sqlc.arg(is_admin)::bool OR NOT (e.scope = 'legacy' AND e.type = 'member.access_changed'))
ORDER BY e.seq
LIMIT $3;

-- name: ListEventsAfterSystem :many
-- scope: system
-- The whole feed of an org, for the notification consumer: it has no viewer
-- and picks recipients itself (T20 #55, #61). Never call it from a handler.
SELECT e.seq, e.type, e.entity_type, e.entity_id, e.actor_id, u.username AS actor_username, u.name AS actor_name,
       e.payload, e.created_at
FROM events e
LEFT JOIN users u ON u.id = e.actor_id
WHERE e.org_id = $1 AND e.seq > $2
ORDER BY e.seq
LIMIT $3;

-- name: ListIssueTeamIDsSystem :many
-- scope: system
-- The teams of issues, for the scope of an event about them (T20 #54). It
-- runs in the writing transaction, after the caller resolved the issues.
SELECT DISTINCT team_id FROM issues WHERE org_id = $1 AND id = ANY(sqlc.arg(ids)::uuid[]);

-- name: GetDocumentProjectIDSystem :one
-- scope: system
-- The project of a document (NULL: an org-level document), for the scope of
-- an event about the document or its comments (T20 #54).
SELECT project_id FROM documents WHERE org_id = $1 AND id = $2;

-- name: LatestEventSeq :one
-- scope: org
SELECT COALESCE(MAX(seq), 0)::bigint AS seq FROM events WHERE org_id = $1;

-- name: ListLegacyEvents :many
-- scope: system
-- The events backfill job (T20 #57, ticket 6b, internal/events/backfill.go)
-- places rows made before migration 0038 (or by an older image after a
-- rollback): scope 'legacy'. Its queries (this one to BackfillProductTeams)
-- are system: the job has no caller and reads every org. Each lookup takes
-- the org of the event, so a row of another org never places an event.
-- The next batch of legacy rows after (after_org, after_seq), in primary key
-- order: the job walks events_pkey and never reads a placed row twice in a run.
SELECT org_id, seq, type, entity_type, entity_id, payload
FROM events
WHERE scope = 'legacy'
  AND (org_id, seq) > (sqlc.arg(after_org)::uuid, sqlc.arg(after_seq)::bigint)
ORDER BY org_id, seq
LIMIT sqlc.arg(batch_size);

-- name: PlaceLegacyEvents :execrows
-- scope: system
-- Sets the scope of legacy rows. A row placed meanwhile (scope no longer
-- 'legacy') is not touched, so a second run, or a second instance, changes
-- nothing. The events_scope_teams_check CHECK still holds the team rule.
UPDATE events
SET scope = sqlc.arg(scope), team_ids = sqlc.arg(team_ids)::uuid[]
WHERE org_id = sqlc.arg(org_id) AND seq = ANY(sqlc.arg(seqs)::bigint[]) AND scope = 'legacy';

-- name: BackfillIssueTeams :many
-- scope: system
SELECT id, team_id FROM issues WHERE org_id = $1 AND id = ANY(sqlc.arg(ids)::uuid[]);

-- name: BackfillCommentTargets :many
-- scope: system
SELECT id, issue_id, document_id FROM comments WHERE org_id = $1 AND id = ANY(sqlc.arg(ids)::uuid[]);

-- name: BackfillAttachmentIssues :many
-- scope: system
SELECT id, issue_id FROM attachments WHERE org_id = $1 AND id = ANY(sqlc.arg(ids)::uuid[]);

-- name: BackfillRelationIssues :many
-- scope: system
SELECT id, issue_id, related_issue_id FROM issue_relations WHERE org_id = $1 AND id = ANY(sqlc.arg(ids)::uuid[]);

-- name: BackfillCycleTeams :many
-- scope: system
SELECT id, team_id FROM cycles WHERE org_id = $1 AND id = ANY(sqlc.arg(ids)::uuid[]);

-- name: BackfillTeams :many
-- scope: system
-- The teams of the org that still exist.
SELECT id FROM teams WHERE org_id = $1 AND id = ANY(sqlc.arg(ids)::uuid[]);

-- name: BackfillProjectTeams :many
-- scope: system
-- Each project that still exists, with its teams now (empty: org-level, #36).
SELECT p.id,
       COALESCE(array_agg(pt.team_id ORDER BY pt.team_id) FILTER (WHERE pt.team_id IS NOT NULL), '{}')::uuid[] AS team_ids
FROM projects p
LEFT JOIN project_teams pt ON pt.org_id = p.org_id AND pt.project_id = p.id
WHERE p.org_id = $1 AND p.id = ANY(sqlc.arg(ids)::uuid[])
GROUP BY p.id;

-- name: BackfillMilestoneProjects :many
-- scope: system
SELECT id, project_id FROM milestones WHERE org_id = $1 AND id = ANY(sqlc.arg(ids)::uuid[]);

-- name: BackfillDocumentProjects :many
-- scope: system
-- project_id NULL: an org-level document (#38).
SELECT id, project_id FROM documents WHERE org_id = $1 AND id = ANY(sqlc.arg(ids)::uuid[]);

-- name: BackfillProjectUpdateProjects :many
-- scope: system
SELECT id, project_id FROM project_updates WHERE org_id = $1 AND id = ANY(sqlc.arg(ids)::uuid[]);

-- name: BackfillLabelTeams :many
-- scope: system
-- team_id NULL: an org label (#8).
SELECT id, team_id FROM labels WHERE org_id = $1 AND id = ANY(sqlc.arg(ids)::uuid[]);

-- name: BackfillProductTeams :many
-- scope: system
-- Each product that still exists, with its teams now (empty: org-level, #69).
SELECT p.id,
       COALESCE(array_agg(pt.team_id ORDER BY pt.team_id) FILTER (WHERE pt.team_id IS NOT NULL), '{}')::uuid[] AS team_ids
FROM products p
LEFT JOIN product_teams pt ON pt.org_id = p.org_id AND pt.product_id = p.id
WHERE p.org_id = $1 AND p.id = ANY(sqlc.arg(ids)::uuid[])
GROUP BY p.id;
