-- 0001_baseline: the full dev-ops schema (ORB-3404 / ORB-3405).
--
-- The whole baseline ships at once because the Linear importer (T4) needs
-- every table. Tranches after T1 add handlers, not tables; any later schema
-- change is a NEW numbered migration. Never edit this file once it ships.
--
-- Tenancy is application-level, not RLS. Every tenant table carries org_id.
-- Every org-scoped table also has UNIQUE (org_id, id), and references between
-- org-scoped rows are COMPOSITE foreign keys on (org_id, x_id). A row can then
-- never point at a row in another org, even if application code has a bug.
-- Three tables are not org-scoped by design: orgs, users (a person can belong
-- to several orgs through org_members) and api_tokens (owned by a user; the
-- optional org_id is a pin, not an owner).

-- uuidv7(): a time-ordered UUID (RFC 9562 version 7) for every primary key.
-- Postgres 16 has no built-in one. It takes 6 bytes of epoch milliseconds and
-- the random bits of gen_random_uuid() (built in, no pgcrypto needed). There
-- is NO monotonic counter: two ids made in the same millisecond sort randomly.
-- Never use ORDER BY id to mean "insertion order". Postgres 18 ships a
-- pg_catalog.uuidv7() that shadows this one with no migration.
CREATE FUNCTION uuidv7() RETURNS uuid
    LANGUAGE plpgsql VOLATILE
    AS $$
DECLARE
    b  bytea  := uuid_send(gen_random_uuid());
    ts bigint := (extract(epoch FROM clock_timestamp()) * 1000)::bigint;
BEGIN
    b := overlay(b PLACING substring(int8send(ts) FROM 3 FOR 6) FROM 1 FOR 6);
    -- version 7 in the high nibble of byte 6; the variant bits stay 10xx.
    b := set_byte(b, 6, (get_byte(b, 6) & 15) | 112);
    RETURN encode(b, 'hex')::uuid;
END;
$$;

-- ---------------------------------------------------------------------------
-- Identity
-- ---------------------------------------------------------------------------

CREATE TABLE orgs (
    id          uuid        PRIMARY KEY DEFAULT uuidv7(),
    slug        text        NOT NULL,
    name        text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT orgs_slug_key UNIQUE (slug),
    CONSTRAINT orgs_slug_format CHECK (slug ~ '^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$'),
    CONSTRAINT orgs_name_len CHECK (char_length(name) BETWEEN 1 AND 200)
);

CREATE TABLE users (
    id              uuid        PRIMARY KEY DEFAULT uuidv7(),
    username        text        NOT NULL,
    name            text        NOT NULL,
    email           text,
    is_agent        boolean     NOT NULL DEFAULT false,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    deactivated_at  timestamptz,
    CONSTRAINT users_username_key UNIQUE (username),
    CONSTRAINT users_username_format CHECK (username ~ '^[a-z][a-z0-9_-]{0,38}$'),
    CONSTRAINT users_name_len CHECK (char_length(name) BETWEEN 1 AND 200)
);
-- Email is the OTP login key (T6). Case-insensitive, unique when present.
CREATE UNIQUE INDEX users_email_lower_key ON users (lower(email)) WHERE email IS NOT NULL;

CREATE TABLE org_members (
    org_id      uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    user_id     uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role        text        NOT NULL DEFAULT 'member',
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, user_id),
    CONSTRAINT org_members_role_check CHECK (role IN ('admin', 'member'))
);
CREATE INDEX org_members_user_id_idx ON org_members (user_id);

-- api_tokens: `odt_` + 43 base62 characters. Only the sha256 is stored, plus
-- the first 12 characters for listing. kind 'session' is the web cookie
-- session (T6); 'pat' is a bearer token for the MCP and native apps.
CREATE TABLE api_tokens (
    id            uuid        PRIMARY KEY DEFAULT uuidv7(),
    user_id       uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    org_id        uuid        REFERENCES orgs (id) ON DELETE CASCADE,
    kind          text        NOT NULL DEFAULT 'pat',
    name          text        NOT NULL,
    token_hash    bytea       NOT NULL,
    prefix        text        NOT NULL,
    scopes        text[]      NOT NULL,
    created_by    uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz,
    last_used_at  timestamptz,
    revoked_at    timestamptz,
    CONSTRAINT api_tokens_token_hash_key UNIQUE (token_hash),
    CONSTRAINT api_tokens_kind_check CHECK (kind IN ('pat', 'session')),
    CONSTRAINT api_tokens_hash_len CHECK (octet_length(token_hash) = 32),
    CONSTRAINT api_tokens_prefix_len CHECK (char_length(prefix) = 12),
    CONSTRAINT api_tokens_scopes_check CHECK (
        cardinality(scopes) >= 1 AND scopes <@ ARRAY['read', 'write', 'admin']::text[]
    ),
    CONSTRAINT api_tokens_name_len CHECK (char_length(name) BETWEEN 1 AND 200)
);
CREATE INDEX api_tokens_user_id_idx ON api_tokens (user_id);
CREATE INDEX api_tokens_org_id_idx ON api_tokens (org_id) WHERE org_id IS NOT NULL;
CREATE INDEX api_tokens_created_by_idx ON api_tokens (created_by) WHERE created_by IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Teams and workflow
-- ---------------------------------------------------------------------------

