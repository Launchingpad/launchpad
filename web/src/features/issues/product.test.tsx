import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { Issue, Team, WorkflowState } from '../../api/types';
import { at, middle, stubBoardLayout } from '../../test/boardLayout';
import { baseRoutes, makeIssue, mockApi, states as baseStates, team as orb } from '../../test/mockApi';
import { renderApp } from '../../test/render';
import { resetDragged } from './cardLink';

// LAUN-65: the product page. The fake server answers the products and the per-team states; the issues route
// echoes what the page asked for, so the tests can read the `product` parameter.

const mkTeam = (key: string): Team => ({
  ...orb,
  id: `t-${key}`,
  key,
  name: `Team ${key}`,
  productIds: ['p-alpha'],
});
const ALP = mkTeam('ALP');
const BET = mkTeam('BET');
const GAM: Team = { ...mkTeam('GAM'), productIds: [] };
const st = (team: Team, id: string, name: string, type: WorkflowState['type'], position: number) => ({
  ...baseStates[0]!,
  id: `${team.key}-${id}`,
  name,
  type,
  position,
  teamId: team.id,
});
const stateSets: Record<string, WorkflowState[]> = {
  ALP: [
    st(ALP, 'b', 'Backlog', 'backlog', 0),
    st(ALP, 't', 'Todo', 'unstarted', 1),
    st(ALP, 'r', 'Review', 'started', 2),
  ],
  BET: [st(BET, 'b', 'Backlog', 'backlog', 0), st(BET, 't', 'Todo', 'unstarted', 1)],
  GAM: [st(GAM, 'b', 'Backlog', 'backlog', 0), st(GAM, 'x', 'Gamma only', 'started', 1)],
};
const card = (n: number, team: Team, state: string, title: string): Issue => {
  const s = stateSets[team.key]!.find((x) => x.name === state)!;
  return makeIssue(n, {
    title,
    identifier: `${team.key}-${n}`,
    team: { id: team.id, key: team.key, name: team.name },
    state: { id: s.id, name: s.name, type: s.type, color: s.color },
  });
};
const product = (over: Record<string, unknown> = {}, teams: Team[] = [ALP, BET]) => ({
  id: 'p-alpha',
  slug: 'alpha',
  name: 'Alpha',
  description: 'The alpha product',
  icon: 'A',
  color: '#336699',
  version: 1,
  teamKeys: teams.map((t) => t.key),
  createdAt: '2026-10-01T00:00:00Z',
  updatedAt: '2026-10-01T00:00:00Z',
  archivedAt: null,
  teams,
  ...over,
});
const page = <T,>(items: T[]) => ({ items, nextCursor: null });

function serve(
  opts: { product?: ReturnType<typeof product> | 'missing'; issues?: Issue[]; gate?: Promise<void> } = {},
) {
  const issues = opts.issues ?? [];
  return mockApi({
    ...baseRoutes(),
    'GET /api/v1/orgs/:org/teams': () => ({ json: page([ALP, BET, GAM]) }),
    'GET /api/v1/orgs/:org/teams/:team/states': async (r) => {
      await opts.gate;
      return { json: page(stateSets[r.params.team!] ?? []) };
    },
    'GET /api/v1/orgs/:org/products': () => ({ json: page([product()]) }),
    'GET /api/v1/orgs/:org/products/:product': (r) =>
      opts.product === 'missing' || r.params.product !== 'alpha'
        ? { status: 404, json: { error: 'product not found', code: 'not_found' } }
        : { json: opts.product ?? product() },
    'GET /api/v1/orgs/:org/issues': () => ({ json: page(issues) }),
    'POST /api/v1/orgs/:org/issues': () => ({ status: 201, json: makeIssue(90, { identifier: 'BET-90' }) }),
  });
}
const issueGets = (api: ReturnType<typeof mockApi>) =>
  api.calls.filter((c) => c.method === 'GET' && c.url.pathname.endsWith('/issues'));

beforeEach(() => {
  document.cookie = 'odt_csrf=csrf-test; path=/';
});
afterEach(() => localStorage.clear());

