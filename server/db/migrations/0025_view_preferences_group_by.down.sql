ALTER TABLE user_view_preferences
    DROP CONSTRAINT user_view_preferences_group_by_check,
    DROP COLUMN group_by;
