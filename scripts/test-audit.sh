#!/usr/bin/env bash
# test-audit.sh: mutant checks for scripts/audit-public.sh. Each case plants one
# value in a tiny tree and the audit must report a hit; a clean tree and the
# allowed fixture values must give none. Run with /bin/bash (3.2 on a Mac).
# The planted values are built from parts so this file is itself not a hit.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/audit-test.XXXXXX")"
trap 'rm -rf "$work"' EXIT
fail() {
	echo "test-audit: FAIL: $*" >&2
	exit 1
}

audit() { /bin/bash "$here/scripts/audit-public.sh" "$1" >"$work/out.log" 2>&1; }

# expect_hit NAME FILE CONTENT: plant CONTENT in FILE of a clean tree.
expect_hit() {
	local name="$1" file="$2" content="$3" d
	d="$(mktemp -d "$work/case.XXXXXX")"
	mkdir -p "$d/$(dirname "$file")"
	printf '%s\n' "$content" >"$d/$file"
	if audit "$d"; then fail "$name: the audit did not fire"; fi
	grep -q "HIT" "$work/out.log" || fail "$name: no HIT line"
	echo "ok: $name fires"
}
expect_clean() {
	local name="$1" file="$2" content="$3" d
	d="$(mktemp -d "$work/case.XXXXXX")"
	mkdir -p "$d/$(dirname "$file")"
	printf '%s\n' "$content" >"$d/$file"
	audit "$d" || {
		cat "$work/out.log" >&2
		fail "$name: false hit"
	}
	echo "ok: $name is clean"
}

clean="$work/clean"
mkdir "$clean"
printf 'hello\n' >"$clean/README.md"
audit "$clean" || fail "clean tree has a hit"
echo "ok: clean tree"

expect_hit "stray deploy/sample.env" deploy/sample.env "WEB_HOST=a"
expect_clean "an .env.example file" deploy/prod.env.example "WEB_HOST=a"
expect_hit "Google API key in a test file" a/x_test.go "k := \"AI""zaSyA1234567890123456789012345678901234\""
expect_hit "Stripe live key in a test file" a/x.test.ts "const k = 'sk_""live_abcDEF123'"
expect_hit "Slack webhook in a test file" a/x_test.go "u := \"https://hooks.""slack.com/services/T0/B0/xyz\""
expect_hit "URL credential" a/x_test.go "u := \"postgres://admin:hunter2""@db.internal:5432/x\""
expect_clean "allowed URL credentials" a/x_test.go "u := \"postgres://devops:devops@postgres:5432/x https://u:p@silo http://user:pw@host.example.com/\""
expect_hit "name in mixed case (host)" a/n.txt "see Minty""Water here"
expect_hit "name in upper case" a/n.txt "by WUN""SCH"
expect_hit "name in a project word" a/n.txt "the Ki""d""Chat app"
expect_hit "first name in upper case" a/n.txt "ask CHR""IS about it"
expect_hit "first name before an underscore" a/n.txt "see ch""ris_notes"
expect_hit "first name after an underscore" a/n.txt "see by_ch""ris"
expect_hit "first name before a digit" a/n.txt "see ch""ris2"
expect_clean "a longer word that holds the name" a/n.txt "see christmas and Christopher Street"

# macOS home folders are case-sensitive; the API path /users/{user} is not one.
expect_hit "a macOS home folder" a/n.txt "see /Us""ers/alex/src"
expect_hit "a macOS home folder, capital name" a/n.txt "see /Us""ers/Alex/src"
expect_hit "a /Volumes path" a/n.txt "see /Vol""umes/Disk/x"
expect_clean "the API path /users/" a/x_test.go "e.do(\"PUT\", \"/api/v1/orgs/acme/users/rita/team-access\")"

