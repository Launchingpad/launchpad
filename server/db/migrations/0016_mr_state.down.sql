-- LAUN-83: reverses 0016. Down policy of the tranche (decisions-T17 #8): the
-- down deletes what the old schema cannot hold. The timeline rows of the three
-- MR kinds are deleted (the old CHECK refuses them), and the MR state columns
-- are dropped with their data.
DELETE FROM issue_activity WHERE kind IN ('mrLinked', 'mrMerged', 'mrClosed');

ALTER TABLE issue_activity DROP CONSTRAINT issue_activity_kind_check;
ALTER TABLE issue_activity ADD CONSTRAINT issue_activity_kind_check CHECK (kind IN (
    'title', 'description', 'assignee', 'priority', 'estimate', 'dueDate',
    'parent', 'project', 'milestone', 'cycle',
    'labelAdded', 'labelRemoved', 'relationRemoved'));

DROP INDEX mr_links_checks_idx;
ALTER TABLE mr_links
    DROP COLUMN draft,
    DROP COLUMN head_sha,
    DROP COLUMN review,
    DROP COLUMN checks,
    DROP COLUMN author,
    DROP COLUMN provider_updated_at,
    DROP COLUMN repo_path,
    DROP COLUMN checks_updated_at,
    DROP COLUMN checks_detail;
