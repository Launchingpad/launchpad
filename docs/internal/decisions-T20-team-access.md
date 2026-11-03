# Decisions T20: team-scoped access for members (LAUN-91)

Design only. No code is in this change. The request (2026-10-26) was that invited members can be given access to
specific teams. Today a member sees every team. This note says how access to a team is
stored, how it is enforced on every read and write, what happens at the edges
(projects that span teams, relations, notifications, the change feed), how it
rolls out, and which build tickets make it, in an order that never leaves a
route open. Migration numbers: taken at merge (#34).

This is an access boundary change. A missed query is a data leak, not a bug in
a feature. So the note spends most of its length on one thing: a design in
which a missed query is hard to write and easy to catch (#10 to #20).

## Summary

Each member gets one of two settings. "All teams" is what everyone has today.
"Selected teams" lists the teams the member may see. Org admins always see
all teams. Nothing changes for a member until an admin narrows them.

A team the member cannot see does not exist for them. The server answers "not
found" for its issues, comments, cycles, projects and files. The sidebar, the
event feed, the inbox and the emails leave it out. Agents and tokens follow the
person or agent they belong to.

The build has eleven numbered tickets and fourteen merge requests. Tickets 2, 6
and 11 are split. First the server learns to filter every route and feed. Last,
admins get the API and the screens to restrict people. No member can be restricted
before every route is safe. Rollback has four steps. Stop the app. Revoke the
open invites that name teams. Set every member back to all teams. Then deploy the
older image. The migration that drops the invite tables refuses to run until you
have revoked those invites.

While the old events are sorted into teams, a restricted member sees new events and
not old ones. The sorting is a job that runs at every boot until it is done.

Questions for you, each with the default I recommend:

- A. What does a new invite give? All teams, and the dialog lets the admin narrow it.
- E. What happens to open issues of a person who loses a team? The admin must choose, and the screen suggests unassign.
- F. Who sees a project that spans teams? Anyone who sees one of its teams.
- L. May a person assign an issue to someone who cannot see the team? Yes. That person gets no mail and no inbox row.
- M. May a webhook touch an issue of any team? No. Each repo must name one team before anyone is restricted. This means one team per repo. A repo shared by two teams needs a second integration, or has no map row and links nothing. Until every enabled repo has a team, the server refuses to restrict anyone. The other choice (a) lets any pusher link and move issues of a hidden team.

One risk stays under every choice. A person who can push to a repo mapped to team T
can link and move issues of T, even if they cannot see T. Webhook senders are not
Launchpad users, so the server cannot tell who pushed. Only a future per-user
mapping could close it.

The other points B to D and G to K are listed at the end with their defaults.

Where the ten questions of the ticket are answered:

| Question | Decisions |
|---|---|
| 1. Model, defaults | #1 to #6 |
| 2. Invites | #26 to #30 |
| 3. Enforcement, one enforcement point | #10 to #25, #61 to #68, the inventory, the build tickets |
| 4. Cross-team objects | #36 to #47, #69 |
| 5. Org-level things | #7 to #9 |
| 6. Agents and tokens | #31 to #33 |
| 7. Removing access | #48 to #52 |
| 8. Events feed and SSE | #53 to #60 |
| 9. Migration, tests, performance, web | #34 and #35, then the Tests, Performance and Web sections (#71 to #78) |
| 10. Products, the Linear import | #67, #69, #70 |

## What the code does today (checked in the code)

- `org_members (org_id, user_id, role, created_at)` holds a role (`admin` or
  `member`) and nothing else (`db/schema.sql`). There is no team membership
  table. `team_follows (org_id, user_id, team_id)` is a notification choice, not
  access; it has a composite FK to `org_members` (`team_follows_member_fk`), the
  shape this note reuses.
- `auth.RequireOrgMember` (`internal/auth/middleware.go`) resolves `{org}`,
  checks the token pin, calls `GetMembership` (role only) and puts an `*auth.Org`
  (`ID`, `Slug`, `Name`, `Role`) in the context. Every `tracker.Service`
  method already takes that `*auth.Org`. No code reads a team list from it,
  because none exists.
- `auth.RequireScope(admin)` also demands the org `admin` role. A platform
  admin who is only a member passes the platform routes but not the org admin
  routes.
- `internal/api/router.go` is the route authority: 94 route registrations (counted over the `/orgs/{org}` block of `router.go`; the file has 116)
  under `/orgs/{org}`, each wrapped by `read`, `write` or `admin`. Nothing in it
  names a team rule.
- Reads that take a reference go through four resolvers in
  `internal/tracker/service.go`: `resolveIssueID`, `resolveTeam`,
  `resolveState`, and `checkPlanningRefs` (project, milestone, cycle). Each
  takes a bare `orgID`. Cycles have `resolveCycleID` / `cyclePathID`
  (`imported.go`). Documents have `resolveDocumentID`.
- The four big reads are hand-written SQL with a `where` slice:
  `ListIssues` (`issue_read.go`), `ListProjects` (`projects.go`), `ListDocuments`
  (`documents.go`), `ListCycles` (`cycles.go`). The rest are sqlc queries in
  `db/queries/*.sql`.
- `issueSelectTmpl` joins the parent issue and its team, so an issue row carries
  the parent's identifier and title (`ParentTitle`). Issues can have a parent in
  any team of the org: `CreateIssue` and `UpdateIssue` only call `resolveIssueID`
  for the parent. Relations also cross teams (`CreateRelation`).
  `relationsFor` returns the other issue's title and state name
  (`issueRefs`, `ListIssueRefs`).
- `checkPlanningRefs` accepts any project of the org for any issue (it calls
  `ProjectExists` only). So an issue in team A can sit in a project that lists
  only team B. It refuses a cycle of another team with a message that names the
  fact ("cycleId is a cycle of another team").
- `projects` have `project_teams (org_id, project_id, team_id)`, zero or more
  rows. `attachProjectTeams` returns every team key of a project. `milestones`
  belong to one project. `documents.project_id` may be null (an org-level
  document). Cycles, workflow states and issues have a `team_id` with a
  composite FK to `teams (org_id, id)`. Labels have no team (`labels` has
  `org_id` only; team labels are LAUN-54).
- Issue activity stores small JSON snapshots made at write time, including the
  other issue's identifier and title for `parent` and `relationRemoved`
  (`snapIssue` in `activity.go`) and names for project, milestone and cycle.
- `events` has `(org_id, seq, type, entity_type, entity_id, actor_id, payload)`.
  It has no team column. `ListEventsAfter` and the SSE handler `streamEvents`
  (`internal/api/events.go`) read the whole org feed for any `read` token. The
  SSE loop re-checks token, user and membership on every poll tick
  (`Service.StillAuthorized`). `seq` is dense per org (`org_event_counters`).
  Issue payloads carry the identifier, not the title; many carry `teamId`,
  comment and attachment payloads carry only `issueId`, project, milestone and
  document payloads carry no team.
- `events.Emit(ctx, q, orgID, Event)` has 21 call sites outside tests
  (`grep events.Emit`). None passes a scope.
- The notification consumer (`internal/notify/consumer.go`) reads `events`,
  looks up the issue, and picks recipients: creator, assignee, commenters,
  subscribers, team followers, mentioned users. Its only recipient filter is
  `ListNotifyEligibleUsers`: an active, non-agent member of the org. The
  inbox queries (`ListInbox`, `GetInboxItem`, `CountUnreadInbox`,
  `ListInboxStreamRows`) join `issues` and return the issue title. The mail
  sender reads the issue through `GetMailIssue`. Agents get no notifications
  (`refuseAgents`).
- The MCP server (`internal/mcp`) has no database access. It calls the REST API
  with the caller's token and builds its team list from `GET /teams`.
- Invites: `InviteInput` is `{email, role}`. `CreateInvite` refuses an email
  that is a member already (409). `AcceptInvite` makes the user and the
  membership (`InsertOrgMemberRole`, `ON CONFLICT DO NOTHING`) in one
  transaction. `Approve` of a sign-up request has no body and inserts a
  `member` (`InsertOrgMember`). `CreateAgent` inserts the agent as `member`.
  `internal/setup` makes the first admin. Six code paths insert into
  `org_members`: setup, two in invites, sign-up approval, agents and `testdb`.
  No route changes a role or removes a member.
- The web gets every team list from one query, `useTeams()` (`GET /teams`): the
  sidebar (`Shell.tsx`, `TeamNavItem`), the create dialog, the command palette,
  the cycle jump, the notification settings page, and the team page's own "not
  found" test (`IssuesPage.tsx`). `MembersPage.tsx` is a read-only table plus
  `InvitesSection`. `IssuesPage` already has a "no team" empty state.
- T18 (products) is design only. Its #26 says a product is not an access
  boundary and that any boundary "comes with a team-level access rule first".
  This note is that rule.

## The model

1. **Two parts: a flag on the membership and a join table.**
   `org_members.team_access text NOT NULL DEFAULT 'all' CHECK (team_access IN
   ('all','selected'))`, and a new table `team_members (org_id, user_id, team_id,
   created_at)` with primary key `(org_id, user_id, team_id)`, a composite FK
   `(org_id, user_id)` to `org_members ON DELETE CASCADE`, a composite FK
   `(org_id, team_id)` to `teams ON DELETE CASCADE`, and an index on `(org_id,
   team_id)`. The rows of `team_members` count only when `team_access =
   'selected'`.
   Why not only a table: with rows alone, "sees everything" must be one row per
   team per member. Then a new team needs a row for each unrestricted member,
   every membership insert (six paths) must also insert rows, and "no rows"
   cannot tell "all" from "none". Why not only a flag: it cannot name teams.
   The two together keep today's meaning as the default and make a new team
   visible to unrestricted members without a write.
2. **Org admins always see every team.** The role decides, not the flag.
   `team_access` of an admin is ignored (and stored as `all`; an invite for an
   admin with a team list is `400`). An admin who is made a member later keeps
   whatever the flag says. No route changes a role today; if one comes, it must
   set the flag in the same transaction.
3. **What "can see a team" covers.** Read and write of everything the team owns:
   the team row, its states, its issues (and their comments, attachments,
   relations, activity, state history, merge-request links), its cycles and their
   snapshots, its notifications. Write rights stay with the token scope
   (`read`, `write`): there is no read-only access to a team in v1.
4. **Existing members keep all teams.** The column default `all` does it. No
   row is written for any member on upgrade (#34).
5. **Zero teams is allowed on an edit, not on an invite.** A restricted member
   with no team rows sees only org-level things (#7). An edit that removes the
   last team must work (it is how access is taken away). An invite or sign-up
   approval with `teamAccess: "selected"` and no teams is `400` (inviting a
   person to nothing is a mistake), and an accept whose teams are all gone is
   `409` (#27). Only an edit by an admin makes a restricted member with no teams. (Open point B.)
6. **New teams.** A new team is visible to admins and unrestricted members at
   once. A restricted member never gets it unless an admin adds it. Nothing
   adds a team to a restricted member by itself, not even the member who
   created it (only admins create teams). An archived team stays in
   `team_members`.

## What stays visible to every member

7. **Org-level things.** The org row, `GET /users` (the member list), labels,
   `GET /me`, `GET /orgs`, the platform routes (which are install-level and do
   not read tenant rows), and a document with no project. The member list
   shows every member's name, username and role to everyone, as today; emails
   stay admin-only (T6a #20). Reason: names appear on every comment and
   assignment a member can see, and @mentions, assignee pickers and the
   importer user map need the roster. Hiding it costs a second visibility
   rule for no secret. The assignee picker (`web/src/features/issues/QuickPicker.tsx`)
   keeps listing the roster (#46). (Open point C.)
8. **Labels stay org-wide.** A label name is shared text. When team labels
   arrive (LAUN-54) a team label is a team-owned object and follows the rules
   here (it is listed in the inventory as "future"). (Open point H.)
9. **Hidden teams are not listed anywhere.** Not in `GET /teams`, not in a
   project's `teamKeys`, not in an error text, not in MCP's "unknown team"
   hint (which lists `GET /teams`), not in a count.

## Enforcement: one design

The goal of #10 to #20 is: the visible set is made once, every read and write
needs it to compile, one SQL shape uses it, and three tests fail when a new
route or query forgets it.

10. **A hidden thing is "not found", in the same words as a thing that does
    not exist.** A path reference to a hidden row is `404 not_found`. A
    reference in a body is `400 validation_failed` with the text of an unknown
    reference (the rule of AGENTS.md for another org's row). A filter value
    that names a hidden row gets the answer an unknown value gets today: an
    unknown `?team=KEY` is an empty list, an unknown `?cycle=<id>` or
    `?parent=<id>` is `400`. Never `403`. Never a different message, header or
    field. (Cross-org does the same today.)
11. **The check is part of the lookup, not after it.** The scope predicate sits
    in the same SQL statement that finds the row. The code never reads the row
    and then compares the team. Reason: one code path for "missing" and
    "hidden", so the answer and the time are the same (security section), and a
    caller cannot forget the second step.
12. **Org admins pass every check.** `Access.All` is true for an admin, so the
    `all_teams` argument is true and the predicate of #15 matches every row. The
    predicate text is always emitted (one rule, #15); only its arguments differ.

13. **The visible set is made once per request, in `RequireOrgMember`.**
    `GetMembership` also returns `team_access`. For `all` (and for admins)
    that is the whole cost. For `selected` one more query reads the
    caller's `team_members` rows (primary-key range, a handful of rows).
    `auth.Org` gets one field:

    ```go
    type TeamAccess struct {
        All bool
        IDs []uuid.UUID // sorted; empty when All
    }
    ```

    with `Can(teamID)`, `Args() (all bool, ids []uuid.UUID)` for SQL, and
    `Empty()`. It is built from the user, never from the token (#32). It is
    read again on every request and every SSE poll tick; there is no cache
    across requests, so a change applies at once. A cache is not added: the
    lookup is a primary-key read, and a cache would need an invalidation rule
    across servers that this design does not need.
14. **Every resolver takes the org, not an id.** `resolveIssueID(ctx, q,
    orgID, ref)` becomes `resolveIssueID(ctx, q, org *auth.Org, ref)`, and the
    same for `resolveTeam`, `resolveState`, `resolveCycleID`, `cyclePathID`,
    `resolveDocumentID`, `checkPlanningRefs`, `teamFilterID`, `getProject`,
    `getDocument`, `GetMilestone` and the milestone, project and cycle
    existence queries. They add the predicate (#11). A raw-`uuid` version no
    longer exists, so a new call site cannot compile without an `*auth.Org`.
    The type system is the first fence. It must cover every package that
    touches tenant rows, not only `tracker`: `notify.Service` inbox methods,
    the consumer and the sender also take the `*auth.Org` (or a `TeamAccess`)
    and call scoped queries. Code with no caller (the cycle job, webhooks, the
    importer, the consumer's own reads) calls `System` copies with a visible
    name (`GetCommentSystem`, `GetTeamByIDSystem`), so a scoped query cannot be
    reused by mistake. `ResolveImportedID` is entity-generic and returns a bare
    id; it is split into one scoped variant per entity (issue, team, cycle),
    and each resolver checks the returned id with the predicate. Linear ids are
    samples in Test B.
15. **One SQL shape.** In sqlc files, a team-scoped query takes two arguments,
    `sqlc.arg(all_teams)::bool` and `sqlc.arg(team_ids)::uuid[]`, and the
    predicate is ALWAYS emitted, for every caller, in this form:

    ```sql
    AND (sqlc.arg(all_teams)::bool OR i.team_id = ANY(sqlc.arg(team_ids)::uuid[]))
    ```

    In the four hand-written lists, a helper `teamScope(org, "i.team_id",
    arg)` returns the same text and adds the two arguments; it never returns an
    empty clause. For an all-teams caller `all_teams` is true and the `OR` is
    satisfied. One rule, so Test C can check that the text is present and no code
    path can leave it out. A project is
    visible by `projectScope(org, "p", arg)`, the any-of rule of #36. The
    helpers are the only place the SQL text lives.
16. **Every query and every SQL string carries a scope tag.** A comment inside
    the SQL says what the query is:

    | Tag | Meaning |
    |---|---|
    | `-- scope: team` | filters by the visible set (#15); has both arguments |
    | `-- scope: project` | uses the project rule (#36) |
    | `-- scope: resolved` | by primary key; the caller got the id from a scoped resolver (#14). Listed in the inventory with its resolver |
    | `-- scope: own` | the caller's own rows (inbox, settings); the join to an issue is filtered (#62) |
    | `-- scope: org` | an org-level table (#7) |
    | `-- scope: admin` | the route is org-admin only |
    | `-- scope: system` | the cycle job, the notification consumer, webhooks, the importer, setup: no caller |

    Tags `none`, `own`, `org`, `system` and `admin` opt out of the predicate, so
    each use is listed by query name in a reviewed golden file
    (`internal/api/testdata/scope_optouts.golden`); a new entry needs review.
    Test C reads whole queries. It cannot check SQL that the code composes at
    run time (`issueSelectFull` plus a `WHERE`); Test B carries that. Test C below fails when a query on a tenant table has no tag, when a `team`
    query lacks the two arguments, and when a `resolved` query is not in the
    reviewed list.
17. **Every route declares its team rule.** `internal/api/teamrules.go` holds
    one table, keyed `METHOD pattern`, with the rule kind: `list(resource)`,
    `path(resource, param)`, `body(field)`, `filter(param)`, `feed`, `own`,
    `admin`, `none(reason)`, and an `Enforced` flag that each build ticket
    flips. Test A walks the chi tree (`chi.Walk`) and fails on a route with no
    entry and on an entry with no route.
18. **Three tests make it hard to forget.**
    - **Test A, `TestTeamRulesCoverRouter`:** every route under `/orgs/{org}`
      has a rule (#17). A new route cannot merge without a decision.
    - **Test B, `TestTeamRules_RestrictedMember`:** for every rule that is not
      `none` or `admin`, it builds a fixture (team A visible, team B hidden,
      one object of every kind in B), calls the route as a restricted member
      with a sample that names the hidden object, and asserts: `404` (path),
      `400` unknown reference (body), no hidden row in the list (list and
      filter), the hidden row unchanged (writes), and a body byte-for-byte the
      same as for a made-up id, after it replaces the echoed reference (for
      example the id in "unknown state %q") with a placeholder. For `admin` rules it asserts `403`. The sample
      for each rule (a Linear id of a hidden issue, team or cycle among them) is a function in the table, so adding a route forces the
      author to write what "a hidden one" is.
    - **Test C, `TestQueriesDeclareScope`:** parses `db/queries/*.sql` and the
      SQL string literals of non-test Go files that mention a tenant table
      (the tables with `org_id` in `db/schema.sql`) and enforces #16. It fails on an
      untagged query, on a `team` query without the two arguments, on a `resolved`
      query not in the reviewed list, and on a new opt-out tag (`none`, `own`, `org`,
      `system`, `admin`) that is not in `scope_optouts.golden`.
    A fourth, **Test D, `TestEventsEmitScope`** (#53), fails when an event kind
    has no scope function. It also emits each event kind through its real route
    from a hidden team and asserts a restricted viewer reads nothing from
    `/events` or the stream.
19. **Access that cannot be written yet cannot leak.** Until the last
    enforcement ticket is merged, nothing in the API can set `team_access` to
    `selected` (the setter routes are the next-to-last tickets). Tests insert
    rows directly. A test (`TestNoSelectedWriterWhileUnenforced`) fails if
    `api/openapi.yaml` has any request schema with a `teamAccess` property while
    any rule in #17 has `Enforced: false`. It reads the spec, not a hand-set flag. This is the "never leaves a route unenforced" rule of
    the ticket, as a test and not as a promise.
20. **Not row-level security.** Postgres RLS with a per-transaction setting would
    give one fence at the database. It was not chosen: AGENTS.md says "No RLS.
    Tenancy is in the application", pool connections would need a `SET LOCAL`
    in every transaction (and a miss reads everything), and sqlc queries,
    the consumer and the importer would need roles. It stays an option as a
    second fence later. (Listed under "Not done".)

21. **Writes use the same resolvers.** A write to an issue, comment, relation,
    attachment, subscription or cycle resolves its target with #14, so a
    restricted member cannot write where they cannot read. `POST /issues` with a
    hidden `team` is `400` unknown team (#10).
22. **No `403` on team grounds, no new error code.** `ErrorCode` in
    `api/openapi.yaml` and `httpx.Codes` stay as they are.
23. **Error texts name nothing hidden.** These existing texts give a hidden
    row away and change (#10):
    - `checkPlanningRefs`: "cycleId is a cycle of another team" and "milestoneId
      is a milestone of project %s" (it prints a project id). Under scope, a
      hidden project, milestone or cycle is "unknown projectId / milestoneId /
      cycleId". A visible milestone of a visible project keeps its text.
    - `createComment`: "parentId is a comment on another issue" becomes
      "unknown parentId" when the parent is not a comment of this target (the
      lookup takes the target in its WHERE).
    - `resolveState`: a state id of a hidden team and an unknown state id give
      the same text; `GetStateByID` is scoped by the team join.
    - `checkNoParentCycle`: the cycle walk (`issue_write.go`) can pass through
      issues the caller cannot see. Its failure answers "unknown parent", the
      text of an unknown parent, so it names no hidden issue.
    - `CreateRelation`: "that relation already exists" is reachable only for two
      visible issues.
    - `placeAfter` / `placeBefore` (`rank.go`): the neighbour is resolved with
      #14.
24. **Pagination stays honest.** A list filters in the `WHERE`, before `LIMIT`.
    It never fetches and drops rows in Go. A page is full or it is the last
    page; `nextCursor` never says "more" for rows the caller cannot see.
25. **Idempotency replay.** A stored response (24 h, per user and key:
    `idempotency.TTL`) is replayed before the handler runs, so after access is
    removed a replay returns what the same caller received earlier. It adds
    nothing new to what they saw; it is accepted and written here so nobody
    wonders. (Not done: a re-check on replay.)

## Invites, sign-ups, agents

26. **Invites carry the access.** `org_invites` gets `team_access text NOT NULL
    DEFAULT 'all'` and a table `org_invite_teams (org_id, invite_id, team_id)`
    with composite FKs (arrays cannot hold FKs). `POST /orgs/{org}/invites` takes
    `teamAccess` (`all`, the default, or `selected`) and `teams` (keys or ids,
    required and non-empty for `selected`, at most 50, each a team the admin can
    see: admins see all). The invite view and `GET /invites` list the team keys.
27. **Accept does it in the same transaction.** In `AcceptInvite`, after
    `InsertOrgMemberRole` for a new or existing user, the same `tx` writes
    `team_access` and the `team_members` rows from the invite, only when
    `InsertOrgMemberRole` inserted a row (it is `ON CONFLICT DO NOTHING`; an
    existing member keeps their access). A team that no
    longer exists is gone by the cascade; an archived team still counts. If every
    chosen team is gone, accept FAILS with `409 conflict` and a clear text, and the
    invite stays open (the transaction rolls back), so the invite path never makes
    a restricted member with no teams. The admin revokes it and sends a new one.
28. **An invite for someone who is a member already is refused today** (409 "a
    member of this org has that email already"). That stays. So "add teams to an
    existing member" is not an invite: it is the members page and the setter of
    #48. An invite for an existing user who is not a member of this org (an
    install-level user) adds the membership with the invite's access, as
    `InsertOrgMemberRole` does today for the role.
29. **Sign-up approval** (`POST .../signups/{id}/approve`) takes an optional JSON
    body `{teamAccess, teams}` with the same rules. No body is `all` (as today).
    The route keeps `s.idem`; the body holds no secret.
30. **An admin invite ignores teams.** `role: admin` with `teams` is
    `400 validation_failed` (#2).
31. **Agents get access the same way.** `POST /orgs/{org}/agents` takes the same
    optional `{teamAccess, teams}`. Omitted is `all`, so the importer setup and
    `deploy.sh --smoke` do not change. An agent is a member with role `member`;
    the same `team_members` rows apply; the same predicate runs. The throwaway
    smoke agent keeps `all`.
32. **A PAT never exceeds its owner.** The visible set comes from the token's
    user at request time (#13). `api_tokens` gets no team column. A narrowing
    "this token sees only team X" is a different feature (open point I).
    `server mint-token` mints for any active member: the token sees what the
    member sees. A web session is the user's own, so the same.
33. **`GET /users` shows access to admins only.** The `Member` object gets
    `teamAccess` and `teams` (keys); both are `null` for a non-admin, as email
    is today. Restricted members never learn who else is restricted. No write path reveals it,
    because an assignment never fails on access (#46).

## Migration

34. **Four small migrations, in four tickets.** `develop` is at `0013` today.
    `0014` is not reserved for this work: LAUN-87 and the products tickets also
    need numbers. The T18 #25 rule holds: the first MR to merge takes the next
    free number. A later branch renumbers its file pair, then runs `make schema
    sqlc` and `TestMigrateUpDownUp` again. The four files are independent and
    may merge in any order.
    - **M1 (ticket 1):** `ALTER TABLE org_members ADD COLUMN team_access ...
      DEFAULT 'all'`; `CREATE TABLE team_members`. A constant default is a
      metadata write in Postgres 16, so it is fast on any size. The down drops
      the table and the column. No member changes.
    - **M2 (ticket 6a):** `events.scope text NOT NULL DEFAULT 'legacy'`,
      `events.team_ids uuid[] NOT NULL DEFAULT '{}'`. The migration does not
      backfill (#57 is a job). The down drops both columns.
    - **M2b (ticket 7):** add `access_removed` to the `skip_reason` CHECK of
      `notification_emails` (#63). The down puts the old CHECK back; rows with
      the new value first become `disabled` (it rewrites those rows, and a test
      covers it).
    - **M3 (ticket 10):** `org_invites.team_access`, `org_invite_teams`. The down
      FAILS with a clear error while open invites with `team_access = 'selected'`
      exist. It never deletes rows by itself. After the operator revokes them (#35
      step 2) it drops the table and the column.
35. **Rollout.** M1 changes nothing a person sees. Enforcement lands resource by
    resource while no restricted member can exist (#19). The feature goes live
    when ticket 9 (the setter) and ticket 10 (invites) merge. So the rollback
    runbook is written in ticket 9. Rollback steps, in this order:
    1. Stop the app, so no admin can change access or invites during the steps.
    2. Revoke every open invite with `team_access = 'selected'`, as an explicit
       operator step (the runbook gives the SQL). Otherwise an older image would
       turn it into an all-teams invite, and the M3 down refuses to run.
    3. `UPDATE org_members SET team_access = 'all'`.
    4. Deploy the older image. It ignores the new column and tables. It writes
       only `all` for sign-up approvals and agents, which is correct. It writes
       events with the `legacy` scope; the job of #57 sorts them after a later
       roll-forward.


## Cross-team objects

36. **A project is visible when the caller sees one of its teams, or when it has
    none.** `projectScope` is `NOT EXISTS (SELECT 1 FROM project_teams ...)` OR
    `EXISTS (... team_id = ANY(visible))`. A project with no teams is org-level,
    as today. Its name, description, content, state, dates and lead are visible
    to everyone who can see the project. Why: one team must not hide a shared
    project from the other teams that work in it, and every-team would hide it
    from all. Any-of matches "a project spans teams".
37. **What a partially visible project shows.** The fields of #36, and:
    - `teamKeys` / `teams` list only the visible teams. A hidden team is not
      listed and not counted.
    - `GET /issues?project=<id>` and the project's issue list return only issues
      of visible teams (the issue predicate still applies). No issue count, no
      progress number from hidden issues. Today a project has no aggregates
      (`projectSelectTmpl`); any aggregate added later (T18 product page,
      progress bars) must count visible issues only and has a test.
    - Milestones follow the project (#38).
38. **Milestones, documents and document comments follow the project.** A
    milestone is visible when its project is. A document with a project follows
    the project; a document with no project is org-level (#7, open point G). A
    document comment follows its document.
39. **Writing a partially visible project.** A restricted member with `write`
    can edit its fields. The `teams` list names visible teams only. The replace
    keeps the rows of hidden teams (as T18 keeps archived ones;
    `replaceProjectTeams` takes the visible subset). A hidden team name is `400`
    unknown team. A restricted member cannot remove the last team they see
    (`400`), or the project would vanish from them. A restricted member who
    creates a project must name one of their teams (`400` otherwise), or the
    project would be org-wide. A restricted member cannot edit the teams of a
    project that has no teams: that is admin only. Otherwise they could add
    their own team and hide an org-wide project from everyone else. (Open
    point F.)
40. **An issue's `projectId` / `milestoneId`.** Because an issue may sit in a
    project that lists none of its team (today's rule), a viewer may see the
    issue but not the project. Then `projectId` and `milestoneId` are returned as
    `null` for that viewer (the join checks `projectScope`), the `project=`
    filter matches visible projects only (a project name filter must not reveal a
    hidden project's name), and the write that sets them uses the scoped
    resolver. This is read-time redaction, not a data change. The `PATCH` bodies already
    tell absent from `null` (`httpx.Opt` has `Set` and `Null`), so the rule is on
    the server: an explicit `null` for `projectId` clears it only when the issue's
    current project is visible to the caller. When the current project is hidden,
    a `null` is a no-op and the response is `200` with the issue as the caller
    sees it. The same hidden value sent again is NOT a no-op: it is judged as a
    new value and gets the `400` of an unknown id, in the same order, text and
    statements as a made-up id (a `200` would confirm a guessed hidden id;
    amended by ticket 5, LAUN-220). The same holds for `milestoneId`, which is
    judged by its project. A hidden current value can never be cleared by a
    restricted caller with `null`. A non-null visible value replaces it (an
    explicit change to something the caller can see). A visible project that
    replaces a hidden one clears the hidden milestone with it (the milestone
    belongs to the project that left, and the caller cannot clear it otherwise;
    ticket 5). Accepted residuals of that clear: it lists `milestoneId` in the
    `issue.updated` `changes` and writes a milestone activity row (null to null
    for a restricted reader, #43), so the reader learns the issue had a
    milestone they could not see; and the extra indexed lookup of
    `hiddenPlanningRef` is a timing difference between a hidden and a missing
    current value (the security analysis claims equal paths per request body,
    not equal times). The web client sends only the fields the person changed, so a
    read-then-write of the whole object that echoes the redacted `null` clears
    nothing hidden. Ticket 5 has the test: a restricted caller sends `null` for a
    hidden project and for a visible one, and the first leaves the project in place
    while the second clears it. (Open point F also
    asks whether attaching an issue should require its team to be in the
    project's team list; that would change a rule that exists today, so it is not
    done here.)
41. **Relations to hidden issues are omitted.** `relationsFor` returns only
    edges whose other issue the viewer can see; the response has no placeholder
    and no count. `GET /issues/{id}?include=relations` and
    `GET .../relations` use the same rule. Creating or deleting an edge needs
    both issues visible (#21). The row stays in the database. (Open point D.)
42. **A hidden parent is omitted.** `parent` is `null` in `issueSelectTmpl`'s
    result when the parent's team is hidden (the parent join gets the predicate).
    Sub-issues of a visible parent that sit in a hidden team do not appear in
    `?parent=`. Setting a hidden parent is `400` unknown parent (#10).
43. **Activity snapshots are redacted at read time.** `ListActivity` returns the
    stored `from` / `to` JSON. For `parent` and `relationRemoved` rows the JSON
    holds another issue's identifier and title; for project, milestone and cycle
    rows it holds a name. At read time the service looks up the referenced id with
    the scope predicate; if the viewer cannot see it, the snapshot is replaced by
    `null`. The write path stays as it is (the stored text is not changed,
    which also keeps the history for an admin). A row whose both sides become
    `null` is still returned (the fact of an edit is the viewer's own issue's
    fact).
44. **State history and comments** of a visible issue are visible. Comments by
    an author who has since lost access stay (the author name is org-level, #7).
45. **Mentions.** Text that names a hidden issue (`LAUN-12`) is plain text: the
    markdown renderer (`web/src/lib/markdown.tsx`) does not look up issues, and
    the server never expands an identifier into a title in a response. So no title
    shows. If a later ticket adds issue links with titles, it must resolve them
    with `GET /issues/{id}` (`404` for a hidden one). A mention of `@user` for a
    user without access to the issue makes no notification and no mention
    subscription (#61). The comment author is not told.
46. **Assignee: any member may be assigned.** Today `resolveAssignee` answers
    `400` "assignee %q is not a member of this org" for a name that is not a member
    (`service.go`; the project lead check in `projects.go` has a similar text for
    "lead"). That stays. A success or a failure that depended on the assignee's
    team access would be a probe: a restricted member with `write` could try every
    roster name and learn who else is restricted. So the assignment does not check
    access (option L(c)). The person assigned gets no notification of any kind
    (#61), cannot open the issue, and is counted in the `dryRun` of the setter
    (#48) so an admin can fix it. A project lead is the same: the lead need not see
    the project. `assignee=me` and `assignee=<name>` filters are unchanged.
47. **Labels on an issue** are org-level (#8) and are returned as they are.

## Removing access

48. **One setter, replace semantics.** `PUT /orgs/{org}/users/{user}/team-access`
    (admin scope) with `{"teamAccess":"all"}` or `{"teamAccess":"selected",
    "teams":[...], "assignedIssues":"unassign"|"keep"}`. It replaces the whole
    set, is idempotent, and runs in one transaction. `dryRun=true` returns what
    would change and writes nothing. It refuses a target that is an org admin
    (`400`: admins always see all, #2). It returns `409` and lists the repos while
    any enabled map row has `team_id` NULL and the new setting would restrict
    someone (#65). The `dryRun` and the real response also count the open issues
    assigned to the target who cannot see their team (the target only, not the whole
    org) (#46). It emits `member.access_changed` with the
    `admin` scope (#54) and an `audit:` log line (no IP, no User-Agent).
49. **Assigned issues.** Without a choice, a removal that would leave open issues
    assigned to the member in a team they lose is `409 conflict`, and the body
    says how many (per team). `assignedIssues: "unassign"` clears the assignee on
    those open issues (state type not `completed` or `canceled`) with
    `issue_activity` source `system` and an `issue.updated` event per issue.
    `"keep"` leaves them. Closed issues are never touched. The recommended
    choice in the web is "unassign". (Open point E.)
50. **Subscriptions, follows, preferences.** In the same transaction, for each team
    lost: delete the member's `issue_subscriptions` on issues of that team,
    their `team_follows` row, and their per-team `notification_settings` rows.
    Delete is a hard delete, not a mute. If access comes back, they re-subscribe;
    implicit subscriptions (creator, assignee, commenter) work again by
    themselves.
51. **Inbox rows stay but are hidden.** `notifications` rows of the member for
    issues of a lost team are not deleted. The inbox queries filter them (#62). If
    access returns they reappear; retention (`notify.Prune`) removes them in time.
52. **Drafts and open pages.** There is no server-side draft. A comment draft
    lives in the page; the next call is `404`, and the page shows the not-found
    state (web section). An open SSE stream ends or narrows on its next poll (#56).
    Sessions and tokens stay valid: they were never team-bound.

## The change feed and SSE

53. **Every event has a scope; `Emit` requires it.** `events.Emit(ctx, q, orgID,
    ev, scope)` takes a `Scope` as a positional argument, so the 21 call sites do
    not compile until each names one. Scopes:
    `events.ScopeTeams(ids...)` (visible when the viewer sees any listed team),
    `events.ScopeAllTeams(ids...)` (visible only when the viewer sees all of them;
    a relation between two issues), `events.ScopeOrg()` (everyone; labels),
    `events.ScopeAdmin()` (org admins only). Stored as `events.scope` (`any`,
    `all`, `org`, `admin`) and `events.team_ids`.
54. **Which scope each kind gets.**

    | Event | Scope |
    |---|---|
    | `issue.created`, `issue.updated` | teams: the issue's team |
    | `comment.*` on an issue | teams: the issue's team |
    | `comment.*` on a document | teams: the document's project teams; org if it has none |
    | `attachment.created` | teams: the issue's team |
    | `relation.created`, `relation.deleted` | all-of: both issues' teams |
    | `team.created`, `team.updated` | teams: that team |
    | `cycle.created`, `cycle.updated`, `cycle.completed` | teams: the cycle's team |
    | `project.*`, `milestone.*`, `document.*` | teams: the project's teams at emit time; org if none |
    | `label.created` | org |
    | `member.access_changed` (new) | admin |
    | GitLab / GitHub moves (`issue.updated`, `attachment.created` from `HandleSCMEvent`) | teams: the issue's team |
    | Cycle job (`MaintainCycles`) | teams: the cycle's team |

    The scope is frozen at emit time; the viewer's side is evaluated at read time
    (#55). Residual: project and document events keep their names readable for a
    former member of a team removed from the project later, because the scope was
    frozen at emit time. Payloads stay as they are: they hold ids and identifiers. A few hold names:
    `project.created`, `milestone.created` and `label.created` carry the name.
    Their scope (project teams, project teams, org) decides who reads the name.
55. **One predicate reads the feed.** `ListEventsAfter` takes `all_teams`,
    `team_ids` and `is_admin`. Unrestricted viewers (`all_teams`) skip the team
    test but not the `admin` scope. For a restricted viewer:

    ```sql
    scope = 'org'
    OR (scope = 'any' AND team_ids && sqlc.arg(team_ids)::uuid[])
    OR (scope = 'all' AND team_ids <@ sqlc.arg(team_ids)::uuid[])
    ```

    `legacy` rows (made before M2 and not yet placed by the job of #57) are visible to unrestricted
    viewers only (fail closed). `GET /events` and `GET /events/stream` use it.
    The notification consumer uses a system variant that reads everything (it
    filters recipients itself, #61).
56. **The stream follows access changes.** `streamEvents` re-reads the caller's
    `TeamAccess` (one primary-key read for a restricted member) at the start of
    every drain, so a wake-up from NOTIFY is covered as well as the 10 s poll. If
    the set changed, it first sends a named SSE event `access` (`event: access`,
    empty data), then drains with the new set. If the membership row is gone on the re-read, the stream ends, and the code never
    treats a missing row as an empty restricted set. It also ends the stream through
    `StillAuthorized` when the token, user or membership is gone. The inbox stream
    (`streamInbox`, `GET /notifications/stream`) does the same: it re-reads the
    access at the start of every drain, and its row query takes the predicate
    (#62). The web adds `access` to its event list (`web/src/api/events.ts`) and
    invalidates every query on it. The `access` event is not stored in `events` and
    goes to that user's connections only.
57. **Old events (M2): a job, not a migration statement.** The migration only adds
    the columns, so it is fast and a failure cannot block a boot. A post-boot job
    (started in `cmd/server`, safe with several instances by an advisory lock,
    batches by `seq` range) sets `scope` and `team_ids` for old rows from the
    entity: issue, comment (issue, else document and its project), attachment,
    relation (both issues), cycle, team, project, milestone, document. Where the
    entity is gone it uses the payload (`issueId`, `teamId`, `projectId`). What it
    cannot place stays `legacy`. `legacy` rows fail closed (#55): a restricted
    member never reads them, whether the job is slow, skipped or unfinished. The
    time of the job is not measured. The setter of ticket 9 does not need the job
    to be finished. The job is re-runnable. It runs at every boot until no
    placeable `legacy` row is left, because an older image after a rollback
    writes `legacy` rows again. While it runs, a restricted member sees new events
    and not the old ones; the old ones appear as the job places them.
58. **Residual: `seq` gaps.** The feed's `seq` is dense per org. A restricted
    viewer sees gaps where hidden events were. A gap tells that something
    happened, not what or where. The cost of a dense per-viewer number is a new
    counter per viewer and resume rules that break `Last-Event-ID`. Accepted. `LatestEventSeq` and `seq` also show how many events the org made, a
    volume signal and no content. (Open point J.)
59. **`LatestEventSeq`** (the "now" resume point) is the org's newest seq. It
    names no event. Unchanged.
60. **Event kinds to add to the web list.** `access` (SSE name only). The stored
    kind `member.access_changed` is not delivered to non-admins, so the web needs
    no handler.

## Notifications and mail

61. **The consumer decides recipients with access.** `ListNotifyEligibleUsers`
    and `ListMentionableUsers` get the issue's team and join `org_members` /
    `team_members` with `(team_access = 'all' OR role = 'admin' OR EXISTS (...))`.
    A user without access gets no candidate of any kind, including a direct
    mention (#45). Both queries are tagged `system`. The `new_issue` candidates
    from `ListTeamFollowers` go through the same eligibility filter, so a
    follower who lost access (and was not cleaned, #50) gets nothing.
62. **Reads re-check, as well.** The consumer's check is at make time. Access can
    be removed later, so every inbox read filters at read time: `ListInbox`,
    `GetInboxItem`, `CountUnreadInbox`, `SetInboxItemState`, `MarkInboxReadBefore`
    and `ListInboxStreamRows` join `issues` and add the predicate. The unread
    count excludes hidden rows, so the bell number matches the list and leaks
    nothing. `read-all` marks only visible rows. A direct `PATCH
    /notifications/{id}` on a hidden row is `404`. `notify.Service` inbox methods
    (`internal/notify/inbox.go`) take the `*auth.Org` (#14), and the inbox stream
    re-reads the access at every drain (#56).
63. **The mail sender re-checks at send time.** `GetMailIssue` is read when an
    email is made. The sender skips (state `skipped`, a new reason
    `access_removed`; the CHECK `notification_emails_skip_reason_check` gets the
    value in a small migration, M2b, in ticket 7) a row whose recipient no longer sees the issue's team. No
    title or text of a hidden issue is ever sent.
64. **Routes under `/me`.** `PUT /me/teams/{team}/follow`,
    `PUT /me/notification-settings/teams/{team}` and the three issue subscription
    routes resolve their team or issue with #14 (hidden is `404`). The member's
    settings view lists only visible teams.

## Webhooks, MCP, import, setup

65. **Webhooks act as the integration, and answer with a status only.** GitLab and
    GitHub events (`HandleSCMEvent`) run with system authority. `linkAndMove`
    matches an issue named in the branch or title by team key and number
    (`FindIssuesByKeyNumber` has no team filter) across all teams, unless the
    optional `git_repo_map.team_id` limits it (`onlyTeam`). Today the response body
    (`tracker.WebhookResult`, written by `api/gitlab.go` and `api/github.go`) lists
    the identifiers it `linked` and `moved`. Counts would be an oracle too: `1`
    versus `0` tells a repo maintainer whether a hidden issue exists and whether
    its state allows a move. So the body is `{"status": ...}` and nothing else, and
    the status does not depend on whether an issue matched. Test: the body is
    byte-identical for a hidden, a visible and a missing issue.
    **The webhook guard.** The write path (link, move, attachment,
    `issue.updated`) is closed by ONE rule (open point M): while any enabled
    map row has `team_id` NULL, no restricted access may exist, and none may be pending or
    be created. Restricted access means a member with `team_access = 'selected'` or
    an open invite or approval that would make one. A repo map has one nullable
    `team_id` and `UNIQUE (integration_id, repo_key)` (`git_repo_map_repo_key`), so
    the rule means one team per repo. The match then uses only that team. One
    helper, `CheckWebhookGuard(ctx, q, orgID)`, answers `409` and lists the repos.
    It runs in the same transaction as the change, under one advisory lock on the
    org (`pg_advisory_xact_lock` on a hash of `launchpad-team-access` and the org
    id) that every entry below takes first, so a concurrent insert cannot slip in.
    Entries, each with a bypass test:
    1. the setter (#48), when it makes or keeps a `selected` member;
    2. `POST /invites` with `selected` (#26), and `AcceptInvite` again inside its
       transaction (#27), because a repo may have lost its team in between;
    3. sign-up approval with `selected` (#29);
    4. `POST /agents` with `selected` (#31);
    5. `POST /integrations` and `PATCH /integrations/{id}` that enable an
       integration (`enabled` true again after a disable);
    6. `POST` and `PATCH /integrations/{id}/repos` that leave a `team_id` empty on
       an enabled integration;
    7. deleting a team. No route deletes teams today. The FK
       `git_repo_map_team_fk` is `ON DELETE SET NULL (team_id)`, so a future
       delete route must run the guard first.
    Two terms are used in this note. "No map row": the webhook answers `202
    unmapped` and links nothing; the guard does not care. "Map row with `team_id`
    NULL": it matches issues of every team today; the guard forbids it while
    restricted access exists, and a row with a `team_id` matches that team only.
    A repo shared by two teams cannot be mapped to both. Under the rule it serves
    one team, needs a second integration, or has no map row (the webhook answers
    `202 unmapped` and links nothing). A repo-to-team table would lift this; no
    ticket is planned for it. Residual: a person who can push to a repo mapped to
    team T can still link and move issues of T, even if they cannot see T.
    **Supersedes:** `decisions-T5.md` (the response body `{status, linked,
    moved}`) and `decisions-T17-git-connections-import.md` (the same body, in
    the webhook request part). Ticket 8 adds a one-line "superseded by T20 #65"
    pointer to both. `required: [status, linked, moved]` in `api/openapi.yaml`
    changes to `required: [status]`, and `linked` / `moved` leave
    `tracker.WebhookResult`. The tests that read `.Linked` and `.Moved`
    (`gitlab_integration_test.go`, `github_integration_test.go`) change.
    The events it writes are scoped to the issue's team (#54). No change to the
    secret checks; the routes stay public (AGENTS.md). Integration config is
    admin-only already.
66. **MCP needs no code for access.** It uses REST with the token, so the server
    enforces. What changes: its team list is the visible list (#9), so a default
    `LAUNCHPAD_TEAM` that the owner cannot see answers "unknown team", and a
    test runs every tool with a restricted token (`mcp_integration_test.go`). The
    golden files do not change. The reviewer profile is read-only and its token
    obeys the same rule.
67. **The Linear import.** The importer and the web import job (`importjob`) write
    with system authority, directly. Imported members are mapped to existing
    members (`GetOrgMemberByUsername`), who are `all` unless an admin set
    otherwise. Teams it creates (`CreateTeamTx`) are visible to `all` members and
    to nobody restricted until an admin adds them. Import-mapped ids
    (`import_mappings`) resolve to the Launchpad row first, and the scope
    predicate runs on that row (#14), so a Linear id is not a back door. Imported
    members "get all teams" by default (the ticket's line).
68. **Setup, `testdb.Fixture`, `deploy.sh --smoke`.** The first user is an admin.
    The fixture's users are `all`. A new fixture helper makes a restricted member.

## Products (T18)

69. **A product is still not an access boundary.** It is a grouping of teams. With
    this note, a product shows only the teams the viewer can see; a product whose
    teams are all hidden is not listed to that viewer (it is visible when the
    viewer sees at least one team of it, or it has none, the project rule of #36).
    `product=` filters return the union of visible teams' issues only. T18's
    "Other teams" group is computed from the visible list. T18 #26 is satisfied:
    the boundary is the team, as it asked. Whoever builds T18 adds the rule in its
    inventory rows (listed below as "future").
70. **No product-level grant** in v1: no "give access to a product's teams" (it
    would change when an admin edits a product). A picker may offer "select all
    teams of this product" as a convenience that writes team rows once (web
    section).

## Inventory: every route, query and feed that needs a team rule

"Rule" uses the kinds of #17. A build ticket may not merge until every row in its
column is `Enforced` and has a Test B case. Rows are grouped as in
`internal/api/router.go`.

### Routes under `/orgs/{org}`

| Route | Resource | How the team is derived | Rule and notes |
|---|---|---|---|
| `GET /` | org | the default team (`orgs.default_team_id`) | `list(org row)`, ticket 3 (LAUN-217 review): the row carries the default team's key (`GetOrgFull`); a hidden default team must not be named (#9), so the key is `null` for a viewer who cannot see that team |
| `GET /teams` | team | `teams.id` | `list(team)`: only visible teams; archived stay hidden as today |
| `POST /teams` | team | new | `admin`: a new team needs no row (#6) |
| `GET /teams/{team}` | team | `resolveTeam` | `path(team)`: key, id or Linear id; hidden is `404` |
| `PATCH /teams/{team}` | team | `resolveTeam` | `admin` (and the scoped resolver, for the Linear id path) |
| `GET /teams/{team}/states` | workflow state | `resolveTeam` | `path(team)` |
| `GET /projects` | project | `project_teams` | `list(project)`: #36; `?team=` filter: hidden key is an empty list; `teamKeys` shows visible only |
| `POST /projects` | project | body `teams` | `body(teams)`: visible teams only; restricted member needs at least one (#39) |
| `GET /projects/{project}` | project | `project_teams` | `path(project)` |
| `PATCH /projects/{project}` | project | `project_teams` | `path(project)` plus `body(teams)` rules of #39; `lead` is any member (#46) |
| `GET /milestones` | milestone | project | `list(milestone)`: list gets `projectScope` (today it filters only by `org_id`); `?project=` of a hidden project is `400` "unknown project", the answer for an unknown id today (`ListMilestones`) |
| `POST /milestones` | milestone | body `projectId` | `body(projectId)` |
| `GET /milestones/{milestone}` | milestone | project | `path(milestone)` |
| `PATCH /milestones/{milestone}` | milestone | project | `path(milestone)` |
| `GET /cycles` | cycle | `cycles.team_id` | `list(cycle)`: `?team=` hidden is empty; `teamFilterID` scoped |
| `GET /cycles/{cycle}` | cycle | `cyclePathID` | `path(cycle)`: cycle id or Linear cycle id |
| `PATCH /cycles/{cycle}` | cycle | `cyclePathID` | `path(cycle)` |
| `GET /cycles/{cycle}/snapshots` | cycle snapshot | `cyclePathID` | `path(cycle)`; the lazy refresh of today's row (`CycleSnapshots`) runs only after the check |
| `GET /documents` | document | project, or none | `list(document)`: `d.project_id IS NULL OR projectScope`; `?project=` hidden is `400` "unknown project", as an unknown id today |
| `POST /documents` | document | body `projectId` | `body(projectId)` |
| `GET /documents/{document}` | document | project | `path(document)`: id or slug |
| `PATCH /documents/{document}` | document | project | `path(document)` plus `body(projectId)` |
| `GET /documents/{document}/comments` | comment | document's project | `path(document)` |
| `POST /documents/{document}/comments` | comment | document's project | `path(document)` |
| `GET /project-labels` | project label | the projects it is on | `list(project label)`, ticket 3 (LAUN-218 review): the catalogue is org-wide (decisions-T28), `projectCount` counts only projects the caller sees (#36) |
| `GET /labels` | label | `labels.team_id` (LAUN-54) | `list(label)` with the `?team=` ref, ticket 3: team labels of hidden teams are not listed; `issueCount` counts visible issues (scoped in 2a) |
| `POST /labels` | label | body `team` | `body(team)`, ticket 3: a team label of a hidden team is `400` unknown team; an org label (no team) stays org-wide (#8) |
| `GET /issues` | issue | `issues.team_id` | `list(issue)`: predicate; filters `team`, `project`, `cycle`, `parent`, `assignee`, `label`, `state`, `query` all run inside it; `?team=` hidden is empty; `?cycle=` / `?parent=` hidden is `400` unknown; `query` is a search and must not match hidden rows; `include` of parent title: #42 |
| `POST /issues` | issue | body `team` | `body(team)` and body refs `parent`, `projectId`, `milestoneId`, `cycleId`, `assignee` (any member, #46), `labels` (#23) |
| `GET /issues/{issue}` | issue | `resolveIssueID` | `path(issue)`: identifier, id or Linear id; `include=relations,attachments,stateHistory,comments` are of the same issue |
| `PATCH /issues/{issue}` | issue | `resolveIssueID` | `path(issue)` plus the body refs above and `placeAfter`, `placeBefore`, `sortOrder` neighbours (`rank.go`) |
| `GET /issues/{issue}/activity` | activity | `resolveIssueID` | `path(issue)`; snapshot redaction (#43) |
| `GET /issues/{issue}/comments` | comment | `resolveIssueID` | `path(issue)` |
| `POST /issues/{issue}/comments` | comment | `resolveIssueID` | `path(issue)`; `parentId` lookup takes the target (#23) |
| `PATCH /comments/{comment}` | comment | comment's issue or document | `path(comment)`: `GetCommentForUpdate` joins to the issue or document and the scope; not the author is still `403` only after the comment is visible |
| `DELETE /comments/{comment}` | comment | comment's issue or document | `path(comment)`; the cascade only touches replies of the same issue |
| `POST /attachments/prepare` | upload ticket | none | `none(no issue yet)`: a presigned PUT under `org/{org_id}/...`; it names no tenant row. The `uploadKey` is bound to an issue only at attach time (`path(issue)`) |
| `GET /issues/{issue}/attachments` | attachment | `resolveIssueID` | `path(issue)` |
| `POST /issues/{issue}/attachments` | attachment | `resolveIssueID` | `path(issue)` |
| `GET /attachments/{attachment}` | attachment | attachment's issue | `path(attachment)`: `GetAttachment` joins issue and scope |
| `GET /attachments/{attachment}/download` | attachment | attachment's issue | `path(attachment)`: check before presigning. A presigned URL already handed out lives its TTL (`downloadTTL`); that is today's behaviour and it names no team |
| `GET /issues/{issue}/relations` | relation | `resolveIssueID` | `path(issue)`; other side visible only (#41) |
| `POST /issues/{issue}/relations` | relation | both | `path(issue)` and `body(issue)` |
| `DELETE /issues/{issue}/relations/{relation}` | relation | `resolveIssueID` | `path(issue)`; the edge must touch this issue; the other side must be visible |
| `GET /users` | member | none | `none(roster, #7)`; `teamAccess`, `teams` for admins only (#33) |
| `GET /me/notification-settings` | settings | own | `own`: lists visible teams only |
| `PUT /me/notification-settings` | settings | own | `own`: no team |
| `PUT /me/notification-settings/teams/{team}` | settings | `resolveTeam` | `path(team)` |
| `PUT`, `DELETE /me/teams/{team}/follow` | follow | `resolveTeam` | `path(team)` |
| `GET`, `PUT`, `DELETE /issues/{issue}/subscription` | subscription | `resolveIssueID` | `path(issue)` |
| `GET /notifications`, `GET /notifications/unread-count`, `GET /notifications/stream`, `POST /notifications/read-all`, `PATCH /notifications/{id}` | inbox | issue of the row | `own` with a filtered join (#62) |
| `POST /agents` | agent | body | `admin`; new fields #31 |
| `GET /signups`, `DELETE /signups/unconfirmed`, `POST .../approve`, `POST .../reject` (4 routes) | sign-up request | none | `admin`; approve body #29 |
| `GET`, `POST`, `DELETE /invites...` (3 routes) | invite | body `teams` | `admin`; fields #26 |
| `GET`, `POST`, `DELETE /tokens...` (3 routes) | token | none | `admin` |
| `/integrations...` (13 routes: list, create, get, check, patch; `rotate-secret` POST, DELETE and `/commit`; `available-repos`; `repos` list, create, PATCH, DELETE) | git config | repo map `team_id` | `admin`; `team_id` of a repo map is a team of the org, not a viewer rule; it limits the webhook (#65, point M). The legacy GitLab project-map routes are gone from the router |
| `/import/...` (11 routes; tables `import_sessions`, `import_jobs`, `import_mappings`) | import | none | `admin` (every route, GETs included). The job runs as `system` and creates teams (#67) |
| `GET /events` | event | `events.scope`, `team_ids` | `feed`: #55 |
| `GET /events/stream` | event | same | `feed`: #55, #56 |
| New: `PUT /users/{user}/team-access` | member | none | `admin`: #48 |

Routes outside `/orgs/{org}`: `/healthz`, `/openapi.yaml`, `/auth/*`, `/setup`,
`/signups`, `/signups/confirm`, `/invites/accept`, `/notifications/unsubscribe/{token}`,
the three webhooks, `/me`, and `/platform/*` read or write no team data of a
caller. They are `none(reason)` in Test A's separate table of public and
install-level routes. `POST /invites/accept` is the one that creates access (#27).
Two exceptions (LAUN-217 review): `GET /orgs` lists the caller's orgs with each
default team's key (`ListOrgsForUser`), so it is `own` with ticket 3, which hides a
hidden default team's key; and the webhooks answer with the linked and moved
identifiers until ticket 8 cuts the body to a status (#65).

### sqlc queries and hand-written SQL on team-owned tables

| Where | Query | Tag | Notes |
|---|---|---|---|
| `issue_read.go` | `ListIssues` (hand-written) | `team` | the predicate in `where`; project filter uses `projectScope`; parent join uses the predicate |
| `issue_read.go` | `getIssueByID`, `issueSelectFull` | `team` | `WHERE i.id = $2 AND scope` |
| `issues.sql` | `ResolveIssueByNumber`, `IssueExists`, `ResolveImportedID` | `team` | the resolvers of #14 |
| `issues.sql` | `ProjectExists`, `MilestoneExists`, `CycleExists` | `project` / `team` | planning refs |
| `issues.sql` | `InsertIssue`, `LockIssue`, `UpdateIssue`, `ColumnTopRank`, `GetIssueRankRow`, `ColumnSuccessor`, `ColumnPredecessor`, `RebalanceColumn` | `resolved` | after `resolveIssueID` / `resolveTeam` in the same `tx` |
| `issues.sql` | `InsertStateChange`, `ListStateChanges`, `ListIssueRefs` | `resolved` / `team` | `ListIssueRefs` (used by `issueRefs` and `snapIssue`) becomes `team` |
| `comments.sql` | `ListIssueComments`, `InsertComment`, `GetComment`, `GetCommentForUpdate`, `UpdateCommentBody`, `DeleteComment` | `resolved` / `team` | `GetComment*` by id become `team` (join to the issue or document) |
| `attachments.sql` | `InsertAttachment`, `ListIssueAttachments` | `resolved` | after the issue |
| `attachments.sql` | `GetAttachment` | `team` | by attachment id |
| `relations.sql` | `InsertRelation`, `RelationExists`, `DeleteRelation` | `resolved` | both issues resolved |
| `relations.sql` | `ListRelationsForIssue` | `team` | filters the other side |
| `activity.sql` | `InsertIssueActivity` | `resolved` | |
| `activity.sql` | `ListIssueActivity` | `resolved` + service redaction (#43) | the issue is resolved; the snapshot lookups are `team` |
| `planning.sql` | `InsertProject`, `GetProject`, `LockProject`, `UpdateProject`, `TouchProject`, `ListTeamsForProjects` | `project` | `GetProject` and `LockProject` add `projectScope`; `ListTeamsForProjects` returns visible teams only |
| `planning.sql` | `InsertProjectTeam`, `DeleteProjectTeam` | `resolved` | `replaceProjectTeams` keeps hidden rows (#39) |
| `planning.sql` | `InsertMilestone`, `GetMilestone`, `LockMilestone`, `UpdateMilestone`, `GetMilestoneProjectID` | `project` | |
| `planning.sql` | `InsertDocument`, `GetDocument`, `GetDocumentBySlug`, `LockDocument`, `UpdateDocument`, `ListDocumentComments`, `InsertDocumentComment` | `project` | document or null project |
| `planning.sql` | `GetCycle`, `GetCycleTeamID`, `UpdateCycleText`, `ListCycleSnapshots`, `UpsertCycleSnapshot`, `ListCycleTeams` | `team` / `system` | route reads `team`; the job's calls are `system` |
| `planning.sql` | `LastCycle`, `InsertCycle`, `ListEndedOpenCycles`, `NextCycleAfter`, `CurrentCycle`, `CompleteCycle`, `RolloverIssues` | `system` | the cycle job |
| `projects.go` | `ListProjects` (hand-written) | `project` | |
| `documents.go` | `ListDocuments` (hand-written) | `project` | |
| `cycles.go` | `ListCycles`, `GetCycle` select (hand-written) | `team` | |
| `teams.sql` | `ListTeams`, `GetTeamByKey`, `GetTeamByID` | `team` | `GetTeamBy*` are also used by `system` code; the system callers use a `system` copy (`GetTeamByIDSystem`) so the tagged query cannot be misused |
| `teams.sql` | `ListTeamStates`, `GetStateByID`, `GetStateByTeamAndName`, `FirstStateOfType` | `resolved` | after `resolveTeam` |
| `teams.sql` | `InsertTeam`, `InsertIssueCounter`, `NextIssueNumber`, `InsertWorkflowState`, `SetTeamAnchorStates`, `LockTeam`, `UpdateTeamSettings` | `admin` / `resolved` | |
| `notifications.sql` | `ListInbox`, `GetInboxItem`, `CountUnreadInbox`, `SetInboxItemState`, `MarkInboxReadBefore`, `ListInboxStreamRows`, `MaxInboxSeq` | `own` | join `issues` + predicate (#62); `MaxInboxSeq` is a cursor start and is left as is |
| `notifications.sql` | `ListNotifyEligibleUsers`, `ListMentionableUsers`, `ListTeamFollowers`, `GetNotifyIssue`, `ListIssueSubscriptions`, `ListIssueCommenterIDs` | `system` | the eligibility filter of #61 |
| `notifications.sql` | `GetMailIssue`, `GetMailRecipient`, `ListDueNotificationEmails` | `system` | recheck at send (#63) |
| `notifications.sql` | `GetIssueSubscription`, `UpsertIssueSubscription`, `DeleteIssueSubscription`, `GetIssueSubscriptionFacts`, `InsertTeamFollow`, `DeleteTeamFollow`, `ListTeamFollows`, `UpsertTeamNotificationSetting` | `resolved` | after `resolveIssueID` / `resolveTeam`; `ListTeamFollows` and `ListNotificationSettings` return visible teams only |
| `events.sql` | `ListEventsAfter`, `LatestEventSeq` | `feed` (`team`) | `ListEventsAfter` gets the predicate; a `ListEventsAfterSystem` serves the consumer |
| `events.sql` | `InsertEvent`, `NextEventSeq`, `NotifyEvents` | `system` | `Emit` takes the scope (#53) |
| `orgs.sql` | `GetMembership`, `InsertOrgMember`, `ListOrgMembers` | `org` | `GetMembership` also returns `team_access`; `ListOrgMembers` joins team rows for admins |
| `orgs.sql` | `GetOrgFull` (`GET /orgs/{org}`), `ListOrgsForUser` (`GET /orgs`) | `GetOrgFull` `team`; `ListOrgsForUser` `own` (ticket 3) | both join `teams` for the default team's key, so a hidden default team gives `null` (#9). `GetOrgFull` takes the #15 predicate; `ListOrgsForUser` reads each membership's access in SQL (one org per row, so no single pair of arguments; ticket 3 build notes). The login and session bodies read `ListOrgsForUser` but drop the key |
| `signups.sql` | `InsertOrgMemberRole`, `InsertOrgInvite`, `ListOrgInvites`, `ConsumeOrgInvite` | `admin` / `system` | new team columns (#26, #27) |
| `gitlab.sql`, `git.sql` | webhook lookups, `mr_links`, repo map | `system` | #65 |
| `importer.sql`, `importjob.sql` | all | `system` | #67 |
| `labels.sql` | `ListLabels`, `InsertLabel`, `FindLabels`, `ListLabelsForIssues`, `InsertIssueLabel`, `DeleteIssueLabel` | `org` | `ListLabelsForIssues` is by issue ids already resolved |
| `tokens.sql`, `login.sql`, `platform.sql`, `setup.sql`, `idempotency.sql` | all | `org` / `admin` / `system` | no tenant team data |
| future (T18) | products, `product_teams`, the `product` filter | `project` rule | #69 |
| `labels.sql` (LAUN-54) | `ListLabelsWithCounts` (takes `visible_team_ids`), team labels | `team` | #8; decisions-T21-team-labels #8 |

### Feeds and side channels

| Channel | What it carries | Rule |
|---|---|---|
| `GET /events` | all event payloads | `feed` (#55) |
| `GET /events/stream` | same, live | `feed` (#55, #56) |
| `LISTEN/NOTIFY` `devops_events` | the org id only | no change: it carries no data |
| inbox stream `GET /notifications/stream` | row ids, kinds, `inbox_seq` | `own`, filtered (#62) |
| Email (notification mail) | issue title, team key, state | recheck at send (#63) |
| Webhook responses to GitLab/GitHub | `status` only (#65) | none |
| MCP tool results | REST bodies | inherited |
| Download redirects (presigned GET) | the object | `path(attachment)` before presign |
| Error messages and `Location` headers | texts | #23 |
| Idempotency replay | the caller's own earlier body | #25 |
| Logs | audit lines | no hidden title or id in a log line for a refused read |
| `issue_activity` snapshots | titles, names | redacted at read (#43) |

## Security analysis

- **404, not 403.** A `403` says "it exists and is not yours". Every refusal on
  team grounds is the answer for a row that does not exist (#10).
- **Ids.** A hidden id is never accepted as proof. Ids are UUIDv7: they carry a
  timestamp and random bits, and they are not secrets. The design does not rely
  on them being hard to guess; it relies on the predicate.
  Identifiers (`ORB-12`) are sequential per team, so they are guessable; the
  predicate answers `404` for a hidden one exactly as for a number above the
  counter.
- **Counts.** Nothing returns a count that includes hidden rows: no project
  aggregate (none today, #37), the unread count is filtered (#62), the product
  and project team lists show visible teams, `nextCursor` is honest (#24). The label count (`issueCount`, which LAUN-54 adds
  to `GET /labels`) is on an org-wide list, so it counts only issues of teams the
  caller sees. That query is tagged `team`, and Test B has a case with a hidden
  issue that carries the label. The
  one residual is the `seq` gap in the feed (#58).
- **Errors.** The refusals listed in #23 changed so a hidden row and a missing
  row give the same text, code and shape. Test B compares the response bytes with
  those for a made-up id (status, body, and the headers that matter: content
  type, no `Location`).
- **Timing.** Because the predicate is part of the lookup (#11), a hidden id and
  a missing id take the same code path and the same queries. The statement says
  nothing about a measured difference: the note does not claim equal times, only
  equal paths. A timing test is not written (it would be flaky); the path test is.
  The existing code already differs by id form (identifier, UUID, Linear id) and
  that is not about hidden rows.
- **Agents and PATs.** The visible set comes from the owner (#13, #32). A PAT
  minted by an admin for a restricted member sees what the member sees. A PAT of
  an org admin sees all. `RequireScope(admin)` demands the org role, so an admin
  token cannot be held by a restricted member. An agent with `selected` is a
  normal restricted member; its token needs an expiry as today.
- **Sessions of a platform admin.** They get the admin scope but not the org
  admin role, so they are restricted by the same rule as any member of that org
  (a platform admin who is not an org admin and has `selected` sees those teams).
  The `/platform` routes read no tenant rows. A platform admin can grant
  themselves nothing via the API, but has database access; that is out of scope.
- **A restricted member who writes where they can see.** They can write to issues
  of their teams with a `write` token, including setting `projectId` to a
  project they can see. They cannot attach an issue to a hidden project (#40).
- **Side channels left.** The `seq` gap and the event volume it shows (#58). A
  webhook write to a team the repo map does not name, if point M is answered (a). The `retry`/heartbeat timing of
  SSE reveals nothing. A presigned download URL stays valid for its TTL after
  access is removed (existing; the URL is for one object and expires). Idempotency
  replay (#25). Backup files hold all teams (outside the app).
- **What goes wrong if one query is missed.** That query leaks rows of hidden
  teams. Tests A to D exist to make that not merge. Test B tests behaviour and
  catches a query that has a tag but the wrong predicate.

## Tests

Every build ticket keeps these, and adds its own cross-team case. This matches the
convention "a cross-org test for every new resource" with one more axis.

- **Test A, B, C, D** of #18 and #53.
- **A cross-team test per resource,** in the style of the cross-org tests: for
  issue, comment, relation, attachment, activity, state history, cycle, snapshot,
  milestone, project, document, document comment, label (org-wide, asserts it is
  visible), notification, inbox, subscription, follow, event, SSE stream, MCP
  tool, token. Each: list excludes hidden; by-id is `404`; write is `404`/`400` and
  changes nothing; the same call as an unrestricted member and as an admin works.
- **Cross-team edges:** a project in A and B seen from A only (teams list, issues,
  milestones); an issue in A with a parent, a relation and a project in B; a
  comment that mentions a hidden issue; a mention of a user without access; a
  subscriber who loses access (no notification, no subscription row, inbox row
  hidden, unread count down); a member whose last team is removed with assigned
  issues (`409` without a choice, unassigned with `unassign`); a relation removed
  by an admin whose snapshot names a hidden issue (redacted for the restricted
  viewer); an event feed read by a restricted member (no hidden event, gap
  allowed); an SSE stream whose owner loses a team mid-stream (an `access` event before
  the next drain, also when the drain is woken by NOTIFY and not by the poll; no
  later hidden event), and the same for the inbox stream.
- **Migrations:** up, down, up for M1, M2, M2b and M3. The M2b down rewrites rows
  with `access_removed` to `disabled`: a test inserts one and checks it. The M3 down
  fails with an open `selected` invite and passes after the invite is revoked; the backfill job (a legacy row placed;
  one it cannot place stays `legacy` and is hidden from a restricted member).
- **Idempotency:** a replay after removal returns the earlier body only (#25).
- **No regression:** the whole suite runs once with every seeded member `all`
  (today's behaviour) and Test B runs for restricted members.
- **Performance check, not a unit test:** `EXPLAIN (ANALYZE, BUFFERS)` of the
  issue list, the board and the inbox with a 1-team and a 5-team restricted
  member on imported data (ticket 3 records the numbers in the note).

## Performance and indexes

- Unrestricted members and admins run the same SQL text with `all_teams` true (#12,
  #15). The `OR` with a bound boolean may change the plan, above all when Postgres
  reuses a generic plan. Ticket 3 compares `EXPLAIN` of the list for an all-teams
  caller before and after, and if the plan is worse it runs these queries with
  custom plans (`plan_cache_mode = force_custom_plan`) so the true boolean folds
  away. Measured in ticket 3 on synthetic data: see "Build notes (LAUN-218,
  ticket 3)", "EXPLAIN". The pgx statement cache chose custom plans every time,
  so no setting was added.
- A restricted member adds `i.team_id = ANY($ids)`. The existing indexes
  `issues_org_updated_idx (org_id, updated_at, id)`, `issues_team_state_idx
  (team_id, state_id)` and `issues_manual_order_idx (org_id, team_id, state_id,
  sort_order, id)` can serve it. Which plan Postgres picks for the default
  `updatedAt` order with a team filter was not measured (Not measured). If the
  planner scans `issues_org_updated_idx` and filters, a restricted member with a
  small team on a large org pays a longer scan; the answer then is an index
  `(org_id, team_id, updated_at, id)`, added by a measured follow-up, not by this
  design.
- `team_members` has its primary key `(org_id, user_id, team_id)` (the per
  request read) and `(org_id, team_id)` (the "who is in this team" read and the
  cascade).
- The request cost is one extra primary-key read for a restricted member (#13).
- The events backfill job (#57) is one `UPDATE` per entity kind; it runs in batches by
  `seq` range if the table is large (size not measured).
- `project_teams` already has `(org_id, project_id)` and `(org_id, team_id)`
  indexes (`project_teams_org_project_idx`, `project_teams_team_idx`), which the
  project predicate uses.
- **The feed (#55), measured in 6a.** `ListEventsAfter` walks `events_pkey
  (org_id, seq)` from `after` and filters each row; there is no index on
  `scope` or `team_ids`, and none is needed while the cursor moves past the
  rows the reader may not read (the high-water mark, build notes 6a). A
  throwaway database on Postgres 16 with 200 000 events of one org (95 % team
  B, 5 % team A, 1 % org), `EXPLAIN (ANALYZE, BUFFERS)` of the prepared query:
  a member of A from seq 0, `limit 100`: 2 000 rows read, 1 900 removed by
  the filter, 54 buffers, 0.3 ms. A member of neither team from seq 0,
  `limit 500` (only org rows visible): 50 000 rows read, 49 500 removed,
  1 271 buffers, 6.8 ms. The same member 10 rows below the high-water mark:
  10 rows, 0.03 ms. An all-teams member: no row removed, 0.03 ms. So the
  cost is the hidden rows between the cursor and the next visible one, paid
  once per reader: the first read from seq 0 after the upgrade (legacy rows
  are hidden from a restricted member) scans the history once. A full scan of
  a large history with nothing visible was not measured; by the numbers above
  it is about 0.14 ms per 1 000 rows.

## Web

The web needs little for enforcement because every team list is `GET /teams`
(`useTeams`). Once the server filters it, the sidebar, create dialog, palette,
cycle jump, notification settings and the team page's own not-found test follow.
What changes:

71. **Sidebar and pickers** show visible teams only. No code change; a test with a
    restricted session asserts it.
72. **A hidden issue, team or cycle by direct link** is the existing not-found page
    (the server answers `404`, the same as a missing one). The page shows no
    difference. Tests: issue detail, team page, cycle page, document, project.
73. **Empty state for a member with no team.** `IssuesPage` has "This organisation
    has no team yet" for an org with no team (`noTeams`). A restricted member with
    zero teams needs another text: "You do not have access to any team yet. Ask an
    organisation admin." (the member cannot tell an empty org from no access, and
    that is fine). The create-issue dialog is disabled with the same text.
74. **Members page** (`web/src/features/settings/MembersPage.tsx`) gets a "Teams" column for admins ("All
    teams" or a list of keys) and an "Edit access" action: a dialog with a radio
    ("All teams" / "Selected teams"), a checklist of teams (with a "select all
    teams of <product>" shortcut once products exist, #70), and, when it would
    leave assigned issues, the count and the choice from #49 with "Unassign" as the
    default. It uses `dryRun` to show the counts first. An admin row shows "All
    teams" and no action (#2).
75. **Invite dialog** (`web/src/features/settings/InvitesSection.tsx`): a "Team access" control under Role:
    "All teams" (preselected) or "Selected teams" with the checklist; hidden for
    role Admin. The invites table shows the access. The agent create form
    (`web/src/features/settings/AgentsPage.tsx`) and the sign-up approve action (`web/src/features/settings/SignupsPage.tsx`) get the
    same control.
76. **The `access` SSE event** (#56): add it to the event list in `web/src/api/events.ts`
    and invalidate all queries; the web then shows the new lists without a reload.
77. **No "request access" button.** It would confirm that a hidden team exists.
78. **MCP page and token forms** need no change; text on the Tokens page says a
    token has the same team access as its owner.

## Not done, on purpose

- **Slack channel and chat services (LAUN-198, `decisions-T33-slack-channel.md`;
  now chat services, T36 to T38).** Restated by LAUN-231: the warning shipped,
  the stricter filter was not built. Each service belongs to one team (T38) and
  posts the events its admin ticks for that team. Team access does not apply to
  a channel, and Launchpad cannot see who is in it. So a person in the channel
  without access to the team sees each post: a fixed sentence of what happened,
  the issue identifier (none for a project reminder), a link (it holds the org
  slug and the issue identifier, or the project id), and for a state change the
  names of the old and new state (T39). A post holds no title, no comment text,
  no person's name and no project name; the link needs a login with access.
  The project owner chose option 1 (2026-11-02): the chat services dialog warns the admin
  on every add and edit, always, with the team key; the OpenAPI text of create
  and update and `docs/architecture.md` say the same. LAUN-231 also closed a
  cross-team path: a change of a service's team or webhook URL deletes its
  waiting posts, and the sender skips (`not_selected`) a post whose issue or
  project is not of the service's current team (T38 #11 is superseded). NOT
  built: a stricter per-service filter (for example "post only for teams with
  no restricted members"). Open it as a new ticket if it is wanted.
- **Chat mention post (residual, predates LAUN-231).** A "Mention on an issue"
  post goes out whenever an active human member is newly mentioned, also a
  member who cannot see the team (the consumer's chat check uses
  `ListActiveHumanMembers`, with no team test). No name is posted, so the channel learns only that someone
  was mentioned on that issue.

- Per-team roles (team admin, read-only member), private teams that even admins
  cannot see, team-level invites by team admins.
- Token-level narrowing (#32, open point I).
- A request-access flow (#77).
- A dense per-viewer feed number (#58).
- Postgres row-level security (#20).
- Re-check on idempotency replay (#25).
- A mention warning in the composer ("this person cannot see this issue").
- A team-aware roster (open point C).
- Changing the rule that an issue may sit in a project that lacks its team (#40).

## Not measured (honest list)

- The plan and time of the issue list, the board and the inbox with the team
  predicate on real imported data. Ticket 3 measured the list, the board and
  the manual order on synthetic data only (its build notes); the inbox is not
  measured.
- How long the events backfill job takes and how many rows end as `legacy`.
- How many teams production has now and how many members would be restricted first.
  The design works for few teams; it was not sized.
- How the web behaves when the `access` event arrives while a form is open.
- That the web shows the not-found page for every hidden link kind: reasoned from
  `IssuesPage.tsx` and `useTeams`, not run.
- Linear's own behaviour when a person leaves a team (assigned issues): not
  checked, so #49 is reasoned, not copied.
- Test B's coverage of body references in the MCP and web write paths is as good as
  its sample functions; a missing sample is a hole the test cannot see.

## Open points (each with a recommended default)

A. **The default access of a new invite, agent or approval.** Options: (1) the
   API default is `all` when the field is absent, and the web dialogs preselect
   "All teams"; (2) the API default is `selected` with no teams (nothing); (3) the
   web forces a choice. Default: (1). It keeps old scripts, the importer setup and
   `--smoke` working, and costs one click to narrow.
B. **A restricted member with zero teams.** Options: allow it (an edit can take the
   last team away; an invite needs one team), or never allow it. Default: allow
   on edit, refuse on invite and approval (#5).
C. **What a restricted member sees of the roster.** Options: all names (today);
   only people who share a team plus authors and assignees of visible things.
   Default: all names (#7). The second option needs a second rule and breaks
   mention pickers.
D. **A hidden parent or related issue.** Options: omit it (default); show a
   placeholder "an issue you cannot see" (it reveals that an edge exists).
E. **Assigned issues when access is removed.** Options: require a choice, with
   "unassign open issues" as the web default (recommended); always unassign; always
   keep; block the removal. Default: require a choice (#49).
F. **Projects across teams.** Options: visible when any team is visible; a project
   with no team is org-wide; a restricted member must name a team when creating one
   (default, #36, #39). A restricted member cannot edit the teams of a project that has none: admin only (#39). Alternatives: org-wide projects are admin-only; or an issue
   may join a project only if its team is in the project's team list (this changes
   an existing rule, #40).
G. **Documents with no project.** Options: org-wide (default, #38); or visible
   only to the creator's teams (needs a new column); or refuse to create one without
   a project for a restricted member.
H. **Labels.** Org-wide (default, #8). When LAUN-54 adds team labels, those follow
   the team rule.
I. **A token that sees fewer teams than its owner.** Default: not in v1 (#32). The
   use is a CI token for one team; it is a small follow-up that adds a team list to
   `api_tokens` and intersects it with the owner's set.
J. **The `seq` gap in the feed.** Default: accept it (#58). The alternative is a
   per-viewer dense number; not recommended.
K. **Who may change team access.** Default: org admins only (#48). Alternative: a
   team lead can add members to their team; not in v1.
L. **Assigning to a person who cannot see the team.** If assignment failed for
   such a person, success versus failure would tell any restricted member with
   `write` who else is restricted, whatever the error text says. Options:
   (a) refuse with a generic text and accept the probe for teams the caller sees;
   (b) refuse with a clear text; (c) allow it and rely on the notification filter.
   Default: (c) (#46). The cost: an issue can name an assignee who cannot see it.
   The person who assigns is not told. The assignee never sees the issue. The
   setter's `dryRun` and the admin screen count such issues. Project leads follow
   the same rule.
M. **What a webhook may write.** The webhooks match an issue by team key and number
   across all teams. A person who can push a branch to a mapped repo can link and
   move an issue of a team they cannot see. Options: (a) accept it and list it as a
   residual; (b) match only the repo map's `team_id`, and let a map row with
   `team_id` NULL match all teams; (c) as (b), plus the one guard of #65: no restricted
   access while an enabled map row has `team_id` NULL. Default: (c). The cost: a repo
   serves one team only (the table has one `team_id` and one row per repo). A repo
   shared by two teams needs a second integration or has no map row (then the webhook links nothing).
   Until every
   enabled repo has a team, the server refuses to restrict anyone. Option (a) leaves
   the write open to any pusher.

## Ticket list, in dependency order

Each ticket is filed after the two reviewers pass this note, in team LAUN. The
order keeps the rule of #19: nothing can create a restricted member until every
route is enforced. Every ticket adds a cross-org test where it adds a route or a query, and keeps `make docker-test`, `docker-test-integration`,
`sqlc-check`, `schema-check` green, updates `api/openapi.yaml` where it changes the
API, and adds its cross-team tests. Each fits one MR; tickets 3 and 5 are the
largest and may be split at the line given.

**1. Schema M1 and access resolution (no behaviour change).**
- Scope: migration M1 (#34); `auth.TeamAccess` and `Org.Access`; `GetMembership`
  returns `team_access`; `ListMemberTeamIDs`; `RequireOrgMember` fills it (one
  extra read only for `selected`); `testdb` helper to make a restricted member
  and to put a member in teams (direct inserts); `StillAuthorized` returns the
  access.
- AC: `TestMigrateUpDownUp`; existing members are `all` after up; an admin is
  `All` whatever the flag; a `selected` member with two rows has those two ids,
  sorted; a deleted team removes its rows (cascade); a deleted member removes its
  rows; the whole suite is unchanged; `GET /orgs/{org}/users` is unchanged.

**2a. Enforcement framework, part 1: scope helpers and resolver signatures.** (Large: it touches every resolver in `tracker` and `notify`. Split it by package if it passes one review.)
- Scope: `teamScope` / `projectScope` helpers (#15); every resolver and scoped
  read in `tracker` and `notify` takes the `*auth.Org` (#14); `System` copies for
  code with no caller; per-entity scoped `ResolveImportedID` variants; `resolveState`
  and `GetStateByID` scoped (#23). The scope passes for `all` members and is tested
  in unit form for a restricted `TeamAccess`.
- AC: the code does not compile without the org in any resolver call; no
  behaviour change for `all` members; the whole suite is green; a unit test per
  helper with a restricted set.

**2b. Enforcement framework, part 2: tags, rule table, tests A to C, the guard.** (Large: it tags every query.)
- Scope: scope tags on every query and SQL string (#16) with the golden opt-out
  file; `internal/api/teamrules.go` with every route and `Enforced: false`; Tests
  A and C; the Test B skeleton for rules already `Enforced`;
  `TestNoSelectedWriterWhileUnenforced`, derived from `api/openapi.yaml` (#19).
- AC: Test A fails when a route is added without a rule (shown by a test that adds
  one in a throwaway router); Test C fails on an untagged query (shown the same
  way) and on a new opt-out not in the golden file; every existing query carries a
  tag; no behaviour change for `all` members; `make sqlc-check schema-check` clean.

**3. Teams, states and issues (the core reads).**
- Scope: `GET /teams`, `GET /teams/{team}`, `/states`; `ListIssues` with every
  filter, `query` search and Linear-id paths; parent redaction (#42);
  `projectId` / `milestoneId` redaction (#40); the issue body refs that do not
  use planning objects (parent, assignee, labels, `placeAfter` / `placeBefore`)
  and error texts (#23). Flips `Enforced` for the teams routes and `GET /issues`.
  `GET /issues/{issue}`, `POST /issues` and `PATCH /issues` are flipped later:
  `GET` in ticket 4 (its `include=relations,attachments,stateHistory,comments`),
  `POST` and `PATCH` in ticket 5 (their `projectId`, `milestoneId` and `cycleId`
  use `checkPlanningRefs`).
- AC: Test B passes for these rules; `?team=` hidden is an empty list identical to
  an unknown key; `?cycle=` / `?parent=` hidden is the same `400` as unknown; a
  page of 50 is full when 50 visible rows exist (no short page); an issue with a
  hidden parent has `parent: null`; an assignee without access is accepted (#46)
  and gets no notification; `TestHiddenAndMissingSamePath` asserts that a hidden
  and a missing issue id give identical status, body and headers and run the same
  queries (counted with a pgx tracer); an `EXPLAIN` of the list, the board and the
  manual order with 1 and 5 teams is recorded in this note (replacing "not
  measured"); no change for `all` members.
- (Split here if large: 3a teams and reads, 3b writes and refs.)

**4. Issue children: comments, activity, state history, attachments, relations,
merge-request links.**
- Scope: flips `GET /issues/{issue}` (core read and its includes); `ListComments`, `CreateComment`, `PATCH` / `DELETE /comments/{id}`,
  `ListActivity` with snapshot redaction (#43), state history in `include`,
  attachment routes and `DownloadURL` (check before presign), relations read and
  write (#41), `parentId` lookup (#23).
- AC: Test B for these rules; a comment on a hidden issue by id is `404` for
  read, edit and delete; the download of a hidden attachment is `404` and no
  presign is made (a store spy sees no call); a relation to a hidden issue is
  absent from both issues' views and cannot be created or deleted by a restricted
  member; an activity row naming a hidden issue returns `null` snapshots to a
  restricted viewer and the stored row is unchanged; the admin sees it whole.

**5. Planning objects: projects, milestones, documents, cycles.**
- Scope: flips `POST` and `PATCH /issues` (planning refs); `ListProjects`, `GetProject`, `UpdateProject` (teams replace keeps hidden
  rows, #39), `CreateProject` rule, `teamKeys` filtering, milestones, documents
  and their comments (#38), cycles and snapshots, `checkPlanningRefs` and the
  `project=` / `cycle=` filters of the issue list (#40), the lead rule (#46).
- AC: Test B for these rules; the project lead rule of #46 (any member may lead); a project in A and B read from A shows only A; a
  restricted member cannot name a hidden team, cannot remove their last team from
  a project, and must name a team to create one; hidden team rows survive an edit;
  a document of a hidden project is `404`; a document with no project is visible;
  `CycleSnapshots` does no write for a hidden cycle (a row-count check); no
  aggregate counts hidden issues (test for any existing counter).
- (Split here if large: 5a projects and milestones, 5b documents and cycles.)

**6a. The change feed: scope on every event, filtered feed and SSE.**
- Scope: migration M2 (columns only); `events.Emit` takes a `Scope` (#53) and the 21
  call sites name one (#54); `ListEventsAfter` predicate and `ListEventsAfterSystem`;
  `streamEvents` re-reads access at every drain and sends `access` (#56); Test D; the
  consumer switches to the system query.
- AC: Test D; a restricted member's `GET /events` and stream carry only visible and
  org-scoped events; a relation event between A and B is hidden from an A-only
  member; `legacy` rows hide from restricted members and show to unrestricted ones;
  an access change mid-stream sends one `access` event before the next drain, also
  on a NOTIFY wake-up; a missing membership row ends the stream; the consumer still
  sees every event; `TestMigrateUpDownUp`.

**6b. The events backfill job.**
- Scope: the post-boot job of #57 in `cmd/server`.
- AC: the job places rows of every entity kind (one test each) and leaves an orphan
  as `legacy`; before it runs, every old row is hidden from a restricted member; two
  instances run it once (advisory lock); a second run changes nothing; it runs again
  at boot while placeable `legacy` rows exist.

**7. Notifications and mail.**
- Scope: consumer eligibility with access (#61); mention rule (#45); inbox queries,
  unread count, stream, read-all, patch (#62); mail sender recheck and the
  `access_removed` skip reason with migration M2b (#63); `/me` team and issue routes resolve scoped
  (#64); the cleanup helper used by removal (#50).
- AC: a user without access gets no notification of any kind (assigned, comment,
  state change, mention, new issue) and no mention subscription; the inbox, the
  unread count and the stream exclude rows of a hidden team and agree with each
  other; a mail is skipped when access is gone at send time and holds no hidden
  title; follow and settings routes on a hidden team are `404`; agents still get
  none.

**8. MCP, webhooks, import and docs of the enforced surface.**
- Scope: MCP integration test with a restricted token across all 19 tools and the
  reviewer profile; GitLab and GitHub event tests (a hidden issue is moved by the
  webhook and the event is scoped, #65); the webhook response body becomes `{"status": ...}` only, in GitLab, GitHub, `api/openapi.yaml` and their tests; the match uses the repo map's `team_id`, and the webhook guard of #65 (the helper `CheckWebhookGuard`, integration enable, repo-map create and patch, team delete rule); the one-line "superseded by T20 #65" pointers in `decisions-T5.md` and `decisions-T17-git-connections-import.md`; `linked` / `moved` leave `WebhookResult`, `required: [status]` in `api/openapi.yaml`, and the tests that read them change; importer and import-job tests (#67);
  `docs/architecture.md`, `docs/mcp.md`; flip the last `Enforced` flags.
- AC: every tool returns only visible data, and an unknown or hidden team is the
  same error text; the golden files are unchanged; the webhook still moves and
  links an issue of the repo map's team, even a team no member can see; a repo with
  `team_id` NULL is refused (`409`) while restricted access exists, and enabling an
  integration with such a repo is refused too (a bypass test for entries 5 to 7 of
  #65); the response body is
  byte-identical for a hidden, a visible and a missing issue; the importer run into an org with a
  restricted member creates teams that member does not see; the webhook response
  body is `{"status": ...}` and nothing else (a test on GitLab and GitHub);
  a mapped repo with a `team_id` never links or moves an issue of another team (#65,
  point M); Test B has no `Enforced: false` left.

**9. Members: the setter and removal effects.** Depends on ticket 8.
- Scope: `PUT /users/{user}/team-access` with `dryRun`, `assignedIssues`, the
  `409` rule (#48, #49); the cleanup of subscriptions, follows, settings (#50);
  `member.access_changed` (admin scope) and the audit line; `Member` fields (#33);
  agents `POST` fields (#31); the setter appears in `api/openapi.yaml`, which arms the guard of #19; the
  rollback runbook of #35 in `docs/deploy.md` (it must exist before the feature is
  usable).
- Depends on ticket 8 (the webhook guard helper). The setter, the agents `POST`
  and the sign-up approval call it (entries 1, 3, 4 of #65).
- AC: only an org admin can call it (`403` otherwise); admin targets are `400`;
  replace is idempotent; a removal with open assigned issues and no choice is `409`
  with per-team counts, `unassign` clears only open issues and writes system
  activity and events, `keep` changes none; subscriptions, follows and per-team
  settings of the lost teams are gone, others stay; the member's next request sees
  the new set; their open stream gets `access`; `TestNoSelectedWriterWhileUnenforced`
  now passes because every rule is enforced; a cross-org test: an admin of another
  org gets `404` for a user of this org; a runbook step revokes open `selected`
  invites before a rollback.

**10. Invites and sign-up approval carry access.**
- Scope: migration M3; `InviteInput`, `CreateInvite`, `ListInvites`, `AcceptInvite`
  (#26 to #30); `Approve` body (#29); openapi.
- Scope addition: the guard helper of ticket 8 starts counting open `selected` invites (the table exists only now).
- AC: accept writes membership and team rows in one transaction (a failure writes
  neither); `selected` needs one or more teams; an admin invite with teams is
  `400`; an existing install-level user joining gets the invite's access; an
  existing member's invite is still `409`; accept with some chosen teams gone skips those; accept with ALL chosen teams gone
  is `409` and the invite stays open (#27); the M3 down fails with an open
  `selected` invite and passes after a revoke;
  approve with no body is `all`; with a `selected` invite, create and accept are
  refused while an enabled map row has `team_id` NULL (entry 2 of #65), including a
  repo that lost its team between create and accept, and a pending `selected`
  invite blocks creating an unmapped repo (bypass tests); the invite view lists team keys; secrets and
  idempotency rules of T12 hold (`TestSignups_NoSecretsInLogsEventsOrIdempotency`).

**11a. Web: members page and invite dialog.**
- Scope: #74 and #75 for `web/src/features/settings/MembersPage.tsx` and
  `web/src/features/settings/InvitesSection.tsx`; the generated client types;
  vitest tests with an admin session.
- AC: the Members page shows each member's access and edits it with the dry-run
  counts and the assigned-issue choice; the invite dialog sends `teamAccess` and
  `teams`, hides them for role Admin; the invites table shows the access.

**11b. Web: the rest.**
- Scope: #71 to #73 and #76 to #78: the agent form
  (`web/src/features/settings/AgentsPage.tsx`) and the sign-up approval
  (`web/src/features/settings/SignupsPage.tsx`) get the control; the no-access empty
  state (`web/src/features/issues/IssuesPage.tsx`, `CreateIssueDialog.tsx`); the
  `access` event in `web/src/api/events.ts`; AGENTS.md section.
- AC: the sidebar (`web/src/features/shell/Shell.tsx`), create dialog and palette
  list only visible teams; a hidden issue, team, cycle, project and document link
  shows the not-found page; a member with no team sees the no-access text and a
  disabled create button; an `access` event refreshes the lists; no "request
  access" control exists.

## Build notes (LAUN-216, ticket 2a)

What ticket 2a built, and the choices the note left open. No numbered rule
changed. Every choice takes the recommended default.

- **Helpers.** `internal/tracker/scope.go` holds `teamScope(org, col, arg)`,
  `projectScope(org, alias, arg)` and `documentScope(org, alias, arg)` (a
  document with no project is org-level, else it follows the project, #7, #38).
  Each adds the two arguments and always returns text (#15). Each has a unit
  test for an all-teams, a restricted and an empty set.
- **sqlc queries.** A scoped query appends the #15 text with
  `sqlc.arg(all_teams)` and `sqlc.arg(team_ids)`. The params struct gains
  `AllTeams` and `TeamIds`. A struct literal that leaves them out compiles, and
  an empty `AllTeams` is false, so the query finds nothing (it fails closed).
  `TestScopedQueriesNameTheirAccess` (a source scan, `scope_source_test.go`)
  also fails when production code builds one of these structs without
  `AllTeams`. Ticket 2b replaces it with Test C.
- **Scoped now (`team` or `project`):** `ResolveIssueByNumber`, `IssueExists`,
  `ListIssueRefs`, `ProjectExists`, `MilestoneExists`, `CycleExists`,
  `GetProject`, `LockProject`, `GetMilestone`, `LockMilestone`, `GetDocument`,
  `GetDocumentBySlug`, `LockDocument`, `GetCycle`, `GetCycleTeamID`,
  `GetMilestoneProjectID`, `GetTeamByKey`, `GetTeamByID`, `GetStateByID`,
  `GetComment`, `GetCommentForUpdate`, and the six inbox reads of #62
  (`ListInbox`, `GetInboxItem`, `CountUnreadInbox`, `SetInboxItemState`,
  `MarkInboxReadBefore`, `ListInboxStreamRows`; `MaxInboxSeq` stays as the note
  says). A row with a project and no issue (`project_update_due`) is judged by
  `projectScope` on `n.project_id` (#36, #62); a project with no team is
  org-level and passes. An issue row is judged by the issue's team.
- **Also scoped after review:** `GetProjectUpdate` (it checks the project like
  create and list), the project avatar queries (`GetProjectAvatar`,
  `LockProjectAvatar`, `SetProjectAvatar`, `ClearProjectAvatar`: a hidden
  project is the 404 of an unknown one, and no write runs before the check),
  and `ListLabelsWithCounts`, which moved from `visible_team_ids` (NULL = all)
  to `all_teams` / `team_ids`, so a nil set means no team (#15).
- **Call sites** read the access with `org.Access.Args()` and pass the two
  values. `scope_source_test.go` scans `internal/` and `cmd/`: each scoped
  params literal names `AllTeams` and `TeamIds`, and none hard-codes
  `AllTeams: true` (code with no viewer uses the `System` copy).
- **`ResolveImportedID` is gone.** It is replaced by `ResolveImportedIssueID`,
  `ResolveImportedTeamID` and `ResolveImportedCycleID`. Each joins the target
  row and adds the predicate, so the check is in the lookup (#11, #14).
- **`System` copies (unscoped, a visible name):** `GetTeamByKeySystem`,
  `GetTeamByIDSystem`, `GetStateByIDSystem`, `GetCommentSystem`,
  `GetProjectSystem`, `GetMilestoneSystem`, `GetCycleSystem`,
  `ListIssueRefsSystem`. Their callers have no viewer: the importer, the
  unsubscribe route, the cycle job (`maintainTeamCyclesTx`), the GitLab webhook,
  the notification consumer and sender, and team creation (it reads the team it
  just made).
- **Activity snapshots use the `System` copies.** A snapshot is stored whole
  and redacted when read (#43). If the write used the scoped query, a
  restricted writer would store `{"id": ...}` for a hidden project, and the
  admin would lose the name. This follows #43; the inventory line "snapshot
  lookups are `team`" is read as the read-time lookup.
- **Resolver signatures.** These take `*auth.Org` and no longer take a bare
  org id: `resolveIssueID`, `resolveTeam`, `resolveState`, `resolveCycleID`,
  `cycleRef`, `cyclePathID`, `teamFilterID`, `importedID`, `checkPlanningRefs`,
  `resolveDocumentID`, `getDocument`, `getProject`, `placeRank` (so `placeAfter`
  and `placeBefore` resolve scoped, #23), `resolveTeams`, `resolveProductTeams`,
  `resolveRepoTeam`, `guidanceTeam`, `lockCycleTx`, `redateCycleTx`,
  `relationsFor`, `issueRefs`. `GetMilestone` already took the org.
- **Hand-written lists** now add the helper text to their `WHERE`:
  `ListIssues`, `getIssueByID`, `ListProjects`, `getProject`, `ListMilestones`,
  `ListDocuments`, `getDocument`, `ListCycles`, `GetCycle`. For an all-teams
  caller the `OR` is satisfied, so nothing changes. The `EXPLAIN` check stays
  in ticket 3 (Performance section).
- **`relationsFor` skips an edge whose other issue is not visible** (#41). The
  scoped `ListIssueRefs` returns no ref for it. The route test is ticket 4.
- **Notify.** The inbox methods (`ListInbox`, `UnreadCount`, `PatchInbox`,
  `ReadAll`, `StreamRows`, `LatestInboxSeq`) take the `*auth.Org`. The consumer
  and the sender call the `System` copy of `GetComment`; their other reads are
  already `system` queries and do not change here (eligibility with access is
  ticket 7). The settings, follow and subscription methods keep their ids:
  their team or issue comes from a scoped resolver in the route, and
  "visible teams only" in `ListTeamFollows` / `ListNotificationSettings` is
  ticket 7 (#64).
- **Not done here, on purpose:** no route has a rule table or `Enforced` flag
  (2b); no route reads `Org.Access` itself; `ListRelationsForIssue`,
  `ListIssueActivity` snapshot redaction, `ListTeams`, `ListTeamStates`,
  `ListDocumentComments` are unchanged (tickets 3 to 5); nothing can set
  `team_access = 'selected'` (#19).
- **Tests that build an `auth.Org` by hand** now set
  `Access: auth.TeamAccess{All: true}`. An org with a zero `Access` is a
  restricted member with no team, and finds nothing.
- **Tests added:** the three helper unit tests, the source scan,
  `TestScope_RestrictedAccess` (tracker; issue, team, state, cycle, project,
  milestone, document, and the lists, for a restricted, an empty, an all-teams
  and an admin caller), and `TestInbox_Scope` (notify).

### Carried to later tickets

Not scoped in 2a. Each item is named so nothing is lost.

- `GetAttachment` and `DownloadURL` (attachment by id, check before presign):
  ticket 4.
- `ListIssues` with `?project=<name>` (the name filter must not match a hidden
  project), `ProjectIssueCounts`, `ListTeamsForProjects`, and the parent,
  project and milestone joins in the issue select (redaction of a hidden
  parent, project and milestone): tickets 3 and 5 (#37, #40, #42).
- `DeleteRelation`: the other side of the edge must be visible (#41): ticket 4.
- `checkNoParentCycle` and the `createComment` parent error text ("parentId is
  a comment on another issue"): they must not name a hidden row (#23).
- Activity snapshots are stored whole and are NOT redacted at read time until
  ticket 4 (#43). Until then a restricted viewer of a visible issue can read a
  snapshot that names a hidden issue or project; no route is restricted yet.
- `EXPLAIN` of the `OR all_teams` shape: ticket 3 measures it, and also checks
  generic plans with the pgx statement cache (the note's Performance section).

## Build notes (LAUN-217, ticket 2b)

What ticket 2b built, and the choices the note left open. No numbered rule
changed. No behaviour changed: `all` members and admins get the same answers,
and nothing can set `team_access = 'selected'` (#19). The second commit answers
the first review (both reviewers): it is folded in below.

- **Scope tags (#16).** Every sqlc query (605) has a `-- scope: <tag>` comment
  after its `-- name:` line; sqlc copies it into the Go doc comment of the
  method. Every SQL string literal in a non-test Go file that names a tenant
  table (21 literals, all in `internal/tracker`) has a `/* scope: <tag> */`
  comment. A block comment is used in Go because a fragment is joined into a
  longer statement, where a `--` comment would cut the line. Five dead queries
  were deleted (`GetLabel`, `GetNotification`, `CountUnreadNotifications`,
  `MarkNotificationRead`, `ArchiveNotification`: no caller, tests included).
- **Two tags more than the table of #16.** `none` means "names no tenant
  table" (backups, login codes, platform settings, idempotency rows). Test C
  checks it: a `none` query that names a tenant table fails. `pending` means a
  later ticket scopes the query (the inventory names it). It is honest about
  today: such a query cannot carry `team` (it lacks the predicate), and
  `resolved` or `org` would be wrong. The 19 `pending` queries: `ListTeams`,
  `ListAgentGuidance`, `GetOrgFull`, `ListOrgsForUser` (ticket 3);
  `GetAttachment`, `ListRelationsForIssue`, `DeleteRelation` (4);
  `ListTeamsForProjects`, `ProjectIssueCounts` and seven product reads (5);
  `ListEventsAfter` (6a); `ListNotificationSettings`, `ListTeamFollows` (7). The
  ticket that scopes one changes its tag to `team` or `project` and removes its
  golden line.
- **The six inbox reads are tagged `team`, not `own`.** They carry the
  predicate (2a), so Test C checks it.
- **Labels have a team since LAUN-54.** The note's "labels have no team" is
  out of date. `FindLabels`, `InsertLabel` and `GetLabelsByIDs` are `resolved`
  (the team or the issue is resolved first); `InsertLabelIfAbsent` (default
  labels at setup) is `system`; `ListLabelsWithCounts` is `team` (2a).
- **The golden file** (`internal/api/testdata/scope_optouts.golden`) lists
  every `own`, `org`, `admin`, `system`, `pending` and `resolved` query (433
  lines), as `<tag> <file> <name>`. `team`, `project` and `none` are not
  listed: Test C checks them by content. There is no update flag: Test C prints
  the exact line to add, and the line is reviewed. Limit: a Go literal is named
  by its enclosing function or package-level declaration, so two literals with
  the same tag in one function share one line; a second one needs no new line.
- **Test C, `TestQueriesDeclareScope`** (`internal/api/scope_tags_test.go`).
  - It reads the tenant tables from `db/schema.sql` (55 tables), every query of
    `db/queries/*.sql`, and the string literals of `internal/` and `cmd/` (go/ast;
    only `internal/dbq` and the test-only `internal/testdb` are skipped, and a
    production import of `testdb` fails).
  - The SQL is scanned, not grepped: comments are split from the code, the
    contents of string literals (`'...'`, `$tag$...$tag$`) are blanked, so a `--`
    in a literal starts no comment and a tag or a predicate counts only where it
    really is (a tag only in a comment, a predicate only in the code).
  - A table counts wherever its name stands as a word in the code: comma joins,
    sub-selects, `LATERAL`, `"quoted"`, `public.x`. Not after another qualifier
    (`x.issues`) or in a URL path. A Go literal is SQL when it has an upper case
    keyword and a tenant table, or a keyword right before a tenant table in any
    case; prose is neither.
  - A sqlc `team` query must have the #15 predicate in parentheses:
    `(sqlc.arg(all_teams)::bool OR x.team_id = ANY(sqlc.arg(team_ids)::uuid[]))`;
    a `project` query the #36 shape (`all_teams OR NOT EXISTS (SELECT 1 FROM
    project_teams ...)` and `team_id = ANY(team_ids)`). A query with the
    arguments and another tag fails.
  - The parsed query names must equal the `-- name:` constants sqlc generated in
    `internal/dbq`, so a splitter miss cannot hide a query.
  - It also fails on two tags, an unknown tag, a `none` query on a tenant table,
    a Go `team` or `project` literal whose declaration (or, for a package-level
    template, file) calls no scope helper, an opt-out or `resolved` query not in
    the golden file, and a golden line no query needs.
  - `TestQueriesDeclareScope_Fails` shows each failure and each trick on
    throwaway input.
- **The 2a source scan moved into Test C.** `scope_source_test.go` is gone.
  Its check runs in `TestQueriesDeclareScope`, with the list of scoped queries
  from the tags: each params literal names `AllTeams` and `TeamIds`; `AllTeams`
  is never `true`, never an expression, and when it is a variable, the file sets
  it from `.Access.Args()`. A second check: no handler in `internal/api` calls a
  `...System` query (the unscoped copies of #14). The service-layer callers of
  `system` queries are reviewed through the golden file.
- **The rule table (#17)** is `internal/api/teamrules.go`: `teamRules` (158
  routes under `/orgs/{org}`, keyed `METHOD pattern` relative to the group)
  and `outsideRules` (47 public and install-level routes). Both hold the same
  rule type: a kind, the resource, the parameter, the other references it covers
  (`Refs`), a reason, the ticket that enforces it and `Enforced` (false
  everywhere). Only the three unauthenticated webhooks may sit under
  `/orgs/{org}` in `outsideRules`. `admin` and `none` rules name ticket 8, which
  flips the last flags.
- **Choices in the table.**
  - `GET /orgs/{org}` is `list(org row)` and `GET /orgs` is `own`, both ticket 3:
    each returns the default team's key (`GetOrgFull`, `ListOrgsForUser`), which
    must not name a hidden team (#9). The first commit had them as `none` and
    "no team"; that was wrong (review). The login and session bodies read
    `ListOrgsForUser` too but drop the key.
  - The webhooks are `none` with ticket 8. Their reason says that today the body
    lists the linked and moved identifiers; ticket 8 cuts it to a status (#65).
  - `GET /labels` is `list(label)` with the `?team=` ref and `POST /labels` is
    `body(team)`, both ticket 3: a team label of a hidden team is hidden (#8).
  - `GET /agent-guidance` is `list` with ticket 3: the inventory has no row for
    it (T31 came later), and a hidden team's guidance text must not be listed.
  - Products (`/products`, `/teams/{team}/products`) follow the project rule
    (#69); the ticket list names no product ticket, so ticket 5 takes them.
  - The `own` routes with no team row (`/me/views`, `/me/git-accounts`,
    `/me/push-devices`) name ticket 8.
- **Test A, `TestTeamRulesCoverRouter`** (`teamrules_test.go`) walks the real
  router. It fails on a route with no rule, an entry with no route, a route in
  both tables, a non-webhook route under `/orgs/{org}` in `outsideRules`, and a
  malformed rule (a path rule whose parameter is not in the pattern, a none, own
  or admin rule with no reason, an unknown kind, no ticket).
  `TestTeamRulesCoverRouter_FailsOnNewRoute` shows it on a throwaway router.
- **The guard of #19, `TestNoSelectedWriterWhileUnenforced`.** A writer is:
  an operation of `api/openapi.yaml` whose request body (by `$ref`, nested) or
  parameters have a field `teamAccess`, `team_access`, `access` or
  `restricted`, or a `teams` field on a membership operation (invites, agents,
  sign-ups, users, members); any non-test SQL (sqlc queries, which are the dbq
  methods, and Go literals) that inserts or updates `team_access` or inserts
  `team_members` rows; and a production Go struct field with the JSON name
  `teamAccess`, `team_access` or `restricted`. A response field is not a
  writer. With a writer and any rule (inside or outside `/orgs/{org}`) not
  `Enforced`, it fails. `TestNoSelectedWriterWhileUnenforced_Fails` shows each
  kind on throwaway input.
- **Test B skeleton, `TestTeamRules_RestrictedMember`**
  (`teamrules_integration_test.go`).
  - The fixture: team ORB visible, team HID hidden with an issue, a comment, a
    project, a milestone, a document, a cycle (inserted: the job makes cycles),
    a workflow state and a team label; a visible ORB issue as the target of body
    references; a restricted member (ORB only), an all-teams member, and an
    admin of another org. Every token has all three scopes, so a refusal comes
    from the role or the team.
  - An Enforced rule needs a sample in `teamRuleSamples`, AND one sample per
    Ref (keyed `"<rule> <ref>"`); a missing one fails, and a sample key that
    names no rule or Ref fails.
  - Checks by kind: path (404) and body (400) compare the hidden answer with a
    made-up one byte for byte (status, body after the echoed reference is
    replaced, `Content-Type`, `Location`); a filter must answer 200 or 400 (never
    5xx), the same for hidden and made-up; list, feed and own assert no string of
    team B's objects. After a refused write, the target row (`Snapshot`) and the
    org's newest event seq must not change.
  - Samples written now: `GET /teams/{team}`; `GET /issues` with `team`,
    `cycle` and `parent`; `PATCH /issues/{issue}` (path) with `parent`,
    `projectId`, `milestoneId`, `cycleId`, `state`, `labels`, `placeAfter`,
    `placeBefore`. The other Refs of `GET /issues` (`project`, `assignee`,
    `label`, `state`, `query`) have none yet, so ticket 3 cannot flip that rule
    without writing them.
  - `admin` rules run today whatever the flag: a restricted member gets `403
    forbidden` on all 79 of them. No other rule is Enforced.
- **`TestTeamRules_Harness`** runs the samples that 2a already makes pass
  before their flags: `GET /teams/{team}`, `PATCH /issues/{issue}` with a hidden
  `projectId` or `parent` (400, the visible issue unchanged, no event), and
  `GET /issues` with a hidden `parent` or `cycle`. The all-teams member and the
  admin still read the team. **`TestTeamRules_SamplesRun`** runs every sample
  as the admin and as the restricted member: no 5xx.
- **Cross-org, `TestTeamRules_CrossOrg`:** every route of `teamRules` answers
  `404 not_found` to an admin of another org.
- **The two review asks of 2a.** `TestAvatars_HiddenProject` (api): prepare,
  confirm (with a real uploaded key of the project) and read of a hidden
  project's avatar answer byte for byte as an unknown project, the stored key
  and version do not change, and the uploaded object is not touched.
  `TestScope_LabelList` (tracker) now checks `issueCount` of an org label with
  one issue in each team: 2 for the admin, 1 for a member of either team, 0
  for a member with no team.

- **Third commit (test hardening after the second review).**
  - `sqlScan` applies backslash escapes only in `E'...'` strings; in a plain
    string a backslash is an ordinary character (`'\'` is complete), so a
    table after it is not blanked.
  - A Go literal is SQL also with lower case keywords when it has a statement
    shape (`select ... from`, `insert into`, `update x set`, `delete from`).
    Runs of constant string pieces joined by `+` are folded with go/ast before
    the scan (`"SELECT ... FROM " + "issues"` is one unit), so a table name
    split from its keyword is still seen. The #19 guard reads the folded units
    too (`"UPDATE org_members SET " + "team_access = ..."`), and Go struct
    fields NAMED `TeamAccess` or `Restricted` with no JSON name (encoding/json
    matches them by name).
  - Test B loops over every rule, `outsideRules` included, so `GET /orgs` needs
    its sample once enforced. The fixture's default team is the hidden team
    HID; the samples of `GET /` and `GET /orgs` exist, and the harness shows
    they find the hidden default team key today.
  - Each has a `_Fails` demo.

### Not done here, carried

- `GET /orgs/{org}` and `GET /orgs` name a hidden default team's key until
  ticket 3 scopes `GetOrgFull` and `ListOrgsForUser` (the join to `teams` takes
  the predicate; the key becomes `null`). Ticket 3 writes their Test B samples.
- Test B's list check looks for fixed strings of team B's objects. An
  attachment is not in the fixture yet (it needs the store); ticket 4 adds it
  with its samples.
- `checkNoParentCycle` (tagged `resolved`) walks issues the caller may not see;
  its error text belongs to ticket 3, whose scope has the parent ref and the
  error texts of #23 (2a carried it without a ticket).
- Test C cannot see SQL built at run time beyond the helper call, nor a
  predicate placed outside the `WHERE`; Test B carries that.
- The guard's Go struct check matches JSON names; a request decoded without a
  struct (a map) is caught only by the spec and SQL checks.
- The #15 and #36 shape checks are text only: the predicate's text is in the
  query, in parentheses, but Test C does not prove it sits in the `WHERE` of the
  right statement or binds the right alias. Test B carries that by behaviour.
- SQL hidden in a string that SQL runs again (`query_to_xml('SELECT ...')`,
  `EXECUTE`) is blanked as a literal and not checked. None exists today.
- Go constant identifiers (`const q = "..."` used in `q + "..."`) are not
  folded across declarations; each literal is checked where it is written.
- The webhook rules are kind `none` today. The status-only body is a promise
  of ticket 8 (#65), not a fact; until then the body lists the linked and
  moved identifiers.

## Build notes (LAUN-218, ticket 3)

What ticket 3 built, and the choices the note left open. No numbered rule
changed. Every choice takes the recommended default. `all` members and admins
get the same answers as before, except one text (the parent cycle, below),
which changes for restricted callers only.

- **Rules flipped to `Enforced`** (`internal/api/teamrules.go`, with
  `enforced()`): `GET /`, `GET /api/v1/orgs`, `GET /teams`, `GET
  /teams/{team}`, `GET /teams/{team}/states`, `GET /agent-guidance` (Ref
  `team`), `GET /labels` (Ref `team`), `POST /labels` (body `team`), `GET
  /issues` (Refs `team`, `project`, `cycle`, `parent`, `assignee`, `label`,
  `state`, `query`), and `GET /project-labels` (added after review, below).
  Each rule and each Ref has a Test B sample, and all pass.
  `GET /issues/{issue}` (ticket 4), `POST` and `PATCH /issues` (ticket 5) stay
  `false`, as the ticket list says.
- **Queries scoped.** `ListTeams` (`team`; a `ListTeamsSystem` copy serves the
  git webhook's `linkAndMove` and, until ticket 7, the notification settings
  view), `ListAgentGuidance` (`team`: the org-wide row, plus team rows the
  caller sees), `GetOrgFull` (`team`: the default team join takes the
  predicate, so a hidden default team gives `defaultTeam: null`). Their
  `pending` lines left the golden file.
- **`ListOrgsForUser` is tagged `own`, not `team`.** It lists the caller's
  orgs, and each org has its own access. So the default team join reads each
  membership's access in SQL, as `auth.NewTeamAccess` builds it: `m.role =
  'admin' OR m.team_access = 'all' OR` a `team_members` row for that team. Any
  other flag fails closed. It cannot carry the #15 arguments (one pair per
  org), so Test C cannot check it by shape; it is in the golden file, and
  `TestTeamAccess_DefaultTeamKey` checks it per org (null in one org, the key
  in another org where the same user is an `all` member). The login and
  session bodies read it too and drop the key.
- **The issue select is a function** (`issueSelect(org, arg, desc)` in
  `issue_read.go`), used by `ListIssues` and `getIssueByID`. Its joins are
  scoped (read-time redaction; the stored row does not change):
  - the parent join takes `teamScope` on `p.team_id`, and the row's parent id
    is `p.id`, so a hidden parent gives `parent: null` (#42);
  - `projectId` is `pj.id` of a join with `projectScope` (#36, #40);
  - `milestoneId` is `ms.id` of a join whose milestone's project passes
    `projectScope` (#38, #40), judged by the milestone's own project.
  `GET /issues/{issue}` and the `PATCH` response use the same select, so they
  redact as well, although their flags are tickets 4 and 5.
- **`GET /issues` filters.** `?project=<id>` and `?project=<name>` match only a
  project the caller sees (`projectScope` in the filter's `EXISTS`), so a hidden
  project answers the empty list of an unknown one and its name cannot be
  probed (#40). `project=none` (LAUN-179) also holds an issue whose project
  the caller cannot see, so the filter agrees with the redacted `projectId`
  (review of the first commit; a Test B `Also` sample and
  `TestTeamAccess_ProjectNone`). `?label=` does not match a team label of a
  hidden team. `?cycle=<number>` and `active` add `teamScope` on the cycle's
  team. These two are defence in depth only: the API never puts a hidden
  team's label or cycle on a visible issue (labels resolve in the issue's
  team, a cycle must be of the issue's team), so
  `TestTeamAccess_LabelCycleFilterDepth` makes such a row by hand to show the
  predicates work. Such a hand-made row would still show its label and
  `cycleId` in the issue body; no API path makes one. `?team=`, `?cycle=<id>` and `?parent=` were scoped in 2a. The
  Linear id forms of `team`, `cycle` and `parent` answer as a made-up Linear id
  (`TestTeamAccess_LinearIDs`). `?product=` is ticket 5 (#69).
- **The parent cycle text (#23).** `checkNoParentCycle` takes the `*auth.Org`
  and the reference. For a restricted caller a cycle answers `unknown parent
  "<ref>"`, the same body as for a parent that does not exist (compared after
  the echoed reference is replaced), because the walk may pass through hidden
  issues. A caller who sees every team keeps "that parent would make a
  cycle". The text depends on the caller's access only, never on what the
  walk met. **Residual:** the refusal itself is a signal. The parent is
  visible to the caller, yet the answer is "unknown", so the caller learns
  that some chain, maybe through hidden issues, closes the loop. It names no
  hidden issue and no count. A refusal is needed to keep the tree acyclic.
- **Agent guidance and labels.** `GET /agent-guidance?team=` and `GET
  /labels?team=` answer `404` for an unknown team today; a hidden team gives
  the same `404`, so their Test B samples are judged as path samples (byte for
  byte). `POST /labels` with a hidden team is `400 unknown team`, no row and
  no event. `GET /labels` was scoped in 2a.
- **Test B fixture additions.** A member `hank` assigned only to the hidden
  issue, which carries the hidden team label; guidance for team HID ("hidden
  guidance", a new mark); and an ORB issue (`child`) whose parent, project and
  milestone are the hidden ones, so the `GET /issues` list check (no mark of
  team B) also catches a redaction miss. `TestTeamRules_Harness` now shows
  that each list sample, read by the all-teams member, does hold team B.
- **`TestHiddenAndMissingSamePath`** (`team_access_integration_test.go`): a
  pgx tracer on the server's pool (`envOpts.tracer`) records every statement.
  For `GET /issues/{issue}` by identifier, issue id and Linear id, and for
  `?parent=`, a hidden issue and a missing one give the same status, body
  (after the echoed reference is replaced), `Content-Type`, `Location` and
  `Cache-Control`, and the same statements in the same order. Limits: it
  compares the SQL text only (not the arguments, the plans or the times); the
  missing identifier is `HID-999` in the hidden team (a key the caller does
  not see), not a key of a visible team; and it covers `GET /issues/{issue}`
  and `?parent=` only. The other path and filter rules rest on Test B's
  byte comparison.
- **Other tests** (`team_access_integration_test.go`): the default team key
  (restricted, a restricted member of that team, admin, `all` member, and per
  org in `GET /orgs`); the redaction of #40 and #42 in the list and by id, with
  the stored row unchanged and visible refs kept; the project filter by id and
  name; full pages (#24: `limit=n` is full and last, `limit=n-1` has one more
  row on page 2); the parent cycle text; an assignee who cannot see the team
  is accepted (#46) and cannot read the issue; teams, guidance and labels lists;
  Linear ids; and cross-org (another org's project id and name, team key in
  `?team=`, `/states`, `?team=` of guidance and labels, `POST /labels`).
- **EXPLAIN** (Performance section; AC of ticket 3). Synthetic data, not
  imported data: one org, 10 teams, 100,000 issues (team T0 40%, the others
  about 6,700 each), 60 projects with one or two teams, a milestone each, 30%
  of issues in a project, 10% with a parent; Postgres 16 in the dev compose
  stack, warm cache. Callers: an admin (`all`), a member with 1 team (T3) and
  one with 5 teams (T0 to T4). Shapes: the list (`updatedAt`, limit 50), the
  board (`?team=` one team, `orderBy=manual`, limit 250) and the manual order
  of the whole org (limit 50). Times are execution times of `EXPLAIN (ANALYZE,
  BUFFERS)` with the custom plan, and the median wall time of 20 runs through
  pgx (planning included).

  | Shape | Caller | Plan (custom) | Execution | Median of 20 |
  |---|---|---|---|---|
  | list | before (2b base, no predicate) | `issues_org_updated_idx` backward | 0.27 ms | 1.96 ms |
  | list | all | the same index; the `OR` folds away | 0.56 ms | 4.46 ms |
  | list | 1 team | the same index, filter `team_id = ANY` (706 rows removed for 51) | 1.28 ms | 4.37 ms |
  | list | 5 teams | the same index and filter | 0.55 ms | 4.01 ms |
  | board | before (T0) | `issues_manual_order_idx`, top-N sort | 23.0 ms | 20.6 ms |
  | board | all (T0) | the same | 25.6 ms | 24.9 ms |
  | board | 1 team (T3) | `issues_manual_order_idx` with `team_id = ANY` in the index condition | 11.3 ms | 11.5 ms |
  | board | 5 teams (T0) | the same | 26.2 ms | 24.6 ms |
  | manual order | before | parallel seq scan, top-N sort | 57 to 64 ms | not run |
  | manual order | all | the same | 68.6 ms | 55.7 ms |
  | manual order | 1 team | the same with the filter | 5.8 ms | 8.3 ms |
  | manual order | 5 teams | the same with the filter | 20.2 ms | 23.6 ms |

  - With a custom plan the `$all_teams OR ...` of an all-teams caller folds
    away: the plans are the plans of before, plus the redaction joins.
  - The list costs about 2 ms more per request (median 1.96 to 4.46 ms). Most
    of it is planning (1.6 ms before, 3.6 to 6.4 ms after): the statement has
    two more joins and the `EXISTS` sub-plans of `projectScope`. Execution
    stays under 1.3 ms.
  - **Generic plans and the pgx statement cache.** In the default mode
    (`plan_cache_mode = auto`), 20 runs of each statement on one connection
    through pgx's statement cache gave `generic_plans = 0`, `custom_plans = 20`
    for every shape and caller (`pg_prepared_statements`): Postgres keeps
    custom plans, so the boolean always folds. Forced generic plans
    (`force_generic_plan`) ran as fast or faster (list 4.1 to 4.8 ms, board 9.5
    to 20.8 ms, manual order 7.8 to 25.1 ms), because they skip planning; the
    generic list plan scans the same index and filters `($2 OR team_id =
    ANY($3))`. So `force_custom_plan` is not set.
  - A restricted member's list scans `issues_org_updated_idx` and filters by
    team, as the Performance section feared. With a team of 6.7% of the issues
    it read 757 rows for 50. A team of 1% would read about 5,000. The index
    `(org_id, team_id, updated_at, id)` is still not added: no measured need on
    real data.
- **`GET /project-labels`** (review of the first commit). `ListProjectLabels`
  counted every project with the label, hidden ones too: a count is a leak
  (security analysis, "Counts"). It is now tagged `project` and its count
  takes the #36 shape, so it counts only projects the caller sees. The rule
  moved from `none` to `list(project label)`, ticket 3, `Enforced`. Its Test B
  sample checks that label `secret-pl`, on the HID-only project, has
  `projectCount` 0 for the restricted member; the harness shows it is 1 for
  the all-teams member. Test B samples gained `Check` (a body check after the
  kind's check) and `Also` (more samples of the same rule or Ref).
- **Other counts reviewed** (queries tagged `org`, `own`, `none`, `resolved`,
  `system`, `admin` or `pending` that count or test rows of issues, projects,
  milestones, cycles or labels): `ProjectIssueCounts` is `pending`, ticket 5.
  `CountIssueChildren`, `CountCycleIssues`, `CountProjectIssues`,
  `CountTeamIssues`, `CountOrgTeams`, `CountProjectsWithStatus`,
  `OnboardingFacts` and `ImportSessionBlocksCycles` are on `admin` routes.
  `CycleRangeTaken` and `UpsertCycleSnapshot` count inside one cycle whose
  team is resolved first. `GetIssueSubscriptionFacts` is about one resolved
  issue. The importer queries are `system`. `ListLabelsWithCounts` was scoped
  in 2a. No other count was found.
- **Not done here, carried.**
  - **An assignee without access gets no notification** (ticket 3 AC): not
    done. The consumer's eligibility with access is #61, ticket 7. Until then
    the consumer would notify such an assignee; no member can be restricted
    before ticket 9 (#19). The acceptance part (#46) is tested.
  - The notification settings view (`GET /me/notification-settings`) still
    lists every team (`ListTeamsSystem`): ticket 7 (#64).
  - `?product=` and the products routes: ticket 5 (#69).
  - `api/openapi.yaml` does not say that `defaultTeam` is null for a caller
    who cannot see the team (the field is nullable already). Ticket 8 writes
    the enforced surface into the docs.
  - The EXPLAIN is on synthetic data. Real imported data and the inbox are not
    measured.

## Build notes (LAUN-221, ticket 6a)

What ticket 6a built, and the choices the note left open. No numbered rule
changed. Every open choice takes the recommended default. It was built on
2b (LAUN-217) and rebased onto ticket 3 (LAUN-218); it needs no code of
tickets 4 or 5. Admins and `all` members
read the same feed as before (no event has the `admin` scope yet).

- **Migration M2 is `0038_events_scope`.** `events.scope text NOT NULL DEFAULT
  'legacy'` with a CHECK (`legacy`, `any`, `all`, `org`, `admin`) and
  `events.team_ids uuid[] NOT NULL DEFAULT '{}'`, plus one more CHECK: `any`
  and `all` need at least one team. The note did not name it: an `all` row
  with no team would match every viewer (the empty set is a subset of every
  set). No backfill (6b). The down drops the CHECK and both columns.
  `TestMigration0038_EventsScope` and `TestMigrateUpDownUp` cover it.
- **`events.Emit(ctx, q, orgID, ev, scope)`** (#53). The constructors are
  `ScopeTeams`, `ScopeAllTeams`, `ScopeOrg`, `ScopeAdmin`, and one more,
  `ScopeProject(teamIDs)`: the project's teams, or `org` when there are none
  (#36, #54). The zero `Scope` is refused, so a forgotten scope fails at the
  first test that runs the path. Team lists are sorted, without duplicates
  and without the nil id.
- **A scope rule per event type, checked at emit time.** `events.kindScopes`
  lists, for every event type constant, the scope kinds it may use. `Emit`
  refuses an unlisted type, a kind the type does not allow, and `any` or
  `all` without a team. This is the "scope function" of Test D: the unit half
  (`TestEventsEmitScope`, `internal/events`) parses `events.go` and fails
  when a type constant has no rule.
- **More call sites than the note counted.** The note said 21 call sites;
  the code had 44 (`events.Emit` outside tests), because the event list grew
  after the note (LAUN-191 to LAUN-209). Each names a scope now. Code that
  collects events and emits them last uses `events.Scoped` (`ev.In(scope)`)
  and `events.EmitAll` (the git webhooks: one event per issue, each with the
  issue's team), or `emitAll(ctx, q, orgID, scope, evs)` (cycles: every event
  of one run is about one team).
- **Scopes of the kinds the table of #54 does not list** (they came later):
  - `issue.deleted`: the issue's team.
  - `cycle.deleted`: the cycle's team.
  - `team.deleted`: that team.
  - `project.deleted`: the project's teams, read before the delete (the
    `project_teams` rows go with it).
  - `project_update.created`, `project.update_due`: the project's teams; org
    when none.
  - `label.created`: org for an org label, the team for a team label
    (LAUN-54).
  - `agent_guidance.updated`: org for the org entry, the team for a team
    entry (the payload names the team key, #9).
  - `chat_service.updated`: the service's team (T38; the payload names the
    team id). Only `any` is allowed.
  - `product.updated`: the product's teams (#69), org when none.
    `product.created`, `product.deleted` and `product.teams_changed` list the
    team ids in their payload, so they are all-of: only a viewer who sees
    every listed team reads them (#9). For `teams_changed` the list is the
    teams now plus the removed ones. Residual: a member who sees only some of
    a product's teams gets no event for these three and refreshes the product
    on the next read.
  - `project_status.*`, `project_label.*`, `org.updated`,
    `onboarding.step_changed`, `git_identity.removed`, `push.updated` and the
    legacy `slack.updated`: org (org-level, #7; their payloads hold ids,
    names of org-wide things and field names). `onboarding.step_changed`
    is not `admin`, so an `all` member sees what they saw before.
- **The scope comes from the writing transaction.** `event_scope.go`
  (tracker) reads the issue's team (`ListIssueTeamIDsSystem`), the project's
  teams (`ListProjectTeamIDs`) and a document's project
  (`GetDocumentProjectIDSystem`) with `system` queries, in the same `tx`,
  before `Emit`. Where the team is at hand (issue create and update, cycle
  and team routes, webhooks) no read is added. A comment follows its issue,
  or its document's project (org when there is none). A document or project
  update uses the project's teams after the change.
- **The feed query (#55).** `ListEventsAfter` takes `is_admin`, `all_teams`
  and `team_ids`. An admin reads every row; an `all` member every row but
  `admin` ones (legacy included); a restricted member `org` rows, `any` rows
  that share a team and `all` rows whose every team is visible (and not
  empty). The filter is in the `WHERE`, before `LIMIT` (#24). `is_admin` is
  `org.Role == "admin"`, so a platform admin who is a plain member is not an
  admin here.
- **A new tag `feed`** (Test C). The feed predicate is not the #15 shape, so
  `ListEventsAfter` is tagged `feed` and Test C checks its five cases on the
  query text without comments (the literals `'org'`, `'any'` must stay
  visible, so the blanked code is not enough). Its params are checked like a
  `team` query's (`AllTeams` from `Access.Args()`). `ListEventsAfter` left the
  `pending` list. `ListEventsAfterSystem` (the consumer, #55, #61),
  `ListIssueTeamIDsSystem` and `GetDocumentProjectIDSystem` are `system` and
  in the golden file.
- **The consumer reads `ListEventsAfterSystem`** and still sees every event.
  Its recipient rules are unchanged (ticket 7).
- **The stream (#56).** `streamEvents` calls `StillAuthorized` at the start
  of every drain: the first one, every NOTIFY wake-up and every poll tick.
  `StillAuthorized` now returns the role and the access (`tracker.Standing`).
  When the token, the user or the membership is gone, the stream ends with
  `: credentials no longer valid`; a missing membership row is never read as
  an empty set. When the role or the access changed, it sends `event: access`
  and then drains with the new set. The poll tick no longer has its own
  check; the drain does it. Cost: two or three primary-key reads per drain
  and stream.
- **`access` carries `data: {}`, not empty data.** A browser `EventSource`
  does not dispatch an event whose data is empty, so the note's "empty data"
  would never reach the web. It has no `id`, so `Last-Event-ID` does not move,
  and it is not stored. The web handler is ticket 11b (#76).
- **Enforced:** `GET /events` and `GET /events/stream` (both `feed`). Test B
  samples: the feed from seq 0, and the stream from seq 0 read up to a marker
  event the caller can see (`readStream`). The fixture makes the hidden
  team's objects through their routes, so the feed holds their events.
- **Tests added.** `TestEventsEmitScope` (unit). `TestEventsEmitScope_RestrictedViewer`
  (Test D, api): 27 kinds made in a hidden team through their routes are
  stored with that team; a restricted member reads none of them from the
  feed or the stream and reads every visible and org event; the `all` member
  and the admin read everything; a relation between a visible and a hidden
  issue is `all` and hidden until the member sees both teams; a legacy row
  shows to unrestricted viewers only, an `admin` row to admins only. Every
  kind that may have a team scope is made by the test or listed in
  `uncoveredKinds` with a reason. `TestEventsStream_AccessChange`: with the
  poll at one hour (so only NOTIFY wakes the stream), a team added sends one
  `access` and then the new team's event; a team removed sends `access` and
  then no event of that team; a removed membership ends the stream; and with
  the 250 ms poll, `access` comes without any event.
  `TestEventsStream_AccessEventOnlyOnChange`: no `access` when nothing
  changed. `TestMigration0038_EventsScope`. A cross-org case for both routes
  is in `TestTeamRules_CrossOrg`.
- **openapi.** The descriptions of `GET /events` (gaps in `seq`) and the
  stream (`event: access`) changed; `web/src/api/schema.d.ts` is regenerated.

### Not done here, carried

- The backfill job of #57: ticket 6b. Until it runs, every event made before
  `0038` is `legacy` and hidden from restricted members.
- Kinds not made by Test D through a route: `cycle.created`,
  `cycle.completed`, `cycle.deleted` (they share `emitAll` with the team
  scope, as `cycle.updated`, which is made), `project.update_due` (the same
  helper as `project_update.created`) and `chat_service.updated` (needs the
  chat test env). `uncoveredKinds` names them.
- The git webhook events (`issue.updated`, `attachment.created` from
  `HandleSCMEvent`) take the issue's team; Test D does not drive a webhook.
  Ticket 8 has the webhook tests (#65).
- The inbox stream (`streamInbox`) does not re-read the access per drain
  yet: ticket 7 (#56, #62).
- Residual of #54: the scope is frozen at emit time, so an event of a
  project or document keeps the teams the project had then.

### Review fixes (LAUN-221, second commit)

- **Access before every page.** A drain re-reads the credentials, the role
  and the access before each page (`recheck`), not once per drain. A removal
  takes effect within one page (100 events). Accepted residual: a page read
  just before the removal is still sent. `TestEventsStream_RemovalWithinOnePage`
  removes HID between the first and the second page of a drain (page size 2
  in the test) and sees two HID events, then `access`, then no HID event,
  also none made after the removal.
- **High-water mark (#55).** A reader's cursor used to move only to events it
  received, so a restricted reader read the hidden rows after its last
  visible event again on every drain. Now each drain reads `LatestEventSeq`
  first and, when its pages end, moves its cursor to it. Why that is safe:
  `events.Emit` holds the org's counter row lock (`NextEventSeq`) until
  COMMIT, so seqs commit in order; once seq M is committed, every smaller seq
  is committed or rolled back, and the page queries (later snapshots) see
  every event up to M. No `id:` is sent for it, so `Last-Event-ID` stays the
  last event the caller received; a reconnect may read the hidden tail once.
  `GET /events`: when a page is not full but the server read past its last
  item, `nextCursor` is the seq read up to; no `nextCursor` is still the end
  (a full page still gives the last item's seq). For an unrestricted reader
  the high-water mark equals the last item, so nothing changes. The web poll
  (`web/src/api/events.ts`) now keeps that cursor as its `lastSeq`.
  `TestEvents_HighWaterMark`: hidden rows only give no item and the mark,
  the mark gives the end, following `nextCursor` with `limit=1` through
  interleaved hidden and visible events returns exactly the visible ones, an
  all-teams member gets no `nextCursor` on a short page, and a stream's next
  drain starts above the hidden rows. The EXPLAIN numbers are in
  "Performance and indexes".
- **Payloads that counted hidden rows.** `project.deleted` lost
  `detachedIssues` (it counted issues of every team in the project, #37), and
  `issue.deleted` lost `parentId` and `detachedChildren` (the parent and the
  children may be in a hidden team, #42). Nothing reads them: not the web,
  the MCP server or the notify consumer (grep); only two tests did, and they
  now assert the fields are absent. `CountProjectIssues` and
  `CountIssueChildren` had no other caller and are deleted. T26 #7 and T32 #8
  point here.
- **Moves between teams.** `project.updated` and `document.updated` go to the
  readers before AND after the change (`eitherScope`): the project's teams
  before and after, or the document's project before and after; org when
  either side is org-level. A team that loses a project hears of it.
- **0038 keeps the inline CHECKs, not `NOT VALID` + `VALIDATE`.** The
  migrator (golang-migrate) sends each file as one multi-statement query,
  which Postgres runs as one implicit transaction, and `ADD COLUMN` takes an
  `ACCESS EXCLUSIVE` lock on `events` until COMMIT anyway; a `VALIDATE` in the
  same transaction would scan under that lock too, so the split gains
  nothing. The scan reads two constant-default columns; its time on a large
  `events` table is not measured. A split into two migrations (the second
  only `VALIDATE`, which takes `SHARE UPDATE EXCLUSIVE`) is the answer if a
  large install needs it.
- **Test D** now also asserts that each kind made in team HID is stored with
  HID in `team_ids` (not only "hidden from ORB"); `team.deleted` is of the
  throwaway team GON.

## Build notes (LAUN-219, ticket 4)

What ticket 4 built, and the choices the note left open. No numbered rule
changed. Every choice takes the recommended default. Admins and `all` members
get the same answers as before; only restricted callers see a change.

- **Rules flipped to `Enforced`** (`internal/api/teamrules.go`): `GET
  /issues/{issue}` (Ref `include`), `GET /issues/{issue}/activity`, `GET` and
  `POST /issues/{issue}/comments` (Ref `parentId`), `PATCH` and `DELETE
  /comments/{comment}`, `GET` and `POST /issues/{issue}/attachments`, `GET
  /attachments/{attachment}` and `.../download`, `GET` and `POST
  /issues/{issue}/relations` (Ref `issue`), `DELETE
  /issues/{issue}/relations/{relation}` (a new Ref `relation`: an edge of a
  visible issue whose other side is hidden). Each rule and each Ref has a Test
  B sample, and all pass. 23 of 205 rules were `Enforced` on this branch; with ticket 6a merged in (the two feed rules) it is 25.
- **Queries scoped** (their `pending` lines left the golden file):
  - `GetAttachment` is `team`: it joins the attachment's issue and takes the
    #15 predicate. `GetAttachment`, `DownloadURL` and the attach replay
    (`replayOf`) all read it through `getAttachmentRow`. So the download looks
    up the row with the scope BEFORE the store is asked: a hidden attachment
    is the 404 of a missing one and no URL is signed. An upload key already
    attached to a hidden issue answers the same `409` as one attached to
    another visible issue.
  - `ListRelationsForIssue` is `team`: it joins the other side of each edge
    and takes the predicate (#41). `relationsFor` still drops an edge whose
    ref is missing (defence in depth).
  - `DeleteRelation` is `team`: the delete needs the path issue (resolved
    first, as before) and an `EXISTS` on the other side with the predicate.
    An edge to a hidden issue is the `404` of an unknown relation; nothing is
    deleted, no activity, no event.
  - `CreateRelation` already resolved both issues with the scoped resolver
    (2a); a hidden `issue` in the body is `400 unknown issue`, as a made-up one.
- **Activity redaction (#43).** `ListActivity` calls `redactActivity` on each
  page. The rows of kind `parent` and `relationRemoved` name an issue,
  `project`, `milestone` and `cycle` name that object. The service reads the
  ids of the page and looks them up with the caller's scope, one query per
  kind that the page has: `ListIssueRefs` (`team`), and three new queries
  `ListVisibleProjectIDs`, `ListVisibleMilestoneIDs` (`project`, the
  milestone by its project) and `ListVisibleCycleIDs` (`team`). A snapshot
  whose id is not returned becomes `null`; the row is still listed. For
  `relationRemoved` the whole `from` (type and issue) becomes `null`. The
  stored rows do not change (the snapshots stay written by the `System`
  lookups of 2a). Choices:
  - A caller who sees every team (`Access.All`) skips the step and runs no
    extra query. Reason: the lookups miss a deleted object, and an admin
    must keep reading the stored name of a deleted project, as today.
  - For a restricted caller a snapshot whose object no longer exists is
    `null` too (its team is unknown, so it fails closed), and so is a
    snapshot of these kinds that has no readable id.
  - Assignee and label snapshots are not redacted: users are org-level (#7);
    a label on an issue is of the issue's team or org-wide (ticket 3 notes),
    and `mrLinked` / `mrMerged` / `mrClosed` name no tracker row.
- **The reply parent text (#23).** `createComment` (issue and document
  comments share it) already found the parent with the scoped `GetComment`
  (2a), so a hidden parent was "unknown parentId". Now a parent that is a
  visible comment of another target also answers "unknown parentId" for a
  caller who does not see every team. A caller who sees every team keeps
  "parentId is a comment on another issue / document". The text depends on
  the caller's access only, as the parent cycle text of ticket 3.
- **Comments by id** were scoped in 2a (`GetComment`, `GetCommentForUpdate`
  judge an issue comment by its issue's team and a document comment by its
  document's project). The `PATCH` and `DELETE /comments/{comment}` samples
  cover a hidden issue comment and a hidden document comment. The
  `/documents/{document}/comments` routes stay ticket 5, as the table says.
- **State history and comments in `include`** read rows of the resolved
  issue only (`ListStateChanges`, `ListIssueComments`, `resolved`). An issue
  never changes team, so its state rows are of its own team. Merge-request
  links (`changeRequests`) are per issue and name no other tracker row.
- **Test B fixture additions.** A comment on the hidden document, a file
  attachment row of the hidden issue (no object: a download only signs a
  URL), an edge from the visible issue to the hidden one, and an edge from
  the child to the hidden issue that the admin removed (so the child's
  activity holds a `relationRemoved` row next to its parent, project and
  milestone rows). New marks: the attachment, the edge, the document comment
  and the milestone name. The fixture's server now has the test store
  (`storagetest`), so a download that passes the scope is a `302`, not a
  `503`; `newTeamFixture` takes optional `envOpts`. Helpers: `issuePath`,
  `commentPath`, `attachmentPath`, `rows(table)` (the snapshot of a refused
  write) and `activityRedacted` (a `Check`). `TestTeamRules_Harness` shows the
  all-teams member reads the hidden issue in the includes, relations and
  activity, and that the activity `Check` fails for that member.
- **`TestHiddenAndMissingSamePath`** gained the routes of this ticket: the
  includes, activity, comments, attachments and relations of a hidden issue;
  an attachment and its download by id; `PATCH` and `DELETE` of a hidden
  comment; a reply whose `parentId` is a hidden comment; and `DELETE` of an
  edge to a hidden issue. Each gives the same status, body, headers and SQL
  statements as a missing one, and the refused writes leave the rows.
- **Other tests** (`team_access_children_integration_test.go`):
  `TestTeamAccess_AttachmentDownload` (a store spy counts `PresignGet`: zero
  for the hidden and the cross-org downloads, one each for a visible
  download, the admin and the all-teams member);
  `TestTeamAccess_Relations` (absent from the visible issue's `/relations`
  and `include=relations`, the hidden side is `404`, no create or delete by
  the restricted member, rows unchanged; edges between visible issues still
  work; the admin and the all-teams member see the edge from both sides);
  `TestTeamAccess_ActivityRedaction` (parent, project, milestone,
  relationRemoved, a hand-made cycle row and a row naming a deleted project
  are `null` for the restricted member and whole for the admin and the
  all-teams member; the stored rows do not change; a visible new parent
  keeps its snapshot); `TestTeamAccess_ReplyParentText`; and
  `TestTeamAccess_ChildrenCrossOrg` (another org's attachment, download,
  comment, reply parent, relation issue, relation id, issue with includes and
  activity answer as unknown ones, byte for byte).
- **Mutation check (manual, not committed).** With the redaction off, the
  attachment lookup unscoped, or the relation delete unscoped, Test B, the
  path test and the ticket 4 tests fail.
- **Separate commit.** develop failed Test C: two LAUN-214 queries in
  `db/queries/chat.sql` (`GetChatEventStateIDs`, `ListChatStateNames`) had no
  scope tag. They are tagged `system` with their golden lines, as on the
  LAUN-221 branch; the duplicate drops out on rebase.

- **Merge of develop (ticket 6a).** Test D
  (`TestEventsEmitScope_RestrictedViewer`) counted every event that spans
  ORB and HID and wanted two. The ticket 4 fixture makes cross-team edges of
  its own, so the test now counts only the events after its own first
  write; every such event of the run is still checked for scope `all`.

### Carried to later tickets

- `GET` and `POST /documents/{document}/comments` (`ListDocumentComments`,
  the document resolver): ticket 5 with the documents (#38).
- `POST` and `PATCH /issues`: ticket 5, as the ticket list says.
- The issue subscription routes (`/issues/{issue}/subscription`): ticket 7.
- The change feed: done by ticket 6a (LAUN-221, merged into this branch).
  Comment, attachment and relation events carry their issue's scope, and
  `legacy` rows are hidden from restricted members until the backfill (6b).
- An activity snapshot names its object at write time. A restricted viewer
  reads `null` for an object that was deleted later, also when it was of a
  visible team; the admin reads the stored name. Accepted: it fails closed.
- A presigned download URL handed out before access was removed lives its
  TTL (5 minutes), as the inventory says.
- Not tested: label snapshots of a hidden team's label (no API path puts one
  on a visible issue).
- Residual: the attach of an upload key held by a hidden issue answers the
  same `409` text as a key held by another visible issue, but it runs one
  more statement (`AttachmentKeyHeldSystem`) than the visible case. This
  needs the EXACT object key (the id and the filename segment as prepare
  made them), which only someone who prepared or was given that upload
  holds; the path difference says only "held by an issue you cannot see".
  Accepted. A key built from a known attachment id alone (an id a caller
  learned earlier, before access changed) is not held and answers as a
  made-up key.
- Residual: the restricted reply parent text and the restricted parent cycle
  text (ticket 3) differ from the texts an all-teams caller gets, by design.

### Review commit (LAUN-219)

Both reviewers passed the first commit; this commit takes their asks.

- **An upload held by a hidden issue never reaches the store.** `replayOf`
  is scoped, so before this commit an upload key already attached to a
  hidden issue missed it and went on to `store.Head` (and, for an object
  that failed the type or size check, to `store.Delete`). Now
  `fileAttachment` asks `AttachmentKeyHeldSystem` (tagged `system`, golden
  line added; it returns a boolean only) whether any row of the org holds
  the upload's id or object key. A held key is the `409` "this upload is
  already attached" of a key held by another visible issue, with no Head and
  no Delete, so an object that a row holds is never deleted by the attach
  path. The query matches the exact object key only, never the id inside
  it (re-check of the review commit: matching the id too made a key built
  from a hidden attachment's id answer `409` where a made-up id answers
  `400`, an oracle of #10). A key built from a known id with another
  filename is not held, goes to `Head`, finds no object, and answers the
  `400` of a missing upload; `Delete` is never reached for a key with no
  object. `TestTeamAccess_AttachHeldKey` (a store spy) checks it: same status
  and body as a key held by a visible issue, zero Heads and Deletes, rows
  unchanged, the object still there; and a key made from the hidden
  attachment's id (both the uploaded one and the fixture's) with another
  filename answers byte for byte as a key with a made-up id, with one Head
  each and no Delete. With the check removed the test fails (one Head).
- **Predicates pinned at the query.** `relationsFor` drops an edge with no
  visible ref, and Test B's `parentId` sample would stay green on another
  check, so `TestTeamAccess_QueryPredicates` calls the queries directly:
  `ListRelationsForIssue` with restricted arguments does not return the edge
  to the hidden issue (and does with all teams), and `GetComment` with
  restricted arguments finds no comment of a hidden issue or a hidden
  document, and does find a visible one.

## Build notes (LAUN-220, ticket 5)

What ticket 5 built, and the choices the note left open. One numbered rule
changed: #40 (the same hidden value is the unknown-id `400`, not a no-op; a
visible project that replaces a hidden one clears the hidden milestone). Later
tickets must not turn either back. Every other choice takes the recommended
default (F, G). `all` members and
admins get the same answers as before.

- **Rules flipped to `Enforced`** (`internal/api/teamrules.go`): `GET
  /teams/{team}/products`, `GET /products`, `GET /products/{product}`; `GET
  /projects` (Refs `team`, `label`), `POST /projects` (body `teams`), `GET` and
  `PATCH /projects/{project}` (Ref `teams`); the four project avatar routes and
  the three project update routes; `GET /milestones` (Ref `project`), `POST
  /milestones`, `GET` and `PATCH /milestones/{milestone}`; `GET /cycles` (Ref
  `team`), `GET` and `PATCH /cycles/{cycle}`, `GET /cycles/{cycle}/snapshots`;
  the six document routes (Refs `project`, `projectId`, `parentId`); `POST
  /issues` (body `team`, Refs `parent`, `projectId`, `milestoneId`, `cycleId`,
  `state`, `labels`) and `PATCH /issues/{issue}` (all its Refs). Each rule and
  each Ref has a Test B sample (`teamrules_planning_integration_test.go`, joined
  to `teamRuleSamples` in `init`), and all pass. 40 rules are `Enforced`.
- **`lead` and `labelIds` are no longer Refs** of `POST` and `PATCH /projects`.
  A Ref names a team-owned object. Any member may lead (#46), and project labels
  are org-wide (decisions-T28). `TestPlanningAccess_ProjectTeams` covers the lead
  rule: a restricted member makes a project led by a member with no team, who
  then cannot read it.
- **Queries scoped.** `ListTeamsForProjects` (`team`: a project's `teams` list
  only visible teams, #37), `ProjectIssueCounts` (`team`: `issueCounts` count
  only issues of visible teams, #37). The teams replace reads a new
  `ListProjectTeamRows` (`resolved`), which returns hidden rows too, so they are
  kept. Products (#69): `GetProductByID`, `GetProductBySlug`, `ListProducts` are
  tagged `project` and carry the #36 shape over `product_teams` (a product is
  visible when the caller sees one of its teams, archived ones included, or it
  has none); Test C accepts `product_teams` in that shape. `ListTeamsForProducts`
  and `ListProductTeams` are `team` (`teamKeys` and `teams` list visible teams
  only). `ListProductIDsForTeams` and `ListProductsForTeam` are `resolved`: their
  teams come from a scoped list or resolver, so each product they return has a
  visible team. Every `pending` line of ticket 5 left the golden file.
  `resolveProduct` takes the `*auth.Org`, so `?product=<hidden>` on `GET
  /issues` matches nothing, as an unknown product.
- **`GET /projects?team=` (a leak found by the new fixture).** The filter's
  `EXISTS` over `project_teams` had no team predicate, so `?team=HID` found a
  visible project through its hidden team row: a restricted member learned that
  the project includes team HID. The `EXISTS` now takes `teamScope` on
  `pt.team_id`, for the key and the id form.
- **#39.** `POST /projects` by a restricted member with no team is `400 teams must
  name at least one team`; a hidden team is `400 unknown team` (`resolveTeams` is
  scoped since 2a). In `PATCH`, `replaceProjectTeams` takes the `*auth.Org`: for a
  restricted member it replaces only the visible rows and keeps the hidden ones;
  an empty or null `teams` on a project with teams is `400 teams must keep at
  least one of your teams`; on a project with no team, a non-empty `teams` is `400
  the teams of a project with no team can be set only by a member who sees every
  team` (an empty one is a no-op). The texts name nothing hidden: a project the
  caller sees with an empty team list has no team.
- **#40 in `PATCH /issues`.** `hiddenPlanningRef` (`issue_write.go`) asks the
  scoped `ProjectExists` / `GetMilestoneProjectID` whether the issue's current
  project or milestone is hidden from the caller. A `null` then leaves it (no
  write, no event, `200` with the redacted issue). Two choices the note left open:
  - **The same hidden id again is `400 unknown projectId`** (or `milestoneId`),
    byte for byte the answer for a made-up id. It does not return early: it
    marks the field as changed, so `checkPlanningRefs` judges it in its usual
    order (project first) and runs the same statements as for a made-up id
    (second commit, after review: an early return answered "unknown milestoneId"
    for `{projectId: made-up, milestoneId: hidden}` but "unknown projectId" for
    two made-up ids, which confirmed a guessed hidden milestone). A milestone
    named this way is not cleared by the rule below. The note said "a `null` (or the
    same value) is a no-op". A `200` for the same value would confirm a guessed
    hidden id (#10, "never accepted as proof"); the web never sends it, because it
    reads `null`.
  - **A visible project that replaces a hidden one takes the hidden milestone
    with it** (the stored milestone becomes null, change `milestoneId`). The
    restricted caller cannot see or clear that milestone, and without this the
    project change failed with "unknown milestoneId", a field the caller never
    sent. An all-teams caller keeps today's `400 milestoneId is a milestone of
    project ...`.
  `POST /issues` needed no change: `checkPlanningRefs` and `cycleRef` were scoped
  in 2a. Its samples show it.
- **Already scoped in 2a, now with samples:** milestones, documents and their
  comments (`documentScope`, the scoped `GetComment` for `parentId`), cycles
  (`teamScope`, `cyclePathID` with the Linear id), project avatars and updates.
  The text "cycleId is a cycle of another team" stays: it is reachable only for a
  cycle the caller sees.
- **`CycleSnapshots` writes nothing for a hidden cycle.** The check runs before
  the lazy refresh. The Test B sample's `Snapshot` counts the cycle's snapshot
  rows; `TestPlanningAccess_CycleSnapshotsNoWrite` shows the admin's read of the
  same running cycle adds a row, so the count can move.
- **Test B fixture additions** (`addPlanningFixture`, called last in
  `newTeamFixture`). The fixture now has an object store (`storagetest`), so the
  avatar samples reach the project check. New objects: a project of ORB and HID
  (`Shared plan`) with a HID issue in it; a document with no project; a comment on
  the hidden document; an update on the hidden project; a product of HID only
  and one of ORB and HID (`mixed`); a Linear id of the hidden cycle. New marks:
  their names and ids. `f.hidden` gains `update` and `product`.
- **Test B harness:** a sample may set `Status` (both answers must have it)
  instead of 400. `placeAfter` and `placeBefore` use it: an unknown neighbour is
  `409 stale_placement` today (`rank.go`), and a hidden one is the same 409, byte
  for byte. They were written in 2b with 400 and could not pass.
- **Found on develop:** `GetChatEventStateIDs` and `ListChatStateNames`
  (`chat.sql`, LAUN-214) merged after 2b with no scope tag, so Test C failed on
  `develop`. Both are read by the chat sender, which has no caller: tagged
  `system`, with golden lines. `develop` now has the same fix; the merge
  keeps one copy.
- **Tests** (`team_access_planning_integration_test.go`): the shared project
  (teams and counts, read and list, admin and all-teams member); the #39 rules and
  the lead rule; #40 (null on hidden and visible, the same hidden id, the
  milestone that goes with a replaced project, the admin's old text); documents (a
  hidden one by id and slug, the open one, moving it into a project, a member with
  no team lists none); cycle snapshots; products (list, detail, team products,
  `?product=`); cross-org (another org's team, project, product in bodies, filters
  and paths). `TestPlanningSamples_Harness` shows the new list samples and
  Checks can fail. `TestTeamRules_CrossOrg` covers every new path.

- **Second commit (review of the first).**
  - The #40 fix above, with samples of two fields compared byte for byte (Test
    B `Also` samples of `PATCH /issues/{issue} projectId` and `milestoneId` on
    the fixture's `child`), and `TestHiddenAndMissingSamePath_PlanningPatch`,
    which compares status, body, headers and the statement trace for five
    planning `PATCH` bodies.
  - New Refs with samples: `addLabels` and `removeLabels` of `PATCH
    /issues/{issue}` (a hidden team label is `400 unknown label`, as a made-up
    one), and `product` of `GET /issues` (a hidden product's slug answers as a
    made-up slug). `GET /teams/{team}/products` has an `Also` sample: on
    `/teams/ORB/products` the mixed product's `teamKeys` is `[ORB]`.
  - Test C accepts the `product_teams` form of the #36 shape in
    `db/queries/products.sql` only (`ProductShapeOutside` in the `_Fails` demo).
  - The merge of `develop` (ticket 6a) brought Test D. Its check that an event
    spanning ORB and HID is all-of now looks at `relation.*` events only: the
    shared project's events of this fixture span both teams and are any-of
    (#54; the mixed product's `product.*` events are all-of). The
    fixture's hidden product is named "Secret product", because Test D makes a
    "Hidden product". The duplicate `chat.sql` tag fix was dropped: `develop`
    has it.
- **Open design question (not changed, for the project owner).** A restricted member may
  move a document with no project (org-wide, #38) into a project of their own
  team only. The document then disappears for every member who cannot see that
  team. #39 forbids the same move for a project (a restricted member cannot set
  the teams of a project with no team). Recommended default: leave it as it is
  until the project owner decides; the choices are to refuse the move for a restricted
  member, or to allow it only into a project with no team.

### Carried to later tickets

- 6a (merged before this ticket) scopes the feed. The planning events follow #54
  there; nothing is carried.
- The project update reminder (`project.update_due`) to a lead who cannot see the
  project: the consumer's access check is ticket 7 (#61). Until then such a lead
  would be notified; no member can be restricted before ticket 9 (#19).
- The activity row that the implicit milestone clear writes holds the hidden
  milestone's name (stored whole, #43). Its read-time redaction is ticket 4
  (LAUN-219). Done after ticket 4 merged: `TestPlanningAccess_MilestoneClearActivity`
  shows the row is null to null for the restricted member and names the hidden
  milestone for the admin.
- `docs/architecture.md` and `api/openapi.yaml` do not yet say that `teams`,
  `teamKeys` and `issueCounts` are of visible teams, nor list the new 400 texts:
  ticket 8 writes the enforced surface into the docs.
- The admin product routes (`PATCH`, `DELETE`, team membership) are `admin` rules;
  their `resolveProduct` is scoped, which changes nothing for an admin.
- `productView` (`products.go`) has no caller (it had none before this ticket).

### Not tested

- The `EXPLAIN` of the project and product lists with the #36 shape: not measured.
- The avatar `PUT` sample uses a made-up upload key; the test with a real
  uploaded key of the hidden project is `TestAvatars_HiddenProject` (2b).
- `TestHiddenAndMissingSamePath` (the statement trace) covers issues only; the
  planning paths rest on Test B's byte comparison.

## Build notes (LAUN-222, ticket 6b)

What ticket 6b built, and the choices the note left open. No numbered rule
changed. Every open choice takes the recommended default. No migration: the
columns of `0038` are enough. Admins and `all` members read the same feed as
before: the job only changes rows they already read (`legacy` to `any`,
`all` or `org`), and it never writes `admin`.

- **Where it lives.** `internal/events/backfill.go`. `cmd/server` starts
  `events.RunBackfill` once at every boot, after the other jobs (#57). It is
  not periodic: a roll-forward is a boot, so the rows an older image wrote
  after a rollback are placed then. The boot does not wait for it.
- **One instance.** A session try advisory lock
  (`hashtextextended('devops:events:backfill', 0)`) on its own connection, as
  the notifications retention job. A second instance gets no lock, logs one
  line and does nothing. A crashed run frees the lock with its session.
- **Batches, no long lock.** `ListLegacyEvents` reads up to 1 000 `legacy`
  rows after a `(org_id, seq)` cursor, in `events_pkey` order (all orgs in
  one walk). For each org in the batch the job reads the rows it needs (one
  query per table, `= ANY(ids)`, always with the event's `org_id`), and
  writes one `PlaceLegacyEvents` per distinct scope: `UPDATE ... WHERE seq =
  ANY(seqs) AND scope = 'legacy'`. No explicit transaction: each statement
  locks only its own rows, for milliseconds. 50 ms pause between full
  batches.
- **Resumable and idempotent.** Only `legacy` rows are read or written. A
  crash loses nothing: the next run reads the rows that are still `legacy`.
  A second run places nothing and changes no row. The job emits no event
  (the row count does not change) and sends no NOTIFY.
- **The rule (#54, build notes 6a), from the entity now, else the payload ids:**

  | Event | Placed by | When the entity is gone |
  |---|---|---|
  | `issue.*` | the issue's team | payload `teamId`, if that team of the org exists; an `issue.deleted` payload with `parentId` or `detachedChildren` (pre-6a shape) stays legacy |
  | `attachment.created` | the attachment's issue's team | payload `issueId`'s team |
  | `comment.*` | the issue's team, or the document's scope | payload `issueId`, else `documentId` |
  | `relation.*` | all-of both issues' teams | payload `issueId` and `relatedIssueId`; both issues must exist |
  | `team.*` | that team | legacy |
  | `cycle.*` | the cycle's team | payload `teamId` (created, completed, deleted) |
  | `project.*`, `project.update_due` | the project's teams now; `org` when none | legacy |
  | `project_update.created` | its project | payload `projectId` |
  | `milestone.*` | its project | payload `projectId` |
  | `document.*` | its project; `org` when it has none | payload `projectId` when not null |
  | `label.created` | `org` for an org label, the team for a team label | payload `teamId` |
  | `product.updated` | the product's teams now; `org` when none | legacy |
  | `product.created`, `.deleted`, `.teams_changed` | all-of every team the payload names (`teamIds`, `added`, `removed`); `org` when it names none | (same: the payload) |
  | `agent_guidance.updated` | `org` when the payload's `team` is null | a team entry stays legacy |
  | `chat_service.updated` | payload `teamId` | legacy |
  | org-level kinds (`project_status.*`, `project_label.*`, `org.updated`, `onboarding.step_changed`, `git_identity.removed`, `slack.updated`, `push.updated`) | `org` | (always org) |
  | any other type | legacy | legacy |

  Every placed scope also passes `checkScope` (the 6a rule table); a scope
  the type does not allow stays legacy.
- **Choices.**
  - A project or a product that is gone is NOT org-level: its teams are
    unknown, so its rows stay legacy. Only an existing project with no team
    is `org` (#36).
  - A gone relation needs BOTH issues: with one issue gone its team is
    unknown, and an all-of scope with one team would show the row to that
    team alone.
  - A payload `teamId` places a row only when that team of the org still
    exists. A deleted team's rows stay legacy (no restricted member could be
    in a deleted team, so the result is the same for readers).
  - Product payload lists are kept whole, deleted teams included: a gone
    team in an all-of scope only makes it stricter.
  - A team guidance event names a team KEY, and a key can be reused by a new
    team after a delete: legacy, not looked up by key.
  - A deleted document whose payload `projectId` is null stays legacy: the
    null said "no project" at create time, not now.
  - Payload ids that are malformed, the nil id, or a payload that is not an
    object place nothing.
  - The scope is the entity's NOW (#57), not the one at the time of the
    event: a project that changed teams gives its rows the teams it has now.
- **Lookups in rounds.** `place` asks for the rows it needs; an id not read
  yet is "wanted" and the batch is placed again after one query per table.
  A result counts only from a round that wanted nothing. A comment needs at
  most three rounds (the comment, its document, the document's project); the
  job stops after six and then leaves the batch legacy (fail closed; not
  reachable with these rules).
- **Logs.** One line per boot with `scanned`, `placed`, `legacy` and
  `batches` (none when nothing was legacy), or the error with the counts.
  Never a payload, an id or a name. `TestBackfill_LogsCountsOnly` checks it.
- **Queries.** `ListLegacyEvents`, `PlaceLegacyEvents` and twelve
  `Backfill*` lookups in `db/queries/events.sql`, all `system` (no caller)
  and in the golden file. None is called from `internal/api`.
- **Tests.**
  - `TestBackfillPlace` (unit): every type, the entity case, each payload
    fallback, each orphan and malformed payload. `TestBackfillCoversEveryType`
    fails when an event type has no rule. `TestBackfillWantsBeforeDeciding`.
  - `internal/events/backfill_integration_test.go`: `TestBackfill_Batches`
    (2 400 rows in two orgs, batches of 100, orphans stay legacy, the row
    count does not change); `TestBackfill_CrashAndResume` (a hook fails after
    batch 2: 200 rows placed, the rest legacy; the next run places the rest;
    a cancelled run places nothing); `TestBackfill_Rerun` (a second run
    changes no row; a legacy row written later is placed by the next run);
    `TestBackfill_AdvisoryLock` (a held lock: no work; two runs at once place
    each row once between them); `TestBackfill_CrossOrg` (an entity or a
    payload team of another org places nothing); `TestBackfill_Feed` (the
    `ListEventsAfter` predicate before and after: a restricted member reads
    no legacy row before, its team's and the org rows after, never an
    orphan; admin and all-teams feeds unchanged); `TestBackfill_LogsCountsOnly`.
  - `TestEventsBackfill_RoundTrip` (api): the Test D writes (now the shared
    helper `makeEventKinds`) make every kind through the routes; every row is
    reset to legacy; before the job the restricted member's `GET /events` is
    empty; after it, each row has exactly the scope Emit gave it or stays
    legacy (never another scope), every kind of `hiddenKinds` with a live
    subject is placed with HID, the kinds left legacy are exactly the
    expected ones (GON's create and delete, the deleted project's create and
    delete, the edge to the deleted issue, the deleted product's update, the
    team guidance), the restricted feed is exactly the #55 rule over the
    placed rows, and the admin's and the all-teams member's feeds do not
    change.

### Review fix (LAUN-222, second commit)

- **M1: old `issue.deleted` payloads.** Rows written before 6a hold
  `parentId` and `detachedChildren` (6a removed both from new writes, #42:
  the parent or the children may be in a hidden team). The first commit
  placed such a row with the payload's team, so a restricted member of that
  team would read the hidden parent id and the child count. Now an
  `issue.deleted` row whose payload has either key (any value, null too)
  stays `legacy` (fail closed). Other types are unchanged. Tests: unit cases
  in `TestBackfillPlace`, and `TestBackfill_OldIssueDeletedShape` (the old
  shapes stay legacy and hidden from a restricted member; the current shape
  is placed and read).

### Carried to later tickets

- Rows placed below a reader's cursor are not sent again. A restricted
  reader whose cursor moved past legacy rows (the high-water mark of 6a)
  does not get them when the job places them; the feed is a change feed and
  clients refetch state. A reader from seq 0 reads them. Accepted.
- Every boot scans `events` for `legacy` rows, also when only orphans are
  left: `ListLegacyEvents` filters `events_pkey`, about 31 ms per call at
  1 000 000 rows (review measure). A partial index on `scope = 'legacy'`
  would make it free but needs a migration; not done. The run time of a
  full backfill of a large history is not measured ("Not measured").
- Residual, next to the one of #54: the job uses the project's teams and the
  document's project as they are NOW. An old `project.created`,
  `milestone.created` or `document.created` row (with an old name and an
  old project id) can become readable to a team the project gained later,
  and to no team it lost. Accepted: the object itself is readable to that
  team now.
- Residual: a project delete turns its documents org-level (#38), so the old
  rows of those documents (and of their comments) get scope `org`. Accepted:
  the documents themselves are org-wide now.
- Orphans stay legacy for ever: admins and `all` members read them, a
  restricted member never does. No job turns them into `admin` rows.
- The rows the job places use the scope of the entity now, so the residual
  of #54 (a scope frozen at emit time) does not apply to them; it applies to
  every row Emit writes.
- `member.access_changed` (ticket 9) is not placed: an older image never
  writes it.

## Build notes (LAUN-223, ticket 7)

What ticket 7 built, and the choices the note left open. No numbered rule
changed. Every choice takes the recommended default. It was built on ticket 5
(LAUN-220, not merged yet), which holds tickets 4 and 6a. Admins and `all`
members get the same notifications, mails, settings and inbox as before.

- **Rules flipped to `Enforced`** (`internal/api/teamrules.go`): `GET` and
  `PUT /me/notification-settings`, `PUT /me/notification-settings/teams/{team}`,
  `PUT` and `DELETE /me/teams/{team}/follow`, `GET`, `PUT` and `DELETE
  /issues/{issue}/subscription`, and the five inbox routes (`GET
  /notifications`, `/unread-count`, `/stream`, `POST /read-all`, `PATCH
  /notifications/{id}`). 13 rules. Each has a Test B sample
  (`teamrules_notify_integration_test.go`, joined to `teamRuleSamples` in
  `init`), and all pass. 68 of 205 rules are `Enforced` on this branch.
- **The access rule of a person, in SQL.** The consumer and the sender have no
  caller, so they read the recipient's access in the query, as
  `ListOrgsForUser` does (ticket 3): `m.role = 'admin' OR m.team_access = 'all'
  OR` a `team_members` row for the team. Any other flag fails closed. A project
  uses the #36 rule: no team is org-level, else one of its teams.
- **Consumer (#61).** `ListNotifyEligibleUsers` takes the issue's team (read
  from the issue row, not the payload). Every candidate passes it: assignee,
  creator, commenter, subscriber, follower (`new_issue`) and mention. So a user
  without access gets no notification of any kind, no mail row, no push row.
  `ListMentionableUsers` returns `sees_team`; only a member who sees the team
  becomes a candidate, so a mention makes no mention subscription for a member
  without access (#45). The reminder (`project.update_due`) uses
  `ListNotifyEligibleProjectUsers`: a lead who cannot see the project gets
  nothing (#46 lets such a lead exist).
- **Chat services are unchanged** (a shared channel is not a person; no rule of
  this note names chat). The consumer keeps the old rule for them in a new
  query, `ListActiveHumanMembers` (active, human, a member; no team test): the
  "an active member muted the issue" check, the "a member was newly mentioned"
  check, and the "the reminder's lead is active" check. A mention of a member
  without access still posts the `mention` kind, and the reminder of a project
  led by such a member still posts. `TestChat_TeamAccessDoesNotApply` shows it.
- **Chat mention newness (review commit).** The chat used to ask the
  mentioned person's notification rows whether a mention was new. Those rows
  exist only for a person who sees the team, so a member without access was
  "new" on every edit, and a member who reads the channel could count posts and
  learn who sees a team (#33, #45, #46). Now the chat keeps its own record,
  `chat_mentions (org_id, dedupe_key, event_seq)` (migration `0040_chat_mentions`,
  down drops it), claimed by `ClaimChatMention` (`system`): the first event
  that names a mention key claims it; a replay of that event gets its own seq
  back and decides the same way. The key is the target id and the user id: no
  text, no address. The person's notification keeps its own dedupe
  (`mentionIsNew`). `TestChat_MentionPostsDoNotDependOnAccess`: the same edits
  post one mention for a member with access and one for a member without.
  The note's "Not done" item about the Slack channel stays open.
- **Mail sender (#63).** After the recipient checks and the issue or project
  read, before the preferences, `MailRecipientSeesTeam` (issue) or
  `MailRecipientSeesProject` (reminder) re-reads the access. Without it, every
  row of the group is `skipped` with `access_removed`, and no mail is built, so
  no title of a hidden issue or name of a hidden project is sent. A recipient
  who is gone or deactivated is still `disabled` (that check comes first).
- **Migration M2b is `0039_mail_access_removed`.** It replaces the CHECK
  `notification_emails_skip_reason_check` with one that adds `access_removed`.
  The down rewrites such rows to `disabled` and puts the old CHECK back.
  `TestMigration0039_MailAccessRemoved` and `TestMigrateUpDownUp` cover it.
- **Settings view and writes (#64).** `notify.Service.View`, `UpdateGlobal` and
  `UpdateTeam` take the `*auth.Org`. The view lists teams with the scoped
  `ListTeams` (the `ListTeamsSystem` stopgap of ticket 3 is gone from it; the
  git webhook still uses it), and `ListTeamFollows` and
  `ListNotificationSettings` are tagged `team` (their `pending` lines left the
  golden file). A row of a hidden team stays stored and is not shown. The mail
  sender and the unsubscribe route read a new `ListNotificationSettingsSystem`.
- **The `/me` team routes and the subscription routes** already resolved their
  team or issue with the scoped resolvers (2a). They now have samples: a hidden
  team or issue is the 404 of a made-up one, byte for byte, and the caller's
  rows do not change.
- **Inbox stream (#56, #62).** `streamInbox` follows `streamEvents`: before
  every page of every drain (the first one, a wake-up and a poll tick) it calls
  `StillAuthorized`. Credentials gone: the stream ends with `: credentials no
  longer valid`; a missing membership row is never read as an empty set. Role
  or access changed: it sends `event: access` with `data: {}` and no `id`, then
  reads the rows and the count with the new access. The poll tick has no check
  of its own any more. The inbox queries were scoped in 2a. The stream sends
  only rows after its cursor: when access grants a team, that team's older
  rows (below the cursor) are not sent, so the web must refetch the inbox and
  the count on `event: access` (ticket 11b; the openapi text says so).
- **The cleanup helper (#50)** is `tracker.RemoveLostTeamNotifyRows(ctx, q, org,
  user, lost)`, with three queries tagged `admin`
  (`DeleteMemberSubscriptionsInTeams`, `DeleteMemberTeamFollowsInTeams`,
  `DeleteMemberTeamSettingsInTeams`). It lives in `tracker`, not `notify`, so
  the setter of ticket 9 can call it in its transaction without importing
  `internal/notify` (AGENTS.md). It has no caller yet. Hard deletes, no event;
  inbox rows stay (#51).
- **Push.** The wake carries no text (decisions-T35); the app then reads the
  inbox, which is scoped. A push row made before access was removed is still
  sent as a bare wake. Not changed.
- **Unsubscribe.** It names one user's settings by token and answers `204`
  always. A token of a team the user lost still writes that user's own email
  `off` row for the team; the row is not shown (#64) and names nothing.
- **openapi.** Descriptions only: the settings view lists visible teams, the
  inbox and its count leave out hidden rows, the stream sends `event: access`.
  `web/src/api/schema.d.ts` is regenerated.
- **Tests.** notify: `TestConsume_TeamAccess` (every kind; a member restricted
  to no team, one restricted to the team, an `all` member, an admin whose flag
  says `selected`, an agent; no mention subscription; access given back works
  again; the logs hold no text), `TestConsume_TeamAccess_ProjectDue`,
  `TestSender_AccessRemoved` (issue and reminder; the mail of a member who
  still sees it is sent), `TestChat_TeamAccessDoesNotApply`. tracker:
  `TestRemoveLostTeamNotifyRows`. api: Test B samples; `TestNotifySamples_Harness`
  (with the member made `all`, the lists hold team B and the Checks fail; the
  admin and an `all` member use the routes as before);
  `TestInboxStream_AccessChange` (a team added: `access`, then the rows that
  are now visible and the new count; removed: `access`, the count drops and a
  new hidden row is not sent; a visible row still comes; a removed membership
  ends the stream); `TestInboxStream_AccessEventOnlyOnChange`;
  `TestNotifyRoutes_CrossOrg` (another org's team and issue in the `/me` and
  subscription paths answer as unknown ones). database:
  `TestMigration0039_MailAccessRemoved`. `TestTeamRules_CrossOrg` covers every
  flipped path.
- **Mutation check (manual, not committed).** With the consumer's final filter
  on the old rule, the sender check off, the inbox stream reading with the
  access of the request, or `ListTeamFollows` unscoped, the tests above fail.

### Carried to later tickets

- Ticket 9 calls `tracker.RemoveLostTeamNotifyRows` in the setter's
  transaction for the teams a member loses (#50), and its tests check the
  rows through the setter.
- The Slack and chat services post events of every team whatever a member's
  access (the "Not done" item). A per-team filter or an admin warning is still
  open before the feature counts as done for chat.
- `chat_mentions` has no retention job (one small row per mention key). A
  prune with the notification retention is a follow-up if it grows.
- A push wake made before access was removed is still sent (no text). A
  `access_removed` skip for push needs a migration of `push_outbox`; not done.
- `UpdateTeam` (team settings) resolves the team before its transaction: an
  access removed between the two still writes the caller's own setting row of
  that team (never shown, names nothing). The read-merge reads the caller's
  rows with `ListNotificationSettingsSystem` (review commit), so the column the
  body did not name is kept (`TestSettings_TeamWriteAfterAccessRemoved`).
- `docs/architecture.md` and the web (`access` on the inbox stream,
  `web/src/api/events.ts` and the inbox client) are tickets 8 and 11b.

### Not tested

- The inbox stream's poll path (only a NOTIFY wakes it in the test; the event
  stream test covers the shared poll code of 6a, not this handler's).
- The plan and time of the eligibility queries and the inbox reads with the
  predicate (Not measured).
- The push sender after access removed (no change was made there).

## Build notes (LAUN-224, ticket 8)

What ticket 8 built, and the choices the note left open. No numbered rule
changed. Every choice takes the recommended default (point M: option c, the
guard of #65). Admins and `all` members get the same answers as before, with
one exception for every caller: a git webhook's answer (below). No migration.

- **Every rule is `Enforced`** (`internal/api/teamrules.go`). `adminRule()` and
  `noneRule()` (and `webhookRule()`) now build `Enforced` rules: an `admin` rule
  is enforced by the org role (Test B asserts `403 forbidden` on each), a `none`
  rule reads no team row (its reason says why). The last `own` rules (`GET
  /me/views`, `PUT /me/views/{place}`, `GET /me/git-accounts`) have Test B
  samples (`team_access_webhooks_integration_test.go`). Five `/me` routes moved
  from `own` to `none`: `PUT` and `DELETE /me/git-accounts/{integration}` and
  `GET`, `PUT` and `DELETE /me/push-devices[/{device}]`. Each names one row of
  the caller's own (a handle on one integration, a device) and no team row; the
  writes answer `404` or `204` for a made-up id, and `GET /me/push-devices`
  answers `404` while push is off, so the `own` check (200 and no mark) does not
  fit them. A new unit test, `TestTeamRules_AllEnforced`, fails on any rule that
  is not `Enforced`, so a new route must arrive enforced with its samples, or as
  `admin` or `none` with a reason. 205 of 205 rules are `Enforced`.
- **#19 is now met by the rule table.** `TestNoSelectedWriterWhileUnenforced`
  passes with any writer. Ticket 9 adds the setter; nothing in this ticket writes
  `team_access` or `team_members` (the guard only reads them).
  `TestNoSelectedWriterWhileUnenforced_Fails` builds its open rule with
  `listRule`, as `noneRule` is now enforced.
- **The webhook answer is `{"status": ...}` only (#65).** `tracker.WebhookResult`
  lost `linked` and `moved`; `api/openapi.yaml` has `required: [status]`. The
  status depends on the delivery (`duplicate`), the map (`unmapped`) and the
  event's plan only: an event that asks for something (a link, a move, a
  refresh, a review, a check) answers `processed` whether or not an issue
  matched, a link existed or the check was older than the stored one; an event
  that asks for nothing (a ping, a fork, another action, an unknown check state)
  answers `ignored` (`answerStatus`). This is the one change for every caller:
  before, a review, refresh or check of an MR with no link, and a stale check,
  answered `ignored`; now `processed`. Reason: the old answer told a repository's
  sender whether an earlier event had matched an issue, a hidden one included.
  What the event did (`processed` or `ignored`) is kept in `WebhookResult.Outcome`
  (`json:"-"`), which the deliveries ring stores (`gitDelivery.reply`), and in
  the `git_webhook_events` row; both are admin only. The backfill (an admin
  route) still reports `linked` and `moved` counts, from an internal `scmTally`.
- **The match uses the repo map's team.** It did already (`linkAndMove` keeps
  only the mapped team's key and issues, `onlyTeam`). Tests now show it for
  GitLab and GitHub against a team no member but the admin sees.
- **The guard (#65): `tracker.CheckWebhookGuard(ctx, q, orgID)`** and
  `tracker.LockTeamAccessGuard` (`internal/tracker/webhook_guard.go`). Three
  queries in `db/queries/git.sql`: `LockTeamAccessGuard` (`none`:
  `pg_advisory_xact_lock(hashtextextended('launchpad-team-access:' || org_id, 0))`),
  `RestrictedAccessExists` and `ListEnabledReposWithoutTeam` (`admin`, golden
  lines added). The rule is checked on the state the write leaves: restricted
  access exists AND an enabled integration has a map row with `team_id` NULL
  gives `409 conflict` naming up to 20 repository paths (an admin route; the
  paths are admin data). No new error code (#22). Each entry takes the lock first
  in its transaction, writes, then calls the check before it emits; the lock
  makes two entries run one after the other, so the second reads the first's
  commit (READ COMMITTED: each statement takes a new snapshot after the lock).
  - "Restricted access" counts every `org_members` row with `team_access =
    'selected'`, whatever the role (fail closed: an admin's flag is ignored for
    reads, #2, but a role change would make it count). Ticket 10 adds open
    `selected` invites.
  - Entries wired: 5 (`CreateGitIntegration`, which today has no repository
    and passes; `UpdateGitIntegration` when it enables), 6 (`insertGitRepo`
    for both modes, `UpdateGitRepo`), 7 (`DeleteTeam`: the FK sets the team's
    repositories to NULL; the check runs after the delete, so it sees them).
    Token mode also checks before the host call (`precheckRepoWithoutTeam`, no
    lock), so a refused map makes no hook; the write checks again under the lock.
  - A map with no team on a disabled integration is allowed: it matches nothing
    until the integration is enabled, and the enable is refused.
  - Entries 1 to 4 (setter, invites, approval, agents) do not exist yet; tickets
    9 and 10 call the helper in their transactions.
- **Tests (api):** `TestWebhookTeamAccess_GitlabStatusOnly` (three opens naming
  a hidden issue of the mapped team, an issue of a visible other team and a
  missing one: the same body `{"status":"processed"}`; the hidden one is linked
  and moved, the other untouched; every event of the deliveries is scoped `any`
  to HID; a review of a linked and an unlinked MR give the same body; the ring
  says `processed` and `ignored`), `TestWebhookTeamAccess_GithubStatusOnly`,
  `TestWebhookGuard_Entries` (entries 5 to 7, each refused with no row changed
  while a member is restricted, and each allowed with no restricted member),
  `TestWebhookGuard_TokenModeMakesNoHook`, `TestWebhookGuard_CrossOrg` (another
  org's team in a map is `400`; a restricted member of another org does not
  block this org). The GitLab, GitHub and Bitbucket tests that read `.Linked`
  and `.Moved` now read them from the database (`hookDiff`: a new link
  attachment, a changed state), and the tests that expected `ignored` for a
  stale or unlinked check or update expect `processed` (the ring still says
  `ignored`, `TestChecks_NoLinkWritesNoRingRow`).
- **MCP (#66):** `internal/mcp/team_access_integration_test.go`.
  `TestIntegration_RestrictedToken` runs all 24 tools of the full profile with
  the token of a member who sees ORB only (team HID holds an issue in a project,
  milestone and team label, a comment, a link attachment, a project update, a
  document, a cycle, a product and guidance): 42 pairs (a hidden reference and a
  made-up one) give the same text and error flag after the references are
  replaced, 9 lists hold no mark of HID, no row changes, and every catalogue tool
  has a case. The cases are shown to be real: the admin's token reads HID through
  the lists, and every read pair answers the admin differently for hidden and
  made-up. `LAUNCHPAD_TEAM=HID` answers as `LAUNCHPAD_TEAM=ZZZ` (unknown team).
  `TestIntegration_RestrictedReviewer`: the three reviewer tools. No MCP code
  changed; the golden files are unchanged.
- **Import (#67):** `TestRun_aRestrictedMemberDoesNotSeeTheImportedTeam`
  (importer) and `TestRun_RestrictedMemberDoesNotSeeImportedTeam` (importjob,
  the web job end to end). The org's team is renamed so the import creates ORB.
  A restricted member, a mapped one (claude) and one not in the user map, sees
  neither the new team nor its issues and projects; the access rows of every
  member are unchanged by the run; an all-teams member sees the team; once an
  admin adds ORB the restricted member sees it. No importer code changed.
- **Docs:** `docs/architecture.md` (a "Team access" section, the webhooks, MCP
  and importer lines), `docs/mcp.md` (team access), the "superseded by T20 #65"
  lines in `decisions-T5.md` (#11) and `decisions-T17-git-connections-import.md`
  (#7 of the webhook part), and `api/openapi.yaml` (the team access rule in
  the intro; `defaultTeam` null when hidden; `teamKeys`, `teams` and
  `issueCounts` of visible teams; the guard's `409` on the repo map, the
  integration enable and the team delete; `WebhookResult`).
  `web/src/api/schema.d.ts` is regenerated.

### Carried to later tickets

- Ticket 9 (setter, agents) and ticket 10 (invites, approval) call
  `LockTeamAccessGuard` first and `CheckWebhookGuard` before they emit (entries
  1 to 4). Ticket 10 adds open `selected` invites to `RestrictedAccessExists`.
- The Slack and chat services still post events of every team (the "Not done"
  item). Not changed here.
- Ticket 10 calls `CheckWebhookGuardNameless` inside `AcceptInvite` (the
  invitee is not an org admin; repository paths are admin data). Admin routes
  keep `CheckWebhookGuard`, which names the repositories.
- Residual of #65 (as the note says): a person who can push to a repository
  mapped to team T can link and move issues of T without seeing T.
- Time is not equal: a hidden and a missing issue run different statements in
  the webhook (a link writes rows). The answer bytes are equal; the time to
  answer is not claimed equal.

### Review commit (LAUN-224)

- **A resent check answered by its link (fixed).** A check delivery for a sha
  no link has keeps no delivery row (LAUN-97), so its resend was `processed`,
  while the resend of a check that found a link was `duplicate`. Repository
  maintainers and admins can resend a delivery from the host (GitHub's
  Redeliver, GitLab's Resend keep the delivery key), so this told them whether
  an issue was linked. Now a duplicate of a check event answers `processed`
  (`answerStatus`); the ring and `WebhookResult.Outcome` still say
  `duplicate`. Every other event writes its delivery row before it reads an
  issue, so its duplicate answer depends on the delivery only and stays
  `duplicate`; an unmapped event still keeps no row and is applied when sent
  again after a map (decisions-T5 #26). Tests:
  `TestWebhookTeamAccess_GitlabResendSameAnswer` and
  `..._GithubResendSameAnswer` (a sha linked to a hidden issue, a sha of a
  visible issue of another team, a sha of a missing issue: each delivery and
  its resend give the same status and body). The three check tests that
  wanted `duplicate` now want `processed` and a `duplicate` ring row.
- **The backfill (fixed).** A backfill acts with system authority and did not
  look at `enabled`. The guard lets a disabled integration keep a repository
  with no team, so a backfill could link and move issues of every team. Now
  `BackfillGitRepo` answers `409 conflict` on a disabled integration, and on a
  repository with no team while restricted access exists; each change's
  transaction checks again under the guard's lock (`applyStandalone`).
  `TestWebhookGuard_Backfill`: both cases link, move and emit nothing, and a
  repository with a team runs.
- **Close, review, refresh and check follow the mapped team (fixed).**
  `updateLinks` and the check path wrote links of any team: a link made before
  the repository was mapped to another team still changed, with activity and
  `issue.updated`. `lockLinkedIssues` now takes the mapped team and keeps only
  its issues (it still takes the rank locks of every team it reads, in the
  same order). `TestWebhookTeamAccess_RemapLeavesOldLinks`: after a remap a
  check, a review and a close leave the old link and emit nothing; after the
  map comes back the close applies. Accepted effect: such a link keeps its
  last state until the map names its team again.
- **The guard's text.** `CheckWebhookGuardNameless` refuses without naming a
  repository (`TestWebhookGuard_Nameless`).
- **Docs.** The importer line of `docs/architecture.md` no longer splits the
  list item; the scope golden file is in order.

### Not tested

- Two concurrent guard entries (a map with no team and a restriction) racing
  under the lock: the lock is shown by reasoning, not by a race test, because
  no route can make a restricted member yet. Ticket 9 can add one with the
  setter.
- Bitbucket's answer for a hidden versus a missing issue (it shares
  `HandleSCMEvent` and `answerStatus` with GitHub and GitLab, which are tested).
- The MCP `list_users` `team` argument: the roster is org-wide (#7), so a
  hidden and a made-up team give the same full roster to any caller; there is
  no pair for it, only the list case.

## Build notes (LAUN-225, ticket 9)

What ticket 9 built, and the choices the note left open. No numbered rule
changed. Every choice takes the recommended default (point E: the admin must
choose, the web default is unassign; point B: zero teams on an edit). This
is the first write that makes a member restricted. No migration: the
columns of M1 are enough. Admins and `all` members get the same answers as
before; `GET /users` gains two fields that are null for a non-admin.

- **The route.** `PUT /orgs/{org}/users/{user}/team-access` (admin scope,
  `s.idem` after the scope check; the middleware acts on POST only, so a key
  on this PUT is ignored and nothing is stored). `{user}` is a user id or a
  username (lower-cased). The body is `{teamAccess, teams, assignedIssues}`
  (`TeamAccessInput`, unknown fields refused); `?dryRun=true` (only `true`
  or `false`). Rule `adminRule()` in `teamrules.go`: Test B asserts `403`
  for a restricted member, Test A and the cross-org walk cover it.
- **Refusals, in this order.** Body: `teamAccess` not `all` or `selected`,
  `teams` with `all`, more than 50 teams, a bad `assignedIssues` or `dryRun`
  (400), no JSON (415). Then, in the transaction: an unknown user or a user
  who is not a member of this org (404, so another org's admin and another
  org's user are both 404); an org admin target, the caller included (400:
  admins always see all, #2; so the last admin can never lose anything and
  self-demotion cannot happen here, because no route changes a role); an
  unknown team or another org's team (400 `unknown team`); open issues in a
  lost team without a choice (409, the text gives `KEY: n` per team, an admin
  route); the webhook guard (409 naming the repositories).
- **One transaction.** `tracker.SetTeamAccess`: `LockTeamAccessGuard` first
  (entry 1 of #65), the member row `FOR UPDATE` (`GetMemberForAccess`), the
  teams, the counts, the writes (`SetMemberTeamAccess`,
  `DeleteMemberTeamsExcept`, `InsertMemberTeams`), the unassign, then
  `RemoveLostTeamNotifyRows` (#50), then `CheckWebhookGuard` when the member
  ends `selected` (setting `all` restricts nobody and is never refused),
  then the events, last.
- **Lost and added teams.** "Before" is every team of the org (archived
  included) for an `all` member, else the member's rows. "After" is the same
  for `all`, else the body's set. Lost = before minus after. Setting `all`
  deletes the member's `team_members` rows (they count only for `selected`,
  #1). An archived team may be selected (#6).
- **Assigned issues (#49).** Open = state type not `completed` or
  `canceled` (triage counts as open; archived issues count too). `unassign`
  clears only open issues of the lost teams (`UnassignOpenIssuesInTeams`, rows
  locked in id order as `RolloverIssues`), bumps `version`, writes an
  `assignee` activity row with source `system` and the admin as actor, and
  emits one `issue.updated` per issue (scope: the issue's team; payload as a
  PATCH's plus `reason: access_removed`). `keep` changes nothing.
  `hiddenAssigned` (#48) counts the target's open issues in teams the target
  will not see after the change (the target only), whatever the choice.
- **Dry run.** The same transaction, rolled back at the end (a sentinel
  error), so the counts, `unassigned` and `removed` are exact and the guard's
  409 shows. It does not refuse a missing `assignedIssues`: the counts are
  the answer. No event, no audit line.
- **The answer** (`TeamAccessResult`) holds ids and counts only: `userId`,
  `teamAccess`, `teamIds`, `dryRun`, `changed`, `addedTeamIds`,
  `lostTeamIds`, `assignedInLostTeams` (`teamId`, `openIssues`),
  `unassigned`, `hiddenAssigned`, `removed` (`subscriptions`, `follows`,
  `settings`).
- **Idempotent.** A call that changes no flag, no team row, no assignee and no
  notification row writes nothing, emits nothing, logs no audit line, and
  answers `changed: false`.
- **Event.** `member.access_changed`, entity type `member`, the member's id,
  the admin as actor, scope `admin` (a new `kindScopes` entry; only `admin`
  is allowed). Payload keys: `userId`, `teamAccess`, `teamIds`,
  `addedTeamIds`, `lostTeamIds`, `unassigned`.
- **Audit line.** `audit: team access changed` with the org slug, the actor's
  username and token kind, the member's id, the access kind and counts. No
  IP, no User-Agent, no team key.
- **Member fields (#33).** `Member.teamAccess` and `Member.teams` (keys,
  sorted by key) are filled for an org admin caller only (null otherwise, the
  caller's own row too). An org admin member is always `all` with `[]`;
  a flag other than `all` reads as `selected` (fail closed). The keys come
  from `ListOrgMemberTeamKeys`, read only for an admin.
- **Agents (#31).** `AgentInput` gains `teamAccess` (default `all`) and
  `teams` (required and non-empty for `selected`, refused with `all`). A
  `selected` agent takes the guard's lock first and runs `CheckWebhookGuard`
  after its writes (entry 4 of #65). The answer shows the access. Creating
  an agent still emits no event.
- **Queries.** `db/queries/team_access.sql`, all tagged `admin` with golden
  lines; `ListOrgMembers` also returns `team_access`.
- **#19.** The setter's `teamAccess` in `api/openapi.yaml` and the SQL that
  writes `team_access` and `team_members` are writers;
  `TestNoSelectedWriterWhileUnenforced` passes because every rule is
  enforced (ticket 8).
- **Docs.** The rollback runbook of #35 in `docs/deploy.md` ("Rollback past
  team access"), `docs/architecture.md`, `api/openapi.yaml` (the route, the
  schemas, `member.access_changed` and entity type `member` in the event
  enums); `web/src/api/schema.d.ts` is regenerated.
- **Web (small, forced by tests).** #60 said the web needs no handler for
  `member.access_changed`. `web/src/api/events.test.ts` keeps `EVENT_TYPES`
  equal to the server's event list, so the type is listed, and its handler
  refreshes the member list (only admins receive it). `mockApi.ts` members
  gain the two fields. The members page and the dialogs stay ticket 11a.
- **Tests** (`internal/api/team_access_setter_integration_test.go`):
  `TestTeamAccessSetter_RoundTrip` (set `selected` through the route: the
  member's next reads of a hidden issue answer as a missing one, lists, teams
  and the feed leave HID out, the open stream sends `access` and then only
  visible events; the event is admin-only, its payload keys are fixed and
  name nothing; the admin and an `all` member read as before; one audit line;
  back to `all` restores access and sends `access`; zero teams works),
  `_MemberFields`, `_Refusals` (member, restricted member, agent with every
  scope, agent fixture token: 403; the only admin on itself and another
  admin: 400 and unchanged; ten bad bodies; unknown, made-up and non-member
  users: 404; 415; no row and no event changes), `_CrossOrg`,
  `_AssignedIssues` (409 with `HID: 2`; dry runs with and without
  `unassign`; `keep`; `unassign` clears the two open HID issues only, not the
  done, canceled or ORB ones, with system activity and two scoped events, and
  `member.access_changed` last), `_NotifyCleanup` (one of each removed, the
  ORB rows and another member's rows stay, inbox rows stay and are hidden),
  `_Idempotent` (repeats with the same key, another key and none: `changed`
  false, no replay header, no idempotency row, no event, one audit line),
  `_WebhookGuard` (entries 1 and 4: refused and dry run refused with the
  repository named, nothing written; `all` allowed; a disabled integration
  allows the setter and its enable is then refused; with a team everything
  passes and clearing it is refused), `_Concurrent` (two setters raced 8
  times: the stored set is one of the two, never a mix; a setter raced
  against clearing a repository's team 8 times: exactly one passes, and
  restricted access never exists next to a repository with no team, the race
  ticket 8 could not test), `TestAgents_TeamAccess`.

### Review commit (LAUN-225)

- **A legacy `member.access_changed` row (fixed, fail closed).** Such a row
  can exist after a manual migration down past `0038` and up again (the
  column default is `legacy`), and the feed gave legacy rows to every
  unrestricted member: an `all` member would have read who is restricted to
  which team ids. Two fences: `ListEventsAfter` gives a legacy row of this
  type to admins only (`AND (is_admin OR NOT (scope = 'legacy' AND type =
  'member.access_changed'))`, outside the #55 shape, so Test C's shape check
  is unchanged), and the backfill places it as `admin` by type, whatever its
  entity or payload. This is the one type the job writes with the `admin`
  scope (the 6b note said it never writes `admin`; this amends it).
  Tests: `TestBackfillPlace` cases, `TestBackfill_MemberAccessChangedStaysAdmin`
  (query level, before and after the job, admin, `all` and restricted
  readers) and `TestTeamAccessSetter_LegacyEventStaysAdmin` (through `GET
  /events`).
- **Runbook.** The "Rollback past team access" section moved after the
  earlier rollback section's last sentence, and states the cut-off: images
  from LAUN-216 on read the flag, but only images from LAUN-224 on enforce it
  on every route, so a rollback to an older image needs all four steps.
- **openapi.** The route says an `Idempotency-Key` is accepted and ignored
  on this PUT (the shared parameter text promises a replay).
- **Test.** The 403 loop also names a made-up user id and username: a
  non-admin gets 403 whether or not the user exists.

### Carried to later tickets

- Ticket 10: invites and sign-up approval carry access (entries 2 and 3 of
  #65), `RestrictedAccessExists` counts open `selected` invites, and the
  `org_invites.team_access` column the runbook's step 2 names.
- Ticket 11a: the members page uses the setter (`dryRun` first, "Unassign"
  as the default choice) and shows `teamAccess` and `teams`; ticket 11b the
  agent form's control and the `access` SSE handler.
- The Slack and chat services still post events of every team (the "Not
  done" item).
- An assignment of the member to an issue of a lost team that commits during
  or after the setter's transaction is not unassigned by it (`PATCH /issues`
  does not take the guard's lock). It is allowed (#46), and the next setter
  call or dry run counts it in `hiddenAssigned`. Accepted.

### Not tested

- A setter racing a concurrent issue assignment (the residual above).
- The audit line under a web session (`via` is the token kind; tested with a
  bearer token only).
- The `EXPLAIN` of the setter's queries (one member, small sets; not
  measured).

## Build notes (LAUN-226, ticket 10)

What ticket 10 built, and the choices the note left open. No numbered rule
changed. Every choice takes the recommended default (point A: a new invite,
approval or agent is `all` when the field is absent; point B: an invite and
an approval need one team). Admins and `all` members get the same answers as
before; `Invite` gains two fields.

- **Migration `0041_invite_team_access` (M3).** `org_invites.team_access`
  (`all` default, CHECK `all`/`selected`), CHECK
  `org_invites_admin_all_teams` (an admin invite is always `all`, #2, #30),
  `UNIQUE (org_id, id)` for the composite FK, and `org_invite_teams (org_id,
  invite_id, team_id)` with composite FKs to `org_invites` and `teams`, both
  `ON DELETE CASCADE`, and an index on `(org_id, team_id)`. The down FAILS
  (`RAISE EXCEPTION`, the whole file rolls back) while an invite with
  `team_access = 'selected'` is neither accepted nor revoked, expired ones
  included (the runbook's step 2 revokes those too); it never deletes or
  changes a row. After the revoke it drops the table, the constraints and the
  column. `TestMigration0041_InviteTeamAccess` (up, the CHECKs, another org's
  team refused, the cascade, the refused down changes nothing, revoke, down,
  up) and `TestMigrateUpDownUp`.
- **`POST /invites`** (admin scope, `idempotency.Reject` as before):
  `InviteInput` gains `teamAccess` and `teams`. `tracker.NewMemberAccess`
  (shared with `POST /agents` and the approval) checks the kind: absent is
  `all`; `selected` needs 1 to 50 teams; `all` takes none. Role `admin` with
  `selected` is `400` (#30). Teams are keys or ids resolved by
  `tracker.ResolveAccessTeams` (the setter's resolver: another org's team and
  a made-up one are the same `400 unknown team`). In one transaction:
  `LockTeamAccessGuard` first (entry 2 of #65), the existing checks, the
  insert, `InsertOrgInviteTeams`, then `CheckWebhookGuard` (an admin route:
  the refusal names the repositories). A refusal writes nothing and sends no
  email. The audit line adds `team_access` and the team count (no key).
- **The invite view** (`Invite`, the 201 body and `GET /invites`) has
  `teamAccess` and `teams`: the keys of the invite's teams that still exist,
  sorted; `[]` for `all`. A `selected` invite whose teams were all deleted
  lists `selected` with `[]`. Keys come from `ListOrgInviteTeamKeys` (one
  query per list, admin only).
- **`POST /invites/accept`** (public; floor, limits and `idempotency.Reject`
  unchanged). The body has no access field (`DisallowUnknownFields`: a
  `teamAccess`, `teams` or `role` is `400` and the invite stays usable), so
  an accept cannot widen the invite. In the transaction:
  `GetInviteAccessByHash` reads the org and the access of the token's row;
  for `selected` the guard's lock comes first; then `ConsumeOrgInvite` (the
  race arbiter, as before). The access is applied only when
  `InsertOrgMemberRole` inserted a row (#27; an existing member keeps theirs,
  #28), for a new user and for an install-level user alike
  (`applyInviteAccess`): `LockInviteTeamsForAccept` reads the invite's teams
  that still exist with `FOR KEY SHARE` on the team rows (a team deleted
  before is gone by the cascade; one deleted while the accept waits is
  skipped, and a delete that comes after waits for the accept); none left is
  `409 conflict` "the teams this invite gives no longer exist; ask an org
  admin for a new invite" (no team named) and the whole accept rolls back,
  so the invite stays open; else `SetInvitedMemberSelected`,
  `InsertInvitedMemberTeams`, then `CheckWebhookGuardNameless` (the caller
  is not an org admin: the text names no repository). Its `409` also rolls
  back and leaves the invite usable. The token's row read adds no oracle:
  the token is 256 random bits, every failure has the same answer and the
  floor pads every answer.
- **`POST /signups/{id}/approve`** takes an optional body
  `SignupApproveInput {teamAccess, teams}` (`optionalJSON`: no body, an empty
  body or `{}` is `all`). The route keeps `s.idem` (the body holds no
  secret, #29). Kind checks before the transaction (`400`); in it, for
  `selected`: the guard's lock first (entry 3), the request row, the teams
  (resolved after the replay short-cut, so a replay of an approved request
  answers it again and changes nothing, whatever its body), the user, the
  membership, `tracker.GrantNewMemberTeams`, the decision, then
  `CheckWebhookGuard` (an admin route, repositories named).
- **The guard counts open invites.** `RestrictedAccessExists` is true for a
  member with `selected` OR an invite with `selected` that is not accepted,
  not revoked and not expired (an expired invite can never be used:
  `ConsumeOrgInvite` wants `expires_at > now()`). So an open `selected`
  invite blocks a repository map with no team, clearing a team, and enabling
  an integration with such a repository (entries 5 to 7), and the accept
  checks again for the case the guard cannot see (a direct database write).
- **No event.** Invites and approvals emit no change-feed event
  (decisions-T12 #14); the accept that makes a restricted member does not
  emit `member.access_changed` either (that is the setter's admin event).
  The audit lines say `team_access`.
- **Queries.** `signups.sql`: `InsertOrgInviteTeams`, `ListOrgInviteTeamKeys`
  (`admin`), `GetInviteAccessByHash`, `LockInviteTeamsForAccept`,
  `SetInvitedMemberSelected`, `InsertInvitedMemberTeams` (`system`: the
  accept has no caller org; separate copies so the setter's `admin` queries
  are not reused from a public route). Golden lines added. `RestrictedAccessExists`
  (`git.sql`) changed in place.
- **Tests** (`internal/api/invite_team_access_integration_test.go`):
  `TestInviteAccess_CreateAndRefusals` (keys and ids, duplicates collapsed,
  the view and the list; eleven bad bodies are `400` with no row written;
  another org's team and a made-up id answer alike; member, restricted
  member, agent token and agent fixture token: `403` for create and list),
  `_AcceptAppliesExactly` (three widen attempts are `400` and the invite
  stays usable; the new member sees INVA only, an ORB issue is `404`; an
  install-level user joins with the invite's two teams and keeps `all` in
  their other org; an existing member keeps `all`; an `all` and an admin
  invite give `all`), `_TeamsDeleted` (one of two teams deleted: the list and
  the accept give the other; all deleted: `409`, no user, no membership, the
  invite open, the same answer twice, then revoked: `401`),
  `_WebhookGuard` (a `selected` invite and a `selected` approval refused
  naming the repository, nothing written; `all` passes; with a team it
  passes; the open invite alone, with no restricted member, blocks clearing
  the team and a new repository with no team; a repository that lost its
  team by a direct write makes the accept `409` with no repository named and
  the invite usable, and the same link works once the team is back; a
  revoked and an expired invite no longer count), `TestSignupApproveAccess`
  (six bad bodies and an unknown field are `400` and nothing is made; a
  member is `403`; `selected` writes the two teams; replays with another
  body change nothing; no body, `{}` and `all` give `all`),
  `_CrossOrg` (another org's admin, pinned and unpinned token: `404` for
  list, create and revoke; this org's team by key or id in its own invite is
  `400 unknown team`; its own invite restricts the person in its org only),
  `_NoOracle` (a wrong token and a used, revoked or expired `selected` invite
  give the same `401` bytes and headers, each after the floor; the
  teams-gone `409` after the floor too), `_NoSecretsNoEvents` (a copied and
  an emailed `selected` invite and a `selected` approval with an
  Idempotency-Key: no token, session or code in the log, the idempotency
  rows or the stored rows; no event; the audit lines). A mutation check (the
  accept skips the access) fails six of the eight.
- **Docs.** `docs/deploy.md` (the invite step and the approval body; the
  rollback runbook's step 2 now names migration `0041` and says the down
  refuses while such an invite is open), `docs/architecture.md`,
  `api/openapi.yaml` (`InviteInput`, `Invite`, `SignupApproveInput`, the
  approve body and `400`, the three route texts); `web/src/api/schema.d.ts`
  is regenerated.

### Carried to later tickets

- Ticket 11a: the invite dialog sends `teamAccess` and `teams` (hidden for
  role Admin) and the invites table shows the access. Ticket 11b: the
  sign-up approve action gets the control.
- The Slack and chat services still post events of every team (the "Not
  done" item).
- A person whose `selected` invite failed at accept (teams gone, or the
  guard) learns that the invite cannot be used now; the text names no team
  and no repository. Accepted (#27 asks for a clear text).

### Not tested

- An accept racing a delete of the invite's last team (the `FOR KEY SHARE`
  lock is reasoned, not raced).
- An invite create racing a repository change under the guard's lock (the
  lock is the one ticket 9 raced with the setter).
- The approval's `selected` path with mail off (it shares the code of the
  mail-on path tested here).

## Build notes (LAUN-227, ticket 11a)

Ticket 11a was built in two parts: the Members page (#74) first, then,
after ticket 10 (LAUN-226) merged, the invite form (#75; "Part 2" below).
No numbered rule changed. No server file and no `api/openapi.yaml` change.

- **Teams column.** `web/src/features/settings/MembersPage.tsx` shows a
  "Teams" column and an action column when the caller is an org admin and the
  server sent the fields (`teamAccess` is null for any other caller, #33). The
  cell reads "All teams" (an admin row or `all`), the team keys, or "No team"
  (`selected` with none, open point B). An admin row has no action (#2: the
  server refuses an admin target with `400`). An agent row has the action
  (agents are members, #31).
- **Edit access dialog** (`TeamAccessDialog.tsx`): a radio "All teams" /
  "Selected teams", the checklist of `GET /teams`, and one "Select all teams
  of <product>" button per product with teams (#70). A key the member has
  that the team list does not show (an archived team, #6) stays in the
  checklist as "<KEY> (not in the team list)", ticked, so a save never drops
  it unseen.
  The `teams` sent are keys, sorted.
- **Dry run first.** "Check changes" sends `?dryRun=true` without
  `assignedIssues` and shows the counts: teams added and removed (keys;
  an id the list does not show reads "1 team not in the list"), issue
  subscriptions, team follows and team notification settings removed, and
  the hidden assigned count (see the review commit). Only then "Save access" appears. Any edit after the check
  hides the counts and asks for a new check (the preview is keyed on the
  body). `changed: false` reads "Nothing changes" and Save is disabled.
- **Assigned issues (#49, point E).** When `assignedInLostTeams` is not empty,
  a radio group names the count per team key; "Unassign them (recommended)"
  is preselected, "Keep them assigned" is the other choice. Save sends
  `assignedIssues` only then.
- **Errors.** 400: "The server did not accept this access: <server text>";
  403: a fixed text; 404: a fixed text (the person left the org); 409: the
  server text (the per-team counts or the repository names; an admin-only
  route, so the admin may read them) and "Check the changes again". A 409 on
  Save drops the preview, so the admin must check again. The server text is
  cleaned by `toApiError` (control characters, markup, 200 characters).
- **Cache.** `useSetTeamAccess` (`web/src/api/queries.ts`) refetches the
  members after a real save, and the issues and issue details when
  `unassigned > 0`. A dry run refetches nothing.
- **Settings search.** The Members page has the keyword "team access".
- **Tests** (`web/src/features/settings/teamAccess.test.tsx`, admin
  session unless named): the column and the cells; "No team"; no column when
  the fields are null; a member session gets "You need admin access." and no
  setter call; dry run then save (query, body, user id, the note); an edit
  after the check; nothing changes; the product shortcut; the archived key
  kept; the counts and the Unassign default; Keep; a 409 on save; 400, 403,
  404 and 409 on the dry run.

### Review commit (LAUN-227)

- **No save without review.** "Check changes" and "Save access" have
  different React keys, so a double-click on Check never lands on Save. Save
  ignores a click with `detail > 1`, a click while a request is in flight (a
  ref, not only the mutation flag) and a click before the counts were drawn.
  The check also ignores Enter while a request is in flight. Busy buttons
  use `aria-disabled`, so focus stays on them.
- **Announced.** A `role="status"` line says the summary when the dry run
  answers, and focus moves to the "Changes" heading of the counts.
- **Hidden count.** The dry run has no `assignedIssues`, so its
  `hiddenAssigned` still holds the issues an Unassign clears. The row reads
  "After the save: open issues assigned to them in teams they cannot see"
  and shows `hiddenAssigned` minus the open issues in the lost teams (never
  below 0) for Unassign, `hiddenAssigned` for Keep.
- **Teams list first.** The check waits until `GET /teams` loaded; a failed
  load shows an alert with "Try again". An id or key that the list does not
  show reads "a team not in the list" (not "archived": the web cannot know).
  The extra checklist rows come from the member's row, so an unticked one
  can be ticked again.
- **Smaller fixes.** A 404 refetches the member list. The checked radio
  takes the first focus. A preview with no team ticked says the member
  will see no team and no issue. A save that answers after its dialog
  closed does nothing (a ref set on unmount; the dialog is keyed on the
  member). The page note gives the real result (unassigned, removed counts)
  and says when it differs from the check. The counts list wraps long labels
  and stacks under 480 px; the members table scrolls sideways inside its
  card (its 680 px minimum), like the other settings tables.
- **Tests added:** a 404 refetch, a double-click on Check, a `detail: 2`
  click on Save, the announcement and focus, the first focus, `/teams`
  answering 500 then the retry, the no-team preview, a save answering after
  Cancel, the note and the Teams cell refreshing after a save.

### Part 2: the invite form (#75)

- **Control.** `web/src/features/settings/InvitesSection.tsx`: under Role, a
  "Team access" radio ("All teams" preselected, "Selected teams") and the
  team checklist. Role Admin hides both and shows "An admin always sees
  every team."; the body then has no access field (#30: an admin invite is
  always `all`). A member invite sends `teamAccess: "all"`, or `"selected"`
  with the ticked keys, sorted. After a create the form goes back to All
  teams.
- **Shared checklist.** The checklist (team boxes, the product shortcut,
  rows for keys not in the list) moved to
  `web/src/features/settings/TeamChecklist.tsx`, used by the members dialog
  and the invite form. LAUN-228 adds `TeamAccessField.tsx` on another
  branch: the two should become one component later.
- **Checks before sending.** "Selected teams" with no team is refused in
  the browser ("Pick at least one team, or choose All teams.", focus on the
  first box, `aria-invalid` and `aria-describedby` on the boxes); with the
  team list not loaded it asks to wait (the list has "Try again"). A ref
  ignores a second Enter or click while the create runs.
- **Errors.** Fixed texts, as for the rest of the invite form: `400 unknown
  team` says a picked team no longer exists; another 400 about teams says
  the choice was not accepted; the webhook guard's `409` says every
  connected repository needs a team first and names none (the server text
  names them; it never reaches the screen).
- **Note and table.** The note after a `selected` create names the teams.
  The invites table has a "Teams" column: "All teams" (an `all` or admin
  invite), the keys, or "No team (its teams were deleted)" for a `selected`
  invite whose teams are all gone.
- **Tests** (`web/src/features/settings/inviteAccess.test.tsx`, admin
  session unless named): the exact body with two teams and the note; role
  Admin hides the control and sends no access field; zero teams refused
  with focus and no POST; a second submit while the create runs sends
  once; a failed team list blocks a Selected invite and retries; the 400
  and 409 texts, with no server detail on screen; the table cells; a member
  session sees no invites, no control and makes no invites call.
  `invites.test.tsx` now expects `teamAccess: "all"` in the member body.

### Carried to later tickets

- Merge `TeamChecklist.tsx` with LAUN-228's `TeamAccessField.tsx`.
- Ticket 11b: the agent form and the sign-up approval control, the `access`
  SSE handler, the no-access empty state.
- The checklist lists the teams `GET /teams` returns. An archived team the
  member does not have cannot be added from the web.

### Not tested

- A real browser (layout at 375 px with the two new columns, keyboard focus
  order in the dialog); jsdom tests only.
- The setter against a live server from the web (the mock follows
  `api/openapi.yaml`).

## Build notes (LAUN-228, ticket 11b)

The web part of #71 to #73 and #76 to #78, and the agent form and the
sign-up approval of #75. No numbered rule changed. The approval control came
in a later commit, after ticket 10 (LAUN-226) was merged (see "Part 2").

- **Agent form (#31, #75).** `web/src/features/settings/TeamAccessField.tsx`
  is the control: a fieldset "Team access" with "All teams" (preselected,
  open point A) and "Selected teams", which shows a checklist of the teams
  (`useTeams`; an admin sees every team, #2). `AgentsPage.tsx` sends
  `teamAccess` always and `teams` only with `selected` (the server refuses
  `teams` with `all`). `selected` with no team is refused before any request
  ("Pick at least one team, or choose All teams.", focus on the first radio).
  A `409` whose text names repositories is the webhook guard (#65, entry 4):
  it shows a fixed text at the control, never the server text (it names
  repositories). A `400` on a team (deleted since the list was read) clears
  the ticks and reads the teams again. The agents table gets a "Teams"
  column ("All teams" or the keys; `Member.teamAccess` is filled for admins,
  #33). Ticket 11a may add its own control for the invite dialog and the
  members page; the two should become one when both are on `develop`.
- **No team (#73, #77).** `IssuesPage` keeps "This organisation has no team
  yet" with the team form for an admin (an admin sees every team, so the org
  has none). A member gets "You do not have access to any team yet" and "Ask
  an organisation admin." (`NoTeamAccess.tsx`); this replaces the old member
  text "Ask an organisation admin to create the first team", because a member
  cannot tell the two cases apart. The create dialog shows the same text in a
  status line and its Create button is disabled (`aria-describedby` points at
  the text); Enter in the title sends nothing. An admin's dialog says the org
  has no team. No "request access" control anywhere (tests assert it).
- **Visible teams only (#71).** The sidebar, the palette and the dialog read
  `useTeams`; nothing in the web hard-codes a team. Three places held a team
  key from elsewhere and now check it against the visible list: the create
  dialog's team (from the page, a product or a hidden team link) falls back
  to the first visible team (or none) and resets the state, cycle, project
  and labels, also while the dialog is open when an `access` event removes
  the team; the projects page's remembered team filter (localStorage) drops a
  key that is not visible, like the status filter drops a gone status; the
  issue filters are not remembered (LAUN-109), and the view preferences keep
  only layout and sort per place (a place key of a hidden team names nothing
  the person did not see before and holds no data).
- **Not found (#72).** The server answers `404` for a hidden issue, cycle,
  project and team. The team page (`/team/HID`) showed only the title "Not
  found" and an empty list with a create button; it now shows "Team not
  found", like the cycle list. The web has no document route: a document is
  read inside its project page, which is the project's not-found page.
- **The `access` event (#56, #76).** `web/src/api/events.ts` listens to
  `access` on the change-feed stream. In the batch window (80 ms) it runs
  `refreshForAccess` once, whatever came in the window: queries of the org
  that belong to a lost team are dropped at once and the rest refetch in
  place (see the final review commit below); the session and the org list
  are read again (the role may have changed). The
  inbox and the count are left to the inbox stream. The cursor does not
  move (no id) and the connection stays: the handler never closes or
  reopens a stream, so it cannot loop. `web/src/api/notifications.ts`
  listens to `access` on the inbox stream and, once per batch window,
  resets the inbox lists and refetches the unread count (older rows of a
  team now visible are below the cursor, ticket 7); the backoff still
  resets on `count` only.
- **Tokens page (#78).** Its description says a token sees the same teams as
  its owner.
- **AGENTS.md.** A "Team access" row in the areas table.
- **Tests.** `web/src/features/teamAccess.test.tsx` (a restricted member's
  sidebar, palette and dialog; a hidden team link's dialog creates in a
  visible team; the remembered project team filter; not found for an issue,
  a team, a team's cycles, a cycle and a project; no team: the text, the
  disabled button and no request control, also for an admin; the `access`
  event takes a team out of the sidebar and puts it back with one stream,
  shows the no-access text when the last team goes, and refetches the
  unread count), `web/src/features/settings/agentTeamAccess.test.tsx`
  (default `all`, `selected` needs a team, the keys sent and shown, the guard
  `409` and an unknown team `400` with fixed texts), `events.test.ts` (drop
  inactive, refetch active, one refresh per burst, cursor and connection
  kept, a feed event after it), `notifications.test.ts` (inbox `access`),
  and the changed texts in `noTeams.test.tsx` and `tokens.test.tsx`.

### Review commit (LAUN-228)

Both reviewers asked for these; each has a test.

- **An open issue of a team that became hidden (B-M1).** The first commit
  invalidated the issue: the refetch answered `404` while the cached copy
  stayed, and the page said "This issue was deleted" with the hidden
  identifier, title and team (also in the tab title). Now the `access`
  refresh resets the org's queries (no cached copy is left), and
  `IssueDetailPage` itself treats a `404` for a cached issue whose team is
  not in a fresh team list (not fetching) as "Issue not found", title "Not
  found", so plain feed events (`team.updated`, `issue.updated`) give the
  same page. A real delete of a visible issue still says it was deleted.
  The `location.state` copy from a list is shown only until the first
  answer for that identifier (and only when its identifier matches), so a
  reset never shows it again. The cycle, project and team pages hold no copy
  after a reset (they read the reset queries; the team and cycle pages test
  the fresh team list). The web has no document page.
- **An access change while the stream is down (B-M2).** The server
  compares access with what it read when a connection opened, so a change
  made in the gap sends no `access`. `OrgEvents` now marks a reconnect after
  an error and runs the access refresh when the new stream opens (not on the
  first open). This is no longer a residual. (Its cost note, "the backoff
  limits it", was wrong: an open reset the backoff. The final review commit
  replaces this design.)
- **The guard `409` (A-L1, B-L1).** Matched by the start of the two guard
  texts in `server/internal/tracker/webhook_guard.go` ("some members see only
  selected teams", "this cannot be done now"), not by "repositor": a taken
  username `repository-bot` is the username text again.
- **Other `400` texts (A-L3).** "at most 50 teams" gets "Pick at most 50
  teams, or choose All teams."; `teamAccess "selected" needs at least one
  team` gets the pick-a-team text; only "unknown team" is "A team you picked
  is gone".
- **Focus on the team error (A-L4).** Focus goes to the first team box,
  which carries `aria-invalid` and `aria-describedby` to the error.
- **Create dialog (A-L5, B-L2).** The no-team line uses a module class
  (`ui.createNoTeam`), not an inline style, and its text goes to the
  dialog's live region (in the page before the text), so it is announced
  once. When the dialog replaces a team that is no longer visible, it also
  clears the parent (a sub-issue of a hidden issue).
- **The projects team filter (B-L3, A-L6).** No remembered team key is used
  or shown until the team list has loaded; a failed read shows none. The
  stored view stays one key per browser (`launchpad.projects.view.v1`):
  keying it by org and user would move every stored view and change five
  test files, and the check against the visible list already stops a key of
  another org or a hidden team from showing. Not done, on purpose.
- **One read per `access` (B-L4).** The org refresh no longer touches the
  inbox and the count; the inbox stream batches its `access` events and
  reads each once.

### Part 2: the sign-up approval (after the LAUN-226 merge)

- **`SignupsPage.tsx`.** "Approve" now opens a dialog "Approve <name>?" with
  `TeamAccessField` ("All teams" preselected) and "Approve request". It
  sends `{teamAccess: "all"}` or `{teamAccess: "selected", teams: [keys]}`
  (`useApproveSignup` takes `{id, body}`; the type is the generated
  `SignupApproveInput`). `selected` with no team is refused before any
  request (focus on the first team box). The button waits for the team list
  ("Loading teams…", disabled). One request at a time: a ref guards a double
  click and the button is disabled while it runs.
- **Errors.** The texts of the agent form, now shared in
  `TeamAccessField.tsx` (`teamAccessError`): the guard `409` (fixed text, the
  repositories are never shown), `400` unknown team (the teams are read
  again and the ticks cleared), `400` at most 50 teams, `400` selected
  needs a team. These keep the dialog open with the text at the control;
  every other error closes it and shows `approveText` as before (the mode
  refusal still shows the mode notice).
- **After the approval** the note names the access the member really has,
  read from a fresh member list by the answer's `userId` (the final review
  commit; the first version named the choice, which is wrong for a replay).
  A request approved before answers `200` and keeps its first access
  whatever the body asked (ticket 10): when the answer's `decidedBy` is
  another user, the note adds that the request was approved already and the
  choice was not applied. When the list cannot be read, the note names no
  access and points to the Members page. The dialog says plainly that
  approving an approved request changes nothing, and links the Members
  page.
- **Tests** (`web/src/features/settings/signupApproveAccess.test.tsx`): the
  default and the dialog text, the exact `selected` body and the note, zero
  teams refused with no request, the wait for the team list, one request
  for a double click, the guard `409`, unknown team, at most 50, approved
  already by another admin, and a member session that sees no page and no
  control. `signups.test.tsx` now confirms in the dialog and expects
  `{teamAccess: "all"}`.

### Final review commit (LAUN-228)

- **A reconnect or an `access` event no longer empties the page (MEDIUM).**
  The review commit reset every on-screen query of the org on every
  reconnect after an error: an open issue showed its loading spinner, the
  page unmounted and a comment draft, an open edit, focus and scroll were
  lost. Reconnects are common (Wi-Fi, sleep, deploy). Now
  `refreshForAccess` reads the session and the team list first and compares
  the visible set with the cached one:
  - no team lost: every other org query is invalidated (refetched in place,
    no spinner, no input lost); a gained team's rows come with the refetch;
  - a team lost: the queries of that team (an issue, its activity, comments
    and subscription by the identifier's prefix; states, cycles and projects
    by the team key) and the lists that mix teams (issues, projects-all,
    cycle-progress, labels, products, milestones) are reset, so their
    hidden rows never show, not even during the refetch; everything else is
    invalidated in place;
  - the team list cannot be read or was never read: everything is reset
    (the safe side) for an `access` event, and nothing for a reconnect (the
    stream is likely down again; the next reconnect checks again).
  A reconnect passes `onlyIfChanged`: with the same team set it reads only
  the team list and the session. An open issue of a lost team still becomes
  "Issue not found" (its query is reset, and `IssueDetailPage` checks the
  team list on a `404` in any case).
- **A flapping stream cannot cause a refetch storm.** The backoff no longer
  resets on `open`: it resets on the first real message or after the
  stream stayed open 10 s (`stableMs`), so a stream that opens and fails at
  once keeps backing off. The reconnect check runs at most once per 30 s
  (`gapCheckMs`); checks asked for inside the window run once at its end.
- **Residual, accepted.** After a team loss, the details that are kept (a
  visible issue, a project of several teams, a member list) may show a link
  to a now-hidden issue or a lost team's key until their refetch lands
  (about one request time). Tracked per team, every key would need a team
  tag; not done.
- **The approval note (LOW).** See "After the approval" above: it reads the
  real access; a second approval by the same admin no longer names the new
  choice.
- **Tests.** `events.test.ts` (refetches with a 50 ms delay: no team lost
  keeps every query's data; a lost team's data and the mixed lists are empty
  while the refetch runs and the rest is kept; the two safe-side cases; a
  reconnect with the same set reads only the teams; a gained team; one
  check per burst; the reconnect check only after a reconnect; a flapping
  stream keeps backing off; a 1 s flapping stream reads the team list
  twice in 50 s; the backoff resets after 10 s or on a message);
  `teamAccess.test.tsx` (with 150 ms refetches: a comment draft survives a
  reconnect and an `access` event that takes another team, both shown to
  fail with the old reset; an open issue of a lost team is Not found with
  the title gone); `signupApproveAccess.test.tsx` (the same admin's replay
  names the first access, an unreadable member list names none, two clicks
  in one render send one request, shown to fail without the guard).

### Second final review commit (LAUN-228)

- **A loss found by any read of the team list (MEDIUM).** The last commit
  compared the fresh list with the CACHED one. A focus refetch (the team
  list is stale after 60 s), a `team.*` or `product.*` event or a mount can
  read the list first; then the cached list already lacked the team, the
  access refresh and the reconnect check saw no change, and cached data of
  the lost team (an issue, its comments and activity, the lists, the search
  and palette results) stayed until `gcTime` (5 min), shown on Back or a
  palette reopen. Now `watchTeamList` (in `events.ts`) subscribes to the
  query cache for the org's team list: every successful answer, whoever
  asked, is compared with the last set this module recorded (a per-client,
  per-org map outside the query), and a loss drops that team's queries and
  the MIXED ones at once (`applyTeamList`). The first list only records; a
  loss and a gain together drop the lost team only; the same list twice
  drops nothing twice. `OrgEvents` starts the watcher in `start` (SSE and
  poll mode) and stops it in `stop`; users are counted, so one watcher
  serves a client and org. `refreshForAccess` compares with the recorded
  set too, and calls the same path.
- **More kinds dropped on a loss (LOW).** MIXED now also holds `project`
  (a project's detail may be a project of the lost team only) and
  `project-updates`; `labels` (team labels) and `milestones` were in it.
  Members, project statuses and project labels name no team row and stay.
  Cost: an open project page reloads (its data goes) when the person loses
  any team; such losses are rare.
- **Rollbacks do not restore dropped rows (LOW).** `useUpdateIssue`, the
  board's `placeMany` and the two inbox mutations read a per-client, per-org
  generation (`api/accessGen.ts`) with their snapshot; a drop (and the inbox
  stream's `access` reset) bumps it, and a failed write then does not put
  the snapshot back (`restoreSnapshot`); the usual refetch follows.
- **The location copy (LOW).** `IssueDetailPage` shows the list's copy
  (`location.state`, which outlives the page: Back, a reload) only when its
  team is in the current team list, and none while that list is not loaded.
- **What "never shows" means now.** Data of a lost team is dropped at the
  moment the web first sees a team list without that team (any read), so
  it never shows after that. Before that moment (the server has removed
  access, the web has not read the list yet) cached data can show; a read
  of it then answers `404`. Kept details of visible things (a visible issue,
  a member list) may name a now-hidden issue or a lost team's key until
  their refetch lands.
- **Tests.** `events.test.ts`: a focus-style refetch drops the lost team's
  queries (on screen and not, project and updates too; another org kept),
  and a later reconnect check empties nothing again; the first list only
  records, a loss with a gain drops only the loss; one watcher per client
  and org, stopped with its last user; the stream watches from start to
  stop (poll mode); a snapshot taken before a drop is not put back.
  `teamAccess.test.tsx` (150 ms answers): a `team.updated` event takes the
  team out of the sidebar and a draft on a visible issue stays; a history
  entry with a copy of a hidden issue never shows it (shown to fail
  without the fix).

### Merge with ticket 11a (LAUN-227)

- **One checklist.** LAUN-227 brought `TeamChecklist.tsx` (the members
  dialog and the invite form: one box per team, a "Select all teams of
  <product>" button per product, rows for keys the list does not show).
  `TeamAccessField.tsx` (the agent form and the sign-up approval) now uses
  it for its checklist; `TeamAccessField` stays the "All teams / Selected
  teams" fieldset with the hint, the error and the loading and error
  states around it. The members dialog and the invite form keep their own
  radios around `TeamChecklist`: one wrapper for all four would be the
  next small step.
- `queries.ts`, `types.ts` and `settings.module.css` merged without a
  conflict; only this note conflicted (sections kept in ticket order).

### Carried to later tickets

- One wrapper for the "All teams / Selected teams" choice in the four
  forms (above).
- A bearer-token web session (the poll mode of `OrgEvents`) gets no `access`
  event: `GET /events` has no such row. Its lists follow at the next
  refetch (focus, navigation, the 60 s team list stale time); the team
  list watcher drops a lost team's data when that refetch reads the list,
  and an open issue of a hidden team becomes Not found at its next refetch.
  Accepted: the normal web uses the cookie and the stream.

### Not tested

- A real browser `EventSource` with `event: access` and `data: {}` (the
  tests use the fake; 6a notes why the data is `{}`).
- A form that is open (other than the create dialog) when an `access` event
  arrives (the "Not measured" item stays).

## Build notes (LAUN-231, chat services warning)

- Option 1 of the ticket (the project owner, 2026-11-02): no migration, no new stored value,
  the text of a post does not change.
- `ChatServicesPage.tsx`: the add and edit dialog shows, under the team select,
  "Everyone in this channel sees each post this service sends for team <KEY>:
  what happened, a link, the issue identifier, and for a state change the names
  of the old and new state. They see it whether or not they have access to team
  <KEY>. Launchpad cannot see who is in the channel." It shows always (not only
  when a restricted member exists): the page cannot know who is in the channel
  either, and an org can restrict a member later. The key is the selected
  team's and follows a change of the select; a team that is not in the list
  (archived) reads "this team". The note has `role="note"` and is in the
  select's `aria-describedby`. The Services section text says the same in one
  sentence. Editing happens only in the dialog, so the list rows do not repeat
  it.
- The first text ("the identifier and kind of every event") was wrong (review):
  a state-change post also carries the old and new state names (T39), only
  ticked events post, and a muted issue posts nothing. The text now names what
  a post holds, checked against `chatSentence` and `ChatText` in
  `internal/notify/chat_post.go` for every kind.
- **Server (review):** a post queued for the old team could reach the channel
  of the new team. `ChatAdmin.Update` kept the waiting rows on a change of
  `teamId` or `webhookUrl` (only `enabled=false` dropped them), and the sender
  read the service's CURRENT URL with no team check, with retries for up to
  about 15 hours. Now `Update` deletes the open rows (`DeleteOpenChatOutbox`)
  in the same transaction when the team changes or a URL is given (the URL is
  sealed, so any URL in the body counts as new). The sender checks the target
  against the service's current team before it builds the post: an issue's
  `team_id` (`GetMailIssue`), a project reminder's `project_teams`
  (`ListProjectTeamIDs`); a mismatch is skipped with `not_selected` ("the
  service does not select this post"), no attempt. That word is reused on
  purpose: a new word needs a migration of the `chat_outbox_skip_check`
  constraint, and the meaning fits. No new query. A rename, an events change
  or the same team keep the rows.
- OpenAPI: the descriptions of `createChatService` and `updateChatService`
  (text only). `docs/architecture.md` "Team access": one item.
  `decisions-T38` #11: a "Superseded by LAUN-231" line.
- Web tests (`chatServices.test.tsx`, "Chat services channel warning"): add and
  edit show the note with the right key; a team change updates it; the
  select's accessible description holds it; an archived team reads "this
  team"; the page sentence; a member who opens the address gets "You need admin
  access.", no note and no chat services request.
- Server tests (`chat_move_integration_test.go`): a move (team only, and team
  with a new URL) deletes the waiting row and nothing is posted, then a post of
  the new team goes to the current URL; a new URL alone deletes the rows, a
  rename, an events change and the same team keep them; a send-time mismatch
  (issue and project reminder) is skipped `not_selected` with no attempt and no
  post, and a normal post goes after; a change in one org keeps another org's
  rows; no webhook token in a log, an event payload or an outbox row.
  `TestChat_TeamChangeMovesFuturePosts` now expects the old row gone; the test
  helper `putService` sends the URL only when it changes.
  `TestChat_DisabledAndDeletedDropWaitingPosts` is unchanged (enabled=false).
- Agent and cross-org rules are unchanged (no route changed).

### Carried to later tickets

- A per-service filter or a "teams with no restricted members" switch (option 2
  of LAUN-231). Not built.
- A post that the sender has already read the service for (in flight) when the
  admin moves the service goes out with the values it read: the old URL and
  the old team, so it reaches the old channel only.
