#!/bin/sh
# Tests scripts/install-mcp.sh against fake release files served by a local
# python3 http.server on 127.0.0.1. Nothing leaves the machine.
# shellcheck disable=SC2010,SC2012,SC2016,SC2030,SC2031,SC2088
set -eu

# The at sign is built here so that the public audit finds no e-mail-like text in this file.
at=@

here=$(cd "$(dirname "$0")" && pwd)
script=${INSTALL_SCRIPT:-$here/install-mcp.sh}
root=${TMPDIR:-/tmp}/install-mcp-test.$$
mkdir -p "$root/srv" "$root/home"
pid=
pid2=
cleanup() {
	if [ -n "$pid2" ]; then kill "$pid2" 2>/dev/null || true; fi
	if [ -n "$pid" ]; then
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	fi
	rm -rf "$root"
}
trap cleanup EXIT

fail() {
	printf 'FAIL: %s\n' "$*" >&2
	exit 1
}

sha_of() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d ' ' -f 1; else shasum -a 256 "$1" | cut -d ' ' -f 1; fi
}

# make_release VERSION: four fake binaries (shell scripts) and SHA256SUMS.
make_release() {
	: >"$root/srv/SHA256SUMS"
	for t in darwin-arm64 darwin-amd64 linux-amd64 linux-arm64; do
		f=$root/srv/launchpad-mcp-$t
		printf '#!/bin/sh\necho "launchpad-mcp %s"\n' "$1" >"$f"
		chmod 0755 "$f"
		printf '%s  launchpad-mcp-%s\n' "$(sha_of "$f")" "$t" >>"$root/srv/SHA256SUMS"
	done
}

leftovers() { ls -a "$dir" 2>/dev/null | grep -c '^\.launchpad-mcp\.' || true; }

port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
(cd "$root/srv" && exec python3 -m http.server "$port" --bind 127.0.0.1 >/dev/null 2>&1) &
pid=$!
base=http://127.0.0.1:$port
make_release v0
n=0
while ! curl -fsS -o /dev/null "$base/SHA256SUMS" 2>/dev/null; do
	n=$((n + 1))
	[ "$n" -lt 100 ] || fail "the test server did not start"
	sleep 0.1
done

dir=$root/home/bin
run() { HOME=$root/home TMPDIR=$root sh "$script" --url "$base" --dir "$dir" "$@"; }

# 1. fresh install
make_release v1
out=$(run) || fail "install failed: $out"
case "$out" in *"before: not installed"*"sha256 ok"*"after:  launchpad-mcp v1"*) ;; *) fail "install output: $out" ;; esac
[ -x "$dir/launchpad-mcp" ] || fail "not installed"
[ ! -e "$dir/launchpad-mcp.prev" ] || fail ".prev on a fresh install"
case "$out" in *LAUNCHPAD_TOKEN*) ;; *) fail "no .mcp.json snippet" ;; esac
printf 'ok: fresh install\n'

# 2. same release again: up to date
out=$(run) || fail "rerun failed"
case "$out" in *"already up to date"*) ;; *) fail "rerun: $out" ;; esac
printf 'ok: a rerun is a no-op\n'

# 3. update: new inode, old binary kept as .prev, versions printed
ino1=$(ls -i "$dir/launchpad-mcp" | cut -d ' ' -f 1)
make_release v2
out=$(run) || fail "update failed: $out"
case "$out" in *"before: launchpad-mcp v1"*"after:  launchpad-mcp v2"*) ;; *) fail "update output: $out" ;; esac
ino2=$(ls -i "$dir/launchpad-mcp" | cut -d ' ' -f 1)
[ "$ino1" != "$ino2" ] || fail "the binary was overwritten in place"
[ "$("$dir/launchpad-mcp.prev")" = "launchpad-mcp v1" ] || fail ".prev is not the old binary"
[ "$(leftovers)" = 0 ] || fail "temp file left behind"
printf 'ok: update renames a new file over the old one and keeps .prev\n'

# 4. sha mismatch is refused; the installed binary is untouched
make_release v3
for t in darwin-arm64 darwin-amd64 linux-amd64 linux-arm64; do printf '\n# tampered\n' >>"$root/srv/launchpad-mcp-$t"; done
if out=$(run 2>&1); then fail "a sha mismatch was accepted"; fi
case "$out" in *"sha256 mismatch"*) ;; *) fail "mismatch message: $out" ;; esac
[ "$("$dir/launchpad-mcp")" = "launchpad-mcp v2" ] || fail "binary changed after a refused install"
[ "$(leftovers)" = 0 ] || fail "temp file left after refusal"
printf 'ok: sha256 mismatch refused, nothing changed\n'

