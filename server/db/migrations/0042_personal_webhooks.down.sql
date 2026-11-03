-- LAUN-229: reverses 0042. The members' webhooks and their waiting posts are
-- lost; an older image has no such feature.
DROP TABLE personal_webhook_outbox;
DROP TABLE personal_webhooks;
