-- T4: the Linear importer's writes (cmd/importer, internal/importer).
--
-- Every row is written with its ORIGINAL id and timestamps. Each upsert is
-- keyed on the primary key and its DO UPDATE has a WHERE ... IS DISTINCT
-- FROM guard, so an unchanged row is not written at all: the statement
-- then returns no row (pgx.ErrNoRows), which the importer counts as a skip.
-- That is what makes a second run change nothing. Every query takes org_id.

-- ---------------------------------------------------------------------------
-- Mappings and lookups
-- ---------------------------------------------------------------------------

-- name: ImportListMappings :many
-- scope: system
SELECT entity_type, source_id, target_id FROM import_mappings
WHERE org_id = $1 AND source = 'linear';

-- name: ImportUpsertMapping :execrows
-- scope: system
INSERT INTO import_mappings (org_id, source, entity_type, source_id, target_id)
VALUES ($1, 'linear', $2, $3, $4)
ON CONFLICT (org_id, source, entity_type, source_id) DO UPDATE SET target_id = EXCLUDED.target_id
WHERE import_mappings.target_id IS DISTINCT FROM EXCLUDED.target_id;

-- name: ImportLockOrg :exec
-- scope: none
-- One importer per org at a time (a second waits, then finds the work done).
SELECT pg_advisory_xact_lock(hashtextextended('devops:import:' || sqlc.arg(org_id)::uuid::text, 0));

-- name: ImportLockTeamCycles :exec
-- scope: none
-- The cycle job's per-team lock (tracker.lockTeamCycles), so the job never
-- runs on a team while its cycles are being imported.
SELECT pg_advisory_xact_lock(hashtextextended('devops:team-cycles:' || sqlc.arg(team_id)::uuid::text, 0));

-- name: ImportListTeamStates :many
-- scope: system
-- Every state of a team, archived ones too (names are unique per team
-- including archived rows).
SELECT * FROM workflow_states WHERE org_id = $1 AND team_id = $2;

-- name: ImportListLabels :many
-- scope: system
SELECT * FROM labels WHERE org_id = $1;

-- name: ImportTeamIssueNumbers :many
-- scope: system
SELECT id, number FROM issues WHERE org_id = $1 AND team_id = $2;

-- name: ImportTeamCycleNumbers :many
-- scope: system
SELECT id, number FROM cycles WHERE org_id = $1 AND team_id = $2;

-- name: ImportListSnapshotKeys :many
-- scope: system
SELECT cycle_id, snapshot_date FROM cycle_snapshots WHERE org_id = $1;

-- name: ImportFindRelationByPair :one
-- scope: system
SELECT id FROM issue_relations
WHERE org_id = $1 AND type = $2
  AND LEAST(issue_id, related_issue_id) = LEAST(sqlc.arg(a)::uuid, sqlc.arg(b)::uuid)
  AND GREATEST(issue_id, related_issue_id) = GREATEST(sqlc.arg(a)::uuid, sqlc.arg(b)::uuid);

-- name: ImportDocumentSlugOwner :one
-- scope: system
SELECT id FROM documents WHERE org_id = $1 AND slug = $2;

-- ---------------------------------------------------------------------------
-- Teams
-- ---------------------------------------------------------------------------

