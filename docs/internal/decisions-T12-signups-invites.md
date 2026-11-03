> Internal design note from the Orbit deployment; kept for history.

# T12 decisions (ORB-3436, part E of ORB-3431): sign-up requests, invites and platform admins

ORB-3436 is the server half of "how a new person joins". It adds sign-up
requests that an org admin approves, org invites, and the platform admin
routes. The platform sign-up mode (decisions-T9 #4) now controls the first
two. The web pages are ORB-3450. The login code (`server login-code`) is not
part of this ticket. This file lists each judgment call, so a later ticket
can change it on purpose. The routes and wire shapes are in
`server/api/openapi.yaml` (tags `signups`, `invites`, `platform`). The
runbook is `docs/deploy.md` "Sign-up requests and invites".

## The modes

1. **What each mode allows.**

   | Mode | `POST /signups`, `/signups/confirm` | Approve | Create an invite, accept one |
   |---|---|---|---|
   | `invite` (default) | `403 signup_unavailable` | `403` | yes |
   | `approval` | yes | yes | yes |
   | `closed` | `403` | `403` | `403 signup_unavailable` |

   List, reject and revoke work in every mode, so an admin can always clean
   up. The mode is read from `platform_settings` on each request, so a `PUT
   /platform/settings` applies at once on every server.
2. **The public refusal is one fixed `403`, not a `404`.** The web app
   (ORB-3450) must know whether to show the sign-up form. A `404` would hide
   the route but not the mode: the settings are not secret, and a fixed
   answer tells nothing about a person. The same text is used in `invite`
   and `closed` mode and before first-run setup (no org yet).
3. **Approve needs mode `approval`.** In `invite` mode only an invite adds a
   person. A request that was pending when an admin moved the mode away
   from `approval` cannot be approved; it can be rejected, or it expires.
   This is the safer reading of "closed means no new users".

## Sign-up requests

4. **Which org.** One instance is one org (decisions-T8 #1), so a public
   request goes to the oldest org (the one setup made). The body names no
   org.
5. **No user before approval.** A request stores only a `signup_requests`
   row. No `users`, `org_members` or `api_tokens` row exists until an admin
   approves it. `GetLoginUserByEmail` needs a user, so a pending person
   gets no login code and cannot verify one
   (`TestSignups_NoAccessBeforeApproval`). The note is limited to 1000
   characters, with no control character except a newline and a tab.
6. **The answer never says who exists.** `POST /signups` answers `202 {}`
   in every case: a new address, a user's address, a pending address, a
   blocked address, a taken username. The answer is padded to 300 ms, and
   every email goes out on a queue, so the time is the same.
   `TestSignups_NoEnumeration` compares the bodies and the headers byte for
   byte, and checks the floor. **The email that follows must not tell a
   stranger anything either** (review of 071c026):
   - **A taken username is not checked at request time.** The row is
     stored and the confirm code is mailed, as for a new username. Before
     the fix, a taken username stopped the request and no email came, so
     an attacker who used their own inbox learned that the username
     exists. Now the conflict shows only at approval, as a `409`, and the
     admin rejects and asks the person to try again with another username.
   - **An address that has an account** gets a short "this address has an
     account, log in" email, not a code. Only the owner of the address can
     read it, and it tells them nothing they do not know.
   - **A blocked address, and a pending address that is confirmed,** get no
     email. Again only the owner of the address can see that no email
     came. This is the residual signal: the owner of a blocked address can
     tell that it is blocked. That is accepted; it tells a third party
     nothing.
7. **A second request does not change the first.** A pending request keeps
   its name, username and note. If its email is not confirmed yet, the
   second request mails a fresh confirm code (the per-email limit applies)
   and does NOT extend its life (#11). So a lost email can be sent again,
   and a stranger cannot overwrite the name, username or note of someone
   else's request. Residual: a stranger who knows the address of a
   visible request made while mail was off (no code) can ask again once
   mail works. The request then gets a code, so it is hidden from admins
   and lives at most 24 hours more (#8). The owner of the address gets the
   confirm email and can still confirm it.
8. **The person must prove the email before the request shows to admins**
   (the ticket's rule). The confirm code is the login code's design
   (decisions-T6a): 6 digits from `crypto/rand`, stored as
   `HMAC-SHA256(AUTH_OTP_PEPPER, salt || code)`, 1 hour, compared in
   constant time. The login package exports its helpers (`login.NewCode`,
   `HashCode`, `CodeMatches`), so there is one copy. Every failure is the
   same `401 invalid_code`, and an unknown address does the same hash
   work.
   - **While mail works,** `GET /signups` leaves out a pending request
     that waits for its confirm code (not confirmed, and a code was made),
     and approve answers it `409 conflict` ("the email of this sign-up
     request is not confirmed yet").
   - **Without working mail,** no code is made. The request shows with
     `emailVerified: false`, and an admin may approve it.
   - **A request made while mail was off stays visible when mail comes
     back** (it has no code). Review round 2 found that hiding it too left
     it in the 200 admin queue for up to 30 days where no admin could see,
     reject or approve it, and every confirm then got `429`. Now the admin
     sees it as unverified and can decide. If the person asks again while
     mail works, the request gets a code and becomes a normal unconfirmed
     request (hidden, the 1000 cap, at most 24 hours from then).
     `TestSignups_MailOffFloodThenMailOn` fills the queue with 200 such
     requests, turns mail on, and checks that they show, that a new
     request still gets in, and that a confirm passes once an admin frees a
     place.
   - **An approval without a proved email** makes an account marked
     `users.email_unproven` (#21).
   - **Wrong tries:** 5 lock one code; `code_attempts_total` counts them
     across every code the request had, and 10 in all lock the request
     (no new code is mailed). Before the fix, each new request reset the
     count, so asking again gave 5 more guesses each time (review of
     071c026). The per-email limits (3 requests and 5 confirms an hour)
     bound it further.
   Confirming does not log anyone in.
9. **Limits, and which address is "the client".** `POST /signups`: 30 a
   minute in all (burst 10), 10 an hour per client, 3 an hour per email.
   `POST /signups/confirm`: 60 a minute (burst 20), 30 an hour per client,
   5 an hour per email. `POST /invites/accept`: 30 a minute (burst 10), 20
   an hour per client.
   All are in memory and per process, like the login limits. Keys are kept
   only as an HMAC under a random key per process; no address is logged.
   - **The narrow limits come first, on all three routes.** The
     per-client and per-email hits are taken before the global bucket, so a
     caller that its own limit refuses does not use up the allowance that
     everyone shares, and an unknown client (no key) skips the per-client
     limit (`perClient`). Review round 2 found accept still took the
     global bucket first and keyed an unknown client as one shared key, so
     20 junk tokens an hour behind nginx locked out every invite.
     `TestSignups_ClientLimits` now covers accept too. Checked call sites:
     the three sign-up routes (`takeAll`); `POST /setup` and the platform
     mail test have only one global bucket (nothing to order);
     `login.RequestCode` takes its global bucket before its per-email
     window (ORB-3414 code, not changed here).
   - **IPv6:** the key of an IPv6 address is its /64, because one host
     usually holds a whole /64 (`TestClientKeyFor`).
   - **A flood of new keys cannot fill a table.** Each per-key table holds
     at most 10000 keys and refuses new keys when full (fail closed).
     Until review round 3 a new email was recorded BEFORE the global
     bucket ran, so random valid emails filled the table (most of them got
     `429` from the global bucket anyway) and then every real new address
     got `429` for up to an hour, after the flood ended. Now
     `login.KeyLimit.TakeThen` records a NEW key only after the wider
     limits let the request in, and a known key is still checked first
     (the narrow-first rule). So keys enter a table no faster than the
     global bucket allows: at most 1800 an hour for `POST /signups`, 3600
     for the confirm, 1800 for accept, plus the burst (1810, 3620 and
     1810). A hit that a wider limit refuses is taken back for a KNOWN
     key. `TestSignups_KeyTableFlood` floods 4 times a small table
     and checks that a real new address and a real new confirm get through
     once the global bucket refills. Residual: a known key's hit is
     recorded before the global bucket, then taken back if it refuses, so
     a known caller still pays nothing for a global refusal.
     Residual (reviewers, round 3): the check for a NEW key and the final
     take are two separate steps. If one new address sends many requests
     at the same moment, all of them can pass the first step and use
     global tokens. Then only the per-key limit admits some of them. The
     refused ones keep their global token and their inner email hit. So
     one fresh address can use up about one burst of the global bucket,
     one time per key per hour. The bucket refills in seconds. The per-key
     limits still hold, so the confirm brute-force cap stays at 5 per email
     per hour. We accept this.
   - **The client address** (`internal/api/clientip.go`). Before the fix
     the key was always the TCP peer. Behind the deploy nginx every caller
     has the nginx address, so about 20 strangers in an hour could lock
     out every invite acceptance and sign-up on the install (review of
     071c026). Now:
     - `TRUSTED_PROXY_CIDRS` empty (the default): a request with no
       forwarding header (`X-Forwarded-For`, `X-Real-IP`, `Forwarded`) is a
       direct connection and is keyed on its peer. A request WITH one came
       through a proxy that the server cannot judge, so it gets NO
       per-client key. The global bucket, the per-email limit and the
       token entropy still hold. One shared address never locks out
       everyone. A direct caller can skip its per-client limit by sending
       a forged header; it then meets only the global limits.
     - `TRUSTED_PROXY_CIDRS` set: a peer outside the list is keyed on
       itself and every forwarding header is ignored, so a forged
       `X-Real-IP` changes nothing. A peer inside the list: the server reads
       `X-Forwarded-For` from the right, skips the trusted hops, and the
       first address that is not trusted is the client (`X-Real-IP` when
       there is no `X-Forwarded-For`). A malformed hop or a chain of
       trusted hops only gives no key. A prefix that covers every address
       (`0.0.0.0/0`, `::/0`) fails the boot.
     - **List every hop or none.** The deploy nginx now sends
       `X-Forwarded-For $proxy_add_x_forwarded_for` and `X-Real-IP
       $remote_addr`. Its `$remote_addr` is the outer TLS proxy, not the
       person. If the list holds the nginx network but not the outer
       proxy, the outer proxy becomes the key of every caller, which is the
       lockout again. So the deploy default is empty, and `docs/deploy.md`
       says how to set both.
10. **Two pending caps per org.** While mail works, a new request waits
    for its confirm code and counts against `signup.DefaultUnconfirmedCap`
    (1000). A confirmed request, and one made while mail did not work,
    counts against `signup.DefaultPendingCap` (200), the admin queue. Before
    the fix, unconfirmed requests held places in the 200 for 30 days, so a
    flood with no proof could fill the admin queue for a month (review of
    071c026). Over a cap every request gets the same `429 rate_limited`
    (`Retry-After: 3600`), whether the address is new or a user's: the cap
    of the would-be row is checked first. A confirm into a full admin queue
    is that `429` too, and the code stays usable. An advisory lock
    (`launchpad-signups`) serialises request and confirm writes, so the
    caps are exact. An approval, a rejection, a confirm or an expiry frees
    a place. `TestSignups_Caps` covers both caps.
    - **Residual: one caller can hold the 1000 cap full.** In the default
      deploy (no trusted proxy) only the global bucket (30 a minute) and
      the per-email limit stand in front of it, so one anonymous caller
      with many addresses fills it in about 35 minutes. Real people then
      get `429`. The way out: the 24-hour expiry frees it by itself, and
      an org admin can clear it at once with `DELETE
      /orgs/{org}/signups/unconfirmed`, which deletes every request that
      waits for its code (no decision and no proof is lost;
      `TestSignups_PurgeUnconfirmed`). Confirmed requests and requests
      made while mail was off stay. Setting `TRUSTED_PROXY_CIDRS` (#9) adds
      the per-client limit in front of it.
    - **A confirm into a full admin queue costs a confirm.** Each retry of a
      correct code while the queue is full uses one of the 5 confirms per
      email per hour.
    - **No bulk clear for requests made while mail was off.** Up to 200
      such requests (no code) can sit in the admin queue; an admin rejects
      them one by one, or they expire after 30 days.
11. **Expiry is lazy, with two lifetimes.** While mail works, an unconfirmed
    request expires 24 hours after it was made
    (`signup.DefaultUnconfirmedTTL`); a new code never moves that later.
    Confirming moves `expires_at` to 30 days after the request was made. A
    request made while mail did not work lives 30 days. The server marks
    expired rows (and drops their codes) with one `UPDATE` before each read
    and write of the table. No sweep goroutine is added. An expired address
    may ask again. `TestSignups_Expiry` covers both lifetimes and that a
    second request does not extend the first.
12. **Approve is one transaction and replay-safe.** The request row is
    locked (`SELECT ... FOR UPDATE`). A pending row makes the user (its
    username, name and email), the membership (role `member`) and the
    decision (`decided_by`, `decided_at`, `user_id`). An approved row is
    answered again and makes nothing. Rejected and expired rows are `409`;
    so are a username or an email that a user has now. The route accepts
    `Idempotency-Key` (its body holds no secret).
    `TestSignups_ApproveIdempotentAndRace` sends 8 approvals while a held
    transaction makes them overlap, and wants one user. Measured: without
    `FOR UPDATE` it fails.
13. **Reject.** The reason (at most 1000 characters) is for admins only and
    is never sent to the person. `block: true` stops new requests from the
    email in that org (they get the usual `202` and store nothing). A
    rejected row can be rejected again to add the block; the reason does
    not change. There is no unblock route yet (see #27).
14. **Audit.** Approve, reject, invite create, revoke and accept each write
    one log line at level info: `audit: sign-up approved`, `audit: sign-up
    rejected`, `audit: invite created`, `audit: invite revoked`, `audit:
    invite accepted`, with org, actor and ids. The lines hold no email,
    code or token. The row keeps who decided and when. **No change-feed
    event:** the same choice as agents and tokens (decisions-T10 #5,
    decisions-T11 #7). Every member can read the feed, and the web app
    keeps a fixed list of event types.
15. **The approval email** says that the account is ready and links to
    `<PUBLIC_BASE_URL>/login`. It is sent only when mail works. A rejection
    sends no email.

## Invites

16. **The token** is the setup link's design (decisions-T8 #4): 32 bytes
    from `crypto/rand`, base64url (43 characters), only `sha256(token)`
    stored, in the URL fragment: `<PUBLIC_BASE_URL>/invite#<token>` (a bare
    path when `PUBLIC_BASE_URL` is empty). It is the base the server already
    uses for the setup link and `downloadUrl`. **When mail works the link
    goes to the address only:** the 201 body has `emailed: true` and NO
    `secret` or `url` (#21). Without working mail the body has them once
    (`emailed: false`), and the admin gives the link to the person. The row
    keeps `emailed`. The email subject holds no token, because the dev log
    mailer writes the subject to the log.
17. **Lifetime.** 7 days (`signup.DefaultInviteTTL`, database clock), one
    use, dead after a revoke. An admin chooses role `member` (default) or
    `admin`.
18. **One open invite per email per org** (a unique partial index). A
    second one is `409`: revoke the old one first. Nothing is replaced
    without the admin asking. An open invite that has expired is closed
    (`revoked_at` set, `revoked_by` NULL) when a new one for the email is
    made, and it lists as `expired`. An invite for a member's email, or for
    a deactivated user's or an agent's, is `409` (the last two with one
    fixed text, "that email cannot be invited", that names neither). An
    admin who revokes an invite after its expiry sees it as `revoked`; an
    open invite closed by a newer one after its expiry lists as `expired`.
19. **Accept is one transaction.** The conditional `UPDATE ... WHERE
    accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now()`
    uses up the token and decides a race (`TestInvites_ConcurrentAccept`:
    8 at once, one `201`; measured: without `accepted_at IS NULL` it
    fails). Then the user, the membership and the session. Any failure (a
    taken username, a missing name, an email that became a user's while
    the accept ran: `409 conflict`, as in approve) rolls back the use, so
    the link still works (`TestInvites_EmailTakenDuringAccept`).
20. **Wrong, expired, used, revoked: one answer**, `401 invalid_invite` (a
    new code in `httpx.Codes` and the `ErrorCode` enum). A malformed token
    gets the same answer before any query. An invite whose email now
    belongs to a deactivated user or an agent gets it too.
21. **Nobody but the owner of the address gets an account with its
    session, if the server can help it.** An org admin must not be able to
    use an invite to act as the person it names (review of 071c026: the
    201 body held the token even when the link was emailed, so an admin
    could accept for victim@x and keep a 30-day session, with the admin
    scope for an admin invite, on the account that the real person later
    logs into).
    - **An emailed link hides the token from the admin** (#16). Only the
      address can accept, so the new account's email is proved.
    - **A copied link (no working mail)** must be shown to the admin, or
      nobody could join. The account it makes is marked
      `users.email_unproven`. Its first email-code login (the first proof
      that the person owns the address) revokes EVERY token and session of
      the account and every token it minted for another user (an agent),
      then clears the flag (`login.Verify`,
      `TestInvites_CopiedLinkSessionDiesAtFirstLogin`). Residual risk:
      until that login, whoever used the copied link has the session.
      Without mail nobody can log in by code anyway, so the risk lasts while
      mail is off; an admin who distrusts a copied link revokes the invite.
    - **A sign-up request approved without a proved email** (mail was off)
      is the same case: the account is `email_unproven`, and its first
      email-code login revokes everything it had
      (`TestSignups_ApprovedUnprovenEmail`).
    - **Not revoked at that first login:** invites that an unproven account
      with the org admin role made in the meantime (they are rows, not
      tokens). An admin can list and revoke them.
    - **A stranger can burn a pending request:** anyone who knows the
      address can send wrong codes to `POST /signups/confirm`; 10 wrong
      tries lock the request until it expires. The per-email limit (5 an
      hour) makes that take two hours. The person can wait for the expiry
      and ask again; it costs the attacker nothing else.
    - **An invite for an existing user adds the membership and starts NO
      session** (`200`, status `joined`): the token holder may not be the
      owner of that account, and a session would let an admin log in as any
      user with an email, a platform admin too. The person logs in as
      usual. The body names only the org, not the user. An existing
      membership is not changed (its role stays).
22. **A new user gets a session** like a login: `201`, the two session
    cookies, and the login body in `session`. Its scopes are read and
    write, plus admin for an `admin` invite (`login.SessionScopes`).

Team access on invites and approvals (which teams the person sees) is
`decisions-T20-team-access.md` #26 to #30 (LAUN-226).

## Platform admins

23. **Routes.** `GET /platform/admins`, `POST /platform/admins
    {"user": "<username or email>"}`, `DELETE /platform/admins/{user id}`,
    behind `auth.RequirePlatformAdmin` (decisions-T8 #15). Add: an active
    human only (`404` unknown, `400` agent or deactivated); `201` new, `200`
    already one. The flag is read on every request, so a removal takes
    effect at once; a token or session made before an add keeps its old
    scopes (log in again).
24. **Never remove the last one.** A removal takes the advisory lock
    `launchpad-platform-admins`, then counts the ACTIVE human platform
    admins. If none would remain, it is `409 conflict`. A deactivated
    admin keeps the flag but does not count. Two admins removing each other
    at once leave one (`TestPlatformAdmins`; measured: with the check off by
    one, it fails). `server grant-platform-admin` stays for an install with
    no platform admin at all.

## Secrets

25. **Never in a log, an event, an idempotency row or a response after the
    first.** The three public routes and invite create use
    `idempotency.Reject`. Approve and reject accept a key, but their bodies
    hold no secret. Only hashes are stored. The mail queue never logs a
    job (a failure logs the subject and the mailer's safe error).
    `TestSignups_NoSecretsInLogsEventsOrIdempotency` runs every flow at
    debug level, and searches the log, the events, the idempotency rows and
    the `signup_requests` and `org_invites` rows for the confirm code, both
    invite tokens and the new session.

## Migration

26. **0007 is reversible.** The up adds `users.email_unproven` (#21: an
    account made from a copied invite link, or from a sign-up request
    approved without a proved email) and makes `signup_requests` and
    `org_invites`
    with CHECKs (states, lower-case emails, username format, lengths, a
    code is all three columns or none, 32-byte hashes, a decision for an
    approved or rejected row, a block only on a rejected row) and the
    indexes (one pending request per email per org, one open invite per
    email per org, the list orders, the foreign keys of `ON DELETE SET
    NULL`). The down drops both tables and `users.email_unproven`: pending
    requests and open invites are lost, and an account from a copied link
    then keeps its sessions at its first login. Users that an approval or an invite made stay; they are
    ordinary users. `TestMigration0007_SignupsInvites` covers up, the
    CHECKs, down and up.

## Not done

27. **Notes.** A platform admin who is not an org member cannot approve
    or invite: the routes need the org admin role. First-run setup makes
    the platform admin the org admin, so this matters only after a
    platform admin is added with `POST /platform/admins`. **A full mail
    queue drops an email** (64 slots, one line in the log, no address or
    text): for an emailed invite the admin sees `emailed: true` but the
    person gets nothing. Revoke the invite and make it again. Not done: the web pages (ORB-3450), an unblock route (clear `blocked` by
    SQL for now), a resend of an invite email (revoke and make a new one),
    and pagination cursors on the two lists (`limit`, at most 500, newest
    first; `nextCursor` is always null).
28. **Not covered by a test:** the queue goroutine in `cmd/server` with the
    real `login.MailSwitch` (the tests send in the request), and the
    per-client limit behind the real nginx and an outer proxy (the address
    rules are unit-tested in `TestClientKeyFor` and through the router in
    `TestSignups_ClientLimits`).
29. **The dev log mailer does not log a confirm code.** It writes only the
    subject. On a local stack you cannot read the code from the database
    either: only its HMAC is stored. Approve the request without it, or
    send mail to a local SMTP catcher.
