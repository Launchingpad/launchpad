-- Per-issue activity (LAUN-49, docs/internal/decisions-T15-activity.md).
-- One row per recorded change of an issue that issue_state_changes does not
-- already hold: field edits, label adds and removes, relation removals.
-- "from" and "to" are small JSON snapshots made at write time (an id and a
-- display name, or a plain value), so a later rename does not rewrite
-- history. A description edit stores neither (the text can be 200k).
CREATE TABLE issue_activity (
    id         uuid        PRIMARY KEY DEFAULT uuidv7(),
    org_id     uuid        NOT NULL,
    issue_id   uuid        NOT NULL,
    actor_id   uuid        REFERENCES users (id) ON DELETE SET NULL,
    source     text        NOT NULL DEFAULT 'api'
               CONSTRAINT issue_activity_source_check CHECK (source IN ('api', 'gitlab', 'import', 'system')),
    kind       text        NOT NULL
               CONSTRAINT issue_activity_kind_check CHECK (kind IN (
                   'title', 'description', 'assignee', 'priority', 'estimate', 'dueDate',
                   'parent', 'project', 'milestone', 'cycle',
                   'labelAdded', 'labelRemoved', 'relationRemoved')),
    "from"     jsonb,
    "to"       jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT issue_activity_issue_fk FOREIGN KEY (org_id, issue_id)
        REFERENCES issues (org_id, id) ON DELETE CASCADE
);

-- Serves the keyset page of GET /issues/{issue}/activity (created_at, id).
CREATE INDEX issue_activity_issue_idx ON issue_activity (org_id, issue_id, created_at, id);
CREATE INDEX issue_activity_actor_idx ON issue_activity (actor_id) WHERE actor_id IS NOT NULL;
