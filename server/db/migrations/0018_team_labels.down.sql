-- LAUN-54: reverses 0018. Team labels cannot exist in the old shape (one
-- name per org), so a down DELETES them (and, by cascade, their issue_labels
-- rows). Org labels are untouched.
DELETE FROM labels WHERE team_id IS NOT NULL;
DROP INDEX labels_team_name_lower_key;
DROP INDEX labels_org_name_lower_key;
CREATE UNIQUE INDEX labels_org_name_lower_key ON labels (org_id, lower(name));
ALTER TABLE labels DROP CONSTRAINT labels_team_fk;
ALTER TABLE labels DROP COLUMN team_id;
