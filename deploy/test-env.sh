#!/usr/bin/env bash
# deploy/.env and project-name safety tests.
#
#   deploy/test-env.sh            (make deploy-test in server/)
#
# deploy.sh runs with a STUB `docker` first on PATH: it answers `info` and
# the version calls, returns nothing for `ps` / `volume ls` (case 9 makes
# some services read as running, and `exec` succeed, through files in its
# control directory), and refuses everything else (build, up, ...). So no
# case can build or start
# anything, even if deploy.sh regresses. Only case 7 passes `ps`, `volume
# ls` and `volume inspect` through to the real docker; the test itself
# creates (never starts) one labelled container and a few volumes with the
# real docker, and removes them. Case 12 (--nuke) switches the stub to a
# restricted pass-through (see there).
#
# 1. An EXISTING deploy/.env, in a checkout whose path contains ".new.", with
#    a bad setting: deploy.sh fails, the file is byte-identical.
# 2. A FIRST run with a bad setting: fails, leaves no .env and no temp file.
# 3. A FIRST run stopped by SIGTERM while it writes its temp file: removed.
# 4. A re-run whose .env lacks POSTGRES_PASSWORD: refused (never
#    regenerated), file byte-identical.
# 5. A re-run whose .env lacks a non-secret key and has no final newline:
#    the key is appended on its own line; nothing else changes.
# 5b. A re-run whose .env lacks SETTINGS_ENCRYPTION_KEY (an install from
#     before): a new 64-hex key is appended on its own line and
#     named; nothing else changes.
# 5c. A malformed SETTINGS_ENCRYPTION_KEY: refused by name, the value is
#     not echoed, the file is byte-identical.
# 6. Two first runs at once: the .env that appears first wins, the other
#    run stops and leaves it byte-identical and no temp file.
# 4b. A .env with an EMPTY SILO_ROOT_USER: refused by name.
# 7. A compose project that another compose file created (the dev stack's
#    labels): refused; this file's labels: accepted; a hand-made volume
#    with the stack's name and no labels: refused; a failing `docker ps`,
#    and a failing `volume inspect` of a listed volume (also first in a
#    20,000-name list): an ERROR naming the guard.
# 8. valid_ipv4 rejects leading zeros and octets over 255.
# 9. --update with a STOPPED stack whose database volume exists: it backs
#    up first, BEFORE the pull, starting postgres alone (--no-deps), and
#    stops when that backup fails, never skips it.
# 9b. postgres running, backup not: backup is started with --no-deps.
# 9c. the re-exec by an older deploy.sh (--update-continue, already
#     pulled): still backs up, starts nothing, says the checkout moved.
# 9d. a real git checkout with an upstream commit: one backup, before the
#     pull; the re-exec passes --non-interactive on.
# 10. The app log cannot be read: a deploy no longer reads it first (the
#     seed-token guard is gone), so it goes on to the build.
# 11. A first run while <project>_postgres_data already exists: refused.
# 12. --nuke: a bare --yes and stray flags refused; no .env
#     (unknown project), an empty name, a dev stack's name and a different
#     exported DEPLOY_PROJECT refused; no terminal without
#     --yes-delete-everything, a wrong name there, and a wrong typed name
#     refused with nothing removed; the right typed name removes only its
#     project (a second labelled project is untouched), keeps deploy/.env,
#     mentions a setup link in the app log without printing it; an unlabelled
#     volume is refused by the guard; --nuke-images and --nuke-env remove
#     the images and the file; a volume held from outside the project exits
#     non-zero and keeps deploy/.env; an image that a container outside the
#     project uses is kept (exit non-zero); after another deploy's build
#     moved the launchpad-api tag, this project's image goes by id and the
#     tag stays; images named dev-ops-api / dev-ops-web (a host deployed
#     before the rename) go too, and are kept when a container outside the
#     project uses them. These cases make real (test-named)
#     volumes, networks and containers; one container runs an echo.
# 13. --setup-link refuses a pipe for stdout before it calls
#     docker; the removed --seed-tokens names its replacement.
# 14. --mint-token with no arguments is refused with the usage;
#     with arguments it refuses a pipe and a file for stdout before it calls
#     docker.
# 15. A checkout pulled by hand: the compose file is new and needs
#     SETTINGS_ENCRYPTION_KEY, the old deploy/.env lacks it. --update and
#     --backup-now (and --smoke) append the key BEFORE their first compose
#     call, so the backup works; no value changes, the mode stays 0600, no
#     value is printed, a file with no final newline still works, and a
#     complete .env stays byte-identical. restore.sh, --setup-link and
#     --mint-token (on a pty) add it too; --backup-now and restore.sh only
#     warn on a malformed key, --update refuses it.
# 16. The first interactive run asks for the bind address, the web
#     host, the files host, the web port and the files port, and never for
#     Postmark: empty answers take the defaults; a bad answer (address, host,
#     port, equal hosts or ports) is asked again; a value in the environment
#     skips its question; --non-interactive, and a re-run with a deploy/.env,
#     ask nothing. The cases type the answers on a pseudo-terminal.
# 17. The first deploy ends with the setup link, through summary()
#     (the file is sourced with DEPLOY_SH_SOURCE_ONLY=1): on a terminal the
#     link is printed last; on a pipe or in a file, and for --update, it is
#     never minted or printed and the hint shows; setup done, an unknown
#     state and a failing `server setup-link` never fail the deploy.
# Scratch: DEPLOY_TMPDIR (default $TMPDIR, else /tmp).
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
scratch="$(mktemp -d "${DEPLOY_TMPDIR:-${TMPDIR:-/tmp}}/devops-test-env.XXXXXX")"
# deploy.sh has no host default: a first run needs both hosts (tests that ask
# for them unset these two with env -u).
export WEB_HOST=web.test.example.com FILES_HOST=files.test.example.com
guard_proj="devops-test-guard-$$"
# The --nuke cases' projects (12), and a container outside them.
NK1="devops-test-nuke-$$-1" NK2="devops-test-nuke-$$-2" NK3="devops-test-nuke-$$-3"
NK4="devops-test-nuke-$$-4" NK5="devops-test-nuke-$$-5" NK6="devops-test-nuke-$$-6"
NK7="devops-test-nuke-$$-7" NK8="devops-test-nuke-$$-8" NK9="devops-test-nuke-$$-9"
holder="devops-test-nuke-$$-holder" holder2="devops-test-nuke-$$-holder2" holder3="devops-test-nuke-$$-holder3" holder4="devops-test-nuke-$$-holder4"
old7_api="" old7_web=""
cleanup() {
	docker rm -f "$guard_proj-c" >/dev/null 2>&1 || true
	docker volume rm -f "${guard_proj}_postgres_data" "${guard_proj}_silo_data" "${guard_proj}_backups" "${guard_proj}_import_work" "${guard_proj}_offsite_work" >/dev/null 2>&1 || true
	local p
	docker rm -f "$holder" "$holder2" "$holder3" "$holder4" "$NK7-mkimg2" >/dev/null 2>&1 || true
	for p in "$NK1" "$NK2" "$NK3" "$NK4" "$NK5" "$NK6" "$NK7" "$NK8" "$NK9"; do
		docker rm -f "$p-app-1" "$p-web-1" "$p-mkimg" >/dev/null 2>&1 || true
		docker network rm "${p}_edge" "${p}_offsite_db" "${p}_egress" >/dev/null 2>&1 || true
		docker volume rm -f "${p}_postgres_data" "${p}_silo_data" "${p}_backups" "${p}_import_work" "${p}_offsite_work" >/dev/null 2>&1 || true
	done
	docker image rm "launchpad-api:nuketest-$$" "launchpad-web:nuketest-$$" \
		"launchpad-api:nuketest6-$$" "launchpad-web:nuketest6-$$" \
		"launchpad-api:nuketest7-$$" "launchpad-web:nuketest7-$$" \
		"dev-ops-api:nuketest8-$$" "dev-ops-web:nuketest8-$$" \
		"launchpad-api:nuketest9-$$" "dev-ops-web:nuketest9-$$" >/dev/null 2>&1 || true
	for p in $old7_api $old7_web; do docker image rm "$p" >/dev/null 2>&1 || true; done
	rm -rf "$scratch"
}
trap cleanup EXIT
fail() {
	echo "test-env: FAIL: $*" >&2
	exit 1
}
sum() { cksum <"$1"; }
# env_files DIR: deploy/.env and any .env.new.* temp file in DIR.
env_files() {
	local f out=""
	for f in "$1/.env" "$1"/.env.new.*; do
		[[ -e "$f" ]] && out="$out ${f##*/}"
	done
	printf '%s' "$out"
}

# The stub docker.
real_docker="$(command -v docker)"
mkdir -p "$scratch/bin"
# docker compose runs in deploy.sh's CLEAN environment (docker_clean), so
# the stub reads its compose switches from files in $ctl, not variables.
ctl="$scratch/stubctl"
mkdir -p "$ctl"
cat >"$scratch/bin/docker" <<'STUB'
#!/bin/sh
ctl=__CTL__
# $ctl/passthru (the --nuke cases, which run docker only through
# docker_clean, so no STUB_* variable reaches here): the calls --nuke
# makes go to the real docker. Anything that could build or start
# (compose up / build / exec / run, docker run / start / create) is still
# refused.
if [ -e "$ctl/passthru" ]; then
	case "$1" in
	info | version | ps | inspect | rm | network | volume | image) exec __REAL__ "$@" ;;
	compose)
		for a in "$@"; do
			case "$a" in
			version | down | logs | ps) exec __REAL__ "$@" ;;
			up | build | exec | run | create | start | restart | pull) break ;;
			esac
		done ;;
	esac
	echo "docker stub: refused: docker $*" >&2
	exit 99
fi
case "$1" in
info) exit 0 ;;
version) echo 0.0.0-stub; exit 0 ;;
compose)
	if [ "$2" = version ]; then echo 2.29.0; exit 0; fi
	# $ctl/trace (case 18): every compose call is appended to $ctl/trace.log, and
	# build / up succeed, so deploy.sh can run past them to the step under test.
	if [ -e "$ctl/trace" ]; then
		echo "$*" >>"$ctl/trace.log"
		for a in "$@"; do
			case "$a" in build | up) exit 0 ;; esac
		done
	fi
	# $ctl/require_settings_key (case 15): the compose file is a NEW one that
	# needs SETTINGS_ENCRYPTION_KEY, so compose fails to interpolate, before
	# any command runs, when the --env-file lacks it (the real error text).
	if [ -e "$ctl/require_settings_key" ]; then
		prev=""
		for a in "$@"; do
			# Missing or empty, like compose's ${VAR:?}.
			if [ "$prev" = --env-file ] && ! grep -q "^SETTINGS_ENCRYPTION_KEY='\{0,1\}[^']" "$a"; then
				echo "error while interpolating services.app.environment.SETTINGS_ENCRYPTION_KEY: required variable SETTINGS_ENCRYPTION_KEY is missing a value: SETTINGS_ENCRYPTION_KEY missing from deploy/.env" >&2
				exit 1
			fi
			prev="$a"
		done
	fi
	# $ctl/require_backup_keys (case 18): the compose file is a NEW one that also
	# needs the two off-site keys.
	if [ -e "$ctl/require_backup_keys" ]; then
		prev=""
		for a in "$@"; do
			if [ "$prev" = --env-file ]; then
				for k in BACKUP_ENCRYPTION_KEY OFFSITE_DB_PASSWORD; do
					if ! grep -q "^$k='\{0,1\}[^']" "$a"; then
						echo "error while interpolating services.offsite.environment.$k: required variable $k is missing a value: $k missing from deploy/.env" >&2
						exit 1
					fi
				done
			fi
			prev="$a"
		done
	fi
	# The last argument (the service, for `ps -q SERVICE`).
	for last; do :; done
	# Read-only compose calls: `logs` (empty, or failing when
	# $ctl/logs_fail exists) and `ps` (no containers, or stub-SERVICE when
	# $ctl/running_SERVICE exists). `exec` succeeds only when $ctl/exec_ok
	# exists (it starts nothing; it stands for `backup.sh once`).
	for a in "$@"; do
		case "$a" in
		logs)
			if [ -e "$ctl/logs_fail" ]; then echo "docker stub: compose logs fails" >&2; exit 1; fi
			exit 0 ;;
		ps)
			if [ -e "$ctl/running_$last" ]; then echo "stub-$last"; fi
			exit 0 ;;
		exec)
			# the two calls the first-run summary makes. psql answers
			# with the text in $ctl/setup_state (nothing: the DB cannot be
			# asked); `server setup-link` prints a fake link, or exits with
			# the code in $ctl/setup_rc. Every such call is logged.
			case "$*" in
			*" psql "*)
				echo "psql" >>"$ctl/calls.log"
				# $ctl/capture (case 18): keep what is sent on stdin.
				if [ -e "$ctl/capture" ]; then cat >>"$ctl/psql.stdin"; fi
				if [ -e "$ctl/setup_state" ]; then cat "$ctl/setup_state"; exit 0; fi
				exit 1 ;;
			*"/bin/server setup-link"*)
				echo "setup-link" >>"$ctl/calls.log"
				if [ -e "$ctl/setup_rc" ]; then exit "$(cat "$ctl/setup_rc")"; fi
				echo "https://launchpad.test/setup#STUBSETUPLINKSECRET"
				exit 0 ;;
			esac
			if [ -e "$ctl/exec_ok" ]; then echo "docker stub: compose exec ok: $*" >&2; exit 0; fi
			break ;;
		esac
	done ;;
inspect)
	# A running stub service is healthy (wait_healthy).
	for last; do :; done
	case "$*" in
	*'{{.Config.Image}}'*) echo launchpad-api:stubtest; exit 0 ;;
	# The networks of the postgres container ($ctl/pg_nets, case 18).
	*'.NetworkSettings.Networks'*) if [ -e "$ctl/pg_nets" ]; then cat "$ctl/pg_nets"; fi; exit 0 ;;
	esac
	if [ -e "$ctl/running_${last#stub-}" ]; then echo healthy; exit 0; fi ;;
ps)
	if [ -n "${STUB_PS_FAIL:-}" ]; then echo "docker stub: ps fails" >&2; exit 1; fi
	if [ -n "${STUB_PASSTHRU:-}" ]; then exec "$REAL_DOCKER" "$@"; fi
	exit 0 ;;
volume)
	# Only the read-only calls the guard makes; the test makes its own
	# volumes with the real docker. STUB_VOLUMES: names `volume ls` lists;
	# STUB_INSPECT_FAIL: `volume inspect` fails like a daemon error.
	case "$2" in
	ls | inspect)
		if [ -n "${STUB_PASSTHRU:-}" ]; then exec "$REAL_DOCKER" "$@"; fi
		if [ "$2" = ls ]; then
			# STUB_VOLUMES_LABEL: the dev.orbit.deploy label of STUB_VOLUMES
			# (for the labelled `ls --format` and for `inspect -f`).
			if [ -n "${STUB_VOLUMES:-}" ] && [ -n "${STUB_VOLUMES_LABEL:-}" ] && [ "$3" != -q ]; then
				printf '%s|%s\n' "$STUB_VOLUMES" "$STUB_VOLUMES_LABEL"
				exit 0
			fi
			[ -n "${STUB_VOLUMES:-}" ] && printf '%s\n' "$STUB_VOLUMES"
			# STUB_VOLUMES_PAD: that many more names after it, for the
			# plain `volume ls -q` only (a long list of other volumes).
			[ -n "${STUB_VOLUMES_PAD:-}" ] && [ "$3" = -q ] && seq -f 'pad-volume-%g' 1 "$STUB_VOLUMES_PAD"
			exit 0
		fi
		if [ -n "${STUB_INSPECT_FAIL:-}" ]; then echo "docker stub: inspect: daemon error" >&2; exit 1; fi
		if [ -n "${STUB_VOLUMES_LABEL:-}" ]; then echo "$STUB_VOLUMES_LABEL"; exit 0; fi
		exit 1 ;;
	esac ;;
