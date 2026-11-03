import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

// LAUN-55: the CSS contract behind the drag-scroll (useDragScroll.ts). jsdom has no layout.
const css = readFileSync(resolve(process.cwd(), 'src/features/issues/issues.module.css'), 'utf8').replace(
  /\/\*[\s\S]*?\*\//g,
  '',
);
function rule(sel: string): string {
  let out = '';
  for (const m of css.matchAll(/(?:^|\n)([^{}\n@][^{}]*)\{([^}]*)\}/g)) {
    if (m[1]!.split(',').some((x) => x.trim() === sel)) out += m[2]!;
  }
  return out;
}

describe('board drag-scroll CSS (LAUN-55)', () => {
  it('the board shows grab and keeps its sideways-only scroll', () => {
    const r = rule('.board');
    expect(r).toMatch(/cursor:\s*grab/);
    expect(r).toMatch(/overflow-x:\s*auto/);
    expect(r).toMatch(/overflow-y:\s*hidden/);
  });
  it('while it runs: grabbing everywhere, no text selection', () => {
    for (const sel of ['.board.dragScrolling', '.board.dragScrolling *']) {
      expect(rule(sel)).toMatch(/cursor:\s*grabbing/);
      expect(rule(sel)).toMatch(/user-select:\s*none/);
    }
  });
});
