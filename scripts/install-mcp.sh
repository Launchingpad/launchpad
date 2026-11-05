#!/bin/sh
# install-mcp.sh - install or update launchpad-mcp from a GitHub release.
#
#   install-mcp.sh [--version vX.Y.Z] [--dir DIR] [--url BASE] [--dry-run]
#
#   --version TAG  install this release (default: the latest release)
#   --dir DIR      where the binary goes (default: $HOME/.local/bin)
#   --url BASE     take the files from BASE instead of GitHub. BASE must serve
#                  launchpad-mcp-<os>-<arch> and SHA256SUMS directly under
#                  it (a mirror, or a private server). https only; plain http
#                  works only to localhost, 127.0.0.1 or [::1]. A redirect may
#                  go to https only.
#   --dry-run      show what would happen; download no binary, change nothing
#
# Run it again to update. The script:
#   - detects your system and downloads launchpad-mcp-<os>-<arch> and
#     SHA256SUMS;
#   - checks the sha256 and refuses to install on a mismatch;
#   - writes the binary to a NEW file and renames it over the old one (it never
#     copies over a binary in place: macOS can kill such a binary), keeps the old
#     one as launchpad-mcp.prev, and clears the macOS quarantine flag;
#   - prints the version before and after;
#   - prints a .mcp.json entry with placeholders.
# It needs curl and sha256sum (or shasum). It never asks for or prints a token
# and never writes .mcp.json.
# The binary is a thin client of your Launchpad server. Use a server that runs
# the same release or a newer one.
#
# Read it before you run it: download it, read it, run it.
set -eu

# The GitHub repository that publishes the releases. The one place to change.
repo='Launchingpad/launchpad'

usage() {
	cat <<'USAGE_END'
install-mcp.sh - install or update launchpad-mcp from a GitHub release.

  install-mcp.sh [--version vX.Y.Z] [--dir DIR] [--url BASE] [--dry-run]

  --version TAG  install this release (default: the latest release)
  --dir DIR      where the binary goes (default: $HOME/.local/bin)
  --url BASE     take the files from BASE instead of GitHub. BASE must serve
                 launchpad-mcp-<os>-<arch> and SHA256SUMS directly under
                 it (a mirror, or a private server). https only; plain http
                 works only to localhost, 127.0.0.1 or [::1]. A redirect may
                 go to https only.
  --dry-run      show what would happen; download no binary, change nothing

Run it again to update. The script:
  - detects your system and downloads launchpad-mcp-<os>-<arch> and
    SHA256SUMS;
  - checks the sha256 and refuses to install on a mismatch;
  - writes the binary to a NEW file and renames it over the old one (it never
    copies over a binary in place: macOS can kill such a binary), keeps the old
    one as launchpad-mcp.prev, and clears the macOS quarantine flag;
  - prints the version before and after;
  - prints a .mcp.json entry with placeholders.
It needs curl and sha256sum (or shasum). It never asks for or prints a token
and never writes .mcp.json.
The binary is a thin client of your Launchpad server. Use a server that runs
the same release or a newer one.

Read it before you run it: download it, read it, run it.
USAGE_END
}

die() {
	printf 'install-mcp: %s\n' "$*" >&2
	exit 1
}

url=
tag=
dir=
dry=0
while [ $# -gt 0 ]; do
	case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	--dir)
		[ $# -ge 2 ] || die "--dir needs a value"
		dir=$2
		shift 2
		;;
	--dir=*)
		dir=${1#--dir=}
		shift
		;;
	--url)
		[ $# -ge 2 ] || die "--url needs a value"
		url=$2
		shift 2
		;;
	--url=*)
		url=${1#--url=}
		shift
		;;
	--version)
		[ $# -ge 2 ] || die "--version needs a value"
		tag=$2
		shift 2
		;;
	--version=*)
		tag=${1#--version=}
		shift
		;;
	--dry-run)
		dry=1
		shift
		;;
	*) die "unknown argument: $1 (try --help)" ;;
	esac
done

case "$tag" in
'') ;;
v[0-9]*) ;;
*) die "--version must look like v1.2.3" ;;
esac
case "$tag" in
*[!A-Za-z0-9._+-]*) die "--version must look like v1.2.3" ;;
esac
if [ -z "$dir" ]; then
	[ -n "${HOME:-}" ] || die "HOME is not set; use --dir"
	dir=$HOME/.local/bin
