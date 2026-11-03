> Internal design note from the Orbit deployment; kept for history.

# Decisions T23: remembered view on the server (LAUN-141)

Follow-up of LAUN-109 / LAUN-142 (`docs/internal/web-decisions.md`). The layout and the sort of an issue page
are kept in the browser per place. This note makes the choice follow the person to another device.
Design choices were made by the builder (no review round yet); judgment calls are marked.

1. **What is stored:** per person, org and place: `view` (`list`, `board`), `sort` (`updated`, `created`,
   `priority`, `manual`) and `at` (the client's time of the choice, epoch ms). The place key is the one the web
   builds (`team:orb`, `cycle:orb:3`, `product:web`, `all`, `mine`); the server accepts `^[a-z0-9._:-]{1,200}$`.
   Filters are not stored (as LAUN-109 #2). Grouping and visible properties do not exist yet; a new column
   (migration) adds them when they do.
2. **Table `user_view_preferences`** (migration 0022): PK `(org_id, user_id, place)`, composite FK to
   `org_members` with `ON DELETE CASCADE`, CHECKs on place, view and sort. No `id`, no `updated_at`: `changed_at`
   is the client time.
3. **Routes** (read scope reads, write scope writes, agents `403 forbidden` through `refuseAgents`):
   `GET /orgs/{org}/me/views` and `PUT /orgs/{org}/me/views/{place}`. No DELETE: a reset is a PUT of the defaults
   (`list`, `updated`), so it reaches other devices and an older choice cannot come back. Judgment call.
4. **No `events.Emit`.** User-private rows, like the notification settings (decisions-T13 #32, #34): an event
   would expose a person's choices on the org-wide feed. Own rows only; there is no admin read.
5. **Conflict rule: the newer `at` wins, a tie keeps the stored value.** One SQL upsert with
   `WHERE EXCLUDED.changed_at > changed_at`. The PUT answers the value now stored, so the client sees who won.
   An `at` ahead of the server clock is set to the server clock (a wrong device clock cannot freeze a place
   against later writes). Client clocks can differ by seconds; accepted for a view preference.
6. **Cap: 100 places per person and org** (the same number as the browser). After an insert or update the
   oldest by `changed_at` are deleted in the same transaction. A reset row counts as a place.
   If the write itself is among the oldest (100 newer places exist), it is trimmed at once; the PUT then answers
   `200` with the state that is stored for the place, which is the default (`list`, `updated`, `at` 0) because
   there is no row, so the answer and the next GET agree. Each PUT takes `pg_advisory_xact_lock` on a hash of
   (org, user) first, so the cap is strict under concurrent writes of one person.
7. **Web: localStorage stays the fast path.** The page renders from it at once. Once per page load and org
   (a `useQuery` with `staleTime: Infinity`) it reads the list and merges per place by `at` (`reconcile`): a newer
   server value replaces the local one; what is newer here, or missing on the server, is sent with a PUT.
   A local reset keeps its time in a second blob (`launchpad.viewsync.v1:<org>:<user>`), because the local store
   keeps no entry for the defaults (LAUN-142 #2). Every send is best effort and its failure is ignored, so
   offline works; the next page load repairs it. An old build's entries (no server row) are sent at the first load.
8. **Not done:** a change made on another device shows at the next page load, not live (no push, no refetch on
   focus). The server value is not re-merged while a page stays open. Judgment call to keep the change small.
9. **Tests:** `internal/api/views_integration_test.go` (store, conflict, validation, cap, agents and scopes,
   cross-org), `internal/api/openapi_notify_test.go` (schemas), `viewPrefs.test.ts` (merge) and
   `viewPrefs.page.test.tsx` (server board shows on a new device, a choice is sent, server down).
   Not verified in a real browser or with two real devices.

## LAUN-163: board column choices

10. **Two more fields per place:** `hideEmpty` (bool, default false) and `hiddenColumns` (state names, default none, at most 30 of
    1-100 characters, no control character or comma, duplicates dropped). Migration 0024 adds `hide_empty` and `hidden_columns`
    (text[]) with a size CHECK. Additive: an old row reads as the defaults and the answer always holds `[]`, never `null`.
    A server change was needed because the table is typed (CHECKs on the columns), not a JSON blob.
11. **A PUT replaces the whole row.** An absent `hideEmpty` / `hiddenColumns` means the defaults. An old browser tab that sends only
    layout and sort therefore clears the column choices of that place (accepted: the tab is stale; the next load of a new
    build sends them again). The conflict rule (newer `at` wins) covers the columns with the rest of the row.
12. **Names, not ids.** The board groups by state NAME (a name shared by several teams is one column), so the choice stores names.
    A state that is renamed or removed leaves a dead name; the web ignores names that are not columns and drops them at the next change.
13. **Tests:** `TestViews_Columns` (store, older write loses, absent fields reset, old row reads as defaults, two places independent,
    validation, null list).
14. **Review fixes (LAUN-163).** The `hidden_columns` CHECK counts BYTES (12100 = 30 names x 100 runes x 4 bytes + 29 commas); the Go
    rune limits stay the contract, so 30 x 100 CJK runes store and 31 names are `400`. The web drops C1 controls (U+0080-U+009F)
    too, as the server does. "Reset view" ignores hidden names that match no state (the Columns button does the same); on a
    scoped page (product) a name that is a state of another team still counts for Reset view but not for the button.
15. **Round 2 fixes (LAUN-163).** Production code: `useHotkeys` ignores plain keys from inside an element marked
    `data-no-hotkeys` (the Columns panel; a checkbox there no longer triggers j/k/x/s/a/l/p or Enter); on a phone the panel anchors
    to the fold (full width of the bar, never past the right edge) instead of the button; `toggleColumn` returns a `reason`
    (`last` or `cap`) and the panel says "You can hide at most 30 columns." for the cap. Tests only: saveStored keeps the columns,
    outside click closes, C1 control (U+0085) on the server. `customised` (Reset view) uses the names of ALL states and the
    Columns button the names of the place's own board columns; they differ only on a scoped page (product) for a name that is a state
    of a team outside the product. Kept as is, on purpose: the hook has no view of the scoped columns.

## LAUN-171: group the board by assignee, priority, label or project

The board grouped by state name only, and a drop changed the state. The project owner's decisions (2026-10-30) are below; the
choices marked "judgment call" are the builder's.

16. **A group-by per place.** Migration 0025 adds `group_by` (`state` default, `assignee`, `priority`, `label`,
    `project`; text with a CHECK) to `user_view_preferences`. The API field is `groupBy` (enum `ViewGroupBy`); the
    service refuses any other value with `400 validation_failed`. Absent in a PUT means `state` (a PUT replaces the
    whole row, #11). An old row reads as `state`. The URL carries `?group=` (omitted for `state`); the address wins over
    the stored value, as for `sort` (#7 of LAUN-109). The control is a "Group:" select in the board toolbar, board
    layout only. Not a filter, so "Clear" keeps it; "Reset view" sets it back to `state`.
17. **Columns.** Assignee: every active member (A to Z) plus "Unassigned" last. Priority: the five fixed columns, "No
    priority" last. Label: the labels of the team (or the org) by name, plus "No label". Project: the org's projects,
    plus "No project". A value that no list holds (a deactivated member, an archived project) still gets a column, so
    no card is lost. A card with several labels shows ONCE, under its first label (a card needs one column to be dragged;
    judgment call, the card shows all its labels as before).
18. **Drag rules.**
    - State columns (unchanged, LAUN-167): a drop sets the state. In Manual it also saves the place.
    - Assignee columns: a drop sets the assignee; "Unassigned" clears it. Priority columns: a drop sets the priority
      ("No priority" is 0). One PATCH per card; a block drag sends one per selected card. No place is sent.
    - Label and project columns: drag is OFF. A card can have many labels and a project is not a quick change. The
      columns, the counts and the card links work; the board hint says why.
    - A drop in the card's own column is a cancel for assignee and priority columns.
19. **"Hold Ctrl or Command to also change priority"** (the project owner's Linear reference). Precise rule, my reading, because
    a drop on a state or assignee column carries no priority: it applies only when the sort is NOT Manual and the
    columns are states or assignees. When the key is held at the pointer release (`metaKey || ctrlKey`), the dropped
    card (or each card of a block) also takes the priority of the card it lands next to: the card just above its
    place, else the card just below; an empty column changes nothing. It is sent in the same PATCH as the state or the
    assignee. In a state column it also works inside the card's own column (the priority changes, the state does not);
    without the key such a drop is still a cancel. It is meaningless for priority columns (the drop already sets
    it), for label and project (no drag) and in Manual (the drop saves a place, which is not a priority); there the key
    does nothing and the hint does not offer it. I apply the neighbour rule to every non-Manual sort, not only to
    Priority, so the hint (which the project owner wants in every non-Manual sort) is always true; it is most useful when the sort
    is Priority. Judgment call.
20. **The drag hint.** While a card is dragged in a non-Manual sort over a board that drags, a small panel shows "Board
    ordered by <sort>" and, when #19 applies, "Hold ⌘ to also change priority" (the key is named "Ctrl" off Apple
    systems). It sits above the board, takes no pointer and is `aria-hidden`; the screen-reader line is the existing
    live region. No new drag code: `boardDrag.ts` (LAUN-29, LAUN-167) gained the key at release, the neighbour card
    and an "active" callback; the drop callback decides what it means.
21. **Manual order per group kind.** Only state columns keep a stored order: the rank is per `(team, state)`
    (decisions-T14 #3), so it means nothing in another kind of column. In assignee and priority columns the Manual
    sort shows the server's order and a drop does not reorder. Judgment call; it keeps the change small and adds no
    rank rules.
22. **Hidden columns per group kind.** Same two fields (#10), no new column: a state column is its bare name (as
    before); a column of another kind is `assignee:<username>`, `priority:<n>`, `label:<lower-case name>` or
    `project:<id>`. Each kind's Columns panel reads and changes only its own entries, so hiding Claude on the assignee
    board does not touch the status board, and each comes back when you switch. `hideEmpty` is ONE flag for all kinds
    (judgment call). The 30-entry cap is shared. A name with a comma cannot be stored (the server refuses it): such a
    label column cannot be hidden. The same holds for a `label:<name>` entry over 100 characters: the web drops it and the
    server would refuse it, so that column cannot be hidden either. A state named like `label:x` would read as a label entry; accepted.
23. **No new route, no new body that names another org's row.** The group-by lives on the existing own-rows-only
    PUT/GET, so the cross-org rule is the one tested in `TestViews_CrossOrg`. Tests: `TestViews_GroupBy` (enum, absent
    = state, older write loses, old row, CHECK), `groupBoard.test.tsx` (real drop sequences and request bodies, the key,
    drag off, hint text, round trip, hidden per kind), `groupModel.test.ts`.
