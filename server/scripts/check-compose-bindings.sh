#!/usr/bin/env bash
# Fail unless every port docker-compose.yml publishes is bound to 127.0.0.1.
# It reads the RENDERED config (`docker compose config`), so an
# env override or a YAML anchor cannot slip a 0.0.0.0 binding past it.
# Needs docker (compose v2) and python3. Starts nothing.
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
cd "$here"

# A throwaway project name: `config` only renders, it creates nothing.
json="$(docker compose -p dev-ops-compose-check config --format json)"
printf '%s' "$json" | python3 -c '
import json, sys
cfg = json.load(sys.stdin)
bad, seen = [], 0
for name, svc in sorted(cfg.get("services", {}).items()):
    for p in svc.get("ports", []) or []:
        seen += 1
        ip = p.get("host_ip", "")
        if ip != "127.0.0.1":
            where = ip or "all interfaces"
            bad.append("%s: %s->%s is bound to %s" % (name, p.get("published"), p.get("target"), where))
if seen == 0:
    sys.exit("check-compose-bindings: no published ports found; is this the right file?")
if bad:
    sys.exit("check-compose-bindings: ports must be bound to 127.0.0.1:\n  " + "\n  ".join(bad))
print(f"check-compose-bindings: {seen} published port(s), all on 127.0.0.1")
'
