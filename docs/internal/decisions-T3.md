> Internal design note from the Orbit deployment; kept for history.

# T3 decisions (ORB-3407): projects, milestones, cycles, documents

T3 adds the planning objects to the API and the cycle job that creates,
completes and rolls over cycles. Where the brief left a gap, T3 made the
smallest sensible choice. Each one is listed here so a later tranche can
change it on purpose. The routes and wire shapes are in
`server/api/openapi.yaml`. Attachments are ORB-3417, the MCP tools are T2,
and the web screens come later.

## Schema (migration `0003_planning`)

1. **The cadence columns were already in 0001** (`teams.cycles_enabled`,
   `cycle_length_days`, `cycle_start_weekday`, `cycle_start_time`,
   `cycle_timezone`, `cycle_rollover`, decisions-T1 #4). 0003 does not add
   them again. It adds:
   - `cycle_snapshots`: `triage_count`, `backlog_count`, `unstarted_count`,
     `canceled_count`, `started_points`, and one CHECK that no count is
     negative. The three point columns are `bigint` (0001's `scope_points`
     and `completed_points` are widened; see #18). The down migration turns
     them back into `integer`, clamping a sum above the int4 range: that is
     its only loss, for a value 0001 could not have stored.
   - Keyset indexes `milestones (org_id, updated_at, id)` and
     `cycles (org_id, starts_at, id)`; the partial index
     `cycles (team_id, number) WHERE completed_at IS NULL` for the job.
   - CHECKs: `projects.version >= 1`, `documents.version >= 1`, and a cycle
     name of 1-255 characters when set.
   The down migration removes exactly these. `TestMigrateUpDownUp` passes.
2. **No overlap constraint on cycles.** An `EXCLUDE USING gist` on
   `(team_id, tstzrange(starts_at, ends_at))` needs the `btree_gist`
   extension. Only the cycle job writes cycle dates, under one lock per
   team, so the application keeps cycles contiguous instead.

## Cycles

3. **Cycles enabled is off by default, and the seed does not turn it on.**
   The importer (T4) brings the Linear cycles with their numbers
   (cycle 7 and so on). If the seed enabled cycles, a fresh server would
   create its own cycle 1 first and the numbers would clash. An admin
   turns cycles on with `PATCH /orgs/{org}/teams/{team}`
   `{"cyclesEnabled": true}` after the import (or at once for a new team).
4. **`PATCH /teams/{team}` is new** (admin scope). It sets the name, the
   description and the cadence. None of the fields can be null. The zone
   must be an IANA name; `Local` is refused. When cycles are on after the
   change, the job runs for that team in the same transaction, so the
   current and the next cycle exist when the response arrives.
5. **Boundaries are computed on the local calendar.** A cycle ends
   `cycle_length_days` later on the team's local calendar, at
   `cycle_start_time` local, and the next cycle starts at that instant. So
   a Sunday 00:00 Europe/London cycle starts at 00:00 local on both sides
   of a DST change; the cycle that spans the change is one hour longer
   (October) or shorter (March). Adding 14 x 24 hours to an instant would
   drift by one hour instead. A start time that does not exist on a change
   day (01:30 on the March day in London) is normalised by Go's `time.Date`;
   A 00:00 start is never affected.
6. **The first cycle** of a team with no cycles starts at the latest cadence
   boundary (weekday and time, local) at or before now. So enabling cycles
   on a Wednesday makes the current cycle start on the last Sunday.
7. **A cadence change shapes new cycles only.** Existing cycles keep their
   dates. A new cycle always starts where the last one ends, so the chain
   has no gap. A length or time change applies from the next new cycle. A
   weekday change applies only to a team with no cycles: a chained cycle
   starts at the end of the one before it, whatever its weekday. (A
   restart after a long pause, #34, also realigns to the cadence.) To move a
   chain to another weekday, a later tranche can add an explicit
   "realign" action.
8. **One upcoming cycle.** After a run, the last cycle starts after now,
   so there is always the current cycle and the next one. Linear lets a
   team keep more upcoming cycles; one meets the brief ("the next cycle
   exists before the current one ends").
9. **The job runs at boot and then every hour**, not once a day. It is
   "daily" in what it delivers (one snapshot row per day, the next cycle a
   whole cycle ahead), but an hourly run rolls issues over within an hour
   of a boundary instead of up to a day later. Every run is idempotent; a
   run with nothing to do takes one lock and writes only today's snapshot
   row. The goroutine is owned by the server's background context and
   WaitGroup, like the event listener and the idempotency sweeper.
10. **Safe with two instances.** One transaction per team takes
    `pg_advisory_xact_lock(hashtextextended('devops:team-cycles:' || team_id, 0))`
    first, then reads the team. A second instance waits, then finds the
    work done. New numbers are `max(number) + 1` under that lock, so they
    are consecutive with no gap; `UNIQUE (team_id, number)` is the
    backstop. The team PATCH takes the same lock before its row lock (the
    job's order), and its row lock is `FOR NO KEY UPDATE`, because a cycle
    insert takes `KEY SHARE` on the team row (its foreign key) and
    `FOR UPDATE` would deadlock with the job.
11. **Catch-up.** A server that was off for weeks creates every missed
    cycle on the next run, in number order, once, and completes the ended
    ones oldest first. So an open issue walks along the chain (cycle 1 to
    2 to 3). **One run creates at most 1000 cycles for a team, and the
    cap is not an error** (review fix): the run stops creating, completes
    only the ended cycles that have a successor, commits, and the next run
    continues. Before the fix the cap returned an error, the transaction
    rolled back, and the team never moved again. The team PATCH uses the
    same rule.
12. **Completion.** An ended cycle (`ends_at` at or before now) that has a
    successor gets its final snapshot, then its rollover, then
    `completed_at = ends_at` (the time it ended, not the time the job ran)
    and a `cycle.completed` event. The successor is **the earliest cycle of
    the team that starts at or after the ended cycle's end**, found by
    time, not by `number + 1` (review fix): a gap in the numbers, as a
    partial import can leave, no longer fails the job. An ended cycle with
    no successor yet (only after the cap, #11) waits for a later run. A
    team with cycles turned off is left alone entirely, even its ended
    cycles: a rollover needs the next cycle, which only the job makes.
    **Failure is per team**: a team whose run fails (for example a zone
    that no longer loads) is logged and skipped, and the other teams still
    run; its next run tries again.
13. **Rollover.** With `cycle_rollover` on, the issues of the ended cycle
    whose state type is not `completed` or `canceled`, and that are not
    archived, move to its successor (#12) in the same transaction.
    Each moved issue gets `version + 1`, a new `updated_at` and one
    `issue.updated` event (`changes: ["cycleId"]`, `fromCycleId`,
    `toCycleId`, `reason: "rollover"`, no actor). The state does not change,
    so no `issue_state_changes` row is written. Closed issues (Done,
    Canceled, Duplicate) stay in the ended cycle. With the setting off,
    nothing moves; the cycle still completes. Archived open issues stay, so
    an archived issue is never silently changed.
14. **Events of the job have no actor** (`actor_id` NULL): `cycle.created`,
    `cycle.completed` and the rollover `issue.updated`. They are emitted
    last in the team's transaction, after every other write.
15. **Cycles are not created through the API.** (Re-dating, completing and deleting
    came with LAUN-172: `decisions-T27-cycle-options.md`.)
    Original text: **not created, deleted or re-dated.**
    `PATCH /cycles/{id}` changes only `name` (null clears) and
    `description`. A manual create would break the numbering rule and the
    contiguous chain.
16. **Issue `cycle=active`** keeps its T1 meaning (`starts_at <= now() <
    ends_at` and not completed). `GET /cycles?active=true` uses the same
    test.

## Snapshots (burn-up)

17. **One row per cycle per team-local day** (`UNIQUE (cycle_id,
    snapshot_date)`). Every write is an upsert, so the same day is
    rewritten, never duplicated.
18. **What is counted.** The issues whose `cycle_id` is the cycle and that
    are not archived, at the moment the row is written, by state type:
    `triage`, `backlog`, `unstarted`, `started`, `completed`, `canceled`.
    `scope_count` is all of them except `canceled` (a canceled issue is out
    of scope, as in Linear's burn-up). Points are the sum of `estimate`
    (no estimate counts as 0): `scope_points` (not canceled),
    `started_points`, `completed_points`. **Point sums are `bigint`**
    (review fix): an int4 sum overflowed with two estimates near 2^31, and
    the error rolled back the team's whole run every hour (no next cycle,
    no completion, no rollover; the snapshot GET and the team PATCH were
    500). Two layers now: the API bounds an estimate to 0-1000 (#33), and
    the sums are bigint, so data that never passed that check (old rows,
    an import, a direct write) cannot overflow either. The counts stay
    `integer`: a count is bounded by the number of issue rows in one
    cycle, far below 2^31. The rollover path has no sum; it returns each
    issue's `version` (int4, +1 per change).
19. **Who writes a row.**
    - The job writes today's row for the running cycle on every run.
    - At completion the job writes the final row, dated the cycle's last
      local day (the day before `ends_at`, local), BEFORE the rollover
      moves issues out, so the last point shows the cycle as it ended.
    - `GET /cycles/{id}/snapshots` refreshes today's row first while the
      cycle is running (`starts_at <= now < ends_at`, not completed), then
      lists every row, oldest first. An upcoming or completed cycle is
      only read. This is a derived write behind a read-scoped GET: it
      recomputes counts from rows the caller can already read, changes no
      user data and emits no event.
    - **An unchanged refresh writes nothing** (review fix): the upsert's
      `DO UPDATE` has a `WHERE (counts) IS DISTINCT FROM (EXCLUDED counts)`
      guard, so a read token calling the GET in a loop does not rewrite the
      row (no WAL, `updatedAt` kept). `updatedAt` is therefore the last
      time the counts CHANGED, not the last time they were checked.
    - The upsert refuses a completed cycle (`HAVING EXISTS ... completed_at
      IS NULL`), so a refresh that races the completion cannot overwrite
      the final row with post-rollover counts. A refresh whose statement
      started before the completion committed recomputes the pre-rollover
      counts, which equal the final ones.
20. **No backfill.** A day the server was down has no row. The final row
    of a cycle completed late (catch-up) holds the counts at completion,
    not at `ends_at`. History could be rebuilt from `issue_state_changes`
    later; T3 does not.
21. **The list is not paginated.** A cycle has at most 56 days, so
    `nextCursor` is always null.

## Projects, milestones, documents

22. **Project state timestamps** follow the issue rule (decisions-T1 #9):
    `started` sets `startedAt`; `completed` sets `completedAt` (and
    `startedAt` if empty) and clears `canceledAt`; `canceled` sets
    `canceledAt` and clears `completedAt`; `paused` keeps `startedAt` and
    clears the other two; `backlog` and `planned` clear all three.
23. **The project lead** is `me` or the username of an active org member
    (as `assignee` is). `lead_id` references `users`, not an org-scoped
    key, so the service checks the membership. Teams are keys or ids of
    the org (at most 50); `teams` on PATCH replaces the set, and a change
    to the set alone still bumps `version`.
24. **Milestones have no `version`** (0001 has none on the table), so their
    PATCH has no compare-and-swap. A milestone cannot move to another
    project (its issues are in the old one): `projectId` on PATCH is an
    unknown field (400).
25. **Document slugs** are `slug(first title, 50) + "-" + 12 random hex
    characters` (48 random bits), made once and never changed, so a link
    survives a rename. `GET`, `PATCH` and the comment routes accept the id
    or the slug. A value that parses as a UUID is tried as an id first and,
    if no document has that id, as a slug (a title such as
    "deadbeef cafe babe face" makes a UUID-shaped slug). A slug clash (practically impossible) is a 409 the caller
    retries.
26. **Lists.** Projects, milestones and documents are ordered by
    `updated_at DESC, id DESC`; cycles by `starts_at DESC, id DESC`. Each
    uses the issue list's opaque keyset cursor, tagged with its list, so a
    project cursor on `/documents` is a 400. Milestones are not ordered by
    `sortOrder` (a float keyset is awkward to make opaque and stable); the
    field is returned for clients to sort by. Project and document lists
    cut `content` to 400 characters unless `full=true`, with
    `contentTruncated`, as the issue list does for descriptions.
27. **Filters.** Projects: `state` (comma list), `lead` (username, `me`,
    `none`), `team` (key or id), `query` (name substring). Milestones:
    `project` (id). Cycles: `team` (key or id), `active`. Documents:
    `project` (id), `query` (title or content substring). All take
    `includeArchived`, `limit` and `cursor`; an unknown parameter is a 400.
    A `project` filter naming no project of this org is a 400 (a filter
    value is a reference, decisions-T1 #25), but a `team` key that matches
    nothing is an empty page, as in the issue list.
28. **Archiving** is not in T3's API (the columns exist). Lists hide
    archived rows unless `includeArchived=true`.

## Comments on documents

29. **The comment routes are shared.** `GET/POST
    /documents/{document}/comments` are new; edit and delete stay
    `PATCH/DELETE /comments/{comment}`, with the same author and moderator
    rules. A reply must be to a comment on the same document (or the same
    issue): anything else is a 400.
30. **`Comment.issueId` is now nullable** and `documentId` is new. Exactly
    one is set. An issue comment looks as before, plus `documentId: null`.
    Comment events keep `issueId` in the payload (null for a document
    comment) and add `documentId` for a document comment.

## The issue API

31. **Planning references are checked against the org and the issue.**
    `projectId` must be a project of the org; `milestoneId` a milestone of
    the org AND of the issue's project; `cycleId` a cycle of the org AND of
    the issue's team. Anything else is a 400 `validation_failed`. A
    milestone needs its project: the server does not fill `projectId` in,
    and a PATCH that moves the issue to another project while it keeps a
    milestone of the old one is a 400 (send `milestoneId` too, or null).
    Only what the write changes is checked, so an issue that already holds
    a reference keeps working. Before T3 the check was the org only.
32. **A completed or archived cycle can still be set** on an issue (to
    record work after the fact). Rollover never moves an issue into a
    completed cycle, because the next cycle of an ended one is newer.

33. **Estimates are bounded to 0-1000** on issue POST and PATCH (400
    `validation_failed` otherwise; `maximum: 1000` in the OpenAPI file).
    Linear's scales top out at 64 (exponential; Fibonacci at 21), and the
    web client has no estimate editor, so 1000 leaves room for any points
    scheme. Rows that already hold a larger value keep it; a PATCH that
    does not touch `estimate` is not refused for it.

## Turning cycles back on (review nit)

34. **After a long pause, the chain restarts at the current cycle.** When
    the team PATCH turns `cyclesEnabled` from off to on and more than one
    whole cycle has been missed (the cycle after the last one has already
    ended), the next cycle starts at the current cadence boundary
    (`FirstStart(now)`, never before the last cycle's end) instead of
    back-dating every missed cycle. So a team paused for six months gets
    two new cycles (the current and the next), not thirteen empty ones,
    and its open issues walk only through the cycles that already existed
    before landing in the current one. This is the one place the chain may
    have a gap in time; numbers still go on from max + 1. The job's own
    catch-up (a server that was off, #11) still backfills every cycle,
    because then the team never asked to stop.

## T4 importer contract

35. **Import cycles BEFORE turning cycles on.** The importer must:
    1. write the team's Linear cycles with their Linear numbers, dates and
       `completed_at` (cycles stay off, so the job does not touch them);
    2. set the cadence if it differs (`PATCH /orgs/{org}/teams/ORB` with
       `cycleLengthDays`, `cycleStartWeekday`, `cycleStartTime`,
       `cycleTimezone`);
    3. only then `PATCH /orgs/{org}/teams/ORB {"cyclesEnabled": true}`.
    The job then continues from the highest imported number. Turning cycles
    on first would make a cycle 1 that clashes with Linear's numbers. A
    gap in the imported numbers is fine (#12). If the last imported cycle
    ended more than a cycle ago, #34 applies.

## Not done, left for later

- No attachments (ORB-3417), no MCP tools (T2), no web screens.
- No snapshot backfill from history (#20).
- No "realign the chain" action for a weekday change (#7), and no setting
  for more than one upcoming cycle (#8).
- No archive / unarchive routes for the planning objects (#28).
- Milestone lists are not in `sortOrder` (#26).

## LAUN-170: issue counts on the project answers

- **Shape.** `Project.issueCounts` = `{total, scope, done, started, canceled}` on every project answer (list, single, create,
  update). `scope` = `total - canceled`; progress = `done / scope`. By state TYPE (`completed`, `started`, `canceled`).
- **What counts.** Every non-archived issue of the project, sub-issues included (the issue list hides archived issues by default,
  so the old browser count did the same). An empty project is all zeros; the client shows "No issues" and never divides by zero.
- **How.** `ProjectIssueCounts` in `db/queries/planning.sql`: ONE grouped read for a whole page of projects (`attachProjectCounts`,
  next to `attachProjectTeams`), filtered by `org_id`, using `issues_project_idx`. No migration, no cache: counts are live.
- **No event, no new route.** Read only. The list filters, cursor and the `full` flag are unchanged.
- **Measured (review):** EXPLAIN on 600k issues (two orgs, 50 projects each) plus 75k without a project uses a Bitmap Scan on
  `issues_project_idx` (org_id, project_id), no seq scan. 50 projects / 225k issues = 74 ms; one project / 6,000 issues = 8.6 ms.
  Cost is linear in the issues of the projects on the page; there is no cache.
- **Not verified:** a real browser; counts changed through git-integration paths reach the page only by the `issue.updated` event.
