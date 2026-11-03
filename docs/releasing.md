# Releasing: the public export

The development history of Launchpad is private. The public repository has
its own history: ONE clean first commit made from a scrubbed tree, then one
squash-merged pull request per later release. This page explains both. The
scripts never push; you do the push by hand.

## What the scripts do

- `scripts/export-public.sh` builds the public tree in a new, empty folder:
  `git archive` of one commit, minus the paths in `scripts/export-ignore.txt`
  (`CLAUDE.local.md`, the private `.gitlab-ci.yml`), then the audit, then
  `git init` and one commit (author and committer: `--name` / `--email`, else
  the git config), then a check of that identity (a hit removes the commit).
  It stops there and points to this page. It never pushes and never adds a
  remote.
- `scripts/audit-public.sh DIR` greps a tree for author hosts, personal
  names, private paths, token shapes, private key blocks, e-mail addresses
  outside example domains, odd IP addresses, ticket ids of the private
  tracker, and files that must not ship. Exit 1 means hits. `--gitleaks`
  adds a gitleaks scan in Docker (`--no-git`, pinned image). The name and
  host checks ignore case. The audit also reads test files for more token
  shapes (Google keys, live payment keys, Slack webhooks) and for credentials
  inside URLs; the allowed fixture values are listed in the script. gitleaks
  reads test files too. `.gitleaks.toml` allows only the known fake values
  that tests plant on purpose (regexes on the secret text, not on paths), so
  a test that plants a new fake secret must add its value there.
- `scripts/test-audit.sh` plants one value per check in a tiny tree and
  proves that the audit fires (and stays quiet on the allowed fixtures).

## First push (an empty public repository only)

These steps made the first public commit. For every later release, use
"Later releases" below.

1. Merge the release-prep work into the branch you export. Commit everything:
   the export reads the commit, not your working folder.
2. Use your GitHub noreply identity for every public commit. The audit
   refuses an identity that names the author's host or name. Set it once in
   your shell (the later steps use these two variables):

   ```bash
   GH_LOGIN="$(gh api user --jq .login)"
   GH_NOREPLY="$(gh api user --jq '"\(.id)+\(.login)@users.noreply.github.com"')"
   ```

3. Export into a new folder outside the repository:

   ```bash
   scripts/export-public.sh --commit develop --dest ~/launchpad-public \
     --message "Launchpad: first public release" --gitleaks \
     --name "$GH_LOGIN" --email "$GH_NOREPLY"
   ```

4. Fix every audit hit in the SOURCE repository (not in the export), commit,
   and export again into a new folder.
5. Test the exported tree as it is:

   ```bash
   cd ~/launchpad-public/server && make docker-test
   cd ../web && npm ci && npm run lint && npm run typecheck && npm test && npm run build
   ```

6. Read the tree once (`git -C ~/launchpad-public show --stat`), then push
   it by hand into the EMPTY public repository (the script prints no push
   command):

   ```bash
   cd ~/launchpad-public
   git remote add github git@github.com:kraetos10/launchpad.git
   git push -u github master
   ```

## Later releases (one pull request into master)

The public `master` already has history, so an export is never pushed as it
is (its commit has no parent). Copy the exact audited commit into a clone of
the public repository and publish it as ONE pull request.

1. Set `GH_LOGIN` and `GH_NOREPLY` (step 2 above). Merge the release work
   into `develop` and export it as in steps 1 to 5 above, into a NEW folder,
   with `--name "$GH_LOGIN" --email "$GH_NOREPLY"`. The audit, gitleaks and
   the identity check must end with no hits. Set `EXPORT` to that folder and
   `W` to a new absolute folder for the clone, for example
   `W="$HOME/launchpad-release-vX.Y.Z"`.

Each block below runs in a subshell with `set -Eeuo pipefail` and an `ERR`
trap: a failed command or check prints a line that starts with `STOP` and
ends the block. Go on only when the block's LAST line is its `OK:` line.

2. In a new clone of the public repository, on a new branch from
   `origin/master`, replace the tracked files with the export commit. The
   block stops if the folder exists, if the branch is not on `origin/master`,
   or if the tree differs from the audited one:

   ```bash
   (
     set -Eeuo pipefail
     trap 'echo "STOP: line $LINENO failed; nothing after it ran"' ERR
     [ ! -e "$W" ] || { echo "STOP: $W exists; use a new folder"; exit 1; }
     git clone -q git@github.com:OWNER/REPO.git "$W"
     git -C "$W" config user.name "$GH_LOGIN"
     git -C "$W" config user.email "$GH_NOREPLY"
     git -C "$W" switch -q -c release/vX.Y.Z origin/master
     git -C "$W" merge-base --is-ancestor origin/master HEAD ||
       { echo "STOP: the branch is not on origin/master"; exit 1; }
     git -C "$W" ls-files -z | (cd "$W" && xargs -0 rm -f)
     git -C "$EXPORT" archive --format=tar HEAD | tar -x -C "$W"
     git -C "$W" add -A
     want="$(git -C "$EXPORT" rev-parse 'HEAD^{tree}')"
     [ "$(git -C "$W" write-tree)" = "$want" ] || { echo "STOP: the tree differs"; exit 1; }
     stat="$(git -C "$W" diff --cached --stat)"; echo "${stat##*$'\n'}"
     git -C "$W" commit -q -m "Launchpad vX.Y.Z"
     echo "OK: branch release/vX.Y.Z ready in $W"
   )
   ```

