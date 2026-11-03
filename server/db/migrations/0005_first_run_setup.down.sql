-- Reverses 0005_first_run_setup. Open setup links are lost (a restart of a
-- server without the migration has no setup step anyway), and so is the
-- platform-admin flag: an operator who rolls back loses nothing else, and
-- re-granting after a new up is `server grant-platform-admin`.
DROP TABLE IF EXISTS setup_tokens;
ALTER TABLE users DROP COLUMN IF EXISTS is_platform_admin;
