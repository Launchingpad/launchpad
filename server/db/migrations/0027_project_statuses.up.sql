-- LAUN-195 (decisions-T29): custom project statuses. A status has one of the six fixed KINDS (the old
-- projects.state values), a name, a colour and a place in the order. projects.state stays and is the kind
-- mirror; projects.status_id names the status, and the composite foreign key keeps the two equal.
CREATE TABLE project_statuses (
    id         uuid        NOT NULL DEFAULT uuidv7() PRIMARY KEY,
    org_id     uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    kind       text        NOT NULL
               CONSTRAINT project_statuses_kind_check
               CHECK (kind IN ('backlog', 'planned', 'started', 'paused', 'completed', 'canceled')),
    name       text        NOT NULL
               CONSTRAINT project_statuses_name_len CHECK (char_length(name) BETWEEN 1 AND 50),
    color      text        NOT NULL
               CONSTRAINT project_statuses_color_check CHECK (color ~ '^#[0-9a-fA-F]{6}$'),
    position   integer     NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT project_statuses_org_id_id_key UNIQUE (org_id, id),
    CONSTRAINT project_statuses_org_id_id_kind_key UNIQUE (org_id, id, kind)
);
CREATE UNIQUE INDEX project_statuses_org_name_key ON project_statuses (org_id, lower(name));
CREATE INDEX project_statuses_org_pos_idx ON project_statuses (org_id, position, id);

-- The six defaults: names as the web showed the old values.
CREATE FUNCTION seed_project_statuses(p_org uuid) RETURNS void
    LANGUAGE sql AS $$
    INSERT INTO project_statuses (org_id, kind, name, color, position) VALUES
        (p_org, 'backlog',   'Backlog',     '#bec2c8', 0),
        (p_org, 'planned',   'Planned',     '#e2e2e2', 1),
        (p_org, 'started',   'In progress', '#f2c94c', 2),
        (p_org, 'paused',    'Paused',      '#95a2b3', 3),
        (p_org, 'completed', 'Completed',   '#5e6ad2', 4),
        (p_org, 'canceled',  'Canceled',    '#95a2b3', 5)
    ON CONFLICT DO NOTHING
$$;

SELECT seed_project_statuses(id) FROM orgs;

-- Every path that makes an org (setup, sign-up, the importer, tests) gets the defaults.
CREATE FUNCTION project_statuses_for_new_org() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    PERFORM seed_project_statuses(NEW.id);
    RETURN NEW;
END
$$;
CREATE TRIGGER orgs_project_statuses AFTER INSERT ON orgs
    FOR EACH ROW EXECUTE FUNCTION project_statuses_for_new_org();

ALTER TABLE projects ADD COLUMN status_id uuid;
UPDATE projects p SET status_id = s.id
FROM project_statuses s WHERE s.org_id = p.org_id AND s.kind = p.state;
ALTER TABLE projects ALTER COLUMN status_id SET NOT NULL;
ALTER TABLE projects ADD CONSTRAINT projects_status_fk
    FOREIGN KEY (org_id, status_id, state) REFERENCES project_statuses (org_id, id, kind);
CREATE INDEX projects_status_idx ON projects (org_id, status_id);
