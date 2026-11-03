import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

// jsdom has no layout: guard the CSS text of the Columns panel at phone width (LAUN-163).
const css = readFileSync(resolve(process.cwd(), 'src/features/issues/issues.module.css'), 'utf8');
const narrow = css.slice(css.indexOf('@media (max-width: 760px)'));

describe('Columns panel layout', () => {
  it('opens rightwards from the left edge and never wider than the screen on a phone', () => {
    const rule = /\.fold \.columnsPanel\s*\{([^}]*)\}/.exec(narrow)?.[1] ?? '';
    expect(rule).toMatch(/left:\s*0/);
    expect(rule).toMatch(/right:\s*0/);
    expect(rule).toMatch(/max-width:\s*none/);
    // The button's wrapper is not the anchor: the fold is, so the panel cannot pass the right edge.
    expect(/\.fold \.columnsWrap\s*\{[^}]*position:\s*static/.test(narrow)).toBe(true);
    expect(/\.fold\s*\{[^}]*position:\s*relative/.test(narrow)).toBe(true);
  });
});
