> Internal design note from the Orbit deployment; kept for history.

# T19: the Triage state (LAUN-20)

Every team gets a workflow state "Triage" of type `triage`. The type was in
the model from 0001 (CHECK, `stateType` filter, MCP, web icon and group
order); no team had a state of it. This file holds the judgment calls. Code:
`server/db/migrations/0011_triage_state.*.sql`, `tracker.DefaultStates`,
`db/queries/planning.sql` (rollover, burn-up).

1. **Always present; no per-team switch.** Linear lets a team turn Triage on.
   Here the state exists for every team, and a team that does not want it
   ignores it (an empty column; the picker lists it first). A switch would be
   a new column, a PATCH field and a second code path for one empty state.
   Add it later if a team asks; nothing here blocks that.
2. **New teams: nine states.** `DefaultStates` starts with `Triage` (gray
   `#bec2c8`), so `CreateTeamTx` (API and first-run setup) gives position 0
   to Triage and 1 to 8 to the old eight.
3. **Existing teams: migration 0011.** It inserts Triage for each team that
   has no state of type `triage` and no state named "Triage" (the
   `(team_id, name)` key, any case; archived states count). Position = the
   team's lowest position - 1, so it is first. A team that already has one
   (for example imported from Linear) is untouched. Org-safe: each row takes
   its team's own `org_id`.
4. **Down.** "The Triage states" are the rows with name "Triage" AND type
   `triage`: only those are changed or deleted. A triage-type state with
   another name (an import) and its issues stay. First a team default that is
   a Triage state, or NULL, is set to the team's lowest-position backlog-type
   state, else its lowest-position non-triage state. Then issues in a Triage
   state move to the team's default (else the same fallback), the
   `issue_state_changes` rows that name a Triage state are deleted (their
   keys have no ON DELETE), then the states go. A down also removes a
   Linear-imported state that is named "Triage". `import_mappings` rows for
   an adopted Triage state stay after a down (no foreign key to the state).
5. **Defaults do not change.** A new issue (web, MCP, REST, sign-up flows,
   GitLab) still starts in the team's `default_state_id`, Backlog. Triage is
   chosen by name or id. Reason: Linear's triage inbox is an intake queue for
   a team that turns it on; here there is no switch (#1), so making it the
   default would put every issue of every team into an inbox nobody works.
   The admin can point `defaultStateId` at Triage if a team wants intake.
6. **Cycles ignore Triage.** `RolloverIssues` does not move issues in a
   `triage` state (they stay in the completed cycle; they are not planned
   work). The burn-up row (`UpsertCycleSnapshot`) leaves them out of
   `scope_count` and `scope_points`; `triage_count` (already in 0003) counts
   them. Setting `cycleId` on a triage issue by hand is allowed.
7. **GitLab.** `stateTypeRank` already ranks triage lowest, so an opened or
   merged MR moves a Triage issue forward (In Review, In Testing). Nothing
   moves an issue back into Triage.
8. **Timestamps.** A triage issue has no started, completed or canceled time
   (the default branch of `stateTimestamps`, as for Backlog).
9. **Importer (T4).** Linear's Triage state (type `triage`) is adopted by name
   like the other states: the seeded Triage row is updated (color, position,
   timestamps from Linear). A Linear team with no Triage keeps the empty
   seeded one. The importer's state counts for a fresh seed are now all
   updates (6 of 6 in the fixture).
10. **API, openapi, MCP: no change.** States are listed from the table, so
    `GET /teams/{team}/states` returns nine rows; `stateType=triage` already
    worked. `list_issue_statuses` and `save_issue state: "Triage"` resolve by
    name. No tool text changed, so the golden files are the same.
11. **Web: no change.** The picker, filter chips, create dialog and
    list/board groups read the team's states and order by type then
    position, so Triage comes first. `filters.test.ts` pins that.
    The saved "Backlog" view (`state=Backlog,Todo`) deliberately excludes
    Triage.
