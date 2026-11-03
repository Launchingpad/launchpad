-- LAUN-223: reverses 0039. The old CHECK has no access_removed, so the rows
-- with it first become disabled (the nearest old reason: the mail was not
-- sent on purpose). That rewrite cannot be undone by a later up.
UPDATE notification_emails SET skip_reason = 'disabled' WHERE skip_reason = 'access_removed';
ALTER TABLE notification_emails DROP CONSTRAINT notification_emails_skip_reason_check;
ALTER TABLE notification_emails ADD CONSTRAINT notification_emails_skip_reason_check CHECK (skip_reason IN (
    'no_mail', 'no_address', 'unproven', 'no_base_url', 'rate_cap', 'expired', 'disabled'));
