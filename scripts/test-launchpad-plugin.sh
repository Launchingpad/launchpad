#!/bin/sh
# Tests the Claude Code plugin in plugins/launchpad: the launcher scripts/launchpad-mcp
# against fake release files served by a local python3 http.server on 127.0.0.1
# (nothing leaves the machine), and the shape of the manifests.
# shellcheck disable=SC2010,SC2015,SC2016,SC2030,SC2031,SC2086
set -eu

# The at sign is built here so that the public audit finds no e-mail-like text in this file.
at=@

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/.." && pwd)
plugin=$repo/plugins/launchpad
root=${TMPDIR:-/tmp}/launchpad-plugin-test.$$
mkdir -p "$root/srv" "$root/home" "$root/stub"
pid=
cleanup() {
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

# make_release LABEL: four fake binaries (shell scripts that print their label and arguments).
make_release() {
	: >"$root/srv/SHA256SUMS"
	for t in darwin-arm64 darwin-amd64 linux-amd64 linux-arm64; do
		f=$root/srv/launchpad-mcp-$t
		printf '#!/bin/sh\necho "fake-mcp %s args=$*"\n' "$1" >"$f"
		chmod 0755 "$f"
		printf '%s  launchpad-mcp-%s\n' "$(sha_of "$f")" "$t" >>"$root/srv/SHA256SUMS"
	done
}

# 0. manifests
python3 -c '
import json, sys
repo = sys.argv[1]
m = json.load(open(repo + "/.claude-plugin/marketplace.json"))
p = json.load(open(repo + "/plugins/launchpad/.claude-plugin/plugin.json"))
s = json.load(open(repo + "/plugins/launchpad/.mcp.json"))
assert m["plugins"][0]["name"] == p["name"] == "launchpad"
assert m["plugins"][0]["source"] == "./plugins/launchpad"
assert "version" not in m["plugins"][0], "the version lives in plugin.json only"
uc = p["userConfig"]
assert uc["token"]["sensitive"] is True and uc["token"]["required"] is True
env = s["mcpServers"]["launchpad"]["env"]
for k, v in env.items():
    assert v.startswith("${user_config.") and v.endswith("}"), k
    assert v[len("${user_config."):-1] in uc, k
assert env["LAUNCHPAD_TOKEN"] == "${user_config.token}"
' "$repo" || fail "manifests"
[ ! -e "$plugin/bin" ] || fail "plugins/launchpad/bin exists (it would land on the agent PATH)"
[ -x "$plugin/scripts/launchpad-mcp" ] || fail "launcher is not executable"
python3 -c "
import json, sys
h = json.load(open(sys.argv[1] + '/hooks/hooks.json'))['hooks']['SessionStart'][0]['hooks'][0]
assert h['async'] is True and h['command'].endswith('/scripts/launchpad-mcp\\\" --prefetch'), h
" "$plugin" || fail "hooks.json"
if grep -rIl 'odt_[A-Za-z0-9]' "$plugin" "$repo/.claude-plugin" >/dev/null 2>&1; then fail "a token-like string is in the plugin"; fi
cmp -s "$repo/scripts/install-mcp.sh" "$plugin/lib/install-mcp.sh" || fail "plugins/launchpad/lib/install-mcp.sh differs from scripts/install-mcp.sh (copy it again)"
printf 'ok: manifests, no token, installer copy is current\n'

port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
(cd "$root/srv" && exec python3 -m http.server "$port" --bind 127.0.0.1 >/dev/null 2>&1) &
pid=$!
base=http://127.0.0.1:$port
make_release v1
n=0
while ! curl -fsS -o /dev/null "$base/SHA256SUMS" 2>/dev/null; do
	n=$((n + 1))
	[ "$n" -lt 100 ] || fail "the test server did not start"
	sleep 0.1
done

data=$root/data
run() { # [ARGS]: the launcher with the plugin root and a fresh-or-given data folder
	env -u LAUNCHPAD_MCP_RELEASE_URL HOME="$root/home" TMPDIR="$root" CLAUDE_PLUGIN_ROOT="${T_ROOT:-$plugin}" CLAUDE_PLUGIN_DATA="${T_DATA:-$data}" \
		LAUNCHPAD_MCP_RELEASE_URL="${T_URL:-$base}" "$plugin/scripts/launchpad-mcp" "$@"
}

# 1. first run downloads, verifies, execs; stdout holds only the binary's output
out=$(run --profile reviewer 2>"$root/err") || fail "first run failed: $(cat "$root/err")"
[ "$out" = "fake-mcp v1 args=--profile reviewer" ] || fail "stdout was: $out"
ver=$(sed -n 's/^[[:space:]]*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$plugin/.claude-plugin/plugin.json" | head -n 1)
[ -x "$data/v$ver/launchpad-mcp" ] || fail "binary not under the version folder"
printf 'ok: first run installs under v%s, stdout is clean, arguments pass through\n' "$ver"

# 2. second run is cached: the server is gone and a new release must not matter
kill "$pid" 2>/dev/null || true
wait "$pid" 2>/dev/null || true
pid=
make_release v2
out=$(run 2>"$root/err") || fail "cached run failed: $(cat "$root/err")"
[ "$out" = "fake-mcp v1 args=" ] || fail "cached run used: $out"
[ ! -s "$root/err" ] || fail "cached run was not silent: $(cat "$root/err")"
printf 'ok: second run is cached and needs no network\n'

# restart the server for the next tests
(cd "$root/srv" && exec python3 -m http.server "$port" --bind 127.0.0.1 >/dev/null 2>&1) &
pid=$!
n=0
while ! curl -fsS -o /dev/null "$base/SHA256SUMS" 2>/dev/null; do
	n=$((n + 1))
	[ "$n" -lt 100 ] || fail "the test server did not restart"
	sleep 0.1
done

# 3. a new plugin version fetches its own binary; the old one stays
mkdir -p "$root/plugin2"
cp -R "$plugin/." "$root/plugin2/"
sed -i.bak 's/"version"[[:space:]]*:[[:space:]]*"[^"]*"/"version": "9.9.9"/' "$root/plugin2/.claude-plugin/plugin.json"
rm -f "$root/plugin2/.claude-plugin/plugin.json.bak"
out=$(T_ROOT="$root/plugin2" run 2>"$root/err") || fail "update run failed: $(cat "$root/err")"
[ "$out" = "fake-mcp v2 args=" ] || fail "update run used: $out"
[ -x "$data/v9.9.9/launchpad-mcp" ] && [ -x "$data/v$ver/launchpad-mcp" ] || fail "version folders"
printf 'ok: a new plugin version fetches the matching binary\n'

# 4. a sha256 mismatch is refused and nothing is left
printf '#!/bin/sh\necho evil\n' >"$root/srv/launchpad-mcp-darwin-arm64"
cp "$root/srv/launchpad-mcp-darwin-arm64" "$root/srv/launchpad-mcp-darwin-amd64"
cp "$root/srv/launchpad-mcp-darwin-arm64" "$root/srv/launchpad-mcp-linux-amd64"
cp "$root/srv/launchpad-mcp-darwin-arm64" "$root/srv/launchpad-mcp-linux-arm64"
data2=$root/data-bad
if out=$(T_DATA="$data2" run 2>"$root/err"); then fail "a mismatch was accepted: $out"; fi
[ -z "$out" ] || fail "stdout not empty on a mismatch: $out"
grep -q 'sha256 mismatch' "$root/err" || fail "no mismatch message: $(cat "$root/err")"
[ ! -e "$data2/v$ver/launchpad-mcp" ] || fail "a binary was installed after a mismatch"
if ls -a "$data2/v$ver" | grep -q '^\.launchpad-mcp\.'; then fail "temp file left behind"; fi
printf 'ok: sha256 mismatch refused, nothing left\n'

# 5. unknown arch and unknown os are refused
make_release v3
for pair in "Linux riscv64" "Plan9 x86_64"; do
	set -- $pair
	printf '#!/bin/sh\ncase "$1" in -s) echo %s ;; -m) echo %s ;; esac\n' "$1" "$2" >"$root/stub/uname"
	chmod 0755 "$root/stub/uname"
	if out=$(PATH="$root/stub:$PATH" T_DATA="$root/data-odd" run 2>"$root/err"); then fail "$pair accepted: $out"; fi
	grep -q 'unsupported' "$root/err" || fail "no unsupported message for $pair: $(cat "$root/err")"
done
printf 'ok: unknown arch and unknown os refused\n'

# 6. odd plugin versions are refused before any download or folder
for v in '../x' '1/../x' '-rf' '.1' '1 2' '' 'v1.0.0'; do
	rm -rf "$root/plugin3" "$root/data-odd2"
	mkdir -p "$root/plugin3"
	cp -R "$plugin/." "$root/plugin3/"
	printf '{\n  "name": "launchpad",\n  "version": "%s"\n}\n' "$v" >"$root/plugin3/.claude-plugin/plugin.json"
	if (T_ROOT="$root/plugin3" T_DATA="$root/data-odd2" run >/dev/null 2>"$root/err"); then fail "odd version '$v' accepted"; fi
	[ ! -e "$root/data-odd2" ] || fail "odd version '$v' made a folder"
done
printf 'ok: odd plugin versions refused\n'

# 7. bad mirror origins are refused before any network call (a curl stub records calls)
mkdir -p "$root/cstub"
printf '#!/bin/sh\necho called >>"%s/curl-calls"\nexit 1\n' "$root" >"$root/cstub/curl"
chmod 0755 "$root/cstub/curl"
for bad in 'http://example.com' 'http://localhost.evil.com' "http://127.0.0.1${at}evil.com" 'https://user:pw@example.com' 'https://example.com/x' 'ftp://example.com'; do
	if out=$(PATH="$root/cstub:$PATH" T_URL="$bad" T_DATA="$root/data-bad-url" run 2>&1); then fail "mirror $bad accepted"; fi
	case "$out" in *"--url must be https"*) ;; *) fail "mirror $bad failed for another reason: $out" ;; esac
	[ ! -e "$root/curl-calls" ] || fail "curl was called for mirror $bad"
