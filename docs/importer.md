# Linear importer: command line

`cmd/importer` copies a Linear workspace into Launchpad. Rules and every
judgment call: `docs/internal/decisions-T4.md` and `docs/internal/decisions-T4b.md` for the
files and the Linear team / cycle ids. **The web app is the main way now**
(Settings, Import from Linear; runbook `docs/deploy.md`, "Web Linear import"). The command line below stays for an offline export and as a
fallback; both run the same `internal/importer`.

- **The Linear key is `LINEAR_API_KEY` in the environment, nothing else** (for
  this command line; the web path takes the key once in the body of the export
  request and never stores it, see `docs/deploy.md`, "Web Linear import").
  Read it from your secret store inside the command and pass it to docker
  BY NAME (`docker run -e LINEAR_API_KEY ...`), so it is never echoed, logged
  or written to a file.
- **It is read-only.** The client refuses any GraphQL mutation before a
  request is made. Never add one.
- Temp and export files go in a scratch directory you choose (`--out`, `--in` and `--org` have no default: always pass them). An export holds every ticket's text and user emails (files are
  0600); it is not a repo file.
- Run it in Docker (or with a local Go). Mount the export directory, and for
  `import` / `verify` join the compose network of YOUR stack:

## Flags

Commands: `export`, `import`, `verify` (`verify` takes the `import` flags).
Taken from `go run ./cmd/importer <command> --help`. Environment:
`LINEAR_API_KEY` (export), `DATABASE_URL` (import, verify), `S3_*` (see below)
and `PUBLIC_BASE_URL` (default of `--public-base-url`).

| Command | Flag | Default | Meaning |
|---|---|---|---|
| export | `--out DIR` | none, required | Export directory. |
| export | `--no-files` | off | Do not download `uploads.linear.app` files. |
| export | `--max-file-mb N` | 100 | Largest file to download, in MB. |
| export | `--max-total-gb N` | 10 | Stop downloading after this many GB. |
| import, verify | `--in DIR` | none, required | Export directory. |
| import, verify | `--org SLUG` | none, required | Org slug of the target. |
| import, verify | `--user-map FILE` | none, required | User map; start from `usermap.template.json` and replace every `CHANGE_ME`. |
| import, verify | `--dry-run` | off | Import in a transaction and roll it back. |
| import, verify | `--report FILE` | none | Also write the full report (every exclusion) as JSON. |
| import, verify | `--skip-files` | off | Store no file and leave `uploads.linear.app` URLs as they are (no `S3_*` needed). |
| import, verify | `--public-base-url URL` | `$PUBLIC_BASE_URL` | API origin that download URLs in texts are written on; empty writes paths. |
| import, verify | `--counter-floor TEAM=N` | none | Minimum next issue number for a team; repeat for more teams. |

There is no default counter floor: an older build had one for a private team
key, and it is gone. Pass `--counter-floor` only when you need to keep issue
numbers above a number you set.

```bash
cd server
# A shell function, so flags go BEFORE the image and paths with spaces survive.
gorun() {  # usage: gorun [docker flags...] -- go args...
  local flags=()
  while [ "$1" != "--" ]; do flags+=("$1"); shift; done; shift
  docker run --rm "${flags[@]}" -v "$PWD:/src" -w /src \
    -v "$EXPORT_DIR":/work \
    -v dev-ops-gomod:/go/pkg/mod -v dev-ops-gocache:/root/.cache/go-build \
    golang:1.27.3 "$@"
}
# 1. export (about 3 minutes and 200 MB of files for a large workspace). The key is passed by name only.
# (LINEAR_API_KEY is already exported in this shell, from your secret store.)
gorun -e LINEAR_API_KEY -- go run ./cmd/importer export --out /work/export
# 2. a user map: start from /work/export/usermap.template.json (Linear user id -> a Launchpad username; replace every CHANGE_ME)
# 3. dry run, then the import (DATABASE_URL of the target; the server must have booted on it)
#    The files go to the attachment store: the server's S3_* variables, and
#    PUBLIC_BASE_URL (the API origin the download URLs in texts are written on).
gorun --network <project>_default \
  -e "DATABASE_URL=postgres://devops:devops@postgres:5432/devops?sslmode=disable" \
  -e S3_ENDPOINT=http://silo:9000 -e S3_BUCKET=devops-attachments \
  -e S3_ACCESS_KEY=devops -e S3_SECRET_KEY=devops-local-s3-secret \
  -e PUBLIC_BASE_URL=http://localhost:8090 -- \
  go run ./cmd/importer import --in /work/export --org SLUG --user-map /work/usermap.json --dry-run
#    ... the same without --dry-run; then `verify` with the same flags
```

- `--dry-run` prints create / update / skip counts and rolls back. A real
  run verifies the counts and **exits 3 on a mismatch** (2 is a usage
  error, 1 any other error). A second run with the same export changes
  nothing.
- **Cutover order** (decisions-T3 #35): import while cycles are OFF, then
  `PATCH /api/v1/orgs/SLUG/teams/KEY {"cyclesEnabled": true}` (your org slug and team key). The importer
  refuses to run if a cycle number it brings is already used.
- **Never run a real (non-dry) import into a shared or hosted database**
  without the maintainer. Use your own compose stack (`-p <name>`) for trials.
- **Files (T4b).** `import` uploads every exported file to the store once
  (outside its transaction; a re-run finds the objects and sends nothing),
  writes a `file` attachment row for it and rewrites every
  `uploads.linear.app` URL to the row's download URL. `verify` then expects
  `file` rows, their objects in the store (`file_object`) and no text left
  on Linear (`linear_upload_url` = 0, imported rows only); any difference
  exits 3. With no `S3_*` the import stops; `--skip-files` is the T4
  behaviour (no upload, URLs left as they are) and is **refused once the
  org has imported files** (it would put the Linear URLs back). **`PUBLIC_BASE_URL` is written into the texts**:
  import with the production origin, or re-run once it is known (the texts
  are then updates).
- **Linear team and cycle ids resolve in the API** (T4b): wherever a team
  or a cycle is taken by id (paths, `team` / `cycle` filters, `team` and
  `cycleId` in issue bodies), the Linear id of an imported one works, in
  its own org only, through `import_mappings`.

