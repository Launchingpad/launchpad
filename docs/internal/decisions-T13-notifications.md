> Internal design note from the Orbit deployment; kept for history.

# T13 decisions (LAUN-23): notifications, email and the in-app inbox

LAUN-23 is the design for notifications. It settles the data model, the
settings, the events, the worker, the email, the inbox API and the web pages,
and it ends with the tickets that build them. Each numbered call has its
reason, so a later ticket can change it on purpose. All nine tickets and the
retention job (LAUN-44) are built; "Build notes: shipped state (LAUN-39)" at
the end lists where the build differs from the calls above. The runbook is
`docs/deploy.md` "Notifications" and the builder's rules are in AGENTS.md
("Areas and where their rules are").

The answers (LAUN-23 comment, 2026-10-26):

- Immediate email in v1. Digests come later; the schema leaves room.
- The in-app inbox is in v1. Email and inbox read the same `notifications` rows.
- v1 events: assigned to me, state change on my issues, comments on my
  issues, mentions, new issue in a team I follow.
- Team "follow" is a per-user choice. No admin team defaults.
- Agents get no notifications.

The answers to the six open points (LAUN-23 comment, 2026-10-26: use the
defaults). They are decided; nothing in this note is open now:

- Defaults (#11): email on for assigned, mention and comment; state change
  and new issue are inbox-only.
- Mail volume (#22, #23): 2 minute quiet window, 30 mails per person per hour.
- A direct mention overrides a mute for that one notification (#9, #14).
- Accounts with `email_unproven` get inbox only (#21).
- No email without `PUBLIC_BASE_URL` (#21). Production must have it set:
  confirm at deploy time (ticket 9 runbook).
- The unsubscribe token is random with only its hash stored (#25).

Revision (2026-10-26): an automated review raised 12 findings.
Fixes are marked "Changed (review)" in the calls they touch; the ones
judged wrong are in "Review notes" at the end. Numbering is unchanged.

## What exists today (checked in the code)

| Part | State | Where |
|---|---|---|
| Change feed | `events.Emit` writes `events` (per-org `seq`) and `pg_notify` in the write transaction. The counter row lock is held to COMMIT, so seq order is commit order. | `server/internal/events/events.go` |
| Broker | One LISTEN connection per server; `Publish(org)` wakes SSE handlers per org. Notifications are only hints; the table is the truth. | `events.go`, `internal/api/events.go` (`streamEvents`) |
| Issue events | `issue.created` payload: `id`, `identifier`, `stateId`. `issue.updated` payload: `id`, `identifier`, `changes` (a list of names such as `state`, `assignee`), `version`, and `fromStateId` / `toStateId` on a state change. It does NOT carry the new assignee id or the team id. | `internal/tracker/issue_write.go` (lines ~184, ~455) |
| Comment events | `comment.created` / `.updated` / `.deleted` payload: comment id, `issueId` (null for a document comment). No body. | `internal/tracker/comments.go` |
| Other emitters | GitLab webhook moves (actor may be null), cycle job (no actor). The importer emits no events. | `gitlab_webhook.go`, `cycles.go` |
| Mail transports | `login.MailSwitch` forwards to the current `TextMailer` (`SendText(to, subject, text)` only: plain text, no HTML, no custom headers). Provider `none` or an empty `env` gives `ErrNoMailer`; `Ready()` reports it. | `internal/login/switch.go`, `mail.go`, `smtp.go` |
| Sign-up email | An in-memory bounded channel queue; a full queue drops the mail; nothing is stored or retried. Good for codes, not for notifications. | `internal/signup/mail.go` |
| Job pattern | `RunCycleJob`: run at boot, then on a ticker; a per-team `pg_advisory_xact_lock`; one failed run is logged and the loop goes on. | `internal/tracker/cycles.go` |
| Users | `users.is_agent`, `email` (nullable), `deactivated_at`, `email_unproven`. Membership is `org_members (org_id, user_id)`. | `server/db/schema.sql` |
| Routes | `router.go` is the route authority. `read` / `write` / `admin` scope middlewares; `s.idem` after the scope check; `idempotency.Reject` for secrets. | `internal/api/router.go` |
| Web | Settings pages live under `web/src/features/settings`. `SettingsLayout` is gated to org admins and platform admins. The shell sidebar and top bar are in `features/shell/Shell.tsx`. | `web/src/features/` |

Missing today, to be built here:

- No mention parsing (no `@username` code anywhere in `internal/tracker`).
- No subscription table, no per-user settings, no team follow.
- No `notifications` or outbox table.
- No HTML or header support in any mailer.
- No member-visible Settings page: `SettingsLayout` shows nothing to a plain member.
- The assignee and team are not in the issue event payloads (see #14).

## Data model (migration `0008_notifications`, or the next free number)

Every table has `org_id`. Every reference to an org row is a composite
`(org_id, x_id)` foreign key, as in the rest of the schema (AGENTS.md: no
RLS, tenancy in the application).

Changed (review): **every column is `NOT NULL` unless this note says `NULL`.**
Every enum column (`state`, `kind`, `event`, `reason`, `level`, `delivery`)
is `NOT NULL` with its CHECK, because a NULL passes a CHECK. Every
`created_at` / `updated_at` is `timestamptz NOT NULL DEFAULT now()`. Every
foreign key to `org_members`, `teams`, `issues` or `notifications` below is
`ON DELETE CASCADE`, so removing a member or a team never fails and never
leaves an orphan; the only `SET NULL` is `notifications.actor_id`.

1. **Eight tables.**

   | Table | Purpose |
   |---|---|
   | `notification_user_settings` | One row per `(org_id, user_id)`: the master switches. |
   | `notification_settings` | Sparse per-event switches, global and per team. |
   | `team_follows` | Which teams a user follows (new issue events). |
   | `issue_subscriptions` | Per-issue mute, manual subscribe, and "mentioned" rows. |
   | `notifications` | The inbox rows. One per recipient per cause. Email and inbox both read it. |
   | `notification_emails` | The email outbox: one row per notification that should be mailed. |
   | `notification_cursors` | Per org: the last event `seq` the consumer processed. |
   | `notification_mail_tokens` | One-click unsubscribe tokens (hash only). |

2. **`notification_user_settings`**
   `(org_id, user_id)` PK, FK `(org_id, user_id)` to `org_members` ON DELETE
   CASCADE; `email_enabled boolean NOT NULL DEFAULT true` (master switch);
   `include_excerpt boolean NOT NULL DEFAULT false`;
   `delivery text NOT NULL DEFAULT 'immediate'` with
   `CHECK (delivery IN ('immediate'))` today; `created_at`, `updated_at`.
   Room for digests: a later migration widens the CHECK to `hourly` and
   `daily` and adds nullable `digest_hour smallint` and `time_zone text`.
   Nothing reads them in v1. No row means all defaults.

3. **`notification_settings`** (sparse rows; an absent row means "inherit")
   `id`, `org_id`, `user_id`, `team_id uuid NULL` (NULL is the global
   level), `event text NOT NULL`
   `CHECK (event IN ('assigned','state_change','comment','mention','new_issue'))`,
   `email text NULL CHECK (email IN ('on','off'))`,
   `inbox text NULL CHECK (inbox IN ('on','off'))`.
   A NULL column means "inherit". FK `(org_id, user_id)` to `org_members`
   ON DELETE CASCADE; FK `(org_id, team_id)` to `teams` ON DELETE CASCADE
   (a `team_id` of NULL skips the check). Two partial
   unique indexes, because a NULL `team_id` is never equal to another NULL:
   `UNIQUE (org_id, user_id, event) WHERE team_id IS NULL` and
   `UNIQUE (org_id, user_id, team_id, event) WHERE team_id IS NOT NULL`.
   A new event kind is a new CHECK value in a migration, not a new column.
   A row with both columns NULL is deleted on write.
   Reason for rows: the event list will grow (the ticket draft lists a
   dozen more), and rows keep the table narrow and the API generic.

4. **`team_follows`** `(org_id, user_id, team_id)` PK, FK `(org_id, user_id)`
   to `org_members` ON DELETE CASCADE, FK `(org_id, team_id)` to `teams` ON
   DELETE CASCADE, `created_at`. Index
   `(org_id, team_id)` for "who follows this team".

5. **`issue_subscriptions`** `(org_id, issue_id, user_id)` PK,
   `state text NOT NULL CHECK (state IN ('subscribed','muted'))`,
   `reason text NOT NULL CHECK (reason IN ('manual','mentioned'))`, `created_at`,
   `updated_at`. FK `(org_id, issue_id)` to `issues` ON DELETE CASCADE; FK
   `(org_id, user_id)` to `org_members` ON DELETE CASCADE. Index
   `(org_id, user_id)`.
   The creator, the assignee and the commenters are NOT stored here (see #9).

6. **`notifications`** (the inbox rows; email reads them too)
   - `id uuid DEFAULT uuidv7()` PK; `org_id`; `recipient_id`;
   - `kind text NOT NULL CHECK (kind IN ('assigned','state_change','comment','mention','new_issue'))`;
   - `issue_id uuid NOT NULL` (v1 events are all about issues; a later
     `entity_type` column can add documents or cycles);
   - `actor_id uuid NULL` (null for GitLab and system moves; FK to `users`
     ON DELETE SET NULL);
   - `data jsonb NOT NULL DEFAULT '{}'` holding ids only (`fromStateId`,
     `toStateId`, `commentId`). No title, no body, no description: they are
     read from the issue when the row is shown or mailed, so a title edit
     or an issue delete never leaves stale text (the FK cascades);
   - `dedupe_key text NOT NULL`;
   - `event_seq bigint NULL` (the source event; NULL for none);
   - `inbox_seq bigint GENERATED ALWAYS AS IDENTITY` (live stream cursor,
     see #31);
   - `inbox boolean NOT NULL` (resolved at creation: show in the inbox);
   - `read_at timestamptz NULL`, `archived_at timestamptz NULL`,
     `created_at timestamptz NOT NULL DEFAULT now()`.
   - FK `(org_id, recipient_id)` to `org_members` CASCADE; FK
     `(org_id, issue_id)` to `issues` CASCADE.
   - **Exactly once:** `UNIQUE (org_id, recipient_id, dedupe_key)`. Inserts
     use `ON CONFLICT DO NOTHING` (see #12 for the key shapes).
   - Indexes: `(org_id, recipient_id, created_at DESC, id DESC) WHERE
     archived_at IS NULL AND inbox` for the list;
     `(org_id, recipient_id) WHERE read_at IS NULL AND archived_at IS NULL
     AND inbox` for the unread count; `(org_id, recipient_id, inbox_seq)`
     for the stream; `(created_at)` for the prune job.
   - A row exists when the recipient wants the inbox row OR the email. A
     row with `inbox = false` is an email-only row; the inbox API never
     returns it.

7. **`notification_emails`** (the outbox)
   `id`, `org_id`, `notification_id` (unique; FK `(org_id,
   notification_id)` CASCADE), `recipient_id`, `issue_id`,
   `state text NOT NULL DEFAULT 'pending' CHECK (state IN
   ('pending','sending','sent','skipped','dead'))`,
   `skip_reason text NULL` (`no_mail`, `no_address`, `unproven`,
   `no_base_url`, `rate_cap`, `expired`, `disabled`; `access_removed` since
   0039, decisions-T20 #63),
   `attempts int NOT NULL DEFAULT 0`,
   `next_attempt_at timestamptz NOT NULL DEFAULT now()` (Changed (review):
   a NULL here would never match the claim in #19, so the default makes
   every insert claimable),
   `batch_id uuid NULL` (the mail this row went out in; also the future
   digest id), `last_error text NULL` (the `MailError` text only, never the
   address or the body), `created_at`, `sent_at timestamptz NULL`.
   A CHECK keeps the invariant: `state = 'sent'` needs `sent_at` and
   `batch_id` set.
   Index `(next_attempt_at) WHERE state = 'pending'` and
   `(org_id, recipient_id, issue_id) WHERE state = 'pending'`.
   Reason for a separate table: delivery state, retries and batches are a
   different life cycle from the inbox row (read, archive). A future
   channel (digest, webhook) adds rows or a `channel` column here and does
   not touch `notifications`.

8. **`notification_cursors`** `(org_id)` PK, `last_seq bigint`, `updated_at`.
   **`notification_mail_tokens`** `(token_hash bytea)` PK, `org_id`,
   `user_id`, `targets jsonb NOT NULL`, `created_at`, `expires_at`,
   `used_at NULL`. FK `(org_id, user_id)` to `org_members` CASCADE. The token
   itself is never stored (see #25). Changed (review): `targets` replaces the
   single `event` / `team_id` / `level` columns. It is a list of 1 to 5
   `{event, teamId}` objects (`teamId` null means the global level), fixed
   when the mail is built, so one token can cover a mail that holds several
   kinds and it does not depend on outbox rows that retention may delete
   before the token expires.

9. **The creator and the assignee are implicit subscribers; commenters are
   read from `comments`.** The recipients of an issue's changes are:
   `issues.creator_id`, `issues.assignee_id`, the distinct
   `comments.author_id` of the issue, and `issue_subscriptions` rows with
   state `subscribed`; minus rows with state `muted`. Reason: no backfill
   is needed for the issues imported from Linear, nothing goes stale when
   the assignee changes, and there is no second table to keep in step with
   `issues` and `comments`. The cost is one small query per event (indexed
   by `issue_id`). A person can mute one issue (Subscribe control): a
   `muted` row wins over all implicit reasons. A person who is mentioned
   gets a `mention` notification; a mention does NOT undo a mute for later
   changes (it adds a `subscribed` row only when no row exists).

10. **Retention.** The job deletes notifications 90 days after `created_at`
    when read or archived, and every row after 180 days; outbox rows with
    state `sent` or `skipped` after 30 days (they go with their
    notification by CASCADE; `dead` rows are kept 90 days for the
    operator). Tokens are deleted 7 days after `expires_at`.
    The job is `notify.RunRetention`: it runs at boot and then every hour,
    deletes in batches of at most 1000 rows per statement (a 50 ms pause
    between full batches), and takes a session try advisory lock so only one
    instance prunes at a time.
    Changed (review): the job has its own build ticket, **LAUN-44**
    ("Server: notifications retention job"), built as `notify.RunRetention`;
    it is not part of tickets 1 to 9.

## Settings resolution

11. **Resolution order, per recipient, per event, per channel (email and
    inbox):** the team row for the issue's team (when its column is not
    NULL), else the global row (when its column is not NULL), else the
    built-in default. The master `email_enabled = false` turns email off for
    every event and wins over all rows. The web app shows the RESOLVED
    value and where it comes from (`team`, `global`, `default`).

    Built-in defaults (a person who never opens Settings still gets useful
    mail, and little noise):

    | Event | Email | Inbox |
    |---|---|---|
    | `assigned` | on | on |
    | `mention` | on | on |
    | `comment` | on | on |
    | `state_change` | off | on |
    | `new_issue` | off | on (only for followed teams) |

    Reason: a state change on an issue you own is frequent and visible in
    the issue list; mail for it is opt-in. The assignment, mention and
    comment are things that wait for a person.

    **`new_issue` needs a follow.** It applies only to teams in
    `team_follows`; no follow, no notification, whatever the settings say.
    The default is "follow nothing" (per-user choice, no admin
    defaults). The settings page lists the teams as switches.

## Events, recipients and the mapping

12. **Event to notification.** One cause makes at most ONE notification per
    recipient. The dedupe key says what the cause is.

    | Source event | Kind | Recipients | `dedupe_key` |
    |---|---|---|---|
    | `issue.created` with an assignee | `assigned` | the assignee | `evt:<seq>:assigned` |
    | `issue.created` | `new_issue` | followers of the issue's team | `evt:<seq>:new_issue` |
    | `issue.updated`, `changes` has `assignee` | `assigned` | the NEW assignee | `evt:<seq>:assigned` |
    | `issue.updated`, `changes` has `state` | `state_change` | issue recipients (#9) | `evt:<seq>:state_change` |
    | `comment.created` on an issue | `comment` | issue recipients (#9) | `evt:<seq>:comment` |
    | `comment.created` / `.updated` with `@name` in the body | `mention` | each mentioned user | `mention:c:<comment_id>:<user_id>` |
    | `issue.created` / description change with `@name` | `mention` | each mentioned user | `mention:i:<issue_id>:<user_id>` |

    A mention key has no `seq`, so an edit that adds a name notifies only
    the new name, and an edit that changes the text but not the names
    notifies nobody. A person mentioned in a comment who is also an issue
    recipient gets ONE notification for that comment (see #13).

    Not in v1: `comment.deleted`, comments on documents, `relation.*`,
    label, priority, cycle and project changes. One update that changes
    both state and assignee makes two notifications (the keys differ);
    #13 still gives one recipient one kind per EVENT, so the new assignee
    gets `assigned` only for that event.

13. **One kind per recipient per event.** When one event gives a recipient
    more than one kind (for example a comment that mentions the assignee),
    the precedence is `mention`, then `assigned`, then `comment`, then
    `state_change`, then `new_issue`. The first kind whose resolved email or
    inbox switch is on wins. If all are off for both channels, no row is
    made. So a person with `mention` off but `comment` on still hears
    about a comment that names them.

14. **Recipient filters** (applied for every kind, at consumer time):
    - never the actor (`events.actor_id`); a null actor excludes nobody;
    - only an ACTIVE user (`deactivated_at IS NULL`) who is NOT an agent
      (`is_agent = false`) and is a current member of the org;
    - not for an archived issue or an archived team;
    - not a user with a `muted` row for the issue (a mention is the one
      exception: it is direct);
    - the same filters apply again when the email is sent (#21).

    Agents never get a row. Changed (review): every notification route
    (inbox, stream, settings, follows, subscription) answers `403 forbidden`
    for an agent token (no new error code), the same everywhere. The earlier
    "empty list and 0" for the inbox is gone; only the public unsubscribe
    route has no caller.

    **Payload gap to close first:** `issue.created` and `issue.updated`
    must carry `teamId` and `assigneeId` (and `fromAssigneeId` on a change).
    Without them the consumer would read the CURRENT assignee, and two quick
    reassignments would notify the wrong person. The change is additive to
    a free-form payload, so no client breaks. The GitLab webhook emits
    through the same helpers; the ticket checks that its `issue.updated`
    payload has the same shape.

## Where notifications are made

15. **A consumer that reads the change feed, not the write transaction.**
    Considered: (a) insert notifications inside each write transaction;
    (b) a job that reads `events` after a cursor. Chosen: (b).

    Reasons:
    - **A notification bug can never fail or slow a write.** The AGENTS.md
      rule is that writes append an event as the LAST statement so the seq
      counter lock stays short. Work for N recipients inside that
      transaction would hold the lock longer and touch more tables.
    - **One place.** Mentions, subscriptions and settings are read in one
      module, not in every write path (issues, comments, GitLab, cycles).
    - **Replay is safe.** The unique key (#6) makes a second pass a no-op, so
      a crash, a restart or a bug fix re-run loses and duplicates nothing.
    - The events already carry everything the consumer needs (after #14),
      and `seq` order is commit order, so a cursor cannot skip a committed
      event.

    The cost is a delay of a second or less (the consumer wakes on the
    broker) and one more reader of `events`. Accepted.

16. **How the consumer runs.** `internal/notify` (new package; it may import
    `tracker`'s `dbq` queries but `tracker` does not import it).
    - Started in `cmd/server` like `RunCycleJob`: run at boot, then wake on
      the broker (`Subscribe(org)` per org that has a cursor, or one
      `PublishAll`-style wake) and also on a 10 second ticker, so a lost
      NOTIFY costs latency only.
    - Per org, in ONE transaction: take
      `pg_try_advisory_xact_lock(hashtextextended('devops:notify:' || org_id, 0))`
      (a second instance gets false and skips that org; the cycle job uses
      the blocking form, but here waiting only wastes a connection);
      read `notification_cursors.last_seq`; read up to 200 events with
      `seq > last_seq ORDER BY seq`; insert notifications and outbox rows;
      advance the cursor; COMMIT. The cursor moves in the same transaction
      as the inserts, so there is no gap and no duplicate.
    - A poison event (a decode error, a missing issue) is logged at WARN
      with the org id, seq and type only, skipped, and the cursor goes on.
      An unexpected database error rolls back the batch and the next tick
      retries it.
    - **No history on first run.** The migration inserts a cursor at the
      current max `seq` for every org. An org without a cursor starts at
      its current max. Nobody gets mail for old events.
    - **Stale events are not mailed.** An event older than 24 hours when
      read (a long outage) advances the cursor and makes no notification.
    - After COMMIT the consumer sends `pg_notify` on the existing channel
      `devops_events` with the org id, so every server's broker wakes its
      streams (#31). Failure to notify is logged and ignored.

17. **Mention parsing** (new, in `internal/notify`, pure and unit tested).
    A mention is `@username` where `username` matches the existing
    `users_username_format` (`[a-z][a-z0-9_-]{0,38}`), not preceded by a
    letter, digit, `_`, `.` or another `@` (so `a@example.com` is not a mention),
    and outside code spans, fenced code blocks and quoted lines (`>`).
    The match is case-insensitive and resolved against active, non-agent
    members of THIS org only; an unknown name is plain text. The text
    comes from the comment row or the issue description at consumer time
    (the event holds no text). The web editor does not change in v1 (a
    typed `@alice` works; autocomplete is a later ticket). A mention of an
    agent is plain text: agents get nothing (#14).

## Email

18. **A stored outbox, not the sign-up queue.** The sign-up queue is an
    in-memory channel that drops mail when full and never retries
    (`internal/signup/mail.go`). That is right for a one-time code and wrong
    here. Notification mail goes through `notification_emails` (#7).
    Mail failure never fails a write because no write touches the mailer.

19. **The sender loop** (`internal/notify`, `RunSender`): at boot and every
    15 seconds. It runs under its own advisory lock
    (`devops:notify-send`, try-lock, held for the batch) so several
    instances never send the same row. It claims rows
    (`state='pending' AND next_attempt_at <= now()`, `FOR UPDATE SKIP LOCKED`,
    limit 50), sets them `sending`, sends OUTSIDE the transaction with a
    15 second timeout, and records the result. A row stuck in `sending` for
    more than 10 minutes (a crash) returns to `pending`. Delivery is
    at-least-once: a crash between the SMTP accept and the `sent` update can
    send one mail twice. The `Message-ID` header is the outbox batch id, so
    a mail client can fold the copy.

20. **Retries and the dead letter.** A failed send adds one to `attempts`
    and sets `next_attempt_at` by this schedule: 1 minute, 5 minutes,
    30 minutes, 2 hours, 12 hours. The sixth failure sets `dead`. Every
    row also expires at 24 hours after creation (`skipped`, `expired`): an
    assignment mail one day late is noise. `last_error` holds only the
    `login.MailError` text (a step and a reply code), as the sign-up
    queue's log does. The sender logs one WARN per batch with counts, never
    an address or a subject. `dead` rows are counted in the boot log and
    listed in the runbook (SQL); a platform-admin view is out of scope.

21. **Mail provider `none`, no address, no base URL.** The sender asks
    `MailSwitch.Ready()` first.
    - Not ready (`none`, or `env` with nothing set): rows stay `pending` and
      are NOT attempted, until the 24 hour expiry sets them `skipped` /
      `expired`. Mail turned on within the day delivers the recent ones;
      older ones are dropped. Inbox rows are unaffected. Nothing queues
      forever and nothing fails.
    - The consumer does not read the mail setting, so a switch of provider
      needs no restart.
    - Before sending, the sender re-checks the recipient: active, not an
      agent, still a member, `users.email` set, and NOT `email_unproven`
      (an address nobody proved might belong to someone else). Failures set
      `skipped` with the reason.
    - Changed (review): it also re-resolves the PREFERENCE with
      `notify.Resolve` (#33) from the rows as they are now: `email_enabled`
      and the event's email value at the issue's team. If the result is not
      `on`, the row is `skipped` / `disabled`. So a person who turns email
      off, turns one event off, or uses one-click unsubscribe (#25) stops
      the mail that is still pending, not only later mail. The inbox row is
      unaffected.
    - `PUBLIC_BASE_URL` empty: `skipped` / `no_base_url`. A mail without a
      working unsubscribe and issue link is not sent (#25).

22. **Burst collapsing.** The sender holds a pending row for a quiet window
    of 2 minutes after the NEWEST pending row of the same
    `(recipient_id, issue_id)` (and for at most 10 minutes after the oldest),
    then sends ONE mail for the group: the rows share a `batch_id`. Five
    edits in two minutes is one mail. Subject and body list the changes in
    order, by kind: "State: Todo to In Review (Alice)", "New comment
    (Claude)". The first notification's kind (precedence order) picks the
    subject. Windows are constants in code in v1, not settings.

23. **Per-user rate cap.** At most 30 mails per recipient per rolling hour.
    Changed (review): it counts MAILS, that is `count(DISTINCT batch_id)`
    over that recipient's `sent` rows with `sent_at` in the last hour, not
    rows; a batch of five changes is one mail. The sender also counts the
    batches it has already built in the current pass. Rows over the cap get `skipped` / `rate_cap`
    and still show in the inbox. The next mail that does go out carries one
    line, "N more updates were not emailed. See your inbox." The cap and
    the window are constants in v1.

24. **Templates** (Go `text/template` and `html/template`, in
    `internal/notify/mail/`, text AND html in one `multipart/alternative`).
    - Subject: `[LAUN-23] <short reason>: <title>`, title cut at 100
      characters, control characters removed (as `oneLine` in
      `internal/signup/mail.go`).
    - Body: the issue identifier and title, the team, the actor's display
      name, the reason line ("Alice assigned this to you", "State changed:
      A to B"), the current state, and a link
      `PUBLIC_BASE_URL/<org>/issue/<identifier>` (singular, as the web route; LAUN-94).
    - **No description, no comment text by default.** With
      `include_excerpt = true` (a global per-user setting, default false) a
      mail adds the first 200 characters of the comment (or description for
      a mention in it), plain text only, HTML-escaped. Reason: a mailbox is
      less private than the tracker.
    - Footer: "You get this because <reason>", a link to Settings,
      Notifications, and the one-click unsubscribe link.
    - No tracking pixel, no external image, no IP or User-Agent stored.
    - The html is escaped by `html/template`; the plain text is the
      source of truth in tests (golden files, like
      `internal/mcp/testdata`).

25. **One-click unsubscribe.**
    - Each MAIL (a batch) gets one random 32-byte token (`crypto/rand`,
      base64url). Only `sha256(token)` is stored in
      `notification_mail_tokens` (like invite tokens in decisions-T12). It
      is opaque, not an HMAC: no signing key to rotate, no way to forge it,
      and a database read leaks nothing usable. (The ticket says "signed
      token"; a random token with a stored hash is the stronger form of the
      same idea.) It expires after 60 days.
    - Changed (review): the token names the user, the org and the
      `targets` of the mail (#8): one `{event, teamId}` per distinct kind in
      the batch. For each kind the level is `team` when the resolved value
      came from a team row, else `global`; a value that came from the
      built-in default also lands at `global`, where the unsubscribe writes
      a stored row (`email = 'off'`, created if absent). Using the token sets
      `email = 'off'` for exactly those events at those levels, nothing
      else: not the account, not the master switch, not the inbox, not other
      events. A single-kind mail (the common case) turns off that one event.
      The sender re-checks preferences (#21), so pending mail stops too.
    - Headers: `List-Unsubscribe` holds the POST URL
      `<PUBLIC_BASE_URL/api/v1/notifications/unsubscribe/<token>>` and
      `List-Unsubscribe-Post: List-Unsubscribe=One-Click` (RFC 8058).
    - Route: public `POST /api/v1/notifications/unsubscribe/{token}`
      (outside `Authenticate`, `idempotency.Reject`, no cookie read, a
      300 ms floor, a per-client limit on the sign-up pattern in
      `internal/api/clientip.go`). The answer is `204` for a valid, used,
      expired or unknown token, so it says nothing about who has an
      account. `GET` of the same URL is NOT an action (mail scanners fetch
      links): it is a web page `/unsubscribe/<token>` with a button that
      sends the POST. The token appears in a path, so access logs must not
      log the path of this route; the ticket adds a test that the server's
      request log does not hold it (the same rule as AGENTS.md: no secret
      in a log line).
    - The token is never logged, emitted, or stored in an idempotency row.

26. **Transport change.** `SendText` cannot carry HTML or headers. Add to
    `internal/login` a `Message` struct (`To`, `Subject`, `Text`, `HTML`,
    `Headers`) and a `MessageMailer` interface with `SendMessage(ctx, Message)`.
    The Postmark mailer maps it to `HtmlBody` and `Headers`; the SMTP mailer
    builds `multipart/alternative` with the headers (CR/LF in any header
    value is refused); the dev log mailer logs the subject only (as
    today); `MailSwitch` forwards it. `SendText` and `SendLoginCode` do not
    change. The `smtptest` fake gains a check of the headers.

## Inbox API

27. **Routes** (all under `/api/v1/orgs/{org}`, all for the CALLER's own
    rows; there is no way to read another user's inbox, not even as admin).
    `router.go` AND `api/openapi.yaml` change together
    (`TestOpenAPIMatchesRoutes`).

    | Route | Scope | Notes |
    |---|---|---|
    | `GET /notifications` | `read` | `?state=unread\|all` (default `all`), `?archived=true`, `?limit` (default 30, max 100), `?cursor` |
    | `GET /notifications/unread-count` | `read` | `{count}` |
    | `PATCH /notifications/{id}` | `write` | `{read?: bool, archived?: bool}`; `true` and `false` both work |
    | `POST /notifications/read-all` | `write` | `{before: timestamp}` REQUIRED; marks unread rows with `created_at <= before` as read |
    | `GET /notifications/stream` | `read` | SSE, see #31 |

    `PATCH` and `read-all` are replay-safe by themselves, so they take no
    `Idempotency-Key` middleware. Changed (review):
    - `PATCH` sets a STATE, so a replay gives the same result and an undo
      works: `read: true` sets `read_at = COALESCE(read_at, now())` (the
      first time stays), `read: false` sets `read_at = NULL`; `archived`
      works the same way with `archived_at`. An empty body is `400`.
    - `read-all` requires `before`. The web app sends the `createdAt` of the
      newest row it has loaded. A retry after a timeout then marks the same
      rows, never a notification that arrived in between. No `before` is
      `400 validation_failed`.
    A notification id of another user or another org is `404 not_found`.
    An agent token gets `403 forbidden` (#14).

28. **Pagination and order.** Newest first by `(created_at DESC, id DESC)`
    with an opaque keyset `cursor` (the pair, base64), as the existing list
    routes do (decisions-T3: milestones and cycles use `(updated_at, id)`).
    Never `ORDER BY id` alone (AGENTS.md: uuidv7 is not monotonic within a
    millisecond); the `id` is only the tie breaker. The response is
    `{items, nextCursor}`.

29. **Item shape.** `id`, `kind`, `createdAt`, `readAt`, `archivedAt`, `actor`
    (`{id, username, name}` or null), `issue` (`{id, identifier, title,
    team key, state name}`), `data` (comment id, from/to state names), and
    NOT the description or comment text (the web app opens the issue). One
    query joins the issue, team and state tables with the recipient filter;
    no per-row query.

30. **Rate limits and size.** No new limiter is needed for reads. The list
    caps at 100 rows. Unread count is a partial-index count (#6).

31. **Live updates reuse the broker.** After the consumer commits it does
    `pg_notify('devops_events', <org id>)`. Every server's listener calls
    `Broker.Publish(org)` (`events.Listen`), which wakes
    `GET /notifications/stream`. That handler is a copy of the
    `streamEvents` pattern (heartbeat, poll fallback every `SSEPoll`,
    re-check token and membership on each tick, `Last-Event-ID`): it
    reads `notifications WHERE org_id = $org AND recipient_id = $caller AND
    inbox AND archived_at IS NULL AND inbox_seq > $last` (Changed (review):
    the `inbox` filter keeps email-only rows out, as in the list routes) and
    sends `{id, kind, inboxSeq}` for each new row.
    On every wake it also reads the unread count and, when it differs from
    the last count sent on this connection, sends a `count` event
    `{count}`. A `read_at` change moves no `inbox_seq`, so the count event is
    what carries a mark-read, an unread, an archive or a read-all to the
    other tabs.
    `inbox_seq` is an identity column; it is safe as a stream cursor
    because only ONE transaction writes an org's notifications at a time
    (the advisory lock in #16), so identity values commit in order within
    an org. The PATCH and read-all handlers publish the org on the broker
    after commit, which wakes the streams, and so the `count` event above
    reaches the other tabs. The org-wide
    event stream is NOT reused: it would send every person every event.
    A web app that cannot open the stream falls back to polling
    `unread-count` every 60 seconds.

## Preference and subscription API

32. **Routes** (under `/orgs/{org}`; the caller's own rows only; agent tokens
    get `403 forbidden`).

    | Route | Scope | Notes |
    |---|---|---|
    | `GET /me/notification-settings` | `read` | the whole resolved view (below) |
    | `PUT /me/notification-settings` | `write` | `{emailEnabled?, includeExcerpt?, events: {<event>: {email?, inbox?}}}` global level; `null` = inherit/default (delete the column) |
    | `PUT /me/notification-settings/teams/{team}` | `write` | `{events: {<event>: {email?, inbox?}}}`; `"on"`, `"off"`, or `null` for inherit |
    | `PUT /me/teams/{team}/follow` | `write` | idempotent; `204` |
    | `DELETE /me/teams/{team}/follow` | `write` | idempotent; `204` |
    | `GET /issues/{issue}/subscription` | `read` | `{state: "subscribed"\|"muted"\|"default", reasons: [...]}` |
    | `PUT /issues/{issue}/subscription` | `write` | `{subscribed: true\|false}`: true adds a `subscribed` row, false adds `muted` |
    | `DELETE /issues/{issue}/subscription` | `write` | removes the row (back to the implicit rules) |

    `PUT` is whole-document for the part it names, replay-safe, and takes
    no `Idempotency-Key`. A `team` in the path is resolved like every team
    path (key, id, or Linear id) in this org; another org's team is `404`.
    A body event name that is not in the CHECK list is `400
    validation_failed`.

    `GET /me/notification-settings` returns, per event: the global
    stored values (`email`, `inbox`, each `"on"`, `"off"` or null), the
    built-in default, and per team the override, the RESOLVED value and its
    `source`; plus `follows` (team ids), `emailEnabled`, `includeExcerpt`,
    and `mail: {available: bool}` (from `MailSwitch.Ready()`, so the page
    can say "email is off for this install" without leaking anything else).

33. **Resolution lives in one pure function** (`notify.Resolve(user, team,
    event, rows) -> {email, inbox, source}`), table tested, and the same
    function builds the GET view and the consumer's decision. The web app
    never recomputes it.

## Safety and privacy

34. **Org scoping.** Every query filters by `org_id` and the caller's
    `user_id`. Every foreign key is composite. Each new resource gets a
    cross-org test (AGENTS.md convention): user A of org 1 cannot read,
    patch or archive org 2's notifications, set org 2's team follow, or
    mute org 2's issue; a team or issue id of another org in a path is
    `404 not_found`; a body reference to another org's row is `400`. The
    consumer test makes two orgs with the same usernames and checks that no
    row crosses.

35. **No IP, no User-Agent, no secrets.** The tables hold no caller address
    and no User-Agent (AGENTS.md). The unsubscribe token appears only in
    the mail and in the request path of one route (#25). No log line, event
    payload or idempotency row holds a token, an address, a subject or a
    body. A test in the pattern of `TestSignups_NoSecretsInLogsEventsOrIdempotency`
    covers unsubscribe tokens. `last_error` is a `MailError` text only.
    `include_excerpt` is off by default.

36. **Agents.** The consumer skips `is_agent` users, the settings and inbox
    routes refuse their tokens, and a token minted for an agent is not given
    notification routes in the MCP tool list (no MCP change in v1).
    `TestNotify_AgentsGetNothing` checks all three (a comment that mentions
    the agent makes no row).

37. **Failure containment.** The consumer and the sender are separate
    goroutines started with `wg.Go` in `cmd/server`, as the cycle job is.
    A panic is recovered per batch, logged with the org id, and the loop
    goes on. A mail or consumer fault cannot fail a write: no write path
    calls into `internal/notify`.

## Web

38. **Settings > Notifications** is for every member, not only admins. Today
    `SettingsLayout` shows a plain member "You need admin access."
    (`web/src/features/settings/SettingsLayout.tsx`, `access.ts`). The
    page goes under a new `/:org/settings/notifications` route that the
    layout lets every member open, and the layout shows only that entry to
    a member (the other pages keep their admin gate). Sections:
    - Email on/off (master), "include a short excerpt", and a note when
      `mail.available` is false ("This install cannot send email. The
      inbox still works.").
    - A table: one row per event, columns Email and Inbox, each a
      three-state control (Default, On, Off) that shows the resolved
      value.
    - Teams: one row per team with a Follow switch and an expandable
      "Override for this team" panel with the same table; the control
      shows "Inherit (On)" for an unset value.
    - Saves on change, with the errors of other settings pages
      (`FixedError`).

39. **Inbox** is a bell with an unread badge in the shell top bar
    (`features/shell/Shell.tsx`), opening a popover with the latest 10 and
    "Mark all read", and a full page `/:org/inbox` with Unread / All /
    Archived, mark read, archive, and a link to the issue. Opening an issue
    from a row marks the row read. The badge is fed by the stream (#31)
    with the polling fallback. Keyboard: the popover follows the shell's
    shortcut pattern (`ShortcutHelp.tsx`).

40. **Subscribe control** on the issue detail page
    (`features/issues/IssueDetailPage.tsx`): a button "Subscribe" /
    "Unsubscribe" with a tooltip that gives the reason ("You are the
    assignee"). It calls the subscription routes (#32); a person who is an
    implicit subscriber sees "Unsubscribe" and gets a mute.
    (Built as a visible sentence instead of a tooltip: `docs/internal/web-decisions.md`, LAUN-38.)

## Out of scope in v1 (and how the design leaves room)

41. | Not built | Room left |
    |---|---|
    | Digests (hourly, daily at an hour and zone) | `notification_user_settings.delivery` (CHECK widened later), `digest_hour`, `time_zone` added later; `notification_emails.batch_id` is already the "one mail, many rows" key; a digest builder is a second consumer of `notification_emails` with `state='pending'` and a longer window. |
    | Quiet hours | Same table, new columns; the sender's "not before" check is one place. |
    | More events (priority to urgent, cycle changes, MR opened or merged, admin events for sign-up requests and invites, any state change in a team, unassigned-only new issues) | A new `event` CHECK value and one mapping row in #12; the settings API is generic over event names. |
    | Notify the actor of their own change (the ticket's "option to override") | A boolean column on `notification_user_settings`; one filter in #14. |
    | Webhooks, mobile push, Slack | A `channel` column on the outbox (or a second table keyed by `notification_id`). Agents stay excluded (#14). |
    | Notifications for documents, projects, milestones | `notifications.issue_id` becomes nullable beside `entity_type` / `entity_id` in a migration. |
    | Admin-set team defaults | Rejected. A later `team_defaults` table would slot in between the team row and the global row in #11. |
    | Mention autocomplete in the editor | Web only; no server change. |
    | A platform-admin page for dead letters | Read-only `GET /platform/notifications` over `notification_emails`. |

## Decided points (2026-10-26: use the defaults)

The five open points of the first draft are answered; they are listed at the
top of this note and applied in #9, #11, #14, #21, #22, #23 and #25. One
follow-up stays for deploy time: confirm that production has `PUBLIC_BASE_URL`
set (#21). Nothing else is open.

## Ticket split (build order)

Each ticket is one MR. All server tickets run `make docker-test` and, with a
database, `make docker-test-integration`; schema tickets also
`make sqlc-check schema-check` and `TestMigrateUpDownUp`.

1. **Notifications: schema, migration and sqlc**
   - Scope: migration `NNNN_notifications` (+ down) with the tables in #2
     to #8 (tokens hold `targets jsonb`, #8), indexes in #6 and #7, cursor seeding (a cursor per existing org
     at its max `seq`); sqlc queries for CRUD (no handlers); `make sqlc
     schema`.
   - Depends on: none.
   - Acceptance: up/down/up passes; every FK is composite on `org_id`; the
     two partial unique indexes on `notification_settings` reject
     duplicates for NULL and non-NULL `team_id`; `notifications` rejects a
     second `(org_id, recipient_id, dedupe_key)`; a member removal cascades;
     the CHECKs reject unknown `event`, `kind`, `state`; every enum column
     is `NOT NULL` (a NULL insert fails); an insert into `notification_emails`
     with only the required ids is claimable (`state = 'pending'`,
     `next_attempt_at` set); removing a member or a team cascades its
     settings and follows; `db/schema.sql` and `internal/dbq` are current.

2. **Notification preferences, follows and issue subscription API**
   - Scope: `internal/notify` settings service with the pure
     `Resolve` (#33); the routes in #32; `router.go` + `openapi.yaml`;
     agent refusal.
   - Depends on: 1.
   - Acceptance: resolution order team, global, default is table tested;
     `GET` returns the resolved value and `source`; `null` clears an
     override; the master switch wins; follow and unfollow are idempotent;
     subscription PUT/DELETE work; unknown event is `400`; an agent token
     is `403`; cross-org tests for team, issue and settings; the OpenAPI
     tests pass.

3. **Notification creation: consumer, mention parsing, event payloads**
   - Scope: add `teamId`, `assigneeId` and `fromAssigneeId` to
     `issue.created` / `issue.updated` payloads (and check the GitLab
     path); `notify.Consume` per #15, #16 and #12 to #14 (cursor, advisory
     lock, batch, stale-event rule, kind precedence, recipient filters, the
     outbox row when email resolves on); mention parser (#17); the loop in
     `cmd/server`; the `pg_notify` after commit.
   - Depends on: 1, 2.
   - Acceptance: every row of the #12 table has a test (one per event);
     the actor, an agent, a deactivated user, a non-member and a muted
     user get nothing; a replayed batch makes no duplicate; an edit that adds
     a mention notifies only the new name; `a@example.com` and code spans are not
     mentions; two instances (two goroutines) on one org make each row once;
     old events at first run and events older than 24 hours make no row; a
     poison event does not stop the cursor; a write succeeds when the
     consumer is stopped; cross-org test with equal usernames; no secret or
     body text in logs.

4. **Notification email: transport, outbox sender, templates, unsubscribe**
   - Scope: `login.Message` / `MessageMailer` for Postmark, SMTP, dev log
     and `MailSwitch` (#26); `notify.RunSender` (#19 to #23); text + html
     templates and golden files (#24); token table use and the public
     unsubscribe route + openapi (#25); the web `/unsubscribe/<token>`
     confirm page can follow in ticket 6.
   - Depends on: 3.
   - Acceptance: with provider `none` nothing is sent, nothing fails and
     rows expire at 24 hours; retry schedule and `dead` are tested with an
     injected clock (`SetClock` pattern); five rows for one issue in two
     minutes make one mail; the 31st mail in an hour is `rate_cap`; an
     unproven or address-less user is `skipped`; a pending row whose
     recipient has since turned email off, turned that event off or used
     unsubscribe is `skipped` / `disabled`; the cap counts mails (distinct
     `batch_id`), so one batch of five rows is one mail; a mail has text AND html,
     `List-Unsubscribe`, `List-Unsubscribe-Post`, no description and no
     comment text by default; the token is random, only its hash is stored,
     the POST turns off exactly the events named in the token's `targets` at
     their levels (a two-kind mail turns off both; a default-resolved value
     gets a global `off` row) and answers `204`
     for a bad token; GET is not an action; no token in logs, events or
     idempotency rows; the `smtptest` fake checks the headers; two senders
     send each row once.

5. **Inbox API and live stream**
   - Scope: the routes in #27, keyset order (#28), item shape (#29), the
     stream (#31), openapi.
   - Depends on: 3.
   - Acceptance: list order is `(created_at, id)` desc with a stable cursor
     across ties (test with equal timestamps); unread count matches;
     `PATCH` sets state both ways (`read: false` marks unread) and keeps the
     first `read_at` on a repeat of `true`; `read-all` without `before` is
     `400` and a retry never marks a newer row; email-only rows are never
     returned by the list or the stream; a mark-read in one connection gives
     a `count` event on another; another user's or
     another org's id is `404`; an agent token gets `403` on every route; the stream
     delivers a new row within the poll interval on one server and
     through NOTIFY on another; the stream ends when the membership goes.

6. **Web: Settings > Notifications**
   - Scope: member-visible route and layout change (#38); the global table,
     master switches, team follow and override panels; the
     `/unsubscribe/<token>` confirm page; generated client update.
   - Depends on: 2 (4 for the unsubscribe page).
   - Acceptance: a plain member opens the page and not the admin pages;
     the control shows resolved values and `source`; "Inherit" clears an
     override; follow persists; the "mail is off" note shows when
     `mail.available` is false; keyboard and phone-width layout work;
     component tests in the style of `settings.test.tsx`.

7. **Web: inbox bell, popover and page**
   - Scope: shell bell and badge, popover, `/:org/inbox` (#39), stream with
     60 second polling fallback.
   - Depends on: 5.
   - Acceptance: the badge follows a new notification without reload; mark
     read, mark all read and archive update the list and the badge; opening
     an issue marks it read; Unread / All / Archived filters work; empty
     and error states; tests with a fake stream.

8. **Web: Subscribe control on the issue page**
   - Scope: button and tooltip on `IssueDetailPage` (#40).
   - Depends on: 2.
   - Acceptance: it shows the reason; subscribe and mute round-trip; the
     state is right after a reload; an agent view or a read-only token does
     not show a broken button.

9. **Docs and runbook**
   - Scope: `docs/deploy.md` "Notifications" (mail provider `none`,
     what the sender does, dead letters and SQL to list and retry them,
     rate caps, retention, `PUBLIC_BASE_URL` requirement, how to turn the
     feature off by stopping mail); a short AGENTS.md section; update this
     file where the build changed a call.
   - Depends on: 1 to 5 and 10.
   - Acceptance: the runbook steps were run on a local stack (provider
     `smtp` against the fake or a local catcher); AGENTS.md links match.

10. **Server: notifications retention job (LAUN-44)** (added by the review)
    - Scope: `notify.RunRetention` per #10, started in `cmd/server`; bounded
      batches; a try advisory lock; constants in code.
    - Depends on: 1, 3. The pruning queries and constants already came with
      ticket 1; LAUN-44 added the loop, the batching and the lock.
    - Acceptance: each rule of #10 is tested with an injected clock; a run is
      idempotent; unread rows younger than 180 days stay; two instances are
      safe.

## Review notes (automated review, 2026-10-26)

The automated review had 12 findings. All 12 pointed at a real gap and are
fixed above (marked "Changed (review)"); none is rejected. One finding is
disputed in part. One line each:

- (FK cascades): fixed. All member and team FKs are ON DELETE CASCADE (#2, #3, #4, #8 and the rule at the top of the data model).
- (sender skips preference re-check): fixed. #21 re-resolves the preference before sending, so pending mail stops after an opt-out.
- (token and batches/defaults): fixed. The token holds a list of `targets`; a default-resolved value is unsubscribed at the global level with a stored row (#8, #25).
- (stream returns email-only rows): fixed. The stream query filters `inbox` and `archived_at IS NULL` (#31).
- (mark-read gives no stream event): fixed. The stream sends a `count` event whenever the unread count changes (#31).
- (cap counts rows): fixed. The cap counts distinct `batch_id` (#23).
- (outbox rows never claimed): fixed. `state` and `next_attempt_at` are NOT NULL with defaults (#7). The insert contract is now safe by default.
- (retention has no ticket): fixed. Ticket 10 is LAUN-44 (#10).
- (PATCH with COALESCE cannot undo): fixed. `false` clears the timestamp; only a repeat of `true` keeps the first value (#27).
- (read-all not replay-safe): fixed. `before` is required (#27).
- (agent inbox contradicts): fixed. Every notification route answers `403` for an agent token (#14, #27, ticket 5).
- (nullable enums): fixed. All enum columns are NOT NULL; one rule covers every table (top of the data model). Disputed in part: the note's own `kind` and `event` columns already had CHECKs, and the rule is now explicit rather than per column.

## Build notes: ticket 4 (LAUN-34, email transport, sender, unsubscribe)

What the build settled, where the note left room. No migration was needed:
the tables of LAUN-31 hold everything (the number 0012 stays free).

- **Transport (#26).** `login.Message` / `MessageMailer` are new; `TextMailer`
  is unchanged, so `SendText`, `SendLoginCode` and every existing fake keep
  working. `MailSwitch.SendMessage` returns a `*MailError` when the current
  transport is a `TextMailer` only, so a mail is never sent without its
  headers. A custom header with CR, LF, another control or non-ASCII
  character is refused (not stripped); `To`, `Subject`, `Content-Type` and
  the like cannot be replaced; `Message-ID` can (the sender sets it to the
  batch id). The dev log mailer logs the subject only (no address).
- **Claim.** A pass claims at most 50 mails (groups), reading at most 1000 due
  rows. The batch id is set when the rows are claimed, and a retry keeps the
  id its rows already have, so every attempt of one mail has one `Message-ID`.
  A group that holds a retry which is due goes out at once (no new quiet
  window). The claim time of a `sending` row is `next_attempt_at`; a row older
  than 10 minutes returns to `pending` at the start of a pass.
- **Skip reasons.** A deactivated or agent recipient, a recipient who is not a
  member any more, an archived issue or team, and a preference that is now off
  are `disabled`; no address `no_address`; `email_unproven` `unproven`;
  no `PUBLIC_BASE_URL` `no_base_url`; over the cap `rate_cap`; older than 24
  hours `expired`. A group can be split: the rows whose preference is off are
  `disabled` and the others still go out in one mail.
- **Rate cap (#23).** The sender records each result at once, so the count of
  recent mails (distinct `batch_id` of `sent` rows in the last hour) already
  holds the mails of the same pass. For a skipped row `next_attempt_at` holds
  the time of the decision. The "N more updates were not emailed" line counts
  the `rate_cap` rows with a decision time at or after the recipient's last
  `sent_at`, so each skipped row is reported once, in the next mail that goes
  out. A mail that fails to send reports them again on its retry.
- **No mailer during a send.** `ErrNoMailer` from a send (mail turned off in
  that moment) returns the rows to `pending` and counts no attempt.
- **Errors in `last_error`.** The `login.PublicMailError` text only; for an
  error that is not a `*MailError` a fixed sentence, never the error's text.
- **Token.** One token per attempt (a retry makes a new one; unused ones are
  pruned with the others). 32 random bytes, base64url, 43 characters; the
  hash is `sha256` of those 43 characters. A token works once: it is marked
  used in the same transaction that writes the settings, so two posts apply
  it once. A used, expired or unknown token does nothing (and is `204`). A
  team in `targets` that is not in the token's org (or is gone) is skipped.
  The inbox value of the row that is changed is kept.
- **Footer link.** The mail's footer links to the web page
  `PUBLIC_BASE_URL/unsubscribe/<token>` (the confirm page is ticket 6; until
  it ships the `List-Unsubscribe` header is the one-click path).
- **Route.** `POST /api/v1/notifications/unsubscribe/{token}`: no
  `Idempotency-Key` (refused with 400), floor 300 ms, 20 a minute per client
  (when the client address is known under the rules of `clientip.go`) and 600
  a minute in all, then `429 rate_limited`. `GET` and every other method is
  `405`. The request log and the "request failed" log write the path as
  `/api/v1/notifications/unsubscribe/{token}`.
- **Build failure.** If the mail cannot be built (a bad row), the rows stay
  in `sending`, return to `pending` after 10 minutes and are tried again
  until they expire at 24 hours. The error is logged without text.
- **Postmark Message-ID (unverified).** The sender gives `Message-ID` as the
  batch id. Postmark may ignore or replace a custom `Message-ID`; only a fake
  HTTP server was tested. Real mail clients were not tried either.

## Build notes: ticket 5 (LAUN-35, inbox API and live stream)

No migration was needed (the number 0012 stays free).

- **Routes.** As #27. `PATCH` answers `200` with the row after the change.
  `read-all` answers `{marked}`; it marks unread, UNARCHIVED rows with
  `created_at <= before`, so a repeat answers `{marked: 0}`. A `PATCH` id that
  is not a uuid is `404`, like an unknown one. Email-only rows are `404` on
  `PATCH`.
- **Item `data`.** `commentId` (a comment or a mention in a comment) and
  `fromStateName` / `toStateName` (names read by the list query from the
  stored ids); each is absent when the row has none.
- **Cursor.** Base64url JSON of `(created_at, id)`; a malformed one is `400`.
  The page reads one row more than `limit` to know whether a next page exists.
- **Wake-ups.** `PATCH` and a `read-all` that changed rows call
  `Broker.Publish` (this server, at once) and `pg_notify('devops_events', org)`
  (the other servers). A failed `pg_notify` is logged and costs latency only.
- **Stream.** `event: notification` carries `id: <inbox_seq>`; `event: count`
  has no id, so it never moves `Last-Event-ID`. The count is sent once at the
  start of every connection (so a client needs no first `GET unread-count`)
  and then only when it changes. With no resume point the stream starts after
  the caller's highest `inbox_seq` (rows made before it are in the list).
  The membership and token check runs on each poll tick (`SSEPoll`, 10 s), so
  a stream ends within one tick of a removal.
- **Poll interval.** The ticket text said a 60 second poll fallback. The
  stream code polls every `SSEPoll` (10 s by default, the same as the event
  stream). The 60 second poll of `unread-count` is the web app's fallback when
  it cannot open the stream (ticket 7).
- **Agents.** An agent token gets `403 forbidden` on every inbox route, not an
  empty inbox. The AC was changed on purpose after the automated review.
- **Archived list.** `archived=true` has no partial index: the list index only
  covers `archived_at IS NULL`. The query still filters by org and recipient,
  so it is bounded by one person's rows (and retention); add an index only if
  it proves slow.

## Build notes: shipped state (LAUN-39, docs and runbook)

Checked against the merged code on 2026-10-26. Where a call above and this list
disagree, this list is the shipped behaviour.

- **Names.** The consumer is `notify.Consumer` (`Run`, `RunOnce`, `ConsumeOrg`)
  and the sender is `notify.Sender` (`Run`, `RunOnce`); the design said
  `RunSender`. The sender lock is a SESSION try lock
  (`devops:notify-send`), released at the end of each pass. The consumer wakes
  on one broker subscription for every org (`SubscribeAll`), not one per org.
- **Test names (#36, #35).** There is no `TestNotify_AgentsGetNothing`. The
  checks are `TestConsume_AgentsGetNothing`, `TestSender_AgentRecipientIsSkipped`
  and `TestNotify_AgentsAndScopes` (the routes). The no-secrets checks are
  `TestConsume_NoSecretsOrBodiesInLogsEventsOrRows`,
  `TestSender_NoTokenAddressOrBodyInLogsEventsOrRows` and
  `TestUnsubscribe_TokenIsNotInTheLogs`. `TestNoWritePathImportsNotify` enforces
  #37 (no write path imports the package).
- **Sender log (#20).** The sender logs one line per PASS (not per batch) with
  counts: INFO when it sent or skipped something, WARN when a mail failed
  (retry or dead). The dead-letter count is a WARN at boot.
- **Skip reasons (#21).** `no_mail` is allowed by the CHECK but never written:
  with mail not ready the rows stay `pending` and expire as `expired`, as #21
  says. The skip reasons in use are `disabled`, `no_address`, `unproven`,
  `no_base_url`, `rate_cap` and `expired`.
- **Mail availability for the web app.** `mail.available` in
  `GET /me/notification-settings` is the mail switch being ready AND
  `PUBLIC_BASE_URL` being set (`notify.MailReady`), so the settings note shows
  when the sender would skip every mail.
- **Inbox in the web app (#39).** The inbox is a sidebar entry first in the list
  with the unread badge, plus a bell next to Create issue (and in the phone top
  bar) that opens the popover. See `docs/internal/web-decisions.md` (LAUN-37).
  Subscribe control (#40): a visible sentence, not a tooltip (LAUN-38).
- **The issue link in the mail (fixed, LAUN-94).** The mail links to
  `PUBLIC_BASE_URL/<org>/issue/<identifier>` (singular), the web route. It used to
  say `issues` (plural), which is not a web route, so the link landed on the
  not-found page. The settings link (`/<org>/settings/notifications`) and the
  unsubscribe link (`/unsubscribe/<token>`) are real routes. One file,
  `server/internal/notify/testdata/web_routes.txt`, lists the three routes;
  `TestSender_MailLinksMatchWebRoutes` matches a sent mail against it and
  `web/src/mailRoutes.test.tsx` matches it against `App.tsx`.
- **Postmark `Message-ID`.** Unverified against the real service (see the
  ticket 4 notes).
- **Operator tooling.** Dead letters are listed and retried with SQL
  (`docs/deploy.md` "Notifications"); the platform-admin page in #41 is still not
  built. The retry SQL resets `created_at`, because a pending row older than 24
  hours would be expired at once.
