import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { apiBase } from '../../env';
import { baseRoutes, members, mockApi, type Handler, type MockRequest } from '../../test/mockApi';
import { renderApp } from '../../test/render';

// the Agents and API tokens pages (decisions-T11).

const SECRET = 'odt_Zx9-planted-secret-value-never-seen-again';
const MARKER = 'RAW-SERVER-TEXT-MARKER';
const USER_REF = { id: 'u1', username: 'chen', name: 'Chen' };
const CLAUDE_REF = { id: 'u2', username: 'claude', name: 'Claude' };

function tok(id: string, over: Record<string, unknown> = {}) {
  return {
    id,
    kind: 'pat',
    name: `token ${id}`,
    prefix: 'odt_abcdefgh',
    scopes: ['read', 'write'],
    user: USER_REF,
    createdAt: '2026-10-01T00:00:00Z',
    expiresAt: '2099-01-01T00:00:00Z',
    lastUsedAt: null,
    revokedAt: null,
    ...over,
  };
}

function created(over: Record<string, unknown> = {}) {
  return {
    status: 201,
    json: {
      token: tok('new', { name: 'mcp', user: CLAUDE_REF, expiresAt: '2027-10-25T12:00:00Z', ...over }),
      secret: SECRET,
    },
  };
}

function server(
  opts: { role?: 'admin' | 'member'; tokens?: unknown[]; routes?: Record<string, Handler> } = {},
) {
  const list = opts.tokens ?? [];
  return mockApi({
    ...baseRoutes(),
    'GET /api/v1/auth/session': () => ({
      json: {
        user: {
          id: 'u1',
          username: 'chen',
          name: 'Chen',
          email: 'chen@example.com',
          isPlatformAdmin: false,
        },
        orgs: [{ id: 'o1', slug: 'orbit', name: 'Orbit', role: opts.role ?? 'admin' }],
        csrfToken: 'csrf-test',
      },
    }),
    'GET /api/v1/orgs/:org/tokens': () => ({ json: { items: list, nextCursor: null } }),
    ...opts.routes,
  });
}

const posts = (api: { calls: MockRequest[] }, path: string) =>
  api.calls.filter((c) => c.method === 'POST' && c.url.pathname.endsWith(path));

const webStorage = () => JSON.stringify({ ...localStorage }) + JSON.stringify({ ...sessionStorage });

describe('navigation and role gating', () => {
  it('an admin sees the Agents and API tokens items', async () => {
    server();
    renderApp('/orbit/settings/organisation');
    const nav = await screen.findByRole('navigation', { name: 'Settings' });
    expect(within(nav).getByRole('link', { name: 'Agents' })).toHaveAttribute(
      'href',
      '/orbit/settings/agents',
    );
    expect(within(nav).getByRole('link', { name: 'API tokens' })).toHaveAttribute(
      'href',
      '/orbit/settings/tokens',
    );
  });

  it.each(['agents', 'tokens'])('a member who opens the %s address sees the notice only', async (page) => {
    const api = server({ role: 'member' });
    renderApp(`/orbit/settings/${page}`);
    expect(await screen.findByRole('heading', { name: 'You need admin access.' })).toHaveFocus();
    // LAUN-36: a member's sidebar holds Notifications and nothing else.
    const links = within(screen.getByRole('navigation', { name: 'Settings' })).getAllByRole('link');
    expect(links.map((a) => a.textContent)).toEqual([
      'Notifications',
      'Sessions',
      'Connected accounts',
      'Mobile devices',
      'Personal webhooks',
      'Appearance',
    ]);
    expect(api.calls.some((c) => c.url.pathname.endsWith('/tokens'))).toBe(false);
  });
});

