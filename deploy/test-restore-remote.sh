#!/usr/bin/env bash
# The remote-copy restore test (T16 ticket 12). Needs only docker (and
# a golang image for the build); publishes no port.
#
#   deploy/test-restore-remote.sh          (part of make restore-test in server/)
#
# A real encrypted, signed pair (the production library code: age + Ed25519) sits
# on a fake S3 server that runs in a container on a NON-loopback address (the
# `restorefake` test tool, never in the production image). restore.sh fetches it
# in throwaway containers of the real server binary and restores it into a scratch
# database of a throwaway postgres, with the row counts checked.
#
#  1. happy paths: --remote-config on a user network and on the default bridge,
#     `latest` (newest pair, manifest time printed), an explicit STEM, the key on
#     the standard input, --trust-remote (its warning), --verify-only, and
#     --remote NAME through a docker shim that stands in for the offsite container.
#  2. refusals before decrypt: wrong age key, bad signature, wrong verify key,
#     sha mismatch, swapped manifest, a pair renamed from another install id, the
#     database's own sha record, no space for the object. None leaves a scratch
#     database, and none runs `decrypt` (except the wrong key, which is its test).
#  3. a failed fetch fails the restore (pipefail), proved with a mutant script.
#  4. a planted secret (private key, destination secret) is in no argv, no
#     environment, no output, no ps sample, and the work folder is gone.
# The docker shim only RECORDS the calls (and, for three named cases, bends one).
# Scratch: DEPLOY_TMPDIR (default $TMPDIR, else /tmp).
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/.." && pwd)"
pgimage="$(grep -m1 -E '^\s+image: postgres:' "$here/docker-compose.yml" | awk '{print $2}')"
[[ -n "$pgimage" ]] || {
	echo "test-restore-remote: no postgres image in deploy/docker-compose.yml" >&2
	exit 2
}
runtime_base='alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc'
golang_image="${RESTORE_TEST_GOLANG_IMAGE:-golang:1.27.3}"
gomod_vol="${RESTORE_TEST_GOMOD_VOLUME:-dev-ops-gomod}"
gocache_vol="${RESTORE_TEST_GOCACHE_VOLUME:-dev-ops-gocache}"

sfx="$$"
c="dev-ops-restore-remote-pg-$sfx"
fake="dev-ops-restore-remote-fake-$sfx"
net="dev-ops-restore-remote-net-$sfx"
img="dev-ops-restore-remote-img:$sfx"
scratch="$(mktemp -d "${DEPLOY_TMPDIR:-${TMPDIR:-/tmp}}/devops-restore-remote.XXXXXX")"
chmod 700 "$scratch"
cleanup() {
	docker rm -f "$c" "$fake" >/dev/null 2>&1 || true
	docker network rm "$net" >/dev/null 2>&1 || true
	docker image rm "$img" >/dev/null 2>&1 || true
	[[ -z "${sampler:-}" ]] || kill "$sampler" 2>/dev/null || true
	rm -rf "$scratch"
}
trap cleanup EXIT

step() { printf '\n== %s\n' "$*"; }
fail() {
	echo "test-restore-remote: FAIL: $*" >&2
	exit 1
}
real_docker="$(command -v docker)"

# ---------------------------------------------------------------- the image

step "build the server binary and the test tool (Go in docker), then a small image like the production one"
mkdir -p "$scratch/ctx" "$scratch/work" "$scratch/bin"
docker run --rm -v "$repo/server:/src:ro" -w /src -v "$scratch/ctx:/out" \
	-v "$gomod_vol:/go/pkg/mod" -v "$gocache_vol:/root/.cache/go-build" \
	-e CGO_ENABLED=0 -e GOOS=linux "$golang_image" sh -c \
	'go build -trimpath -o /out/server ./cmd/server && go build -trimpath -o /out/restorefake ./internal/offsite/restorefake' ||
	fail "could not build the binaries"
cat >"$scratch/ctx/Dockerfile" <<EOF
FROM $runtime_base
RUN apk add --no-cache ca-certificates && adduser -D -H -u 10001 devops
COPY server /bin/server
COPY restorefake /bin/restorefake
USER devops
EOF
docker build -q -t "$img" "$scratch/ctx" >/dev/null || fail "could not build the test image"
# The production image must not carry the test tool.
grep -q 'restorefake' "$repo/server/Dockerfile" && fail "server/Dockerfile mentions restorefake"

# ---------------------------------------------------------------- postgres and the dumps

step "postgres $pgimage (container $c)"
docker run -d --rm --name "$c" \
	-e POSTGRES_USER=devops -e POSTGRES_DB=devops -e POSTGRES_PASSWORD=restore-test-only \
	-e PGHOST=127.0.0.1 -e PGUSER=devops -e PGPASSWORD=restore-test-only -e PGDATABASE=devops \
	"$pgimage" >/dev/null
for _ in $(seq 1 60); do
	docker exec "$c" psql -h 127.0.0.1 -tAc 'select 1' >/dev/null 2>&1 && break
	sleep 1
done
sleep 2
until docker exec "$c" psql -tAc 'select 1' >/dev/null 2>&1; do sleep 1; done
version=0
for f in "$repo"/server/db/migrations/*.up.sql; do
	docker exec -i "$c" psql -X -q -v ON_ERROR_STOP=1 <"$f" >/dev/null
	b="$(basename "$f")"
	num=$((10#${b%%_*}))
	((num > version)) && version=$num
done
docker exec -i "$c" psql -X -q -v ON_ERROR_STOP=1 >/dev/null <<SQL
CREATE TABLE IF NOT EXISTS schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL);
INSERT INTO schema_migrations VALUES ($version, false);
INSERT INTO orgs (slug, name) VALUES ('orbit', 'Orbit');
INSERT INTO users (username, name, email, is_agent) VALUES ('chen', 'Chen', 'chen@example.com', false);
INSERT INTO teams (org_id, key, name) SELECT id, 'ORB', 'Orbit' FROM orgs;
INSERT INTO workflow_states (org_id, team_id, name, type, position) SELECT org_id, id, 'Backlog', 'backlog', 0 FROM teams;
UPDATE teams t SET default_state_id = (SELECT id FROM workflow_states w WHERE w.team_id = t.id);
INSERT INTO issue_counters (team_id, org_id, next_number) SELECT id, org_id, 1000 FROM teams;
SQL
add_issues() { # FROM TO
	docker exec -i "$c" psql -X -q -v ON_ERROR_STOP=1 >/dev/null <<SQL
INSERT INTO issues (org_id, team_id, number, title, description, state_id, priority, creator_id)
  SELECT t.org_id, t.id, g, 'Issue ' || g, 'Body ' || g, (SELECT id FROM workflow_states w WHERE w.team_id = t.id LIMIT 1), 0,
         (SELECT id FROM users WHERE username = 'chen')
  FROM teams t, generate_series($1, $2) g;
SQL
}
add_issues 1 50
docker exec "$c" pg_dump -Fc -d devops >"$scratch/old.dump"
add_issues 51 60
docker exec "$c" pg_dump -Fc -d devops >"$scratch/new.dump"
echo "dumps: old $(wc -c <"$scratch/old.dump") bytes (50 issues), new $(wc -c <"$scratch/new.dump") bytes (60 issues)"

# ---------------------------------------------------------------- the fake remote

inst=ab12cd34
other_inst=0badc0de
keys="$scratch/keys" remote="$scratch/remote"
mkdir -p "$remote"
gen() { # DUMP INSTALL TIME [REMOTE_DIR]  -> "stem sha"
	docker run --rm --user "$(id -u):$(id -g)" -v "$scratch:/s" "$img" /bin/restorefake gen \
		--dump "/s/$1" --remote "/s/${4:-remote}" --keys /s/keys --install-id "$2" --time "$3"
}
read -r old_stem _ < <(gen old.dump "$inst" 20261001T031500Z)
read -r new_stem new_sha < <(gen new.dump "$inst" 20261027T031500Z)
echo "old pair $old_stem"
echo "new pair $new_stem"
cp -Rp "$remote" "$scratch/remote.orig"
reset_remote() {
	rm -rf "${remote:?}"/*
	cp -p "$scratch/remote.orig/"* "$remote/"
}
chmod -R a+rX "$scratch/remote" "$scratch/remote.orig"

docker network create "$net" >/dev/null
docker run -d --name "$fake" --network "$net" --user "$(id -u):$(id -g)" -v "$remote:/remote:ro" "$img" \
	/bin/restorefake serve --dir /remote --addr 0.0.0.0:9000 --bucket backups >/dev/null
docker network connect bridge "$fake"
net_ip="$(docker inspect -f "{{(index .NetworkSettings.Networks \"$net\").IPAddress}}" "$fake")"
net_subnet="$(docker network inspect -f '{{range .IPAM.Config}}{{.Subnet}}{{end}}' "$net")"
br_ip="$(docker inspect -f '{{(index .NetworkSettings.Networks "bridge").IPAddress}}' "$fake")"
br_subnet="$(docker network inspect -f '{{range .IPAM.Config}}{{if .Subnet}}{{.Subnet}} {{end}}{{end}}' bridge | awk '{print $1}')"
echo "fake S3 at $net_ip:9000 ($net_subnet) and $br_ip:9000 (default bridge $br_subnet)"
for _ in $(seq 1 30); do
	docker exec "$fake" wget -qO- "http://127.0.0.1:9000/backups/?list-type=2" >/dev/null 2>&1 && break
	sleep 1
done
[[ "$net_ip" =~ ^[0-9.]+$ && "$br_ip" =~ ^[0-9.]+$ ]] || fail "no addresses for the fake remote"

# Random per run, so a parallel run of this test on the same host (its ps samples) cannot match.
planted_access="PLANTEDaccess$(od -An -N12 -tx1 /dev/urandom | tr -d ' \n')"
planted_secret="PLANTEDsecret$(od -An -N12 -tx1 /dev/urandom | tr -d ' \n')"
write_cfg() { # FILE IP
	printf '{"type":"s3","endpoint":"http://%s:9000","bucket":"backups","region":"us-east-1","addressing":"path","accessKey":"%s","secretKey":"%s"}\n' \
		"$2" "$planted_access" "$planted_secret" >"$1"
	chmod 600 "$1"
}
write_cfg "$scratch/net.json" "$net_ip"
write_cfg "$scratch/bridge.json" "$br_ip"
agekey="$(grep '^AGE-SECRET-KEY-' "$keys/age.key")"
wrongkey="$(grep '^AGE-SECRET-KEY-' "$keys/age-wrong.key")"
planted=("$agekey" "$wrongkey" "$planted_secret" "$planted_access")

# The database record of the new object, for the database sha check.
docker exec -i "$c" psql -X -q -v ON_ERROR_STOP=1 >/dev/null <<SQL
UPDATE backup_settings SET install_id = '$inst';
INSERT INTO backup_runs (destination_name, trigger, status, finished_at, bytes, sha256, remote_name)
  VALUES ('Offsite NAS', 'manual', 'succeeded', now(), 1, '$new_sha', '$new_stem.dump.age');
SQL

# ---------------------------------------------------------------- the docker shim

shim="$scratch/shim"
mkdir -p "$shim"
SHIM_LOG="$scratch/shim.log" SHIM_ENVLOG="$scratch/shim.env"
cat >"$shim/docker" <<'SHIM'
#!/usr/bin/env bash
# A recording stand-in for the docker client in restore.sh's tests. It logs every
# call (arguments and environment), then runs the real docker. Three named cases
# bend ONE call: the file fetch_fail (the fetch container exits 1 after a complete
# frame), the file tiny_out (the unpack folder is a 1 MB tmpfs) and the `compose`
# calls of the stack path, which it answers itself (there is no stack here).
# restore.sh runs docker through docker_clean, which drops every variable that is not
# on its allowlist: this shim's settings are in a file beside it, not in the environment.
here="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=/dev/null
source "$here/conf"
printf '%q ' "$@" >>"$SHIM_LOG"
printf '\n' >>"$SHIM_LOG"
env >>"$SHIM_ENVLOG"
real="$SHIM_REAL"
args=("$@")
# the config file is replaced between its check and its use (a new inode, same content).
if [[ "$1" == image && "${2:-}" == inspect && -e "$here/swap_cfg" ]]; then
	f="$(cat "$here/swap_cfg")"
	cp -p "$f" "$f.swap" && rm -f "$f" && mv "$f.swap" "$f"
fi
# the database volume reports 1 KB free.
if [[ "$1" == exec && " $* " == *" df -Pk "* && -e "$here/tiny_pgdf" ]]; then
	echo 1
	exit 0
fi
if [[ "$1" == run ]]; then
	for a in "$@"; do
		if [[ "$a" == *:/identity:ro ]]; then
			python3 -c 'import os,sys; print("IDENTITY-MODE %o" % (os.stat(sys.argv[1]).st_mode & 0o777))' "${a%:/identity:ro}" >>"$SHIM_LOG"
		fi
	done
	if [[ -e "$here/tiny_out" && " $* " == *" offsite unpack "* ]]; then
		new=()
		for a in "$@"; do
			if [[ "$a" == */in:/out ]]; then
				unset 'new[${#new[@]}-1]' # the -v before it
				new+=(--tmpfs /out:size=1m,mode=1777)
			else
				new+=("$a")
			fi
		done
		args=("${new[@]}")
	fi
	if [[ -e "$here/fill_out" && " $* " == *" offsite fetch "* && " $* " == *" --files "* ]]; then
		touch "$(cat "$here/fill_out")/late.txt" # the folder is filled while the list is fetched
	fi
	if [[ -e "$here/fetch_fail" && " $* " == *" offsite fetch "* ]]; then
		"$real" "${args[@]}"
		exit 1
	fi
