import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

// jsdom has no layout and no media queries: this guards the CSS text of the product page (LAUN-65).
const dir = resolve(process.cwd(), 'src/features/issues');
const css = readFileSync(resolve(dir, 'product.module.css'), 'utf8');
const shell = readFileSync(resolve(process.cwd(), 'src/features/shell/shell.module.css'), 'utf8');
const query = '@media (max-width: 760px), (pointer: coarse)';
const touch = css.slice(css.indexOf(query), css.indexOf('@media (max-width: 760px) {'));
const rule = (src: string, sel: string) => {
  const esc = sel.replace(/[.[\]()>:'=*]/g, '\\$&');
  return new RegExp(`${esc}\\s*(?:,[^{]*)?\\{([^}]*)\\}`).exec(src)?.[1] ?? '';
};

describe('product page layout', () => {
  it('uses the phone and coarse-pointer query', () => {
    expect(css).toContain(query);
  });
  it.each(['.page .team', '.page .headBtn'])(
    '%s is at least 40px high on a phone or a touch screen',
    (sel) => {
      expect(touch).toContain(sel);
      expect(Number(/min-height:\s*(\d+)px/.exec(rule(touch, sel))?.[1])).toBeGreaterThanOrEqual(40);
    },
  );
  it('the head wraps, so the phone has no horizontal page scroll', () => {
    expect(rule(css, '.titleRow')).toMatch(/flex-wrap:\s*wrap/);
    expect(rule(css, '.teams')).toMatch(/flex-wrap:\s*wrap/);
    expect(rule(css, '.head')).toMatch(/min-width:\s*0/);
    expect(rule(css, '.title')).toMatch(/overflow-wrap:\s*anywhere/);
  });
  it('has no underline on hover and uses tokens for the banner (dark mode follows the tokens)', () => {
    expect(rule(css, '.team:hover')).toMatch(/text-decoration:\s*none/);
    expect(rule(css, '.archived')).toMatch(/var\(--warn-bg\)/);
    expect(rule(css, '.archived')).toMatch(/var\(--warn-text\)/);
  });
  it('the sidebar product link is 44px wide and high on a phone, and has no hover underline', () => {
    expect(shell).toMatch(/\.groups \.groupOpen\s*\{[^}]*width:\s*44px/);
    expect(rule(shell, '.groupOpen:hover')).toMatch(/text-decoration:\s*none/);
  });
});
