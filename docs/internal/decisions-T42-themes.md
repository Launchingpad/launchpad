# Decisions T42: themes, Linear, Paper and Terminal (LAUN-235)

The project owner (2026-11-02), after the Penpot mock-ups of LAUN-234: "the Paper theme is
nice and the Terminal one was good too. Let's add those as options along with
what we have now." The design source is the Penpot file of LAUN-234, page
"Themes", boards "Paper / ..." and "Terminal / ..." (token sheets, issue list,
issue detail, board, settings). The decisions below are the builder's; the
judgment calls say so. Change any of them with a new record.

## The setting

1. **One theme per person, install-wide.** Table `user_preferences`
   (migration 0044): `user_id` primary key with `ON DELETE CASCADE` to
   `users`, `theme` (`linear`, `paper`, `terminal`, a CHECK) and
   `updated_at`. No row means `linear`. The view preferences of
   decisions-T23 are per org and place; a theme is about the person, so it
   follows them to every org and device (judgment call). The table has no
   `org_id`: it is not a tenant table, like the login sessions, and its
   queries carry `-- scope: none`.
2. **Routes:** `GET /api/v1/me/preferences` (read scope) and `PUT
   /api/v1/me/preferences` (write scope, body `{"theme": ...}`, required).
   Any other value is `400 validation_failed`; a body of the wrong shape is
   the decoder's `400 bad_request`. The PUT answers the stored value; the
   last write wins (a theme is an explicit click, so the client-time rule of
   T23 #5 is not needed). `s.idem` is mounted after the scope check, as for
   the view PUT. Team rule: `noneRule` (no org row).
3. **Agents have no theme.** Both routes answer `403 forbidden` for an agent
   token, as every own-preference route does, with their own text "display
   preferences are not available to agent users" (`refuseAgentsWith`; the
   other routes keep the notifications text). The web app
   keeps the look the browser remembered when the GET fails.
4. **No event.** A user-private row (T23 #4, T13 #32).
5. **Cross-org:** there is no org in the path. A token pinned to another org
   of the same person reads the same value; another person never reads or
   changes it (`TestPreferences_CrossOrg`).

## The web app

6. **Two attributes on `<html>`.** `data-theme` stays the light or dark
   variant (`light`, `dark`, none = follow the OS), as before. The new
   `data-look` is the theme: `paper` or `terminal`; Linear sets NO attribute.
   So the Linear CSS is byte for byte the old cascade, and a Linear page
   cannot change (#18). The name `data-look` because `data-theme` was taken
   (judgment call).
7. **Tokens.** `styles/themes.css` holds, per theme, a type and shape block
   (`--font`, `--mono`, `--font-heading`, `--radius`, `--body-size`, no
   shadows) and three colour blocks: light, dark from the OS, dark chosen.
   Each colour block redefines EVERY colour token of `tokens.css`, so no
   Linear value leaks in (a test checks it). New tokens: `--border-strong`
   (the control border, 3:1), `--success`, `--knob` (the switch knob; Linear
   `#ffffff`), `--state-*` (backlog, todo, progress, review, testing, done,
   canceled) and `--font-heading` (Linear: `var(--font)`). The Penpot colour
   values are used as they are; the hover tones, tints (`danger-bg`,
   `warn-bg`) and the card fills are derived by the builder and pass the
   contrast test. `--warn-text` is the sheet's warning colour (Paper light
   `#875400`, 5.29:1 on its tint; Terminal light `#7a5200`, 5.62:1). Three
   changes from the sheet, because the sheet checked bg and raised only and a
   state also sits on the hover tones, the cards and the selected row (review,
   LAUN-235): Paper light backlog and canceled `#82796c` (sheet `#8a8174`,
   2.95:1 on the selected row and the hover tone); Paper dark backlog and
   canceled `#8a8072` (sheet `#857b6d`, 2.95:1 on `--card-surface-hover`;
   now 3.16:1 at worst); Terminal light backlog and canceled `#5c5e55` (sheet
   `#5f6158`, a TAG is text and was 4.42:1 on `--page-hover`; now 4.63:1).
   The contrast test takes the backgrounds from the theme's own tokens
   (every `bg*`, `*surface*`, `*-hover` and `selected`), so a new background
   token is checked without a test change.
8. **The variant combines with the existing choice.** Light, Dark and Match
   the system work for every theme. Under "system", Terminal follows the OS
   like the others, although its sheet calls dark its default (judgment
   call: one rule for all themes).
9. **Theme rules that no token can carry** are in `styles/themeRules.css`
   (body size, Paper headings in Fraunces 600, the Paper focus ring 3px
   outside, Terminal square corners on everything) and in the CSS modules as
   `:global(:root[data-look='x']) .class` (flat shell; Paper sidebar on the
   page tone with Fraunces italic section labels; rows of 46px (Paper) and
   28px (Terminal); Paper serif group heads over a strong rule; the drag
   shadows become rules; the switch). Hard-coded `#fff` on the accent fill
   became `var(--on-accent)` (Linear value `#ffffff`, no change).
10. **State glyphs.** `StatusIcon` reads the look (`useLook`). Linear: the old
    SVG in the state's own colour, unchanged. Paper: a 14px ring in the
    theme's state colour, dashed for backlog and canceled (canceled also has
    an x, so the two differ by more than colour), a dot for the started
    states, a check for done. Terminal: a bracket tag `[BKLG]`, `[TODO]`,
    `[WIP ]`, `[REVW]`, `[TEST]`, `[DONE]`, `[CNCL]`, `[NEW ]` (triage). The
    theme colours a state by KIND; a `started` state named like "review" is
    review, like "test", "QA" or "verify" is testing, any other is progress
    (judgment call: the database has no such kind). The glyph and the tag are
    `aria-hidden` in every theme; the state name is in an sr-only span or
    next to it, so the accessible text does not change.
