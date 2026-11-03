#!/usr/bin/env bash
# Self-test of the production compose rules, deploy/check-compose.py.
# It renders deploy/docker-compose.yml once with the placeholder
# deploy/.env.example (the real-render baseline, which must pass), then
# feeds the checker mutated copies of that JSON. Each mutation must exit 2
# and print its exact FAIL line; each allowed variant must exit 0. The
# mutations need no Docker; only the one render does.
#
# Run by `make compose-check` in server/ (CI's test job).
#
#   COMPOSE_JSON=<file>  use this render instead of calling docker
#   CHECKER=<file>       test another copy of check-compose.py (to show
#                        that a weakened checker makes this test fail)
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
checker="${CHECKER:-$here/check-compose.py}"
envfile="$here/.env.example"

scratch="$(mktemp -d "${TMPDIR:-/tmp}/devops-test-compose-check.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT

if [[ -n "${COMPOSE_JSON:-}" ]]; then
	cp "$COMPOSE_JSON" "$scratch/base.json"
else
	# shellcheck source=deploy/lib.sh
	source "$here/lib.sh"
	docker_clean -- docker compose -f "$here/docker-compose.yml" --env-file "$envfile" \
		config --format json >"$scratch/base.json"
fi

python3 - "$checker" "$envfile" "$scratch/base.json" <<'EOF'
import copy, json, subprocess, sys

checker, envfile, base_path = sys.argv[1:4]
base = json.load(open(base_path))
VI = "volume-init"


def svc(cfg, name):
    return cfg["services"][name]


def drop_nnp(name):
    def m(cfg):
        s = svc(cfg, name)
        s["security_opt"] = [o for o in (s.get("security_opt") or []) if o != "no-new-privileges:true"]
    return m


def setkey(key, value):
    def m(cfg):
        svc(cfg, VI)[key] = value
    return m


def delkey(*keys):
    def m(cfg):
        for k in keys:
            svc(cfg, VI).pop(k, None)
    return m


def both(*ms):
    def m(cfg):
        for f in ms:
            f(cfg)
    return m


def drop_app_env(key):
    def m(cfg):
        svc(cfg, "app")["environment"].pop(key, None)
    return m


def app_volumes_drop(cfg):
    svc(cfg, "app")["volumes"] = []


def init_entrypoint_without_import(cfg):
    svc(cfg, VI)["entrypoint"] = ["/bin/sh", "-c", "chown 10001:10001 /silo && chown 70:70 /backups"]


def app_no_init_dep(cfg):
    svc(cfg, "app")["depends_on"].pop(VI, None)


def off(cfg):
    return svc(cfg, "offsite")


def off_set(key, value):
    def m(cfg):
        off(cfg)[key] = value
    return m


def off_del(*keys):
    def m(cfg):
        for k in keys:
            off(cfg).pop(k, None)
    return m


def off_env(**kv):
    def m(cfg):
        off(cfg)["environment"].update(kv)
    return m


def off_env_drop(key):
    def m(cfg):
        off(cfg)["environment"].pop(key, None)
    return m


def off_mount(vol):
    def m(cfg):
        off(cfg)["volumes"].append(vol)
    return m


def off_backups_rw(cfg):
    for v in off(cfg)["volumes"]:
        if v["target"] == "/backups":
            v.pop("read_only", None)


def off_networks(*names):
    def m(cfg):
        off(cfg)["networks"] = {n: None for n in names}
    return m


def net_internal(name, value):
    def m(cfg):
        cfg["networks"][name]["internal"] = value
    return m


def svc_join(name, net):
    def m(cfg):
        svc(cfg, name).setdefault("networks", {})[net] = None
    return m


def svc_leave(name, net):
    def m(cfg):
        svc(cfg, name)["networks"].pop(net, None)
    return m


def pg_leave_offsite_db(cfg):
    svc(cfg, "postgres")["networks"].pop("offsite_db", None)


def off_remove(cfg):
    del cfg["services"]["offsite"]


def vi_no_offsite_work(cfg):
    svc(cfg, VI)["volumes"] = [v for v in svc(cfg, VI)["volumes"] if v["target"] != "/offsite-work"]


def vi_chown_without_offsite_work(cfg):
    svc(cfg, VI)["entrypoint"] = ["/bin/sh", "-c", "chown 10001:10001 /silo /import-work && chown 70:70 /backups"]


def vol_label_drop(cfg):
    cfg["volumes"]["offsite_work"].pop("labels", None)


