-- LAUN-215: reverses 0037. The team rows and the flag are dropped. A member
-- who was 'selected' becomes unrestricted again (the older image has no such
-- notion), so run the rollback steps of T20 #35 first.
DROP TABLE team_members;
ALTER TABLE org_members DROP COLUMN team_access;
