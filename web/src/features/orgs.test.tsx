import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { FakeEventSource } from '../test/fakes';
import { baseRoutes, makeIssue, mockApi } from '../test/mockApi';
import { renderApp } from '../test/render';

const TWO_ORGS = {
  items: [
    { id: 'o1', slug: 'orbit', name: 'Orbit', role: 'admin', createdAt: '2026-01-01T00:00:00Z' },
    { id: 'o2', slug: 'acme', name: 'Acme', role: 'member', createdAt: '2026-01-01T00:00:00Z' },
  ],
  nextCursor: null,
};

function twoOrgServer() {
  const data: Record<string, ReturnType<typeof makeIssue>[]> = {
    orbit: [makeIssue(1, { title: 'Orbit secret roadmap' })],
    acme: [makeIssue(7, { title: 'Acme invoice bug', identifier: 'ACM-7' })],
  };
  return mockApi({
    ...baseRoutes(),
    'GET /api/v1/auth/session': () => ({
      json: {
        user: { id: 'u1', username: 'chen', name: 'Chen', email: 'chen@example.com' },
        orgs: TWO_ORGS.items,
        csrfToken: 'csrf-test',
      },
    }),
    'GET /api/v1/orgs/:org/issues': (r) => ({ json: { items: data[r.params.org!] ?? [], nextCursor: null } }),
  });
}

describe('org switching', () => {
  it('shows only the chosen org, and every request names the org from the route', async () => {
    const api = twoOrgServer();
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    expect(await screen.findByText('Orbit secret roadmap')).toBeInTheDocument();

    const before = api.calls.length;
    await user.click(screen.getByRole('button', { name: 'Switch organisation' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Acme' }));

    expect(await screen.findByText('Acme invoice bug')).toBeInTheDocument();
    expect(screen.queryByText('Orbit secret roadmap')).not.toBeInTheDocument();

    const orgPaths = api.calls
      .slice(before)
      .map((c) => c.url.pathname)
      .filter((p) => p.startsWith('/api/v1/orgs/'));
    expect(orgPaths.length).toBeGreaterThan(0);
    // The org record itself (the sidebar logo, LAUN-22) is `/orgs/acme` with no trailing slash.
    for (const p of orgPaths)
      expect(p === '/api/v1/orgs/acme' || p.startsWith('/api/v1/orgs/acme/'), p).toBe(true);
  });

  it('opens one event stream per org and closes the old one on switch', async () => {
    twoOrgServer();
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByText('Orbit secret roadmap');
    await waitFor(() => expect(FakeEventSource.eventStreams).toHaveLength(1));
    expect(FakeEventSource.eventStreams[0]!.url).toContain('/orgs/orbit/events/stream');

    await user.click(screen.getByRole('button', { name: 'Switch organisation' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Acme' }));
    await screen.findByText('Acme invoice bug');
    await waitFor(() => expect(FakeEventSource.lastEvents!.url).toContain('/orgs/acme/events/stream'));
    expect(FakeEventSource.eventStreams[0]!.closed).toBe(true);
    expect(FakeEventSource.eventStreams.filter((s) => !s.closed)).toHaveLength(1);
  });

  it('does not leak one org into the other through the cache', async () => {
    twoOrgServer();
    const user = userEvent.setup();
    renderApp('/acme/issues');
    expect(await screen.findByText('Acme invoice bug')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Switch organisation' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Orbit' }));
    expect(await screen.findByText('Orbit secret roadmap')).toBeInTheDocument();
    expect(screen.queryByText('Acme invoice bug')).not.toBeInTheDocument();
  });

  it('an org you are not in is a clear not-found, with no data requests for it', async () => {
    const api = twoOrgServer();
    renderApp('/stranger/issues');
    expect(await screen.findByRole('heading', { name: 'Organisation not found' })).toBeInTheDocument();
    expect(api.calls.some((c) => c.url.pathname.includes('/orgs/stranger'))).toBe(false);
  });
});
