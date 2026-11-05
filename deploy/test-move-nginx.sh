#!/usr/bin/env bash
# A multi-GB upload and download through the REAL production nginx template
# (LAUN-251). The stack, the root docker-compose.yml and deploy/docker-compose.yml,
# mount the same file, deploy/nginx/default.conf.template, so one test covers both.
#
#   deploy/test-move-nginx.sh            (make deploy-test in server/)
#
# Needs only docker. It starts two containers on a scratch network (nginx with
# the template, and a Go stub named "app" that counts request bodies and streams
# zeros), publishes ONE port on 127.0.0.1, and removes everything on exit.
#
# Proves:
#   1. a body of MOVE_TEST_GIB GiB (default 3) with a Content-Length passes
#      POST /api/v1/setup/restore and the stub counts every byte;
#   2. the same with chunked transfer encoding (no Content-Length);
#   3. nginx spooled nothing to disk (its temp folders are a 32 MiB tmpfs, so a
#      spooled body fails the upload; the loaded config is also checked with nginx -T);
#   4. GET /api/v1/platform/move/export/<ticket> streams MOVE_TEST_GIB GiB back;
#   5. the limit is on that ONE path: 3 MiB to /api/v1/setup and to
#      /api/v1/platform/move/export is 413 and the rest of /api/ stays at 2 MiB.
# MOVE_TEST_TEMPLATE points at another template file: a negative control (a
# copy without the streaming directives must FAIL this test).
# MOVE_TEST_SKIP_CONFIG=1 skips the nginx -T check, to prove the tmpfs check alone fails.
# Settings: MOVE_TEST_GIB, MOVE_TEST_PORT (default 14081), NGINX_IMAGE.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/.." && pwd)"
gib="${MOVE_TEST_GIB:-3}"
port="${MOVE_TEST_PORT:-14081}"
# The web image's base, pinned there; this test uses the same one.
nginx_image="${NGINX_IMAGE:-$(sed -n 's/^FROM \(nginx:[^ ]*\).*/\1/p' "$repo/web/Dockerfile")}"
go_image=golang:1.27.3-alpine
scratch="$(mktemp -d "${TMPDIR:-/tmp}/launchpad-test-move-nginx.XXXXXX")"
id="lp-move-nginx-$$"
fail() {
	echo "test-move-nginx: FAIL: $*" >&2
	exit 1
}
ok() { echo "ok: $*"; }
cleanup() {
	docker rm -f "$id-web" "$id-app" >/dev/null 2>&1 || true
	docker network rm "$id" >/dev/null 2>&1 || true
	rm -rf "$scratch"
}
trap cleanup EXIT

[[ -n "$nginx_image" ]] || fail "no nginx image found in web/Dockerfile"
docker image inspect "$nginx_image" >/dev/null 2>&1 || docker pull -q "$nginx_image" >/dev/null || fail "cannot get $nginx_image (set NGINX_IMAGE)"

# The pieces the web image adds: security.inc (the real one is a template of
# the web image; this test needs only that the include resolves).
mkdir -p "$scratch/templates"
cp "${MOVE_TEST_TEMPLATE:-$repo/deploy/nginx/default.conf.template}" "$scratch/templates/default.conf.template"
echo 'add_header X-Content-Type-Options "nosniff" always;' >"$scratch/templates/security.inc.template"

docker network create "$id" >/dev/null
docker run -d --name "$id-app" --network "$id" --network-alias app \
	-e EXPORT_GIB="$gib" -v "$here/test-move-nginx":/src:ro -w /src "$go_image" \
	sh -c 'cp /src/stub.go /tmp/stub.go && cd /tmp && go run stub.go' >/dev/null
# The temp folders are a 32 MiB tmpfs (the production stack mounts a tmpfs there too):
# a body that nginx spools to disk cannot fit and the upload fails. nginx unlinks its
# temp files at once, so looking for files would prove nothing; this does.
docker run -d --name "$id-web" --network "$id" -p "127.0.0.1:$port:8080" \
	--tmpfs /var/cache/nginx:size=32m --tmpfs /tmp:size=32m \
	-e WEB_HOST=lp.example.com -e FILES_ORIGIN= -e API_ORIGIN= \
	-v "$scratch/templates":/etc/nginx/templates:ro "$nginx_image" >/dev/null

base="http://127.0.0.1:$port"
H=(-H "Host: lp.example.com")
up=0
for _ in $(seq 1 90); do
	if curl -fsS -o /dev/null "${H[@]}" "$base/nginx-health" 2>/dev/null &&
		[[ "$(curl -s -o /dev/null -w '%{http_code}' "${H[@]}" -X POST --data x "$base/api/v1/setup/restore")" == 201 ]]; then
		up=1
		break
	fi
	sleep 1
