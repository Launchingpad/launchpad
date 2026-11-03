> Internal design note from the Orbit deployment; kept for history.

# T6a decisions (ORB-3414): server login for the web app

T6a adds email one-time-code login, the cookie session, CSRF, CORS and the
token-lifetime rules that T1 deferred. Where the brief left a gap, T6a made
the smallest safe choice. Each one is listed here so a later tranche can
change it on purpose. The routes and wire shapes are in `server/api/openapi.yaml`.

## Login codes

1. **Who may log in.** An active (`deactivated_at` NULL), human
   (`is_agent` false) user with an email, who is a member of at least one
   org. Anyone else is treated as a stranger: the same `202 {}`, no code, no
   email. An agent such as `claude` cannot log in to the web app even if it
   has an email.
2. **One live code per person.** A new request closes every open code of
   that person (`consumed_at`), so verify checks only the newest open code.
   Five wrong tries lock it (`attempts >= 5`); after that even the right
   code fails, and only a new request helps. The code row is locked with
   `SELECT ... FOR UPDATE`, so concurrent guesses are counted exactly.
   Wrong, expired, used, replaced, locked and unknown-email are all the same
   `401 invalid_code`. A malformed email or a code that is not six digits
   is `400 validation_failed`: that is about the format, not the person.
3. **Storage.** `code_hash = HMAC-SHA256(pepper, code_salt || code)`, with
   a random 16-byte salt per row (migration `0002_login_codes`) and a
   server-side pepper, `AUTH_OTP_PEPPER` (at least 32 bytes, from the
   environment, never stored in the database and never logged). A plain
   salted hash is not enough (review of ORB-3414): a code has only 10^6
   values, so someone who can READ the database could request a code for a
   member, read its salt and hash, try every value in under a second, and
   log in. Without the pepper the row reveals nothing. The pepper is
   REQUIRED whenever login is on (Postmark set, or LOG_ENV local/test): a
   missing or short pepper fails the boot, and `login.New` also turns login
   off without one. With login off (no transport) it is not needed. The
   compose file gives the local stack a public dev pepper; a real
   deployment sets its own random value. Changing the pepper only voids the
   codes in flight (10 minutes); sessions are not affected.
4. **Delivery.**
   - `POSTMARK_SERVER_TOKEN` set: emailed through Postmark (same request
     shape as another internal service), sender `POSTMARK_FROM` (required with the
     token), stream `POSTMARK_MESSAGE_STREAM` (default `outbound`). Plain
     text only. The code is in the email body and nowhere else.
   - No token and `LOG_ENV` local or test: the code is written to the app
     log, and `AUTH_FIXED_OTP` (6 digits), when set, is the code every
     request gets. The check is positive (only an explicit local/test turns
     it on), as that service's fail-closed rule says.
   - No token and any other `LOG_ENV`: login is OFF. The server still
     starts; `POST /auth/otp` and `/auth/otp/verify` answer
     `503 login_unavailable`, and boot logs an error line saying why.
   - `AUTH_FIXED_OTP` outside local/test, or together with a Postmark
     token, is a boot error (fail closed), never silently ignored. So is
     login without `AUTH_OTP_PEPPER` (decision 3).
5. **Same answer, same time.** Emails go out on a bounded background queue
   (64), so the Postmark round trip does not tell members from strangers.
   When the queue is full a code is dropped and logged (without the code);
   the person asks again. Both public routes are padded to a 300 ms floor,
   which hides the few milliseconds of database work a member's request
   does.

## Rate limits

6. **Per email, for every email.** 5 code requests per email per hour,
   counted in memory for EVERY address, member or not, keyed by an HMAC
   with a per-process random key (the table holds no address). The sixth is
   `429 rate_limited` whether or not the email belongs to anyone, so the
   429 is not an enumeration oracle. A refused request is not counted.
7. **A durable cap too.** The database also caps a person at 5 codes per
   hour (`login_codes.created_at`). When it trips, the answer is still
   `202` and no code is sent, so a restart (which empties the in-memory
   limiter) cannot open an oracle either.
8. **Coarse per-route limits.** A token bucket per process: `/auth/otp` 30 a
   minute (burst 10), `/auth/otp/verify` 60 a minute (burst 20). It also
   bounds the size of the per-email table. A flood can therefore block real
   logins for a minute; accepted for a tracker with two users. All limits
   are per process: with more than one replica they multiply. **Known
   trade-off:** the per-email limits (decisions 6 and 7) mean that anyone
   who knows an email can lock that address out of login for an hour by
   asking for five codes. This was accepted: the alternative, limiting by
   caller, would need the caller's IP, which this service never stores.

