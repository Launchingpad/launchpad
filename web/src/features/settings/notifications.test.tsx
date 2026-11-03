import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { baseRoutes, mockApi, type Handler, type MockRequest } from '../../test/mockApi';
import { currentLocation, renderApp } from '../../test/render';
import { saveText } from './NotificationsPage';
import { ApiError } from '../../api/errors';

// LAUN-36: Settings > Notifications and the unsubscribe page (decisions-T13 #38, #25).

const EVENTS = ['assigned', 'mention', 'comment', 'state_change', 'new_issue', 'project_update_due'] as const;
const DEFAULTS: Record<string, { email: 'on' | 'off'; inbox: 'on' | 'off' }> = {
  assigned: { email: 'on', inbox: 'on' },
  mention: { email: 'on', inbox: 'on' },
  comment: { email: 'on', inbox: 'on' },
  state_change: { email: 'off', inbox: 'on' },
  new_issue: { email: 'off', inbox: 'on' },
  project_update_due: { email: 'on', inbox: 'on' },
};

type Val = 'on' | 'off' | null;
type Cols = { email: Val; inbox: Val; push?: Val };
interface Store {
  emailEnabled: boolean;
  includeExcerpt: boolean;
  mailAvailable: boolean;
  pushAvailable?: boolean;
  pushDevice?: boolean;
  follows: string[];
  global: Record<string, Cols>;
  team: Record<string, Cols>; // keyed by event, for team1
}

/** A tiny fake of the server's resolution: team, else global, else default; master off wins for email. */
function view(st: Store) {
  const out = (v: 'on' | 'off', source: string, ch: string) => ({
    value: ch === 'email' && !st.emailEnabled ? 'off' : v,
    source,
  });
  const dflt = (ev: string, ch: 'email' | 'inbox' | 'push') => (ch === 'push' ? 'off' : DEFAULTS[ev]![ch]);
  const globalLevel = (ev: string, ch: 'email' | 'inbox' | 'push') => {
    const g = st.global[ev]?.[ch] ?? null;
    return g ? out(g, 'global', ch) : out(dflt(ev, ch), 'default', ch);
  };
  const teamLevel = (ev: string, ch: 'email' | 'inbox' | 'push') => {
    const t = st.team[ev]?.[ch] ?? null;
    return t ? out(t, 'team', ch) : globalLevel(ev, ch);
  };
  return {
    emailEnabled: st.emailEnabled,
    includeExcerpt: st.includeExcerpt,
    mail: { available: st.mailAvailable },
    push: { available: st.pushAvailable ?? false, hasDevice: st.pushDevice ?? st.pushAvailable ?? false },
    follows: st.follows,
    events: EVENTS.map((ev) => ({
      event: ev,
      default: { ...DEFAULTS[ev], push: 'off' },
      global: {
        email: st.global[ev]?.email ?? null,
        inbox: st.global[ev]?.inbox ?? null,
        push: st.global[ev]?.push ?? null,
      },
      resolved: {
        email: globalLevel(ev, 'email'),
        inbox: globalLevel(ev, 'inbox'),
        push: globalLevel(ev, 'push'),
      },
      requiresFollow: ev === 'new_issue',
      // project_update_due has no team level (LAUN-196): the server lists no teams for it
      teams:
        ev === 'project_update_due'
          ? []
          : [
              {
                teamId: 'team1',
                teamKey: 'ORB',
                override: {
                  email: st.team[ev]?.email ?? null,
                  inbox: st.team[ev]?.inbox ?? null,
                  push: st.team[ev]?.push ?? null,
                },
                resolved: {
                  email: teamLevel(ev, 'email'),
                  inbox: teamLevel(ev, 'inbox'),
                  push: teamLevel(ev, 'push'),
                },
              },
            ],
    })),
  };
}

