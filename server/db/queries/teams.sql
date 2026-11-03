-- name: ListTeams :many
-- scope: team
-- The teams the caller sees (T20 #9): a hidden team is not listed.
SELECT * FROM teams WHERE org_id = $1 AND archived_at IS NULL
  AND (sqlc.arg(all_teams)::bool OR id = ANY(sqlc.arg(team_ids)::uuid[]))
ORDER BY key;

-- name: ListTeamsSystem :many
-- scope: system
-- The unscoped copy, for code with no caller (the git webhooks).
SELECT * FROM teams WHERE org_id = $1 AND archived_at IS NULL ORDER BY key;

-- name: GetTeamByKey :one
-- scope: team
-- Team scoped (T20 #14).
SELECT * FROM teams WHERE org_id = $1 AND key = $2
  AND (sqlc.arg(all_teams)::bool OR id = ANY(sqlc.arg(team_ids)::uuid[]));

-- name: GetTeamByKeySystem :one
-- scope: system
-- The unscoped copy, for code with no caller (the importer).
SELECT * FROM teams WHERE org_id = $1 AND key = $2;

-- name: GetTeamByID :one
-- scope: team
-- Team scoped (T20 #14).
SELECT * FROM teams WHERE org_id = $1 AND id = $2
  AND (sqlc.arg(all_teams)::bool OR id = ANY(sqlc.arg(team_ids)::uuid[]));

-- name: GetTeamByIDSystem :one
-- scope: system
-- The unscoped copy, for code with no caller (the importer, the unsubscribe
-- route, the cycle job).
SELECT * FROM teams WHERE org_id = $1 AND id = $2;

-- name: InsertTeam :one
-- scope: admin
INSERT INTO teams (org_id, key, name, description) VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: SetTeamAnchorStates :exec
-- scope: admin
UPDATE teams
SET default_state_id = $3, review_state_id = $4, testing_state_id = $5, updated_at = now()
WHERE org_id = $1 AND id = $2;

-- name: InsertIssueCounter :exec
-- scope: admin
INSERT INTO issue_counters (team_id, org_id, next_number) VALUES ($1, $2, 1)
ON CONFLICT (team_id) DO NOTHING;

-- name: NextIssueNumber :one
-- scope: resolved
-- The row lock taken here is held to COMMIT, so concurrent creates in one
-- team are serialised and can never get the same number.
UPDATE issue_counters SET next_number = next_number + 1
WHERE org_id = $1 AND team_id = $2
RETURNING (next_number - 1)::integer AS number;

-- name: InsertWorkflowState :one
-- scope: admin
INSERT INTO workflow_states (org_id, team_id, name, type, color, position)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListTeamStates :many
-- scope: resolved
SELECT * FROM workflow_states
WHERE org_id = $1 AND team_id = $2 AND archived_at IS NULL
ORDER BY position, name;

-- name: GetStateByID :one
-- scope: team
-- Scoped by the team join (T20 #23): a state of a hidden team is not found, in
-- the same words as a state that does not exist.
SELECT s.* FROM workflow_states s
JOIN teams t ON t.org_id = s.org_id AND t.id = s.team_id
WHERE s.org_id = $1 AND s.id = $2
  AND (sqlc.arg(all_teams)::bool OR t.id = ANY(sqlc.arg(team_ids)::uuid[]));

-- name: GetStateByIDSystem :one
-- scope: system
-- The unscoped copy, for code with no caller (webhooks).
SELECT * FROM workflow_states WHERE org_id = $1 AND id = $2;

-- name: GetStateByTeamAndName :one
-- scope: resolved
SELECT * FROM workflow_states
WHERE org_id = $1 AND team_id = $2 AND lower(name) = lower(sqlc.arg(name)::text) AND archived_at IS NULL;

-- name: FirstStateOfType :one
-- scope: resolved
SELECT * FROM workflow_states
WHERE org_id = $1 AND team_id = $2 AND type = $3 AND archived_at IS NULL
ORDER BY position
LIMIT 1;

-- name: LockTeam :one
-- scope: resolved
-- NO KEY UPDATE, not UPDATE: inserting a cycle takes KEY SHARE on the team
-- row (its foreign key), and FOR UPDATE would block that.
SELECT * FROM teams WHERE org_id = $1 AND id = $2 FOR NO KEY UPDATE;

-- name: UpdateTeamSettings :one
-- scope: admin
UPDATE teams
SET name = $3, description = $4, cycles_enabled = $5, cycle_length_days = $6, cycle_start_weekday = $7,
    cycle_start_time = $8, cycle_timezone = $9, cycle_rollover = $10, updated_at = now()
WHERE org_id = $1 AND id = $2
RETURNING *;

-- name: LockTeamForDelete :one
-- scope: admin
-- FOR UPDATE (not NO KEY UPDATE): it conflicts with the KEY SHARE an issue
-- insert takes on the team row, so no issue can arrive between the count and
-- the delete (LAUN-191).
SELECT * FROM teams WHERE org_id = $1 AND id = $2 FOR UPDATE;

-- name: LockOrgDefaultTeam :one
-- scope: none
-- Serialises team deletes in one org (the last-team check) and reads the default.
-- FOR NO KEY UPDATE, not FOR UPDATE: events.Emit locks org_event_counters and
-- then needs KEY SHARE on the org row (events_org_id_fkey); FOR UPDATE would
-- deadlock with any event-emitting write in the org (LAUN-191, decisions-T26 #6).
SELECT default_team_id FROM orgs WHERE id = $1 FOR NO KEY UPDATE;

-- name: CountOrgTeams :one
-- scope: admin
SELECT count(*) FROM teams WHERE org_id = $1;

-- name: CountTeamIssues :one
-- scope: admin
-- Every issue of the team, archived ones included.
SELECT count(*) FROM issues WHERE org_id = $1 AND team_id = $2;

-- name: DeleteTeam :exec
-- scope: admin
DELETE FROM teams WHERE org_id = $1 AND id = $2;
