import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import type { Cycle } from '../../api/types';
import { callsTo, joinServer, page } from '../../test/join';
import { makeIssue, states, team } from '../../test/mockApi';
import { currentLocation, renderApp } from '../../test/render';

const day = 86400_000;
const iso = (offsetDays: number) => new Date(Date.now() + offsetDays * day).toISOString();
const cy = (n: number, from: number, to: number, over: Partial<Cycle> = {}): Cycle => ({
  id: `cy${n}`,
  team: { id: 'team1', key: 'ORB', name: 'Orbit' },
  number: n,
  name: null,
  description: '',
  startsAt: iso(from),
  endsAt: iso(to),
  completedAt: null,
  createdAt: iso(from),
  updatedAt: iso(from),
  archivedAt: null,
  ...over,
});
const cycles = [
  cy(3, -2, 12),
  cy(4, 12, 26),
  cy(2, -16, -2, { completedAt: iso(-2) }),
  cy(1, -30, -16, { completedAt: iso(-16) }),
];
const snap = (scope: number, done: number) => ({
  cycleId: 'x',
  date: '2026-10-25',
  scopeCount: scope,
  triageCount: 0,
  backlogCount: 0,
  unstartedCount: 0,
  startedCount: 0,
  completedCount: done,
  canceledCount: 0,
  scopePoints: 0,
  startedPoints: 0,
  completedPoints: 0,
  updatedAt: iso(0),
});
const on = { ...team, cyclesEnabled: true };

const PER_PAGE = 50;

/** Honours `team` and pages with `cursor`, like the server (latest start first). */
function cyclesRoute(all: Cycle[] = cycles) {
  return (r: { url: URL }) => {
    const t = r.url.searchParams.get('team');
    const rows = all
      .filter((c) => !t || c.team.key === t)
      .sort((a, b) => Date.parse(b.startsAt) - Date.parse(a.startsAt));
    const from = Number(r.url.searchParams.get('cursor') ?? 0);
    const next = from + PER_PAGE;
    return { json: { items: rows.slice(from, next), nextCursor: next < rows.length ? String(next) : null } };
  };
}

function server(routes = {}, teams: unknown[] = [on]) {
  return joinServer({
    routes: {
      'GET /api/v1/orgs/:org/teams': () => ({ json: page(teams) }),
      'GET /api/v1/orgs/:org/cycles': cyclesRoute(),
      'GET /api/v1/orgs/:org/cycles/:cycle/snapshots': (r) => ({
        json: page(r.params.cycle === 'cy3' ? [snap(5, 1), snap(4, 2)] : []),
      }),
      ...routes,
    },
  });
}

describe('sidebar cycles link', () => {
  it('shows only for a team with cycles on', async () => {
    server({}, [on, { ...team, id: 'team2', key: 'OFF', name: 'Off team' }]);
    renderApp('/orbit/issues');
    // First test of the file: the app renders cold, and on a slow CI runner that can take longer than the
    // 1 s default of findBy. Wait for the link, not for a fixed time.
    const link = await screen.findByRole('link', { name: 'Cycles' }, { timeout: 10_000 });
    expect(link).toHaveAttribute('href', '/orbit/team/ORB/cycles');
    expect(screen.getAllByRole('link', { name: 'Cycles' })).toHaveLength(1);
  });

  it('shows the Cycles link and the team menu together, and keeps the menu on a cycle page', async () => {
    server();
    renderApp('/orbit/team/ORB/cycle/3');
    const nav = await screen.findByRole('navigation', { name: 'Primary' });
    const current = await within(nav).findByRole('link', { name: 'Current' });
    await waitFor(() => expect(current).toHaveAttribute('aria-current', 'page'));
    const btn = within(nav).getByRole('button', { name: 'Orbit team options' });
    expect(btn).toHaveAttribute('aria-haspopup', 'menu');
    await userEvent.setup().click(btn);
    expect(await screen.findByRole('menuitem', { name: 'Team settings' })).toBeInTheDocument();
  });

  it('highlights only the Cycles link on the list page', async () => {
    server();
    renderApp('/orbit/team/ORB/cycles');
    const cyclesLink = await screen.findByRole('link', { name: 'Cycles' });
    const teamLink = screen.getByRole('link', { name: /Orbit/ });
    await waitFor(() => expect(cyclesLink).toHaveAttribute('aria-current', 'page'));
    expect(teamLink).not.toHaveAttribute('aria-current');
    expect(screen.getByRole('link', { name: 'All issues' })).not.toHaveAttribute('aria-current');
  });

  it('highlights Current, and not the team link, on the running cycle page', async () => {
    server();
    renderApp('/orbit/team/ORB/cycle/3');
    const nav = await screen.findByRole('navigation', { name: 'Primary' });
    const current = await within(nav).findByRole('link', { name: 'Current' });
    await waitFor(() => expect(current).toHaveAttribute('aria-current', 'page'));
    expect(within(nav).getByRole('link', { name: 'Cycles' })).not.toHaveAttribute('aria-current');
    expect(within(nav).getByRole('link', { name: 'Upcoming' })).not.toHaveAttribute('aria-current');
    expect(within(nav).getByRole('link', { name: /Orbit/ })).not.toHaveAttribute('aria-current');
  });

  it('keeps the team link active on the team issue list', async () => {
    server();
    renderApp('/orbit/team/ORB');
    const teamLink = await screen.findByRole('link', { name: /Orbit/ });
    expect(teamLink).toHaveAttribute('aria-current', 'page');
    expect(screen.getByRole('link', { name: 'Cycles' })).not.toHaveAttribute('aria-current');
  });

  it('scopes the create dialog to the team on the cycle routes', async () => {
    const web = { ...on, id: 'team2', key: 'WEB', name: 'Website' };
    server({}, [on, web]);
    for (const path of ['/orbit/team/WEB/cycles', '/orbit/team/WEB/cycle/3']) {
      const user = userEvent.setup();
      const { unmount } = renderApp(path);
      await screen.findAllByRole('link', { name: 'Cycles' });
      await user.keyboard('c');
      const dialog = await screen.findByRole('dialog');
      expect(await within(dialog).findByText('WEB · Website')).toBeInTheDocument();
      unmount();
    }
  });
});