# Slack webhooks: only the known fake ids and the placeholder text pass.
hook="https://hooks.""slack.com/services"
expect_clean "fake Slack ids T000n/B000n" a/x_test.go "u := \"$hook/T0001/B0001/abcDEF123\""
expect_clean "fake Slack ids, encoded slash" a/x_test.go "u := \"$hook/T0001%2fB0001/B0001/abc\""
expect_clean "fake Slack ids T1/B1 and T9999/B9999" a/x_test.go "u := \"$hook/T1/B1/xyz $hook/T9999/B9999/x\""
expect_clean "Slack placeholder T.../B..." a/x.yaml "URL \`$hook/T.../B.../...\`"
expect_clean "Slack dot-dot path test" a/x_test.go "u := \"$hook/../B0001/abc\""
expect_clean "Slack prefix in help text" a/x.ts "'It starts with $hook/ and has three parts.' '$hook/…'"
expect_hit "a real-shaped Slack webhook" a/x_test.go "u := \"$hook/T0AB""CD1234/B0AB""CD5678/aBcD""eFgHiJkLmNoPqRsTuVwX\""
expect_hit "Slack id T0 is not an allowed fake" a/x_test.go "u := \"$hook/T0/B0/xyz\""
expect_hit "a real Slack webhook next to a fake one" a/x_test.go "a := \"$hook/T0001/B0001/abc\"; b := \"$hook/T0AB""CD1234/B0AB""CD5678/xyz\""

# The missing-issue id of the delete test passes in that file only.
expect_clean "the missing-issue id in the delete test" server/internal/api/delete_issue_integration_test.go "base+\"ORB-99""99\""
expect_hit "the missing-issue id in another file" server/internal/api/other_test.go "base+\"ORB-99""99\""
expect_hit "another ORB id in the delete test" server/internal/api/delete_issue_integration_test.go "base+\"ORB-12""34\""
expect_hit "another ORB id on the line of the allowed one" server/internal/api/delete_issue_integration_test.go "a := \"ORB-99""99\"; b := \"ORB-12""34\""

# Names of the author's other private repositories (built from parts here, as
# in the audit script), in text and in file names.
expect_hit "a private repository name (apps)" a/n.md "see launch""pad-apps"
expect_hit "a private repository name (relay)" a/n.md "see Launch""pad-Relay"
expect_hit "a private repository name (site)" a/n.md "see launch""pad-site"
expect_hit "a private repository name (out...)" a/n.md "see Out""post"
expect_hit "a private repository name with an underscore" a/n.md "see launch""pad_apps"
expect_hit "a private repository name with a space" a/n.md "see Launch""pad Relay"
expect_hit "a private repository name with no separator" a/n.md "see launch""padsite"
expect_hit "a private repository in a file name with an underscore" "launch""pad_relay/readme.txt" "hello"
expect_clean "the public plugin name" a/n.md "see launchpad-mcp and launchpad-tickets"

# File and folder names.
expect_hit "a host name in a file name" "a/minty""water-notes.txt" "hello"
expect_hit "a private repository in a folder name" "out""post/readme.txt" "hello"
expect_hit "the first name in a file name" "docs/ch""ris-notes.md" "hello"

# Paths that must not ship.
expect_hit "a node_modules folder" web/node_modules/x/index.js "x"
expect_hit "a .claude folder" .claude/settings.json "{}"
expect_hit "a nested .claude folder" a/.claude/agents/x.md "x"
expect_hit "a root .mcp.json" .mcp.json "{}"
expect_clean "the .claude-plugin folder" plugins/x/.claude-plugin/plugin.json "{}"
expect_hit "an .envrc file" .envrc "export A=1"

