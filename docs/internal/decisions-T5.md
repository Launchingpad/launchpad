> Internal design note from the Orbit deployment; kept for history.

# T5 decisions (ORB-3409): GitLab webhooks

T5 adds the GitLab merge_request webhook and the per-org config it needs.
The design is `docs/architecture.md` ("GitLab integration"). Where the brief
left a gap, T5 made the smallest sensible choice. Each one is listed here so
a later tranche can change it on purpose. The routes and wire shapes are in
`server/api/openapi.yaml`.

**No hostname is decided yet, so no hook exists on GitLab.** Everything was
built and tested locally against fixtures that are **hand-written from
GitLab's documented payload shape**, not recorded deliveries
(`server/internal/gitlab/testdata/README.md`). Adding the hooks is
a cutover step (see "Turning it on" below).

## Routes

**Superseded config routes.** The `GET/POST /integrations/gitlab`, `PATCH .../gitlab/{integration}` and
`.../gitlab/{integration}/projects` rows below were replaced by the provider-neutral
`/integrations`, `rotate-secret` and `/integrations/{id}/repos` routes (LAUN-7a, LAUN-86; see
decisions-T17). Only the webhook row is still live.

| Route | Auth | Notes |
|---|---|---|
| `POST /orgs/{org}/integrations/gitlab/webhook` | `X-Gitlab-Token` only | Public: no bearer, no session, no membership. |
| `GET /orgs/{org}/integrations/gitlab` | admin | Secrets are never returned. |
| `POST /orgs/{org}/integrations/gitlab` | admin | Never idempotent (the body holds the secret). |
| `PATCH /orgs/{org}/integrations/gitlab/{integration}` | admin | Base URL, secret rotation, on/off. |
| `GET /orgs/{org}/integrations/gitlab/{integration}/projects` | admin | |
| `POST /orgs/{org}/integrations/gitlab/{integration}/projects` | admin | Idempotency-Key works. |
| `DELETE /orgs/{org}/integrations/gitlab/{integration}/projects/{project}` | admin | Links already made stay. |

## Schema

1. **One migration, `0004_mr_links_integration`** (review fix, round 5).
   0001 already has `gitlab_integrations`, `gitlab_project_map`,
   `gitlab_webhook_events` and `mr_links`. 0004 adds `mr_links.integration_id`
   (NOT NULL, composite FK to `gitlab_integrations (org_id, id)`, ON DELETE
   CASCADE), makes the unique key `(issue_id, integration_id,
   gitlab_project_id, mr_iid)` and rebuilds `mr_links_mr_idx` as
   `(org_id, integration_id, gitlab_project_id, mr_iid)` (#25).
   - **Backfill rule:** the oldest integration of the org that maps the
     link's project id; if none maps it any more, the oldest integration of
     the org; a link in an org with no integration (which no hook could have
     written) is deleted. No hook existed before 0004, so in practice the
     table is empty.
   - **Down:** restores the 0001 key. Where two integrations had a link for
     the same issue, project id and MR iid, it keeps the oldest row and
     deletes the others: the only loss, for rows 0001 could not hold.
   - Tested: `TestMigration0004_MRLinksIntegration` (backfill, new key, down,
     up again) and `TestMigrateUpDownUp`.
2. **One integration per GitLab base URL per org** (0001's
   `gitlab_integrations_org_base_key`). An org with gitlab.com and a
   self-hosted GitLab has two integrations, each with its own secret and
   project map. The base URL is stored lower-case with no default port
   (443 for https, 80 for http; round 5) and no trailing slash, so
   one instance cannot be added twice under two spellings. It is a label and
   a uniqueness key; the webhook does not compare it with the payload.

## The secret

3. **Stored as sha256 only**, in 0001's `webhook_secret_hash` (its CHECK
   requires 32 bytes). The admin chooses the secret and pastes the same value
   into GitLab's "Secret token" field; the server never returns it, not even
   once. Unsalted, fast sha256 is safe here for the same reason it is safe for
   `api_tokens`: the input is a high-entropy secret, not a password. So the
   server requires **32-256 printable ASCII characters with no space**. A
   human-chosen short word is refused (400).
4. **The constant-time check.** The webhook hashes `X-Gitlab-Token` and
   compares it with `subtle.ConstantTimeCompare` against the hash of every
   ENABLED integration of the org, with no early exit. The matching
   integration is the one whose project map the event uses. Comparing hashes
   (fixed 32 bytes) also hides the secret's length.