describe('cycle list page', () => {
  it('lists current, next, then past, with dates and progress', async () => {
    server();
    renderApp('/orbit/team/ORB/cycles');
    const current = await screen.findByRole('region', { name: 'Current' });
    expect(within(current).getByRole('link', { name: /Cycle 3/ })).toHaveAttribute(
      'href',
      '/orbit/team/ORB/cycle/3',
    );
    expect(await within(current).findByText('50% (2/4)')).toBeInTheDocument();
    const order = screen.getAllByRole('region').map((r) => r.getAttribute('aria-label'));
    expect(order.indexOf('Current')).toBeLessThan(order.indexOf('Next'));
    expect(order.indexOf('Next')).toBeLessThan(order.indexOf('Past'));
    expect(within(screen.getByRole('region', { name: 'Past' })).getAllByRole('listitem')).toHaveLength(2);
    expect(screen.getAllByText(/ to /).length).toBeGreaterThan(0);
    expect(within(screen.getByRole('region', { name: 'Next' })).getByText('Not started')).toBeInTheDocument();
  });

  it('asks for the team, skips snapshots of a next cycle, and names a row without the number glued on', async () => {
    const api = server();
    renderApp('/orbit/team/ORB/cycles');
    expect(await screen.findByRole('link', { name: 'Cycle 3 Current' })).toBeInTheDocument();
    expect(callsTo(api, 'GET', '/cycles')[0]!.url.searchParams.get('team')).toBe('ORB');
    await screen.findByText('50% (2/4)');
    const asked = callsTo(api, 'GET', '/snapshots').map((c) => c.params.cycle);
    expect(asked).not.toContain('cy4');
    expect(asked).toContain('cy3');
  });

  it('accepts a lower case team in the URL', async () => {
    const api = server();
    renderApp('/orbit/team/orb/cycles');
    expect(await screen.findByRole('region', { name: 'Current' })).toBeInTheDocument();
    expect(callsTo(api, 'GET', '/cycles')[0]!.url.searchParams.get('team')).toBe('ORB');
  });

  it('says so for an unknown team', async () => {
    const api = server();
    renderApp('/orbit/team/NOPE/cycles');
    expect(await screen.findByText('Team not found')).toBeInTheDocument();
    expect(callsTo(api, 'GET', '/cycles')).toHaveLength(0);
  });

  it('shows "No data yet" when a cycle has no snapshot', async () => {
    server();
    renderApp('/orbit/team/ORB/cycles');
    const past = await screen.findByRole('region', { name: 'Past' });
    await waitFor(() => expect(within(past).getAllByText('No data yet')).toHaveLength(2));
  });

  it('shows the burn-up chart on the cycle page from the one snapshot call', async () => {
    const api = server();
    renderApp('/orbit/team/ORB/cycle/3');
    expect(await screen.findByRole('img', { name: /Burn-up chart/ })).toBeInTheDocument();
    expect(screen.getByRole('table', { name: 'Burn-up by day, in issues' })).toBeInTheDocument();
    expect(callsTo(api, 'GET', '/snapshots')).toHaveLength(1);
  });

  it('shows the burn-up empty state for a cycle with no rows', async () => {
    server();
    renderApp('/orbit/team/ORB/cycle/2');
    expect(await screen.findByText(/No burn-up data yet/)).toBeInTheDocument();
  });

  it('says a next cycle has no chart yet', async () => {
    server();
    renderApp('/orbit/team/ORB/cycle/4');
    expect(await screen.findByText(/starts when the cycle starts/)).toBeInTheDocument();
  });

  it('loads every page of cycles: Past lists them all and "Show all" opens the rest', async () => {
    const many: Cycle[] = [];
    for (let n = 1; n <= 58; n++)
      many.push(cy(n, -30 - (60 - n) * 14, -16 - (60 - n) * 14, { completedAt: iso(-16) }));
    many.push(cy(59, -2, 12), cy(60, 12, 26));
    const api = server({ 'GET /api/v1/orgs/:org/cycles': cyclesRoute(many) });
    const user = userEvent.setup();
    renderApp('/orbit/team/ORB/cycles');
    await user.click(await screen.findByRole('button', { name: 'Show all 58 past cycles' }));
    const past = screen.getByRole('region', { name: 'Past' });
    expect(within(past).getAllByRole('listitem')).toHaveLength(58);
    expect(callsTo(api, 'GET', '/cycles')).toHaveLength(2);
    expect(callsTo(api, 'GET', '/cycles')[1]!.url.searchParams.get('cursor')).toBe('50');
  });

  it('opens a cycle that is on the second page', async () => {
    const many: Cycle[] = [];
    for (let n = 1; n <= 58; n++)
      many.push(cy(n, -30 - (60 - n) * 14, -16 - (60 - n) * 14, { completedAt: iso(-16) }));
    many.push(cy(59, -2, 12), cy(60, 12, 26));
    server({ 'GET /api/v1/orgs/:org/cycles': cyclesRoute(many) });
    renderApp('/orbit/team/ORB/cycle/5');
    expect(await screen.findByRole('heading', { name: /Cycle 5/ })).toBeInTheDocument();
    expect(await screen.findByText('No issues in this cycle')).toBeInTheDocument();
  });

  it('says so when the team has cycles off', async () => {
    server({}, [team]);
    renderApp('/orbit/team/ORB/cycles');
    expect(await screen.findByText('Cycles are off for this team')).toBeInTheDocument();
  });

  it('shows an empty state', async () => {
    server({ 'GET /api/v1/orgs/:org/cycles': () => ({ json: page([]) }) });
    renderApp('/orbit/team/ORB/cycles');
    expect(await screen.findByText('No cycles yet')).toBeInTheDocument();
  });

  it('shows an error state with a retry', async () => {
    server({
      'GET /api/v1/orgs/:org/cycles': () => ({ status: 500, json: { error: 'x', code: 'internal' } }),
    });
    renderApp('/orbit/team/ORB/cycles');
    expect(await screen.findByText('Could not load cycles')).toBeInTheDocument();
  });

  it('shows "Progress unavailable" when one snapshot call fails', async () => {
    server({
      'GET /api/v1/orgs/:org/cycles/:cycle/snapshots': () => ({
        status: 500,
        json: { error: 'x', code: 'internal' },
      }),
    });
    renderApp('/orbit/team/ORB/cycles');
    expect((await screen.findAllByText('Progress unavailable')).length).toBeGreaterThan(0);
    expect(screen.getByRole('region', { name: 'Current' })).toBeInTheDocument();
  });
});

