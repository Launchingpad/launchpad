# T46: Move an install to a new server (LAUN-248, 249, 251)

One record for the whole feature: the bundle library (`internal/move`,
LAUN-248), the HTTP side (`internal/moveapi`, `internal/api/move.go`,
LAUN-249), the deploy limits and docs (LAUN-251). The web UI (Settings > Platform > Move server, and "Restore from a move file" on /setup) is LAUN-250, described in `web-decisions.md`.
Docs: `docs/deploy.md` "Move to a new server", `docs/self-hosting.md`.

## The bundle (library)

1. **Format.** One age stream encrypted to a passphrase (scrypt). Inside:
   a magic line, then frames (type byte, 4-byte length, payload): the manifest
   first, then each table as COPY text from ONE repeatable-read snapshot, then
   the attachment objects (each followed by its sha256), then an end frame.
   `format` is 1; an unknown format is `bundle_invalid`. No temp file: the app
   root file system is read-only, so both directions stream.
2. **Registry.** `internal/move/registry.go` lists every table with a mode
   (copy, copy and re-seal, skip) and every sealed column. A drift test fails
   when the schema has a table or `*_enc` column the registry does not know,
   so a new migration cannot ship a table that silently stays behind. Skipped
   on purpose: web sessions (`api_tokens.kind = 'session'`), queues and
   outboxes (they would send twice), backup dumps, runs and the change log.
3. **Re-seal.** Sealed columns (`*_enc`) travel as PLAINTEXT inside the
   passphrase-encrypted stream: export opens each with the old server's
   keyrings, import seals again with the new server's keys. The two servers
   need not share `SETTINGS_ENCRYPTION_KEY` or `BACKUP_ENCRYPTION_KEY`. A
   backup secret goes through `platform.BackupKeyring` only. A missing key or
   an unreadable secret stops the export (`move_blocked`), never a bundle
   with holes.
4. **Passphrase rule.** The server makes it (160 random bits, 8 groups of 4
   base32 characters). A person never picks one. Shown once; never logged,
   emitted or stored; never in a query string; the CLI reads it from a hidden
   prompt or stdin, never an argument or an environment variable. Keep it
   apart from the file.
5. **Version rule.** Import needs the bundle's migration version to equal the
   new server's, so both servers run the same release. A mismatch is
   `409 bundle_version_mismatch` and rolls back. Restore into a newer
   migration version is a later ticket.
6. **One transaction, empty target.** Import runs in one transaction under the
   setup advisory lock, only into a database with no user. Any failure rolls
   back and deletes the objects it wrote, so the install stays in setup state.
7. **Backups are disabled after import.** Backup destinations come across with
   `enabled = false` and no `enabled_since`, and `backup_settings` gets a new
   `install_id` and `instance_id`: the new install is a new backup source and
   never writes to the old install's target by surprise. An admin turns each
   destination on again.
8. **No write freeze in v1.** The old server keeps taking writes while the
   export runs. The snapshot is consistent, but anything written after it is
   not in the bundle. The runbook says to stop using the old server (or take
   the final export at a quiet time) before the cut-over. Freezing writes is a
   later ticket.
9. **Event policy.** The move emits no event on either server: it is
   install-level, like the other install settings. The `events` table travels
   as data, so the new install keeps the old feed and its history. The package
   imports no `internal/notify`; the restored install does not replay
   notifications.
10. **Not moved.** Web sessions (everyone logs in again; the restore signs the
    restoring admin in), git webhooks (they still point at the old URL: edit
    each host), DNS and the outer proxy, the old server's backup files and
    queues.
11. **Single process.** The ticket store is in memory (rule 2 below), so one
    app process must serve both the start and the download. The supported
    setup is one.

## Routes (API)

1. `POST /api/v1/platform/move/export` (platform admin, human). Body optional
   JSON `{"includeAttachments": bool}`. `201` with `passphrase`, `ticket`,
   `downloadPath`, `filename`, `expiresAt`, `includeAttachments`. Never
   idempotent (`idempotency.Reject`): the answer holds secrets. No event.
2. `GET /api/v1/platform/move/export/{ticket}` (same caller). Streams the
   `.lpmove` file. No event.
3. `POST /api/v1/setup/restore` (public; the setup link is the credential).
   `201` with the `POST /setup` body plus `summary` (counts only).

## Rules

1. **Caller.** Both export routes sit in the `/platform` group
   (`RequirePlatformAdmin`: platform admin flag, admin scope, no org pin) and
   add `refuseAgents`, so an agent is `403` even when flagged. A member, an
   org-pinned token or another org's admin is `403`. The ticket is bound to
   the user who started it: another platform admin gets `404`.
2. **Ticket storage: memory, not a table.** One process serves an install,
   and the passphrase must outlive the POST (the file is encrypted at
   download time). A table would store the passphrase (a secret) or force the
   client to resend it. So `moveapi.Service` keeps `sha256(ticket)` ->
   `{user, passphrase, includeAttachments, expiry}` in a map. The ticket value
   is 256 random bits and is never held after the POST answer; the passphrase
   is held only until the ticket is used or expires (5 minutes). An expired
   ticket and its passphrase are deleted by a timer at expiry and by every
   start and lookup. A new start drops only the SAME user's older ticket: one
   live ticket per user, so another platform admin's ticket survives and the
   map stays bounded by the number of platform admins.
   A restart drops all tickets. No migration. Two app instances behind one
   database would break this; the supported setup is one.