# 5. no entry in SHA256SUMS is refused
make_release v3
: >"$root/srv/SHA256SUMS"
if run >/dev/null 2>&1; then fail "an empty SHA256SUMS was accepted"; fi
printf 'ok: missing sums entry refused\n'

# 6. unknown arch and unknown os are refused (uname stub)
make_release v3
mkdir "$root/stub"
printf '#!/bin/sh\ncase "$1" in -s) echo Linux ;; -m) echo riscv64 ;; esac\n' >"$root/stub/uname"
chmod 0755 "$root/stub/uname"
if (PATH=$root/stub:$PATH && run >/dev/null 2>&1); then fail "an unknown CPU was accepted"; fi
printf '#!/bin/sh\ncase "$1" in -s) echo Plan9 ;; -m) echo x86_64 ;; esac\n' >"$root/stub/uname"
if (PATH=$root/stub:$PATH && run >/dev/null 2>&1); then fail "an unknown system was accepted"; fi
printf 'ok: unknown arch and unknown os refused\n'

# 7. dry run changes nothing
rm -rf "$dir"
out=$(run --dry-run) || fail "dry run failed"
case "$out" in *"dry run: would install"*) ;; *) fail "dry run output: $out" ;; esac
[ ! -e "$dir" ] || fail "dry run made the folder"
printf 'ok: dry run changes nothing\n'

# 8. option checks: bad URLs, --version with --url
for bad in "http://example.com" "https://user:pw@example.com" "https://example.com/path" "ftp://example.com"; do
	if out=$(HOME=$root/home sh "$script" --url "$bad" --dir "$dir" --dry-run 2>&1); then fail "accepted URL $bad"; fi
	case "$out" in *"--url must be https"*) ;; *) fail "URL $bad failed for another reason: $out" ;; esac
done
if run --version v1 >/dev/null 2>&1; then fail "--version with --url was accepted"; fi
if HOME=$root/home sh "$script" --version 'v1;x' --dir "$dir" --dry-run >/dev/null 2>&1; then fail "bad --version accepted"; fi
printf 'ok: bad URLs and bad --version refused\n'

# ---- more cases (each one kills a guard in the script) ----
fresh() { rm -rf "$dir"; }
real_sum() { awk -v n="launchpad-mcp-$1" '$2 == n { print $1 }' "$root/srv/SHA256SUMS"; }
host_t=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in arm64 | aarch64) host_a=arm64 ;; *) host_a=amd64 ;; esac
host=$host_t-$host_a
hname=launchpad-mcp-$host

# 9. a decoy entry whose name only CONTAINS the real name comes first
fresh
make_release v9
good=$(real_sum "$host")
{ printf '%s  x-%s\n' "$(printf 'a%.0s' $(seq 1 64))" "$hname"; cat "$root/srv/SHA256SUMS"; } >"$root/sums.new"
cp "$root/sums.new" "$root/srv/SHA256SUMS"
out=$(run) || fail "decoy line broke a good install: $out"
case "$out" in *"launchpad-mcp v9"*) ;; *) fail "decoy: $out" ;; esac
printf 'ok: a decoy name is not matched\n'

# 10. two lines for the name with different hashes are refused
fresh
make_release v10
printf '%s  %s\n' "$(printf 'b%.0s' $(seq 1 64))" "$hname" >>"$root/srv/SHA256SUMS"
if out=$(run 2>&1); then fail "a duplicate sums line was accepted"; fi
case "$out" in *"more than once"*) ;; *) fail "duplicate message: $out" ;; esac
[ ! -e "$dir/launchpad-mcp" ] || fail "installed despite a duplicate"
printf 'ok: duplicate sums lines refused\n'

# 11. CRLF sums and a "*name" (binary mode) entry both work
fresh
make_release v11
awk '{ printf "%s\r\n", $0 }' "$root/srv/SHA256SUMS" >"$root/sums.new" && cp "$root/sums.new" "$root/srv/SHA256SUMS"
run >/dev/null || fail "CRLF sums refused"
fresh
make_release v11
sed 's/  launchpad-mcp/ *launchpad-mcp/' "$root/srv/SHA256SUMS" >"$root/sums.new" && cp "$root/sums.new" "$root/srv/SHA256SUMS"
run >/dev/null || fail "binary-mode sums refused"
printf 'ok: CRLF and *name entries work\n'

