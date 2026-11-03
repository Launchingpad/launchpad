-- LAUN-206: chat service kind 'fluxer' (Discord-format body, any public https host).
ALTER TABLE chat_services DROP CONSTRAINT chat_services_kind_check;
ALTER TABLE chat_services ADD CONSTRAINT chat_services_kind_check
    CHECK (kind IN ('slack', 'discord', 'fluxer', 'custom'));