function server(
  opts: {
    role?: 'admin' | 'member';
    store?: Partial<Store>;
    routes?: Record<string, Handler>;
  } = {},
) {
  const st: Store = {
    emailEnabled: true,
    includeExcerpt: false,
    mailAvailable: true,
    follows: [],
    global: {},
    team: {},
    ...opts.store,
  };
  const api = mockApi({
    ...baseRoutes(),
    'GET /api/v1/auth/session': () => ({
      json: {
        user: { id: 'u1', username: 'chen', name: 'Chen', email: 'c@example.com', isPlatformAdmin: false },
        orgs: [{ id: 'o1', slug: 'orbit', name: 'Orbit', role: opts.role ?? 'member' }],
        csrfToken: 'csrf-test',
      },
    }),
    'GET /api/v1/orgs/:org/me/notification-settings': () => ({ json: view(st) }),
    'PUT /api/v1/orgs/:org/me/notification-settings': (r: MockRequest) => {
      const b = r.body as {
        emailEnabled?: boolean | null;
        includeExcerpt?: boolean | null;
        events?: Record<string, Record<string, Val>>;
      };
      if (b.emailEnabled !== undefined) st.emailEnabled = b.emailEnabled ?? true;
      if (b.includeExcerpt !== undefined) st.includeExcerpt = b.includeExcerpt ?? false;
      for (const [ev, cols] of Object.entries(b.events ?? {}))
        st.global[ev] = { email: null, inbox: null, ...st.global[ev], ...cols };
      return { json: view(st) };
    },
    'PUT /api/v1/orgs/:org/me/notification-settings/teams/:team': (r: MockRequest) => {
      const b = r.body as { events?: Record<string, Record<string, Val>> };
      for (const [ev, cols] of Object.entries(b.events ?? {}))
        st.team[ev] = { email: null, inbox: null, ...st.team[ev], ...cols };
      return { json: view(st) };
    },
    'PUT /api/v1/orgs/:org/me/teams/:team/follow': () => {
      st.follows = ['team1'];
      return { status: 204 };
    },
    'DELETE /api/v1/orgs/:org/me/teams/:team/follow': () => {
      st.follows = [];
      return { status: 204 };
    },
    ...opts.routes,
  });
  return { api, st };
}

const writes = (api: { calls: MockRequest[] }) => api.calls.filter((c) => c.method !== 'GET');

// LAUN-197: the Push channel shows only when the org has a push relay (the settings view says so).
describe('Settings, Notifications: Push channel (LAUN-197)', () => {
  it('has no Push column when the org has no relay', async () => {
    server({ role: 'member' });
    renderApp('/orbit/settings/notifications');
    expect(await screen.findByRole('radiogroup', { name: 'Assigned to me, email' })).toBeInTheDocument();
    expect(screen.queryByRole('radiogroup', { name: 'Assigned to me, push' })).not.toBeInTheDocument();
    expect(screen.queryByText(/Push wakes the Launchpad mobile app/)).not.toBeInTheDocument();
  });

  it('with a relay, shows Push off by default and saves On as a push patch only', async () => {
    const { api } = server({ role: 'member', store: { pushAvailable: true } });
    const user = userEvent.setup();
    renderApp('/orbit/settings/notifications');
    const group = await screen.findByRole('radiogroup', { name: 'Assigned to me, push' });
    expect(within(group).getByRole('radio', { name: 'Default (Off)' })).toBeChecked();
    expect(screen.getAllByText('Now Off (the default)').length).toBeGreaterThan(0);
    await user.click(within(group).getByRole('radio', { name: 'On' }));
    await waitFor(() => expect(writes(api)).toHaveLength(1));
    expect(writes(api)[0]!.body).toEqual({ events: { assigned: { push: 'on' } } });
    await waitFor(() =>
      expect(
        within(screen.getByRole('radiogroup', { name: 'Assigned to me, push' })).getByRole('radio', {
          name: 'On',
        }),
      ).toBeChecked(),
    );
    expect(screen.getByText(/Push wakes the Launchpad mobile app/)).toBeInTheDocument();
  });

  it('with a relay but no registered device, shows no Push radios and a note that links to Mobile devices', async () => {
    server({ role: 'member', store: { pushAvailable: true, pushDevice: false } });
    renderApp('/orbit/settings/notifications');
    expect(await screen.findByRole('radiogroup', { name: 'Assigned to me, email' })).toBeInTheDocument();
    expect(screen.queryByRole('radiogroup', { name: 'Assigned to me, push' })).not.toBeInTheDocument();
    expect(screen.queryByText(/Push wakes the Launchpad mobile app/)).not.toBeInTheDocument();
    expect(within(screen.getByRole('main')).getByRole('link', { name: 'Mobile devices' })).toHaveAttribute(
      'href',
      '/orbit/settings/mobile-devices',
    );
  });

  it('a team override has a Push choice too', async () => {
    const { api } = server({ role: 'member', store: { pushAvailable: true } });
    const user = userEvent.setup();
    renderApp('/orbit/settings/notifications');
    await user.click(await screen.findByRole('button', { name: /Override for ORB/ }));
    const teamGroups = await screen.findAllByRole('radiogroup', { name: 'Comments, push' });
    expect(teamGroups).toHaveLength(2); // the global row and the team row
    await user.click(within(teamGroups[1]!).getByRole('radio', { name: 'On' }));
    await waitFor(() => expect(writes(api)).toHaveLength(1));
    expect(writes(api)[0]!.url.pathname).toBe('/api/v1/orgs/orbit/me/notification-settings/teams/ORB');
    expect(writes(api)[0]!.body).toEqual({ events: { comment: { push: 'on' } } });
  });
});

