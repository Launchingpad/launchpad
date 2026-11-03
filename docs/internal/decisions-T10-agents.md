> Internal design note from the Orbit deployment; kept for history.

# T10 decisions (ORB-3434, part C of ORB-3431): agent users and `server mint-token`

ORB-3434 adds two things: a route that makes an agent user
(`POST /api/v1/orgs/{org}/agents`), and a server command that mints a token
on the server (`server mint-token`, wrapped by `deploy/deploy.sh
--mint-token`). Before it, an install made by first-run setup had no way to
make `claude`, and the only way to mint a token without an admin token was
an SQL recipe in `docs/deploy.md`. Each judgment call is listed here so a
later ticket can change it on purpose. The wire shapes are in
`server/api/openapi.yaml`; the runbook is `docs/deploy.md` "Agent users and
their tokens".

## Scope

1. **In:** the agents route, `server mint-token`, `deploy.sh --mint-token`,
   the runbook steps and the importer's error text. **Out** (the project owner, on the
   ticket): `server login-code`, a split of login into "can issue" and "can
   verify", and any login page change. Web login still needs Postmark.

## Agent users

2. **An agent has no email, and that is the whole login rule.** The route
   has no `email` field: the decoder refuses unknown fields, so a body with
   `email` is `400 bad_request`, and the row has `email` NULL.
   `GetLoginUserByEmail` can never match NULL, and it also has `AND NOT
   u.is_agent`, so an email written in by hand later still gets no code.
   `TestAgents_CannotLogIn` is the tested property: with an email set by SQL
   and the server's fixed code on, `POST /auth/otp` sends nothing and stores
   no `login_codes` row, and `POST /auth/otp/verify` with the fixed code is
   `401 invalid_code` with no cookie. Measured: with `AND NOT u.is_agent`
   removed from the generated query, the test fails ("a code was sent to the
   agent").
3. **The role is always `member`.** That is the role the old seed and
   `testdb.Fixture` give `claude`, and the importer needs only "an active
   member of the org" (`GetOrgMemberByUsername`). There is no `role` field
   (a body with one is `400 bad_request`). So an agent token can never carry
   the `admin` scope (`tracker.CreateToken` and `mint-token` both refuse
   it). An admin agent needs a later ticket.
4. **Usernames are unique on the install**, as the `users` table makes them.
   Any username in use, human or agent, in any org, active or deactivated,
   is `409 conflict` (`a user with username "x" exists already`), and
   nothing is written. There is no "add an existing agent to this org": one
   instance is one org (decisions-T8 #1). The 409 tells an org admin that a
   username exists somewhere on the install; with one org that tells nothing
   new.
5. **The route:** admin scope (so the org admin role, through
   `RequireScope`), `requireJSON` (`415` otherwise, as `/setup`),
   `idempotency.Reject` (the ticket's rule; a retry is a plain 409, and no
   body is kept in an idempotency row), one transaction for the user and
   the membership. The 201 body is a `Member`. A web session may call it
   (with the CSRF header): decisions-T6a #17 closes only token minting to a
   session. **No event is emitted:** there is no member event type, the web
   app keeps a list of the types it handles, and tokens and GitLab config
   are not in the feed either. Clients refetch `GET /users`.
6. **Validation is setup's.** `tracker.ValidateUser` (trim, lower-case,
   the `users_username_format` and `users_name_len` CHECKs) is now the one
   copy; first-run setup calls it and prefixes `user.` to the message, so
   setup's messages did not change.

## `server mint-token`

7. **Who may run it:** whoever can run a command in the app container. That
   person can already read `DATABASE_URL`, so the command gives no new
   power; it replaces the SQL recipe, which wrote a token by hand and could
   get the hash or the prefix wrong.
8. **The same rules as `tracker.CreateToken`**, through one shared function
   (`tokenRules`): name 1-200 characters, scopes from `read`, `write`,
   `admin`, the `admin` scope only for a member with the org admin role, an
   agent token must expire (1-365 days), a human token may (1-3650). The
   token is an org-pinned PAT; only `sha256(secret)` and the 12-character
   prefix are stored. The target must be an active member of the org (the
   lookup skips a deactivated user). An unknown org or member is refused.
9. **Rules that need a caller do not apply**, because there is no calling
   token: the web session refusal, the "no token for another human" rule
   (the operator may mint for the admin: that is the rescue) and "no token
   outlives the token that minted it". `created_by` is NULL: no user minted
   it. A human token with no `--expires-days` does not expire, the same as
   the first admin token (decisions-T8 #14). The runbook says to mint the
   replacement admin token with the command directly, not a short rescue
   token first: a token minted WITH a 1-day token is cut to 1 day.
10. **Output:** the secret alone on stdout, one line; the name, id, prefix,
    user, org, scopes and expiry on stderr. Nothing goes to the log. Exit 0
    on success, 1 on a refusal or error (the reason on stderr), 2 on bad
    usage. `TestMintToken_PrintsTheSecretOnceAndStoresTheHash` checks that
    stdout is only the secret, stderr does not hold it, the row holds the
    hash, and the token works on the real router.
11. **The terminal check is in `deploy.sh`, not in the server command.**
    `docker compose exec -T` gives the container no TTY, so a check inside
    `server mint-token` would refuse every wrapped run. `deploy.sh
    --mint-token` refuses a pipe or a file for stdout before it calls
    docker, as `--setup-link` does (`deploy/test-env.sh` case 14). Run by
    hand in the container, the command writes to whatever stdout is: that
    is the operator's choice.
12. **`deploy.sh --mint-token` takes every argument after it** and passes
    them to `server mint-token` as separate words, never through a shell.
    With no arguments it stops with the usage before it reads `deploy/.env`.

## Around it

13. **The importer's stop names the fix.** "user map names "claude", which
    is not a member of the org" now goes on to name `POST
    /api/v1/orgs/<org>/agents` and the runbook section.
    `TestRun_findsAnAgentMadeByCreateAgent` takes the fixture's `claude`
    away, checks the error, makes `claude` with `tracker.CreateAgent` (the
    route's call) and imports, with the Linear bot mapped to the new user.
14. **No migration.** `users.is_agent` and a nullable `users.email` are in
    0001.
15. **Not covered by a test:** the success path of `deploy.sh --mint-token`
    (the docker stub has no terminal to give it) and the production `docker
    compose exec`. The server side of that path is covered by the
    `cmd/server` integration tests.
