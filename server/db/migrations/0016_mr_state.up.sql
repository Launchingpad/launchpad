-- LAUN-83 (T17 migration D, decisions-T17 #18 and #20): the state of a merge
-- request on its mr_links row, and the three timeline kinds for it.
-- The columns carry host content as short text only (the author LOGIN, the repo
-- label, the head sha); each CHECK bounds the length, and the app cuts a value
-- to its length before it writes. provider_updated_at orders events (an older
-- event is stale). checks_updated_at and checks_detail belong to the check
-- events of a later ticket and stay at their defaults here.
ALTER TABLE mr_links
    ADD COLUMN draft boolean NOT NULL DEFAULT false,
    ADD COLUMN head_sha text NOT NULL DEFAULT '' CHECK (length(head_sha) <= 64),
    ADD COLUMN review text NOT NULL DEFAULT 'none' CHECK (review IN ('none','approved','changes_requested')),
    ADD COLUMN checks text NOT NULL DEFAULT 'none' CHECK (checks IN ('none','pending','success','failure')),
    ADD COLUMN author text NOT NULL DEFAULT '' CHECK (length(author) <= 100),
    ADD COLUMN provider_updated_at timestamptz,
    ADD COLUMN repo_path text NOT NULL DEFAULT '' CHECK (length(repo_path) <= 300),
    ADD COLUMN checks_updated_at timestamptz,
    ADD COLUMN checks_detail jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(checks_detail) = 'object');

CREATE INDEX mr_links_checks_idx ON mr_links (org_id, integration_id, repo_key, head_sha) WHERE head_sha <> '';

ALTER TABLE issue_activity DROP CONSTRAINT issue_activity_kind_check;
ALTER TABLE issue_activity ADD CONSTRAINT issue_activity_kind_check CHECK (kind IN (
    'title', 'description', 'assignee', 'priority', 'estimate', 'dueDate',
    'parent', 'project', 'milestone', 'cycle',
    'labelAdded', 'labelRemoved', 'relationRemoved',
    'mrLinked', 'mrMerged', 'mrClosed'));
