> Internal design note from the Orbit deployment; kept for history.

# T14: manual issue order (LAUN-51)

A board drop must keep its place. Linear's model: every issue has a rank,
"Manual" ordering sorts by it, and a drop between two cards gets a rank
between theirs. This file holds the judgment calls. The code is in
`server/internal/tracker/rank.go` (maths, placement), `issue_write.go`
(create, PATCH), `issue_read.go` (list), `db/queries/issues.sql`.

1. **The column existed.** `issues.sort_order double precision NOT NULL
   DEFAULT 0` is from migration 0001; the importer already filled it from
   Linear. Migration 0008 gives it a meaning: a backfill and the index
   `issues_manual_order_idx (org_id, team_id, state_id, sort_order, id)`.
   The ticket said the column was missing; it was only unused.
2. **Rank type: `double precision`, ascending, smaller is higher.** This is
   Linear's own type, so imported values fit. A float64 round-trips through
   JSON and through the list cursor exactly. Ranks are bounded: a client
   `sortOrder` must satisfy |v| <= 1e12.
3. **A "column" is `(org, team, state)`.** A rank only means something next
   to the ranks of the same column. Archived issues stay in the column's
   order (an archived neighbour is a valid neighbour).
4. **Backfill (0008).** Per column, `row_number() * 1024` ordered by the old
   `sort_order`, then `created_at DESC`, then `id`. Imported issues keep the
   relative order of Linear's `sortOrder`; the never-ordered ones (all 0) go
   newest first, as a new issue goes to the top. Running it twice gives the
   same numbers. The down migration drops the index only: the column is
   from 0001 and the old values are not kept.
5. **A new issue goes to the TOP of its column** (like Linear): rank =
   smallest rank of the column less 1024 (0 for an empty column). Ranks
   therefore drift negative; that is fine for a double, and a rebalance
   resets them.
6. **A state change without a placement also puts the issue on top of the
   new column** (the status picker, MCP `save_issue`, and the GitLab move
   to In Review / In Testing). Otherwise it would keep a rank that means
   nothing in the new column. The GitLab move takes the team's rank lock
   first (item 10).
7. **Placement on PATCH** (`PATCH /orgs/{org}/issues/{issue}`): at most one
   of
   - `placeAfter` / `placeBefore`: an issue id or identifier. It must be in
     the same team (else `400 validation_failed`) and in the destination
     column, which is the NEW state when `state` is in the same body. It
     cannot be the issue itself (`400`) or something that is not an issue
     id or identifier (`400`). The server computes the rank and returns it
     as `sortOrder`.
   - `sortOrder`: a number, set as given (no neighbour maths). For scripts
     and imports.

   Two of them together are `400 validation_failed`. A neighbour that
   vanished (unknown, moved to another column; archived rows stay valid) is
   `409 stale_placement`, its own code (in `httpx.Codes` and the openapi
   `ErrorCode`), so the web app refetches the column without parsing text.
   A neighbour of another org is the same answer as an unknown one (a row
   of another org must not be told apart from a missing one); this differs
   from the ticket's "400 for cross-org", and is safer. `validation_failed`
   stays for malformed input. We chose an error over "fall back to the
   end" because a silent wrong place is worse than a refetch. With `state`, the move and the place are ONE update in
   one transaction: a refused placement changes nothing, and there is ONE
   `issue.updated` event with `changes` = `["state","sortOrder"]`.
8. **Where a rank goes.** After X: if X has a successor S (same column,
   leaving the moved issue out), the midpoint of X and S; else X + 1024.
   Before X: the midpoint of the predecessor and X; else X - 1024.
   A placement that leaves the issue where it already is changes nothing
   (no version bump, no event), so a retried drag is harmless. PATCH has no
   `Idempotency-Key` middleware, as no PATCH has.
