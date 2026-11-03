import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

// jsdom has no layout, so these pin the CSS contract behind LAUN-50 (the stretched title link).
// The real check is the headless-browser run in docs/internal/web-decisions.md.
const css = readFileSync(resolve(process.cwd(), 'src/features/issues/issues.module.css'), 'utf8').replace(
  /\/\*[\s\S]*?\*\//g,
  '',
);
/** Declarations of every top-level rule whose selector list contains exactly `sel`, joined. */
function rule(sel: string): string {
  let out = '';
  for (const m of css.matchAll(/(?:^|\n)([^{}\n@][^{}]*)\{([^}]*)\}/g)) {
    if (m[1]!.split(',').some((x) => x.trim() === sel)) out += m[2]!;
  }
  return out;
}
const num = (decl: string, prop: string) => Number(new RegExp(`${prop}:\\s*(-?\\d+)`).exec(decl)?.[1] ?? NaN);

describe('stretched title link (LAUN-50)', () => {
  it('the row and the card are positioned and their own stacking context', () => {
    expect(rule('.row')).toMatch(/position:\s*relative/);
    expect(rule('.card')).toMatch(/position:\s*relative/);
    expect(rule('.row')).toMatch(/isolation:\s*isolate/);
    expect(rule('.card')).toMatch(/isolation:\s*isolate/);
  });
  it('the ::after covers the card and the link does not clip it', () => {
    for (const sel of ['.title::after', '.cardTitle::after']) {
      expect(rule(sel)).toMatch(/position:\s*absolute/);
      expect(rule(sel)).toMatch(/inset:\s*0/);
    }
    expect(rule('.title')).not.toMatch(/position:/);
    expect(rule('.cardTitle')).not.toMatch(/position:/);
    expect(rule('.title')).not.toMatch(/overflow:/);
    expect(rule('.cardTitle')).not.toMatch(/overflow:/);
    expect(rule('.titleText')).toMatch(/overflow:\s*hidden/);
  });
  it('controls and the identifier sit above the overlay', () => {
    const overlay = num(rule('.title::after'), 'z-index');
    for (const sel of [
      '.check',
      '.cardCheck',
      '.row button',
      '.row input',
      '.card input',
      '.row .ident',
      '.card .ident',
      '.row .prio',
      '.row .status',
      '.row .assignee',
      '.row .updated',
    ])
      expect(num(rule(sel), 'z-index'), sel).toBeGreaterThan(overlay);
    expect(rule('.row .ident')).toMatch(/user-select:\s*text/);
  });
  it('the sticky group head is above the row controls and below the floating bar', () => {
    const head = num(rule('.groupHead'), 'z-index');
    expect(head).toBeGreaterThan(num(rule('.check'), 'z-index'));
    expect(head).toBeLessThan(num(rule('.selFloat'), 'z-index'));
  });
  it('hover never beats a selected or focused card or row', () => {
    expect(rule('.card:hover:not(.selected):not(.focused)')).toMatch(/background/);
    expect(rule('.row:hover:not(.selected):not(.focused)')).toMatch(/background/);
    expect(css).not.toMatch(/\n\.card:hover\s*\{/);
    expect(css).not.toMatch(/\n\.row:hover\s*\{/);
  });
  it('the focus ring is on the overlay, not behind :has()', () => {
    expect(rule('.title:focus-visible::after')).toMatch(/outline:\s*2px solid var\(--focus\)/);
    expect(rule('.cardTitle:focus-visible::after')).toMatch(/outline:\s*2px solid var\(--focus\)/);
    expect(css).not.toMatch(/:has\(/);
  });
});