3. **Single use, expiry.** The ticket is used when the stream starts (after the
   lock is won). Used, expired, unknown, malformed or another user's: `404
   not_found`, not told apart. `409 export_in_progress` (the lock was busy)
   does NOT use the ticket.
4. **One export at a time.** A session advisory lock
   (`pg_try_advisory_lock(hashtext('devops-move-export'))`) on a dedicated
   connection for the whole stream, so it also holds across instances and dies
   with a lost connection. A failed unlock closes the connection instead of
   returning it to the pool.
5. **Streaming.** The export writes through a 256 KiB writer to the response;
   the import reads the request body through a 1 MiB buffered reader. No temp
   file, no body buffer. The server's read and write deadlines are cleared for
   these two requests (`ResponseController`). The heap test moves 96 MiB with a
   heap growth of a few MiB. The passphrase KDF is the exception by design:
   age's scrypt (work factor 18) allocates about 256 MiB for a second, on the
   export and again on the import. Size the app container for that
   (`ExportOptions.ScryptWorkFactor` exists only so tests can see the stream).
6. **Errors before and after the first byte.** The download headers are sent
   with the first byte. A failure before it is a JSON error (`409 move_blocked`:
   key missing, stored secret unreadable). A failure after it aborts the
   connection (`http.ErrAbortHandler`) so a short file never looks whole.
7. **Restore: the link first.** `X-Launchpad-Setup-Token` (header) is checked
   by `setup.CheckLink` before the body is touched: malformed, wrong, expired,
   used or replaced is `401 invalid_setup_token`; a live link on an install
   with a user is `409 setup_complete`. `EnableFullDuplex` stops net/http from
   reading up to 256 KiB of the body before it answers. The route is outside
   `Authenticate`; no limiter and no `Idempotency-Key` middleware sit in front
   (`idempotency.Reject` only refuses the header). The link is consumed INSIDE
   the import transaction (`ImportOptions.Authorize`, after the setup lock and
   the empty check), so two restores cannot both use it, and a failed restore
   rolls the consumption back: the link still works.
8. **Passphrase input.** `X-Launchpad-Passphrase` header, or the FIRST
   multipart part named `passphrase` (then the part `file` is the bundle). Never
   the query string (a query passphrase is ignored: `400 validation_failed`).
   Without multipart the body is the raw bundle. The passphrase is normalised
   by the library (case, spaces).
9. **Access after restore.** `ImportOptions.Finish` runs in the import
   transaction after the rows are loaded: it picks the exporting admin
   (`manifest.exportedBy`) if still an active human platform admin who is an
   org admin, else the oldest such person, and mints a web session (30 days)
   and a PAT (`restore admin token`: read, write, admin; no expiry; pinned to
   that person's first org where they are admin). With no such person the
   import rolls back: `409 move_blocked`, install in setup state.
10. **Error codes added:** `export_in_progress` (409), `bad_passphrase` (400),
    `bundle_invalid` (400: damaged, cut short, changed, checksum, unknown
    format), `bundle_version_mismatch` (409), `move_blocked` (409: key or
    object store missing, unreadable secret, object already in the bucket or
    missing from it, no admin). The library's `ErrNotEmpty` is `409
    setup_complete`. Texts are fixed: no object key, row value or secret.
11. **Never logged, emitted or stored:** passphrase, ticket, setup link,
    session, token secret. `logPath` replaces the ticket in the access log with
    `{ticket}`; the internal-error log holds only the error chain, which the
    library keeps free of values. No event for either route (install-level);
    the restored events come from the bundle. No idempotency row.
12. **nginx.** `deploy/nginx/default.conf.template` has two locations (the
    restore path and the export prefix): no body size limit on the restore, no
    request or response buffering, one-hour timeouts. Everything else keeps the
    2 MiB limit. The export location has `access_log off` (the access log
    format holds `$uri`, and the one-time ticket is in that path) and
    `error_log ... crit` (error-level lines for a 413, an upstream timeout or
    an aborted client quote the request line; the test provokes a 413 there and
    checks the log). A crit line could still quote it: the ticket is single
    use and lives 5 minutes. The user's OUTER proxy is not ours: its access
    log will hold the ticket path (same limits, same answer). The root
    `docker-compose.yml` (LAUN-246) mounts the same template, so there is one
    config. `deploy/test-move-nginx.sh` (in `make deploy-test`) runs the
    template in a real nginx with a stub app: a 3 GiB upload (with and without
    Content-Length), no disk spool, a 3 GiB download, no ticket in the log, 413
    for 3 MiB on every other path. A user's own outer proxy needs the same
    limits (`docs/self-hosting.md`).
13. **Team access rule.** All three routes are `noneRule`: install-level, no
    tenant row.