9. **Rebalance.** A midpoint is refused when the gap is below 1/1024, or
   below 1e-9 of the ranks (a coarse float step), or when two ranks are
   equal. A gap of 1024 can be split 21 times before that. Then the whole
   column (leaving the moved issue out) is renumbered 1024, 2048, ... in
   its current order, in the same transaction, and the placement is
   computed again. A rebalance keeps `version` and `updated_at` of the
   other issues (a rank is not content; a `version` check of an editor must
   not fail because of someone else's drag) and writes no event for them.
   The moved issue's `issue.updated` payload has `"rebalanced": true` and
   `"stateId"`: a client that sees it refetches that column. The PATCH
   response of the dragging client carries `"rebalanced": true` too (an
   optional field of `Issue`, absent otherwise). A rebalance always counts
   as a change of the moved issue, even when its own new rank equals its
   old one: the row is written, the version moves and the event is emitted,
   so the signal cannot be lost. **Limit:** a reader that polls by
   `updatedAt` never sees rank changes of the OTHER cards; it must use the
   event or refetch the column.
10. **Concurrency.** A placement or a state change takes
    `pg_advisory_xact_lock('devops:issue-rank:' || team_id)` BEFORE the
    issue row lock; a create takes it too, and so does the GitLab webhook
    (`linkAndMove`, for every team it may move, in team id order, before
    its first issue row lock). In `UpdateIssue` the order is always: the
    org parent lock (only when re-parenting), then the team rank lock, then
    the issue row; a create takes the rank lock, then the team's counter row
    (so creates were already sequential per team). A rebalance writes many rows, so a
    transaction holding one of them first could deadlock with it; taking
    the team lock first removes the common case. The rebalance
    (`RebalanceColumn`) and the cycle rollover (`RolloverIssues`) both lock
    their rows in id order first (a materialised CTE with `FOR UPDATE ...
    ORDER BY id`), so those two cannot deadlock with each other. Result: two
    people dragging at once are serialised. Each drag is relative to a
    neighbour, so both stay valid: the last write wins per card, and two
    cards dropped after the same neighbour both sit right after it. The
    `version` field still works (`409 version_conflict`) when a client wants
    it.

    **Two narrow deadlocks remain.** This is a reduction, not a proof.
    (a) A re-parent (`PATCH parent`) of a card that is in a column that is
    being rebalanced: the foreign key check key-share-locks the parent row,
    which can be the dragged card, in an order the rebalance does not
    follow. (b) The hourly cycle rollover updates many rows under its own
    advisory lock, not the rank lock; it can meet a rebalance of a column
    that holds the same rows (the id order above removes this for two
    pure writers, but not against the FK locks of other statements).
    Effect in both: Postgres aborts ONE transaction with SQLSTATE 40P01. For
    a drag that is a `500` and nothing is written (the client retries); for
    the rollover it is one failed hourly run, which is idempotent and runs
    again an hour later. Both need a rebalance (rare: about 21 drops into one
    gap) at the same instant, so we accept them.
11. **List: `sort=manual`** (same as `orderBy=manual`; `sort` accepts no
    other value; `sort=manual&orderBy=createdAt` is `400`). Ascending
    `(sort_order, id)`. The cursor holds the last rank (`r`, a float64) and
    id: a keyset, exact with ties, because `id` is a total order even
    though uuidv7 is not monotonic inside a millisecond. A cursor is tied
    to its `orderBy` as before. Ranks are per column, so a list over
    several states or teams interleaves the columns by number; the board
    asks for one `team` and one `state` per column.
12. **Importer.** It maps Linear's `sortOrder` into `sort_order` on INSERT.
    On an UPDATE (a re-run) the rank is kept, so a re-run after the cutover
    never undoes a drag and "a second run changes nothing" holds. One
    exception: when the export moves an issue to another state, the issue
    goes to the top of its new column (the old rank means nothing there;
    no team lock, a tie is harmless). An issue that is first imported
    LATER (new in a later export) keeps Linear's raw value, which does not
    fit the 1024 steps of columns that were renumbered. We keep it on
    purpose: a first import is the only case where Linear's order is the
    truth, and normalising to "top" would destroy it. After the cutover,
    issues should not be first imported at all; if one is, its place in the
    column is arbitrary until the next rebalance repairs ties and small
    gaps.
13. **MCP.** `save_issue` does not send a placement. It still works: the
    `Issue` object has one more field, and a state change from MCP goes to
    the top of the new column (item 6).
14. **Not done here.** No bulk reorder endpoint, no "move to the bottom" on
    create, no per-view order (one order per column for the whole team, as in
    Linear's Manual ordering). The web drag is LAUN-29.
15. **Direction of Linear's `sortOrder` (not established).** The importer
    and the backfill assume ASCENDING = top of the column, like our own
    ranks. Nothing in this repo proves it: the GraphQL query in
    `internal/importer/linear/export.go` only fetches the field, and no
    recorded export is in the repo (fixtures are hand-written). If the real
    imported columns turn out upside down, flip them once, before the web
    release, with `UPDATE issues SET sort_order = -sort_order WHERE org_id =
    '<org>'`: a negation reverses every column exactly and keeps ranks
    distinct. Do not do it after people have dragged cards.
16. **Cursors.** `decodeCursor` now requires `t` for every list except a
    `manual` cursor, which needs `r`; a cursor of another list or order
    stays `400`.
17. **Events carry what a client needs to place a card.** Every
    `issue.updated` that has `sortOrder` in `changes` also carries
    `sortOrder` and `stateId` in its payload (the GitLab move too), and
    `issue.created` carries `sortOrder` and `stateId`. A client can put the
    card in its column without a refetch; only `rebalanced: true` asks for
    a refetch of the column (item 9).
