import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';

// jsdom has no layout and no media queries: this guards the source of the 40px touch targets (phone and coarse
// pointer) of the Products page, and that the page and EVERY dialog carry the class that holds them.
const dir = resolve(process.cwd(), 'src/features/settings');
const css = readFileSync(resolve(dir, 'products.module.css'), 'utf8');
const query = '@media (max-width: 760px), (pointer: coarse)';
const touch = css.slice(css.indexOf(query));

function minHeight(selector: string): number {
  const esc = selector.replace(/[.[\]()>:'=*]/g, '\\$&');
  const m = new RegExp(`${esc}[^{]*\\{([^}]*)\\}`).exec(touch);
  return Number(/min-height:\s*(\d+)px/.exec(m?.[1] ?? '')?.[1]);
}

describe('products page touch targets', () => {
  it('uses the phone and coarse-pointer query', () => {
    expect(css).toContain(query);
  });
  it.each([
    '.touch button',
    '.touch select',
    ".touch input[type='text']",
    '.touch input:not([type])',
    ".touch label:has(> input[type='checkbox'])",
  ])('%s has min-height of at least 40px', (sel) => {
    expect(minHeight(sel)).toBeGreaterThanOrEqual(40);
  });
  it('the page and every <Dialog> of the page carry the touch class', () => {
    const dialogs = readFileSync(resolve(dir, 'ProductDialogs.tsx'), 'utf8');
    const page = readFileSync(resolve(dir, 'ProductsPage.tsx'), 'utf8');
    const count = (dialogs.match(/<Dialog\b/g) ?? []).length;
    expect(count).toBe(3);
    expect((dialogs.match(/<Dialog\s+className=\{touchClass\}/g) ?? []).length).toBe(count);
    expect(page).toMatch(/<div className=\{touchClass\}>/);
  });
});
