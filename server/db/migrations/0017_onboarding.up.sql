-- LAUN-56 (docs/internal/decisions-T17-onboarding.md): the first-run checklist on
-- the server.
--   1. orgs: a time zone and a default team (the org settings of PATCH /orgs/{org}).
--   2. onboarding_skips: a step an org admin skipped, shared by all admins of the org.
--   3. platform_settings.signup_mode_chosen_at: set when a platform admin changes the
--      sign-up mode or confirms it, never by another save.
ALTER TABLE orgs
    ADD COLUMN time_zone text NOT NULL DEFAULT 'UTC' CHECK (char_length(time_zone) BETWEEN 1 AND 64),
    ADD COLUMN default_team_id uuid;

-- Composite, so the default team is always a team of the same org. Deleting the
-- team clears only the default (column list: PostgreSQL 15).
ALTER TABLE orgs
    ADD CONSTRAINT orgs_default_team_fk FOREIGN KEY (id, default_team_id)
    REFERENCES teams (org_id, id) ON DELETE SET NULL (default_team_id);

CREATE TABLE onboarding_skips (
    org_id     uuid NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    step       text NOT NULL CHECK (step IN
        ('organisation', 'mail', 'signups', 'team', 'agent', 'import', 'githost', 'invite')),
    skipped_by uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, step)
);

ALTER TABLE platform_settings ADD COLUMN signup_mode_chosen_at timestamptz;
-- An install that left the default has chosen.
UPDATE platform_settings SET signup_mode_chosen_at = updated_at WHERE signup_mode <> 'invite';
