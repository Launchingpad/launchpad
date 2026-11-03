import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Product, Team } from '../../api/types';
import { FakeEventSource } from '../../test/fakes';
import { baseRoutes, mockApi, team as orb } from '../../test/mockApi';
import { currentLocation, renderApp } from '../../test/render';

const mkTeam = (key: string, over: Partial<Team> = {}): Team => ({
  ...orb,
  id: `t-${key}`,
  key,
  name: `Team ${key}`,
  productIds: [],
  ...over,
});
const mkProduct = (slug: string, over: Partial<Product> = {}): Product => ({
  id: `p-${slug}`,
  slug,
  name: slug[0]!.toUpperCase() + slug.slice(1),
  description: '',
  icon: null,
  color: null,
  version: 1,
  teamKeys: [],
  createdAt: '2026-10-01T00:00:00Z',
  updatedAt: '2026-10-01T00:00:00Z',
  archivedAt: null,
  ...over,
});
const page = <T,>(items: T[]) => ({ items, nextCursor: null });

/** The org the tests read. `data` is mutable, so a test can change it and send an event. */
function serve(data: { teams: Team[]; products: Product[] | 'fail' }) {
  return mockApi({
    ...baseRoutes(),
    'GET /api/v1/orgs/:org/teams': () => ({ json: page(data.teams) }),
    'GET /api/v1/orgs/:org/products': () =>
      data.products === 'fail'
        ? { status: 500, json: { error: 'boom', code: 'internal' } }
        : { json: page(data.products) },
  });
}

const nav = () => screen.getByRole('navigation', { name: 'Primary' });
const rows = (name: string | RegExp) => within(nav()).queryAllByRole('link', { name });
const head = (name: string) => within(nav()).getByRole('button', { name });

afterEach(() => {
  localStorage.clear();
  vi.restoreAllMocks();
});

describe('sidebar without products', () => {
  it.each([
    ['an empty product list', [] as Product[]],
    ['a product list that failed to load', 'fail' as const],
    ['only products with no team to show', [mkProduct('empty')]],
  ])('is the flat Teams list as before (%s)', async (_n, products) => {
    serve({ teams: [mkTeam('ALP'), mkTeam('BET')], products });
    renderApp('/orbit/issues');
    await screen.findByRole('link', { name: /ALP/ });
    // Same shape as before products: each team row sits straight under the nav, with no group around it.
    for (const key of ['ALP', 'BET']) {
      const link = within(nav()).getByRole('link', { name: new RegExp(key) });
      expect(link.parentElement?.parentElement).toBe(nav());
    }
    expect(within(nav()).queryByRole('group')).toBeNull();
    expect(within(nav()).queryByText('Other teams')).toBeNull();
    // The only buttons are the team menus that an admin has: no group header, nothing that opens or closes a group.
    for (const b of within(nav()).queryAllByRole('button')) {
      expect(b).not.toHaveAttribute('aria-controls');
      expect(b).toHaveAccessibleName(/team options$/);
    }
  });
});

