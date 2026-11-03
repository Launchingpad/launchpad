# Decisions T43: undo and redo on the board and the list (LAUN-236, LAUN-237)

Status: built, web only. The board came first (LAUN-236); the issue list got
the same undo next (LAUN-237, section "The list" below). Every rule here holds
for both views unless that section says otherwise. The project owner asked (2026-11-02): "on the web board, add
ability to do cmd+z to undo the last change, create etc". The ticket gave a
default design; the builder checked it against the real API. No server change:
every undo is an ordinary `PATCH /issues/{issue}` with the `version` check, so
it shows in the activity feed and the change feed like any other change.

## Keys and scope

1. **Keys.** Command+Z on an Apple system, Ctrl+Z elsewhere, undoes the last
   change made on the board in this tab. Command+Shift+Z / Ctrl+Shift+Z redoes
   it; Ctrl+Y redoes too, but not on a Mac (there Command+Y is the history
   window in some browsers). Ctrl on a Mac and Command elsewhere do nothing, so
   no system shortcut is taken. The letter comes from `key` when it is a Latin
   letter (so AZERTY and QWERTZ follow their own labels: on AZERTY the Z key is
   `code` KeyW); on a non-Latin layout (Cyrillic, Greek...) the physical key
   (`code` KeyZ / KeyY) counts, as the system's own Cmd+Z does.