3. Check the identity of what you will push. The block stops unless there is
   exactly one new commit and its author and committer are the noreply
   identity. Every public commit since the v0.2.0 history rewrite has the
   noreply identity, so the audit's identity check of the whole clone must
   pass too.

   ```bash
   (
     set -Eeuo pipefail
     trap 'echo "STOP: line $LINENO failed; nothing after it ran"' ERR
     git -C "$W" fetch -q origin
     git -C "$W" merge-base --is-ancestor origin/master HEAD ||
       { echo "STOP: master moved; start again from step 2"; exit 1; }
     [ "$(git -C "$W" rev-list --count origin/master..HEAD)" = 1 ] ||
       { echo "STOP: not one new commit"; exit 1; }
     ids="$(git -C "$W" log origin/master..HEAD --format='%ae%n%ce')"
     ids="$(sort -u <<<"$ids")"
     [ "$ids" = "$GH_NOREPLY" ] || { echo "STOP: not the noreply identity"; exit 1; }
     "$EXPORT/scripts/audit-public.sh" "$W" --identity-only >/dev/null ||
       { echo "STOP: the identity audit of the clone failed"; exit 1; }
     echo "OK: identity"
   )
   ```

4. Push the branch, open the pull request, wait for the four checks, then
   squash-merge with the noreply author, pinned to the head you checked:

   ```bash
   (
     set -Eeuo pipefail
     trap 'echo "STOP: line $LINENO failed; nothing after it ran"' ERR
     head="$(git -C "$W" rev-parse HEAD)"
     git -C "$W" push -q origin release/vX.Y.Z
     gh pr create --repo OWNER/REPO --base master --head release/vX.Y.Z \
       --title "Launchpad vX.Y.Z" --body-file /absolute/path/to/release-notes.md
     n=0
     for i in $(seq 1 60); do
       n="$(gh pr view release/vX.Y.Z --repo OWNER/REPO --json statusCheckRollup \
         --jq '[.statusCheckRollup[] | select(.name == "server" or .name == "deploy" or .name == "build" or .name == "web") | .name] | unique | length')"
       [ "$n" -ge 4 ] && break
       sleep 10
     done
     [ "$n" -ge 4 ] || { echo "STOP: the four checks did not appear"; exit 1; }
     gh pr checks release/vX.Y.Z --repo OWNER/REPO --watch --fail-fast >/dev/null ||
       { echo "STOP: a check failed"; exit 1; }
     gh pr merge release/vX.Y.Z --repo OWNER/REPO --squash --delete-branch \
       --author-email "$GH_NOREPLY" --match-head-commit "$head"
     echo "OK: merged"
   )
   ```

   Then tag the release from this clone with the noreply identity
   (`releasing-mcp.md`).

## After the first push (manual, on GitHub)

The default branch of the public repository is `master`, and it takes
squash merges only. Do these steps once, by hand, after the first push.
The scripts never do them.

1. Settings, Code security: enable **Private vulnerability reporting**.
   `SECURITY.md` and `CODE_OF_CONDUCT.md` point reporters to it.
2. Allow squash merges only (replace `OWNER/REPO`, for example
   `kraetos10/launchpad`):

   ```bash
   gh api -X PATCH repos/OWNER/REPO \
     -F allow_squash_merge=true -F allow_merge_commit=false \
     -F allow_rebase_merge=false -F delete_branch_on_merge=true
   ```

3. Suggested protection for `master` (Settings, Branches): require a pull
   request before merging, require the CI checks (`server`, `deploy`,
   `build`, `web`) to pass, block force pushes and deletion.

## Rules

- Never push from a script. Never add the `github` remote to the private
  repository.
- Never export from a dirty or unreviewed branch.
- Changes reach the public repository as pull requests into `master`, each
  squash-merged (one commit per change).
- After the first push, the public repository and the private one are two
  histories. A change moves from private to public as a new export, copied
  into a pull request ("Later releases"); never push a private commit or tag
  to the public repository.

## Release checklist

The list of what to check before each public export is in
`docs/internal/open-source-todo.md`.