CREATE TABLE teams (
    id                   uuid        PRIMARY KEY DEFAULT uuidv7(),
    org_id               uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    key                  text        NOT NULL,
    name                 text        NOT NULL,
    description          text        NOT NULL DEFAULT '',
    -- Workflow anchors. FKs are added after workflow_states exists.
    default_state_id     uuid,
    review_state_id      uuid,
    testing_state_id     uuid,
    -- Cycle cadence (T3). Orbit: 14 days from Sunday 00:00 Europe/London.
    cycles_enabled       boolean     NOT NULL DEFAULT false,
    cycle_length_days    integer     NOT NULL DEFAULT 14,
    cycle_start_weekday  smallint    NOT NULL DEFAULT 0,
    cycle_start_time     time        NOT NULL DEFAULT '00:00',
    cycle_timezone       text        NOT NULL DEFAULT 'Europe/London',
    cycle_rollover       boolean     NOT NULL DEFAULT true,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    archived_at          timestamptz,
    CONSTRAINT teams_org_id_id_key UNIQUE (org_id, id),
    CONSTRAINT teams_org_id_key_key UNIQUE (org_id, key),
    CONSTRAINT teams_key_format CHECK (key ~ '^[A-Z][A-Z0-9]{0,9}$'),
    CONSTRAINT teams_name_len CHECK (char_length(name) BETWEEN 1 AND 200),
    CONSTRAINT teams_cycle_length_check CHECK (cycle_length_days BETWEEN 1 AND 56),
    CONSTRAINT teams_cycle_weekday_check CHECK (cycle_start_weekday BETWEEN 0 AND 6)
);

-- Issue numbers: UPDATE ... SET next_number = next_number + 1 RETURNING
-- next_number - 1, inside the issue INSERT transaction. The row lock
-- serialises concurrent creates per team, so numbers never repeat.
CREATE TABLE issue_counters (
    team_id      uuid    PRIMARY KEY,
    org_id       uuid    NOT NULL,
    next_number  integer NOT NULL DEFAULT 1,
    CONSTRAINT issue_counters_team_fk FOREIGN KEY (org_id, team_id)
        REFERENCES teams (org_id, id) ON DELETE CASCADE,
    CONSTRAINT issue_counters_next_number_check CHECK (next_number >= 1)
);
CREATE INDEX issue_counters_org_team_idx ON issue_counters (org_id, team_id);

CREATE TABLE workflow_states (
    id           uuid             PRIMARY KEY DEFAULT uuidv7(),
    org_id       uuid             NOT NULL,
    team_id      uuid             NOT NULL,
    name         text             NOT NULL,
    type         text             NOT NULL,
    color        text             NOT NULL DEFAULT '#95a2b3',
    description  text             NOT NULL DEFAULT '',
    position     double precision NOT NULL DEFAULT 0,
    created_at   timestamptz      NOT NULL DEFAULT now(),
    archived_at  timestamptz,
    CONSTRAINT workflow_states_org_id_id_key UNIQUE (org_id, id),
    CONSTRAINT workflow_states_team_fk FOREIGN KEY (org_id, team_id)
        REFERENCES teams (org_id, id) ON DELETE CASCADE,
    CONSTRAINT workflow_states_team_name_key UNIQUE (team_id, name),
    -- Target of the (team_id, state_id) foreign keys: a state reference can
    -- only name a state of the same team.
    CONSTRAINT workflow_states_team_id_id_key UNIQUE (team_id, id),
    CONSTRAINT workflow_states_type_check CHECK (
        type IN ('triage', 'backlog', 'unstarted', 'started', 'completed', 'canceled')
    ),
    CONSTRAINT workflow_states_name_len CHECK (char_length(name) BETWEEN 1 AND 100)
);
CREATE INDEX workflow_states_org_team_idx ON workflow_states (org_id, team_id);

