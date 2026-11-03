-- Reverses 0001_baseline: drops every object it made, children first.
-- This destroys all data. It exists so `up, down, up` can be tested and so a
-- fresh dev database can be reset; never run it against production.

DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS import_mappings;
DROP TABLE IF EXISTS mr_links;
DROP TABLE IF EXISTS gitlab_webhook_events;
DROP TABLE IF EXISTS gitlab_project_map;
DROP TABLE IF EXISTS gitlab_integrations;
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS org_event_counters;
DROP TABLE IF EXISTS cycle_snapshots;
DROP TABLE IF EXISTS issue_state_changes;
DROP TABLE IF EXISTS attachments;
DROP TABLE IF EXISTS comments;
DROP TABLE IF EXISTS issue_relations;
DROP TABLE IF EXISTS issue_labels;
DROP TABLE IF EXISTS issues;
DROP TABLE IF EXISTS documents;
DROP TABLE IF EXISTS cycles;
DROP TABLE IF EXISTS milestones;
DROP TABLE IF EXISTS project_teams;
DROP TABLE IF EXISTS projects;
DROP TABLE IF EXISTS labels;
-- teams and workflow_states reference each other; drop the cycle first.
ALTER TABLE IF EXISTS teams
    DROP CONSTRAINT IF EXISTS teams_default_state_fk,
    DROP CONSTRAINT IF EXISTS teams_review_state_fk,
    DROP CONSTRAINT IF EXISTS teams_testing_state_fk;
DROP TABLE IF EXISTS workflow_states;
DROP TABLE IF EXISTS issue_counters;
DROP TABLE IF EXISTS teams;
DROP TABLE IF EXISTS api_tokens;
DROP TABLE IF EXISTS org_members;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS orgs;
DROP FUNCTION IF EXISTS public.uuidv7();
