# T16 decisions (LAUN-28): scheduled backups to remote storage

LAUN-28 is the design for backups that leave the host. It adds no code.
It settles the targets, where the work runs, where the secrets live, the
encryption, the schedule, the restore path and the tickets that build it.
Each numbered call has its reason, so a later ticket can change it on
purpose. Numbers below are this note's own decisions (`#N`); the tickets are
numbered separately ("ticket N") in the last section.

The request (2026-10-26) was for scheduled backup options to Dropbox, SMTP, FTP,
Nextcloud and similar services. The open points at the end were not yet answered when
this note was written. This note proposes a default for each; a ticket that depends on
one waits for the answer.

The ticket text names `docs/decisions-T14-backups.md`. This file is
`docs/internal/decisions-T16-backups.md`: T14 is the manual-order note, T15
is LAUN-49's activity note (`decisions-T15-activity.md`, already on develop), and design notes live in
`docs/internal/`.

## What exists today (checked in the code and `docs/deploy.md`)

| Part | State | Where |
|---|---|---|
| Nightly dump | The `backup` service runs `pg_dump --format=custom`, reads the whole archive with `pg_restore -f /dev/null`, renames it to `devops-<UTC time>.dump` (mode 0600) in volume `<project>_backups`, and prunes by `BACKUP_KEEP_DAYS` (default 30). It connects as the `devops` database user (`PGUSER`), the same user as the app. | `deploy/backup/backup.sh`, `deploy/docker-compose.yml` |
| Its sandbox | The postgres image, uid 70, read-only root, `cap_drop: [ALL]`, `no-new-privileges`, only on network `backend`, which is `internal: true` (no route out). | `deploy/docker-compose.yml` |
| Networks today | `postgres` and `backup`: `backend` only. `app`: `backend` and `edge`. `silo` and `web`: `edge` only. `edge` is a normal bridge with a route out. | `deploy/docker-compose.yml` |
| Run now | `deploy.sh --backup-now`; `--update` makes a dump first. | `docs/deploy.md` "Backup and restore" |
| Restore check | `deploy/restore.sh latest` or a host file path restores into a scratch database and counts rows. For a host file it reads the file on the host and pipes it into `docker exec -i ... pg_restore` in the `backup` container. | `deploy/restore.sh` |
| Off the host | Manual only: a `docker run` copy of the dumps and of the silo volume (runbook). | `docs/deploy.md` |
| Attachments | Not in the dump. They are in the `<project>_silo_data` volume. | `docs/deploy.md` |
| Sealed secrets | `SETTINGS_ENCRYPTION_KEY` (64 hex) seals secrets with AES-256-GCM (HKDF-derived key, additional data names the column). `platform.Secret` prints `[redacted]`. Write-only on the API. Unreadable secrets never stop the boot. | `decisions-T9-mail-settings.md` #5 to #12 |
| Platform settings | One row, routes behind `auth.RequirePlatformAdmin`, `idempotency.Reject` on writes that carry secrets, no event emitted. | `decisions-T9-mail-settings.md` #1, #12, #13 |
| Mail | `login.MailSwitch` sends plain text through the provider a platform admin chose. It lives in the app process. | `decisions-T9-mail-settings.md` #18 |
| S3 client | `internal/storage` signs SigV4 on the standard library. It is built for one configured bucket and path-style URLs. It has `Put`, `Head`, `Delete`; no list, no multipart. | `server/internal/storage/s3.go` |
| Job pattern | `RunCycleJob`: at boot, then on a ticker, advisory lock, one failed run is logged and the loop goes on. | `server/internal/tracker/cycles.go` |
| Image | The server image installs `ca-certificates` and `tzdata`. | `server/Dockerfile` |
| Dependencies | `go.mod` has no SSH, SFTP, FTP, age or Dropbox library. `golang.org/x/crypto` v0.36.0 is an indirect dependency. | `server/go.mod` |

Two facts shape the design. The `backup` service has no route out and must
keep none. And the secrets that open a destination are set in the web app,
so they cannot live in the `backup` service's environment.

## Goal and scope

A platform admin sets up scheduled backups in Settings, Platform,
Backups: where to send them, when, how many to keep, and sees the result
of each run. The local volume stays the first copy and the default.

