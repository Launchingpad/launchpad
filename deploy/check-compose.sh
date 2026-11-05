#!/usr/bin/env bash
# Static security check of the PRODUCTION compose file. It renders
# deploy/docker-compose.yml with the placeholder deploy/.env.example
# (`docker compose config`; starts nothing) and fails unless:
#
#   - (the offsite worker's networks, role, mounts and env allowlist are
#     checked too, in check-compose.py)
#   - only web (-> 8080) and silo (-> 9000) publish a port, on
#     DEPLOY_WEB_PORT / DEPLOY_FILES_PORT; nothing else publishes anything;
#   - no service mounts the Docker socket or any host path but the two
#     read-only config files, and none is privileged or on the host network;
#   - every service but volume-init has no-new-privileges, and capabilities
#     are only the listed exceptions; silo and backup run as non-root users;
#   - volume-init (the ONE no-new-privileges exception, on Linux
#     7.0 that flag with cap_drop ALL + cap_add CHOWN stops its exec) keeps
#     cap_drop ALL, adds CHOWN and nothing else, has network_mode none and no
#     network, no other security_opt, and restart "no" (one-shot);
#   - every pulled image is pinned to a digest, and every Dockerfile base too;
#   - the app gets LOG_ENV=production, no AUTH_FIXED_OTP, and https URLs on
#     the two host names.
#
# It also checks the ROOT docker-compose.yml (LAUN-246) with the same rules
# where the services are the same, and these differences: the data lives in
# host directories, so a bind mount is allowed ONLY for the three
# LAUNCHPAD_*_DIR directories and the two read-only config files (any other
# host mount, or the Docker socket, fails); images come from
# ghcr.io/launchingpad at the required LAUNCHPAD_VERSION (never latest);
# silo-init is a one-shot with no port. The drift test
# (test-compose-check.sh) compares the shared services of the two files.
#
# The rules live in deploy/check-compose.py; deploy/test-compose-check.sh
# runs them against mutated renders. Both run under `make compose-check` in
# server/ (CI's test job). Needs docker and python3. check-compose.py exits 2
# on a finding.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/.." && pwd)"

# The same clean environment deploy.sh gives docker (lib.sh docker_clean).
# shellcheck source=deploy/lib.sh
source "$here/lib.sh"
json="$(docker_clean -- docker compose -f "$here/docker-compose.yml" --env-file "$here/.env.example" config --format json)"

# The rules are in check-compose.py, so test-compose-check.sh can feed them
# mutated renders without Docker.
printf '%s' "$json" | python3 "$here/check-compose.py" "$here/.env.example"

# Fail closed: with DEPLOY_BIND_ADDR absent, both ports bind to 127.0.0.1.
noaddr="$(mktemp "${TMPDIR:-/tmp}/devops-check-compose.XXXXXX")"
trap 'rm -f "$noaddr"' EXIT
grep -v '^DEPLOY_BIND_ADDR=' "$here/.env.example" >"$noaddr"
docker_clean -- docker compose -f "$here/docker-compose.yml" --env-file "$noaddr" config --format json |
	python3 -c '
import json, sys
cfg = json.load(sys.stdin)
ips = sorted({p.get("host_ip", "") for s in cfg["services"].values() for p in (s.get("ports") or [])})
if ips != ["127.0.0.1"]:
    sys.exit("check-compose (deploy): FAIL - without DEPLOY_BIND_ADDR the ports bind to %s, want 127.0.0.1" % ips)
print("check-compose (deploy): the default bind address is 127.0.0.1")
'

# The root docker-compose.yml, rendered with a FILLED copy of .env.example
# (fake values; example-env.sh). The project directory is the repo root.
rootenv="$(mktemp "${TMPDIR:-/tmp}/devops-check-compose-root.XXXXXX")"
trap 'rm -f "$noaddr" "$noaddr.err" "$rootenv"' EXIT
"$here/example-env.sh" "$rootenv"
docker_clean -- docker compose -f "$repo/docker-compose.yml" --env-file "$rootenv" config --format json |
	python3 "$here/check-compose.py" --root "$rootenv" "$repo"
# Fail closed: with LAUNCHPAD_BIND_ADDR absent, both ports bind to 127.0.0.1.
grep -v '^LAUNCHPAD_BIND_ADDR=' "$rootenv" >"$noaddr"
docker_clean -- docker compose -f "$repo/docker-compose.yml" --env-file "$noaddr" config --format json |
	python3 -c '
import json, sys
cfg = json.load(sys.stdin)
ips = sorted({p.get("host_ip", "") for s in cfg["services"].values() for p in (s.get("ports") or [])})
if ips != ["127.0.0.1"]:
    sys.exit("check-compose (root): FAIL - without LAUNCHPAD_BIND_ADDR the ports bind to %s, want 127.0.0.1" % ips)
print("check-compose (root): the default bind address is 127.0.0.1")
'
# A required value missing is a render error, never a default (no latest).
grep -v '^LAUNCHPAD_VERSION=' "$rootenv" >"$noaddr"
if docker_clean -- docker compose -f "$repo/docker-compose.yml" --env-file "$noaddr" config >/dev/null 2>"$noaddr.err"; then
	echo "check-compose (root): FAIL - the file renders without LAUNCHPAD_VERSION" >&2
	exit 1
fi
grep -q 'LAUNCHPAD_VERSION missing' "$noaddr.err" || {
	echo "check-compose (root): FAIL - the render failed for another reason than the missing version:" >&2
	cat "$noaddr.err" >&2
	exit 1
}
rm -f "$noaddr.err"
echo "check-compose (root): LAUNCHPAD_VERSION is required (no default)"

# Every Dockerfile base is pinned to a digest as well.
unpinned="$(grep -H -E '^FROM ' "$repo/server/Dockerfile" "$repo/web/Dockerfile" | grep -v '@sha256:' || true)"
if [[ -n "$unpinned" ]]; then
	echo "check-compose (deploy): FAIL - Dockerfile bases without a digest:" >&2
	echo "$unpinned" >&2
	exit 1
fi
echo "check-compose (deploy): every Dockerfile FROM is pinned to a digest"
