# Decisions T30: project updates, schedule and reminders (LAUN-196)

Status: built. Part A: project updates (migration 0028). Part B: schedule and
reminder (migration 0029).

The orchestrator decided (the project owner was not asked) that a document is not a good
enough stand-in for an update, so the ticket has two parts, in this order.

## Part A: the project update object

Before this ticket Launchpad had none (grep of server, web, migrations and
OpenAPI found no table, no health value, no route). Only the Linear importer
knew the word (`ProjectUpdateID` on a comment).

1. **Table `project_updates`** (0028): `org_id`, `project_id` with a composite
   FK `(org_id, project_id)` and `ON DELETE CASCADE`, `author_id` (`ON DELETE
   SET NULL`), `health` (`on_track|at_risk|off_track`, a CHECK), `body`
   (a text body, shown as plain text; agents may write markdown; at most 50 000 characters, a CHECK and an API check), `created_at`.
   No `updated_at`: v1 is create, list and read only. No edit, no delete (the
   task allowed this when edit/delete is not simple; a post is a record).
2. **Routes**: `GET|POST /projects/{project}/updates`,
   `GET /projects/{project}/updates/{update}`. Read scope for GET, write scope
   for POST, `Idempotency-Key` on POST after the scope check. Cross-org is 404
   (the project is looked up with `org_id` first). A bad `health` or too long a
   body is 400. Keyset pages, newest first (`created_at, id`). An agent may
   post (an agent can lead work); the notification rules are Part B.
3. **Event** `project_update.created`, entity type `project_update`, emitted
   last in the transaction. The payload holds ids and the health word, never
   the body (a test checks it). The type is in `web/src/api/events.ts`
   `EVENT_TYPES`; the page test regex for the server list now allows `_` in the
   first part of a name.
4. **Web**: an Updates section on the project page (health picker, body box,
   list, newest first; the health is a word as well as a colour; no composer on
   an archived project). The body shows as plain text with line breaks (the app
   has no markdown renderer yet). A `project_update.created` event refreshes
   the list.
5. **MCP**: projects are exposed, so `list_project_updates` (read-only) and
   `save_project_update` (write) were added; the reviewer profile does not get
   them. Goldens updated.
6. **Importer: not done.** The Linear export fetches comments with a
   `projectUpdateId` but never exports project updates themselves (no query, no
   file in the export format). Importing them needs a new export file, a model
   and a verify count: its own ticket.

## Part B: schedule and reminder (migration 0029)

1. **Schedule** on the project: `update_frequency` (`none|weekly|biweekly|monthly`,
   default `none`), `update_weekday` (0 Sunday to 6, default 5), `update_hour`
   (0-23, default 9), `update_last_due_at`. The time zone is the org's
   `time_zone` (no new setting). Weekly: every `weekday`. Biweekly: a Monday-start week has
   index floor((days since 1970-01-01 + 3) / 7); the due weeks are the EVEN
   indexes (the same weeks for every project, no anchor column). The week that
   starts Monday 1970-01-05 has index 1, so it is not due; Fri 2026-10-23 and
   2026-11-06 are due, Fri 2026-10-30 is not (pinned by a test). Monthly: the first `weekday` of the month. The hour is
   wall-clock time in the org zone, so DST moves the UTC instant and never the
   local hour. A missing local hour (spring forward) or a repeated one (fall
   back) gets the one instant `time.Date` picks; the stored `update_last_due_at`
   makes it fire once either way.
2. **API**: `Project.updateSchedule` `{frequency, weekday, hour}` (always present),
   set by `PATCH /projects/{project}` `updateSchedule` (write scope; absent
   fields keep their value; a PATCH that changes nothing writes nothing; the
   schedule and other fields together are one version bump). Setting a
   schedule stores the latest PAST due time as `update_last_due_at`, so the first
   reminder is the NEXT due time and a new schedule never reminds for the past.
   Turning it to `none` clears it. The New project dialog is not changed (the
   project page has the form; a new project can be scheduled right after).
3. **Job** `Service.RemindProjectUpdates(ctx, now)`: boot run, then hourly
   (`RunUpdateReminderJob`, like the cycle job). One transaction per project
   under `FOR UPDATE` on the project row. If the latest due time `D <= now` is
   after `update_last_due_at`, the job stores `D` and, when the project has a
   human lead (active, not an agent) and no `project_updates` row was created at
   or after the PREVIOUS due time, emits `project.update_due` (entity project, no
   actor, payload `projectId`, `leadId`, `dueAt`; `events.Emit` last). The stored
   `D` and the event are one transaction: a restart, a second instance or a
   repeated run makes nothing. Only the latest due time is handled after a long
   stop. Archived, completed or canceled projects, no lead, an agent lead and a
   deleted project are skipped. A lead change counts at the next due time (the
   lead is read at the due time).
4. **Delivery** (the notify consumer reads the feed; no write path imports
   `internal/notify`): kind `project_update_due`, a normal event in
   `notification_settings` (default email on, inbox on; the person's global row
   and master email switch apply). It has no team level: the settings view lists
   no teams for it and a team PUT naming it is `400`. `notifications.issue_id`
   and `notification_emails.issue_id` are now nullable, with `project_id` and a
   CHECK that exactly one is set (and kind `project_update_due` iff `project_id`).
   Dedupe key `project-due:<project>:<unix due time>`. The row holds ids and the
   due time only; an agent or an inactive user gets nothing (ListNotifyEligibleUsers).
5. **Inbox**: `issue` is null and `project {id, name}` is set for this kind. The
   web shows "Project / <name> / A project update is due" and links to the project.
6. **Mail**: `mail.BuildProjectDue`, plain text and HTML (the name is escaped), one
   mail per project, with the same unsubscribe token and `List-Unsubscribe`
   headers; the settings are read again at send time. The mail holds the project
   name and links, never an address or update text.
7. **Honest limits**:
   - The app has no markdown renderer, so an update body shows as plain text with
     line breaks, and a web address in it is not a link (on purpose: safe). The
     composer says "Plain text for now". Do not claim markdown rendering.
   - A later change of the org time zone can cause one extra reminder for a week
     that was already reminded (the stored due time is an instant; the new zone
     may put the same week's due time later).
   - A project reopened from completed or canceled can get one late reminder,
     for a due time up to one period old (the job handles only the latest due
     time, and it skips closed projects without storing it).
   - The mail rate cap (RateCap per hour) applies to project mails like the
     others: a mail over the cap is skipped, the inbox row stays, and the next
     mail says how many updates were not emailed (CountUnreportedRateCapped; a
     count only, no address).
8. **Not done**: the New project dialog has no schedule field; `save_project` in
   the MCP server does not take `updateSchedule` (the REST PATCH does).
