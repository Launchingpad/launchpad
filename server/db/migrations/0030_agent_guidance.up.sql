-- LAUN-200 (T31, docs/internal/decisions-T31-agent-guidance.md): guidance text
-- an org admin writes for the agents that work in the org. One org-wide row
-- (team_id NULL) and at most one row per team. The text is data for an agent,
-- never a setting that changes what a token may do.

CREATE TABLE agent_guidance (
    id         uuid        NOT NULL PRIMARY KEY DEFAULT uuidv7(),
    org_id     uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    team_id    uuid,
    body       text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT agent_guidance_team_fk FOREIGN KEY (org_id, team_id)
        REFERENCES teams (org_id, id) ON DELETE CASCADE,
    CONSTRAINT agent_guidance_team_key UNIQUE (org_id, team_id),
    -- 8 KiB of UTF-8, and never empty (an empty text is a delete).
    CONSTRAINT agent_guidance_body_check CHECK (octet_length(body) BETWEEN 1 AND 8192)
);
-- A UNIQUE over a nullable column lets many NULLs through: one org-wide row.
CREATE UNIQUE INDEX agent_guidance_org_key ON agent_guidance (org_id) WHERE team_id IS NULL;
