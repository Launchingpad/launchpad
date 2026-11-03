# Decisions T39: A state-change chat post names the two states (LAUN-214)

The project owner asked (2026-11-01) that a chat post for a state change say from which
state to which state, for example `Issue changed state: In Progress → In
Review`, and not only `Issue changed state`.

## What this changes

1. **T33/T36 "a fixed sentence, an identifier and a link" gets ONE addition:**
   a `state_change` post carries the OLD and the NEW state name. A state name
   is org data an admin sets; it is not an issue title and not a person. Still
   never in a post: an issue title, a person's name, comment text.
2. **Text:** `[LAUN-12] Issue changed state: <old> → <new> - <link>` (Slack:
   `<link|open in Launchpad>`; Discord and Fluxer: `<link>`; custom: the plain
   link, and the JSON `text` holds the same line). The custom body gets no new
   field. All four kinds go through the one builder, `ChatText`.
3. **Both names or neither.** If either name cannot be found, or cleans to nothing (only format characters, or only marks on Slack), the post is the old
   fixed sentence `Issue changed state: <link>`. Other kinds are unchanged.
4. Email, inbox and push text are unchanged. Mute still mutes (the consumer
   makes no row for a muted issue, so no name is read).

## Where the names come from

5. The `issue.updated` event already carries `fromStateId` and `toStateId`
   (ids only; events stay ids only). The outbox row already holds `event_seq`.
   The sender reads those two ids from the event (`GetChatEventStateIDs`, by
   org and seq) and the names from `workflow_states` in the SAME org
   (`ListChatStateNames`, `org_id = row.org_id AND id = ANY(...)`). No migration.
6. **The outbox stores no text.** A row holds ids only; the post text is built
   when the row is sent and goes to the service, nothing else. So no name is
   stored by notify; TTL (24 h for a waiting row, 30 days for a finished one)
   is unchanged.
7. **Renamed between the change and the post:** the post shows the CURRENT
   name (names are resolved at post time). A post that waits a retry shows the
   name of the retry.
8. **Deleted, or the event is gone:** a state that no longer exists, an event
   that was pruned, or an id that is not a UUID gives the fixed sentence
   without names (rule 3). An archived state still has a row and still resolves.
9. **Cross-org:** the lookup filters by the row's org, so a state id of another
   org never resolves; the post falls back to the fixed sentence (rule 3).
   A cross-org test covers it.

## Escaping and size

10. A name is cleaned first for every kind: Unicode format characters (Cf: bidi
    overrides and isolates such as U+202E and U+2066, zero-width marks) are
    dropped, control characters and line breaks
    become one space, runs of space collapse, `@` and `://` are broken with a
    zero-width space (no mention, no link forms), and a name over 40 runes is cut
    to 39 plus `…`.
11. Then per kind. Slack: `& < >` become entities (so `<!channel>`, `<@U1>`
    and `<url|text>` stay text) and `* _ ~` and the backtick are removed (Slack
    mrkdwn has no escape). Discord and Fluxer: the markdown escapes of the
    identifier (`\ * _ ~ ` | > [ ] <`), and the body already sets
    `allowed_mentions.parse = []`. Custom: `<` and `>` become `‹` and `›`.
12. **Logs:** notify never logs a state name; the tests check the log buffer.

## Honest limits

- A name is visible to everybody in the channel. An admin who puts a secret in
  a state name shows it there. The 40-rune cut limits the damage only.
- A state of the same org but another team still resolves (the check is the
  org, as for every other lookup).
- Slack pings only through `<!...>` and `<@...>` forms, which are neutralised;
  the zero-width space after `@` is added anyway.
- Slack auto-links bare domains (`evil.example.com` with no scheme). The `://`
  break only stops a scheme link, so it is a weak control. The post is plain
  text in a channel that already holds the admin's own link.
- The 40-rune cut counts runes, not graphemes. A flag emoji or a combining mark
  can be split at the cut. Accepted.
- On Slack the marks `* _ ~` and the backtick are deleted BEFORE the `@` and
  `://` breaks, so deleting cannot join a link back together.
