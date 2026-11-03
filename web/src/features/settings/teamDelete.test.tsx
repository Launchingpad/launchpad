import { renderHook, screen, waitFor, within } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { ReactNode } from 'react';
import { OrgProvider } from '../../api/org';
import { keys } from '../../api/keys';
import { useDeleteTeam } from '../../api/queries';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { callsTo, joinServer, page } from '../../test/join';
import { mockApi, baseRoutes, team } from '../../test/mockApi';
import { renderApp } from '../../test/render';

// LAUN-191: delete a team from Settings, Teams.
describe('delete a team', () => {
  it('asks first, sends DELETE and the row leaves the table', async () => {
    let rows = [team, { ...team, id: 'team2', key: 'AAA', name: 'Alpha' }];
    const api = joinServer({
      routes: {
        'GET /api/v1/orgs/:org/teams': () => ({ json: page(rows) }),
        'DELETE /api/v1/orgs/:org/teams/:team': (r) => {
          rows = rows.filter((t) => t.key !== r.params.team);
          return { status: 204 };
        },
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/teams');
    await user.click(await screen.findByRole('button', { name: 'Delete team AAA' }));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByRole('button', { name: 'Cancel' })).toHaveFocus();
    expect(callsTo(api, 'DELETE', '/teams/AAA')).toHaveLength(0);
    await user.click(within(dialog).getByRole('button', { name: 'Delete team' }));
    await waitFor(() =>
      expect(screen.queryByRole('button', { name: 'Delete team AAA' })).not.toBeInTheDocument(),
    );
    expect(callsTo(api, 'DELETE', '/teams/AAA')).toHaveLength(1);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('shows the 409 sentence of the server and keeps the dialog and the row', async () => {
    const api = joinServer({
      routes: {
        'GET /api/v1/orgs/:org/teams': () => ({ json: page([team]) }),
        'DELETE /api/v1/orgs/:org/teams/:team': () => ({
          status: 409,
          json: {
            error: 'team ORB still has 3 issue(s); a team that has issues cannot be deleted',
            code: 'team_in_use',
          },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/teams');
    await user.click(await screen.findByRole('button', { name: 'Delete team ORB' }));
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete team' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('team ORB still has 3 issue(s)');
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    expect(callsTo(api, 'DELETE', '/teams/ORB')).toHaveLength(1);
  });

  it('a member sees no Delete button', async () => {
    joinServer({
      role: 'member',
      routes: { 'GET /api/v1/orgs/:org/teams': () => ({ json: page([team]) }) },
    });
    renderApp('/orbit/settings/teams');
    await screen.findByText('You need admin access.');
    expect(screen.queryByRole('button', { name: /Delete team/ })).not.toBeInTheDocument();
  });

  it('Escape closes the dialog without a request', async () => {
    const api = joinServer({
      routes: { 'GET /api/v1/orgs/:org/teams': () => ({ json: page([team]) }) },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/teams');
    await user.click(await screen.findByRole('button', { name: 'Delete team ORB' }));
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(callsTo(api, 'DELETE', '/teams/ORB')).toHaveLength(0);
  });

  it('refreshes the teams, labels, cycles and project caches after a delete (they cascade)', async () => {
    document.cookie = 'odt_csrf=csrf-test; path=/';
    mockApi({
      ...baseRoutes(),
      'DELETE /api/v1/orgs/:org/teams/:team': () => ({ status: 204 }),
    });
    const qc = new QueryClient();
    const stale = [
      keys.teams('orbit'),
      keys.labels('orbit'),
      keys.cycles('orbit', 'AAA'),
      keys.projectsAll('orbit'),
      keys.project('orbit', 'p1'),
      keys.issues('orbit'),
    ];
    for (const k of stale) qc.setQueryData(k, {});
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={qc}>
        <OrgProvider slug="orbit">{children}</OrgProvider>
      </QueryClientProvider>
    );
    const { result } = renderHook(() => useDeleteTeam(), { wrapper });
    await result.current.mutateAsync('AAA');
    await waitFor(() => expect(qc.getQueryState(keys.teams('orbit'))?.isInvalidated).toBe(true));
    for (const k of stale) expect(qc.getQueryState(k)?.isInvalidated, k.join('/')).toBe(true);
  });
});
