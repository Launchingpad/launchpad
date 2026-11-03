# Self-hosting Launchpad

This page shows how to run Launchpad on one server. It is the short guide.
The full runbook, with every option and failure case, is
[`deploy.md`](deploy.md).

> Launchpad is early software and is not ready for production use by other
> people (see the [README](../README.md)). Test your backups and read
> [`SECURITY.md`](../SECURITY.md) before you store real data.

## What you get

`deploy/deploy.sh` starts one stack with Docker Compose:

| Service | What it does | Port on the host |
|---|---|---|
| `web` | nginx: the web app, and a proxy for `/api/` and `/healthz` | 4001 (`DEPLOY_WEB_PORT`) |
| `silo` | S3-compatible file store for attachments | 4002 (`DEPLOY_FILES_PORT`) |
| `app` | the API | none |
| `postgres` | Postgres 16 | none |
| `backup` | nightly `pg_dump`, 30 days kept | none |

Both published ports speak plain HTTP. They bind to `127.0.0.1` by default.

## What you need

- A Linux server with Docker Engine and Compose v2.20 or newer, plus `git`,
  `curl` and `python3`.
- Two DNS names, for example `launchpad.example.com` (web and API) and
  `launchpad-files.example.com` (file store).
- A reverse proxy that you run, which ends TLS (see below).
- Email for login codes: a Postmark account or an SMTP server. Without mail,
  nobody can log in with a code.

## 1. Set up the reverse proxy

The stack has no TLS. Your proxy (nginx, Caddy, HAProxy or another) must:

1. end TLS for `https://launchpad.example.com` and
   `https://launchpad-files.example.com`;
2. forward the web host to port 4001 and the files host to port 4002;
3. pass the `Host` header unchanged (silo checks the request signature
   against it, and nginx answers `421` for any other host);
4. send `X-Forwarded-Proto: https`;
5. allow request bodies of at least 100 MiB on the files host, and not
   buffer or time out `/api/v1/orgs/*/events/stream` (server-sent events).