-- name: ImportSetTeamCadence :execrows
-- scope: system
-- Only while cycles are OFF: once an admin has turned them on, the cadence
-- is theirs (decisions-T3 #35).
UPDATE teams
SET cycle_length_days = sqlc.arg(length_days), cycle_start_weekday = sqlc.arg(weekday),
    cycle_start_time = sqlc.arg(start_time), cycle_timezone = sqlc.arg(timezone), updated_at = now()
WHERE org_id = sqlc.arg(org_id) AND id = sqlc.arg(id) AND NOT cycles_enabled
  AND (cycle_length_days IS DISTINCT FROM sqlc.arg(length_days)
       OR cycle_start_weekday IS DISTINCT FROM sqlc.arg(weekday)
       OR cycle_start_time IS DISTINCT FROM sqlc.arg(start_time)
       OR cycle_timezone IS DISTINCT FROM sqlc.arg(timezone));

-- name: ImportRaiseIssueCounter :execrows
-- scope: system
-- Never lowers the counter, so a re-run after new issues were made cannot
-- hand out a number twice.
UPDATE issue_counters SET next_number = sqlc.arg(next_number)
WHERE org_id = sqlc.arg(org_id) AND team_id = sqlc.arg(team_id) AND next_number < sqlc.arg(next_number);

-- name: ImportIssueCounter :one
-- scope: system
SELECT next_number FROM issue_counters WHERE org_id = $1 AND team_id = $2;

-- ---------------------------------------------------------------------------
-- Workflow states and labels
-- ---------------------------------------------------------------------------

-- name: ImportUpsertState :one
-- scope: system
INSERT INTO workflow_states (id, org_id, team_id, name, type, color, description, position, created_at, archived_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name, type = EXCLUDED.type, color = EXCLUDED.color, description = EXCLUDED.description,
    position = EXCLUDED.position, archived_at = EXCLUDED.archived_at
WHERE workflow_states.org_id = EXCLUDED.org_id
  AND (workflow_states.name, workflow_states.type, workflow_states.color, workflow_states.description,
       workflow_states.position, workflow_states.archived_at)
      IS DISTINCT FROM
      (EXCLUDED.name, EXCLUDED.type, EXCLUDED.color, EXCLUDED.description, EXCLUDED.position, EXCLUDED.archived_at)
RETURNING id;

-- name: ImportUpsertLabel :one
-- scope: system
-- team_id: set on insert, and set later only on a row that has none (a label
-- imported before LAUN-54 was org-wide; Linear says it is a team's). A label
-- never loses or changes a team.
INSERT INTO labels (id, org_id, name, color, description, parent_id, created_at, archived_at, team_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, sqlc.narg(team_id))
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name, color = EXCLUDED.color, description = EXCLUDED.description,
    parent_id = EXCLUDED.parent_id, archived_at = EXCLUDED.archived_at,
    team_id = COALESCE(labels.team_id, EXCLUDED.team_id)
WHERE labels.org_id = EXCLUDED.org_id
  AND (labels.name, labels.color, labels.description, labels.parent_id, labels.archived_at, labels.team_id)
      IS DISTINCT FROM
      (EXCLUDED.name, EXCLUDED.color, EXCLUDED.description, EXCLUDED.parent_id, EXCLUDED.archived_at,
       COALESCE(labels.team_id, EXCLUDED.team_id))
RETURNING id;

-- name: ImportLabelUsedOutsideTeam :one
-- scope: system
-- How many issues of OTHER teams carry the label (LAUN-54).
SELECT count(*) FROM issue_labels il
JOIN issues i ON i.org_id = il.org_id AND i.id = il.issue_id
WHERE il.org_id = $1 AND il.label_id = sqlc.arg(label_id) AND i.team_id <> sqlc.arg(team_id);

-- ---------------------------------------------------------------------------
-- Projects, milestones, cycles, snapshots
-- ---------------------------------------------------------------------------

-- name: ImportUpsertProject :one
-- scope: system
INSERT INTO projects (id, org_id, name, description, content, state, status_id, lead_id, start_date, target_date,
                      created_at, updated_at, started_at, completed_at, canceled_at, archived_at)
-- LAUN-195: the org's first status of the kind (its seeded default unless an admin reordered them).
VALUES ($1, $2, $3, $4, $5, $6,
        (SELECT s.id FROM project_statuses s WHERE s.org_id = $2 AND s.kind = $6::text ORDER BY s.position, s.id LIMIT 1),
        $7, $8, $9, $10, $11, $12, $13, $14, $15)
ON CONFLICT (id) DO UPDATE SET
    name = EXCLUDED.name, description = EXCLUDED.description, content = EXCLUDED.content,
    state = EXCLUDED.state,
    status_id = CASE WHEN projects.state = EXCLUDED.state THEN projects.status_id ELSE EXCLUDED.status_id END, lead_id = EXCLUDED.lead_id, start_date = EXCLUDED.start_date,
    target_date = EXCLUDED.target_date, updated_at = EXCLUDED.updated_at, started_at = EXCLUDED.started_at,
    completed_at = EXCLUDED.completed_at, canceled_at = EXCLUDED.canceled_at,
    archived_at = EXCLUDED.archived_at, version = projects.version + 1
WHERE projects.org_id = EXCLUDED.org_id
  AND (projects.name, projects.description, projects.content, projects.state, projects.lead_id,
       projects.start_date, projects.target_date, projects.updated_at, projects.started_at,
       projects.completed_at, projects.canceled_at, projects.archived_at)
      IS DISTINCT FROM
      (EXCLUDED.name, EXCLUDED.description, EXCLUDED.content, EXCLUDED.state, EXCLUDED.lead_id,
       EXCLUDED.start_date, EXCLUDED.target_date, EXCLUDED.updated_at, EXCLUDED.started_at,
       EXCLUDED.completed_at, EXCLUDED.canceled_at, EXCLUDED.archived_at)
RETURNING id;

-- name: ImportAddProjectTeam :execrows
-- scope: system
INSERT INTO project_teams (org_id, project_id, team_id) VALUES ($1, $2, $3)
ON CONFLICT (project_id, team_id) DO NOTHING;

-- name: ImportPruneProjectTeams :execrows
-- scope: system
DELETE FROM project_teams
WHERE org_id = $1 AND project_id = $2 AND NOT (team_id = ANY(sqlc.arg(keep)::uuid[]));

-- name: ImportUpsertMilestone :one
-- scope: system
INSERT INTO milestones (id, org_id, project_id, name, description, target_date, sort_order,
                        created_at, updated_at, archived_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (id) DO UPDATE SET
    project_id = EXCLUDED.project_id, name = EXCLUDED.name, description = EXCLUDED.description,
    target_date = EXCLUDED.target_date, sort_order = EXCLUDED.sort_order,
    updated_at = EXCLUDED.updated_at, archived_at = EXCLUDED.archived_at
WHERE milestones.org_id = EXCLUDED.org_id
  AND (milestones.project_id, milestones.name, milestones.description, milestones.target_date,
       milestones.sort_order, milestones.updated_at, milestones.archived_at)
      IS DISTINCT FROM
      (EXCLUDED.project_id, EXCLUDED.name, EXCLUDED.description, EXCLUDED.target_date,
       EXCLUDED.sort_order, EXCLUDED.updated_at, EXCLUDED.archived_at)
RETURNING id;

-- name: ImportUpsertCycle :one
-- scope: system
INSERT INTO cycles (id, org_id, team_id, number, name, description, starts_at, ends_at, completed_at,
                    created_at, updated_at, archived_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (id) DO UPDATE SET
    number = EXCLUDED.number, name = EXCLUDED.name, description = EXCLUDED.description,
    starts_at = EXCLUDED.starts_at, ends_at = EXCLUDED.ends_at, completed_at = EXCLUDED.completed_at,
    updated_at = EXCLUDED.updated_at, archived_at = EXCLUDED.archived_at
WHERE cycles.org_id = EXCLUDED.org_id AND cycles.team_id = EXCLUDED.team_id
  AND (cycles.number, cycles.name, cycles.description, cycles.starts_at, cycles.ends_at,
       cycles.completed_at, cycles.updated_at, cycles.archived_at)
      IS DISTINCT FROM
      (EXCLUDED.number, EXCLUDED.name, EXCLUDED.description, EXCLUDED.starts_at, EXCLUDED.ends_at,
       EXCLUDED.completed_at, EXCLUDED.updated_at, EXCLUDED.archived_at)
RETURNING id;

-- name: ImportUpsertSnapshot :one
-- scope: system
-- A burn-up row from Linear's cycle history arrays. Linear has no history
-- for the per-type counts, so they are written as 0.
INSERT INTO cycle_snapshots (id, org_id, cycle_id, snapshot_date, scope_count, started_count, completed_count,
                             scope_points, completed_points, started_points, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, 0, $6, $7, $8, $9, $10, $10)
ON CONFLICT (cycle_id, snapshot_date) DO UPDATE SET
    scope_count = EXCLUDED.scope_count, completed_count = EXCLUDED.completed_count,
    scope_points = EXCLUDED.scope_points, completed_points = EXCLUDED.completed_points,
    started_points = EXCLUDED.started_points, updated_at = now()
WHERE cycle_snapshots.org_id = EXCLUDED.org_id
  AND (cycle_snapshots.scope_count, cycle_snapshots.completed_count, cycle_snapshots.scope_points,
       cycle_snapshots.completed_points, cycle_snapshots.started_points)
      IS DISTINCT FROM
      (EXCLUDED.scope_count, EXCLUDED.completed_count, EXCLUDED.scope_points,
       EXCLUDED.completed_points, EXCLUDED.started_points)
RETURNING id;

-- ---------------------------------------------------------------------------
-- Issues
-- ---------------------------------------------------------------------------

-- name: ImportUpsertIssue :one
-- scope: system
-- sort_order is Linear's value on INSERT. On a re-run it is kept, except that
-- an issue whose state changed goes to the top of its new column: the old
-- rank means nothing there (decisions-T14 #12). A re-run never undoes a drag.
INSERT INTO issues (id, org_id, team_id, number, title, description, state_id, priority, estimate,
                    assignee_id, creator_id, parent_id, project_id, milestone_id, cycle_id, due_date,
                    sort_order, created_at, updated_at, started_at, completed_at, canceled_at, archived_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)
ON CONFLICT (id) DO UPDATE SET
    number = EXCLUDED.number, title = EXCLUDED.title, description = EXCLUDED.description,
    state_id = EXCLUDED.state_id, priority = EXCLUDED.priority, estimate = EXCLUDED.estimate,
    assignee_id = EXCLUDED.assignee_id, creator_id = EXCLUDED.creator_id, parent_id = EXCLUDED.parent_id,
    project_id = EXCLUDED.project_id, milestone_id = EXCLUDED.milestone_id, cycle_id = EXCLUDED.cycle_id,
    due_date = EXCLUDED.due_date,
    sort_order = CASE WHEN issues.state_id IS DISTINCT FROM EXCLUDED.state_id
        THEN (SELECT COALESCE(min(x.sort_order), 1024) - 1024 FROM issues x
              WHERE x.org_id = EXCLUDED.org_id AND x.team_id = EXCLUDED.team_id AND x.state_id = EXCLUDED.state_id)
        ELSE issues.sort_order END,
    updated_at = EXCLUDED.updated_at,
    started_at = EXCLUDED.started_at, completed_at = EXCLUDED.completed_at,
    canceled_at = EXCLUDED.canceled_at, archived_at = EXCLUDED.archived_at, version = issues.version + 1
WHERE issues.org_id = EXCLUDED.org_id AND issues.team_id = EXCLUDED.team_id
  AND (issues.number, issues.title, issues.description, issues.state_id, issues.priority, issues.estimate,
       issues.assignee_id, issues.creator_id, issues.parent_id, issues.project_id, issues.milestone_id,
       issues.cycle_id, issues.due_date, issues.updated_at, issues.started_at,
       issues.completed_at, issues.canceled_at, issues.archived_at)
      IS DISTINCT FROM
      (EXCLUDED.number, EXCLUDED.title, EXCLUDED.description, EXCLUDED.state_id, EXCLUDED.priority,
       EXCLUDED.estimate, EXCLUDED.assignee_id, EXCLUDED.creator_id, EXCLUDED.parent_id, EXCLUDED.project_id,
       EXCLUDED.milestone_id, EXCLUDED.cycle_id, EXCLUDED.due_date, EXCLUDED.updated_at,
       EXCLUDED.started_at, EXCLUDED.completed_at, EXCLUDED.canceled_at, EXCLUDED.archived_at)
RETURNING id;

-- name: ImportSetIssueParent :execrows
-- scope: system
-- The second pass for a parent written after its child. It keeps
-- updated_at and version: the parent link is part of the original row.
UPDATE issues SET parent_id = sqlc.narg(parent_id)
WHERE org_id = sqlc.arg(org_id) AND id = sqlc.arg(id) AND parent_id IS DISTINCT FROM sqlc.narg(parent_id);

-- name: ImportIssueLabelIDs :many
-- scope: system
SELECT label_id FROM issue_labels WHERE org_id = $1 AND issue_id = $2;

-- name: ImportAddIssueLabel :execrows
-- scope: system
INSERT INTO issue_labels (org_id, issue_id, label_id, created_at) VALUES ($1, $2, $3, $4)
ON CONFLICT (issue_id, label_id) DO NOTHING;

-- name: ImportPruneIssueLabels :execrows
-- scope: system
DELETE FROM issue_labels
WHERE org_id = $1 AND issue_id = $2 AND NOT (label_id = ANY(sqlc.arg(keep)::uuid[]));

-- name: ImportUpsertStateChange :one
-- scope: system
INSERT INTO issue_state_changes (id, org_id, issue_id, from_state_id, to_state_id, actor_id, source, created_at)
VALUES ($1, $2, $3, $4, $5, $6, 'import', $7)
ON CONFLICT (id) DO UPDATE SET
    from_state_id = EXCLUDED.from_state_id, to_state_id = EXCLUDED.to_state_id,
    actor_id = EXCLUDED.actor_id, created_at = EXCLUDED.created_at
WHERE issue_state_changes.org_id = EXCLUDED.org_id AND issue_state_changes.issue_id = EXCLUDED.issue_id
  AND (issue_state_changes.from_state_id, issue_state_changes.to_state_id, issue_state_changes.actor_id,
       issue_state_changes.created_at)
      IS DISTINCT FROM
      (EXCLUDED.from_state_id, EXCLUDED.to_state_id, EXCLUDED.actor_id, EXCLUDED.created_at)
RETURNING id;

-- ---------------------------------------------------------------------------
-- Documents, comments, relations, attachments
-- ---------------------------------------------------------------------------

-- name: ImportUpsertDocument :one
-- scope: system
INSERT INTO documents (id, org_id, project_id, title, content, slug, creator_id, created_at, updated_at, archived_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (id) DO UPDATE SET
    project_id = EXCLUDED.project_id, title = EXCLUDED.title, content = EXCLUDED.content,
    creator_id = EXCLUDED.creator_id, updated_at = EXCLUDED.updated_at, archived_at = EXCLUDED.archived_at,
    version = documents.version + 1
WHERE documents.org_id = EXCLUDED.org_id
  AND (documents.project_id, documents.title, documents.content, documents.creator_id,
       documents.updated_at, documents.archived_at)
      IS DISTINCT FROM
      (EXCLUDED.project_id, EXCLUDED.title, EXCLUDED.content, EXCLUDED.creator_id,
       EXCLUDED.updated_at, EXCLUDED.archived_at)
RETURNING id;

-- name: ImportUpsertComment :one
-- scope: system
INSERT INTO comments (id, org_id, issue_id, document_id, parent_id, author_id, body, created_at, updated_at, edited_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (id) DO UPDATE SET
    issue_id = EXCLUDED.issue_id, document_id = EXCLUDED.document_id, parent_id = EXCLUDED.parent_id,
    author_id = EXCLUDED.author_id, body = EXCLUDED.body, updated_at = EXCLUDED.updated_at,
    edited_at = EXCLUDED.edited_at
WHERE comments.org_id = EXCLUDED.org_id
  AND (comments.issue_id, comments.document_id, comments.parent_id, comments.author_id, comments.body,
       comments.updated_at, comments.edited_at)
      IS DISTINCT FROM
      (EXCLUDED.issue_id, EXCLUDED.document_id, EXCLUDED.parent_id, EXCLUDED.author_id, EXCLUDED.body,
       EXCLUDED.updated_at, EXCLUDED.edited_at)
RETURNING id;

-- name: ImportSetCommentParent :execrows
-- scope: system
UPDATE comments SET parent_id = sqlc.narg(parent_id)
WHERE org_id = sqlc.arg(org_id) AND id = sqlc.arg(id) AND parent_id IS DISTINCT FROM sqlc.narg(parent_id);

-- name: ImportUpsertRelation :one
-- scope: system
INSERT INTO issue_relations (id, org_id, type, issue_id, related_issue_id, created_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (id) DO UPDATE SET
    type = EXCLUDED.type, issue_id = EXCLUDED.issue_id, related_issue_id = EXCLUDED.related_issue_id
WHERE issue_relations.org_id = EXCLUDED.org_id
  AND (issue_relations.type, issue_relations.issue_id, issue_relations.related_issue_id)
      IS DISTINCT FROM (EXCLUDED.type, EXCLUDED.issue_id, EXCLUDED.related_issue_id)
RETURNING id;

-- name: ImportUpsertLinkAttachment :one
-- scope: system
INSERT INTO attachments (id, org_id, issue_id, kind, title, subtitle, url, metadata, creator_id, created_at, updated_at)
VALUES ($1, $2, $3, 'link', $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (id) DO UPDATE SET
    issue_id = EXCLUDED.issue_id, title = EXCLUDED.title, subtitle = EXCLUDED.subtitle, url = EXCLUDED.url,
    metadata = EXCLUDED.metadata, creator_id = EXCLUDED.creator_id, updated_at = EXCLUDED.updated_at
WHERE attachments.org_id = EXCLUDED.org_id AND attachments.kind = 'link'
  AND (attachments.issue_id, attachments.title, attachments.subtitle, attachments.url, attachments.metadata,
       attachments.creator_id, attachments.updated_at)
      IS DISTINCT FROM
      (EXCLUDED.issue_id, EXCLUDED.title, EXCLUDED.subtitle, EXCLUDED.url, EXCLUDED.metadata,
       EXCLUDED.creator_id, EXCLUDED.updated_at)
RETURNING id;

-- name: ImportUpsertFileAttachment :one
-- scope: system
-- An uploads.linear.app file (T4b): a `file` row whose object the importer
-- put in the store. A Linear attachment that was imported as a link (T4)
-- becomes a file row on the next run.
INSERT INTO attachments (id, org_id, issue_id, kind, title, subtitle, url, object_key, filename,
                         content_type, size_bytes, metadata, creator_id, created_at, updated_at)
VALUES ($1, $2, $3, 'file', $4, $5, NULL, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (id) DO UPDATE SET
    issue_id = EXCLUDED.issue_id, kind = 'file', title = EXCLUDED.title, subtitle = EXCLUDED.subtitle,
    url = NULL, object_key = EXCLUDED.object_key, filename = EXCLUDED.filename,
    content_type = EXCLUDED.content_type, size_bytes = EXCLUDED.size_bytes, metadata = EXCLUDED.metadata,
    creator_id = EXCLUDED.creator_id, updated_at = EXCLUDED.updated_at
WHERE attachments.org_id = EXCLUDED.org_id
  AND (attachments.issue_id, attachments.kind, attachments.title, attachments.subtitle, attachments.url,
       attachments.object_key, attachments.filename, attachments.content_type, attachments.size_bytes,
       attachments.metadata, attachments.creator_id, attachments.updated_at)
      IS DISTINCT FROM
      (EXCLUDED.issue_id, 'file'::text, EXCLUDED.title, EXCLUDED.subtitle, NULL::text, EXCLUDED.object_key,
       EXCLUDED.filename, EXCLUDED.content_type, EXCLUDED.size_bytes, EXCLUDED.metadata,
       EXCLUDED.creator_id, EXCLUDED.updated_at)
RETURNING id;

-- ---------------------------------------------------------------------------
-- Verification
-- ---------------------------------------------------------------------------

-- name: ImportVerifyMapped :many
-- scope: system
-- Per entity type: the Linear ids whose mapping points at a row that
-- exists in its table (in this org). A mapping whose row is gone is not
-- listed. The importer counts only the ids the current export expects.
SELECT 'team'::text AS entity_type, m.source_id FROM import_mappings m
  JOIN teams t ON t.org_id = m.org_id AND t.id = m.target_id
  WHERE m.org_id = sqlc.arg(org_id) AND m.source = 'linear' AND m.entity_type = 'team'
UNION ALL
SELECT 'workflow_state', m.source_id FROM import_mappings m
  JOIN workflow_states t ON t.org_id = m.org_id AND t.id = m.target_id
  WHERE m.org_id = sqlc.arg(org_id) AND m.source = 'linear' AND m.entity_type = 'workflow_state'
UNION ALL
SELECT 'label', m.source_id FROM import_mappings m
  JOIN labels t ON t.org_id = m.org_id AND t.id = m.target_id
  WHERE m.org_id = sqlc.arg(org_id) AND m.source = 'linear' AND m.entity_type = 'label'
UNION ALL
SELECT 'user', m.source_id FROM import_mappings m
  JOIN org_members t ON t.org_id = m.org_id AND t.user_id = m.target_id
  WHERE m.org_id = sqlc.arg(org_id) AND m.source = 'linear' AND m.entity_type = 'user'
UNION ALL
SELECT 'project', m.source_id FROM import_mappings m
  JOIN projects t ON t.org_id = m.org_id AND t.id = m.target_id
  WHERE m.org_id = sqlc.arg(org_id) AND m.source = 'linear' AND m.entity_type = 'project'
UNION ALL
SELECT 'milestone', m.source_id FROM import_mappings m
  JOIN milestones t ON t.org_id = m.org_id AND t.id = m.target_id
  WHERE m.org_id = sqlc.arg(org_id) AND m.source = 'linear' AND m.entity_type = 'milestone'
UNION ALL
SELECT 'cycle', m.source_id FROM import_mappings m
  JOIN cycles t ON t.org_id = m.org_id AND t.id = m.target_id
  WHERE m.org_id = sqlc.arg(org_id) AND m.source = 'linear' AND m.entity_type = 'cycle'
UNION ALL
SELECT 'issue', m.source_id FROM import_mappings m
  JOIN issues t ON t.org_id = m.org_id AND t.id = m.target_id
  WHERE m.org_id = sqlc.arg(org_id) AND m.source = 'linear' AND m.entity_type = 'issue'
UNION ALL
SELECT 'issue_state_change', m.source_id FROM import_mappings m
  JOIN issue_state_changes t ON t.org_id = m.org_id AND t.id = m.target_id
  WHERE m.org_id = sqlc.arg(org_id) AND m.source = 'linear' AND m.entity_type = 'issue_state_change'
UNION ALL
SELECT 'document', m.source_id FROM import_mappings m
  JOIN documents t ON t.org_id = m.org_id AND t.id = m.target_id
  WHERE m.org_id = sqlc.arg(org_id) AND m.source = 'linear' AND m.entity_type = 'document'
UNION ALL
SELECT 'comment', m.source_id FROM import_mappings m
  JOIN comments t ON t.org_id = m.org_id AND t.id = m.target_id
  WHERE m.org_id = sqlc.arg(org_id) AND m.source = 'linear' AND m.entity_type = 'comment'
UNION ALL
SELECT 'relation', m.source_id FROM import_mappings m
  JOIN issue_relations t ON t.org_id = m.org_id AND t.id = m.target_id
  WHERE m.org_id = sqlc.arg(org_id) AND m.source = 'linear' AND m.entity_type = 'relation'
UNION ALL
SELECT 'file', m.source_id FROM import_mappings m
  JOIN attachments t ON t.org_id = m.org_id AND t.id = m.target_id AND t.kind = 'file'
  WHERE m.org_id = sqlc.arg(org_id) AND m.source = 'linear' AND m.entity_type = 'file'
UNION ALL
SELECT 'attachment', m.source_id FROM import_mappings m
  JOIN attachments t ON t.org_id = m.org_id AND t.id = m.target_id
  WHERE m.org_id = sqlc.arg(org_id) AND m.source = 'linear' AND m.entity_type = 'attachment';

-- name: ImportVerifySnapshots :one
-- scope: system
-- How many of the expected burn-up rows exist. A key is
-- "<cycle uuid>|<YYYY-MM-DD>".
SELECT count(*)::bigint FROM cycle_snapshots s
WHERE s.org_id = sqlc.arg(org_id)
  AND (s.cycle_id::text || '|' || to_char(s.snapshot_date, 'YYYY-MM-DD')) = ANY(sqlc.arg(keys)::text[]);

-- name: ImportListFileObjects :many
-- scope: system
-- The object of every imported file row, for verify's store check.
SELECT m.source_id, t.object_key, t.size_bytes FROM import_mappings m
JOIN attachments t ON t.org_id = m.org_id AND t.id = m.target_id AND t.kind = 'file'
WHERE m.org_id = $1 AND m.source = 'linear' AND m.entity_type = 'file';

-- name: ImportListLinearUploadCandidates :many
-- scope: system
-- Imported rows whose text MAY hold an uploads.linear.app URL (T4b). A
-- cheap, case-insensitive filter only: verify applies the importer's one
-- URL definition (internal/importer markdown.go) to the text, so prose
-- that merely names the host is not counted. Only rows an import mapping
-- points at: a Linear URL a person pastes into a new comment after the
-- cutover is not the importer's.
SELECT 'issue'::text AS entity_type, i.id, i.description AS body FROM issues i
WHERE i.org_id = sqlc.arg(org_id)::uuid AND i.description ILIKE '%uploads.linear.app/%'
  AND EXISTS (SELECT 1 FROM import_mappings m WHERE m.org_id = i.org_id AND m.source = 'linear' AND m.entity_type = 'issue' AND m.target_id = i.id)
UNION ALL
SELECT 'comment', c.id, c.body FROM comments c
WHERE c.org_id = sqlc.arg(org_id)::uuid AND c.body ILIKE '%uploads.linear.app/%'
  AND EXISTS (SELECT 1 FROM import_mappings m WHERE m.org_id = c.org_id AND m.source = 'linear' AND m.entity_type = 'comment' AND m.target_id = c.id)
UNION ALL
SELECT 'document', d.id, d.content FROM documents d
WHERE d.org_id = sqlc.arg(org_id)::uuid AND d.content ILIKE '%uploads.linear.app/%'
  AND EXISTS (SELECT 1 FROM import_mappings m WHERE m.org_id = d.org_id AND m.source = 'linear' AND m.entity_type = 'document' AND m.target_id = d.id)
UNION ALL
SELECT 'project', p.id, p.description || ' ' || p.content FROM projects p
WHERE p.org_id = sqlc.arg(org_id)::uuid AND (p.description ILIKE '%uploads.linear.app/%' OR p.content ILIKE '%uploads.linear.app/%')
  AND EXISTS (SELECT 1 FROM import_mappings m WHERE m.org_id = p.org_id AND m.source = 'linear' AND m.entity_type = 'project' AND m.target_id = p.id)
UNION ALL
SELECT 'milestone', ms.id, ms.description FROM milestones ms
WHERE ms.org_id = sqlc.arg(org_id)::uuid AND ms.description ILIKE '%uploads.linear.app/%'
  AND EXISTS (SELECT 1 FROM import_mappings m WHERE m.org_id = ms.org_id AND m.source = 'linear' AND m.entity_type = 'milestone' AND m.target_id = ms.id)
UNION ALL
SELECT 'attachment', a.id, COALESCE(a.url, '') FROM attachments a
WHERE a.org_id = sqlc.arg(org_id)::uuid AND a.url ILIKE '%uploads.linear.app/%'
  AND EXISTS (SELECT 1 FROM import_mappings m WHERE m.org_id = a.org_id AND m.source = 'linear' AND m.entity_type = 'attachment' AND m.target_id = a.id)
ORDER BY 1, 2;

-- name: ImportCountFileMappings :one
-- scope: system
-- How many files an earlier import stored for the org (T4b).
SELECT count(*)::bigint FROM import_mappings
WHERE org_id = $1 AND source = 'linear' AND entity_type = 'file';
