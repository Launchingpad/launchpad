-- LAUN-171: the board's group-by field per place, beside the layout, the sort and the column choices
-- (decisions-T23, "LAUN-171"). Additive: an old row keeps grouping by state.
ALTER TABLE user_view_preferences
    ADD COLUMN group_by text NOT NULL DEFAULT 'state',
    ADD CONSTRAINT user_view_preferences_group_by_check
        CHECK (group_by IN ('state', 'assignee', 'priority', 'label', 'project'));
