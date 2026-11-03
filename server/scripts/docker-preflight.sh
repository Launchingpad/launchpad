#!/usr/bin/env bash
# Fail fast when the Docker daemon is down or wedged (a known
# failure mode): `docker info` with a bound, so a dead daemon costs seconds, not
# the job timeout. `docker compose version` is client-only and proves nothing.
set -uo pipefail
limit="${DOCKER_PREFLIGHT_TIMEOUT:-20}"
docker info --format '{{.ServerVersion}}' >/dev/null 2>&1 &
pid=$!
for _ in $(seq 1 "$limit"); do
  if ! kill -0 "$pid" 2>/dev/null; then
    if wait "$pid"; then
      echo "docker daemon OK"
      exit 0
    fi
    echo "DOCKER DAEMON NOT RUNNING: start Docker Desktop on the runner and retry" >&2
    exit 1
  fi
  sleep 1
done
kill -9 "$pid" 2>/dev/null
echo "DOCKER DAEMON WEDGED: no answer in ${limit}s; restart Docker Desktop on the runner and retry" >&2
exit 1