# .env.example files hold placeholders only (values built from parts).
expect_hit "a secret value in an .env.example" deploy/.env.example "API_TOKEN='abc""123def'"
expect_hit "a secret value in a sample.env.example" deploy/x.env.example "DB_PASSWORD=hunter""2"
expect_hit "a value for a _PAT key" deploy/.env.example "GITHUB_PAT='gh""p-abc123'"
expect_hit "a value for an API key name" deploy/.env.example "export STRIPE_API=sk""abc"
expect_hit "a value for a CREDENTIAL key" deploy/.env.example "GCP_CREDENTIALS=abc""def"
expect_clean "PATH is not a PAT" deploy/.env.example "PATH_PREFIX=/srv/app"
expect_clean "placeholder values in an .env.example" deploy/.env.example "$(printf '%s\n' "# c" "WEB_HOST='launchpad.example.com'" "API_TOKEN=''" "DB_PASSWORD='placeholder-not-a-secret'" "ENC_KEY='0000000000'" "S3_ACCESS_KEY='app-placeholder'" "VITE_API_BASE=")"

# Binary files: only the fonts folder and the screenshots folder may hold
# them, and those files must have the right structure.
# mk_bin KIND FILE: write a small file of that kind (perl, for exact bytes).
# png: a minimal PNG; png-text: with a tEXt chunk naming the host; png-exif:
# with an eXIf chunk; png-tail: bytes after IEND; png-noend: no IEND; jpeg,
# zip: those formats; woff2: starts with wOF2; woff2-bad: does not.
mk_bin() {
	local kind="$1" file="$2"
	mkdir -p "$(dirname "$file")"
	KIND="$kind" HOST="minty""water" perl -MCompress::Zlib -e '
		my $k = $ENV{KIND};
		sub c { my ($t, $d) = @_; return pack("N", length $d) . $t . $d . "\0\0\0\0" }
		my $png = "\x89PNG\r\n\x1a\n" . c("IHDR", "\0\0\0\1\0\0\0\1\x08\x02\0\0\0");
		my $idat = c("IDAT", compress("\0\0\0\0"));
		my $woff = "wOF2\0\x01\0\0" . pack("N", 16) . "\0\0\0\0";
		my $out =
			$k eq "png"           ? $png . $idat . c("IEND", "") :
			$k eq "png-text"      ? $png . c("tEXt", "Comment\0see $ENV{HOST}.example") . $idat . c("IEND", "") :
			$k eq "png-exif"      ? $png . c("eXIf", "MM\0*") . $idat . c("IEND", "") :
			$k eq "png-tail"      ? $png . $idat . c("IEND", "") . "trailing junk" :
			$k eq "png-noend"     ? $png . $idat :
			$k eq "png-ihdr"      ? "\x89PNG\r\n\x1a\n" . c("IHDR", "\0" x 12) . $idat . c("IEND", "") :
			$k eq "png-gama"      ? $png . c("gAMA", "\0" x 8) . $idat . c("IEND", "") :
			$k eq "png-idat-host" ? $png . c("IDAT", compress("see the host $ENV{HOST}.example")) . c("IEND", "") :
			$k eq "png-raw-host"  ? $png . c("IDAT", "see the host $ENV{HOST}.example") . c("IEND", "") :
			$k eq "png-noinflate" ? $png . c("IDAT", "x\x9c\0") . c("IEND", "") :
			$k eq "jpeg"          ? "\xff\xd8\xff\xe0\0\x10JFIF\0\x01\x01\0\0\x01" :
			$k eq "zip"           ? "PK\x03\x04\x14\0\0\0\0\0secret.txt\0\0" :
			$k eq "woff2"         ? $woff :
			$k eq "woff2-len"     ? "wOF2\0\x01\0\0" . pack("N", 99) . "\0\0\0\0" :
			$k eq "woff2-tail"    ? $woff . "see $ENV{HOST}.example" :
			                        "OTTO\0\x01\0\0\xff\xfe";
		binmode STDOUT; print $out' >"$file"
}
# bin_case NAME FILE KIND WANT [HIT_TEXT]
bin_case() {
	local name="$1" file="$2" kind="$3" want="$4" hit="${5:-HIT: binary files}" d
	d="$(mktemp -d "$work/case.XXXXXX")"
	mk_bin "$kind" "$d/$file"
	if audit "$d"; then
		[[ "$want" == clean ]] || fail "$name: the audit did not fire"
	else
		[[ "$want" == hit ]] || {
			cat "$work/out.log" >&2
			fail "$name: false hit"
		}
		grep -q "$hit" "$work/out.log" || {
			cat "$work/out.log" >&2
			fail "$name: not a '$hit' hit"
		}
	fi
	echo "ok: $name is $want"
}
bin_case "a font in the fonts folder" web/src/assets/fonts/x.woff2 woff2 clean
bin_case "a font elsewhere" web/public/x.woff2 woff2 hit
bin_case "an image" docs/x.png png hit
bin_case "a README screenshot" docs/screenshots/board.png png clean
bin_case "a screenshot in a subfolder" docs/screenshots/old/board.png png hit
bin_case "a JPEG in the screenshots folder" docs/screenshots/board.jpg jpeg hit
bin_case "a font in the screenshots folder" docs/screenshots/x.woff2 woff2 hit
bin_case "a PNG with a tEXt chunk" docs/screenshots/board.png png-text hit "HIT: allowed binary files with a wrong structure"
bin_case "a PNG with an eXIf chunk" docs/screenshots/board.png png-exif hit "chunk eXIf is not allowed"
bin_case "bytes after IEND" docs/screenshots/board.png png-tail hit "byte(s) after IEND"
bin_case "a PNG without IEND" docs/screenshots/board.png png-noend hit "no IEND chunk"
bin_case "a JPEG named .png" docs/screenshots/board.png jpeg hit "not a PNG file"
bin_case "a zip named .png" docs/screenshots/board.png zip hit "not a PNG file"
bin_case "a font that is not WOFF2" web/src/assets/fonts/x.woff2 woff2-bad hit "does not start with wOF2"
bin_case "an IHDR of the wrong length" docs/screenshots/board.png png-ihdr hit "chunk IHDR has length 12, not 13"
bin_case "a gAMA of the wrong length" docs/screenshots/board.png png-gama hit "chunk gAMA has length 8, not 4"
bin_case "the host inside the compressed image data" docs/screenshots/board.png png-idat-host hit "the image data matches"
bin_case "the host in raw image bytes" docs/screenshots/board.png png-raw-host hit "the raw bytes match"
bin_case "image data that does not inflate" docs/screenshots/board.png png-noinflate hit "does not inflate"
bin_case "a WOFF2 header length that is not the file length" web/src/assets/fonts/x.woff2 woff2-len hit "the WOFF2 header says 99 bytes"
bin_case "the host after a WOFF2 font" web/src/assets/fonts/x.woff2 woff2-tail hit "the raw bytes match"
d="$(mktemp -d "$work/case.XXXXXX")"
mkdir -p "$d/docs/screenshots"
printf 'see the host %s.example\n' "minty""water" >"$d/docs/screenshots/notes.png"
if audit "$d"; then fail "a text file named .png: the audit did not fire"; fi
grep -q "HIT: allowed binary files with a wrong structure" "$work/out.log" || fail "a text file named .png: no structure hit"
echo "ok: a text file named .png is hit"

