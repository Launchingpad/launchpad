> Internal design note from the Orbit deployment; kept for history.

# Decisions T18: products, a tier above teams (LAUN-59)

Design only. No code is in this change. The request (2026-10-26) was that an org can
have many products, with the ability to create a product and assign teams to
product(s). Today one org holds a flat list of teams. This note says what a
product is, how it is stored, how it is exposed and how it is rolled out. The
follow-up tickets are listed at the end. Migration number: taken at merge (#25).

What the code looks like today (read for this note):

- `teams` has `(org_id, key)` and `(org_id, id)` unique keys. Every child table
  points at `(org_id, id)` with a composite FK (`issues_team_fk`,
  `cycles_team_fk`, `project_teams_team_fk`).
- `project_teams (org_id, project_id, team_id)` is already a many-to-many
  between projects and teams. Products reuse its shape.
- `router.go` mounts `/teams` and `/projects` under the `read`, `write` and
  `admin` scope groups; creates carry `s.idem`.
- The MCP catalogue has 19 tools, pinned in
  `internal/mcp/testdata/tools_full.golden.json` and `tools_reviewer.golden.json`.
- The sidebar (`web/src/features/shell/Shell.tsx`) has "My issues", "All
  issues", a flat "Teams" list (`TeamNavItem`), then "Views".
- The settings area (LAUN-26) lists its pages in `settingsPages.ts`; Teams is an
  `admin` page.
- The Linear importer reads teams, states, labels, users, projects and
  issues. It reads no initiatives (`internal/importer/linear/export.go`).

## What a product is

1. **A product is a named group of teams. Nothing more in v1.** It has a name,
   a slug, a description, an icon (an emoji or a letter) and a colour. It owns no
   issues, no states, no labels, no cycles, no documents, no views. Reason: the
   question is "I see every team in one flat list". A grouping answers that. Each
   extra owned thing is a new tenancy rule to get right.
2. **Not a project.** A project is scoped work: it has a state, dates, a lead,
   milestones, and it holds issues. A product is long-lived and has no end date
   and no issues of its own. A project may span teams of several products; that
   needs no rule.
3. **Not a Linear initiative.** An initiative groups projects and has a
   roadmap and status updates. Launchpad has no initiatives. A product groups
   teams. If initiatives are wanted later, they are a separate tier over
   projects and do not clash with this design.
4. **Not a Linear team group / sub-team.** Linear sub-teams are a tree: one
   parent per team. A product is many-to-many (#5), and has no tree. Products
   do not nest.
5. **Why not a team field or a label.** A label is per issue. A team field
   would make teams belong to one product only; the request wrote "product(s)".

## Teams and products

6. **Many-to-many.** A team belongs to zero, one or several products. Table
   `product_teams` (#10).
7. **Zero products.** If the org has no products, the sidebar is the flat
   Teams list as today, and nothing else changes. If the org has at least one
   non-archived product, a team that is in no non-archived product shows under a
   last group called "Other teams" (not "Unassigned": it is a place, not a
   task). The group is computed in the client from the two lists; the server
   stores no row for it.
8. **A team in several products** shows under each of them in the sidebar.
   Reason: the sidebar is a view of the assignment, so it shows every
   assignment. A team row stays one team: the same key, the same page. The
   sidebar rules for this are in #22.
9. **Archive, restore, delete: products are the first object with all three.**
   Teams have no archive or delete route and no `version`; projects have a
   state but their `archived_at` has no route either. So this is a new
   pattern and the rules are written out here:
   - **Archive and restore: `PATCH {archived: true|false}` is the only path.**
     No separate route. It sets or clears `archived_at`, bumps `version` and
     `updated_at`, and emits `product.updated` (one event kind for both).
     A PATCH that changes nothing (already archived) writes nothing, as in T15.
   - An archived product leaves the sidebar, the pickers and the Product filter
     chip. Its `product_teams` rows stay. Nothing is deleted.
   - **Delete** (`DELETE`) works only on an archived product (`409 conflict`
     otherwise). It removes the product and its `product_teams` rows
     (`ON DELETE CASCADE`). It never touches a team, an issue or a project.
   - **What an archived product returns:**
     - `GET /products` omits it unless `includeArchived=true`.
     - `GET /products/{p}` returns it with `archivedAt` set, for any `read`
       token. A member can open it by link and sees it read-only (#23); the
       web hides it from the sidebar and the pickers, and only the Products
       settings page (admin) links to it.
     - `Team.productIds` and `GET /teams/{team}/products` list only
       non-archived products. `teamKeys` on a product lists its non-archived
       teams, whether the product is archived or not.
     - `?product=<archived>` on the issue list works (it is a filter, not a
       view): it returns the union as for any product. `?product=<unknown>`
       returns an empty list, as an unknown `?team=` key does today
       (`issue_read.go` matches `t.key`, no error).
     - `PUT` / `DELETE .../products/{p}/teams...` on an archived product is
       `409 conflict`: restore it first.
   - **Archived teams inside a product.** `Team` has no `archivedAt` and
     `ListTeams` filters archived teams out today, so the web never sees one.
     That stays. An archived team is excluded from `teamKeys`, from the teams in
     `GET /products/{p}` and from the sidebar. Its `product_teams` row is NOT
     deleted: archiving a team does not touch the join (teams have no delete
     route; if a team row were ever deleted, the FK cascade removes its join rows
     with it). If the team is unarchived, it is back in its products. `Team` does
     not get `archivedAt` in this design.
   - **`PUT /products/{p}/teams` (replace) with an archived member.** The body
     names visible teams only. The replace changes the set of NON-archived
     teams to match the body and **preserves** the rows of archived teams: it
     never drops one silently. A P1 test: a product with teams A (archived) and
     B, `PUT {teams:[C]}` leaves A, C and removes B. Archived teams DO resolve
     today (`resolveTeam` uses `GetTeamByID` / `GetTeamByKey`, which do not
     filter `archived_at`; only `ListTeams` does). One rule for both forms,
     replace (`PUT .../teams`) and add-one (`PUT .../teams/{team}`): naming an
     archived team is `400 validation_failed` ("team is archived"), checked in
     the product code, not left to the resolver. `DELETE .../teams/{team}`
     of an archived member is allowed (it resolves) and removes the row. Create
     (`POST /products` `teams`) takes the same forms (key or id) and the same
     rule.
   - **PATCH of an archived product.** Only `archived: false` (restore) is
     accepted, with `version` (the stale check still applies); any other field in
     the same body is `409 conflict` (restore first). So name and slug are not edited while archived.
   - **The 200 cap counts archived products too** (the rows are still there).
     The count is taken inside the create's `InTx` under an advisory lock on
     the org (`pg_advisory_xact_lock` on a hash of `org_id`), so two creates
     cannot both pass at 199.
   - **Linear ids do not resolve for products** (no `import_mappings` rows exist
     for them, #15): a product path takes a slug or a canonical id only.
   - **Create with `teams`** is one transaction: the product row, the
     `product_teams` rows, then ONE event, `product.created`, as the last
     statement; it carries the team ids. It does NOT also emit
     `product.teams_changed` (that kind is for a later change of the set).
10. **Migration (one pair of files, number at merge, #25):**
    - `products`: `id uuid DEFAULT uuidv7()`, `org_id`, `slug text`, `name text`,
      `description text DEFAULT ''`, `icon text NULL`, `color text NULL`,
      `version int DEFAULT 1`, `created_at`, `updated_at`, `archived_at`.
      `UNIQUE (org_id, id)` and `UNIQUE (org_id, slug)`. CHECKs: name 1-200
      characters, slug 1-40 characters and
      `^[a-z]([a-z0-9]|-[a-z0-9])*$` (starts with a letter, no double and no
      trailing hyphen) and NOT in the canonical UUID form
      (`slug !~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'`;
      the letter-start rule does not block it: `a1234567-1234-...` matches it), `version >= 1`, colour a `#rrggbb` string when
      set, icon at most 8 characters when set.
    - `product_teams`: `org_id`, `product_id`, `team_id`,
      `created_at`; PK `(product_id, team_id)`; FK `(org_id, product_id)` to
      `products(org_id, id)` ON DELETE CASCADE; FK `(org_id, team_id)` to
      `teams(org_id, id)` ON DELETE CASCADE; index `(org_id, team_id)`.
      Same shape as `project_teams`, plus `created_at`.
    - `org_id` is on both tables and every query filters by it (no RLS, as the
      rest of the schema).
    - The `down` drops both tables. It loses data only for rows made after the
      `up`, which is the nature of a down. `TestMigrateUpDownUp` covers it.
11. **IDs** are `uuidv7()`. No `ORDER BY id` as insertion order.
12. **The slug** is the product's key in URLs and in tool arguments.
    - **Lookup of `{product}`:** only the canonical 36-character hyphenated
      form (the regex in #10, either case) is an id, and then the lookup is by id
      only. Everything else is a slug lookup. This is NOT `uuid.Parse`, which also
      takes 32 hex digits, braces and `urn:uuid:`; the lookup uses the regex, so
      those forms are slugs. The CHECK in #10 forbids exactly the canonical form,
      so no stored slug can be read as an id. (A 32-hex slug is a legal slug and
      is looked up as a slug; the issue filter in #20 uses the same helper.)
    - **Derivation when `slug` is not sent:** from the name: Unicode NFKD, drop
      marks, lower case, every run of characters outside `[a-z0-9]` becomes one
      hyphen, trim hyphens, cut to 40 and trim again. "Launchpad" gives
      `launchpad`; "Café Ünit" gives `cafe-unit`.
    - **A derived slug must start with a letter.** "3D Tools" gives `3d-tools`
      and "2024" gives `2024`: both are `400 validation_failed`, "send a slug".
      The server does not strip digits or add a prefix.
    - **Empty result or no letter start** (a name of only non-Latin letters or symbols, for
      example a name in Japanese): `400 validation_failed`, "send a slug". The
      server does not invent `product-1`.
    - **Collision** (derived or sent): `409 conflict`. No numeric suffix.
      Reason: a silent suffix makes URLs the admin did not choose; one retry
      with a slug is cheap.
    - It is lower case so it never looks like a team key (`LAUN`) in `?team=` or a
      path. An admin can change it with PATCH. A changed slug breaks old
      bookmarks; no redirect in v1.
13. **Order.** Products are ordered by name (case-insensitive, then id) in every
    list, and teams inside a product by key. No manual order in v1 (open point
    B). Reason: the T14 manual order is a real feature with its own rank
    columns; the request asked for neither.
14. **Version.** `version` follows `projects`: PATCH takes an optional
    `version` and a stale one is `409 conflict`. Not measured: whether the web
    needs it for this object; it costs one column and keeps the rule uniform.
15. **Import of Linear.** Nothing in Linear's export maps to a product. The
    importer does not read initiatives and the Linear data has no grouping
    of teams (Linear sub-teams are not exported). The importer is unchanged. An
    admin makes the products by hand after the import. (Not measured: whether
    the workspace has sub-teams. Run the `teams` query with `parent { id }`
    once before the cutover if an auto-grouping is wanted; today it is out
    of scope.)

## API

16. **Routes** (all under `/api/v1/orgs/{org}`, in `router.go` and
    `openapi.yaml`; the drift tests enforce both):

    | Route | Scope | Notes |
    |---|---|---|
    | `GET /products` | read | `?includeArchived=true` adds archived; each item has `teamKeys`; no paging (see below) |
    | `POST /products` | admin, `s.idem` | `{name, slug?, description?, icon?, color?, teams?: [key]}` |
    | `GET /products/{product}` | read | slug or id; includes `teams` (the Team shape) |
    | `PATCH /products/{product}` | admin | name, slug, description, icon, color, `archived`, `version` |
    | `DELETE /products/{product}` | admin | only when archived; `409 conflict` otherwise |
    | `PUT /products/{product}/teams` | admin | body `{teams: [key or id]}` replaces the set; `409` if the product is archived |
    | `PUT /products/{product}/teams/{team}` | admin | add one; replay-safe (204 both times) |
    | `DELETE /products/{product}/teams/{team}` | admin | remove one; 204 if absent |
    | `GET /teams/{team}/products` | read | the non-archived products of a team |

    `idem` goes after the scope check on the create only (the PUTs and DELETEs
    are idempotent by shape).
    **List convention: as `GET /teams`.** `GET /products` returns the
    `httpx.Page` shape, `{items, nextCursor: null}`, always the full list, with
    no paging parameters. It is capped at 200 products per org, archived ones
    included (#9); a create beyond it is `409`. The MCP `list_products` is built
    like `list_teams` (`listFiltered` in `internal/mcp/catalog.go`, about line
    790, and `slicePage` in `internal/mcp/page.go`) and
    returns `{items, nextCursor: null}` too, so REST and MCP agree. Reason: the T3
    lists page because they hold thousands of rows; a product list is a sidebar.
    `includeArchived` is the name `GET /issues` and `GET /projects` already use.
    **`Team.productIds` is always present** (an empty array when none). It lists
    non-archived products only (#9). The web keeps it in the same cached teams
    list and refetches the teams and the products lists on every `product.*`
    event (#19), so an assignment shows at once. A fresh `GET /teams` computes it
    with one extra query over `product_teams`, not per team.
    **A no-op membership change emits nothing:** `PUT` of a team already in the
    product, or `DELETE` of one that is not, writes no row and no event; a
    replace that leaves the set unchanged emits nothing.
17. **Scopes.** `read` reads, `admin` writes (as teams). Not `write`: a
    product shapes the whole sidebar for everyone. Projects use `write`
    because they are work, not structure. An agent has org role `member` and
    can never hold an admin scope (`tracker.CreateToken`, T10), so an agent can
    read products and can never change them. That is the intended v1 limit.
18. **Cross-org** is `404 not_found` for a product path; a body reference to a
    team of another org is `400 validation_failed`. The tests add a cross-org case
    for the product, for `product_teams`, and for `?product=` on the issue list.
19. **Events.** New kinds `product.created`, `product.updated`,
    `product.deleted` and `product.teams_changed` (payload: product id, slug,
    team ids; `entityType` is `product`, which is added to the `Event`
    `entityType` enum in `openapi.yaml`). Archive and restore use `product.updated` (#9). Each is the last statement
    of its transaction, in `database.InTx`. The four kinds are listed in
    `openapi.yaml` where the event kinds are listed, and in `events.go`.
    The web side is in #22 and ticket P3 (`events.ts`). No secret is in a
    payload.
20. **Issue list filter.** `GET /issues` gets `product` (slug or id; UUID
    parsed first, #12). `product` is added to `knownListParams` in
    `issue_read.go` (an unknown parameter is `400` today, so it must be
    listed). The filter is one more clause in the dynamic where-builder there:
    `i.team_id IN (SELECT team_id FROM product_teams WHERE org_id = $1 AND
    product_id = $n)`, with the product resolved by slug or id first. It
    combines with `team` as AND. The PK `(product_id, team_id)` serves this
    subquery; the `(org_id, team_id)` index serves the reverse direction
    (products of a team). The subquery does not filter `teams.archived_at`, so the filter is the union
    of ALL joined teams including archived ones. This is intended: it matches
    `?team=<archived key>`, which also returns that team's issues, and an issue
    in an archived team is still an issue. An unknown or archived product: see #9. Not measured:
    plan and time on the production data size (a few thousand issues, a few
    products). Expect it to be small; a measurement is part of ticket P1.
21. **MCP: read-only in v1.** One new tool, `list_products` (read): `{items, nextCursor: null}`,
    each with its team keys; `includeArchived` as REST. `list_issues` takes
    `product`; `list_teams` gets a `productSlugs` field: the slugs of the team's
    non-archived products (from `Team.productIds`, resolved to slugs by the MCP
    layer; an array, empty when none). `save_product` is not built: an
    agent cannot hold the admin scope (#17), so the tool could never succeed.
    If wanted, it is a later ticket that first needs an admin-capable path for
    agents, which is a policy change and not part of this design. Linear has no
    tool for this tier, so the name is Launchpad's own; record it as a
    difference in `decisions-T2.md`. The reviewer profile gets no new tool.
    The full profile goes from 19 to 20 tools; `tools_full.golden.json`
    changes and is regenerated with `-update` and reviewed;
    `tools_reviewer.golden.json` must not change. "19 tools" is stated in
    `AGENTS.md` (MCP profiles line) and `docs/mcp.md` line 126: both become 20
    in P2.

## Web

22. **Sidebar: collapsible groups, not a switcher.** Each product is a
    group with its teams under it. With no products the sidebar is the flat
    list as today. "Other teams" is the last group (#7). Rules:
    - **Keys:** a row's React key is `product.id + ':' + team.id` (today
      `Shell.tsx` uses `t.id`; a team in two groups would clash).
    - **Active state:** the route knows only the team, so every row of that team
      highlights. Both highlighted rows are intended.
    - **Collapse:** the header is a `button` with `aria-expanded`, toggled by
      Enter and Space. The state is in `localStorage` keyed by **product id**
      (not slug: a slug can change), guarded by try/catch. A group holding the
      active team auto-expands when the route changes to it, without writing the
      stored state. Keyboard navigation skips rows of a collapsed group (they
      are not rendered, not hidden by CSS only).
    - **Cycles sub-link** (`CyclesNavLink`) renders per row, so a team with cycles
      on shows it under each product it is in.
    - **Scale.** The flat list is fine for 3 teams. Today's org has about
      three teams (the unmeasured list says so); an org with 20 teams in 3
      products is the case this rule is for, not a measured one. The 25-row
      number is a guess for where a long sidebar stops being scannable (about one
      laptop screen); it is not used in any rule. The rule is the 12-row one:
      count the team rows that would show with every group open (a team in two
      products counts twice, "Other teams" counts). Above 12, every group that
      does not hold the active team starts collapsed. If the active team is in
      no product group (it is only in "Other teams", or the route is not a team), the groups
      all start collapsed, including "Other teams" unless it holds the active
      team. A person can open any group.
    - **The `...` team menu (LAUN-15)** renders per row for an admin, so a team
      under two products has two menus. Both act on the same team (the Teams
      settings page, as today), so the duplicate is harmless; it is accepted for
      v1 and not de-duplicated.
    - **Honest comparison with Linear.** As far as recalled and NOT verified
      for this note: Linear's sidebar lists only the teams a person has joined,
      with sub-teams nested, and has no product tier. A flat list scales
      there because of that join. Launchpad has no team membership: everyone sees
      every team (#26). So the flat list does not scale here, and groups
      (collapsed by default past the limit) do the job a join does in Linear.
      A top switcher (Slack style) hides the other products' teams and costs a
      click for every change of product; it was not chosen for that reason, and
      was not tried with a person.

23. **Product page** `/:org/product/:slug`: a header (icon, name, description),
    a Teams strip (links to the team pages), and the issue browser
    (`IssueBrowser`, list and board) with the filter `product=slug` fixed. It is
    the union of its teams' issues. Cycles are not shown there (#29). An admin
    sees "Manage teams" which opens the Products settings page. A product with no teams shows
    an empty state ("No teams yet", and for an admin a "Manage teams" button). For
    an archived product the REST read works (#9): any member who opens the link
    sees the page read-only with an "Archived" banner; only the settings link
    to it is admin-only.
24. **Where things live:**
    - **Settings, Products** (`/:org/settings/products`, `admin`, group
      Administration, in `settingsPages.ts`): list, create, edit, archive,
      delete, and the teams of each product as a checklist of all teams. "Assign
      teams" lives here and only here for v1; the team form (`TeamsPage`) does not
      get a product field. (Open point D.)
    - **Filters:** the filter bar gets a Product chip next to Team
      (`filters.ts`: `product` in the URL and the query). Choosing a product
      while a team is set keeps both (AND).
    - **Create-issue dialog:** no product field. The dialog needs a team; a
      product only narrows the team picker. When opened from a product page, it
      shows that product's teams first and preselects the team only if the product
      has one. Reason: an issue belongs to a team, never to a product.
    - **Command palette:** "Go to product ..." entries. A small addition in P3.
    - **Phone layout:** the sidebar is the existing off-canvas drawer. Groups
      collapse, and a header is a 44 px target. The product page's Teams strip
      scrolls sideways; the browser is unchanged. The Settings page lists
      products as stacked rows (no table).

## Permissions, issues, notifications

25. **Migration number.** The next free number is `0010` today. Other tickets
    (for example LAUN-20, LAUN-22, LAUN-31) may also need one, and their state
    is not known to this note. One rule only: take the next free number at merge
    time. A branch that loses the race rebases, renumbers its file pair and runs
    `make schema` again before its MR opens. The products migration is one
    pair of files. It has no dependency on another ticket's migration and
    touches no other table; no number is fixed in this note or in the ticket
    list.
26. **Visibility: everyone sees everything, as today.** A member sees every
    product and every team in the org. A product is not an access boundary.
    Out of scope for now: private teams, private products, per-product roles, a
    member list on a product. If wanted, they come with a team-level access
    rule first, never a product-only one. (Open point E.)
27. **Product issue list and board.** The union of its teams' issues, through
    the `product` filter (#20). Status columns on the board are workflow states,
    which are per team (`workflow_states` has `team_id`), so the same state name
    from two teams is two states. The board groups by state name and type as the
    all-issues board does today; the product page adds no new rule. Not
    measured: how the all-issues board groups states of the same name. Ticket
    P4 checks it and copies it. A drag onto a column whose state name another
    team lacks needs a rule: the drop is refused with a toast for a card whose
    team has no state of that name (the PATCH would fail the same way). P4
    checks how `boardDrag.ts` sends the state today (it moves state and place
    in one update) and keeps that.
28. **Labels and states stay as they are.** Labels are per org (and LAUN-54
    adds team labels). Workflow states are per team. A product has neither. On
    a product page the label chip offers the union of the labels of its teams
    (org labels plus the team labels that exist when LAUN-54 lands).
29. **Cycles stay per team.** A product has no cycle. The product page shows no
    cycle chip; the sidebar's Cycles link stays under each team.
30. **Notifications (LAUN-23, T13).** No product subscription in v1. T13 has
    `team_follows (org_id, user_id, team_id)` for "new issue in a team I
    follow". A product follow would be "follow each team of the product", which
    changes with the assignment and surprises a person when an admin adds a team.
    Out of scope; a "follow all teams of this product" button that writes the
    team rows once is a cheap later step and needs no schema (open point F).
31. **Search, the palette and "My issues"** do not change.

## Rollout and interactions

32. **Rollout.** The migration adds two empty tables. An org with no products
    behaves exactly as today: no product group, no product filter chip
    (hidden while the product list is empty), the settings page shows "No
    products yet". No data change, so rollback is the `down` while the tables are
    empty. The first-run checklist (LAUN-25, `onboarding.ts`) gets an optional
    step "Group your teams into products" that shows only when the org has two
    or more teams and no product, and never blocks "done". LAUN-56 (onboarding)
    may move it; the step is one entry in a list. Docs: `docs/architecture.md`
    (tables, routes, tools), `docs/mcp.md` (tool count and the new tool),
    `docs/deploy.md` needs nothing, `AGENTS.md` (pointer to this note and the
    tool count), `web-decisions.md` (the sidebar choice).
33. **LAUN-22 (avatars).** Products have `icon` (a short text) and `color`
    from the start, so the sidebar needs no image. When LAUN-22 adds image
    avatars for orgs and projects, it adds products in the same pass. Not in P1.
34. **LAUN-26 (settings).** Products is one more entry in `settingsPages.ts`
    and one route in `App.tsx`. The settings search keywords: `product`, `group
    teams`, `assign teams`.
35. **LAUN-31..39 (notifications).** No dependency either way (#30). Events
    `product.*` are not notification kinds.
36. **LAUN-54 (team labels).** No interaction: a product does not own labels
    (#28).
37. **LAUN-56 (server onboarding).** The checklist step in #32 is a web entry
    today (`onboarding.ts`). If LAUN-56 moves onboarding state to the server,
    P6 adapts to it, and P6 depends on LAUN-56 only in that case. Whoever
    merges second adapts.
37a. **Built (LAUN-67, P6).** LAUN-56 had moved onboarding to the server, so the step is server side: id `products`,
    last in the order. `GET /orgs/{org}/onboarding` lists it only when the org has two or more teams (archived ones
    included, as `teams` rows) and no product (archived ones included); otherwise it is absent. `done` is "a product exists",
    so a listed step is never done. `optionalSteps` keeps it out of `remaining`. Skip works through the existing route; the
    `onboarding_skips` CHECK changed in migration 0020 (0019 is LAUN-68's). The web step opens Settings, Products.
38. **Not done, on purpose:** nested products; product-owned documents, views,
    labels, cycles or states; roadmap or status updates; access control; manual
    order; a product key in issue identifiers (an issue stays `LAUN-59`);
    redirect after a slug change; an importer step; a product follow.

## Unmeasured or open (honest list)

- The `product` filter's query plan and time on real data (#20).
- How the all-issues board treats same-name states of different teams (#27).
- Whether the Linear workspace has sub-teams that could seed the grouping (#15).
- The "25 rows" and "12 rows" thresholds (#22) are guesses; 12 is the one used.
- The claims about Linear's sidebar (joined teams only, nesting, no product
  tier) are from memory and not checked against Linear today.
- The golden files have not been regenerated; the tool count (20) is by plan.
- Whether the sidebar auto-expand rule feels right is not tried.
- No usability check: the sidebar choice (#22) is reasoned from three products
  and three teams, not tested with a person.

## Open points (each with a recommended default)

A. **Name: "product", and "Other teams" for the zero group.** Default: yes.
B. **Manual order of products.** Default: no, order by name. Add the T14 rank if
   the alphabet is wrong for you.
C. **A team in two products shows under both in the sidebar.** Default: yes.
D. **Where "assign teams" lives.** Default: only the Products settings page.
   Alternative: also a Products field in the team form.
E. **Visibility.** Default: all members see all products and teams; no private
   products.
F. **A product follow for notifications.** Default: none in v1; a later
   "follow its teams" button.
G. **Delete rule.** Default: archive first, then delete. Alternative: delete
   at once with a confirm.
H. **Slug change.** Default: allowed for an admin, no redirect.

## Ticket list, in dependency order

Each ticket is filed after the two reviewers pass this note. All are in team
LAUN. No ticket waits for another ticket's migration (#25).

**P1. Server: products tables, API, events, openapi** (next free migration
number at merge).
- Scope: migration up and down; sqlc queries; `tracker` service (create,
  list, get, update incl. archive and restore, delete, assign and unassign,
  set); routes #16; `Team.productIds` (always present); `product` in
  `knownListParams` and the where-builder; four event kinds in `events.go` and
  `openapi.yaml`, and `product` in the `Event` `entityType` enum; the cap lock
  (#9); cross-org tests; `make sqlc-check schema-check` clean.
- AC: `TestMigrateUpDownUp` passes; `TestOpenAPIMatchesRoutes` and
  `TestOpenAPISchemasMatchModels` pass; a cross-org test per resource answers
  `404` or `400`; a read token cannot write; PATCH with a stale `version` is
  `409`; delete of an unarchived product is `409`; archive and restore bump
  `version` and emit `product.updated`; the archived-product rules in #9 each
  have a test (`productIds`, `teamKeys`, GET of an archived product, `?product=`
  archived and unknown, PUT on an archived product); a canonical-UUID-form, trailing-
  hyphen and double-hyphen slug is refused by the CHECK; `{product}` lookup
  treats only the canonical 36-character form as an id (a 32-hex string, a braced
  or `urn:uuid:` string is a slug lookup); a derived slug that does not start
  with a letter (`3D Tools`, `2024`) is `400` "send a slug"; `GET /products`
  returns `{items, nextCursor: null}`; PUT replace keeps an archived member (A
  archived, B; `PUT [C]` leaves A and C); archived teams are absent from
  `teamKeys` and `GET /products/{p}`; PATCH of an archived product other than
  `archived:false` is `409`; create-with-teams emits one `product.created`
  and no `product.teams_changed`; two concurrent creates at 199 products give
  one `409`; slug derivation (ASCII, accented,
  empty result, collision) has tests; the 200 cap is tested; deleting a product
  leaves its teams and issues; the `product` filter returns exactly the union
  of ALL joined teams' issues, including a team that is archived (a test with
  one archived team: its issues are in the result); naming an archived team in
  `POST`, `PUT` replace and `PUT` add-one is `400`; `DELETE` of an archived
  member removes it; PATCH `{archived:false, version}` is accepted on an
  archived product; a no-op `PUT`/`DELETE` of a membership and an unchanged
  replace write nothing and emit no event; create accepts keys and ids;
  `schema.d.ts` is regenerated here (`npm run openapi`); the plan of that query is recorded in the MR; unit and
  integration lanes are green.
- Depends on: nothing.

**P2. MCP: `list_products` and the product filter (read-only)**
- Scope: `list_products` (built like `list_teams`: `listFiltered` in
  `internal/mcp/catalog.go`, `slicePage` in `internal/mcp/page.go`), `product`
  on `list_issues`, `productSlugs` on `list_teams`; the difference note in `decisions-T2.md`; golden files;
  "19 tools" becomes 20 in `AGENTS.md` and `docs/mcp.md` (line 126) and the tool
  is described in `docs/mcp.md`.
- AC: `tools_full.golden.json` changes and shows 20 tools; `list_products` returns
  `{items, nextCursor: null}`;
  `tools_reviewer.golden.json` is byte-identical; the integration test runs
  `list_products` and the filter against the real router; both a `read` token
  and a `read,write` agent token succeed (reads only); no admin token is needed
  or used; neither the reviewer profile nor a read token changes a row.
- Depends on: P1.

**P3. Web: sidebar groups and product filter**
- Scope: grouped sidebar per #22, "Other teams", the Product chip in
  `filters.ts` and `FilterBar`, palette entries, `useProducts`; in `web/src/api/events.ts` add `product.created`,
  `product.updated`, `product.deleted` and `product.teams_changed` to
  `EVENT_TYPES` and to the `invalidateForEvent` switch (they refetch
  `keys.products` and the teams list), add `keys.products` in `keys.ts`, and
  add the missing `team.updated` to both while there (it is absent today);
  tests in `events.test.ts`; pins in `styles/layout.test.ts` (44 px header
  target) and `styles/contrast.test.ts` (group header and count colours).
- AC: `events.test.ts` shows each of the four kinds invalidates the product
  and team lists, and `team.updated` is covered; no products gives the same DOM
  as today (a test); one product and two
  teams shows the group and "Other teams"; a team in two products renders two
  rows with distinct keys and both highlight when active; a collapsed group
  holding the active team auto-expands; the header is a button with
  `aria-expanded` and works with Enter and Space; rows of a collapsed group are
  not reachable by keyboard; collapse state is in `localStorage` by product id
  and the page works when storage throws; the Cycles link shows under each row
  of a team with cycles on; with more than 12 team rows, groups without the
  active team start collapsed; the chip filters the list and the URL round-trips;
  the drawer works at phone width with 44 px targets; contrast tests pass.
- Depends on: P1.

**P4. Web: product page**
- Scope: `/:org/product/:slug` with header, Teams strip, list and board from
  `IssueBrowser`, archived banner, empty state for a product with no teams, and
  the create-issue dialog opened from it: `Shell.tsx` has one `useMatch` for
  `/:org/team/:team/*` (line 37) and needs a second for `/:org/product/:slug/*`
  that opens the dialog with that product's teams first (#24).
- AC: the list equals the union of its teams' issues, archived teams
  included (the same set as the `product` filter); a board column whose
  state name exists for only some teams shows only those cards, and a drag of a
  card whose team lacks that state is refused with a toast and sends no
  PATCH; the label chip offers the union of the teams' labels; an archived
  product shows the banner and is read-only; a product with no teams shows the empty state; a bad slug is the not-found
  page; the `c` shortcut and the `+` button open the dialog from the product
  page;
  the phone layout has no horizontal page scroll.
- Depends on: P3.

**P5. Web: Settings, Products page**
- Scope: `settingsPages.ts` entry, route, list (with an "include archived"
  toggle), create and edit form (name, slug, description, icon, colour),
  archive, restore, delete with confirm, the team checklist.
- AC: admin only (a member gets "You need admin access"); a taken slug shows
  the `409` message; a name with no Latin letters asks for a slug; assigning
  and unassigning is saved and the page's own list refreshes; delete is offered
  only when archived; the settings search finds it by "product" and "assign
  teams" (a test in `settingsArea.test.tsx`, with the page tests there and the
  search keyword test); editing name or slug is disabled while archived.
- AC that needs P3: assigning or unassigning updates the sidebar through the
  events. It is checked in P3's tests (with a mocked event), not here.
- Depends on: P1. The page can run in parallel with P3 and P4; only the
  sidebar-refresh behaviour belongs to P3.

**P6. Rollout: checklist step, docs**
- Scope: the optional step (two or more teams, no product) in `onboarding.ts`,
  or where LAUN-56 puts it; `docs/architecture.md`; `docs/mcp.md`;
  `web-decisions.md`; `AGENTS.md`.
- AC: the step shows only under that condition and never blocks "done"; docs
  list the tables, routes and the tool; `TestOpenAPI*` still green.
- Depends on: P3 and P5; adapts to LAUN-56 if it moved onboarding to the
  server (#37).

**P7. Avatars for products (a comment on LAUN-22, not a new ticket)**
- Ask LAUN-22 to include products in its pass (#33). Depends on P1.

## LAUN-100: teams next to a product name

Where the web app shows a product name, it shows the product's teams too. No server change: `Product.teamKeys`
(non-archived teams, key order) is already in the list and detail answers; the MCP `list_products` already returns it.
Short form (`ui/teamKeys.ts`): up to 3 keys, then `+N` ("ALP, BET, GAM +2"); the Settings row allows 6; the filter chip 2.
The full list is the tooltip and, for picker and palette rows, the accessible name ("Alpha: teams ALP, BET"). Places:
Product filter chip and picker, the command palette "Go to product", the Settings, Products row and the sidebar group header
(tooltip; its teams are listed under it). The product page already lists its teams. A long hint cuts with an ellipsis
(`.optionHint`), so it cannot widen a phone.