fi
if [[ "$1" == compose ]]; then
	shift
	while [[ "${1:-}" == -* ]]; do shift 2; done
	sub="$1"
	shift
	case "$sub" in
	ps) # ps -q SERVICE
		printf 'fakecid-%s\n' "${@: -1}"
		exit 0
		;;
	exec) # exec -T SERVICE CMD...
		shift # -T
		svc="$1"
		shift
		if [[ "$svc" == offsite ]]; then
			echo "OFFSITE-EXEC $*" >>"$SHIM_LOG"
			# Only the destination name and the stem are taken from the call; the test's
			# own config stands in for the stored destination.
			dest="" stem="" extra=()
			while [[ $# -gt 0 ]]; do
				case "$1" in
				--destination) dest="$2" ;;
				--stem) stem="$2" ;;
				--files) extra+=(--files) ;;
				--file-object) extra+=(--file-object "$2") ;;
				esac
				shift
			done
			[[ "$dest" == "$SHIM_EXPECT_DEST" ]] || exit 3
			if [[ -n "$stem" ]]; then extra+=(--stem "$stem"); fi
			if [[ -e "$here/fetch_fail" ]]; then
				"$real" run --rm --pull never --user "$(id -u):$(id -g)" --network "$SHIM_NET" \
					-v "$SHIM_CFG:/config.json:ro" "$SHIM_IMG" /bin/server offsite fetch --config /config.json \
					--install-id "$SHIM_INST" "${extra[@]}" --allow-cidr "$SHIM_CIDR"
				exit 1
			fi
			exec "$real" run --rm --pull never --user "$(id -u):$(id -g)" --network "$SHIM_NET" \
				-v "$SHIM_CFG:/config.json:ro" "$SHIM_IMG" /bin/server offsite fetch --config /config.json \
				--install-id "$SHIM_INST" "${extra[@]}" --allow-cidr "$SHIM_CIDR"
		fi
		exec "$real" exec -i "$SHIM_PG" "$@"
		;;
	esac
	exit 9
fi
if [[ "$1" == inspect && " $* " == *" fakecid-"* ]]; then
	printf '%s\n' "$SHIM_IMG"
	exit 0
fi
exec "$real" "${args[@]}"
SHIM
chmod 755 "$shim/docker"
# A df that reports 1 KB free when the file tiny_hostdf is there (the host's free-space check).
cat >"$shim/df" <<'SHIM'
#!/usr/bin/env bash
here="$(cd "$(dirname "$0")" && pwd)"
if [[ -e "$here/fixed_hostdf" ]]; then # the free space in KB is the file's content
	printf 'Filesystem 1024-blocks Used Available Capacity Mounted on\nfake 9999999 1 %s 1%% /\n' "$(cat "$here/fixed_hostdf")"
	exit 0
fi
if [[ -e "$here/tiny_hostdf" ]]; then
	printf 'Filesystem 1024-blocks Used Available Capacity Mounted on\nfake 100 99 1 99%% /\n'
	exit 0
fi
PATH="$(getconf PATH)" exec df "$@"
SHIM
chmod 755 "$shim/df"
{
	printf 'SHIM_LOG=%q\nSHIM_ENVLOG=%q\n' "$SHIM_LOG" "$SHIM_ENVLOG"
	printf 'SHIM_REAL=%q\nSHIM_PG=%q\nSHIM_IMG=%q\nSHIM_NET=%q\n' "$real_docker" "$c" "$img" "$net"
	printf 'SHIM_INST=%q\nSHIM_CIDR=%q\nSHIM_CFG=%q\nSHIM_EXPECT_DEST=%q\n' "$inst" "$net_subnet" "$scratch/net.json" "Offsite NAS"
} >"$shim/conf"
work="$scratch/work"
allout="$scratch/all.out" alllog="$scratch/all.log" allenv="$scratch/all.env" allps="$scratch/all.ps"
: >"$allout"
: >"$alllog"
: >"$allenv"
: >"$allps"

out="" rc=0
# rr SCRIPT ARGS...: restore.sh (or a copy) under the shim; sets $out and $rc; the
# shim log of THIS run is $SHIM_LOG. STDIN of the call is RR_STDIN (default /dev/null).
rr() {
	local script="$1"
	shift
	: >"$SHIM_LOG"
	: >"$SHIM_ENVLOG"
	rc=0
	out="$(PATH="$shim:${RR_PATH_EXTRA:+$RR_PATH_EXTRA:}$PATH" TMPDIR="$work" DEPLOY_ENV_FILE="$scratch/none.env" "$script" "$@" 2>&1 <"${RR_STDIN:-/dev/null}")" || rc=$?
	printf '%s\n' "$out" >>"$allout"
	cat "$SHIM_LOG" >>"$alllog"
	cat "$SHIM_ENVLOG" >>"$allenv"
}
base=(--container "$c" --image "$img" --verify-key "$keys/signing.pub" --identity "$keys/age.key" --install-id "$inst"
	--remote-config "$scratch/net.json" --fetch-network "$net" --allow-cidr "$net_subnet" --work-dir "$work" --no-host-age)
no_scratch_db() {
	[[ -z "$(docker exec "$c" psql -X -tAc "SELECT datname FROM pg_database WHERE datname LIKE 'restore_%'")" ]] ||
		fail "a scratch database was left ($1)"
	[[ -z "$(ls -A "$work")" ]] || fail "the work folder was not removed ($1): $(ls -A "$work")"
}
# refused_early NAME EXPECTED_TEXT: the last rr failed, said the text, made no
# database, ran no decrypt and no pg_restore, and left nothing behind.
refused_early() {
	((rc != 0)) || {
		echo "$out"
		fail "$1: exit 0 (it must fail)"
	}
	grep -q -- "$2" <<<"$out" || {
		echo "$out"
		fail "$1: no '$2' in the output"
	}
	grep -q 'CREATE DATABASE' "$SHIM_LOG" && fail "$1: a database was created"
	grep -q 'pg_restore' "$SHIM_LOG" && fail "$1: pg_restore ran"
	grep -q 'offsite decrypt' "$SHIM_LOG" && fail "$1: decrypt ran"
	grep -q 'restore: OK' <<<"$out" && fail "$1: printed restore: OK"
	no_scratch_db "$1"
	echo "ok: $1 -> exit $rc, '$2', nothing created, no decrypt"
}

# ---------------------------------------------------------------- 1. happy paths

step "ps sampler (looks for a secret in any process list while the tests run)"
(while :; do
	ps -axww -o args= >>"$allps" 2>/dev/null
	sleep 0.2
done) &
sampler=$!

