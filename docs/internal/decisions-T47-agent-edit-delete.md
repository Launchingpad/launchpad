# Decisions T47: edit and delete an agent (LAUN-334)

Settings, Agents gets two row actions: "Edit access" (the team access of the
agent) and "Delete". Extends `decisions-T10-agents.md` and
`decisions-T20-team-access.md`.

1. **Edit access reuses what exists.** The Agents page opens
   `TeamAccessDialog` (the Members page opens the same one) and calls
   `PUT /users/{user}/team-access` (T20 #48). Nothing changed on the server.
   The saved note shows on the Agents page. An agent is never an org admin,
   so the dry run, the open-issue choice and the webhook guard work as for a
   member.
2. **Route.** `DELETE /api/v1/orgs/{org}/agents/{user}` (`user` = id or
   username, like the team-access route). Admin scope; a web session needs the
   CSRF header as for any write. `204`, no body.
3. **Agents only, and a human is `404 not_found`.** A human, an unknown name
   and another org's user give the same answer, so the route never confirms
   that a human exists and never turns into a way to remove a person. (`400`
   would say "this user exists but is human".)
4. **An agent that is a member of another org is `409 conflict`.** The row
   is install-wide (`users`); the route would delete it in that org too.
   `POST /agents` never makes such an agent, so this is a guard only.
5. **No `Idempotency-Key`.** The route does not mount `s.idem`; a repeat is
   `404`, which is the true answer.
6. **What goes.** One `DELETE FROM users`. Every foreign key to `users` and
   `org_members` was read in `db/schema.sql`. CASCADE: `api_tokens` (the
   tokens), `org_members`, and through the membership `team_members`,
   `issue_subscriptions`, `team_follows`, notification settings, inbox rows
   (`notifications`, `notification_emails`), push devices, personal webhooks,
   git identities, view preferences; also `idempotency_keys`, `login_codes`,
   `user_preferences`. SET NULL: `issues.assignee_id` and `creator_id`,
   `comments.author_id`, `documents.creator_id`, `projects.lead_id`,
   `project_updates.author_id`, `attachments.creator_id`, `issue_activity`,
   `issue_state_changes`, `issue_relations`, `events` actors, notification
   actors, the `*_by` audit columns. No composite `(org_id, user_id)` FK is
   SET NULL (that would try to null `org_id`), so no migration was needed.
   `TestAgents_Delete` proves it with an agent that owns rows in these tables.
7. **Lock.** The transaction takes the team access lock (T20 #65) first, like
   the other member writes, and locks the member row. It does not run the
   webhook guard: removing a member cannot leave a restricted member beside
   an unmapped repository.
8. **No event.** There is no member event type (T10 #5), and the issues'
   assignee going to null is not an `issue.updated` (the FK does it). The web
   refetches after the call: members, tokens, issues, issue details, comments
   and projects (`useDeleteAgent`). Another open tab sees the change on its
   next refetch. Open event streams of the deleted agent end at their next
   page recheck.
9. **Audit line** `audit: agent deleted` with the org, the actor, the user id
   and the username. Never a token or any secret.
10. **Web confirm text** says: tokens are removed, issues and comments stay
    with no author or assignee, it cannot be undone. Cancel is the default
    focus. Errors: `403`, `404` and `409` have fixed texts.
