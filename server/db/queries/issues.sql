-- Issue READS with joins live in internal/tracker/issue_read.go (hand-written
-- pgx), because the list query is built from optional filters. Writes and
-- the small lookups are here.

-- name: InsertIssue :one
-- scope: resolved
INSERT INTO issues (
    org_id, team_id, number, title, description, state_id, priority, estimate,
    assignee_id, creator_id, parent_id, project_id, milestone_id, cycle_id, due_date,
    started_at, completed_at, canceled_at, sort_order
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19
)
RETURNING *;

-- name: LockIssue :one
-- scope: resolved
SELECT * FROM issues WHERE org_id = $1 AND id = $2 FOR UPDATE;

-- name: UpdateIssue :one
-- scope: resolved
UPDATE issues SET
    title = $3, description = $4, state_id = $5, priority = $6, estimate = $7,
    assignee_id = $8, parent_id = $9, project_id = $10, milestone_id = $11, cycle_id = $12,
    due_date = $13, started_at = $14, completed_at = $15, canceled_at = $16, sort_order = $17,
    version = version + 1, updated_at = now()
WHERE org_id = $1 AND id = $2
RETURNING *;

-- Manual order (docs/internal/decisions-T14-manual-order.md). A column is
-- (org_id, team_id, state_id); issues_manual_order_idx serves all of these.

-- name: ColumnTopRank :one
-- scope: resolved
-- The rank that puts an issue above everything in a column: the column's
-- smallest rank less one step (0 for an empty column).
SELECT (COALESCE(min(sort_order), 1024) - 1024)::float8 AS rank
FROM issues WHERE org_id = $1 AND team_id = $2 AND state_id = $3;

-- name: GetIssueRankRow :one
-- scope: resolved
SELECT id, team_id, state_id, sort_order FROM issues WHERE org_id = $1 AND id = $2;

-- name: ColumnSuccessor :one
-- scope: resolved
-- The issue after (sort_order, id) in a column, leaving one issue out.
SELECT id, sort_order FROM issues
WHERE org_id = $1 AND team_id = $2 AND state_id = $3 AND id <> sqlc.arg(skip_id)
  AND (sort_order, id) > (sqlc.arg(after_rank)::float8, sqlc.arg(after_id)::uuid)
ORDER BY sort_order, id LIMIT 1;

-- name: ColumnPredecessor :one
-- scope: resolved
SELECT id, sort_order FROM issues
WHERE org_id = $1 AND team_id = $2 AND state_id = $3 AND id <> sqlc.arg(skip_id)
  AND (sort_order, id) < (sqlc.arg(before_rank)::float8, sqlc.arg(before_id)::uuid)
ORDER BY sort_order DESC, id DESC LIMIT 1;

-- name: RebalanceColumn :execrows
-- scope: resolved
-- Renumbers a column 1024, 2048, ... in its current order, leaving one issue
-- out (the one being placed). It keeps version and updated_at: a rank is not
-- content, and a version check must not fail because of it.
-- The rows are locked in id order first (never inlined), the same order as
-- RolloverIssues, so two writers of many rows cannot deadlock.
WITH locked AS MATERIALIZED (
    SELECT x.id FROM issues x
    WHERE x.org_id = $1 AND x.team_id = $2 AND x.state_id = $3 AND x.id <> sqlc.arg(skip_id)
    ORDER BY x.id
    FOR UPDATE OF x
), ranked AS (
    SELECT y.id, row_number() OVER (ORDER BY y.sort_order, y.id) AS n
    FROM issues y JOIN locked l ON l.id = y.id
)
UPDATE issues i SET sort_order = r.n * 1024.0
FROM ranked r
WHERE i.org_id = $1 AND i.id = r.id;

-- name: ResolveIssueByNumber :one
-- scope: team
-- T20 #14, #15: team scoped. The predicate is part of the lookup (#11).
SELECT i.id FROM issues i
JOIN teams t ON t.org_id = i.org_id AND t.id = i.team_id
WHERE i.org_id = $1 AND t.key = $2 AND i.number = $3
  AND (sqlc.arg(all_teams)::bool OR i.team_id = ANY(sqlc.arg(team_ids)::uuid[]));

-- name: IssueExists :one
-- scope: team
SELECT i.id FROM issues i WHERE i.org_id = $1 AND i.id = $2
  AND (sqlc.arg(all_teams)::bool OR i.team_id = ANY(sqlc.arg(team_ids)::uuid[]));

