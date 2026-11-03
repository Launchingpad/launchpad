#!/usr/bin/env bash
# Restore a Launchpad pg_dump into a SCRATCH database and count its rows.
# It never writes to the live `devops` database. Runbook:
# docs/deploy.md "Backup and restore" and "Restore from a remote copy".
#
#   deploy/restore.sh latest                  newest dump in the backups volume
#   deploy/restore.sh /path/devops-....dump   a dump file on this host
#
#   deploy/restore.sh (--remote NAME | --remote-config FILE) [--identity FILE|-]
#       (--verify-key FILE | --trust-remote) [--install-id ID] [--allow-cidr CIDR]
#       [--work-dir DIR] [--image IMAGE] [--verify-only] latest|STEM
#                     a signed, encrypted copy from an off-site destination
#
#   deploy/restore.sh (--remote NAME | --remote-config FILE) [--identity FILE|-]
#       (--verify-key FILE | --trust-remote) [--install-id ID] [--allow-cidr CIDR]
#       [--work-dir DIR] [--image IMAGE] --files (--files-out DIR | --verify-only)
#       [--files-key KEY] latest|FILES-STEM
#                     the attachments (files) of a remote copy, as plain files under
#                     DIR (a new or empty folder), checked against the signed list
#
#   --compare         also count the live database and fail on any difference
#                     (meaningful right after a backup, before new writes)
#   --keep            keep the scratch database after `restore: OK` (default:
#                     drop it at the end). A failed restore drops it even
#                     with --keep, so a partial copy is never left to swap in
#   --name NAME       the scratch database name (default restore_<UTC time>)
#   --container NAME  run in this docker container (it needs the postgres
#                     client tools and PG* variables) instead of the stack's
#                     `backup` service; deploy/test-restore.sh uses it
#
# Remote copies (docs/internal/decisions-T16-backups.md #39):
#   --remote NAME     fetch through the stack's `offsite` container, from the
#                     saved destination NAME
#   --remote-config FILE  fetch from the destination in a JSON file (mode 0600,
#                     yours): for a new host. {"type":"s3","endpoint":...}
#   --identity FILE   the age PRIVATE key (a file). `-` reads one line from the
#                     standard input (on a terminal: a hidden prompt, as without the
#                     flag).
#                     The key is never an argument or an environment value
#   --verify-key FILE the saved signing PUBLIC key; --trust-remote skips the
#                     signature check (a bad dump can run SQL as the bootstrap
#                     superuser). Before anything is fetched it asks for the install
#                     id, typed on a terminal; with no terminal give
#                     --yes-trust-remote INSTALL_ID
#   --install-id ID   the install id of the remote files (8 hex); needed with
#                     --remote-config unless the database tells it
#   --allow-cidr CIDR with --remote-config: private range the fetch may reach
#   --work-dir DIR    where the encrypted object is unpacked (default
#                     ${TMPDIR:-/var/tmp}); a 0700 folder is made and removed
#   --image IMAGE     the server image for the throwaway commands (default: the
#                     stack's); see the runbook for the docker build on a new host
#   --verify-only     fetch, verify, check the key; restore nothing
#   --files           restore the attachments, not the database. Needs
#                     --files-out DIR (a folder that does not exist or is empty:
#                     nothing is ever overwritten or deleted) unless --verify-only
#   --files-out DIR   where the objects are written: DIR/<silo key>, mode 0600
#   --files-key KEY   only the object with this silo key
#   --no-host-age     decrypt in a container even when `age` is on this host
#   --fetch-network N tests only: join this docker network for the fetch
#
# Prints one `rows <table> <count>` line per table, then a summary with the
# measured restore time. Exit 0 only when the restore and every check pass.
set -euo pipefail

# shellcheck source=deploy/lib.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib.sh"

keep=0 compare=0 container="" name="" dump=""
remote="" rconfig="" identity="" verify_key="" trust=0 install_id="" allow_cidr=""
work_parent="" image="" verify_only=0 no_host_age=0 fetch_net=""
files=0 files_out="" files_key="" yes_trust=""
while [[ $# -gt 0 ]]; do
	case "$1" in
	--keep) keep=1 ;;
	--compare) compare=1 ;;
	--container)
		container="${2:?--container needs a name}"
		shift
		;;
	--name)
		name="${2:?--name needs a value}"
		shift
		;;
	--remote)
		remote="${2:?--remote needs a destination name}"
		shift
		;;
	--remote-config)
		rconfig="${2:?--remote-config needs a file}"
		shift
		;;
	--identity)
		identity="${2:?--identity needs a file, or - for the standard input}"
		shift
		;;
	--verify-key)
		verify_key="${2:?--verify-key needs a file}"
		shift
		;;
	--trust-remote) trust=1 ;;
	--yes-trust-remote)
		yes_trust="${2:?--yes-trust-remote needs the install id}"
		shift
		;;
	--install-id)
		install_id="${2:?--install-id needs a value}"
		shift
		;;
	--allow-cidr)
		allow_cidr="${2:?--allow-cidr needs a value}"
		shift
		;;
	--work-dir)
		work_parent="${2:?--work-dir needs a directory}"
		shift
		;;
	--image)
		image="${2:?--image needs an image name}"
		shift
		;;
	--fetch-network)
		fetch_net="${2:?--fetch-network needs a network name}"
		shift
		;;
	--files) files=1 ;;
	--files-out)
		files_out="${2:?--files-out needs a folder}"
		shift
		;;
	--files-key)
		files_key="${2:?--files-key needs a silo key}"
		shift
		;;
	--verify-only) verify_only=1 ;;
	--no-host-age) no_host_age=1 ;;
	-h | --help)
		sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//'
		exit 0
		;;
	-*) die "unknown flag: $1" ;;
	*) dump="$1" ;;
	esac
	shift
done

