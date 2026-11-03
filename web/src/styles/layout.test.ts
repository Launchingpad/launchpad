import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

// jsdom has no layout, so the real overflow check is `npm run check:overflow` (a browser audit at
// 375 and 320 px). This test guards the CSS rules that caused the clipping bug in a grid
// with an implicit `auto` column grows to its widest child (a code block, a branch name) and the
// page then clips instead of wrapping.
const css = readFileSync(resolve(process.cwd(), 'src/features/issues/issues.module.css'), 'utf8');
const rule = (sel: string) => {
  const m = css.match(new RegExp(`(?:^|\\n|,\\s*)${sel.replace('.', '\\.')}\\s*(?:,[^{]*)?\\{([^}]*)\\}`));
  return m?.[1] ?? '';
};

describe('detail layout cannot grow past the viewport', () => {
  for (const sel of ['.detailHead', '.detailMain', '.block']) {
    it(`${sel} has a shrinkable single column`, () => {
      expect(rule(sel)).toMatch(/grid-template-columns:\s*minmax\(0,\s*1fr\)/);
    });
  }
  it('.props and .branch can shrink and wrap', () => {
    expect(rule('.props')).toMatch(/min-width:\s*0/);
    expect(rule('.branch')).toMatch(/min-width:\s*0/);
    expect(rule('.branch')).toMatch(/overflow-wrap:\s*anywhere/);
  });
  it('phone layout puts the title block before the properties, and the body last', () => {
    const phone = css.slice(css.indexOf('@media (max-width: 900px)'));
    expect(phone).toMatch(/'head'\s*'props'\s*'main'/);
  });
});

describe('issue list on a phone (LAUN-16)', () => {
  const start = css.indexOf('@media (max-width: 760px)');
  const block = (from: number) => {
    let depth = 0;
    for (let i = css.indexOf('{', from); i < css.length; i++) {
      if (css[i] === '{') depth++;
      if (css[i] === '}' && --depth === 0) return css.slice(from, i + 1);
    }
    return css.slice(from);
  };
  const phone = block(start);
  it('folds the filters, lets the title wrap to two lines, and hides checkboxes outside select mode', () => {
    expect(css).toMatch(/\.fold\s*\{[^}]*display:\s*contents/);
    expect(phone).toMatch(/\.filterOpen \.fold\s*\{[^}]*display:\s*flex/);
    expect(phone).toMatch(/-webkit-line-clamp:\s*2/);
    expect(phone).toMatch(/\.row \.check\s*\{[^}]*display:\s*none/);
    expect(phone).toMatch(/\.selecting \.row \.check\s*\{[^}]*display:\s*inline-block/);
  });
  it('base display:none rules beat .btn by specificity, not by file order', () => {
    expect(css).toMatch(/\.filterBar \.filterToggle,[^{]*\{[^}]*display:\s*none/);
    expect(css).toMatch(/\.card \.cardDate\s*(,|\{)/);
    expect(phone).toMatch(/\.filterBar \.filterToggle\s*\{[^}]*display:\s*inline-flex/);
  });
  it('select mode keeps the count visible on a phone', () => {
    expect(phone).toMatch(/\.selecting \.pageHead\s*\{[^}]*display:\s*flex/);
  });
  it('shows the card date only on a phone', () => {
    expect(css.slice(0, start)).toMatch(/\.cardDate\s*\{[^}]*display:\s*none/);
    expect(phone).toMatch(/\.cardDate\s*\{[^}]*display:\s*block/);
  });
});

// LAUN-64: the sidebar's product groups. jsdom has no layout, so this reads the CSS that gives the targets.
describe('sidebar groups on a phone or a touch screen (LAUN-64)', () => {
  const shell = readFileSync(resolve(process.cwd(), 'src/features/shell/shell.module.css'), 'utf8');
  const query = '@media (max-width: 760px), (pointer: coarse)';
  const at = shell.indexOf(query);
  // The block of the query, up to its closing brace (the rules inside have braces of their own).
  const touch = (() => {
    let depth = 0;
    for (let i = shell.indexOf('{', at); i < shell.length; i++) {
      if (shell[i] === '{') depth++;
      if (shell[i] === '}' && --depth === 0) return shell.slice(at, i + 1);
    }
    return '';
  })();
  const px = (sel: string, prop: string) => {
    const block = new RegExp(`${sel.replace(/[.]/g, '\\.')}\\s*\\{([^}]*)\\}`).exec(touch)?.[1] ?? '';
    return Number(new RegExp(`(?:^|[\\s;])${prop}:\\s*(\\d+)px`).exec(block)?.[1]);
  };
  it('uses the same query as the other pages', () => {
    expect(at).toBeGreaterThan(-1);
  });
  it('makes the group header, the team rows and the Cycles links at least 44px high', () => {
    // `.groups .groupHead, .groups .link {` is one rule for both; the regex finds the block after the list.
    expect(px('.groups .link', 'min-height')).toBeGreaterThanOrEqual(44);
    expect(touch).toMatch(/\.groups \.groupHead,\s*\.groups \.link\s*\{/);
  });
  it('makes the team menu button 44px wide and high, and leaves room for it in the row', () => {
    expect(px('.groups .teamMoreBtn', 'width')).toBeGreaterThanOrEqual(44);
    expect(px('.groups .teamMoreBtn', 'height')).toBeGreaterThanOrEqual(44);
    expect(px('.groups .teamRowMenu > a', 'padding-right')).toBeGreaterThanOrEqual(44);
  });
  it('is a real button with no underline on hover', () => {
    const head = /\.groupHead:hover\s*\{([^}]*)\}/.exec(shell)?.[1] ?? '';
    expect(head).toMatch(/text-decoration:\s*none/);
  });
});