-- name: ResolveImportedIssueID :one
-- scope: team
-- The issue a Linear id became (T4), only when its team is visible (T20 #14).
SELECT m.target_id FROM import_mappings m
JOIN issues i ON i.org_id = m.org_id AND i.id = m.target_id
WHERE m.org_id = $1 AND m.source = 'linear' AND m.entity_type = 'issue' AND m.source_id = $2
  AND (sqlc.arg(all_teams)::bool OR i.team_id = ANY(sqlc.arg(team_ids)::uuid[]));

-- name: ResolveImportedTeamID :one
-- scope: team
-- The team a Linear id became, only when it is visible. The mapping row is
-- the lookup: the team join carries the predicate on teams.id.
SELECT m.target_id FROM import_mappings m
JOIN teams t ON t.org_id = m.org_id AND t.id = m.target_id
WHERE m.org_id = $1 AND m.source = 'linear' AND m.entity_type = 'team' AND m.source_id = $2
  AND (sqlc.arg(all_teams)::bool OR t.id = ANY(sqlc.arg(team_ids)::uuid[]));

-- name: ResolveImportedCycleID :one
-- scope: team
-- The cycle a Linear id became, only when its team is visible.
SELECT m.target_id FROM import_mappings m
JOIN cycles c ON c.org_id = m.org_id AND c.id = m.target_id
WHERE m.org_id = $1 AND m.source = 'linear' AND m.entity_type = 'cycle' AND m.source_id = $2
  AND (sqlc.arg(all_teams)::bool OR c.team_id = ANY(sqlc.arg(team_ids)::uuid[]));

-- name: InsertStateChange :exec
-- scope: resolved
INSERT INTO issue_state_changes (org_id, issue_id, from_state_id, to_state_id, actor_id, source)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: ListStateChanges :many
-- scope: resolved
SELECT sc.id, sc.from_state_id, fs.name AS from_state_name, sc.to_state_id, ts.name AS to_state_name,
       sc.actor_id, u.username AS actor_username, u.name AS actor_name, sc.source, sc.created_at
FROM issue_state_changes sc
LEFT JOIN workflow_states fs ON fs.org_id = sc.org_id AND fs.id = sc.from_state_id
JOIN workflow_states ts ON ts.org_id = sc.org_id AND ts.id = sc.to_state_id
LEFT JOIN users u ON u.id = sc.actor_id
WHERE sc.org_id = $1 AND sc.issue_id = $2
ORDER BY sc.created_at, sc.id;

-- name: ListIssueRefs :many
-- scope: team
-- Team scoped (T20 #14): an issue the caller cannot see is not in the result.
SELECT i.id, t.key AS team_key, i.number, i.title, s.name AS state_name, s.type AS state_type
FROM issues i
JOIN teams t ON t.org_id = i.org_id AND t.id = i.team_id
JOIN workflow_states s ON s.org_id = i.org_id AND s.id = i.state_id
WHERE i.org_id = $1 AND i.id = ANY(sqlc.arg(ids)::uuid[])
  AND (sqlc.arg(all_teams)::bool OR i.team_id = ANY(sqlc.arg(team_ids)::uuid[]));

-- name: ListIssueRefsSystem :many
-- scope: system
-- The unscoped copy, for code with no caller (the activity snapshot, which is
-- stored whole and redacted at read time, T20 #43).
SELECT i.id, t.key AS team_key, i.number, i.title, s.name AS state_name, s.type AS state_type
FROM issues i
JOIN teams t ON t.org_id = i.org_id AND t.id = i.team_id
JOIN workflow_states s ON s.org_id = i.org_id AND s.id = i.state_id
WHERE i.org_id = $1 AND i.id = ANY(sqlc.arg(ids)::uuid[]);

-- name: ProjectExists :one
-- scope: project
-- Project scoped (T20 #36).
SELECT p.id FROM projects p WHERE p.org_id = $1 AND p.id = $2
  AND (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id)
    OR EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id
               AND pst.team_id = ANY(sqlc.arg(team_ids)::uuid[])));

-- name: MilestoneExists :one
-- scope: project
-- Project scoped: a milestone follows its project (T20 #38).
SELECT m.id FROM milestones m
JOIN projects p ON p.org_id = m.org_id AND p.id = m.project_id
WHERE m.org_id = $1 AND m.id = $2
  AND (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id)
    OR EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id
               AND pst.team_id = ANY(sqlc.arg(team_ids)::uuid[])));

-- name: CycleExists :one
-- scope: team
-- Team scoped (T20 #14).
SELECT c.id FROM cycles c WHERE c.org_id = $1 AND c.id = $2
  AND (sqlc.arg(all_teams)::bool OR c.team_id = ANY(sqlc.arg(team_ids)::uuid[]));

-- name: DeleteIssue :execrows
-- scope: admin
-- LAUN-201 (decisions-T32): a hard delete. Comments, attachments, relations,
-- activity, label links, state changes, subscriptions, MR links and
-- notifications go by their (org_id, issue_id) cascade; child issues keep their
-- row with parent_id NULL (issues_parent_fk, SET NULL).
DELETE FROM issues WHERE org_id = $1 AND id = $2;

-- name: ListIssueObjectKeys :many
-- scope: admin
-- The object keys of the file attachments of an issue, read before the delete.
SELECT object_key FROM attachments
WHERE org_id = $1 AND issue_id = $2 AND object_key IS NOT NULL;

-- name: DeleteIssueImportMapping :exec
-- scope: admin
-- A mapping from a Linear id to a deleted issue would resolve to a missing row.
DELETE FROM import_mappings
WHERE org_id = $1 AND entity_type = 'issue' AND target_id = $2;

-- name: RedactIssueSnapshots :execrows
-- scope: admin
-- LAUN-201: other issues' activity rows hold a snapshot {id, identifier, title}
-- of the deleted issue (kind parent in from/to; kind relationRemoved in
-- from.issue). Drop the title; keep id and identifier so the line still reads.
UPDATE issue_activity SET
    "from" = CASE
        WHEN kind = 'parent' AND "from"->>'id' = sqlc.arg(id)::text THEN "from" - 'title'
        WHEN kind = 'relationRemoved' AND "from"->'issue'->>'id' = sqlc.arg(id)::text THEN "from" #- '{issue,title}'
        ELSE "from" END,
    "to" = CASE
        WHEN kind = 'parent' AND "to"->>'id' = sqlc.arg(id)::text THEN "to" - 'title'
        ELSE "to" END
WHERE org_id = sqlc.arg(org_id)
  AND issue_id <> sqlc.arg(id)::uuid
  AND ((kind = 'parent' AND ("from"->>'id' = sqlc.arg(id)::text OR "to"->>'id' = sqlc.arg(id)::text))
    OR (kind = 'relationRemoved' AND "from"->'issue'->>'id' = sqlc.arg(id)::text));