describe('Settings, Notifications: access', () => {
  it('a plain member opens it, sees only Notifications in the sidebar, and not the admin pages', async () => {
    server({ role: 'member' });
    renderApp('/orbit/settings/notifications');
    expect(await screen.findByRole('heading', { name: 'Notifications' })).toBeInTheDocument();
    const nav = screen.getByRole('navigation', { name: 'Settings' });
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
    ]);
  });

  it('a member is turned away from an admin page', async () => {
    server({ role: 'member' });
    renderApp('/orbit/settings/organisation');
    expect(await screen.findByRole('heading', { name: 'You need admin access.' })).toBeInTheDocument();
  });

  it('the bare settings address takes a member to Notifications', async () => {
    server({ role: 'member' });
    renderApp('/orbit/settings');
    await screen.findByRole('heading', { name: 'Notifications' });
    expect(currentLocation.pathname).toBe('/orbit/settings/notifications');
  });

  it('the user menu offers Settings to a plain member', async () => {
    server({ role: 'member' });
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await user.click(await screen.findByRole('button', { name: /User menu/ }));
    await user.click(await screen.findByRole('menuitem', { name: 'Settings' }));
    expect(await screen.findByRole('heading', { name: 'Notifications' })).toBeInTheDocument();
  });

  it('an admin sees Notifications first in the sidebar, with the admin pages after it', async () => {
    server({ role: 'admin' });
    renderApp('/orbit/settings/notifications');
    await screen.findByRole('heading', { name: 'Notifications' });
    const nav = screen.getByRole('navigation', { name: 'Settings' });
    expect(within(nav).getAllByRole('link')[0]).toHaveTextContent('Notifications');
    expect(within(nav).getByRole('link', { name: 'Organisation' })).toBeInTheDocument();
  });
});