describe('sidebar groups', () => {
  it('shows a product group and Other teams, in that order', async () => {
    serve({
      teams: [mkTeam('ALP', { productIds: ['p-alpha'] }), mkTeam('ZED')],
      products: [mkProduct('alpha')],
    });
    renderApp('/orbit/issues');
    const alpha = await screen.findByRole('button', { name: 'Alpha' });
    const other = head('Other teams');
    expect(alpha).toHaveAttribute('aria-expanded', 'true');
    expect(alpha.compareDocumentPosition(other) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(
      within(screen.getByRole('group', { name: 'Alpha' })).getByRole('link', { name: /ALP/ }),
    ).toBeVisible();
    expect(
      within(screen.getByRole('group', { name: 'Other teams' })).getByRole('link', { name: /ZED/ }),
    ).toBeVisible();
  });

  it('leaves out an archived product and a team that is in no live product goes to Other teams', async () => {
    serve({
      teams: [mkTeam('ALP', { productIds: ['p-old', 'p-alpha'] }), mkTeam('OLD', { productIds: ['p-old'] })],
      products: [mkProduct('alpha'), mkProduct('old', { archivedAt: '2026-10-02T00:00:00Z' })],
    });
    renderApp('/orbit/issues');
    await screen.findByRole('button', { name: 'Alpha' });
    expect(within(nav()).queryByRole('button', { name: 'Old' })).toBeNull();
    expect(
      within(screen.getByRole('group', { name: 'Other teams' })).getByRole('link', { name: /OLD/ }),
    ).toBeVisible();
  });

  it('shows a team of two products as two rows, and both light up', async () => {
    serve({
      teams: [mkTeam('ALP', { productIds: ['p-alpha', 'p-beta'], cyclesEnabled: true })],
      products: [mkProduct('alpha'), mkProduct('beta')],
    });
    renderApp('/orbit/team/ALP');
    await screen.findByRole('button', { name: 'Beta' });
    const both = rows(/ALP/);
    expect(both).toHaveLength(2);
    for (const l of both) expect(l).toHaveAttribute('aria-current', 'page');
    // The Cycles link and the team menu are under each row, each named by its group.
    expect(within(nav()).getAllByRole('link', { name: 'Cycles' })).toHaveLength(2);
    expect(within(nav()).getByRole('group', { name: 'ALP cycles in Alpha' })).toBeInTheDocument();
    expect(within(nav()).getByRole('group', { name: 'ALP cycles in Beta' })).toBeInTheDocument();
    expect(within(nav()).getByRole('button', { name: 'Team ALP team options in Alpha' })).toBeInTheDocument();
    expect(within(nav()).getByRole('button', { name: 'Team ALP team options in Beta' })).toBeInTheDocument();
  });

  it('shows no Cycles link for a team with cycles off', async () => {
    serve({ teams: [mkTeam('ALP', { productIds: ['p-alpha'] })], products: [mkProduct('alpha')] });
    renderApp('/orbit/issues');
    await screen.findByRole('button', { name: 'Alpha' });
    expect(within(nav()).queryByRole('link', { name: 'Cycles' })).toBeNull();
  });

  it('shows the product icon and colour as decoration, and ignores a colour that is not #rrggbb', async () => {
    serve({
      teams: [mkTeam('ALP', { productIds: ['p-alpha', 'p-beta'] })],
      products: [
        mkProduct('alpha', { icon: 'A', color: '#336699' }),
        mkProduct('beta', { color: 'red; background:url(x)' }),
      ],
    });
    renderApp('/orbit/issues');
    const a = await screen.findByRole('button', { name: 'Alpha' });
    expect(a.querySelector('[style]')).toHaveStyle({ background: '#336699' });
    expect(head('Beta').querySelector('[style]')).toBeNull();
    expect(a.querySelector('[aria-hidden="true"]')).not.toBeNull();
  });
});

describe('collapse', () => {
  const two = () =>
    serve({
      teams: [mkTeam('ALP', { productIds: ['p-alpha'] }), mkTeam('BET', { productIds: ['p-beta'] })],
      products: [mkProduct('alpha'), mkProduct('beta')],
    });

  it('is a button that Enter and Space toggle, and a closed group is not in the page at all', async () => {
    two();
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    const alpha = await screen.findByRole('button', { name: 'Alpha' });
    expect(alpha).toHaveAttribute('aria-expanded', 'true');
    expect(alpha.tagName).toBe('BUTTON');
    alpha.focus();
    await user.keyboard('{Enter}');
    expect(alpha).toHaveAttribute('aria-expanded', 'false');
    // Not rendered (a CSS-hidden row would still be in the tab order in some browsers).
    expect(rows(/ALP/)).toHaveLength(0);
    expect(alpha).not.toHaveAttribute('aria-controls');
    await user.keyboard(' ');
    expect(alpha).toHaveAttribute('aria-expanded', 'true');
    expect(rows(/ALP/)).toHaveLength(1);
    expect(alpha).toHaveAttribute('aria-controls');
  });

  it('keeps Tab off the rows of a closed group', async () => {
    two();
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    const alpha = await screen.findByRole('button', { name: 'Alpha' });
    await user.click(alpha);
    alpha.focus();
    await user.tab();
    // LAUN-65: the product link is the next stop; after it comes the next group header, never a team of the closed one.
    expect(document.activeElement).toBe(within(nav()).getByRole('link', { name: 'Open product Alpha' }));
    await user.tab();
    expect(document.activeElement).toBe(head('Beta').closest('button'));
    await user.tab();
    expect(document.activeElement).toBe(within(nav()).getByRole('link', { name: 'Open product Beta' }));
    await user.tab();
    expect(document.activeElement).toBe(within(nav()).getByRole('link', { name: /BET/ }));
  });

  it('keeps the choice in localStorage under the product id', async () => {
    two();
    const user = userEvent.setup();
    const first = renderApp('/orbit/issues');
    await user.click(await screen.findByRole('button', { name: 'Alpha' }));
    expect(localStorage.getItem('devops-nav-group:p-alpha')).toBe('0');
    expect(localStorage.getItem('devops-nav-group:p-beta')).toBeNull();
    first.unmount();
    renderApp('/orbit/issues');
    expect(await screen.findByRole('button', { name: 'Alpha' })).toHaveAttribute('aria-expanded', 'false');
    expect(head('Beta')).toHaveAttribute('aria-expanded', 'true');
  });

  it('works when storage throws', async () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    two();
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    const alpha = await screen.findByRole('button', { name: 'Alpha' });
    expect(alpha).toHaveAttribute('aria-expanded', 'true');
    await user.click(alpha);
    expect(alpha).toHaveAttribute('aria-expanded', 'false');
    await user.click(alpha);
    expect(alpha).toHaveAttribute('aria-expanded', 'true');
  });

  it('opens a closed group that holds the team of the route, and writes nothing to storage', async () => {
    localStorage.setItem('devops-nav-group:p-beta', '0');
    two();
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    const beta = await screen.findByRole('button', { name: 'Beta' });
    expect(beta).toHaveAttribute('aria-expanded', 'false');
    // Go to a team of Alpha: Beta stays closed. Then go to Beta's team by the palette.
    await user.click(within(nav()).getByRole('link', { name: /ALP/ }));
    expect(beta).toHaveAttribute('aria-expanded', 'false');
    await user.click(screen.getAllByRole('button', { name: /Search/ })[0]!);
    await user.type(await screen.findByRole('combobox'), 'Team BET');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/team/BET'));
    await waitFor(() => expect(head('Beta')).toHaveAttribute('aria-expanded', 'true'));
    expect(localStorage.getItem('devops-nav-group:p-beta')).toBe('0'); // the route never writes the choice
  });

  it('opens it at once when the page loads on that team, and the person can still close it', async () => {
    localStorage.setItem('devops-nav-group:p-alpha', '0');
    two();
    const user = userEvent.setup();
    renderApp('/orbit/team/ALP');
    const alpha = await screen.findByRole('button', { name: 'Alpha' });
    await waitFor(() => expect(alpha).toHaveAttribute('aria-expanded', 'true'));
    await user.click(alpha);
    expect(alpha).toHaveAttribute('aria-expanded', 'false');
    expect(localStorage.getItem('devops-nav-group:p-alpha')).toBe('0');
  });

  it('starts every group closed when the org has more than 12 team rows, except the one with the active team', async () => {
    const mk = (prefix: string, n: number, product: string) =>
      Array.from({ length: n }, (_, i) => mkTeam(`${prefix}${i}`, { productIds: [product] }));
    const teams = [...mk('A', 5, 'p-alpha'), ...mk('B', 5, 'p-beta'), ...mk('C', 3, 'p-gamma')]; // 13 rows
    serve({ teams, products: [mkProduct('alpha'), mkProduct('beta'), mkProduct('gamma')] });
    renderApp('/orbit/team/B1');
    await screen.findByRole('button', { name: 'Beta' });
    expect(head('Alpha')).toHaveAttribute('aria-expanded', 'false');
    expect(head('Gamma')).toHaveAttribute('aria-expanded', 'false');
    expect(head('Beta')).toHaveAttribute('aria-expanded', 'true');
    expect(rows(/Team B1/)).toHaveLength(1);
  });

  it('starts all groups closed past 12 rows when the route is not a team (Other teams too)', async () => {
    const teams = [
      ...Array.from({ length: 11 }, (_, i) => mkTeam(`A${i}`, { productIds: ['p-alpha'] })),
      mkTeam('X1'),
      mkTeam('X2'),
    ];
    serve({ teams, products: [mkProduct('alpha')] });
    renderApp('/orbit/issues');
    await screen.findByRole('button', { name: 'Alpha' });
    expect(head('Alpha')).toHaveAttribute('aria-expanded', 'false');
    expect(head('Other teams')).toHaveAttribute('aria-expanded', 'false');
  });

  it('opens a stored choice over the 12-row rule, and counts a team of two products twice', async () => {
    // 6 teams in alpha, the same 6 in beta: 12 rows, so all open. One more row makes 13.
    const six = Array.from({ length: 6 }, (_, i) => mkTeam(`S${i}`, { productIds: ['p-alpha', 'p-beta'] }));
    const data = { teams: six, products: [mkProduct('alpha'), mkProduct('beta')] as Product[] };
    serve(data);
    const first = renderApp('/orbit/issues');
    await screen.findByRole('button', { name: 'Alpha' });
    expect(head('Alpha')).toHaveAttribute('aria-expanded', 'true');
    first.unmount();
    data.teams = [...six, mkTeam('Z1')]; // 13 rows with Other teams
    localStorage.setItem('devops-nav-group:p-beta', '1');
    renderApp('/orbit/issues');
    await screen.findByRole('button', { name: 'Other teams' });
    expect(head('Alpha')).toHaveAttribute('aria-expanded', 'false');
    expect(head('Beta')).toHaveAttribute('aria-expanded', 'true');
  });
});

