-- The team access setter (decisions-T20 #48 to #50, ticket 9) and the access
-- fields of the member list for admins (#33). Every query here runs on an
-- org admin route (PUT /users/{user}/team-access, POST /agents, GET /users
-- for an admin caller): admins see every team (#2).

-- name: GetMemberForAccess :one
-- scope: admin
-- The target of the setter, by id or username, locked for the transaction.
-- Another org's user is no row (404).
SELECT m.user_id, m.role, m.team_access, u.username, u.is_agent
FROM org_members m
JOIN users u ON u.id = m.user_id
WHERE m.org_id = sqlc.arg('org_id')
  AND (u.id = sqlc.narg('user_id')::uuid OR u.username = sqlc.narg('username')::text)
FOR UPDATE OF m;

-- name: ListOrgTeamRefs :many
-- scope: admin
-- Every team of the org, archived ones included (an archived team still
-- counts in team_members, #6).
SELECT id, key FROM teams WHERE org_id = $1 ORDER BY key;

-- name: SetMemberTeamAccess :exec
-- scope: admin
UPDATE org_members SET team_access = sqlc.arg('team_access')
WHERE org_id = sqlc.arg('org_id') AND user_id = sqlc.arg('user_id');

-- name: DeleteMemberTeamsExcept :execrows
-- scope: admin
DELETE FROM team_members
WHERE org_id = sqlc.arg('org_id') AND user_id = sqlc.arg('user_id')
  AND NOT (team_id = ANY(sqlc.arg('keep_team_ids')::uuid[]));

-- name: InsertMemberTeams :execrows
-- scope: admin
INSERT INTO team_members (org_id, user_id, team_id)
SELECT sqlc.arg('org_id'), sqlc.arg('user_id'), t
FROM unnest(sqlc.arg('new_team_ids')::uuid[]) AS t
ON CONFLICT DO NOTHING;

-- name: CountOpenAssignedIssuesByTeam :many
-- scope: admin
-- Open issues (state type not completed or canceled) assigned to the member,
-- per team (#46, #49).
SELECT i.team_id, count(*)::bigint AS open_issues
FROM issues i
JOIN workflow_states s ON s.org_id = i.org_id AND s.id = i.state_id
WHERE i.org_id = sqlc.arg('org_id') AND i.assignee_id = sqlc.arg('user_id')::uuid
  AND s.type NOT IN ('completed', 'canceled')
GROUP BY i.team_id
ORDER BY i.team_id;

-- name: UnassignOpenIssuesInTeams :many
-- scope: admin
-- Clears the assignee of the member's open issues in the lost teams (#49).
-- The rows are locked in id order first (as RolloverIssues), so this cannot
-- deadlock with a rebalance. Closed issues are never touched.
WITH locked AS MATERIALIZED (
    SELECT x.id FROM issues x
    JOIN workflow_states s ON s.org_id = x.org_id AND s.id = x.state_id
    WHERE x.org_id = sqlc.arg('org_id') AND x.assignee_id = sqlc.arg('user_id')::uuid
      AND x.team_id = ANY(sqlc.arg('lost_team_ids')::uuid[])
      AND s.type NOT IN ('completed', 'canceled')
    ORDER BY x.id
    FOR UPDATE OF x
)
UPDATE issues i
SET assignee_id = NULL, version = i.version + 1, updated_at = now()
FROM locked
WHERE i.org_id = sqlc.arg('org_id') AND i.id = locked.id
RETURNING i.id, i.number, i.team_id, i.version;

-- name: ListOrgMemberTeamKeys :many
-- scope: admin
-- The team keys of every member's team_members rows, for an admin's member
-- list (#33).
SELECT tm.user_id, t.key
FROM team_members tm
JOIN teams t ON t.org_id = tm.org_id AND t.id = tm.team_id
WHERE tm.org_id = $1
ORDER BY t.key;
