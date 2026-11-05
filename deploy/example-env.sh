#!/usr/bin/env bash
# Write a FILLED copy of the root .env.example, with fake values, for the
# checks and for CI: `docker compose config` needs every required key set.
#
#   deploy/example-env.sh OUT_FILE [KEY=value ...]
#
# Each empty KEY='' becomes KEY='fake-<key>-value' (distinct per key, so a test
# can look for one in a place it must not be). The three host/version keys get
# fixed example.com values. Extra KEY=value arguments replace a line. The fake
# values are NOT secrets and the file is not usable for a real stack.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
out="${1:?usage: example-env.sh OUT_FILE [KEY=value ...]}"
shift
: >"$out"
while IFS= read -r line || [[ -n "$line" ]]; do
	if [[ "$line" =~ ^([A-Z][A-Z0-9_]*)=\'\'$ ]]; then
		k="${BASH_REMATCH[1]}"
		case "$k" in
		WEB_HOST) line="WEB_HOST='launchpad.example.com'" ;;
		FILES_HOST) line="FILES_HOST='launchpad-files.example.com'" ;;
		LAUNCHPAD_VERSION) line="LAUNCHPAD_VERSION='1.2.3'" ;;
		POSTMARK_* | TRUSTED_PROXY_CIDRS | GIT_ALLOW_*) ;;
		*) line="$k='fake-$(printf '%s' "$k" | tr 'A-Z_' 'a-z-')-value'" ;;
		esac
	fi
	for kv in "$@"; do
		if [[ "${line%%=*}" == "${kv%%=*}" ]]; then line="${kv%%=*}='${kv#*=}'"; fi
	done
	printf '%s\n' "$line" >>"$out"
done <"$here/../.env.example"
for kv in "$@"; do
	grep -q "^${kv%%=*}=" "$out" || printf "%s='%s'\n" "${kv%%=*}" "${kv#*=}" >>"$out"
done