describe('progress caching', () => {
  const progressStale = (client: ReturnType<typeof renderApp>['client'], id: string) =>
    client.getQueryCache().find({ queryKey: ['org', 'orbit', 'cycle-progress', id] })!.observers[0]!.options
      .staleTime;

  it('keeps a completed cycle forever, but not one that ended and is not completed yet', async () => {
    const ended = cy(5, -20, -1); // ended yesterday, the job has not completed it
    server({
      'GET /api/v1/orgs/:org/cycles': cyclesRoute([cy(2, -16, -2, { completedAt: iso(-2) }), ended]),
    });
    const { client } = renderApp('/orbit/team/ORB/cycles');
    await screen.findAllByText('No data yet');
    expect(progressStale(client, 'cy2')).toBe(Infinity);
    expect(progressStale(client, 'cy5')).not.toBe(Infinity);
  });

  it('does the same on the cycle page', async () => {
    server({ 'GET /api/v1/orgs/:org/cycles': cyclesRoute([cy(5, -20, -1)]) });
    const { client } = renderApp('/orbit/team/ORB/cycle/5');
    await screen.findByText('No issues in this cycle');
    expect(progressStale(client, 'cy5')).not.toBe(Infinity);
  });
});

describe('cycle page', () => {
  it('labels the cycle (current) or (next) in the head', async () => {
    server();
    const first = renderApp('/orbit/team/ORB/cycle/3');
    expect(await screen.findByText(/\(current\)/)).toBeInTheDocument();
    first.unmount();
    renderApp('/orbit/team/ORB/cycle/4');
    expect(await screen.findByText(/\(next\)/)).toBeInTheDocument();
  });

  it('does not ask for snapshots of a next cycle', async () => {
    const api = server();
    renderApp('/orbit/team/ORB/cycle/4');
    await screen.findByText(/\(next\)/);
    expect(callsTo(api, 'GET', '/snapshots')).toHaveLength(0);
  });

  it('says not found when the team has cycles off', async () => {
    const api = server({}, [team]);
    renderApp('/orbit/team/ORB/cycle/3');
    expect(await screen.findByText('Cycle not found')).toBeInTheDocument();
    expect(callsTo(api, 'GET', '/issues')).toHaveLength(0);
  });

  it.each(['abc', '03', '0', '-1'])('says not found for the number "%s"', async (n) => {
    const api = server();
    renderApp(`/orbit/team/ORB/cycle/${n}`);
    expect(await screen.findByText('Cycle not found')).toBeInTheDocument();
    expect(callsTo(api, 'GET', '/issues')).toHaveLength(0);
  });

  it('asks for the cycle issues and groups them by status', async () => {
    const api = server({
      'GET /api/v1/orgs/:org/issues': () => ({
        json: page([
          makeIssue(1),
          makeIssue(2, {
            state: { id: 's3', name: 'In Progress', type: 'started', color: states[2]!.color },
          }),
        ]),
      }),
    });
    renderApp('/orbit/team/ORB/cycle/3');
    expect(await screen.findByRole('region', { name: 'Backlog' })).toBeInTheDocument();
    expect(screen.getByRole('region', { name: 'In Progress' })).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: /Cycle 3/ })).toBeInTheDocument();
    const call = callsTo(api, 'GET', '/issues')[0]!;
    expect(call.url.searchParams.get('cycle')).toBe('3');
    expect(call.url.searchParams.get('team')).toBe('ORB');
  });

  it('shows an empty state for a cycle with no issues', async () => {
    server();
    renderApp('/orbit/team/ORB/cycle/3');
    expect(await screen.findByText('No issues in this cycle')).toBeInTheDocument();
  });

  it('shows an error state when the issues fail', async () => {
    server({
      'GET /api/v1/orgs/:org/issues': () => ({ status: 500, json: { error: 'x', code: 'internal' } }),
    });
    renderApp('/orbit/team/ORB/cycle/3');
    expect(await screen.findByText('Could not load issues')).toBeInTheDocument();
  });

  it('says not found for an unknown cycle number and does not ask for issues', async () => {
    const api = server();
    renderApp('/orbit/team/ORB/cycle/99');
    expect(await screen.findByText('Cycle not found')).toBeInTheDocument();
    expect(callsTo(api, 'GET', '/issues')).toHaveLength(0);
  });
});