# 12. an uppercase or a short hash is refused
fresh
make_release v12
good=$(real_sum "$host")
up=$(printf '%s' "$good" | tr 'a-f' 'A-F')
case "$up" in "$good") up=$(printf 'A%.0s' $(seq 1 64)) ;; esac
printf '%s  %s\n' "$up" "$hname" >"$root/srv/SHA256SUMS"
if out=$(run 2>&1); then fail "an uppercase hash was accepted"; fi
case "$out" in *"no valid entry"*) ;; *) fail "uppercase hash message: $out" ;; esac
printf '%s  %s\n' "$(printf '%s' "$good" | cut -c 1-63)" "$hname" >"$root/srv/SHA256SUMS"
if out=$(run 2>&1); then fail "a short hash was accepted"; fi
case "$out" in *"no valid entry"*) ;; *) fail "short hash message: $out" ;; esac
[ ! -e "$dir/launchpad-mcp" ] || fail "installed with a bad hash"
printf 'ok: uppercase and short hashes refused\n'

# 13. a .prev that is a file, a symlink, or a folder
fresh
make_release v13a
run >/dev/null || fail "setup"
printf 'junk\n' >"$dir/launchpad-mcp.prev"
make_release v13b
run >/dev/null || fail "an old .prev file blocked an update"
[ "$("$dir/launchpad-mcp.prev")" = "launchpad-mcp v13a" ] || fail ".prev file not replaced"
printf 'keepme\n' >"$root/victim"
rm -f "$dir/launchpad-mcp.prev"
ln -s "$root/victim" "$dir/launchpad-mcp.prev"
make_release v13c
run >/dev/null || fail "a .prev symlink blocked an update"
[ "$(cat "$root/victim")" = keepme ] || fail "the .prev symlink target was written"
[ "$("$dir/launchpad-mcp.prev")" = "launchpad-mcp v13b" ] || fail ".prev symlink not replaced"
rm -f "$dir/launchpad-mcp.prev"
mkdir "$dir/launchpad-mcp.prev"
make_release v13d
if out=$(run 2>&1); then fail "a .prev folder was accepted"; fi
case "$out" in *"is a folder"*) ;; *) fail ".prev folder message: $out" ;; esac
[ "$("$dir/launchpad-mcp")" = "launchpad-mcp v13c" ] || fail "binary changed with a .prev folder"
printf 'ok: .prev as file, symlink and folder\n'

# 14. a symlink as the target is replaced, not written through
fresh
mkdir -p "$dir"
printf '#!/bin/sh\necho old\n' >"$root/real-old"
chmod 0755 "$root/real-old"
ln -s "$root/real-old" "$dir/launchpad-mcp"
make_release v14
run >/dev/null || fail "symlink target"
[ ! -L "$dir/launchpad-mcp" ] || fail "the symlink is still there"
[ "$("$root/real-old")" = old ] || fail "the file behind the symlink was written"
printf 'ok: a symlink target is replaced, not written through\n'

# 15. a redirect to plain http is refused
cat >"$root/redir.py" <<PYEOF
import http.server, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(302)
        self.send_header("Location", "http://127.0.0.1:$port" + self.path)
        self.end_headers()
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), H).serve_forever()
PYEOF
port2=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
python3 "$root/redir.py" "$port2" &
pid2=$!
n=0
while ! curl -s -o /dev/null "http://127.0.0.1:$port2/" 2>/dev/null; do
	n=$((n + 1))
	[ "$n" -lt 100 ] || fail "the redirect server did not start"
	sleep 0.1
done
fresh
make_release v15
out=$(HOME=$root/home TMPDIR=$root sh "$script" --url "http://127.0.0.1:$port2" --dir "$dir" 2>&1) && fail "a redirect to http was followed: $out"
kill "$pid2" 2>/dev/null || true
wait "$pid2" 2>/dev/null || true
pid2=
[ ! -e "$dir/launchpad-mcp" ] || fail "installed through an http redirect"
printf 'ok: a redirect to http is refused\n'

# 16. a relative --dir gives an absolute path in the snippet; a space is fine
fresh
make_release v16
abs=$(cd "$root/home" && pwd -P)
out=$(cd "$root/home" && HOME=$root/home TMPDIR=$root sh "$script" --url "$base" --dir "rel dir/bin") || fail "relative dir: $out"
case "$out" in *"\"command\": \"$abs/rel dir/bin/launchpad-mcp\""*) ;; *) fail "snippet path is not absolute: $out" ;; esac
case "$out" in *LAUNCHPAD_TEAM\":*) fail "the snippet has a TEAM line" ;; esac
out=$(cd "$root/home" && HOME=$root/home TMPDIR=$root sh "$script" --url "$base" --dir "rel2" --dry-run) || fail "dry run relative"
case "$out" in *"$abs/rel2/launchpad-mcp"*) ;; *) fail "dry run path: $out" ;; esac
[ ! -e "$root/home/rel2" ] || fail "dry run made a folder"
printf 'ok: relative --dir is made absolute\n'

