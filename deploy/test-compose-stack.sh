#!/usr/bin/env bash
# A real bring-up of the ROOT docker-compose.yml (LAUN-246), on a scratch
# project, with host-directory data. CI runs it (job `compose-stack`); you can
# run it on a Mac or Linux box with Docker.
#
#   LAUNCHPAD_VERSION=0.0.0-ci deploy/test-compose-stack.sh
#
# The two images must exist locally under the names the compose file expects:
#   docker build -t ghcr.io/launchingpad/launchpad-api:$LAUNCHPAD_VERSION server
#   docker build -t ghcr.io/launchingpad/launchpad-web:$LAUNCHPAD_VERSION web
#
# Settings (all optional): COMPOSE_TEST_PROJECT (default launchpad-compose-test;
# never `launchpad`), COMPOSE_TEST_WEB_PORT (14001), COMPOSE_TEST_FILES_PORT
# (14002), COMPOSE_TEST_DIR (a scratch folder; default a new one under TMPDIR).
#
# What it proves, in order:
#   1. `docker compose config` accepts the filled example env.
#   2. gen-env.sh makes a .env; `up -d --wait` brings every service up; the
#      silo-init one-shot made the bucket and the app's user (no manual step).
#   3. First-run setup through the log link; the repo's smoke test; an
#      attachment round trip through the web and files ports (upload as the
#      app's silo user, download, same bytes).
#   4. A second `up -d` changes nothing; `down` then `up -d` keeps the users,
#      the issue and the attachment; a backup lands in the host backup folder.
#   5. `down -v` removes the stack and the host data folder is deleted.
# Secrets are never printed. Ports bind to 127.0.0.1 only.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/.." && pwd)"
project="${COMPOSE_TEST_PROJECT:-launchpad-compose-test}"
[[ "$project" != launchpad ]] || {
	echo "test-compose-stack: the project name must not be 'launchpad'" >&2
	exit 2
}
version="${LAUNCHPAD_VERSION:?set LAUNCHPAD_VERSION (the tag of the local images)}"
wport="${COMPOSE_TEST_WEB_PORT:-14001}"
fport="${COMPOSE_TEST_FILES_PORT:-14002}"
work="${COMPOSE_TEST_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/launchpad-compose-test.XXXXXX")}"
mkdir -p "$work"
work="$(cd "$work" && pwd)"
web=lp.example.com
files=lpf.example.com
fail() {
	echo "test-compose-stack: FAIL: $*" >&2
	exit 1
}
say() { echo "== $*"; }

dc() { docker compose -p "$project" -f "$repo/docker-compose.yml" --env-file "$work/.env" "$@"; }

cleanup() {
	local rc=$?
	if [[ -f "$work/.env" ]]; then
		dc down -v --remove-orphans --timeout 20 >/dev/null 2>&1 || true
	fi
	# The data folders belong to uid 70 and 10001: remove them with a container.
	if [[ -d "$work/data" ]]; then
		docker run --rm -v "$work/data":/d --entrypoint /bin/sh \
			postgres:16.15-alpine@sha256:f4610795587492cd4fb887c2dc3de301b813278bb6f616992061141b5f15ac36 \
			-c 'rm -rf /d/* /d/.[!.]* 2>/dev/null; true' >/dev/null 2>&1 || true
	fi
	rm -rf "$work/data" "$work/.env" "$work/curlrc" "$work/body" "$work/hdr" "$work/upload.txt" 2>/dev/null || true
	rmdir "$work" 2>/dev/null || true
	return $rc
}
trap cleanup EXIT

say "1. config with the filled example env"
"$here/example-env.sh" "$work/example.env"
docker compose -f "$repo/docker-compose.yml" --env-file "$work/example.env" config --quiet || fail "config rejected the example env"
rm -f "$work/example.env"

say "2. gen-env.sh, up -d --wait"
(cd "$work" && WEB_HOST="$web" FILES_HOST="$files" LAUNCHPAD_VERSION="$version" "$repo/scripts/gen-env.sh" --non-interactive --example "$repo/.env.example" >/dev/null)
sed -i.bak "s#^LAUNCHPAD_DATA_DIR=.*#LAUNCHPAD_DATA_DIR='$work/data'#;s#^LAUNCHPAD_WEB_PORT=.*#LAUNCHPAD_WEB_PORT='$wport'#;s#^LAUNCHPAD_FILES_PORT=.*#LAUNCHPAD_FILES_PORT='$fport'#" "$work/.env"
rm -f "$work/.env.bak"
chmod 600 "$work/.env"
dc up -d --wait --wait-timeout 240 >/dev/null 2>"$work/up.err" || {
	cat "$work/up.err" >&2
	dc logs --no-color --tail 40 >&2 || true
	fail "up -d --wait failed"
}
rm -f "$work/up.err"
dc logs --no-color silo-init 2>/dev/null | grep -q 'silo-init: bucket ' || fail "silo-init did not report the bucket"
for s in postgres silo app web backup; do
	[[ -n "$(dc ps -q --status running "$s")" ]] || fail "$s is not running"
