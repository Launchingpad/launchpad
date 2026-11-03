> Internal design note from the Orbit deployment; kept for history.

# T2 decisions (ORB-3406): the MCP server

T2 adds `server/cmd/mcp`, a stdio MCP server that agents start and that calls
the REST API with a bearer token. The code is `server/internal/mcp`. T2 does
not change any REST route. Where the brief left a gap, T2 made the smallest
sensible choice. Each one is listed here so a later tranche can change it on
purpose.

## Protocol

1. **No MCP SDK.** The stdio transport needs JSON-RPC 2.0 over
   newline-delimited stdin / stdout, four methods (`initialize`, `ping`,
   `tools/list`, `tools/call`) and two notifications (`initialized`,
   `cancelled`). That is about 250 lines on the standard library, so the
   module gets no new dependency. Protocol versions `2025-06-18`,
   `2025-03-26` and `2024-11-05` are accepted; any other request gets the
   newest. No JSON-RPC batches (2025-06-18 removed them).
2. **stdout carries only protocol messages.** Logs go to stderr: one line
   per tool call with the tool name, the outcome, the HTTP status of a
   failure and the duration. Arguments and results are never logged.
3. **Up to 8 tool calls run at once**, so calls that are in flight together
   have no order: a client that sends a create and a read without waiting
   for the first answer may read before the create commits (seen in the
   smoke run). Wait for a write's result before a call that depends on
   it, as agent runtimes do. `notifications/cancelled` cancels the
   named call and its HTTP request, and the cancelled call sends no
   response (MCP: the receiver SHOULD NOT answer). A `tools/call` whose id
   is already in flight is refused (`-32600`) and the first call goes on.
   When stdin ends, calls still running finish and answer, because the
   stdio shutdown is "close stdin, wait, then SIGTERM". SIGINT / SIGTERM
   cancel everything at once, even with stdin still open (round 1 could
   hang here when the reader stopped on the signal; measured by
   `TestServer_Serve_SignalWhileReaderStops`, which hangs on the old wait).
   A line over 8 MiB is refused on its own (`-32600`, id null); the
   session goes on.
4. **Errors.** An unknown tool is a JSON-RPC error (`-32602`). Bad
   arguments and API failures are a tool result with `isError: true`. An API
   error reads `tracker API <status> <code>: <message>`, for example
   `tracker API 409 version_conflict: ...`, so an agent can act on the code.

## Configuration

| Variable | Meaning |
|---|---|
| `LAUNCHPAD_URL` | Base URL, with or without `/api/v1`. Required. |
| `LAUNCHPAD_TOKEN` | An `odt_` personal access token. Required. Never logged, never in an error. |
| `LAUNCHPAD_ORG` | Org slug, e.g. `orbit`. Required: no default, so a wrong setup fails at start. |
| `LAUNCHPAD_TEAM` | Default team key, e.g. `ORB`. Optional. Used by `save_issue` (create), `save_project` (create), `list_issue_statuses` and `list_cycles` when no team is given. `list_issues` never defaults it: with no `team` it lists the whole org, as Linear does. |
| `LAUNCHPAD_ALLOW_INSECURE_HTTP` | `1` allows plain http to a non-loopback host (a private network, the compose network). Otherwise plain http is allowed only to `localhost`, `127.0.0.0/8` and `::1`. |

5. **The start-up check** calls `GET /me` once (5 s), logs the user and the
   scopes, and warns when the reviewer profile has a token that can write.
   It never stops the server: each tool call reports its own error.
6. **The token stays with the configured host.** The HTTP client never
   follows a redirect (a 3xx is an error), so the `Authorization` header
   cannot be sent anywhere else. Over plain http (loopback, or a private
   network with `LAUNCHPAD_ALLOW_INSECURE_HTTP=1`) the client uses no
   proxy, so `HTTP_PROXY` never sees the token in clear; over https a proxy
   from the environment is kept, because it only tunnels. The URL may not
   carry credentials, a query or a fragment. A token that does not start with `odt_` or holds
   whitespace is refused at start, and the message does not quote it.
7. **Caller ids are one path segment.** Every id, identifier and slug is
   escaped with `url.PathEscape`, so `../tokens` reaches
   `/issues/..%2Ftokens` (a 404), never another route. `""`, `.` and `..`
   are refused before any request.
8. **Responses are capped at 32 MiB**; HTTP calls time out after 30 s.

