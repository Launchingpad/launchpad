#!/usr/bin/env python3
"""The rules of deploy/check-compose.sh.

Usage: check-compose.py ENV_FILE < render.json

Reads one `docker compose config --format json` render of
deploy/docker-compose.yml on stdin and the env file it was rendered with
(for the expected ports, bind address and host names). Needs no Docker, so
deploy/test-compose-check.sh can feed it mutated renders.

Exit 0 and one summary line on stdout when the render passes. Exit 2 and
"check-compose (deploy): FAIL" plus one indented line per finding on stderr
when it does not.
"""
import json
import re
import sys

# The only service allowed to run without no-new-privileges.
# On Linux 7.0 (7.0.0-38-generic, Docker 29.9.2, AppArmor + default seccomp)
# that flag with cap_drop ALL + cap_add CHOWN makes its exec fail. It stays
# safe only as a one-shot root chown with no network: checked below.
NNP_EXEMPT = ("volume-init",)

# The operator switches of the git SSRF policy (decisions-T17 #31).
GIT_SWITCHES = ("GIT_ALLOW_PRIVATE_PROVIDERS", "GIT_ALLOW_PRIVATE_CIDRS")

# The off-site backup worker (decisions-T16 #10 to #15, #46). The ONLY
# variables it may get. It holds no mail key and no AUTH_OTP_PEPPER. Its S3_*
# (#38) name silo on files_net and a READ-ONLY user made for it alone.
OFFSITE_ENV = {
    "DATABASE_URL", "LOG_ENV", "BACKUP_ENCRYPTION_KEY", "BACKUP_ALLOW_PRIVATE_CIDRS",
    "BACKUP_DUMP_DIR", "OFFSITE_WORK_DIR", "OFFSITE_HEALTH_FILE", "LAUNCHPAD_VERSION",
    "S3_ENDPOINT", "S3_BUCKET", "S3_REGION", "S3_ACCESS_KEY", "S3_SECRET_KEY",
}
# Named on purpose: these get their own finding even though OFFSITE_ENV excludes them.
OFFSITE_FORBIDDEN = ("SETTINGS_ENCRYPTION_KEY", "AUTH_OTP_PEPPER", "S3_PUBLIC_ENDPOINT")
OFFSITE_FORBIDDEN_PREFIX = ("POSTMARK_",)
OFFSITE_NETS = {"offsite_db", "egress", "files_net"}


def check_backup_key(svcs, bad):
    """The app seals the destination secrets with BACKUP_ENCRYPTION_KEY: it must reach it."""
    if not (svcs.get("app", {}).get("environment") or {}).get("BACKUP_ENCRYPTION_KEY"):
        bad.append("app: BACKUP_ENCRYPTION_KEY is not passed to the app service")