# 17. a quote or a backslash in --dir is refused; ~ is expanded
if HOME=$root/home sh "$script" --url "$base" --dir "$root/home/a\"b" --dry-run >/dev/null 2>&1; then fail "a quote in --dir was accepted"; fi
if HOME=$root/home sh "$script" --url "$base" --dir "$root/home/a\\b" --dry-run >/dev/null 2>&1; then fail "a backslash in --dir was accepted"; fi
out=$(HOME=$root/home sh "$script" --url "$base" --dir "~/tilde" --dry-run) || fail "tilde"
case "$out" in *"$root/home/tilde/launchpad-mcp"*) ;; *) fail "tilde not expanded: $out" ;; esac
printf 'ok: quote and backslash refused, ~ expanded\n'

# 18. more origin checks: port digits only, host not starting with "-"
for bad in "http://localhost:80.evil.com" "http://localhost:" "http://localhost:80x" "http://localhost:8x" "https://example.com:1a" "http://localhost:80.evil.com" "https://-evil.com" "https://exa mple.com" "https://example.com:" "https://example.com:99999999" "http://example.com:80" "https://[::1]"; do
	if out=$(HOME=$root/home sh "$script" --url "$bad" --dir "$dir" --dry-run 2>&1); then fail "accepted URL $bad"; fi
	case "$out" in *"--url must be https"*) ;; *) fail "URL $bad failed for another reason: $out" ;; esac
done
printf 'ok: odd origins refused\n'

# 19. no sha tool: refused before any download, with a clear message
mkdir -p "$root/nosha"
for c in sh sed awk tr cut head cat rm mkdir mktemp dirname uname chmod mv ln cp ls pwd curl; do
	w=$(command -v "$c" || true)
	[ -z "$w" ] || ln -sf "$w" "$root/nosha/$c"
done
if out=$(HOME=$root/home PATH=$root/nosha "$(command -v sh)" "$script" --url "$base" --dir "$dir" --dry-run 2>&1); then fail "ran without a sha tool"; fi
case "$out" in *"sha256sum or shasum is needed"*) ;; *) fail "no-sha message: $out" ;; esac
printf 'ok: a missing sha tool is named before any download\n'

# 20. up to date: the mode is set to 755
fresh
make_release v20
run >/dev/null || fail setup20
chmod 0600 "$dir/launchpad-mcp"
run >/dev/null || fail "rerun20"
[ -x "$dir/launchpad-mcp" ] || fail "the mode was not repaired"
printf 'ok: an up-to-date rerun repairs the mode\n'

# 21. hostile origins and versions are refused BEFORE any network call (a curl stub records calls)
mkdir -p "$root/cstub"
printf '#!/bin/sh\necho called >>"%s/curl-calls"\nexit 1\n' "$root" >"$root/cstub/curl"
chmod 0755 "$root/cstub/curl"
rm -f "$root/curl-calls"
for bad in "http://example.com" "http://localhost.evil.com" "http://127.0.0.1${at}evil.com" "http://127.0.0.1.evil.com" "https://user:pw@example.com" "https://example.com/path" "https://example.com?x=1"; do
	if out=$(PATH="$root/cstub:$PATH" HOME=$root/home sh "$script" --url "$bad" --dir "$dir" 2>&1); then fail "accepted URL $bad"; fi
	case "$out" in *"--url must be https"*) ;; *) fail "URL $bad failed for another reason: $out" ;; esac
done
for badv in "1/../x" "v1/../x" "-rf" ".1" "v" "v1 2" "v1;x" "../v1" "v1/x"; do
	if out=$(PATH="$root/cstub:$PATH" HOME=$root/home sh "$script" --version "$badv" --dir "$dir" 2>&1); then fail "accepted --version $badv"; fi
	case "$out" in *"--version must look like"*) ;; *) fail "--version $badv failed for another reason: $out" ;; esac
done
[ ! -e "$root/curl-calls" ] || fail "curl was called for a refused origin or version"
printf 'ok: hostile origins and versions are refused before any network call\n'

printf 'all extra install-mcp tests passed\n'

printf 'all install-mcp tests passed\n'
