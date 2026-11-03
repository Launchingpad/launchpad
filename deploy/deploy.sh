#!/usr/bin/env bash
# Launchpad one-command production deploy. Runbook: docs/deploy.md.
#
#   deploy/deploy.sh                    first deploy, or redeploy this checkout.
#                                       A first deploy on a terminal ends with
#                                       the setup link (one command); with no
#                                       terminal it prints a hint, never the link.
#   deploy/deploy.sh --update           back up, git pull --ff-only, redeploy
#   deploy/deploy.sh --setup-link       first-run setup: print a NEW setup
#                                       link (24h, single use; older links
#                                       stop working). Needs a terminal.
#   deploy/deploy.sh --mint-token --org SLUG --user NAME --name N --scopes read,write
#                    [--expires-days N] an org-pinned token for a member (an
#                                       agent such as claude, or the rescue
#                                       admin token); every argument after
#                                       --mint-token goes to `server
#                                       mint-token`. Needs a terminal.
#   deploy/deploy.sh --backup-now       one pg_dump into the backups volume
#   deploy/deploy.sh --backup-keygen    off-site backups: make an age key
#                                       pair in a throwaway container with no
#                                       network. Prints the PRIVATE key too: a
#                                       terminal only. Keep it off this host.
#   deploy/deploy.sh --backup-rotate-signing  replace the manifest signing key
#                                       (prints the new public key; terminal only)
#   deploy/deploy.sh --backup-recipient-remove FINGERPRINT  take an age recipient
#                                       out at once (64 hex characters; terminal only)
#   deploy/deploy.sh --backup-recipient-cancel FINGERPRINT  cancel a pending
#                                       recipient add or removal at once (terminal only)
#   deploy/deploy.sh --backup-resume    after a restore or a clone: a NEW install
#                                       id, so this copy never writes into the
#                                       original's off-site folders (terminal only)
#   deploy/deploy.sh --smoke            only the smoke test, against the stack
#   deploy/deploy.sh --nuke             DELETE this deploy and ALL its data:
#                                       containers, networks and the five
#                                       volumes (database, attachments,
#                                       backups, import exports, off-site
#                                       scratch). Asks for the project name;
#                                       with no terminal, give
#                                       --yes-delete-everything <project>.
#                                       --nuke-images: also the launchpad-api/web
#                                       images (and the old dev-ops-api/web ones);
#                                       --nuke-env: also deploy/.env.
#
# Flags for the first run: --non-interactive (no prompt; POSTMARK_* from the
# environment). An interactive first run asks for the bind address, the web
# and files hosts and the two ports. It does not ask for mail: set that on
# the web Platform page. Every setting can be given in the environment on
# the FIRST run; a value in the environment skips its question
# (DEPLOY_PROJECT, WEB_HOST, FILES_HOST, DEPLOY_WEB_PORT, DEPLOY_FILES_PORT,
# DEPLOY_BIND_ADDR, POSTMARK_SERVER_TOKEN, POSTMARK_FROM, ...); after that
# deploy/.env is the authority, and you edit it there.
#
# It never prints a secret, except the setup link, the token and the age
# private key that --setup-link, --mint-token and --backup-keygen exist to
# print (on a terminal only). Exit status is non-zero on any failure.
set -euo pipefail

# shellcheck source=deploy/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

mode=deploy
skip_pull=0
# 1 once this run (or the run that re-executed it) took the backup before
# the update.
backed_up=0
interactive=1
[[ -t 0 ]] || interactive=0
# --nuke and its flags.
nuke_flag=0
nuke_images=0
nuke_env=0
nuke_confirm_set=0
nuke_confirm=""
# --mint-token's arguments, passed on unchanged.
mint_args=()
# --backup-recipient-remove / -cancel: the fingerprint.
backup_fp=""
usage() { sed -n '2,54p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }
while [[ $# -gt 0 ]]; do
	case "$1" in
	--nuke) nuke_flag=1 ;;
	--nuke-images) nuke_images=1 ;;
	--nuke-env) nuke_env=1 ;;
	--yes-delete-everything)
		[[ $# -ge 2 && -n "$2" && "$2" != -* ]] ||
			die "--yes-delete-everything needs the project name after it (the DEPLOY_PROJECT in deploy/.env); nothing was done"
		nuke_confirm="$2"
		nuke_confirm_set=1
		shift
		;;
	--yes | -y | --yes=* | --yes-delete-everything=*)
		die "$1 is not accepted. --nuke deletes the database, the attachments and the backups: confirm it by typing the project name, or with --yes-delete-everything <project> (the exact name); nothing was done"
		;;
	--update) mode=update ;;
	--update-after-pull) # internal: the re-exec after the backup and git pull
		mode=update
		skip_pull=1
		backed_up=1
		;;
	--update-continue) # internal, older: the re-exec by a deploy.sh from
		# before, which pulled BEFORE it backed up. This run still
		# takes the backup (deploy below).
		mode=update
		skip_pull=1
		;;
	--setup-link) mode=setuplink ;;
	--mint-token)
		# Every argument after it belongs to `server mint-token`.
		mode=minttoken
		shift
		mint_args=("$@")
		((${#mint_args[@]} > 0)) ||
			die "--mint-token needs the mint-token flags after it: --org <slug> --user <username> --name <token name> --scopes read,write[,admin] [--expires-days N] (docs/deploy.md 'Agent users and their tokens'); nothing was done"
		break
		;;
	--seed-tokens)
		die "--seed-tokens is gone: there are no seed tokens any more. A new install is set up through a setup link: deploy/deploy.sh --setup-link (docs/deploy.md 'First-run setup')"
		;;
	--backup-now) mode=backup-now ;;
	--backup-keygen) mode=backupkeygen ;;
	--backup-rotate-signing) mode=backuprotate ;;
	--backup-resume) mode=backupresume ;;
	--backup-recipient-remove | --backup-recipient-cancel)
		[[ "$1" == --backup-recipient-remove ]] && mode=backuprecipientremove || mode=backuprecipientcancel
		[[ $# -ge 2 && "$2" =~ ^[0-9a-f]{64}$ ]] ||
			die "$1 needs the FULL fingerprint after it: 64 lower-case hex characters (the sha256 of the age1... key; the web page shows the first 12); nothing was done"
		backup_fp="$2"
		shift
		;;
	--smoke) mode=smoke ;;
	--non-interactive) interactive=0 ;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		usage >&2
		die "unknown argument: $1"
		;;
	esac
	shift
done
if ((nuke_flag)); then
	[[ "$mode" == deploy ]] || die "--nuke cannot be combined with another mode (--$mode); nothing was done"
	mode=nuke
elif ((nuke_images || nuke_env || nuke_confirm_set)); then
	die "--nuke-images, --nuke-env and --yes-delete-everything go only with --nuke; nothing was done"
fi

# ---------------------------------------------------------------- prerequisites

check_prereqs() {
	say "== prerequisites"
	local c
	for c in docker git curl python3 od tr head; do
		command -v "$c" >/dev/null 2>&1 || die "$c is not installed"
	done
	docker_ok || die "the Docker daemon is not running or does not answer (docker info)"
	local v major minor
	v="$(docker compose version --short 2>/dev/null || true)"
	v="${v#v}"
	[[ -n "$v" ]] || die "docker compose v2 is not installed (the docker-compose v1 binary is not enough)"
	major="${v%%.*}"
	minor="${v#*.}"
	minor="${minor%%.*}"
	if ! [[ "$major" =~ ^[0-9]+$ && "$minor" =~ ^[0-9]+$ ]] || ((major < 2 || (major == 2 && minor < 20))); then
		die "docker compose $v is too old; 2.20 or newer is needed"
	fi
	say "  docker $(docker version --format '{{.Server.Version}}'), compose $v"
}

# port_free PORT: nothing but this project listens on it.
port_free() {
	local port="$1" owner
	owner="$(docker ps --filter "publish=$port" --format '{{.Label "com.docker.compose.project"}}' | sed -n 1p)"
	if [[ -n "$owner" ]]; then
		[[ "$owner" == "$DEPLOY_PROJECT" ]] && return 0
		warn "port $port is published by docker project '$owner'"
		return 1
	fi
	local a
	for a in 127.0.0.1 "$DEPLOY_BIND_ADDR"; do
		[[ "$a" == 0.0.0.0 ]] && continue
		if tcp_open "$a" "$port"; then
			warn "port $port is in use on $a"
			return 1
		fi
	done
	return 0
}

check_ports() {
	local p bad=0
	for p in "$DEPLOY_WEB_PORT" "$DEPLOY_FILES_PORT"; do
		port_free "$p" || bad=1
	done
	((bad == 0)) || die "a port is taken; free it or set DEPLOY_WEB_PORT / DEPLOY_FILES_PORT in $ENV_FILE"
	say "  ports $DEPLOY_WEB_PORT and $DEPLOY_FILES_PORT are free (or already this stack's)"
}

# ---------------------------------------------------------------- deploy/.env


