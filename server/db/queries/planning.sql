-- T3: projects, milestones, documents, cycles and snapshots.
-- Every query takes org_id. The list endpoints are hand-written pgx in
-- internal/tracker (optional filters), as the issue list is.

-- ---------------------------------------------------------------------------
-- Projects
-- ---------------------------------------------------------------------------

-- name: InsertProject :one
-- scope: resolved
INSERT INTO projects (org_id, name, description, content, state, status_id, lead_id, start_date, target_date,
                      started_at, completed_at, canceled_at, emoji)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
RETURNING *;

-- name: GetProject :one
-- scope: project
-- Project scoped (T20 #36).
SELECT p.* FROM projects p WHERE p.org_id = $1 AND p.id = $2
  AND (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id)
    OR EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id
               AND pst.team_id = ANY(sqlc.arg(team_ids)::uuid[])));

-- name: GetProjectSystem :one
-- scope: system
-- The unscoped copy, for code with no caller (activity snapshots, T20 #43).
SELECT * FROM projects WHERE org_id = $1 AND id = $2;

-- name: LockProject :one
-- scope: project
-- Project scoped (T20 #36).
SELECT p.* FROM projects p WHERE p.org_id = $1 AND p.id = $2
  AND (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id)
    OR EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id
               AND pst.team_id = ANY(sqlc.arg(team_ids)::uuid[])))
FOR UPDATE OF p;

-- name: UpdateProject :one
-- scope: resolved
UPDATE projects
SET name = $3, description = $4, content = $5, state = $6, status_id = $13, lead_id = $7, start_date = $8,
    target_date = $9, started_at = $10, completed_at = $11, canceled_at = $12, emoji = $14,
    version = version + 1, updated_at = now()
WHERE org_id = $1 AND id = $2
RETURNING *;

-- name: TouchProject :one
-- scope: resolved
-- A change to the team set alone still bumps version and updated_at.
UPDATE projects SET version = version + 1, updated_at = now()
WHERE org_id = $1 AND id = $2
RETURNING *;

