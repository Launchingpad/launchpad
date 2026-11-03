import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { baseRoutes, mockApi, type Handler, type MockRequest } from '../../test/mockApi';
import { renderApp, testClient } from '../../test/render';
import { currentCsrf, getSessionEpoch } from '../../api/http';
import { keys } from '../../api/keys';
import { searchSettings, settingsPages } from './settingsPages';

// LAUN-160: Settings > Sessions (LAUN-130 routes).

const MARKER = 'RAW-SERVER-TEXT-MARKER';
const ID1 = '11111111-1111-4111-8111-111111111111';
const ID2 = '22222222-2222-4222-8222-222222222222';

function ses(id: string, over: Record<string, unknown> = {}) {
  return {
    id,
    kind: 'session',
    name: 'web session',
    createdAt: '2026-10-01T00:00:00Z',
    lastUsedAt: '2026-10-02T00:00:00Z',
    expiresAt: '2026-11-01T00:00:00Z',
    current: false,
    ...over,
  };
}
const LIST = [ses(ID1, { current: true }), ses(ID2, { kind: 'native', name: 'native session' })];

function server(routes: Record<string, Handler> = {}, role: 'admin' | 'member' = 'member') {
  const state = { list: LIST as unknown[], signedOut: false };
  const api = mockApi({
    ...baseRoutes(),
    'GET /api/v1/auth/session': () =>
      state.signedOut
        ? { status: 401, json: { error: 'x', code: 'unauthorized' } }
        : {
            json: {
              user: {
                id: 'u1',
                username: 'chen',
                name: 'Chen',
                email: 'c@example.com',
                isPlatformAdmin: false,
              },
              orgs: [{ id: 'o1', slug: 'orbit', name: 'Orbit', role }],
              csrfToken: 'csrf-test',
            },
          },
    'GET /api/v1/me/sessions': () => ({ json: { items: state.list } }),
    ...routes,
  });
  return { api, state };
}

const calls = (api: { calls: MockRequest[] }, method: string, suffix: string) =>
  api.calls.filter((c) => c.method === method && c.url.pathname.endsWith(suffix));

describe('settings registry', () => {
  it('Sessions is an Account page for every member and searchable', () => {
    const p = settingsPages.find((x) => x.path === 'sessions')!;
    expect(p.group).toBe('Account');
    expect(p.access).toBe('member');
    expect(searchSettings(settingsPages, 'log out everywhere').map((h) => h.page.path)).toContain('sessions');
  });
});

