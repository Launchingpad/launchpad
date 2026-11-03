-- LAUN-197: reverses 0033. It loses the stored relay (sealed), every
-- registered device and any push still waiting, and the push column of the
-- members' notification settings, which is the nature of a down.
DROP TABLE push_outbox;
DROP TABLE push_devices;
DROP TABLE push_relays;
ALTER TABLE notification_settings DROP CONSTRAINT notification_settings_push_check;
ALTER TABLE notification_settings DROP COLUMN push;
