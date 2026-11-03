> Internal design note from the Orbit deployment; kept for history.

# T3b decisions (ORB-3417): attachments

T3b adds file and link attachments on issues. The flow is the one in
`docs/architecture.md` ("Attachments (silo)"). Where the brief left a gap,
T3b made the smallest sensible choice. Each one is listed here so a later
tranche can change it on purpose. The routes and wire shapes are in
`server/api/openapi.yaml`.

## Routes

| Route | Scope | Notes |
|---|---|---|
| `POST /orgs/{org}/attachments/prepare` | write | Presigned PUT. Never idempotent (the response holds a credential). |
| `PUT <uploadUrl>` | (store) | The client sends the bytes straight to silo. |
| `POST /orgs/{org}/issues/{issue}/attachments` | write | `kind: file` (from an upload) or `kind: link`. Idempotency-Key works. |
| `GET /orgs/{org}/issues/{issue}/attachments` | read | Oldest first. |
| `GET /orgs/{org}/attachments/{attachment}` | read | |
| `GET /orgs/{org}/attachments/{attachment}/download` | read | 302 to a presigned GET. |

## Schema

1. **No migration.** 0001 already has `attachments` with every column the
   flow needs: `kind`, `object_key` (unique), `filename`, `content_type`,
   `size_bytes`, `url`, `creator_id`, the composite issue foreign key and
   the shape CHECKs. Only queries were added (`db/queries/attachments.sql`).
   `ListIssueAttachments` moved there from `issues.sql` and now joins the
   creator.
