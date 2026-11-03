#!/usr/bin/env bash
# test-export.sh: runs scripts/export-public.sh on a tiny throwaway repository,
# with /bin/bash (3.2 on a Mac, so an empty array under `set -u` would fail).
# Checks: the default run (no --gitleaks) works; the branch is master; there is
# ONE commit and NO remote; the ignored paths are gone; the hint points to the
# pull request flow; --name / --email set the identity and a bad identity
# removes the commit; a planted hit stops the export before any commit; the
# script text never runs a push.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/export-test.XXXXXX")"
trap 'rm -rf "$work"' EXIT
fail() {
	echo "test-export: FAIL: $*" >&2
	exit 1
}

src="$work/src"
mkdir -p "$src/scripts"
cp "$here/scripts/export-public.sh" "$here/scripts/audit-public.sh" "$here/scripts/export-ignore.txt" "$src/scripts/"
printf 'hello\n' >"$src/README.md"
printf 'private\n' >"$src/CLAUDE.local.md"
printf 'ci\n' >"$src/.gitlab-ci.yml"
git -C "$src" init -q
git -C "$src" config user.name Tester
git -C "$src" config user.email tester@example.com
git -C "$src" add -A
git -C "$src" -c commit.gpgsign=false commit -q -m one

echo "-- default run under /bin/bash"
/bin/bash "$src/scripts/export-public.sh" --dest "$work/out" --message "first" >"$work/out.log" 2>&1 || {
	cat "$work/out.log"
	fail "the default export failed"
}
[[ "$(git -C "$work/out" rev-parse --abbrev-ref HEAD)" == master ]] || fail "branch is not master"
[[ "$(git -C "$work/out" rev-list --count HEAD)" == 1 ]] || fail "not exactly one commit"
[[ -z "$(git -C "$work/out" remote)" ]] || fail "a remote was added"
[[ ! -e "$work/out/CLAUDE.local.md" && ! -e "$work/out/.gitlab-ci.yml" ]] || fail "an ignored path shipped"
grep -q 'docs/releasing.md, "Later releases"' "$work/out.log" || fail "the hint does not point to the pull request flow"
if grep -q 'git push\|remote add' "$work/out.log"; then fail "the hint prints a push or remote command"; fi
[[ "$(git -C "$work/out" log -1 --format='%an <%ae> %cn <%ce>')" == "Tester <tester@example.com> Tester <tester@example.com>" ]] || fail "the default identity is not the git config"

echo "-- --name and --email set the author and the committer"
/bin/bash "$src/scripts/export-public.sh" --dest "$work/outn" --name pub --email 1+pub@users.noreply.github.com >"$work/outn.log" 2>&1 || {
	cat "$work/outn.log"
	fail "the export with --name and --email failed"
}
[[ "$(git -C "$work/outn" log -1 --format='%an <%ae> %cn <%ce>')" == "pub <1+pub@users.noreply.github.com> pub <1+pub@users.noreply.github.com>" ]] || fail "--name / --email not used"

echo "-- an identity that names the author's host removes the commit"
rc=0
# built from two parts so this file itself is not an audit hit
/bin/bash "$src/scripts/export-public.sh" --dest "$work/outi" --name pub --email "me@minty""water.example" >"$work/outi.log" 2>&1 || rc=$?
((rc == 1)) || fail "a bad identity gave exit $rc, not 1"
[[ ! -d "$work/outi/.git" ]] || fail "the commit with a bad identity was kept"

echo "-- a planted hit stops the export"
# built from two parts so this file itself is not an audit hit
printf 'see the host %s.example\n' "minty""water" >"$src/notes.txt"
git -C "$src" add -A
git -C "$src" -c commit.gpgsign=false commit -q -m two
rc=0
/bin/bash "$src/scripts/export-public.sh" --dest "$work/out2" >"$work/out2.log" 2>&1 || rc=$?
((rc == 1)) || fail "a planted hit gave exit $rc, not 1"
[[ ! -d "$work/out2/.git" ]] || fail "a commit was made after a hit"

echo "-- an exported commit whose audit has no --identity-only stops with a clear message"
git -C "$src" rm -q notes.txt
sed 's/identity-only/identity-gone/g' "$src/scripts/audit-public.sh" >"$work/old-audit.sh"
cp "$work/old-audit.sh" "$src/scripts/audit-public.sh"
git -C "$src" add -A
git -C "$src" -c commit.gpgsign=false commit -q -m three
rc=0
/bin/bash "$src/scripts/export-public.sh" --dest "$work/out3" >"$work/out3.log" 2>&1 || rc=$?
((rc == 2)) || fail "an old audit script gave exit $rc, not 2"
grep -q 'has no --identity-only' "$work/out3.log" || fail "no clear message for an old audit script"
[[ ! -d "$work/out3/.git" ]] || fail "a commit was made with an old audit script"

echo "-- the script never runs a push or adds a remote"
if grep -nE '^[[:space:]]*(git|command git)[^#]* (push|remote add)' "$here/scripts/export-public.sh" | grep -v '^[0-9]*:  '; then
	fail "export-public.sh runs push or remote add"
fi
echo "test-export: PASS"
