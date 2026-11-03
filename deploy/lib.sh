# shellcheck shell=bash
# Shared by deploy.sh and restore.sh. Sourced, never run.
# Bash 3.2 compatible (the macOS /bin/bash): no associative arrays, and an
# empty array is expanded as ${a[@]+"${a[@]}"} under `set -u`.

DEPLOY_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC2034  # used by deploy.sh
REPO_DIR="$(cd "$DEPLOY_DIR/.." && pwd)"
ENV_FILE="${DEPLOY_ENV_FILE:-$DEPLOY_DIR/.env}"
COMPOSE_FILE="$DEPLOY_DIR/docker-compose.yml"

# Every key deploy/.env may hold. The file is parsed, never sourced.
ENV_KEYS="DEPLOY_PROJECT WEB_HOST FILES_HOST DEPLOY_BIND_ADDR DEPLOY_WEB_PORT DEPLOY_FILES_PORT
S3_BUCKET BACKUP_AT_UTC BACKUP_KEEP_DAYS POSTMARK_SERVER_TOKEN POSTMARK_FROM
POSTGRES_PASSWORD AUTH_OTP_PEPPER SILO_ROOT_USER SILO_ROOT_PASSWORD S3_ACCESS_KEY S3_SECRET_KEY
SETTINGS_ENCRYPTION_KEY BACKUP_ENCRYPTION_KEY OFFSITE_DB_PASSWORD OFFSITE_S3_ACCESS_KEY OFFSITE_S3_SECRET_KEY"

say() { printf '%s\n' "$*"; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }
die() {
	printf 'ERROR: %s\n' "$*" >&2
	exit 1
}

# env_get KEY: the value of KEY in deploy/.env (empty if absent).
env_get() {
	local key="$1" line
	[[ -f "$ENV_FILE" ]] || return 0
	while IFS= read -r line || [[ -n "$line" ]]; do
		case "$line" in
		"$key="*)
			line="${line#"$key"=}"
			line="${line#\'}"
			line="${line%\'}"
			printf '%s' "$line"
			return 0
			;;
		esac
	done <"$ENV_FILE"
}

# env_has KEY: deploy/.env has the key (even with an empty value).
env_has() {
	[[ -f "$ENV_FILE" ]] && grep -q "^$1=" "$ENV_FILE"
}

# load_env: every key into this shell (not exported).
load_env() {
	[[ -f "$ENV_FILE" ]] || die "$ENV_FILE not found; run deploy/deploy.sh first"
	local k v
	for k in $ENV_KEYS; do
		v="$(env_get "$k")"
		printf -v "$k" '%s' "$v"
	done
	[[ -n "${DEPLOY_PROJECT:-}" ]] || die "DEPLOY_PROJECT is empty in $ENV_FILE"
}

# ---- deploy/.env settings (moved from deploy.sh so restore.sh shares them)

# rand_hex N: N random bytes as hex (2N characters).
rand_hex() { od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n'; }

# A value must fit in one single-quoted dotenv line.
valid_value() { [[ "$1" != *"'"* && "$1" != *$'\n'* && "$1" != *$'\r'* ]]; }

# default_for KEY: a setting's first-run value: the environment, else the
# default. Secrets are generated here and nowhere else.
default_for() {
	local k="$1"
	case "$k" in
	DEPLOY_PROJECT) printf '%s' "${DEPLOY_PROJECT:-launchpad}" ;;
	# No default host: every install has its own. Empty here means the first
	# run must be told (a question on a terminal, else the environment).
	WEB_HOST) printf '%s' "${WEB_HOST:-}" ;;
	FILES_HOST) printf '%s' "${FILES_HOST:-}" ;;
	DEPLOY_BIND_ADDR) printf '%s' "${DEPLOY_BIND_ADDR:-127.0.0.1}" ;;
	DEPLOY_WEB_PORT) printf '%s' "${DEPLOY_WEB_PORT:-4001}" ;;
	DEPLOY_FILES_PORT) printf '%s' "${DEPLOY_FILES_PORT:-4002}" ;;
	S3_BUCKET) printf '%s' "${S3_BUCKET:-devops-attachments}" ;;
	BACKUP_AT_UTC) printf '%s' "${BACKUP_AT_UTC:-03:15}" ;;
	BACKUP_KEEP_DAYS) printf '%s' "${BACKUP_KEEP_DAYS:-30}" ;;
	POSTMARK_SERVER_TOKEN | POSTMARK_FROM) printf '%s' "${!k:-}" ;;
	POSTGRES_PASSWORD) rand_hex 32 ;;
	AUTH_OTP_PEPPER) rand_hex 48 ;;
	SILO_ROOT_USER) printf 'silo-root-%s' "$(rand_hex 6)" ;;
	SILO_ROOT_PASSWORD) rand_hex 32 ;;
	S3_ACCESS_KEY) printf 'devops-app-%s' "$(rand_hex 6)" ;;
	S3_SECRET_KEY) rand_hex 32 ;;
	# The key of the mail secrets in the database: 32 bytes, hex.
	SETTINGS_ENCRYPTION_KEY) rand_hex 32 ;;
	# (decisions-T16 #12, #15): the key of the off-site backup secrets
	# and the password of the restricted `offsite` database role. Generated
	# here and nowhere else, appended when missing, never made up again.
	BACKUP_ENCRYPTION_KEY) rand_hex 32 ;;
	OFFSITE_DB_PASSWORD) rand_hex 32 ;;
	# the read-only silo user of offsite (the attachment backup).
	OFFSITE_S3_ACCESS_KEY) printf 'devops-offsite-%s' "$(rand_hex 6)" ;;
	OFFSITE_S3_SECRET_KEY) rand_hex 32 ;;
	*) die "no default for $k" ;;
	esac
}

