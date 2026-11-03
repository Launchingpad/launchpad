> Internal design note from the Orbit deployment; kept for history.

# LAUN-56: onboarding state, capabilities and org settings

Server part of the first-run checklist (the web checklist is LAUN-25, `docs/internal/web-decisions.md`
"Guided first-run checklist"). Migration 0017. Routes: `GET /orgs/{org}/onboarding`,
`PUT /orgs/{org}/onboarding/skip`, `PATCH /orgs/{org}`. The web switch-over is a separate ticket.

## Decisions

1. **The server computes `done`.** Eight steps, in this order: `organisation`, `mail`, `signups`, `team`,
   `agent`, `import`, `githost`, `invite`. One SQL statement (`OnboardingFacts`) reads the org facts; every
   subquery filters by `org_id`.
   - `organisation`: always done (setup made it).
   - `mail`: mail really goes out now: login can issue codes AND the active transport is Postmark or SMTP.
     The dev log (the `env` provider with no `POSTMARK_*`) writes codes to the log, so it is NOT working
     mail. (The browser guess used `loginAvailable` only; this is stricter on purpose.)
   - `signups`: a platform admin chose the sign-up mode (#4).
   - `team`: some team of the org has cycles on.
   - `agent`: an agent user that is an active member of the org has a PAT that is not revoked, not expired,
     and pinned to this org or to no org.
   - `import`: the org has any `import_mappings` row (web or CLI import).
   - `githost`: the org has an enabled `git_integrations` row.
   - `invite`: more than one active human member (agents and deactivated users do not count).
2. **Skip is org state, not a person's.** Table `onboarding_skips (org_id, step, skipped_by, created_at)`,
   primary key `(org_id, step)`, `org_id` FK with cascade, a CHECK on the step id. All admins of the org
   see and change it. `skipped_by` is the first admin who skipped (a repeat keeps it). A skip never changes
   `done`, and a done step can be skipped. `PUT` is idempotent by shape; the `Idempotency-Key` is mounted
   after the scope check anyway (a replay returns the first answer). A change emits `onboarding.step_changed`
   (entity type `org`, payload `{step, skipped}`) as the last statement; a repeat emits nothing.
3. **What an org admin may learn about the platform: two booleans and nothing else.** The body holds step
   ids, booleans, a skipper's `UserRef`, a timestamp, a count and capability names. No provider name, host,
   address, key state, mode value or secret. `GET /platform/settings` stays platform-admin only.
   `TestOnboarding_NoSecretsOrPlatformSettings` checks the body, the event and the log.
4. **Sign-up mode "chosen".** Column `platform_settings.signup_mode_chosen_at`. Every `PUT /platform/settings`
   carries the whole settings including `signupMode`, so "the PUT carried the mode" cannot be the marker.
   The marker is set (once, never cleared) when (a) the PUT CHANGES the mode, or (b) the PUT sends the new
   optional field `signupModeConfirmed: true` (the web's "keep invites only" button). A mail save that leaves
   the mode alone and omits the field does not set it. Migration 0017 backfills `updated_at` for an install
   whose mode is not `invite` (that was a choice). `GET /platform/settings` shows `signupModeChosen`.
5. **Capabilities** (`capabilities`, strings): `linearImport` when the import manager, the attachment store
   and `PUBLIC_BASE_URL` are all set (else its routes answer 404/503/409); `gitHosts` always (the config
   routes are mounted; webhook mode needs no key); `attachments` when the store is set. A capability says
   the SERVER can do it. The web also needs the page; it shows "Coming soon" until both exist.
   `remaining` does not count `import` / `githost` while their capability is missing.
6. **Banner dismissal stays in the browser.** It is a view preference of one person, not org state, and no
   AC needs it. (Skips are the shared part.)
7. **`PATCH /orgs/{org}`**: `name` (1-200), `timeZone` (an IANA name that `time.LoadLocation` accepts;
   `Local` and path-like values refused; the binary embeds `time/tzdata`), `defaultTeam` (team key or id of
   THIS org; another org's team is `400 validation_failed`; `null` clears). The slug is fixed: an unknown
   field is `400 bad_request`. Admin scope; the org row is locked; a no-op writes and emits nothing; a change
   emits `org.updated` (entity type `org`, payload `{id, slug, changes}`) last. Columns `orgs.time_zone`
   (default `UTC`) and `orgs.default_team_id` with the composite FK `(id, default_team_id)` to
   `teams (org_id, id)` `ON DELETE SET NULL (default_team_id)`. "Defaults" means the default team in this
   ticket; nothing reads either value yet except the web. Avatar is LAUN-22.
8. **`Org` grew `timeZone` and `defaultTeam`** (list and get). `GET /orgs/{org}` now also returns the real
   `createdAt` (it returned the zero time before).
9. **Notifications and MCP are untouched.** `org.updated` and `onboarding.step_changed` are not notification
   events (the consumer ignores them). No MCP tool.
10. **Auth.** All three routes: admin scope AND org admin role (`RequireScope`). A member, a read-only PAT
    and an agent get 403; a user of another org gets 404 (`RequireOrgMember`).

## LAUN-67: an optional ninth step

Eight steps above are the required ones. LAUN-67 adds `products` ("Group your teams into products",
decisions-T18 #32, #37a) as an OPTIONAL step: the server lists it only when the org has two or more teams and no
product, it is not counted in `remaining`, and a skip is stored like any other (migration 0020 changes the CHECK).
`OnboardingFacts` gained `teams` and `has_product`.

## What the web must change (follow-up ticket)

- Replace `computeSteps` inputs with `GET /orgs/{org}/onboarding`; drop the platform, tokens, members and
  teams reads of the checklist and the banner, and the `needsPlatformAdmin` status for mail and sign-up
  (the flags are readable; the LINK to Platform still needs a platform admin).
- Skip and unskip call `PUT .../onboarding/skip`; drop `skipped` from localStorage. Keep `dismissed` there.
- The "Mark reviewed" button for sign-up mode becomes `PUT /platform/settings` with `signupModeConfirmed: true`
  (platform admin only); the `reviewed` local state goes. A non-platform admin sees the step todo with
  "Ask a platform admin", or skips it.
- `capabilities` constant in `onboarding.ts` becomes the `capabilities` list.
- Organisation page: edit name, time zone and default team with `PATCH /orgs/{org}`; regenerate `schema.d.ts`.

## Not tested

- The capability-present path: `linearImport` listed and the import step counted in `remaining` (the test
  harness has no import manager or attachment store).
- Postmark in the mail-working check (SMTP and `none` are tested).
- Two admins skipping the same step at the same instant (the insert is `ON CONFLICT DO NOTHING`).
