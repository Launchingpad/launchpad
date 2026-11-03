-- The project reminders (and their email rows, by cascade) and the
-- project_update_due settings cannot exist under the old constraints, so the
-- down deletes them. The schedule columns go with the data.
DELETE FROM notification_settings WHERE event = 'project_update_due';
DELETE FROM notifications WHERE project_id IS NOT NULL;

DROP INDEX notifications_project_idx;
ALTER TABLE notification_settings
    DROP CONSTRAINT notification_settings_event_check,
    ADD CONSTRAINT notification_settings_event_check CHECK (event IN
        ('assigned', 'state_change', 'comment', 'mention', 'new_issue'));

DROP INDEX notification_emails_project_idx;
ALTER TABLE notification_emails
    DROP CONSTRAINT notification_emails_one_target,
    DROP CONSTRAINT notification_emails_project_fk,
    DROP COLUMN project_id,
    ALTER COLUMN issue_id SET NOT NULL;

ALTER TABLE notifications
    DROP CONSTRAINT notifications_kind_target,
    DROP CONSTRAINT notifications_kind_check,
    ADD CONSTRAINT notifications_kind_check CHECK (kind IN ('assigned', 'state_change', 'comment', 'mention', 'new_issue')),
    DROP CONSTRAINT notifications_one_target,
    DROP CONSTRAINT notifications_project_fk,
    DROP COLUMN project_id,
    ALTER COLUMN issue_id SET NOT NULL;

ALTER TABLE projects
    DROP CONSTRAINT projects_update_hour_check,
    DROP CONSTRAINT projects_update_weekday_check,
    DROP CONSTRAINT projects_update_frequency_check,
    DROP COLUMN update_last_due_at,
    DROP COLUMN update_hour,
    DROP COLUMN update_weekday,
    DROP COLUMN update_frequency;