describe('events', () => {
  it('assigning teams to a product regroups the sidebar when the event arrives', async () => {
    const data = { teams: [mkTeam('ALP'), mkTeam('BET')], products: [] as Product[] };
    serve(data);
    renderApp('/orbit/issues');
    await screen.findByRole('link', { name: /ALP/ });
    expect(within(nav()).queryByRole('button', { name: 'Alpha' })).toBeNull();

    // An admin makes a product and puts ALP in it (in another tab); the server tells us.
    data.products = [mkProduct('alpha', { teamKeys: ['ALP'] })];
    data.teams = [mkTeam('ALP', { productIds: ['p-alpha'] }), mkTeam('BET')];
    await waitFor(() => expect(FakeEventSource.lastEvents).toBeDefined());
    act(() => {
      FakeEventSource.lastEvents!.emit('product.teams_changed', {
        seq: 1,
        type: 'product.teams_changed',
        entityType: 'product',
        entityId: 'p-alpha',
        actor: null,
        payload: {},
        createdAt: '2026-10-27T00:00:00Z',
      });
    });
    expect(await screen.findByRole('button', { name: 'Alpha' })).toBeInTheDocument();
    expect(
      within(screen.getByRole('group', { name: 'Alpha' })).getByRole('link', { name: /ALP/ }),
    ).toBeVisible();
    expect(
      within(screen.getByRole('group', { name: 'Other teams' })).getByRole('link', { name: /BET/ }),
    ).toBeVisible();

    // And a later change takes a team out again.
    data.teams = [mkTeam('ALP'), mkTeam('BET')];
    data.products = [];
    act(() => {
      FakeEventSource.lastEvents!.emit('product.deleted', {
        seq: 2,
        type: 'product.deleted',
        entityType: 'product',
        entityId: 'p-alpha',
        actor: null,
        payload: {},
        createdAt: '2026-10-27T00:00:01Z',
      });
    });
    await waitFor(() => expect(within(nav()).queryByRole('button', { name: 'Alpha' })).toBeNull());
    expect(within(nav()).getByRole('link', { name: /BET/ })).toBeVisible();
  });
});