In scope: the database dump, sent encrypted. Attachments are designed here
(#38) and built after the database path works. A web restore is out of
scope: it is dangerous, and it needs a key that the server must not hold
(#19).

## Targets

1. **First: S3-compatible and WebDAV (including Nextcloud).** Both are
   plain HTTPS. They need no new network library. The repo has a SigV4
   signer, and Go's `net/http` does WebDAV (`PUT`, `MKCOL`, `PROPFIND`,
   `DELETE`). They cover AWS S3, MinIO, Backblaze B2's S3 API, Wasabi, silo
   on another host, Nextcloud, ownCloud and any WebDAV server.
2. **Second: SFTP.** The standard way to a plain Linux server or a NAS. It
   needs `golang.org/x/crypto/ssh` (already in the module graph, indirect)
   and an SFTP client, `github.com/pkg/sftp`. Auth by private key or by
   password. The host key is pinned (#36).
3. **Third: Dropbox.** HTTPS and OAuth 2 only, so no library. It is third
   because the admin must make a Dropbox app first and the code exchange needs
   its own flow (#9).
4. **Cut: plain FTP.** It sends the password in clear. The dump is
   encrypted (#17), but a password for a backup folder is still worth
   protecting. There is no plain FTP option, now or later.
5. **Cut for now: FTPS (FTP over TLS).** It needs a library
   (`github.com/jlaffaye/ftp` is the usual one). Its data connection must
   resume the control connection's TLS session on many servers, and Go's
   `crypto/tls` has had gaps there; this is not measured here. SFTP covers
   the same need for most hosts. FTPS gets a ticket only if someone
   needs it (ticket 13 is marked "not proposed now").
6. **Cut for now: Google Drive and OneDrive.** Each needs an OAuth app that
   the operator registers and, for broad scopes, a review by the provider.
   The gain over Dropbox, WebDAV and S3 is small. They fit the driver
   interface (#16) if wanted later.
7. **SMTP is NOT a backup destination.** A dump holds every ticket and every
   user email. Mail has size limits (10 to 25 MB is common; not measured for
   the user's relay), and mail is stored in clear in many hops. SMTP stays
   for the notices of a run (#41), using the platform mail settings (T9).
   The request said "smtp"; if SFTP was meant, SFTP is target 2 (open point 1).
8. **Backblaze B2 is a user-chosen target only.** The project owner decided on
   2026-10-17 not to move the product's object storage to B2. Nothing here
   changes that: attachments stay in silo (#38). An admin may point an S3
   destination at B2, or at any other S3 service, as a place to put a copy.
   The UI and the docs name no provider as a recommendation.
9. **Dropbox auth: an app with "App folder" access and a refresh token,
   exchanged by `offsite`.** The admin makes a Dropbox app with the App folder
   permission (it sees only its own folder), runs the authorization in a
   browser, and pastes the app key, the app secret and the authorization code
   into the form. The app (web server) never dials a destination (#10). The app key and
   the app secret are saved in the destination's sealed secrets, because every
   later token refresh needs them. The code is sealed with them into a run row
   (trigger `exchange`, route `POST .../destinations/{id}/exchange`, #43) and
   queued. `offsite` opens it, exchanges the code once at Dropbox, and writes the
   resulting refresh token, sealed, into the destination's secrets (a column
   grant, #15), merging under a row lock (#15). The destination is saved `disabled` and becomes enableable
   when the exchange succeeded. The code is single use, so "test unsaved
   config" is NOT offered for Dropbox: the exchange is the first test (it
   also lists the App folder). An expired or used code makes the `exchange` run `failed` (`auth_failed`) and the destination stays disabled; a failed exchange needs a new code, sent to the same
   `exchange` route. Access
   tokens live in memory. Large files use Dropbox's upload session API (one
   upload call is limited to 150 MB; limits are from Dropbox's documentation,
   not measured here). There is no redirect URL and no callback route.

## The uploader: where it runs

10. **A new `offsite` service, running the server binary in a new mode
   (`server offsite`). All traffic to a user-entered host leaves from it.**
   - *The `backup` container.* Rejected. It is on an internal network with no
     route out and its image is the postgres image (no Go). A route out plus
     credentials for any host an admin types would turn the job that holds
     the whole database into the job that talks to the internet.
   - *The app container.* Rejected. It is the biggest attack surface, does not
     mount the backups volume and has no `pg_dump`. It also should not dial
     user-entered hosts (#34).
   - *A new service.* Chosen. Same image as the app (`launchpad-api:<version>`),
     command `offsite`. It parses its own small config (it needs no `AUTH_OTP_PEPPER`
     and no `S3_*`, #46). It mounts the backups volume read-only and a scratch
     volume read-write (#18). No Docker socket, no published port.
11. **What each process holds.** (The scratch volume mount is `/offsite-work`, not `/work`: superseded by Build notes ticket 5 #1.)

   | Process | Postgres | `SETTINGS_ENCRYPTION_KEY` (mail) | `BACKUP_ENCRYPTION_KEY` | Backups volume | Route out |
   |---|---|---|---|---|---|
   | `backup` (shell) | owner role (`devops`) | no | no | read-write | none |
   | `app` | `devops` role | yes | yes (to seal, and to merge secrets on a `PATCH`) | no | mail only, as today |
   | `offsite` | its own restricted role (#15) | NO | yes (to open a destination's secrets) | read-only | yes, `egress` only |

   `offsite` never holds the mail key, and its database role has no right on
   `platform_settings`, so it cannot open the Postmark or SMTP secrets. The
   app holds both keys, because it seals the backup secrets and merges
   partial updates; it never dials a destination. The notice mails are sent by
   the app (#41).
12. **Two master keys, not one.** `BACKUP_ENCRYPTION_KEY` (64 hex, the same
   format and sealing code as T9 #5 to #7, other HKDF info strings, its own
   `key_check` in `backup_settings`, written only by the app when it first seals a
   destination secret; `offsite` never writes it) seals the destination secrets, the
   signing key (#22) and the unsaved-test secrets (#36). `deploy.sh`
   generates it like `SETTINGS_ENCRYPTION_KEY` (`rand_hex 32`, appended on a
   re-run when missing; `deploy-test` cases like 5b and 5c) and the compose
   file passes it to `app` and to `offsite`. **It is optional at boot until
   backups are configured** (T9 #8 style): an install without it starts, and a
   write that stores a secret without it answers `409 conflict`, and an action that needs it
   answers `503 backup_unavailable` (ticket 2 states the one rule). **Recovery from a lost or changed key.** The likeliest cause is a
   restored `deploy/.env` that lacks the key, so `ensure_env` / `env_topup` silently generated a NEW
   one. The operator then sees: an ERROR line at boot, every destination and the signing key
   `secretsUnreadable`, the page saying the key does not match, and runs failing
   `secrets_unreadable`. Per destination, `secretsUnreadable` means that destination's blob fails its
   GCM check; the global flag `keyMismatch` means the stored non-NULL `key_check` differs from the
   current key. The app replaces a mismatching non-NULL `key_check` on the FIRST successful seal
   under the current key (an admin re-entering a secret), in that transaction; other destinations
   stay unreadable until their secrets are entered again. Order: re-enter the secrets, run
   `deploy.sh --backup-rotate-signing` (the old signing key cannot be opened; the trigger allows the
   replacement with its change-log row, #22), then enable the destinations. The T9 #7 rule applies: the server and
   `offsite` refuse a public dev key and an all-zero `BACKUP_ENCRYPTION_KEY` unless
   `LOG_ENV` is `local` or `test`, and a malformed key fails the boot. Why a second key: if `offsite`
   held the mail key it could open the Postmark or SMTP secrets, and a bug in a
   driver (it parses a hostile server's replies, with a route out) would reach
   them. With a separate key and a restricted role it cannot. A lost or changed
   key behaves as T9 #10: an ERROR line, `secretsUnreadable`, runs fail with
   `secrets_unreadable`, the boot goes on, and the fix is to enter the secrets
   again (and, for the signing key, #22).
13. **Networks: `offsite` can reach postgres and the internet, and nothing
   else of ours.** `app` is on `backend` and `edge`, so a service on `backend`
   would resolve `app` and `backup` too; `offsite` must not join `backend`.
   - A NEW network `offsite_db` (`internal: true`) that only `postgres` and
     `offsite` join. `postgres` joins `backend` and `offsite_db`.
   - A NEW network `egress` (a normal bridge with a route out) that only
     `offsite` joins.
   - `offsite` joins `offsite_db` and `egress` only. It does not join
     `backend` or `edge`, so `app`, `web`, `silo` and `backup` do not
     resolve and are not routed. `postgres` is reachable (it must be).
   - Not closed by this layout: the published ports. With `DEPLOY_BIND_ADDR`
     set to a LAN address, nginx (4001) and silo (4002) are reachable from a
     container through the host's gateway or the LAN address. They are
     private addresses, so the SSRF guard refuses them by default (#34);
     they open only if the operator lists their range (#35). The
     runbook says so and says to list a narrow range, not `192.168.0.0/16`.
   `make compose-check` gets rules for the service (#46). Whether
   compose stops `offsite` reaching the host gateway is not measured on Linux;
   the guard is the control for that.
14. **User and files.** Dumps are mode 0600, owned by uid 70. `offsite`
   needs to read them, so the compose file runs it as `user: "70:70"` with a
   read-only root, `cap_drop: [ALL]` and `no-new-privileges`. That the server
   binary runs as uid 70 in the app image (world-executable, no file owned
   by 10001 needed) is NOT measured. Ticket 5 measures it.
15. **A restricted database role for `offsite` is in v1** (ticket 5). It is
   created by a deploy step (`deploy.sh` runs `psql` in the postgres container
   and keeps the password in `deploy/.env`), not by a migration: a migration
   cannot read `deploy/.env`. Grants:
   - `CONNECT` on the database and `USAGE` on schema `public`.
   - `SELECT` on `backup_settings`, `backup_destinations`, `backup_dumps`,
     `backup_runs`, `backup_requests`, `schema_migrations`.
   - `INSERT`, `UPDATE` on `backup_runs`.
   - `INSERT` on `backup_requests` (a scheduled run that finds the newest dump
     too old, #28) and on `backup_change_log` (the signing key replacement,
     #22).
   - `UPDATE (secrets_enc, secrets_set)` on `backup_destinations` (the Dropbox
     exchange, #9); no other column.
   - `UPDATE (signing_public, signing_private_enc)` on `backup_settings`
     (first-use signing key, #22); no other column (`key_check` is the app's). A
     trigger limits it (see #22).
   - No right on any other table. `LISTEN` needs no grant. `offsite` deletes no
     row anywhere: the row cleanup belongs to the app (#29).
   - `offsite` reads and merges `secrets_enc` with `SELECT ... FOR UPDATE` (a
     Dropbox refresh and an app `PATCH` both read, merge and seal under the row
     lock); the column `UPDATE` grant is enough for that.
   **Order.** The grants name tables that a migration creates, and the app
   migrates on boot. Start order, one rule used in #15, #46, #48 and ticket 5: `deploy.sh` starts everything except `offsite` with `up -d`; after the app is healthy and migrated it creates the role and applies the grants, then starts `offsite` and waits for its health check. `offsite` also, at boot and on every scan, waits until the role can connect and
   `schema_migrations` shows the migration of ticket 1 (#48).
   **Re-applied every time.** A restore uses `pg_restore --no-owner
   --no-privileges` (`restore.sh`), so a restored database has no grants. The
   deploy step is therefore idempotent and runs on EVERY `deploy.sh` run and
   `--update` (create the role if missing, set its password from `deploy/.env`,
   grant). The runbook says to run `deploy.sh` after a restore swap, and a test
   proves a dropped grant comes back (ticket 5). The role password is generated
   with the role (here), not with the master key (#12).
   What the role buys: `offsite` already reads every dump from the volume, so
   it can read all data in the database anyway (a dump holds it all). The role
   does NOT protect confidentiality. It protects integrity (a bug in `offsite`
   cannot write issues or users) and the mail secrets (it has no right on
   `platform_settings`).
16. **One driver interface** in `internal/backup/driver`: `Probe`,
   `Test`, `Put(name, size, reader)`, `List(prefix)`, `Get(name)`,
   `Delete(name)`, plus `Validate(config)` (checks the typed JSON config of that type) and
   `Identity(config)` (returns the canonical `(type, host, bucket, folder)` that the unique rule of #25 compares).
   Ticket 3 owns both. Drivers are listed in a registry (`Register(type, driver)`, which PANICS on a duplicate type); under the
   `backuptest` build tag `RegisterForTest(type, driver)` replaces the registered driver for one test and
   restores it in `t.Cleanup`, so a test may use a fake driver under a REAL type name (`s3`, `webdav`, `sftp`, `dropbox`);
   a driver's own tests call the driver directly, never through the registry, so they never exercise a fake,
   which is how ticket 2's acceptance runs before ticket 6 exists. The local-directory driver registers the
   same way, under a real type name in tests only: it is not a valid `type` in the #23 CHECK. The runner
   knows no protocol. Every reply body is read through a size cap: list and `PROPFIND`
   bodies at most 16 MiB (a guess), and a `verify` or `fetch` download at most the
   declared object size plus a small slack. Errors are typed and safe (#32). A local-directory
   driver exists for tests only, behind a build tag (`backuptest`), so it is
   not in the production binary.

## Encryption and authenticity

17. **Every remote copy is encrypted before it leaves the host, with age.**
   The format is `age` (X25519 recipients, ChaCha20-Poly1305 in 64 KiB
   chunks). The uploader encrypts to the admin's public keys. Plain uploads
   are not an option: a dump holds every ticket and user email.
   - *Why public-key.* A passphrase scheme needs the passphrase on the host at
     every run, so a stolen host could decrypt the remote copies. With a public
     key the host can only encrypt.
   - *Why age.* A small, audited, documented format with a command-line tool
     (`age`) that anyone can use to decrypt without Launchpad.
   - *Library.* `filippo.io/age` is a NEW dependency (it uses
     `golang.org/x/crypto`). Running the `age` binary from the image instead
     adds a binary and a process to manage. The library is chosen. Whether
     `go mod tidy` moves `golang.org/x/crypto` past v0.36.0 is not measured;
     ticket 3 reports the diff.
   - *Recipient types.* X25519 only (`age1...`, parsed with the library's
     X25519 recipient parser). No scrypt, no SSH-key and no plugin recipients:
     a scrypt recipient needs a passphrase on the host, and the others add
     parsers and trust questions. The page refuses any other type.
18. **Scratch space.** `offsite` encrypts the dump into a file on its
   own scratch volume `offsite_work`, computing the sha256 of the encrypted
   bytes in the same pass, then uploads that file. The file exists for two
   reasons: the S3 and WebDAV drivers need the exact size before the first
   byte, and an upload retry must send the SAME bytes (an age file cannot be
   produced twice identically: each encryption uses a new random file key). The
   volume is created like the other volumes (same label block, owned by uid 70
   by `volume-init`) and needs free space of about one dump. Two runs may be active at once (#40), so the volume must hold two files. Each
   run checks free space before it starts: free space must be at least its dump size
   plus the dump sizes of the other `running` runs (read from their rows), else it
   fails `no_space`. A failed run makes a best-effort delete of its own partial
   remote object (the name it chose). The local backups volume filling up is seen
   only through the "no `ok` dump in 36 hours" banner (#41); nothing else watches
   it. A leftover test file `launchpad-<install_id>-test-*.tmp` from a crash is
   accepted (tiny; the retention regex does not match it). It deletes the file
   at the end of every run and at boot. The volume size on a real install is
   not measured.
19. **Keys: the private key never touches the server.**
   - A platform admin makes an age key pair OFF the server and pastes the public
     key (`age1...`) into the Backups page. Up to 5 LIVE public keys are allowed (active or pending adds; removed and cancelled
     elements do not count) (a
     second is a recovery key held by another person).
   - `deploy.sh --backup-keygen` (ticket 5) runs `server offsite keygen` in a
     throwaway container with no network. It makes the AGE pair only: it prints
     the private key and the public key to the terminal once, and refuses a pipe
     or a file (the rule of `--mint-token`). `age-keygen` gives the same result.
     The signing key is not made here (#22).
   - The page shows each public key as text (it is not secret), a short
     fingerprint (the first 12 hex characters of the sha256 of the `age1...`
     string), the date added and its state (#20).
   - **A lost private key means the remote copies cannot be read, by anyone.**
     The page and the runbook say so before the first destination is enabled.
     The local volume copy is not encrypted by this feature and stays usable.
   - **Rotation:** add the new public key, then remove the old. New backups use
     the new key; old remote copies need the OLD private key until retention
     removes them. Nothing re-encrypts old copies.
   - A "generate in the browser" button is possible later (an age library for
     JavaScript); it is not in v1 and not measured.
20. **Recipient changes take effect after a 24 hour hold.** A stolen platform
   admin session could add an age key it holds and, with later backups readable
   by it, remove the admin's own key. A rule that only protects the last
   recipient does not stop that (add the attacker's key, then remove the
   admin's). The control is a delay with a notice:
   - **An add is immediate only when the install never had a recipient.** That is
     a stored fact, `backup_settings.ever_had_recipient` (boolean, default false, set
     true by the first add and never reset), so emptying the set later does not
     bring the immediate case back. After that, every add is `pending` for 24 hours (`effectiveAt` = now + 24 hours), whether or
     not a destination is enabled: a disabled destination must not be a way
     around the hold. `offsite` encrypts to active recipients only.
   - A recipient that is REMOVED stays active until `removeAt` (now + 24 hours;
     the record keeps `removeBy`, the admin who scheduled it). `DELETE
     .../recipients/{fingerprint}` on a PENDING add is a cancel (the add never
     becomes active); on an active recipient it schedules a removal.
     Every add and remove writes a change-log row and mails all active platform
     admins at once (#27).
   - **Who may cancel.** A pending ADD can be cancelled by any platform admin,
     the one who made it included. A pending REMOVAL can be cancelled only by a
     DIFFERENT platform admin account than the one that scheduled it: otherwise a stolen
     session could cancel the real admin's removal of the attacker's key. The same rule
     means the owner of a stolen session cannot cancel, in the web, a removal that the
     thief scheduled with their account: another admin or the host command is needed. (On an
     install with one platform admin a pending removal cannot be cancelled in the
     web app. That admin ends a stolen-session removal with the host command
     `deploy.sh --backup-recipient-cancel <fingerprint>` (below); without it the
     removal lands after 24 hours and the admin must add the key again, which is
     held another 24 hours.)
   - While a destination is enabled the set of recipients active after all
     pending changes must not be empty (`409`).
   - **Emergency removal on the host.** `deploy.sh --backup-recipient-remove
     <fingerprint>` (terminal only, ticket 5) runs `server backup-recipient-remove`
     in the app container (ticket 2). It removes the recipient at once, writes a
     change-log row (no actor) and mails all admins. It refuses to leave no
     active recipient while a destination is enabled. The matching
     `deploy.sh --backup-recipient-cancel <fingerprint>` (terminal only, ticket 5)
     runs `server backup-recipient-cancel` and cancels a pending add or removal at
     once, with a change-log row. **A recipient is identified by the FULL sha256
     (64 hex) of its `age1...` string**; that is the route key and the command
     argument. The page shows the first 12 characters only; adding a key whose
     short form equals an existing one is refused (`409`).
   - What this gives, honestly: 24 hours in which a real admin can see the notice
     and cancel a pending add. **The web path has no fast removal**: if an
     attacker's key is already active, only the host command (or waiting 24 hours)
     removes it. The hold does not stop an attacker who also controls the admins'
     mailboxes, or who waits unseen for 24 hours; the notice is only as good as
     someone reading it. It also delays a legitimate rotation by 24 hours. The
     length of the hold is a guess. It is open point 10.
21. **The local volume stays as it is** (a plain dump, 0600). It is on the
   host, it is what `restore.sh latest` and the update rollback use, and
   encrypting it would break both. Disk encryption of the host is the admin's
   choice.
22. **Authenticity: age gives secrecy, not authenticity, so a manifest is
   signed and the object's hash is kept in the database.** An age public key is
   public: anyone who can write to the remote folder can encrypt a malicious dump
   to the admin's recipient and swap it in. So:
   - `backup_runs.sha256` holds the sha256 of each uploaded (encrypted) object. It
     is the reference for the `verify` run (#42). A manifest on the same
     remote is NOT used as the reference.
   - Each upload writes a manifest next to the object. It names the object, its
     sha256 and size, the dump's UTC time, the schema version, the Launchpad
     version and the install id; it holds no host, URL or secret. It is signed
     with an Ed25519 key (standard library).
   - **The signing key's owner is `offsite`.** At BOOT (once the role and the
     migration are ready), and again on any scan that finds both `signing_public` and
     `signing_private_enc` empty, `offsite` makes the key pair. So the key exists
     before any destination can be enabled: the API refuses to enable a destination
     while `signing_public` is empty, and the page keeps "enable" disabled until the
     public key is shown (#44). `offsite` makes the pair, seals the private half with `BACKUP_ENCRYPTION_KEY` into
     `signing_private_enc`, and stores the public half (the column grant of #15). It does not touch
     `key_check`. An unreadable signing key shows as a failed GCM check when it is
     opened (T9 #9). The page shows the public half and asks the
     admin to keep it off the host (with the age private key). `keygen` makes the
     age pair only. There is no web route to replace it. **Replacement** is
     `deploy.sh --backup-rotate-signing` (terminal only, ticket 5), which runs a
     `server offsite rotate-signing` command in the running `offsite` container with
     `docker exec -T` (it needs the database and the key; it prints the new PUBLIC key
     only, so it needs no `--log-driver none`); it writes
     a change-log row (#27) and prints the new public key once.
   - **First use is compare-and-set**: `UPDATE backup_settings SET ... WHERE
     signing_public = '' AND signing_private_enc IS NULL`; if it changes no row,
     another `offsite` won and this one reads the stored key. A trigger on
     `backup_settings` is `UPDATE OF signing_public, signing_private_enc` only (two columns; `key_check`
     is the app's and is not covered). It allows a change to them only when the
     row is in that empty state, or in the same transaction as a `backup_change_log`
     row of kind `signing_key_replaced` (the check, for example on `xmin`, is
     chosen in ticket 1). The trigger function is `SECURITY DEFINER` (it must read
     `backup_change_log`, and `offsite` has no SELECT right on it; ticket 1 sets the
     function's `search_path`). So the role's UPDATE grant cannot overwrite or blank
     the key by a bug. **Honestly:** `offsite` also has INSERT on `backup_change_log`,
     so a COMPROMISED `offsite` can insert the row and replace the key; the trigger
     guards bugs, not a hostile process. The same holds for the recipient hold of
     #20: it is enforced in app code only; a DB trigger that rejects an
     `effectiveAt` less than 24 hours ahead is possible but, with the app and the
     table owner being one role, it also guards bugs only, and it is not in v1. A key that is blanked, half-empty or unreadable never makes
     `offsite` create a new one: runs fail `secrets_unreadable` until the admin
     runs `--backup-rotate-signing`.
   - **If the signing key is unreadable** (`BACKUP_ENCRYPTION_KEY` changed or
     lost), runs fail with `secrets_unreadable`. `offsite` does NOT make a new
     key by itself, because a silent new key would hide a tampered row. The admin
     runs `--backup-rotate-signing`. Old backups still verify with the old public
     key the admin saved.
   - What the signature proves: a restore with the saved public key rejects an
     object or manifest made or changed by someone with only remote write access.
     What it does not prove: freshness. A signature does not say "this is the
     newest". An attacker with remote write access can put back an OLD signed pair
     (or delete newer ones), and it still verifies. `restore.sh` prints the dump
     time and age of the pair it picked and the admin must look at it (#39).
     Nor does it help against an attacker who controls the host (the signing key
     is on it). A restore without the public key (`--trust-remote`, explicit,
     prints a warning) trusts the remote.
   - While it reads the dump to encrypt it, `offsite` computes the dump file's
     sha256 in the same pass (one read, no second read) and at the end compares it
     with `backup_dumps.sha256`; a mismatch deletes the scratch file and fails the
     run (`dump_mismatch`) before any upload, so a changed local file is not uploaded
     as good.
   - `restore.sh` verifies the signature and the sha256 against the manifest
     before it decrypts (#39). The sha in `backup_runs` is used too when the
     database is alive.
## Where the config lives

23. **Six new tables, one migration (the next free number).** Every column is `NOT
   NULL` unless it says NULL; text columns default to `''`, counters to 0,
   timestamps that are set at insert to `now()`. Enums are `text` with a CHECK.
   - `backup_settings`: one row (`id boolean PRIMARY KEY DEFAULT true CHECK (id)`,
     as `platform_settings`). `install_id` (8 random lowercase hex characters, made
     by the migration with `substr(replace(gen_random_uuid()::text, '-', ''), 1, 8)`, which
     needs no pgcrypto; names remote files); `instance_id` (a random uuid made by the
     migration; #37); `recipients jsonb` (default `[]`; each element
     `{publicKey, fingerprint, addedAt, addedBy, effectiveAt, removeAt, removeBy}`,
     where `fingerprint` is the full sha256 hex, `removeAt` and `removeBy` are null
     unless a removal is scheduled, #20); `ever_had_recipient boolean` (default
     false, #20); `signing_public text` (default `''`); `signing_private_enc bytea`
     (NULL until first use; the empty state is `signing_public = ''` AND
     `signing_private_enc IS NULL`); `key_check bytea` (NULL until the app first
     seals a secret, then the 32-byte check; the CHECK is `key_check IS NULL OR
     octet_length(key_check) = 32`: it must not be copied from T9 as `NOT NULL`);
     `notify_on_failure` (default true); `notify_on_success` (default false);
     `stale_alert_hours` (0 to 8760, default 48; 0 turns it off); `updated_at`;
     `updated_by` (NULL, `ON DELETE SET NULL`).
   - `backup_destinations`: `id uuid` (uuidv7); `name` (unique, 1 to 60); `type`
     (`s3`, `webdav`, `sftp`, `dropbox`); `enabled` (default false); `enabled_since` (NULL, set when `enabled` becomes true); `config jsonb`
     (default `{}`; non-secret: URL, bucket, region, folder, username, SFTP host
     key, TLS pin); `secrets_enc bytea` (NULL when no secret is set); `secrets_set
     text[]` (default `{}`); `schedule` (`daily`, `weekly`; default `daily`); `at_utc`
     (`HH:MM`, default `04:15`); `weekday` (NULL; 0 to 6; CHECK: not NULL if and only
     if `schedule = 'weekly'`); `keep_last` (1 to 365, default 14); `verify_every_days`
     (0 to 90, default 7); `last_stale_alert_at` (NULL); `created_at`; `updated_at`;
     `updated_by` (NULL, `ON DELETE SET NULL`).
   - `backup_dumps`: written by `backup.sh`:
     `id uuid`; `file_name` (unique); `status` (`running`, `ok`, `failed`);
     `request_id` (NULL, `ON DELETE SET NULL`: the request that caused it; NULL for the
     nightly dump); `started_at`; `finished_at` (NULL until done); `bytes` (NULL until
     done); `sha256` (NULL until done); `migration_version` (NULL); `file_removed_at`
     (NULL); `error_text` (default `''`, a fixed message).
   - `backup_requests`: `id uuid`; `state` (`queued`, `taken`, `done`, `failed`);
     `requested_by` (`app`, `offsite`); `requested_by_user` (NULL, `ON DELETE SET
     NULL`); `requested_at` (default `now()`, the DATABASE clock); `taken_at` (NULL);
     `finished_at` (NULL); `dump_id` (NULL, `ON DELETE SET NULL`, the dump made for it).
     Rule: a run that needs a requested dump waits for an `ok` dump whose `started_at` is
     at or after the request's `requested_at` (#28): a dump that started before the click
     is not uploaded for "Back up now".
   - `backup_runs`: `id`; `destination_id` (NULL, `ON DELETE SET NULL`);
     `destination_name` (copied from the destination at insert and kept after a delete; `''` for an unsaved-form probe or test, which has no destination); `dump_id` (NULL for `test`, `probe`,
     `exchange` and `verify` runs; `ON DELETE SET NULL`); `request_id` (NULL, `ON DELETE SET NULL`: the dump request a `waiting_dump`
     run waits on); `trigger` (`schedule`, `manual`, `catchup`, `retry`, `test`, `probe`,
     `verify`, `exchange`); `status` (`queued`, `waiting_dump`, `running`, `succeeded`,
     `failed`, `interrupted`, `canceled`);
     `attempt` (default 1); `will_retry` (default false); `retry_at` (NULL);
     `queued_at` (default `now()`); `started_at` (NULL until running); `finished_at`
     (NULL until final); `bytes` (NULL until the upload); `sha256` (NULL until the
     upload; of the encrypted object); `remote_name` (NULL until chosen); `steps jsonb`
     (default `[]`); `error_code` (default `''`); `error_text` (default `''`, safe);
     `locked_until` (NULL unless the run is `running` or `waiting_dump`); `notified_at` (NULL); `test_config jsonb`
     and `test_secrets_enc bytea` (both NULL; only an unsaved-config probe or test or a
     Dropbox exchange sets them, #36). CHECKs: `finished_at` is not NULL if and only if
     the status is final; `will_retry` only with `failed`.
   - `backup_change_log`: `id`; `at` (default `now()`); `actor_id` (NULL, `ON DELETE
     SET NULL`; NULL for a deploy command); `kind`; `destination_id` (NULL, `ON DELETE
     SET NULL`); `destination_name` (default `''`); `detail jsonb` (default `{}`, no
     secret); `notified_at` (NULL).
   A destination is a platform thing: no `org_id` (one instance is one org,
   decisions-T8 #1).
   **Run state transitions** (the only ones the code makes; a final status never
   changes):

   | From | To | When |
   |---|---|---|
   | (insert) | `queued` | the app or the scheduler makes the row |
   | `queued` | `running` | the worker claims it (dump ready, or no dump needed) |
   | `queued` | `waiting_dump` | a dump was requested and is not ready |
   | `queued` | `canceled` | a new `manual` run supersedes a queued retry (#31); or the destination was deleted (every trigger) or disabled (backup runs only, #31) |
   | `waiting_dump`, `running` | `canceled` | the destination was deleted, or disabled (backup runs only, #31); a `running` run is aborted and its partial remote object deleted best-effort (#18) |
   | `waiting_dump` | `running` | an `ok` dump with `started_at` at or after the request's `requested_at` exists |
   | `waiting_dump` | `failed` (`dump_unavailable`) | no dump started within 30 minutes of the request, or a started dump still `running` after 6 hours, or the dumps that started at or after the request have all ended `failed` and none is `running` or `ok` (#28) |
   | `running` | `succeeded` / `failed` | the run ends |
   | `running`, `waiting_dump` | `interrupted` | the lease ran out (boot recovery, #31) |

   `locked_until` (the lease) is set while a run is `running` AND while it is
   `waiting_dump`, and moved every 15 seconds in both. The destination's advisory lock
   (#31) is NOT held while a run waits for a dump (up to 30 minutes); it is taken when
   the run becomes `running`. Only one backup run per destination can be queued, waiting
   or running (the busy rule, #31), so two runs of one destination cannot both wait.
   A `failed` run with `will_retry` has `retry_at = now() + 30 minutes` on its own row, and leaves a NEW
   `queued` row (trigger `retry`, `will_retry = false`) with the same `retry_at`; the claim of #31 takes a queued row only when `retry_at IS NULL OR retry_at <=
   now()` (database clock); the failed row stays failed. `succeeded`, `failed`, `interrupted` and `canceled` are
   final.

24. **Sealing.** The T9 code (AES-256-GCM, HKDF, `Secret`) with
   `BACKUP_ENCRYPTION_KEY` and new info strings. The additional data names the
   column and the owner: the destination id for `secrets_enc`, the run id for
   `test_secrets_enc` (an unsaved test has no destination id), and the fixed
   word `settings` for the signing key. A blob copied to another row does not
   open. One blob holds all secrets of a destination; a partial update is a
   read, merge and seal under a row lock (T9 #15).
25. **Write-only on the API, as T9 #11.** `GET` shows `secretsSet`
   (from `secrets_set`) and `secretsUnreadable`. In a `PATCH`, a secret absent
   or `null` is kept, `""` clears it, a value replaces it. A `PATCH` that
   changes ANY field of `config` (host, URL, bucket, region, user, folder), the
   TLS pin or the SFTP host key while keeping a stored secret is `400` (the one
   exception is the FIRST pin or host key, set by `confirmedFingerprint` when none exists,
   #36): a stored
   password must not go to a new host, or to a host with a new trust anchor, by
   an edit that does not re-enter it. Two destinations of one install may not
   share the same type, host, bucket and folder (`409`): they share the
   `install_id` name prefix and would prune each other's copies (#33). The same rule applies to an unsaved-config test that names
   a stored secret (#36).
26. **Never in a log, an event, an idempotency row or an error.** Same
   as T9 #12: `Secret` redaction; every write route uses `idempotency.Reject`;
   errors name the field, not the value. A planted-secret test plants a secret
   in every field, including a sealed unsaved-test secret in `backup_runs`
   and the Dropbox code, and greps the log, the events, the idempotency rows,
   the responses, the run table (every column), the notice mails and every
   database column except the sealed blobs.
   **Events: an explicit exception to AGENTS.md's `events.Emit` rule.** AGENTS.md
   says writes that change state append an event. The change feed is per org and
   backups are not an org change (T9 #12 and #13 took the same line), so no
   event is emitted. The record of a change is `backup_change_log` (#27).
27. **Change control against a stolen admin session.** A stolen platform
   admin session could add a destination it controls, add or swap an age
   recipient, or aim a destination at an internal host, and so have the next
   backups sent to the attacker in a form it can read. The controls:
   - Every such change writes a row in `backup_change_log` (`at`, `actor_id`
     (NULL for a deploy command), `kind`, `destination_id`, `destination_name`,
     `detail` with no secret, `notified_at`) in the same transaction as the
     change: destination added, edited in host/URL/user/folder, enabled, deleted;
     recipient added, removed or cancelled; signing key replaced; notice settings
     changed; and EVERY change to a destination field, including `keep_last`,
     `schedule`, `at_utc`, `weekday`, `verify_every_days`, `enabled` (a disable too),
     the pin and the host key. Each one is mailed (#41). A `keep_last` below 2 is
     refused (`400`) unless the body has `confirmLowKeep: true` (the page asks for a
     typed confirmation): `keep_last = 1` plus a prune is a quick way to destroy older
     copies. It is an audit table, not only `updated_by`.
   - The app's notice job mails ALL active platform admins (not only the actor)
     for each such row (#41).
   - Recipient adds and removals are held for 24 hours (#20).
   - **Private-range reach is an operator setting, not a UI tick** (#35).
   - Limits, said plainly: an attacker with a platform admin session can also
     change the mail provider (T9) and so divert the notices; a notice is a delay
     for a defender, not a block. The log stays.
## The runner

28. **Three kinds of rows drive it.**
   - `backup_dumps`: written by the `backup` service, with `psql`, as the `devops`
     owner role (its existing `PGUSER`). Columns in #23. Both `backup.sh once` and
     `loop` record the dump (`running` at the start, then `ok` or `failed`). It stamps `started_at` and `finished_at` with SQL `now()`, not the shell's `date`, so they share the database clock with `requested_at`. `offsite` checks a dump file name against `^devops-\d{8}T\d{6}Z\.dump$` before it opens the file. The
     error text is fixed (never `pg_dump` output). **`psql -c` does not interpolate
     `:'name'` variables**, so the script sends its SQL on standard input (`psql -f -`
     or a here-document) with each value passed as a `-v` variable and used as
     `:'name'`; no value is built into a SQL or shell string. Ticket 5 checks this
     with a hostile value. **Reconcile with pruning:** after `backup.sh` deletes a
     file by age it sets `file_removed_at` on that name; at the start of each run it
     also marks rows whose file is missing. `offsite` only uses `ok` rows with
     `file_removed_at` NULL and a file that exists. If the tables are missing (the app
     has not migrated yet) `backup.sh` logs it and goes on: the dump must never fail
     because of the new tables.
   - `backup_requests`: written by the app for "Back up now" and by `offsite` when a
     scheduled run finds the newest `ok` dump older than 6 hours (by the database
     clock). Columns in #23. The `backup` service checks it every 30 seconds with
     `psql` while it waits for its daily time (this replaces one long `sleep` with a
     short loop; the daily time and `BACKUP_KEEP_DAYS` work as before). It takes a
     request with one statement (`UPDATE ... SET state='taken', taken_at=now() WHERE id
     = (SELECT id ... WHERE state='queued' ORDER BY requested_at LIMIT 1 FOR UPDATE SKIP
     LOCKED) RETURNING id`), makes the dump, then sets `done` and `dump_id` or `failed`.
     **No value read from `backup_requests` is ever interpolated into a shell command**:
     the script validates the id as a UUID with a `case` pattern and uses it only as a
     `psql` variable. A run waits for an `ok` dump whose `started_at` is at or after the
     request's `requested_at` (the run stores the request in `request_id`); an older
     dump never satisfies it, even if it finished later. The cost of the 30
     second check is a guess; ticket 5 measures it.
   - `backup_runs`: written by `offsite`. One row per destination per attempt.
     Triggers and statuses are in #23. A `waiting_dump` run waits for a requested dump.
     The deadline is measured from the dump's START, not its end: the run fails `dump_unavailable` only if no
     dump with `started_at` at or after the request has STARTED within 30 minutes of the request (for example
     the `backup` service is stopped), or if the dump that started is still `running` after 6 hours, or, at once, if the dumps that started at or after the request have all ended `failed` and none is `running` or `ok` (a failed dump is not replaced by a wait: a person asks again; the 6 hours is a guess;
     the real dump duration is not measured, #40, ticket 5 measures it). A slow `pg_dump` therefore does not
     fail the run. `dump_unavailable` is not retried automatically, but it is not permanent for a person: a new
     "Back up now" makes a new request and a new run.
   - **Extra dumps.** A destination whose time is more than 6 hours after the newest
     dump (the dump's default is 03:15 UTC) makes `offsite` request an extra dump at
     each run: a second `pg_dump` per day, one more local file counted by
     `BACKUP_KEEP_DAYS`. The default destination time (04:15) avoids it. The page shows
     a hint when a destination's time is more than 6 hours after the latest dump.

29. **Row cleanup belongs to the app.** `offsite` has no DELETE right
   (#15). A job in the app (hourly) deletes: `backup_runs` older than 180 days;
   `backup_dumps` rows with `file_removed_at` older than 180 days, and failed rows
   (their `file_removed_at` stays NULL) whose `finished_at` is older than 180 days; `backup_requests`
   in a final state older than 7 days, and a request left `taken` for more than 2 hours whose dump is not `running` (set `failed`); a request whose
   dump is still `running` is left alone until the 6 hour rule below marks that dump `failed`. `backup.sh`
   sets a request `done` only `WHERE state = 'taken'`, so a request already `failed` is never flipped back; the
   run does not use the request state at all, it uses the dump rows (#23 rule);
   recipient elements whose `removeAt` has passed or whose add was cancelled (dropped from
   `recipients`; the next recipient write drops them too); `backup_change_log` older than 730 days (a guess for an audit trail); and it clears `test_secrets_enc` on any run row older than 10 minutes. The
   `offsite` boot clear and the app sweep both exist; either alone is enough. The same
   job marks a `backup_dumps` row left `running` for more than 6 hours `failed` (a killed
   `backup` container, or a restored database that carries another host's `running`
   row); `backup.sh` does the same for its own rows at the start of each run.
30. **Waking `offsite`: LISTEN/NOTIFY with a poll as the safety net.** The app
   (and `offsite` itself) issue `pg_notify('backup_work', '')` in the transaction
   that inserts a run or request row. `offsite` keeps one dedicated connection
   that `LISTEN`s, and also scans every 30 seconds in case a notification is lost
   (a restart, a dropped connection). So a test or "Back up now" should start
   within about a second when `offsite` is up (a guess, not measured) and never
   later than the 30 second scan. The `backup` service polls instead of listening
   (a shell script), so a "Back up now" waits up to 30 seconds for the dump to
   start, then the dump time. The page polls the run every 2 seconds, up to 60
   seconds for a test.
31. **The sequence of one run.**
   1. Take the destination's advisory lock on a **dedicated connection**
      (`pg_try_advisory_lock` is session scoped; a pooled connection would
      return to the pool with the lock held or lose it) BEFORE the claim of #31. If the
      try-lock returns false, nothing changes: no row is claimed and the next scan tries
      again, so a run is never `running` without the lock. One run per destination, safe with two
      `offsite` containers. The database enforces the one-run rule too: a partial unique index on
      `backup_runs (destination_id) WHERE status IN ('queued','waiting_dump','running') AND trigger IN
      ('schedule','manual','catchup','retry')` (ticket 1). The scheduler's `catchup`, the `schedule` and the
      API's `manual` inserts use `ON CONFLICT DO NOTHING` (or answer `409 destination_busy`); a `retry` row is
      inserted after its parent run is final, so it does not collide; a `manual` run cancels a queued retry and
      inserts in one transaction.
   2. Pick the newest usable dump (or ask for one, #28); check free space (#18);
      encrypt to scratch, computing the sha256 (#18).
   3. Upload the object, then the signed manifest (a manifest never exists for
      a missing object); list and compare size (and a hash where the protocol
      gives one).
   4. Store `bytes`, `sha256`, `remote_name` in the run; prune (#33);
      delete the scratch file; write the final status.
   The run row's `locked_until` moves every 15 seconds. A `running` row whose
   lease ran out is marked `interrupted` at boot (and counts as a missed
   slot).
   - **Remote names:** `launchpad-<install_id>-<dump UTC time>-<4 hex>.dump.age`, the time written as
     `20261026T031500Z` (`YYYYMMDDTHHMMSSZ`),
     and the same name ending `.json`. The 4 hex characters are random per
     attempt. A retry or a second run of the same dump makes a NEW pair; it
     never overwrites an older one with different ciphertext. Two successful
     runs of one dump are two copies, and both count toward retention.
   - **Schedule.** Per destination: `daily at HH:MM UTC` or `weekly on a weekday at
     HH:MM UTC`. UTC only in v1. (The image does have `tzdata`, `server/Dockerfile`;
     time zones are left out to avoid daylight-saving double and missing
     runs. A later ticket can add a zone.) The page shows the local time in the
     browser. Default `04:15` UTC daily (the dump's default is 03:15).
   - **Clock.** Slots, leases (`locked_until`), `effectiveAt`/`removeAt` and the stale
     ages all use the DATABASE's `now()`, never the container's clock. A host or
     container clock that is far off still breaks request signing at the remote:
     S3 answers `RequestTimeTooSkewed` and other servers' `Date` header disagrees; the
     driver maps that to `clock_skew` (not `forbidden`, not retried). The runbook
     says to run NTP on the host.
   - **Missed runs.** At boot and every minute, for each enabled destination, if
     the last passed slot has no run, make ONE run (`catchup`), not one per
     missed slot. A slot counts as having a run when a run of trigger `schedule`, `catchup`, `retry` or
     `manual` for it is `queued`, `waiting_dump`, `running`, `succeeded` or `failed`; an `interrupted`
     or `canceled` run does not satisfy it. Only slots after `max(created_at, enabled_since)` count: a new or
     re-enabled destination makes no immediate run for a slot that passed before it
     existed or was enabled ("Back up now" is there for that).
   - **Retry.** A failed `schedule`, `catchup` or `manual` run is retried ONCE. On a retryable failure
     the run sets `will_retry = true` and `retry_at = now() + interval '30 minutes'` (database clock) on
     ITS OWN row, and inserts the queued `retry` row with the same `retry_at` and `will_retry = false`.
     Only these codes are retried:
     `timeout`, `connect_refused`, `dns_failed`, `stalled`, and `server_error`
     with a 5xx status. NOT retried (they need a person): `auth_failed`,
     `forbidden`, `not_found`, `quota_exceeded`, `tls_*`, `host_key_changed`,
     `ssrf_blocked`, `secrets_unreadable`, `too_large`, `no_space`,
     `dump_mismatch`, `dump_unavailable`, `clock_skew`, `instance_changed`, `scratch_full`, and `server_error` with a 3xx or 4xx
     status (a redirect is a configuration fault, not a transient one). A run
     that will be retried sends no notice until the retry ends.
   - **Deleted or disabled destinations.** For a DISABLED destination the worker marks only its BACKUP runs
     (`schedule`, `manual`, `catchup`, `retry`) that are `queued`, `waiting_dump` or `running` as `canceled`;
     `probe`, `test`, `exchange` and `verify` runs are left alone, because a new destination is created
     disabled and must be probed, tested and (for Dropbox) exchanged before it can be enabled (#9, #36). For a
     DELETED destination, identified as `destination_id IS NULL AND destination_name <> ''` (the name is copied at insert and survives the delete, #23), every run in those states is canceled, whatever its trigger. An unsaved probe or test (`test_config` set, no destination, `destination_name = ''`) is NOT a deleted destination and is never canceled by this rule.
     A `running` run that is canceled is aborted by the worker, which makes the same best-effort delete of its
     partial remote object as #18.
   - **Advisory lock and states.** The destination's advisory lock is not held in `queued` or `waiting_dump`; it is taken before the move to `running` (from `queued` or from `waiting_dump`, step 1 of #31) and released when the run ends. A run in `waiting_dump` renews only its lease (`locked_until`).
   - **Instance guard.** `offsite` rereads `/offsite-work/instance-id` on every scan, not only at boot.
   - **Claim order.** For a backup run the worker picks a candidate destination, try-locks it (#31 step 1), then claims a row filtered by that `destination_id`. `test`, `probe`, `exchange` and `verify` runs, and unsaved runs (NULL `destination_id`), are claimed WITHOUT the destination lock, so a test is allowed while a backup runs.
   - **Claim.** A worker claims a run with one statement: `UPDATE backup_runs SET
     status = 'running', started_at = now(), locked_until = ... WHERE id = (SELECT id
     FROM backup_runs WHERE status = 'queued' AND <due> ORDER BY queued_at LIMIT 1 FOR
     UPDATE SKIP LOCKED) RETURNING id`. The cancel of a queued retry is `UPDATE ... SET
     status = 'canceled' WHERE id = $1 AND status = 'queued'`: if it changes no row the
     retry was just claimed, and the `manual` request answers `409 destination_busy`.
   - **Busy.** Only backup runs (`schedule`, `manual`, `catchup`, `retry`) count;
     `test`, `probe`, `exchange` and `verify` runs are excluded. `POST .../run` on a DISABLED destination is refused `409 conflict`; otherwise it answers
     `409 destination_busy` while a `running`, `waiting_dump` or `queued` backup
     run for that destination exists, except a queued
     RETRY: a new `manual` run cancels a queued retry (status `canceled`) and
     starts. A `test` is allowed while a backup runs; it uses its own connection.
   - `interrupted` and `canceled` runs send no notice (#41).
   - **Weekly vs the dump time.** A destination does not run its own
     `pg_dump`. It uploads the newest dump at its own time.
32. **Safe errors.** A driver returns a code and a fixed text: `dns_failed`,
   `connect_refused`, `tls_untrusted`, `tls_wrong_host`, `tls_pin_mismatch`,
   `auth_failed`, `forbidden`, `not_found`, `timeout`, `too_large`,
   `quota_exceeded`, `host_key_changed`, `ssrf_blocked`, `secrets_unreadable`,
   `no_space`, `stalled`, `dump_mismatch`, `dump_unavailable`, `clock_skew`, `instance_changed`, `scratch_full`
   (the scratch volume ran out DURING encryption; `no_space` is the check before it),
   `server_error` (with the HTTP or SFTP status number).
   Never the remote server's reply text, never the URL with its query, never a
   path. The rule of T9 #26: a server may echo a username, a password or
   anything else.
33. **Retention per destination: keep the last N complete backups.**
   `keep_last` is 1 to 365. The job lists the folder and selects only names
   that match `^launchpad-<this install_id>-\d{8}T\d{6}Z-[0-9a-f]{4}\.(dump\.age|json)$`.
   - A **backup is a pair** (object and manifest with the same stem). Only
     complete pairs count toward N. They are sorted by the time in the name,
     then the stem, not by the remote's modified time.
   - It deletes pairs older than the newest N, object and manifest together.
   - An object or a manifest without its partner whose dump time in the name is
     more than 24 hours old (the time in the name, not the remote's modified time)
     and that no `running` run names, is an orphan of this install and is deleted; it
     never counts toward N.
   - It runs only after a new upload is verified, so a failed run never deletes
     a good copy. It never touches a name that does not match: other files in
     the folder and other installs' files are safe.
   - Request URLs for `Get` and `Delete` (WebDAV and others) are built ONLY from
     names that match the regex above, never from the `href` or path a `PROPFIND` or
     list reply returned.
   - A failed delete is a warning (the run stays `succeeded`, `error_text`
     says `prune_failed`).
   "Keep N days" was considered. It is unclear for a weekly schedule and a
   destination that was down; keep-last-N is plain.

## Network safety

34. **The SSRF guard.** (LAUN-1 note: the guard code lives in `internal/netguard`, owned by decisions-T17 ticket 2 (T17 #31); `internal/backup` keeps only its policy.) The admin runs the install, but the code dials a host
   typed in a request, and a stolen admin session must not become a probe of the
   host's network. T9 #28 accepted this for SMTP; for backups the risk is larger
   (a long upload of the database, and tests that tell open from closed), so
   there are rules:
   - The check is at DIAL time, a `net.Dialer.Control` function on the resolved
     address, not only when the config is saved. That defeats DNS rebinding.
   - **Always refused**, whatever the operator setting: `0.0.0.0/8`, `127.0.0.0/8`,
     `::1`, `::`, `169.254.0.0/16` (link-local, with the cloud metadata address),
     `fe80::/10`, `fec0::/10`, `192.0.0.0/24`, `192.88.99.0/24`, `198.18.0.0/15`,
     `240.0.0.0/4`, the documentation ranges (`192.0.2.0/24`, `198.51.100.0/24`,
     `203.0.113.0/24`, `2001:db8::/32`), `100::/64`, `64:ff9b:1::/48`, multicast
     (`224.0.0.0/4`, `ff00::/8`) and the broadcast address. Addresses that embed
     an IPv4 address are unwrapped first and checked as that IPv4 address:
     IPv4-mapped (`::ffff:0:0/96`), IPv4-compatible (`::/96`), NAT64
     (`64:ff9b::/96`), 6to4 (`2002::/16`) and Teredo (`2001::/32`); an address that
     cannot be unwrapped safely is refused.
   - **Metadata addresses are refused even inside an operator range:**
     `169.254.169.254` and `169.254.170.2` (already in `169.254/16`),
     `100.100.100.200` (inside `100.64/10`), `fd00:ec2::254` (inside `fc00::/7`)
     and `168.63.129.16` (a public-looking Azure address).
   - **Refused unless the operator listed its range** (#35): private ranges
     (`10/8`, `172.16/12`, `192.168/16`, `fc00::/7`, `100.64/10`). The container's
     default gateway (read from `/proc/net/route`) is refused unless the operator
     lists it as an exact `/32` (a listed range that merely CONTAINS the gateway, such
     as `172.17.0.0/16`, does not open it): it is the host, and the published ports of nginx and
     silo are on it. This gateway rule is not measured on Linux.
   - Every HTTP client sets `Proxy: nil` (the environment's proxy variables are
     ignored) and does not follow redirects (a `3xx` is `server_error` with the
     status, not retried). A redirect is a common way to send credentials to a
     second host.
   - **SFTP uses the same guarded dialer.** `ssh.Dial` would dial on its own, so the
     driver dials with the guarded `net.Dialer` and hands the connection to
     `ssh.NewClientConn`.
   - **The product's own silo is dialed by a different dialer** (ticket 14): a
     non-user-entered dialer that may connect only to the one host and port of
     `offsite`'s own `S3_ENDPOINT` (from the environment, set by `deploy.sh`), on
     `files_net`, whatever the guard says. It compares the requested host:port NAME
     with the configured one BEFORE any resolution, and only the files code path
     (ticket 14) can obtain it. It cannot be pointed at anything else and does not read `BACKUP_ALLOW_PRIVATE_CIDRS`. User-entered destinations
     never use it.
   - The service names on `offsite_db` (`postgres`) resolve from `offsite`. A
     destination typed as `postgres` resolves to a private address and is refused
     unless the operator listed that range; with the range listed it would connect
     to port 5432 and speak the wrong protocol. A known edge, not measured.
   - HTTPS only for WebDAV, S3 and Dropbox. `http://` is allowed only when the
     host's address is in an operator-listed range (a MinIO on the LAN); the form
     says "not encrypted in transit". The dump is still age-encrypted.
   - **The SSRF test table** has a row for every refused range and form above
     (including each metadata address inside an operator range), the compose
     service names and Docker DNS names (`postgres`, `app`, `silo`, `backup`,
     `host.docker.internal`, `gateway.docker.internal`), a resolver that answers a
     public address and then a private one (rebinding), `http_proxy` set in the
     environment, a redirect to a private address, and an SFTP host that resolves
     to loopback.
35. **Private-range reach is an operator setting.**
   `BACKUP_ALLOW_PRIVATE_CIDRS` in `deploy/.env` (comma list of CIDRs, default
   empty) is passed to `offsite`. A destination in a listed range works; there is
   no per-destination tick and no UI or API that sets it. A home server or NAS on
   the LAN needs the operator to list its range once.
   **The list is validated when it is parsed** (at boot and by `deploy.sh`): a
   prefix of `/0`, any range that overlaps an always-refused range of #34
   (loopback, link-local, `0.0.0.0/8`, and so on), and a range shorter than `/8`
   (IPv4) or `/32` (IPv6) are rejected, and the boot of `offsite` fails with the
   reason. The metadata addresses stay refused inside a valid range (#34). Why
   not a UI tick: a stolen admin session could turn it on and aim a destination at
   an internal address. Changing the setting needs a shell on the host. This is
   open point 8; the default is the operator setting.
36. **TLS, host keys, pins and the test connection: two phases.**
   - TLS 1.2 or newer. For a normal destination the certificate chain is
     checked against the system roots and the host name. There is no "skip
     verify" for a connection that sends credentials or data.
   - **Phase 1, probe (no credentials).** Open a TCP connection through the
     guarded dialer and do a TLS handshake WITHOUT certificate verification (a
     verification callback that only records the chain, and whether it would have
     verified), or, for SFTP, the SSH key exchange with a host key callback that
     only records. It records the leaf's SPKI sha256 or the SSH host key sha256 and
     closes at once. Nothing is sent after the handshake: no credentials, no data,
     no request. The result is shown to the admin: "valid certificate for this
     host" or "not trusted: fingerprint X". It is a handshake without verification,
     and it is safe only because it sends nothing the server could use.
   - **The first pin is trust on first use, for TLS as well as for SSH.** The
     admin confirms a fingerprint the server showed; the dialer cannot know it is
     the right server. The docs say to compare it with the fingerprint the server's
     owner gives.
   - **Where the probe result goes.** A probe is a `backup_runs` row with trigger
     `probe` (for a saved destination, its id; for an unsaved form, a NULL
     `destination_id` and the form in `test_config`). `offsite` writes the result
     (kind, fingerprint, whether the chain verified, safe error) into `steps`. The
     web polls `GET .../runs/{id}` as for a test.
   - **Phase 2, authenticate and test.** Starts only when the certificate or
     host key is trusted (a valid chain for the host name, or a stored pin that
     matches, or an SSH host key that matches the stored one), or the admin
     confirmed the probed fingerprint: the `test` body carries it in the field
     `confirmedFingerprint`, and the APP saves it as the pin or the host key (in
     `config`, with a change-log row) before it queues the run, but ONLY when the
     destination has no pin or host key yet (the first pin). If one exists and differs,
     the request is `400`: changing a trust anchor needs the secrets to be re-entered by
     a `PATCH` (#25), then a new test; for an unsaved form
     it rides in `test_config` and is saved with the destination on create. Then it authenticates, writes a small random file
     `launchpad-<install_id>-test-<random>.tmp` (which the retention regex cannot
     match), lists the folder, reads the file back, compares, deletes it,
     and records steps (`connect`, `auth`, `write`, `read`, `delete`, each ok
     or a safe code).
   - **The pin is an SPKI pin** (sha256 of the leaf's SubjectPublicKeyInfo), not
     a hash of the whole certificate. A renewal that keeps the key keeps the
     pin; a renewal with a new key (the default for many ACME clients) needs
     the admin to confirm the new pin again (the runs fail with
     `tls_pin_mismatch` until then). **A pin bypasses the chain and host name
     checks**: the connection is accepted if the SPKI matches. The expiry date
     is still checked. The form says so.
   - **SFTP host key:** trust on first use, shown to the admin and saved in
     `config` only on confirmation. A different key later is `host_key_changed`
     and never updates by itself; changing it is an explicit confirmed edit.
   - **Throwaway state.** `offsite` writes the probe and test result only into the
     run row; it never edits `config`.
   - **Unsaved-config tests.** A test of a form that is not saved sends its
     secrets in the request. They are sealed into `backup_runs.test_secrets_enc`
     (additional data: the run id) for the length of the test, and deleted when
     it ends, on failure, at `offsite` boot, and by a sweep of anything older
     than 10 minutes. A test that names a STORED secret ("use the saved
     password") for a different host, URL, bucket, user or folder than the saved one is
     `400` (#25).
   - **The test is a port-scan oracle** (it tells an open port from a closed
     one and a TLS host from a non-TLS host, by the step codes). That is
     accepted, as T9 #28 accepted it for SMTP: only platform admins reach it, it is
     rate limited (6 a minute, burst 3; a guess, copied from the mail test and not measured), and the guard refuses the internal ranges
     first (#34). Replies of the remote server are never shown.
   - **Credentials scope.** The docs tell the admin to give Launchpad a
     credential limited to one folder or bucket, with write, list and delete
     rights (an S3 key limited to the bucket and prefix, a Nextcloud app
     password, a chrooted SFTP user, the Dropbox App folder). It does not
     need read-all. A broken-into host with these rights could still delete old
     copies; the docs say to turn on the provider's versioning or object lock
     where it exists.

37. **(Paths `/work` below read `/offsite-work`: superseded by Build notes ticket 5 #1.) Disaster recovery needs what the lost host held, and a clone must not write to the
   original's folder.** The destination's
   credentials and `BACKUP_ENCRYPTION_KEY` are on the host, and so is the
   database. If the host is lost, they are lost together. So restore from a
   remote copy must work with NO running Launchpad and NO database
   (#39). The runbook tells the admin to keep four things off the host:
   the age private key, the signing PUBLIC key (#22), the destination's own
   credentials as the provider issued them, and the URL and folder. The page
   lists them as "keep these safe".
   **A restored or cloned database must not start backing up into the original's
   folders** (it would prune the original's copies, #33). `offsite` keeps a file
   `/offsite-work/instance-id` on its scratch volume (which a restore does not bring)
   holding `backup_settings.instance_id`. If the file is missing while the database
   already has a `succeeded` run, or the file and the database disagree, `offsite`
   runs no backup run of any of the four backup triggers (`schedule`, `manual`, `catchup`, `retry`; a cloned
   database may carry a queued `retry` row, which would otherwise upload and prune under the original's
   `install_id`): the claim statement of #31 skips them, and the scan marks every `queued` and `waiting_dump` backup run `failed` with `instance_changed` (and `will_retry = false`, so no retry row follows) on each pass while the guard fails, which also frees the one-run index of #31 (a queued retry row would otherwise block its destination), and the
   page says why. A person resumes with `deploy.sh --backup-resume` (terminal only). It gives the
   instance a NEW `install_id` and a new `instance_id` (a `psql` update as the owner
   role, with a change-log row), then runs `docker run --rm --user 70:70
   --network none -v <project>_offsite_work:/offsite-work launchpad-api:<version> /bin/server offsite resume
   --instance-id <new id>` (ticket 4), which writes `/offsite-work/instance-id`. A first deploy
   (no `succeeded` run) writes the file by itself. **Why a new `install_id`:** a clone
   that resumed with the old one would share the name prefix and prune the original's
   copies in a shared folder (#33). The cost: copies made under the old `install_id`
   are never pruned by the new one (clean them by hand; the runbook says so). A VM
   clone that copies the volumes copies `/offsite-work/instance-id` too, so the guard does not
   fire: the runbook says to run `--backup-resume` after any clone. So
   a restored or cloned instance starts with backups stopped, not with destinations
   silently live.
38. **Attachments (silo): designed, built later (tickets 14 and 15).** They are in
   no backup now, so a restore gives a database that names files that are gone.
   After the database path works:
   - Silo objects are written once and never changed (`docs/deploy.md`), so a copy
     is incremental by key: list the bucket, compare with the keys in the last
     files manifest, upload only new objects, each as its own age file named by a
     hash of the key, under `launchpad-<install_id>-files/`.
   - The files manifest (key, size, sha256, remote name, `lastSeen`) is encrypted,
     signed like #22, and uploaded after the objects.
   - **Silo is on `edge` only, and `offsite` must not join `edge`** (#13). So
     ticket 14 adds a NEW `internal` network `files_net` that only `silo` (in
     addition to `edge`) and `offsite` join, a read-only silo user for `offsite` (a
     new key, not the app's bucket key and not the root key), and `S3_*` variables
     for `offsite`. `offsite` reaches silo with the dedicated dialer of #34, not
     the guarded one (the silo address is private and would be refused). The
     compose check then allows `S3_*` on `offsite` and nowhere else new. Until
     ticket 14 `offsite` has none.
   - **File retention works without the database** (`offsite` has no right on the
     attachment tables). The manifest records `lastSeen`, the last time the object
     was in the silo listing. A remote object is pruned only when it has been absent
     from the silo listing for at least `max(30 days, the age of the oldest dump kept
     on that destination + 1 day)`. The app removes an attachment's object from
     silo when the attachment is deleted (to be confirmed by ticket 15 against the
     code; not checked here). Until built, nothing is deleted remotely (safe, costs
     space).
   - The first full upload of a large bucket may take hours; its size is not
     measured. Until the first full upload the page says "files not backed up".
   Product storage stays in silo. This is only a copy.
## Restore, limits and reporting

39. **Restore from a remote copy: `deploy/restore.sh` pulls, verifies and decrypts,
   on the host.** `restore.sh` is shell: it cannot check an Ed25519 signature or
   read a database. The Go binary does those, in short-lived commands.
   - **Usage (one line):** `deploy/restore.sh (--remote NAME | --remote-config FILE)
     [--identity FILE] (--verify-key FILE | --trust-remote) [--allow-cidr CIDR]
     [--work-dir DIR] [--fetch-network NAME]` (and, with `--remote-config`, `--install-id ID`; `--fetch-network` is for tests only, ticket 12)`
     latest|STEM`. `latest` means the newest complete pair by the (unauthenticated)
     remote name; `STEM` is the remote
     name without its extension (`launchpad-<install_id>-<time>-<4 hex>`), not a
     file path. Without `--remote` the old `latest` and file path forms mean the
     local volume or a host file, as today.
   - **Fetch.** `--remote NAME` runs `docker exec -T <offsite> /bin/server offsite
     fetch --destination NAME --stem STEM|latest` (it reads the destination from the database).
     `--remote-config FILE` runs `server offsite fetch --config FILE` in a throwaway
     container (below). Both write one length-prefixed frame to stdout: a version
     word, the length of the manifest, the manifest, the length of the object, the
     object. Nothing else is written to stdout; `docker exec -T` avoids a TTY.
   - **Host disk.** The object (up to 5 GiB, #40) is written on the host under
     `--work-dir DIR` (default `${TMPDIR:-/var/tmp}`, a directory `restore.sh` makes mode
     0700 and removes at the end). `unpack` checks the free space of that file system
     against the declared object length plus 1 GiB before it writes, and refuses with
     `no_space`.
   - **User of each throwaway command.** The image runs as uid 10001, `offsite_work` is owned
     by 70:70, and restore work directories are owned by the caller with mode 0700. So:
     `resume` runs `--user 70:70`; `keygen` runs with the image's default user, no mounts, `--network none` and
     `--log-driver none`; `verify`, `decrypt`, `fetch --config FILE` and `unpack` run
     `--user <caller uid:gid>`; every file or directory mounted into them must be readable (or, for
     `unpack`, writable) by that uid. The deploy tests assert the `--user` of each command, as they
     assert `--log-driver none`.
   - **Unpack, with no host `tar`.** The frame goes to `server offsite unpack
     --out /out --max-object-bytes N` in a throwaway container (`--network none`,
     the host directory mounted, `--user` the caller's uid and gid). It writes
     only two fixed names, `object` and `manifest.json`, into a directory that
     `restore.sh` made mode 0700 and removes at the end. It refuses a wrong
     version, a manifest above 1 MiB, an object above the cap (5 GiB, #40), a length
     that does not match the bytes, and trailing bytes. A hostile stream cannot
     choose a file name, a link or a path because it never carries one. A test
     feeds hostile frames (huge and negative lengths, short and long bodies, extra
     bytes).
   - **Verify, then decrypt.** The ciphertext is a host file before anything is
     verified, because a hash needs the whole object. The order: `server offsite
     verify --public-key FILE --manifest manifest.json --object object` in a
     throwaway container with `--network none` (it checks the Ed25519 signature of
     the manifest, then the object's sha256 against the manifest; it needs no
     database and no network); then, when the database is alive (a read-only `psql` lookup that `restore.sh` runs in the
     postgres or `backup` container; ticket 12), the object's
     sha256 against `backup_runs.sha256`; then decrypt and pipe. `--trust-remote`
     replaces `--verify-key`, skips the signature and prints a warning that a
     malicious dump can run arbitrary SQL as the bootstrap superuser (for example
     `COPY ... PROGRAM`), because the restore connects as that role.
   - **Binding.** `verify` fails if the manifest's object name, dump time and
     `install_id` disagree with the fetched stem and with the destination (the
     `install_id` from the database, or `--install-id` with `--remote-config`). A
     renamed or swapped pair therefore fails.
   - **Freshness:** `restore.sh` prints the dump time and the age of the pair it
     picked, taken from the SIGNED MANIFEST, not from the unsigned remote name (a
     signature does not prove it is the newest, #22) and, for `latest`, the manifest
     time of the newest pair it saw. The admin must read it.
   - **Decryption on the host, outside the long-running containers.** The private
     key is a file the admin gives (`--identity`). If `age` is on the host,
     `restore.sh` uses it. Otherwise it runs `server offsite decrypt --identity
     /identity` in a throwaway container with `--network none` and the key file
     mounted read-only. The PLAIN dump is piped from that process into the existing
     `pg_restore` path (the host reads a file and pipes it into `docker exec -i`, as
     `restore.sh` does for a host file today). It is
     not written to a file by `restore.sh`. **Docker logs:** every throwaway container
     that prints a plaintext dump or a private key (`decrypt` and `keygen`) runs `docker run --rm --log-driver none`, and `docker exec`
     calls use `-T`; the host `age` and `age-keygen` are preferred. The deploy tests
     assert those flags. The plain dump still passes through pipes and process
     memory. The scratch database and the row count check stay as they are.
   - **No database, no running app (new host):** what must exist is either a
     postgres container to restore into, named with the existing `--container NAME`
     (it needs the postgres client tools and `PG*` variables), or a stack made by
     `deploy.sh`. With `--remote-config`, `restore.sh` skips its stack preamble:
     `load_env`, `guard_project`, `env_topup` and the `cid backup` check. `--remote-config
     FILE` (the file must be mode 0600 and owned by the caller: `restore.sh` refuses one readable by group or
     other; it is mounted read-only into the throwaway container as `-v FILE:/config.json:ro` and read by
     `fetch --config /config.json`, never put on a command line or in the environment; the admin deletes it
     after the restore, and `restore.sh` says so) reads a JSON
     file the admin wrote (`{"type":"s3","endpoint":...,"bucket":...,"accessKey":...,
     "secretKey":...}`, or `{"type":"webdav","url":...,"username":...,"password":...,"folder":...}`). There is no compose project and no `egress` network, so
     the fetch runs on Docker's default `bridge` network (a route out, no other
     service of ours). The guarded dialer in the binary is the control, with its
     always-refused list and the container's default gateway refused; the CIDR list
     is `--allow-cidr`, validated as in #35.
   - **The server image on a new host.** The throwaway commands run the server
     image (`launchpad-api:<version>`). It is built locally by the deploy script
     and is not on a new host; no registry image is published (none is known
     today). So the runbook gives the exact `docker build` command of
     `server/Dockerfile` that `deploy.sh` uses, for the same version as the
     backups; ticket 12 checks the command against `deploy.sh`.
   - `--backup-resume` makes a new `install_id` (#37), so `--remote NAME` looks for the NEW prefix.
     Older copies are restored with `--remote-config FILE --install-id <old id>` (the old id is in
     the manifests, the admin's records, or the file names).
   - A restore does not restore the destination settings into a running server:
     after a restore the admin re-enters the secrets if the key differs (T9 #10),
     and runs `deploy.sh` to re-apply the `offsite` role grants (#15).
40. **Size and timeouts (sizes are NOT measured on a real install).**
   - Dumps are `pg_dump -Fc --compress=6`. The production dump size is not
     measured in this note; ticket 5 records it in the runbook. The T7a restore
     test uses a seeded database of 6,000 comments, not a real one.
   - **v1 limits, decided:** S3 uses a single `PUT`, so an object above 5 GiB
     (the S3 limit for one request) fails `too_large`. There is no multipart in
     v1. A multipart ticket is added only if a real dump nears the limit.
     WebDAV: the server's own limit applies; Nextcloud's PHP and web server
     limits may be lower than 5 GiB (not measured), and its chunked upload API is
     a later change.
     SFTP: none in the protocol. Dropbox: upload sessions (#9).
   - Timeouts: connect 15 s; TLS and first byte 30 s; no progress for 120 s is
     `stalled`; a whole run is limited to 4 hours. One run at a time per
     destination; at most 2 destinations at once (a guess, for the scratch space
     and the uplink).
   - Memory: the encrypted file is streamed from disk; a driver must not hold a
     dump in memory (a test checks the peak with a large sparse file).
41. **Failure reporting and notices.**
   - **The Backups page** shows, per destination, the last run (time, status, size,
     code and safe text) and a runs table (last 50, filter by destination).
   - **Notices are sent by the APP** (it owns `login.MailSwitch`; `offsite` has no
     mail key and no mail code). A job in the app, every minute, takes unnotified
     rows with `FOR UPDATE SKIP LOCKED` and mails every active platform admin who
     has an email. Two kinds, two rules:
     - **Run notices are at-most-once.** The job sets `notified_at` in the
       transaction and sends after the commit. A crash between the two loses that
       mail. Chosen because a duplicate "failed" mail every minute is worse than a
       missed one, and the page, the stale alert and the run table still show the
       failure.
     - **Change-log notices are at-least-once.** A new destination or recipient
       must not go unannounced, so the job sends first and sets `notified_at`
       after; a crash can send the same mail twice. A send that fails is retried on
       the next tick.
   - `interrupted` and `canceled` runs send no notice.
   - A run needs a mail only when its trigger is `schedule`, `manual`, `catchup`,
     `retry` or `verify` (not `test`, `probe` or `exchange`, which the admin watches), and it
     is `failed` with `will_retry = false` and `notify_on_failure`, or `succeeded`
     and `notify_on_success`. A change-log row (#27) always mails.
   - The mail is plain text: destination name, time, status, code, safe text and a
     link to the page (needs `PUBLIC_BASE_URL`; without it, no link). No dump data,
     no host, no URL, no secret.
   - **Stale alert.** `stale_alert_hours = 0` means no stale alerts at all (checked first). Otherwise a
     destination alerts when its last SUCCESS is older than
     `max(stale_alert_hours, 2 x its schedule interval)`, the interval being 24
     hours for `daily` and 168 hours for `weekly` (two missed slots; a weekly
     destination never alerts at 48 hours). Only runs with trigger `schedule`,
     `catchup`, `retry` or `manual` that `succeeded` count: `test`, `probe`, `verify` and
     `exchange` do not. A destination that never succeeded counts from `max(created_at, enabled_since)`, as in #31. A disabled destination never alerts. One alert per destination per
     24 hours, tracked in `last_stale_alert_at`. With mail off (provider `none`)
     the page shows "alerts are not sent: mail is off".
   - **The local backup too:** the page shows the newest `backup_dumps` row and a
     warning when no dump is `ok` in 36 hours, so a stopped `backup` service shows
     even with no destination.
   - Why not the T13 inbox: T13 is per org and per user and is built separately.
42. **Verification of remote copies.** A destination with `verify_every_days`
   above 0 gets a `verify` run on that interval: `offsite` downloads the newest
   object and checks its sha256 against `backup_runs.sha256` (the DATABASE, not
   the remote manifest, #22) and that the file starts with the age header.
   This proves the object is whole and the remote did not change it. It does NOT
   prove it decrypts: the server has no private key (#19). The full test,
   a restore into a scratch database, is `restore.sh --remote` by an admin who
   holds the key; the runbook says to do it after setup and every quarter. A
   verify costs one download per interval, which some providers charge for; the
   default is 7 days and 0 turns it off.

## API and web

43. **Routes (all behind `auth.RequirePlatformAdmin`, outside `/orgs/{org}`, in
   `router.go` AND `openapi.yaml`, tag `platform`).**

   | Route | Purpose |
   |---|---|
   | `GET`, `PUT /api/v1/platform/backups/settings` | the signing public key (read only), notices. `PUT` takes the whole notice object (T9 #14). |
   | `GET`, `POST .../recipients`; `DELETE .../recipients/{fingerprint}`; `POST .../recipients/{fingerprint}/cancel` | list; add (pending 24 hours); schedule a removal; cancel a pending add or removal (#20). |
   | `GET`, `POST .../destinations` | list and create. |
   | `GET`, `PATCH`, `DELETE .../destinations/{id}` | read, edit (#25), delete (runs keep `destination_name`). |
   | `POST .../destinations/{id}/probe`, `POST .../destinations/probe` | phase 1 of #36. |
   | `POST .../destinations/{id}/exchange` | Dropbox: the authorization code, queues an `exchange` run; also the retry after a failed exchange (#9). `idempotency.Reject`. |
   | `POST .../destinations/{id}/test`, `POST .../destinations/test` | phase 2; the second tests unsaved config. The body may carry `confirmedFingerprint` (#36). |
   | `POST .../destinations/{id}/run` | "Back up now" (`202`, a run id; `409 destination_busy`, #31). |
   | `GET .../runs`, `GET .../runs/{id}` | the run table with a cursor; a test run returns its steps. |
   | `GET .../changes` | the change log. |
   | `GET .../status` | the newest dump, the local state, recipients, `supportedTypes`, whether `BACKUP_ENCRYPTION_KEY` is set, whether the stored secrets and the signing key open, mail state, banners. Built in ticket 2 with the data that exists then; ticket 8 adds the stale and mail-off fields. |

   There is no route to replace the signing key (#22). Every write has
   `idempotency.Reject`. A cookie write needs the CSRF header. Rate limit for
   probe, test, exchange and run: 6 a minute (burst 3) per process (T9 #16; the numbers are
   a guess, not measured). A platform admin token needs the admin scope and no org
   pin (T8 #15). No event is emitted (#26). New error codes
   (`backup_unavailable`, `destination_busy`) go in `httpx.Codes` and the
   `ErrorCode` enum.
44. **The page: Settings, Platform, Backups** (`web/src/features/settings`).
   - A status band: newest dump (local), banners from `status`.
   - Encryption: the public keys, the signing public key (to copy and keep off
     the host), a short "how to make a key" text with `deploy.sh --backup-keygen`,
     the lost-key warning, and a required tick "I have saved the private key and
     the signing public key" before the first destination can be enabled. "Enable" stays
     disabled until the signing public key is shown (it exists after the `offsite` boot,
     #22), and the API refuses to enable while it is empty. The tick is UI only; the API
     enforces just a non-empty set of active recipients.
   - Destinations: a list (name, type, schedule, keep, last run); add and edit in a
     dialog per type; secret fields are write-only (`Set`, `Replace`, `Clear`, as
     the mail form); a pin or host key field filled from the probe, with the
     fingerprint shown for confirmation; Probe, Test (steps shown); Back up
     now; enable and disable; delete with a confirm. A note that private
     ranges need the operator setting (#35).
   - A short notice in the page copy and in the stale banner: a clone of this host's VM or volumes copies
     the instance guard (#37), so after any clone run `deploy.sh --backup-resume` before backups run there.
   - Runs: the table; a row opens its detail. Changes: the change log.
   - Only a platform admin sees the page.
   - Web tests: secret fields never show a value; the probe and test flow; the
     key gate; the permission gate.

## Testing without cloud accounts

45. **Fakes, as `internal/login/smtptest` is for SMTP.** A package
   `internal/backup/backuptest` (a test package) with in-process servers and
   failure injection (a slow body, a reset, a 507, a 401, a redirect, a changed
   host key):
   - WebDAV: `net/http/httptest` serving `PUT`, `MKCOL`, `PROPFIND`, `DELETE`, with
     Basic auth, over TLS with a test certificate (for the pin tests).
   - S3: the real silo that the integration lane runs
     (`internal/storage/storagetest`); `httptest` for failures silo cannot make.
     Under `INTEGRATION=1` a missing silo fails, as there.
   - SFTP: an in-process SSH server (`x/crypto/ssh`) with the `pkg/sftp` server on
     a temporary directory: key auth, password auth, a changed host key.
   - Dropbox: an `httptest` server for the token endpoint and the upload session.
   - Also: an age round trip (and with the `age` command where present), the SSRF
     table (#34), the retention test (pairs, orphans, foreign and other-install
     files), the signed manifest and its failures, and an end-to-end test on
     Postgres (a dump row, a destination, a run, a notice) with `-race`.
   - `make deploy-test` and `make restore-test` are extended: an encrypted,
     signed round trip through `restore.sh --remote` against a fake remote.
   - **What fakes do not prove:** real Nextcloud, Dropbox, B2 and SFTP servers
     differ in limits and errors. Each driver ticket ends with a manual check by
     the project owner against one real account (a checklist in the ticket), and the runbook
     says which services were tried. Until then they are "unmeasured".

## Deploy and docs

46. **Compose and deploy.** `deploy/docker-compose.yml` gets the service
   `offsite` with `LOG_ENV: production` (fixed, as for `app`) and `DATABASE_URL` of the
   restricted role (#15), and `command: ["/bin/server","offsite"]`: the image has no ENTRYPOINT and its
   CMD is `["/bin/server"]`; `check-compose.py` fails a service with neither `build` nor a
   pinned `@sha256` image, so `offsite` carries `build: ../server` and the SAME `image:
   launchpad-api:${DEPLOY_VERSION:-unversioned}` as `app`, and the one `dc build` builds
   the image once for both), the volume `offsite_work` (same label block), and the networks
   `offsite_db` and `egress` (#13); `postgres` joins `offsite_db`. `deploy.sh`
   follows the start order of #15 (`deploy.sh` starts everything except `offsite` with `up -d`; after the app is healthy and migrated it creates the role and applies the grants, then starts `offsite` and waits for its health check. The health check is `/bin/server offsite healthcheck`, which only reports that the
   process is alive, so a missing role cannot deadlock the first deploy). The grant SQL is
   one file, `deploy/backup/offsite-grants.sql`, used by `deploy.sh` and by the ticket 5
   test, which runs it against a test database, generates
   `BACKUP_ENCRYPTION_KEY` (#12) and the role password (#15) (both topped up by `ensure_env` / `env_topup` like the other
   generated values; the password is not bound to a volume, so it is not an `env_bad` case
   like the postgres password: the grant step sets it again at every run), passes
   `BACKUP_ALLOW_PRIVATE_CIDRS` (#35), and prints the state in the summary.
   `make compose-check` asserts for `offsite`: no Docker socket, no published
   port, `backups` mounted read-only, networks exactly `offsite_db` and `egress`,
   `cap_drop: [ALL]`, `no-new-privileges`, read-only root with `tmpfs: [/tmp]`,
   `user: 70:70`, `depends_on` `volume-init` (completed), a pinned image, **no `POSTMARK_*`, no `SETTINGS_ENCRYPTION_KEY`, no `AUTH_OTP_PEPPER`
   and no `S3_*`** (ticket 14 adds `S3_*` and `files_net` to the allowed set, and
   only then), and that `offsite_db` is `internal` and has only `postgres` and
   `offsite`. `deploy/test-compose-check.sh` mutates each rule to prove it fails.
   `volume-init` also chowns `offsite_work` to uid 70. The new volume must also be
   added wherever the deploy code lists the stack's volumes: the `guard_project`
   check in `deploy/lib.sh` (today `postgres_data`, `silo_data`, `backups`), the
   `--nuke` volume list and its "all three volumes" messages, the `--nuke` cases of
   `make deploy-test`, and `deploy/check-compose.py` (its `read_only` rule for
   `app`, `web` and `backup`, its non-root rule and its allowed bind mounts, which
   must now cover `offsite`). Ticket 5 does all of it. The role and grant step runs on
   every `deploy.sh` run and update (#15).
47. **Runbook:** `docs/deploy.md` gets "Scheduled off-site backups" (set up, the
   keys, each destination with its credential advice, the operator CIDR setting,
   restore from a remote copy on a new host, key rotation, the Dropbox app steps,
   what to do when a destination is stuck). The manual `docker run` copy recipe
   stays as the by-hand fallback. `AGENTS.md` gets a short section like the
   others, added by ticket 5 (the first ticket that changes the deploy); ticket 12 adds the
   runbook.
48. **Upgrades and start order.** An install without the new tables keeps
   working: `backup.sh` skips recording when a table is missing (#28). The first
   `--update` past this change: pulls and builds; recreates `postgres` so it joins
   `offsite_db` (`up -d postgres`; `--no-deps` on `offsite` alone would leave it
   unable to reach the database; this restarts the database, a short downtime that
   is not measured, and the compose volume-recreate warning in `docs/deploy.md`
   still applies); then, by the start order of #15 (`deploy.sh` starts everything except `offsite` with `up -d`; after the app is healthy and migrated it creates the role and applies the grants, then starts `offsite` and waits for its health check.) the role and grant step also runs on every run and update, not only here. `offsite` waits for the role and for the migration. An install
   without `BACKUP_ENCRYPTION_KEY` starts the app (#12); `deploy.sh` adds one.
   **Rollback and swap.** `docs/deploy.md` "Rollback" stops only `app backup`, then runs
   `ALTER DATABASE ... RENAME`, which fails while `offsite` holds its `LISTEN` connection
   (#30). Ticket 12 edits both Rollback blocks and the new-host swap steps: stop `app backup
   offsite` first; after the swap restart in this order: `deploy.sh` (the start order of #15, which
   re-applies the grants), then `deploy.sh --backup-resume` (#37). After a rollback to a commit
   from before this design, `--remove-orphans` removes the `offsite` container; the networks
   `offsite_db` and `egress` and the volume `offsite_work` stay, and the runbook says so and how to
   remove them. Ticket 5 scopes every `dc up` in `deploy.sh` to named services: a bare `dc up -d
   --remove-orphans` would start `offsite` before the grants exist.

## Open points (proposed defaults; the project owner decides)

1. SMTP is not a destination. Was SFTP meant? Default: SFTP is in (ticket 9).
2. Targets: S3 and WebDAV first, then SFTP, then Dropbox. FTP is cut and FTPS is
   not built. Is it acceptable that FTP is cut and Dropbox comes after the first
   release? Default: yes.
3. Attachments: after the database path (tickets 14 and 15). Default: yes.
4. Encryption is required and public-key (age). A passphrase would let a stolen
   host read the remote copies. Default: public key.
5. S3 targets include Backblaze and any S3 service as a user's choice. Default: yes.
6. UTC schedules only in v1. Default: yes.
7. Retry policy and stale threshold. Default: one retry after 30 minutes for
   transient errors; the stale alert at `max(48 hours, 2 schedule intervals)`.
8. Private-range reach as an operator setting (`BACKUP_ALLOW_PRIVATE_CIDRS`)
   instead of a UI tick. Default: operator setting. A LAN Nextcloud then needs one
   line in `deploy/.env`.
9. May `offsite` hold the settings master key? Default: no. A separate
   `BACKUP_ENCRYPTION_KEY` and a restricted database role.
10. A 24 hour hold on adding or removing an age recipient, with a notice to all
    platform admins and a cancel by any of them (#20). Alternatives: a
    second admin's confirmation (needs two admins; many installs have one), or an
    operator-side flag so that recipients can be changed only on the host. Default:
    the 24 hour hold.

## Ticket split (build order)

Each ticket is one MR. All server tickets run `make docker-test` and, with a
database, `make docker-test-integration`; schema tickets also `make sqlc-check
schema-check` and `TestMigrateUpDownUp`. This note (LAUN-28) is not in the list;
the tickets are numbered from 1.

1. **Backups: schema, migration, sqlc and the sealing code**
   - Scope: migration `NNNN_backups` (+ down) for the six tables in #23
     (`backup_settings` with `install_id`, `recipients`, the signing key columns
     and `key_check`; `backup_destinations`; `backup_dumps`; `backup_runs` with
     `destination_name`, nullable `destination_id` and `dump_id`, `sha256`,
     `test_config`, `test_secrets_enc`, the status list; `backup_requests`;
     `backup_change_log`); indexes for the run list and the job scans; sqlc queries;
     seal and open for the three additional-data forms (#24); optional
     `BACKUP_ENCRYPTION_KEY` config parsing (#12).
   - Depends on: this note merged.
   - Acceptance: up/down/up passes; the single-row table rejects a second row;
     CHECKs reject unknown `type`, `status`, `trigger`, `schedule`, `keep_last`
     outside 1 to 365; a blob sealed for one row does not open on another
     (destination id, run id, `settings`); a wrong key reads as unreadable, not a
     crash; a boot without the key succeeds; deleting a destination keeps its runs
     with `destination_name`; `db/schema.sql` and `internal/dbq` are current.
   - Also in scope: the trigger of #22 on the signing columns (allowed only from the
     empty state, or with a `signing_key_replaced` change-log row in the same
     transaction); `backup_runs.dump_id` is `ON DELETE SET NULL`.
   - Also acceptance: an UPDATE of the signing columns from a non-empty key without a
     change-log row fails; blanking `signing_public` fails; the first-use
     compare-and-set changes one row for two concurrent writers.
   - Also in scope: the columns, defaults, NULL rules and CHECKs of #23 (`probe` in the
     trigger CHECK, `ever_had_recipient`, `removeBy`, `instance_id`, the request and dump
     columns); the `SECURITY DEFINER` signing trigger with its `search_path` (#22); the
     keyring API: `Keyring.Seal` and `Open` take only a column name today, so this
     ticket extends them to take the column AND the owner id (#24) without changing T9's
     callers.
   - Also acceptance: `key_check` accepts NULL and rejects a non-32-byte value; the weekday
     CHECK holds in both directions; a final run needs `finished_at`; T9's mail settings
     still seal and open (their tests pass).
   - Also in scope: `backup_dumps.request_id` and `backup_requests.dump_id` point at each other, so
     the migration creates the tables first and adds ONE of the two foreign keys with `ALTER
     TABLE` afterwards; the down drops that key before it drops the tables, in order;
     `backup_runs.request_id` and `enabled_since`; the trigger is `UPDATE OF
     signing_public, signing_private_enc`; the `BACKUP_ENCRYPTION_KEY` dev-key and all-zero
     refusal outside `LOG_ENV` local or test (T9 #7, #12).
   - Also acceptance: an update of `key_check` alone does not fire the signing trigger;
     a `waiting_dump` run can name its request; the dev key is refused when `LOG_ENV` is
     production.
   - Also in scope: the `key_check` replacement of #12 (a mismatching non-NULL value is replaced
     on the first successful seal under the current key, in the same transaction).
   - Also acceptance: after the key is changed, re-entering ONE destination's secrets makes that
     destination readable, replaces `key_check`, and leaves the others unreadable; the signing key
     stays unreadable until `rotate-signing`.
   - Also in scope: the dev compose file gets a `BACKUP_ENCRYPTION_KEY` default, public and accepted only
     with `LOG_ENV` local or test (as T9 #7).
   - Also in scope: the partial unique index of #31 on `backup_runs (destination_id)` for the four backup
     triggers in the three active statuses.
   - Also acceptance: two concurrent inserts of backup runs for one destination leave one row; a `retry` row can
     be inserted when its failed parent exists; probe, test and exchange runs are not limited by it.

2. **Backups API: settings, recipients, destinations, runs, changes, status**
   - Scope: #43; validation per type; write-only secrets and the no-host-change
     rule (#25); X25519-only recipient validation with the age parser
     (ticket 3); the 24 hour recipient hold, cancel and the non-empty rule
     (#20); the change-log writes (#27); the notify wake (#30);
     the no-event exception (#26); the busy and supersede rules of #31;
     `router.go` and `openapi.yaml`; error codes; rate limits. Probe, test, run and
     exchange only queue rows here. The status route with the data that exists now.
   - Depends on: 1, 3.
   - Acceptance: platform-admin gate matrix (admin session, admin token with and
     without an org pin, org admin, member, agent); secrets never returned and never
     in the log, events, idempotency rows, responses, run rows or change log (the
     planted-secret test, #26); a host change with a kept secret is `400`; an
     added recipient is not active for 24 hours (an injected clock) and a cancel
     keeps it out; a removal that would leave no active recipient is `409`; a
     destination cannot be enabled without an active recipient; `409` without
     `BACKUP_ENCRYPTION_KEY`; `POST run` is `409 destination_busy` during a running
     run and cancels a queued retry; every listed change writes one change-log row;
     `TestOpenAPIMatchesRoutes` and `TestOpenAPISchemasMatchModels` pass.
   - Also in scope: the API refuses to enable a destination while `signing_public` is empty
     (a test with a signing-key fixture), without an active recipient, or with an unreadable
     secret (`409 conflict`, a state the write would create). **One error rule:** `409
     conflict` is for a write that would put the configuration in an invalid state (a secret
     saved without `BACKUP_ENCRYPTION_KEY`, #12; enabling without a recipient or a signing
     key); `503 backup_unavailable` is for an action that cannot be started now (probe, test,
     run or exchange without the key or with a destination type this build does not run yet).
     A `dropbox` destination can be created only when the build has the driver of ticket 10;
     before it, creating one answers `409 conflict` (the write would create a destination this build
     cannot run; `status` lists `supportedTypes`, which comes from the driver registry of ticket 3).
     Probe, test, run and exchange on such a destination answer `503 backup_unavailable`. Enabling a
     `dropbox` destination requires a stored refresh token (`secrets_set` has it). Validation includes
     the cap of 5 live recipients (#19).
   - Also in scope: `POST .../destinations/{id}/exchange` (#9, #43); the unique
     (type, host, bucket, folder) rule (#25); the cancel rules of #20 (a pending
     removal only by a different admin); `server backup-recipient-remove`; the add
     rule (immediate only when `ever_had_recipient` is false). The non-empty (`409`) rule applies to
     cancels too: cancelling a pending add can empty the active set. On a stolen sole-admin
     session the order is: cancel the removal on the host first, then the add.
   - Also acceptance: a disabled destination does not make an add immediate; the
     admin who scheduled a removal cannot cancel it, another admin can, and the
     actor can cancel their own add; `server backup-recipient-remove` removes at
     once, writes a change-log row and refuses to empty the set while a destination
     is enabled; two destinations with one type, host, bucket and folder are `409`;
     a `PATCH` of any `config` field, the pin or the host key with a kept secret is
     `400`; the exchange route is listed in `router.go` and `openapi.yaml`.
   - Also in scope: the full-fingerprint route key and the short-form collision rule (#20);
     `ever_had_recipient`; `removeBy`; the pending-add DELETE as cancel; `server
     backup-recipient-cancel`; the `confirmLowKeep` floor and a change-log row for every
     destination field change (#27); the `confirmedFingerprint` body field (#36); the
     rate limit on the exchange route.
   - Also acceptance: emptying the recipient set does not make the next add immediate;
     DELETE on a pending add cancels it; `server backup-recipient-cancel` cancels a pending
     removal; `keep_last` 1 without `confirmLowKeep` is `400`; every destination field change
     writes a change-log row; a probe or test with `confirmedFingerprint` saves the pin or
     host key (only when none exists; otherwise `400` and the secret must be re-entered) and
     writes a change-log row before the run is queued.
   - Also in scope: ANY type absent from `supportedTypes` (not only `dropbox`) is `409 conflict` on create;
     enabling checks that the signing key opens (else `409`).
   - Also acceptance: enabling a destination without `BACKUP_ENCRYPTION_KEY` is `409`, and probe, test, run
     and exchange without it are `503 backup_unavailable`; the tests register fake drivers under real type
     names with the `backuptest` tag (#16).

3. **Backup library: SSRF guard, driver interface, age, signed manifest, names,
   retention filter**
   - Scope (LAUN-1 note: the guard code, the operator CIDR parser (#35) and their tests are in `internal/netguard`, T17 ticket 2; this ticket imports it and keeps only the backup policy and the silo dialer; **the SSRF test table of #34 and the CIDR-parser tests run in `internal/netguard` (T17 ticket 2), so this ticket's AC for them is satisfied there; it keeps only the backup-policy rows (operator list, silo dialer)**): `internal/backup`: the dial-time guard with the full list (#34), the
     operator CIDR parsing and validation (#35) and the dedicated silo dialer
     type; the `Proxy: nil` no-redirect HTTP helper; the guarded SFTP dial helper;
     the driver interface (#16); age encryption with `filippo.io/age` (X25519
     only); the Ed25519 manifest sign and verify; the name format and the retention
     filter and pair logic (#31, #33); the local-directory driver behind
     the `backuptest` build tag; the new dependency and its `go.mod` diff reported.
   - Depends on: none, or T17 ticket 2 (`internal/netguard`) if it has landed.
   - Acceptance: an age round trip (and with the `age` command where present); the
     SSRF table of #34 (every refused range and form, each metadata address
     inside an operator range, service names, rebinding, `http_proxy`, a redirect to
     a private address, an SFTP host resolving to loopback); an operator list with
     `/0`, a loopback overlap or a `/4` is rejected at parse time; the retention test
     (pairs only, orphans, foreign and other-install files stay, a failed run deletes
     nothing); a manifest with a bad signature or a changed sha is rejected; the
     local driver is absent from the normal build (`go build` of `cmd/server` has no
     reference to it).
   - Also in scope: `-tags backuptest` added to the Makefile `vet`, `test` and integration
     targets (and their docker forms) so the local driver's tests build, and a check
     that `go build ./cmd/server` WITHOUT the tag has no reference to it; the reply-body
     size caps of #16.
   - Also acceptance: the check fails when the tag-only file is imported by a normal file;
     a list reply above the cap is refused.
   - Also in scope: a driver registry (type name to driver) that answers the set of supported types for
     the API's `supportedTypes` and the unsupported-type refusals of ticket 2.
   - Also acceptance: a type with no registered driver is absent from `supportedTypes`.
   - Also in scope: `Validate` and `Identity` on the driver interface and `Register` (#16); the `backuptest`
     registration of a fake driver under a real type name.

4. **Backup runner: the `offsite` worker**
   - Scope: `server offsite` and its subcommands `keygen` (age pair only),
     `rotate-signing`, `fetch` (length-prefixed frame), `decrypt`, `healthcheck`; the worker
     loop on a LISTEN connection with a 30 second scan (#30); the advisory lock on
     a dedicated connection and the lease (#31); catch-up, retry and
     `waiting_dump`; the first-use signing key (#22); the dump sha check; scratch
     handling (#18); the probe and test phases (#36); the sealed
     unsaved-test secrets and their deletion; the verify run (#42).
   - Depends on: 1, 3.
   - Acceptance: two workers never run one destination at once (a test with two
     pools, and one that proves the lock survives a pool checkout and return); a
     killed run is `interrupted` at boot; a missed slot makes one catch-up run; a
     transient failure is retried once after 30 minutes and each permanent code of
     #31 is not (a table test, including a 3xx and `not_found`); a `waiting_dump`
     run fails `dump_unavailable` after 30 minutes; a changed dump file fails
     `dump_mismatch`; `no_space` is reported before work; a 2 GiB sparse file uses
     bounded memory; `test_secrets_enc` is empty after a success, a failure and a
     boot, and the planted-secret grep covers it; every error code is safe (a
     planted secret in a fake server's reply never appears); an unreadable signing
     key fails `secrets_unreadable` and makes no new key.
   - Also in scope: `server offsite verify` (Ed25519 signature and sha256, no network,
     no database), `fetch --config FILE` and `--allow-cidr`, `unpack` (#39); the
     compare-and-set first use and the refusal to make a key when the columns are
     blank or half-empty (#22); `SELECT ... FOR UPDATE` merging of `secrets_enc` (#15).
   - Also acceptance: `verify` rejects a bad signature, a changed object and a changed
     manifest; `unpack` rejects every hostile frame of #39; a blanked `signing_public`
     never makes a new key; a Dropbox refresh and an app `PATCH` do not lose each
     other's write (two writers).
   - Also in scope: the `instance-id` file and the `instance_changed` rule (#37); the
     `probe` trigger and its `steps` result (#36); `clock_skew` mapping; database-clock
     slots and leases (#31); the free-space rule for two concurrent runs and the best-effort
     delete of a partial remote object (#18); `offsite` reads its own small config (no
     `AUTH_OTP_PEPPER`, no `S3_*`); the one-pass sha256 of the dump (#22).
   - Also acceptance: a cloned database with a fresh volume runs nothing and reports
     `instance_changed`; a skewed clock shows `clock_skew` and is not retried; two concurrent
     runs on a volume sized for one fail `no_space` for the second; `offsite` starts with no
     `AUTH_OTP_PEPPER`; a changed dump file fails `dump_mismatch` before upload with the
     scratch file deleted.
   - Also in scope (their acceptance tests are in ticket 5, which creates the restricted role, because
     they need that role as a fixture): `offsite` never uses `INSERT ... RETURNING` on `backup_change_log` (it has no
     SELECT right; the id is made in the client); it never inserts a second dump request while one
     is `queued` or `taken` (two destinations share one extra dump, each run stores the existing
     `request_id`); `server offsite resume --instance-id ID` (#37, writes `/offsite-work/instance-id`);
     the claim and cancel statements of #31; `scratch_full`; `command` form `/bin/server
     offsite ...`; no immediate catch-up before `max(created_at, enabled_since)`.
   - Also acceptance: `resume` in a container with only `offsite_work` mounted, run as the non-root
     uid 70, makes the guard pass; a queued retry cancelled while claimed leaves one run and a `409`; ENOSPC
     in the middle of encryption gives `scratch_full`, not retried and the scratch file
     removed; a newly created destination makes no run for an earlier slot.
   - Also in scope: the claim order of #31 (backup runs: pick a destination, try-lock, claim a row filtered by it; `test`, `probe`, `exchange`, `verify` and unsaved runs claimed without the lock) and the lock handover: released on `queued` to `waiting_dump`, taken again before `waiting_dump` to `running`, so lock-then-claim is not applied to a run already in `waiting_dump`.
   - Also acceptance: a `test` run is claimed and finishes while a backup run of the same destination holds the lock; a run in `waiting_dump` gets the lock only when it moves to `running`; a dump that started and ended `failed` makes the waiting run fail `dump_unavailable` at once.

5. **Compose, `backup.sh`, keys and the database role**
   - Scope: #46, #28, #15, #12, #14; `backup.sh` records
     `backup_dumps` and takes `backup_requests` with a 30 second loop, without
     changing the daily time or `BACKUP_KEEP_DAYS`; `volume-init` chowns
     `offsite_work`; `deploy.sh --backup-keygen` and `--backup-rotate-signing`
     (terminal only); the start order of #15 (everything but `offsite` first; after the app is healthy, the role
     and grants; then `offsite`); `BACKUP_ENCRYPTION_KEY` and the role password
     generated; `BACKUP_ALLOW_PRIVATE_CIDRS` passed through and validated; the
     compose-check rules and their mutation tests; measurements: the uid 70 run of
     the server binary, the 30 second loop cost, the real dump size, the scratch
     size, whether `offsite` reaches `app` and `backup` (it must not).
   - Depends on: 1, 2, 4 (the recipient commands live in ticket 2).
   - Acceptance: `make compose-check deploy-test deploy-lint restore-test` pass;
     `backup.sh` still makes and prunes dumps when the new tables are missing; a
     request row makes a dump within 60 seconds; no request value reaches a shell
     command (a test with a hostile id); `file_removed_at` is set on prune; the role
     can do exactly the grants in #15 and cannot read `issues`, `users` or
     `platform_settings` (a test); `offsite` starts after the role exists and waits
     when it does not; `offsite` has no socket, no published port, `backups`
     read-only, none of the forbidden variables; a re-run of `deploy.sh` keeps the
     generated secrets; measurements are in the runbook.
   - Also in scope: the role and grant step on every run and update (#15); recreating
     `postgres` on the first update (#48); `deploy.sh --backup-recipient-remove` (#20).
   - Also acceptance: after a `restore.sh` style restore (no privileges) a `deploy.sh`
     run restores the grants; a dropped grant comes back on a re-run; the compose-check
     rules include `tmpfs` `/tmp` and `depends_on` `volume-init` (#46); the first
     update past this change recreates `postgres` and `offsite` then reaches it.
   - Also in scope: `deploy.sh --backup-recipient-cancel` and `--backup-resume`; `backup.sh
     once` also records `backup_dumps`; the `offsite_work` volume in `lib.sh` `guard_project`,
     the `--nuke` list and messages, the `deploy-test` `--nuke` cases and `check-compose.py`
     (#46); `--log-driver none` on every throwaway container that prints a plaintext dump or
     a private key (#39); `psql` SQL on standard input, not `-c` (#28); measuring, with the two
     networks `offsite_db` and `egress`, which interface holds the container's default route
     (`/proc/net/route`) and that the gateway rule of #34 reads the `egress` one.
   - Also acceptance: the `--nuke` cases remove `offsite_work` and refuse a project whose
     volumes lack the label; `check-compose.py` fails when `offsite` loses `read_only`, runs as
     root or has an extra bind mount (mutation tests); a hostile value in a request or dump
     row reaches no shell and no SQL string; the deploy tests assert `--log-driver none`;
     the measured default-route interface is recorded in the runbook.
   - Also in scope: `offsite` with `LOG_ENV: production` and `DATABASE_URL` (the restricted
     role) set, and `check-compose.py`'s `LOG_ENV` check extended from `app` to `offsite`; `build: ../server`, the shared image tag and
     `command: ["/bin/server","offsite"]` (#46); `deploy/backup/offsite-grants.sql`; the
     `deploy.sh --backup-resume` of #37 (new `install_id`); `rotate-signing` through `docker
     exec -T`; the offsite password in `ensure_env` / `env_topup`; the unfinished-`running`
     dump cleanup in `backup.sh` (#29).
   - Also acceptance: the grants file applies twice without error and gives exactly the grants
     of #15; `check-compose.py` accepts `offsite` with `build` and the shared tag; a first
     deploy does not wait on `offsite` before the grant step; a killed `backup.sh` leaves no
     `running` dump after the next run.
   - Also acceptance (moved from ticket 4): with the restricted role, `offsite` inserts a change-log row
     without `RETURNING`; two destinations that both need an extra dump make ONE request.
   - Also in scope: every `dc up` is scoped to named services; `--update` prints a line before it
     recreates `postgres`, saying the database restarts briefly (#48).
   - Also acceptance: no `deploy.sh` path runs a bare `dc up -d --remove-orphans`; the deploy tests assert
     the `--user` of each throwaway command (#39).
   - Also in scope: the Dockerfile fallback if uid 70 cannot run `/bin/server` (a `chmod 0755 /bin/server`
     there); a stub "Scheduled off-site backups" section in `docs/deploy.md` (the variables and the start
     order; ticket 12 fills it).

6. **S3-compatible driver**
   - Scope: a driver on the standard library and the SigV4 signer (extend
     `internal/storage` or share its signer; add list; path-style and virtual-hosted
     style as an option); `Probe`, `Test`, `Put` (single PUT; above 5 GiB is
     `too_large`, #40), `List`, `Get`, `Delete`; SPKI pin.
   - Depends on: 3, 4.
   - Acceptance: against the real silo (integration) a run uploads, lists, prunes and
     fetches; failures on `httptest` (403, 507, a redirect, a stall, a reset); an
     object above 5 GiB is `too_large` (a test with a faked size); a manual check on
     one real S3 service; `TestPresign_AWSExample` is untouched.

7. **WebDAV and Nextcloud driver**
   - Scope: `PUT`, `MKCOL` (each folder level), `PROPFIND` depth 1, `DELETE`; Basic
     auth over TLS only; a Nextcloud note (app password, the
     `remote.php/dav/files/<user>/` path) in the form help; SPKI pin; no redirects.
   - Depends on: 3, 4.
   - Acceptance: tests on the fake WebDAV (nested folders, 401, 403, 507, 423, a
     refused redirect, a changed certificate with a pin); a manual check on one real
     Nextcloud that states the upload limit it found.
   - Also in scope: `Get` and `Delete` URLs are built only from names that match the
     retention regex, never from a `PROPFIND` href (#33).
   - Also acceptance: a `PROPFIND` reply with a hostile href (`../`, another host, an
     encoded slash) makes no request to it.

8. **Backup notices, change notices, stale alert and the app's row cleanup**
   - Scope: the job of #41 with `login.MailSwitch` (run notices at-most-once,
     change-log notices at-least-once, which triggers mail), the stale rule and
     `last_stale_alert_at`, the status route's stale and mail-off fields, and the
     hourly row cleanup and `test_secrets_enc` sweep of #29.
   - Depends on: 1, 2 (it runs on rows; real runs need 4).
   - Cross-ticket note (LAUN-72): `instance_resumed` is a new `backup_change_log` kind (written by
     `deploy.sh --backup-resume`). The notice job must give it a sentence (`changeText`).
   - Acceptance: a failed run mails every active platform admin once, not twice, with
     two app instances (`SKIP LOCKED`); a run with `will_retry` mails nothing; a
     failed `test` or `exchange` run mails nothing; success mail only when set; every
     change-log kind mails ALL platform admins and a crash before `notified_at`
     resends it; a crash between commit and send loses a run notice and does not
     resend it; the stale alert once per 24 hours and by the schedule rule (a weekly
     destination: no alert at 48 hours, an alert after 2 missed slots; `test`,
     `verify` and `exchange` runs do not count); provider `none` sends nothing and
     the status says so; the mail text has no secret and no host (a planted-secret
     test); the cleanup deletes only rows past the ages in #29.

9. **SFTP driver**
   - Scope: `golang.org/x/crypto/ssh` and `github.com/pkg/sftp` (two new direct
     dependencies, diff reported); key and password auth; the host-key probe and
     confirm flow (#36); the private key as a sealed secret; dialing through the
     guarded dialer (#34).
   - Depends on: 3, 4.
   - Acceptance: tests on the in-process SSH and SFTP server (key auth, password
     auth, the probe sends no credentials, a changed host key is `host_key_changed`
     and never updates, a refused write, a full disk, a host resolving to loopback is
     refused); a manual check on one real server.

10. **Dropbox driver**
    - Scope: the `exchange` run (#9); the refresh token sealed by `offsite`
      (the column grant of ticket 5); the access token in memory; upload sessions
      above 150 MB; list and delete in the App folder; `Retry-After` handling.
    - Depends on: 3, 4, 5.
    - Acceptance: tests on the fake Dropbox (the exchange, a refresh, an expired token,
      a 429 with `Retry-After`, an upload session with a failed chunk resumed); the
      code is single use (a second exchange fails and the destination stays
      disabled); the refresh token and the code are never in a log or an error; a
      manual check on a real Dropbox app.

11. **Web: Settings, Platform, Backups**
    - Scope: #44; generated client from `openapi.yaml`; the probe, confirm and test
      flow; the recipient hold and cancel; the key gate; the permission gate; the
      runs and changes tables.
    - Depends on: 2 for the page; 8 for the stale and mail-off banners (or those banner tests
      wait for 8); 4, 5 and one driver (6) for probe and test to work;
      the keygen command text comes from 5.
    - Acceptance: the web tests of #44; `npm run lint` and the test lane pass; no
      secret value is ever rendered (a test); keyboard and screen-reader names on the
      dialogs.
   - Also in scope: the type picker offers only `supportedTypes` from the status route.

12. **`restore.sh --remote`, `--remote-config` and the runbook**
    - Scope: #39 and #47; the "Scheduled off-site backups" section of
      `docs/deploy.md`.
    - Depends on: 3, 4, 5 and 6 (the first driver); later drivers need no change.
    - Acceptance: `make restore-test` has an encrypted, signed round trip through a
      fake remote and the row counts match; a wrong age key fails before any restore
      starts; a bad signature, a wrong `--verify-key` and a sha mismatch each fail
      before decrypt; `--trust-remote` prints its warning; the age of the picked pair
      is printed; a restore on a host with an empty database and only
      `--remote-config` works on the default bridge network; the runbook states what
      was and was not tried on real accounts.
   - Also in scope: the usage of #39 (`--remote NAME` and `--remote-config FILE` in
     one line, `latest|STEM`); the `docker build` recipe for a new host (#39); the
     post-restore `deploy.sh` step (#15).
   - Also acceptance: the restore test cannot use the `backuptest` local driver (it is
     not in the production binary, #16): it runs against the real silo (integration)
     or an `httptest` S3 server bound to a non-loopback address of the test network (the always-refused
     loopback range cannot be listed, #34, #35) with that range given to `--allow-cidr`; `server offsite verify` runs in a
     `--network none` container.
   - Also in scope: `restore.sh --remote-config` skips `load_env`, `guard_project`,
     `env_topup` and `cid backup`, and needs `--container NAME` (a postgres container) or a
     `deploy.sh` stack (#39); `verify` binds the manifest's object name, dump time and
     `install_id` to the stem and destination; the freshness line uses the manifest time.
   - Also acceptance: with `--remote-config` and `--container`, a restore works on a host
     with no `deploy/.env` and no compose project; `verify` fails for a renamed pair, a
     wrong `install_id` and a changed dump time; `latest` and the freshness line print the
     manifest time.
   - Also in scope: a `cid offsite` lookup in `restore.sh` for `--remote NAME` (as it has `cid
     backup`); `--work-dir` and the host free-space check by `unpack` (#39); the
     `--trust-remote` warning text.
   - Also acceptance: `unpack` refuses when the work directory cannot hold the declared object
     (`no_space`); the `--trust-remote` warning names arbitrary SQL as the bootstrap superuser.
   - Also in scope: `restore.sh` takes a tests-only `--fetch-network NAME` so the throwaway fetch can join the
     test stack's network and reach the compose-internal silo (with `--allow-cidr` for that subnet).

13. **FTPS driver (not proposed now)**
    - Scope: explicit FTPS only, with TLS session resumption on the data connection; a
      library chosen then. Plain FTP is never built.
    - Depends on: 3, 4. Build only if the project owner asks.

14. **Attachment backup: incremental files manifest**
    - Scope: #38: list the silo bucket, upload new objects as age files, the
      encrypted and signed files manifest with `lastSeen`, "files not backed up"
      status; the `files_net` network, the read-only silo user, the `S3_*` variables
      and the dedicated silo dialer for `offsite`; the compose-check change.
    - Depends on: 3, 4, 5, 6 (or another usable driver), 12 (the restore check).
    - Acceptance: a second run uploads only new objects; the manifest lists every
      object; `offsite` cannot reach `app` or `web` (compose-check); the silo dialer
      connects only to the configured silo host and port (a test with another host);
      a restore of one object works with `restore.sh --remote`; nothing is deleted
      remotely; the first full size and time are recorded.

15. **Attachment restore path and files retention**
    - Scope: `restore.sh --remote --files` into a silo volume (the runbook's "never
      delete before known good" rule applies); the `lastSeen` prune of #38, which
      needs no database right; confirm in the code that deleting an attachment
      removes its silo object.
    - Depends on: 14.
    - Acceptance: a restore into a fresh silo volume matches the objects' sha256; the
      prune never deletes an object that is still in the silo listing, and never one
      that a kept dump could name (a test with a 40-day-old kept dump).

Build order: 1 and 3; then 2 and 4; then 5 (after 2 and 4); then the drivers 6 and 7; then 8; then
11 and 12; then 14 and 15. Tickets 9 and 10 come after the first release; 13 only on
request. The first useful release is tickets 1 to 8, 11 and 12: S3 and WebDAV, with
the page, the notices and a tested restore.

## Build notes: ticket 1 (LAUN-68): schema, queries and sealing code

Built: migration `0019_backups` (number re-checked at merge), `db/queries/backup.sql`,
`internal/platform/backup_crypto.go`, `BACKUP_ENCRYPTION_KEY` in `internal/config`,
the dev compose default. Judgment calls where the note was silent or two ways fit:

1. **`BackupKeyring` is a new type, not a change of `Keyring`.** Go has no overload, so
   "extend `Keyring.Seal` and `Open` to take the column AND the owner" cannot also keep T9's
   callers unchanged. `platform.BackupKeyring` (`Seal(column, owner, pt) ([]byte, error)`,
   `Open(column, owner, blob)`) sits beside `Keyring` and `GitKeyring`, as the git scope did.
   `Keyring` and its callers are untouched. `Seal` returns an error for a column other than
   the three (`BackupCol*`), an empty owner, `settings` as a row id, or a signing-key owner
   other than `settings`; `Open` reports any of those as `ErrUnreadable`.
2. **Additional data** is `<table>.<column>.<owner>`, for example
   `backup_destinations.secrets_enc.<destination id>`. HKDF info strings:
   `launchpad backup secret sealing v1` and `launchpad backup key check v1`. Never change them.
3. **`key_check` replacement** is `BackupKeyring.RecordBackupKeyCheck(ctx, q)`: one
   `UPDATE ... WHERE key_check IS DISTINCT FROM $1`. The caller passes the queries of the
   transaction that stores the seal, and calls it only after a seal under the current key.
   It changes no row when the check already matches. `BackupKeyMismatch(stored)` is the
   `keyMismatch` flag (NULL is not a mismatch). Nothing calls them yet: ticket 2 does.
4. **The signing trigger** (`backup_settings_signing_guard`, `BEFORE UPDATE OF signing_public,
   signing_private_enc`, `SECURITY DEFINER`, `search_path = pg_catalog, pg_temp`, names
   schema-qualified, `EXECUTE` revoked from `PUBLIC`):
   - a write that changes neither column is allowed;
   - the new pair must be full (`signing_public <> ''` AND `signing_private_enc IS NOT NULL`),
     also with a change-log row: a blank or half-empty key is always refused;
   - from the fully empty state a full pair is allowed (first use);
   - from any other state it needs a `backup_change_log` row of kind `signing_key_replaced`
     whose `xmin` is the current transaction's id (`pg_current_xact_id()::xid`). A row made
     in a SAVEPOINT of the same transaction has the sub-transaction's id and does NOT count:
     ticket 5 (`rotate-signing`) must insert the row and update the key without a savepoint.
   - A half-empty OLD row (only a superuser can make one) is treated as non-empty, so it
     needs the log row too.
   Errors are SQLSTATE `23514` (check_violation).
5. **Extra CHECKs beyond #23**, each from T9's precedent or a plain safety rule: sealed blobs
   are at least 28 bytes (nonce and tag); `sha256` columns are 64 lower-case hex characters;
   `bytes >= 0`; `attempt >= 1`; `recipients`, `steps` are JSON arrays and `config`, `detail`
   are JSON objects; `install_id` is 8 hex; `at_utc` is `HH:MM` 00:00 to 23:59; destination
   name 1 to 60 characters; `backup_change_log.kind` 1 to 64 characters (the list of kinds is
   open: later tickets add them); `locked_until` is NULL unless the status is `running` or
   `waiting_dump` (the note says "NULL unless"; a runner must clear it when it ends a run).
6. **`install_id` and `instance_id`** come from column defaults and one `INSERT` of the single
   row in the migration (same values as the note's expression).
7. **Circular keys.** `backup_dumps.request_id` is added with `ALTER TABLE` after
   `backup_requests` exists; the down drops it first. All `SET NULL` keys have a partial index
   on the referencing column so a delete does not scan.
8. **The busy index** is `backup_runs_one_active_idx` (#31). `InsertBackupRun` copies
   `destination_name` from the destination in the same statement; a run with no destination
   (an unsaved probe or test) gets `''`.
9. **`BACKUP_ENCRYPTION_KEY`** follows `SETTINGS_ENCRYPTION_KEY` exactly: optional, 64 hex
   characters, malformed fails the boot, the public dev key (hex of
   `launchpad-local-dev-backup-key-0`) and an all-zero key only with `LOG_ENV` local or test.
   `cmd/server` logs one INFO line when it is unset and builds the keyring once to prove the
   key is usable. The production compose file does not pass it yet (ticket 5).
10. **No event, no log** of any value: this ticket writes no route and no code path that
    emits. The table owner (`devops`) and the app are one role, so the trigger guards bugs,
    not a hostile app (#22).

## Build notes: ticket 3 (LAUN-70): the backup library

Built: `internal/backup` (policy, silo dialer, HTTP and SSH helpers, age, manifest, retention),
`internal/backup/driver` (interface, registry, safe errors, caps, the tagged local driver),
`internal/backup/remotename` (the name format), `scripts/check-backuptest.sh`, and
`-tags backuptest` in the Makefile. The SSRF guard and the CIDR parser are NOT here: they are
`internal/netguard` (LAUN-6, merged); their table runs there. Judgment calls where the note was
silent or two ways fit:

1. **`go.mod` diff.** New: `filippo.io/age v1.2.1` (direct) and `golang.org/x/sys v0.31.0`
   (indirect, age needs it). `golang.org/x/crypto` stays at `v0.36.0`: #17 asked whether tidy would
   move it; it does not (ticket 3 reports the diff: x/crypto only moves from indirect to direct,
   because `internal/backup` imports `x/crypto/ssh`). No other module changed. `go.sum` also gains lines for `golang.org/x/term` and `c2sp.org/CCTV/age` (age's dependencies; no `go.mod` change).
2. **The driver interface is split in two.** #16 lists `Probe`, `Test`, `Put`, `List`, `Get`,
   `Delete`, `Validate`, `Identity` on one interface, but a registered driver is one value per TYPE
   while `Put` needs one destination's config and secrets. So `Driver` (per type: `Validate`,
   `Identity`, `Probe`, `Open`) opens a `Remote` (per destination: `Test`, `Put`, `List`, `Get`,
   `Delete`, `Close`). `Config` carries the typed JSON, the opened secrets, the guarded `*netguard.Dialer`
   and `AllowPlainHTTP`. `Test` returns `[]Step` (connect, auth, write, read, delete) and takes the
   test file name from the caller.
3. **Names live in `internal/backup/remotename`**, a pure package, so the drivers (which cannot
   import `internal/backup`: it imports them) and the retention code share ONE format. `Matcher` (one
   install id, the exact #33 expression, a real UTC date, whole-string match) is used by retention;
   `Valid` (any install id, or a `launchpad-<id>-test-<8..64 hex>.tmp` file) is the driver's last
   defence against path traversal; the local driver and a test prove it. The install id is checked
   (`^[0-9a-f]{8}$`) BEFORE it enters an expression. A name with a month 13 is foreign and never deleted.
4. **Backup policy.** `backup.Policy(cidrs, gateways)` opens exactly the operator list (no "all
   standard ranges" switch, unlike git). Names refused before resolution: `postgres app silo backup
   host.docker.internal gateway.docker.internal` (not `web`: it is not on `offsite`'s networks, #13).
   The default-gateway rule uses `netguard.Policy.Gateways`. If the route file is missing (not Linux)
   no gateway is known; if it cannot be read for any other reason the policy FAILS CLOSED (every
   private address refused), as the git policy does for the interface read. The gateway rule is not
   measured on Linux (unchanged from #34).
5. **Silo dialer.** `NewSiloDialer(S3_ENDPOINT)` accepts a host-only http(s) URL, compares the
   requested name and port with the configured ones before any lookup, and has no policy. Nothing in
   Go stops other code in the module calling the constructor; "only the files code path can obtain
   it" is a review rule for ticket 14, not a compiler rule. The HTTP client on it also has `Proxy: nil`
   and follows no redirect.
6. **SFTP.** `DialSSH` dials with the guarded dialer and calls `ssh.NewClientConn`; the handshake ends at the
   earlier of the context deadline and `now + cfg.Timeout` (30 s default). The host-key callback is the driver's
   (ticket 8); `DialSSH` refuses a config without one.
7. **Safe errors.** `driver.Error` has a code and a status only, so the remote's words cannot be put
   in it. `MapNetError` maps a guard refusal to `ssrf_blocked`, DNS, timeouts, refusals and the x509
   errors; anything else is `server_error` with no status. `Retryable` is the #31 list;
   `StatusError` maps 3xx to `server_error` with the status (never retried), 401, 403, 404, 408, 413, 507
   to their codes. `clock_skew` is for the S3 driver to map.
8. **Reply caps.** `ReadCapped` (list bodies, 16 MiB) and `CapReader` / `DownloadCap` (declared size
   plus 64 KiB; the slack is a guess).
9. **age.** X25519 only (`age1...`; scrypt, SSH and plugin forms are refused by a prefix test and
   the parser), 1 to 5 recipients. `Encrypt` streams (one chunk in memory) and returns the sha256 and
   size of the plaintext AND of the ciphertext from one pass (#18, #22). Every decrypt failure is
   `ErrDecrypt` with no cause text; an output limit gives `ErrTooLarge`. On error `dst` may hold
   earlier authentic chunks: the caller discards it. `Fingerprint` is the full sha256 of the `age1...`
   string. `GenerateKeyPair` is for `keygen` (ticket 4) and tests.
10. **Manifest format.** The `.json` file is `{"payload": base64(payload JSON), "signature":
    base64(Ed25519)}`; the signature covers `"launchpad-backup-manifest-v1\n"` + the payload BYTES as
    stored, so no canonical-JSON rule is needed. The payload is `version, installId, object, sha256,
    size, dumpTime, schemaVersion, launchpadVersion` (no host, URL or secret). `VerifyManifest` checks
    the 1 MiB cap, a strict envelope (unknown field or trailing bytes refused), the signature, and
    only then the payload (strict, every field validated, the dump time must equal the time in the
    object name). `Bind(installID, stem)` and `CheckObject(sha, size)` are separate calls, because
    `verify` (ticket 4) needs them in that order and #39 names them separately. The public key text is
    standard base64 of the 32 raw bytes (the note did not choose a text form). `schemaVersion` is an
    integer (the migration number); the note says "the schema version" only.
11. **Freshness is not proved.** A correctly signed old pair still verifies by itself; a replay is
    caught only by `Bind` against the stem the caller fetched, and an old whole pair is caught by
    nobody (#22). A test pins this so nobody expects more.
12. **Retention.** `PlanPrune` is pure. It needs the just-uploaded stem to be a COMPLETE pair in the
    listing, else it errors and deletes nothing ("runs only after a new upload is verified"). The
    new pair is NEVER deleted (review round 1): if the newest N by the time in the name do not
    include it (the clock stepped back, or future-dated pairs were planted), it is kept IN ADDITION
    to the newest N, so the plan can keep N+1 pairs. The note (#33) is silent; the alternative
    (keep it and N-1 others) would let a planted future pair evict a real copy. `Prune` checks the
    install id before it builds the `List` prefix; drivers must still treat a `List` prefix as
    untrusted input. Pairs delete
    manifest first, then object; orphans (a name's own time more than 24 hours before `now`, not
    named by a running run) delete one file. `Prune` lists with the prefix `launchpad-<id>-`, plans,
    deletes only planned names and counts failures; a failed delete never stops the rest.
13. **Registry and tag.** `Register` panics on a duplicate, an empty type or a nil driver.
    `RegisterForTest(t, type, driver)` and the local driver are in files with
    `//go:build backuptest`. The local driver is `driver.Local`, not a valid destination `type`. The
    production registry is EMPTY in this ticket: no real driver exists (tickets 6 to 9), so
    `SupportedTypes()` is empty and a type with no driver is absent.
14. **Tag check.** `scripts/check-backuptest.sh` (run by `make test` and `make docker-test`, so by
    CI): builds `cmd/server` without the tag and greps its symbols; builds `./...` without the tag
    (an untagged file that uses a tag-only name fails to compile); plants such a file and proves the
    build fails. `cmd/server` does not import `internal/backup` yet, so step one is trivially true
    today; it starts to matter when ticket 4 wires `offsite`.
15. **Not done here (later tickets):** the frame format of `fetch` and `unpack` (#39), any real
    driver, the `Test` and `Probe` implementations beyond the local fake, the `offsite` command and
    config parsing of `BACKUP_ALLOW_PRIVATE_CIDRS` at boot (`backup.PolicyFromEnv` is ready).

## Build notes: ticket 2 (LAUN-69)

The API: settings, recipients, destinations, probe / test / exchange / run, runs, changes, status,
and two host commands. No migration: the unique rule runs in the app under the settings row lock,
and the rest is queries (`db/queries/backup.sql`). Code: `internal/backupapi` (logic),
`internal/api/backups.go` (handlers), routes under `/platform/backups` in `router.go`.

1. **Gate and shape.** Every route is behind `RequirePlatformAdmin`. Every write is JSON-only and
   uses `idempotency.Reject` (secrets, and work that must not replay). No route dials. No route
   calls `events.Emit` (#26); the change log is the record.
2. **Errors.** New codes `backup_unavailable` (503) and `destination_busy` (409). One rule: a state
   the admin can fix by editing is `409 conflict`; a server that cannot do the action at all (no
   `BACKUP_ENCRYPTION_KEY`, or no driver for the type) is `503 backup_unavailable`. A write that
   only stores a secret with no key is `409`; an action (probe, test, exchange, run) with no key
   or no driver is `503`. Order: 404, then 503, then 409.
3. **Recipients.** Stored in `backup_settings.recipients`. Live count (cap 5) includes pending and
   removing keys. The first add ever is active at once; after that every add is held 24 hours,
   also when the set was emptied (`ever_had_recipient`). Web removal and web cancel use the
   PROJECTED non-empty rule (keys with no removal scheduled, pending adds counted). The host
   command `backup-recipient-remove` needs a key ACTIVE now to remain while a destination is
   enabled. A cancelled add leaves the array at once; passed removals are pruned on the next
   write. The admin who scheduled a removal cannot cancel it (403); another admin can. Recipient
   times come from the injected clock, else the database `now()`.
4. **Destinations.** Made disabled. Secrets sealed with `BackupColDestinationSecrets`, owner = the
   destination id; the key check is recorded on those writes only. A driver's `Validate` error
   text is dropped (it may echo a value). A PATCH that changes the config while any stored secret
   is not entered again in the same request is `400` (#25). An unreadable stored blob is dropped
   when any secret is written. Enabling checks, all `409`: key, driver, signing key present and
   openable, one key active now, secrets readable, required secrets present. Dropbox
   `refreshToken` can be cleared but never written through the API (only the exchange writes it).
5. **Pin and host key field names are my choice:** `tlsPin` (s3, webdav, dropbox) and `hostKey`
   (sftp) in `config`. The real drivers (tickets 6 to 9) MUST accept them in `Validate`.
6. **Actions only queue rows.** `pg_notify('backup_work')` runs in the same transaction. Unsaved
   test secrets are sealed with `BackupColRunTestSecrets`, owner = the run id. "Back up now" adds a
   `backup_requests` row and a `manual` run with `request_id`; a queued retry is cancelled, any
   other open backup run is `409 destination_busy`. Probe, test and exchange are not limited by the
   busy rule. `useSavedSecretsFrom` needs a config equal to the saved one. A Dropbox form cannot
   be tested unsaved (the code is single use).
7. **Rate limit:** one bucket for probe, test, exchange and run: 6 a minute, burst 3, per process.
   The numbers are a guess. The token is taken inside `queueRun` / `QueueRun`, AFTER the body, the
   404, the key, the driver, the config and the disabled checks, so a bad request spends none. A
   request that then loses to `destination_busy` or a different-pin 400 has spent one.
8. **Status** has no mail-off or stale fields (ticket 8). Banners: `encryption_key_missing`,
   `key_mismatch`, `signing_key_missing`, `signing_key_unreadable`, `no_active_recipient`,
   `no_recent_dump`.
9. **Host commands:** `server backup-recipient-remove` and `server backup-recipient-cancel`, both
   `--fingerprint <64 hex>`. Exit 0 done, 1 refused or failed, 2 usage. They write a
   `*_by_host` change row with no actor.
10. **Tests** (`-tags backuptest`, `INTEGRATION=1`): the gate matrix over every route (non-admin,
    org admin without platform admin, agent token, CSRF), the hold with an injected clock, the
    cancel rules, the no-host-change rule, the enable matrix, the queued rows and the wake, the
    planted-secret grep (log, events, idempotency, responses, all backup rows in text, hex and
    base64), no events, the busy race, the rate limit. Mutation checks done: the scheduler-cancel
    guard, the kept-secret guard and the platform-admin gate each make a test fail when removed.
11. **Not verified:** that the real drivers accept `tlsPin` / `hostKey`; the `NOTIFY` timing against
    a real `offsite`; the rate-limit numbers.
12. **Change-log order.** The service makes the change-log id (`uuid.NewV7`, google/uuid v1.6.0,
    strictly increasing in one process) instead of the database `uuidv7()`. Rows of one transaction
    share `at`; the read order `(at DESC, id DESC)` is then the write order. No migration. Order in a
    PATCH: renamed, config, secrets, schedule, retention, enabled. Two servers on one database order
    only by `at` across transactions, which is enough: one transaction is one process.
13. **Contract for the offsite worker (ticket 4).**
    - `test_config` of an UNSAVED probe or test: `{"type": ..., "config": {...}}`, plus
      `"confirmedFingerprint"` for a test. The run has no `destination_id`. Exchange:
      `{"type": "dropbox"}` with the destination id on the run. A SAVED probe or test has no
      `test_config`: read the destination row.
    - `test_secrets_enc` is sealed with `BackupColRunTestSecrets`, owner = the run id. An unsaved
      test: the secrets map (names below). Exchange: `{"code", "appKey", "appSecret"}`. A probe has
      none. The worker deletes the blob when the run ends.
    - `confirmedFingerprint` in an unsaved test's `test_config` is separate from the saved config.
      The worker must not use it to override a saved pin or host key when it sends saved secrets
      (`useSavedSecretsFrom` requires an identical config, but the fingerprint rides beside it).
    - Secret names per type: s3 `accessKey`, `secretKey`; webdav `password`; sftp `password` or
      `privateKey`; dropbox `appKey`, `appSecret`, `refreshToken` (written by the exchange only).
    - Disable and delete do NOT cancel queued or waiting runs. The worker must cancel them (#31).
    - A manual run that cancels a queued retry sets the retry to `canceled`; the FAILED parent run
      keeps `will_retry = true`. Ticket 4 and the web page must not read that flag as "a retry is
      queued".

## Build notes: ticket 4 (LAUN-71)

The runner: `server offsite` and the package `internal/offsite`. No migration. New queries are in
the "LAUN-71" part of `db/queries/backup.sql`; every one runs within the grants of #15. Judgment
calls the design left open (the safest default each time):

1. **The key is required.** `server offsite` fails its boot without `BACKUP_ENCRYPTION_KEY` (the dev
   key is refused outside `LOG_ENV` local/test). The app keeps it optional (#12). Config:
   `config.LoadOffsite`; it reads no `AUTH_OTP_PEPPER`, no `POSTMARK_*`, no `S3_*`.
2. **New safe codes.** `no_recipient` (no active age recipient when the run starts) and
   `object_mismatch` (a verify run found a remote object that differs from the recorded sha256 or size).
   An unconfirmed SSH host key in a test is `host_key_changed`; an unconfirmed TLS chain is `tls_untrusted`.
   Both new codes are not transient (no retry).
3. **Slots.** A run satisfies a slot when a schedule, catchup, retry or manual run of that destination
   was queued at or after the slot and is queued, waiting, running, succeeded or failed. The trigger is
   `schedule` when the slot passed less than 5 minutes ago, else `catchup`. Time is the database clock.
4. **A usable dump** is `ok`, not removed, has sha256 and bytes, and the file is a regular file (Lstat)
   named `devops-YYYYMMDDTHHMMSSZ.dump`. The newest usable dump older than 6 hours makes the run ask for an
   extra dump: ONE shared open request (`requested_by` `offsite`, an advisory transaction lock).
5. **Free space.** Counts only OTHER running backup runs that started earlier (tie-break `started_at, id`),
   so of two concurrent runs on one volume the second fails `no_space`. Encryption also needs the dump
   size once; `scratch_full` is a write error ENOSPC or EDQUOT during encryption.
6. **Concurrency.** Two backup or verify runs at once, four probe, test or exchange runs. A
   `waiting_dump` run holds no destination lock and no slot; the lock is taken again before it becomes `running`.
7. **Lease.** 60 s, renewed every 15 s. Expired leases are swept on EVERY scan (not only at boot): the
   run is `interrupted`. Boot also clears stale test secrets (a non-queued run, or queued over 10 minutes;
   every scan repeats this) and scratch files of runs that are not running or waiting.
8. **Health.** The health file is touched every 10 s and during the boot wait; `healthcheck` passes when
   its mtime is at most 3 minutes old.
9. **Run text.** A run's `error_text` is a fixed sentence or the driver code text. A non-driver error
   from a driver is never stored; it is logged by its code only.
10. **Manifest.** `schemaVersion` is the dump's `migration_version` (0 if NULL). `launchpadVersion` is the
    env `LAUNCHPAD_VERSION` (ticket 5 must pass it). The dump time is the time in the dump's file name.
11. **Signing key.** Made only when BOTH columns are empty, by compare-and-set (two workers: one wins).
    A blank, half-empty, unreadable or non-matching pair makes the run fail `secrets_unreadable` and the
    worker changes nothing. `rotate-signing` writes the `signing_key_replaced` change-log row (no actor, no
    RETURNING) and the new key in ONE transaction, no savepoint. It never writes `key_check`.
12. **Instance guard.** Reread every scan. Present and equal: ok. Different: not ok. Missing and the
    database has no succeeded run: written, ok. Missing with a succeeded run: not ok (never written over
    history). Any read error: not ok. While not ok, queued and waiting backup runs fail `instance_changed`
    with no retry; probe, test and exchange still run. `resume --instance-id` writes the file.
13. **Cancel.** A disabled destination cancels its backup runs only; a deleted one cancels every open run
    that names it; a running run is aborted and its partial remote object deleted. An unsaved probe or test
    (destination name empty) is never canceled by this rule.
14. **Test runs.** A saved or unsaved test seals its secrets with `BackupColRunTestSecrets`, owner = run id.
    The blob is NULL after success, failure, cancel and boot. Phase 2 of a test runs only with a verified chain,
    a stored pin, or a confirmed fingerprint equal to the probed one; a pin in the config always wins over a
    confirmed fingerprint.
15. **Exchange.** The refresh token is merged into the sealed secrets under `FOR UPDATE` on the destination
    (`Worker.MergeSecrets`); other secrets stay and `secrets_set` gains the name. A failure changes nothing.
    The app's PATCH must use the same row lock (tested with two writers).
16. **Verify run.** Downloads the newest object and compares its sha256 and size with the DATABASE record
    (`object_mismatch`, `not_found`). Scheduled when `verify_every_days` days passed since the last verify,
    or since the first success; never doubled while one is open.
17. **Fetch frame.** `fetch` writes: 4 bytes version (1), 8 bytes manifest length, manifest (1 MiB at most),
    8 bytes object length, object (5 GiB at most), nothing after. `unpack` creates files with O_EXCL and
    removes only files it made. The object size must be known from the remote listing.
18. **Plain HTTP** to a destination is allowed only when EVERY resolved address is in an operator-listed
    range (`BACKUP_ALLOW_PRIVATE_CIDRS`).

Contract for ticket 5 (compose and deploy): the image runs `/bin/server offsite`; env `DATABASE_URL`
(role), `BACKUP_ENCRYPTION_KEY`, `BACKUP_ALLOW_PRIVATE_CIDRS`, `BACKUP_DUMP_DIR`, `OFFSITE_WORK_DIR`,
`OFFSITE_HEALTH_FILE`, `LAUNCHPAD_VERSION`, `LOG_ENV`; health check `/bin/server offsite healthcheck`; the
role needs exactly: SELECT on `backup_settings`, `backup_destinations`, `backup_dumps`, `backup_runs`, `backup_requests` and `schema_migrations` (NOT `backup_change_log`: the worker only inserts there); INSERT and UPDATE on
`backup_runs`; INSERT on `backup_requests` and `backup_change_log`; UPDATE (`secrets_enc`, `secrets_set`) on
`backup_destinations`; UPDATE (`signing_public`, `signing_private_enc`) on `backup_settings`; USAGE on the
schema. `TestRestrictedRole` runs the flow with these grants and nothing more. `LISTEN backup_work` needs no grant.

Review changes: the scratch directory is cleaned on every scan after the lease sweep (a killed run's
dump-sized file goes within a scan, not at the next restart); a full backup slot no longer stops the
dispatch loop (the run is skipped for the pass, so a probe or test behind it starts).

Known limits for later tickets:
- **Plain HTTP and DNS rebinding.** `plainHTTPAllowed` (`otherruns.go`) resolves the host once, outside the
  dialer. The drivers and the dialer MUST enforce "plain HTTP only to listed ranges" at dial time
  (ticket 5 or the driver tickets); the check here is not enough alone.
- **`fetch | unpack`** without `pipefail` can succeed after `ErrFrameTrailing`. The runbook must say: use
  `set -o pipefail`, and always pass `--install-id` and `--stem` to `verify`.
- A duplicate verify run is possible (two scans racing); it is benign.
- `doProbe` does not pass `AllowPlainHTTP`, unlike test and backup runs. It matters when real drivers land.
- `InsertBackupRetryRun` uses `ON CONFLICT DO NOTHING`: if a newer run for the destination appeared, the
  retry row is not made and `will_retry` stays true on the failed run.
- `cmdFetch --destination` passes no `AllowPlainHTTP` (same class as `doProbe`).
- A worker that dies leaves the partial remote object of an `interrupted` run; retention must clean it
  up as an orphan.
- `keygen` prints both keys and does not check for a terminal (the usage text no longer says it does).
- `clock_skew` has no end-to-end test until a real driver exists.
- The health file is touched by an independent ticker: it shows liveness, not loop progress (note 8).

Not verified here: any real driver, a real `pg_dump` from the production image, uid 70 on the volumes,
NOTIFY timing in the real compose stack, the egress gateway rule on Linux, and the real
`offsite-grants.sql` (ticket 5).

## Build notes: ticket 5 (LAUN-72)

Compose service, role, keys, `backup.sh` and the host flags. No migration. Open points, decided on the
safest default:

1. **Scratch volume at `/offsite-work`, not `/work`.** Measured: a new named volume mounted at `/work`
   (the image has `/work/import` owned by 10001) gets its root owner reset to root, which undoes
   `volume-init`'s `chown 70:70`. `OFFSITE_WORK_DIR=/offsite-work`. This differs from #11's text.
2. **Start order** (#47): scoped `up -d` of the other services, wait for the app (migrated), apply the
   grants, then start `offsite`. Never a bare `up -d --remove-orphans`.
3. **Grants file** revokes everything in the database first, then grants the exact list (a hand-added
   grant disappears at the next run). It fails with psql exit 3 when `offsite_pw` is not sent. Caveat:
   if `ALTER ROLE ... PASSWORD` itself fails, Postgres may log the statement with the password; the
   password is alphanumeric and generated, and the log stays on the host.
4. **Passwords/keys**: generated once into `deploy/.env` (`rand_hex 32`); the password is validated
   `^[A-Za-z0-9]{32,128}$`. An existing install gets a new `BACKUP_ENCRYPTION_KEY` silently, with a warning.
5. **`BACKUP_ALLOW_PRIVATE_CIDRS`**: a bash pre-check (IPv4 /8 to /32, IPv6 /32 to /128, refuses
   0/8, 127/8, 169.254/16, multicast and above); the worker's `internal/config` stays the authority.
6. **`backup.sh`**: SQL only through quoted heredocs on stdin (no `psql -c`, no interpolated names);
   it waits for the next second if `devops-<ts>.dump` exists (`backup_dumps.file_name` is UNIQUE); the
   lock is a directory in `/tmp` with a pid file (stale lock recovery by `kill -0`); recording never fails
   a dump; at start it fails `running` dumps and `taken` requests left by a killed run.
   `BACKUP_REQUEST_POLL_SECONDS` (default 30) is also a test override.
7. **`--backup-resume`** writes a `backup_change_log` row of kind `instance_resumed` (a new kind for
   the ticket 7 notice job to handle) and is not confirmed by a prompt: it needs a terminal and is the
   operator's explicit act.
8. **Terminal rule**: `--backup-keygen`, `--backup-rotate-signing`, `--backup-resume` and the recipient
   commands refuse a pipe or a file; a fingerprint is validated as 64 lower-case hex.
9. **Postgres network**: postgres joins `offsite_db`, so the first update recreates it; `deploy.sh`
   says so first.
10. **Grants file is one transaction** (BEGIN ... COMMIT after the `offsite_pw` check): `test-offsite.sh` and the `\set` / `\gexec` / `ALTER ROLE` handling are unaffected.
11. **Project name**: the trial used the throwaway compose project `launchpad-laun72`.

Measured in a real throwaway stack (project `launchpad-laun72`, removed afterwards): `offsite` healthy
in about 40 s, uid 70, role `offsite` without super/createdb/createrole/replication/bypassrls; as that
role `SELECT users` and `SELECT backup_change_log` and `DELETE backup_runs` are refused, `SELECT
backup_dumps` works; the worker made its signing key; the default route is on the `egress` network
(eth1), `offsite_db` is internal; `app`, `backup`, `silo` and `web` do not resolve from `offsite`.

Not verified: the egress gateway rule on Linux (Docker Desktop only); `--backup-rotate-signing`,
`--backup-keygen` and `--backup-resume` in the real stack (they need a terminal; the stub tests prove
their flags); real dump size and time on production data; a real destination.

## Build notes: ticket 6 (LAUN-73): the S3-compatible driver

Built: `internal/backup/driver/s3` (type `s3`, registered by a blank import in `cmd/server/main.go`, so the
app's `Validate` / `Identity` and `offsite` both see it), and `internal/storage/presign_export.go` (an exported
`PresignURL` over the SAME unexported signer; `sigv4.go` and `TestPresign_AWSExample` are untouched). No SDK,
no new module, no migration. Judgment calls where the note was silent or two ways fit:

1. **Config (typed JSON, strict: unknown field, trailing JSON refused).** `endpoint` (`scheme://host[:port]`, no
   path, query, user info or backslash), `bucket` (`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`), `region` (default
   `us-east-1`), `folder` (optional; every segment `^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`, no `..`, 200 characters at
   most), `addressing` (`path` default, or `virtual`: `bucket.host/key`), `tlsPin` (64 lower-case hex, the name
   ticket 2 chose). Secrets: `accessKey`, `secretKey` (note 13 of ticket 2). The `--remote-config` file of #39 uses
   the same names.
2. **Identity** is `(s3, host without a default port and in lower case, bucket, folder)`; the region and the
   pin are not part of it.
3. **Signing: the query-string form of SigV4 with `UNSIGNED-PAYLOAD`**, the form the attachment store already
   uses, so one signer serves both and a body can be streamed without hashing it first. Each request is signed
   with a one-hour expiry; the server checks the date at the START of a request, so a long upload is fine.
   Cost: the signature is in the URL. The URL is never logged or put in an error. A service that refuses
   presigned requests cannot be used (not measured on any real service).
4. **One PUT, no multipart (#40, kept).** An object above 5 GiB (and a negative size) is `too_large` BEFORE the
   source is read or a request is sent. After the PUT the driver does one `HEAD` and compares the stored size;
   a different size is `server_error`. The source must give EXACTLY `size` bytes: a short or a longer source
   fails the request (Go's transport refuses a longer body), so a dump that changed while it was read never
   becomes a "good" object. A failed upload may leave a partial object; the runner's best-effort delete and
   the orphan rule of #33 clean it.
5. **List** is `ListObjectsV2` (1000 a page, at most 1000 pages, all pages together at most 16 MiB). Only keys
   under the folder that pass `remotename.Valid` AND start with the prefix are returned, as bare names; a key
   with a slash left over, a stranger's file or a hostile key is dropped. A `List` prefix is checked
   (`[A-Za-z0-9._-]`, 128 characters) because the caller's prefix is untrusted input.
6. **Names** for `Put`, `Get` and `Delete` go through `driver.CheckName`; a refused name is `not_found` with no
   request. A request URL is built from the folder and that name only, never from a reply.
7. **Error mapping.** The driver reads at most 64 KiB of an error reply, and only to find the S3 `<Code>`; the
   message and the rest are dropped. `RequestTimeTooSkewed` and `RequestExpired` are `clock_skew`; a 403 whose
   answer `Date` differs from our clock by more than 5 minutes, with `SignatureDoesNotMatch`, `ExpiredToken` or
   `AccessDenied`, is `clock_skew` too (not retried). `InvalidAccessKeyId` and a `SignatureDoesNotMatch` with a
   close clock are `auth_failed`; other 403 is `forbidden`; `EntityTooLarge` and 413 are `too_large`;
   `QuotaExceeded`, `storage_cap_exceeded` and 507 are `quota_exceeded`; `NoSuchBucket` and 404 are `not_found`
   (`Delete` of a missing key is not an error); any 3xx is `server_error` with its status, never followed.
   Everything else is `driver.StatusError`. The clock-skew mapping rests on documented S3 behaviour and the
   `httptest` cases; no real service was asked.
8. **Retries.** The driver never retries inside a call. The run-level retry of #31 (once, after 30 minutes, on
   `timeout`, `connect_refused`, `dns_failed`, `stalled`, 5xx) is the only retry. A connection reset that gives no
   status is `server_error` with status 0 and is therefore NOT retried (a gap in #31's list, kept as written).
9. **Timeouts (#40).** The dial uses the dialer's connect timeout; TLS handshake is the dialer's; the first byte
   (`ResponseHeaderTimeout`) is 30 s; a request with no byte moved for 120 s (a read of the source, a write to the
   server, a read of the reply, tracked by a timer that every read resets) is `stalled`. The 4-hour run limit is
   the caller's context. HTTP/2 is off (plain HTTP/1.1; connections are kept alive and idle ones closed after 30 s).
10. **TLS.** Without a pin: TLS 1.2 or newer, system roots and the host name. With a pin: the chain and the host
    name are NOT checked, the leaf's SPKI sha256 must equal `tlsPin` (constant time) and the leaf must be inside
    its validity dates (`tls_pin_mismatch`, `tls_untrusted`). `InsecureSkipVerify` is set only together with that
    mandatory `VerifyConnection`. Chain errors map to `tls_untrusted` / `tls_wrong_host`.
11. **Plain `http://`** needs `AllowPlainHTTP` (else `ssrf_blocked`) AND is checked at DIAL time: after the guarded
    dialer connected, the socket's remote address must be inside an operator-listed range
    (`Policy.Private`), else the connection is closed and the call is `ssrf_blocked`. This closes the
    "resolves once, outside the dialer" gap of the ticket 4 limits list: the pre-check of the worker is now only an
    early answer. The HTTP client is `backup.NewHTTPClient` (guarded dialer, no proxy, no redirect).
12. **Probe** (phase 1): TCP connect through the guarded dialer, TLS handshake with no verification that records the
    leaf, close. It sends no HTTP request. The fingerprint is the lower-case hex SPKI sha256 (what `tlsPin` takes).
    `ChainVerified` is a real chain and host name check against the system roots. For `http://` there is no
    handshake: the kind is `plain` and the fingerprint is empty. **Change in ticket 4's code:** `doTest` treats a
    `plain` probe as trusted (nothing to pin; the dial-time rule is the control), and `doProbe`, the test's probe and
    `fetch` now pass `AllowPlainHTTP` (closing two limits of the ticket 4 list). The web page must show kind `plain`
    ("not encrypted in transit") instead of a certificate.
13. **Test (phase 2)** steps: `connect` and `auth` are one signed listing (a reply of any status means the server was
    reached: a transport failure is a failed `connect`, a 401/403/404/clock error is a failed `auth`), then `write`
    (a small file), `read` (a listing must show it with its size, then a download must match byte for byte) and
    `delete`. A read-only key stops at `write` with `forbidden`; a key that cannot delete stops at `delete`.
14. **Tests.** `internal/backup/driver/s3`: an in-memory S3 `httptest` server that re-verifies every signature
    (403, 507, redirect, stall in three directions, reset, deadline, huge and hostile lists, virtual addressing,
    every TLS case with a real handshake, proxy variables, the plain-HTTP dial rule, the policy refusals, a 5 GiB
    + 1 size, a 192 MiB streamed PUT with a heap ceiling). `silo_integration_test.go` (INTEGRATION=1): the whole
    contract against the real compose silo, wrong keys, a missing bucket, the real policy refusing silo, a TLS
    reverse proxy in front of silo for the pin. `internal/offsite/s3_silo_integration_test.go`: the worker with the
    real driver on silo: probe, test, three runs, retention (`keep_last` 2 leaves two pairs), a fetch frame.
15. **Virtual addressing and TLS (review fix).** With `addressing: virtual` requests go to `bucket.host`, so the
    dial, the TLS server name (SNI and host name check), the pin and the Probe all use `bucket.host`, never the bare
    endpoint host. A pin saved from the Probe is therefore the leaf the requests see. `Delete` swallows only an
    absent OBJECT (`NoSuchKey`, or a 404 that does not say `NoSuchBucket`); a missing bucket is `not_found`.
16. **Plain `http://` and presigned URLs.** A `plain` (http://) destination sends its presigned URLs in clear on the
    operator-listed network, so a sniffer there could replay one for up to an hour (the signed expiry). The dump itself
    is age-encrypted. Use https where the service has it.
17. **Not verified (no real service was reached):** AWS S3, Backblaze B2, Wasabi, MinIO and any other real service
    (presigned PUT/HEAD/DELETE/list acceptance, virtual-hosted addressing on a real DNS name and certificate, the
    real clock-skew reply, the real quota reply, a real 5 GiB object, a real `Date` header on a 403). Silo is
    MinIO-derived, so path-style signing and `ListObjectsV2` are proven there only. The manual check on one real
    service that the ticket asks for is NOT done and stays open. Multipart is not built (#40).
    Also not verified: virtual addressing on a real DNS name and certificate (tested only with a test CA and a
    resolver map); the 30 s first-byte limit (`ResponseHeaderTimeout`) starts after the last body byte, so a slow
    service that needs longer to answer a very large PUT (up to 5 GiB) may time out, and nothing was measured;
    the in-memory S3 fake re-verifies signatures with the SAME signer, so only `TestPresign_AWSExample` and the silo
    tests prove the canonical form.

## Build notes: ticket 8 (LAUN-75)

Notices, change notices, the stale alert, the status fields and the app's row cleanup. No migration
(every column, `notified_at`, `last_stale_alert_at` and the partial indexes, exists since ticket 1) and no
new route (the status route grows). Code: `internal/backupnotice` (mail), `internal/backupapi/cleanup.go`
and `notices.go`, queries in the "LAUN-75" part of `db/queries/backup.sql`, wiring in `cmd/server`.
Judgment calls where the design was silent or two ways fit (the safest default each time):

1. **Notices derive from ROWS, not events.** The job reads `backup_runs`, `backup_change_log` and
   `backup_destinations` (#26, #41). It does not import `internal/notify` and does not use the T13 inbox.
2. **Mail not ready: nothing is sent and nothing is marked.** `MailSwitch.Ready()` false (provider `none`,
   or `env` with nothing set) skips run notices, change notices and the stale alert for the tick. The
   test-secret sweep still runs. Windows (a guess): a run that ended more than 24 hours ago and a change
   older than 7 days are not mailed when mail comes back; the page and tables still show them. Rows that
   need no mail are marked in the same pass, so they are not looked at twice.
3. **The stale clock never counts time before the destination was created or last enabled.**
   `Since = max(last success, created_at, enabled_since)`. #41 says a never-succeeded destination counts from
   `max(created_at, enabled_since)`; I applied the same floor to one that did succeed once, so a destination
   that is re-enabled after months off does not alert at once. The alert fires when `now - Since` is more
   than the threshold (strictly), the threshold being `max(stale_alert_hours, 2 x interval)` (24 h daily,
   168 h weekly). `stale_alert_hours = 0` turns off the mail, the status list and the banner.
4. **Alert once per 24 hours** is a compare-and-set `UPDATE last_stale_alert_at` (`ClaimBackupStaleAlert`),
   so of two app instances one sends. The claim comes BEFORE the send (at-most-once): a crash loses that
   alert and the next one comes 24 hours after the claim. If no admin has an address, nothing is claimed.
   `last_stale_alert_at` is not reset when a destination recovers (a second stale spell within 24 hours of an
   alert waits for the 24 hours).
5. **Change notices count as sent when at least one admin got the mail.** The design says a failed send is
   retried on the next tick. Retrying the whole row for one permanently bad address would mail the others
   every minute for 7 days, so a row is marked when one send worked; a failure is logged (fixed text, no
   address). With no admin address, or every send failing, the row is not marked and the next tick tries again.
6. **A change mail holds only a whitelist of the row.** Kind sentence, destination name, actor username (or
   "a command on the host"), time, the 12-character fingerprint (`fingerprint`, `newKeyFingerprint`) and, for
   `notice_settings_changed`, the old and new values. `identity` (a host), URLs and anything else in `detail`
   never reach a mail. An unknown kind mails a generic sentence (its name only if it is a plain identifier).
7. **Run mail text:** destination name, kind of run, finish time, size (success), `error_code` (only if it is
   a plain identifier) and `error_text` (control characters stripped, 200 characters). Subjects strip control
   characters (a CR or LF would end a header). The link is `<PUBLIC_BASE_URL>/<org slug>/settings/platform`
   (the web route `/:org/settings/platform`; the slug is the install's one org, read with `GetSignupOrg`). Without a
   base URL, or without an org, the mail has no link and says where to look. `TestPageLink_ShapeMatchesTheWebRoute`
   reads `web/src/App.tsx` to keep the shape true.
8. **Status fields (all additive):** `mailReady`, `staleAlertHours`, `staleDestinations` (id, name,
   `lastSuccessAt`, `since`, `thresholdHours`, `lastAlertAt`), `failedDestinations` (an ENABLED destination
   whose newest backup run `failed` with `will_retry` false) and banners `backup_stale`, `backup_run_failed`,
   `mail_off` (only while a destination is enabled). `mailReady` comes from `backupapi.Options.MailReady`
   (`MailSwitch.Ready`); it is false when unset. The stale list uses the same pure function as the mail.
9. **The sweep of `test_secrets_enc` runs every minute** (in the notice tick) as well as hourly (cleanup):
   #29 says "older than 10 minutes", and an hourly run alone would leave a secret up to 70 minutes. It clears
   by `queued_at`, whatever the run state, as #29 says.
10. **Cleanup details.** Runs: only FINAL rows (`finished_at` set) older than 180 days; a stuck `queued` row is
    not deleted by age. Dumps: `file_removed_at` older than 180 days, or `failed` with `finished_at` older than
    180 days; an `ok` dump whose file is not marked removed is never deleted. Requests: `done` / `failed` older
    than 7 days. Change log: 730 days. Dump `running` over 6 hours: `failed`, fixed `error_text`. Request
    `taken` over 2 hours with no running dump (by `request_id` or `dump_id`): `failed`; dumps are failed first,
    so one pass ends both. Recipient elements whose `removeAt` has passed are dropped under the settings row
    lock (order and `updated_by` kept, `ever_had_recipient` untouched, no change-log row: the removal was
    logged when it was scheduled). Deletes run in batches of 1000 with a 50 ms pause. The job holds a session
    try advisory lock (`devops:backup:cleanup`) like `notify.RunRetention`.
11. **Two app instances:** run and change rows are taken with `FOR UPDATE SKIP LOCKED` (a change notice holds its
    transaction while it sends, at most 20 rows and 15 s per mail); the stale alert is one `UPDATE`. A test runs
    four concurrent ticks on 40 runs and 10 stale destinations and counts each mail once.
12. **Time budget for change notices.** A change mail is sent inside the transaction that holds its rows, so one
    tick stops sending change notices after 45 s (`ChangeBudget`; `Options.Mono` is its clock) and leaves the rest
    unmarked for the next tick. A dead mail server therefore cannot hold the tick for 20 rows x admins x 15 s. At-least-once is kept.
13. **Run notices are lost when no admin has an address** (at-most-once): the rows are marked before the admins are
    read, so a run that failed while no platform admin had an email is never mailed later. Change notices wait instead.
14. **`instance_resumed`** (written by `deploy.sh --backup-resume`, LAUN-72; free text kind) has its own sentence. The kind string was checked against `deploy/deploy.sh` in LAUN-72: it writes exactly `'instance_resumed'`.
    **Known limit:** a `staleAlertHours` above about 180 days (4320 h) can fire early, because runs are pruned at 180 days and the stale clock then falls back to `created_at` / `enabled_since`.
15. **Not done here:** the web banners (ticket 11); a dedicated "mail was off" backlog notice; the `last_stale_alert_at`
    reset; mail for `exchange` or `test` runs (the admin watches those, #41).

Not verified here: a real SMTP or Postmark send (the tests use an in-process fake mailer); behaviour across
a Postgres restart mid-tick; the 1000-row batch pause against a large production table.

## Build notes: ticket 7 (LAUN-74): the WebDAV and Nextcloud driver

Built: `internal/backup/driver/webdav` (type `webdav`, registered by a blank import in `cmd/server/main.go`).
No SDK, no new module, no migration, no change to ticket 3 or 4 code. It copies the structure of the S3
driver (ticket 6). Judgment calls where the note was silent or two ways fit:

1. **Config (typed JSON, strict: unknown field, trailing JSON refused).** `url` (the https base of the WebDAV
   tree, for Nextcloud `https://host/remote.php/dav/files/<user>/`; no user info, query, fragment, backslash,
   encoded slash or NUL; every decoded path segment `^[A-Za-z0-9@._~+ -]{1,100}$`, no `.` and nothing with `..`;
   400 characters of path at most), `username` (1 to 255 characters, no `:`, no control character), `folder`
   (optional, under the base; every segment `^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`, 200 characters at most) and
   `tlsPin` (64 lower-case hex). The only secret is `password` (a Nextcloud app password). The `username` is in
   `config`, as the `--remote-config` example of #39 already has it. Non-ASCII user or folder names are refused (safest default;
   widen the regexes if a real Nextcloud login needs it).
2. **HTTPS only, no `http://` at all.** The ticket says "Basic auth over TLS only"; #34 allows `http://` to an operator-listed
   range. The stricter rule wins: `Validate`, `Open` and `Probe` refuse `http://`, so a password is never sent in clear.
   A LAN Nextcloud without TLS cannot be used; one with a self-signed certificate works with the pin. Consequence: the
   probe kind `plain` never comes from this driver, and `AllowPlainHTTP` is ignored. To allow `http://` later, copy the
   `plainDialer` rule of the S3 driver (check the socket's remote address against `Policy.Private` at dial time).
3. **Identity** is `(webdav, host without the default port and in lower case, the decoded base path in the Bucket
   field, folder)`. The user name and the pin are not part of it.
4. **URLs come only from the config and a checked name.** `urlFor` joins the configured host, the base segments, the folder
   segments and a name that passed `driver.CheckName`, and escapes each segment. A `PROPFIND` href is never used to
   make a request (#33). List maps an href to a bare name only when it is a path (or an `https://` URL of THIS host and
   port, no user info, query or fragment) whose decoded form is exactly the folder's collection path plus ONE segment,
   when the raw text holds no `%2f`, `%5c`, `%00`, backslash or control character, and when the segment passes
   `remotename.Valid` and the prefix. Collections, the folder itself, other hosts, another scheme, `../`, nested paths and
   relative hrefs are dropped without a request. If a proxy in front of Nextcloud rewrites the path so that hrefs no
   longer match the configured path, List returns nothing (nothing is deleted), and the Test's `read` step fails
   `not_found`: that is the signal.
5. **Methods.** `Put`: `MKCOL` for each folder level once per opened remote (a `405` means it exists; a `409` is
   `not_found`), then one `PUT` of exactly `size` bytes with `Expect: 100-continue` (an early 401, 413 or 507 stops the
   body before it is sent; the transport sends it after 5 s if no `100` comes), accepting `200`, `201` and `204`. Then
   `PROPFIND` depth 0 compares `getcontentlength` with `size`; a different or missing size, a collection or a missing
   answer is `server_error` (a server that never reports the length cannot be used; not measured on any real server).
   `List`: `PROPFIND` depth 1 on the folder, a missing folder (`404`) lists as empty. `Get`: `GET`, `200` only.
   `Delete`: `DELETE`, a `404` is not an error. Above 5 GiB (and a negative size) is `too_large` before a request: WebDAV
   has no protocol limit, but `fetch` frames at most 5 GiB (ticket 4 note 17), so a larger object could not be restored.
   No chunked upload (Nextcloud's chunking API is a later change, #40).
6. **XML.** The reply is read through the 16 MiB cap (`too_large`), refused when it holds `<!DOCTYPE` or `<!ENTITY` in any
   case (Go's decoder expands no entity anyway), and must be a `DAV: multistatus` with status `207`. A property in a
   `propstat` whose status is not `200` is ignored. A non-207 `2xx` is `server_error` with the status.
7. **Error mapping.** The driver reads no error body at all (it drains at most 4 KiB so the connection can be reused); only
   the status maps: `401` `auth_failed`; `403` `forbidden`; `404` and `409` (a missing parent) `not_found`; `413` `too_large`;
   `507` `quota_exceeded` (what Nextcloud answers over quota); `423` (locked) and `405` `server_error` with the status; `429`
   (Nextcloud's brute-force throttle) and every `3xx` `server_error` with the status, never followed and never retried; `5xx`
   `server_error` with the status (retried once by the runner, #31). No `clock_skew`: Basic auth has no clock.
8. **TLS, timeouts, network.** As ticket 6 notes 8 to 10 and 12: `backup.NewHTTPClient` (guarded dialer, no proxy, no
   redirect), TLS 1.2+, the SPKI pin replaces the chain and host check but not the expiry, connect and TLS by the dialer, 30 s
   first byte, `stalled` after 120 s without a byte moved, HTTP/2 off, no compression. The password is sent only after the
   TLS handshake of the pinned or verified host; a failed pin or chain sends no request (tested). A redirect target never
   gets the `Authorization` header (tested). Probe: TCP, TLS handshake without verification, close; it sends no request.
9. **Test (phase 2)** steps: `connect` and `auth` are one `PROPFIND` depth 0 on the base (a reply of any status means the
   server was reached; a base that does not exist is a failed `auth` with `not_found`; the web page must word a missing base path clearly), then `write` (makes the folder, puts a
   small file), `read` (a listing shows the file with its size, a download matches), `delete`.
10. **Tests.** `internal/backup/driver/webdav`: an in-memory WebDAV `httptest` TLS server (Basic auth, `MKCOL` 405/409,
    `PROPFIND` 207) with: config and identity tables, the whole contract, nested folders, every status above, a refused
    redirect with no credential forwarded, wrong credentials, a hostile `PROPFIND` reply (about 25 hrefs: `../`, other
    host, other port, other scheme, user info, encoded slash, encoded NUL and backslash, nested, relative, query,
    fragment; a positive control for plain, absolute-same-host and percent-encoded names), `DOCTYPE`/`ENTITY`/malformed/
    wrong-namespace/deep/huge bodies, a size report that disagrees, stalls in three directions, a reset, a deadline, the
    policy refusing loopback and service names, proxy variables, the pin (match, mismatch, expiry, probe), a real chain and
    host-name check with a test CA, an early 507 that stops the body, escaped path characters, and a 192 MiB streamed
    PUT with a heap ceiling. Run with `-race -count=20`.
11. **Not verified:** a real Nextcloud, ownCloud or any other real WebDAV server. Nothing was measured about: the upload
    limit of a real Nextcloud (PHP and web server limits, the manual check the ticket asks for stays OPEN), `Expect:
    100-continue` behind a real reverse proxy, whether a real server's `PROPFIND` hrefs match the configured path
    (note 4), `getcontentlength` on every server (note 5), Nextcloud's `423` and `429` behaviour, `MKCOL` of an existing
    folder (`405` per RFC 4918 and per the fake), and non-ASCII login names. No WebDAV container is in the dev compose,
    so there is no integration lane for this driver.

## Build notes: ticket 10 (LAUN-77): the Dropbox driver

Built: `internal/backup/driver/dropbox` (type `dropbox`, registered by a blank import in `cmd/server/main.go`, so
the app's `Validate` / `Identity` and `offsite` both see it). It implements `driver.Driver`, `driver.Remote`
and `driver.Exchanger`. Standard library only, no SDK, no new module, no migration, no change to `offsite`
(`doExchange`, `MergeSecrets` and the column grant of ticket 5 already exist and are used as they are).

**NOT verified against a real Dropbox.** No Dropbox account, app or credential was used or asked for. Every
Dropbox fact below is from Dropbox's documentation as the builder remembers it; the tests run against an
in-memory fake that the builder wrote from the same memory, so the fake and the driver can be wrong in the
same way. The manual check on a real Dropbox app that the ticket asks for is OPEN. The list is at the end.

1. **Config (typed JSON, strict: unknown field and trailing JSON refused).** `access` (`app_folder` default, or
   `full_dropbox`), `folder` (optional sub folder, the segments of the S3 driver, no segment ends in `.`; REQUIRED
   with `full_dropbox`, so the driver never lists or writes in the root of a person's whole Dropbox), `tlsPin`
   (64 lower-case hex, the name ticket 2 chose; it applies to BOTH Dropbox hosts and replaces the chain check, the
   expiry check stays; Dropbox has a public chain, so it is for a TLS-inspecting proxy). There is no host or
   endpoint field: the hosts are fixed (`api.dropboxapi.com`, `content.dropboxapi.com`), so an admin cannot point
   the driver anywhere else. Plain `http://` does not exist for this type (`AllowPlainHTTP` is ignored).
   Secrets: `appKey`, `appSecret`, `refreshToken` (the last one is written by the exchange only, ticket 2 note 4).
2. **Identity** is `(dropbox, api.dropboxapi.com, <access mode>, lower-case folder)`. Dropbox paths are not case
   sensitive, so `Backups/Main` and `backups/main` are one destination. The Dropbox app (the app key) is a secret
   and not part of it: two destinations on different apps with the same folder are refused as one (a false
   collision, on the safe side of #25).
3. **Auth.** OAuth 2 refresh token flow. `POST https://api.dropboxapi.com/oauth2/token` with a form body
   (`grant_type=refresh_token`, the refresh token, `client_id`, `client_secret`); the secrets are never in a URL
   (a test checks every request URL has no query). The access token is cached in memory in the `remote` with its
   expiry, replaced 10 minutes before it ends (half the lifetime for a short token), dropped on `Close`, and never
   stored, logged or put in an error. A `401` on a call that carried a token drops it, refreshes it and tries the
   call once more; a second `401` is `auth_failed` (no loop). Refresh tokens do not rotate, so the driver writes
   no secret and needs no `MergeSecrets` (the "refresh merges under the row lock" case of #15 does not occur).
   `invalid_grant`, `invalid_client`, `unauthorized_client` and `access_denied` (OAuth `error` strings) and `401`
   are `auth_failed`.
4. **Exchange (`driver.Exchanger`).** `grant_type=authorization_code` with the code, the app key and the app
   secret (from `c.Secrets`). A code with a space, a control character, non-ASCII or over 512 characters is
   `auth_failed` and never sent. A reply with no `refresh_token` (an online authorization, the admin forgot
   `token_access_type=offline`) is `auth_failed`. Then ONE `list_folder` runs with the new access token: an
   `auth_failed` or `forbidden` there (an app without the file scopes) returns NO token, so nothing is saved
   (#9: "the exchange also lists the App folder"); any other listing failure still returns the token, because the
   code is already used and throwing the token away would waste it (the test run finds the real fault). The
   refresh token goes back to the worker as the return value only. The app must have the scopes
   `files.metadata.read`, `files.metadata.write`, `files.content.read` and `files.content.write` (the runbook of
   ticket 12 must say so; not checked against the Dropbox console).
5. **Upload.** The chunk size is 32 MiB (a multiple of the 4 MiB hash block), always below Dropbox's documented
   150 MB limit for one content request (`maxCall`; a test pins the default). **This is a change from the ticket
   text** ("upload sessions above 150 MB"): an object up to one chunk goes in ONE `files/upload` call, a larger
   object uses an upload session (`start`, `append_v2`, `finish`) of chunk-sized pieces, because only a piece held
   in memory can be sent again after a failure and #40 says a driver must not hold a dump in memory. The memory
   is one chunk buffer per `Put`, never the object. The documented limit for a session is 350 GB; above it (or a
   negative size) the object is `too_large` before the source is read. The commit mode is `overwrite` with
   `autorename: false` and `mute: true`: a name is unique per attempt (#31), and a resend of the same name replaces
   its own partial result instead of failing on it.
6. **A failed chunk is resumed (the ticket's acceptance).** A transient failure of a `start`, `append_v2`,
   `finish` or `upload` request (timeout, connection refused, DNS, stall, 5xx, and also a reset that gave no
   status, which the S3 driver does not retry) is sent again up to 3 tries with a 1 s and a 4 s wait. A chunk that
   had landed while its answer was lost is detected by the offset rule: the resend gets `409 incorrect_offset`
   and when `correct_offset` equals the end of this chunk the chunk is counted as done; any other offset is
   `server_error` 409. A `finish` that gets `incorrect_offset`, `not_found` or `closed` on a resend looks at the
   file with `get_metadata` and counts as done only when size AND content hash match. This is a deliberate
   exception to "the driver never retries inside a call" of the S3 notes (#31's one run-level retry stays in
   force on top of it).
7. **Content check.** The driver computes Dropbox's `content_hash` itself while it sends (sha256 of the
   concatenated sha256 digests of 4 MiB blocks; a test compares it with a second implementation in the fake, on
   sizes around the block edge) and compares it and the size with the metadata Dropbox returns for the commit.
   A different size or hash is `server_error` (no status), and the runner deletes the partial object as for S3. A
   reply with no `content_hash` is checked by size only. The driver does not send a `content_hash` argument (not
   sure the commit accepts it). The source must give EXACTLY `size` bytes: a short or a longer source is
   `dump_mismatch` and, for a longer one, nothing is committed (the check runs before the last chunk or the single
   call).
8. **Rate limits (#31).** A `429`, or a `too_many_requests` / `too_many_write_operations` tag on any status, waits
   `Retry-After` (the header; else `retry_after` from the body; else 1 s; at least 1 s, at most 5 minutes) and
   sends the same request again, up to 6 times per call (30 minutes at most), for every call including the token
   call. The wait honours the context. After 6 waits the call fails `server_error` with status 429, which is NOT
   retryable at run level (#31's list has only 5xx): a gap in #31, kept as written, noted for review.
9. **List.** `files/list_folder` (2000 a page, `recursive: false`, no deleted, no mounted folders) and
   `list_folder/continue` with the cursor, at most 1000 pages and 16 MiB of replies together; a cursor that is
   empty, over 4096 characters or the same as the one before is `server_error`. Only entries tagged `file` whose
   name starts with the prefix and passes `remotename.Valid` are returned, as bare names (a folder, a deleted
   entry, a stranger's file or a hostile name is dropped, never used). The prefix is checked like S3's. A folder
   that does not exist yet (`path/not_found` on the first page) is an EMPTY listing: the first upload creates it.
   Retention still filters with `Matcher` (a month 13 passes `Valid` and is dropped there, as for S3).
10. **Get / Delete.** `files/download` on the content host (the caller caps the stream); `files/delete_v2`, where a
    `not_found` is not an error and any other 409 is. Names go through `driver.CheckName` and a request path is
    built only from the folder and such a name. The `Dropbox-API-Arg` header is JSON from Go structs; every value in
    it is ASCII by validation.
11. **Error mapping (no remote text).** The driver reads at most 64 KiB of an error reply and walks its JSON only
    for `.tag` values that are lower-case words, `correct_offset` and `retry_after`, and an OAuth `error` string. It
    never reads `error_summary`, `user_message`, `error_description` or any message, and `respError` / `driver.Error`
    have no field for them (a test plants a marker in every reply and greps errors and test steps). Map:
    `429` / `too_many_*` wait; `insufficient_space` `quota_exceeded`;
    `missing_scope` `forbidden` under ANY status (tested before the 401 rule, so no refresh is wasted on it and the
    exchange cannot mistake it for an expired token; Dropbox's real status for it is unverified, the tests run 401 and
    403); `401` `auth_failed`; `403`, `no_write_permission`, `disallowed_name`, `restricted_content`, `team_folder`
    `forbidden`; `409` with `not_found`, `not_file` or `not_folder` `not_found`; `too_large` and 413 `too_large`;
    507 `quota_exceeded`; 408 `timeout`; 3xx `server_error` with the status (never followed); everything else
    `driver.StatusError` (a `409 conflict` or `malformed_path` is `server_error` 409). The tag names are from
    memory of the documentation.
12. **Transport.** Two clients from `backup.NewHTTPClient` (guarded dialer, no proxy, no redirect): the API host
    (30 s to the first byte, #40) and the content host (120 s, because Dropbox assembles a chunk before it
    answers; not measured). HTTP/2 off, no compression. Stall: no byte moved for 120 s is `stalled` (a timer reset
    by every read and write; it is created per request, so a `Retry-After` wait is not a stall). TLS 1.2 or newer,
    system roots and host name, or the pin. The tests point the two hosts at `httptest` TLS servers through the
    package variables `apiBase` and `contentBase`; production never changes them, and a non-`https` value makes
    `Open` fail.
13. **Probe** (phase 1): TCP connect to the API host and a TLS handshake that only records the leaf; nothing is
    sent. Kind `tls`; the fingerprint is the SPKI sha256 of the API host's leaf. Because a pin applies to both
    hosts, a pin from the Probe is right only when the content host has the same key (true behind an inspecting
    proxy; **not known for the real Dropbox hosts**, so the pin may need a second value later).
14. **Test** (phase 2): `connect` and `auth` are one pass of token refresh and one listing (a transport failure is
    a failed `connect`; an HTTP reply with an error is a failed `auth`, as for S3), then `write`, `read` (a listing
    shows the file with its size, a download matches byte for byte) and `delete`.
15. **Tests.** `internal/backup/driver/dropbox`: an in-memory Dropbox over two `httptest` TLS servers that refuses
    a call on the wrong host, checks the bearer token on every file call, checks the client id and secret on the
    token call (and fails a request that has a query string), keeps names in their written case, keeps a code single
    use, runs real upload sessions with offsets, and has fault hooks (status, reset, a lost reply after the request
    was processed, a stall). Cases: validation and identity; token reuse, refresh on expiry with an injected clock,
    server-side expiry (refreshed once), persistent 401 (no loop), bad refresh token / app key / app secret;
    single and session upload with the exact offsets, exact chunk multiples, the hash against a second
    implementation, a refused chunk resent, a landed chunk not added twice, a landed commit settled by
    `get_metadata`, persistent failure, a foreign `correct_offset`, wrong size / hash / bad JSON replies; short and
    long sources, a source read error, too large; 429 (header wins, body, floor and cap, bounded, token endpoint,
    cancel); the error table and the no-leak planted marker; pagination with cursors, foreign / hostile /
    folder / deleted entries, endless and repeated cursors, a huge reply, a missing folder; TLS pin (right, wrong,
    untrusted, expired leaf), probe, loopback refused by the default policy, `http://` refused, proxy variables
    ignored, redirect not followed, the exchange (success, single use, no refresh token, missing scope loses the
    token, other listing failure keeps it, bad codes never sent, secrets). `-race -count=20` on the package.
16. **Not verified (no real Dropbox was reached):** that any Dropbox endpoint name, argument, reply field or error
    tag above is exactly as Dropbox sends it (`files/upload`, `upload_session/*`, `list_folder`, `delete_v2`,
    `get_metadata`, `download`, `oauth2/token`, the `content_hash` in the commit reply, the `incorrect_offset` and
    `correct_offset` shape, `too_many_write_operations`, the `Retry-After` header and `retry_after` field); that
    `overwrite` and `mute` are accepted on the commit; that the code exchange works without a redirect URI for an
    app that has none; that a Dropbox access token and code use only visible ASCII; that the 32 MiB chunk, the
    350 GB session limit, the 120 s first-byte wait for a commit and the retry counts fit real behaviour and real
    uplinks; that `list_folder` shows a file at once after its commit; that the pin works for both real hosts;
    that the app scopes in note 4 are the right and complete set; team Dropbox spaces (`Dropbox-API-Path-Root`) are
    not handled; no peak-memory test (the fake holds the object, so a heap ceiling would measure the fake). The
    manual check on one real Dropbox app is OPEN. Also unverified: Dropbox's HTTP status for `missing_scope` (401 or 403).
17. **Known gaps (review round).** A list cursor equal to the one before is `server_error` (a real Dropbox may repeat one; not
    verified). `not_folder` maps to `not_found`, so a configured folder that is a FILE lists as empty on page 0. The stall
    timer and the content first-byte wait are both 120 s, so the second never fires first. A commit reply without
    `content_hash` is accepted on size alone (the runner's sha256 and the verify run are the backstop). `app_folder` and
    `full_dropbox` identities differ by mode but can name the same real folder if the admin made a full-access app whose
    folder is also an app folder's name. A `Get` stream keeps its stall timer until `Close` (the caller always closes). The
    exchange sets `fresh`: its access token is never refreshed (it has no refresh token), so a 401 there is final.
18. **More known gaps and unverified points (second review round, no behaviour change).**
    - `concurrent_session_*` tags (a `lookup_failed` on a racing resend) get no special rule; the run-level retry is the backstop.
    - A list treats `not_found`, `not_file` and `not_folder` on page 0 as empty: a folder path that is really a file looks
      empty and the `Put` fails later.
    - `Get` checks neither size nor `content_hash`; the caller's cap and the runner's sha256 are the backstop.
    - A Test run that fails after the write step leaves its test file on Dropbox (the file name is the `.tmp` test name).
    - The exchange orphans a refresh token if its folder listing then fails with `auth_failed` or `forbidden`: the admin must
      authorize again and send a new code.
    - `Retry-After` is honoured for 429 (and the `too_many_*` tags) only, not for 503.
    - JSON `null` for the config (or a field) becomes the `app_folder` default.
    - The token call holds the remote's mutex through its 429 waits, up to 30 minutes, so other calls on that remote wait.
    - No test captures logs: the package logs nothing today.
    - The OAuth authorize URL the admin opens must carry `token_access_type=offline`. It is built outside this package (the
      web page, ticket 11) and was not reviewed here.

## Build notes: ticket 9 (LAUN-76): the SFTP driver

Built: `internal/backup/driver/sftp` (type `sftp`, registered by a blank import in `cmd/server/main.go`). No
migration. Judgment calls where the note was silent or two ways fit:

1. **Dependencies.** `golang.org/x/crypto` v0.36.0 -> v0.58.0 (the latest tag; its `ssh` package is now used for
   real), `github.com/pkg/sftp` v1.13.12 (direct, new), `github.com/kr/fs` v0.1.0 (indirect, new; pkg/sftp's only
   non-x dependency). Moved on the way: `golang.org/x/text` v0.23.0 -> v0.43.0 (direct), `golang.org/x/sys`
   v0.31.0 -> v0.49.0 and `golang.org/x/sync` v0.12.0 -> v0.24.0 (indirect), because x/crypto v0.58.0 requires them.
   Licences: pkg/sftp BSD-2-Clause, kr/fs BSD-3-Clause, x/* BSD-3-Clause. testify stays out of go.mod (a test
   dependency of pkg/sftp itself, pruned by the module graph).
2. **Config (strict, typed JSON).** `host` (DNS name or bare IP literal, no port), `port` (default 22), `username`,
   `folder` (optional; every segment `^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`, no `..`; relative to the login directory or
   chroot, or ABSOLUTE with exactly one leading slash; 200 characters at most), `hostKey` (`SHA256:` + 43 base64
   characters). Secrets: `password` and/or `privateKey` (ticket 2 note 13); at least one.
3. **Host key text is the OpenSSH form** (`ssh.FingerprintSHA256`, what `ssh-keygen -l` prints), not hex, so the admin
   can compare it with what the server's owner gives (#36). `confirmedFingerprint` accepts it (the API allows
   `+ / = :`). The probe reports the key the server offered; Probe and Open use the same default algorithm
   list, so a server with several host keys offers the same one both times. If a server changes WHICH key type it
   offers first, the fingerprint differs and the run fails `host_key_changed` (not measured on a real server).
4. **Identity** is `(sftp, host[:port when not 22], username as "bucket", folder)`. The user name is in it because
   two users of one host may use the same relative folder in different home directories; the cost is that two users
   with one shared home are not detected as the same directory. The folder keeps its leading slash (absolute and
   relative are different places).
5. **Probe** runs the SSH key exchange through `backup.DialSSH`; the host key callback records the fingerprint and
   RETURNS AN ERROR, so the handshake ends before any authentication request. Not even the SSH "none" request (which
   names a user) is sent; a test counts the server's auth log. `Kind` is `ssh`, `ChainVerified` false.
6. **Open does not dial and refuses with no `hostKey`** (`host_key_changed`, as ticket 4 note 2 says for a test with
   an unconfirmed key). Every call (Put, List, Get, Delete, Test) opens its own connection: a stall or a cancel
   closes only that call's connection, and nothing is shared between calls. Cost: one handshake per call (a prune
   of N old copies costs N+1). The host key is checked in the callback with a constant-time compare BEFORE the
   client sends a credential; a mismatch is `host_key_changed`, and a test proves the server saw no auth request.
   Nothing in the driver can change the pin.
7. **Auth.** The key is tried first, then the password (also as keyboard-interactive, answered with the password,
   because some servers offer only that). A key that does not parse, and an encrypted key (there is no passphrase
   secret), is `secrets_unreadable` at Open. The `ssh` package has no typed "bad credentials" error, so a failure
   AFTER the host key was accepted that is not a timeout or a dropped connection is `auth_failed`; the error text is
   never read. A server banner is ignored.
8. **Test steps.** `connect` = dial, key exchange, host key; `auth` = SSH sign-in and the start of the sftp
   subsystem (a server that has no sftp fails here with `server_error`); then `write` (makes the folder when missing),
   `read` (listing shows the file with its size, then a download compares), `delete`. One connection for all.
9. **Folder.** Put makes a missing folder (`MkdirAll`) before it writes; an existing one costs one `Stat`. A folder
   that is a file is `not_found`. `List` of a folder that does not exist is EMPTY, not an error (a destination before
   its first upload).
10. **Put.** Exactly `size` bytes go to `.launchpad-part-<16 hex>` opened with `O_EXCL` (a planted symlink at that name
    cannot be followed; the name cannot match the retention regex and `List` drops it). A source that ends early or
    holds more than `size` is `dump_mismatch`; a read error of the source is a mapped transport error. After the
    copy the driver closes the file, `Stat`s it and compares the size (a server that stored less is `server_error`),
    then renames: `posix-rename@openssh.com` when the server advertises it (atomic, replaces), else plain rename; if the
    plain rename fails and the final name exists, it removes that file and renames once more (not atomic, only for
    servers without posix-rename; it matters when a retry uploads a name that already exists): see note 20. A rename
    over a symlink replaces the link and never writes through it. A failed upload removes its temp file; when the
    call was cut mid-copy (stall, cancel, deadline) a fresh connection removes it: see note 21. No `fsync` is
    requested (the extension is not used).
11. **Disk full.** SFTP status 14 (NO_SPACE_ON_FILESYSTEM) and 15 (QUOTA_EXCEEDED) are `quota_exceeded`; OpenSSH
    sends a plain `failure` (4) with strerror text, so for code 4 ONLY the text "no space left" or "quota exceeded"
    (case-insensitive) is compared and dropped. This is the one place the driver looks at a server's words; every
    other failure code is `server_error` with the status number. 2 is `not_found`, 3 is `forbidden`.
12. **List** reads the folder with the client's `ReadDir`, but the client has no entry limit, so the reader under it
    counts the server's bytes and, past 16 MiB (`MaxListBody`) for one listing, closes the connection: `too_large`.
    Returned: regular files only (the mode is from the server's attributes; a symlink, directory, pipe or device
    under a valid name is dropped), whose base name starts with the prefix and passes `remotename.Valid`. The prefix
    argument is checked like the S3 driver (`[A-Za-z0-9._-]`, 128 characters). `Valid` checks the shape of a name, not
    that its date is real (`Matcher` in retention does).
13. **Get** does an `Lstat` first and refuses anything that is not a regular file (`not_found`), then opens it. The
    returned reader owns the connection; `Close` ends it. The caller caps it (`driver.CapReader`). A name that
    is not a valid backup name is `not_found` before any connection, for Put, Get and Delete.
14. **Timeouts.** Dial, key exchange and SSH auth: 30 s (the earlier of that and the context). The sftp start and
    every call: no byte moved for 120 s closes the connection and the call is `stalled` (a timer reset by every
    byte from the server and every read of the source). The upload copy runs in its own goroutine so a source that
    blocks forever in `Read` cannot hold the call; that goroutine ends when `Read` returns. The 4-hour run limit is
    the caller's context. No retries inside the driver (#31).
15. **The server's words and the secrets** never reach an error: errors are `*driver.Error`; the client's error text
    is mapped by type or code and dropped; the server's stderr is drained to nowhere. A test plants a password,
    a server message and a path and greps every error text.
16. **Tests** (`-race -count=20`): an in-process SSH server (`x/crypto/ssh`) with pkg/sftp's in-memory request server
    (hooks for hostile replies) and its real-file-system server (posix-rename, real symlinks). Covered: password,
    key, key + wrong password, unknown key, passphrase key, keyboard-interactive; the probe sends no auth request; a
    changed host key (no credential is sent, every call fails the same way, the pin never changes); the SSRF table
    (loopback, a name resolving to loopback, metadata, private, `::1`, unresolvable); symlink / directory / pipe /
    device / traversal / stranger names in a listing; an endless listing (100 million entries) cut at 16 MiB; refused
    write, full disk, quota text, short and long source, a server that stores fewer bytes, a failed rename; a
    server that never answers the banner, one that never starts sftp, a download and an upload that stall; deadline
    and cancel; eight concurrent calls. Mutation checks done (each mutant fails at least one test, then
    was restored): round 1, the probe's abort (returning nil); `maxConcurrent` 64 -> 1; round 2, the status-4 gate of the
    rename fallback removed (`TestRenameOtherStatusCodesKeepTheFinalFile`), the regular-file check forced true
    (`TestRenameFallbackNeverRemovesADirectoryOrASymlink`), the temp-file `Lstat` forced true
    (`TestRenameFallbackNeverRunsWhenTheTempFileIsGone`), the Get size check removed (`TestShortDownloadIsAnError`),
    the host key mismatch case put after the context case (`TestWrongPinIsNotHiddenByAnExpiredContext`), the Get
    size check `!=` -> `<` (`TestLongDownloadIsAnError`), the orphan sweep dropped (the stall and deadline tests expect no
    leftover temp file). The "passed deadline without Err" clause of note 24 survives a mutant: no deterministic test
    reaches the microsecond gap.
17. **Not verified (no real SSH server was reached):** OpenSSH (`internal-sftp`, chroot, the strerror text of a full
    disk or a quota, posix-rename and its advertisement, keyboard-interactive-only servers), a NAS (Synology, QNAP),
    ECDSA / RSA host keys and RSA user keys against a real server, host key type negotiation order, a cipher or kex
    mismatch, the 30 s and 120 s limits against a real network, a real half-open connection. The fake servers are
    ours, from the protocol's documents. The manual check on one real server that the ticket asks for is NOT done
    and stays open.
18. **Config strictness is a decision, and a known gap.** The folder segment rule (`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
    refuses a space and a leading dot, so a Synology share such as `Backup Share` or a folder `.backups` is refused
    by `Validate`; the user name rule (`^[A-Za-z0-9_][A-Za-z0-9._@-]{0,63}$`) refuses `DOMAIN\user`. The rules keep a
    config from carrying anything a path or a log line could misread, and are the same family as the S3 driver's. The
    cost is real for NAS users; a later ticket can widen them (with a test for each new character) if the project owner wants.
19. **Listing cap.** Next to the 16 MiB byte cap, one listing holds at most 200,000 entries (`too_large`). The client
    returns the whole directory at once, so the entry count is checked on the result; the byte cap is what stops memory
    growth first.
20. **Plain-rename fallback (servers without posix-rename), and its risk.** The old file is removed ONLY when all of
    these hold: the first rename failed with the generic failure status (4), the temp file still exists (`Lstat`), and the
    final name is a regular file. Then it removes that file and renames once more. Any other rename error (permission,
    no space, a vanished temp file) returns the error and leaves the final file alone. **If the second rename fails, the
    old file is gone** (and the temp file is removed): the call fails and the caller must upload again. Between the
    remove and the rename there is a short window with no file of that name. A server with posix-rename (OpenSSH) never
    takes this path. Tests force it with a seam (`allowPosixRename`) because the in-memory server advertises the
    extension but its version does not replace.
21. **Orphaned temp files.** When a call ends with the connection cut (stall, cancel, deadline, the server went away) the
    temp file `.launchpad-part-<rand>` could not be removed on that connection. The driver then makes a best-effort
    removal over a fresh connection (same pin, same guarded dialer, a new 10 s context); it never changes the error the
    call returns, and a failure to connect is silent. A hard crash of the process (or a server that stays down) can still
    leave one; `List` never shows it and nothing else sweeps it yet.
22. **Temp file mode and `O_EXCL`.** The temp file is created with `O_EXCL` (so first with the server's default mode, for
    a short window) and then `chmod 0600` (best effort: a
    server that refuses setstat is not an error, because the data is age-encrypted). It keeps that mode after the rename.
    Tests on the real-file-system server: the final file is `0600`; a planted file and a planted symlink at the temp name
    make the upload fail and are left untouched (the symlink is not followed). A failed `O_EXCL` open does not remove
    the planted file (the driver only removes a file it created).
23. **Throughput.** The client is opened with concurrent writes and reads and up to 64 requests in flight per file
    (`maxConcurrent`, 2 MiB at 32 KiB a packet). OpenSSH's own limits are open handles, read and write length and message size
    (it reports them in `limits@openssh.com`); the driver does not read that extension and uses the client's 32 KiB packet. An upload hands the client an `io.LimitedReader` so it knows the length and
    keeps writes in flight; the stall watch is touched by every source read and every byte from the server. A download
    runs the client's concurrent `WriteTo` into a pipe. A consumer that pauses for 120 s stops the requests and the next
    read is `stalled`. A test server with a per-request delay counts the requests in flight (at least 4 for both
    directions; a client that waits one round trip per packet has 1). Not measured: real throughput on a real network or
    against a server with its own request limit (OpenSSH has a limit on open requests and on the read length).
24. **A caller's deadline in any phase** (dial, key exchange, auth, sftp start) is `timeout` (or the cancel's mapping),
    never `auth_failed` or `server_error`. Order in `connect`: stalled first, then a host key MISMATCH (a definitive
    security signal is never hidden as a timeout), then the caller's context, then auth. A passed deadline counts as
    ended even when the context's own timer has not set `Err` yet (a socket deadline can fire a few microseconds
    first). Tests: a deadline sweep across `List`, a deadline inside a 2 s auth delay with the right pin (timeout, stage
    `auth`), and a wrong pin with an expired context (`host_key_changed`). `TestPassedDeadlineWithoutErrIsTimeout` is only a
    smoke test: a past deadline makes the socket time out, which already maps to `timeout`, so it does not guard the
    clause that counts a passed deadline as ended. Known: a stall AFTER the host key was trusted is reported in step
    `connect` (the stalled case is checked first), not `auth`.
25. **Get checks the byte count.** The size from the server's `Lstat` is remembered; a stream that ends at another count is
    `server_error`, so a server that caps read lengths below the packet size (a short read) or a file that changed
    cannot look like a clean stream. The caller's hash check is still the final proof.
26. **More not verified / known gaps (round 2):** concurrent-read behaviour against servers that cap read length (guarded
    only by the size check of note 25, never run against such a server); an `OpenFile` reply that is lost leaves a temp
    file nothing knows about (`pending` is set only after the open succeeds); a server that refuses setstat leaves the
    umask mode on the temp file; `Delete` can remove an empty directory that carries a backup name (the client's `Remove`
    tries `rmdir` after a failed unlink); status 14 cannot be produced by the library's test server, so it is covered by
    a unit table only.

## Build notes: ticket 12 (LAUN-79): `restore.sh --remote`, `--remote-config` and the runbook

Code: `deploy/restore.sh`; test: `deploy/test-restore-remote.sh` (part of `make restore-test`); test tool:
`server/internal/offsite/restorefake` (never in the production image); runbook: `docs/deploy.md` "Restore from a
remote copy". No migration, no change to the Go library. Judgment calls the design left open (the safest default each time):

1. **Flags added to #39's list:** `--install-id` (needed with `--remote-config` unless the database knows it), `--image`
   (the server image; the default is the stack's `offsite` or `app` container's image; with `--container` and no stack it is
   REQUIRED), `--verify-only`, `--no-host-age`, `--fetch-network` (tests only, as the ticket says). The key comes from
   `--identity FILE`, `--identity -` (one line of the standard input) or a hidden prompt on a terminal; with none of them the
   script stops. The key is validated as `AGE-SECRET-KEY-1[A-Z0-9]+` and is never echoed.
2. **The key is read before any docker call.** `docker exec -i` would otherwise read what is left of the standard input.
3. **The private key and files.** Host `age` present (and no `--no-host-age`): the key goes to `age -d -i -` on a pipe from a
   shell builtin (no file, no argument). Otherwise a key from stdin or the prompt is written to `<work folder>/identity`
   (mode 0600, the 0700 work folder, removed at exit) and mounted read-only into the `decrypt` container. A key file named with
   `--identity FILE` is mounted as it is (a warning when it is readable by others). Not measured: the host `age` branch (no
   `age` on the test host).
4. **A whole decrypt pass before any database is made.** After the signature, binding, sha256 and database checks the script
   runs `decrypt` to `/dev/null`, so a wrong key, a changed byte or a cut object fails with no scratch database. The real
   restore decrypts a second time into `pg_restore`. The cost is one more decrypt pass (not measured on a large dump).
5. **`latest` and the binding.** `verify --install-id --stem` needs a stem, and `latest` has none before the fetch. The script
   reads the manifest's `object` and `dumpTime` with `python3 -I` BEFORE it is trusted, checks both against a strict regular
   expression and uses the stem only to ask `verify`; `verify` then authenticates the same manifest and binds its install id,
   object name and dump time. So for `latest` the stem is not independent evidence; the install id is, and so are the sha256 and
   the signature. A swapped pair for `latest` fails on the object hash. For an explicit STEM the manifest must name that stem.
6. **Freshness.** The printed time is the SIGNED manifest's `dumpTime` (`verify` output), with its age; `--trust-remote` prints
   the unsigned one marked UNVERIFIED. `latest` adds a fixed paragraph that the signature does not prove freshness. The script
   does not judge the age.
7. **The database record.** When the live database has a `backup_runs` row (`status='succeeded'`, `remote_name` = the object
   name) its `sha256` must equal the object's; a mismatch stops before decrypt. No row, or no table: a line says so and the
   restore goes on. A restore onto a new host has no row, so this check only runs where the original database is alive.
8. **`--remote-config` without `--container`** uses the stack like every other mode (it needs `deploy/.env`). With
   `--container` the script never loads `deploy/.env`, checks no compose project and runs no `cid` lookup (tested with a
   missing env file). `--remote NAME` with `--container` is refused: the destination is read from the stack's database.
9. **Throwaway commands** (`dk run`): `--rm --pull never --read-only --cap-drop ALL --security-opt no-new-privileges
   --log-driver none`, `--user <caller uid:gid>`; fetch on `bridge` (or `--fetch-network`), unpack/verify/decrypt on
   `--network none`. The test asserts these flags from a recorded argument log. `--log-driver none` is on `fetch` and
   `unpack` too (they carry ciphertext); the design required it for `decrypt` and `keygen` only.
10. **pipefail.** The fetch/unpack pipeline sets `set -o pipefail` on its own line and the test runs a mutant without it:
    a fetch container that exits 1 after a whole frame fails the real script and does not fail the mutant.
11. **Paths.** A path with `:` or `,` is refused (docker `-v` would misread it). The `--remote-config` file must be a regular
    file, not a link, owned by the caller, with no group or other permission bits.
12. **Test shape.** The remote is the `restorefake` S3 server in a container on a user network AND connected to the default
    bridge, reached by IP with `--allow-cidr` (the gateway stays refused). The server image of the test is built from the
    repo's `./cmd/server` in a `golang:1.27.3` container (cache volumes `dev-ops-gomod`, `dev-ops-gocache`, override with
    `RESTORE_TEST_GOMOD_VOLUME` / `RESTORE_TEST_GOCACHE_VOLUME`) on the production runtime base. It uses neither the
    `backuptest` driver nor the silo. The `--remote NAME` path runs through a docker shim that answers `compose ps` and
    `compose exec`; it proves restore.sh's calls, not a real offsite container. The `no_space` case turns the unpack folder
    into a 1 MB tmpfs. The planted-secret test greps the argument log, the shim's environments, all output and `ps` samples
    every 200 ms for the age keys and the destination keys.

Not verified: any real destination or provider; Linux (bind-mount ownership, the default bridge's gateway rule, `-perm`
and `stat` forms were only run on macOS); the host `age` branch; the hidden prompt (no terminal in the test); a dump of
production size or time; `--remote NAME` against a real `offsite` container; the `docker build` recipe on a host without the
dev-ops cache (it uses `deploy.sh`'s own context `server/`, compared by reading, not built on a clean host).

Known gaps / follow-ups (ticket 12):
- `--identity -` on a terminal echoes the key (use `read -s` when `-t 0`).
- No warning when `backup_runs` holds a succeeded run newer than the chosen stem (a rollback of `latest`).
- `--trust-remote` is a flag plus a warning only (no typed confirmation).
- No disk-space check on the host work folder beyond `unpack`'s own.
- The `--remote-config` file is checked, then mounted later (a check/use gap).
- Any database error in the `backup_runs` lookup is treated as "no record" (note 7).
- Untried: the host `age` branch, the hidden prompt, Linux, a real remote, a real `offsite` container for `--remote NAME`,
  a production-size dump, and the `docker build` recipe on a clean host.
- Test reliability: the planted destination secrets are random per run, and a leak mutant proves the check can fail.

## Build notes: ticket 14 (LAUN-80): the attachment backup

Built: `internal/offsite/files.go` (the phase), `silosrc.go` (read-only silo client), `internal/backup/filesmanifest.go`
(the signed files manifest and the list format), `internal/backup/remotename/files.go` (the names), the CLI additions in
`fetch.go` / `cli.go`, the status fields in `internal/backupapi`, and the deploy changes (`files_net`, the read-only silo
user, `check-compose.py`). Decision #38 is the design; where it was open, the judgment calls below fix it.

1. **The files copy is the LAST PHASE of a backup run, not a run of its own.** After the dump is uploaded and verified, under
   the same destination lock, lease and heartbeat. No migration, no `kind` column, no new trigger. The result is a `files` entry
   in `backup_runs.steps` (`ok`, `code`, `objects`, `uploaded`, `uploadedBytes`, `bytes`, `skipped`, `failed`, `gone`,
   `repaired`, `initial`, `ms`). A failed or partial files phase never fails the run, because the dump is already safe on
   the destination and a retry of the whole run would send it again: the run stays `succeeded` and its `error_text` says
   `files_failed` (the phase stopped) or `files_incomplete` (it finished but skipped or could not read some objects). A
   dump `prune_failed` warning wins over a files warning in `error_text` (`doBackup` keeps the first warning); both are visible in `steps`. [LAUN-81 note 5: a files retention failure is not a files warning of this kind.] The run timeout of a run
   becomes `RunTimeout + FilesTimeout` (default 4 h + 12 h) when silo is configured. The first full copy of a large bucket may
   take hours; "first full size and time" is the `initial: true` step (`bytes`, `uploadedBytes`, `ms`).
2. **Two sub folders, flat names.** Drivers have no sub folders in their interface, and WebDAV, SFTP and Dropbox list the
   WHOLE folder (cap 16 MiB), so one name per attachment in the dump folder would break the dump listing and retention. The
   phase opens the SAME driver twice with `folder` extended (`subFolderConfig`, validated again by the driver):
   `<folder>/launchpad-files-<install>` (lists and manifests) and `<folder>/launchpad-files-<install>-objects` (one age file per
   attachment). Neither starts with `launchpad-<install>-`, so S3's prefix listing of the dump folder never walks them
   (`TestFiles_RealS3DriverSubFolders`). Names: `launchpad-files-<install>-obj-<32 hex>.age` (the first 16 bytes of
   `sha256(silo key)`, as #38 says), `launchpad-files-<install>-list-<time>-<4 hex>.list.age` and `....json`. `remotename.Valid`
   accepts them; the dump `Matcher` does not, so dump retention treats them as foreign and never deletes them.
3. **Offsite cannot read its own earlier list, so it keeps a STATE file.** #38 says "compare with the keys in the last files
   manifest", but that manifest is age encrypted to the admin's PUBLIC keys: offsite holds no private key and cannot open it. The
   state is `files-state-<destination id>.jsonl` on the scratch volume (`offsite_work`, mode 0600, one JSON line per object, the
   same line format as the list, written atomically). It is bound to the install id and to a hash of the destination's config
   (a changed folder, bucket or host makes it invalid). Any problem (missing, damaged, another install or config) means NO state
   and a full copy under the same names, never a guess. It holds silo keys, so it is as sensitive as the scratch files (same
   volume, same user). Consequences, for ticket 15: **the authoritative `lastSeen` for retention is this state, not the
   encrypted list** (offsite cannot read the list; the list carries the same values for a restore by an admin). A lost state loses
   the `lastSeen` of objects that are already gone from silo, so they are never pruned (safe, costs space). The state of a deleted
   destination stays (small); nothing sweeps it.
4. **Manifest format: a signed manifest over an encrypted list (two files), not one file.** The list is one JSON line per object
   (`key`, `size`, `etag`, `modified`, `sha256`, `remote`, `cipherSize`, `cipherSha256`, `lastSeen`), age encrypted to the same
   active recipients as a dump and streamed (never held whole). The manifest is the same envelope as a dump manifest (payload +
   Ed25519 signature over a domain prefix and the stored payload bytes) over `{version, installId, list, sha256, size, time,
   objects, bytes, launchpadVersion}`. The domain is DIFFERENT (`launchpad-backup-files-manifest-v1`), so a dump manifest never
   verifies as a files manifest and the other way round (both directions are tested with a payload valid for the other kind).
   It stays under the 1 MiB manifest cap whatever the number of objects. Order: objects, list, manifest; after the manifest the
   phase lists the list folder and compares both sizes. A failure after the list went up deletes only the two names this run chose.
5. **A new list and manifest pair is written EVERY run, even when nothing changed.** `lastSeen` moves on every run for every
   object still in silo; a run that skipped the write would leave a stale `lastSeen` that makes ticket 15 prune too early. The
   pairs are not deleted by the copy; LAUN-81 deletes those past the retention window. Cost: one list per run (about 300 bytes an object, compressed little: it is age).
6. **Change detection is `size` and `etag` from the silo listing.** `modified` is recorded and not compared. Silo objects are
   written once, so a changed key is rare; when it happens the object is copied again under the SAME remote name (a hash of the
   key), so the old remote content is replaced. An object that changes or vanishes WHILE it is read (the length differs from the
   listing) is skipped and counted (`skipped`); the next run sees the new size and etag.
7. **Scope of the bucket and the caps.** Only keys under `org/` (what the app writes) are listed, and they are all copied: the
   instance is one org, and keys carry the org id, so two orgs' same file name never share a remote name. A key that is not valid
   UTF-8, has a control character or is over 1024 bytes, or an object over 4 GiB, is skipped and counted (`files_incomplete`); a
   folder marker (a key ending in `/`) is ignored. At most 200 000 entries in a list (the state and the previous list are held in
   memory for the compare, about 100 MB at the cap); more is `too_large` and the phase stops. The cap counts entries that are gone
   from silo too (they stay in the list until pruned, LAUN-81).
8. **Spot repair of the destination.** Each run lists ONE shard of the object folder (the objects whose name starts with
   `...-obj-<2 hex>`, the shard is the day number mod 256) and queues for copying again every carried object of that shard that is
   missing or has another size. A full pass takes 256 days. A destination whose folder listing is `too_large` (WebDAV, SFTP and
   Dropbox list the whole folder) skips the check; any other listing error stops the phase. `repaired` is in the step.
9. **One object at a time.** Silo -> age -> scratch file -> destination, sequentially. Memory is one age chunk plus the
   maps of judgment call 7; disk is the largest object (`no_space` if the scratch volume cannot hold the next one, checked per
   object). A stall of the silo read or the upload is `stalled` under the run's `StallAfter`. A silo read error (not the
   destination's) counts as `failed` and the phase goes on; ten in a row stop it. A destination or scratch error stops it at once.
   The state is saved every 500 objects or 30 s and when the phase stops, so a crash or a timeout resumes from there.
10. **Silo access.** `offsite` gets `S3_ENDPOINT` (fixed `http://silo:9000`), `S3_BUCKET`, `S3_REGION`, `S3_ACCESS_KEY`,
    `S3_SECRET_KEY` (all four or none, `config.LoadOffsite`; none means no files phase). The key is a silo user made for offsite
    alone: `deploy.sh` generates `OFFSITE_S3_ACCESS_KEY` / `OFFSITE_S3_SECRET_KEY` into `deploy/.env` once (never again, appended
    to an older file by the top-up), and `silo_bootstrap` makes the user with the policy `devops-offsite-ro`
    (`s3:GetObject`, `s3:ListBucket`, `s3:GetBucketLocation`; no put, no delete), on the same stdin as the app's secret. It is
    proven on the dev silo that this user cannot write or delete (an ad hoc test, not kept; the pinned production image was not
    run). All silo traffic uses `backup.SiloDialer` (one host and port, checked before resolution; `TestSiloSource_OtherHostRefused`
    sends requests to other servers and addresses). `check-compose.py` now allows exactly those five variables on `offsite`
    (`S3_PUBLIC_ENDPOINT` is refused), refuses the app's key pair and silo's root pair as offsite's, requires the app's bucket and
    the endpoint above, and requires the networks of #38: `files_net` (`internal: true`) holds exactly `silo` and `offsite`;
    `offsite` is on exactly `offsite_db`, `egress` and `files_net`; `silo` on exactly `edge` and `files_net`. Mutations for each
    rule are in `deploy/test-compose-check.sh` (83 cases).
11. **Status.** `GET /platform/backups/status` has `filesStored` (this server has the object store) and `filesNotBackedUp`
    (an enabled destination with no run that finished `succeeded` with an `ok` files step in the last 48 hours; test, probe, verify
    and exchange runs do not count) and the banner `files_not_backed_up`. A server without attachments never says it. Until the
    first good copy the page says it ("files not backed up"). The stale alert MAIL is unchanged (it follows the dump); a files
    mail is not built. A run notice mail does not mention the files step.
12. **Restore of one object (no `restore.sh` change).** The offsite commands read it: `fetch ... --files [--stem STEM|latest]`
    (a frame: the signed files manifest, then the encrypted list), `verify --files --public-key F --manifest F --object F
    [--install-id ID --stem STEM]`, `decrypt` (the list), `files-entry --install-id ID --key KEY` (reads the decrypted list on stdin
    and prints `remote<TAB>sha256<TAB>size`), `fetch ... --file-object NAME` (a frame with an empty manifest and one object),
    `unpack`, `decrypt`. `TestFiles_IncrementalManifest` and `TestFiles_RealS3DriverSubFolders` walk it. `restore.sh --remote
    --files` into a silo volume is ticket 15; the acceptance line "a restore of one object works with `restore.sh --remote`" is
    met by these commands (the same fetch, unpack, verify, decrypt chain), not by a new restore.sh flag. This is a judgment call.
13. **(Changed by LAUN-81: retention now deletes, notes of ticket 15.)** Nothing is deleted on the destination by the copy itself except the two names a failed run chose itself (judgment call 4). Orphans
    (objects uploaded by a run that crashed before its manifest, an overwritten object's older content is not an orphan: it is
    gone) are not listed or removed; the next run lists nothing of them. An orphan object has a deterministic name, so the next
    run that copies the same key overwrites it.

Tests: unit (`internal/backup/filesmanifest_test.go`, `remotename_test.go`, `internal/offsite/files_test.go`, `internal/config`) and
DB + silo (`internal/offsite/files_integration_test.go`, `internal/api/backups_notices_integration_test.go`
`TestStatus_FilesNotBackedUp`). Mutants run by hand and killed: always upload; `lastSeen` of a gone object moved; no `org/` prefix;
state not bound to the config; manifest signed in the dump domain (needed the cross-domain payload test); silo client on a
default dialer; list accepts a foreign remote name or a duplicate key; no spot repair; a files failure failing the run; stale
threshold 72 h; no state save on abort.

Not verified: any real remote or real offsite host; the pinned production silo image's `mcli` against the new policy lines
(run only on the dev compose silo); `deploy.sh` end to end on a stack (`make deploy-test` stubs docker); that `files_net`
isolates on Linux (compose-check proves the file, not the kernel); WebDAV, SFTP and Dropbox with a nested folder (only the S3
driver and the test driver ran; the folder rule is the drivers' own); a bucket of tens of thousands of objects or a first copy
of hours; the 200 000 entry cap and the memory of the maps at that size; restore of a real bucket. Known gaps: no files notice
mail; retention and `restore.sh --files` came with LAUN-81; a changed object replaces its remote copy; the state is not backed up.

### Review round 1 of ticket 14 (LAUN-80): changes and added notes

14. **Dump retention runs BEFORE the files phase.** The dump is already verified, and a long first files copy must not spend the
    run's deadline that prune needs (it failed `prune_failed` on an expired deadline, with a stale handle). `TestFiles_PruneRunsBeforeTheFilesPhaseUsesTheDeadline`
    makes the files phase use up the deadline and checks that prune deleted the oldest pair. A silo `get` that fails because the
    run's own deadline ended is now the run's timeout code, not a counted silo failure.
15. **The silo listing cap is per PAGE, not in total.** One page is at most 8 MiB (`siloListMaxPage`; 1000 keys of 1 KiB are about
    1.5 MiB). The whole listing is bounded by the page limit (400 pages) and `MaxFileEntries` (200 000). The first build capped
    all pages together at 16 MiB, which fails at about 55 000 objects, forever (`files_failed`). Tests list 60 pages (about 21 MiB)
    and refuse one oversized page.
16. **`lastSeen` moves for every key still in the silo listing**, also when its copy failed, was skipped (too large) or is queued
    for a copy that did not finish. Only a key gone from silo keeps its old value. Ticket 15 may therefore prune by
    `lastSeen` in the state without the risk that a live key with a failing copy looks old. The state is saved on every abort, so
    the new value survives a failed run (`TestFiles_LastSeenMovesForLiveKeysWhoseCopyFailed`).
17. **Notes, no code.**
    - *State size.* The state is rewritten whole at each save (every 500 objects or 30 s, and at an abort). At 200 000 entries
      that is about 60 MiB of JSON lines a save; a first copy of that size rewrites it a few hundred times. This costs disk
      writes on the scratch volume, not memory beyond the map. Raise `filesStateSaveN` if a real install shows the cost.
    - *State files of deleted destinations* (`files-state-<dest id>.jsonl`) are never removed. They are small and harmless; an
      operator can delete them from the `offsite_work` volume.
    - *Duplicate keys in a hostile listing* are dropped in the list callback BEFORE anything is stored (a repeat is counted as
      `skipped`, so the step says `incomplete`). Round 2 found that the first fix only capped `len(seen)`: a silo repeating one key
      on 400 pages queued every copy in `todo` (millions of entries, out of memory). `TestFiles_RepeatedKeyIsQueuedOnce`
      sends the same key 40 000 times and checks one GET and one upload.
    - *Run time.* With silo configured the WHOLE run, the dump upload included, gets `RunTimeout + FilesTimeout` (default 16 h)
      before `errRunTimeout`; the stall watchdog still guards a hung upload.
    - *Object keys* (user filenames) appear only in the local state file and in the encrypted list. No log line, step or error text
      holds one (`TestFiles_ObjectKeysNeverLeak`).

Added tests: `TestFiles_EtagOnlyChangeIsCopiedAgain`, `TestFiles_TenSiloReadFailuresInARowStop`,
`TestFiles_IncompleteIsReportedAndPruneFailedWins`, `TestFiles_PruneRunsBeforeTheFilesPhaseUsesTheDeadline`,
`TestFiles_ObjectKeysNeverLeak`, `TestFiles_LastSeenMovesForLiveKeysWhoseCopyFailed`, `TestSiloSource_ListManyPagesIsNotCappedInTotal`,
`TestSiloSource_OnePageOverTheCapIsRefused`, `TestLoadState_InstallIDAloneIsChecked`, the abort-save assert in `TestFiles_CrashAndResume`,
and case 18b4 of `deploy/test-env.sh` (the off-site silo policy has the three read actions only). Mutants killed: etag ignored; ten
failures changed to 1000; the files warning winning over `prune_failed`; prune after the files phase; `files_incomplete` not
reported; no state save on a destination abort; a key logged on a silo failure; no `lastSeen` bump; the install id not checked in
`loadState`; the old cumulative list cap; no per-page cap.

## Build notes: LAUN-155 and LAUN-156 (two flaky tests)

- `deploy/test-offsite.sh` B3 killed `backup.sh` in a container that has no init, so postgres
  is PID 1. The orphaned `pg_dump` shim became a child of the postmaster; when its `sleep` was
  killed the shim ended with status 137, the postmaster took it for a crashed backend and went
  into crash recovery ("the database system is in recovery mode"). The shim now ends with
  `exit 0`, and the script waits (bounded) for the shim to be gone and for the database to
  answer. Under load 7 of 10 runs failed before, 0 of 10 after.
- The WebDAV `Put` with a source LONGER than the declared size could succeed (a cut copy):
  `net/http` finds the extra byte only after it has written the declared bytes, and a server
  that answers on those bytes wins the race. `sized` now reads one byte ahead before it hands
  over the last declared bytes and fails the request when there is one
  (`TestLongSourceNeverSucceedsEvenIfServerAnswersFirst`).
- Follow-up to LAUN-155 and LAUN-156: `exit 0` in the shim is the real fix for B3. The psql
  poll is a backstop, and the pgrep poll alone fixes nothing. A direct table test of `sized`
  (`TestSizedReader`) pins the reader on its own.
- The S3 driver has the same race (a source longer than the declared size can succeed) and is
  NOT fixed here; a follow-up ticket covers it. The SFTP and Dropbox drivers were not analysed.

## Build notes: ticket 15 (LAUN-81): attachment restore path and files retention

Built: `internal/backup/filesretention.go` (the pure plans), the prune in `internal/offsite/files.go` (`pruneGone`,
`pruneLists`) and `backuprun.go` (the window), `restore.sh --remote --files`, the `genfiles` command of `restorefake`, and
the tests (`filesretention_test.go`, `files_retention_integration_test.go`, the attachment section of
`deploy/test-restore-remote.sh`). No migration, no grant change (`offsite` already deletes through the destination driver,
not through the database), no new route, no openapi or web change.

1. **Deleting an attachment does not remove its silo object today (checked in the code).** There is no route that deletes an
   attachment or an issue's file; `s.store.Delete` is called only for an upload that failed the type or size check
   (`internal/tracker/attachments.go`), and abandoned uploads are not swept (T3b #9). So, as of this ticket, an object leaves the
   silo listing only when an operator removes it by hand or the rejected-upload path runs. The prune below is correct and
   tested, and will mostly find nothing to do until the app deletes objects. This corrects #38's "to be confirmed by ticket 15".
2. **The rule, and its two knobs (constants, not settings).** A remote attachment object is deleted only when its key was NOT in
   this run's silo listing AND its `lastSeen` in the LOCAL STATE (not the encrypted list) is older than the window. The
   window is `max(FilesKeepMin = 30 days, age of the oldest dump file on the destination + FilesKeepMargin = 1 day)`. A kept dump
   can name any object that was in silo when it was made; that object has a `lastSeen` no older than that dump's run (the files
   phase runs in the same run, right after the dump), so it lives as long as the dump. With the default `keep_last` 14 a daily
   schedule gives a window of about 30 days; a destination that keeps a dump 40 days has a 41 day window (tested with a 40 day
   old pair). No new setting: the ticket does not name one, and a setting would be another way to lose data.
3. **The window is read after the dump prune, from the destination listing.** `doBackup` lists the dump prefix again after
   `prune` and takes the oldest time in any matching name (a half pair counts: the larger window is the safe one). A listing that
   fails gives no window, and the phase prunes NOTHING that run (`pruneSkipped: true` in the step). A time that cannot be read, an
   empty `lastSeen` or one in the future keeps the object.
4. **Prune runs FIRST in the files phase, before the copy.** The state holds gone keys until they are pruned and the cap is
   200 000 entries, so a state full of gone keys would make `save()` fail `too_large` as new keys arrive, for ever. Pruning first
   shrinks the state before anything is added. (A state that is at the cap with LIVE keys only is still `too_large`: the
   bucket itself is over the cap.)
5. **Crash-safe order for one object:** delete the remote object; only when the destination says done (or `not_found`: it is
   already absent) drop the state entry; save the state after the pass. A crash between the delete and the save repeats the
   delete next run and counts it again. A delete that fails (any other code) KEEPS the entry and counts `pruneFailed`; ten
   in a row stop the pass. `PruneFailed > 0` alone leaves the step `ok: true` (see 16), code `prune_failed`, and `error_text` `prune_failed`
   (precedence: note 16). The run stays `succeeded`.
6. **A state line whose `remote` is not this install's object name is never deleted from the destination**
   (the name must equal `FilesObjectName(install, key)`, note 18); it is counted as a prune failure and kept.
7. **Old lists and manifests follow the same window.** After the new pair is written and verified, every pair in the list
   folder whose time (in its name) is older than `now - window` is deleted, manifest first, then list; the pair this run wrote is
   never deleted, even if the clock stepped. A half pair older than the window is deleted too. A listing or delete error is counted
   in `pruneFailed`. Because the window is at least the age of the oldest kept dump plus a day, the list written right after any
   kept dump is kept (restoring that dump's attachments needs that list). A list inside the window may name an object that was
   pruned later (it was out of silo for longer than the window); fetching that object is `not_found`, so restore from an OLD
   list can miss such objects. The newest list never names a pruned object (the pruned entries are out of the state before it is
   written).
8. **Step fields added:** `pruned`, `prunedBytes`, `pruneFailed`, `listsPruned`, `pruneSkipped`. The files step is still stored as
   JSON in `backup_runs.steps`; no schema, no openapi change (steps are opaque).
9. **Orphans are still not swept** (decisions note 13 of ticket 14): an object a crashed run uploaded before its entry reached the
   state has no state entry and is never deleted. A later change could list the object folder and delete names no state entry
   or list names after the window; not done, because the listing of a WebDAV/SFTP/Dropbox folder is capped (16 MiB).
10. **State files of deleted destinations** (`files-state-<id>.jsonl`) are still not removed (they hold silo keys, so they are
    sensitive as well as small). Handling is left to the operator and is in the runbook; a sweep in the worker would need the
    list of destination ids and a delete on the scratch volume, a small change for a later ticket. Not hidden: it is a known gap.
11. **`restore.sh --remote --files` writes plain files into a FOLDER (`--files-out DIR`), not into a silo volume.** Silo stores
    objects in its own on-disk format; writing into its data folder is not a supported way in. The folder is then uploaded with the
    app's own S3 key (runbook). This is a judgment call; it meets "a restore into a fresh silo volume matches the objects'
    sha256" as: every restored file's sha256 equals the one in the SIGNED list, checked by the script, before the file is moved
    into the folder; the upload step is the operator's.
12. **The order of `restore.sh --files` is the one of LAUN-79:** fetch the list, read the manifest, `verify --files` (signature,
    install id and stem binding, list size and sha256), a note that a files list has no database record, a whole decrypt pass of the
    list that writes nothing, then (and only then) the plain list goes to the 0700 work folder, every entry is checked (a key must
    start with `org/`, no `..`, no empty part, no control character or backslash, no repeat; the remote name must be this
    install's object name; both sha256 must be 64 hex), and the first byte is written under `--files-out`. Per object: fetch,
    check the stored size and sha256 against `cipherSize` and `cipherSha256` of the list, decrypt into the work folder, check the
    plain size and sha256, `mv` into place. A mismatch at any step empties what this run wrote and exits non-zero.
    It does NOT do a second decrypt-without-writing pass per object: the object is decrypted into the 0700 work folder and
    checked there; nothing reaches `--files-out` before it is known good. The list has one no-write pass (it proves the key).
13. **`--files-out` must not exist or must be empty** (never overwrite or delete the operator's files). The cleanup on failure
    removes only what is inside it, with `find -mindepth 1 -delete`, and only after this run began to write. `--verify-only`
    needs no folder. `--files-key KEY` restores one object. `--trust-remote` is allowed and warns that anyone who can write to the
    remote can put wrong files into the list.
14. **No object key reaches output, logs or steps:** the script says "object N of M". Keys are in the work folder's list and
    in the operator's `--files-key` argument only.
15. **Cost:** a fetch container, an unpack container and a decrypt (host `age` or a container) per object, about a second or
    more each. A bucket of tens of thousands of objects takes hours; restore one with `--files-key` or run it in a terminal
    multiplexer. A faster batch fetch would need a new offsite command. Not built.

Tests: unit (`internal/backup/filesretention_test.go`: window, plan, list plan, oldest dump); integration with the real silo and the
fake destination (`TestFilesRetention_PrunesByLastSeen`, `_KeptDumpWidensTheWindow` with a 40 day old kept pair,
`_DestinationErrorKeepsTheEntry`, `_PruneKeepsTheStateUnderTheCap`); `deploy/test-restore-remote.sh` section 5 (four objects, one
empty and one with a space; `--files-key`; `--verify-only`; `--remote NAME`; a non-empty folder; bad signature; wrong verify key;
changed list; wrong age key; stem and flag refusals; a damaged object in the middle; a swapped object; a signed list with a wrong
sha256; a failed fetch with the pipefail mutant). Mutants run by hand and killed: prune ignores the `seen` set; no window; a
failed delete drops the state entry; the kept dump does not widen the window; (restore.sh) the plain sha256 check removed; the
stored-copy check removed (the plain check still stops a swapped object); pipefail removed.

Not verified: any real remote or real offsite host; a bucket of thousands of objects or the 200 000 cap (the cap test adds 50 gone
entries, it does not build 200 000); prune of a WebDAV, SFTP or Dropbox destination (only the test driver and the fake were used for
the delete path; the drivers' `Delete` is their own); that the app ever deletes a silo object (note 1); the upload of a restored
folder into a fresh silo (not run: the runbook command is a plan); `restore.sh --files` with host `age` (the tests use
`--no-host-age`; the same `decrypt_run` as the dump path).

### Review round 1 of ticket 15 (LAUN-81): changes and added notes

16. **Retention failures keep the step `ok: true`.** The status query counts a destination as backed up only for a run with a files
    step `ok: true` (`db/queries/backup.sql`); a delete that fails does not make the copy incomplete, so it must not raise
    `files_not_backed_up` after 48 hours. A prune-only failure is the step code `prune_failed` and the run warning `prune_failed`
    (`filesOutcome`, unit tested). A skipped or unreadable object (`incomplete`) still makes the step not ok and wins in the step code and
    the files warning. In `error_text` the first warning wins: a dump `prune_failed` (dump retention), then the files warning.
17. **Guards: an empty or untrusted listing never prunes (blocking review finding B1).** `PlanFilesPrune` returns no keys and a fixed
    reason, shown as `pruneSkip` in the step, when (a) the silo listing was empty but the state is not (`empty_listing`: a wrong bucket,
    a recreated silo volume, a silo that answers 200 with nothing); (b) the listing held keys the phase could not read, `bad > 0`
    (`bad_keys`); (c) the newest `lastSeen` in the state, read BEFORE this run bumps anything, is older than the window
    (`long_gap`: the files phase failed for longer than the window; this run bumps what is still in silo and the next run prunes).
    (d) Cap: one run deletes at most `FilesPruneMaxFraction` = 50 % of the state entries, rounded down, oldest `lastSeen` first.
    So no single run empties the remote, and a state of one entry is never pruned. The constants are judgment calls; the
    window-based design alone trusted one listing completely.
18. **Names.** `pruneGone` deletes only the name `FilesObjectName(install, key)` derives; a state line with any other name is kept and
    counted (`loadState` already drops a whole state with such a line, so this is a second guard). `restore.sh` checks the same rule on every
    list entry. `pruneLists` keeps a list whose manifest delete failed.
19. **`restore.sh --files` checks `--files-out` again right before its first write** (it may have been filled while the fetch ran).
    Nine hostile SIGNED lists are in the test (`..`, leading `/`, empty part, backslash, newline, duplicate key, a 256 byte part, a
    foreign object name of this install, an object name of another install): each is refused with nothing written.
20. **Recorded risks, not fixed.**
    - *Margin (reviewer B N3).* The window assumes the files phase succeeded around each kept dump: an object's `lastSeen` is no older
      than the run that made that dump only if that run's files phase ran. If the phase failed for many runs, `lastSeen` of gone
      objects is older than the dump that could name them. The `long_gap` guard and the 50 % cap soften this; they do not remove it.
    - *Crash window (A N4).* A crash after a delete and before the state save repeats the delete next run (absent counts as done).
      Between the two, the list written by the last good run still names the deleted object.
    - *Two destinations on one folder (A N5).* Two destinations that point at the same folder and install id share object and list
      names but each has its own state; one's prune deletes objects the other still lists. Do not point two destinations at one folder.

Tests added: `TestPlanFilesPrune_Guards`, `_CutoffBoundary`; `TestFilesRetention_EmptyListingNeverWipes`, `_LongGapBumpsFirstAndPrunesNextRun`,
`_UnreadableKeyInTheListingSkipsPrune`, `_CapsOneRunAtHalfTheState`, `_ForeignRemoteNameIsNeverDeleted`, `_ListIsKeptWhenItsManifestStays`,
`TestFilesOutcome_Precedence`, and the hostile-list section of `deploy/test-restore-remote.sh`. Mutants killed: no empty guard, no bad-keys
guard, no gap guard, no cap, cutoff moved by a second, no foreign-name guard, list deleted after a failed manifest delete, a prune
failure making the step not ok.

### Review round 2 of ticket 15 (LAUN-81): changes and added notes

21. **`mass_gone` guard.** A pass is skipped (`pruneSkip: mass_gone`, `pruneSkipped: true`) when the keys it would prune (not seen,
    past the cutoff) are MORE than 50 % of the state entries (state of two or more). The 50 % cap and `long_gap` slow the erosion
    by a silo that keeps listing a subset but do not stop it (simulated by the reviewer: 1000 entries fall to 10 in about a week, and
    "run now" repeated reaches the end in a day). The conservative direction is right because the app cannot delete silo objects today,
    so a legitimate mass disappearance does not occur. The cap (rounded down) stays as a second guard, though after this one it
    cannot bind. An operator who really removed the objects clears it by deleting the state file of that destination
    (`docs/deploy.md`): the next run copies everything again and prunes nothing.
22. **A future `lastSeen` is unreadable for the gap check.** `NewestLastSeen(state, now)` ignores a value after `now`, so a planted
    far-future `lastSeen` cannot freshen the state and defeat `long_gap`. The entry itself is kept (it is never past the cutoff).
23. **Recorded risk (reviewer A, finding 3).** The window is read from the dump listing after the dump prune. A PARTIAL dump listing
    (a destination that lists fewer files than it holds) makes the oldest dump look younger and the window shrink toward 30 days.
    The guards above (50 %, `mass_gone`, `long_gap`) bound the damage; they do not remove it. A listing error means no pruning.
24. `restore.sh`'s list check is now an `if ! python3 ...; then die; fi` (same refusal text). A second emptiness check of
    `--files-out` is tested with a fetch stub that fills the folder while the list is fetched, and a mutant (`:` in place of the check)
    is caught.

Tests added: `TestPlanFilesPrune_MassGoneSubsetNeverErodes` (1000 entries, 10 seen, 10 repeated passes), `TestNewestLastSeen_IgnoresTheFuture`,
`TestFilesRetention_MassGoneNeverErodes` (real silo, 10 runs, then the state file removed), `_FutureLastSeenCannotDefeatLongGap`,
`_HalfOfTheStateMayGo`, and the late-fill step of `deploy/test-restore-remote.sh`.

### Touch-ups after the round 3 review of ticket 15 (LAUN-81), test and doc only

25. **Boundary of note 21.** `mass_gone` needs a state of two or more entries. A state of ONE entry never prunes: with one candidate
    and a cap of zero (rounded down) nothing is deleted, so the "cap cannot bind after `mass_gone`" claim of note 21 holds for
    states of two or more only. Tests: a two-entry state with both entries past the window is `mass_gone` (a mutant with `>= 3` fails),
    and a one-entry state prunes nothing.
26. **Recovery period and dilution.** The first 30 days after silo loses objects are the recovery period. A silo that loses less
    than half of its objects and stays that way for the window gets exactly those remote copies deleted; they may be the only copy and
    may be named by the newest dump. Nothing alerts on the `gone` count of the files step. A growing bucket dilutes the `mass_gone`
    ratio. The runbook says so.
27. A test now isolates the hostile-list validator: with the path-part check removed from `restore.sh`, the `..` list is no longer
    refused with "not safe to restore" (it fails later, on the fetch), so the hostile section proves the validator by itself.

## Build notes: ticket LAUN-158 (long source on S3, SFTP, Dropbox)

- One shared reader now does the look-ahead: `driver.Sized(src, size)` in `internal/backup/driver/sized.go`
  (moved from the WebDAV `sized`). It fails a short source with `io.ErrUnexpectedEOF` and a long one with
  `driver.ErrSourceLong`, and reads one byte ahead (`io.ReadFull`, so a `(0, nil)` read is retried) before it
  hands over the last declared bytes. `ErrSourceLong` is a plain error with a fixed text; `MapNetError` turns it
  into `server_error` (no status, no remote text). `TestSizedReader` (table: sizes 1 to 32769, buffers 1 to 100000,
  short / exact / long by 1 and 5, one-byte, data-err, half and (0, nil) readers) moved there from `webdav`.
- **S3: was broken.** `ContentLength = size` with a reader that only checked for a SHORT source, so the same race as
  WebDAV applied; the post-check (`HEAD` size equals declared size) compares the stored size with the declared one and
  cannot see a cut copy. `Put` now uses `driver.Sized`. `TestLongSourceNeverSucceedsEvenIfServerAnswersFirst` (s3)
  failed on the old code ("a source longer than its declared size succeeded") and passes now.
- **SFTP: not affected.** The copy runs on the client side: `io.Copy` over `io.LimitReader(src, size+1)` into the file,
  and the call fails (`dump_mismatch`) unless exactly `size` bytes were copied and then the source ended. No transport
  probes for the extra byte, so there is no race; the temp file is removed and nothing is renamed on a mismatch.
  `TestLongSourceNeverSucceedsEvenIfExtraByteIsLate` pins it (it passes on the old code too).
- **Dropbox: not affected.** Chunks are read into memory with `io.ReadFull`, and `noMore` reads one byte ahead BEFORE the
  single upload call or the last piece (`session/finish`) is sent, so a long source fails (`dump_mismatch`) before the
  commit; the commit reply is also checked for size and `content_hash`. A middle chunk may already be appended, but a
  session never committed is not a file. `TestLongSourceNeverSucceedsEvenIfExtraByteIsLate` pins both paths (it passes on
  the old code too).
- Mutants of `Sized` killed: no look-ahead; `Read` instead of `ReadFull` for the look-ahead; no short-source check; no
  clamp of the buffer to the bytes left; a look-ahead error swallowed; a byte past the size handed over
  (`TestSizedReadPastTheSizeIsRefused`). Not verified: a real S3 service (only the fake and silo).
- This supersedes the LAUN-155/156 note line saying the S3 driver is not fixed: S3 is fixed here (the older text stays, append-only).
- Declared size 0 (review round): S3 and WebDAV sent `http.NoBody` and never read the source, so a non-empty source
  succeeded as an empty object. `driver.NoMore(src)` now reads one byte (`io.ReadFull`) before the empty request: a byte is
  `ErrSourceLong` (`server_error`), end of source sends `NoBody` as before, so a truly empty dump still succeeds
  (`TestSizeZeroSource` in s3 and webdav failed on the old code; `TestNoMore`). SFTP and Dropbox already fail it.
- The stored run text differs by driver for a long source: `dump_mismatch` (SFTP, Dropbox) against `server_error`
  (S3, WebDAV). This is cosmetic: neither is retried (`driver.Retryable` accepts only Timeout, ConnectRefused, DNSFailed,
  Stalled and server_error with a 5xx status; this one has status 0).

## Build notes: LAUN-154 (SFTP replace without a delete window, part-file sweep, Dropbox Get check and token lock)

1. **Swap, not "refuse to replace".** Note 20 of ticket 9 is superseded. On a server without posix-rename whose plain rename
   refuses with the generic failure code (4), and only when the temp file is still there and the final name is a regular file
   (a directory or a symlink is never touched), `swapReplace` renames the old file to a second part name
   (`.launchpad-part-<rand>`, a fresh random), renames the new file in, then removes the second name. There is no moment in
   which the previous good copy has no name. We did not "refuse to replace": `Test` and a retry of one name need replace to
   work, and a refusal would turn a retried name into a permanent failure.
2. **Failure paths.** (a) The move out fails: nothing moved, the call fails, the old file is intact. (b) The new rename fails:
   the old file is renamed back. (c) The rename back fails too, or the connection is gone (a cancel or stall in the window):
   the old copy stays under its part name and `finish` makes ONE more try on a fresh 10 s connection: if the final name is
   empty it renames the old copy back; if the final name exists (the new file is in) it removes the leftover. If that fails
   as well, the old bytes remain under a part name until the sweep (note 4). (d) A failed removal of the second name never
   fails the Put (the new file is in place); the leftover is a part file and List never shows it.
3. **mtime of the second name.** A rename keeps the old file's modification time, which can be days old. The swap sets the
   second name's times to now (best effort, `Chtimes`) so a sweep never removes an old copy that a failed swap just put under
   a part name. If the server refuses setstat, that protection is missing: UNVERIFIED on NAS firmware.
4. **Sweep.** After a SUCCESSFUL `Put` (not `Test`, not a failed Put), the driver lists the folder once and removes part files
   that a crash left. One rule, `sweepable`, and nothing else is removed: a REGULAR file (not a directory, not a symlink), a
   name that is the prefix plus 1 to 64 lower case hex digits and is not a valid backup name, and a KNOWN modification time
   (zero and epoch count as unknown) strictly more than 24 hours before `nowFn()`. At most 100 removals per sweep; the listing
   is bounded like `List`; any error is ignored and never changes the Put's result. A part file of exactly 24 h is kept. The
   server's clock and ours may differ by minutes; the age is a day. A server that reports no usable mtime never has a file
   swept. A clock that is wrong by more than a day on either side can sweep a live upload: an upload of over 24 hours is not a
   case this product has (a backup run has a shorter lease and stall limit).
5. **Tests with mutants.** Swap: success order and no leftover; new rename fails, rollback; rollback fails, `finish` restores; all
   renames fail after the move (old bytes kept under a part name); move-out fails; leftover removal fails; other status codes
   never start a swap. Sweep: the table rule (`TestSweepableRule`: 24 h boundary, zero and epoch times, directory, symlink,
   backup name, stranger), the real-file-system test (old part removed; 24 h, 23 h, old dump, old test file, bad suffix, no
   random, upper case, part-named directory and symlink kept), only after a successful Put, the cap of 100, a listing failure.
   Mutants run by hand and caught: the sweep boundary `<=`; the regular-file check dropped; the swap without the rollback;
   `finish` without the restore; the sweep removed; the Dropbox size check dropped; a waiter that ignores its context; a leader's
   cancel handed to the waiters.
6. **Dropbox `Get` checks size and `content_hash`.** The download reply's `Dropbox-API-Result` header (at most 64 KiB) carries
   `size` and `content_hash`. The stream is wrapped: more bytes than `size` fail at once, a clean end with another count or
   another content hash is `server_error`, and a failed stream stays failed. A reply with no usable header, or without one of the
   two fields, is not checked for that field (the runner's sha256 stays the backstop). UNVERIFIED: whether a real Dropbox always
   sends both on `/2/files/download` (the fake does).
7. **Dropbox token refresh does not hold the lock.** `accessToken` held `remote.mu` across the OAuth call, which can wait out
   429 replies for up to 30 minutes, so every other call and `Close` waited behind it with no way out. Now `mu` guards the cache
   only. One caller leads the refresh; the others wait on it with their OWN context (a waiter whose context ends leaves at
   once). A leader whose own context ended does not hand its error to the waiters: one of them leads the next try. A real
   refresh error is shared (waiters do not repeat a failing token call). Tests: eight callers, one token call; a waiter
   leaves on its timeout while the refresh blocks; `Close` returns during a refresh; a cancelled leader and a waiter that takes
   over; a refresh error is not repeated by the waiters.
8. **Not done / unverified.** The shared `driver.Sized` and `driver.NoMore` helpers of LAUN-158 were not merged when this was
   built, so nothing was duplicated or changed there. No real SFTP server without posix-rename was tried (all the swap tests
   use the in-memory server with a scripted refusal); the exact status code such a server sends for a refused replace is
   the assumption of note 20 (code 4). Sweeping of Dropbox, WebDAV and S3 leftovers is not part of this ticket.

## Build notes: LAUN-153 (follow-ups of the `restore.sh --remote` review, ticket 12)

Code: `deploy/restore.sh`; test: `deploy/test-restore-remote.sh`, section 6 (each new behaviour has a mutant that the test
catches). No migration, no Go change. Judgment calls the ticket left open (the smallest safe reading each time):

1. **The key on a terminal (item 1).** `--identity -` with a terminal on the standard input is the hidden prompt, the same
   as no `--identity`. Piped input is read as before. Verified on a pseudo-terminal only, not a person at a keyboard.
2. **A rollback is a warning, not a stop (item 2).** After the object is chosen, the script asks the live database for the
   newest `succeeded` run (by the time in its `remote_name`, any install id). If it is newer than the dump time of the chosen
   copy (the signed time when verified), it prints `ROLLBACK: ...` with both ages. A failed run does not count. The restore
   goes on: restoring an older copy is sometimes the point. It also warns for `latest` (a remote with an older pair put back).
   The comparison is on the fixed-width time strings.
3. **`--trust-remote` asks first (item 3).** The warning and a confirmation come BEFORE the fetch, after the install id is
   known. The words to type are the install id (`--yes-trust-remote ID` when there is no terminal; a bare flag is not
   enough, and the flag without `--trust-remote` is refused). The standard input is read for this only when it is a terminal,
   so it never takes what `--identity -` is about to read. This adds the flag `--yes-trust-remote`; `--verify-only` with
   `--trust-remote` now says `NOT verified` instead of `verify-only OK ... is signed`.
4. **Free space (item 4).** What can be checked is what is known: (a) the no-write decrypt pass now counts the plain dump
   bytes; before the scratch database is made, the free space of the database volume is checked against that count
   (a floor: the database is larger than its compressed dump) where PGDATA is visible, that is, in the postgres container
   (`--container`); in the stack's `backup` container it is not visible and the script says so and goes on; (b) for `--files`, before
   the first write, the file system of `--files-out` must hold the sum of the plain sizes of the signed list and the work
   folder must hold the largest object stored plus plain. The plain size of a database dump is not in the signed manifest, so
   `maxPlain 0` (trust mode) is covered by the measured count, not by a declared one. A `df` that cannot be read is a warning.
5. **The config file check (item 5).** The file is checked at its resolved path (a link as typed is still refused), before
   any docker call, and that path is what the container mounts. Its identity (device, inode, size, times, mode, owner) is
   kept and compared right before every fetch; a changed file stops with `changed after it was checked`. This closes the
   swap of the file between check and mount; it does not stop a process of the same user that changes the file's content in
   place within the same second (same inode, size and time), which that user could do anyway.
6. **The database record (item 6).** One query now returns the record and the newest time. A failed query (the database does
   not answer, no `backup_runs` table) prints a warning that the object was NOT checked and that this is not `no record`; an
   empty answer still prints `no record`. Neither stops the restore (a new host has no table). A garbled answer counts as failed.
7. **Item 7 (untested things).** Now tried: the hidden prompt and the typed confirmation, on a pseudo-terminal. Still NOT
   tried: Linux (bind-mount ownership, rootless uid, SELinux `:z`), `--remote NAME` against a real `offsite` container, a
   production-size dump, and the `docker build -t launchpad-api:restore server` recipe on a clean host. The host `age`
   branch: now tried on this host (a macOS arm64 build of the same age module; the test builds `age` for its host and checks no decrypt container runs). Not tried with a distribution package of age.

### Review-gate changes for LAUN-153 (made after both reviewers passed)

8. The test `df` shim runs the system `df` through `getconf PATH` (it could loop when `$TMPDIR` had `..` or a link). The pty runner waits
   30 s for each prompt, types nothing early, and exits 98 when a prompt never shows; the pipe-branch mutant is caught by that 98.
9. The `--files` fetch has its own config-swap test and mutant (`cfg_recheck` in `fetch_unpack`). The usage text and AGENTS.md name
   `--yes-trust-remote`.
10. For `--files`, when `--files-out` and the work folder are on one file system (same device), the two needs are added in one check.

## Build notes: LAUN-161 (the pty echo check flaked on a loaded CI runner)

- **Cause: a race in the test, not a bug in `restore.sh`.** One CI job failed `--identity - on a terminal echoed the key`. bash 3.2 (the same on the CI runner and a macOS dev machine) prints the `read -s -p` prompt BEFORE it turns terminal echo off. `ptyrun.py` typed the key as soon as it saw `hidden):`, so on a loaded machine the key went in during that gap and the line discipline echoed it. Reproduced without the script: a 4-line `read -r -s -p` script under three CPU burners echoed the key in 2 of 300 runs; with the fix, 0 of 300. Other jobs passed the same section, so it is a rare race.
- **Fix:** a step in the `ptyrun.py` steps file may have a third field `noecho`. The runner then waits until `tcgetattr` on the pty shows ECHO off before it types, at most 5 s. A script that never turns echo off (the `-s` mutant) is typed to after 5 s, so its echo is still seen and the mutant is still caught. The check that a planted key is not echoed is unchanged.
- Not verified: the real CI runner. `pty.fork` makes its own session, so the missing controlling terminal of a shell runner does not matter; a run with `setsid` and stdin from `/dev/null` passed locally.

### LAUN-174: a request body outlives `Do`

`net/http` may keep reading a request body from its own write goroutine after `Do` returns (an early reply, a
cancel). The Dropbox `Put` reuses ONE buffer for every chunk, so a late `Read` of chunk n could race with the copy of
chunk n+1 into the same buffer (a flaky `-race` failure in `TestFailedChunkIsResumed`, stack in `dialConn`, which
starts the write loop). The `ctx.Err()` check in `progress.Read` is not enough: it is check-then-copy. `once` now wraps
the body in `reqBody` and calls `stop()` before it returns; `stop` waits for a `Read` in progress and refuses every
later one. Any driver that reuses a buffer across requests needs the same. Wire bytes do not change.

## LAUN-175: stall tests without wall-clock margins

`TestStall/slow_but_moving_is_fine` (s3 and webdav) failed on a loaded CI runner: `stalled: the transfer stalled`.
Root cause: the test used a real 300 ms stall timer with 100 ms sleeps between reads, and the timer starts when the
request starts, so the TLS handshake and any scheduling delay came out of a 200 ms margin. A descheduled test
looked like a stalled transfer. `download stops` had the same flaw in its set-up: `Get` had to return inside the window.

- The s3 and webdav drivers build the stall timer through the package variable `newStallTimer` (production: `time.AfterFunc`,
  never changed). Tests use `fakeClock` to move time by hand. Moving is proven by progress: 10 reads, 100 ms of fake time each,
  against a 300 ms window, plus a count of timer resets. No real sleep, no retry.
- Tests that must see a stall without a set-up phase (`server never answers`, `server stops reading an upload`) keep the
  real timer: a stall is the expected result, so a slow host cannot fail them.
- Mutation check: with `touch` made empty, both `slow but moving` tests fail at once.
- sftp and dropbox have only tests that expect a stall; they need no change. A new positive stall test must use a fake clock.

## LAUN-176: backup tests without sleep-as-synchronisation

No production code changed. A sleep that guesses "the others have arrived" or "nothing else will come" passes
on a fast host and tests less on a slow one. Replaced with signals:

- **dropbox `laun154_test.go`** (`ConcurrentCallersShareOneRefresh`, `LeaderCancelledWaiterTakesOver`,
  `LeaderRealErrorIsSharedNotRepeated`): `waitWaiters` reads the goroutine dump until N goroutines are parked in
  `(*remote).accessToken` itself (the leader's top frame is the HTTP client). `WaiterLeavesWithItsOwnContext...` uses
  `manualDeadline`, a context the test ends (with `DeadlineExceeded`) after the waiter is parked, in place of a 150 ms timeout
  that could pass before the waiter started.
- **sftp**: `testServer.settle()` dials a canary (the accept loop is in order, so earlier connections are counted), then
  waits until every serve loop has returned. It replaces the 100-200 ms sleeps before the "no auth request", "never dialed"
  and "no credentials to a changed host key" checks. `TestDeadlineInAuthWindow...` holds the auth request on `authGate`
  (closed at cleanup) and needs no elapsed-time limit: the call must return while the server still holds it; the deadline
  is 1 s and the guard 60 s. `TestSweepNeverChangesTheError` closes the listener after the sftp session has started, not after 300 ms.
  This test no longer proves that a later sweep reconnect fails; it proves only the `stalled` mapping.
- Mutation checks: shared refresh removed (`flight` ignored): `ConcurrentCallersShareOneRefresh` fails ("only 0 of 7
  callers reached the wait"). Waiter ignoring its context: `WaiterLeaves...` fails.
- **Left alone on purpose:** `TestCancelDuringRetryAfterWait` (cancel is anchored to the server seeing the call; wait is 120 s),
  `lateReader` in dropbox and sftp (a late byte is the scenario; a slower host only makes it later),
  `TestWrongPinIsNotHiddenByAnExpiredContext` (the hook waits for the context), `TestPassedDeadlineWithoutErrIsTimeout`
  (deadline already passed), `ops_integration_test` 30 ms sleeps (they hold a lock open to widen a race), and the
  expect-a-timeout tests. offsite `LeaseEvery` 100 ms is a heartbeat tick against a 60 s lease: a slow host delays a renewal, it
  cannot lapse the lease. `StallAfter` 300 ms in `TestRun_Stalled` expects `stalled`, so an early stall gives the same result.
  The `files_*` 1.1 s sleeps wait for a one-second database clock (a floor, not a guess).
- Found by the stress run: the LAUN-175 `TestStall` (s3 and webdav) "download stops" subtest read the package variable
  `stallAfter` in a goroutine that outlives the subtest, while the parent test restores it (a data race under `-race`, seen at
  `-count=20 --cpus 1`). The goroutine now uses a local copy taken before it starts. Test-only.
