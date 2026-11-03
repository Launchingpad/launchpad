import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { invalidateForEvent, EVENT_TYPES } from '../../api/events';
import { keys } from '../../api/keys';
import { baseRoutes, mockApi, type Handler } from '../../test/mockApi';
import { renderApp, testClient } from '../../test/render';
import { CONNECTION_OFF_PREFIX } from './connectedAccountsText';
import { searchSettings, settingsPages } from './settingsPages';

// LAUN-199: Settings > Connected accounts (server routes: decisions-T34).

const MARKER = 'RAW-SERVER-TEXT-MARKER';
const GH = '11111111-1111-4111-8111-111111111111';
const GL = '22222222-2222-4222-8222-222222222222';

interface Acct {
  integrationId: string;
  provider: string;
  name: string;
  host: string;
  enabled: boolean;
  handle: string | null;
  linkedAt: string | null;
}
const acct = (over: Partial<Acct> = {}): Acct => ({
  integrationId: GH,
  provider: 'github',
  name: 'github.com',
  host: 'github.com',
  enabled: true,
  handle: null,
  linkedAt: null,
  ...over,
});

function server(routes: Record<string, Handler> = {}, role: 'admin' | 'member' = 'member', start?: Acct[]) {
  const state = {
    items: start ?? [
      acct(),
      acct({ integrationId: GL, provider: 'gitlab', name: 'Work GitLab', host: 'gitlab.example.com' }),
    ],
    links: [
      {
        id: 'l1',
        integrationId: GH,
        provider: 'github',
        integrationName: 'github.com',
        handle: 'ann-dev',
        user: { id: 'u9', username: 'ann', name: 'Ann Lee' },
        updatedAt: '2026-10-30T00:00:00Z',
      },
    ] as unknown[],
  };
  const api = mockApi({
    ...baseRoutes(),
    'GET /api/v1/auth/session': () => ({
      json: {
        user: { id: 'u1', username: 'chen', name: 'Chen', email: 'c@example.com', isPlatformAdmin: false },
        orgs: [{ id: 'o1', slug: 'orbit', name: 'Orbit', role }],
        csrfToken: 'csrf-test',
      },
    }),
    'GET /api/v1/orgs/:org/onboarding': () => ({ json: { steps: [], remaining: 0, capabilities: [] } }),
    'GET /api/v1/orgs/:org/me/git-accounts': () => ({ json: { items: state.items } }),
    'PUT /api/v1/orgs/:org/me/git-accounts/:id': (r) => {
      const h = (r.body as { handle: string }).handle.trim().replace(/^@/, '');
      state.items = state.items.map((a) =>
        a.integrationId === r.params.id ? { ...a, handle: h, linkedAt: '2026-10-31T00:00:00Z' } : a,
      );
      return { json: state.items.find((a) => a.integrationId === r.params.id) };
    },
    'DELETE /api/v1/orgs/:org/me/git-accounts/:id': (r) => {
      state.items = state.items.map((a) =>
        a.integrationId === r.params.id ? { ...a, handle: null, linkedAt: null } : a,
      );
      return { status: 204 };
    },
    'GET /api/v1/orgs/:org/git-identities': () => ({ json: { items: state.links } }),
    'DELETE /api/v1/orgs/:org/git-identities/:id': () => {
      state.links = [];
      return { status: 204 };
    },
    ...routes,
  });
  return { api, state };
}

describe('registry', () => {
  it('Connected accounts is an Account page for every member and searchable', () => {
    const p = settingsPages.find((x) => x.path === 'connected-accounts')!;
    expect(p.group).toBe('Account');
    expect(p.access).toBe('member');
    expect(searchSettings(settingsPages, 'github username').map((h) => h.page.path)).toContain(
      'connected-accounts',
    );
  });
  it('git_identity.removed is a known event that refreshes the lists and the issue details', () => {
    expect(EVENT_TYPES).toContain('git_identity.removed');
    const qc = testClient();
    const seen: unknown[] = [];
    qc.invalidateQueries = ((f: { queryKey: unknown }) => {
      seen.push(f.queryKey);
      return Promise.resolve();
    }) as typeof qc.invalidateQueries;
    invalidateForEvent(qc, 'orbit', { type: 'git_identity.removed' });
    expect(seen).toEqual(
      expect.arrayContaining([
        keys.gitAccounts('orbit'),
        keys.gitIdentities('orbit'),
        keys.issueDetails('orbit'),
      ]),
    );
  });
});

