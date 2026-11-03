# Decisions T32: delete issues (LAUN-201)

Status: built. The project owner asked "Can't delete tickets". Nothing existed: no REST
route, no MCP tool and no web button. The defaults below were chosen by the
builder (the project owner was not asked) and follow T26 (`decisions-T26-delete.md`).

1. **Route.** `DELETE /orgs/{org}/issues/{issue}` (id or identifier), `204`,
   admin scope. A `write` token (an agent's), a `read` token and a human
   member's web session get `403 insufficient_scope`. No `refuseAgents` (not a
   notification route).
2. **No `Idempotency-Key`**, like T26 and the cycle delete: a repeat is `404
   not_found`. `s.idem` would replay the first `204` for a retry with the same
   key, which hides that the issue is gone.
3. **Cross-org is `404 not_found`.** The issue is resolved by `org_id`.
4. **Hard delete, one transaction** (`database.InTx`). No soft delete or trash:
   there is no restore feature, and a hidden row would keep a number, a title and
   a body the person wanted gone. The dialog says it cannot be undone.
   **What is guaranteed:** the issue row, its own comments, activity, relations,
   attachments and notifications are gone, and its title no longer appears in
   OTHER issues' activity. Those rows hold a snapshot `{id, identifier, title}`
   of it (kind `parent` in `from`/`to`, kind `relationRemoved` in `from.issue`);
   `RedactIssueSnapshots` removes the `title` key in the same transaction and
   keeps `id` and `identifier`, so the history line still reads. **Not
   guaranteed:** the old `issue.updated` events and the `issue.created` event in
   the change feed keep what they held (the identifier and field names, no title
   or body today), and backups made before the delete keep the row.
5. **What goes.** No migration is needed: every foreign key onto `issues`
   already cascades (comments, attachments rows, `issue_relations` on both
   sides, `issue_activity`, `issue_labels`, `issue_state_changes`,
   `issue_subscriptions`, `mr_links`, `notifications`, `notification_emails`)
   except `issues_parent_fk`, which is `SET NULL (parent_id)`: **sub-issues are
   kept** and lose their parent, like a project delete detaches issues. The
   test `TestDeleteIssue_EveryForeignKeyCanGo` fails when a future table
   references `issues` with any other rule. The `import_mappings` row of the
   issue is deleted in the same transaction (no foreign key; a Linear id would
   otherwise resolve to a missing row). **Known gap:** the mappings of its
   comments, attachments and relations stay; a re-run of the importer would
   meet them. Linear is frozen, so this is left as is.
6. **Numbers are not reused.** `issue_counters` only grows. `ORB-7` deleted
   stays unused.
7. **A write that loses to the delete is not a 500.** `database.goneFKs` maps `_issue_fk`,
   `issues_parent_fk` and `issue_relations_related_fk` to `400 validation_failed` ("the issue no
   longer exists"), like T26 #6a. Test: `TestDeleteIssue_LostRaceIsClean`.

7a. **Locks**, in the order `UpdateIssue` uses: the org's parent lock, the
   team's rank lock, then the issue row `FOR UPDATE`. A concurrent re-parent or
   move waits, and a residual deadlock is `503 overloaded` (T26 #6b).
8. **Event.** `issue.deleted` (entity `issue`), `events.Emit` the LAST
   statement of the transaction. The payload holds `id`, `identifier`,
   `teamId`, `parentId` and `detachedChildren` only: no title, no body
   (LAUN-221, decisions-T20 build notes 6a, dropped `parentId` and
   `detachedChildren`: the parent and the children may be in a team a reader
   cannot see). The
   children emit no `issue.updated` each; the web refreshes on `issue.deleted`.
   The comments, relations and attachments emit nothing either.
9. **Attachment objects.** The file objects are deleted AFTER the commit, best
   effort, through the existing store (`s.store.Delete`), like a replaced
   avatar (T24). A failure leaves an orphan object and is logged with the org
   id and a fixed code, never the key. Keys are read inside the transaction, so
   a concurrent attachment create that commits first is still removed (its row
   cascades) and one that loses the race fails on the foreign key.
10. **Web.** A "Delete issue" button in the issue page toolbar, org admins only.
    There is no issue options menu to extend (the toolbar holds Copy link, Copy
    ID and Copy branch name). The dialog names the identifier, says that the
    comments, attachments, relations and activity go, that it cannot be undone
    and that sub-issues stay. Cancel has focus; Escape and Cancel send no
    request; a failure shows the server's reason in an alert and keeps the dialog
    and the page; success goes to the team's list. `issue.deleted` is in
    `EVENT_TYPES` and refreshes the issue lists, the open details, comments,
    subscriptions, cycle progress and project counts. A page open on the issue
    when it is deleted refetches, gets `404`, and shows "This issue was
    deleted" with a link back to the team (not the error loop of failed saves).
    A 404 on a page that never loaded stays "Issue not found". If a team key
    changes while the page is open the old identifier also answers 404, and the
    page then says "deleted"; the person reloads. That is accepted.
11. **MCP.** No `delete_issue` tool, as T26 #9: the agent surface stays
    read/write; deletion is an admin action for a person.
11a. **Honest limits.** The cycle rollover job and a manual cycle write take the
    team cycle lock; `DeleteIssue` takes the rank lock and the issue row. The
    rollover moves issues of a cycle (rows it locks) while a delete holds one of
    them and then waits on a lock the job holds, so a deadlock (40P01) is
    possible in a narrow window. `database.Retryable` turns it into a retry or
    `503 overloaded` with `Retry-After: 1` (T26 #6b); nothing is half-deleted.
    No code handles it beyond that.
12. **Tests.** `delete_issue_integration_test.go` (scope 403 for agent, read
    and write tokens; cascade in every table with an issue column; child kept
    with no parent; object removed; event payload without title or body and
    last in the feed; number not reused; cross-org 404; foreign key guard) and
    `web/src/features/issues/deleteIssue.test.tsx`. Mutants: `write` scope on the
    route, a no-op delete, no `Emit`, no store delete: each fails a test.
    `TestDeleteIssue_RedactsTitleInOtherHistories` (mutant: redaction skipped fails). `TestDeleteIssue_EmitIsTheLastWrite` puts triggers in the test database: one on `events` marks the transaction, the others on `issues`, `issue_activity`, `import_mappings` and `comments` raise after the mark, so a write after `Emit` makes the delete answer 500 (mutant: Emit moved first fails).
