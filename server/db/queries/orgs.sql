-- name: GetOrgBySlug :one
-- scope: none
SELECT * FROM orgs WHERE slug = $1;

-- name: ListOrgsForUser :many
-- scope: own
-- The caller's orgs. Each org's default team key is named only when the
-- caller sees that team in that org (T20 #9): the access of each membership
-- is read here, as auth.NewTeamAccess builds it (an admin or the flag 'all'
-- sees every team; anything else only its team_members rows: fail closed).
SELECT o.id, o.slug, o.name, o.created_at, o.time_zone, t.key AS default_team_key, m.role,
       o.avatar_version, (o.avatar_key IS NOT NULL)::boolean AS has_avatar
FROM orgs o
JOIN org_members m ON m.org_id = o.id
LEFT JOIN teams t ON t.org_id = o.id AND t.id = o.default_team_id
  AND (m.role = 'admin' OR m.team_access = 'all'
       OR EXISTS (SELECT 1 FROM team_members tm
                  WHERE tm.org_id = m.org_id AND tm.user_id = m.user_id AND tm.team_id = t.id))
WHERE m.user_id = $1
ORDER BY o.slug;

-- name: InsertOrg :one
-- scope: none
INSERT INTO orgs (slug, name) VALUES ($1, $2)
ON CONFLICT (slug) DO NOTHING
RETURNING *;

-- name: GetMembership :one
-- scope: org
SELECT role, team_access FROM org_members WHERE org_id = $1 AND user_id = $2;

-- name: ListMemberTeamIDs :many
-- scope: own
-- The teams of a member whose team_access is 'selected' (T20 #13). Sorted.
SELECT team_id FROM team_members WHERE org_id = $1 AND user_id = $2 ORDER BY team_id;

-- name: InsertOrgMember :exec
-- scope: system
INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, $3)
ON CONFLICT (org_id, user_id) DO NOTHING;

-- name: ListOrgMembers :many
-- scope: org
SELECT u.id, u.username, u.name, u.email, u.is_agent, u.created_at, u.deactivated_at, m.role, m.team_access
FROM org_members m
JOIN users u ON u.id = m.user_id
WHERE m.org_id = $1
ORDER BY u.username;

-- name: GetOrgMemberByUsername :one
-- scope: org
SELECT u.id, u.username, u.name, u.is_agent, m.role
FROM org_members m
JOIN users u ON u.id = m.user_id
WHERE m.org_id = $1 AND u.username = $2 AND u.deactivated_at IS NULL;

-- name: GetUserByUsername :one
-- scope: none
SELECT * FROM users WHERE username = $1;

-- name: InsertUser :one
-- scope: none
INSERT INTO users (username, name, email, is_agent) VALUES ($1, $2, $3, $4)
ON CONFLICT (username) DO NOTHING
RETURNING *;

-- name: GetOrgFull :one
-- scope: team
-- The org with its default team's key (LAUN-56). The key is null when the
-- caller cannot see the default team (T20 #9).
SELECT o.id, o.slug, o.name, o.created_at, o.time_zone, t.key AS default_team_key,
       o.avatar_version, (o.avatar_key IS NOT NULL)::boolean AS has_avatar
FROM orgs o
LEFT JOIN teams t ON t.org_id = o.id AND t.id = o.default_team_id
  AND (sqlc.arg(all_teams)::bool OR t.id = ANY(sqlc.arg(team_ids)::uuid[]))
WHERE o.id = sqlc.arg(id);

-- name: LockOrg :one
-- scope: none
SELECT * FROM orgs WHERE id = $1 FOR UPDATE;

-- name: UpdateOrg :exec
-- scope: none
UPDATE orgs SET name = $2, time_zone = $3, default_team_id = $4, updated_at = now() WHERE id = $1;
