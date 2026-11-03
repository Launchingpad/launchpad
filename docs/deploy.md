# Launchpad production deploy

This is the runbook for the production stack in `deploy/`. One command
starts it; one command updates it. Internal names (image tags, database)
stay `dev-ops` and `devops`. The production compose project is `launchpad`;
the name `dev-ops` belongs to the local dev stack. The host names come from
`deploy/.env`, so a rename is a change to the settings, not to the code.
The MCP server binary is `launchpad-mcp` and its variables start with
`LAUNCHPAD_` (see `docs/mcp.md`).

## What runs

| Service | Image | Published | Notes |
|---|---|---|---|
| `web` | built from `web/` (nginx 1.27, digest-pinned) | `DEPLOY_WEB_PORT` (4001) -> 8080 | The web app. Proxies `/api/` and `/healthz` to `app` on the same origin. |
| `silo` | `pgsty/silo:RELEASE.2026-09-16T00-00-00Z@sha256:…` | `DEPLOY_FILES_PORT` (4002) -> 9000 | The S3 API for presigned uploads and downloads. No console. |
| `app` | built from `server/` (alpine 3.20, digest-pinned) | none | The API. `LOG_ENV=production`, fixed in the compose file. |
| `postgres` | `postgres:16.15-alpine@sha256:…` | none | Volume `<project>_postgres_data`. Internal network only. |
| `backup` | the postgres image | none | Nightly `pg_dump -Fc` to volume `<project>_backups`, 30 days kept. |
| `offsite` | the `app` image (`/bin/server offsite`) | none | The off-site backup worker. User 70, read-only root, no capability. Reads dumps from `<project>_backups` read-only, keeps scratch files in `<project>_offsite_work` (mounted at `/offsite-work`). Networks `offsite_db` (internal, postgres only) and `egress` (the way out). Database role `offsite`. |
| `volume-init` | the postgres image | none | One-shot. Gives the silo and backup volumes to their non-root users. No network, only `CHOWN`, and the one service without `no-new-privileges` (see "Security notes"). |

```
browser --https--> outer proxy --http--> :4001 nginx (web) --/api/--> app:8080 --> postgres
                                 \-http--> :4002 silo (S3 API) <------ app (http://silo:9000)
```

Both published ports speak plain HTTP. **The outer proxy is yours.** It
must:

1. terminate TLS for `https://$WEB_HOST` and `https://$FILES_HOST`;
2. forward `$WEB_HOST` to port 4001 and `$FILES_HOST` to port 4002;
3. **pass the `Host` header unchanged.** silo checks the SigV4 signature
   against it, and nginx answers `421` to any other host;
4. send `X-Forwarded-Proto: https` (nginx passes `http` or `https` on, and
   nothing else);
5. allow request bodies of at least 100 MiB on `$FILES_HOST` (the
   attachment limit), and not buffer or time out `/api/v1/orgs/*/events/stream`
   (Server-Sent Events) for under an hour.

The two ports bind to `DEPLOY_BIND_ADDR`, default `127.0.0.1`, so the
default fails closed: nothing off this host reaches them. **Check how your
proxy runs before the first deploy**, because the right address depends on
it:

| The outer proxy runs | `DEPLOY_BIND_ADDR` |
|---|---|
| as a process on this host (nginx, Caddy, HAProxy from the OS packages) | `127.0.0.1` (the default) |
| in a container on this host with host networking (`network_mode: host`) | `127.0.0.1` (the default) |
| in a container on this host on a plain bridge network (Nginx Proxy Manager, Traefik or a Caddy container started with `docker run` or compose) | NOT `127.0.0.1`: inside that container, 127.0.0.1 is the container itself. Use the gateway address of the proxy's bridge network (`docker network inspect <network> -f '{{(index .IPAM.Config 0).Gateway}}'`, often `172.17.0.1`), and point the proxy at that address. The LAN address also works, with the firewall rule below. Both paths are **not measured** on Linux: run the in-proxy check below. |
| as a Docker swarm service on an overlay network (CapRover's nginx: `captain-overlay-network`) | NOT the overlay network's gateway: it is not an address of this host, so `up` fails with "cannot assign requested address". Use this host's LAN address plus the firewall rule below. **Not measured** on Linux: run the in-proxy check below. (The gateway of `docker_gwbridge`, the host-side bridge of swarm containers, may also work; not measured.) |
| on another host | this host's LAN address, plus the firewall rule below |

With the wrong address, the site answers 502 behind the proxy while the
smoke test still passes: the smoke test connects from this host, not from
the proxy.

**For a proxy in a container on a plain bridge network on this host, the
bridge gateway address is the safest bind.** That address lives on the
Docker bridge, so the proxy container reaches it, and machines on the LAN
or the internet normally have no route to it.

**Every container proxy path is not measured on Linux,** the gateway one
and the LAN-address one alike (CapRover included): the packet is DNATed on
the host, then forwarded from the proxy's bridge into the stack's bridge,
through Docker's isolation chains. Check it from INSIDE the proxy container
before you rely on it, with the bind address and the literal web host name
(here read from `deploy/.env`):

```bash
H=$(grep '^WEB_HOST=' deploy/.env | cut -d"'" -f2)
A=$(grep '^DEPLOY_BIND_ADDR=' deploy/.env | cut -d"'" -f2)
docker exec <proxy-container> wget -q -O- --header "Host: $H" "http://$A:4001/healthz"
# want {"status":"ok"} (use curl -H instead of wget if the image has curl)
```

The health route is `/healthz` (proxied by nginx; there is no
`/api/v1/health`). For CapRover and other swarm proxies, and for a proxy on
another host, bind to the LAN address and add the firewall rule.

**A host firewall does not cover those ports on Linux.** Docker publishes a
port with its own iptables/nftables rules, which come before the INPUT
chain that ufw and firewalld manage, so `ufw deny 4001` has no effect. Use
Docker's `DOCKER-USER` chain, or a network or cloud firewall in front of
the host. `DOCKER-USER` sees the packet AFTER Docker's DNAT, when the
destination port is already the container port (8080 or 9000), so a plain
`--dport 4001` never matches. Match the original port through conntrack,
and only for packets that arrive on the host's EXTERNAL interface (`-i`,
for example `eth0`). The `-i` keeps the rule off two kinds of traffic: this
host's own containers connecting out to port 4001 or 4002 of another
server, and a proxy container on this host (its packets come from a Docker
bridge, never from the external interface).

```bash
EXT=<this host's external interface, e.g. eth0 - NEVER a docker0 or br-* bridge>
for p in 4001 4002; do
  # Proxy on THIS host (any kind): drop every packet from outside.
  iptables -I DOCKER-USER -i "$EXT" -p tcp -m conntrack --ctorigdstport "$p" --ctdir ORIGINAL -j DROP
  # Proxy on ANOTHER host: use this form instead, which keeps only the proxy.
  # iptables -I DOCKER-USER -i "$EXT" -p tcp -m conntrack --ctorigdstport "$p" --ctdir ORIGINAL \
  #   ! -s <the proxy's address> -j DROP
done
```

- `EXT` is the external interface in EVERY case. Never put a Docker bridge
  there: a rule on the proxy's own bridge only sees packets from the proxy
  network, so it drops nothing and leaves 4001 and 4002 open to the LAN.
- `DOCKER-USER` exists only with Docker's default iptables backend. With
  the opt-in nftables backend (`"firewall-backend": "nftables"` in
  `daemon.json`) there is no `DOCKER-USER` chain; write an nftables rule in
  your own table instead, or filter in front of the host.

Make the rules persistent with your distribution's tool. They were **not
measured** here (this deploy was tested on macOS, where Docker runs in a
VM).

**Check it from another machine before you rely on it.** Run
`curl -m 5 http://<this host's LAN or public address>:4001/` and the same
for 4002 from a machine that is not the proxy: both must time out (or be
refused). Never bind to `0.0.0.0` on a host with a public address.

### Cookies and https

The server sets `Secure` on its cookies whenever `LOG_ENV` is not `local`,
so here they are always `__Host-odt_session` and `__Host-odt_csrf`:
`Secure`, `Path=/`, no `Domain`. The browser binds them to `$WEB_HOST`. The
server reads no forwarded header for this, so no server change was needed.
nginx passes `Host`, `X-Forwarded-Host` and `X-Forwarded-Proto` on for any
later use.

### URLs the server is given

| Variable | Value |
|---|---|
| `PUBLIC_BASE_URL` | `https://$WEB_HOST` (the origin of every `downloadUrl`) |
| `WEB_ORIGINS` | `https://$WEB_HOST` (same origin; harmless, and right if the API moves) |
| `S3_ENDPOINT` | `http://silo:9000` (what the server dials) |
| `S3_PUBLIC_ENDPOINT` | `https://$FILES_HOST` (what goes into presigned URLs) |

silo allows CORS from `https://$WEB_HOST` only (`MINIO_API_CORS_ALLOW_ORIGIN`).
The web app's CSP has `https://$FILES_HOST` in `connect-src` (the browser
PUTs uploads there) and `img-src` and `media-src` (the download redirect lands there; an mp4 plays inline).

## First deploy

On the server (Linux; this was measured on macOS with Docker Desktop):

Prerequisites: Docker Engine with Compose v2.20 or newer, `git`, `curl`,
`python3`. Nothing else listens on 4001 or 4002 (the script probes
`127.0.0.1` and `DEPLOY_BIND_ADDR`).

```bash
git clone https://github.com/kraetos10/launchpad.git
cd launchpad
deploy/deploy.sh
```

The script:

1. checks the prerequisites and that the Docker daemon answers;
2. writes `deploy/.env` (mode 0600, gitignored) with generated secrets:
   `POSTGRES_PASSWORD` (32 random bytes, hex), `AUTH_OTP_PEPPER` (48 bytes,
   hex), the silo root user and password, the app's silo key and secret,
   `SETTINGS_ENCRYPTION_KEY` and `BACKUP_ENCRYPTION_KEY` (32 bytes each, hex;
   see "Mail settings" and "Backup and restore"), `OFFSITE_DB_PASSWORD` and
   the off-site worker's silo key and secret;
3. on a first run at a terminal, asks for the bind address
   (`DEPLOY_BIND_ADDR`), the web host (`WEB_HOST`), the files host
   (`FILES_HOST`), the web port (`DEPLOY_WEB_PORT`) and the files port
   (`DEPLOY_FILES_PORT`). Each prompt shows its default (the two host names have none: it shows `required`), for example
   `Web port [4001]: `. Press Enter to take the default. A bad answer is
   asked again. A setting that is already in the environment is not asked.
   The script does not ask about mail. Until mail is set, login is OFF (the
   login routes answer 503). Set Postmark or SMTP on the Platform page of the
   web app (see "Mail settings"), or put `POSTMARK_*` in `deploy/.env`;
4. checks that the two ports are free, builds both images (tagged with the
   git commit), starts postgres and silo, makes the bucket (private), a
   policy for that bucket only and the app's own silo user, then starts the
   app, web and backup services and waits until each is healthy;
5. runs the smoke test (below), takes the first backup and prints a summary;
6. on a first run at a terminal, ends with the first-run setup link
   ("First-run setup" below). That is the one command: open the link in a
   browser. With no terminal it prints a hint instead and no link.

It never prints a secret, except that setup link on a terminal. It exits non-zero on any failure. A first run
writes `deploy/.env` only after every check of the settings passes, so a
first run that stops there, or is stopped by Ctrl-C or a kill, leaves no
file behind (its temp file `deploy/.env.new.<pid>` is removed); if two
first runs race, the file that lands first wins and the other run stops.
A re-run never deletes an existing `deploy/.env`, whatever the checkout
path is, and never changes a value in it. It appends every key of the
list (`ENV_KEYS` in `deploy/lib.sh` is the authority; `deploy/.env.example`
shows the same keys) that the file lacks, each on a new line, and names
them (`added: ...`):

- a plain setting gets its default (on a plain re-run, a variable of the
  same name exported in your shell wins; under `--update` the default is
  used);
- a missing `AUTH_OTP_PEPPER`, `S3_ACCESS_KEY`, `S3_SECRET_KEY` or
  `SETTINGS_ENCRYPTION_KEY` gets a NEW generated secret (a new pepper makes
  codes already sent invalid; a new silo key gets its own silo user on that
  run; a new settings key makes the mail secrets stored in the app
  unreadable, so enter them again: "Mail settings");
- a missing `POSTMARK_SERVER_TOKEN` or `POSTMARK_FROM` gets the value
  exported in your shell on a plain re-run (empty if none), so an exported
  token is written into the file.

It never generates `POSTGRES_PASSWORD`, `SILO_ROOT_USER` or
`SILO_ROOT_PASSWORD` again: if one is missing it stops and asks you to
restore it, because the volumes keep the old value (`make deploy-test`
checks the refusal and the append). If a setting in an
existing `deploy/.env` is wrong, the script says which one: edit it there.
**Do not delete `deploy/.env` while the stack's volumes exist:** the
database and silo were created with its secrets. (`deploy.sh --nuke
--nuke-env` removes both, the volumes first; see "Remove a deploy".)