step "latest from the remote config on a user network: the newest pair, 60 issues, manifest time printed"
rr "$here/restore.sh" "${base[@]}" latest
echo "$out"
((rc == 0)) || fail "latest failed (exit $rc)"
grep -q '^rows issues 60$' <<<"$out" || fail "latest did not restore the NEW pair (want 60 issues)"
grep -q "restore: signature and sha256 OK (install id $inst, $new_stem)" <<<"$out" || fail "no signature line"
grep -q 'dump time 2026-10-27 03:15:00 UTC (.* old), from the SIGNED manifest' <<<"$out" || fail "no manifest time and age line"
grep -q "'latest' is the newest COMPLETE pair" <<<"$out" || fail "no freshness warning for latest"
grep -q 'made by Launchpad test, database schema version 21' <<<"$out" || fail "no version line"
grep -q 'the object.s sha256 equals the database record' <<<"$out" || fail "the database record was not checked"
grep -q 'delete the --remote-config file now' <<<"$out" || fail "no reminder to delete the config"
grep -q 'restore: OK - ' <<<"$out" || fail "no restore: OK"
grep -q 'ROLLBACK' <<<"$out" && fail "the newest pair warned about a rollback"
grep -q 'the database volume has room for the plain dump' <<<"$out" || fail "no free-space line for the database volume"
no_scratch_db latest
# The documented hardening of each throwaway command.
fl="$(grep ' offsite fetch ' "$SHIM_LOG")"
for want in '--log-driver none' '--read-only' '--cap-drop ALL' "--user $(id -u):$(id -g)" "--network $net" '--pull never' '/config.json:ro'; do
	grep -q -- "$(printf '%s' "$want" | sed 's/[][\\.*^$]/\\&/g')" <<<"$fl" || fail "the fetch call lacks: $want"
done
grep -q -- '--network none' < <(grep ' offsite unpack ' "$SHIM_LOG") || fail "unpack is not --network none"
grep -q -- '--network none' < <(grep ' offsite verify ' "$SHIM_LOG") || fail "verify is not --network none"
grep -q -- '--network none' < <(grep ' offsite decrypt ' "$SHIM_LOG") || fail "decrypt is not --network none"
grep -q -- '--log-driver none' < <(grep ' offsite decrypt ' "$SHIM_LOG") || fail "decrypt keeps its logs"
for v in unpack verify decrypt fetch; do
	grep -q -- "--user $(id -u):$(id -g)" < <(grep " offsite $v " "$SHIM_LOG") || fail "$v does not run as the caller"
done
echo "ok: latest restored the new pair; every throwaway command is read-only, cap-dropped, log-less and runs as the caller"

step "an explicit STEM (the old pair), the key on the standard input, the work folder gone"
RR_STDIN="$scratch/stdin.key"
printf '# a comment\n%s\n' "$agekey" >"$RR_STDIN"
rr "$here/restore.sh" --container "$c" --image "$img" --verify-key "$keys/signing.pub" --identity - --install-id "$inst" \
	--remote-config "$scratch/net.json" --fetch-network "$net" --allow-cidr "$net_subnet" --work-dir "$work" --no-host-age "$old_stem"
unset RR_STDIN
echo "$out" | tail -n 4
((rc == 0)) || fail "the old stem failed (exit $rc)"
grep -q '^rows issues 50$' <<<"$out" || fail "the old stem did not restore 50 issues"
grep -q "'latest' is the newest" <<<"$out" && fail "an explicit stem printed the latest warning"
grep -q 'no record of this object in the database' <<<"$out" || fail "the old pair has no record and must say so"
grep -q 'ROLLBACK: the database records a succeeded backup NEWER than this copy' <<<"$out" || fail "restoring the old pair did not warn about a rollback"
grep -q '^IDENTITY-MODE 600$' "$SHIM_LOG" || fail "the key file the container reads is not mode 0600"
no_scratch_db stem
echo "ok: stem restored 50 issues; the key from the standard input went to a 0600 file in the 0700 work folder, now removed"

step "the default bridge network with --remote-config (a new host: no stack, no deploy/.env, no fetch network)"
rr "$here/restore.sh" --container "$c" --image "$img" --verify-key "$keys/signing.pub" --identity "$keys/age.key" --install-id "$inst" \
	--remote-config "$scratch/bridge.json" --allow-cidr "$br_subnet" --work-dir "$work" --no-host-age latest
echo "$out" | tail -n 3
((rc == 0)) || fail "the bridge restore failed (exit $rc)"
grep -q -- '--network bridge' "$SHIM_LOG" || fail "the fetch did not use the default bridge"
grep -q '^rows issues 60$' <<<"$out" || fail "bridge restore: wrong count"
[[ ! -e "$scratch/none.env" ]] || fail "unexpected env file"
no_scratch_db bridge
echo "ok: restored over the default bridge with no deploy/.env"

step "--verify-only fetches and checks but makes no database"
rr "$here/restore.sh" "${base[@]}" --verify-only latest
echo "$out" | tail -n 2
((rc == 0)) || fail "verify-only failed"
grep -q 'verify-only OK' <<<"$out" || fail "no verify-only OK"
grep -q 'CREATE DATABASE' "$SHIM_LOG" && fail "verify-only made a database"
no_scratch_db verify-only
echo "ok: verify-only"

step "--trust-remote prints its warning and skips the signature"
rr "$here/restore.sh" --container "$c" --image "$img" --trust-remote --yes-trust-remote "$inst" --identity "$keys/age.key" --install-id "$inst" \
	--remote-config "$scratch/net.json" --fetch-network "$net" --allow-cidr "$net_subnet" --work-dir "$work" --no-host-age latest
echo "$out" | head -n 8
((rc == 0)) || fail "trust-remote restore failed"
grep -q 'arbitrary SQL as the bootstrap superuser' <<<"$out" || fail "the warning does not name arbitrary SQL as the bootstrap superuser"
grep -q 'UNVERIFIED' <<<"$out" || fail "the dump time is not marked unverified"
grep -q ' offsite verify ' "$SHIM_LOG" && fail "--trust-remote ran verify"
# The warning comes BEFORE the fetch.
[[ "$(grep -n 'arbitrary SQL as the bootstrap superuser' <<<"$out" | head -n 1 | cut -d: -f1)" -lt "$(grep -n 'restore: fetching' <<<"$out" | head -n 1 | cut -d: -f1)" ]] ||
	fail "the --trust-remote warning is not printed before the fetch"
no_scratch_db trust
echo "ok: --trust-remote warned and skipped verify"

step "--remote NAME through the stack path (the shim answers compose: an offsite container and a backup container)"
stack_env="$scratch/stack.env"
sed -e "s/^DEPLOY_PROJECT=.*/DEPLOY_PROJECT='devops-restore-remote-$sfx'/" "$here/.env.example" >"$stack_env"
chmod 600 "$stack_env"
: >"$SHIM_LOG"
: >"$SHIM_ENVLOG"
rc=0
out="$(PATH="$shim:$PATH" TMPDIR="$work" DEPLOY_ENV_FILE="$stack_env" "$here/restore.sh" --remote "Offsite NAS" \
	--verify-key "$keys/signing.pub" --identity "$keys/age.key" --work-dir "$work" --no-host-age latest 2>&1 </dev/null)" || rc=$?
printf '%s\n' "$out" >>"$allout"
cat "$SHIM_LOG" >>"$alllog"
cat "$SHIM_ENVLOG" >>"$allenv"
echo "$out" | tail -n 4
((rc == 0)) || fail "--remote NAME failed (exit $rc)"
grep -q -- 'OFFSITE-EXEC /bin/server offsite fetch --destination Offsite NAS --stem latest' "$SHIM_LOG" || fail "the offsite exec has the wrong arguments"
grep -q '^rows issues 60$' <<<"$out" || fail "--remote NAME restored the wrong pair"
grep -q "install id $inst" <<<"$out" || fail "the install id was not read from the database"
no_scratch_db stack
rm -f "$stack_env"
echo "ok: --remote NAME ran fetch in the offsite container and took the install id from the database"

# ---------------------------------------------------------------- 2. refusals

step "a wrong age key fails after the check and before any restore starts"
rr "$here/restore.sh" --container "$c" --image "$img" --verify-key "$keys/signing.pub" --identity "$keys/age-wrong.key" --install-id "$inst" \
	--remote-config "$scratch/net.json" --fetch-network "$net" --allow-cidr "$net_subnet" --work-dir "$work" --no-host-age latest
((rc != 0)) || fail "a wrong key restored"
grep -q 'private key does not decrypt this backup' <<<"$out" || fail "no message for a wrong key"
grep -q 'CREATE DATABASE' "$SHIM_LOG" && fail "a wrong key still created a database"
grep -q 'pg_restore' "$SHIM_LOG" && fail "a wrong key still ran pg_restore"
no_scratch_db wrongkey
echo "ok: wrong key -> exit $rc, no database"

step "a bad signature fails before decrypt"
reset_remote
python3 -I - "$remote/$new_stem.json" <<'PY'
import sys, json, base64
p = sys.argv[1]
e = json.load(open(p))
sig = bytearray(base64.b64decode(e["signature"]))
sig[0] ^= 1
e["signature"] = base64.b64encode(bytes(sig)).decode()
json.dump(e, open(p, "w"))
PY
rr "$here/restore.sh" "${base[@]}" latest
refused_early "bad signature" 'signature or hash check FAILED'
grep -q 'signature is not valid' <<<"$out" || fail "bad signature: no fixed reason"

step "the wrong verify key fails before decrypt"
reset_remote
rr "$here/restore.sh" "${base[@]}" --verify-key "$keys/signing-wrong.pub" latest
refused_early "wrong verify key" 'signature or hash check FAILED'

step "a changed object (same size, so the sha256 differs) fails before decrypt"
reset_remote
python3 -I - "$remote/$new_stem.dump.age" <<'PY'
import sys
p = sys.argv[1]
f = open(p, "r+b")
f.seek(200)
b = f.read(1)
f.seek(200)
f.write(bytes([b[0] ^ 1]))
f.close()
PY
rr "$here/restore.sh" "${base[@]}" latest
refused_early "changed object" 'signature or hash check FAILED'
grep -q 'does not match its manifest' <<<"$out" || fail "sha mismatch: no fixed reason"

step "a swapped manifest (an old manifest under the new name) fails: for a stem and for latest"
reset_remote
cp -p "$scratch/remote.orig/$old_stem.json" "$remote/$new_stem.json"
rr "$here/restore.sh" "${base[@]}" "$new_stem"
refused_early "swapped manifest, explicit stem" 'manifest names another backup'
rr "$here/restore.sh" "${base[@]}" latest
refused_early "swapped manifest, latest" 'signature or hash check FAILED'

