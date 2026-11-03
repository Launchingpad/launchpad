import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { contrastRatio as ratio } from './wcag';

// LAUN-235 (docs/internal/decisions-T42-themes.md): WCAG AA from the token values of the Paper and Terminal
// themes, every variant. Text 4.5:1, UI parts (focus ring, control border, toggle fill, a state glyph) 3:1.
// The Terminal theme writes its state as a word ([WIP ]), so its state colours are text: 4.5:1.

const read = (f: string) =>
  readFileSync(resolve(process.cwd(), 'src/styles', f), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');

/** Every rule of a stylesheet as [selector list, declarations], with @media rules flattened (prefixed). */
function rules(css: string): [string, Record<string, string>][] {
  const out: [string, Record<string, string>][] = [];
  let i = 0;
  while (i < css.length) {
    const open = css.indexOf('{', i);
    if (open < 0) break;
    const sel = css.slice(i, open).trim();
    if (sel.startsWith('@media')) {
      let depth = 1;
      let j = open + 1;
      for (; j < css.length && depth; j++) depth += css[j] === '{' ? 1 : css[j] === '}' ? -1 : 0;
      for (const [s, d] of rules(css.slice(open + 1, j - 1))) out.push([`${sel} ${s}`, d]);
      i = j;
      continue;
    }
    const close = css.indexOf('}', open);
    const decl = Object.fromEntries(
      [...css.slice(open + 1, close).matchAll(/(--[a-z-]+|color-scheme):\s*([^;]+);/g)].map((m) => [
        m[1]!.replace(/^--/, ''),
        m[2]!.trim(),
      ]),
    );
    out.push([sel.replace(/\s+/g, ' '), decl]);
    i = close + 1;
  }
  return out;
}

const themes = rules(read('themes.css'));
const linear = rules(read('tokens.css'));
const merged = (pick: (sel: string) => boolean, list = themes) =>
  Object.assign({}, ...list.filter(([s]) => pick(s)).map(([, d]) => d)) as Record<string, string>;

const LOOKS = ['paper', 'terminal'] as const;
const variants = (look: string) => ({
  light: merged((s) => s === `[data-preview='${look}'], :root[data-look='${look}']`),
  'dark (OS)': merged(
    (s) =>
      s ===
      `@media (prefers-color-scheme: dark) :root:not([data-theme='light']) [data-preview='${look}'], :root[data-look='${look}']:not([data-theme='light'])`,
  ),
  'dark (chosen)': merged(
    (s) =>
      s ===
      `:root[data-theme='dark'] [data-preview='${look}'], :root[data-look='${look}'][data-theme='dark']`,
  ),
});
const hex = /^#[0-9a-f]{6}$/;
const linearColours = Object.entries(merged((s) => s.endsWith(':root'), linear))
  .filter(([, v]) => hex.test(v))
  .map(([k]) => k);

const TEXT: [string, string][] = [
  ['text', 'bg'],
  ['text', 'bg-page'],
  ['text', 'bg-card'],
  ['text', 'surface'],
  ['text', 'surface-hover'],
  ['text', 'card-surface'],
  ['text', 'card-surface-hover'],
  ['text', 'page-hover'],
  ['text', 'selected'],
  ['muted', 'bg'],
  ['muted', 'bg-page'],
  ['muted', 'bg-card'],
  ['muted', 'surface'],
  ['muted', 'surface-hover'],
  ['muted', 'card-surface'],
  ['muted', 'card-surface-hover'],
  ['muted', 'page-hover'],
  ['muted', 'selected'],
  ['accent', 'bg'],
  ['accent', 'bg-page'],
  ['accent', 'bg-card'],
  ['accent', 'surface'],
  ['accent', 'card-surface'],
  ['accent', 'selected'],
  ['on-accent', 'accent-solid'],
  ['danger', 'bg'],
  ['danger', 'danger-bg'],
  ['warn-text', 'warn-bg'],
  ['success', 'bg'],
  ['bg', 'text'], // toast
  ['bg', 'danger'], // error toast, danger button
];
const UI: [string, string][] = [
  ['focus', 'bg'],
  ['focus', 'bg-page'],
  ['focus', 'bg-card'],
  ['focus', 'surface'],
  ['border-strong', 'bg'],
  ['border-strong', 'bg-page'],
  ['border-strong', 'surface'],
  ['accent-solid', 'bg'], // primary button, toggle on
  ['accent-solid', 'bg-page'],
];
const STATES = ['backlog', 'todo', 'progress', 'review', 'testing', 'done', 'canceled'];
/**
 * Every background a state glyph, tag or row text can sit on, taken from the theme's own tokens (a new background
 * token is checked without editing this file): the page, card, surface and hover tones and the selected row. The
 * alert tints (danger-bg, warn-bg) hold their own text only.
 */
const backgrounds = (t: Record<string, string>) =>
  Object.keys(t).filter(
    (k) => /^(bg|bg-.+|.*surface.*|.*-hover|selected)$/.test(k) && !/^(danger|warn)-bg$/.test(k),
  );

describe('theme contrast (LAUN-235)', () => {
  for (const look of LOOKS) {
    const vs = variants(look);
    it(`${look}: the two dark blocks are identical`, () => {
      expect(Object.keys(vs['dark (OS)']).length).toBeGreaterThan(20);
      expect(vs['dark (OS)']).toEqual(vs['dark (chosen)']);
    });
    for (const [vname, t] of Object.entries(vs)) {
      it(`${look} ${vname}: defines every Linear colour token (none leaks in)`, () => {
        const missing = linearColours.filter((k) => !hex.test(t[k] ?? ''));
        expect(missing).toEqual([]);
        expect(t['color-scheme']).toBe(vname === 'light' ? 'light' : 'dark');
      });
      for (const [fg, bg] of TEXT) {
        it(`${look} ${vname}: text ${fg} on ${bg} is at least 4.5:1`, () => {
          const r = ratio(t[fg]!, t[bg]!);
          expect(r, `${t[fg]} on ${t[bg]} = ${r.toFixed(2)}`).toBeGreaterThanOrEqual(4.5);
        });
      }
      for (const [fg, bg] of UI) {
        it(`${look} ${vname}: UI ${fg} on ${bg} is at least 3:1`, () => {
          const r = ratio(t[fg]!, t[bg]!);
          expect(r, `${t[fg]} on ${t[bg]} = ${r.toFixed(2)}`).toBeGreaterThanOrEqual(3);
        });
      }
      it(`${look} ${vname}: the state check covers every background token`, () => {
        expect(backgrounds(t).sort()).toEqual(
          [
            'bg',
            'bg-card',
            'bg-page',
            'card-surface',
            'card-surface-hover',
            'page-hover',
            'selected',
            'surface',
            'surface-hover',
          ].sort(),
        );
      });
      for (const bg of [
        'bg-card',
        'bg-page',
        'card-surface',
        'card-surface-hover',
        'page-hover',
        'selected',
      ]) {
        it(`${look} ${vname}: text and muted on ${bg} are at least 4.5:1`, () => {
          for (const fg of ['text', 'muted']) {
            const r = ratio(t[fg]!, t[bg]!);
            expect(r, `${fg} on ${bg} = ${r.toFixed(2)}`).toBeGreaterThanOrEqual(4.5);
          }
        });
      }
      const min = look === 'terminal' ? 4.5 : 3;
      for (const s of STATES) {
        it(`${look} ${vname}: state ${s} is at least ${min}:1 on every row background`, () => {
          for (const bg of backgrounds(t)) {
            const r = ratio(t[`state-${s}`]!, t[bg]!);
            expect(r, `${s} ${t[`state-${s}`]} on ${bg} ${t[bg]} = ${r.toFixed(2)}`).toBeGreaterThanOrEqual(
              min,
            );
          }
        });
      }
    }
  }

  // The token sheets in Penpot (LAUN-234) are the source: these anchor values must not drift.
  it('keeps the Penpot sheet anchors', () => {
    const p = variants('paper');
    const t = variants('terminal');
    expect([p.light.bg, p.light['bg-page'], p.light.text, p.light.accent]).toEqual([
      '#fbf8f1',
      '#ede6d8',
      '#2b2620',
      '#2f5670',
    ]);
    expect([p['dark (OS)'].bg, p['dark (OS)'].text, p['dark (OS)'].accent]).toEqual([
      '#1a1714',
      '#ede5d6',
      '#9fc0d8',
    ]);
    expect([t['dark (OS)'].bg, t['dark (OS)'].text, t['dark (OS)'].accent]).toEqual([
      '#0a0e0b',
      '#d3f2d9',
      '#ffb000',
    ]);
    expect([t.light.bg, t.light.text, t.light['accent-solid'], t.light['on-accent']]).toEqual([
      '#f6f6ef',
      '#121310',
      '#121310',
      '#ffb000',
    ]);
  });

  it('gives each theme its type and shape', () => {
    const shape = (look: string) =>
      merged((s) => s === `[data-preview='${look}'], :root[data-look='${look}']`);
    expect(shape('paper').radius).toBe('6px');
    expect(shape('paper')['font-heading']).toMatch(/^'Fraunces'/);
    expect(shape('paper').font).toMatch(/^'Source Sans 3'/);
    expect(shape('paper').mono).toMatch(/^'IBM Plex Mono'/);
    expect(shape('terminal').radius).toBe('0px');
    expect(shape('terminal').font).toMatch(/^'JetBrains Mono'/);
    for (const look of LOOKS) {
      expect(shape(look).shadow).toBe('none');
      expect(shape(look)['shadow-card']).toBe('none');
    }
  });
});
