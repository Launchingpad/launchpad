-- LAUN-209: reverses 0036. The team of each service is dropped; nothing else is
-- lost. (A service removed by the up because its org had no team is not
-- restored: it could not have been placed.)
DROP INDEX chat_services_team_idx;
ALTER TABLE chat_services DROP CONSTRAINT chat_services_team_fk;
ALTER TABLE chat_services DROP COLUMN team_id;
