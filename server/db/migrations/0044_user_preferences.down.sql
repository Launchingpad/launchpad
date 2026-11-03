-- LAUN-235: reverses 0044. Every person's theme choice is lost; an older image
-- has one theme only, and the browser keeps its own copy until the next load.
DROP TABLE user_preferences;
