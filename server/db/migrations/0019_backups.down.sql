-- Reverses 0019. The circular foreign key goes first, then the tables in
-- dependency order. The data goes with them (this is a development rollback;
-- production rolls forward).
DROP TRIGGER backup_settings_signing_guard ON backup_settings;
DROP FUNCTION backup_settings_signing_guard();
ALTER TABLE backup_dumps DROP CONSTRAINT backup_dumps_request_id_fkey;
DROP TABLE backup_runs;
DROP TABLE backup_change_log;
DROP TABLE backup_requests;
DROP TABLE backup_dumps;
DROP TABLE backup_destinations;
DROP TABLE backup_settings;
