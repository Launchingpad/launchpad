ALTER TABLE user_view_preferences
    DROP CONSTRAINT user_view_preferences_hidden_check,
    DROP COLUMN hidden_columns,
    DROP COLUMN hide_empty;
