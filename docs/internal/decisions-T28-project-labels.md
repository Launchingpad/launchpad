# Decisions T28: project labels (LAUN-194)

Status: built. The project owner did not choose; the orchestrator took the v1 below as
the default. Linear has Settings > Projects > Labels: org-wide labels with a
colour and groups, put on projects. This file copies that, small.

1. **Org-wide, not per team.** A project can belong to many teams, so a team
   label has no meaning. `project_labels` has `org_id`, `name`, `color`,
   `parent_id`. Names are unique ignoring case per org (`409 conflict`).
   These are NOT the issue labels (`labels`): different table, different routes.
2. **Groups are a parent label, one level.** The same shape as issue labels
   (`parent_id`, composite FK, `ON DELETE SET NULL (parent_id)`). The service
   keeps it to one level: a group cannot be in a group, and a label with
   children cannot be put in a group (`400 validation_failed`). Deleting a
   group keeps its labels, with no group. A group is a normal label: it can be
   put on a project too.
3. **Assignments.** `project_label_assignments (org_id, project_id, label_id)`
   with composite FKs and `ON DELETE CASCADE` on both sides. At most 50 labels
   on one project.
4. **Routes.** `GET /project-labels` (read, with `projectCount`),
   `POST /project-labels` (admin, `Idempotency-Key` after the scope check),
   `PATCH` and `DELETE /project-labels/{label}` (admin). The catalogue is admin
   scope like teams and products; an agent with `write` can read it and put
   labels on projects. A project's labels are set by `labelIds` on
   `POST /projects` and `PATCH /projects/{id}` (replaces the set; `null` or
   `[]` clears). A project answer has `labels` (id, name, colour, parentId),
   by name. Another org's label: `404` in a path, `400 validation_failed` in a
   body. The label rows are share-locked in the write transaction so a
   concurrent label delete waits (no 500 from the foreign key).
5. **Filter.** `GET /projects?label=` takes a label id or a name (ignoring
   case). A group matches the projects that carry any label in it. An unknown
   label matches nothing (not an error). The web Projects page keeps it in the
   URL as `?label=<id>`.
6. **Events.** `project_label.created`, `.updated`, `.deleted` (entity type
   `project_label`; payload: created and deleted have `id` and `name`, updated has
   `id` and `changes`, the changed field names). A change of the
   labels ON a project is `project.updated` with `labels` in `changes` (it
   bumps `version`). A label delete emits only `project_label.deleted`: the
   projects are not touched, so the web refreshes the project queries on that
   event. `EVENT_TYPES` in `web/src/api/events.ts` has the three.
7. **MCP.** Project output is passed through, so it has `labels`.
   `save_project` takes `labels` (names or ids, replaces the set) and sends
   `labelIds`; `list_projects` takes `label`. Golden file updated.
8. **Linear importer: not done.** The importer's project query does not select
   project labels, so the export has none. Adding them is a bigger change
   (query, files, plan, mapping); left out on purpose.
9. **Web.** Settings > Administration > Project labels (create, rename,
   recolour, group, delete); a Labels picker on the project form (the New
   project dialog and the edit dialog); chips on the Projects list and the
   project page; a label filter on the Projects page.
10. **Lock order.** Project labels are locked in id order, always: the share
    lock of a project write (`LockProjectLabelsByID`, `ORDER BY id`) and the
    row lock of a label edit with its new group (`LockProjectLabelsForUpdate`,
    `ORDER BY id`, taken before the single-row lock). So two writes on the same
    labels cannot deadlock. If one still does, `database.InTx` maps 40P01 and
    40001 to `503 overloaded` through `Retryable` (decisions-T26); a 5xx is
    never stored under an `Idempotency-Key`.