describe('sidebar children (LAUN-17)', () => {
  it('shows Current and Upcoming under Cycles, and none when cycles are off', async () => {
    server({}, [on, { ...team, id: 'team2', key: 'OFF', name: 'Off team' }]);
    renderApp('/orbit/issues');
    const group = await screen.findByRole('group', { name: 'ORB cycles' });
    expect(within(group).getByRole('link', { name: 'Current' })).toHaveAttribute(
      'href',
      '/orbit/team/ORB/cycles/current',
    );
    expect(within(group).getByRole('link', { name: 'Upcoming' })).toHaveAttribute(
      'href',
      '/orbit/team/ORB/cycles/upcoming',
    );
    expect(screen.getAllByRole('link', { name: 'Current' })).toHaveLength(1);
  });

  it('lights the right child when cycles share a start date or leave a gap', async () => {
    server({
      'GET /api/v1/orgs/:org/cycles': cyclesRoute([
        cy(3, -2, 12),
        cy(4, 12, 20),
        cy(5, 12, 26),
        cy(6, 40, 54),
      ]),
    });
    renderApp('/orbit/team/ORB/cycle/4');
    const up = await screen.findByRole('link', { name: 'Upcoming' });
    await waitFor(() => expect(up).toHaveAttribute('aria-current', 'page'));
  });

  it('does not read the cycles on a page that is not a cycle page', async () => {
    const api = server();
    renderApp('/orbit/issues');
    await screen.findByRole('group', { name: 'ORB cycles' });
    expect(callsTo(api, 'GET', '/cycles')).toHaveLength(0);
  });

  it('lights Upcoming on the next cycle, and only Cycles on a past one', async () => {
    server();
    const first = renderApp('/orbit/team/ORB/cycle/4');
    const up = await screen.findByRole('link', { name: 'Upcoming' });
    await waitFor(() => expect(up).toHaveAttribute('aria-current', 'page'));
    expect(screen.getByRole('link', { name: 'Current' })).not.toHaveAttribute('aria-current');
    first.unmount();
    renderApp('/orbit/team/ORB/cycle/2');
    const nav = await screen.findByRole('navigation', { name: 'Primary' });
    const cyc = await within(nav).findByRole('link', { name: 'Cycles' });
    await waitFor(() => expect(cyc).toHaveAttribute('aria-current', 'page'));
    expect(screen.getByRole('link', { name: 'Current' })).not.toHaveAttribute('aria-current');
    expect(screen.getByRole('link', { name: 'Upcoming' })).not.toHaveAttribute('aria-current');
  });

  it('Current opens the running cycle and Upcoming the next one', async () => {
    server();
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await user.click(await screen.findByRole('link', { name: 'Current' }));
    expect(await screen.findByRole('heading', { name: /Cycle 3/ })).toBeInTheDocument();
    await user.click(screen.getByRole('link', { name: 'Upcoming' }));
    expect(await screen.findByRole('heading', { name: /Cycle 4/ })).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Upcoming' })).toHaveAttribute('aria-current', 'page');
  });

  it('says so when there is no current or upcoming cycle', async () => {
    server({ 'GET /api/v1/orgs/:org/cycles': cyclesRoute([cy(2, -16, -2, { completedAt: iso(-2) })]) });
    const first = renderApp('/orbit/team/ORB/cycles/current');
    expect(await screen.findByText('No current cycle')).toBeInTheDocument();
    first.unmount();
    renderApp('/orbit/team/ORB/cycles/upcoming');
    expect(await screen.findByText('No upcoming cycle')).toBeInTheDocument();
  });
});

