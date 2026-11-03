# GitHub webhook fixtures

These payloads are **hand-written from GitHub's documented `pull_request`
webhook shape** ("Webhook events and payloads", `pull_request`), with invented
repository, user and PR data (repository 4242 `example-org/example-app`, PR
17). They are **not** recorded deliveries: no GitHub hook existed when they
were written (LAUN-5). Only the fields the tracker reads, plus some context
fields, are present.

- `pr_opened.json` is the base. The others were made from it by changing only
  the fields GitHub changes for that action (`action`, `draft`, `merged`,
  `state`, the closing times, `head.sha`).
- `pr_fork_opened.json` and `pr_fork_closed_merged.json`: `head.repo` is a
  different repository (7777). `pr_deleted_fork_opened.json`: `head.repo` is
  null, as for a deleted fork. Open point 16 of decisions-T17: such a delivery
  is answered `200 ignored`.
- `ping.json` is the event GitHub sends when a hook is made.
- After the first real deliveries, follow the T5 cutover step 7 for GitHub:
  copy one real delivery per action from the hook's Recent Deliveries, scrub it
  (names, emails, avatars, repository data), and compare it with these files,
  or replace them.
- Never paste an unscrubbed real payload here.

## Check events (LAUN-85)

`check_suite_*.json` (`check_suite`) and `status_*.json` (`status`) are
hand-written from GitHub's documented payload shapes, with invented data (the
same repository 4242 and the head sha of `pr_synchronize.json`). Only the fields
the tracker reads, plus some context, are present. Not recorded deliveries.