fi
# A leading ~ is expanded here (the shell does not do it for --dir=~/x).
# shellcheck disable=SC2088
case "$dir" in
"~") dir=${HOME:-} ;;
"~/"*) dir=${HOME:-}/${dir#"~/"} ;;
esac
[ -n "$dir" ] || die "--dir is empty"
# The path goes into a JSON snippet: no quote, backslash or control character.
case "$dir" in
*'"'* | *\\* | *[[:cntrl:]]*) die "--dir must not hold a quote, a backslash or a control character" ;;
esac
# Always absolute, so the snippet works from any folder. Nothing is created here.
case "$dir" in
/*) ;;
*)
	here=$(pwd -P) || die "cannot read the current folder"
	dir=$here/${dir#./}
	;;
esac
dest=$dir/launchpad-mcp

# valid_origin URL: https://host[:port], or http://loopback[:port]. Nothing else
# (no path, query or credentials; the port is digits only; the host does not
# start with "-" or "."), so nothing odd reaches curl.
valid_origin() {
	case "$1" in
	https://*) vo_rest=${1#https://} vo_https=1 ;;
	http://*) vo_rest=${1#http://} vo_https=0 ;;
	*) return 1 ;;
	esac
	case "$vo_rest" in
	"[::1]" | "[::1]:"*)
		vo_host='[::1]'
		vo_port=${vo_rest#"[::1]"}
		;;
	*)
		vo_host=${vo_rest%%:*}
		vo_port=${vo_rest#"$vo_host"}
		;;
	esac
	case "$vo_host" in
	'' | -* | .* | *[!A-Za-z0-9.-]*)
		if [ "$vo_host" != '[::1]' ]; then return 1; fi
		;;
	esac
	if [ "$vo_https" -eq 0 ]; then
		case "$vo_host" in
		localhost | 127.0.0.1 | '[::1]') ;;
		*) return 1 ;;
		esac
	elif [ "$vo_host" = '[::1]' ]; then
		return 1
	fi
	case "$vo_port" in
	'') ;;
	:*)
		vo_digits=${vo_port#:}
		case "$vo_digits" in
		'' | *[!0-9]*) return 1 ;;
		esac
		[ "${#vo_digits}" -le 5 ] || return 1
		;;
	*) return 1 ;;
	esac
	return 0
}

if [ -n "$url" ]; then
	url=${url%/}
	valid_origin "$url" || die "--url must be https://host (http only to localhost); no path or credentials"
	[ -z "$tag" ] || die "--version and --url cannot be used together"
	base=$url
	proto=https
	case "$url" in http://*) proto=http ;; esac
elif [ -n "$tag" ]; then
	base=https://github.com/$repo/releases/download/$tag
	proto=https
else
	base=https://github.com/$repo/releases/latest/download
	proto=https
fi

case "$(uname -s)" in
Darwin) os=darwin ;;
Linux) os=linux ;;
*) die "unsupported system: $(uname -s) (darwin and linux only)" ;;
esac
case "$(uname -m)" in
arm64 | aarch64) arch=arm64 ;;
x86_64 | amd64) arch=amd64 ;;
*) die "unsupported CPU: $(uname -m) (arm64 and amd64 only)" ;;
esac
name=launchpad-mcp-$os-$arch

# fetch URL OUTFILE: curl only. -q ignores ~/.curlrc, -g turns off URL globbing.
# GitHub answers with a redirect to its file store; a redirect may go to https
# only (so an http mirror cannot send you to another plain http address).
command -v curl >/dev/null 2>&1 || die "curl is needed"
fetch() { curl -q -g -fsSL --proto "=$proto" --proto-redir =https --connect-timeout 20 --max-time 300 -o "$2" "$1"; }

# Check for a sha256 tool ONCE, before any download.
if command -v sha256sum >/dev/null 2>&1; then
	sha_of() { sha256sum "$1" | cut -d ' ' -f 1; }
elif command -v shasum >/dev/null 2>&1; then
	sha_of() { shasum -a 256 "$1" | cut -d ' ' -f 1; }
else
	die "sha256sum or shasum is needed to check the download"
fi

version_of() { # FILE: prints the version line or a fixed text
	if [ -x "$1" ] && out=$("$1" --version 2>/dev/null </dev/null) && [ -n "$out" ]; then
		printf '%s\n' "$out" | head -n 1
	else
		printf 'unknown (no --version; an older build)\n'
	fi
}

work=$(mktemp -d "${TMPDIR:-/tmp}/install-mcp.XXXXXX") || die "cannot make a work folder"
newfile=
cleanup() {
	rm -rf "$work"
	if [ -n "$newfile" ]; then rm -f "$newfile"; fi
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

if [ -e "$dest" ]; then
	have=$(version_of "$dest")
else
	have='not installed'
fi

fetch "$base/SHA256SUMS" "$work/SHA256SUMS" || die "cannot download SHA256SUMS from $base"
# One line must name the file, as "hash  name" or "hash *name" (CRLF is fine).
want=$(tr -d '\r' <"$work/SHA256SUMS" | awk -v n="$name" '{ f = $2; sub(/^\*/, "", f); if (f == n) { c++; h = $1 } } END { if (c == 1) print h; else if (c > 1) print "DUPLICATE" }')
[ "$want" != DUPLICATE ] || die "SHA256SUMS names $name more than once"
case "$want" in
'' | *[!0-9a-f]*) die "SHA256SUMS has no valid entry for $name" ;;
esac
[ "${#want}" -eq 64 ] || die "SHA256SUMS has no valid entry for $name"