# ask_setting VAR LABEL CHECK_FN [OTHER_VAR]: ask for one setting on a
# first run. A value already in the environment skips the question. An
# empty answer keeps the default. A bad answer is asked again. OTHER_VAR
# names the setting that must differ (web and files host, web and files
# port). The check runs when that setting is already fixed: it came from
# the environment, or it was asked first.
ask_setting() {
	local var="$1" label="$2" check="$3" other="${4:-}" def ans
	[[ -z "${!var:-}" ]] || return 0
	def="$(default_for "$var")"
	while :; do
		read -r -p "$label [${def:-required}]: " ans || die "no answer for $label (end of input). Run with --non-interactive, or set $var in the environment"
		ans="${ans:-$def}"
		if ! "$check" "$ans"; then
			warn "'$ans' is not valid for $var. Try again."
			continue
		fi
		# The partner is set when it came from the environment, or when it
		# was asked before this one. Else it is not fixed yet: its own
		# question checks the pair.
		if [[ -n "$other" && -n "${!other:-}" && "$ans" == "${!other}" ]]; then
			warn "$var and $other must differ. Try again."
			continue
		fi
		break
	done
	printf -v "$var" '%s' "$ans"
	export "${var?}"
}

# valid_port_answer N: digits, no leading zero, 1 to 65535.
valid_port_answer() { [[ "$1" =~ ^[1-9][0-9]*$ ]] && ((10#$1 <= 65535)); }

# ask_settings: the first run asks for the bind address, the two hosts and
# the two ports. Mail is not asked: set it on the web Platform page.
ask_settings() {
	((interactive)) || return 0
	say ""
	say "Press Enter to take the default shown in [ ]."
	ask_setting DEPLOY_BIND_ADDR "Bind address" valid_ipv4
	ask_setting WEB_HOST "Web host" valid_host FILES_HOST
	ask_setting FILES_HOST "Files host" valid_host WEB_HOST
	ask_setting DEPLOY_WEB_PORT "Web port" valid_port_answer DEPLOY_FILES_PORT
	ask_setting DEPLOY_FILES_PORT "Files port" valid_port_answer DEPLOY_WEB_PORT
}

ensure_env() {
	say "== settings ($ENV_FILE)"
	local created=0 k final="$ENV_FILE"
	if [[ ! -f "$ENV_FILE" ]]; then
		created=1
		ask_settings
		# A first run writes a new file and moves it into place only after
		# every check passes, so a failed first run leaves no partial .env.
		ENV_TMP="$final.new.$$"
		ENV_FILE="$ENV_TMP"
		# Ctrl-C or a kill while it holds fresh secrets: remove it.
		trap 'drop_env_tmp' EXIT
		trap 'drop_env_tmp; exit 130' INT
		trap 'drop_env_tmp; exit 143' TERM
		rm -f "$ENV_TMP"
		(
			umask 077
			{
				echo "# Launchpad deploy settings and secrets. Written by deploy/deploy.sh."
				echo "# Mode 0600, gitignored. Never commit, paste or print this file."
				echo "# A re-run keeps every value; edit here to change one, then re-run."
			} >"$ENV_FILE"
		)
	fi
	chmod 600 "$ENV_FILE"
	env_append_missing "$created"
	local added=("${ENV_ADDED[@]+"${ENV_ADDED[@]}"}")
	load_env
	if ((created)) && volume_exists "${DEPLOY_PROJECT}_postgres_data"; then
		env_bad "deploy/.env is missing, but the volume ${DEPLOY_PROJECT}_postgres_data exists: its database was created with the secrets of the old deploy/.env. Restore that file (from your copy) instead of generating new secrets"
	fi
	# Validate what the stack is about to get.
	[[ -n "$WEB_HOST" && -n "$FILES_HOST" ]] || env_bad "WEB_HOST and FILES_HOST are required (no default): answer the questions on a terminal, or set both in the environment, e.g. WEB_HOST=launchpad.example.com FILES_HOST=launchpad-files.example.com"
	valid_host "$WEB_HOST" || env_bad "WEB_HOST '$WEB_HOST' is not a lower-case host name"
	valid_host "$FILES_HOST" || env_bad "FILES_HOST '$FILES_HOST' is not a lower-case host name"
	[[ "$WEB_HOST" != "$FILES_HOST" ]] || env_bad "WEB_HOST and FILES_HOST must differ"
	[[ "$DEPLOY_PROJECT" =~ ^[a-z0-9][a-z0-9_-]*$ ]] || env_bad "DEPLOY_PROJECT '$DEPLOY_PROJECT' is not a compose project name"
	[[ "$DEPLOY_WEB_PORT" =~ ^[0-9]+$ && "$DEPLOY_FILES_PORT" =~ ^[0-9]+$ ]] || env_bad "the ports must be numbers"
	[[ "$DEPLOY_WEB_PORT" != "$DEPLOY_FILES_PORT" ]] || env_bad "DEPLOY_WEB_PORT and DEPLOY_FILES_PORT must differ"
	valid_ipv4 "$DEPLOY_BIND_ADDR" || env_bad "DEPLOY_BIND_ADDR '$DEPLOY_BIND_ADDR' must be an IPv4 address (four octets, 0-255)"
	[[ "$S3_BUCKET" =~ ^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$ ]] || env_bad "S3_BUCKET '$S3_BUCKET' is not a bucket name (3-63 characters of a-z 0-9 . -)"
	((${#AUTH_OTP_PEPPER} >= 32)) || env_bad "AUTH_OTP_PEPPER must be at least 32 bytes"
	for k in POSTGRES_PASSWORD SILO_ROOT_USER SILO_ROOT_PASSWORD S3_ACCESS_KEY S3_SECRET_KEY; do
		[[ -n "${!k}" ]] || env_bad "$k is empty in the settings. Restore the value the stack was created with (on a NEW stack with no volumes yet, you may instead delete deploy/.env and re-run)"
	done
	((${#POSTGRES_PASSWORD} >= 32)) || env_bad "POSTGRES_PASSWORD must be at least 32 characters"
	((${#SILO_ROOT_PASSWORD} >= 32)) || env_bad "SILO_ROOT_PASSWORD must be at least 32 characters"
	((${#S3_SECRET_KEY} >= 32)) || env_bad "S3_SECRET_KEY must be at least 32 characters"
	check_settings_key
	check_backup_key
	check_offsite_password
	check_offsite_silo_keys
	BACKUP_ALLOW_PRIVATE_CIDRS="$(env_get BACKUP_ALLOW_PRIVATE_CIDRS)"
	valid_backup_cidrs "$BACKUP_ALLOW_PRIVATE_CIDRS" || env_bad "BACKUP_ALLOW_PRIVATE_CIDRS is not a comma list of CIDRs such as 192.168.1.20/32 (no /0, an IPv4 range no shorter than /8, an IPv6 range no shorter than /32, none that overlaps loopback or link-local); the worker's own parser is the authority and fails its boot with the reason"
	if [[ -n "$POSTMARK_SERVER_TOKEN" && -z "$POSTMARK_FROM" ]]; then
		env_bad "POSTMARK_FROM is required when POSTMARK_SERVER_TOKEN is set"
	fi
	if ((created)); then
		# `ln` fails if deploy/.env appeared meanwhile (two first runs at
		# once): the other run's file wins and this one stops.
		if ! ln "$ENV_TMP" "$final" 2>/dev/null; then
			drop_env_tmp
			if [[ -e "$final" ]]; then
				die "$final appeared while this first run was writing its own (another deploy.sh?). Nothing was changed; re-run to use it"
			fi
			die "could not put $final into place (ln failed: permissions, or a file system without hard links?). Nothing was written"
		fi
		drop_env_tmp
		ENV_FILE="$final"
		trap - EXIT INT TERM
		say "  created (mode 0600); generated POSTGRES_PASSWORD, AUTH_OTP_PEPPER, SILO_ROOT_USER/PASSWORD, S3_ACCESS_KEY/SECRET_KEY, SETTINGS_ENCRYPTION_KEY, BACKUP_ENCRYPTION_KEY, OFFSITE_DB_PASSWORD, OFFSITE_S3_ACCESS_KEY/SECRET_KEY"
	elif ((${#added[@]})); then
		say "  kept every existing value; added: ${added[*]}"
		if [[ " ${added[*]} " == *" BACKUP_ENCRYPTION_KEY "* ]] && volume_exists "${DEPLOY_PROJECT}_postgres_data"; then
			warn "a NEW BACKUP_ENCRYPTION_KEY was generated for an existing install. That is right for the first update that adds off-site backups. If backup destinations were set up before, deploy/.env lost its old key: restore it from your copy, or every stored destination secret is unreadable (docs/deploy.md 'Scheduled off-site backups')"
		fi
	else
		say "  kept every existing value"
	fi
	[[ -n "$POSTMARK_SERVER_TOKEN" ]] || warn "web login needs mail. Open the Platform page in the web app and set Postmark or SMTP there."
}

# ---------------------------------------------------------------- steps

set_version() {
	DEPLOY_VERSION="$(git -C "$REPO_DIR" rev-parse --short=12 HEAD 2>/dev/null || echo unversioned)"
	if [[ -n "$(git -C "$REPO_DIR" status --porcelain --untracked-files=no 2>/dev/null)" ]]; then
		DEPLOY_VERSION="$DEPLOY_VERSION-dirty"
	fi
	export DEPLOY_VERSION
}

check_clean_checkout() {
	if [[ -n "$(git -C "$REPO_DIR" status --porcelain --untracked-files=no)" ]]; then
		die "the checkout has uncommitted changes; commit or discard them before --update"
	fi
}

git_update() {
	say "== update: git pull --ff-only"
	check_clean_checkout
	local before after
	before="$(git -C "$REPO_DIR" rev-parse --short=12 HEAD)"
	git -C "$REPO_DIR" pull --ff-only
	after="$(git -C "$REPO_DIR" rev-parse --short=12 HEAD)"
	say "  $before -> $after"
	# deploy.sh itself may have changed: run the new one for the rest. The
	# backup is already taken; --non-interactive is passed on.
	if [[ "$before" != "$after" && -z "${DEPLOY_REEXEC:-}" ]]; then
		local args=(--update-after-pull)
		((interactive)) || args+=(--non-interactive)
		say "  re-running the updated deploy.sh"
		DEPLOY_REEXEC=1 exec "$DEPLOY_DIR/deploy.sh" "${args[@]}"
	fi
}

# backup_now [start]: one dump now. It never recreates postgres: the backup
# service is started with `up --no-deps`, so compose leaves postgres and
# volume-init alone even when the checked-out compose file is newer than
# the running stack. With "start", a stopped postgres is started (alone,
# --no-deps); without it, a stopped postgres is an error. Each step returns
# non-zero on failure (set -e does not apply inside `backup_now || die`);
# a failed health wait runs in a subshell so that it returns too.
backup_now() {
	if [[ -z "$(cid postgres)" ]]; then
		if [[ "${1:-}" != start ]]; then
			warn "postgres is not running"
			return 1
		fi
		say "  postgres is not running: starting it alone (--no-deps)"
		dc up -d --no-deps postgres >/dev/null || return 1
	fi
	(wait_healthy postgres 120) || return 1
	if [[ -z "$(cid backup)" ]]; then
		dc up -d --no-deps backup >/dev/null || return 1
		(wait_healthy backup 60) || return 1
	fi
	dc exec -T backup /bin/sh /backup/backup.sh once || return 1
}

# update_backup before-pull|after-pull: the restore point of an update,
# taken whenever the database volume exists, running or not.
#   before-pull  the normal path: the checkout (and so the compose file) is
#                still the running version's; a stopped postgres is started.
#   after-pull   only after a re-exec by a deploy.sh from before,
#                which pulled first: the compose file may already be the new
#                one, so it starts nothing but the backup service and needs
#                postgres running.
update_backup() {
	if [[ ! -f "$ENV_FILE" ]]; then
		say "== no $ENV_FILE yet (no stack has been deployed from this checkout): nothing to back up before the update"
		return 0
	fi
	if ! volume_exists "${DEPLOY_PROJECT}_postgres_data"; then
		say "== no database volume yet: nothing to back up before the update"
		return 0
	fi
	say "== backup before the update"
	if [[ "$1" == before-pull ]]; then
		backup_now start || die "the backup before the update failed; nothing was pulled or updated (at most postgres and the backup service were started)"
	else
		backup_now || die "the backup before the update failed; at most the backup service was started, but the older deploy.sh already pulled the checkout. Start the stack at the version that is running (git checkout <that commit>; deploy/deploy.sh), then run --update again"
	fi
	backed_up=1
}

# The silo bucket, a bucket-scoped policy and the app's user, and
# (decisions-T16 #38) a READ-ONLY user for the off-site worker: its own key, a
# policy of s3:GetObject, s3:ListBucket and s3:GetBucketLocation, nothing that
# writes or deletes. Idempotent.
# The app's secret goes into the container on STDIN, and from there to
# `mcli admin user add` on its stdin: mcli reads the secret there when the
# argument is left out (measured on the pinned silo). `read` and `printf`
# are builtins of the image's sh (bash; measured). So the secret is in no
# argv, on the host or in the container. The root credentials are
# silo's own environment; the bucket and access keys are not secrets. The two
# secrets go in as line 1 (the app) and line 2 (offsite) of the same stdin.
silo_bootstrap() {
	say "== silo: bucket, app user and the read-only off-site user"
	local script
	# shellcheck disable=SC2016  # expanded inside the container
	script='set -eu
IFS= read -r app_secret
IFS= read -r ro_secret
export MC_CONFIG_DIR=/tmp/.mcli
export MC_HOST_local="http://${MINIO_ROOT_USER}:${MINIO_ROOT_PASSWORD}@127.0.0.1:9000"
mcli --quiet mb --ignore-existing "local/${S3_BUCKET}" >/dev/null
mcli --quiet anonymous set none "local/${S3_BUCKET}" >/dev/null
cat >/tmp/devops-app-policy.json <<EOF
{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3:GetBucketLocation","s3:ListBucket"],"Resource":["arn:aws:s3:::${S3_BUCKET}"]},
 {"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:DeleteObject"],"Resource":["arn:aws:s3:::${S3_BUCKET}/*"]}]}
EOF
mcli --quiet admin policy create local devops-app /tmp/devops-app-policy.json >/dev/null
rm -f /tmp/devops-app-policy.json
printf "%s\n" "$app_secret" | mcli --quiet admin user add local "${S3_ACCESS_KEY}" >/dev/null
unset app_secret
info="$(mcli --quiet admin user info local "${S3_ACCESS_KEY}" --json)"
case "$info" in
*"\"devops-app\""*) ;;
*) mcli --quiet admin policy attach local devops-app --user "${S3_ACCESS_KEY}" >/dev/null ;;
esac
cat >/tmp/devops-offsite-policy.json <<EOF
{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3:GetBucketLocation","s3:ListBucket"],"Resource":["arn:aws:s3:::${S3_BUCKET}"]},
 {"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::${S3_BUCKET}/*"]}]}
EOF
mcli --quiet admin policy create local devops-offsite-ro /tmp/devops-offsite-policy.json >/dev/null
rm -f /tmp/devops-offsite-policy.json
printf "%s\n" "$ro_secret" | mcli --quiet admin user add local "${OFFSITE_S3_ACCESS_KEY}" >/dev/null
unset ro_secret
info="$(mcli --quiet admin user info local "${OFFSITE_S3_ACCESS_KEY}" --json)"
case "$info" in
*"\"devops-offsite-ro\""*) ;;
*) mcli --quiet admin policy attach local devops-offsite-ro --user "${OFFSITE_S3_ACCESS_KEY}" >/dev/null ;;
esac
echo "  bucket ${S3_BUCKET} (private), user ${S3_ACCESS_KEY} with policy devops-app, user ${OFFSITE_S3_ACCESS_KEY} with policy devops-offsite-ro (read only)"'
	printf '%s\n%s\n' "$S3_SECRET_KEY" "$OFFSITE_S3_SECRET_KEY" |
		dc exec -T -e "S3_BUCKET=$S3_BUCKET" -e "S3_ACCESS_KEY=$S3_ACCESS_KEY" -e "OFFSITE_S3_ACCESS_KEY=$OFFSITE_S3_ACCESS_KEY" silo sh -c "$script"
}

# setup_link_count: the setup-link lines in the app log, or
# "unknown" when the log cannot be read. Information only: a link that is
# lost costs nothing, a restart or --setup-link makes a new one.
setup_link_count() {
	local log
	log="$(dc logs --no-color app 2>/dev/null)" || {
		echo unknown
		return 0
	}
	grep -c 'setup: open ' <<<"$log" || true
}

# setup_required: "yes" while the install has no user (first-run setup is
# open), "no" after, "unknown" if the database cannot be asked.
setup_required() {
	local out
	out="$(printf '%s\n' "SELECT CASE WHEN EXISTS (SELECT 1 FROM users) THEN 'no' ELSE 'yes' END;" |
		dc exec -T postgres psql -X -q -t -A -v ON_ERROR_STOP=1 -U devops -d devops 2>/dev/null | tr -d '\r')" || out=""
	case "$out" in
	yes | no) printf '%s' "$out" ;;
	*) printf 'unknown' ;;
	esac
}

# show_setup_link: --setup-link. `server setup-link` in the app container
# mints a new link (closing the older ones) and prints it on stdout only,
# never to the log. Like the old --seed-tokens it refuses a pipe or a file,
# so the link is not kept anywhere by accident.
show_setup_link() {
	say "== setup link"
	[[ -t 1 ]] || die "--setup-link prints a credential and needs a terminal; it refuses to write it to a pipe or a file"
	[[ -n "$(cid app)" ]] || die "the app container is not running; run deploy/deploy.sh first"
	local rc=0
	say ""
	dc exec -T app /bin/server setup-link </dev/null || rc=$?
	case "$rc" in
	0)
		say ""
		say "  Open it in a browser, or POST /api/v1/setup with the part after # as"
		say "  \"token\" (docs/deploy.md 'First-run setup'). It works for 24 hours and once;"
		say "  any older setup link, including the one in the app log, no longer works."
		;;
	3) say "  setup is complete: this install has a user, so there is no setup link." ;;
	*) die "server setup-link failed (exit $rc); the reason is above" ;;
	esac
}

# show_mint_token: --mint-token. `server mint-token` in the app container
# prints the new token's secret alone on stdout (the only copy; only its hash
# is stored) and its description on stderr, and logs nothing. Like
# --setup-link it refuses a pipe or a file, so the secret is not kept
# anywhere by accident. The arguments reach the container as separate words,
# never through a shell.
show_mint_token() {
	say "== mint token"
	[[ -t 1 ]] || die "--mint-token prints a credential and needs a terminal; it refuses to write it to a pipe or a file"
	[[ -n "$(cid app)" ]] || die "the app container is not running; run deploy/deploy.sh first"
	local rc=0
	say ""
	dc exec -T app /bin/server mint-token "${mint_args[@]}" </dev/null || rc=$?
	case "$rc" in
	0)
		say ""
		say "  The line above is the token: copy it into your password manager or the"
		say "  agent's environment now. It is not shown again."
		;;
	2) die "server mint-token: bad arguments (the usage is above); nothing was made" ;;
	*) die "server mint-token refused or failed (exit $rc); the reason is above; nothing was made" ;;
	esac
}

# ---------------------------------------------------------------- off-site backups

# The off-site worker is `offsite`: the same image as the app, its own database
# role and two networks (decisions-T16 #10 to #15). Its pieces here: the role and
# its grants (offsite_grants), the start (start_offsite), and the host commands
# (--backup-keygen, --backup-rotate-signing, --backup-recipient-*, --backup-resume).

# offsite_grants: create the restricted role `offsite` and give it exactly the grants
# of deploy/backup/offsite-grants.sql. Idempotent; it runs on EVERY deploy and update
# (a restore leaves a database with no grants). The password goes to psql on STANDARD
# INPUT as a `\set` line before the file (never an argument, so it is in no argv and
# no log). It is checked to be letters and digits, so the line cannot break.
offsite_grants() {
	say "== database role offsite (grants)"
	check_offsite_password
	{
		printf '\\set offsite_pw '"'"'%s'"'"'\n' "$OFFSITE_DB_PASSWORD"
		cat "$DEPLOY_DIR/backup/offsite-grants.sql"
	} | dc exec -T postgres psql -X -q -v ON_ERROR_STOP=1 -U devops -d devops >/dev/null ||
		die "could not create the offsite role or give it its grants (the reason is above; the app migrates on boot, so the backup tables must exist)"
	say "  role offsite: password set, grants applied"
}

# start_offsite: after the app is healthy (it migrated) and the grants are in. The
# health check is process liveness only (decisions-T16 #46), so a role that cannot
# connect yet does not stop the start; the worker itself waits for it.
start_offsite() {
	say "== start offsite (the off-site backup worker)"
	dc up -d offsite
	wait_healthy offsite 90
}

# api_image: the image of the running stack (the app's), so a throwaway command runs
# the code that is deployed. Else launchpad-api:<version> if that image exists.
api_image() {
	local id img=""
	id="$(cid app)"
	[[ -n "$id" ]] || id="$(cid offsite)"
	if [[ -n "$id" ]]; then
		img="$(docker inspect -f '{{.Config.Image}}' "$id" 2>/dev/null || true)"
	fi
	if [[ -z "$img" ]]; then
		set_version
		img="launchpad-api:$DEPLOY_VERSION"
		docker image inspect "$img" >/dev/null 2>&1 ||
			die "no running app container and no image $img; run deploy/deploy.sh first"
	fi
	printf '%s' "$img"
}

# postgres_lacks_offsite_db: a running postgres container that is not attached to the
# project's offsite_db network (an install from before the off-site worker).
postgres_lacks_offsite_db() {
	local id nets
	id="$(cid postgres)"
	[[ -n "$id" ]] || return 1
	nets="$(docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$id" 2>/dev/null || true)"
	[[ -n "$nets" && " $nets" != *" ${DEPLOY_PROJECT}_offsite_db "* ]]
}

