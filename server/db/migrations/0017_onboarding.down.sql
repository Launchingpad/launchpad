-- LAUN-56: reverses 0017. It loses the skips, the time zone, the default team
-- and the sign-up "chosen" marker, which is the nature of a down.
ALTER TABLE platform_settings DROP COLUMN signup_mode_chosen_at;
DROP TABLE onboarding_skips;
ALTER TABLE orgs DROP CONSTRAINT orgs_default_team_fk;
ALTER TABLE orgs DROP COLUMN default_team_id, DROP COLUMN time_zone;