describe('Sessions page', () => {
  it('lists rows with kind labels, dates and the This device mark on the current one only', async () => {
    server();
    renderApp('/orbit/settings/sessions');
    const table = await screen.findByRole('table', { name: 'Your login sessions' });
    const rows = within(table).getAllByRole('row').slice(1);
    expect(rows).toHaveLength(2);
    expect(within(rows[0]!).getByText('This device')).toBeInTheDocument();
    expect(within(rows[0]!).getByText('Web')).toBeInTheDocument();
    expect(within(rows[0]!).getByText('2026-10-01')).toBeInTheDocument();
    expect(within(rows[1]!).queryByText('This device')).not.toBeInTheDocument();
    expect(within(rows[1]!).getByText('App')).toBeInTheDocument();
    // The nav entry is there for a plain member.
    const nav = screen.getByRole('navigation', { name: 'Settings' });
    expect(within(nav).getByRole('link', { name: 'Sessions' })).toHaveAttribute(
      'href',
      '/orbit/settings/sessions',
    );
  });

  it('shows the empty state', async () => {
    const { state } = server();
    state.list = [];
    renderApp('/orbit/settings/sessions');
    expect(await screen.findByText('No sessions')).toBeInTheDocument();
  });

  it('an agent (403) gets a fixed sentence and no raw server text', async () => {
    server({
      'GET /api/v1/me/sessions': () => ({ status: 403, json: { error: MARKER, code: 'forbidden' } }),
    });
    renderApp('/orbit/settings/sessions');
    expect(
      await screen.findByText('Sessions are for people. An agent has no login sessions.'),
    ).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
    expect(screen.queryByRole('table')).not.toBeInTheDocument();
  });

  it('a load error shows a fixed sentence and Try again refetches', async () => {
    let n = 0;
    const { api } = server({
      'GET /api/v1/me/sessions': () => {
        n++;
        return n === 1
          ? { status: 500, json: { error: MARKER, code: 'internal_error' } }
          : { json: { items: LIST } };
      },
    });
    renderApp('/orbit/settings/sessions');
    expect(await screen.findByText('Could not load sessions')).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
    await userEvent.setup().click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByRole('table')).toBeInTheDocument();
    expect(calls(api, 'GET', '/me/sessions')).toHaveLength(2);
  });

  it('revoking another session sends DELETE with the id and refetches the list', async () => {
    const { api, state } = server({
      'DELETE /api/v1/me/sessions/:id': (r) => {
        state.list = LIST.filter((s) => (s as { id: string }).id !== r.params.id);
        return { status: 204 };
      },
    });
    renderApp('/orbit/settings/sessions');
    const user = userEvent.setup();
    await user.click(
      await screen.findByRole('button', {
        name: /Revoke: app session created 2026-10-01, last used 2026-10-02/,
      }),
    );
    await waitFor(() => expect(screen.getAllByRole('row')).toHaveLength(2));
    const del = calls(api, 'DELETE', `/me/sessions/${ID2}`);
    expect(del).toHaveLength(1);
    expect(del[0]!.headers.get('X-CSRF-Token')).toBe('csrf-test');
    expect(calls(api, 'GET', '/me/sessions').length).toBeGreaterThanOrEqual(2);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('revoking the current session warns first, and only the confirm sends the request and logs out', async () => {
    const { api, state } = server({
      'DELETE /api/v1/me/sessions/:id': () => {
        state.signedOut = true;
        return { status: 204 };
      },
    });
    renderApp('/orbit/settings/sessions');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: /Log out this device/ }));
    const dlg = await screen.findByRole('dialog');
    expect(within(dlg).getByText(/logged out at once/)).toBeInTheDocument();
    expect(calls(api, 'DELETE', '/me/sessions/' + ID1)).toHaveLength(0);
    // Cancel sends nothing.
    await user.click(within(dlg).getByRole('button', { name: 'Cancel' }));
    expect(calls(api, 'DELETE', '/me/sessions/' + ID1)).toHaveLength(0);
    await user.click(screen.getByRole('button', { name: /Log out this device/ }));
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Log out' }));
    expect(await screen.findByRole('heading', { name: 'Sign in to Launchpad 🚀' })).toBeInTheDocument();
    expect(calls(api, 'DELETE', '/me/sessions/' + ID1)).toHaveLength(1);
  });

  it('Log out everywhere else confirms, posts, and shows the count revoked', async () => {
    const { api } = server({
      'POST /api/v1/me/sessions/revoke-others': () => ({ json: { revoked: 3 } }),
    });
    renderApp('/orbit/settings/sessions');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Log out everywhere else' }));
    expect(calls(api, 'POST', '/revoke-others')).toHaveLength(0);
    const dlg = await screen.findByRole('dialog');
    await user.click(within(dlg).getByRole('button', { name: 'Log out everywhere else' }));
    expect(await screen.findByText('Logged out 3 other sessions.')).toBeInTheDocument();
    const post = calls(api, 'POST', '/revoke-others');
    expect(post).toHaveLength(1);
    expect(post[0]!.headers.get('X-CSRF-Token')).toBe('csrf-test');
    // Still signed in.
    expect(screen.getByRole('table')).toBeInTheDocument();
    expect(calls(api, 'GET', '/me/sessions').length).toBeGreaterThanOrEqual(2);
  });

  it('a zero count is said in words', async () => {
    server({ 'POST /api/v1/me/sessions/revoke-others': () => ({ json: { revoked: 0 } }) });
    renderApp('/orbit/settings/sessions');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Log out everywhere else' }));
    await user.click(
      within(await screen.findByRole('dialog')).getByRole('button', { name: 'Log out everywhere else' }),
    );
    expect(await screen.findByText('No other sessions were signed in.')).toBeInTheDocument();
  });

  it('a 404 on revoke says the session is gone, with fixed text', async () => {
    server({
      'DELETE /api/v1/me/sessions/:id': () => ({ status: 404, json: { error: MARKER, code: 'not_found' } }),
    });
    renderApp('/orbit/settings/sessions');
    await userEvent.setup().click(await screen.findByRole('button', { name: /Revoke: app session/ }));
    expect(await screen.findByText('This session is already gone. The list is updated.')).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('a 429 on revoke-others keeps the dialog open with a fixed sentence', async () => {
    server({
      'POST /api/v1/me/sessions/revoke-others': () => ({
        status: 429,
        json: { error: MARKER, code: 'rate_limited' },
      }),
    });
    renderApp('/orbit/settings/sessions');
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Log out everywhere else' }));
    await user.click(
      within(await screen.findByRole('dialog')).getByRole('button', { name: 'Log out everywhere else' }),
    );
    const dlg = await screen.findByRole('dialog');
    expect(within(dlg).getByText('Too many requests. Wait a moment and try again.')).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('two sessions of one kind created on one day have different button names', async () => {
    const { state } = server();
    state.list = [
      ses(ID1, { current: true }),
      ses(ID2, { lastUsedAt: '2026-10-03T00:00:00Z' }),
      ses('33333333-3333-4333-8333-333333333333', { lastUsedAt: '2026-10-04T00:00:00Z' }),
    ];
    renderApp('/orbit/settings/sessions');
    await screen.findByRole('table');
    const names = screen
      .getAllByRole('button', { name: /^(Revoke|Log out this device):/ })
      .map((b) => b.getAttribute('aria-label'));
    expect(names).toHaveLength(3);
    expect(new Set(names).size).toBe(3);
  });

  it('row buttons are disabled while a revoke is pending: no second DELETE', async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => (release = r));
    const { api } = server({
      'DELETE /api/v1/me/sessions/:id': async () => {
        await gate;
        return { status: 204 };
      },
    });
    renderApp('/orbit/settings/sessions');
    const user = userEvent.setup();
    const btn = await screen.findByRole('button', { name: /Revoke: app session/ });
    await user.click(btn);
    await waitFor(() => expect(btn).toBeDisabled());
    expect(screen.getByRole('button', { name: /Log out this device/ })).toBeDisabled();
    await user.click(btn);
    release();
    await waitFor(() => expect(btn).not.toBeDisabled());
    expect(calls(api, 'DELETE', '/me/sessions/' + ID2)).toHaveLength(1);
  });

  it('after revoking another row, focus moves to the status line', async () => {
    server({ 'DELETE /api/v1/me/sessions/:id': () => ({ status: 204 }) });
    renderApp('/orbit/settings/sessions');
    await userEvent.setup().click(await screen.findByRole('button', { name: /Revoke: app session/ }));
    const status = await screen.findByText(/^Logged out the app session/);
    await waitFor(() => expect(status).toHaveFocus());
  });

  it('revoking the current session clears the CSRF token, bumps the epoch and removes cached data', async () => {
    const client = testClient();
    const { state } = server({
      'DELETE /api/v1/me/sessions/:id': () => {
        state.signedOut = true;
        return { status: 204 };
      },
    });
    renderApp('/orbit/settings/sessions', client);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: /Log out this device/ }));
    expect(currentCsrf()).toBe('csrf-test');
    expect(client.getQueryData(keys.mySessions)).toBeDefined();
    expect(client.getQueryData(keys.teams('orbit'))).toBeDefined();
    const epoch = getSessionEpoch();
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Log out' }));
    expect(await screen.findByRole('heading', { name: 'Sign in to Launchpad 🚀' })).toBeInTheDocument();
    expect(currentCsrf()).toBeUndefined();
    expect(getSessionEpoch()).toBeGreaterThan(epoch);
    expect(client.getQueryData(keys.mySessions)).toBeUndefined();
    expect(client.getQueryData(keys.teams('orbit'))).toBeUndefined();
    expect(client.getQueryData(keys.session)).toBeNull();
  });
});
