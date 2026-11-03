-- Reverses 0004_mr_links_integration. The only loss: when two integrations
-- of one org had links for the same issue, project id and MR iid (the case
-- 0004 exists to allow), the old key cannot hold both, so all but the
-- oldest such row (created_at, id) are deleted before the old key returns.

DROP INDEX mr_links_mr_idx;
DELETE FROM mr_links l
 WHERE EXISTS (
     SELECT 1 FROM mr_links o
      WHERE o.issue_id = l.issue_id AND o.gitlab_project_id = l.gitlab_project_id
        AND o.mr_iid = l.mr_iid
        AND (o.created_at, o.id) < (l.created_at, l.id));

ALTER TABLE mr_links
    DROP CONSTRAINT mr_links_unique,
    ADD CONSTRAINT mr_links_unique UNIQUE (issue_id, gitlab_project_id, mr_iid),
    DROP CONSTRAINT mr_links_integration_fk,
    DROP COLUMN integration_id;

CREATE INDEX mr_links_mr_idx ON mr_links (org_id, gitlab_project_id, mr_iid);
