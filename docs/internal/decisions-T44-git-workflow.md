# T44: Git workflow rules (LAUN-244)

The project owner, 2026-11-02: "the git workflows should be customisable, i.e. a merged PR
may go to Done, an opened MR may move to Testing?" This record says how the
moves worked before LAUN-244, which part is now a team setting, and the rules
that keep a setting safe.

## How the moves work before LAUN-244 (kept as the built-in rules)

Read from `internal/scm/scm.go` (`Event.Plan`), `internal/tracker/mr_state.go`
(`linkAndMove`, `updateLinks`, `nextLinkForLinkPlan`) and
`internal/tracker/gitlab_webhook.go` (`moveForward`, `isForwardMove`), and from
`decisions-T5.md` #16 to #21 and `decisions-T17-git-connections-import.md` #16.

1. **The events and their moves.** One parsed event (GitLab, GitHub, Bitbucket
   Cloud and Data Center) has a neutral action:
   - `opened` and `reopened`, not a draft: link, move to the team's
     `review_state_id` (In Review).
   - `opened` and `reopened` of a draft: link, no move.
   - `ready` (GitHub `ready_for_review`, GitLab `changes.draft` true to
     false): link, move to the review state.
   - `merged` (GitHub `closed` with `merged: true`, GitLab `merge`, Bitbucket
     `fulfilled` / `pr:merged`): link, move to `testing_state_id` (In Testing),
     a draft too.
   - `closed` without a merge (Bitbucket `rejected` / `pr:declined`): the
     existing links become `closed`; no move.
   - `updated`, `drafted`, review events and check events: no move.
   Bitbucket sends no `reopened` and no `ready`.
2. **Which state.** The target is a state ID on the team row: `review_state_id`
   and `testing_state_id`, set to In Review and In Testing when the team is made
   (`CreateTeamTx`). No route changes them. Not by name and not by state kind.
   A team with no anchor (the FK is `ON DELETE SET NULL`) moves nothing.
3. **Forward only** (`isForwardMove`). An issue in a `completed` or `canceled`
   state never moves. Else it moves only to a later state kind (triage <
   backlog < unstarted < started < completed < canceled) or, within one kind, to
   a later board position. Nothing moves into an archived state, and an archived
   issue never moves.
4. **Which issues.** Only issues named in the source branch or the title move;
   an issue named only in the description is linked and keeps its state. A
   repository mapped to a team links and moves only that team's issues.
5. **Stale and replayed events.** A delivery key seen before is `duplicate`
   and writes nothing. A merged link is terminal: a later open, update or close
   changes nothing. A closed link opens again only on an explicit reopen. An
   event older than the stored host time is stale and moves nothing. A second
   `merged` event under a NEW delivery key (or a backfill read of an open
   change) still runs the move: it is forward only, so it is safe, but it moves
   an issue forward again after a person moved it back.
6. **Who.** A move writes an `issue_state_changes` row with `source` = the
   provider and no actor, and an `issue.updated` event with no actor and
   `source` and `mergeRequest` in the payload. A connected account
   (decisions-T34) is display attribution of the change request only; it is
   never the actor of a move.

These six points stay true for every team that has no rule, and for every event
of a team that has no rule for that event.
`TestGitWorkflow_BuiltinPinned` (unit) pins point 1 and 2 against
`scm.Event.Plan`; the existing webhook tests (`TestGitlabWebhook_*`,
`TestGithubWebhook_*`, `TestBitbucketWebhook_*`) pin the rest, and
`TestGitWorkflow_DefaultSecondMergeMovesForward` pins the last sentence of
point 5.

## Decisions

1. **A rule per team and event.** Table `team_git_rules` (migration 0045):
   `(org_id, team_id, event, target_state_id)`, primary key `(team_id, event)`,
   composite FKs `(org_id, team_id)` to `teams` and `(team_id,
   target_state_id)` to `workflow_states`, so a rule can only name a state of
   its own team. `target_state_id` NULL is "no move". The optional "only from"
   set is `team_git_rule_from_states`, one row per state, with the same FKs. No
   org-wide default: a new team starts with the built-in rules.
