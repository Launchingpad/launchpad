import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

// jsdom has no layout: pin the pill contract of the issue header buttons (LAUN-182).
const css = readFileSync(resolve(process.cwd(), 'src/features/issues/detail.module.css'), 'utf8').replace(
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

describe('issue header copy buttons are pills (LAUN-182)', () => {
  it('are fully round with a token border and background', () => {
    const r = rule('.toolbar .toolBtn');
    expect(r).toMatch(/border-radius:\s*999px/);
    expect(r).toMatch(/border:\s*1px solid var\(--border\)/);
    expect(r).toMatch(/background:\s*var\(--bg-card\)/);
  });
  it('have hover and active states', () => {
    expect(rule('.toolbar .toolBtn:hover:not(:disabled)')).toMatch(/var\(--surface-hover\)/);
    expect(rule('.toolbar .toolBtn:active:not(:disabled)')).toMatch(/background/);
  });
  it('are 40px tall on a phone or a coarse pointer, after the base rule', () => {
    const m = /@media \(max-width: 760px\), \(pointer: coarse\) \{\s*\.toolbar \.toolBtn \{([^}]*)\}/.exec(
      css,
    );
    expect(m?.[1]).toMatch(/min-height:\s*40px/);
    expect(css.indexOf(m![0])).toBeGreaterThan(css.indexOf('.toolbar .toolBtn {'));
  });
});
