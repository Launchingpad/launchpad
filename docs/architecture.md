# Launchpad architecture

A self-hosted, multi-tenant issue tracker. This is the design of the first version. Rules for each part are in `docs/internal/` (design notes from the Orbit deployment, kept for history).

## Decisions

- Hosted server is the source of truth. Go 1.27.3, chi, pgx/v5 + sqlc, Postgres, numbered reversible migrations, Docker, silo (an S3-compatible object store).
- Multi-tenant orgs in the schema. One install is one org (see "Repo layout").
- A Linear importer brings issue history and numbers from a Linear workspace.
- Git integration (GitLab, GitHub, Bitbucket): MR opened -> In Review, MR merged -> In Testing.
- Clients: MCP server and web app exist. A macOS app and an iOS app are ideas, not built.
- Human login: email one-time code (Postmark). Web uses a cookie session. No passwords.
- Backups: nightly `pg_dump` into a Docker volume (`<project>_backups`) on the same host, kept 30 days. Optional scheduled, encrypted off-site copies (S3, SFTP, WebDAV, Dropbox) are set up under Settings, Platform; see `docs/deploy.md`.
- Cycles: the server creates the next cycle itself (see "Cycles").
- Hostnames are settings in `deploy/.env` (`WEB_HOST` for web and API, `FILES_HOST` for silo), so a rename is a config change. Runbook: `docs/deploy.md`.

## Tenancy and auth