## Sessions and cookies

9. **The session token** is an `api_tokens` row of kind `session`: unpinned
   (it covers every org of the user), scopes `read` and `write`, plus
   `admin` only when the user is an admin of at least one org.
   `RequireScope(admin)` still checks the role in each org, so the admin
   scope never reaches an org where the user is a member. The scopes are
   fixed at login: someone promoted later logs in again. Fixed 30-day life,
   no sliding renewal. Stored as sha256 like every token.
10. **Cookies.** `__Host-odt_session` (HttpOnly) and `__Host-odt_csrf`
    (readable by JavaScript), both `Secure`, `Path=/`, no `Domain`,
    `SameSite=Lax`, 30 days. The `__Host-` prefix makes the browser accept
    the cookie only with exactly those attributes, from this host over
    HTTPS, so no sibling subdomain and no plain-http page can plant or
    overwrite it. Only a `LOG_ENV=local` server (plain http, no Secure,
    where a browser refuses `__Host-` cookies) uses the bare names
    `odt_session` and `odt_csrf`. A server reads only its own names; a
    cookie under the other name is no credential. (A `test` server is
    Secure and uses the `__Host-` names.)
    **Decided (2026-10-24): the deployment is same-site** (the
    web app and the API under one registrable domain; ports and subdomains
    are fine), so the cookies stay `SameSite=Lax`. Cross-host
    `SameSite=None` cookies are not supported and are not planned.
