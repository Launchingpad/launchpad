#!/usr/bin/env bash
# Write .env for the root docker-compose.yml: it copies .env.example, gives every
# secret a random value and sets your two host names and the release version.
#
#   scripts/gen-env.sh                      (run it in the directory that holds
#                                            docker-compose.yml and .env.example)
#   WEB_HOST=lp.example.com FILES_HOST=lpf.example.com LAUNCHPAD_VERSION=1.2.3 \
#     scripts/gen-env.sh --non-interactive
#
# Options: --example FILE (default .env.example), --out FILE (default .env),
# --non-interactive (never ask; a missing value is an error).
#
# Rules (docs/internal/decisions-T45-docker-distribution.md):
#   - It refuses to overwrite: if the output file exists it stops and changes
#     nothing. The file is made with mode 0600 from the first byte.
#   - WEB_HOST and FILES_HOST have no default. Read them from the environment
#     or ask on a terminal. LAUNCHPAD_VERSION is the same (no `latest`).
#   - It prints names only, never a secret. The secrets reach the file and
#     nothing else: not stdout, not stderr, not an argument list.
#   - Randomness: openssl rand, else /dev/urandom.
set -euo pipefail
umask 077

example=".env.example"
out=".env"
interactive=auto
while (($#)); do
	case "$1" in
	--example)
		example="${2:?--example needs a file}"
		shift 2
		;;
	--out)
		out="${2:?--out needs a file}"
		shift 2
		;;
	--non-interactive)
		interactive=no
		shift
		;;
	-h | --help)
		sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		echo "gen-env: unknown option: $1" >&2
		exit 2
		;;
	esac
done

die() {
	echo "gen-env: $*" >&2
	exit 1
}

[[ -f "$example" ]] || die "$example not found (run this in the directory that holds .env.example, or pass --example)"
[[ ! -e "$out" && ! -L "$out" ]] || die "$out exists; nothing was changed. Delete it yourself if you want a new one (a new secret makes old data unreadable)"

# Keys that get a random value. The value form decides the shape.
hex32() { # 64 hex characters
	if command -v openssl >/dev/null 2>&1; then
		openssl rand -hex 32
	else
		od -An -N32 -tx1 /dev/urandom | tr -d ' \n'
	fi
}
hex6() { hex32 | cut -c1-12; }

can_ask=0
if [[ "$interactive" == auto && -t 0 && -t 2 ]]; then can_ask=1; fi

# valid_host: a DNS name (letters, digits, dots, hyphens), no scheme, no path,
# no port. localhost is allowed.
valid_host() {
	[[ "$1" =~ ^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$ ]] && [[ ${#1} -le 253 ]]
}
valid_version() { [[ "$1" =~ ^[0-9][A-Za-z0-9._+-]*$ ]]; }

# ask NAME VALIDATOR PROMPT: the value of $NAME from the environment, else
# asked on a terminal. No default.
ask() {
	local name="$1" check="$2" prompt="$3" val
	val="${!name:-}"
	if [[ -z "$val" ]]; then
		((can_ask)) || die "$name is not set (export it, or run on a terminal)"
		read -r -p "$prompt: " val </dev/tty >&2
	fi
	"$check" "$val" || die "$name has a value that is not allowed (a plain value is expected; nothing was written)"
	printf '%s' "$val"
}

web_host="$(ask WEB_HOST valid_host "Web host name (for example launchpad.example.com)")"
files_host="$(ask FILES_HOST valid_host "Files host name (for example launchpad-files.example.com)")"
[[ "$web_host" != "$files_host" ]] || die "WEB_HOST and FILES_HOST must be two different names"
version="$(ask LAUNCHPAD_VERSION valid_version "Release version, without the v (for example 1.2.3)")"

# Build the file in a temp file beside the output (same mode, same disk), then
# link it into place: ln fails if the target exists, so a race cannot overwrite.
dir="$(dirname -- "$out")"
tmp="$(mktemp "$dir/.gen-env.XXXXXX")"
trap 'rm -f "$tmp"' EXIT
chmod 0600 "$tmp"

set_keys=""
while IFS= read -r line || [[ -n "$line" ]]; do
	if [[ "$line" =~ ^([A-Z][A-Z0-9_]*)=\'\'$ ]]; then
		key="${BASH_REMATCH[1]}"
		val=""
		case "$key" in
		WEB_HOST) val="$web_host" ;;
		FILES_HOST) val="$files_host" ;;
		LAUNCHPAD_VERSION) val="$version" ;;
		SILO_ROOT_USER) val="root-$(hex6)" ;;
		S3_ACCESS_KEY) val="app-$(hex6)" ;;
		POSTGRES_PASSWORD | AUTH_OTP_PEPPER | SETTINGS_ENCRYPTION_KEY | BACKUP_ENCRYPTION_KEY | SILO_ROOT_PASSWORD | S3_SECRET_KEY)
			val="$(hex32)"
			;;
		esac
		if [[ -n "$val" ]]; then
			printf "%s='%s'\n" "$key" "$val" >>"$tmp"
			set_keys="$set_keys $key"
			continue
		fi
	fi
	printf '%s\n' "$line" >>"$tmp"
done <"$example"

# Every required key must have been filled; a key the template has but this
# script does not know stays empty and compose names it.
for k in WEB_HOST FILES_HOST LAUNCHPAD_VERSION POSTGRES_PASSWORD AUTH_OTP_PEPPER SETTINGS_ENCRYPTION_KEY BACKUP_ENCRYPTION_KEY SILO_ROOT_USER SILO_ROOT_PASSWORD S3_ACCESS_KEY S3_SECRET_KEY; do
	case " $set_keys " in
	*" $k "*) ;;
	*) die "$example has no empty $k line; nothing was written" ;;
	esac
done

ln "$tmp" "$out" 2>/dev/null || die "$out appeared while this ran; nothing was changed"
echo "gen-env: wrote $out (mode 0600)."
echo "  set by you:    WEB_HOST FILES_HOST LAUNCHPAD_VERSION"
echo "  random values: POSTGRES_PASSWORD AUTH_OTP_PEPPER SETTINGS_ENCRYPTION_KEY BACKUP_ENCRYPTION_KEY SILO_ROOT_USER SILO_ROOT_PASSWORD S3_ACCESS_KEY S3_SECRET_KEY"
echo "  Keep a copy of $out off this host. A lost SETTINGS_ENCRYPTION_KEY or BACKUP_ENCRYPTION_KEY cannot be rebuilt."
echo "  Next: docker compose up -d"
