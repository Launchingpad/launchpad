# T17 decisions (LAUN-1): git connections and the Linear import in the web app

LAUN-1 is the design for two things an org admin does today only on the
command line or by script, and the design for more git hosts. It adds no
code. Each numbered call (`#N`) has its reason, so a later ticket can change
it on purpose. The tickets are numbered separately ("ticket N") in the last
section.

The architect's first design text (2026-10-25) was never saved. This note
rebuilds it from LAUN-1, its two comments, the child tickets LAUN-3 to
LAUN-11 and the code, and adds the project owner's scope addition of 2026-10-26 (GitLab,
GitHub and Bitbucket, hosted and self-hosted, with the MR state on the
ticket). Where this note and a child ticket disagree, this note wins; the
tickets are rewritten in "Ticket split".

The request (2026-10-25): all of the setup and import done so far should be possible in the web app,
and GitHub connections should be supported too.

## What exists today (checked in the code)

| Part | State | Where |
|---|---|---|
| First-run setup, mail settings, platform admins, agents, tokens, invites, sign-ups, create team | Web and API | T8, T9, T10, T11, T12 |
| Linear export, import, verify | Command line only (`cmd/importer`); needs `DATABASE_URL` and the file store. Exits 3 on a mismatch. | `internal/importer`, T4, T4b |
| GitLab integration | When LAUN-1 was written: REST only, `/integrations/gitlab` (create, list, patch), `.../projects` (replaced by LAUN-7a and LAUN-86: `/integrations`, `rotate-secret`, `/repos`). One public webhook. Secret stored as sha256 only. Hooks are added by hand in GitLab (a glab script). | `internal/gitlab`, `internal/api`, T5 |
| Provider code | `internal/gitlab` holds the parser, the matcher (`Refs`, `MRRefs`, `LinkRef`) and the plan (`Plan`, `Move`). `tracker.HandleGitlabMergeRequest` applies it. | |
| Tables | `gitlab_integrations`, `gitlab_project_map`, `gitlab_webhook_events`, `mr_links` (keyed by integration, project id, MR iid). `issue_state_changes.source` and `issue_activity.source` allow `gitlab`, not `github`. | `db/schema.sql` |
| Sealed secrets | `internal/platform/crypto.go`: AES-256-GCM, HKDF info strings, one key `SETTINGS_ENCRYPTION_KEY`. | T9 |
| Cycles on | `PATCH /orgs/{org}/teams/{team} {"cyclesEnabled": true}`; the web switch shipped in LAUN-3 (`TeamCycles.tsx`). | T3 |
| Latest migration | `0010_notifications` when this was last checked (numbers are taken at merge time). | `db/migrations` |

## Goal

An org admin can, in the web app only: run the Linear import (dry run, real
run, verify, progress), connect GitLab, GitHub and Bitbucket (token, repo
mapping, the webhook made for them) and turn cycles on. Every provider gives
the same ticket moves as T5, and the ticket shows the state of its MR.

## Decisions the project owner made (2026-10-25 and 2026-10-26)

C1. **GitHub by personal access token, plus a webhook-only mode.** A GitHub
   App is deferred (open point 1).
C2. **The old `/integrations/gitlab` config routes are replaced in one
   tranche.** The webhook URL does not change. The glab hook script retires.