# backup_term_only FLAG: the commands that print or change a backup secret need a
# terminal for stdout, like --mint-token.
backup_term_only() {
	[[ -t 1 ]] || die "$1 needs a terminal; it refuses to write to a pipe or a file"
}

# show_backup_keygen: --backup-keygen. A throwaway container makes an age key pair:
# no network, no mounts, a read-only root, no logs (the private key is on its stdout).
# The private key is printed once; Launchpad stores neither half (decisions-T16 #19).
show_backup_keygen() {
	say "== backup key pair (age)"
	local img rc=0
	img="$(api_image)"
	say ""
	dk run --rm --pull never --network none --log-driver none --read-only \
		--cap-drop ALL --security-opt no-new-privileges "$img" /bin/server offsite keygen </dev/null || rc=$?
	((rc == 0)) || die "server offsite keygen failed (exit $rc); nothing was made"
	say ""
	say "  Copy BOTH lines into your password manager now. The private key is shown"
	say "  once and is NOT stored here: keep it OFF this host. A lost private key means"
	say "  the remote copies cannot be read by anyone."
	say "  Paste the PUBLIC key (age1...) into the Backups page (Settings, Platform)."
}

# show_backup_rotate_signing: --backup-rotate-signing. Runs in the running offsite
# container (it needs the database and BACKUP_ENCRYPTION_KEY) and prints the new
# PUBLIC key; keep a copy of it off this host.
show_backup_rotate_signing() {
	say "== replace the manifest signing key"
	[[ -n "$(cid offsite)" ]] || die "the offsite container is not running; run deploy/deploy.sh first"
	local rc=0
	say ""
	dc exec -T offsite /bin/server offsite rotate-signing </dev/null || rc=$?
	((rc == 0)) || die "server offsite rotate-signing failed (exit $rc); the reason is above; the old key is still in use"
	say ""
	say "  Save the new public key OFF this host. Backups made before this still verify"
	say "  with the old public key: keep that one too."
}

