-- projects.state still holds each project's kind, so dropping the statuses loses only names, colours and order.
ALTER TABLE projects DROP CONSTRAINT projects_status_fk;
DROP INDEX projects_status_idx;
ALTER TABLE projects DROP COLUMN status_id;
DROP TRIGGER orgs_project_statuses ON orgs;
DROP FUNCTION project_statuses_for_new_org();
DROP FUNCTION seed_project_statuses(uuid);
DROP TABLE project_statuses;