describe('Settings, Notifications: global section', () => {
  it('shows resolved values with their source, and the unset choice names the built-in value', async () => {
    server({ store: { global: { state_change: { email: 'on', inbox: null } } } });
    renderApp('/orbit/settings/notifications');
    const group = await screen.findByRole('group', { name: 'Status changes' });
    const stateEmail = within(group).getByRole('radiogroup', { name: 'Status changes, email' });
    expect(within(stateEmail).getByRole('radio', { name: 'On' })).toBeChecked();
    expect(within(stateEmail).getByRole('radio', { name: 'Default (Off)' })).not.toBeChecked();
    expect(within(group).getByText('Now On (your setting)')).toBeInTheDocument();
    expect(within(group).getByText('Now On (the default)')).toBeInTheDocument(); // inbox
    expect(within(group).getByText('Default (Off)')).toBeInTheDocument(); // email default
    const assigned = screen.getByRole('group', { name: 'Assigned to me' });
    const aEmail = within(assigned).getByRole('radiogroup', { name: 'Assigned to me, email' });
    expect(within(aEmail).getByRole('radio', { name: 'Default (On)' })).toBeChecked();
    expect(within(aEmail).getByRole('radio', { name: 'On' })).not.toBeChecked();
    expect(within(aEmail).getByRole('radio', { name: 'Off' })).not.toBeChecked();
    expect(within(assigned).getAllByText('Default (On)')).toHaveLength(2);
  });

  it('a change saves at once and the reply shows the new source; Default clears it again', async () => {
    const { api } = server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/notifications');
    const mEmail = await screen.findByRole('radiogroup', { name: 'Mentions, email' });
    await user.click(within(mEmail).getByRole('radio', { name: 'Off' }));
    await waitFor(() => expect(screen.getByText('Now Off (your setting)')).toBeInTheDocument());
    expect(writes(api).at(-1)).toMatchObject({
      method: 'PUT',
      body: { events: { mention: { email: 'off' } } },
    });
    expect(await screen.findByRole('status')).toHaveTextContent('Saved.');
    await user.click(within(mEmail).getByRole('radio', { name: 'Default (On)' }));
    await waitFor(() => expect(screen.queryByText('Now Off (your setting)')).not.toBeInTheDocument());
    expect(writes(api).at(-1)?.body).toEqual({ events: { mention: { email: null } } });
  });

  it('the master switch and the excerpt switch save; master off shows email as off with the reason', async () => {
    const { api } = server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/notifications');
    const master = await screen.findByRole('switch', { name: 'Email notifications' });
    expect(master).toHaveAttribute('aria-checked', 'true');
    await user.click(master);
    await waitFor(() => expect(master).toHaveAttribute('aria-checked', 'false'));
    expect(writes(api).at(-1)?.body).toEqual({ emailEnabled: false });
    expect(screen.getAllByText('Off: email is turned off above')).toHaveLength(6);
    const excerpt = screen.getByRole('switch', { name: 'Include a short excerpt' });
    await user.click(excerpt);
    await waitFor(() => expect(excerpt).toHaveAttribute('aria-checked', 'true'));
    expect(writes(api).at(-1)?.body).toEqual({ includeExcerpt: true });
  });

  it('shows the "cannot send email" note only when mail is not available', async () => {
    server({ store: { mailAvailable: false } });
    renderApp('/orbit/settings/notifications');
    expect(await screen.findByRole('note')).toHaveTextContent('This install cannot send email');
  });

  it('shows no note when mail works', async () => {
    server();
    renderApp('/orbit/settings/notifications');
    await screen.findByRole('heading', { name: 'Notifications' });
    expect(screen.queryByRole('note')).not.toBeInTheDocument();
  });
});

describe('Settings, Notifications: radio buttons (LAUN-136)', () => {
  it('each choice is a radio group of three radios, with no select left on the page', async () => {
    server();
    renderApp('/orbit/settings/notifications');
    await screen.findByRole('radiogroup', { name: 'Mentions, email' });
    expect(screen.queryByRole('combobox')).not.toBeInTheDocument();
    expect(screen.getAllByRole('radiogroup')).toHaveLength(12); // 6 events x email, inbox
    const g = screen.getByRole('radiogroup', { name: 'Mentions, inbox' });
    expect(within(g).getAllByRole('radio')).toHaveLength(3);
    // one name per group, so the arrows stay inside it
    const names = within(g)
      .getAllByRole('radio')
      .map((r) => (r as HTMLInputElement).name);
    expect(new Set(names).size).toBe(1);
    // and a different name in every group
    const all = screen
      .getAllByRole('radiogroup')
      .map((rg) => (within(rg).getAllByRole('radio')[0] as HTMLInputElement).name);
    expect(new Set(all).size).toBe(all.length);
    expect(names[0]).not.toBe('');
  });

  it('the arrow keys move inside a group and save the chosen value', async () => {
    const { api } = server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/notifications');
    const g = await screen.findByRole('radiogroup', { name: 'Mentions, inbox' });
    const [unset, on] = within(g).getAllByRole('radio');
    unset!.focus();
    await user.keyboard('{ArrowDown}');
    expect(on).toHaveFocus();
    expect(on).toBeChecked();
    await waitFor(() => expect(writes(api).at(-1)?.body).toEqual({ events: { mention: { inbox: 'on' } } }));
  });
});

