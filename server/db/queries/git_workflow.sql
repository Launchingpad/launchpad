-- LAUN-244 (decisions-T44-git-workflow.md): per-team git workflow rules.
-- Every query filters by org_id.

-- name: ListTeamGitRules :many
-- scope: resolved
-- The team id comes from resolveTeam (hidden is 404) or from the webhook's
-- locked issue (system). ONE statement reads each rule with its "only from"
-- set, so a concurrent PUT cannot mix the halves of two rule sets (one
-- snapshot). The set is ordered by state id (uuid order is byte order).
SELECT r.event, r.target_state_id,
       COALESCE(array_agg(f.state_id ORDER BY f.state_id) FILTER (WHERE f.state_id IS NOT NULL), '{}')::uuid[] AS from_state_ids
FROM team_git_rules r
LEFT JOIN team_git_rule_from_states f ON f.org_id = r.org_id AND f.team_id = r.team_id AND f.event = r.event
WHERE r.org_id = $1 AND r.team_id = $2
GROUP BY r.event, r.target_state_id
ORDER BY r.event;

-- name: DeleteTeamGitRules :exec
-- scope: admin
-- The from-state rows go with their rule (ON DELETE CASCADE).
DELETE FROM team_git_rules WHERE org_id = $1 AND team_id = $2;

-- name: InsertTeamGitRule :exec
-- scope: admin
INSERT INTO team_git_rules (org_id, team_id, event, target_state_id) VALUES ($1, $2, $3, $4);

-- name: InsertTeamGitRuleFromState :exec
-- scope: admin
INSERT INTO team_git_rule_from_states (org_id, team_id, event, state_id) VALUES ($1, $2, $3, $4);

-- name: ListTeamStatesWithArchived :many
-- scope: resolved
-- Every state of the team, archived ones too: a rule may name a state that
-- was archived after the rule was saved (it is flagged, never followed).
SELECT * FROM workflow_states
WHERE org_id = $1 AND team_id = $2
ORDER BY position, name;
