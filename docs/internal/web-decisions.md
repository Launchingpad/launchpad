> Internal design note from the Orbit deployment; kept for history.

# Web app decisions (ORB-3410, T6)

The web app is in `web/`. It talks to the T1 API only. The client types come
from `server/api/openapi.yaml` (`npm run openapi`). CI fails if they are old.

## Stack choices that differ from the brief

- React 18.3.1, as the brief says. React 19.3.0 is the latest.
- TypeScript 5.9.3. TypeScript 7.0.2 is the latest. It conflicts with
  `openapi-typescript` 7 and `typescript-eslint`.
- All dependencies are exact versions at least two weeks old. The install used
  `npm_config_before=2026-10-09T00:00:00Z`, so transitive packages obey it too.
- CSS Modules and one `tokens.css` (light and dark). No UI library.
- The `auth` module is hand-written and small (OTP request, verify, logout,
  session). It uses the generated types for the bodies. It calls `secureFetch`
  directly so that the CSRF guard applies.

## Security

- No token in `localStorage`. The session is an HttpOnly cookie. Its name is
  `__Host-odt_session` on a Secure server. It is the bare `odt_session` only on
  a LOG_ENV=local server.
  `localStorage` holds two preferences only: `devops-theme`, `devops-last-org`.
- Every unsafe request goes through `secureFetch` (`src/api/http.ts`). It adds
  `X-CSRF-Token`. The value comes from the in-memory token first. The CSRF
  cookie is the fallback. If the token is missing, it throws
  `CsrfMissingError` and sends nothing. The OTP request and verify routes are
  exempt. Logout is not.
- The CSRF token comes from the `csrfToken` field of the verify reply and of
  `GET /auth/session`. It is kept in memory and preferred to the cookie. The
  cookie is only a fallback, under either name: `__Host-odt_csrf` (Secure
  server) or `odt_csrf` (LOG_ENV=local).
- On a reload the app calls `GET /auth/session`. It rebuilds the user, the orgs
  and the CSRF token from that one reply.
- The dev bearer token (`VITE_DEV_TOKEN`) is read only when
  `import.meta.env.DEV` is true. It stays in memory. `vite build` throws if the
  variable is set. `scripts/check-dist.mjs` searches `dist` for it.
- nginx sends a strict CSP (`default-src 'none'`, scripts and styles from
  `'self'` only), `nosniff`, `DENY` framing, no referrer. `API_ORIGIN` and
  `FILES_ORIGIN` (the attachment store, ORB-3420) are the only extra origins,
  for `connect-src` and `img-src`: the browser PUTs uploads to the files host,
  and a download's 302 lands there (CSP checks the redirect target too). The theme script is an
  external file (`public/theme-init.js`) because the CSP forbids inline scripts.
- Markdown uses `react-markdown` with `rehype-sanitize` and no `rehype-raw`.
  Links accept `http`, `https` and `mailto`. Images load only from the API
  origin. Other images show as text. Links open with `noopener noreferrer`.
- The app never logs a token, a cookie or an OTP code.

## Data and live updates

- The org comes only from the route `/:org`. Every query key holds the org.
  A test proves that a switch changes the request path.
- Cache strategy: invalidate on event, not patch from event. One event feed per
  org (`EventSource`, `withCredentials`). Events are batched for 80 ms.
- `EventSource` cannot send `Last-Event-ID`. The client reconnects with
  `?after=<lastSeq>`, with backoff (30 s cap) and jitter. A `/me` probe stops
  the retries when the session has ended. The shell shows a "Reconnecting"
  pill.
- `EventSource` cannot send a bearer token. In dev bearer mode the client polls
  `GET events?after=` every 4 s. Cookie mode uses SSE.
- Edits are optimistic. This tab sends its writes to one issue ONE AT A TIME,
  in the order they were made (a first-in first-out line per issue). A write
  waits for the one before it (at most 10 s, then it goes on). A one-click edit
  (status, priority, assignee, labels) then sends the newest cached version,
  moved forward along this tab's own writes. A title or description draft is
  different. The page records the version when the draft starts, and the save
  sends that version, moved forward along this tab's own writes only. It never
  takes the newest cached version. So a live update that arrives while the
  draft is open gives a 409, not a silent overwrite.
- The own-write map, per issue, goes from the version this tab sent to the
  version the server returned. The client records a step only when the reply is
  exactly one version above what it sent. A bigger jump means another tab wrote
  in between, so nothing is recorded and an older draft gets a 409. The map is
  cleared on logout and when another user is found. A write by another tab is
  never in the map, so the draft base never moves past it, even when this tab
  also changed the issue. On 409 `version_conflict`, the detail page keeps the
  draft and offers "Reload latest". The list and board roll back and show a
  toast.
- The client does not send a one-click edit that would change nothing (same
  status or priority, same assignee, a label already there or already gone).
  It also records no step for it. Known residual risk, narrow window: a no-op
  that reaches the server can be answered with a re-read version that includes
  another tab's write made just after it. That reply looks like "one above".
  The proper fix is on the server (return the version inside the transaction).
  It is a separate server ticket.
- A session epoch goes up by one each time the signed-in user changes (login as
  another user, logout, or another user found by a refused CSRF retry). A write
  remembers the epoch it started in. If the epoch has moved before the write is
  sent (also after it left the line), the client throws `SessionChangedError`
  and sends nothing. A request also remembers the epoch it was built in. A late
  403 `csrf_failed` is never replayed after the epoch moved, so a retry made for
  user A cannot go out as user B.
- On 403 `csrf_failed` in the same epoch, `secureFetch` loads
  `GET /auth/session` WITHOUT side effects. It installs the new CSRF token and
  retries one time only if the same user is still signed in. If the user is
  different (or this tab did not know its user), it installs nothing and does
  not retry. It starts a new epoch, blocks all further unsafe writes
  (`SessionChangedError`, nothing is sent) and tells the page, which reloads the
  session query. The block ends when the session loads as the new user, or the
  person signs out. `POST /auth/logout` is the one write that the block does not
  stop: it only ends a session. While blocked, logout first loads the current
  session so that its CSRF token matches the cookie, then signs that session
  out. Concurrent 403s share one session load. If the session reply is 401, it
  does not retry. Other 403 codes are not retried.
- If `POST /auth/logout` fails (a 5xx or a network error), the app shows a
  message and keeps the local state. The in-memory CSRF token stays too, because
  the server session is still alive and the memory token is the only source on
  a cross-origin deployment. The token is dropped only after a 204 or a 401.
- A comment gets a new Idempotency-Key when the text changes after a failed
  post.

## UI choices

- Board drag and drop is pointer-driven (LAUN-29, last section): mouse, touch
  and pen, with a persisted order. Keyboard: the `S` status picker.
- Saved views (Active, Backlog, Urgent) are client-side presets over the list
  filters. The server has no saved-view route.
- The priority filter is one value, because the API takes one integer.
- The "priority" sort is done in the browser on the loaded page. The API sorts
  only by `updatedAt` or `createdAt`.
- Keyboard: `C` create, `J` and `K` move, `Enter` open, `X` select, `S` status,
  `A` assign, `L` label, `P` priority, `?` help, `Ctrl/Cmd+K` palette. Plain keys
  do nothing while you type, with a dialog open, or with a modifier held.
- Layout rule: grid and flex children use `minmax(0, 1fr)` / `min-width: 0`. Long
  strings (branch names, URLs, code) wrap, or scroll inside their own block. On a
  phone the issue page shows breadcrumb and title first, then a Properties block,
  then the description. `npm run check:overflow` (browser audit at 375 and 320 px
  on login, list, detail, board, create, palette, drawer) checks every element
  against the viewport and its clipping parents. It needs Chromium, so CI does not
  run it. `src/styles/layout.test.ts` guards the CSS rules in CI.
- At 375 px the sidebar becomes a drawer, and the board scrolls sideways in its
  own container. The page itself does not scroll sideways.

## First-run setup page (ORB-3433, part B of ORB-3431)

1. **`/setup` is a public route**, outside `RequireAuth`. A signed-out
   person must not be sent to `/login` first. nginx already serves
   `index.html` for it (`try_files $uri /index.html`).
2. **The token is read from `location.hash` once**, then removed with
   `history.replaceState`. It lives in memory only (a module variable and
   component state). It is never in storage, never in a message, and goes
   only in the `POST /setup` body. The page checks its shape (43 base64url
   characters) before it sends anything.
3. **`POST /setup` is exempt from the CSRF header**, like the OTP routes:
   there is no session yet, and the server ignores cookies on it. The 201
   starts a session, so `completeSetup` stores the CSRF token in memory and
   notes the user, as a login does.
4. **The page asks `GET /setup` first.** `setupRequired: false` shows "Setup
   is already done" with a link to `/login`, even with a token in the URL.
   If that call fails, the form still shows and the POST decides.
5. **Checks run in the browser first** with the server's rules (username,
   email, slug, team key, name lengths). The server stays the judge: a 400
   message that starts with `user.email`, `org slug`, `team.key` and so on is
   shown under that field; any other 400 shows as a banner. A 409 `conflict`
   (slug in use) is a slug error. 401 `invalid_setup_token`, 409
   `setup_complete` and 429 (with the `Retry-After` wait) have their own
   states. A 400 or a 429 keeps the form, because the link is not used up.
6. **The first admin token is shown once.** The page warns, offers a copy
   button, asks for a confirmation box, and asks the browser to confirm a
   close while the token is on screen. Only after the box is ticked does the
   page put the session in the query cache and go to the app. The secret is
   dropped from state at that moment. A web session cannot make an admin or
   no-expiry token (decisions-T11), so this is the one place a person gets a
   first one. Other tokens: "Agents and API tokens" below.
7. **`isPlatformAdmin`** comes with the session (`GET /auth/session` and the
   login and setup replies: `LoginUser`). No server change was needed. The
   settings area uses it (ORB-3446, ORB-3448).

## Settings area (ORB-3446)

1. **`/:org/settings`** has a left nav: Organisation, Teams, Members and, for
   a platform admin only, Platform. The bare address redirects to Organisation,
   or to Platform for a platform admin who is not an org admin (ORB-3448).
2. **The role comes from the session.** `GET /auth/session` returns
   `orgs[].role` (`admin` or `member`) and `user.isPlatformAdmin`. The app adds
   no server route for this.
3. **"Settings" shows to org admins and platform admins** (ORB-3448; it was
   org admins only), in the org menu and in the user
   menu. An "Admin" badge sits next to the name in the user menu. A member who
   opens the address gets "You need admin access." and no nav. A person who is
   not a platform admin and opens `settings/platform` gets "You need platform
   admin access." The server judges every write. The gate only hides pages.
