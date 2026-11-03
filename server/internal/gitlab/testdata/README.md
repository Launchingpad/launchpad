# GitLab webhook fixtures

These payloads are **hand-written from GitLab's documented merge_request
webhook shape** ("Merge request events" in GitLab's webhook docs), with
invented project, user and MR data (project 42 `example-org/example-app` on
`gitlab.example.com`). They are **not** recorded deliveries: no GitLab hook
existed when they were written (no hostname yet).

- `mr_open.json` is the base; the others were made from it by changing only
  the fields GitLab changes for that action (`action`, `state`, `state_id`,
  `draft` / `work_in_progress`, `changes`, merge fields).
- Once a real hook exists, follow "Turning it on" in `docs/internal/decisions-T5.md`:
  copy one real delivery per action from the hook's Recent events, scrub it
  (names, emails, avatars, project data), and compare it with these files,
  or replace them.
- Never paste an unscrubbed real payload here.

## Pipeline events (LAUN-85)

`pipeline_*.json` ("Pipeline Hook") are hand-written from GitLab's documented
pipeline payload shape, with invented data (project 42, the head sha of
`mr_open.json`). Not recorded deliveries. The payload has `created_at` and
`finished_at` but no `updated_at`; the tracker uses `finished_at` when set.