# show_backup_recipient remove|cancel: --backup-recipient-remove / -cancel. The app
# container writes the change-log row (no actor) and mails the platform admins.
show_backup_recipient() {
	local what="$1" rc=0
	say "== backup recipient: $what ${backup_fp:0:12}"
	[[ -n "$(cid app)" ]] || die "the app container is not running; run deploy/deploy.sh first"
	say ""
	dc exec -T app /bin/server "backup-recipient-$what" --fingerprint "$backup_fp" </dev/null || rc=$?
	((rc == 0)) || die "server backup-recipient-$what refused or failed (exit $rc); the reason is above; nothing was changed"
}

# show_backup_resume: --backup-resume (decisions-T16 #37). After a restore or a clone:
# a NEW install_id and instance_id in the database (psql as the owner role, one
# transaction, with a change-log row), then the guard file /offsite-work/instance-id
# in the offsite_work volume. Until then offsite runs no backup run, and so never
# writes into, or prunes, the original's remote folders. The old copies keep the old
# install id and are never pruned by the new one.
show_backup_resume() {
	say "== resume off-site backups with a NEW install id"
	[[ -n "$(cid postgres)" ]] || die "postgres is not running; run deploy/deploy.sh first"
	volume_exists "${DEPLOY_PROJECT}_offsite_work" || die "the volume ${DEPLOY_PROJECT}_offsite_work does not exist; run deploy/deploy.sh first"
	local img install new rc=0
	img="$(api_image)"
	install="$(rand_hex 4)"
	new="$(printf '%s\n' "BEGIN;" \
		"WITH s AS (UPDATE backup_settings SET install_id = :'iid', instance_id = gen_random_uuid(), updated_at = now() RETURNING instance_id)" \
		"INSERT INTO backup_change_log (kind, detail) SELECT 'instance_resumed', jsonb_build_object('newInstallId', :'iid') FROM s;" \
		"SELECT instance_id FROM backup_settings;" \
		"COMMIT;" |
		dc exec -T postgres psql -X -q -t -A -v ON_ERROR_STOP=1 -v "iid=$install" -U devops -d devops -f - | tr -d '\r')" ||
		die "could not give the database a new install id (the reason is above); nothing was changed"
	new="$(printf '%s\n' "$new" | grep -E '^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$' | sed -n 1p)"
	[[ -n "$new" ]] || die "the database did not return the new instance id; nothing was written to the volume"
	dk run --rm --pull never --user 70:70 --network none --read-only --cap-drop ALL \
		--security-opt no-new-privileges -e OFFSITE_WORK_DIR=/offsite-work \
		-v "${DEPLOY_PROJECT}_offsite_work:/offsite-work" "$img" \
		/bin/server offsite resume --instance-id "$new" </dev/null || rc=$?
	((rc == 0)) || die "the database has a new install id, but the guard file was not written (exit $rc); run --backup-resume again"
	say "  new install id $install; the guard file is written. Backups of this copy run again at the next scan."
	say "  Copies made under the old install id stay in the remote folders; this copy never prunes them."
}

