> Internal design note from the Orbit deployment; kept for history.

# T4b decisions (ORB-3419): the importer stores Linear's files; Linear team and cycle ids resolve

T4b finishes the importer's file step (the seam T4 left for ORB-3417),
makes the Linear ids of imported teams and cycles work in the REST API, and
closes three T3b follow-ups. Where the brief left a gap, T4b made the
smallest sensible choice; each one is listed here so a later tranche can
change it on purpose. No migration: 0001 already has every column.

## Files

1. **One file, one object, one `file` row.** Every downloaded
   `uploads.linear.app` file is put in the attachment store once, gets one
   `kind = file` attachment row, and every text that refers to it
   (issue descriptions, comments, documents, project description and
   content, milestone descriptions) is rewritten to that row's download URL
   `{PUBLIC_BASE_URL}/api/v1/orgs/{org}/attachments/{id}/download`
   (decisions-T3b #12). Texts never hold a raw silo URL.
2. **Which row holds a file.** In this order:
   - an imported Linear attachment whose URL is the file: that attachment
     IS the file row (kind `file`, its own id). A T4 import made it a
     `link` row; the next run turns it into a `file` row (an update);
   - else the first imported issue (by `createdAt`) whose description
     refers to it;
   - else the first imported comment on an issue that does.
   Measured on a large export: of 376 files, 123 are Linear attachments,
   250 are referred to by a description and 3 by a comment only. None is
   referred to by more than one issue; when that happens, the other issues
   simply link to the same download URL.
3. **A file only a document (or a comment on one), a project or a
   milestone refers to** has no
   issue for its row (`attachments.issue_id` is NOT NULL). It is excluded
   (`only a document (or a comment on one), a project or a milestone refers
   to it`), its URL stays as
   it is, and verify's `linear_upload_url` row then fails, so the import
   exits 3 and commits nothing. The measured export has **none**, so no schema
   change was made for it. If a later export has one, the fix is a
   migration that lets an attachment belong to a document or a project;
   it is not worth a migration for zero rows.
4. **Stored type.** Linear's type when it is on the upload allowlist
   (decisions-T3b #3). Any `text/*` (Orbit has `text/csv` twice and
   `text/markdown` once) is stored as `text/plain`, which is always served
   as a download, never rendered, so it is safe for CSV, markdown or HTML
   text. The filename extension decides (`.png`, `.jpg`, `.mp4`, `.pdf`,
   `.txt`, ...) ONLY when Linear gave no real type (empty or
   `application/octet-stream`): a declared type off the allowlist, such as
   `image/svg+xml` named `x.png`, is refused, never relabelled. A failed
   download, a refused type or a size outside 1 byte to 100 MiB is excluded
   with its reason. **Such an excluded file's URL stays in every text that
   refers to it, so verify's `linear_upload_url` row fails and the import
   exits 3 without committing**: an excluded file is never silently left
   on Linear. The measured export has none. The display filename is the export's
   safe name; one that fails the attachment filename rules becomes `file`
   plus its extension when that is `.` and 1-10 ASCII letters or digits,
   else plain `file` (no control character can reach it).
5. **Keys and ids.** The key is `org/{org_id}/{yyyy}/{id}/{safe name}`,
   built by the tracker's own `AttachmentObjectKey` (decisions-T3b #7), with
   `yyyy` the owner's creation year. The id is the row's mapping when it has
   one, else a UUIDv7 at the owner's `createdAt` whose random bits are
   `sha256("devops-import:" + org + entity + Linear source)`. So the key is
   the same on every run, including after a run that rolled back, so a
   re-run of the same export finds every object and uploads nothing. This
   limits orphans; it does not rule them out. A run that rolls back leaves
   its objects in the store until a re-run uses them, and if a file's owner
   or filename changes before any run has COMMITTED its mapping, the next
   key differs and the earlier object stays unused (as an abandoned upload
   does, decisions-T3b #9). Once a mapping is committed the row keeps its
   id and its object key whatever changes (#5a).
5a. **An owner change keeps the row.** When a Linear attachment appears for
   a URL that only a text referred to in an earlier import, the attachment
   takes over the existing row: its id is the file URL's mapping, and the
   object key stored on that row is kept (even if the owner's year
   differs). So no second row and no second upload. Tested by
   `TestRun_anOwnerChangeKeepsTheFileRow`.
   One rare case is NOT covered: a Linear attachment A was imported as a
   T4 `link` row Y; a later export no longer has A (the URL is owned by a
   text, so a new `file` row X is made under `file:U`); then A comes back.
   A's own mapping (to Y) wins, so Y becomes the file row and X stays as a
   second, unmapped `file` row for the same URL. It needs a link import, a
   file import and an attachment that disappears and returns between
   exports; it is left as it is.
   Tested: `TestRun_aFileOnlyAProjectRefersToFailsVerify` uploads, fails
   verify and rolls back, then re-runs on a corrected export and gets 0
   uploads, 2 present and the same keys; replacing `seededV7` with a random
   id makes it fail (measured).
6. **Uploads run before the transaction**, not inside it: no row lock is
   held across a network call (decisions-T3b #6, supabase
   `lock-short-transactions`). Each file is HEADed; one already there with
   the same size is not sent. The transaction then re-reads the mappings
   and refuses to go on if a file's mapping changed in between (another
   importer ran).
7. **A dry run uploads nothing**: it HEADs and reports `would upload N`;
   the rewrite and the rows still happen inside the rolled-back
   transaction, so its counts and its `linear_upload_url` check are real.
8. **The server-side PUT** (`storage.S3.Put`) signs `content-type` and
   `content-length`, like the presigned client PUT, so the store refuses a
   body of another size. It has its own HTTP client with **no fixed total
   timeout**: a client `Timeout` covers sending the whole body, and a file
   near 100 MiB over a normal uplink to a remote store would never finish
   inside 30 s. Instead each Put has a deadline from its size: 2 minutes
   plus the body at 128 KiB/s (`storage.PutSlack`, `PutMinRate`; 100 MiB
   gets 15.3 minutes), so a store that stops reading mid-body fails the
   upload instead of hanging it (tested with a stalled server). The
   transport also times out the dial, the TLS handshake and a wait of over
   a minute for the response headers. HEAD, DELETE and the bucket calls
   keep 30 s. The uploads step writes a progress line to stderr every 25
   files and at the end, and one before each file of 10 MiB or more:
   counts and sizes only, never a URL, a key or a file name.
9. **`--skip-files`** keeps the T4 behaviour (no store needed, nothing
   uploaded, URLs left), **but only for an org that has no imported files
   yet.** Once an import has stored files, a `--skip-files` run would write
   the `uploads.linear.app` URLs back over the download URLs, and its
   verify would not look at files, so both `import --skip-files` and
   `verify --skip-files` refuse with a clear error (exit 1) when the org has
   any `file` mapping; `import` checks before it writes anything and again
   inside its transaction, and `verify` is the last backstop. Without the
   flag, `import` needs `S3_ENDPOINT`, `S3_BUCKET`, `S3_ACCESS_KEY`,
   `S3_SECRET_KEY` (and `S3_REGION` optionally), the server's own
   variables.
10. **`PUBLIC_BASE_URL` is baked into the texts** (`--public-base-url`,
    default the environment). The production hostname is not decided yet
    (architecture). Import with the real origin; if it changes later, a
    re-run rewrites the texts (they are updates), because the importer
    always starts from Linear's text.

## Verification

11. Three new rows:
    - `file`: planned files whose URL mapping points at a `file` row;
    - `file_object`: those rows whose object the store holds with the
      row's size (HEAD per file; not in a dry run, which uploads nothing);
    - `linear_upload_url`: IMPORTED texts and attachment URLs (rows an
      import mapping points at) that still contain `uploads.linear.app`
      (expected 0). Rows made in dev-ops are not counted, so a person who
      pastes an old Linear upload link into a new comment after the cutover
      does not make a later verify exit 3. Editing an IMPORTED issue,
      comment, document, project or milestone to hold an old Linear upload
      URL does make a later verify exit 3 (it is still an imported row).
      **One definition of an upload URL** (`importer/markdown.go`) is
      used by the export, the plan, the rewrite and this check: http or
      https, any case in the scheme and host, at least one character of
      path, ending at whitespace, a markdown or HTML delimiter or a
      backquote. Files are keyed by the canonical form (https, lower-case
      host), so `HTTP://UPLOADS.LINEAR.APP/...` in a text is rewritten
      like the https form. Prose that only names the host
      (`uploads.linear.app/...`, `https://uploads.linear.app/<org>/`) is
      not a URL and does not fail verify; the SQL only pre-filters
      candidates. When the row is not 0, verify names the first 20 rows
      (`issue <id>`, ids only).
    Any difference exits 3, as before. `--skip-files` drops all three.

## Linear team and cycle ids in the API

12. **Where.** Every place the API takes a team or a cycle by id also takes
    the Linear id of an imported one, through `import_mappings`
    (`entity_type` `team` / `cycle`, the importer's): the `{team}` and
    `{cycle}` paths, the `team` filter of issues, cycles and projects, the
    issue list's `cycle` filter, `team` and `cycleId` in issue bodies, and
    project `teams`. Issue ids already resolved this way (T1/T4).
12a. **A mapping whose row was deleted names nothing**: a Linear cycle id
    mapped to a cycle that no longer exists is a 404 on a path and a 400 in
    a body or the `cycle` filter, exactly as an unknown id (tested). A team
    filter with such an id gives an empty page, as an unknown team key does.
13. **The issue `cycle` filter now also takes a cycle id** (dev-ops or
    Linear), as agents pass one (`list_issues cycle`). A number and
    `active` work as before.
14. **Org scoping.** The lookup is by org, so another org's Linear id is
    treated like another org's dev-ops id (decisions-T1 #25): 404 on a
    path, 400 in a body or in the `cycle` filter, an empty page in a `team`
    filter (as an unknown team key gives).
15. **A dev-ops id always wins.** The mapping is consulted only when no row
    of the org has that id.
16. **The MCP server** (not merged at the time) is not touched; once this is
    merged its Linear-id calls reach REST and resolve.
17. **A bug found on the way:** `GET /projects?team=<uuid>` was a 500 (an
    unused query argument Postgres could not type). Fixed, and covered by
    the new test.

## T3b follow-ups

18. **Default ports are dropped from the S3 endpoints**: `https://host:443`
    is signed as `host`, which is what net/http sends.
19. **No signed URL in an error.** The `http.NewRequestWithContext` error
    (and the transport error, as before) is reported without its URL.
20. **A non-member gets 404 on `POST .../attachments/prepare`**, now tested.

## Notes from the review round

21. `resolveTeam` looks the mapping up by the parsed id's canonical form,
    so an upper-case or braced (`{...}`) Linear UUID resolves too (tested),
    as `resolveCycleID` and the filters already did.

## Not done

- Files that only a document (or a comment on one), a project or a
  milestone refers to (#3).
- Objects of Linear files whose rows are later deleted are not removed
  (as for any upload, decisions-T3b #9).
