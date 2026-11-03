# Development: run the dev stack on your computer

This page starts the development stack with Docker only: Postgres, silo (the
file store) and the API. You then set up the first user and sign in. To
install Launchpad for real use, read [`quickstart.md`](quickstart.md).

This stack is for development. It has public default passwords. It binds to
`127.0.0.1` only. Do not expose it. To run Launchpad on a server, read
[`self-hosting.md`](self-hosting.md).

You need Docker with Compose v2, `curl` and `python3`. For `make smoke` (step 6) you also need `make`.

## 1. Pick a compose project name

The compose file in `server/` has no default project name. You must give one.
A name made from the folder would make every checkout called `server` one
project, and `down -v` in one would delete the data of another.

Pick a name, for example `launchpad-dev`. Use it with `-p` in every
`docker compose` command below. (Another way: put
`DEV_OPS_PROJECT=launchpad-dev` in `server/.env`, which git ignores.)

## 2. Start the stack

```bash
cd server
AUTH_FIXED_OTP=000000 docker compose -p launchpad-dev up -d --build
```

`AUTH_FIXED_OTP` makes every login code `000000`, so you do not need email.
The server accepts it only when `LOG_ENV` is `local` or `test` (the compose
default is `local`) and no Postmark token is set. The first build takes a few
minutes.

The value reaches the app only when compose creates the container. If you
run `up` again later (or compose recreates the app) without it, the app
starts with real login and no fixed code. To keep it, put the line
`AUTH_FIXED_OTP=000000` in `server/.env` (compose reads that file for
variable values), or set it on every `up`.

Ports on `127.0.0.1`:

| Service | Default port | Change with |
|---|---|---|
| API | 8090 | `DEV_OPS_APP_PORT` |
| Postgres | 5433 | `DEV_OPS_PG_PORT` |
| silo (S3 API) | 9100 | `DEV_OPS_S3_PORT` |

If 8090 is taken on your computer, set `DEV_OPS_APP_PORT` (for example
`DEV_OPS_APP_PORT=8091`) on the `up` command, and also `PUBLIC_BASE_URL=http://localhost:8091` (its default names port 8090). Use the same port in every command below. Keep both on every later `up`, or put them in `server/.env`.

Wait until the API answers (it can take a minute after the build):

```bash
for i in $(seq 1 60); do
  curl -fsS "http://localhost:${DEV_OPS_APP_PORT:-8090}/healthz" && break
  sleep 2
done
# {"status":"ok"}
```

## 3. First-run setup

There is no seed data. While the database has no user, the server logs one
setup link at each boot (valid 24 hours, one use). The script
`scripts/dev-setup.sh` reads that link from the log, completes setup and
prints the first admin token once:

```bash
cd server        # if you are not there already
PROJECT=launchpad-dev DEV_OPS_APP_PORT="${DEV_OPS_APP_PORT:-8090}" \
SETUP_USERNAME=admin SETUP_NAME=Admin SETUP_EMAIL=you@example.com \
ORG_SLUG=acme ORG_NAME=Acme TEAM_KEY=ACME TEAM_NAME=Acme \
  scripts/dev-setup.sh
```

Without these variables the script uses generic defaults: `admin`,
`Admin`, `admin@example.com`, org `orbit` (`Orbit`), team `ORB` (`Orbit`).
They are fine for a throwaway stack. Set your own if you want other values.

The script makes, in one transaction: the first user (a platform admin and
the org's admin), the org, its first team with nine workflow states (Triage first), the
default labels, and the first admin token. Copy the token to a safe place.
It is not shown again. If setup is done already, the script says so. To start
over, run `docker compose -p launchpad-dev down -v` (this deletes the
database) and go back to step 2.

## 4. Sign in

Login uses an email one-time code. With `AUTH_FIXED_OTP=000000` the code is
always `000000`:

```bash
curl -sS -c cookies.txt -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com"}' http://localhost:8090/api/v1/auth/otp
curl -sS -c cookies.txt -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","code":"000000"}' \
  http://localhost:8090/api/v1/auth/otp/verify
```

For a native app, add `"client":"native"` to the verify body. The answer then
holds a `token` (no cookie is set); send it as `Authorization: Bearer <token>`,
and `POST /api/v1/auth/logout` revokes it.

The session is a cookie. A local server (`LOG_ENV=local`) uses the plain
names `odt_session` and `odt_csrf`. Delete `cookies.txt` when you are done.

Without `AUTH_FIXED_OTP`, a local server writes each code to the app log:

```bash
docker compose -p launchpad-dev logs app | grep 'login code'
```

You can also use the token from step 3. Type it at a hidden prompt, so it
stays out of your shell history:

```bash
read -r -s TOKEN
printf 'Authorization: Bearer %s\n' "$TOKEN" \
  | curl -sS -H @- http://localhost:8090/api/v1/me
```

## 5. Run the web app (optional)

The local stack runs the API only. To use the web app, start the Vite dev
server (Node 22 or newer). It proxies `/api` to the API:

```bash
cd web
npm ci
VITE_PROXY_TARGET=http://localhost:8090 npm run dev
```

Open http://localhost:5173 and sign in with your email and the code
`000000`. (`VITE_PROXY_TARGET` defaults to `http://localhost:18091`.)

## 6. Try the API and the smoke test

The OpenAPI file is `server/api/openapi.yaml`. A script runs a set of calls
against the stack:

```bash
cd server
BASE_URL=http://localhost:8090 TOKEN=<your token> ORG=acme TEAM=ACME \
  SMOKE_TMPDIR=<a writable directory> make smoke
```

The smoke test needs an org-admin token (the first admin token is one).

## Stop and clean up

```bash
docker compose -p launchpad-dev down        # stop, keep the data
docker compose -p launchpad-dev down -v     # stop and delete the data
```

## Next steps

- Install for real use: [`quickstart.md`](quickstart.md).
- Connect an agent: [`mcp.md`](mcp.md).
- Run the tests: [`../CONTRIBUTING.md`](../CONTRIBUTING.md).
- Every setting: [`configuration.md`](configuration.md).