step "a pair renamed from another install id fails the install id binding"
reset_remote
read -r ostem _ < <(gen new.dump "$other_inst" 20261028T010000Z)
renamed="launchpad-$inst-20261028T010000Z-${ostem##*-}"
mv "$remote/$ostem.dump.age" "$remote/$renamed.dump.age"
mv "$remote/$ostem.json" "$remote/$renamed.json"
chmod a+r "$remote/"*
rr "$here/restore.sh" "${base[@]}" latest
refused_early "renamed pair" 'signature or hash check FAILED'
grep -q 'does not belong to this object' <<<"$out" || fail "renamed pair: no binding reason"
rr "$here/restore.sh" "${base[@]}" "$renamed"
refused_early "renamed pair, explicit stem" 'manifest names another backup'

step "a stem of another install id, and a wrong --install-id"
reset_remote
rr "$here/restore.sh" "${base[@]}" "launchpad-$other_inst-20261001T031500Z-aaaa"
refused_early "stem of another install" 'belongs to another install id'
rr "$here/restore.sh" "${base[@]}" --install-id "$other_inst" latest
refused_early "wrong install id (nothing under that id)" 'FAILED'

step "the database's own record of the object: a different sha256 fails before decrypt"
docker exec "$c" psql -X -q -c "UPDATE backup_runs SET sha256 = repeat('0', 64)"
rr "$here/restore.sh" "${base[@]}" latest
refused_early "database sha mismatch" 'differs from the database record'
docker exec "$c" psql -X -q -c "UPDATE backup_runs SET sha256 = '$new_sha'"

step "no space for the declared object: unpack refuses (no_space) and nothing is left"
touch "$shim/tiny_out"
rr "$here/restore.sh" "${base[@]}" latest
rm -f "$shim/tiny_out"
refused_early "no space" 'FAILED'
grep -q 'not enough free space' <<<"$out" || {
	echo "$out"
	fail "no_space: no fixed reason"
}

# ---------------------------------------------------------------- argument refusals

step "argument refusals (nothing is fetched)"
chmod 644 "$scratch/net.json"
rr "$here/restore.sh" "${base[@]}" latest
refused_early "config readable by others" 'readable by group or other'
chmod 600 "$scratch/net.json"
rr "$here/restore.sh" "${base[@]}" --verify-key "$keys/signing.pub" --trust-remote latest
refused_early "verify-key and trust-remote" 'not both'
nokey=() skip=0
for a in "${base[@]}"; do
	if ((skip)); then
		skip=0
	elif [[ "$a" == --verify-key ]]; then
		skip=1
	else
		nokey+=("$a")
	fi
done
rr "$here/restore.sh" "${nokey[@]}" latest
refused_early "no verify key" 'needs --verify-key FILE'
rr "$here/restore.sh" "${base[@]}" ../../etc/passwd
refused_early "a path as the stem" "or a STEM"
rr "$here/restore.sh" --container "$c" --remote "Offsite NAS" --verify-key "$keys/signing.pub" --identity "$keys/age.key" latest
refused_early "--remote with --container" 'use --remote-config FILE with --container'
RR_STDIN="$scratch/bad.key"
echo "PLANTEDnotAKeyAtAll123" >"$RR_STDIN"
rr "$here/restore.sh" --container "$c" --image "$img" --verify-key "$keys/signing.pub" --identity - --install-id "$inst" \
	--remote-config "$scratch/net.json" latest
unset RR_STDIN
refused_early "a malformed key on stdin" 'not an age private key'
grep -q PLANTEDnotAKeyAtAll <<<"$out" && fail "a malformed key was echoed"
rr "$here/restore.sh" --container "$c" --image "$img" --verify-key "$keys/signing.pub" --install-id "$inst" \
	--remote-config "$scratch/net.json" latest
refused_early "no key and no terminal" 'no terminal for a hidden prompt'
rr "$here/restore.sh" --container "$c" --verify-key "$keys/signing.pub" --identity "$keys/age.key" --install-id "$inst" \
	--remote-config "$scratch/net.json" latest
refused_early "no image" 'give --image IMAGE'
rr "$here/restore.sh" --container "$c" --image "$img" --verify-key "$keys/signing.pub" --identity "$keys/age.key" \
	--remote-config "$scratch/net.json" --work-dir "$work" --name devops latest
refused_early "scratch name devops" "refusing to restore into 'devops'"
rr "$here/restore.sh" --container "$c" --identity "$keys/age.key" "$scratch/old.dump"
refused_early "a remote flag on a local restore" 'needs --remote NAME or --remote-config FILE'

# ---------------------------------------------------------------- 3. pipefail

step "a failed fetch fails the restore (pipefail), and a mutant without pipefail proves the test sees it"
reset_remote
touch "$shim/fetch_fail"
rr "$here/restore.sh" "${base[@]}" latest
((rc != 0)) || fail "a fetch that exited 1 after a whole frame restored"
grep -q 'fetch or the unpack FAILED' <<<"$out" || fail "no message for the failed fetch"
grep -q 'CREATE DATABASE' "$SHIM_LOG" && fail "a failed fetch still created a database"
no_scratch_db pipefail
mkdir -p "$scratch/mutant"
cp "$here/restore.sh" "$here/lib.sh" "$scratch/mutant/"
sed -e 's/^set -euo pipefail$/set -eu/' -e '/^[[:space:]]*set -o pipefail$/d' "$here/restore.sh" >"$scratch/mutant/restore.sh"
chmod 755 "$scratch/mutant/restore.sh"
cmp -s "$here/restore.sh" "$scratch/mutant/restore.sh" && fail "the mutant is the same file"
grep -q pipefail "$scratch/mutant/restore.sh" && grep -v '^#' "$scratch/mutant/restore.sh" | grep -q 'set .*pipefail' && fail "the mutant still sets pipefail"
rr "$scratch/mutant/restore.sh" "${base[@]}" latest
rm -f "$shim/fetch_fail"
if ((rc == 0)) && grep -q 'restore: OK' <<<"$out"; then
	echo "ok: the mutant (no pipefail) restores after the failed fetch, the real script refuses: the test discriminates"
else
	echo "$out"
	fail "the mutant did not survive: the pipefail test proves nothing (exit $rc)"
fi
no_scratch_db mutant

# ---------------------------------------------------------------- 5. attachments

step "attachment fixtures: four objects (one empty, one with a space in its name), a good list and a signed list with a wrong plain sha256"
srcfiles="$scratch/srcfiles"
org=0198aaaa-0000-7000-8000-00000000000a
mkdir -p "$srcfiles/org/$org/2026/u1" "$srcfiles/org/$org/2026/u2" "$srcfiles/org/$org/2026/u3" "$srcfiles/org/$org/2026/u4"
head -c 3000 /dev/urandom >"$srcfiles/org/$org/2026/u1/report.pdf"
head -c 300000 /dev/urandom >"$srcfiles/org/$org/2026/u2/big.bin"
printf 'plain text with a space in its name\n' >"$srcfiles/org/$org/2026/u3/my notes.txt"
: >"$srcfiles/org/$org/2026/u4/empty.txt"
chmod -R a+rX "$srcfiles"
reset_remote
hostile=()
{
	read -r fstem
	for _ in 1 2 3 4 5 6 7 8 9; do
		read -r h
		hostile+=("$h")
	done
	read -r badstem
} < <(docker run --rm --user "$(id -u):$(id -g)" -v "$scratch:/s" "$img" /bin/restorefake genfiles \
	--src /s/srcfiles --remote /s/remote --keys /s/keys --install-id "$inst" --time 20261027T040000Z --hostile --bad-sha-time 20261026T040000Z)
