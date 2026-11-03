import { render, screen, waitFor, within } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { OrgProvider } from '../../api/org';
import { TeamCyclesControl } from './TeamCycles';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { baseRoutes, members, mockApi, team, type Handler } from '../../test/mockApi';
import { renderApp } from '../../test/render';

function server(
  opts: { role?: 'admin' | 'member'; platform?: boolean; routes?: Record<string, Handler> } = {},
) {
  return mockApi({
    ...baseRoutes(),
    'GET /api/v1/auth/session': () => ({
      json: {
        user: {
          id: 'u1',
          username: 'chen',
          name: 'Chen',
          email: 'chen@example.com',
          isPlatformAdmin: opts.platform ?? false,
        },
        orgs: [{ id: 'o1', slug: 'orbit', name: 'Orbit', role: opts.role ?? 'admin' }],
        csrfToken: 'csrf-test',
      },
    }),
    ...opts.routes,
  });
}

const settingsNav = () => screen.getByRole('navigation', { name: 'Settings' });

describe('settings entry points and gating', () => {
  it('an org admin sees Settings in the org menu and the user menu, and the Admin badge', async () => {
    server();
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    const orgMenu = await screen.findByRole('button', { name: 'Switch organisation' });
    await user.click(orgMenu);
    expect(await screen.findByRole('menuitem', { name: 'Settings' })).toBeInTheDocument();
    await user.keyboard('{Escape}');
    const userMenu = screen.getByRole('button', { name: 'User menu' });
    expect(within(userMenu).getByText('Admin')).toBeInTheDocument();
    await user.click(userMenu);
    await user.click(await screen.findByRole('menuitem', { name: 'Settings' }));
    expect(await screen.findByRole('heading', { name: 'Organisation' })).toBeInTheDocument();
    expect(settingsNav()).toBeInTheDocument();
  });

  it('a member has no Settings entry in the organisation menu, no badge, and Settings in the user menu (LAUN-36)', async () => {
    server({ role: 'member' });
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await user.click(await screen.findByRole('button', { name: 'Switch organisation' }));
    expect(screen.queryByRole('menuitem', { name: 'Settings' })).not.toBeInTheDocument();
    await user.keyboard('{Escape}');
    const userMenu = screen.getByRole('button', { name: 'User menu' });
    expect(within(userMenu).queryByText('Admin')).not.toBeInTheDocument();
    await user.click(userMenu);
    expect(await screen.findByRole('menuitem', { name: 'Sign out' })).toBeInTheDocument();
    expect(screen.getByRole('menuitem', { name: 'Settings' })).toBeInTheDocument();
  });

  it('a member who opens the settings address sees the notice and no navigation', async () => {
    server({ role: 'member' });
    renderApp('/orbit/settings/teams');
    const h = await screen.findByRole('heading', { name: 'You need admin access.' });
    expect(h).toHaveFocus();
    // LAUN-36: the sidebar holds Notifications only.
    expect(
      within(screen.getByRole('navigation', { name: 'Settings' }))
        .getAllByRole('link')
        .map((a) => a.textContent),
    ).toEqual([
      'Notifications',
      'Sessions',
      'Connected accounts',
      'Mobile devices',
      'Personal webhooks',
      'Appearance',
    ]);
    expect(screen.queryByRole('heading', { name: 'Teams' })).not.toBeInTheDocument();
  });

  it('the settings address alone opens the Organisation page', async () => {
    server();
    renderApp('/orbit/settings');
    expect(await screen.findByRole('heading', { name: 'Organisation' })).toBeInTheDocument();
  });

  it('the Platform item shows for a platform admin only', async () => {
    server({ platform: true });
    renderApp('/orbit/settings/organisation');
    await screen.findByRole('heading', { name: 'Organisation' });
    expect(within(settingsNav()).getByRole('link', { name: 'Platform' })).toBeInTheDocument();
  });

  it('an org admin who is not a platform admin has no Platform nav item', async () => {
    server();
    renderApp('/orbit/settings/organisation');
    await screen.findByRole('heading', { name: 'Organisation' });
    expect(within(settingsNav()).queryByRole('link', { name: 'Platform' })).not.toBeInTheDocument();
  });

  it('a direct visit to Platform by a non-platform admin shows the notice', async () => {
    server();
    renderApp('/orbit/settings/platform');
    expect(
      await screen.findByRole('heading', { name: 'You need platform admin access.' }),
    ).toBeInTheDocument();
    expect(screen.queryByText(/Mail and sign-up settings/)).not.toBeInTheDocument();
  });

  it('a platform admin who is only an org member reaches the Platform pages and nothing else', async () => {
    server({ platform: true, role: 'member' });
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await user.click(await screen.findByRole('button', { name: 'Switch organisation' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Settings' }));
    expect(await screen.findByRole('heading', { name: 'Platform' })).toBeInTheDocument();
    const nav = settingsNav();
    // LAUN-36: Notifications is open to every member.
    expect(
      within(nav)
        .getAllByRole('link')
        .map((a) => a.textContent),
    ).toEqual([
      'Notifications',
      'Sessions',
      'Connected accounts',
      'Mobile devices',
      'Personal webhooks',
      'Appearance',
      'Platform',
      'Backups',
    ]);
    expect(within(nav).getByRole('link', { name: 'Platform' })).toBeInTheDocument();
  });

  it('that person gets the notice on an organisation page, and no Admin badge', async () => {
    server({ platform: true, role: 'member' });
    renderApp('/orbit/settings/teams');
    expect(await screen.findByRole('heading', { name: 'You need admin access.' })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'Teams' })).not.toBeInTheDocument();
    expect(screen.queryByText('Admin')).not.toBeInTheDocument();
  });
});

describe('members page', () => {
  it('lists name, username, role and the agent tag', async () => {
    server();
    renderApp('/orbit/settings/members');
    expect(await screen.findByRole('heading', { name: 'Members' })).toHaveFocus();
    const table = await screen.findByRole('table', { name: 'Members' });
    const rows = within(table).getAllByRole('row');
    expect(rows).toHaveLength(members.length + 1);
    expect(within(rows[1]!).getByText('Admin')).toBeInTheDocument();
    expect(within(rows[2]!).getByText('Agent')).toBeInTheDocument();
    expect(within(rows[2]!).getByText('Member')).toBeInTheDocument();
  });

  it('shows an error with a retry when the list fails', async () => {
    server({
      routes: {
        'GET /api/v1/orgs/:org/users': () => ({ status: 500, json: { error: 'boom', code: 'internal' } }),
      },
    });
    renderApp('/orbit/settings/members');
    expect(await screen.findByText('Could not load members')).toBeInTheDocument();
  });
});

describe('teams page', () => {
  it('lists the teams', async () => {
    server();
    renderApp('/orbit/settings/teams');
    expect(await screen.findByRole('heading', { name: 'Teams' })).toHaveFocus();
    const table = await screen.findByRole('table', { name: 'Teams' });
    expect(within(table).getByText(team.key)).toBeInTheDocument();
    expect(within(table).getByText(team.name)).toBeInTheDocument();
  });

  it('shows an empty state when there are no teams', async () => {
    server({ routes: { 'GET /api/v1/orgs/:org/teams': () => ({ json: { items: [], nextCursor: null } }) } });
    renderApp('/orbit/settings/teams');
    expect(await screen.findByText('No teams yet')).toBeInTheDocument();
    expect(screen.queryByRole('table')).not.toBeInTheDocument();
  });

  it('upper-cases the key as typed, creates the team and adds it to the sidebar with no reload', async () => {
    const created: unknown[] = [];
    const list = [team];
    const api = server({
      routes: {
        'GET /api/v1/orgs/:org/teams': () => ({ json: { items: list, nextCursor: null } }),
        'POST /api/v1/orgs/:org/teams': (r) => {
          created.push(r.body);
          list.push({ ...team, id: 'team2', key: 'WEB', name: 'Web app' });
          return { status: 201, json: list[1] };
        },
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/teams');
    await screen.findByRole('table', { name: 'Teams' });
    const key = screen.getByLabelText('Team key');
    await user.type(key, 'web');
    expect(key).toHaveValue('WEB');
    await user.type(screen.getByLabelText('Team name'), 'Web app');
    await user.click(screen.getByRole('button', { name: 'Create team' }));
    expect(await screen.findByText('Team WEB is created.')).toBeInTheDocument();
    expect(created).toEqual([{ key: 'WEB', name: 'Web app' }]);
    // The settings sidebar replaces the app sidebar (LAUN-26), so the new team shows in the table here.
    const table = screen.getByRole('table', { name: 'Teams' });
    await waitFor(() => expect(within(table).getByText('Web app')).toBeInTheDocument());
    expect(screen.getByLabelText('Team key')).toHaveValue('');
    expect(api.calls.filter((c) => c.method === 'POST')).toHaveLength(1);
  });

  it('checks the fields before it sends anything and puts focus on the first bad one', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/teams');
    await screen.findByRole('table', { name: 'Teams' });
    await user.click(screen.getByRole('button', { name: 'Create team' }));
    expect(await screen.findByText(/Use 1 to 10 characters/)).toBeInTheDocument();
    expect(screen.getByLabelText('Team key')).toHaveFocus();
    await user.type(screen.getByLabelText('Team key'), 'x1');
    await user.click(screen.getByRole('button', { name: 'Create team' }));
    expect(await screen.findByText('Enter 1 to 200 characters.')).toBeInTheDocument();
    expect(screen.getByLabelText('Team name')).toHaveFocus();
    expect(api.calls.some((c) => c.method === 'POST')).toBe(false);
  });

  it('maps a 409 to an error on the key field', async () => {
    server({
      routes: {
        'POST /api/v1/orgs/:org/teams': () => ({
          status: 409,
          json: { error: 'team key taken', code: 'conflict' },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/teams');
    await screen.findByRole('table', { name: 'Teams' });
    await user.type(screen.getByLabelText('Team key'), 'orb');
    await user.type(screen.getByLabelText('Team name'), 'Again');
    await user.click(screen.getByRole('button', { name: 'Create team' }));
    expect(
      await screen.findByText('A team with this key exists already. Use another key.'),
    ).toBeInTheDocument();
    const key = screen.getByLabelText('Team key');
    expect(key).toHaveAttribute('aria-invalid', 'true');
    expect(key).toHaveFocus();
  });

  it('maps a 400 that starts with key or name to that field', async () => {
    let msg = 'key must be 1-10 characters, A-Z and 0-9, starting with a letter';
    server({
      routes: {
        'POST /api/v1/orgs/:org/teams': () => ({ status: 400, json: { error: msg, code: 'invalid' } }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/teams');
    await screen.findByRole('table', { name: 'Teams' });
    await user.type(screen.getByLabelText('Team key'), 'web');
    await user.type(screen.getByLabelText('Team name'), 'Web');
    await user.click(screen.getByRole('button', { name: 'Create team' }));
    await waitFor(() => expect(screen.getByLabelText('Team key')).toHaveAttribute('aria-invalid', 'true'));
    expect(screen.getByLabelText('Team key')).toHaveFocus();
    expect(screen.getByLabelText('Team name')).not.toHaveAttribute('aria-invalid', 'true');
    msg = 'name must be 1-200 characters';
    await user.click(screen.getByRole('button', { name: 'Create team' }));
    await waitFor(() => expect(screen.getByLabelText('Team name')).toHaveAttribute('aria-invalid', 'true'));
    expect(screen.getByLabelText('Team name')).toHaveFocus();
    expect(screen.getByLabelText('Team key')).not.toHaveAttribute('aria-invalid', 'true');
  });

  it('shows another failure in an alert', async () => {
    server({
      routes: {
        'POST /api/v1/orgs/:org/teams': () => ({
          status: 403,
          json: { error: 'forbidden', code: 'forbidden' },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/teams');
    await screen.findByRole('table', { name: 'Teams' });
    await user.type(screen.getByLabelText('Team key'), 'web');
    await user.type(screen.getByLabelText('Team name'), 'Web');
    await user.click(screen.getByRole('button', { name: 'Create team' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('You need admin access to create a team.');
  });
});

describe('settings navigation', () => {
  it('moves focus to the new page heading on a route change', async () => {
    server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/organisation');
    await screen.findByRole('heading', { name: 'Organisation' });
    await user.click(within(settingsNav()).getByRole('link', { name: 'Members' }));
    expect(await screen.findByRole('heading', { name: 'Members' })).toHaveFocus();
    expect(within(settingsNav()).getByRole('link', { name: 'Members' })).toHaveAttribute(
      'aria-current',
      'page',
    );
  });
});

describe('team menu in the sidebar (LAUN-15)', () => {
  it('an org admin opens Team settings from the team menu', async () => {
    server();
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    const btn = await screen.findByRole('button', { name: 'Orbit team options' });
    expect(btn).toHaveAttribute('aria-haspopup', 'menu');
    await user.click(btn);
    await user.click(await screen.findByRole('menuitem', { name: 'Team settings' }));
    expect(await screen.findByRole('heading', { name: 'Teams' })).toBeInTheDocument();
    expect(settingsNav()).toBeInTheDocument();
  });

  it('is reachable by keyboard', async () => {
    server();
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    const btn = await screen.findByRole('button', { name: 'Orbit team options' });
    btn.focus();
    await user.keyboard('{Enter}');
    expect(await screen.findByRole('menuitem', { name: 'Team settings' })).toHaveFocus();
    await user.keyboard('{Enter}');
    expect(await screen.findByRole('heading', { name: 'Teams' })).toBeInTheDocument();
  });

  it('a member sees no team menu', async () => {
    server({ role: 'member' });
    renderApp('/orbit/issues');
    expect(await screen.findByRole('link', { name: /Orbit/ })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /team options/ })).not.toBeInTheDocument();
  });

  it('a platform admin who is only an org member sees Settings but no team menu', async () => {
    server({ role: 'member', platform: true });
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    expect(await screen.findByRole('link', { name: /Orbit/ })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /team options/ })).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Switch organisation' }));
    expect(await screen.findByRole('menuitem', { name: 'Settings' })).toBeInTheDocument();
  });
});

describe('cycles switch (LAUN-3)', () => {
  const patches = (api: ReturnType<typeof server>) => api.calls.filter((c) => c.method === 'PATCH');

  it('has a labelled switch per team that is off, and turning on asks first', async () => {
    const list = [team, { ...team, id: 'team2', key: 'WEB', name: 'Web app', cyclesEnabled: true }];
    const api = server({
      routes: {
        'GET /api/v1/orgs/:org/teams': () => ({ json: { items: list, nextCursor: null } }),
        'PATCH /api/v1/orgs/:org/teams/:team': (r) => {
          list[0] = { ...list[0]!, cyclesEnabled: (r.body as { cyclesEnabled: boolean }).cyclesEnabled };
          return { json: list[0] };
        },
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/teams');
    const sw = await screen.findByRole('switch', { name: 'Cycles for ORB' });
    expect(sw).toHaveAttribute('aria-checked', 'false');
    expect(screen.getByRole('switch', { name: 'Cycles for WEB' })).toHaveAttribute('aria-checked', 'true');
    await user.click(sw);
    const dialog = await screen.findByRole('dialog', { name: 'Turn on cycles for ORB?' });
    expect(within(dialog).getByText(/Import your cycles from Linear first/)).toBeInTheDocument();
    expect(patches(api)).toHaveLength(0);
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(sw).toHaveAttribute('aria-checked', 'false');
    expect(patches(api)).toHaveLength(0);

    await user.click(sw);
    await user.click(await screen.findByRole('button', { name: 'Turn on' }));
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ cyclesEnabled: true });
    expect(patches(api)[0]!.params.team).toBe('ORB');
    await waitFor(() =>
      expect(screen.getByRole('switch', { name: 'Cycles for ORB' })).toHaveAttribute('aria-checked', 'true'),
    );
  });

  it('turns off with no dialog, and the note says what happens to the cycles', async () => {
    const api = server({
      routes: {
        'GET /api/v1/orgs/:org/teams': () => ({
          json: { items: [{ ...team, cyclesEnabled: true }], nextCursor: null },
        }),
        'PATCH /api/v1/orgs/:org/teams/:team': () => ({ json: { ...team, cyclesEnabled: false } }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/teams');
    await user.click(await screen.findByRole('switch', { name: 'Cycles for ORB' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ cyclesEnabled: false });
    expect(screen.getByText(/the cycles that exist stay and keep their dates/)).toBeInTheDocument();
  });

  it('moves at once and goes back with a clear error when the server refuses', async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => (release = r));
    server({
      routes: {
        'PATCH /api/v1/orgs/:org/teams/:team': async () => {
          await gate;
          return { status: 500, json: { error: 'boom', code: 'internal' } };
        },
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/teams');
    await user.click(await screen.findByRole('switch', { name: 'Cycles for ORB' }));
    await user.click(await screen.findByRole('button', { name: 'Turn on' }));
    // Optimistic: on before the server answers.
    await waitFor(() =>
      expect(screen.getByRole('switch', { name: 'Cycles for ORB' })).toHaveAttribute('aria-checked', 'true'),
    );
    release();
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('ORB: Could not turn on cycles. The switch is back as it was.');
    expect(screen.getByRole('switch', { name: 'Cycles for ORB' })).toHaveAttribute('aria-checked', 'false');
  });

  it('a member sees the state as text and no switch', () => {
    server({ role: 'member' });
    render(
      <QueryClientProvider client={new QueryClient()}>
        <OrgProvider slug="orbit">
          <TeamCyclesControl team={{ ...team, cyclesEnabled: true }} canEdit={false} onError={vi.fn()} />
        </OrgProvider>
      </QueryClientProvider>,
    );
    expect(screen.queryByRole('switch')).not.toBeInTheDocument();
    expect(screen.getByText('On')).toBeInTheDocument();
  });
});
