-- LAUN-141: remembered issue-page views (docs/internal/decisions-T23-view-preferences.md).

-- name: ListViewPreferences :many
-- scope: own
SELECT place, view, sort, hide_empty, hidden_columns, group_by, changed_at FROM user_view_preferences
WHERE org_id = $1 AND user_id = $2 ORDER BY place;

-- name: GetViewPreference :one
-- scope: own
SELECT place, view, sort, hide_empty, hidden_columns, group_by, changed_at FROM user_view_preferences
WHERE org_id = $1 AND user_id = $2 AND place = $3;

-- name: UpsertViewPreference :one
-- scope: own
-- A write that is not strictly newer than the stored one returns no row.
INSERT INTO user_view_preferences (org_id, user_id, place, view, sort, hide_empty, hidden_columns, group_by, changed_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (org_id, user_id, place) DO UPDATE
SET view = EXCLUDED.view, sort = EXCLUDED.sort, hide_empty = EXCLUDED.hide_empty,
    hidden_columns = EXCLUDED.hidden_columns, group_by = EXCLUDED.group_by, changed_at = EXCLUDED.changed_at
WHERE EXCLUDED.changed_at > user_view_preferences.changed_at
RETURNING place, view, sort, hide_empty, hidden_columns, group_by, changed_at;

-- name: TrimViewPreferences :exec
-- scope: own
-- Keeps the newest $3 places of the person in the org.
DELETE FROM user_view_preferences v
WHERE v.org_id = $1 AND v.user_id = $2
  AND (v.org_id, v.user_id, v.place) IN (
    SELECT org_id, user_id, place FROM user_view_preferences
    WHERE org_id = $1 AND user_id = $2
    ORDER BY changed_at DESC, place OFFSET $3);