## Profiles

9. **`--profile full`** (the default) exposes the 24 tools below.
   **`--profile reviewer`** exposes ONLY `get_issue`, `list_comments` and
   `get_document`. Three layers keep a reviewer from writing:
   - The other tools are not registered in that profile at all, so
     `tools/list` does not show them and `tools/call` answers "unknown
     tool" without any HTTP request.
   - The reviewer's HTTP client is read-only: it refuses every method but
     GET before it builds the request (`ErrReadOnly`). `NewTools` refuses to
     build the reviewer profile on a client that can write.
   - Give reviewers a `read`-scoped token. The server's scope check then
     refuses a write whatever the client does.
   Measured: `TestServer_ReviewerProfile_CannotCallWriteTools` (every
   non-reviewer tool, zero requests at the fake API, and the same calls
   reach it under the full profile), `..._ClientRefusesWrites` (every write
   handler run through the reviewer client), and
   `TestIntegration_ReviewerProfile` (a WRITE-scoped token in the reviewer
   profile, row counts of every writable table unchanged in Postgres).
   **A read token cannot write in the full profile either**: the same
   integration test runs every write tool (each mode of `save_issue`,
   `save_comment`, `save_document`, `save_project`) with a `read` token
   under `--profile full`; each gets `403 insufficient_scope` and no row
   changes. The test fails if a write tool is added without a case.

## Tools

The names and arguments follow Linear's MCP server, so agent briefs keep
working. Every list tool returns `{items, nextCursor}`; `limit` defaults to
50 and a larger value than 250 is cut to 250 (not refused).