# Text checks read every file as bytes: a NUL after the first block, or a
# Latin-1 byte, does not hide a line from them.
d="$(mktemp -d "$work/case.XXXXXX")"
perl -e 'print "x" x 4096, "\0\n", "see the host ", "minty", "water", ".example\n"' >"$d/notes.txt"
if audit "$d"; then fail "a NUL after the first block: the audit did not fire"; fi
grep -q "HIT: author hosts and machines" "$work/out.log" || fail "a NUL after the first block: the host line was not read"
echo "ok: a NUL after the first block does not hide a line"
d="$(mktemp -d "$work/case.XXXXXX")"
perl -e 'print "caf\xe9 au lait\nsee the host ", "minty", "water", ".example\n"' >"$d/notes.txt"
if audit "$d"; then fail "a Latin-1 byte: the audit did not fire"; fi
grep -q "HIT: author hosts and machines" "$work/out.log" || fail "a Latin-1 byte: the host line was not read"
if grep -q "HIT: binary files" "$work/out.log"; then fail "a Latin-1 text file was called binary"; fi
echo "ok: a Latin-1 byte does not hide a line"

# A file the audit cannot read is exit 2 ("NOT audited"), never a pass.
# (root reads a mode 000 file, so the case needs a normal user.)
if [[ "$(id -u)" != 0 ]]; then
	d="$(mktemp -d "$work/case.XXXXXX")"
	printf 'see the host %s.example\n' "minty""water" >"$d/locked.txt"
	chmod 000 "$d/locked.txt"
	rc=0
	audit "$d" || rc=$?
	chmod 600 "$d/locked.txt"
	((rc == 2)) || {
		cat "$work/out.log" >&2
		fail "an unreadable file: exit $rc, not 2"
	}
	grep -q "NOT audited" "$work/out.log" || fail "an unreadable file: no NOT audited line"
	echo "ok: an unreadable file is exit 2"
