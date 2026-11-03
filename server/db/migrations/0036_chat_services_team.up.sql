-- 0036_chat_services_team: LAUN-209 (decisions-T38-chat-service-team.md). Every
-- chat service belongs to one team of its org, and posts only the events of that
-- team's issues. Existing services (other installs) first get their org's
-- default team, else the org's oldest team, then the column is set NOT NULL.
-- A service of an org with no team at all cannot be placed and is removed (it
-- could not have posted: an issue event needs a team). Its outbox rows go with it.

ALTER TABLE chat_services ADD COLUMN team_id uuid;

UPDATE chat_services s SET team_id = COALESCE(
    (SELECT o.default_team_id FROM orgs o WHERE o.id = s.org_id),
    (SELECT t.id FROM teams t WHERE t.org_id = s.org_id ORDER BY t.created_at, t.id LIMIT 1));

DELETE FROM chat_services WHERE team_id IS NULL;

ALTER TABLE chat_services ALTER COLUMN team_id SET NOT NULL;
-- No ON DELETE action: the API refuses to delete a team that has services
-- (409 team_in_use); this is the backstop.
ALTER TABLE chat_services ADD CONSTRAINT chat_services_team_fk
    FOREIGN KEY (org_id, team_id) REFERENCES teams (org_id, id);
CREATE INDEX chat_services_team_idx ON chat_services (org_id, team_id);
