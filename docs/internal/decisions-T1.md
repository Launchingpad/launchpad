> Internal design note from the Orbit deployment; kept for history.

# T1 decisions (ORB-3405)

Where `docs/architecture.md` left a gap or needed a small correction, T1 made
the smallest sensible choice. Each one is listed here so a later tranche can
change it on purpose.

**0001 was edited after the first review (ORB-3405 review round 1).** It had
not shipped to any database outside tests, so the fixes went into
`0001_baseline.up.sql` / `.down.sql` directly, and `db/schema.sql` and
`internal/dbq` were regenerated. From the first deployment on, 0001 is frozen
and every change is a new migration.

## Schema

1. **Not every table has `org_id`.** `orgs` and `users` are global (a person
   can be in several orgs through `org_members`). `api_tokens` belongs to a
   user; its `org_id` is the optional pin, not an owner. `idempotency_keys`
   is per user (a key is the caller's, whatever the org). Every other table
   has `org_id`.
2. **Composite foreign keys for tenancy.** Every org-scoped table has
   `UNIQUE (org_id, id)`, and references between org rows are
   `(org_id, x_id) REFERENCES t (org_id, id)`. A row cannot point into
   another org even if a query forgets a filter. Nullable references use
   `ON DELETE SET NULL (col)` (Postgres 15+) so `org_id` is never nulled.
   **State references are team-scoped.** `workflow_states` has
   `UNIQUE (team_id, id)`; `issues.state_id` is the foreign key
   `(team_id, state_id)`, and a team's default / review / testing states are
   `(id, x_state_id)`. A state of another team is refused by the database;
   the service also checks it (CreateIssue, resolveState) to answer 400.
3. **Two tables added to the doc's list.**
   - `org_event_counters`: per-org event seq. See "Change feed" below.
   - `project_teams`: projects belong to several teams, as in Linear. T3 needs it.
4. **Columns added to `teams`:** `default_state_id` (new issues start there;
   seeded to Backlog) and the cycle cadence (`cycles_enabled`,
   `cycle_length_days` 14, `cycle_start_weekday` 0 = Sunday,
   `cycle_start_time` 00:00, `cycle_timezone` Europe/London,
   `cycle_rollover` true) for T3.
5. **`db/schema.sql` is a generated snapshot, not a boot baseline.**
   another internal service applies `schema.sql` on a fresh boot; here the server runs the
   migrations from `0001`, and `schema.sql` exists for review and for drift
   checks (`make schema-check`). sqlc reads `db/migrations/` directly, so the
   generated code cannot lag a migration.
6. **`uuidv7()` is plpgsql** on `gen_random_uuid()` (no pgcrypto). It is not
   monotonic within a millisecond. Postgres 18's built-in one will shadow it.
7. An internal guideline says an agent should not design schemas. The
   ticket requires the baseline, so the schema was written to the
   `supabase-postgres-best-practices` rules instead: every foreign key is
   indexed, `timestamptz` and `text` throughout, CHECK constraints for every
   enum-like column, partial indexes for sparse references, keyset indexes
   `(org_id, updated_at, id)` and `(org_id, created_at, id)`. It still wants
   a human review before production.

## Workflow and issues

8. **State types** (the doc names the states, not their types). Linear's
   types: Backlog `backlog`; Todo `unstarted`; In Progress, In Review, In
   Testing `started`; Done `completed`; Canceled and Duplicate `canceled`.
9. **Timestamps from the state type.** Entering `started` sets `startedAt` if
   empty. `completed` sets `completedAt` (and `startedAt` if empty) and clears
   `canceledAt`. `canceled` sets `canceledAt` and clears `completedAt`.
   `backlog` / `unstarted` / `triage` clear all three.
10. **History on create.** Creating an issue writes an `issue_state_changes`
    row with `from_state_id` NULL, so burn-up (T3) sees the starting state.
11. **`gitBranchName` user.** The doc says `{username}/...` but not whose.
    T1 uses the assignee's username, or the caller's when the issue is
    unassigned. Today Linear gives the viewer's name; for assigned
    tickets the result is the same `admin/orb-...` form.
12. **PATCH cannot move an issue to another team** (that renumbers it). A
    `team` field is rejected as unknown.
13. **List description cut** is 400 characters (runes), with a
    `descriptionTruncated` flag so a client knows to fetch the full text.
14. **`createdAt` / `updatedAt` filters** are a lower bound. `-P1D` and `P1D`
    both mean "since one day ago", as in Linear. A timestamp or a date also
    works. Archived issues are hidden unless `includeArchived=true`.
15. **`query`** is a case-insensitive substring match on title and
    description (LIKE metacharacters escaped), plus an exact identifier
    match. It is a scan within one org, which is fine at a small org's size; add
    `pg_trgm` in a later migration if it gets slow.
16. **An unknown list parameter is a 400**, so `asignee=me` fails loudly
    instead of returning every issue.
17. **A PATCH that changes nothing** does not bump `version` and writes no
    event.

## Relations, comments, labels

18. **Relation types on the wire:** `blocks`, `blockedBy`, `related`,
    `duplicateOf`, `duplicates`. Only `blocks`, `related` and `duplicate_of`
    are stored; `blockedBy` and `duplicates` are the reverse rows. A relation
    whose reverse already exists is a 409 (no A-blocks-B and B-blocks-A).
    The unique index `issue_relations_pair_key` on
    `(type, LEAST(issue_id, related_issue_id), GREATEST(...))` enforces this,
    so two concurrent creates of a pair, in either direction, cannot both
    insert; the loser's unique violation maps to 409. The pre-check stays only
    for a clearer message.
19. **Comments:** only the author may edit (an edit touches one comment, no
    cascade). Delete is a hard delete that removes every reply by FK cascade,
    so it is judged on the WHOLE subtree:
    - A **moderator** (org admin role AND an admin-scoped token) may delete
      any comment. An admin's write-scoped (agent) token is not a moderator.
    - Anyone else may delete only a comment whose root and every reply, at
      any depth, they wrote. One foreign reply anywhere below makes it 403,
      with nothing deleted and no event. (Round 1 checked only the root, so
      an author could cascade away other users' replies; fixed in round 2.)
    - It emits one `comment.deleted` per removed comment (the root's payload
      lists `deletedReplyIds`, each reply's has `cascadedFrom`).
    - **Tree lock.** Every reply insert and every delete take one org-scoped
      advisory transaction lock (`lockCommentTrees`). So the subtree a delete
      reads is exactly the one it cascades: a racing reply either commits
      first (and is then checked and gets its event) or waits and finds its
      parent gone (400 `unknown parentId`). An FK violation on
      `comments_parent_fk` also maps to that 400. The cost is that reply
      creates and deletes in one org run one at a time; root comments skip
      the lock.
20. **Labels** are unique per org ignoring case. The `label` list filter
    takes label NAMES only (comma-separated). Request bodies (`labels`,
    `addLabels`, `removeLabels`) take names or ids. So a label name may not be
    a UUID or contain a comma.

## Auth

21. **Scopes are cumulative:** admin implies write implies read. The admin
    scope also needs the org `admin` role, so a token never grants more than
    its owner has.
22. **Seed tokens are pinned to `orbit`.** (Superseded: ORB-3432 removed the seed, `decisions-T8-first-run.md`.) admin: read, write, admin. claude:
    read, write. They are minted only in the run that creates the user, so a
    revoked seed token never comes back on restart.
23. **Token management covers tokens pinned to the org in the path.** Tokens
    made through `POST /orgs/{org}/tokens` are always pinned to that org, so
    an org admin can never mint or see a credential for another org.
    Unpinned tokens have no API yet (an account screen can come with T6).
    **No impersonation:** `user` may name only the caller or an agent user
    (`is_agent`, e.g. `claude` for reviewer and MCP tokens). A token for
    another human is 403, because the admin would hold that person's secret.
24. **Session tokens (T6, ORB-3410):** the schema has `kind = 'session'` and
    `auth.TokenSource` is the extension point. The bearer source accepts only
    `pat`; T6 adds a cookie source for `session` and must add CSRF checks and
    CORS for it.
25. **Cross-org responses:** a path reference to another org's row is
    `404 not_found` (and an org the caller is not in, or that a pinned token
    is not pinned to, is the same 404). A reference inside a request body is
    `400 validation_failed`.
26. **Team creation needs the admin scope**; labels, issues, comments and
    relations need write; moderating others' comments needs admin (#19).

## Change feed

27. **Per-org seq in commit order.** A global identity column would let a
    lower seq commit after a higher one, and a client polling `after=N`
    would skip it for ever. `NextEventSeq` upserts the org's counter row and
    holds that row lock to COMMIT, so per org seq order is commit order. It
    is the last write of each transaction to keep the lock short. The cost
    is that writes in one org serialise for that last moment, which is fine
    for a tracker.
28. **No events for token create or revoke**, so credentials activity is not
    broadcast to every member's stream.
29. **SSE:** with no `Last-Event-ID` and no `after`, a stream starts at the
    newest event. The handler wakes on NOTIFY and also polls every 10
    seconds, and sends `: ping` every 15 seconds. It clears the server read
    and write timeouts and sets a 30-second deadline per write.
    **On every poll tick it re-checks the credentials**: the token (revoked,
    expired), the user (deactivated), a pin and the org membership. When any
    is gone it writes `: credentials no longer valid` and ends the stream. So
    an open stream outlives a revocation by at most one poll interval (10s).

## Idempotency

30. `Idempotency-Key` applies to the POST creates of teams, labels, issues,
    comments and relations. The key is per user and kept 24 hours (an hourly
    sweeper deletes expired keys). The request hash covers method, path and
    body.
    - **POST /tokens never uses it.** Its response holds the token secret, so
      it must not be stored or replayed; a request with the header gets 400.
      As a second line of defence, a response body with an `odt_` secret is
      never stored by any route.
    - **The scope check runs first.** The middleware is mounted per route,
      after `RequireScope`, so a stored response is replayed only to a token
      allowed to make that request. (Before the fix it ran before the scope
      check, and a read token of the same user could replay a write route's
      stored body.)
    - A 4xx is stored and replayed, as Stripe does. A 5xx, 401 or 403 is not
      stored; its claim is released only if nothing committed.
    - **Crash safety.** `database.InTx` sets `idempotency_keys.committed_at`
      in the same transaction as the request's write. An in-flight claim with
      no `committed_at` older than 60 seconds (`Options.StaleAfter`, measured
      on the DATABASE clock that stamped `created_at`) is taken over by a
      retry, which then runs. A claim with `committed_at` set and no stored
      response (a crash after commit, a failed store, a 5xx after commit) is
      never re-run: a retry gets `409 idempotency_outcome_unknown` and must
      read the resource. A failed store therefore keeps the key.
    - **A slow first request cannot double the write.** If the first request
      was only slow (still running after the window), its commit marker
      `UPDATE ... WHERE id = $claim AND committed_at IS NULL` then hits 0
      rows, `InTx` returns `ErrIdempotencyClaimLost` (409) and the whole
      transaction rolls back. The marker row-locks the claim, so a takeover
      that starts after it waits and then sees `committed_at` set. So "never
      re-run" holds for every write that goes through `InTx` (all
      idempotent POST routes do). The limit: a handler that wrote outside
      `InTx` would not be covered; none does, and AGENTS.md requires it.

## Issue hierarchy

34. **Re-parenting is serialised per org.** A PATCH that sets `parent` takes
    an org-scoped advisory transaction lock BEFORE it locks the issue row,
    then runs the cycle check. Without it, A.parent=B and B.parent=A racing
    each other deadlock in Postgres (each holds its own row and the parent
    FK check wants the other's), which surfaced as a 500; and in a longer
    chain the two cycle checks could both pass. The lock order matters: the
    reverse order deadlocks.

## Deferred to T6 / follow-ups

Found in review, accepted for T1, and to be decided or fixed later.
**T6a (ORB-3414) decided items 1 and 4 below and decision 35, and built
decision 24's cookie source: see `docs/internal/decisions-T6a.md`.**

1. **Token lifetime policy.** Minted tokens have no default expiry, and an
   admin token that expires can mint one that does not. Before T6, decide:
   cap a minted token's expiry at the caller's own, and require an expiry
   for API-made agent tokens.
2. **Cycle-check depth bound.** The parent cycle check walks at most 100
   ancestors. Before the importer (T4), either fail closed at the bound or
   walk unbounded with `CYCLE` detection.
3. **Serialisation failures are not retried.** `40P01` (deadlock) and
   `40001` (serialization) surface as 500 with no retry.
4. **Listener backoff never resets.** After a good connection, the LISTEN
   loop keeps its last backoff for the next reconnect.
5. **Re-auth only on the poll tick.** Stream credentials are re-checked on
   the poll tick, not before a NOTIFY-woken drain, so a revoked token can
   still receive events for up to 10 seconds (decision 29).
6. **Idempotency hash is over raw bytes.** Equal JSON with other spacing or
   key order is a different request (409 `idempotency_key_reused`).
7. **No length limit on team and label `description`.**
8. **smoke.sh passes the token on curl's command line,** so it is visible in
   `ps` while the script runs.
9. **A body read error in the idempotency middleware is reported as 413,**
   whatever the cause.

## Known, left for T6

35. **Member emails are visible to every member.** `GET /orgs/{org}/users`
    returns `email` to any member, including read-scoped (reviewer) tokens.
    It is NULL for everyone today. Decide before T6 stores OTP login emails
    whether to hide it from non-admins or from tokens.
36. **Seed tokens are printed in plaintext to the app log, on purpose** (superseded: ORB-3432 logs a one-time setup link instead, `decisions-T8-first-run.md`)
    (the design asks for it, and there is no other way to hand over the
    first credential). AGENTS.md says to rotate both after first boot.

## Local dev and CI

31. **Port 8090 can be taken on a dev machine** by another service. The
    compose ports are overridable (`DEV_OPS_APP_PORT`, `DEV_OPS_PG_PORT`); the
    defaults stay as the doc says. CI uses postgres on 55433.
32. The Go module path is `devops`.
33. CI runs on merge requests and pushes to `develop` (as briefed). CI
    review automation is not part of the public tree (`docs/releasing.md`).
