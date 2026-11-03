-- name: InsertIssueActivity :exec
-- scope: resolved
INSERT INTO issue_activity (org_id, issue_id, actor_id, source, kind, "from", "to")
VALUES ($1, $2, $3, $4, $5, sqlc.narg('from_value'), sqlc.narg('to_value'));

-- name: ListIssueActivity :many
-- scope: resolved
-- Oldest first, keyset on (created_at, id). after_at NULL is the first page.
SELECT a.id, a.actor_id, u.username AS actor_username, u.name AS actor_name,
       a.source, a.kind, a."from" AS from_value, a."to" AS to_value, a.created_at
FROM issue_activity a
LEFT JOIN users u ON u.id = a.actor_id
WHERE a.org_id = $1 AND a.issue_id = $2
  AND (sqlc.narg('after_at')::timestamptz IS NULL OR (a.created_at, a.id) > (sqlc.narg('after_at')::timestamptz, sqlc.narg('after_id')::uuid))
ORDER BY a.created_at, a.id
LIMIT sqlc.arg('max_rows');

-- name: GetLabelsByIDs :many
-- scope: resolved
-- Archived labels too: a removal record may name one.
SELECT id, name FROM labels WHERE org_id = $1 AND id = ANY(sqlc.arg(ids)::uuid[]);

-- The read-time redaction of activity snapshots (T20 #43): of the ids a page
-- names, the ones the caller sees. A snapshot whose id is not returned is
-- shown as null to a restricted caller; the stored row does not change.

-- name: ListVisibleProjectIDs :many
-- scope: project
SELECT p.id FROM projects p
WHERE p.org_id = $1 AND p.id = ANY(sqlc.arg(ids)::uuid[])
  AND (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id)
    OR EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id
               AND pst.team_id = ANY(sqlc.arg(team_ids)::uuid[])));

-- name: ListVisibleMilestoneIDs :many
-- scope: project
-- A milestone follows its project (T20 #38).
SELECT m.id FROM milestones m
JOIN projects p ON p.org_id = m.org_id AND p.id = m.project_id
WHERE m.org_id = $1 AND m.id = ANY(sqlc.arg(ids)::uuid[])
  AND (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id)
    OR EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id
               AND pst.team_id = ANY(sqlc.arg(team_ids)::uuid[])));

-- name: ListVisibleCycleIDs :many
-- scope: team
SELECT c.id FROM cycles c
WHERE c.org_id = $1 AND c.id = ANY(sqlc.arg(ids)::uuid[])
  AND (sqlc.arg(all_teams)::bool OR c.team_id = ANY(sqlc.arg(team_ids)::uuid[]));
