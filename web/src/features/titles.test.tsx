import { act, screen, waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it } from 'vitest';
import { keys } from '../api/keys';
import type { Cycle } from '../api/types';
import { MAX_ITEM_TITLE } from '../lib/documentTitle';
import { baseRoutes, makeIssue, mockApi, team, type Handler } from '../test/mockApi';
import { renderApp, testClient } from '../test/render';

// LAUN-30: the tab title follows the page. Format: "<Org name> › <page or item>".

const cycle: Cycle = {
  id: 'cy8',
  team: { id: 'team1', key: 'ORB', name: 'Orbit' },
  number: 8,
  name: null,
  description: '',
  startsAt: new Date(Date.now() - 86400_000).toISOString(),
  endsAt: new Date(Date.now() + 86400_000 * 13).toISOString(),
  completedAt: null,
  createdAt: '2026-01-01T00:00:00Z',
  updatedAt: '2026-01-01T00:00:00Z',
  archivedAt: null,
};

function server(routes: Record<string, Handler> = {}, platform = false) {
  return mockApi({
    ...baseRoutes([makeIssue(123, { title: 'Fix the thing' })]),
    'GET /api/v1/orgs/:org/teams': () => ({
      json: { items: [{ ...team, cyclesEnabled: true }], nextCursor: null },
    }),
    'GET /api/v1/orgs/:org/cycles': () => ({ json: { items: [cycle], nextCursor: null } }),
    'GET /api/v1/orgs/:org/cycles/:cycle/snapshots': () => ({ json: { items: [], nextCursor: null } }),
    'GET /api/v1/auth/session': () => ({
      json: {
        user: { id: 'u1', username: 'chen', name: 'Chen', email: 'c@e.com', isPlatformAdmin: platform },
        // The display name differs from the slug on purpose.
        orgs: [{ id: 'o1', slug: 'orbit', name: 'Orbit Labs', role: 'admin' }],
        csrfToken: 'csrf-test',
      },
    }),
    ...routes,
  });
}

const title = () => document.title;

beforeEach(() => {
  document.title = 'Launchpad 🚀';
});