done
printf 'ok: bad mirror origins refused before any network call\n'

# 8. the mirror line shows the origin only; no credential reaches the installer or curl
mkdir -p "$root/estub"
printf '#!/bin/sh\nenv | grep -E "^(LAUNCHPAD_TOKEN|CLAUDE_PLUGIN_OPTION_[A-Za-z0-9_]*)=" >>"%s/leak" || true\nexec "%s" "$@"\n' "$root" "$(command -v curl)" >"$root/estub/curl"
chmod 0755 "$root/estub/curl"
make_release v8
rm -f "$root/leak"
out=$(PATH="$root/estub:$PATH" LAUNCHPAD_TOKEN=odt_secretvalue CLAUDE_PLUGIN_OPTION_TOKEN=odt_hooktoken CLAUDE_PLUGIN_OPTION_ORG=acme T_DATA="$root/data-tok" run 2>"$root/err") || fail "token run failed: $(cat "$root/err")"
grep -q "using mirror http://127.0.0.1:$port\$" "$root/err" || fail "no mirror line: $(cat "$root/err")"
[ ! -s "$root/leak" ] || fail "a credential reached curl: $(sed 's/=.*/=<hidden>/' "$root/leak")"
if grep -q -e odt_secretvalue -e odt_hooktoken "$root/err"; then fail "token printed"; fi
# the prefetch hook has the same environment
rm -rf "$root/data-tok"
(PATH="$root/estub:$PATH" CLAUDE_PLUGIN_OPTION_TOKEN=odt_hooktoken T_DATA="$root/data-tok" run --prefetch >/dev/null 2>&1) || fail "prefetch with option env failed"
[ ! -s "$root/leak" ] || fail "a hook credential reached curl in prefetch"
printf 'ok: mirror line shows the origin, no credential is passed on\n'

