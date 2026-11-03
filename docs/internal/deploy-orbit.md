> Internal design note from the Orbit deployment; kept for history.

# Launchpad deploy: Orbit-specific parts

The generic runbook is `docs/deploy.md`. This file holds what only the first
deployment used: its hosts (shown here with example names) and the Linear
import cutover. Note: `--in`, `--out` and `--org` have no default.

## Orbit hosts

- Web and API: `launchpad.example.com` (port 4001). Files: `launchpad-files.example.com` (port 4002).
- Org `orbit`, first team `ORB`: the importer, the MCP config and the GitLab webhook path name them.

## Linear import cutover (not part of deploy)

The import is a separate, one-time step that the project owner runs. **Do not** run a
real import into the hosted database without the project owner (`docs/importer.md`). Order:

1. Deploy, complete first-run setup (org `orbit`, team `ORB`), log in once
   on the web, and make the `claude` agent ("Agent users and their tokens",
   step 1). The user map needs an org admin and `claude`.
2. Back up: `deploy/deploy.sh --backup-now`.
3. Export from Linear on a workstation (`docs/importer.md`, step 1), and copy the
   export directory to the server (it holds every ticket and user email;
   keep it 0600 and delete it afterwards).
4. Build the importer once (needs the internet), then run it. It needs
   the database (on the stack's `backend` network, which is internal) and
   silo (only on the `edge` network) to upload the exported files, and
   `PUBLIC_BASE_URL` for the download URLs it writes into the texts. So the
   container is created on `backend`, connected to `edge` as well, then
   started. `edge` is an ordinary bridge network with a route to the
   internet: for this one-off run the importer could reach the internet;
   it makes no such call (an import reads only the export directory). The
   values come from `deploy/.env` (the bucket-scoped app key, not the silo
   root key) and reach docker BY NAME (`-e NAME`), never on a command
   line:

   ```bash
   cd launchpad/server
   docker run --rm -v "$PWD:/src" -w /src -e CGO_ENABLED=0 golang:1.27.3@sha256:b76020929b709ddb6202afc05410a0faefae20ae05e27ad548b3870092688a69 \
     go build -o bin/importer ./cmd/importer
   cd ../deploy
   umask 077
   envv() { grep "^$1=" .env | cut -d"'" -f2; }
   P=$(envv DEPLOY_PROJECT)
   export DATABASE_URL="postgres://devops:$(envv POSTGRES_PASSWORD)@postgres:5432/devops?sslmode=disable"
   export S3_ENDPOINT=http://silo:9000 S3_BUCKET="$(envv S3_BUCKET)"
   export S3_ACCESS_KEY="$(envv S3_ACCESS_KEY)" S3_SECRET_KEY="$(envv S3_SECRET_KEY)"
   export PUBLIC_BASE_URL="https://$(envv WEB_HOST)"
   EXPORT=/path/to/export-dir     # holds export/ and usermap.json
   imp() {   # imp import|verify FLAGS...
     docker create --name launchpad-importer --network "${P}_backend" \
       -e DATABASE_URL -e S3_ENDPOINT -e S3_BUCKET -e S3_ACCESS_KEY -e S3_SECRET_KEY -e PUBLIC_BASE_URL \
       -v "$PWD/../server/bin:/opt/importer:ro" -v "$EXPORT:/work:ro" \
       alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc /opt/importer/importer "$@" >/dev/null || return 1
     if ! docker network connect "${P}_edge" launchpad-importer; then
       docker rm -f launchpad-importer >/dev/null; return 1
     fi
     local rc=0
     docker start -a launchpad-importer || rc=$?
     docker rm -f launchpad-importer >/dev/null
     return "$rc"
   }
   imp import --in /work/export --org orbit --user-map /work/usermap.json --dry-run
   ```

   **If you interrupt a run** (Ctrl-C, a lost terminal), the container is
   left behind, and even a stopped container keeps `DATABASE_URL` (with the
   database password) and `S3_SECRET_KEY` in its configuration
   (`docker inspect` shows them). Remove it at once, and before the next
   `imp` (which otherwise stops on the name clash):
   `docker rm -f launchpad-importer`.

5. Check the dry-run counts, then the real run and the check, in the same
   shell (exit 3 is a count mismatch), and drop the secrets from it:

   ```bash
   imp import --in /work/export --org orbit --user-map /work/usermap.json
   imp verify --in /work/export --org orbit --user-map /work/usermap.json
   unset DATABASE_URL S3_ACCESS_KEY S3_SECRET_KEY
   ```
6. Turn cycles on AFTER the import:
   `PATCH /api/v1/orgs/orbit/teams/ORB {"cyclesEnabled": true}`.
7. Back up again, and run `deploy/restore.sh --compare latest`.
8. Then the rest of T7: agent tokens and `.mcp.json` ("Agent users and
   their tokens", steps 2 and 3) with `https://$WEB_HOST`, the GitLab webhook (T5) to
   `https://$WEB_HOST/api/v1/orgs/orbit/integrations/gitlab/webhook`, and
   Linear kept read-only.