# ---------------------------------------------------------------- smoke test

smoke() {
	say "== smoke test (plain HTTP to the published ports, with the real Host names)"
	local addr="$DEPLOY_BIND_ADDR"
	[[ "$addr" == 0.0.0.0 ]] && addr=127.0.0.1
	local web_base="http://$WEB_HOST" files="http://$FILES_HOST"
	local web_to="$WEB_HOST:80:$addr:$DEPLOY_WEB_PORT" files_to="$FILES_HOST:80:$addr:$DEPLOY_FILES_PORT"
	local tmp
	tmp="$(mktemp -d "${DEPLOY_TMPDIR:-${TMPDIR:-/tmp}}/devops-smoke.XXXXXX")"
	chmod 700 "$tmp"
	SMOKE_TMP="$tmp"
	SMOKE_ORG_ID=""
	trap smoke_cleanup EXIT

	ok() { say "  ok  $*"; }
	bad() {
		say "  FAIL $*" >&2
		exit 1
	}
	# web METHOD URL [curl args...] -> status; body in $tmp/body, headers in
	# $tmp/hdr. A connection failure is status 000, so the check that reads
	# it names the step.
	web() {
		local m="$1" u="$2"
		shift 2
		: >"$tmp/hdr"
		curl -sS --max-time 30 --connect-to "$web_to" --connect-to "$files_to" \
			-o "$tmp/body" -D "$tmp/hdr" -w '%{http_code}' -X "$m" "$@" "$u" || true
	}
	# api METHOD PATH [JSON]: with the smoke token, read from stdin by curl.
	api() {
		local m="$1" p="$2" data="${3:-}" args=()
		[[ -n "$data" ]] && args=(-H 'Content-Type: application/json' --data "$data")
		printf 'header = "Authorization: Bearer %s"\n' "$SMOKE_TOKEN" |
			web "$m" "$web_base$p" -K - ${args[@]+"${args[@]}"}
	}
	jf() { python3 -c "import json,sys; b=json.load(open(sys.argv[1])); print($1)" "$tmp/body"; }
	hdr() { { grep -i "^$1:" "$tmp/hdr" || true; } | sed -n 1p | cut -d' ' -f2- | tr -d '\r'; }

	local st csp
	st="$(web GET "$web_base/healthz")"
	if [[ "$st" != 200 ]] || ! grep -q '"ok"' "$tmp/body"; then bad "GET /healthz -> $st"; fi
	ok "API health through nginx: GET /healthz -> 200 {\"status\":\"ok\"}"

	st="$(web GET "$web_base/")"
	if [[ "$st" != 200 ]] || ! grep -q '<div id="root"' "$tmp/body"; then bad "GET / -> $st (no app shell)"; fi
	csp="$(hdr content-security-policy)"
	[[ "$csp" == *"connect-src 'self'"*"https://$FILES_HOST"* ]] || bad "CSP connect-src lacks https://$FILES_HOST: $csp"
	[[ "$csp" == *"img-src 'self' data:"*"https://$FILES_HOST"* ]] || bad "CSP img-src lacks https://$FILES_HOST: $csp"
	[[ "$csp" == *"media-src 'self'"*"https://$FILES_HOST"* ]] || bad "CSP media-src lacks https://$FILES_HOST: $csp"
	[[ "$(hdr x-frame-options)" == DENY && "$(hdr x-content-type-options)" == nosniff ]] || bad "security headers missing on /"
	[[ -n "$(hdr strict-transport-security)" ]] || bad "no HSTS on /"
	ok "web page: GET / -> 200, app shell, CSP names https://$FILES_HOST, security headers set"

	st="$(web GET "$web_base/issues/ORB-1")"
	if [[ "$st" != 200 ]] || ! grep -q '<div id="root"' "$tmp/body"; then bad "SPA fallback -> $st"; fi
	ok "SPA fallback: GET /issues/ORB-1 -> 200 app shell"

	st="$(web GET "$web_base/api/v1/openapi.yaml")"
	[[ "$st" == 200 ]] || bad "GET /api/v1/openapi.yaml -> $st"
	st="$(web GET "$web_base/api/v1/me")"
	[[ "$st" == 401 ]] || bad "GET /api/v1/me with no token -> $st (want 401)"
	ok "API proxied on the same origin: openapi.yaml 200, /me without a token 401"

	st="$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' "http://$addr:$DEPLOY_WEB_PORT/" || true)"
	[[ "$st" == 421 ]] || bad "a request for another Host -> $st (want 421)"
	st="$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' --proxy "http://$addr:$DEPLOY_WEB_PORT" "http://example.com/" || true)"
	[[ "$st" == 421 ]] || bad "nginx as a forward proxy for example.com -> $st (want 421)"
	ok "no open proxy: other Host -> 421, absolute-URI proxy request -> 421"

	smoke_files
	trap - EXIT
	smoke_cleanup
	say "  SMOKE OK"
}

# The attachment round trip, through the server and the files port, in a
# throwaway org. The org, a throwaway agent user who is its admin, and a
# 15-minute token pinned to it are made in SQL (the operator owns the
# database), so the smoke needs no user of the install and never touches
# its orgs or issue numbers. The org is deleted at the end (every org table
# cascades), then the user (an org delete does not cascade to users), and
# the objects too.
#
# While the smoke user exists the install has a user, so on a first deploy
# POST /setup answers 409 for those few seconds; once it is deleted, setup is
# open again with the same link. A smoke user left behind (the cleanup
# failed) keeps setup closed: delete it as the warning says.
# The marker name of the smoke org and user: only rows with it are deleted.
SMOKE_ORG_NAME='deploy.sh smoke test (deleted after each run)'

