# Decisions T27: cycle options (LAUN-172)

Status: built. The project owner chose the v1 menu on 2026-10-30: a `...` button on the
cycle page with Edit name and description, Change dates, Mark complete,
Delete, Copy link. Not in v1: calendar, notifications, favorite, CSV export.
Decisions-T3 #15 said cycles are not re-dated or deleted through the API; this
file replaces that for the four rules below. The cycle job still makes cycles.

The problem: the cycle job owns the dates, keeps the chain contiguous (a cycle
starts where the one before it ends) and completes and rolls over. A manual
write must not break the chain and must not race the job.

1. **One lock.** Every manual write (name, dates, complete, delete) takes the
   team cycle advisory lock (`lockTeamCycles`, the job's lock) FIRST, then
   re-reads the cycle. So it runs before or after a job run, never inside one.
   `UpdateCycle` (name) took no lock before; it does now. Dates, Mark complete
   and Delete also run the job's own maintenance for the team under the lock
   BEFORE they judge the chain (an ended cycle is completed, a missing upcoming
   cycle is made), so a rule never sees a chain the job is about to change, and
   the result is the same whether the job or the manual write got the lock
   first. The job's events are emitted with ours, last. No migration: the
   rules use the existing columns and foreign keys.
2. **Routes.** `PATCH /cycles/{cycle}` (now also `startsAt`, `endsAt`),
   `POST /cycles/{cycle}/complete` (200, the cycle; `Idempotency-Key` after the
   scope check) and `DELETE /cycles/{cycle}` (204, no key: a repeat is 404).
   Scope: the name and date edit stay `write` (the existing PATCH); Mark complete
   and Delete need `admin`, like the team, project and product deletes
   (decisions-T26): a destructive action is for a person. A `write` or `read`
   token gets `403 insufficient_scope`. (The project owner had not decided; this is the safer
   default and one line each in `router.go` to reverse.)
   Cross-org is 404. No `refuseAgents`: these are not notification routes.
   New codes: `409 cycle_conflict` (a rule forbids it now) and
   `409 cycle_in_use` (delete, the cycle has issues). A bad value is 400.
3. **Name and description**: any cycle, as before. A cycle with no name shows
   "Cycle N".
4. **Dates.** `startsAt` and `endsAt` are RFC 3339 instants, or `YYYY-MM-DD`,
   read as that team-local day at the team's `cycleStartTime` (the web sends
   days). A day that equals the current local day of the stored instant is "no
   change" (an imported cycle with another time of day keeps it). Rules:
   - a completed cycle, or one that has ended, is history: 409;
   - `startsAt` changes only for a cycle that has not started (409 otherwise);
   - a new `startsAt` and a new `endsAt` must be after now (400; to end a
     running cycle now, use Mark complete) and `endsAt` after `startsAt` (400);
   - **contiguous by moving the neighbour.** The cycle whose end equals this
     start moves its end with the new start; the cycle whose start equals this
     end moves its start with the new end. The neighbour must stay non-empty
     (the new end before the next cycle's end; the new start after the
     previous cycle's start) and not be completed, else 409. Nothing else
     moves, so the chain has no gap and no overlap;
   - the new range must not overlap any other cycle of the team (409; this
     covers a chain that was already broken, for example by an import).
   Numbers do not change. A cycle's dates are no longer always a cadence
   boundary; the job never reads a cycle's boundaries, only the last end.
5. **Mark complete** = the job's completion run now, for one cycle. Allowed only
   for the CURRENT cycle (started, not ended, not completed) of a team with
   cycles on, and only when a next cycle exists (409 otherwise). It sets the
   cycle's end to now, the next cycle's start to now (so the next cycle is
   current and the chain stays contiguous), writes the final snapshot, rolls
   open issues to the next cycle when `cycle_rollover` is on (the job's query
   and rules, decisions-T3 #13), sets `completed_at = now`, and then runs the
   job's maintenance for the team in the same transaction (so a new upcoming
   cycle exists). The code is shared with the job: one function
   (`completeCycleTx`). Events carry the actor, `reason: "manual"` in the
   `cycle.completed` payload.
6. **Delete** only a cycle that is not completed and has NO issue (archived
   issues count, and the 409 message says "including archived issues" because the cycle page hides them; the foreign key would set `cycle_id` NULL with no version bump,
   no activity row, no event, so the server refuses instead: `cycle_in_use`,
   move the issues first). The next cycle absorbs its time (its start moves to
   this cycle's start; the chain stays contiguous). A cycle with no next cycle
   cannot be deleted while the team has cycles on (the job would make it again
   the same minute): `cycle_conflict`. A team with cycles off may delete its
   last cycle. After the delete, the job's maintenance runs in the same
   transaction (deleting the current cycle makes the next one current, and a
   new upcoming one is made). Snapshots go with the cycle (cascade).
   The cycle row is locked `FOR UPDATE` before the issue count: an issue
   write that names the cycle holds `KEY SHARE` on it, so the count cannot
   miss a concurrent assign; a write that loses the race fails on
   `issues_cycle_fk`, which `database.Retryable` maps to a clean 400 ("the cycle
   no longer exists").
7. **Events**, last in the transaction: `cycle.updated` (actor; `changes`),
   `cycle.completed`, new `cycle.deleted` (id, teamId, number), plus
   `cycle.updated` with `changes: ["startsAt"]` or `["endsAt"]` for each
   neighbour that moved, and the rollover `issue.updated` events with the actor.
8. **Web.** The `...` menu shows five rows. Every signed-in member has the
   `write` scope, so no row is hidden by role (a web session is never an
   agent). Mark complete and Delete need `admin`: for a non-admin (`useIsOrgAdmin`)
   they are disabled with "Only admins can complete or delete a cycle." A row that a rule forbids is disabled with a one-line reason
   (completed or past cycle: dates, complete, delete; upcoming: complete;
   last cycle: delete). The server is the judge: its message shows on a 409.