esac
echo "docker stub: refused: docker $*" >&2
exit 99
STUB
# Bake the control directory into the stub (sed, not the heredoc: the
# heredoc is quoted so that the stub's own $ variables stay literal).
sed -i.bak -e "s|__CTL__|'$ctl'|" -e "s|__REAL__|'$real_docker'|" "$scratch/bin/docker"
rm -f "$scratch/bin/docker.bak"
chmod +x "$scratch/bin/docker"
export REAL_DOCKER="$real_docker"
# High ports that nothing uses, and test project names.
run() { env PATH="$scratch/bin:$PATH" DEPLOY_WEB_PORT=47491 DEPLOY_FILES_PORT=47492 "$@"; }
# A valid .env for a test project (placeholders from .env.example).
make_env() {
	sed -e "s/^DEPLOY_PROJECT=.*/DEPLOY_PROJECT='$1'/" \
		-e "s/^DEPLOY_WEB_PORT=.*/DEPLOY_WEB_PORT='47491'/" \
		-e "s/^DEPLOY_FILES_PORT=.*/DEPLOY_FILES_PORT='47492'/" "$here/.env.example"
}

# A copy of deploy/ under a path that contains ".new.".
co="$scratch/srv/launchpad.new.1"
mkdir -p "$co"
cp -R "$here" "$co/deploy"
rm -f "$co/deploy/.env" "$co"/deploy/.env.new.*
d="$co/deploy"

echo "== 1. existing .env under $co, bad S3_BUCKET"
make_env devops-test-env | sed "s/^S3_BUCKET=.*/S3_BUCKET='Bad_Bucket'/" >"$d/.env"
chmod 600 "$d/.env"
before="$(sum "$d/.env")"
if run "$d/deploy.sh" --non-interactive </dev/null >"$scratch/1.out" 2>&1; then
	cat "$scratch/1.out"
	fail "deploy.sh accepted a bad S3_BUCKET"
fi
{ grep ERROR "$scratch/1.out" || true; } | cut -c1-200
[[ -f "$d/.env" ]] || fail "deploy/.env was DELETED"
[[ "$(sum "$d/.env")" == "$before" ]] || fail "deploy/.env changed"
grep -q 'edit .*/.env and re-run' "$scratch/1.out" || fail "no 'edit deploy/.env' advice"
echo "ok: exit non-zero, deploy/.env byte-identical"

echo "== 2. first run, bad S3_BUCKET"
rm -f "$d/.env"
if run env DEPLOY_PROJECT=devops-test-env S3_BUCKET=Bad_Bucket "$d/deploy.sh" --non-interactive </dev/null >"$scratch/2.out" 2>&1; then
	cat "$scratch/2.out"
	fail "a first run accepted a bad S3_BUCKET"
fi
{ grep ERROR "$scratch/2.out" || true; } | cut -c1-200
left="$(env_files "$d")"
[[ -z "$left" ]] || fail "a failed first run left: $left"
echo "ok: exit non-zero, no .env and no temp file"

# A slow `od` (the secret generator) holds a first run inside ensure_env.
mkdir -p "$scratch/slowbin"
real_od="$(command -v od)"
printf '#!/bin/sh\nsleep 1\nexec %s "$@"\n' "$real_od" >"$scratch/slowbin/od"
chmod +x "$scratch/slowbin/od"
wait_for_temp() {
	local i
	for i in $(seq 1 100); do
		ls "$d"/.env.new.* >/dev/null 2>&1 && return 0
		sleep 0.1
	done
	return 1
}

echo "== 3. first run stopped by SIGTERM while the temp file exists"
# `env` (not the run function) so that $! is deploy.sh itself (env execs it).
env PATH="$scratch/slowbin:$scratch/bin:$PATH" DEPLOY_WEB_PORT=47491 DEPLOY_FILES_PORT=47492 \
	DEPLOY_PROJECT=devops-test-env "$d/deploy.sh" --non-interactive </dev/null >"$scratch/3.out" 2>&1 &
pid=$!
wait_for_temp || {
	kill "$pid" 2>/dev/null || true
	cat "$scratch/3.out"
	fail "the temp file never appeared"
}
kill -TERM "$pid"
rc=0
wait "$pid" || rc=$?
((rc != 0)) || fail "deploy.sh exited 0 after SIGTERM"
left="$(env_files "$d")"
[[ -z "$left" ]] || fail "SIGTERM left: $left"
echo "ok: exit $rc, the temp file was removed"

echo "== 4. re-run, POSTGRES_PASSWORD missing from .env"
make_env devops-test-env | grep -v '^POSTGRES_PASSWORD=' >"$d/.env"
chmod 600 "$d/.env"
before="$(sum "$d/.env")"
if run "$d/deploy.sh" --non-interactive </dev/null >"$scratch/4.out" 2>&1; then
	cat "$scratch/4.out"
	fail "a missing POSTGRES_PASSWORD was accepted"
fi
{ grep ERROR "$scratch/4.out" || true; } | cut -c1-200
grep -q 'POSTGRES_PASSWORD is missing' "$scratch/4.out" || fail "no clear message for the missing password"
[[ "$(sum "$d/.env")" == "$before" ]] || fail "deploy/.env changed (a password was generated?)"
echo "ok: refused, deploy/.env byte-identical"

echo "== 4b. re-run, SILO_ROOT_USER empty"
make_env devops-test-env | sed "s/^SILO_ROOT_USER=.*/SILO_ROOT_USER=''/" >"$d/.env"
chmod 600 "$d/.env"
before="$(sum "$d/.env")"
if run "$d/deploy.sh" --non-interactive </dev/null >"$scratch/4b.out" 2>&1; then
	cat "$scratch/4b.out"
	fail "an empty SILO_ROOT_USER was accepted"
fi
{ grep ERROR "$scratch/4b.out" || true; } | cut -c1-200
grep -q 'SILO_ROOT_USER is empty' "$scratch/4b.out" || fail "no clear message for the empty SILO_ROOT_USER"
[[ "$(sum "$d/.env")" == "$before" ]] || fail "deploy/.env changed"
echo "ok: refused by name, deploy/.env byte-identical"

echo "== 5. re-run, a non-secret key missing and no final newline"
make_env devops-test-env | grep -v '^BACKUP_KEEP_DAYS=' | perl -pe 'chomp if eof' >"$d/.env"
chmod 600 "$d/.env"
cp "$d/.env" "$scratch/5.before"
# It gets past the settings and stops at the stub (build is refused).
run "$d/deploy.sh" --non-interactive </dev/null >"$scratch/5.out" 2>&1 || true
grep -q 'added: BACKUP_KEEP_DAYS' "$scratch/5.out" || {
	cat "$scratch/5.out"
	fail "BACKUP_KEEP_DAYS was not added"
}
{
	cat "$scratch/5.before"
	printf "\nBACKUP_KEEP_DAYS='30'\n"
} >"$scratch/5.want"
cmp -s "$d/.env" "$scratch/5.want" || {
	diff "$scratch/5.want" "$d/.env" || true
	fail "the appended .env is not the old file + newline + the new key"
}
grep -q 'docker stub: refused' "$scratch/5.out" || fail "the run did not stop at the docker stub"
echo "ok: appended on its own line, the rest unchanged; stopped at the stub"

