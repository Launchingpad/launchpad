-- LAUN-163: board column choices per place, beside the layout and the sort (decisions-T23, "LAUN-163").
-- Additive: an old row keeps showing every column (hide_empty false, no hidden columns).
ALTER TABLE user_view_preferences
    ADD COLUMN hide_empty     boolean NOT NULL DEFAULT false,
    ADD COLUMN hidden_columns text[]  NOT NULL DEFAULT '{}',
    ADD CONSTRAINT user_view_preferences_hidden_check CHECK (
        cardinality(hidden_columns) <= 30 AND octet_length(array_to_string(hidden_columns, ',')) <= 12100);
