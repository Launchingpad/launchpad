-- name: ListIssueComments :many
-- scope: resolved
SELECT c.id, c.issue_id, c.parent_id, c.body, c.created_at, c.updated_at, c.edited_at,
       c.author_id, u.username AS author_username, u.name AS author_name
FROM comments c
LEFT JOIN users u ON u.id = c.author_id
WHERE c.org_id = $1 AND c.issue_id = $2
ORDER BY c.created_at, c.id;

-- name: InsertComment :one
-- scope: resolved
INSERT INTO comments (org_id, issue_id, parent_id, author_id, body)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetComment :one
-- scope: team
-- Scoped by the comment's issue (team) or document (project), T20 #14.
SELECT c.* FROM comments c
LEFT JOIN issues i ON i.org_id = c.org_id AND i.id = c.issue_id
LEFT JOIN documents d ON d.org_id = c.org_id AND d.id = c.document_id
LEFT JOIN projects p ON p.org_id = d.org_id AND p.id = d.project_id
WHERE c.org_id = $1 AND c.id = $2
  AND ((c.issue_id IS NOT NULL AND (sqlc.arg(all_teams)::bool OR i.team_id = ANY(sqlc.arg(team_ids)::uuid[])))
    OR (c.document_id IS NOT NULL AND (d.project_id IS NULL OR (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id)
    OR EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id
               AND pst.team_id = ANY(sqlc.arg(team_ids)::uuid[]))))));

-- name: GetCommentSystem :one
-- scope: system
-- The unscoped copy, for code with no caller (the notification consumer and sender).
SELECT * FROM comments WHERE org_id = $1 AND id = $2;

-- name: GetCommentForUpdate :one
-- scope: team
SELECT c.* FROM comments c
LEFT JOIN issues i ON i.org_id = c.org_id AND i.id = c.issue_id
LEFT JOIN documents d ON d.org_id = c.org_id AND d.id = c.document_id
LEFT JOIN projects p ON p.org_id = d.org_id AND p.id = d.project_id
WHERE c.org_id = $1 AND c.id = $2
  AND ((c.issue_id IS NOT NULL AND (sqlc.arg(all_teams)::bool OR i.team_id = ANY(sqlc.arg(team_ids)::uuid[])))
    OR (c.document_id IS NOT NULL AND (d.project_id IS NULL OR (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id)
    OR EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id
               AND pst.team_id = ANY(sqlc.arg(team_ids)::uuid[]))))))
FOR UPDATE OF c;

-- name: UpdateCommentBody :one
-- scope: resolved
UPDATE comments SET body = $3, edited_at = now(), updated_at = now()
WHERE org_id = $1 AND id = $2
RETURNING *;

-- name: DeleteComment :exec
-- scope: resolved
DELETE FROM comments WHERE org_id = $1 AND id = $2;