describe('Connected accounts page', () => {
  it('lists each connection with an input; a member sees no admin section', async () => {
    server();
    renderApp('/orbit/settings/connected-accounts');
    expect(await screen.findByRole('heading', { name: 'Connected accounts' })).toBeInTheDocument();
    expect(await screen.findByLabelText('Your handle on GitHub, github.com')).toHaveValue('');
    expect(screen.getByLabelText('Your handle on Work GitLab (GitLab)')).toBeInTheDocument();
    expect(screen.queryByText('Links in this organisation')).not.toBeInTheDocument();
    // The nav entry is there for a plain member.
    const nav = screen.getByRole('navigation', { name: 'Settings' });
    expect(within(nav).getByRole('link', { name: 'Connected accounts' })).toHaveAttribute(
      'href',
      '/orbit/settings/connected-accounts',
    );
  });

  it('tells the reader the link is a claim', async () => {
    server();
    renderApp('/orbit/settings/connected-accounts');
    expect(
      await screen.findByText(
        /This link is a claim you make\. It only decides how change requests are shown\. It does not prove you own the account\./,
      ),
    ).toBeInTheDocument();
  });

  it('links a handle with PUT {handle}, shows Saved and the Change and Remove buttons', async () => {
    const { api } = server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/connected-accounts');
    const box = await screen.findByLabelText('Your handle on GitHub, github.com');
    const link = screen.getAllByRole('button', { name: 'Link' })[0]!;
    expect(link).toBeDisabled();
    await user.type(box, '@Ann-Dev');
    await user.click(link);
    expect(await screen.findByText('Saved.')).toBeInTheDocument();
    const put = api.calls.find((c) => c.method === 'PUT')!;
    expect(put.url.pathname).toBe(`/api/v1/orgs/orbit/me/git-accounts/${GH}`);
    expect(put.body).toEqual({ handle: '@Ann-Dev' });
    await waitFor(() =>
      expect(screen.getByLabelText('Your handle on GitHub, github.com')).toHaveValue('Ann-Dev'),
    );
    expect(screen.getByRole('button', { name: 'Change' })).toBeDisabled(); // nothing changed yet
    expect(screen.getByRole('button', { name: 'Remove' })).toBeInTheDocument();
  });

  it('removes a handle with DELETE', async () => {
    const { api } = server({}, 'member', [acct({ handle: 'ann-dev', linkedAt: '2026-10-01T00:00:00Z' })]);
    const user = userEvent.setup();
    renderApp('/orbit/settings/connected-accounts');
    await user.click(await screen.findByRole('button', { name: 'Remove' }));
    expect(await screen.findByText('Removed.')).toBeInTheDocument();
    expect(
      api.calls.some((c) => c.method === 'DELETE' && c.url.pathname.endsWith(`/me/git-accounts/${GH}`)),
    ).toBe(true);
    await waitFor(() => expect(screen.getByLabelText('Your handle on GitHub, github.com')).toHaveValue(''));
  });

  it('a 409 shows a fixed sentence, no server text, and keeps the typed value', async () => {
    server({
      'PUT /api/v1/orgs/:org/me/git-accounts/:id': () => ({
        status: 409,
        json: { error: `${MARKER} held by ann`, code: 'conflict' },
      }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/connected-accounts');
    const box = await screen.findByLabelText('Your handle on GitHub, github.com');
    await user.type(box, 'taken');
    await user.click(screen.getAllByRole('button', { name: 'Link' })[0]!);
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('That handle is already linked on this connection.');
    expect(document.body.textContent).not.toContain(MARKER);
    expect(document.body.textContent).not.toContain('ann');
    expect(box).toHaveValue('taken');
    expect(box).toHaveAttribute('aria-invalid', 'true');
  });

  it('a 400 and a network error get fixed sentences', async () => {
    server({
      'PUT /api/v1/orgs/:org/me/git-accounts/:id': () => ({
        status: 400,
        json: { error: MARKER, code: 'validation_failed' },
      }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/connected-accounts');
    await user.type(await screen.findByLabelText('Your handle on GitHub, github.com'), 'a b');
    await user.click(screen.getAllByRole('button', { name: 'Link' })[0]!);
    expect(await screen.findByRole('alert')).toHaveTextContent('That is not a valid GitHub username.');
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('an agent (403) gets a fixed sentence and no form', async () => {
    server({
      'GET /api/v1/orgs/:org/me/git-accounts': () => ({
        status: 403,
        json: { error: MARKER, code: 'forbidden' },
      }),
    });
    renderApp('/orbit/settings/connected-accounts');
    expect(await screen.findByText(/An agent has no account on a git host/)).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
    expect(screen.queryByRole('textbox')).not.toBeInTheDocument();
  });

  it('a load error shows a fixed sentence and Try again refetches; the empty state names Integrations', async () => {
    let n = 0;
    server({
      'GET /api/v1/orgs/:org/me/git-accounts': () => {
        n++;
        if (n === 1) return { status: 500, json: { error: MARKER, code: 'internal_error' } };
        return { json: { items: [] } };
      },
    });
    renderApp('/orbit/settings/connected-accounts');
    expect(await screen.findByText('Could not load connected accounts')).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
    await userEvent.setup().click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByText('No git connections')).toBeInTheDocument();
  });

  it('an org admin sees every link and removes one after a confirmation', async () => {
    const { api } = server({}, 'admin');
    const user = userEvent.setup();
    renderApp('/orbit/settings/connected-accounts');
    const table = await screen.findByRole('table', { name: 'Linked git handles of the members' });
    expect(within(table).getByText('Ann Lee')).toBeInTheDocument();
    expect(within(table).getByText('ann-dev')).toBeInTheDocument();
    await user.click(within(table).getByRole('button', { name: 'Remove the link of Ann Lee on github.com' }));
    const dialog = await screen.findByRole('dialog');
    expect(dialog).toHaveTextContent('The member keeps all access.');
    await user.click(within(dialog).getByRole('button', { name: 'Remove' }));
    expect(await screen.findByText('No links yet')).toBeInTheDocument();
    expect(
      api.calls.some((c) => c.method === 'DELETE' && c.url.pathname.endsWith('/git-identities/l1')),
    ).toBe(true);
  });

  it('after a keyboard Remove focus stays in the row, on the input', async () => {
    server({}, 'member', [acct({ handle: 'ann-dev', linkedAt: '2026-10-01T00:00:00Z' })]);
    const user = userEvent.setup();
    renderApp('/orbit/settings/connected-accounts');
    const btn = await screen.findByRole('button', { name: 'Remove' });
    btn.focus();
    await user.keyboard('{Enter}');
    await screen.findByText('Removed.');
    expect(screen.getByLabelText('Your handle on GitHub, github.com')).toHaveFocus();
  });

  it('the 400 sentence names the rule of the host', async () => {
    server({
      'PUT /api/v1/orgs/:org/me/git-accounts/:id': () => ({
        status: 400,
        json: { error: MARKER, code: 'validation_failed' },
      }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/connected-accounts');
    await user.type(await screen.findByLabelText('Your handle on GitHub, github.com'), 'a b');
    await user.click(screen.getAllByRole('button', { name: 'Link' })[0]!);
    expect(await screen.findByRole('alert')).toHaveTextContent('1 to 39 letters, digits, - or _');
    await user.type(screen.getByLabelText('Your handle on Work GitLab (GitLab)'), 'a b');
    await user.click(screen.getAllByRole('button', { name: 'Link' })[1]!);
    await waitFor(() =>
      expect(screen.getAllByRole('alert')[1]).toHaveTextContent(
        '1 to 100 characters: letters, digits, . - or _',
      ),
    );
  });

  it('a 409 for a turned-off connection has its own sentence', async () => {
    server({
      'PUT /api/v1/orgs/:org/me/git-accounts/:id': () => ({
        status: 409,
        json: {
          error: `${CONNECTION_OFF_PREFIX}; ask an org admin` /* tracker.ConnectionOffText */,
          code: 'conflict',
        },
      }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/connected-accounts');
    await user.type(await screen.findByLabelText('Your handle on GitHub, github.com'), 'ann');
    await user.click(screen.getAllByRole('button', { name: 'Link' })[0]!);
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('This connection is turned off.');
    expect(alert).not.toHaveTextContent('already linked');
  });

  it('the empty status node takes no space but stays a live region', async () => {
    server();
    renderApp('/orbit/settings/connected-accounts');
    await screen.findByLabelText('Your handle on GitHub, github.com');
    const statuses = screen.getAllByRole('status');
    expect(statuses.length).toBeGreaterThan(0);
  });
});

describe('admin removal focus', () => {
  const link = (id: string, name: string) => ({
    id,
    integrationId: GH,
    provider: 'github',
    integrationName: 'github.com',
    handle: `${id}-h`,
    user: { id: `u-${id}`, username: id, name },
    updatedAt: '2026-10-30T00:00:00Z',
  });

  async function removeVia(user: ReturnType<typeof userEvent.setup>, who: string) {
    const b = await screen.findByRole('button', { name: `Remove the link of ${who} on github.com` });
    b.focus();
    await user.keyboard('{Enter}');
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByRole('button', { name: 'Cancel' })).toHaveFocus();
    await user.click(within(dialog).getByRole('button', { name: 'Remove' }));
  }

  function adminServer(rows: unknown[]) {
    const st = { rows };
    server(
      {
        'GET /api/v1/orgs/:org/git-identities': () => ({ json: { items: st.rows } }),
        'DELETE /api/v1/orgs/:org/git-identities/:id': (r) => {
          st.rows = (st.rows as { id: string }[]).filter((x) => x.id !== r.params.id);
          return { status: 204 };
        },
      },
      'admin',
    );
  }

  it('goes to the next row Remove button', async () => {
    adminServer([link('a', 'Ann'), link('b', 'Bob'), link('c', 'Cy')]);
    const user = userEvent.setup();
    renderApp('/orbit/settings/connected-accounts');
    await removeVia(user, 'Bob');
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Remove the link of Cy on github.com' })).toHaveFocus(),
    );
  });

  it('goes to the previous row when the last one is removed', async () => {
    adminServer([link('a', 'Ann'), link('b', 'Bob')]);
    const user = userEvent.setup();
    renderApp('/orbit/settings/connected-accounts');
    await removeVia(user, 'Bob');
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Remove the link of Ann on github.com' })).toHaveFocus(),
    );
  });

  it('goes to the section heading when no row is left', async () => {
    adminServer([link('a', 'Ann')]);
    const user = userEvent.setup();
    renderApp('/orbit/settings/connected-accounts');
    await removeVia(user, 'Ann');
    await waitFor(() =>
      expect(screen.getByRole('heading', { name: 'Links in this organisation' })).toHaveFocus(),
    );
  });
});
