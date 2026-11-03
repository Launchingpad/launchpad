-- The restricted database role of the off-site backup worker (
-- decisions-T16 #15). deploy.sh pipes this file into
--   psql -X -q -v ON_ERROR_STOP=1 -U devops -d devops
-- on its standard input, AFTER a line `\set offsite_pw '<password>'`. The password is
-- never an argument and never printed. The file is idempotent: it runs on EVERY
-- deploy.sh run and update, and after a restore (pg_restore --no-owner
-- --no-privileges leaves a restored database with no grants).
--
-- It creates the role if it is missing, sets its password, REVOKES everything the
-- role may hold in this database and then grants exactly the list below, so a grant
-- that was added by hand disappears at the next run and a dropped one comes back.
--
-- The list (the worker "offsite" in server/internal/offsite; TestRestrictedRole runs
-- the whole flow with these grants and nothing more):
--   CONNECT on the database, USAGE on schema public
--   SELECT   backup_settings, backup_destinations, backup_dumps, backup_runs,
--            backup_requests, schema_migrations
--            (NOT backup_change_log: the worker only inserts there)
--   INSERT, UPDATE   backup_runs
--   INSERT           backup_requests, backup_change_log
--   UPDATE (secrets_enc, secrets_set)            backup_destinations
--   UPDATE (signing_public, signing_private_enc) backup_settings
-- No DELETE anywhere, no right on any other table (so none on users, issues or
-- platform_settings), no sequence, no CREATE. LISTEN needs no grant.
--
-- What the role does NOT protect: confidentiality. The worker reads every dump from
-- the backups volume, so it can read all data anyway. The role protects integrity
-- (a bug in the worker cannot write issues or users) and the mail secrets.

\if :{?offsite_pw}
\else
  -- With ON_ERROR_STOP this ends psql with exit status 3 before anything changes.
  DO $$ BEGIN RAISE EXCEPTION 'offsite-grants.sql: offsite_pw is not set (deploy.sh sends \set offsite_pw first)'; END $$;
\endif

-- One transaction: a failure (ON_ERROR_STOP ends psql) rolls back, so the role is never left half set.
BEGIN;

-- The role: no superuser, no create-db, no create-role, no replication, no bypass of
-- row security, and an inherit/connection setting that adds nothing.
SELECT 'CREATE ROLE offsite LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS'
 WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'offsite')
\gexec

-- Every run sets these again, so a hand edit of the attributes goes away too.
ALTER ROLE offsite WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
SELECT format('ALTER ROLE offsite WITH PASSWORD %L', :'offsite_pw')
\gexec

-- Membership in any other role would add rights the list does not name.
SELECT format('REVOKE %I FROM offsite', r.rolname)
  FROM pg_auth_members m
  JOIN pg_roles r ON r.oid = m.roleid
 WHERE m.member = (SELECT oid FROM pg_roles WHERE rolname = 'offsite')
\gexec

-- Start from nothing in this database ...
SELECT format('REVOKE ALL ON DATABASE %I FROM offsite', current_database())
\gexec
REVOKE ALL ON SCHEMA public FROM offsite;
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM offsite;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM offsite;
REVOKE ALL ON ALL FUNCTIONS IN SCHEMA public FROM offsite;

-- ... and give back exactly the list.
SELECT format('GRANT CONNECT ON DATABASE %I TO offsite', current_database())
\gexec
GRANT USAGE ON SCHEMA public TO offsite;
GRANT SELECT ON
  public.backup_settings, public.backup_destinations, public.backup_dumps,
  public.backup_runs, public.backup_requests, public.schema_migrations
  TO offsite;
GRANT INSERT, UPDATE ON public.backup_runs TO offsite;
GRANT INSERT ON public.backup_requests, public.backup_change_log TO offsite;
GRANT UPDATE (secrets_enc, secrets_set) ON public.backup_destinations TO offsite;
GRANT UPDATE (signing_public, signing_private_enc) ON public.backup_settings TO offsite;

COMMIT;
