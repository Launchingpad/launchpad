# Decisions T34: connected accounts, a per-user git identity (LAUN-199)

A member tells Launchpad their handle on each git connection of the org
(Settings > Connected accounts, from LAUN-108). A change request that comes in
by webhook from that handle then shows the member. The defaults below were
chosen by the orchestrator; the project owner was not asked. Change them with a new record.

1. **Scope of v1.** One thing: a per-user link of a git handle per connection
   (GitHub, GitLab or Bitbucket host row in `git_integrations`). No OAuth. No
   third-party token, no email and no password is asked for or stored. The user
   types the handle they have on that host.
2. **A link is a claim.** Nobody proves they own the handle. So the link is read
   for ONE purpose: display attribution of a change request (the `authorUser`
   field of the issue "Changes" block). It is never read for a permission, a
   scope, a notification, an assignment or a state move. Removing it takes no
   access away; having it gives none. A test pins that the issue read gives the
   same answer to every member.
3. **Data.** Table `git_identities` (migration 0032): `org_id`, `user_id`,
   `integration_id`, `handle` (as typed) and `handle_key` (lower case; a CHECK
   keeps `handle_key = lower(handle)`). Composite foreign keys
   `(org_id, user_id)` to `org_members` and `(org_id, integration_id)` to
   `git_integrations`, both `ON DELETE CASCADE`: a member who leaves the org, or
   a connection that is deleted, takes its links with it. Two unique keys:
   `(org_id, user_id, integration_id)` (one handle per member per connection)
   and `(integration_id, handle_key)` (one holder per handle per connection).
4. **Handle rules, per host** (API 400 `validation_failed`; trimmed, one leading
   `@` dropped, compared in lower case):
   - GitHub: `[A-Za-z0-9][A-Za-z0-9_-]{0,38}` (1 to 39). A `[bot]` account is
     not a person and is refused.
   - GitLab: `[A-Za-z0-9_][A-Za-z0-9_.-]{0,99}` (1 to 100).
   - Bitbucket: `[A-Za-z0-9_][A-Za-z0-9 _.:@-]{0,99}` (1 to 100). The webhook
     author is the nickname or the account id (`557058:...`), so a space and a
     colon are allowed.
   100 is the length of `mr_links.author`, the text the match is made against.
5. **Who may claim, hijack and squat.** The first claim of a handle on a
   connection wins; a second claim by another member is `409 conflict` with the
   fixed sentence "that handle is already linked on this connection; ask an org
   admin if it is yours". It names nobody, but it does tell the caller that SOMEONE in the org linked that handle (a status-code oracle). That is accepted: the routes need org membership and a handle is a public host name, so the only fact leaked is that a colleague linked it. It is not "leaks nothing". It does not touch the second member's
   own existing link (the upsert is one statement). A member cannot take a
   handle over, because the holder never changes without the holder's or an
   admin's own act. The rejected cost of this rule is squatting: a member can
   claim a handle that belongs to someone else, and the real owner then gets the
   409. The cure is the org admin: Settings > Connected accounts shows an admin
   every link, and an admin removes one (`DELETE /git-identities/{id}`). A
   squatter can also be pointed at a person by a wrong claim; the damage is
   limited to the name shown on a Changes row (see 2).
6. **Routes.** All in `router.go` and `openapi.yaml`.
   - `GET /orgs/{org}/me/git-accounts` (read): the org's enabled connections
     (and a disabled one the member has a link on), each with the member's
     handle or null.
   - `PUT /orgs/{org}/me/git-accounts/{integration}` (write) body `{handle}`:
     link or change. `DELETE` the same path (write): unlink; a missing link is
     204. A connection of another org is `404 not_found`.
   - `GET /orgs/{org}/git-identities` and `DELETE /orgs/{org}/git-identities/{id}`
     (admin scope AND the org admin role).
   The three `/me` routes use `refuseAgents`: an agent has no email, cannot log
   in and has no account on a git host, so it gets `403 forbidden` (the same rule
   as the notification, views and sessions routes). PUT and DELETE are replay-safe
   by shape, so no `Idempotency-Key` is mounted (as the views routes). Nothing
   calls a git host, so the git rate limiter does not apply. The rows a member
   can make are bounded by the number of connections, and one handle per
   connection; the body is small and strict (unknown fields are `400`).
