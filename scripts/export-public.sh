#!/usr/bin/env bash
# export-public.sh [--commit REV] --dest DIR [--message TEXT] [--gitleaks]
#                  [--name NAME --email EMAIL]
#
# Builds the public tree of this repository in a NEW, EMPTY folder DIR:
#   1. git archive REV (default HEAD) into DIR (tracked files only);
#   2. removes the paths in scripts/export-ignore.txt;
#   3. runs scripts/audit-public.sh on DIR (exit 1 on any hit; --gitleaks adds
#      a gitleaks scan in docker);
#   4. git init in DIR, ONE commit with the given message. Author and
#      committer are --name / --email (use the GitHub noreply address), else
#      this repository's git config;
#   5. checks that identity with audit-public.sh --identity-only; on a hit it
#      removes the commit (DIR/.git) and exits 1;
#   6. STOPS and says how the tree reaches the public repository.
#
# It NEVER pushes and NEVER adds a remote. Publishing is a separate, manual
# step that you run yourself (docs/releasing.md).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
rev=HEAD
dest=""
msg="Launchpad: first public release"
gitleaks=()
name=""
email=""

die() {
	echo "export-public.sh: $*" >&2
	exit 2
}

while (($#)); do
	case "$1" in
	--commit)
		rev="${2:?--commit needs a value}"
		shift 2
		;;
	--dest)
		dest="${2:?--dest needs a value}"
		shift 2
		;;
	--message)
		msg="${2:?--message needs a value}"
		shift 2
		;;
	--gitleaks)
		gitleaks=(--gitleaks)
		shift
		;;
	--name)
		name="${2:?--name needs a value}"
		shift 2
		;;
	--email)
		email="${2:?--email needs a value}"
		shift 2
		;;
	*) die "unknown argument '$1' (usage: --commit REV --dest DIR [--message TEXT] [--gitleaks] [--name NAME --email EMAIL])" ;;
	esac
done

[[ -n "$dest" ]] || die "--dest DIR is required"
if [[ -e "$dest" ]]; then
	[[ -d "$dest" ]] || die "$dest exists and is not a folder"
	[[ -z "$(ls -A "$dest")" ]] || die "$dest is not empty; give a NEW or empty folder"
fi
mkdir -p "$dest"
dest="$(cd "$dest" && pwd)"
[[ "$dest" != "$here" && "$dest" != "$here"/* ]] || die "the destination must be outside the repository"

git -C "$here" rev-parse --verify --quiet "$rev^{commit}" >/dev/null || die "'$rev' is not a commit in this repository"
[[ -n "$name" ]] || name="$(git -C "$here" config user.name || true)"
[[ -n "$email" ]] || email="$(git -C "$here" config user.email || true)"
[[ -n "$name" && -n "$email" ]] || die "give --name and --email, or set git config user.name and user.email first"

echo "== archive $(git -C "$here" rev-parse --short "$rev") into $dest"
git -C "$here" archive --format=tar "$rev" | tar -x -C "$dest"

echo "== remove the paths in scripts/export-ignore.txt"
while IFS= read -r line || [[ -n "$line" ]]; do
	line="${line%%#*}"
	line="${line#"${line%%[![:space:]]*}"}"
	line="${line%"${line##*[![:space:]]}"}"
	[[ -n "$line" ]] || continue
	case "$line" in
	/* | *..*) die "bad path in export-ignore.txt: $line" ;;
	esac
	rm -rf -- "${dest:?}/$line"
done <"$dest/scripts/export-ignore.txt"

# The identity check below needs the audit script of the exported commit to
# know --identity-only. An older commit does not: say so instead of failing
# later with "unknown argument".
grep -q -- '--identity-only' "$dest/scripts/audit-public.sh" ||
	die "the audit-public.sh of '$rev' has no --identity-only; export a newer commit (nothing was committed)"

echo "== audit"
"$dest/scripts/audit-public.sh" "$dest" ${gitleaks[@]+"${gitleaks[@]}"} || {
	echo "export-public.sh: the audit found hits; fix them in the source and export again. Nothing was committed." >&2
	exit 1
}

echo "== one commit"
git -C "$dest" init -q -b master
git -C "$dest" add -A
GIT_AUTHOR_NAME="$name" GIT_AUTHOR_EMAIL="$email" GIT_COMMITTER_NAME="$name" GIT_COMMITTER_EMAIL="$email" \
	git -C "$dest" -c commit.gpgsign=false commit -q -m "$msg"

echo "== commit identity"
"$dest/scripts/audit-public.sh" "$dest" --identity-only || {
	rm -rf -- "${dest:?}/.git"
	echo "export-public.sh: the commit identity names the author's host or name; give --name and --email (the GitHub noreply address). The commit was removed." >&2
	exit 1
}
echo "exported commit $(git -C "$dest" rev-parse --short HEAD) in $dest ($(git -C "$dest" ls-files | wc -l | tr -d ' ') files)"

cat <<EOT

The export is done. Nothing was pushed and no remote was added.
Check the tree and run the tests in it. Then publish it as ONE pull request
into master of the public repository: copy this commit into a clone of the
public repository, check the tree, push a branch, open the pull request and
squash-merge it. The steps are in docs/releasing.md, "Later releases".
Never push this folder: its commit has no parent.

EOT
