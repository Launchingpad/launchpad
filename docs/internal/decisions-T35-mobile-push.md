# Decisions T35: Mobile push (LAUN-197)

Launchpad can wake the mobile app when a notification needs a person. The
defaults below were chosen by the builder; the project owner was not asked. Change any of
them with a new record. Related: `decisions-T22-native.md` #47 (the server holds
no APNs or FCM key; a relay does), `decisions-T13-notifications.md` (the
consumer and outbox this copies), `decisions-T33-slack-channel.md` (the sender
and poster this copies), `decisions-T9-mail-settings.md` (sealed, write-only
secrets).

## The shape

1. **A relay, not a vendor key.** The server never holds an APNs or FCM key. An
   org admin enters a relay `https` URL and a bearer secret (admin scope, page
   Settings > Mobile push). The relay is run by the publisher of the app.
2. **Wake only.** The server sends `POST <relay>/v1/wake` with
   `Authorization: Bearer <secret>` and the body
   `{"kind":"wake","id":"<outbox id>","platform":"ios|android","token":"<device token>"}`.
   No title, comment, name or issue id leaves the server. The app opens and
   fetches the data with its own sign-in. The `id` is the opaque outbox row id.
3. **A third channel, `push`.** Notification settings gain `push` next to
   `email` and `inbox`. Default off, per event, with the usual team override.
   The web Push column shows only when the relay is configured. A push-only
   notification is stored with `inbox=false`, so it does not appear in the inbox.
   Actor and agents get nothing (same eligibility filter as email).
4. **Devices are per member, per org.** `PUT/GET/DELETE
   /orgs/{org}/me/push-devices[/{device}]`. Humans only (`refuseAgents`, 403 for
   an agent token). A person registers again in each org.

## Defaults

| Item | Value |
|---|---|
| Sealing | relay URL, relay secret and device token are sealed (AES-GCM, HKDF) with `platform.PushKeyring`; AAD `push.<column>.<scope>`; relay scope = org id, token scope = `org.device` |
| Key | `SETTINGS_ENCRYPTION_KEY`; without it a write is `409 encryption_unavailable` |
| Device token | also stored as sha256 (`token_hash`, unique per org); same token under a new id takes over: the old row is deleted. A registration takes an advisory lock per (org, token hash), so two members who register one phone at once never hit the unique key (no 500) |
| Device id | chosen by the client, so the key is `(org_id, id)`, not `id` alone: the same id in another org is a separate row, never a denial. Another member's id in the same org is `404` |
| Device cap | 10 per member per org; the 11th is `409 conflict`; a re-register of a known id is no new device |
| Retention | 90 days from `last_seen_at`; pruned on write and by the sender |
| Rate cap | 300 sends per hour per org; over the cap the row is skipped `rate_cap` (not retried) |
| Fairness | 20 rows per org per pass, 200 per pass overall |
| Retry | 1m, 5m, 30m, 2h, 12h; dead on the 6th failure; rows older than 24 h expire |
| Permanent failures | rejected, unauthorized, blocked, redirect, token_invalid: dead at once |
| 410 from relay | `push_token_invalid`: the device and its outbox rows are deleted |
| Test push | goes to the admin's own devices; 6 per minute, burst 3; `502 push_send_failed` with a fixed word |
| Events | `push.updated` (entity org, payload `{configured}`), by admin Put or Delete only. Device registration emits none (per-user private rows) |
| Consumer | outbox rows are inserted in the same transaction as the notification and the cursor advance; with no relay no row is made |
| Port | any public `https` port |

## Outbound safety

The relay host is user entered, so it cannot be allowlisted (Slack posts to one
known host; this does not). The control is the `internal/netguard` public-only
dial guard: no private, loopback or link-local address, no redirect, no proxy,
`https` only, no userinfo, query, fragment or encoded path, 512 bytes at most.
The poster is built by `notify.NewPushClient` over netguard; no second client
or dialer exists. The URL and secret never appear in a log line, an event, an
idempotency row, `last_error` or a response; `last_error` holds a fixed word
(`push_unreachable`, `push_blocked`, ...). The `GET` returns the host, the
last ok time, the last error word and counts only.

## Relay changes and the address check

- **A new host needs the secret again.** `PUT /push` with a `relayUrl` whose
  host differs from the stored host and no `relaySecret` is `400
  validation_failed`. The kept secret would otherwise go to a host the admin
  has just named. The same host (another path or port) may keep the secret. The
  web form says so and asks for the secret before it sends anything.
- **IP literals are refused at save.** A relay host that is an IP literal in a
  range the dial guard refuses (private, loopback, link-local, metadata, CGNAT,
  IPv4-mapped, ...) or an internal service name is `400`. The same save check
  refuses `localhost`, `*.localhost`, `*.internal`, `*.local`, `*.lan`,
  `*.home.arpa` (added in LAUN-204), a single-label name with no dot, and an
  old numeric IPv4 spelling (`127.1`, `2130706433`, `0x7f.1`, octal). The parser
  is `netguard.LegacyIPv4`, which the dialer also uses, so the dial path never
  hands such a spelling to a resolver. It uses
  `PushPolicy(nil).Check` and `RefusesName`, the same policy as the dial: there
  is no second range list. A host NAME that resolves to a private address
  passes the save check and is refused at dial time (`push_blocked`), because
  DNS can change after save. The page copy says both.
- **The URL field is plain text, the secret is a password field.** The URL is
  not shown again after save (only its host), but it is no password. The
  secret stays hidden.
- **The Push radios need a relay and a device.** The settings view carries
  `push.available` (relay set) and `push.hasDevice` (the caller has a device in
  this org). Without a device the page shows a note with a link to Mobile
  devices. Registering a device emits no event, so the settings page learns of
  a new device on its next load.

## Accepted behaviour

- **A relay that answers 410 to everything deletes that org's devices.** The
  relay is the admin's own choice and 410 is its word for "this token is dead",
  so the target is admin-chosen and accepted. Members register again by
  opening the app. A 410 deletes a device only while it still holds the token
  that was sent (`token_hash` must match), so a token the app refreshed while
  the wake was in flight survives; the waiting wake is skipped as `gone`. The
  same rule holds for the test push.
- **The test-push rate limiter is process-wide.** Burst 3, 6 per minute, kept
  in memory of one server process: it is not per admin and not shared between
  processes. One admin can use the burst of the others.
- **Takeover keeps the count.** A member at the device cap who registers their
  own token under a new id takes the old row over (the takeover delete runs
  before the cap check); a new token at the cap is `409`.

## Honest limits

- **No relay and no app exist yet.** Nothing here was delivered to a phone, end
  to end. Tests use a fake relay (`httptest`).
- The relay API above is our own contract. It is not a published standard.
- **Vendor silent-push rules are unverified.** APNs and FCM limit or drop
  background wakes (power state, user force-quit, budgets). The app must not
  depend on a wake for correctness; it must also fetch on open.
- **Delivery is at-least-once.** A crash after a send and before the commit
  can send twice. The wake is idempotent by design (it holds no content).
- The rate cap drops, it does not queue. The admin page shows how many were
  dropped in the last day.
- Web was tested with a mocked API in jsdom, not in a real browser.