else
	echo "test-audit: SKIP: the unreadable-file case needs a normal user" >&2
fi

# Symbolic links and submodules.
d="$(mktemp -d "$work/case.XXXXXX")"
ln -s /etc/hosts "$d/hosts"
if audit "$d"; then fail "a symbolic link: the audit did not fire"; fi
grep -q "HIT: symbolic links or git submodules" "$work/out.log" || fail "a symbolic link: no link hit"
echo "ok: a symbolic link is hit"
expect_hit "a .gitmodules file" .gitmodules "[submodule \"x\"]"
expect_hit "a nested .git file (a submodule)" vendor/lib/.git "gitdir: ../../.git/modules/lib"
expect_hit "a nested .GIT folder in upper case" vendor/lib/.GIT/HEAD "ref: refs/heads/main"

# Commit identities (when the tree is a git repository).
id_case() {
	local name="$1" an="$2" ae="$3" cn="$4" ce="$5" want="$6" msg="${7:-one}" d rc=0 mode
	for mode in full identity-only; do
		d="$(mktemp -d "$work/case.XXXXXX")"
		printf 'hello\n' >"$d/README.md"
		git -C "$d" init -q
		git -C "$d" add -A
		GIT_AUTHOR_NAME="$an" GIT_AUTHOR_EMAIL="$ae" GIT_COMMITTER_NAME="$cn" GIT_COMMITTER_EMAIL="$ce" \
			git -C "$d" -c commit.gpgsign=false commit -q -m "$msg"
		rc=0
		if [[ "$mode" == full ]]; then audit "$d" || rc=$?; else /bin/bash "$here/scripts/audit-public.sh" "$d" --identity-only >"$work/out.log" 2>&1 || rc=$?; fi
		if [[ "$want" == hit ]]; then
			((rc == 1)) || fail "$name ($mode): exit $rc, not 1"
			grep -q "HIT: commit or tag identity" "$work/out.log" || fail "$name ($mode): no identity hit"
			if grep -qiF "$ae" "$work/out.log" || grep -qiF "$ce" "$work/out.log"; then fail "$name ($mode): the hit prints an address"; fi
		else
			((rc == 0)) || {
				cat "$work/out.log" >&2
				fail "$name ($mode): exit $rc, not 0"
			}
		fi
	done
	echo "ok: identity: $name is $want"
}
id_case "a noreply identity" pub 1+pub@users.noreply.github.com pub 1+pub@users.noreply.github.com clean
id_case "the host in the author address" pub "me@minty""water.example" pub 1+pub@users.noreply.github.com hit
id_case "the host in the committer address" pub 1+pub@users.noreply.github.com pub "me@minty""water.example" hit
id_case "the surname as author name" "A Wun""sch" 1+pub@users.noreply.github.com pub 1+pub@users.noreply.github.com hit
nr=1+pub@users.noreply.github.com
id_case "the host in a message trailer" pub "$nr" pub "$nr" hit "$(printf 'one\n\nSigned-off-by: pub <me@minty''water.example>')"
id_case "the first name in the message" pub "$nr" pub "$nr" hit "fix from ch""ris_review"
id_case "a plain message and trailer" pub "$nr" pub "$nr" clean "$(printf 'one\n\nCo-Authored-By: Bot <noreply@example.com>')"