# env_bad MESSAGE: a setting is wrong. On a first run nothing was written
# yet; otherwise the operator edits deploy/.env.
# ENV_TMP is set only while a FIRST run builds its new file; it is the one
# path env_bad and the traps ever delete. The real deploy/.env is never
# removed, whatever its path looks like.
ENV_TMP=""

# drop_env_tmp: delete the first run's temp file, if one exists.
drop_env_tmp() {
	if [[ -n "$ENV_TMP" ]]; then
		rm -f "$ENV_TMP"
		ENV_TMP=""
	fi
}

env_bad() {
	if [[ -n "$ENV_TMP" ]]; then
		drop_env_tmp
		die "$* (first run: nothing was written; fix the setting in the environment and re-run)"
	fi
	die "$* - edit $ENV_FILE and re-run. Do not delete it while the stack's volumes exist: its secrets are the ones the database and silo were created with"
}

# valid_host NAME: a lower-case DNS name with at least one dot.
valid_host() { [[ "$1" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$ ]]; }

# ENV_ADDED: the keys the last env_append_missing appended (names only).
ENV_ADDED=()

# env_append_missing CREATED: append every key of ENV_KEYS that $ENV_FILE
# lacks, each on its own line (also when the file has no final newline).
# It only appends: it never changes or removes a value. CREATED is 1 for a
# first run (which may take Postmark from the environment) and 0 for an
# existing file (which asks nothing). For an existing file it first refuses a
# file that lacks a secret the volumes were created with: that is never made
# up again. It never prints a value. Shared by ensure_env and env_topup.
env_append_missing() {
	local created="$1" k v
	ENV_ADDED=()
	# A re-run never makes up the secrets the volumes were created with: a
	# new Postgres or silo root password would not match the data, and the
	# stack would fail its health checks.
	if ((created == 0)); then
		for k in POSTGRES_PASSWORD SILO_ROOT_USER SILO_ROOT_PASSWORD; do
			env_has "$k" || env_bad "$k is missing from $ENV_FILE. It must be the value the stack's volumes were created with, so it is never generated again; restore it from your copy of deploy/.env"
		done
	fi
	for k in $ENV_KEYS; do
		if env_has "$k"; then
			continue
		fi
		# Append on a new line, even if the file does not end in one.
		if [[ -s "$ENV_FILE" && "$(tail -c 1 "$ENV_FILE" | wc -l | tr -d ' ')" == 0 ]]; then
			printf '\n' >>"$ENV_FILE"
		fi
		if [[ $created -eq 0 && ($k == POSTMARK_SERVER_TOKEN || $k == POSTMARK_FROM) ]]; then
			v="${!k:-}"
		else
			v="$(default_for "$k")"
		fi
		# The two hosts have no default: refuse before an empty or bad value
		# is written (an existing file keeps its other lines untouched).
		if [[ $k == WEB_HOST || $k == FILES_HOST ]]; then
			[[ -n "$v" ]] || env_bad "$k is missing from $ENV_FILE and not set in the environment (no default). Set WEB_HOST and FILES_HOST in the environment, e.g. WEB_HOST=launchpad.example.com FILES_HOST=launchpad-files.example.com, or add them to the file"
			valid_host "$v" || env_bad "$k '$v' is not a lower-case host name"
		fi
		valid_value "$v" || env_bad "$k: a value may not hold a single quote or a newline"
		printf "%s='%s'\n" "$k" "$v" >>"$ENV_FILE"
		ENV_ADDED+=("$k")
	done
}

