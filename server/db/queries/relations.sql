-- name: InsertRelation :one
-- scope: resolved
INSERT INTO issue_relations (org_id, type, issue_id, related_issue_id, created_by)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: RelationExists :one
-- scope: resolved
SELECT EXISTS (
    SELECT 1 FROM issue_relations
    WHERE org_id = $1 AND issue_id = $2 AND related_issue_id = $3 AND type = $4
) AS found;

-- name: ListRelationsForIssue :many
-- scope: team
-- Only edges whose other issue the caller sees (T20 #41): no placeholder,
-- no count.
SELECT r.* FROM issue_relations r
JOIN issues o ON o.org_id = r.org_id
 AND o.id = CASE WHEN r.issue_id = sqlc.arg(issue_id) THEN r.related_issue_id ELSE r.issue_id END
WHERE r.org_id = $1 AND (r.issue_id = sqlc.arg(issue_id) OR r.related_issue_id = sqlc.arg(issue_id))
  AND (sqlc.arg(all_teams)::bool OR o.team_id = ANY(sqlc.arg(team_ids)::uuid[]))
ORDER BY r.created_at, r.id;

-- name: DeleteRelation :one
-- scope: team
-- Deletes only a relation that touches the issue in the path (resolved) and
-- whose other issue the caller sees (T20 #41): else it is not found.
DELETE FROM issue_relations r
WHERE r.org_id = $1 AND r.id = $2 AND (r.issue_id = sqlc.arg(issue_id) OR r.related_issue_id = sqlc.arg(issue_id))
  AND EXISTS (SELECT 1 FROM issues o WHERE o.org_id = r.org_id
    AND o.id = CASE WHEN r.issue_id = sqlc.arg(issue_id) THEN r.related_issue_id ELSE r.issue_id END
    AND (sqlc.arg(all_teams)::bool OR o.team_id = ANY(sqlc.arg(team_ids)::uuid[])))
RETURNING r.*;
