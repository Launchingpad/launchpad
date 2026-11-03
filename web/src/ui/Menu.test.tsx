import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { Menu } from './Menu';

// jsdom has no layout: this checks the markup and the CSS rules that keep a menu row's name readable (LAUN-41).
const css = readFileSync(resolve(process.cwd(), 'src/styles/ui.module.css'), 'utf8');

describe('Menu row', () => {
  it('gives the name a title with the full text, and the date and key their own classes', () => {
    render(
      <Menu
        label="Switch cycle"
        trigger="x"
        items={[
          { label: 'Cycle 10 with a long name', detail: 'Nov 23 - Dec 6', hint: 'Alt+K', onSelect: () => {} },
        ]}
      />,
    );
    fireEvent.click(screen.getByRole('button', { name: 'Switch cycle' }));
    const row = screen.getByRole('menuitem');
    const name = row.querySelector('span[title]')!;
    expect(name.getAttribute('title')).toBe('Cycle 10 with a long name');
    expect(name.className).toMatch(/menuText/);
    expect(row.querySelector('[class*="menuDetail"]')?.textContent).toBe('Nov 23 - Dec 6');
    expect(row.querySelector('kbd')?.className).toMatch(/menuKey/);
    expect(row.className).toMatch(/menuRow/);
    expect(row.getAttribute('style')).toBeNull();
  });
  it('CSS: the name has a minimum width and the date never shrinks', () => {
    expect(css).toMatch(/\.menuText\s*\{[^}]*min-width:\s*8ch/);
    expect(css).toMatch(/\.menuDetail\s*\{[^}]*flex:\s*none[^}]*white-space:\s*nowrap/);
    expect(css).toMatch(/\.menuRow\s*\{[^}]*background:\s*transparent/);
    expect(css).toMatch(/\.menuKey\s*\{[^}]*flex:\s*none/);
  });
  it('CSS: a focused menu row has a thin inset ring, not the 2px box', () => {
    expect(css).toMatch(
      /\.panel \[role='menuitem'\]:focus-visible\s*\{[^}]*outline:\s*1px[^}]*outline-offset:\s*-1px/,
    );
  });
});
