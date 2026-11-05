#!/usr/bin/env bash
# Tests of scripts/gen-env.sh (the .env maker of the root docker-compose.yml).
# It needs no Docker and starts nothing.
#
#   deploy/test-gen-env.sh            (make deploy-test in server/)
#
# 1. The output has mode 0600 and every key of .env.example, in the same
#    order, with no empty required value; the host names and version are the
#    ones given; the other lines (comments, defaults) are copied unchanged.
# 2. Secrets: 64 hex characters for the four keys, different from each other,
#    and a second run makes different values. No secret on stdout or stderr.
# 3. It refuses to overwrite: an existing .env (also a symlink) stays
#    byte-identical, exit non-zero, and no temp file is left.
# 4. No terminal and a missing WEB_HOST, FILES_HOST or LAUNCHPAD_VERSION:
#    refused, nothing written. Bad values (a scheme, a path, equal hosts, a
#    "latest"-like word) are refused and echoed nowhere.
# 5. Without openssl on PATH, the /dev/urandom fallback works.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
repo="$(cd "$here/.." && pwd)"
gen="$repo/scripts/gen-env.sh"
scratch="$(mktemp -d "${TMPDIR:-/tmp}/launchpad-test-gen-env.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT
fail() {
	echo "test-gen-env: FAIL: $*" >&2
	exit 1
}
ok() { echo "ok: $*"; }

# GNU stat first: on Linux `stat -f` means filesystem status and exits 0.
mode_of() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }

W=lp.example.com F=lpf.example.com V=1.2.3
run() { # run DIR ARGS... : the maker in DIR with the three values
	local d="$1"
	shift
	(cd "$d" && WEB_HOST="$W" FILES_HOST="$F" LAUNCHPAD_VERSION="$V" "$gen" --non-interactive "$@")
}
fresh() { # fresh NAME : a directory with .env.example
	mkdir -p "$scratch/$1"
	cp "$repo/.env.example" "$scratch/$1/.env.example"
	echo "$scratch/$1"
}
val() { sed -n "s/^$2='\(.*\)'\$/\1/p" "$1/.env"; }

# 1 and 2
d="$(fresh a)"
run "$d" >"$scratch/a.out" 2>"$scratch/a.err" || fail "first run exited non-zero: $(cat "$scratch/a.err")"
[[ "$(mode_of "$d/.env")" == 600 ]] || fail "mode is $(mode_of "$d/.env"), want 600"
grep -o "^[A-Z0-9_]*=" "$d/.env.example" >"$scratch/k1"
grep -o "^[A-Z0-9_]*=" "$d/.env" >"$scratch/k2"
cmp -s "$scratch/k1" "$scratch/k2" || fail "the keys differ from .env.example"
[[ "$(val "$d" WEB_HOST)" == "$W" && "$(val "$d" FILES_HOST)" == "$F" && "$(val "$d" LAUNCHPAD_VERSION)" == "$V" ]] || fail "host names or version not set"
for k in POSTGRES_PASSWORD AUTH_OTP_PEPPER SETTINGS_ENCRYPTION_KEY BACKUP_ENCRYPTION_KEY SILO_ROOT_PASSWORD S3_SECRET_KEY; do
	v="$(val "$d" "$k")"
	[[ "$v" =~ ^[0-9a-f]{64}$ ]] || fail "$k is not 64 hex characters"
	[[ "$v" != "$(printf '0%.0s' $(seq 64))" ]] || fail "$k is all zeros"
done
[[ "$(val "$d" SILO_ROOT_USER)" =~ ^root-[0-9a-f]{12}$ && "$(val "$d" S3_ACCESS_KEY)" =~ ^app-[0-9a-f]{12}$ ]] || fail "user names have the wrong shape"
dups="$(for k in POSTGRES_PASSWORD AUTH_OTP_PEPPER SETTINGS_ENCRYPTION_KEY BACKUP_ENCRYPTION_KEY SILO_ROOT_PASSWORD S3_SECRET_KEY; do val "$d" "$k"; done | sort | uniq -d | wc -l | tr -d ' ')"
[[ "$dups" == 0 ]] || fail "two secrets are equal"
# Lines that the maker must not change: every comment and default is still there.
while IFS= read -r l; do
	grep -q -x -F -e "$l" "$d/.env" || fail "a line of .env.example was changed: $l"
