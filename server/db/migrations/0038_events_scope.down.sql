-- LAUN-221: reverses 0038. The scope of every event is lost; an older image
-- shows the whole feed to every member, which is correct only while no
-- member is restricted (run the rollback steps of T20 #35 first).
ALTER TABLE events DROP CONSTRAINT events_scope_teams_check;
ALTER TABLE events DROP COLUMN team_ids;
ALTER TABLE events DROP COLUMN scope;
