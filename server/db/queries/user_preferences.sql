-- LAUN-235: a person's own display preferences (docs/internal/decisions-T42-themes.md).
-- user_preferences has no org_id (install-wide, per user), so these name no tenant table.

-- name: GetUserPreferences :one
-- scope: none
SELECT theme FROM user_preferences WHERE user_id = $1;

-- name: UpsertUserTheme :one
-- scope: none
INSERT INTO user_preferences (user_id, theme) VALUES ($1, $2)
ON CONFLICT (user_id) DO UPDATE SET theme = EXCLUDED.theme, updated_at = now()
RETURNING theme;