smoke_files() {
	# odt_ + 43 base62 characters (hex is a subset): the token shape.
	local body
	body="$(rand_hex 22)"
	SMOKE_TOKEN="odt_${body:0:43}"
	local out
	out="$(printf '%s\n' "\\set tok '$SMOKE_TOKEN'" "
BEGIN;
DO \$\$
BEGIN
  -- Only a leftover of an earlier smoke run may be removed: same marker
  -- name, and no member but the smoke user. Anything else is real.
  IF EXISTS (SELECT 1 FROM users u WHERE u.username = 'deploy-smoke' AND (u.name <> '$SMOKE_ORG_NAME' OR NOT u.is_agent)) THEN
    RAISE EXCEPTION 'deploy smoke: a user deploy-smoke exists that a smoke run did not create; rename it or delete it by hand';
  END IF;
  IF EXISTS (SELECT 1 FROM orgs o WHERE o.slug = 'deploy-smoke' AND (o.name <> '$SMOKE_ORG_NAME'
       OR EXISTS (SELECT 1 FROM org_members m JOIN users u ON u.id = m.user_id
                  WHERE m.org_id = o.id AND u.username <> 'deploy-smoke'))) THEN
    RAISE EXCEPTION 'deploy smoke: an org with slug deploy-smoke exists that a smoke run did not create; rename it or delete it by hand';
  END IF;
END
\$\$;
DELETE FROM orgs WHERE slug = 'deploy-smoke' AND name = '$SMOKE_ORG_NAME';
DELETE FROM users WHERE username = 'deploy-smoke' AND name = '$SMOKE_ORG_NAME' AND is_agent;
WITH o AS (INSERT INTO orgs (slug, name) VALUES ('deploy-smoke', '$SMOKE_ORG_NAME') RETURNING id),
     u AS (INSERT INTO users (username, name, is_agent) VALUES ('deploy-smoke', '$SMOKE_ORG_NAME', true) RETURNING id),
     m AS (INSERT INTO org_members (org_id, user_id, role) SELECT o.id, u.id, 'admin' FROM o, u RETURNING org_id),
     t AS (INSERT INTO api_tokens (user_id, org_id, kind, name, token_hash, prefix, scopes, expires_at)
           SELECT u.id, m.org_id, 'pat', 'deploy-smoke', sha256(convert_to(:'tok', 'UTF8')), left(:'tok', 12),
                  ARRAY['read','write','admin'], now() + interval '15 minutes' FROM m, u RETURNING org_id)
SELECT org_id FROM t;
COMMIT;" | dc exec -T postgres psql -X -q -t -A -v ON_ERROR_STOP=1 -U devops -d devops)" || bad "could not create the smoke org (the reason is above)"
	SMOKE_ORG_ID="$(printf '%s\n' "$out" | grep -E '^[0-9a-f-]{36}$' | sed -n 1p)"
	[[ -n "$SMOKE_ORG_ID" ]] || bad "no smoke org id"
	local api_p="/api/v1/orgs/deploy-smoke" st
	st="$(api POST "$api_p/teams" '{"key":"SMK","name":"Deploy smoke"}')"
	[[ "$st" == 201 ]] || bad "create team -> $st: $(cat "$SMOKE_TMP/body")"
	st="$(api POST "$api_p/issues" '{"team":"SMK","title":"deploy smoke"}')"
	[[ "$st" == 201 ]] || bad "create issue -> $st: $(cat "$SMOKE_TMP/body")"
	local issue
	issue="$(jf 'b["identifier"]')"

	local payload
	payload="dev-ops deploy smoke $(date -u +%FT%TZ) $RANDOM"
	printf '%s' "$payload" >"$SMOKE_TMP/upload.txt"
	local size=${#payload}
	st="$(api POST "$api_p/attachments/prepare" "{\"filename\":\"smoke.txt\",\"contentType\":\"text/plain\",\"sizeBytes\":$size}")"
	[[ "$st" == 200 || "$st" == 201 ]] || bad "prepare -> $st: $(cat "$SMOKE_TMP/body")"
	local put_url key
	put_url="$(jf 'b["uploadUrl"]')"
	key="$(jf 'b["uploadKey"]')"
	[[ "$put_url" == "https://$FILES_HOST/"* ]] || bad "uploadUrl is not on https://$FILES_HOST/"
	ok "prepare: presigned PUT on https://$FILES_HOST (signed for that Host)"

	# CORS on the files host: the web origin may, another origin may not.
	web OPTIONS "$files/$S3_BUCKET/$key" -H "Origin: https://$WEB_HOST" \
		-H 'Access-Control-Request-Method: PUT' -H 'Access-Control-Request-Headers: content-type' >/dev/null
	[[ "$(hdr access-control-allow-origin)" == "https://$WEB_HOST" ]] || bad "silo CORS does not allow https://$WEB_HOST"
	web OPTIONS "$files/$S3_BUCKET/$key" -H 'Origin: https://evil.example' \
		-H 'Access-Control-Request-Method: PUT' >/dev/null
	[[ -z "$(hdr access-control-allow-origin)" ]] || bad "silo CORS allows https://evil.example"
	ok "files CORS: preflight from https://$WEB_HOST allowed, from another origin refused"

	st="$(web PUT "http://${put_url#https://}" -H 'Content-Type: text/plain' --data-binary "@$SMOKE_TMP/upload.txt")"
	[[ "$st" == 200 ]] || bad "presigned PUT through :$DEPLOY_FILES_PORT -> $st"
	ok "presigned PUT through port $DEPLOY_FILES_PORT -> 200 ($size bytes)"

	st="$(api POST "$api_p/issues/$issue/attachments" "{\"kind\":\"file\",\"uploadKey\":\"$key\"}")"
	[[ "$st" == 201 ]] || bad "attach -> $st: $(cat "$SMOKE_TMP/body")"
	local dl
	dl="$(jf 'b["downloadUrl"]')"
	[[ "$dl" == "https://$WEB_HOST/api/v1/"* ]] || bad "downloadUrl is not on https://$WEB_HOST: $dl"
	st="$(api GET "${dl#https://"$WEB_HOST"}")"
	[[ "$st" == 302 ]] || bad "download -> $st (want 302)"
	local loc
	loc="$(hdr location)"
	[[ "$loc" == "https://$FILES_HOST/"* ]] || bad "the download redirect is not on https://$FILES_HOST"
	st="$(web GET "http://${loc#https://}")"
	[[ "$st" == 200 ]] || bad "presigned GET through :$DEPLOY_FILES_PORT -> $st"
	cmp -s "$SMOKE_TMP/body" "$SMOKE_TMP/upload.txt" || bad "the downloaded bytes differ from the upload"
	ok "attach + download: downloadUrl on https://$WEB_HOST, 302 to https://$FILES_HOST, presigned GET -> 200, same bytes"
}

smoke_cleanup() {
	local rc=$?
	if [[ -n "${SMOKE_ORG_ID:-}" ]]; then
		printf '%s\n' "BEGIN;
DELETE FROM orgs WHERE id = '$SMOKE_ORG_ID' AND slug = 'deploy-smoke' AND name = '$SMOKE_ORG_NAME';
DELETE FROM users WHERE username = 'deploy-smoke' AND name = '$SMOKE_ORG_NAME' AND is_agent;
COMMIT;" |
			dc exec -T postgres psql -X -q -v ON_ERROR_STOP=1 -U devops -d devops >/dev/null ||
			warn "could not delete the smoke org and user; delete them by hand (a leftover user keeps first-run setup closed): DELETE FROM orgs WHERE slug = 'deploy-smoke'; DELETE FROM users WHERE username = 'deploy-smoke';"
		# shellcheck disable=SC2016  # expanded inside the container
		dc exec -T -e SMOKE_PREFIX="org/$SMOKE_ORG_ID/" silo sh -c '
export MC_CONFIG_DIR=/tmp/.mcli
export MC_HOST_local="http://${MINIO_ROOT_USER}:${MINIO_ROOT_PASSWORD}@127.0.0.1:9000"
mcli --quiet rm --recursive --force "local/'"$S3_BUCKET"'/${SMOKE_PREFIX}" >/dev/null' ||
			warn "could not delete the smoke objects under org/$SMOKE_ORG_ID/"
		SMOKE_ORG_ID=""
	fi
	[[ -n "${SMOKE_TMP:-}" ]] && rm -rf "$SMOKE_TMP"
	SMOKE_TMP=""
	return $rc
}

# ---------------------------------------------------------------- summary

# first_run_link: the end of a first deploy. Setup is open (no
# user yet). On a terminal it mints a NEW link with the same call as
# --setup-link and prints it, so setup is one command. The link is a
# credential: with stdout not a terminal (CI, a pipe, a file) it is NOT
# printed and the hint is shown instead. So too when CI is set (not empty,
# not false or 0) or DEPLOY_NO_SETUP_LINK=1: a pty in a build log is still a
# log. --update only shows the hint (its output is the backup and update
# log). It never fails the deploy.
ci_is_set() {
	case "${CI:-}" in
	"" | 0 | [Ff][Aa][Ll][Ss][Ee]) return 1 ;;
	esac
}
first_run_link() {
	local why="" rc=0
	if [[ "$mode" != deploy ]]; then
		why="this is not a plain deploy run (--update shows the hint only)"
	elif [[ ! -t 1 ]]; then
		why="stdout is not a terminal"
	elif ci_is_set; then
		why="CI is set (a build log keeps what a terminal shows)"
	elif [[ "${DEPLOY_NO_SETUP_LINK:-}" == 1 ]]; then
		why="DEPLOY_NO_SETUP_LINK=1"
	fi
	if [[ -z "$why" ]]; then
		say "  FIRST RUN: setup is open (no user yet)."
		say ""
		dc exec -T app /bin/server setup-link </dev/null || rc=$?
		case "$rc" in
		0)
			say ""
			say "  Open the link above in a browser to create the first user, the org and"
			say "  its first team (docs/deploy.md 'First-run setup')."
			say "  Valid 24 hours, single use."
			say "  Running deploy.sh again before setup is done prints a NEW link; each"
			say "  new link closes the older ones, including the one in the app log."
			return 0
			;;
		3)
			say "  setup is complete: this install has a user, so there is no setup link."
			return 0
			;;
		esac
		warn "could not make the setup link (server setup-link exit $rc); the deploy itself worked"
		why="server setup-link failed"
	fi
	say "  FIRST RUN: setup is open (no user yet). No setup link is printed: $why."
	say "  The link is a credential and goes to a terminal only. On a terminal, run"
	say "    deploy/deploy.sh --setup-link"
	say "  and open the link it prints (24 hours, single use) to create the first"
	say "  user, the org and its first team (docs/deploy.md 'First-run setup')."
}

# git_private_state: how the git API clients treat private addresses
# (GIT_ALLOW_PRIVATE_PROVIDERS / GIT_ALLOW_PRIVATE_CIDRS in deploy/.env, read
# here, passed through by the compose file).
git_private_state() {
	local on cidrs
	on="$(env_get GIT_ALLOW_PRIVATE_PROVIDERS)"
	cidrs="$(env_get GIT_ALLOW_PRIVATE_CIDRS)"
	if [[ "$on" == "1" || "$on" == "true" ]]; then
		echo "private ranges OPEN (10/8, 172.16/12, 192.168/16, fc00::/7, 100.64/10)${cidrs:+, plus $cidrs}; the app's own networks stay refused"
	elif [[ -n "$cidrs" ]]; then
		echo "private ranges open only for $cidrs; the app's own networks stay refused"
	else
		echo "public hosts only (GIT_ALLOW_PRIVATE_PROVIDERS and GIT_ALLOW_PRIVATE_CIDRS unset)"
	fi
}

# offsite_state: one line for the summary. Never a secret.
offsite_state() {
	local cidrs h
	cidrs="$(env_get BACKUP_ALLOW_PRIVATE_CIDRS)"
	h="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$(cid offsite)" 2>/dev/null || true)"
	echo "worker ${h:-not running}, role offsite, BACKUP_ENCRYPTION_KEY set; private ranges for destinations: ${cidrs:-none (BACKUP_ALLOW_PRIVATE_CIDRS unset)}; add recipients and destinations on the Backups page"
}

