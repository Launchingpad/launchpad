-- LAUN-54: team labels. A label has either no team (an org-wide label, as
-- before) or one team. The composite FK keeps the team in the label's org;
-- with a NULL team_id it is not checked (MATCH SIMPLE), so org labels need
-- nothing. A team label dies with its team.
ALTER TABLE labels ADD COLUMN team_id uuid;
ALTER TABLE labels
    ADD CONSTRAINT labels_team_fk FOREIGN KEY (org_id, team_id) REFERENCES teams (org_id, id) ON DELETE CASCADE;

-- Names are unique per scope, ignoring case: among the org labels, and among
-- one team's labels. A team label MAY share a name with an org label or with
-- another team's label (decisions-T21-team-labels #3).
DROP INDEX labels_org_name_lower_key;
CREATE UNIQUE INDEX labels_org_name_lower_key ON labels (org_id, lower(name)) WHERE team_id IS NULL;
CREATE UNIQUE INDEX labels_team_name_lower_key ON labels (org_id, team_id, lower(name)) WHERE team_id IS NOT NULL;
