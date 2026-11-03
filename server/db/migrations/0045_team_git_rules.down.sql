-- LAUN-244: reverses 0045. Every team's git workflow rules are lost; every
-- team then has the built-in moves again.
DROP TABLE team_git_rule_from_states;
DROP TABLE team_git_rules;