describe('Product filter chip', () => {
  const data = () => ({
    teams: [mkTeam('ALP', { productIds: ['p-alpha'] })],
    products: [
      mkProduct('alpha', { teamKeys: ['ALP'] }),
      mkProduct('beta', { teamKeys: ['ALP', 'BET', 'GAM', 'DEL', 'EPS'] }),
    ],
  });

  it('is left out while the org has no product', async () => {
    serve({ teams: [mkTeam('ALP')], products: [] });
    renderApp('/orbit/issues');
    await screen.findByRole('button', { name: 'Priority' });
    expect(screen.queryByRole('button', { name: /^Product/ })).toBeNull();
  });

  it('is left out on a team page, where the team is fixed', async () => {
    serve(data());
    renderApp('/orbit/team/ALP');
    await screen.findByRole('button', { name: 'Priority' });
    expect(screen.queryByRole('button', { name: /^Product/ })).toBeNull();
  });

  it('reads the URL, asks the server for that product, and writes the URL back on a change', async () => {
    const api = serve(data());
    const user = userEvent.setup();
    renderApp('/orbit/issues?product=alpha');
    const chip = await screen.findByRole('button', { name: 'Product: Alpha (ALP)' });
    await waitFor(() =>
      expect(
        api.calls.some(
          (c) => c.url.pathname.endsWith('/issues') && c.url.searchParams.get('product') === 'alpha',
        ),
      ).toBe(true),
    );

    await user.click(chip);
    await user.click(await screen.findByRole('option', { name: /^Beta/ }));
    await waitFor(() => expect(currentLocation.search).toContain('product=beta'));
    await waitFor(() =>
      expect(
        api.calls.some(
          (c) => c.url.pathname.endsWith('/issues') && c.url.searchParams.get('product') === 'beta',
        ),
      ).toBe(true),
    );

    // Choosing the same product again clears it, and Clear (n) counts it.
    await user.click(await screen.findByRole('button', { name: 'Product: Beta (ALP, BET +3)' }));
    await user.click(await screen.findByRole('option', { name: /^Beta/ }));
    await waitFor(() => expect(currentLocation.search).not.toContain('product'));
  });

  it('shows the teams in the picker rows and the chip; the tooltips and names hold every key (LAUN-100)', async () => {
    serve(data());
    const user = userEvent.setup();
    renderApp('/orbit/issues?product=beta');
    const chip = await screen.findByRole('button', { name: 'Product: Beta (ALP, BET +3)' });
    expect(chip).toHaveAttribute('title', 'Beta: teams ALP, BET, GAM, DEL, EPS');
    await user.click(chip);
    const row = await screen.findByRole('option', { name: 'Beta: teams ALP, BET, GAM, DEL, EPS' });
    expect(row).toHaveTextContent('ALP, BET, GAM +2');
    expect(row).toHaveAttribute('title', 'Beta: teams ALP, BET, GAM, DEL, EPS');
    const one = screen.getByRole('option', { name: 'Alpha: team ALP' });
    expect(one).toHaveTextContent('ALP');
  });

  it('Clear removes the product, and a slug the list does not know is still shown so it can be cleared', async () => {
    serve(data());
    const user = userEvent.setup();
    renderApp('/orbit/issues?product=gone');
    await screen.findByRole('button', { name: 'Product: gone' });
    await user.click(screen.getByRole('button', { name: /Clear \(1\)/ }));
    await waitFor(() => expect(currentLocation.search).not.toContain('product'));
  });
});

