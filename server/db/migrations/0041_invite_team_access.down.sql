-- LAUN-226: reverses 0041, but FAILS while an open invite names teams
-- (decisions-T20 #34 M3, #35 step 2). Without the column an older image would
-- read such an invite as an all-teams invite. This down never deletes or
-- changes a row by itself: revoke those invites first (docs/deploy.md,
-- "Rollback past team access", step 2), then run it again. Closed invites
-- (accepted or revoked) lose their team list; they can never be used again.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM org_invites
               WHERE team_access = 'selected' AND accepted_at IS NULL AND revoked_at IS NULL) THEN
        RAISE EXCEPTION 'open invites with team_access = selected exist; revoke them first (docs/deploy.md, Rollback past team access, step 2)';
    END IF;
END
$$;
DROP TABLE org_invite_teams;
ALTER TABLE org_invites
    DROP CONSTRAINT org_invites_org_id_id_key,
    DROP CONSTRAINT org_invites_admin_all_teams,
    DROP COLUMN team_access;