- Application-level tenancy, not RLS. Every table has `org_id`. Every sqlc query takes `org_id`. `RequireOrgMember` resolves `{org}` from the path before any handler. A per-resource cross-org test must return 404.
- Tokens: `odt_` + 43 base62 chars. Store sha256 only, plus a 12-character prefix for listing. Scopes `read`, `write`, `admin`. A token may pin one org. Kinds: `pat`, `session` (web cookie only) and `native` (a native app's login token, bearer only). The bearer header carries `pat` and `native`; the cookie carries `session`; a token of the wrong kind is `401 invalid_token`.
- Users are humans or agents (`is_agent=true`). Agent tokens belong to the agent user. Reviewer agents get `read` tokens.
- Web login: email OTP issues a `session` token. The browser keeps it in an HttpOnly, Secure, SameSite cookie. The server needs CORS rules and CSRF protection for cookie requests. The MCP server and other API clients use bearer tokens.

## Team access

A member sees all teams (`org_members.team_access = 'all'`, the default) or only the teams in `team_members` (`'selected'`). Org admins always see every team. Rules: `docs/internal/decisions-T20-team-access.md`.

- The visible set is read once per request in `RequireOrgMember` (`auth.Org.Access`) from the user, never from the token: a token or agent sees what its owner sees.
- A hidden thing does not exist for the caller. A hidden object in a path is `404 not_found`; in a body it is `400 validation_failed` with the text of an unknown reference; in a filter it gets the answer of an unknown value. The status, body and headers are those of a made-up reference. There is no `403` on team grounds.
- The check is part of the lookup: every scoped query takes `all_teams` and `team_ids` and the same predicate. Every query carries a `-- scope:` tag; opt-outs are listed in `internal/api/testdata/scope_optouts.golden`.
- Every route has a rule in `internal/api/teamrules.go`. Tests A (`TestTeamRulesCoverRouter`), B (`TestTeamRules_RestrictedMember`), C (`TestQueriesDeclareScope`) and D (`TestEventsEmitScope`) fail when a route, a query or an event kind has no rule. Every rule is enforced (`TestTeamRules_AllEnforced`).
- What a restricted member reads:
  - Lists (`teams`, `issues`, `projects`, `cycles`, `documents`, `milestones`, `labels`, `products`, `agent-guidance`) leave out rows of hidden teams, before `LIMIT`. A page is full or it is the last page.
  - A project or product is visible when the caller sees one of its teams, or it has none. Its `teams`, `teamKeys` and `issueCounts` hold visible teams and issues only. `projectCount` of a project label counts visible projects only.
  - An issue's `parent`, `projectId` and `milestoneId` are `null` when they are hidden. Relations to hidden issues are left out. Activity rows that name a hidden issue, project, milestone or cycle have `null` snapshots.
  - The org's `defaultTeam` (in `GET /orgs` and `GET /orgs/{org}`) is `null` when the caller cannot see it.
  - The member list (`GET /users`) and labels with no team are org-wide.
- The change feed (`/events`, `/events/stream`): each event has a scope (`events.scope`, `team_ids`), set by `events.Emit`. A restricted reader gets only events of visible teams, and `seq` has gaps. The stream sends `event: access` when the caller's access changes. Events from before the scope column are placed by a job at boot (`legacy` until then; hidden from restricted readers).
- Notifications, the inbox and mail go only to a member who sees the issue's team. The mail sender checks again at send time.
- Chat services are outside team access (LAUN-231). A service posts the events its admin ticks for its one team to a shared channel, and Launchpad cannot see who is in that channel. Everyone in it sees each post, whether or not they have access to the team. A post is a fixed sentence of what happened, the issue identifier (none for a project reminder), a link (it holds the org slug and the issue identifier, or the project id), and for a state change the names of the old and new state. It holds no title, no comment text, no person's name and no project name; the link needs a login with access. A muted issue posts nothing. A change of the service's team or webhook URL deletes its waiting posts, and the sender skips a post whose issue or project is not of the service's current team. The chat services page warns the admin on every add and edit; the API docs of `POST` and `PATCH .../chat-services` say the same.
- Writes use the same scoped lookups. Assignment and project lead do not check access (a probe would tell who is restricted).
- Git webhooks act with system authority. Their answer is `{status}` only, the same whether or not an issue matched. A repository mapped to a team links and moves issues of that team only. While a member has restricted access (or an open invite would make one), every repository of an enabled integration must name a team: the guard (`tracker.CheckWebhookGuard`) refuses with `409 conflict` a map with no team, clearing a team, enabling an integration with such a repository, and deleting the team of a repository. A close, review, refresh or check changes only links of the repository's mapped team. A backfill is refused on a disabled integration, and on a repository with no team while a member is restricted.
- The Linear importer and the web import job write with system authority. A team they create is visible to all-teams members and to nobody restricted until an admin adds it; they never change a member's access.
- An org admin sets a member's access with `PUT /orgs/{org}/users/{user}/team-access` (LAUN-225, #48 to #50): replace semantics, `dryRun`, `409` while the member would lose a team with open issues assigned and no `assignedIssues` choice (`unassign` or `keep`), and in the same transaction the member's subscriptions, follows and team settings of the lost teams are deleted. An admin target is `400`. The guard of the webhooks runs too. It emits `member.access_changed` (admin scope, ids and counts only) and an audit line. `POST /agents` takes the same `teamAccess` and `teams`. `GET /users` shows `teamAccess` and `teams` to admins only.
- Invites and sign-up approvals carry access (LAUN-226, #26 to #30). `POST /invites` (admin) and `POST /signups/{id}/approve` (optional body) take `teamAccess` (`all`, the default, or `selected`) and `teams` (1 to 50 keys or ids of the org's teams); an admin invite is always `all`. The invite stores them (`org_invites.team_access`, `org_invite_teams`, migration `0041`) and lists the team keys. The accept gives a NEW membership exactly the invite's teams that still exist, in its transaction; the accept body cannot name teams; an existing member keeps their access. When every team of a `selected` invite is gone the accept is `409` and the invite stays open. An open `selected` invite (not accepted, revoked or expired) counts as restricted access for the webhook guard; the accept runs the guard again with a refusal that names no repository (`CheckWebhookGuardNameless`). These flows emit no event (decisions-T12 #14).

## Schema (migration `0001_baseline`)

Full baseline ships at once because the importer needs every table. IDs are UUIDv7. The importer mints them from each entity's original `created_at`.

Tables: `orgs`, `users`, `org_members`, `api_tokens`, `teams` (with `review_state_id`, `testing_state_id`), `issue_counters`, `workflow_states`, `labels` (per org), `projects`, `milestones`, `cycles`, `issues`, `issue_labels`, `issue_relations` (`blocks`, `related`, `duplicate_of`; blockedBy is the reverse read), `comments` (issue or document), `documents`, `attachments` (`link` or `file`), `issue_state_changes` (append-only history), `cycle_snapshots` (daily burn-up), `events` (change feed), `products` and `product_teams` (a product groups teams; many-to-many; `docs/internal/decisions-T18-products.md`), `onboarding_skips` (checklist skips, per org), `git_integrations`, `git_repo_map`, `git_webhook_events`, `mr_links`, `team_git_rules` and `team_git_rule_from_states` (git workflow rules, LAUN-244), `import_mappings`, `idempotency_keys`.

Key rules:

- Identifier is `team.key || '-' || number`. It is computed on read, never stored.
- Numbers come from `UPDATE issue_counters SET next_number = next_number + 1 WHERE team_id = $1 RETURNING next_number - 1` inside the insert transaction. The importer sets a team's counter past its highest imported number.
- `gitBranchName` is computed on read: `{username}/{team-key}-{number}-{slug(title, 40)}`. GitLab matching finds identifiers with the generic pattern in "GitLab integration" below.
- `issues`, `projects` and `documents` have a `version` column for optional compare-and-swap.
- State changes are written in the same transaction as the issue update.
- Burn-up is snapshotted once a day per cycle, and refreshed on demand for today.

## Cycles

- Each team has a cadence: length in days, start weekday and time, and a timezone (for example 14 days, starting Monday 00:00 Europe/London; the importer reads it from the live cycles).
- A daily in-process job creates the next cycle before the current one ends. Cycle numbers are consecutive per team.
- Open issues in a finished cycle roll over to the next cycle, as Linear does. Make this rollover a team setting, on by default.
- The job runs at boot and hourly; boundaries are computed on the team's local calendar, so a cycle does not drift across DST. Details and every judgment call: `docs/internal/decisions-T3.md`.
- The importer reads the live cadence and the history arrays from Linear.

## REST API (`/api/v1`)

Auth: `Authorization: Bearer odt_...` or the session cookie. Errors: `{"error":"msg","code":"wire_code"}`.

| Route | Notes |
|---|---|
| `GET /orgs` | orgs the user belongs to |
| `GET/POST /orgs/{org}/teams`, `GET/PATCH/DELETE .../teams/{key}`, `GET .../teams/{key}/states` | PATCH sets the cycle cadence. DELETE (admin scope) is `409 team_in_use` while the team has issues, or for the only or the default team. Rules: `docs/internal/decisions-T26-delete.md` |
| `GET/POST /orgs/{org}/products`, `GET/PATCH/DELETE .../products/{product}`, `PUT .../products/{product}/teams`, `PUT/DELETE .../products/{product}/teams/{team}`, `GET .../teams/{team}/products` | products, a tier above teams. Read scope reads; admin scope writes. `GET .../issues?product=<slug or id>` filters by all its teams. Rules: `docs/internal/decisions-T18-products.md` |
| `GET .../onboarding`, `PUT .../onboarding/skip`, `PATCH /orgs/{org}` | first-run checklist (admin). The optional `products` step is listed only with two or more teams and no product, and never counts in `remaining`. `docs/internal/decisions-T17-onboarding.md` |
| `GET/POST /orgs/{org}/labels` | |
| `GET/POST /orgs/{org}/issues`, `GET/PATCH .../issues/{idOrIdentifier}` | accepts an identifier such as `ACME-123`, a UUID, or an imported Linear UUID |
| `.../issues/{id}/comments`, `.../relations` | |
| `.../attachments/prepare`, `.../issues/{id}/attachments`, `GET /attachments/{id}`, `GET /attachments/{id}/download` | Rules: `docs/internal/decisions-T3b.md` |
| `DELETE /orgs/{org}/projects/{project}` | admin scope; the project's issues and documents stay with no project; milestones go. Rules: `docs/internal/decisions-T26-delete.md` |
| `.../projects`, `.../milestones`, `.../cycles`, `.../documents` | `GET .../cycles/{id}/snapshots` for burn-up; `.../documents/{id}/comments`. Rules: `docs/internal/decisions-T3.md` |
| `GET /orgs/{org}/users` | members (the roster is org-wide; see "Team access") |
| `PUT /orgs/{org}/users/{user}/team-access` | admin scope: a member's team access (see "Team access") |
| `GET/PUT /me/preferences` | the caller's own theme (`linear`, `paper`, `terminal`; default `linear`), one value per person for every org and device; humans only, no event. The web app sets `<html data-look>` before the first paint from a localStorage copy. Rules: `docs/internal/decisions-T42-themes.md` |
| `.../tokens` | admin scope |
| `GET .../events?after=<seq>`, `GET .../events/stream` | change feed, SSE |
| `POST .../integrations/gitlab/webhook`, `.../github/webhook`, `.../bitbucket/webhook` | verified with `X-Gitlab-Token` or the body signature; no bearer auth. The answer is `{status}` only (decisions-T20 #65). |
| `.../integrations`, `.../integrations/{id}`, `.../integrations/{id}/check`, `.../integrations/{id}/rotate-secret` (POST starts or retries, DELETE discards; `/commit`), `.../integrations/{id}/available-repos`, `.../integrations/{id}/repos` (and `/{repo}`) | admin scope: git integrations (GitLab, GitHub; secrets and tokens write-only), secret rotation with a pending secret, and the repo map |
| `POST /auth/otp`, `POST /auth/otp/verify`, `POST /auth/logout` | human login; `"client":"native"` on verify returns a bearer `native` token instead of cookies |

- Pagination: `limit` (max 250, default 50) and an opaque keyset `cursor`. Response: `{items, nextCursor}`.
- Issue filters: `assignee` (username or `me`), `state`, `stateType`, `team`, `label`, `project`, `cycle` (number or `active`), `parent`, `priority`, `query`, `createdAt` and `updatedAt` (ISO-8601 duration such as `-P1D`, or a timestamp), `orderBy`.
- `GET /issues/{id}?include=relations,attachments,stateHistory,comments`. List responses cut `description` to 400 characters unless `full=true`.
- `Idempotency-Key` header on POST. Same key with a different body returns 409.
- Live updates: SSE with `Last-Event-ID` = `events.seq`. Clients fall back to polling `events?after=`.
- Every list, path, body reference and filter follows the caller's team access ("Team access" above).
- The server publishes an OpenAPI file. The web client is generated from it.

## MCP server (`server/cmd/mcp`)

A Go stdio program that calls the REST API. Config: `LAUNCHPAD_URL`, `LAUNCHPAD_TOKEN`, `LAUNCHPAD_ORG`, `LAUNCHPAD_TEAM`. The `--profile reviewer` flag exposes only `get_issue`, `list_comments`, `get_document`.

Tool names match Linear's: `get_issue`, `list_issues`, `save_issue` (append-only `addLabels`, `blocks`, `blockedBy`, `relatedTo`, `links`), `list_issue_statuses`, `list_teams`, `get_guidance` (read-only), `list_products` (read-only), `list_issue_labels`, `list_users`, `list_cycles`, `list_milestones`, `save_comment`, `list_comments`, `get_document`, `list_documents`, `save_document`, `list_projects`, `get_project`, `save_project`, `list_project_updates`, `save_project_update`, `prepare_attachment_upload`, `create_attachment_from_upload`, `download_attachment` (saves a file attachment to a local folder; 24 tools in the full profile). List calls return `{items, nextCursor}` and cap at 250.

The MCP server has no team logic of its own: it calls REST with the token, so a tool sees what the token's owner sees. Its team list is `GET /teams`, so a team the owner cannot see (also as `LAUNCHPAD_TEAM`) is an unknown team, in the same words as a team that does not exist (decisions-T20 #66; `TestIntegration_RestrictedToken`).

Details and every difference from Linear: `docs/internal/decisions-T2.md`.

## Attachments (silo)

1. `prepare`: check the content type against an allowlist (png, jpeg, gif, mp4, plain text, pdf) and a 100 MiB limit. Return a presigned PUT (15 minutes) for `org/{org_id}/{yyyy}/{attachment_uuid}/{filename}`.
2. The client PUTs the file to silo.
3. `create_attachment_from_upload`: the server checks the object, inserts the row, emits an event.
4. Download: 302 to a presigned GET. Markdown refers to the API download URL, never to a raw silo URL.

The presigned PUT signs the content type and the
size, so silo enforces both; create HEADs the object again before the insert;
plain text downloads as `attachment`, the rest `inline`. Details:
`docs/internal/decisions-T3b.md`.

## GitLab integration

Since T17 GitHub and Bitbucket use the same path and every connection is made in the web app
(Settings, Integrations). This section is the GitLab part; the rest is in
`docs/internal/decisions-T17-git-connections-import.md`.

- `merge_request` events only. Verify `X-Gitlab-Token` in constant time. Unknown projects get 202 and are logged.
- Idempotency: delivery keys in `git_webhook_events` (`gitlab.DeliveryKeys`): a `gl:ik:` key from the `Idempotency-Key` header when sent and a `gl:ev:` key from `X-Gitlab-Event-UUID`, each plus project, MR iid and action. Both are recorded; either seen before makes the delivery a duplicate. Process in one transaction. Return 500 on a DB error: it keeps nothing, and processing the event again is safe. The server does not rely on GitLab sending a failed delivery again.
- Link issues by the regex `(?i)\b([a-z][a-z0-9]+)-(\d+)\b` over the source branch, then title, then description. Keep only matches whose prefix is a team key in the org. Only issues named in the branch or title move; an issue named only in the description is linked but keeps its state.
- Built-in moves: `open` or `reopen` moves to the team's `review_state_id`. `merge` moves to `testing_state_id`. Moves are forward only. Done, Canceled and Duplicate never move back. A team's git workflow rules (`team_git_rules`, `/teams/{team}/git-workflow`) replace the move of an event: opened, ready, reopened, merged or closed to any state of the team, or no move (`docs/internal/decisions-T44-git-workflow.md`).
- Store an `mr_links` row (per integration, migration 0004) and a `link` attachment with the MR URL. Every move writes `issue_state_changes` with `source='gitlab'`.
- Details: a draft MR links but does not move until
  it is marked ready (an `update` whose `changes.draft` goes true to false);
  every other `update` and a `close` move nothing. One integration per GitLab
  base URL per org, each with a write-only secret (sha256) and a project map;
  a mapping's optional team limits linking to that team. Unknown org, no
  integration and a bad token are the same 401. Every judgment call:
  `docs/internal/decisions-T5.md`.
- The answer body is `{status}` only: it never lists or counts issues, and a
  link or move event answers `processed` whether or not an issue matched. The
  deliveries ring (admins) keeps what the event did. While a member has
  restricted team access, every mapped repository of an enabled integration
  must name a team (decisions-T20 #65).

## Linear importer (`server/cmd/importer`)

The web app runs this same importer (Settings, Import from Linear; routes under `/orgs/{org}/import`).
The list below is the command line.

- Export from the Linear GraphQL API, not the MCP, to JSONL files in a directory you choose. Download all attachment files.
- Import writes straight to Postgres through the store layer, so original timestamps survive.
- `import_mappings` makes re-runs safe and lets `get_issue` resolve old Linear UUIDs.
- Rewrite embedded `uploads.linear.app` URLs to the new download URLs.
- `--dry-run` shows create, update and skip counts. A real run compares export counts with DB counts and exits non-zero on a mismatch.
- A user map file turns Linear users into Launchpad users.
- `export`, `import [--dry-run]` and `verify`
  subcommands; exported files are uploaded to silo.
  Details and every judgment call: `docs/internal/decisions-T4.md`.
- The import writes with system authority and never changes a member's team
  access. A team it creates is hidden from restricted members until an admin
  adds it (decisions-T20 #67).
## Repo layout

```
launchpad/
  AGENTS.md  CLAUDE.md  README.md  LICENSE  docs/
  server/   cmd/{server,mcp,importer}  internal/*  db/{migrations,queries,schema.sql}
            sqlc.yaml  Dockerfile  docker-compose.yml  Makefile
  web/      React + TypeScript + Vite
  deploy/   the production stack (deploy.sh, compose, nginx, backup)
```

- Migrations: `NNNN_name.up.sql` and `.down.sql`, applied on boot. The rule "every DB change is a numbered migration" applies from `0001`.
- Local dev: compose with Postgres, silo and the app on ports 8090, 5433 and 9100. No default compose project name. Set `DEV_OPS_PROJECT` in `server/.env` (gitignored), or pass `-p <name>`. See `docs/development.md`.
- Secrets: `DATABASE_URL`, `PORT`, `LOG_ENV`, `S3_ENDPOINT`, `S3_ACCESS_KEY`, `S3_SECRET_KEY`, `S3_BUCKET`, `PUBLIC_BASE_URL`, `POSTMARK_SERVER_TOKEN`. Optional: `S3_PUBLIC_ENDPOINT` (client-facing endpoint), `S3_REGION`. GitLab webhook secrets live in the DB per org.
- First run (`docs/internal/decisions-T8-first-run.md`): while no user exists the server logs a one-time setup link; `POST /api/v1/setup` makes the platform admin, the one org, its first team (nine states, Triage first) and the default labels, and returns a session and the first admin token. One instance is one org.
- Agent users (`docs/internal/decisions-T10-agents.md`): `POST /api/v1/orgs/{org}/agents` makes an agent (no email, so no login code; role `member`), and `server mint-token` (`deploy.sh --mint-token`) mints an org-pinned token on the server by the `POST /tokens` rules. `DELETE /api/v1/orgs/{org}/agents/{user}` deletes an agent (`docs/internal/decisions-T47-agent-edit-delete.md`).

## Risks

1. Importer fidelity (embedded images and history). Mitigation: dry-run counts, an audit map, and the source workspace kept read-only after import.
2. A person moving an issue while a webhook fires. Forward-only moves, history rows and `version` CAS cover this.
3. The tracker holds all ticket history, so backups must be tested with a restore (`make restore-test`).