describe('product page', () => {
  it('shows the header, the teams strip and the issues of the product (the product parameter, no chip)', async () => {
    const api = serve({ issues: [card(1, ALP, 'Todo', 'Alpha task'), card(2, BET, 'Todo', 'Beta task')] });
    renderApp('/orbit/product/alpha');
    expect(await screen.findByRole('heading', { level: 1, name: 'Alpha' })).toBeInTheDocument();
    expect(await screen.findByText('Alpha task')).toBeInTheDocument();
    expect(screen.getByText('Beta task')).toBeInTheDocument();
    expect(screen.getByText('The alpha product')).toBeInTheDocument();
    const strip = screen.getByRole('list', { name: 'Teams in this product' });
    expect(within(strip).getByRole('link', { name: /ALP/ })).toHaveAttribute('href', '/orbit/team/ALP');
    expect(within(strip).getByRole('link', { name: /BET/ })).toHaveAttribute('href', '/orbit/team/BET');
    expect(issueGets(api).every((c) => c.url.searchParams.get('product') === 'alpha')).toBe(true);
    // The product is fixed by the page: no Product chip, and no filter is counted.
    expect(screen.queryByRole('button', { name: /^Product/ })).toBeNull();
    expect(screen.queryByRole('button', { name: /^Clear \(/ })).toBeNull();
    expect(document.title).toContain('Alpha');
  });

  it('does not show the slug in the header (LAUN-151)', async () => {
    serve();
    renderApp('/orbit/product/alpha');
    await screen.findByRole('heading', { level: 1, name: 'Alpha' });
    expect(screen.queryByText('alpha')).toBeNull();
  });

  it('drops a ?product= or ?team= in the address (the page sends its own product only)', async () => {
    const api = serve({ issues: [card(1, ALP, 'Todo', 'Alpha task')] });
    renderApp('/orbit/product/alpha?product=other&team=GAM');
    await screen.findByText('Alpha task');
    for (const c of issueGets(api)) {
      expect(c.url.searchParams.get('product')).toBe('alpha');
      expect(c.url.searchParams.get('team')).toBeNull();
    }
  });

  it('the Status chip offers only the states of the product teams', async () => {
    serve({ issues: [card(1, ALP, 'Todo', 'Alpha task')] });
    renderApp('/orbit/product/alpha');
    await screen.findByText('Alpha task');
    await userEvent.setup().click(screen.getByRole('button', { name: 'Status' }));
    const names = (await screen.findAllByRole('option')).map((o) => o.textContent);
    expect(names).toEqual(expect.arrayContaining(['Backlog', 'Todo', 'Review']));
    expect(names.some((n) => /Gamma only/.test(n ?? ''))).toBe(false);
  });

  it('the lane "+" (LAUN-173) opens New issue with the product teams first and the lane state', async () => {
    const api = serve({ product: product({}, [BET]), issues: [card(1, BET, 'Todo', 'Beta task')] });
    const user = userEvent.setup();
    renderApp('/orbit/product/alpha?view=board');
    await screen.findByText('Beta task');
    await user.click(screen.getByRole('button', { name: 'Create issue in Todo' }));
    const dialog = await screen.findByRole('dialog', { name: 'New issue' });
    await user.type(within(dialog).getByLabelText('Title'), 'From a lane');
    await user.click(within(dialog).getByRole('button', { name: 'Create issue' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    const post = api.calls.find(
      (c) => c.method === 'POST' && c.url.pathname === '/api/v1/orgs/orbit/issues',
    )!;
    expect(post.body).toMatchObject({ team: 'BET', state: 'Todo' });
  });

  it('a lane "+" submitted before the team states load still sends the lane state (LAUN-173)', async () => {
    let open!: () => void;
    const gate = new Promise<void>((r) => (open = r));
    const api = serve({ product: product({}, [BET]), issues: [card(1, BET, 'Todo', 'Beta task')], gate });
    const user = userEvent.setup();
    renderApp('/orbit/product/alpha?view=board');
    await screen.findByText('Beta task');
    await user.click(screen.getByRole('button', { name: 'Create issue in Todo' }));
    const dialog = await screen.findByRole('dialog', { name: 'New issue' });
    await user.type(within(dialog).getByLabelText('Title'), 'Early');
    await user.click(within(dialog).getByRole('button', { name: 'Create issue' }));
    await waitFor(() =>
      expect(
        api.calls.some((c) => c.method === 'POST' && c.url.pathname === '/api/v1/orgs/orbit/issues'),
      ).toBe(true),
    );
    open();
    const post = api.calls.find(
      (c) => c.method === 'POST' && c.url.pathname === '/api/v1/orgs/orbit/issues',
    )!;
    expect(post.body).toMatchObject({ team: 'BET', state: 'Todo' });
  });

  it('a bad slug is the not-found page', async () => {
    const api = serve({ product: 'missing' });
    renderApp('/orbit/product/nope');
    expect(await screen.findByText('Page not found')).toBeInTheDocument();
    expect(issueGets(api)).toHaveLength(0);
    expect(document.title).toContain('Not found');
  });

  it('a product with no teams shows the empty state and sends no issue request', async () => {
    const api = serve({ product: product({}, []) });
    renderApp('/orbit/product/alpha');
    expect(await screen.findByText('This product has no teams yet')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /Assign teams in Settings, Products/ })).toHaveAttribute(
      'href',
      '/orbit/settings/products',
    );
    expect(issueGets(api)).toHaveLength(0);
    // Only the app's own two + buttons: the page has no create button of its own.
    expect(screen.getAllByRole('button', { name: 'Create issue' })).toHaveLength(2);
  });

  it('an archived product shows the banner and offers no create button on the page', async () => {
    serve({ product: product({ archivedAt: '2026-10-20T00:00:00Z' }), issues: [] });
    renderApp('/orbit/product/alpha');
    expect(await screen.findByText(/This product is archived/)).toBeInTheDocument();
    await screen.findByText('No issues yet');
    expect(screen.queryByRole('button', { name: 'Create the first issue' })).toBeNull();
    // The head button is gone; the app's own + stays (it is not a product control).
    expect(screen.getAllByRole('button', { name: 'Create issue' })).toHaveLength(2);
  });

  it('c opens the create dialog with the teams of the product first', async () => {
    serve({ product: product({}, [BET]), issues: [] });
    renderApp('/orbit/product/alpha');
    await screen.findByRole('heading', { name: 'Alpha' });
    await waitFor(() =>
      expect(screen.getByRole('list', { name: 'Teams in this product' })).toBeInTheDocument(),
    );
    const user = userEvent.setup();
    await user.keyboard('c');
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText(/BET · Team BET/)).toBeInTheDocument();
  });

  it('the + button opens the dialog with the product team', async () => {
    serve({ product: product({}, [BET]), issues: [] });
    renderApp('/orbit/product/alpha');
    await screen.findByRole('list', { name: 'Teams in this product' });
    const user = userEvent.setup();
    await user.click(screen.getAllByRole('button', { name: 'Create issue' })[0]!);
    expect(await within(await screen.findByRole('dialog')).findByText(/BET · Team BET/)).toBeInTheDocument();
  });
});

describe('product board', () => {
  let restore: () => void;
  beforeEach(() => {
    restore = stubBoardLayout();
    resetDragged();
  });
  afterEach(() => restore());

  const patches = (api: ReturnType<typeof mockApi>) => api.calls.filter((c) => c.method === 'PATCH');
  const cardEl = (id: string) => document.querySelector(`[data-issue-id="${id}"]`) as HTMLElement;
  const column = (name: string) => document.querySelector(`[data-column="${name}"]`) as HTMLElement;
  const ptr = { pointerId: 1, pointerType: 'mouse', isPrimary: true };

  it('a column of a state only some teams have shows only their cards; the other columns are not there', async () => {
    serve({ issues: [card(1, ALP, 'Review', 'Alpha review'), card(2, BET, 'Todo', 'Beta todo')] });
    renderApp('/orbit/product/alpha?view=board');
    await screen.findByText('Alpha review');
    expect(within(column('Review')).getByText('Alpha review')).toBeInTheDocument();
    expect(within(column('Review')).queryByText('Beta todo')).toBeNull();
    expect(column('Gamma only')).toBeNull();
  });

  it('a drag of a card whose team lacks the state is refused with a toast and sends no PATCH', async () => {
    const api = serve({
      issues: [card(1, ALP, 'Review', 'Alpha review'), card(2, BET, 'Todo', 'Beta todo')],
    });
    renderApp('/orbit/product/alpha?view=board');
    await screen.findByText('Beta todo');
    // Columns: Backlog 0, Todo 1, Review 2. Drag the BET card from Todo to the empty Review area.
    fireEvent.pointerDown(cardEl('i2'), {
      ...ptr,
      button: 0,
      clientX: middle(1, 0).x,
      clientY: middle(1, 0).y,
    });
    const to = at(2, 1);
    fireEvent.pointerMove(window, { ...ptr, clientX: middle(1, 0).x + 10, clientY: middle(1, 0).y + 10 });
    fireEvent.pointerMove(window, { ...ptr, clientX: to.x, clientY: to.y });
    fireEvent.pointerUp(window, { ...ptr, clientX: to.x, clientY: to.y });
    expect(await screen.findByText(/has no state "Review"/)).toBeInTheDocument();
    expect(patches(api)).toHaveLength(0);
  });

  it('a drag onto a state the team has still sends the one update', async () => {
    const api = serve({ issues: [card(1, ALP, 'Backlog', 'Alpha back'), card(2, BET, 'Todo', 'Beta todo')] });
    renderApp('/orbit/product/alpha?view=board');
    await screen.findByText('Beta todo');
    fireEvent.pointerDown(cardEl('i1'), {
      ...ptr,
      button: 0,
      clientX: middle(0, 0).x,
      clientY: middle(0, 0).y,
    });
    const to = at(2, 0);
    fireEvent.pointerMove(window, { ...ptr, clientX: middle(0, 0).x + 10, clientY: middle(0, 0).y + 10 });
    fireEvent.pointerMove(window, { ...ptr, clientX: to.x, clientY: to.y });
    fireEvent.pointerUp(window, { ...ptr, clientX: to.x, clientY: to.y });
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect((patches(api)[0]!.body as { state: string }).state).toBe('Review');
  });
});
