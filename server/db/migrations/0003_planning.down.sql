-- Reverses 0003_planning. The extra snapshot counts are lost; the columns
-- 0001 created (scope, started, completed, points) keep their values.
ALTER TABLE cycles DROP CONSTRAINT IF EXISTS cycles_name_len;
ALTER TABLE documents DROP CONSTRAINT IF EXISTS documents_version_check;
ALTER TABLE projects DROP CONSTRAINT IF EXISTS projects_version_check;

DROP INDEX IF EXISTS cycles_open_idx;
DROP INDEX IF EXISTS cycles_org_starts_idx;
DROP INDEX IF EXISTS milestones_org_updated_idx;

ALTER TABLE cycle_snapshots
    DROP CONSTRAINT IF EXISTS cycle_snapshots_counts_check,
    DROP COLUMN IF EXISTS started_points,
    DROP COLUMN IF EXISTS canceled_count,
    DROP COLUMN IF EXISTS unstarted_count,
    DROP COLUMN IF EXISTS backlog_count,
    DROP COLUMN IF EXISTS triage_count,
    -- Back to 0001's integer. A sum above the int4 range is clamped: the
    -- only loss, and only for a snapshot that 0001 could not have stored.
    ALTER COLUMN scope_points TYPE integer USING LEAST(scope_points, 2147483647)::integer,
    ALTER COLUMN completed_points TYPE integer USING LEAST(completed_points, 2147483647)::integer;
