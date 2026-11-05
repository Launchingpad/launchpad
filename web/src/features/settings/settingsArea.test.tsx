import { render, screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { describe, expect, it, vi } from 'vitest';
import { baseRoutes, mockApi, type Handler } from '../../test/mockApi';
import { apiBase } from '../../env';
import { currentLocation, renderApp } from '../../test/render';
import { SettingsCard, SettingsRow, SettingsSection, SettingsToggle } from './SettingsParts';
import { searchSettings, settingsPages } from './settingsPages';

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

describe('shared settings parts (LAUN-26)', () => {
  it('sections have h2 (the page h1 and focus are checked on the MCP page) and a row has a label and a control', () => {
    render(
      <MemoryRouter>
        <>
          <SettingsSection title="General" description="The basics.">
            <SettingsCard>
              <SettingsRow label="Language" description="Shown in menus." labelFor="lang">
                <select id="lang">
                  <option>English</option>
                </select>
              </SettingsRow>
              <SettingsRow label="More" to="/orbit/settings/more" />
            </SettingsCard>
          </SettingsSection>
        </>
      </MemoryRouter>,
    );
    expect(screen.getByRole('region', { name: 'General' })).toBeInTheDocument();
    expect(screen.getByLabelText('Language')).toBeInTheDocument();
    expect(screen.getByText('Shown in menus.')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'More' })).toHaveAttribute('href', '/orbit/settings/more');
  });

  it('a toggle is a switch that reports its state and calls back with the next one', async () => {
    const onChange = vi.fn();
    render(<SettingsToggle checked={false} onChange={onChange} label="Send on Enter" />);
    const sw = screen.getByRole('switch', { name: 'Send on Enter' });
    expect(sw).toHaveAttribute('aria-checked', 'false');
    await userEvent.setup().click(sw);
    expect(onChange).toHaveBeenCalledWith(true);
  });
});

describe('searchSettings', () => {
  it('finds a page by name and by a row label, and nothing for an empty query', () => {
    expect(searchSettings(settingsPages, '  ')).toEqual([]);
    expect(searchSettings(settingsPages, 'tok').map((h) => h.page.path)).toContain('tokens');
    const hit = searchSettings(settingsPages, 'postmark');
    expect(hit).toHaveLength(1);
    expect(hit[0]).toMatchObject({ page: { path: 'platform' }, via: 'postmark' });
    expect(searchSettings(settingsPages, 'zzz')).toEqual([]);
    // LAUN-3: the Cycles switch is on the Teams page.
    expect(searchSettings(settingsPages, 'cycles')[0]).toMatchObject({
      page: { path: 'teams' },
      via: 'cycles',
    });
  });
});

describe('Products in the settings search (LAUN-66)', () => {
  it('"product" and "assign teams" find the Products page, by name and by keyword', () => {
    expect(searchSettings(settingsPages, 'product')[0]).toMatchObject({ page: { path: 'products' } });
    expect(searchSettings(settingsPages, 'assign teams')).toEqual([
      { page: expect.objectContaining({ path: 'products' }), via: 'assign teams' },
    ]);
  });

  it('the sidebar search offers Products to an admin and a click opens the page', async () => {
    server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/organisation');
    await user.type(await screen.findByRole('searchbox', { name: 'Search settings' }), 'assign teams');
    const nav = screen.getByRole('navigation', { name: 'Settings' });
    expect(within(nav).getByRole('link', { name: /Products/ })).toHaveAttribute(
      'href',
      '/orbit/settings/products',
    );
  });

  it('a plain member is not offered Products', async () => {
    server({ role: 'member' });
    const user = userEvent.setup();
    renderApp('/orbit/settings/notifications');
    await user.type(await screen.findByRole('searchbox', { name: 'Search settings' }), 'product');
    expect(screen.getByText('No settings match.')).toBeInTheDocument();
  });
});