# A git error never passes: the identity check exits 2 when git cannot read
# the repository (a .git file that points nowhere).
for mode in full identity-only; do
	d="$(mktemp -d "$work/case.XXXXXX")"
	printf 'hello\n' >"$d/README.md"
	printf 'gitdir: %s\n' "$work/no-such-git-dir" >"$d/.git"
	rc=0
	if [[ "$mode" == full ]]; then audit "$d" || rc=$?; else /bin/bash "$here/scripts/audit-public.sh" "$d" --identity-only >"$work/out.log" 2>&1 || rc=$?; fi
	((rc == 2)) || {
		cat "$work/out.log" >&2
		fail "a broken repository ($mode): exit $rc, not 2"
	}
	grep -q "were not checked" "$work/out.log" || fail "a broken repository ($mode): no clear message"
done
echo "ok: identity: a git error is exit 2"
# GIT_DIR in the caller's environment does not redirect the check.
d="$(mktemp -d "$work/case.XXXXXX")"
printf 'hello\n' >"$d/README.md"
git -C "$d" init -q
git -C "$d" add -A
GIT_AUTHOR_NAME=pub GIT_AUTHOR_EMAIL="me@minty""water.example" GIT_COMMITTER_NAME=pub GIT_COMMITTER_EMAIL="$nr" \
	git -C "$d" -c commit.gpgsign=false commit -q -m one
rc=0
GIT_DIR="$work/no-such-git-dir" /bin/bash "$here/scripts/audit-public.sh" "$d" --identity-only >"$work/out.log" 2>&1 || rc=$?
((rc == 1)) || fail "GIT_DIR in the environment changed the identity check (exit $rc)"
echo "ok: identity: GIT_DIR in the environment is ignored"

# The user-info case of the chat URL tests is not an address; others are.
expect_clean "user:pw@ in a fake Slack URL" a/x_test.go "u := \"https://user:pw@hooks.""slack.com/services/T0001/B0001/abc\""
expect_hit "another address at hooks.slack.com" a/n.txt "mail ops""@hooks.slack.com"

