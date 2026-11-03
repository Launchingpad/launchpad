-- 0028_project_updates: LAUN-196 (decisions-T30). A status update posted on a
-- project, with a health value. Create, list and read only in v1.
CREATE TABLE project_updates (
    id          uuid        PRIMARY KEY DEFAULT uuidv7(),
    org_id      uuid        NOT NULL,
    project_id  uuid        NOT NULL,
    author_id   uuid        REFERENCES users (id) ON DELETE SET NULL,
    health      text        NOT NULL,
    body        text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT project_updates_org_id_id_key UNIQUE (org_id, id),
    CONSTRAINT project_updates_project_fk FOREIGN KEY (org_id, project_id)
        REFERENCES projects (org_id, id) ON DELETE CASCADE,
    CONSTRAINT project_updates_health_check CHECK (health IN ('on_track', 'at_risk', 'off_track')),
    CONSTRAINT project_updates_body_len CHECK (char_length(body) <= 50000)
);
CREATE INDEX project_updates_project_idx ON project_updates (org_id, project_id, created_at DESC, id DESC);
CREATE INDEX project_updates_author_idx ON project_updates (author_id) WHERE author_id IS NOT NULL;