2. **The events** are those the parsers already know and that may move an
   issue: `opened`, `ready`, `reopened`, `merged`, `closed` (closed without a
   merge). Approvals, checks, updates and branch events are not rule events
   (the ticket's "keep the set small"). A draft that opens or reopens never
   moves, under any rule: a draft is not ready for review (T5 #16).
3. **A missing rule is the built-in move.** A PUT carries the whole set of
   custom rules; an event that is not listed keeps the built-in move, and an
   empty list is "reset to defaults" (it deletes every row). So "reset" gives
   exactly the behaviour of a team that never had a rule.
4. **Safety.**
   - A rule with no from set moves **forward only**, with the built-in check
     (`isForwardMove`): never out of a completed or canceled state, never
     backwards. This is stricter than the ticket's "any non-completed state";
     it is chosen so that a rule equal to the built-in move behaves the same
     (open question 1).
   - A rule with a from set moves an issue only when its current state is in
     the set, in any direction. Only such a rule can move an issue out of Done
     or Canceled, and only when it lists that state.
   - Nothing moves into an archived state; an archived issue never moves; only
     issues named in the branch or title move (T5 #21), the close rule too.
5. **Once per event.** A rule moves an issue only when the event changed its
   link: a new link, a link state change (opened, closed, merged, reopened), or
   a draft marked ready. A redelivery under the same key is `duplicate` (as
   before). A second `merged` under a new key, a close of a closed link or a
   backfill that reads an open change again changes nothing on the link, so it
   moves nothing. The built-in rules keep point 5 above unchanged.
6. **The close rule.** A close went through `updateLinks`, which never moved.
   It now moves an issue when the team has a `closed` rule with a target, the
   link changed from opened to closed, and the event names the issue in its
   branch or title. The move event carries `changes: ["state", "sortOrder",
   "changeRequests"]`, as a move of an open or merge does.
7. **Removed and renamed states.** A rule stores the state id, so a rename
   changes nothing. No route deletes a state; a team delete removes its states
   and its rules in the same statement, so the FKs are plain (NO ACTION). A
   state that is archived (the importer can do that) stays in the rule: GET
   answers `problem: "state_archived"` for that rule, the webhook does not move
   to it, and a PUT that names an archived state is `400 validation_failed`, so
   the admin must choose another state. A built-in anchor that is archived is
   flagged the same way. An archived "only from" state flags the rule with
   `problem: "from_state_archived"`: the rule still moves from its other
   states, a PUT that keeps the archived state is `400`, and the web page
   names the row and leaves the archived state out when it saves (with none
   left, the rule moves forward only).
8. **Who and what.** A move under a rule is the same write as a built-in move:
   history row with the provider as source and no actor, `issue.updated` with
   `source` and `mergeRequest`, notifications as for any state change. The
   actor stays empty: the T34 connected account is a claim and never acts.
9. **Routes.** `GET /orgs/{org}/teams/{team}/git-workflow` (read scope, any
   member who sees the team; a hidden team is `404`, Test B sample) and `PUT`
   on the same path (admin scope). Both refuse agent tokens with
   `refuseAgentsWith` and the route's own words (the ticket: agents none). The PUT mounts `s.idem` after the scope check; the
   middleware stores POST only, so a key on this PUT is accepted and not stored,
   and the PUT is idempotent by shape. Launchpad has no team admin role
   (decisions-T20 "Per-team roles" is out of scope), so "team admins" are the
   org admins.
10. **Validation.** `rules` is required (an empty list resets). Each event at
    most once; `stateId` is a required key (null is "no move"; a rule without
    the key is `400`); `stateId` and every `fromStateIds` entry a non-archived state of
    the team, else `400 validation_failed` (a state of another team or another
    org is the same 400, AGENTS.md); `fromStateIds` at most 50 and no repeats; a
    rule with no move takes no from set.
11. **Event.** A change emits `team.updated` with `changes: ["gitWorkflow"]`,
    the team id and key, and the actor, scoped to the team, as the last write of
    the transaction (as the team PATCH does). The payload never carries the
    rules. A PUT of the same rules emits nothing. The web refetches the rules on
    `team.updated`.
12. **One snapshot of the rules.** A rule and its "only from" set are read
    in ONE statement (`ListTeamGitRules`, an `array_agg` over the from rows),
    so a webhook that runs while an admin PUTs reads either the old set or the
    new one, never half of each. No lock is added on the webhook path, so its
    lock order (rank locks, issue rows, link rows) does not change.
13. **The backfill tally.** The backfill reads open changes and applies them
    as `opened` only, so it never runs a close and never moves on one. Its
    `moved` count therefore covers every move it can make. A close moves only
    through a live webhook, whose answer carries no count (decisions-T20 #65).
14. **Webhook routes.** Unchanged: outside `Authenticate`, nothing in front of
    the secret or signature check, no new path. The rules are read inside the
    event's transaction, after the delivery row (one or two extra indexed reads
    per team the event touches).
15. **MCP.** No MCP tool lists integration or team settings, so there is no
    tool change. `get_guidance` is not changed either.
16. **Web.** Settings, Administration, "Git workflow" (admin page, as every
    integration page): one card per team, one row per event with a state
    picker ("Built-in: <state>", every live state, "No move"), a preview line,
    a warning on a rule whose state is archived, Save and "Reset to defaults".
    The Integrations repository help line and the README say the real mapping.

## Open questions for the project owner

1. A rule with no from set moves forward only (decision 4). This is the
   chosen default (2026-11-02); the ticket's default was "any non-completed
   state". With that default, a rule could move an In Testing issue back to
   In Review on a reopen, and a rule equal to the built-in move would behave
   differently from it. To be confirmed by the project owner.
2. A draft that opens never moves (decision 2). Should a team be able to move
   on a draft open too (a sixth event, `opened_draft`)?
3. Approved and checks-failed events are not rule events yet. Add them?
