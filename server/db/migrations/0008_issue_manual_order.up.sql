-- Manual issue order (LAUN-51, docs/internal/decisions-T14-manual-order.md).
-- issues.sort_order exists since 0001 (double precision NOT NULL DEFAULT 0;
-- the importer fills it from Linear). This migration gives it a meaning and
-- an index. A "column" is (org_id, team_id, state_id); ranks ascend; a
-- smaller rank is higher up.

-- Backfill: renumber every column 1024, 2048, ... Imported issues keep the
-- relative order of their Linear sortOrder. Issues with equal ranks (all the
-- ones made here so far have 0) go newest first, as a new issue goes to the
-- top of its column. Id breaks the last tie, so the result is stable.
UPDATE issues i SET sort_order = r.n * 1024.0
FROM (
    SELECT id,
           row_number() OVER (PARTITION BY org_id, team_id, state_id
                              ORDER BY sort_order, created_at DESC, id) AS n
    FROM issues
) r
WHERE i.id = r.id;

-- Serves sort=manual (keyset on sort_order, id) and the neighbour lookups of
-- a placement. Archived rows are in it: the order of a column does not
-- depend on whether a reader shows them.
CREATE INDEX issues_manual_order_idx ON issues (org_id, team_id, state_id, sort_order, id);
