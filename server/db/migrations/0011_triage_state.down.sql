-- Reverses 0011_triage_state. A down cannot keep issues in a state that is
-- deleted. "The Triage states" below are the rows the DELETE removes: name
-- 'Triage' AND type 'triage'. A triage-type state with another name (an
-- import from Linear) is never touched, and its issues stay.
-- 1. A team default that is a Triage state, or is NULL, is set to a
--    non-triage state of the team: the backlog-type one with the lowest
--    position, else the lowest-position non-triage state. A team with none
--    keeps a NULL default.
-- 2. Issues in a Triage state move to the team's default (now never Triage),
--    else to the same fallback state.
-- 3. The state change rows that name a Triage state are deleted (their keys
--    have no ON DELETE); review and testing anchors clear by SET NULL.
-- import_mappings rows for an adopted Triage state are not touched (they
-- carry no foreign key to the state).
UPDATE teams t
SET default_state_id = (
    SELECT s.id FROM workflow_states s
    WHERE s.team_id = t.id AND s.archived_at IS NULL AND NOT (s.type = 'triage' AND s.name = 'Triage')
    ORDER BY (s.type = 'backlog') DESC, s.position, s.name
    LIMIT 1)
WHERE t.default_state_id IS NULL
   OR t.default_state_id IN (SELECT id FROM workflow_states WHERE type = 'triage' AND name = 'Triage');

UPDATE issues i
SET state_id = COALESCE(
    (SELECT t.default_state_id FROM teams t WHERE t.id = i.team_id),
    (SELECT s.id FROM workflow_states s
     WHERE s.team_id = i.team_id AND s.archived_at IS NULL AND NOT (s.type = 'triage' AND s.name = 'Triage')
     ORDER BY (s.type = 'backlog') DESC, s.position, s.name
     LIMIT 1),
    -- last resort: any non-triage state of the team, archived or not
    (SELECT s.id FROM workflow_states s
     WHERE s.team_id = i.team_id AND NOT (s.type = 'triage' AND s.name = 'Triage')
     ORDER BY (s.type = 'backlog') DESC, s.archived_at IS NOT NULL, s.position, s.name
     LIMIT 1))
WHERE i.state_id IN (SELECT id FROM workflow_states WHERE type = 'triage' AND name = 'Triage');

DELETE FROM issue_state_changes c
USING workflow_states s
WHERE s.org_id = c.org_id AND s.type = 'triage' AND s.name = 'Triage'
  AND (s.id = c.from_state_id OR s.id = c.to_state_id);

DELETE FROM workflow_states WHERE type = 'triage' AND name = 'Triage';
