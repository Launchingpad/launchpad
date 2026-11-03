-- LAUN-221 (T20 #53, #55, #34 M2): every event names who may read it. scope
-- is 'any' (a viewer who sees one of team_ids), 'all' (a viewer who sees every
-- one of team_ids: a relation between two issues), 'org' (every member),
-- 'admin' (org admins only) or 'legacy' (made before this migration, or by an
-- older image after a rollback). A legacy row is shown to unrestricted
-- viewers only (fail closed); the job of #57 (ticket 6b) places old rows. No
-- backfill here: the columns have constant defaults, so this is a metadata
-- write on Postgres 16 (the CHECKs scan the table once).
ALTER TABLE events
    ADD COLUMN scope text NOT NULL DEFAULT 'legacy'
        CONSTRAINT events_scope_check CHECK (scope IN ('legacy', 'any', 'all', 'org', 'admin')),
    ADD COLUMN team_ids uuid[] NOT NULL DEFAULT '{}',
    -- A team scope with no team would match nobody ('any') or everybody
    -- ('all': the empty set is a subset of every set). Neither is allowed.
    ADD CONSTRAINT events_scope_teams_check CHECK (scope NOT IN ('any', 'all') OR cardinality(team_ids) > 0);
