import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

// jsdom has no layout, so these pin the CSS contract behind LAUN-52 (columns as tall as the board).
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

describe('board columns stretch to the bottom (LAUN-52)', () => {
  it('the scroll area holds the board in a fixed-height flex column and does not scroll', () => {
    expect(rule('.scrollBoard')).toMatch(/display:\s*flex/);
    expect(rule('.scrollBoard')).toMatch(/flex-direction:\s*column/);
    expect(rule('.scrollBoard')).toMatch(/overflow:\s*hidden/);
  });
  it('the board fills the area, stretches its columns and scrolls only sideways', () => {
    const r = rule('.board');
    expect(r).toMatch(/flex:\s*1/);
    expect(r).toMatch(/min-height:\s*0/);
    expect(r).toMatch(/align-items:\s*stretch/);
    expect(r).toMatch(/overflow-x:\s*auto/);
    expect(r).toMatch(/overflow-y:\s*hidden/);
    expect(r).not.toMatch(/align-items:\s*flex-start/);
  });
  it('the column is the gray panel, full height, and can shrink so the cards scroll inside it', () => {
    const r = rule('.column');
    expect(r).toMatch(/background:\s*var\(--surface\)/);
    expect(r).toMatch(/min-height:\s*0/);
    expect(r).toMatch(/flex-direction:\s*column/);
    expect(r).not.toMatch(/align-self:/);
    expect(r).not.toMatch(/(?:^|;)\s*height:/);
    expect(r).not.toMatch(/max-height:/);
  });
  it('the card list takes the rest of the column and scrolls', () => {
    const r = rule('.cards');
    expect(r).toMatch(/flex:\s*1/);
    expect(r).toMatch(/overflow-y:\s*auto/);
    expect(r).toMatch(/align-content:\s*start/);
  });
});