2. **No "pending upload" table.** Prepare stores nothing. The object key
   carries everything create needs, and create checks it again:
   - the key must be `org/{this org's id}/{yyyy}/{uuid v7}/{safe name}`.
     A key of another org is `400 validation_failed` (a body reference to
     another org, decisions-T1);
   - the attachment id IS the uuid in the key, so the primary key and the
     unique `object_key` index make one upload attach once. **LAUN-106:**
     the second attach of that key to the SAME issue is idempotent: `200`
     with the existing attachment (the first one was `201`), no second row
     and no second `attachment.created` event, so a client retry is safe. A replay ignores any
     title or subtitle of the second request and returns the original row unchanged, and it runs
     before the storage HEAD.
     The same key on ANOTHER issue stays `409 conflict` (an upload is one
     file, attached once). Two parallel attaches leave one row: the loser
     of the unique index reads the winner's row and answers `200`. A
     key of another org is `400`, an issue of another org `404`.
     **No migration:** the unique `attachments_object_key_key` index and the
     primary key (id = the key's uuid) have existed since 0001, so no
     issue can hold two rows for one key, in production too.

   Cost of this choice: any write-scoped member of the org who learns an
   unattached key could attach that upload to an issue of the same org.
   The key holds a random UUIDv7 and is only ever returned to the caller
   who prepared it, so this needs a leaked key, and it never crosses an
   org. A table would also give a sweep of abandoned uploads (#9); that is
   the trigger to add one.

## Upload rules

3. **Allowlist:** `image/png`, `image/jpeg`, `image/gif`, `video/mp4`,
   `text/plain`, `application/pdf` (architecture). Parameters are dropped:
   `text/plain; charset=utf-8` is prepared as `text/plain`. SVG and HTML are
   refused on purpose (both can carry script).
4. **Size:** 1 byte to 100 MiB (104,857,600 bytes) inclusive. Zero bytes
   is refused. A wrong type or size is `400 validation_failed`, not `413`
   or `415`: those codes describe the request body, and here the body is a
   small JSON object.
5. **The store enforces the size and the type.** The presigned PUT signs
   `content-type` AND `content-length`, so silo answers 403 to any other
   size or type. Measured against the pinned silo in
   `TestS3_RoundTrip` and `TestAttachments_PrepareValidation`. A browser
   sets `Content-Length` itself, so `headers` in the prepare response lists
   only `Content-Type`.
6. **Create checks the object again.** It HEADs the object before the
   transaction (no row lock across a network call), and refuses an object
   whose type is not allowed or whose size is out of range. Such an object
   cannot pass the signed PUT; the check is defence against a store written
   some other way. The object is deleted, and the answer is 400 ("it was
   deleted"). If that delete fails, the answer is 500 and the failure is
   logged, so an oversized object is never left behind without a log line.
7. **Object key:** `org/{org_id}/{yyyy}/{attachment_id}/{filename}`. `yyyy`
   is the server clock's UTC year at prepare. The filename segment keeps
   `[A-Za-z0-9._-]`; every other character becomes `_`, leading dots are
   removed, and the segment is cut to 100 bytes with the extension kept
   (empty becomes `file`). So no key holds `/`, `..`, spaces or Unicode.
   The display name (`filename` on the row) keeps the original text: 1-255
   characters, no control characters, no `/` or `\`.
8. **Presign lifetimes:** PUT 15 minutes (architecture), the GET behind a
   download 5 minutes, and the server's own HEAD / DELETE 1 minute.
9. **Abandoned uploads are not swept.** A prepared and uploaded object that
   is never attached stays in the bucket. Follow-up: a bucket lifecycle
   rule, or a pending-uploads table plus a sweeper (#2).

## Downloads

10. **302 to a presigned GET.** The 302 has `Cache-Control: private,
    no-store` and `Referrer-Policy: no-referrer`, because its `Location` is
    a credential for 5 minutes. The route works with the session cookie,
    so `<img src=downloadUrl>` works in the web app.
11. **Content-Disposition:** `inline` for images, video and PDF;
    `attachment` for plain text, which is also served as
    `text/plain; charset=utf-8`. A browser therefore never renders an
    uploaded text file as a page. The header has an ASCII `filename` (quote,
    backslash and non-ASCII replaced) and an RFC 5987 `filename*`.
12. **`downloadUrl`** on every file attachment is
    `{PUBLIC_BASE_URL}/api/v1/orgs/{org}/attachments/{id}/download`, or the
    bare path when `PUBLIC_BASE_URL` is empty. Markdown must use it, never a
    raw silo URL (architecture). The T4 importer rewrites
    `uploads.linear.app` URLs to it.
13. **A link has no file:** its download is `400 validation_failed`, not a
    redirect to the link. That keeps the route from being an open redirect.

## Links

14. **`kind: link`** takes an absolute `http` / `https` URL of at most 2048
    bytes, with no user info and no control characters. The title defaults
    to the URL (cut to 255 characters). Links need no store: they work on a
    server with attachments off.
15. **No de-duplication.** The same URL can be attached twice to one issue.
    The GitLab integration (T5) keys MR links on `mr_links`, so it can
    check there before it writes a link attachment.

## Events

16. **`attachment.created`**, entity type `attachment`, payload
    `{id, issueId, kind}`, emitted as the last statement of the insert
    transaction. A refused create emits nothing.

## Configuration

17. **`S3_*` is all or nothing.** `S3_ENDPOINT`, `S3_BUCKET`,
    `S3_ACCESS_KEY` and `S3_SECRET_KEY` are all set, or none. A partial set
    fails the boot. None set: the server starts, logs that attachments are
    off, and the file routes answer `503 attachments_unavailable` (a new
    error code). Link attachments and reads still work.
18. **Two endpoints.** `S3_ENDPOINT` is what the server dials;
    `S3_PUBLIC_ENDPOINT` (default: the same) goes into the URLs handed to
    clients. Local compose needs both (`http://silo:9000` inside, and
    `http://localhost:9100` for the host). In CapRover the server can dial
    silo on the internal network while clients get the public https host.
19. **Outside `LOG_ENV` local/test:** the client-facing endpoint must be
    `https` (a presigned URL is a credential), and the compose file's
    public dev secret is refused. Both fail the boot.
20. **Bucket:** a local/test server creates it at boot (and fails the boot
    if it cannot). Any other server only checks it; a failed check is
    logged as an error, not fatal, so a storage fault does not take the
    tracker down. Production buckets are made by the operator.
21. **Region** defaults to `us-east-1`. Requests are path-style
    (`/bucket/key`), which silo needs.

## Implementation

22. **No AWS SDK.** `internal/storage` signs with SigV4 query
    authentication on the standard library (HMAC-SHA256). Every request,
    including the server's own HEAD and DELETE, is a presigned URL, so there
    is one signing path. It is pinned by AWS's published presigned-URL
    example (`TestPresign_AWSExample`) and exercised against the pinned silo
    in the integration lane. This avoids a large dependency tree for four
    calls.
23. **Integration tests use the real silo.** `internal/storage/storagetest`
    is gated by `INTEGRATION=1` like `testdb`, and under it a missing silo
    FAILS the test. CI starts silo next to postgres (`DEV_OPS_S3_PORT`
    59100). Tests share the bucket `devops-test`; keys are unique per test
    because each test database has a new org id. Objects are left in that
    bucket; `down -v` removes them.

## Not done, left for later

- No delete or edit of an attachment.
- No sweep of abandoned uploads (#9).
- No link de-duplication (#15).
- No document attachments: `attachments.issue_id` is NOT NULL in 0001.
- The MCP tools `prepare_attachment_upload` and
  `create_attachment_from_upload` are T2; the web upload screen is a web
  ticket.
