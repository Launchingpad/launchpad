import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

// LAUN-230: jsdom has no layout, so these pin the CSS rules as text (like detailTone.layout.test.ts).
const read = (p: string) => readFileSync(resolve(process.cwd(), p), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');
const detail = read('src/features/issues/detail.module.css');
const issues = read('src/features/issues/issues.module.css');
const ui = read('src/styles/ui.module.css');
const pill = read('src/ui/labelPill.module.css');

/** All declarations of a selector in a sheet, from every rule (outside @media) that lists it. */
function decls(css: string, sel: string): string {
  const plain = css.replace(/@media[^{]*\{[\s\S]*?\n\}\n/g, '');
  return [...plain.matchAll(/([^{}]+)\{([^{}]*)\}/g)]
    .filter((m) => m[1]!.split(',').some((s) => s.trim() === sel))
    .map((m) => m[2])
    .join('\n');
}
function gridSelectors(css: string): string[] {
  const plain = css.replace(/@media[^{]*\{[\s\S]*?\n\}\n/g, '');
  const out = new Set<string>();
  for (const m of plain.matchAll(/([^{}]+)\{([^{}]*)\}/g)) {
    if (/display:\s*grid/.test(m[2]!)) for (const s of m[1]!.split(',')) out.add(s.trim());
  }
  return [...out];
}
const hasColumns = (d: string) => /grid-template-columns:|grid-template:/.test(d);

describe('nothing on the issue page is wider than its column (LAUN-230)', () => {
  it('every grid in the detail sheet names its columns (an implicit auto column grows to its content)', () => {
    // .playBadge only centres an icon in a fixed box.
    const missing = gridSelectors(detail).filter((s) => s !== '.playBadge' && !hasColumns(decls(detail, s)));
    expect(missing).toEqual([]);
  });
  it('the one-column grids of the sub-issues, properties, comments and changes use minmax(0, 1fr)', () => {
    for (const s of [
      '.descEdit',
      '.sub',
      '.card',
      '.props',
      '.propSection',
      '.crBlock',
      '.crList',
      '.crBody',
      '.attArea',
    ]) {
      expect(decls(detail, s), s).toMatch(/grid-template-columns:\s*minmax\(0, 1fr\)/);
    }
    for (const s of ['.thread', '.comment', '.compose']) {
      expect(decls(issues, s), s).toMatch(/grid-template-columns:\s*minmax\(0, 1fr\)/);
    }
  });
  it('a related or sub-issue title truncates in its row', () => {
    const r = decls(issues, '.relLink');
    expect(r).toMatch(/min-width:\s*0/);
    expect(r).toMatch(/text-overflow:\s*ellipsis/);
  });
  it('a long label pill in the Properties row shrinks and shows an ellipsis', () => {
    const li = decls(pill, '.row > li');
    expect(li).toMatch(/min-width:\s*0/);
    expect(li).toMatch(/max-width:\s*100%/);
    expect(decls(pill, '.pill')).toMatch(/max-width:\s*100%/);
    const name = decls(pill, '.name');
    expect(name).toMatch(/min-width:\s*0/);
    expect(name).toMatch(/text-overflow:\s*ellipsis/);
  });
  it('a long one-word name in a Properties picker button breaks inside the button', () => {
    const r = decls(detail, '.ghostBtn');
    expect(r).toMatch(/max-width:\s*100%/);
    expect(r).toMatch(/overflow-wrap:\s*anywhere/);
  });
  it('a long one-word author name wraps in the comment head', () => {
    const r = decls(issues, '.commentHead strong');
    expect(r).toMatch(/min-width:\s*0/);
    expect(r).toMatch(/overflow-wrap:\s*anywhere/);
  });
});

/** Every rule of a sheet, inside @media blocks too, as [selector list, declarations]. */
function allRules(css: string): [string, string][] {
  return [...css.matchAll(/([^{}]+)\{([^{}]*)\}/g)]
    .map((m) => [m[1]!.replace(/^[\s\S]*\{/, ''), m[2]!] as [string, string])
    .filter(([sel]) => !sel.trim().startsWith('@'));
}

/** The declarations of a selector inside the `@media (max-width: 760px)` blocks (the phone layout). */
function phoneDecls(css: string, sel: string): string {
  return [...css.matchAll(/@media \(max-width: 760px\) \{([\s\S]*?)\n\}\n/g)]
    .map((m) => decls(m[1]!, sel))
    .join('\n');
}

describe('long labels stay in an issue row and a board card (LAUN-233)', () => {
  it('the labels column takes its content width, but never the title floor of 120px', () => {
    // An `auto` labels column grew to three long pills and squeezed the title to nothing.
    expect(decls(issues, '.row')).toMatch(
      /grid-template-columns:\s*20px 18px 74px var\(--row-state, 18px\) minmax\(120px, 1fr\) minmax\(0, max-content\) 28px var\(--row-date, 40px\)/,
    );
  });
  it('the labels box is one clipped line whose cap grows with the screen, with room for a focus ring', () => {
    const l = decls(issues, '.labels');
    expect(l).toMatch(/box-sizing:\s*content-box/);
    expect(l).toMatch(/min-width:\s*0/);
    expect(l).toMatch(/max-width:\s*max\(400px, 30vw\)/);
    expect(l).toMatch(/padding:\s*0 4px/);
    expect(l).toMatch(/margin:\s*0 -4px/);
    expect(l).toMatch(/overflow-x:\s*clip/);
    // Nothing clips it vertically, so a large minimum font size cuts no text.
    expect(l).not.toMatch(/max-height|overflow:\s|overflow-y/);
    expect(l).not.toMatch(/flex-wrap/);
    const empty = decls(issues, '.labels:empty');
    expect(empty).toMatch(/padding:\s*0/);
    expect(empty).toMatch(/margin:\s*0/);
  });
  it('a pill keeps its width (a long one is capped at 160px) and a cut one is hidden', () => {
    expect(decls(issues, '.labels > *')).toMatch(/flex:\s*none/);
    expect(decls(issues, '.labels > :not(.projectChip)')).toMatch(/max-width:\s*160px/);
    // A percentage max-width counts as none for the max-content size: a long project widened the column to the cap.
    expect(decls(issues, '.labels > .projectChip')).toBe('');
    expect(decls(issues, '.labels > [data-clipped]')).toMatch(/opacity:\s*0/);
  });
  it('the "+N" count sits outside the clipped box and keeps its width', () => {
    const c = decls(issues, '.labelsCell');
    expect(c).toMatch(/display:\s*inline-flex/);
    expect(c).toMatch(/min-width:\s*0/);
    expect(c).not.toMatch(/overflow/);
    expect(decls(issues, '.labelsCell > .more')).toMatch(/flex:\s*none/);
    // Above the stretched title link, so the hover text shows.
    expect(decls(issues, '.row .labelsCell')).toMatch(/z-index:\s*1/);
  });
  // Text pins cannot see every cascade override (a later rule can still win); these catch the likely ones.
  it('only the two known rules set the overflow of the labels box', () => {
    const hits = allRules(issues).flatMap(([sel, body]) =>
      sel
        .split(',')
        .map((x) => x.trim())
        .filter((x) => /\.labels$/.test(x))
        .flatMap((x) =>
          [...body.matchAll(/(overflow(?:-[xy])?):\s*([^;]+);/g)].map((d) => `${x} ${d[1]}: ${d[2]}`),
        ),
    );
    expect(hits).toEqual(['.labels overflow-x: clip', '.row .labels overflow: hidden']);
  });
  it('a cut pill is hidden by one rule, and nothing else touches it or the count', () => {
    const clipped = allRules(issues).filter(([sel]) => sel.includes('[data-clipped]'));
    expect(clipped.map(([sel]) => sel.trim())).toEqual(['.labels > [data-clipped]']);
    expect(clipped[0]![1]).toMatch(/opacity:\s*0/);
    const more = allRules(issues).filter(([sel]) => sel.split(',').some((x) => /\.more$/.test(x.trim())));
    for (const [sel, body] of more) {
      expect(body, sel).not.toMatch(/display:\s*none|visibility:|opacity:/);
    }
  });
  it('on a phone the labels line is as before: the chip gives way first, the box clips', () => {
    expect(phoneDecls(issues, '.row .labelsCell')).toMatch(/grid-area:\s*l/);
    const l = phoneDecls(issues, '.row .labels');
    expect(l).toMatch(/overflow:\s*hidden/);
    expect(l).toMatch(/max-width:\s*none/);
    expect(l).toMatch(/padding:\s*0/);
    expect(l).toMatch(/margin:\s*0/);
    expect(phoneDecls(issues, '.row .labels > .projectChip')).toMatch(/flex:\s*0 1 auto/);
    expect(phoneDecls(issues, '.row .labels > :not(.projectChip)')).toMatch(/max-width:\s*100%/);
  });
  it('a board column and a card have one minmax(0, 1fr) column, so a long pill cannot widen them', () => {
    for (const s of ['.cards', '.card']) {
      expect(decls(issues, s), s).toMatch(/grid-template-columns:\s*minmax\(0, 1fr\)/);
    }
  });
  it('a long pill in a card takes at most one line and the priority icon keeps its size', () => {
    expect(decls(issues, '.cardMeta')).toMatch(/min-width:\s*0/);
    expect(decls(issues, '.cardMeta > *')).toMatch(/min-width:\s*0/);
    expect(decls(issues, '.cardMeta > :not(.projectChip)')).toMatch(/max-width:\s*100%/);
    expect(decls(issues, '.cardMeta > svg')).toMatch(/flex:\s*none/);
  });
  it('the project chip keeps its 160px cap in a card', () => {
    expect(decls(issues, '.projectChip')).toMatch(/max-width:\s*160px/);
    expect(decls(issues, '.cardMeta > .projectChip')).toMatch(/max-width:\s*min\(160px, 100%\)/);
  });
  it('no `> *` rule sets a max-width (same specificity as `.projectChip` but later, so it wins over its cap)', () => {
    // LAUN-233 review: `.cardMeta > * { max-width: 100% }` replaced `.projectChip { max-width: 160px }`.
    const all = issues + detail;
    const hits = [...all.matchAll(/([^{}]+)\{([^{}]*)\}/g)].filter(
      (m) => m[1]!.split(',').some((s) => /> \*$/.test(s.trim())) && /(^|[\s;])max-width:/.test(m[2]!),
    );
    expect(hits.map((m) => m[1]!.trim())).toEqual([]);
  });
  it('the icon and the chevron of a Properties picker button keep their width beside a long name', () => {
    expect(decls(detail, '.ghostBtn > svg')).toMatch(/flex:\s*none/);
  });
});

describe('rendered markdown stays in its box (LAUN-230)', () => {
  it('long words and URLs wrap and the block can shrink', () => {
    const r = decls(ui, '.md');
    expect(r).toMatch(/overflow-wrap:\s*anywhere/);
    expect(r).toMatch(/min-width:\s*0/);
    expect(r).toMatch(/max-width:\s*100%/);
  });
  it('a code block and a table scroll inside themselves', () => {
    for (const s of ['.md pre', '.md table']) {
      const r = decls(ui, s);
      expect(r, s).toMatch(/max-width:\s*100%/);
      expect(r, s).toMatch(/overflow-x:\s*auto/);
    }
    expect(decls(ui, '.md table')).toMatch(/display:\s*block/);
  });
  it('an image shrinks to the column', () => {
    expect(decls(ui, '.md img')).toMatch(/max-width:\s*100%/);
  });
  it('no markdown rule clips its content (a focus ring must stay visible)', () => {
    const plain = ui.replace(/@media[^{]*\{[\s\S]*?\n\}\n/g, '');
    const clip = [...plain.matchAll(/([^{}]+)\{([^{}]*)\}/g)].filter(
      (m) =>
        m[1]!.split(',').some((s) => /^\.md\b/.test(s.trim())) &&
        /overflow(-x|-y)?:\s*(hidden|clip)/.test(m[2]!),
    );
    expect(clip.map((m) => m[1]!.trim())).toEqual([]);
  });
});
