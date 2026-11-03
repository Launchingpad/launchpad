# Decisions T36: Chat services (LAUN-204)

The project owner asked for the "Slack" settings section to get a different title, with
services in it: webhooks for Slack, Fluxer and "any other services, i.e.
Discord". The defaults below were chosen by the orchestrator; the project owner was not
asked (they are on the ticket). Change any of them with a new record. This
extends `decisions-T33-slack-channel.md` (LAUN-198), which stays the record for
the post content, the sender and the secret rules, and borrows from
`decisions-T35-mobile-push.md` (the save-time host refusals).

## The shape

1. **"Chat services".** The settings section is `Chat services`
   (`/settings/chat-services`, admin). `/settings/slack` redirects to it. The
   sidebar, the settings search (Slack, Discord, Fluxer, webhook are keywords)
   and the page title say the new name.
2. **Several services per org, at most 10.** Each has a name (1 to 40
   characters, unique in the org ignoring case), a kind, a webhook URL (sealed,
   write-only), an event list (the six kinds of T33 #2, default none), an on/off
   switch, and a status (`lastOkAt`, `lastError` as a fixed word, the count the
   rate cap dropped). The cap is checked inside the create transaction under a
   lock on the org row (`FOR NO KEY UPDATE`), so concurrent creates cannot pass
   it. Over the cap is `409 integration_limit`; a taken name is `409 conflict`.
3. **Kinds.** `slack`, `discord`, `custom`. The kind of a service never changes
   (the URL is checked against it). Each service is one webhook to one channel:
   no per-person addressing, as T33 #1.

## Kinds and hosts

4. **`slack`** is unchanged: host exactly `hooks.slack.com`, path
   `/services/T/B/token`, at most 256 bytes. **`discord`**: `https`, host exactly
   `discord.com` or `discordapp.com`, no port, no user info, no query or
   fragment, path `/api/webhooks/<digits>/<token>`, at most 256 bytes. Discord's
   token is in the path, so the whole URL is the secret. Discord documents
   `POST /webhooks/{id}/{token}` with a JSON body `content` (up to 2000
   characters), `allowed_mentions` and `flags`, and answers `204 No Content`
   unless `?wait=true` (source: the Discord developer documentation,
   `docs/resources/webhook` "Execute Webhook" and `topics/rate-limits`; read on
   2026-10-31, the page is not in this repository). A 429 carries `Retry-After`
   (whole seconds in their examples; the parser also takes a decimal). **A 2xx is
   success for every kind** (Slack answers 200, Discord 204).
5. **`custom`** is any public `https` host the admin types. Save-time checks:
   `https` only, a host, no user info, no fragment, no encoded path, at most 1024
   bytes, then `CheckRelayHostLiteral` of the push relay (a private, loopback or
   link-local IP literal, an old numeric spelling such as `127.1` or
   `2130706433`, a local or internal name: `localhost`, a single label, or a name
   that ends in `.localhost`, `.internal`, `.local`, `.lan` or `.home.arpa`). **No second list**: the same helper,
   `netguard.LegacyIPv4` and `netguard.IsLocalName`. A query is allowed (some
   services keep a token there); the whole URL is secret. The control that
   matters is the dial guard: the same `netguard.Dialer` and client for all three
   kinds (public addresses only, checked on the resolved address at dial time,
   no redirect, no proxy, the app's own subnets refused). A name that resolves to
   a private address passes the save and is refused when it posts
   (`chat_blocked`). A custom host on the admin's own network cannot be used,
   by design. No shared secret header for `custom` in v1 (rejected: scope).
6. **Fluxer is a kind (LAUN-206, 2026-10-31).** Verified: a Fluxer webhook takes
   the Discord body (GET 200, POST `{"content": ...}` 204; the custom body was
   refused). Kind `fluxer` posts the Discord body through the one Discord
   builder. Fluxer is self-hosted, so the host is free: the rules of `custom`
   (`ParseCustomWebhook`, then `netguard` public-only at dial, no redirect, no
   proxy; a `.lan` or private host is refused at save and at dial), plus the
   path must be `/api/webhooks/<id>/<token>` or `/api/v1/webhooks/<id>/<token>`
   and there is no query. The fixed hosts of `discord` do not apply. Sealed
   like every kind (scope per service). The kind never changes after creation.
   Migration 0035 widens the kind CHECK; its down turns Fluxer services into
   `custom`. Existing `custom` services keep their body. The add form lists
   Fluxer next to Discord with its own label.

## What a post holds (unchanged rule, T33 #13)

7. A fixed sentence per event kind, the issue identifier and a link built from
   `PUBLIC_BASE_URL`. No title, no name, no ticket text, no user names. The
   bodies: Slack `{"text": "[ID] Sentence: <link|open in Launchpad>",
   "unfurl_links": false, "unfurl_media": false}`; Discord `{"content": "[ID]
   Sentence: <link>", "allowed_mentions": {"parse": []}}` (angle brackets stop an
   embed; the empty parse list lets no mention ping anyone; markdown and `@` in
   the identifier are defused); Fluxer: the Discord body, byte for byte; custom `{"text": "[ID] Sentence: link", "kind":
   "<event kind or test>", "id": "<identifier>", "link": "<link>"}`. The test
   message is the fixed sentence "Launchpad test message. This channel is
   connected." with kind `test`. Personal settings do not apply. (Who triggers a
   post changed in LAUN-208: see `decisions-T37-chat-every-event.md`; the old
   "agents get nothing" rule of T33 #3 and #17 no longer applies to posts.)

## Data (migration 0034) and the secret

8. **`chat_services`** (`org_id` FK, composite `(org_id, id)` unique for the
   outbox FK, CHECKs for kind, name length, events, error words, blob size; a
   unique index on `(org_id, lower(name))`). **`chat_outbox`** replaces
   `slack_outbox` with a `service_id` (composite FK `(org_id, service_id)`, ON
   DELETE CASCADE); the dedupe key is unique per service (`chat:evt:<seq>:<kind>`,
   a project reminder `chat:project-due:<project>:<unix>`), so a replay makes no
   second post to one service and each service gets its own row. The fixed error
   words are renamed `chat_*` (`chat_rejected`, `chat_gone`, `chat_rate_limited`,
   `chat_unavailable`, `chat_unreachable`, `chat_blocked`, `chat_redirect`,
   `chat_unreadable`); the migration maps old stored words.
9. **Sealing keeps the key and the keyring.** The keyring is still
   `platform.SlackKeyring` (HKDF of `SETTINGS_ENCRYPTION_KEY`, info string
   `launchpad slack_channels sealing v1`, which must never change; the name is
   historic). The additional data is `slack_channels.webhook_enc.<scope>`. A blob
   made by LAUN-198 used the org id as the scope; SQL cannot re-seal, so the
   migration keeps it: `chat_services.seal_scope` holds the scope string, and the
   migrated row has `seal_scope = org id`. A new service is sealed under
   `svc.<org id>.<service id>`, so a blob copied to another service or org does
   not open. On every open the code checks the scope: the org-id scope is valid
   only for a `slack` service of that org, else the service must have its own
   scope. A replaced URL keeps the row's scope. Proof:
   `TestChat_MigratedSlackChannelStillUnsealsAndSends` seals a webhook under the
   old scheme at schema 0033, migrates to 0034 and then lists, tests and sends
   through the real service and sender; `TestChat_SealScopes` covers the moves.
10. **The migration.** The Slack channel of each org becomes one `slack` service
    named `Slack` with its events, enabled, and its status (words mapped). Its
    outbox rows move to it (pending rows are kept and sent; sent and dead rows
    keep their history). Rows of an org with no channel are not copied (they
    could only have been skipped). **Down** recreates `slack_channels` and
    `slack_outbox` and restores the one `slack` service per org whose blob still
    has the org scope (with its waiting posts and words mapped back; a disabled
    service returns with an empty event list). It **cannot restore, and so
    loses,** Discord and custom services, extra Slack services (their scope does
    not fit the old column) and the posts waiting for them.
    `TestMigration0034_ChatServices` and `TestMigrateUpDownUp` cover it.
11. **No secret in a log, event, error, idempotency row or response**, as T33
    #10: the poster maps every transport error to a word, validation messages
    name the rule and never the value, and `chat_service.updated` carries the
    action, the service id, the kind, `enabled` and the event kinds only (no
    name, no host, no URL). `entityType org`, the org id as the entity id.

## Routes

12. **`/orgs/{org}/chat-services`**: `GET` (list, with `available`,
    `linksAvailable`, `limit`), `POST` (create, 201); `/chat-services/{service}`:
    `GET`, `PATCH`, `DELETE` (204, idempotent); `/chat-services/{service}/test`
    `POST`. Admin scope, `refuseAgents` after it, `idempotency.Reject` on the
    secret-bearing POST and PATCH and on the test. A service of another org is
    `404 not_found` on every verb (a delete is `204` and removes nothing, as
    every idempotent delete here). The test is limited to 6 a minute, burst 3 per
    process, `429` with `Retry-After`; it works for a switched-off service; a
    failure is `502 chat_send_failed` with a fixed word (`slack_send_failed` was
    renamed). A PATCH that replaces the URL clears the status; one that switches
    the service off deletes its waiting posts.
13. **The old `/slack` routes are removed**, with their OpenAPI entries, schemas
    and the `slack.updated` emitter. They shipped in LAUN-198 (days before), no
    client exists but the web app (which is updated), and a compatibility wrapper
    would keep a second model and a second set of tests for one org-wide row.
    `slack.updated` stays in the event type list (old feed rows keep that type)
    and the web client treats it like `chat_service.updated`.

## Delivery

14. **The sender is generalised, not copied**: `ChatSender` (advisory lock
    `devops:chat-send`, boot and every 15 seconds, stuck rows back after 10
    minutes, expiry 24 hours, retries 1m, 5m, 30m, 2h, 12h then dead, fixed-word
    `last_error`, claim skipped on 0 rows affected, 30-day prune, fair
    per-org selection with at most 10 rows per org per pass and 200 in all). The
    rate cap (60 an hour) is **per service**, counted from its own sent rows, and
    the cap count shown to the admin is per service. A kind dispatches the URL
    check and the body shape; the client and dialer are shared. Permanent
    failures (`chat_gone`, `chat_rejected`, `chat_blocked`, `chat_redirect`) are
    dead at once. A service that is off or gone makes its rows `skipped`
    (`disabled`); the consumer makes no row for a service that is off. A success
    or a failure sets the status of that service only. No write path imports
    `internal/notify`.
15. **`Retry-After`** is read as a whole or a decimal number of seconds and
    bounded (one hour) before it is multiplied, so a huge value cannot overflow.

## The page

16. **Chat services** lists the services (name, kind, host, a status chip:
    Working, Not used yet, Last post failed, Off, Webhook unreadable; the fixed
    sentence of the last failure; the rate-cap count) with Edit, Send test and
    Remove (a confirm dialog, focus on Cancel). **Add service** opens a dialog:
    kind, name, URL (`type=password`, no autofill), events. **Edit** changes the
    name, the switch, the events, and the URL only when a new one is typed (the
    stored URL is never shown; a custom service that moves to another host needs
    the whole new URL). Every error is a fixed sentence. Add is off at 10 services
    and on a server without a key. The page follows `chat_service.updated`.
    **Focus**: a dialog puts focus back on its opener; the page also moves focus
    when the opener is gone or never took it (after a remove: the next row's Edit
    button, else Add service; never `<body>`). Two clicks in one tick send one
    request (synchronous refs). One column and 40 px targets on a phone.
17. **Honest copy.** For `custom` the page says that Launchpad refuses private
    addresses, localhost, single-label names and `.internal`, `.local`, `.lan`,
    `.home.arpa` names and follows no redirect; Fluxer says the same, and that
    it takes the same post as Discord.

## Honest limits

- Verified only against fake servers (an `httptest` server through the real dial
  guard; the web page against a mocked API in a headless Brave). No real Slack,
  Discord or Fluxer delivery from tests (the project owner confirms a real Fluxer post); Discord's JSON follows the documentation above.
- A custom service must be reachable from the server on a public address.
- No per-team or per-event routing, no threads, no mentions, no OAuth app.
- The keyring type and its info string keep the name `Slack` (renaming would
  change the derived key). It seals every chat service.
- Team restrictions (T33, honest limits) still apply to every service.

## Follow-up after review (LAUN-204)

- **Sender tarpit.** A service whose post ends in `chat_unreachable` (a timeout
  or a dead host) gets no more posts in that pass: its other claimed rows go back
  to pending with no attempt used (`ReleaseChatOutbox`). One pass also has a
  wall-clock budget (`ChatPassBudget`, 60 s); past it every claimed row that is
  left is released the same way. Selection stays the fair round-robin of the
  claim, so the next pass starts as before. The claim ranks rows per service
  first (`ListDueChatOutbox`: the first row of every service of an org before the
  second row of any), so a dead service with many due rows cannot fill the org's
  ten slots and starve its healthy services. A 429 or any Retry-After trips the
  service for the pass the same way. Limit: ten or more dead services in one org
  can still crowd out a healthy one for a pass; each dead service uses one
  attempt per pass and then waits out its backoff.
- **Sent count.** A post counts as sent only when the row was still in
  `sending` (a service switched off mid-flight deletes it).
- **Local names.** `netguard.IsLocalName` also refuses `.internal`, `.local`,
  `.lan` and `.home.arpa`. It is used only when a push relay or a chat webhook
  is saved (never by the git hosts or the dial path).
