> Internal design note from the Orbit deployment; kept for history.

# T24 decisions (LAUN-22): avatars for the org and for projects

An org and a project can have one small image, a logo. This note lists every
judgment call. The routes and wire shapes are in `server/api/openapi.yaml`.
The attachment rules of `decisions-T3b.md` still hold: file bytes reach the
object store (silo) from the client, never through the app on the way in.

## Serving (the project owner, 2026-10-29)

1. **The app streams the image.** `GET /orgs/{org}/avatar` and
   `GET /orgs/{org}/projects/{project}/avatar` return the bytes. The app reads
   the small object from silo with a SigV4 GET it signs for itself (the same
   signer as HEAD and DELETE, `storage.S3.Get`). A 302 to a presigned URL is
   not used: a short-lived URL cannot sit in a long-cached `<img>`. This is the
   one place the app proxies file bytes out; the limit is 2 MiB, so the cost is
   small. Attachments keep their 302.
2. **Login is required.** Read scope, org member (cookie or token), like every
   org route. There is no public avatar URL. A cross-org request is
   `404 not_found`. No avatar set is `404 not_found`.
3. **Cache.** `ETag: "av<version>"` and `If-None-Match` give `304` before any
   call to silo. `avatarUrl` ends in `?v=<version>`. When the `v` of the request
   equals the stored version the answer is `Cache-Control: private,
   max-age=31536000, immutable`; any other `v` (or none) gets `private,
   no-cache` (the browser revalidates). A new image bumps the version, so the
   new URL busts the cache.
4. **Response headers.** `Content-Type` is the stored type (checked at
   confirm, never sniffed), `X-Content-Type-Options: nosniff`,
   `Content-Security-Policy: default-src 'none'; sandbox`,
   `Content-Disposition: inline`, `Referrer-Policy: no-referrer`,
   `Content-Length` from the object.

## Upload

5. **Presigned flow with a confirm step.** Three calls, like attachments:
   `POST .../avatar/prepare {contentType, sizeBytes}` returns a presigned PUT
   (15 minutes, type and length signed) and an `uploadKey`; the client PUTs the
   bytes to silo; `PUT .../avatar {uploadKey}` confirms. Prepare is never
   idempotent (it holds a credential): `idempotency.Reject`, `Cache-Control:
   no-store`. Nothing is stored at prepare.
6. **Object keys.** Org: `org/{org_id}/avatars/org/{uuid}`. Project:
   `org/{org_id}/avatars/project/{project_id}/{uuid}`. A fresh uuid per upload,
   so a replace never overwrites an object a client may still be reading. Confirm
   accepts only a key of exactly this shape for exactly this owner; a key of
   another org or another owner is `400 validation_failed`.
7. **Confirm checks the object.** It HEADs it (size 1 byte to 2 MiB, type on
   the allowlist; the stored type is the one the object carries, which the
   signed PUT fixed) and reads it (at most 2 MiB) to
   check the magic bytes against the type (PNG, JPEG, GIF, WebP). A bad object
   is deleted and the answer is `400 validation_failed`. A missing object is
   `400` ("upload the file first").
8. **Allowlist:** `image/png`, `image/jpeg`, `image/gif`, `image/webp`.
   **SVG is refused on purpose.** The avatar is served from the app's own
   origin, an SVG there can run script, and the CSP above is a second line of
   defence, not the first. Size: 1 byte to 2 MiB (2,097,152). A wrong type or
   size is `400 validation_failed`.
9. **Store off** (`S3_*` unset): prepare, confirm and the GET of a stored
   avatar answer `503 attachments_unavailable`. A clear works without the
   store (it deletes nothing then). A GET with no avatar is `404` first.

## Who and what

10. **Org avatar:** org admin (admin scope, as `PATCH /orgs/{org}`). **Project
    avatar:** write scope, the same as `PATCH /projects/{project}`. Projects
    have no per-project role: any org member with a write token edits.
11. **Set, replace, clear.** `PUT` sets or replaces; `DELETE` clears (`200` with
    the org or project, like the `PUT`; clearing when none is set is also `200`, no event). Both run in one
    transaction under the row lock. The old object is deleted AFTER the commit,
    best effort: a failed delete logs the owner kind and the org id (never the
    key) and leaves an orphan object. Attachments cannot be deleted yet, but the
    store has `Delete`, so the avatar does clean up.
12. **Confirming the same key twice** is `200` with the same result, no second
    event (the stored key already equals it).
13. **Schema (migration 0023).** `orgs` and `projects` gain `avatar_key`,
    `avatar_content_type` (both null or both set, a CHECK) and `avatar_version
    integer NOT NULL DEFAULT 0` (CHECK >= 0). Every set and every clear adds 1,
    so an ETag is never reused. Project queries filter on `org_id`. A project
    avatar change sets `updated_at` and does NOT bump the project `version`
    (it is the compare-and-swap of the content fields, and an image never
    conflicts with them).
14. **Events.** A set, replace or clear emits `org.updated` (changes
    `["avatar"]`) or `project.updated` (changes `["avatar"]`) as the LAST
    statement of the transaction. The payload holds no key, no URL.
15. **JSON.** `Org` (GET `/orgs/{org}`, `/me` org list, PATCH) and `Project`
    gain `avatarUrl`: a string or `null`. It is a path `/api/v1/orgs/{slug}/avatar?v=N`
    (absolute on `PUBLIC_BASE_URL` when set, as `downloadUrl`).
16. **Never log** the upload URL, the upload key or the object key.
17. **MCP is unchanged.** `get_project` and `list_projects` pass the REST body
    through, so they carry `avatarUrl` only if the golden shapes list it; they
    are left alone. The importer is not touched.
