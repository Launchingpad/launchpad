#!/usr/bin/env bash
# Checks the CapRover one-click template.
#   deploy/caprover/test-one-click.sh
# 1. one-click-app.yml equals what gen-one-click.sh makes now (no drift from
#    the .in file or from deploy/nginx/default.conf.template).
# 2. The YAML parses (needs ruby; skipped with a message when it is missing).
# 3. A $$ is always the start of a CapRover variable ($$cap_...).
# 4. The web and silo CMD lines hold no $$ (in a shell, $$ is the process id).
# 5. The web nginx config has no leftover http://app: upstream.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
yml="$here/one-click-app.yml"
fail() { echo "test-one-click: FAIL: $*" >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

"$here/gen-one-click.sh" "$work/generated.yml"
if ! diff -u "$yml" "$work/generated.yml" >"$work/diff"; then
	cat "$work/diff" >&2
	fail "one-click-app.yml is out of date; run deploy/caprover/gen-one-click.sh"
fi
echo "ok: one-click-app.yml is current"

if command -v ruby >/dev/null 2>&1; then
	ruby -ryaml -e 'YAML.load_file(ARGV[0])' "$yml" || fail "the YAML does not parse"
	echo "ok: the YAML parses"
else
	echo "skip: ruby is not installed, the YAML parse check did not run"
fi

# Every $$ must be followed by cap_.
# shellcheck disable=SC2016  # $$cap_ is literal text, not a variable
if sed 's/\$\$cap_//g' "$yml" | grep -n '\$\$' >&2; then
	fail "a \$\$ that is not \$\$cap_"
fi
echo "ok: every \$\$ is a CapRover variable"

if grep -n '^ *- CMD ' "$yml" | grep '\$\$' >&2; then
	fail "a CMD line holds \$\$"
fi
echo "ok: no \$\$ in a CMD line"

if grep -n 'http://app:' "$yml" >&2; then
	fail "the nginx config in the template still points at http://app:"
fi
# shellcheck disable=SC2016  # $$cap_ is literal text, not a variable
grep -q 'http://srv-captain--\$\$cap_appname-api:8080' "$yml" ||
	fail "the nginx config does not point at the API app"
echo "ok: the nginx config points at the API app"

echo "test-one-click: PASS"
