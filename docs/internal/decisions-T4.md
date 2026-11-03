> Internal design note from the Orbit deployment; kept for history.

# T4 decisions (ORB-3408): the Linear importer

> Later: the web app runs the same importer (LAUN-8, LAUN-10; see
> `decisions-T17-git-connections-import.md` #36 to #45). This note covers the
> command line.

`server/cmd/importer` moves a Linear workspace into dev-ops. Where the
brief left a gap, T4 made the smallest sensible choice. Each one is listed
here so a later tranche can change it on purpose. The code is
`server/internal/importer` (plan, import, verify) and
`server/internal/importer/linear` (the GraphQL export).

## Shape

1. **Three subcommands.** `export` reads Linear into a directory of JSONL
   files; `import` writes that directory into Postgres; `verify` compares
   the two. Export never touches the database and import never touches
   Linear, so an import can be repeated, dry-run and tested offline from
   one export.
2. **The export is Linear's own shape.** Each JSONL record is the GraphQL
   node as Linear returned it (the Go types in `internal/importer/model.go`
   mirror the selections in `linear/export.go`). `manifest.json` holds the
   count of every file and is written last; import refuses an export whose
   files do not match it (a truncated or hand-edited file).
3. **A pure plan.** `BuildPlan` works out, from the export alone, what will
   be written, what is excluded and why, the label and relation merges, the
   cycle number gaps, the cadence, the burn-up rows and the issue counter.
   Import and verify use the same plan, so they cannot disagree about the
   expected counts, and the rules are unit-tested without a database.
4. **The store layer is sqlc.** Every write is a query in
   `db/queries/importer.sql` (generated into `internal/dbq`). No migration:
   0001 already has `import_mappings` and every column the importer needs.

## Read-only and the key

5. **The key is read from `LINEAR_API_KEY` only**, sent in the
   `Authorization` header to `api.linear.app` and `uploads.linear.app`, and
   never logged, written or put in an error (tests check the error paths).
   A redirect off the uploads host drops the header explicitly: net/http
   only drops it for another hostname, not another port.
6. **Mutations are refused before any request.** `Client.Query` accepts
   only a document that starts with `{` or `query` and contains no
   `mutation` or `subscription` keyword; a test sends a mutation and sees
   zero requests.
7. **Rate limits.** Every list is paginated (`first`/`after`, page sizes
   kept under Linear's complexity limit). A 429, a `RATELIMITED` GraphQL
   error or a 5xx is retried up to 8 times with back-off; `Retry-After` or
   `X-RateLimit-Requests-Reset` is honoured (one wait is capped at 15
   minutes). When `X-RateLimit-Requests-Remaining` falls to 5, the next
   request waits for the reset instead of hitting 429.
8. **Issue history** comes from each issue's nested `history` connection
   (50 per issue in the list query); an issue with more is followed with
   its own cursor. Linear has no root `issueHistory` list.

## Ids, timestamps, idempotency

9. **Ids are UUIDv7 minted from the original `createdAt`** (`NewV7At`: the
   48-bit millisecond time is the Linear time, the rest random), so an
   imported row sorts by its real creation time. A burn-up row's id is
   minted from its date.
10. **Every upsert is keyed on the primary key** with a
    `WHERE (columns) IS DISTINCT FROM (EXCLUDED columns)` guard. An
    unchanged row is not written at all (no new `updated_at`, no `version`
    bump), and the statement returns no row, which counts as a skip. So a
    second run with the same export reports zero creates and zero updates.
11. **Counts.** *create*: a new row. *update*: an existing row changed, or
    an existing row adopted for the first time (a seeded state or label,
    the seeded team, a relation pair made through the API), or a mapping
    only (a merged label or relation, a user). *skip*: nothing changed.
    Users are mappings, not rows: `create` for a user means "first mapped".
12. **`import_mappings` is the memory.** Each Linear id maps to the row it
    became (`entity_type` as in `plan.go`; issues use `issue`, which
    `tracker.resolveIssueID` reads, so `GET /issues/{linear-uuid}` works).
    A mapped row that was deleted after an import is written again with
    its old id. The importer never deletes a row whose Linear record is
    gone; it only stops counting it.
13. **An update overwrites with Linear's values.** Until cutover Linear is
    the source of truth, so a re-run replaces an imported row's fields
    (and bumps `version`, so a compare-and-swap client notices). Edits made
    in dev-ops to an imported row are lost on a re-run; do not edit
    imported rows before the last import.
14. **No change-feed events.** The import runs once, before anyone uses
    the org; thousands of `issue.created` events would only flood the feed.
    A client that was open during an import reloads.

## One transaction

15. **The whole import is one transaction** behind an org advisory lock
    (`devops:import:<org>`). It is all-or-nothing; `--dry-run` is the same
    transaction rolled back, so its counts are exactly what a real run
    would do; and the verification runs inside it before the commit, so a
    mismatch rolls everything back. This goes against "keep transactions
    short" (supabase `lock-short-transactions`) on purpose: it runs at
    cutover with no other writer, and the rows it locks are its own new
    rows plus the ORB team, counter and cycle lock. Every statement still
    has the pool's 30-second `statement_timeout`.
16. **Row by row, not batched.** Measured on a large export (17,757
    records, roughly 50,000 statements with the mappings and labels): the
    whole import transaction takes about 4 seconds over the compose
    network. Batching would make the per-row outcome (create / update /
    skip) and the per-row error message harder to get for no real gain.

## Mapping rules

17. **Teams** are matched by key; ORB adopts the seeded team (its name and
    states stay the seed's). A Linear team with no dev-ops team is made
    with `tracker.CreateTeamTx`, as the API would.
18. **States** are matched by name within the team (case-insensitive), so
    Backlog, Todo, In Progress, In Review, In Testing, Done, Canceled and
    Duplicate adopt the seeded rows and get Linear's colour, position and
    description; any other state is created. Linear's `duplicate` state
    type is `canceled` in dev-ops (decisions-T1 #8).
19. **Labels** are unique per org ignoring case in dev-ops, but Linear's
    team labels can repeat a name. Labels of the same name are merged into
    the oldest; the others are mappings to it. Groups are written before
    their children; the seed's Bug / Improvement / Feature are adopted.
20. **Users** go through the user map file (`--user-map`): a Linear user id
    or email to an existing username such as `admin` or `claude`, with an optional `default`. With no
    default, an unmapped user stops the import. The export writes
    `usermap.template.json` (every Linear user with a guess, no emails)
    to start from. Linear users are never created in dev-ops.
21. **Issues** keep their Linear number, state, priority (the same 0-4
    scale), timestamps, sort order, due date, project, milestone and cycle.
    Estimates are rounded to whole points (dev-ops estimates are integers).
    A title over 500 characters is cut. A cycle of another team or a
    milestone of another project is dropped (the API rule, decisions-T3
    #31). A parent written after its child is set in a second pass that
    keeps `updated_at`. **Trashed issues are not imported** (they were
    deleted in Linear); their numbers stay unused.
22. **State history.** Every history entry that changes the state becomes
    an `issue_state_changes` row with `source = 'import'` and its original
    time and actor. When Linear has no "from nothing" entry, a creation
    row (from NULL) is made at the issue's `createdAt` into the first
    state, so the burn-up sees the start as for an issue made in dev-ops
    (decisions-T1 #10). An entry naming a state that is not in the export
    is excluded.
23. **Comments** on an issue or a document are imported with their thread
    parent, author and edit time. A comment on a project update (or
    anything dev-ops has no table for), an empty one, or one on an issue
    that was not imported is excluded. **An archived comment on an open
    issue is excluded as deleted; an archived comment on an archived issue
    is kept** (it was archived with its issue). Relations and attachments
    follow the same rule. Measured on 2026-10-24: the measured export has no
    archived issue, comment, relation or attachment, so the rule changed
    nothing; the one excluded comment is not on an issue or a document.
24. **Relations.** `blocks`, `related` and `duplicate` (as `duplicate_of`)
    are imported; `similar` (Linear's AI suggestion) is not. dev-ops keeps
    one row per unordered pair and type, so a Linear pair in both
    directions is merged into the first.
25. **Documents** keep Linear's URL shape as their slug,
    `slug(title, 50)-<slugId>` (Linear's slugId is the same 12-hex form
    dev-ops mints, decisions-T3 #25). Trashed documents are not imported.
26. **Projects** take their state from Linear's status type (the same six
    names) and keep their team set. **Milestones** keep their project,
    target date and sort order.
27. **The issue counter** is raised to `max(number) + 1`, at least the
    `--counter-floor` (default `ORB=3405`), and never lowered. Linear's
    own ORB tickets above 3404 are imported with their numbers, so the
    next new dev-ops issue is the first free number after them. Measured
    on 2026-10-24: Linear already holds ORB-3405 to ORB-3418 (the dev-ops
    tickets themselves), so the counter becomes **3419**. Setting it to
    exactly 3405, as the ticket comment asked, would hand out numbers that
    the import has just used; the AC ("ORB-3405 or higher") holds.

## Cycles (the decisions-T3 #35 contract)

28. **Cycles are imported with their Linear numbers while cycles are off**,
    under the cycle job's per-team lock. The importer refuses to run when
    any imported number is already held by a cycle it did not make (cycles
    were turned on first), and names the numbers. A gap in Linear's
    numbers is reported (`cycle numbers missing in Linear`) and is fine
    (decisions-T3 #12).
29. **The live cadence** is written only while cycles are off: length from
    Linear's `cycleDuration` (weeks), weekday and start time from the
    newest cycle's start read in the team's zone (what Linear actually
    did), the zone from the team. Once an admin has turned cycles on, the
    importer leaves the cadence alone. Measured on 2026-10-24: the
    Linear cycles are 14 days from **Monday** 00:00 Europe/London (Linear
    `cycleStartDay` 1), so that is what the import sets, not the Sunday of
    the 0001 defaults. Linear had also pre-made the upcoming cycles 8 to 17;
    they are imported, so the job makes nothing new until cycle 17 starts.
30. **The burn-up history** comes from each cycle's history arrays: entry
    `i` is the cycle's `i`-th local day. `scope_count`,
    `completed_count`, `scope_points`, `completed_points` and
    `started_points` are filled; Linear has no daily history of the
    started, triage, backlog, unstarted or canceled COUNTS, so those are
    0 in imported rows.
31. **The importer never turns cycles on.** After the import, an admin
    sends `PATCH /api/v1/orgs/orbit/teams/ORB {"cyclesEnabled": true}`;
    the job then goes on from the highest imported number (tested: a
    fixture with cycles 7, 8, 10 continues at 11).

## Attachments (T3b not merged)

**#33 to #35 are superseded by `docs/internal/decisions-T4b.md` (ORB-3419):** the
files are uploaded, the texts rewritten and a Linear upload attachment is a
`file` row. #32 (the export side) still holds.

32. **Files are exported, not uploaded.** The export downloads every
    `uploads.linear.app` URL found in descriptions, comments, documents,
    project and milestone texts and attachment links into
    `files/<sha256(url)[:16]>/<safe name>` (0600, under an `os.Root`), with
    `files.jsonl` as the manifest. Limits: 100 MB a file, 10 GB in all. A
    failed download is recorded with its status, not fatal; a second
    export reuses files it already has.
33. **The upload step is a seam.** `importer.FileStore` is the interface
    T3b (ORB-3417) implements: put one file, write its `file` attachment
    row when an issue refers to it, return the API download URL. With no
    store (today), every `uploads.linear.app` URL stays as it is (it keeps
    working while Linear stays read-only) and the report counts them as
    pending. A later run with a store rewrites the texts; those rows then
    count as updates.
34. **Linear attachments are `link` rows**, including one whose URL is an
    upload (its URL goes through the same rewrite).
35. **What T3b still has to decide:** a file that only a document, a
    project or a milestone refers to has no issue, and `attachments.issue_id`
    is NOT NULL.

## Verification

36. **Verify counts, per entity, the plan's Linear ids whose mapping points
    at a row that exists**, and compares that with the plan (export minus
    exclusions). A lost row is a mismatch; a mapping left from an older
    export is not counted. Burn-up rows are checked as (cycle, date) pairs.
    `import` verifies inside its transaction and again after the commit;
    `verify` does it alone. A mismatch exits 3.
37. **The importer refuses a database that is not at its newest
    migration** (it never migrates: the server does that at boot, and a
    dry run must change nothing).

## Not done, left for later

- The upload step itself (#33, ORB-3417).
- `linear.app/.../issue/ORB-n` links in texts are not rewritten to
  dev-ops links; they keep working while Linear is read-only.
- Rows deleted in Linear after an import are not deleted in dev-ops (#12).
- No snapshot backfill for the per-type counts (#30).
