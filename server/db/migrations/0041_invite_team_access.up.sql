-- LAUN-226 (decisions-T20 #26 to #30, #34 M3): an invite carries the team
-- access its accept gives. Every existing invite is 'all' (today's meaning).
-- The rows of org_invite_teams count only when team_access = 'selected'. An
-- admin invite gives every team (#2, #30), so it is always 'all'.
ALTER TABLE org_invites
    ADD COLUMN team_access text NOT NULL DEFAULT 'all'
    CONSTRAINT org_invites_team_access_check CHECK (team_access IN ('all', 'selected')),
    ADD CONSTRAINT org_invites_admin_all_teams CHECK (role <> 'admin' OR team_access = 'all'),
    ADD CONSTRAINT org_invites_org_id_id_key UNIQUE (org_id, id);

-- Composite FKs (an array cannot hold one): a team of another org can never
-- be named, and a deleted team leaves the invite by the cascade (#27).
CREATE TABLE org_invite_teams (
    org_id    uuid NOT NULL,
    invite_id uuid NOT NULL,
    team_id   uuid NOT NULL,
    PRIMARY KEY (org_id, invite_id, team_id),
    CONSTRAINT org_invite_teams_invite_fk FOREIGN KEY (org_id, invite_id)
        REFERENCES org_invites (org_id, id) ON DELETE CASCADE,
    CONSTRAINT org_invite_teams_team_fk FOREIGN KEY (org_id, team_id)
        REFERENCES teams (org_id, id) ON DELETE CASCADE
);
CREATE INDEX org_invite_teams_team_idx ON org_invite_teams (org_id, team_id);