describe('Settings, Notifications: teams', () => {
  it('follow persists: the switch calls follow, and an unfollow calls delete', async () => {
    const { api } = server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/notifications');
    const sw = await screen.findByRole('switch', { name: 'Follow Orbit' });
    expect(sw).toHaveAttribute('aria-checked', 'false');
    await user.click(sw);
    await waitFor(() => expect(sw).toHaveAttribute('aria-checked', 'true'));
    expect(writes(api).at(-1)).toMatchObject({ method: 'PUT' });
    expect(writes(api).at(-1)?.url.pathname).toBe('/api/v1/orgs/orbit/me/teams/ORB/follow');
    await user.click(sw);
    await waitFor(() => expect(sw).toHaveAttribute('aria-checked', 'false'));
    expect(writes(api).at(-1)).toMatchObject({ method: 'DELETE' });
  });

  it('a followed team shows as on when the page loads', async () => {
    server({ store: { follows: ['team1'] } });
    renderApp('/orbit/settings/notifications');
    expect(await screen.findByRole('switch', { name: 'Follow Orbit' })).toHaveAttribute(
      'aria-checked',
      'true',
    );
  });

  it('the team override panel leaves out the event that has no team level (LAUN-196)', async () => {
    server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/notifications');
    // The global section lists the event ...
    expect(await screen.findByRole('group', { name: 'Project updates due' })).toBeInTheDocument();
    await user.click(await screen.findByRole('button', { name: 'Override for ORB' }));
    const panel = screen.getByRole('region', { name: 'Overrides for Orbit' });
    // ... the team panel does not (its Off would be a 400).
    expect(within(panel).queryByRole('group', { name: 'Project updates due' })).toBeNull();
    expect(within(panel).getByRole('group', { name: 'Comments' })).toBeInTheDocument();
  });

  it('the override panel opens by keyboard, shows Inherit with its value, saves an override and Inherit clears it', async () => {
    const { api } = server({ store: { global: { comment: { email: 'off', inbox: null } } } });
    const user = userEvent.setup();
    renderApp('/orbit/settings/notifications');
    const toggle = await screen.findByRole('button', { name: 'Override for ORB' });
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    toggle.focus();
    await user.keyboard('{Enter}');
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    const panel = screen.getByRole('region', { name: 'Overrides for Orbit' });
    const comment = within(panel).getByRole('group', { name: 'Comments' });
    const sel = within(comment).getByRole('radiogroup', { name: 'Comments, email' });
    expect(within(sel).getByRole('radio', { name: 'Inherit (Off)' })).toBeChecked();
    expect(within(comment).getByText('Inherit (Off)')).toBeInTheDocument();
    expect(within(comment).getByText('Now Off (your setting)')).toBeInTheDocument();

    await user.click(within(sel).getByRole('radio', { name: 'On' }));
    await waitFor(() => expect(within(panel).getByText('Now On (set for this team)')).toBeInTheDocument());
    const last = writes(api).at(-1)!;
    expect(last.url.pathname).toBe('/api/v1/orgs/orbit/me/notification-settings/teams/ORB');
    expect(last.body).toEqual({ events: { comment: { email: 'on' } } });
    // the global section is untouched
    const globalComments = screen.getAllByRole('radiogroup', { name: 'Comments, email' })[0]!;
    expect(within(globalComments).getByRole('radio', { name: 'Off' })).toBeChecked();

    await user.click(
      within(within(panel).getByRole('radiogroup', { name: 'Comments, email' })).getByRole('radio', {
        name: 'Inherit (Off)',
      }),
    );
    await waitFor(() => expect(within(panel).getByText('Now Off (your setting)')).toBeInTheDocument());
    expect(writes(api).at(-1)?.body).toEqual({ events: { comment: { email: null } } });
  });
});

