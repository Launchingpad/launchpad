# Cut a release (MCP binary)

A release gives agent machines the MCP binary and its install script. It is
built by `.github/workflows/release.yml` when you push a tag that starts
with `v`.

1. Make sure `develop` (or the branch you release) is green and merged.
2. Pick a version, for example `v1.2.3`. Set the same number (`1.2.3`, no
   `v`) as `version` in `plugins/launchpad/.claude-plugin/plugin.json` and
   merge that first. The Claude Code plugin downloads the release with the tag
   `v<plugin version>`, and Claude Code updates the plugin only when this
   number changes. The workflow stops if the tag and the plugin version
   differ.
3. Make the tag in a clone of the PUBLIC repository, on the `master` commit
   that holds the version bump, and push only that tag:

   `W` is the clone of the public repository (`releasing.md`, "Later
   releases"); `GH_LOGIN` and `GH_NOREPLY` are the noreply identity. The
   block checks the tagger and the identity of every commit and tag BEFORE
   the push, and stops (a line that starts with `STOP`) on any failure:

   ```bash
   (
     set -Eeuo pipefail
     trap 'echo "STOP: line $LINENO failed; nothing after it ran"' ERR
     git -C "$W" switch -q master
     git -C "$W" pull -q --ff-only
     git -C "$W" -c user.name="$GH_LOGIN" -c user.email="$GH_NOREPLY" \
       -c tag.gpgsign=false tag -a v1.2.3 -m "v1.2.3"
     tagger="$(git -C "$W" for-each-ref --format='%(taggername) %(taggeremail)' refs/tags/v1.2.3)"
     [ "$tagger" = "$GH_LOGIN <$GH_NOREPLY>" ] ||
       { echo "STOP: the tagger is not the noreply identity"; exit 1; }
     "$W/scripts/audit-public.sh" "$W" --identity-only >/dev/null ||
       { echo "STOP: the identity audit of the commits and tags failed"; exit 1; }
     git -C "$W" push origin refs/tags/v1.2.3
     echo "OK: tag v1.2.3 pushed"
   )
   ```

   Never make or push the tag from the private repository: the public
   repository has its own history (`releasing.md`), and pushing a private
   commit would upload the private history behind it. Push the tag right
   after the bump is merged: until the release exists, a plugin update
   cannot download its binary.
4. Wait for the `release` workflow. It first checks that the tag matches the
   plugin version and that the tagged commit is on `master` (it fetches the
   full `master`); a tag on any other commit stops the release. Then it
   builds `launchpad-mcp-darwin-arm64`, `-darwin-amd64`, `-linux-amd64` and
   `-linux-arm64`, then `SHA256SUMS`, and publishes them with
   `install-mcp.sh` as a GitHub Release. A second job (`images`) builds the
   two container images for amd64 and arm64 and pushes them to
   `ghcr.io/launchingpad/launchpad-api` and `launchpad-web`, tagged with the
   version (no `v`) and the full commit sha, never `latest`. Self-hosters set
   `LAUNCHPAD_VERSION` to that version in `.env`
   (`docs/internal/decisions-T45-docker-distribution.md`).
5. Check the release page: six files. Check that the two packages show the new
   version tag (and that the package is public: set it once in the GitHub
   package settings, after the first push). Then, on a test machine, run the
   install command from [`mcp.md`](mcp.md) and look at the printed version.

Rules:

- Keep the window short between the merge of a plugin version bump into
  `master` and its release: push the tag right after the merge. In that
  window, a user who updates the plugin gets a binary that cannot be
  downloaded.
- Never move or re-push a tag. Make a new version instead.
- The version in the binary is the tag name. `launchpad-mcp --version`
  prints it.
- Keep your server on the same release as the binary, or newer.
- `SHA256SUMS` is served from the same origin as the binaries. The installer
  and the plugin launcher check it, which guards against corruption,
  truncation, a stale CDN copy and a wrong-architecture file. It does not
  protect against a compromised release or account. Signing (minisign, cosign
  or a build attestation) is a later step.
- The plugin launcher trusts a binary that is already in its data folder and
  does not hash it again at each start.
