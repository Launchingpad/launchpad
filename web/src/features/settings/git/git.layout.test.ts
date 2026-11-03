import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

// jsdom has no layout: this guards the touch targets of the git pages (40px on a phone and a coarse pointer).
const css = readFileSync(resolve(process.cwd(), 'src/features/settings/git/git.module.css'), 'utf8');
const query = '@media (max-width: 760px), (pointer: coarse)';
const touch = css.slice(css.indexOf(query));

function minHeight(selector: string): number {
  const esc = selector.replace(/[.[\]()>:'=*]/g, '\\$&');
  const m = new RegExp(`${esc}[^{]*\\{([^}]*)\\}`).exec(touch);
  const h = /min-height:\s*(\d+)px/.exec(m?.[1] ?? '');
  return Number(h?.[1]);
}

describe('git pages touch targets', () => {
  it('uses the phone and coarse-pointer query', () => {
    expect(css).toContain(query);
  });
  it.each([
    '.item',
    '.pickRow',
    '.touch button',
    '.touch select',
    ".touch input[type='text']",
    ".touch input[type='search']",
    ".touch input[type='password']",
    '.touch input:not([type])',
    ".touch label:has(> input[type='radio'])",
    ".touch label:has(> input[type='checkbox'])",
    '.touch .back',
  ])('%s has min-height of at least 40px', (sel) => {
    expect(minHeight(sel)).toBeGreaterThanOrEqual(40);
  });
});
