# T41 decisions (LAUN-232): an emoji as the project icon

A project can carry one emoji as its icon, next to the logo of
`decisions-T24-avatars.md`. This note lists the judgment calls. The wire shapes
are in `server/api/openapi.yaml`.

## Model

1. **One nullable column.** Migration 0043 adds `projects.emoji text`, NULL
   when none is set. A CHECK refuses an empty string and more than 32 bytes
   (`octet_length`). The down drops the column (the emoji of every project is
   lost).
2. **Validation on the server** (`tracker.normalizeProjectEmoji`). Surrounding
   white space is dropped; an empty value (or `null` on PATCH) means none.
   Anything else must be valid UTF-8, at most 32 bytes, hold no control
   character (`unicode.IsControl`) and be exactly ONE emoji sequence (UTS #51):
   - a flag: exactly two regional indicators;
   - a keycap: `0-9`, `#` or `*`, an optional VS16, U+20E3;
   - a subdivision flag: ONLY the three RGI ones, England, Scotland and Wales
     (U+1F3F4, the tag letters `gbeng`, `gbsct` or `gbwls`, CANCEL TAG). Any
     other tag text is hidden ASCII and is refused;
   - one or more elements joined by ZWJ. An element is a pictograph (the
     Extended_Pictographic property), an optional variation selector and an
     optional skin tone. A skin tone may follow only an Emoji_Modifier_Base
     (`👍🏽` passes, `🚀🏽` does not).
   Each of these is one grapheme cluster (UAX #29), so a ZWJ sequence (`👩‍💻`,
   a family, a couple, `🏳️‍🌈`, `❤️‍🔥`) and a skin tone sequence pass. Text, two
   emoji side by side, a lone modifier, a dangling ZWJ, a ZWJ chain of letters
   (`ⓐ‍ⓑ`) and symbols that are not pictographs (`⌘`, `█`, the braille blank
   U+2800, U+FFFC, U+FFFD, U+FDFD) are `400 validation_failed`. The check is
   structural: it does not hold the list of RGI ZWJ sequences, so a chain of
   pictographs that no vendor draws (`🚀‍🚀`) passes and shows as two glyphs.
   It is still one grapheme and at most 32 bytes.
3. **No new dependency.** The Go standard library has no table of the Unicode
   emoji properties and no grapheme segmenter. `emoji.go` holds two small range
   tables copied from `emoji-data.txt` (Unicode 15.1): Extended_Pictographic
   (the bases) and Emoji_Modifier_Base (the bases a skin tone may follow). A
   test checks that both are sorted and that every modifier base is a
   pictograph. A future Unicode version can add emoji: update the tables then.
   Extended_Pictographic also holds code points that are not RGI emoji today:
   a few old symbols (for example U+2607 ☇ LIGHTNING) and the unassigned code
   points Unicode keeps for future emoji. They are accepted on purpose: each is
   one symbol (or a missing-glyph box) and harmless as an icon.
4. **32 bytes (the project owner, 2026-11-02).** The ticket's first default
   was 16 bytes; it refused real emoji: families of three or more (`👨‍👩‍👧` is
   18 bytes, `👨‍👩‍👧‍👦` 25), couples with skin tones (`👩🏽‍❤️‍👨🏿` 28) and the
   subdivision flags (England, Scotland, Wales are 28 bytes). 32 bytes takes
   all of these. It still refuses the longest sequences, for example a kiss
   with two skin tones (35 bytes). Every emoji of the web grid is within the
   limit (a web test checks).

## API, events, MCP

5. **The existing project routes carry it.** `emoji` on `POST /projects`
   (string, empty means none), `PATCH /projects/{project}` (string or null;
   empty or null clears) and every `Project` read (string or null). There is no
   new route, no new query outside the project row, and no new team-access rule:
   the project scope of `LockProject` and the project select still decide who
   sees and writes the project (`404` for a hidden or other-org project).
6. **A change is a content change.** It goes through `UpdateProject`, bumps
   `version` and `updated_at`, and emits `project.updated` with `"emoji"` in
   `changes`, the LAST statement of the transaction, like every other field.
   The payload holds the field name only, never the value. A PATCH with the
   same value writes nothing. (A logo change does not bump `version`,
   `decisions-T24` #13; the emoji is a plain field of the PATCH body, so it
   does.)
7. **MCP.** `save_project` takes `emoji` (nullable; null clears on update).
   `get_project` and `list_projects` pass the REST body through, so they carry
   it. The tool golden changed by one property.

## Web

8. **Order.** Wherever a project icon shows (project list, project page header,
   the logo preview, the issue Project chip and picker, the create dialog chip
   and picker, the issue filter picker), the shared `Logo` shows the image,
   else the emoji, else the letter tile. A failed image load falls back the same
   way. A decorative icon (a name is next to it) is hidden from a screen reader;
   the preview names the project. The sidebar lists no projects (only a
   "Projects" link), so there is no project icon there; this stays as it is
   (the project owner, 2026-11-02).
9. **Picker.** An "Emoji" section on the project page, under the logo section:
   a grid of 24 common emoji (each button has a name and `aria-pressed`), one
   tab stop with arrow keys, Home and End inside it; a text field to paste one
   emoji ("Use emoji", Enter works); "Remove emoji" when one is set. The web
   makes a quick check first (one grapheme by `Intl.Segmenter`, at most 32
   bytes, no control character, a pictograph in it); the server's check is the
   real one. Errors are fixed sentences; no server text reaches the screen.
