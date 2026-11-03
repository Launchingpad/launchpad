import { renderHook, screen, waitFor, within } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { ReactNode } from 'react';
import { OrgProvider } from '../../api/org';
import { keys } from '../../api/keys';
import { useRenameTeam } from '../../api/queries';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { baseRoutes, mockApi, team } from '../../test/mockApi';
import { renderApp } from '../../test/render';

// LAUN-99: rename a team from Settings, Teams. The key is fixed; the sidebar follows.
describe('rename a team', () => {
  it('sends the new name, closes, and the table and sidebar show it', async () => {
    let current = { ...team };
    let sent: unknown = null;
    mockApi({
      ...baseRoutes(),
      'GET /api/v1/orgs/:org/teams': () => ({ json: { items: [current], nextCursor: null } }),
      'PATCH /api/v1/orgs/:org/teams/:team': (r) => {
        sent = r.body;
        current = { ...current, name: (r.body as { name: string }).name };
        return { json: current };
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/teams');
    await user.click(await screen.findByRole('button', { name: 'Rename team ORB' }));
    const dialog = await screen.findByRole('dialog');
    const input = within(dialog).getByLabelText('Team name');
    await user.clear(input);
    await user.type(input, '  Orbit Apps ');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    expect(
      await within(await screen.findByRole('table', { name: 'Teams' })).findByText('Orbit Apps'),
    ).toBeInTheDocument();
    expect(sent).toEqual({ name: 'Orbit Apps' });
    // Back in the app, the sidebar shows the new name (it reads the same teams query).
    await user.click(screen.getByRole('link', { name: /Back to app/ }));
    expect(
      await within(await screen.findByLabelText('Sidebar')).findByText('Orbit Apps'),
    ).toBeInTheDocument();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('refuses an empty name before any call and shows a fixed sentence on a server error', async () => {
    let calls = 0;
    mockApi({
      ...baseRoutes(),
      'PATCH /api/v1/orgs/:org/teams/:team': () => {
        calls++;
        return { status: 403, json: { error: 'RAW', code: 'forbidden' } };
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/teams');
    await user.click(await screen.findByRole('button', { name: 'Rename team ORB' }));
    const dialog = await screen.findByRole('dialog');
    const input = within(dialog).getByLabelText('Team name');
    await user.clear(input);
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('Enter 1 to 200 characters.');
    expect(calls).toBe(0);
    await user.type(input, 'New');
    await user.click(within(dialog).getByRole('button', { name: 'Save' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(
      'You need admin access to rename a team.',
    );
    expect(within(dialog).queryByText('RAW')).not.toBeInTheDocument();
  });

  it('a member gets no Rename button', async () => {
    const base = baseRoutes();
    mockApi({
      ...base,
      'GET /api/v1/auth/session': (r) => {
        const out = base['GET /api/v1/auth/session']!(r) as { json: { orgs: { role: string }[] } };
        out.json.orgs[0]!.role = 'member';
        return out;
      },
    });
    renderApp('/orbit/settings/teams');
    await screen.findByText('You need admin access.');
    expect(screen.queryByRole('button', { name: /Rename team/ })).not.toBeInTheDocument();
  });

  it('refreshes product team names and issue details, not only the teams list', async () => {
    document.cookie = 'odt_csrf=csrf-test; path=/';
    mockApi({
      ...baseRoutes(),
      'PATCH /api/v1/orgs/:org/teams/:team': () => ({ json: { ...team, name: 'Renamed' } }),
    });
    const qc = new QueryClient();
    qc.setQueryData(keys.teams('orbit'), [team]);
    qc.setQueryData(keys.products('orbit'), []);
    qc.setQueryData(keys.products('orbit').concat(['alpha']), {});
    qc.setQueryData(keys.issue('orbit', 'ORB-1'), {});
    qc.setQueryData(keys.labels('orbit'), []);
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={qc}>
        <OrgProvider slug="orbit">{children}</OrgProvider>
      </QueryClientProvider>
    );
    const { result } = renderHook(() => useRenameTeam(), { wrapper });
    await result.current.mutateAsync({ team: 'ORB', name: 'Renamed' });
    await waitFor(() => expect(qc.getQueryState(keys.products('orbit'))?.isInvalidated).toBe(true));
    expect(qc.getQueryState(keys.products('orbit').concat(['alpha']))?.isInvalidated).toBe(true);
    expect(qc.getQueryState(keys.issue('orbit', 'ORB-1'))?.isInvalidated).toBe(true);
    expect(qc.getQueryState(keys.labels('orbit'))?.isInvalidated).toBe(false);
  });
});
