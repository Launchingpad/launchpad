> Internal design note from the Orbit deployment; kept for history.

# T11 decisions (ORB-3447): agents and API tokens in the web app

ORB-3447 lets an org admin make an agent user and its API token in the web
app, with no terminal. It reverses decisions-T6a #17 ("a session cannot mint
tokens"). Each judgment call is listed here so a later ticket can change it
on purpose. The wire shapes are in `server/api/openapi.yaml`. The web
pages are in `docs/internal/web-decisions.md` "Agents and API tokens (ORB-3447)".

## Who may make a token

1. **Decisions-T6a #17 is superseded.** A web session (cookie) may call
   `POST /orgs/{org}/tokens`. The route still needs the admin scope, so only
   a session of an org admin gets through. A member's session is
   `403 insufficient_scope`, as before. The write needs the `X-CSRF-Token`
   header like every cookie write: without it the answer is
   `403 csrf_failed`, before the handler runs. The route stays outside the
   idempotency store (`idempotency.Reject`): a request with an
   `Idempotency-Key` header is `400`.

## What a session may make

2. **The token always expires.** A token from a session needs
   `expiresInDays` from 1 to 365. Without it, or out of range, the answer
   is `400 validation_failed` with a message that names `expiresInDays`. A
   token with no expiry comes only from a bearer admin token or from
   `server mint-token`. The web app shows 90 days for a person and 365 days
   for an agent.
3. **The token never has the admin scope.** Only `read` and `write` are
   allowed. A request with `admin` is `403 forbidden` ("a web session cannot
   make a token with the admin scope; use a personal access token or the
   server command"). This check runs before the expiry check, so an admin
   request always gets the 403. A stolen cookie therefore cannot make an
   admin credential that outlives the cookie.
4. **The owner is the caller or an agent.** The rules of
   `tracker.CreateToken` hold: a token for another human is `403`, a user
   that is not a member is `400`, and an agent token needs an expiry of 1 to
   365 days (decisions-T6a #18).
5. **The session's own expiry does not cut the token.** Decisions-T6a #19
   cuts a new token to the expiry of the token that made it. A session lasts
   30 days. If it applied here, a 365-day agent token made in the web app
   would expire in 30 days or less, and the screen would say 365. The cut
   stays for a bearer token. The reason it is safe: the new token has its
   own expiry, a maximum of 365 days, and an admin can revoke it at once.

## Limits and audit

6. **Rate limit.** A session may make 5 tokens at once, then 10 an hour for
   each user (a token bucket in memory, per process, like the login
   limits). The next request is `429 rate_limited` with `Retry-After`. A
   request that makes no token (a typo, a refusal) gives its token back, so
   a form error does not use the allowance. A bearer token is not limited:
   it already proved a stored secret. The limit is on the user, so a
   second browser tab of the same admin shares it.
7. **Audit.** Create and revoke each write one log line at level info:
   `audit: token created` and `audit: token revoked`. Both carry org, actor,
   how the actor signed in (`via`), token id, owner, name, scopes and expiry.
   The revoke line is written once, by the call that revoked the token. A
   repeat revoke writes none. These lines are for every caller, not only for a
   session. **No change-feed event is written.** The feed has no fitting
   type, every member can read it, and the web app keeps a list of the
   types it handles (`EVENT_TYPES`). A new type would show token names to
   members and would be a new case in the web app. T10 #5 made the same
   choice for agents. The audit is log only. The project owner may overrule this and ask for a feed
   event. That is a later ticket.
8. **The secret is in the one 201 body and nowhere else.** It is not in a
   log line, an event, an idempotency row or an error text.
   `TestSessionTokens_SecretIsNeverStored` makes a token at debug log level
   with a revoke and a refused request that repeats the secret, then
   searches the log, the events and the idempotency rows for it.

## Unchanged

9. **The bearer path.** A bearer admin token can still make a token with no
   expiry (a human), with the `admin` scope, and for an agent. `server
   mint-token` is unchanged: it calls the same `tokenRules` with
   `fromSession` false. The new rules for a session are in `tokenRules`, so
   the API and the command share one copy of every rule.
10. **Revoke** by a session already worked (admin scope, CSRF header) and
    still does. Revoking twice is `204`.
11. **No migration.** The tables are the same.

## Not covered

12. **A real browser against the real stack** was used for the screenshots
    on the merge request. The component tests use a mocked `fetch`.
13. **The limiter is per process.** With more than one server process the
    limit is per process, as the login limits are. Production runs one.
