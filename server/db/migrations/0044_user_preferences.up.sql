-- 0044_user_preferences: LAUN-235 (docs/internal/decisions-T42-themes.md). A
-- person's own display preferences that follow them across devices AND orgs:
-- today the theme only. One row per user, install-wide (a person has one
-- theme, not one per org), so the table has no org_id: it is not a tenant
-- table. No row means the defaults ('linear'). Own row only: no admin reads
-- it, no event names it. Agents have no row (the routes refuse them).
CREATE TABLE user_preferences (
    user_id    uuid        NOT NULL PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    theme      text        NOT NULL DEFAULT 'linear',
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT user_preferences_theme_check CHECK (theme IN ('linear', 'paper', 'terminal'))
);
