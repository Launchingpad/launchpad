> Internal design note from the Orbit deployment; kept for history.

# T8 decisions (ORB-3432, part A of ORB-3431): first-run setup

First-run setup replaces the boot seed. Before ORB-3432 every first boot
made org `orbit`, team ORB, users `admin` and `claude`, and wrote two tokens
to the app log. Now a fresh install makes nothing by itself: it waits for its
operator to set it up through a one-time link. Each judgment call is listed
here so a later ticket can change it on purpose. The routes and wire shapes
are in `server/api/openapi.yaml`; the runbook is `docs/deploy.md` "First-run
setup".

## One instance is one org

1. **One instance = one org** (the project owner, 2026-10-25). Setup creates, in ONE
   transaction: the first user, the org (slug and name from the form), the
   user's membership with role `admin`, the first team (key and name, with
   the default states (nine since LAUN-20), through `tracker.CreateTeamTx`), the Bug /
   Improvement / Feature labels (`tracker.InsertDefaultLabelsTx`, the one
   shared helper; the seed's copy is gone), a web session and the first
   admin token. More teams use the existing `POST /orgs/{org}/teams`. There
   is no org-create route and no multi-org feature; the `org_id` tenancy
   columns stay as they are. Orbit uses slug `orbit` and team `ORB`, which
   the importer, the MCP config and the webhook path name.
2. **Validation is the existing rules.** Org slug: the `orgs_slug_format`
   CHECK of 0001 (`^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$`), checked in Go first
   (`tracker.ValidateOrg`) so a bad slug is a 400 and not a constraint
   error. Team key and name: the `POST /teams` rules (`tracker.ValidateTeam`,
   now shared). Username: the `users_username_format` CHECK, lower-cased
   first. Email: `login.NormalizeEmail`, and it is required (it is the login
   key). Validation runs before the transaction, so a typo never uses up the
   link.

## The setup link

3. **The link is the one credential the server logs.** While the database
   has no user at all, every boot mints a link and writes one line:
   `setup: open <PUBLIC_BASE_URL>/setup#<token> (valid 24h; restart the app
   for a new link)`. With `PUBLIC_BASE_URL` empty it logs the bare path and a
   hint. `setup.LogPrefix` (`setup: open `) is what `deploy/lib.sh`
   (`app_logs_redacted`) filters out of failure reports.
   `TestSetup_NoSecretsInLogsEventsOrIdempotency` checks that the token is
   in that one line and nowhere else, and that the first token, the session
   and the CSRF token are in no log line, event payload or idempotency row.
4. **The token is in the URL fragment**, never the query string: a browser
   does not send a fragment to any server, so it cannot land in nginx, the
   outer proxy or a `Referer`. It is 32 bytes from `crypto/rand`, base64url
   without padding (43 characters). Only `sha256(token)` is stored
   (`setup_tokens.token_hash`, 32 bytes by CHECK, unique). No pepper: unlike
   a 6-digit code, 256 random bits cannot be guessed from the hash.
5. **24 hours, single use, newest wins.** A link expires 24 hours after it
   is minted (database clock). Completing setup consumes it. Every mint (a
   restart while no user exists, or `server setup-link`) first closes every
   older live link, so at most one works. `server setup-link` prints the new
   link to stdout only and logs nothing, so after it the link in the log is
   dead. A link that is lost costs nothing: restart, or run the command.
6. **"Setup is open" means no user at all**, not "no platform admin". An
   install from before ORB-3432 has users and no platform admin; keying on
   the platform admin would have opened setup (and printed a link) on every
   existing install. Any user, an agent too, closes it. Its upgrade step is
   `server grant-platform-admin --user <name>` (an active human only).

## Races and the transaction

7. **One advisory lock, `pg_advisory_xact_lock(hashtext('devops-setup'))`**,
   is taken by the mint and by completion, so two instances booting together
   mint one after the other (each closing the links before it), and a mint
   cannot interleave with a completion. Inside completion the order is:
   lock; re-check that no user exists (else `409 setup_complete`); consume
   the link with `UPDATE setup_tokens SET consumed_at = now() WHERE
   token_hash = $1 AND consumed_at IS NULL AND expires_at > now() RETURNING
   id` (no row: `401 invalid_setup_token`); then every insert; then
   `events.Emit(team.created)` as the last write. Any failure rolls the
   whole transaction back, including the consumption, so nothing is half
   made. The conditional UPDATE is the race arbiter even without the lock:
   a second request with the same link waits on the row lock and then sees
   `consumed_at` set. `TestSetup_ConcurrentCompletion` sends 8 at once and
   wants exactly one 201 and the row counts of one setup.
8. **409 wins over 401, for a well-formed request.** Once a user exists, a
   POST whose token and body have a valid shape is `409 setup_complete`,
   whatever the token. A malformed token still gets `401
   invalid_setup_token` and a body that fails validation gets `400`: the
   shape checks run before the 409 check. That leaks nothing: `GET /setup`
   says the same thing in public.
9. **A used link stays used** even if every user is deleted later (setup is
   then open again, and needs a NEW link). `TestSetup_LinkIsSingleUse`
   covers exactly the case only `consumed_at IS NULL` protects.

## Routes

| Route | Auth | Notes |
|---|---|---|
| `GET /api/v1/setup` | public | `{"setupRequired": bool}`. |
| `POST /api/v1/setup` | public, the link token in the body | `requireJSON`, `idempotency.Reject`, 300 ms floor, a token bucket (30 a minute, burst 10, as `/auth/otp`). Reads no cookie. |

10. **Errors:** `401 invalid_setup_token` (wrong, expired, used or replaced,
    not told apart), `409 setup_complete`, `400 validation_failed` /
    `bad_request`, `409 conflict` (an org with that slug survives from
    deleted users: an org delete does not cascade to users and the other
    way round), `415`, `429 rate_limited` with `Retry-After`. Both new codes
    are in `httpx.Codes` and the `ErrorCode` enum.
11. **The 201 body is the login body plus `token`**: `user` (with
    `isPlatformAdmin`), `orgs`, `csrfToken` and `token` (a `CreatedToken`;
    `token.secret` is the only copy). It sets the same two session cookies
    as `POST /auth/otp/verify`.

## The first user, session and token

12. **The first user is a platform admin** (`users.is_platform_admin`,
    migration 0005) and the org's `admin`. The flag belongs to the user, not
    to a token.
13. **The session** is an ordinary unpinned `session` token (30 days) with
    read, write and admin. A web session could not mint tokens
    when this was written (decisions-T6a #17, superseded by decisions-T11);
    setup is still the one place that returns a first token with no expiry
    and the admin scope.
14. **The first admin token follows the existing PAT rules** (the
    the ticket left the pin open; this is the choice): pinned to the new
    org (every PAT the API mints is pinned), read, write and admin (the user
    is the org admin), and **no expiry**, as any human PAT minted without
    `expiresInDays` (decisions-T6a #18). An expiry was considered and
    rejected: no token outlives its minter (decisions-T6a #19), and a web
    session cannot mint, so a 90-day first token would cap every token the
    operator ever mints with it, and its descendants, at 90 days. The token
    never touches the log (only the response carries it). Rotate it if you
    like: mint one with it, then revoke it. Because it is pinned, it does
    NOT pass `RequirePlatformAdmin`; the setup session does.
15. **`RequirePlatformAdmin`** (`internal/auth`) passes only when the user
    is a platform admin AND the token has the admin scope AND the token has
    no org pin; everything else is `403 forbidden`. An org-pinned token is a
    credential for one org, so it never acts on the install. No route used
    it in ORB-3432; the `/platform` mail settings routes (ORB-3435,
    decisions-T9) are the first.
    `TestRequirePlatformAdmin` (unit) and `TestRequirePlatformAdmin_RealTokens`
    (real token rows) cover member, org admin (pinned and unpinned), platform
    admin with an unpinned admin PAT, with the org-pinned first token,
    without the admin scope, a session without it, and the setup session.
16. **Login and the platform admin.** `login.Verify` adds the admin scope
    for a platform admin even with no org admin role, and
    `GetLoginUserByEmail` lets a platform admin who is in no org log in.
    `isPlatformAdmin` is on the user of `GET /auth/session`, `GET /me`, the
    login response and the setup response.

## Commands

17. `server` (or `server serve`) serves. `server setup-link`: exit 0 with
    the link alone on stdout; 3 when setup is complete; 1 on an error.
    `server grant-platform-admin --user <name>`: refuses an unknown user, an
    agent and a deactivated user. Neither migrates the database; they run
    beside a server that has booted. `deploy/deploy.sh --setup-link` wraps
    the first and refuses a pipe or a file for stdout, as `--seed-tokens`
    did.

## What changed around it

18. `--seed-tokens`, the seed-token guard on deploy and update, and
    `SEED_ADMIN_EMAIL` are gone. A deploy no longer reads the app log before
    it builds: the log holds nothing that a restart cannot make again.
19. `deploy.sh --smoke` makes its own throwaway agent user `deploy-smoke`
    (admin of the smoke org) in the SQL block it already used, and deletes
    the org and then the user. While that user exists the install "has a
    user", so on a first deploy a `POST /setup` in those few seconds is a
    409; afterwards setup is open again with the same link. A leftover smoke
    user (a failed cleanup) keeps setup closed; the warning names the
    `DELETE`.
20. The seed's data lives on as `testdb.Fixture` for the integration tests
    only. No server ever creates it.
21. `scripts/dev-setup.sh` completes setup on a local stack (org `orbit`,
    team `ORB` by default) and prints the first token. Agent users
    (`claude`) are not made: the importer's user map and the MCP server need
    one, and it comes with the invite tickets.

## Residual risk

22. **Anyone who can read the container log first can take the install.**
    The link is a bearer credential for the first, most powerful account,
    and it sits in the app log (and in CapRover's copy of it) until it is
    used, expires or is replaced. That is the same exposure the seed tokens
    had, made smaller: 24 hours instead of forever, one use, and dead the
    moment setup completes or a new link is made. Mitigations: complete setup
    right after the first deploy; `--setup-link` to replace a link you think
    was seen; whoever can read the log can usually also run `docker exec`,
    so this does not widen access on a host. Not mitigated: an operator who
    deploys and leaves setup open for a day lets a log reader win the race.
23. **Not covered by a test:** the boot path in `cmd/server` itself
    (`MintIfRequired` then `LogLink`) is exercised by the real compose run
    recorded in the ORB-3432 handoff, not by a Go test.

## Trial on one computer (LAUN-45)

24. **No code: the trial uses the dev stack (option 3).** LAUN-45 listed three
    options for a laptop trial of `deploy/deploy.sh` over plain HTTP: (1) a
    `--local` mode with http URLs and cookies without `Secure` and `__Host-`;
    (2) a `--trial` mode that bundles a TLS proxy; (3) no code, and the docs
    say "use the dev stack". The project owner chose option 3 on 2026-10-29. Reason: the
    deploy stack is production (`LOG_ENV: production`, `PUBLIC_BASE_URL` and
    `S3_PUBLIC_ENDPOINT` as `https://` with no port, `Secure` `__Host-`
    cookies, no logged login codes), and a relaxed mode would be a second way
    to run it. For a trial: `PROJECT=<name> scripts/dev-setup.sh` in
    `server/`, or a TLS proxy in front of `deploy.sh`. `docs/quickstart.md`
    "Trying it on one computer" says so. Safari and the cookie refusal are
    unmeasured.