describe('palette', () => {
  it('has a Go to product entry that opens the product page', async () => {
    serve({
      teams: [mkTeam('ALP', { productIds: ['p-alpha'] })],
      products: [mkProduct('alpha', { name: 'Launchpad', teamKeys: ['ALP', 'BET', 'GAM', 'DEL'] })],
    });
    const user = userEvent.setup();
    renderApp('/orbit/my-issues');
    await screen.findByRole('button', { name: 'Launchpad' });
    await user.click(screen.getAllByRole('button', { name: /Search/ })[0]!);
    await user.type(await screen.findByRole('combobox'), 'go to product');
    const opt = await screen.findByRole('option', { name: 'Launchpad: teams ALP, BET, GAM, DEL' });
    expect(opt).toHaveTextContent('Launchpad (ALP, BET, GAM +1)');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/product/alpha'));
    expect(currentLocation.search).toBe('');
  });
});

describe('product group header (LAUN-100)', () => {
  it('holds the teams of the product in its tooltip; Other teams has none', async () => {
    serve({
      teams: [
        mkTeam('ALP', { productIds: ['p-alpha'] }),
        mkTeam('BET', { productIds: ['p-alpha'] }),
        mkTeam('ZED'),
      ],
      products: [mkProduct('alpha', { teamKeys: ['ALP', 'BET'] })],
    });
    renderApp('/orbit/issues');
    expect(await screen.findByRole('button', { name: 'Alpha' })).toHaveAttribute(
      'title',
      'Alpha: teams ALP, BET',
    );
    expect(screen.getByRole('button', { name: 'Other teams' })).not.toHaveAttribute('title');
  });
});

