#!/bin/sh
# Make deploy/caprover/one-click-app.yml from one-click-app.yml.in and the
# production nginx config (deploy/nginx/default.conf.template).
#   deploy/caprover/gen-one-click.sh [output-file]
# The marker line @@NGINX_CONF@@ in the .in file is replaced by the nginx
# config, indented for a YAML block scalar. In the config, the API upstream
# http://app:8080 becomes the CapRover name of the API app. POSIX sh and awk
# only, so it runs the same on macOS and Linux.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
src="$here/../nginx/default.conf.template"
tpl="$here/one-click-app.yml.in"
out=${1:-"$here/one-click-app.yml"}
# shellcheck disable=SC2016  # $$cap_ is literal text, not a variable
upstream='http://srv-captain--$$cap_appname-api:8080'

[ -f "$src" ] || { echo "gen-one-click: missing $src" >&2; exit 1; }
[ -f "$tpl" ] || { echo "gen-one-click: missing $tpl" >&2; exit 1; }
# A $$ in the nginx config would clash with the CapRover variable syntax.
if grep -q '\$\$' "$src"; then
	echo "gen-one-click: $src holds a \$\$" >&2
	exit 1
fi

awk -v conf="$src" -v up="$upstream" '
BEGIN {
	n = 0
	while ((getline line < conf) > 0) {
		c = gsub("http://app:8080", up, line)
		hits += c
		if (line ~ /http:\/\/app[:\/]/) bad = 1
		lines[n++] = line
	}
	close(conf)
}
/^@@NGINX_CONF@@$/ {
	for (i = 0; i < n; i++) {
		if (lines[i] == "") print ""
		else print "        " lines[i]
	}
	seen = 1
	next
}
{ print }
END {
	if (!seen) { print "gen-one-click: marker not found" > "/dev/stderr"; exit 1 }
	if (hits < 1) { print "gen-one-click: no http://app:8080 upstream in the nginx config" > "/dev/stderr"; exit 1 }
	if (bad) { print "gen-one-click: another http://app upstream is left" > "/dev/stderr"; exit 1 }
}' "$tpl" >"$out.tmp.$$" || { rm -f "$out.tmp.$$"; exit 1; }
mv -f "$out.tmp.$$" "$out"