echo "== 5b. re-run, SETTINGS_ENCRYPTION_KEY missing (an install from before the settings key)"
make_env devops-test-env | grep -v '^SETTINGS_ENCRYPTION_KEY=' >"$d/.env"
chmod 600 "$d/.env"
cp "$d/.env" "$scratch/5b.before"
run "$d/deploy.sh" --non-interactive </dev/null >"$scratch/5b.out" 2>&1 || true
grep -q 'added: SETTINGS_ENCRYPTION_KEY' "$scratch/5b.out" || {
	cat "$scratch/5b.out"
	fail "SETTINGS_ENCRYPTION_KEY was not added"
}
new_line="$(tail -n 1 "$d/.env")"
[[ "$new_line" =~ ^SETTINGS_ENCRYPTION_KEY=\'[0-9a-f]{64}\'$ ]] || fail "the appended key is not 64 hex characters"
head -n "$(($(wc -l <"$d/.env") - 1))" "$d/.env" | cmp -s - "$scratch/5b.before" || fail "other lines of deploy/.env changed"
grep -qF "${new_line#SETTINGS_ENCRYPTION_KEY=}" "$scratch/5b.out" && fail "deploy.sh printed the generated key"
grep -q 'docker stub: refused' "$scratch/5b.out" || fail "the run did not stop at the docker stub"
echo "ok: a 64-hex key appended, not printed; the rest unchanged"

echo "== 5c. re-run, SETTINGS_ENCRYPTION_KEY malformed"
make_env devops-test-env | sed "s/^SETTINGS_ENCRYPTION_KEY=.*/SETTINGS_ENCRYPTION_KEY='not-hex-planted-5c'/" >"$d/.env"
chmod 600 "$d/.env"
before="$(sum "$d/.env")"
if run "$d/deploy.sh" --non-interactive </dev/null >"$scratch/5c.out" 2>&1; then
	cat "$scratch/5c.out"
	fail "a malformed SETTINGS_ENCRYPTION_KEY was accepted"
fi
grep -q 'SETTINGS_ENCRYPTION_KEY must be exactly 64 hex characters' "$scratch/5c.out" || fail "no clear message for the malformed key"
grep -q 'not-hex-planted-5c' "$scratch/5c.out" && fail "deploy.sh echoed the key"
[[ "$(sum "$d/.env")" == "$before" ]] || fail "deploy/.env changed"
echo "ok: refused by name, not echoed, deploy/.env byte-identical"

echo "== 6. two first runs at once: the first .env wins"
rm -f "$d/.env"
# `env` (not the run function) so that $! is deploy.sh itself (env execs it).
env PATH="$scratch/slowbin:$scratch/bin:$PATH" DEPLOY_WEB_PORT=47491 DEPLOY_FILES_PORT=47492 \
	DEPLOY_PROJECT=devops-test-env "$d/deploy.sh" --non-interactive </dev/null >"$scratch/6.out" 2>&1 &
pid=$!
wait_for_temp || {
	kill "$pid" 2>/dev/null || true
	fail "the temp file never appeared"
}
# The "other" run finishes first.
make_env devops-test-env >"$d/.env"
other="$(sum "$d/.env")"
rc=0
wait "$pid" || rc=$?
((rc != 0)) || fail "the second first run did not stop"
grep -q 'appeared while this first run was writing' "$scratch/6.out" || {
	cat "$scratch/6.out"
	fail "no race message"
}
[[ "$(sum "$d/.env")" == "$other" ]] || fail "the winning .env was changed"
left="$(env_files "$d")"
[[ "$left" == " .env" ]] || fail "files left: $left"
echo "ok: exit $rc, the first .env kept byte-identical, no temp file"

echo "== 7. a compose project that another compose file created"
make_env "$guard_proj" >"$d/.env"
chmod 600 "$d/.env"
img="$(grep -m1 -E '^\s+image: postgres:' "$here/docker-compose.yml" | awk '{print $2}')"
# The dev stack's labels: config_files is server/docker-compose.yml, and
# its volumes carry no dev.orbit.deploy label.
"$real_docker" volume create --label "com.docker.compose.project=$guard_proj" \
	--label com.docker.compose.volume=postgres_data "${guard_proj}_postgres_data" >/dev/null
"$real_docker" create --name "$guard_proj-c" --label "com.docker.compose.project=$guard_proj" \
	--label "com.docker.compose.project.config_files=/srv/dev ops/server/docker-compose.yml" \
	"$img" true >/dev/null
if run env STUB_PASSTHRU=1 "$d/deploy.sh" --non-interactive </dev/null >"$scratch/7.out" 2>&1; then
	cat "$scratch/7.out"
	fail "deploy.sh accepted another stack's project"
fi
{ grep ERROR "$scratch/7.out" || true; } | cut -c1-200
grep -q 'already belongs to another stack' "$scratch/7.out" || {
	cat "$scratch/7.out"
	fail "no refusal for another stack's project"
}
if ! grep -q "$guard_proj-c" "$scratch/7.out" || ! grep -q "${guard_proj}_postgres_data" "$scratch/7.out"; then
	fail "the refusal does not name the container and the volume"
fi
if grep -q 'docker stub: refused' "$scratch/7.out"; then fail "it went past the guard"; fi
# This file's own labels (a path with a space) pass the guard.
"$real_docker" rm -f "$guard_proj-c" >/dev/null
"$real_docker" volume rm -f "${guard_proj}_postgres_data" >/dev/null
"$real_docker" volume create --label "com.docker.compose.project=$guard_proj" \
	--label dev.orbit.deploy=production "${guard_proj}_silo_data" >/dev/null
"$real_docker" create --name "$guard_proj-c" --label "com.docker.compose.project=$guard_proj" \
	--label "com.docker.compose.project.config_files=/srv/my host/deploy/docker-compose.yml" \
	"$img" true >/dev/null
run env STUB_PASSTHRU=1 "$d/deploy.sh" --non-interactive </dev/null >"$scratch/7b.out" 2>&1 || true
if grep -q 'already belongs to another stack' "$scratch/7b.out"; then
	cat "$scratch/7b.out"
	fail "the guard refused this file's own stack"
fi
grep -q 'docker stub: refused' "$scratch/7b.out" || {
	cat "$scratch/7b.out"
	fail "the run with this file's labels did not reach the stub"
}
# A volume with this stack's name, made by hand: no compose labels at all,
# so only `docker volume inspect` sees it.
"$real_docker" rm -f "$guard_proj-c" >/dev/null
"$real_docker" volume rm -f "${guard_proj}_silo_data" >/dev/null
"$real_docker" volume create "${guard_proj}_backups" >/dev/null
if run env STUB_PASSTHRU=1 "$d/deploy.sh" --non-interactive </dev/null >"$scratch/7c.out" 2>&1; then
	cat "$scratch/7c.out"
	fail "a hand-made volume ${guard_proj}_backups was accepted"
fi
grep -q "already belongs to another stack.*${guard_proj}_backups" "$scratch/7c.out" || {
	cat "$scratch/7c.out"
	fail "the hand-made volume was not refused by name"
}
if grep -q 'docker stub: refused' "$scratch/7c.out"; then fail "it went past the guard (hand-made volume)"; fi
"$real_docker" volume rm -f "${guard_proj}_backups" >/dev/null
# the import_work volume is guarded like the other three.
"$real_docker" volume create "${guard_proj}_import_work" >/dev/null
if run env STUB_PASSTHRU=1 "$d/deploy.sh" --non-interactive </dev/null >"$scratch/7c2.out" 2>&1; then
	cat "$scratch/7c2.out"
	fail "a hand-made volume ${guard_proj}_import_work was accepted"
fi
grep -q "already belongs to another stack.*${guard_proj}_import_work" "$scratch/7c2.out" || {
	cat "$scratch/7c2.out"
	fail "the hand-made import_work volume was not refused by name"
}
"$real_docker" volume rm -f "${guard_proj}_import_work" >/dev/null
# so is the off-site worker's scratch volume.
"$real_docker" volume create "${guard_proj}_offsite_work" >/dev/null
if run env STUB_PASSTHRU=1 "$d/deploy.sh" --non-interactive </dev/null >"$scratch/7c3.out" 2>&1; then
	cat "$scratch/7c3.out"
	fail "a hand-made volume ${guard_proj}_offsite_work was accepted"
fi
grep -q "already belongs to another stack.*${guard_proj}_offsite_work" "$scratch/7c3.out" || {
	cat "$scratch/7c3.out"
	fail "the hand-made offsite_work volume was not refused by name"
}
"$real_docker" volume rm -f "${guard_proj}_offsite_work" >/dev/null
# docker ps itself fails: an ERROR line that names the guard, nothing done.
if run env STUB_PS_FAIL=1 "$d/deploy.sh" --non-interactive </dev/null >"$scratch/7d.out" 2>&1; then
	cat "$scratch/7d.out"
	fail "a failing docker ps was ignored"
fi
grep -q "^ERROR: guard_project: 'docker ps' failed" "$scratch/7d.out" || {
	cat "$scratch/7d.out"
	fail "no ERROR line naming the guard"
}
if grep -q 'docker stub: refused' "$scratch/7d.out"; then fail "it went past the guard (docker ps failed)"; fi
# `docker volume inspect` fails on a volume that `volume ls` lists (a
# daemon error, not an absent volume): the guard ERROR, nothing done.
if run env STUB_VOLUMES="${guard_proj}_postgres_data" STUB_INSPECT_FAIL=1 \
	"$d/deploy.sh" --non-interactive </dev/null >"$scratch/7e.out" 2>&1; then
	cat "$scratch/7e.out"
	fail "a failing docker volume inspect was ignored"
fi
grep -q "^ERROR: guard_project: 'docker volume inspect ${guard_proj}_postgres_data' failed" "$scratch/7e.out" || {
	cat "$scratch/7e.out"
	fail "a failing volume inspect did not stop with the guard ERROR"
}
if grep -q 'docker stub: refused' "$scratch/7e.out"; then fail "it went past the guard (inspect failed)"; fi
# The same with the stack's volume FIRST in a long list: a `printf | grep -q`
# pipeline under pipefail can SIGPIPE there and read as "absent".
if run env STUB_VOLUMES="${guard_proj}_postgres_data" STUB_VOLUMES_PAD=20000 STUB_INSPECT_FAIL=1 \
	"$d/deploy.sh" --non-interactive </dev/null >"$scratch/7f.out" 2>&1; then
	fail "a failing docker volume inspect (long list) was ignored"
fi
grep -q "^ERROR: guard_project: 'docker volume inspect ${guard_proj}_postgres_data' failed" "$scratch/7f.out" || {
	cut -c1-300 "$scratch/7f.out"
	fail "with a long volume list the stack's volume read as absent"
}
echo "ok: another stack's project refused (container and volume named); this file's labels accepted; hand-made volume refused; failing docker ps and failing volume inspect stop with a guard ERROR"

echo "== 9. --update with a stopped stack: the database volume exists -> back up first, before the pull"
make_env devops-test-env >"$d/.env"
chmod 600 "$d/.env"
# $co is no git checkout: the run must stop at the backup, before any pull.
run env STUB_VOLUMES="devops-test-env_postgres_data" STUB_VOLUMES_LABEL=production \
	"$d/deploy.sh" --update --non-interactive </dev/null >"$scratch/9.out" 2>&1 || true
grep -q '== backup before the update' "$scratch/9.out" || {
	cat "$scratch/9.out"
	fail "--update did not back up although the database volume exists"
}
# A stopped postgres is started ALONE, from the compose file of the running
# version (before the pull), never through the backup service's depends_on.
grep -q 'docker stub: refused: docker compose .* up -d --no-deps postgres$' "$scratch/9.out" || {
	cat "$scratch/9.out"
	fail "--update did not start the stopped postgres alone (up -d --no-deps postgres)"
}
if grep -q '== update: git pull' "$scratch/9.out"; then fail "--update pulled before its backup"; fi
if grep -q 'docker stub: refused: docker compose .* build' "$scratch/9.out"; then fail "--update went on to build after a failed backup"; fi
grep -q 'the backup before the update failed; nothing was pulled or updated (at most postgres and the backup service were started)' "$scratch/9.out" || fail "no 'backup failed' message"
echo "ok: backed up before the pull (started postgres alone, --no-deps), stopped when that failed: no pull, no build"

echo "== 9b. --update, postgres running and the backup service not: start backup with --no-deps"
touch "$ctl/running_postgres"
run env STUB_VOLUMES="devops-test-env_postgres_data" STUB_VOLUMES_LABEL=production \
	"$d/deploy.sh" --update --non-interactive </dev/null >"$scratch/9b.out" 2>&1 || true
rm -f "$ctl"/running_*
grep -q 'docker stub: refused: docker compose .* up -d --no-deps backup$' "$scratch/9b.out" || {
	cat "$scratch/9b.out"
	fail "--update did not start the backup service with --no-deps"
}
if grep -Eq 'docker stub: refused: docker compose .* up( -d)? (backup|postgres)$' "$scratch/9b.out" ||
	grep -q 'up -d --no-deps postgres' "$scratch/9b.out"; then
	cat "$scratch/9b.out"
	fail "--update started a service that could recreate postgres"
fi
if grep -q '== update: git pull' "$scratch/9b.out"; then fail "--update pulled before its backup (9b)"; fi
grep -q 'the backup before the update failed; nothing was pulled or updated (at most postgres and the backup service were started)' "$scratch/9b.out" || fail "no 'backup failed' message (9b)"
echo "ok: postgres left alone, backup started with --no-deps, no pull after the failure"

echo "== 9c. re-exec by an older deploy.sh (--update-continue, already pulled), postgres stopped"
run env STUB_VOLUMES="devops-test-env_postgres_data" STUB_VOLUMES_LABEL=production \
	"$d/deploy.sh" --update-continue --non-interactive </dev/null >"$scratch/9c.out" 2>&1 || true
grep -q '== backup before the update' "$scratch/9c.out" || {
	cat "$scratch/9c.out"
	fail "the older re-exec path skipped the backup"
}
if grep -q 'docker stub: refused: docker compose .* up ' "$scratch/9c.out"; then
	cat "$scratch/9c.out"
	fail "after the pull it started a service from the NEW compose file"
fi
grep -q 'WARNING: postgres is not running' "$scratch/9c.out" || fail "no 'postgres is not running' warning (9c)"
grep -q 'at most the backup service was started, but the older deploy.sh already pulled the checkout' "$scratch/9c.out" || {
	cat "$scratch/9c.out"
	fail "no accurate message after the pull (9c)"
}
if grep -q 'docker stub: refused: docker compose .* build' "$scratch/9c.out"; then fail "it went on to build (9c)"; fi
echo "ok: backup still taken on the older re-exec path; it starts nothing after the pull and says the checkout moved"

echo "== 9d. --update in a real git checkout: one backup BEFORE the pull; the re-exec keeps --non-interactive"
g="$scratch/git"
mkdir -p "$g"
gq() { git -c user.name=test-env -c user.email=test-env@example.invalid -c init.defaultBranch=develop "$@" >/dev/null 2>&1; }
gq init --bare "$g/origin.git"
gq clone "$g/origin.git" "$g/work"
cp -R "$here" "$g/work/deploy"
rm -f "$g/work/deploy/.env" "$g/work"/deploy/.env.new.*
gq -C "$g/work" add -A
gq -C "$g/work" commit -m base
gq -C "$g/work" push origin HEAD:develop
gq clone -b develop "$g/origin.git" "$g/srv"
# The upstream commit: the new deploy.sh prints the arguments it was run with.
perl -0pi -e 's/\nset -euo pipefail\n/\nset -euo pipefail\necho "REEXEC_ARGS: \$*"\n/' "$g/work/deploy/deploy.sh"
grep -q '^echo "REEXEC_ARGS' "$g/work/deploy/deploy.sh" || fail "could not plant the upstream change"
gq -C "$g/work" commit -am upstream
gq -C "$g/work" push origin HEAD:develop
make_env devops-test-env >"$g/srv/deploy/.env"
chmod 600 "$g/srv/deploy/.env"
touch "$ctl/running_postgres" "$ctl/running_backup" "$ctl/exec_ok"
run env STUB_VOLUMES="devops-test-env_postgres_data" STUB_VOLUMES_LABEL=production \
	"$g/srv/deploy/deploy.sh" --update --non-interactive </dev/null >"$scratch/9d.out" 2>&1 || true
rm -f "$ctl"/running_* "$ctl/exec_ok"
grep -q '^REEXEC_ARGS: --update-after-pull --non-interactive$' "$scratch/9d.out" || {
	cat "$scratch/9d.out"
	fail "the re-exec after the pull did not pass --non-interactive on (or did not happen)"
}
[[ "$(grep -c '== backup before the update' "$scratch/9d.out")" == 1 ]] || {
	cat "$scratch/9d.out"
	fail "not exactly one backup before the update"
}
grep -q 'docker stub: compose exec ok: .* backup /bin/sh /backup/backup.sh once' "$scratch/9d.out" || fail "backup.sh once did not run (9d)"
b_line="$(grep -n '== backup before the update' "$scratch/9d.out" | cut -d: -f1)"
p_line="$(grep -n '== update: git pull' "$scratch/9d.out" | cut -d: -f1)"
((b_line < p_line)) || fail "the backup (line $b_line) came after the pull (line $p_line)"
grep -q 'docker stub: refused: docker compose .* build' "$scratch/9d.out" || {
	cat "$scratch/9d.out"
	fail "the re-executed deploy.sh did not reach the build"
}
# a limited check: the stub refuses the first compose build, so the
# run ends there. It proves only that, up to that build, the update ran one
# compose build and called no docker image rm / rmi / pull / tag and named no
# dev-ops-api / dev-ops-web tag. Nothing here runs after the build.
[[ "$(grep -c 'docker stub: refused: docker compose .* build' "$scratch/9d.out")" == 1 ]] || {
	cat "$scratch/9d.out"
	fail "--update did not build exactly once"
}
if grep -q 'dev-ops-\(api\|web\)' "$scratch/9d.out"; then fail "--update names an old dev-ops-api / dev-ops-web image"; fi
if grep -Eq 'docker stub: refused: docker (image|rmi|pull|tag) ' "$scratch/9d.out"; then fail "--update touched an image by name"; fi
echo "ok: backup once, before the pull; the new deploy.sh ran with --update-after-pull --non-interactive"

echo "== 9e. the compose file names the images launchpad-api / launchpad-web, never the old names"
grep -qF '    image: launchpad-api:' "$here/docker-compose.yml" || fail "the app image is not launchpad-api:<version>"
grep -qF '    image: launchpad-web:' "$here/docker-compose.yml" || fail "the web image is not launchpad-web:<version>"
if grep -q 'dev-ops-\(api\|web\)' "$here/docker-compose.yml"; then fail "the compose file still names dev-ops-api / dev-ops-web"; fi
echo "ok: compose images are launchpad-api / launchpad-web"

echo "== 10. the app log cannot be read: no longer a reason to refuse"
touch "$ctl/logs_fail"
if run "$d/deploy.sh" --non-interactive </dev/null >"$scratch/10.out" 2>&1; then
	cat "$scratch/10.out"
	fail "the stub refuses the build, so this run must fail"
fi
rm -f "$ctl/logs_fail"
if grep -q -i 'app log\|seed token' "$scratch/10.out"; then
	cat "$scratch/10.out"
	fail "a deploy still reads the app log before it builds"
fi
grep -q 'docker stub: refused: docker compose .* build' "$scratch/10.out" || {
	cat "$scratch/10.out"
	fail "it did not go on to the build"
}
echo "ok: no app-log guard, it went on to the build (refused by the stub)"

echo "== 11. first run, but the database volume already exists"
rm -f "$d/.env"
if run env DEPLOY_PROJECT=devops-test-env STUB_VOLUMES="devops-test-env_postgres_data" \
	"$d/deploy.sh" --non-interactive </dev/null >"$scratch/11.out" 2>&1; then
	cat "$scratch/11.out"
	fail "a first run next to an existing database volume was accepted"
fi
grep -q 'deploy/.env is missing, but the volume devops-test-env_postgres_data exists' "$scratch/11.out" || {
	cat "$scratch/11.out"
	fail "no refusal naming the existing volume"
}
left="$(env_files "$d")"
[[ -z "$left" ]] || fail "the refused first run left: $left"
echo "ok: refused, no .env and no temp file written"

# ---------------------------------------------------------------- 12. --nuke
# The --nuke cases run docker through docker_clean, so the stub is switched
# to its restricted pass-through ($ctl/passthru): ps, inspect, rm, network,
# volume, image and compose down / logs / ps reach the real docker; build,
# up, exec, run, create and start are still refused. The test makes each
# project by hand with the labels compose writes (compose matches a
# container only with a config-hash label): four labelled volumes, an
# edge network and app + web containers (created, never started; the
# app container of the 12f project runs once, an echo only, to put a fake
# seed-token line in its log).
# IMAGE-PREFIX is launchpad (default) or dev-ops (the names before the rename:
# a host deployed before the rename has containers on them). A third word
# sets the web image's prefix on its own (a mixed project).
mk_proj() { # mk_proj PROJECT [IMAGE-TAG [API-PREFIX [WEB-PREFIX]]]
	local p="$1" tag="${2:-}" v svc image api_p="${3:-launchpad}" web_p="${4:-${3:-launchpad}}"
	for v in postgres_data silo_data backups import_work offsite_work; do
		"$real_docker" volume create --label "com.docker.compose.project=$p" --label "com.docker.compose.volume=$v" \
			--label dev.orbit.deploy=production "${p}_$v" >/dev/null
	done
	"$real_docker" network create --label "com.docker.compose.project=$p" --label com.docker.compose.network=edge \
		"${p}_edge" >/dev/null
	# the two networks of the off-site worker (internal, and the one with a route out).
	"$real_docker" network create --internal --label "com.docker.compose.project=$p" --label com.docker.compose.network=offsite_db \
		"${p}_offsite_db" >/dev/null
	"$real_docker" network create --label "com.docker.compose.project=$p" --label com.docker.compose.network=egress \
		"${p}_egress" >/dev/null
	for svc in app web; do
		image="$img"
		if [[ -n "$tag" ]]; then
			# An image of its own (a commit of a never-started container), so
			# its id is not the base image's, as with a real build.
			if [[ $svc == app ]]; then image="$api_p-api:$tag"; else image="$web_p-web:$tag"; fi
			"$real_docker" create --name "$p-mkimg" "$img" true >/dev/null
			"$real_docker" commit "$p-mkimg" "$image" >/dev/null
			"$real_docker" rm "$p-mkimg" >/dev/null
		fi
		"$real_docker" create --name "${p}-${svc}-1" --network "${p}_edge" \
			--label "com.docker.compose.project=$p" --label "com.docker.compose.service=$svc" \
			--label com.docker.compose.oneoff=False --label com.docker.compose.container-number=1 \
			--label com.docker.compose.config-hash=devops-test \
			--label "com.docker.compose.project.config_files=/srv/launchpad/deploy/docker-compose.yml" \
			-v "${p}_backups:/backups" --entrypoint /bin/sh "$image" -c true >/dev/null
	done
}
# The project's objects, as "name:state" words (state there|gone).
proj_state() {
	local p="$1" out="" x
	for x in "${p}_postgres_data" "${p}_silo_data" "${p}_backups" "${p}_import_work" "${p}_offsite_work"; do
		if "$real_docker" volume inspect "$x" >/dev/null 2>&1; then out="$out $x:there"; else out="$out $x:gone"; fi
	done
	for x in "${p}_edge" "${p}_offsite_db" "${p}_egress"; do
		if "$real_docker" network inspect "$x" >/dev/null 2>&1; then out="$out $x:there"; else out="$out $x:gone"; fi
	done
	for x in "${p}-app-1" "${p}-web-1"; do
		if "$real_docker" container inspect "$x" >/dev/null 2>&1; then out="$out $x:there"; else out="$out $x:gone"; fi
	done
	printf '%s' "${out# }"
}
all_there() { [[ "$(proj_state "$1")" != *:gone* ]]; }
none_left() {
	[[ "$(proj_state "$1")" != *:there* ]] || return 1
	local f="label=com.docker.compose.project=$1"
	[[ -z "$("$real_docker" ps -aq --filter "$f")$("$real_docker" network ls -q --filter "$f")$("$real_docker" volume ls -q --filter "$f")" ]]
}
image_there() { "$real_docker" image inspect "$1" >/dev/null 2>&1; }
# A .env for a nuke project, with recognisable secret values: no output
# may contain "nuke-secret-".
nuke_env() {
	make_env "$1" | sed -E "s/^(POSTGRES_PASSWORD|AUTH_OTP_PEPPER|SILO_ROOT_PASSWORD|S3_SECRET_KEY)=.*/\1='nuke-secret-\1-0123456789abcdef0123456789abcdef'/" >"$d/.env"
	chmod 600 "$d/.env"
}
no_secret() { if grep -q 'nuke-secret-' "$1"; then fail "$1 shows a secret value from deploy/.env"; fi; }
# pty_run INPUT OUT CMD...: CMD on a pseudo-terminal (stdin is a tty, so
# deploy.sh asks); INPUT is typed at the confirmation prompt. Exit: CMD's.
pty_run() {
	python3 -c '
import os, pty, select, sys
inp, out, cmd = sys.argv[1], sys.argv[2], sys.argv[3:]
pid, fd = pty.fork()
if pid == 0:
    os.execvp(cmd[0], cmd)
buf, sent = b"", False
while True:
    r, _, _ = select.select([fd], [], [], 120)
    if not r:
        break
    try:
        data = os.read(fd, 4096)
    except OSError:
        break
    if not data:
        break
    buf += data
    if not sent and b"Type the project name" in buf:
        os.write(fd, inp.encode() + b"\n")
        sent = True
_, st = os.waitpid(pid, 0)
open(out, "wb").write(buf)
sys.exit(os.waitstatus_to_exitcode(st))
' "$@"
}
nrun() { run env -u DEPLOY_PROJECT "$d/deploy.sh" "$@"; }
# want_refused OUT PATTERN WHY: the last run ($rc) failed and OUT matches.
want_refused() {
	if ((rc == 0)) || ! grep -q -- "$2" "$1"; then
		cat "$1"
		fail "$3 (exit $rc)"
	fi
}

echo "== 12a. --nuke: a bare --yes, flags without --nuke, and --nuke with another mode are refused"
nuke_env "$NK1"
for args in "--nuke --yes" "--nuke -y" "--nuke --yes-delete-everything=$NK1" "--nuke-env" "--nuke-images" \
	"--yes-delete-everything $NK1" "--nuke --update --yes-delete-everything $NK1" "--nuke --yes-delete-everything"; do
	rc=0
	# shellcheck disable=SC2086  # the words of $args are the arguments
	nrun $args </dev/null >"$scratch/12a.out" 2>&1 || rc=$?
	((rc != 0)) || {
		cat "$scratch/12a.out"
		fail "deploy.sh $args exited 0"
	}
	grep -q '^ERROR: .*nothing was done' "$scratch/12a.out" || {
		cat "$scratch/12a.out"
		fail "deploy.sh $args: no 'nothing was done' refusal"
	}
	if grep -q 'docker stub' "$scratch/12a.out"; then fail "deploy.sh $args called docker before refusing"; fi
	echo "  $args -> exit $rc"
done
grep -q -- '--yes is not accepted' <(nrun --nuke --yes </dev/null 2>&1) || fail "a bare --yes is not refused by name"
echo "ok: each refused before any docker call"

echo "== 12b. --nuke: no deploy/.env, an empty DEPLOY_PROJECT, a dev stack's name, a different exported DEPLOY_PROJECT"
rm -f "$d/.env"
rc=0
nrun --nuke --yes-delete-everything launchpad </dev/null >"$scratch/12b1.out" 2>&1 || rc=$?
want_refused "$scratch/12b1.out" 'not found, so the project is unknown' "--nuke with no deploy/.env was not refused as unknown"
echo "  no deploy/.env -> exit $rc"
make_env x | sed "s/^DEPLOY_PROJECT=.*/DEPLOY_PROJECT=''/" >"$d/.env"
rc=0
nrun --nuke --yes-delete-everything '' </dev/null >"$scratch/12b2.out" 2>&1 || rc=$?
((rc != 0)) || fail "--nuke with an empty DEPLOY_PROJECT exited 0"
rc=0
nrun --nuke --non-interactive </dev/null >"$scratch/12b2.out" 2>&1 || rc=$?
want_refused "$scratch/12b2.out" 'DEPLOY_PROJECT is empty' "--nuke with an empty DEPLOY_PROJECT was not refused"
echo "  empty DEPLOY_PROJECT -> exit $rc"
# Only the stub here (no pass-through): a dev name never reaches docker.
for p in dev-ops dev-ops-orb1234 dev-ops-ci; do
	make_env "$p" >"$d/.env"
	rc=0
	nrun --nuke --yes-delete-everything "$p" </dev/null >"$scratch/12b3.out" 2>&1 || rc=$?
	want_refused "$scratch/12b3.out" "'$p' is a dev stack's project name" "--nuke accepted the dev stack name $p"
	if grep -q 'docker stub' "$scratch/12b3.out"; then fail "--nuke called docker for the dev name $p"; fi
	echo "  DEPLOY_PROJECT=$p -> exit $rc"
done
nuke_env "$NK1"
rc=0
run env DEPLOY_PROJECT="$NK2" "$d/deploy.sh" --nuke --yes-delete-everything "$NK1" </dev/null >"$scratch/12b4.out" 2>&1 || rc=$?
want_refused "$scratch/12b4.out" "DEPLOY_PROJECT '$NK2' in your shell is not the project '$NK1'" "an exported DEPLOY_PROJECT that differs from deploy/.env was not refused"
echo "  exported DEPLOY_PROJECT differs -> exit $rc"
echo "ok: unknown, empty, dev-stack and mismatched project names refused"

touch "$ctl/passthru"
mk_proj "$NK1"
mk_proj "$NK2"
# NK1's app container runs once (its command is only an echo) and leaves
# a fake setup-link line in its log.
"$real_docker" rm -f "$NK1-app-1" >/dev/null
"$real_docker" create --name "$NK1-app-1" --network "${NK1}_edge" \
	--label "com.docker.compose.project=$NK1" --label com.docker.compose.service=app \
	--label com.docker.compose.oneoff=False --label com.docker.compose.container-number=1 \
	--label com.docker.compose.config-hash=devops-test \
	--label "com.docker.compose.project.config_files=/srv/launchpad/deploy/docker-compose.yml" \
	-v "${NK1}_backups:/backups" --entrypoint /bin/sh "$img" \
	-c 'echo "{\"level\":\"WARN\",\"msg\":\"setup: open https://launchpad.test/setup#nukeFAKEsetupLINK (valid 24h; restart the app for a new link)\"}"' >/dev/null
"$real_docker" start -a "$NK1-app-1" >/dev/null
all_there "$NK1" || fail "setup: $NK1 is incomplete: $(proj_state "$NK1")"
all_there "$NK2" || fail "setup: $NK2 is incomplete: $(proj_state "$NK2")"

echo "== 12c. --nuke with no terminal and no --yes-delete-everything: refused, nothing removed"
nuke_env "$NK1"
rc=0
nrun --nuke </dev/null >"$scratch/12c.out" 2>&1 || rc=$?
want_refused "$scratch/12c.out" 'no terminal to confirm on' "--nuke without a terminal and without --yes-delete-everything was not refused"
all_there "$NK1" || fail "a refused --nuke removed something: $(proj_state "$NK1")"
for want in "${NK1}_postgres_data" 'the DATABASE' 'the ATTACHMENTS' 'the BACKUPS' "${NK1}_offsite_work" 'SCRATCH'; do
	grep -q -- "$want" "$scratch/12c.out" || {
		cat "$scratch/12c.out"
		fail "the plan does not name '$want'"
	}
done
no_secret "$scratch/12c.out"
echo "ok: exit $rc, the plan named what would go, nothing removed"

echo "== 12d. --yes-delete-everything with the wrong name (the other labelled project's): refused"
rc=0
nrun --nuke --yes-delete-everything "$NK2" </dev/null >"$scratch/12d.out" 2>&1 || rc=$?
want_refused "$scratch/12d.out" "yes-delete-everything '$NK2' is not the project '$NK1'" "a wrong --yes-delete-everything name was not refused"
all_there "$NK1" || fail "a wrong name removed something of $NK1: $(proj_state "$NK1")"
all_there "$NK2" || fail "a wrong name removed something of $NK2: $(proj_state "$NK2")"
echo "ok: exit $rc, both projects untouched"

echo "== 12e. interactive, the wrong name typed: refused"
rc=0
pty_run "${NK1}x" "$scratch/12e.out" env -u DEPLOY_PROJECT PATH="$scratch/bin:$PATH" "$d/deploy.sh" --nuke || rc=$?
want_refused "$scratch/12e.out" "the name typed is not '$NK1'" "a wrong typed confirmation was not refused"
all_there "$NK1" || fail "a wrong typed name removed something: $(proj_state "$NK1")"
rc=0
pty_run "" "$scratch/12e2.out" env -u DEPLOY_PROJECT PATH="$scratch/bin:$PATH" "$d/deploy.sh" --nuke || rc=$?
if ((rc == 0)) || ! all_there "$NK1"; then fail "an empty answer was accepted"; fi
echo "ok: exit $rc, nothing removed"

echo "== 12f. interactive, the right name typed: only $NK1 is removed; $NK2 is untouched; .env and images kept"
rc=0
pty_run "$NK1" "$scratch/12f.out" env -u DEPLOY_PROJECT PATH="$scratch/bin:$PATH" "$d/deploy.sh" --nuke || rc=$?
((rc == 0)) || {
	cat "$scratch/12f.out"
	fail "--nuke with the right name exited $rc"
}
none_left "$NK1" || {
	cat "$scratch/12f.out"
	fail "$NK1 is not fully removed: $(proj_state "$NK1")"
}
all_there "$NK2" || fail "--nuke of $NK1 touched $NK2: $(proj_state "$NK2")"
[[ -f "$d/.env" ]] || fail "--nuke without --nuke-env deleted deploy/.env"
grep -q 'kept; a re-run reuses its host names and secrets' "$scratch/12f.out" || fail "no 'deploy/.env kept' line"
grep -q 'the app log holds 1 setup link line' "$scratch/12f.out" || {
	cat "$scratch/12f.out"
	fail "the setup link in the app log was not mentioned"
}
if grep -q 'nukeFAKEsetupLINK' "$scratch/12f.out"; then fail "--nuke printed the setup link"; fi
no_secret "$scratch/12f.out"
image_there "$img" || fail "--nuke removed the postgres image"
echo "ok: exit 0, $NK1 gone (containers, network, volumes), $NK2 untouched, .env kept, setup link mentioned not printed"

echo "== 12g. an unlabelled volume with the stack's name: refused by the guard, nothing removed"
nuke_env "$NK3"
"$real_docker" volume create "${NK3}_postgres_data" >/dev/null
rc=0
nrun --nuke --yes-delete-everything "$NK3" </dev/null >"$scratch/12g.out" 2>&1 || rc=$?
want_refused "$scratch/12g.out" "already belongs to another stack.*${NK3}_postgres_data" "--nuke accepted an unlabelled volume"
"$real_docker" volume inspect "${NK3}_postgres_data" >/dev/null 2>&1 || fail "the refused --nuke removed the unlabelled volume"
"$real_docker" volume rm "${NK3}_postgres_data" >/dev/null
echo "ok: exit $rc, the unlabelled volume is still there"

echo "== 12h. --nuke-images --nuke-env, non-interactive: the images and deploy/.env go too"
mk_proj "$NK4" "nuketest-$$"
nuke_env "$NK4"
rc=0
nrun --nuke --nuke-images --nuke-env --yes-delete-everything "$NK4" </dev/null >"$scratch/12h.out" 2>&1 || rc=$?
((rc == 0)) || {
	cat "$scratch/12h.out"
	fail "--nuke --nuke-images --nuke-env exited $rc"
}
none_left "$NK4" || fail "$NK4 is not fully removed: $(proj_state "$NK4")"
if image_there "launchpad-api:nuketest-$$" || image_there "launchpad-web:nuketest-$$"; then fail "--nuke-images left an image"; fi
image_there "$img" || fail "--nuke-images removed the base image $img"
[[ ! -e "$d/.env" ]] || fail "--nuke-env left deploy/.env"
all_there "$NK2" || fail "--nuke of $NK4 touched $NK2: $(proj_state "$NK2")"
no_secret "$scratch/12h.out"
echo "ok: exit 0, both images and deploy/.env removed, base image and $NK2 untouched"

echo "== 12i. something outside the project holds a volume: exit non-zero, deploy/.env kept even with --nuke-env"
mk_proj "$NK5"
nuke_env "$NK5"
"$real_docker" create --name "$holder" --network none -v "${NK5}_backups:/b" --entrypoint /bin/sh "$img" -c true >/dev/null
rc=0
nrun --nuke --nuke-env --yes-delete-everything "$NK5" </dev/null >"$scratch/12i.out" 2>&1 || rc=$?
want_refused "$scratch/12i.out" "NOT fully removed.*${NK5}_backups" "--nuke exited 0 although ${NK5}_backups is left"
[[ -f "$d/.env" ]] || fail "--nuke-env deleted deploy/.env although a volume is left"
"$real_docker" rm -f "$holder" >/dev/null
echo "ok: exit $rc, named the volume left, kept deploy/.env"
echo "== 12j. --nuke-images when a container outside the project uses an image: kept, exit non-zero"
mk_proj "$NK6" "nuketest6-$$"
nuke_env "$NK6"
"$real_docker" create --name "$holder2" --network none --entrypoint /bin/sh "launchpad-web:nuketest6-$$" -c true >/dev/null
rc=0
nrun --nuke --nuke-images --nuke-env --yes-delete-everything "$NK6" </dev/null >"$scratch/12j.out" 2>&1 || rc=$?
want_refused "$scratch/12j.out" "image launchpad-web:nuketest6-$$ ([0-9a-f]*) is kept: container(s) outside the project use it: $holder2" \
	"an image another container uses was not kept by name"
image_there "launchpad-web:nuketest6-$$" || fail "--nuke-images removed an image that $holder2 uses"
if image_there "launchpad-api:nuketest6-$$"; then fail "--nuke-images kept the unused api image"; fi
"$real_docker" container inspect "$holder2" >/dev/null 2>&1 || fail "--nuke removed $holder2"
none_left "$NK6" || fail "$NK6 is not fully removed: $(proj_state "$NK6")"
[[ ! -e "$d/.env" ]] || fail "--nuke-env kept deploy/.env although every volume is gone"
"$real_docker" rm -f "$holder2" >/dev/null
echo "ok: exit $rc, the used image kept and named, the unused one removed, the project and deploy/.env gone"
echo "== 12k. --nuke-images after another deploy's build moved the tag: this project's image goes by id, the tag stays"
mk_proj "$NK7" "nuketest7-$$"
nuke_env "$NK7"
old7_api="$("$real_docker" inspect -f '{{.Image}}' "$NK7-app-1")"
old7_web="$("$real_docker" inspect -f '{{.Image}}' "$NK7-web-1")"
# "Another deploy" builds the same commit again: a new image takes the tag.
"$real_docker" create --name "$NK7-mkimg2" "$img" true >/dev/null
"$real_docker" commit "$NK7-mkimg2" "launchpad-api:nuketest7-$$" >/dev/null
"$real_docker" rm "$NK7-mkimg2" >/dev/null
new7_api="$("$real_docker" image ls -q --no-trunc "launchpad-api:nuketest7-$$")"
[[ "$new7_api" != "$old7_api" ]] || fail "setup: the tag did not move"
"$real_docker" create --name "$holder3" --network none --entrypoint /bin/sh "launchpad-api:nuketest7-$$" -c true >/dev/null
rc=0
nrun --nuke --nuke-images --yes-delete-everything "$NK7" </dev/null >"$scratch/12k.out" 2>&1 || rc=$?
((rc == 0)) || {
	cat "$scratch/12k.out"
	fail "--nuke-images after a moved tag exited $rc"
}
if image_there "$old7_api" || image_there "$old7_web"; then fail "--nuke-images left this project's image (by id)"; fi
[[ "$("$real_docker" image ls -q --no-trunc "launchpad-api:nuketest7-$$")" == "$new7_api" ]] ||
	fail "--nuke-images removed or moved the tag that now names another deploy's image"
"$real_docker" container inspect "$holder3" >/dev/null 2>&1 || fail "--nuke removed $holder3"
none_left "$NK7" || fail "$NK7 is not fully removed: $(proj_state "$NK7")"
"$real_docker" rm -f "$holder3" >/dev/null
echo "ok: exit 0, both of this project's images removed by id, the moved tag and its container untouched"
echo "== 12l. --nuke-images on a host deployed BEFORE the rename: the old dev-ops-api / dev-ops-web images go too"
mk_proj "$NK8" "nuketest8-$$" dev-ops
nuke_env "$NK8"
rc=0
nrun --nuke --nuke-images --yes-delete-everything "$NK8" </dev/null >"$scratch/12l.out" 2>&1 || rc=$?
((rc == 0)) || {
	cat "$scratch/12l.out"
	fail "--nuke-images on old-named images exited $rc"
}
none_left "$NK8" || fail "$NK8 is not fully removed: $(proj_state "$NK8")"
if image_there "dev-ops-api:nuketest8-$$" || image_there "dev-ops-web:nuketest8-$$"; then fail "--nuke-images left an old-named (dev-ops-*) image"; fi
grep -q "image dev-ops-api:nuketest8-$$ ([0-9a-f]*): removed" "$scratch/12l.out" || fail "no 'removed' line for the old api image"
grep -q "image dev-ops-web:nuketest8-$$ ([0-9a-f]*): removed" "$scratch/12l.out" || fail "no 'removed' line for the old web image"
image_there "$img" || fail "--nuke-images removed the base image $img"
no_secret "$scratch/12l.out"
echo "ok: exit 0, both old-named images removed, base image kept"

echo "== 12m. a mixed project (new-named api image, old-named web image): both go; an old-named image another container uses is kept"
mk_proj "$NK9" "nuketest9-$$" launchpad dev-ops
# The api image has the new name, the web image the old one; a container
# outside the project holds the OLD web image.
nuke_env "$NK9"
"$real_docker" create --name "$holder4" --network none --entrypoint /bin/sh "dev-ops-web:nuketest9-$$" -c true >/dev/null
rc=0
nrun --nuke --nuke-images --nuke-env --yes-delete-everything "$NK9" </dev/null >"$scratch/12m.out" 2>&1 || rc=$?
want_refused "$scratch/12m.out" "image dev-ops-web:nuketest9-$$ ([0-9a-f]*) is kept: container(s) outside the project use it: $holder4" \
	"an old-named image another container uses was not kept by name"
image_there "dev-ops-web:nuketest9-$$" || fail "--nuke-images removed an old-named image that $holder4 uses"
if image_there "launchpad-api:nuketest9-$$"; then fail "--nuke-images kept the unused new-named api image"; fi
"$real_docker" container inspect "$holder4" >/dev/null 2>&1 || fail "--nuke removed $holder4"
none_left "$NK9" || fail "$NK9 is not fully removed: $(proj_state "$NK9")"
"$real_docker" rm -f "$holder4" >/dev/null
echo "ok: exit $rc, the used old-named image kept and named, the unused new-named one removed"
rm -f "$ctl/passthru" "$d/.env"
all_there "$NK2" || fail "$NK2 changed during the --nuke cases: $(proj_state "$NK2")"

echo "== 13. --setup-link needs a terminal; --seed-tokens is gone"
make_env devops-test-env >"$d/.env"
chmod 600 "$d/.env"
set +e
run env -u DEPLOY_PROJECT "$d/deploy.sh" --setup-link </dev/null 2>"$scratch/13.err" | cat >"$scratch/13.out"
rc="${PIPESTATUS[0]}"
set -e
cat "$scratch/13.out" >>"$scratch/13.err"
want_refused "$scratch/13.err" 'needs a terminal; it refuses to write it to a pipe' "--setup-link wrote to a pipe"
if grep -q 'docker stub: refused' "$scratch/13.err"; then fail "--setup-link called docker before the terminal check"; fi
rc=0
run env -u DEPLOY_PROJECT "$d/deploy.sh" --seed-tokens </dev/null >"$scratch/13b.out" 2>&1 || rc=$?
want_refused "$scratch/13b.out" '--seed-tokens is gone.*--setup-link' "--seed-tokens did not name its replacement"
rm -f "$d/.env"
echo "ok: a piped --setup-link refused before docker; --seed-tokens names --setup-link"

echo "== 14. --mint-token needs its arguments and a terminal"
make_env devops-test-env >"$d/.env"
chmod 600 "$d/.env"
rc=0
run env -u DEPLOY_PROJECT "$d/deploy.sh" --mint-token </dev/null >"$scratch/14a.out" 2>&1 || rc=$?
((rc != 0)) || fail "--mint-token with no arguments exited 0"
want_refused "$scratch/14a.out" '--mint-token needs the mint-token flags after it: --org <slug> --user <username>' "--mint-token with no arguments did not give the usage"
if grep -q 'docker stub: refused' "$scratch/14a.out"; then fail "--mint-token with no arguments called docker"; fi
set +e
run env -u DEPLOY_PROJECT "$d/deploy.sh" --mint-token --org orbit --user claude --name mcp --scopes read,write --expires-days 90 \
	</dev/null 2>"$scratch/14b.err" | cat >"$scratch/14b.out"
rc="${PIPESTATUS[0]}"
set -e
((rc != 0)) || fail "a piped --mint-token exited 0"
cat "$scratch/14b.out" >>"$scratch/14b.err"
want_refused "$scratch/14b.err" '--mint-token prints a credential and needs a terminal; it refuses to write it to a pipe' "--mint-token wrote to a pipe"
if grep -q 'docker stub: refused' "$scratch/14b.err"; then fail "a piped --mint-token called docker before the terminal check"; fi
rc=0
run env -u DEPLOY_PROJECT "$d/deploy.sh" --mint-token --org orbit --user claude --name mcp --scopes read --expires-days 1 \
	</dev/null >"$scratch/14c.out" 2>"$scratch/14c.err" || rc=$?
((rc != 0)) || fail "--mint-token into a file exited 0"
want_refused "$scratch/14c.err" '--mint-token prints a credential and needs a terminal' "--mint-token wrote to a file"
if grep -q 'docker stub: refused' "$scratch/14c.err"; then fail "--mint-token into a file called docker before the terminal check"; fi
rm -f "$d/.env"
echo "ok: --mint-token with no arguments gives the usage; piped and to a file it is refused before docker"

echo "== 15. the checkout was pulled by hand: a new compose file needs a key the old .env lacks"
# GNU stat first: on Linux `stat -f` means filesystem status and exits 0.
mode_of() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }
# up15: postgres and backup read as running, and `backup.sh once` succeeds.
up15() { touch "$ctl/running_postgres" "$ctl/running_backup" "$ctl/exec_ok" "$ctl/require_settings_key"; }
down15() { rm -f "$ctl"/running_* "$ctl/exec_ok" "$ctl/require_settings_key"; }
# check15 NAME: the key was appended as the last line, nothing else changed
# (against $scratch/NAME.before), 0600, and no value of it was printed.
check15() {
	local n="$1" kl
	kl="$(tail -n 1 "$d/.env")"
	[[ "$kl" =~ ^SETTINGS_ENCRYPTION_KEY=\'[0-9a-f]{64}\'$ ]] || fail "$n: the appended key is not 64 hex characters"
	head -n "$(($(wc -l <"$d/.env") - 1))" "$d/.env" | cmp -s - "$scratch/$n.before" || fail "$n: other lines of deploy/.env changed"
	[[ "$(mode_of "$d/.env")" == 600 ]] || fail "$n: deploy/.env is not mode 0600"
	if grep -qF "${kl#SETTINGS_ENCRYPTION_KEY=}" "$scratch/$n.out"; then fail "$n: deploy.sh printed the generated key"; fi
	if grep -q 'missing a value' "$scratch/$n.out"; then
		cat "$scratch/$n.out"
		fail "$n: compose still failed to interpolate"
	fi
	grep -q 'added the keys that .* lacked: SETTINGS_ENCRYPTION_KEY' "$scratch/$n.out" || fail "$n: the added key is not named"
}
make_env devops-test-env | grep -v '^SETTINGS_ENCRYPTION_KEY=' >"$d/.env"
chmod 600 "$d/.env"
cp "$d/.env" "$scratch/15a.before"
up15
run env STUB_VOLUMES="devops-test-env_postgres_data" STUB_VOLUMES_LABEL=production \
	"$d/deploy.sh" --update --non-interactive </dev/null >"$scratch/15a.out" 2>&1 || true
down15
check15 15a
grep -q 'docker stub: compose exec ok: .* backup /bin/sh /backup/backup.sh once' "$scratch/15a.out" || {
	cat "$scratch/15a.out"
	fail "--update did not take its backup"
}
if grep -q 'the backup before the update failed' "$scratch/15a.out"; then fail "--update reported a failed backup"; fi
echo "ok: --update added the key, then took the backup; values unchanged, 0600, key not printed"

make_env devops-test-env | grep -v '^SETTINGS_ENCRYPTION_KEY=' >"$d/.env"
chmod 600 "$d/.env"
cp "$d/.env" "$scratch/15b.before"
up15
rc=0
run env STUB_VOLUMES="devops-test-env_postgres_data" STUB_VOLUMES_LABEL=production \
	"$d/deploy.sh" --backup-now --non-interactive </dev/null >"$scratch/15b.out" 2>&1 || rc=$?
down15
((rc == 0)) || {
	cat "$scratch/15b.out"
	fail "--backup-now failed (exit $rc)"
}
check15 15b
grep -q 'docker stub: compose exec ok: .* backup /bin/sh /backup/backup.sh once' "$scratch/15b.out" || fail "--backup-now did not run the backup"
echo "ok: --backup-now added the key, then took the backup"

echo "-- 15c. no final newline in the old .env"
printf '%s' "$(make_env devops-test-env | grep -v '^SETTINGS_ENCRYPTION_KEY=')" >"$d/.env"
chmod 600 "$d/.env"
[[ "$(tail -c 1 "$d/.env" | wc -l | tr -d ' ')" == 0 ]] || fail "15c: could not make a file with no final newline"
{ cat "$d/.env"; printf '\n'; } >"$scratch/15c.before"
up15
rc=0
run env STUB_VOLUMES="devops-test-env_postgres_data" STUB_VOLUMES_LABEL=production \
	"$d/deploy.sh" --backup-now --non-interactive </dev/null >"$scratch/15c.out" 2>&1 || rc=$?
down15
((rc == 0)) || {
	cat "$scratch/15c.out"
	fail "--backup-now failed on a file with no final newline (exit $rc)"
}
check15 15c
echo "ok: the key went on its own line"

echo "-- 15d. --smoke tops up too; a complete .env stays byte-identical"
make_env devops-test-env | grep -v '^SETTINGS_ENCRYPTION_KEY=' >"$d/.env"
chmod 600 "$d/.env"
cp "$d/.env" "$scratch/15d.before"
up15
run env STUB_VOLUMES="devops-test-env_postgres_data" STUB_VOLUMES_LABEL=production \
	"$d/deploy.sh" --smoke --non-interactive </dev/null >"$scratch/15d.out" 2>&1 || true
down15
check15 15d
make_env devops-test-env >"$d/.env"
chmod 600 "$d/.env"
before="$(sum "$d/.env")"
up15
run env STUB_VOLUMES="devops-test-env_postgres_data" STUB_VOLUMES_LABEL=production \
	"$d/deploy.sh" --backup-now --non-interactive </dev/null >"$scratch/15e.out" 2>&1 || true
down15
[[ "$(sum "$d/.env")" == "$before" ]] || fail "15e: a complete deploy/.env changed"
if grep -q 'added the keys' "$scratch/15e.out"; then fail "15e: it said it added keys to a complete file"; fi
rm -f "$d/.env"
echo "ok: --smoke adds the key; a complete .env is untouched and silent"

echo "-- 15f. an EMPTY key is not 'present' for compose; a malformed key: --backup-now and restore.sh warn, --update refuses"
make_env devops-test-env | sed "s/^SETTINGS_ENCRYPTION_KEY=.*/SETTINGS_ENCRYPTION_KEY=''/" >"$d/.env"
chmod 600 "$d/.env"
up15
rc=0
run env STUB_VOLUMES="devops-test-env_postgres_data" STUB_VOLUMES_LABEL=production \
	"$d/deploy.sh" --backup-now --non-interactive </dev/null >"$scratch/15f.out" 2>&1 || rc=$?
down15
grep -q 'missing a value' "$scratch/15f.out" || fail "15f: the stub accepted an empty key"
make_env devops-test-env | sed "s/^SETTINGS_ENCRYPTION_KEY=.*/SETTINGS_ENCRYPTION_KEY='not-hex-planted-15g'/" >"$d/.env"
chmod 600 "$d/.env"
before="$(sum "$d/.env")"
up15
rc=0
run env STUB_VOLUMES="devops-test-env_postgres_data" STUB_VOLUMES_LABEL=production \
	"$d/deploy.sh" --backup-now --non-interactive </dev/null >"$scratch/15g.out" 2>&1 || rc=$?
((rc == 0)) || {
	cat "$scratch/15g.out"
	fail "15g: --backup-now was blocked by a malformed SETTINGS_ENCRYPTION_KEY (exit $rc)"
}
grep -q 'WARNING: SETTINGS_ENCRYPTION_KEY .* is not 64 hex characters' "$scratch/15g.out" || fail "15g: no warning"
grep -q 'backup.sh once' "$scratch/15g.out" || fail "15g: the backup did not run"
if grep -q 'not-hex-planted-15g' "$scratch/15g.out"; then fail "15g: the key was echoed"; fi
rc=0
run env STUB_VOLUMES="devops-test-env_postgres_data" STUB_VOLUMES_LABEL=production \
	"$d/deploy.sh" --update --non-interactive </dev/null >"$scratch/15h.out" 2>&1 || rc=$?
down15
((rc != 0)) || fail "15h: --update accepted a malformed key"
grep -q 'SETTINGS_ENCRYPTION_KEY must be exactly 64 hex characters' "$scratch/15h.out" || fail "15h: no refusal by name"
if grep -q 'backup.sh once' "$scratch/15h.out"; then fail "15h: --update backed up before it refused"; fi
[[ "$(sum "$d/.env")" == "$before" ]] || fail "15g/h: deploy/.env changed"
echo "ok: empty key is missing to compose; --backup-now warns and backs up; --update refuses before any backup"

echo "-- 15i. restore.sh (stack mode) tops up too"
make_env devops-test-env | grep -v '^SETTINGS_ENCRYPTION_KEY=' >"$d/.env"
chmod 600 "$d/.env"
cp "$d/.env" "$scratch/15i.before"
up15
rc=0
run "$d/restore.sh" latest </dev/null >"$scratch/15i.out" 2>&1 || rc=$?
down15
check15 15i
if grep -q 'the backup service is not running' "$scratch/15i.out"; then
	cat "$scratch/15i.out"
	fail "15i: restore.sh blamed the backup service (compose failed to interpolate)"
fi
grep -q 'no dump in the backups volume' "$scratch/15i.out" || {
	cat "$scratch/15i.out"
	fail "15i: restore.sh did not get past the backup-service check"
}
make_env devops-test-env | sed "s/^SETTINGS_ENCRYPTION_KEY=.*/SETTINGS_ENCRYPTION_KEY='not-hex-planted-15i'/" >"$d/.env"
chmod 600 "$d/.env"
up15
run "$d/restore.sh" latest </dev/null >"$scratch/15i2.out" 2>&1 || true
down15
grep -q 'WARNING: SETTINGS_ENCRYPTION_KEY .* is not 64 hex characters' "$scratch/15i2.out" || fail "15i: restore.sh gave no warning for a malformed key"
grep -q 'no dump in the backups volume' "$scratch/15i2.out" || fail "15i: restore.sh was blocked by a malformed key"
echo "ok: restore.sh added the key and reached the dump lookup"

echo "-- 15j. --setup-link and --mint-token on a pseudo-terminal top up too"
for m in setup-link mint-token; do
	make_env devops-test-env | grep -v '^SETTINGS_ENCRYPTION_KEY=' >"$d/.env"
	chmod 600 "$d/.env"
	cp "$d/.env" "$scratch/15j-$m.before"
	up15
	touch "$ctl/running_app"
	margs=()
	[[ "$m" == mint-token ]] && margs=(--org orbit --user claude --name mcp --scopes read)
	rc=0
	pty_run "" "$scratch/15j-$m.out" env PATH="$scratch/bin:$PATH" DEPLOY_WEB_PORT=47491 DEPLOY_FILES_PORT=47492 \
		STUB_VOLUMES="devops-test-env_postgres_data" STUB_VOLUMES_LABEL=production \
		"$d/deploy.sh" "--$m" ${margs[@]+"${margs[@]}"} || rc=$?
	down15
	rm -f "$ctl/running_app"
	((rc == 0)) || {
		cat "$scratch/15j-$m.out"
		fail "15j: --$m failed (exit $rc)"
	}
	cp "$scratch/15j-$m.before" "$scratch/15j.before"
	cp "$scratch/15j-$m.out" "$scratch/15j.out"
	check15 15j
done
echo "ok: both reached their compose exec after adding the key"

echo "== 16. first run: the questions"
# pty_ask OUT ANSWERS CMD...: CMD on a pseudo-terminal. Each prompt that
# ends in "]: " gets the next line of ANSWERS (an empty line is Enter; the
# line <EOF> sends end of input).
# More prompts than answers: the run is killed and fails. Exit: CMD's.
pty_ask() {
	python3 -c '
import os, pty, select, signal, sys
out, answers, cmd = sys.argv[1], sys.argv[2].split("\n"), sys.argv[3:]
pid, fd = pty.fork()
if pid == 0:
    os.execvp(cmd[0], cmd)
buf, mark, n = b"", 0, 0
while True:
    r, _, _ = select.select([fd], [], [], 30)
    if not r:
        os.kill(pid, signal.SIGKILL)
        break
    try:
        data = os.read(fd, 4096)
    except OSError:
        break
    if not data:
        break
    buf += data
    if buf[mark:].endswith(b"]: "):
        mark = len(buf)
        if n >= len(answers):
            os.kill(pid, signal.SIGKILL)
            break
        if answers[n] == "<EOF>":
            os.write(fd, b"\x04")
        else:
            os.write(fd, answers[n].encode() + b"\n")
        n += 1
_, st = os.waitpid(pid, 0)
open(out, "wb").write(buf)
sys.exit(os.waitstatus_to_exitcode(st))
' "$@"
}
# first16 OUT ANSWERS [VAR=VALUE...]: a FIRST run on a pty (F16_ARGS: more
# deploy.sh arguments).
first16() {
	local out="$1" ans="$2"
	shift 2
	rm -f "$d/.env"
	pty_ask "$out" "$ans" env -u DEPLOY_WEB_PORT -u DEPLOY_FILES_PORT -u WEB_HOST -u FILES_HOST -u DEPLOY_BIND_ADDR \
		-u POSTMARK_SERVER_TOKEN -u POSTMARK_FROM PATH="$scratch/bin:$PATH" DEPLOY_PROJECT=devops-test-env "$@" "$d/deploy.sh" ${F16_ARGS:+"$F16_ARGS"} || true
}
want_line() { grep -qx -- "$2" "$1/.env" || fail "$3: deploy/.env lacks the line $2"; }
count_in() { grep -c -- "$2" "$1" || true; }
no_postmark_q() {
	if grep -q 'Postmark server token\|Postmark sender\|login is OFF' "$1"; then
		cat "$1"
		fail "$2: a Postmark question or the old warning appeared"
	fi
}

echo "-- 16a. the hosts have no default; the ports and the bind address do"
# Enter at the bind address, an empty host (refused, asked again), then two hosts and two Enters.
first16 "$scratch/16a.out" $'\n\nweb.example.com\nfiles.example.com\n\n'
for p in 'Bind address \[127.0.0.1\]: ' 'Web host \[required\]: ' 'Files host \[required\]: ' 'Web port \[4001\]: ' 'Files port \[4002\]: '; do
	grep -q -- "$p" "$scratch/16a.out" || {
		cat "$scratch/16a.out"
		fail "16a: no prompt $p"
	}
done
(("$(count_in "$scratch/16a.out" 'Try again')" == 1)) || fail "16a: an empty host was not refused exactly once"
want_line "$d" "DEPLOY_BIND_ADDR='127.0.0.1'" 16a
want_line "$d" "WEB_HOST='web.example.com'" 16a
want_line "$d" "FILES_HOST='files.example.com'" 16a
want_line "$d" "DEPLOY_WEB_PORT='4001'" 16a
want_line "$d" "DEPLOY_FILES_PORT='4002'" 16a
no_postmark_q "$scratch/16a.out" 16a
grep -q 'Platform page' "$scratch/16a.out" || fail "16a: no pointer to the Platform page"
echo "ok: no host default, ports and bind address have one, no Postmark question"

echo "-- 16b. bad answers are asked again"
# bad address, then good; bad host, then good; files host = web host, then
# good; port "abc", port 0, then good; files port = web port, then good.
first16 "$scratch/16b.out" $'999.1.1.1\n10.1.2.3\nBad_Host\nweb.example.com\nweb.example.com\nfiles.example.com\nabc\n0\n047491\n47491\n47491\n47492'
(("$(count_in "$scratch/16b.out" 'Try again')" == 7)) || {
	cat "$scratch/16b.out"
	fail "16b: want 7 'Try again' lines"
}
(("$(count_in "$scratch/16b.out" ']: ')" == 12)) || fail "16b: not 12 prompts"
want_line "$d" "DEPLOY_BIND_ADDR='10.1.2.3'" 16b
want_line "$d" "WEB_HOST='web.example.com'" 16b
want_line "$d" "FILES_HOST='files.example.com'" 16b
want_line "$d" "DEPLOY_WEB_PORT='47491'" 16b
want_line "$d" "DEPLOY_FILES_PORT='47492'" 16b
echo "ok: 7 bad answers refused and asked again; the good values written"

echo "-- 16c. a value in the environment skips its question"
first16 "$scratch/16c.out" $'\nfiles.example.com' WEB_HOST=env.example.com DEPLOY_WEB_PORT=47491 DEPLOY_FILES_PORT=47492
(("$(count_in "$scratch/16c.out" ']: ')" == 2)) || {
	cat "$scratch/16c.out"
	fail "16c: want 2 prompts (bind address, files host)"
}
for p in 'Web host' 'Web port' 'Files port'; do
	if grep -q -- "$p" "$scratch/16c.out"; then fail "16c: asked '$p' with the value in the environment"; fi
done
want_line "$d" "WEB_HOST='env.example.com'" 16c
want_line "$d" "DEPLOY_WEB_PORT='47491'" 16c
echo "ok: three settings from the environment, two questions only"

echo "-- 16d. --non-interactive on a terminal asks nothing; Postmark comes from the environment"
F16_ARGS=--non-interactive first16 "$scratch/16d.out" "" POSTMARK_SERVER_TOKEN=pm-test-token-16d POSTMARK_FROM=t@example.com WEB_HOST=web.example.com FILES_HOST=files.example.com DEPLOY_WEB_PORT=47491 DEPLOY_FILES_PORT=47492
(("$(count_in "$scratch/16d.out" ']: ')" == 0)) || fail "16d: --non-interactive asked a question"
want_line "$d" "POSTMARK_FROM='t@example.com'" 16d
grep -q "^POSTMARK_SERVER_TOKEN='pm-test-token-16d'" "$d/.env" || fail "16d: the token from the environment was not kept"
if grep -q 'pm-test-token-16d' "$scratch/16d.out"; then fail "16d: the token was printed"; fi
echo "ok: no question; POSTMARK_* read from the environment; token not printed"

echo "-- 16e. a re-run with a deploy/.env asks nothing"
cp "$d/.env" "$scratch/16e.before"
pty_ask "$scratch/16e.out" "" env PATH="$scratch/bin:$PATH" "$d/deploy.sh" || true
(("$(count_in "$scratch/16e.out" ']: ')" == 0)) || fail "16e: a re-run asked a question"
cmp -s "$d/.env" "$scratch/16e.before" || fail "16e: the re-run changed deploy/.env"
rm -f "$d/.env"
echo "ok: no question on a re-run"

echo "-- 16e2. an existing deploy/.env without the hosts: refused, nothing appended"
F16_ARGS=--non-interactive first16 "$scratch/16e2.seed" "" WEB_HOST=web.example.com FILES_HOST=files.example.com DEPLOY_WEB_PORT=47493 DEPLOY_FILES_PORT=47494
grep -v '^\(WEB_HOST\|FILES_HOST\)=' "$d/.env" >"$scratch/16e2.env"
cp "$scratch/16e2.env" "$d/.env"
chmod 600 "$d/.env"
cp "$d/.env" "$scratch/16e2.before"
if env -u WEB_HOST -u FILES_HOST PATH="$scratch/bin:$PATH" DEPLOY_PROJECT=devops-test-env "$d/deploy.sh" --non-interactive >"$scratch/16e2.out" 2>&1; then
	fail "16e2: a .env without the hosts was accepted"
fi
grep -q 'WEB_HOST is missing from' "$scratch/16e2.out" || {
	cat "$scratch/16e2.out"
	fail "16e2: no 'WEB_HOST is missing' message"
}
cmp -s "$d/.env" "$scratch/16e2.before" || fail "16e2: the refused run changed deploy/.env"
if grep -q '^\(WEB_HOST\|FILES_HOST\)=' "$d/.env"; then fail "16e2: an empty host line was written"; fi
# A host set in the environment is appended (and checked).
env WEB_HOST=web.example.com FILES_HOST=files.example.com PATH="$scratch/bin:$PATH" DEPLOY_PROJECT=devops-test-env "$d/deploy.sh" --non-interactive >"$scratch/16e2b.out" 2>&1 || true
want_line "$d" "WEB_HOST='web.example.com'" 16e2
rm -f "$d/.env"
echo "ok: an existing .env lacking the hosts is refused before anything is appended"

echo "-- 16f. the first question of a pair is not checked against the other default"
# Web port 4002 (the files default) and then files port 4003 are fine.
first16 "$scratch/16f.out" $'\nweb.example.com\nfiles.example.com\n4002\n4003'
(("$(count_in "$scratch/16f.out" 'Try again')" == 0)) || {
	cat "$scratch/16f.out"
	fail "16f: 4002 then 4003 was refused"
}
want_line "$d" "DEPLOY_WEB_PORT='4002'" 16f
want_line "$d" "DEPLOY_FILES_PORT='4003'" 16f
# Web host = a name used later as the files host elsewhere, then another files host: fine.
first16 "$scratch/16f2.out" $'\nfiles.example.com\nother.example.com\n\n'
(("$(count_in "$scratch/16f2.out" 'Try again')" == 0)) || fail "16f: web host = the files default was refused"
want_line "$d" "WEB_HOST='files.example.com'" 16f
# The same port twice: refused at the second question, asked again.
first16 "$scratch/16f3.out" $'\nweb.example.com\nfiles.example.com\n4001\n4001\n4002'
(("$(count_in "$scratch/16f3.out" 'Try again')" == 1)) || fail "16f: 4001 twice was not refused exactly once"
grep -q 'DEPLOY_FILES_PORT and DEPLOY_WEB_PORT must differ' "$scratch/16f3.out" || fail "16f: no 'must differ' at the second question"
want_line "$d" "DEPLOY_FILES_PORT='4002'" 16f
echo "ok: 4002/4003 and a web host equal to the files default accepted; 4001 twice refused at the second question"

echo "-- 16g. end of input at a prompt stops the run and leaves no file"
rm -f "$d/.env"
rc=0
pty_ask "$scratch/16g.out" $'\n<EOF>' env -u DEPLOY_WEB_PORT -u DEPLOY_FILES_PORT -u WEB_HOST -u FILES_HOST -u DEPLOY_BIND_ADDR \
	PATH="$scratch/bin:$PATH" DEPLOY_PROJECT=devops-test-env "$d/deploy.sh" || rc=$?
((rc != 0)) || fail "16g: end of input exited 0"
grep -q 'no answer for Web host' "$scratch/16g.out" || {
	cat "$scratch/16g.out"
	fail "16g: no 'no answer for' message"
}
[[ -z "$(env_files "$d")" ]] || fail "16g: left$(env_files "$d")"
echo "ok: exit $rc, 'no answer for' message, no .env and no temp file"

echo "== 8. valid_ipv4"
# shellcheck source=deploy/lib.sh
source "$here/lib.sh"
for a in 127.0.0.1 0.0.0.0 10.20.30.40 255.255.255.255; do
	valid_ipv4 "$a" || fail "valid_ipv4 rejected $a"
done
for a in 010.0.0.1 1.2.3.04 256.1.1.1 1.2.3 1.2.3.4.5 a.b.c.d ""; do
	if valid_ipv4 "$a"; then fail "valid_ipv4 accepted '$a'"; fi
done
echo "ok: 4 valid accepted, 7 invalid rejected"

echo "== 17. first run ends with the setup link, on a terminal only"
# GitLab CI exports CI=true, which suppresses the link: start from a clean slate.
unset CI DEPLOY_NO_SETUP_LINK
make_env devops-test-env >"$d/.env"
chmod 600 "$d/.env"
touch "$ctl/running_app" "$ctl/running_postgres"
# summary17 MODE OUT: deploy.sh's summary() in MODE with stdout on a
# pseudo-terminal (OUT is its screen), a pipe or a file.
s17_cmd() {
	# shellcheck disable=SC2016  # expanded by the inner bash
	printf '%s' 'cd "$1" && set -- $2 && DEPLOY_SH_SOURCE_ONLY=1 source ./deploy.sh && load_env && DEPLOY_VERSION=test && summary'
}
s17() { # KIND ARG [VAR=VALUE...]: ARG is deploy.sh's argument ("deploy" = none; --update is parsed by the real code)
	local kind="$1" m="$2" out="$scratch/17-$1-$2.out"
	shift 2
	[[ "$m" == deploy ]] && m=""
	: >"$ctl/calls.log"
	rc=0
	case "$kind" in
	tty) pty_run "" "$out" env PATH="$scratch/bin:$PATH" DEPLOY_PROJECT=devops-test-env "$@" bash -c "$(s17_cmd)" _ "$d" "$m" || rc=$? ;;
	pipe) env PATH="$scratch/bin:$PATH" DEPLOY_PROJECT=devops-test-env "$@" bash -c "$(s17_cmd)" _ "$d" "$m" </dev/null 2>&1 | cat >"$out" || rc=$? ;;
	file) env PATH="$scratch/bin:$PATH" DEPLOY_PROJECT=devops-test-env "$@" bash -c "$(s17_cmd)" _ "$d" "$m" </dev/null >"$out" 2>&1 || rc=$? ;;
	esac
	((rc == 0)) || {
		cat "$out"
		fail "17: summary ($kind, $m) exited $rc"
	}
}
no_link() { # OUT WHY
	if grep -q 'STUBSETUPLINKSECRET' "$1"; then fail "17: the link is in $1 ($2)"; fi
	if grep -q 'setup-link' "$ctl/calls.log"; then fail "17: server setup-link ran ($2)"; fi
}
printf 'yes\n' >"$ctl/setup_state"
s17 tty deploy
grep -q 'STUBSETUPLINKSECRET' "$scratch/17-tty-deploy.out" || fail "17a: no link on a terminal"
grep -q 'Valid 24 hours, single use' "$scratch/17-tty-deploy.out" || fail "17a: no validity line"
grep -q 'prints a NEW link' "$scratch/17-tty-deploy.out" || fail "17a: no re-run line"
[[ "$(grep -v '^[[:space:]]*$' "$scratch/17-tty-deploy.out" | tail -n 8 | grep -c 'STUBSETUPLINKSECRET\|Valid 24 hours')" == 2 ]] || fail "17a: the link is not at the end"
echo "ok: a first run on a terminal ends with the link"
for kind in pipe file; do
	s17 "$kind" deploy
	no_link "$scratch/17-$kind-deploy.out" "$kind"
	grep -q 'FIRST RUN: setup is open' "$scratch/17-$kind-deploy.out" || fail "17b: no hint on a $kind"
	grep -q 'deploy/deploy.sh --setup-link' "$scratch/17-$kind-deploy.out" || fail "17b: hint lacks the command on a $kind"
