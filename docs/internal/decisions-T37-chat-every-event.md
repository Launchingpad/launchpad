# Decisions T37: Chat services post every ticked event (LAUN-208)

The project owner moved a ticket to Done through an agent token and no chat post came, with
`state_change` ticked on their service. The cause was the rule in
`decisions-T33-slack-channel.md` #3: a post needs at least one eligible person
(an active member, not an agent, not the actor) who would be notified. The issue
had no assignee and no follower, and an agent made the change, so nobody was
eligible. The project owner decided on 2026-11-01 (on the ticket) to drop that rule for chat
services, and that mute stays mute. The mute meaning for a shared channel below
is the default proposed on the ticket; change it with a new record.

## What this supersedes

1. **T33 #3, the eligible-person rule, is superseded for chat services only.**
   Also superseded for chat services: the "agents get nothing" part of T33 #17
   and T36 #7 as it reads for posts. A chat post needs no recipient. An agent
   still gets no notification, and an agent token still gets `403` on the
   `/chat-services` routes (T33 #17, first sentence, is unchanged).
2. **Email, inbox and push are unchanged.** The recipient filters of
   `decisions-T13-notifications.md` #14 (actor, active non-agent member, mute,
   follows, personal settings) still decide who gets a notification, an email
   and a push. The consumer's notification code is not touched; tests assert
   the same notifications, mail rows and push rows for the same events.

## The rule now

3. **A service posts each kind it has ticked that the event made, once per
   event.** The kinds come from the event, not from people:
   - `new_issue`: an issue is created (no follower is needed).
   - `assigned`: an issue is created with an assignee, or an update sets an
     assignee. Clearing an assignee posts nothing.
   - `state_change`: an update changes the state.
   - `comment`: a comment is created on an issue.
   - `mention`: keeps its meaning. It posts when a text names an eligible person
     (active, not an agent, not the actor) who was not already notified of that
     mention. A mention of nobody eligible posts nothing.
   - `project_update_due`: keeps its meaning (the job's lead must still be an
     eligible person).
   The actor may be a human or an agent. One row per service, kind and event;
   the dedupe key is still `chat:evt:<seq>:<kind>`, so a replay makes no second
   post.
   The per-recipient precedence of T13 #13 (one kind per recipient per event)
   no longer applies to chat: an event posts every ticked kind it made. A comment
   that also @mentions someone posts both `comment` and `mention`; a created,
   assigned issue posts `new_issue` and `assigned`.
4. **Mute stays mute, for the shared channel: the issue posts nothing if ANY
   active member has muted it.** A channel has no single muter, so the rule is
   the quietest reading: one mute silences the issue for every service and every
   kind, a direct mention included (the personal mention exception of T13 #9 is
   a per-person rule and does not apply to a shared channel). "Active member"
   means the same as T13 #14: in the org, not deactivated, not an agent. A mute
   by a removed or deactivated person, or by an agent, is ignored. Removing the
   mute (or the muter) lets the issue post again. Cost: one member can silence an
   issue for the channel; the Settings page says so.
5. **Kept from T33 and T36:** the event list per service (nothing posts until an
   admin ticks a kind); the fixed sentence, identifier and link (no title, name
   or ticket text, so an agent's change reads the same); dedupe; the sender, the
   hourly cap and the fair per-pass rule (10 rows per org per pass, round robin
   per service, 200 in all) are not changed. The hourly cap is LAUN-210. Teams
   (a webhook per team) are LAUN-209.
6. **No schema change, no write path imports `internal/notify`.** The change is
   in the consumer's chat selection. It reads the muters' eligibility with the
   existing `ListNotifyEligibleUsers` query. Nothing is logged or stored that
   was not before.

## Honest limits

- Every issue event of every team now posts, including events nobody cares
  about, so a busy org gets more posts (the cap and the fair pass bound the
  rate). Team restrictions (T33, honest limits) still apply.
- One mute by any active member silences an issue for the channel.
- The Settings test button never applied the old rule, which is why it worked
  when real events did not.
