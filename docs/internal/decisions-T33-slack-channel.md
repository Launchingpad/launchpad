# Decisions T33: Slack channel (LAUN-198)

> **#3 (the eligible-person rule) and the "agents get nothing" part of #17 are
> superseded for chat services by `decisions-T37-chat-every-event.md`
> (LAUN-208).** A chat service posts every ticked kind, whoever the issue
> belongs to and whoever acted; a muted issue still posts nothing.

> **Superseded in part by `decisions-T36-chat-services.md` (LAUN-204).** The one
> channel per org, the `/slack` routes, the `slack_channels` and `slack_outbox`
> tables, `slack.updated`, the `slack_*` error words and the `/settings/slack`
> page are gone: an org now has several chat services (Slack, Discord, custom).
> Still in force from this record: the event list and personal settings (#2, #3),
> what a post holds (#13, #14), the delivery rules (#15 to #17), the secret rules
> (#8 to #11) and the honest limits. Read T36 first for names and routes.

An org admin connects one Slack Incoming Webhook. The org then posts chosen
kinds of events to that one channel. This is the Slack item of the Linear
settings match (LAUN-108: Notifications, Push channels, Slack). The defaults
below were chosen by the orchestrator; the project owner was not asked. Change any of them
with a new record. Related: `decisions-T13-notifications.md` (the consumer and
the outbox this copies), `decisions-T9-mail-settings.md` (sealed, write-only
secrets), `decisions-T16-backups.md` and `decisions-T17-git-connections-import.md`
(the dial guard).

## The shape, and why it is not a per-user channel

1. **One webhook per org, one event list per org.** A Slack Incoming Webhook
   posts to one channel that the whole channel can read. It cannot address a
   person. A per-user "send me Slack" switch would either leak one person's
   private notification to everyone in the channel, or force Launchpad to ask
   Slack for user ids (an OAuth app, a token, a bot scope), which v1 does not
   do. So the channel is an **org setting**, set by an admin (admin scope, an
   org admin; platform admin is not needed), next to the other org
   administration. Rejected: a per-user Slack opt-in on the Notifications page
   (privacy: a shared channel cannot honour it); a team-level channel (the
   webhook is org-wide in Slack's model, and the other channels have no team
   admin level either).
2. **The admin picks the event kinds.** `events` is a subset of the six
   notification kinds (`assigned`, `mention`, `comment`, `state_change`,
   `new_issue`, `project_update_due`). The default is none: a new webhook posts
   nothing until the admin ticks a kind. The list is stored as `text[]` with a
   CHECK that every value is a known kind.
3. **Personal settings do not apply.** A person's own email and inbox switches,
   unsubscribe, follows and excerpt choice do not change what the channel gets.
   The Settings page says so. Two rules do apply, because they are not
   preferences: a post goes out only if at least one **eligible person** (an
   active member, not an agent, not the actor) would be notified of the event,
   so an event that concerns only an agent, or only the person who did it,
   posts nothing ("agents get nothing", decisions-T13 #14); and an issue-level
   **mute** still wins (a muted issue is quiet for everyone it was muted for),
   except a direct mention.

## Data (migration 0031)

4. **`slack_channels`**: one row per org (`org_id` is the primary key and a
   foreign key to `orgs`, ON DELETE CASCADE). `webhook_enc bytea` is the sealed
   URL, `events text[]`, `last_ok_at`, `last_error` (a fixed word),
   `updated_by` (SET NULL), timestamps. No tenant is reachable without its
   `org_id`: every query filters by it.
5. **`slack_outbox`**: one row per post. `org_id`, `kind`, exactly one of
   `issue_id` / `project_id` (composite FKs `(org_id, x_id)`, ON DELETE
   CASCADE), `dedupe_key`, `event_seq`, `state` (`pending`, `sending`, `sent`,
   `skipped`, `dead`), `skip_reason`, `attempts`, `next_attempt_at`,
   `last_error` (a fixed word, CHECKed), `sent_at`. `UNIQUE (org_id,
   dedupe_key)`. It holds **ids and a kind only**: no title, no name, no text,
   no URL. There is no recipient column: a post is not addressed.
6. **One post per event and kind, not per person.** The consumer (inside the
   same transaction that makes the notifications, after every read) inserts one
   outbox row per distinct kind an event produced for an eligible person, if
   the org has a channel and the kind is on its list. The dedupe key is
   `slack:evt:<seq>:<kind>` (a project reminder: `slack:project-due:<project>:<due
   unix time>`), so a replay of the feed makes no second post. A comment on an
   issue with five watchers is one post. `ON CONFLICT DO NOTHING`.
   Limit: an edit of a comment that still contains an @mention can post
   "Mention on an issue" again, because the "already notified" check reads
   `notifications`, and a person who turned every channel off has no such row.
7. **Down.** The down drops both tables and so loses the sealed webhook and the
   waiting posts. `TestMigrateUpDownUp` covers it.

## The secret

8. **Sealed, write-only.** The URL is the whole credential (anyone who has it
   can post). It is sealed with AES-256-GCM through a **third keyring scope**,
   `platform.SlackKeyring`: HKDF-SHA256 of `SETTINGS_ENCRYPTION_KEY` with its own
   info string (`launchpad slack_channels sealing v1`). The additional data is
   `slack_channels.webhook_enc.<org id>`, so a blob moved to another org or
   column does not open, and a platform-settings or git blob does not open as a
   Slack blob. It is not the backup keyring and `BackupKeyring` is not used.
   The key is optional on a server: without it a `PUT` with a URL is `409
   encryption_unavailable`, `GET` says `available: false`, and the page says why.
   A URL that does not open (the key changed) reads as `secretUnreadable: true`;
   the sender skips its rows (`unreadable`) and the admin enters it again.
9. **Never read back.** `GET` says `configured`, the host name
   (`hooks.slack.com`, a constant) and the event list. It never returns the URL,
   a part of it, a prefix, a length or a hash. The page shows "Set" and the
   host. `PUT` takes `webhookUrl` (absent or null keeps the stored one; a value
   replaces it) and the whole `events` list. `DELETE` removes the row and the
   waiting posts. `PUT` uses `idempotency.Reject`: the body is never hashed or
   stored. `Idempotency-Key` is `400`.
10. **Never logged, emitted or stored in clear.** A failed post becomes a fixed
    word (`slack_rejected`, `slack_gone`, `slack_rate_limited`,
    `slack_unavailable`, `slack_unreachable`, `slack_blocked`,
    `slack_redirect`, `slack_unreadable`). Go's `*url.Error` holds the URL, so
    the poster **never returns or logs the transport error**: it maps it to a
    word and drops it. Validation messages name the rule, never the value. The
    change event `slack.updated` (entity type `org`) carries `configured` and the
    kinds only. `TestSlack_SecretNeverStoredOrLogged` (notify) and
    `TestSlack_SecretNeverLeaks` (api) plant a token and grep the logs, events,
    idempotency rows, outbox rows, last errors, the sealed column and every
    response.
11. **Tenancy.** Cross-org is `404 not_found` (a token pinned to another org;
    a member of two orgs sees each channel only through that org's path). A blob
    is bound to its org. The sender reads the channel of the row's own org, so
    an event posts to its own org's webhook only.

## Outbound requests

12. **Two defences, both required.** (a) An exact allowlist, checked on save and
    again before every post: `https`, host exactly `hooks.slack.com`, no port, no
    user info, no query or fragment, no encoded path, and a path of
    `/services/<T>/<B>/<token>` (three plain segments), at most 256 bytes. A lookalike host, a
    port, `user@host`, an IP, a trailing dot or a different case is refused.
    (b) The dial-time guard: the client is `netguard.NewClient` (no redirect, no
    proxy, TLS 1.2 or newer, system roots) on a `netguard.Dialer` with
    `notify.SlackPolicy`: public addresses only (the zero policy, so no private
    range opens and no environment switch can), the always-refused ranges and
    metadata addresses, the service names of the git policy, and this
    container's own subnets (`netguard.NewAttached`, the same object the git
    hosts use). The check runs on the resolved address at dial time, so a DNS
    answer that changes after the save (rebinding) gains nothing. One refused
    answer refuses the host. A redirect is returned, not followed:
    `slack_redirect`. No second HTTP client or dialer exists; tests reach a fake
    Slack through the real `Dialer` with `netguardtest` (a `_test.go` import
    only, and `TestNoProductionBypass` still passes).
13. **What a post holds.** `{"text": "...", "unfurl_links": false,
    "unfurl_media": false}` and nothing else. The text is a fixed sentence per
    kind, the issue identifier and a link built from `PUBLIC_BASE_URL`:
    `[LAUN-12] Issue assigned: <https://host/org/issue/LAUN-12|open in Launchpad>`;
    a project reminder has no identifier. **No issue title, no project name, no
    comment text, no person's name or Slack mention.** This respects "notify never
    stores a message body", and a shared channel cannot honour any private
    detail. The cost is honest: a post is a pointer. Rejected: titles in the
    post (they are the most useful part, and also the most likely to hold
    something private); an `include title` switch (a later record if the project owner wants
    it; it is a send-time lookup, so no schema change); Slack user mentions
    (needs OAuth and a user map).
14. **No `PUBLIC_BASE_URL`, no post.** A post is a link. With it empty the sender
    skips every row (`no_base_url`) and the page shows a warning.

## Delivery

15. **The sender copies the mail sender.** `notify.SlackSender` runs at boot and
    every 15 seconds; a session advisory lock (`devops:slack-send`) keeps one
    instance sending; rows stuck in `sending` for 10 minutes return to `pending`;
    a row expires after 24 hours; claim and record follow the mail pattern, so
    delivery is at-least-once and a database error never loses a row. The same
    retry schedule as the mail sender (1m, 5m, 30m, 2h, 12h, then `dead` on the
    sixth failure), `Retry-After` honoured (capped at an hour). Failures that a
    retry cannot fix (`slack_gone`, `slack_rejected`, `slack_blocked`,
    `slack_redirect`) are `dead` at once. A success clears the channel's
    `last_error`; a failure sets it.
16. **Rate cap and pacing.** At most 60 sent posts per org per rolling hour
    (`rate_cap`, skipped, not retried, and counted for the admin) and at most 10 posts per org per pass, so
    a burst is spread (Slack allows about one post a second per webhook). Rows
    are skipped, with a reason, when the admin removed the channel (`disabled`),
    took the kind off the list (`not_selected`), the URL is unreadable
    (`unreadable`), or the issue or project is gone or archived (`gone`).
    Terminal rows are pruned after 30 days by the sender itself.
17. **Agents.** Every route refuses an agent token with `403 forbidden`
    (`refuseAgents`, after the scope check), as the notification routes do. An
    agent is never an eligible person, so it never causes a post by being a
    recipient.
18. **No write path imports `internal/notify`.** Only the consumer and the API
    server (as for the settings routes) do. `TestNoWritePathImportsNotify`
    still passes.

## The API

19. **Routes** (admin scope, `refuseAgents`, in `router.go` and
    `api/openapi.yaml`): `GET /orgs/{org}/slack`, `PUT /orgs/{org}/slack`,
    `DELETE /orgs/{org}/slack`, `POST /orgs/{org}/slack/test`. A change emits
    `slack.updated` as the last statement of its transaction (a delete of a
    missing channel emits nothing). The new error code is `slack_send_failed`
    (`502`, a fixed word in the message). The MCP catalog is **unchanged**:
    admin configuration with a secret is not an agent tool.
20. **The test message.** `POST /slack/test` posts one fixed sentence ("Launchpad
    test message. This channel is connected.") through the same poster. It is
    limited to 6 a minute, burst 3, per process (the mail test's limiter, `429`
    with `Retry-After`). No `Idempotency-Key` (a replay must not skip the post).
    It records `last_ok_at` / `last_error` on the channel.

## The web page

21. **Settings, Administration, Slack** (`/settings/slack`, admin). One form:
    the webhook field (`type=password`, no autofill; only on a new channel,
    after "Replace webhook", or when the stored URL is unreadable), a
    fieldset of six checkboxes with a line each, Save, a Test section and a
    Remove button that opens a confirm dialog (focus on Cancel). Fixed
    sentences for every error; no server text reaches the screen. It follows
    `slack.updated` on the event stream, and a form the person is editing is not
    overwritten. One column and 40 px targets on a phone. The Notifications
    page is not changed: it is about the person's own channels, and says nothing
    about an org channel the person cannot see.

## Honest limits

- A post carries no title, name or text. People must open the link.
- One channel per org. No per-team or per-event channel, no threads, no
  mentions, no Slack OAuth app, no slash commands.
- The channel does not follow personal settings (by design, #3).
- **Team restrictions (decisions-T20).** The channel posts the identifier and a link
  of issues of EVERY team. Once team access restriction lands
  (`decisions-T20-team-access.md`), a person in the Slack channel may learn that an
  issue of a team they cannot open exists. A team-restricted org must therefore
  exclude hidden-team issues from the channel (a per-team switch or the
  consumer's visibility rule) or warn the admin before it turns the channel on.
  Until then the Slack page says in plain words that every team is posted.
- **Rate cap is visible, not silent.** `GET /slack` returns `rateCappedCount` and
  `lastRateCappedAt` (the posts the 60-an-hour cap dropped in the last 24 hours;
  counts and a time only) and the page shows them.
- **Fair sending.** One pass reads at most 10 due rows of each org, round-robin,
  then 200 in all, so one org's burst cannot delay another org's posts.
- **Delete drops waiting posts.** `DELETE /slack` deletes the org's `pending` and
  `sending` outbox rows in the same transaction, so a webhook set later never gets
  old posts. Terminal rows stay for the 30-day prune. A `PUT` that replaces the
  webhook keeps waiting rows (they go to the new webhook).
- Slack's reply is not parsed. A 200 is success. A webhook later disabled by a
  Slack admin shows up as `slack_gone` or `slack_rejected` after the next post.
- At-least-once: a crash between the POST and its record can post twice.
- Verified only against a fake Slack (an `httptest` server through the real
  dial guard). No real Slack workspace was used, and no real browser.