-- A team's anchor states must be states OF THAT TEAM: (id, x_state_id)
-- references workflow_states (team_id, id).
ALTER TABLE teams
    ADD CONSTRAINT teams_default_state_fk FOREIGN KEY (id, default_state_id)
        REFERENCES workflow_states (team_id, id) ON DELETE SET NULL (default_state_id),
    ADD CONSTRAINT teams_review_state_fk FOREIGN KEY (id, review_state_id)
        REFERENCES workflow_states (team_id, id) ON DELETE SET NULL (review_state_id),
    ADD CONSTRAINT teams_testing_state_fk FOREIGN KEY (id, testing_state_id)
        REFERENCES workflow_states (team_id, id) ON DELETE SET NULL (testing_state_id);
CREATE INDEX teams_default_state_idx ON teams (default_state_id) WHERE default_state_id IS NOT NULL;
CREATE INDEX teams_review_state_idx ON teams (review_state_id) WHERE review_state_id IS NOT NULL;
CREATE INDEX teams_testing_state_idx ON teams (testing_state_id) WHERE testing_state_id IS NOT NULL;

-- Labels are per org (Linear workspace labels). parent_id is a label group.
CREATE TABLE labels (
    id           uuid        PRIMARY KEY DEFAULT uuidv7(),
    org_id       uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    name         text        NOT NULL,
    color        text        NOT NULL DEFAULT '#95a2b3',
    description  text        NOT NULL DEFAULT '',
    parent_id    uuid,
    created_at   timestamptz NOT NULL DEFAULT now(),
    archived_at  timestamptz,
    CONSTRAINT labels_org_id_id_key UNIQUE (org_id, id),
    CONSTRAINT labels_parent_fk FOREIGN KEY (org_id, parent_id)
        REFERENCES labels (org_id, id) ON DELETE SET NULL (parent_id),
    CONSTRAINT labels_name_len CHECK (char_length(name) BETWEEN 1 AND 100)
);
CREATE UNIQUE INDEX labels_org_name_lower_key ON labels (org_id, lower(name));
CREATE INDEX labels_parent_idx ON labels (org_id, parent_id) WHERE parent_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Planning objects (handlers arrive in T3)
-- ---------------------------------------------------------------------------

CREATE TABLE projects (
    id            uuid        PRIMARY KEY DEFAULT uuidv7(),
    org_id        uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    name          text        NOT NULL,
    description   text        NOT NULL DEFAULT '',
    content       text        NOT NULL DEFAULT '',
    state         text        NOT NULL DEFAULT 'planned',
    lead_id       uuid        REFERENCES users (id) ON DELETE SET NULL,
    start_date    date,
    target_date   date,
    version       integer     NOT NULL DEFAULT 1,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    started_at    timestamptz,
    completed_at  timestamptz,
    canceled_at   timestamptz,
    archived_at   timestamptz,
    CONSTRAINT projects_org_id_id_key UNIQUE (org_id, id),
    CONSTRAINT projects_state_check CHECK (
        state IN ('backlog', 'planned', 'started', 'paused', 'completed', 'canceled')
    ),
    CONSTRAINT projects_name_len CHECK (char_length(name) BETWEEN 1 AND 255)
);
CREATE INDEX projects_org_updated_idx ON projects (org_id, updated_at, id);
CREATE INDEX projects_lead_idx ON projects (lead_id) WHERE lead_id IS NOT NULL;

CREATE TABLE project_teams (
    org_id      uuid NOT NULL,
    project_id  uuid NOT NULL,
    team_id     uuid NOT NULL,
    PRIMARY KEY (project_id, team_id),
    CONSTRAINT project_teams_project_fk FOREIGN KEY (org_id, project_id)
        REFERENCES projects (org_id, id) ON DELETE CASCADE,
    CONSTRAINT project_teams_team_fk FOREIGN KEY (org_id, team_id)
        REFERENCES teams (org_id, id) ON DELETE CASCADE
);
CREATE INDEX project_teams_team_idx ON project_teams (org_id, team_id);
CREATE INDEX project_teams_org_project_idx ON project_teams (org_id, project_id);