describe('cycle page header (LAUN-17)', () => {
  it('has the breadcrumb Team > Cycles > Cycle N', async () => {
    server();
    renderApp('/orbit/team/ORB/cycle/3');
    const crumbs = await screen.findByRole('navigation', { name: 'Breadcrumb' });
    expect(await within(crumbs).findByRole('link', { name: 'Orbit' })).toHaveAttribute(
      'href',
      '/orbit/team/ORB',
    );
    expect(within(crumbs).getByRole('link', { name: 'Cycles' })).toHaveAttribute(
      'href',
      '/orbit/team/ORB/cycles',
    );
    expect(within(crumbs).getByRole('heading', { name: 'Cycle 3' })).toBeInTheDocument();
    expect(within(crumbs).getByRole('heading', { name: 'Cycle 3' }).closest('li')).toHaveAttribute(
      'aria-current',
      'page',
    );
    expect(await within(crumbs).findByRole('button', { name: 'Switch cycle' })).toHaveAttribute(
      'aria-haspopup',
      'menu',
    );
  });

  it('opens a switcher with the next cycle, dates and shortcut, and no past cycle', async () => {
    server();
    const user = userEvent.setup();
    renderApp('/orbit/team/ORB/cycle/3');
    await user.click(await screen.findByRole('button', { name: 'Switch cycle' }));
    const menu = await screen.findByRole('menu', { name: 'Switch cycle' });
    expect(within(menu).getByText('Next cycle')).toBeInTheDocument();
    // Cycle 2 is past (LAUN-116): not listed; the Cycles list has it.
    expect(within(menu).queryByText('Previous cycle')).not.toBeInTheDocument();
    expect(within(menu).queryByRole('menuitem', { name: /Cycle 2/ })).not.toBeInTheDocument();
    expect(within(menu).getByRole('menuitem', { name: 'All cycles' })).toBeInTheDocument();
    const next = within(menu).getByRole('menuitem', { name: /Cycle 4/ });
    expect(next).toHaveAttribute('aria-keyshortcuts', 'Alt+K');
    expect(next.textContent).toMatch(/[A-Z][a-z]{2} \d+ - [A-Z][a-z]{2} \d+/);
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
  });

  it('on a past cycle keeps it in the menu, marked past, and lists no other past cycle (LAUN-116)', async () => {
    server();
    const user = userEvent.setup();
    renderApp('/orbit/team/ORB/cycle/2');
    await user.click(await screen.findByRole('button', { name: 'Switch cycle' }));
    const menu = await screen.findByRole('menu', { name: 'Switch cycle' });
    const here = within(menu).getByRole('menuitem', { name: /Cycle 2/ });
    expect(here.textContent).toMatch(/Past/);
    expect(here).toHaveAttribute('aria-current', 'true');
    expect(here).not.toHaveAttribute('aria-keyshortcuts');
    expect(within(menu).queryByRole('menuitem', { name: /Cycle 1/ })).not.toBeInTheDocument();
    expect(within(menu).getByRole('menuitem', { name: /Cycle 3/ })).toBeInTheDocument();
    expect(within(menu).getByRole('menuitem', { name: 'All cycles' })).toBeInTheDocument();
    // Selecting it changes nothing: same route, same heading.
    await user.click(here);
    expect(screen.getByRole('heading', { name: 'Cycle 2' })).toBeInTheDocument();
    expect(currentLocation.pathname).toBe('/orbit/team/ORB/cycle/2');
  });

  it('the Cycles list still shows the past cycles (LAUN-116)', async () => {
    server();
    renderApp('/orbit/team/ORB/cycles');
    expect(await screen.findByText('Past')).toBeInTheDocument();
  });

  it('picks a cycle from the switcher with the keyboard', async () => {
    server();
    const user = userEvent.setup();
    renderApp('/orbit/team/ORB/cycle/3');
    await user.click(await screen.findByRole('button', { name: 'Switch cycle' }));
    await screen.findByRole('menu');
    await user.keyboard('{Enter}');
    expect(await screen.findByRole('heading', { name: 'Cycle 4' })).toBeInTheDocument();
  });

  it('Alt+K goes to the next cycle and Alt+J to the previous one; an end does nothing', async () => {
    server();
    const user = userEvent.setup();
    renderApp('/orbit/team/ORB/cycle/3');
    await screen.findByRole('button', { name: 'Switch cycle' });
    await user.keyboard('{Alt>}k{/Alt}');
    expect(await screen.findByRole('heading', { name: 'Cycle 4' })).toBeInTheDocument();
    await user.keyboard('{Alt>}k{/Alt}');
    expect(screen.getByRole('heading', { name: 'Cycle 4' })).toBeInTheDocument();
    await screen.findByRole('button', { name: 'Switch cycle' });
    await user.keyboard('{Alt>}j{/Alt}');
    await screen.findByRole('heading', { name: 'Cycle 3' });
    await user.keyboard('{Alt>}j{/Alt}');
    expect(await screen.findByRole('heading', { name: 'Cycle 2' })).toBeInTheDocument();
  });

  it('ignores Alt+K in a field, with other modifiers, on repeat and with a dialog open', async () => {
    server();
    const user = userEvent.setup();
    renderApp('/orbit/team/ORB/cycle/3');
    await screen.findByRole('button', { name: 'Switch cycle' });
    const input = document.createElement('input');
    document.body.append(input);
    input.focus();
    await user.keyboard('{Alt>}k{/Alt}');
    input.remove();
    await user.keyboard('{Control>}{Alt>}k{/Alt}{/Control}{Shift>}{Alt>}k{/Alt}{/Shift}');
    document.body.dispatchEvent(
      new KeyboardEvent('keydown', { code: 'KeyK', altKey: true, repeat: true, bubbles: true }),
    );
    const dlg = document.createElement('div');
    dlg.setAttribute('role', 'dialog');
    document.body.append(dlg);
    await user.keyboard('{Alt>}k{/Alt}');
    dlg.remove();
    expect(screen.getByRole('heading', { name: 'Cycle 3' })).toBeInTheDocument();
  });

  it('returns focus to the switch button after a pick', async () => {
    server();
    const user = userEvent.setup();
    renderApp('/orbit/team/ORB/cycle/3');
    await user.click(await screen.findByRole('button', { name: 'Switch cycle' }));
    await user.click(await screen.findByRole('menuitem', { name: /Cycle 4/ }));
    await screen.findByRole('heading', { name: 'Cycle 4' });
    expect(screen.getByRole('button', { name: 'Switch cycle' })).toHaveFocus();
  });

  it('closes the open switcher when Alt+K moves to another cycle', async () => {
    server();
    const user = userEvent.setup();
    renderApp('/orbit/team/ORB/cycle/3');
    await user.click(await screen.findByRole('button', { name: 'Switch cycle' }));
    await screen.findByRole('menu');
    await user.keyboard('{Alt>}k{/Alt}');
    await screen.findByRole('heading', { name: 'Cycle 4' });
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
  });

  it('shows only "All cycles" for a team with one cycle', async () => {
    server({ 'GET /api/v1/orgs/:org/cycles': cyclesRoute([cy(3, -2, 12)]) });
    const user = userEvent.setup();
    renderApp('/orbit/team/ORB/cycle/3');
    await user.click(await screen.findByRole('button', { name: 'Switch cycle' }));
    const menu = await screen.findByRole('menu');
    expect(within(menu).getAllByRole('menuitem')).toHaveLength(1);
    expect(within(menu).getByRole('menuitem', { name: 'All cycles' })).toBeInTheDocument();
    expect(within(menu).queryByText('Next cycle')).not.toBeInTheDocument();
  });

  it('has no star or dead button in the header', async () => {
    server();
    renderApp('/orbit/team/ORB/cycle/3');
    await screen.findByRole('button', { name: 'Switch cycle' });
    expect(screen.queryByRole('button', { name: /star|favou?rite/i })).not.toBeInTheDocument();
  });

  it('keeps no selection when the cycle changes', async () => {
    server({ 'GET /api/v1/orgs/:org/issues': () => ({ json: page([makeIssue(1)]) }) });
    const user = userEvent.setup();
    renderApp('/orbit/team/ORB/cycle/3');
    const box = await screen.findByRole('checkbox');
    await user.click(box);
    expect(screen.getByRole('checkbox')).toBeChecked();
    (document.activeElement as HTMLElement).blur();
    await user.keyboard('{Alt>}k{/Alt}');
    await screen.findByRole('heading', { name: 'Cycle 4' });
    expect(await screen.findByRole('checkbox')).not.toBeChecked();
  });
});