describe('Settings, Notifications: details (review round 1)', () => {
  it('the Now line is the description of its radio group, and New issues says it is for followed teams', async () => {
    server();
    renderApp('/orbit/settings/notifications');
    const sel = await screen.findByRole('radiogroup', { name: 'Mentions, email' });
    expect(sel).toHaveAccessibleDescription('Now On (the default)');
    const ni = screen.getByRole('radiogroup', { name: 'New issues, inbox' });
    expect(ni).toHaveAccessibleDescription('Now On (the default) Only for teams you follow');
    expect(screen.getAllByText('Only for teams you follow')).toHaveLength(2); // email and inbox
  });

  it('a failed teams load shows an error with a retry, not "no teams yet"', async () => {
    server({
      routes: {
        'GET /api/v1/orgs/:org/teams': () => ({ status: 500, json: { error: 'x', code: 'internal' } }),
      },
    });
    renderApp('/orbit/settings/notifications');
    expect(await screen.findByText('Could not load your teams')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Try again' })).toBeInTheDocument();
    expect(screen.queryByText('There are no teams yet.')).not.toBeInTheDocument();
  });
});

describe('Settings, Notifications: failures', () => {
  it('a failed save shows a fixed sentence, never the server text', async () => {
    const MARK = 'RAW-SERVER-TEXT-MARKER';
    server({
      routes: {
        'PUT /api/v1/orgs/:org/me/notification-settings': () => ({
          status: 500,
          json: { error: MARK, code: 'internal' },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/notifications');
    await user.click(await screen.findByRole('switch', { name: 'Email notifications' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not save. Try again.');
    expect(document.body.textContent).not.toContain(MARK);
  });

  it('a failed load shows a retry', async () => {
    server({
      routes: {
        'GET /api/v1/orgs/:org/me/notification-settings': () => ({
          status: 403,
          json: { error: 'x', code: 'forbidden' },
        }),
      },
    });
    renderApp('/orbit/settings/notifications');
    expect(await screen.findByRole('button', { name: 'Try again' })).toBeInTheDocument();
  });

  it('saveText maps statuses to fixed sentences', () => {
    expect(saveText(new ApiError(0, 'x', 'network'))).toContain('Could not reach the server');
    expect(saveText(new ApiError(404, 'x', 'not_found'))).toContain('team no longer exists');
    expect(saveText(new Error('boom'))).toBe('Could not save. Try again.');
  });
});

describe('Unsubscribe page', () => {
  const TOKEN = 'A'.repeat(43);
  const route = (status = 204) => ({
    'POST /api/v1/notifications/unsubscribe/:token': () => ({
      status,
      json: { error: 'RAW', code: 'bad_request' },
    }),
  });

  it('opening the page sends nothing, needs no sign-in, and keeps the token out of the title', async () => {
    const api = mockApi({
      ...route(),
      'GET /api/v1/auth/session': () => ({ status: 401, json: { error: 'x', code: 'unauthorized' } }),
    });
    renderApp(`/unsubscribe/${TOKEN}`);
    expect(await screen.findByRole('heading', { name: 'Turn off these emails?' })).toBeInTheDocument();
    expect(api.calls.filter((c) => c.method !== 'GET')).toEqual([]);
    expect(document.title).not.toContain(TOKEN);
  });

  it('the button sends one POST with the token in the path and no CSRF token, then says done', async () => {
    const api = mockApi(route());
    const user = userEvent.setup();
    renderApp(`/unsubscribe/${TOKEN}`);
    await user.click(await screen.findByRole('button', { name: 'Turn off these emails' }));
    expect(await screen.findByRole('heading', { name: 'Done' })).toBeInTheDocument();
    const posts = api.calls.filter((c) => c.method === 'POST');
    expect(posts).toHaveLength(1);
    expect(posts[0]!.url.pathname).toBe(`/api/v1/notifications/unsubscribe/${TOKEN}`);
    expect(posts[0]!.headers.get('X-CSRF-Token')).toBeNull();
    expect(document.body.textContent).not.toContain(TOKEN);
  });

  it('a link that is not well formed shows a calm message and sends nothing', async () => {
    const api = mockApi(route());
    renderApp('/unsubscribe/not-a-token');
    expect(await screen.findByRole('heading', { name: 'This link does not work' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Turn off these emails' })).not.toBeInTheDocument();
    expect(api.calls.filter((c) => c.method === 'POST')).toEqual([]);
  });

  it('a 400 from the server shows the same calm message, without its text', async () => {
    mockApi(route(400));
    const user = userEvent.setup();
    renderApp(`/unsubscribe/${TOKEN}`);
    await user.click(await screen.findByRole('button', { name: 'Turn off these emails' }));
    expect(await screen.findByRole('heading', { name: 'This link does not work' })).toBeInTheDocument();
    expect(document.body.textContent).not.toContain('RAW');
  });

  it('a 429 keeps the button so the person can try again', async () => {
    mockApi(route(429));
    const user = userEvent.setup();
    renderApp(`/unsubscribe/${TOKEN}`);
    await user.click(await screen.findByRole('button', { name: 'Turn off these emails' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Too many requests');
    expect(screen.getByRole('button', { name: 'Turn off these emails' })).toBeEnabled();
  });
});
