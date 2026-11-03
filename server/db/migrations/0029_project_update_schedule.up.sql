-- 0029_project_update_schedule: LAUN-196 part B (decisions-T30). A per-project
-- update schedule, and a notification that is about a project, not an issue.

ALTER TABLE projects
    ADD COLUMN update_frequency   text     NOT NULL DEFAULT 'none',
    ADD COLUMN update_weekday     smallint NOT NULL DEFAULT 5,
    ADD COLUMN update_hour        smallint NOT NULL DEFAULT 9,
    ADD COLUMN update_last_due_at timestamptz,
    ADD CONSTRAINT projects_update_frequency_check CHECK (update_frequency IN ('none', 'weekly', 'biweekly', 'monthly')),
    ADD CONSTRAINT projects_update_weekday_check CHECK (update_weekday BETWEEN 0 AND 6),
    ADD CONSTRAINT projects_update_hour_check CHECK (update_hour BETWEEN 0 AND 23);

-- A notification is about one issue OR one project.
ALTER TABLE notifications
    ALTER COLUMN issue_id DROP NOT NULL,
    ADD COLUMN project_id uuid,
    ADD CONSTRAINT notifications_project_fk FOREIGN KEY (org_id, project_id)
        REFERENCES projects (org_id, id) ON DELETE CASCADE,
    ADD CONSTRAINT notifications_one_target CHECK (num_nonnulls(issue_id, project_id) = 1),
    DROP CONSTRAINT notifications_kind_check,
    ADD CONSTRAINT notifications_kind_check CHECK (kind IN
        ('assigned', 'state_change', 'comment', 'mention', 'new_issue', 'project_update_due')),
    ADD CONSTRAINT notifications_kind_target CHECK ((kind = 'project_update_due') = (project_id IS NOT NULL));

ALTER TABLE notification_emails
    ALTER COLUMN issue_id DROP NOT NULL,
    ADD COLUMN project_id uuid,
    ADD CONSTRAINT notification_emails_project_fk FOREIGN KEY (org_id, project_id)
        REFERENCES projects (org_id, id) ON DELETE CASCADE,
    ADD CONSTRAINT notification_emails_one_target CHECK (num_nonnulls(issue_id, project_id) = 1);
CREATE INDEX notification_emails_project_idx ON notification_emails (org_id, project_id) WHERE project_id IS NOT NULL;

ALTER TABLE notification_settings
    DROP CONSTRAINT notification_settings_event_check,
    ADD CONSTRAINT notification_settings_event_check CHECK (event IN
        ('assigned', 'state_change', 'comment', 'mention', 'new_issue', 'project_update_due'));
CREATE INDEX notifications_project_idx ON notifications (org_id, project_id) WHERE project_id IS NOT NULL;