printf 'before: %s\n' "$have"
if [ "$dry" -eq 1 ]; then
	printf 'dry run: would install %s (sha256 %s) to %s\n' "$name" "$want" "$dest"
	printf 'dry run: nothing was downloaded or changed\n'
	exit 0
fi

if [ -d "$dest.prev" ] && [ ! -L "$dest.prev" ]; then
	die "$dest.prev is a folder; move it away and run again"
fi
current=0
if [ -e "$dest" ] && [ "$(sha_of "$dest")" = "$want" ]; then current=1; fi
if [ "$current" -eq 1 ]; then
	chmod 0755 "$dest" || die "cannot set the mode of $dest"
	printf 'already up to date: %s matches the release\n' "$dest"
else
	mkdir -p "$dir" || die "cannot make $dir"
	dir=$(cd "$dir" && pwd -P) || die "cannot enter $dir"
	dest=$dir/launchpad-mcp
	printf 'downloading %s from %s\n' "$name" "$base"
	# The binary goes next to its final place, so the last step is one rename.
	newfile=$(mktemp "$dir/.launchpad-mcp.XXXXXX") || die "cannot write in $dir"
	fetch "$base/$name" "$newfile" || die "cannot download $name"
	got=$(sha_of "$newfile")
	if [ "$got" != "$want" ]; then
		die "sha256 mismatch for $name: nothing was installed"
	fi
	printf 'sha256 ok\n'
	chmod 0755 "$newfile"
	if [ "$os" = darwin ] && command -v xattr >/dev/null 2>&1; then
		xattr -c "$newfile" >/dev/null 2>&1 || true
	fi
	if [ -e "$dest" ]; then
		# A hard link, not a copy: the old file keeps its own inode as .prev.
		rm -f "$dest.prev"
		ln "$dest" "$dest.prev" 2>/dev/null || cp -p "$dest" "$dest.prev" || die "cannot keep the old binary as $dest.prev"
	fi
	mv -f "$newfile" "$dest"
	newfile=
	printf 'installed: %s\n' "$dest"
fi
printf 'after:  %s\n' "$(version_of "$dest")"
if [ -e "$dest.prev" ]; then printf 'the previous binary is kept as %s\n' "$dest.prev"; fi

printf '\nAdd this to your .mcp.json (the script does not edit it). Replace the example\nhosts with your own. Put your token in the\n'
printf 'environment variable LAUNCHPAD_TOKEN; this script never asks for it.\n\n'
printf '{\n  "mcpServers": {\n    "launchpad": {\n      "command": "%s",\n      "args": [],\n      "env": {\n' "$dest"
printf '        "LAUNCHPAD_URL": "https://launchpad.example.com",\n'
printf '        "LAUNCHPAD_FILES_URL": "https://launchpad-files.example.com",\n'
# shellcheck disable=SC2016
printf '        "LAUNCHPAD_TOKEN": "${LAUNCHPAD_TOKEN}",\n'
printf '        "LAUNCHPAD_ORG": "<your org slug>"\n'
printf '      }\n    }\n  }\n}\n\n'
printf 'Optional: add LAUNCHPAD_TEAM (a default team key) and LAUNCHPAD_DOWNLOAD_DIR\n'
printf '(where download_attachment saves files) to "env".\n'
case ":${PATH:-}:" in
*":$dir:"*) ;;
*) printf 'note: %s is not in your PATH (the entry above uses the full path, so this is fine).\n' "$dir" ;;
esac