is_remote=0
[[ -n "$remote" || -n "$rconfig" ]] && is_remote=1
if ((is_remote)); then
	[[ -n "$dump" ]] || die "usage: deploy/restore.sh (--remote NAME | --remote-config FILE) [--identity FILE] (--verify-key FILE | --trust-remote [--yes-trust-remote INSTALL_ID]) [--allow-cidr CIDR] [--work-dir DIR] latest|STEM"
else
	[[ -n "$dump" ]] || die "usage: deploy/restore.sh [--compare] [--keep] [--name NAME] latest|DUMP_FILE"
	if [[ -n "$identity$verify_key$install_id$allow_cidr$work_parent$image$fetch_net$files_out$files_key$yes_trust" ]] || ((trust || verify_only || no_host_age || files)); then
		die "a remote-copy flag needs --remote NAME or --remote-config FILE"
	fi
fi
name="${name:-restore_$(date -u +%Y%m%dt%H%M%S)}"
[[ "$name" =~ ^[a-z_][a-z0-9_]{0,62}$ ]] || die "--name must be a plain lower-case identifier"
case "$name" in devops | postgres | template0 | template1) die "refusing to restore into '$name'" ;; esac

stem_re='^launchpad-[0-9a-f]{8}-[0-9]{8}T[0-9]{6}Z-[0-9a-f]{4}$'
files_stem_re='^launchpad-files-[0-9a-f]{8}-list-[0-9]{8}T[0-9]{6}Z-[0-9a-f]{4}$'
if ((files)); then stem_re="$files_stem_re"; fi
if ((!files)) && [[ -n "$files_out$files_key" ]]; then die "--files-out and --files-key go with --files"; fi
if ((files)); then
	((compare || keep)) && die "--files restores attachments, not a database: --compare and --keep do not apply"
	[[ -z "$files_out" || "$files_out" != -* ]] || die "--files-out is not a folder name"
	if ((!verify_only)) && [[ -z "$files_out" ]]; then die "--files needs --files-out DIR (or --verify-only)"; fi
	[[ -z "$files_key" || ("$files_key" == org/* && "$files_key" != *[[:cntrl:]]*) ]] || die "--files-key is a silo key like org/<org id>/<year>/<id>/<file name>"
fi
if ((is_remote)); then
	[[ -z "$remote" || -z "$rconfig" ]] || die "give --remote NAME or --remote-config FILE, not both"
	[[ "$dump" == latest || "$dump" =~ $stem_re ]] || die "the backup to restore is 'latest' or a STEM like launchpad-0123abcd-20261028T031500Z-9f3a (with --files: launchpad-files-0123abcd-list-20261028T031500Z-9f3a); a name without its extension, not a path"
	if ((trust)) && [[ -n "$verify_key" ]]; then die "give --verify-key FILE or --trust-remote, not both"; fi
	if [[ -n "$yes_trust" ]] && ((!trust)); then die "--yes-trust-remote goes with --trust-remote"; fi
	if ((!trust)) && [[ -z "$verify_key" ]]; then
		die "a remote copy needs --verify-key FILE (the signing public key you saved) or, knowing the risk, --trust-remote"
	fi
	[[ -z "$install_id" || "$install_id" =~ ^[0-9a-f]{8}$ ]] || die "--install-id must be 8 lower-case hex characters"
	if [[ -n "$remote" && (-n "$allow_cidr" || -n "$fetch_net") ]]; then
		die "--allow-cidr and --fetch-network go with --remote-config; the stack's offsite worker uses BACKUP_ALLOW_PRIVATE_CIDRS"
	fi
	if [[ -n "$allow_cidr" ]]; then valid_backup_cidrs "$allow_cidr" || die "--allow-cidr is not a list of private CIDRs (see docs/deploy.md)"; fi
	[[ -z "$fetch_net" || "$fetch_net" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] || die "--fetch-network is not a network name"
	if [[ -n "$remote" ]]; then
		[[ "$remote" != -* && "$remote" != *[[:cntrl:]]* ]] || die "--remote is not a destination name"
	fi
fi

# abs_path FILE: an absolute path of an existing file, refused when docker's -v
# would misread it (a colon or a comma).
abs_path() {
	local p="$1" d
	[[ -e "$p" ]] || die "no such file: $p"
	d="$(cd "$(dirname "$p")" && pwd -P)"
	p="$d/$(basename "$p")"
	[[ "$p" != *[:,]* ]] || die "the path '$p' holds a colon or a comma, which docker -v cannot take; move the file"
	printf '%s' "$p"
}

# loose_mode FILE MASK: true when the file has any permission bit of MASK (octal),
# for example 077 for "group or other". (find -perm differs between macOS and Linux.)
loose_mode() {
	python3 -I -c 'import os, sys; sys.exit(0 if os.stat(sys.argv[1]).st_mode & int(sys.argv[2], 8) else 1)' "$1" "$2"
}

uid="$(id -u)" gid="$(id -g)"
idkey="" idfile="" idmount=""

# read_identity: the age private key, from a file (never copied), the standard
# input, or a hidden prompt. The key is held in a shell variable only (builtins
# never put it in an argument), and goes to a 0600 file in the 0700 work folder
# only when a container must read it. It runs BEFORE any docker command, because
# `docker exec -i` would read what is left of the standard input.
read_identity() {
	local line
	if [[ -n "$identity" && "$identity" != - ]]; then
		[[ -f "$identity" ]] || die "no such key file: $identity"
		idfile="$(abs_path "$identity")"
		if loose_mode "$idfile" 044; then
			warn "the key file is readable by other users; make it mode 0600 and keep it off this host when you are done"
		fi
		return 0
	fi
	if [[ "$identity" == - && ! -t 0 ]]; then
		while IFS= read -r line; do
			case "$line" in "" | "#"*) continue ;; esac
			idkey="$line"
			break
		done
	elif [[ -t 0 ]]; then
		# A terminal (also with `--identity -`): never echo the key.
		IFS= read -r -s -p "age private key (AGE-SECRET-KEY-1..., hidden): " idkey || true
		printf '\n' >&2
	else
		die "give the age private key with --identity FILE (or - to read it from the standard input); there is no terminal for a hidden prompt"
	fi
	[[ "$idkey" =~ ^AGE-SECRET-KEY-1[A-Z0-9]+$ ]] || die "that is not an age private key (AGE-SECRET-KEY-1...)"
}
if ((is_remote)); then read_identity; fi

# The scratch work folder of a remote restore.
wd=""
created=0 ok=0
files_out_made=0 files_out_abs=""
# The scratch database is kept only with --keep AND after `restore: OK`.
# Any other exit (a failed pg_restore, a failed check, an interrupt) drops
# it: a partial copy must never be there for the rollback swap to rename.
cleanup() {
	local rc=$?
	if ((created && !(keep && ok))); then
		run psql -X -q -v ON_ERROR_STOP=1 -d postgres -c "DROP DATABASE IF EXISTS \"$name\" WITH (FORCE)" >/dev/null ||
			warn "could not drop the scratch database $name"
	fi
	if [[ -n "$wd" && -d "$wd" ]]; then rm -rf "$wd"; fi
	# A failed attachment restore empties the folder it filled (it was new or empty when it began).
	if ((files_out_made && rc != 0)) && [[ -n "$files_out_abs" && -d "$files_out_abs" ]]; then
		find "$files_out_abs" -mindepth 1 -delete 2>/dev/null || warn "could not empty the attachment folder $files_out_abs"
	fi
	return $rc
}
trap cleanup EXIT

# run ARGS...: a command where the client tools and the dumps are.
if [[ -n "$container" ]]; then
	[[ -z "$remote" ]] || die "--remote NAME reads the destination from the stack's database: on a host with no stack use --remote-config FILE with --container"
	run() { docker exec -i "$container" "$@"; }
else
	load_env
	guard_project
	env_topup lenient
	[[ -n "$(cid backup)" ]] || die "the backup service is not running (deploy/deploy.sh starts it)"
	run() { dc exec -T backup "$@"; }
fi
now() { python3 -c 'import time; print("%.3f" % time.time())'; }

# The live database's name comes from the container (PGDATABASE).
live_db="$(run sh -c 'printf %s "${PGDATABASE:-devops}"' </dev/null)"
[[ "$name" != "$live_db" ]] || die "refusing to restore into the live database '$live_db'"

# ---------------------------------------------------------------- remote copy

# dk ARGS...: docker with a clean environment (no value of the operator's shell
# reaches the docker client).
dk() { docker_clean -- docker "$@"; }

# Every throwaway container: read-only root, no capabilities, no logs (a log
# would keep the plain dump or a key), only what is mounted.
hard=(--rm --pull never --read-only --cap-drop ALL --security-opt no-new-privileges --log-driver none)

# decrypt_run: the encrypted object ($wd/in/object) as a plain dump on stdout.
decrypt_run() {
	local src="${1:-$wd/in/object}"
	if ((!no_host_age)) && command -v age >/dev/null 2>&1; then
		if [[ -n "$idfile" ]]; then
			age -d -i "$idfile" "$src"
		else
			printf '%s\n' "$idkey" | age -d -i - "$src"
		fi
		return
	fi
	dk run "${hard[@]}" -i --user "$uid:$gid" --network none -v "$idmount:/identity:ro" "$image" \
		/bin/server offsite decrypt --identity /identity <"$src"
}

# dump_age TIME: "2026-10-28 03:15:00 UTC (3d 4h 2m old)" for a manifest time
# like 20261028T031500Z.
dump_age() {
	python3 -I -c '
import sys, datetime
t = datetime.datetime.strptime(sys.argv[1], "%Y%m%dT%H%M%SZ").replace(tzinfo=datetime.timezone.utc)
s = int((datetime.datetime.now(datetime.timezone.utc) - t).total_seconds())
age = "in the future: check this host clock" if s < 0 else "%dd %dh %dm old" % (s // 86400, s % 86400 // 3600, s % 3600 // 60)
print("%s (%s)" % (t.strftime("%Y-%m-%d %H:%M:%S UTC"), age))
' "$1"
}

sha_of() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'; else shasum -a 256 "$1" | awk '{print $1}'; fi
}

# prepare_remote: fetch, unpack, verify, check the key. Nothing is restored and
# no database is touched except for read-only lookups. Sets rstem.
rstem="" cfg="" cfg_sig="" vkey="" parent="" plain_bytes=0
# file_sig FILE: which file this is (device, inode, size, times, mode, owner).
file_sig() {
	python3 -I -c 'import os, sys; s = os.stat(sys.argv[1]); print(s.st_dev, s.st_ino, s.st_size, s.st_mtime_ns, s.st_ctime_ns, s.st_mode, s.st_uid)' "$1"
}

# cfg_recheck: right before the config file is mounted, it must still be the file that
# was checked (not replaced, not changed, not a link).
cfg_recheck() {
	[[ -z "$cfg" ]] && return 0
	[[ -f "$cfg" && ! -L "$cfg" && "$(file_sig "$cfg")" == "$cfg_sig" ]] ||
		die "the --remote-config file changed after it was checked; nothing was fetched. Run the restore again"
}

# prepare_common: the image, the install id, the config and key files, the work
# folder and the key mount. Shared by the database and the files restore.
prepare_common() {
	local id=""
	# The config file first, before any docker call: check the file at its resolved
	# path, remember which file that was (cfg_recheck), and mount that same path.
	if [[ -n "$rconfig" ]]; then
		[[ -f "$rconfig" && ! -L "$rconfig" ]] || die "the --remote-config file must be a regular file (not a link)"
		cfg="$(abs_path "$rconfig")"
		[[ -f "$cfg" && ! -L "$cfg" ]] || die "the --remote-config file must be a regular file (not a link)"
		[[ -O "$cfg" ]] || die "the --remote-config file must be owned by you"
		! loose_mode "$cfg" 077 || die "the --remote-config file is readable by group or other: chmod 600 it (it holds the destination's secrets)"
		cfg_sig="$(file_sig "$cfg")"
	fi
	if [[ -z "$image" ]]; then
		if [[ -n "$container" ]]; then
			die "give --image IMAGE (the server image; docs/deploy.md 'Restore from a remote copy' has the docker build)"
		fi
		id="$(cid offsite)"
		[[ -n "$id" ]] || id="$(cid app)"
		[[ -n "$id" ]] || die "no running app or offsite container to take the server image from: give --image IMAGE"
		image="$(docker inspect -f '{{.Config.Image}}' "$id" 2>/dev/null || true)"
		[[ -n "$image" ]] || die "could not read the server image of the running stack: give --image IMAGE"
	fi
	dk image inspect "$image" >/dev/null 2>&1 || die "the image '$image' is not on this host (docs/deploy.md 'Restore from a remote copy' has the docker build)"

	# The install id: given, else the database's.
	if [[ -z "$install_id" ]]; then
		install_id="$(run psql -X -q -t -A -d "$live_db" -c 'SELECT install_id FROM backup_settings' </dev/null 2>/dev/null | tr -d '[:space:]' || true)"
		[[ "$install_id" =~ ^[0-9a-f]{8}$ ]] || die "this database has no install id: give --install-id ID (8 hex characters; it is in the remote file names launchpad-<ID>-...)"
	fi

	if [[ -n "$verify_key" ]]; then vkey="$(abs_path "$verify_key")"; fi

	# The work folder: 0700, ours, removed at exit.
	parent="${work_parent:-${TMPDIR:-/var/tmp}}"
	[[ -d "$parent" ]] || die "the work directory '$parent' does not exist"
	wd="$(umask 077 && mktemp -d "${parent%/}/launchpad-restore.XXXXXX")" || die "cannot make a work folder under $parent"
	chmod 700 "$wd"
	[[ "$wd" != *[:,]* ]] || die "the work folder path holds a colon or a comma; use --work-dir"
	mkdir -m 700 "$wd/in"

	if [[ -n "$idfile" ]]; then
		idmount="$idfile"
	elif ((no_host_age)) || ! command -v age >/dev/null 2>&1; then
		(umask 077 && printf '%s\n' "$idkey" >"$wd/identity")
		idmount="$wd/identity"
	fi
}

# confirm_trust WARNING: with --trust-remote, the warning and a typed confirmation BEFORE
# anything is fetched. The words to type are the install id: on a terminal, typed; with no
# terminal, --yes-trust-remote ID (a bare flag is not enough). Standard input is read only
# from a terminal, so it never takes what `--identity -` is about to read.
confirm_trust() {
	local typed=""
	((trust)) || return 0
	warn "$1"
	if [[ -n "$yes_trust" ]]; then
		[[ "$yes_trust" == "$install_id" ]] || die "--yes-trust-remote does not match the install id $install_id; nothing was fetched"
		return 0
	fi
	if [[ -t 0 ]]; then
		IFS= read -r -p "To go on WITHOUT a signature check, type the install id ($install_id): " typed || true
		[[ "$typed" == "$install_id" ]] || die "not confirmed; nothing was fetched"
		return 0
	fi
	die "--trust-remote needs a confirmation before anything is fetched: type the install id on a terminal, or give --yes-trust-remote $install_id"
}

# free_bytes PATH: free bytes on the file system that holds PATH (or its nearest parent).
free_bytes() {
	local d="$1"
	while [[ ! -d "$d" && "$d" != / && "$d" != . ]]; do d="$(dirname "$d")"; done
	df -Pk "$d" 2>/dev/null | awk 'NR==2 && $4 ~ /^[0-9]+$/ {printf "%.0f", $4 * 1024}'
}

# dev_of PATH: the device number of the file system that holds PATH (or its nearest parent).
dev_of() {
	python3 -I -c '
import os, sys
d = os.path.abspath(sys.argv[1])
while not os.path.isdir(d) and d != "/":
    d = os.path.dirname(d)
print(os.stat(d).st_dev)' "$1"
}

# need_space PATH BYTES WHAT: stop, before anything is written, when PATH's file system has less.
need_space() {
	local have
	have="$(free_bytes "$1")"
	if [[ ! "$have" =~ ^[0-9]+$ ]]; then
		warn "could not measure the free space for $3; make sure there is room for $2 bytes"
		return 0
	fi
	((have >= $2)) || die "not enough free space for $3: it needs $2 bytes and $have are free; nothing was restored"
}

prepare_remote() {
	local want="$dump" vout="" mline pobj ptime sid dbsha objsha dbrec dbrc=0 newest dbsql
	local fetch_cmd=()
	prepare_common
	[[ "$want" == latest || "$want" == launchpad-"$install_id"-* ]] || die "the STEM belongs to another install id than $install_id (a backup made under the old id needs --install-id with that id)"
	confirm_trust "--trust-remote: the signature will NOT be checked. Anyone who can write to the remote can make a dump that runs arbitrary SQL as the bootstrap superuser (for example COPY ... PROGRAM) when it is restored. Use --verify-key with the signing public key you saved"

	say "restore: fetching '$want' from the remote copy (install id $install_id)"
	if [[ -n "$remote" ]]; then
		fetch_cmd=(dc exec -T offsite /bin/server offsite fetch --destination "$remote" --stem "$want")
	else
		fetch_cmd=(dk run "${hard[@]}" --user "$uid:$gid" --network "${fetch_net:-bridge}" -v "$cfg:/config.json:ro" "$image"
			/bin/server offsite fetch --config /config.json --install-id "$install_id" --stem "$want")
		if [[ -n "$allow_cidr" ]]; then fetch_cmd+=(--allow-cidr "$allow_cidr"); fi
	fi
	cfg_recheck
	# A failed fetch must fail the restore, whatever unpack made of the bytes.
	# pipefail makes the pipeline fail when EITHER side does; keep this line.
	set -o pipefail
	if ! "${fetch_cmd[@]}" </dev/null |
		dk run "${hard[@]}" -i --user "$uid:$gid" --network none -v "$wd/in:/out" "$image" \
			/bin/server offsite unpack --out /out; then
		die "the fetch or the unpack FAILED (the reason is above); nothing was verified or restored"
	fi
	[[ -f "$wd/in/object" && -f "$wd/in/manifest.json" ]] || die "the fetch did not produce a backup; nothing was restored"

	# The manifest's own object name, read before it is trusted, only to choose the
	# stem for `latest`. The signature check below decides whether it is believed.
	mline="$(python3 -I -c '
import sys, json, base64
e = json.load(open(sys.argv[1]))
p = json.loads(base64.b64decode(e["payload"]))
print(p["object"]); print(p["dumpTime"])
' "$wd/in/manifest.json" 2>/dev/null)" || die "the manifest is not readable; nothing was restored"
	pobj="$(printf '%s\n' "$mline" | sed -n 1p)"
	ptime="$(printf '%s\n' "$mline" | sed -n 2p)"
	[[ "$pobj" =~ ^launchpad-[0-9a-f]{8}-[0-9]{8}T[0-9]{6}Z-[0-9a-f]{4}\.dump\.age$ && "$ptime" =~ ^[0-9]{8}T[0-9]{6}Z$ ]] ||
		die "the manifest does not name a backup object; nothing was restored"
	rstem="${pobj%.dump.age}"
	if [[ "$want" != latest && "$rstem" != "$want" ]]; then
		die "the manifest names another backup than the one asked for; nothing was restored"
	fi

	if ((trust)); then
		say "restore: dump time $(dump_age "$ptime"), UNVERIFIED (read from the manifest the remote sent)"
	else
		vout="$(dk run "${hard[@]}" --user "$uid:$gid" --network none -v "$wd/in:/in:ro" -v "$vkey:/key.pub:ro" "$image" \
			/bin/server offsite verify --public-key /key.pub --manifest /in/manifest.json --object /in/object \
			--install-id "$install_id" --stem "$rstem" </dev/null)" ||
			die "the signature or hash check FAILED (the reason is above): this copy is not trusted; nothing was decrypted or restored"
		# The authenticated values, from the verify output.
		sid="$(printf '%s\n' "$vout" | sed -n 's/^object: //p')"
		ptime="$(printf '%s\n' "$vout" | sed -n 's/^dumpTime: //p')"
		[[ "$sid" == "$pobj" && "$ptime" =~ ^[0-9]{8}T[0-9]{6}Z$ ]] || die "the verify output was not understood; nothing was restored"
		say "restore: signature and sha256 OK (install id $install_id, $rstem)"
		say "restore: dump time $(dump_age "$ptime"), from the SIGNED manifest"
		say "restore: made by Launchpad $(printf '%s\n' "$vout" | sed -n 's/^launchpadVersion: //p' | tr -cd 'A-Za-z0-9._+-'), database schema version $(printf '%s\n' "$vout" | sed -n 's/^schemaVersion: //p' | tr -cd '0-9')"
	fi
	if [[ "$want" == latest ]]; then
		say "restore: 'latest' is the newest COMPLETE pair by the remote file names, which nobody signed. A signature does not prove freshness: whoever can write to the remote can put back an older pair. Check that this time is the one you expect"
	fi

	# The database's own record of the object, when this database has one, and the newest
	# succeeded backup it knows. An answer is told apart from a failed question: a database
	# that is down or has no backup_runs table is not the same as "no record".
	objsha="$(sha_of "$wd/in/object")"
	dbsql="$(
		cat <<'SQL'
SELECT coalesce((SELECT sha256 FROM backup_runs WHERE status = 'succeeded' AND remote_name = :'obj' ORDER BY queued_at DESC LIMIT 1), '')
  || '|' ||
  coalesce((SELECT max(substring(remote_name from '([0-9]{8}T[0-9]{6}Z)-[0-9a-f]{4}[.]dump[.]age$')) FROM backup_runs
            WHERE status = 'succeeded' AND remote_name ~ '^launchpad-[0-9a-f]{8}-[0-9]{8}T[0-9]{6}Z-[0-9a-f]{4}[.]dump[.]age$'), '');
SQL
	)"
	dbrec="$(printf '%s\n' "$dbsql" | run psql -X -q -t -A -v ON_ERROR_STOP=1 -v "obj=$pobj" -d "$live_db" -f - 2>/dev/null)" || dbrc=$?
	dbrec="$(printf '%s' "$dbrec" | tr -d '[:space:]')"
	dbsha="${dbrec%%|*}"
	newest="${dbrec#*|}"
	if ((dbrc != 0)) || [[ "$dbrec" != *"|"* ]] || [[ -n "$dbsha" && ! "$dbsha" =~ ^[0-9a-f]{64}$ ]] || [[ -n "$newest" && ! "$newest" =~ ^[0-9]{8}T[0-9]{6}Z$ ]]; then
		dbsha="" newest=""
		warn "the database could not be asked about this backup (it did not answer, or it has no backup_runs table): the object is NOT checked against a database record, and nothing says whether a newer backup exists. This is not the same as 'no record'"
	else
		if [[ -n "$newest" && "$newest" > "$ptime" ]]; then
			warn "ROLLBACK: the database records a succeeded backup NEWER than this copy (newest: $(dump_age "$newest"); this copy: $(dump_age "$ptime")). Restoring it loses everything written since. Go on only if that is what you want"
		fi
		if [[ -z "$dbsha" ]]; then
			say "restore: no record of this object in the database (not checked against it)"
		elif [[ "$dbsha" == "$objsha" ]]; then
			say "restore: the object's sha256 equals the database record"
		else
			die "the object's sha256 differs from the database record (backup_runs): do not trust this copy; nothing was decrypted or restored"
		fi
	fi

	# A whole decrypt pass BEFORE any database is made: a wrong key, a changed
	# byte or a cut file fails here, and nothing is left to clean up.
	say "restore: checking the private key and the whole object (nothing is written)"
	plain_bytes="$(decrypt_run | wc -c | tr -d ' ')" || die "the private key does not decrypt this backup, or the object is damaged; nothing was restored"
	say "restore: the private key decrypts the object ($plain_bytes bytes of plain dump)"
}

# check_db_space: before the scratch database is made, the database's volume must hold at
# least the plain dump (a floor: the database is usually larger than its compressed dump).
# Only measurable where PGDATA is visible (the postgres container itself).
check_db_space() {
	local kb
	kb="$(run sh -c 'd="${PGDATA:-/var/lib/postgresql/data}"; [ -d "$d" ] && df -Pk "$d" | awk "NR==2{print \$4}"' </dev/null 2>/dev/null || true)"
	kb="$(printf '%s' "$kb" | tr -d '[:space:]')"
	if [[ ! "$kb" =~ ^[0-9]+$ ]]; then
		say "restore: the free space of the database volume cannot be measured from here; the plain dump is $plain_bytes bytes and the database needs more. Check the volume yourself"
		return 0
	fi
	if ((kb * 1024 < plain_bytes)); then
		die "not enough free space in the database volume: the plain dump is $plain_bytes bytes and $((kb * 1024)) are free (the database needs more than the dump); nothing was restored"
	fi
	say "restore: the database volume has room for the plain dump ($plain_bytes bytes; $((kb * 1024)) free)"
}

# ---------------------------------------------------------------- attachments

# fetch_unpack OUTDIR FETCH-ARGS...: one frame from the remote into OUTDIR, in
# throwaway containers. A failed fetch must fail this function, whatever unpack
# made of the bytes.
fetch_unpack() {
	local o="$1" fc=()
	shift
	if [[ -n "$remote" ]]; then
		fc=(dc exec -T offsite /bin/server offsite fetch --destination "$remote" "$@")
	else
		fc=(dk run "${hard[@]}" --user "$uid:$gid" --network "${fetch_net:-bridge}" -v "$cfg:/config.json:ro" "$image"
			/bin/server offsite fetch --config /config.json --install-id "$install_id" "$@")
		if [[ -n "$allow_cidr" ]]; then fc+=(--allow-cidr "$allow_cidr"); fi
	fi
	cfg_recheck
	# pipefail makes the pipeline fail when EITHER side does; keep this line.
	set -o pipefail
	"${fc[@]}" </dev/null |
		dk run "${hard[@]}" -i --user "$uid:$gid" --network none -v "$o:/out" "$image" \
			/bin/server offsite unpack --out /out
}

# restore_files: the attachments of a remote copy as plain files. The order is the
# one of the database restore: signature, sha256 of the list, a decrypt pass that
# writes nothing, and only then the first byte under --files-out. Each object is
# checked against the signed list (sha256 of the stored bytes, then of the plain
# bytes) in the work folder before it is moved into place. Any failure removes what
# this run made under --files-out. No key (file name) is printed or logged: only counts.
restore_files() {
	local want="$dump" vout="" mline plist ptime stem n total=0 bytes=0 i=0 line remote_name psha psize csha csize b64key rel dest
	local out_abs="" need_out need_wd
	prepare_common
	[[ "$want" == latest || "$want" == launchpad-files-"$install_id"-* ]] || die "the STEM belongs to another install id than $install_id (a backup made under the old id needs --install-id with that id)"
	confirm_trust "--trust-remote: the signature will NOT be checked. Anyone who can write to the remote can put wrong files into the attachment list. Use --verify-key with the signing public key you saved"
	if [[ -n "$files_out" ]]; then
		if [[ -e "$files_out" ]]; then
			[[ -d "$files_out" && ! -L "$files_out" && -z "$(ls -A "$files_out")" ]] || die "--files-out must be a folder that does not exist or is empty: this script never overwrites or deletes your files"
		fi
		out_abs="$(python3 -I -c 'import os,sys; print(os.path.abspath(sys.argv[1]))' "$files_out")"
	fi

	say "restore: fetching the attachment list '$want' from the remote copy (install id $install_id)"
	fetch_unpack "$wd/in" --files --stem "$want" ||
		die "the fetch or the unpack FAILED (the reason is above); nothing was verified or restored"
	[[ -f "$wd/in/object" && -f "$wd/in/manifest.json" ]] || die "the fetch did not produce an attachment list; nothing was restored"
	mline="$(python3 -I -c '
import sys, json, base64
e = json.load(open(sys.argv[1]))
p = json.loads(base64.b64decode(e["payload"]))
print(p["list"]); print(p["time"])
' "$wd/in/manifest.json" 2>/dev/null)" || die "the manifest is not readable; nothing was restored"
	plist="$(printf '%s\n' "$mline" | sed -n 1p)"
	ptime="$(printf '%s\n' "$mline" | sed -n 2p)"
	[[ "$plist" =~ ^launchpad-files-[0-9a-f]{8}-list-[0-9]{8}T[0-9]{6}Z-[0-9a-f]{4}\.list\.age$ && "$ptime" =~ ^[0-9]{8}T[0-9]{6}Z$ ]] ||
		die "the manifest does not name an attachment list; nothing was restored"
	stem="${plist%.list.age}"
	if [[ "$want" != latest && "$stem" != "$want" ]]; then
		die "the manifest names another list than the one asked for; nothing was restored"
	fi
	n=""
	if ((trust)); then
		say "restore: list time $(dump_age "$ptime"), UNVERIFIED (read from the manifest the remote sent)"
	else
		vout="$(dk run "${hard[@]}" --user "$uid:$gid" --network none -v "$wd/in:/in:ro" -v "$vkey:/key.pub:ro" "$image" \
			/bin/server offsite verify --files --public-key /key.pub --manifest /in/manifest.json --object /in/object \
			--install-id "$install_id" --stem "$stem" </dev/null)" ||
			die "the signature or hash check FAILED (the reason is above): this list is not trusted; nothing was decrypted or restored"
		ptime="$(printf '%s\n' "$vout" | sed -n 's/^time: //p')"
		n="$(printf '%s\n' "$vout" | sed -n 's/^objects: //p' | tr -cd '0-9')"
		[[ "$ptime" =~ ^[0-9]{8}T[0-9]{6}Z$ && -n "$n" ]] || die "the verify output was not understood; nothing was restored"
		say "restore: signature and sha256 of the list OK (install id $install_id, $stem)"
		say "restore: list time $(dump_age "$ptime"), from the SIGNED manifest; it names $n objects"
	fi
	if [[ "$want" == latest ]]; then
		say "restore: 'latest' is the newest COMPLETE list by the remote file names, which nobody signed. A signature does not prove freshness: check that this time is the one you expect"
	fi
	say "restore: no database record exists for an attachment list (the signature and the list's sha256 are the check)"
	say "restore: checking the private key and the whole list (nothing is written)"
	decrypt_run >/dev/null || die "the private key does not decrypt this list, or the list is damaged; nothing was restored"
	say "restore: the private key decrypts the list"

	# The plain list stays in the 0700 work folder. Every entry is checked before use.
	(umask 077 && decrypt_run >"$wd/list.jsonl") || die "the list could not be decrypted; nothing was restored"
	if ! python3 -I - "$wd/list.jsonl" "$install_id" "$files_key" "$n" >"$wd/entries.tsv" <<'PY'
import sys, json, base64, re, hashlib
path, inst, only, want_n = sys.argv[1:5]
obj_re = re.compile(r"^launchpad-files-" + inst + r"-obj-[0-9a-f]{32}\.age$")
hex64 = re.compile(r"^[0-9a-f]{64}$")
seen, rows = set(), []
for line in open(path, "rb"):
    e = json.loads(line)
    k = e["key"]
    if not isinstance(k, str) or k in seen or not k.startswith("org/") or k.endswith("/") or "\\" in k:
        sys.exit(3)
    if any(ord(c) < 32 or ord(c) == 127 for c in k):
        sys.exit(3)
    comps = k.split("/")
    if any(c in ("", ".", "..") or len(c.encode()) > 255 for c in comps):
        sys.exit(3)
    if e["remote"] != "launchpad-files-" + inst + "-obj-" + hashlib.sha256(k.encode()).hexdigest()[:32] + ".age":
        sys.exit(3)
    if not (obj_re.match(e["remote"]) and hex64.match(e["sha256"]) and hex64.match(e["cipherSha256"])):
        sys.exit(3)
    if not (isinstance(e["size"], int) and e["size"] >= 0 and isinstance(e["cipherSize"], int) and e["cipherSize"] > 0):
        sys.exit(3)
    seen.add(k)
    rows.append((e["remote"], e["sha256"], e["size"], e["cipherSha256"], e["cipherSize"], base64.b64encode(k.encode()).decode(), k))
if want_n and len(rows) != int(want_n):
    sys.exit(4)
if only:
    rows = [r for r in rows if r[6] == only]
    if not rows:
        sys.exit(5)
for r in rows:
    print("%s\t%s\t%d\t%s\t%d\t%s" % r[:6])
PY
	then
		die "the list holds an entry that is not safe to restore, or does not match its manifest; nothing was restored"
	fi
	total="$(wc -l <"$wd/entries.tsv" | tr -d ' ')"
	if ((verify_only)); then
		if ((trust)); then
			say "restore: verify-only: NOT verified (--trust-remote); $stem decrypts with this key; it names $total object(s); nothing was restored"
		else
			say "restore: verify-only OK - $stem is signed, matches its manifest and decrypts; it names $total object(s); nothing was restored"
		fi
		return 0
	fi
	if ((total == 0)); then
		say "restore: the list is empty; nothing to restore"
		return 0
	fi

	# Room first: the plain files under --files-out, and one object (stored and plain) in the work folder.
	read -r need_out need_wd < <(awk -F'\t' '{s += $3; if ($3 + $5 > m) m = $3 + $5} END {printf "%.0f %.0f\n", s, m}' "$wd/entries.tsv")
	if [[ "$(dev_of "$out_abs")" == "$(dev_of "$wd")" ]]; then
		# One file system holds both: the files and the staged object are there together.
		need_space "$out_abs" "$((need_out + need_wd))" "the restored files under the folder you gave and one object in the work folder (the same file system)"
	else
		need_space "$out_abs" "$need_out" "the restored files under the folder you gave"
		need_space "$wd" "$need_wd" "one object in the work folder"
	fi

	# From here on, bytes are written under --files-out. Look again right before the first write:
	# the folder may have been filled while the fetch ran.
	if [[ -e "$out_abs" ]]; then
		[[ -d "$out_abs" && ! -L "$out_abs" && -z "$(ls -A "$out_abs")" ]] || die "--files-out is no longer a folder that is empty: this script never overwrites or deletes your files; nothing was restored"
	fi
	if [[ ! -d "$out_abs" ]]; then
		(umask 077 && mkdir -p "$out_abs") || die "cannot make $out_abs"
	fi
	files_out_made=1
	files_out_abs="$out_abs"
	mkdir -m 700 "$wd/obj"
	say "restore: restoring $total object(s) into the folder you gave (each is checked against the signed list before it is moved there)"
	while IFS=$'\t' read -r remote_name psha psize csha csize b64key; do
		i=$((i + 1))
		rm -rf "${wd:?}/obj" && mkdir -m 700 "$wd/obj"
		fetch_unpack "$wd/obj" --file-object "$remote_name" ||
			die "object $i of $total: the fetch FAILED (the reason is above); the folder was emptied again"
		[[ -f "$wd/obj/object" ]] || die "object $i of $total: the fetch produced nothing; the folder was emptied again"
		[[ "$(wc -c <"$wd/obj/object" | tr -d ' ')" == "$csize" && "$(sha_of "$wd/obj/object")" == "$csha" ]] ||
			die "object $i of $total: the stored copy does not match the signed list (damaged or swapped); the folder was emptied again"
		(umask 077 && decrypt_run "$wd/obj/object" >"$wd/obj/plain") ||
			die "object $i of $total: it does not decrypt; the folder was emptied again"
		[[ "$(wc -c <"$wd/obj/plain" | tr -d ' ')" == "$psize" && "$(sha_of "$wd/obj/plain")" == "$psha" ]] ||
			die "object $i of $total: the restored bytes do not match the sha256 in the signed list; the folder was emptied again"
		rel="$(printf '%s' "$b64key" | python3 -I -c 'import sys,base64; sys.stdout.write(base64.b64decode(sys.stdin.read()).decode())')"
		dest="$out_abs/$rel"
		[[ ! -e "$dest" ]] || die "object $i of $total: the target file exists; the folder was emptied again"
		(umask 077 && mkdir -p "$(dirname "$dest")") || die "object $i of $total: cannot make its folder; the folder was emptied again"
		mv "$wd/obj/plain" "$dest" || die "object $i of $total: cannot move it into place; the folder was emptied again"
		bytes=$((bytes + psize))
		if ((i % 100 == 0)); then say "restore: $i of $total objects restored"; fi
	done <"$wd/entries.tsv"
	say "restore: files OK - $total object(s), $bytes bytes, every sha256 equals the signed list; written under the folder you gave (mode 0600)"
	say "restore: next: upload them to the new bucket (docs/deploy.md 'Restore the attachments'). The folder holds every attachment in plain text: protect it and delete it when done"
	if [[ -n "$rconfig" ]]; then
		say "restore: delete the --remote-config file now: it holds the destination's secrets"
	fi
}

if ((files)); then
	restore_files
	ok=1
	exit 0
fi

if ((is_remote)); then
	prepare_remote
	if ((verify_only)); then
		if ((trust)); then
			say "restore: verify-only: NOT verified (--trust-remote); $rstem decrypts with this key; nothing was restored"
		else
			say "restore: verify-only OK - $rstem is signed, matches its manifest and decrypts; it holds $plain_bytes bytes of plain dump; nothing was restored"
		fi
		exit 0
	fi
	check_db_space
	say "restore: $rstem (remote copy) -> database $name"
	source_arg=()
	feed() { decrypt_run; }
elif [[ "$dump" == latest ]]; then
	dump_path="$(run sh -c 'ls -1t "${BACKUP_DIR:-/backups}"/devops-*.dump 2>/dev/null | head -n 1')"
	[[ -n "$dump_path" ]] || die "no dump in the backups volume"
	say "restore: $dump_path (in the backups volume) -> database $name"
	source_arg=("$dump_path")
	feed() { cat /dev/null; }
else
	[[ -f "$dump" ]] || die "no such file: $dump"
	say "restore: $dump (from this host) -> database $name"
	source_arg=()
	feed() { cat "$dump"; }
fi

counts_sql="SELECT 'rows ' || table_name || ' ' ||
  (xpath('/row/c/text()', query_to_xml(format('SELECT count(*) AS c FROM public.%I', table_name), false, true, '')))[1]::text
FROM information_schema.tables
WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
ORDER BY table_name;"

run psql -X -q -v ON_ERROR_STOP=1 -d postgres -c "CREATE DATABASE \"$name\"" >/dev/null
created=1

t0="$(now)"
# --no-owner / --no-privileges: the scratch copy belongs to the restoring
# role. --exit-on-error: any error fails the restore (no partial "success").
# pipefail (set at the top): a failed decrypt fails the restore too.
feed | run pg_restore --no-owner --no-privileges --exit-on-error -d "$name" ${source_arg[@]+"${source_arg[@]}"}
t1="$(now)"

restored="$(run psql -X -q -t -A -v ON_ERROR_STOP=1 -d "$name" -c "$counts_sql")"
printf '%s\n' "$restored"
tables="$(printf '%s\n' "$restored" | grep -c '^rows ' || true)"
total="$(printf '%s\n' "$restored" | awk '/^rows /{s+=$3} END{print s+0}')"
((tables > 0)) || die "the restored database has no tables"
version="$(run psql -X -q -t -A -d "$name" -c 'SELECT version FROM schema_migrations' 2>/dev/null || echo '?')"

if ((compare)); then
	live="$(run psql -X -q -t -A -v ON_ERROR_STOP=1 -d "$live_db" -c "$counts_sql")"
	# backup.sh records each dump in backup_dumps AFTER pg_dump finishes, and the
	# worker and the app write the other backup_* tables at any time, so those counts
	# never equal the live ones. Every other table is compared.
	skip_re='^rows backup_(dumps|runs|requests|change_log) '
	live_cmp="$(printf '%s\n' "$live" | grep -Ev "$skip_re" || true)"
	restored_cmp="$(printf '%s\n' "$restored" | grep -Ev "$skip_re" || true)"
	if [[ "$live_cmp" != "$restored_cmp" ]]; then
		say "restore: counts differ from the live database ($live_db):"
		diff <(printf '%s\n' "$live_cmp") <(printf '%s\n' "$restored_cmp") || true
		die "restore check FAILED"
	fi
	say "restore: every table's count equals the live database ($live_db) (the backup_dumps, backup_runs, backup_requests and backup_change_log tables are not compared)"
fi

secs="$(python3 -c "print('%.2f' % ($t1 - $t0))")"
say "restore: OK - $tables tables, $total rows, migration version $version, pg_restore took ${secs}s"
ok=1
if ((keep)); then
	say "restore: kept database $name; drop it with: DROP DATABASE \"$name\";"
fi
if ((is_remote)); then
	say "restore: next: swap the copy in (docs/deploy.md), then run deploy/deploy.sh (it gives the offsite role its grants again) and deploy/deploy.sh --backup-resume before backups start: a restored copy has the OLD install id"
	if [[ -n "$rconfig" ]]; then
		say "restore: delete the --remote-config file now: it holds the destination's secrets"
	fi
fi
