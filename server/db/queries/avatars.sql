-- name: GetOrgAvatar :one
-- scope: none
SELECT avatar_key, avatar_content_type, avatar_version FROM orgs WHERE id = $1;

-- name: LockOrgAvatar :one
-- scope: none
SELECT avatar_key, avatar_content_type, avatar_version FROM orgs WHERE id = $1 FOR UPDATE;

-- name: SetOrgAvatar :exec
-- scope: none
UPDATE orgs SET avatar_key = $2, avatar_content_type = $3, avatar_version = avatar_version + 1, updated_at = now()
WHERE id = $1;

-- name: ClearOrgAvatar :exec
-- scope: none
UPDATE orgs SET avatar_key = NULL, avatar_content_type = NULL, avatar_version = avatar_version + 1, updated_at = now()
WHERE id = $1;

-- name: GetProjectAvatar :one
-- scope: project
-- T20 #36: project scoped; a hidden project is not found.
SELECT p.avatar_key, p.avatar_content_type, p.avatar_version FROM projects p WHERE p.org_id = $1 AND p.id = $2
  AND (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id)
    OR EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id
               AND pst.team_id = ANY(sqlc.arg(team_ids)::uuid[])));

-- name: LockProjectAvatar :one
-- scope: project
SELECT p.avatar_key, p.avatar_content_type, p.avatar_version FROM projects p WHERE p.org_id = $1 AND p.id = $2
  AND (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id)
    OR EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id
               AND pst.team_id = ANY(sqlc.arg(team_ids)::uuid[])))
FOR UPDATE OF p;

-- name: SetProjectAvatar :exec
-- scope: project
UPDATE projects p SET avatar_key = $3, avatar_content_type = $4, avatar_version = p.avatar_version + 1, updated_at = now()
WHERE p.org_id = $1 AND p.id = $2
  AND (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id)
    OR EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id
               AND pst.team_id = ANY(sqlc.arg(team_ids)::uuid[])));

-- name: ClearProjectAvatar :exec
-- scope: project
UPDATE projects p SET avatar_key = NULL, avatar_content_type = NULL, avatar_version = p.avatar_version + 1, updated_at = now()
WHERE p.org_id = $1 AND p.id = $2
  AND (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id)
    OR EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id
               AND pst.team_id = ANY(sqlc.arg(team_ids)::uuid[])));