describe('agents page', () => {
  it('lists the agents with the number of live tokens', async () => {
    server({
      tokens: [
        tok('a', { user: CLAUDE_REF }),
        tok('b', { user: CLAUDE_REF, revokedAt: '2026-10-02T00:00:00Z' }),
        tok('c', { user: CLAUDE_REF, expiresAt: '2020-01-01T00:00:00Z' }),
        tok('d', { user: USER_REF }),
      ],
    });
    renderApp('/orbit/settings/agents');
    expect(await screen.findByRole('heading', { name: 'Agents' })).toHaveFocus();
    const table = await screen.findByRole('table', { name: 'Agents' });
    const rows = within(table).getAllByRole('row');
    expect(rows).toHaveLength(2); // the header and claude: chen is no agent
    await waitFor(() => expect(within(rows[1]!).getAllByRole('cell')[3]).toHaveTextContent('1'));
    expect(within(rows[1]!).getByText('claude')).toBeInTheDocument();
  });

  it('shows an empty state with no agents', async () => {
    server({
      routes: {
        'GET /api/v1/orgs/:org/users': () => ({ json: { items: [members[0]], nextCursor: null } }),
      },
    });
    renderApp('/orbit/settings/agents');
    expect(await screen.findByText('No agents yet')).toBeInTheDocument();
  });

  it('checks the fields before it sends anything and puts focus on the first bad one', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/agents');
    await screen.findByRole('table', { name: 'Agents' });
    await user.click(screen.getByRole('button', { name: 'Create agent' }));
    expect(await screen.findByText(/Use 1 to 39 characters/)).toBeInTheDocument();
    expect(screen.getByLabelText('Username')).toHaveFocus();
    expect(screen.getByLabelText('Username')).toHaveAttribute('aria-invalid', 'true');
    await user.type(screen.getByLabelText('Username'), 'Bot-1');
    await user.click(screen.getByRole('button', { name: 'Create agent' }));
    expect(await screen.findByText('Enter 1 to 200 characters.')).toBeInTheDocument();
    expect(screen.getByLabelText('Name')).toHaveFocus();
    expect(posts(api, '/agents')).toHaveLength(0);
  });

  it('maps a 409 to the username field', async () => {
    server({
      routes: {
        'POST /api/v1/orgs/:org/agents': () => ({
          status: 409,
          json: { error: `a user with username "claude" exists already ${MARKER}`, code: 'conflict' },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/agents');
    await screen.findByRole('table', { name: 'Agents' });
    await user.type(screen.getByLabelText('Username'), 'claude');
    await user.type(screen.getByLabelText('Name'), 'Claude');
    await user.click(screen.getByRole('button', { name: 'Create agent' }));
    expect(await screen.findByText('This username is in use. Use another username.')).toBeInTheDocument();
    expect(screen.getByLabelText('Username')).toHaveFocus();
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('shows fixed text for an unexpected failure', async () => {
    server({
      routes: {
        'POST /api/v1/orgs/:org/agents': () => ({
          status: 500,
          json: { error: MARKER, code: 'internal_error' },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/agents');
    await screen.findByRole('table', { name: 'Agents' });
    await user.type(screen.getByLabelText('Username'), 'bot');
    await user.type(screen.getByLabelText('Name'), 'Bot');
    await user.click(screen.getByRole('button', { name: 'Create agent' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not create the agent. Try again.');
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('creates an agent, then its token, in one flow, and the secret stays in component state', async () => {
    const list = [...members];
    const api = server({
      routes: {
        'GET /api/v1/orgs/:org/users': () => ({ json: { items: list, nextCursor: null } }),
        'POST /api/v1/orgs/:org/agents': (r) => {
          const b = r.body as { username: string; name: string };
          const m = { ...members[1]!, id: 'u9', username: b.username, name: b.name };
          list.push(m);
          return { status: 201, json: m };
        },
        'POST /api/v1/orgs/:org/tokens': () =>
          created({ user: { id: 'u9', username: 'claude-web', name: 'Claude (web)' } }),
      },
    });
    const user = userEvent.setup();
    const { client } = renderApp('/orbit/settings/agents');
    await screen.findByRole('table', { name: 'Agents' });
    await user.type(screen.getByLabelText('Username'), 'Claude-Web');
    await user.type(screen.getByLabelText('Name'), 'Claude (web)');
    await user.click(screen.getByRole('button', { name: 'Create agent' }));
    expect(posts(api, '/agents')[0]!.body).toEqual({
      username: 'claude-web',
      name: 'Claude (web)',
      teamAccess: 'all',
    });
    expect(await screen.findByText('Agent Claude (web) is created.')).toBeInTheDocument();
    expect(await screen.findByRole('cell', { name: 'claude-web' })).toBeInTheDocument();

    await user.click(screen.getByRole('button', { name: 'Create a token for this agent' }));
    const form = await screen.findByRole('dialog', { name: 'Create a token for Claude (web)' });
    // An agent: read and write, 365 days; the owner is fixed.
    expect(within(form).getByLabelText('Read and write')).toBeChecked();
    expect(within(form).getByLabelText('Days until it expires')).toHaveValue('365');
    expect(within(form).queryByRole('combobox')).not.toBeInTheDocument();
    await user.type(within(form).getByLabelText('Token name'), 'mcp');
    await user.click(within(form).getByRole('button', { name: 'Create token' }));

    expect(posts(api, '/tokens')[0]!.body).toEqual({
      name: 'mcp',
      scopes: ['read', 'write'],
      expiresInDays: 365,
      user: 'claude-web',
    });
    const secretDialog = await screen.findByRole('dialog', { name: 'Copy your token' });
    expect(within(secretDialog).getByTestId('new-token')).toHaveTextContent(SECRET);
    // The secret is in no query or mutation cache, no storage and no URL.
    // (The agent mutation stays in the cache; it holds no secret.)
    expect(
      JSON.stringify(
        client
          .getMutationCache()
          .getAll()
          .map((m) => m.state),
      ),
    ).not.toContain(SECRET);
    expect(
      JSON.stringify(
        client
          .getQueryCache()
          .getAll()
          .map((q) => q.state.data),
      ),
    ).not.toContain(SECRET);
    expect(webStorage()).not.toContain(SECRET);
    expect(window.location.href).not.toContain(SECRET);

    await user.click(within(secretDialog).getByLabelText('I saved the token in a safe place.'));
    await user.click(within(secretDialog).getByRole('button', { name: 'Done' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain(SECRET);
    const later = api.calls.slice(api.calls.indexOf(posts(api, '/tokens')[0]!) + 1);
    expect(JSON.stringify(later.map((c) => [c.url.href, c.body, [...c.headers.entries()]]))).not.toContain(
      SECRET,
    );
  });

  it('the Create a token button in the table opens the form for that agent', async () => {
    server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/agents');
    await user.click(await screen.findByRole('button', { name: 'Create a token for Claude' }));
    const form = await screen.findByRole('dialog', { name: 'Create a token for Claude' });
    expect(within(form).getByLabelText('Token name')).toHaveFocus();
    await user.click(within(form).getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });
});

describe('agents page: the token form right after the create', () => {
  it('has the agent as owner, with write and 365 days, before the member list refetches', async () => {
    // The members list never shows the new agent: the refetch has not landed.
    const api = server({
      routes: {
        'POST /api/v1/orgs/:org/agents': (r) => ({
          status: 201,
          json: { ...members[1]!, id: 'u9', ...(r.body as object) },
        }),
        'POST /api/v1/orgs/:org/tokens': () => created(),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/agents');
    await screen.findByRole('table', { name: 'Agents' });
    await user.type(screen.getByLabelText('Username'), 'fresh-bot');
    await user.type(screen.getByLabelText('Name'), 'Fresh Bot');
    await user.click(screen.getByRole('button', { name: 'Create agent' }));
    await user.click(await screen.findByRole('button', { name: 'Create a token for this agent' }));
    const form = await screen.findByRole('dialog', { name: 'Create a token for Fresh Bot' });
    expect(within(form).getByText('fresh-bot')).toBeInTheDocument();
    expect(within(form).getByLabelText('Read and write')).toBeChecked();
    expect(within(form).getByLabelText('Days until it expires')).toHaveValue('365');
    await user.type(within(form).getByLabelText('Token name'), 'mcp');
    await user.click(within(form).getByRole('button', { name: 'Create token' }));
    await screen.findByRole('dialog', { name: 'Copy your token' });
    expect(posts(api, '/tokens')[0]!.body).toEqual({
      name: 'mcp',
      scopes: ['read', 'write'],
      expiresInDays: 365,
      user: 'fresh-bot',
    });
  });
});

describe('API tokens page', () => {
  it('lists live tokens with their facts and no secret; revoked and expired tokens are hidden', async () => {
    server({
      tokens: [
        tok('a', { name: 'laptop', scopes: ['read'], lastUsedAt: '2026-10-20T10:00:00Z' }),
        tok('b', { name: 'mcp', user: CLAUDE_REF, expiresAt: '2027-01-02T00:00:00Z' }),
        tok('c', { name: 'old', revokedAt: '2026-10-02T00:00:00Z' }),
        tok('d', { name: 'stale', expiresAt: '2020-01-01T00:00:00Z' }),
      ],
    });
    renderApp('/orbit/settings/tokens');
    expect(await screen.findByRole('heading', { name: 'API tokens' })).toHaveFocus();
    const table = await screen.findByRole('table', { name: 'API tokens' });
    const rows = within(table).getAllByRole('row');
    expect(rows).toHaveLength(3);
    const a = within(rows[1]!);
    expect(a.getByText('laptop')).toBeInTheDocument();
    expect(a.getByText('Read only')).toBeInTheDocument();
    expect(a.getByText('2026-10-01')).toBeInTheDocument();
    expect(a.getByText('2026-10-20')).toBeInTheDocument();
    const b = within(rows[2]!);
    expect(b.getByText('Agent')).toBeInTheDocument();
    expect(b.getByText('Read and write')).toBeInTheDocument();
    expect(b.getByText('2027-01-02')).toBeInTheDocument();
    expect(b.getByText('Never')).toBeInTheDocument();
    expect(screen.queryByText('old')).not.toBeInTheDocument();
    expect(screen.queryByText('stale')).not.toBeInTheDocument();
  });

  it('shows an empty state and a retry on a load error', async () => {
    server({ tokens: [] });
    const { unmount } = renderApp('/orbit/settings/tokens');
    expect(await screen.findByText('No tokens yet')).toBeInTheDocument();
    unmount();
    server({
      routes: {
        'GET /api/v1/orgs/:org/tokens': () => ({ status: 500, json: { error: MARKER, code: 'internal' } }),
      },
    });
    renderApp('/orbit/settings/tokens');
    expect(await screen.findByText('Could not load tokens')).toBeInTheDocument();
  });

  it('makes a token for me: 90 days, read only, no user field; owner changes the defaults', async () => {
    const api = server({
      routes: { 'POST /api/v1/orgs/:org/tokens': () => created({ user: USER_REF, name: 'laptop' }) },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/tokens');
    await screen.findByText('No tokens yet');
    expect(screen.getByLabelText('Read only')).toBeChecked();
    expect(screen.getByLabelText('Days until it expires')).toHaveValue('90');
    // Pick the agent: the defaults follow (read and write, 365)...
    await user.selectOptions(screen.getByLabelText('Owner'), 'claude');
    expect(screen.getByLabelText('Read and write')).toBeChecked();
    expect(screen.getByLabelText('Days until it expires')).toHaveValue('365');
    // ...and a value the person typed is kept.
    await user.clear(screen.getByLabelText('Days until it expires'));
    await user.type(screen.getByLabelText('Days until it expires'), '30');
    await user.click(screen.getByLabelText('Read only'));
    await user.selectOptions(screen.getByLabelText('Owner'), 'chen');
    expect(screen.getByLabelText('Read only')).toBeChecked();
    expect(screen.getByLabelText('Days until it expires')).toHaveValue('30');
    await user.type(screen.getByLabelText('Token name'), '  laptop ');
    await user.click(screen.getByRole('button', { name: 'Create token' }));
    expect(await screen.findByRole('dialog', { name: 'Copy your token' })).toBeInTheDocument();
    expect(posts(api, '/tokens')[0]!.body).toEqual({ name: 'laptop', scopes: ['read'], expiresInDays: 30 });
  });

  it('makes a token for an agent from the picker', async () => {
    const api = server({ routes: { 'POST /api/v1/orgs/:org/tokens': () => created() } });
    const user = userEvent.setup();
    renderApp('/orbit/settings/tokens');
    await screen.findByText('No tokens yet');
    await user.selectOptions(screen.getByLabelText('Owner'), 'claude');
    await user.type(screen.getByLabelText('Token name'), 'mcp');
    await user.click(screen.getByRole('button', { name: 'Create token' }));
    await screen.findByRole('dialog', { name: 'Copy your token' });
    expect(posts(api, '/tokens')[0]!.body).toEqual({
      name: 'mcp',
      scopes: ['read', 'write'],
      expiresInDays: 365,
      user: 'claude',
    });
  });

  it('checks the name and the days before it sends anything', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/tokens');
    await screen.findByText('No tokens yet');
    await user.click(screen.getByRole('button', { name: 'Create token' }));
    expect(await screen.findByText('Enter 1 to 200 characters.')).toBeInTheDocument();
    expect(screen.getByLabelText('Token name')).toHaveFocus();
    await user.type(screen.getByLabelText('Token name'), 'x');
    for (const bad of ['366', '0', '', '1.5', 'abc', '-3']) {
      const days = screen.getByLabelText('Days until it expires');
      await user.clear(days);
      if (bad) await user.type(days, bad);
      await user.click(screen.getByRole('button', { name: 'Create token' }));
      expect(await screen.findByText('Enter a whole number from 1 to 365.')).toBeInTheDocument();
      expect(days).toHaveFocus();
      expect(days).toHaveAttribute('aria-invalid', 'true');
    }
    expect(posts(api, '/tokens')).toHaveLength(0);
  });

  it.each([
    [400, 'name must be 1-200 characters', 'Token name', 'Enter 1 to 200 characters.'],
    [400, 'expiresInDays must be 1-365', 'Days until it expires', 'Enter a whole number from 1 to 365.'],
  ])('maps a %i that starts with "%s" to the field', async (status, error, label, text) => {
    server({
      routes: {
        'POST /api/v1/orgs/:org/tokens': () => ({ status, json: { error, code: 'validation_failed' } }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/tokens');
    await screen.findByText('No tokens yet');
    await user.type(screen.getByLabelText('Token name'), 'x');
    await user.click(screen.getByRole('button', { name: 'Create token' }));
    expect(await screen.findByText(text)).toBeInTheDocument();
    expect(screen.getByLabelText(label)).toHaveFocus();
  });

  it.each([
    [400, 'user "x" is not a member', 'The server did not accept this token. Check the fields.'],
    [403, 'a web session cannot make a token with the admin scope', 'You cannot make this token.'],
    [500, MARKER, 'Could not make the token. Try again.'],
  ])('shows fixed text for a %i and never the server text', async (status, error, text) => {
    server({
      routes: {
        'POST /api/v1/orgs/:org/tokens': () => ({ status, json: { error: `${error} ${MARKER}`, code: 'x' } }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/tokens');
    await screen.findByText('No tokens yet');
    await user.type(screen.getByLabelText('Token name'), 'x');
    await user.click(screen.getByRole('button', { name: 'Create token' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(text);
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('shows the wait for a 429', async () => {
    server({
      routes: {
        'POST /api/v1/orgs/:org/tokens': () => ({
          status: 429,
          headers: { 'Retry-After': '300' },
          json: { error: 'too many', code: 'rate_limited' },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/tokens');
    await screen.findByText('No tokens yet');
    await user.type(screen.getByLabelText('Token name'), 'x');
    await user.click(screen.getByRole('button', { name: 'Create token' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Try again in 5 minutes.');
  });

  describe('the copy-once screen', () => {
    async function open() {
      const api = server({
        routes: {
          'POST /api/v1/orgs/:org/tokens': () => created(),
          'GET /api/v1/orgs/:org/teams': () => ({
            json: { items: [{ id: 't1', key: 'ORB', name: 'Orbit' }], nextCursor: null },
          }),
        },
      });
      const user = userEvent.setup();
      const { client } = renderApp('/orbit/settings/tokens');
      await screen.findByText('No tokens yet');
      await user.type(screen.getByLabelText('Token name'), 'mcp');
      await user.click(screen.getByRole('button', { name: 'Create token' }));
      const dialog = await screen.findByRole('dialog', { name: 'Copy your token' });
      return { api, user, dialog, client };
    }

    it('warns, copies, and needs the confirm box before Done', async () => {
      const { user, dialog } = await open();
      expect(within(dialog).getByRole('alert')).toHaveTextContent(
        'Copy this token now. It is not shown again.',
      );
      expect(within(dialog).getByTestId('new-token')).toHaveTextContent(SECRET);
      expect(within(dialog).getByRole('button', { name: 'Copy token' })).toHaveFocus();
      const done = within(dialog).getByRole('button', { name: 'Done' });
      expect(done).toBeDisabled();
      await user.click(within(dialog).getByRole('button', { name: 'Copy token' }));
      expect(await navigator.clipboard.readText()).toBe(SECRET);
      expect(within(dialog).getByText('Copied.')).toBeInTheDocument();
      await user.click(within(dialog).getByLabelText('I saved the token in a safe place.'));
      expect(done).toBeEnabled();
      await user.click(done);
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      expect(document.body.textContent).not.toContain(SECRET);
    });

    it('cannot be closed by Escape or the close button before the box is ticked', async () => {
      const { user, dialog } = await open();
      await user.keyboard('{Escape}');
      expect(screen.getByRole('dialog', { name: 'Copy your token' })).toBeInTheDocument();
      expect(within(dialog).getByText(/Tick the box when you saved the token/)).toBeInTheDocument();
      await user.click(within(dialog).getByRole('button', { name: 'Close' }));
      expect(screen.getByRole('dialog', { name: 'Copy your token' })).toBeInTheDocument();
      await user.click(within(dialog).getByLabelText('I saved the token in a safe place.'));
      await user.keyboard('{Escape}');
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });

    it('asks the browser to confirm before the page closes, only while the secret is on screen', async () => {
      const { user, dialog } = await open();
      const during = new Event('beforeunload', { cancelable: true });
      window.dispatchEvent(during);
      expect(during.defaultPrevented).toBe(true);
      await user.click(within(dialog).getByLabelText('I saved the token in a safe place.'));
      await user.click(within(dialog).getByRole('button', { name: 'Done' }));
      const after = new Event('beforeunload', { cancelable: true });
      window.dispatchEvent(after);
      expect(after.defaultPrevented).toBe(false);
    });

    it('keeps the secret out of storage, the URL, the caches and every other request', async () => {
      const { api, user, dialog, client } = await open();
      expect(webStorage()).not.toContain(SECRET);
      expect(window.location.href).not.toContain(SECRET);
      expect(client.getMutationCache().getAll()).toHaveLength(0);
      expect(
        JSON.stringify(
          client
            .getQueryCache()
            .getAll()
            .map((q) => q.state.data),
        ),
      ).not.toContain(SECRET);
      await user.click(within(dialog).getByLabelText('I saved the token in a safe place.'));
      await user.click(within(dialog).getByRole('button', { name: 'Done' }));
      const later = api.calls.slice(api.calls.indexOf(posts(api, '/tokens')[0]!) + 1);
      expect(JSON.stringify(later.map((c) => [c.url.href, c.body, [...c.headers.entries()]]))).not.toContain(
        SECRET,
      );
      expect(later.some((c) => c.headers.get('Authorization'))).toBe(false);
    });

    it('shows the MCP setup with the origin, the org and the first team, and never the token', async () => {
      const { dialog } = await open();
      expect(within(dialog).getByRole('heading', { name: 'MCP setup' })).toBeInTheDocument();
      const env = within(dialog).getByLabelText('MCP environment variables');
      expect(env).toHaveTextContent(`LAUNCHPAD_URL=${apiBase()}`);
      expect(env).toHaveTextContent('LAUNCHPAD_TOKEN=<paste the token>');
      expect(env).toHaveTextContent('LAUNCHPAD_ORG=orbit');
      await waitFor(() => expect(env).toHaveTextContent('LAUNCHPAD_TEAM=ORB'));
      expect(env).not.toHaveTextContent(SECRET);
      expect(window.location.origin).toBe(new URL(apiBase()).origin);
    });

    it('refetches the list after Done and shows the new token', async () => {
      const list: unknown[] = [];
      server({
        tokens: list,
        routes: {
          'GET /api/v1/orgs/:org/tokens': () => ({ json: { items: list, nextCursor: null } }),
          'POST /api/v1/orgs/:org/tokens': () => {
            list.push(tok('new', { name: 'mcp', user: CLAUDE_REF }));
            return created();
          },
        },
      });
      const user = userEvent.setup();
      renderApp('/orbit/settings/tokens');
      await screen.findByText('No tokens yet');
      await user.type(screen.getByLabelText('Token name'), 'mcp');
      await user.click(screen.getByRole('button', { name: 'Create token' }));
      const dialog = await screen.findByRole('dialog', { name: 'Copy your token' });
      await user.click(within(dialog).getByLabelText('I saved the token in a safe place.'));
      await user.click(within(dialog).getByRole('button', { name: 'Done' }));
      expect(await screen.findByRole('table', { name: 'API tokens' })).toBeInTheDocument();
      expect(screen.getByText('mcp')).toBeInTheDocument();
    });
  });

  describe('revoke', () => {
    const one = [tok('a', { name: 'laptop' })];

    it('asks first; Cancel changes nothing', async () => {
      const api = server({ tokens: one });
      const user = userEvent.setup();
      renderApp('/orbit/settings/tokens');
      await user.click(await screen.findByRole('button', { name: 'Revoke laptop' }));
      const dialog = await screen.findByRole('dialog', { name: 'Revoke this token?' });
      expect(dialog).toHaveTextContent('stops working at once');
      await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      expect(api.calls.some((c) => c.method === 'DELETE')).toBe(false);
    });

    it('revokes after the confirm and the list refetches', async () => {
      let revoked = false;
      const api = server({
        routes: {
          'GET /api/v1/orgs/:org/tokens': () => ({
            json: { items: revoked ? [] : one, nextCursor: null },
          }),
          'DELETE /api/v1/orgs/:org/tokens/:token': () => {
            revoked = true;
            return { status: 204 };
          },
        },
      });
      const user = userEvent.setup();
      renderApp('/orbit/settings/tokens');
      await user.click(await screen.findByRole('button', { name: 'Revoke laptop' }));
      await user.click(
        within(await screen.findByRole('dialog')).getByRole('button', { name: 'Revoke token' }),
      );
      expect(await screen.findByText('Token laptop is revoked.')).toBeInTheDocument();
      expect(await screen.findByText('No tokens yet')).toBeInTheDocument();
      const del = api.calls.filter((c) => c.method === 'DELETE');
      expect(del).toHaveLength(1);
      expect(del[0]!.url.pathname).toBe('/api/v1/orgs/orbit/tokens/a');
      expect(del[0]!.headers.get('X-CSRF-Token')).toBe('csrf-test');
    });

    it('shows fixed text when the revoke fails and keeps the dialog open', async () => {
      server({
        tokens: one,
        routes: {
          'DELETE /api/v1/orgs/:org/tokens/:token': () => ({
            status: 500,
            json: { error: MARKER, code: 'internal' },
          }),
        },
      });
      const user = userEvent.setup();
      renderApp('/orbit/settings/tokens');
      await user.click(await screen.findByRole('button', { name: 'Revoke laptop' }));
      const dialog = await screen.findByRole('dialog');
      await user.click(within(dialog).getByRole('button', { name: 'Revoke token' }));
      expect(await within(dialog).findByRole('alert')).toHaveTextContent(
        'Could not revoke the token. Try again.',
      );
      expect(document.body.textContent).not.toContain(MARKER);
    });
  });
});
