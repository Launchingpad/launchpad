import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import { Breadcrumbs, type Crumb } from './Breadcrumbs';

const css = readFileSync(resolve(process.cwd(), 'src/ui/breadcrumbs.module.css'), 'utf8');
const detailCss = readFileSync(resolve(process.cwd(), 'src/features/issues/detail.module.css'), 'utf8');

/** Declarations of the rule whose selector is exactly `selector` (no media query: the first one found). */
function decls(source: string, selector: string, from = 0): Record<string, string> {
  const re = /([^{}]+)\{([^{}]*)\}/g;
  re.lastIndex = from;
  for (let m = re.exec(source); m; m = re.exec(source)) {
    const sel = m[1]!.replace(/\/\*[\s\S]*?\*\//g, '').trim();
    if (sel === selector) {
      const out: Record<string, string> = {};
      for (const d of m[2]!.split(';')) {
        const i = d.indexOf(':');
        if (i > 0) out[d.slice(0, i).trim()] = d.slice(i + 1).trim();
      }
      return out;
    }
  }
  return {};
}
const phone = css.indexOf('@media (max-width: 520px)');

const three: Crumb[] = [
  { key: 'a', label: 'Orbit', to: '/o/team/ORB' },
  { key: 'b', label: 'Cycles', to: '/o/team/ORB/cycles' },
  {
    key: 'c',
    label: 'A very long cycle name',
    title: 'Full title',
    after: <button type="button">Switch</button>,
  },
];

function show(items: Crumb[] = three) {
  render(
    <MemoryRouter>
      <Breadcrumbs items={items} />
    </MemoryRouter>,
  );
  return screen.getByRole('navigation', { name: 'Breadcrumb' });
}

describe('Breadcrumbs (LAUN-157)', () => {
  it('is a nav with an ordered list; links for ancestors, none for the current item', () => {
    const nav = show();
    expect(nav.querySelector('ol')).not.toBeNull();
    expect(
      within(nav)
        .getAllByRole('link')
        .map((a) => a.getAttribute('href')),
    ).toEqual(['/o/team/ORB', '/o/team/ORB/cycles']);
    expect(within(nav).getAllByRole('listitem')).toHaveLength(3);
  });
  it('aria-current is on the last item only', () => {
    const nav = show();
    const items = within(nav).getAllByRole('listitem');
    expect(items.map((li) => li.getAttribute('aria-current'))).toEqual([null, null, 'page']);
    expect(nav.querySelectorAll('[aria-current]')).toHaveLength(1);
  });
  it('hides every separator from screen readers', () => {
    const nav = show();
    const seps = nav.querySelectorAll('[aria-hidden="true"]');
    expect(seps).toHaveLength(2);
    seps.forEach((s) => expect(s.textContent).toContain('›'));
  });
  it('the current item is an h1 only when heading is set', () => {
    show();
    expect(screen.queryByRole('heading')).toBeNull();
  });
  it('heading: the current item is an h1 and keeps its extra control', () => {
    show([three[0]!, { ...three[2]!, heading: true }]);
    expect(screen.getByRole('heading', { level: 1, name: 'A very long cycle name' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Switch' })).toBeInTheDocument();
  });
  it('a `to` on the last item is ignored: the current page is never a link', () => {
    const nav = show([three[0]!, { key: 'z', label: 'Here', to: '/o/here' }]);
    expect(within(nav).getAllByRole('link')).toHaveLength(1);
    expect(within(nav).queryByRole('link', { name: 'Here' })).toBeNull();
    expect(within(nav).getByText('Here').closest('a')).toBeNull();
  });
  it('extra control shows only on the current item', () => {
    show([{ ...three[0]!, after: <button type="button">Nope</button> }, three[2]!]);
    expect(screen.queryByRole('button', { name: 'Nope' })).toBeNull();
  });
  it('every item has a title: the given one, else the label', () => {
    const nav = show();
    expect(within(nav).getByText('Orbit')).toHaveAttribute('title', 'Orbit');
    expect(within(nav).getByText('Cycles')).toHaveAttribute('title', 'Cycles');
    expect(within(nav).getByText('A very long cycle name')).toHaveAttribute('title', 'Full title');
  });
  it('phone: only items before the nearest ancestor are marked hidden; the "…" sits before the nearest ancestor', () => {
    const nav = show();
    const items = within(nav).getAllByRole('listitem');
    expect(items.map((li) => /hideSmall/.test(li.className))).toEqual([true, false, false]);
    const gaps = Array.from(nav.querySelectorAll('li')).map((li) => li.textContent?.includes('…'));
    expect(gaps).toEqual([false, true, false]);
  });
  it('with two items nothing is hidden and there is no "…"', () => {
    const nav = show([three[0]!, three[2]!]);
    const items = within(nav).getAllByRole('listitem');
    expect(items.map((li) => /hideSmall/.test(li.className))).toEqual([false, false]);
    expect(nav.textContent).not.toContain('…');
    expect(nav.querySelectorAll('[aria-hidden="true"]')).toHaveLength(1);
  });
  it('with four items two are hidden on a phone and one "…" shows', () => {
    const nav = show([{ key: 'z', label: 'Z', to: '/z' }, ...three]);
    const items = within(nav).getAllByRole('listitem');
    expect(items.map((li) => /hideSmall/.test(li.className))).toEqual([true, true, false, false]);
    expect(nav.textContent?.match(/…/g)).toHaveLength(1);
  });
  it('a single item has no separator', () => {
    const nav = show([{ key: 'a', label: 'Only' }]);
    expect(within(nav).getByRole('listitem')).toHaveAttribute('aria-current', 'page');
    expect(nav.querySelector('[aria-hidden="true"]')).toBeNull();
  });
});

describe('breadcrumbs.module.css (LAUN-157)', () => {
  it('clips a label with an ellipsis and lets it shrink', () => {
    const t = decls(css, '.text');
    expect(t['min-width']).toBe('0');
    expect(t['overflow']).toBe('hidden');
    expect(t['text-overflow']).toBe('ellipsis');
    expect(t['white-space']).toBe('nowrap');
  });
  it('an item can shrink and is never wrapped', () => {
    expect(decls(css, '.item')['min-width']).toBe('0');
    expect(decls(css, '.list')['flex-wrap']).toBe('nowrap');
    expect(decls(css, '.list')['min-width']).toBe('0');
  });
  it('the "…" is hidden on desktop and shown only inside the phone query', () => {
    expect(decls(css.slice(0, phone), '.gap')['display']).toBe('none');
    expect(phone).toBeGreaterThan(0);
    expect(decls(css, '.gap', phone)['display']).toBe('inline');
    expect(decls(css, '.item.hideSmall', phone)['display']).toBe('none');
    expect(decls(css.slice(0, phone), '.item.hideSmall')['display']).toBeUndefined();
  });
  it('no item is capped to a percentage of the trail', () => {
    expect(css).not.toMatch(/first-child/);
    expect(css).not.toMatch(/flex:[^;]*%/);
  });
  it('a link has a hover state and nothing removes the focus ring', () => {
    expect(decls(css, '.link:hover')['background']).toBe('var(--surface-hover)');
    expect(css).not.toMatch(/outline:\s*none/);
  });
  it('colours are tokens only, so dark mode follows', () => {
    expect(css).not.toMatch(/#[0-9a-fA-F]{3,8}\b/);
    expect(css).not.toMatch(/rgba?\(/);
  });
});

describe('issue bar layout (LAUN-157)', () => {
  it('the left block has a real basis, so the bar can still wrap the toolbar', () => {
    expect(decls(detailCss, '.barLeft')['flex']).toBe('1 1 160px');
    expect(decls(detailCss, '.bar')['flex-wrap']).toBe('wrap');
  });
  it('the trail keeps a readable minimum width', () => {
    const c = decls(detailCss, '.bar .crumbs');
    expect(c['min-width']).toBe('8ch');
    expect(c['flex']).toBe('1 1 0');
  });
});
