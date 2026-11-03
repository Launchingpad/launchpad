# Decisions T38: Every chat service belongs to a team (LAUN-209)

The project owner asked for "a webhook per team setting" on 2026-11-01: LAUN issues post to
one channel and ORB issues to another. This extends
`decisions-T36-chat-services.md` and changes the org-wide model of its #2. The
decisions below are the project owner's, from the ticket; the answers marked "proposed" are
the builder's defaults, to be changed with a new record. The T37 rules (post
every ticked kind; the mute rule) are unchanged.

## What changes in T36

1. **T36 #2 (several services per ORG) becomes: several services per org, each
   in ONE team.** `team_id` is required. There is no org-wide service. The cap of
   10 stays per org (not per team), checked under the same org-row lock.
2. **T36 #8 (the table):** `chat_services.team_id uuid NOT NULL` with a composite
   FK `(org_id, team_id)` to `teams (org_id, id)` (no ON DELETE action) and an
   index on `(org_id, team_id)`. Migration 0036.
3. **T36 #12 (the routes):** no new route and no new path. `POST` takes
   `teamId` (required); `PATCH` takes `teamId` (optional); every service in a
   response (`GET` list, `GET`, `POST`, `PATCH`) has `teamId`. Admin scope,
   `refuseAgents` and `idempotency.Reject` are unchanged.
4. **T36 #16 (the page):** the Add and Edit dialogs have a required team picker;
   each card shows the team name; the page copy states the rule below.
5. **T36 honest limit "No per-team or per-event routing"** is superseded for
   teams (not for per-event routing).

## The rules

6. **An event posts only to the enabled services of the team the issue is in.**
   For an issue or comment event the team is the one the consumer already uses
   for notifications (the team in the event payload, else the issue's team). A
   team with no service gets no post. The cap, the fair per-pass rule, dedupe,
   the mute rule (T37 #4), the sender and the rate cap are unchanged.
7. **An event with no team posts nowhere.** The page says so.
8. **`project.update_due` (proposed).** Found in the schema: a project has no
   lead team and no single team, but `project_teams` links a project to any
   number of teams. A reminder posts to the services of EVERY team linked to the
   project. A project with no linked team (an org-level project) posts nowhere.
   Dedupe is per service, so a service never gets one reminder twice. The lead
   must still be an eligible person (T37 #3).
9. **No org-wide service (the project owner).** There is no "all teams" value. To post a
   team's events to several channels, add several services in that team.
10. **Team delete (the project owner, extends T26).** A team that has chat services is
    `409 team_in_use` ("team X still has N chat service(s); move or remove
    them"), like a team with issues. The check is inside the delete transaction
    under the team row lock `FOR UPDATE`, which conflicts with the KEY SHARE the
    service insert takes through the FK, so a service cannot arrive between the
    count and the delete. The FK has no cascade: a missed case is a failed
    delete, never a lost service.
11. **The team can change after creation; the kind cannot.** A PATCH with
    `teamId` moves the service. Only posts for events consumed after the change
    use the new team; rows already in the outbox are sent as they are.
    **Superseded by LAUN-231** (decisions-T20, team access): a change of
    `teamId` or a new `webhookUrl` deletes the service's waiting rows in the
    same transaction, and the sender skips (`not_selected`, no retry) a row
    whose issue or project is not of the service's current team.
12. **Who manages (the project owner):** an org admin only, as today.
13. **Cross-org (AGENTS.md).** The routes have no team in the path. A service
    of another org in the path is `404 not_found` on every verb (delete: `204`,
    removes nothing, as T36 #12). A `teamId` in a body that is not a team of
    this org (another org's, or unknown) is `400 validation_failed`, on POST and
    PATCH, and nothing is written.
14. **The event.** `chat_service.updated` carries `teamId` as well (ids only: the
    action, service id, kind, team id, enabled, event kinds). No name, host or
    URL.
15. **No write path imports `internal/notify`; no secret changes.** The webhook
    stays write-only, sealed, never returned, logged or emitted. Moving a
    service changes no seal scope.
16. **The hourly cap and its page** are LAUN-210, not this record.

## The migration (0036)

17. Add the column nullable; set each existing service to its org's default
    team, else the org's oldest team (`created_at`, `id`); delete a service whose
    org has no team at all (it could not post); set NOT NULL; add the FK and
    index. **Down:** drops the index, the FK and the column; nothing else is
    lost (a service deleted by the up is not restored). The project owner deletes their one
    service before deploy, so the backfill matters only to other installs.
    `TestMigration0036_ChatServicesTeam` and `TestMigrateUpDownUp` cover it.

## Archived teams and races (review follow-up)

- A team that is archived is refused as a service's team: `400
  validation_failed` on create, and on a patch that changes the team. A service
  that already sits in a team keeps it when the team is later archived (the
  consumer posts nothing for an archived team anyway, as for notifications).
- The team check reads the team row `FOR KEY SHARE` inside the write
  transaction, so a team delete (which locks it `FOR UPDATE`) cannot run
  between the check and the write. A foreign-key error (23503) is also mapped
  to `400 validation_failed`, never a 500.

## Honest limits

- After the deploy every existing service posts only its backfilled team's
  events. Events of other teams that posted before no longer do. That is the
  point of the change, and the page says so: after the upgrade an existing
  service posts only the events of the team it was moved to. To get the events
  of the other teams, an admin adds a service per team.
- A team with no service, and a project with no team, post nothing. Nothing
  warns an admin when events are dropped for lack of a service.
