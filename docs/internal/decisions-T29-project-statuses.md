# Decisions T29: custom project statuses (LAUN-195)

Status: built. The project owner was not asked; the orchestrator chose the v1 below (Linear: Settings > Projects > Project
statuses). Before this change a project's `state` was a fixed enum of six values.

1. **Six fixed KINDS, many statuses.** The kinds are the old enum: `backlog`, `planned`, `started`, `paused`,
   `completed`, `canceled`. A status is a row of `project_statuses` (`org_id`, `kind`, `name`, `color`, `position`).
   Admins rename, recolour, add (to any kind), reorder and delete statuses. They cannot change a status's kind or
   add a kind. The kind drives behaviour: the timestamps (`startedAt`, `completedAt`, `canceledAt`), "open"
   (not completed or canceled), overdue and health. Names are unique per org in any case, 1 to 50 characters, and
   never the word of ANOTHER kind, and never contains a comma (the `state=` filter splits on commas) (so a `state` of `started` can never mean a status of a different kind).
   At most 60 statuses per org.
2. **`projects.state` stays as the kind mirror; `projects.status_id` is new.** The composite foreign key
   `(org_id, status_id, state)` to `project_statuses (org_id, id, kind)` keeps the two equal in the database, and
   keeps every status in the project's own org. The old column keeps old clients, the MCP and the importer working
   and needs no rewrite of the timestamp code. Least risky: nothing is dropped, the down migration loses only
   names, colours and order.
3. **Seed.** Migration 0027 gives every org six defaults named `Backlog`, `Planned`, `In progress`, `Paused`,
   `Completed`, `Canceled` (the labels the web showed for the old values, so nothing changes visibly) and sets
   every project's `status_id` to its org's default of the project's kind. A trigger on `orgs` seeds each new org
   (setup, sign-up, the importer and tests all insert into `orgs`, so no code path can forget it).
4. **API.** `Project` keeps `state` (the KIND) and gains `status` `{id, kind, name, color}`. Create and PATCH take
   `statusId` (wins) or `state`; `state` is a status NAME (any case) first, then a KIND (the first status of that
   kind in the org's order). On a PATCH, a bare kind word that equals the project's CURRENT kind is a no-op: the
   project keeps its status (custom or not), nothing is written and no event is emitted (the MCP and old clients
   send the kind back); a kind word of another kind moves to the first status of that kind. A `state` that disagrees with `statusId` is 400. A status of another org is 400
   (body reference). Moving to another status of the SAME kind changes no timestamp; a change of kind recomputes
   them as before. The `project.updated` payload lists `status` (was `state`) in `changes`.
   `GET /projects?state=` takes comma-separated kinds (every status of the kind), names or ids; an unknown item
   is 400.
5. **Routes** (`/api/v1/orgs/{org}/project-statuses`): `GET` (read), `POST` (admin, `Idempotency-Key`),
   `PATCH /{status}` (admin), `DELETE /{status}` (admin), `PUT /order` (admin; body `{ids}` is EVERY status of the
   org once, else 400; idempotent by shape). Cross-org is 404 by path. Every write locks the org's status rows
   (`FOR UPDATE`) so the kind minimum, the in-use count and the order see a stable
   set; a project write that names a status holds `FOR KEY SHARE` on it, so a delete cannot slip between.
6. **Delete rules.** `409 status_in_use` while any project (archived too) has the status; `409
   status_last_of_kind` when it is the only one of its kind (checked first). Move the projects first. Codes are in
   `httpx.Codes` and the OpenAPI `ErrorCode`.
7. **Events,** last in the transaction: `project_status.created`, `.updated` (changes `name`, `color`; a no-op
   writes nothing), `.deleted`, `.reordered` (payload: the full id list; none when the order did not change).
   Entity type `project_status`. Also in `web/src/api/events.ts`.
8. **Linear importer.** The Linear project status type maps to the kind as before; `ImportUpsertProject` sets
   `status_id` to the org's FIRST status of that kind. A re-import keeps the project's status when the kind did not
   change, so it never undoes an admin's choice.
9. **MCP.** `save_project.state` is no longer an enum: a status name or a kind. `list_projects.state` takes kinds
   or names. Output is the REST project, so `status.name` is there. Goldens updated (descriptions and the
   removed enum only).
10. **Web.** `Settings > Project statuses` is an admin page (add, rename, recolour, reorder with move buttons,
    delete); a member gets the admin notice there and reads the statuses through the pickers and the filter.
    The list API and every picker order statuses by kind (backlog, planned, started, paused, completed,
    canceled), then by the org's order inside the kind. The New/Edit project dialog, the project page and the list show `status.name` with the kind's
    icon; the filter lists statuses and filters by status id. Sorting by status uses the kind order, then the name.
