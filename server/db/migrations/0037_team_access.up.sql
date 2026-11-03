-- LAUN-215 (T20 #1, #34 M1): team-scoped access for members. Nothing reads
-- this yet, so no member sees a change. Every existing member is 'all'.
-- The rows of team_members count only when team_access = 'selected'.
ALTER TABLE org_members
    ADD COLUMN team_access text NOT NULL DEFAULT 'all'
    CONSTRAINT org_members_team_access_check CHECK (team_access IN ('all', 'selected'));

CREATE TABLE team_members (
    org_id     uuid        NOT NULL,
    user_id    uuid        NOT NULL,
    team_id    uuid        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id, team_id),
    CONSTRAINT team_members_member_fk FOREIGN KEY (org_id, user_id)
        REFERENCES org_members (org_id, user_id) ON DELETE CASCADE,
    CONSTRAINT team_members_team_fk FOREIGN KEY (org_id, team_id)
        REFERENCES teams (org_id, id) ON DELETE CASCADE
);
CREATE INDEX team_members_team_idx ON team_members (org_id, team_id);
