#!/usr/bin/env python3
"""Drift test between deploy/docker-compose.yml and the root docker-compose.yml.

Usage: check-compose-drift.py DEPLOY_RENDER.json ROOT_RENDER.json

Both files are `docker compose config --format json` renders. The services
they share (volume-init, postgres, silo, backup, web, app) must keep the same
image pins, security options, capabilities, users, networks, health checks,
logging, restart policy, published ports, mount targets and environment
names. A difference passes only if it is on INTENDED below, with its reason.
Anything else exits 2 and says "check-compose-drift: FAIL".

(LAUN-246, docs/internal/decisions-T45-docker-distribution.md.)
"""
import json
import sys

SHARED = ("volume-init", "postgres", "silo", "backup", "web", "app")

# Env values that differ by design (placeholders, secrets, URLs built from them).
VALUE_VARIES = {
    "POSTGRES_PASSWORD", "PGPASSWORD", "AUTH_OTP_PEPPER", "SETTINGS_ENCRYPTION_KEY", "BACKUP_ENCRYPTION_KEY",
    "MINIO_ROOT_USER", "MINIO_ROOT_PASSWORD", "S3_ACCESS_KEY", "S3_SECRET_KEY", "DATABASE_URL",
    "S3_BUCKET", "TRUSTED_PROXY_CIDRS", "GIT_ALLOW_PRIVATE_PROVIDERS", "GIT_ALLOW_PRIVATE_CIDRS",
    "POSTMARK_SERVER_TOKEN", "POSTMARK_FROM",
}


def names(x):
    return sorted(x.keys()) if isinstance(x, dict) else sorted(x or [])


def field_values(s, name):
    """The comparable view of one service: field -> value."""
    v = {}
    for k in ("image", "user", "security_opt", "cap_drop", "cap_add", "read_only", "tmpfs", "healthcheck",
              "restart", "logging", "network_mode", "command"):
        v[k] = s.get(k)
    if name != "volume-init":
        v["entrypoint"] = s.get("entrypoint")
    v["build"] = "build" in s
    v["networks"] = names(s.get("networks"))
    v["depends_on"] = names(s.get("depends_on"))
    v["depends_on_conditions"] = {k: d.get("condition") for k, d in (s.get("depends_on") or {}).items()}
    v["env_names"] = names(s.get("environment"))
    env = s.get("environment") or {}
    v["env_values"] = {k: env[k] for k in sorted(env) if k not in VALUE_VARIES}
    if name != "volume-init":
        v["mounts"] = sorted((m.get("target"), bool(m.get("read_only"))) for m in s.get("volumes") or [])
    v["ports"] = sorted((p.get("target"), p.get("host_ip"), str(p.get("published"))) for p in s.get("ports") or [])
    return v


def subset(a, b, extra_a=(), extra_b=()):
    return set(a) - set(b) == set(extra_a) and set(b) - set(a) == set(extra_b)


# (service, field) -> (why, check(deploy_value, root_value) -> bool). The check
# accepts exactly the intended difference and nothing wider.
INTENDED = {
    ("app", "image"): ("deploy builds launchpad-api locally; the root file pulls it from GHCR",
                       lambda a, b: a.startswith("launchpad-api:") and b.startswith("ghcr.io/launchingpad/launchpad-api:")),
    ("web", "image"): ("deploy builds launchpad-web locally; the root file pulls it from GHCR",
                       lambda a, b: a.startswith("launchpad-web:") and b.startswith("ghcr.io/launchingpad/launchpad-web:")),
    ("app", "build"): ("deploy builds; the root file has only a commented build line", lambda a, b: a is True and b is False),
    ("web", "build"): ("deploy builds; the root file has only a commented build line", lambda a, b: a is True and b is False),
    ("web", "env_names"): ("DEPLOY_NGINX_CONF_SUM recreates web in deploy.sh; the root file has no such sum",
                           lambda a, b: subset(a, b, extra_a=["DEPLOY_NGINX_CONF_SUM"])),
    ("backup", "env_names"): ("DEPLOY_BACKUP_SH_SUM recreates backup in deploy.sh; the root file has no such sum",
                              lambda a, b: subset(a, b, extra_a=["DEPLOY_BACKUP_SH_SUM"])),
    ("web", "env_values"): ("same reason as web env_names", lambda a, b: subset(a, b, extra_a=["DEPLOY_NGINX_CONF_SUM"]) and all(b[k] == a[k] for k in b)),
    ("backup", "env_values"): ("same reason as backup env_names", lambda a, b: subset(a, b, extra_a=["DEPLOY_BACKUP_SH_SUM"]) and all(b[k] == a[k] for k in b)),
    ("postgres", "networks"): ("no offsite worker in the root file, so no offsite_db network",
                               lambda a, b: a == ["backend", "offsite_db"] and b == ["backend"]),
    ("silo", "networks"): ("no offsite worker in the root file, so no files_net network",
                           lambda a, b: a == ["edge", "files_net"] and b == ["edge"]),
    ("postgres", "depends_on"): ("the root file waits for volume-init (host directories)", lambda a, b: a == [] and b == ["volume-init"]),
    ("app", "depends_on"): ("the root file also waits for silo-init", lambda a, b: subset(a, b, extra_b=["silo-init"])),
    ("postgres", "depends_on_conditions"): ("same as postgres depends_on", lambda a, b: a == {} and b == {"volume-init": "service_completed_successfully"}),
    ("app", "depends_on_conditions"): ("same as app depends_on",
                                       lambda a, b: {k: v for k, v in b.items() if k != "silo-init"} == a and b.get("silo-init") == "service_completed_successfully"),
}


def compare(deploy, root):
    bad, intended = [], []
    for name in SHARED:
        if name not in deploy.get("services", {}):
            bad.append("%s: missing from the deploy render" % name)
            continue
        if name not in root.get("services", {}):
            bad.append("%s: missing from the root render" % name)
            continue
        a = field_values(deploy["services"][name], name)
        b = field_values(root["services"][name], name)
        for f in a:
            if a[f] == b[f]:
                continue
            rule = INTENDED.get((name, f))
            if rule and rule[1](a[f], b[f]):
                intended.append("%s %s: %s" % (name, f, rule[0]))
            else:
                bad.append("%s: %s differs\n    deploy: %s\n    root:   %s" % (name, f, json.dumps(a[f], sort_keys=True), json.dumps(b[f], sort_keys=True)))
    return bad, intended


def main():
    deploy = json.load(open(sys.argv[1]))
    root = json.load(open(sys.argv[2]))
    bad, intended = compare(deploy, root)
    if bad:
        sys.stderr.write("check-compose-drift: FAIL\n  " + "\n  ".join(bad) + "\n")
        sys.exit(2)
    print("check-compose-drift: the shared services match (%d intended differences)" % len(intended))
    if "-v" in sys.argv:
        for i in sorted(intended):
            print("  " + i)


if __name__ == "__main__":
    main()