done
echo "ok: on a pipe and in a file: no link, the hint, exit 0"
s17 tty --update
no_link "$scratch/17-tty---update.out" "--update"
grep -q 'FIRST RUN: setup is open' "$scratch/17-tty---update.out" || fail "17c: no hint for --update"
echo "ok: --update on a terminal gives the hint, no link"
# The CI and DEPLOY_NO_SETUP_LINK guards: a pty is not enough.
for g in CI=true CI=1 CI=yes DEPLOY_NO_SETUP_LINK=1; do
	: >"$ctl/calls.log"
	s17 tty deploy "$g"
	cp "$scratch/17-tty-deploy.out" "$scratch/17-guard.out"
	no_link "$scratch/17-guard.out" "$g on a pty"
	grep -q 'FIRST RUN: setup is open' "$scratch/17-guard.out" || fail "17h: no hint with $g"
	grep -q 'No setup link is printed: ' "$scratch/17-guard.out" || fail "17h: no reason with $g"
done
grep -q 'DEPLOY_NO_SETUP_LINK=1' "$scratch/17-guard.out" || fail "17h: the reason is not named"
# CI false, 0 and empty do not block; the guard variable other than 1 does not.
for g in CI=false CI=0 CI= DEPLOY_NO_SETUP_LINK=0; do
	s17 tty deploy "$g"
	grep -q 'STUBSETUPLINKSECRET' "$scratch/17-tty-deploy.out" || fail "17i: $g blocked the link"
