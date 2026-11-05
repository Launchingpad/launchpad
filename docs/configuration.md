# Configuration

This page lists every environment variable that Launchpad reads. It was
checked against the code in `server/internal/config`, `server/cmd/*`,
`deploy/` and `web/`.

Read the table like this:

- **Used by**: `server` (the API, `server/cmd/server`), `mcp` (the MCP
  server), `importer`, `migrate` (`server/cmd/migrate`), `deploy` (the
  scripts in `deploy/`, read from the shell or from `deploy/.env`), `dev`
  (the local stack in `server/docker-compose.yml` and its scripts), `compose`
  (the root `docker-compose.yml`, read from `.env`; see
  [`self-hosting.md`](self-hosting.md)), `web`
  (the web app build and its nginx image), `test` (the test suites).
- **Required**: "yes" means the program stops at start without it.
- A secret must come from the environment (or `deploy/.env`), never from a
  file in the repository. The server never logs a secret.

The production stack sets most server variables for you (see
[`self-hosting.md`](self-hosting.md)). You edit `deploy/.env` only for the
`deploy` rows. The compose file passes the other values on.

| Name | Used by | Default | Required | Meaning |
|---|---|---|---|---|
| `DATABASE_URL` | server, migrate, importer | none | yes | Postgres connection URL. Postgres 15 or newer. |
| `PORT` | server | `8080` | no | Port the API listens on (1 to 65535). |
| `LOG_ENV` | server | none | yes | One of `local`, `test`, `staging`, `production`. Only `local` and `test` turn on dev features (logged login codes, `AUTH_FIXED_OTP`, the public dev keys). `local` also sets debug logging and plain cookie names without `Secure`. Any other value sets `Secure` cookies. The production compose file fixes it to `production`. |
| `PUBLIC_BASE_URL` | server, importer | empty | no | Origin of the web app, for example `https://launchpad.example.com`. Used in setup links, invite links and `downloadUrl`. Empty: those links are bare paths. The importer takes it as the default of `--public-base-url`. |
| `WEB_ORIGINS` | server | empty | no | Exact origins that get CORS headers, comma separated, as `scheme://host[:port]`. No wildcard, path or query. Empty: no CORS headers. |
| `TRUSTED_PROXY_CIDRS` | server, deploy | empty | no | Comma list of CIDRs or addresses of proxies whose `X-Forwarded-For` the server believes, for the per-client sign-up limits. Empty: no proxy is trusted and a proxied request gets no per-client limit. `0.0.0.0/0` and `::/0` are refused. In production, put it in `deploy/.env` (the compose file passes it on). |
| `AUTH_OTP_PEPPER` | server | none (the dev compose file has a public default) | yes | Secret key (HMAC) for stored login codes. At least 32 bytes. `deploy.sh` generates one (48 random bytes as hex). |
| `AUTH_FIXED_OTP` | server, dev | empty | no | Six digits. Every login code is this value. Allowed only when `LOG_ENV` is `local` or `test` and no Postmark token is set; any other use stops the boot. |
| `POSTMARK_SERVER_TOKEN` | server, deploy | empty | no | Postmark server token. With it, login codes go out by Postmark. Without it, login on a non-dev server uses the mail settings stored in the app, or is off. Needs `POSTMARK_FROM`. |
| `POSTMARK_FROM` | server, deploy | empty | with the token | Sender address for Postmark. |
| `POSTMARK_MESSAGE_STREAM` | server | `outbound` | no | Postmark message stream. |
| `SETTINGS_ENCRYPTION_KEY` | server, deploy | none (the dev compose file has a public default) | no | 64 hex characters (32 random bytes: `openssl rand -hex 32`). Seals the Postmark token and SMTP password that a platform admin stores in the app, and the tokens and webhook secrets of git connections. There is no rotation: a changed key makes the sealed values unreadable. Without it the server starts, but storing a mail secret fails with `409`. Outside `local` and `test` the server refuses the public dev key and an all-zero key. `deploy.sh` generates one. |
| `S3_ENDPOINT` | server, importer | none | with the other `S3_` values | URL the server uses to reach the object store (silo or another S3-compatible store). |
| `S3_BUCKET` | server, importer, deploy | `devops-attachments` in `deploy` and the dev stack | with the other `S3_` values | Bucket for attachments. |
| `S3_ACCESS_KEY` | server, importer, deploy | none (`deploy.sh` generates it) | with the other `S3_` values | Access key. |
| `S3_SECRET_KEY` | server, importer, deploy | none (`deploy.sh` generates it) | with the other `S3_` values | Secret key. Outside `local` and `test` the server refuses the public dev value. |
| `S3_PUBLIC_ENDPOINT` | server | the value of `S3_ENDPOINT` | no | Endpoint in the presigned URLs given to clients. Must be `https` outside `local` and `test`. |
| `S3_REGION` | server, importer | `us-east-1` | no | Region used to sign requests. |
| `LAUNCHPAD_URL` | mcp | none | yes | Base URL of the Launchpad server. Plain `http` is accepted only for a loopback host. |
| `LAUNCHPAD_TOKEN` | mcp | none | yes | API token the MCP server uses. Never put it in a committed file. |
| `LAUNCHPAD_ORG` | mcp | none | yes | Slug of the org. |
| `LAUNCHPAD_TEAM` | mcp | none | no | Default team (key, name or id) for tools that need one. |
| `LAUNCHPAD_ALLOW_INSECURE_HTTP` | mcp | unset | no | Set to `1` to allow plain `http` to a non-loopback host (a private network). |
| `LAUNCHPAD_FILES_URL` | mcp | unset | no | Origin of the file store's public endpoint (for example `https://launchpad-files.example.com`). Needed by `download_attachment`: a redirect to any other host is refused. Must be https unless the host is loopback. |
| `LAUNCHPAD_DOWNLOAD_DIR` | mcp | a 0700 folder `launchpad-mcp` under the OS temp dir | no | Where `download_attachment` saves files. Owned by you, mode 0700, not a symlink. |
| `LAUNCHPAD_DOWNLOAD_MAX_BYTES` | mcp | 25 MiB | no | Size cap of one download. The hard cap is 100 MiB. |
| `LINEAR_API_KEY` | importer | none | yes, for `export` | Linear API key for the command line importer, from the environment only. The client is read-only. The web import does not use it: the key is typed in the web app, sent once and kept in memory only. |
| `GIT_ALLOW_PRIVATE_PROVIDERS` | server, deploy | unset | no | `1` or `true` lets the git API clients reach the standard private ranges (`10/8`, `172.16/12`, `192.168/16`, `fc00::/7`, `100.64/10`). Default: public hosts only. Never opens loopback, link-local, the metadata addresses, the app's own networks or the gateway. `0` and `false` mean off; any other value stops the boot. See `deploy.md`. |
| `GIT_ALLOW_PRIVATE_CIDRS` | server, deploy | empty | no | Comma list of private ranges the git API clients may reach. No `/0`, nothing shorter than `/8` (IPv4) or `/32` (IPv6); a bad value stops the boot. Combined with the switch above (the union). |
| `IMPORT_WORK_DIR` | server | `/work/import` | no | Absolute path of the folder for web import exports (ticket text and user emails; mode 0700). The production compose file mounts the `import_work` volume there. |
| `DEPLOY_PROJECT` | deploy | `launchpad` | no | Compose project of the production stack. Never `dev-ops` or a `dev-ops-*` name. |
| `WEB_HOST` | deploy, web | none (a first run must set it) | yes, on a first run | Host name of the web app and API. The nginx image uses it as its `server_name`; silo allows CORS from it. |
| `FILES_HOST` | deploy | none (a first run must set it) | yes, on a first run | Host name of the file store, used in presigned URLs. |
| `DEPLOY_BIND_ADDR` | deploy | `127.0.0.1` | no | Address the two published ports bind to. See `deploy.md` for how to choose it. |
| `DEPLOY_WEB_PORT` | deploy | `4001` | no | Host port of the web app (nginx). |
| `DEPLOY_FILES_PORT` | deploy | `4002` | no | Host port of the file store (silo S3 API). |
| `BACKUP_AT_UTC` | deploy | `03:15` | no | Time of the nightly database dump, in UTC. |
| `BACKUP_KEEP_DAYS` | deploy | `30` | no | Days to keep dumps. A whole number, 1 or more. |
| `POSTGRES_PASSWORD` | deploy | generated | yes (generated) | Password of the database user. `deploy.sh` makes it and never makes it again. |
| `SILO_ROOT_USER` | deploy | generated | yes (generated) | Root user of silo. Never generated again. |
| `SILO_ROOT_PASSWORD` | deploy | generated | yes (generated) | Root password of silo. Never generated again. |
| `LAUNCHPAD_VERSION` | compose | none | yes | Release version of the two images (`ghcr.io/launchingpad/launchpad-api` and `launchpad-web`), the tag without the `v`, for example `1.2.3`. No default and no `latest`. Change it to update. |
| `LAUNCHPAD_DATA_DIR` | compose | `./data` | no | Host folder for the data of the root compose file. A relative path is relative to `docker-compose.yml`. Holds `postgres`, `files` and `backups`. |
| `LAUNCHPAD_DB_DIR` | compose | `$LAUNCHPAD_DATA_DIR/postgres` | no | Host folder of the database. |
| `LAUNCHPAD_FILES_DIR` | compose | `$LAUNCHPAD_DATA_DIR/files` | no | Host folder of the attachments (silo's data). |
| `LAUNCHPAD_BACKUP_DIR` | compose | `$LAUNCHPAD_DATA_DIR/backups` | no | Host folder of the nightly dumps. |
| `LAUNCHPAD_BIND_ADDR` | compose | `127.0.0.1` | no | Address the two published ports bind to. Read "Set up the reverse proxy" in `self-hosting.md` before you change it. |
| `LAUNCHPAD_WEB_PORT` | compose | `4001` | no | Host port of the web app (nginx). |
| `LAUNCHPAD_FILES_PORT` | compose | `4002` | no | Host port of silo (presigned file URLs). |
| `WEB_HOST`, `FILES_HOST` | compose | none | yes | The two DNS names your proxy serves. No scheme, no path. `scripts/gen-env.sh` asks for them. |
| `POSTGRES_PASSWORD`, `AUTH_OTP_PEPPER`, `SETTINGS_ENCRYPTION_KEY`, `BACKUP_ENCRYPTION_KEY`, `SILO_ROOT_USER`, `SILO_ROOT_PASSWORD`, `S3_ACCESS_KEY`, `S3_SECRET_KEY` | compose | none | yes | Secrets and silo logins. `scripts/gen-env.sh` makes random values. `S3_BUCKET` (default `launchpad-attachments`) is the bucket that `silo-init` makes. `BACKUP_AT_UTC`, `BACKUP_KEEP_DAYS` and the optional `POSTMARK_*`, `TRUSTED_PROXY_CIDRS`, `GIT_ALLOW_*` work as in the `deploy` rows. |
| `DEPLOY_ENV_FILE` | deploy | `deploy/.env` | no | Path of the env file the scripts read. |
| `DEV_OPS_PROJECT` | dev | none | yes, or pass `-p` | Compose project name for `server/docker-compose.yml`. Put it in `server/.env`. `COMPOSE_PROJECT_NAME` also works. |
| `DEV_OPS_APP_PORT` | dev | `8090` | no | Host port of the API in the local stack (127.0.0.1 only). |
| `DEV_OPS_PG_PORT` | dev | `5433` | no | Host port of Postgres in the local stack. |
| `DEV_OPS_S3_PORT` | dev | `9100` | no | Host port of silo in the local stack. |
| `PROJECT`, `BASE_URL`, `SETUP_USERNAME`, `SETUP_NAME`, `SETUP_EMAIL`, `ORG_SLUG`, `ORG_NAME`, `TEAM_KEY`, `TEAM_NAME` | dev | `SETUP_USERNAME` `admin`, `SETUP_NAME` `Admin`, `SETUP_EMAIL` `admin@example.com`, `ORG_SLUG` `orbit`, `ORG_NAME` `Orbit`, `TEAM_KEY` `ORB`, `TEAM_NAME` `Orbit` | no | Settings of `server/scripts/dev-setup.sh`. `PROJECT` falls back to `DEV_OPS_PROJECT`. `BASE_URL` defaults to `http://localhost:$DEV_OPS_APP_PORT` (8090). The defaults are generic and fine on a throwaway stack. |
| `BASE_URL`, `TOKEN`, `ORG`, `TEAM`, `SMOKE_TMPDIR` | dev | `BASE_URL` `http://localhost:8090`, `ORG` `orbit`, `TEAM` `ORB` | `TOKEN` yes | Settings of `server/scripts/smoke.sh`. `TOKEN` is an org-admin token. The defaults match the `dev-setup.sh` defaults. `SMOKE_TMPDIR` must be a writable directory. |
| `API_ORIGIN` | web | empty | no | Image setting of the web container: the browser-facing origin of the API. Empty: the API is on the same origin. |
| `FILES_ORIGIN` | web | empty | no | Image setting of the web container: the origin of the file store, added to the content security policy (connect-src, img-src and media-src). The production compose file sets it from `FILES_HOST`. |
| `VITE_API_BASE` | web | empty | no | Build-time API base URL, no trailing slash. Empty: same origin. |
| `VITE_PROXY_TARGET` | web | `http://localhost:18091` | no | Dev server only: where `/api` is proxied. |
| `VITE_DEV_TOKEN` | web | unset | no | Dev server only: a bearer token used instead of the cookie login. A production build fails if it is set. |
| `INTEGRATION` | test | unset | no | Set to `1` to run the database-backed tests. Without it they are skipped. |
| `TEST_DATABASE_URL` | test | `postgres://devops:devops@localhost:5433/devops?sslmode=disable` | no | Postgres for the integration tests. |
| `TEST_S3_ENDPOINT` | test | `http://localhost:9100` | no | silo for the integration tests. |
| `TEST_S3_BUCKET`, `TEST_S3_ACCESS_KEY`, `TEST_S3_SECRET_KEY` | test | `devops-test`, `devops`, `devops-local-s3-secret` | no | Bucket and keys for the integration tests. |

## Notes

- **S3 variables:** set `S3_ENDPOINT`, `S3_BUCKET`, `S3_ACCESS_KEY` and
  `S3_SECRET_KEY` all together, or none. None: attachments are off and the
  file routes answer `503 attachments_unavailable`. A partial set stops the
  boot.
- **Login:** the server needs `AUTH_OTP_PEPPER` at every boot. Login also
  needs a mail transport: `POSTMARK_*`, or the mail settings in the app.
  Without one (outside `local` and `test`), the login routes answer
  `503 login_unavailable`.
- **Hosts of the deploy script:** `WEB_HOST` and `FILES_HOST` have no
  default. A first run must set both: type them at the prompt, or export them
  (or use `--non-interactive` with them exported). After the first run, `deploy/.env` is
  the authority.
- **The deploy environment is cleaned:** `deploy.sh` runs `docker compose`
  with an empty environment (plus a few Docker and proxy variables), so a
  variable such as `LOG_ENV` exported in your shell never reaches the
  production stack. Edit `deploy/.env` instead.
- **The root compose file** (`docker-compose.yml`) reads `.env`, not
  `deploy/.env`. Its template is `.env.example`; `scripts/gen-env.sh` fills it.
  It does not use the `DEPLOY_*` variables.
- `deploy/.env` also holds `DEPLOY_PROJECT` and the other `deploy` rows.
  `deploy/.env.example` lists the keys with placeholder values.
- The MCP server also takes a flag: `--profile full` (default) or
  `--profile reviewer`. See [`mcp.md`](mcp.md).
