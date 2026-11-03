-- LAUN-200 (T31): agent guidance. Every query filters by org_id.

-- name: ListAgentGuidance :many
-- scope: team
-- The org-wide row first, then the team rows by team key. A hidden team's
-- row is not listed (T20 #9); the org-wide row is for every member.
SELECT g.id, g.org_id, g.team_id, t.key AS team_key, g.body, g.updated_at
FROM agent_guidance g
LEFT JOIN teams t ON t.org_id = g.org_id AND t.id = g.team_id
WHERE g.org_id = sqlc.arg(org_id)
  AND (NOT sqlc.arg(only_team)::bool OR g.team_id IS NULL OR g.team_id = sqlc.narg(team_id)::uuid)
  AND (g.team_id IS NULL OR (sqlc.arg(all_teams)::bool OR g.team_id = ANY(sqlc.arg(team_ids)::uuid[])))
ORDER BY (g.team_id IS NOT NULL), t.key;

-- name: GetOrgAgentGuidance :one
-- scope: org
SELECT * FROM agent_guidance WHERE org_id = $1 AND team_id IS NULL FOR UPDATE;

-- name: GetTeamAgentGuidance :one
-- scope: admin
SELECT * FROM agent_guidance WHERE org_id = $1 AND team_id = $2 FOR UPDATE;

-- name: UpsertOrgAgentGuidance :one
-- scope: admin
INSERT INTO agent_guidance (org_id, body) VALUES ($1, $2)
ON CONFLICT (org_id) WHERE team_id IS NULL DO UPDATE SET body = EXCLUDED.body, updated_at = now()
RETURNING *;

-- name: UpsertTeamAgentGuidance :one
-- scope: admin
INSERT INTO agent_guidance (org_id, team_id, body) VALUES ($1, $2, $3)
ON CONFLICT (org_id, team_id) DO UPDATE SET body = EXCLUDED.body, updated_at = now()
RETURNING *;

-- name: DeleteOrgAgentGuidance :execrows
-- scope: admin
DELETE FROM agent_guidance WHERE org_id = $1 AND team_id IS NULL;

-- name: DeleteTeamAgentGuidance :execrows
-- scope: admin
DELETE FROM agent_guidance WHERE org_id = $1 AND team_id = $2;