Set `DEPLOY_BIND_ADDR` to an address your proxy can reach. The default
`127.0.0.1` works only if the proxy runs on the same host as a process, or
with host networking. For a proxy in a container, or on another machine, see
the table in [`deploy.md`](deploy.md#what-runs). On Linux, Docker publishes
ports around `ufw` and `firewalld`, so add a rule in the `DOCKER-USER` chain
or use a firewall in front of the server. `deploy.md` has the rule. Do not
bind to `0.0.0.0` on a host with a public address.

## 2. First deploy

**The host prompts have no default. Type your own host names, or set `WEB_HOST` and `FILES_HOST` first.**

```bash
git clone https://github.com/kraetos10/launchpad.git
cd launchpad
deploy/deploy.sh
```

On a terminal, the script asks for the bind address, the two host names
and the two ports. Press Enter to take a default. The host names have no default, so you must
set `WEB_HOST` and `FILES_HOST` on the first run (type them at the prompt, or export them). To give the answers up front,
export `WEB_HOST`, `FILES_HOST` and the others before the first run, and add
`--non-interactive`.

The script:

- writes `deploy/.env` (mode 0600) with generated secrets. Keep a copy of
  this file in a safe place. Never commit it. Never delete it while the
  stack's volumes exist;
- builds the images, starts the stack and waits until it is healthy;
- runs a smoke test and takes the first backup.

It never prints a secret, except the setup link, on a terminal. For every variable, see
[`configuration.md`](configuration.md).

## 3. First-run setup

While the database has no user, the app logs one setup link at each boot
(valid 24 hours, one use). A first `deploy/deploy.sh` on a terminal ends with
a link, so setup is one command. To get a fresh link later, on your terminal:

```bash
deploy/deploy.sh --setup-link
```

Open the link in your browser. Enter your account, your organisation and
your first team. The page shows the first admin token once: save it in a
password manager. You are signed in when it finishes.

Set `PUBLIC_BASE_URL` correctly (the production stack does it from
`WEB_HOST`), or the link has no host name.

One install is one organisation.

## 4. Turn on email login

Login uses an email one-time code. The deploy script does not ask about
mail. A platform admin sets it in the web app: **Settings**, then
**Platform**. Choose one of:

| Mail provider | Login email goes through |
|---|---|
| `env` (default) | `POSTMARK_SERVER_TOKEN` and `POSTMARK_FROM` in `deploy/.env`; with no token, login is off |
| `none` | nothing: login is off |
| `postmark` | Postmark, with a token stored in the app |
| `smtp` | an SMTP server (STARTTLS, TLS or none), with the credentials stored in the app |

The stored secrets are sealed with `SETTINGS_ENCRYPTION_KEY`, which the
script generates. Do not lose it: back up `deploy/.env`. Use **Send test
email** (`POST /api/v1/platform/mail/test`) to check. Details:
[`deploy.md`](deploy.md#mail-settings).

Until mail works, the setup session (30 days) and the first admin token are
how you get in.

## 5. Sign-ups and invites

A platform admin chooses the sign-up mode in **Settings**, **Platform**:

| Mode | Invites | Public sign-up requests |
|---|---|---|
| `invite` (default) | yes | no |
| `approval` | yes | yes; an org admin approves each one |
| `closed` | no | no |

An org admin makes invites in **Settings**, **Members**, and decides
sign-up requests in **Settings**, **Sign-ups**. If a reverse proxy sits in
front of the stack, read the `TRUSTED_PROXY_CIDRS` part of
[`deploy.md`](deploy.md#per-client-limits-behind-proxies-trusted_proxy_cidrs)
so that the per-client limits do not lock everyone out.

## 6. Agents

To let an AI agent use the tracker, make an agent user and a token (Settings,
Agents). See [`mcp.md`](mcp.md). On the server you can also run
`deploy/deploy.sh --mint-token` (see `deploy.md`).

## Run it from a compose file only

`deploy/deploy.sh` is the supported way to run Launchpad. If you prefer one
compose file and one env file, `deploy/docker-compose.sample.yml` is a
sample. Know these limits first:

- `deploy.sh` does not use it, and `make compose-check` does not cover it.
  It is a hand-made copy of `deploy/docker-compose.yml`. It can drift from
  that file, and no CI job checks it.
- It keeps the same hardening (ports on `127.0.0.1`, no socket mounts,
  digest-pinned third-party images, internal networks, read-only root file
  systems).
- It leaves out the nightly `backup` job and the `offsite` worker, so it makes
  **no backups**. Add your own (see the end of this section).
- It serves plain HTTP only. Your own proxy ends TLS (see "Set up the reverse
  proxy" above).
- Its project name is `launchpad-sample`. Never run it as `launchpad` (the
  name `deploy.sh` uses): its volumes have no deploy label, and a stack with
  the same name would reuse them. Do not set `COMPOSE_PROJECT_NAME` or `-p`
  to `launchpad`.
- `TRUSTED_PROXY_CIDRS`, `GIT_ALLOW_PRIVATE_PROVIDERS` and
  `GIT_ALLOW_PRIVATE_CIDRS` pass through from the env file, empty by default,
  as in the real file. Where steps above say `deploy/.env`, read
  `deploy/sample.env`.

**Docker images.** The images
`launchpad-api` and `launchpad-web` are not on any registry. The sample uses
local tags (`pull_policy: never`), so `docker compose pull` does nothing for
them. You build them on the host that runs the stack.

1. Build the images from a checkout, with one tag of your choice:

   ```bash
   git clone https://github.com/kraetos10/launchpad.git && cd launchpad
   docker build -t launchpad-api:local server
   docker build -t launchpad-web:local web
   ```

   (`deploy.sh` runs `docker compose build app web`, which builds the same
   two contexts and tags them `launchpad-api:<commit>` and
   `launchpad-web:<commit>`.)
2. Go to `deploy/` and run `cp sample.env.example sample.env && chmod 600
   sample.env`. Set every value. The required secrets are empty in the
   example, so compose refuses to start until you fill them. Make each secret
   with `openssl rand -hex 32`. The silo root user and the app's access key
   are user names: any word of 3 or more characters works. Never commit
   `sample.env`.
3. Start it from the `deploy/` directory (the nginx file is a relative path):

   ```bash
   docker compose -f docker-compose.sample.yml --env-file sample.env up -d
   ```

4. Make the silo bucket and the app's silo user, once (script below).
5. Do the proxy setup, first-run setup and mail steps above. The first-run
   setup link is in the app log: `docker compose -f docker-compose.sample.yml
   --env-file sample.env logs app | grep 'setup: open'`.

Step 4 script. `deploy.sh` does this for you; with the sample you run it by
hand, from `deploy/`. It reads the secrets from `sample.env` (in a subshell,
so your shell keeps no copy) and sends the app secret on stdin. It is a
shortened copy of `silo_bootstrap` in `deploy/deploy.sh`, and it is safe to
run again. It prints one line at the end. To check the result later, run
`mcli admin user info local <access key>` inside the silo container.

```bash
(
set -eu
set -a; . ./sample.env; set +a
printf '%s\n' "$S3_SECRET_KEY" | docker compose -f docker-compose.sample.yml \
  --env-file sample.env exec -T \
  -e "B=$S3_BUCKET" -e "K=$S3_ACCESS_KEY" silo sh -c '
set -eu
IFS= read -r app_secret
export MC_CONFIG_DIR=/tmp/.mcli
export MC_HOST_local="http://${MINIO_ROOT_USER}:${MINIO_ROOT_PASSWORD}@127.0.0.1:9000"
mcli --quiet mb --ignore-existing "local/$B" >/dev/null
mcli --quiet anonymous set none "local/$B" >/dev/null
cat >/tmp/p.json <<EOP
{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3:GetBucketLocation","s3:ListBucket"],"Resource":["arn:aws:s3:::$B"]},
 {"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:DeleteObject"],"Resource":["arn:aws:s3:::$B/*"]}]}
EOP
mcli --quiet admin policy create local launchpad-app /tmp/p.json >/dev/null
rm -f /tmp/p.json
printf "%s\n" "$app_secret" | mcli --quiet admin user add local "$K" >/dev/null
info="$(mcli --quiet admin user info local "$K" --json)"
case "$info" in
*"\"launchpad-app\""*) ;;
*) mcli --quiet admin policy attach local launchpad-app --user "$K" >/dev/null ;;
esac
echo "bucket $B (private), user $K with policy launchpad-app"'
)
```

To update, build new images with a new tag, set `LAUNCHPAD_VERSION`, take a
database backup, and run the `up -d` command again. If you edit the nginx
template, add `--force-recreate web` (the sample cannot detect a changed
template).

Take a database backup with the stack running (run from `deploy/`; the file
stays on the host, so keep it somewhere safe):

```bash
docker compose -f docker-compose.sample.yml --env-file sample.env exec -T postgres \
  pg_dump -U devops -d devops -Fc > launchpad-$(date +%Y%m%d).dump
```

Attachments live in the `launchpad-sample_silo_data` volume: copy that too.

### If images are published later (not decided)

Nothing here is decided. A published image would need:

- a registry (GitLab container registry, Docker Hub, GHCR or another);
- a tag rule (a release version and the commit, never only `latest`) and
  multi-arch builds (amd64 and arm64);
- a list of who may push, and a CI job that pushes only from protected
  branches or tags;
- image signing and a build record (for example cosign), and a scan step;
- a rule for the compose file: pin images by digest, as the third-party
  images are today.

Until then, build from source as above.

## Backups

- The `backup` service writes a `pg_dump -Fc` file every night at
  `BACKUP_AT_UTC` (default 03:15 UTC) to the `<project>_backups` Docker
  volume and keeps `BACKUP_KEEP_DAYS` days (default 30).
- `deploy/deploy.sh --backup-now` takes one at once. Every `--update` takes
  one first.
- The volume is on the same disk as the database. Copy the dumps to another
  place. Attachments live in the `<project>_silo_data` volume: copy that
  too. [`deploy.md`](deploy.md#backup-and-restore) has the commands.
- Optional encrypted off-site copies (S3, SFTP, WebDAV, Dropbox) are set up by a platform admin under **Settings**, **Platform**. [`deploy.md`](deploy.md) has the runbook.
- Test a restore: `deploy/restore.sh latest` restores the newest dump into
  a scratch database and counts the rows.

## Update

```bash
deploy/deploy.sh --update
```

It refuses to run on a checkout with uncommitted changes. It takes a
backup, runs `git pull --ff-only`, rebuilds, restarts what changed and runs
the smoke test. The server applies new database migrations when it boots.
There is no automatic rollback: see "Rollback" in
[`deploy.md`](deploy.md#rollback). Then update the MCP binary on each agent machine: run the install command
again ([`mcp.md`](mcp.md)). The binary is a thin client; keep your server on
the same release as the binary, or newer.

## Other commands

| Command | What it does |
|---|---|
| `deploy/deploy.sh --smoke` | run only the smoke test |
| `deploy/deploy.sh --setup-link` | print a new first-run setup link |
| `deploy/deploy.sh --mint-token ...` | make an API token for a member |
| `deploy/deploy.sh --backup-now` | take a backup now |
| `deploy/restore.sh latest` | restore a dump into a scratch database to check it |
| `deploy/deploy.sh --nuke` | delete the deploy and ALL its data (a trial stack only) |

Never run `--nuke`, or `docker compose down -v`, on a stack that holds real
data: it deletes the database, the attachments and every backup.

## If something is wrong

- Look at the logs: `docker compose -p launchpad -f deploy/docker-compose.yml
  --env-file deploy/.env logs --tail=100 app web silo`.
- A 502 behind your proxy usually means the wrong `DEPLOY_BIND_ADDR`.
- `deploy/deploy.sh --smoke` talks to the ports from the server itself, so it
  does not prove TLS, DNS or your proxy. Open the site and log in by hand
  after the first deploy.