def check_offsite(cfg, svcs, bad):
    """The rules of the `offsite` service and of its two networks (decisions-T16 #46)."""
    o = svcs.get("offsite")
    if o is None:
        bad.append("offsite: the service is missing")
        return
    app_s = svcs.get("app", {})
    env = o.get("environment") or {}
    if env.get("LOG_ENV") != "production":
        bad.append("offsite: LOG_ENV is %r, want 'production'" % env.get("LOG_ENV"))
    if not str(env.get("DATABASE_URL", "")).startswith("postgres://offsite:"):
        bad.append("offsite: DATABASE_URL must use the restricted role 'offsite', not the owner role")
    for k in OFFSITE_FORBIDDEN:
        if k in env:
            bad.append("offsite: %s must not be passed" % k)
    for k in sorted(env):
        if k.startswith(OFFSITE_FORBIDDEN_PREFIX):
            bad.append("offsite: %s must not be passed" % k)
    extra = sorted(k for k in env if k not in OFFSITE_ENV and not k.startswith(OFFSITE_FORBIDDEN_PREFIX)
                   and k not in OFFSITE_FORBIDDEN)
    if extra:
        bad.append("offsite: environment %s is not allowed" % extra)
    for k in ("BACKUP_ENCRYPTION_KEY", "BACKUP_ALLOW_PRIVATE_CIDRS", "S3_ENDPOINT", "S3_BUCKET", "S3_ACCESS_KEY", "S3_SECRET_KEY"):
        if k not in env:
            bad.append("offsite: %s is not passed to the offsite service" % k)
    if o.get("command") != ["/bin/server", "offsite"]:
        bad.append("offsite: command must be ['/bin/server', 'offsite'], got %s" % o.get("command"))
    if not o.get("build"):
        bad.append("offsite: must carry build (the same image as app)")
    if o.get("image") != app_s.get("image"):
        bad.append("offsite: image %r must be the same tag as app's %r" % (o.get("image"), app_s.get("image")))
    if set(o.get("cap_drop") or []) != {"ALL"}:
        bad.append("offsite: must cap_drop [ALL], got %s" % o.get("cap_drop"))
    if "/tmp" not in (o.get("tmpfs") or []):
        bad.append("offsite: must have tmpfs /tmp, got %s" % o.get("tmpfs"))
    # silo is read over files_net with a key of offsite's own. Never the
    # app's key, never silo's root key; the endpoint is the compose service.
    app_env = app_s.get("environment") or {}
    if env.get("S3_ENDPOINT") != "http://silo:9000":
        bad.append("offsite: S3_ENDPOINT must be http://silo:9000, got %r" % env.get("S3_ENDPOINT"))
    silo_env = (svcs.get("silo") or {}).get("environment") or {}
    for k, what in (("S3_ACCESS_KEY", "the app's access key"), ("S3_SECRET_KEY", "the app's secret key")):
        if env.get(k) and env.get(k) == app_env.get(k):
            bad.append("offsite: %s must not be %s (offsite has a read-only user of its own)" % (k, what))
    if env.get("S3_ACCESS_KEY") and env.get("S3_ACCESS_KEY") == silo_env.get("MINIO_ROOT_USER"):
        bad.append("offsite: S3_ACCESS_KEY must not be silo's root user")
    if env.get("S3_SECRET_KEY") and env.get("S3_SECRET_KEY") == silo_env.get("MINIO_ROOT_PASSWORD"):
        bad.append("offsite: S3_SECRET_KEY must not be silo's root password")
    if env.get("S3_BUCKET") != app_env.get("S3_BUCKET"):
        bad.append("offsite: S3_BUCKET must be the app's bucket, got %r" % env.get("S3_BUCKET"))
    nets = set((o.get("networks") or {}).keys())
    if nets != OFFSITE_NETS:
        bad.append("offsite: networks must be exactly egress, files_net and offsite_db, got %s" % sorted(nets))
    mounts = {v.get("target"): v for v in o.get("volumes") or []}
    b = mounts.get("/backups")
    if not b or b.get("type") != "volume" or b.get("source") != "backups" or b.get("read_only") is not True:
        bad.append("offsite: /backups must be the named volume backups mounted read-only, got %s" % b)
    w = mounts.get("/offsite-work")
    if not w or w.get("type") != "volume" or w.get("source") != "offsite_work" or w.get("read_only"):
        bad.append("offsite: /offsite-work must be the writable named volume offsite_work, got %s" % w)
    if set(mounts) - {"/backups", "/offsite-work"}:
        bad.append("offsite: mounts %s are not allowed" % sorted(set(mounts) - {"/backups", "/offsite-work"}))
    if env.get("OFFSITE_WORK_DIR") != "/offsite-work":
        # Not /work: the image has /work/import, and Docker copies that directory's
        # owner (root) onto a new volume mounted there (measured), undoing the chown.
        bad.append("offsite: OFFSITE_WORK_DIR must be /offsite-work, got %r" % env.get("OFFSITE_WORK_DIR"))
    if env.get("BACKUP_DUMP_DIR") != "/backups":
        bad.append("offsite: BACKUP_DUMP_DIR must be /backups, got %r" % env.get("BACKUP_DUMP_DIR"))
    dep = (o.get("depends_on") or {}).get("volume-init")
    if not dep or dep.get("condition") != "service_completed_successfully":
        bad.append("offsite: must depend on volume-init (service_completed_successfully)")
    hc = (o.get("healthcheck") or {}).get("test")
    if hc != ["CMD", "/bin/server", "offsite", "healthcheck"]:
        bad.append("offsite: healthcheck must be ['CMD', '/bin/server', 'offsite', 'healthcheck'], got %s" % hc)
    # The networks: offsite_db is internal and holds only postgres and offsite; egress
    # has a route out and holds only offsite.
    nw = cfg.get("networks") or {}
    if (nw.get("offsite_db") or {}).get("internal") is not True:
        bad.append("network offsite_db: must be internal")
    if (nw.get("egress") or {}).get("internal"):
        bad.append("network egress: must not be internal (it is offsite's route out)")
    if (nw.get("files_net") or {}).get("internal") is not True:
        bad.append("network files_net: must be internal")
    for net, want in (("offsite_db", {"postgres", "offsite"}), ("egress", {"offsite"}), ("files_net", {"silo", "offsite"})):
        on = {n for n, s in svcs.items() if net in (s.get("networks") or {})}
        if on != want:
            bad.append("network %s: services on it must be exactly %s, got %s" % (net, sorted(want), sorted(on)))
    silo_nets = set((svcs.get("silo", {}).get("networks") or {}).keys())
    if silo_nets != {"edge", "files_net"}:
        bad.append("silo: networks must be exactly edge and files_net, got %s" % sorted(silo_nets))
    pg_nets = set((svcs.get("postgres", {}).get("networks") or {}).keys())
    if pg_nets != {"backend", "offsite_db"}:
        bad.append("postgres: networks must be exactly backend and offsite_db, got %s" % sorted(pg_nets))
    # volume-init chowns the scratch volume to uid 70.
    vi = svcs.get("volume-init", {})
    vm = {v.get("target"): v for v in vi.get("volumes") or []}
    ow = vm.get("/offsite-work")
    if not ow or ow.get("source") != "offsite_work" or ow.get("read_only"):
        bad.append("volume-init: must mount offsite_work writable at /offsite-work, got %s" % ow)
    ep = " ".join(vi.get("entrypoint") or [])
    if "chown 70:70 /backups /offsite-work" not in ep:
        bad.append("volume-init: must chown 70:70 /backups /offsite-work, got %s" % vi.get("entrypoint"))
    if (cfg.get("volumes") or {}).get("offsite_work", {}).get("labels", {}).get("dev.orbit.deploy") != "production":
        bad.append("volume offsite_work: must carry the label dev.orbit.deploy=production")


