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
git clone https://github.com/Launchingpad/launchpad.git
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

`deploy/deploy.sh` is one supported way to run Launchpad. The other is the
compose file in the repository root. It needs no checkout and builds no image.
It pulls the released images from GitHub's registry.

### Quick start

You need Docker with Compose v2.24 or newer, and `curl`. Make a folder and
get the files from the release you want (here `1.2.3`):

```bash
mkdir launchpad && cd launchpad
V=1.2.3
RAW=https://raw.githubusercontent.com/Launchingpad/launchpad/v$V
curl -fsSLO "$RAW/docker-compose.yml"
curl -fsSLO "$RAW/.env.example"
mkdir -p scripts deploy/nginx deploy/backup
curl -fsSL "$RAW/scripts/gen-env.sh" -o scripts/gen-env.sh && chmod +x scripts/gen-env.sh
curl -fsSL "$RAW/deploy/nginx/default.conf.template" -o deploy/nginx/default.conf.template
curl -fsSL "$RAW/deploy/backup/backup.sh" -o deploy/backup/backup.sh
scripts/gen-env.sh        # asks for your two host names and the version
docker compose up -d
```

(The two files under `deploy/` are config files that the compose file mounts
read-only. A `git clone` of the repository also has everything.)

`gen-env.sh` writes `.env` (mode 0600) with random secrets. It does not
overwrite an existing `.env`. It prints no secret. Keep a copy of `.env` off
the host: a lost `SETTINGS_ENCRYPTION_KEY` or `BACKUP_ENCRYPTION_KEY` cannot be
rebuilt. To set the values by hand instead, copy `.env.example` to `.env`, set
every empty value and run `chmod 600 .env`.

`docker compose up -d` pulls the images, makes the data folders, makes the
silo bucket and the app's silo user (the `silo-init` service), starts the
app, the web server and the nightly backup. There is no manual step. Then:

1. Set up the reverse proxy (see above). The stack speaks plain HTTP on
   `127.0.0.1:4001` (web) and `127.0.0.1:4002` (files). Your proxy ends TLS
   for `WEB_HOST` and `FILES_HOST`.
2. Open the first-run setup link from the app log:
   `docker compose logs app | grep 'setup: open'`.
3. Turn on email login and invites (sections 4 and 5 above). Where those
   sections say `deploy/.env`, read `.env`.

### What is in it

- `postgres`, `silo` (the S3 API for attachments), `silo-init` (a one-shot),
  `app`, `web` and `backup`. A one-shot `volume-init` gives the data folders
  their owners.
- The same hardening as `deploy/docker-compose.yml`: ports on `127.0.0.1`,
  read-only root file systems, `cap_drop: ALL`, `no-new-privileges`, internal
  networks, third-party images pinned by digest, no Docker socket.
  `make compose-check` checks the file, and a test compares it with
  `deploy/docker-compose.yml`.
- Its project name is `launchpad-compose`. Never run it as `launchpad` (the
  name `deploy.sh` uses). Do not set `COMPOSE_PROJECT_NAME` or `-p` to
  `launchpad`.
- It does **not** run the `offsite` worker. That worker needs a database role
  that only `deploy.sh` makes. The nightly dump works. For off-site copies,
  copy the backup folder and the files folder to another place yourself.
- `TRUSTED_PROXY_CIDRS`, `GIT_ALLOW_PRIVATE_PROVIDERS` and
  `GIT_ALLOW_PRIVATE_CIDRS` pass through from `.env`, empty by default.
  [`configuration.md`](configuration.md) lists every variable.

### Where the data is

The data is in folders on the host, by default under `./data` (next to the
compose file):

| Folder | Variable | Holds |
|---|---|---|
| `data/postgres` | `LAUNCHPAD_DB_DIR` | the database |
| `data/files` | `LAUNCHPAD_FILES_DIR` | the attachments |
| `data/backups` | `LAUNCHPAD_BACKUP_DIR` | the nightly dumps |

Set `LAUNCHPAD_DATA_DIR` to move all three, or one variable to move one. Use
an absolute path for a disk of your choice. The one-shot `volume-init` sets the
owner of each folder (the database and backup folders to user 70, the files
folder to user 10001). Do not change the owners by hand while the stack runs.

Know these limits of bind mounts:

- **Docker Desktop on macOS.** A folder in your home directory works. A folder
  on a network share or an exFAT disk may have no owners and no reliable
  `fsync`: do not keep the database there.
- **NFS with root squash.** Root in the container cannot change the owner. Set
  the owners yourself (70 for the database and backup folders, 10001 for the
  files folder) and `volume-init` will leave them alone.
- **SELinux.** Add the `:z` label in an override file, or use named volumes.
- **Rootless Docker.** The user ids are mapped. Use named volumes.