[[ "$fstem" == launchpad-files-$inst-list-20261027T040000Z-* && "$badstem" == launchpad-files-$inst-list-20261026T040000Z-* ]] || fail "genfiles printed: $fstem / $badstem"
chmod -R a+rX "$remote"
cp -Rp "$remote" "$scratch/remote.files.orig"
reset_files() {
	rm -rf "${remote:?}"/*
	cp -Rp "$scratch/remote.files.orig/." "$remote/"
	chmod -R a+rX "$remote"
}
fobj_of() { # NUMBER: the n-th object file name (sorted)
	find "$remote/launchpad-files-$inst-objects" -type f -exec basename {} \; | sort | sed -n "${1}p"
}
# files_ok OUTDIR: every source file is in OUTDIR with the same bytes and mode 0600.
files_ok() {
	local o="$1" f rel
	while IFS= read -r -d '' f; do
		rel="${f#"$srcfiles"/}"
		cmp -s "$f" "$o/$rel" || fail "the restored file differs: object #$(grep -c . <<<"${rel//[^/]/}")"
		[[ "$(python3 -I -c 'import os,sys; print("%o" % (os.stat(sys.argv[1]).st_mode & 0o777))' "$o/$rel")" == 600 ]] || fail "a restored file is not mode 0600"
	done < <(find "$srcfiles" -type f -print0)
	[[ "$(find "$o" -type f | wc -l | tr -d ' ')" == 4 ]] || fail "the restored folder does not hold exactly 4 files"
}
# files_refused NAME TEXT: the last rr failed with TEXT, made no database, and left no plain file under OUT.
files_refused() {
	((rc != 0)) || {
		echo "$out"
		fail "$1: exit 0 (it must fail)"
	}
	grep -q -- "$2" <<<"$out" || {
		echo "$out"
		fail "$1: no '$2' in the output"
	}
	grep -q 'CREATE DATABASE' "$SHIM_LOG" && fail "$1: a database was created"
	grep -q 'pg_restore' "$SHIM_LOG" && fail "$1: pg_restore ran"
	grep -q 'files OK' <<<"$out" && fail "$1: printed files OK"
	grep -q -e 'report\.pdf' -e 'my notes' -e 'big\.bin' <<<"$out" && fail "$1: an object key reached the output"
	no_scratch_db "$1"
	echo "ok: $1 -> exit $rc, '$2'"
}
no_plain_left() { # DIR
	[[ ! -e "$1" || -z "$(find "$1" -type f)" ]] || fail "plain files were left under $1"
}

step "restore the attachments: the latest list, four objects, every byte and sha256, mode 0600, no database"
outA="$scratch/outA"
rr "$here/restore.sh" "${base[@]}" --files --files-out "$outA" latest
echo "$out" | tail -n 6
((rc == 0)) || fail "the attachment restore failed (exit $rc)"
grep -q "signature and sha256 of the list OK (install id $inst, $fstem)" <<<"$out" || fail "no signature line for the list"
grep -q 'it names 4 objects' <<<"$out" || fail "no object count"
grep -q 'files OK - 4 object(s), ' <<<"$out" || fail "no files OK"
grep -q "no database record exists for an attachment list" <<<"$out" || fail "no note about the missing database record"
grep -q 'delete the --remote-config file now' <<<"$out" || fail "no reminder to delete the config"
files_ok "$outA"
grep -q 'CREATE DATABASE' "$SHIM_LOG" && fail "an attachment restore made a database"
grep -q -e 'report\.pdf' -e 'my notes' -e 'big\.bin' <<<"$out" && fail "an object key reached the output"
no_scratch_db files
for v in unpack decrypt fetch; do
	grep -q -- "--user $(id -u):$(id -g)" < <(grep " offsite $v " "$SHIM_LOG") || fail "$v does not run as the caller"
	grep -q -- '--read-only' < <(grep " offsite $v " "$SHIM_LOG") || fail "$v is not read-only"
	grep -q -- '--cap-drop ALL' < <(grep " offsite $v " "$SHIM_LOG") || fail "$v keeps capabilities"
done
grep -q -- '--file-object' < <(grep ' offsite fetch ' "$SHIM_LOG") || fail "no object was fetched"
grep -q -- '--network none' < <(grep ' offsite decrypt ' "$SHIM_LOG") || fail "decrypt is not --network none"
n_dec="$(grep -c ' offsite decrypt ' "$SHIM_LOG" || true)"
((n_dec == 2 + 4)) || fail "expected 2 decrypts of the list and 4 of objects, saw $n_dec"
echo "ok: 4 objects restored with the same bytes and mode 0600; every throwaway command is hardened"

step "one object only (--files-key), into an existing empty folder"
outB="$scratch/outB"
mkdir "$outB"
rr "$here/restore.sh" "${base[@]}" --files --files-out "$outB" --files-key "org/$org/2026/u1/report.pdf" "$fstem"
((rc == 0)) || {
	echo "$out"
	fail "--files-key failed"
}
grep -q 'files OK - 1 object(s), ' <<<"$out" || fail "--files-key did not restore exactly one object"
cmp -s "$srcfiles/org/$org/2026/u1/report.pdf" "$outB/org/$org/2026/u1/report.pdf" || fail "the one object differs"
[[ "$(find "$outB" -type f | wc -l | tr -d ' ')" == 1 ]] || fail "--files-key restored more than one file"
rr "$here/restore.sh" "${base[@]}" --files --files-out "$scratch/outB2" --files-key "org/$org/2026/u1/none.pdf" latest
files_refused "unknown --files-key" 'not safe to restore, or does not match'
no_plain_left "$scratch/outB2"

step "--verify-only checks the list and the key and writes nothing"
rr "$here/restore.sh" "${base[@]}" --files --verify-only "$fstem"
((rc == 0)) || {
	echo "$out"
	fail "files verify-only failed"
}
grep -q 'verify-only OK' <<<"$out" || fail "no verify-only OK"
grep -q ' offsite fetch .*--file-object' "$SHIM_LOG" && fail "verify-only fetched an object"
no_scratch_db files-verify-only

step "--remote NAME for the attachments (the stack path: fetch --files and --file-object go to the offsite container)"
stack_env="$scratch/stack.env"
sed -e "s/^DEPLOY_PROJECT=.*/DEPLOY_PROJECT='devops-restore-remote-$sfx'/" "$here/.env.example" >"$stack_env"
chmod 600 "$stack_env"
: >"$SHIM_LOG"
: >"$SHIM_ENVLOG"
rc=0
out="$(PATH="$shim:$PATH" TMPDIR="$work" DEPLOY_ENV_FILE="$stack_env" "$here/restore.sh" --remote "Offsite NAS" \
	--verify-key "$keys/signing.pub" --identity "$keys/age.key" --work-dir "$work" --no-host-age \
	--files --files-out "$scratch/outS" --files-key "org/$org/2026/u3/my notes.txt" latest 2>&1 </dev/null)" || rc=$?
printf '%s\n' "$out" >>"$allout"
cat "$SHIM_LOG" >>"$alllog"
cat "$SHIM_ENVLOG" >>"$allenv"
((rc == 0)) || {
	echo "$out"
	fail "--remote NAME for files failed"
}
grep -q -- 'OFFSITE-EXEC /bin/server offsite fetch --destination Offsite NAS --files --stem latest' "$SHIM_LOG" || fail "the offsite exec for the list has the wrong arguments"
grep -q -- 'OFFSITE-EXEC /bin/server offsite fetch --destination Offsite NAS --file-object launchpad-files-' "$SHIM_LOG" || fail "the offsite exec for the object has the wrong arguments"
cmp -s "$srcfiles/org/$org/2026/u3/my notes.txt" "$scratch/outS/org/$org/2026/u3/my notes.txt" || fail "the stack path restored the wrong bytes"
no_scratch_db files-stack
rm -f "$stack_env"
echo "ok: --remote NAME restored one attachment through the offsite container"

step "refusals before a single byte is written under --files-out"
mkdir -p "$scratch/outFull"
echo precious >"$scratch/outFull/keep.txt"
rr "$here/restore.sh" "${base[@]}" --files --files-out "$scratch/outFull" latest
files_refused "a folder that is not empty" 'does not exist or is empty'
[[ "$(cat "$scratch/outFull/keep.txt")" == precious && "$(find "$scratch/outFull" -type f | wc -l | tr -d ' ')" == 1 ]] || fail "the non-empty folder was changed"
grep -q ' offsite fetch ' "$SHIM_LOG" && fail "a non-empty folder still fetched"

reset_files
python3 -I - "$remote/launchpad-files-$inst/$fstem.json" <<'PY'
import sys, json, base64
p = sys.argv[1]
e = json.load(open(p))
sig = bytearray(base64.b64decode(e["signature"]))
sig[0] ^= 1
e["signature"] = base64.b64encode(bytes(sig)).decode()
json.dump(e, open(p, "w"))
PY
rr "$here/restore.sh" "${base[@]}" --files --files-out "$scratch/outBadSig" "$fstem"
files_refused "a bad signature on the list" 'signature or hash check FAILED'
grep -q 'offsite decrypt' "$SHIM_LOG" && fail "a bad signature still decrypted"
no_plain_left "$scratch/outBadSig"

reset_files
rr "$here/restore.sh" "${base[@]}" --verify-key "$keys/signing-wrong.pub" --files --files-out "$scratch/outWrongKey" "$fstem"
files_refused "the wrong verify key" 'signature or hash check FAILED'

reset_files
python3 -I - "$remote/launchpad-files-$inst/$fstem.list.age" <<'PY'
import sys
f = open(sys.argv[1], "r+b")
f.seek(200)
b = f.read(1)
f.seek(200)
f.write(bytes([b[0] ^ 1]))
f.close()
PY
rr "$here/restore.sh" "${base[@]}" --files --files-out "$scratch/outBadList" "$fstem"
files_refused "a changed list" 'signature or hash check FAILED'
grep -q 'offsite decrypt' "$SHIM_LOG" && fail "a changed list still decrypted"

reset_files
rr "$here/restore.sh" --container "$c" --image "$img" --verify-key "$keys/signing.pub" --identity "$keys/age-wrong.key" --install-id "$inst" \
	--remote-config "$scratch/net.json" --fetch-network "$net" --allow-cidr "$net_subnet" --work-dir "$work" --no-host-age \
	--files --files-out "$scratch/outWrongAge" latest
files_refused "a wrong age key" 'private key does not decrypt this list'
[[ ! -e "$scratch/outWrongAge" ]] || fail "a wrong age key still made the output folder"
grep -q -- '--file-object' "$SHIM_LOG" && fail "a wrong age key still fetched an object"

rr "$here/restore.sh" "${base[@]}" --files --files-out "$scratch/outX" "launchpad-$inst-20261027T031500Z-aaaa"
files_refused "a dump stem with --files" 'or a STEM'
rr "$here/restore.sh" "${base[@]}" --files latest
files_refused "no --files-out" 'needs --files-out DIR'
rr "$here/restore.sh" "${base[@]}" --files-out "$scratch/outX" latest
files_refused "--files-out without --files" 'go with --files'
rr "$here/restore.sh" "${base[@]}" --files --files-out "$scratch/outX" --files-key "../etc/passwd" latest
files_refused "a --files-key that is not a silo key" 'silo key like'
rr "$here/restore.sh" "${base[@]}" --files --files-out "$scratch/outX" "launchpad-files-$other_inst-list-20261027T040000Z-aaaa"
files_refused "a list stem of another install" 'another install id'

step "nine hostile SIGNED lists (.. / leading slash / empty part / backslash / newline / duplicate / 256 byte part / foreign object names): each is refused with nothing written"
hnames=(dotdot leading-slash empty-part backslash newline duplicate long-part wrong-hash other-install)
for i in 0 1 2 3 4 5 6 7 8; do
	reset_files
	rr "$here/restore.sh" "${base[@]}" --files --files-out "$scratch/outHostile$i" "${hostile[$i]}"
	files_refused "hostile list: ${hnames[$i]}" 'not safe to restore'
	no_plain_left "$scratch/outHostile$i"
	grep -q -- '--file-object' "$SHIM_LOG" && fail "hostile list ${hnames[$i]} still fetched an object"
done

step "a damaged object in the middle: the earlier objects were written, then the whole folder is emptied again"
reset_files
last="$(fobj_of 4)"
python3 -I - "$remote/launchpad-files-$inst-objects/$last" <<'PY'
import sys
f = open(sys.argv[1], "r+b")
f.seek(100)
b = f.read(1)
f.seek(100)
f.write(bytes([b[0] ^ 1]))
f.close()
PY
rr "$here/restore.sh" "${base[@]}" --files --files-out "$scratch/outDamaged" latest
files_refused "a damaged object" 'does not match the signed list'
grep -q 'the folder was emptied again' <<<"$out" || fail "no emptied-again sentence"
no_plain_left "$scratch/outDamaged"

step "an object swapped for another valid object: the stored copy does not match the signed list"
reset_files
o1="$(fobj_of 1)"
o2="$(fobj_of 2)"
cp -p "$remote/launchpad-files-$inst-objects/$o2" "$remote/launchpad-files-$inst-objects/$o1"
rr "$here/restore.sh" "${base[@]}" --files --files-out "$scratch/outSwapped" latest
files_refused "a swapped object" 'does not match the signed list'
no_plain_left "$scratch/outSwapped"

step "the signed list names a wrong plain sha256: the restored bytes are refused, and a mutant without that check is caught"
reset_files
rr "$here/restore.sh" "${base[@]}" --files --files-out "$scratch/outBadSha" "$badstem"
files_refused "a wrong sha256 in the list" 'do not match the sha256 in the signed list'
no_plain_left "$scratch/outBadSha"
mkdir -p "$scratch/mutant2"
cp "$here/lib.sh" "$scratch/mutant2/"
# mutate_restore OLD NEW: restore.sh with one exact line fragment replaced (the fragment must exist).
mutate_restore() {
	python3 -I - "$here/restore.sh" "$scratch/mutant2/restore.sh" "$1" "$2" <<'PY'
import sys
src, dst, old, new = sys.argv[1:5]
t = open(src).read()
if t.count(old) != 1:
    sys.exit("mutation target not found exactly once")
open(dst, "w").write(t.replace(old, new))
PY
	chmod 755 "$scratch/mutant2/restore.sh"
}
# shellcheck disable=SC2016  # the fragments are literal text of restore.sh
mutate_restore '== "$psha" ]] ||' '== "$psha" || true ]] ||'
rr "$scratch/mutant2/restore.sh" "${base[@]}" --files --files-out "$scratch/outBadSha2" "$badstem"
if ((rc == 0)) && grep -q 'files OK' <<<"$out"; then
	echo "ok: the mutant without the plain sha256 check restores the wrong list, the real script refuses"
else
	echo "$out"
	fail "the sha mutant did not survive: the plain sha256 test proves nothing"
fi
rm -rf "$scratch/outBadSha2"
reset_files
cp -p "$remote/launchpad-files-$inst-objects/$(fobj_of 2)" "$remote/launchpad-files-$inst-objects/$(fobj_of 1)"
# shellcheck disable=SC2016  # the fragment is literal text of restore.sh
mutate_restore '[[ "$(wc -c <"$wd/obj/object" | tr -d '"' '"')" == "$csize" && "$(sha_of "$wd/obj/object")" == "$csha" ]] ||' 'true ||'
rr "$scratch/mutant2/restore.sh" "${base[@]}" --files --files-out "$scratch/outSwapped2" latest
if ((rc != 0)) && grep -q 'do not match the sha256 in the signed list' <<<"$out"; then
	echo "ok: with the stored-copy check removed the plain sha256 check still stops a swapped object (defence in depth)"
else
	echo "$out"
	fail "a swapped object with the stored-copy check removed was not stopped by the plain check"
fi
rm -rf "$scratch/outSwapped2"

step "the hostile-list validator alone: with its path-part check removed, the .. list is no longer refused as unsafe"
reset_files
# shellcheck disable=SC2016  # the fragment is literal text of restore.sh
mutate_restore 'if any(c in ("", ".", "..") or len(c.encode()) > 255 for c in comps):' 'if False:'
rr "$scratch/mutant2/restore.sh" "${base[@]}" --files --files-out "$scratch/outHostileMut" "${hostile[0]}"
if grep -q 'not safe to restore' <<<"$out"; then
	echo "$out"
	fail "the validator mutant was still refused by the validator: the hostile section does not test the path-part check"
fi
((rc != 0)) || fail "the .. list restored with the validator mutant"
rm -rf "$scratch/outHostileMut"
echo "ok: without the path-part check the .. list gets past the validator (it only fails later), so the hostile section tests the validator"

step "the folder is filled while the list is fetched: the second emptiness check refuses, and a mutant without it is caught"
reset_files
mkdir "$scratch/outLate"
printf '%s' "$scratch/outLate" >"$shim/fill_out"
rr "$here/restore.sh" "${base[@]}" --files --files-out "$scratch/outLate" latest
files_refused "a folder filled during the fetch" 'no longer a folder that is empty'
[[ "$(find "$scratch/outLate" -type f | wc -l | tr -d ' ')" == 1 && -e "$scratch/outLate/late.txt" ]] || fail "the late file was changed or objects were written"
grep -q -- '--file-object' "$SHIM_LOG" && fail "a filled folder still fetched an object"
# shellcheck disable=SC2016  # the fragment is literal text of restore.sh
mutate_restore '[[ -d "$out_abs" && ! -L "$out_abs" && -z "$(ls -A "$out_abs")" ]] || die "--files-out is no longer' ': || die "--files-out is no longer'
rm -f "$scratch/outLate/late.txt"
rr "$scratch/mutant2/restore.sh" "${base[@]}" --files --files-out "$scratch/outLate" latest
if ((rc == 0)) && grep -q 'files OK' <<<"$out"; then
	echo "ok: the mutant without the second check writes into a folder that was filled meanwhile, the real script refuses"
else
	echo "$out"
	fail "the second-check mutant did not survive: the test proves nothing"
fi
rm -f "$shim/fill_out"
rm -rf "$scratch/outLate"

step "a failed fetch fails the attachment restore (pipefail), and a mutant without pipefail proves the test sees it"
reset_files
touch "$shim/fetch_fail"
rr "$here/restore.sh" "${base[@]}" --files --files-out "$scratch/outFetchFail" latest
files_refused "a fetch that exits 1 after a whole frame" 'fetch or the unpack FAILED'
mkdir -p "$scratch/mutant3"
cp "$here/lib.sh" "$scratch/mutant3/"
sed -e 's/^set -euo pipefail$/set -eu/' -e '/^[[:space:]]*set -o pipefail$/d' "$here/restore.sh" >"$scratch/mutant3/restore.sh"
chmod 755 "$scratch/mutant3/restore.sh"
rr "$scratch/mutant3/restore.sh" "${base[@]}" --files --files-out "$scratch/outFetchFail2" latest
rm -f "$shim/fetch_fail"
if ((rc == 0)) && grep -q 'files OK' <<<"$out"; then
	echo "ok: the mutant (no pipefail) restores after the failed fetches, the real script refuses"
else
	echo "$out"
	fail "the pipefail mutant did not survive for the attachment restore"
fi
rm -rf "$scratch/outFetchFail2"
no_scratch_db files-end


# ---------------------------------------------------------------- 6. follow-ups

# mut4 FROM TO: a copy of restore.sh with ONE fragment changed (it must occur exactly once).
mkdir -p "$scratch/mutant4"
cp "$here/lib.sh" "$scratch/mutant4/"
mut4() {
	python3 -I - "$here/restore.sh" "$scratch/mutant4/restore.sh" "$1" "$2" <<'PY'
import sys
src, dst, old, new = sys.argv[1:5]
t = open(src).read()
if t.count(old) != 1:
    sys.exit("mutation target not found exactly once")
open(dst, "w").write(t.replace(old, new))
PY
	chmod 755 "$scratch/mutant4/restore.sh"
}
# A pseudo-terminal runner: STEPS is "expect|send" lines; each send waits up to 30 s for its expect text
# and types the text plus a newline. A third field "noecho" also waits (5 s at most) until the terminal has echo off. No prompt in time: nothing is typed and the exit code is 98. Output goes to OUTFILE; the exit code is printed.
cat >"$scratch/ptyrun.py" <<'PY'
import os, pty, sys, select, time, termios
outfile, stepsfile = sys.argv[1], sys.argv[2]
cmd = sys.argv[4:]
steps = [(l.rstrip("\n").split("|") + [""])[:3] for l in open(stepsfile) if l.strip()]
def echo_on():
    try:
        return bool(termios.tcgetattr(fd)[3] & termios.ECHO)
    except Exception:
        return False
echo_since = None
for st in steps:
    if st[2] not in ("", "noecho"):
        sys.stderr.write("ptyrun: the third field of a step must be empty or noecho\n")
        print(97)
        sys.exit(0)
pid, fd = pty.fork()
if pid == 0:
    os.execvp(cmd[0], cmd)
buf = b""
idx = 0
mark = 0
t_step = time.time()
deadline = time.time() + 300
while True:
    r, _, _ = select.select([fd], [], [], 0.2)
    if r:
        try:
            d = os.read(fd, 4096)
        except OSError:
            d = b""
        if not d:
            break
        buf += d
    if idx < len(steps):
        if steps[idx][0].encode() in buf[mark:] and steps[idx][2] == "noecho" and echo_on():
            # bash prints the prompt BEFORE it turns echo off. Typing in that gap echoes the key.
            # Wait until the terminal has echo off. A script that never turns it off (the -s mutant) is
            # typed to after 5 s, so its echo is still seen and the check keeps its teeth.
            if echo_since is None:
                echo_since = time.time()
            if time.time() - echo_since < 5:
                continue
        if steps[idx][0].encode() in buf[mark:]:
            echo_since = None
            os.write(fd, (steps[idx][1] + "\n").encode())
            idx += 1
            mark = len(buf)
            t_step = time.time()
        elif time.time() - t_step > 30:
            os.kill(pid, 9)
            os.waitpid(pid, 0)
            open(outfile, "wb").write(buf)
            sys.stderr.write("ptyrun: the prompt '%s' did not show in 30 s; nothing was typed\n" % steps[idx][0])
            print(98)
            sys.exit(0)
    if time.time() > deadline:
        os.kill(pid, 9)
        break
_, st = os.waitpid(pid, 0)
open(outfile, "wb").write(buf)
print(os.waitstatus_to_exitcode(st))
PY
pty_out="$scratch/pty.out" pty_steps="$scratch/pty.steps"
# rr_pty STEPS... -- ARGS: restore.sh (or $PTY_SCRIPT) on a pseudo-terminal under the shim; sets $out, $rc.
rr_pty() {
	local script="${PTY_SCRIPT:-$here/restore.sh}"
	: >"$SHIM_LOG"
	: >"$SHIM_ENVLOG"
	rc=0
	rc="$(PATH="$shim:$PATH" TMPDIR="$work" DEPLOY_ENV_FILE="$scratch/none.env" python3 -I "$scratch/ptyrun.py" "$pty_out" "$pty_steps" -- "$script" "$@")" || rc=99
	out="$(tr -d '\r' <"$pty_out")"
	printf '%s\n' "$out" >>"$allout"
	cat "$SHIM_LOG" >>"$alllog"
	cat "$SHIM_ENVLOG" >>"$allenv"
}
reset_remote
ptybase=(--container "$c" --image "$img" --verify-key "$keys/signing.pub" --install-id "$inst"
	--remote-config "$scratch/net.json" --fetch-network "$net" --allow-cidr "$net_subnet" --work-dir "$work" --no-host-age)

step "item 1: the key is not echoed on a terminal, with --identity - and with no --identity (a pseudo-terminal)"
printf '%s\n' "hidden):|PLANTEDnotAKeyAtAll123|noecho" >"$pty_steps"
PTY_SCRIPT="" rr_pty "${ptybase[@]}" --identity - latest
grep -q 'age private key (AGE-SECRET-KEY-1' <<<"$out" || fail "no hidden prompt on a terminal with --identity -"
grep -q 'not an age private key' <<<"$out" || fail "the bad key was not refused"
grep -q PLANTEDnotAKeyAtAll <<<"$out" && fail "--identity - on a terminal echoed the key"
printf '%s\n' "hidden):|$agekey|noecho" >"$pty_steps"
rr_pty "${ptybase[@]}" --verify-only latest
((rc == 0)) || { echo "$out"; fail "the hidden prompt with a real key failed (exit $rc)"; }
grep -q 'verify-only OK' <<<"$out" || fail "the hidden prompt: no verify-only OK"
grep -q -F -- "$agekey" <<<"$out" && fail "the hidden prompt echoed the key"
# Mutant A: the prompt stays but echo is on. Mutant B: a terminal with --identity - is read like a pipe
# (no prompt at all: the runner types nothing and reports 98).
# shellcheck disable=SC2016  # the fragment is literal text of restore.sh
mut4 'IFS= read -r -s -p "age private key' 'IFS= read -r -p "age private key'
printf '%s\n' "hidden):|PLANTEDnotAKeyAtAll123|noecho" >"$pty_steps"
PTY_SCRIPT="$scratch/mutant4/restore.sh" rr_pty "${ptybase[@]}" --identity - latest
if grep -q PLANTEDnotAKeyAtAll <<<"$out"; then
	echo "ok: hidden on a terminal; the mutant without -s echoes the key, so the test discriminates"
else
	echo "$out"
	fail "the echo mutant was not caught: the terminal test proves nothing"
fi
# shellcheck disable=SC2016  # the fragment is literal text of restore.sh
mut4 'if [[ "$identity" == - && ! -t 0 ]]; then' 'if [[ "$identity" == - ]]; then'
PTY_SCRIPT="$scratch/mutant4/restore.sh" rr_pty "${ptybase[@]}" --identity - latest
if [[ "$rc" == 98 ]] && ! grep -q PLANTEDnotAKeyAtAll <<<"$out"; then
	echo "ok: the mutant that reads a terminal like a pipe shows no prompt; the runner typed nothing and failed with 98"
else
	echo "$out"
	fail "the pipe-branch mutant was not caught (exit $rc)"
fi
echo "note: a pseudo-terminal is not a person at a keyboard; the hidden prompt is verified on a pty only"

step "item 3: --trust-remote needs a confirmation BEFORE any fetch (typed on a terminal, or --yes-trust-remote ID)"
trustbase=(--container "$c" --image "$img" --trust-remote --identity "$keys/age.key" --install-id "$inst"
	--remote-config "$scratch/net.json" --fetch-network "$net" --allow-cidr "$net_subnet" --work-dir "$work" --no-host-age)
no_fetch() { grep -q ' offsite fetch ' "$SHIM_LOG" && fail "$1: a fetch ran before the confirmation"; return 0; }
rr "$here/restore.sh" "${trustbase[@]}" latest
refused_early "trust-remote with no terminal and no flag" 'needs a confirmation before anything is fetched'
no_fetch "no flag"
grep -q 'arbitrary SQL as the bootstrap superuser' <<<"$out" || fail "the warning was not printed before the refusal"
rr "$here/restore.sh" "${trustbase[@]}" --yes-trust-remote "$other_inst" latest
refused_early "trust-remote with the wrong install id" 'does not match the install id'
no_fetch "wrong id"
rr "$here/restore.sh" "${base[@]}" --yes-trust-remote "$inst" latest
refused_early "--yes-trust-remote without --trust-remote" 'goes with --trust-remote'
printf '%s\n' "type the install id|nope" >"$pty_steps"
rr_pty "${trustbase[@]}" --verify-only latest
refused_early "trust-remote typed wrongly on a terminal" 'not confirmed'
no_fetch "typed wrongly"
printf '%s\n' "type the install id|$inst" >"$pty_steps"
rr_pty "${trustbase[@]}" --verify-only latest
((rc == 0)) || { echo "$out"; fail "the typed install id did not confirm (exit $rc)"; }
grep -q 'is signed' <<<"$out" && fail "a trusted run claimed a signed copy"
grep -q 'verify-only: NOT verified' <<<"$out" || fail "a trusted verify-only does not say it verified nothing"
grep -q 'UNVERIFIED' <<<"$out" || fail "the typed confirmation run is not marked unverified"
# shellcheck disable=SC2016  # the fragment is literal text of restore.sh
mut4 'die "--trust-remote needs a confirmation before anything is fetched: type' 'true "--trust-remote needs a confirmation before anything is fetched: type'
rr "$scratch/mutant4/restore.sh" "${trustbase[@]}" --verify-only latest
if ((rc == 0)); then
	echo "ok: without the confirmation check the same call fetches and verifies nothing signed; the real script refuses"
else
	echo "$out"
	fail "the confirmation mutant did not survive: the test proves nothing"
fi
# shellcheck disable=SC2016  # the fragment is literal text of restore.sh
mut4 '[[ "$typed" == "$install_id" ]] || die "not confirmed; nothing was fetched"' 'true'
printf '%s\n' "type the install id|nope" >"$pty_steps"
PTY_SCRIPT="$scratch/mutant4/restore.sh" rr_pty "${trustbase[@]}" --verify-only latest
((rc == 0)) || { echo "$out"; fail "the typed-check mutant was still refused: the test proves nothing"; }
echo "ok: --trust-remote asks first; the typed and the flag forms work; two mutants are caught"

step "item 2: a rollback warning when the database holds a newer succeeded backup (latest too)"
rr "$here/restore.sh" "${base[@]}" --verify-only latest
((rc == 0)) || fail "verify-only latest failed"
grep -q ROLLBACK <<<"$out" && fail "the newest pair warned about a rollback"
docker exec "$c" psql -X -q -c "INSERT INTO backup_runs (destination_name, trigger, status, finished_at, bytes, sha256, remote_name)
  VALUES ('Offsite NAS', 'manual', 'failed', now(), 1, repeat('1', 64), 'launchpad-$inst-20261028T020000Z-ffff.dump.age')"
rr "$here/restore.sh" "${base[@]}" --verify-only latest
grep -q ROLLBACK <<<"$out" && fail "a FAILED newer run warned about a rollback"
docker exec "$c" psql -X -q -c "UPDATE backup_runs SET status = 'succeeded' WHERE remote_name = 'launchpad-$inst-20261028T020000Z-ffff.dump.age'"
rr "$here/restore.sh" "${base[@]}" --verify-only latest
((rc == 0)) || fail "a rollback warning must not stop the restore"
grep -q 'ROLLBACK: the database records a succeeded backup NEWER than this copy (newest: 2026-10-28 02:00:00 UTC' <<<"$out" || {
	echo "$out"
	fail "latest, with a newer succeeded run recorded, did not warn about a rollback"
}
# shellcheck disable=SC2016  # the fragment is literal text of restore.sh
mut4 '[[ -n "$newest" && "$newest" > "$ptime" ]]' '[[ -n "$newest" && "$newest" < "$ptime" ]]'
rr "$scratch/mutant4/restore.sh" "${base[@]}" --verify-only latest
if grep -q ROLLBACK <<<"$out"; then
	echo "$out"
	fail "the comparison mutant still warned: the rollback test proves nothing"
fi
docker exec "$c" psql -X -q -c "DELETE FROM backup_runs WHERE remote_name LIKE '%ffff.dump.age'"
echo "ok: a succeeded newer run warns (and does not stop); a failed one does not; the old stem warned above; the comparison mutant is caught"

step "item 6: a database that cannot be asked is not the same as no record"
docker exec "$c" psql -X -q -c "ALTER TABLE backup_runs RENAME TO backup_runs_away"
rr "$here/restore.sh" "${base[@]}" --verify-only latest
docker exec "$c" psql -X -q -c "ALTER TABLE backup_runs_away RENAME TO backup_runs"
((rc == 0)) || { echo "$out"; fail "an unreadable record must not stop the verify"; }
grep -q 'could not be asked about this backup' <<<"$out" || fail "no sentence for an unreadable database record"
grep -q 'no record of this object in the database' <<<"$out" && fail "an unreadable record was reported as no record"
echo "ok: a missing backup_runs table says the database could not be asked"

step "item 4: no room: the database volume (plain dump size measured by the decrypt pass) and the files folder"
touch "$shim/tiny_pgdf"
rr "$here/restore.sh" "${base[@]}" latest
rm -f "$shim/tiny_pgdf"
# The plain size is measured by the decrypt pass, so decrypt HAS run here; no database may exist.
if ((rc == 0)) || ! grep -q 'not enough free space in the database volume' <<<"$out"; then
	echo "$out"
	fail "no refusal for a full database volume"
fi
grep -q 'CREATE DATABASE' "$SHIM_LOG" && fail "a database was made although the volume had no room"
no_scratch_db "no room in the database volume"
# shellcheck disable=SC2016  # the fragment is literal text of restore.sh
mut4 'if ((kb * 1024 < plain_bytes)); then' 'if false; then'
touch "$shim/tiny_pgdf"
rr "$scratch/mutant4/restore.sh" "${base[@]}" latest
rm -f "$shim/tiny_pgdf"
((rc == 0)) || { echo "$out"; fail "the space mutant was still refused: the test proves nothing"; }
reset_files
touch "$shim/tiny_hostdf"
rr "$here/restore.sh" "${base[@]}" --files --files-out "$scratch/outNoRoom" latest
rm -f "$shim/tiny_hostdf"
files_refused "no room for the restored files" 'not enough free space for the restored files'
[[ ! -e "$scratch/outNoRoom" ]] || fail "a folder was made although there was no room"
grep -q -- '--file-object' "$SHIM_LOG" && fail "an object was fetched although there was no room"
# shellcheck disable=SC2016  # the fragment is literal text of restore.sh
mut4 '((have >= $2)) || die' 'true || die'
touch "$shim/tiny_hostdf"
rr "$scratch/mutant4/restore.sh" "${base[@]}" --files --files-out "$scratch/outNoRoom2" latest
rm -f "$shim/tiny_hostdf"
if ((rc == 0)) && grep -q 'files OK' <<<"$out"; then
	echo "ok: both space checks refuse before anything is written; each mutant gets through, so the tests discriminate"
else
	echo "$out"
	fail "the host-space mutant was refused: the test proves nothing"
fi
rm -rf "$scratch/outNoRoom2"

step "item 5: the config file is mounted from its checked, resolved path, and a swap after the check is refused"
mkdir -p "$scratch/realdir"
cp -p "$scratch/net.json" "$scratch/realdir/via.json"
ln -s "$scratch/realdir" "$scratch/linkdir"
physdir="$(cd "$scratch/realdir" && pwd -P)"
viabase=()
for a in "${base[@]}"; do
	if [[ "$a" == "$scratch/net.json" ]]; then viabase+=("$scratch/linkdir/via.json"); else viabase+=("$a"); fi
done
rr "$here/restore.sh" "${viabase[@]}" --verify-only latest
((rc == 0)) || { echo "$out"; fail "a config under a linked folder failed"; }
grep -q -F -- "-v $physdir/via.json:/config.json:ro" < <(grep ' offsite fetch ' "$SHIM_LOG") || fail "the fetch did not mount the resolved path"
printf '%s' "$scratch/net.json" >"$shim/swap_cfg"
rr "$here/restore.sh" "${base[@]}" --verify-only latest
rm -f "$shim/swap_cfg"
refused_early "a config swapped after its check" 'changed after it was checked'
grep -q ' offsite fetch ' "$SHIM_LOG" && fail "the swapped config was used by a fetch"
# shellcheck disable=SC2016  # the fragment is literal text of restore.sh
mut4 '[[ -f "$cfg" && ! -L "$cfg" && "$(file_sig "$cfg")" == "$cfg_sig" ]] ||' 'true ||'
printf '%s' "$scratch/net.json" >"$shim/swap_cfg"
rr "$scratch/mutant4/restore.sh" "${base[@]}" --verify-only latest
rm -f "$shim/swap_cfg"
((rc == 0)) || { echo "$out"; fail "the recheck mutant was still refused: the swap test proves nothing"; }
echo "ok: resolved path mounted; the swap is refused; without the recheck the swapped file is used"
no_scratch_db followups

step "items 3 and 6 of the review: the config swap is refused on the --files path; one file system sums both needs"
reset_files
printf '%s' "$scratch/net.json" >"$shim/swap_cfg"
rr "$here/restore.sh" "${base[@]}" --files --verify-only latest
rm -f "$shim/swap_cfg"
files_refused "a config swapped after its check (--files)" 'changed after it was checked'
grep -q ' offsite fetch ' "$SHIM_LOG" && fail "the swapped config was used by a files fetch"
# shellcheck disable=SC2016  # the fragment is literal text of restore.sh
mut4 '	cfg_recheck
	# pipefail makes the pipeline fail when EITHER side does; keep this line.
	set -o pipefail
	"${fc[@]}"' '	# pipefail makes the pipeline fail when EITHER side does; keep this line.
	set -o pipefail
	"${fc[@]}"'
printf '%s' "$scratch/net.json" >"$shim/swap_cfg"
rr "$scratch/mutant4/restore.sh" "${base[@]}" --files --verify-only latest
rm -f "$shim/swap_cfg"
((rc == 0)) || { echo "$out"; fail "the files recheck mutant was still refused: the test proves nothing"; }
echo "ok: the --files path refuses a swapped config; without its recheck the swapped file is used"
reset_files
# 700 KB free: enough for the files (about 300 KB) and for one object (about 600 KB) each, not for both on one file system.
printf '%s' 700 >"$shim/fixed_hostdf"
rr "$here/restore.sh" "${base[@]}" --files --files-out "$scratch/outSum" latest
files_refused "files and one staged object do not fit together" 'not enough free space for the restored files'
[[ ! -e "$scratch/outSum" ]] || fail "a folder was made although the two needs did not fit"
# shellcheck disable=SC2016  # the fragment is literal text of restore.sh
mut4 '"$((need_out + need_wd))"' '"$need_out"'
rr "$scratch/mutant4/restore.sh" "${base[@]}" --files --files-out "$scratch/outSum2" latest
rm -f "$shim/fixed_hostdf"
if ((rc == 0)) && grep -q 'files OK' <<<"$out"; then
	echo "ok: the two needs are summed on one file system; the mutant that checks only the files passes the same call"
else
	echo "$out"
	fail "the sum mutant was refused: the test proves nothing"
fi
rm -rf "$scratch/outSum2"

step "the host age branch (item 7): a real age for THIS host, built from the same module; no decrypt container runs"
case "$(uname -s)" in Darwin) hos=darwin ;; *) hos=linux ;; esac
case "$(uname -m)" in arm64 | aarch64) harch=arm64 ;; *) harch=amd64 ;; esac
mkdir -p "$scratch/agemod" "$scratch/agedir"
cp "$repo/server/go.mod" "$repo/server/go.sum" "$scratch/agemod/"
printf 'package tools\n\nimport _ "filippo.io/age/cmd/age"\n' >"$scratch/agemod/t.go"
docker run --rm -v "$scratch/agemod:/src" -w /src -v "$scratch/agedir:/out" \
	-v "$gomod_vol:/go/pkg/mod" -v "$gocache_vol:/root/.cache/go-build" \
	-e CGO_ENABLED=0 -e "GOOS=$hos" -e "GOARCH=$harch" -e GOFLAGS=-mod=mod "$golang_image" \
	sh -c 'go build -o /out/age filippo.io/age/cmd/age && chmod 755 /out/age' || fail "could not build age for this host"