done
echo "ok: CI set or DEPLOY_NO_SETUP_LINK=1 on a pty: no link, the hint with the reason; CI false/0/empty: link"
printf 'no\n' >"$ctl/setup_state"
s17 tty deploy
no_link "$scratch/17-tty-deploy.out" "setup done"
if grep -q 'FIRST RUN' "$scratch/17-tty-deploy.out"; then fail "17d: FIRST RUN shown after setup"; fi
echo "ok: setup done: no link, no first-run text"
rm -f "$ctl/setup_state"
s17 tty deploy
no_link "$scratch/17-tty-deploy.out" "unknown"
grep -q 'could not ask the database whether first-run setup is done' "$scratch/17-tty-deploy.out" || fail "17e: unknown state not said"
echo "ok: unknown state: said, no link"
printf 'yes\n' >"$ctl/setup_state"
printf '1\n' >"$ctl/setup_rc"
s17 tty deploy
grep -q 'could not make the setup link' "$scratch/17-tty-deploy.out" || fail "17f: no warning when setup-link fails"
grep -q 'deploy/deploy.sh --setup-link' "$scratch/17-tty-deploy.out" || fail "17f: no hint when setup-link fails"
printf '3\n' >"$ctl/setup_rc"
s17 tty deploy
grep -q 'setup is complete' "$scratch/17-tty-deploy.out" || fail "17g: exit 3 not explained"
echo "ok: a failing or finished setup-link does not fail the deploy"
rm -f "$ctl/setup_state" "$ctl/setup_rc" "$ctl/running_app" "$ctl/running_postgres" "$ctl/calls.log"
rm -f "$d/.env"

