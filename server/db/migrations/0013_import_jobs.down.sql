-- LAUN-8: reverses 0013. The import sessions and jobs are working state of
-- the web import (no tenant data the importer wrote lives here: that is in
-- import_mappings and the entity tables), so the down drops them.
DROP TABLE import_jobs;
DROP TABLE import_sessions;
