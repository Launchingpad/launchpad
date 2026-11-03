-- ORB-3409 (T5): scope mr_links by GitLab integration. 0001 keyed a link by
-- (issue_id, gitlab_project_id, mr_iid), so two GitLab instances of one org
-- whose projects share a numeric id and an MR iid shared link rows (a close
-- on one marked the other's link closed). The webhook always knows its
-- integration (the matched secret), so every link now records it.

ALTER TABLE mr_links ADD COLUMN integration_id uuid;

-- Backfill. A link was written by a webhook of an integration that mapped
-- its project, so the rule is: the oldest integration of the org that maps
-- the link's gitlab_project_id; if none maps it any more (a mapping was
-- removed), the oldest integration of the org. A link whose org has no
-- integration at all cannot exist (no hook could have written it); if one
-- does, it is deleted, because it can no longer be attributed. No hook
-- existed before this migration (no hostname yet), so the table is empty
-- everywhere it ships; the rule exists for completeness.
UPDATE mr_links l SET integration_id = COALESCE(
    (SELECT m.integration_id FROM gitlab_project_map m
       JOIN gitlab_integrations g ON g.org_id = m.org_id AND g.id = m.integration_id
      WHERE m.org_id = l.org_id AND m.gitlab_project_id = l.gitlab_project_id
      ORDER BY g.created_at, g.id LIMIT 1),
    (SELECT g.id FROM gitlab_integrations g WHERE g.org_id = l.org_id
      ORDER BY g.created_at, g.id LIMIT 1));
DELETE FROM mr_links WHERE integration_id IS NULL;

ALTER TABLE mr_links
    ALTER COLUMN integration_id SET NOT NULL,
    ADD CONSTRAINT mr_links_integration_fk FOREIGN KEY (org_id, integration_id)
        REFERENCES gitlab_integrations (org_id, id) ON DELETE CASCADE,
    DROP CONSTRAINT mr_links_unique,
    ADD CONSTRAINT mr_links_unique UNIQUE (issue_id, integration_id, gitlab_project_id, mr_iid);

-- The close path looks links up by (org, integration, project, iid); the
-- index also covers the new foreign key (its leading columns).
DROP INDEX mr_links_mr_idx;
CREATE INDEX mr_links_mr_idx ON mr_links (org_id, integration_id, gitlab_project_id, mr_iid);