def off_no_init_dep(cfg):
    off(cfg)["depends_on"].pop(VI, None)


def rename(cfg):
    cfg["services"]["volume-init-x"] = cfg["services"].pop(VI)


def remove(cfg):
    del cfg["services"][VI]


MISSING = "no-new-privileges exception names a missing service: ['volume-init']"
NET = "volume-init: must have network_mode none and no networks, got network_mode=%s networks=%s"

# (name, mutation, expected exit, exact FAIL lines that must appear)
cases = [
    ("baseline: the real render", None, 0, []),
    ("allowed: no-new-privileges put back on volume-init",
     setkey("security_opt", ["no-new-privileges:true"]), 0, []),
    ("web loses no-new-privileges", drop_nnp("web"), 2, ["web: no no-new-privileges"]),
    ("postgres loses no-new-privileges", drop_nnp("postgres"), 2, ["postgres: no no-new-privileges"]),
    ("silo loses no-new-privileges", drop_nnp("silo"), 2, ["silo: no no-new-privileges"]),
    ("app loses no-new-privileges", drop_nnp("app"), 2, ["app: no no-new-privileges"]),
    ("backup loses no-new-privileges", drop_nnp("backup"), 2, ["backup: no no-new-privileges"]),
    ("volume-init cap_add CHOWN + DAC_OVERRIDE", setkey("cap_add", ["CHOWN", "DAC_OVERRIDE"]), 2,
     ["volume-init: cap_add must be exactly [CHOWN], got ['CHOWN', 'DAC_OVERRIDE']",
      "volume-init: cap_add ['DAC_OVERRIDE'] not allowed"]),
    ("volume-init has no cap_add", delkey("cap_add"), 2,
     ["volume-init: cap_add must be exactly [CHOWN], got None"]),
    ("volume-init has no cap_drop", delkey("cap_drop"), 2,
     ["volume-init: must cap_drop [ALL], got None"]),
    ("volume-init network_mode bridge", setkey("network_mode", "bridge"), 2,
     [NET % ("'bridge'", "None")]),
    ("volume-init gets a networks entry", setkey("networks", {"backend": None}), 2,
     [NET % ("'none'", "{'backend': None}")]),
    ("volume-init network_mode removed (compose adds the default network)",
     both(delkey("network_mode"), setkey("networks", {"default": None})), 2,
     [NET % ("None", "{'default': None}")]),
    ("volume-init network_mode removed, no networks", delkey("network_mode"), 2,
     [NET % ("None", "None")]),
    ("volume-init security_opt seccomp=unconfined", setkey("security_opt", ["seccomp=unconfined"]), 2,
     ["volume-init: security_opt ['seccomp=unconfined'] not allowed"]),
    ("volume-init restart unless-stopped", setkey("restart", "unless-stopped"), 2,
     ["volume-init: must be one-shot (restart \"no\"), got 'unless-stopped'"]),
    ("volume-init restart removed", delkey("restart"), 2,
     ["volume-init: must be one-shot (restart \"no\"), got None"]),
    ("volume-init renamed", rename, 2, [MISSING, "volume-init-x: no no-new-privileges"]),
    ("volume-init removed", remove, 2, [MISSING]),
    ("app loses GIT_ALLOW_PRIVATE_PROVIDERS", drop_app_env("GIT_ALLOW_PRIVATE_PROVIDERS"), 2,
     ["app: GIT_ALLOW_PRIVATE_PROVIDERS is not passed to the app service"]),
    ("app loses GIT_ALLOW_PRIVATE_CIDRS", drop_app_env("GIT_ALLOW_PRIVATE_CIDRS"), 2,
     ["app: GIT_ALLOW_PRIVATE_CIDRS is not passed to the app service"]),
    ("app loses both git switches", both(drop_app_env("GIT_ALLOW_PRIVATE_PROVIDERS"), drop_app_env("GIT_ALLOW_PRIVATE_CIDRS")), 2,
     ["app: GIT_ALLOW_PRIVATE_PROVIDERS is not passed to the app service",
      "app: GIT_ALLOW_PRIVATE_CIDRS is not passed to the app service"]),
    ("app has no import volume", app_volumes_drop, 2,
     ["app: /work/import must be the writable named volume import_work, got None"]),
    ("volume-init does not chown the import volume", init_entrypoint_without_import, 2,
     ["volume-init: must chown 10001:10001 /import-work, got ['/bin/sh', '-c', 'chown 10001:10001 /silo && chown 70:70 /backups']"]),
    ("app does not wait for volume-init", app_no_init_dep, 2,
     ["app: must depend on volume-init (the import volume is chowned there)"]),
    ("allowed: the operator lists a private range for backups",
     off_env(BACKUP_ALLOW_PRIVATE_CIDRS="192.168.1.20/32"), 0, []),
    ("offsite loses no-new-privileges", drop_nnp("offsite"), 2, ["offsite: no no-new-privileges"]),
    ("offsite publishes a port", off_set("ports", [{"mode": "ingress", "target": 8081, "published": "8081", "protocol": "tcp"}]), 2,
     ["offsite: publishes [{'mode': 'ingress', 'target': 8081, 'published': '8081', 'protocol': 'tcp'}]; only web and silo may publish a port"]),
    ("offsite is privileged", off_set("privileged", True), 2, ["offsite: privileged"]),
    ("offsite on the host network", off_set("network_mode", "host"), 2, ["offsite: network_mode host"]),
    ("offsite mounts the Docker socket",
     off_mount({"type": "bind", "source": "/var/run/docker.sock", "target": "/var/run/docker.sock"}), 2,
     ["offsite: mounts the Docker socket",
      "offsite: bind mount /var/run/docker.sock -> /var/run/docker.sock is not allowed",
      "offsite: mounts ['/var/run/docker.sock'] are not allowed"]),
    ("offsite gets an extra bind mount",
     off_mount({"type": "bind", "source": "/srv/x", "target": "/x", "read_only": True}), 2,
     ["offsite: bind mount /srv/x -> /x is not allowed", "offsite: mounts ['/x'] are not allowed"]),
    ("offsite mounts backups read-write", off_backups_rw, 2,
     [lambda cfg: "offsite: /backups must be the named volume backups mounted read-only, got %s" % [v for v in off(cfg)["volumes"] if v["target"] == "/backups"][0]]),
    ("offsite loses read_only", off_set("read_only", False), 2, ["offsite: root file system must be read-only"]),
    ("offsite runs as root", off_set("user", "0:0"), 2, ["offsite: must run as 70:70"]),
    ("offsite has no user", off_del("user"), 2, ["offsite: must run as 70:70"]),
    ("offsite gets cap_add NET_ADMIN", off_set("cap_add", ["NET_ADMIN"]), 2, ["offsite: cap_add ['NET_ADMIN'] not allowed"]),
    ("offsite loses cap_drop", off_del("cap_drop"), 2, ["offsite: must cap_drop [ALL], got None"]),
    ("offsite loses tmpfs /tmp", off_del("tmpfs"), 2, ["offsite: must have tmpfs /tmp, got None"]),
    ("offsite does not wait for volume-init", off_no_init_dep, 2,
     ["offsite: must depend on volume-init (service_completed_successfully)"]),
    ("offsite has neither build nor a pinned image",
     both(off_del("build"), off_set("image", "launchpad-api:unversioned")), 2,
     ["offsite: must carry build (the same image as app)",
      "offsite: image launchpad-api:unversioned is not pinned to a digest"]),
    ("offsite has another image tag", off_set("image", "launchpad-api:other"), 2,
     ["offsite: image 'launchpad-api:other' must be the same tag as app's 'launchpad-api:unversioned'"]),
    ("offsite has another command", off_set("command", ["/bin/server"]), 2,
     ["offsite: command must be ['/bin/server', 'offsite'], got ['/bin/server']"]),
    ("offsite has another health check", off_set("healthcheck", {"test": ["CMD", "true"]}), 2,
     ["offsite: healthcheck must be ['CMD', '/bin/server', 'offsite', 'healthcheck'], got ['CMD', 'true']"]),
    ("offsite LOG_ENV local", off_env(LOG_ENV="local"), 2, ["offsite: LOG_ENV is 'local', want 'production'"]),
    ("offsite uses the owner database role",
     off_env(DATABASE_URL="postgres://devops:x@postgres:5432/devops"), 2,
     ["offsite: DATABASE_URL must use the restricted role 'offsite', not the owner role"]),
    ("offsite gets POSTMARK_SERVER_TOKEN", off_env(POSTMARK_SERVER_TOKEN="x"), 2,
     ["offsite: POSTMARK_SERVER_TOKEN must not be passed"]),
    ("offsite gets SETTINGS_ENCRYPTION_KEY", off_env(SETTINGS_ENCRYPTION_KEY="x"), 2,
     ["offsite: SETTINGS_ENCRYPTION_KEY must not be passed"]),
    ("offsite gets AUTH_OTP_PEPPER", off_env(AUTH_OTP_PEPPER="x"), 2, ["offsite: AUTH_OTP_PEPPER must not be passed"]),
    ("allowed: offsite gets the S3_* of its own silo user (the baseline has them)", None, 0, []),
    ("offsite gets S3_PUBLIC_ENDPOINT", off_env(S3_PUBLIC_ENDPOINT="https://files.example"), 2,
     ["offsite: S3_PUBLIC_ENDPOINT must not be passed"]),
    ("offsite reads another endpoint", off_env(S3_ENDPOINT="http://192.168.1.5:9000"), 2,
     ["offsite: S3_ENDPOINT must be http://silo:9000, got 'http://192.168.1.5:9000'"]),
    ("offsite uses the app's access key",
     lambda cfg: off_env(S3_ACCESS_KEY=svc(cfg, "app")["environment"]["S3_ACCESS_KEY"])(cfg), 2,
     ["offsite: S3_ACCESS_KEY must not be the app's access key (offsite has a read-only user of its own)"]),
    ("offsite uses the app's secret key",
     lambda cfg: off_env(S3_SECRET_KEY=svc(cfg, "app")["environment"]["S3_SECRET_KEY"])(cfg), 2,
     ["offsite: S3_SECRET_KEY must not be the app's secret key (offsite has a read-only user of its own)"]),
    ("offsite uses silo's root user",
     lambda cfg: off_env(S3_ACCESS_KEY=svc(cfg, "silo")["environment"]["MINIO_ROOT_USER"])(cfg), 2,
     ["offsite: S3_ACCESS_KEY must not be silo's root user"]),
    ("offsite uses silo's root password",
     lambda cfg: off_env(S3_SECRET_KEY=svc(cfg, "silo")["environment"]["MINIO_ROOT_PASSWORD"])(cfg), 2,
     ["offsite: S3_SECRET_KEY must not be silo's root password"]),
    ("offsite reads another bucket", off_env(S3_BUCKET="other-bucket"), 2,
     ["offsite: S3_BUCKET must be the app's bucket, got 'other-bucket'"]),
    ("offsite loses S3_SECRET_KEY", off_env_drop("S3_SECRET_KEY"), 2,
     ["offsite: S3_SECRET_KEY is not passed to the offsite service"]),
    ("offsite loses S3_ENDPOINT", off_env_drop("S3_ENDPOINT"), 2,
     ["offsite: S3_ENDPOINT is not passed to the offsite service",
      "offsite: S3_ENDPOINT must be http://silo:9000, got None"]),
    ("offsite gets an unlisted variable", off_env(GIT_ALLOW_PRIVATE_PROVIDERS="1"), 2,
     ["offsite: environment ['GIT_ALLOW_PRIVATE_PROVIDERS'] is not allowed"]),
    ("offsite loses BACKUP_ENCRYPTION_KEY", off_env_drop("BACKUP_ENCRYPTION_KEY"), 2,
     ["offsite: BACKUP_ENCRYPTION_KEY is not passed to the offsite service"]),
    ("offsite loses BACKUP_ALLOW_PRIVATE_CIDRS", off_env_drop("BACKUP_ALLOW_PRIVATE_CIDRS"), 2,
     ["offsite: BACKUP_ALLOW_PRIVATE_CIDRS is not passed to the offsite service"]),
    ("offsite scratch dir is /work (the image's own directory)", off_env(OFFSITE_WORK_DIR="/work"), 2,
     ["offsite: OFFSITE_WORK_DIR must be /offsite-work, got '/work'"]),
    ("offsite joins backend", off_networks("offsite_db", "egress", "files_net", "backend"), 2,
     ["offsite: networks must be exactly egress, files_net and offsite_db, got ['backend', 'egress', 'files_net', 'offsite_db']"]),
    ("offsite joins edge (where app and web are)", off_networks("offsite_db", "egress", "files_net", "edge"), 2,
     ["offsite: networks must be exactly egress, files_net and offsite_db, got ['edge', 'egress', 'files_net', 'offsite_db']"]),
    ("offsite loses egress",
     both(off_networks("offsite_db", "files_net"), svc_leave("postgres", "backend")), 2,
     ["offsite: networks must be exactly egress, files_net and offsite_db, got ['files_net', 'offsite_db']",
      "network egress: services on it must be exactly ['offsite'], got []",
      "postgres: networks must be exactly backend and offsite_db, got ['offsite_db']"]),
    ("offsite loses files_net", off_networks("offsite_db", "egress"), 2,
     ["offsite: networks must be exactly egress, files_net and offsite_db, got ['egress', 'offsite_db']",
      "network files_net: services on it must be exactly ['offsite', 'silo'], got ['silo']"]),
    ("files_net is not internal", net_internal("files_net", False), 2, ["network files_net: must be internal"]),
    ("app joins files_net", svc_join("app", "files_net"), 2,
     ["network files_net: services on it must be exactly ['offsite', 'silo'], got ['app', 'offsite', 'silo']"]),
    ("web joins files_net", svc_join("web", "files_net"), 2,
     ["network files_net: services on it must be exactly ['offsite', 'silo'], got ['offsite', 'silo', 'web']"]),
    ("postgres joins files_net", svc_join("postgres", "files_net"), 2,
     ["network files_net: services on it must be exactly ['offsite', 'silo'], got ['offsite', 'postgres', 'silo']",
      "postgres: networks must be exactly backend and offsite_db, got ['backend', 'files_net', 'offsite_db']"]),
    ("silo leaves files_net", svc_leave("silo", "files_net"), 2,
     ["network files_net: services on it must be exactly ['offsite', 'silo'], got ['offsite']",
      "silo: networks must be exactly edge and files_net, got ['edge']"]),
    ("silo leaves edge (the app could not reach it)", svc_leave("silo", "edge"), 2,
     ["silo: networks must be exactly edge and files_net, got ['files_net']"]),
    ("offsite_db is not internal", net_internal("offsite_db", False), 2, ["network offsite_db: must be internal"]),
    ("egress is internal", net_internal("egress", True), 2,
     ["network egress: must not be internal (it is offsite's route out)"]),
    ("app joins offsite_db", svc_join("app", "offsite_db"), 2,
     ["network offsite_db: services on it must be exactly ['offsite', 'postgres'], got ['app', 'offsite', 'postgres']"]),
    ("app joins egress", svc_join("app", "egress"), 2,
     ["network egress: services on it must be exactly ['offsite'], got ['app', 'offsite']"]),
    ("postgres leaves offsite_db", pg_leave_offsite_db, 2,
     ["network offsite_db: services on it must be exactly ['offsite', 'postgres'], got ['offsite']",
      "postgres: networks must be exactly backend and offsite_db, got ['backend']"]),
    ("offsite removed", off_remove, 2, ["offsite: the service is missing", "offsite: root file system must be read-only"]),
    ("volume-init does not mount offsite_work", vi_no_offsite_work, 2,
     ["volume-init: must mount offsite_work writable at /offsite-work, got None"]),
    ("volume-init does not chown offsite_work", vi_chown_without_offsite_work, 2,
     ["volume-init: must chown 70:70 /backups /offsite-work, got ['/bin/sh', '-c', 'chown 10001:10001 /silo /import-work && chown 70:70 /backups']"]),
    ("offsite_work loses the deploy label", vol_label_drop, 2,
     ["volume offsite_work: must carry the label dev.orbit.deploy=production"]),
    ("app loses BACKUP_ENCRYPTION_KEY", drop_app_env("BACKUP_ENCRYPTION_KEY"), 2,
     ["app: BACKUP_ENCRYPTION_KEY is not passed to the app service"]),
]

failed = 0
for name, mutate, want_rc, want_lines in cases:
    cfg = copy.deepcopy(base)
    if mutate:
        mutate(cfg)
    p = subprocess.run([sys.executable, checker, envfile], input=json.dumps(cfg),
                       capture_output=True, text=True)
    errs = []
    if p.returncode != want_rc:
        errs.append("exit %d, want %d" % (p.returncode, want_rc))
    lines = p.stderr.splitlines()
    if want_rc == 2 and (not lines or lines[0] != "check-compose (deploy): FAIL"):
        errs.append("no 'check-compose (deploy): FAIL' header")
    for want in want_lines:
        if callable(want):
            want = want(cfg)
        if "  " + want not in lines:
            errs.append("missing FAIL line: %s" % want)
    if errs:
        failed += 1
        print("FAIL: %s\n  %s\n  stdout: %s\n  stderr: %s" % (
            name, "\n  ".join(errs), p.stdout.strip(), p.stderr.strip().replace("\n", " | ")))
    else:
        print("ok: %s (exit %d)" % (name, p.returncode))

print()
if failed:
    sys.exit("test-compose-check: FAIL (%d of %d cases)" % (failed, len(cases)))
print("test-compose-check: PASS (%d cases)" % len(cases))
EOF
