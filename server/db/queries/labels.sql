-- name: ListLabelsWithCounts :many
-- scope: team (decisions-T20 #8; decisions-T21-team-labels #8).
-- The labels of an org with the number of NON-ARCHIVED issues that carry each
-- (closed issues count: decisions-T21-team-labels #5).
--   team_id          the ?team= filter: org labels plus that team's labels, and
--                    only that team's issues are counted. NULL: every label.
--   all_teams, team_ids  the caller's access (T20 #15): all_teams true sees every
--                    team; otherwise team labels of other teams are not listed and
--                    their issues are not counted. A nil team_ids sees NO team.
SELECT l.id, l.org_id, l.name, l.color, l.description, l.parent_id, l.created_at, l.archived_at, l.team_id,
       t.key AS team_key, COALESCE(c.n, 0)::int AS issue_count
FROM labels l
LEFT JOIN teams t ON t.org_id = l.org_id AND t.id = l.team_id
LEFT JOIN (
    SELECT il.label_id, count(*) AS n
    FROM issue_labels il
    JOIN issues i ON i.org_id = il.org_id AND i.id = il.issue_id
    WHERE il.org_id = sqlc.arg(org_id) AND i.archived_at IS NULL
      AND (sqlc.narg(team_id)::uuid IS NULL OR i.team_id = sqlc.narg(team_id)::uuid)
      AND (sqlc.arg(all_teams)::bool OR i.team_id = ANY(sqlc.arg(team_ids)::uuid[]))
    GROUP BY il.label_id
) c ON c.label_id = l.id
WHERE l.org_id = sqlc.arg(org_id) AND l.archived_at IS NULL
  AND (l.team_id IS NULL
       OR ((sqlc.narg(team_id)::uuid IS NULL OR l.team_id = sqlc.narg(team_id)::uuid)
           AND (sqlc.arg(all_teams)::bool OR l.team_id = ANY(sqlc.arg(team_ids)::uuid[]))))
ORDER BY lower(l.name), l.team_id NULLS FIRST, l.id;

-- name: InsertLabel :one
-- scope: resolved
INSERT INTO labels (org_id, name, color, description, team_id) VALUES ($1, $2, $3, $4, sqlc.narg(team_id))
RETURNING *;

-- name: InsertLabelIfAbsent :exec
-- scope: system
INSERT INTO labels (org_id, name, color) VALUES ($1, $2, $3)
ON CONFLICT (org_id, lower(name)) WHERE team_id IS NULL DO NOTHING;

-- name: FindLabels :many
-- scope: resolved
-- Resolves label references given either as ids or as names, for an issue of
-- team_id: an org label or a label of THAT team (another team's label is not
-- found). By name, the service prefers the team's label over an org label.
SELECT * FROM labels
WHERE org_id = sqlc.arg(org_id) AND archived_at IS NULL
  AND (team_id IS NULL OR team_id = sqlc.arg(team_id))
  AND (id = ANY(sqlc.arg(ids)::uuid[]) OR lower(name) = ANY(sqlc.arg(names)::text[]));

-- name: ListLabelsForIssues :many
-- scope: resolved
SELECT il.issue_id, l.id, l.name, l.color
FROM issue_labels il
JOIN labels l ON l.org_id = il.org_id AND l.id = il.label_id
WHERE il.org_id = $1 AND il.issue_id = ANY(sqlc.arg(issue_ids)::uuid[])
ORDER BY il.issue_id, lower(l.name);

-- name: ListIssueLabelIDs :many
-- scope: resolved
SELECT label_id FROM issue_labels WHERE org_id = $1 AND issue_id = $2;

-- name: InsertIssueLabel :exec
-- scope: resolved
INSERT INTO issue_labels (org_id, issue_id, label_id) VALUES ($1, $2, $3)
ON CONFLICT (issue_id, label_id) DO NOTHING;

-- name: DeleteIssueLabel :exec
-- scope: resolved
DELETE FROM issue_labels WHERE org_id = $1 AND issue_id = $2 AND label_id = $3;