# 8b. a mirror URL with user info, path, query and fragment is never echoed
for bad in 'https://user:pw@example.com/x?secret=abc#f' 'https://example.com/x?secret=abc' 'https://example.com#frag'; do
	out=$(T_URL="$bad" T_DATA="$root/data-echo" run 2>&1) && fail "mirror $bad accepted"
	for w in user pw secret abc frag '/x'; do
		case "$out" in *"$w"*) fail "mirror text echoed '$w' from $bad: $out" ;; esac
	done
done
printf 'ok: a mirror with user info, path, query or fragment is not echoed\n'

# 9. --prefetch downloads, prints nothing on stdout, never fails the session
out=$(T_DATA="$root/data-pre" run --prefetch 2>/dev/null) || fail "prefetch failed"
[ -z "$out" ] || fail "prefetch wrote stdout: $out"
[ -x "$root/data-pre/v$ver/launchpad-mcp" ] || fail "prefetch did not install"
out=$(T_URL="http://example.com" T_DATA="$root/data-pre2" run --prefetch 2>/dev/null) || fail "prefetch failure exited non-zero"
[ -z "$out" ] || fail "prefetch failure wrote stdout"
printf 'ok: --prefetch works and never fails\n'

# 10. a literal ${user_config.x} in an optional variable is treated as unset
printf '#!/bin/sh\necho "team=[$LAUNCHPAD_TEAM] files=[$LAUNCHPAD_FILES_URL]"\n' >"$root/srv/launchpad-mcp-darwin-arm64"
for t in darwin-amd64 linux-amd64 linux-arm64; do cp "$root/srv/launchpad-mcp-darwin-arm64" "$root/srv/launchpad-mcp-$t"; done
: >"$root/srv/SHA256SUMS"
for t in darwin-arm64 darwin-amd64 linux-amd64 linux-arm64; do printf '%s  launchpad-mcp-%s\n' "$(sha_of "$root/srv/launchpad-mcp-$t")" "$t" >>"$root/srv/SHA256SUMS"; done
out=$(LAUNCHPAD_TEAM='${user_config.team}' LAUNCHPAD_FILES_URL='${user_config.files_url}' T_DATA="$root/data-lit" run 2>/dev/null) || fail "literal run"
[ "$out" = "team=[] files=[]" ] || fail "literals not cleared: $out"
out=$(LAUNCHPAD_TEAM=ACME LAUNCHPAD_FILES_URL=https://f.example.com T_DATA="$root/data-lit" run 2>/dev/null) || fail "real values run"
[ "$out" = "team=[ACME] files=[https://f.example.com]" ] || fail "real values changed: $out"
printf 'ok: unsubstituted optional values become empty\n'

printf 'all plugin tests passed\n'