done < <(grep -v "^[A-Z0-9_]*=''$" "$d/.env.example")
# The optional empties stay empty.
for k in POSTMARK_SERVER_TOKEN POSTMARK_FROM TRUSTED_PROXY_CIDRS GIT_ALLOW_PRIVATE_PROVIDERS GIT_ALLOW_PRIVATE_CIDRS; do
	grep -q "^$k=''$" "$d/.env" || fail "$k is not left empty"
done
grep -q "^LAUNCHPAD_BIND_ADDR='127.0.0.1'$" "$d/.env" || fail "the default bind address was not copied"
ok "mode 0600, same keys, values set, optional keys empty"
# no secret printed
while IFS= read -r v; do
	if grep -q -F -e "$v" "$scratch/a.out" "$scratch/a.err"; then fail "a secret value was printed"; fi
done < <(grep -E "^(POSTGRES_PASSWORD|AUTH_OTP_PEPPER|SETTINGS_ENCRYPTION_KEY|BACKUP_ENCRYPTION_KEY|SILO_ROOT_PASSWORD|S3_SECRET_KEY|SILO_ROOT_USER|S3_ACCESS_KEY)=" "$d/.env" | sed "s/^[A-Z0-9_]*='\(.*\)'$/\1/")
ok "no secret on stdout or stderr"
d2="$(fresh b)"
run "$d2" >/dev/null 2>&1
[[ "$(val "$d" POSTGRES_PASSWORD)" != "$(val "$d2" POSTGRES_PASSWORD)" ]] || fail "two runs made the same password"
ok "two runs make different secrets"

# 3
before="$(cksum <"$d/.env")"
if run "$d" >/dev/null 2>"$scratch/e3"; then fail "overwrote an existing .env"; fi
[[ "$(cksum <"$d/.env")" == "$before" ]] || fail ".env changed"
grep -q "exists" "$scratch/e3" || fail "no clear message"
[[ -z "$(find "$d" -name '.gen-env.*')" ]] || fail "temp file left behind"
d3="$(fresh c)"
ln -s "$scratch/nowhere" "$d3/.env"
if run "$d3" >/dev/null 2>&1; then fail "wrote through a symlink"; fi
[[ ! -e "$scratch/nowhere" ]] || fail "symlink target was created"
ok "refuses to overwrite (file and symlink), leaves nothing behind"

# 4
d4="$(fresh d)"
for miss in WEB_HOST FILES_HOST LAUNCHPAD_VERSION; do
	if (cd "$d4" && WEB_HOST="$W" FILES_HOST="$F" LAUNCHPAD_VERSION="$V" env -u "$miss" "$gen" --non-interactive) >/dev/null 2>"$scratch/e4"; then fail "ran without $miss"; fi
	grep -q "$miss is not set" "$scratch/e4" || fail "no message for missing $miss"
	[[ ! -e "$d4/.env" ]] || fail "wrote .env without $miss"
done
bad() { # bad "label" WEB FILES VERSION
	if (cd "$d4" && WEB_HOST="$2" FILES_HOST="$3" LAUNCHPAD_VERSION="$4" "$gen" --non-interactive) >"$scratch/o4" 2>"$scratch/e4"; then fail "accepted: $1"; fi
	[[ ! -e "$d4/.env" ]] || fail "wrote .env for: $1"
}
bad "a scheme" "https://lp.example.com" "$F" "$V"
bad "a path" "lp.example.com/x" "$F" "$V"
bad "a port" "lp.example.com:8080" "$F" "$V"
bad "a space" "lp example.com" "$F" "$V"
bad "equal hosts" "$W" "$W" "$V"
bad "a v prefix" "$W" "$F" "v1.2.3"
bad "latest" "$W" "$F" "latest"
bad "a quote" "$W" "$F" "1.2'3"
ok "missing and bad values are refused, nothing written"

# 5: no openssl on PATH
d5="$(fresh e)"
mkdir -p "$scratch/nobin"
for t in bash env sed cut tr od mktemp chmod ln rm dirname cat printf grep; do
	p="$(command -v "$t" || true)"
	[[ -n "$p" && -x "$p" ]] && ln -sf "$p" "$scratch/nobin/$t"
done
(cd "$d5" && PATH="$scratch/nobin" WEB_HOST="$W" FILES_HOST="$F" LAUNCHPAD_VERSION="$V" "$scratch/nobin/bash" "$gen" --non-interactive) >/dev/null 2>"$scratch/e5" || fail "no-openssl run failed: $(cat "$scratch/e5")"
[[ "$(val "$d5" POSTGRES_PASSWORD)" =~ ^[0-9a-f]{64}$ ]] || fail "fallback made a bad secret"
ok "works without openssl (urandom fallback)"
echo "test-gen-env: PASS"
