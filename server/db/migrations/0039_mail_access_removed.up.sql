-- LAUN-223 (T20 M2b, decisions-T20-team-access.md #63): the mail sender skips
-- a row whose recipient no longer sees the issue's team or the project, with
-- the reason access_removed.
ALTER TABLE notification_emails DROP CONSTRAINT notification_emails_skip_reason_check;
ALTER TABLE notification_emails ADD CONSTRAINT notification_emails_skip_reason_check CHECK (skip_reason IN (
    'no_mail', 'no_address', 'unproven', 'no_base_url', 'rate_cap', 'expired', 'disabled', 'access_removed'));