# Names that must not ship, at any depth and in any letter case.
expect_hit "a nested .mcp.json" a/b/.mcp.json "{}"
expect_hit "a CLAUDE.local.md in upper case, nested" docs/CLAUDE.LOCAL.MD "x"
expect_hit "a .Claude folder" a/.Claude/settings.json "{}"
expect_hit "an .ENV file" app/.ENV "A=1"
expect_hit "an .npmrc file" web/.npmrc "registry=https://registry.example.com/"
expect_hit "an npm token line" a/notes.md "//registry.example.com/:_auth""Token=abc"
expect_hit "a yarn token line" a/yarnrc.txt "npmAuth""Token: abc"
expect_clean "POSTGRES_PASSWORD is not an npm key" a/run.sh "docker run -e POSTGRES_PASSWORD=x postgres"
# shellcheck disable=SC2016 # the ${...} placeholders are literal text here
expect_clean "the plugin .mcp.json with placeholders" plugins/launchpad/.mcp.json "$(printf '%s\n' '{"mcpServers": {"launchpad": {"command": "${CLAUDE_PLUGIN_ROOT}/scripts/launchpad-mcp",' '"env": {"LAUNCHPAD_URL": "${user_config.url}", "LAUNCHPAD_TOKEN": "${user_config.token}"}}}}')"
expect_hit "the plugin .mcp.json with a URL" plugins/launchpad/.mcp.json "$(printf '%s\n' '{"mcpServers": {"launchpad": {"command": "x",' '"env": {"LAUNCHPAD_URL": "https://tracker.example.com"}}}}')"
expect_hit "the plugin .mcp.json with a literal value" plugins/launchpad/.mcp.json "$(printf '%s\n' '{"mcpServers": {"launchpad": {"command": "x",' '"env": {"LAUNCHPAD_TOKEN": "odt_abc"}}}}')"
# shellcheck disable=SC2016 # the ${...} placeholders are literal text here
expect_hit "the plugin .mcp.json with a placeholder default" plugins/launchpad/.mcp.json '{"mcpServers": {"l": {"command": "${CLAUDE_PLUGIN_ROOT}/scripts/x", "env": {"T": "${TOKEN:-abc}"}}}}'
# shellcheck disable=SC2016 # the ${...} placeholders are literal text here
expect_hit "the plugin .mcp.json with a literal argument" plugins/launchpad/.mcp.json '{"mcpServers": {"l": {"command": "${CLAUDE_PLUGIN_ROOT}/scripts/x", "args": ["--token", "abc"]}}}'
# shellcheck disable=SC2016 # the ${...} placeholders are literal text here
expect_hit "the plugin .mcp.json with a command outside the plugin" plugins/launchpad/.mcp.json '{"mcpServers": {"l": {"command": "${CLAUDE_PLUGIN_ROOT}/../x"}}}'
expect_hit "the plugin .mcp.json that is not JSON" plugins/launchpad/.mcp.json '{"mcpServers": '

# IP addresses: private, shared, link-local and low-numbered IPv4 addresses
# pass only as the listed examples; global IPv6 only as documentation or the
# listed examples.
expect_hit "a private IPv4 address that is not an example" a/n.txt "host 192.168.7.""7 is the NAS"
expect_hit "a 10/8 address that is not an example" a/n.txt "dial 10.200.3.""4"
expect_hit "a low-numbered public IPv4 address" a/n.txt "dns 4.5.6.""7"
expect_hit "a private IPv4 address before a full stop" a/n.txt "the NAS is 192.168.7.""7."
expect_hit "a private IPv4 address after a dot" a/n.txt "see .192.168.7.""7 there"
expect_hit "the second of two addresses on a line" a/n.txt "10.0.0.1,192.168.7.""7"
expect_hit "a version-like address that is not listed" a/n.txt "image tag 1.27.3.""1"
expect_hit "a unique local IPv6 address that is not listed" a/n.txt "dial fd34"":5678::1 now"
expect_clean "listed example IPv4 addresses" a/n.txt "192.168.1.5 10.0.0.1 1.2.3.4 169.254.169.254 8.8.8.8 192.0.2.10"
expect_hit "a global IPv6 address" a/n.txt "dial 2a01"":4f8:1:2::3 now"
expect_clean "a documentation IPv6 address" a/n.txt "dial 2001:db8::1 and 2606:4700:4700::1111 and fd00::1"

# Identity: a 300 KB message (grep -q stops early: no pipe may turn the hit
# into a pass), a non-repository folder with --identity-only (exit 2), and
# annotated tags (tagger and message).
d="$(mktemp -d "$work/case.XXXXXX")"
printf 'hello\n' >"$d/README.md"
git -C "$d" init -q
git -C "$d" add -A
perl -e 'print "fix for minty", "water.example\n\n", ("filler line of a long message\n" x 10000)' >"$work/long-msg.txt"
GIT_AUTHOR_NAME=pub GIT_AUTHOR_EMAIL="$nr" GIT_COMMITTER_NAME=pub GIT_COMMITTER_EMAIL="$nr" \
	git -C "$d" -c commit.gpgsign=false commit -q -F "$work/long-msg.txt"