5. **Never logged, emitted or stored in the clear.** The create route has
   `idempotency.Reject`, so the body is never hashed into an
   `idempotency_keys` row. PATCH is never idempotent by key. Config changes
   emit no event (as tokens, decisions-T1 #28). Validation errors never echo
   the value. Tests check the log, the events and the idempotency table.
5b. **No two enabled integrations of one org share a secret** (review fix,
    round 3): create, rotate and enable answer `409` when another enabled
    integration of the org already has that hash, because the token check
    could not tell them apart. An org-scoped advisory lock makes the check
    and the write atomic (no migration, so no unique index).
6. **Rotation** is `PATCH {"secret": ...}`: the old secret is refused from the
   next request on. There is no overlap window; update GitLab's field right
   after. `{"enabled": false}` refuses the secret without deleting anything.

Compared with another internal service (for ideas only, not touched): it holds one GitLab
token in the environment. Here the secret is per org and in the database, as
the architecture's "Secrets" line says.

## The webhook request

7. **Order of checks.** (1) the token, before the body is read and before any
   header is parsed; (2) `X-Gitlab-Event` must be `Merge Request Hook`,
   otherwise `202 ignored` and the body is never read; (3)
   `X-Gitlab-Event-UUID` is required (1-100 printable characters, else 400),
   and an `Idempotency-Key`, when sent, must be the same shape;
   (4) the body, at most 8 MiB (413); (5) the payload must be a
   merge_request (400); (6) one transaction, in which the idempotency row
   comes first, then the project mapping, and only then the MR URL check
   (400 for a URL that is not absolute http/https, which rolls the event
   row back). So a replay is always `duplicate` and an unmapped project
   always `202`, whatever its URL. An unknown org, an org with no
   enabled integration, a missing token and a wrong token are all the same
   `401 unauthenticated`, so the response body does not tell which org
   slugs exist. Timing can: an unknown org costs one query (the org
   lookup), a known org two (plus its secret hashes). Org slugs are not
   secrets (they are in every API path), so T5 accepts this; padding the
   unknown-org path with a dummy query would close it.
8. **8 MiB body limit.** GitLab allows a 1,000,000-character description, and
   an update event carries the old and the new one. The body is read only
   after the token check, so only a holder of the secret can make the server
   buffer it.
9. **"Rate-limit-safe."** The route is outside `Authenticate` and
   `RequireOrgMember`, and no rate limiter sits in front of it. A limiter
   would be a lever against us: GitLab sends bursts (every push to an MR is
   an update event), and GitLab disables a hook that keeps failing, so a
   limiter that answers 429 to GitLab, or that an attacker can fill with bad
   tokens, turns into a denial of service on the integration. Instead the
   refusal path is cheap and bounded: two indexed reads (org by slug, the
   org's hashes), a fixed-size hash compare, no body read. Brute force of a
   32-character random secret is not a practical threat.
10. **No Idempotency-Key middleware on the webhook.** The delivery key
    (#12b, from GitLab's own `Idempotency-Key` or `X-Gitlab-Event-UUID`) is
    its idempotency, and nothing may answer before the token check (even
    `idempotency.Reject` would answer 400 first). The tracker's
    idempotency table is never used for it.
11. **Answers.** `200 processed` (it did its work, which may be nothing to
    link), `200 ignored` (an event that asks for nothing), `200 duplicate`
    (delivery key seen before), `202 unmapped` (unknown project), `202 ignored`
    (not a merge_request event). Every body is
    `{status, linked, moved}` (identifiers). `linked` lists the issues
    this event NEWLY linked (an `mr_links` row created by it; round 5), not
    every issue the MR names; `moved` the issues it moved. GitLab treats
    any 2xx as delivered.
    **Superseded by T20 #65** (`decisions-T20-team-access.md`, LAUN-224): the
    body is `{status}` only, and the status does not depend on whether an
    issue matched.

## Idempotency and the transaction

12. **One transaction per event.** The `gitlab_webhook_events` row, the
    `mr_links` rows, the link attachments, the issue moves, their history
    rows and their events commit together. The event row is the FIRST write:
    `INSERT ... ON CONFLICT (org_id, event_uuid) DO NOTHING RETURNING id`.
    No row back means the delivery key (#12b) was seen before: `200
    duplicate`, nothing else is written. Two concurrent deliveries of one
    key: the second waits
    on the unique index, then gets no row once the first commits (tested
    with 8 concurrent deliveries).
13. **A database error is 500** and rolls the event row back with
    everything else, so a failed event keeps nothing (tested by renaming
    `mr_links` mid-test: the 500 keeps no row, and the same delivery sent
    again afterwards is processed in full). Processing an event a second
    time is safe in any case: moves are forward only and `mr_links` is
    unique per issue, integration and MR (#25). **Whether, when and how GitLab sends a failed
    delivery again was not verified in T5**, so nothing here relies on it;
    the cutover steps say to watch the hook's Recent events. The status
    `failed` in 0001's CHECK is therefore never written: a failed event
    leaves no row.
12b. **The dedupe keys** (review fix, rounds 3, 4 and 5),
    `gitlab.DeliveryKeys`, are stored in the existing `event_uuid` text
    column, one `gitlab_webhook_events` row each. Each is a header value
    plus the MR:
    - `ik:` + the `Idempotency-Key` header + `:p<project id>:mr<iid>:<action>`
      when GitLab sends one (1-100 printable characters, else 400). A
      reviewer reports GitLab sends it from 17.4 and keeps it the same when
      a delivery is sent again; not verified in T5.
    - `ev:` + `X-Gitlab-Event-UUID` + `:p<project id>:mr<iid>:<action>`,
      always (the Event UUID is required).
    **With both headers both keys are recorded** (round 5), ik: first, and
    the delivery is a duplicate if EITHER exists: a resend that drops or
    changes the Idempotency-Key, or changes the Event UUID but keeps the
    Idempotency-Key, still dedupes. A second key that already exists rolls
    the transaction back (the first key's new row with it) and answers
    `duplicate`. The fixed order and the distinct prefixes mean two
    concurrent deliveries cannot wait on each other in a cycle.
    Delivery keys are unique **per org, not per integration**
    (`gitlab_webhook_events_uuid_key` is `(org_id, event_uuid)`): two
    GitLab instances of one org that sent the same header value for the
    same project id, MR iid and action would collide. With random UUIDs that
    does not happen in practice; scoping the key by integration would need
    a migration.
    A reviewer reports that GitLab may give every hook fired by one request
    or job the same Event UUID (it is meant for recursion detection), and
    the same could hold for the Idempotency-Key; neither is verified in T5.
    If it is so, two MRs merged by one push would share the header value,
    and keying on it alone would answer the second `duplicate` and never
    move its issue. So BOTH forms carry the MR suffix (round 4 added it to
    `ik:`): the project, iid and action tell such events apart, and the
    same delivery sent again still has the same key.
    The two prefixes cannot collide. Both headers are length-checked
    printable ASCII and the action is cut to 32 printable characters, so
    a key stays under about 200 bytes and holds no secret.
14. **What the event row keeps.** `event_type`, `gitlab_project_id`, the
    status, and a `payload` summary of `{action, iid, draft}` only (since LAUN-4: the
    neutral action word, such as `opened` or `merged`, and no `state`; rows
    written before hold GitLab's `open` / `merge` and a `state`).
    No title, description, branch, URL or user data, so the table never
    becomes a copy of GitLab content. Event rows are not swept yet (they are
    small; a sweep can come with the backup work).
15. **Statuses.** `processed` (applied), `ignored` (asked for nothing). A
    duplicate writes nothing, and an unmapped project's row is rolled back
    (#26), so neither has a status of its own in the table.

## Which events do what

16. **Moves** (each for the issues named in the branch or title only, #21).
    Since LAUN-244 these are the built-in moves: a team's git workflow rule
    replaces the move of its event (`decisions-T44-git-workflow.md`).
    - `open`, `reopen` of a non-draft MR: link, move to the team's
      `review_state_id`.
    - `open`, `reopen` of a **draft**: link, no move. A draft is not ready
      for review.
    - `update` that marks a draft **ready** (`changes.draft` true to false,
      or the older `changes.work_in_progress`, on an open MR): link, move to
      review. This is the one "update that changes state".
    - every other `update` (title, labels, pushes, ready to draft): nothing.
      The event row is kept (status `ignored`) and nothing else changes, not
      even an `mr_links` refresh.
    - `merge` (GitLab sends action `merge`, state `merged`): link, move to the
      team's `testing_state_id`, even for a draft.
    - `close` (unmerged): the MR's existing `mr_links` rows become `closed`.
      No move, no new link.
    - `approved`, `unapproved`, `approval`, `unapproval` and anything else:
      nothing.
17. **Forward only** (`isForwardMove`). An issue whose state type is
    `completed` or `canceled` (Done, Canceled, Duplicate) never moves. Else a
    move happens only to a later state type (triage < backlog < unstarted <
    started < completed < canceled) or, within one type, to a later board
    `position` (In Progress < In Review < In Testing are all `started`). So a
    reopen after a merge leaves In Testing alone, and a Backlog issue merged
    without a review goes straight to In Testing. Nothing moves into an
    archived state.
18. **Also not moved:** an archived issue (as the cycle rollover: an
    archived issue is never changed silently), an issue already in the
    target state, and an issue of a team with no review / testing anchor.
    Such issues are still linked.
19. **A merge of one MR moves the ticket to In Testing** even when the ticket
    has other open MRs. "The last merge moves a multi-MR ticket" stays human
    judgement (architecture), and the AC asks for the move.
20. **History and events.** A move updates the issue (version + 1, the state
    timestamps of decisions-T1 #9) and writes an `issue_state_changes` row
    with `source = 'gitlab'` and no actor, in the same transaction. It emits
    `issue.updated` with no actor and the payload of a PATCH state change
    plus `source: "gitlab"` and `mergeRequest: {gitlabProjectId, iid, url}`.
    A new link attachment emits `attachment.created` (payload
    `source: "gitlab"`). Events are the last writes of the transaction.

## Linking

21. **The matcher** is the architecture's regex
    `(?i)\b([a-z][a-z0-9]+)-(\d+)\b`, run over the source branch, then the
    title, then the description. The key is upper-cased and kept only if it
    is the key of a (non-archived) team of the org. Numbers outside
    1..2147483647 are skipped. Effects of the regex worth knowing:
    - `\b` is ASCII, so `feature/ORB-12_fix` does NOT match (`_` is a word
      character). Use `orb-12-fix`, as Linear's `gitBranchName` does.
    - A one-letter team key can never match (the prefix needs two
      characters).
    - **Only the branch and the title move an issue** (2026-10-24).
      An issue named ONLY in the description is linked (an `mr_links` row
      and a link attachment) but its state never changes and it gets no
      history row: a description names related tickets ("Refs ORB-2")
      that the MR does not deliver. An issue named in the branch or title
      AND in the description is listed once and moves once
      (`gitlab.MRRefs`). No "magic words" are needed in the branch or
      title.
22. **Caps.** At most 25 distinct issues per event (in that order; more are
    ignored), and at most 1000 regex matches per text, so a megabyte of
    `UTF-8` mentions cannot cost much. Non-team matches do not use up the 25.
23. **The project map's `team_id`.** 0001 has a nullable `team_id` on
    `gitlab_project_map`. T5 gives it one meaning: when set, only that
    team's issues are linked from that project; when null, any team of the
    org. With one team the two are the same.
24. **Issues are locked in id order** (`FOR UPDATE`, one by one, sorted), so
    two events naming the same issues cannot deadlock each other
    (supabase lock-deadlock-prevention). A person's PATCH that races a
    webhook waits on the row lock; the forward-only check runs on the locked
    row, so it sees the person's change.
25. **`mr_links`** is keyed `(issue_id, integration_id, gitlab_project_id,
    mr_iid)` (migration 0004, #1), so two GitLab instances of one org whose
    projects share a numeric id and an MR iid have separate links, and a
    close on one leaves the other alone. Every query on it is scoped by the
    integration whose secret matched. The first link inserts the row and
    adds one `link` attachment (title `MR !{iid}: {title}`, subtitle the
    project path, `creator_id` NULL: GitLab made it, not a user, and the
    column is nullable; built by the T3b code, `linkAttachmentParams`). A
    later event of the same MR updates the row (url, title, branch, state)
    and adds no second attachment (decisions-T3b #15).
    **Stale events cannot move a link back** (review fixes, rounds 2 and 5).
    GitLab can deliver out of order, so `UpdateMRLink` treats an event as
    stale for a link when:
    - the link is `merged` (terminal): a delayed open, update or close
      changes nothing, not even the title, URL or branch;
    - the link is `closed` and the event says `opened` but is not an
      explicit `reopen`: a delayed open, or a delayed "marked ready" update,
      after the close. A real `reopen` opens it again; a `merge` after a
      close is allowed (closed to merged is never a step back).
    A stale event leaves the whole row as it is, and it does not move the
    issue (the update returns the state it left; a mismatch means stale).
    The test reads the row's own state in each SET expression (round 6),
    so an UPDATE that waited on the row lock of a concurrent close sees the
    committed close and stays stale; a FROM-subquery form read the state
    from before the wait and could overwrite the close (tested with a real
    lock wait, `TestGitlabWebhook_StaleOpenDuringConcurrentClose`).
    **Not ordered:** nothing orders events of the same kind. A `reopen`
    delivered after a later `close` (reordered) is still applied: the link
    opens and the issue may move to review. `object_attributes.updated_at`
    could order them; T5 does not.
    Not covered: an event that reaches an issue with no link yet has
    nothing to compare against (a close only updates existing links).
    The MR URL must pass the T3b link check (absolute http/https, no user
    info), or the event is a 400. Control characters in GitLab text become
    spaces.
26. **Unmapped projects** are `202` and one log line with the org, the
    integration id and the numeric GitLab project id: the one value an
    admin needs to map the project. Nothing else from the payload is logged.
    **Nothing is kept** (review fix, round 4): the event row is rolled back
    with the rest of the transaction. So after an admin maps the project, a
    resend of that delivery (same key) is applied instead of being answered
    `duplicate`; an unmapped delivery sent twice is `202` both times. The
    cost: an unmapped project's deliveries leave no trace in the database,
    only the log line.

## Turning it on (cutover, needs the hostname)

1. Pick a 32+ character random secret (for example
   `openssl rand -base64 33`).
2. `POST /api/v1/orgs/orbit/integrations {"provider": "gitlab", "baseUrl": "<GitLab URL>",
   "secret": "..."}` (LAUN-7a replaced `/integrations/gitlab`; omit `secret` to have the server make one, shown once) with an admin token. Note `webhookUrl` in the answer
   (absolute when `PUBLIC_BASE_URL` is set).
3. For each project (LAUN-86 replaced `.../gitlab/{id}/projects`): `POST
   /api/v1/orgs/orbit/integrations/{id}/repos {"repoKey": "<numeric id>", "repoPath":
   "example-org/example-app"}` (webhook-only), or `{"repoId": ...}` from `available-repos` in token mode.
4. In GitLab: Settings, Webhooks, URL = `webhookUrl`, Secret token = the
   secret, trigger "Merge request events" only, SSL verification on.
5. **Do not press GitLab's "Test" button on a mapped project.** What it
   sends was not verified in T5, and it may build the payload from the
   project's latest real MR, which would link or move a real ticket.
   Verify with a throwaway MR instead: a draft MR whose branch and title
   name a throwaway issue of the team, then close it. Check the response
   in Recent events (`linked` and `moved`) and the issue's history. Since
   T20 #65 the response is a status only: check the issue's links and history.
   Record this as a cutover check.
6. **Watch the hook's Recent events** (Settings, Webhooks, Edit, Recent
   events) for the first days: any 5xx or failed delivery must be resent
   by hand ("Resend request"), and a hook that keeps failing may be
   disabled by GitLab. A 5xx keeps nothing, so sending it again is safe.
   A delivery answered `202 unmapped` (look for `"status":"unmapped"`)
   also keeps nothing: map the project, then resend it.
7. **Check the fixtures against real deliveries.** The files in
   `server/internal/gitlab/testdata/` are hand-written from the documented
   shape. Once real MRs have passed, copy one real delivery of each action
   (open, draft open, update, marked ready, reopen, merge, close) from
   Recent events, scrub it (names, emails, avatars, URLs, descriptions),
   and either replace the matching file or diff it against the file and
   fix any field the parser reads (`object_kind`, `project.id`,
   `object_attributes.{iid,action,state,draft,work_in_progress,url,
   source_branch,title,description}`, `changes.draft`). Run
   `go test ./internal/gitlab/ ./internal/api/ -run Gitlab` after.

## Not done, left for later

- No hook is configured on GitLab (no hostname yet). (Written at T5. Hooks are now made in the web app, T17; a hook that exists is mapped by migration 0012. See `docs/deploy.md`.)
- No route to read `mr_links` directly; the link attachment shows the MR on
  the issue. `ListIssueMRLinks` exists for a later issue `include`.
- No delete of an integration (disable it with `enabled: false`). (LAUN-104 added `DELETE /integrations/{id}`.)
- No sweep of `gitlab_webhook_events`. (Rows of `git_webhook_events` older than 30 days are deleted at boot and hourly, LAUN-82.)
- No web screens for the config; the routes are in the OpenAPI file. (Superseded: Settings, Integrations, LAUN-9; see decisions-T17 and `docs/internal/web-decisions.md`.)
