# AGENTS.md — Launchpad

Rules for people and AI coding agents who change this repository. Launchpad
is a self-hosted issue tracker. The design is `docs/architecture.md`. The
decision records (why each rule exists) are in `docs/internal/`; the section
for each area below names its file. The production deploy runbook is
`docs/deploy.md`.

## Layout

```
server/
  cmd/server        the API: migrate on boot, setup link, serve; subcommands
                    setup-link, grant-platform-admin, mint-token, offsite
  cmd/migrate       manual `up` / `down` (DATABASE_URL)
  cmd/mcp           the stdio MCP server agents run (calls the REST API)
  cmd/importer      the Linear importer: export, import, verify
  internal/api      route tree (router.go is the route authority) + handlers
  internal/auth     tokens, bearer + session-cookie auth, CSRF, scope checks
  internal/login    email one-time-code login
  internal/tracker  domain logic (issues, comments, relations, teams, projects,
                    milestones, documents, cycles)
  internal/events   change feed: Emit in-transaction, LISTEN/NOTIFY broker
  internal/notify   notifications: consumer, outbox sender, inbox support, chat services
  internal/storage  attachment store: SigV4 presigned URLs (stdlib only)
  internal/netguard the ONE dial-time SSRF guard for outbound requests
  internal/backup, internal/offsite, internal/backupapi, internal/backupnotice
                    scheduled, encrypted backups to remote storage
  internal/gitapi, internal/github, internal/gitlab, internal/bitbucket, internal/scm
                    git host integrations and webhooks
  internal/importer, internal/importjob   the Linear import (CLI and web)
  internal/dbq      sqlc output (generated; never edit)
  db/migrations     NNNN_name.up.sql + .down.sql, embedded, applied on boot
  db/queries        sqlc inputs
  db/schema.sql     generated snapshot (scripts/schema.sh); never edit
  api/openapi.yaml  the published API; the web client is generated from it
web/                the React app (Vite, TypeScript)
deploy/             the production stack: deploy.sh, restore.sh, compose, nginx, backup
docker-compose.yml  the root compose file: GHCR images, host-directory data, no deploy.sh
                    (with .env.example, docker-compose.override.example.yml, scripts/gen-env.sh)
docs/               user and operator docs; docs/internal has decision records
```

## Build and test

Go runs in Docker, so you need no Go on your host (`golang:1.27.3`, Debian,
so `-race` works). A host with Go can use the plain targets.

```bash
cd server
make docker-test                                   # vet, gofmt, unit lane, -race
docker compose -p mydev up -d postgres silo        # always give a project name
make docker-test-integration PROJECT=mydev         # + DB-backed tests
make sqlc-check schema-check                       # generated files are current
make compose-check                                 # compose ports are on 127.0.0.1
make deploy-test deploy-lint restore-test          # the deploy scripts
cd ../web && npm ci && npm run lint && npm run typecheck && npm test && npm run build && npm run openapi:check
```

- DB-backed tests are `*_integration_test.go` and SKIP unless `INTEGRATION=1`.
  A green run without it proves nothing about the database. Each test gets its
  own freshly migrated database (`internal/testdb`).
- Do not pipe test output through `tail`; check the exit code.
- Smoke against a running stack: `BASE_URL=... TOKEN=... make smoke`.

## Compose project name — required

`server/docker-compose.yml` names no default project. Give one with
`docker compose -p <name> ...`, or `DEV_OPS_PROJECT=<name>` in `server/.env`
(gitignored). Use your own name per checkout, so one `down -v` never deletes
another stack's volumes. Host ports: app 8090, postgres 5433, silo 9100;
override with `DEV_OPS_APP_PORT` / `DEV_OPS_PG_PORT` / `DEV_OPS_S3_PORT`. All
are published on **127.0.0.1 only** (`make compose-check` fails otherwise):
the stack uses fixed passwords and logs login codes. The production stack
(`deploy/`) is a different compose project; its default name is `launchpad`.

## First-run setup