-- name: ListTeamsForProjects :many
-- scope: team
-- The teams of projects the caller sees: a hidden team is not listed (T20 #37).
SELECT pt.project_id, t.id, t.key, t.name
FROM project_teams pt
JOIN teams t ON t.org_id = pt.org_id AND t.id = pt.team_id
WHERE pt.org_id = $1 AND pt.project_id = ANY(sqlc.arg(project_ids)::uuid[])
  AND (sqlc.arg(all_teams)::bool OR pt.team_id = ANY(sqlc.arg(team_ids)::uuid[]))
ORDER BY t.key;

-- name: ListProjectTeamRows :many
-- scope: resolved
-- Every team row of one project, hidden ones too: the teams replace keeps the
-- rows of teams the caller cannot see (T20 #39). The project is resolved first.
SELECT team_id FROM project_teams
WHERE org_id = $1 AND project_id = $2
ORDER BY team_id;

-- name: ProjectIssueCounts :many
-- scope: team
-- Issue counts per project by state type (LAUN-170). Archived issues are out, as in the issue list;
-- sub-issues count. One grouped read for a whole page of projects. Only issues of
-- teams the caller sees count (T20 #37).
SELECT i.project_id,
       count(*)::int AS total,
       (count(*) FILTER (WHERE s.type = 'completed'))::int AS done,
       (count(*) FILTER (WHERE s.type = 'started'))::int AS started,
       (count(*) FILTER (WHERE s.type = 'canceled'))::int AS canceled
FROM issues i
JOIN workflow_states s ON s.org_id = i.org_id AND s.id = i.state_id
WHERE i.org_id = $1 AND i.archived_at IS NULL AND i.project_id = ANY(sqlc.arg(project_ids)::uuid[])
  AND (sqlc.arg(all_teams)::bool OR i.team_id = ANY(sqlc.arg(team_ids)::uuid[]))
GROUP BY i.project_id;

-- name: InsertProjectTeam :exec
-- scope: resolved
INSERT INTO project_teams (org_id, project_id, team_id) VALUES ($1, $2, $3)
ON CONFLICT (project_id, team_id) DO NOTHING;

-- name: DeleteProjectTeam :exec
-- scope: resolved
DELETE FROM project_teams WHERE org_id = $1 AND project_id = $2 AND team_id = $3;

-- name: GetUserRefs :many
-- scope: none
SELECT id, username, name FROM users WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- ---------------------------------------------------------------------------
-- Milestones
-- ---------------------------------------------------------------------------

-- name: InsertMilestone :one
-- scope: resolved
INSERT INTO milestones (org_id, project_id, name, description, target_date, sort_order)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetMilestone :one
-- scope: project
-- Project scoped: a milestone follows its project (T20 #38).
SELECT m.* FROM milestones m
JOIN projects p ON p.org_id = m.org_id AND p.id = m.project_id
WHERE m.org_id = $1 AND m.id = $2
  AND (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id)
    OR EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id
               AND pst.team_id = ANY(sqlc.arg(team_ids)::uuid[])));

-- name: GetMilestoneSystem :one
-- scope: system
-- The unscoped copy, for code with no caller (activity snapshots, T20 #43).
SELECT * FROM milestones WHERE org_id = $1 AND id = $2;

-- name: LockMilestone :one
-- scope: project
-- Project scoped (T20 #38).
SELECT m.* FROM milestones m
JOIN projects p ON p.org_id = m.org_id AND p.id = m.project_id
WHERE m.org_id = $1 AND m.id = $2
  AND (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id)
    OR EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id
               AND pst.team_id = ANY(sqlc.arg(team_ids)::uuid[])))
FOR UPDATE OF m;

-- name: UpdateMilestone :one
-- scope: resolved
UPDATE milestones
SET name = $3, description = $4, target_date = $5, sort_order = $6, updated_at = now()
WHERE org_id = $1 AND id = $2
RETURNING *;

-- ---------------------------------------------------------------------------
-- Documents
-- ---------------------------------------------------------------------------

-- name: InsertDocument :one
-- scope: resolved
INSERT INTO documents (org_id, project_id, title, content, slug, creator_id)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetDocument :one
-- scope: project
-- Project scoped: a document with no project is org-level (T20 #7, #38).
SELECT d.* FROM documents d
LEFT JOIN projects p ON p.org_id = d.org_id AND p.id = d.project_id
WHERE d.org_id = $1 AND d.id = $2
  AND (d.project_id IS NULL OR (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id)
    OR EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id
               AND pst.team_id = ANY(sqlc.arg(team_ids)::uuid[]))));

-- name: GetDocumentBySlug :one
-- scope: project
SELECT d.* FROM documents d
LEFT JOIN projects p ON p.org_id = d.org_id AND p.id = d.project_id
WHERE d.org_id = $1 AND d.slug = $2
  AND (d.project_id IS NULL OR (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id)
    OR EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id
               AND pst.team_id = ANY(sqlc.arg(team_ids)::uuid[]))));

-- name: LockDocument :one
-- scope: project
SELECT d.* FROM documents d
LEFT JOIN projects p ON p.org_id = d.org_id AND p.id = d.project_id
WHERE d.org_id = $1 AND d.id = $2
  AND (d.project_id IS NULL OR (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id)
    OR EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id
               AND pst.team_id = ANY(sqlc.arg(team_ids)::uuid[]))))
FOR UPDATE OF d;

-- name: UpdateDocument :one
-- scope: resolved
UPDATE documents
SET title = $3, content = $4, project_id = $5, version = version + 1, updated_at = now()
WHERE org_id = $1 AND id = $2
RETURNING *;

-- name: ListDocumentComments :many
-- scope: resolved
SELECT c.id, c.document_id, c.parent_id, c.body, c.created_at, c.updated_at, c.edited_at,
       c.author_id, u.username AS author_username, u.name AS author_name
FROM comments c
LEFT JOIN users u ON u.id = c.author_id
WHERE c.org_id = $1 AND c.document_id = $2
ORDER BY c.created_at, c.id;

-- name: InsertDocumentComment :one
-- scope: resolved
INSERT INTO comments (org_id, document_id, parent_id, author_id, body)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- ---------------------------------------------------------------------------
-- Cycles
-- ---------------------------------------------------------------------------

