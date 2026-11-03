# Decisions T40: Personal webhooks (LAUN-229)

The project owner asked (2026-11-01): "Allow users to create webhook that will ping only
when a ticket they own is changed or when one is assigned to them. This webhook
is only visible to them in settings." The project owner said "use my defaults"; the
decisions below are those defaults, with a short reason each. Change any of
them with a new record. The open questions are at the end.

This record borrows from `decisions-T36-chat-services.md` (kinds, URL checks,
the poster, the sealed write-only URL), `decisions-T35-mobile-push.md` (rows
per member, humans only, no event), `decisions-T13-notifications.md` (the
consumer, the recipient filters) and `decisions-T20-team-access.md` #61 and #63
(only a member who sees the team; a re-check at send time).

## The shape

1. **A personal webhook belongs to ONE human member of ONE org.** The row holds
   `org_id` and `user_id`, with a composite FK to `org_members` (`ON DELETE
   CASCADE`: a member who leaves the org takes the webhooks along). Reason: a
   person's interest is per org, like push devices (T35 #4).
2. **Only the owner sees it.** The routes are the caller's own (`/me/webhooks`).
   An admin cannot list, read, change or remove another member's webhook; there
   is no admin route. Another member's id, or another org's id, is `404
   not_found` (a delete is `204` and removes nothing). Reason: the project owner said "only
   visible to them".
3. **Humans only.** Every route answers `403 forbidden` for an agent token
   (`refuseAgents`), and the consumer makes no row for an agent (the
   eligibility filter below). Reason: T13 #36, agents get nothing.