summary() {
	local pg silo_v api_img web_img setup
	pg="$(dc exec -T postgres postgres --version 2>/dev/null | tr -d '\r' || true)"
	silo_v="$(dc exec -T silo silo --version 2>/dev/null | sed -n 1p | tr -d '\r' || true)"
	api_img="$(docker inspect -f '{{.Config.Image}}' "$(cid app)" 2>/dev/null || true)"
	web_img="$(docker inspect -f '{{.Config.Image}}' "$(cid web)" 2>/dev/null || true)"
	setup="$(setup_required)"
	say ""
	say "== Launchpad is up (project $DEPLOY_PROJECT, version $DEPLOY_VERSION)"
	say "  web + API  https://$WEB_HOST   -> $DEPLOY_BIND_ADDR:$DEPLOY_WEB_PORT (nginx)"
	say "  files      https://$FILES_HOST -> $DEPLOY_BIND_ADDR:$DEPLOY_FILES_PORT (silo S3 API)"
	say "  images     $api_img, $web_img"
	say "  postgres   ${pg:-?}"
	say "  silo       ${silo_v:-?}"
	say "  login      $([[ -n "$POSTMARK_SERVER_TOKEN" ]] && echo "Postmark, from $POSTMARK_FROM (unless the app's mail settings say otherwise)" || echo "no POSTMARK_SERVER_TOKEN: OFF unless a platform admin sets mail in the app (Platform settings)")"
	say "  backups    nightly at $BACKUP_AT_UTC UTC, $BACKUP_KEEP_DAYS days kept (volume ${DEPLOY_PROJECT}_backups)"
	say "  git hosts  $(git_private_state)"
	say "  off-site   $(offsite_state)"
	case "$setup" in
	yes) ;;
	unknown) say "  setup      could not ask the database whether first-run setup is done" ;;
	esac
	say ""
	say "Not proven here: TLS, DNS and the outer proxy (it must forward with the Host"
	say "header unchanged), and real Postmark delivery. See docs/deploy.md."
	# Last, so the link is the final thing on the screen.
	if [[ "$setup" == yes ]]; then
		say ""
		first_run_link
	fi
}

# ---------------------------------------------------------------- nuke

# dk ARGS...: docker in the clean environment (docker_clean, the allowlist
# runner), like every compose call. Every --nuke call that removes something
# goes through dk or dc; check_prereqs and guard_project are read-only checks
# that call docker directly.
dk() { docker_clean -- docker "$@"; }

# nuke_inventory: what docker still holds of $DEPLOY_PROJECT, one name per
# line, in NUKE_C (containers), NUKE_N (networks) and NUKE_V (volumes: the
# project-labelled ones plus the five names, labelled or not). A failing
# docker call is an error, never "nothing left".
nuke_inventory() {
	local f="label=com.docker.compose.project=$DEPLOY_PROJECT" all v
	NUKE_C="$(dk ps -a --filter "$f" --format '{{.Names}}')" ||
		die "nuke: 'docker ps' failed; cannot tell what is left of '$DEPLOY_PROJECT'"
	NUKE_N="$(dk network ls --filter "$f" --format '{{.Name}}')" ||
		die "nuke: 'docker network ls' failed; cannot tell what is left of '$DEPLOY_PROJECT'"
	NUKE_V="$(dk volume ls -q --filter "$f")" ||
		die "nuke: 'docker volume ls' failed; cannot tell what is left of '$DEPLOY_PROJECT'"
	all="$(dk volume ls -q)" ||
		die "nuke: 'docker volume ls -q' failed; cannot tell what is left of '$DEPLOY_PROJECT'"
	for v in postgres_data silo_data backups import_work offsite_work; do
		if grep -qxF -- "${DEPLOY_PROJECT}_$v" <<<"$all" && ! grep -qxF -- "${DEPLOY_PROJECT}_$v" <<<"$NUKE_V"; then
			NUKE_V="${NUKE_V:+$NUKE_V$'\n'}${DEPLOY_PROJECT}_$v"
		fi
	done
}

# words LIST: a newline list on one line ("none" when empty).
words() {
	local w out=""
	for w in $1; do out="$out $w"; done
	printf '%s' "${out# }"
	[[ -n "$out" ]] || printf 'none'
}

