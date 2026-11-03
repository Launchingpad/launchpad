-- Reverses 0006_platform_settings. The stored mail settings and the sign-up
-- mode are lost: a server without this migration sends login email from the
-- environment only (POSTMARK_* or the dev log), as before. After a new up,
-- a platform admin enters the settings (and the secrets) again.
DROP TABLE IF EXISTS platform_settings;