done
[[ "$up" == 1 ]] || {
	docker logs "$id-web" 2>&1 | head -20 >&2
	docker logs "$id-app" 2>&1 | head -20 >&2
	fail "the stack did not come up"
}

# The effective config (what nginx loaded, not the file): the three directives
# that make the streaming work must be in the right locations.
docker exec "$id-web" nginx -T >"$scratch/effective.conf" 2>&1 || fail "nginx -T failed"
loc() { # loc HEADER : the text of the location block that starts with HEADER
	awk -v h="$1" 'index($0, h) && !on { on = 1 } on { print } on && /^  }/ { exit }' "$scratch/effective.conf"
}
r="$(loc 'location = /api/v1/setup/restore')"
[[ -z "${MOVE_TEST_SKIP_CONFIG:-}" ]] && for d in 'proxy_request_buffering off;' 'client_max_body_size 0;' 'proxy_buffering off;'; do
	[[ "$r" == *"$d"* ]] || fail "the restore location lacks '$d' in the loaded config"
done
e="$(loc 'location /api/v1/platform/move/export')"
[[ -z "${MOVE_TEST_SKIP_CONFIG:-}" ]] && for d in 'proxy_buffering off;' 'access_log off;'; do
	[[ "$e" == *"$d"* ]] || fail "the export location lacks '$d' in the loaded config"
done
ok "the loaded config has the streaming directives on the restore and export locations"

want=$((gib * 1024 * 1024 * 1024))
# 1. Content-Length upload (a sparse file: no disk used).
truncate -s "${gib}G" "$scratch/big"
out="$(curl -sS "${H[@]}" -X POST -T "$scratch/big" -H "Content-Type: application/octet-stream" -w '\n%{http_code}' "$base/api/v1/setup/restore")" || fail "upload (Content-Length) failed: curl exit $?"
code="${out##*$'\n'}"
[[ "$code" == 201 ]] || fail "upload (Content-Length): status $code, want 201: ${out%$'\n'*}"
[[ "$out" == *"\"bytes\":$want"* ]] || fail "upload (Content-Length): the app counted a different size: ${out%$'\n'*}"
ok "a $gib GiB upload with Content-Length passed nginx (app counted $want bytes)"
rm -f "$scratch/big"

# 2. Chunked (no Content-Length): the form a streaming client may send.
out="$(head -c "$want" /dev/zero | curl -sS "${H[@]}" -X POST -T - -H "Transfer-Encoding: chunked" -H "Content-Type: application/octet-stream" -w '\n%{http_code}' "$base/api/v1/setup/restore")" || fail "upload (chunked) failed: curl exit $?"
code="${out##*$'\n'}"
[[ "$code" == 201 && "$out" == *"\"bytes\":$want"* ]] || fail "upload (chunked): status $code: ${out%$'\n'*}"
ok "a $gib GiB chunked upload passed nginx"

# 3. Not a separate assertion: checks 1 and 2 above prove it. The temp folders are a
# 32 MiB tmpfs, so a spooled body would have failed those uploads.
ok "no spooling (shown by checks 1 and 2: the $gib GiB bodies passed with 32 MiB temp folders)"

# 4. Download.
got="$(curl -sS "${H[@]}" -o /dev/null -w '%{http_code} %{size_download}' "$base/api/v1/platform/move/export/ticket")" || fail "download failed: curl exit $?"
[[ "$got" == "200 $want" ]] || fail "download: got '$got', want '200 $want'"
ok "a $gib GiB export download streamed through nginx, unbuffered"

# 5. The limit is only on the restore path.
head -c 3145728 /dev/zero >"$scratch/3m"
for p in /api/v1/setup /api/v1/issues /api/v1/platform/move/export/ticket; do
	c="$(curl -s -o /dev/null -w '%{http_code}' "${H[@]}" -X POST -T "$scratch/3m" "$base$p")"
	[[ "$c" == 413 ]] || fail "3 MiB to $p: status $c, want 413"
done
ok "3 MiB to every other API path is still 413"
# 6. The one-time ticket is in the export path: neither the access log nor the
# error log (the 413 above writes an error line) may hold it.
if docker logs "$id-web" 2>&1 | grep -q 'move/export'; then fail "the nginx logs hold the export path (the ticket)"; fi
ok "the nginx logs, access and error, do not hold the export ticket"
echo "test-move-nginx: all passed"