# nuke: delete this deploy's containers, networks and ALL its volumes,
# after the operator typed the project name (or gave it to
# --yes-delete-everything). Nothing is removed before that.
nuke() {
	say "== nuke: delete this deploy and ALL its data"
	# An exported DEPLOY_PROJECT is compared with the file, never used.
	local shell_project="${DEPLOY_PROJECT:-}"
	[[ -f "$ENV_FILE" ]] ||
		die "nuke: $ENV_FILE not found, so the project is unknown. --nuke deletes only the project named in deploy/.env; nothing was done"
	load_env
	if [[ -n "$shell_project" && "$shell_project" != "$DEPLOY_PROJECT" ]]; then
		die "nuke: DEPLOY_PROJECT '$shell_project' in your shell is not the project '$DEPLOY_PROJECT' in $ENV_FILE. --nuke acts only on the file's project; unset DEPLOY_PROJECT and re-run. Nothing was done"
	fi
	[[ "$DEPLOY_PROJECT" =~ ^[a-z0-9][a-z0-9_-]*$ ]] ||
		die "nuke: DEPLOY_PROJECT '$DEPLOY_PROJECT' in $ENV_FILE is not a compose project name; nothing was done"
	# The local dev stacks: dev-ops (shared), dev-ops-<ticket> (worktrees),
	# dev-ops-ci. Their compose file declares the same volume names.
	if [[ "$DEPLOY_PROJECT" =~ ^dev-ops($|-) ]]; then
		die "nuke: '$DEPLOY_PROJECT' is a dev stack's project name (dev-ops, dev-ops-<ticket>, dev-ops-ci). --nuke never runs on it; nothing was done"
	fi
	check_prereqs
	# Refuses a project that another compose file made, or whose volumes
	# lack the dev.orbit.deploy=production label.
	guard_project
	# No env_topup here: --nuke must not write to a file it may
	# delete (--nuke-env), or refuse a damaged one. After a hand pull the
	# compose file may not interpolate, so `dc down` below can fail; the
	# one-by-one removal that follows it says so and finishes the job.
	nuke_inventory
	# --nuke-images: the images the project's app and web containers run, as
	# "name|id" words. By id: the launchpad-api:<commit> tag is global, and a
	# later build of the same commit by another deploy moves it to that
	# deploy's new image (measured); this project's image is then untagged,
	# and the tag is no longer this project's to remove. Before the rename the
	# images were called dev-ops-api / dev-ops-web: a host deployed before the
	# rename still has containers on them, so both names are matched (image
	# names only; the dev-ops PROJECT-name refusal above is a different rule).
	local imgs="" svc c pair list
	if ((nuke_images)); then
		for svc in app web; do
			list="$(dk ps -aq --filter "label=com.docker.compose.project=$DEPLOY_PROJECT" \
				--filter "label=com.docker.compose.service=$svc")" ||
				die "nuke: 'docker ps' failed; nothing was done"
			for c in $list; do
				pair="$(dk inspect -f '{{.Config.Image}}|{{.Image}}' "$c")" ||
					die "nuke: 'docker inspect $c' failed; nothing was done"
				# Only this file's built images, never a base image.
				[[ "$pair" =~ ^(launchpad|dev-ops)-(api|web):[A-Za-z0-9_.-]+\|sha256:[0-9a-f]{64}$ ]] || continue
				[[ " $imgs " == *" $pair "* ]] || imgs="${imgs:+$imgs }$pair"
			done
		done
	fi
	local n
	n="$(setup_link_count)"

	say ""
	say "  project     $DEPLOY_PROJECT (DEPLOY_PROJECT in $ENV_FILE)"
	say "  containers  $(words "$NUKE_C")"
	say "  networks    $(words "$NUKE_N")"
	say "  volumes     $(words "$NUKE_V")"
	say "    ${DEPLOY_PROJECT}_postgres_data  the DATABASE: every ticket, comment and user"
	say "    ${DEPLOY_PROJECT}_silo_data      the ATTACHMENTS: every uploaded file"
	say "    ${DEPLOY_PROJECT}_backups        the BACKUPS: every nightly dump, so no restore point is left"
	say "    ${DEPLOY_PROJECT}_import_work    the web import's EXPORTS: ticket text and emails from Linear"
	say "    ${DEPLOY_PROJECT}_offsite_work   the off-site backup worker's SCRATCH space and its instance guard file"
	if ((nuke_images)); then
		list=""
		for pair in $imgs; do
			c="${pair#*|sha256:}"
			list="$list ${pair%%|*}=${c:0:12}"
		done
		say "  images      $(words "$list") (--nuke-images; name=image id)"
	else
		say "  images      kept (--nuke-images deletes the project's launchpad-api / launchpad-web images, and the old dev-ops-api / dev-ops-web ones)"
	fi
	if ((nuke_env)); then
		say "  deploy/.env DELETED too (--nuke-env): the next deploy.sh generates new secrets"
	else
		say "  deploy/.env kept: a re-run of deploy.sh reuses its host names and secrets on the empty volumes"
	fi
	case "$n" in
	0) ;;
	unknown) say "  NOTE: the app log could not be read; a setup link in it goes with it (a new deploy prints a new one)" ;;
	*) say "  NOTE: the app log holds $n setup link line(s); they go with it (a new deploy prints a new link)" ;;
	esac
	say ""
	say "  This cannot be undone. Copy the backups off the host first if you may need"
	say "  the data (docs/deploy.md 'Backup and restore'). Never after the Linear cutover."
	say ""

	if ((nuke_confirm_set)); then
		[[ "$nuke_confirm" == "$DEPLOY_PROJECT" ]] ||
			die "nuke: --yes-delete-everything '$nuke_confirm' is not the project '$DEPLOY_PROJECT'; nothing was done"
	elif ((interactive)); then
		local answer=""
		read -r -p "Type the project name to delete all of it (anything else stops): " answer || answer=""
		[[ "$answer" == "$DEPLOY_PROJECT" ]] ||
			die "nuke: the name typed is not '$DEPLOY_PROJECT'; nothing was done"
	else
		die "nuke: no terminal to confirm on. Give --yes-delete-everything <project> with the exact project name (a bare --yes is not accepted); nothing was done"
	fi

	say "== nuke: removing project $DEPLOY_PROJECT"
	dc down -v --remove-orphans --timeout 30 ||
		warn "docker compose down failed; removing what is left of the project one by one"
	# Anything compose left (a container or network it did not match, a
	# labelled volume the file does not declare): the guard checked each.
	local x
	nuke_inventory
	for x in $NUKE_C; do dk rm -f -v "$x" >/dev/null || warn "could not remove container $x"; done
	for x in $NUKE_N; do dk network rm "$x" >/dev/null || warn "could not remove network $x"; done
	for x in $NUKE_V; do dk volume rm "$x" >/dev/null || warn "could not remove volume $x"; done
	nuke_inventory
	if [[ -n "${NUKE_C//[[:space:]]/}${NUKE_N//[[:space:]]/}${NUKE_V//[[:space:]]/}" ]]; then
		die "nuke: project '$DEPLOY_PROJECT' is NOT fully removed. Left: containers $(words "$NUKE_C"); networks $(words "$NUKE_N"); volumes $(words "$NUKE_V"). Something outside the project may use them (docker ps -a). $ENV_FILE was kept. Fix it and run --nuke again"
	fi
	say "  containers, networks and volumes of $DEPLOY_PROJECT: none left"

	local left_imgs="" all_ids cur users name id
	for pair in $imgs; do
		name="${pair%%|*}"
		id="${pair#*|}"
		all_ids="$(dk image ls -a -q --no-trunc)" || die "nuke: 'docker image ls' failed; cannot tell whether $name is left"
		if ! grep -qxF -- "$id" <<<"$all_ids"; then
			say "  image $name (${id:7:12}): not there (already removed)"
			continue
		fi
		# A container outside the project on the same image (another deploy
		# of this commit before its rebuild): keep it, and name the container.
		users="$(dk ps -a --filter "ancestor=$id" --format '{{.Names}}')" ||
			die "nuke: 'docker ps' failed; cannot tell whether another container uses $name"
		if [[ -n "${users//[[:space:]]/}" ]]; then
			warn "image $name (${id:7:12}) is kept: container(s) outside the project use it: $(words "$users")"
			left_imgs="${left_imgs:+$left_imgs }$name"
			continue
		fi
		# By the tag while it still names this image; else by id, and the tag
		# (now another deploy's image) is left alone.
		cur="$(dk image ls -q --no-trunc "$name")" || die "nuke: 'docker image ls' failed; cannot tell what $name names"
		if grep -qxF -- "$id" <<<"$cur"; then
			dk image rm "$name" >/dev/null || warn "could not remove image $name"
		else
			dk image rm "$id" >/dev/null || warn "could not remove image ${id:7:12} (was $name)"
		fi
		all_ids="$(dk image ls -a -q --no-trunc)" || die "nuke: 'docker image ls' failed; cannot tell whether $name is left"
		if grep -qxF -- "$id" <<<"$all_ids"; then
			left_imgs="${left_imgs:+$left_imgs }$name"
		else
			say "  image $name (${id:7:12}): removed"
		fi
	done
	if ((nuke_env)); then
		rm -f -- "$ENV_FILE"
		[[ ! -e "$ENV_FILE" ]] || die "nuke: could not delete $ENV_FILE"
		say "  $ENV_FILE: deleted"
	else
		say "  $ENV_FILE: kept; a re-run reuses its host names and secrets (--nuke-env deletes it)"
	fi
	[[ -z "$left_imgs" ]] || die "nuke: the containers, networks and volumes are gone, but these images are left: $left_imgs (named above). Remove them when no other deploy uses them"
	say ""
	say "== nuke done: nothing of project $DEPLOY_PROJECT is left."
	say "  deploy/deploy.sh now makes a fresh first deploy: an empty database and a"
	say "  new first-run setup link (print one with deploy/deploy.sh --setup-link)."
}

# ---------------------------------------------------------------- main

deploy() {
	check_prereqs
	ensure_env
	set_version
	say "== version $DEPLOY_VERSION"
	guard_project
	# The normal --update backed up before the pull. Only a re-exec by an
	# older deploy.sh (--update-continue) gets here without that backup.
	if [[ "$mode" == update ]] && ((backed_up == 0)); then
		update_backup after-pull
	fi
	check_ports
	say "== build"
	dc build app web
	say "== start postgres and silo"
	# (decisions-T16 #48): the first update past the off-site backups
	# change recreates postgres so that it joins the offsite_db network.
	if postgres_lacks_offsite_db; then
		say "  postgres is not on the offsite_db network yet: it is recreated now, and the database restarts briefly (a short downtime, not measured)"
	fi
	# Every `up` names its services: a bare `dc up -d --remove-orphans` would start
	# offsite before the app has migrated and the role has its grants.
	dc up -d --remove-orphans postgres silo
	wait_healthy postgres
	wait_healthy silo
	silo_bootstrap
	say "== start the app, web and backup"
	dc up -d --remove-orphans app web backup
	wait_healthy app 180
	wait_healthy web 60
	wait_healthy backup 60
	# The app has migrated (it is healthy): the role's grants name tables that the
	# migrations make. Then offsite, which waits for the role and the migration.
	offsite_grants
	start_offsite
	smoke
	if ! dc exec -T backup sh -c 'ls /backups/devops-*.dump' >/dev/null 2>&1; then
		say "== first backup"
		backup_now
	fi
	summary
}

# The tests source this file to call summary() alone (test-env.sh case 17).
# It is no mode: it only skips the main case, and runs nothing else.
if [[ "${DEPLOY_SH_SOURCE_ONLY:-}" == 1 && "${BASH_SOURCE[0]}" != "$0" ]]; then
	return 0
fi

case "$mode" in
deploy) deploy ;;
update)
	if ((skip_pull == 0)); then
		check_prereqs
		# Refuse BEFORE the pull, so a refused update leaves the checkout
		# at the version that is running. env_topup adds the keys that a
		# hand-pulled (new) compose file needs and the .env lacks.
		if [[ -f "$ENV_FILE" ]]; then
			load_env
			guard_project
			env_topup
		fi
		check_clean_checkout
		# The restore point BEFORE the pull: the compose file is still the
		# running version's (unless the person pulled by hand), so starting what
		# the dump needs changes nothing.
		update_backup before-pull
		git_update
	fi
	deploy
	;;
setuplink)
	# Before anything else: a piped run must not even start.
	[[ -t 1 ]] || die "--setup-link prints a credential and needs a terminal; it refuses to write it to a pipe or a file"
	load_env
	guard_project
	env_topup
	show_setup_link
	;;
minttoken)
	# Before anything else: a piped run must not even start.
	[[ -t 1 ]] || die "--mint-token prints a credential and needs a terminal; it refuses to write it to a pipe or a file"
	load_env
	guard_project
	env_topup
	show_mint_token
	;;
backup-now)
	load_env
	guard_project
	env_topup lenient
	backup_now start
	;;
backupkeygen)
	# Before anything else: a piped run must not even start.
	backup_term_only --backup-keygen
	load_env
	guard_project
	env_topup
	show_backup_keygen
	;;
backuprotate)
	backup_term_only --backup-rotate-signing
	load_env
	guard_project
	env_topup
	show_backup_rotate_signing
	;;
backuprecipientremove | backuprecipientcancel)
	backup_term_only "--backup-recipient-${mode#backuprecipient}"
	load_env
	guard_project
	env_topup
	show_backup_recipient "${mode#backuprecipient}"
	;;
backupresume)
	backup_term_only --backup-resume
	load_env
	guard_project
	env_topup
	show_backup_resume
	;;
smoke)
	load_env
	guard_project
	env_topup
	smoke
	;;
nuke) nuke ;;
esac
