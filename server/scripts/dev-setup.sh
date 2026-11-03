#!/usr/bin/env bash
# First-run setup of a LOCAL dev stack. It reads the newest setup
# link from the app log, completes POST /api/v1/setup (the first user, the
# org and its first team) and prints the first admin token, once.
#
#   PROJECT=dev-ops-mytask scripts/dev-setup.sh      (in server/)
#
# Settings (environment; defaults in brackets):
#   PROJECT       the compose project of the stack (required in a worktree;
#                 the shared checkout's server/.env sets DEV_OPS_PROJECT)
#   BASE_URL      [http://localhost:${DEV_OPS_APP_PORT:-8090}]
#   SETUP_USERNAME [admin]  SETUP_NAME [Admin]  SETUP_EMAIL [admin@example.com]
#   ORG_SLUG [orbit]  ORG_NAME [Orbit]  TEAM_KEY [ORB]  TEAM_NAME [Orbit]
#
# Local stacks only: the link is read from `docker compose logs app`. The
# token is printed to stdout; it is not written to any file. Only once per
# database: after setup the server prints no link (reset with `down -v`).
#
# Not here yet: agent users (claude) and their tokens, which the importer's
# user map and the MCP server need. They come with the later invite tickets
# (later invite work); until then make the agent token with the API by hand
# (POST /orgs/{org}/tokens needs an agent member, which needs SQL today).
set -euo pipefail

cd "$(dirname "$0")/.."
project="${PROJECT:-${DEV_OPS_PROJECT:-}}"
compose=(docker compose)
[[ -n "$project" ]] && compose+=(-p "$project")
base="${BASE_URL:-http://localhost:${DEV_OPS_APP_PORT:-8090}}"

for c in docker curl python3; do
	command -v "$c" >/dev/null 2>&1 || {
		echo "dev-setup.sh: $c is not installed" >&2
		exit 2
	}
done

status="$(curl -sS --max-time 10 "$base/api/v1/setup")" || {
	echo "dev-setup.sh: $base does not answer; is the stack up (make up PROJECT=...)?" >&2
	exit 1
}
if [[ "$status" != *'"setupRequired":true'* ]]; then
	echo "dev-setup.sh: setup is complete on $base (it has a user); reset the stack with down -v to start again" >&2
	exit 3
fi

# The newest link in the log (each boot closes the older ones).
token="$("${compose[@]}" logs --no-color app 2>/dev/null | python3 -c '
import re, sys
found = re.findall(r"setup: open \S*/setup#([A-Za-z0-9_-]{43})", sys.stdin.read())
print(found[-1] if found else "")
')"
if [[ -z "$token" ]]; then
	echo "dev-setup.sh: no setup link in the app log of project '${project:-<default>}'; restart the app for a new one (${compose[*]} restart app)" >&2
	exit 1
fi

# The body goes to curl on stdin, so the link token is never in an argv.
body="$(SETUP_TOKEN="$token" python3 -c '
import json, os
e = os.environ.get
print(json.dumps({
    "token": e("SETUP_TOKEN"),
    "user": {"username": e("SETUP_USERNAME") or "admin", "name": e("SETUP_NAME") or "Admin",
             "email": e("SETUP_EMAIL") or "admin@example.com"},
    "org": {"slug": e("ORG_SLUG") or "orbit", "name": e("ORG_NAME") or "Orbit"},
    "team": {"key": e("TEAM_KEY") or "ORB", "name": e("TEAM_NAME") or "Orbit"},
}))
')"
resp="$(printf '%s' "$body" | curl -sS --max-time 30 -w '\n%{http_code}' -H 'Content-Type: application/json' \
	--data-binary @- "$base/api/v1/setup")"
code="${resp##*$'\n'}"
resp="${resp%$'\n'*}"
if [[ "$code" != 201 ]]; then
	echo "dev-setup.sh: POST /api/v1/setup -> $code: $resp" >&2
	exit 1
fi
printf '%s' "$resp" | python3 -c '
import json, sys
b = json.load(sys.stdin)
t = b["token"]
print("setup complete: user %s (platform admin), org %s" % (b["user"]["username"], b["orgs"][0]["slug"]))
print("first admin token (shown once; pinned to the org, %s, expires %s):" % (",".join(t["token"]["scopes"]), t["token"]["expiresAt"] or "never"))
print(t["secret"])
'