**Non-interactive:** `deploy/deploy.sh --non-interactive` asks nothing. It
reads `POSTMARK_SERVER_TOKEN` and `POSTMARK_FROM` from the environment. A
re-run, or an `--update` of an existing deploy, never asks. Only a first run
asks. `--update` with no `deploy/.env` is a first run, and it asks.
On the FIRST run, every setting can come from the environment:
`DEPLOY_PROJECT` (default `launchpad`; on a host that also runs the local
dev stack, never use its project name `dev-ops` or a `dev-ops-<ticket>`
name: `deploy.sh --nuke` refuses those names, but a hand-typed `down -v`
under one would delete the dev stack's volumes), `WEB_HOST` and `FILES_HOST` (set both: they have no default), `DEPLOY_BIND_ADDR`, `DEPLOY_WEB_PORT`,
`DEPLOY_FILES_PORT`, `S3_BUCKET`, `BACKUP_AT_UTC`,
`BACKUP_KEEP_DAYS`. After that, `deploy/.env` is the authority: edit it
there and re-run. The script runs `docker compose` with a clean
environment: a subshell that unsets every exported variable except
`PATH HOME USER LOGNAME LANG LC_ALL TERM TMPDIR`, `DOCKER_HOST
DOCKER_CONTEXT DOCKER_CONFIG DOCKER_CERT_PATH DOCKER_TLS_VERIFY
DOCKER_API_VERSION DOCKER_DEFAULT_PLATFORM DOCKER_BUILDKIT
BUILDKIT_PROGRESS`, the proxy variables (`HTTP_PROXY HTTPS_PROXY NO_PROXY`
and their lower-case forms), `SSL_CERT_FILE SSL_CERT_DIR`,
`XDG_RUNTIME_DIR` and `SSH_AUTH_SOCK`, then runs docker itself. Everything
else is dropped, including every `COMPOSE_*` (a `COMPOSE_FILE` or
`COMPOSE_PROJECT_NAME` would point compose elsewhere) and `BUILDX_*`. So
an exported `LOG_ENV`, `S3_*` or `AUTH_FIXED_OTP` in your shell never
reaches the stack. `make compose-check` renders the file with the same
environment. No secret is put on a command line: compose
reads them from `deploy/.env`, and the app's silo secret goes to `mcli` on
stdin. `deploy/.env.example` lists every key.

**The compose project must be this file's.** Before it acts, deploy.sh
(and every other mode, and restore.sh) checks the containers and volumes
that carry `com.docker.compose.project=$DEPLOY_PROJECT`. A container must
come from a `deploy/docker-compose.yml` (its `config_files` label), and a
volume must carry this file's `dev.orbit.deploy=production` label. Anything
else, such as the dev stack (`server/docker-compose.yml`, project
`dev-ops`), which declares the same volume names, makes it stop and name
what it found. It never adopts, removes or chowns another stack's
containers or volumes.

**Never edit the `volumes:` block of `deploy/docker-compose.yml`** (names,
labels, driver). If `docker compose` ever says a volume's configuration
changed and asks to recreate it, answer **N**: `y` deletes the volume, and
for `postgres_data` that is the whole database.

**Stacks created from pre-release commits of this branch** (before the
project guard and the volume labels) are not supported by deploy.sh and are
refused by the guard (`--nuke` refuses them too). If you have one, do not
run `down -v` on it (that also deletes its `<project>_backups` volume of
nightly dumps); take a
verified dump yourself (`pg_dump` exit status checked, then restore it into
a scratch database), and deploy the release into a NEW `DEPLOY_PROJECT`
name.

**Re-running is safe.** A re-run keeps every value in `deploy/.env` (it only
adds a key that a newer version introduced), rebuilds what changed, and
leaves the data alone.

## First-run setup

There are no seed tokens. Rules: `docs/internal/decisions-T8-first-run.md`.
One install is one org. While the database has **no user at all**, every
boot of the app writes ONE line to its log:

```
setup: open https://launchpad.example.com/setup#<token> (valid 24h; restart the app for a new link)
```

That link is the only credential the server ever logs. It works for 24
hours and once, and every new link closes the older ones.

**Setup is one command.** A first `deploy/deploy.sh` run on a terminal ends
with the link, after the summary, and the line "Valid 24 hours, single
use". The deploy makes it with the same call as `--setup-link`, so the
older link in the app log stops working. `--non-interactive` on a terminal
prints it too. Running `deploy/deploy.sh` again before setup is done prints
a NEW link and closes the older ones.

The link is a credential, so it goes to a terminal only. When stdout is not
a terminal (CI, `| tee`, `> file`) deploy.sh does not make or print a link:
it shows the hint `FIRST RUN: setup is open ... run deploy/deploy.sh
--setup-link`, and the deploy still succeeds. `--update` shows the hint too.
It also prints no link when `CI` is set (not empty, not `false`, not `0`)
or when `DEPLOY_NO_SETUP_LINK=1`, even on a terminal: a CI job or
`script(1)` can give a pty, and a build log would keep the single-use link.
The hint then says why. Nothing prints a link when setup is done (a user exists) or when the
database cannot be asked (deploy.sh says so). deploy.sh writes no log file,
and `--update`'s backup output and `--smoke` never carry the link.

To get a fresh link later, on your terminal (nothing new goes to the log,
and the link in the log stops working):

```bash
deploy/deploy.sh --setup-link
```

It refuses a pipe or a file for stdout. It runs `server setup-link` in the
app container; once setup is done it says so and prints nothing. A restart
of the app also makes a new link (in the log). The deploy summary says
`FIRST RUN: setup is open` until setup is done.

**Complete setup in the web app.** Open the link in your browser. It goes
to `/setup` and needs no login. The page removes the token from the address
bar at once and keeps it in memory only.

1. Fill in your account (username, name, email), your organisation (name,
   slug) and your first team (name, key). Example: org slug `acme`, team key `ACME`.
2. Select **Set up Launchpad**.
3. The page shows the **first admin token** one time. Copy it into your
   password manager. It is not shown again.
4. Tick the box to confirm you saved it. Then select **Continue to Launchpad**. You are
   signed in already: setup starts a web session.
5. The app opens **Get Launchpad ready** (Settings, Get started): the steps
   to finish a new install (mail, sign-up mode, team cycles, an agent and its
   MCP token, inviting people, the Linear import and a git host). A step is done when the
   server sees it (sign-up mode is done when a platform admin chooses a mode or selects **Mark reviewed**, which
   sends `signupModeConfirmed`). You can skip a step (the skip is kept for the whole organisation) or
   dismiss the banner (kept in that browser). The page stays in
   Settings. The Platform pages for mail and sign-up mode need a platform admin (the first user is one).

A link that does not work (wrong, expired, used or replaced) gets a page
that tells you to make a new one. If setup is done already, the page links
to the login page.

**Complete setup with curl** (an alternative). POST the fragment as `token`.
Paste the link at a prompt, not into your shell history; the body goes to
curl on stdin:

```bash
read -r -s LINK            # the whole link from --setup-link
H=https://launchpad.example.com
printf '{"token":"%s","user":{"username":"admin","name":"Admin","email":"admin@example.com"},"org":{"slug":"acme","name":"Acme"},"team":{"key":"ACME","name":"Acme"}}' "${LINK##*#}" \
  | curl -sS -H 'Content-Type: application/json' --data-binary @- "$H/api/v1/setup" \
  | python3 -c 'import json,sys; b=json.load(sys.stdin); print(b.get("token",{}).get("secret") or b)'
```

In one transaction it makes the user (a **platform admin**, and admin of
the org), the org, its first team with the nine default states, the Bug /
Improvement / Feature labels, a web session (the two cookies) and the
**first admin token**: read, write and admin, pinned to the org, no
expiry. Its secret is shown once and stored nowhere; keep it in your
password manager (or mint your own with it, `POST /api/v1/orgs/acme/tokens`,
and revoke it). A wrong, expired, used or replaced link is `401
invalid_setup_token`; once any user exists every attempt is `409
setup_complete`. A body that fails validation (`400`) leaves the link
usable. More teams: `POST /api/v1/orgs/acme/teams` with that token.

Next, make the `claude` agent and its token in the web app (Settings,
Agents): "Agent users and their tokens" below. To add people, use the
invites in the web app (Settings, Members): "Sign-up requests and invites"
below. Web login needs a mail transport: `POSTMARK_*` in `deploy/.env`,
or the mail settings in the app ("Mail settings" below). Until one is set,
the setup session (30 days) and the first token are how you get in.

**An install with users but no platform admin** never shows a setup link.
Mark the person who runs it as the platform admin once:

```bash
P=$(grep '^DEPLOY_PROJECT=' deploy/.env | cut -d"'" -f2)
docker compose -p "$P" -f deploy/docker-compose.yml --env-file deploy/.env \
  exec -T app /bin/server grant-platform-admin --user admin
```

It refuses an agent, a deactivated user and an unknown name. A session or
token made before it keeps its scopes; log in again for a session with the
platform flag. **If every admin token is lost**, mint a new one on the
server (see "Agent users and their tokens", step 2, for what the command
does):

```bash
deploy/deploy.sh --mint-token --org acme --user admin --name admin \
  --scopes read,write,admin
```

With no `--expires-days` a human token does not expire, the same as the
first admin token. Do not mint a short "rescue" token and then mint the
real one with it: no token outlives the token that minted it, so the real
one would expire as soon as the rescue token. With the new token, revoke
the lost ones (`GET` and `DELETE /api/v1/orgs/acme/tokens`). Do not write
tokens into the database with SQL.

## Agent users and their tokens

An agent user is a user for a program, for example the MCP server that
Claude runs. An agent has no email, so it cannot get or use a login code.
It works only with tokens. Its org role is `member`, so its tokens cannot
have the `admin` scope. Its tokens must expire (1 to 365 days). Rules:
`docs/internal/decisions-T10-agents.md` (agents) and `docs/internal/decisions-T11-web-tokens.md`
(tokens made in the web app).

Do these steps once, after first-run setup. Use the web app. The command
line way after it is the fallback.

### In the web app

You need to be an org admin. Setup made you one.

1. Open **Settings**, then **Agents**. Enter the username `claude` and the
   name `Claude`. Choose **Create agent**. A username is unique on the
   install: if it is in use, the page says so, and nothing changes.
2. Choose **Create a token for this agent**. Enter a name (for example
   `mcp`). Leave **Read and write** and **365** days, or change them. The
   maximum is 365 days. Choose **Create token**.
3. Choose **Copy token** and put the token in your password manager at once.
   The page shows it ONE time. The server keeps only a hash of it. Tick
   **I saved the token in a safe place**, then choose **Done**.
4. The window also shows **MCP setup**: the four variables
   `LAUNCHPAD_URL`, `LAUNCHPAD_TOKEN`, `LAUNCHPAD_ORG` and
   `LAUNCHPAD_TEAM`. The URL, the org and the first team are filled in.
   Paste the token as the value of `LAUNCHPAD_TOKEN`. The sample
   `.mcp.json` entry is in `docs/mcp.md`.
5. **Settings**, then **API tokens**, lists every live token: name, owner,
   access, created, last used and expiry date. Choose **Revoke** and confirm
   to stop a token at once. Make the next token before the old one expires.

Rules for a token made in the web app: it always expires (1 to 365 days),
it is `read` and `write` only (never `admin`), and at most 5 tokens can be
made at once, then 10 an hour. A token with no expiry or with the `admin`
scope needs a bearer admin token or the server command below. Create and
revoke each write an `audit: token created` or `audit: token revoked` line
to the server log (never the secret).

### On the command line (the fallback)

Use this when you have no web session: for example when web login is not set
up (it needs Postmark) and you have no setup session.

**1. Make the `claude` agent.** Do this on your workstation. Type the admin
token at the hidden prompt. Do not put it on the command line: the shell
history and `ps` would keep it. curl reads the header from stdin:

```bash
read -r -s ADMIN_TOKEN     # paste your admin token; the prompt shows nothing
H=https://launchpad.example.com
printf 'Authorization: Bearer %s\n' "$ADMIN_TOKEN" \
  | curl -sS -w '\n%{http_code}\n' -H @- -H 'Content-Type: application/json' \
      --data '{"username":"claude","name":"Claude"}' "$H/api/v1/orgs/acme/agents"
unset ADMIN_TOKEN
```

The answer is `201` with the new member (`"isAgent": true`, `"role":
"member"`, `"email": null`). A second run answers `409 conflict`: the user
exists already, and nothing changes. Usernames are unique on the install,
so you cannot reuse a human's username.

**2. Mint the agent's token.** Do this on the server, in the `launchpad`
checkout:

```bash
deploy/deploy.sh --mint-token --org acme --user claude --name mcp \
  --scopes read,write --expires-days 90
```

The command runs `server mint-token` in the app container. It prints the
secret ONE time, alone on one line. Copy it into your password manager at
once. The server keeps only a hash of it and never writes it to a log. The
command refuses a pipe or a file for its output, the same as
`--setup-link`. The lines after the secret give the token id, the scopes
and the expiry date.

- The token is pinned to the org (`acme`). It cannot reach another org.
- `--scopes` is `read`, `write` and `admin`, with commas. Only an org admin
  can have `admin`; for `claude` the command refuses it.
- `--expires-days` is required for an agent (1 to 365). For a human it is
  optional (1 to 3650; no value means no expiry).
- For a reviewer agent, give it a read-only token: `--name reviewer
  --scopes read`.
- An unknown org, an unknown or deactivated user, or a user who is not a
  member of the org: the command refuses and makes nothing.

You can also mint the token through the API with your admin token:
`POST /api/v1/orgs/acme/tokens
{"name":"mcp","scopes":["read","write"],"user":"claude","expiresInDays":90}`.

Before the token expires, mint a new one and revoke the old one.

**A reviewer entry (read-only).** A reviewer agent (for example an automated
code reviewer) must not write. The tool grant of the agent runtime is not enough:
a runtime that ignores it can call every tool. Use a second MCP entry,
`launchpad-reviewer`, with `--profile reviewer` (only `get_issue`,
`list_comments` and `get_document`; its HTTP client sends `GET` only) and a
read-scoped token. The org admin makes the token on the server, on a
terminal (the org slug is yours; `orb` on the author's install):

```bash
deploy/deploy.sh --mint-token --org orb --user <agent> --name reviewer \
  --scopes read --expires-days 90
```

The token name is free (`reviewer` matches the bullet above). `<agent>` must
already exist (step 1). A separate reviewer agent is better than reusing
`claude`; this is advice, not a rule.

Put the secret in your password manager. Export it as
`LAUNCHPAD_REVIEWER_TOKEN` in the environment of the reviewer's runtime.
The entry is in `docs/mcp.md`. Its tools are `mcp__launchpad-reviewer__*`:
point the reviewer briefs at them, and give the reviewer no full-profile
entry. The key of the full entry is free to choose: `docs/mcp.md` and
`AGENTS.md` use `launchpad`.

**3. Configure the MCP server** on the machine that runs the agent. The
entry is in `docs/mcp.md`. Set these values:

| Variable | Value |
|---|---|
| `LAUNCHPAD_URL` | `https://launchpad.example.com` |
| `LAUNCHPAD_TOKEN` | the secret from step 2, from the environment only |
| `LAUNCHPAD_ORG` | `acme` |
| `LAUNCHPAD_TEAM` | `ACME` |

`.mcp.json` reads `${LAUNCHPAD_TOKEN}` from the environment. Never put
the token in a committed file. Make a check: an MCP `get_issue` call must
answer, and `GET /api/v1/me` with the token must show `"username":
"claude"` and `"isAgent": true`.

### Backup recipients from the host

Run these in the app container. Both act at once, write a change-log row with no actor, and need
no `BACKUP_ENCRYPTION_KEY`. Both take `--fingerprint <64 hex characters>` (the full sha256 of the key).

- `server backup-recipient-remove --fingerprint <fp>` removes an age recipient now (the web app
  holds a removal for 24 hours). It refuses to leave no active key while a destination is enabled.
- `server backup-recipient-cancel --fingerprint <fp>` cancels a pending add or a scheduled removal now.

### Off-site backup worker
`server offsite` copies backups off the host. `deploy.sh` runs it as the compose service `offsite`.
Rules: "Build notes: ticket 5" in `docs/internal/decisions-T16-backups.md`.

- **Keys in `deploy/.env`** (mode 0600, never printed): `BACKUP_ENCRYPTION_KEY` (64 hex) and
  `OFFSITE_DB_PASSWORD` (letters and digits, 32 to 128). `deploy.sh` makes both once and never makes
  them again. **Back up `BACKUP_ENCRYPTION_KEY` apart from the host**: without it, stored destination
  secrets cannot be opened. Keep the recipients' age PRIVATE key, and the signing public key that `--backup-rotate-signing` prints, off the host too ("Restore from a remote copy" lists everything to save). On an install that already has a `deploy/.env`, the first run adds the
  key and prints a warning.
- **`BACKUP_ALLOW_PRIVATE_CIDRS`** (optional, in `deploy/.env`): comma list of private ranges the
  worker may reach (for example a NAS). Empty means public hosts only. Use the narrowest range
  (a /24 or smaller). The host's own ranges, the gateway and the compose service names stay refused.
- **Start order.** `deploy.sh` starts postgres and silo, then `app web backup`, waits until the app
  is healthy (migrated), applies `deploy/backup/offsite-grants.sql` (it makes the role `offsite` and
  sets exactly its grants; safe to run again, runs on every deploy), then starts `offsite` and waits
  until it is healthy. The password goes to psql on standard input only.
- **First update on an old install:** postgres joins the new `offsite_db` network, so its container
  is recreated and the database restarts for a few seconds. `deploy.sh` says so before it does it.
- **Dumps.** `backup.sh` names a dump `devops-<YYYYMMDDTHHMMSSZ>.dump` and records its size and
  sha256 in `backup_dumps`. An on-demand request from the app (`backup_requests`) is served within
  `BACKUP_REQUEST_POLL_SECONDS` (default 30).
- **Host commands** (a terminal only; they refuse a pipe or a file):
  `deploy.sh --backup-keygen` (new age key pair: the private half is shown once),
  `--backup-rotate-signing`, `--backup-resume` (after a restore on another host: new instance id),
  `--backup-recipient-remove <fp>`, `--backup-recipient-cancel <fp>`.
- **Health.** `docker compose -p <project> ps` shows `offsite` healthy. The health file shows
  liveness, not progress. Logs hold counts and fixed words, no secret.
- **Measured** (Docker Desktop, a throwaway stack): `offsite` runs as uid 70 and starts; it is healthy
  about 40 seconds after the stack starts; its default route is on the `egress` network (`eth1`), and
  `offsite_db` is internal; `app`, `backup`, `silo` and `web` do not resolve from it. An idle `backup.sh`
  request check (every 30 s) costs about nothing at one-second resolution (100 checks in under 1 s).
  **NOT measured:** the Linux egress rule, dump size and time on production data, the size of the scratch
  volume, and the health file over days.
- Other worker commands in the container: `rotate-signing`, `resume --instance-id <id>`,
  `healthcheck`, `fetch`, `unpack`, `verify`, `decrypt`. `deploy/restore.sh --remote` runs the last four ("Restore from a remote copy").

### Backup notices and the stale alert
The app (not `offsite`) mails every active platform admin who has an address, through the platform
mail settings. A job runs every minute; it needs no setting of its own. Rules:
`docs/internal/decisions-T16-backups.md`, "Build notes: ticket 8".

- **What is mailed.** A `schedule`, `manual`, `catchup`, `retry` or `verify` run that ended `failed` with
  no retry coming (when "mail on failure" is on, the default), or `succeeded` (when "mail on success"
  is on, off by default). Every change-log row is mailed to ALL platform admins, whoever made it.
  A destination that has had no success for longer than `max(stale alert hours, 2 x its schedule
  interval)` gets one "overdue" mail every 24 hours. A `test`, `probe` or `exchange` run, an
  interrupted run and a cancelled run send nothing.
- **Mail is off** (provider `none`, or `env` with nothing set): nothing is sent and nothing is marked as
  sent. The Backups status says `mailReady: false` and shows the `mail_off` banner while a destination
  is enabled. When mail comes back, a run that ended in the last 24 hours and a change made in the last
  7 days are mailed then; older ones are not (the page and the tables still show them).
- **A lost or doubled mail.** A run notice is at-most-once: a crash between marking it and sending it
  loses that mail (also when no platform admin has an address at that moment). A change notice is at-least-once: a crash can send it twice. A change mail counts
  as sent when at least one admin got it; the other admins' failures are logged (fixed text, no address).
- **Row cleanup** (hourly, in the app): final runs and failed or file-removed dumps after 180 days, final
  requests after 7 days, the change log after 730 days. A dump left `running` for 6 hours and a request left
  `taken` for 2 hours (with no running dump) are marked `failed`. An unsaved-test secret older than
  10 minutes is cleared (every minute).

## Mail settings

Login codes go out by email. A platform admin chooses how,
in the app: Settings, then Platform in the web app (a platform admin only),
or `GET` / `PUT /api/v1/platform/settings`. Rules and every judgment call:
`docs/internal/decisions-T9-mail-settings.md`; the page: `docs/internal/web-decisions.md`.

| `mailProvider` | Login email goes through |
|---|---|
| `env` (default) | the environment, as before: `POSTMARK_*` from `deploy/.env`; with no token, login is OFF (`503 login_unavailable`) |
| `none` | nothing: login is OFF |
| `postmark` | Postmark, with the token and sender stored in the app |
| `smtp` | an SMTP server, with the host, port, security, user and password stored in the app |

- **Only a platform admin** can read or change them: the setup session, a
  later login session, or that person's token with the admin scope and no
  org pin. The first token from setup is pinned, so it gets `403`.
- **Secrets are write-only.** The Postmark token and the SMTP password are
  stored sealed (AES-256-GCM) with a key derived from
  `SETTINGS_ENCRYPTION_KEY`. `GET` shows only `postmarkTokenSet` and
  `smtpPasswordSet`. In a `PUT`, leave a secret out (or send `null`) to keep
  it, send `""` to clear it.
- **Test it:** `POST /api/v1/platform/mail/test` sends one email to your own
  address through the transport login uses now. A failure is `502
  mail_send_failed`; the message names the step and the reply code only.
- **A change applies at once** on the server that took the `PUT`, and on
  every other server within 60 seconds.
- **The sign-up mode** (`signupMode`: `invite`, the default, `approval` or
  `closed`) is stored here. Sign-up requests and invites follow it: see
  "Sign-up requests and invites" below.

### SMTP

- `smtpSecurity`: `starttls` (connect, then STARTTLS; usually port 587; the
  send fails if the server does not offer STARTTLS), `tls` (TLS from the
  first byte; usually port 465) or `none` (no TLS).
- Auth is PLAIN, and only when `smtpUsername` is set. The server refuses to
  send a password without TLS, except to `localhost`, `127.0.0.1` or `::1`
  (a relay on the same host). So `none` with a username is accepted only
  for those three hosts.
- The certificate must be valid for `smtpHost` (the system's trusted
  roots). There is no option to skip the check.
- No DKIM signing and no OAuth: use a provider or relay that signs for your
  domain, or Postmark.
- The message is plain text, UTF-8, the same text as the Postmark email.

### `SETTINGS_ENCRYPTION_KEY`

- 64 hex characters (32 random bytes): `openssl rand -hex 32`. `deploy.sh`
  generates it into `deploy/.env` on the first run, and on a re-run whose
  file lacks it (an install from before this key existed). It never prints it.
- The server refuses a malformed key at boot. Outside `LOG_ENV` local/test
  it also refuses the public dev key of `server/docker-compose.yml` and a
  key of all zeros (the `.env.example` placeholder).
- Without the key, the server starts, but a `PUT` that sets a secret
  answers `409 conflict`.
- **The same key also seals the git connections**: the access
  token of a token-mode connection and the webhook secret of GitHub and
  Bitbucket connections (and a sealed copy of a GitLab one). Without the key a
  connect or a secret rotation that must store a token or a webhook secret
  answers `409 encryption_unavailable`; a GitLab webhook-only connection with a
  pasted secret still works (it is stored as a hash). **There is no key
  rotation.** A changed or lost key never stops the boot, but every sealed
  value stays unreadable. A webhook that needs the sealed secret answers the
  plain `401` and logs `code=secret_unreadable` (never the error text); that
  alone does not change the connection's status, so a webhook-only connection
  can still show `ok`. The status `needs_attention` with `attentionCode`
  `secret_unreadable` is set only when a token use, a rotation, a commit or
  **Check token** fails to open a sealed value. A GitLab connection that has only
  the hash (see "Git integrations: webhook secret rotation") is not affected.
  To recover, rotate the secret and commit it (`rotate-secret`, then
  `rotate-secret/commit`, with the host updated between the two) and, in token
  mode, replace the token. There is no tool that re-seals old values under a
  new key. Keep `deploy/.env` safe: it holds the key.
- **`AUTH_OTP_PEPPER` is required at every boot**: a platform admin can turn login on at
  run time, and login needs the pepper. `deploy.sh` always generates it.
  An install that has none (for example an old CapRover app) must add one
  before it upgrades: `openssl rand -hex 48`. `GET /platform/settings`
  says `loginAvailable` (a transport AND the pepper).
- **If the key is lost or changed**, the stored secrets cannot be opened.
  The server still starts and logs an ERROR line (`mail settings: the
  stored mail secrets cannot be opened`). If the provider needs one of
  them (`postmark`, or `smtp` with a username), login email goes through
  the environment (`POSTMARK_*`) instead. A provider that needs none
  (`none`, `env`, `smtp` without a username) is used as it is: `none`
  stays `none`. `GET /platform/settings` says `secretsUnreadable: true`.
  To recover, a platform admin `PUT`s the settings again with the Postmark
  token or the SMTP password.
- **Getting back in when login is off.** The `/platform` routes need a
  platform admin's web session, or their token with the admin scope and
  NO org pin. Every token the API and `deploy.sh --mint-token` make is
  pinned to the org, so a minted token gets `403` here; only a session
  works. The paths, in order:
  1. The setup session (30 days from setup), if that browser still has it.
  2. Put `POSTMARK_SERVER_TOKEN` and `POSTMARK_FROM` in `deploy/.env` and
     run `deploy/deploy.sh` again. With the provider `env`, or after a
     fallback, login then works: log in and `PUT` the settings.
  3. If the provider is `none`, or `smtp` / `postmark` with settings that
     do not work, set it back to `env` in the database first (it takes
     effect on every server within 60 seconds), then do step 2:

     ```bash
     P=$(grep '^DEPLOY_PROJECT=' deploy/.env | cut -d"'" -f2)
     docker compose -p "$P" -f deploy/docker-compose.yml --env-file deploy/.env \
       exec -T postgres psql -U devops -d devops \
       -c "UPDATE platform_settings SET mail_provider = 'env', updated_at = GREATEST(clock_timestamp(), updated_at + interval '1 microsecond')"
     ```
- Keep a copy of `deploy/.env` (it holds this key) with your other secrets.

## Sign-up requests and invites

A new person joins the org in one of two ways. You can do both in
the web app with no API call by hand: an org admin makes invites in
Settings, Members, and decides requests in Settings, Sign-ups. A platform admin
sets the mode and the platform admins in Settings, Platform. The people use
two public pages: `/invite#<token>` (the invite link) and `/signup` (a request).
The text below names the API calls behind each page. Rules and every
judgment call: `docs/internal/decisions-T12-signups-invites.md`. The sign-up mode in
the platform settings chooses which ways are open:

| `signupMode` | Invites | Public sign-up requests |
|---|---|---|
| `invite` (default) | yes | no (`403 signup_unavailable`) |
| `approval` | yes | yes; an org admin approves each one |
| `closed` | no | no |

**Invite a person** (an org admin; mode `invite` or `approval`):

1. `POST /api/v1/orgs/acme/invites {"email": "ann@example.com", "role": "member"}`.
   When mail works, the server emails the link
   (`<PUBLIC_BASE_URL>/invite#<token>`) to the address ONLY
   (`emailed: true`), and the answer has no link. Without working mail the
   answer has `url` once (`emailed: false`): only a hash of the token is
   stored, so copy the link now and give it to the person. The web app shows
   the link one time in a window that you cannot close before you confirm
   the copy. An account made
   from a copied link loses every session and token it had at its first
   email-code login.
2. The person opens the link. The web page `/invite` removes the token from
   the address bar and sends `POST
   /api/v1/invites/accept {"token", "name", "username"}`. A new person gets
   an account and is logged in. A person who has an account already joins
   the org and then logs in as usual.
3. The link works once and for 7 days. `GET /api/v1/orgs/acme/invites`
   lists invites; `DELETE /api/v1/orgs/acme/invites/<id>` stops a link.
4. Team access (decisions-T20 #26 to #30): add `"teamAccess": "selected",
   "teams": ["KEY", ...]` to the invite to give the person only those teams;
   without them the person sees all teams. The accept gives exactly those
   teams that still exist. If every team of the invite was deleted, the
   accept answers `409` and the link stays usable: revoke it and invite
   again. While an invite names teams, every repository of an enabled git
   integration must have a team (the invite is refused otherwise, and so is
   a change that leaves a repository with no team).

**Let people ask** (mode `approval`):

1. A platform admin sets `signupMode` to `approval` (Settings, Platform, or
   `PUT /api/v1/platform/settings`).
2. A person opens `<PUBLIC_BASE_URL>/signup` (the login page links to it) and
   fills the form. The page sends `POST /api/v1/signups {"email", "name",
   "username", "note"}`. The login page cannot read the sign-up mode while
   signed out, so the link is always there. In another mode the sign-up page
   shows "Sign-up is not open". The answer is always `202`. When mail works the person gets a
   6-digit code and confirms the address with `POST /api/v1/signups/confirm`
   within 24 hours. No account exists yet, and the person cannot log in.
3. An org admin reads `GET /api/v1/orgs/acme/signups`. While mail works,
   it lists only requests whose address was proved; without mail it lists
   them all, with `emailVerified: false`. Then `POST
   .../signups/<id>/approve` (this makes the account and the membership;
   an optional body `{"teamAccess": "selected", "teams": ["KEY"]}` gives
   only those teams, no body gives all teams) or
   `POST .../signups/<id>/reject {"reason", "block"}`. `block: true` stops
   new requests from that email.
4. A confirmed request that nobody decides expires 30 days after it was
   made. A request made while mail was off shows as unverified, also after
   mail works again. At most 200 can wait for an admin and 1000 for their code; over a
   cap new requests get `429`.
   If a flood holds the 1000 places for unconfirmed requests, `DELETE
   /api/v1/orgs/acme/signups/unconfirmed` deletes every request that waits
   for its code (they also expire after 24 hours by themselves).
   The Sign-ups page has this as the button "Remove unconfirmed requests".

**Platform admins.** A platform admin can list, add and remove platform
admins: `GET /api/v1/platform/admins`, `POST /api/v1/platform/admins
{"user": "<username or email>"}`, `DELETE /api/v1/platform/admins/<user
id>`. The server never removes the last active platform admin (`409`). If
an install has no platform admin at all, use `server grant-platform-admin
--user <name>` in the app container.

- Set `PUBLIC_BASE_URL` to the web address, or invite links are bare paths
  (`/invite#...`) that you must complete by hand.
- The limits are per server process: a global bucket per route, a limit
  per email, and a limit per client address. Behind a proxy the server
  knows the client address only if you set `TRUSTED_PROXY_CIDRS` (below).
  Without it there is no per-client limit behind nginx, so one shared
  address cannot lock out everyone.
- Audit lines (`audit: sign-up approved`, `audit: invite created`, and so
  on) go to the app log at level info. They hold ids, never an email
  address, a code or a token.

### Per-client limits behind proxies (`TRUSTED_PROXY_CIDRS`)

Optional. Leave it empty unless you know every proxy in front of the app.
Rules: `docs/internal/decisions-T12-signups-invites.md` #9.

1. Find the edge network of the stack:
   `docker network inspect <project>_edge --format '{{(index .IPAM.Config 0).Subnet}}'`
   (for example `172.20.0.0/16`).
2. Find the address that your outer TLS proxy connects FROM (its own
   address, or the docker bridge gateway when it runs in a container).
   Make sure that proxy sets `X-Forwarded-For` to the real client.
3. Put both in `deploy/.env`, comma separated:
   `TRUSTED_PROXY_CIDRS='172.20.0.0/16,192.0.2.10'`, then `deploy/deploy.sh
   --update`.

Set it on any install behind a proxy. Four public routes use the per-client
key: `POST /signups`, `POST /signups/confirm`, `POST /invites/accept` and the
one-click unsubscribe (`POST /notifications/unsubscribe/{token}`). The git
webhooks (`.../integrations/{gitlab,github,bitbucket}/webhook`) take no
per-client key in the shipped code (GitHub and Bitbucket have a byte budget and
a read deadline; the deliveries ring is throttled per org, provider and result).
So an unset `TRUSTED_PROXY_CIDRS` does not make git deliveries share a bucket.

If you list only the edge network, every caller gets the address of the
outer proxy as its key, and about 20 requests an hour lock out sign-up and
invite acceptance for everyone. List every hop, or none. `0.0.0.0/0` and
`::/0` are refused at boot.

### Git hosts on a private network (`GIT_ALLOW_PRIVATE_*`)

Optional. Rules: `docs/internal/decisions-T17-git-connections-import.md`
#31. The app calls the GitHub, GitLab and Bitbucket APIs in token mode through
one guard (`internal/netguard`). It checks the resolved address at every
connection. By default it refuses every private address, so a self-hosted
GitLab on your LAN does not work until you set one of two variables in
`deploy/.env` (a shell on the host; there is no setting in the app, so a stolen
admin session cannot open your network):

- `GIT_ALLOW_PRIVATE_PROVIDERS='1'` opens the five standard private ranges
  (`10/8`, `172.16/12`, `192.168/16`, `fc00::/7`, `100.64/10`).
- `GIT_ALLOW_PRIVATE_CIDRS='192.168.4.0/24,10.9.0.0/16'` opens only the ranges
  you list. A `/0`, a range shorter than `/8` (IPv4) or `/32` (IPv6), and a
  range that overlaps loopback, link-local or another always-refused range stop
  the boot with the reason.
- With both set, the app opens the union.

Always refused, whatever you set: loopback, link-local, the cloud metadata
addresses, the documentation and multicast ranges, **every subnet the app
container is attached to** (this stack's `backend` and `edge` networks, so
`silo` and `postgres` stay out of reach), the container's default gateway, and
the compose service names. A Git host that runs on the Docker host itself is
reached from the app through the gateway or a published port. The code refuses
the gateway and every attached subnet, with no exception: a `/32` for the
gateway in `GIT_ALLOW_PRIVATE_CIDRS` does not help. Advice from the design,
not measured here: give such a host its **LAN address** (the host's address on
the physical network, inside a range you opened) and do not use the gateway or
a published-port address. Or use webhook-only mode, which makes
no outbound call. An `app` container on host networking sees the host's own
interfaces as attached subnets, so every private address is refused: it must
use webhook-only mode. The app reads its attached subnets at boot and each hour.
If it cannot read them, it refuses every private address until it can. The
state is printed in the deploy summary (`git hosts`). Apply a change with
`deploy/deploy.sh --update`.

### Git integrations: connect a host in the web app
The web app is the main way. An org admin opens **Settings, Integrations**
(`/<org>/settings/integrations`) and selects **Connect a git host**. Rules:
`docs/internal/decisions-T17-git-connections-import.md`.

1. Choose GitLab, GitHub or Bitbucket and the web address of the host (the
   hosted address is filled in; self-managed hosts need their own address).
2. Choose the mode.
   - **Token:** you paste an access token once. Launchpad makes the webhook on
     each repository you map and keeps its secret. It needs
     `SETTINGS_ENCRYPTION_KEY` and an `https` `PUBLIC_BASE_URL` (`409 public_url_required`
     without it; plain `http` passes only on a local, non-production server), and the host must be reachable from the app (see "Git hosts
     on a private network").
   - **Webhook only:** Launchpad makes no call to the host. The dialog shows the
     webhook address and a secret **once**; you make the hook on the host by
     hand and map each repository with its id and path.
3. Map repositories on the connection's page (a team limits the linking to that
   team). The page also has the deliveries table, **Rotate secret**, **Replace
   token**, **Check token**, **Disable** and **Remove connection**. In token mode each
   mapped repo has **Refresh hooks** and **Backfill open MRs**. Disable stops
   deliveries only; the hooks stay on the host until the repository is unmapped.

The same actions are REST routes (`.../integrations`, in `api/openapi.yaml`;
admin scope), for scripts. The webhook address is
`https://<WEB_HOST>/api/v1/orgs/<org>/integrations/<provider>/webhook`, where
`<provider>` is `gitlab`, `github` or `bitbucket`. The old per-provider
config routes (`.../integrations/gitlab/...` for config and projects) and the
old hook script are gone. There is no command line for git connections.

#### Cutover of a live GitLab hook

A GitLab hook made before the git tables keeps working with no change on
GitLab: migration 0012 renames the old rows into the new tables (the connection
becomes a GitLab, webhook-only connection), keeps the webhook address and the
stored secret hash, and rewrites the stored delivery keys once to the `gl:`
form. After the update:

1. Open Settings, Integrations. The old connection is in the list.
2. Rotate the secret once so a sealed copy is stored: **Rotate secret**, paste
   the new secret into the hook on GitLab (Settings, Webhooks, Edit, Secret
   token), then **Commit**. Until you commit, the old and the new secret both
   work. Without `SETTINGS_ENCRYPTION_KEY` the row still rotates, by hash only,
   and stays without a sealed copy. Token mode on this connection needs the
   sealed copy (`409 secret_hash_only` until then).
3. Watch the deliveries table (and GitLab's Recent events) for the first days.

**Run one app instance during the update.** Migration 0012 renames
`gitlab_*` tables and prefixes old delivery keys with `gl:`. An older server
that still runs against the same database would fail on the renamed tables or,
for one that still reaches them, write unprefixed keys that the new code does
not see, so a redelivery could be handled twice. The production stack has one
`app` service, so `deploy/deploy.sh --update` replaces it. If you run another
copy of the app against this database (a CapRover app, a test copy), stop it
before the update and start it again only on the new version.

### Git integrations: webhook secret rotation

Git integrations are made at `POST /api/v1/orgs/{org}/integrations`. A
webhook secret changes in two steps, so no delivery is lost:

1. `POST /integrations/{id}/rotate-secret` stores a pending secret. Until step 2
   the webhook accepts the old AND the new secret.
2. Update the host, then `POST /integrations/{id}/rotate-secret/commit`. Only
   the new secret is accepted after it. `DELETE /integrations/{id}/rotate-secret`
   drops the pending secret instead.

In webhook-only mode the new secret is in the answer of step 1, once. In token
mode Launchpad pushes it to every mapped repo's hook and commits by itself; if a
hook fails the answer lists the repos still on the old secret, both secrets stay
accepted, and calling `rotate-secret` again retries only those repos.

A GitLab integration made before the git tables (a "hash-only" row) has no
sealed copy of its secret, so it cannot be switched to token mode or map a repo
in token mode (`409 secret_hash_only`) until one rotate-and-commit has stored a sealed
secret. Without `SETTINGS_ENCRYPTION_KEY` a GitLab row still rotates (by hash
alone); GitHub and token mode answer `409 encryption_unavailable`.

### Git integrations: remove a connection
An org admin removes a connection from its page in Settings, Integrations, "Remove
connection", or with `DELETE /api/v1/orgs/{org}/integrations/{id}`. It answers `409
repos_mapped` while a repository is mapped: unmap the repositories first (in token
mode that also deletes the hooks on the host; a hook made by hand stays on the host:
delete it there). The removal deletes the connection's secrets, webhook events,
deliveries and merge request links. A stale hash-only connection that blocks a new
one for the same host is removed this way, then connected again.

### Git integrations: after the update that adds MR state
Migration 0016 adds the merge request state to the ticket (the Changes block data,
the three timeline kinds). A link made before it has empty `repo_path` and `author`
until its next event, and `review` is `none`. A GitHub hook made before it
subscribes to `pull_request` only, so approvals do not arrive. In token mode press
**once per mapped repo**: `POST /api/v1/orgs/{org}/integrations/{id}/repos/{repo}/hook/refresh`
(an admin token, or **Refresh hooks** on the repo's row in Settings, Integrations). It updates the same hook to the
current events and keeps its secret. A webhook-only integration: tick
`Pull request reviews` in the GitHub hook by hand. GitLab needs nothing (approvals
come in the merge request events). To link merge requests that are already open,
call `POST .../repos/{repo}/backfill` once per repo (token mode, at most 200 open
changes; `truncated: true` means run it again).

### Git integrations: after the update that adds MR checks
No migration. A hook made before this update has no check events, so no checks
badge appears. In token mode press `POST /api/v1/orgs/{org}/integrations/{id}/repos/{repo}/hook/refresh`
**once per mapped repo** (an admin token). It updates the same hook (GitHub adds
`check_suite` and `status`; GitLab turns on `pipeline_events`) and keeps its secret.
A webhook-only integration: in the GitHub hook tick `Check suites` and `Statuses`; in
the GitLab hook tick `Pipeline events`. A link shows checks only for events that
arrive after that, and only for the head sha the link holds (a new push clears them).
Bitbucket: a token-mode hook made by Launchpad subscribes to the commit status
events too (Cloud `repo:commit_status_created` and `_updated`; Data Center's build status
event, see below). A webhook-only Bitbucket hook: tick the same events by hand.

### Git integrations: Bitbucket Cloud and Data Center
No migration. Connect it like GitHub or GitLab (`POST /api/v1/orgs/{org}/integrations`,
`"provider": "bitbucket"`; the web dialog lists it). Rules and the list of facts that no one
checked on a live host: `docs/internal/decisions-T17-git-connections-import.md`, "Bitbucket".

- **Which host:** `https://bitbucket.org` is Bitbucket Cloud (API `api.bitbucket.org/2.0`).
  Any other base URL is Bitbucket Data Center (REST `/rest/api/1.0` under that URL).
- **Tokens:** Bearer tokens only: a repository access token (Cloud) or an HTTP access
  token (Data Center). An app password needs a user name, so it is not supported.
  Cloud: `Webhooks: read and write`, `Repositories: read`, `Pull requests: read`. Data
  Center: repository admin on the chosen repositories.
- **The webhook secret is required.** Launchpad checks every delivery's `X-Hub-Signature`
  (HMAC-SHA256 of the body). A Data Center version that cannot sign webhooks with a secret
  cannot be used with Launchpad: its deliveries have no signature and answer `401`. The
  first deploy needs `SETTINGS_ENCRYPTION_KEY` (the secret is sealed), as for GitHub.
- **Webhook-only:** make the hook yourself with the address and secret the connect dialog
  shows. Map each repository with its key: Data Center the numeric repository id (it is
  `repository.id` in the payload) and a path `PROJECT/slug`; Cloud
  `{workspace uuid}/{repository uuid}` with the braces. Events: Cloud pull request created,
  updated, approved, unapproved, merged and declined, and repository commit status created
  and updated; Data Center pull request opened, source branch updated, modified, merged,
  declined, reviewer approved, unapproved and needs work, and the build status event.
- **Backfill, webhook-only Data Center:** a repo mapped without a `PROJECT/slug` path cannot be backfilled; give the path when you map it.
- **Data Center build status:** the name `repo:build_status` and the payload are the
  builder's reading of the documentation. If your Data Center refuses a hook that names it
  (`400`), Launchpad makes the hook without it and checks stay empty. Tell us the real
  event name from your host's webhook list.
- **Test connection:** Data Center's "Test connection" (`diagnostics:ping`) answers `200`.
- **After an update that changes the event set,** press `hook/refresh` once per repo (token
  mode), as for the other providers.

## Notifications

The app tells people about issue changes in two ways: an **inbox** in the web
app (bell and `/<org>/inbox`) and **email**. People choose per event and per
team in Settings, Notifications. Rules and every judgment call:
`docs/internal/decisions-T13-notifications.md`. This section is the operator's
view. There is nothing to deploy: three loops start inside the API process.

| Loop | What it does | How often |
|---|---|---|
| Consumer | Reads the change feed (`events`), writes `notifications` rows (the inbox) and `notification_emails` rows (the outbox) | At boot, on every change-feed wake-up, and every 10 seconds |
| Sender | Sends the outbox rows by the platform mail transport | At boot and every 15 seconds |
| Retention | Deletes old rows | At boot and every hour |

No write in the tracker calls any of them. A mail fault cannot fail a write,
and the inbox does not depend on mail. With several app instances, a
per-org lock (consumer), a sender lock and a retention lock make one instance
work at a time.

### Mail provider `none`, and when mail is off

- The consumer never reads the mail setting. It always makes the inbox rows
  and the outbox rows.
- **Mail not ready** (provider `none`, or provider `env` with no `POSTMARK_*` on a
  server whose `LOG_ENV` is not local or test; a local or test server then uses the
  dev log mailer, which is ready and sends nothing: it logs the subject only):
  the sender does not try. Outbox rows stay `pending` and nothing fails. After
  24 hours each row becomes `skipped` with `skip_reason = 'expired'`. Turn mail
  on within the day and the recent rows go out; older ones are dropped. The
  inbox is not touched. Settings, Notifications tells people "This install
  cannot send email".
- **No restart** is needed to change the provider: the sender asks the mail
  switch on every pass (Settings, Platform; a change applies at once on the
  server that took it, and on every other one within 60 seconds).
- **To stop all notification email**, set the provider to `none` (Settings,
  Platform). There is no other switch, and none for the inbox.

### `PUBLIC_BASE_URL` is required for email

A mail holds a link to the issue and a working unsubscribe link, both built
from `PUBLIC_BASE_URL`. With it empty the app logs a WARN at boot
(`notifications: PUBLIC_BASE_URL is empty ...`) and, when mail is ready, the
sender marks every row `skipped` with `skip_reason = 'no_base_url'`. The inbox
still works, and Settings, Notifications shows the "cannot send email" note.
The production compose file (`deploy/docker-compose.yml`) sets it to
`https://$WEB_HOST`; the dev compose defaults to
`http://localhost:8090`. The footer and the `List-Unsubscribe` header use
`PUBLIC_BASE_URL` with a trailing slash trimmed: it must be the address people open in a browser.

### What the sender does to a mail

- **One mail per recipient and issue.** Rows for the same person and issue wait
  until 2 minutes after the newest row, or 10 minutes after the oldest, and go
  out as one mail (one `Message-ID`, the batch id).
- **At most 50 mails a pass** (reading at most 1000 due rows). One send has a
  15 second timeout.
- **Rate cap: 30 mails per person per rolling hour.** Rows over it are
  `skipped` / `rate_cap` (they still show in the inbox). The next mail that goes
  out says how many updates were not emailed.
- **Checked again at send time.** A row is `skipped` / `disabled` when the
  person is gone, deactivated, an agent or not a member, when the issue or its
  team is archived, or when the person has since turned that email off (the
  master switch, the event, or one-click unsubscribe). `no_address` (no email),
  `unproven` (the address is `email_unproven`) and `no_base_url` are the other
  reasons. `access_removed` (migration 0039): the person can no longer see the
  issue's team or the project, so no title of it is sent. The column also
  allows `no_mail`; the code never writes it.
- **No excerpt by default.** A mail names the issue, the actor and the change;
  comment text appears only for a person who turned on the excerpt (200
  characters).
- **Retries.** A failed send waits 1 minute, 5 minutes, 30 minutes, 2 hours,
  then 12 hours. The sixth failure makes the row `dead` (the "dead letter").
  `last_error` holds a short step and reply code (300 characters at most),
  never an address or a body.
- **A row stuck in `sending`** for 10 minutes (a crash) goes back to
  `pending`. Delivery is at-least-once: a crash between the mail server's
  accept and the `sent` update can send one mail twice. If mail is turned off
  during a pass, the rows go back to `pending` with no attempt counted.
- **Logs** hold counts only (`notification sender: ...` lines with `mails`,
  `retry`, `dead`, `expired` ...), never an address, a subject or a body. A
  failed mail logs at WARN. At boot a WARN
  (`notification mail: dead letters are waiting`) gives the number of dead rows.
- **Unsubscribe token.** One per mail, 60 days, stored as a sha256 hash. The
  public `POST /api/v1/notifications/unsubscribe/<token>` allows 20 a minute
  per client and 600 a minute in all, takes at least 300 ms, and answers `204`
  for any token. See "Security notes" for the proxy log.

### Dead letters

The SQL below was run against a database made from `server/db/schema.sql`.
Run it from the deploy checkout (the same form as the mail-settings reset):

```bash
P=$(grep '^DEPLOY_PROJECT=' deploy/.env | cut -d"'" -f2)
PSQL() { docker compose -p "$P" -f deploy/docker-compose.yml --env-file deploy/.env \
  exec -T postgres psql -X -U devops -d devops -v ON_ERROR_STOP=1 "$@"; }

# 1. The outbox by state (how many dead, pending, skipped for which reason).
PSQL -c "SELECT state, skip_reason, count(*) FROM notification_emails GROUP BY 1, 2 ORDER BY 1, 2"

# 2. The dead rows: the issue, the person (a username, not an address), the error.
PSQL -c "SELECT e.id, o.slug || '/' || t.key || '-' || i.number AS issue, u.username AS recipient,
                e.attempts, e.created_at, e.last_error
         FROM notification_emails e
         JOIN orgs o ON o.id = e.org_id
         JOIN issues i ON i.org_id = e.org_id AND i.id = e.issue_id
         JOIN teams t ON t.org_id = i.org_id AND t.id = i.team_id
         JOIN users u ON u.id = e.recipient_id
         WHERE e.state = 'dead' ORDER BY e.created_at"
```

Fix the cause first (the mail settings: "Mail settings" above, then
`POST /api/v1/platform/mail/test`). Then either:

```bash
# 3a. Retry every dead row. created_at is reset on purpose: a pending row
#     older than 24 hours is expired at the next pass, so without it the retry
#     would be dropped. The retry schedule starts again from the first attempt.
PSQL -c "UPDATE notification_emails
         SET state = 'pending', attempts = 0, next_attempt_at = now(),
             created_at = now(), last_error = NULL
         WHERE state = 'dead'"

# 3b. Or discard them (the inbox rows stay; retention deletes dead rows
#     after 90 days anyway).
PSQL -c "DELETE FROM notification_emails WHERE state = 'dead'"
```

Add `AND id = '<id>'` to either statement to act on one row. A retried row goes
through the 2 minute quiet window again and is rechecked (a person who has
since turned email off gets nothing). A retry keeps its `Message-ID`.

### Retention

The job deletes in batches of 1000 rows per statement (50 ms apart), so no
statement holds a long lock. It logs counts at INFO (`notifications retention`).

| Rows | Deleted |
|---|---|
| Inbox rows that are read or archived | 90 days after `created_at` |
| Every inbox row, read or not | 180 days after `created_at` |
| Outbox rows `sent` or `skipped` | 30 days after `created_at` |
| Outbox rows `dead` | 90 days after `created_at` |
| Unsubscribe tokens | 7 days after `expires_at` |

Deleting an inbox row also deletes its outbox rows (`ON DELETE CASCADE`), so
an outbox row can go earlier than the list says.

### Limits and constants

These are constants in `server/internal/notify` (not settings):

| Constant | Value |
|---|---|
| Consumer batch, events per org per transaction | 200 |
| Consumer: an event older than this when read makes no notification | 24 hours |
| New org, or a cursor made by the migration | Starts at the current newest event: no mail for history |
| Inbox list page | 30 default, 100 most |
| Quiet window / longest hold | 2 minutes / 10 minutes |
| Mails per pass / due rows read per pass | 50 / 1000 |
| Rate cap | 30 mails per person per rolling hour |
| Outbox row life before `expired` | 24 hours |
| Retry waits, then dead | 1 m, 5 m, 30 m, 2 h, 12 h; the sixth failure |

Only the people who can log in get notifications: an agent user never gets a
row, a mail or access to the routes (they answer `403`).

### Known limits

- Delivery is at-least-once (above). The `Message-ID` is the batch id; whether
  Postmark keeps a custom `Message-ID` was not tested against the real service.
- There is no platform-admin page for dead letters; use the SQL above.
- The consumer does not stop. To stop only the email, use provider `none`.


## Chat services (Slack, Discord, Fluxer, custom)

An org admin can add up to 10 chat services in Settings, Chat services: Slack
Incoming Webhooks, Discord webhooks, Fluxer webhooks and custom webhooks. Each posts the chosen event kinds (assigned, mentions,
comments, state changes, new issues, project updates due) to one channel. Rules
and every limit: `docs/internal/decisions-T36-chat-services.md` (and
`decisions-T33-slack-channel.md` for the post content and the sender). A service
posts every ticked event whoever the issue belongs to and whoever made the
change (`decisions-T37-chat-every-event.md`); an issue that any active member
has muted posts nothing. Nothing
to deploy: one more loop (the chat sender) starts in the API process. The
migration (0034) turns an existing Slack channel into one `slack` service named
Slack, and keeps its waiting posts and its stored webhook; it works with the
same `SETTINGS_ENCRYPTION_KEY` as before.

- **Needs `SETTINGS_ENCRYPTION_KEY`** (a webhook URL is stored sealed and never
  shown again) and **`PUBLIC_BASE_URL`** (every post is a link). Without the key
  the page cannot store a URL; without the base URL no post is sent.
- **Outbound:** Slack posts go to `hooks.slack.com` and Discord posts to
  `discord.com` or `discordapp.com`; a Fluxer or custom service posts to the https host the
  admin typed. All go through the same dial guard as the git hosts (public
  addresses only, no redirect, no proxy). Make sure the server can reach them on
  port 443. No setting opens a private range, so a Fluxer or custom webhook on your own
  network is refused.
- **What a post holds:** a fixed sentence, the issue identifier and a link. A state
  change also names the old and the new state (`Issue changed state: In Progress → In Review`). No
  title, person name or comment text. A Fluxer service gets the Discord body. A custom service gets
  `{"text", "kind", "id", "link"}` as JSON.
- **Limits:** 60 posts per service per hour, 10 per org per pass; a failed post
  retries (1m, 5m, 30m, 2h, 12h) and then stays `dead` in `chat_outbox` for 30
  days. A row older than 24 hours is skipped as `expired`. A service that is
  switched off or removed drops its waiting posts.
- **If posts stop:** Settings, Chat services shows the last result of each
  service as a fixed word (for example the webhook was deleted: replace it).
  `Send test` checks the saved webhook. A changed `SETTINGS_ENCRYPTION_KEY` makes
  the stored URLs unreadable: enter them again.

## Web Linear import
The Linear import runs from the web app (admin only). Its exports (ticket text
and user emails from Linear) go to `IMPORT_WORK_DIR`, `/work/import` in the app
container, the named volume `<project>_import_work`.

- The volume is new. `deploy.sh --update` creates it; `volume-init` chowns it to
  uid 10001 before the app starts (the app has a read-only root file system and no
  capability). The server sets the mode 0700 itself and fails the web import (not
  the boot) with a log line when it cannot write there.
- The import needs `S3_*` and `PUBLIC_BASE_URL` (deploy.sh sets both).
- The Linear API key is typed in the web app, sent once, kept in memory for the
  export and never stored or logged. A restart of the app stops a running job:
  it is marked `interrupted` and the import writes no rows (one transaction);
  start it again.
- Free space: the export stops with `no_space` when the work directory is nearly
  full. An idle session is closed after 7 days and its directory removed.
  Closing a session (or the idle sweep) removes an ADOPTED directory too, one an
  operator placed in the work directory for adoption. Keep your own copy.
- `--nuke` removes the import volume too.

**Steps in the web app** (Settings, Import from Linear; the five steps):
paste the key and watch the export; map each Linear user to a member (**Create
agent** makes an agent at once; a person needs an invite first); a dry run that
writes nothing and shows the counts; the real run (you type the org slug, and it
needs a dry run on the current export and user map), then **Verify**; then turn
cycles on. Turning cycles on is refused (`409 import_in_progress`) while a
session is exporting, exported, in the user map step, past a good dry run, or
importing. Close the session when you are done.

**The Linear key rule.** `LINEAR_API_KEY` in the environment is for the command
line importer only; it is never in a file, a log or a command line. The web path
takes the key once, in the body of the export request; the server keeps it in
memory for that export and never stores, logs, emits or returns it. The route
refuses an `Idempotency-Key`.

**Command line fallback.** The importer (`server/cmd/importer`) stays for an
offline export, or for a Linear workspace that the server cannot reach. Its steps, the Docker form, exit code 3 on a count mismatch
and the rule against a real run into a shared database without the maintainer are in
`docs/importer.md`; the production cutover of the first
deployment is in `docs/internal/deploy-orbit.md`. An export made with the CLI can
be adopted by the web path: an operator places the directory in `IMPORT_WORK_DIR`
(the directory the CLI export wrote, with its `manifest.json`) and the session
starts with `POST /orgs/{org}/import/sessions {"adopt":"<name>"}` (the name
matches `^[A-Za-z0-9._-]{1,64}$`; the web wizard does not offer this).

The same rules hold on both paths: import while cycles are off, then turn
cycles on; a second run with the same export changes nothing; edits made to
imported rows are lost by a re-run.

## Smoke test

Every deploy and update ends with it; `deploy/deploy.sh --smoke` runs it
alone. It talks plain HTTP to the published ports, with the real host names
(`curl --connect-to`), so the `Host` header is exactly what the proxy will
send:

- `GET /healthz` through nginx is 200 `{"status":"ok"}` (the server pings
  the database);
- `GET /` is the app shell, with the CSP naming `https://$FILES_HOST`, and
  `X-Frame-Options`, `nosniff` and HSTS set; an unknown path falls back to
  the app;
- `/api/v1/openapi.yaml` is 200 and `/api/v1/me` without a token is 401, on
  the same origin;
- another `Host`, and an absolute-URI forward-proxy request for
  `http://example.com/`, are both 421 (no open proxy);
- the attachment round trip, in a throwaway org `deploy-smoke` that the
  script creates in SQL with a 15-minute token pinned to it: prepare (the
  presigned PUT is on `https://$FILES_HOST`), a CORS preflight to silo
  (allowed from `https://$WEB_HOST`, refused from another origin), the PUT
  through port 4002, attach, the download (302 to `https://$FILES_HOST`),
  and the presigned GET through port 4002 returns the same bytes. Then the
  org (every org table cascades) and its objects are deleted. The smoke test
  never touches a real org or its numbers. It runs as its own
  throwaway agent user `deploy-smoke` (admin of the smoke org), made in the
  same SQL and deleted after the org (an org delete does not cascade to
  users). It deletes a leftover `deploy-smoke` org or user only when it
  carries the smoke marker name (and the org has no other member); anything
  else with that name stops the test, and nothing is deleted. On a first
  deploy the smoke user briefly makes the install "have a user", so a
  `POST /setup` in those seconds answers 409; after the cleanup setup is
  open again with the same link. A smoke user left behind keeps setup
  closed: the warning names the `DELETE` that removes it.

**What it does not prove:** TLS, DNS, and the outer proxy itself — that it
forwards both names to the right ports with the `Host` header unchanged,
sends `X-Forwarded-Proto`, allows 100 MiB bodies on the files host and does
not buffer SSE. It does not prove email: no login code is requested, so
real Postmark delivery is unmeasured. Check those once by hand after the
first deploy: open `https://$WEB_HOST`, log in with an emailed code, and
attach an image to an issue in the browser.

## Update

```bash
deploy/deploy.sh --update
```

`--update` updates the server only. It does not build or serve the MCP
binary. The binary runs on each agent machine and comes from a GitHub
Release: run the install command in [`mcp.md`](mcp.md) there. Keep the server
on the same release as the binary, or newer.

You can run `git pull` by hand before `--update`. A new version can need a
new key in `deploy/.env`. Before its first compose call, `--update` adds each
missing key to the file. `--backup-now`, `--smoke`, `--setup-link`,
`--mint-token` and `deploy/restore.sh` do the same. It adds the generated
secrets, and also the missing non-secret defaults (hosts, ports) and empty
`POSTMARK_*` lines. It only adds lines at the end. It does not change a
value, it keeps the file mode 0600, it asks no question and it does not
print a value. It does not make up the database or silo root secrets that are
missing: it stops and names the key.

These modes stop if `SETTINGS_ENCRYPTION_KEY` is not 64 hex characters.
`--backup-now` and `restore.sh` only warn, so a malformed key never
blocks a safety backup. `--nuke` does not do the top-up on purpose. It must not write to a
file that it can delete. After a hand pull it can fall back to removing the
objects one by one, and it says so.

If you pulled by hand, the backup before the update uses the NEW compose
file. Make sure postgres runs before you start `--update`.

It refuses to run on a checkout with uncommitted changes. Then it takes a
backup (the restore point before any migration) BEFORE it pulls, with the
compose file of the version that is running (unless you pulled by hand): it starts only what the dump
needs, with `up --no-deps` (postgres if it is stopped, then the `backup`
service), so no container is recreated from a new compose file. If that
backup fails, it stops: nothing was pulled; at most postgres and the backup
service were started.
Then: `git pull --ff-only`; if that brought new commits it re-runs the new
`deploy.sh` (with `--non-interactive` if you gave it); rebuilds; recreates
only the services whose image or settings changed; waits for health; runs
the smoke test. (The first update FROM a `deploy.sh` older than the backup-before-update step
pulls first; the new script then still backs up, but only if postgres is
running, and starts nothing else for it.) The volumes are never touched: the server
migrates the database forward on boot. Do not run `--nuke` (or a
hand-typed `down -v`) to "start clean" after an update: it deletes the
database, the silo data and `<project>_backups` (every nightly dump)
together. Use **Rollback** below.

**If the smoke test (or a health wait) fails after the new containers
started,** the new version is already live, and its migrations have already
run. The script stops with a non-zero exit and names the failed step; it
does not roll back by itself. Then:

1. look at the failed step and `docker compose -p <project> -f
   deploy/docker-compose.yml --env-file deploy/.env logs --tail=100 app web
   silo`;
2. if the fault is the new version, use **Rollback** below. The restore
   point is the dump the update wrote at its start; its name is on the line
   `backup: wrote devops-<time>.dump` under `== backup before the update`
   in the output;
3. if the fault is outside the stack (the proxy, DNS), fix it and run
   `deploy/deploy.sh --smoke`.

## Rollback

After a rollback or restore that swaps the database, the off-site worker must not run on the old state: stop `offsite` with `app` and `backup`, redeploy, then run `deploy/deploy.sh --backup-resume` (a new instance id). To restore from an off-site copy instead of a local dump, see "Restore from a remote copy".

Each build is tagged `launchpad-api:<commit>` and `launchpad-web:<commit>`.
(Before the rename the names were `dev-ops-api` and `dev-ops-web`. The first
`--update` after the rename builds the `launchpad-*` images and recreates the
containers; the old images stay as unused images. A rollback to a commit from
before the rename uses that commit's own compose file, so it builds
`dev-ops-api` / `dev-ops-web` again and the `launchpad-*` images become the
unused ones. Prune the old names only when you are back on a commit after the
rename.) To free the space, once you are on such a commit: `docker image ls 'dev-ops-*'`, check that no
container uses them (`docker ps -a --filter ancestor=<id>`), then
`docker image rm dev-ops-api:<commit> dev-ops-web:<commit>`. Take care: the
shared dev stack may use `dev-ops-*` names on a dev host; on the production
host only this project's old images carry them.

**No migration between the two versions** (compare `server/db/migrations`):

```bash
git checkout <previous-commit>     # detached is fine for a rollback
deploy/deploy.sh                   # rebuilds from the cache, smoke-tests
# later: git checkout <your default branch> && deploy/deploy.sh --update
```

**A migration in between:** the server never migrates down, and an older
binary cannot boot on a newer schema (golang-migrate finds no file for the
database's version; read from the code, not measured). Restore the dump
that `--update` wrote before it changed anything: its exact name is on the
`backup: wrote devops-<time>.dump` line under `== backup before the update`
in the update output. Do not use `latest`: a nightly or `--backup-now` dump
taken after the update holds the migrated schema. Copy that dump out of the
volume, restore it into a scratch database, stop the app, the backup AND the offsite worker
job, swap the databases, then deploy the old commit. These are two blocks:
run the second only after the first printed `restore: OK`.

**1. Restore into `devops_rollback`.** Nothing live changes here.

```bash
P=<project>; DC="docker compose -p $P -f deploy/docker-compose.yml --env-file deploy/.env"
$DC exec -T backup ls -1 /backups                      # find devops-<time>.dump
D=devops-<time>.dump                                   # the pre-update one
umask 077                                              # the copy holds all data
docker run --rm --network none -v "${P}_backups:/b:ro" alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc \
  cat "/b/$D" > "/srv/rollback-$D"
deploy/restore.sh --keep --name devops_rollback "/srv/rollback-$D"; echo "restore.sh exit $?"
```

Go on only if the output ends with a `restore: OK` line and `restore.sh
exit 0`. If the restore fails, restore.sh drops `devops_rollback` even with
`--keep`, so a partial copy is never there to swap in: read the error,
fix the cause (for example copy the dump again), and run block 1 again. If
it stops because `devops_rollback` already exists (left from an earlier
try), drop that one first: `$DC exec -T postgres psql -X -U devops -d
postgres -c 'DROP DATABASE devops_rollback'`.

**2. Swap, then deploy the old commit.** Start this block only after block
1 printed `restore: OK`. The two renames are ONE transaction: if either
fails, neither happens and `devops` stays the live database. The `&&`
chain stops at the first failure.

```bash
$DC stop app backup offsite &&
printf '%s\n' 'BEGIN;' \
  'ALTER DATABASE devops RENAME TO devops_before_rollback;' \
  'ALTER DATABASE devops_rollback RENAME TO devops;' \
  'COMMIT;' |
  $DC exec -T postgres psql -X -U devops -d postgres -v ON_ERROR_STOP=1 &&
git checkout <previous-commit> && deploy/deploy.sh
```

- **The swap (psql) failed:** nothing was renamed (measured: when the
  second rename fails, the first is undone too). A rename fails while a
  session is connected to that database: find it (`SELECT datname,
  application_name FROM pg_stat_activity`), close it, and run block 2
  again. It also fails when `devops_rollback` is missing (block 1 did not
  print `restore: OK`), or when a `devops_before_rollback` from an earlier
  rollback is still there: drop that one only if you are sure you no
  longer need it. To give up instead, `$DC start app backup` puts the
  stack back as it was.
- **The swap worked but the old version does not come up:** undo the swap
  (also one transaction), then go back to the newer commit:

  ```bash
  $DC stop app backup offsite &&
  printf '%s\n' 'BEGIN;' \
    'ALTER DATABASE devops RENAME TO devops_rollback;' \
    'ALTER DATABASE devops_before_rollback RENAME TO devops;' \
    'COMMIT;' |
    $DC exec -T postgres psql -X -U devops -d postgres -v ON_ERROR_STOP=1 &&
  git checkout <the newer commit> && deploy/deploy.sh
  ```

Writes made after that backup are lost. **When the rollback is confirmed**
(the old version is up, you can log in and open a ticket), drop the old
database and delete the host copy of the dump, which holds every ticket and
user email:

```bash
$DC exec -T postgres psql -X -U devops -d postgres -v ON_ERROR_STOP=1 -c 'DROP DATABASE devops_before_rollback'
rm -f "/srv/rollback-$D"
```

Never use `--nuke` (or `down -v`) for a rollback: it deletes
`<project>_backups` (every nightly dump, including the pre-update one)
together with the database.

`make restore-test` checks the restore path, and runs the swap SQL against
a scratch postgres: a failed second rename leaves `devops` in place.

### Rollback past team access (decisions-T20 #35)

Team access (an org admin sets a member to "selected teams", `PUT
/orgs/{org}/users/{user}/team-access`) exists from the T20 setter on
(LAUN-225). Images from LAUN-215 on carry the `org_members.team_access`
column, images from LAUN-216 on enforce part of it, and only images from
LAUN-224 (the last enforcement ticket) on enforce it on every route; images
from the tickets before that leave some routes open. So a rollback to an
image older than LAUN-224 needs all four steps below: a member who is
restricted now could read hidden teams there. A rollback to an image from
LAUN-224 on needs steps 1, 2 and 4 (skip step 3), and only from LAUN-226 on
(invites carry access, migration `0041`): step 1 stops an admin from opening
an invite during the revoke, and step 4 is the deploy itself. A rollback to
an image older than LAUN-226 that also runs the `0041` down migration
(`cmd/migrate down`) needs step 2 first: that down refuses to run while an
open invite names teams, and never deletes one by itself. When you roll back the database with the dump of block 1, the dump
already holds the access of that time, and nothing more is needed. When you
deploy an older image on the SAME database (no migration in between, or you
keep the data), do these steps first, in this order:

1. Stop the app, so no admin can change access or invites during the steps:
   `$DC stop app`.
2. Revoke every open invite that names teams. The column
   `org_invites.team_access` exists from LAUN-226 on (migration `0041`); on
   an older database no invite can name teams and the statement fails with
   "column does not exist", which is harmless. Otherwise an older image would
   turn such an invite into an all-teams invite (it ignores the column), and
   the `0041` down migration refuses to run while one is open. Expired
   invites that were never revoked are revoked too, so the down passes. The
   person can be invited again after a roll-forward:

   ```bash
   $DC exec -T postgres psql -X -U devops -d devops -v ON_ERROR_STOP=1 -c \
     "UPDATE org_invites SET revoked_at = now() WHERE team_access = 'selected' AND accepted_at IS NULL AND revoked_at IS NULL"
   ```

3. Set every member back to all teams. The team rows stay (they count only
   for `selected`), so a roll-forward can restore the access by hand:

   ```bash
   $DC exec -T postgres psql -X -U devops -d devops -v ON_ERROR_STOP=1 -c \
     "UPDATE org_members SET team_access = 'all' WHERE team_access <> 'all'"
   ```

4. Deploy the older image (`git checkout <previous-commit> && deploy/deploy.sh`).
   It writes `all` for new agents and approvals, which is correct, and change
   feed events with the `legacy` scope; the backfill job places them after a
   later roll-forward.

After a roll-forward, the members are `all`: an admin sets the restricted
members again with the setter (or the members page, once it has the control).

## Backup and restore

- **Database:** the `backup` service runs `pg_dump -Fc` every day at
  `BACKUP_AT_UTC` (default 03:15 UTC) into the `<project>_backups` volume
  as `devops-<UTC time>.dump` (mode 0600). It writes a `.partial` file,
  reads every entry of it, data blocks included, with `pg_restore -f
  /dev/null` (`pg_restore --list` reads only the table of contents, and
  passes a truncated dump), and only then renames it. A dump that fails the
  read is deleted, and the run fails. Dumps older than `BACKUP_KEEP_DAYS`
  (30) days are deleted. `BACKUP_KEEP_DAYS` must be a whole number, 1 or
  more (0 is refused: it would delete the pre-update dump a rollback
  needs); any other value stops the job before it dumps or prunes anything
  (the backup container then restarts and logs the error). Every deploy also makes one on a new stack, and
  every `--update` makes one first. A failed night is logged
  (`docker compose ... logs backup`) and retried the next night.
- `deploy/deploy.sh --backup-now` makes one at once.
- **Off the host:** the volume is on the same disk as the database. Copy
  the dumps somewhere else, for example nightly from cron. Every dump holds
  every ticket and user email: the target directory must be mode 0700, and
  the copies are written 0600.

  ```bash
  install -d -m 700 /srv/offsite
  docker run --rm --network none -v <project>_backups:/b:ro -v /srv/offsite:/out \
    alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc \
    sh -c 'umask 077 && cp /b/devops-*.dump /out/ && chmod 600 /out/devops-*.dump'
  ```

- **Files (silo):** the attachments are in the `<project>_silo_data`
  volume. Objects are written once and never changed, so a copy while silo
  runs is safe except for an upload in flight:

  ```bash
  install -d -m 700 /srv/offsite
  docker run --rm --network none -v <project>_silo_data:/data:ro -v /srv/offsite:/out \
    alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc \
    sh -c "umask 077 && tar -C /data -czf /out/silo-$(date -u +%Y%m%d).tgz ."
  ```

  To restore it (the copy holds silo's own users and policies too). The
  live data is never deleted before the copy is known good: block 1 reads
  the whole archive and changes nothing; block 2 extracts it into a new
  directory in the volume, and only when that worked moves the live data
  aside (to `/data/.restore-old`, in the same volume) and the new data in.

  ```bash
  P=$(grep '^DEPLOY_PROJECT=' deploy/.env | cut -d"'" -f2)
  DC="docker compose -p $P -f deploy/docker-compose.yml --env-file deploy/.env"
  ALPINE=alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
  A=silo-<YYYYMMDD>.tgz       # the copy in /srv/offsite
  # 1. Read every entry and the gzip check sum. Nothing is changed.
  docker run --rm --network none -v /srv/offsite:/in:ro -e A="$A" "$ALPINE" \
    sh -c 'gzip -t "/in/$A" && tar -tzf "/in/$A" >/dev/null' && echo "archive OK: $A"
  ```

  Run block 2 only after block 1 printed `archive OK`. If the extract
  fails, it deletes its own new directory and stops; the live data is not
  touched. Run `deploy/deploy.sh` to start silo on it again.

  ```bash
  # 2. Extract beside the live data, then swap (same volume: renames only).
  # mvall FROM TO: move every entry of FROM, hidden ones too, into TO.
  MVALL='mvall() { for e in "$1"/* "$1"/.[!.]* "$1"/..?*; do
    [ -e "$e" ] || [ -L "$e" ] || continue
    case "${e##*/}" in .restore-new | .restore-old | .restore-bad) continue ;; esac
    mv "$e" "$2"/; done; }'
  $DC stop silo &&
  docker run --rm --network none -v "${P}_silo_data:/data" -v /srv/offsite:/in:ro -e A="$A" "$ALPINE" \
    sh -euc "$MVALL"'
      cd /data
      for d in .restore-new .restore-old; do
        if [ -e "$d" ]; then echo "/data/$d is left from an earlier restore: finish (step 3) or undo it first" >&2; exit 1; fi
      done
      mkdir .restore-new
      if ! tar -C .restore-new -xzpf "/in/$A"; then
        rm -rf .restore-new; echo "extract FAILED: the live data is untouched" >&2; exit 1
      fi
      mkdir .restore-old
      mvall . .restore-old
      mvall .restore-new .
      rmdir .restore-new
      echo "restored $A; the old data is in /data/.restore-old"' &&
  deploy/deploy.sh          # starts silo again and smoke-tests
  ```

  **3.** When the restored files are confirmed (download an attachment you
  know in the web app), delete the old copy. Do this before the next
  `silo-*.tgz` copy, or that copy carries it too:

  ```bash
  docker run --rm --network none -v "${P}_silo_data:/data" "$ALPINE" rm -rf /data/.restore-old
  ```

  To undo block 2 instead (put the old data back). It refuses while
  `/data/.restore-new` exists: then block 2 did not finish, part of the
  live data may still be in `/data` and part in `.restore-old`, and an
  undo would delete the part still in `/data`. In that case do not undo:
  look at `/data`, `/data/.restore-old` and `/data/.restore-new` by hand
  (`docker run --rm --network none -v "${P}_silo_data:/data:ro" "$ALPINE"
  ls -lA /data /data/.restore-old /data/.restore-new`) and move the entries
  by hand, or ask for help.

  ```bash
  $DC stop silo &&
  docker run --rm --network none -v "${P}_silo_data:/data" "$ALPINE" sh -euc "$MVALL"'
      cd /data
      if [ -e .restore-new ]; then
        echo "/data/.restore-new exists: block 2 did not finish; do not undo, inspect /data by hand" >&2; exit 1
      fi
      if [ ! -d .restore-old ]; then echo "no /data/.restore-old: nothing to undo" >&2; exit 1; fi
      mkdir .restore-bad
      mvall . .restore-bad; mvall .restore-old .; rmdir .restore-old; rm -rf .restore-bad
      echo "the old data is back"' &&
  deploy/deploy.sh
  ```

  **Free space:** block 2 keeps the old data until step 3, so the volume's
  disk needs room for a second full copy, about the size of the live data.
  Check both before block 2: `docker run --rm --network none -v
  "${P}_silo_data:/data:ro" "$ALPINE" sh -c 'du -sh /data; df -h /data'`.
  If the disk fills during the extract, the extract fails and block 2
  deletes `.restore-new`; the live data is not touched.

  **An archive that itself holds `.restore-new`, `.restore-old` or
  `.restore-bad`** (a copy taken before step 3 was done): block 2 swaps
  everything else, then stops at `rmdir .restore-new` with an error and
  does NOT print `restored ...`. The restored data is then in place, the
  old data is in `.restore-old`, and `/data/.restore-new` holds only that
  stray directory from the archive. Check that with `ls -lA
  /data/.restore-new`, remove it by hand (`docker run --rm --network none
  -v "${P}_silo_data:/data" "$ALPINE" rm -rf /data/.restore-new`), then run
  `deploy/deploy.sh` and go on with step 3.
- **Restore a dump (a drill, or before a real restore):**

  ```bash
  deploy/restore.sh latest                 # newest dump in the volume
  deploy/restore.sh --compare latest       # also equal to the live counts
  deploy/restore.sh /srv/offsite/devops-20261024T031500Z.dump
  ```

  It restores into a NEW scratch database (`restore_<time>`; it refuses
  `devops`), prints `rows <table> <count>` for every table, the migration
  version and the time `pg_restore` took, then drops the scratch database
  (`--keep` keeps it). `--compare` is meaningful right after a backup.
- **Test:** `cd server && make restore-test` (also in CI's `test` job)
  applies every migration to a throwaway postgres, loads 3,000 issues and
  6,000 comments, runs `backup.sh once`, checks the 30-day prune, restores
  the dump with `restore.sh --compare` and checks that a truncated dump
  fails (and leaves no scratch database, with `--keep` too). It also checks
  that each dump is 0600, that a truncated `.partial` is never renamed into
  place, that a bad `BACKUP_KEEP_DAYS` dumps and prunes nothing, and the
  rollback swap SQL.

Measured on 2026-10-24 (Mac, Docker Desktop): `make restore-test`
18,559 rows in 30 tables, `pg_restore` 0.30 s, every count equal; a 631 KB
dump. On the deployed test stack: a fresh-stack dump of 99 KB; a restore of
the newest dump with `--compare` took 2 s end to end (25 s once, under
heavy host load; it is mostly `docker compose exec` start-up), and the
scheduled job, set two minutes ahead, wrote its dump at that minute.

Measured on 2026-10-25 (same Mac, a throwaway project with
postgres, backup and silo only; no app, so `deploy/deploy.sh` was replaced
by `up -d silo`): the silo blocks as written here. A truncated archive
failed block 1, and block 2 run on it anyway left the live objects as they
were. A good archive restored the backup's objects, silo listed only the
real bucket while `.restore-old` was there, and the undo and step 3 worked.
The rollback blocks as written: a truncated dump in block 1 left no
`devops_rollback`, block 2 run anyway renamed nothing (psql exit 3, the
`&&` chain stopped), and the good swap, its undo and the cleanup worked.

## Restore from a remote copy
Use this when the host or its disk is lost, or to prove that a remote copy works.
`deploy/restore.sh --remote NAME` and `--remote-config FILE` fetch the newest (or a named)
encrypted, signed copy from an off-site destination, check it, decrypt it and restore it into a
SCRATCH database, with the same row count report as `restore.sh latest`. It never writes to the
live `devops` database. Rules: `docs/internal/decisions-T16-backups.md` #39 and "Build notes: ticket 12".

**What was and was not tried.** The restore path was tested against a FAKE S3 server (the
`restorefake` test tool, on a Docker network address, with the real age, Ed25519 and driver code).
It was NOT tried against a real AWS, Backblaze, MinIO, Nextcloud, WebDAV or Dropbox account, on
Linux, or with a production-size dump. Do a drill (below) before you need it.

**Host requirements.** `python3` (the script uses it for times and the manifest), `docker`, and on Linux or rootless
docker the key file, the signing public key and the config file must be readable by your uid (the throwaway containers run as you).

### What you must have saved OFF the host

A lost host takes `deploy/.env` with it. Keep these in your password manager, not on the host:

| Item | Where it comes from | Used for |
|---|---|---|
| The age PRIVATE key (`AGE-SECRET-KEY-1...`) | `deploy.sh --backup-keygen` showed it once | decrypting. **Without it nobody can read the copies.** |
| The signing PUBLIC key (a base64 line) | the Backups page, or `--backup-rotate-signing` printed it | `--verify-key FILE`. Keep the OLD public keys too: an old copy verifies only with the key that signed it. |
| The destination's access data | your provider: endpoint, bucket, keys (S3); URL, user, password, folder (WebDAV) | the `--remote-config` file. |
| The install id (8 hex characters) | the Backups page, or the remote file names `launchpad-<ID>-<time>-<4 hex>.dump.age` | `--install-id`. After a `--backup-resume` the id changed: older copies keep the OLD id. |
| `deploy/.env` (`BACKUP_ENCRYPTION_KEY`, `SETTINGS_ENCRYPTION_KEY`, ...) | your copy | the restored database holds destination secrets sealed with these. Without them you enter the secrets again. |

### Steps (a new host, no stack yet)

1. **Get the code and build the server image.** The throwaway commands run the server image, and a new
   host has none. Use the same commit as the backups (the restore prints `made by Launchpad <version>`):

   ```bash
   git clone <repo> launchpad && cd launchpad && git checkout <commit>
   docker build -t launchpad-api:restore server
   ```

   This is the image `deploy/deploy.sh` builds (`build: ../server`); `deploy.sh` tags it
   `launchpad-api:<commit>`. If the stack runs on this host, `restore.sh` uses its image and you can leave out `--image`.
2. **Write the destination file**, mode 0600, owned by you (the script refuses anything else).
   Delete it when you are done. It holds secrets:

   ```bash
   umask 077
   cat > /srv/restore/remote.json <<'JSON'
   {"type":"s3","endpoint":"https://s3.example.com","bucket":"my-backups","region":"us-east-1",
    "folder":"launchpad","addressing":"path","accessKey":"...","secretKey":"..."}
   JSON
   ```

   WebDAV: `{"type":"webdav","url":"https://...","username":"...","password":"...","folder":"..."}`.
   SFTP (`password`, `privateKey` are the secret fields; the other fields are the destination's own config) and Dropbox
   (`appKey`, `appSecret`, `refreshToken`) are accepted by `fetch --config` by their secret names, but their file shapes
   and the restore through them are NOT covered or tried here.
   The file is mounted read-only into the fetch container. It is never an argument or an environment value.
3. **Have a database to restore into**: either a deploy.sh stack (run `deploy/deploy.sh` first: it
   makes an empty database) or any postgres container with the client tools and `PG*` variables,
   named with `--container NAME`. On a host with only a bare postgres container, no `deploy/.env` and
   no compose project are needed.
4. **Verify first** (nothing is restored):

   ```bash
   deploy/restore.sh --remote-config /srv/restore/remote.json --container pg --image launchpad-api:restore \
     --install-id 0123abcd --verify-key /srv/restore/signing.pub --identity /srv/restore/age.key \
     --verify-only latest
   ```

   It prints the signed dump time and its age, and the line about `latest`: **read it**. A signature
   does not prove that this is the newest copy. Whoever can write to the remote can put back an older
   signed pair. If the time is not the one you expect, name the copy: use its STEM
   (`launchpad-0123abcd-20261028T031500Z-9f3a`, the file name without `.dump.age` or `.json`).
5. **Restore into a scratch database**: the same command without `--verify-only`, and with `--keep
   --name devops_rollback` if you will swap it in. The key can also be typed at a hidden prompt (leave out
   `--identity`) or read from the standard input (`--identity -`, from a file or a pipe, never from a shell
   history line). `--compare` is not meaningful here.
6. **Swap it in** with block 2 of "Rollback" (one transaction), but SKIP its `git checkout <previous-commit>` part: stay on the current commit. Then run `deploy/deploy.sh`. A
   restored database has no grants (`pg_restore --no-privileges`): `deploy.sh` gives the `offsite` role its
   grants again. Then run `deploy/deploy.sh --backup-resume` BEFORE backups start: the restored database
   still has the old install id and instance id, and a copy that backs up into the original's remote
   folder is dangerous (decisions-T16 #37).
7. **Clean up**: if the script was killed (SIGKILL), delete a leftover `launchpad-restore.*` folder in the work directory yourself (it holds the encrypted object, and maybe the key file). Delete `remote.json`, the key file and any copy of the dump. In the web app, check the
   destinations: the sealed secrets open only if `BACKUP_ENCRYPTION_KEY` is the old one (otherwise enter
   them again, as after a mail-key change).

With a running stack, `--remote NAME` (a saved destination, by its name on the Backups page) runs the fetch
inside the `offsite` container and reads the install id from the database. Use `--remote-config` when the
database is empty, or when the destination is not saved in it.

### What the script checks, in order

1. The arguments (a stem is a name, never a path; the config file is a regular file, yours, mode 0600).
2. **Fetch and unpack**, in two throwaway containers of the server image (`--read-only`, `--cap-drop
   ALL`, `--log-driver none`, run as you; the fetch on the default `bridge` network or `--fetch-network`,
   unpack on `--network none`). The pipeline fails when EITHER side fails (`pipefail`). `unpack` refuses a
   work folder that cannot hold the object plus 1 GiB (`not enough free space`): use `--work-dir` on a
   bigger disk. The work folder is mode 0700 and removed at the end.
3. **Verify** (`--network none`): the Ed25519 signature of the manifest with `--verify-key`, then the
   manifest's install id, object name and dump time against the stem, then the object's sha256 and size.
4. **The database's record** (`backup_runs`), when the live database has one: a different sha256 stops the restore. Any database error (no table, no connection) counts as "no record" and the restore goes on.
5. **A whole decrypt pass with the private key, written nowhere**, BEFORE any database is made. A wrong key
   or a damaged object stops here.
6. The restore into the scratch database (the decrypt runs again and is piped into `pg_restore`; the plain
   dump is never a file), then the row counts.

`--trust-remote` replaces `--verify-key`. It skips steps 3's signature and prints a warning: a dump from
someone with write access to the remote can run arbitrary SQL as the bootstrap superuser (for example
`COPY ... PROGRAM`) because the restore connects as that role. Use it only to read a copy whose signing key is lost,
into a database you can throw away.

### Where the secrets go

- The age private key is read from a file you name, the standard input, or a hidden prompt. It is held in
  the shell, never in an argument or an environment variable. When no `age` is on the host, the throwaway
  `decrypt` container needs it as a file: the script writes it to a mode 0600 file in the 0700 work folder and
  removes the folder at exit. If `age` is installed, the script uses it and the key stays out of any file
  (`--no-host-age` forces the container). The key is shown to the throwaway container read-only.
- The destination secrets stay in the 0600 file you wrote.
- Messages are fixed sentences. A remote's own words are never printed.
- Keep the private key off the host the dump goes to: someone with the key and the encrypted copies
  can read every ticket.

### Common failures

| Message | Meaning and fix |
|---|---|
| `needs --verify-key FILE` | give the signing public key you saved, or (knowing the risk) `--trust-remote` |
| `readable by group or other` | `chmod 600` the `--remote-config` file |
| `the image '...' is not on this host` | build it (step 1) or pass `--image` |
| `this database has no install id` | pass `--install-id ID` (8 hex characters; the remote file names show it) |
| `the fetch or the unpack FAILED` | the line above says why: `not_found` (wrong install id or an empty folder), `auth_failed` / `forbidden` (the keys in the config file), `dns_failed` / `connect_refused` / `timeout` (network), `ssrf_blocked` (the host is on a private address: add `--allow-cidr`), `tls_untrusted` / `tls_wrong_host`, `not enough free space` |
| `signature or hash check FAILED` | `signature is not valid` (a wrong `--verify-key`, a rotated signing key: try the older public keys, or a changed manifest), `does not match its manifest` (the object changed or is cut), `does not belong to this object` (a renamed or swapped pair, or another install id) |
| `differs from the database record` | the object is not the one this database recorded: do not trust it |
| `private key does not decrypt this backup` | the wrong age key (the recipient may have been replaced: try the older keys), or the object is damaged |
| `manifest names another backup than the one asked for` | a file was swapped on the remote |

Private ranges: a NAS or Nextcloud on your own network is refused by the fetch's guarded dialer
unless the range is listed. `--remote-config` takes `--allow-cidr 192.168.1.0/24`; with `--remote NAME` the
worker's `BACKUP_ALLOW_PRIVATE_CIDRS` applies. The container's default gateway and the loopback range stay refused.

### A drill

Do this once a quarter, on a spare machine or the production host (it restores into a scratch database and drops it):
`--verify-only latest`, then the full command with the same flags, and read the `rows` lines, the migration
version and the dump age. `make restore-test` runs the same path against a fake remote on every CI run.

## Remove a deploy and start over (`--nuke`)

For a trial stack, or a fresh start before you hold real data:

```bash
deploy/deploy.sh --nuke
```

It deletes this deploy's containers and networks and ALL its volumes (the database, the attachments, the dumps and the off-site scratch volume `<project>_offsite_work`; the list that follows names the first three):
`<project>_postgres_data` (the database), `<project>_silo_data` (the
attachments) and `<project>_backups` (every nightly dump, so no restore
point is left). Nothing else: no other compose project, no image and no
file unless you ask.

- **It asks first.** It prints the project name (from `deploy/.env`, the
  only authority) and what is deleted, then wants the project name typed.
  Anything else stops it, and nothing is removed. With no terminal, give
  the name on the command line: `--yes-delete-everything <project>`, the
  exact name. A bare `--yes` is refused.
- **It refuses** a project with no `deploy/.env` (unknown) or an empty
  `DEPLOY_PROJECT`, an exported `DEPLOY_PROJECT` that is not the file's, a
  dev stack's name (`dev-ops`, `dev-ops-<ticket>`, `dev-ops-ci`), and, through
  the project guard, any project whose containers another compose file made
  or whose volumes lack the `dev.orbit.deploy=production` label.
- **`deploy/.env` is kept** by default, so a re-run of `deploy/deploy.sh`
  reuses its host names and secrets on the new, empty volumes. Add
  `--nuke-env` to delete it too (the next run writes new secrets). The file
  is deleted only after every volume is confirmed gone.
- **Images are kept** by default. `--nuke-images` also removes the
  `launchpad-api:<commit>` and `launchpad-web:<commit>` images that the
  project's app and web containers ran, and the old `dev-ops-api` /
  `dev-ops-web` ones when the containers still run them (a host deployed
  before the rename; never a base image). It goes by
  the containers' image ids: the tags are global, and a later build of the
  same commit by another deploy on this host moves the tag to that deploy's
  new image (measured). Then this project's image is removed by id and the
  tag is left alone. An image that a container outside the project still
  uses is kept and named, and the run exits non-zero.
- Every docker call that removes something goes through the clean-environment
  runner. The read-only checks call docker directly. It never prints a
  secret, and exits non-zero if anything of the project is left (for
  example a volume that a container outside the project still uses); it
  names what is left and keeps `deploy/.env`.
- A setup link in the app log is mentioned (never printed), and goes with
  it. After a nuke, `deploy/deploy.sh` is a fresh first deploy: an empty
  database and a new setup link (`--setup-link`).

Measured on 2026-10-25 (Mac, Docker 29.9, compose 5.6) with two
throwaway stacks deployed by `deploy.sh` (own projects, high ports, fake
host names; the second with its own `DEPLOY_ENV_FILE`): `--nuke` of the
first removed its 6 containers, 2 networks and 3 volumes in under 2 s and
left the second stack's containers healthy and its volumes in place; a
re-run of `deploy.sh` on the kept `deploy/.env` was a fresh first deploy
(first backup, smoke OK). The other project's name, on
the command line or typed, was refused, as were no terminal and `--yes`.
After the second stack's build of the same commit had moved both tags, the
first stack's own images were gone once its containers were (this Docker
deletes an untagged image with no container), and the tags, now the second
stack's images, stayed until that stack's own `--nuke --nuke-images`.

**Do not use it on a production stack that holds real data**:
the backups go with the database, so nothing is left to restore. Copy the
dumps and the silo data off the host first ("Backup and restore") if you
may ever need them. **Never on a dev stack** (`server/docker-compose.yml`
has its own `docker compose -p <name> down -v`).

## Security notes

- **Unsubscribe token in proxy logs.** The one-click unsubscribe is
  `POST /api/v1/notifications/unsubscribe/<token>`. The app hides the token
  in its own logs, but your reverse proxy's access log (and nginx's) will
  hold the path of that POST. The token works once, so the risk is low; you
  can drop that path from the proxy log if you wish.
- Published: only nginx (4001) and silo's S3 API (4002), on
  `DEPLOY_BIND_ADDR` (default `127.0.0.1`; see the firewall note at the
  top). postgres and the app publish nothing; postgres and
  the backup job sit on an `internal` network with no route out.
  `make compose-check` fails on any other port, a Docker socket mount, a
  privileged or host-network service, a host bind mount other than the two
  read-only config files, a missing `no-new-privileges` (on any service but
  `volume-init`, see below), an extra capability, an unpinned image or
  Dockerfile base, or an app environment without `LOG_ENV=production` (or
  with `AUTH_FIXED_OTP`).
- **`volume-init` has no `no-new-privileges`.** Every other
  service has it. On a Linux 7.0 server (kernel 7.0.0-38-generic, Docker
  29.9.2, AppArmor and the default seccomp profile), `cap_drop: [ALL]` +
  `cap_add: [CHOWN]` + `no-new-privileges` stops the container at its first
  exec: `exec /bin/sh: operation not permitted`, exit 255. Adding
  `DAC_OVERRIDE` does not help. The same capabilities without
  `no-new-privileges` work. Docker Desktop on macOS ran the shipped form
  (with the flag) without the error, so CI and Mac runs did not show it.
  The exception is acceptable because `volume-init` runs once, as root, with no network and no capability but
  `CHOWN`, and only changes the owner of two of its own volumes.
  `make compose-check` fails if it gets another capability, loses
  `cap_drop: [ALL]`, gets any network (anything but `network_mode: none`)
  or another `security_opt`, or stops being one-shot (`restart: "no"`).
  `deploy/test-compose-check.sh` (in `make compose-check`) feeds the
  checker mutated renders to prove each of these rules fails.
- **`web` uses the same pattern and is not measured on Linux 7.0.** It has
  `cap_drop: [ALL]`, `cap_add: [CHOWN, SETUID, SETGID]` and
  `no-new-privileges`, so it can fail in the same way on that kernel. If
  `web` does not start, run `docker logs launchpad-web-1` (your project
  name in place of `launchpad`). A line `exec ... operation not permitted`
  is this fault. Do not remove the flag from `web` by hand; report it, so
  the change goes through the compose check.
- Users: app uid 10001 (read-only root, no capabilities), silo uid 10001
  (no capabilities), backup uid 70 (read-only root, no capabilities),
  postgres drops to its own user. nginx's master runs as root to start (it
  renders its templates) with only `CHOWN`, `SETUID`, `SETGID`, a read-only
  root and its workers as `nginx`.
- The app's silo key is limited to the one bucket (get, put, delete, list);
  the silo root key stays in `deploy/.env` for the script. The app's secret
  reaches `mcli admin user add` on stdin (mcli reads it there when the
  argument is left out), not on a command line.
- nginx: the API body limit is 2 MiB (the server's own is 1 MiB, so it
  answers with its JSON error), static pages take no body and allow only GET
  and HEAD, the upstream is a fixed name (no open proxy), unknown hosts get
  421, and `server_tokens` is off. The access log has no client address, no
  User-Agent and no query string. (The error log does name the client,
  which is the outer proxy, not the browser.)
- All container logs rotate at 10 MB x 5.
- `deploy/.env` holds every secret, mode 0600, gitignored, on the server
  only. Nothing in the repo is a real secret; `.env.example` holds
  placeholders.

## Attachment (files) backup
The off-site worker copies the attachments (the silo bucket) to every enabled destination, after each backup run's dump. Design:
`docs/internal/decisions-T16-backups.md` #38 and "Build notes: ticket 14". It is on when the `offsite` service has its `S3_*`
variables, which the production compose file always sets. It is a copy; silo stays the store.

- **What is copied.** Every object under `org/` in the bucket, one age file each, encrypted to the SAME recipients as the dumps.
  The first run copies everything (hours for a large bucket); later runs copy only new or changed objects. A deleted attachment
  is deleted from the destination only by the files retention ("Attachment restore and files retention" below): after it has been gone from silo for at least 30 days and longer than the oldest kept dump.
- **Where it goes.** In two sub folders of the destination's folder: `launchpad-files-<install id>` (the encrypted lists and
  signed manifests, one pair per run) and `launchpad-files-<install id>-objects` (the attachments). The dump folder is unchanged.
- **The read-only silo user.** `deploy.sh` makes `OFFSITE_S3_ACCESS_KEY` / `OFFSITE_S3_SECRET_KEY` in `deploy/.env` (once, never
  again; an older `deploy/.env` gets them appended) and a silo user with the policy `devops-offsite-ro`: it can list and read the
  bucket and cannot write or delete. It is not the app's key and not silo's root key. KEEP A COPY OF `deploy/.env` off the host
  as before. `offsite` reaches silo over the internal network `files_net` (only `silo` and `offsite` are on it) and nothing else
  of ours: it is still not on `edge` or `backend`.
- **First update past this change.** `deploy.sh --update` appends the two keys, recreates `silo` (it joins `files_net`: a short
  interruption of attachment downloads) and `offsite`, and makes the silo user. Run it, then press "Back up now" on the Backups
  page once. The status shows "files not backed up" until a run has copied the files.
- **Where to look.** The Backups page banner "files not backed up" (no good files copy in the last 48 hours). A run's steps show
  a `files` entry: objects, how many were uploaded, bytes, skipped, failed, milliseconds. The run's `error_text` says `files_failed`
  (the files phase stopped; the dump is fine) or `files_incomplete` (some objects were skipped or unreadable). The first run's
  `initial: true` step is the first full size and time.
- **The state file.** `offsite` remembers what it copied in `files-state-<destination id>.jsonl` on the `offsite_work` volume (it
  cannot read the encrypted list: it has no private key). If the volume is lost, the next run copies everything again under the
  same names (no duplicates). Do not edit it.
- **Restore one attachment** (by hand; `restore.sh --remote --files` now restores them all or one: see "Attachment restore and files retention" below). With the
  throwaway-container pattern of "Restore from a remote copy" (a terminal, `--network` limited, the private key read from a
  file), the commands are, in order: `server offsite fetch --destination NAME --files | server offsite unpack --out DIR`;
  `server offsite verify --files --public-key PUB --manifest DIR/manifest.json --object DIR/object --install-id ID --stem STEM`;
  `server offsite decrypt --identity KEY < DIR/object > list.jsonl`; `server offsite files-entry --install-id ID --key
  'org/<org id>/<yyyy>/<uuid>/<file>' < list.jsonl` (prints the remote name, the sha256 and the size);
  `server offsite fetch --destination NAME --file-object REMOTE | server offsite unpack --out DIR2`;
  `server offsite decrypt --identity KEY < DIR2/object > file`; compare `sha256sum file` with the printed sha256. Put the file
  back with the attachment procedure of the app, or into the silo volume by hand (never over a silo you have not checked).
- **Limits.** 200 000 objects in the list; an object over 4 GiB, a key with a control character and a key over 1024 bytes are
  skipped and counted. WebDAV, SFTP and Dropbox list a whole folder with a 16 MiB cap, so the daily spot check of the destination is skipped
  for a very large files folder; S3 destinations are not affected. Retention removes only objects and lists past the window (see below).

## Attachment restore and files retention
### Retention of the attachment copy

- **What is deleted.** A remote attachment object is deleted only when it has not been in the silo listing for longer than the
  window, where the window is the larger of 30 days and the age of the oldest dump kept on that destination plus one day. An object
  that is in silo now is never deleted, whatever its age. Old attachment lists and manifests older than the window are deleted
  with it (the newest pair is never deleted). Dump retention (`keep_last`) is unchanged.
- **Where it shows.** The `files` step of a backup run has `pruned`, `prunedBytes`, `pruneFailed`, `listsPruned`. A failed delete
  shows as `prune_failed` in `error_text`; the object stays and the next run tries again. `pruneSkipped: true` means nothing was pruned that run; `pruneSkip` says why: `no_window` (the dump listing failed), `empty_listing` (silo listed nothing but the state has objects), `bad_keys` (the listing held a key the phase could not read), `long_gap` (no key was seen for longer than the window: the next run prunes) or `mass_gone` (more than half of the state would be pruned at once: a silo that lists only a subset is not trusted). One run deletes at most half of the state entries.
  To clear a `mass_gone` you are sure about (you really removed those objects), wait for the running backup to end (a running worker rewrites the file), then delete the state file of that destination and run a backup: `$DC exec -T offsite rm -- /offsite-work/files-state-<destination id>.jsonl` (`$DC` as in "Rollback"). The next run copies everything again and prunes nothing; the objects you removed stay on the destination until you delete them there.
- **Today it rarely deletes anything.** The app has no way to delete an attachment, so silo objects stay in the bucket. The
  retention is there for the day the app does.
- **State files.** `offsite` keeps `files-state-<destination id>.jsonl` on the `offsite_work` volume. A destination you delete leaves
  its file behind (it holds silo keys). Remove it by hand:
  `$DC exec -T offsite rm -- /offsite-work/files-state-<destination id>.jsonl` (wait until no backup is running; the id is in the Backups page URL
  or `SELECT id, name FROM backup_destinations`). Losing a state file is safe: the next run copies everything again.

### Restore the attachments: `restore.sh --remote --files`

Run it like the database restore of "Restore from a remote copy" (same flags, same saved items: the age private key, the
signing public key, the install id). Add `--files` and a NEW or EMPTY folder:

```
deploy/restore.sh --remote-config FILE --identity KEY --verify-key signing.pub --install-id ID --image IMAGE \
  --files --files-out /srv/restored-attachments latest
```

- It restores the attachments only, never the database. Restore the database first (or at the same time) if you rebuild a host.
- `latest` or a list stem `launchpad-files-<id>-list-<time>-<4 hex>`. `--files-key org/<org id>/<yyyy>/<uuid>/<file>` restores one
  object. `--verify-only` checks the signature, the list and your key and writes nothing.
- The folder will hold every attachment in PLAIN form (mode 0600, under `<silo key>`): keep it on a protected disk and delete it
  when the objects are in the new bucket. The script never overwrites or deletes your files: a folder that holds anything is
  refused. On a failure it empties the folder it was filling.
- Each object is checked against the SIGNED list (the stored copy's size and sha256, then the plain size and sha256) before it
  moves into the folder. A change anywhere stops the restore. The script prints only "object N of M", never a file name.
- It is slow: about a second or more per object (three short containers). For a large bucket use a terminal that stays open.
- **Put the files in a new silo.** Start the new stack and let `deploy.sh` make the bucket. Then copy the folder into the bucket
  with an S3 client and the app's key (`S3_ACCESS_KEY` / `S3_SECRET_KEY` in `deploy/.env`) against the files port, for example
  `aws s3 sync /srv/restored-attachments s3://<S3_BUCKET>/ --endpoint-url http://127.0.0.1:<DEPLOY_FILES_PORT>` (this command has
  not been run against silo). Do not write into silo's data folder. Then open a few attachments in the app and compare.
  Never delete the old silo volume or the folder until you have opened attachments from the new bucket ("never delete before known good").

### What the retention guards do not protect
- The first 30 days after silo loses objects are the recovery period: nothing is deleted from the destination before then. After it,
  a silo that loses LESS than half of its objects, and stays that way for the whole window, gets exactly those remote copies
  deleted. They may be the only copy, and the newest dump may still name them. Watch the `gone` count in the `files` step of the
  runs (nothing alerts on it). A bucket that keeps growing dilutes the `mass_gone` ratio, so the guard protects less over time.

### Restore from a remote copy: the typed install-id confirmation

- **The private key on a terminal** is never echoed, also with `--identity -`.
- **A rollback warning.** When the live database records a succeeded backup newer than the copy you restore (also with
  `latest`), the script prints `ROLLBACK: ...`. It does not stop; check that you mean it. On a new host the database has no
  record, so there is nothing to compare; the warning says so only when the question itself failed ("could not be asked").
- **`--trust-remote`** now asks before it fetches anything: type the install id at the prompt, or give
  `--yes-trust-remote INSTALL_ID` when there is no terminal. Use `--verify-key` with the saved signing public key whenever you can.
- **Free space.** The script measures the plain dump during its decrypt check and stops when the database volume (where the
  script can see it: the postgres container) has less than that. For `--files` it stops before the first write when the
  `--files-out` file system or the work folder cannot hold the files. The database needs more than the dump: leave room.
- **The `--remote-config` file** is checked and then mounted from the same resolved path; if the file is replaced between the
  check and the fetch, the script stops. Run it again.
- Still not tried: Linux hosts, a real `offsite` container for `--remote NAME`, a production-size dump, the image build recipe on a clean host.