| Tool | REST | Notes |
|---|---|---|
| `get_issue` | `GET /issues/{id}` | Always includes attachments. `includeRelations`, `includeComments`, `includeStateHistory` add the others; so do `fields` entries `relations`, `comments`, `stateHistory` (#21). |
| `list_issues` | `GET /issues` | `parentId` maps to `parent`. A cycle id becomes the team + cycle number the API filters on. `team` and `assignee` resolve names (#19, #20). `fields` with `description` returns full descriptions (#21). |
| `save_issue` | `POST /issues`, `PATCH /issues/{id}`, `POST .../relations` | See #10-#13. |
| `list_issue_statuses` | `GET /teams/{team}/states` | |
| `list_teams` | `GET /teams` (and `GET /products`) | `query` matches the key or the name. Each team also has `productSlugs` (its non-archived products' slugs; LAUN-63). |
| `get_guidance` | `GET /agent-guidance` | LAUN-200 (decisions-T31). Optional `team` (default `LAUNCHPAD_TEAM`) is sent as `?team=`. Adds a fixed `note`: the text is org-authored data. Read-only; not in the reviewer profile. |
| `list_products` | `GET /products` | Launchpad's own name: Linear has no tool for this tier. `query` matches the slug or the name; `includeArchived`. Read-only: no `save_product`, because an agent cannot hold the admin scope (decisions-T18 #21). `list_issues` also takes `product`. |
| `list_issue_labels` | `GET /labels` | `name` is a substring. `team` (name, key or id) is sent as `?team=` and narrows to the org labels plus that team's labels, with that team's `issueCount` (LAUN-54). `includeGroups` is accepted and ignored. |
| `list_users` | `GET /users` | `query` matches username, name or email. `team` is accepted and ignored. |
| `list_cycles` | `GET /cycles` | `teamId` as in Linear (a key or an id). `type` `current` / `previous` / `next` returns that one cycle. |
| `list_milestones` | `GET /milestones` | `project` (required) is a name or an id. |
| `save_comment` | `POST .../comments`, `PATCH /comments/{id}` | `issueId` (or its alias `issue`) or `documentId` to create; `id` to edit (body only). |
| `list_comments` | `GET .../comments` | `issueId` or `documentId`. Newest first, by `createdAt` or `orderBy: updatedAt`, as Linear (#26). |
| `get_document` | `GET /documents/{id}` | id or slug. |
| `list_documents` | `GET /documents` | `project` is a name or an id. |
| `save_document` | `POST /documents`, `PATCH /documents/{id}` | `project` is a name or an id; `version` for compare-and-swap. |
| `list_projects` | `GET /projects` | `query`, `state`, `team`, `lead`. |
| `get_project` | `GET /projects/{id}` | `query` is an id or an exact name (ignoring case), as in Linear. |
| `save_project` | `POST /projects`, `PATCH /projects/{id}` | `teams` replaces; `addTeams` appends. A create with no team uses `LAUNCHPAD_TEAM`. |
| `prepare_attachment_upload` | `GET /issues/{issue}`, `POST /attachments/prepare` | Linear's args `issue`, `filename`, `contentType`, `size`, `title`. Returns the REST fields plus `uploadRequest` and `assetUrl` (#29). |
| `create_attachment_from_upload` | `POST /issues/{issue}/attachments` (`kind: file`) | Linear's args `issue`, `assetUrl`, `title` (+ `subtitle`). |

10. **`save_issue` is append-only where Linear is.** `addLabels`,
    `blocks`, `blockedBy`, `relatedTo` and `duplicateOf` only ever add.
    The server reads the issue's relations first and skips any that exist
    (by identifier, ignoring case, or by id), so repeating a call writes
    nothing. `labels` replaces the set; `removeLabels` removes (update
    only). On create, `labels` and `addLabels` are joined into one set.
11. **Names become ids in the MCP server, not in REST.** `project`,
    `milestone` and `cycle` take a name (exact, ignoring case), a number
    (cycles), `active` / `current` (cycles) or an id. A milestone resolves
    in the project given in the same call, else the issue's current
    project; a cycle resolves in the issue's team. Two projects with the
    same name are an error listing both ids.
12. **A partial failure names the saved issue.** The issue write and the
    relation writes are separate requests (the API has no single call for
    both). If the issue was saved but a relation then failed, the error
    starts `issue ORB-12 was saved, but changing relations failed: ...`, so
    the agent does not create the issue again. No write is retried.
13. **No Idempotency-Key.** The server never retries a write, so a key
    would never be reused; an agent that retries a failed call makes a new
    request.
14. **Arguments are strict.** An argument that is not in the tool's schema
    is an error (`unknown argument "delegate"`), the same rule as REST's
    unknown query parameters (decisions-T1 #16), so a Linear-only argument
    fails loudly instead of being ignored. Round 2 measured the arguments
    today's agents really send to Linear (tallies of past sessions) and
    added every one of them that was missing: `fields` (get_issue,
    list_issues; #21), `patch` (save_issue; #22), `removeBlocks` /
    `removeBlockedBy` / `removeRelatedTo` (save_issue; #23), `orderBy`
    (list_comments; #26), `includeGroups` (list_issue_labels, ignored) and
    `issue` (save_comment, alias of `issueId`). `team` on
    `list_users` is accepted and ignored (a harmless superset); on
    `list_issue_labels` it narrows the list (LAUN-54). Tolerated value shapes: a numeric or boolean string (`"5"`,
    `"true"`) for an integer or boolean; for a list of strings, a
    JSON-encoded array in a string (`"[\"Bug\"]"`, seen 9 times) or one
    plain string (`"Bug"`, a one-item list). `null` is allowed only where
    the schema says so, and means "clear" on an update (and is dropped on
    a create).

## REST gaps (reported, not widened)

15. **(Round 1 only.)** `links` and the two attachment tools waited for the
    attachment routes. ORB-3417 merged them, and round 3 built all three
    (#28, #29). Nothing in this server waits for ORB-3417 any more.
16. **Five routes are not paginated by the API**: teams, team states,
    labels, users and comments return every row. The MCP server pages
    them itself with its own offset cursor (`mcpo1:` + offset, base64), so
    the tool shape is the same. An offset cursor can skip or repeat a row
    if the list changes between pages; at a small org's size that is acceptable.
    If one of those lists grows, give the route a keyset cursor in a later
    tranche and pass it through.
17. **No tools beyond the brief's list.** There is no label, milestone,
    cycle or team write tool (Linear has some), because the architecture
    lists none. Milestones and labels are made through REST or the web
    app.

## Hostname

18. **The hostname is not decided** (architecture.md). The `.mcp.json`
    sample in AGENTS.md uses a placeholder. Once the host exists, add the
    real entry with the token taken from the environment, never written
    into a committed file.

## Round 2 (review fixes)

19. **Teams resolve by key, id or NAME in the MCP layer.** Today's briefs
    send Linear's team name (`team: "Orbit"` on 537 sampled `save_issue`
    calls and 110 of 112 `list_issues` calls), and REST resolves a team only
    by id or key. Every team argument (`save_issue.team`, `list_issues.team`,
    `list_issue_statuses.team`, `list_cycles.teamId`, `list_projects.team`,
    `save_project.teams` / `addTeams`, and `LAUNCHPAD_TEAM`) is matched
    against `GET /teams` (key or id ignoring case, then an exact name
    ignoring case) and sent to REST as the key. The list is cached for the
    process and re-read once on a miss. Two teams with one name is an
    error naming both keys. REST is unchanged.
20. **People resolve by username, email or display name.** `assignee` and
    `lead` (a display name such as `"Alice"`) are matched against `GET /users`
    (username or email, then a unique display name, ignoring case). `me`
    and `none` pass through; a value that matches nobody is passed to REST
    unchanged, so its own error reaches the agent.
21. **`fields` is accepted; every field is returned.** Linear's `fields`
    picks output fields (144 of 365 `get_issue` and 100 of 126
    `list_issues` calls send it). This server returns the whole object,
    a superset, so a `fields` call works unchanged. Where a name changes
    what is FETCHED, it is honoured: `relations`, `comments` and
    `stateHistory` on `get_issue` add those includes, and `description` on
    `list_issues` returns full descriptions (`full=true`) instead of the
    400-character cut. Linear-only names (`status`, `statusType`) are
    ignored; the data is under `state`.
22. **`patch` is implemented** for the description (87 sampled calls).
    Ops: `append` / `prepend` `{text}`, `insert_before` / `insert_after`
    `{anchor, text}`, `replace` `{old_string, new_string}` (with
    `replace_all: true`, every occurrence, at least one), and
    `replace_range` `{from, to, new_string}`, which replaces from the start
    of `from` up to (not including) the first `to` after it; `from` must
    occur once. Each was seen in real calls. Ops are applied in
    order. Text is inserted exactly as given (the briefs put their own
    `\n\n` in it). An anchor or `old_string` must occur exactly once in
    the description, or the call fails before any write, so an edit never
    lands in the wrong place. Not with `description`, and not on create.
    The server reads the full description and its `version`, and the
    PATCH sends that version (unless the caller sent one), so a
    concurrent edit is a `409 version_conflict`, never a lost update.
23. **Relations: dedupe, 409 as "present", and removal.** Each relation
    list is deduped (ignoring case) before any request, ids match ignoring
    case, and a `409` from `POST .../relations` that says the relation
    already exists counts as present: `that relation already exists`
    always, and for `related` (which has no direction) also the reverse
    and race messages. So an imported Linear UUID, an upper-case UUID, a
    ref twice in one call, or the symmetric `related` row added from the
    other issue are all no-ops. A reverse `blocks` / `duplicateOf` row
    (B blocks A when asked for A blocks B) is a real conflict and is
    reported. `removeBlocks`, `removeBlockedBy` (seen once) and
    `removeRelatedTo` delete the matching relations; a missing one is a
    no-op.
24. **Golden files pin every tool schema.** `internal/mcp/testdata/
    tools_full.golden.json` and `tools_reviewer.golden.json` hold the exact
    `tools/list` result of each profile (names, descriptions, input
    schemas, annotations). `TestToolsList_Golden` byte-compares them; after
    a deliberate change run `go test ./internal/mcp/ -run Golden -update`
    and review the diff.
25. **CI builds the Mac binary.** The `mcp-darwin` job (stage `build`)
    runs `CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath` on the
    runner's Homebrew Go with `GOTOOLCHAIN` pinned, and keeps
    `server/bin/launchpad-mcp` as an artifact for 30 days. "Static"
    here means no cgo: macOS has no fully static binaries, and the file
    links only libSystem.
    Since LAUN-183 this artifact is a CI-only convenience; the GitHub Release is the supported install (#46).

26. **Comments are newest first, as Linear** (corrected in round 3; round 2
    returned them oldest first). The round-3 reviewers measured Linear live:
    `orderBy: createdAt` returns the newest first, and 208 real calls send
    `orderBy` with a `limit` of 2-20 to read the latest comments. The API
    returns oldest first, so the MCP layer sorts the comments newest first
    by `createdAt` (the default) or `updatedAt`, then pages them. The cursor
    is an offset into that order (#16).

## REST and importer gaps found in round 2 (reported, not widened)

27. **Linear team and cycle UUIDs do not resolve.** Sampled calls pass
    Linear's team UUID (`list_cycles teamId: "1bfad36e-..."`, 12 of 16) and
    Linear cycle UUIDs (`save_issue cycle: "2160b4bf-..."`, about 70 of 93).
    After the import those are not ids in this tracker, and REST resolves
    imported Linear ids for issues only (`import_mappings`). The MCP layer
    reports a clear error: `a Linear team id is not a team id here`, and
    for a cycle UUID (`save_issue cycle`, `list_issues cycle`) it first
    asks `GET /cycles/{id}` and on a 404 answers `A Linear cycle id is not
    a cycle id here: pass the cycle number or active` before any write. It
    cannot map them without the importer's mapping; the real fix is on the
    importer ticket ORB-3419. Fix in T4: either keep
    the Linear UUIDs as the team and cycle ids, or resolve
    `import_mappings` for teams and cycles in REST. Until then the briefs
    should pass the team name and the cycle number.

## Round 3 (review fixes, after ORB-3417)

28. **`save_issue` `links` adds link attachments** (351 of 2761 real
    `save_issue` calls; 262 of 388 "In Review" moves carry the MR link).
    After the issue write and the relations, each `{url, title}` becomes
    `POST /issues/{ref}/attachments {kind: link, url, title}`. Add-only: the
    issue's attachments are read first and a URL already attached as a link
    is skipped, a URL given twice in one call is posted once, and a 409
    counts as present, so a repeat writes nothing. `links` is checked
    (array of `{url, title}`, url required, no other key) before ANY write,
    so a bad item never leaves a half-saved issue; the API checks the URL
    itself (http / https). A link failure after the issue write answers
    `issue ORB-12 was saved, but adding links failed: ...` (#12). Note that
    REST has no link de-duplication of its own (decisions-T3b #15): the
    skip is the MCP layer's.
29. **The attachment tools follow Linear's real shape** (131 and 128 real
    calls). `prepare_attachment_upload {issue, filename, contentType, size,
    title}` checks the issue (`GET /issues/{issue}`, so a wrong id fails
    before any upload), then calls `POST /attachments/prepare
    {filename, contentType, sizeBytes: size}`. Its result is the REST body
    (`uploadKey`, `uploadUrl`, `method`, `headers`, `contentType`,
    `sizeBytes`, `expiresAt`) plus Linear's names that the agent briefs
    use: `uploadRequest {url, method, headers}` and `assetUrl`
    (= `uploadKey`), and `issue` (the identifier). `title` is accepted and
    not used there: give it to the create. **The MCP server never reads a
    local file**, as Linear's tool does not: the agent PUTs the bytes to
    `uploadRequest.url` with every header in `uploadRequest.headers`
    (`curl --data-binary @file`). `create_attachment_from_upload {issue,
    assetUrl, title}` is `POST /issues/{issue}/attachments {kind: file,
    uploadKey: assetUrl, title}`. A Linear `assetUrl`
    (`uploads.linear.app`) gets a hint to prepare on this server first.
    Both are write tools: not in the reviewer profile, refused to a read
    token (403), and in the AC4 loop.
30. **Content types follow the server's allowlist** (decisions-T3b #3):
    png, jpeg, gif, mp4, plain text, pdf. One real call prepared
    `text/markdown`, which REST refuses with 400; the MCP layer does not
    widen it (a REST decision).
31. **`list_issues` `state` takes a state TYPE, as Linear.** When every
    comma-separated part is a state type (`triage`, `backlog`, `unstarted`,
    `started`, `completed`, `canceled`, any case) and no state of the team
    (or of any team, with no team) has that name, it is sent as
    `stateType`. A value that is also a name (`backlog`: Orbit has a
    "Backlog" state) stays a name filter; so does any mix of names and
    types, and so does `state` when the caller also sends `stateType`.
    State names are cached per team for the process.
32. **The read loop never waits for a slot.** Up to 8 calls run; up to 64
    more wait in their own goroutine for a slot. The loop keeps reading, so
    a `ping` or a `notifications/cancelled` is handled at once even with
    every slot busy, and a cancelled call that is still waiting never runs
    and sends no answer. Above 72 calls in flight a call gets `-32000` "too
    many tool calls in flight".
33. **A reply needs its issue.** `save_comment` with only `parentId` (2
    real calls) is refused with "a reply needs issueId (or documentId) as
    well as parentId": the API has no route that reads one comment, so the
    MCP layer cannot find the parent's issue. A REST gap, not widened.

## Not measured

- A live run under Claude Code or another MCP client. The tests drive the server
  over its stdio protocol (in process, and the built binary in a smoke
  run against the compose app), not through an agent runtime.
- The `mcp-darwin` CI job has not run: its YAML passes `glab ci lint`, and
  the same build runs locally in Docker (`make docker-build-mcp`).
- Linear's comment order (#26) was measured by the round-3 reviewers, not
  by this agent.
- The tests PUT a file to the real silo from Go, not with `curl`.

## download_attachment (LAUN-178)

34. **The tool.** `download_attachment {issue, attachment}` returns `{path, contentType, sizeBytes,
    attachmentId}`. `attachment` is an attachment id or the `downloadUrl` from `get_issue` (the id is cut out
    of the path and must be a UUID). It is the 21st tool of the full profile. It only GETs the API, but it
    writes a local file, so it is not `ReadOnly` (annotation `readOnlyHint: false`) and the reviewer-profile
    guard (`NewTools` refuses a non-`ReadOnly` reviewer tool) would catch it if it were listed there.
35. **Not in the reviewer profile.** The reviewer profile stays exactly three tools and its client stays
    GET-only. The tool would work on that client, but a reviewer reads text, and a local write (and a second
    HTTP host) is more than the three-tool promise. `TestDownload_NotInReviewerProfile` and the integration
    test pin it.
36. **Flow.** (a) `GET /issues/{issue}` for the issue id; (b) `GET /attachments/{id}`: the record must belong to
    THAT issue (else 404 "not found on this issue"), be `kind=file`, and its content type must be in the
    upload allowlist (png, jpeg, gif, mp4, text/plain, pdf) and its size within the cap; (c) `GET
    /attachments/{id}/download` with the token, NOT followed: the 302 `Location` is read; (d) the address is
    checked; (e) one GET to it with no `Authorization`. Type and size come from our record, not from the file
    host. Cross-org is the API's 404.
37. **Redirect rules.** The MCP client never auto-follows (it already set `ErrUseLastResponse`); the second
    GET uses a copy of the same transport with the same rule, so a redirect from the file host is an error
    (`file_host_status`), not a hop. The target host (with port) must equal the API host or the host of
    `LAUNCHPAD_FILES_URL`; there is no way to learn the store's public host from the API, so without
    that variable a store on another host is refused (`host_not_allowed`). Scheme: https, or http only to a
    loopback host or with `LAUNCHPAD_ALLOW_INSECURE_HTTP=1` (the same rule as the base URL). Credentials in
    the address are refused.
38. **Size.** Default 25 MiB, `LAUNCHPAD_DOWNLOAD_MAX_BYTES` can lower or raise it, never above the hard
    cap of 100 MiB (the upload limit). Checked on the record, on `Content-Length`, and on the streamed
    copy (it reads at most cap+1 bytes and aborts). The byte count must equal the record's `sizeBytes`.
39. **Type.** The response `Content-Type` (media type only; `text/plain; charset=utf-8` is fine) must equal the
    record's type.
40. **Folder and file.** `LAUNCHPAD_DOWNLOAD_DIR`, else `os.TempDir()/launchpad-mcp`; created 0700 on
    the first call (a failure is a tool error, not a boot error). Refused if it is a symlink, not a
    directory, not owned by the user (unix) or not mode 0700. The model cannot pass a folder. The file is
    `<attachment id><ext>` (`.png .jpg .gif .mp4 .txt .pdf`), written as `.part-<random>` (O_EXCL, 0600) and
    renamed; a symlink or directory at the final name is refused. A part file is removed on every failure.
    Known gap: the folder's parents are not checked (only the last component), and a same-user process can
    race the final name; the folder is 0700 and ours.
41. **Errors and logs.** Refusals are `download refused (<code>): <fixed sentence>`. No token, address, query,
    byte or user file name is ever put in an error or a log: the HTTP client error of the file host is
    replaced (it would hold the presigned URL), and its error body is never read. API errors keep the API's
    own envelope text, as every other tool.
42. **Scope.** A read-scoped token works (all three routes are `read`).
43. **Not measured.** No real S3, CDN or object store with its own redirects; no run under a real MCP client.
    The tests use `httptest` servers and, with `INTEGRATION=1`, the real router and silo.
44. **Scheme.** The presigned address must use the scheme configured for its host (the API URL or
    `LAUNCHPAD_FILES_URL`), so `LAUNCHPAD_ALLOW_INSECURE_HTTP=1` cannot downgrade an https files host.
    A write error while copying is code `write`, not `read`.
45. **Known gaps (LAUN-178).**
    - A `.part-*` file left by a SIGKILL is never swept.
    - If another user pre-creates `/tmp/launchpad-mcp`, the default folder is refused (denial of service
      only; set `LAUNCHPAD_DOWNLOAD_DIR`).
    - `host:port` is compared as written; `LAUNCHPAD_FILES_URL` must write the host and port as the API
      writes them in the presigned address.
    - The production `.mcp.json` needs `LAUNCHPAD_FILES_URL=https://launchpad-files.example.com`
      and, if wanted, `LAUNCHPAD_DOWNLOAD_DIR`. This ticket edits no `.mcp.json`.
    - The tool description says "at most 25 MiB by default"; the real cap follows the env (hard cap 100 MiB).

## Release binary and installer (LAUN-183)

46. **Where the binary lives.** On a GitHub Release of the public repository, not on the production server.
    The MCP runs on the agent's machine, so each machine fetches it. `deploy.sh --update` is unchanged: it
    builds no binary and nginx serves none. (The first design served the files from the web origin; the
    owner chose GitHub Releases instead, so the server has no extra public path, build step or image
    content.)
47. **How it is built.** `.github/workflows/release.yml`, on a pushed tag `v*`: `CGO_ENABLED=0`, `-trimpath`,
    `-ldflags "-X main.version=<tag>"`, for darwin and linux on arm64 and amd64. Asset names are
    `launchpad-mcp-<os>-<arch>`. The release also holds `SHA256SUMS` and `install-mcp.sh`. The job has
    `contents: write` only and uses one third-party action (`actions/checkout`, an exact tag); the release
    is made with the runner's `gh`. No artifact attestation yet (decision: keep the workflow short).
48. **`--version`.** `launchpad-mcp --version` prints `launchpad-mcp <version>` and exits before any
    configuration is read. A binary from before this change has no such flag; the installer then prints
    "unknown".
49. **The installer** (`scripts/install-mcp.sh`, POSIX sh). Source: the latest release, or `--version TAG`,
    or `--url BASE` (a mirror or private server with the same file names). The repository path is one
    variable at the top. It checks the sha256 against `SHA256SUMS` and refuses on a mismatch or a missing
    entry. The new file is written in the target folder, then `mv -f` over the old one: copying over a
    binary that has run can get it killed on macOS with Apple silicon. The old binary is kept as `.prev`
    (a hard link). On macOS it runs `xattr -c`. It prints the version before and after, and a `.mcp.json`
    entry with placeholders. It never asks for or prints a token and never writes `.mcp.json`. Redirects
    are followed but only to https. `--url` takes an https origin (http only to loopback).
50. **Trust.** The sha256 comes from the same place as the binary, so it guards against a bad download, not
    against a hostile release host. The documented way is download, read, run, not `curl | sh`.
51. **Server version.** The binary is a thin client. The recommendation is a server on the same release or
    a newer one. There is no compatibility check and no guarantee.
52. **Not measured.** The real GitHub download and a real workflow run (the repository was not public when
    this was built). The tests use a local `python3 -m http.server` and shell-script stand-ins for binaries.

## Claude Code plugin (LAUN-186)

53. **What it is.** The repository is a Claude Code plugin marketplace: `.claude-plugin/marketplace.json` (name
    `launchpad-mcp`, one plugin `launchpad`, relative `source` `./plugins/launchpad`). Install is `claude plugin
    marketplace add <owner>/<repo>` then `claude plugin install launchpad@launchpad-mcp`. Field names come from the
    current Claude Code docs (code.claude.com/docs/en/plugins-reference, plugin-marketplaces, plugins/loading);
    `claude plugin validate` passes on both the marketplace and the plugin.
54. **Settings.** `userConfig` in `plugin.json`: `url`, `org`, `token` (`sensitive: true`), `team`, `files_url` (the last
    two optional). `.mcp.json` maps them with `${user_config.KEY}` to the `LAUNCHPAD_*` env (the docs allow
    substitution in `env` of stdio servers). Sensitive values go to the system's secure credential store (macOS Keychain storage is NOT yet tested; on Linux it
    was a plain `.credentials.json` in the Claude config folder). An unset optional value reaches the binary as an empty string, which the binary treats as unset. No token is
    in any repo file.
55. **The launcher.** `scripts/launchpad-mcp` (POSIX sh) reads the plugin `version`, uses `${CLAUDE_PLUGIN_DATA}` (the
    docs: kept across updates, deleted on uninstall) with a `~/.local/share` fallback, and keeps the binary in
    `v<version>/`. A cached binary starts with no network and no checks. Otherwise it runs `lib/install-mcp.sh`
    (`--version v<version>`) with stdout discarded: stdout is the MCP protocol, errors stay on stderr. Same hardening as
    LAUN-183 decisions #49: https only, curl only, sha256, new file then `mv -f`. Concurrent first starts both download
    and the last rename wins; both files are verified, so this is harmless.
56. **One installer, two copies.** A copied plugin does not include files outside its folder, so the installer is copied
    to `plugins/launchpad/lib/install-mcp.sh`. A test fails when the copy differs from `scripts/install-mcp.sh`. (A symlink
    out of the plugin is not allowed by the docs. Making the plugin the repository root would copy the whole repository
    into every user's cache.)
57. **Versions.** The plugin sets `version` in `plugin.json` (the docs: it pins users to that version until it changes;
    the marketplace entry has no version). The launcher turns it into the tag `v<version>`, and `release.yml` refuses a
    tag that differs from the plugin version. So a release is: bump the plugin version, merge, tag. Old version folders in
    the data directory are not deleted.
58. **Mirror seam.** `LAUNCHPAD_MCP_RELEASE_URL` replaces the GitHub address (`install-mcp.sh --url` rules). It is how
    the tests and the local trial serve fake files. It changes where files come from, not the sha256 check.
59. **Reviewer server: not included.** A second `launchpad-reviewer` entry would need a second, optional token, and an
    entry with an unset required value fails at start for everyone who did not set it. The manual `.mcp.json` entry in
    `docs/mcp.md` stays the way to get one. The launcher passes `--profile reviewer` through if someone wires it.
60. **Not measured.** The real fetch of the marketplace from GitHub (the repository was not public). Tried with a local
    directory marketplace on macOS (add and install work; the sensitive store failed in a scratch HOME because it has no
    Keychain) and, for the whole chain including the token reaching the binary, in a Linux container with the real
    `claude` CLI (`claude mcp list` showed the plugin server connected, with a fake MCP binary served from a local http
    server). One early health check in the container failed with `CONNECTION_CLOSED` and did not repeat in three
    reruns; its cause is unknown.
61. **Review fixes.** (a) `claude plugin install` never prompts: values are set with `/plugin configure` or `--config
    KEY=VALUE`; until the required ones are set the server does not appear. (b) The launcher moved to
    `plugins/launchpad/scripts/` because files in `bin/` land on the Bash tool's PATH and block claude.ai/Cowork
    installs. (c) Claude Code waits 30 s for an MCP server; a slow first download can time out. A `SessionStart` hook
    (`matcher startup`, `async: true`) runs `launchpad-mcp --prefetch` (exit 0 always, nothing on stdout, which would
    become context); `docs/mcp.md` has the `/mcp` and `MCP_TIMEOUT` advice. The hook was seen firing
    (`SessionStart:startup ... moved to the background`) in a headless `claude -p --debug-file` session; its effect on timing
    in an interactive session is not measured. No lock between the hook and the MCP start (a possible double download is
    harmless: both verify, last rename wins); skipped to keep the launcher simple. (d) The installer and curl run in a subshell with `LAUNCHPAD_TOKEN`
    and every `CLAUDE_PLUGIN_OPTION_*` unset (Claude Code exports all userConfig values to hooks); a mirror is echoed
    on stderr only when it is a plain origin, otherwise "using a mirror (not shown)". (e) An unset optional
    `${user_config.*}` reached the binary as an empty string in the container trial; the launcher also turns a literal
    `${user_config.` value into empty (tested). (f) Trust: `SHA256SUMS` is from the same origin as the binary, so it
    does not defend against a compromised release; a cached binary is not re-hashed. Signing is a later step.
    (g) Tests added for hostile origins and versions refused before any curl call, the http redirect, the fresh inode
    and the version guards; each guard was removed in a scratch copy (both installer copies) and a test failed.