C3. **The only way to START an export from the web is the pasted-key, server-side export.** The CLI
   stays for offline exports (adopting one is #44).
C4. **Private address ranges are blocked by default.** `GIT_ALLOW_PRIVATE_PROVIDERS=1`
   overrides it. See #31: this note
   joins it to the backup guard so there is one guard.
C5. **Disable stops deliveries only.** Hooks stay on the host until a repo is
   unmapped.
C6. **Three providers, hosted and self-hosted** (comment of 2026-10-26): GitLab
   (gitlab.com, self-managed), GitHub (github.com, Enterprise Server),
   Bitbucket (bitbucket.org, Data Center). No provider has one fixed base URL.

## Components

```
internal/scm        neutral types and the one shared writer path
                    Event, Plan, Move, Ref, LinkRef, Refs, MRRefs, forward-only,
                    Provider interface, registry (pure, no DB, no HTTP)
internal/gitlab     Provider: parse, verify (X-Gitlab-Token). No HTTP client.
internal/github     Provider: parse, verify (HMAC-SHA256). No HTTP client.
internal/bitbucket  Provider: parse (Cloud and Data Center), verify (HMAC). No HTTP client.
internal/gitapi     ALL outbound HTTP, in one place: the guarded client (caps, timeouts,
                    retry rules) and the three provider API clients (gitapi/gitlab,
                    gitapi/github, gitapi/bitbucket). The Provider interface's `Client` method
                    returns one of these; the parser packages never dial.
internal/netguard   the ONE dial-time SSRF guard (#31); gitapi and
                    internal/backup both import it
internal/tracker    HandleSCMEvent (one transaction), integration CRUD, repo map,
                    MR state, delivery ring
internal/importjob  the import job runner (#36): sessions, jobs, progress
internal/importer   API unchanged; a progress callback and a ctx check (#40); linear/ is the export
internal/api        routes (router.go is the authority), webhook handlers
web/                Settings > Integrations, Settings > Import from Linear,
                    Settings > Teams (cycles switch)
```

### The provider interface

P1. **One interface, one implementation per host family.** All in
   `internal/scm`; pure where possible.

   ```go
   type Provider interface {
       Name() string                         // "gitlab", "github", "bitbucket"
       // Verify checks the delivery against ONE secret. It sees the headers and
       // the body (GitHub and Bitbucket sign the body; GitLab compares a token).
       Verify(h http.Header, body []byte, secret []byte) bool
       // Parse turns a delivery into a neutral Event, or reports "ignored".
       Parse(h http.Header, body []byte) (Event, ParseResult, error)
       // DeliveryKeys are the dedupe keys of a delivery (see #14).
       DeliveryKeys(h http.Header, ev Event) ([]string, error)
       // Optional API client, nil when the integration has no token.
       Client(cfg ClientConfig) (APIClient, error)
   }
   type APIClient interface {              // all calls go through internal/gitapi
       CheckToken(ctx) (Identity, error)
       ListRepos(ctx, query string, cursor string) (RepoPage, error)
       EnsureHook(ctx, repo RepoRef, hook HookSpec) (HookRef, error)
       DeleteHook(ctx, repo RepoRef, hook HookRef) error
       GetRepo(ctx, id string) (RepoRef, error) // id is OPAQUE, made by ListRepos
       GetChange(ctx, repo RepoRef, number int64) (Change, error)
       ListOpenChanges(ctx, repo RepoRef, cursor string) (ChangePage, error) // backfill, newest first
       // PostStatus / PostComment exist only behind the post-back switch (#22).
   }
   ```
P2. **The neutral event.** `scm.Event`: `Provider`, `Kind` (`change`, `review`,
   `checks`), `RepoKey` (the provider's stable id as text, #8), `RepoPath`,
   `Number` (MR iid, PR number), `Action` (`opened`, `reopened`, `ready`,
   `drafted`, `updated`, `merged`, `closed`, `approved`, `unapproved`,
   `checks`), `URL`, `Title`, `SourceBranch`, `Description`, `Author`
   (login only is stored, #18; a display name is dropped when the event is stored), `Draft`, `Fork` (true when the head repo differs from the base repo) and `SourceRepoKey`, `HeadSHA`, `Review`
   (`none|approved|changes_requested`), `Checks`
   (`none|pending|success|failure`), `OccurredAt`. The event carries no other
   host data (T5 #14 rule: the store never becomes a copy of host content).
P3. **Shared writer.** `tracker.HandleGitlabMergeRequest` becomes one
   `HandleSCMEvent`. The matcher, the forward-only rule, the `mr_links` writer,
   the stale-event rules (T5 #25) and the link attachment code stay shared and
   untouched. Ticket 1 is this pure refactor.

## Rules kept from T5

1. These rules are the project owner's T5 calls, kept as they are.
2. **Webhook routes stay public and check the secret or signature first.** No
   bearer, no session, no membership, no rate limiter and no Idempotency-Key
   middleware in front (T5 #9, #10). Nothing may answer before the check. **The one exception is the signed-body path of #14** (GitHub and Bitbucket): for an org that has an enabled integration of that provider, `411`, `413` and `503` can come before the signature check, because the body must be read to check it. That reveals that the org has such an integration, a small oracle that T5 #7 already accepts for slugs.
3. **Only the source branch and the title move a ticket.** An issue named only
   in the description is linked and keeps its state. The matcher regex and the
   caps (25 issues, 1000 matches) are T5 #21, #22, unchanged.
4. **Forward only.** Never out of Done, Canceled or Duplicate; never into an
   archived state; an archived issue is not changed (T5 #17, #18). A merge
   moves to In Testing even with other open MRs (T5 #19).
5. **`merged` is terminal** for a link; a closed link opens again only on an
   explicit reopen (T5 #25). A stale event moves nothing.
6. **One event, one transaction.** The delivery row is the FIRST write; the
   events are the LAST writes (T5 #12, AGENTS.md conventions). A DB error is
   500 and keeps nothing; processing an event again is safe.
7. **Answers** keep T5 #11: `200 processed | ignored | duplicate`, `202
   unmapped`, body `{status, linked, moved}`. An unknown org, an org with no
   enabled integration of that provider, and a bad secret or signature all give
   the same `401 unauthenticated`. **Superseded by T20 #65**
   (`decisions-T20-team-access.md`, LAUN-224): the body is `{status}` only,
   and the status does not depend on whether an issue matched.

## Data model and migrations

Migration letters (A, B, C, D) are labels only. Migration numbers are taken at merge time (the tickets name numbers that are
stale; the latest merged when last checked is `0010_notifications`). Below, "migration A to D"
are in build order. Each has a matching `.down.sql`. Never edit one that has
shipped.

8. **Migration A: provider-neutral tables, and the port of the GitLab code.**
   All of this is ONE migration, and ticket 3 ports every GitLab query, route
   handler and test to the new names in the same MR (the old config routes keep
   working on the renamed tables until ticket 5 replaces them; the live webhook
   answers the same).
   - **Renames, explicit.** Tables: `gitlab_integrations` to `git_integrations`,
     `gitlab_project_map` to `git_repo_map`, `gitlab_webhook_events` to
     `git_webhook_events`. Every constraint and index that carries the old name
     is renamed (`ALTER ... RENAME CONSTRAINT`, `ALTER INDEX ... RENAME`), so
     `schema.sql` holds no `gitlab_` name that is now wrong:
     `gitlab_integrations_org_base_key`, `gitlab_integrations_org_id_id_key`,
     `gitlab_integrations_hash_len`, `gitlab_project_map_project_key`,
     `gitlab_project_map_org_integration_idx`, `gitlab_project_map_team_idx`,
     `gitlab_webhook_events_uuid_key`, `gitlab_webhook_events_received_idx`, `gitlab_webhook_events_status_check`, the three primary keys (`gitlab_integrations_pkey`, `gitlab_project_map_pkey`, `gitlab_webhook_events_pkey`), every `*_org_id_fkey`, `*_integration_fk` and `*_team_fk` foreign key, A test lists the constraint names. (`mr_links_unique` and `mr_links_state_check` have no `gitlab_` name and are not renamed; the unique key follows the column rename.)
   - `git_integrations`: add `provider text NOT NULL DEFAULT 'gitlab'` (CHECK
     `gitlab|github|bitbucket`), `flavor text` (`cloud|self_hosted`, a label for the
     UI), `mode text NOT NULL DEFAULT 'webhook_only'` (CHECK `token|webhook_only`, and CHECK `mode <> 'token' OR token_enc IS NOT NULL`),
     `api_base_url text`, `name text`, `token_enc bytea`, `webhook_secret_enc bytea`
     and `webhook_secret_next_enc bytea`, and `webhook_secret_next_hash bytea` (CHECK: NULL or `octet_length = 32`, as `webhook_secret_hash`) (#11: a pending secret during a
     rotation, with its hash, because the lookup and the T5 #5b duplicate check work on hashes), and
     `attention_code text` (our own codes only: `secret_unreadable`, `token_rejected`,
     `attention_unreachable`, `hook_failed`; NULL is `ok`; #11a says when it is set and cleared; the column is created here, in ticket 3, and ticket 5 sets and clears it).
     **The row id is generated in the app** (`uuidv7()` in Go, not the column default) and used in the
     INSERT, because the AAD of a sealed column contains the row id (#11).
     Keep `webhook_secret_hash` (32 bytes; the constant-time lookup and the T5 #5b
     duplicate check). The unique key `(org_id, base_url)` becomes
     `(org_id, provider, base_url)`. Existing rows keep `mode = 'webhook_only'` and
     NULL sealed columns (the secret was never stored): GitLab verification compares
     hashes (#11), so they keep working; the first rotate in the web app stores the
     sealed copy.
   - `git_repo_map`: `gitlab_project_id bigint` becomes `repo_key text`
     (`ALTER COLUMN ... TYPE text USING gitlab_project_id::text`); `project_path`
     becomes `repo_path`; add `hook_id text`, `hook_state text` (CHECK
     `none|active|failed`, default `none`) and `hook_error text` (a short code of
     ours, never host text).
   - `git_webhook_events`: `gitlab_project_id` becomes `repo_key text` (same cast);
     add `integration_id uuid` (backfilled with the org's oldest integration, then
     NOT NULL; rows of an org with NO integration are deleted first (nothing could have written them); this can mis-attribute a row of an org with several integrations, and T5 notes the same
     header value can come from several hooks, so the backfill is approximate and the table is a
     dedupe key store only, never shown; the DOWN restores `(org_id, event_uuid)` and so first deletes
     all but the oldest of any rows that would collide, composite FK to `git_integrations (org_id, id)` ON DELETE CASCADE). The unique key becomes `(org_id, integration_id, event_uuid)` (it was `(org_id, event_uuid)`; this closes the T5 #12b gap, and the rule everywhere in this note is the new one). **Retention:** rows older than 30 days are deleted by a prune that **ticket 3 owns** (boot plus hourly, started in `cmd/server`, as `internal/notify` does for LAUN-31; dedupe keys only need the hosts' retry windows; T5 had no sweep). It is in the git lane, so the git lane does not depend on the import lane's sweep (#39). **Runbook line (ticket 13):** during a rolling deploy an old instance still writes unprefixed `event_uuid` values, which will not dedupe against the rewritten rows; deploy the one-instance stack (as `deploy.sh` does), or accept one duplicate window. The `event_type`, `status` and `payload` columns and their CHECK carry
     over; the T5 #14 rule stays: `payload` holds a summary only (`{action, state,
     number, draft}`), never a title, description, branch, URL or user. Existing
     `event_uuid` values get the `gl:` prefix in this migration (a one-time rewrite,
     #17); the down strips it.
   - `mr_links`: `gitlab_project_id` becomes `repo_key text` (cast as above);
     `mr_iid integer` becomes `number bigint` (`USING mr_iid::bigint`); the unique key
     `(issue_id, integration_id, repo_key, number)` and `mr_links_mr_idx` follow. The
     MR-state columns are NOT here: they are migration D (ticket 11, #18).
   - `issue_state_changes.source` and `issue_activity.source` CHECKs gain `github`
     and `bitbucket`.
   - **One down policy for the whole tranche (A, B, C, D): a down deletes or
     rewrites what the old schema cannot hold, and the `.down.sql` says so in a
     comment. No down refuses.** Concretely for A: rows of `git_integrations` with
     provider other than `gitlab`, and their repo map, events and links, are deleted
     (cascade); `repo_key` of GitLab rows casts back to bigint (the API validates that a GitLab `repoKey` matches `^[0-9]+$`, `400 validation_failed` otherwise, including in webhook-only mode; and the down deletes any GitLab map row, event row or link whose key is not numeric, per the down policy); `issue_state_changes.source` and `issue_activity.source` values
     `github` and `bitbucket` are rewritten to `system` (history is kept); the sealed
     columns are dropped.
   - **No compatibility view.** All code moves in one tranche (C2).
     `TestMigrateUpDownUp`, a backfill test like `TestMigration0004`, and a test that
     inserts a GitHub integration row directly (there is no create route before
     ticket 5) cover it.
9. **Migration B: deliveries ring** (its `(org_id, integration_id)` FK is ON DELETE CASCADE). `git_webhook_deliveries`: `id`, `org_id`,
   `integration_id` (nullable: a bad-signature row has none; the composite FK `(org_id, integration_id)` tolerates NULL), `provider`, `received_at`, `result` (`processed|ignored|duplicate|
   unmapped|bad_signature|bad_request|error`), `event` (text, the provider's
   event name), `repo_key`, `http_status`, `error_code` (our code, never the
   body). Summary only: no payload, no title, no URL, no branch, no user. Ring
   of 100 per integration (rows with a NULL `integration_id` are trimmed per org and provider, also to 100), trimmed in the same statement that inserts. It is
   written AFTER the event transaction, in its own short statement, so a
   failure to write it never changes the answer and a rolled-back event
   leaves a row that says `error`. A bad signature cannot be tied to one integration (the signature did not match any), so its ring row has a NULL `integration_id` and an `org_id` and `provider`, and is shown on the provider's list page. The org slug is public, so a row per bad request would let an attacker rotate the ring and hide real failures: the throttle is one `bad_signature` row per org and provider per minute. The throttle is in memory, before any DB write; the rest are counted in memory and dropped on restart. No event is emitted (see #52).
10. **Migration C: import sessions and jobs** (#37): `import_sessions`,
    `import_jobs`, and the partial unique index that gives one active job per
    org.
11. **Secrets: sealed, write-only, with the hash kept.** `token_enc`,
    `webhook_secret_enc` and `webhook_secret_next_enc` are sealed with AES-256-GCM.
    **This needs new code in `internal/platform/crypto.go`, not just new strings.**
    `platform.Keyring` today has fixed HKDF info strings and an AAD that is always
    `"platform_settings." + column`. Ticket 4 adds a second scope with its own
    constructor (`NewGitKeyring(master)`) and its own info strings (`launchpad
    git_integrations sealing v1`; no key-check string: T9's `key_check` row is moved to the current key by a mail settings PUT, so it is not a git signal; git detects a wrong key when a blob fails to open (`secret_unreadable`),
    and a second would have no column), and a `SealFor` / `OpenFor`
    that takes the AAD `"git_integrations." + column + "." + rowID`, so a blob cannot
    be moved to another row or column. The existing info strings never change (they
    pin T9). There is no "column grant" (that belongs to T16's backup tables); the
    protection here is the AAD binding plus the write-only API. `platform.Secret` is `type Secret string`; it is used for the API and for logging only (it prints
    `[redacted]`), and is not the in-memory form of the verify path (that is `[]byte`, below). API: `tokenSet` and `webhookSecretSet` booleans,
    never the value. In a PATCH, absent or `null` keeps a secret, `""` clears it.
    - **Why sealed, not only hashed:** HMAC verification (GitHub, Bitbucket) needs the
      plaintext secret. GitLab compares a token, so the hash is enough; new GitLab
      integrations seal too, so Launchpad can reuse the secret when it makes or
      rotates a hook (token mode).
    - **One master key on purpose.** `SETTINGS_ENCRYPTION_KEY` now covers mail
      secrets AND git tokens and secrets. There is no key rotation story in this
      design (T9 has none either): a changed key makes every sealed value
      unreadable. State this in the runbook (ticket 13). A re-key tool is a separate
      ticket if wanted.
    - **No `SETTINGS_ENCRYPTION_KEY`: 409** `encryption_unavailable` for any create
      or rotate that must store a token or an HMAC secret. A GitLab `webhook_only`
      integration with a pasted secret still works without a key (hash only), as
      today. A lost or changed key never stops the boot: sealed values read as
      unavailable, the integration shows `needs_attention` (`secret_unreadable`) and
      its webhook answers 401 until the admin rotates. This applies to a row whose verification NEEDS a sealed value (every GitHub and Bitbucket row); a GitLab row that has its hash always verifies by hash (the GitLab path hashes the token and never opens the blob). **One unreadable integration never stops the others of the provider:** the verify loop skips an unreadable one and continues. **Exception:** a GitLab integration
      with no sealed copy (every integration from before migration A) authenticates by hash, so a
      lost key does not affect it.
    - **Plaintext in memory.** Verifying an HMAC needs the secret decrypted. The
      verify path keeps decrypted secrets as `[]byte` in a small in-process cache keyed
      by integration id and `updated_at`; an entry is overwritten when it is evicted,
      and the cache drops on a PATCH, rotate or disable. This is best effort: Go can
      move memory, and a string cannot be wiped, so no secret is ever held as a
      `string` on this path. The cache holds the secret only as long as the process
      lives; a core dump of the process holds it.
    - **Rotation is hooks first, then the swap, and the swap re-seals.** `rotate-secret`
      makes a new secret, seals it into `webhook_secret_next_enc` (AAD column
      `webhook_secret_next_enc`) and stores its hash in `webhook_secret_next_hash`. Token mode updates
      every active hook with it through the API. Verification accepts the current AND the pending
      secret while one is pending (at most two per integration; the T5 #5b duplicate check and the
      GitLab hash compare run over both hashes). **The commit step is app code, not one SQL statement:**
      a blob bound to `webhook_secret_next_enc` does not open under `webhook_secret_enc`, so the commit
      decrypts the pending blob, seals it under the current column's AAD, and in ONE transaction sets
      `webhook_secret_enc`, `webhook_secret_hash`, clears both `next` columns and clears
      `attention_code`. **A hash-only GitLab row (from before migration A, or a pasted secret with no key) has no plaintext to put in a hook**, so switching it to token mode, or mapping a repo in token mode, answers `409 conflict` until a rotate has stored a sealed secret. If a hook update fails, the integration keeps both secrets, the answer lists
      the failed repos (`hookState=failed`) and the admin retries (a token-mode rotate over many repos may also hit the 30 s route cap and leave some hooks `failed`; the retry covers the rest, since it reuses the pending secret); the old secret keeps working until
      the commit, so no delivery is lost. **A repo mapped while a rotation is pending gets the PENDING secret in its new hook. A retry of `rotate-secret` in token mode while a secret is pending reuses the PENDING secret (server-side only: **token mode never returns the secret in any response**, and a test checks every route's body)** (it does not make another), so a hook already moved to it never holds a secret the server forgot; `DELETE .../rotate-secret` is the explicit way to drop it, and after that the admin must re-rotate (the compound edge: a hook on a discarded secret answers 401 until refreshed, and the UI says so). Webhook-only: the admin updates the host, then confirms with
      `POST .../rotate-secret/commit`. **Without `SETTINGS_ENCRYPTION_KEY`** a hash-only GitLab integration can still rotate and commit: the commit swaps the hashes, sets `webhook_secret_enc` to NULL (the old sealed copy, if any, no longer matches the new hash) and skips the seal; a server-generated GitLab secret with no key is allowed and stored as a hash only (the secret is shown once), because the GitLab path needs nothing else. GitHub and Bitbucket need the key (`409 encryption_unavailable`). (This replaces T5 #6's "no overlap" for new integrations; an
      old GitLab integration has no sealed copy, so its first rotate stores one.)
    - **11a. The status (`attention_code`).** Set by: a sealed value that does not open
      (`secret_unreadable`, found when the verify path or a rotation tries it), a `check` or a hook
      call that answers 401 or 403 (`token_rejected`), a hook call that cannot connect
      (`attention_unreachable`; the HTTP code `provider_unreachable` of #12 is a different thing), a hook create or update that failed (`hook_failed`). Cleared by: a
      successful `check`, a token replace followed by a successful check, a committed rotation. The list
      route's `status` is `disabled` when `enabled` is false, else `needs_attention` when
      `attention_code` is set, else `ok`. Ticket 4 only answers 401 and logs; the status logic comes in ticket 5 (below).
    - **A change of `baseUrl` or `api_base_url` clears the stored token** (and sets `mode` to
      `webhook_only` unless the same PATCH carries a new token). Otherwise a stolen admin session
      could aim an integration at an attacker's host and then call `check` or `available-repos` to send
      the token there, which would break "write-only". A PATCH that changes the URL and sends a
      fresh token is allowed. Ticket 5's AC has a test: change the URL, then `check` sends nothing to
      the new host with the old token. The base URL also cannot change while a repo is mapped (the
      PATCH row says so).
    - **`updated_at` is the cache key.** Rotate, commit, discard and every PATCH set `updated_at = now()` in their statement; the verify cache is keyed by `(integration id, updated_at)`, which is how a second instance learns of a change (it re-reads the row each request, two indexed reads, and refreshes its cache on a new `updated_at`).
    - Never log, emit or idempotency-store a token or secret. The create and
      rotate routes use `idempotency.Reject`. A test checks log, events and idempotency rows
      (like `TestSignups_NoSecretsInLogsEventsOrIdempotency`).

## API routes

Under `/orgs/{org}`. All config routes use the `admin` scope AND the org-admin role, GETs included (as T5's table and `router.go`: `r.With(admin)` on the GETs). `RequireScope(read)` does not check the org role, so a `read` token of any member or agent would otherwise list repo paths, hook state, `attention_code` and deliveries. All in `router.go` AND
`api/openapi.yaml`.

| Route | Ticket | Notes |
|---|---|---|
| `GET /integrations` | 5 | All providers. Name, provider, base URL, mode, enabled, `status` (`ok`, `needs_attention`, `disabled`; #11a) and `attentionCode` (the reason, shown by #47), last delivery summary, repo count. No secret. |
| `POST /integrations` | 5 | `{provider, baseUrl, name?, mode, token?, secret?}`. `idempotency.Reject`. **Only registered providers are accepted** (ticket 5 registers gitlab and github; any other value is `400 validation_failed`; ticket 10 registers bitbucket). Token mode: checks the token (#28), seals it, makes the secret (server-generated, 32 random bytes, base64url) and seals it. Webhook-only: the server generates the secret (or accepts a pasted one for GitLab compatibility, which keeps T5 #3: 32-256 printable ASCII characters, no space, else `400 validation_failed`) and returns it ONCE in the 201 as `webhookSecret`, with `webhookUrl`. |
| `GET /integrations/{id}` | 5 | One integration; no secret. |
| `POST /integrations/{id}/check` | 5 | Token mode. Re-checks the token through the guarded client; sets or clears `attention_code`. Rate limited (#28). |
| `PATCH /integrations/{id}` | 5 | `name`, `baseUrl` and `apiBaseUrl` (the admin override of #29; both only while no repo is mapped, else `409 conflict`; either clears the token, #11a; `POST` also takes an optional `apiBaseUrl`), `enabled`, `token` (replace or clear; **clearing the token alone forces `mode` to `webhook_only` in the same statement**, so the CHECK `mode <> 'token' OR token_enc IS NOT NULL` holds), `mode` (`token` with no stored token is `400 validation_failed`; a change from `token` to `webhook_only` clears `token_enc` and leaves the hooks and each `hook_state` as they are). |
| `POST /integrations/{id}/rotate-secret` | 5 | `idempotency.Reject`. New secret, shown once; rules in #11. |
| `DELETE /integrations/{id}/rotate-secret` | 5 | Discards a pending secret (both `next` columns cleared; hooks already on the new secret must then be refreshed or rotated again). A second `rotate-secret` while one is pending: **token mode reuses the pending secret; webhook-only replaces it** (a webhook-only admin has not moved a hook yet that the server could forget; in token mode hooks may already hold it). |
| `POST /integrations/{id}/rotate-secret/commit` | 5 | Re-seals and swaps (#11). Webhook-only calls it when the host is updated; token mode calls it itself after the hooks. |
| `GET /integrations/{id}/available-repos?q=&cursor=` | 5 | Token mode only. Provider repo search through the guarded client; one page, at most 50 rows. The `cursor` is opaque (#32): never a URL. |
| `GET /integrations/{id}/repos` | 5 | The mapped repos with `hookState`. |
| `POST /integrations/{id}/repos` | 5 | Token mode takes `{repoId, teamId?}` (the id from `available-repos`; a repo that is already mapped is `409 conflict`); the server calls `GetRepo` and derives `repoKey` and `repoPath` from the host, never from the body. Webhook-only takes `{repoKey, repoPath, teamId?}`. Token mode makes the hook first, then the row; a hook that fails leaves no row and answers `502 provider_error`. Webhook-only: the row only; `repoKey` is what the provider's webhook sends. `Idempotency-Key` works. |
| `PATCH /integrations/{id}/repos/{repo}` | 5 | `teamId` only (T5 #23). |
| `DELETE /integrations/{id}/repos/{repo}` | 5 | `{repo}` is the map row id (not `repoKey`). Token mode: deletes the hook (best effort; a 404 from the host counts as done); then the row. Links stay (T5). `200` with `hookRemoved: true|false`: a host 5xx, timeout or 403 gives `hookRemoved: false` (the row is still deleted and the admin is told to remove the hook by hand); a host 404 gives `true`. |
| `POST /integrations/{id}/repos/{repo}/backfill` | **11** | Token mode. Synchronous, capped (#21). The web button for it is also ticket 11. |
| `POST /integrations/{id}/repos/{repo}/hook/refresh` | **11** | Token mode, org admin. Re-ensures the repo's hook with the current `HookSpec` (list-and-match, #24); answers `{hookState}`; `provider_*` errors as #12; `409 conflict` when the integration has no token; in OpenAPI. Ticket 12 reuses it. |
| `GET /integrations/{id}/deliveries` | 5 | The ring, newest first. |
| `POST /orgs/{org}/integrations/gitlab/webhook` | 3 | **Unchanged.** |
| `POST /orgs/{org}/integrations/github/webhook` | 4 | New. Public (no auth in OpenAPI `security`), like the GitLab one. |
| `POST /orgs/{org}/integrations/bitbucket/webhook` | 10 | New. Public. Every route in this table is in `api/openapi.yaml` in its own ticket (`TestOpenAPIMatchesRoutes`). |
| `GET /orgs/{org}/issues/{issue}` | 11 | The issue gains `changeRequests` (#18; read from `mr_links`; T5 "Not done"). |


(The Ticket column names the building ticket; rows are in route order, not build order.)

12. **The old routes go.** `GET/POST/PATCH /integrations/gitlab` and
    `.../projects` are removed in the same tranche (ticket 5). The response of
    the old `POST /integrations/gitlab` named `webhookUrl`; the new one does
    too, and the URL value for GitLab is the same string, so a hook already on
    GitLab keeps working. (Ticket 5; ticket 3 ports, and keeps, the old routes.) The OpenAPI file and the web client are regenerated.
    A test keeps the live GitLab webhook answer byte-identical before and
    after.
    **Error codes: one table.** Codes in `internal/httpx` `Codes` AND the `ErrorCode` enum in
    `api/openapi.yaml` (a test keeps them equal). Existing codes are reused where they fit. The enum holds HTTP codes only; the `attention_code` values of #11a are separate. Job and status values that are NOT HTTP codes are listed after the table. **Each code is added to `httpx.Codes` and the OpenAPI enum by the FIRST ticket that returns it** (the equality test fails otherwise): ticket 4 adds `length_required` and `overloaded` and reuses `payload_too_large` (the receive path never returns `encryption_unavailable`); ticket 5 adds `integration_limit`, `encryption_unavailable`, the `provider_*` codes and `public_url_required`; ticket 7 adds `job_active`, `import_in_progress`, `public_url_required` (if ticket 5 has not merged first; whichever of 5 and 7 merges first adds it to `httpx.Codes` and the OpenAPI enum, and the other reuses it), `dry_run_required`, `linear_unauthorized`, `linear_unreachable` and `no_space`.

    | Code | HTTP | Route |
    |---|---|---|
    | `integration_limit` (new) | 409 | `POST /integrations`, enable (more than 8 enabled per provider) |
    | `encryption_unavailable` (new) | 409 | create, PATCH token, rotate with no `SETTINGS_ENCRYPTION_KEY` |
    | `provider_unreachable` (new) | 502 | `check`, `available-repos`, repo map (create), backfill. (Rotate in token mode answers `200` with `committed` and `failedRepos`, see the LAUN-86 section; it returns `provider_*` only when the pending secret cannot be stored. Unmap never returns it: a host failure on the hook delete is `200` with `hookRemoved: false`.) |
    | `provider_unauthorized` (new) | 502 | same (the host refused our token with 401; not our 401) |
    | `provider_forbidden` (new) | 502 | same (403: token lacks a scope) |
    | `provider_rate_limited` (new) | 503 with `Retry-After` | same (host 429) |
    | `provider_error` (new) | 502 | same (any other host failure, a 3xx, a bad body) |
    | `payload_too_large` (existing) | 413 | the git webhooks above the cap (#14), and import bodies |
    | `length_required` (new) | 411 | the GitHub and Bitbucket webhooks without `Content-Length` |
    | `overloaded` (new) | 503 with `Retry-After` | the GitHub and Bitbucket webhooks over the byte budget |
    | `rate_limited` (existing) | 429 | `check`, `available-repos`, repo map, unmap (before its host call), rotate, backfill (#28) |
    | `unauthenticated` (existing) | 401 | the webhooks (bad or no signature, unknown org, none enabled) |
    | `conflict` (existing) | 409 | `dry-run` with `failed_files > 0` and no `acceptFailedFiles`; a route outside the session transition table (#43); a second map of a mapped repo; a URL change with a repo mapped; token mode on a hash-only GitLab row; `hook/refresh` on an integration with no token; `check` or `available-repos` on a webhook-only integration; `rotate-secret/commit` with no pending secret |
    | `validation_failed` (existing) | 400 | unknown provider, bad URL, `confirmOrg` that is not the org slug |
    | `public_url_required` (new; added by whichever of tickets 5 and 7 merges first) | 409 | token-mode `POST /integrations` and repo map with no https `PUBLIC_BASE_URL` (#25; ticket 5); import session start with an empty `PUBLIC_BASE_URL` (#42g; ticket 7) |
    | `job_active` (new) | 409 | any import start while a job is active, `PUT .../usermap` while a job of the session is active, and `DELETE /import/sessions/{id}` while one of its jobs is active |
    | `import_in_progress` (new) | 409 | `PATCH .../teams/{team}` with `cyclesEnabled: true` (#43) |
    | `dry_run_required` (new) | 409 | `POST /import/sessions/{id}/run` with no succeeded dry run on the current user map and export |
    | `attachments_unavailable` (existing) | 503 | session start with no `S3_*` |
    | `linear_unauthorized` (new) | 502 | export start with a key Linear refuses |
    | `linear_unreachable` (new) | 502 | export start when Linear cannot be reached |
    | `no_space` (new) | 507 | export start below the free-space floor |

    Not HTTP codes, but job `error_code` or status values shown by the web: `secret_unreadable`,
    `token_rejected`, `attention_unreachable`, `hook_failed` (an integration's `attention_code`, #11a), `no_space` (a job `error_code`; it is also the HTTP `507` of the table), `verify_mismatch`,
    `interrupted`, `canceled` (job states and codes). A webhook answer of `200 ignored` or `202 ignored`
    keeps T5 #11 as it is for GitLab (`202 ignored` for an event that is not the right kind); GitHub and
    Bitbucket use `200 ignored` for a delivery that asks for nothing (a `ping`) and `202 ignored` for
    an event type we do not handle, the same split as T5.
13. **Disable stops deliveries only** (the project owner's decision C5). `enabled=false`
    makes the webhook answer `401` like a bad secret (the integration drops out
    of the lookup, as in T5 #4). Hooks stay on the host until the repo is
    unmapped. The UI says so. (A hook that keeps getting 401 may be turned off
    by the host: the UI says that too.)

## The webhook request

14. **Order of checks, per provider.**
    - **GitLab (unchanged):** token first, before the body is read; the event
      header; the delivery headers; the body (8 MiB; **behind the shipped nginx this cap is dead**, because its API location caps bodies at 2m, so it matters only for a direct connection); the payload. No limiter
      (T5 #9 stands, and it is consistent with the rest: the token check needs no
      body, so nothing has to be buffered before it).
        - **Production nginx already limits this** (`deploy/nginx/default.conf.template`: the API
      location has `client_max_body_size 2m`, with request buffering on, so nginx holds the whole body
      and enforces its own timeouts before the app sees a byte). So behind the shipped stack every provider's webhook, GitLab included, is capped at 2 MiB by nginx (a larger GitLab event gets nginx's own `413`), and the app
      sees a complete, already-bounded body, a `Content-Length` is always present, and the deadline
      below and the `411` mostly protect a direct connection (a dev stack, or an operator proxy with
      buffering off). The outer TLS proxy on another machine is the operator's; its limits are not
      measured here. The runbook (ticket 13) must say: set `TRUSTED_PROXY_CIDRS`, or every request
      from a proxy shares the empty-key bucket.
    - **GitHub and Bitbucket sign the body, so the body is read before the
      check.** That is a pre-auth buffer, and a limiter on it can be turned into a
      lever (hold the slots, starve real deliveries). So the order is built to keep
      the unauthenticated cost small and bounded, and the design says plainly what
      stays open:
      1. Org lookup and the list of enabled integrations of that provider (two
         indexed reads). None: `401`, before anything is read. So `413` and `503`
         below are seen only for an org that has an enabled integration of that
         provider (a small oracle on org and integration existence; T5 #7 already
         accepts a timing one for org slugs, which are not secrets).
      2. The signature header must be present and well formed (`sha256=` and 64
         hex), else `401`.
      3. `Content-Length` must be present (no chunked bodies: `411`) and at most
         **2 MiB**, else `413`. Nothing has been read yet.
      4. Only now a read slot is taken: a **weighted global budget** of in-flight
         unsigned body bytes (proposed 32 MiB for the process) charged by
         `Content-Length`, (and no per-client cap: GitHub delivers from a few shared addresses and does not retry on
         its own, so a per-client cap would 503 real bursts; the byte budget and the deadline are the
         limits). Over budget: `503` with `Retry-After: 5`.
      5. The read has a **hard deadline** of 5 s in total
         (`http.ResponseController.SetReadDeadline`) ; a slow reader is closed and frees its bytes at once. Under 5 s the
         longest hold of any slot is 5 s. The deadline is an absolute 5 s, so no separate minimum rate is needed. The connection's previous read deadline is saved before the body read and restored after it (keep-alive connections).
      6. HMAC-SHA256 over the body with EACH enabled integration's secret of that
         provider (current and pending, #11; from the in-memory cache), `hmac.Equal`,
         no early exit; the match picks the integration. A miss is `401`. Only then
         the event header and the payload.
      - **What is not solved, said plainly.** A set of sources with well-formed
        headers can still fill the 32 MiB budget for 5 s at a time, and real
        deliveries during that window get `503`. The limit is on bytes in flight, not
        on signed traffic, because there is no way to tell signed from unsigned before
        the read. This is a bounded denial of service on webhook delivery only, not on
        the API, and the host's redelivery (GitHub's "Redeliver", Bitbucket's retry)
        recovers. A host that treats `503` as a failure may mark the hook as failing.
        The HMAC cost is also unmeasured: a 2 MiB unauthenticated body costs up to 16 SHA-256 passes (8 integrations, each with a current and a pending secret); measure it with the budget, and cap the pending checks if it matters. The numbers (32 MiB, 5 s) are guesses and not
        measured. Open point 8.
      - **The 2 MiB cap is ours, not a vendor guarantee.** GitHub allows payloads up to
        25 MiB; a PR event is far smaller (a body is at most 65,536 characters and an
        update carries two copies) but not provably under 2 MiB. An event above the
        cap is `413`, shows in GitHub's Recent Deliveries as failed, and is ring
        `error: payload_too_large`.
      - Compare cost: at most 8 enabled integrations per provider per org (a create
        beyond it is `409 integration_limit`), so the HMAC check is bounded.
    - Because the signature selects the integration, **no two enabled integrations
      of one provider in one org may share a secret** (T5 #5b: ticket 3 ports the GitLab check to the renamed tables, ticket 5 extends it to `(org, provider)` and the `next` hash; same advisory-lock
      rule, now by `(org_id, provider)`, over current and pending secrets).
15. **Headers per provider.** Verified against the vendors' documentation
    only; no live delivery was seen. The fixture rule is in "Tests every ticket keeps".
    - GitLab: `X-Gitlab-Token`, `X-Gitlab-Event: Merge Request Hook` (add
      `Note Hook` and `Pipeline Hook` only for the optional checks and review
      phases), `X-Gitlab-Event-UUID`, optional `Idempotency-Key`.
    - GitHub: `X-Hub-Signature-256: sha256=<hex>`, `X-GitHub-Event`
      (`pull_request`, later `pull_request_review`, `check_suite`, `status`;
      `ping` answers `200 ignored`), `X-GitHub-Delivery` (the delivery GUID; a
      redelivery from GitHub's UI keeps the same GUID).
    - Bitbucket Cloud: `X-Hub-Signature: sha256=<hex>` when the webhook has a
      secret, `X-Event-Key` (`pullrequest:created`, `:updated`, `:fulfilled`,
      `:rejected`, `:approved`, `:unapproved`, and `repo:commit_status_*` for
      checks), `X-Request-UUID` (per delivery) and `X-Hook-UUID` (per hook).
    - Bitbucket Data Center: `X-Hub-Signature: sha256=<hex>`, `X-Event-Key`
      (`pr:opened`, `pr:from_ref_updated`, `pr:merged`, `pr:declined`,
      `pr:reviewer:approved`, ...), `X-Request-Id`.
    - **Verify at build, from the vendors' documentation, before the fixtures are
      written (none was seen live):** whether Bitbucket Cloud reuses
      `X-Request-UUID` on a retry (the dedupe key assumes it does; if not, a retry
      is processed again, which is safe by forward-only), whether GitHub's
      `X-GitHub-Delivery` is stable on redelivery, Cloud's retry header
      (`X-Attempt-Number`), Bitbucket's percent-encoding of the braces in `{uuid}` path parts, whether GitHub's `GET /repositories/{id}/hooks` exists for the hook calls (else the owner/name path is rebuilt from the id), the scope a GitLab project access token needs for `ListRepos`, and the Data Center version floor that signs webhooks
      with `X-Hub-Signature` (older versions send no secret: such a Data Center
      host cannot be used, and the connect dialog says so).
        - **Delivery header values are validated like T5 #12b:** `X-GitHub-Delivery`, `X-Request-UUID` and `X-Request-Id` must be 1-100 printable ASCII characters, else `400`; the repo-key part of a delivery key is at most 200 characters (`400` beyond), so a key stays under about 400 bytes. **Bitbucket repo keys.** Cloud's API paths are `/2.0/repositories/{workspace}/{repo}/...` and each part may be a `{uuid}`, so the Cloud `repo_key` is `{workspace-uuid}/{repo-uuid}` (braces kept, lower-case, `/` kept); the workspace is therefore stored in the key, not only in `repo_path`. Data Center's paths are `/rest/api/1.0/projects/{projectKey}/repos/{slug}`; its `repo_key` is the numeric repository id (the webhook payload carries it), and `repo_path` is `KEY/slug` (a label). When a slug is renamed the path call returns 404: the client then lists the project's repos, matches the id, and updates `repo_path` (and says `hook_state` unchanged). The `repoId` that `available-repos` returns is an OPAQUE string the client made (Data Center has no lookup by numeric id alone, so it is `KEY/slug#id`; GitHub and GitLab use the numeric id; Cloud the workspace and repo UUIDs); `GetRepo` parses it. Both forms and the payload fields (`repository.id`, `repository.uuid`, `workspace.uuid`, `project.key`) are **verify at build**. The delivery key uses `r` plus the repo key with `:` and `/` replaced by `_` so the `:` separators stay unambiguous.
    - **Bitbucket drafts:** Cloud has a `draft` field only on newer payloads;
      Data Center has none in the versions known. Treat a missing field as not
      a draft (so a Bitbucket PR moves to In Review when opened). Open point 3.
16. **Which events do what** (same moves as T5 #16, in neutral terms). Since
    LAUN-244 these are the built-in moves; a team's git workflow rule replaces
    the move of its event (`decisions-T44-git-workflow.md`).
    - `opened` / `reopened`, not draft: link, move to the team's `review_state_id`.
    - `opened` / `reopened`, draft: link, no move.
    - `ready` (GitHub `ready_for_review`; GitLab `changes.draft` true to false):
      link, move to review.
    - `drafted` (GitHub `converted_to_draft`): **accepted and answered `200 ignored` with nothing written until ticket 11** (the same holds for a plain `updated`, until ticket 11 adds the refresh). The `draft` and `head_sha` columns do not exist before migration D (ticket 11; migration A has no MR-state columns), so ticket 4 cannot store them. From ticket 11 the event updates the stored `draft` only. (Chosen over moving the columns into migration A, so migration A stays a pure rename and port.)
    - `updated` (title, branch, push): refresh the link row's title, URL,
      branch, `head_sha` and `draft` unless stale (rule 5 above, T5 #25). It does not move.
      **A change from T5**, which refreshed nothing on a plain update (T5 #16);
      the new MR state column needs fresh data. Proposed; open point 4.
    - `merged` (GitHub `closed` with `merged: true`; Bitbucket `fulfilled` or
      `pr:merged`): link, move to `testing_state_id`, even from draft.
    - `closed` unmerged: the link becomes `closed`; no move.
    - `approved` / `unapproved` / review events: set `review` on the link. No
      move (T5 #16: approvals asked for nothing, they still ask for no move).
    - Check events (#19): set `checks` on the links whose `head_sha` matches.
17. **Delivery keys per provider** (replace `gitlab.DeliveryKeys`; stored in
    `event_uuid`, each with the MR suffix, T5 #12b; unique per
    `(org_id, integration_id, event_uuid)`, #8):
    - `gl:` + the `ik:` / `ev:` forms as today. **Migration A rewrites the
      existing rows once** (`event_uuid` gets the `gl:` prefix; the down strips it),
      so there is one form and no dual check.
    - `gh:<X-GitHub-Delivery>:r<repo id>:pr<n>:<action>`.
    - `bb:<X-Request-UUID or X-Request-Id>:r<repo>:pr<n>:<action>`. The hook UUID is
      NOT part of the key: it names the hook, not the delivery.
    A host's own redelivery keeps its GUID and is `duplicate`. A delivery the host
    sends again after we answered 5xx is processed (a 5xx keeps nothing).

## MR state on the ticket (#18)

18. **Show it.** `mr_links` already makes a `link` attachment on the issue (T5
    #25). The attachment stays the cheap pointer. The web issue page gets a
    small "Changes" block fed by `changeRequests` in the issue response: link,
    title, `repoPath` and number, author, the state `draft | open | approved |
    merged | closed` and the checks badge. The state is computed, not stored:
    `merged` and `closed` from the link state; `draft` from `draft`; `approved`
    when `review = 'approved'` and open; else `open`. `changes_requested` shows
    as `open` with a flag (the five states are the project owner's list). The MCP
    `get_issue` shows the same list. **Migration D** (ticket 11) adds the
    `mr_links` columns and the `issue_activity` kinds (#20). **The DDL, pinned:** `ALTER TABLE mr_links ADD COLUMN draft boolean NOT NULL DEFAULT false, ADD COLUMN head_sha text NOT NULL DEFAULT '' CHECK (length(head_sha) <= 64), ADD COLUMN review text NOT NULL DEFAULT 'none' CHECK (review IN ('none','approved','changes_requested')), ADD COLUMN checks text NOT NULL DEFAULT 'none' CHECK (checks IN ('none','pending','success','failure')), ADD COLUMN author text NOT NULL DEFAULT '' CHECK (length(author) <= 100), ADD COLUMN provider_updated_at timestamptz, ADD COLUMN repo_path text NOT NULL DEFAULT '' CHECK (length(repo_path) <= 300), ADD COLUMN checks_updated_at timestamptz, ADD COLUMN checks_detail jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(checks_detail) = 'object');` **Meaning of the new columns:** `provider_updated_at` is the host's own "MR last updated" time carried in a change or review event (GitHub `pull_request.updated_at`, GitLab `object_attributes.updated_at`, Bitbucket Cloud `updated_on`, Data Center `updatedDate`), set by every change and review event; **an event whose time is OLDER than the stored one is stale: it writes nothing and moves nothing** (an equal time is accepted, since hosts give second resolution). This adds time ordering to T5 #25's state rules, and closes T5's "not ordered" gap. `repo_path` is the repo label at the time of the event (the Changes block shows it, #18, and a link outlives an unmap, so the path cannot come from the map). `checks_updated_at` and `checks_detail` belong to check events only (#19), so a newer MR update never hides an older check event, and the reverse. A change of `head_sha` (a push) resets `checks` to `none` and clears `checks_detail`. `mr_links_state_check` has no `gitlab_` name and is not renamed. and `CREATE INDEX mr_links_checks_idx ON mr_links (org_id, integration_id, repo_key, head_sha) WHERE head_sha <> '';` (the checks lookup of #19). `Event.Author` is stored as the host LOGIN only (no display name, no email, no avatar; control characters become spaces, as T5 does for text). Text values are truncated to their CHECK length (`author` 100, `repo_path` 300, `head_sha` 64) BEFORE the insert or update, so a CHECK never fails an event. The down drops the index and the columns. The existing rows get the defaults; the next event of each MR fills them.
19. **Checks come from separate events and are a later phase.** The MR
    payload does not carry the pipeline result. GitHub sends `check_suite` and
    `status`; GitLab `Pipeline Hook`; Bitbucket `repo:commit_status_*`. They
    update links by `(integration, repo_key, head_sha)` (`mr_links_checks_idx`). Each admin must
    tick the event in the hook; token mode makes hooks with the right set
    (#24). A checks event that matches no link is `200 ignored` and keeps
    nothing. **Aggregation:** `checks_detail` is a map from a check name (GitHub app slug or check name, GitLab the pipeline `name` or `"pipeline"`, Bitbucket the build key) to `{state, at}`, at most 50 names; an event for a name is ignored if its `at` is older than the stored one for THAT name, else it replaces it. The link's `checks` is the worst of the entries (`failure` over `pending` over `success`), so one app's later success never hides another app's failure. A check event also needs its own delivery key (no PR number): `<prefix>:<delivery>:r<repo>:sha<first 12 of head sha>:checks` (ticket 12). This is ticket 12, after the base works.
20. **Timeline entry: yes, three kinds; comment: no.** The ticket's Activity gets
    rows (`issue_activity`, T15; new kinds `mrLinked`, `mrMerged`, `mrClosed`, source
    = the provider) written in the same transaction, one per link event that changes
    state, not one per update. **`issue_activity.kind` has a closed CHECK list**
    (`db/schema.sql`, T15 #3), so migration D rewrites `issue_activity_kind_check`
    with the three new kinds (the down deletes rows of those kinds, #8). Ticket 11
    also: adds the three kinds to the OpenAPI activity `kind` enum
    (`TestOpenAPISchemasMatchModels`), regenerates `web/src/api/schema.d.ts`, and
    renders them in `web/src/features/issues/activityModel.ts` and `Activity.tsx`
    (text, icon, link to the MR). Reason for the rows: the Activity timeline already
    answers "what happened to this ticket". Not a comment: a comment is a person's
    or agent's words and it notifies (T13), so a bot comment per MR event is noise.
    Proposed; open point 5. The existing `issue_state_changes` row (source `gitlab`,
    now also `github`, `bitbucket`) stays for moves.
21. **Backfill is a synchronous, capped request, not a job.** An integration made
    after MRs exist has no links for them. `POST /integrations/{id}/repos/{repo}/backfill`
    (token mode, org admin, `Idempotency-Key` works, rate limited as #28) lists the
    repo's OPEN changes through the guarded client (at most 200, newest first, at
    most 5 provider pages, 30 s total), parses each into a neutral event of action
    `opened` or `updated` and runs it through `HandleSCMEvent` with the same rules (**backfill has no delivery row and no delivery keys**: it skips the first-write step of T5 #12, so a repeat is safe only because link writes and moves are idempotent and forward-only)
    (forward only; a draft is linked, not moved). The answer is `{seen, linked,
    moved, truncated}`. No runner, no table, so it is independent of the import job
    and does not collide with its one-active-job index. A repo with more than 200
    open changes is `truncated: true`; the admin can run it again after the oldest
    close. Ticket 11 builds it, with an OpenAPI entry, and needs the client of
    ticket 2 and the routes of ticket 5. Webhook-only has no backfill.
22. **Post back to the host: not in v1.** The question (does Launchpad post a
    comment or a commit status on the MR?) has a default answer here: no.
    Reasons: (a) it needs a write scope on the token (GitHub `repo:status` or
    a fine-grained `Commit statuses: write` and `Pull requests: write`; GitLab
    `api`; Bitbucket `repository:write`), and a read-and-hook token is a much
    smaller target; (b) a second writer on the MR can loop with the host's own
    CI or with a bot rule; (c) nobody asked for it. The provider interface
    keeps `PostStatus` and `PostComment` out of v1 but reserves the names. A
    later ticket can add "Launchpad status" (a commit status with the ticket
    state, never a comment) behind an integration switch and a stronger token
    prompt. Open point 6.
23. **Minimal token scopes** (shown in the connect dialog, verified at build
    from the vendors' docs and checked by `CheckToken`, which refuses a token
    that can do nothing):
    - GitHub: a fine-grained token with `Metadata: read` and `Webhooks:
      read and write` on the chosen repos (a classic token needs `repo` plus
      `admin:repo_hook`; the dialog says a fine-grained one is better).
      Backfill needs `Pull requests: read`.
    - GitLab: `api` (the narrowest scope that can add project hooks; `read_api`
      cannot). A project access token with role Maintainer. The dialog says the
      scope is wider than needed and why.
    - Bitbucket Cloud: a repository access token or app password with
      `Webhooks: read and write` and `Repositories: read` (`Pull requests: read`
      for backfill). Data Center: an HTTP access token with project admin on the
      chosen repos (hooks need it).
24. **Hooks made in token mode** subscribe only to what #16 needs: GitHub
    `pull_request` (later `pull_request_review`, `check_suite`, `status`);
    GitLab merge request events (later pipeline and note); Bitbucket the PR
    events (later commit status). SSL verification on, always. A hook update
    is idempotent: the client first lists hooks and matches by URL. **Who wires `Provider.Client`:** ticket 5 for gitlab and github (it is the first ticket that needs a client in a route), ticket 10 for bitbucket. **Extra events are owned by
    tickets 11 and 12**, not by ticket 5. The `HookSpec` carries an event set per provider; ticket 5
    subscribes to the change events only; ticket 11 adds the review events (GitHub
    `pull_request_review`, GitLab note and approval events, Bitbucket approval events) and ticket 12
    adds the check events (GitHub `check_suite` and `status`, GitLab `Pipeline Hook`, Bitbucket
    build statuses). A hook already made must learn the new set: each of those tickets uses an
    admin action `POST /integrations/{id}/repos/{repo}/hook/refresh` (token mode; it re-ensures the
    hook with the current spec, by list-and-match; ticket 11 adds the route) and a "Refresh hooks" button, and tells the
    admin in its release note to press it once. Webhook-only hooks are the admin's: the connect
    help lists the events to tick.
25. **The webhook URL** is `PUBLIC_BASE_URL` plus the route path, per
    provider. **Token mode needs an absolute `https` `PUBLIC_BASE_URL`** (`http` only on `LOG_ENV`
    local or test), because the host must call it and a path is useless to the host. With it empty or
    not https, `POST /integrations` and the repo map in token mode answer `409 public_url_required`.
    Webhook-only still works: the response and the UI show the path with a warning "set
    PUBLIC_BASE_URL so the full URL can be shown". Ticket 5's AC tests both. A hook made for a repo points at the provider's route. The
    secret is in the host's secret field, never in the URL. No query string
    (a URL is logged by proxies).

## Security

26. **Order of work and the cheap refusal path** is #14 and T5 rule 2 (T5 #9, #10). Every bad
    request ends in `401` with no body detail, with one bounded read in the
    HMAC case.
27. **Outbound requests are the SSRF risk.** The server dials a host an admin
    typed (and a base URL the admin owns). See #28 to #31.
28. **The token check at create.** `CheckToken` runs in the request, through
    the guarded client, with a 10 s timeout. Its errors map to our codes
    (`provider_unreachable`, `provider_unauthorized`, `provider_forbidden`,
    `provider_rate_limited`, `provider_error`); no host text is echoed (a
    self-hosted host's error page could carry anything). Rate-limit (in memory per process, like T9, so two instances double the effective rate) the create,
    check, available-repos and rotate routes per org and per user (a guess,
    copied from the mail test: 6 a minute, burst 3; not measured), because they
    let an admin session probe addresses.
29. **API base URL rules.** `base_url` is validated at save: https only outside
    local/test (an `http` host is refused unless `GIT_ALLOW_PRIVATE_PROVIDERS` is set or `GIT_ALLOW_PRIVATE_CIDRS` lists the host's address, AND the host's address is private; same rule as #31), no user info,
    no query, no fragment, lower-case host, default port removed (T5 #2). The
    API root is derived: GitHub `github.com` -> `https://api.github.com`,
    Enterprise `https://HOST/api/v3`; GitLab `https://HOST/api/v4`; Bitbucket
    `bitbucket.org` -> `https://api.bitbucket.org/2.0`, Data Center
    `https://HOST/rest/api/1.0` (base path kept if the host runs on a path). The
    admin can override `api_base_url` for an odd setup; it passes the same checks.
    **Self-hosted TLS:** system roots only. A self-hosted host with a private CA
    does not work in v1 (no "skip verify", as T16 #36); an operator can add the
    CA to the container's roots (`SSL_CERT_FILE`). Open point 9.
30. **The guard allows a base URL per integration, not one per provider.** A
    second self-hosted host is a second integration. The guard runs at dial
    time for every request, not only at save.
31. **ONE guard design with the backups.** T16 #34 and #35 define a dial-time
    guard (`net.Dialer.Control` on the resolved address, always-refused ranges,
    metadata addresses, IPv4-in-IPv6 unwrap, `Proxy: nil`, no redirects). LAUN-1
    uses the same code: **`internal/netguard`**, owned by ticket 2 of this note, built to T16's list; T16's ticket 3 (LAUN-70) imports it and keeps only the backup policy (merge-order rules are in "Ticket changes to file"). Only the PRIVATE-RANGE POLICY differs, and each caller passes
    its own policy:
    - Always refused, for both: T16's whole always-refused list and the
      metadata addresses. `GIT_ALLOW_PRIVATE_PROVIDERS` cannot open them.
    - Private ranges (`10/8`, `172.16/12`, `192.168/16`, `fc00::/7`, `100.64/10`):
      backups read `BACKUP_ALLOW_PRIVATE_CIDRS` (a list of CIDRs). Git
      connections read `GIT_ALLOW_PRIVATE_PROVIDERS`.
    - **Proposed alignment (open point 10):** the same CIDR-list shape for git,
      `GIT_ALLOW_PRIVATE_CIDRS`, validated by the same parser (no `/0`, nothing
      that overlaps an always-refused range, nothing shorter than `/8`). The project owner
      decided a boolean, `GIT_ALLOW_PRIVATE_PROVIDERS=1`. Keep the boolean as
      the documented switch and make it mean "allow the standard private ranges
      (`10/8`, `172.16/12`, `192.168/16`, `fc00::/7`, `100.64/10`), never the
      container gateway, never the metadata addresses". `GIT_ALLOW_PRIVATE_CIDRS`
      is the narrower, preferred form for an operator who wants one range, and
      the boolean is a shortcut for the list. The host's own published ports (the gateway) stay refused for git with no
      exception (rule below); backups alone keep T16 #34's exact `/32` option. A self-hosted GitLab on the
      LAN works with the boolean; it needs a shell on the host, and not a UI tick
      (the T16 #35 reason: a stolen admin session must not open the internal
      network).
    - **The app's own network is refused by the git policy, switch or no switch.**
      The `app` container sits on the `backend` and `edge` networks beside
      `postgres` and `silo` (`deploy/docker-compose.yml`), and both are private
      addresses. With `GIT_ALLOW_PRIVATE_PROVIDERS=1` or `GIT_ALLOW_PRIVATE_CIDRS` a
      range such as `172.16/12` opens, and a stolen admin session could aim an
      integration at `http://silo:9000` or `http://postgres:5432` (blind requests: a
      status or timing leak, no body back). So the git policy, in addition to the
      always-refused list, ALWAYS refuses: (1) every subnet attached to the app
      container (read from its interface addresses at boot and re-read each hour; an
      operator range cannot override it), (2) the container's default gateway, **which lies inside an attached subnet, so rule (1) already
      refuses it and there is no `/32` escape for git** (T16 #34's `/32` exception is for backups and
      applies only there; the git policy has none), and (3) the
      compose service and Docker DNS names (`postgres`, `app`, `silo`, `backup`, `web`,
      `host.docker.internal`, `gateway.docker.internal`) before resolution, as a
      second defence (the address check in (1) is the control; names are only a
      shortcut). An `app` container on host networking would see the host's own interfaces and the whole LAN as attached subnets, so the guard would refuse every private address: that deployment cannot use the private switches and must use webhook-only mode. A self-hosted GitLab on the LAN works with the switch as long as its
      subnet is not one the app container is on. **A Git host that listens on the Docker host's own
      published ports (the host's gateway address) cannot be used from `app`**; the operator gives
      it a LAN address, or uses webhook-only mode (no outbound call). The attached-subnet list is
      re-read each hour; a network attached after boot is refused only from the next read (an accepted
      race, small because the compose networks are fixed at `up`). What the switch still exposes: every
      other private host on the LAN can receive blind requests from an admin session.
      That is why it needs a shell on the host. The app's own silo and database are refused by rule (1), and that rule is tested, not assumed. Swarm or overlay deployments (CapRover) are covered the same way, because the guard reads the real interfaces of the container.
    - **The T16 #34 test table runs for the git client**, plus these rows: an address
      inside each of the app's own subnets (with the switch on and with a listed CIDR
      that contains it), the service names above, the gateway address (refused even if listed as a `/32` in `GIT_ALLOW_PRIVATE_CIDRS`), a resolver that answers public then private (rebinding),
      `http_proxy` set, and a redirect to a private address.
    - **Test seam.** `httptest` servers bind `127.0.0.1`, which the guard always
      refuses. As T16, the dialer takes a `Policy` and a `Resolver` as values; a test
      builds a policy that permits its own listener's address. Production code has no
      flag, variable or build tag that does this (a grep test checks).
    - **`GIT_ALLOW_PRIVATE_*` semantics, one rule** (ticket 2 AC): unset and unset
      means no private range. `GIT_ALLOW_PRIVATE_PROVIDERS=1` adds the five standard
      ranges (a built-in set: it is NOT run through the CIDR-list parser, whose T16 #35 rule rejects an IPv6 range shorter than `/32` and would refuse `fc00::/7`) (`10/8`, `172.16/12`, `192.168/16`, `fc00::/7`, `100.64/10`).
      `GIT_ALLOW_PRIVATE_CIDRS` adds the listed ranges (only this list is validated, as T16 #35). The
      result is the union. The always-refused sets above (the list, the app's subnets with the
      gateway) and the metadata addresses are removed from it last.
    - Deploy: `deploy/docker-compose.yml` passes the two variables to the `app`
      service; `compose-check` and `deploy-test` list them; `deploy.sh` prints the
      state in the summary (ticket 2).
32. **Response caps, time, and the token's origin.** Every provider response is
    capped (1 MiB for JSON; at most 5 pages per list), timeouts 10 s per request and
    30 s per route, at most 3 attempts only for `GET`, only for `5xx` and `429`, with
    `Retry-After` honoured up to 5 s. A write (`POST` or `DELETE` of a hook) is never
    retried blindly: a `POST` is followed by a list-and-match, so a lost answer does
    not make a second hook. A `3xx` is an error (`provider_error`, no follow). GitHub answers `301` for a renamed repo, so the client addresses repos BY ID (`/repositories/{id}` for GitHub, the numeric project id for GitLab, `{workspace-uuid}/{repo-uuid}` for Bitbucket Cloud, the numeric id for Data Center with the rename re-lookup of #15), never by `owner/name`; a stored `repo_path` is a label only and holds no information the key lacks.
    - **Fork PRs on a public repo.** A pull request from a stranger's fork of a mapped public repo can name `LAUN-n` in its branch or title and so link, and move, a ticket (T5 rules apply to any MR the host sends). The webhook is signed by the host, not by the author. Proposed default: the recommendation is open point 16 (same-repo PRs only); decide before ticket 4.
    - **The token goes only to the integration's own origin.** Bitbucket `next` links
      and GitHub or GitLab `Link` headers are absolute URLs from the host, and the
      `cursor` of `available-repos` comes from the browser. A hostile or hijacked
      host could name another host. Rules: (a) a cursor sent to the browser is opaque
      and holds only the provider's page parameter (a number or a short token matching
      `^[A-Za-z0-9_.~-]{1,200}$`), never a URL; (b) the client rebuilds every
      follow-up URL from `api_base_url` plus the path it already knows and that
      parameter; (c) if a response's next link is used at all, its scheme, host and
      port must equal `api_base_url`'s, else `provider_error`; (d) the `Authorization`
      header is set by one `RoundTripper` that compares the request's origin with the
      integration's and drops the header otherwise, so a code path that forgets (a) to
      (c) still cannot send the token away. Ticket 2 AC tests all four, with a fake
      host whose `next` points to a second origin.
    - The token goes in the `Authorization` header only, never in a URL, and the
      client strips it from the request on any error path before it is wrapped in an
      error.
    - **Bitbucket auth in v1: Bearer tokens only.** Bitbucket Cloud repository access
      tokens and Data Center HTTP access tokens are Bearer. App passwords need a
      username (Basic auth) and are not supported in v1 (no username field); the
      dialog says so.
33. **Never log.** No token, secret, signature, header value of a delivery
    (other than the event name), body or provider error text. Log lines carry the
    org, the integration id, the provider, the repo key and our own error code
    (T5 #26: unmapped logs only the repo id).
34. **Tenancy.** Every table has `org_id`; every query filters by it; the
    composite `(org_id, x_id)` FKs apply. A cross-org id is `404 not_found`; a
    body reference to another org's row (`teamId`) is `400 validation_failed`.
    Every new resource gets a cross-org test.
35. **Store no caller IP and no User-Agent** (AGENTS.md rule), also in the
    deliveries ring.

## Linear import in the web app

36. **A background job inside the API process, one at a time per org.** A single
    job runner (`internal/importjob`) starts goroutines for `export`, `dry_run`,
    `import` and `verify`. It is not a queue. **What a restart or a cancel does
    (checked in `internal/importer/import.go`):** `importer.Run` writes ALL rows in
    ONE transaction. It commits at the end and rolls back on any error or on a verify
    mismatch (`ErrMismatch`). Only the file uploads to the store happen outside it,
    and they are kept (a re-run finds the objects and sends nothing). So:
    - A **cancel** or a **restart** during `import` rolls back every row; nothing the
      run wrote stays. The next run starts from the beginning and is faster only
      because the files are already uploaded. The UI says "a stopped import keeps no
      rows; start it again". It is **restartable, not resumable**: do not call it
      resumable anywhere.
    - **The transaction holds one pool connection and its row locks for the whole
      run.** The duration of a large import is not measured here. Ticket 7 measures
      it on the real export and states the pool size it needs (one connection spare
      beyond the API's normal load; a measurement recorded in ticket 7, not an open point).
    - The job context is the transaction's context: a cancel cancels it, and the driver rolls back. **The UI may show `failed` (a stale heartbeat) while a stalled process still holds its transaction;** the `ImportLockOrg` lock covers the data, so a new import waits on it (cancellable) rather than corrupting anything.
    - **A restart marks a running job `failed: interrupted`, by heartbeat, not by
      boot.** Two server instances can run, so a boot cannot know that a job is
      dead. A running job row has `instance_id` (random at process start) and
      `heartbeat_at`, updated every 5 s. A sweep (same loop as the cycle job, every
      minute) marks `failed` with code `interrupted` any `running` job whose
      heartbeat is older than 60 s. That frees the partial unique index of #37.
    - **Two guards, both real.** (a) The partial unique index of #37 is the web guard: one
      active job per org across instances. (b) **`importer.Run` already takes a transaction advisory
      lock** (`ImportLockOrg`, `pg_advisory_xact_lock(hashtextextended('devops:import:'||org))`,
      `db/queries/importer.sql`, called in `import.go` after the uploads). It stays: it is the guard
      between the CLI and the web and between two runners. A second runner waits on it while it holds a pool
      connection, so ticket 7 must make the wait cancellable (the job context) and tests a cancel during
      the wait. Normal writes are not blocked by the lock (it only serialises imports), but whether
      ordinary API writes stall during the single long transaction (row locks on the rows it touches) is
      not known: ticket 7 measures and records it.
    - **Cancel works across instances.** `import_jobs.cancel_requested boolean`: `POST
      /import/jobs/{id}/cancel` sets it (it may land on the other instance), and the owning runner reads
      it at every heartbeat (5 s) and cancels its job context. If the heartbeat is stale (#36 above) the
      sweep marks the job `failed: interrupted` instead.
37. **Tables (migration C).**
    - `import_sessions`: `id`, `org_id`, `created_by`, `state` (`new|exporting|exported|user_map|dry_run_ok|importing|imported|verified|failed|closed`; transitions in #43), `export_dir`
      (a name under `IMPORT_WORK_DIR` made by the server, never a path from a request),
      `user_map jsonb` (it can hold Linear user emails as keys or values: PII, kept only for the session's life and swept with it (#39); the importer's own shape, `{default, users}`: see #45),
      `workspace_name`, `failed_files integer NOT NULL DEFAULT 0` and `failed_file_names jsonb NOT NULL DEFAULT '[]'` (at most 20 entries `{name, reason}`; set by the export job's manifest check, or by the same check when a CLI export is adopted, #44; the dry-run gate of #39 reads these session values; `GET /import/sessions/current` and `GET /import/jobs/{id}` return them), `export_hash` and `user_map_hash` (sha256 of the export manifest and of the stored map; every change to either recomputes it), export counts, `created_at`, `closed_at`. At most one open session per org, by a partial unique index `(org_id) WHERE state <> 'closed'`.
    - `import_jobs`: `id`, `org_id`, `session_id`, `kind` (`export|dry_run|import|
      verify`; CHECK), `state` (`queued|running|succeeded|failed|canceled`), `phase`
      (the entity being worked on), `done` and `total`, `counts jsonb` (the importer
      `Report` shape), `rows jsonb` (verify rows, capped at 500 with a flag), `committed boolean` (#42c),  `export_hash` and `user_map_hash` (copied from the session when the job starts; a dry run is "current" for `run` only when both equal the session's now),
      `error_code`, `error_text`, `cancel_requested`, `instance_id`, `heartbeat_at`, started and finished times. **`error_text` is a short message of ours** chosen from a fixed list by `error_code`; the importer's own error strings can carry a user email, a ticket title or Linear's text, so they go to the server log only after the same scrubbing as T4 (no emails, no titles) and never into the row or the response; the row keeps the entity and the counts. **A partial
      unique index** `(org_id) WHERE state IN ('queued','running')` gives one active job
      per org.
    - Neither table holds the Linear key. Git backfill (#21) is not a job and has no
      row here.
38. **The Linear key lives in memory only.** It arrives once, in the body of
    `POST /import/sessions/{id}/export`, goes into the goroutine's closure and the
    `linear` client, and nowhere else: not a row, a log line, an event, an
    idempotency row (`idempotency.Reject`), an error message or the response. A wrong
    key answers `linear_unauthorized` and a network error `linear_unreachable`, never
    with Linear's text. The request body is decoded with the key field as `json.RawMessage` (the standard `DecodeJSON` would make a `string`), copied to a `[]byte`, and the raw buffer is overwritten; the `[]byte` is held in the job runner and overwritten when the export job ends. Ticket 7 states which of these it does; if the helper cannot do it cheaply, one string copy is accepted for the job's life. **`linear.NewDownloader` takes a `string` today** (`linear/files.go`): a Go string cannot be wiped, so ticket 7 (which also notes in a code comment that `linear/files.go` follows redirects to Linear's signed storage without `netguard`: the target is Linear's choice and is accepted) either changes it (and the client) to take `[]byte` and build the header per request, or accepts the one string copy, which lives only as long as the export job and is left to the garbage collector. Proposed: change it if cheap, else accept and say so in a code comment.
    The import and verify jobs do not need it: they read the export on disk. A test
    proves no secret in logs, events or idempotency rows.
39. **Work directory.** `IMPORT_WORK_DIR` (default `/work/import`, a 0700 volume in
    the production compose, mounted only on `app`; `deploy.sh` makes it; the dev
    compose too). An export is up to about 200 MB for a large workspace and holds
    ticket text and user emails, so: files 0600 and the session directory 0700 (**the export code writes its directories 0750 today**, `linear/export.go`, `linear/files.go`: ticket 7 changes those `MkdirAll` modes to 0700 or sets the work dir's own mode to 0700 so the parent bars others, and a test checks both), the session directory is swept when
    the session closes (explicit `close`, or 7 days after the last job, by the sweep
    of #36), and the free space is checked before the export, and the runner passes `linear.DownloadAll` a `maxTotal` derived from the free space (its default is 10 GiB). **`DownloadAll` does not fail when the limit is reached:** it sets `Error = "skipped: total download limit reached"` on each remaining file and the export still succeeds (any failed download is recorded as a non-fatal error in the manifest). Such a file keeps its `uploads.linear.app` URL in the text, so `verify` would later fail on `linear_upload_url`. So **the export job inspects the files of the manifest after the export** (#40): any file with the "total download limit" error fails the job with `no_space`; any other file error does not fail it but sets `failed_files` and `failed_file_names` on the session (#37), and the wizard (ticket 8) shows the count and the first 20 names in step 1, before step 3. `POST .../dry-run` then answers `409 conflict` while `failed_files > 0` unless the body has `acceptFailedFiles: true` (the admin has seen the list). (Refuse an export under a proposed 1 GiB free: `no_space`.) Path handling uses `filepath.Join` on a name the server made;
    nothing from the request becomes a path.
40. **Progress.** `importer.Options` and the `linear` export take a progress
    callback `func(phase string, done, total int)`. The runner debounces it into the
    job row (at most once a second; the heartbeat is separate). The client polls
    `GET /import/jobs/{id}` every 2 s (no websocket, no event: see #52). The export
    phases are the Linear entity types, the import phases are the importer's own
    order, and the files phase reports bytes. **The importer and runner changes are these and no
    others:** the callback, a check of the run context between entities
    (`ctx.Err()`), so a cancel is prompt, and the post-export manifest check of #39 (in the runner: it reads the manifest's file errors). Its API and the user-map shape are
    unchanged.
41. **Routes** (org admin; the session cookie works with the CSRF header):

    | Route | Notes |
    |---|---|
    | `POST /import/sessions` | Start (or return the open one). Optional `{"adopt": "<name>"}` (#44). |
    | `GET /import/sessions/current` | State, export counts, last job per kind. |
    | `POST /import/sessions/{id}/export` | Body `{linearApiKey}`. Starts the export job. |
    | `GET /import/sessions/{id}/users` | The Linear users from the export, each with a suggested match (email, then username) and the org's members. |
    | `PUT /import/sessions/{id}/usermap` | The importer's `{default, users}` shape; every username must be an org member (validated). `409 job_active` while any job of the session is active. |
    | `POST /import/sessions/{id}/dry-run` | Body `{acceptFailedFiles?: boolean}`. Starts `dry_run` (the importer's own rollback). `409 conflict` while the session has `failed_files > 0` and the body does not set `acceptFailedFiles: true`. |
    | `POST /import/sessions/{id}/run` | Needs a `dry_run` job that succeeded on the CURRENT user map and export (a newer map or export invalidates it) AND a typed org slug (`confirmOrg`). Starts `import`. |
    | `POST /import/sessions/{id}/verify` | Starts `verify`. |
    | `POST /import/jobs/{id}/cancel` | Cancels the job context. A dry run or an import rolls back (#36); an export keeps the files it made. |
    | `GET /import/jobs/{id}` | Progress and result. |
    | `DELETE /import/sessions/{id}` | Closes, sweeps the work dir. `409 job_active` while a job of the session is active. |

    Cross-org is `404`; every route above has a cross-org test (a session or job id
    from another org). A second start while a job is active is `409 job_active`.
42. **Dry run, real run, verify rules.** (a) The dry-run counts must equal a real
    run's counts on the same export (a test). **A dry run writes nothing, to the database or to the store:** `uploads()` honours `dryRun` (`files.go`: `if r.dryRun { r.rep.Uploads.Planned++; continue }` before the upload; its doc comment says "none in a dry run"). It only READS the store (a `Head` per file) and reports `Uploads.Planned`. Its one real dependency is a reachable, readable store, which the session-start check (`503 attachments_unavailable`) already requires.  (b) A second real run reports zero
    creates. (c) **A verify mismatch has two cases, and the UI says which** (`import.go`): (i) the check INSIDE the transaction fails before the commit: `ErrMismatch`, everything rolled back, no rows kept; (ii) the second check AFTER the commit, on the pool, fails: `ErrMismatch` with the rows ALREADY committed. In both the job ends `failed` with code `verify_mismatch` (the CLI's exit 3) and the rows are shown; the job also stores `committed: true|false`. For (ii) the wizard says "The rows were written, but the check found a difference. Run verify again, or run the import again: a second run reports zero creates for rows that match."; for (i) it says nothing was written. (d)
    `verify` mismatches show as a table (entity, expected, found). (e) The importer's
    refusals stand: it refuses a cycle number already used; `--skip-files` is not
    offered in the web (no `S3_*` means the web import is off: `503
    attachments_unavailable` at session start, with a clear message). (g) **Session start with an empty `PUBLIC_BASE_URL` answers `409 public_url_required`** (T4b writes it into the imported texts and download URLs, so an empty one would write path-only links). The code is added by whichever of tickets 5 and 7 merges first (both list it). (f) **Edits to imported rows are lost on a re-run** (T4 sets imported rows by source id): the
    wizard says so before the real run and again before a re-run.
43. **Session states and the cycles guard.** The session moves only as in this table (a route outside it answers `409 conflict`):

    | From | Action or job result | To |
    |---|---|---|
    | none | `POST /import/sessions` | `new` (or `exported` with `adopt`, #44) |
    | `new`, `exported`, `user_map`, `dry_run_ok`, `imported`, `verified`, `failed` | `POST .../export` starts | `exporting` |
    | `exporting` | export job succeeds | `exported` (a new export changes `export_hash`, so any earlier dry run is no longer current) |
    | `exporting`, `importing` | the export or import job fails, is canceled, or is marked `interrupted` | `failed` |
    | `user_map`, `dry_run_ok`, `failed` (a dry run running) | the dry run fails, is canceled or is interrupted | `failed` |
    | `imported`, `verified`, `failed` (a verify running) | the verify job is canceled, interrupted or fails for a reason other than a mismatch | unchanged (the job shows `failed`; the session keeps its state) |
    | `exported`, `user_map`, `dry_run_ok`, `failed` (with an export) | `PUT .../usermap` | `user_map` (a map change recomputes `user_map_hash`; a dry run taken before it is no longer current, so a map edit after `dry_run_ok` drops back to `user_map`) |
    | `user_map`, `dry_run_ok`, `failed` (with a saved map and an export) | `POST .../dry-run` succeeds | `dry_run_ok` (allowed straight from `failed`; no reset is needed) |
    | `dry_run_ok`, `failed`, `imported`, `verified` (each needing a CURRENT succeeded dry run) | `POST .../run` starts | `importing` |
    | `importing` | the run succeeds | `imported` |
    | `importing` | the run fails (before the commit: no rows; after the commit: rows kept, `committed: true`, #42c) | `failed` |
    | `imported`, `verified`, `failed` (when the LAST import job has `committed: true`, or an earlier import job did and rows exist; a rolled-back re-run after a committed run leaves the rows, so verify stays allowed) | `POST .../verify` succeeds | `verified` |
    | the same | `POST .../verify` finds a mismatch | `failed` |
    | any state with no active job | `DELETE .../sessions/{id}` or the 7-day sweep | `closed` (final) |

    While a dry run or a verify runs, the session stays in its state (`dry_run` is not a session state): the wizard resumes from the last job of each kind. **`PUT .../usermap` while any job is active is `409 job_active`**, so a dry run's hashes cannot change under it. **Verify jobs, and a dry run started from `failed`, do not block turning cycles on** (they write nothing the cycle numbers clash with); only the states listed below do. `closed` allows nothing. `imported` and `verified` allow a re-run, a verify, a new export and close. While a job is active every start route answers `409 job_active`. Each state above can be reached through the routes, which is what tickets 7 and 8 test. **The cycles guard.** The server answers `409 import_in_progress` to `PATCH .../teams/{team}
    {"cyclesEnabled": true}` (turning ON only; off is never blocked) when the org has an import
    session in one of these exact states: `exporting`, `exported`, `user_map`, `dry_run_ok`,
    `importing`. It does not block in `new`, `imported`, `verified`, `failed` or `closed`, and a session that
    has never started a real run does not block once it is `closed`. **No session means no block.**
    (T3 #35: cycles on first makes a cycle 1 that clashes with Linear's numbers; a started import
    that has not finished is what clashes.) **An abandoned session is cleared** by `DELETE
    /import/sessions/{id}` (state `closed`), or by the 7-day sweep of #39 for a session whose last job
    ended more than 7 days ago; a session that never started a job ends 7 days after `created_at`.
    The web Teams switch (shipped in LAUN-3, `TeamCycles.tsx`) handles no 409 today; ticket 7 owns
    the guard and ticket 8 owns the web message "An import is running" with a web test. Ticket 7's AC
    tests each state above.
44. **The CLI stays** for an offline export (decision C3): `cmd/importer export`
    writes a directory. Open point 11: let the web step 1 accept an uploaded export.
    Proposed: not in v1. A directory made by the CLI can be placed in
    `IMPORT_WORK_DIR` by an operator and adopted by `POST /import/sessions {"adopt":
    "<name>"}` (**an adopted export gets the same manifest check as an export job (`no_space` for a download-limit error, `failed_files` for others), which sets the same session values, since there is no export job to do it;** **the name is the one request value that becomes a path, so:** it must match `^[A-Za-z0-9._-]{1,64}$` and not be `.` or `..`; the server `Lstat`s it under the work dir and refuses a symlink or anything that is not a directory; it is then opened with `os.Root`, so no later step can leave the directory): proposed, ticket 7,
    cheap and uses the same session.
45. **The user map needs members; `create` is not a map entry.** The importer's map is
    `{default, users}` (`internal/importer/usermap.go`): each value is an existing
    username, and an unmapped user with no default is an error. T4 has no `create`.
    Decision: the wizard's "Create agent" button calls the EXISTING agent route
    (`POST /orgs/{org}/agents`, T10 rules) at once, then selects the new username in
    the table; the map the server stores and passes to the importer has only existing
    members. So no importer change, and the dry run and the real run see the same
    members. A human is never made here (no email: invite them first, T12). The
    wizard table gives each Linear user a member picker, "Create agent" and a default
    for the unmapped.

## Screens

46. **Settings > Integrations** (org admin; nav item beside Members). A list: one
    row per integration, with provider icon, name, base URL, mode, status and the
    last delivery (result and time). "Connect": a dialog, three steps. (1)
    Provider (GitLab, GitHub, Bitbucket) and base URL (prefilled for the hosted
    one; a "self-hosted" choice shows the field). (2) Mode: token (Launchpad makes
    the hook; shows the minimal scopes of #23 and a link to the host's token page)
    or webhook only (copy the URL and the secret once; steps for the host). (3)
    Done. A copy-once dialog follows the ORB-3447 pattern: plain async call, no
    `useMutation`, no cache, the value only in component state, cleared on close.
47. **Integration detail.** Status line with the reason (`secret_unreadable`,
    token rejected, hook failed). Repos: a searchable list of the provider's repos
    (token mode) with a map button, or a form `repo id + path` (webhook only), a
    team picker (T5 #23), the hook state, unmap. (The "Backfill open MRs" button is part of ticket 11, with its route.) Deliveries
    table from the ring, with the result chip. Actions: rotate secret (copy-once),
    replace token, enable or disable (with the sentence of #13), delete is not in v1
    (disable it, T5). The webhook-only help shows the exact header and event the
    host must send.
48. **Settings > Import from Linear**, five steps (LAUN-10, adjusted): (1) paste
    the key and watch the export; (2) the user map table; (3) the dry-run report (counts, exclusions, and how many files a real run WOULD upload: `Uploads.Planned`)
    (counts, exclusions, files); (4) typed org slug, run, watch the progress,
    then verify; (5) a link to turn cycles on. A banner when a job runs, so a
    reload resumes the view from `GET /import/sessions/current`. The key field is
    `type=password`, `autocomplete=off`, held in component state only, cleared on
    submit; the query client has no cache entry for the body; the key is in no URL
    or storage and no error text. A web test checks a 401 and a network error do
    not show the key.
49. **Settings > Teams** already has the Cycles switch (LAUN-3, shipped: `TeamCycles.tsx`, with the
    T3 #35 text). The 409 `import_in_progress` of #43 shows as "An import is running." (ticket 8)
50. **Issue page** gets the "Changes" block of #18. The issue list does not. Block
    rules: nothing renders (not even a heading) when `changeRequests` is empty; while
    the issue loads the block is not drawn; an MR the viewer cannot open (a link is a
    plain external anchor) is still shown, because the data came from the org's own
    hook; the checks badge is absent, not "unknown", when no checks event has come.
51. **Web rules for all new screens.** The pages are added to
    `web/src/features/settings/settingsPages.ts`, with the same org-admin gate as
    Teams (web-decisions "Settings area": shown to org admins only; a platform admin
    who is not an org admin does not see them). Secret-bearing calls (token, secret,
    Linear key) use the plain async pattern of ORB-3447: a plain async call, no
    `useMutation`, no query cache, state held in the component and cleared on close.

## Events

52. **`events.Emit` policy (AGENTS.md convention).** Org-scoped writes that change
    state emit an event as the LAST statement of their transaction, except where
    this list says otherwise.
    - **Integration CRUD, repo map, rotate, deliveries ring: no event.** An explicit
      exception, like T5 #5 (config changes emit no event) and the AGENTS.md exception list (install-level settings: T9 #12; T1 #28 is token create and revoke, not a precedent for config):
      they hold secret-adjacent config, and nothing in the web needs a live feed of
      them. Ticket 5 adds one line for this to the AGENTS.md exception list.
    - **Import sessions and jobs: no event.** Progress is polled (#40); an org-wide
      feed would carry progress to every connected client. The rows the importer
      writes follow T4 as they are today (no event per row).
    - **MR-link changes: yes.** Every `HandleSCMEvent` that changes a link, its state,
      review or checks, or that writes a timeline row, emits `issue.updated` for each
      affected issue (T5 #20 already does it for a move and `attachment.created` for a
      new link), with `source` the provider and a `mergeRequest` marker, as the LAST
      statement of the same transaction, after the link rows and the new `issue_activity` rows (#20) are
      written (AGENTS.md: Emit is last). Ticket 11 reads
      `web/src/api/events.ts` and `docs/internal/web-decisions.md`, wires the existing
      `issue.updated` invalidation to refresh the issue and its Activity, and has this
      AC: a live MR event updates the "Changes" block and the Activity without a reload.
    - Install-level state (none here) would follow T9 #12 as well.

## Phases and dependencies

```
1 scm ──┬─> 2 netguard+gitapi ─────────────────────────────────────────────┐
        └─> 3 migration A + port ──> 4 sealed secrets + GitHub receive ───┤
                                                                         v
                                                      5 config API rework (needs 2, 3, 4)
                                                       └─> 6 connections UI
                                                            └─> 11 MR state + backfill (needs 2, 4, 5, 6)
                                                                 └─> 10 Bitbucket (needs 2, 4, 5, 6, 11)
                                                                      └─> 12 checks (needs 5, 6, 10, 11)
7 import job (migration C; independent of 1-6) ──> 8 import wizard UI
9 cycles toggle (LAUN-3; its 409 guard lands with 7)
13 docs: after 5, 6, 7 and 8 (last)
```

Two lanes can run in parallel: the git lane (1 to 6, 10 to 12) and the import lane
(7, 8, 9). Ticket 13 closes both.

## Ticket split (build order; ticket 10 comes after ticket 11, and ticket 5 may be split at the deliveries ring and the hook refresh if it overruns)

| # | Ticket | LAUN id | New or edit | Depends on |
|---|---|---|---|---|
| 1 | Extract `internal/scm`; GitLab parses into it | LAUN-4 | edit | none |
| 2 | `internal/netguard` and `internal/gitapi`: one SSRF guard and the provider clients | LAUN-6 | edit and rename | 1 (the shared scm client types), T16 guard if landed |
| 3 | Migration A: `git_*` tables, provider columns, port of the GitLab code | new | NEW (split from LAUN-5) | 1 |
| 4 | Sealed git secrets and GitHub webhook receive | LAUN-5 | edit (narrowed) | 1, 3 |
| 5 | Integrations config API rework and deliveries (migration B) | LAUN-7 | edit | 2, 3, 4 |
| 6 | Git connections UI (GitLab, GitHub; Bitbucket is a row later) | LAUN-9 | edit | 5 |
| 7 | Import job runner and routes (migration C) | LAUN-8 | edit | none |
| 8 | Import wizard UI | LAUN-10 | edit | 7 |
| 9 | Cycles toggle in team settings | LAUN-3 | shipped; nothing to do | none |
| 10 | Bitbucket provider, Cloud and Data Center | new | NEW | 2, 4, 5, 6, 11 (ticket 6 for the connect dialog; it parses `approved`, which writes `mr_links.review`, a migration D column) |
| 11 | MR state on the ticket: Changes block, review state, timeline entries, backfill (migration D) | new | NEW | 2, 4, 5, 6 (ticket 6 for the repo list that holds the two buttons; Bitbucket approval events in the hook spec arrive with ticket 10, which adds them to its own spec) |
| 12 | MR checks status from provider events | new | NEW | 5, 6, 10, 11 |
| 13 | Docs and cutover runbook | LAUN-11 | edit | 5, 6, 7, 8 (the Bitbucket, MR state and checks docs ship with tickets 10, 11 and 12) |

The ticket texts below are the acceptance criteria.

1. **LAUN-4.** Move the neutral types and the `Provider` interface in, **and declare the shared client types that ticket 2 implements** (`ClientConfig`, `APIClient`, `RepoRef`, `HookSpec`, `Identity`, `RepoPage`, `Change`, `ChangePage`; types only, no HTTP);
   `internal/gitlab` becomes the first implementation (`Parse`, `Verify`,
   `DeliveryKeys`); one `HandleSCMEvent`. Add every field of P2 to `Event`: `Kind`, `Author`, `RepoPath`, `Draft`, `Fork`, `SourceRepoKey`, `HeadSHA`, `Review`, `Checks` and `OccurredAt` (which fills `provider_updated_at`); the new fields are unused yet. A legacy GitLab row verifies by hash, and `Provider.Verify` is not on that path (the token check stays as T5 has it). AC: all T5 tests pass unchanged; `go vet` clean;
   no behaviour change.
2. **LAUN-6.** (It is large: if the builder wants, split it at the Bitbucket client into two tickets. It depends on the "verify at build" facts of #15: the Bitbucket path encoding, GitHub's hook listing path and GitLab's `ListRepos` token scope; ticket 5 rechecks them against its fakes.) `internal/netguard` built to T16 #34. **Ticket 2 owns `internal/netguard`**: it has no dependency on the backup lane, so it lands first; LAUN-70 (T16 ticket 3) imports it and adds only the backup policy (if LAUN-70 merges first, ticket 2 moves the guard to `internal/netguard` and adds the git policy only). It adds the git policy of #31, the two variables and the compose passthrough;
   `internal/gitapi`: the guarded client (#32) and clients for GitLab, GitHub and
   Bitbucket (check token, list repos, ensure and delete hook, `GetChange` and `ListOpenChanges` with an opaque cursor, at most 5 pages). AC: the T16 #34 table
   plus the app-subnet, service-name and gateway rows of #31 all refuse; the one-rule
   semantics of `GIT_ALLOW_PRIVATE_*` (#31) is tested for each of the four
   combinations; the test seam of #31 exists and a grep test proves production code has
   no bypass; `httptest` fakes for 401/403/404/429/5xx and an oversize body; a token
   never appears in an error string; **the token is never sent to a second origin
   (a fake host whose `next` link and whose cursor name another origin: the second
   origin sees no `Authorization`)**; cursors are opaque; a failing read of the app's interface addresses FAILS CLOSED (every private address refused); compose-check fails on a
   missing variable.
3. **Migration A and the port (new).** Migration A (#8, without MR-state columns) and
   the port of EVERY GitLab query, handler, `HandleSCMEvent` call and test to the
      renamed tables. **`DeliveryKeys` changes to the `gl:` form in this same MR and that is the only change to stored keys.** The GitLab auth path and `ListGitlabSecretHashes` filter `provider = 'gitlab'` (a GitHub row must never be a candidate for the token compare). AC: all T5 and GitLab tests pass on the renamed tables (the tests that assert delivery-key strings are updated to the `gl:` form; nothing else in them changes); the OpenAPI `source` enums (for `issue_state_changes` and `issue_activity`, and `schema.d.ts`) gain `github` and `bitbucket` so `TestOpenAPISchemasMatchModels` passes, with the old
   config routes still in place; the live GitLab webhook answers byte-identical before
   and after (secret hash check, URL, bodies); the prune deletes rows older than 30 days and keeps newer ones (boot and hourly); constraint names list; up/down/up and a
   backfill test (including the `gl:` prefix, the casts, the new columns being NULL or their defaults with their CHECKs holding, and the down policy); a test
   inserts a GitHub integration row directly (no create route yet).
4. **LAUN-5 (narrowed).** (The GitHub Provider's `Client` returns "not wired" until ticket 5; ticket 5 adds the `next`-secret verify path for GitHub and GitLab.) (Also: AGENTS.md's "never let anything answer before the token check" is reworded here to carry the #14 exception, since this ticket introduces the signed-body path.) The second keyring scope and its new constructor (#11),
   `internal/github` (parse, HMAC, plan), the `/integrations/github/webhook` route
   (#14 to #17) with its OpenAPI entry and the error codes assigned to it in #12 (`length_required`, `overloaded`). Tests insert integration rows directly. AC:
   fixtures: `ready_for_review` moves a branch-named issue to In Review, `closed` plus
   merged moves it to In Testing, `converted_to_draft` and a plain `updated` are answered `200 ignored` and write nothing (the `draft` and refresh columns arrive in ticket 11; T5 also wrote nothing for a plain update), a
   redelivered GUID is `duplicate`; a fork PR (head repo id different from the base repo id, or a deleted fork whose head repo is null) is `200 ignored` (the recommended default of open point 16; this AC changes only if the project owner chooses otherwise); a bad signature, a missing header and an unknown org
   are the same `401`; no `Content-Length` is `411`, above 2 MiB is `413`; a slow
   reader is closed at the deadline and its bytes are freed; the in-flight budget
   returns `503` and `Retry-After`; no secret in logs, events or idempotency rows; `attention_code` is created in ticket 3, set in ticket 5, and in ticket 4 only logged;
   unreadable sealed values give `401` and a log line that carries only our code (`secret_unreadable`, #33) (the status logic and the ring come in ticket 5, which adds the ring writes to BOTH the GitLab and the GitHub handlers).
5. **LAUN-7.** The routes marked ticket 5 in the table of "API routes" (not backfill: ticket 11), with `attention_code` set and cleared by #11a, the one-line AGENTS.md events-exception edit of #52, ring writes in both webhook handlers, and only registered providers accepted by create (including `check`, repo `PATCH`,
   `rotate-secret/commit`), the deliveries ring (migration B), token mode (hook create,
   delete, rotate with the pending secret) and webhook-only (secret once), removal of the
   old GitLab config routes and the glab script, OpenAPI with the error codes assigned to this ticket in #12 (`integration_limit`, `provider_*`, `public_url_required` (if ticket 7 has not merged first), `encryption_unavailable`); both the token-mode `public_url_required` 409 and the webhook-only path warning are tested. AC: a
   webhook-only create shows the secret exactly once; token mode makes one hook; a second map of the same repo is `409 conflict` (list-and-match matters only after an unmap or for a concurrent map); unmapping deletes the hook (a
   host 404 counts as done) by map row id; disabled integrations answer `401`; a rotation
   with one failing hook keeps both secrets accepted and loses no delivery; the ring holds
   100, stores no payload field, and the `bad_signature` row is throttled in memory; the
   live GitLab webhook answers unchanged; a URL change clears the token and a following `check` sends nothing to the new host; a URL change with a repo mapped is `409 conflict`; the rate limit of #28 is tested (at 6 a minute with burst 3, a fourth immediate request is refused with `429 rate_limited`); **retry reuses pending** (a second token-mode `rotate-secret` returns the same pending secret and leaves the hooks that already hold it valid); `integration_limit` (the 9th enabled, and an enable that would make a 9th), `encryption_unavailable` (create, rotate, PATCH token), the `provider_*` mapping (401, 403, 429, 5xx, timeout from a fake host), and `409 conflict` when the token is replaced and the URL changes in the same PATCH; if the project owner has decided the GitLab fork rule (open point 16), a GitLab MR whose source project differs from the target is `200 ignored` (a test; otherwise this line is dropped); the verify loop is extended to `next` (current and pending) for GitHub, and the GitLab hash path too, with tests for both; a cross-org test for every route;
   `TestOpenAPIMatchesRoutes` and the error-code enum test (the table of #12); the T5 route tests (`gitlab_integration_test.go` config cases) move to the new routes, and the old-route cases are removed with the routes.
6. **LAUN-9.** Settings > Integrations per #46, #47 and #51, provider-neutral so
   Bitbucket is a row. AC: the token and secret appear in no cache, storage, URL or
   error text (a test dumps the query client's cache); the pages are in
   `settingsPages.ts` and hidden from non-admins, with `keywords` set for the settings search; the disable sentence shows; keyboard
   and screen-reader pass of the connect dialog.
7. **LAUN-8.** Migration C, `internal/importjob`, the routes of #41, the callback and
   `ctx` check (#40), the work dir and its sweep, the heartbeat sweep (which also sets the session to `failed` in the same transaction as it marks the job, #43), the `409
   import_in_progress` guard (#43), adopt (#44), and the error codes assigned to this ticket in #12. AC: dry-run counts equal a real run; a
   second run reports zero creates; verify mismatch shows as rows; no secret in logs,
   events or idempotency; one active job per org (two parallel starts, one `409
   job_active`); **a cancel or a restart during `import` leaves no rows (the single
   transaction rolled back) and a second run completes, with its files already in the
   store (not "resumes")**; a mismatch before the commit keeps no rows and a mismatch after the commit keeps them, each with `committed` set correctly on the job (the texts are ticket 8's); a heartbeat older than 60 s is marked `interrupted` and frees
   the index; the cost of a large import (duration and pool use) is measured and recorded
   in this ticket; `IMPORT_WORK_DIR` is 0700 and swept; a cross-org test for every import route; the user map keeps the importer's `{default, users}` shape; an export, dry-run or import job marked `interrupted` makes its session `failed`, and a verify job marked `interrupted` leaves the session as it is (the transition table of #43; a test); the compose and deploy changes: the `IMPORT_WORK_DIR` volume in both compose files and in `deploy.sh`, with `compose-check` and `deploy-test` updated; the exact session states of #43 each block or allow `cyclesEnabled: true`; a second runner that waits on `ImportLockOrg` can be cancelled; cancel sent to the other instance works through `cancel_requested`; the work dir modes of #39; an export that hits the download limit fails with `no_space`, and an export with another file error succeeds with `failed_files` and `failed_file_names` (at most 20) set on the session and returned by `GET /import/jobs/{id}`, an adopted CLI export gets the same check and the same values, and the dry run answers `409 conflict` until `acceptFailedFiles: true` (tests with a fake Linear and a small `maxTotal`); a session start with no `PUBLIC_BASE_URL` is `409 public_url_required`; a dry run writes no object to the store and no DB row, and `Uploads.Planned` equals the number of files a real run then uploads (a test; a missing or unreadable store fails the start with `503 attachments_unavailable`); the routes of #41 are in `api/openapi.yaml` (`TestOpenAPIMatchesRoutes`); the AGENTS.md no-event exception line for import sessions and jobs (#52) is added; the `app` container (read-only root, uid 10001, `cap_drop: ALL`) can write to the `IMPORT_WORK_DIR` volume (a named volume is root-owned unless the image's directory is owned by 10001: `compose-check` or an init step proves it, because `deploy-test` stubs docker); two ACs are **record only** (no pass or fail threshold): the pool use and duration of a large import, whether normal API writes stall during the long transaction, and whether the import transaction blocks the hourly cycle job (it takes `ImportLockTeamCycles`).
8. **LAUN-10.** The five steps of #48 with polling and resume of the VIEW. AC: the key
   appears in no cache, storage, URL or error text, with a test where a 401 and a network
   error do not show the key; the wizard reloads into the running step (a test that
   mounts it with a `GET /import/sessions/current` answer of each state); the wizard says
   a stopped import keeps no rows and that edits to imported rows are lost on a re-run;
   "Create agent" calls the agent route and selects the user; **step 1 shows the failed-file count and the first 20 names, and the dry-run button sends `acceptFailedFiles: true` only after an explicit tick; step 3 shows `Uploads.Planned`; the #42c texts show for `committed: false` ("nothing was written") and `committed: true` ("the rows were written, but the check found a difference...") (web tests for each)**; the Teams cycles switch shows "An import is running" on `import_in_progress` (a web test); the page has `keywords` in `settingsPages.ts`.
9. **LAUN-3.** Shipped (`TeamCycles.tsx` is on develop). Nothing to build. The server guard that it lacks is in ticket 7, and its web message in ticket 8 (#43).
10. **Bitbucket (new): "Bitbucket provider (Cloud and Data Center): webhook and parser".** `internal/bitbucket` (parse, HMAC, plan; **no HTTP client**: ticket 2 built `internal/gitapi/bitbucket`, Bearer tokens only, and this ticket only wires it into the Provider's `Client` method and tests it; it registers `bitbucket` in ticket 5's create route; it needs ticket 11 for the `review` column; its hook spec includes the approval events),
    route `/integrations/bitbucket/webhook` and its OpenAPI entry, repo ids (Cloud UUID in
    braces; Data Center numeric id and `PROJ/slug`). AC: hand-written fixtures per
    documented payload for Cloud and Data Center (opened, updated, fulfilled or merged,
    declined or rejected, approved); the same moves as GitLab; a signature check with each
    secret kind; a missing `draft` field is not a draft; the Bitbucket hook events, `hook/refresh` and backfill are tested against a fake Bitbucket (Cloud and Data Center); the ring is written for each outcome (processed, ignored, duplicate, bad signature) as ticket 5 does for the other two providers; a Data Center without a webhook
    secret is explained in the connect dialog's text in webhook-only mode ("this version cannot sign webhooks; it cannot be used"); the "verify at build" facts of #15
    are checked and recorded; the connect dialog lists Bitbucket; cross-org and no-secret
    tests.
11. **MR state (new): "MR state on the ticket: Changes block, review state, timeline
    entries, backfill".** Migration D (the `mr_links` columns of #8 and #18, and the
    `issue_activity_kind_check` rewrite, with a down per #8), `changeRequests` on the issue
    (REST, OpenAPI, MCP; `TestOpenAPISchemasMatchModels`), the review events (with the hook spec change and the hook refresh of #24, and an AC that a mapped repo's hook gets the new events after a refresh, and that the "Refresh hooks" and "Backfill open MRs" buttons (both in ticket 11, in the repo list of #47) call their routes, show the result and hide for a webhook-only integration) and the
    `updated` refresh of #16, the three activity kinds (OpenAPI enum, `schema.d.ts`,
    `activityModel.ts`, `Activity.tsx`), the "Changes" block (empty and loading rules of
    #50), the events of #52 and the backfill route of #21. AC: the block shows
    draft/open/approved/merged/closed; an approval does not move the ticket; backfill makes
    the same links as live events, moves forward only, caps at 200 and answers
    `truncated`; a stale event does not change the state, and an event older than `provider_updated_at` writes and moves nothing (a test with a delayed older `closed` and a delayed older `opened`); an issue with no MR is
    unchanged in the response; the new activity kinds render and the migration up/down/up
    passes; a live event updates the block and Activity without a reload.
12. **Checks (new): "MR checks status from provider events".** (Needs `checks_updated_at` and `checks_detail` of migration D, #18.) `check_suite` / `status`
    (GitHub), `Pipeline Hook` (GitLab), commit status (Bitbucket) update `mr_links.checks`
    by head sha; token-mode hooks gain the check events for all three providers through the `HookSpec` and the `hook/refresh` route of #24 (an AC: a mapped repo's hook has the check events after a refresh, for each provider fake); the badge. It therefore needs tickets 5 and 10 as well as 11. AC: out-of-order events keep the later one per check name (`checks_detail`), the worst entry wins (one failing app plus one succeeding app is `failure`), a new `head_sha` resets the checks, and a check event has its own delivery key without a PR number; a sha with no link is `200 ignored`; a fixture per provider.
13. **LAUN-11.** Replace the two named files with this note (T17) plus the T4 and T5
    pointers; update AGENTS.md ("GitLab webhooks", "Linear importer": the "no web screens"
    lines),  `docs/deploy.md` (web path primary, CLI
    fallback; the variables `GIT_ALLOW_PRIVATE_PROVIDERS`, `GIT_ALLOW_PRIVATE_CIDRS`,
    `IMPORT_WORK_DIR`; `SETTINGS_ENCRYPTION_KEY` now covers git, with no rotation story),
    the glab script retirement, and the org slug `orb` and MCP name `launchpad` examples
    (ORB-3459); the names `docs/decisions-T13-git-connections.md` and `docs/decisions-T14-import-wizard.md` in LAUN-11 are replaced by this T17 note, and the web rules of #51 are appended to `docs/internal/web-decisions.md`; the runbook says to set `TRUSTED_PROXY_CIDRS`, restates the Linear-key rule (`LINEAR_API_KEY` in the environment, nothing else: AGENTS.md and T4; the web path takes it once in a request body and never stores it), and says that under the private switch a Git host on the Docker host itself needs the host's LAN address, never the gateway or the published-port address (#31). Cutover of the live GitLab hook: migration A maps it into the new rows;
    rotate the secret once in the web app to store the sealed copy.

Ticket changes to file (LAUN ids; LAUN-3 shipped and needs no edit):

- **Edit LAUN-4** (ticket 1): add the shared client types (P1). No blocker.
- **Edit LAUN-6** (ticket 2): rename to "internal/netguard and internal/gitapi: one SSRF guard and the provider clients"; blocked by LAUN-4.
- **Edit LAUN-5** (ticket 4): rename to "Sealed git secrets and GitHub webhook receive" (the title has a stale migration number); blocked by LAUN-4 and the new migration A ticket.
- **Edit LAUN-7** (ticket 5): rename to "Integrations config API rework and deliveries" (stale number); blocked by LAUN-6, LAUN-5 and the new migration A ticket.
- **Edit LAUN-8** (ticket 7): rename to "Import job runner and routes" (stale number); no blocker.
- **Edit LAUN-9** (ticket 6): rename to "Git connections UI (GitLab, GitHub; Bitbucket is a row later)"; text of #46, #47, #51; blocked by LAUN-7.
- **Edit LAUN-10** (ticket 8): blocked by LAUN-8.
- **Edit LAUN-11** (ticket 13): blocked by LAUN-7 and LAUN-8, and by the web tickets LAUN-9 and LAUN-10 (its wording must stay true at merge).
- **Edit LAUN-70** (T16 ticket 3, backup library): the SSRF guard code goes in `internal/netguard`, owned by this note's ticket 2 (#31); `internal/backup` keeps only its own policy (`BACKUP_ALLOW_PRIVATE_CIDRS` and the silo dialer). **Merge order:** if ticket 2 merges first, LAUN-70 imports `internal/netguard` and adds only the backup policy; if LAUN-70 merges first (the guard in `internal/backup`), ticket 2 moves the guard to `internal/netguard` in its own MR, adds the git policy, and LAUN-70's package imports it from then on. A one-sentence pointer was added at T16 #34 and at T16 ticket 3 in this branch (marked "LAUN-1 note"); nothing else in T16 changed.

New tickets to file (all four take parent LAUN-1; ticket numbers in brackets are this note's):

- **"Migration A: git_* tables, provider columns and port of the GitLab code"** (3; blocked by LAUN-4).
- **"MR state on the ticket: Changes block, review state, timeline entries, backfill"** (11; blocked by LAUN-6, LAUN-5, LAUN-7 and LAUN-9).
- **"Bitbucket provider (Cloud and Data Center): webhook and parser"** (10; blocked by LAUN-6, LAUN-5, LAUN-7, LAUN-9 and the MR state ticket).
- **"MR checks status from provider events"** (12; blocked by LAUN-7, LAUN-9, the Bitbucket ticket and the MR state ticket).

## Tests every ticket keeps

- DB-backed tests are `*_integration_test.go` and run under `INTEGRATION=1`.
- Fixtures per provider are hand-written from the vendor's documented payloads
  with invented data, never a pasted real delivery (a `testdata/README.md` per
  provider, as T5). After the first real deliveries, scrub one of each action
  and compare (the T5 cutover step 7, per provider).
- `TestMigrateUpDownUp` for every migration of the tranche, and a backfill-and-down test for A and D (B and C make empty tables and have nothing to backfill). The letters A to D are labels, not the merge order; numbers are taken at merge time, and tickets 3, 5, 7 and 11 may merge in any order their dependencies allow.
- A no-secret test for each new route that takes a secret (log, events,
  idempotency): tokens, webhook secrets, the Linear key.
- `make sqlc-check schema-check compose-check` after the migrations and the compose
  change.

## Open points (proposed defaults, for the project owner)

1. **GitHub App.** Default: deferred (the project owner decided). A PAT is tied to a person
   and expires; webhook-only mode works with no token at all. Revisit when
   an org wants org-wide install.
2. **Hosted and self-hosted in one integration list.** Default: yes, one row per
   base URL; the `flavor` is a label only.
3. **Bitbucket draft.** Default: no draft state unless the payload has one.
4. **Refresh the link row on a plain `updated` event** (a change from T5 #16).
   Default: yes, needed for the state and the title; it never moves a ticket.
5. **Timeline entry and comment.** Default: Activity rows for linked, merged and
   closed; no comment on the ticket.
6. **Post back to the host.** Default: not in v1. If wanted later: a commit
   status only, behind a switch, with a write scope.
7. **Old GitLab delivery keys.** Decided in #17: migration A rewrites them once to the `gl:` form; there is no dual check and no 30-day window.
8. **Webhook pre-auth body limits.** Default: 2 MiB cap, a 32 MiB in-flight byte budget, a 5 s read deadline, `503` with `Retry-After` on overload (#14). Not measured; GitHub's documented cap is 25 MiB but
   a PR event is far smaller.
9. **Self-hosted private CA.** Default: not supported in v1; the operator adds
   the CA to the container.
10. **One guard, two switches.** Default: the shared `internal/netguard`; backups
    keep the CIDR list (`BACKUP_ALLOW_PRIVATE_CIDRS`); git keeps the project owner's boolean
    (`GIT_ALLOW_PRIVATE_PROVIDERS=1`, the standard private ranges) and also reads
    `GIT_ALLOW_PRIVATE_CIDRS`. Both need a shell on the host. The project owner may prefer one
    variable for both: then backups' list would also open git hosts, which is
    wider than it needs.
11. **Web import from an uploaded export.** Default: no; adopt a CLI export that
    an operator put in `IMPORT_WORK_DIR`.
12. **Free space floor for an export.** Default: 1 GiB.
13. **Rate limits** of the connection routes (6 a minute, burst 3) and the
    `bad_signature` ring cap (1 a minute). Defaults only; not measured.
14. **GitLab token scope.** `api` is wider than needed. Default: accept it and
    say so; a narrower path is webhook-only mode.
15. **Delete an integration.** Default: not in v1 (disable only, as T5).
16. **Fork PRs (decide before ticket 4).** A stranger's fork PR on a public repo can put an attacker-chosen title and author into the Changes block, the Activity and MCP `get_issue` output, and can name `LAUN-n` to link and move a ticket. **Recommended default: GitHub accepts a delivery for linking or moving a ticket only when the PR's head repo id equals the base repo id** (a same-repo PR); a fork PR is `200 ignored`. GitLab and Bitbucket have the same shape (source project or repo differs from target) and get the same rule. T5 accepted any MR; this is a tightening for hosts that allow public repos. The project owner decides, and the answer must be known before ticket 4 builds the GitHub plan. **Cost:** ignoring a fork PR on `merged` too means a maintainer-merged fork PR never moves the ticket to In Testing; the ticket is linked by nobody and stays put (the cost of the rule). **Owners:** ticket 4 for GitHub; ticket 10 for Bitbucket; ticket 5 for GitLab, once decided. Until the project owner decides, GitLab stays open (T5's behaviour) and ticket 1 stays "no behaviour change" (it only adds the `Fork` field to the event). Whatever the answer, host titles and authors are rendered as plain text in the web (never HTML or Markdown) and are labelled as external text in MCP output (`source: "github"` and a note that the text is from the host).

## Build notes: ticket 4 (LAUN-5)

What the build chose where the note left room.

- **No migration.** Migration 0012 (LAUN-82) already holds every column used.
- **`GitKeyring` is a separate type** next to `Keyring` in `internal/platform/crypto.go`
  (`NewGitKeyring`, `SealFor`, `OpenFor`), with the one info string `launchpad
  git_integrations sealing v1`. The tracker takes the `GitSealer` interface, so it
  does not import `platform`.
- **Read deadline.** `http.ResponseController.SetReadDeadline` cannot read the old
  deadline back, so it is cleared after a complete read (the server sets its own
  again before the next request on the connection). After a FAILED read it is left
  expired on purpose: the server's drain of the unread body then fails at once and
  the connection closes (found by the slow-reader test: with the deadline cleared the
  connection stayed open until the sender gave up). The answer to a failed read is
  `400 bad_request` with `Connection: close`; no new error code.
- **The byte budget is released after the signature check**, not at the end of the
  request: it counts unsigned bytes in flight.
- **A signature that matches two integrations is `401`** (as the GitLab hash check
  does); ticket 5's secret-in-use check keeps that from happening.
- **Ignored without a row.** A fork PR, an action that asks for nothing
  (`converted_to_draft`, `edited`, `synchronize`, a label) and a `ping` answer
  `200 ignored` and write no `git_webhook_events` row. A `push` or any other event
  type is `202 ignored`. An unmapped repo is `202 unmapped` and keeps nothing.
- **Delivery key** is `gh:<delivery>:r<repo id>:pr<n>:<neutral action>`, so a
  `closed` that merged and one that did not have different keys.
- **Shared writer** (`HandleSCMEvent`) now takes the provider from the event: the
  history row, the `issue.updated` and `attachment.created` events say `github`; the
  link title is `PR #n: ...`; the `mergeRequest` object of the event is
  `{repoKey, number, url}` for GitHub (GitLab keeps `{gitlabProjectId, iid, url}`).
  `git_webhook_events.event_type` is `pull_request` for GitHub.

## LAUN-7a: integration config routes (built)

LAUN-7 is split (2026-10-26). **7a** (this section) is the config routes
without a migration. **7b** (rotation and repo map) and **7c** (deliveries ring,
needs a migration) are separate tickets. The ACs that moved:

- 7b: "Token mode makes one hook", "A second map of the same repo is 409",
  "Unmapping deletes the hook by map row id", "A rotation with one failing hook keeps both
  secrets accepted and loses no delivery", "The verify loop covers current and pending
  secrets for GitHub and for the GitLab hash path" (GitHub already covers both since LAUN-5;
  the GitLab hash path needs the pending hash), available-repos, repos, rotate-secret and its
  commit and discard routes, `provider_*` on repo map, and removal of the legacy
  `/integrations/gitlab/{integration}/projects` routes.
- 7c: the deliveries ring, `GET /integrations/{id}/deliveries`, ring writes in both webhook
  handlers, the `lastDelivery` summary of the list route (not in the list yet).
- Open point 16 (fork rule): not decided; behaviour unchanged.

What 7a does: `GET/POST /integrations`, `GET/PATCH /integrations/{id}`, `POST
/integrations/{id}/check`; the old `GET/POST /integrations/gitlab` and `PATCH
/integrations/gitlab/{id}` are gone (the webhook URL and the legacy project map routes stay).
Judgment calls:

- **Interim gap until 7b (closed by LAUN-86):** the old PATCH `secret` (GitLab rotation) was
  gone and the rotate route was not there yet. `rotate-secret` now exists (see "LAUN-86").
- Create `mode` defaults to `webhook_only`; `provider` is required. A token in webhook-only
  mode, a secret in token mode and a pasted GitHub secret are `400`.
- The duplicate-secret check is per `(org, provider)` over the current and the pending hash,
  under one advisory lock that also guards the 8-per-provider limit.
- The limiter (6 a minute, burst 3, per org and per user) covers `check`, a token-mode create
  and a PATCH that sends a token: the calls that reach a host.
- **Save-time SSRF check** (#29, #31): https only, except a local/test server or a host whose
  addresses are all in the five private ranges AND allowed by the git policy. Token mode (create,
  or a PATCH with a token) also refuses a compose service name and any address the policy
  refuses, resolving the name once; the dial-time guard still checks every connection
  (rebinding). Webhook-only makes no outbound call, so it gets the scheme rule only.
- `netguard.NewAttached` is wired in `cmd/server` (`gitHostAccess`): `Refresh` at boot (a
  failure logs an ERROR and leaves private hosts refused), `Run` hourly.
- `check` returns the integration plus `login` (may be empty). A good check clears every
  attention code except `secret_unreadable`.
- A new `Retry-After` field on `httpx.Error`.

### Vendor-API facts LAUN-6 could not verify (rechecked 2026-10-26, vendor docs only)

| Fact | Result |
|---|---|
| GitLab `GET /projects?membership=true&min_access_level=40` | Parameters verified from docs.gitlab.com/api/projects (`membership`: projects the user is a member of; `min_access_level`: integer 10 to 50). **Token scope and whether a project access token may call it: unverified** (the page does not say). |
| Bitbucket Cloud: braces of `{uuid}` in paths | **Unverified** (the fetched docs page held no text on it). |
| Bitbucket repository access token: `/user`, list repositories | **Unverified.** |
| Bitbucket Data Center `/repos`, `/webhooks` paths and fields | **Unverified** (the fetched page names no paths). |
| Version floor for signed webhooks (Data Center) | **Unverified** (the page says secrets exist in the 10.x docs, no first version). |

No live host was called. The Bitbucket items stay open for the Bitbucket ticket (outcome: see "LAUN-84" at the end; all five are still UNVERIFIED, now recorded with the code's assumption).

**Owner (LAUN-86 review item 8).** A fact with no owner is never checked. LAUN-84 (the Bitbucket
provider) owns all five: its AC checks and records the #15 verify-at-build facts.

| Fact | Owner |
|---|---|
| GitLab project-token scope for `ListRepos` (`available-repos`) | LAUN-84 |
| Bitbucket Cloud brace encoding of `{uuid}` in paths | LAUN-84 |
| Bitbucket repository access token: `/user`, list repositories | LAUN-84 |
| Data Center `/repos`, `/webhooks` paths and fields | LAUN-84 |
| Data Center version floor for signed webhooks | LAUN-84 |

### For the UI (LAUN-7 review notes)

Handle an empty `login` in the `check` answer (Bitbucket) and short pages from GitHub
`ListRepos` (7b). The list shows `status` and `attentionCode`; `webhookUrl` is a path when
`PUBLIC_BASE_URL` is empty.

## LAUN-86: secret rotation and the repo map (built)

No migration. Routes: `POST/DELETE /integrations/{id}/rotate-secret`, `POST
.../rotate-secret/commit`, `GET .../available-repos`, `GET/POST .../repos`, `PATCH/DELETE
.../repos/{repo}`. The legacy `/integrations/gitlab/{id}/projects` routes, their queries and
the `GitlabProject*` schemas are removed; the GitLab webhook is unchanged.

Judgment calls where #11 left room:

- **The pending secret is visible to the verify loops at once.** GitHub's HMAC loop already
  opened `webhook_secret_next_enc`. The GitLab hash compare now reads
  `webhook_secret_next_hash` too (`ListGitlabSecretHashes`), still with no early exit.
  `git_integrations.updated_at` is set by every rotate, commit and discard, and
  `ForgetGitSecrets` runs after each.
- **Token mode, hooks first.** A NEW pending secret first marks every mapped repo
  `hook_state=failed`, `hook_error=not_updated` (no hook holds it yet), in the same
  transaction that stores the secret. The push loop sets a repo `active` when its hook took
  the secret. A retry reuses the pending secret and pushes only to repos that are not
  `active`; the 30 s route budget therefore converges over retries. The loop stops at the
  first 401, 403 or 429 (every further call would fail the same way). The commit runs in its
  own transaction under the row lock, as a compare and swap on the pending hash, and
  re-checks under the lock that every repo is `active` (a repo mapped during the push cannot
  slip through). The answer is `200` with `committed` and `failedRepos`; a failed hook is not
  an HTTP error.
- **A repo mapped while a rotation is pending** gets the pending secret in its hook. The map
  insert refuses (409, "changed while this ran") if `updated_at` moved since the secret was
  read, so a hook with a stale secret never becomes a row. A hook made before such a refusal
  stays on the host and is adopted by the retry (list and match by URL); it is never deleted
  on a failed insert, because a concurrent map of the same repo shares it.
- **Discard in token mode** marks the hooks that had moved (`active`) as `failed`,
  `hook_error=secret_discarded`. A repo marked `not_updated` that has a hook becomes
  `failed` / `secret_discarded` too (LAUN-92) until the next rotate or refresh; one with no hook
  becomes `none`. Discard is
  idempotent (`200` with no pending secret).
- **A pasted secret on rotate** is accepted for a GitLab webhook-only integration (the old
  PATCH `secret` is gone, and the duplicate check needs a way to be reached). It is never
  returned. A secret equal to the integration's own current one is `409 conflict`.
- **Commit with no key.** A GitLab row whose pending blob cannot be opened (or was made with
  no key) swaps by hash alone and sets `webhook_secret_enc` NULL. GitHub is `409
  encryption_unavailable`.
- **Hash-only GitLab row in the rotate flow** (review item 7): such a row (from before
  migration A, or a pasted secret with no key) has no plaintext, so token mode and a token-mode
  repo map are `409 conflict` for it. The first rotate stores a sealed pending secret and the
  commit seals it as the current one, after which token mode works. Without a key the same
  rotate works by hash alone and leaves the row hash-only. `deploy.md` says so.
- **Repo map.** Token mode takes `repoId` only (a `repoKey` or `repoPath` in the body is 400);
  the server reads the repo from the host, checks the key is numeric (`^[0-9]{1,18}$`, as for
  webhook-only) and the path, pre-checks "already mapped" (409), makes the hook, then inserts
  under a SHARE lock on the integration row. A second map is `409 conflict` from the unique key
  too. `teamId` is a team id or key. Unmap: the hook is deleted by the stored hook id; a host
  404 is `hookRemoved: true`; webhook-only, an unreadable token or a host failure is `false`.
  Unmap takes the same allowance, just before its host call (a webhook-only unmap makes none).
- **Rate limit** (6 a minute, burst 3, per org and per user): `rotate-secret` (every mode),
  `available-repos`, a repo map in token mode (the body has `repoId`) and an unmap's hook delete.
- **`available-repos`** asks the clients for 50 a page (it was 100), so a page is never cut
  short of the provider's cursor; rows the server would not store (a non-numeric key, a bad
  path) are dropped.
- **Attention codes** (#11a): a failed hook call sets `token_rejected` (401/403),
  `attention_unreachable` (cannot connect) or `hook_failed` (anything else); a 429 sets
  nothing. A committed rotation clears the code; so does a good `check`.
- **No event** is emitted by any of these routes (#52).
- **Races and the manual commit (review round 2).** After the push loop the pending hash is read
  again: if a discard (or a newer rotation) took it, the repos pushed in this run are marked
  `failed`/`secret_discarded` and nothing is committed. A manual `commit` in token mode needs
  every repo `active` (`409 conflict` otherwise, naming the repos); `{"force": true}` skips that
  check and accepts that the hooks that did not move are refused after the swap. Webhook-only
  commit is unchanged. Discard changes only repos marked `active` or `not_updated` (see item 2 of the
  LAUN-92 hardening note); a repo that failed for another reason stays failed. `availableRepos[].url` is blanked unless it is http or https.
- **Hardening (LAUN-92, no API change).**
  1. *State writes outlive the request.* The writes after a host push (each repo's hook state,
     the re-check of the pending hash, `secret_discarded` marks, the attention code, the commit)
     run on `context.WithoutCancel(ctx)` with a 15 s timeout. A client that disconnects during
     the push no longer leaves repos `not_updated` while the host holds the new secret. The push
     loop itself still follows the request context.
  2. *Settle never trusts `not_updated`.* After a crash between a host write and the DB write the
     host may hold the dropped secret. `SettleGitHooksAfterDiscard` now turns a `not_updated` row
     that has a hook id into `failed` / `secret_discarded` (one extra retry), not `active`. A
     `not_updated` row with no hook id is still `none`.
  3. *One push at a time per integration.* A token-mode rotate takes a transaction-level advisory
     lock (`launchpad:git-push:<integration id>`) on a held connection before it reads the secret
     and keeps it until the commit and the answer are done. A second rotate of the same
     integration waits (up to the 30 s route budget, then `409 conflict`), so a late write of an
     older run cannot reach the host after a newer one. Discard and commit do not take this lock;
     the re-check after the loop covers them.
  4. *`committed` after a force commit.* If a force commit took this run's pending secret during
     the push, the run's own commit finds no pending secret but a current hash equal to its own.
     That is a swap of this run's secret, so the answer says `committed: true`.

## Build notes: ticket 7, the import job runner and routes (LAUN-8)

What the build chose where the note left room. Migration C is `0013_import_jobs`.

- **One MR, not split.** The runner, the routes, the schema and the compose change
  depend on each other; the tests of the state machine need all of them.
- **No `workspace_name` column.** The Linear export holds no workspace name.
- **`GET /import/sessions/current` is 404 with no open session.** `POST
  /import/sessions` is 201 for a new session and 200 for the open one.
- **`PUT /usermap` always moves the session to `user_map`** (from `exported`,
  `user_map`, `dry_run_ok`, `imported`, `verified`, `failed`) and recomputes
  `user_map_hash`; a dry run on the old map is then not current.
- **A real run is allowed from `exported`, `user_map`, `dry_run_ok`, `failed`,
  `imported` and `verified`**, and the dry-run lookup (same export hash and user map
  hash, succeeded) decides. This gives `dry_run_required` in every state, not a
  `conflict` in some.
- **Counter floors.** The plan has no counter floors (the CLI's `ORB=3405` is
  specific to one workspace).
- **Importer additions.** `Options.Progress`, `Report.Committed` (set after
  `tx.Commit`), a context check between steps and between files, `ReadExportRoot`
  (an `os.Root`), `linear.ErrTotalLimit`, directories 0700.
- **Importer errors are logged scrubbed and never stored.** Row text is a fixed text
  per error code (emails and titles can be in the importer's errors).
- **The key is one `string` copy** for the export job (a Go string cannot be wiped);
  the `[]byte` buffers are cleared. Accepted in #38.
- **Shutdown** (`Manager.Close`) marks running jobs `failed` / `interrupted` and
  frees the index. A job that commits after the sweep marked it lost is not
  recorded (WARN in the log).
- **Cycles guard race.** A session start and `cyclesEnabled: true` at the same instant
  can both pass; the importer's own refusal of a used cycle number is the net.
- **Measured (record-only, a 3-issue fixture, `IMPORT_MEASURE=1`):** the import holds
  1 pool connection; an unrelated write during the transaction took under 1 ms; the
  hourly cycle job waits for the import on the team lock
  (`devops:team-cycles:<team>`, the importer takes it). NOT measured: a large import
  (thousands of issues), so the pool use and the stall of the cycle job for the
  real workspace are open.
- **Work volume (measured on Docker Desktop for Mac, not on Linux):** a container
  with `--read-only --cap-drop ALL --security-opt no-new-privileges --user 10001`
  writes to a named volume that a `--cap-drop ALL --cap-add CHOWN` root container
  chowned to 10001; a second chown run succeeds. `volume-init` does not chmod
  (it could not on a re-run); the server sets 0700 itself.

## LAUN-87: the deliveries ring (built, 7c)

Migration 0014 `git_webhook_deliveries`; `tracker.RecordGitDelivery`; ring writes in
`gitlabWebhook` and `githubWebhook` (`internal/api/git_deliveries.go`); `GET
/integrations/{id}/deliveries`; `lastDelivery` on every integration answer
(`{receivedAt, result, httpStatus, errorCode}` or null). Judgment calls:

- **Result mapping.** The answer's own status word (`processed`, `ignored`,
  `duplicate`, `unmapped`) for a 2xx; `bad_signature` for a 401; `bad_request` for a
  400 or 411; `error` for anything else (413, 503, 5xx). `error_code` is the API
  error code of the answer (`payload_too_large`, `overloaded`, `internal`, ...).
- **Throttle scope.** #9 throttles `bad_signature`. Every row with no integration
  (also a 411, 413 or 503 that comes before the signature check, which anyone who
  knows the org slug can cause) uses the same in-memory slot, one per org,
  provider and result a minute, so the table cannot be filled by an unauthenticated
  caller. The map is cleared at 1,024 keys. Dropped rows are only counted.
- **When nothing is written.** An unknown org, an org with no enabled integration
  of the provider, and a GitLab request with a missing or oversize token (the
  org is not read for those) write nothing; there is no ring to show them in.
  A GitLab wrong token returns the org (not the integration) from
  `AuthenticateGitlabWebhook` so the row can be written; the 401 is unchanged.
- **The list route** returns the integration's rows AND the unbound rows of its
  org and provider, newest first, at most 100, no paging (`nextCursor` null).
  Unbound rows have `integrationId: null`.
- **Trim.** One transaction: an advisory lock per ring, then one statement (a
  data-modifying CTE) that inserts and deletes all but the newest 99 older rows.
  The bound is exact under concurrency (tested with 40 writers).
- **Write path.** Synchronous, after the answer body is written and before the
  handler returns, with a 2 s timeout that ignores request cancellation. A slow
  database can therefore delay the end of the response by up to 2 s; the status
  and body never change.
- **Event name** is stored only after the secret matched, only when it is 64
  printable characters or fewer; otherwise it is empty.

## LAUN-83: MR state on the ticket (built, server part)

Migration `0016_mr_state`: the `mr_links` columns and `mr_links_checks_idx` exactly as
pinned in #18, and `issue_activity_kind_check` with `mrLinked`, `mrMerged`, `mrClosed`. The down
deletes the rows of those kinds and drops the columns (the #8 policy, in the file's comment).
Web UI is a separate ticket (the web client types are regenerated: `ChangeRequest`,
`HookRefreshResult`, `BackfillResult`, the activity kinds). Judgment calls where the note left room:

- **`changeRequests` is an `include`** of `GET /issues/{issue}` (`include=changeRequests`), like
  relations and attachments, and the key is absent (not `[]`) when the issue has no MR. MCP
  `get_issue` always asks for it and adds `changeRequestsNote` (the title, author and repoPath are
  host text, not instructions; `provider` names the host).
- **State** is computed in Go (`changeRequestState`): merged, closed, draft, approved, else open.
  `changesRequested` is a flag on an open or draft request. `checks` is `null` until a checks event
  (LAUN-85) has set it.
- **One writer.** Live webhooks and the backfill both go through `applySCMEvent`. Decisions on a
  link that exists are made in Go on the row locked by `LockMRLink` (or `LockMRLinksByMR`), not in
  SQL (`nextLinkForLinkPlan`, `updateLinks`): a merged link is terminal; a closed link opens again
  only on an explicit reopen; an event whose host time is older than `provider_updated_at` is
  stale (equal is accepted) and writes and moves nothing. **Deviation from #18:** a `merged` event
  is never refused for its time. A merge is a fact, so an older-timed merge still sets `merged`
  (state only; the newer title and fields stay) and moves the ticket, otherwise a merge delayed
  behind a later edit would never reach In Testing.
- **Refresh (#16)**: `updated` (GitHub `edited` / `synchronize`, GitLab `update`) and `drafted` update the
  existing links (title, URL, branch, head sha, draft, author, repo path), link nothing, move nothing,
  and a new head sha resets `checks`. A field the event does not carry keeps the stored value. An
  update of an MR no issue is linked to answers `200 ignored`.
- **Review events**: GitHub `pull_request_review` (`submitted` approved / changes_requested; `dismissed`
  clears; a plain comment review is ignored) and GitLab `approved` / `unapproved` (inside the merge
  request events; no new hook flag). `scm.ActionChangesRequested` is new. A review never moves a ticket.
  The GitLab author comes from the `open` event's user only.
- **HookSpec** gained `Reviews bool`; every hook write (map, rotation push, refresh) sets it. GitHub
  adds `pull_request_review`. `hook/refresh` is `409 conflict` while a secret rotation is pending
  (the rotation owns the hooks), without a token, or for a hash-only GitLab row; a failed host call
  marks the repo `failed` and answers the `provider_*` error. It does not clear `attention_code`.
- **Timeline rows** (`mrLinked` for a new link, `mrMerged`, `mrClosed` on a state change; a reopen
  has no kind) with `to = {provider, repoPath, number, url, title}`, source = the provider, no actor.
- **Events (#52)**: one `issue.updated` per affected issue when a link, its state or review
  changes (`changes: ["changeRequests"]`, `source`, `mergeRequest`), merged into the move event
  when the issue also moved. Emit is last. A stale event emits nothing.
- **Backfill** runs each change as its own transaction through the live writer with action
  `opened` (a draft links without a move; a fork change is read and skipped). Answer
  `{seen, linked, moved, truncated}`; `seen` caps at 200; `truncated` is also set when the 30 s
  ran out. **Not done (done in LAUN-96, below):** the host's review state of a change is not read (the clients return `none`), so
  a backfilled MR shows approved only after its next review or update event. GitLab approval events
  and `pull_request_review` field shapes are from the vendors' documented shape; no live host was called.
- Open for LAUN-85 (checks): `checks_updated_at`, `checks_detail` exist and are reset with a new head sha.

## LAUN-85: MR checks from provider events (built, GitHub and GitLab)

No migration: migration 0016 already holds `checks`, `checks_updated_at`, `checks_detail` and
`mr_links_checks_idx`. **Bitbucket is not built here.** Its provider (webhook route, parser) is
LAUN-84, which is not merged; LAUN-84 must add the commit status events (`repo:commit_status_created`,
`repo:commit_status_updated`; Data Center's build status event), map them to the neutral
`scm.Event` of `KindChecks` (the build key is the check name), set the delivery key
`bb:<delivery>:r<repo>:sha<12>:checks` and add the events to its `HookSpec` handling (`HookSpec.Checks`
exists, the Bitbucket client ignores it today). `HandleSCMEvent` needs no change for it, except that
a Bitbucket repo key is not numeric (the same open change as for its MR events).

- **The neutral event.** `scm.Event` gets `CheckName`. `Kind = checks`, `Action = checks`, `Checks` is
  `pending | success | failure`, `HeadSHA` is the commit, `Number` is 0. `Plan.Checks` is new and is
  not `Noop`. A state the tracker does not know leaves `Action` empty: `200 ignored`.
- **GitHub.** `check_suite` (name = app slug, else app name): a suite not `completed` is pending;
  completed: `success`, `neutral`, `skipped` are success; `failure`, `timed_out`, `cancelled`,
  `action_required`, `startup_failure` are failure; `stale` is pending. Host time = `check_suite.updated_at`.
  `status` (name = `context`): `pending`, `success`, `failure`, `error` (failure). Host time =
  `updated_at`, else `created_at`. Both need `repository.id` and the sha, else `400`.
- **GitLab.** `Pipeline Hook` (`X-Gitlab-Event`), name = `object_attributes.name`, else `pipeline`.
  `success`, `skipped` are success; `failed`, `canceled` are failure; `created`,
  `waiting_for_resource`, `preparing`, `pending`, `running`, `scheduled`, `manual` are pending. The payload
  has no `updated_at`: host time = `finished_at` if set, else `created_at`. Limit: a merged-results
  pipeline runs on a merge commit, not on the MR head sha, so it matches no link.
- **Order of work (`HandleSCMEvent`).** The token / signature checks come first, as for every event.
  For a check event the transaction then: (1) looks up the repo map (unmapped: `202`, nothing kept);
  (2) locks the links of `(org, integration, repo, head sha)` (`LockMRLinksByHeadSHA`, `FOR UPDATE`);
  with none the answer is `200 ignored` and **no `git_webhook_events` row and no event is written** (a busy
  repo sends many check events for commits no ticket names; the AC says "keeps nothing"). The handler still
  writes its `git_webhook_deliveries` ring row (LAUN-87), so a busy CI repo can fill the newest-100 ring with
  `ignored` rows; skipping the ring write for this path is a possible follow-up; (3) inserts the
  delivery keys (a repeat is `duplicate`); (4) merges the result into each link; (5) `events.Emit` last.
- **Aggregation** is #19: `mergeCheck` drops a result whose time is older than the stored one for that
  name (equal replaces), evicts the entry with the oldest time when a 51st name comes, and `checks` is the
  worst entry. A result with no usable host time takes the server time. `checks_updated_at` is the newest
  time in the map. A check event never touches `provider_updated_at` (and an MR event never touches the
  check map, except a new head sha, which clears it). A merged or closed link still takes check results.
- **Event (#52).** One `issue.updated` (`changes: ["changeRequests"]`, `source`, `mergeRequest`) per
  affected issue, only when the link's aggregate `checks` changed. A stale or unchanged result emits
  nothing. No timeline row, no move, no notification kind.
- **Delivery keys.** GitHub `gh:<delivery>:r<repo id>:sha<first 12 of the sha>:checks` (as the ticket
  says). GitLab keeps its own form: `gl:ik:<key>` and `gl:ev:<uuid>` plus `:p<project id>:sha<12>:checks`
  (`p`, not `r`, so GitLab has one form of key; the ticket's `r` is the GitHub and Bitbucket spelling).
  The sha part is cut to printable ASCII without `:`.
- **Hooks (#24).** `HookSpec.Checks` is set by every hook write (map, rotation push, refresh). GitHub's
  event set is `pull_request`, `pull_request_review`, `check_suite`, `status`; GitLab sends
  `pipeline_events: true`. Operator note: press `hook/refresh` once per mapped repo (`docs/deploy.md`).
- **Not solved, said plainly.** A check event can arrive before the `synchronize` / `update` event that
  gives the link its new head sha; it then matches no link and is lost (the next event of that check
  restores it). Several pipelines of one sha that share the name `pipeline` overwrite each other (the later
  one wins). No live host was called: all payload shapes are from the vendors' documentation.
- **Web.** The checks badge is LAUN-95. `ChangeRequest.checks` (`null | pending | success | failure`) is
  the data it reads; its OpenAPI description now says what it holds.

## LAUN-84: Bitbucket provider, Cloud and Data Center (built, no migration)

No migration: `repo_key` is `text`; the numeric rule was in code and OpenAPI only. No live Bitbucket
host was called. Every payload shape below is from the vendors' documentation; fixtures
(`internal/bitbucket/testdata`) are hand-written with invented data.

- **Route.** `POST /api/v1/orgs/{org}/integrations/bitbucket/webhook`, public. It uses the same
  signed-body handler as GitHub (`signedBodyWebhook` in `internal/api/github.go`; #14 order: org and
  integrations lookup, header shape, `411`, `413`, byte budget, 5 s deadline, HMAC over each enabled
  integration's current and pending secret, then the event and delivery headers and the payload). Only
  the header names and the event list differ (`internal/api/bitbucket.go`). GitHub behaviour is unchanged.
- **Headers.** Both flavours: `X-Hub-Signature: sha256=<hex>`, `X-Event-Key`. Delivery id: Cloud
  `X-Request-UUID`, Data Center `X-Request-Id` (1 to 100 printable ASCII). A missing id is `400`.
  `diagnostics:ping` is `200 ignored`.
- **Events.** Cloud: `pullrequest:created|updated|fulfilled|rejected`, `pullrequest:approved|unapproved`,
  `repo:commit_status_created|updated`. Data Center: `pr:opened|modified|merged|declined|from_ref_updated`,
  `pr:reviewer:approved|unapproved|needs_work`, `repo:build_status`. Every other event is `200 ignored`,
  also Cloud `pullrequest:changes_request_created|removed` and Data Center `pr:deleted` (`Handles()`
  does not list them).
- **Repo key.** Data Center: the numeric repository id. Cloud: `{workspace-uuid}/{repo-uuid}`, braces
  kept, lower case (`scm.CanonicalRepoKey`). Used by the webhook, the repo map, backfill and unmap. The
  OpenAPI `repoKey` pattern accepts both. Data Center client calls need `KEY/slug#id` (the rename-safe id
  is in the `#`); `tracker.gitRepoRef` builds it from the stored label.
- **Delivery key.** `bb:<delivery>:r<repo key, ":" and "/" as "_">:pr<n>:<action>`; checks:
  `...:sha<12>:checks`.
- **Forks.** Cloud: source repository uuid differs from the destination repository, or is null (deleted
  fork): `200 ignored`. Data Center: `fromRef.repository.id` differs from `toRef`. Missing `draft`: not a draft.
- **Checks.** `SUCCESSFUL` success; `FAILED`, `STOPPED`, `CANCELLED` failure; `INPROGRESS` pending.
  Check name = status `key` (else `name`). Host time = `updated_on` (Cloud) or `dateAdded` (Data Center, ms).
- **Hooks.** `HookSpec.Checks` adds the commit status events (Cloud) or `repo:build_status` (Data
  Center). Data Center answering `400` to a hook with the build status event: `EnsureHook` retries once
  without it (the check badge is then absent for that repo). Cloud never retries.
- **Web.** `bitbucket` is a provider in the connect dialog and repo map (token page link hidden because
  the token page differs by flavour). The dialog says a Data Center without webhook secrets cannot be used.

### The five owned facts (outcome)

| Fact | Outcome |
|---|---|
| GitLab project-token scope for `ListRepos` | **UNVERIFIED.** Offline; the docs page is silent. Left as built (no change to the GitLab client); a `403` from the host is `provider_forbidden`. |
| Cloud braces of `{uuid}` in paths | **UNVERIFIED.** The client sends the braces percent-encoded (`%7B...%7D`) in paths; whether Cloud wants raw braces instead is not tested (`cloud.go` says so). |
| Bitbucket repository access token: `/user`, list repositories | **UNVERIFIED.** A repository token is expected to answer `/user` with `403`; `check` accepts that and leaves `login` empty (the LAUN-7 rule). Listing repositories with it is not expected to work: map by key (webhook-only or typed). |
| Data Center `/repos`, `/webhooks` paths and fields | **UNVERIFIED.** Built to `/rest/api/1.0/projects/{KEY}/repos/{slug}/webhooks`, `/rest/api/1.0/repos`, fields `id`, `slug`, `project.key`. |
| Data Center version floor for signed webhooks | **UNVERIFIED.** No first version found. The web and `docs/deploy.md` say: a Data Center that cannot sign webhooks cannot be used. |

### UNVERIFIED payload and header facts

1. Data Center build status event name `repo:build_status` and its shape (`buildStatus`, `repository.id`,
   `commit` as string or object). The shape is the least sure item; the retry on `400` protects hook setup.
2. Cloud workspace uuid is read from `repository.workspace.uuid`, else `repository.owner.uuid`.
3. Whether Cloud reuses `X-Request-UUID` on a retry (the key assumes a retry keeps it; if not, a retried
   delivery is a new key and applies again; moves are forward-only so this is safe).
4. `X-Hub-Signature` when the hook has no secret: assumed absent; then the request is `401`.
5. Data Center `updatedDate` as milliseconds; Cloud `updated_on` as RFC 3339.
6. A Data Center `pr:modified` that retargets the branch; it is treated as a plain update.
7. Cloud `commit_status` carries `commit.hash`; `links.commit.href` is the fallback.
8. Cloud: the name of the draft field (`pullrequest.draft`); a missing field means not a draft.
9. Data Center: `X-Request-Id` as the delivery header.
10. `diagnostics:ping` is signed when the hook has a secret (the handler checks the signature first).
11. Data Center date format of PR `updatedDate` (ms) and `dateAdded` (ms).
12. Data Center: `fromRef.latestCommit` as the head sha.

## LAUN-96: lock order, backfill review, review removals, backfill deadline (built, no migration)

From the LAUN-83 reviews.

- **One lock order for every writer of `mr_links`: team rank locks, then issue rows (id order), then link
  rows.** The rule: a transaction that touches a link row has first locked the issue row(s) of the links it
  will touch, in issue-id order, in ONE pass, AFTER the rank lock of every team those issues belong to
  (team id order); link-id order is never relied on (link ids run in creation order, issue ids in another
  order). The rank lock matters because a PATCH that places or moves an issue holds the team's rank lock,
  one issue row, and then locks every other row of the column in id order (a rebalance): an event holding
  issue B and waiting for A would cross it. `lockLinkedIssues` does the rank locks and the issue locks
  for `updateLinks` and the checks path: it reads the teams (plain read), takes their rank locks in team
  id order, repeats the read until no new team appears, locks the issues (the statement returns each
  row's team; a row of a team not held is an error, though no route moves an issue to another team today),
  and the link query then takes only the locked issues' links (`issue_id = ANY(ids)`): a link made in the
  last gap is left alone, its writer committed with its own state. Path by path:
  - `linkAndMove` (opened, reopened, ready, merged; also the backfill): rank locks, then `LockIssue` per
    named issue in id order, then that issue's link (`LockMRLink`) and its inserts. Conforms.
  - `updateLinks` (closed, update, review): was link rows first, then the issue row (timeline row of a
    close: key-share lock). Now the rank locks of the linked issues' teams, then `LockMRLinkIssues` (one
    statement, `ORDER BY id FOR UPDATE`), then `LockMRLinksByMR`.
  - Checks (`HandleSCMEvent`, `plan.Checks`): was `LockMRLinksByHeadSHA` alone, link-id order, no issue
    lock. On an MR naming two issues whose link ids run against their issue ids this crossed
    `linkAndMove` (it holds issue A and link La and wants issue B and link Lb; the check held Lb and wanted
    La). Now the rank locks, `LockMRLinkIssuesByHeadSHA`, then `LockMRLinksByHeadSHA`. The smaller change was one
    more query, not a pre-pass in `linkAndMove`.
  - Backfill: `applyStandalone` is `applySCMEvent`, so it is `linkAndMove` / `updateLinks` above, one
    transaction per change.
  - Hook refresh: calls the host and writes `git_repos` hook columns; it touches no link row or issue row.
  - Cycle job: locks a per-team advisory lock and updates issue rows; it never touches `mr_links`, so no
    cycle through a link row exists. Issue-vs-issue order there is not changed here.
  - Issue edits and issue delete: edits take the rank lock and `LockIssue` and no link row; a delete
    cascades to its links after the issue row is locked (issue first). Conform.
  - Any path that writes only link columns (the checks write) already holds the issue rows by then.
  A link made by another transaction between the issue statement and the link statement is already
  committed, so its writer let go of its issue. The issue locks also make a concurrent link of the same
  issue wait for the event. Tests: `TestMRState_CloseRacingAnIssueFirstWriterDoesNotDeadlock` (holds the
  issue row, then asks for the link row while a close waits), `TestMRState_RankLockBeforeIssueLocksOnEventPaths`
  (a PATCH-rebalance holding the rank lock and issue A, then asking for B, with an update and a check event
  in flight; before the rank locks: `deadlock detected`, a 500),
  `TestChecks_LockOrderWithLinkAndMoveOnTwoIssues` (two issues on one MR, link ids inverted, a check event in
  flight; before the change `deadlock detected`, a 500) and `TestMRState_ConcurrentCloseAndReopenAllAnswer200`
  (40 events at once). Cost: every update, review, close and checks event of a linked MR now holds the
  linked issue rows for its transaction.
- **The backfill reads the host's review state, also for a link that exists.** `scm.Event.ReviewRead`
  (set only by the backfill) makes `withEventFields` write `Review`; a live opened / ready / merged event
  has `ReviewRead` false, so its always-none review never wipes a review (`TestMRState_LiveChangeEventKeepsTheReview`).
  The source per provider: Bitbucket Cloud (`participants`) and Data Center (`reviewers`) carry it in the
  list answer (`Change.ReviewKnown`); GitHub (`ReadReview`: `GET /pulls/{n}/reviews`, each reviewer's LATEST
  approval or request for changes, a dismissal removes that reviewer, a comment changes nothing, any
  standing request for changes wins; more than 5 pages is an error) and GitLab (`ReadReview`:
  `GET /merge_requests/{iid}/approvals`, approved when the answer's `approved` flag is true (what the `approved` webhook means: all required approvers; `approved_by` not empty only when the flag is absent); GitLab's request-changes
  state is not in that answer) cost one more call per change, through the optional `scm.ReviewReader`.
  A review read that fails keeps the stored review; only the deadline stops the run. This is an extra
  API call per change, inside the same 30 s and 200-change limits. The host's review is applied under the
  same stale rule as any event (an event older than `provider_updated_at` writes nothing). Unverified: no
  live host was called; the Bitbucket field shapes are from the vendors' documentation.
- **Review removals: decided, not tracked per reviewer.** The link stores one `review` value, not one per
  reviewer (a per-reviewer table would be a migration). A GitHub `dismissed` review and a Bitbucket
  `unapproved` set the review to none. This can understate (another reviewer's request for changes, or
  another approver, still stands). It errs towards "open", never towards a false "approved" from a removal.
  The next review event of that MR or a backfill (which now reads the host) puts the truth back. GitLab:
  per its webhook documentation `approved` / `unapproved` are the WHOLE merge request becoming / ceasing to
  be fully approved, and `approval` / `unapproval` are ONE person's action. We map the first pair (as
  before) and keep the second pair ignored, so one person's removal cannot clear another's approval.
  GitHub `ReadReview`: a `DISMISSED` review deletes that reviewer's whole entry, even if they had an
  earlier approval; GitHub marks the dismissed review in place, so this is conservative.
- **A backfill deadline during a database write is `truncated`, not a 500.** When the 30 s run out
  while a change is applied (or while its review is read), `BackfillGitRepo` answers 200 with the counts
  of the changes already committed and `truncated: true`. The interrupted change rolled back (its own
  transaction); a repeat is safe. `Service.SetBackfillTimeout` is the test seam
  (`TestMRState_BackfillDeadlineDuringAWriteIsTruncated` holds an issue row so the write waits).
- **Limits kept on purpose.** The backfill has no resume cursor: every run starts at the newest change.
  Worst case on GitHub is 200 changes x up to 5 review calls inside the 30 s, so a big repo can answer
  `truncated` on every run and never reach its oldest changes. A failed review read is silent (the
  stored review stays; nothing is logged or counted in the answer, because the tracker service has no
  logger and the answer shape is fixed by the API).

## LAUN-104: real reasons for a 409, and removing a connection (built)

An admin could not save a token or add a connection: both answered the generic 409 and a reload could not fix it. A stale
hash-only connection also blocked a fresh one for the same host, and there was no way to delete it.

1. **Distinct 409 codes** (`httpx.Codes`, openapi `ErrorCode`, kept equal by a test): `integration_exists` (unique
   provider + base URL, on create and on a URL change), `secret_in_use` (another enabled integration of the provider
   has the secret), `secret_hash_only` (token mode needs a sealed secret; rotate first), `repos_mapped` (the URL cannot
   change, or the integration cannot be removed, while a repository is mapped) and `integration_changed` (the row moved
   while the PATCH ran; a retry works). The messages are unchanged. The web maps by `code` to its own fixed sentences;
   server words never reach the screen, and no sentence says "reload" for a conflict a reload cannot fix.
2. **`DELETE /orgs/{org}/integrations/{integration}`** (admin scope, 204, no `Idempotency-Key` handling: a repeat is a
   plain 404). In one transaction: the provider advisory lock, `FOR UPDATE` on the row (a repo-map insert holds a SHARE
   lock, so none slips in), a re-count of mapped repos, then the delete. Afterwards `ForgetGitSecrets(id)`.
3. **Mapped repos: refuse (`409 repos_mapped`), do not cascade.** Unmapping in token mode deletes the hook on the host
   with the token; a silent cascade would leave live hooks on the host that keep sending signed requests to a row that
   no longer exists, and would need a host call inside a delete. The admin unmaps first (the same rule as a URL change).
   No safer design found: the delete route makes no host call at all. Known race: a token-mode map creates the hook on the host without a DB lock, so a concurrent delete can make the mapper get 404 and leave an orphan hook on the host.
4. **FK analysis.** Every table that points at `git_integrations` does it by the composite `(org_id, integration_id)`
   FK with `ON DELETE CASCADE`: `git_repo_map` (empty by now), `git_webhook_events`, `git_webhook_deliveries` (the ring)
   and `mr_links` (scoped by integration, decisions-T5 / migration 0004). The delete matches `org_id AND id`, so only
   this integration's rows go; another integration of the same org, or of another org with the same host, is untouched
   (tested). Rows with a NULL `integration_id` in the ring (bad signature) belong to no integration and stay. Cost to know:
   the removed integration's `mr_links` go, so the Changes block of the tickets it linked disappears; the link
   attachments and the `issue_state_changes` rows stay (neither references the integration). The web confirm says so.
5. **No `events.Emit`.** Git integration config is an explicit exception (item 52 above, AGENTS.md "Conventions"), as
   create, PATCH and rotate are. The route logs and returns no secret; the sealed columns go with the row.
6. **Web.** "Remove connection" row on the connection page (danger button, confirm dialog that names the connection and
   says that mapped repositories must be unmapped first; 40px via `touchClass`). The replace-token dialog, on
   `secret_hash_only`, shows "Rotate the webhook secret first", which closes it and starts the rotation (webhook only:
   the new secret is shown once, the admin updates the host hook and commits, then adds the token). The connect dialog,
   on `integration_exists`, shows "Open the existing connection" (matched by provider and host, else the list).
7. **Not proven.** jsdom tests only; no browser run, no real host.

## LAUN-103: token help in the connect dialog (web text only, no server change)

No host was called. Every vendor fact below is **from documentation, unverified**; the web text says "usually"
and "can be wrong" and does not link to an outside address (links are the connection's own base URL plus a path).

1. **GitLab.** Two token forms. (a) Classic (legacy) personal token or a project access token: scope `api`, role
   Maintainer. This is the form `decisions #23` was built on. (b) The newer fine-grained form ("Add resource
   permissions": Resource access Group and project / User / Global, then resources such as Integrations, Projects,
   Project Planning). Launchpad needs: read projects and merge requests, read / create / update / delete project
   hooks, list the user's projects (available-repos). UNVERIFIED: which fine-grained permission names give exactly
   that (the web text says "read for Projects and Merge requests, read and write for project hooks (named Webhooks
   or Integrations)"), and whether the project list works with "Group and project" access only. Names change by
   GitLab version, so the dialog tells the person to make a classic token when they cannot find them.
2. **GitHub.** Fine-grained: `Metadata: read`, `Webhooks: read and write`, and for backfill `Pull requests: read`;
   classic: `repo` and `admin:repo_hook`. Unchanged from #23. UNVERIFIED: the exact label of the Commit statuses
   and Checks read permissions that check events use (not in the text).
3. **Bitbucket.** Cloud: repository access token with Webhooks, Repositories and Pull requests; Data Center: HTTP
   access token with Repository admin. UNVERIFIED: the current token types and permission names. The text says
   they change and that it can be wrong. No token page link (no one page for all installs).
4. **403 message (`provider_forbidden`).** No server change: the code already comes from `CheckToken`, hook calls
   and the repository list. The web chooses fixed words from the call (`GitContext`: `hook` and `repos` = hook write,
   `list` = repository list, `backfill`, `check` / `connect` / `token` = identity check) and the provider
   (`ProviderInfo.needs`). It names what the token "usually needs". The host's words never reach the screen
   (#51). Without a provider the old general sentence stays. The server cannot tell which permission was missing,
   so the message is a guide by call, not a diagnosis.
5. **Manual map form** (webhook only) shows a hint that a connection with an access token picks repositories from a
   list; the Add a token button is on the same page.
6. **Replace and Add a token dialog** now show the same permission lines, the note and the link to the host's
   token page (built from `integration.baseUrl`, only for an `https://` address) as the connect dialog.

## LAUN-97: checks hardening (built, no migration)

From the LAUN-85 reviews. Changes below; items 4 and 7 need no code.

1. **No ring row for a check event with no link.** `HandleSCMEvent` sets `WebhookResult.NoLink` (not
   in the JSON answer) when `LockMRLinksByHeadSHA` finds no link; `gitDelivery.reply` then does not write the
   ring row (GitHub, GitLab and Bitbucket share it). The answer is unchanged (200 `ignored`). A stale check
   result on a link, an unknown pipeline state and an unmapped repo still write their row. Tests:
   `TestChecks_NoLinkWritesNoRingRow`, `TestChecks_GitHubNoLinkWritesNoRingRow`.
2. **GitLab `canceling` is pending** (a cancel under way; the final `canceled` is failure). `manual` stays
   pending: a pipeline that waits on a manual job shows pending until someone runs it or it ends. `skipped`
   stays success (nothing ran). Test: `TestPipeline_StatesAndRejects`.
3. **Future host time.** `clampCheckTime`: a host time more than 5 minutes (`maxCheckSkew`) ahead of the
   server clock takes the server time, like a missing time. A time inside the skew is kept (it can block
   a later result for at most the skew). Tests: `TestClampCheckTime`, `TestChecks_FutureHostTimeIsClamped`.
4. **Lock order: already done in LAUN-96.** The checks path locks the team ranks, the issue rows
   (`LockMRLinkIssuesByHeadSHA`), then the link rows (`lockLinkedIssues`); see the LAUN-96 section.
   `TestChecks_LockOrderWithLinkAndMoveOnTwoIssues` covers it. Nothing changed here.
5. **Hook refresh turns GitLab `pipeline_events` on**, shown through the real handler and client by
   `TestGitLabHooks_RefreshTurnsPipelineEventsOn` (a fake GitLab host).
6. **Decided: no new event.** A new head sha that clears a non-none `checks` already emits `issue.updated`
   (`updateLinks` emits when `reset` is true). `TestChecks_GitLab` now asserts it.
7. **Limits kept** (unchanged): a check event before its `synchronize` event is lost; GitLab pipelines that
   share a name overwrite each other; merged-results pipelines match no link.


## LAUN-239: `integrationId` on a change request

`ChangeRequest` (the `changeRequests` include of `GET /issues/{issue}`) gets `integrationId`: the
`mr_links.integration_id` that `ListIssueMRLinks` already read. The web uses it to link the host name to
Settings > Integrations > that connection for an org admin. It is an id only: the connection's name, base URL,
mode and secrets stay behind the admin-scope integration routes, and a member who opens that route still gets
the same answer as before. No query, scope tag, team-access rule or event changed (the issue read was already
team-scoped). The repository link needs no new field: the web cuts it from the request `url` (web-decisions,
LAUN-239). Agents also see `integrationId` through the MCP `get_issue` (it passes the issue answer on): an
id only, which gives no access to the connection. Test: `TestMRState_ChangesBlockShowsAllFiveStates` checks the id.
