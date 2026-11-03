import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

// LAUN-95: jsdom has no layout, so these read the CSS that gives the touch targets and the tokens.
const css = readFileSync(resolve(process.cwd(), 'src/features/issues/detail.module.css'), 'utf8');
const git = readFileSync(resolve(process.cwd(), 'src/features/settings/git/git.module.css'), 'utf8');
const query = '@media (max-width: 760px), (pointer: coarse)';

describe('the Changes block (LAUN-95)', () => {
  it('has 40px touch targets on a phone and a coarse pointer', () => {
    const block = css.slice(css.lastIndexOf(query));
    expect(block).toMatch(/\.crTitle\s*\{[^}]*min-height:\s*40px/);
  });
  it('the title link does not underline on hover', () => {
    expect(css).toMatch(/a\.crTitle:hover\s*\{[^}]*text-decoration:\s*none/);
  });
  it('colours come from tokens only, so dark mode follows', () => {
    const own = css.slice(css.indexOf('The Changes block (LAUN-95)'));
    expect(own).not.toMatch(/#[0-9a-fA-F]{3,8}\b/);
    expect(own).not.toMatch(/rgba?\(/);
  });
  it('lets long host text wrap instead of overflowing a phone', () => {
    expect(css).toMatch(/\.crTitle\s*\{[^}]*overflow-wrap:\s*anywhere/);
    expect(css).toMatch(/\.crBody\s*\{[^}]*min-width:\s*0/);
  });
});

describe('repository buttons (LAUN-95)', () => {
  it('a waiting button looks waiting but keeps focus', () => {
    expect(git).toMatch(/\.slot button\[aria-disabled='true'\]\s*\{[^}]*opacity/);
  });
});