CREATE TABLE milestones (
    id           uuid             PRIMARY KEY DEFAULT uuidv7(),
    org_id       uuid             NOT NULL,
    project_id   uuid             NOT NULL,
    name         text             NOT NULL,
    description  text             NOT NULL DEFAULT '',
    target_date  date,
    sort_order   double precision NOT NULL DEFAULT 0,
    created_at   timestamptz      NOT NULL DEFAULT now(),
    updated_at   timestamptz      NOT NULL DEFAULT now(),
    archived_at  timestamptz,
    CONSTRAINT milestones_org_id_id_key UNIQUE (org_id, id),
    CONSTRAINT milestones_project_fk FOREIGN KEY (org_id, project_id)
        REFERENCES projects (org_id, id) ON DELETE CASCADE,
    CONSTRAINT milestones_name_len CHECK (char_length(name) BETWEEN 1 AND 255)
);
CREATE INDEX milestones_project_idx ON milestones (org_id, project_id);

CREATE TABLE cycles (
    id            uuid        PRIMARY KEY DEFAULT uuidv7(),
    org_id        uuid        NOT NULL,
    team_id       uuid        NOT NULL,
    number        integer     NOT NULL,
    name          text,
    description   text        NOT NULL DEFAULT '',
    starts_at     timestamptz NOT NULL,
    ends_at       timestamptz NOT NULL,
    completed_at  timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    archived_at   timestamptz,
    CONSTRAINT cycles_org_id_id_key UNIQUE (org_id, id),
    CONSTRAINT cycles_team_fk FOREIGN KEY (org_id, team_id)
        REFERENCES teams (org_id, id) ON DELETE CASCADE,
    CONSTRAINT cycles_team_number_key UNIQUE (team_id, number),
    CONSTRAINT cycles_number_check CHECK (number >= 1),
    CONSTRAINT cycles_range_check CHECK (ends_at > starts_at)
);
CREATE INDEX cycles_team_range_idx ON cycles (org_id, team_id, starts_at, ends_at);

CREATE TABLE documents (
    id           uuid        PRIMARY KEY DEFAULT uuidv7(),
    org_id       uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    project_id   uuid,
    title        text        NOT NULL,
    content      text        NOT NULL DEFAULT '',
    slug         text        NOT NULL,
    creator_id   uuid        REFERENCES users (id) ON DELETE SET NULL,
    version      integer     NOT NULL DEFAULT 1,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    archived_at  timestamptz,
    CONSTRAINT documents_org_id_id_key UNIQUE (org_id, id),
    CONSTRAINT documents_project_fk FOREIGN KEY (org_id, project_id)
        REFERENCES projects (org_id, id) ON DELETE SET NULL (project_id),
    CONSTRAINT documents_org_slug_key UNIQUE (org_id, slug),
    CONSTRAINT documents_title_len CHECK (char_length(title) BETWEEN 1 AND 255)
);
CREATE INDEX documents_project_idx ON documents (org_id, project_id) WHERE project_id IS NOT NULL;
CREATE INDEX documents_org_updated_idx ON documents (org_id, updated_at, id);
CREATE INDEX documents_creator_idx ON documents (creator_id) WHERE creator_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- Issues
-- ---------------------------------------------------------------------------

