-- 0003_planning: T3 (ORB-3407) projects, milestones, cycles, documents.
--
-- The baseline (0001) already has the planning tables and the team cycle
-- cadence (cycles_enabled, cycle_length_days, cycle_start_weekday,
-- cycle_start_time, cycle_timezone, cycle_rollover). This migration adds
-- only what the T3 API and the cycle job need:
--
-- 1. cycle_snapshots counts for every state type, so a burn-up can be drawn
--    without a second query (docs/decisions-T3.md "Snapshots").
-- 2. Keyset indexes for the new list endpoints, in the same
--    (org_id, sort_column, id) shape as the issue list.
-- 3. The index the cycle job uses to find ended cycles that are not yet
--    completed.

-- Points are sums of issue estimates, so they are bigint: an int4 sum can
-- overflow, and an overflow would roll back the whole cycle job for the
-- team (ORB-3407 review). Counts stay integer: they are bounded by the
-- number of issue rows.
ALTER TABLE cycle_snapshots
    ALTER COLUMN scope_points TYPE bigint,
    ALTER COLUMN completed_points TYPE bigint,
    ADD COLUMN triage_count     integer NOT NULL DEFAULT 0,
    ADD COLUMN backlog_count    integer NOT NULL DEFAULT 0,
    ADD COLUMN unstarted_count  integer NOT NULL DEFAULT 0,
    ADD COLUMN canceled_count   integer NOT NULL DEFAULT 0,
    ADD COLUMN started_points   bigint  NOT NULL DEFAULT 0,
    ADD CONSTRAINT cycle_snapshots_counts_check CHECK (
        scope_count >= 0 AND started_count >= 0 AND completed_count >= 0
        AND triage_count >= 0 AND backlog_count >= 0 AND unstarted_count >= 0
        AND canceled_count >= 0 AND scope_points >= 0 AND started_points >= 0
        AND completed_points >= 0
    );

-- GET /milestones and GET /cycles keyset pages. Projects and documents
-- already have (org_id, updated_at, id) from 0001.
CREATE INDEX IF NOT EXISTS milestones_org_updated_idx ON milestones (org_id, updated_at, id);
CREATE INDEX IF NOT EXISTS cycles_org_starts_idx ON cycles (org_id, starts_at, id);

-- The cycle job: "cycles of this team that have ended and are not
-- completed", in number order.
CREATE INDEX IF NOT EXISTS cycles_open_idx ON cycles (team_id, number) WHERE completed_at IS NULL;

-- The project version column (CAS) must stay >= 1, as issues' does.
ALTER TABLE projects ADD CONSTRAINT projects_version_check CHECK (version >= 1);
ALTER TABLE documents ADD CONSTRAINT documents_version_check CHECK (version >= 1);
ALTER TABLE cycles ADD CONSTRAINT cycles_name_len CHECK (name IS NULL OR char_length(name) BETWEEN 1 AND 255);