# check_settings_key: SETTINGS_ENCRYPTION_KEY (loaded) is 64 hex characters.
# Never echo it: the server takes exactly 64 hex characters (32 bytes).
check_settings_key() {
	[[ "$SETTINGS_ENCRYPTION_KEY" =~ ^[0-9a-fA-F]{64}$ ]] || env_bad "SETTINGS_ENCRYPTION_KEY must be exactly 64 hex characters (32 random bytes: openssl rand -hex 32). If you change it, the mail secrets stored in the app must be entered again"
}

# check_backup_key: BACKUP_ENCRYPTION_KEY (loaded) is 64 hex characters. Never
# echo it. The server (and offsite) also refuse all zeros outside local/test.
check_backup_key() {
	[[ "$BACKUP_ENCRYPTION_KEY" =~ ^[0-9a-fA-F]{64}$ ]] || env_bad "BACKUP_ENCRYPTION_KEY must be exactly 64 hex characters (32 random bytes: openssl rand -hex 32). It was generated once and must never change: a new value makes every stored backup destination secret unreadable, so restore the value from your copy of deploy/.env"
}

# check_offsite_password: OFFSITE_DB_PASSWORD (loaded) is a plain word of 32 or
# more letters and digits. It goes into a URL and into a psql \set line, so no
# other character is allowed. Never echo it.
check_offsite_password() {
	[[ "$OFFSITE_DB_PASSWORD" =~ ^[A-Za-z0-9]{32,128}$ ]] || env_bad "OFFSITE_DB_PASSWORD must be 32 to 128 letters and digits (deploy.sh generates one; the grant step sets the role's password from it on every run)"
}

# check_offsite_silo_keys: OFFSITE_S3_ACCESS_KEY and OFFSITE_S3_SECRET_KEY (loaded)
# are plain words (they go into silo's user add on stdin and into compose), and are
# NOT the app's pair: offsite's silo user is read-only, the app's is not. Never echo.
check_offsite_silo_keys() {
	[[ "$OFFSITE_S3_ACCESS_KEY" =~ ^[A-Za-z0-9_-]{8,64}$ ]] || env_bad "OFFSITE_S3_ACCESS_KEY must be 8 to 64 letters, digits, _ or - (deploy.sh generates one)"
	[[ "$OFFSITE_S3_SECRET_KEY" =~ ^[A-Za-z0-9]{32,128}$ ]] || env_bad "OFFSITE_S3_SECRET_KEY must be 32 to 128 letters and digits (deploy.sh generates one)"
	[[ "$OFFSITE_S3_ACCESS_KEY" != "$S3_ACCESS_KEY" ]] || env_bad "OFFSITE_S3_ACCESS_KEY must differ from S3_ACCESS_KEY: the off-site worker's silo user is read-only, the app's is not"
}