-- name: GetCycle :one
-- scope: team
-- Team scoped (T20 #14).
SELECT c.*, t.key AS team_key, t.name AS team_name
FROM cycles c
JOIN teams t ON t.org_id = c.org_id AND t.id = c.team_id
WHERE c.org_id = $1 AND c.id = $2
  AND (sqlc.arg(all_teams)::bool OR c.team_id = ANY(sqlc.arg(team_ids)::uuid[]));

-- name: GetCycleSystem :one
-- scope: system
-- The unscoped copy, for code with no caller (activity snapshots, T20 #43).
SELECT c.*, t.key AS team_key, t.name AS team_name
FROM cycles c
JOIN teams t ON t.org_id = c.org_id AND t.id = c.team_id
WHERE c.org_id = $1 AND c.id = $2;

-- name: GetCycleTeamID :one
-- scope: team
-- Team scoped (T20 #14): a cycle of a hidden team is not found.
SELECT c.team_id FROM cycles c WHERE c.org_id = $1 AND c.id = $2
  AND (sqlc.arg(all_teams)::bool OR c.team_id = ANY(sqlc.arg(team_ids)::uuid[]));

-- name: GetMilestoneProjectID :one
-- scope: project
-- Project scoped (T20 #38): a milestone of a hidden project is not found.
SELECT m.project_id FROM milestones m
JOIN projects p ON p.org_id = m.org_id AND p.id = m.project_id
WHERE m.org_id = $1 AND m.id = $2
  AND (sqlc.arg(all_teams)::bool
    OR NOT EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id)
    OR EXISTS (SELECT 1 FROM project_teams pst WHERE pst.org_id = p.org_id AND pst.project_id = p.id
               AND pst.team_id = ANY(sqlc.arg(team_ids)::uuid[])));

-- name: UpdateCycleText :one
-- scope: resolved
UPDATE cycles SET name = $3, description = $4, updated_at = now()
WHERE org_id = $1 AND id = $2
RETURNING *;

-- name: LockCycle :one
-- scope: resolved
-- The cycle row FOR UPDATE (cycle options, decisions-T27 #6): an issue write
-- that names the cycle holds KEY SHARE on it, so a delete that counts the
-- issues after this lock cannot miss one.
SELECT * FROM cycles WHERE org_id = $1 AND id = $2 FOR UPDATE;

-- name: SetCycleRange :one
-- scope: resolved
UPDATE cycles SET starts_at = $3, ends_at = $4, updated_at = now()
WHERE org_id = $1 AND id = $2
RETURNING *;

-- name: CycleEndingAt :one
-- scope: resolved
-- The cycle of a team whose end is exactly at, other than one id: the
-- predecessor in the contiguous chain.
SELECT * FROM cycles
WHERE org_id = $1 AND team_id = $2 AND ends_at = sqlc.arg(at) AND id <> sqlc.arg(except_id)
ORDER BY number
LIMIT 1;

-- name: CycleStartingAt :one
-- scope: resolved
-- The successor in the contiguous chain.
SELECT * FROM cycles
WHERE org_id = $1 AND team_id = $2 AND starts_at = sqlc.arg(at) AND id <> sqlc.arg(except_id)
ORDER BY number
LIMIT 1;

-- name: CycleRangeTaken :one
-- scope: resolved
-- True when another cycle of the team (not in except_ids) overlaps the range.
SELECT EXISTS (
    SELECT 1 FROM cycles
    WHERE org_id = $1 AND team_id = $2 AND NOT (id = ANY(sqlc.arg(except_ids)::uuid[]))
      AND starts_at < sqlc.arg(range_end) AND ends_at > sqlc.arg(range_start)
);

-- name: CountCycleIssues :one
-- scope: admin
-- Every issue of the cycle, archived ones too.
SELECT count(*) FROM issues WHERE org_id = $1 AND cycle_id = $2;

-- name: DeleteCycle :exec
-- scope: admin
DELETE FROM cycles WHERE org_id = $1 AND id = $2;

-- name: ListCycleTeams :many
-- scope: system
-- Every team the cycle job looks after, across all orgs.
SELECT org_id, id FROM teams WHERE cycles_enabled AND archived_at IS NULL ORDER BY org_id, id;

-- name: LastCycle :one
-- scope: system
SELECT * FROM cycles WHERE org_id = $1 AND team_id = $2 ORDER BY number DESC LIMIT 1;

-- name: InsertCycle :one
-- scope: system
INSERT INTO cycles (org_id, team_id, number, starts_at, ends_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListEndedOpenCycles :many
-- scope: system
SELECT * FROM cycles
WHERE org_id = $1 AND team_id = $2 AND completed_at IS NULL AND ends_at <= sqlc.arg(now)
ORDER BY starts_at, number;

-- name: NextCycleAfter :one
-- scope: system
-- The successor of a cycle: the earliest cycle of the team that starts at
-- or after its end. By time, not number + 1, so a gap in the numbers (a
-- partial import) does not break the rollover.
SELECT * FROM cycles
WHERE org_id = $1 AND team_id = $2 AND starts_at >= sqlc.arg(after)
ORDER BY starts_at, number
LIMIT 1;

-- name: CurrentCycle :one
-- scope: system
SELECT * FROM cycles
WHERE org_id = $1 AND team_id = $2 AND completed_at IS NULL
  AND starts_at <= sqlc.arg(now) AND ends_at > sqlc.arg(now)
ORDER BY number
LIMIT 1;

-- name: CompleteCycle :exec
-- scope: admin
UPDATE cycles SET completed_at = $3, updated_at = now()
WHERE org_id = $1 AND id = $2 AND completed_at IS NULL;

-- name: RolloverIssues :many
-- scope: system
-- Moves the OPEN issues of one cycle to another: state type not completed
-- or canceled or triage (LAUN-20: a triage issue is not planned work, so it
-- stays where it is), and not archived. The state does not change, so no
-- issue_state_changes row is written; version is bumped as for any change.
-- The rows are locked in id order first (a CTE with FOR UPDATE, never
-- inlined), the same order as RebalanceColumn, so the two cannot deadlock
-- (decisions-T14 #10).
WITH locked AS MATERIALIZED (
    SELECT x.id FROM issues x
    JOIN workflow_states s ON s.org_id = x.org_id AND s.id = x.state_id
    WHERE x.org_id = sqlc.arg(org_id) AND x.cycle_id = sqlc.arg(from_cycle_id) AND x.archived_at IS NULL
      AND s.type NOT IN ('completed', 'canceled', 'triage')
    ORDER BY x.id
    FOR UPDATE OF x
)
UPDATE issues i
SET cycle_id = sqlc.arg(to_cycle_id), version = i.version + 1, updated_at = now()
FROM locked
WHERE i.org_id = sqlc.arg(org_id) AND i.id = locked.id
RETURNING i.id, i.number, i.version;

-- name: UpsertCycleSnapshot :one
-- scope: resolved
-- One row per cycle per day. The counts are of the issues IN the cycle at
-- the moment this runs (archived issues excluded); scope is every such issue
-- whose state is not canceled or triage (decisions-T3, decisions-T19-triage).
INSERT INTO cycle_snapshots (org_id, cycle_id, snapshot_date,
    scope_count, triage_count, backlog_count, unstarted_count, started_count, completed_count, canceled_count,
    scope_points, started_points, completed_points)
SELECT sqlc.arg(org_id)::uuid, sqlc.arg(cycle_id)::uuid, sqlc.arg(snapshot_date)::date,
    count(*) FILTER (WHERE s.type NOT IN ('canceled', 'triage'))::integer,
    count(*) FILTER (WHERE s.type = 'triage')::integer,
    count(*) FILTER (WHERE s.type = 'backlog')::integer,
    count(*) FILTER (WHERE s.type = 'unstarted')::integer,
    count(*) FILTER (WHERE s.type = 'started')::integer,
    count(*) FILTER (WHERE s.type = 'completed')::integer,
    count(*) FILTER (WHERE s.type = 'canceled')::integer,
    COALESCE(sum(i.estimate) FILTER (WHERE s.type NOT IN ('canceled', 'triage')), 0)::bigint,
    COALESCE(sum(i.estimate) FILTER (WHERE s.type = 'started'), 0)::bigint,
    COALESCE(sum(i.estimate) FILTER (WHERE s.type = 'completed'), 0)::bigint
FROM issues i
JOIN workflow_states s ON s.org_id = i.org_id AND s.id = i.state_id
WHERE i.org_id = sqlc.arg(org_id)::uuid AND i.cycle_id = sqlc.arg(cycle_id)::uuid AND i.archived_at IS NULL
-- Never for a completed cycle: after its rollover the counts would be of
-- what is left, not of the cycle as it ended. No row then (pgx.ErrNoRows).
HAVING EXISTS (SELECT 1 FROM cycles c WHERE c.org_id = sqlc.arg(org_id)::uuid AND c.id = sqlc.arg(cycle_id)::uuid
               AND c.completed_at IS NULL)
ON CONFLICT (cycle_id, snapshot_date) DO UPDATE SET
    scope_count = EXCLUDED.scope_count, triage_count = EXCLUDED.triage_count,
    backlog_count = EXCLUDED.backlog_count, unstarted_count = EXCLUDED.unstarted_count,
    started_count = EXCLUDED.started_count, completed_count = EXCLUDED.completed_count,
    canceled_count = EXCLUDED.canceled_count, scope_points = EXCLUDED.scope_points,
    started_points = EXCLUDED.started_points, completed_points = EXCLUDED.completed_points,
    updated_at = now()
-- A refresh that changes nothing writes nothing (no row, no WAL): a read
-- token calling GET .../snapshots cannot churn the table.
WHERE (cycle_snapshots.scope_count, cycle_snapshots.triage_count, cycle_snapshots.backlog_count,
       cycle_snapshots.unstarted_count, cycle_snapshots.started_count, cycle_snapshots.completed_count,
       cycle_snapshots.canceled_count, cycle_snapshots.scope_points, cycle_snapshots.started_points,
       cycle_snapshots.completed_points)
  IS DISTINCT FROM
      (EXCLUDED.scope_count, EXCLUDED.triage_count, EXCLUDED.backlog_count, EXCLUDED.unstarted_count,
       EXCLUDED.started_count, EXCLUDED.completed_count, EXCLUDED.canceled_count, EXCLUDED.scope_points,
       EXCLUDED.started_points, EXCLUDED.completed_points)
RETURNING *;

-- name: ListCycleSnapshots :many
-- scope: resolved
SELECT * FROM cycle_snapshots WHERE org_id = $1 AND cycle_id = $2 ORDER BY snapshot_date;

-- name: DeleteProject :exec
-- scope: admin
-- Milestones and project_teams go with the project (ON DELETE CASCADE); issues
-- and documents keep existing with a null project_id (ON DELETE SET NULL).
DELETE FROM projects WHERE org_id = $1 AND id = $2;

-- ---------------------------------------------------------------------------
-- Project updates (LAUN-196, decisions-T30)
-- ---------------------------------------------------------------------------

-- name: InsertProjectUpdate :one
-- scope: resolved
INSERT INTO project_updates (org_id, project_id, author_id, health, body)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetProjectUpdate :one
-- scope: resolved
SELECT * FROM project_updates WHERE org_id = $1 AND project_id = $2 AND id = $3;

-- ---------------------------------------------------------------------------
-- Project update schedule and reminders (LAUN-196 part B, decisions-T30)
-- ---------------------------------------------------------------------------

-- name: ListUpdateScheduledProjects :many
-- scope: system
SELECT org_id, id FROM projects
WHERE update_frequency <> 'none' AND archived_at IS NULL AND state NOT IN ('completed', 'canceled')
ORDER BY org_id, id;

-- name: LockScheduledProject :one
-- scope: system
SELECT p.id, p.org_id, p.lead_id, p.state, p.archived_at, p.update_frequency, p.update_weekday, p.update_hour,
       p.update_last_due_at, o.time_zone
FROM projects p JOIN orgs o ON o.id = p.org_id
WHERE p.org_id = $1 AND p.id = $2
FOR UPDATE OF p;

-- name: SetProjectUpdateLastDue :exec
-- scope: system
UPDATE projects SET update_last_due_at = $3 WHERE org_id = $1 AND id = $2;

-- name: SetProjectUpdateSchedule :exec
-- scope: resolved
-- No version bump: the caller bumps it once for the whole PATCH.
UPDATE projects
SET update_frequency = $3, update_weekday = $4, update_hour = $5, update_last_due_at = $6
WHERE org_id = $1 AND id = $2;

-- name: ProjectHasUpdateSince :one
-- scope: system
SELECT EXISTS (SELECT 1 FROM project_updates WHERE org_id = $1 AND project_id = $2 AND created_at >= $3);

-- name: GetOrgTimeZone :one
-- scope: none
SELECT time_zone FROM orgs WHERE id = $1;

-- name: LeadIsHuman :one
-- scope: system
-- True for an active, non-agent member of the org.
SELECT EXISTS (
  SELECT 1 FROM users u JOIN org_members m ON m.user_id = u.id
  WHERE m.org_id = $1 AND u.id = $2 AND NOT u.is_agent AND u.deactivated_at IS NULL);

-- ---------------------------------------------------------------------------
-- Project labels (LAUN-194, decisions-T28)
-- ---------------------------------------------------------------------------

-- name: ListProjectLabels :many
-- scope: project
-- The catalogue is org-wide (decisions-T28); project_count counts only the
-- projects the caller sees (T20 #36, security analysis "Counts").
SELECT l.id, l.name, l.color, l.parent_id, l.created_at,
       (SELECT count(*) FROM project_label_assignments a
         WHERE a.org_id = l.org_id AND a.label_id = l.id
           AND (sqlc.arg(all_teams)::bool
                OR NOT EXISTS (SELECT 1 FROM project_teams pt WHERE pt.org_id = a.org_id AND pt.project_id = a.project_id)
                OR EXISTS (SELECT 1 FROM project_teams pt WHERE pt.org_id = a.org_id AND pt.project_id = a.project_id
                           AND pt.team_id = ANY(sqlc.arg(team_ids)::uuid[]))))::int AS project_count
FROM project_labels l
WHERE l.org_id = sqlc.arg(org_id)
ORDER BY lower(l.name), l.id;

-- name: GetProjectLabel :one
-- scope: org
SELECT * FROM project_labels WHERE org_id = $1 AND id = $2;

-- name: LockProjectLabel :one
-- scope: admin
SELECT * FROM project_labels WHERE org_id = $1 AND id = $2 FOR UPDATE;

-- name: InsertProjectLabel :one
-- scope: admin
INSERT INTO project_labels (org_id, name, color, parent_id) VALUES ($1, $2, $3, sqlc.narg(parent_id))
RETURNING *;

-- name: UpdateProjectLabel :one
-- scope: admin
UPDATE project_labels SET name = $3, color = $4, parent_id = sqlc.narg(parent_id)
WHERE org_id = $1 AND id = $2
RETURNING *;

-- name: DeleteProjectLabel :exec
-- scope: admin
DELETE FROM project_labels WHERE org_id = $1 AND id = $2;

-- name: CountProjectLabelChildren :one
-- scope: admin
SELECT count(*)::int FROM project_labels WHERE org_id = $1 AND parent_id = $2;

-- name: LockProjectLabelsByID :many
-- scope: org
-- Share-locks the named labels of the org in id order, so a concurrent delete waits and lock order is global.
SELECT id FROM project_labels
WHERE org_id = sqlc.arg(org_id) AND id = ANY(sqlc.arg(ids)::uuid[])
ORDER BY id
FOR SHARE;

-- name: LockProjectLabelsForUpdate :many
-- scope: org
-- Row-locks the named labels in id order (the global lock order of project
-- labels, decisions-T28 #10), so two edits that touch the same pair cannot deadlock.
SELECT id FROM project_labels
WHERE org_id = sqlc.arg(org_id) AND id = ANY(sqlc.arg(ids)::uuid[])
ORDER BY id
FOR UPDATE;

-- name: ListProjectLabelIDs :many
-- scope: resolved
SELECT label_id FROM project_label_assignments WHERE org_id = $1 AND project_id = $2;

-- name: InsertProjectLabelAssignment :exec
-- scope: resolved
INSERT INTO project_label_assignments (org_id, project_id, label_id) VALUES ($1, $2, $3)
ON CONFLICT (project_id, label_id) DO NOTHING;

-- name: DeleteProjectLabelAssignment :exec
-- scope: resolved
DELETE FROM project_label_assignments WHERE org_id = $1 AND project_id = $2 AND label_id = $3;

-- name: ListLabelsForProjects :many
-- scope: resolved
SELECT a.project_id, l.id, l.name, l.color, l.parent_id
FROM project_label_assignments a
JOIN project_labels l ON l.org_id = a.org_id AND l.id = a.label_id
WHERE a.org_id = $1 AND a.project_id = ANY(sqlc.arg(project_ids)::uuid[])
ORDER BY a.project_id, lower(l.name);