11. **Token kinds do not cross.** The bearer header carries only PATs (and,
    since LAUN-129, `native` tokens, #34) and the cookie only sessions;
    anything else is `401 invalid_token`. So a session can never be used as a
    bearer token to dodge CSRF. When a request has both, the bearer token wins
    and the cookie is ignored. (LAUN-122 broke this for a while: #31 let the
    bearer carry `session`. #34 to #38 repair it.)
12. **CSRF is a signed double submit.** The CSRF token is derived from the
    session: `base64url(sha256("odt-csrf-v1\0" || session))`. A cookie write
    (any method but GET, HEAD, OPTIONS) passes only when the
    `X-CSRF-Token` header equals the CSRF cookie AND that cookie is this
    session's own token, both compared in constant time. What the binding
    does and does not do: a planted CSRF cookie ON ITS OWN cannot match the
    victim's session. It does NOT stop an attacker who can plant BOTH
    cookies, because they can plant their own session with its own valid
    CSRF token (session fixation, or login CSRF). An earlier version of this
    decision said a sibling subdomain could not get past the check; that
    overstated it. The real defence against planted cookies is the
    `__Host-` prefix (decision 10), which keeps every other host from
    setting these cookies at all. A local server, with bare names, has no
    such defence, which is acceptable for localhost. The CSRF token is
    one-way, so reading it (JavaScript can) reveals nothing about the
    HttpOnly session. The check runs before the token lookup, so a forged
    request is `403 csrf_failed` even with a revoked cookie
    (`TestCSRF_CheckedBeforeTheTokenLookup`). Bearer requests skip it.
13. **The login routes** (`/auth/otp`, `/auth/otp/verify`) never read a
    cookie, are exempt from CSRF, refuse `Idempotency-Key` (400: the verify
    response is a credential), and require `Content-Type: application/json`
    (else 415). JSON forces a CORS preflight, which a foreign origin fails,
    so a cross-site page cannot log a visitor into an attacker's account
    (login CSRF).
14. **Logout** sits behind the normal auth chain, so it needs the cookie
    and the CSRF header. It revokes only the caller's own session and
    clears both cookies (`Max-Age=0`). A PAT cannot log out (`403
    forbidden`); revoke it through `DELETE /orgs/{org}/tokens/{id}`. A
    logout with a dead cookie is a 401 that does not clear the cookies: the
    web app should drop its state on any 401.
15. **SSE with a cookie.** `EventSource(url, {withCredentials: true})`
    sends the cookie; a GET needs no CSRF header. The existing per-tick
    re-check reads the token row by id, so a logged-out or expired session
    ends its stream within one poll interval, as a PAT does (T1 #29).

## CORS

16. `WEB_ORIGINS` is a comma list of exact `scheme://host[:port]` origins;
    a wildcard, a path, a query or user info is a boot error. An allowed
    origin gets `Access-Control-Allow-Origin: <origin>`,
    `Allow-Credentials: true` and `Expose-Headers: Idempotent-Replayed,
    Retry-After` on
    every answer, errors included (so the web app can read a 401). A
    preflight from an allowed origin is answered `204` before any auth,
    with `Allow-Methods: GET, POST, PATCH, DELETE`, the five allowed
    headers and `Max-Age: 600`. A preflight from any other origin falls
    through to the router (405, no CORS headers). `Vary: Origin` is always
    set when CORS is on. Empty `WEB_ORIGINS`: no CORS headers at all.

## Token lifetime (T1 deferred item 1, decided)

17. **Superseded by `docs/internal/decisions-T11-web-tokens.md` (ORB-3447).** This
    entry said: a session cannot mint tokens (`POST /orgs/{org}/tokens` is
    `403 forbidden`), so a stolen cookie cannot become a long-lived
    credential. Now a web session of an org admin may mint a token, but the
    token always expires (1 to 365 days) and never has the admin scope. A
    session with the admin scope can still list and revoke tokens.
18. **Agent tokens must expire.** A token for an `is_agent` user (minted by
    an admin for the agent, or by the agent for itself) needs
    `expiresInDays` from 1 to 365; without it, or out of range, `400
    validation_failed`. Human PATs keep the optional expiry (1 to 3650).
    (Superseded: ORB-3432 removed the seed, `decisions-T8-first-run.md`.)
    Seed tokens are not minted through the API and are unaffected.
19. **No token outlives its minter.** When the caller's token has an
    `expiresAt`, the new token's `expiresAt` is cut to it, without an
    error; the response shows the real value. A caller token with no expiry
    imposes nothing.

## Small decisions

20. **Member emails are shown only to org admins** (and each member sees
    their own); everyone else gets `email: null` in `GET /orgs/{org}/users`.
    This closes T1 decision 35 now that emails are login identifiers.
21. **Seed email.** (Superseded: ORB-3432 removed the seed and
    `SEED_ADMIN_EMAIL`, `decisions-T8-first-run.md`.) The seed admin got `SEED_ADMIN_EMAIL` (default
    `admin@example.com`) in the run that created it. A restart never
    changes an existing user, as briefed, so a database seeded by T1 keeps
    its NULL email and that user cannot log in until it is set once by hand:
    `UPDATE users SET email = 'admin@example.com' WHERE username = 'admin' AND email IS NULL;`
    (a dev stack can instead reset with `down -v`).
22. **Compose ports are on 127.0.0.1**, postgres and the app both. Every
    client of this stack (tests, the web dev server, curl, CI on the
    runner) is on the same host, postgres has a fixed password, and a local
    app logs login codes. Production is CapRover, which does not read this
    file. `make compose-check` reads the rendered `docker compose config`
    and fails on any other binding.
23. **The panic recoverer sits inside the access log**, so a panic is
    written through the status writer and logged as 500. When the handler
    had already started its response, the recoverer aborts the connection
    (`http.ErrAbortHandler`) instead of appending an error body to it.
24. **The LISTEN backoff resets to 1s** after a connection that worked
    (T1 deferred item 4).

## Session state, /me and Retry-After (added to T6a by the web client's gap list)

25. **`GET /auth/session`** returns the login body (`user`, `orgs`,
    `csrfToken`) for the credential of the request, so the web app can
    rebuild its state after a reload. It rotates nothing and sets no
    cookie. It needs the `read` scope like `/me`, so any live session or
    PAT works; no credential or a dead one is `401`. With the cookie,
    `csrfToken` is this session's derived CSRF token, which equals a
    valid CSRF cookie; it is NOT copied from the cookie, so a
    planted cookie is never reflected. With a bearer token `csrfToken` is
    `""`, and a pinned token sees only its org.
26. **`GET /me`** adds `displayName` (equal to `name`, which stays for T1
    clients) and `email` (the caller's own; null when it has none, as for
    agents). The email comes with the token lookup, so `/me` still makes
    no extra query.
27. **Every login 429 carries `Retry-After`** in whole seconds (at least
    1) and the stable code `rate_limited`. For the per-email limit it is
    the time until the oldest counted request leaves the hour; for a
    route limit, the time until the next token. The durable database cap
    answers 202 (decision 7), so it has no Retry-After.

## Native login (LAUN-122)

28. **A client mode on verify.** `POST /auth/otp/verify` takes an optional
    `client`: `web` (default) or `native`. Any other value is `400
    validation_failed`, before the code is looked at, so it spends no try.
29. **Same rules.** The 300 ms floor, the limits, the five-try lock, the one
    `401 invalid_code`, agents (no code, no session) and `email_unproven`
    (the first login clears it and revokes older tokens) are the code of the
    web login; the mode changes only what is returned.
30. **Answer.** `{user, orgs, token, expiresAt}`, `Cache-Control: no-store`,
    no `Set-Cookie`, no `csrfToken`. The token is shown once.
31. **The token is a `session` kind row** (unpinned, 30 days, scopes as a web
    session; the name is `native session`). No migration. `BearerSource` now
    accepts kinds `pat` and `session`. A bearer request has no CSRF check, as
    before; a session value sent in a cookie still needs the header. All
    session rules follow the kind: a session cannot mint an admin token and its
    minting is rate limited (T11), also over bearer.
    **Superseded by #34: the row is kind `native`, and the bearer header no
    longer accepts `session`.**
32. **Logout** is the same route. `Logout` already revokes by token id for
    kind `session`, so a bearer session logs out with no change; it also
    sends the (harmless) cookie-clearing headers. A PAT still gets `403`.
    (Since #34: the same route revokes a `native` row too.)
33. **No secret anywhere.** The token is in the 200 body only: not in logs,
    events or idempotency rows (the route has `idempotency.Reject`, so a
    replay can never store the body). The no-secrets test now runs a native login.

## Native tokens are their own kind (LAUN-129)

LAUN-122 (#31) let the bearer header carry kind `session`. So the value of the
web cookie `__Host-odt_session` also worked as `Authorization: Bearer`, and a
bearer request has no CSRF check. A cookie value stolen from a log or a proxy
skipped the CSRF defence of #12.

34. **A new token kind, `native`, not a marker on the row.** Migration 0021 adds
    `native` to the `api_tokens_kind_check` CHECK. The native login
    (`client: "native"`) makes kind `native`; the web login makes `session`.
    Why a kind and not a marker (a column, or the token name): the whole
    credential check already keys on the kind. `TokenSource.Kinds` is
    enforced in one place (`resolveToken`) before any scope or org rule, so
    the rule "this source carries these kinds" cannot be skipped by a new
    route or a new query. A marker would need a second check in every place
    that reads a token, and a forgotten check would fail OPEN (a web value
    accepted as a bearer). With a kind, a forgotten place fails CLOSED: code
    that does not know `native` refuses it. The database CHECK also stops
    any other value. The name `native session` was never a safe marker: it is
    text, not a constraint.
35. **Sources.** `BearerSource.Kinds` = `pat`, `native`. `CookieSource.Kinds` =
    `session`. A refused kind is the same `401 invalid_token` and the same body
    an unknown token gets (`TestLoginNative_WebValueAsBearerRefused` compares
    the bytes), so the answer does not say that the value is a real session.
    A cookie write with a native token in the cookie is `403 csrf_failed` when
    the CSRF pair is missing or wrong (the check runs before the lookup, #12),
    else `401 invalid_token`. CSRF is unchanged: a bearer needs none, a cookie
    still needs the signed double submit.
36. **Login rules follow both kinds.** `auth.IsLoginKind(kind)` is true for
    `session` and `native`. The mint rules (#18, T11: expiry required, no
    admin scope, the rate limit also over bearer) and logout use it. Logout
    (`RevokeSessionToken`) revokes a `session` or `native` row of the caller;
    a PAT is still `403`. A native token logs out by bearer with no CSRF
    header; the web session logs out by cookie with CSRF.
37. **Listing.** `GET /orgs/{org}/tokens` lists rows pinned to the org. A login
    token is unpinned (#9), so neither `session` nor `native` is ever listed
    there, as before. `kind` in the schema (`Token`, `Me.tokenKind`) gains the
    value `native`; `GET /me` shows `tokenKind: "native"` for a native login.
    A native client that reads `tokenKind` should expect it.
38. **Existing rows.** Migration 0021 turns every row that has kind `session`
    and the name `native session` into kind `native`. Only the login code writes
    that name (a PAT has kind `pat`; the web login writes `web session`), so
    the few native sessions made since LAUN-122 keep working and nobody needs
    a new login. Web sessions stay `session`. The down turns `native` back into
    `session` (the old CHECK would refuse it); under the old code that token
    works as a bearer again. This was a judgment call: the other choice was to
    revoke them and ask for a new login.

## Native login is refused for a browser (LAUN-137)

Threat (automated review of the T22 design note): a script on the web origin
(XSS) can read the emailed code the person types into the web page and call
verify with `client: "native"`. It gets a 30-day bearer token in the JSON body
that it can send off the device. The HttpOnly cookie of the web login prevents
exactly this for web logins.

39. **The rule** (it stops only the in-tab variant of the attack; see #42). `POST /auth/otp/verify` with `client: "native"` and an `Origin`
    header (any value, also `null`, also an allowed web origin) is `401
    invalid_code`: the same status, body and headers a wrong code gets, no hint.
    Why `Origin` works against THIS attacker: the attacker is a script in a
    browser. A browser always sets `Origin` on a POST, and a script cannot set,
    change or remove it (a forbidden header name). The earlier argument in
    decisions-T22 #15 (a non-browser client can forge `Origin`) is true and
    beside the point: a non-browser client does not need the trick, see #42.
40. **`Sec-Fetch-Site` and `Sec-Fetch-Mode` count too.** The presence of either
    header gives the same refusal. They are forbidden header names as well, a
    browser sends them on every request, and they cover a browser that omits
    `Origin` in some edge case. Cost: none for the app. A native HTTP client
    (URLSession, OkHttp, Ktor) sends none of the three. Presence is the test,
    never the value. A normal proxy does not add them (see #43 for one that does). (`Sec-Fetch-Dest`, `-User` are
    not used: `Site` and `Mode` are sent with every fetch.)
41. **The refused request spends nothing.** The check runs after the body is
    decoded and BEFORE `Verify`: no code lookup, no try recorded, no limiter
    token taken. Reasons: (a) it learns the script nothing, because the answer
    is the same whether the code is right or wrong, and whether the email exists
    (the web check of `Verify` is never reached); (b) it must not burn the
    person's code: the script that tried and failed must not lock the person
    out of a real login (a failed try is committed, five lock the code), so the
    legitimate app can still use the same code. **Where things sit (checked in
    LAUN-145):** `Pad` is called in the handler (`verifyLoginCode`), after the
    refusal and after `Verify`, so a refused request takes the 300 ms floor like a
    wrong code (timing does not tell them apart; `TestLoginNative_RefusedIsPaddedAndLogged`).
    The verify limiter is ONE global bucket INSIDE `Verify` (`verifies`); there is
    no per-client limiter in front of `/auth/otp/verify`, only `idempotency.Reject`.
    A refused request returns before `Verify`, so it takes no token from that
    bucket. (The first text of this point said a per-client limiter "still sees"
    refused requests. That was wrong: none exists on this route.) A malformed
    email or code in this case is also the plain `401`, not `400`. The cost is
    that a script can send unlimited refused requests that touch no state; each
    one is held for the floor, which bounds one connection, not many.
    **Operator signal (LAUN-145):** each refusal logs one WARN line,
    `native login refused reason=browser_header header=<Origin|Sec-Fetch-Site|Sec-Fetch-Mode>`:
    the header NAME only, never its value, no email, code, token, IP or User-Agent.
    The answer does not change. There is no counter (the server has no metrics
    registry). Tests: `TestLoginNative_RefusedForBrowser`,
    `TestLoginNative_RefusedSpendsNoTry`, `TestLoginNative_RefusedIsPaddedAndLogged`,
    and the refusal in `TestLogin_NoSecretsInLogsEventsOrIdempotency`.
42. **What this does NOT stop (be honest about the size of the gain).** This is
    hygiene against the simplest XSS payload. It is not a defence against XSS that
    can read the code. A script that can read the emailed code from the page can
    also send it to its own server, or intercept the form submit; that server
    calls verify with `client: "native"` and no `Origin`, and gets the token. The
    rule only stops a script that wants the token inside the same tab and cannot
    send the code out. Also not stopped: a non-browser attacker who holds the
    code (a stolen mailbox, a code read off the screen) has the account already,
    as web mode works for them too; and a script that rides a web login that was
    finished (the cookie model), which for an org admin includes minting a PAT
    with `POST /orgs/{org}/tokens` (T11: up to 365 days, no admin scope, rate
    limited). LAUN-130 (list and revoke my own sessions) does not prevent theft;
    it lets the person find and revoke a taken token. Web mode and requests with
    none of the headers are unchanged.
43. **Apps must not send these headers.** A native client that adds `Origin` or
    `Sec-Fetch-*` (a browser-based engine such as Kotlin/JS or wasm, an embedded
    WebView login, an interceptor, a proxy that injects them) gets the hintless
    `401`. The apps use a native HTTP engine (URLSession, OkHttp, Ktor CIO/OkHttp).

## Not done, left for later

- Revoked and expired session rows stay in `api_tokens`; there is no
  sweeper yet. `login_codes` rows older than a day are deleted only when the
  same person asks for a new code.
- No "log out everywhere", and no account screen for unpinned PATs.
- CI does not run `make compose-check` yet: `.gitlab-ci.yml` is being
  changed by ORB-3410 at the same time, so the one-line addition to the
  `test` job is left to avoid a conflict. `make ci` runs it.
- T1 deferred item 5 is unchanged: a stream re-checks credentials on the
  poll tick only.

## Sessions list and revoke (LAUN-130)

A native session lasts 30 days. Before this, only the device that held it could end it. A
lost phone kept a valid token. Three routes, outside `/orgs/{org}` (a login token is unpinned,
#9). No migration: `api_tokens` already has `created_at`, `last_used_at`, `expires_at` and
`revoked_at`.

44. **Routes.** `GET /me/sessions` (read scope), `DELETE /me/sessions/{id}` and `POST
    /me/sessions/revoke-others` ("log out everywhere else"; write scope). All three sit behind
    `refuseAgents`: an agent token is `403 forbidden` (an agent has no session anyway). A
    cookie write needs the CSRF header as always; a bearer needs none.
45. **Ownership is in the SQL.** Every query filters on `user_id = caller` AND `kind IN
    ('session','native')` (`ListMySessions`, `RevokeMySession`, `RevokeMyOtherSessions`). Another
    person's id (in any org), a PAT's id (even the caller's own) and an unknown or malformed id
    are the same `404 not_found`, so the answer does not say that an id exists. Cross-org: a
    person who belongs only to another org cannot see or touch these rows, and the tests
    plant one. PATs stay with `/orgs/{org}/tokens` (admin).
46. **What the list holds.** `id`, `kind` (`session` web, `native`), `name`, `createdAt`,
    `lastUsedAt` (the server already stores it, throttled to once a minute), `expiresAt`,
    `current`. No token value, hash or prefix, no address, no User-Agent (none is stored).
    Only live rows: revoked and expired ones are left out. Newest first, 200 at most (there is
    no sweeper yet, so old rows can pile up). `Cache-Control: private, no-store`. `current` is true
    for the row that made the call; a PAT caller has no current row.
47. **Revoking the current session is allowed.** It is a logout by id (a cookie session also gets
    both cookies cleared). The web page should warn. A repeat revoke is `204` with no audit line,
    as for `DELETE /orgs/{org}/tokens/{id}` (T11 #10). The token stops at once: the credential
    check reads the row on every request.
48. **Who may call.** Any human credential with the scope: a session, a native token or a PAT with
    `write`. A PAT may therefore end the caller's sessions. Revoking is not an escalation (the
    worst a stolen PAT does is log the person out, and it could already change data), and it is
    what a person needs when the phone is lost and only a laptop PAT is at hand. From a PAT
    `revoke-others` ends every session (it has no current one).
49. **No event, no idempotency store, no rate limit.** The rows are user-private; an event
    would show the person's devices on the org-wide feed (T13 #32, #34). `DELETE` and
    `revoke-others` are replay-safe by shape (a replay revokes nothing more), carry no secret
    and no body worth replaying, and a stored replay of `revoke-others` would skip sessions
    made after the first call, which is the wrong thing in a "lost device" flow. So no `s.idem`
    is mounted (a key is ignored; `TestSessions_NoSecretsNoEventsNoIdempotency`). The mint limit (T11 #6)
    guards creating credentials; this route only removes them, so it has none.
50. **Audit.** One info line per call that changed something: `audit: session revoked`
    (actor, `via`, session id, kind, `self`) and `audit: other sessions revoked` (actor,
    `via`, count). Never a token.
51. **Not done: the web page.** The tracker in the web app has no Settings page for sessions
    yet; this is the server half. Follow-up ticket for the page (list, "this device", revoke,
    "log out everywhere else").
52. **Tests.** `internal/api/sessions_integration_test.go`. Mutants checked by hand on the
    SQL: dropping `user_id`, dropping the kind list, in each of the three queries, each fails a test.
53. **The `revoked` count can exceed the list (LAUN-130 review).** `revoke-others` marks every row of
    the caller that is not yet revoked, so it also counts sessions that had expired but were never
    revoked (there is no sweeper). The list leaves those out. A count higher than the listed
    sessions is therefore correct. `TestSessions_ListNewestFirst` pins the list order (newest first).