rc=0
/bin/bash "$here/scripts/audit-public.sh" "$d" --identity-only >"$work/out.log" 2>&1 || rc=$?
((rc == 1)) || fail "a 300 KB message with the host: exit $rc, not 1"
grep -q " message" "$work/out.log" || fail "a 300 KB message with the host: no message hit"
echo "ok: identity: a 300 KB message is read whole"
d="$(mktemp -d "$work/case.XXXXXX")"
printf 'hello\n' >"$d/README.md"
rc=0
/bin/bash "$here/scripts/audit-public.sh" "$d" --identity-only >"$work/out.log" 2>&1 || rc=$?
((rc == 2)) || fail "--identity-only without a repository: exit $rc, not 2"
echo "ok: identity: --identity-only without a repository is exit 2"
tag_case() {
	local name="$1" tn="$2" te="$3" tmsg="$4" want="$5" rc=0
	d="$(mktemp -d "$work/case.XXXXXX")"
	printf 'hello\n' >"$d/README.md"
	git -C "$d" init -q
	git -C "$d" add -A
	GIT_AUTHOR_NAME=pub GIT_AUTHOR_EMAIL="$nr" GIT_COMMITTER_NAME=pub GIT_COMMITTER_EMAIL="$nr" \
		git -C "$d" -c commit.gpgsign=false commit -q -m one
	GIT_COMMITTER_NAME="$tn" GIT_COMMITTER_EMAIL="$te" git -C "$d" -c tag.gpgsign=false tag -a v1 -m "$tmsg"
	/bin/bash "$here/scripts/audit-public.sh" "$d" --identity-only >"$work/out.log" 2>&1 || rc=$?
	if [[ "$want" == hit ]]; then
		((rc == 1)) || fail "$name: exit $rc, not 1"
		grep -q "tag v1" "$work/out.log" || fail "$name: no tag hit"
	else
		((rc == 0)) || {
			cat "$work/out.log" >&2
			fail "$name: exit $rc, not 0"
		}
	fi
	echo "ok: identity: $name is $want"
}
tag_case "a noreply tagger" pub "$nr" "v1" clean
tag_case "the host in the tagger address" pub "me@minty""water.example" "v1" hit
tag_case "the first name in the tag message" pub "$nr" "release by ch""ris" hit

# gitleaks: the fake push relay secret passes in its one file only (path AND
# value). Needs docker and the pinned image, as audit-public.sh --gitleaks does.
gl_case() {
	local name="$1" file="$2" value="$3" want="$4" d
	d="$(mktemp -d "$work/case.XXXXXX")"
	cp "$here/.gitleaks.toml" "$d/.gitleaks.toml"
	mkdir -p "$d/$(dirname "$file")"
	printf 'package api\n\nconst (\n\tpushRelaySecret = "%s"\n)\n' "$value" >"$d/$file"
	if /bin/bash "$here/scripts/audit-public.sh" "$d" --gitleaks >"$work/out.log" 2>&1; then
		[[ "$want" == clean ]] || fail "gitleaks: $name: no finding"
	else
		[[ "$want" == hit ]] || {
			cat "$work/out.log" >&2
			fail "gitleaks: $name: false finding"
		}
		grep -q "HIT: gitleaks" "$work/out.log" || fail "gitleaks: $name: not a gitleaks hit"
	fi
	echo "ok: gitleaks: $name is $want"
}
if command -v docker >/dev/null 2>&1; then
	relay="APIRELAY""SECRET-8842"
	gl_case "the fake relay secret in its test" server/internal/api/push_integration_test.go "$relay" clean
	gl_case "the fake relay secret in another file" server/internal/other/push_integration_test.go "$relay" hit
	gl_case "another value in the push test" server/internal/api/push_integration_test.go "Zq8vR2mX""k7PwL4tN9sYb3" hit
else
	echo "test-audit: SKIP: the gitleaks cases need docker" >&2
fi
echo "test-audit: PASS"