4. **At most 3 per member per org.** The cap is checked inside the create
   transaction under a lock on the member's `org_members` row (`FOR NO KEY
   UPDATE`), so two concurrent creates cannot pass it. Over the cap is `409
   integration_limit`. Reason: a person needs one or two; 3 bounds the posts one
   event can make.
5. **Fields.** A name (1 to 40 characters, no control characters, unique per
   member ignoring case; a taken name is `409 conflict`), a kind, a webhook URL
   (sealed, write-only), an event choice, an on/off switch and a status
   (`lastOkAt`, `lastError` as a fixed word). The shape of a chat service minus
   the team. Reason: the same page model the members know from chat services.
6. **Kinds and URL checks are those of chat services**, the same function
   (`notify.ParseChatWebhook`): `slack` (host `hooks.slack.com`), `discord`
   (`discord.com`, `discordapp.com`), `fluxer` and `custom` (any public `https`
   host; the save-time refusals of T36 #5 via `CheckRelayHostLiteral`,
   `netguard.LegacyIPv4`, `netguard.IsLocalName`). No second list. The kind
   never changes after creation. Reason: one rule set, one place to fix.

## Events

7. **Two choices, both on by default:**
   - `assigned`, "Assigned to me": an issue is created with me as assignee, or
     an update sets me as the assignee.
   - `my_issues`, "Changes on my issues": an update of an issue I own, or a new
     comment on it.
   Reason: the two cases the project owner named.
8. **"An issue I own" is: I am its assignee or its creator** (default). The
   assignee is the one the event names (the payload `assigneeId`, else the
   issue row); the creator is the issue row's. Reason: the creator is who asked
   for the work, and the assignee is who does it; both see the issue as "mine".
   A person who wants only assigned work ticks only `assigned`.
9. **What counts as a change:** `issue.updated` (any field but a board move
   alone: `changes` only `sortOrder`) and `comment.created` on the issue. Not
   counted: a comment edit or delete, a relation, an attachment,
   `issue.created` (only its assignee hears of it, via `assigned`). A cycle
   roll-over by the cycle job is an update with no actor and counts. Reason:
   the feed events the consumer already reads (T13 #12).
10. **One post per webhook per event.** The kind of the post, first match:
    `assigned` (when the event assigns me and I ticked `assigned`), then
    `state_change` (the update changed the state), `comment`, `issue_updated`
    (any other update). An update that assigns me with only `my_issues` ticked
    posts the change kind. The dedupe key is `pwh:evt:<seq>`, unique per
    webhook, so a replay makes no second post.

## Who gets a post (the consumer)

11. **The consumer decides at event time, in the same transaction as the
    notifications** (T13 #15, #16). No write path imports `internal/notify`.
12. **Filters, all of them:**
    - never the actor: my own change posts nothing to me;
    - only an active, non-agent member who SEES the issue's team: the same
      query as inbox and mail, `ListNotifyEligibleUsers` with the issue's team
      (T20 #61). A member restricted away from the team gets nothing;
    - not when I muted the issue (my own mute row; the any-member rule of T37 #4
      is for a shared channel, and this channel is mine);
    - not for an archived issue or an archived team (T13 #14);
    - only an enabled webhook whose event choice matches.
    My notification settings (email, inbox, push, team overrides) do not
    apply: the webhook's own event choice is the setting. Reason: the webhook
    is a separate channel I configure on its own page.
13. **Send-time re-check** (T20 #63). Before a post the sender checks, in this
    order, and skips the row with a fixed reason when one fails: the webhook is
    gone or off (`disabled`), the kind is no longer ticked (`not_selected`), no
    `PUBLIC_BASE_URL` (`no_base_url`), the owner is no longer an active, human
    member (`disabled`), the URL does not open (`unreadable`), the issue is gone
    or archived (`gone`), the owner no longer sees the issue's team
    (`access_removed`, with `MailRecipientSeesTeam`), the hourly cap
    (`rate_cap`). Reason: access can be removed between the event and the post;
    no identifier of a hidden issue leaves the server.

## What a post holds

14. **The chat post, with a personal sentence**: a fixed sentence, the issue
    identifier and a link. No title, no name, no comment text, no actor. The
    sentences: "Issue assigned to you", "Your issue changed state" (with the two
    state names of T39, both or neither), "New comment on your issue", "Your
    issue was updated". The bodies per kind are those of T36 #7, built by the
    one builder `ChatText`; the custom body's `kind` is the post kind. The test
    message is the chat test sentence. Reason: a webhook URL can leak or point
    at a shared channel; the same minimum as chat.

## Delivery

15. **The same poster and client as chat**: `ChatPoster` over
    `netguard.NewClient` (public addresses only at dial, no redirect, no proxy,
    the app's own subnets refused). No second HTTP client or dialer. The poster
    checks the URL against its kind again before each post.
16. **A sender of its own, with the chat rules.** `PersonalWebhookSender` runs
    at boot and every 15 seconds under its own advisory lock
    (`devops:personal-webhook-send`). The same constants as chat: a 24-hour
    expiry, retries 1m, 5m, 30m, 2h, 12h then dead, permanent failures
    (`chat_gone`, `chat_rejected`, `chat_blocked`, `chat_redirect`) dead at
    once, stuck rows back after 10 minutes, a 30-day prune, at most 10 rows per
    org per pass (round-robin per webhook first) and 200 in all, the
    unreachable tarpit and the 60-second pass budget (T36 follow-up). Reason:
    the table differs (a member's webhook, not a team's service), the policy
    does not.
17. **Rate cap: 60 posts per webhook per rolling hour** (`ChatRateCap`), counted
    from the webhook's own sent rows; over it the row is skipped `rate_cap`
    (dropped, not queued). Reason: the chat cap, per webhook.
18. **No auto-disable.** A webhook that keeps failing stays on, as a chat
    service does; its status shows the last fixed word, and each post is dead
    after its retries. Reason: chat has no auto-disable either; the member sees
    "Last post failed" on the page.

## Secrets and events

19. **Sealed with `platform.SlackKeyring`**, the chat keyring (HKDF of
    `SETTINGS_ENCRYPTION_KEY`). No new keyring. A new column name,
    `platform.PersonalColWebhook` (`personal_webhooks.webhook_enc`), and a scope
    per webhook, `pwh.<org id>.<user id>.<webhook id>`, so the additional data
    differs from every chat blob and a blob copied to another row, member or
    org does not open. The scope is checked on every open. Without the key a
    write is `409 encryption_unavailable`.
20. **Write-only.** No response holds the URL or a part of it but the host. The
    POST, the PATCH and the test use `idempotency.Reject` (the body holds the
    secret). Validation errors name the rule, never the value. The poster maps
    every transport error to a fixed word.
21. **No event.** Personal webhooks are per-user private rows: create, change
    and delete emit nothing (T35, device registration; AGENTS.md). The page
    re-reads the list after its own writes. Reason: an event would tell other
    members that a person set a webhook.
22. **Nothing secret in a log.** The sender and the test log counts, ids and
    fixed words only; never a URL, a host, a name, an identifier or a state
    name.

## Routes

23. Under `/orgs/{org}`, scope `read` for the GET and `write` for the rest, then
    `refuseAgents`:
    - `GET /me/webhooks`: `{available, linksAvailable, limit, items}`;
    - `POST /me/webhooks` (201): `{name, kind, webhookUrl, events?, enabled?}`;
      absent or null `events` means both; `[]` is allowed and posts nothing (the
      webhook stays for the test message);
    - `PATCH /me/webhooks/{webhook}`: `{name?, webhookUrl?, events?, enabled?}`;
      a new URL clears the status; switching off deletes the waiting posts;
    - `DELETE /me/webhooks/{webhook}` (204, idempotent);
    - `POST /me/webhooks/{webhook}/test`: one fixed message; 6 a minute per
      member and org, in memory and per process (`429 rate_limited`, "too many
      test messages", with `Retry-After`); the webhook is read first, so an id
      the caller does not own is `404` and uses no token; a failure is `502
      chat_send_failed` with the fixed word; it works for a webhook that is off.
    Each route has a `noneRule` in `internal/api/teamrules.go`: the caller's own
    rows, no team-owned row is read or returned.

## Data (migration 0042)

24. **`personal_webhooks`** (`org_id`, `user_id`, composite FK to `org_members`
    with cascade, `(org_id, id)` unique, a unique index on `(org_id, user_id,
    lower(name))`, CHECKs for kind, name length, events, error words, blob
    size). **`personal_webhook_outbox`** (`webhook_id` composite FK with
    cascade, `issue_id` composite FK to `issues` with cascade, `kind`, the
    dedupe key unique per webhook, the chat states, skip reasons and error
    words, plus `access_removed`). The down drops both tables (the webhooks are
    lost; an older image has no such feature).

## Web

25. **Settings > Personal webhooks** (`/settings/personal-webhooks`), group
    Account, for every member. It lists the caller's webhooks (name, kind,
    host, status chip, the events) with Edit, Send test and Remove, and an Add
    webhook dialog: kind, name, URL (`type=password`, never shown again: Edit
    says "saved; type a new URL to replace it"), the two event boxes, the
    on/off switch. It says what posts and what never posts. Add is off at 3
    webhooks and on a server without a key.

## Open questions for the project owner

- Is "an issue I own" right as assignee OR creator? The other choice is the
  assignee only (then `my_issues` is "changes on issues assigned to me").
- Should a comment count as a change (now: yes), and a comment EDIT (now: no)?
- Should a mention of me post too (now: no; the inbox has mentions)?
- Is 3 webhooks per person and 60 posts per webhook per hour right?
- Should repeated failures switch a webhook off (now: no, as chat)?
- Should a platform or org admin be able to see that a member HAS webhooks
  (a count, never the URL)? Now: no admin view at all.
- The test route lets any member make a POST to any public https host they
  type (custom kind). The fixed reply word (`chat_gone`, `chat_unreachable`,
  ...) is a small oracle about that host. It is bounded by the test limiter
  (6 a minute per member) and the posts by the hourly cap (60 per webhook).
  Accept, or limit custom webhooks to admins?
- A stale post after a quick off and on: the consumer reads `enabled` when it
  makes the row; if the owner switches the webhook off (the waiting rows are
  deleted) and on again before the consumer commits, one row made from the
  old read can still be sent. Accepted as rare; say if not.

## Build notes

What LAUN-229 built: migration `0042_personal_webhooks`, queries
`db/queries/personal_webhooks.sql` (own and system tags, in the golden file),
`notify.PersonalWebhooks` (the member API), `notify.PersonalWebhookSender`
(started in `cmd/server` next to the chat sender), the consumer hook
`enqueuePersonal` (after the chat enqueue, before the person filters), the
routes in `internal/api/personal_webhooks.go` with `noneRule` entries, the
OpenAPI paths and schemas, and the web page `PersonalWebhooksPage`. The chat
state-name lookup became `chatStateNames` so both senders use it.

Tests: notify `TestPersonal_*` (assigned post and its text, no title or name,
replay; creator and assignee changes, own change, comment edit; event
choices and `not_selected`; hidden team and `access_removed`; muted, off,
deactivated; cap and failure policy; seal scope, agent rows, no events) and
the unit tests (`personalPostKind`, the sentences, the scope); api
`TestPersonalWebhooks_*` (auth and agent 403, own rows only, validation and
SSRF refusals, Idempotency-Key 400, cap, no key, cross-org, no secret in a
response, log, event or idempotency row) and
`TestOpenAPIPersonalWebhookSchemasMatchModels`; web
`personalWebhooks.test.tsx`.

### Not tested

- No real Slack, Discord, Fluxer or custom delivery: tests post to an
  `httptest` server through the real dial guard (as chat).
- The web page was tested with a mocked API in jsdom, not a real browser.
- The dial-time refusal of a public name that resolves to a private address
  is not tested here; it is the chat poster's, tested by the chat tests.
- Two concurrent creates at the cap (the member-row lock) are not raced in a
  test.

Review follow-up: the test limiter is per member and org (one member cannot
use up the others' tests) and is taken after the webhook is found; a custom or
Fluxer host over 253 bytes is `400 validation_failed` in the shared parser
(`MaxWebhookHostBytes`), so chat services get the same fix, where it was a
`500` before (CHECK of the host column); the sender wraps the last result
error.

### Carried

- No retention job beyond the sender's 30-day prune (as chat).
- A post that waits a retry after the owner muted the issue is still sent (the
  mute is read at event time; the access is read again at send time).