describe('product header link (LAUN-65)', () => {
  it('opens the product page; it is a link beside the toggle button, never inside it, and Other teams has none', async () => {
    serve({
      teams: [mkTeam('ALP', { productIds: ['p-alpha'] }), mkTeam('ZED')],
      products: [mkProduct('alpha')],
    });
    renderApp('/orbit/issues');
    const toggle = await screen.findByRole('button', { name: 'Alpha' });
    const link = within(nav()).getByRole('link', { name: 'Open product Alpha' });
    expect(link).toHaveAttribute('href', '/orbit/product/alpha');
    expect(toggle.contains(link)).toBe(false);
    expect(within(nav()).queryByRole('link', { name: /Open product Other/ })).toBeNull();
    // The toggle still toggles and does not navigate.
    await userEvent.setup().click(toggle);
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    expect(currentLocation.pathname).toBe('/orbit/issues');
  });
});

describe('org logos in the sidebar (LAUN-22)', () => {
  it('shows the current org logo, and the logos of every org in the switcher', async () => {
    mockApi({
      ...baseRoutes(),
      'GET /api/v1/auth/session': () => ({
        json: {
          user: { id: 'u1', username: 'chen', name: 'Chen', email: 'c@e.com', isPlatformAdmin: false },
          orgs: [
            { id: 'o1', slug: 'orbit', name: 'Orbit', role: 'admin' },
            { id: 'o2', slug: 'acme', name: 'Acme', role: 'member' },
          ],
          csrfToken: 'csrf-test',
        },
      }),
      'GET /api/v1/orgs/:org': () => ({
        json: {
          id: 'o1',
          slug: 'orbit',
          name: 'Orbit',
          role: 'admin',
          createdAt: '2026-01-01T00:00:00Z',
          timeZone: 'UTC',
          defaultTeam: null,
          avatarUrl: 'https://api.example.com/api/v1/orgs/orbit/avatar?v=2',
        },
      }),
      'GET /api/v1/orgs': () => ({
        json: page([
          {
            id: 'o1',
            slug: 'orbit',
            name: 'Orbit',
            role: 'admin',
            avatarUrl: '/api/v1/orgs/orbit/avatar?v=2',
          },
          { id: 'o2', slug: 'acme', name: 'Acme', role: 'member', avatarUrl: null },
        ]),
      }),
    });
    const user = userEvent.setup();
    const { container } = renderApp('/orbit/issues');
    const btn = await screen.findByRole('button', { name: 'Switch organisation' });
    await waitFor(() =>
      expect(btn.querySelector('img')).toHaveAttribute('src', '/api/v1/orgs/orbit/avatar?v=2'),
    );
    expect(container.querySelector('aside img')).toHaveAttribute('alt', '');
    await user.click(btn);
    const acme = await screen.findByRole('menuitem', { name: /Acme/ });
    expect(acme).toHaveTextContent('A'); // no logo: the letter
  });
});
