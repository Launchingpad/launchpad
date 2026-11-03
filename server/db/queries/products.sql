-- LAUN-62 (T18): products and product_teams. Every query filters by org_id.

-- name: LockProductsOrg :exec
-- scope: none
-- Serialises product creates in one org (the 200 cap, decisions-T18 #9).
SELECT pg_advisory_xact_lock(hashtextextended('products:' || sqlc.arg(org_id)::text, 0));

-- name: CountProducts :one
-- scope: admin
SELECT count(*) FROM products WHERE org_id = $1;

-- name: InsertProduct :one
-- scope: admin
INSERT INTO products (org_id, slug, name, description, icon, color)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetProductByID :one
-- scope: project
-- The project rule (T20 #36, #69): a product is visible when the caller sees
-- one of its teams (archived ones count), or it has none.
SELECT p.* FROM products p WHERE p.org_id = $1 AND p.id = $2
  AND (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM product_teams ppt WHERE ppt.org_id = p.org_id AND ppt.product_id = p.id)
    OR EXISTS (SELECT 1 FROM product_teams ppt WHERE ppt.org_id = p.org_id AND ppt.product_id = p.id
               AND ppt.team_id = ANY(sqlc.arg(team_ids)::uuid[])));

-- name: GetProductBySlug :one
-- scope: project
-- The project rule (T20 #69), as GetProductByID.
SELECT p.* FROM products p WHERE p.org_id = $1 AND p.slug = $2
  AND (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM product_teams ppt WHERE ppt.org_id = p.org_id AND ppt.product_id = p.id)
    OR EXISTS (SELECT 1 FROM product_teams ppt WHERE ppt.org_id = p.org_id AND ppt.product_id = p.id
               AND ppt.team_id = ANY(sqlc.arg(team_ids)::uuid[])));

-- name: LockProduct :one
-- scope: admin
SELECT * FROM products WHERE org_id = $1 AND id = $2 FOR UPDATE;

-- name: ListProducts :many
-- scope: project
-- The project rule (T20 #69): a product whose teams are all hidden is not listed.
SELECT p.* FROM products p
WHERE p.org_id = $1 AND (sqlc.arg(include_archived)::bool OR p.archived_at IS NULL)
  AND (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM product_teams ppt WHERE ppt.org_id = p.org_id AND ppt.product_id = p.id)
    OR EXISTS (SELECT 1 FROM product_teams ppt WHERE ppt.org_id = p.org_id AND ppt.product_id = p.id
               AND ppt.team_id = ANY(sqlc.arg(team_ids)::uuid[])))
ORDER BY lower(p.name), p.id;

-- name: UpdateProduct :one
-- scope: admin
UPDATE products
SET slug = $3, name = $4, description = $5, icon = $6, color = $7, archived_at = $8,
    version = version + 1, updated_at = now()
WHERE org_id = $1 AND id = $2
RETURNING *;

-- name: DeleteProduct :exec
-- scope: admin
DELETE FROM products WHERE org_id = $1 AND id = $2;

-- name: ListTeamsForProducts :many
-- scope: team
-- The NON-archived teams of products, by key. Archived teams keep their
-- join row but never show (decisions-T18 #9). Hidden teams never show (T20 #69).
SELECT pt.product_id, t.id, t.key, t.name
FROM product_teams pt
JOIN teams t ON t.org_id = pt.org_id AND t.id = pt.team_id
WHERE pt.org_id = $1 AND pt.product_id = ANY(sqlc.arg(product_ids)::uuid[]) AND t.archived_at IS NULL
  AND (sqlc.arg(all_teams)::bool OR pt.team_id = ANY(sqlc.arg(team_ids)::uuid[]))
ORDER BY t.key;

-- name: ListProductTeams :many
-- scope: team
-- The non-archived teams of one product, by key (GET /products/{p}). Hidden
-- teams are not listed (T20 #69).
SELECT t.* FROM teams t
JOIN product_teams pt ON pt.org_id = t.org_id AND pt.team_id = t.id
WHERE pt.org_id = $1 AND pt.product_id = $2 AND t.archived_at IS NULL
  AND (sqlc.arg(all_teams)::bool OR t.id = ANY(sqlc.arg(team_ids)::uuid[]))
ORDER BY t.key;

-- name: ListProductTeamLinks :many
-- scope: admin
-- Every join row of one product, with whether the team is archived.
SELECT pt.team_id, (t.archived_at IS NOT NULL)::bool AS team_archived
FROM product_teams pt
JOIN teams t ON t.org_id = pt.org_id AND t.id = pt.team_id
WHERE pt.org_id = $1 AND pt.product_id = $2
ORDER BY t.key;

-- name: InsertProductTeam :execrows
-- scope: admin
INSERT INTO product_teams (org_id, product_id, team_id) VALUES ($1, $2, $3)
ON CONFLICT (product_id, team_id) DO NOTHING;

-- name: DeleteProductTeam :execrows
-- scope: admin
DELETE FROM product_teams WHERE org_id = $1 AND product_id = $2 AND team_id = $3;

-- name: ListProductIDsForTeams :many
-- scope: resolved
-- Non-archived products of teams: one query for a whole GET /teams. The teams
-- come from a scoped list, so each product has a visible team (T20 #69).
SELECT pt.team_id, pt.product_id
FROM product_teams pt
JOIN products p ON p.org_id = pt.org_id AND p.id = pt.product_id
WHERE pt.org_id = $1 AND pt.team_id = ANY(sqlc.arg(team_ids)::uuid[]) AND p.archived_at IS NULL
ORDER BY lower(p.name), p.id;

-- name: ListProductsForTeam :many
-- scope: resolved
-- The team is resolved scoped first, so each product has a visible team (T20 #69).
SELECT p.* FROM products p
JOIN product_teams pt ON pt.org_id = p.org_id AND pt.product_id = p.id
WHERE p.org_id = $1 AND pt.team_id = $2 AND p.archived_at IS NULL
ORDER BY lower(p.name), p.id;