def main():
    cfg = json.load(sys.stdin)
    env = {}
    with open(sys.argv[1]) as f:
        for line in f:
            m = re.match(r"^([A-Z0-9_]+)='(.*)'$", line.strip())
            if m:
                env[m.group(1)] = m.group(2)
    svcs = cfg.get("services", {})
    bad = []
    want_ports = {"web": (8080, env["DEPLOY_WEB_PORT"]), "silo": (9000, env["DEPLOY_FILES_PORT"])}
    allowed_binds = {"web": {"/etc/nginx/templates/default.conf.template"}, "backup": {"/backup/backup.sh"}}
    allowed_caps = {"web": {"CHOWN", "SETUID", "SETGID"}, "volume-init": {"CHOWN"}}
    nonroot = {"silo": "10001:10001", "backup": "70:70", "offsite": "70:70"}
    missing = sorted(set(NNP_EXEMPT) - set(svcs))
    if missing:
        bad.append("no-new-privileges exception names a missing service: %s" % missing)
    for name, s in sorted(svcs.items()):
        ports = s.get("ports") or []
        if name in want_ports:
            tgt, pub = want_ports[name]
            if len(ports) != 1 or ports[0].get("target") != tgt or str(ports[0].get("published")) != pub:
                bad.append("%s: must publish exactly %s -> %d, got %s" % (name, pub, tgt, ports))
            elif ports[0].get("host_ip") != env["DEPLOY_BIND_ADDR"]:
                bad.append("%s: published on %r, want DEPLOY_BIND_ADDR %r" % (name, ports[0].get("host_ip"), env["DEPLOY_BIND_ADDR"]))
        elif ports:
            bad.append("%s: publishes %s; only web and silo may publish a port" % (name, ports))
        if s.get("privileged"):
            bad.append("%s: privileged" % name)
        for k in ("network_mode", "pid", "ipc"):
            if s.get(k) == "host":
                bad.append("%s: %s host" % (name, k))
        for v in s.get("volumes") or []:
            src = v.get("source", "")
            if "docker.sock" in src:
                bad.append("%s: mounts the Docker socket" % name)
            if v.get("type") == "bind":
                if v.get("target") not in allowed_binds.get(name, set()):
                    bad.append("%s: bind mount %s -> %s is not allowed" % (name, src, v.get("target")))
                elif not v.get("read_only"):
                    bad.append("%s: bind mount %s must be read-only" % (name, v.get("target")))
        if name in NNP_EXEMPT:
            # the exception holds only while the service stays a
            # one-shot with no network and no capability but CHOWN.
            if set(s.get("cap_drop") or []) != {"ALL"}:
                bad.append("%s: must cap_drop [ALL], got %s" % (name, s.get("cap_drop")))
            if set(s.get("cap_add") or []) != {"CHOWN"}:
                bad.append("%s: cap_add must be exactly [CHOWN], got %s" % (name, s.get("cap_add")))
            if s.get("network_mode") != "none" or s.get("networks"):
                bad.append("%s: must have network_mode none and no networks, got network_mode=%r networks=%r" % (
                    name, s.get("network_mode"), s.get("networks")))
            if set(s.get("security_opt") or []) - {"no-new-privileges:true"}:
                bad.append("%s: security_opt %s not allowed" % (name, s.get("security_opt")))
            if s.get("restart") != "no":
                bad.append("%s: must be one-shot (restart \"no\"), got %r" % (name, s.get("restart")))
        elif "no-new-privileges:true" not in (s.get("security_opt") or []):
            bad.append("%s: no no-new-privileges" % name)
        extra = set(s.get("cap_add") or []) - allowed_caps.get(name, set())
        if extra:
            bad.append("%s: cap_add %s not allowed" % (name, sorted(extra)))
        if name in nonroot and s.get("user") != nonroot[name]:
            bad.append("%s: must run as %s" % (name, nonroot[name]))
        if "build" not in s and "@sha256:" not in s.get("image", ""):
            bad.append("%s: image %s is not pinned to a digest" % (name, s.get("image")))
    # (decisions-T17 #39): the app writes its import exports to a
    # named volume that volume-init made writable for uid 10001. The root
    # file system is read-only and every capability is dropped, so without
    # this chown the app could not write there.
    app_s = svcs.get("app", {})
    vi = svcs.get("volume-init", {})
    mounts = {v.get("target"): v for v in app_s.get("volumes") or []}
    wd = (app_s.get("environment") or {}).get("IMPORT_WORK_DIR")
    if wd != "/work/import":
        bad.append("app: IMPORT_WORK_DIR must be /work/import, got %r" % wd)
    m = mounts.get("/work/import")
    if not m or m.get("type") != "volume" or m.get("source") != "import_work" or m.get("read_only"):
        bad.append("app: /work/import must be the writable named volume import_work, got %s" % m)
    if app_s.get("user") not in (None, "10001:10001", "10001"):
        bad.append("app: must run as uid 10001 (the owner volume-init sets), got %r" % app_s.get("user"))
    vi_m = {v.get("target"): v for v in vi.get("volumes") or []}
    iw = vi_m.get("/import-work")
    if not iw or iw.get("source") != "import_work" or iw.get("read_only"):
        bad.append("volume-init: must mount import_work writable at /import-work, got %s" % iw)
    if "chown 10001:10001" not in " ".join(vi.get("entrypoint") or []) or "/import-work" not in " ".join(vi.get("entrypoint") or []):
        bad.append("volume-init: must chown 10001:10001 /import-work, got %s" % vi.get("entrypoint"))
    if "volume-init" not in (app_s.get("depends_on") or {}):
        bad.append("app: must depend on volume-init (the import volume is chowned there)")
    if (app_s.get("depends_on") or {}).get("volume-init", {}).get("condition") != "service_completed_successfully":
        bad.append("app: volume-init must be service_completed_successfully")
    app = svcs.get("app", {}).get("environment", {})
    checks = {
        "LOG_ENV": "production",
        "PUBLIC_BASE_URL": "https://" + env["WEB_HOST"],
        "WEB_ORIGINS": "https://" + env["WEB_HOST"],
        "S3_PUBLIC_ENDPOINT": "https://" + env["FILES_HOST"],
    }
    for k, want in checks.items():
        if app.get(k) != want:
            bad.append("app: %s is %r, want %r" % (k, app.get(k), want))
    # the two private-range switches must reach the app (an empty value
    # is the default; a missing key means the operator's setting is dropped).
    for k in GIT_SWITCHES:
        if k not in app:
            bad.append("app: %s is not passed to the app service" % k)
    for k in ("AUTH_FIXED_OTP",):
        if k in app:
            bad.append("app: %s must not be passed in production" % k)
    for name in ("app", "web", "backup", "offsite"):
        if svcs.get(name, {}).get("read_only") is not True:
            bad.append("%s: root file system must be read-only" % name)
    check_offsite(cfg, svcs, bad)
    check_backup_key(svcs, bad)
    if bad:
        sys.stderr.write("check-compose (deploy): FAIL\n  " + "\n  ".join(bad) + "\n")
        sys.exit(2)
    print("check-compose (deploy): %d services; only web:%s and silo:%s published; no socket mounts; images pinned; LOG_ENV=production; offsite on offsite_db, egress and files_net only, backups read-only, silo read with its own key" % (
        len(svcs), env["DEPLOY_WEB_PORT"], env["DEPLOY_FILES_PORT"]))


if __name__ == "__main__":
    main()