# The chmod runs in the container: the file belongs to root there, and a host user that is not
# root (a GitHub runner) cannot chmod it. The directory is the host user's, so cleanup still works.
hostbase=()
for a in "${base[@]}"; do
	[[ "$a" == --no-host-age ]] || hostbase+=("$a")
done
RR_PATH_EXTRA="$scratch/agedir" rr "$here/restore.sh" "${hostbase[@]}" latest
((rc == 0)) || { echo "$out"; fail "the host age restore failed (exit $rc)"; }
grep -q '^rows issues 60$' <<<"$out" || fail "host age: wrong count"
grep -q ' offsite decrypt ' "$SHIM_LOG" && fail "host age: a decrypt container ran"
RR_STDIN="$scratch/stdin.key" RR_PATH_EXTRA="$scratch/agedir" rr "$here/restore.sh" "${hostbase[@]/#$keys\/age.key/-}" latest
((rc == 0)) || { echo "$out"; fail "the host age restore with the key on a pipe failed (exit $rc)"; }
grep -q ' offsite decrypt ' "$SHIM_LOG" && fail "host age with a piped key: a decrypt container ran"
grep -q -F -- "$agekey" "$SHIM_LOG" && fail "host age: the key reached a docker call"
echo "ok: with age on the host, a key file and a piped key both restore 60 issues and no decrypt container runs (macOS or Linux build of the same age module; not a system package)"
no_scratch_db hostage