4. **Organisation is read only.** The API has no rename route.
5. **Teams** lists key and name and creates a team with `POST /orgs/{org}/teams`.
   The key is upper-cased as typed. Checks run in the browser first (the
   server's rules). A 409 shows as an error on the key field. A 400 that names
   `key` or `name` shows on that field. The create mutation invalidates the
   teams query, so the sidebar list updates with no reload.
6. **The member list is read only** (Members also has the invites, ORB-3450 below) and uses the existing `GET /orgs/{org}/users`
   (not paginated on the server). It hides deactivated people, like the
   assignee pickers. The server sends an email only to an org admin, and this
   page is admin only, so the column is filled.
7. **Platform** was a placeholder here. ORB-3448 built it: "Platform settings
   (ORB-3448)" below. The access rule of item 3 changed there too.
8. **Focus.** Each page title takes focus when the page appears, like the
   setup page, so a screen reader starts at the new page after a route change.

9. **Team menu in the sidebar (LAUN-15).** Each team row has a `...` button
   (`aria-label` "<team name> team options", a `Menu` without the chevron)
   with one item, "Team settings", that goes to `/:org/settings/teams`. It
   shows to org admins only, not to every person who sees Settings: a platform
   admin who is not an org admin cannot open the Teams page (item 3 and the
   settings layout), so the item would lead to "You need admin access." The
   Teams page has no team picker, so the item opens the list for every team.
   The button is hidden until hover or focus on a pointer device and always
   visible on a touch device. The row is `TeamNavItem.tsx`.

## Agents and API tokens (ORB-3447)

Rules on the server: `docs/internal/decisions-T11-web-tokens.md`.

1. **Two new settings pages**, for org admins only, with the same gate as the
   others: **Agents** (`/:org/settings/agents`) and **API tokens**
   (`/:org/settings/tokens`).
2. **Agents** lists the members with `isAgent` (the existing members query)
   and the number of live tokens each has (not revoked, not expired). The
   form makes an agent with `POST /orgs/{org}/agents`: username (lower-cased
   as typed) and name. A 409 shows on the username field. A 400 that starts
   with `username` or `name` shows on that field. After the create, the page
   offers "Create a token for this agent". Each row has a button for the same.
3. **API tokens** lists the live tokens: name, owner (an "Agent" tag), access,
   created, last used, expires. Revoked and expired tokens are not listed.
   Dates are UTC, `YYYY-MM-DD`. The form picks the owner (me, or an agent),
   read only or read and write, and the days. Defaults: a person gets read
   only and 90 days, an agent gets read and write and 365 days. A choice the
   person made is kept when the owner changes. The maximum is 365 days. A
   session cannot make the admin scope, so the form has no admin choice.
4. **Revoke asks first** in a dialog ("stops working at once. You cannot undo
   this."). A failure shows a fixed text and keeps the dialog open.
5. **The copy-once screen is a modal dialog**, in the same flow as the setup
   page: a warning, a copy button, the "I saved the token" box that gates
   Done, and a `beforeunload` guard while it is open. A dialog, not a page,
   because the rest of the app is then inert: a click on the sidebar cannot
   throw the only copy away. Escape, the close button and a click outside do
   not close it before the box is ticked; they show a hint.
6. **The secret lives in component state only.** The create call is a plain
   async function (`useCreateToken`), not `useMutation`, because the
   mutation cache keeps a result for minutes. The secret is never in web
   storage, a URL, a log, an error text or any later request.
   `tokens.test.tsx` checks each of these, and the query and mutation caches.
7. **Fixed error text.** A token or agent create never shows server text.
   A 400 maps to a field and its fixed rule, a 403, a 429 and the rest have
   fixed sentences. (The list pages use the shared `ErrorState`, as the other
   settings pages do.)
8. **MCP setup** sits under the secret: `LAUNCHPAD_URL` (the current
   origin, or `VITE_API_BASE`), `LAUNCHPAD_TOKEN` (the text says to paste
   the token; the token is not repeated there), `LAUNCHPAD_ORG` and
   `LAUNCHPAD_TEAM` (the first team key, or a placeholder until the
   teams load).

## Platform settings (ORB-3448)

Rules on the server: `docs/internal/decisions-T9-mail-settings.md`.

1. **Who may open Settings.** An org admin, or a platform admin. Before
   ORB-3448 only an org admin could, so a platform admin who is only a
   member of the org (the ORB-3446 reviewer note) could not reach Platform.
   The server does not need the org role for the platform routes. It needs
   the platform flag, the admin scope and no org pin
   (`auth.RequirePlatformAdmin`). A web session has no org pin, and it gets
   the admin scope for an org admin or a platform admin
   (`login.SessionScopes`). So a cookie session of such a person passes. The scopes are fixed when
   the session starts, so a person made a platform admin after they signed
   in must sign in again.
2. **What that person sees.** "Settings" shows in both menus. The settings
   address opens Platform. The nav has one item, Platform. An organisation
   page shows "You need admin access." The "Admin" badge stays for an org
   admin only. A person who is neither gets the same notice as before.
   `useCanOpenSettings` in `access.ts` is the gate. The server judges every
   call.
3. **One form, saved as a whole.** The page loads `GET /platform/settings` and
   saves with `PUT`. It sends every non-secret field each time, as the server
   requires (T9 #14), also the fields of a provider that is not chosen. A
   field of the chosen provider only is on screen. The Save button is off
   until something changed.
4. **Mail provider** is a list: Server environment, None, Postmark, SMTP,
   with one short line for each. Postmark shows token, sender and stream.
   SMTP shows host, port, security, username, password, sender address and
   sender name. A port of 0 or an empty box means the default, 587.
5. **Secrets are write-only.** A secret box is always empty. Next to it the
   page says "Set" or "Not set". Typing a value replaces the secret. "Clear"
   sends `""`. "Keep it" undoes a clear. A secret that was not touched is left
   out of the `PUT`, so the server keeps it (T9 #11). A secret typed for one
   provider is dropped when the person picks another. The boxes are empty
   again after a save. When the server has no `SETTINGS_ENCRYPTION_KEY`, the
   secret boxes are off and the page says why.
6. **No secret in a cache.** The save is a plain async function
   (`useSavePlatformSettings`), not `useMutation`, like `useCreateToken`.
   The mutation cache would keep the secret. The reply has no secret, so it
   goes in the query cache. The settings query does not refetch on focus,
   because a refetch could drop what the person typed. A secret is never in
   web storage, a URL, a log or an error text. `platform.test.tsx` checks the
   query cache, the mutation cache, storage, the address and the page text.
7. **Fixed error text.** The page never shows server text, also not when the
   load fails (a 403 says to sign out and in again). A 400 shows a fixed
   sentence and the fixed labels of the fields that the message names. A 409
   (no encryption key), a 403, a 429 and the rest have fixed sentences.
8. **Login status.** `loginAvailable` false shows a red alert at the top:
   "Login is off. Nobody can get a sign-in code." If there is no transport,
   it says to choose Postmark or SMTP, or to set `POSTMARK_SERVER_TOKEN`. If
   there is a transport, it says that `AUTH_OTP_PEPPER` is missing. A line
   under it says what login email goes through now (`activeMail`). A second
   alert shows for `secretsUnreadable`.
9. **Send test email** calls `POST /platform/mail/test`. It uses the saved
   settings, so the button is off while the form has unsaved changes. The
   page shows the address from the reply on success. Each failure has a
   fixed text: 503 (login email is off), 502 (the mail server did not accept
   the test), 400 (your account has no email address), 429, 403 and the rest.
   The text of a 502 never reaches the screen, because it comes from a
   mail server.
10. **Sign-up mode** is three radio buttons with one line each: Invite only,
    Approval, Closed. The server uses a saved mode at once (decisions-T12 #1).
    Since ORB-3450 the page says what each mode does and links to the Members
    page (invites), the sign-up page and the Sign-ups page (requests). The
    Platform page also lists the platform admins (ORB-3450 below).
11. **Not done.** A confirm step before "None". It turns login off for
    everyone, but it does not end a session that is open.

## Create issue dialog (ORB-3452)

The dialog looks and works like the Linear "New issue" window. The API calls
and the checks are the same as before. No server change.

1. **Layout.** The header has the team chip (key and name, for example `ORB · Orbit`; the
   name is cut with `…` on a narrow screen), a `›`, the words "New issue",
   an expand button and a close button. Below are a large title field and a
   description field. Both have no border. The title has focus on open. One
   row of chips follows. The row wraps on a narrow screen. The footer has the
   "Create more" switch and the "Create issue" button. The Cancel button is
   gone: the close button and Esc do that job. There is no attach button,
   because the creator did not support attachments before.
2. **Chips.** Each chip is a button that opens a list in a popover: Status
   (state icon), Priority (icon), Assignee (avatar), Labels (colour dots,
   many choices, and "Create" for a new name), Parent issue (server search),
   and Cycle. The Cycle chip shows only when the team has cycles. It lists
   the cycles that are not complete. The `...` chip opens a small panel with
   Due date and Estimate. The API accepts both fields. A number on the `...`
   chip shows how many are set. The team chip opens a list only when the org
   has more than one team. A new team clears Status and Cycle, because they
   belong to a team.
3. **One popover part.** `ui/ChipPicker.tsx` shows the chip and the list.
   `ui/Floating.tsx` places the panel. The list is the `PickerList` from
   `ui/Picker.tsx`. No new package was needed.
4. **Position.** The panel is drawn in a portal inside the dialog node
   (`[role=dialog]`) with `position: fixed`. The scroll area of the dialog
   cannot cut it. It is inside the node because an `aria-modal` dialog can hide
   everything outside its own node from assistive technology. The focus trap of
   `Dialog` skips nodes inside a `[data-floating]` panel. The panel
   opens below the chip. It opens above the chip when there is less room
   below than above. It stays 8 pixels inside the screen edges. It is
   placed again on resize and on scroll.
5. **Keyboard and ARIA.** The chip is a button with `aria-haspopup="listbox"`
   and `aria-expanded`. The search box is a `combobox` that controls the
   `listbox`. The options are `option` with `aria-selected`. A check mark
   shows each chosen option. Arrow keys move, Enter chooses, typing filters.
   A one-choice list closes after a choice. The Labels list stays open.
   Focus goes back to the chip when the list closes by Esc, Tab or a choice.
6. **Esc order.** Esc in a popover closes only the popover. The next Esc
   closes the dialog. A click outside the popover closes the popover and does
   not close the dialog. If the click is on the dark area, the dialog stays
   open. A second click there closes it.
7. **Shortcuts.** `S`, `P`, `A` and `L` open Status, Priority, Assignee and
   Labels. They work only while the dialog is open, no popover is open and
   no text field has focus (`isTypingTarget`). A key with Ctrl, Cmd or Alt
   does nothing. Ctrl or Cmd with Enter creates the issue from any place in the
   dialog: the title, a chip, the header, the footer or an open popover. The
   dialog root handles the key. In a popover list Ctrl or Cmd with Enter does
   not pick the marked option and does not make a new label. The shortcut
   help lists both.
8. **Create more.** The switch is off at each open. When it is on, a good
   create shows the toast and keeps the dialog open. The title and the
   description are cleared. All other properties stay. The title gets focus.
   The dialog makes a new Idempotency-Key, because the old key would return
   the first issue again. A failed create keeps the text and the key, so a
   retry cannot make two issues.
9. **Status default.** The Status chip shows the default state of the team
   (`defaultStateId`). Before ORB-3452 it showed the first state. The create
   call still sends no state until the person chooses one.
10. **Tab.** In the Due date and Estimate panel Tab moves from field to field.
    The panel closes only when focus would leave it: Tab from the last field,
    or Shift+Tab from the first. A list popover has one field, so Tab closes it.
11. **Estimate.** The API takes a whole number from 0 to 1000. The field has
    `min`, `max` and `step`, and the dialog checks the value again at create:
    "Estimate must be a whole number from 0 to 1000." No call is made.
12. **Toasts.** A dialog makes `#root` inert. The toast region is now a portal
    on `<body>` with `data-live-region`, and `Dialog` does not make it inert.
    So the "ORB-N created" text is announced and its Open link works while the
    dialog stays open (Create more).
13. **Search box focus.** `PickerList` now puts focus in its search box when it
    opens. This holds for every list that uses it: the chips here, and also
    the pickers in the issue detail page, the filter bar and Relations. A
    `Dialog` reads the focused element in its first render, before any child
    takes focus, so a quick picker (`s`, `a`, `l`, `p`) still gives focus back
    to the element that opened it.
14. **Resize.** A `ResizeObserver` places the panel again when its height
    changes (a filter), so a panel above the chip stays against the chip.
15. **Focus trap and Tab.** A Tab that leaves a panel closes it and goes on to the
    dialog, so the trap of `Dialog` sees a Tab that starts on the chip. Shift+Tab
    from the team popover (the first control) then wraps to the last control and
    does not leave the modal. A Tab that stays in the panel does not go on. A date
    input has segments, so Tab in it is left to the browser; the panel closes when
    focus really leaves it (blur).
16. **Opener of a dialog.** If the opener read at render time is gone when the
    dialog opens (the palette closes and opens "Create issue" in one step), the
    dialog uses the element that has focus when its effect runs. The palette has
    given focus back by then. Esc on the new dialog returns focus there.
17. **Input method.** Enter or Esc that ends an input-method composition
    (`isComposing`, or key code 229, which Safari sends with `isComposing`
    false) does not act. Guarded: Enter in a list (`PickerList`), Esc in a panel
    (`Floating`), Enter in the title, and Esc in the dialog (title and
    description). The description has no Enter rule.
18. **Announcements.** The toast region is on `<body>` (point 12). A success
    message is also written to a hidden polite live region inside the dialog
    ("Created ORB-N."), so it is announced where an `aria-modal` dialog hides
    the toast. Verified: the DOM places (tests). Not verified: VoiceOver and
    Safari were not run. The dialog and the panel were checked in Chromium
    (Brave) only.
19. **Ctrl or Cmd with Enter in every list.** `PickerList` now ignores
    Ctrl/Cmd+Enter. In the quick pickers (`s`, `a`, `l`, `p`), the filter bar and
    the issue detail page it used to choose the marked option. It now does
    nothing there, and the key goes on to the dialog or page.
20. **Press inside a panel.** A press on text, padding, a button or an empty-state
    row of a panel does not move focus (the dialog node has `tabindex="-1"` and
    would take it). Only inputs, selects, textareas and options keep their own
    focus. The click on a button still fires. The panel stays open and Esc closes
    only the panel. A label still focuses its field. "Clear both" moves focus to
    the Due date field, so focus never sits on a button that just disabled
    itself. If focus still falls to the body or to the dialog node while a panel
    is open, the panel takes it back (first field, else the panel node), so Esc
    always has the panel in its key path and the dialog keeps the title. A focus
    move by keyboard out of a panel (for example Shift+Tab out of the Due date
    field) closes it and puts focus back on its chip. Ctrl or Cmd+Enter that ends
    an input-method composition does not create. A press on a chip whose panel is
    open does not move focus, and the click that closes the panel puts focus on
    the chip first, so focus never drops to the body. With Create more on, the
    panel unmounts before focus goes to the title.
21. **Double announcement.** With Create more on, a screen reader that does not
    prune outside an `aria-modal` dialog can say the success twice: the toast and
    the hidden status in the dialog. This is kept on purpose: a missed success
    is worse than a repeat. It is not measured.
22. **Not done.** Linear shows issue counts on some options. The API has no
    cheap count, so Launchpad shows none. The Project and Milestone fields are
    not in the dialog.

## Server gaps

Closed by ORB-3414 (merged): `GET /auth/session` (with `csrfToken`), `displayName`
and `email` in `/me`, `Retry-After` on 429 (exposed through CORS), cookie names
and CORS rules. The web app uses all of them. The workarounds are gone.

Still open (the client works around each one):

1. **EventSource cannot send a bearer token.** Fine for cookies. Dev bearer
   mode polls `events?after=` every 4 s.
2. **No org create route.** The app shows only the orgs from the session.
3. **List filters.** No multi-priority filter and no priority sort. The client
   sorts in the browser.
4. **Login user field.** The login reply has `name`, not `displayName`. The app
   uses `name`. (`/me` has both.)

## Proof status

Real, against a local server (compose project `-p dev-ops-orb3410`,
`LOG_ENV=local`, `AUTH_FIXED_OTP=000000`, seeded user `admin@example.com`),
with `scripts/real-flow.mjs` (Chromium driven by puppeteer-core, Vite dev server
as the same-origin proxy, no mocked fetch, no dev token in the browser):
email OTP login, reload keeps the session, a cookie write with no CSRF header is
refused (403 csrf_failed), the same write with it works, a comment written in
the UI is read back from the server, cookie-mode SSE shows an issue created
elsewhere with no reload and no polling, logout clears both cookies and
`GET /auth/session` returns 401. The 429 `Retry-After` countdown was also seen
for real.

Not real, or not proven:

- The `__Host-` cookie names and the Secure cookie flag. The local server uses the
  bare names on plain http. The `__Host-` name is covered by unit tests of the cookie fallback only.
- A cross-origin deployment (CORS with credentials). The proof used the Vite
  proxy, so the browser saw one origin.
- Real email delivery (Postmark). The fixed code `000000` replaced it.
- Org switching with two real orgs. It is tested with mocked fetch only. The
  local server has one org.
- The component flow tests (Testing Library) stay mocked. They remain as
  regression tests.

## Invites, sign-ups and platform admins (ORB-3450)

Rules on the server: `docs/internal/decisions-T12-signups-invites.md`. The server half
is ORB-3436. No server change was needed.

1. **New routes.** Public: `/signup` and `/invite#<token>`, outside
   `RequireAuth`. Settings: `/:org/settings/signups` (Sign-ups, org admins).
   Settings, Members now has the invites. Settings, Platform now has the
   platform admins.
2. **The three public calls are CSRF-exempt** (`POST /signups`,
   `/signups/confirm`, `/invites/accept`), like the OTP routes. A signed-out
   person has no CSRF token, and the server reads no cookie on these routes.
   The admin routes next to them are not exempt (`http.test.ts` checks both).
   They send no `Idempotency-Key`, because the server answers 400 to one.
3. **Fixed text only.** No error text from the server reaches the screen on
   any of these pages, also not on a failed list load (`FixedError`). A 409
   from invite create or approve has several causes with one `conflict` code;
   `joinText.ts` reads the server message only to choose a fixed sentence.
   Tests plant a marker in every server error and look for it on the page.
4. **Invites (Members).** The form takes an email and a role. When the server
   answers `emailed: true`, the 201 has no link, and the page says "The invite
   is sent by email to ...". When mail is off, the 201 holds the link once and
   the page shows it in a copy-once dialog, built like the token dialog of
   ORB-3447 (decisions 5 and 6 there): modal, the confirm box gates Done,
   Escape and the close button do not close it first, `beforeunload` guard,
   `useCreateInvite` is a plain async function and not `useMutation`, so the
   link is in no cache. A bare path from the server (`/invite#...`, no
   `PUBLIC_BASE_URL`) gets the page origin in front. The list shows status
   and "Sent by email" or "Link shown once", hides revoked invites, and has
   Revoke (with a confirm) for pending ones. The list holds no token, so
   "copy link" is possible only at creation. The tag says "Link shown once"
   because the data only says the invite was not emailed. It does not say
   the person copied the link. Browser Back while the dialog is open leaves
   the page and loses the only copy of the link, as for the token dialog.
   Revoke the invite and make a new one.
5. **The sign-up mode is not known to an org admin.** `GET /platform/settings`
   needs the platform flag. A platform admin sees the mode and, when it is
   `closed`, a notice instead of the form. Any other org admin sees the form
   and learns the mode from a `403 signup_unavailable`, which then shows the
   same notice. Sign-ups works the same for approve.
6. **Sign-ups page.** It lists pending requests: name, username, email, note,
   age. "Email unverified" shows when `emailVerified` is false, with a line
   about the first sign-in. Approve sends no body. Reject opens a dialog with
   an optional reason (1000 characters) and an optional block of the email.
   "Remove unconfirmed requests" asks first, then shows the count from
   `DELETE /orgs/{org}/signups/unconfirmed`. The page says in plain text that
   unconfirmed requests wait for the email code and are hidden while mail
   works (decisions-T12 #8 and #10). An approve that gets 409 "not confirmed",
   a taken username, or 429 shows its own fixed sentence.
7. **Platform admins.** The list shows name, username, email and a
   "Deactivated" tag. Add takes a username or an email. Remove asks first. The
   server's 409 for the last active admin shows as "You cannot remove the
   last platform admin. Add another platform admin first." A person who is
   added must sign in again (the scopes are fixed at sign-in, decisions-T12
   #23), and the page says so. A platform admin who removes themselves sees "You are
   no longer a platform admin." in place of the page, and the cached session
   drops the flag (the sign-in scopes stay until the next sign-in).
8. **Public sign-up page.** Fields: email, name, username, note (optional).
   The browser checks the server's rules first. After the 202 the page shows
   one fixed "Check your email" step, whatever the address is (decisions-T12
   #6): it shows a code box, "Send a new code" (the same request again) and
   "Use another email address". The server keeps the name, username and note
   of the FIRST request for an email address and only mails a new code
   (decisions-T12 #7). So the button goes back to the form with the values
   kept, and the form shows one fixed line: a new name, username or note for
   the same email address is not used; only a different email address makes
   a new request. A test compares the page text for three addresses.
   The code goes only in the `POST /signups/confirm` body and is dropped from
   state after the call. Confirmed: "An admin must now approve your request."
   The page starts no session. A 403 `signup_unavailable` replaces the form
   with "Sign-up is not open". A 429 shows the wait. The page adds no timing
   trick; the server pads every answer to 300 ms.
9. **Public invite page.** The token follows the setup page rules (decisions
   2 of ORB-3433): read from `location.hash` once, removed with
   `history.replaceState` at once, in a module variable and state only, sent
   only in the body of `POST /invites/accept`, dropped on success and on a 401. The form has name and username. The page cannot know whether the
   email has an account, so it says: a person with an account leaves both
   boxes empty. Only the token is sent then. A 201 (new account) puts the
   session in the query cache, keeps the CSRF token in memory and opens the
   org. A 200 `joined` starts no session: "You joined", with a link to the
   login page. One generic text covers every 401 (wrong, expired, used,
   revoked). A 400 keeps the form and the token. A 409 is an error on the
   username field.
10. **The login page always links to the sign-up page.** No public route
    tells a signed-out browser the sign-up mode (`GET /setup` has
    `setupRequired` only). So the link "Ask to join" is always there, and the
    sign-up page shows its plain notice when the mode is not Approval. The
    person fills the form before they see that notice. This is a known gap.
    The fix is a public mode field on the server (ORB-3457, public `signupMode`).
11. **No team.** An org can have no team (made by hand; setup always makes
    one). The issues page then shows "This organisation has no team yet". An
    org admin gets the create-team form there (`TeamForm`, the same one as
    Settings, Teams). A member is told to ask an admin. The Teams form now
    shows fixed text for an unknown failure (it showed the server text).

## Cycle list and cycle page (LAUN-13)

Code is in `web/src/features/cycles/`. The burn-up chart is LAUN-14.

1. **Sidebar:** a "Cycles" link under a team, only when `team.cyclesEnabled`.
   The link is `CyclesNavLink`. `TeamNavItem` (LAUN-15) renders it right after
   the team row, so a team shows its link, its `...` menu (org admin) and the
   Cycles link together. The team link uses `end`, so it is not active on a
   cycle page.
2. **Routes:** `/:org/team/:team/cycles` (list) and
   `/:org/team/:team/cycle/:number` (one cycle). The team is the key, any case.
3. **Sections:** the API has no "current" field, so the page decides from the
   dates. Current: `startsAt <= now < endsAt` and not completed. Next: starts
   later (soonest first). Past: completed, or `endsAt` passed (latest first;
   10 shown, a button shows all). An ended cycle that the hourly job has not
   completed yet is already past.
4. **Progress:** the `Cycle` object holds no counts. A row reads the newest
   row of `GET /cycles/{id}/snapshots` (done = `completedCount`, scope =
   `scopeCount`, which leaves out canceled issues). The server writes today's
   row of a running cycle on that call, so the web keeps the calls few: one per
   current or past row; a next cycle never has rows (decisions-T3 #19), so it
   shows "Not started" and sends nothing. The query key is NOT under the org's
   `issues` key, so an issue event does not refetch every row. A current cycle
   goes stale after 2 minutes; a past cycle never (its rows are final). A failed
   call shows "Progress unavailable" on that row; no snapshot shows "No data yet".
5. **Cycle lookup:** `useCycles` follows `nextCursor` to the end (50 per call),
   so a team with imported history has every cycle. An unknown number, or one
   that is not a plain positive number (`abc`, `03`), shows "Cycle not found"
   and asks for no issues. A team with cycles off shows "Cycles are off".
   Current / next / past use the clock, re-read each minute (`useNow`), on both
   pages.
   5a. **Dates:** shown as calendar days in the team's `cycleTimezone`, not the
   browser zone. `endsAt` is the next cycle's start (exclusive), so the shown
   last day is the day before it: consecutive cycles read "11 Oct to 24 Oct",
   "25 Oct to 6 Nov".
   5b. **Known gap:** the sidebar Cycles link does not refresh live when a team
   turns cycles on (no `team.updated` event handling yet). A reload shows it.
6. **Cycle page issues:** `GET /issues?team=<key>&cycle=<number>`, grouped by
   status with `groupByState` and `IssueRow`, the same as the issue list. It
   has no filter bar, board, hotkeys or bulk pickers yet; the page keeps only
   row focus and selection checkboxes. (LAUN-18 changed this: see its section.)

## Create issue dialog closer to Linear (LAUN-19)

These points change the ORB-3452 dialog (its section above). Where they differ from
ORB-3452 points 1 and 2, these points win. The numbers below are this section's own.

1. **Frame.** The dialog uses `Dialog plain`: no divider line under the header
   or above the footer. The title has no focus box; the caret shows focus.
   Chips are round and quiet (muted text until a value is set).
2. **Team pill.** The API has no team icon (`Team` has no such field), so the
   header keeps a clean text pill: key and name. Add an icon when the API has one.
3. **Chips.** Order: Status, Priority, Assignee, Project, Labels, Cycle, `...`.
   Parent moved into `...`: the panel has a "Parent issue" row that opens the
   same server-search list on the `...` chip. A parent shows as "parent ORB-7"
   in the `...` chip name and count.
4. **Project.** Lists the projects of the chosen team (`GET /projects?team=`,
   not archived) and sends `projectId`. A new team clears it.
5. **Cycle.** A real picker (shown only when the team has cycles on). It lists
   "No cycle", the last two finished cycles, the running one and the coming
   ones. It shows the cycle name or "Cycle N" and sends `cycleId`. The running
   cycle (started, not ended, not complete) is preselected once the cycles load.
   After the person chooses anything, including "No cycle", it is not
   preselected again. A new team resets this.
6. **Attach.** The round paperclip button is at the bottom left. The T3b flow
   needs an issue to attach to, so files are queued (name list under the chips,
   each with a remove button) and uploaded after the issue exists (prepare, PUT,
   attach). Only the T3b allowlist and 100 MiB are accepted; others are refused
   at once. If an upload fails, the issue stays created and the toast names the
   files that were not attached. Create more clears the queue.
7. **Footer.** Paperclip left; "Create more" and "Create issue" together on the
   right. The Create button keeps the primary colour (no disabled look when the
   title is empty, because the empty-title message must still show).
8. **One submit at a time.** A busy flag (a ref, so Enter and Cmd+Enter see it at
   once) covers the create call AND the uploads. The button is disabled and says
   "Creating…" then "Uploading…". The file queue is copied and emptied before the
   uploads start, so no file uploads twice.
9. **Half-created issue.** After the create works the issue is remembered. If a
   file fails, the issue stays, the failed files stay in the queue, the dialog
   stays open (also with Create more) and a banner names the files. The button
   then says "Retry attach"; it attaches the queued files to that issue and never
   creates a second one. Removing all files and pressing it ends the flow.
10. **Close during upload.** Closing is allowed. The uploads go on. When they end,
    a form that is gone sets no state and closes nothing (so a newer dialog stays
    open); a failure then shows as a toast that names the files.
11. **Projects.** Completed and canceled projects are not offered.
12. **Locked once created.** When the create worked but files failed, the title and
    description are read-only, the team chip is disabled and the chip row is
    `inert`; the shortcut letters do nothing. Only the files can change (retry or
    remove). The banner says so.
13. **Queue edits.** The Attach and remove buttons are disabled while the dialog
    is busy. Queue updates are functional (`setFiles(cur => ...)`) and failed
    files are matched by `File` identity, not by name, so two files with one name
    cannot mix up.
14. **Close after a failed attach.** A close with the issue created and files
    still queued shows a toast: "ORB-N was created. Not attached: names".
15. **Focus ring.** `.attachBtn` and `.attachRemove` use the global `:focus-visible`
    rule (global.css). The title and description keep no outline on purpose.
16. **Idempotency key after a refusal.** The server stores a 4xx answer under its
    key. So after a 4xx (not `idempotency_in_progress` or
    `idempotency_outcome_unknown`) the dialog makes a new key, and the corrected
    form can be created. A network error or a 5xx keeps the key.
17. **Create error after close.** If the dialog was closed while it showed
    "Creating…" and the create failed, a toast says: Could not create "title": reason.
18. **Focus when locking.** If focus is in the chip row when the form locks, it
    moves to the title first, so Esc, the Tab trap and Cmd+Enter still work.

## Cycles sidebar and cycle switcher, like Linear (LAUN-17)

1. **Sidebar:** `CyclesNavLink` is now "Cycles" (the list) with two children,
   "Current" and "Upcoming", in a group with a thin rule at the left; the
   active child has the gray pill (`.active`). Only when the team has cycles
   on. Always open: there is no collapse toggle, because a toggle that hides
   two links gains nothing.
2. **Children are redirect routes:** `/:org/team/:team/cycles/current` and
   `/cycles/upcoming` (`CycleJump`) find the running cycle, or the soonest
   cycle that has not started, and `Navigate replace` to `/cycle/:number`. The
   link never goes stale, and the sidebar needs no cycle data. With none, the
   page says "No current cycle" / "No upcoming cycle" and links to the list.
3. **Active state:** the cycles are read in the sidebar only while a cycle page
   of that team is open (the query is shared with the page, so no extra call).
   Current lights on the running cycle, Upcoming on the soonest next cycle. A
   past cycle, or a later next cycle, lights "Cycles" only. The list page
   lights "Cycles". Only one of the three has `aria-current`.
4. **Header:** breadcrumb `Team > Cycles > Cycle N` (a `nav` with an `ol`;
   the `>` is CSS content with an empty alt text, so it is not read out; the
   last item has `aria-current="page"`). The `h1` is plain text "Cycle N". Next
   to it is a `Menu` button "Switch cycle" (a chevron). The menu has the groups
   "Next cycle" and "Previous cycle", each with the cycle after / before THIS one
   by start date (one each; a group with no cycle is left out). The labels are
   neutral on purpose: the neighbour may not be "upcoming" or "completed" (a gap,
   or a past cycle the job has not completed). Each entry has the icon, the name,
   the dates ("Nov 9 - Nov 22", team zone, last day inclusive) and the shortcut.
   The last item "All cycles" opens the list. `Menu` got optional `detail`,
   `hint` (also `aria-keyshortcuts`), `wrapClassName` and `panelClassName`. After
   a pick, focus returns to the button (as after Esc). `Menu` has a `closeKey`
   (the cycle id), so Alt+K / Alt+J close an open menu.
   4a. **Phone (480 px and narrower):** the panel is positioned against the header
   and spans it, 8px in from each side, so it cannot pass the screen edge. The
   shortcut hints are hidden there and on touch screens (`hover: none`).
   `cycles.layout.test.ts` guards these rules (jsdom has no layout).
5. **Keys:** Alt+K opens the next cycle, Alt+J the previous one, on a cycle page
   only. They are ignored in a field, on key repeat, with Ctrl, Meta or Shift,
   and while any dialog is open. They use `event.code`, because Option+K types
   another character on a Mac. Listed in the `?` help.
6. **Star and `...`:** left out. Launchpad has no favourites and the page has no
   actions for a menu, so they would be dead buttons.
7. **Cycle change:** the issue list is keyed by cycle number, so selection and
   focus do not carry over to the next cycle. `CycleJump` waits for a running
   refetch before it redirects, so it never uses stale cycles.
8. **Cards:** the page still uses `IssueRow` as it is (LAUN-16 owns the card
   look). A board by state is not part of this change.

## Issue list and board on a phone (LAUN-16)

Code: `FilterBar.tsx`, `IssueRow.tsx`, `BoardView.tsx`, `issues.module.css`.
The breakpoint is the existing 760 px one. Above it nothing changes: the two
filter wrappers (`.fold`) are `display: contents` and the card date is hidden.

1. **Toolbar:** below 760 px the bar is one row: Search (fills the row), a
   `Filter` button (shows `(n)` for active filters) and the list/board toggle.
   The Status/Assignee/Label/Priority chips, Clear, Sort and Select fold
   behind the Filter button (`aria-expanded`, `aria-controls`). A closed fold
   is `display: none`, so its controls are out of the tab order.
2. **Rows:** two lines. Line 1 is the title (wraps to two lines, then `...`)
   and the assignee. Line 2 is priority, id, status icon, labels and age.
   Padding is 12 px. The id and the status icon show again on a phone.
3. **Checkbox:** on a phone it shows only in select mode. Select mode is the
   `Select` button (in the fold, `aria-pressed`) or any selected issue. Turning
   `Select` off clears the selection. The `x` key and `Escape` work as before.
   The button is the smallest way in that needs no long press. In select mode
   the page head shows on a phone (the h1 stays hidden) with "N selected" and
   Clear. `Escape` also leaves select mode. Bulk actions (status, assignee,
   label, priority) stay hotkey-only: there is no phone button for them yet. The cycle page
   has no Select button, so its checkboxes stay hidden on a phone (it has no
   bulk actions yet, decision 6 of LAUN-13).
4. **Board cards:** Linear's layout: id, title, priority and labels, then
   `Created <date>` (`shortDate`, browser zone). More air (14 px padding,
   10 px gaps), columns `min(84vw, 320px)` wide, a quieter column header. The
   date is on every card in the DOM but visible only on a phone, so the desktop
   card does not change. The card checkbox follows the same select-mode rule.
   4a. **Focus order:** on a phone (`matchMedia`, 760 px) the DOM is Search,
   Filter, view toggle, chips, Clear/Sort/Select, so Tab matches what is
   shown. On a wide screen it is chips, Search, Clear/Sort, view toggle, as
   before. The fold ids come from `useId`. The card date uses the browser's
   zone; tests use midday UTC so they hold in any zone.
5. **Not done:** no row swipe actions and no sticky filter summary.

## Issue detail page (LAUN-21)

The project owner asked for Linear's issue page. The decisions:

1. **Layout:** a quiet bar (team link at the left, toolbar at the right), the
   title, "Sub-issue of ..." when there is a parent, the description, then
   sub-issues and one Activity stream. The right column is Properties. At
   900 px and below it is one column: title, then Properties, then the body.
2. **Toolbar:** (a plain group named "Issue actions", not an ARIA toolbar: it has no arrow-key behaviour) Copy link, Copy ID, Copy branch name (text buttons, muted;
   there is no "more" menu because nothing else is left to put in it). The
   link is `origin + /org/issue/ID`. The Branch row is gone: the branch name
   is the third button's title and its copy.
3. **Where each old function went:** Status, Priority, Assignee, Labels: same
   pickers, now ghost rows (no caption; the empty text is the call to act:
   "Set priority", "Assign", "Add label"). Parent: its own small "Parent"
   section (picker `Parent issue`, can clear) plus the "Sub-issue of" line.
   Team: the link in the bar. Breadcrumb "Issues" link: gone (Escape, the
   sidebar and the team link go back; no function is lost). Created: the first
   Activity line (creator and time). Updated: the hover title of that line
   ("Updated <date>"); there is no row.
   Relations: the "Related" section, with the same type select, picker and
   remove buttons ("x" instead of the word, the aria-label is unchanged).
   Sub-issue add: "+ Add sub-issues" (opens the create dialog with the parent).
4. **Cycle row** shows only when the team has cycles on. The list is current,
   next, then the last 10 past cycles (`useCycles`). Set/clear sends
   `cycleId`. **Project** uses `useProjects(team)` (all pages of the team's projects that are not archived, 50 per call; the create dialog uses the same hook)
   and sends `projectId`; a set milestone is cleared in the same PATCH (it
   belongs to the old project).
5. **Description in place:** the text is the editor's door: click it, or
   focus it and press Enter. The editor, Save / Cancel, Ctrl/Cmd+Enter, Escape
   to drop, the version taken when editing STARTS, the conflict banner and the
   "unsaved text" box are unchanged (the tests of decisions 9 and later still
   pass). Links in the text stay links and do not start an edit. There is no
   heading and no Edit button; the empty text reads "Add description...".
   Known gap: the editor is still the markdown textarea, not rich text.
6. **Sub-issue pill:** "done/total" of the PARENT's sub-issues, where done is
   state type `completed` (canceled does not count). It reads
   `GET /issues?parent=`, the same call as the sub-issue list.
7. **Activity:** one ordered list: "X created the issue" (from `createdAt`;
   the creation row of the state history is skipped, it says the same),
   state changes, and comment cards (replies nested). The comment box is last.
   The "Comments" heading is gone. Known gap: Linear's "Subscribe" control is
   not built (the API has no subscription).
8. The new styles are in `detail.module.css`. The old detail rules in
   `issues.module.css` (`.detail`, `.props`, `.propList`, `.crumbs`, `.branch*`,
   `.parentLink`, `.titleInput`, `.activity`, `.relAdd`) are now unused. They
   stay until LAUN-16's edits to that file are merged, to avoid conflicts;
   delete them after.
9. **Reply nesting:** the Activity list carries the `thread` class too, so a
   reply keeps the indent and the left border it had.
10. **Selection and focus:** a click on the description does not start an edit
    when text is selected, or on a link, button or image. When the editor
    closes, focus returns to the description text.
11. **Screen readers:** the ghost buttons read "Status: Backlog" (the
    `triggerLabel` prop of `PickerPopover`). The Properties column is one
    landmark; its first group is a plain heading, not a second region.
12. **DOM order:** the body comes BEFORE the Properties column in the DOM, so on
    a wide screen a keyboard user reaches the description first. The grid areas
    place the column on the right; at 900 px and below they place it above the
    body. So on a phone it is first on the screen but later in the tab order.
13. **Project:** `useProjects(team)` is the same hook as the create dialog's
    (all pages, not archived, off without a team). An issue whose project is not
    in that list (archived) is named through `GET /projects/{id}` (`useProject`).
    Changing the cycle also refreshes the cycle progress queries.
14. A copy with no `navigator.clipboard`, or one that fails, shows the
    "Could not copy" error toast.

## One scroll area (LAUN-40)

The project owner saw two vertical scrollbars on the issue page, a shell shorter than the
window and a white band below it.

1. **Root cause:** `.sr-only` was `position: absolute`. The issue page is a
   scroll container (`.detail`, `overflow: auto`) with no positioned ancestor,
   so its `sr-only` labels sat at their static position in the initial
   containing block. An absolute box is not clipped by a scroll container
   that is not its containing block, so the labels (6000 px down on a long
   issue) stretched the PAGE scroll height. The page then scrolled as well as
   `.detail`, and a scrolled page showed the shell ending early.
2. **Fix:** `.sr-only` is `position: fixed` (a fixed box adds nothing to any
   scroll height). The shell also owns the height: `.app` is `100dvh` with
   `overflow: hidden`, `.main` and `.content` have `min-height: 0`, and
   `.content` is `overflow-y: auto` as the fallback scroller for a page that
   does not scroll itself. List, board, cycle, settings and detail keep their
   own scroller; the board still scrolls sideways inside itself (LAUN-16).
3. **Guard:** `src/styles/scroll.test.ts` pins the rules (jsdom has no
   layout).
4. **Browser check:** open a page in headless Chrome with the API stubbed and
   compare `document.documentElement.scrollHeight` with `innerHeight` (must be
   equal), and look for `position: absolute` elements whose bottom is past the
   window. Done once for LAUN-40 at 1280x800, 1900x1000 and 375x700 (phone
   emulation) on all issues, team, board, issue detail (120 paragraphs) and
   settings: page overflow was 5652 px on the issue page before, 0 after.

## Cycle page filters and view (LAUN-18)

Code: `IssueBrowser.tsx` (new), `IssuesPage.tsx`, `CyclePage.tsx`.

1. **One component.** The filter bar, list, board, selection, hotkeys, bulk
   pickers, infinite scroll and the empty states moved out of `IssuesPage`
   into `IssueBrowser`. `IssuesPage` and the cycle page both render it, so the
   cycle page is the issues list with a fixed `team` and `cycle`. Nothing on
   the issues list changed (its tests pass as they were).
2. **Inputs.** `IssueBrowser` takes `filters` and `onChange` (the caller owns
   where they live), `extra` (API parameters, here `cycle`), `lockTeam`,
   `lockAssignee`, an optional `title`, `blocked` (replaces the rows) and the
   empty state. The cycle page gives `lockTeam`, so the Status chip lists only
   that team's states, and `emptyTitle` "No issues in this cycle".
3. **URL.** The filters are in the query string of the cycle URL, with the
   same names as the issues list (`state`, `assignee`, `label`, `priority`,
   `q`, `sort`, `view`), written with `replace`. `team` comes from the path
   and is never written. A reload or a shared link keeps filters and view. The
   cycle is not in the query: it is the path.
4. **Header.** The head (dates, "N% done (a/b)") reads the cycle snapshots
   and ignores the filters, so the progress stays on the whole cycle.
5. **Board.** Same columns (one per state) and cards as the issues board. The
   board keeps loading pages (up to 20), as on the issues list.
6. **Selection and keys: same as the issues list** (decision made here; it
   replaces "hidden" in LAUN-13 #6). `j` `k` `x` `s` `a` `l` `p` `Enter` and
   `Escape` work, and the phone Select button and "N selected" with Clear show.
   `Alt+J` / `Alt+K` still switch cycle; the hotkey hook ignores Alt.
   The cycle page has no `h1` row: its own head is the title. So that the bar
   and the rows do not move when the first row is selected, "N selected" and
   Clear float at the bottom right (`.selFloat`) when `IssueBrowser` has no
   `title`; with a title they stay in the head row.
7. **Layout.** The issue browser is its own flex column and scroll area, so
   the cycle page puts it straight under the head. Other states (loading,
   error, not found) keep the `.scroll` wrapper.
8. **Switching cycle** still resets selection (the browser is keyed by cycle
   number). The filters stay in the URL only while the path is the same: the
   switcher goes to a clean URL of the other cycle.
9. **Not verified in a real browser**, only in jsdom tests.

## Labels like Linear (LAUN-46)

1. **One pill.** `ui/LabelPill.tsx` (+ `labelPill.module.css`) is the only label
   pill: round (11px radius), 1px outline in `--border`, 22px high, 12px text,
   colour dot, transparent fill. Issue rows, board cards and the detail page use
   it. `LabelDot` is the same dot; the picker rows (detail, quick picker, filter
   bar, create dialog) and the create dialog chip use it, so there is no second
   dot style. `features/issues/LabelChip.tsx` is gone.
2. **Detail page.** `LabelsField`: the "Labels" heading, then one wrapping
   list of pills with a quiet "+" as the last item (so it stays on the last
   line when the row wraps). With no labels the "+" reads "Add label". No box,
   no chevron.
3. **Picker.** "+" and a mouse press on a pill open the existing `ChipPicker`
   (search, multi-select, check marks, create on the fly, arrow keys, Esc).
   `ChipPicker` got two optional props, `triggerClassName` and `triggerName`.
   The "+" button is named "Add label"; the list and search box are named "Labels".
4. **Remove.** Each pill has an "x" button named "Remove label <name>". The pill
   always keeps a 22px strip on its right for it, and the "x" is laid over that
   strip, so hover never changes the pill's size (no wrap flicker at a line end).
   It shows on hover or keyboard focus, and always on a touch screen
   (`hover: none`). Its hit area is 24px wide and as high as the pill. After a
   removal focus goes to the next pill's "x", else the previous one, else "+".
   A press on a pill toggles the picker (a press while it is open closes it).
   The picker can still un-check a label.
5. **Screen readers.** The row is a list named by the "Labels" heading; a pill's
   name is its label name.
6. **Create dialog chip and filter bar share only the dot, not the pill.** They
   are property buttons in a row of other property buttons (Status, Priority,
   Assignee), not a list of labels: they say "Labels", "2 labels" or "Label: bug",
   and a pill row inside them would break their size and the shortcut and
   accessible names that tests pin. So the AC "one shared pill" is met for
   the places that list labels (detail, rows, cards); for these two it is met
   for the dot only. Known gap: `.chip` and `.dot` in `ui.module.css` are now unused;
   delete them after LAUN-41 is merged (that file is open on another branch).

## Cycle switcher: the name keeps its width (LAUN-41)

1. **Cause:** the menu row name was `flex: 1; min-width: 0` and the date had
   no wrap rule, so on the 220-320 px panel the name got only what was left
   after the date and the key hint, and shrank to "C..".
2. **Fix (all `Menu` rows):** the name (`menuText`) has `min-width: 8ch` and
   a `title` with the full text; the date (`menuDetail`) does not shrink or
   wrap (a `min-content` floor broke the date into one word per line in a real
   browser), so a long name ellipsizes and the date never meets the key hint; the
   key hint (`menuKey`) does not shrink. The cycle switcher panel is 300-400 px
   on a wide screen (selector `.head .switchPanel`, so it beats `.panel`
   whatever the stylesheet order); the phone rule (8 px in from each edge,
   hints hidden) is unchanged.
   2a. **No inline style on a row:** the row's width, border, background and
   alignment are in `.menuRow`, so the hover tint and the focus tint apply (an
   inline `background` had beaten both). The "current" mark does not wrap.
3. **Focus:** a focused menu row has the selected tint and a 1 px inset
   ring, not the global 2 px box with offset.
4. **Checked:** markup and CSS rules in jsdom tests. Also in headless Brave
   (puppeteer-core, Vite dev server, mocked API) at 390, 768 and 1280 px with
   two long custom names: the name ellipsizes, the date stays on one line, the
   key hint does not overlap it (hidden on a phone), no horizontal overflow,
   and keyboard focus shows the tint and a 1 px ring. Not checked: short
   names, dark mode, hover by eye, a real touch device.

## Task-list tick boxes (LAUN-47)

`- [ ] text` and `- [x] text` in descriptions, comments and documents render as
real checkboxes, and a click toggles the box in the source text.

1. **No new dependency, no schema change.** react-markdown here has no GFM
   plugin. `lib/markdown.tsx` adds a small rehype step that runs AFTER
   `rehype-sanitize`. The sanitiser schema is unchanged, so user text can
   never make an `<input>` (raw HTML is not parsed). Only the step makes one,
   for a list item whose first text is `[ ]`, `[x]` or `[X]`.
2. **Offsets, not counting.** The parser gives each text node its offset in
   the source. The step stores it on the box (`data-task-offset`), and
   `lib/taskList.ts` `toggleTaskAt(source, offset)` flips exactly those three
   characters. So code fences, inline code, an escaped `\[ ]`, CRLF, nested
   lists, `-`, `*`, `+` and `1.` markers all follow the parser, never a regex
   over lines. If the source no longer has a marker at the offset (changed
   under the click), nothing is sent.
3. **Read-only by default.** `<Markdown>` takes an optional `onToggleTask`.
   Without it the boxes are `disabled` (previews, documents, someone else's
   comment). The web app has no write-scope flag for a session, so the only
   gate is: description not a placeholder (`loading`), comment is the
   author's.
4. **Description.** The toggle uses the existing `save` with
   `baseVersion = issue.version` (a compare-and-swap on the version the
   person saw) and the optimistic patch. A 409 shows the existing "changed
   elsewhere" banner and the box rolls back. A click on an `input` does not
   open the click-to-edit editor (the handler skips `a,button,img,input`, and
   the box stops propagation).
5. **Comment.** The author's click calls the existing edit mutation. The box
   shows the new state at once (`pending` text) until the refetched comment
   carries it; a failure drops it and shows the error line.
6. **Stable component identity.** The box reads the text and handler from a
   React context, so the `components` map is one constant. A map that changed
   on every render remounted the checkbox and lost the click (found by a
   test).
7. **Look.** No bullet on a task item, checked text is muted with a
   strike-through, nested lists are not struck (inline-block), a plain item
   in a mixed list keeps its bullet. Accessible name is the item text
   (`aria-label`); Space toggles because it is a real checkbox.
8. **Browser check:** headless Brave and puppeteer-core, API stubbed, at 1280
   and 390, light and dark: boxes, strike-through, mixed and nested lists,
   code fence untouched, click saves the right line without opening the
   editor, Space toggles, a comment of another user is disabled, state
   survives a reload.

## Activity timeline (LAUN-48)

The Activity block was judged too plain. It is now a Linear-style
timeline. The model is `web/src/features/issues/activityModel.ts` (pure, with
tests); the view is `Activity.tsx`, the cards `Comments.tsx`, the look
`detail.module.css`.

**What the server returns today (checked 2026-10-26)**

| Kind                   | Source                                      | Actor                             | Time | Shown               |
| ---------------------- | ------------------------------------------- | --------------------------------- | ---- | ------------------- |
| Created                | `issue.createdAt`, `creator`                | yes                               | yes  | yes                 |
| State change           | `include=stateHistory`                      | yes or null                       | yes  | yes (icons, source) |
| Link / file attachment | `include=attachments` (added to `useIssue`) | `creator`, null for a GitLab hook | yes  | yes                 |
| Comment, reply         | `GET /issues/{id}/comments`                 | yes                               | yes  | yes (cards)         |

**Gaps (nothing recorded per issue, so nothing invented): ** assignee,
priority, labels, cycle, project, milestone, parent, estimate, due date,
title edit, description edit, attachment removed, relation removed. Relation ADDED is stored
(`issue_relations.created_by` / `created_at`) but the REST `Relation` omits
both, so it only needs EXPOSING; removals need recording. `issue.updated`
events in the org-wide feed carry `changes` (field names), actor and time,
but no values, and the feed cannot be filtered by issue. The server emits
`attachment.created` only (no delete event). A comment has no `source`, so "via MCP" cannot be told; the web says
"via agent" when the author is an agent user (`GET /users`, `isAgent`).
Attachments record no source and a null creator shows no name
("Linked 3 links: ..."). Server ticket: **LAUN-49** "Server: record and expose more issue activity
kinds" (with a design hint). When it ships, the web adds kinds to
`activityModel.ts`.

**Subscriptions:** no server support (LAUN-23 / LAUN-31). So there is no
Subscribe / Unsubscribe button and no subscriber avatar. Marked gap; add them
with the notification work.

**Decisions**

1. One `<ol>` is the stream. Each event is one `<li>`: an `aria-hidden`
   gutter cell (avatar or icon), then one line of text with a `<time>` (its
   `title` is the exact local time). A comment is one `<li>` with a card.
   The "Comments" heading is gone; the comment box is last.
2. **Gutter and line:** a 24 px column; a 1 px `--border` line runs through
   every item (from the first icon to the last). Event icons sit on a
   `--bg` square so the line seems to stop at them. Cards leave the gutter
   empty (the line passes).
3. **Fold rule** (`foldEvents`): neighbours fold when they have the same kind,
   the same actor (a missing actor counts as one actor) and, for state
   changes, the same source, and each is at most 5 minutes
   (`FOLD_GAP_MS`) after the one before it (a slow chain keeps folding). A
   comment ends a burst. State changes fold to "moved from FIRST to LAST"
   at the last time; links fold to "linked 3 links: a, b, c"; files apart
   from links. Equal times keep the order created, state, attachment, comment.
4. **State names** show their state icon from the team's states (a name
   not in the list shows no icon). A null actor shows its source as the
   name ("GitLab", "Import"); with an actor the suffix "via GitLab" /
   "via import" stays (LAUN-21).
5. **Links** open only when the URL is http(s); a file links to its
   `downloadUrl`.
6. **Comment cards:** a top-level comment is a card; replies stay a nested
   `<ul>` inside it (separated by a line, not a left bar). The card ends
   with a "Leave a reply…" row that opens the same reply box as the Reply
   button (kept, with Edit and Delete and their rules, unchanged). There is
   no attach icon and no send icon: comments have no attachments today.
7. **Pieces I did not touch:** `IssueDetailPage.tsx` has no diff (the
   `ActivityStream` props are the same), so LAUN-46 and LAUN-47 merge clean.
   The only API change is `include=relations,stateHistory,attachments`.

8. **Live refresh:** `attachment.created` is in `EVENT_TYPES` and
   invalidates the issue details, so an open page shows a link added by an
   agent or another user.
9. **Round trips:** a fold that ends where it began (Todo, Done, Todo; A, B,
   C, A) is dropped, never "moved from X to X".
10. **Long folds:** a folded attachment line lists 3 titles, then "+N more".
11. **Reply controls:** a card has one "Leave a reply to <author>" row; the
    ghost Reply button remains only on nested replies (labelled "Reply to
    <author>"). The "·" separator is nowrap with the time, so it cannot
    dangle at a line end.
12. **Accepted limits:** a deactivated agent loses "via agent"
    (`useMembers` lists active members only); state icons use the team's
    CURRENT state names (a renamed or deleted state shows no icon).

**Browser check:** headless Brave with the API stubbed (created, 2 moves
that fold, 3 links that fold, a file, a comment by an agent, a reply, a
GitLab move), light and dark, 1280 and 390 wide; no sideways overflow.

**NOT checked:** the API was stubbed (no real server run); no screen reader
was used (structure checked by tests only); real GitLab, import and system
events were not seen (stub data only); tablet widths were not looked at;
keyboard order was not tried in a browser; CI has not run.

## Whole card or row opens the issue (LAUN-50)

Code: `BoardView.tsx`, `IssueRow.tsx`, `cardLink.ts`, `issues.module.css`
(the cycle page list uses the same `IssueBrowser`, so it has this too).

1. **Stretched link.** The title `<a>` stays the ONE link in a card or row (its
   accessible name is the title). Its `::after` is `position: absolute; inset: 0`
   over the `li`, so a click on the padding, the age or the avatar opens the issue.
   Keyboard, middle-click and Cmd-click work because it is a real anchor. No link
   or button is nested in it.
2. **The ellipsis moved to an inner span** (`.titleText`). `overflow: hidden` on
   the link itself would clip its `::after`. The two-line clamp on a phone moved
   there too.
3. **Stacking.** `.row` and `.card` are `position: relative; isolation: isolate`, so
   the z-indexes inside them never reach the sticky `.groupHead` (z-index 3, below
   `.selFloat` 5). Any `button` or `input` in a card or row gets `z-index: 1` over the
   overlay (the checkbox now; pickers and pill buttons when they come).
4. **Tooltips and the identifier.** The overlay would hide the native `title`
   tooltips (row priority, status, assignee, age) and stop the identifier being
   copied. So `.ident` and those four spans are `position: relative; z-index: 1`,
   and `.ident` is `user-select: text`. A plain click on them still opens the issue:
   the `li` has `onClick={onCardClick}`, which clicks the title link (so the select
   mode rule below applies too). It ignores clicks on a link, button, input or label,
   modified clicks, and the end of a text selection. A Cmd-click on these spans does
   nothing (it cannot be forwarded as a new-tab click). Board cards have no tooltips;
   only their identifier is raised. Dragging over a board card's id drags the card
   (the card is `draggable`), so selecting it there needs a double-click.
5. **Click rule** (`onCardLinkClick`): a modified or non-primary click is left to the
   browser. A MOUSE click (`detail > 0`) is not a click when a text selection that
   starts inside this same card or row is open; Enter (`detail 0`) always works. On a
   phone (760 px) in select mode a tap on the card or row toggles it instead of
   navigating (before, only the checkbox did). On a wide screen a click opens.
6. **Drag.** The card stays `draggable`. The link has `-webkit-user-drag: none` /
   `user-drag: none` (not `draggable="false"`, which could block the card drag in
   Firefox and Safari: unmeasured), and `dragstart` sets the card as the drag image
   so a drag that begins on the stretched link still carries the card. A completed
   drag fires no click. LAUN-29 will rework the drag: keep the click rule in
   `onCardLinkClick`.
7. **Look.** Card hover `--surface-hover`, row hover `--surface`, both only when the
   item is not `.selected` or `.focused` (`:hover:not(.selected):not(.focused)`), so
   hover never hides the selection or the keyboard cursor. Title underline only on
   hover of the item or keyboard focus of the link (on the span). The focus ring is
   `outline: 2px solid var(--focus)` on the link's `::after` (the whole card), and
   the link's own outline is off. It needs no `:has()`, so no browser loses it.
8. **Tests.** `cardClick.test.tsx`: one link and no nesting; title and identifier
   click open; checkbox does not; Cmd/Ctrl/middle click not prevented; phone
   select-mode tap toggles; selection guard (mouse click, same item only; Enter
   works); dragStart/dragEnd leave the page. `styles/cardLinkCss.test.ts` pins the
   CSS contract (jsdom has no layout). Real browser (headless Brave, puppeteer,
   `<scratch>/LAUN-50/builder/real50.mjs`): at 1280 on list and board a
   click on the padding and on the empty middle navigates, the checkbox does not,
   Cmd-click opens a tab and the page stays, the row tooltips exist, the id is
   selectable, the sticky group head paints over a checkbox while scrolling, the
   focus ring shows; a real board drag sends the PATCH and does not navigate; at 390
   touch a tap on card and row padding navigates and a select-mode tap toggles.
9. **Not checked:** Safari and Firefox. Quick pickers, label pill buttons and menus
   do not exist inside a card or row yet.

## Board columns stretch to the bottom (LAUN-52)

1. **Chain.** `.scroll` gets `.scrollBoard` while the board shows (not for the
   skeleton, error or empty state): a flex column, `overflow: hidden`. `.board` is
   `flex: 1; min-height: 0; align-items: stretch`, scrolls sideways only
   (`overflow-x: auto; overflow-y: hidden`). `.column` has no `max-height` or
   `align-self`, so every column is as tall as the board. `.cards` is `flex: 1`
   with `overflow-y: auto`: a long column scrolls inside itself, and the page has
   no second scrollbar (LAUN-40 rules are unchanged).
2. **Drop target.** The gray panel and the empty area under the cards are the
   `<section data-column>` itself, which owns the drag handlers. LAUN-29 needs no
   extra element.
3. **Same for both boards.** The issues board and the cycle board share
   `BoardView` and `IssueBrowser`. The phone rule (`flex-basis`) is unchanged: the
   columns keep their height and the board scrolls sideways.
4. **Tests.** `styles/boardColumnCss.test.ts` pins the CSS contract. Real browser
   (headless Brave, Vite dev, stubbed API, 1 / 3 / 30 / 0 cards in four columns,
   `<scratch>/LAUN-52/builder/shot.mjs`): at 1280, 1900 and 375 every
   column spans from the board top to the board bottom, the 30-card column scrolls
   inside (`scrollHeight` 3004, `clientHeight` 624), the document height equals
   the window, and a point in the empty area of the empty column hits its
   `<section>`. At 375 the board scrolls sideways (`scrollWidth` 1322).
5. **Not checked:** Safari and Firefox.

## Settings area in Linear's style (LAUN-26)

1. **Sidebar.** While the path is `/:org/settings/*` the shell draws
   `SettingsSidebar` in the sidebar slot instead of the app sidebar (org menu,
   Create, My issues, Teams, Views are not drawn; the user menu stays). It has
   "Back to app" (`/:org/issues`), a search box and the pages the person may
   open, in groups with a small heading: Administration (Organisation, Teams,
   Members, Sign-ups, Agents, API tokens, MCP) and Platform (Platform). The
   active page has a gray pill and `aria-current="page"`. The list of pages is
   one array, `settingsPages.ts`; the sidebar and the search both read it.
   `<nav aria-label="Settings">` holds only the page links (Back to app and the
   search box are outside it).
2. **Access is unchanged** (`access.ts`): an `admin` page shows to an org
   admin, the `platform` page to a platform admin; a platform admin who is only
   a member sees Platform and nothing else; a member sees no sidebar and the
   "You need admin access." notice. The search only looks at pages the person
   may open.
3. **Search.** A page matches by its name or by `keywords`, the labels of its
   rows and sections ("postmark" finds Platform and shows the word under it).
   Escape clears the box. "No settings match." is a `role=status` text.
   Rows are not indexed from the rendered page: the keywords are written by
   hand next to the page, so a new row needs a keyword to be found.
4. **Shared parts** (`SettingsParts.tsx`, `settingsParts.module.css`):
   `SettingsPage` (the one h1, takes focus on mount, optional `description`,
   `wide` for table pages), `SettingsSection` (h2 and a help line; `id` names a
   form inside it), `SettingsCard` (rounded, thin border; `padded` for a table
   or a form instead of rows), `SettingsRow` (label, one line of help, a control
   slot; with `to` the whole row is a link and ends in a chevron; `labelFor`
   ties the label to a control), `SettingsToggle` (`role=switch`, accent when
   on) and `NoAccess`. Page background is `--surface`, cards `--bg`; both are
   existing tokens, so dark mode needs nothing new. The old `PageHeading` is
   gone. A new settings page (LAUN-22, 23, 25) uses these and adds one entry to
   `settingsPages.ts` and one route.
5. **Migration.** Every page uses `SettingsPage`; tables sit in a card (the
   card scrolls sideways under 680 px, the table keeps its width); forms are a
   `SettingsSection` (visible h2, the form keeps `aria-labelledby` to it) in a
   padded card. Organisation is now rows. The Platform form is one card with its
   own sub headings. Routes did not change, so every old URL works.
6. **MCP page (new, `/:org/settings/mcp`).** Asked for by the project owner (move MCP to its own group) and not in the ticket. Admin only. It shows the server URL,
   the org and the first team as the `LAUNCHPAD_*` values, links to Agents
   and API tokens, and a copyable `.mcp.json` entry. The entry names the token
   `${LAUNCHPAD_TOKEN}`: the page never holds or shows a secret. The server
   key in the entry is `launchpad`; it is only a local name and the user may
   change it (the page says so). "Default team" is the first team in the list,
   and the row says so; it is not a select.
7. **Phone.** The settings sidebar sits in the same drawer as the app sidebar
   (the menu button in the top bar), so it is the page list, and a tap opens a
   page and closes the drawer. Pages have 16 px side padding and rows wrap.
8. **Not done.** Personal group (Preferences, Profile, Notifications, own API
   tokens): the pages do not exist, and a member cannot open Settings yet. "Your
   teams" and "Create a team": there is no page per team; Teams is the one page.
   Platform is one page, not three (Mail, Sign-up mode, Platform admins are
   sections of it). No per-row search index. Safari and Firefox not checked.
9. **Tests.** `settingsArea.test.tsx` (parts, `searchSettings`, groups, Back to
   app, search, access, MCP page); the team-create test in `settings.test.tsx`
   now reads the new team from the table, because the app sidebar is not drawn
   on a settings page. Real browser (headless Brave, Vite dev, stubbed API,
   `<scratch>/LAUN-26/builder/shot.mjs`): 1280 and 375 px, no
   sideways page scroll on Organisation, Members, Tokens, MCP, Platform, Teams.

## Browser tab title follows the page (LAUN-30)

1. **Format.** `<Org name> › <page or item>`, as in Linear. The separator is U+203A
   with a space on each side. The org name is the display name from the session
   (`useOrgName`), not the slug. The project owner chose the issue form: `Orbit › ORB-123 Title`.
2. **One hook.** `lib/documentTitle.ts`: `useDocumentTitle(parts, prefix)` joins the
   parts that exist and sets `document.title`. It restores nothing on unmount: the next
   page sets its own, and `index.html` keeps `Launchpad 🚀` for the first paint. With no
   part it leaves the title alone. `usePageTitle(...parts)` adds the org name;
   `useAppTitle(page)` is for pages with no org. `prefix` is the place for the unread
   count (`"(3) "`, LAUN-23); nothing builds it here.
3. **Titles.** Issues `Issues`; My issues; a team's issues the team name (the same text
   as the page heading and the sidebar); `Cycles`; a cycle `Cycle 8` (or its name);
   an issue `ORB-123 <title>`; settings `Settings › <page>` (Organisation, Teams,
   Members, Sign-ups, Agents, API tokens, Platform; the no-access notice `Settings`);
   an unknown address, a missing issue and a team page with no match `Not found`; an org
   the person is not in `Not found` (no org name). Outside an org: `Launchpad › Log in`,
   `Set up`, `Sign up`, `Join`, `No organisation`. Projects, documents and the inbox have
   no web page yet; each gets its call when it lands.
4. **Data.** No request is made for a title. An issue shows the identifier alone until
   it loads (or the list row passed as the placeholder), then `ORB-123 <title>`; a live
   edit (event, refetch) changes the tab at once. The unsaved draft is not used.
5. **Long titles.** The issue title is cut at 60 characters (code points, white space
   collapsed) with `…`.
6. **Focus.** Untouched. `PageHeading` still takes focus; it only also sets the title.
7. **Tests.** `lib/documentTitle.test.tsx` (hook, update, no restore, prefix, cut) and
   `features/titles.test.tsx` (every route above, live edit, loading, long title).
   Real browser (headless Brave, Vite dev, stubbed API, `document.title` on eleven
   routes and after an in-app click): all as listed.
8. **Not checked:** the history and bookmark entries in a real browser menu (they use
   `document.title`); Safari and Firefox.

## Board drag: the card moves, the others make room, the order is kept (LAUN-29)

Code: `boardDrag.ts` (the drag machine), `dropPosition.ts` (pure maths),
`BoardView.tsx`, `cardLink.ts` (click guard), `actions.ts` (`place`),
`queries.ts` (`moveIssueInCaches`), `issues.module.css`. Server rules:
`docs/internal/decisions-T14-manual-order.md`. The cycle page board is the same
`BoardView`, so it has all of this.

1. **Pointer events, no library.** A press on a card (not on its checkbox, not
   with a modifier key, left button for a mouse) becomes a drag after 4 px
   (mouse, pen). Before that it is a click. Touch: press and hold 250 ms; a move
   of more than 8 px before that is a scroll and no drag starts. While a card is
   held, `touchmove` is cancelled, so the page does not scroll. The browser's own
   drag is cancelled (`dragstart` is prevented; the card is not `draggable`), so
   there is no ghost image. The drag starts text selection off: `selectstart` is
   prevented, the selection is cleared and the body gets `user-select: none` while
   it lasts. A long-press menu on touch is cancelled.
2. **What moves.** A copy of the card (the clone) follows the pointer in a fixed
   layer with a shadow, at the grab offset. The real card stays in its slot,
   hidden (`visibility`). The cards under the gap slide with `transform:
translateY` (150 ms ease), and ONE gray placeholder (`li[data-gap]`, absolutely
   positioned in the target column) marks the gap. The source slot closes up. No
   layout changes anywhere, so no layout shift. `prefers-reduced-motion`: no
   slide, and the clone lands at once; the gap still moves.
3. **No React state during a drag.** Everything above is style writes from
   `boardDrag.ts`. A card is `memo`; a drag renders no card (test: a 120-card
   column, 40 moves, zero card renders). The target is measured once per column
   (offsets of the cards), in the CLOSED layout (the gap is not counted), so
   opening the gap never moves the thresholds and the gap does not flicker.
4. **Which place.** The dragged card's vertical centre decides: the gap goes after
   every card whose centre is above it (`dropIndex`). Over a column anywhere (its
   empty area counts; the gap between two columns belongs to the left one). Outside
   the board is no target.
5. **The drop is ONE update.** `PATCH /issues/{issue}` with `state` (only when the
   column is another one) and ONE of `placeAfter` (the card above the gap) or
   `placeBefore` (the card below, for the top of a column). The client sends
   neighbour ids and never a rank. An empty column sends the state only: the server
   puts the card on top. A drop where the card already was sends nothing.
6. **Teams.** A rank only means something inside one team. On a board that mixes
   teams (all issues) a column can hold cards of several teams; `placementAt` skips
   cards of other teams and names the nearest card of the dragged issue's team (so
   the saved place can differ a little from the gap you saw, never the column).
   With one team on the board (a team page, the cycle page) there is no difference.
7. **How the sort options meet the board.** (Superseded by LAUN-110, last section: the board follows the Sort select.) "Manual" is a fourth Sort option
   (`?sort=manual`, `orderBy=manual`). The board ALWAYS fetches the manual order
   and keeps the server's order inside each column (it no longer re-sorts by
   priority); its Sort select shows "Manual" and is disabled, with a title that says
   why. So a drop on the board is always a manual order change (no "this column is
   sorted another way" case, and nothing to switch). The list keeps Updated,
   Created, Priority and Manual; its choice stays in the URL for when you come back
   from the board, and only "Manual" shows the order you made on the board. One
   query fetches every column (a team, or all). A rank is per column and the list is
   sorted by `(rank, id)`, so the cards of one column keep their order. This differs
   from decisions-T14 item 11 (one query per column, with `team` and `state`):
   per-column queries are a later change; the cost is that a refetch loads every
   page. A new or state-changed card comes back on top of its column from the server.
8. **Optimistic update and rollback.** On drop the card is moved in the cached
   lists at once (`moveIssueInCaches`: next to its neighbour, plus the state). On
   an error the snapshot is put back and a toast says why. The clone and the
   slid cards stay on screen until the optimistic update has rendered
   (`columnsChanged`, or 500 ms), so the card never flashes back for a frame.
9. **Refetch.** `stale_placement` (409, the neighbour left the column): the toast
   says "The column changed ... drop it again" and the lists are fetched again (the
   mutation's `onSettled` invalidates them on every outcome). A `rebalanced: true`
   answer, or an `issue.updated` event from someone else, also refetch all lists:
   `invalidateForEvent` already does that for every issue event, so a rebalance
   needs no extra code.
10. **Escape, outside drop, pointercancel.** The clone flies back to the slot
    (160 ms, none under reduced motion), nothing is sent, and aria-live says
    "Move of ORB-1 canceled". Escape is taken before the page's own Escape (clear
    selection).
11. **Click guard.** A drop ends with a `pointerup`, which can be followed by a
    `click` on the card. `markDragged()` (`cardLink.ts`) is called when a drag ends;
    for 350 ms `onCardLinkClick` (a mouse click on the title link) and
    `onCardClick` ignore the click. A keyboard Enter (`detail` 0) on the link still
    opens the issue.
12. **Auto-scroll.** In a loop (one animation frame at a time) within 56 px of the
    board's left or right edge the board scrolls sideways, and within 56 px of the
    target column's top or bottom edge that column scrolls (up to 22 px a frame,
    faster toward the edge). The gap is measured again after a scroll.
13. **Keyboard and screen readers.** There is no keyboard drag. The path stays the
    `S` status picker (and the other pickers), which changes the state, and the
    server puts the card on top of its new column. A drop is announced in an
    `aria-live="polite"` region of the board ("ORB-1 moved to Todo, position 2 of
    4"; "Move of ORB-1 canceled").
14. **Old wording removed.** The ticket's "drop in the same column does what it does
    today (ordering is not stored)" is gone: the order is stored (LAUN-51). The
    "no touch drag" note above is gone too.
15. **Limits.** A column that is only partly loaded (the board loads up to 20 pages)
    can get "place after the last LOADED card", not the true bottom: the server
    places it after that card, and cards beyond it stay below. The board does not set
    `touch-action` on the cards (the page and the board must scroll with a finger);
    it relies on the 250 ms hold and cancelling `touchmove`. Whether iOS Safari honours
    that for a held finger is unmeasured.
16. **Cancel paths.** Escape, an outside drop, a drop at the same spot, `pointercancel`,
    `lostpointercapture` and the window `blur` all send the card back, send nothing and
    announce "Move of ORB-1 canceled". The click guard stays on from the start of a
    drag until its pointer is released (Escape leaves the button down: a click much
    later must not open the issue), then 350 ms more. The hidden source card is the
    attribute `data-drag-source`, not a class, so a React re-render cannot show it. A
    drop with no neighbour (empty column) is shown on top of the column at once, as the
    server does.
17. **Tests.** `dropPosition.test.ts` (the maths), `boardDrag.test.tsx` (the full
    drag against a fake layout in `src/test/boardLayout.ts`: places, one-update body,
    rollback, `stale_placement`, Escape, the click guard, touch, auto-scroll,
    reduced motion, the render-count guard), `flows.test.tsx` and `cardClick.test.tsx`.
    jsdom has no layout, so the real pointer drag is checked in a browser (below).

## Guided first-run checklist (LAUN-25)

1. **Where.** `/:org/settings/setup`, "Get Launchpad ready", first page of the
   Administration group in the settings sidebar (label "Get started"; the search
   finds it by "checklist"). Admin only. The finish of `/setup` (after the
   token is saved) now opens this page instead of All issues.
2. **No form of its own.** Each step has a title, one help line, a status badge
   and a button to the real page (Organisation, Platform, Teams, MCP, Members).
   Built from `SettingsPage`, `SettingsSection` and `SettingsCard`.
3. **Done comes from the data, not from a click.** Worked out in the browser
   (`onboarding.ts`, `computeSteps`, pure and tested) from routes the app
   already reads. Organisation: always done (setup made it). Mail: platform
   settings `loginAvailable` and provider not `none`. Sign-up mode: never done
   by itself. The API has no field that says the mode was chosen, so the step
   shows "Review" with the current mode, and the person taps "Mark reviewed" (or
   Skip); `updatedBy` is not used, because mail saves set it too. Team and cycles: some team
   has cycles on. Agent and MCP token: an agent user has a live PAT (not revoked, and not expired). Invite your team: more than one human member (agents do not count). Deactivated members count for nothing.
4. **Mail and sign-up mode need a platform admin** (the platform routes). An
   org admin who is not one sees "Needs a platform admin", no link, and the
   banner does not count these steps. The data hooks run only for an org admin
   (the banner mounts them only for one), so a member makes no token or platform
   request; platform settings are read only for a platform admin. A list that
   fails to load shows "Could not load" with Retry, never "Checking" for ever.
5. **Coming soon by capability.** Import from Linear and Git host (LAUN-1) show
   "Coming soon" and no button. The server has no capability route, so the
   switch is the `capabilities` constant in `onboarding.ts`: set the key to
   true when the page exists.
6. **Skip and dismiss are stored in this browser** (`localStorage`
   `launchpad-onboarding:<org>:<user id>`: `skipped` and `reviewed` ids and
   `dismissed`; two people or two orgs do not share it). A skip never
   hides a step that is really done. A thin banner ("Get Launchpad ready. N
   steps are still to do", Open the checklist, Dismiss) shows above app pages to
   an org admin while a step is open; Dismiss hides it, and the checklist page
   has "Show the banner again". The banner never blocks anything.
7. **Gaps, closed by LAUN-56 (server) and LAUN-98 (web).** The server now has `GET /orgs/{org}/onboarding`,
   `PUT .../onboarding/skip` and `PATCH /orgs/{org}`. The checklist and the Organisation page use them; see
   "LAUN-98: the server onboarding state" at the end of this file. Items 3, 5 and 6 above describe the LAUN-25
   build and are replaced there.
8. **Tests.** `onboarding.test.tsx`: `computeSteps`, the page, skip and undo,
   banner show, dismiss, member sees nothing, search. Real browser (headless
   Brave, Vite dev, stub API) at 1280 and 375 px, light and dark: no sideways
   scroll. Screenshots in `<scratch>/LAUN-25/builder/`.

## LAUN-53: the label picker like Linear's

1. **One list, two callers.** The issue page (`LabelsField`) and the create dialog use
   `LabelList` (`features/issues/LabelList.tsx`) through `ChipPicker`'s new `renderList`
   prop. It draws `PickerList` with grouped options. `PickerOption.section` is the new
   field: a small gray heading shows above each run of rows with the same section, and
   only when two or more sections still have a row (a search that leaves one group shows
   no heading; the heading rows are `aria-hidden`). The bulk "Toggle label" dialog
   (`QuickPicker`) keeps the flat list.
2. **Rows.** In every `multi` picker a row has a 16 px box at the left, drawn on the
   focused row, on hover and on a ticked row (its space is always kept, so nothing
   shifts), then the colour dot and the name. Rows are 36 px (40 px under 640 px wide).
   A multi panel has a 12 px radius. This look is in `PickerList` for EVERY multi
   picker, not only labels: the FilterBar filters and the bulk "Toggle label"
   `QuickPicker` share it now. "Bulk picker unchanged" holds only for its flat list
   (no groups); its rows have the box, the new height and the Space key. There is no
   prop to switch this off. Ticking keeps the picker open; Escape or a press
   outside closes it and focus goes back to the trigger.
3. **Keys.** `L` on the issue page opens this picker (the open request is a changing
   counter: a field that mounts with an old count stays shut) (it used to open the flat dialog).
   Arrows move, Enter ticks. Space ticks too while the search box is empty; with text in
   the box it types a space. Escape closes. The search box shows the `L` hint.
4. **The API has no label use count and labels have no team** (`Label` has `parentId`
   only). So there is no "Team labels" group: the third group is **All labels**.
5. **Where the counts come from.** Only while a picker is open, the app asks for the
   team's newest 250 issues (`GET /issues?team=KEY&limit=250`, one page, key
   `['org', o, 'label-usage', team]`, 5 minutes fresh) and counts the labels on them. This
   is a sample, not an all-time total. It is not under the `issues` key prefix, so the
   optimistic list helpers never touch it.
6. **Frequently used** = the 3 labels on the most issues of that sample, at least 2
   issues each (ties by name), not already in Suggestions.
7. **Suggestions** = the labels most used (at most 5) on the issues of the same project
   or of the same parent (the parent itself and its other sub-issues), the issue being
   edited left out. With no project and no parent there is no group. An honest rule needs
   that link, so a loose issue gets none. The create dialog uses its project and parent
   choices.
8. **A label is in one group only**, in the order Suggestions, Frequently used, All labels
   (by name). The groups do not depend on what is ticked, so a tick never moves a row.
   They can change once, when the usage answer arrives after the picker opened.
9. **Saving** is unchanged: each tick is `addLabels` / `removeLabels` with the optimistic
   update and the rollback of `useIssueActions`; the create dialog keeps its local list.
10. **Server field that would help (not done here):** a per-label issue count on
    `GET /labels` (optionally `?team=`), and labels with a team. Then the sample goes away
    and "Team labels" can be real. File as a separate ticket.
11. **Tests:** `labelPicker.test.tsx` (the rules, groups, search, multi-select, keyboard,
    rollback, `L`, usage request) and a case in `CreateIssueDialog.test.tsx`. Browser
    check: headless Brave, stubbed API, 1280 and 375 px, light and dark.

## Board: drag on empty space scrolls sideways (LAUN-55)

Code: `useDragScroll.ts` (the hook), `BoardView.tsx` (one line), `issues.module.css`
(`.board` cursor, `.dragScrolling`), `cardLink.ts` (the click guard, reused).

1. **Separate hook.** `useDragScroll(boardRef, activeClass)` puts one `pointerdown`
   listener on the board. It shares no state with `boardDrag.ts` (LAUN-29), except the
   click guard of `cardLink.ts`.
2. **What starts it.** A press with a mouse (left button) or a pen, no modifier key,
   not already handled. It does NOT start on `[data-issue-id]` (a card: LAUN-29 owns
   it), `a`, `button`, `input`, `select`, `textarea`, `label`, `summary`,
   `[contenteditable]`, or on a scrollbar (the target or an ancestor up to the board:
   the pointer is in the gutter between `clientWidth` and `offsetWidth`, or the same
   for the height). The gap between columns, the board padding, a column header's
   text and the empty area of a column (the `<section>` below its cards) all start it.
   Touch never starts it: a finger scrolls the board natively.
3. **Threshold and follow.** After 4 px sideways the drag begins. `scrollLeft` is then
   `scrollLeft at the press - (pointer x - press x)`, so the first 4 px are not lost.
   Only the sideways scroll changes; a column's own vertical scroll is untouched.
4. **Look.** `.board` has `cursor: grab`. While dragging the board gets
   `.dragScrolling` (`cursor: grabbing` and `user-select: none` on it and all
   children). `selectstart` is cancelled from the press to the end, and the selection
   is cleared at the start. The board holds pointer capture while dragging.
5. **End.** Pointer up, Escape (capture phase, stops the page's own Escape only while
   dragging), `pointercancel`, `lostpointercapture` and window `blur` end it. The
   board stays where it is. A release outside the window arrives as a window
   `pointerup` (the board has capture). Unmount ends it too.
6. **Click guard.** While it drags, `setDragActive(true)`; at the end `markDragged()`.
   So the click that follows a release is ignored for 350 ms (`onCardLinkClick`,
   `onCardClick`). After Escape, blur or cancel with the button still down, the guard
   holds until that button is released. A press and release with under 4 px do nothing
   and set no guard.
7. **Momentum.** Small. On release, when the pointer was moving (the last move under 50
   ms old) and its speed over the last 100 ms is at least 0.3 px/ms, the board coasts
   with an exponential decay: it starts at the release speed and goes at most 400 px
   (`MOMENTUM_MAX_PX`), for at most 1.5 s. It stops at once on a new press, when
   something else scrolls the board (a wheel, the scrollbar), at an edge, and on
   unmount. `prefers-reduced-motion: reduce`: no momentum.
8. **Tests.** `useDragScroll.test.tsx` (follow, threshold, plain click, card press,
   link and checkbox, scrollbar, touch / right button / modifier, pen, selectstart,
   click guard, Escape, blur, pointercancel, lostpointercapture, release outside, other
   pointer, momentum: coast, cap, slow, stopped pointer, reduced motion, outside scroll,
   new press) and `styles/boardDragScrollCss.test.ts`. jsdom has no layout, so
   `scrollLeft` is a plain number there.
9. **Real browser** (headless Brave, Vite dev, stubbed API, 8 columns, 30 cards in one,
   real `page.mouse`; script `<scratch>/LAUN-55/builder/real55.mjs`), at 1280
   and 1900: the board overflows (`scrollWidth` 2278); a gap drag of 120 px left moves
   `scrollLeft` 0 to 120 with cursor `grab`, then `grabbing`, and no text selected; a drag
   from the empty area of a column moves it 200; a plain click does nothing; a card drag
   still sends the PATCH and does not scroll the board; Escape stops it and keeps the
   place; a release outside the window ends it; the wheel still scrolls a column
   vertically; a click on a card opens the issue. Screenshots `drag-1280.png`,
   `drag-1900.png`, `carddrag-1280.png`, `carddrag-1900.png` in the same folder.
10. **Not checked:** Safari and Firefox; a real pen; a visible scrollbar (headless Brave
    has overlay scrollbars of zero thickness, so the scrollbar rule is unit-tested only).

## LAUN-58: drop files on the create dialog and the issue page

1. **One hook, two zones.** `ui/useFileDrop.tsx` (`useFileDrop`, `DropOverlay`) is the whole
   drop logic. The create dialog (on the dialog panel, through the new `panelProps` and
   `overlay` props of `Dialog`) and the issue page (on its root, `useIssueFileDrop.ts`) use it.
2. **Counting.** `dragenter` / `dragleave` fire for every nested child, so they are counted;
   the overlay clears at zero. A `dragover` also turns it on, because a missed `dragenter`
   was seen in headless Brave. The overlay has `pointer-events: none`.
3. **Only file drags count** (`dataTransfer.types` has `Files`). Text and links are ignored:
   no overlay and no `preventDefault`.
4. **It always clears:** drop, leave, Escape (window, capture phase, so the dialog's own
   Escape handler cannot hide it), `dragend`, a drop anywhere else, and a zone that gets
   disabled (an upload is running).
5. **No navigation to a dropped file.** While a zone is mounted, a window-level `dragover`
   and `drop` cancel any file drag that no zone took (drop effect `none` outside a zone).
6. **Same upload code.** The rules moved to `lib/attachRules.ts` (allowlist, 100 MiB,
   one fixed message per refused file: `Not attached: name (reason)`). The paperclip and the
   drop share `addPicked`. The issue page uses `useAttachFiles` (prepare, PUT, attach),
   which got two optional callbacks (`onAttached`, `onFailed`); its return value is the same.
7. **Create dialog:** a drop queues files as the paperclip does (list, remove button, upload
   when the issue is created). A drop is ignored while the dialog is creating or uploading.
   Other files of the same drop still queue when one is refused.
8. **Issue page:** a drop anywhere on the page uploads and attaches the files, then
   invalidates the issue (the `attachment.created` event does too). Progress and errors are
   toasts, which are an aria-live region (`role=status` / `role=alert`).
9. **Markdown:** a drop that lands on a textarea (description editor, a comment or reply
   box; not the title) also writes the file at the caret: `![name](downloadUrl)` for an
   image, `[name](downloadUrl)` for any other file. `downloadUrl` is the API path, never a
   silo URL. The text goes in with the native value setter and an `input` event, so the
   controlled textarea keeps it. It is skipped when the editor closed meanwhile.
   The file is attached too, so it shows in Activity.
10. **Attachments off:** `503 attachments_unavailable` shows `Attachments are turned off on
this server.` once per drop (a toast on the issue page, the retry banner in the dialog).
11. **Not done:** paste of an image from the clipboard (nice to have). No per-file progress
    bar; the toast says "Uploading N files…" then "N files attached.". A drop on the
    sidebar or the shell outside the page is not a zone (it is cancelled, nothing uploads).
12. **Board drag (LAUN-29) and drag scroll (LAUN-55) are untouched:** they use pointer
    events, this uses HTML5 file drag events, and a file drag carries no card.
13. **Tests:** `fileDrop.test.tsx` and a `LAUN-58` block in `CreateIssueDialog.test.tsx`
    (fake `dataTransfer`). Browser check: headless Brave, real files through CDP
    `Input.dispatchDragEvent`, stubbed API, 1280 and 375 px; one real upload through silo on
    a `dev-ops-laun58` stack (removed with `down -v`).

## Text fields: no blue ring (LAUN-57)

The request (2026-10-26) was to remove the blue border when entering text in all fields. LAUN-19 had
done it for the create title only.

1. **A11y choice.** A text field (text, search, email, password, number, url, tel, a
   textarea, a `contenteditable`) shows the caret plus a quiet neutral border
   (`--field-focus-border`, slightly darker than `--border`, both themes). No blue, no
   outline, no box-shadow. Buttons, links, menu items, checkboxes, radios, switches, tabs,
   cards and rows keep the global `:focus-visible` ring (2px `--focus`). A `select` is not a
   typing field, so it gets the same quiet border, not the ring.
2. **One rule.** `src/styles/global.css`, after the `:focus-visible` rule: the selector is
   more specific than any module class, so no component needs its own override. An invalid
   field (`aria-invalid="true"`) is not red at rest (no CSS does that); it turns `--danger` only
   while focused. Disabled fields get no change. **Forced colors:** `outline: none` would hide
   focus there, so `@media (forced-colors: active)` gives text fields a 2px `Highlight`
   (system colour) outline, `!important` because the text-field rule is far more specific.
   A `contenteditable` has no border of its own, so it also gets a 1px inset neutral
   hairline (`--field-focus-border`) on focus, not the caret alone.
3. **Places that needed an override** (a ring drawn by a component, now removed or made a
   border): the filter bar search (`issues.module.css` `.search:focus-within`, a wrapper
   with its own border: outline became `border-color`), the settings sidebar search
   (`settingsSidebar.module.css`, same), the command palette input (`palette.module.css`
   `.input:focus-visible`, removed), the picker search (`ui.module.css`
   `.pickerSearch:focus-visible`, removed) and the issue page description region
   (`detail.module.css` `.desc`): NOT a typing field, so it keeps the keyboard ring
   (`:focus-visible` only; a mouse click opens the editor, which is quiet). No override there. Wrappers
   with `:focus-within` must set `border-color`, never an outline.
4. **Pin test:** `src/styles/focusCss.test.ts` (the global rule, the types it leaves out,
   the red state, forced colors, and a scan of every other CSS rule on `:focus*`: any outline,
   box-shadow, accent/focus/blue border or hard-coded blue is a failure unless its exact selector
   is on a short list of real controls). No existing test asserted a focus style.
5. **Browser check** (headless Brave, stubbed API, 1280 and 375 px, light and dark): the
   create dialog title and description, issue page title, description (region and editor)
   and comment box, filter bar search, picker search, settings sidebar search, an agent form
   field and the login email all read `outline-style: none`, `box-shadow: none` and the
   neutral border. A button, a link and a checkbox under Tab keep `2px solid` `--focus`.
   Gap: the list checkbox is not shown at 375 px, so its ring was checked at 1280 only.

## No underline on hover (LAUN-60)

1. **Cause:** `.row:hover .titleText` and `.card:hover .titleText` (issues.module.css) set
   `text-decoration: underline`, and `global.css` has `a:hover { underline }` for every link,
   which also hit the cycle list title and the relation links.
2. **Rule:** only an inline prose link underlines on hover. `global.css` has one underline
   rule: `a:not([class]):hover, a.textLink:hover`. Any link with a class (a row, card, button
   or nav link: issue rows and cards, cycle rows, relation links, settings row links, both
   sidebars, `ui.btn` links, the skip link) is never underlined; its cue is the background
   or colour, unchanged. The row and card rules in issues.module.css and cycles.module.css
   (`.title`, `.cardTitle`, `.titleText`, `.ident`, `.relLink`, `.sub`) also say `none`.
3. **Links that keep the hover underline:** the plain classless prose links (login, sign-up and
   invite pages, the platform and sign-ups settings text, the "Go to your organisations"
   notice, the create-dialog link, the parent link in the issue detail), and markdown and
   activity links (`class="textLink"`). A new prose link must have no class, or `textLink`.
4. **Focus:** the keyboard focus ring (2px `--focus` outline, on the overlay for rows and
   cards) is unchanged. No `a:focus-visible` underline rule exists.
5. **No project page:** the web app has no project page, so there is nothing to change there.
6. **Pin:** `src/styles/noUnderlineCss.test.ts`: the scoped rules are `none`, prose links keep
   the underline, and no module CSS underlines a row or card class on hover. Browser check:
   headless Brave, 1280 px, stubbed API: the first card and row in board and list view have
   `text-decoration-line: none` on hover (it was `underline` before); the "Ask to join" link
   on `/login` and `/signup` still underlines on hover.

## Cycles switch in Settings, Teams (LAUN-3)

1. **Where:** a "Cycles" column in the Settings, Teams table (`TeamCycles.tsx`), one switch per
   team, labelled "Cycles for ORB" (`role=switch`, `aria-checked`, the shared `SettingsToggle`).
   The table has three short columns, so it overrides the card's 680 px minimum width
   (`fitTable`) and fits at 375 px with no scroll; with the minimum the Cycles column sat off
   screen in the card's scroll area.
2. **API:** `PATCH /orgs/{org}/teams/{team}` with `{cyclesEnabled}` only. `Team` already has
   `cyclesEnabled` and the cadence fields, so the API needs no change. No server change and no
   migration. The 409 guard for the import job is a later ticket; the web shows any other
   server error as it is mapped (403, offline, 429, else a general text).
3. **Who:** an org admin gets the switch. Anyone else would see "On" or "Off" as text. (Settings
   itself is admin-only today, so this is a guard, tested on the component.)
4. **Turning on** opens a dialog first: import your cycles from Linear first, or the job makes a
   cycle 1 that clashes with the Linear numbers (decisions-T3 #35). Buttons "Cancel" and
   "Turn on"; focus starts on "Turn on". Cancel changes nothing and sends nothing.
5. **Turning off** has no dialog: nothing is deleted. A note under the table says what happens
   (decisions-T3 #12, #34): existing cycles stay with their dates and issues; the job makes no
   new cycle, completes none and moves no open issue; the team's Cycles sidebar link goes
   (it reads `cyclesEnabled`); turning on again continues the numbers.
6. **Optimistic:** `useSetTeamCycles` sets `cyclesEnabled` in the teams cache at once. On an
   error it puts back only that team's value and the page shows an alert ("ORB: Could not turn
   on cycles. The switch is back as it was. Try again."). It then refetches the teams and the
   cycle lists (the enable makes the first cycles on the server).
7. **Search:** the Teams page has the keywords "cycles", "turn on cycles" and "sprints".
8. **Tests:** `settings.test.tsx` (switch per team, dialog, cancel, off with no dialog, the
   body, rollback with the alert, the read-only text) and `settingsArea.test.tsx` (search).

## Cycle burn-up chart (LAUN-14)

1. **API:** no server change. `GET /orgs/{org}/cycles/{cycle}/snapshots` (read scope, in
   `router.go` and `openapi.yaml`) already returns the daily rows. No migration.
2. **One call:** `useCycleSnapshots` and `useCycleProgress` share one query key
   (`cycle-progress`) and differ only in `select`, so the page makes one snapshot call (the
   call refreshes today's row on the server). A `next` cycle makes none.
3. **Chart:** `BurnUp.tsx`, inline SVG, no new dependency. The model is `burnModel.ts`
   (pure): every day from the cycle's first to last day in the team zone, a row's scope and
   done counts (issues, not points) or `null` where the job wrote no row (the lines join the
   days with a row by a straight segment across a gap). Scope is a dashed `--muted` line, done a solid `--accent` line (so the two
   differ by more than colour), a dotted today marker, and tick labels thinned to fit.
   Width follows the container (`ResizeObserver`; 640 where it is missing, as in jsdom).
4. **Access:** the SVG is `role="img"` with an `aria-label` summary; a visually hidden
   `<table>` lists every day with a row. The pointer tooltip is `aria-hidden` (the table is
   the keyboard and screen reader path). The tooltip shows only for days with a row.
5. **Empty states:** no rows, "No burn-up data yet"; an upcoming cycle, "starts when the cycle
   starts". A failed snapshot call hides the chart (the header already shows "Progress
   unavailable" on the list page).
6. **Files:** `src/features/cycles/BurnUp.tsx`, `burnModel.ts`, `queries.ts`, `CyclePage.tsx`,
   `cycles.module.css`; the file `BurnUp.tsx` and `burnModel.ts` have different names on
   purpose (a `burnup.ts` beside `BurnUp.tsx` collides on a case-insensitive disk).
7. **Browser check:** headless Brave, stubbed API, light and dark at 1280 and 375 px: chart
   fits (no horizontal overflow), today marker and tooltip show. Gap: no points mode, no
   keyboard tooltip.

## Select all in a column, drag the selection (LAUN-61)

Code: `BoardView.tsx` (`ColumnCheck`, the group in `onPress`), `boardDrag.ts` (`press(e, issue, group)`,
`DropRequest.group`), `actions.ts` (`placeMany`), `IssueBrowser.tsx` (`toggleColumn`, `onDropGroup`),
`queries.ts` (`UpdateVars.deferSettle`), `issues.module.css` (`.colCheck`, `.dragBadge`,
`[data-drag-group]`). Tests: `groupDrag.test.tsx`. It builds on LAUN-29 (the drag), LAUN-51 (manual order:
a PATCH takes the state and at most one of `placeAfter` / `placeBefore` / `sortOrder`; `409
stale_placement`), LAUN-15/19 (the selection and the `N selected` status) and changes none of their rules.

1. **Select all.** Each board column header has a checkbox before the status icon (only when the column
   has cards). Label: "Select all N loaded in Todo" (N = the cards that are loaded); when all are selected:
   "Clear all in Todo". Click: all loaded cards of the column are added to the selection, or, when all
   were selected already, removed from it. Some selected: the box is mixed (`indeterminate`). It does
   not touch other columns, so a selection can cross columns. The keyboard path is the box itself (Tab,
   Space); no extra menu item was added. The board loads up to 20 pages, so "all" means the loaded cards
   (decisions LAUN-29 #15); cards that load later are not selected.
2. **Look.** Selected cards keep the existing selection look (`.selected`). The `N selected` `role=status`
   line (with Clear) already announces the count politely; the box changes call it. Escape (no drag)
   clears the selection, as before.
3. **Drag.** A press on a SELECTED card, with 2 or more cards selected, holds the whole selection (cards of
   all columns, in board order). The copy that follows the pointer is the held card with a round count
   badge ("3"). The other selected cards stay in their slots, dimmed (`data-drag-group`, an attribute so
   a React re-render keeps it); only the held card's slot closes up and the gap is one card high. A press on
   an unselected card, or with one card selected, is the LAUN-29 single drag, unchanged.
4. **The drop.** The gap says where. The neighbour is taken from the column WITHOUT any card of the group,
   per team (`placementAt`, LAUN-29 #6). The group lands as one block in its board order. A drop where
   every card is already in that column in exactly that order sends nothing (and says "canceled").
5. **Requests.** One `PATCH /issues/{issue}` per card, in a row (`placeMany`): the first card of each team
   goes to the drop place (`placeAfter` / `placeBefore`, with `state` when the column differs); each
   later card of that team goes `placeAfter` the last card of that team that WORKED. So the order is kept
   and a failed card is skipped without leaving a hole. No bulk route exists, so none was added; there is
   no migration. A card already in the state with no place to name (nothing to send) counts as done.
6. **Optimistic update and rollback.** All cards move in the cached lists at once (state, then place, in
   the same order as the requests). Each request has `deferSettle` so its `onSettled` does not refetch in
   the middle (a refetch would put the later cards back for a moment). When all are done, or after a
   failure, ONE refetch of the lists runs. Any failure puts the lists back to the snapshot taken before the
   drop, then the refetch shows what the server did. A toast says "N of M issues could not be moved" with the
   first reason ("The column changed; it was reloaded" for `stale_placement`). `stale_placement` stops the
   rest (they would fail the same way); another error continues with the next card.
7. **Escape, cancel.** Escape (and an outside drop, `pointercancel`, blur) cancels as in LAUN-29: nothing
   is sent, the selection stays, the live region says "Move of N issues canceled". Escape is taken
   before the page's own Escape. After a DROP the selection clears (`onDropGroup`), and the live
   region says "N issues moved to Todo".
8. **Phone and touch.** A touch press on a selected card does not start a drag when 2 or more are
   selected (so the page scrolls, and a long press does nothing odd). The column box shows only in select
   mode, like the card boxes (LAUN-16). On a phone the way to act on a selection is the bulk keys and pickers
   (the Select toggle, then S, A, L, P); a single card still drags by long press. There is no drag
   preview on touch.
9. **No regressions.** The drag-scroll (LAUN-55) starts only on empty space, so the header box and cards
   are unaffected; the card click (LAUN-50) and the click guard are unchanged (the group drag uses the
   same `markDragged`); the single-card path, `place`, and the manual order are untouched. A card still renders no
   React state during a drag.
10. **Limits.** The gap is one card high even for a group (the block appears on drop). The group copy is the
    held card only, not a stack. Mixed-team blocks keep order within each team only. Checked in headless
    Brave (Chromium) with a stub API; Safari, Firefox and real touch were not run.

## Settings, Notifications and the unsubscribe page (LAUN-36)

Design: `docs/internal/decisions-T13-notifications.md` #38 and #25. The server computes every resolved value and
its source (`GET /me/notification-settings`); the page never recomputes one.

1. **Who opens it.** `/:org/settings/notifications` is open to every org member. `SettingsLayout` lets that path
   (and the bare `/settings`, which only redirects) through for a plain member; every other settings path keeps its
   gate and still shows the "You need admin access." notice. The bare `/settings` goes to Organisation for an org
   admin, Platform for a platform admin, else Notifications. The server stays the judge of every write.
2. **Where it is.** A new group "Account" in the settings sidebar (first, before Administration), made of pages with
   `access: 'member'`. The user menu (bottom of the app sidebar) shows Settings to everyone now. The organisation
   menu keeps Settings for admins and platform admins only. The page is in the sidebar search too.
3. **Layout.** Linear style: the shared `SettingsPage`, `SettingsSection`, `SettingsCard`, `SettingsRow` and
   `SettingsToggle` (LAUN-26). Sections: Email (master switch, excerpt switch), What you hear about (one group per
   event), Teams (a Follow switch per team and a disclosure "Override for KEY").
4. **Three-state control.** A native `select` per event and channel (Email, Inbox): the unset choice, On, Off. Global
   level: "Default (On)" names the built-in value. Team level: "Inherit (On)" names the value you would get with no
   override (the global value if set, else the default). Under each control one line says "Now On (your setting)",
   "(the default)" or "(set for this team)"; with the master email switch off the Email lines say "Off: email is
   turned off above" (the server already resolves them to off). A select was chosen over a segmented control: it is
   keyboard and screen reader complete with no extra code, and fits a phone.
5. **Saving.** Each change saves at once. Writes run one after the other (a chain of promises), so a slow reply never
   lands over a newer one and no control is disabled (a disabled control would drop focus). Every reply is the whole
   resolved view and replaces the cached one. A follow answers 204, so the cached `follows` list is edited. A short
   "Saved." line is a `role=status`; a failure is a `role=alert` with a fixed sentence. No server text is shown.
6. **Mail note.** When `mail.available` is false the page says "This install cannot send email, so nothing below
   reaches your mailbox. The inbox still works." (`role=note`). The server decides `available`; the web app does not read
   the platform settings, which a member cannot. Since the LAUN-36 review, `available` is the mail switch being ready AND
   `PUBLIC_BASE_URL` being set (`notify.MailReady`, wired in `cmd/server/main.go`): with no base URL the sender skips every
   mail (`no_base_url`), so the note must show. A provider of `none` or a missing transport also gives false.
   Other review changes: a failed team load shows a fixed error with Try again (not "no teams yet"); New issues shows
   "Only for teams you follow" beside its resolved values; each "Now ..." line is the `aria-describedby` of its select;
   the override disclosure is at least 32px high and its +/- mark is hidden from screen readers.
7. **Unsubscribe page.** `/unsubscribe/<token>` is public (outside `RequireAuth`). Opening it sends nothing; only the
   button sends `POST /api/v1/notifications/unsubscribe/{token}`. The route cannot be asked what a token covers (a GET
   is 405 by design and the POST answers 204 for any token), so the page says what the action does in general: it turns
   off email for the kind of notification that sent the link and changes nothing else. The "Done" text says "If this
   link was valid" because the server never tells. A token that is not 43 base64url characters, or a 400, shows "This
   link does not work" with no request (or no server text). 429 and network faults keep the button. The token is
   never shown, never put in the page title and never stored; it is only in the address and the one POST.
   The POST is exempt from the CSRF header by a path prefix in `api/http.ts` (it reads no cookie, and a signed-out
   person has no CSRF token).
8. **Not verified.** No real browser run: the phone layout (one column under 420px, two under 760px) and the look in
   dark mode come from the CSS and the unit tests only. Not tried against a real server in this ticket.

## Notifications 7/9: the web inbox (LAUN-37)

Code: `api/notifications.ts` (hooks, `InboxStream`, `useInboxLive`), `features/inbox/` (`InboxPage`, `InboxBell`,
`InboxList`, `InboxControls`, `inboxModel.ts`, `inbox.module.css`), `Shell.tsx`, `ShortcutHelp.tsx`. Tests:
`api/notifications.test.ts`, `features/inbox/inbox.page.test.tsx`, `inboxModel.test.ts`. Design:
decisions-T13 #27 to #31 and #39; API: LAUN-35. No server change.

1. **Where it shows.** The sidebar has an Inbox entry first in the list (`/:org/inbox`) with the unread badge. Next to
   Create issue (sidebar) and in the phone top bar there is a bell with the same badge; it opens the quick
   triage popover. This follows the ticket (Linear's sidebar has Inbox at the top); decisions-T13 #39 put the bell
   in a top bar, which a wide screen does not have. "My issues" took the person icon so the two do not share one.
2. **The badge.** It is the `unread-count` query (`keys.unread`). Nothing shows at 0; above 99 it reads "99+". The
   digits are `aria-hidden`; a screen reader gets "N unread". The stream's `count` event sets the query data
   directly, so the badge follows without a request.
3. **The stream.** `InboxStream` opens `GET /notifications/stream` (cookie sessions; EventSource cannot send a
   bearer token, so a bearer session runs with polling only). `count` sets the badge; when the count differs from the
   cached one the lists are refreshed too (a read in another tab moves the count and no row). `notification` events
   refresh the lists and the count, batched in 80 ms. A drop reconnects with backoff and `?after=<last inboxSeq>`
   (as `OrgEvents` does), probes the session before each retry, and refreshes the lists once when the stream is
   back (rows may have changed while it was down). It is separate from `OrgEvents`: that stream is the org's change
   feed, this one is the person's own rows.
4. **Polling fallback.** While the stream is not `live` (never opened, down, or poll mode) the unread count is read
   every 60 seconds (decisions-T13 #31) and a changed count refreshes the lists. While it is live nothing polls.
5. **Lists.** One infinite query per filter: Unread (`state=unread`), All (`state=all`), Archived
   (`archived=true`), 30 per page and a "Load more" button (the board and list use the same cursor pattern). The
   filter is in the address (`?filter=all`; Unread is the default and adds nothing), so Back and a reload keep it.
   The popover uses the same queries with Unread and All only, so it can show more than the 10 rows #39 named; the
   page shows the same cache and a mutation updates both.
6. **Row.** Unread dot, actor avatar (a dashed circle when there is no actor), issue identifier and title (bold when
   unread), one line of what happened (`inboxModel.summary`: "Ann assigned this to you", "Ann moved this from Todo to
   Done", a move by GitLab or the system reads "Moved from X to Y"), and the time. No comment text (the API sends
   none). Hover or focus shows two buttons in place of the time: read / unread and archive / unarchive. On a device
   with no hover they are always shown.
7. **Mutations.** `PATCH` sets a state, so the web app sends the state it wants (`read: true`, `archived: true`).
   The cached rows and the badge change at once (`useUpdateNotification`): a row leaves any list it no longer
   belongs to (`inFilter`), the badge moves by the change of `counts(row)`. On an error everything goes back to the
   snapshot and a toast gives the message. After every call the count and the lists are fetched again, so the
   server has the last word.
8. **Mark all read.** It sends `before` = `createdAt` of the NEWEST ROW LOADED in the list on screen (the API
   requires it; a retry then marks the same rows). It is hidden in Archived and disabled with no unread row loaded.
   The badge goes to the number of unread rows newer than `before` that are in the cache (normally 0), then the
   refetch corrects it.
9. **Opening an issue.** A click on the row (also a middle click or Ctrl click; the page opens either way) marks the
   row read and goes to `/:org/issue/:identifier`. Enter on the focused row does the same.
10. **Keyboard.** On the page: `j` / `k` move the focus (the first row starts focused; `aria-current` marks it, like
    the issue list), `Enter` opens, `e` archives (unarchives in Archived), `u` toggles read. After a row leaves the
    list the next one takes its focus. `e` is free in the hotkey map (the issue list uses s, a, l, p, x). Plain keys
    stay quiet while typing and while a dialog is open (`useHotkeys`). The popover is focused when it opens and has
    the same keys on its own list; while it is open it registers as a modal, so the page keys do not also fire.
    Tab moves inside it; Esc closes it and puts focus back on the bell. `ShortcutHelp` lists E and U.
    Linear's `g i` (go to inbox) is not built: the hotkey map has no key sequences.
11. **Aria.** The list is `ul[aria-label=Notifications]`; the filter is a `role=group` of buttons with
    `aria-pressed`; the row buttons name the issue ("Archive: ORB-1"); an unread row says "Unread." to a screen
    reader; the popover is a labelled region ("Inbox quick view"), the bell has `aria-expanded`.
12. **States.** Loading is the skeleton; an error is `ErrorState` with Try again; each filter has its own empty text
    ("You are all caught up", "No notifications yet", "Nothing archived").
13. **Phone.** Rows keep one line each (the text cuts with an ellipsis), the head wraps, the keyboard hint is hidden,
    the popover is at most the viewport width less 16 px (`Floating`).
14. **Tab title.** Not built, as the ticket says. The place for the unread prefix is `useDocumentTitle(parts, prefix)`
    (LAUN-30); `useUnreadCount()` gives the number. The page sets "<Org> › Inbox".
15. **Tests.** `FakeEventSource` now has `eventStreams` / `lastEvents` / `inboxStreams`, because the shell opens two
    streams; the two older tests that counted all streams use `eventStreams`.
16. **Not seen in a browser.** The layout, the hover buttons, the popover position and the phone width were
    checked only in jsdom (no layout there). Not run: a real EventSource against the server, Safari, Firefox.
17. **Review round 1.** The popover is a `role=dialog` labelled "Inbox quick view". It focuses the first row link (else
    its first control), never the list box, so Floating's Tab handling counts it and Tab / Shift+Tab move inside;
    Esc returns focus to the bell. When the focused row leaves the list, focus moves to the next row link so the keys
    keep working. The list scrolls into view only after a key move, so a new top row from the stream does not move a
    reader who scrolled down. Under `(max-width: 760px), (pointer: coarse)` the filter tabs, row buttons, bell and
    Mark all as read are 40px or more (`inbox.layout.test.ts`). The stream resets its backoff on the first `count`
    event, not on `open`. Mark read and unread both show `(u)` in the title.
18. **Review round 2.** The highlighted row and the DOM-focused control are one thing: a focus on any control of a row
    highlights that row (`onFocus` on the `li`), and j / k move the DOM focus to the row's link, so Enter, e and u act
    on the row the person sees. The highlight is `data-focused` (no `aria-current`: the DOM focus is the current
    item). While the list loads, the popover focuses its first control at once (Tab and Esc work), and moves to the
    first row link when rows arrive unless the person went elsewhere. The loading skeleton has `aria-busy`. Only one
    bell shows at a time: the top bar is `display: none` except on a phone, where the sidebar is a closed drawer
    (`inbox.layout.test.ts`).
19. **Review round 3.** Floating stops the browser from moving focus on a press, so a row's `onMouseDown` focuses the
    pressed button or link itself (else the row link); the row's `onFocus` is then the only thing that sets the
    highlight. After `e` or `u` removes the focused control, focus goes to the next row link on the page too (when
    focus was in the list). `j` / `k` at an end of the list set no pending focus move. The bell's name carries the
    count ("Inbox quick view, 3 unread").

## Ticket detail: Back button (LAUN-89)

1. **Where.** The first control in the top bar of the issue page, left of the team link (`detail.barLeft`). It is a
   `button type=button` named "Back" (an arrow, then the word). It is a normal tab stop. The team link may wrap
   (`min-width: 0`, `overflow-wrap: anywhere`) so a long team name cannot overflow at 320px.
2. **One helper.** `lib/backNav.ts` has `hasNoEarlierEntry` and `useGoBack(fallback)`. The Back button and the
   Escape key both use it, with the same fallback: the team's issue list, `/:org/team/:key`.
3. **What it does.** With an earlier entry it calls `navigate(-1)`: the same result as the browser back button or
   Alt+Left, which are unchanged. Filters, grouping and layout live in the URL and come back with it.
4. **No earlier entry.** The test is `window.history.state.idx === 0` (react-router keeps `idx`). `location.key ===
'default'` is only the fallback where there is no `idx` (MemoryRouter in tests). The key alone is wrong: the
   logged-out redirect (`Navigate replace` to /login) and the login `replace` give the first entry a random key,
   and Back would then call `navigate(-1)` with nothing earlier (a dead button in a new tab, or it leaves the
   app). The idx test covers that case.
5. **Scroll position.** The app scrolls inside containers, not the window, so `ScrollRestoration` does not help.
   `useScrollRestore` (same file) saves `scrollTop` / `scrollLeft` of one container per history entry (key, path,
   query) in a module map (at most 100 entries, oldest dropped), and puts it back when that entry comes back. It
   tries each frame for about 2 seconds, so rows that load late still count; a restore that needs more pages than
   the first fetch gives up after that time. A wheel, touch, key or pointer action on the container cancels it.
   The map is in memory: a reload forgets it. The container is the one that really scrolls in each view:
   - Kept: the issue list in list view (`.scroll`, on the team, my issues, all issues pages and the cycle issue list in
     list mode); the board sideways (`.board`, `scrollLeft`).
   - Kept since LAUN-90: the inbox list on the page, the inner scroll of each board column, the cycle list page and
     the cycle page without the issue browser (see "Back navigation: more containers (LAUN-90)" below).
   - Not kept: settings pages.
     Escape on the issue page now uses the same helper, so its fallback changed from `/:org/issues` to the team's
     issue list.
6. **Phone.** Under `(max-width: 760px), (pointer: coarse)` the button is at least 40px high and wide. It uses the
   `ghost` button tokens, so dark mode needs nothing new.
7. **Tests.** `detail.test.tsx`: with history (a list entry with a query, then the issue), without, after the login
   redirect (idx 0 under a non-default key), Escape fallback, and the button role and place. `backNav.test.tsx`:
   the scroll offset comes back. `renderApp` also takes a list of entries.
8. **Not seen in a browser.** The layout, the 40px target, dark mode, and the real scroll restore (jsdom has no
   layout, so the test only proves the offset is set again) were not run in a browser.

## Web inbox: a failed refetch keeps the rows (LAUN-88)

Code: `features/inbox/InboxList.tsx`, `inbox.module.css` (`.inlineError`). Tests: `features/inbox/inbox.page.test.tsx`
("inbox failed refetch"). Found by the review of LAUN-37.

1. **The rule.** The full error state (`ErrorState`, "Could not load this") shows only when the query has an error AND
   no data. A failed first load looks as before.
2. **With data.** TanStack Query keeps `data` and sets `isError` after a failed background refetch (for example a
   stream event that invalidates `inboxAll`) or a failed `fetchNextPage`. The list keeps every loaded row (and the
   empty state, if the loaded list is empty) and shows one `role=alert` line above it: "Could not refresh." with a
   Try again button, or "Could not load more." after a failed Load more (its button calls `fetchNextPage`; the Load
   more button stays too). The button is disabled while a fetch runs. The line leaves when a fetch succeeds.
3. **No flicker.** The row elements are the same before, during and after the failure: no skeleton, no remount.
4. **Not seen in a browser.** The look of the line (colour, spacing) and its size on a phone were not run in a browser.
   The retry and Load more buttons have a 40px phone rule (`inbox.layout.test.ts`); jsdom cannot show it.

## Notifications: Subscribe control (LAUN-38)

1. **Where.** In the Activity header of the issue page, right of the "Activity" heading
   (`features/issues/SubscribeControl.tsx`, used by `Activity.tsx`). A sentence, then a ghost button
   "Subscribe" or "Unsubscribe". The sentence wraps; on a phone the button is 40px or more.
2. **State and reason.** The visible sentence REPLACES the tooltip of decisions-T13 #40 (it works on touch and for screen readers). `GET /issues/{issue}/subscription` gives `state` and `reasons`; the page only words them
   (fixed text, nothing from the server is shown): "You are subscribed because you are the assignee, you commented."
   (`creator`: you created it; `assignee`: you are the assignee; `commenter`: you commented; `mentioned`: you were mentioned; `manual`: you
   subscribed), "You muted this issue. You get no notifications about it." or "You are not subscribed."
3. **What the button does.** "Subscribe" sends `PUT {subscribed: true}`. "Unsubscribe" (shown when the state is not
   `muted` and there is a reason) sends `PUT {subscribed: false}`, a mute, when an implicit reason holds (creator,
   assignee, commenter), because a mute row wins over them. When only a row of the person's own is left (`manual`,
   `mentioned`) it sends `DELETE`, back to "not subscribed", and the query is read again. A PUT answer replaces the
   cached state. The state is the server's, so it is right after a reload.
4. **No broken control.** While the state loads, and when the read fails (403 for an agent token, 404, network), the
   control renders nothing and the query does not retry. A web session cannot hold a read-only token, so "read-only"
   is the refused read; a refused write (403) shows "You cannot change this subscription." and keeps the button.
5. **Keyboard and aria.** A real `button type=button`: Tab, Enter, Space. While a write runs the button is
   `aria-disabled` + `aria-busy` (clicks are ignored) and not `disabled`, so keyboard focus stays on it. The sentence is a
   `role=status` (polite); a failed write shows its fixed text in a separate `role=alert`.
   The Activity header (`blockHead`) wraps, so on a phone the sentence drops under the heading.
6. **Tests.** `subscribe.test.tsx`: the wording; mute and reload; subscribe by keyboard; DELETE of a manual row;
   403 on read (nothing shown); 403 and other failures on write (`role=alert`); Space; a comment and a self-assign change the reason; place and roles. The mock keeps a row like the server.
7. **Fresh state.** The server computes creator, assignee and commenter live, so a new comment, a deleted comment and
   every issue update invalidate the cached subscription (`keys.issueSubscription(s)`). A DELETE does not wait for its
   re-read: a failed re-read is not a failed save. The change feed
   (`issue.created`, `issue.updated`, `comment.created`, `comment.deleted`) and the drag-to-group settle (which
   skips the normal `onSettled`) invalidate it too, so a colleague's assignment or a second tab shows without a reload.
8. **Not seen in a browser.** The layout in the header, the wrap at 320px, the 40px target, the focus ring and dark mode
   were not run in a browser.

## Git connections (LAUN-9)

Code: `features/settings/git/` (`IntegrationsPage`, `IntegrationPage`, `ConnectDialog`, `RepoSection`, `RotateSection`,
`ReplaceTokenDialog`, `gitParts.tsx`, `gitText.ts`), `api/gitIntegrations.ts`. Design: `decisions-T17` #46, #47, #51.
Tests: `features/settings/git/gitIntegrations.test.tsx`.

1. **Pages.** Settings, Integrations (`integrations`, `integrations/:integration`), group Administration, access
   `admin`, with `keywords` for the settings search. A member sees the "You need admin access." notice and no call is made.
2. **Provider table.** `PROVIDERS` in `gitText.ts` holds the hosted address, the minimal token scopes (T17 #23), the
   host's token page and the hook help (header, event). Bitbucket is one more row there once the server accepts it
   (`CreateGitIntegrationInput.provider`; LAUN-84). Nothing else in the UI names a provider.
3. **Secrets.** The token (connect, replace) and the webhook secret (connect, rotate) use plain async functions
   (`useCreateGitIntegration`, `useReplaceGitToken`, `useRotateGitSecret`): no `useMutation`, no `setQueryData`; they only
   mark the list, the integration and its repos stale. The secret lives in component state and is cleared as soon as the
   call returns (also when it fails). The fields are `type=password`, `autocomplete=new-password`, no `name`, with
   password-manager ignore hints. A test dumps the query and mutation caches, storage, the URL, the page and every later
   request body, and finds neither the token nor the secret.
4. **Copy once.** `SecretOnceDialog` follows the token dialog: a warning, Copy, a confirm box, Done off until ticked;
   Escape, the close button and a click outside do not close it before. It also asks the browser to warn on unload.
5. **Errors in plain words.** `gitErrorText` chooses a fixed sentence from the status and `code` (`provider_*`,
   `integration_limit`, `encryption_unavailable`, `public_url_required`, `rate_limited`, ...). Server text never reaches the
   screen. A 400 is read for a field name (`baseUrl`, `secret`, `token`) and then shows a fixed field text; the address
   text names `GIT_ALLOW_PRIVATE_PROVIDERS` for the SSRF case. The check `login` is the only host data shown (cleaned).
6. **Connect dialog.** Three steps (provider and address; mode and credentials; done). The focus moves to the first
   field after each step and after Back. Webhook only shows the URL (a bare path gets the PUBLIC_BASE_URL warning) and,
   for a made secret, the copy-once screen with the hook help. A pasted GitLab secret is never shown again.
7. **Detail page.** Banner per `attentionCode` (T17 #11a). The disable sentence (T17 #13) shows next to the switch and
   in the confirm. Check token, replace token (or add one to a webhook-only connection, which sends `mode: token`),
   enable, disable.
8. **Repositories.** Token mode: "Find repositories" calls `available-repos` only when pressed (the route allows 6 a
   minute, so nothing refetches by itself); Load more passes the opaque cursor back; a short GitHub page with a cursor
   says "Load more". Webhook only: id (1 to 18 digits) and path. A team picker per mapping and per new mapping. Unmap
   says when `hookRemoved` is false. The actions cell has `data-slot="repo-actions"`: the place for the two buttons of
   LAUN-83.
9. **Rotate.** Token mode: rotate, then `failedRepos` (from the repos' `hookState`) with Try again, Commit anyway
   (confirm) and Discard (confirm, with the 401 warning). Webhook only: the new secret once, then Commit or Discard.
   The pending state comes from `secretPending`, so a reload shows it again.
10. **Not built here (LAUN-93).** The last delivery on the list and the deliveries table. The server part
    (`/deliveries`, `lastDelivery`) is merged (LAUN-87); the UI part is LAUN-93: a Deliveries section after Rotate on
    the detail page and a last-delivery cell on the list row. Until then the list shows status and reason only.
    **Unverified, written from memory of the vendors' docs, no live host:** the token-page paths (`gitText.ts`: GitLab
    `/-/user_settings/personal_access_tokens`, GitHub `/settings/personal-access-tokens/new`) and the hook help text
    (events, header names, content type).
11. **Touch targets.** `.touch` (git.module.css) sits on the git pages and, through the `className` prop of `Dialog`, on
    every git dialog. Under `(max-width: 760px), (pointer: coarse)` it gives buttons, selects, text, search and
    password inputs, radio and checkbox labels and the back link 40px. `git.layout.test.ts` reads the CSS.
12. **Retry and partial failure.** Pressing Search again with the same search retries a failed Find (or only the next
    page after a failed Load more); with no error it asks nothing. Pages already loaded stay with the error beside them.
    The "Mapped" mark comes from the live repo list. The Rotate section does not wait for the repo list.
13. **Not seen in a browser.** Layout, the 40px targets, dark mode and a real screen reader pass were not run. The tests
    check roles, names, focus and keyboard order in jsdom only.

## Back navigation: more containers (LAUN-90)

Code: `lib/backNav.ts` (`useScrollRestore`, `useEntryMemory`), `features/inbox/InboxList.tsx`, `features/issues/BoardView.tsx`
(`ColumnCards`), `features/issues/IssueBrowser.tsx`, `features/cycles/CycleListPage.tsx`, `CyclePage.tsx`. Tests:
`lib/scrollRestore.test.tsx`. This extends the scroll rules of LAUN-89 (#5 above).

1. **Containers now kept.** The inbox list on the inbox page (`.pageList` now has `overflow-y: auto`: before, the
   page's one scroll area `main` moved the whole page, header included); one hook per board column (`ColumnCards`, name
   `col:<column key>`); the cycle list page (`.scroll`); the cycle page when it is not an issue browser ("Cycle not
   found", `.scroll`). The cycle page with issues was already kept by the issue browser. The inbox popover is not kept
   (it remounts on every open).
2. **Selected inbox row.** `useEntryMemory(name)` keeps one string per history entry, in memory (100 entries, oldest
   dropped), like the scroll offsets. The inbox page keeps the highlighted row id there and starts from it on Back. A
   row that is gone falls back to the old rule (the nearest index). The cycle list keeps "Show all N past cycles" the
   same way, or an offset inside the long list would be out of reach.
3. **More pages.** `useScrollRestore(name, ref, enabled, loadMore?)`: while the offset is out of reach the hook calls
   `loadMore()` each frame. The caller starts the next page when none is on the way and returns true while it waits
   (false: nothing more). The wait is then up to 10 seconds in all, not 2. The issue list and the inbox load at most 10
   pages (500 issues, 10 inbox pages), and stop at once when a page fails (`isFetchNextPageError`: no retry each frame;
   the error line of LAUN-88 and its Try again take over); the step is the pure `nextPageStep` in `backNav.ts`, asking once
   per history entry and page count. On the issue list a failed page shows the full error state (the rows leave); its
   retry refetches from the top, so the offset is not restored there (pre-existing wipe behaviour). Beyond 10 pages the
   restore stops where it can. The board already loads up to 20 pages by itself, so its hooks only wait while `loadingMore` is true.
4. **When it matters.** A page is cached by TanStack Query, so Back usually finds every loaded page at once. The loading
   covers a cache that was dropped or a refetch that starts again from page 1.
5. **Tests.** Each container has a test that scrolls it (and for the inbox, moves the selection with j), opens an issue,
   presses Back and checks the NEW element has the offset. The pager tests use a scrollTop that stops at the content
   height, with a control that shows the offset is lost without `loadMore`. The tests were checked by breaking each
   restore path in turn (see the MR).
6. **Not seen in a browser.** jsdom has no layout and does not stop scrollTop at the content height (except in the pager
   tests, which fake it), so the tests prove the offset is read, kept per entry and written back to the right container;
   not that Chrome, Safari or Firefox scroll there. Not run: the inbox page with `overflow-y: auto` (the header and the
   hint stay in place and the list scrolls between them), column scroll after the board's late cards, the phone width, and
   the three browsers the ticket asks for.
7. **Focus ring.** The highlighted inbox row is marked with an inset `box-shadow` (`.focused`), so the new scroller at
   `.pageList` does not clip it. The link's own focus outline was not checked at the list edge in a browser.

## Import from Linear wizard (LAUN-10)

Server rules: `decisions-T17-git-connections-import.md` #36 to #52. Code: `ImportPage.tsx`,
`ImportSteps.tsx`, `ImportUserMap.tsx`, `ImportParts.tsx`, `importText.ts`, `src/api/importer.ts`.
Tests: `import.test.tsx`, `import.layout.test.tsx`.

1. **Where:** Settings, "Import from Linear" (`/:org/settings/import`), Administration group, org
   admins only (the same gate as Teams). `keywords` are in `settingsPages.ts`.
2. **Five steps, one source of truth.** `GET /import/sessions/current` decides the step
   (`stepsFor` in `importText.ts`): a 404 or `new` or `exporting` is step 1, `exported` is 2,
   `user_map` is 2 until a map is saved and 3 after, `dry_run_ok` is 3, `importing` and `imported`
   are 4, `verified` is 5, `failed` is the step of the most recent job. A running job moves the view
   to its own step. The step buttons open any step up to the furthest the state allows; a state change
   from the server takes the view back to the current step. A reload therefore resumes the view.
3. **Polling.** The page polls the session (it holds the last job of each kind, with `phase`,
   `done`, `total`) and not each job. 2 s while a job runs or the state is `exporting` / `importing`;
   each failed poll in a row doubles the wait (a success resets it) (4, 8, 16, at most 30 s); no poll when nothing runs. The
   observer stops on unmount; the browser pauses it in a background tab (react-query default). A
   failed refetch keeps the last data and shows a banner.
4. **The Linear key.** A `type=password` field with `autocomplete=off`. On submit the value is copied
   to a local, the state is cleared at once, and when the form unmounts React drops the state with it. The export call (and
   every other write) is a plain async function, never `useMutation`, so no mutation cache holds the
   body. There is no Idempotency-Key (the server refuses one). The key is in no URL, storage, query
   cache or log. No error shows server text: `importErrorText` picks a fixed sentence by status and
   code (`linear_unauthorized`, `linear_unreachable`, `no_space`, `job_active` ...). A 401 signs the
   person out as everywhere else; the form is gone with the key. The field accepts printable ASCII
   only. The session is made by the same submit when none exists, so a missing file store
   (`attachments_unavailable`) or public URL (`public_url_required`) shows on that form.
5. **Failed files.** Step 1 shows the session's `failedFiles` count and at most the first 20 names.
   The dry-run button in step 3 is disabled until the box "I read the list of files that failed" is
   ticked, and only then sends `{acceptFailedFiles: true}`; with no failed file it sends `{}`.
6. **User map.** One row per Linear user: a member picker that starts from the saved map, else the
   server's suggestion, and a default member. "Create agent" calls the agent route at once and
   selects the new username; no human is made here (decisions-T17 #45). Saving needs a member for
   every non-app person or a default (a client check; the server's 400 shows as "Every name must be a
   member"). Saving makes an earlier dry run out of date, and the page says so.
7. **Dry-run report.** A table of counts per entity (in the export, left out, would create, update,
   skip), `Uploads.Planned` as "A real run would upload N files", merged labels and relations, and a
   collapsed list of the first exclusions with the total. A report from before a map change is shown
   with "out of date".
8. **Real run.** Needs the typed org slug; the button stays off until it matches. The note above
   it says a stopped import keeps no rows and that edits to imported rows are lost on a re-run. It
   is never called "resumable". A re-run changes the button to "Run the import again". Cancel is on
   every running job. A `verify_mismatch` shows the #42c text by `committed` (false: nothing was
   written; true: the rows were written, run verify or the import again) and the verify rows
   (entity, expected, found, with the first records of a difference).
9. **Cycles.** Step 5 lists each team with the shared `TeamCyclesControl`. The `import_in_progress`
   409 (turning ON while a session is in `exporting`, `exported`, `user_map`, `dry_run_ok` or
   `importing`) shows "An import is running. Finish it or close it in Settings, Import from Linear,
   then turn cycles on." on the Teams page and here (`cyclesErrorText`).
10. **Close.** "Close this import" (a confirm dialog; focus starts on "Keep it", the safe button) calls `DELETE /import/sessions/{id}`; it is
    the way out of a session that blocks cycles. It shows only when no job runs.
11. **Touch targets.** On a phone or a coarse pointer (`@media (max-width: 760px), (pointer: coarse)`)
    these are at least 40px high: inside the page (`.page`) the step buttons, buttons, selects, inputs
    (not tick boxes), summary toggles, tick-box label rows and `role=switch` controls (the cycles switches in
    step 5); inside the Create agent dialog (`.dialogForm`), its buttons and inputs; the Close dialog
    (body in `.dialogForm`); and the buttons in the "Turn on cycles" confirm dialog
    (`cyclesDialog.module.css`, also used on the Teams page). Dialogs are drawn in a portal, so each
    carries its own class. **Not raised:** the X close button of every dialog (it lives in `ui/Dialog.tsx`,
    which another ticket changes), and the cycles switch on the Teams page itself. `import.layout.test.tsx`
    reads the CSS and renders the Close dialog and the cycles dialog to check their buttons sit inside the
    class (jsdom has no layout). The layout test does not cover the Create agent dialog (`.dialogForm` there is
    set by the CSS only).
12. **Steps off.** A step button the state does not allow is disabled and carries the reason in
    `title` and `aria-describedby`. After the import ran (`imported`, `verified`) steps 2 and 3 are off:
    the server refuses a new user map or dry run there; "Close this import" and export again to change them.
    A running job has one progress line with `role=status` (`JobProgress`); the page banner is plain text.
    Other `role=status` lines exist, shown with their own state: result lines such as "Verify passed", the
    failed-files list (`FailedFiles`) and the step 3 "out of date" banner. The progress line is the only
    one that updates while a job runs.
13. **Not verified in a browser.** The tests run in jsdom against a mocked API; no real Linear
    export and no real server were used.

## Sidebar product groups and the Product filter (LAUN-64)

Code: `features/shell/sidebarGroups.ts` (the pure model), `TeamGroups.tsx`, `sidebar.test.tsx`; `useProducts` in
`api/queries.ts`; the chip in `FilterBar.tsx`; `product` in `features/issues/filters.ts`. Design: `decisions-T18-products.md`
#22 and #24. The product page is LAUN-65; the Products settings page is a later ticket.

1. **Groups.** A product with at least one team to show is a group, in the order the server sends (by name). "Other teams" is
   last. A team shows under each live product in `Team.productIds` (the teams list is the source, so one refetch of it moves a
   team). A product with no team to show has no group; when no group is left the sidebar is the flat Teams list, with the
   same markup as before products (also while the product list loads or fails to load). This differs from T18 #7 in one
   way: an empty product does not turn "Other teams" on.
2. **Collapse.** The header is a `button` with `aria-expanded`. A closed group renders no rows (not CSS-hidden), so Tab
   and screen readers never reach them. The choice is `localStorage` `devops-nav-group:<product id>` (`1` open, `0` closed);
   "Other teams" uses `devops-nav-group:other:<org>`. Reads and writes are in try/catch, and a click also goes to
   component state, so it works when storage throws (it then lasts until the page closes).
3. **Default state.** With at most 12 team rows (every group open; a team in two products counts twice) all groups start open.
   With more, only a group that holds the active team starts open. A stored choice wins over the default.
4. **Auto-open.** When the route is a team page, every group that holds that team opens. This is kept in state only, never
   written to storage, and a click on the header closes it again. It is added to when the route changes, so a group you
   left stays open until you close it.
5. **Rows.** Row keys are `<product id>:<team id>`. Each group is its own parent, so a clash was not possible anyway; the
   key follows the design. Every row of the team lights up (the route knows only the team). The team menu and the Cycles
   group of a row are named with the group ("Team ALP team options in Alpha") so two rows of one team differ. Count and
   product mark are `aria-hidden`; the mark is the icon text and a colour dot (only a `#rrggbb` colour is used), so no
   text sits on a user colour.
6. **Filter chip.** `product` (a slug) is in the URL, in the query to the server and in the filter count; Clear removes it.
   The chip shows when the org has a live product, or when the URL has a product (so a stale slug can be cleared). It is not
   shown on a team page (the team is fixed there). No Team chip exists in the bar, so "next to Team" has no place to go;
   the chip is first.
7. **Palette.** "Go to product" entries open `/<org>/issues?product=<slug>`. LAUN-65 points them at the product page.
8. **Events.** `product.*` events refetch the products list and the teams list; `team.updated` (it was missing) refetches
   the teams list. All five kinds are in `EVENT_TYPES`, which the stream needs because the server sends named events.
9. **Touch targets.** Under `(max-width: 760px), (pointer: coarse)` the header, the team rows, the Cycles links and the team
   menu button inside groups are 44px. The flat list keeps its 32px rows. Pinned in `styles/layout.test.ts`; header and
   count colours are pinned in `styles/contrast.test.ts` by reading the CSS.
10. **Not seen in a browser.** jsdom has no layout, no focus ring and no media queries. The tests prove roles, names,
    `aria-expanded`, keyboard toggling, what is in the DOM, storage keys and URL round trips. They do not prove the 44px
    sizes, the chevron turn, dark mode, the drawer at phone width, or a real screen reader. The 12-row rule and the
    auto-open feel are from the design and have not been tried with a person.

## Settings, Products (LAUN-66)

Page `/:org/settings/products` (admin, group Administration, after Teams). Rules: decisions-T18 #24 and the P5 list.

1. **One page, three dialogs.** The list is stacked rows (no table, so a phone needs no scroll): icon and colour swatch,
   name, slug, team keys, and the buttons Assign teams, Edit, Archive or Restore, and (archived only) Delete. Create and
   Edit share one form dialog. "Include archived" is a checkbox that adds `includeArchived=true` to the list call.
2. **Teams save at once.** The Assign teams dialog is a checklist of all teams. Each tick is one `PUT` or `DELETE
.../products/{p}/teams/{team}` (replay-safe), not a "Save" for the whole set. After every call, success or not, the
   products lists and the teams list are refetched, so the box shows what the server holds. The dialog reads the live
   product from the list, never a copy. An archived product shows the checklist disabled (the server answers 409).
3. **Archived means read-only.** Edit opens with every field disabled and no Save button; a Restore button is in
   the dialog and on the row. Delete is offered only on an archived row and always asks first. Archive and restore send
   `archived` and the row's `version`.
4. **Edit sends the changed fields and `version`.** No change: no request. `icon` and `colour` cleared send `null`.
5. **Fixed sentences** (`productText.ts`), never the server's words: a 409 on save is "A product with this slug exists
   already" under the slug field (a 201st product and a `version_conflict` have their own sentences); a 400 "send a
   slug" (a name with no Latin letters, or one that starts with a digit) asks for a slug. The form checks name, slug,
   icon and colour first; the server stays the judge. The slug field is not required: empty means "make it from the name".
6. **Touch targets.** `products.module.css` holds the 40px rules in the phone / coarse-pointer query under `.touch`.
   The page and ALL three dialogs carry the class (`products.layout.test.ts` counts the `<Dialog` tags and checks each one).
7. **Keys.** `keys.products(org)` is the prefix of both list variants (with and without archived), so one
   invalidation refreshes both. The sidebar (LAUN-64) uses the same prefix.
8. **Not proven here.** jsdom has no layout and no media queries: the 40px targets and the phone layout (no
   horizontal scroll) are checked in the CSS text only, not seen in a browser. The colour swatch, the dark mode look and
   the focus order across dialogs were not looked at in a browser. The sidebar refresh after an assignment belongs to
   LAUN-64 (events).

Review round (LAUN-66):

9. **A failed save still refetches.** Every product mutation refreshes the products and teams lists in `onSettled` and
   waits for it, so a `version_conflict` or a 404 leaves the list current, and Archive then an instant Restore sends the
   new version (the row buttons stay disabled until the refetch ends).
10. **The edit dialog keeps typed fields** when the row changes in the background (its key is the product id, not the
    version). A note says so. Reason: dropping a person's typing is worse than saving over a field they can see. The next
    Save sends the new `version` and only the fields the person changed since the dialog opened (not the ones another person changed meanwhile). An empty slug on edit is refused first.
11. **Focus.** The archived edit dialog starts on Restore (a disabled field cannot take focus; focus on one is lost
    and breaks Escape and the trap). The team boxes use `aria-disabled` while a call runs and ignore a second tick, so
    focus stays on the box. The delete confirm starts on Cancel.
12. **409 sentences.** Only "slug already used" gives the slug sentence; the cap and `version_conflict` have their own;
    any other 409 (for example "archived; restore it first") is "The product changed or is archived. Reload and try again."
13. **Close button.** The X of the shared `ui/Dialog` is 32 px wide, 40 px high on a phone or coarse pointer (`.touch
button`). Not changed here; `Dialog.tsx` is shared.

## Changes block, MR rows, hook refresh and backfill (LAUN-95)

The web part of LAUN-83 (server notes: decisions-T17 #50, "LAUN-83"). Code: `ChangeRequests.tsx`,
`changeText.ts`, `activityModel.ts`, `Activity.tsx`, `api/queries.ts` (`useIssueActivity`),
`settings/git/RepoSection.tsx`, `api/gitIntegrations.ts`.

1. **Changes block.** `useIssue` now asks `include=relations,stateHistory,attachments,changeRequests`.
   The block sits between the sub-issues and the Activity. With no `changeRequests` key (the server
   leaves it out), an empty list, or a placeholder issue that is still loading: nothing, not even the
   heading. A row has the title (a link when the URL is http or https, else plain text), the state chip
   (Draft, Open, Approved, Merged, Closed), a "Changes requested" chip, the checks chip and
   `repoPath · by author · provider`. **The checks chip shows only when `checks` is not null**
   (LAUN-85 sets it): Checks running, passed, failed. `title`, `author` and `repoPath` are text nodes:
   no HTML, no Markdown. A host URL that is not http(s) is shown as text with no link (the server
   checks it too).
2. **Activity.** `GET /issues/{issue}/activity` is read to the end (250 a page, 20 pages at most; the rows come oldest first, so a longer history would lose the NEWEST: the page then says "The newest recorded changes are not all shown"; the cap and the note stay, because the server has no newest-first order) by
   `useIssueActivity` and merged by `buildTimeline`. Every kind has a line: field edits (title,
   description, assignee, priority, estimate, due date, parent, project, milestone, cycle) as
   "set / changed / removed the X", label adds and removes (a burst of one verb folds to "added 2
   labels"), `relationRemoved`, `mrLinked` / `mrMerged` / `mrClosed` ("GitLab linked merge request
   !98 Title", "GitLab marked ... as merged"; the name is the provider because the server records no
   actor; the title links only for http(s)), and a kind this web does not know ("changed <kind>"),
   so nothing is dropped. The link attachment the server writes next to each `mrLinked` row (same URL, no creator) is not drawn a second time; an attachment with no matching `mrLinked` row (old data) stays. The MR title is clipped to 100 characters like other host text. A relation ADDED comes from `issue.relations` (`createdAt`, `createdBy`); no
   creator gives a line with no name. A row with no actor takes its source as the name (GitLab,
   GitHub, Bitbucket, System). A snapshot of a strange shape never throws: the line falls back to
   "changed the X". A failed activity read shows an error above the list; the rest still draws.
3. **Live refresh.** `keys.issueActivity` sits under `keys.issueDetails`, so the existing
   `issue.updated` invalidation (changes `["changeRequests"]` too) refreshes the Changes block (it is
   part of the issue read) and the activity. `events.ts` is unchanged; a test pins the key prefix.
4. **Repository buttons** (token mode only; webhook-only rows show Unmap only): "Refresh hooks" and
   "Backfill open MRs" in the `repo-actions` slot. One call at a time for the whole table (the calls
   are rate limited like Check token). While one runs the buttons are `aria-disabled` (not
   `disabled`), so keyboard focus stays; a press then does nothing. The result is a status line with
   the repository path: the new hook state in words, or "Read 12 open requests. 3 tickets got a new
   link. 1 ticket moved forward. More open requests were left..." for `{seen, linked, moved,
truncated}`. Errors are fixed sentences (`gitErrorText` contexts `hook` and `backfill`; the status-only 429 and 502/503 sentences apply to these two contexts only): 409
   (rotation pending, no token, hash-only secret), 429 (wait N), 502/503 and the `provider_*` codes.
   Server text never shows. The status line is always mounted (empty and hidden until it has text). A refresh says "The hook is up to date and working.", "The hook could not be updated..." or "Launchpad does not manage this hook." The button names start with their visible text ("Backfill open MRs for X"). `patchIssueInCaches` and `findIssueInCaches` skip the activity key (it is a list of changes, not issues). A backfill also marks the issue lists and details stale.
5. **Touch and theme.** The Changes title has `min-height: 40px` under the shared phone / coarse
   query; the repository buttons are covered by `.touch button`. Colours are tokens only (warn and
   danger pairs are in the contrast test). No hover underline (`a.crTitle:hover`).

**Not done / limits:** a backfilled MR shows no review state until its next event (server note);
state icons are unchanged; there is no per-repository result memory (the status line shows the last
call only).

**NOT checked:** an `issue.updated` event over a real SSE stream (tests call `invalidateForEvent`); wrapping at 320px wide; the 20-page note against a real long history; no browser run (light and dark, 1280 and 390 wide); no screen reader; no real
GitLab or GitHub payloads (all rows are hand-written fixtures of the documented shapes); keyboard
order inside the table was tested in jsdom only.

## Git connections: follow-ups and the deliveries table (LAUN-93)

Code: `features/settings/git/` (`DeliveriesSection.tsx`, `deliveryText.ts`, `ConnectDialog.tsx`, `RotateSection.tsx`),
`api/gitIntegrations.ts` (`useGitDeliveries`, `useAvailableRepos`). Tests: the "LAUN-93 follow-ups" and "deliveries"
blocks of `gitIntegrations.test.tsx`.

1. **A pasted GitLab secret does not follow a provider change.** Changing the provider clears the pasted secret, and
   the create body carries `secret` only for GitLab in webhook-only mode.
2. **No close while the create is in flight.** Escape, the close button and Cancel do nothing while the call runs, so the
   dialog cannot unmount before it shows the one-time webhook secret. Connect and Back use `aria-disabled` (focus stays)
   and a second press is ignored.
3. **`notMoved` is "any hook that is not `active`"**, the same rule as the server (`tracker/git_rotation.go`). A repo
   with `hookState: none` therefore keeps plain Commit off and offers Commit anyway.
4. **A failed search is not asked again by itself.** `retryOnMount: false` alone did not do it: when the search text
   changes back to a key on a live observer, TanStack refetches any query that has no data. `useAvailableRepos` now turns
   the query off when the cache holds an error for the key. The press on Search still retries (`refetch()` works on a
   disabled query). A test counts the host calls.
5. **Touch class test.** A rendered test checks that the page wrapper and the dialog panels carry `touchClass`. The CSS
   text test stays.
6. **Deliveries table** (after the Rotate section): time, event, repository, result, HTTP status and reason, in the
   server's order (newest first, 100 at most, no paging). Values are text nodes. The result and our error code are shown
   as fixed sentences (`deliveryText.ts`); an unknown code gets a fixed sentence and its text is not printed. A row with
   no integration says "no integration matched" (the server lists those on every integration of the provider). Empty
   state, a fixed error with Try again, and a Reload button. The table scrolls inside its box on a phone.
7. **Last delivery.** A row on the summary card of the detail page, and a part of the meta line of each list row
   ("Last delivery: Processed, <time>", or "No deliveries yet"). It comes from `lastDelivery`, which refreshes when the
   integration is refetched; the table has its own query (15 s stale time).
8. **Not proven here.** No browser run: the table on a phone, dark mode, and the 40px target of Reload are checked in
   jsdom and the CSS text only.
   Also not proven: the focus order and the focus return after a dialog closes, and that Escape, the close button and
   the backdrop are ignored while the create is in flight with no reason shown to the person.

## Product page (LAUN-65)

Page `/:org/product/:slug`. Code: `features/issues/ProductPage.tsx`, `product.module.css`, `product.test.tsx`,
`product.layout.test.ts`; `useProduct` in `api/products.ts`. Rules: `decisions-T18-products.md` #24, #27, #28 and the P4 list.

1. **One call for the product.** `GET /products/{slug}` (the slug is lower-cased first). It answers an archived product
   too and lists its NON-archived teams. A 404 is the "Page not found" page and is not retried; no issue request is sent.
   The key sits under `keys.products(org)`, so `product.*` events and every product mutation refresh it.
2. **The product is not a filter here.** The page sends it as `extra.product` to `IssueBrowser`. So it is not a chip, not in
   the filter count and not in the address; a stray `?product=` or `?team=` in the address is dropped. The server's `product`
   filter takes archived teams too, so the list is the union as T18 #27 says. The Product chip is hidden (`lockProduct`).
3. **Board columns and the Status chip are scoped** to the states of the product's teams (`scopeTeams`, team keys). A column
   whose state name exists for only some teams holds only those teams' cards. A card of an archived team still shows (its
   state comes from the card). Same-name states of different teams are one column, as on the all-issues board (measured:
   `groupByState` and `boardColumns` group by name).
4. **A drop onto a state the card's team lacks is refused** in `useIssueActions.place`: a toast ("ORB-1 is in team BET,
   which has no state "Review".") and no PATCH. It applies on every board, not only here, and only once that team's
   states have loaded (before that the server stays the judge). A group drop (`placeMany`) already skipped such cards.
5. **Labels.** The label chip is the org label list. Labels are per org today, so it is the union of the teams' labels;
   when LAUN-54 team labels land, this chip needs the union of the product's teams (T18 #28).
6. **Create.** `c`, the sidebar and top bar `+` and the page button call `openCreate({ firstTeams })`. Shell has a second
   `useMatch('/:org/product/:slug/*')` and reads the product from the shared `useProduct` cache. The dialog starts on the
   first product team and lists the product's teams before the others. Before the product has loaded, `c` opens the
   dialog with no default (as on the issue list).
7. **Archived.** A `role="status"` banner, and no page create button and no "Create the first issue" button. Judgment: the
   product is read-only; its teams' issues are not, so the board still drags and the global `c` and `+` still open the
   dialog (the issue belongs to a team, not to the product). A reviewer who wants `c` off here changes `createHere` in Shell.
8. **No teams.** The empty state "This product has no teams yet" with a link to Settings, Products for an admin, or "Ask an
   organisation admin" for a member. No issue request. Not shown with the archived link.
9. **Sidebar.** Each product group has a link "Open product NAME" (a `›`) BESIDE the toggle button, never inside it. The
   button keeps its name and `aria-expanded`. Tab goes button, link, then rows (the closed-group test was changed for this).
   "Other teams" has no link. The Command palette "Go to product" now opens the page.
10. **Touch targets.** 40px for the team chips and the page buttons, 44px for the sidebar link, under the usual query
    (pinned in `product.layout.test.ts` by reading the CSS). The head wraps; the board scrolls inside its own area.
11. **Leaks.** Only data the routes return is shown (name, slug, description, teams). Everyone sees every team today
    (T18 #26); LAUN-91 (team access) must filter `ProductDetail.teams` on the server.
12. **Not seen in a browser.** jsdom has no layout, no media queries, no focus ring. Not proven: the 40/44px sizes, the phone
    layout without horizontal scroll, dark mode of the banner and chips, the real pointer drag (the tests use the fake board
    layout), the look of the `›` link, a screen reader.

## LAUN-98: the server onboarding state

Web part of LAUN-56 (`decisions-T17-onboarding.md`). Replaces items 3 to 6 of "Guided first-run checklist (LAUN-25)".

1. **One read.** `useOnboarding` reads `GET /orgs/{org}/onboarding` only (`useOnboardingState`). The checklist and the banner
   make no teams, members, tokens or platform request any more. `computeSteps` is pure over that answer and `isPlatformAdmin`.
2. **Done and skip are the server's.** `done` and the shared `skipped` come from the server. A skip never hides a done step.
   Skip and Undo skip are one button (`PUT .../onboarding/skip`), so focus stays on it. It is `aria-disabled` in flight, and a
   second press is ignored. The detail line says who skipped ("Skipped by Sam.").
3. **No `needsPlatformAdmin` status.** Mail and Sign-up mode are `todo` (sign-up mode `review` for a platform admin) with the line
   "Ask a platform admin." for an org admin, who also gets no link to Platform and can skip the step.
4. **Mark reviewed** is `PUT /platform/settings` with `signupModeConfirmed: true` (platform admin only). The PUT takes the whole
   settings, so `useConfirmSignupMode` reads them and sends them back unchanged with no secret field (a stored secret stays).
   Focus moves to the step title when the button goes.
5. **Capabilities.** The constant is gone. A step shows "Coming soon" while the server capability (`linearImport`, `gitHosts`) is
   missing OR `stepPages` has no page for it. Both pages exist (`import`, `integrations`).
6. **Only `dismissed` is in localStorage** (`launchpad-onboarding:<org>:<user>`), in try/catch. If storage is blocked, an in-memory
   value keeps the banner closed until the page closes. Old `skipped` and `reviewed` values are ignored and dropped on the next save.
7. **Live events.** `onboarding.step_changed` refreshes the onboarding query. `org.updated` refreshes it, the org detail and the session
   (the sidebar reads the org name from the session). Both are in `EVENT_TYPES` (`events.test.ts`).
8. **Organisation page.** Name, time zone (a select of `Intl.supportedValuesOf('timeZone')` with a short fallback list; UTC first; the
   current value is always in the list) and default team (a select of the org's teams, "No default team" sends `null`) with
   `PATCH /orgs/{org}`. The slug is shown, fixed. Only changed fields are sent; no change shows "Nothing changed." with no request.
   400, 403 and 409 and the other codes are fixed sentences. The saved org updates the session name at once.
9. **Touch and style.** 40px on a phone and a coarse pointer for the checklist, the banner and the Organisation form (CSS text tests
   plus the class check). No hover underline. Colours are the existing tokens.
10. **Mark reviewed can overwrite.** It reads the platform settings, then sends the whole settings back in a PUT. The API has no
    version or precondition, so a change by another platform admin between the read and the PUT is overwritten. The window is small.
11. **Time zone groups.** The select is grouped by region with `<optgroup>` (the part before the first slash). `UTC` has no slash, so it
    sits in a group called "Other".
12. **Busy style.** A button that waits (`aria-disabled`) is drawn at 60% opacity with a progress cursor
    (`.touch button[aria-disabled='true']`); a CSS-text test guards it. The shared `.btn:hover` tint still applies to it.
13. **Not proven here.** No browser run: layout at phone width, dark mode, the real focus order, and the select with the browser's full
    zone list are checked in jsdom and CSS text only. The capability-present import path is tested with a fake answer only.

## Rename teams, products and the organisation (LAUN-99)

The ticket text is "ability to rename teams, products and org". The server already allowed all three (`PATCH /orgs/{org}`,
`PATCH /orgs/{org}/teams/{team}`, `PATCH /orgs/{org}/products/{product}`, admin scope, one event each). The organisation page
(LAUN-98) and the product dialog (LAUN-66) already had a name field. The gap was the team name in the web app.

1. **Where.** Organisation: Settings, Organisation, Name. Product: Settings, Products, Edit (the product page links there).
   Team: Settings, Teams, a Rename button on each row (admin only), which opens a dialog with one field.
2. **Fixed.** The org slug, the team key and (unless sent) the product slug do not change on a rename. The org slug is an unknown
   field (400). The dialog says the key is fixed.
3. **Rules.** Name: trimmed, 1 to 200 characters. A name equal to the stored one writes nothing and emits no event. Names are
   NOT unique (only the key and the slugs are); two teams may share a name. Duplicate-name refusal would break existing data and
   the Linear import, so it is not added.
4. **Refresh.** The team rename writes the answer into the teams query, then invalidates the teams, the products and the
   issue-detail queries (also on the `team.updated` event), so the sidebar, the team page heading, the tab title, the team names
   in products (product page, Settings, Products, Assign teams) and the team name on an issue follow with no reload. The org
   rename already updates the session (the org menu).
5. **Errors.** A fixed sentence per status (400 name rule, 403 admin, 404, 429, network); server text never reaches the screen.
6. **Touch.** The page body and the dialog use `touchClass`: 40px buttons and inputs on a phone or a coarse pointer.
7. **Not proven here.** No browser run (phone layout, dark mode, real focus order); jsdom tests only.

## The products checklist step (LAUN-67)

Web part of decisions-T18 #32 and #37a. Code: `features/settings/onboarding.ts`, `SetupChecklistPage.tsx`, tests in
`onboarding.test.tsx`.

1. **Server driven.** The step is in `GET /orgs/{org}/onboarding` only for two or more teams and no product. `computeSteps` shows
   it only when the server lists it (never while loading or on error), so the web keeps no copy of the rule.
2. **Optional.** `Step.optional` is true for ids in `optionalSteps`. `isOpen` is false for an optional step, so the banner count
   and the "still to do" count ignore it, and the page counts "N of M steps done" over the required steps only. It still has
   Open Products and Skip buttons, and a skip shows as Skipped.
3. **Link.** "Open Products" goes to Settings, Products (`stepPages.products`). The sidebar choice itself (product groups,
   hidden with no products) is in the LAUN-63 to LAUN-66 sections above.
4. **Not proven here.** jsdom tests only; no browser run.

## Label picker: server counts and team labels (LAUN-101)

Follow-up to LAUN-53 and LAUN-54 (decisions-T21 #12). This replaces the "no Team labels group" and
"250-issue sample" rules of the LAUN-53 section above.

1. **No sample.** The `['org', o, 'label-usage', team]` query (the team's newest 250 issues) is gone. "Frequently used" is
   the 3 labels with the highest `issueCount` (at least 2) from `GET /labels`. A closed issue counts, an archived one does not
   (T21 #5), so the count is a true total, not a sample.
2. **Scope.** `useLabels(team?)` calls `GET /labels?team=KEY` for the issue's team (issue page, create dialog, the filter bar
   on a team page, the `l` quick picker when all targets share a team). Without a team (all-issues page, mixed selection) it
   calls `GET /labels` and a team label shows its team key as a hint. The query key is `['org', o, 'labels', TEAM]`, so one
   invalidation of `keys.labels(org)` reaches every scope. The issue page and the dialog wait for the team before asking.
3. **Groups, in this order, one label in one group:** Suggestions, Frequently used, **Team labels** (`teamId != null`),
   **Org labels** (the rest; was "All labels"). A heading shows only when two groups have a row.
4. **Suggestions** still come from the issues that share the project (`GET /issues?project=ID&limit=100`) and the sub-issues
   of the parent (`?parent=IDENT&limit=100`), read only while the picker is open and only when there is a project or a parent.
   Change from LAUN-53: the parent issue's OWN labels are not read any more (they came with the old team sample). A project
   with more than 100 issues gives a sample for Suggestions only; counts are never sampled.
5. **Create.** A label made from the issue page or the create dialog sends `team` (the issue's or the form's team), so it is
   a team label (T21 #7). There is no way yet to make an org-wide label from the picker (Settings has none either).
6. **Filter.** The label chip lists the org and team labels of the page's team (the all-issues page: every label). It matches
   by NAME (T21 #4), so a name that an org label and a team label share shows once. `?label=` is sent with `team=` (already
   so: `toApiQuery`); a server test (`TestLabels_FilterCombinesWithTeam`) pins the combination.
7. **Create dialog (review fixes).** A team change clears the ticked labels (like state, cycle and project): the new team's list
   may not hold a team label. The dialog picks by NAME, so an org label and a team label of one name show once (the team's, which
   the server also resolves first, T21 #4). A refused label create shows the error toast; create does nothing until a team is
   set, so a label never becomes org-wide by accident.
8. **Not proven here.** jsdom tests only: no phone layout, dark mode or real focus run. The group heading and rows reuse the
   LAUN-53 picker styles, so 40px targets and colours are unchanged.

## Attachment previews, the drop pane, one drop one file (LAUN-105)

1. **Previews.** `features/issues/AttachmentPreview.tsx`. A `file` attachment with content type png, jpeg
   or gif is an inline image; anything else, mp4 too, keeps the link or chip.
   **No video player (deferred):** the production CSP (`web/deploy/security.inc.template`) has no
   `media-src`, so media falls back to `default-src 'none'` and a `<video>` would be dead. It needs a
   `media-src` for the files origin, the deploy.sh / compose-check assertions and a decision. The source is always the API `downloadUrl` (the 302 route), never a silo URL. A link opens the
   download URL full size in a new tab (no lightbox: a link is keyboard-reachable and needs no focus trap).
   Alt text is the filename.
2. **Stable layout.** The API sends no image dimensions (only `contentType` and `sizeBytes`), so the frame
   has a FIXED height (160 px, image `object-fit: contain`, `loading="lazy"`). A late image moves nothing,
   so scroll restore is safe. Phone width: frames wrap and are `max-width: 100%`. Colours are tokens, so
   dark mode follows.
3. **Where.** _(Superseded by LAUN-115: no previews in the Activity row.)_ The Activity row of a file burst shows up to 6 previews under its line; a new "Attachments"
   area (between the Changes block and Activity) shows every file: image previews, then a chip per other file.
   No server change.
4. **Drop overlay.** It was inside the scrolling grid (`.detail`, `overflow: auto`), so `inset: 0` meant
   the first screen of the CONTENT and it scrolled away. The zone is now a non-scrolling pane
   (`ui.dropPane`) that holds the scroller; the overlay is its sibling, so it covers the whole pane.
   The message is centred and names the issue: "Drop files to attach to LAUN-85". The create dialog uses
   the same overlay. Nested enter/leave are still counted (LAUN-58 #2), the overlay has no pointer events.
5. **Reply and comment boxes** are not drop targets of their own: there is ONE zone (the pane). A drop on
   a textarea still also writes markdown at the caret (LAUN-58 #9).
6. **Double attach.** Cause NOT proven in a browser. The code has one handler on one zone, so a double
   fire from two handlers is not possible. The likely cause is a browser listing one dragged screenshot
   twice in `dataTransfer.files` (seen with a macOS screenshot thumbnail), which the code uploaded twice.
   Fix, in `useFileDrop`: the files of one drop are de-duplicated by name, size, type and modified time,
   and one native drop event is handled once (a `WeakSet`). A deliberate second drop of the same file is
   allowed (it is a new event): two rows. A retry is not the cause: `useAttachFiles` never retries.
   The server does not refuse a second attach of one `uploadKey`; a unique key per issue would be a
   server follow-up.
7. **Tests.** _(Superseded by LAUN-115: no previews in the Activity row.)_ `fileDrop.test.tsx` (one drop, one row; same file twice in one drop; one native event twice;
   two deliberate drops; overlay beside the scroller; no flicker), `attachmentPreview.test.tsx`, and
   `detail.test.tsx` (image in the Activity row and the area).
8. **Not seen in a browser:** the overlay covering the real pane at 1280 and 375 px, dark mode, the real
   macOS screenshot drop, an image loading through the 302 to silo, keyboard focus order.

## Git settings: 409 reasons and Remove connection (LAUN-104)

1. `gitErrorText` maps the 409 codes `integration_exists`, `secret_in_use`, `secret_hash_only`, `repos_mapped` and
   `integration_changed` to fixed sentences. No git settings sentence says "reload" for a conflict (the old generic 409,
   the rotate 409, the repo 409 and not-found were reworded). Server text still never reaches the screen.
2. Connection page: a "Remove connection" row opens a confirm that names the connection, says mapped repositories must be
   unmapped first and what goes with it. Success goes back to the list; `repos_mapped` stays in the dialog as a fixed
   sentence. 40px targets via `touchClass`.
3. Replace/Add token on `secret_hash_only` shows "Rotate the webhook secret first": it closes the dialog and raises
   `startSignal` on the rotate section, which starts the rotation (the secret dialog opens in webhook-only mode).
4. Connect dialog on `integration_exists`: "Open the existing connection" (provider + host match in the list, else the list).
5. Not proven: jsdom only, no browser run.

## Git token help and 403 text (LAUN-103)

Text only; no server, route or API type changed. Rules and the unverified list: `decisions-T17-git-connections-import.md`
"LAUN-103".

1. `ProviderInfo` (`gitText.ts`) has a new `needs` block (hook, list, check) per provider. GitLab `scopes` now has two
   lines: the classic or project token (scope api, Maintainer) and the new fine-grained form. The words "usually" and
   "can be wrong" are on purpose: the names are from documentation and unverified.
2. `gitErrorText(err, ctx, provider?)` takes the provider. `provider_forbidden` goes to `forbiddenText(ctx, provider)`,
   which names what the token usually needs for that call. New context `list` is the repository search. No host text
   is shown. With no provider the old general sentence shows.
3. The Replace token and Add a token dialog show the same permission lines, the note and a link built from the
   connection's own base URL (https only). The connect dialog heading line now says "permissions".
4. The webhook-only "Map a repository" form has a one-line hint to add a token for the repository list.
5. Not proven here: jsdom tests only; no real GitLab, GitHub or Bitbucket form was looked at.

## Board sort and "I cannot change the ticket order" (LAUN-110)

Code: `filters.ts` (`defaultSort`, `parseFilters`, `toApiQuery`), `FilterBar.tsx`,
`IssueBrowser.tsx` (the hints), `BoardView.tsx` (`dragEnabled`). Test: `boardSort.test.tsx`.

1. **What was checked.** The real stack (compose project `dev-ops-laun110`, vite dev server) and a real
   browser (Brave, headless, driven with puppeteer-core: real mouse events, and touch through the
   DevTools protocol). A drag inside one column, a drag to another column and a drag into an empty column
   each sent ONE `PATCH` (`placeAfter` / `placeBefore`, plus `state` across columns), the answer was 200,
   and the new place was still there after a reload. Touch (hold 250 ms, then move) moved a card too. So
   the drag in the code of LAUN-29 and LAUN-51 works in Chromium. Not checked: Safari and Firefox (no way
   to drive them here), a real phone, and the production data (no token used).
2. **What was wrong.** Nothing in the drag. The page gave no sign of it: on the board the Sort select was
   greyed out and said "Manual" with only a tooltip, the cards had no hint, and the list view with sort
   Manual shows the stored order but has no way to change it. A person who looks at "Sort: Manual" and
   finds it locked has no way to learn that the cards drag. The old rule (LAUN-29 item 7: the board always
   shows Manual) also made it impossible to pick any other sort on the board.
3. **The board follows the sort.** Its Sort select works. The board starts in Manual: with no `sort` in the
   URL the sort is Manual on the board (`defaultSort('board')`) and Updated on the list; the URL only
   carries a sort that differs from the default of its view. The Board button changes Updated to Manual.
   The request asks the server for that order (`orderBy` `manual`, `createdAt`, `updatedAt`); Priority is
   sorted in the page, as in the list. This replaces LAUN-29 item 7 (the board always fetches manual).
4. **Drag is on only with Manual.** With another sort `BoardView` gets `dragEnabled={false}`: a press does
   nothing (no clone, no gap, no `PATCH`), the cards have a normal cursor, and a line above the board says
   "Sort is Priority: switch to Manual to reorder." with a "Switch to Manual" button. With Manual the line
   says "Drag a card to change its place. The order is saved." The status picker (`S`) still changes a
   state in any sort; the server puts the card on top of its new column.
5. **The list.** (Superseded by LAUN-111, last section: the list drags by a handle.) There was no drag in the list (follow-up LAUN-111: it needs vertical target maths,
   a handle and touch rules; `boardDrag.ts` finds a column by x). With sort Manual the list says "Manual
   order. To change it, drag cards on the board." and has an "Open the board" button.
6. **Filtered boards.** The drag names the neighbours that are on screen (`placeAfter` / `placeBefore`);
   the server checks that they are in the same team and column, so a hidden card between two shown ones
   is no problem. Test: a card hidden by the filter, the body names a shown card.
7. **Tests.** `boardSort.test.tsx`: the default, a drop and a reload, Priority turns drag off and says why,
   the button, the select, the Board button, the list hint, a filtered board. `filters.test.ts`: the
   default per view, the URL round trip, the `orderBy` per sort. jsdom has no layout, so the drop tests use
   the fake layout of `test/boardLayout.ts`; the real-browser run is item 1.

## LAUN-112: flaky timing tests

Six tests failed at random on the shared CI runner and passed on a retry. Four
`npm test` at once on one Mac made the same failures (2 of 4 runs failed before the fix).

1. **Cause 1: the 1000 ms default.** `findBy*` and `waitFor` wait 1000 ms. Five of the six tests render the
   whole app (`renderApp`): a session, the org, the teams and the page data come one after the other. On a
   loaded runner that took 1.2 to 1.7 s. Nothing was wrong in the app. `src/test/setup.ts` now sets
   `configure({ asyncUtilTimeout: 5000 })`; a wait that succeeds returns at once, so it costs nothing, and
   only a real failure waits the full time. `vite.config.ts` sets `testTimeout` and `hookTimeout` to 20 s
   (the vitest default of 5 s would end a test that legitimately waits 5 s).
2. **Cause 2: a race, not a timeout** (`scrollRestore.test.tsx`, "issue list: a failed page stops the loading").
   After Back the list shows "Row 1" only until page 2 fails; then it becomes the error state and the rows
   leave the DOM. `findByText('Row 1')` is a poll and can miss that frame. A longer timeout does not help.
   The test now uses `watchText`, a `MutationObserver` started before the click, and asserts that the row
   was shown at some time. The assertion is as strong as before; it no longer depends on a poll.
3. **Checked and not the cause:** the test `QueryClient` already has `retry: false` for queries and
   mutations (`test/render.tsx`); there are no fake timers in these tests (`afterEach` also restores real
   timers); the shared module state (back-nav memory, bearer, CSRF, EventSource fakes) is reset in `afterEach`.
4. **Rule.** A new test that renders the whole app must not set its own short timeout. A flaky test gets a
   ticket and a cause; it is never retried to green.
5. **Proof.** 20 full `npm test` runs, four at a time (5 rounds): 20 of 20 passed, 1342 tests each. One round
   of 8 at once: 8 of 8 passed. The AC "20 consecutive CI runs without a retry" is still open: only the CI runner can confirm it after
   the merge. The local runs were not on that runner.

## Issue page: padding and a toned background (LAUN-120)

The project owner asked for Linear's look: text off the window edge, a soft grey page, the comment box as a white card.

1. **Tokens** (`tokens.css`, light, dark by OS and the `data-theme` override): `--bg-page` (light `#f1f1f5`, dark `#0a0a0d`)
   is the page behind the content. `--bg-card` (light `#ffffff`, dark `#17171c`) is a card on it. `contrast.test.ts`
   checks text, muted and accent on both, in all three blocks.
2. **Column.** `.bar`, `.head` and `.main` share `max-width: 800px` and `justify-self: center` inside the left grid
   column, so the Back link, the toolbar, the title, the description, sub-issues and Activity line up. The grid and
   the DOM order (decision 12 of LAUN-21) are unchanged. The Properties column keeps its 280 px at the right.
3. **Padding.** `.detail` is `20px 48px 64px` on a wide screen and `12px 16px 48px` at 900 px and below. The `.detail`
   scroller itself has the tone, so the tone fills the pane and scrolls with no seam.
4. **Cards.** `.inputCard` (white card, faint border and shadow, 12 px padding) is on the comment box and the
   description editor. On a phone the Properties box is a `--bg-card` card. Hover of a property row is
   `--surface-hover` (`--surface` is too close to the page tone).
5. **Other pages.** Only the issue page gets the tone now. The web app has no project or document detail page yet;
   when one is built it uses `--bg-page` and `--bg-card` the same way. The cycle page, product page, lists,
   boards and settings keep `--bg`: they are work surfaces with dense rows, and the tone would add a second grey
   to their surface rows.
6. **Drop pane and scroll restore.** Not touched: the tone is on the scroller INSIDE `ui.dropPane`, the overlay is
   still the pane's sibling, and no scroll container or element moved (LAUN-105, LAUN-90).
7. **Guard.** `detailTone.layout.test.ts` pins the tokens, the page background, the 720 to 860 px column and the
   16 px phone padding (jsdom has no layout).
8. **Seen in a real browser** (Brave headless, real stack, Vite dev server, issue with long text, a sub-issue and two
   comments): light and dark at 1400 x 850 and 375 x 812. Column centred with side space, tone visible, comment box a
   white card (light) and a raised dark card (dark), no horizontal scroll (`scrollWidth` equals the width at both).
   **Not seen:** the LAUN-105 overlay during a real drop, scroll restore after Back, `check:overflow` (it expects the
   seed of another stack), 320 px, the description editor card open, and Safari / Firefox.
9. **Review round 1 (surfaces on the tone).** `.event .gutter` masks the timeline line behind an icon, so it is
   `--bg-page` (it was `--bg`, a white patch on the toned page). Every other surface in `detail.module.css` that was
   `--bg` or `--surface` is now `--bg-card`: comment cards (`.card`), the "Leave a reply" row (`.replyRow`, on
   purpose: it is an input-like row, so it is a card like the comment box), change-request rows, image preview frames
   and file chips. Reason: `--surface` (#f6f6f8) on `--bg-page` (#f1f1f5) was too weak. Hover states keep
   `--surface-hover`. The card shadow is the token `--shadow-card` (light and dark). The guard test fails when a
   non-hover rule paints `--bg` or `--surface` in the detail sheet, or when `.event .gutter` is not `--bg-page`.
10. **Seen again in Brave** (light and dark, 1400 and 375 px) with state-change and priority rows in Activity: gutter
    icons show no patch, comment cards read white on the grey page, the comment box is a card. Screenshots stay in the
    scratch folder, not in the repo.

## Issue page: only the centre column is the card (LAUN-202)

LAUN-181 put the whole issue page on the white shell card. The ticket asks for white only in the centre.

1. **Tone.** `.detail` (the scroller) paints `--bg-page`: side margins and the Properties column sit on the page tone.
2. **Card.** `.detail::before` is a grid item (`grid-area: 1 / 1 / 4 / 2`, behind bar, title and body) painted `--bg-card`
   with the border and radius of the shell cards. It is 24 px wider than the 800 px column on each side. A pseudo-element
   keeps the DOM order and the grid areas (decision 12 of LAUN-21) as they were. Dark: card `#17171c` on `#0a0a0d`.
3. **Inside the card.** `.event .gutter` (the timeline mask) is `--bg-card`. Comment cards and the comment box stay
   `--bg-card` with their border.
4. **Phone (900 px and below).** One column, so no side tone: the pane is `--bg-card` and the pseudo-card is hidden.
5. **Other pages.** Project, cycle and product pages do not use `detail.module.css`; they are unchanged.
6. **Card-aware surfaces (review).** In dark, `--surface` equals `--bg-card` (`#17171c`), so the shared fills (`.md pre`,
   `.md code`, row and column hovers) vanished inside the card. `.detail` now sets `--surface` and `--surface-hover` to
   the new tokens `--card-surface` (light `#f6f6f8`, dark `#202027`) and `--card-surface-hover` (`#ececf0`, `#2b2b34`);
   only this page changes. The Properties column is on the page tone, so its hover is `--page-hover` (light `#e3e3ea`,
   dark `#1b1b21`; light was 1.05:1 with `--surface-hover`). The phone rule keeps `--surface-hover` (card). `contrast.test.ts`
   checks the new pairs.
7. **Known choice.** On a dark phone the pane is `--bg-card` (`#17171c`) under the shell banner, a small tone step
   from the banner; kept as designed.
8. **Guard.** `detailTone.layout.test.ts`. **Seen** in Brave headless (light and dark, 1440, 1000 and 390 px): no
   horizontal overflow. Not seen: Safari, Firefox, 320 px.

## List view: drag rows to reorder with sort Manual (LAUN-111)

Code: `listDrag.ts` (the machine and `useListDrag`), `listDropTarget.ts` (the pure maths), `IssueRow.tsx` (the
handle), `IssueBrowser.tsx` (wiring, hint, keyboard, live region), `issues.module.css` (`.handle`, `.listGap`).
Tests: `listDropTarget.test.ts` (maths), `listDrag.test.tsx` (real pointer sequences, bodies asserted),
`test/listLayout.ts` (the fake layout jsdom needs), `boardSort.test.tsx` (the hints).

1. **Same API as the board.** A drop sends ONE `PATCH` through `actions.place(issue, stateName, placement)`: the
   state (only when the group is another one) plus `placeAfter` or `placeBefore`, as the board. The server did not
   change and nothing in the server was missing. The optimistic update is `moveIssueInCaches`, which already
   follows the manual list query. `placementAt` (shared with the board) picks the neighbour of the same team.
2. **A handle, only with Manual.** Each row has a grip button at the left (`data-drag-handle`, in the row's left
   padding, so the grid does not change). It shows on hover or focus on a desktop and always on a phone
   (`hover: none` or 760 px wide). The press starts on the handle only: the rest of the row is a stretched link, so
   a click still opens the issue (verified: a click on a row title opens it). A press on the row never drags.
   With another sort there is no handle and the line above the list says "Sort is Updated: switch to Manual to
   reorder." with a "Switch to Manual" button (the board's wording). With Manual the line says
   "Drag the handle at the left of a row to change its place. The order is saved." The LAUN-110 line "drag cards
   on the board" and its "Open the board" button are gone. With Manual the row's `padding-left` is 30px (36px on a
   phone), up from 16px, so the row content shifts right by about 14 to 20px in Manual.
3. **Own vertical maths** (`listTarget`). The groups are stacked, so the target comes from the pointer's y alone:
   the group is the first whose bottom is below `y` (a head, the space above the first row and the 4 px between two
   groups go with the lower group; below the last group is its end), then `dropIndex` of the board counts the
   rows whose centre is above `y`. Rows are measured against the top of the list wrapper (`[data-list]`), so
   scrolling needs no correction. The dragged row stays in place, so the thresholds never move and the line
   cannot flicker. The pointer must be inside the scroller (any x, y inside it) or a drop is a cancel.
4. **The unloaded edge.** The list loads by infinite scroll, so a group may be only partly loaded (and a group
   whose rows are all on a later page is not shown, so it cannot be a target). The drop names LOADED rows only:
   `placeAfter` the loaded row of the same team above the line, else `placeBefore` the one below. At the end of a
   partly loaded group the row lands after the last loaded row, in front of rows the person has not seen yet. This
   is what the screen showed. A group with no loaded row of the dragged row's team sends no neighbour; the server
   then puts the row on top of its new group (`moveIssueInCaches` does the same). Test: "below the last LOADED row".
   Not done: loading more pages for the person during a drag.
5. **Touch.** The handle has `touch-action: none` and is 36 px wide on a phone. A finger on the handle moves the
   row after 4 px (no hold: the handle is the only thing there is to scroll-conflict with, and it does not
   scroll); a finger anywhere else scrolls the list as before (verified with a real touch swipe: the list
   scrolled, no `PATCH`). The long-press menu is blocked while a press is on. A touch has an implicit capture on
   the element it landed on (the svg in the handle); `lostpointercapture` from it must not cancel the drag. The
   machine does not call `setPointerCapture` for touch, and a `lostpointercapture` cancels only when its target is
   the handle (bug found in the real browser run: the first version cancelled every touch drag; test "touch
   capture").
6. **What you see.** A copy of the row follows the pointer (fixed layer), the real row stays, dimmed
   (`data-drag-source`), one accent line marks where it will land and the target group's head gets an accent
   underline. Rows do NOT slide apart like the board's cards: the board makes room because its cards are tall, a
   one-line row needs only a line, and sliding rows across stacked groups (the source group shrinks, the target
   grows) is the costly part the ticket warned about. The copy lands on the line (140 ms; none with reduced
   motion) before the update is sent, and stays until the optimistic update has rendered (`rowsChanged`, else
   500 ms). Escape, a lost pointer, a blur and a drop outside cancel and announce "Move of ORB-n canceled".
   Edge auto-scroll uses `edgeScroll` on the scroller.
7. **Keyboard.** Up and Down on the focused handle move the row one place inside its group (one `PATCH` each,
   `stepPlacement`; a row of another team is stepped over, as in `placementAt`). At the edge of the loaded rows
   it says so and sends nothing. Focus stays on the handle after the row moves. A state change by keyboard is the
   status picker (`S`), as before. The change is announced in a polite live region (`list-live`).
8. **Not built.** Dragging a selected group of rows (the board has it, LAUN-61): the list drags the held row only.
   Dragging in the cycle or product list: they use `IssueBrowser` too, so they get the handle with Manual with no
   extra code (the same rules; not separately checked).
9. **Real browser run** (Brave headless, puppeteer-core, the dev stack `launpad111`, vite dev server; real mouse,
   real touch through `page.touchscreen`, 8 issues in three states): a drag inside Todo (last row to the top) sent
   `{"placeBefore": <id>, "version": 1}`; a drag from Backlog into Todo sent `{"state":"Todo","placeAfter": <id>,
"version": 1}` and the emptied Backlog group left the list; each order was the same after a reload; Escape sent
   nothing; Up on a handle sent `placeAfter` and kept the focus; a touch drag on the handle sent `placeBefore`;
   a swipe on a row scrolled. `check:overflow` as shipped needs the demo data (it waits for ORB-19), so the
   list was audited with Manual and the default sort at 375 and 320 px: no overflow. Not checked: Safari,
   Firefox, a real phone (CDP touch is not a finger: a real iOS Safari may treat the `touch-action` and the
   long press differently), and the production data.

10. **Known limits** (a follow-up ticket is filed): the live region says "moved" before the result is known (the
    update can be refused or rolled back); two fast ArrowDown key repeats can compute the second move from the old
    order; the cancel text for a drop on the same place says "canceled".

## Inline mp4 video and CSP media-src (LAUN-107)

1. **CSP.** `web/deploy/security.inc.template` now has `media-src 'self' ${API_ORIGIN} ${FILES_ORIGIN}`: the same origins
   as `img-src`, without `data:`. `default-src` stays `'none'`, so without it a `<video>` is dead (LAUN-105 #1). The
   `deploy.sh --smoke` check asserts `media-src 'self'` and the files host. `csp.test.ts` (vitest) pins the template.
   compose-check, deploy-test and deploy-lint pass; they do not read the template CSP, so the smoke and the vitest test are
   the assertions. The dev stack has no nginx or CSP (the Vite dev server), so nothing to change there.
2. **Player.** _(Superseded by LAUN-115: no previews in the Activity row.)_ `previewKind` returns `'video'` for `video/mp4` only. `AttachmentPreview` renders
   `<video controls preload="none" playsInline aria-label="Video: name">` from the API `downloadUrl`, never autoplay,
   160 px high frame (same fixed height as images; width follows the video, `max-width: 100%`), and a caption link with the
   filename that is the download link (`aria-label="Download name"`). The native controls are keyboard reachable.
   It shows in the Attachments area and in the Activity row of a file burst (same up-to-6 rule as images).
3. **No poster.** `preload="none"` shows a black frame until Play. A poster needs a server-made thumbnail; not done.
4. _(Superseded by LAUN-115: no previews in the Activity row.)_ **Browser check (Brave headless, puppeteer-core, 1280 and 375 px).** Stack: compose project `launpad107`, the built
   `dist` behind a small Node server that sends the template CSP (envsubst by hand, `FILES_ORIGIN` = the silo origin) and
   proxies `/api`. A 3 s H.264 mp4 was uploaded; the player appeared in both places, played (`readyState 4`, no
   `error`), the 302 to silo answered `206` with `Range: bytes=0-`, the console had no CSP message, and there was no
   horizontal scroll. A first run with the download URL on another origin than the page was blocked by `media-src`
   (a test setup fault, not a product one): it shows the directive is enforced.
5. **Not seen:** Safari, Firefox, the production nginx and TLS, a real `FILES_HOST` redirect across hosts, other
   containers or codecs (an mp4 the browser cannot decode shows the native error in the player; the caption link still
   downloads it), range requests to a large file, a poster, screen reader output.
6. **Gaps to know.** _(Superseded by LAUN-115: no previews in the Activity row.)_ No test renders a video in the Activity row; it shares `previewKind` and `AttachmentPreview` with
   the Attachments area, which the unit test covers. The ticket AC "Chrome, Safari, Firefox on the production-like stack"
   is only partly met: Brave (Chromium) only, behind a test server that sends the template CSP, not the real nginx.

## Show an image once (LAUN-115)

Ticket: "Don't show preview twice, keep the attachments row under the desc." Read literally.

1. **The double.** In a browser (dev stack, Brave, 1280 px) one attached image showed twice: in the Attachments area and
   again in its Activity row (LAUN-105 #3). An image that the description also shows inline (markdown) showed a third time.
2. **Rule.** Supersedes LAUN-105 #3 and LAUN-107 #2 (and the dependent LAUN-105 #7, LAUN-107 #4 and #6). One place shows a
   file preview.
   - The Activity row of a file burst has its line only, no previews, for images and for video (they always repeated the
     area). The `evPreviews` style is gone. The row text still names the files.
   - The Attachments area does NOT preview a file when the description really shows it. That is true only when ALL hold:
     the file is an image (`previewKind` is `image`; a video is exempt and keeps its player, because markdown cannot show
     it); the description renders an `<img>` for it, as a real markdown image node (not in a code block, not inline
     code, not escaped `\![]()`, not a plain `[x](url)` link); the URL is a relative path or our own origin (a blocked
     URL renders as "[image: ...]" and does not count); and the URL path ends in `/attachments/<id>/download` (a title,
     angle brackets, a query or a hash do not matter; the id match is case-insensitive; a1 does not match a10).
     Only the description counts: a comment never hides a preview.
   - Such a file is a chip in the same row, so the row stays under the description and still lists it.
   - "Really shows" comes from the same renderer: `renderedImageSources(text)` in `lib/markdown.tsx` renders the
     ReactMarkdown config the page uses (schema, `urlTransform`, `components`) to static markup and reads the `<img>`
     tags, so the two cannot disagree. `shownInText` in `AttachmentPreview.tsx` applies the id test.
   - An image that is only an attachment (no reference) still previews once, in the area.
3. **Ambiguity chosen.** "Preview" was read as the image preview. The chip row stays; the Activity line stays.
4. **Tests.** `attachmentPreview.test.tsx`: referenced image is a chip; attachment-only previews once; near-miss id
   (a1 vs a10) and another attachment's URL; relative path and own-origin absolute URL; off-origin URL keeps the preview;
   video named in the description keeps its player; fenced code, inline code, escaped, indented code and a plain link
   keep the preview; a title, angle brackets, a query or hash, and an upper-case path hide it. `detail.test.tsx`: one
   img on the page, none in Activity.
5. **Browser.** Real run (Brave, 1280 px, dev stack): attachment-only issue shows one img (area); issue with an inline
   markdown image shows it once in the description, a chip for it, and previews only the other attachment. Before the
   change: two and four imgs.
6. **Not verified:** Safari, Firefox, dark mode, 375 px, the production CSP, and a comment that embeds an image (a
   comment-embedded image plus an attachment still shows twice).
7. **Why render, not walk a tree.** `renderedImageSources` renders the Markdown config to static markup and reads the
   `<img>` tags, instead of walking an mdast/hast tree. unified and remark are not direct dependencies of the web app.
   Rendering the real component with the same schema, `safeUrl` and `mdComponents` cannot disagree with the screen.
   `rehypeTaskList` is left out on purpose: it only builds checkboxes after sanitising and never makes an image.
8. **Cost (reviewers' measure).** `react-dom/server` is in the entry chunk: JS 789,980 B to 859,889 B (+69,909 B raw),
   gzip 228,379 B to 250,294 B (+21,915 B, about +9.6%). A 50 KB description with 290 images takes about 74 ms per
   run in jsdom. The result is memoised on the saved description, and the render is skipped when the text has no `![`.
9. **Known limit.** A lighter hast walk over the existing remark chain would cost about 0 kB but needs parity tests
   against the renderer. It is tracked as a follow-up ticket.

## Mapped items: not offered again in the repo picker (LAUN-113)

The request was to remove mapped items from the list and to show an alert on error. The intent, confirmed on the ticket: in the
list used to map repositories ("Repositories on the host"), a repository that is already mapped is NOT shown. An
earlier build read it as a Remove button on mapped rows; that is gone.

1. The picker filters the loaded pages by the live repo list: a host repo is hidden when its `repoKey` is the `repoKey`
   of a mapped repo. The server returns the canonical key (`scm.CanonicalRepoKey`) in both lists, so the web compares
   the strings and does not re-derive anything. The `mapped` flag of the host answer is not used: it can be stale after
   an unmap, the live list cannot. A map takes the row out after the repos refetch; an unmap puts it back the same way.
   The mapped list (`useGitRepos`, `GET .../repos`, `ListGitRepos`) is the live list and is NOT paged: the server returns
   every mapped repo of the integration in one answer (no limit, no cursor), which is why the filter is complete.
2. Filtering is on the client over the pages already loaded. The cursor, "Load more" and the search are untouched, so
   paging stays right even when a whole page is mapped. The "Mapped" label and the row Remove button are gone.
3. Empty cases: all loaded rows mapped with more pages: "Every repository loaded so far is already mapped. Load
   more."; all mapped and no more pages: "Every repository found is already mapped."; nothing from the host keeps the
   old texts.
4. An error from map, unmap or team change still shows in the one `role="alert"` banner. It is scrolled into view when
   it appears, because the picker can be far below the table.
5. Tests: `gitIntegrations.test.tsx` (hidden, map leaves, unmap returns, alerts, all-mapped text, Load more past a
   fully mapped page).

## Larger attachment image in a dialog (LAUN-117)

The ticket: clicking an attachment should show a larger version over the current view, not open a new tab.

1. An image preview is now a `<button>` ("View <name> larger"), not a link with `target="_blank"`. It opens the
   shared `ui/Dialog` (wide, 1100 px) with the same image. Esc, a click outside and the Close button close it;
   the Dialog already traps focus and puts focus back on the thumbnail.
2. The image is the API `downloadUrl` (never a silo URL). Inside the dialog a small link "Open original in a new
   tab" stays for people who want the file itself.
3. Guesses about intent: only image previews (png, jpeg, gif) got this. An mp4 already plays inline (LAUN-107).
   A pdf or other file stays a chip that opens in a new tab: the ticket shows an image, and an in-page pdf viewer
   needs a CSP change. Images shown by the description (markdown) are unchanged; a click there is not wired.
   No previous/next buttons (not trivial: previews are separate components).
4. Not verified in a real browser; tests cover open, Esc, focus return, close button, outside click, Enter.
5. Known gaps (from review): description images are plain `<img>` in `lib/markdown.tsx` with no click handler, so
   they do not open the overlay. Whether they should is a question for the project owner. There is no `onError` state for a
   broken image in the dialog. Safari does not focus a button on click, so focus return to the thumbnail may not
   work there. `84vh` may be taller than the visible area under a mobile browser bar.

## Options: the tick is enough (LAUN-118)

Ticket text: "Dont highlight the option, the tick is enough." The screenshot was not readable by the builder, so this is a reading.

1. **A chosen option in a picker list has no background.** The rule `.option[aria-selected='true']` in `ui.module.css` is removed. The tick (single choice) or the checked box (multi choice) shows the choice. This covers every `PickerList` / `PickerPopover` (filters, quick pickers, labels, product, issue search).
2. **Kept:** the hover tint (`.option:hover`), the keyboard-active row (`.optionActive`, which follows the arrow keys), `aria-selected` and `aria-multiselectable` (screen readers still hear the choice), and the focus ring of menu rows.
3. **The command palette** uses `aria-selected` for the active row only; its highlight comes from `.optionActive`, so it looks the same as before.
4. **Not changed:** toggle buttons (`aria-pressed`, view toggle, select mode) and selected issue rows. They are not option lists.
5. Tests: `src/ui/Picker.test.tsx`. Not verified in a browser.

## Remember the view per place (LAUN-109)

The request: the layout should not reset to grid all the time. The layout (list or board) reset when the person left
the page.

1. **Where it is stored: the browser only, for this build.** `localStorage`, per device. No migration, no route, no
   server change. The ticket first recommended server storage with a localStorage fast path; the project owner chose the cheap
   first step on 2026-10-28. Reasons: it fixes the complaint now, it needs no schema, and a stored choice that is
   lost (new browser) only costs one click.
2. **What is remembered:** the layout (`list` / `board`) and the sort. **Not remembered:** the filters (Linear does
   the same: filters clear unless a view is saved). **Grouping and visible properties do not exist in the app yet**
   (the list is always grouped by state, the row shows fixed fields), so there is nothing to remember for them. When
   they are built, add them to `StoredView`. There is no issue list on a project page either; the places are
   all issues (`all`), My issues (`mine`), each team (`team:<key>`), each cycle (`cycle:<team>:<number>`) and each
   product (`product:<slug>`).
3. **Key:** one JSON object per person and org, `launchpad.view.v1:<org>:<user id>`, holding `{ place: { view, sort,
at } }`. The user id keeps two people of one browser apart. `v1` is the format: a new format gets `v2` and the old
   key is ignored. At most 100 places are kept; the one changed longest ago goes first.
4. **The address wins.** `?view=` or `?sort=` in the URL is explicit and is never replaced or saved. What the
   address leaves out comes from the stored choice, then from the defaults. A stored sort belongs to the layout it was
   chosen in: a link that says `view=list` while the stored choice was made on the board gets the list's default sort.
   Applying the stored view writes nothing into the address, so history entries and Back (LAUN-89) are untouched. The
   manual sort rules (LAUN-110, LAUN-111, LAUN-51) are unchanged; Manual is stored like any other sort.
5. **When it is saved:** only when the person changes the layout or the sort (the page's setter compares them), not
   when a filter changes or a shared link is opened.
6. **Safe by default.** Every read and write is in try/catch; a stored value outside the allowed sets (a stale build, a
   hostile edit) is dropped, as is bad JSON. With storage blocked the page works as before; the choice still lives in
   the address for that visit. A `storage` event from another tab refreshes the choice.
7. **Reset view:** a ghost button in the bar (next to Sort, so it folds behind Filter on a phone). It shows only while
   the place has a stored choice or a non-default view. It forgets the place and shows the defaults (list, Updated);
   filters stay.
8. **Tests:** `viewPrefs.test.ts` (store, cap, junk values, throwing storage, URL precedence) and
   `viewPrefs.page.test.tsx` (leave and return, another place, shared link, reset, throwing storage, stale value).
   `setup.ts` now clears `localStorage` after each test. Not verified in a real browser; phone width and dark mode
   use the existing button style.
9. **Follow-up (ticket to file):** server storage that follows the person across devices. It needs a
   `user_view_preferences` table (org, user, place, view, sort, version), a migration, routes, openapi, and an
   `events` rule: user-private like notification settings, so no event (decisions-T13 #32). The web would read the
   server value and keep this localStorage as the fast path.
10. **Known gaps (from review).**
    - Manual sort chosen by the board button can stick to the list: the board starts in Manual (LAUN-110), List keeps the
      sort, and `{list, manual}` is saved, so it returns on every visit.
    - Reset view stays visible after the person returns to the defaults, because an entry equal to the defaults remains.
    - These two behaviours are tracked in LAUN-142.
    - Two tabs saving at the same moment can lose one entry (read, change and write of the whole blob, no lock).
    - The multi-tab refresh is tested only with a synthetic `storage` event, not with two real tabs.
    - Place keys replace unusual characters, so `a/b` and `a_b` would share a place. Real team keys, cycle numbers and
      product slugs cannot collide this way.

## Backups page (LAUN-78)

Settings, Platform, Backups at `/:org/settings/backups`, for a platform admin only (the nav item is hidden from others and a
direct visit gets "You need platform admin access."). Code: `web/src/features/settings/backups`, hooks `web/src/api/backups.ts`.
Server rules: `docs/internal/decisions-T16-backups.md` #44 and the build notes of tickets 2 and 8.

1. **Fixed text only.** Every sentence is in `backupText.ts`. An error message, a run's `errorText`, a banner's `errorText` and a
   change's `detail` never reach the screen. A server `errorCode` maps to our sentence (`codeText`); an unknown code shows as the
   plain identifier (`[a-z0-9_]`, 40 characters at most). A change `kind` we do not know shows as its plain identifier. Errors
   map by status and code (`backupErrorText`): 409 `conflict` has a sentence per context, `destination_busy`, `503
backup_unavailable`, 403 on a recipient cancel (another admin must cancel), 429, network.
2. **Secrets are write-only and short lived.** `secretsSet` gives "Set" / "Not set"; a typed value replaces, Clear sends `""`,
   Keep it undoes. Calls that carry a secret (create, update, unsaved test, exchange) are plain async functions, not mutations,
   so the mutation cache never holds one. The secret state is cleared after a save; the dialog unmounts. The Dropbox
   authorisation code is cleared as soon as it is sent. The private key box is a masked textarea (`-webkit-text-security`; Firefox
   has no such property, so it shows the text while typing: not verified in a browser). Nothing goes in the URL, storage or logs.
3. **Re-enter rule (decisions #25).** If the connection settings (any config field or the pin / host key) differ from the saved
   ones, the form lists every stored secret that is not yet entered and keeps Save off. The page sends only changed fields. A
   Dropbox destination whose connection changed also clears the refresh token (it can only be cleared, so the admin authorises again).
4. **Probe and test** (decisions #36), in one panel (`CheckPanel.tsx`). Probe has no credentials. A `tls` probe with a verified
   chain: "valid for this host", no pin. A `tls` probe without a verified chain, or an `ssh` probe: the fingerprint is shown and Test
   stays off until the admin ticks "I compared this fingerprint". A `plain` probe: "Not encrypted in transit" with a warning
   and a required tick. The confirmed fingerprint is the pin / host key: a saved destination with no pin and no other change
   uses `POST .../{id}/test` with `confirmedFingerprint` (the server saves it first). Otherwise the pin is put in the form and
   an unsaved test is used (`useSavedSecretsFrom` only when the config still equals the saved one). Dropbox has no unsaved
   test. The run is polled every 1.5 s until it ends; after 45 s without a start the page says the offsite worker may not run.
5. **Key gate (UI only).** Enable stays off until the signing public key exists and the admin ticks "I have saved the private
   key and the signing public key". The tick is not stored; the server checks the rest.
6. **Banners** come from `GET /status`: the nine codes each have a sentence; `backup_stale` and `backup_run_failed` list the
   destinations; `destinationsUnreadable` adds one more line. `mail_off` links to the Platform page.
7. **Type picker** offers only `supportedTypes` of the status route. The API has no driver catalogue, so the field lists are in
   `destinationTypes.ts`. S3 and WebDAV match the shipped drivers (build notes of tickets 6 and 7). SFTP and Dropbox were
   checked against the real `config.go` files of the UNMERGED branches `claude/laun-76-...` and
   `claude/laun-77-backups-dropbox-driver`. SFTP: `host`, `port`, `username`, `folder`, `hostKey`
   (`SHA256:...`), secrets `password` and `privateKey` (no passphrase). Dropbox: `access` (`app_folder` default |
   `full_dropbox`, which needs a `folder`), `folder`, `tlsPin`; secrets `appKey`, `appSecret` (and `refreshToken` from the
   exchange). Re-check when those MRs merge. A saved destination that leaves a field out is read as the type's default
   (for example Dropbox `access`), so it does not count as a changed connection.
8. **WebDAV** says to include the full WebDAV path (a missing base path makes the test fail at sign-in) and refuses `http://`
   in the form (the driver is https only).
9. **Recipients:** state, hold time, full fingerprint on demand, cancel a pending add or removal (POST cancel), Remove for an
   active key (DELETE, schedules in 24 hours) with the host command shown in the confirm. No fast web removal exists.
10. **Limits mirrored in the form** (server: `backupapi`): name 1 to 60 characters; `keepLast` 1 to 365; `verifyEveryDays` 0 to 90;
    `staleAlertHours` 0 to 8760. A folder has no slash at the start or the end (SFTP may start with one slash).
    **API gaps (nothing changed on the server):** there is no driver catalogue.
11. **Probe state.** The probe result, the fingerprint tick and the plain-http tick belong to one set of connection values. The
    check panel is keyed on the type and the connection fields, so a change drops them. The pin is not part of the key (a
    confirmed test writes it). A run is polled every 1.5 s until it ends, or until 5 reads fail in a row; then the page says
    it stopped asking. Double submit is guarded for Authorise, Delete destination and Schedule the removal.
12. **Private key box.** Firefox ignores `-webkit-text-security`, so the box shows a one-line warning. A key can also be loaded from
    a file, which the page does not display.
13. **Banners** at load use `role="status"`; only failed actions and a failed load use `role="alert"`.
14. **Tests:** `backups/backups.test.tsx` (63 tests). Mutants checked: key gate, plain tick, fingerprint tick, platform gate,
    re-enter rule, server text, secret leak, run error text and the Dropbox code clear each make a test fail. Round 1 added mutants for the 0 to 90 limit, the notice remount, the probe key, a secret in an attribute, the polling stop and give-up, the runs filter, the Remove error context, the Dropbox save-first block, the delete guard, the worker hint and the folder rule.
    **Not verified in a browser:** layout at phone width and in dark mode (the page uses the shared settings classes, cards and
    `overflow-x: auto` on tables), keyboard order in the dialog, focus rings, the masked key box, and any real server.

15. **Known gaps (from review, filed as a follow-up ticket).**
    - The key file has no size cap and no `onerror` handler on the reader.
    - The enable-409 sentence says "Check four things" and lists five.
    - The Enable button has no `aria-describedby` for the gate hint.
    - The recipient cancel buttons have no double-click guard.
    - `runTest` edge case: a first pin together with one retyped secret.
    - A pin edited by hand after a probe keeps the probe fingerprint.
    - A Test result stays on screen after a secret is edited.
    - The delete guard (disabled button and early return) is defence in depth: either one alone is enough.
    - Nothing was viewed in a browser: phone width, dark mode, dialog focus order, and the masked key box in Firefox are all unchecked.

## Remembered view: Manual sort and Reset view (LAUN-142)

Follow-up of LAUN-109, gap 10. Still browser only (server storage is LAUN-141).

1. **A sort that was not chosen is not kept.** When the layout changes and the sort is still the old layout's default
   (Updated on the list, Manual on the board), the sort becomes the new layout's default. So the board button's
   Manual does not follow the person back to List. A sort the person picked (any value other than the old layout's
   default) stays across a layout change.
2. **An entry equal to the defaults (list, Updated) is deleted, not saved.** A stored entry like that from an
   earlier build is ignored, so Reset view shows only when the view differs from the defaults.
3. **Tests:** `viewPrefs.test.ts` (default entry deleted) and `viewPrefs.page.test.tsx` (Manual does not stick, a chosen
   sort stays, board and back leaves no entry and no Reset view, an old default entry shows no button). Not verified in a
   real browser.
4. **Trade-offs (known, accepted).**
   - An old stored `{list, manual}` from before this fix is not migrated. It looks like an explicit Manual. Leave it by
     picking Updated, or by Reset view.
   - An explicit Manual on the list is lost after a trip through the board, because the board default equals Manual.
   - A shared `?view=board` link with a stored `{list, priority}` shows Manual on the board. Clicking List then resets the
     sort to Updated and deletes the stored Priority.
   - `CyclePage` and `ProductPage` share the hook, but only `IssuesPage` has page tests.
   - Not checked in a browser.

## LAUN-151: slugs are hidden where the name is enough

The ticket says "We can hid the slugs here" and the screenshot was not readable by the builder, so
the places are a guess. A slug is an internal address part; the person did not type it and the
name already says which product it is. Hidden: the slug under each product name in Settings,
Products, and the slug beside the product name in the Product page header. Kept: the Slug input in
the product dialog (the person edits it, with its link warning), the read-only Slug field in
Settings, Organisation (it explains the address), the organisation slug in the import confirmation
(the person types it), the sign-up Slug input, and every address (`/product/<slug>`, filters,
the command palette). Issue identifiers such as LAUN-123 are not slugs and stay.

## Notifications: radio buttons (LAUN-136)

The ticket says only "What to hear about should also use radio buttons". The exact intent is a guess (the screenshot was
not read). "Also" is taken to mean "like the Sign-up mode choice on the Platform page".

1. Every Default/Inherit, On, Off `select` in Settings, Notifications is now a `role=radiogroup` of three native radios
   (`ValueRadios`). This covers "What you hear about" and the team override panel, which share `EventRow`.
2. The group name is "<event>, <channel>" as before. The "Now ..." line is still its `aria-describedby`. Native radios give
   the arrow keys; each group has its own `name`, so the arrows stay inside it. The first radio keeps the text
   "Default (On)" or "Inherit (On)".
3. A choice is not one group for the whole section: every event and channel is its own value, so there are ten groups
   (five events, Email and Inbox). Values sent to the API and the save chain are unchanged. No server change.
4. Not checked in a browser. The layout (a column of three radios per channel) is a guess.

## Alerts: one stack (LAUN-150)

The ticket says only "2 in the same small space". The screenshot was not read (by the builder or the reviewers), so which
two alerts is a guess. Two candidate places are fixed:

1. The app shell: the thin "Reconnecting..." bar and the "Get Launchpad ready" bar sit one on the other at the top of the page.
2. The git connection page (`IntegrationPage`): up to four flush siblings (the attention banner, the "connection is off"
   banner, the note and the error) in a plain block with no gap.

Other places with two banners (Settings, Platform login status; Backups) already use a grid with a gap and are unchanged.

- New `AlertStack` (`ui/AlertStack.tsx`, `.alertStack` in `ui.module.css`): a flex column, 8px gap, each child its own row,
  long words wrap, an empty stack takes no space. It adds no role; every alert keeps its own.
- Used in the shell and in `IntegrationPage`. No change to roles, text or conditions.
- Use it wherever 2 or more alerts can show together in a plain block.
- The 8px gap shows the page background between full-bleed bars (shell). This needs a visual check.
- Not checked in a browser (phone width, dark mode).

## Overlapping text and the relations row (LAUN-152)

The ticket says only "text overlaps. also the blocks row is messy". The screenshot was not read, so the screen is a guess:
the Related section of the issue page.

1. The add row: `.ghostBtn` has a -6px side margin (made for the properties column). In the wrapping `.relAdd` row it makes
   the "Add relation..." picker touch the type select (a 6px misalignment; it does not cover it). The margin is 0 there now;
   children may shrink. The real cause in the screenshot is unconfirmed.
2. A relation row (`.relRow`) now wraps. The status icon and the remove button do not shrink; the title link takes the
   rest and truncates; an error under it takes a full line and wraps long words.
3. No change to markup, roles or behaviour. Tests read the CSS as text (`relationsLayout.test.ts`).
4. Not checked in a browser (320px, dark mode). The "blocks" timeline entry in `Activity.tsx` was not examined.

## Backups page: review follow-ups (LAUN-146)

Closes the "Known gaps" of "Backups page (LAUN-78)" item 15 (except the delete guard note). Code:
`web/src/features/settings/backups/`. Numbers follow the ticket.

1. **Key file.** A file over 64 KiB (`KEY_FILE_MAX_BYTES`) is not read; the page shows "This file is too big for a key..." (a
   `role=alert` under the file box). A failed read (`onerror`, or a result that is not text) shows "The file could not be
   read...". Both sentences are fixed and hold no file name. The box stays masked and keeps what it had.
2. **Enable 409.** "Check four things." is now "Check these things." (the fifth item is only for Dropbox, so no count is the
   smallest safe wording).
3. **Enable button.** While disabled by the key gate it has `aria-describedby` pointing at the gate hint (`#enable-gate`). When
   enabled it has none.
4. **Recipient cancel.** "Cancel the add" and "Cancel the removal" have a per-key in-flight guard (a ref set) and are disabled
   while their request runs. As with the delete guard, either part alone is enough (defence in depth); the test fails only when
   both are removed.
5. **First pin with a retyped secret.** The server uses `useSavedSecretsFrom` only when the config equals the saved config,
   and a first pin changes it (checked in `backupapi/actions.go`). So no route can reuse the other saved secrets with a new
   pin. Decision: when the test would save a first pin (saved destination, no pin, fingerprint ticked) and a stored secret is
   not retyped (or cleared), the page sends nothing and shows a fixed sentence: enter every saved secret again. With every
   stored secret retyped it sends the unsaved test with the pin and the typed secrets, as before. With no typed secret the
   saved test route still saves the pin. (`PageBlocked`, `FIRST_PIN_NEEDS_SECRETS`.)
6. **Notices saved.** The line is hidden as soon as any notice field changes, so it does not return when an edit is undone.
7. **Hand-edited pin.** Decision: the fingerprint tick holds only while the pin box is empty or holds the probed fingerprint.
   Otherwise the tick shows as off, Test stays off and a hint says to clear the box or paste the fingerprint. The tick is
   derived, not cleared: it comes back if the box is put right. The confirmed fingerprint is therefore never sent beside a
   different hand-typed pin. A test with a hand-typed pin and a probe that needs no confirmation is unchanged.
8. **Test result and secrets.** Any secret edit (type, Clear, Keep it, type switch) clears the Test result and its error. The
   probe, its fingerprint and ticks stay (the panel is not remounted). A test already in flight when the edit is made can still
   show its result afterwards (not handled).
9. **Docs format.** `docs/internal/web-decisions.md` was formatted with `prettier --write` (7 added, 10 removed lines; text
   unchanged). The web lint still does not cover `../docs`; no script was added.
10. **Drivers.** The sftp and dropbox drivers are merged. `destinationTypes.ts` was compared with
    `server/internal/backup/driver/sftp/config.go` and `dropbox/config.go` and the secret names in `sftp.go` and `dropbox.go`:
    SFTP `host`, `port`, `username`, `folder`, `hostKey`, secrets `password`, `privateKey`; Dropbox `access`, `folder`, `tlsPin`,
    secrets `appKey`, `appSecret` (`refreshToken` from the exchange). They match, so item 7 of the LAUN-78 section is verified
    against the merged files. Still from documentation only: no real SFTP server or Dropbox account was used.
11. **Tests:** 11 new tests in `backups.test.tsx` ("follow-ups (LAUN-146)"). Mutants checked, each fails a test: no size cap, no
    `onerror`, "four things" back, no `aria-describedby`, no PageBlocked, no hide on edit, tick ignores the pin, no Test reset;
    recipient guard: both parts removed fails, one alone does not.
12. **Not verified in a browser (none was available):** phone width, dark mode, dialog focus order, the masked key box in
    Firefox, the file picker with a real large file.

Touch-ups after the first review (both reviewers PASS):

13. **Prettier edits to earlier sections** (item 9) are whitespace only: no word of an earlier section changed.
14. **Pin hint** (`pin-differs`) is one `role=status` paragraph, so a screen reader hears that the tick dropped. Only one
    can show at a time.
15. **Recipient cancel buttons** use `aria-disabled` plus the ref guard, not `disabled`: keyboard focus stays on the button
    while its request runs, and a click in that time does nothing. This replaces the `disabled` part of item 4.
16. **Stale Test response.** A Test started before a secret edit is dropped when it lands afterwards (a token that the edit
    bumps); its error is dropped too. The server may still have queued the run (and a first pin may be saved by it). This
    replaces the "not handled" line of item 8.
17. **First-pin sentence** is generic on purpose: "The saved secrets work only with the saved settings. Enter all of them
    again, or test without typing a secret." For sftp, a stored password with a newly typed private key also asks for the
    password again (the server would refuse a test with the saved password and a new pin anyway).
18. `destinationTypes.ts` header comment now says all four drivers are built and checked against `config.go`.
19. **Tests:** 80 in `backups.test.tsx`. New mutants killed: file error not cleared, stale error kept, hours-only edit,
    pin without trim, recipient ref guard alone, `aria-disabled` alone, stale-token, status role.

## Breadcrumb trail (LAUN-157)

The ticket says "Nicer breadcrumb view at the top." with a screenshot that the builder could not read. **The intent is a guess:**
a clear, accessible team > section > item trail on the cycle page and the issue page.

1. **One component:** `web/src/ui/Breadcrumbs.tsx` (+ `breadcrumbs.module.css`). Before, the cycle page had its own
   `nav` and `ol` with a CSS `>` separator, and the issue page had a lone team link beside Back. Both use it now.
2. **Markup:** `nav aria-label="Breadcrumb"`, an `ol`, one `li` per item. The last item is the current page: no link,
   `aria-current="page"` on its `li`, and an `h1` only when the page asks (`heading`, the cycle page). Separators are real
   `aria-hidden` chevrons (not CSS content), so no reader says them.
3. **Issue page trail:** team (link) > parent identifier (link, only for a sub-issue) > this issue's identifier. The title stays in the
   page body. A sub-issue now has two links to its parent (the trail and the "Sub-issue of" line): that is fine. Routes and behaviour are unchanged. The Back button stays first.
4. **Long text:** every label has `min-width: 0`, `overflow: hidden` and an ellipsis, and a `title` with the full text. The
   current item shrinks last.
5. **Phone (520 px and narrower), judgment call:** only the NEAREST ancestor and the current item show; every earlier item
   is hidden and one `…` goes before the nearest ancestor's chevron (cycle page: `… > Cycles > Cycle 3`, so the Cycles link
   stays; a two-item trail shows both, no `…`). Chosen over "first and last" because the parent is the link a person
   uses to go up. The trail never wraps.
   Issue bar: `.barLeft` has a `160px` flex basis and `.crumbs` a `8ch` minimum, so the toolbar still wraps to a second line
   (at 320 px the six-button toolbar goes under, and the trail gets the full row) before the trail is squeezed. Reasoned, not seen.
6. **Look:** 13 px, muted ancestors, current item in the text colour and semibold, a link gets a surface-hover background on
   hover and the global focus ring. Tokens only, so dark mode follows.
7. **Not converted yet:** pages with ad-hoc headers (CycleListPage, ProductPage, the git IntegrationPage back link, the
   settings pages) do not use the component. The project owner should confirm which page the screenshot shows.
8. **Tests:** `ui/Breadcrumbs.test.tsx` and two in `issues/detail.test.tsx`. Mutants killed: no `aria-hidden`, no `aria-current`,
   no hide class, no `aria-label`, `ul` for `ol`, no ellipsis, a 40% cap on the first item, the `…` visible on desktop,
   `heading` ignored, `title` removed, `aria-current` on the wrong item, `after` on every item, `.barLeft` basis 0, no `8ch` minimum.
   The CSS tests parse declarations (no property order). Mutants ran in a copy outside the worktree. One survivor is
   equivalent (`gap` without `i > 0`: item 0 has no separator).
9. **Not verified in a browser (none was available):** phone collapse, dark mode, hover and focus look, the issue bar when
   the toolbar wraps.

## Remembered view follows the person (LAUN-141)

Server storage for LAUN-109's choice. Rules and the server side: `docs/internal/decisions-T23-view-preferences.md`.

1. The layout and the sort of a place are also stored on the server (`GET /me/views`, `PUT /me/views/{place}`).
   localStorage stays the fast path; the page never waits for the network.
2. At the first render of an issue page (per org and page load) the web reads the server list and merges per place:
   the newer `at` wins. Local-only or newer-here places are sent. A page that cannot reach the server works as before.
3. A reset ("Reset view", or going back to list and Updated) is sent as the defaults with its time, so another
   device drops its old choice at its next load. The time is kept in `launchpad.viewsync.v1:<org>:<user>`.
4. A change on another device appears at the next page load, not while the page is open.
5. Tests: `viewPrefs.test.ts` (merge), `viewPrefs.page.test.tsx` (new device, server down). Not verified in a browser
   or on two devices.

## List drag follow-ups and the image finder without react-dom/server (LAUN-131, LAUN-132)

One branch, web only. No server change.

1. **The live region tells the result (LAUN-131).** A drop and an Up / Down key move say "moved to ..." only after
   `actions.place` resolves `true`. If it resolves `false` (the team lacks the state, or the update failed and was
   rolled back) the region says "ORB-n could not be moved"; the toast of `actions` still gives the reason.
2. **Fast key repeat.** `latestGroups` changes only after a render, so a second press could use the old order. While
   moves are in flight, `IssueBrowser` keeps the working order of each group in `keyOrder` (updated at once on every
   press) and clears it when the last pending move ends or any move fails. The writes are already one line per issue
   (`useUpdateIssue`), so the versions follow. The 800 ms refocus timer is cleared by each press, and the layout
   effect puts focus back on the handle only when focus is on `body` (the row was drawn again); if the person has
   moved on, focus stays where it is.
3. **Wording.** Escape, a lost pointer or a blur still say "Move of ORB-n canceled". A drop on the row's own place says
   "ORB-n stays in its place"; a drop outside the list says "ORB-n was not moved: dropped outside the list".
4. **Resize.** A window `resize` during a drag measures the rows again (`ListDrag.onResize`). Test helper
   `shiftListLayout`.
5. **Not done.** A roving tabindex (one tab stop for the whole list; item 4 of the ticket was "consider"): it changes
   how every row is reached by keyboard and needs a screen reader check we cannot make here. An unrelated refetch in
   the committed phase can still end the drag picture early (the 500 ms fallback bounds it).
6. **`renderedImageSources` is a hast walk (LAUN-132).** It runs the chain react-markdown builds (`remark-parse`,
   `remark-rehype` with `allowDangerousHtml`, `rehype-sanitize` with OUR `schema`), walks the tree for `img`, applies
   `safeUrl` and `isOwnImage` to `src` (the two steps the renderer takes) and so finds exactly the images that
   render. `unified`, `remark-parse` and `remark-rehype` are now direct dependencies, at the versions
   react-markdown already used (no new code in the bundle). `react-dom/server` is gone from the app.
7. **Bundle (`npm run build`, the one entry chunk).** Before: 929,265 B raw, gzip 272.65 kB (vite), 270,075 B
   (`gzip -6`). After: 859,764 B raw, gzip 250.61 kB (vite), 248,292 B (`gzip -6`). Change: -69.5 KB raw, about -22 KB
   gzip. The ticket's estimate held.
8. **Guards.** `renderedImages.test.tsx` renders the real `Markdown` for 28 texts and compares the `img` sources with
   the function (code, escapes, plain links, title, angle brackets, query and hash, upper case, off origin,
   reference style, collapsed reference, image in a link, raw html, data: / javascript: / blob:, protocol relative,
   the same image twice, lists, quotes, headings, empty src); a source scan fails on any import of
   `react-dom/server`; `check:dist` fails if `renderToStaticMarkup` is in a built `.js`.
9. **Unverified.** No browser run, and no screen reader (VoiceOver, NVDA, TalkBack): what is announced is the text in
   the live region, tested as DOM text only.

## Video in a dialog with a download button (LAUN-159)

Ticket: "Add support for uploading videos. videos should load as a pop over like images and play inline, as well as a download button."

1. **Already there:** the server allowlist and 100 MiB limit name `video/mp4` (T3b #3); upload by picker, drop and paste
   already accepted mp4 (`lib/attachRules.ts`); `Content-Disposition: inline` for video (T3b #11); CSP `media-src` (LAUN-107).
   Before this ticket an mp4 played in a small inline `<video controls>` with a filename link.
2. **Added:** (web only, no server change) an mp4 attachment is now a thumbnail button ("Play <name>"): a muted first frame
   (`preload="metadata"`, no controls, never autoplay) with a play badge. It opens the shared `ui/Dialog` (wide) with
   `<video controls playsInline preload="metadata">` from the API `downloadUrl`, and a "Download <name>" button (an `<a download>`).
   Esc, outside click and Close shut it; the Dialog traps focus and returns it to the thumbnail. Colours are tokens.
3. **Not extended:** webm and quicktime stay refused. The allowlist, openapi and silo-signed types are unchanged.
4. **Not done (follow-up):** videos linked in markdown descriptions do not open the dialog; no poster from the server.
5. **Not verified:** no browser run, no real playback; the `download` attribute is ignored by some browsers when the 302
   goes to another origin (the file then opens in the player tab instead of saving); Safari/iOS (thumbnail first frame
   under `preload="metadata"`, playsInline, focus return), phone width and dark mode were not seen.

## LAUN-116: no past cycles in the cycle switcher menu

The project owner's answer: the place is the switcher menu on the cycle page (`CyclePage`, `switchWrap` / `switchPanel`). The earlier
attempt (the issue-page and create-dialog pickers, 15fd8d9) was the wrong place and is reverted by a new commit (no rewrite).

1. **Source.** Client filter, no server change: the menu already works from `useCycles`, whose rows carry `startsAt`,
   `endsAt` and `completedAt`. `switcherCycles` (`cycles.ts`) takes the neighbours and drops a past one by `cycleKind`
   (completed, or `endsAt` gone by; so an ended cycle the job has not completed yet is also past).
2. **What stays.** "Next cycle" and "Previous cycle" show only when that neighbour is current or upcoming. The cycle being
   viewed stays when it is past: a "This cycle" row, marked `current`, detail "Past, <range>", selecting it does nothing.
   Order: This cycle, Next cycle, Previous cycle, All cycles.
3. **Reach the past.** "All cycles" (already last in the menu) opens the Cycles list, which keeps its "Past" section.
4. **Empty state.** Nothing but "All cycles" is a valid menu (as with one cycle).
5. **Keys unchanged.** Alt+J / Alt+K still use the unfiltered neighbours, so Alt+J still walks back through history even
   though the menu no longer lists the past cycle. Menu focus, Escape, `aria-keyshortcuts` (only on listed rows) and the
   phone CSS are untouched.
6. **Gap.** Not seen in a browser. From a past cycle the menu does not offer a jump to the current one unless it is the
   direct next neighbour; use "All cycles".

## Palette search: limited to the current team (LAUN-114)

The project owner answered the ticket ("Search should show just repos on that team?"): limit search to the current team, with a way to widen it.

1. **Which search.** The command palette (Cmd/Ctrl+K), the only global issue search. The issue list's filter box already sits on a team page with the team locked. The issue pickers (relations, parent) are unchanged: they search every team.
2. **"Inside a team".** The palette was opened on `/:org/team/:key/...` (any team page), or on `/:org/issue/:identifier` (the team is the identifier's prefix, matched to a known team key). There is no stored "last picked team". All issues, My issues, Product, Inbox and Settings have no team context: search is over all teams, and no toggle shows.
3. **Default.** Scoped when there is a team context. The request is the existing `GET /issues?query=...&team=KEY`; no server or openapi change.
4. **Toggle.** A line under the input, a polite live region (`role="status"`): "Searching ORB only (ticket numbers search all teams)" or "Searching all teams". The button has a STABLE label "All teams" and `aria-pressed` (never a changing label as well). Pressing it, or the empty-state button, moves focus back to the search input. It lasts for that opening of the palette only; it is not stored.
5. **Ticket numbers.** A search text that is a bare number (`113`) or an identifier (`ORB-113`) is sent WITHOUT `team`, so the ticket is found in any team (the LAUN-133 order is kept). The "Open ORB-12" row already needed no search. The scope line says so.
6. **Empty state.** "No results in ORB. [Search all teams]"; the button does the same as the toggle.
7. **Tests.** `palette/paletteScope.test.tsx`. Mutants tried: dropping the `team` param fails 4 tests; removing the ticket-number exception fails 1.
8. **Not checked:** a real browser.
9. **Follow-ups, not done:** the empty-state button sits inside the listbox (invalid ARIA); the scope line shows late when the teams have not loaded; `useMemo` deps; the buttons have no `:focus-visible` rule of their own; mixed text such as "113 login" is scoped (untested).

## Projects in the sidebar and pages (LAUN-119)

Ticket: "Add support for projects." The screenshot could not be read; the project owner's answer is the scope: a Projects sidebar item, a list page and a project page, like Linear.

1. **Already there:** the server routes `GET/POST /projects`, `GET/PATCH /projects/{id}`, `GET/POST/PATCH /milestones`, the `project` filter on issues, the MCP tools, and the Project chip and row (create dialog, issue page). No server change was needed (no Go touched).
2. **Added (web only):** sidebar item "Projects" under All issues (a `NavLink`, so it is current on `/projects` and `/projects/:id`; same styles and tab order as its neighbours); a "Projects" entry in the command palette; routes `/:org/projects` and `/:org/projects/:project`; `features/projects/`.
3. **List page.** All projects that are not archived (50 per call, up to 20 calls), live first (in progress, planned, backlog, paused, completed, canceled), then by name. A row has name, status, lead, target date (past and still open: shown in the danger colour with a hidden "(overdue)") and progress. Loading, empty, error (retry) states. At 760 px the row folds to two lines.
4. **Progress is derived.** The server has no counts. Each row reads the project's issues (`project=<id>`, 100 per call, at most 5 calls). Canceled issues are out of the scope; a `completed` state is done. A project over 500 issues shows `N+` (a floor). The key is outside the issue-list key, so an issue event does not refetch it; stale after a minute. One request series per row; fine for tens of projects, a server count would be better for hundreds (follow-up).
5. **Project page.** Head (name, status, description, lead, start, target, progress, teams as links), milestones (by `sortOrder`, with target date, hidden when none), then the shared `IssueBrowser` with `extra.project`. The Product chip is hidden (`lockProduct`), a stray `?team=` / `?product=` in the address is dropped, the view is remembered per project (`placeKey('project', id)`), status columns come from the project's teams. "Create issue" shows for an open project with teams (the dialog is not told the project: it has no such default yet). A malformed id is "Project not found" without a request; so is a 404.
6. **Not done (follow-ups):** create and edit a project in the web app (the API supports it; the project owner asked for pages, so no dialog now); a link from the issue page's Project row to the project; milestones editing; a project filter chip on the issue list; the Create dialog taking a project default; a server-side issue count for progress.
7. **Tests.** `projects.test.ts` (helpers) and `projects.page.test.tsx` (sidebar, list, page). Mutants checked: counting canceled issues, dropping `project` from the progress call, dropping it from the page's issue query: each fails a test.
8. **Unverified.** No browser run: phone width, dark mode, keyboard order and screen-reader output were not seen (names and `progressbar` values are tested as DOM only). No test with a project over 500 issues.

## Org logo (LAUN-22)

Server rules: `decisions-T24-avatars.md`. Built in this ticket: the org logo. The project logo UI is not (see 5).

1. **`ui/Logo.tsx` is the shared avatar** for an org or a project: the image, or the first letter on an accent tile (token
   colours, so dark mode follows). A failed load shows the letter; a new URL tries again. `avatarSrc` keeps only the path and
   query of `avatarUrl`, so the `<img>` is requested on the page's own origin and the session cookie goes with it.
2. **Alt text.** The name, except where the name is written next to the logo (sidebar button, switcher rows, top bar), where
   `decorative` gives an empty alt so a reader says the name once. The preview in Settings has the name as alt.
3. **Where it shows:** the sidebar header button, the organisation switcher rows, the phone top bar. The current org's logo is
   read from `GET /orgs/{org}` (`useOrgDetail`); `GET /orgs` is read only when the person has more than one org.
4. **Settings, Organisation, Logo** (`OrgLogoField`): preview, Upload / Replace, Remove. The file type and size (PNG, JPEG, GIF,
   WebP, 2 MiB) are checked before any call; every error is a fixed sentence in `role="alert"`; the result is in `role="status"`.
   The buttons use `aria-disabled` while a call runs, so focus stays. The file input is hidden; the button opens it.
5. **Not built (follow-up):** the project logo in the project list, the project page header, the project chip and picker, and a
   project settings field. The project pages were being written on `claude/laun-119-projects` and are not on `develop`. The
   server routes and `avatarUrl` on `Project` are done, and `Logo` takes a project name and `avatarUrl` as it is.
6. **Not verified:** no browser run, no screen reader, phone width and dark mode were not seen. A presigned PUT to the store
   needs the store to allow it from the web origin (as attachments already do).
## Web rules for the git and import screens (LAUN-11, T17 #51)

Design: `decisions-T17-git-connections-import.md` #51. The rules were built with LAUN-9 (Settings, Integrations)
and LAUN-10 (Settings, Import from Linear); their sections above hold the details. This section writes the rules
down once, as they stand in the code, so the next screen of this kind follows them.

1. **Where a page goes.** In `web/src/features/settings/settingsPages.ts`, group Administration, `access: 'admin'`,
   with `keywords` for the settings search. Today: `integrations` (and `integrations/:integration`) and `import`.
2. **Who sees it.** Org admins only, the same gate as Teams (`SettingsLayout`). A member opens only Notifications
   and gets "You need admin access." elsewhere. A platform admin who is not an org admin reaches the Platform page
   only (`/settings/platform`), not these pages. The gate is a courtesy: the server judges every call by the admin
   scope and the org role.
3. **Secret-bearing calls are plain async functions.** The token and the webhook secret (`useCreateGitIntegration`,
   `useReplaceGitToken`, `useRotateGitSecret`) and the Linear key (the export call in `src/api/importer.ts`) are not
   `useMutation` and write nothing with `setQueryData`: no mutation cache or query cache holds the body or the
   answer. The value lives in component state and is cleared when the call returns (also when it fails) or the dialog
   closes. Same pattern as ORB-3447 (`useCreateToken`).
4. **Show a secret once.** A returned secret (webhook secret) opens `SecretOnceDialog`: Copy, a confirm box that
   gates Done, no close by Escape or a click outside before that.
5. **No server text in the UI.** Errors are fixed sentences chosen by status and `code` (`gitErrorText`,
   `importErrorText`).
6. **Tests.** Each page has a test that dumps the caches, storage, the URL, the page and later request bodies and
   finds no secret (`gitIntegrations.test.tsx`, `import.test.tsx`).

Not checked here: these pages in a real browser (the earlier sections list what each ticket did not see).

## Board drag in every sort (LAUN-167)

Report: dragging a ticket was broken on the latest build. The sort was not Manual, and the person expects to still move a ticket to another lane. The LAUN-110 rule "another sort turns the board drag off" (above) is **superseded for moves between columns**. Its rules for the order inside a column (Manual only) stand.

1. **Cross-column drop, any sort.** A card dragged to another column changes its state only: `PATCH {state, version}`, no `placeAfter` / `placeBefore` (the shown order is not the stored one). A group (LAUN-61) does the same, one PATCH per card (`placeMany(..., stateOnly)`); cards already in the target column stay out.
2. **Same-column drop, not Manual.** Nothing is sent and the card goes back (a cancel, no 500 ms wait). The live region says "Reordering inside <column> needs the Manual sort". Never a silent reorder.
3. **Where the card lands.** The board is sorted in the page (`boardColumns`), so the card lands where the sort puts it: Created and Priority keep the card's data, so its place follows from them. Updated: the server sets `updatedAt` on a state change, so the cached Updated lists move the card to the top at once (`touchIssueInCaches`, `touch` in `UpdateVars`). Any failure rolls the lists back as before.
4. **Announcement.** A cross-column move says "<ID> moved to <state>" (or "N issues moved to <state>").
5. **Hint** (board, not Manual): "Sort is X. Drag a card to another column to change its status; switch to Manual to reorder inside a column." The `data-drag-off` attribute and its cursor rule are gone. The Manual hint is unchanged.
6. **Keyboard.** The board has no keyboard drag; the status hotkey `s` and the bulk picker already change state in every sort. The list (Up / Down on the handle) stays Manual-only.
7. **List view: unchanged.** The handle shows only with Manual. Open question for the project owner: should the list also let a row move to another state group in other sorts?
8. **Tests.** `boardSort.test.tsx`, "board drag in every sort". Mutants that fail a test: the old gate in `onPress`; the group plan sending `placeAfter`; dropping `touch`; dropping the same-column cancel; sending `r.placement` for a cross-column drop.
9. **Not checked:** a real browser (no pointer capture, no real layout).
10. **Follow-up, not done:** `useAllStates` builds a new `states` array each render, so the board groups are recomputed each render (the drag re-measures each time). A `useMemo` over `useQueries` results is not safe without a stable key; left alone.

## Project logo UI (LAUN-162, LAUN-22 part 2)

1. **Where it shows:** the project list row (20 px), the project page header beside the title (28 px), the Project chip and
   picker rows on the issue page, and the Project chip and picker rows in the Create issue dialog (16 px). All use `ui/Logo`
   with `decorative` (empty alt) because the name is written next to the logo. With no logo the chip shows the letter tile
   (the Create dialog keeps the project icon while no project is chosen).
2. **The field.** A "Logo" section on the project page (`ProjectLogoField`), between the head and the milestones: a 48 px
   preview (the project name as alt), Upload / Replace, Remove, the rule line, errors in `role="alert"`, results in
   `role="status"`. Same client checks as the org logo (`LOGO_TYPES`, `LOGO_MAX_BYTES` are shared): PNG, JPEG, GIF, WebP, no SVG,
   2 MiB, checked before any call. The PUT to the store uses `prep.headers` as given. Error sentences are fixed; 403 says
   "You need write access to change the logo."
3. **No scope gate.** The web app has no write-scope check for any project or issue write (cookie sessions of members can write),
   so the field is shown to everyone, as other write controls are. A refused call shows the 403 sentence. If a read-only web
   role is added later, hide this field with the others.
4. **Caches.** `useSetProjectAvatar` / `useClearProjectAvatar` (in `projects/queries.ts`) write the answer into the project
   detail cache and invalidate the all-projects list and the per-team project lists, so the new `?v` shows without a reload.
   The issue pages read `projects` per team, so their chips follow after the invalidation.
5. **Tests.** `projects/projectLogo.test.tsx` and one case in `issues/detail.test.tsx`. Mutants checked, each fails a test: no type
   check, no size check, no detail cache write, no list invalidation, wrong prepare route, wrong DELETE route, PUT headers
   dropped, header / list logo not decorative, preview decorative, picker row logo dropped. "Hidden for read-only" does not apply (3).
6. **Not verified:** no browser run (layout of the 28 px header logo, phone width, dark mode, screen-reader output); the Create
   dialog chip and picker rows have no test of their own; the presigned PUT needs the store to allow the web origin.
   Follow-ups: a link from the issue Project row to the project, a project create / edit dialog with the logo.

## Sessions settings page (LAUN-160, LAUN-130)

1. **Where.** `/:org/settings/sessions`, group Account, access `member` (like Notifications). `SettingsLayout` opens it to a plain
   member by path, as it does Notifications. Code: `features/settings/SessionsPage.tsx`, fixed sentences in `sessionText.ts`.
2. **List.** `GET /me/sessions` (`useMySessions`, key `keys.mySessions`, no retry, always refetched on open). Columns: kind (Web
   for `session`, App for `native`), created, last used, expires, all through `day()` from `tokenText.ts` (UTC date, `Never` for
   null). The row with `current` has the tag "This device". No token value exists in the list.
3. **Revoke.** Another session: one click sends `DELETE /me/sessions/{id}` and a status line says it is done (no dialog; it
   cannot lock the person out). The current session: the button reads "Log out", a dialog warns first, and only its confirm
   sends the request. After the 204 the page runs `useClearSignedIn` (the clear half of Sign out, now shared with `useLogout`):
   cancel queries, drop all but the session entry, set the session to `null`; `RequireAuth` then shows the login page. The
   server already cleared the cookies, so there is no `POST /auth/logout`.
4. **Log out everywhere else.** A dialog, then `POST /me/sessions/revoke-others`; the status line says "Logged out N other
   sessions." with the server's count (it can exceed the rows shown), or "No other sessions were signed in." for 0.
5. **Caches.** Both mutations invalidate `keys.mySessions` on settle, also after a failure (a 404 means the row is gone).
6. **Errors.** Fixed text only: network, 401, 403, 404 (revoke: "already gone", dialog closes), 429, other. A 403 on the list
   (an agent token) shows an empty state "Sessions are for people" and no table; the buttons are disabled while it loads or
   after an error. CSRF is added by the shared client like any cookie write.
7. **Search keywords.** log out, sign out, devices, this device, revoke a session.
8. **Tests and mutants.** `settings/sessions.test.tsx`. Each mutant fails a test: current mark inverted, no warning for the
   current session, no sign-out clear after it, count changed, 403 sentence changed, no invalidation after DELETE or after
   revoke-others, wrong method on DELETE.
9. **Not verified:** no browser run (phone width scroll box, dark mode, screen-reader output, focus return after a dialog).
   The row aria-label names the kind and created date because rows have no other unique name.
   Follow-ups: a time of day in "Last used" (the server throttles it to a minute), a per-session device name.

## Create and edit project, group by product (LAUN-166)

1. **Server facts (no server change).** `POST /orgs/{org}/projects` and `PATCH .../projects/{project}` need the write scope; create
   takes `Idempotency-Key`, the PATCH is not idempotent and takes `version` (409 `version_conflict`). Create requires only `name`
   (1-255); `state` defaults to `planned`; `teams` may be empty on the server. The server has NO rule against editing an archived,
   completed or canceled project, so the dialog does not block it.
2. **Dialog (`ProjectFormDialog`).** One dialog for New project (list header, and the empty state) and Edit project (project page).
   Fields: name, description, status, lead (native select of active members; a lead who is no longer listed stays as an option),
   start and target date (native date inputs), teams (checkbox group in a fieldset). The web rule on create is "at least one
   team": the server allows none, but a project with no team cannot hold an issue from the Create issue button. Edit allows
   none (the server does). The stored description is compared trimmed, like the form value. Target before start is refused in the web before the call (same sentence as the server rule).
   After create the app opens the new project page.
3. **Edit sends only changed fields** with the `version` the dialog OPENED with, not a refreshed one: a background change
   makes a 409 with a fixed sentence instead of a silent overwrite. Clearing the lead or a date sends `null`; teams send the
   full set. No change sends no call.
4. **Errors are fixed sentences** (`projectText.ts`), by field where the server's 400 text names one (name, description, dates,
   lead, "unknown team", "at most 50 teams"; matched with `startsWith`), else a banner (`role="alert"`). Fields carry `aria-invalid`
   and `aria-describedby`; the teams fieldset has `aria-describedby` only (a group does not support `aria-invalid`); the first bad
   field takes focus. **Idempotency-Key:** the server stores a reply only for a 4xx other than 401 and 403
   (`idempotency.storable`). After such a reply (400, 404, 429, other 409) the dialog takes a NEW key, because a changed body under
   the old key is `409 idempotency_key_reused`. It KEEPS the key after status 0, any 5xx, 401, 403 and the codes
   `idempotency_in_progress` / `idempotency_outcome_unknown`: the create may have been committed, and a new key would make a
   second project. Those two codes show "The project may have been created. Check the project list before you try again." and
   "The first try is still running. Check the project list before you try again."
5. **Caches.** Create writes the detail cache and invalidates the all-projects and per-team project lists; edit does the same on
   success AND on failure.
6. **Grouping rule (`groupByProduct`).** Teams can be in several products and a project can span teams (T18 #5, #6), but a project
   must appear once. A project goes under the product that holds the MOST of its teams; a tie goes to the first product by name,
   then slug. Projects whose teams are in no product (or that have no team) go under "No product", last. Products with no
   project are left out. Order inside a group is the existing list sort. Why not "all teams": a project over two products would
   fall to No product, which hides it from its real owners. Why not "any, first by name": it ignores the project's centre of mass.
7. **Where and how it shows.** A one-line hint above the groups says a project with teams in several products appears under the
   product with most of them. The group header uses `--surface`. Only when the org has a product and the list is not empty: each group is a `<section>` with an `h2`
   (name and count) and a `<ul>` named by it. With no products (or a failed products read) the page is the flat list as before and
   there is no toggle. A "Group by product" checkbox (default on) is remembered in `localStorage`
   (`launchpad.projects.group-by-product.v1`, `off` or `on`, wrapped in try/catch). The issue-page remembered view (list/board +
   sort, server copy) does not fit, so this is a per-browser choice only.
8. **Tests.** `projects/projectForm.test.tsx`. Mutants that fail a test: no team rule, no name rule, no version, lead `undefined`
   instead of `null`, tie goes to the last, No product bucket dropped, grouping with no products, version-conflict sentence
   dropped, empty Idempotency-Key, wrong create route.
9. **Not verified:** no browser run (dialog layout and focus at phone width, native date inputs, dark mode, screen-reader output).
   Follow-ups: a logo field in the dialog, a product filter on the list, archive/restore a project from the page, a project
   picker with search for a long member list.

## Projects table, filters and Display (LAUN-165)

Web only, no server change. The project owner's decisions: a table with a header row, a filter bar and a Display menu like the issues page.
Code: `web/src/features/projects/` (`ProjectListPage.tsx`, `ProjectFilterBar.tsx`, `projectsView.ts`).

1. **A real `<table aria-label="Projects">`.** Name is a `th scope="row"` with the link; the other columns are `td`. With product grouping on,
   one table, one header row and one `<tbody>` per group, named (`aria-labelledby`) by a `th colSpan` that holds the `h2` (name and count). Flat
   when grouping is off, the org has no product, or the products read fails (LAUN-166 rules unchanged).
2. **Columns:** Name (always), Health, Priority, Lead, Target date, Status, Progress. **Priority starts hidden**: a project has no priority
   field on the server, so the cell is "No priority" for every row and the header is not sortable. Follow-up: a server field. Progress is
   not sortable (it is read per project, not in the list).
3. **Health is derived in the browser** (`healthOf`), because the server stores none: finished (completed, canceled) has none; a target date
   in the past is Off track; paused, or planned/backlog with a target within 14 days, is At risk; any other open project with a target is On
   track; no target is "No data". It is a text label as well as a colour (never colour alone). It is a hint, not a status update.
4. **Sorting.** Header buttons (real `<button>`, Enter and Space) set `aria-sort` on the active `th`; a second click flips the direction. The
   default is Status ascending, the order the page had (live first, then name). A missing lead, target date or health sorts LAST in both
   directions; a tie falls back to the name. The Sort select and the Ascending/Descending button in the bar do the same, and are the only sort
   on a phone (the header row is hidden there).
5. **Filters** (AND): search by name, Status (several), Lead (one, or "No lead"), Team (several, any match), Product (one; a project matches when
   any of its teams is in the product, which is wider than the one-bucket grouping rule). They reuse `PickerPopover` and the issues page's chip
   look. "Clear (N)" resets them. A filter that hides everything shows "No projects match" with "Clear filters". The count shows "N of M".
6. **Display** is a multi-select `PickerPopover` of the columns. The "Group by product" checkbox stays in the page header (LAUN-166).
7. **Remembered per browser** in `localStorage` `launchpad.projects.view.v1` (sort, direction, columns, filters), read and written in try/catch,
   every field validated on read (junk falls back to the default field by field). The search text is NOT remembered. There is no server
   preference for this page (the LAUN-141 pref is the issue page's).
8. **Phone width (max 760px):** the header row is hidden and each row is a flex-wrap card: name on its own line, the other cells wrapping
   under it. The scroll container is `.scroll`, so the page itself does not scroll sideways.
9. **Tests:** `projectsView.test.ts` (pure), `projectsTable.test.tsx` (semantics, sort, keyboard, filters, Display, remembered view, storage
   throwing). Mutants that fail a test: no `aria-sort`, missing values not last, product filter off, search remembered, "No lead" filter wrong.
10. **Not verified:** no browser run (phone layout, dark mode, screen-reader output of the table when rows are `display: flex`, focus ring
    look). Follow-ups: a project priority field; a real health/status update; sortable Progress.

## LAUN-163: board Columns control

The request was for Linear-like board settings; the answer: COLUMNS, remembered PER PLACE (not card fields, not density). The server
rules are in `decisions-T23-view-preferences.md` #10 to #13 (migration 0024).

1. **The control.** A "Columns" button in the filter bar, board only (`ColumnsControl.tsx`). It opens a panel (the `ui.panel` shell)
   of native checkboxes: "Show empty columns", one box per state (with the card count), and "Reset columns". Native boxes give Tab,
   Space and screen-reader state for free; Escape closes and returns focus to the button; a click outside closes. The button name
   is "Columns", or "Columns (changed)" when a choice differs from the defaults. A polite live region (`columns-live`) says what
   changed ("Done column hidden. 3 of 4 columns chosen."). It sits with Sort in the "more" fold, so on a phone it is behind Filter.
2. **Hiding.** Pure functions in `boardColumns.ts`: `applyColumns` (hidden state, or hideEmpty and no card) and `toggleColumn`
   (the last column that is not hidden cannot be hidden: its box is disabled and described by "At least one column must stay.").
   A hidden column leaves the DOM, so it cannot be a drop target; it also leaves the j/k walk, the selection (the existing
   "rows that left the list" effect clears it) and a block drag. The LAUN-167 rule and Manual ordering are untouched (the drag code
   only sees the columns that show).
3. **Totals.** Each column keeps its own count. A note above the board says "N columns hidden, holding M issues." with a
   "Show all columns" button, so hidden cards are still counted. A filter or hideEmpty that leaves no column shows an empty state
   ("No columns to show for these filters") with "Show all columns".
4. **Remembered per place** in the same store as layout and sort (`StoredView` gets `hideEmpty`, `hidden`), localStorage first and the
   server through LAUN-141. An entry from before has neither key and reads as the defaults; hostile values are dropped
   (`cleanHidden`). The defaults mean no entry (LAUN-142), so choosing the defaults again deletes the entry. Choosing columns stores the
   layout and sort that were already stored (or the defaults), NOT what the address shows, so `?view=board` is not remembered by a
   column click. A layout or sort change keeps the columns. "Reset view" clears them too; "Reset columns" clears only them.
5. **Not built: grouping.** The board is state columns only (`boardColumns` groups by state name, and the drag moves a card by
   state). Grouping by assignee, priority, label or project would need: a group-by choice in the stored view (a new column and
   enum), a different column source with no state meaning, drag rules per group (assignee or priority change instead of state,
   or no drag), and the Manual order per group. Follow-up ticket.
6. **Tests:** `boardColumns.test.ts`, `columns.page.test.tsx`, updated `viewPrefs.test.ts`. Mutants that fail a test: hideEmpty
   ignored, hidden state ignored, last-column guard off, column keys dropped on read, hideEmpty forced, columns missing from the
   default check, hidden columns still given to the board.
7. **Not verified:** no browser run (panel position and width at phone width, dark mode, real screen-reader output, the drag with
   real layout). Stale hidden names are not shown in the panel (they are dropped at the next change).

## Burn-up chart show / hide (LAUN-164)

1. **What it does.** A button in the head of the burn-up section: chevron plus "Hide chart" / "Show chart", `aria-expanded`,
   `aria-controls` the chart region (`useId`). Hidden: the heading and the button stay; the legend, the SVG, the sr-only table
   and the pointer handlers are not rendered, and the width observer is released. Default: shown. The button is absent when
   there is no chart (no rows, or an upcoming cycle).
2. **One choice for all cycles**, not one per cycle: it is a view preference ("I do not want the chart"), as in Linear.
3. **Storage: `localStorage`, key `launchpad.cycle.chart.v1`**, value `hidden` or `shown` (anything but `hidden` reads as shown).
   The LAUN-141 server prefs (`viewPrefs.ts`) are per team and issue page view and need a server field; this ticket must not
   change the server, so they do not fit (evidence: `decisions-T23-view-preferences.md` #1, the stored fields are `view`, `sort` and `at` per place, and #2, the table `user_view_preferences` has CHECKs on view and sort; a boolean needs a new column and a migration). Reads and writes are in try/catch: blocked storage means shown, and a click still
   works until the page closes. The choice is per browser, not per account.
4. **Data fetching is unchanged.** The snapshots query lives in `CyclePage`; hiding only stops the render.
5. **Tests** `burnToggle.test.tsx`: default shown, click hides svg and table, keyboard Enter / Space, remembered across remount,
   bad stored value, storage throwing on read and write, no button without data. Mutants: default hidden, `aria-expanded` fixed,
   chart still rendered when hidden, no write, no try/catch on read each fail a test.
6. **Not verified:** a real browser (button height on a phone, dark mode contrast, chevron look, screen reader output).
   Follow-up: if more view prefs appear, move this to the server prefs with the LAUN-141 pattern.

## Create issue from a board lane (LAUN-173)

1. **What it does.** Each shown board lane has a "+" button below its card list: accessible name and title `Create issue in <State>`.
   A click calls `openCreate` (the same New issue dialog the header button uses) with `state` set to the lane's state name.
2. **Context.** `team` is the page's locked team (`lockTeam`) or the Team filter; on a product page `firstTeams` is the scope
   teams, as the product page's own New issue button does. The dialog has no project or cycle default today, so the lane button
   passes none (follow-up: a `project` / `cycle` default in `CreateDefaults`).
3. **Showing it.** The button always takes its space (no layout jump) and is `opacity: 0` at rest. It shows on lane hover, on
   `:focus-within` of the lane, and on its own `:focus-visible`. Where there is no hover (`hover: none`) or the screen is 760 px
   or narrower, it is always visible and 40 px high. It is a real `<button>`: Tab, Enter and Space work, and the Enter hotkey rule
   already leaves Enter on a focused button to the button.
4. **Drag and drop.** The button sits in the `<section>` after the `ul[data-cards]`, so the drag measures (`offsetTop`, gap) are
   unchanged. `useDragScroll` already ignores presses on a `button`. While a card is dragged (`body.dragActive`) the button is
   hidden and takes no pointer events (`body.dragActive .column .colAdd` out-ranks the hover reveal; the layout test compares specificity). Card hotkeys are untouched.
5. **Hidden lanes** are not rendered, so they have no button. The list view has none (`onCreate` is a board prop).
6. **Triage.** The dialog has no Triage rule (the server accepts any state of the team on create), so the Triage lane behaves
   like any lane. If the team has no state of the lane's name (the dialog's team differs from the lane's source), the dialog now
   sends no `state` when the team's states have loaded and lack that name; the server then uses the team default. Before, the
   chip showed the default while the stale name was sent.
7. **Tests.** `boardCreate.test.tsx` (names, keyboard, state and team in the POST, hidden lane, list view, stale state) and
   `boardCreate.layout.test.ts` (CSS text: hidden at rest, hover / focus-within, touch, drag). Mutants run: wrong state, wrong
   name, no team, no focus-within, no touch rule, no drag hide, no stale-state guard: each fails a test.
8. **Not verified:** a real browser (hover, phone width, dark mode contrast, screen reader, focus ring look, a real drag next to
   the button).

## Project progress from the server (LAUN-170)

1. **One source.** The Progress column of the project table and the project page read `project.issueCounts` (server, live) through
   `ProjectProgress`. The browser series of calls (5 pages of 100 per project), the "N+" floor and the "Progress unavailable" row
   state are gone, with `useProjectProgress` and `keys.projectProgress`. Canceled issues are out of scope; sub-issues count.
2. **Fresh.** Issue events (`issue.created`, `issue.updated`), the issue mutations (`useUpdateIssue`, `useCreateIssue`) and the bulk
   move in `actions.ts` call `refreshProjectCounts` (`api/events.ts`), which invalidates the all-projects list, the per-team
   project lists and the project details. `useAllProjects` keeps its 30 s `staleTime` for everything else.
3. **Tests.** `projects.page.test.tsx` (no `/issues` call for the list, server numbers shown as sent, only-canceled = "No issues",
   page progress) and `events.test.ts` (the invalidation). Mutant run: using `total` instead of `scope` fails three tests.
4. **Not verified:** a real browser.

## Cycle options (LAUN-172)

Rules and server side: `decisions-T27-cycle-options.md`. Code: `web/src/features/cycles/CycleOptions.tsx`, `cycleRules.ts`.

1. **The menu.** A `...` button ("Cycle options") after the breadcrumbs of the cycle page opens five rows: Edit name and
   description, Change dates, Mark complete, Delete cycle, Copy link. Nothing else in v1 (no calendar, notifications,
   favorite or CSV). The first four open a dialog; Copy link copies `<origin>/<org>/team/<KEY>/cycle/<n>` and shows a toast.
2. **No row is hidden by role.** Mark complete and Delete need the admin scope (decisions-T27 #2): for a member
   (`useIsOrgAdmin` false) they are disabled with "Only admins can complete or delete a cycle."; the other rows need `write`,
   which every web session has. An agent cannot log in. A row that a rule forbids stays, dimmed (`aria-disabled`, not `disabled`, so the keyboard still reaches
   it), with a one-line reason as its description. Choosing it does nothing and keeps the menu open. `Menu` got `disabled` and
   `reason` for this. The reasons are in `cycleRules.ts` (`optionReasons`); the server judges again and its sentence shows in the
   dialog on a 400 or 409 (`cycleErrorText`).
3. **Dates are days.** The dialog edits the first and the last day in the team zone. `endsAt` is exclusive, so the last day
   goes out as the day after (`dayAfter`). Only a changed day is sent. A running cycle has its first day locked.
4. **Events.** `cycle.*` events refresh the cycle lists and the burn-up rows (`api/events.ts`). Mark complete sends an
   `Idempotency-Key`, new after a refusal. Delete goes to the cycle list on success.
5. **Tests.** `cycleOptions.page.test.tsx` (each action's request, the server message on a refusal, disabled rows with
   reasons, copy link, member and admin). Mutants run: `endsAt` not exclusive, a disabled row that still opens, a relative
   link: each fails a test.
6. **Layout (review fix).** The `...` button sits at the left of the header, so its panel opens left-aligned (an
   `align="right"` panel ran off the left edge). On a phone (480px or less) the panel spans the header 8px in from each side,
   like the cycle switcher. The panel is 300px wide so a disabled row's reason fits on one line (it ends in an ellipsis if
   longer). A disabled input is dimmed (`.input:disabled`).
7. **Last day of a cycle.** `endsAt` is exclusive only at a local midnight of the team zone. Mark complete writes a mid-day
   end, so for such a cycle the last day is the end's own local day (`lastDayMs` in `cycles.ts`, used by `dateRange`,
   `shortRange`, `cycleDays` and the burn-up). The dates dialog sends `endsAt` as the day after the last day only for a
   midnight end, else as the last day itself.
8. **Live events.** `EVENT_TYPES` (`api/events.ts`) lists every type the server emits, because an `EventSource` calls only
   listeners registered by event name. `events.test.ts` reads `server/internal/events/events.go` and fails when a type is
   missing. Before this fix `cycle.*`, `project.created`, `project.updated`, `milestone.*` and `document.*` were missing:
   a stream never delivered them (only `invalidateForEvent` cases decide what refreshes; the last five have none yet).
9. **Not verified:** the date picker UI and focus return after a dialog.

## New project dialog after Linear (LAUN-177)

1. **Layout.** `ProjectFormDialog` (create and edit share it): header "<org name> › New project" (or "Edit project") with a Close X; a large
   borderless name (an auto-growing one-line textarea, so a long name wraps on a phone; Enter moves to the description, it adds no newline;
   `maxLength` 255 as the server's 1-255 rule), directly under it the row of pills Status (with the app's `StatusIcon`), Lead, Start date,
   Target date, Teams, a thin divider, then the description (grows with its text, small minimum); the Milestones card is last; Cancel and the
   pill-shaped primary button bottom right. Not in this version: Priority, Members, Labels,
   Dependencies, Slack channel, Create with Agent, short summary (a project has no such data).
2. **Pills reuse the app parts.** Status, Lead and Teams are `ChipPicker` (Teams is `multi`); the dates are `DatePill` (`projectFormParts.tsx`),
   a chip that opens the same `Floating` panel with a native date field that the issue dialog's "More" panel uses. No second popover or date
   picker. Esc closes only the open panel. The target date field has `min` = the start date; the order rule still runs on submit.
3. **Teams stay required on create.** The Create button is `disabled` while the name is empty or no team is chosen, and a visible hint next to it
   (`aria-describedby`) says which. Cmd/Ctrl+Enter runs the submit anyway, so it shows the field error and moves focus to the missing field.
4. **Milestones (create only).** No route takes a project and its milestones together (`POST /milestones` needs `projectId`), so the dialog makes the
   project, then each milestone one by one in the order shown (`sortOrder` = row index, `targetDate` only when set). No server change. A row with a
   date and no name is refused before anything is sent; a blank row is ignored. Enter in a milestone name adds a row (it does not send the form).
5. **A failed milestone never loses the project.** The project is kept (`made`), the dialog stays open, an alert names the failed milestones, and
   each failed row says so. Made rows are locked and show "Created". The primary button becomes "Create milestones" and sends only the rows not
   made (the project is not made twice). Cancel, X or Esc now open the project without asking to discard. The milestone POST has no
   Idempotency-Key (the route takes none in the web client), so a retry after a lost reply could make a duplicate row.
6. **Closing.** Esc, X, scrim click and Cancel ask "Discard this project?" (inline strip, focus on Keep editing, Esc goes back to editing) when
   the create form has a name, description, any pill value or any milestone text. Edit closes at once, as before.
7. **Phone.** At 760 px or narrower the dialog is full-screen (`.dlg`); buttons, pills and date fields are 40 px high (the existing `.touch` rule).
8. **Tests.** `projectFormNew.test.tsx` (pills and request bodies, Cmd+Enter, discard, milestone order, failure and retry, no card on edit) and the
   updated `projectForm.test.tsx`. Mutants run: retry resends made milestones, no discard check, wrong `sortOrder`, Create not disabled: each fails a test.
9. **Not verified:** a real server. Looked at with a mocked API in Brave (light, dark, phone); no screen reader.
10. **A save in flight (fix batch).** `busyRef` is set at once, so a second Create or Cmd+Enter starts no second save. Esc, X, Cancel and a
    scrim click are ignored while saving (the button says "Saving..."). An `alive` ref stops every late callback and state write after the dialog is
    gone, so `onSaved` and `onClose` run once and never after a close. Milestone fields are `readOnly` (not `disabled`) while saving: focus stays.
11. **Focus after a save.** The disabled Create button (and the inert top block after a partial failure) drops focus to the page, and then Esc and Tab
    stop working. When a save ends, focus goes to the first failed milestone name, else to the primary button.
12. **Enter in a milestone date** adds a row, like Enter in a name. Milestone names have `maxLength` 255 (the server uses `validateProjectName` for both).
    The failure alert uses `overflow-wrap: anywhere`.
13. **Phone.** The footer is pinned to the bottom of the full-screen dialog; the hint is above, Cancel and Create share one row.
14. **Mutants (fix batch):** close not ignored while busy, no focus move after failure, milestone-only form not dirty, second submit not guarded, no
    Enter handler on the date, no `inert`, Esc on the discard strip closing: each fails a test.
15. **Final batch.** On phone the BODY (3rd child of the dialog node) takes the free height, so the footer stays at the bottom while a picker panel is open
    (a panel is appended after the footer, so `:last-child` was wrong). While a save runs the name, the description, the pills and their panels are
    read-only or ignored (what is shown equals what was sent); X and Cancel are `aria-disabled`. Enter ignores `keyCode 229` (Safari IME) in the name and
    in a milestone row. The name has `aria-multiline="false"`. Edit puts the caret at the end of the name.
16. **`maxLength` 255 counts UTF-16 units, the server counts runes.** A name with emoji can stop a little short of 255 runes in the browser. That is
    acceptable: the browser is never looser than the server, so a name it allows is never refused for length.
17. **Tests:** `projectFormAlive.test.tsx` (StrictMode, unmount during the project create and during a milestone, IME Enter, read-only while saving).
    Mutants: no `alive.current = true` (StrictMode test fails), no `alive` guards (both unmount tests fail), no `readOnly` and no open guard (the saving
    test fails), no `keyCode 229` check in either handler (the IME test fails). The `unlessPending` wrapper is a second line behind the open guard and
    `readOnly`, so removing it alone changes no visible behaviour.

## Nothing on the issue page is wider than its column (LAUN-230)

1. **Root cause.** A `display: grid` with no column template has one implicit `auto` column. That column grows to the
   min-content width of a child that keeps the default `min-width: auto`. On the issue page `.sub` (sub-issues),
   `.props` and `.propSection` (relations), `.card` (a comment), `.descEdit`, `.crBlock`, `.crList`, `.crBody` and
   `.attArea` were such grids, and so were `.thread`, `.comment` and `.compose` in `issues.module.css`.
   A no-wrap sub-issue or relation title, a long code line or a wide table made the column as wide as the content. The
   `ellipsis` on `.relLink` never took effect, and the row bled past the card. On a phone the overflow in Properties
   made the whole `.detail` scroller scroll sideways, so the description looked cut on the left.
2. **Fix.** Each of those grids names its column `grid-template-columns: minmax(0, 1fr)`. The column is then the width
   of the container, and the children shrink, wrap or truncate as their own rules say. No design token changed. The
   unused `.activity` class was removed.
3. **Shared markdown (`.md` in `ui.module.css`).** It keeps `overflow-wrap: anywhere` and adds `min-width: 0` and
   `max-width: 100%`. `pre` and `table` (a block) have `max-width: 100%` and `overflow-x: auto`, so they scroll inside
   themselves; `img` has `max-width: 100%`. No `.md` rule uses `overflow: hidden` or `clip`, so focus rings stay.
   Tables do not render today: `lib/markdown.tsx` has no `remark-gfm`, so the `.md table` rules are ready but untested.
4. **Label pills.** In the Properties label row (`.row` in `labelPill.module.css`) each `li` is a flex item that kept
   `min-width: auto`, so the pill's no-wrap name set its minimum and `.pill { max-width: 100% }` and the ellipsis on
   `.name` never applied: a long label ran past the 280 px column and, on a phone, scrolled the page. Now
   `.row > li { min-width: 0; max-width: 100% }`; the full name stays in the `title` of `.name`. The project page
   already had the same rule on `.labelRow li` (LAUN-194). The comment head's author name (`.commentHead strong`)
   gets `min-width: 0` and `overflow-wrap: anywhere` for a long one-word name in a deep reply.
   The Properties picker buttons (`.ghostBtn`: assignee, project, cycle, milestone) get `overflow-wrap: anywhere`, so
   a long one-word name breaks inside the 280 px column instead of pushing the chevron out of it.
5. **Where it applies.** Markdown renders only in the issue description and in comments (`Markdown` in
   `lib/markdown.tsx`). Project descriptions, project update bodies and the change-request lines are plain text and
   already had `overflow-wrap: anywhere`.
6. **Tests.** jsdom has no layout. `overflow.layout.test.ts` reads the CSS as text: every grid in `detail.module.css`
   names its columns (except `.playBadge`, an icon box), the listed grids use `minmax(0, 1fr)`, and the `.md` rules
   hold, plus the label row, `.ghostBtn` and `.commentHead strong` rules. `LabelPill.test.tsx` checks that the full
   name stays in the `title`. Layout was measured once by hand in headless Chromium (Brave) on
   a harness page built from the real sheets (long URL, long word, long code line, wide table, 3000 px image, deep
   list, long sub-issue and relation titles): at 1280 px and 375 px the old CSS overflowed (scroll width 3253 px and
   3185 px), the new CSS did not. A second harness loads the built app with a stubbed API (long labels, a long
   one-word assignee and author, long titles, replies six deep): at 1440, 1280, 375 and 320 px the `.detail` scroll
   width equals its client width and no element leaves the main or Properties column. Neither harness is in the repo.

## Long labels stay in an issue row and a board card (LAUN-233)

1. **Root cause.** The same as LAUN-230. In the list row (`.row` in `issues.module.css`) the labels column was `auto`,
   and `.labels` is a no-wrap flex row whose pills kept `min-width: auto`. Three labels of 100 unbroken characters
   made the column about 2000 px wide: on develop the title was 0 px wide at every desktop width (761 px and up), the
   assignee and the date left the row, and the list scrolled sideways. Short labels did the same between 761 and
   about 1000 px (the title went to 0 px at 800 px). On the board, `.cards` and `.card` were grids with no column
   template, so a long pill made the card and its column about 700 px wide instead of 272 px.
2. **List row (desktop, 761 px and up).** The template is `... minmax(120px, 1fr) minmax(0, max-content) 28px 40px`.
   The labels column is as wide as its content (as `auto` was), but only with the space left after the title's
   120 px. The grid item is `.labelsCell`: the clipped box `.labels` (project chip and at most three pills) and,
   outside the clip, the "+N" count. `.labels` is one no-wrap line, `overflow-x: clip`, `max-width: max(400px,
   30vw)` (so a wide screen shows more), `box-sizing: content-box`. Nothing clips it vertically, so a large minimum
   font size cuts no text. No item shrinks (`flex: none`): a short pill keeps its width, and a pill is capped at
   160 px with an ellipsis (the full name in its `title`). The chip keeps its own `max-width: 160px`. A 4px side
   padding, taken back by a -4px margin so nothing moves, keeps the chip's focus ring (2px outline, 2px offset)
   inside the clip; `clip`, not `hidden`, so a focused item cannot scroll the box. `.labels:empty` has no padding.
3. **The count.** `useClippedPills` (`labelOverflow.ts`) finds the pills that the clip cuts. A cut pill gets
   `data-clipped`; the CSS makes it `opacity: 0`, so a screen reader still reads it. "+N" = the labels past the
   third, plus the cut pills. So for every row, the pills that show plus N equal the number of labels. "+N" is
   outside the clip and `flex: none`, so it never hides; its `title` names the hidden labels. `.labelsCell` has a
   `title` with the project and every label (`Project: …` and `Labels: …`) and sits above the stretched title link
   (`z-index: 1`), so the hover text shows; a click on it still opens the issue (the row forwards it). Without
   ResizeObserver or layout (a test) nothing counts as cut, and N is the labels past the third, as before. When
   pills are hidden, "+N" stays at the right end of the box, so a gap can show between the last pill and the count.
   - **When it measures.** ONE `ResizeObserver` for the whole page (module state) watches every labels box AND every
     child of it (the chip and the pills): a pill can change width while the box keeps its width (a rename, a late
     web font, user CSS). A `MutationObserver` per box (`childList`, `subtree`, `characterData`) catches a child
     that comes or goes (the project chip mounts when the project list arrives after the issues, and pushes the
     pills right without any size change) and text changes; it observes the new children and measures the box in
     the next animation frame. `document.fonts` `loadingdone` measures every box again. The first notification of
     a new box comes after layout and before paint, so a row never paints with a cut pill.
   - **How it measures.** One pass per notification for all affected boxes: first it READS every box and pill
     rectangle, then it WRITES, and only what changed (`data-clipped` where it differs, the count where it differs).
     So a long list forces one layout per pass, not one per row. Hiding a pill changes only its opacity, never a
     size, so the observer does not loop (no "ResizeObserver loop" errors in any run).
4. **Trade-offs.** Between 761 and about 960 px the title keeps 120 px, and pills that do not fit are hidden and
   counted (on develop the title went to 0 px, or the row overflowed). There, short-label rows are not the same as
   on develop. When the box is narrower than the project chip (a long project name at about 800 px), the clip cuts
   the chip without an ellipsis; the cell's hover text names the project. A percentage max-width on the chip would
   give it an ellipsis, but in this content-sized column a percentage counts as none for the chip's max-content
   size, so a long project name made the column as wide as the cap (measured: the title lost 50 px at 1440 px).
5. **List row (phone, 760 px or less).** As before: `.labelsCell` in the `l` area, `.labels` one line with
   `overflow: hidden`, the project chip gives way first (`flex: 0 1 auto`), a pill longer than the line ends in an
   ellipsis (`max-width: 100%`). New: a pill that the clip cuts is hidden and counted in "+N", so a short-label row
   whose last pill was cut (for example "needs design" at 375 px) now shows "+1" in its place. We kept the old phone
   rule because a proportional shrink made short labels unreadable ("b", "u").
6. **Board card.** `.cards` and `.card` have `grid-template-columns: minmax(0, 1fr)`. `.cardMeta` has `min-width: 0`,
   its children `min-width: 0`, every child but the project chip `max-width: 100%`, and the chip
   `max-width: min(160px, 100%)`. The priority icon keeps its size (`.cardMeta > svg { flex: none }`).
7. **Cascade lesson.** The first fix (c2b85e4f) had `.cardMeta > * { min-width: 0; max-width: 100% }`. That rule has the
   same specificity as `.projectChip { max-width: 160px }` (0,1,0: `*` adds nothing) and comes later in the sheet,
   so it replaced the chip's cap and a long project name took the
   whole card line and pushed the pills to the next line. A `> *` rule must not set a property that a child class
   sets on purpose; name the children (`:not(.projectChip)`) or restate the cap. The text test now fails if any
   `> *` rule in `issues.module.css` or `detail.module.css` sets `max-width`.
8. **Picker buttons.** `.ghostBtn > svg { flex: none }` in `detail.module.css`: the icon and the chevron kept 4 to
   9 px of their 16 px beside a long one-word name; now they keep 16 px.
9. **Other users.** `labelPill.module.css` did not change, so the issue page, the project list and the project page
   are not affected. The settings pages use only `LabelDot`.
10. **Tests.** `overflow.layout.test.ts` pins each rule as text, including that `.labels` has no vertical clip, no
    wrap and no rule for `.labels > .projectChip`. It also checks the likely cascade overrides: only `.labels`
    (`overflow-x: clip`) and the phone `.row .labels` (`overflow: hidden`) set the box's overflow, only
    `.labels > [data-clipped]` touches a cut pill (`opacity: 0`), and no rule hides `.more`. A text pin cannot see
    every override (a later rule with another selector can still win); that stays a limit. `labelOverflow.test.tsx`
    tests the pure functions, renders a row with five labels and a project ("+2" and both hover texts), and tests
    the DOM path of the hook with a fake ResizeObserver and fake rectangles: the box and every child are observed,
    `data-clipped` is set and removed, the slack is half a pixel, nothing is written when nothing changed, a chip
    that mounts later and a renamed label are measured again (through the mutation observer), and unmount stops
    the observing. Review 5 added: unmount disconnects the mutation observer and forgets the box (a `loadingdone`
    after unmount reads nothing), a child that leaves the box is no longer observed, and a `loadingdone` measures a
    mounted box again. The fonts listener follows `document.fonts` if a test replaces it. Of 41 single changes (a CSS rule removed or overridden, or a line of the hook changed), 40 fail
    one of the two files. The one that passes removes the "count changed" check before the state update; React
    skips a state update to the same value anyway, so the tests cannot see it.
11. **Measured.** By hand in headless Brave (Chromium) on the built app with a stubbed API, team list and board, for
    develop (501059e9), 91a71fae and this fix. The 16 issues include three labels of 100 unbroken characters, a
    30-emoji label beside short ones, Arabic and Hebrew labels, ten labels, five medium labels with a 120-character
    project, ten labels with that project, a 200-character title, a long assignee name, no labels, and short labels
    with and without the long project. Widths: 1920, 1440, 1280, 1024, 960, 900, 800, 761, 760, 375 and 320 px, and
    640 px at device scale 2 (the CSS size of a 1280 px window at 200% zoom; not a real browser zoom). The checks:
    no element that scrolls sideways (the document included); no visible part of an element (after the clip of its
    ancestors) outside its row or card; no visible part of the labels cell over the assignee or the date; the title
    not over the labels; every board column 272 px (on a phone `min(84vw, 320px)`); no project chip wider than
    160 px; for every row, the fully visible pills plus the shown N equal the number of labels, a pill with
    `data-clipped` has opacity 0, "+N" is never cut, and the cell has the hover text. With the fix every check
    passes at every width. The title is at least 120 px (765 px at 1920, 429 px at 1440, 301 px at 1280).
    91a71fae failed the count check in 15 to 54 places per width (labels hidden with no count). Short-label rows
    (a separate run with only those issues) have the same position and size for every element as on develop at
    1920, 1440 and 1280 px, at 1024 px except one row (a 120-character project and three short labels: develop gave
    its title 82 px; the fix keeps 120 px, hides "needs design" and shows "+1"), at 760 px and at the 200% case. At 375 and 320 px they differ only where the clip cut the
    last pill: it is now hidden and "+1" shows. Every board card is the same as on develop at every width. With a
    browser minimum font size of 24 px (profile preference) at 1440 and 800 px the labels text is not cut. The list
    then scrolls sideways by 33 px, from the date column ("last mo." overflows its 40px column); develop does the
    same with short labels only, so it is not from this change. Drag and drop, the select mode, dark mode, real
    zoom, other browsers and real devices were not measured. The harness is not in the repo.
12. **Review 4 cases (measured).** The review harness, at 1920, 1440, 1280, 1024, 961, 960, 900, 800, 761, 760, 375
    and 320 px, checks the count rule above on every row. With the first version of the hook (one observer per box,
    measured again only when the box resized or the label ids changed) it failed when the project list arrived
    after the issues (150 ms and 1500 ms late: 2 to 19 rows wrong at every width from 1440 down), when a letter
    spacing was added to the pills at a fixed box width (a stand-in for a late web font, at 1024, 961, 960, 800, 760
    and 375 px) and after a live rename (961, 960, 375, 320 px). With the shared observer every case passes at every
    width, and so do a resize sequence (1920 down to 761 and back, and a phone rotation) and the base case; no
    "ResizeObserver loop" error and no page error.
13. **Performance (measured).** Drag-resize of the window in 21 steps of 5 px, the list not virtualised, rows from
    the stub API, headless Brave on a shared and busy machine. Layout time (Chrome metrics), develop / first hook /
    shared observer:
    - 100 rows, 1000 to 900 px: 19 / 54 / 45 ms.
    - 300 rows, 1000 to 900 px: 71 / not run / 95 ms.
    - 1000 rows, 1000 to 900 px: 264 to 310 / 5717 / 317 to 358 ms (two to three runs). Observer callbacks: 12997 with
      the first hook, 21 with the shared one (one per step), 37 ms of script in them.
    So the layout cost is within about 1.2 times develop. Wall time was noisy on this machine (develop itself took
    746 to 1838 ms for some runs); the shared observer took 1012 to 1679 ms for 1000 rows. A test that swaps the global ResizeObserver class gets a
    new shared observer, and a fake without `unobserve` is accepted. Scrolling, virtualised
    lists and slow devices were not measured.

## Undo and redo on the board and the list (LAUN-236, LAUN-237)

Command+Z (Apple) or Ctrl+Z undoes the last board or list change in this tab; Shift redoes (Ctrl+Y too, not on a Mac). The
stack (`undoStack.ts`, last 20 actions, one per page and view) is pure; ONE hook, `useIssueUndo.tsx`, serves both views: it
owns the keys, the writes and the toasts, and records through a `WriteRecorder` given to `useIssueActions`. Undo of a create
cancels the issue. The full rules: `decisions-T43-board-undo.md`.

## Change request links: repository and host (LAUN-239)

Code: `ChangeRequests.tsx`, `changeText.ts` (`repoWebUrl`). Server part: decisions-T17, "LAUN-239".

1. **Repository link.** The repository label links to the repository page on the host, in a new tab
   (`target="_blank"`, `rel="noopener noreferrer"`). The URL is cut from the request's own web URL (`url`), not
   built from the connection's base URL: members cannot read the connection, the request URL already has the
   right scheme, host and any path prefix (a self-managed GitLab under `/gitlab`), and it covers Bitbucket Data
   Center, whose repository page is `/projects/KEY/repos/slug` while the label is `KEY/slug`. The tail per host:
   GitLab `/-/merge_requests/<n>`, GitHub (cloud and Enterprise) `/pull/<n>`, Bitbucket (Cloud and Data Center)
   `/pull-requests/<n>`, with `<n>` the request number. **No link (plain text)** unless: the URL parses, the
   scheme is `https:` or `http:` (the same rule as the title link, so a self-hosted http host still works), it
   has no user info, it ends in that tail, and the path before the tail ends in the label shown
   (case-insensitive; Data Center, Bitbucket only: `projects/KEY/repos/slug`). The host is the request URL's host,
   the same as the title link (the host is not compared with the connection's host: see LAUN-243).
   `javascript:`, `data:`, `ftp:` and the rest are never links. Bitbucket Data Center personal repositories
   (`/users/<u>/repos/<r>`, label `~USER/r`) and GitLab URLs without `/-/` give plain text.
2. **Host link.** For an org admin the host name ("GitLab", "GitHub", "Bitbucket") is a router link to
   `/<org>/settings/integrations/<integrationId>` (the existing connection page; no anchor or focus support
   was needed). Settings > Integrations is `access: 'admin'`, so a member, and a platform admin who is only a
   member of the org, get the host name as plain text: no link that ends at "You need admin access".
3. **Look.** Both are plain anchors with no class inside the `.crText` run: the global link colour, the
   underline on hover (LAUN-60) and the global focus ring. No CSS changed. A long one-word repository wraps
   with the run (`overflow-wrap: anywhere`, LAUN-230), so nothing moves and nothing is cut. The title link is
   unchanged.
4. **Names.** `aria-label` keeps the visible text first: "GitLab integration settings", "Repository
   acme/team-docs on GitLab, opens in a new tab".
5. **Tests.** `changes.test.tsx`: each host kind (links and targets), the click to the connection page, a
   member, hostile and mismatched URLs (`javascript:`, `data:`, `ftp:`, user info, another repository, another
   number), a long repository, and `repoWebUrl` cases (subgroups, a path prefix, http, Enterprise, Data
   Center).