# valid_backup_cidrs LIST: the shape of BACKUP_ALLOW_PRIVATE_CIDRS, a comma list
# of CIDRs (decisions-T16 #35). Empty is valid. A /0, an IPv4 prefix shorter
# than /8 and an IPv6 prefix shorter than /32 are refused here; the worker's own
# parser (internal/netguard) is the authority on loopback, link-local and the
# other always-refused ranges and fails its boot with the reason.
valid_backup_cidrs() {
	local list="$1" item addr bits IFS=,
	[[ -z "$list" ]] && return 0
	for item in $list; do
		item="${item#"${item%%[![:space:]]*}"}"
		item="${item%"${item##*[![:space:]]}"}"
		[[ "$item" == */* ]] || return 1
		addr="${item%/*}"
		bits="${item##*/}"
		[[ "$bits" =~ ^[0-9]{1,3}$ ]] || return 1
		bits=$((10#$bits))
		if [[ "$addr" == *:* ]]; then
			[[ "$addr" =~ ^[0-9A-Fa-f:.]+$ ]] || return 1
			((bits >= 32 && bits <= 128)) || return 1
		else
			valid_ipv4 "$addr" || return 1
			((bits >= 8 && bits <= 32)) || return 1
			# The common always-refused ranges (loopback, 0/8, link-local, multicast
			# and above); the worker's parser knows the whole list.
			case "$addr" in 0.* | 127.* | 169.254.* | 22[4-9].* | 2[3-5][0-9].*) return 1 ;; esac
		fi
	done
	return 0
}

# env_topup [lenient]: for every mode that runs compose on an existing
# deploy/.env, except a first deploy and --nuke. A checkout that was
# `git pull`ed by hand has a compose file that may need a key the old
# deploy/.env lacks, and compose then fails to interpolate before
# any step runs. So append the missing keys first (env_append_missing: the
# generated secrets, and the non-secret defaults; mode 0600; nothing asked;
# no value changed or printed), then load the file again. Call it after
# guard_project, before the first dc.
# "lenient" (--backup-now, restore.sh): a malformed SETTINGS_ENCRYPTION_KEY
# is a warning, not a refusal. A safety backup must not wait for a key that
# the backup does not use. The other modes refuse it.
env_topup() {
	[[ -f "$ENV_FILE" ]] || return 0
	chmod 600 "$ENV_FILE"
	env_append_missing 0
	if ((${#ENV_ADDED[@]})); then
		say "== settings: added the keys that $ENV_FILE lacked: ${ENV_ADDED[*]}"
		if [[ " ${ENV_ADDED[*]} " == *" BACKUP_ENCRYPTION_KEY "* ]] && volume_exists "${DEPLOY_PROJECT}_postgres_data"; then
			warn "a NEW BACKUP_ENCRYPTION_KEY was generated for an existing install. That is right for the first update that adds off-site backups. If backup destinations were set up before, deploy/.env lost its old key: restore it from your copy, or every stored destination secret is unreadable (docs/deploy.md 'Scheduled off-site backups')"
		fi
	fi
	load_env
	if [[ "${1:-}" == lenient ]]; then
		[[ "$SETTINGS_ENCRYPTION_KEY" =~ ^[0-9a-fA-F]{64}$ ]] ||
			warn "SETTINGS_ENCRYPTION_KEY in $ENV_FILE is not 64 hex characters; the app will refuse to start with it. Continuing: this step does not use it"
		[[ "$BACKUP_ENCRYPTION_KEY" =~ ^[0-9a-fA-F]{64}$ ]] ||
			warn "BACKUP_ENCRYPTION_KEY in $ENV_FILE is not 64 hex characters; the app and the off-site worker will refuse to start with it. Continuing: this step does not use it"
	else
		check_settings_key
		check_backup_key
		check_offsite_password
		check_offsite_silo_keys
	fi
}

# The docker CLI gets a CLEAN environment: compose prefers the shell's
# variables over --env-file, so an exported LOG_ENV, S3_* or POSTMARK_* in
# the operator's shell must not reach the stack. docker_clean runs a
# command in a subshell that unsets every exported variable not in this
# allowlist, then execs it (no `env` binary, so no value lands in an argv;
# secrets reach compose only through deploy/.env). Everything else is
# dropped, on purpose including COMPOSE_* (COMPOSE_FILE or
# COMPOSE_PROJECT_NAME would redirect the stack) and BUILDX_*.
DOCKER_ENV_ALLOW=" PATH HOME USER LOGNAME LANG LC_ALL TERM TMPDIR
DOCKER_HOST DOCKER_CONTEXT DOCKER_CONFIG DOCKER_CERT_PATH DOCKER_TLS_VERIFY
DOCKER_API_VERSION DOCKER_DEFAULT_PLATFORM DOCKER_BUILDKIT BUILDKIT_PROGRESS
HTTP_PROXY HTTPS_PROXY NO_PROXY http_proxy https_proxy no_proxy
SSL_CERT_FILE SSL_CERT_DIR XDG_RUNTIME_DIR SSH_AUTH_SOCK "

# docker_clean [NAME=value...] -- COMMAND ARGS...: COMMAND in the clean
# environment, plus the given (non-secret) variables.
docker_clean() {
	(
		local __v __extra=()
		while [[ $# -gt 0 && "$1" != -- ]]; do
			__extra+=("$1")
			shift
		done
		shift
		for __v in $(compgen -e); do
			case "$DOCKER_ENV_ALLOW" in
			*[[:space:]]"$__v"[[:space:]]*) ;;
			*) unset "$__v" 2>/dev/null || true ;;
			esac
		done
		for __v in ${__extra[@]+"${__extra[@]}"}; do
			export "${__v?}"
		done
		exec "$@"
	)
}

# dc ARGS...: docker compose for this project, file and env file.
dc() {
	# A change to a bind-mounted config file is invisible to compose; its
	# checksum in the service environment makes `up` recreate the service.
	local nginx_sum backup_sum
	nginx_sum="$(cksum <"$DEPLOY_DIR/nginx/default.conf.template" | awk '{print $1}')"
	backup_sum="$(cksum <"$DEPLOY_DIR/backup/backup.sh" | awk '{print $1}')"
	# Arguments are expanded here, before docker_clean unsets anything (an
	# operator may export DEPLOY_PROJECT on a first run).
	docker_clean DEPLOY_VERSION="${DEPLOY_VERSION:-unversioned}" \
		DEPLOY_NGINX_CONF_SUM="$nginx_sum" DEPLOY_BACKUP_SH_SUM="$backup_sum" -- \
		docker compose -p "$DEPLOY_PROJECT" -f "$COMPOSE_FILE" --env-file "$ENV_FILE" "$@"
}

# cid SERVICE: the container id of a running service (empty if none).
cid() { dc ps -q "$1" 2>/dev/null | sed -n 1p; }

# app_logs_redacted [N]: the last N app log lines without the setup-link
# line (the one credential the server logs; its text starts with
# setup.LogPrefix, "setup: open "), so a failure report never prints it.
# The old seed-token lines are dropped too, for a log from before.
app_logs_redacted() {
	dc logs --no-color --tail "${1:-40}" app 2>&1 | grep -v -i -e 'setup: open ' -e 'seed token' || true
}

# service_logs_safe SERVICE [N]
service_logs_safe() {
	if [[ "$1" == app ]]; then
		app_logs_redacted "${2:-40}"
	else
		dc logs --no-color --tail "${2:-40}" "$1" 2>&1 || true
	fi
}

# wait_healthy SERVICE [SECONDS]: until the container reports healthy.
wait_healthy() {
	local svc="$1" limit="${2:-180}" i id st
	for ((i = 0; i < limit; i += 2)); do
		id="$(cid "$svc")"
		if [[ -n "$id" ]]; then
			st="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$id" 2>/dev/null || true)"
			case "$st" in
			healthy | running)
				say "  $svc: $st"
				return 0
				;;
			exited | dead)
				service_logs_safe "$svc" 40 >&2
				die "$svc stopped ($st)"
				;;
			esac
		fi
		sleep 2
	done
	service_logs_safe "$svc" 40 >&2
	die "$svc not healthy after ${limit}s (last state: ${st:-no container})"
}

# docker_ok: the daemon answers within DEPLOY_DOCKER_TIMEOUT seconds (default
# 30). A wedged daemon must not hang the deploy (the
# server/scripts/docker-preflight.sh lesson). A watchdog kills the probe;
# `wait` gives its real exit status.
docker_ok() {
	local limit="${DEPLOY_DOCKER_TIMEOUT:-30}" pid dog rc=0
	docker info --format '{{.ServerVersion}}' >/dev/null 2>&1 </dev/null &
	pid=$!
	(
		sleep "$limit"
		kill -9 "$pid" 2>/dev/null
	) >/dev/null 2>&1 &
	dog=$!
	wait "$pid" || rc=$?
	kill "$dog" 2>/dev/null || true
	wait "$dog" 2>/dev/null || true
	return "$rc"
}

# valid_ipv4 ADDR: four decimal octets, each 0-255, no leading zeros.
valid_ipv4() {
	local IFS=. o n=0
	# No leading zeros: "010" reads as octal to some tools.
	[[ "$1" =~ ^(0|[1-9][0-9]{0,2})(\.(0|[1-9][0-9]{0,2})){3}$ ]] || return 1
	for o in $1; do
		((10#$o <= 255)) || return 1
		n=$((n + 1))
	done
	((n == 4))
}

# tcp_open HOST PORT: something accepts a TCP connection within 3 seconds.
# bash's /dev/tcp has no timeout of its own; a filtered address would hang.
tcp_open() {
	local pid dog rc=0
	(exec 3<>"/dev/tcp/$1/$2") 2>/dev/null &
	pid=$!
	(
		sleep 3
		kill -9 "$pid" 2>/dev/null
	) >/dev/null 2>&1 &
	dog=$!
	wait "$pid" || rc=$?
	kill "$dog" 2>/dev/null || true
	wait "$dog" 2>/dev/null || true
	return "$rc"
}

# guard_project: refuse to act on a compose project that this file did not
# create. The local dev stack (server/docker-compose.yml) uses names such as
# dev-ops and declares the same volume names (postgres_data, silo_data): an
# `up` here under its project name would adopt its volumes and remove its
# containers as orphans. A container of the project must come from a
# deploy/docker-compose.yml, and a volume must carry this file's
# dev.orbit.deploy=production label.
guard_project() {
	local raw_c raw_v bad_c bad_v v name label
	raw_c="$(docker ps -a --filter "label=com.docker.compose.project=$DEPLOY_PROJECT" \
		--format '{{.Names}}|{{.Label "com.docker.compose.project.config_files"}}')" ||
		die "guard_project: 'docker ps' failed, so it cannot check that project '$DEPLOY_PROJECT' is this stack's; nothing was done"
	raw_v="$(docker volume ls --filter "label=com.docker.compose.project=$DEPLOY_PROJECT" \
		--format '{{.Name}}|{{.Label "dev.orbit.deploy"}}')" ||
		die "guard_project: 'docker volume ls' failed, so it cannot check that project '$DEPLOY_PROJECT' is this stack's; nothing was done"
	# Names are one word each; the word splitting below is wanted.
	bad_c="$(printf '%s\n' "$raw_c" | awk -F'|' 'NF && $2 !~ /\/deploy\/docker-compose\.yml$/ {print $1}')"
	bad_v="$(printf '%s\n' "$raw_v" | awk -F'|' 'NF && $2 != "production" {print $1}')"
	# A volume with this stack's name but no compose labels at all (made by
	# hand, or by `docker volume create`) is not in the list above; compose
	# would still use it. Check the names directly.
	# A failed inspect must not read as "absent": list the names first, and
	# treat an inspect error on a listed volume as a daemon error.
	local all_v
	all_v="$(docker volume ls -q)" ||
		die "guard_project: 'docker volume ls -q' failed, so it cannot check the stack's volumes; nothing was done"
	for v in postgres_data silo_data backups import_work offsite_work; do
		name="${DEPLOY_PROJECT}_$v"
		# A here-string, not a pipeline: under pipefail, `printf | grep -q`
		# can fail with SIGPIPE and read as "absent".
		grep -qxF -- "$name" <<<"$all_v" || continue
		label="$(docker volume inspect -f '{{index .Labels "dev.orbit.deploy"}}' "$name" 2>/dev/null)" ||
			die "guard_project: 'docker volume inspect $name' failed; nothing was done"
		if [[ "$label" != production && " $bad_v " != *" $name "* ]]; then
			bad_v="$bad_v $name"
		fi
	done
	if [[ -n "${bad_c//[[:space:]]/}${bad_v//[[:space:]]/}" ]]; then
		local names
		# shellcheck disable=SC2086
		names="$(printf '%s ' $bad_c $bad_v)"
		die "guard_project: compose project '$DEPLOY_PROJECT' already belongs to another stack (not deploy/docker-compose.yml): ${names% }. deploy.sh does not support it (another compose file's stack, or one created from a pre-release commit of this branch) and will not touch it. Do not run down -v on it (that also deletes its backups volume). If it holds data, take a verified dump yourself (pg_dump exit status checked, then restore into a scratch database), and deploy into a NEW DEPLOY_PROJECT name in $ENV_FILE"
	fi
}

# volume_exists NAME: a docker volume of that exact name exists. A failing
# `docker volume ls` is an error, never "absent".
volume_exists() {
	local all
	all="$(docker volume ls -q)" || die "'docker volume ls' failed; nothing was done"
	grep -qxF -- "$1" <<<"$all"
}
