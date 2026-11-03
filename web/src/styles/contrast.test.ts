import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

const css = readFileSync(resolve(process.cwd(), 'src/styles/tokens.css'), 'utf8');

function block(selector: RegExp): Record<string, string> {
  const m = selector.exec(css);
  if (!m) throw new Error(`block not found: ${selector}`);
  const body = css.slice(m.index + m[0].length, css.indexOf('}', m.index + m[0].length));
  return Object.fromEntries(
    [...body.matchAll(/--([a-z-]+):\s*(#[0-9a-fA-F]{6})\s*;/g)].map((x) => [x[1]!, x[2]!]),
  );
}
const light = block(/:root\s*\{/);
const darkOs = block(/:root:not\(\[data-theme='light'\]\)\s*\{/);
const darkManual = block(/:root\[data-theme='dark'\]\s*\{/);

function lum(hex: string): number {
  const c = [1, 3, 5]
    .map((i) => parseInt(hex.slice(i, i + 2), 16) / 255)
    .map((v) => (v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4));
  return 0.2126 * c[0]! + 0.7152 * c[1]! + 0.0722 * c[2]!;
}
export function ratio(a: string, b: string): number {
  const [hi, lo] = [lum(a), lum(b)].sort((x, y) => y - x);
  return (hi! + 0.05) / (lo! + 0.05);
}

// [foreground token, background token]
const PAIRS: [string, string][] = [
  ['text', 'bg'],
  ['text', 'surface'],
  ['text', 'surface-hover'],
  ['text', 'selected'],
  ['muted', 'bg'],
  ['text', 'bg-page'],
  ['muted', 'bg-page'],
  ['accent', 'bg-page'],
  ['text', 'bg-card'],
  ['muted', 'bg-card'],
  ['accent', 'bg-card'],
  ['muted', 'surface'],
  ['muted', 'surface-hover'],
  ['text', 'card-surface'],
  ['muted', 'card-surface'],
  ['accent', 'card-surface'],
  ['text', 'card-surface-hover'],
  ['muted', 'card-surface-hover'],
  ['text', 'page-hover'],
  ['muted', 'page-hover'],
  ['accent', 'bg'],
  ['accent', 'surface'],
  ['accent', 'selected'],
  ['on-accent', 'accent-solid'],
  ['danger', 'bg'],
  ['danger', 'danger-bg'],
  ['warn-text', 'warn-bg'],
  ['bg', 'text'], // toast
  ['bg', 'danger'], // error toast
];

describe('WCAG AA contrast (4.5:1) of every text/background token pair', () => {
  for (const [name, theme] of [
    ['light', light],
    ['dark (OS)', darkOs],
    ['dark (manual)', darkManual],
  ] as const) {
    for (const [fg, bg] of PAIRS) {
      it(`${name}: ${fg} on ${bg}`, () => {
        const r = ratio(theme[fg]!, theme[bg]!);
        expect(r, `${theme[fg]} on ${theme[bg]} = ${r.toFixed(2)}`).toBeGreaterThanOrEqual(4.5);
      });
    }
  }
  it('the two dark blocks are identical', () => {
    expect(darkOs).toEqual(darkManual);
  });
  it('focus ring is visible against the page (3:1)', () => {
    expect(ratio(light.focus!, light.bg!)).toBeGreaterThanOrEqual(3);
    expect(ratio(darkOs.focus!, darkOs.bg!)).toBeGreaterThanOrEqual(3);
  });
});

// LAUN-64: the sidebar group header and its count take their colours from tokens. This reads the rules in
// shell.module.css and checks the token each one names, so a change to a raw colour or another token is seen.
describe('sidebar group header and count (LAUN-64)', () => {
  const shell = readFileSync(resolve(process.cwd(), 'src/features/shell/shell.module.css'), 'utf8');
  const decl = (sel: string, prop: string): string => {
    const body = new RegExp(`${sel.replace(/[.:]/g, '\\$&')}\\s*\\{([^}]*)\\}`).exec(shell)?.[1] ?? '';
    const v = new RegExp(`(?:^|[\\s;])${prop}:\\s*var\\(--([a-z-]+)\\)`).exec(body)?.[1];
    if (!v) throw new Error(`${sel} has no ${prop}: var(--token)`);
    return v;
  };
  // [rule, text colour rule, the background it sits on]
  const cases: [string, string, string, string][] = [
    ['header', '.groupHead', 'color', 'surface'],
    ['count', '.groupCount', 'color', 'surface'],
    ['header on hover', '.groupHead:hover', 'color', 'surface-hover'],
    ['count on hover', '.groupHead:hover .groupCount', 'color', 'surface-hover'],
  ];
  for (const [name, themeName, theme] of [
    ['light', 'light', light],
    ['dark (OS)', 'dark', darkOs],
    ['dark (manual)', 'dark', darkManual],
  ] as const) {
    for (const [label, sel, prop, bgToken] of cases) {
      it(`${name}: ${label} is at least 4.5:1`, () => {
        // The hover background is the rule's own `background`; the others sit on the sidebar (--surface).
        if (sel === '.groupHead:hover') expect(decl(sel, 'background')).toBe(bgToken);
        const fg = theme[decl(sel, prop)]!;
        const r = ratio(fg, theme[bgToken]!);
        expect(
          r,
          `${themeName} ${label}: ${fg} on ${theme[bgToken]} = ${r.toFixed(2)}`,
        ).toBeGreaterThanOrEqual(4.5);
      });
    }
  }
  it('the sidebar itself is --surface', () => {
    expect(decl('.sidebar', 'background')).toBe('surface');
  });
});