# ---------------------------------------------------------------- 18. off-site backups
echo "== 18a. re-run, BACKUP_ENCRYPTION_KEY and OFFSITE_DB_PASSWORD missing (an install from before off-site backups)"
make_env devops-test-env | grep -v -e '^BACKUP_ENCRYPTION_KEY=' -e '^OFFSITE_DB_PASSWORD=' >"$d/.env"
chmod 600 "$d/.env"
cp "$d/.env" "$scratch/18a.before"
run "$d/deploy.sh" --non-interactive </dev/null >"$scratch/18a.out" 2>&1 || true
grep -q 'added: BACKUP_ENCRYPTION_KEY OFFSITE_DB_PASSWORD' "$scratch/18a.out" || {
	cat "$scratch/18a.out"
	fail "18a: the two keys were not added and named"
}
bk="$(grep '^BACKUP_ENCRYPTION_KEY=' "$d/.env")"
op="$(grep '^OFFSITE_DB_PASSWORD=' "$d/.env")"
[[ "$bk" =~ ^BACKUP_ENCRYPTION_KEY=\'[0-9a-f]{64}\'$ ]] || fail "18a: the appended BACKUP_ENCRYPTION_KEY is not 64 hex characters"
[[ "$op" =~ ^OFFSITE_DB_PASSWORD=\'[0-9a-f]{64}\'$ ]] || fail "18a: the appended OFFSITE_DB_PASSWORD is not 64 hex characters"
[[ "${bk#BACKUP_ENCRYPTION_KEY=}" != "${op#OFFSITE_DB_PASSWORD=}" ]] || fail "18a: the two values are equal"
head -n "$(($(wc -l <"$d/.env") - 2))" "$d/.env" | cmp -s - "$scratch/18a.before" || fail "18a: other lines of deploy/.env changed"
[[ "$(mode_of "$d/.env")" == 600 ]] || fail "18a: deploy/.env is not mode 0600"
for v in "${bk#BACKUP_ENCRYPTION_KEY=}" "${op#OFFSITE_DB_PASSWORD=}"; do
	if grep -qF "${v//\'/}" "$scratch/18a.out"; then fail "18a: deploy.sh printed a generated value"; fi
done
grep -q 'docker stub: refused' "$scratch/18a.out" || fail "18a: the run did not stop at the docker stub"
# A second run keeps both byte for byte: they are never made up again.
cp "$d/.env" "$scratch/18a.after"
run "$d/deploy.sh" --non-interactive </dev/null >"$scratch/18a2.out" 2>&1 || true
cmp -s "$d/.env" "$scratch/18a.after" || fail "18a: a re-run changed deploy/.env (a key was generated again?)"
if grep -q 'added:' "$scratch/18a2.out"; then fail "18a: a re-run added a key again"; fi
echo "ok: two 64-hex values appended, not printed, the rest unchanged; a re-run keeps them byte for byte"

echo "== 18b. malformed BACKUP_ENCRYPTION_KEY, OFFSITE_DB_PASSWORD or BACKUP_ALLOW_PRIVATE_CIDRS: refused by name, never echoed"
for case18b in "BACKUP_ENCRYPTION_KEY|not-hex-planted-18b|BACKUP_ENCRYPTION_KEY must be exactly 64 hex characters" \
	"OFFSITE_DB_PASSWORD|short-planted-18b|OFFSITE_DB_PASSWORD must be 32 to 128 letters and digits" \
	"BACKUP_ALLOW_PRIVATE_CIDRS|0.0.0.0/0|BACKUP_ALLOW_PRIVATE_CIDRS is not a comma list of CIDRs" \
	"BACKUP_ALLOW_PRIVATE_CIDRS|10.0.0.0/4|BACKUP_ALLOW_PRIVATE_CIDRS is not a comma list of CIDRs" \
	"BACKUP_ALLOW_PRIVATE_CIDRS|127.0.0.0/8|BACKUP_ALLOW_PRIVATE_CIDRS is not a comma list of CIDRs" \
	"BACKUP_ALLOW_PRIVATE_CIDRS|192.168.1.20|BACKUP_ALLOW_PRIVATE_CIDRS is not a comma list of CIDRs" \
	"BACKUP_ALLOW_PRIVATE_CIDRS|192.168.1.0/24;rm|BACKUP_ALLOW_PRIVATE_CIDRS is not a comma list of CIDRs" \
	"BACKUP_ALLOW_PRIVATE_CIDRS|fd00::/16|BACKUP_ALLOW_PRIVATE_CIDRS is not a comma list of CIDRs"; do
	IFS='|' read -r k v want <<<"$case18b"
	make_env devops-test-env | grep -v "^$k=" >"$d/.env"
	printf "%s='%s'\n" "$k" "$v" >>"$d/.env"
	chmod 600 "$d/.env"
	before="$(sum "$d/.env")"
	rc=0
	run "$d/deploy.sh" --non-interactive </dev/null >"$scratch/18b.out" 2>&1 || rc=$?
	want_refused "$scratch/18b.out" "$want" "18b: $k=$v was accepted"
	# A range is no secret; a key and a password are never echoed.
	if [[ "$k" != BACKUP_ALLOW_PRIVATE_CIDRS ]] && grep -qF -- "$v" "$scratch/18b.out"; then fail "18b: deploy.sh echoed the value of $k"; fi
	[[ "$(sum "$d/.env")" == "$before" ]] || fail "18b: deploy/.env changed"
	if grep -q 'docker stub: refused' "$scratch/18b.out"; then fail "18b: $k=$v went past the settings check"; fi
done
# A good list gets past the settings (to the stub).
make_env devops-test-env | grep -v '^BACKUP_ALLOW_PRIVATE_CIDRS=' >"$d/.env"
printf "BACKUP_ALLOW_PRIVATE_CIDRS='192.168.1.20/32, 10.1.0.0/16,fd00::/48'\n" >>"$d/.env"
chmod 600 "$d/.env"
run "$d/deploy.sh" --non-interactive </dev/null >"$scratch/18b2.out" 2>&1 || true
grep -q 'docker stub: refused' "$scratch/18b2.out" || {
	cat "$scratch/18b2.out"
	fail "18b: a valid BACKUP_ALLOW_PRIVATE_CIDRS list was refused"
}
echo "ok: bad key, bad password and 8 bad range lists refused by name and not echoed, deploy/.env unchanged; a valid list accepted"

echo "== 18b2. malformed OFFSITE_S3_*: refused by name, never echoed; the app's key is refused"
for case18b2 in "OFFSITE_S3_ACCESS_KEY|bad key planted 18b2|OFFSITE_S3_ACCESS_KEY must be 8 to 64 letters" \
	"OFFSITE_S3_SECRET_KEY|short-planted-18b2|OFFSITE_S3_SECRET_KEY must be 32 to 128 letters and digits" \
	"OFFSITE_S3_ACCESS_KEY|devops-app-placeholder|OFFSITE_S3_ACCESS_KEY must differ from S3_ACCESS_KEY"; do
	IFS='|' read -r k v want <<<"$case18b2"
	make_env devops-test-env | grep -v "^$k=" >"$d/.env"
	printf "%s='%s'\n" "$k" "$v" >>"$d/.env"
	chmod 600 "$d/.env"
	before="$(sum "$d/.env")"
	run "$d/deploy.sh" --non-interactive </dev/null >"$scratch/18b2s.out" 2>&1 || true
	want_refused "$scratch/18b2s.out" "$want" "18b2: $k=$v was accepted"
	if [[ "$k" == OFFSITE_S3_SECRET_KEY ]] && grep -qF -- "$v" "$scratch/18b2s.out"; then fail "18b2: deploy.sh echoed the value of $k"; fi
	[[ "$(sum "$d/.env")" == "$before" ]] || fail "18b2: deploy/.env changed"
	if grep -q 'docker stub: refused' "$scratch/18b2s.out"; then fail "18b2: $k=$v went past the settings check"; fi
done
echo "ok: a bad access key, a short secret and the app's access key are refused by name; deploy/.env unchanged"

echo "== 18b3. re-run, the OFFSITE_S3_* keys missing (an install from before the attachment backup)"
make_env devops-test-env | grep -v -e '^OFFSITE_S3_ACCESS_KEY=' -e '^OFFSITE_S3_SECRET_KEY=' >"$d/.env"
chmod 600 "$d/.env"
cp "$d/.env" "$scratch/18b3.before"
run "$d/deploy.sh" --non-interactive </dev/null >"$scratch/18b3.out" 2>&1 || true
grep -q 'added: OFFSITE_S3_ACCESS_KEY OFFSITE_S3_SECRET_KEY' "$scratch/18b3.out" || {
	cat "$scratch/18b3.out"
	fail "18b3: the two keys were not added and named"
}
ak="$(grep '^OFFSITE_S3_ACCESS_KEY=' "$d/.env")"
sk="$(grep '^OFFSITE_S3_SECRET_KEY=' "$d/.env")"
[[ "$ak" =~ ^OFFSITE_S3_ACCESS_KEY=\'devops-offsite-[0-9a-f]{12}\'$ ]] || fail "18b3: the appended OFFSITE_S3_ACCESS_KEY has the wrong shape"
[[ "$sk" =~ ^OFFSITE_S3_SECRET_KEY=\'[0-9a-f]{64}\'$ ]] || fail "18b3: the appended OFFSITE_S3_SECRET_KEY is not 64 hex characters"
head -n "$(($(wc -l <"$d/.env") - 2))" "$d/.env" | cmp -s - "$scratch/18b3.before" || fail "18b3: other lines of deploy/.env changed"
for v in "${ak#OFFSITE_S3_ACCESS_KEY=}" "${sk#OFFSITE_S3_SECRET_KEY=}"; do
	if grep -qF "${v//\'/}" "$scratch/18b3.out"; then fail "18b3: deploy.sh printed a generated value"; fi
done
cp "$d/.env" "$scratch/18b3.after"
run "$d/deploy.sh" --non-interactive </dev/null >"$scratch/18b3b.out" 2>&1 || true
cmp -s "$d/.env" "$scratch/18b3.after" || fail "18b3: a re-run changed deploy/.env (a key was generated again?)"
echo "ok: two values appended, not printed, the rest unchanged; a re-run keeps them byte for byte"

echo "== 18b4. the off-site silo policy holds the three read actions and nothing else"
pol="$(sed -n '/^cat >\/tmp\/devops-offsite-policy.json <<EOF$/,/^EOF$/p' "$here/deploy.sh")"
[[ -n "$pol" ]] || fail "18b4: the off-site policy block was not found in deploy.sh"
acts="$(printf '%s\n' "$pol" | grep -o '"s3:[A-Za-z*]*"' | sort | tr '\n' ' ')"
[[ "$acts" == '"s3:GetBucketLocation" "s3:GetObject" "s3:ListBucket" ' ]] || fail "18b4: the off-site policy actions are: $acts"
if printf '%s\n' "$pol" | grep -q -e '"Deny"' -e '"Action":"\*"' -e 's3:\*' -e '"NotAction"'; then fail "18b4: the off-site policy has a wildcard, Deny or NotAction"; fi
[[ "$(printf '%s\n' "$pol" | grep -c '"Effect":"Allow"')" == 2 ]] || fail "18b4: the off-site policy must have exactly two Allow statements"
echo "ok: devops-offsite-ro allows s3:GetBucketLocation, s3:ListBucket and s3:GetObject only"

echo "== 18c. the checkout was pulled by hand: the new compose file needs the two off-site keys"
make_env devops-test-env | grep -v -e '^BACKUP_ENCRYPTION_KEY=' -e '^OFFSITE_DB_PASSWORD=' >"$d/.env"
chmod 600 "$d/.env"
cp "$d/.env" "$scratch/18c.before"
touch "$ctl/running_postgres" "$ctl/running_backup" "$ctl/exec_ok" "$ctl/require_backup_keys"
rc=0
run env STUB_VOLUMES="devops-test-env_postgres_data" STUB_VOLUMES_LABEL=production \
	"$d/deploy.sh" --backup-now --non-interactive </dev/null >"$scratch/18c.out" 2>&1 || rc=$?
rm -f "$ctl"/running_* "$ctl/exec_ok" "$ctl/require_backup_keys"
((rc == 0)) || {
	cat "$scratch/18c.out"
	fail "18c: --backup-now failed (exit $rc)"
}
if grep -q 'missing a value' "$scratch/18c.out"; then fail "18c: compose still failed to interpolate"; fi
grep -q 'added the keys that .* lacked: BACKUP_ENCRYPTION_KEY OFFSITE_DB_PASSWORD' "$scratch/18c.out" || fail "18c: the added keys are not named"
head -n "$(($(wc -l <"$d/.env") - 2))" "$d/.env" | cmp -s - "$scratch/18c.before" || fail "18c: other lines of deploy/.env changed"
echo "ok: --backup-now added both keys before its first compose call; the rest unchanged"

echo "== 18d. the deploy order: everything but offsite first, then the role and its grants, then offsite; every up names its services"
make_env devops-test-env >"$d/.env"
chmod 600 "$d/.env"
pw18="OffsitePw18dAbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
sed -i.bak "s/^OFFSITE_DB_PASSWORD=.*/OFFSITE_DB_PASSWORD='$pw18'/" "$d/.env"
rm -f "$d/.env.bak"
touch "$ctl/trace" "$ctl/capture" "$ctl/exec_ok" "$ctl"/running_{postgres,silo,app,web,backup,offsite}
: >"$ctl/trace.log"
: >"$ctl/psql.stdin"
printf 'launchpad_backend \n' >"$ctl/pg_nets"
printf 'no\n' >"$ctl/setup_state"
rc=0
run env STUB_VOLUMES="devops-test-env_postgres_data" STUB_VOLUMES_LABEL=production \
	"$d/deploy.sh" --non-interactive </dev/null >"$scratch/18d.out" 2>&1 || rc=$?
rm -f "$ctl/trace" "$ctl/capture" "$ctl/exec_ok" "$ctl"/running_* "$ctl/pg_nets" "$ctl/setup_state"
# The run ends at the smoke test (nothing listens on the test ports), after offsite.
grep -q 'start offsite' "$scratch/18d.out" || {
	cat "$scratch/18d.out"
	fail "18d: the run never reached the offsite step"
}
n_build="$(grep -n ' build app web' "$ctl/trace.log" | head -1 | cut -d: -f1)"
n_pgsilo="$(grep -n ' up -d --remove-orphans postgres silo$' "$ctl/trace.log" | head -1 | cut -d: -f1)"
n_app="$(grep -n ' up -d --remove-orphans app web backup$' "$ctl/trace.log" | head -1 | cut -d: -f1)"
n_grants="$(grep -n ' exec -T postgres psql ' "$ctl/trace.log" | head -1 | cut -d: -f1)"
n_off="$(grep -n ' up -d offsite$' "$ctl/trace.log" | head -1 | cut -d: -f1)"
for v in n_build n_pgsilo n_app n_grants n_off; do
	[[ -n "${!v}" ]] || {
		cat "$ctl/trace.log"
		fail "18d: no trace line for $v"
	}
done
((n_build < n_pgsilo && n_pgsilo < n_app && n_app < n_grants && n_grants < n_off)) || {
	cat "$ctl/trace.log"
	fail "18d: wrong order (build $n_build, postgres+silo $n_pgsilo, app+web+backup $n_app, grants $n_grants, offsite $n_off)"
}
if grep -E ' up ' "$ctl/trace.log" | grep -vE ' up -d (--remove-orphans )?(postgres silo|app web backup|offsite)$'; then
	fail "18d: an up that does not name its services (above)"
fi
if grep -q 'offsite' <(sed -n "1,${n_app}p" "$ctl/trace.log" | grep ' up '); then fail "18d: offsite was started before the grants"; fi
# The password goes to psql on stdin only: not in any argument, not in the output.
if grep -qF "$pw18" "$ctl/trace.log"; then fail "18d: the role password is in a compose argument"; fi
if grep -qF "$pw18" "$scratch/18d.out"; then fail "18d: the role password is in the output"; fi
head -n 1 "$ctl/psql.stdin" | grep -qxF "\\set offsite_pw '$pw18'" || {
	head -c 300 "$ctl/psql.stdin" >&2
	fail "18d: the first line sent to psql is not the \\set of the password"
}
grep -q 'CREATE ROLE offsite' "$ctl/psql.stdin" || fail "18d: offsite-grants.sql was not sent"
cmp -s <(tail -n +2 "$ctl/psql.stdin") "$d/backup/offsite-grants.sql" || fail "18d: what follows the \\set line is not offsite-grants.sql"
echo "ok: build, postgres+silo, app+web+backup, grants, offsite; no unscoped up; the password reaches psql on stdin only"

echo "== 18e. the first update past this change says that postgres is recreated"
make_env devops-test-env >"$d/.env"
chmod 600 "$d/.env"
printf 'no\n' >"$ctl/setup_state"
touch "$ctl/trace" "$ctl/exec_ok" "$ctl"/running_{postgres,silo,app,web,backup,offsite}
printf 'devops-test-env_backend \n' >"$ctl/pg_nets"
run env STUB_VOLUMES="devops-test-env_postgres_data" STUB_VOLUMES_LABEL=production \
	"$d/deploy.sh" --non-interactive </dev/null >"$scratch/18e.out" 2>&1 || true
rm -f "$ctl/trace" "$ctl/exec_ok" "$ctl"/running_* "$ctl/pg_nets"
grep -q 'postgres is not on the offsite_db network yet: it is recreated now, and the database restarts briefly' "$scratch/18e.out" || {
	cat "$scratch/18e.out"
	fail "18e: no line about recreating postgres"
}
touch "$ctl/trace" "$ctl/exec_ok" "$ctl"/running_{postgres,silo,app,web,backup,offsite}
printf 'devops-test-env_backend devops-test-env_offsite_db \n' >"$ctl/pg_nets"
run env STUB_VOLUMES="devops-test-env_postgres_data" STUB_VOLUMES_LABEL=production \
	"$d/deploy.sh" --non-interactive </dev/null >"$scratch/18e2.out" 2>&1 || true
rm -f "$ctl/trace" "$ctl/exec_ok" "$ctl"/running_* "$ctl/pg_nets" "$ctl/trace.log" "$ctl/setup_state"
if grep -q 'is not on the offsite_db network yet' "$scratch/18e2.out"; then fail "18e: said it for a postgres that is on the network"; fi
echo "ok: the line is printed before the recreate, and only when postgres lacks offsite_db"

echo "== 18f. the host commands: a terminal only, the fingerprint checked, the throwaway containers locked down"
make_env devops-test-env >"$d/.env"
chmod 600 "$d/.env"
fp18="$(printf 'a%.0s' $(seq 1 64))"
for args in "--backup-keygen" "--backup-rotate-signing" "--backup-resume" "--backup-recipient-remove $fp18" "--backup-recipient-cancel $fp18"; do
	set +e
	# shellcheck disable=SC2086  # the words of $args are the arguments
	run env -u DEPLOY_PROJECT "$d/deploy.sh" $args </dev/null 2>"$scratch/18f.err" | cat >"$scratch/18f.out"
	rc="${PIPESTATUS[0]}"
	set -e
	cat "$scratch/18f.out" >>"$scratch/18f.err"
	want_refused "$scratch/18f.err" "${args%% *} needs a terminal" "18f: $args wrote to a pipe"
	if grep -q 'docker stub: refused' "$scratch/18f.err"; then fail "18f: $args called docker before the terminal check"; fi
	rc=0
	# shellcheck disable=SC2086
	run env -u DEPLOY_PROJECT "$d/deploy.sh" $args </dev/null >"$scratch/18f.out" 2>"$scratch/18f.err" || rc=$?
	want_refused "$scratch/18f.err" "${args%% *} needs a terminal" "18f: $args wrote to a file"
done
for bad in "" "abc" "$(printf 'A%.0s' $(seq 1 64))" "${fp18}0" "${fp18:1}"; do
	rc=0
	run env -u DEPLOY_PROJECT "$d/deploy.sh" --backup-recipient-remove ${bad:+"$bad"} </dev/null >"$scratch/18f.out" 2>&1 || rc=$?
	want_refused "$scratch/18f.out" 'needs the FULL fingerprint after it: 64 lower-case hex characters' "18f: fingerprint '$bad' was accepted"
done
# On a terminal, with the stub standing in for docker: the commands it would run.
touch "$ctl/running_app" "$ctl/running_postgres" "$ctl/running_offsite"
t18() { # OUT ARGS...: deploy.sh on a pseudo-terminal
	local out="$1"
	shift
	rc=0
	pty_run "" "$out" env -u DEPLOY_PROJECT PATH="$scratch/bin:$PATH" STUB_VOLUMES="devops-test-env_offsite_work" \
		STUB_VOLUMES_LABEL=production "$d/deploy.sh" "$@" || rc=$?
}
t18 "$scratch/18g.out" --backup-keygen
tr -d '\r' <"$scratch/18g.out" >"$scratch/18g.txt"
want_refused "$scratch/18g.txt" 'docker stub: refused: docker run --rm --pull never --network none --log-driver none --read-only --cap-drop ALL --security-opt no-new-privileges launchpad-api:stubtest /bin/server offsite keygen' "18f: --backup-keygen did not run the locked-down throwaway container"
if grep -q -e '--user' -e ' -v ' "$scratch/18g.txt"; then fail "18f: keygen got a --user or a mount (the image default user, no mounts)"; fi
t18 "$scratch/18h.out" --backup-resume
tr -d '\r' <"$scratch/18h.out" >"$scratch/18h.txt"
want_refused "$scratch/18h.txt" 'could not give the database a new install id' "18f: --backup-resume did not ask the database first (stub psql fails)"
# With a psql that answers with an instance id: the guard-file container is the second step.
printf '0191c3b2-7d4e-7a11-8c3a-0123456789ab\n' >"$ctl/setup_state"
t18 "$scratch/18i.out" --backup-resume
tr -d '\r' <"$scratch/18i.out" >"$scratch/18i.txt"
rm -f "$ctl/setup_state"
want_refused "$scratch/18i.txt" 'docker stub: refused: docker run --rm --pull never --user 70:70 --network none --read-only --cap-drop ALL --security-opt no-new-privileges -e OFFSITE_WORK_DIR=/offsite-work -v devops-test-env_offsite_work:/offsite-work launchpad-api:stubtest /bin/server offsite resume --instance-id 0191c3b2-7d4e-7a11-8c3a-0123456789ab' "18f: --backup-resume did not run the guard-file container as 70:70 with only offsite_work"
touch "$ctl/exec_ok"
t18 "$scratch/18j.out" --backup-rotate-signing
tr -d '\r' <"$scratch/18j.out" >"$scratch/18j.txt"
((rc == 0)) || {
	cat "$scratch/18j.txt"
	fail "18f: --backup-rotate-signing exited $rc"
}
grep -q 'compose exec ok: .* exec -T offsite /bin/server offsite rotate-signing' "$scratch/18j.txt" || fail "18f: rotate-signing did not run in the offsite container with -T"
t18 "$scratch/18k.out" --backup-recipient-remove "$fp18"
tr -d '\r' <"$scratch/18k.out" >"$scratch/18k.txt"
grep -q "compose exec ok: .* exec -T app /bin/server backup-recipient-remove --fingerprint $fp18" "$scratch/18k.txt" || fail "18f: recipient-remove did not run in the app container with -T"
t18 "$scratch/18l.out" --backup-recipient-cancel "$fp18"
tr -d '\r' <"$scratch/18l.out" >"$scratch/18l.txt"
grep -q "compose exec ok: .* exec -T app /bin/server backup-recipient-cancel --fingerprint $fp18" "$scratch/18l.txt" || fail "18f: recipient-cancel did not run in the app container with -T"
rm -f "$ctl"/running_* "$ctl/exec_ok" "$d/.env"
echo "ok: all five refuse a pipe and a file before docker; a bad fingerprint refused; keygen: --network none --log-driver none --read-only, default user, no mount; resume: --user 70:70, only offsite_work; the others run with -T in the right container"

printf '\ntest-env: PASS\n'