11. **Terminal flourishes done:** bracket tags; `[x]` / `[ ]` switches (the
    element stays the `role="switch"` button with `aria-checked`); buttons
    read `[ label ]` (not icon-only or ghost buttons); `## teams` section
    heads (lower case by CSS); an amber `>` on the current sidebar page and on
    the focused list row; `-- In Progress (3) ------` list group heads (the
    tag hidden there). Every mark is CSS generated content with empty
    alternative text (`content: '>' / ''`), so a screen reader reads only the
    real name. **Not done** (each needs a larger change than the ticket
    allows): the column header row of the list (PRI ID STATE TITLE ...),
    `#label` text instead of label pills, `@handle` instead of avatars, the
    `~/acme/launchpad/issues $` prompt title, `acme@launchpad:~` in the
    sidebar head, `[online]` in the footer, `> ` inside text fields, `v`
    select arrows, lower-case text everywhere, and the inverse-video tab.
    Paper's design is fully carried by tokens and the overrides above, except
    the outlined label pills and the `LAUNCHPAD` eyebrow over the page title.
12. **The picker.** Settings > Appearance (Account group, every member): a
    `radiogroup` of three native radios (arrow keys move and apply), each with
    a live mini-preview drawn with the theme's own tokens (`data-preview`
    selectors in `tokens.css` and `themes.css`, in the variant the page
    shows), and a "Light or dark" group (System, Light, Dark). A choice
    applies at once (`applyLook`), is saved with an optimistic update, and on
    a failed save the previous theme comes back with an error line and a
    toast. Quick choices (review): the writes run one after another
    (TanStack `scope: { id: 'my-preferences' }`), so the server ends on the
    last choice; each choice has a number, and only the answer to the LATEST
    one changes what is shown. An older write's success only records the
    confirmed value; an older write's failure is dropped. When the latest
    write fails, the theme goes back to the last value the server accepted.
    The command palette offers "Theme: Linear / Paper / Terminal" through the
    same code (`useChooseTheme`); its variant entries and the user menu say
    "mode" ("Light mode", "Dark mode", "Match system mode", "Mode: system"),
    because "theme" now means the look. A variant that storage cannot keep
    (blocked, or full) shows for the page. `useTheme()` now reads localStorage through
    `useSyncExternalStore`, so the menu, the palette and the page show the
    same variant.
13. **No flash.** `public/theme-init.js` (already loaded as a file before the
    app, because the CSP is `script-src 'self'` with no inline script) also
    reads `localStorage['launchpad-look']` and sets `data-look` before the
    first paint. `ThemeSync` (in the shell) reads the server value once per
    load (`staleTime: Infinity`) and applies and mirrors it, so a theme chosen
    on another device shows after one switch at the next load. The mirror is
    per browser. **Sign-out** (and revoking the session in use) removes the
    copy and the attribute, so the login page and the next load are Linear
    (the project owner, 2026-11-02). When another tab signs in as someone else, this tab
    reads the theme again. The light or dark variant stays: it was a choice
    of the browser before this ticket and still is. **Other tabs:** a
    `storage` event for `launchpad-look` or `devops-theme` makes every other
    open tab of the browser follow at once.

## Fonts

14. **Self-hosted, OFL.** Fraunces 600 and 600 italic, Source Sans 3 400 and
    600, IBM Plex Mono 400, JetBrains Mono 400 and 700; latin and latin-ext
    woff2 subsets from the fontsource npm packages (`npm pack`, sha256 of the
    tarball: `@fontsource/fraunces` 5.3.0 `c4d77f01...c1f6ce`,
    `@fontsource/source-sans-3` 5.3.1 `b598201d...f37881`,
    `@fontsource/ibm-plex-mono` 5.3.0 `60d3c0cf...d4c029`,
    `@fontsource/jetbrains-mono` 5.3.0 `1bbea47d...0b4a`; full values in the
    commit message), 14 files, 284 KB in all. The fontsource Source Sans 3
    licence names "Google Inc."; the file here has Adobe's copyright line, as
    upstream. They live in `web/src/assets/fonts` (Vite hashes
    them), with the four OFL texts. `font-display: swap`; `unicode-range`
    per subset. No third-party request; the CSP `font-src 'self'` holds.
