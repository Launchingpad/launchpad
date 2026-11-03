# Decisions T22: native apps (LAUN-121)

Design only. No code is in this change. The project owner decided (2026-10-27) that Launchpad
gets native apps: iOS and macOS, Android and Linux. This note says how they log
in, store the token, sync, cache, open links, handle files, ship and are built.
The build tickets are LAUN-122, LAUN-129, LAUN-130, LAUN-134 and LAUNAPP-1 to LAUNAPP-9 (the plan is #52). Migration
numbers: none are needed by this note.
The app tickets are in the team `LAUNAPP` (LAUNAPP-1 to LAUNAPP-9); the server tickets, and LAUN-134 (the apps repo scaffold), are in `LAUN`.

Fixed input (the project owner, 2026-10-27). These are not judgment calls and this note does
not reopen them:

- **ONE Kotlin Multiplatform (KMP) core is shared by all four apps** (iOS, macOS,
  Android, Linux). It owns networking (a client generated from `openapi.yaml`),
  models, the cache, sync, the outbox and the access-loss purge. The UIs stay
  native: SwiftUI with TCA (Apple, one multiplatform target for iOS and macOS),
  Compose (Android) and Compose Desktop (Linux). (the project owner, 2026-10-27, later the same
  day. The first version of this note had two sync implementations, one in Swift and
  one in Kotlin; this version has one.)
- The apps do NOT live in the server repo. They live in a separate
  repository, one repo for all the apps (the "apps repo"). It holds the shared core (`shared/`), `apple/`, `android/` and `linux/`
  (#6). The server repo (it may move to another group later, so no path of it is written into code or CI, #59) keeps
  the API, `server/api/openapi.yaml` and this note. (the project owner, 2026-10-27, later the
  same day.)
- No push in v1. SSE while the app is open, plus background refresh. A relay for
  APNs and FCM is a later design.
- The user types the instance URL (self-hosted). Login is an email code, then a
  bearer token.
- v1 scope: login, issue list and board with filters, issue detail, comments,
  create and edit issue, state and assignee, inbox, attachments. Admin screens,
  git connections, the Linear import and platform settings stay on the web.
- Offline: a local SQLite cache, SSE with `Last-Event-ID`, writes with
  `Idempotency-Key` and `version`.

What the code looks like today (read for this note; file and section names are
in the text):

- On `develop` today, `POST /auth/otp/verify` in web mode (the default) sets two cookies and
  returns `{user, orgs, csrfToken}`, with no token in the body (`openapi.yaml`,
  `LoginResponse`). With `client: "native"` it sets no cookie and returns the token
  (`LoginNativeResponse`, #15). A web session is an `api_tokens` row of kind
  `session`, 30 days, no sliding renewal (decisions-T6a #9). T6a #11 says the
  bearer header carries only PATs and `native` tokens and the cookie only sessions. LAUN-122 (merged as 7dc1786) added the native
  login, and for a short time a web cookie value also worked as a bearer. LAUN-129
  (merged as 6b99f21) closed that: a native login makes a token of a new
  kind, `native` (migration 0021, decisions-T6a #34 to #38), and each kind has one
  source (#15). `server/api/openapi.yaml` on `develop` is the source of this note.
- `POST /orgs/{org}/tokens` needs the admin scope. A member cannot mint a token
  (decisions-T11). So a member has no way today to get a bearer token. This is
  why LAUN-122 exists.
- `GET /orgs/{org}/events/stream` and `GET /orgs/{org}/notifications/stream` are
  SSE. Both honour `Last-Event-ID` or `?after=`. Both end with the comment line
  `: credentials no longer valid`. A `: ping` comment comes every 15 seconds.
  The API text describes the browser cookie for these streams. It also says a
  bearer token wins when both are sent, so a bearer should work; that is
  **unverified** for the streams and LAUN-134 must test it first.
- `PATCH /issues/{issue}` takes `version`; a stale value is `409 version_conflict`.
  Only some `POST` routes take `Idempotency-Key` (create issue, comment and
  attachment; not `prepare`). It lasts 24 hours, per user, over the RAW body bytes.
- Notification routes answer `403 forbidden` for an agent token (`refuseAgents`).
- `web/deploy/nginx.conf.template` sends every unknown path to `index.html`
  (`try_files $uri /index.html`). There is no `.well-known` file today.
- The web issue page is `/:org/issue/:identifier` (`web/src/App.tsx`).
- `.gitlab-ci.yml` of the server repo defines four jobs (`test`, `build`, `mcp-darwin`, `web`).
  `mcp-darwin` runs on a self-hosted macOS shell runner and builds a
  Go binary. No job builds Swift, Kotlin or anything for Linux.

## Scope and shape

1. **Four apps, two UI code bases, one API.** Apple app (iOS and macOS), Android
   app, Linux app. The server is the only source of truth. An app never talks to
   anything but its instance and the object store the instance names in a
   presigned URL (#41).
2. **No server code in the apps repo.** It holds client code only. A server
   change that an app needs is its own MR in the server repo, merged first
   (LAUN-122 and LAUN-129 were the first; the order is #60).
3. **v1 screens (the same on every platform):** instance setup, login, org picker
   (only when the login answer has more than one org), issue list, board, filters,
   issue detail, comments, create and edit issue, state and assignee, inbox,
   attachments, a small settings screen (sign out, cache size, clear cache). Not
   in v1: documents, projects, milestones, cycles as pages (a cycle is only a
   filter), products as pages (a product is only a filter), relations editing
   (they show, read-only), notification settings, tokens, agents, members,
   integrations, import, platform. Reason: the project owner listed the scope. A read-only
   detail for relations and activity costs little because `GET /issues/{issue}`
   with `include=relations,attachments,comments` and
   `GET /issues/{issue}/activity` already return them.
4. **What the apps are not.** They are not a second admin console. An admin
   screen in an app is a link that opens the web page in the system browser
   (`<instance>/<org>/settings/...`).
5. **Linux is the last and the lowest priority** (LAUNAPP-9 is priority Low). The
   shared core has a JVM target from its first commit (LAUN-134), so Linux needs
   only a UI and packaging, never a rewrite of sync.

## Repo layout

6. **Layout: one apps repo, five top-level folders.** The apps repo
   (a separate repository) holds all four apps. The server repo holds none of
   them (#59 to #61 give the reasons for the three choices below).

   ```
   apps-repo/
     settings.gradle.kts         ONE Gradle build for all the Kotlin modules (#61)
     gradle/libs.versions.toml   one version catalog
     spec/openapi.yaml           pinned copy of the server's spec (#59)
     spec/spec.lock              server commit + sha256 of that copy
     scripts/                    sync-spec, regenerate-client, spec checks
     shared/                     KMP module `shared`: client, models, cache, sync,
                                 outbox, purge. Targets: androidTarget,
                                 jvm("desktop"), iosArm64, iosSimulatorArm64,
                                 macosArm64 (#56)
     composeUi/                  KMP module (androidTarget + jvm): the Compose screens
                                 that Android and Linux share (#61)
     android/                    androidApp: the Compose Android app
     linux/                      desktopApp: the Compose Desktop app (LAUNAPP-9)
     apple/
       Launchpad.xcodeproj       one multiplatform app target (iOS + macOS)
       Packages/LaunchpadKit/    Swift package: the XCFramework as a binary target,
                                 the Swift bridge, the TCA dependency clients (#58)
       App/                      SwiftUI views, entitlements, assets
     .gitlab-ci.yml              the apps repo's own pipeline (#48)
     AGENTS.md                   rules for the apps repo; points to this note (#62)
   ```

   Why `shared/` is a top-level folder and not inside `android/`: Apple builds it
   too, and an `apple/` build that reaches into `android/` reads wrong. The Apple
   job runs Gradle for the `shared` module (the XCFramework), then Xcode. Every
   Gradle module that has an Android target needs the Android SDK to configure, and
   that includes `shared`, `composeUi` and the apps built on them. Two jobs do not
   want the SDK: the Apple jobs and a Linux packaging job. **Plan, unverified:** one
   Gradle property, for example `-Plaunchpad.noAndroid=true`, that drops the Android
   targets and the Android modules from the build. It must gate the Android plugin in
   `shared` and `composeUi`, the `include` lines of `settings.gradle.kts` and the
   Android source sets, so that the rest still compiles with no `androidMain`. Xcode's run-script phase that
   calls Gradle MUST set it, or every Apple developer needs the Android SDK. If the
   property does not work, those hosts get the SDK (the `kotlin` job's image will have it; LAUN-134
   builds that image). LAUN-134 measures it. `android/androidApp` and `linux/desktopApp` are
   the Gradle projects (`settings.gradle.kts` maps `:android:androidApp` and
   `:linux:desktopApp` to those folders). Names are a plan, not a measurement;
   LAUN-134, LAUNAPP-5, LAUNAPP-8 and LAUNAPP-9 may rename a folder. `shared/`,
   `composeUi/`, `apple/`, `android/` and `linux/` are the planned names (open
   point L).
7. **One implementation, shared test vectors.** Because the core is one code base,
   there is one sync engine and one outbox. Its tests are **JSON scenarios** in
   `shared/src/commonTest/resources/scenarios/` (made by LAUNAPP-1): events and server
   answers in, expected cache rows and expected outgoing requests out. `commonTest`
   runs every scenario on EVERY target (`jvm`, `androidTarget`, `iosSimulatorArm64`,
   `macosArm64`), so a difference between the Kotlin/Native and the JVM runtime shows
   up as a failing scenario. These are four of the five targets of #56: **`iosArm64`
   (a real iPhone or iPad) runs no scenario.** It compiles the same code as
   `iosSimulatorArm64`, and a device run needs a device on the runner. That gap is
   known and accepted; a failure that only shows on `iosArm64` would be found in the
   app tests or by a person. The scenarios follow `openapi.yaml` and this note; they
   are NOT recorded deliveries (as the GitLab fixtures are not).
8. **One generated Ktor client.** LAUN-134 generates a Ktor client from the PINNED
   copy of the server's spec, `spec/openapi.yaml` (#59), not from a path in another
   repo (generator chosen and written down by LAUN-134, for example a KMP-capable
   OpenAPI generator; **not tried**). Generated code is committed, a script
   regenerates it, and a CI check fails when it is stale against the pinned copy
   (the same idea as `make sqlc-check`). A spec problem is fixed in the SERVER
   repo's spec (an MR there, #60), never in the generated code and never in the
   pinned copy. The SSE reader is hand-written (#27), because a generator does not
   model a stream that never ends. **Unverified:** that the spec's
   `type: [string, "null"]` unions and `oneOf` with `type: "null"` (OpenAPI 3.1,
   used all through `openapi.yaml`) generate cleanly. LAUN-134 finds out first.
9. **The server is the contract.** The apps decode with a tolerant decoder: an
   unknown field is ignored, and an unknown enum value (an event `type`, a
   notification `kind`, a `contentType`) maps to an `unknown` case and is shown
   as plain data or skipped, never a crash. Reason: a self-hosted instance may run
   a newer or older server than the app. The spec has no API level field (open
   point A).

## Instance setup and `/healthz`

10. **The user types the instance URL.** There is no default instance and no
    server list from a vendor. The field takes what the person pastes.
    Normal form (the app does this, not the server):
    - Trim spaces. If there is no scheme, add `https://`.
    - Keep only scheme, host and port. A path, query or fragment is dropped, and
      the screen shows the cleaned origin before it goes on. Reason: the stack
      serves the API at `/api/` on the web origin (`docs/deploy.md`, nginx), and
      an install under a path prefix is not a supported shape. **One prefix:** every
      route in `openapi.yaml` is under `<origin>/api/v1` (for example
      `<origin>/api/v1/auth/otp`), except `/healthz`, which is at the origin root.
      This note writes routes without `/api/v1` (`POST /auth/otp`, `GET
      /orgs/{org}/events/stream`); the paths in `openapi.yaml` already hold `/api/v1`, and the client's base URL is the origin (#8).
    - `https` is required. Plain `http` is allowed only for `localhost`,
      `127.0.0.1` and `::1` (the same rule as the MCP server:
      `LAUNCHPAD_ALLOW_INSECURE_HTTP`). A debug build may allow other hosts
      with a build flag; a release build never. Reason: a bearer token over plain
      http crosses the LAN in the clear.
    - Refuse a URL with user info (`https://user:pw@host`).
11. **The check.** `GET <origin>/healthz` (public; `200 {"status":"ok"}`, or `503`
    with the error body). Then `GET <origin>/api/v1/setup`
    (public; `{"setupRequired": bool}`):
    - `healthz` fails or is not Launchpad's shape: "This address does not answer
      as a Launchpad server." with the reason class (no network, certificate
      invalid, not found, not JSON). The app never prints the response body.
    - `setupRequired: true`: "This server is not set up yet. Finish setup in a web
      browser." with a button that opens `<origin>/setup`. The apps do not run
      first-run setup (decisions-T8: the link token is a one-time credential
      that belongs in the browser).
    - Both pass: go to login.
    The check is a hint, not security. A server that lies about `healthz` gets
    the email address the person types next, and nothing more (#12, #36).
12. **Certificates.** The system trust store only. No "accept this certificate"
    button, no pinning in v1. A self-signed instance fails with the plain text
    "The certificate is not trusted." Reason: a click-through teaches people to
    accept a man in the middle for a credential screen. Open point D asks about
    pinning and private CAs. (A private CA that the person installed on the device
    works, because it is in the system store.)
13. **Remembered instances.** The app stores the origin and, per origin, the org
    slug. One signed-in account per instance in v1; several instances may be saved
    and switched in settings. Each has its own token (#18), cache (#25) and outbox.
    Reason: a person may run more than one install (production and a dev stack).
14. **Org.** The login answer lists `orgs` (`LoginOrg {id, slug, name, role}`).
    One org: use it. More than one: show the picker once and remember the choice.
    AGENTS.md says one instance is one org; the API still supports more, and the
    picker costs one screen. The org slug is the `{org}` of every path.

## Auth and token storage

15. **Sign-in is the email code, through the native mode (LAUN-122, merged).**
    Flow: `POST /auth/otp {email}` (always `202 {}`), then
    `POST /auth/otp/verify {email, code, client: "native"}`. The 200 body is
    `LoginNativeResponse`: `user`, `orgs`, `token` (shown once) and `expiresAt`. It
    sets no cookie and holds no CSRF value. LAUN-122 is built and merged; `openapi.yaml` on `develop` is the source:
    - The token is a `native` kind token (LAUN-129; T6a #34): 30 days, unpinned,
      scopes read and write, admin only for an org admin (T6a #9). It is sent as
      `Authorization: Bearer <token>`; a bearer request needs no CSRF header.
    - `POST /auth/logout` with that bearer revokes it. A PAT still gets `403`.
    - An unknown `client` value is `400 validation_failed` and spends no try. The
      default is `web` (cookies).
    - The 300 ms floor, the per-email and per-client limits, the five-try lock, the
      one `401 invalid_code`, "agents cannot log in" and `email_unproven` are as in
      web login. If the revoke call fails, the app still signs out on the
      device (#16).
    - **Sources by kind (LAUN-129, T6a #35).** The bearer header accepts kinds `pat`
      and `native`. The cookie accepts kind `session` only. A web cookie value sent as
      a bearer, and a native token sent as the cookie, are both refused with the same
      plain `401 invalid_token` (a native token as a cookie write with a missing or
      wrong CSRF pair is `403 csrf_failed` first). `GET /me` shows
      `tokenKind: "native"` for a native login, so the core must decode that value
      (#9). The core never needs a cookie and never sends one.
    - **Browser use (LAUN-137, T6a #39 to #42; this corrects an earlier text).** The worry: a script
      on the web origin (XSS) reads the emailed code from the web page and asks for
      `client: "native"`, then holds a bearer token in the JSON body that it can take off
      the device (an HttpOnly cookie hides the web token from it). The first text of this
      note said an `Origin` check adds no security because a non-browser client can forge
      or omit the header. That was wrong for this threat: the attacker is a script in a
      browser, and a browser always sets `Origin` on a POST, which a script cannot forge
      or omit. So the server refuses `client: "native"` with `Origin`, `Sec-Fetch-Site` or
      `Sec-Fetch-Mode` present, with the plain `401 invalid_code` (no hint), before the
      code is looked at (the code and the tries are not spent). The app is not a browser and
      sends none of them. **What it does not stop (T6a #42):** XSS that can read the code can send it to its own
      server, which calls verify with no `Origin` (so this is hygiene, not a defence against that);
      a non-browser attacker who holds the code already has the account (web mode works for them too);
      an XSS script riding an org admin's web session can mint a PAT; and a taken token is
      still valid for 30 days until LAUN-130 (list and revoke own sessions) exists. The
      rules that follow: (a) the web app never sends `client: "native"` (pinned by
      `web/src/api/auth.test.ts`, LAUN-145); (b) the response is `Cache-Control: no-store`; (c) the apps treat the token as
      a bearer secret (#18, #20); (d) the apps use a native HTTP engine that sends none of those headers (T6a #43).
    - **The cookie-as-bearer weakness is FIXED (LAUN-129, merged as
      6b99f21).** Between LAUN-122 and LAUN-129 a stolen HttpOnly web cookie value
      would also have worked as a bearer. It no longer does (the sources above). **One
      weakness is still open: LAUN-130** (list and revoke my own sessions). Today one
      native session cannot be listed or revoked except by its own logout, and
      `GET /orgs/{org}/tokens` never lists a login token (T6a #37). A lost phone must be
      revocable, so LAUN-130 is a numbered step of the plan and **no build goes to
      anyone outside the team before it is done** (#52).
16. **Sign-out.** If rows are queued, #28's sign-out rule comes first. Then the app calls
    the revoke route. If the network fails, it
    still deletes the local token and the cache, shows "Signed out on this device.
    The session on the server ends by itself within 30 days." The token is
    deleted, so the app cannot revoke it later. Reason: a person who is signing out
    of a phone with no network wants the secret off the device first. The server-side session then simply ages out.
    (A short-lived leftover session is a known cost; open point B asks for an
    "other sessions" list.)
17. **Expiry.** The session lives 30 days with no renewal. The app does not
    refresh a token. On `401` with code `token_expired`, `token_revoked`,
    `invalid_token` or `unauthenticated` for a call that sent a token, the app
    goes to a "Sign in again" screen for that instance. **The UI shows no cached ticket data after the
    `401`**, only the sign-in screen. Reason: the cache is not encrypted (#25), and an
    unlocked device must not show a stranger the tickets of an expired session. The
    data stays on disk so the SAME user who signs in again gets it back at once, and
    the outbox rows wait (#28). If the person does not sign in within 7 days of the
    `401` (a guess, **unmeasured**), the core deletes the cache, the downloads and
    the outbox, and the app asks nothing (a sign-out question cannot be answered on
    a screen nobody opens; the drafts and queued rows are lost, and the sign-in screen
    says so while they exist). A different user on the same instance gets a fresh
    cache (the cache key includes the user id: #25). Open point B asks if 30 days
    is too short for a phone.
18. **Token storage, per platform.** The core declares one interface, `TokenStore`
    (read, write, delete by instance origin and user id, and list; #55). Each platform implements it, and
    tests use a fake.
    - **iOS and macOS:** Keychain, class `kSecClassGenericPassword`, service
      `launchpad`, account = instance origin + user id,
      `kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly` (a background refresh
      must read it with the phone locked; "this device only" keeps it out of
      iCloud Keychain and backups), `kSecAttrSynchronizable` false. On macOS use
      the data protection keychain (`kSecUseDataProtectionKeychain`), not the
      legacy file keychain. No access group is shared with another app.
    - **Android:** a key in Android Keystore (AES-GCM, non-exportable, hardware
      backed where the device has it, no user-auth requirement so background work
      can use it) encrypts the token; the ciphertext is in app-private storage.
      The app sets `allowBackup="false"` and a `dataExtractionRules` and backup-rules file that
      exclude ALL app files (the token file, the cache, `outbox.sqlite`, the draft
      folder and the downloads folder); the app also keeps them in `noBackupFilesDir`. Library choice at LAUNAPP-8 (Jetpack
      Security's `EncryptedSharedPreferences` is deprecated; use the Keystore
      directly or a maintained wrapper). **Unverified:** how the non-exportable
      key behaves after a device restore (the ciphertext is excluded from backups,
      so there is nothing to restore; the person signs in again).
    - **Linux:** the Secret Service API through libsecret (GNOME Keyring, KWallet
      with its Secret Service front end). When no Secret Service is running the
      app shows "No secret store found. Start GNOME Keyring or KWallet, then sign
      in again." and does NOT fall back to a file. Reason: LAUNAPP-9 AC and #36.1.
    **Unverified:** the JVM route to libsecret (a JNA or a small native shim)
    has not been tried. LAUNAPP-9 decides and records it.
19. **The token never leaves its origin.** The client attaches `Authorization`
    only to requests whose origin equals the saved instance origin. Redirects are
    not followed with the header (#43 for the one place a redirect is expected).
    The presigned object-store URL gets no `Authorization` header at all (#41).
20. **No token in a log, a crash report, a URL or a screenshot.**
    - The HTTP logging of each client prints method, path without the query,
      status and duration. It redacts the `Authorization` header and every
      `Set-Cookie`. The `Idempotency-Key`, a presigned `uploadUrl` and a
      `downloadUrl` redirect target are never printed in full.
    - No analytics SDK and no crash-report service in v1. A crash report that
      carries request data would break the AGENTS.md rule (never log, emit or
      store a secret). If a crash reporter is added later, it needs its own note.
    - The token is never put in a URL, a deep link or the pasteboard.
    - iOS: the task switcher snapshot hides the app when the screen holds a login
      code field (`.privacySensitive()` on that screen).
    - The login code field asks the OS for its one-time-code autofill where the
      platform has one (iOS `.oneTimeCode`). Whether it works for a code sent by
      email is **unverified**.
21. **No other sign-in in v1.** No password, no passkey, no pasting a PAT. A
    server with mail turned off (`503 login_unavailable`) cannot be used from an
    app: the app says "This server cannot send login codes. Ask its admin to turn
    on mail." Open point C: a "paste a token" path would work with the existing
    PAT, but only an org admin can mint one in the web app (T11), so it helps only
    admins. Default: not in v1.
22. **Agents.** An agent user cannot log in (T6a #1, `TestAgents_CannotLogIn`), so
    an app session is never an agent's. The `403 forbidden` handling is still
    required (#33), because the server answers it by token kind and the same
    client code may meet it for another reason in future.

## Sync, conflicts and the cache

23. **The model: the cache is a mirror of server rows, plus an outbox.** The
    screens read the cache first and never wait for the network to draw. The
    network updates the cache. The user's offline writes are rows in the
    **outbox** (#28), shown with a "waiting" mark until the server accepts them.
    The cache never holds a value the server did not send, except the optimistic
    state of a queued write, which is kept apart (#30).
24. **What is cached.** Per (instance origin, user id, org id):
    teams, team states, labels, members (`GET /users`), the issues the person has
    looked at or that match the initial window (#26), their comments, attachments
    metadata, the inbox rows, and the sync cursor. Downloaded attachment bytes are kept in a
    separate files folder (#43) and count as part of the cache (#35). Not cached:
    presigned URLs, platform data, tokens, the other orgs.
25. **One SQLite file per (origin, user, org)**, in the app's private storage,
    with the OS file protection that keeps it closed until the first unlock
    (Apple: `NSFileProtectionCompleteUntilFirstUserAuthentication`; Android:
    `noBackupFilesDir`, no cloud backup; Linux: XDG data dir, mode 0600). Each row
    keeps the server JSON of the object plus the columns the filters and sort need
    (id, identifier, team id, state id and type, assignee id, priority, label ids,
    cycle id, project id, `sortOrder`, `version`, `updatedAt`). A **schema version**
    number is stored in the file: when the app's version differs, the app DELETES
    the cache file and rebuilds it from the server. The cache has no migrations,
    because it holds nothing the server does not hold. **The outbox (#28) is NOT in
    this file.** It is a second SQLite file next to it (`outbox.sqlite`, same folder,
    same protection, same (origin, user, org) key) with its own schema version and
    real migrations. A cache rebuild never touches it: a queued row holds the method,
    the path, the request bytes, the key (POST), the object id, the base `version` and
    the BASE field values (PATCH), and its state (#28), so it stays valid
    when the cache is empty (the BASE values are in the row; the conflict rule #29
    refetches the object). The SQLite library is SQLDelight, in the shared core (#57); SQLiteData is not
    used. **The cache file is not encrypted by the app in v1**: it holds ticket text,
    and the OS protects the file (iOS file protection class "until first user
    authentication", which background refresh needs). The risk that an unlocked
    device shows the data is closed by #17 (no data shown after expiry, wipe after 7
    days) and by sign-out (#35); an optional app lock is open point M. Open point E
    asks about SQLCipher.
26. **Initial sync and size.** First open of an org, in this order:
    1. **Seed the cursor, with a bound.** Page `GET /events?after=0&limit=500` and
       follow `nextCursor` (the API sets it while a page is full) until it is `null`,
       but at most **20 pages** (10,000 events; a guess, **unmeasured**). The cursor
       `C` is the `seq` of the last event seen, or `0` if the org has no event. Keep
       no event body. Reason: a quiet org sends no event, and a stream opened with no
       cursor starts at the newest event, so the stream alone can never give a
       cursor; the list does. **If the cap is reached** (`nextCursor` is still set),
       the app stops paging and uses **stream-first mode**: it opens the stream
       (which starts at the newest event) BEFORE steps 2 and 3 and keeps its events;
       the cursor becomes the `seq` of the last event it has applied (after the refetches
       of step 4, #27), never the first one. If the org stays quiet, the cursor
       stays unknown, and the next launch repeats steps 2 and 3 (a full resync whose
       cost grows with issues, not with events). So a big old org still starts in a
       bounded number of requests. The permanent fix is a server read of the latest
       `seq`: a suggested ticket, not made here (open point F, #52).
    2. Fetch `GET /teams`, `GET /teams/{team}/states` per team, `GET /labels`,
       `GET /users`.
    3. Fetch issues with `GET /issues?stateType=triage,backlog,unstarted,started&full=false`
       (all open work), then `GET /issues?stateType=completed,canceled&updatedAt=-P90D`
       for the closed ones changed in the last 90 days (cursor pages, `limit=250`).
    4. Store `C` with the rows. Catch up with `GET /events?after=C` (each event marks
       something stale and is refetched, #27), then open the stream with
       `Last-Event-ID` = the last applied `seq`.
    Rows fetched in steps 2 and 3 may already include changes with a `seq` above `C`;
    step 4 then refetches them once more. That is safe (a refetch is idempotent).
    Older closed issues are fetched on demand (search, open by link) and kept with
    a least-recently-used cap of 20,000 issue rows. The 90 days and the 20,000
    are guesses (**unmeasured**; no one has counted an instance's open issues on a phone).
    A list row carries a `description` cut to 400 characters
    (`descriptionTruncated`); the detail screen fetches the full row
    (`GET /issues/{issue}?include=relations,attachments,comments`, which is the
    full text) and stores it.
27. **Live updates: the change feed.** The app opens
    `GET /orgs/{org}/events/stream` with `Authorization: Bearer` and
    `Last-Event-ID: <last applied seq>`. Each event is `id: <seq>`, `event: <type>`
    and a small `Event` JSON (ids, the identifier, the changed field names). The
    app never builds a row from an event. It **marks the object stale and
    refetches it** (`GET /issues/{issue}` for `issue.*`, `GET .../comments` for
    `comment.*`, the attachments list for `attachment.created`, the team list for
    `team.*`, the label list for `label.created`, the owning issue for `relation.created` and
    `relation.deleted`; any other type is ignored). Reason: the payload is small by design
    ("Refetch for full data"), and one rule works for every kind.
    - **Cursor.** The cursor moves only after the refetches for an event (and for
      every event before it) are written to the cache. The app holds all refetch
      results of an event (or of a batch of events) in memory and writes ALL their
      rows plus the new cursor in ONE transaction. The cursor is the last `seq` of
      that batch, never the `seq` of a half-applied event. A crash or a failed
      refetch leaves the old cursor and no partial rows of that batch, so the events
      are read again (safe: a refetch is idempotent). **A refetch that keeps
      failing does not block the feed for ever.** The app retries a batch with a growing
      wait (1 s, 5 s, 30 s, 2 min, 10 min). The count is kept per object, in memory,
      and a restart resets it. After the fifth failure of one object, the app writes the
      rest of the batch and the cursor in the usual single transaction, leaves that
      object as it was, and marks it `stale` (a flag stored with the object). A `stale`
      object is refetched when the person opens it, at the next return to the
      foreground and at the next good stream connect; a full resync replaces it too.
      The list shows it as normal; its detail screen shows "This ticket may be out of
      date" until the refetch works. Settings shows "Some tickets could not be
      refreshed" with the count. A single response that cannot be decoded follows the
      same rule. **A `404` on a refetch
      counts as applied:** the object is deleted from the cache (#34) in the same
      transaction that moves the cursor. Otherwise the event would be read again
      on every reconnect.
    - **Never open the stream without a cursor.** The stream with neither
      `Last-Event-ID` nor `after` starts at the newest event, so the app
      must not open it without a cursor. #26 seeds the cursor first.
    - **Catch-up by list.** If the stream cannot be opened, or on a background
      wake, the app calls `GET /events?after=<cursor>&limit=500` and follows
      `nextCursor`. Same rule: each event marks something stale.
    - **A rank change with no version change.** `issue.updated` with
      `rebalanced: true` and `stateId` means the other cards of that column got
      new `sortOrder` values with no new `version`. The app refetches the whole
      column (`orderBy=manual`, one team, one state). Polling by `updatedAt` never
      sees this (`openapi.yaml`, `updateIssue`).
    - **Unknown cursor / gap.** The spec says nothing about how old a `seq` may
      be, and no code that deletes events was found (**unverified**: no retention is
      documented). The app therefore takes a **full resync** (all of #26, steps 1
      to 4: reseed the cursor, fetch, store the new cursor, catch up; then drop rows
      the server no longer returns; the new cursor REPLACES the old one) when: the
      time of the last successful sync is more than 7 days ago (a guess,
      **unmeasured**; the age of the cursor's event does not count, or a quiet org
      would resync at every launch); there is no cursor (first run, or a lost cache file);
      the cache schema version changed; the feed answer itself (`GET /events`, or an
      event on the stream) cannot be decoded; or the app
      sees a `seq` lower than its cursor (a restored database on the server). A resync
      that a decode failure caused runs at most ONCE per launch. If the answer still
      cannot be decoded, the app stops and shows "Update the app" (the server may be
      newer than the app, open point A); it does not loop. A
      server ticket for a stable "latest seq" read is open point F; until then,
      step 1 of #26 seeds the cursor.
    - **Data with no event.** The feed has `label.created` but no event for a label
      edit or delete, a workflow-state change, or a member change (`Event.type` in
      `openapi.yaml`; whether `team.updated` covers states is **unverified**).
      So the app refetches labels, each team's states and members (#26 step 2) at
      every sign-in, every full resync and every return to the foreground after
      10 minutes or more, and when a write fails with `400 validation_failed` on a
      state, label or assignee. Pickers can be stale for a short time; this is a
      known limit.
    - **Silence.** A `: ping` comes every 15 seconds. 45 seconds with no byte means
      the connection is dead: close and reconnect.
    - **Reconnect.** Backoff 1 s, 2 s, 4 s ... 30 s, with random jitter, reset on
      the first event or ping. The comment line `: credentials no longer valid`
      stops the loop; the app calls `GET /me`: a `401` goes to #17, a `200` (for
      example a network blip that the server read as a credential race) reconnects.
    - **One stream per org per device**, plus the inbox stream (#32). No stream
      while the app is in the background.
28. **The outbox (offline writes).** Each write the person makes while offline, or
    that fails with a network error, is a row: method, path, the exact request body
    BYTES, for a create-issue or create-comment row a random `Idempotency-Key` made when
    the row is made, the
    base `version` of the object and the BASE value of each field the edit touches
    (for a `PATCH` row; #29 needs them, and the cache row may change or vanish while
    the write waits), the object id, a created time, `firstSentAt` (empty until the first send starts)
    and a state (`waiting`, `sending`, `failed`, `needs_check`). A row with a
    `firstSentAt` has been **attempted**, whatever the answer was. A row found in
    `sending` after a crash or restart returns to `waiting`; `firstSentAt` stays, so
    it counts as attempted with no answer. A successful row is deleted. Rules:
    - **Same bytes, same key (create rows).** The server hashes the method, the path and the raw
      body bytes. A retry resends exactly the stored bytes. The app never
      re-serialises a queued body (`idempotency_key_reused` otherwise). A `PATCH`
      route has no `Idempotency-Key` (`router.go`, `openapi.yaml` `updateIssue`): a
      `PATCH` row carries no key, and `version` is its guard (see the `409` rule below).
    - **Offline create, then more.** A queued create gets a local id. While its row
      has had ZERO send attempts, an edit of that issue changes the create row's body
      (no key has reached the server; a new key is made). After the first attempt
      (even a timeout, which keeps the row `waiting` with the same key and bytes) the
      row is frozen. Every later change is a **dependent row**. A comment is always
      a dependent row, also before the first attempt (it cannot be merged into the
      create body).
      A dependent row waits until the parent has a `2xx` ANSWER: not while it is
      `waiting`, `sending`, `failed` or `needs_check`. Then the app fills in the real
      id, and for a `PATCH` the `version`, from the create's answer. A file for that
      issue waits as a local draft (#41) and uses the real id the same way. If the
      parent is discarded, its dependent rows are discarded with a warning.
    - **A create row the person edits after it failed gets a new key.** The server
      stored the old 4xx under the old key for 24 hours (`IdempotencyKey`), so the
      old key would replay it. A new key also clears `firstSentAt`: the new key
      has never reached the server.
    - **Version chain.** Two queued edits to one issue are NOT both made against the
      same `version`. A `PATCH` row takes its `version` from the answer to the
      previous row for the same object (the app rewrites `version` in the unsent
      body, and BASE is the previous row's MINE). Rows for one object are sent one at
      a time, so an edit is never a conflict with the person's own earlier edit. If the previous
      row was deleted without being sent (a stale board move, #31), the next row
      refetches the issue and takes its `version` and BASE from the fresh row.
    - **Order.** Rows for one object go in the order they were made. Rows for
      different objects may go in parallel (limit 3).
    - **Which routes.** Create issue, create comment (`POST`, `Idempotency-Key`),
      update issue (`PATCH`, `version`, no key), mark notification read / archived (`PATCH`, replay-
      safe by itself; no key), read-all (`before` makes it replay-safe; no key).
      Not queued: create attachment (#41), edit or delete a comment (v1 allows them
      online only), anything that needs the admin scope.
    - **Answers.**
      - A `2xx`, whether or not it is a replay (`Idempotent-Replayed: true`): apply the response to the cache,
        delete the row.
      - **Create rows only (they have a key):** `409 idempotency_in_progress`: wait 2 s and retry (the first try is still
        running).
      - **Create rows only:** `409 idempotency_outcome_unknown`: the write committed but
        its answer was lost. Never re-run it on the old key. The app can NOT prove
        which object is the one it queued, because titles, descriptions and comment
        bodies are not unique (the person may already have an identical issue or
        comment). So **the result stays ambiguous and the person decides**; the app
        never deletes a row or binds a dependent row on a match alone. The row becomes
        `needs_check` with the text "It may have been created already". The app lists
        CANDIDATES to help: for an issue, `GET /issues?full=true&updatedAt=<firstSentAt
        minus 1 hour>` (no `team` filter; the device and server clocks may differ),
        keeping those created by the person at or after `firstSentAt` minus 1 hour with
        the same title; for a comment, `GET .../comments` by the person with the same
        body from that time. The person picks: **"This is mine"** (the row is deleted,
        and its dependent rows take the real id and `version` from the chosen object),
        **"Send again"** (a NEW key) or **"Discard"** (the default button). With no
        candidate the same three choices are shown, and the text says none was found
        but that the issue may have been edited, moved or deleted since. No automatic
        path makes a duplicate, and none loses a write.
      - **PATCH row, `409 version_conflict`:** the first send may have committed and
        its answer been lost, so the 409 can be the app's own change. Refetch the
        issue. If every field in MINE already equals the fresh row, the change is
        applied: delete the row. Otherwise go to #29.
      - `401` / `403`: not stored by the server (`storable` in
        `server/internal/idempotency/idempotency.go` skips `401`, `403` and every
        `5xx`, and any body that holds a token secret), so a retry with the same key
        and bytes runs the write again; on `401`
        stop and go to #17 (the row stays); `403 insufficient_scope` or `forbidden`
        marks the row `failed` with the server's message.
      - `408` and `429`: transient. Keep the row `waiting`; honour `Retry-After`.
        Every status below 500 except `401` and `403` IS stored and replayed for 24
        hours (the same `storable` function). So a `429` that the idempotency layer sees
        is replayed for the same key and bytes. A limiter that runs BEFORE the layer
        never reaches it. In `router.go` today no create-issue or create-comment route has
        a rate limiter and the server code has no `408`, so any such answer there comes
        from a proxy in front of the server, before the layer; the question is small
        today. **Unverified:** that this stays true. **Rule:** a create row gets a NEW
        key only for a `429` that carries the server's JSON error body AND only when
        every earlier send of the row got a definite answer (so none may have
        committed). A `408`, an answer with another body, or a row with an earlier send
        that timed out or failed on the network keeps the SAME key and bytes and waits
        with a backoff. The new key clears `firstSentAt` only in the first case. If a
        retry with the same key returns a replayed `429` or `408` (`Idempotent-Replayed`
        on a `4xx`) after an unanswered send, the row goes to `needs_check`, as for
        `idempotency_outcome_unknown`. LAUNAPP-7 tests both.
      - `4xx` other than the above: the row is `failed`; the person sees the
        message and can edit or discard it.
      - `5xx`, timeout, no network: keep the row `waiting`; retry with backoff
        (5 s up to 5 min).
    - **A key is kept 24 hours** (`openapi.yaml`, `IdempotencyKey`). A create row whose
      `firstSentAt` was more than 23 hours ago and never got an answer becomes
      `needs_check`: the app cannot tell if the first send committed, and a new
      send could make a second issue or comment. The person sees both the queued
      text and the server's recent items by them and decides. Rows that were
      never sent have no such limit (their key has never reached the server). A
      PATCH row has no key and no such limit.
    - **One rule for the outbox and sign-out.** The outbox survives an app restart,
      a cache rebuild (#25) and a `401` (#17: the rows wait for the same user to sign
      in again, for at most 7 days). It does NOT survive a sign-out: without the token nothing can be
      sent. A sign-out (or "remove instance") with rows in the outbox first asks:
      "3 changes are not sent. Send them first, or discard them and sign out?" The count includes
      rows in every state and the draft files. Send is offered only when the network
      and the token work and at least one row is `waiting` (a row that depends on a
      parent that is `failed` or `needs_check` does not count as sendable). It sends
      the `waiting` rows; if any row ends `failed` or `needs_check`, the app lists them and asks
      again: Discard and sign out, or Cancel. Discard deletes the rows. Cancel stops.
      With a dead token (#17) or no network, only Discard and Cancel are offered. A
      row in `sending` after a crash or restart returns to `waiting` with the same
      bytes and key. A different user never reads the file (#25).
29. **Conflict rule for an issue edit.** Every edit sends `version` (the cached
    one the edit was made against). On `409 version_conflict`:
    1. Refetch the issue (`GET /issues/{issue}`).
    2. Compare three values per field: the BASE (from the outbox row), MINE (the
       queued change) and THEIRS (the fresh row).
    3. For each field in MINE: if THEIRS already equals MINE (both sides changed it
       to the same value, or the fresh row shows MINE), that field is **applied**:
       drop it from MINE with no question. If no field is left, delete the row. For
       the rest: if the fields that THEIRS changed since BASE do not overlap with the
       fields in MINE, **rebase**: build a new body from MINE on the fresh row, send it
       with the new `version` (a `PATCH` has no key).
       This is silent, and the row shows "merged with a newer change" once.
    4. If a field overlaps with a DIFFERENT value, **ask**: the person sees MINE
       and THEIRS for that field only, and chooses. No automatic last-write-wins
       for a field the other side also changed.
    Reason: the project owner asked for `version` on writes. The web app is the same shape. The
    description is one field: two edits to it always ask (no text merge in v1).
    A `PATCH` that changes nothing is not an error and not a new version.
    Labels use `addLabels` / `removeLabels` in the body, not the whole `labels` set,
    so two people adding different labels do not conflict. Same for the board: see
    #31.
30. **Optimistic display.** A queued edit shows on top of the cache value with a
    "waiting" mark. The cache row itself is only changed by a server answer. If the
    row fails and is discarded, the value goes back at once. The inbox "read" state
    is optimistic with no mark (it is a cheap, replay-safe flag).
31. **Board order.** A move uses `PATCH` with `state` and `placeAfter` or
    `placeBefore` (an issue of the same team in the destination column), never a
    computed `sortOrder` (`openapi.yaml`: "Do the maths on the server"). A
    `409 stale_placement` means the neighbour moved: the app refetches the column
    (`orderBy=manual`), deletes the row and shows a short message (a dependent row
    of the same object is not affected: it is a different change). A queued board
    move older than 10 minutes is not sent on reconnect (its neighbour has
    probably gone). The row is deleted AND the person is told ("A move you made
    offline was not sent: the board changed"); the card shows the server's position
    after the column refetch, so the screen never claims the move happened. The drag is LAUNAPP-6 "where the web supports it". A `state`
    change with no placement puts the issue on top of the new column (server rule).
32. **Inbox.** `GET /orgs/{org}/notifications` (`state=unread|all`, `limit` up to
    100, `cursor`), `PATCH /notifications/{id}` for read and archived,
    `POST /notifications/read-all` with `before` = the `createdAt` of the newest
    loaded row. The inbox stream `GET /notifications/stream` gives
    `event: notification` (`id: <inboxSeq>`) and `event: count`. The app resumes it
    with `Last-Event-ID` = the last `inboxSeq` it saw. When the stream is closed,
    the app polls `GET /notifications/unread-count` every 60 seconds while the app
    is in the foreground (the API says the same for the web). The inbox is the
    only place for these events: the apps do not read `notification_emails` or
    settings (notification settings stay on the web in v1).
33. **Agents and `403`.** If the inbox, count or stream answers `403 forbidden`,
    the app hides the Inbox tab for this session, stops polling it and does not
    retry. It does not show an error. `GET /me` has `isAgent`; the app reads it
    once after sign-in and hides the tab before the first call when it is true.
    Tests: a fake server answers `403` on the notification routes and the reducer /
    view model test shows no retry (LAUNAPP-7 AC).
34. **Team-scoped access later (T20, design only).** When LAUN-91 ships, the
    server filters lists and the change feed per viewer. The apps need no new
    code path except one rule, set now: **a `404 not_found` on an object the cache
    holds deletes that object (and its comments and attachments) from the cache,**
    and a `403` on a write marks the outbox row `failed`. Reason: a removed member
    must not keep reading a team's tickets from the phone cache. The same rule
    covers an issue deleted or an org the person left. **Proactive purge.** A
    removed member gets no event for the issues they can no longer read (the feed is
    filtered per viewer), so waiting for a `404` is not enough. Once LAUN-91 ships
    (T20 is design only; **unverified** that `GET /teams` is then filtered per
    viewer): (a) at every stream (re)connect and every return to the foreground after
    10 minutes or more, the app refetches `GET /teams`. **The purge is OFF until LAUN-91
    ships and the filtered answer is verified.** Today the server returns every team,
    so a team that is "missing" could only be a bad answer. When it is on, a purge needs
    a guard, because a purge cannot be undone for the bytes and the outbox state: the
    answer must be a complete `200` that decoded fully, it must list at least one team,
    and the SAME team must be missing from TWO such answers, the second a new request
    made at least 30 seconds after the first. Any other answer (an error, a partial or
    empty list, a decode failure) purges nothing and counts as "not read". A team that
    passes the guard is purged: its issues, comments, attachment metadata, downloaded
    bytes and cached inbox rows for those issues; outbox rows for it become `failed`
    (the rows stay, with "You no longer have access to this team"; none is deleted).
    If the guard was wrong, the next full resync restores the cache rows and the
    downloads fetch again; only the `failed` outbox rows need the person to send them
    again. The first miss is held in memory only; a restart or a return from the background
    forgets it, and the second answer comes from the next reconnect or foreground
    return after the 30 seconds (the app does not poll for it). A person removed from
    their LAST team gets an empty list, which the guard refuses, so for that case (b)
    is the only signal that something changed. Today there is no proactive signal at all, because the server
    returns every team. (b) Any `403` or `404` on a team, a team's states or the
    org itself forces a full resync (#27). That resync drops the rows of an object the
    server no longer returns, and it deletes the downloaded bytes of every attachment
    whose row it drops (as the `404` rule does). It drops the rows of a WHOLE TEAM only
    under the same two-answer guard (once LAUN-91 is on); a team that passes the guard
    gets the full purge of (a): rows, bytes, inbox rows, and its outbox rows become
    `failed`. The resync never deletes an outbox row. **Limit:** a person removed from
    their LAST team gets an empty team list, which the guard refuses, so that resync
    drops no whole team and its bytes stay. Three things bound that: the `404` rule
    deletes each object (and its bytes) when the app next refetches it, and
    "Clear cache" and sign-out delete every download (#35). The bytes are on the
    person's own device, and the person could read them before. (c) A `401` is #17, not this. The cache holds no more than the person
    could read at last sync; an offline device keeps it until the next sync (a known
    limit, as with any offline cache).
35. **Cache and sign-out.** "Clear cache" deletes the cache file and the downloaded
    attachment folder (#43); the outbox and the draft files stay. The cache size in
    Settings is the sum of the two. Sign-out deletes the token, the cache (file and attachment folder) and the outbox file,
    after the question in #28 (the outbox is deleted only when it is empty or the
    person chose to discard). Removing an instance does the same. Local attachment draft files (#41) belong to
    the outbox for this rule: the sign-out question counts them, and sign-out deletes
    them with the outbox file. App uninstall removes everything the OS owns; the
    Keychain item on iOS may survive an uninstall (a known iOS behaviour,
    **unverified for current iOS**), so on the first launch after an install the app
    lists the stored (origin, user id) pairs (`TokenStore.list`, #55), compares them with
    its saved instance records and deletes the rest. The cleanup runs only then, and an
    instance record is saved before its token, so it cannot delete a fresh token. It
    runs on iOS and macOS; the Android and Linux stores go with the app's own files.

## Security rules (the short list)

36. **Rules for every platform** (each is repeated where it is used):
    1. The token lives only in the platform secret store (#18) and in memory.
    2. No token, code, `Idempotency-Key`, presigned URL or request body in a log,
       a crash report, an error text, a deep link or the pasteboard (#20).
    3. The token goes only to the saved origin; no redirect carries it (#19).
    4. HTTPS only, except loopback (#10). System trust store only (#12).
    5. Text that came from a person or a git host is data. Issue and comment
       Markdown is rendered with a Markdown renderer that does not run HTML or
       script and does not fetch images from any origin except the instance
       (#44). `ChangeRequest.title`, `author` and `repoPath` are plain text (the
       API says so).
    6. A `launchpad://` or universal link only navigates inside the app. It never
       carries a credential, never sends a request with side effects, and an
       unknown route opens the home screen (#37).
    7. The app asks the server for nothing it does not show. It sends no caller IP
       or User-Agent of its own making (the server stores neither, AGENTS.md); a
       default HTTP library User-Agent is fine.
    8. Screenshots of Markdown or ticket text are the person's own business. The
       login code screen is hidden in the app switcher (#20).
    A **cross-org / cross-user** test per core: two users and two orgs on one
    device never read each other's cache (the file name carries both ids).

## Deep links

37. **Link shapes.** The app understands two kinds of link and nothing else:
    - **Web links** that already exist: `https://<instance>/<org>/issue/<IDENTIFIER>`
      (the web route, `web/src/App.tsx`), `https://<instance>/<org>/inbox`,
      `https://<instance>/<org>/team/<KEY>`. Other web paths open in the system
      browser.
    - **The custom scheme:** `launchpad://<instance-host[:port]>/<org>/issue/<IDENTIFIER>`,
      and `.../inbox`, `.../team/<KEY>`. The path part is the same as the web
      path, so one parser serves both. The host is the instance, so a link works
      when the app has several instances saved. If the host is not a saved
      instance, the app shows "Open this link in a browser" and does nothing else
      (#36.6). `LAUNAPP-7` says "`/orgs/<org>/issues/<ID>`": that is the API path
      shape, not the web path. **The correct web shape is `/<org>/issue/<ID>`.**
      LAUNAPP-7's AC must read that.
38. **Universal links and app links work only for hosts built into the app.** An
    iOS Associated Domains entitlement and an Android `intent-filter` with
    `autoVerify` list their hosts at BUILD time. A self-hosted instance with a name
    the build does not know cannot open the app from an `https` link. So:
    - The official builds (TestFlight, Play) list the hosts the project owner owns:
      `launchpad.example.com` first (the production instance). The list is a
      build setting. Adding a host needs a new release. A wildcard
      host is possible on both: iOS `applinks:*.example.com` (the
      `apple-app-site-association` file is fetched per host), Android
      `android:host="*.example.com"` (the `assetlinks.json` check looks at the ROOT
      domain for a wildcard). A wildcard does not match the bare domain (on iOS the same is likely; also unverified).
      This is from general knowledge and **unverified**; LAUNAPP-8 tries it on a
      device before the list relies on it. Open point G.
    - A different instance uses `launchpad://` links and the in-app "open link"
      field (paste a web link; the app reads the host and path). The web app can
      show an "Open in the app" button that builds the `launchpad://` link from
      `window.location` (a later web ticket; not in the plan below).
    - Anyone who builds the app themselves can list their own hosts.
39. **What the server must serve for the official host.**
    - iOS: `/.well-known/apple-app-site-association`, JSON, `Content-Type:
      application/json`, no redirect, `200`.
    - Android: `/.well-known/assetlinks.json`, JSON, no redirect, `200`. For a
      wildcard host (#38) it must be on the ROOT domain (`example.com`), which
      this stack's nginx does not serve; a wildcard therefore needs a change outside
      this repo.
    **Today** `web/deploy/nginx.conf.template` answers any unknown path with
    `index.html` (200, HTML), so both files would be served as HTML and fail the
    check. The fix is two `location =` blocks and two small files in the web
    image, in the SERVER repo (`web/deploy/`), so it is an MR there (#60). It is part of
    LAUNAPP-7 (Apple) and LAUNAPP-8 (Android), each for its own file, and the files hold the real team id / signing certificate
    fingerprint (**not known to this note**). A TLS proxy in front of the stack
    must pass these paths through unchanged (**unverified**). Both OS vendors fetch the files from their own
    network, not from the phone; a private-network-only instance cannot use
    universal links at all (another reason for #38's fallback).
40. **The routes a link may open** are read routes. Opening `.../issue/ORB-12` does
    `GET /issues/ORB-12` (the identifier works as the path value) and shows the
    cached or fetched issue. A link to an issue the person cannot read is "This
    ticket was not found." (`404`; the API does not tell a missing issue from a
    forbidden one). A link opened while signed out keeps the target and goes to
    sign-in first; it never survives a sign-out (#35).

## Attachments

41. **Upload is the three-step flow, direct to the object store.**
    1. `POST /orgs/{org}/attachments/prepare {filename, contentType, sizeBytes}`
       gives `uploadUrl`, `headers`, `uploadKey`, `expiresAt` (15 minutes). This
       call refuses an `Idempotency-Key` (400): it holds a credential. Never send
       one.
    2. `PUT` exactly `sizeBytes` bytes to `uploadUrl` with exactly the `headers`
       given and **no `Authorization` header** (the URL is the credential). The
       store refuses any other size or type with `403`.
    3. `POST /orgs/{org}/issues/{issue}/attachments {"kind":"file","uploadKey":...}`.
       This route takes `Idempotency-Key`; the attachment id is inside the
       `uploadKey`. As `server/api/openapi.yaml` on `develop` says (LAUN-106,
       decisions-T3b #2), a second attach of it to the SAME issue returns the existing
       attachment with `200` (safe to retry, no new row, no event). To ANOTHER issue it
       is `409 conflict`: the upload was already used by a different issue, so the file
       is NOT on this one. The app treats `200` as success, and treats `409 conflict`
       as a failure with a message (the file must be uploaded again with a new
       `prepare`); it never reads a `409` as "already attached".
    The apps do not queue step 1 or 2 across a long offline time: the 15 minutes
    would pass. An offline attachment is kept as a **local draft file** (in app
    storage) with the issue id. When the network returns, the app runs all three
    steps at once. Step 1 is made at send time, never at queue time. A draft file
    is deleted after it is attached, or when the person discards it.
    The core hands the PUT to a platform `BackgroundUploader` (#55): on Apple a
    background `URLSession` upload task so a locked screen does not cancel it; on
    Android a `WorkManager` job with a foreground notification for large files. **Unverified:** limits of both for a 100 MiB
    file on a bad network.
42. **Allowed types and size.** png, jpeg, gif, mp4, plain text, pdf; at most
    100 MiB; the exact `sizeBytes`. The picker filters to those types, plus HEIC
    and QuickTime `.mov`, which the app converts on selection. **HEIC photos** (the
    iPhone default) are NOT on the server's list, so the app converts a photo
    to JPEG before upload and sends the JPEG's size. A video that is not mp4
    (QuickTime `.mov`) is transcoded to mp4 or refused with a message; the choice is
    in LAUNAPP-7 (**unmeasured** cost). The picker also shows the 100 MiB limit
    before the upload, not after.
43. **Download is the 302.** `GET <downloadUrl>` with the bearer answers
    `302` to a presigned GET (5 minutes, `Cache-Control: private, no-store`). The
    client makes this ONE request with redirects OFF, reads `Location`, and makes
    the second request to the object store with NO `Authorization` header (#19).
    Reason: an HTTP library may copy the header to the new host. The core uses
    Ktor, whose engine differs per target (Darwin, OkHttp, and a JVM engine for
    Linux); whether each engine honours "no redirects" is **unverified**. Doing the
    redirect by hand, once, in the core, makes the rule independent of the engine.
    Downloaded bytes go to the app's cache directory, keyed by attachment id, with
    a cap of 500 MiB and least-recently-used delete. This folder is part of the cache:
    "Clear cache" and sign-out delete it, and the cache size in Settings is the
    SQLite file plus this folder (#35). Images are shown inline; pdf and text open in the platform viewer;
    mp4 plays in the platform player. `kind: link` has no file (`download` gives
    400): the app opens the URL in the system browser after it shows the host.
44. **Images in Markdown.** Markdown that shows a file uses `downloadUrl`. The
    renderer treats an image whose URL is on the instance origin as an attachment
    image (fetched by #43's two steps, with the bearer on the first only), and any
    other image URL as a link the person must tap (no automatic fetch from any
    other origin: it would leak the person's IP and read time to a third party).
45. **Attachments are off on some servers.** Without the `S3_*` settings the file
    routes answer `503 attachments_unavailable`. The app hides the "add file" button
    after the first such answer for this instance and keeps "add link".

## Push and background work

46. **No push in v1.** The project owner's decision. The apps do not register for APNs or FCM.
    What they do instead:
    - **Foreground:** the streams (#27, #32).
    - **Background refresh:** Apple `BGAppRefreshTask`; Android `WorkManager`
      periodic work (minimum 15 minutes). Each wake: read the token, call
      `GET /events?after=<cursor>` and `GET /notifications/unread-count`, apply to
      the cache, and (best effort) post a local notification if the unread count
      went up. **The OS decides when and if this runs.** iOS and Android both may
      run it rarely or never on a phone that is low on battery or unused
      (**unmeasured**; this note gives no number). The apps never promise timely alerts, and
      the Settings screen says so.
    - **Desktop (macOS, Linux):** the stream stays open while the app runs, and a
      local notification comes from the inbox stream.
    - **A local notification is generic by default:** "Launchpad: 2 unread
      notifications" (the count from `unread-count`), with no identifier, title,
      person or kind. An app setting, off by default and kept on the device, lets the
      person show the kind and the issue identifier and title. The comment body is
      never shown in a notification. The `include_excerpt` setting (a web setting) is
      not read by the app in v1. Reason: a lock screen shows the notification, a
      ticket title is often the sensitive part, and T13's rule is that excerpts are
      off unless the person turns them on.
47. **The relay idea (later, not designed here).** APNs and FCM need a sender with
    the vendor key and a device token. A self-hosted instance cannot hold the app
    vendor's key, and the key must not be in the app. The idea: a small relay
    service, run by the app's publisher, that holds the APNs and FCM keys. A
    self-hosted server sends the relay a message that holds ONLY a device token
    and an opaque id (no ticket text); the relay pushes "something new"; the app
    wakes, and pulls the real data from its own instance with its own token. This
    keeps ticket text off the vendor path. It needs: a server setting for the
    relay URL, a device-token register route per user, a retention rule, and a
    privacy note for the relay. It is out of scope for v1; if the project owner wants it, it is
    a new decision note (T23). **Unverified:** both vendors' rules for a
    content-free "silent" push and their rate limits; the design must check them.

## CI and distribution

48. **CI: what each job needs.** The apps repo has its OWN `.gitlab-ci.yml`. Any
    automated review job is set up separately from this note (CI review automation is
    not part of the public tree). **Who adds what.** LAUN-134 is
    the first build ticket in the new repo, so it adds the jobs `kotlin` and `spec-pin`
    (and, as first built, `spec-upstream`, which was dropped: see below) to that file. Its ticket text says "no edit to
    `.gitlab-ci.yml`"; that was written when the apps were to live in the server repo,
    and the text should change (a suggested ticket edit, not made here). Without
    those jobs, the stale-client check of #8 would not exist. LAUNAPP-5 adds `apple`.
    Each release job is added by its platform ticket (LAUNAPP-7, LAUNAPP-8, LAUNAPP-9). The server
    repo's `.gitlab-ci.yml` gets no native job. The Apple jobs run on a macOS runner
    that the apps repo can use (a shell executor with a small number of slots). The
    jobs, all `rules` as the server repo's (MRs and `develop`), each under the 10
    minute CI budget or with its own timeout:
    - **`apple`:** on a macOS runner only. Gradle `:shared`: the Apple tests
      (`macosArm64Test`, `iosSimulatorArm64Test`, the shared scenarios of #7) and
      `assembleXCFramework`; then `xcodebuild test` for the Swift package (the bridge
      test target and the TCA reducer tests with a fake core client) and `xcodebuild
      build` of the app for iOS Simulator and macOS. **Unverified:** that the runner
      has Xcode, a simulator runtime and a JDK for Gradle, and which versions;
      LAUN-134 measures it first. Build time and XCFramework size are
      **unmeasured** (#58).
    - **`kotlin`:** in a Docker image with a JDK and the Android SDK, so it does not
      depend on the macOS runner having them. The runner is a shell executor, so a
      GitLab `image:` key does nothing: the job runs `docker run` itself, as
      `make docker-test` does for Go. The Android build tools (`aapt2` and others)
      are Linux x86_64 programs, and Docker Desktop on Apple silicon runs arm64
      containers, so the job probably needs `--platform linux/amd64` under emulation
      (slow, **unmeasured**). Both facts are in the unverified list:
      `:shared` `jvmTest` and the Android unit tests (the same scenarios; **unverified** how an
      Android unit test gets a SQLite driver: Robolectric or the JDBC driver in
      `androidUnitTest`, chosen and proved by LAUNAPP-1), then
      `:android:assembleDebug`, `:linux` compile and lint. This is the job that
      checks the core on the JVM, which is also the Linux runtime.
    - **`spec-pin`** (blocks the MR, needs no access to the server repo): the sha256 of `spec/openapi.yaml`
      equals the one in `spec/spec.lock`, and regenerating the client changes nothing
      (the stale check of #8). It may share the `kotlin` job.
    - **`spec-upstream` (DROPPED, project owner, 2026-10-28; LAUNAPP-10, apps !6, merged
      2026-10-30; LAUN-190).** It was a weak check (`allow_failure: true`) that the server
      commit in `spec/spec.lock` is on the server's `develop` and that the file at that
      commit equals the pinned copy. It needed a CI variable (the server project) and a
      read path (a job token allowlist on the server project, or a read-only token) that
      were never set, so it could never check anything. **What guards the spec now:**
      `scripts/sync-spec` refuses a sha that is not on the server's `develop` (when a
      person runs it), `spec-pin` blocks a hand edit of the copy, and MR review is the
      only guard against a hand edit of both the copy and the lock. **No release job
      (LAUNAPP-7, LAUNAPP-8, LAUNAPP-9) runs a spec-upstream check any more**, and none
      needs a read path to the server repo.
    - **Release jobs** are manual (`when: manual`), on tags, and use CI variables
      for signing; they are not in the per-MR pipeline.
    - **Runner trust: an accepted, pre-existing risk (a decision for the project owner, not fixed
      by this note).** A shell-executor runner runs the code of a merge request as the
      runner's user, on the host, with every CI variable the pipeline can see. In the
      apps repo that means the group variables for any automated review job (any
      variable that is not protected reaches an MR pipeline), and later the signing
      variables of the release jobs. The same risk already exists for every repository on the instance, and the
      apps repo adds no new kind of exposure. It is written down so it is not a surprise, not
      because this note solves it. Options: (1) an isolated executor for MR jobs (the
      Docker executor, or a VM per job; the shell executor stays for the Apple jobs,
      which need Xcode, so those would need their own answer); (2) protected and
      environment-scoped variables, so that MR pipelines from unprotected branches
      cannot read them (the cost: a review job and any signing step then run only on
      protected refs, or use a different token with less power); (3) one runner per
      group, tagged, so the jobs of different groups do not share a host. The release
      signing variables should be protected and tag-scoped in any case (open point O).
      Until the project owner chooses, treat anyone who can open an MR in the apps
      repo as able to run code on the runner.

49. **Linux has no CI home today. This is open (point H).** The macOS runner
    has no Linux (Docker Desktop on the Mac runs a Linux VM, so a container is
    possible, but it is arm64 Linux, and an AppImage or Flatpak for x86_64 needs
    an x86_64 build: emulated, slow, **unmeasured**). Options LAUNAPP-9 decides
    between: (a) a Linux GitLab runner on a Linux host with Docker, tag-selected (**not** on
    the production host: a runner runs merge request code, and with Docker access that
    is close to root there; use a separate host or a VM); (b) build in a Docker container on the macOS runner
    with `--platform linux/amd64` under emulation, accept the time; (c) build the
    Linux artifact by hand on a Linux machine for the first releases and add CI
    later. The default is (a). The shared core's JVM tests (`jvmTest`) run in the `kotlin` job's
    Docker image from LAUN-134 on, and need no Linux host. Only the AppImage or
    Flatpak packaging needs Linux. A Linux packaging job builds `linux/desktopApp`,
    which depends on `composeUi` and `shared`, and both have an Android target
    (#61): the job needs the Android SDK, or the `noAndroid` property of #6 working.
    LAUNAPP-9 measures it.
50. **Distribution.**
    - **iOS, macOS:** TestFlight from CI (LAUNAPP-7 AC). Needs an Apple Developer
      account, an App Store Connect API key (CI variable, never in the repo) and a
      signing setup; **fastlane or `xcodebuild -exportArchive` with `altool` /
      `notarytool`** is a LAUNAPP-7 choice. A macOS build outside the App Store
      needs notarization. The bundle ids and the account are **not known to this
      note** (open point I).
    - **Android:** Google Play internal testing track from CI (LAUNAPP-8 AC), by the
      Play Developer API with a service account key (CI variable). The app signing
      key is the upload key plus Play App Signing. The same applies to the
      `assetlinks.json` fingerprint in #39: it is the Play App Signing certificate,
      which is only known after the first upload.
    - **Linux:** AppImage or Flatpak, built in CI (LAUNAPP-9). Flatpak sandboxes the
      app: it needs the permission to talk to the Secret Service (a D-Bus
      talk-name, `org.freedesktop.secrets`) and `--share=network`. AppImage has no
      sandbox and no auto update. The default is **Flatpak first** if the Secret
      Service access works inside the sandbox (**unverified**), else AppImage. A
      Flathub listing is not planned.
    - **Updates:** the stores update the mobile and macOS apps. The apps never
      download code. An app shows "This server is newer than the app" only if an
      API level exists (open point A).
    - **Open source:** the apps repo is PRIVATE (project owner, 2026-10-28), so no
      licence is needed while it stays private (open point N). The server repo's
      `docs/internal/open-source-todo.md` does not cover it; a change to public needs
      its own list. Store listings and screenshots must not show real ticket data.
51. **Versioning.** The app version is its own (`1.0.0`, one per platform), not the
    server's. The build number comes from the CI pipeline id. The API
    `info.version` in `openapi.yaml` (`0.4.0 (T5 GitLab webhooks)`) is not a
    reliable compatibility signal (#9).

## Build plan, in order

The ticket texts below are as they read on 2026-10-27 (not re-checked later).

52. **Order and the ticket for each step.** Each step lists what must be done
    before it. Steps 1, 3 and 4 are MRs in the SERVER repo. Steps 7 and 8 need a
    server MR as well (the `.well-known` files, #39). The other steps are MRs in the
    apps repo (#60). **LAUN-134 comes first for the apps** (it was split from
    LAUNAPP-1 and does not need the sync and outbox rules of this note): it proves the
    generated client, the SSE reader and the Swift bridge. It blocks LAUNAPP-5 and
    LAUNAPP-1. LAUNAPP-1 (the rest of the core) blocks LAUNAPP-6, LAUNAPP-8 and
    LAUNAPP-9. LAUN-129 is done. LAUN-130 is a step of this plan, not an optional
    follow-up (it closes the session weakness named in #15), and no build goes to
    anyone outside the team before it is done. That includes the Linux build.

    | Step | Ticket | What | Needs |
    |---|---|---|---|
    | 1 | LAUN-122 | Server: native login mode, bearer-usable session token, logout by bearer (#15). **Done** (merged, 7dc1786). | nothing |
    | 2 | LAUN-134 | Apps repo, KMP core step 1: scaffold, the CI jobs `kotlin`, `spec-pin` (and `spec-upstream`, dropped later) in the apps repo's `.gitlab-ci.yml` (#48), the pinned spec and the generated client (#59), the SSE reader with a check of a bearer on the streams, the URL normaliser and `/healthz` check (#10, #11), `TokenStore` interface, native login, logout, the Swift bridge spike with a tiny Swift test target (checked by hand until LAUNAPP-5 adds the `apple` job), the apps repo's `AGENTS.md` (#62) (#6, #8, #54 to #58) | 1 and 3 |
    | 2b | LAUNAPP-1 | The rest of the shared core: SQLDelight cache and outbox, sync, purge, JSON scenario tests on every target (#7, #23 to #35, #54 to #57) | 2 |
    | 3 | LAUN-129 | Server: stop a web cookie value working as a bearer; native token kind (#15). **Done** (merged, 6b99f21). | 1 |
    | 4 | LAUN-130 | Server: list and revoke my sessions. **Open.** | 1 and 3 |
    | 5 | LAUNAPP-5 | `apple/`: project, instance setup (over the core's normaliser), login, Keychain `TokenStore`, org picker, TCA clients around the core, reducer tests, the `apple` CI job on the macOS runner (#10 to #18, #48, #58) | 2 |
    | 6 | LAUNAPP-6 | Apple issues: list, board, filters, detail, create, edit, comments over the core (#29 to #31) | 5 and 2b |
    | 7 | LAUNAPP-7 | Apple: inbox, wiring the core's sync and outbox to the app lifecycle and background refresh, deep links, attachments, TestFlight, and a server MR for the `apple-app-site-association` file (#39, #27 to #45, #50) | 6; to leave the team: 4 |
    | 8 | LAUNAPP-8 | `android/` and `composeUi/`: Compose Android app on the core, the first shared Compose screens, Keystore `TokenStore`, app links, Play internal track, and a server MR for `assetlinks.json` (it needs the Play App Signing fingerprint, #50, #39) (#18, #38) | 2b; to leave the team: 4 |
    | 9 | LAUNAPP-9 | `linux/`: Compose Desktop on the core and the `composeUi` screens of step 8, libsecret `TokenStore`, local notifications, `launchpad://`, AppImage or Flatpak, Linux runner decision (#18, #49, #50) | 2b and the `composeUi` screens of 8; to leave the team: 4 |

    Notes on the order:
    - The cache, outbox, sync and purge rules (#23 to #35) are built ONCE, in LAUNAPP-1
      (after LAUN-134). LAUNAPP-6 and LAUNAPP-7 call the core; they do not rebuild any of it. LAUNAPP-7
      wires the core to the OS lifecycle (foreground, background refresh), not to a
      second sync engine.
    - Step 4 can run in parallel with steps 2 and 5 to 8. A build for the team
      only (the developers' own devices) may come before them; TestFlight external
      groups and the Play track beyond the team may not.
    - Steps 5 to 7 (Apple) and step 8 (Android) can run in parallel after step 2. Step 8 and the issue
      screens of step 6 wait for 2b. Heavy builds should stay within what the build host can run at once
      (about four).
    - **Tickets this note suggests and does not make:** (a) the web "Open in the
      app" button (#38); (b) an API level field or route (open point A); (c) a
      server read of the latest event `seq` (open point F, #26); (d) the two
      `.well-known` routes if the nginx change cannot be done in the web image (#39).
      Each of these is an MR in the server repo.

## Tests

53. **What the shared core must test** (`commonTest`, on every target; the scenarios
    of #7 hold the data). The Swift side tests the bridge and the TCA reducers with a
    fake core client (#58); it does not retest sync.
    - The URL normaliser (#10) with a table of inputs.
    - Login flow in the core (the sign-in calls and their errors; the Swift reducer and
      the Compose state holder only show them): code request, wrong code (`401 invalid_code`),
      429 with `Retry-After`, `503 login_unavailable`, `setupRequired`.
    - Token store fake: the token is read only for the saved origin (#19); a
      redirect to another origin is not given the header.
    - SSE reader: ids, event names, comment lines, partial chunks, `Last-Event-ID`
      on reconnect, 45 s silence, `: credentials no longer valid`.
    - Cursor seed: paging `GET /events?after=0` ends at `nextCursor: null` or at the cap; an org with no event gives cursor 0; a quiet org does not full-resync at every launch.
    - Sync: all rows of an event batch and the cursor commit in one transaction (a
      crash mid-batch leaves the old cursor and no partial rows), a missing team purges its rows, full-resync triggers (#27), `rebalanced` column refetch.
    - Outbox: a lost create answer gives candidates, never an automatic match; an edit of an attempted create is a dependent PATCH; PATCH rows keep BASE values across a cache rebuild; an offline-created issue with a queued comment sends in order; same bytes and key on retry, `idempotency_outcome_unknown`,
      `idempotency_in_progress`, 23-hour `needs_check`, ordering per object.
    - Conflict: rebase when fields do not overlap, ask when they do, no question when
      THEIRS already equals MINE (#29).
    - Idempotency: a `401` or `403` is not stored, so the same key and bytes run the write
      after sign-in; a `429` with the JSON body gets a new key only when every earlier send
      had an answer; an earlier timeout, then `429` or `408`: same key, and a replayed
      `4xx` goes to `needs_check` (#28).
    - Team purge: nothing is purged before LAUN-91; after it, only after two complete
      answers 30 seconds apart; an empty or partial answer purges nothing (#34).
    - Feed: a refetch that fails five times moves the cursor and marks the object
      `stale` (#27) and refetches it on open; a decode failure resyncs once per launch, then
      shows "Update the app" (#27); a stream-first start keeps the last applied `seq` (#26).
    - `TokenStore.list` returns every stored pair, and the first-launch cleanup deletes
      the unknown ones (#35).
    - Lost create: `idempotency_outcome_unknown` never deletes a row or binds a
      dependent row without the person's choice, even with a matching candidate (#28).
    - Expiry: after a `401` no cached data is exposed to the UI, and the wipe happens
      after 7 days (#17).
    - Cursor seed cap: at 20 pages the core switches to stream-first mode (#26).
    - `404` deletes cache rows (#34); sign-out deletes token, cache and outbox, and asks when rows are queued (#28, #35); a cache schema bump keeps the outbox (#25); two
      users and two orgs never share a file (#36).
    - Agent: `403` on notification routes hides the tab and does not retry (#33).
    - Attachments: no `Authorization` on the object-store PUT and GET; no
      `Idempotency-Key` on `prepare`; HEIC converted before `prepare` (#41 to #43).
    - Log redaction: a test logs a request with a bearer, an upload URL and a key
      and greps the output for them (like `TestLogin_NoSecretsInLogsEventsOrIdempotency`).

## The shared Kotlin core

54. **One core, and who owns what.** `shared/` owns: the Ktor client and models
    (generated, #8), the SSE reader, the URL normaliser and `/healthz` check (#10,
    #11), the cache and the outbox (#25, #28), sync and the cursor (#26, #27), the
    conflict rule (#29), the access-loss purge (#34), cache and sign-out deletion
    (#35), the no-token-in-logs HTTP logging (#20), the origin rule for
    `Authorization` and the by-hand redirect (#19, #43), and the allowed-type and
    size checks for uploads (#42). The UIs own: screens, navigation, the platform
    pieces behind the interfaces of #55, deep-link entry points (#37), local
    notification display (#46) and the OS lifecycle hooks (foreground, background
    refresh). The UIs never write SQLite, never call the API directly and never hold
    the token. A rule in this note that says "the app" means the core unless it names
    a platform.
55. **Platform interfaces.** The core takes these from each platform; a test fake
    exists for each:
    - `TokenStore`: read, write and delete the token by instance origin and user id
      (the Keychain account of #18), and **list** the (origin, user id) pairs it
      holds. #35's first-launch cleanup needs the list: it compares the pairs with the
      saved instance records and deletes the rest. Each platform implements all four;
      the fake does too.
    - `Connectivity`: a flow of "network usable" (iOS `NWPathMonitor`, Android
      `ConnectivityManager`, JVM a periodic probe). The core still treats a failed
      request as the truth; this only starts a retry sooner.
    - `Clock`: the time, so tests control it (the 7-day, 10-minute and 23-hour rules).
    - `FileAccess`: the folders for the cache file, `outbox.sqlite`, drafts and
      downloads (#25, #41, #43) and file reads for uploads.
    - `NotificationSink`: show a local notification (#46); the generic text is built
      in the core, the display is the platform's.
    - `BackgroundUploader`: run the object-store PUT so it survives a locked screen
      (#41).
    Everything else (HTTP engine choice, SQLite driver) is `expect`/`actual` inside
    the core, not a UI concern.
56. **Targets.** `androidTarget`, `jvm("desktop")` (Linux), `iosArm64`,
    `iosSimulatorArm64`, `macosArm64`. `macosX64` (Intel Macs) is **not** built; open
    point L. The Apple app is one multiplatform SwiftUI target for iOS and macOS, so
    it links one XCFramework that holds the iOS device, iOS simulator and macOS
    slices. No `iosX64` (no Intel simulators on the macOS runner). All five targets are
    modules of the one Gradle build at the apps repo root (#61), not of a separate
    build per folder.
57. **Cache stack: SQLDelight.** The cache and the outbox are two SQLite files (#25),
    each its own SQLDelight database, with `.sq` files for the schema and queries and
    `.sqm` files for the outbox migrations. Reasons: (a) SQL-first, so the filters of
    #3 and #26 are plain SQL that a reader can check against this note; (b) its
    drivers cover Android, native iOS/macOS and JVM, the same five targets as #56;
    (c) migrations are explicit files, which the outbox needs and the cache does not;
    (d) the sqlc habit of this repo is the same idea (SQL files, generated code).
    Room KMP is the alternative: it is Google's API and fits Android well, but its
    support for the macOS native target is **unverified**, and it hides SQL behind
    annotations. SQLiteData (the earlier plan for Apple) is NOT used: it would be a
    second cache in Swift next to the core's. **Unverified for both:** a SQLDelight
    native driver on `macosArm64` with a Kotlin version in use; LAUNAPP-1 proves it
    before anything else is built on it.
58. **The Swift bridge and TCA.** The core builds an XCFramework
    (`assembleXCFramework`), which `apple/Packages/LaunchpadKit` takes as a binary
    target. Kotlin `suspend` functions, `Flow` and sealed classes are not good Swift
    by default (callbacks, no `AsyncSequence`, erased enums). **Plan:** SKIE (a
    Touchlab Gradle plugin) maps `suspend` to `async`, `Flow` to `AsyncSequence` and
    sealed classes to Swift enums; the fallback is KMP-NativeCoroutines. **All of
    this is unverified here**, including SKIE's support for the Kotlin version, the
    `macosArm64` slice and Swift 6 concurrency checking; LAUN-134 proves it with a
    tiny Swift test target that calls the core. TCA wraps the core as
    **dependency clients**: a `CoreClient` struct of closures (`issues(filter)`
    returning an `AsyncStream`, `perform(edit)`, `signIn(...)`, and so on) with a live
    value that calls the Kotlin object and a test value that is a fake, so reducers
    are tested with no Kotlin. The reducers hold UI state only; the core's flows are
    the source of data. **Unmeasured risks:** the Kotlin/Native memory model and
    threading with Swift's main actor and TCA's effects (the current memory model is
    the default, but how Kotlin objects cross actors under Swift 6 is untried);
    SKIE's coverage of the types the generated models use; the XCFramework size in
    the app; the Gradle build time on the macOS runner.

## The apps repo and the server repo

59. **The spec is a pinned copy in the apps repo (chosen).** The core is generated
    from `spec/openapi.yaml`, a copy of the server's file. `spec/spec.lock` holds the
    server commit it was taken from and the sha256 of the copy. `scripts/sync-spec`
    takes a server commit (a SHA on the server's `develop`), writes the copy and the
    lock, and regenerates the client. Checks: `spec-pin` (#48). (`spec-upstream` was dropped; see #48.)
    Options weighed:
    - **Git submodule of the server repo.** Rejected. It drags the whole server repo
      (Go, web, history) into every apps clone and CI checkout; it needs the server
      repo's address in `.gitmodules` (a path that may change if the server repo moves); a submodule pointer shows a commit hash, not a diff, so a
      reviewer cannot see in the MR what changed in the API; and an agent that works
      on the apps can edit the server tree by mistake.
    - **A CI fetch of `develop`'s spec at build time.** Rejected. A build would no
      longer be reproducible: the same apps commit would generate a different client
      next week, and a server MR could break the apps CI with no apps change. It would
      also make every apps build depend on the server repo being up and readable.
    - **A pinned copy (chosen).** A build is reproducible and offline; the API change
      shows as a normal diff of `spec/openapi.yaml` in the apps MR, next to the
      generated code and the app code that uses it; no CI job needs the server repo's
      address; only `scripts/sync-spec` reads it (the `LAUNCHPAD_SERVER_REPO` clone path), so a move of the server repo changes nothing in CI.
    **The cost:** a person must run `scripts/sync-spec` to take a server change (it is
    never automatic), so the copy can lag; nothing in CI warns about it (the
    `spec-upstream` job that did was dropped, #48). The copy is a
    duplicate of a spec of about 6,700 lines in the apps repo. And one thing is
    **unverified**: that the
    chosen generator reads the file from that folder with no change. A hand edit of
    `spec/openapi.yaml` fails `spec-pin` (the sha256 no longer matches the lock); a
    hand edit that also changes the lock is caught only in MR review (the dropped
    `spec-upstream` job used to compare the copy with the server's file at the lock's commit).
60. **A change in both repos is two MRs, server first.** An API change that an app
    needs:
    1. The server MR (with `openapi.yaml`, the handler, tests, its decisions note) is
       reviewed and **merged to the server's `develop` first**.
    2. The apps MR runs `scripts/sync-spec <that merge commit>`, which changes the
       copy, the lock and the generated client, and then uses the change in the core
       or the UI. It may only pin a commit that is on the server's `develop`
       (`sync-spec` refuses any other sha; the dropped `spec-upstream` job used to check it too).
       `sync-spec <sha>` takes ALL server API changes up to that commit, not only the
       one the MR needs, so the generated client may change in other places too.
    3. Each MR names the other in its description. The ticket is one ticket in the
       Launchpad team (`LAUN`), and a comment on it says which repo each MR is in.
    A change that ONLY ADDS (a route, a field, an enum value) needs no more: older
    apps ignore what they do not know (#9). A change that REMOVES or CHANGES the
    meaning of something is done in three steps: the apps stop using it and ship, the
    server then removes it, and the spec copy follows. Self-hosted servers and
    store-reviewed apps update at different times, so the server must keep working
    with the last released app; an API level (open point A) would make this
    checkable. A server MR that needs an app MR to be useful (the `.well-known` files
    of #39) is still merged first. One exception: `assetlinks.json` needs the Play
    App Signing fingerprint, which exists only after the first Play upload (#50). Its
    server MR therefore comes after that upload; until then Android app links are
    unverified and the `launchpad://` fallback applies (#38). The Apple file comes
    first, because the team id is known from the account. **No cross-repo CI check exists** (a server MR
    does not run the apps' tests). A server author who changes `openapi.yaml` in a
    breaking way must say so in the MR; this is a rule for people, not a test, and its
    cost is a break found only at the next `scripts/sync-spec`.
61. **One Gradle build; `linux/` is its own folder; `composeUi` is shared (chosen).**
    The Linux app is `linux/`, a top-level folder with its own `desktopApp`. The
    Compose screens that Android and Linux share are `composeUi/`, a KMP module
    (androidTarget + jvm) that both `android/` and `linux/` use. The earlier plan put
    the Linux app inside `android/`. Reasons for the change: (a) `linux/` inside
    `android/` says Linux is a part of Android; it is a peer; (b) its CI, its
    packaging (Flatpak, AppImage, a runner of its own, #49) and its release tag are
    its own; (c) a third module, `composeUi`, that both depend on makes the sharing
    visible and keeps the Android-only code (`androidApp`, Keystore, app links) out of
    Linux's CODE. It does not keep the Android SDK out of Linux's TOOLCHAIN:
    `composeUi` and `shared` have Android targets, so a Linux build configures them
    too (#6 has the `noAndroid` property that is meant to avoid it, **unverified**). **Costs:** one more top-level folder; `composeUi` must use neither an
    Android-only API nor a desktop-only API (the other target fails to compile), and
    it needs Compose Multiplatform for both targets; and
    the Compose screens are shared, so a change for Android can break Linux (the
    `kotlin` job builds both). All the Kotlin modules are in ONE Gradle build
    (`settings.gradle.kts` at the root): one version catalog, no composite-build
    substitution to debug, `:shared`, `:composeUi`, `:android:androidApp` and
    `:linux:desktopApp` as plain project dependencies. The Apple app is not a Gradle
    module; Xcode calls Gradle for the XCFramework (#58). The cost is that the Apple
    job configures a build that also lists the Android modules (#6 has the property
    that is meant to avoid this, **unverified**).
62. **Where the design lives.** This note stays in the SERVER repo
    (`docs/internal/decisions-T22-native.md`). The apps repo's `AGENTS.md` (written
    by LAUN-134) links to it by name and does not copy it, because a copy would
    drift. A decision that a build ticket changes (a result of the bridge spike, a
    renamed folder) is written back into this note by an MR in the server repo; a
    build ticket's results note (for example LAUN-134's `shared/README.md`) lives in
    the apps repo and this note points to it.

## Not measured (honest list)

- Whether the first automated review run in the apps repo works (#48).
- Whether the `kotlin` job's Docker image runs the Android build tools on an arm64
  macOS runner's Docker, and how slow `--platform linux/amd64` is (#48).
- Whether the generator reads `spec/openapi.yaml` with no change (#59).
- Whether a bearer token works on the two SSE streams (the API text describes the
  cookie; LAUN-134 tests it first).
- Whether `team.updated` covers a workflow-state change (#27).
- The 7-day full-resync limit (#27) and the cost of paging `GET /events?after=0` (#26).
- Whether the OS one-time-code autofill works for a code sent by email (#20).
- Background upload limits of `URLSession` and `WorkManager` for 100 MiB (#41).
- The vendors' rules for a content-free push (#47).
- That `openapi.yaml` generates a clean Kotlin client (#8, LAUN-134).
- Whether each Ktor engine (Darwin, OkHttp, JVM) honours "no redirects" (#43).
- The Kotlin/Native memory model and threading with TCA and Swift 6 actors, SKIE
  coverage, the `macosArm64` slice, the XCFramework size and the Gradle build time
  (#58).
- SQLDelight's native driver on `macosArm64`, and Room KMP on macOS (#57).
- The 20-page seed cap and the 7-day wipe after a `401` (#26, #17).
- The size of an instance's open and recent issues on a phone: the 90 days and the 20,000
  row cap (#26) are guesses.
- Whether the server keeps all events forever, and how old a cursor may be (#27).
- Background refresh frequency on iOS and Android (#46).
- The JVM route to libsecret, and Flatpak access to the Secret Service (#18, #50).
- Whether the macOS runner has Xcode, a simulator, a JDK and the Android SDK, and
  which versions (#48).
- Whether a Gradle property keeps the Android modules out of the Apple job (#6, #61).
- That `.privacySensitive()` hides the iOS task-switcher snapshot (#20).
- Whether an iOS `*.` associated domain matches the bare domain (#38).
- Whether `GET /teams` is filtered per viewer once LAUN-91 ships (#34): the proactive
  purge relies on it.
- An x86_64 Linux build under emulation on an arm64 macOS runner (#49).
- Which routes have their rate limiter after the idempotency layer, so a `429` or `408` is stored (#28).
- Whether the filtered `GET /teams` of LAUN-91 is complete enough for the purge guard of #34.
- Whether a shared shell runner can be made safe for merge request jobs (#48, open point O).
- Android wildcard app links and where their `assetlinks.json` must live (#38).
- Whether a TLS proxy in front of the stack passes `/.well-known/` unchanged (#39).
- The Android non-exportable key after a device restore (#18); the iOS Keychain item
  after an uninstall (#35).
- The cost of HEIC to JPEG and `.mov` to mp4 for large files (#42).
- Every claim about SwiftUI, TCA, SKIE, SQLDelight, Room, Compose Desktop, APNs,
  FCM, TestFlight, Play and Flatpak is from my general knowledge, not tried on this
  project.

## Open points for the project owner (each with a recommended default)

A. **An API level.** The apps cannot tell if a server is too old or too new. Default:
   tolerant decoding now (#9); a small `apiLevel` in `GET /me` or `/healthz` as a
   later server ticket.
B. **Session life and an "other sessions" list.** 30 days, no renewal (T6a #9).
   Default: keep it, sign in again; revisit if it annoys. A way for a person to list
   and revoke their own sessions is LAUN-130, a ship blocker before any external
   tester (#15, #52). LAUN-129 (a web cookie value as a bearer) is fixed and merged.
C. **Paste a token as a sign-in.** Default: not in v1 (#21).
D. **Certificate pinning or an "accept this certificate" path for private CAs.**
   Default: no (#12); a private CA installed in the OS works.
E. **Encrypt the cache file (SQLCipher).** Default: no in v1; the OS protects the
   file and #17 hides and wipes it after expiry (#25). Ticket text is the same
   sensitivity as on the web.
F. **A stable "latest seq" read** so the core can start a cursor without paging
   the feed. Default: page `GET /events?after=0` up to the 20-page cap and then use
   stream-first mode (#26). A server ticket for the read is suggested (#52); make it
   before a big old org is a customer.
G. **Universal link hosts.** Default: `launchpad.example.com` only; add a
   wildcard of `*.example.com` if an operator runs more instances there (on Android
   the bare domain is listed too; the wildcard is unverified, #38), or use the
   `launchpad://` fallback.
H. **Linux CI.** Default: a Linux runner (a), decided in LAUNAPP-9 (#49).
I. **Apple and Google accounts, bundle ids, signing.** Not known here. Default: the account
   holder makes them before LAUNAPP-5 ends, because TestFlight is in LAUNAPP-7.
J. **Name of the Markdown renderer per platform.** Not chosen here (#36.5). Default:
   a native renderer in each UI code base that does not run HTML; chosen in LAUNAPP-6
   and LAUNAPP-8. (The web's choice is in `docs/internal/web-decisions.md`; the apps
   do not have to match it.)
K. **Local notifications on mobile in v1.** Default: yes, best effort (#46). If
   the project owner prefers no alert without push, remove the permission request and show the
   badge only.
L. **Names and Intel Macs.** The folders `shared/`, `composeUi/`, `apple/`, `android/`
   and `linux/` (#6, #61), and no `macosX64` slice (#56). Default: as written; add
   `macosX64` only if someone needs an Intel Mac.
M. **An app lock** (device passcode or biometric to open the app) as a setting.
   Default: not in v1; #17 and OS file protection cover expiry and loss.
N. **Visibility of the apps repo. Settled (project owner, 2026-10-28):** the apps repo
   is private, so it needs no licence now. If it ever goes public, it needs its own
   licence and its own list (the server repo's is `docs/internal/open-source-todo.md`).
O. **Runner trust. Decided (project owner, 2026-10-28).** Merge request jobs run on a shared shell
   runner with the CI variables of their project (#48). Options: an isolated executor, protected and
   scoped variables, or a runner per group. Decision: accept the risk as it
   is for the private repositories, make the release signing variables protected and tag-scoped,
   and revisit when more people can open merge requests.