describe('settings sidebar', () => {
  it('replaces the app sidebar, has Back to app, and groups the pages', async () => {
    server({ platform: true });
    renderApp('/orbit/settings/organisation');
    await screen.findByRole('heading', { name: 'Organisation' });
    expect(screen.queryByRole('navigation', { name: 'Primary' })).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: /Back to app/ })).toHaveAttribute('href', '/orbit/issues');
    const nav = screen.getByRole('navigation', { name: 'Settings' });
    const admin = within(nav).getByRole('group', { name: 'Administration' });
    expect(
      within(admin)
        .getAllByRole('link')
        .map((a) => a.textContent),
    ).toEqual([
      'Get started',
      'Organisation',
      'Teams',
      'Products',
      'Agent guidance',
      'Chat services',
      'Mobile push',
      'Project statuses',
      'Project labels',
      'Members',
      'Sign-ups',
      'Agents',
      'API tokens',
      'Integrations',
      'Git workflow',
      'MCP',
      'Import from Linear',
    ]);
    expect(
      within(within(nav).getByRole('group', { name: 'Platform' })).getByRole('link', { name: 'Platform' }),
    ).toBeInTheDocument();
    expect(within(nav).getByRole('link', { name: 'Organisation' })).toHaveAttribute('aria-current', 'page');
  });

  it('Back to app returns to the app sidebar', async () => {
    server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/organisation');
    await user.click(await screen.findByRole('link', { name: /Back to app/ }));
    expect(await screen.findByRole('navigation', { name: 'Primary' })).toBeInTheDocument();
    expect(screen.queryByRole('navigation', { name: 'Settings' })).not.toBeInTheDocument();
  });

  it('search lists pages, finds one by a row label, and says when nothing matches', async () => {
    server({ platform: true });
    const user = userEvent.setup();
    renderApp('/orbit/settings/organisation');
    const box = await screen.findByRole('searchbox', { name: 'Search settings' });
    await user.type(box, 'postmark');
    const nav = screen.getByRole('navigation', { name: 'Settings' });
    const link = within(nav).getByRole('link', { name: /Platform/ });
    expect(link).toHaveAttribute('href', '/orbit/settings/platform');
    expect(within(nav).queryByRole('link', { name: 'Teams' })).not.toBeInTheDocument();
    await user.clear(box);
    await user.type(box, 'zzzz');
    expect(screen.getByText('No settings match.')).toBeInTheDocument();
    await user.keyboard('{Escape}');
    expect(box).toHaveValue('');
    expect(within(nav).getByRole('link', { name: 'Teams' })).toBeInTheDocument();
  });

  it('search never offers a page the person may not open', async () => {
    server({ platform: false });
    const user = userEvent.setup();
    renderApp('/orbit/settings/organisation');
    await user.type(await screen.findByRole('searchbox', { name: 'Search settings' }), 'postmark');
    expect(screen.getByText('No settings match.')).toBeInTheDocument();
  });

  it('a platform admin who is not an org admin sees only the Platform pages, and old URLs still work', async () => {
    server({ role: 'member', platform: true });
    renderApp('/orbit/settings/platform');
    await screen.findByRole('heading', { name: 'Platform' });
    const nav = screen.getByRole('navigation', { name: 'Settings' });
    // LAUN-36: and Notifications, which every member has.
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
      'Move server',
      'Backups',
    ]);
  });
});

describe('MCP page (LAUN-26)', () => {
  it('shows the connection, links to agents and tokens, and an entry that holds no secret', async () => {
    server();
    renderApp('/orbit/settings/mcp');
    expect(await screen.findByRole('heading', { level: 1, name: 'MCP' })).toHaveFocus();
    expect(screen.getByText('LAUNCHPAD_ORG')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: /Make an agent/ })).toHaveAttribute(
      'href',
      '/orbit/settings/agents',
    );
    expect(screen.getByRole('link', { name: /Make a token/ })).toHaveAttribute(
      'href',
      '/orbit/settings/tokens',
    );
    const entry = screen.getByLabelText('MCP configuration');
    expect(entry).toHaveTextContent(`"LAUNCHPAD_URL": "${apiBase()}"`);
    expect(entry).toHaveTextContent('"LAUNCHPAD_ORG": "orbit"');
    expect(entry).toHaveTextContent('${LAUNCHPAD_TOKEN}');
    expect(entry.textContent).not.toMatch(/odt_/);
  });

  it('a member cannot open it', async () => {
    server({ role: 'member' });
    renderApp('/orbit/settings/mcp');
    expect(await screen.findByRole('heading', { name: 'You need admin access.' })).toBeInTheDocument();
  });
});

describe('bare settings address', () => {
  it('goes to Organisation for an org admin', async () => {
    server();
    renderApp('/orbit/settings');
    expect(await screen.findByRole('heading', { level: 1, name: 'Organisation' })).toBeInTheDocument();
    expect(currentLocation.pathname).toBe('/orbit/settings/organisation');
  });

  it('goes to Platform for a platform admin who is only a member', async () => {
    server({ role: 'member', platform: true });
    renderApp('/orbit/settings');
    expect(await screen.findByRole('heading', { level: 1, name: 'Platform' })).toBeInTheDocument();
    expect(currentLocation.pathname).toBe('/orbit/settings/platform');
  });
});

describe('SettingsRow label rule', () => {
  it('a row with a control and labelFor ties the label to it; the MCP rows hold no form control', () => {
    render(
      <MemoryRouter>
        <SettingsRow label="Name" labelFor="n">
          <input id="n" />
        </SettingsRow>
      </MemoryRouter>,
    );
    expect(screen.getByLabelText('Name')).toBeInstanceOf(HTMLInputElement);
  });
});