There is no seed. While the database has no user, every boot logs ONE setup
link, `setup: open <PUBLIC_BASE_URL>/setup#<token>` (24 hours, single use).
It is the only credential the server ever logs. `POST /api/v1/setup` makes the
first user (a platform admin and the org's admin), the org, its first team and
a first admin token. Rules: `docs/internal/decisions-T8-first-run.md`.

- Local stack: `PROJECT=<name> scripts/dev-setup.sh` (in `server/`) completes
  setup (settings by environment: `SETUP_USERNAME`, `SETUP_NAME`,
  `SETUP_EMAIL`, `ORG_SLUG`, `ORG_NAME`, `TEAM_KEY`, `TEAM_NAME`) and prints
  the first token.
- A new link: restart the app, or `server setup-link` (production:
  `deploy/deploy.sh --setup-link`).
- Tests get their data from `testdb.Fixture(t, pool)` (org `orbit`, team
  `ORB`, a human admin and an agent). No server ever makes it.

## Conventions

- Errors are `{"error": "...", "code": "..."}`. Codes live in `internal/httpx`
  (`Codes`) and in the `ErrorCode` enum in `api/openapi.yaml`; a test keeps
  them equal.
- A new route goes in `internal/api/router.go` AND `api/openapi.yaml`.
  `TestOpenAPIMatchesRoutes` and `TestOpenAPISchemasMatchModels` fail on
  drift. After an OpenAPI change: `cd web && npm run openapi`.
- Cross-org access is `404 not_found`; a body reference to another org's row
  is `400 validation_failed`. Add a cross-org test for every new resource.
- Writes that change state append an event with `events.Emit` as the LAST
  statement of their transaction. Install-level settings and per-user private
  rows emit none (see the decision records).
- `Idempotency-Key`: mount `s.idem` AFTER the route's scope check, never on a
  group. A route whose response holds a secret uses `idempotency.Reject`.
  Write paths go through `database.InTx`.
- Store no caller IP and no User-Agent anywhere.
- Never log, emit or store a secret: no login code, token, session, CSRF
  value, sealed key or password in a log line, an event payload or an
  idempotency row.
- IDs are `uuidv7()`. They are not monotonic within a millisecond: never use
  `ORDER BY id` to mean insertion order.
- Outbound requests to a user-entered host go through `internal/netguard` and
  `internal/gitapi` (or `backup.NewHTTPClient` / `backup.DialSSH`). Never
  build a second HTTP client or dialer for such a host.

## The database rule — every change is a numbered migration

- A schema or reference-data change is a new `db/migrations/NNNN_name.up.sql`
  with a matching `.down.sql`. Numbers are contiguous; never skip one.
- Never edit a migration that has shipped. Write a follow-up.
- The `down` must reverse the `up`, or carry a comment saying why it cannot.
  `TestMigrateUpDownUp` runs up, down, up and compares the schemas.
- After a migration: `make sqlc` (if queries changed) and `make schema`, and
  commit `internal/dbq` and `db/schema.sql`. CI fails when either is stale.
- Postgres 15 or newer is required (16 in compose and CI).
- No RLS. Tenancy is in the application: every tenant table has `org_id`,
  every query filters by it, and org-scoped references are composite
  `(org_id, x_id)` foreign keys.

## Safety rules that tests enforce

- Only `_test.go` files may import `netguardtest` (it opens private ranges
  for `httptest`). `TestNoProductionBypass` greps for it.
- The public webhook routes (GitLab, GitHub, Bitbucket) stay OUTSIDE the
  `Authenticate` group. No rate limiter and no `Idempotency-Key` middleware
  goes in front of them, and nothing answers before the secret or signature
  check.
- `internal/tracker`, `events`, `setup`, `signup`, `login` and `importer`
  never import `internal/notify` (`TestNoWritePathImportsNotify`). `notify`
  never logs or stores an address, a token or a message body; its logs and
  `last_error` hold counts, ids and fixed words.
- `BearerSource` never carries token kind `session`: the web cookie value sent
  as a bearer is `401`. A native login (`"client":"native"`) from a browser
  (`Origin`, `Sec-Fetch-Site` or `Sec-Fetch-Mode` present) is refused BEFORE
  `Verify` with the plain `invalid_code` answer. Never move that check.
- Every notification route (settings, follows, subscriptions, the inbox and its
  stream) answers `403 forbidden` for an agent token (`refuseAgents`). Keep it
  on any new notification route.
- The one-click unsubscribe token appears in the mail and in one request path;
  `logPath` hides it from the logs. Never log that path another way, and never
  store the token except as a sha256.
- Backup secrets are sealed through `platform.BackupKeyring` only. Never use
  `Keyring` or `GitKeyring` for a backup secret.

## MCP server binary

- Install it from a GitHub Release with `scripts/install-mcp.sh` (it checks the sha256 and replaces the file
  with `mv -f`; never `cp` over a binary that has run, macOS can kill it). Cut a release: `docs/releasing-mcp.md`.
- **Claude Code plugin:** `.claude-plugin/marketplace.json` (marketplace `launchpad-mcp`) lists `plugins/launchpad/`
  (`claude plugin install launchpad@launchpad-mcp`). `scripts/launchpad-mcp` (not `bin/`: that lands on the agent PATH)
  fetches the release binary once per plugin version with `lib/install-mcp.sh`, which MUST equal
  `scripts/install-mcp.sh` (the test `cmp`s them). Plugin `version` = release tag without `v` (`release.yml` checks).
  Test: `scripts/test-launchpad-plugin.sh`. Rules: `decisions-T2.md` #53 to #61.
- A GitLab `mcp-darwin` CI artifact is CI-only, not a supported install. From source: `cd server && make build-mcp`.
- In a sample `.mcp.json` the command is `/home/you/.local/bin/launchpad-mcp`. The token comes from the
  environment, never from a file.

## Areas and where their rules are

| Area | Record in `docs/internal/` | Key rule |
|---|---|---|
| Web login | `decisions-T6a.md` | Email one-time code; cookie session `__Host-odt_session` + CSRF; a native login returns the token in the body and is refused from a browser. |
| Platform mail settings | `decisions-T9-mail-settings.md` | Secrets are write-only and sealed with `SETTINGS_ENCRYPTION_KEY`; never log them. |
| Sign-up, invites, platform admins | `decisions-T12-signups-invites.md` | Public routes never say whether an email or username exists; keep the 300 ms floor and the limits. |
| Agent users, `server mint-token` | `decisions-T10-agents.md`, `decisions-T11-web-tokens.md` | An agent has no email and cannot log in; an agent token must expire. |
| Onboarding checklist, org settings | `decisions-T17-onboarding.md` | `done` is computed on the server; the body holds booleans and ids only. |
| Project labels | `decisions-T28-project-labels.md` | Org-wide, optional one-level groups; the catalogue is admin scope, `labelIds` on a project sets them; another org's label is 404 in a path and 400 in a body. |
| Team access | `decisions-T20-team-access.md` | A member (or agent) sees `all` teams or `selected` ones; org admins see all. A hidden row is `404` like a missing one (never `403`, no new code, texts name nothing hidden); every query carries a `-- scope:` tag and every route a rule in `internal/api/teamrules.go` (Tests A to D fail on drift); every event has a scope; the streams send `event: access` (no id) on a change, and the web then refetches the org's lists and the inbox; only org admins change access (`PUT .../team-access`, `dryRun` first); no restricted access while an enabled repository has no team (the webhook guard); a PAT never exceeds its owner; no "request access" control. |
| Products | `decisions-T18-products.md` | A named group of teams; with none, an org behaves as before. |
| Notifications | `decisions-T13-notifications.md` | The consumer reads the change feed; no write path imports `internal/notify`. Agents get nothing. |
| Slack channel | `decisions-T33-slack-channel.md` | Superseded by chat services for the model, the routes and the page (see the next row); still the record for the post content, the event list, the sender and the secret rules. |
| Chat services (Slack, Discord, custom) | `decisions-T36-chat-services.md`, `decisions-T37-chat-every-event.md`, `decisions-T38-chat-service-team.md` | Up to 10 org services (`/chat-services`), each in ONE team (`teamId`, required; an event posts only to its team's services, no team posts nowhere, a team with services is `409 team_in_use` on delete, `decisions-T38-chat-service-team.md`), each name, kind, sealed write-only webhook URL (`platform.SlackKeyring`, scope per service, old Slack blobs keep the org scope), events and an on/off switch; never returned, logged or emitted; `slack` and `discord` hit fixed hosts, `custom` any public https host (save-time refusals shared with push, `netguard` public-only at dial, no redirect); a post is a fixed sentence, an identifier and a link, no title or name; a service posts every ticked event whoever the issue belongs to or made the change, but a muted issue posts nothing (`decisions-T37-chat-every-event.md`); agents get no notifications and no access to the routes. `fluxer` is its own kind: the Discord body, any public https host under the `custom` rules, path `/api/webhooks/<id>/<token>` or `/api/v1/webhooks/<id>/<token>` (T36 #6). |
| Personal webhooks | `decisions-T40-personal-webhooks.md` | A member's own webhooks (`/me/webhooks`, up to 3 per member and org): only the owner sees them, no event, agents `403`; the chat kinds, URL checks, poster and keyring (`platform.PersonalColWebhook`, scope per webhook); post on assignment to me or a change on an issue I own (assignee or creator), never my own change, a muted issue or a team I cannot see (`ListNotifyEligibleUsers`), re-checked at send time (`access_removed`). |
| Mobile push | `decisions-T35-mobile-push.md` | A relay URL and bearer secret, sealed and write-only (`platform.PushKeyring`), never returned, logged or emitted; the host cannot be allowlisted, so `netguard` public-only is the control (no redirect, no proxy); the body is a bare wake (token, platform, opaque id); devices are per member, capped, humans only; agents get nothing. |
| Projects, cycles, documents | `decisions-T3.md` | Cycles are off until an admin turns them on; tests inject the clock. |
| Cycle options (dates, complete, delete) | `decisions-T27-cycle-options.md` | Every manual cycle write takes the team cycle lock the job takes; the chain stays contiguous; complete and delete are admin scope. |
| Project update schedule | `decisions-T30-project-update-schedule.md` | Project updates (health + body, create/list/read, no edit); a per-project reminder schedule: the job emits `project.update_due` once per due time, the notify consumer makes the notification (inbox + email, lead only, never an agent). |
| Attachments | `decisions-T3b.md` | Files go to the object store with presigned URLs, never through the server. |
| MCP server | `decisions-T2.md` | `cmd/mcp` calls REST only; tool schemas are pinned in `internal/mcp/testdata/*.golden.json` (update with `-update`, then review the diff). |
| Linear importer | `decisions-T4.md`, `decisions-T4b.md`, `decisions-T17-git-connections-import.md` | Read-only against Linear; the key is memory only; `--in`, `--out` and `--org` have no default. |
| Git hosts and webhooks | `decisions-T5.md`, `decisions-T17-git-connections-import.md` | Webhooks are public and checked by secret or signature BEFORE the body is read; fixtures are written from documentation, not recorded deliveries. |
| Backups | `decisions-T16-backups.md` | Secrets sealed with `BACKUP_ENCRYPTION_KEY` through `platform.BackupKeyring` only; the runner writes only its granted tables. |
| Manual order, activity, triage, labels, view preferences, avatars | `decisions-T14` to `T24` | See the file for the area. |
| Project statuses | `decisions-T29-project-statuses.md` | Six fixed kinds, custom statuses per org; `projects.state` is the kind mirror, `status_id` the status; a status in use or the last of its kind is `409`. The settings page is admin only; members read statuses in the pickers. |
| Project emoji | `decisions-T41-project-emoji.md` | One optional emoji per project (one emoji sequence, at most 32 bytes, checked on the server); the icon order is logo, then emoji, then letter; a change is `project.updated` with `"emoji"` in `changes`, never the value. |
| Delete projects, teams | `decisions-T26-delete.md` | Admin scope; a team with issues, the last team or the default team is `409 team_in_use`; a project delete detaches its issues. |
| Agent guidance | `decisions-T31-agent-guidance.md` | Admin-written text per org and team, read by any token, handed to agents by the MCP `get_guidance` tool; it is data, never a permission; events never carry the text. |
| Delete issues | `decisions-T32-delete-issues.md` | Admin scope, hard delete, sub-issues kept without a parent, `issue.deleted` with ids only; no MCP tool. |
| Connected accounts | `decisions-T34-connected-accounts.md` | A member's git handle per connection is a claim: display attribution of change requests only, never a permission. One holder per handle per connection (`409` names nobody); admins list and remove links. Own routes refuse agents and emit no event; the admin unlink emits ids only. |
| Themes (Linear, Paper, Terminal) | `decisions-T42-themes.md` | A per-person, install-wide theme in `user_preferences` (`GET/PUT /me/preferences`, agents `403`, no event); the web sets `<html data-look>` before first paint from `public/theme-init.js` (no inline script, the CSP); Linear sets no attribute and must stay pixel-identical; every theme variant redefines every colour token and passes `themeContrast.test.ts`; fonts are self-hosted OFL woff2 (`web/src/assets/fonts`, `THIRD_PARTY_NOTICES.md`), never fetched from a third party. |
| Git workflow rules | `decisions-T44-git-workflow.md` | Per team and event (opened, ready, reopened, merged, closed): a target state of the team or no move, optional "only from" states; no rule is the built-in move (review / testing anchors, forward only); a rule with no from set is forward only, only a listed state can leave Done or Canceled; a rule moves once, when the event changes the link; an archived target is flagged and never followed; members read, org admins write, agents neither; `team.updated` with `changes: ["gitWorkflow"]`. |
| Docker distribution (root compose file) | `decisions-T45-docker-distribution.md` | The root `docker-compose.yml` runs without deploy.sh: project `launchpad-compose` (never `launchpad`), images `ghcr.io/launchingpad/launchpad-{api,web}:${LAUNCHPAD_VERSION}` (required, no `latest`), data in host directories (`LAUNCHPAD_*_DIR`); `check-compose.py --root` allows ONLY those bind mounts and the two read-only config files, and a drift test keeps the shared services equal to `deploy/docker-compose.yml`; `silo-init` makes the bucket and user; no `offsite` worker; `scripts/gen-env.sh` never overwrites and never prints a secret. |
| Move an install (export, restore) | `decisions-T46-move-server.md` | Same release on both servers, passphrase made by the server, sealed columns re-sealed, backups off after import, no write freeze; `POST /platform/move/export` gives the passphrase and a one-time ticket (memory only, sha256 key, 5 minutes, bound to the caller; humans only, agents `403`); `GET .../export/{ticket}` streams the bundle under one advisory lock; `POST /setup/restore` is public, checks the setup link (header) BEFORE reading the body, streams the upload, takes the passphrase from a header or the first form part (never the query), and mints the session and an admin token in the import transaction. Never log or store the passphrase, ticket or link. |
| Native apps | `decisions-T22-native.md` | Design only. |
| Web app | `web-decisions.md` | Page and component decisions. |

## Production deploy

`deploy/deploy.sh` is the one-command production stack: Postgres 16, silo
(S3 API), the API and the web app behind nginx. Runbook (first deploy,
update, backup and restore, rollback): `docs/deploy.md`.

- `deploy/.env` (mode 0600, gitignored, on the server only) holds the
  generated secrets. A re-run keeps every value. Never commit or print it.
- `WEB_HOST` and `FILES_HOST` have no default: deploy.sh asks on a terminal,
  else read them from the environment.
- The label key `dev.orbit.deploy=production` on the compose volumes is a
  historic name kept on purpose: the project guard refuses volumes without it.
- Checks: `make compose-check`, `make restore-test`, `make deploy-test`,
  `make deploy-lint`. To try the real stack, give it its own project, high
  ports and fake host names, and remove it with `--nuke` (see
  `docs/deploy.md`). Never `--nuke` an install that holds real data.

## Review and commits

- Keep a change small and about one thing. Add or change tests with it.
- Run `make docker-test` (and the integration lane for a database change)
  before you push. Run the web checks for a web change, and the deploy checks
  for a change in `deploy/`.
- Do not commit a secret, a real hostname, a person's email address or a
  token. Use `example.com` style values in tests, docs and fixtures.
- Releases and the public export: `docs/releasing.md`.
