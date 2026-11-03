# Bitbucket webhook fixtures

These payloads are **hand-written from Bitbucket's documented webhook shapes**
with invented data. They are **not** recorded deliveries: no Bitbucket hook
existed when they were written (LAUN-84), and no live host was called. Only the
fields the tracker reads, plus some context fields, are present.

## Cloud (`cloud_*.json`)

Repository `example-org/example-app`, workspace uuid
`{11111111-2222-3333-4444-555555555555}`, repository uuid
`{aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee}`, pull request 17.

- `cloud_pr_created.json` is the base (`pullrequest:created`). It has **no
  `draft` field** on purpose: a missing field is not a draft. The other
  `cloud_pr_*` files change only what Bitbucket changes for that event
  (`state`, `updated_on`, `source.commit.hash`).
- `cloud_pr_created_draft.json`: `draft: true` (newer payloads only).
- `cloud_pr_fork_created.json`: `source.repository` is another repository.
  `cloud_pr_deleted_fork_created.json`: `source.repository` is `null`.
  Open point 16: both are answered `200 ignored`.
- `cloud_status_*.json`: `repo:commit_status_created` / `_updated`.
  `cloud_status_href_only.json` has no `commit.hash`, only `links.commit.href`.

## Data Center (`dc_*.json`)

Repository id 4242, `PROJ/example-app`, pull request 17.

- `dc_pr_*.json`: `pr:opened`, `pr:from_ref_updated`, `pr:modified`,
  `pr:merged`, `pr:declined`, `pr:reviewer:approved`, `:unapproved`,
  `:needs_work`. `dc_pr_opened.json` has no `draft` field.
- `dc_build_*.json`: Data Center's build status event. **Its event name
  (`repo:build_status`) and its payload shape are UNVERIFIED**; they are the
  builder's best reading of the documentation, and the parser is tolerant of the
  commit being a string or an object. Replace them with a real delivery.

## After the first real deliveries

Copy one real delivery per event from the hook's delivery log, scrub it (names,
emails, avatars, repository data) and compare it with these files, or replace
them. Never paste an unscrubbed real payload here.