2. **Where the keys are not ours.** Nothing happens, and the event is not
   prevented, when focus is in an input, a textarea, a select or a
   contenteditable (the browser's own text undo wins), while a dialog, a menu,
   a picker or a listbox is open, and inside a `[data-no-hotkeys]` panel.
   While the pointer still holds a card, a press writes nothing and the toast
   says "Drop the card first, then undo." A press right after the release (the
   card is still landing, about 200 ms) waits for the drop's write and then
   undoes that drop. The state is read from the drag machines
   (`boardDragState()`), not from the body class, which stays a little longer.
3. **The stack.** The last 20 actions (`UNDO_CAP`), in this tab, in memory
   only. A new action empties the redo stack. The stacks are emptied when the
   board leaves the screen (another page, the list view) and when its scope
   changes: another team, product, cycle or project, or another path (`scope`
   in `IssueBrowser.tsx`: All issues and My issues are the same page instance,
   so the path and the "mine" lock are in it). The list has its own stack
   (LAUN-237, #16). A toast outlives the page: its Undo or Redo button does
   nothing once the page has gone. Two guards, kept as a pair on purpose: the
   unmount empties the stacks (and the rank memory), so nothing of the page
   stays in memory; the `mounted` flag makes the button silent (with the empty
   stacks alone it would still toast "Nothing to redo" on another page).
4. **One key press, one action.** A group drop, or a picker (`S`, `A`, `L`,
   `P`) on a selection, is ONE entry: every write started in the same task
   joins it. A drop or a picker is recorded only after its writes come back, and
   a press while one is still being saved waits for it (at most 10 s, then a
   toast says to press again), so the press undoes that action and not the one
   before.

## What each action's undo writes

5. **Move a card (state column and manual order).** Undo sends the old
   `state` (when it changed) and the old `sortOrder`, ALWAYS when the state
   changed or a place was sent, also when the rank is the same number. A move
   to an empty column gives rank 0 (the column's top), so a card that had rank
   0 keeps it; without the rank in the undo, the server would put the card on
   top of its old column (found in review on a real stack). `sortOrder` sets the rank
   directly (T14), so the card goes back to its exact place without naming a
   neighbour; a neighbour may have moved since, and `placeAfter` would then be
   `409 stale_placement`. Redo sends the new state and the rank the server
   returned. If the column was renumbered in between (T14 #9), the old rank may
   land the card a little off; the order of the other cards is not touched.
5a. **The "before" rank is the server's, not the cache's.** A drop moves the
   card in the cached list, but the cached `sortOrder` stays the old number
   until a list refetch lands, and each new write cancels the pending refetch.
   So a second move of the same card before a refetch would record the rank
   from before the FIRST move, and its undo would land the card in the wrong
   place (found in review on a real stack). The hook keeps the rank of each
   card from the last answer of a write made here (a map by issue, emptied with
   the stacks); a change takes that rank as its "before" when it is from the
   same or a newer version than the issue the page showed. This also covers a
   second drop made while the first write is still in flight. Only a newer
   version replaces a remembered rank: a write that sends nothing (a no-op
   patch) answers with the cached issue, whose version is current but whose
   rank may be stale. Tests kill three mutants: no version guard (a newer edit
   by someone else must win), a rank never replaced (a third move), and `>=`
   in place of `>` (the no-op).
6. **Field changes from the board**: status, priority, assignee (pickers and
   drops on assignee or priority columns, Ctrl/Command drops), labels (the `L`
   picker). Undo sends the old value of each field the write changed; labels as
   `addLabels` / `removeLabels` of exactly the labels the write added or removed.
   A write that changed nothing is not recorded.
6a. **Ids, not names.** States and labels are sent by id (the API takes a name
   or an id), so a rename between the change and the undo does not break it.
   The toast keeps the names as they were at the change. The assignee is sent by
   username: the API takes no member id there (a username does not change).
7. **Create (lane "+" or `C` while the board is on screen).** A member cannot
   delete an issue (T32: admin scope), and a delete has no version check and
   cannot be redone. So undo of a create, for everyone, moves the issue to its
   team's canceled-type state (the first by position), and the toast says
   "undo cancels a new issue; it does not delete it". Redo puts it back in the
   state and at the rank it was created with (without the rank the server
   would put it on top of the column, T14). A team with no canceled-type state: the toast says
   the create cannot be undone, and the entry is dropped. Files attached in the
   dialog stay.
7b. **A new issue in use is not canceled (LAUN-237 review).** A comment or a
    sub-issue does not raise the issue's version, so the compare-and-swap (#9)
    would not see it, and the undo would cancel an issue someone already works
    on. So, before the undo of a create, the hook asks the server for the
    issue's comments and for its sub-issues (`parent=`, `limit=1`). If there is
    any, the change is skipped and dropped; the toast says "ORB-90 has comments
    or sub-issues now; undo skipped". A failure of these reads follows #10 to
    #12 (a 404 is "gone", the network keeps the entry). The reads run one after
    the other, comments first: for a deleted or hidden issue that read is a
    `404`, whereas the sub-issue list answers `400 validation_failed` "unknown
    parent" (the parent is resolved with the caller's team access); read in
    parallel, whichever failed first would pick the toast (review). That `400`
    is also taken as "gone", for an issue that goes between the two reads. Chosen over the cache
    (it may not hold the comments or the sub-issues) and over a note in the
    record (a silent cancel of a used issue is the worse error). Not covered:
    a sub-issue in a team the person cannot see (T20: the list does not show
    it), and a comment written between the check and the write (milliseconds).
8. **Not on the board, so not covered:** archive (the API has no issue archive),
   delete (admin, issue page only), title, description, project, cycle,
   estimate and due date (the board does not edit them; label and project
   columns do not drag). Comments are not board actions.

## What an undo does not put back, and what it sets off

8a. **Timestamps are not restored.** The server sets `startedAt`,
    `completedAt` and `canceledAt` from the state a write moves to, at the time
    of that write. An undo is such a write, so the times are the undo's: a Done
    card dragged to Todo by mistake and undone gets a new `completedAt` (its
    first completion date is lost); the undo of a create sets `canceledAt` and
    the redo clears it. The API has no way to set these times, and this ticket
    adds no server change, so this is accepted on purpose. The state history
    (`issue_state_changes`) keeps both moves, so the first date can still be
    read there.
8b. **Side effects.** Each undo and each redo is an ordinary update. It writes
    activity rows and, for a state change, a state-change row; it emits
    `issue.updated` on the change feed; so the notification consumer acts on
    it (an undone unassign is "assigned to you" again, with email, inbox and
    mobile push as the person's settings say), chat services post it when the
    event is ticked (an undo then a redo posts twice), personal webhooks fire,
    and other tabs refetch. A move with a rank may renumber its column (T14 #9),
    and the lists refetch.

## Safety

9. **Never over a newer change.** Each change keeps the version the server
   returned for it, and the undo sends it as `baseVersion` (the draft rule of
   `useUpdateIssue`): this tab's own later writes are followed, anyone else's
   is a `409 version_conflict`. Then the toast says "This card changed since;
   undo skipped" and the entry is dropped.
10. **Hidden or gone (T20).** `404` is the same answer for a deleted issue and
    one in a team the person can no longer see: the change is skipped and
    dropped; the toast names only the identifier the person already had.
11. **Network, 5xx, 401, 429.** Nothing was written for sure: the change stays
    on the stack and the toast says to press the keys again. In an entry of
    several issues, the ones after the failure stay too. **Offline**
    (`navigator.onLine` false): TanStack would pause the write and the press
    would seem dead, so no write starts; the toast says "You are offline;
    nothing was changed" and the entry stays.
12. **Other refusals** (`400`, `403`, for example an assignee who left): the
    change is dropped and the toast gives the server's text.
13. **One at a time.** A press while an undo or redo runs does nothing, so a
    double press makes one write. A held key (`repeat`) on an empty stack shows
    no toast. A new action made while an undo or redo runs stays on top of the
    undo stack: a change kept after a failed undo goes below it, a redone
    change goes below it, and the redo stack holds nothing from before it (a new
    branch). `placeMany` ends its batch in a `finally`, so a throw cannot leave
    the undo waiting.

## Feedback and accessibility

13a. **Known, not fixed (review, LOW and NIT).** A press while an undo or redo
    runs gives no feedback (the first press's toast follows). In development
    under React StrictMode the effects run twice, which empties the stacks once
    more at mount; there is nothing to undo at that time, so it has no effect
    a person can see, and production does not run effects twice.

14. **Toast.** "Undone: moved ORB-12 to Backlog" with a Redo button ("Redone:
    ..." with an Undo button), 8 seconds. Failures are error toasts. The toasts
    are the existing live region (`aria-live="polite"`, `role="status"` or
    `alert`) in a portal on `<body>`, and the button is a real `<button>`, so a
    keyboard reaches it. "Nothing to undo" / "Nothing to redo" when a stack is
    empty. A toast's button acts on ITS entry only, and only while that entry
    is still the top of its stack (LAUN-237 review): with two toasts on screen
    the older Redo would otherwise redo the newer change. When it is not on
    top, nothing is written and a toast says "That change is no longer the last
    one; nothing was changed. Use the keys to redo." The keys always act on the
    top of the stack. When an undo or redo of several issues is only partly
    written (#11), the kept part goes back with a NEW entry id, and the done
    part keeps the old one: two entries never share an id, so a toast's button
    cannot match the wrong one.
15. **Help.** The keyboard shortcut help lists both keys.

## Code

- `web/src/features/issues/undoStack.ts`: the pure stacks (`record`, `take`,
  `settle`), the change from one write (`changeOf`, `createdChange`) and the
  words. Unit tests: `undoStack.test.ts`.
- `web/src/features/issues/useIssueUndo.tsx`: the keys, the batching, the
  writes and the toasts, for the board and the list (LAUN-237).
  `useIssueActions` takes a `WriteRecorder`; `UiProvider` has `issueCreated` /
  `onIssueCreated` for the New issue dialog. `listDrag.ts` has
  `listDragState()`, as `boardDrag.ts` has `boardDragState()`.
- Web tests: `boardUndo.test.tsx` (move, redo, Ctrl+Y, double press, empty
  stack, 409 after the cache has the foreign version, 404, network, offline,
  text fields, dialog, view switch, team switch, priority, create, a picker
  group change, a real group drag, a partial multi-card undo, rank 0, a
  renamed state, this tab's own later write, a press during the landing and
  during a held drag, a held key, a new action during a failed undo, and a
  throw in `placeMany`, one card moved twice before a refetch lands, also with
  the second drop during the first write, a create redone at its rank, and the
  retry text with a period). Mutants run by hand (each made at least one test
  fail): no `baseVersion`, the rank only when it differs, `end` outside the
  `finally`, no landing wait, states by name, no offline check, `newer`
  ignored.
- List tests (LAUN-237): `listUndo.test.tsx`, one or more per list action:
  status (with the toast Redo), priority (Shift and Ctrl+Y redo), assignee,
  labels, a handle drag to another group and inside a group, Up on the handle,
  a create with `C` (undo cancels, no DELETE; redo at its rank), a picker on a
  selection as one step, a partial failure of that picker (the step holds the
  written row only), a multi-row undo with one stale row, 409, 404, network
  then offline, a text field and a dialog, a press during the landing and
  during a held row, the view switch, the team switch, and the shortcut help.
  The fake server is shared with the board tests (`web/src/test/undoServer.ts`).
  Mutants run by hand (each made at least one list test fail): the list drag
  machine not read (held and landing), `view` out of the reset effect.
  Review follow-ups (each with a test that a mutant made fail): All issues to
  My issues empties the stack, an older toast's Redo writes nothing, a toast's
  Redo after leaving the page writes nothing, and the undo of a create skips
  an issue with a comment or a sub-issue. Second review: the reads before the
  undo of a create (gone: 404 then 400; the 400 alone; network keeps; 403
  refused), a split entry's new id (`undoStack.test.ts`), a handle only
  pressed counts as held, and the list drag machine under StrictMode.

## Real-browser check

The keyboard flow was run in headless Brave (puppeteer-core) on the built app
with a stubbed API (a scratch harness, not in the
repo), once as macOS (Command) and once with the platform set to Linux (Ctrl).
A real mouse drag moved ORB-1 from Backlog to Todo (`PATCH {state: "Todo",
placeAfter: "i2", version: 1}`); the undo key sent `{state, sortOrder: 1024,
version: 2}` and the card was back in Backlog with the toast "Undone: moved
ORB-1 to Backlog"; the toast's Redo button, focused and pressed with Enter,
moved it again; Shift redid; a double press made one write; a stale version
gave "This card changed since; redo skipped"; the key in the command palette's
field wrote nothing. A reviewer also ran it on a real stack (Postgres, the API)
and found the rank-0 case (#5), now fixed. Not checked: a screen reader, a
phone, and whether the browser's own text undo runs in a field (only that the
board does not take the key there).

## The list (LAUN-237)

16. **One hook for both views.** `useIssueUndo.tsx` (it was `useBoardUndo.tsx`)
    serves the board and the list; `IssueBrowser.tsx` mounts it once, with
    `view` (`board` or `list`) and `scope`. Chosen over a hook per view: the
    keys, the guards, the batching, the rank memory (#5a), the 409 / 404 /
    network / offline rules and the toasts are the same code, so a fix in one
    view cannot miss the other. Only the words differ: the list says "issue"
    ("This issue changed since; undo skipped", "Drop the issue first, then
    undo.") and "group" ("moved ORB-3 in its group") where the board says
    "card" and "column".
17. **One stack per page and view.** The stack is still the last 20 actions.
    A change of view (board to list or back) empties it, as a change of team,
    product, cycle, project or path does; leaving the page empties it. So
    every change on the stack was made in the view and place now on screen.
    The issue itself may not be on screen: a row that has left the filter
    since (its new state is filtered out), or an issue created in another
    team from the dialog, is still undone, by its id.
18. **List actions recorded.** Status, priority, assignee and labels (the `S`,
    `P`, `A`, `L` pickers, on the focused row or on a selection), the manual
    order (a handle drag, or Up / Down on the handle, sort Manual only: the
    same `place` write as a board drop, so #5 and #5a hold), and a create from
    the New issue dialog while the list is on screen (`C`, the palette, the
    page button: undo cancels it, #7). The list offers no project change, so
    there is none to undo (the board has none either, #8).
19. **A selection is ONE step.** A picker on a selection writes every row in
    the same task, so the writes join one entry (#4). A row whose write fails
    (the toast of the picker says why) is not in the entry; the entry holds
    the rows that were written, as a board group drop does. An undo of that
    entry where one row changed since skips that row and undoes the others
    (#9, "ORB-2 changed since; undo skipped for it").
20. **Drag guard on the list.** The list's drag machine (`listDrag.ts`,
    `listDragState()`) is read as the board's is (#2): a press while a row is
    pressed or dragged writes nothing and says "Drop the issue first, then
    undo."; a press while the row lands waits for the drop's write and undoes
    it.

**The list (LAUN-237).** The same way (a scratch
harness, not in the repo), on the built app,
as macOS and as Linux, list view, sort Manual: a real mouse drag of ORB-1's
handle into Todo (`{state: "Todo", placeAfter: "i2", version: 1}`), the undo
key (`{state: "s1", sortOrder: 1024, version: 2}`, the row back in Backlog,
"Undone: moved ORB-1 to Backlog"), Redo from the toast with Enter, undo, Shift
redo, a double press making one write; `J` then `P` High and undo
(`{priority: 0}`); `X`, `J`, `X` and `S` Done on two rows, then ONE press
writing both back; `C`, a title, Command/Ctrl+Enter, then undo moving the new
issue to Canceled (no DELETE); the key in the command palette's field writing
nothing; a press while a row is held writing nothing ("Drop the issue first,
then undo."); a press right after a release undoing that drop. The LAUN-236
board harness was run again on the same build: unchanged.

## Open questions for the project owner

- Should an admin's undo of a create delete the issue instead of canceling it?
  (Chosen: cancel for everyone; a delete has no version check and no redo.)
- Should the stack survive a switch between board and list on the same page?
  (Chosen: no, #17. The other view shows the same issues, so it would work,
  but the person could undo a change made in a view they no longer see.)