If bind mounts do not work for you, use named Docker volumes:
`cp docker-compose.override.example.yml docker-compose.override.yml`. Compose
reads that file by itself. Start with an empty data folder: the override does
not move data that is already in `./data`.

### Update

1. Back up (below).
2. Set `LAUNCHPAD_VERSION` in `.env` to the new version. Read the release
   notes first. There is no `latest`.
3. Run `docker compose pull && docker compose up -d`.

The server applies new database migrations when it boots. There is no
automatic rollback. To go back, restore the backup and set the old version
(see "Rollback" in [`deploy.md`](deploy.md#rollback) for the ideas; the
commands there use `deploy.sh`). If you change a config file that the compose
file mounts (the nginx template or `backup.sh`), add `--force-recreate web`
(or `backup`): compose cannot see a changed file.

To build the images yourself, clone the repository, uncomment the two
`build:` lines in `docker-compose.yml`, set `LAUNCHPAD_VERSION` to any tag
of your choice and run `docker compose up -d --build`.

### Backup and restore

The `backup` service writes `devops-<time>.dump` files (`pg_dump -Fc`) to
`LAUNCHPAD_BACKUP_DIR` every night at `BACKUP_AT_UTC` (03:15 UTC) and keeps
`BACKUP_KEEP_DAYS` days (30). Take one now:

```bash
docker compose exec backup sh /backup/backup.sh once
```

The dumps and the attachments are on the same disk as the database. Copy
`data/backups` and `data/files` to another place, and keep a copy of `.env`.
A dump is readable by user 70 only: copy it as root.


To restore a dump into the stack (the dump must be in `LAUNCHPAD_BACKUP_DIR`;
`<file>` is its name), stop what uses the database, replace the database and
start again:

```bash
docker compose stop app web backup
docker compose exec -T postgres dropdb -U devops devops
docker compose exec -T postgres createdb -U devops devops
docker compose start backup
docker compose exec -T backup pg_restore --no-owner -d devops /backups/<file>
docker compose up -d
```

(`deploy/test-compose-stack.sh` runs these commands on a scratch stack.)
For a new host, make the stack first with the old `.env`, then copy the dump
into `data/backups` as root and run the commands above.

Restore the attachments by copying the old files folder to `LAUNCHPAD_FILES_DIR`
while the stack is stopped, and run `docker compose up -d` (the `volume-init`
service sets the owner; `silo-init` makes the app's user again). Use the old
`.env`: the new stack needs the same `SETTINGS_ENCRYPTION_KEY` and
`BACKUP_ENCRYPTION_KEY`. Test a restore on a scratch stack before you need it.

### Check the stack and remove it

`docker compose config` checks the file and `.env`. `docker compose ps` shows
the state. `docker compose down` stops the stack and keeps the data folders.
`docker compose down -v` also removes the `import_work` volume. It does not
delete the host folders. To delete all data, delete `./data` yourself: that is
the database, the attachments and every backup.

### Images and the release

The release workflow pushes
`ghcr.io/launchingpad/launchpad-api:<version>` and
`ghcr.io/launchingpad/launchpad-web:<version>` for amd64 and arm64 on each
version tag, with the version and the full commit sha as tags and never
`latest`. The version is the tag without the `v`. The images carry OCI labels.
Image signing, SBOMs and a vulnerability scan are future work. The record is
[`decisions-T45-docker-distribution.md`](internal/decisions-T45-docker-distribution.md).

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

## Move to a new server

To change hosts, export the whole install from **Settings**, **Platform**,
**Move server** on the old server, then on the new, empty server (same
release, setup link open) choose **Restore from a move file** on `/setup`.
The page shows a passphrase once: keep it apart from the file.

- Both servers must run the same release. Stop using the old server before
  the final export: writes after the export are not in the file.
- Your own reverse proxy must allow a very large body (no size limit, no
  request buffering, timeouts of an hour or more) on
  `POST /api/v1/setup/restore`, and must not buffer or time out the download
  `GET /api/v1/platform/move/export/*`. The stack's own nginx already does.
  For a huge install use the command line, which needs no proxy:
  `server move-export FILE` on the old host, `server move-import FILE` on the
  new one.
- The passphrase step uses about 256 MiB of memory for a second on each side.
- One app process only: the export ticket lives in memory.
- NOT moved: web sessions (sign in again), git webhooks (edit them to the new
  URL), DNS and TLS, backup destinations (they arrive switched off).

Full steps and the command line: [`deploy.md`](deploy.md#move-to-a-new-server).
On the compose-file-only stack the app root is read-only: put the file in
`/work/import` and run `docker compose -p launchpad-compose exec app
/bin/server move-import ...` (the exact commands are in that section).

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
