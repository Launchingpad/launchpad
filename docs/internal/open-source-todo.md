# Release checklist (public export)

Short list to check before each public export (`docs/releasing.md`). The
first-release scrub (LAUN-185) is done; what stays is what to re-check.

## Before every export

- [ ] `scripts/export-public.sh` ends with `audit-public.sh: no hits`.
- [ ] `--gitleaks` finds nothing (pinned image, `--no-git`).
- [ ] `make docker-test`, the web checks and `make compose-check deploy-test
      deploy-lint` pass in the exported tree.
- [ ] No new `ORB-nnnn` ticket id, real host name, personal e-mail address or
      `/Volumes` path in code, tests or docs outside `docs/internal` and the
      shipped migrations.
- [ ] `git config user.name` / `user.email` are the identity for the public
      commit.

## Kept on purpose

- The label key `dev.orbit.deploy=production` on the deploy volumes: the
  project guard in `deploy/deploy.sh` refuses volumes without it, so renaming
  would break existing installs.
- `dev-ops` / `devops` names (compose project of the dev stack, database and
  role, Go module path): renaming needs a data migration of installs.
- The org `orbit`, team `ORB` and the name Orbit in tests, fixtures and the
  `dev-setup.sh` / `smoke.sh` defaults: neutral sample data.
- Shipped migrations keep their ticket-id comments (never edited).
- `docs/internal` keeps ticket ids (LAUN-n, ORB-n) as history.

## Open

Blocks the first push (do these before `git push -u github master`):

- [x] A read of `docs/internal` for anything private that no grep can find
      (names of customers or colleagues, plans). LAUN-187: the four largest
      notes were read in full and the rest were swept by grep; chat quotes, the
      private CI wiring and live host state were removed. A human skim of the
      three largest files (`web-decisions.md`, `decisions-T16-backups.md`,
      `decisions-T22-native.md`) is still recommended before the first push.
- [x] The MCP binary and variables were renamed to `launchpad-mcp` /
      `LAUNCHPAD_*` before the first release (LAUN-188). Nothing had shipped,
      so there is no compatibility shim.
- [x] The git identity for the public commit is set (the repository's local git config
      was changed to a public address on 2026-10-30).
- [ ] A fresh export passes the audit, gitleaks and the tests in the exported
      tree (see "Before every export").

Does not block the first push (can follow):

- Decide how changes move between the private and the public repository
  after the first push.
- The GitHub steps in `docs/releasing.md` "After the first push" (private
  vulnerability reporting, squash-only merges, protection of `master`).
- Pin the `actions/*` versions in `.github/workflows/ci.yml` to commit SHAs
  after the workflow has run once.