describe('cycle page filters and view (LAUN-18)', () => {
  const issuesRoute = () => ({
    'GET /api/v1/orgs/:org/issues': () => ({
      json: page([
        makeIssue(1),
        makeIssue(2, {
          state: { id: 's3', name: 'In Progress', type: 'started', color: states[2]!.color },
        }),
      ]),
    }),
  });
  const lastIssuesCall = (api: Parameters<typeof callsTo>[0]) => callsTo(api, 'GET', '/issues').at(-1)!;

  it('shows the toolbar of the issues list', async () => {
    server(issuesRoute());
    renderApp('/orbit/team/ORB/cycle/3');
    const bar = await screen.findByRole('toolbar', { name: 'Filters' });
    for (const name of ['Status', 'Assignee', 'Label', 'Priority', 'List view', 'Board view'])
      expect(within(bar).getByRole('button', { name })).toBeInTheDocument();
    expect(within(bar).getByRole('searchbox')).toBeInTheDocument();
    expect(within(bar).getByRole('combobox')).toHaveValue('updated');
  });

  it('sends each filter with the cycle and the team, and keeps it in the URL', async () => {
    const api = server(issuesRoute());
    const user = userEvent.setup();
    renderApp('/orbit/team/ORB/cycle/3');
    const bar = await screen.findByRole('toolbar', { name: 'Filters' });

    await user.click(within(bar).getByRole('button', { name: 'Priority' }));
    await user.click(await screen.findByRole('option', { name: /Urgent/ }));
    await waitFor(() => expect(lastIssuesCall(api).url.searchParams.get('priority')).toBe('1'));
    let call = lastIssuesCall(api);
    expect(call.url.searchParams.get('cycle')).toBe('3');
    expect(call.url.searchParams.get('team')).toBe('ORB');

    await user.keyboard('{Escape}');
    await user.type(within(bar).getByRole('searchbox'), 'login');
    await waitFor(() => expect(lastIssuesCall(api).url.searchParams.get('query')).toBe('login'));

    await user.selectOptions(within(bar).getByRole('combobox'), 'created');
    await waitFor(() => expect(lastIssuesCall(api).url.searchParams.get('orderBy')).toBe('createdAt'));
    call = lastIssuesCall(api);
    expect(call.url.searchParams.get('cycle')).toBe('3');
    expect(call.url.searchParams.get('priority')).toBe('1');

    const loc = currentLocation;
    expect(loc.pathname).toBe('/orbit/team/ORB/cycle/3');
    const sp = new URLSearchParams(loc.search);
    expect(sp.get('priority')).toBe('1');
    expect(sp.get('q')).toBe('login');
    expect(sp.get('sort')).toBe('created');
    expect(sp.has('team')).toBe(false);
  });

  it('keeps the filters and the view after a reload', async () => {
    const api = server(issuesRoute());
    renderApp('/orbit/team/ORB/cycle/3?state=In%20Progress&priority=2&view=board');
    await screen.findByRole('toolbar', { name: 'Filters' });
    await waitFor(() => expect(callsTo(api, 'GET', '/issues').length).toBeGreaterThan(0));
    const call = lastIssuesCall(api);
    expect(call.url.searchParams.get('state')).toBe('In Progress');
    expect(call.url.searchParams.get('priority')).toBe('2');
    expect(call.url.searchParams.get('cycle')).toBe('3');
    expect(screen.getByRole('button', { name: 'Board view' })).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByRole('button', { name: /Clear \(2\)/ })).toBeInTheDocument();
  });

  it('shows a board of the cycle issues, with columns by status', async () => {
    server(issuesRoute());
    const user = userEvent.setup();
    renderApp('/orbit/team/ORB/cycle/3');
    await screen.findByRole('region', { name: 'Backlog' });
    await user.click(screen.getByRole('button', { name: 'Board view' }));
    const board = await screen.findByRole('group', { name: 'Board' });
    expect(within(board).getByRole('region', { name: /^Backlog, 1 issues/ })).toBeInTheDocument();
    expect(within(board).getByRole('region', { name: /^In Progress, 1 issues/ })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Board view' })).toHaveAttribute('aria-pressed', 'true');
    await user.click(screen.getByRole('button', { name: 'List view' }));
    expect(await screen.findByRole('region', { name: 'Backlog' })).toBeInTheDocument();
  });

  it('says no issues match when filters empty the cycle, and clears them', async () => {
    const user = userEvent.setup();
    server({
      'GET /api/v1/orgs/:org/issues': (r: { url: URL }) => ({
        json: page(r.url.searchParams.get('priority') ? [] : [makeIssue(1)]),
      }),
    });
    renderApp('/orbit/team/ORB/cycle/3?priority=1');
    expect(await screen.findByText('No issues match these filters')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Clear filters' }));
    expect(await screen.findByRole('region', { name: 'Backlog' })).toBeInTheDocument();
  });

  it('sends the status, assignee and label filters with the cycle', async () => {
    const api = server(issuesRoute());
    const user = userEvent.setup();
    renderApp('/orbit/team/ORB/cycle/3');
    const bar = await screen.findByRole('toolbar', { name: 'Filters' });
    const pick = async (chip: string, option: string | RegExp) => {
      await user.click(within(bar).getByRole('button', { name: chip }));
      await user.click(await screen.findByRole('option', { name: option }));
      await user.keyboard('{Escape}');
    };
    await pick('Status', /In Progress/);
    await waitFor(() => expect(lastIssuesCall(api).url.searchParams.get('state')).toBe('In Progress'));
    await pick('Assignee', /Chen/);
    await waitFor(() => expect(lastIssuesCall(api).url.searchParams.get('assignee')).toBe('chen'));
    await pick('Label', /bug/);
    await waitFor(() => expect(lastIssuesCall(api).url.searchParams.get('label')).toBe('bug'));
    const call = lastIssuesCall(api);
    expect(call.url.searchParams.get('cycle')).toBe('3');
    expect(call.url.searchParams.get('team')).toBe('ORB');
    expect(call.url.searchParams.get('state')).toBe('In Progress');
    expect(call.url.searchParams.get('assignee')).toBe('chen');
  });

  it('does not move the filter bar when the first row is selected', async () => {
    const user = userEvent.setup();
    server(issuesRoute());
    renderApp('/orbit/team/ORB/cycle/3');
    const bar = await screen.findByRole('toolbar', { name: 'Filters' });
    await screen.findByRole('region', { name: 'Backlog' });
    expect(bar.previousElementSibling).toBeNull();
    await user.keyboard('j');
    await user.keyboard('x');
    const status = screen.getByRole('status');
    expect(status).toHaveTextContent('1 selected');
    expect(bar.previousElementSibling).toBeNull();
    expect(bar.parentElement!.firstElementChild).toBe(bar);
    // the count is out of the flow: it is not a sibling above the bar or the rows
    expect(status.parentElement!.parentElement).toBe(bar.parentElement);
    expect(status.parentElement).not.toBe(bar.parentElement);
    expect(bar.nextElementSibling).not.toBe(status.parentElement);
    await user.click(within(status).getByRole('button', { name: 'Clear' }));
    expect(screen.queryByRole('status')).not.toBeInTheDocument();
    expect(bar.previousElementSibling).toBeNull();
  });

  it('leaves the header progress on the whole cycle when a filter is on', async () => {
    const api = server({
      'GET /api/v1/orgs/:org/issues': () => ({ json: page([makeIssue(1)]) }),
    });
    renderApp('/orbit/team/ORB/cycle/3?state=Done&priority=1');
    expect(await screen.findByText(/50% done \(2\/4\)/)).toBeInTheDocument();
    await screen.findByRole('region', { name: 'Backlog' });
    // one filtered row, none done: a head built from the rows would say 0% (0/1)
    expect(screen.queryByText(/\(0\/1\)/)).not.toBeInTheDocument();
    expect(callsTo(api, 'GET', '/snapshots').every((c) => !c.url.searchParams.has('priority'))).toBe(true);
  });

  it('uses the bulk keys of the issues list', async () => {
    const user = userEvent.setup();
    server(issuesRoute());
    renderApp('/orbit/team/ORB/cycle/3');
    await screen.findByRole('region', { name: 'Backlog' });
    await user.keyboard('j');
    await user.keyboard('x');
    expect(screen.getAllByRole('checkbox')[0]).toBeChecked();
    expect(screen.getByRole('status')).toHaveTextContent('1 selected');
  });
});
