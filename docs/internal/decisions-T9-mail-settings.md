> Internal design note from the Orbit deployment; kept for history.

# T9 decisions (ORB-3435, part D of ORB-3431): platform mail settings

A platform admin can now choose how login email is sent, in the app:
the environment (as before), none, Postmark or SMTP. The settings also
hold the sign-up mode. This file lists each judgment call, so a later
ticket can change it on purpose. The routes and wire shapes are in
`server/api/openapi.yaml` (tag `platform`); the runbook is `docs/deploy.md`
"Mail settings". The web page is built (ORB-3448); its rules are in
`docs/internal/web-decisions.md` "Platform settings (ORB-3448)".

## The table

1. **One row for the install.** Migration 0006 makes `platform_settings`
   with `id boolean PRIMARY KEY DEFAULT true CHECK (id)`, so a second row
   cannot exist, and inserts the row. A read always finds it. The table is
   not per org: one instance is one org (decisions-T8 #1), and mail is an
   install concern.
2. **Columns.** `mail_provider` (`env` default, `none`, `postmark`,
   `smtp`); `postmark_token_enc`, `postmark_from`, `postmark_stream`;
   `smtp_host`, `smtp_port` (default 587), `smtp_security` (`starttls`
   default, `tls`, `none`), `smtp_username`, `smtp_password_enc`,
   `smtp_from_address`, `smtp_from_name`; `key_check`; `signup_mode`
   (`invite` default, `approval`, `closed`); `updated_at`, `updated_by`
   (`ON DELETE SET NULL`). Non-secret text is `NOT NULL DEFAULT ''`. Enums,
   the port range and the blob and key-check lengths are CHECKs; formats
   are checked in Go. `updated_by` has no index: the table has one row.
3. **The down drops the table.** The stored settings are lost; a server
   without 0006 sends mail from the environment only, as before.
4. **`signup_mode` is stored and validated here** (the project owner, 2026-10-25:
   default `invite`; no instant open sign-up; no `org_creation_open`).
   Since ORB-3436 sign-up requests and invites follow it: see
   `docs/internal/decisions-T12-signups-invites.md` #1.

## Secrets

5. **AES-256-GCM, key from HKDF.** `SETTINGS_ENCRYPTION_KEY` is 32 random
   bytes. HKDF-SHA256 derives the sealing key (info `launchpad
   platform_settings secret sealing v1`) and, separately, the key check
   (info `... key check v1`). Each seal has a random 96-bit nonce
   (`cipher.NewGCMWithRandomNonce`); a blob is nonce, ciphertext and tag.
   The additional data names the column, so a token blob copied into the
   password column does not open. All standard library.
6. **The key is 64 hex characters**, not base64: `deploy.sh` already makes
   every secret with `rand_hex`, and `openssl rand -hex 32` makes one by
   hand. Upper or lower case. The value is never echoed in an error.
7. **The dev key is refused outside local/test.** `server/docker-compose.yml`
   has a public default (hex of `launchpad-local-dev-settings-key`). The
   server refuses it, and a key of all zeros (the `.env.example`
   placeholder), unless `LOG_ENV` is `local` or `test`. A malformed key
   fails the boot: that is a configuration error, like a bad `PORT`.
8. **The key is optional.** An install that has no key (an old CapRover
   app) starts. It can store every setting except a secret: a `PUT` that
   sets one answers `409 conflict` and names the variable. The boot log
   warns.
9. **`key_check`** is the HKDF check value of the key the stored secrets
   use (32 bytes, compared in constant time). It is NULL while no secret is
   stored. When the row holds a secret and the check does not match (the
   key changed, or none is set), the secrets are unreadable. A blob that
   fails GCM authentication under the right key (it was changed) counts the
   same way.
10. **Unreadable secrets never crash the boot.** The server logs one ERROR
    line (the reason, never a secret). A provider that needs one of the
    unreadable secrets (`postmark`, or `smtp` with a username) falls back to
    the environment's mail, and the line says so. A provider that needs
    none (`none`, `env`, `smtp` without a username) is used as it is, and
    the line says that instead: `none` stays `none`, so a lost key never
    turns mail on. `GET` says `secretsUnreadable: true`,
    and `postmarkTokenSet` / `smtpPasswordSet` are false. The recovery is to
    enter the secrets again.
11. **Write-only on the API.** `GET` shows `postmarkTokenSet` and
    `smtpPasswordSet` only ("stored and readable"). In a `PUT`, a secret
    that is absent or `null` keeps the stored one, `""` clears it, and a
    value replaces it. A stored secret this server cannot open is dropped
    by any `PUT`, even when "kept": it is useless, and the row's key check
    moves to the current key. If the chosen provider needs it, the `PUT` is
    a `400` that asks for it again.
12. **Never in a log, an event, an idempotency row or an error.** The
    `Secret` type prints `[redacted]` through `fmt` (every verb), `slog` and
    `encoding/json`; only a mailer and the sealing code call `Reveal`. The
    `PUT` and the mail test use `idempotency.Reject`, so their bodies are
    never hashed or stored. No event is emitted: the change feed is per org,
    and this is not an org change. Validation messages name the field,
    never the value. `TestPlatformSettings_NoSecretsInLogsEventsOrIdempotency`
    plants a token and a password, runs every route (a failed send and
    refused requests too), and greps the log, the events, the idempotency
    rows, the responses and the row.

## The API

13. **Routes.** `GET /api/v1/platform/settings`, `PUT
    /api/v1/platform/settings`, `POST /api/v1/platform/mail/test`, inside
    `Authenticate`, behind `auth.RequirePlatformAdmin` (a platform admin's
    session, or their token with the admin scope and no org pin;
    decisions-T8 #15). Outside `/orgs/{org}`. A cookie write needs the CSRF
    header as everywhere.
14. **`PUT` is the whole settings.** Every non-secret field is required
    (`""` clears a text). `smtpPort` 0 stores the default 587, for every
    provider; the range is 0 or 1-65535. Validation
    runs before the transaction (formats, enums, limits: `smtpHost` a host
    name or IP with no scheme or port, `smtpFromAddress` a plain address,
    `postmarkFrom` an address or `Name <address>`, no control character in
    any field, secrets at most 512 bytes) and inside it (what the chosen
    provider needs, with the stored secrets). One `400 validation_failed`
    names every bad field. Unknown fields are `400 bad_request`; a body
    that is not JSON is `415`.
15. **The row lock.** `PUT` reads the row `FOR UPDATE`, so a kept secret is
    the one in the row it writes, even with two writers.
16. **The mail test** sends one plain email to the caller's own address
    (`400` when the caller has none) through the transport login uses now,
    after a refresh from the database. Success is `200 {ok, to,
    activeMail}`. No transport is `503 login_unavailable`, also when a PUT
    to `none` empties the switch between the check and the send (the send
    answers `login.ErrNoMailer`, which maps to the same 503, never a 502). A failed send is
    `502 mail_send_failed` (a new code in `httpx.Codes` and the
    `ErrorCode` enum), with the mailer's safe text. It is limited to 6 a
    minute (burst 3) per process; only platform admins reach it.
17. **`activeMail`** (`transport`: `postmark`, `smtp`, `dev-log`, `none`;
    `source`: `settings` or `environment`) is what this server sends with
    now, so the page can show a fallback. **`loginAvailable`** says whether
    login can issue codes on this server now (the login service's
    `Enabled`: a transport AND `AUTH_OTP_PEPPER`), so the page can say
    when login is off even though mail works.

## The switchable mailer

18. **`login.MailSwitch`** is the login service's mailer: an
    `atomic.Pointer` to the current transport. A send that has picked a
    transport finishes on it; later sends use the new one. The login
    service gets `Options.Available = MailSwitch.Ready`, so `Enabled()`
    (and with it the `503` of `/auth/otp` and `/auth/otp/verify`) follows
    the provider. The mail queue now always runs.
19. **Provider `env` is the old behaviour.** `cmd/server` builds the
    environment's transport exactly as before (Postmark with
    `POSTMARK_SERVER_TOKEN`, else the dev log on local/test, else none),
    and `env` uses it. The `CanIssue` / `CanVerify` split and the login
    code are not here (ORB-3434 is parked).
20. **`AUTH_OTP_PEPPER` is required at every boot** (review of dff6f56).
    Before ORB-3435 it was required only when the environment had a mail
    transport. Now a platform admin can turn login on at run time, and
    without a pepper `login.New` keeps login off: the mail test would pass,
    `GET` would say `activeMail` smtp, and `/auth/otp` would still answer
    503. So `config.Load` refuses a boot without one (at least 32 bytes),
    whatever `LOG_ENV` and `POSTMARK_*` say. `deploy.sh` always generates
    it and the dev compose has a public default; an install that has none
    (an old CapRover app) must add one before it upgrades
    (`openssl rand -hex 48`).
21. **When the switch changes.** At boot (if the read fails, the
    environment's transport, and the log says so); at once after a `PUT`,
    on the server that took it; and on every server from a refresh every 60
    seconds (`platform.RefreshInterval`), so other replicas follow. A
    refresh applies a row only if its `updated_at` is newer than the one in
    force, so a refresh that read the row before a `PUT` committed cannot
    undo it. **`updated_at` is monotonic in commit order** (review of
    dff6f56): the UPDATE sets it to `GREATEST(clock_timestamp(), updated_at
    + 1 microsecond)` under the row lock. `now()` is the transaction START,
    so a PUT that started first and committed last got the older stamp,
    and a server that had applied the other PUT skipped it for good.
    `TestPlatformSettings_CommitOrderWins` commits in that order. The refresh goroutine is owned by `cmd/server` and stops with
    the other workers.

## SMTP

22. **Standard library only** (`net/smtp`). `starttls`: connect, then
    STARTTLS, and the send fails if the server does not offer it (never a
    silent plain session). `tls`: TLS from the first byte. `none`: no TLS.
    TLS 1.2 or newer, the certificate checked against `smtpHost` and the
    system roots; there is no "skip verify" option.
23. **PLAIN auth, never in clear to a remote host.** Auth runs only with a
    username. The password goes only over TLS or to `localhost`,
    `127.0.0.1` or `::1`: the same rule as `net/smtp`'s `PlainAuth`. The
    mailer checks it before it dials, and `PUT` refuses `none` with a
    username for any other host.
24. **Timeouts.** One deadline (20 seconds) covers the whole session; a
    cancelled context closes the connection.
25. **Header injection.** Every header value has its control characters
    removed; the From name and the subject are RFC 2047 encoded when not
    ASCII; the sender and the recipient must be plain addresses with no
    CR or LF (`net/smtp` also refuses CR/LF in `MAIL` and `RCPT`). The body
    is `text/plain; charset=UTF-8`, quoted-printable, CRLF line ends: the
    login code text of the Postmark path.
26. **Safe errors.** Every mailer error is a `login.MailError`. Its text
    names the step and the server's numeric reply code (`refused the login
    (reply code 535)`), never the server's reply text (a server may echo
    the username, the password or anything else), the password or the
    message. TLS errors say which kind (untrusted, wrong host, invalid).
    The Postmark mailer says the HTTP status and Postmark's numeric error
    code, as before. The mail test returns that text and logs it.
27. **No DKIM, no OAuth.** Use a relay or provider that signs for the
    domain, or Postmark.
28. **Any host and port.** A platform admin can point SMTP (and the mail
    test) at any host:port the server can reach, internal ones too. That
    is SSRF-shaped; it is accepted for this role, which runs the install
    and can already change its environment. The errors give only the step
    and a reply code, so the test does not read back another service's
    answers.

## Deploy

29. `deploy.sh` generates `SETTINGS_ENCRYPTION_KEY` (`rand_hex 32`) on the
    first run, and appends a new one on a re-run whose `deploy/.env` lacks
    it (an install from before ORB-3435), like `AUTH_OTP_PEPPER`. It is not
    in the "never generate again" list (`POSTGRES_PASSWORD` and the silo
    root): a missing key cannot be told apart from an install that never
    had one, and a new key costs only re-entering the mail secrets. It
    checks the format and never prints the value. `deploy/docker-compose.yml`
    passes it with `:?`; `.env.example` has an all-zero placeholder.
    `make deploy-test` cases 5b and 5c cover the append and a malformed key.

## Tests

30. Unit: `internal/platform` (the crypto round trip, a wrong key, tamper
    and column binding, `Secret` redaction, the validation matrix, apply
    and every fallback), `internal/login` (the SMTP mailer against the
    in-process fake `internal/login/smtptest`: STARTTLS, implicit TLS,
    none, PLAIN auth, refused auth with a planted secret in the server's
    reply, a refused recipient, no STARTTLS, a wrong certificate host, a
    timeout, plain auth to a remote host; header injection; the switch
    under load with `-race`), `internal/config` (the key formats).
    DB-backed: `internal/database` (0006 up, CHECKs, down, up) and
    `internal/api/platform_integration_test.go` (the auth matrix, defaults,
    the `PUT` matrix, keep and clear, no key, the provider switching login,
    the mail test, its rate limit, no secret anywhere, a changed key, a
    second server following, `PUT`s during login and test traffic, the
    commit-order case of #21, port 0, and the 503 of #16).

## Not done

31. The web "Platform" page was here. It is built now (ORB-3448). The
    sign-up behaviour is built too (ORB-3436, decisions-T12). Not
    done: the login code (ORB-3434), DKIM, OAuth, and a key rotation
    command (rotating means: set the new key, restart, enter the secrets
    again).