15. **Loaded only when used.** A browser fetches a `@font-face` file only when
    rendered text uses that family and range. Linear names none of them, so a
    Linear page loads no font file (the harness shows none). The Appearance
    page loads the fonts of the two previews (accepted: they show the fonts).

## Tests and measurements

16. **Server:** `internal/api/preferences_integration_test.go` (default,
    store, repeat, own row only, no event, validation and the CHECK, agents
    and scopes, cross-org), the OpenAPI schema checks, the team rules.
17. **Web:** `styles/themeContrast.test.ts` computes WCAG ratios from the
    token values for every theme and variant: text pairs 4.5:1, UI parts
    (focus ring, control border, accent fill) 3:1, state glyphs 3:1 (Paper)
    and state tags 4.5:1 (Terminal, they are text) on every row background;
    every Linear colour token is defined; the two dark blocks are equal; the
    Penpot anchors do not drift. `features/settings/appearance.test.tsx`: the
    page, the PUT, the revert, the variant, ThemeSync, the StatusIcon per
    theme, `theme-init.js`.
18. **Linear is pixel-identical (real browser).** The built app of
    origin/develop and of this branch, API stubbed, Brave headless at 1440,
    1280, 375 and 320 px, light and dark: the list, the board and the issue
    page are 0 px different. The settings page (Sessions) differs only in the
    sidebar box (x 27-146, y 323-883, about 10 500 px): the new Appearance
    item and the items below it, one row lower. One flake: the assignee
    avatar in the issue page's Properties at 1440 px sits about half a pixel
    higher or lower from run to run (163 px in a 22x23 box); two runs of the
    SAME origin/develop build differ there too, and other runs of the pair
    are 0 px.
19. **Layout in the new themes (real browser).** The same harness, the
    LAUN-233 and LAUN-230 checks (every part of a row or card inside it,
    labels never over the assignee or the date, the title never over the
    labels, the board column width, no sideways scroll except the board,
    every part of the issue page inside its column) plus the overflow audit
    of `web/scripts/lib/audit.mjs`, on list, board, issue page and settings,
    six variants, four widths. On this branch alone (origin/develop has no
    LAUN-233 yet) Paper and Terminal fail exactly where Linear fails (the long
    labels of the LAUN-233 data), except two more Terminal list rows at 1440
    and 1280 (an RTL row and a long-labels row: the wider monospace labels
    push the avatar and date out of the row; the same unbounded `auto` labels
    column that LAUN-233 fixes). With the LAUN-233 branch merged on top (a
    scratch merge; the one conflict is the row's `grid-template-columns` line,
    resolved as `... var(--row-state, 18px) minmax(120px, 1fr)
    minmax(0, max-content) 28px var(--row-date, 40px)`), every rule passes in
    Linear, Paper and Terminal at every width (0 failures). At 375 and 320 px
    the audit lists label pills clipped by the row's labels box, by design
    (Linear 45 and 46, Paper 37 and 46, Terminal 47 and 55: a monospace pill
    is wider). The Appearance page has no finding in any variant. Font files:
    none on a Linear page; Paper loads Source Sans 3 400/600, Fraunces 600
    and 600 italic, IBM Plex Mono 400; Terminal JetBrains Mono 400/700; no
    latin-ext file for English text; no request leaves the origin.
20. **Not tested:** Safari and Firefox (the CSS `content: 'x' / ''`
    alternative text needs Safari 17.4 or Firefox 128; older ones drop the
    second declaration and keep the first, so the mark is read aloud);
    a real screen reader; Windows high-contrast mode in the new themes; the
    pages beyond the four (inbox, projects, cycles, dialogs, the command
    palette) were seen only in passing; two real devices for the sync.

## Questions answered by the project owner (2026-11-02)

21. Terminal under "Match the system" follows the OS, as built.
22. The Terminal and Paper flourishes of #11 that are not done get a follow-up
    ticket.
23. Sign-out clears the browser's theme copy, so the login page shows Linear
    (#13).

## The visible name (LAUN-245)

24. **"Linear" is shown as "Default"** (the project owner, 2026-11-02). Settings >
    Appearance (the radio label, so its accessible name), the save-failed
    text and the command palette ("Theme: Default") say Default. The value
    stays `linear`: in `user_preferences.theme`, the API (`UiTheme`), the
    OpenAPI file, `[data-preview='linear']` and the code. It still sets no
    `data-look`. No migration. In this record and in the code comments,
    "Linear" means that theme.
