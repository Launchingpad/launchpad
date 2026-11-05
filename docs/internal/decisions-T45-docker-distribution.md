# T45: Docker distribution, one compose file (LAUN-246)

Goal: a person downloads `docker-compose.yml` and `.env.example`, sets a few
values and runs `docker compose up -d`. No checkout, no image build, no manual
silo step. This record says what was built and why each rule exists. The guide
for users is `docs/self-hosting.md`, "Run it from a compose file only".

Before LAUN-246 the repository had `deploy/docker-compose.sample.yml` and
`deploy/sample.env.example`: a hand-made copy with local image tags, no
backup, a manual silo step and no CI check. They are deleted. Their rules that
still hold are kept (a fixed project name, no `launchpad` name, same
hardening).

## The files

1. **Three files in the repository root:** `docker-compose.yml`,
   `.env.example` and `docker-compose.override.example.yml`. `scripts/gen-env.sh`
   makes `.env`. `deploy/docker-compose.yml` and `deploy/deploy.sh` are not
   changed. An install that deploy.sh made keeps working with no step.
2. **Two small config files are mounted read-only:**
   `deploy/nginx/default.conf.template` (the same-origin server block) and
   `deploy/backup/backup.sh`. They are the same files deploy.sh uses, so there
   is one copy of each. A user who downloads files gets them at the same
   relative paths. (A third and fourth file is the price of not baking the
   nginx block and the backup loop into an image. Baking them in would change
   the shipped web image and the postgres image choice; that is a later
   change.)
3. **Project name `launchpad-compose`.** Fixed in the file. It must never be
   `launchpad`, the name deploy.sh uses, so that a stack never shares
   networks or volumes with a deploy.sh stack. `-p` and `COMPOSE_PROJECT_NAME`
   still win, so the docs and `check-compose.py` (the rendered `name`) say:
   do not set them to `launchpad`. The old name `launchpad-sample` is not
   used, because the file is no longer a sample.
4. **`import_work` stays a named volume** without the deploy label. It holds
   the web import's transient exports; Docker owns it and `volume-init`
   chowns it. The label is left off on purpose: deploy.sh must never adopt
   this stack's volumes.

## Bind mounts, and the policy change

5. **Data lives in host directories.** `LAUNCHPAD_DATA_DIR` (default `./data`)
   with `LAUNCHPAD_DB_DIR` (`postgres`), `LAUNCHPAD_FILES_DIR` (`files`,
   attachments) and `LAUNCHPAD_BACKUP_DIR` (`backups`). Why: the person can see,
   back up and move the data with ordinary tools, and does not need to know
   where Docker keeps volumes. The override example shows named volumes for
   hosts where bind mounts fail.
6. **The old rule was "no host mounts except the config files".** For the
   root file the rule is now an ALLOWLIST in `deploy/check-compose.py`
   (`--root`). A bind mount is allowed only for the exact pairs in
   `ROOT_DATA_MOUNTS` (service, container path, and the source must equal the
   directory the env gives, writable) and `ROOT_CONFIG_MOUNTS` (the two files
   above, exact source, read-only). Any other host path, a data mount from a
   wrong path, a read-only data mount, the Docker socket (also under an
   allowed target), a named volume other than `import_work` and a port on
   `0.0.0.0` fail. `deploy/test-compose-check.sh` proves each with a mutated
   render. The rule for `deploy/docker-compose.yml` is unchanged.
7. **`volume-init` chowns the host directories** (postgres and backups to uid
   70, files and import_work to uid 10001). It keeps the exception of
   `deploy/docker-compose.yml`: cap_drop ALL, cap_add CHOWN, network_mode
   none, restart "no", and no `no-new-privileges` (measured, see the comment in `deploy/docker-compose.yml`; on
   Linux 7.0 that flag stops the exec). The checker holds all of it. It
   changes the owner of a directory only when the owner is wrong, so a disk
   that refuses `chown` (NFS with root squash) works when the owner is already
   right. The chown is not recursive: postgres' own entrypoint handles its
   directory; silo and the app create files with their own uid.
8. **Caveats for the docs:** Docker Desktop on macOS with a directory on a
   network or exFAT disk (no owners, slow, no fsync guarantee: do not put
   the database there), NFS with root squash (chown refused; set the owners by
   hand), SELinux (add `:z` in an override), and rootless Docker (uids are
   mapped; use named volumes).

## Images

9. **GHCR, no `latest`.** `ghcr.io/launchingpad/launchpad-api:${LAUNCHPAD_VERSION}`
   and `ghcr.io/launchingpad/launchpad-web:${LAUNCHPAD_VERSION}`.
   `LAUNCHPAD_VERSION` has no default (a `:?` form) and the checker rejects
   `latest`. Reason: an update must be a decision in `.env`, not a side effect
   of a pull. The version is the release tag without the `v` (the plugin
   rule). The tag `v1.2.3` gives the image tag `1.2.3`.
10. **The release workflow** (`.github/workflows/release.yml`, job `images`):
    buildx with QEMU for amd64 and arm64; tags are the version and the full
    commit sha, `latest=false`; OCI labels (title, description, licence,
    version, source, revision) from `docker/metadata-action`; job-level
    `permissions: contents: read, packages: write` only (a job block replaces
    the top block, so the images job cannot write the release); actions are
    pinned to full commit shas; it runs the same tag and "commit is on master"
    checks as the `mcp` job. Provenance and SBOM attestations are off, so the
    result is a plain two-platform index.
