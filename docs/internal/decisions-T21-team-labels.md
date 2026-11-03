> Internal design note from the Orbit deployment; kept for history.

# T21: issue counts and team labels (LAUN-54)

`GET /orgs/{org}/labels` returns `issueCount` per label and takes `?team=`.
A label may belong to a team. Built for the label picker (LAUN-53); the web
switch-over is a separate ticket. Code: `server/db/migrations/0018_team_labels.*.sql`,
`db/queries/labels.sql` (`ListLabelsWithCounts`, `FindLabels`),
`tracker.ListLabels` / `CreateLabel` / `resolveLabels`, `internal/importer`
(`plan.go`, `import.go`). Tests: `internal/api/labels_integration_test.go`,
`internal/importer` (`TestPlan_labelScopes`, `TestRun_givesAnOrgWideLabelItsTeam`),
`internal/mcp/catalog_test.go`.

1. **Shape.** `labels.team_id` is nullable. NULL is an org-wide label (every
   label that exists today). The foreign key is composite
   `(org_id, team_id) -> teams (org_id, id)`; with a NULL `team_id` it is not
   checked (MATCH SIMPLE). `ON DELETE CASCADE`: a team label dies with its
   team (teams are not deleted by the API; this only keeps the schema
   consistent). A label never changes its team after it is made.
2. **A team label belongs only on issues of its team.** Issues cannot change
   team (no `team` in `PATCH /issues`), so this holds for life. It is enforced
   in `resolveLabels` (the service), not by the database: a label id or name
   of another team is `400 validation_failed` ("unknown label"), as a label of
   another org is. The importer writes `issue_labels` directly and relies on
   Linear for this (Linear allows a team label only on its team's issues).
3. **Names.** Unique ignoring case inside one scope: among the org-wide labels
   (partial index `labels_org_name_lower_key WHERE team_id IS NULL`) and among
   one team's labels (`labels_team_name_lower_key (org_id, team_id, lower(name))
   WHERE team_id IS NOT NULL`). **A team label may share a name with an org
   label and with another team's label.** Reason: Linear allows both; refusing
   would need a check across rows that no index can make, and a race. A
   duplicate inside a scope is `409 conflict`.
4. **Resolving a name.** An issue of team T that names a label (`labels`,
   `addLabels`, `removeLabels`, MCP) finds T's label first, else the org label.
   Another team's label is not found. So "bug" on a WEB issue is WEB's "bug" if
   WEB has one. The `?label=` filter on `GET /issues` still matches by name
   only, so it matches every label of that name (org and teams); combine it
   with `team=` to narrow it.
5. **What `issueCount` counts.** Issues that carry the label and are NOT
   archived (`issues.archived_at IS NULL`). **Closed issues (Done, Canceled,
   Duplicate) count.** Reason: the picker uses the count for "Frequently
   used" and a closed issue is still a use; a team that wants open-only can
   have it later as `?issueCount=open`. Without `?team=` the count is over all
   teams; with it, over that team's issues only.
6. **`?team=KEY|id`.** Lists the org-wide labels plus that team's labels,
   with that team's counts. An unknown team (or one of another org) is
   `404 not_found`. A team's labels never include another team's. With no
   `?team=`, every label is listed (org-wide and every team's), each with
   `teamId` / `teamKey` (null for an org-wide one), ordered by name ignoring
   case, org-wide first within a name. The list is not paged (`nextCursor`
   is null), as before; a label count that needs paging is far off.
7. **Create.** `POST /labels` takes `team` (key or id). Unknown team: `400`.
   Same scope and rights as before (write scope). `label.created` carries
   `teamId` when set. The event stays org-wide (decisions-T20 #55 lists
   `label.created` as org).
8. **Team access (T20, LAUN-91; not built).** Today every member sees every
   team. The count query is written for the later rule: `ListLabelsWithCounts`
   takes `visible_team_ids` (NULL = all teams). A set removes the team labels
   of hidden teams from the list and the issues of hidden teams from every
   count, so an org-wide label's count never includes a hidden issue
   (decisions-T20 #35, Test B). `Service.listLabels` has the parameter; the
   public `ListLabels` passes nil. When T20 lands, pass the caller's set there
   and tag the query in the inventory (it is `team`: decisions-T20 #8). A team
   label follows the team rule: it is hidden with its team. Nothing else in
   this ticket needs to change.
9. **Importer.** A Linear label with a team keeps it (`labels.team_id` is the
   mapped team). Merging, in this order: org-wide labels of the same name
   merge into the oldest; a team label named like an org-wide label merges
   into it (as before, decisions-T4 #19: this keeps what an earlier import
   already did, and an issue that carries both gets one); team labels of the
   same name on the SAME team merge; the same name on two teams stays two
   labels (before: merged into one). A label whose team is not in the export is
   org-wide. **An install that was imported before this change** has one
   org-wide row for labels the old plan merged (two teams' "bug" were both
   mapped to one row). The new plan sees two labels, but the importer never
   moves data on an installed row: a row that more than one source label is
   mapped to is not written at all (no team, no rename, no recolour; the
   mappings stay). Any other existing row takes a team only when
   (a) no other canonical source label is mapped to it (a label merged into it by name does not count), (b) its `team_id` is NULL
   (`ImportUpsertLabel` sets `team_id` only while it is NULL and never changes
   or removes one) and (c) no issue of another team carries it
   (`ImportLabelUsedOutsideTeam`). A row that fails any of these stays
   org-wide, and issues keep pointing at it. So a re-run after the upgrade
   creates no label and moves no issue for already-merged labels; only a
   label that was never merged gets its team. New labels (not yet mapped) are
   written with their team. Test: `TestRun_preMergedTeamLabelsStayOrgWide`.
10. **MCP.** `list_issue_labels` passes `team` (name, key or id, resolved
    through `GET /teams`) as `?team=KEY`; before, it was ignored. The rows carry
    `teamId`, `teamKey` and `issueCount`. `save_issue` needs no change (rule 4).
    The tool description and the golden file changed on purpose.
11. **Query plan.** One grouped aggregate over `issue_labels` joined to
    `issues`, then a hash join to `labels`. Measurement: see the note at the
    end of this file.
13. **Removing by name.** `removeLabels` with a name first matches the labels
    the issue already carries (either scope), so "bug" removes the org "bug" the
    issue has even when its team has its own "bug"; a name it does not carry
    resolves as for an add (rule 4).
12. **What the web must change (LAUN-53 follow-up).** Drop the 250-issue sample;
    call `GET /labels?team=KEY` for the issue's team; use `issueCount` for
    "Frequently used" / "Suggestions"; build a "Team labels" group from the
    rows with `teamId != null`; offer "create label" with `team` set when made
    from the team's picker. `schema.d.ts` is regenerated in this change. The picker switch-over (AC "the
    picker uses the counts; the old sample request is gone") is NOT in this
    server change; it is deferred to a web follow-up ticket (done in LAUN-101; see
    `web-decisions.md`).

## Query plan note (measured)

One org, 2 teams, 10,000 issues, 30,000 `issue_labels` rows, 40 labels, local
Postgres 16 (`EXPLAIN ANALYZE` of the count subquery, no `?team=`): a hash join
of `issue_labels` and `issues`, a HashAggregate by label, a hash join to
`labels`. Execution 7 ms. The planner used sequential scans because the one org
holds every row; with many orgs the `org_id` predicates would use
`issue_labels_label_idx` / `issue_labels_org_issue_idx`. NOT measured: a table
of millions of rows, several orgs, or concurrent load. The cost is linear in
the org's labelled issues; if a large org makes it slow, cache the counts or
keep a counter column then.
