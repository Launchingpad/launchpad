-- LAUN-195 (decisions-T29): project statuses. Every query takes org_id.

-- name: ListProjectStatuses :many
-- scope: org
-- Kind order (the order a project moves through the kinds), then the org's order inside the kind.
SELECT * FROM project_statuses WHERE org_id = $1
ORDER BY array_position(ARRAY['backlog','planned','started','paused','completed','canceled'], kind), position, id;

-- name: GetProjectStatus :one
-- scope: org
SELECT * FROM project_statuses WHERE org_id = $1 AND id = $2;

-- name: GetProjectStatusByName :one
-- scope: org
SELECT * FROM project_statuses WHERE org_id = $1 AND lower(name) = lower(sqlc.arg(name)::text);

-- name: FirstProjectStatusOfKind :one
-- scope: org
SELECT * FROM project_statuses WHERE org_id = $1 AND kind = $2 ORDER BY position, id LIMIT 1;

-- name: LockProjectStatuses :many
-- scope: admin
-- One lock for every status write of the org: the kind minimum and the order need a stable set.
SELECT id FROM project_statuses WHERE org_id = $1 ORDER BY id FOR UPDATE;

-- name: InsertProjectStatus :one
-- scope: admin
INSERT INTO project_statuses (org_id, kind, name, color, position)
VALUES ($1, $2, $3, $4,
        COALESCE((SELECT max(position) + 1 FROM project_statuses WHERE org_id = $1), 0))
RETURNING *;

-- name: UpdateProjectStatus :one
-- scope: admin
UPDATE project_statuses SET name = $3, color = $4, updated_at = now()
WHERE org_id = $1 AND id = $2
RETURNING *;

-- name: SetProjectStatusPosition :exec
-- scope: admin
UPDATE project_statuses SET position = $3, updated_at = now() WHERE org_id = $1 AND id = $2 AND position <> $3;

-- name: DeleteProjectStatus :exec
-- scope: admin
DELETE FROM project_statuses WHERE org_id = $1 AND id = $2;

-- name: CountProjectsWithStatus :one
-- scope: admin
SELECT count(*) FROM projects WHERE org_id = $1 AND status_id = $2;

-- name: CountProjectStatusesOfKind :one
-- scope: admin
SELECT count(*) FROM project_statuses WHERE org_id = $1 AND kind = $2;

-- name: CountProjectStatuses :one
-- scope: admin
SELECT count(*) FROM project_statuses WHERE org_id = $1;

-- name: ShareLockProjectStatus :one
-- scope: org
-- A project write that names a status holds a KEY SHARE lock on it, so a delete (which locks the set FOR UPDATE
-- and then counts) cannot miss the write.
SELECT * FROM project_statuses WHERE org_id = $1 AND id = $2 FOR KEY SHARE;
