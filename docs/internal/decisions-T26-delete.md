# Decisions T26: delete projects and teams (LAUN-191)

Status: built. Products already had `DELETE /products/{product}` (T18); this
ticket adds the same for projects and teams and keeps the product rule.

1. **Routes.** `DELETE /orgs/{org}/projects/{project}` and
   `DELETE /orgs/{org}/teams/{team}`, both `204`, both admin scope (the scope
   of a team write and of a product delete). A `write` token, an agent token
   without admin scope and a `read` token get `403 insufficient_scope`. There
   is no new bypass and no `refuseAgents` (these are not notification routes).
   No `Idempotency-Key`: a repeat is `404`, like the product delete.
2. **Cross-org is `404 not_found`.** Every query filters by `org_id`.
3. **Project delete detaches, never deletes issues.** The foreign keys already
   say so and no migration is needed: `issues.project_id` and
   `documents.project_id` are `ON DELETE SET NULL`; `issues.milestone_id` is
   `SET NULL`; `milestones` and `project_teams` are `ON DELETE CASCADE`.
   A project need not be archived first (unlike a product): nothing of value
   is lost but the project row and its milestones, and the dialog says so.
4. **Product delete is unchanged.** It needs an archived product and removes
   only its `product_teams` rows, so the teams stay with no product.
5. **Team delete is refused with `409 team_in_use`** (new code in
   `httpx.Codes` and the `ErrorCode` enum) when
   (a) the team has any issue, archived ones included; (b) it is the org's
   only team; (c) it is the org's default team (`orgs.default_team_id`;
   choose another default first). Nothing is deleted on a refusal and no
   event is written. The message names the reason and the issue count.
   A team that passes goes with its workflow states, labels, cycles and
   snapshots, follows, notification settings and product links (cascade);
   git repo maps keep their row with a null team (`SET NULL`). Products and
   projects stay.
6. **Locks.** `DeleteTeam` takes: the cycle advisory lock, the rank
   advisory lock (`rankLock(team)`), the org row `FOR NO KEY UPDATE`, then
   the team row `FOR UPDATE`. Every issue create and move takes the rank lock
   first, so none can run between the issue count and the delete, and the
   cascade onto `issue_counters` cannot meet a create (the first version took
   the team row without the rank lock and deadlocked, 500 both ways; without
   the rank lock a create could even commit and then be cascaded away).
   The org row is `NO KEY UPDATE`, not `UPDATE`: `events.Emit` locks
   `org_event_counters` and then needs KEY SHARE on the org row
   (`events_org_id_fkey`), so `FOR UPDATE` deadlocked with any event-writing
   request in the org. `NO KEY UPDATE` still serialises deletes among
   themselves (the last-team check) and does not block that KEY SHARE.
   (`PATCH /orgs` locks the org row `FOR UPDATE`; that is older and is not
   changed here.) The team row `FOR UPDATE` conflicts with the KEY SHARE every
   insert of a team reference takes.
   Tests: `TestDeleteTeam_ConcurrentCreate` (fails without the rank lock) and
   `TestDeleteTeam_RacesWithOtherWriters` (fails with `FOR UPDATE` on the org).
6a. **A write that loses to a delete is not a 500 (teams AND projects).** A
   label, project, product, follow or notification-settings write that names
   a team, or an issue, milestone or document write that names a project, can
   lose the race: its insert then fails on a foreign key (23503):
   `*_team_fk`, `*_project_fk` (issues, milestones, documents, project_teams)
   or `issues_milestone_fk` (the milestone goes with its project).
   `database.Retryable` (used by `InTx` and by the one non-transactional
   follow insert) maps that to `*database.GoneError{Ref}`: `400
   validation_failed` ("the team/project/milestone no longer exists"), because
   a body reference to a missing row is 400. Handlers that name the row in the
   path (follow, notification settings, product add team) turn it into `404`
   (`pathGone`). An issue create that finds its team gone at the counter
   answers the same 400 as one that does not find the team at the lookup. The
   cycle job skips a team deleted after `ListCycleTeams`; only the team lookup
   of that job means "gone" (sentinel `errTeamGone`), any other no-rows is a
   real error. Tests: `TestDeleteTeam_RacesWithOtherWriters`,
   `TestDeleteProject_RacesWithWriters` (many rounds, both start orders) and
   `TestLostRace_IsCleanNotFound` (deterministic: an open transaction deletes
   the row, the writer waits on its lock, then the delete commits). Mutant:
   remove the `_team_fk` or `_project_fk` entry of `goneFKs` and the matching
   tests fail with 500. The `issues_milestone_fk` entry is pinned by the unit
   `TestRetryable` only: no integration test reaches it (the project foreign key
   or the milestone lookup fires first).
6b. **A residual deadlock or serialization failure** (40P01, 40001) is `503
   overloaded` with `Retry-After: 1`, never 500 and never 409. A 5xx is not
   stored by the idempotency middleware, so a retry with the same
   `Idempotency-Key` runs the write again (a 409 would have been replayed).
   `Server.fail` logs it at warn level (method, route, code; no body) so an
   operator can see the rate.
   Any write can answer `503 overloaded` after a deadlock; this is known, and
   openapi documents it on the two delete routes and on the shared
   `Overloaded` response.
7. **Events.** `project.deleted` (payload id, name; `detachedIssues` was
   dropped by LAUN-221, decisions-T20 build notes 6a: it counted issues of
   teams a reader may not see) and
   `team.deleted` (id, key). `events.Emit` is the last statement of the
   transaction. Detached issues emit no `issue.updated` each (a project
   with thousands would flood the feed); the web refreshes issue lists on
   `project.deleted`. The project avatar object is deleted after the commit,
   best effort, like a replaced avatar (T24).
8. **Web.** Settings, Teams: a Delete button per row (admins), a confirm
   dialog that says what goes and what stays, and the server's `409` sentence
   when refused. Project page: a Delete project button (org admins), a
   confirm dialog saying issues and documents stay, then a return to the
   list. Cancel has focus; Escape closes with no request.
9. **MCP.** No tool is added. The agent surface stays read/write; deleting
   a project or a team is an admin action for a person in the web app or REST.
10. **Known gap.** The Linear importer does not take the team's rank lock. A
    team deleted in the middle of an import makes that import fail with an
    unclean error. It loses no data and is left as is.