done
[[ "$(dc ps -a -q --status exited | wc -l | tr -d ' ')" == 2 ]] || fail "expected the two one-shots (volume-init, silo-init) to have exited"
[[ "$(docker inspect -f '{{.State.ExitCode}}' "$(dc ps -a -q silo-init)")" == 0 ]] || fail "silo-init did not exit 0"
echo "ok: all services up; the one-shots exited 0"
# Bind mounts: the host folders exist and have the owners volume-init sets.
owner() { stat -c %u "$1" 2>/dev/null || stat -f %u "$1"; }
for d in postgres:70 backups:70 files:10001; do
	[[ -d "$work/data/${d%%:*}" ]] || fail "$work/data/${d%%:*} was not made"
done
if [[ "$(uname -s)" == Linux ]]; then
	for d in postgres:70 backups:70 files:10001; do
		[[ "$(owner "$work/data/${d%%:*}")" == "${d##*:}" ]] || fail "owner of data/${d%%:*} is not ${d##*:}"
	done
	echo "ok: host folders owned by uid 70 / 10001"
fi

# curl reaches the real host names through the published ports (no DNS, no root).
cat >"$work/curlrc" <<RC
connect-to = $web:80:127.0.0.1:$wport
connect-to = $files:80:127.0.0.1:$fport
RC
cu() { CURL_HOME="$work" curl -q -K "$work/curlrc" -sS --max-time 30 "$@"; }
st="$(cu -o "$work/body" -w '%{http_code}' "http://$web/healthz")"
[[ "$st" == 200 ]] || fail "GET /healthz -> $st"
echo "ok: GET /healthz -> 200 through the web port"
st="$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$wport/" || true)"
[[ "$st" == 421 ]] || fail "another Host -> $st (want 421)"

say "3. first-run setup, smoke, attachment round trip"
token="$(dc logs --no-color app | python3 -c '
import re, sys
found = re.findall(r"setup: open \S*/setup#([A-Za-z0-9_-]{43})", sys.stdin.read())
print(found[-1] if found else "")
')"
[[ -n "$token" ]] || fail "no setup link in the app log"
body="$(SETUP_TOKEN="$token" python3 -c '
import json, os
print(json.dumps({"token": os.environ["SETUP_TOKEN"],
  "user": {"username": "admin", "name": "Admin", "email": "admin@example.com"},
  "org": {"slug": "orbit", "name": "Orbit"}, "team": {"key": "ORB", "name": "Orbit"}}))')"
unset token
resp="$(printf '%s' "$body" | cu -w '\n%{http_code}' -H 'Content-Type: application/json' --data-binary @- "http://$web/api/v1/setup")"
[[ "${resp##*$'\n'}" == 201 ]] || fail "POST /setup -> ${resp##*$'\n'}"
admin="$(printf '%s' "${resp%$'\n'*}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"]["secret"])')"
unset resp body
echo "ok: setup done (the token stays in memory)"

# api METHOD PATH [JSON]: the admin token goes to curl on stdin, never argv.
api() {
	local m="$1" p="$2" data="${3:-}" args=()
	[[ -n "$data" ]] && args=(-H 'Content-Type: application/json' --data "$data")
	printf 'header = "Authorization: Bearer %s"\n' "$admin" |
		cu -K - -o "$work/body" -D "$work/hdr" -w '%{http_code}' -X "$m" ${args[@]+"${args[@]}"} "http://$web$p"
}
jf() { python3 -c "import json,sys; b=json.load(open(sys.argv[1])); print($1)" "$work/body"; }
hdr() { { grep -i "^$1:" "$work/hdr" || true; } | sed -n 1p | cut -d' ' -f2- | tr -d '\r'; }

# The repo's smoke test, through the same host-name mapping (curl reads
# $CURL_HOME/.curlrc). The token goes in the environment of that one process.
cp "$work/curlrc" "$work/.curlrc"
BASE_URL="http://$web" TOKEN="$admin" ORG=orbit TEAM=ORB SMOKE_TMPDIR="$work" CURL_HOME="$work" \
	"$repo/server/scripts/smoke.sh" >"$work/smoke.out" 2>&1 || {
	rm -f "$work/.curlrc"
	cat "$work/smoke.out" >&2
	fail "server/scripts/smoke.sh failed"
}
rm -f "$work/.curlrc" "$work/smoke.out"
echo "ok: server/scripts/smoke.sh passed"

