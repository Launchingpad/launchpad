-- LAUN-206: reverses 0035. The old CHECK has no 'fluxer', so a Fluxer service
-- becomes a 'custom' one (same sealed URL and host; it then posts the custom
-- body, which Fluxer refuses, so re-create it after the up). Nothing else is lost.
UPDATE chat_services SET kind = 'custom' WHERE kind = 'fluxer';
ALTER TABLE chat_services DROP CONSTRAINT chat_services_kind_check;
ALTER TABLE chat_services ADD CONSTRAINT chat_services_kind_check
    CHECK (kind IN ('slack', 'discord', 'custom'));