-- The identifier (team.key || '-' || number) and gitBranchName are computed
-- on read and never stored. priority: 0 none, 1 urgent, 2 high, 3 medium,
-- 4 low (Linear's scale). version is the optional compare-and-swap counter.
CREATE TABLE issues (
    id            uuid             PRIMARY KEY DEFAULT uuidv7(),
    org_id        uuid             NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    team_id       uuid             NOT NULL,
    number        integer          NOT NULL,
    title         text             NOT NULL,
    description   text             NOT NULL DEFAULT '',
    state_id      uuid             NOT NULL,
    priority      smallint         NOT NULL DEFAULT 0,
    estimate      integer,
    assignee_id   uuid             REFERENCES users (id) ON DELETE SET NULL,
    creator_id    uuid             REFERENCES users (id) ON DELETE SET NULL,
    parent_id     uuid,
    project_id    uuid,
    milestone_id  uuid,
    cycle_id      uuid,
    due_date      date,
    sort_order    double precision NOT NULL DEFAULT 0,
    version       integer          NOT NULL DEFAULT 1,
    created_at    timestamptz      NOT NULL DEFAULT now(),
    updated_at    timestamptz      NOT NULL DEFAULT now(),
    started_at    timestamptz,
    completed_at  timestamptz,
    canceled_at   timestamptz,
    archived_at   timestamptz,
    CONSTRAINT issues_org_id_id_key UNIQUE (org_id, id),
    CONSTRAINT issues_team_number_key UNIQUE (team_id, number),
    CONSTRAINT issues_team_fk FOREIGN KEY (org_id, team_id)
        REFERENCES teams (org_id, id) ON DELETE CASCADE,
    -- The state must belong to the issue's team.
    CONSTRAINT issues_state_fk FOREIGN KEY (team_id, state_id)
        REFERENCES workflow_states (team_id, id),
    CONSTRAINT issues_parent_fk FOREIGN KEY (org_id, parent_id)
        REFERENCES issues (org_id, id) ON DELETE SET NULL (parent_id),
    CONSTRAINT issues_project_fk FOREIGN KEY (org_id, project_id)
        REFERENCES projects (org_id, id) ON DELETE SET NULL (project_id),
    CONSTRAINT issues_milestone_fk FOREIGN KEY (org_id, milestone_id)
        REFERENCES milestones (org_id, id) ON DELETE SET NULL (milestone_id),
    CONSTRAINT issues_cycle_fk FOREIGN KEY (org_id, cycle_id)
        REFERENCES cycles (org_id, id) ON DELETE SET NULL (cycle_id),
    CONSTRAINT issues_number_check CHECK (number >= 1),
    CONSTRAINT issues_priority_check CHECK (priority BETWEEN 0 AND 4),
    CONSTRAINT issues_estimate_check CHECK (estimate IS NULL OR estimate >= 0),
    CONSTRAINT issues_not_own_parent CHECK (parent_id IS NULL OR parent_id <> id),
    CONSTRAINT issues_title_len CHECK (char_length(title) BETWEEN 1 AND 500),
    CONSTRAINT issues_version_check CHECK (version >= 1)
);
-- Keyset pagination for the two orderBy values (scanned backwards for DESC).
CREATE INDEX issues_org_updated_idx ON issues (org_id, updated_at, id);
CREATE INDEX issues_org_created_idx ON issues (org_id, created_at, id);
-- Foreign keys and the list filters.
CREATE INDEX issues_state_idx ON issues (org_id, state_id);
CREATE INDEX issues_team_state_idx ON issues (team_id, state_id);
CREATE INDEX issues_assignee_idx ON issues (assignee_id) WHERE assignee_id IS NOT NULL;
CREATE INDEX issues_creator_idx ON issues (creator_id) WHERE creator_id IS NOT NULL;
CREATE INDEX issues_parent_idx ON issues (org_id, parent_id) WHERE parent_id IS NOT NULL;
CREATE INDEX issues_project_idx ON issues (org_id, project_id) WHERE project_id IS NOT NULL;
CREATE INDEX issues_milestone_idx ON issues (org_id, milestone_id) WHERE milestone_id IS NOT NULL;
CREATE INDEX issues_cycle_idx ON issues (org_id, cycle_id) WHERE cycle_id IS NOT NULL;

CREATE TABLE issue_labels (
    org_id      uuid        NOT NULL,
    issue_id    uuid        NOT NULL,
    label_id    uuid        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (issue_id, label_id),
    CONSTRAINT issue_labels_issue_fk FOREIGN KEY (org_id, issue_id)
        REFERENCES issues (org_id, id) ON DELETE CASCADE,
    CONSTRAINT issue_labels_label_fk FOREIGN KEY (org_id, label_id)
        REFERENCES labels (org_id, id) ON DELETE CASCADE
);
CREATE INDEX issue_labels_label_idx ON issue_labels (org_id, label_id);
CREATE INDEX issue_labels_org_issue_idx ON issue_labels (org_id, issue_id);

-- One row per directed relation. blockedBy is the reverse read of 'blocks';
-- 'related' is read in both directions.
CREATE TABLE issue_relations (
    id                uuid        PRIMARY KEY DEFAULT uuidv7(),
    org_id            uuid        NOT NULL,
    type              text        NOT NULL,
    issue_id          uuid        NOT NULL,
    related_issue_id  uuid        NOT NULL,
    created_by        uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT issue_relations_org_id_id_key UNIQUE (org_id, id),
    CONSTRAINT issue_relations_issue_fk FOREIGN KEY (org_id, issue_id)
        REFERENCES issues (org_id, id) ON DELETE CASCADE,
    CONSTRAINT issue_relations_related_fk FOREIGN KEY (org_id, related_issue_id)
        REFERENCES issues (org_id, id) ON DELETE CASCADE,
    CONSTRAINT issue_relations_type_check CHECK (type IN ('blocks', 'related', 'duplicate_of')),
    CONSTRAINT issue_relations_not_self CHECK (issue_id <> related_issue_id)
);
-- At most ONE row per unordered pair and type. This makes the duplicate
-- check race-free, and it also refuses the reverse row: B related A when A
-- related B exists, B blocks A when A blocks B exists, and the same for
-- duplicate_of.
CREATE UNIQUE INDEX issue_relations_pair_key ON issue_relations
    (type, LEAST(issue_id, related_issue_id), GREATEST(issue_id, related_issue_id));
CREATE INDEX issue_relations_related_idx ON issue_relations (org_id, related_issue_id);
CREATE INDEX issue_relations_org_issue_idx ON issue_relations (org_id, issue_id);
CREATE INDEX issue_relations_created_by_idx ON issue_relations (created_by) WHERE created_by IS NOT NULL;

-- A comment belongs to exactly one issue or one document. parent_id threads.
CREATE TABLE comments (
    id           uuid        PRIMARY KEY DEFAULT uuidv7(),
    org_id       uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    issue_id     uuid,
    document_id  uuid,
    parent_id    uuid,
    author_id    uuid        REFERENCES users (id) ON DELETE SET NULL,
    body         text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    edited_at    timestamptz,
    CONSTRAINT comments_org_id_id_key UNIQUE (org_id, id),
    CONSTRAINT comments_issue_fk FOREIGN KEY (org_id, issue_id)
        REFERENCES issues (org_id, id) ON DELETE CASCADE,
    CONSTRAINT comments_document_fk FOREIGN KEY (org_id, document_id)
        REFERENCES documents (org_id, id) ON DELETE CASCADE,
    CONSTRAINT comments_parent_fk FOREIGN KEY (org_id, parent_id)
        REFERENCES comments (org_id, id) ON DELETE CASCADE,
    CONSTRAINT comments_one_target CHECK (num_nonnulls(issue_id, document_id) = 1),
    CONSTRAINT comments_body_len CHECK (char_length(body) BETWEEN 1 AND 100000)
);
CREATE INDEX comments_issue_idx ON comments (org_id, issue_id, created_at) WHERE issue_id IS NOT NULL;
CREATE INDEX comments_document_idx ON comments (org_id, document_id, created_at) WHERE document_id IS NOT NULL;
CREATE INDEX comments_parent_idx ON comments (org_id, parent_id) WHERE parent_id IS NOT NULL;
CREATE INDEX comments_author_idx ON comments (author_id) WHERE author_id IS NOT NULL;

-- kind 'link' carries url; kind 'file' carries an object in silo (T3).
CREATE TABLE attachments (
    id            uuid        PRIMARY KEY DEFAULT uuidv7(),
    org_id        uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    issue_id      uuid        NOT NULL,
    kind          text        NOT NULL,
    title         text        NOT NULL DEFAULT '',
    subtitle      text        NOT NULL DEFAULT '',
    url           text,
    object_key    text,
    filename      text,
    content_type  text,
    size_bytes    bigint,
    metadata      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    creator_id    uuid        REFERENCES users (id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT attachments_org_id_id_key UNIQUE (org_id, id),
    CONSTRAINT attachments_issue_fk FOREIGN KEY (org_id, issue_id)
        REFERENCES issues (org_id, id) ON DELETE CASCADE,
    CONSTRAINT attachments_kind_check CHECK (kind IN ('link', 'file')),
    CONSTRAINT attachments_link_shape CHECK (kind <> 'link' OR url IS NOT NULL),
    CONSTRAINT attachments_file_shape CHECK (
        kind <> 'file' OR (object_key IS NOT NULL AND filename IS NOT NULL
                           AND content_type IS NOT NULL AND size_bytes IS NOT NULL)
    ),
    CONSTRAINT attachments_size_check CHECK (size_bytes IS NULL OR size_bytes >= 0)
);
CREATE INDEX attachments_issue_idx ON attachments (org_id, issue_id, created_at);
CREATE UNIQUE INDEX attachments_object_key_key ON attachments (object_key) WHERE object_key IS NOT NULL;
CREATE INDEX attachments_creator_idx ON attachments (creator_id) WHERE creator_id IS NOT NULL;

-- Append-only state history. from_state_id is NULL for the row written when
-- the issue is created.
CREATE TABLE issue_state_changes (
    id             uuid        PRIMARY KEY DEFAULT uuidv7(),
    org_id         uuid        NOT NULL,
    issue_id       uuid        NOT NULL,
    from_state_id  uuid,
    to_state_id    uuid        NOT NULL,
    actor_id       uuid        REFERENCES users (id) ON DELETE SET NULL,
    source         text        NOT NULL DEFAULT 'api',
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT issue_state_changes_issue_fk FOREIGN KEY (org_id, issue_id)
        REFERENCES issues (org_id, id) ON DELETE CASCADE,
    CONSTRAINT issue_state_changes_from_fk FOREIGN KEY (org_id, from_state_id)
        REFERENCES workflow_states (org_id, id),
    CONSTRAINT issue_state_changes_to_fk FOREIGN KEY (org_id, to_state_id)
        REFERENCES workflow_states (org_id, id),
    CONSTRAINT issue_state_changes_source_check CHECK (source IN ('api', 'gitlab', 'import', 'system'))
);
CREATE INDEX issue_state_changes_issue_idx ON issue_state_changes (org_id, issue_id, created_at);
CREATE INDEX issue_state_changes_from_idx ON issue_state_changes (org_id, from_state_id) WHERE from_state_id IS NOT NULL;
CREATE INDEX issue_state_changes_to_idx ON issue_state_changes (org_id, to_state_id);
CREATE INDEX issue_state_changes_actor_idx ON issue_state_changes (actor_id) WHERE actor_id IS NOT NULL;

-- Daily burn-up per cycle (T3).
CREATE TABLE cycle_snapshots (
    id                uuid        PRIMARY KEY DEFAULT uuidv7(),
    org_id            uuid        NOT NULL,
    cycle_id          uuid        NOT NULL,
    snapshot_date     date        NOT NULL,
    scope_count       integer     NOT NULL DEFAULT 0,
    started_count     integer     NOT NULL DEFAULT 0,
    completed_count   integer     NOT NULL DEFAULT 0,
    scope_points      integer     NOT NULL DEFAULT 0,
    completed_points  integer     NOT NULL DEFAULT 0,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT cycle_snapshots_cycle_fk FOREIGN KEY (org_id, cycle_id)
        REFERENCES cycles (org_id, id) ON DELETE CASCADE,
    CONSTRAINT cycle_snapshots_cycle_date_key UNIQUE (cycle_id, snapshot_date)
);
CREATE INDEX cycle_snapshots_org_cycle_idx ON cycle_snapshots (org_id, cycle_id);

-- ---------------------------------------------------------------------------
-- Change feed
-- ---------------------------------------------------------------------------

-- seq is per org and is allocated from org_event_counters inside the writing
-- transaction. The counter row lock is held to COMMIT, so per org the seq
-- order is the commit order: a reader that has seen seq N can never later
-- find a committed N-1. (A global identity column does not give that: a
-- lower seq can commit after a higher one and be skipped by `after=`.)
CREATE TABLE org_event_counters (
    org_id    uuid   PRIMARY KEY REFERENCES orgs (id) ON DELETE CASCADE,
    next_seq  bigint NOT NULL DEFAULT 1,
    CONSTRAINT org_event_counters_next_seq_check CHECK (next_seq >= 1)
);

CREATE TABLE events (
    org_id       uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    seq          bigint      NOT NULL,
    type         text        NOT NULL,
    entity_type  text        NOT NULL,
    entity_id    uuid        NOT NULL,
    actor_id     uuid        REFERENCES users (id) ON DELETE SET NULL,
    payload      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, seq)
);
CREATE INDEX events_entity_idx ON events (org_id, entity_type, entity_id);
CREATE INDEX events_actor_idx ON events (actor_id) WHERE actor_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- GitLab integration (T5)
-- ---------------------------------------------------------------------------

-- The webhook secret is stored as sha256 only; the X-Gitlab-Token header is
-- hashed and compared in constant time.
CREATE TABLE gitlab_integrations (
    id                   uuid        PRIMARY KEY DEFAULT uuidv7(),
    org_id               uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    base_url             text        NOT NULL,
    webhook_secret_hash  bytea       NOT NULL,
    enabled              boolean     NOT NULL DEFAULT true,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT gitlab_integrations_org_id_id_key UNIQUE (org_id, id),
    CONSTRAINT gitlab_integrations_org_base_key UNIQUE (org_id, base_url),
    CONSTRAINT gitlab_integrations_hash_len CHECK (octet_length(webhook_secret_hash) = 32)
);

CREATE TABLE gitlab_project_map (
    id                 uuid        PRIMARY KEY DEFAULT uuidv7(),
    org_id             uuid        NOT NULL,
    integration_id     uuid        NOT NULL,
    gitlab_project_id  bigint      NOT NULL,
    project_path       text        NOT NULL,
    team_id            uuid,
    created_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT gitlab_project_map_integration_fk FOREIGN KEY (org_id, integration_id)
        REFERENCES gitlab_integrations (org_id, id) ON DELETE CASCADE,
    CONSTRAINT gitlab_project_map_team_fk FOREIGN KEY (org_id, team_id)
        REFERENCES teams (org_id, id) ON DELETE SET NULL (team_id),
    CONSTRAINT gitlab_project_map_project_key UNIQUE (integration_id, gitlab_project_id)
);
CREATE INDEX gitlab_project_map_org_integration_idx ON gitlab_project_map (org_id, integration_id);
CREATE INDEX gitlab_project_map_team_idx ON gitlab_project_map (org_id, team_id) WHERE team_id IS NOT NULL;

-- Webhook idempotency on X-Gitlab-Event-UUID. No caller IP, no User-Agent.
CREATE TABLE gitlab_webhook_events (
    id                 uuid        PRIMARY KEY DEFAULT uuidv7(),
    org_id             uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    event_uuid         text        NOT NULL,
    event_type         text        NOT NULL,
    gitlab_project_id  bigint,
    status             text        NOT NULL DEFAULT 'received',
    error              text,
    payload            jsonb       NOT NULL DEFAULT '{}'::jsonb,
    received_at        timestamptz NOT NULL DEFAULT now(),
    processed_at       timestamptz,
    CONSTRAINT gitlab_webhook_events_uuid_key UNIQUE (org_id, event_uuid),
    CONSTRAINT gitlab_webhook_events_status_check CHECK (
        status IN ('received', 'processed', 'ignored', 'failed')
    )
);
CREATE INDEX gitlab_webhook_events_received_idx ON gitlab_webhook_events (org_id, received_at);

CREATE TABLE mr_links (
    id                 uuid        PRIMARY KEY DEFAULT uuidv7(),
    org_id             uuid        NOT NULL,
    issue_id           uuid        NOT NULL,
    gitlab_project_id  bigint      NOT NULL,
    mr_iid             integer     NOT NULL,
    url                text        NOT NULL,
    title              text        NOT NULL DEFAULT '',
    state              text        NOT NULL,
    source_branch      text        NOT NULL DEFAULT '',
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT mr_links_issue_fk FOREIGN KEY (org_id, issue_id)
        REFERENCES issues (org_id, id) ON DELETE CASCADE,
    CONSTRAINT mr_links_state_check CHECK (state IN ('opened', 'merged', 'closed')),
    CONSTRAINT mr_links_unique UNIQUE (issue_id, gitlab_project_id, mr_iid)
);
CREATE INDEX mr_links_org_issue_idx ON mr_links (org_id, issue_id);
CREATE INDEX mr_links_mr_idx ON mr_links (org_id, gitlab_project_id, mr_iid);

-- ---------------------------------------------------------------------------
-- Importer and request bookkeeping
-- ---------------------------------------------------------------------------

-- Maps a source system id (a Linear UUID) to the row the importer made, so a
-- re-run is safe and get_issue can resolve an old Linear UUID.
CREATE TABLE import_mappings (
    id           uuid        PRIMARY KEY DEFAULT uuidv7(),
    org_id       uuid        NOT NULL REFERENCES orgs (id) ON DELETE CASCADE,
    source       text        NOT NULL DEFAULT 'linear',
    entity_type  text        NOT NULL,
    source_id    text        NOT NULL,
    target_id    uuid        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT import_mappings_source_key UNIQUE (org_id, source, entity_type, source_id)
);
CREATE INDEX import_mappings_target_idx ON import_mappings (org_id, target_id);

-- Idempotency-Key on POST creates. A row with status_code NULL is a request
-- in flight. Rows expire after 24 hours and a sweeper deletes them.
-- POST /tokens never uses this table: its response holds a token secret.
CREATE TABLE idempotency_keys (
    id             uuid        PRIMARY KEY DEFAULT uuidv7(),
    user_id        uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    key            text        NOT NULL,
    method         text        NOT NULL,
    path           text        NOT NULL,
    request_hash   bytea       NOT NULL,
    status_code    integer,
    response_body  bytea,
    -- Set INSIDE the handler's domain transaction (database.InTx), so it is
    -- true exactly when the request's write committed. A claim with
    -- status_code NULL and committed_at NULL that is older than the stale
    -- window was abandoned before writing and may be re-run; one with
    -- committed_at set must never be re-run.
    committed_at   timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    expires_at     timestamptz NOT NULL,
    CONSTRAINT idempotency_keys_user_key_key UNIQUE (user_id, key),
    CONSTRAINT idempotency_keys_key_len CHECK (char_length(key) BETWEEN 1 AND 255)
);
CREATE INDEX idempotency_keys_expires_idx ON idempotency_keys (expires_at);