st="$(api POST /api/v1/orgs/orbit/issues '{"team":"ORB","title":"compose stack test"}')"
[[ "$st" == 201 ]] || fail "create issue -> $st"
issue="$(jf 'b["identifier"]')"
payload="compose stack test $(date -u +%FT%TZ) $RANDOM"
printf '%s' "$payload" >"$work/upload.txt"
st="$(api POST /api/v1/orgs/orbit/attachments/prepare "{\"filename\":\"t.txt\",\"contentType\":\"text/plain\",\"sizeBytes\":${#payload}}")"
[[ "$st" == 200 || "$st" == 201 ]] || fail "prepare -> $st"
put_url="$(jf 'b["uploadUrl"]')"
key="$(jf 'b["uploadKey"]')"
[[ "$put_url" == "https://$files/"* ]] || fail "uploadUrl is not on https://$files/"
st="$(cu -o /dev/null -w '%{http_code}' -X PUT -H 'Content-Type: text/plain' --data-binary "@$work/upload.txt" "http://${put_url#https://}")"
[[ "$st" == 200 ]] || fail "presigned PUT -> $st (silo-init did not make the bucket or the user?)"
st="$(api POST "/api/v1/orgs/orbit/issues/$issue/attachments" "{\"kind\":\"file\",\"uploadKey\":\"$key\"}")"
[[ "$st" == 201 ]] || fail "attach -> $st"
dl="$(jf 'b["downloadUrl"]')"
echo "ok: upload as the app's silo user, attached to $issue"

download_matches() {
	local s loc
	s="$(api GET "${dl#https://"$web"}")"
	[[ "$s" == 302 ]] || return 1
	loc="$(hdr location)"
	[[ "$loc" == "https://$files/"* ]] || return 1
	s="$(cu -o "$work/body" -w '%{http_code}' "http://${loc#https://}")"
	[[ "$s" == 200 ]] && cmp -s "$work/body" "$work/upload.txt"
}
download_matches || fail "download did not return the uploaded bytes"
echo "ok: download -> 302 -> presigned GET, same bytes"

say "4. up -d again; down and up -d keep the data; backup lands on the host"
before="$(dc ps -q | sort | tr '\n' ' ')"
dc up -d --wait --wait-timeout 240 >/dev/null 2>&1 || fail "second up -d failed"
for s in postgres silo app web backup; do
	cid="$(dc ps -q --status running "$s")"
	[[ -n "$cid" && " $before" == *"$cid"* ]] || fail "the second up -d replaced $s"
done
echo "ok: the second up -d kept the running containers"

dc down >/dev/null 2>&1 || fail "down failed"
[[ -z "$(dc ps -q)" ]] || fail "containers remain after down"
[[ -d "$work/data/postgres" && -n "$(ls "$work/data/postgres" 2>/dev/null)" ]] || fail "the host database folder is empty after down"
dc up -d --wait --wait-timeout 240 >/dev/null 2>&1 || fail "up -d after down failed"
st="$(cu -o "$work/body" -w '%{http_code}' "http://$web/api/v1/setup")"
grep -q '"setupRequired":false' "$work/body" || fail "setup is open again after down/up (the database was lost)"
st="$(api GET /api/v1/me)"
[[ "$st" == 200 ]] || fail "the token no longer works after down/up -> $st"
st="$(api GET "/api/v1/orgs/orbit/issues/$issue")"
[[ "$st" == 200 ]] || fail "the issue is gone after down/up -> $st"
download_matches || fail "the attachment is gone after down/up"
echo "ok: after down and up -d: setup closed, token, issue and attachment kept"

dc exec -T backup sh /backup/backup.sh once >/dev/null 2>&1 || fail "backup.sh once failed"
ls "$work"/data/backups/devops-*.dump >/dev/null 2>&1 || fail "no dump in the host backup folder"
echo "ok: a dump is in the host backup folder"

# The restore recipe of docs/self-hosting.md: replace the database with the dump.
dump="$(dc exec -T backup sh -c 'ls -t /backups/devops-*.dump | head -n 1' | tr -d '\r')"
[[ -n "$dump" ]] || fail "no dump name"
dc stop app web backup >/dev/null 2>&1 || fail "stop failed"
dc exec -T postgres dropdb -U devops devops || fail "dropdb failed"
dc exec -T postgres createdb -U devops devops || fail "createdb failed"
dc start backup >/dev/null 2>&1 || fail "start backup failed"
dc exec -T backup pg_restore --no-owner -d devops "$dump" || fail "pg_restore failed"
dc up -d --wait --wait-timeout 240 >/dev/null 2>&1 || fail "up -d after the restore failed"
st="$(api GET "/api/v1/orgs/orbit/issues/$issue")"
[[ "$st" == 200 ]] || fail "the issue is gone after the restore -> $st"
echo "ok: the documented restore brought the data back"

say "5. down -v"
dc down -v --remove-orphans --timeout 20 >/dev/null 2>&1 || fail "down -v failed"
[[ -z "$(docker ps -a -q --filter "label=com.docker.compose.project=$project")" ]] || fail "containers remain after down -v"
[[ -z "$(docker volume ls -q --filter "label=com.docker.compose.project=$project")" ]] || fail "volumes remain after down -v"
[[ -z "$(docker network ls -q --filter "label=com.docker.compose.project=$project")" ]] || fail "networks remain after down -v"
echo "test-compose-stack: PASS"
