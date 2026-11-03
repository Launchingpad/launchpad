> Internal design note from the Orbit deployment; kept for history.

# Decisions T15: per-issue activity (LAUN-49)

The web Activity timeline (LAUN-48) shows what the server returns. This
tranche records and exposes more. Migration `0009_issue_activity`.

1. **Relation added is exposed, not recorded again.** `issue_relations` has
   `created_by` and `created_at`. `Relation` (REST) now has `createdAt` and
   `createdBy` (`UserRef` or null: an imported relation or a removed user).
2. **One table, `issue_activity`.** Columns: id (uuidv7), org_id, issue_id,
   actor_id (null: the cycle job, or a removed user), source, kind, `from`,
   `to` (jsonb, nullable), created_at. Composite `(org_id, issue_id)` FK with
   ON DELETE CASCADE, index `(org_id, issue_id, created_at, id)`. No RLS:
   every query filters by `org_id`.
3. **Kinds recorded** (a CHECK lists them): `title`, `description`,
   `assignee`, `priority`, `estimate`, `dueDate`, `parent`, `project`,
   `milestone`, `cycle`, `labelAdded`, `labelRemoved`, `relationRemoved`.
   One row per changed field; one row per label added or removed. A PATCH that
   changes nothing writes nothing.
4. **Snapshots, not live joins.** `from` / `to` hold the value or an object
   made when the change was written, so a later rename does not rewrite
   history: title and priority and estimate are plain values, `dueDate` is
   `YYYY-MM-DD`; assignee `{id, username, name}`; project, milestone, label
   `{id, name}`; cycle `{id, number, name}`; parent `{id, identifier,
   title}`. `labelAdded` has only `to`, `labelRemoved` only `from`.
   `relationRemoved` has `from = {type, issue}` where `type` is the relation as
   that issue saw it (blocks, blockedBy, related, duplicateOf, duplicates) and
   `issue` is the other issue.
5. **A description edit stores no text** (it can be 200k characters). The row
   says only that it changed, when and by whom. Old text is not kept.
6. **A relation removal writes a row on BOTH issues** (the one in the path
   and the other end), in the delete's transaction.
7. **Sources.** The API writes `api`. The cycle job's rollover writes
   `system` with no actor, one `cycle` row per moved issue. The `gitlab` and
   `import` values are allowed by the CHECK (the same set as
   `issue_state_changes`) but nothing writes them: GitLab moves state only
   (already in `issue_state_changes`), and the importer writes issues
   directly and has no history to replay.
8. **Not done, on purpose:**
   - `mcp` as a source. The MCP server calls the same REST API with a bearer
     token, so the server cannot tell it apart from any other client. The web
     keeps saying "via agent" from `isAgent`. A real value needs the MCP
     binary to send a marker header and a trust rule for it: a separate ticket.
   - A source on comments and attachments. Same reason for comments. For
     attachments, the existing `creator` null already means a hook or an
     import. Adding a column to both tables for one label was not worth it.
   - Attachment removed. There is no route that deletes an attachment, so
     there is nothing to record. Add the row when the route exists.
   - Folding `issue_state_changes` into `issue_activity`. It stays as is; the
     web folds both.
   - Backfill. Past edits are not known and are not invented.
   - Archive / unarchive, issue ordering (`sortOrder`) and state changes are
     not in `issue_activity`. State has its own table; rank is board order.
   - Events. `issue.updated` and `relation.deleted` are unchanged. The
     activity rows hold no secret: only ids, names and field values.
9. **Read.** `GET /orgs/{org}/issues/{issue}/activity?limit=&cursor=`
   (`read` scope), oldest first, keyset on `(created_at, id)`, limit 1-250
   (default 50). Cross-org is `404 not_found`. Rows of one transaction share
   `created_at` (`now()`), so their order inside one PATCH is by `id`, which
   is not insertion order within a millisecond: treat them as one edit.
10. **Web.** `web/src/api/schema.d.ts` is regenerated. Rendering the new
    kinds in `activityModel.ts` is a web follow-up, not part of this change.
