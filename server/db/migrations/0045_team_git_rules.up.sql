-- LAUN-244 (decisions-T44-git-workflow.md): per-team git workflow rules. A
-- team with no row keeps the built-in moves (opened, ready and reopened to the
-- team's review state, merged to its testing state, closed: no move). A row
-- replaces the built-in move of ONE event for ONE team.
--
-- target_state_id NULL means "no move". The state must be a state of the same
-- team (composite FK). No route deletes a workflow state today; a team delete
-- cascades its states and its rules in one statement, so the plain (NO ACTION)
-- FK holds. A state that is archived stays referenced: the API flags the rule
-- and the webhook does not move to it.
CREATE TABLE team_git_rules (
    org_id          uuid        NOT NULL,
    team_id         uuid        NOT NULL,
    event           text        NOT NULL,
    target_state_id uuid,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT team_git_rules_pkey PRIMARY KEY (team_id, event),
    CONSTRAINT team_git_rules_event_check CHECK (event IN ('opened', 'ready', 'reopened', 'merged', 'closed')),
    CONSTRAINT team_git_rules_team_fk FOREIGN KEY (org_id, team_id)
        REFERENCES teams (org_id, id) ON DELETE CASCADE,
    CONSTRAINT team_git_rules_target_fk FOREIGN KEY (team_id, target_state_id)
        REFERENCES workflow_states (team_id, id)
);

CREATE INDEX team_git_rules_org_team_idx ON team_git_rules (org_id, team_id);
CREATE INDEX team_git_rules_target_idx ON team_git_rules (team_id, target_state_id) WHERE target_state_id IS NOT NULL;

-- The optional "only from these states" set of a rule. No row: the rule moves
-- forward only (the built-in rule). Rows: the rule moves an issue only from
-- one of these states, in any direction.
CREATE TABLE team_git_rule_from_states (
    org_id   uuid NOT NULL,
    team_id  uuid NOT NULL,
    event    text NOT NULL,
    state_id uuid NOT NULL,
    CONSTRAINT team_git_rule_from_states_pkey PRIMARY KEY (team_id, event, state_id),
    CONSTRAINT team_git_rule_from_states_team_fk FOREIGN KEY (org_id, team_id)
        REFERENCES teams (org_id, id) ON DELETE CASCADE,
    CONSTRAINT team_git_rule_from_states_rule_fk FOREIGN KEY (team_id, event)
        REFERENCES team_git_rules (team_id, event) ON DELETE CASCADE,
    CONSTRAINT team_git_rule_from_states_state_fk FOREIGN KEY (team_id, state_id)
        REFERENCES workflow_states (team_id, id)
);

CREATE INDEX team_git_rule_from_states_org_team_idx ON team_git_rule_from_states (org_id, team_id);
CREATE INDEX team_git_rule_from_states_state_idx ON team_git_rule_from_states (team_id, state_id);
