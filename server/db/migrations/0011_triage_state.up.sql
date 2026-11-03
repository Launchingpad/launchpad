-- LAUN-20: a "Triage" workflow state (type triage) for every team that has
-- none (docs/internal/decisions-T19-triage.md). New teams get it from
-- tracker.DefaultStates (CreateTeamTx), not from here.
-- It sits first: position = the team's lowest position - 1. A team that
-- already has a state of type triage, or any state named "Triage" (the
-- unique (team_id, name) key), is left alone. Archived states count.
INSERT INTO workflow_states (org_id, team_id, name, type, color, position)
SELECT t.org_id, t.id, 'Triage', 'triage', '#bec2c8',
       COALESCE((SELECT min(s.position) FROM workflow_states s WHERE s.team_id = t.id), 0) - 1
FROM teams t
WHERE NOT EXISTS (
    SELECT 1 FROM workflow_states s
    WHERE s.team_id = t.id AND (s.type = 'triage' OR lower(s.name) = 'triage')
);