11. **Future work, not done:** image signing (cosign keyless), SBOM and
    provenance attestations, a vulnerability scan step, pinning the compose
    images by digest in the release notes. Each needs a decision about who
    verifies it. The compose file would then pin by digest.
12. **A `build:` option stays in the file as a comment.** Uncomment `build:
    ./server` and `build: ./web` to build from a checkout. The checker rejects
    a live `build` key in the root file, so a built stack is a local choice
    and not what CI approves.

## No manual steps

13. **`silo-init`** is a one-shot (the silo image, so `mcli` is there) that
    creates the private bucket, the `launchpad-app` policy (list, get, put,
    delete in that bucket only) and the app's user, and attaches the policy.
    It is the port of `silo_bootstrap` in `deploy/deploy.sh` without the
    off-site read-only user. It is idempotent (it runs on every `up`).
    The secrets reach it only as environment variables of that container; the
    script text names variables, so no secret is in an argument list (the
    checker looks for each secret value in every `command` and `entrypoint`).
    It runs on `edge` with a read-only root, `/tmp` tmpfs, cap_drop ALL and
    `no-new-privileges`. `app` depends on it with `service_completed_successfully`.
14. **`scripts/gen-env.sh` runs on the host, not in a container.** A container
    cannot write to the person's directory with the right owner and mode
    without a bind mount, a root user or a docker socket; all three are worse
    than 150 lines of bash. It copies `.env.example`, so there is one template.
    Rules: it refuses to overwrite (it links the finished temp file into place,
    so a race cannot overwrite either, and a symlink is refused); the file is
    mode 0600 from creation; `WEB_HOST`, `FILES_HOST` and `LAUNCHPAD_VERSION`
    have no default (the environment, or a prompt on a terminal); it prints
    names and never a value. Random values: `openssl rand -hex 32`, else
    `/dev/urandom`. `deploy/test-gen-env.sh` tests all of it.

## Backup and the off-site worker

15. **The nightly `backup` service is included**, writing to
    `LAUNCHPAD_BACKUP_DIR`. It is the same service and script as in deploy.
16. **The `offsite` worker is NOT included.** It needs: a restricted database
    role (`deploy/backup/offsite-grants.sql`) that deploy.sh creates after the
    app has migrated, on every run, with a password it sets; a read-only silo
    user for the attachment copy; two more networks (`offsite_db`, `files_net`)
    and `egress`; a scratch volume with an instance guard
    (`offsite resume`); and a start order (grants, then worker). A compose
    profile cannot run the grants after the migration without either mounting
    the SQL (another host file) or embedding it in the file, and a
    wrong grant weakens the one role that is not the owner. This cannot be done
    safely without deploy.sh now, so it is left out. The app still has the
    Backups page; destinations can be saved but nothing sends. The docs say so
    and say to copy `LAUNCHPAD_BACKUP_DIR` and `LAUNCHPAD_FILES_DIR` off the
    host. A later ticket can add an `offsite-init` one-shot and a profile.
    `BACKUP_ENCRYPTION_KEY` is still passed to the app, so a later worker can
    read what the app sealed.

## Tests

17. **`check-compose.py --root`** (called by `check-compose.sh`, rendered with
    `deploy/example-env.sh`, a filled copy of `.env.example` with fake values):
    the services are exactly the seven; ports only on web and silo, on
    `LAUNCHPAD_BIND_ADDR`, never `0.0.0.0`; no socket, privileged, host
    network; the allowlist (item 6); no-new-privileges everywhere but
    `volume-init`; cap_drop ALL (not postgres, as in deploy); non-root users;
    digests for third-party images; GHCR names at the required version; read-only
    roots; networks; the app's fixed `LOG_ENV`, URLs and dependencies.
    The default bind address with no `LAUNCHPAD_BIND_ADDR` is 127.0.0.1, and a
    render without `LAUNCHPAD_VERSION` fails.
18. **The drift test** (`deploy/check-compose-drift.py`, cases in
    `test-compose-check.sh`) compares the services the two files share
    (volume-init, postgres, silo, backup, web, app): image pins, user,
    security options, capabilities, read-only, tmpfs, health checks, logging,
    restart, networks, depends_on, mount targets, published ports, environment
    names and the non-secret values. The `INTENDED` table lists every allowed
    difference with its reason: the app and web images (local build against
    GHCR), the two checksum variables of deploy.sh, `offsite_db` and `files_net`
    (no worker), the `volume-init` and `silo-init` waits. A new difference
    fails until someone adds a line with a reason. This replaces the old
    warning "no CI job checks it".
19. **`deploy/test-compose-stack.sh`** (CI job `compose-stack` in `ci.yml`)
    brings the stack up for real: images built from the commit and tagged
    `ghcr.io/launchingpad/...:0.0.0-ci`, high ports, a temp data directory,
    a scratch project name. It runs `server/scripts/smoke.sh`, an attachment
    round trip through the files port (proof that `silo-init` worked), a
    second `up -d`, a `down` and `up -d` (the user, issue and file are kept),
    a backup into the host folder, and `down -v`. The GitLab pipeline is private
    and has no comparable job (it runs `make compose-check` and `make
    deploy-test`, which now include the new checks), so it is not mirrored.
