-- Reverses 0010_notifications. Every notification row, setting, follow,
-- subscription, outbox row, cursor and token is dropped with its table.
DROP TRIGGER orgs_notification_cursor ON orgs;
DROP FUNCTION notification_cursor_for_new_org();
DROP TABLE notification_mail_tokens;
DROP TABLE notification_cursors;
DROP TABLE notification_emails;
DROP TABLE notifications;
DROP TABLE issue_subscriptions;
DROP TABLE team_follows;
DROP TABLE notification_settings;
DROP TABLE notification_user_settings;