# ---------------------------------------------------------------- 4. planted secrets

kill "$sampler" 2>/dev/null || true
wait "$sampler" 2>/dev/null || true
sampler=""
step "planted secrets: not in any argument, environment, output, process list or file"
for s in "${planted[@]}"; do
	for f in "$alllog" "$allenv" "$allout" "$allps"; do
		if grep -q -F -- "$s" "$f"; then
			fail "a planted secret (${s:0:10}...) appears in $(basename "$f")"
		fi
	done
done
# The ps sampler must have seen real docker processes, or it proves nothing.
grep -q 'offsite' "$allps" || echo "note: the ps sampler saw no offsite container call (short runs); the argument log is the proof"
grep -q 'offsite fetch' "$alllog" || fail "the argument log is empty"
# Positive controls: the greps above can see a value that IS there.
grep -q -F -- "$inst" "$alllog" || fail "control: the argument log does not hold the install id"
grep -q -F -- "$new_stem" "$allout" || fail "control: the output does not hold the stem"
[[ -z "$(ls -A "$work")" ]] || fail "the work folder is not empty"
echo "ok: no planted secret in $(wc -l <"$alllog" | tr -d ' ') logged docker calls, their environments, the output or the process samples; the work folder is empty"

step "the planted-secret check can fail: a mutant that puts the key in a container's environment is caught"
mkdir -p "$scratch/leak"
cp "$here/lib.sh" "$scratch/leak/"
# shellcheck disable=SC2016  # the pattern is literal text of restore.sh
sed -e 's|dk run "${hard\[@\]}" -i --user "$uid:$gid" --network none -v "$idmount:/identity:ro"|dk run "${hard[@]}" -i -e "LEAK=$idkey" --user "$uid:$gid" --network none -v "$idmount:/identity:ro"|' "$here/restore.sh" >"$scratch/leak/restore.sh"
chmod 755 "$scratch/leak/restore.sh"
cmp -s "$here/restore.sh" "$scratch/leak/restore.sh" && fail "the leak mutant is the same file"
RR_STDIN="$scratch/stdin.key"
rr "$scratch/leak/restore.sh" --container "$c" --image "$img" --verify-key "$keys/signing.pub" --identity - --install-id "$inst" \
	--remote-config "$scratch/net.json" --fetch-network "$net" --allow-cidr "$net_subnet" --work-dir "$work" --no-host-age --verify-only latest
unset RR_STDIN
grep -q -F -- "$agekey" "$SHIM_LOG" || fail "the leak mutant was not caught: the secret check proves nothing"
echo "ok: the leaking mutant puts the key in a docker argument and the check finds it"

printf '\ntest-restore-remote: PASS\n'