7. **Events.** The `/me` routes write a user-private row: no event (as view
   preferences and notification settings, decisions-T13 #32). The admin unlink
   changes another member's state, so it emits `git_identity.removed` as the
   last statement of its transaction: entity type `org`, the org id as entity id,
   payload `{userId, integrationId}`. The payload never holds the handle. The
   web client refreshes the lists and the open issue on it.
8. **Attribution.** `ListIssueMRLinks` joins `git_identities` on the SAME org and
   the SAME connection and `handle_key = lower(mr_links.author)`, then `users`
   where the user is active. A link on another connection with the same text
   attributes nothing. A deactivated member shows as the plain host login. The
   host login stays in the API (`author`) and in the screen reader text.
   `authorUser` is a `UserRef` or null.
9. **Web.** A page for every person (group Account, next to Sessions): a row per
   connection with the handle field, Link / Change / Remove, a fixed sentence
   for every failure, no server text. For an org admin a second section lists
   every link with a confirmed Remove. The Changes row shows the member's name
   and avatar in place of "by login". It works at 390 px (the field wraps; the
   admin table scrolls inside its card) and uses the theme variables for dark
   mode.
10. **Live update.** After a change the client refreshes the list, the admin list
    and the issue details. The event of item 7 refreshes another admin's and the
    unlinked member's views. A member's own change in a second tab is a
    user-private row with no event; it shows after a reload or the next load
    (the query is never cached).

## Honest limits

- **No per-user rate limit.** The repo's per-user limiter (`mintLimiter`) is bespoke
  with fixed constants, so reuse is not cheap. The writes are bounded instead: one
  row per member per connection, a small strict body, and no host call. The 409
  oracle above can be probed by a member at normal request speed; this is accepted.
- **Disabled connection.** A PUT on a turned-off connection is `409 conflict` with a
  fixed sentence and stores nothing; an unlink still works. The page disables Link.
- **ASCII only.** The attribution join compares only an ASCII author login, so a
  login with U+212A (Kelvin sign), which `lower()` folds to `k`, cannot match.
- **Disclosure.** The page says the link is a claim, and the Changes row marks the
  member's name "linked by the member, not verified" (tooltip and screen reader text).

- A claim is not a proof. Squatting is possible (5). The only check is the org
  admin. No commit author email is stored, so no email match is possible and
  none is added: the rule is "do not store an email you do not already have".
- Only the change request author is attributed. Commits and branch pushes are
  not stored with an author today (`mr_links.author` is the only author text), so
  nothing else can be attributed without a new store.
- Attribution is by the login at the time of reading. A host that renames a
  login breaks the match until the member updates the handle.
- Bitbucket Server and Cloud name the author differently (slug, nickname,
  account id); the member types the one the webhook sends.
- No browser run was made for the page layout in this ticket.

## Rejected

- **OAuth per host** to prove the handle. It stores a third-party token and needs
  an app registration on every host. Not in v1.
- **Verify by a code in the profile or a gist.** Needs a host call and the
  netguard path for every user-typed host. Not in v1.
- **Match by commit author email.** Not stored; not added.
- **Last claimant wins / steal-by-claim.** Lets any member hijack another's
  attribution. Rejected for the unique key.
- **Show who holds a taken handle** in the 409. It leaks membership. Rejected;
  the admin list is the place for that.
- **One global handle per user.** A person has different handles on different
  hosts, and one host can have several connections.

Migration 0032 may need renumbering at merge: develop is at 0030 and the Slack
ticket takes 0031.
