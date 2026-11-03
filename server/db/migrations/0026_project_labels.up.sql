-- LAUN-194: project labels (docs/internal/decisions-T28-project-labels.md).
-- Org-wide labels for projects, like Linear's Settings > Projects > Labels.
-- parent_id makes a label a member of a group (one level; the service
-- enforces it, as the issue labels do not). A project carries any number of
-- labels through project_label_assignments.
CREATE TABLE project_labels (
    id          uuid        PRIMARY KEY DEFAULT uuidv7(),
    org_id      uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    name        text        NOT NULL,
    color       text        NOT NULL DEFAULT '#95a2b3',
    parent_id   uuid,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT project_labels_org_id_id_key UNIQUE (org_id, id),
    -- Deleting a group keeps its children, as top-level labels.
    CONSTRAINT project_labels_parent_fk FOREIGN KEY (org_id, parent_id)
        REFERENCES project_labels (org_id, id) ON DELETE SET NULL (parent_id),
    CONSTRAINT project_labels_name_len CHECK (char_length(name) BETWEEN 1 AND 100),
    CONSTRAINT project_labels_color_fmt CHECK (color ~ '^#[0-9a-fA-F]{6}$'),
    CONSTRAINT project_labels_not_own_parent CHECK (parent_id IS DISTINCT FROM id)
);
CREATE UNIQUE INDEX project_labels_org_name_lower_key ON project_labels (org_id, lower(name));
CREATE INDEX project_labels_parent_idx ON project_labels (org_id, parent_id) WHERE parent_id IS NOT NULL;

CREATE TABLE project_label_assignments (
    org_id      uuid NOT NULL,
    project_id  uuid NOT NULL,
    label_id    uuid NOT NULL,
    PRIMARY KEY (project_id, label_id),
    CONSTRAINT project_label_assignments_project_fk FOREIGN KEY (org_id, project_id)
        REFERENCES projects (org_id, id) ON DELETE CASCADE,
    CONSTRAINT project_label_assignments_label_fk FOREIGN KEY (org_id, label_id)
        REFERENCES project_labels (org_id, id) ON DELETE CASCADE
);
CREATE INDEX project_label_assignments_label_idx ON project_label_assignments (org_id, label_id);