describe('tab titles', () => {
  it.each([
    ['/orbit/issues', 'Orbit Labs › Issues'],
    ['/orbit/my-issues', 'Orbit Labs › My issues'],
    ['/orbit/team/ORB', 'Orbit Labs › Orbit'],
    ['/orbit/team/ORB/cycles', 'Orbit Labs › Cycles'],
    ['/orbit/team/ORB/cycle/8', 'Orbit Labs › Cycle 8'],
    ['/orbit/issue/ORB-123', 'Orbit Labs › ORB-123 Fix the thing'],
    ['/orbit/settings/members', 'Orbit Labs › Settings › Members'],
    ['/orbit/settings/organisation', 'Orbit Labs › Settings › Organisation'],
    ['/orbit/settings/teams', 'Orbit Labs › Settings › Teams'],
    ['/orbit/settings/signups', 'Orbit Labs › Settings › Sign-ups'],
    ['/orbit/settings/agents', 'Orbit Labs › Settings › Agents'],
    ['/orbit/settings/tokens', 'Orbit Labs › Settings › API tokens'],
    ['/orbit/nowhere', 'Orbit Labs › Not found'],
    ['/orbit/settings/nowhere', 'Orbit Labs › Not found'],
  ])('%s', async (path, expected) => {
    server();
    renderApp(path);
    await waitFor(() => expect(title()).toBe(expected));
  });

  it('settings, Platform', async () => {
    server({}, true);
    renderApp('/orbit/settings/platform');
    await waitFor(() => expect(title()).toBe('Orbit Labs › Settings › Platform'));
  });

  it('an org the person is not in: Not found, with no org name', async () => {
    server();
    renderApp('/other/issues');
    await waitFor(() => expect(title()).toBe('Not found'));
  });

  it('the page name changes at once on navigation inside the app', async () => {
    server();
    renderApp('/orbit/issues');
    await waitFor(() => expect(title()).toBe('Orbit Labs › Issues'));
    screen.getByRole('link', { name: 'My issues' }).click();
    await waitFor(() => expect(title()).toBe('Orbit Labs › My issues'));
  });

  it('an issue page shows the identifier alone while it loads, then adds the title', async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => (release = r));
    server({
      'GET /api/v1/orgs/:org/issues/:issue': async () => {
        await gate;
        return { json: { ...makeIssue(123, { title: 'Fix the thing' }) } };
      },
    });
    renderApp('/orbit/issue/ORB-123');
    await waitFor(() => expect(title()).toBe('Orbit Labs › ORB-123'));
    release();
    await waitFor(() => expect(title()).toBe('Orbit Labs › ORB-123 Fix the thing'));
  });

  it('a missing issue is Not found', async () => {
    server();
    renderApp('/orbit/issue/ORB-999');
    await waitFor(() => expect(title()).toBe('Orbit Labs › Not found'));
  });

  it('an unknown team page is Not found once the teams have loaded', async () => {
    server();
    renderApp('/orbit/team/ZZZ');
    await waitFor(() => expect(title()).toBe('Orbit Labs › Not found'));
  });

  it('only a 404 makes an issue Not found: a 500 keeps the identifier', async () => {
    server({
      'GET /api/v1/orgs/:org/issues/:issue': () => ({
        status: 500,
        json: { error: 'boom', code: 'internal_error' },
      }),
    });
    renderApp('/orbit/issue/ORB-123');
    await screen.findByText(/Could not load/i);
    expect(title()).toBe('Orbit Labs › ORB-123');
  });

  it('a whitespace-only issue title leaves no trailing space', async () => {
    server({ 'GET /api/v1/orgs/:org/issues/:issue': () => ({ json: makeIssue(123, { title: '   ' }) }) });
    renderApp('/orbit/issue/ORB-123');
    await waitFor(() => expect(title()).toBe('Orbit Labs › ORB-123'));
  });

  it('a long issue title is cut with an ellipsis', async () => {
    const long = 'word '.repeat(40);
    server({ 'GET /api/v1/orgs/:org/issues/:issue': () => ({ json: makeIssue(123, { title: long }) }) });
    renderApp('/orbit/issue/ORB-123');
    await waitFor(() => expect(title()).toMatch(/^Orbit Labs › ORB-123 word /));
    const item = title().slice('Orbit Labs › '.length);
    expect(item.endsWith('…')).toBe(true);
    // The identifier, a space and the cut title.
    expect([...item].length).toBeLessThanOrEqual('ORB-123 '.length + MAX_ITEM_TITLE);
  });

  it('follows the issue title when it is edited elsewhere (live)', async () => {
    let current = 'Fix the thing';
    server({
      'GET /api/v1/orgs/:org/issues/:issue': () => ({ json: makeIssue(123, { title: current }) }),
    });
    const client = testClient();
    renderApp('/orbit/issue/ORB-123', client);
    await waitFor(() => expect(title()).toBe('Orbit Labs › ORB-123 Fix the thing'));
    current = 'Fix the other thing';
    await act(() => client.invalidateQueries({ queryKey: keys.issueDetails('orbit') }));
    await waitFor(() => expect(title()).toBe('Orbit Labs › ORB-123 Fix the other thing'));
  });

  it('a page with no data yet shows the page name only (cycle list while teams load)', async () => {
    server();
    renderApp('/orbit/team/ORB/cycles');
    await waitFor(() => expect(title()).toBe('Orbit Labs › Cycles'));
  });

  it('the page title makes no request of its own', async () => {
    const api = server();
    renderApp('/orbit/settings/members');
    await waitFor(() => expect(title()).toBe('Orbit Labs › Settings › Members'));
    expect(api.calls.filter((c) => c.url.pathname === '/api/v1/orgs')).toHaveLength(0);
  });
});

describe('titles outside an org', () => {
  const unauth = { status: 401, json: { error: 'unauthenticated', code: 'unauthenticated' } };
  beforeEach(() => {
    mockApi({
      ...baseRoutes(),
      'GET /api/v1/me': () => unauth,
      'GET /api/v1/auth/session': () => unauth,
      'GET /api/v1/setup': () => ({ json: { setupRequired: true } }),
    });
  });

  it('a person in no organisation: Launchpad › No organisation', async () => {
    mockApi({
      ...baseRoutes(),
      'GET /api/v1/auth/session': () => ({
        json: {
          user: { id: 'u1', username: 'chen', name: 'Chen', email: 'c@e.com', isPlatformAdmin: false },
          orgs: [],
          csrfToken: 'csrf-test',
        },
      }),
    });
    renderApp('/');
    await waitFor(() => expect(title()).toBe('Launchpad › No organisation'));
  });

  it.each([
    ['/login', 'Launchpad › Log in'],
    ['/setup', 'Launchpad › Set up'],
    ['/signup', 'Launchpad › Sign up'],
    ['/invite', 'Launchpad › Join'],
  ])('%s', async (path, expected) => {
    renderApp(path);
    await waitFor(() => expect(title()).toBe(expected));
  });
});
