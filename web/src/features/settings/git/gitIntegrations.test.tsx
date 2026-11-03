import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import type { QueryClient } from '@tanstack/react-query';
import { describe, expect, it, vi } from 'vitest';
import { baseRoutes, mockApi, type Handler, type MockRequest } from '../../../test/mockApi';
import { renderApp } from '../../../test/render';
import { searchSettings, settingsPages } from '../settingsPages';
import { touchClass } from './gitParts';

// LAUN-9: Settings, Integrations (decisions-T17 #46, #47, #51).

const TOKEN = 'glpat-planted-token-value-never-seen-again';
const SECRET = 'whsec-planted-secret-value-never-seen-again';
const NEW_SECRET = 'whsec-rotated-secret-value-never-seen-again';
const MARKER = 'RAW-SERVER-TEXT-MARKER';

function integ(over: Record<string, unknown> = {}) {
  return {
    id: 'i1',
    provider: 'gitlab',
    flavor: 'cloud',
    name: 'gitlab.com',
    baseUrl: 'https://gitlab.com',
    apiBaseUrl: null,
    mode: 'token',
    enabled: true,
    status: 'ok',
    attentionCode: null,
    tokenSet: true,
    webhookSecretSet: true,
    secretPending: false,
    webhookUrl: 'https://launchpad.example.com/api/v1/orgs/orbit/integrations/gitlab/webhook',
    repoCount: 0,
    lastDelivery: null,
    createdAt: '2026-10-01T00:00:00Z',
    updatedAt: '2026-10-01T00:00:00Z',
    ...over,
  };
}
function repo(id: string, over: Record<string, unknown> = {}) {
  return {
    id,
    integrationId: 'i1',
    repoKey: `10${id.length}`,
    repoPath: `orbit/${id}`,
    team: null,
    hookState: 'active',
    hookError: null,
    createdAt: '2026-10-02T00:00:00Z',
    ...over,
  };
}
const list = (items: unknown[]) => ({ json: { items, nextCursor: null } });

function server(
  opts: {
    role?: 'admin' | 'member';
    integrations?: unknown[];
    routes?: Record<string, Handler>;
  } = {},
) {
  const rows = opts.integrations ?? [];
  const one = (r: MockRequest) => rows.find((x) => (x as { id: string }).id === r.params.integration);
  return mockApi({
    ...baseRoutes(),
    'GET /api/v1/auth/session': () => ({
      json: {
        user: { id: 'u1', username: 'chen', name: 'Chen', email: 'c@example.com', isPlatformAdmin: false },
        orgs: [{ id: 'o1', slug: 'orbit', name: 'Orbit', role: opts.role ?? 'admin' }],
        csrfToken: 'csrf-test',
      },
    }),
    'GET /api/v1/orgs/:org/integrations': () => list(rows),
    'GET /api/v1/orgs/:org/integrations/:integration': (r) => {
      const f = one(r);
      return f ? { json: f } : { status: 404, json: { error: 'not found', code: 'not_found' } };
    },
    'GET /api/v1/orgs/:org/integrations/:integration/repos': () => list([]),
    'GET /api/v1/orgs/:org/integrations/:integration/deliveries': () => list([]),
    ...opts.routes,
  });
}

const posts = (api: { calls: MockRequest[] }, suffix: string) =>
  api.calls.filter((c) => c.method === 'POST' && c.url.pathname.endsWith(suffix));
const webStorage = () => JSON.stringify({ ...localStorage }) + JSON.stringify({ ...sessionStorage });
function cacheDump(client: QueryClient) {
  return (
    JSON.stringify(
      client
        .getQueryCache()
        .getAll()
        .map((q) => [q.queryKey, q.state.data, q.state.error]),
    ) +
    JSON.stringify(
      client
        .getMutationCache()
        .getAll()
        .map((m) => [m.state, m.options.mutationKey]),
    )
  );
}

describe('navigation, access and settings search', () => {
  it('an admin sees Integrations in the sidebar', async () => {
    server();
    renderApp('/orbit/settings/organisation');
    const nav = await screen.findByRole('navigation', { name: 'Settings' });
    expect(within(nav).getByRole('link', { name: 'Integrations' })).toHaveAttribute(
      'href',
      '/orbit/settings/integrations',
    );
  });

  it('a member sees the notice only and no integration call is made', async () => {
    const api = server({ role: 'member' });
    renderApp('/orbit/settings/integrations');
    expect(await screen.findByRole('heading', { name: 'You need admin access.' })).toBeInTheDocument();
    const links = within(screen.getByRole('navigation', { name: 'Settings' })).getAllByRole('link');
    expect(links.map((a) => a.textContent)).toEqual([
      'Notifications',
      'Sessions',
      'Connected accounts',
      'Mobile devices',
      'Personal webhooks',
      'Appearance',
    ]);
    expect(api.calls.some((c) => c.url.pathname.includes('/integrations'))).toBe(false);
  });

  it('the page is admin only and has keywords for the settings search', () => {
    const page = settingsPages.find((p) => p.path === 'integrations');
    expect(page).toMatchObject({ label: 'Integrations', access: 'admin' });
    expect(page!.keywords.length).toBeGreaterThan(3);
    for (const word of ['gitlab', 'github', 'webhook', 'rotate secret']) {
      expect(searchSettings(settingsPages, word).map((h) => h.page.path)).toContain('integrations');
    }
  });
});

describe('the list', () => {
  it('shows each connection with provider, address, mode, status and the reason', async () => {
    server({
      integrations: [
        integ({ repoCount: 2 }),
        integ({
          id: 'i2',
          provider: 'github',
          name: 'github.com',
          baseUrl: 'https://github.com',
          mode: 'webhook_only',
          status: 'needs_attention',
          attentionCode: 'secret_unreadable',
        }),
        integ({ id: 'i3', name: 'off.example', enabled: false, status: 'disabled' }),
      ],
    });
    renderApp('/orbit/settings/integrations');
    const rows = await screen.findAllByRole('listitem');
    expect(rows).toHaveLength(3);
    expect(rows[0]).toHaveTextContent('GitLab: gitlab.com');
    expect(rows[0]).toHaveTextContent('Working');
    expect(rows[0]).toHaveTextContent('2 repositories');
    expect(rows[1]).toHaveTextContent('GitHub: github.com');
    expect(rows[1]).toHaveTextContent('Needs attention');
    expect(rows[1]).toHaveTextContent('Webhook only');
    expect(rows[1]).toHaveTextContent('Launchpad cannot read the stored secret.');
    expect(rows[2]).toHaveTextContent('Disabled');
    expect(within(rows[0]!).getByRole('link')).toHaveAttribute('href', '/orbit/settings/integrations/i1');
  });

  it('shows an empty state, and a fixed text when the load fails', async () => {
    server();
    const { unmount } = renderApp('/orbit/settings/integrations');
    expect(await screen.findByText('No git connections yet')).toBeInTheDocument();
    unmount();
    server({
      routes: {
        'GET /api/v1/orgs/:org/integrations': () => ({
          status: 500,
          json: { error: MARKER, code: 'internal_error' },
        }),
      },
    });
    renderApp('/orbit/settings/integrations');
    expect(await screen.findByText('Could not load the connections')).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
  });
});

describe('the connect dialog', () => {
  async function openToken() {
    const api = server({
      routes: {
        'POST /api/v1/orgs/:org/integrations': (r) => ({
          status: 201,
          json: { ...integ({ id: 'new' }), baseUrl: (r.body as { baseUrl: string }).baseUrl },
        }),
      },
    });
    const user = userEvent.setup();
    const view = renderApp('/orbit/settings/integrations');
    await screen.findByText('No git connections yet');
    await user.click(screen.getByRole('button', { name: 'Connect' }));
    const dialog = await screen.findByRole('dialog', { name: 'Connect a git host' });
    return { api, user, dialog, client: view.client };
  }

  it('walks through three steps with the keyboard and a named dialog', async () => {
    const { user, dialog } = await openToken();
    // Step 1: the first provider is focused; the radios are one group.
    expect(within(dialog).getByRole('radio', { name: 'GitLab' })).toHaveFocus();
    expect(within(dialog).getByRole('group', { name: 'Provider' })).toBeInTheDocument();
    expect(within(dialog).getByRole('list', { name: 'Steps' })).toBeInTheDocument();
    await user.keyboard('{ArrowDown}');
    expect(within(dialog).getByRole('radio', { name: 'GitHub' })).toBeChecked();
    await user.click(within(dialog).getByRole('button', { name: 'Next' }));
    // Step 2: token mode is first, the scopes show, and the token field is a password field.
    const token = await within(dialog).findByLabelText('Token');
    expect(within(dialog).getByRole('radio', { name: /Token: Launchpad makes the hook/ })).toBeChecked();
    expect(token).toHaveAttribute('type', 'password');
    expect(token).toHaveAttribute('autocomplete', 'new-password');
    expect(token).not.toHaveAttribute('name');
    expect(within(dialog).getByText(/Fine-grained token \(better\)/)).toBeInTheDocument();
    expect(within(dialog).getByRole('link', { name: /Make a token on GitHub/ })).toHaveAttribute(
      'href',
      'https://github.com/settings/personal-access-tokens/new',
    );
    // Back keeps the dialog, Escape closes it.
    await user.click(within(dialog).getByRole('button', { name: 'Back' }));
    expect(within(dialog).getByRole('radio', { name: 'GitHub' })).toBeChecked();
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('checks the address before it asks the server', async () => {
    const { api, user, dialog } = await openToken();
    await user.click(within(dialog).getByLabelText('My GitLab runs on my own server'));
    const url = within(dialog).getByLabelText('Web address of the host');
    await user.click(within(dialog).getByRole('button', { name: 'Next' }));
    expect(url).toHaveFocus();
    expect(url).toHaveAccessibleDescription('Enter the web address of the host.');
    await user.type(url, 'https://user:pw@git.example.com');
    await user.click(within(dialog).getByRole('button', { name: 'Next' }));
    expect(url).toHaveAccessibleDescription('Remove the user name and password from the address.');
    expect(api.calls.some((c) => c.method === 'POST')).toBe(false);
  });

  it('keeps the token out of every cache, storage, the URL and the page, and clears it', async () => {
    const { api, user, dialog, client } = await openToken();
    await user.click(within(dialog).getByRole('button', { name: 'Next' }));
    await user.type(await within(dialog).findByLabelText('Token'), TOKEN);
    await user.click(within(dialog).getByRole('button', { name: 'Connect' }));
    expect(await screen.findByRole('dialog', { name: 'Connected' })).toBeInTheDocument();
    const body = posts(api, '/integrations')[0]!.body as Record<string, unknown>;
    expect(body).toMatchObject({
      provider: 'gitlab',
      baseUrl: 'https://gitlab.com',
      mode: 'token',
      token: TOKEN,
    });
    expect(cacheDump(client)).not.toContain(TOKEN);
    expect(client.getMutationCache().getAll()).toHaveLength(0);
    expect(webStorage()).not.toContain(TOKEN);
    expect(window.location.href).not.toContain(TOKEN);
    expect(document.body.innerHTML).not.toContain(TOKEN);
    // No later request carries it.
    const later = api.calls.slice(api.calls.indexOf(posts(api, '/integrations')[0]!) + 1);
    expect(JSON.stringify(later.map((c) => [c.url.href, c.body]))).not.toContain(TOKEN);
  });

  it('shows fixed text for a host error, never the server words or the token, and clears the field', async () => {
    const api = server({
      routes: {
        'POST /api/v1/orgs/:org/integrations': () => ({
          status: 502,
          json: { error: `host said ${MARKER} for ${TOKEN}`, code: 'provider_unauthorized' },
        }),
      },
    });
    const user = userEvent.setup();
    const { client } = renderApp('/orbit/settings/integrations');
    await screen.findByText('No git connections yet');
    await user.click(screen.getByRole('button', { name: 'Connect' }));
    const dialog = await screen.findByRole('dialog', { name: 'Connect a git host' });
    await user.click(within(dialog).getByRole('button', { name: 'Next' }));
    const field = await within(dialog).findByLabelText('Token');
    await user.type(field, TOKEN);
    await user.click(within(dialog).getByRole('button', { name: 'Connect' }));
    const alert = await within(dialog).findByRole('alert');
    expect(alert).toHaveTextContent('The host refused the token.');
    expect(document.body.textContent).not.toContain(MARKER);
    expect(document.body.textContent).not.toContain(TOKEN);
    expect(cacheDump(client)).not.toContain(TOKEN);
    expect(within(dialog).getByLabelText('Token')).toHaveValue('');
    expect(api.calls.filter((c) => c.method === 'POST')).toHaveLength(1);
  });

  it.each([
    ['provider_unreachable', 502, 'could not reach the host'],
    ['provider_forbidden', 502, 'not allowed to do this'],
    ['provider_error', 502, 'answered with an error'],
    ['integration_limit', 409, 'already has 8 connections'],
    ['encryption_unavailable', 409, 'SETTINGS_ENCRYPTION_KEY'],
    ['public_url_required', 409, 'PUBLIC_BASE_URL'],
    ['rate_limited', 429, 'Too many tries'],
  ])('maps %s to plain text', async (code, status, text) => {
    server({
      routes: {
        'POST /api/v1/orgs/:org/integrations': () => ({ status, json: { error: MARKER, code } }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations');
    await screen.findByText('No git connections yet');
    await user.click(screen.getByRole('button', { name: 'Connect' }));
    const dialog = await screen.findByRole('dialog', { name: 'Connect a git host' });
    await user.click(within(dialog).getByRole('button', { name: 'Next' }));
    await user.type(await within(dialog).findByLabelText('Token'), TOKEN);
    await user.click(within(dialog).getByRole('button', { name: 'Connect' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(new RegExp(text, 'i'));
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('turns a 400 about the address into a field message at step 1, without the server words', async () => {
    server({
      routes: {
        'POST /api/v1/orgs/:org/integrations': () => ({
          status: 400,
          json: { error: `baseUrl ${MARKER}: address is private`, code: 'validation_failed' },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations');
    await screen.findByText('No git connections yet');
    await user.click(screen.getByRole('button', { name: 'Connect' }));
    const dialog = await screen.findByRole('dialog', { name: 'Connect a git host' });
    await user.click(within(dialog).getByLabelText('My GitLab runs on my own server'));
    await user.type(within(dialog).getByLabelText('Web address of the host'), 'https://10.0.0.5');
    await user.click(within(dialog).getByRole('button', { name: 'Next' }));
    await user.type(await within(dialog).findByLabelText('Token'), TOKEN);
    await user.click(within(dialog).getByRole('button', { name: 'Connect' }));
    const url = await within(dialog).findByLabelText('Web address of the host');
    await waitFor(() => expect(url).toHaveFocus());
    expect(url).toHaveAccessibleDescription(/GIT_ALLOW_PRIVATE_PROVIDERS/);
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('webhook only: shows the secret once with the address, needs the box, and keeps it out of the caches', async () => {
    const api = server({
      routes: {
        'POST /api/v1/orgs/:org/integrations': () => ({
          status: 201,
          json: { ...integ({ id: 'w1', mode: 'webhook_only', tokenSet: false }), webhookSecret: SECRET },
        }),
      },
    });
    const user = userEvent.setup();
    const { client } = renderApp('/orbit/settings/integrations');
    await screen.findByText('No git connections yet');
    await user.click(screen.getByRole('button', { name: 'Connect' }));
    let dialog = await screen.findByRole('dialog', { name: 'Connect a git host' });
    await user.click(within(dialog).getByRole('button', { name: 'Next' }));
    await user.click(await within(dialog).findByRole('radio', { name: /Webhook only/ }));
    await user.click(within(dialog).getByRole('button', { name: 'Connect' }));
    dialog = await screen.findByRole('dialog', { name: 'Copy the webhook secret' });
    expect(within(dialog).getByRole('alert')).toHaveTextContent(
      'Copy this secret now. It is not shown again.',
    );
    expect(within(dialog).getByTestId('new-git-secret')).toHaveTextContent(SECRET);
    expect(within(dialog).getByTestId('webhook-url')).toHaveTextContent('/integrations/gitlab/webhook');
    expect(within(dialog).getByText(/X-Gitlab-Token/)).toBeInTheDocument();
    expect(cacheDump(client)).not.toContain(SECRET);
    expect(client.getMutationCache().getAll()).toHaveLength(0);
    expect(webStorage()).not.toContain(SECRET);
    expect(window.location.href).not.toContain(SECRET);
    const done = within(dialog).getByRole('button', { name: 'Done' });
    expect(done).toBeDisabled();
    await user.keyboard('{Escape}');
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Copy secret' }));
    expect(await navigator.clipboard.readText()).toBe(SECRET);
    await user.click(within(dialog).getByLabelText('I saved the secret in a safe place.'));
    await user.click(done);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain(SECRET);
    const later = api.calls.slice(api.calls.indexOf(posts(api, '/integrations')[0]!) + 1);
    expect(JSON.stringify(later.map((c) => [c.url.href, c.body]))).not.toContain(SECRET);
  });

  it('lists Bitbucket, names the token kinds and says a Data Center that cannot sign cannot be used (LAUN-84)', async () => {
    const api = server({
      routes: {
        'POST /api/v1/orgs/:org/integrations': () => ({
          status: 201,
          json: {
            ...integ({
              id: 'b1',
              provider: 'bitbucket',
              mode: 'webhook_only',
              tokenSet: false,
              baseUrl: 'https://bitbucket.org',
              webhookUrl: 'https://launchpad.example.com/api/v1/orgs/orbit/integrations/bitbucket/webhook',
            }),
            webhookSecret: SECRET,
          },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations');
    await screen.findByText('No git connections yet');
    await user.click(screen.getByRole('button', { name: 'Connect' }));
    let dialog = await screen.findByRole('dialog', { name: 'Connect a git host' });
    await user.click(within(dialog).getByRole('radio', { name: 'Bitbucket' }));
    expect(within(dialog).getByLabelText('My Bitbucket is Bitbucket Data Center')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Next' }));
    expect(
      await within(dialog).findByText(/repository access token with Webhooks: read and write/),
    ).toBeInTheDocument();
    expect(within(dialog).getByText(/HTTP access token with Repository admin/)).toBeInTheDocument();
    expect(within(dialog).getByText(/app password needs a user name/)).toBeInTheDocument();
    // Bitbucket has no one token page, so no link is shown.
    expect(within(dialog).queryByRole('link', { name: /Make a token/ })).not.toBeInTheDocument();
    await user.click(within(dialog).getByRole('radio', { name: /Webhook only/ }));
    await user.click(within(dialog).getByRole('button', { name: 'Connect' }));
    dialog = await screen.findByRole('dialog', { name: 'Copy the webhook secret' });
    expect(within(dialog).getByTestId('webhook-url')).toHaveTextContent('/integrations/bitbucket/webhook');
    expect(within(dialog).getByText(/X-Hub-Signature/)).toBeInTheDocument();
    expect(
      within(dialog).getByText(
        /Data Center: the hook must have a secret. A version that cannot sign webhooks cannot be used with Launchpad/,
      ),
    ).toBeInTheDocument();
    expect(posts(api, '/integrations')[0]!.body).toMatchObject({
      provider: 'bitbucket',
      baseUrl: 'https://bitbucket.org',
    });
  });

  it('GitLab webhook only: a pasted secret goes in the body and is not shown again', async () => {
    const api = server({
      routes: {
        'POST /api/v1/orgs/:org/integrations': () => ({
          status: 201,
          json: integ({ id: 'w2', mode: 'webhook_only', tokenSet: false }),
        }),
      },
    });
    const user = userEvent.setup();
    const { client } = renderApp('/orbit/settings/integrations');
    await screen.findByText('No git connections yet');
    await user.click(screen.getByRole('button', { name: 'Connect' }));
    const dialog = await screen.findByRole('dialog', { name: 'Connect a git host' });
    await user.click(within(dialog).getByRole('button', { name: 'Next' }));
    await user.click(await within(dialog).findByRole('radio', { name: /Webhook only/ }));
    await user.type(within(dialog).getByLabelText('Use my own secret (optional)'), SECRET);
    await user.click(within(dialog).getByRole('button', { name: 'Connect' }));
    expect(await screen.findByRole('dialog', { name: 'Connected' })).toBeInTheDocument();
    expect(posts(api, '/integrations')[0]!.body).toMatchObject({ mode: 'webhook_only', secret: SECRET });
    expect(cacheDump(client)).not.toContain(SECRET);
    expect(document.body.innerHTML).not.toContain(SECRET);
  });
});

describe('the detail page', () => {
  it.each([
    ['secret_unreadable', 'Launchpad cannot read the stored secret.', 'Rotate the secret below'],
    ['token_rejected', 'The host refused the token.', 'Replace the token'],
    ['attention_unreachable', 'Launchpad could not reach the host.', 'Check the address'],
    ['hook_failed', 'A hook could not be made or updated on the host.', 'hook state'],
  ])('shows the banner for %s', async (code, title, fix) => {
    server({ integrations: [integ({ status: 'needs_attention', attentionCode: code })] });
    renderApp('/orbit/settings/integrations/i1');
    const banner = await screen.findByRole('alert');
    expect(banner).toHaveTextContent(title);
    expect(banner).toHaveTextContent(fix);
  });

  it('puts two banners at once in separate rows of one alert stack (LAUN-150)', async () => {
    server({
      integrations: [integ({ status: 'needs_attention', attentionCode: 'token_rejected' })],
      routes: {
        'POST /api/v1/orgs/:org/integrations/:integration/check': () => ({
          status: 500,
          json: { error: 'boom', code: 'internal' },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    await screen.findByRole('alert');
    await user.click(await screen.findByRole('button', { name: 'Check token' }));
    await waitFor(() => expect(screen.getAllByRole('alert')).toHaveLength(2));
    const [first, second] = screen.getAllByRole('alert');
    expect(first).not.toBe(second);
    expect(first!.parentElement).toHaveAttribute('data-testid', 'alert-stack');
    expect(second!.parentElement).toBe(first!.parentElement);
    expect(first).toBeVisible();
    expect(second).toBeVisible();
  });

  it('shows the disable sentence, and disables after a confirm', async () => {
    const api = server({
      integrations: [integ()],
      routes: {
        'PATCH /api/v1/orgs/:org/integrations/:integration': () => ({
          json: integ({ enabled: false, status: 'disabled' }),
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    const sentence =
      'Disable stops deliveries only. Hooks stay on the host until you unmap the repository. A host may turn off a hook that keeps getting 401.';
    expect(await screen.findByText(sentence)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Disable' }));
    const dialog = await screen.findByRole('dialog', { name: 'Disable this connection?' });
    expect(within(dialog).getByText(sentence)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Disable' }));
    await waitFor(() =>
      expect(api.calls.find((c) => c.method === 'PATCH')?.body).toEqual({ enabled: false }),
    );
  });

  it('shows a not-found text for an unknown connection', async () => {
    server();
    renderApp('/orbit/settings/integrations/zzz');
    expect(await screen.findByText('This connection does not exist, or it was removed.')).toBeInTheDocument();
  });

  it('check token shows the result; an empty login is fine', async () => {
    server({
      integrations: [integ()],
      routes: {
        'POST /api/v1/orgs/:org/integrations/:integration/check': () => ({ json: { ...integ(), login: '' } }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    await user.click(await screen.findByRole('button', { name: 'Check token' }));
    expect(await screen.findByText('The token works.')).toBeInTheDocument();
  });

  it('replace token: sends the token, clears it, checks, keeps it out of the caches', async () => {
    const api = server({
      integrations: [integ()],
      routes: {
        'PATCH /api/v1/orgs/:org/integrations/:integration': () => ({ json: integ() }),
        'POST /api/v1/orgs/:org/integrations/:integration/check': () => ({
          json: { ...integ(), login: 'chen' },
        }),
      },
    });
    const user = userEvent.setup();
    const { client } = renderApp('/orbit/settings/integrations/i1');
    await user.click(await screen.findByRole('button', { name: 'Replace token' }));
    const dialog = await screen.findByRole('dialog', { name: 'Replace the token' });
    const field = within(dialog).getByLabelText('New token');
    expect(field).toHaveFocus();
    await user.type(field, TOKEN);
    await user.click(within(dialog).getByRole('button', { name: 'Save token' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(api.calls.find((c) => c.method === 'PATCH')?.body).toEqual({ token: TOKEN });
    expect(await screen.findByText('The token works. It belongs to chen.')).toBeInTheDocument();
    expect(cacheDump(client)).not.toContain(TOKEN);
    expect(document.body.innerHTML).not.toContain(TOKEN);
    expect(webStorage()).not.toContain(TOKEN);
  });

  it('webhook only: shows the hook help and a token can be added (mode switches to token)', async () => {
    const api = server({
      integrations: [integ({ mode: 'webhook_only', tokenSet: false })],
      routes: {
        'PATCH /api/v1/orgs/:org/integrations/:integration': () => ({ json: integ() }),
        'POST /api/v1/orgs/:org/integrations/:integration/check': () => ({ json: { ...integ(), login: '' } }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    expect(await screen.findByText(/X-Gitlab-Token/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Add a token' }));
    const dialog = await screen.findByRole('dialog', { name: 'Add a token' });
    await user.type(within(dialog).getByLabelText('New token'), TOKEN);
    await user.click(within(dialog).getByRole('button', { name: 'Save token' }));
    await waitFor(() => expect(api.calls.some((c) => c.method === 'PATCH')).toBe(true));
    expect(api.calls.find((c) => c.method === 'PATCH')?.body).toEqual({ token: TOKEN, mode: 'token' });
  });

  it('webhook only: the Map a repository form hints that a token can pick repos from a list', async () => {
    server({ integrations: [integ({ mode: 'webhook_only', tokenSet: false })] });
    renderApp('/orbit/settings/integrations/i1');
    const form = await screen.findByRole('form', { name: 'Map a repository by id' });
    expect(within(form).getByText(/can pick repositories from a list/)).toBeInTheDocument();
  });

  it('the Add a token dialog shows the GitLab help and a link built from the connection address', async () => {
    server({
      integrations: [integ({ mode: 'webhook_only', tokenSet: false, baseUrl: 'https://git.example.org' })],
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    await user.click(await screen.findByRole('button', { name: 'Add a token' }));
    const dialog = await screen.findByRole('dialog', { name: 'Add a token' });
    expect(within(dialog).getByText(/new fine-grained form/)).toBeInTheDocument();
    expect(within(dialog).getByText(/Classic \(legacy\) personal token/)).toBeInTheDocument();
    expect(within(dialog).getByRole('link', { name: /Make a token on GitLab/ })).toHaveAttribute(
      'href',
      'https://git.example.org/-/user_settings/personal_access_tokens',
    );
  });
});

describe('repositories', () => {
  const avail = (items: unknown[], nextCursor: string | null = null) => ({ json: { items, nextCursor } });
  const ar = (id: string, over: Record<string, unknown> = {}) => ({
    repoId: id,
    repoKey: id,
    repoPath: `orbit/${id}`,
    url: `https://gitlab.com/orbit/${id}`,
    mapped: false,
    ...over,
  });

  it('a mapped repo is not offered; mapping removes it, unmapping brings it back, errors show an alert (LAUN-113)', async () => {
    const mapped: string[] = ['299'];
    let failMap = true;
    let failUnmap = true;
    const api = server({
      integrations: [integ()],
      routes: {
        'GET /api/v1/orgs/:org/integrations/:integration/repos': () =>
          list(mapped.map((k) => repo(`r${k}`, { repoKey: k, repoPath: `orbit/${k}` }))),
        'GET /api/v1/orgs/:org/integrations/:integration/available-repos': () =>
          // The mapped repo has host id 12 and canonical key 299; orbit/other has repoId 299 but another key.
          avail([ar('300'), ar('299', { repoId: '12', mapped: true }), ar('other', { repoId: '299' })]),
        'POST /api/v1/orgs/:org/integrations/:integration/repos': (r) => {
          if (failMap) return { status: 502, json: { error: 'x', code: 'provider_error' } };
          const k = (r.body as { repoId: string }).repoId;
          mapped.push(k);
          return { status: 201, json: repo(`r${k}`, { repoKey: k, repoPath: `orbit/${k}` }) };
        },
        'DELETE /api/v1/orgs/:org/integrations/:integration/repos/:repo': () => {
          if (failUnmap) return { status: 502, json: { error: 'x', code: 'provider_error' } };
          mapped.splice(0, mapped.length, ...mapped.filter((k) => k !== '299'));
          return { json: { hookRemoved: true } };
        },
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    await user.click(await screen.findByRole('button', { name: 'Find repositories' }));
    const found = await screen.findByRole('list', { name: 'Repositories on the host' });
    // The row whose repoKey is mapped is hidden; the row with the same repoId but another key stays.
    expect(within(found).getAllByRole('listitem')).toHaveLength(2);
    expect(within(found).queryByText('orbit/299')).toBeNull();
    expect(within(found).getByText('orbit/other')).toBeInTheDocument();
    expect(within(found).queryByRole('button', { name: /Remove/ })).toBeNull();
    // A failed map shows the alert and keeps the repo in the list.
    await user.click(within(found).getByRole('button', { name: 'Map orbit/300' }));
    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(within(found).getByText('orbit/300')).toBeInTheDocument();
    // A good map takes it out of the list; every repo is now mapped.
    failMap = false;
    await user.click(within(found).getByRole('button', { name: 'Map orbit/300' }));
    await waitFor(() => expect(within(found).queryByText('orbit/300')).toBeNull());
    expect(within(found).getAllByRole('listitem')).toHaveLength(1);
    // Unmap from the table: a failure shows the alert; a success brings the repo back.
    const table = await screen.findByRole('table', { name: 'Mapped repositories' });
    await user.click(within(table).getAllByRole('button', { name: /Unmap/ })[0]!);
    let dialog = await screen.findByRole('dialog', { name: 'Unmap this repository?' });
    await user.click(within(dialog).getByRole('button', { name: 'Unmap' }));
    expect(await screen.findAllByRole('alert')).toHaveLength(1);
    expect(screen.getByRole('alert')).toHaveTextContent(/\S/);
    failUnmap = false;
    await user.click(within(table).getAllByRole('button', { name: /Unmap/ })[0]!);
    dialog = await screen.findByRole('dialog', { name: 'Unmap this repository?' });
    await user.click(within(dialog).getByRole('button', { name: 'Unmap' }));
    await waitFor(() => expect(within(found).getByText('orbit/299')).toBeInTheDocument());
    expect(api.calls.filter((c) => c.method === 'DELETE').length).toBeGreaterThanOrEqual(2);
  });

  it('says so when every repository found is already mapped (LAUN-113)', async () => {
    server({
      integrations: [integ()],
      routes: {
        'GET /api/v1/orgs/:org/integrations/:integration/repos': () =>
          list([repo('x', { repoKey: '299', repoPath: 'orbit/299' })]),
        'GET /api/v1/orgs/:org/integrations/:integration/available-repos': () => avail([ar('299')]),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    await user.click(await screen.findByRole('button', { name: 'Find repositories' }));
    expect(await screen.findByText('Every repository found is already mapped.')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Map orbit/299' })).toBeNull();
  });

  it('with the first page all mapped, Load more still works and shows the next page (LAUN-113)', async () => {
    server({
      integrations: [integ()],
      routes: {
        'GET /api/v1/orgs/:org/integrations/:integration/repos': () =>
          list([repo('x', { repoKey: '299', repoPath: 'orbit/299' })]),
        'GET /api/v1/orgs/:org/integrations/:integration/available-repos': (r) =>
          r.url.searchParams.get('cursor') === 'CUR-1'
            ? avail([ar('301')])
            : avail([ar('299', { mapped: true })], 'CUR-1'),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    await user.click(await screen.findByRole('button', { name: 'Find repositories' }));
    expect(
      await screen.findByText('Every repository loaded so far is already mapped. Load more.'),
    ).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Load more' }));
    expect(await screen.findByRole('button', { name: 'Map orbit/301' })).toBeInTheDocument();
    expect(screen.queryByText(/already mapped/)).toBeNull();
  });

  it('searches, pages with the opaque cursor, and maps a repository with the chosen team', async () => {
    let page = 0;
    const api = server({
      integrations: [integ()],
      routes: {
        'GET /api/v1/orgs/:org/integrations/:integration/repos': () =>
          list([repo('x', { repoKey: '299', repoPath: 'orbit/299' })]),
        'GET /api/v1/orgs/:org/integrations/:integration/available-repos': (r) => {
          page++;
          return r.url.searchParams.get('cursor') === 'CUR-1'
            ? avail([ar('301')])
            : avail([ar('300'), ar('299', { mapped: true })], 'CUR-1');
        },
        'POST /api/v1/orgs/:org/integrations/:integration/repos': (r) => ({
          status: 201,
          json: repo('300', { repoKey: (r.body as { repoId: string }).repoId }),
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    await user.click(await screen.findByRole('button', { name: 'Find repositories' }));
    const found = await screen.findByRole('list', { name: 'Repositories on the host' });
    expect(within(found).getAllByRole('listitem')).toHaveLength(1);
    expect(within(found).queryByText('orbit/299')).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Load more' }));
    await waitFor(() => expect(within(found).getAllByRole('listitem')).toHaveLength(2));
    const calls = api.calls.filter((c) => c.url.pathname.endsWith('/available-repos'));
    expect(calls).toHaveLength(2);
    expect(calls[1]!.url.searchParams.get('cursor')).toBe('CUR-1');
    expect(page).toBe(2);
    // A search is a new query.
    await user.type(screen.getByLabelText('Search repositories'), 'kid');
    await user.click(within(screen.getByRole('search')).getByRole('button', { name: 'Search' }));
    await waitFor(() =>
      expect(
        api.calls
          .filter((c) => c.url.pathname.endsWith('/available-repos'))
          .at(-1)!
          .url.searchParams.get('q'),
      ).toBe('kid'),
    );
    await user.selectOptions(screen.getByLabelText('Team for the new mapping'), 'team1');
    await user.click(await screen.findByRole('button', { name: 'Map orbit/300' }));
    await waitFor(() => expect(posts(api, '/repos')).toHaveLength(1));
    expect(posts(api, '/repos')[0]!.body).toEqual({ repoId: '300', teamId: 'team1' });
    expect(await screen.findByText('orbit/300 is mapped. Launchpad made the hook.')).toBeInTheDocument();
  });

  it('shows fixed text when the host fails, and for a second map', async () => {
    server({
      integrations: [integ()],
      routes: {
        'GET /api/v1/orgs/:org/integrations/:integration/available-repos': () => ({
          status: 502,
          json: { error: MARKER, code: 'provider_unreachable' },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    await user.click(await screen.findByRole('button', { name: 'Find repositories' }));
    expect(await screen.findByText(/could not reach the host/)).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('webhook only: validates the id, then maps by id and path', async () => {
    const api = server({
      integrations: [integ({ mode: 'webhook_only', tokenSet: false })],
      routes: {
        'POST /api/v1/orgs/:org/integrations/:integration/repos': () => ({ status: 201, json: repo('x') }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    await user.type(await screen.findByLabelText('Repository id'), 'abc');
    await user.click(screen.getByRole('button', { name: 'Map repository' }));
    expect(screen.getByLabelText('Repository id')).toHaveAccessibleDescription(
      'Enter the numeric repository id: 1 to 18 digits.',
    );
    expect(posts(api, '/repos')).toHaveLength(0);
    await user.clear(screen.getByLabelText('Repository id'));
    await user.type(screen.getByLabelText('Repository id'), '4821');
    await user.type(screen.getByLabelText('Repository path'), 'example-org/example-app');
    await user.click(screen.getByRole('button', { name: 'Map repository' }));
    await waitFor(() => expect(posts(api, '/repos')).toHaveLength(1));
    expect(posts(api, '/repos')[0]!.body).toEqual({ repoKey: '4821', repoPath: 'example-org/example-app' });
  });

  it('Bitbucket webhook only: takes a Data Center id or a Cloud workspace and repository uuid pair', async () => {
    const api = server({
      integrations: [
        integ({
          provider: 'bitbucket',
          mode: 'webhook_only',
          tokenSet: false,
          baseUrl: 'https://bitbucket.org',
        }),
      ],
      routes: {
        'POST /api/v1/orgs/:org/integrations/:integration/repos': () => ({ status: 201, json: repo('x') }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    const key = await screen.findByLabelText('Repository id');
    await user.type(key, 'PROJ/slug');
    await user.click(screen.getByRole('button', { name: 'Map repository' }));
    expect(screen.getByLabelText('Repository id')).toHaveAccessibleDescription(
      'Enter the numeric repository id (Data Center), or {workspace uuid}/{repository uuid} with the braces (Cloud).',
    );
    expect(posts(api, '/repos')).toHaveLength(0);
    const cloud = '{11111111-2222-3333-4444-555555555555}/{aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee}';
    await user.clear(screen.getByLabelText('Repository id'));
    await user.click(screen.getByLabelText('Repository id'));
    await user.paste(cloud);
    await user.type(screen.getByLabelText('Repository path'), 'example-org/example-app');
    await user.click(screen.getByRole('button', { name: 'Map repository' }));
    await waitFor(() => expect(posts(api, '/repos')).toHaveLength(1));
    expect(posts(api, '/repos')[0]!.body).toEqual({ repoKey: cloud, repoPath: 'example-org/example-app' });
  });

  it('lists mapped repositories with hook state, changes the team, and unmaps with a hook warning', async () => {
    const api = server({
      integrations: [integ({ repoCount: 2 })],
      routes: {
        'GET /api/v1/orgs/:org/integrations/:integration/repos': () =>
          list([repo('a'), repo('b', { hookState: 'failed', hookError: 'not_updated' })]),
        'PATCH /api/v1/orgs/:org/integrations/:integration/repos/:repo': () => ({ json: repo('a') }),
        'DELETE /api/v1/orgs/:org/integrations/:integration/repos/:repo': () => ({
          json: { hookRemoved: false },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    const table = await screen.findByRole('table', { name: 'Mapped repositories' });
    expect(within(table).getByText('Hook working')).toBeInTheDocument();
    expect(within(table).getByText('Hook not on the new secret yet')).toBeInTheDocument();
    // The place for the per-repository buttons of LAUN-83.
    expect(table.querySelectorAll('[data-slot="repo-actions"]')).toHaveLength(2);
    await user.selectOptions(within(table).getByLabelText('Team for orbit/a'), 'team1');
    await waitFor(() =>
      expect(api.calls.find((c) => c.method === 'PATCH')?.body).toEqual({ teamId: 'team1' }),
    );
    await user.click(within(table).getByRole('button', { name: 'Unmap orbit/a' }));
    const dialog = await screen.findByRole('dialog', { name: 'Unmap this repository?' });
    await user.click(within(dialog).getByRole('button', { name: 'Unmap' }));
    expect(await screen.findByText(/the hook may still be on the host/)).toBeInTheDocument();
  });
});

describe('rotating the secret', () => {
  it('token mode: a clean rotation says so', async () => {
    const api = server({
      integrations: [integ()],
      routes: {
        'POST /api/v1/orgs/:org/integrations/:integration/rotate-secret': () => ({
          json: { ...integ(), committed: true, failedRepos: [] },
        }),
      },
    });
    const user = userEvent.setup();
    const { client } = renderApp('/orbit/settings/integrations/i1');
    await user.click(await screen.findByRole('button', { name: 'Rotate secret' }));
    expect(
      await screen.findByText('The secret is rotated. Every hook uses the new secret.'),
    ).toBeInTheDocument();
    expect(posts(api, '/rotate-secret')).toHaveLength(1);
    expect(client.getMutationCache().getAll()).toHaveLength(0);
  });

  it('token mode: failed hooks are listed; retry, commit anyway (confirmed) and discard (confirmed) work', async () => {
    const api = server({
      integrations: [integ({ secretPending: true, repoCount: 2 })],
      routes: {
        'GET /api/v1/orgs/:org/integrations/:integration/repos': () =>
          list([repo('a'), repo('b', { hookState: 'failed', hookError: 'not_updated' })]),
        'POST /api/v1/orgs/:org/integrations/:integration/rotate-secret': () => ({
          json: { ...integ({ secretPending: true }), committed: false, failedRepos: [repo('b')] },
        }),
        'POST /api/v1/orgs/:org/integrations/:integration/rotate-secret/commit': () => ({ json: integ() }),
        'DELETE /api/v1/orgs/:org/integrations/:integration/rotate-secret': () => ({ json: integ() }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    const section = (await screen.findByRole('heading', { name: 'Webhook secret' })).closest('section')!;
    expect(within(section).getByText(/orbit\/b: Hook not on the new secret yet/)).toBeInTheDocument();
    // Plain Commit is off while a hook has not moved.
    expect(within(section).getByRole('button', { name: 'Commit' })).toBeDisabled();
    await user.click(within(section).getByRole('button', { name: 'Try the hooks again' }));
    expect(await within(section).findByText(/1 hook did not take the new secret yet/)).toBeInTheDocument();
    await user.click(within(section).getByRole('button', { name: 'Commit anyway' }));
    const force = await screen.findByRole('dialog', { name: 'Commit anyway?' });
    await user.click(within(force).getByRole('button', { name: 'Commit anyway' }));
    await waitFor(() => expect(posts(api, '/rotate-secret/commit')).toHaveLength(1));
    expect(posts(api, '/rotate-secret/commit')[0]!.body).toEqual({ force: true });
    await user.click(within(section).getByRole('button', { name: 'Discard new secret' }));
    const dlg = await screen.findByRole('dialog', { name: 'Discard the new secret?' });
    await user.click(within(dlg).getByRole('button', { name: 'Discard' }));
    await waitFor(() =>
      expect(api.calls.some((c) => c.method === 'DELETE' && c.url.pathname.endsWith('/rotate-secret'))).toBe(
        true,
      ),
    );
  });

  it('webhook only: shows the new secret once, then commits', async () => {
    let pending = false;
    const api = server({
      integrations: [],
      routes: {
        'GET /api/v1/orgs/:org/integrations/:integration': () => ({
          json: integ({ mode: 'webhook_only', tokenSet: false, secretPending: pending }),
        }),
        'POST /api/v1/orgs/:org/integrations/:integration/rotate-secret': () => {
          pending = true;
          return {
            json: {
              ...integ({ mode: 'webhook_only', secretPending: true }),
              webhookSecret: NEW_SECRET,
              committed: false,
              failedRepos: [],
            },
          };
        },
        'POST /api/v1/orgs/:org/integrations/:integration/rotate-secret/commit': () => ({ json: integ() }),
      },
    });
    const user = userEvent.setup();
    const { client } = renderApp('/orbit/settings/integrations/i1');
    await user.click(await screen.findByRole('button', { name: 'Rotate secret' }));
    const dialog = await screen.findByRole('dialog', { name: 'Copy the new secret' });
    expect(within(dialog).getByTestId('new-git-secret')).toHaveTextContent(NEW_SECRET);
    expect(cacheDump(client)).not.toContain(NEW_SECRET);
    expect(client.getMutationCache().getAll()).toHaveLength(0);
    expect(webStorage()).not.toContain(NEW_SECRET);
    await user.click(within(dialog).getByLabelText('I saved the new secret.'));
    await user.click(within(dialog).getByRole('button', { name: 'Done' }));
    expect(document.body.innerHTML).not.toContain(NEW_SECRET);
    expect(
      await screen.findByText('Put the new secret in the hook on the host. Then press Commit.'),
    ).toBeInTheDocument();
    await user.click(await screen.findByRole('button', { name: 'Commit' }));
    await waitFor(() => expect(posts(api, '/rotate-secret/commit')).toHaveLength(1));
    expect(posts(api, '/rotate-secret/commit')[0]!.body).toEqual({ force: false });
  });

  it('shows the pending state again after a reload (webhook only)', async () => {
    server({ integrations: [integ({ mode: 'webhook_only', tokenSet: false, secretPending: true })] });
    renderApp('/orbit/settings/integrations/i1');
    expect(await screen.findByText('A new secret is waiting.')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Commit' })).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Make another new secret' })).toBeInTheDocument();
  });

  it('a failed rotation shows fixed text only', async () => {
    server({
      integrations: [integ()],
      routes: {
        'POST /api/v1/orgs/:org/integrations/:integration/rotate-secret': () => ({
          status: 429,
          json: { error: MARKER, code: 'rate_limited' },
          headers: { 'Retry-After': '30' },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    await user.click(await screen.findByRole('button', { name: 'Rotate secret' }));
    expect(await screen.findByText('Too many tries. Wait 30 seconds and try again.')).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
  });
});

describe('retry and partial failure (review round 1)', () => {
  const ar = (id: string) => ({
    repoId: id,
    repoKey: id,
    repoPath: `orbit/${id}`,
    url: `https://gitlab.com/orbit/${id}`,
    mapped: false,
  });
  const availCalls = (api: { calls: MockRequest[] }) =>
    api.calls.filter((c) => c.url.pathname.endsWith('/available-repos'));

  it('a failed Find repositories can be tried again with Search, and nothing refetches by itself', async () => {
    let n = 0;
    const api = server({
      integrations: [integ()],
      routes: {
        'GET /api/v1/orgs/:org/integrations/:integration/available-repos': () => {
          n++;
          return n === 1
            ? { status: 429, json: { error: MARKER, code: 'rate_limited' }, headers: { 'Retry-After': '20' } }
            : { json: { items: [ar('7')], nextCursor: null } };
        },
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    await user.click(await screen.findByRole('button', { name: 'Find repositories' }));
    expect(await screen.findByText(/Too many tries. Wait 20 seconds/)).toBeInTheDocument();
    expect(availCalls(api)).toHaveLength(1);
    await user.click(within(screen.getByRole('search')).getByRole('button', { name: 'Search' }));
    expect(await screen.findByRole('button', { name: 'Map orbit/7' })).toBeInTheDocument();
    expect(availCalls(api)).toHaveLength(2);
    expect(document.body.textContent).not.toContain(MARKER);
    // A second press with the same search and no error asks nothing.
    await user.click(within(screen.getByRole('search')).getByRole('button', { name: 'Search' }));
    expect(availCalls(api)).toHaveLength(2);
  });

  it('a failed Load more keeps the loaded rows, shows the error beside them, and Load more retries', async () => {
    let second = 0;
    const api = server({
      integrations: [integ()],
      routes: {
        'GET /api/v1/orgs/:org/integrations/:integration/available-repos': (r) => {
          if (r.url.searchParams.get('cursor') !== 'C1')
            return { json: { items: [ar('1'), ar('2')], nextCursor: 'C1' } };
          second++;
          return second === 1
            ? { status: 502, json: { error: MARKER, code: 'provider_error' } }
            : { json: { items: [ar('3')], nextCursor: null } };
        },
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    await user.click(await screen.findByRole('button', { name: 'Find repositories' }));
    const found = await screen.findByRole('list', { name: 'Repositories on the host' });
    await user.click(screen.getByRole('button', { name: 'Load more' }));
    expect(await screen.findByText(/answered with an error/)).toBeInTheDocument();
    expect(within(found).getAllByRole('listitem')).toHaveLength(2);
    expect(document.body.textContent).not.toContain(MARKER);
    await user.click(screen.getByRole('button', { name: 'Load more' }));
    await waitFor(() => expect(within(found).getAllByRole('listitem')).toHaveLength(3));
    expect(availCalls(api)).toHaveLength(3);
  });

  it('the rotate section stays when the repo list fails, and Commit waits in token mode', async () => {
    server({
      integrations: [integ({ secretPending: true })],
      routes: {
        'GET /api/v1/orgs/:org/integrations/:integration/repos': () => ({ status: 500, json: {} }),
      },
    });
    renderApp('/orbit/settings/integrations/i1');
    expect(await screen.findByText('Could not load the repositories')).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Webhook secret' })).toBeInTheDocument();
    expect(screen.getByText(/The list of hooks did not load/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Commit' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Discard new secret' })).toBeEnabled();
  });

  it('an uncommitted rotation with no failed repos has its own note', async () => {
    server({
      integrations: [integ()],
      routes: {
        'POST /api/v1/orgs/:org/integrations/:integration/rotate-secret': () => ({
          json: { ...integ({ secretPending: true }), committed: false, failedRepos: [] },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    await user.click(await screen.findByRole('button', { name: 'Rotate secret' }));
    expect(await screen.findByText(/stored but not committed yet/)).toBeInTheDocument();
    expect(document.body.textContent).not.toContain('0 hooks');
  });

  it('copy feedback lives in a status region that is there before the press, and says when copy fails', async () => {
    server({
      integrations: [integ({ mode: 'webhook_only', tokenSet: false })],
      routes: {
        'POST /api/v1/orgs/:org/integrations/:integration/rotate-secret': () => ({
          json: {
            ...integ({ mode: 'webhook_only', secretPending: true }),
            webhookSecret: NEW_SECRET,
            committed: false,
            failedRepos: [],
          },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    await user.click(await screen.findByRole('button', { name: 'Rotate secret' }));
    const dialog = await screen.findByRole('dialog', { name: 'Copy the new secret' });
    const regions = within(dialog).getAllByRole('status');
    expect(regions.some((r) => r.textContent === '')).toBe(true);
    vi.spyOn(navigator.clipboard, 'writeText').mockRejectedValue(new Error('denied'));
    await user.click(within(dialog).getByRole('button', { name: 'Copy secret' }));
    expect(await within(dialog).findByText(/Could not copy/)).toBeInTheDocument();
    vi.restoreAllMocks();
  });

  it('copies the webhook address and says so', async () => {
    server({ integrations: [integ({ mode: 'webhook_only', tokenSet: false })] });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    await user.click(await screen.findByRole('button', { name: 'Copy address' }));
    expect(await screen.findByText('Copied.')).toBeInTheDocument();
  });

  it('the webhook-only id hint for GitHub names the API call', async () => {
    server({
      integrations: [
        integ({ provider: 'github', mode: 'webhook_only', tokenSet: false, baseUrl: 'https://github.com' }),
      ],
    });
    renderApp('/orbit/settings/integrations/i1');
    expect(await screen.findByText(/GET \/repos\/\{owner\}\/\{repo\}/)).toBeInTheDocument();
  });
});

describe('refresh hooks and backfill (LAUN-95)', () => {
  const HOOK = '/api/v1/orgs/:org/integrations/:integration/repos/:repo/hook/refresh';
  const FILL = '/api/v1/orgs/:org/integrations/:integration/repos/:repo/backfill';
  const reposRoute = {
    'GET /api/v1/orgs/:org/integrations/:integration/repos': () => list([repo('a'), repo('b')]),
  };

  it('token mode: both buttons are in each row; the answers are said in plain words', async () => {
    const api = server({
      integrations: [integ({ repoCount: 2 })],
      routes: {
        ...reposRoute,
        [`POST ${HOOK}`]: () => ({ json: { hookState: 'active' } }),
        [`POST ${FILL}`]: () => ({ json: { seen: 12, linked: 3, moved: 1, truncated: true } }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    const table = await screen.findByRole('table', { name: 'Mapped repositories' });
    await user.click(within(table).getByRole('button', { name: 'Refresh hooks for orbit/a' }));
    expect(await screen.findByText(/orbit\/a: The hook is up to date and working\./)).toBeInTheDocument();
    expect(posts(api, '/repos/a/hook/refresh')).toHaveLength(1);
    await user.click(within(table).getByRole('button', { name: 'Backfill open MRs for orbit/b' }));
    const note = await screen.findByText(/orbit\/b: Read 12 open requests\./);
    expect(note).toHaveTextContent('3 tickets got a new link.');
    expect(note).toHaveTextContent('1 ticket moved forward.');
    expect(note).toHaveTextContent('More open requests were left.');
    expect(posts(api, '/repos/b/backfill')).toHaveLength(1);
  });

  it('a failed hook shows the new hook state', async () => {
    let state: 'active' | 'failed' = 'failed';
    server({
      integrations: [integ({ repoCount: 1 })],
      routes: {
        'GET /api/v1/orgs/:org/integrations/:integration/repos': () =>
          list([
            repo('a', { hookState: state, hookError: state === 'failed' ? 'provider_forbidden' : null }),
          ]),
        [`POST ${HOOK}`]: () => {
          state = 'active';
          return { json: { hookState: 'active' } };
        },
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    const table = await screen.findByRole('table', { name: 'Mapped repositories' });
    expect(within(table).getByText('Hook failed: the token lacks a scope')).toBeInTheDocument();
    await user.click(within(table).getByRole('button', { name: 'Refresh hooks for orbit/a' }));
    expect(await within(table).findByText('Hook working')).toBeInTheDocument();
  });

  it('webhook only: no refresh and no backfill button', async () => {
    server({
      integrations: [integ({ mode: 'webhook_only', tokenSet: false, repoCount: 1 })],
      routes: {
        'GET /api/v1/orgs/:org/integrations/:integration/repos': () =>
          list([repo('a', { hookState: 'none' })]),
      },
    });
    renderApp('/orbit/settings/integrations/i1');
    const table = await screen.findByRole('table', { name: 'Mapped repositories' });
    expect(within(table).getByRole('button', { name: 'Unmap orbit/a' })).toBeInTheDocument();
    expect(within(table).queryByRole('button', { name: /Refresh hooks/ })).not.toBeInTheDocument();
    expect(within(table).queryByRole('button', { name: /Backfill/ })).not.toBeInTheDocument();
  });

  it.each([
    ['409 on refresh', HOOK, 409, 'conflict', /secret rotation may be waiting/],
    ['502 on refresh', HOOK, 502, 'provider_error', /The host answered with an error/],
    ['429 on refresh', HOOK, 429, 'rate_limited', /Too many tries\. Wait/],
    ['409 on backfill', FILL, 409, 'conflict', /has no token/],
    ['502 on backfill', FILL, 502, 'provider_unreachable', /could not reach the host/],
    ['429 on backfill', FILL, 429, 'rate_limited', /Too many tries\. Wait/],
  ])('%s says it in plain words and never shows the server text', async (_n, route, status, code, re) => {
    server({
      integrations: [integ({ repoCount: 1 })],
      routes: {
        'GET /api/v1/orgs/:org/integrations/:integration/repos': () => list([repo('a')]),
        [`POST ${route}`]: () => ({
          status,
          json: { error: MARKER, code },
          headers: { 'Retry-After': '30' },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    const table = await screen.findByRole('table', { name: 'Mapped repositories' });
    const name = route === HOOK ? 'Refresh hooks for orbit/a' : 'Backfill open MRs for orbit/a';
    await user.click(within(table).getByRole('button', { name }));
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(re);
    expect(alert).not.toHaveTextContent(MARKER);
    expect(alert).toHaveTextContent('orbit/a:');
  });

  it('while a call runs both buttons stay focusable (aria-disabled) and a second press sends nothing', async () => {
    let release: (v: unknown) => void = () => {};
    const gate = new Promise((r) => (release = r));
    const api = server({
      integrations: [integ({ repoCount: 1 })],
      routes: {
        'GET /api/v1/orgs/:org/integrations/:integration/repos': () => list([repo('a')]),
        [`POST ${HOOK}`]: async () => {
          await gate;
          return { json: { hookState: 'active' } };
        },
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    const table = await screen.findByRole('table', { name: 'Mapped repositories' });
    const refresh = within(table).getByRole('button', { name: 'Refresh hooks for orbit/a' });
    const fill = within(table).getByRole('button', { name: 'Backfill open MRs for orbit/a' });
    // The accessible name contains the visible text (WCAG 2.5.3).
    expect(refresh).toHaveAccessibleName(expect.stringContaining(refresh.textContent!));
    expect(fill).toHaveAccessibleName(expect.stringContaining(fill.textContent!));
    refresh.focus();
    await user.keyboard('{Enter}');
    await waitFor(() => expect(refresh).toHaveAttribute('aria-disabled', 'true'));
    expect(fill).toHaveAttribute('aria-disabled', 'true');
    expect(refresh).not.toBeDisabled();
    expect(refresh).toHaveFocus();
    await user.click(refresh);
    await user.click(fill);
    expect(posts(api, '/hook/refresh')).toHaveLength(1);
    expect(posts(api, '/backfill')).toHaveLength(0);
    release(null);
    await screen.findByText(/The hook is up to date and working/);
    expect(refresh).not.toHaveAttribute('aria-disabled');
  });
});

describe('LAUN-93 follow-ups', () => {
  async function openConnect() {
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations');
    await screen.findByText('No git connections yet');
    await user.click(screen.getByRole('button', { name: 'Connect' }));
    const dialog = await screen.findByRole('dialog', { name: 'Connect a git host' });
    return { user, dialog };
  }

  it('does not send a secret pasted for GitLab after the provider changes to GitHub', async () => {
    const api = server({
      routes: {
        'POST /api/v1/orgs/:org/integrations': () => ({
          status: 201,
          json: {
            ...integ({ id: 'new', provider: 'github', mode: 'webhook_only' }),
            webhookSecret: NEW_SECRET,
          },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations');
    await screen.findByText('No git connections yet');
    await user.click(screen.getByRole('button', { name: 'Connect' }));
    const dialog = await screen.findByRole('dialog', { name: 'Connect a git host' });
    await user.click(within(dialog).getByRole('button', { name: 'Next' }));
    await user.click(within(dialog).getByRole('radio', { name: /Webhook only/ }));
    await user.type(within(dialog).getByLabelText('Use my own secret (optional)'), SECRET);
    await user.click(within(dialog).getByRole('button', { name: 'Back' }));
    await user.click(within(dialog).getByRole('radio', { name: 'GitHub' }));
    await user.click(within(dialog).getByRole('button', { name: 'Next' }));
    await user.click(within(dialog).getByRole('button', { name: 'Connect' }));
    await screen.findByRole('dialog', { name: 'Copy the webhook secret' });
    const body = posts(api, '/integrations')[0]!.body as Record<string, unknown>;
    expect(body.provider).toBe('github');
    expect(body).not.toHaveProperty('secret');
    expect(JSON.stringify(body)).not.toContain(SECRET);
  });

  it('ignores Escape, the close button and Cancel while the create is in flight, and keeps the one-time secret', async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => (release = r));
    server({
      routes: {
        'POST /api/v1/orgs/:org/integrations': async () => {
          await gate;
          return {
            status: 201,
            json: { ...integ({ id: 'new', mode: 'webhook_only' }), webhookSecret: NEW_SECRET },
          };
        },
      },
    });
    const { user, dialog } = await openConnect();
    await user.click(within(dialog).getByRole('button', { name: 'Next' }));
    await user.click(within(dialog).getByRole('radio', { name: /Webhook only/ }));
    await user.click(within(dialog).getByRole('button', { name: 'Connect' }));
    const busy = await within(dialog).findByRole('button', { name: 'Connecting…' });
    expect(busy).toHaveAttribute('aria-disabled', 'true');
    await user.keyboard('{Escape}');
    await user.click(within(dialog).getByRole('button', { name: /close/i }));
    expect(screen.getByRole('dialog', { name: 'Connect a git host' })).toBeInTheDocument();
    // A second press while busy sends nothing more.
    await user.click(busy);
    release();
    const secret = await screen.findByRole('dialog', { name: 'Copy the webhook secret' });
    expect(within(secret).getByTestId('new-git-secret')).toHaveTextContent(NEW_SECRET);
  });

  it('counts every hook that is not active as not moved, like the server', async () => {
    server({
      integrations: [integ({ secretPending: true, repoCount: 2 })],
      routes: {
        'GET /api/v1/orgs/:org/integrations/:integration/repos': () =>
          list([repo('a'), repo('n', { hookState: 'none' })]),
      },
    });
    renderApp('/orbit/settings/integrations/i1');
    const section = (await screen.findByRole('heading', { name: 'Webhook secret' })).closest('section')!;
    expect(await within(section).findByText(/orbit\/n: You manage the hook/)).toBeInTheDocument();
    expect(within(section).getByRole('button', { name: 'Commit' })).toBeDisabled();
    expect(within(section).getByRole('button', { name: 'Commit anyway' })).toBeInTheDocument();
  });

  it('does not refetch a search whose query failed when the search text goes back to it', async () => {
    const calls: string[] = [];
    const api = server({
      integrations: [integ()],
      routes: {
        'GET /api/v1/orgs/:org/integrations/:integration/available-repos': (r) => {
          const q = r.url.searchParams.get('q') ?? '';
          calls.push(q);
          return q === 'bad'
            ? { status: 502, json: { error: MARKER, code: 'provider_error' } }
            : { json: { items: [], nextCursor: null } };
        },
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    await user.click(await screen.findByRole('button', { name: 'Find repositories' }));
    await waitFor(() => expect(calls).toEqual(['']));
    const box = screen.getByLabelText('Search repositories');
    const search = () => within(screen.getByRole('search')).getByRole('button', { name: 'Search' });
    await user.type(box, 'bad');
    await user.click(search());
    await waitFor(() => expect(calls).toEqual(['', 'bad']));
    await user.clear(box);
    await user.type(box, 'ok');
    await user.click(search());
    await waitFor(() => expect(calls).toEqual(['', 'bad', 'ok']));
    await user.clear(box);
    await user.type(box, 'bad');
    await user.click(search());
    expect(await screen.findByText(/answered with an error/)).toBeInTheDocument();
    expect(calls).toEqual(['', 'bad', 'ok']);
    expect(api.calls.filter((c) => c.url.pathname.endsWith('/available-repos'))).toHaveLength(3);
  });

  it('the page wrapper and every dialog panel carry the touch class', async () => {
    expect(touchClass).toBeTruthy();
    server({ integrations: [integ({ secretPending: true })] });
    const user = userEvent.setup();
    const view = renderApp('/orbit/settings/integrations/i1');
    const h1 = await screen.findByRole('heading', { name: /GitLab: gitlab.com/ });
    expect(h1.parentElement!.querySelector(`.${touchClass}`)).not.toBeNull();
    await user.click(await screen.findByRole('button', { name: 'Discard new secret' }));
    const discard = await screen.findByRole('dialog', { name: 'Discard the new secret?' });
    expect(discard).toHaveClass(touchClass!);
    await user.click(within(discard).getByRole('button', { name: 'Cancel' }));
    await user.click(screen.getByRole('button', { name: 'Disable' }));
    expect(await screen.findByRole('dialog', { name: 'Disable this connection?' })).toHaveClass(touchClass!);
    view.unmount();
  });
});

describe('deliveries (LAUN-87)', () => {
  const delivery = (id: string, over: Record<string, unknown> = {}) => ({
    id,
    integrationId: 'i1',
    provider: 'gitlab',
    receivedAt: '2026-10-26T10:00:00Z',
    result: 'processed',
    event: 'Merge Request Hook',
    repoKey: '101',
    httpStatus: 200,
    errorCode: null,
    ...over,
  });
  const route = 'GET /api/v1/orgs/:org/integrations/:integration/deliveries';

  it('lists the rows in the server order with plain words, and labels an unbound row', async () => {
    server({
      integrations: [integ()],
      routes: {
        [route]: () =>
          list([
            delivery('d1', { event: 'Merge Request Hook' }),
            delivery('d2', {
              integrationId: null,
              result: 'bad_signature',
              event: '',
              repoKey: null,
              httpStatus: 401,
              errorCode: 'unauthenticated',
            }),
            delivery('d3', {
              result: 'error',
              httpStatus: 413,
              errorCode: 'payload_too_large',
              repoKey: '<b>x</b>',
            }),
          ]),
      },
    });
    renderApp('/orbit/settings/integrations/i1');
    const table = await screen.findByRole('table', { name: 'Recent deliveries' });
    const rows = within(table).getAllByRole('row');
    expect(rows).toHaveLength(4);
    expect(within(rows[1]!).getByText('Processed')).toBeInTheDocument();
    expect(within(rows[1]!).getByText('Merge Request Hook')).toBeInTheDocument();
    expect(within(rows[2]!).getByText('no integration matched')).toBeInTheDocument();
    expect(within(rows[2]!).getByText('401')).toBeInTheDocument();
    expect(within(rows[2]!).getByText('The secret or signature did not match.')).toBeInTheDocument();
    expect(within(rows[3]!).getByText('The request was too large.')).toBeInTheDocument();
    // Text nodes only: markup in a value is not parsed.
    expect(within(rows[3]!).getByText('<b>x</b>')).toBeInTheDocument();
    expect(table.querySelector('b')).toBeNull();
    expect(document.body.textContent).not.toContain('payload_too_large');
  });

  it('shows an empty state', async () => {
    server({ integrations: [integ()], routes: { [route]: () => list([]) } });
    renderApp('/orbit/settings/integrations/i1');
    expect(await screen.findByText('No deliveries yet', { selector: 'h2, h3, div, p' })).toBeInTheDocument();
  });

  it('shows a fixed error with a retry that asks again', async () => {
    let n = 0;
    server({
      integrations: [integ()],
      routes: {
        [route]: () => {
          n++;
          return n === 1
            ? { status: 500, json: { error: MARKER, code: 'internal_error' } }
            : list([delivery('d1')]);
        },
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    expect(await screen.findByText('Could not load the deliveries')).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
    const box = screen.getByText('Could not load the deliveries').closest('div')!;
    await user.click(within(box).getByRole('button', { name: 'Try again' }));
    expect(await screen.findByRole('table', { name: 'Recent deliveries' })).toBeInTheDocument();
  });

  it('shows the last delivery on the list row and the summary', async () => {
    const last = {
      receivedAt: '2026-10-26T10:00:00Z',
      result: 'duplicate',
      httpStatus: 200,
      errorCode: null,
    };
    server({
      integrations: [integ({ lastDelivery: last }), integ({ id: 'i2', name: 'second' })],
      routes: { [route]: () => list([]) },
    });
    const view = renderApp('/orbit/settings/integrations');
    const rows = await screen.findAllByRole('listitem');
    expect(within(rows[0]!).getByText(/Last delivery: Duplicate \(already handled\),/)).toBeInTheDocument();
    expect(within(rows[1]!).getByText(/Last delivery: No deliveries yet/)).toBeInTheDocument();
    view.unmount();
    renderApp('/orbit/settings/integrations/i1');
    expect(await screen.findByText('Last delivery')).toBeInTheDocument();
    expect(screen.getByText(/^Duplicate \(already handled\),/)).toBeInTheDocument();
  });
});

describe('real reasons for a 409 and removing a connection (LAUN-104)', () => {
  const conflict = (code: string) => ({ status: 409, json: { error: `${MARKER} ${TOKEN}`, code } });

  it('every 409 code has its own fixed sentence, never "reload" and never server words', async () => {
    const { ApiError } = await import('../../../api/errors');
    const { gitErrorText } = await import('./gitText');
    const seen = new Set<string>();
    for (const code of [
      'integration_exists',
      'secret_in_use',
      'secret_hash_only',
      'repos_mapped',
      'integration_changed',
    ]) {
      const t = gitErrorText(new ApiError(409, code, `${MARKER} ${TOKEN}`), 'connect');
      expect(t).not.toMatch(/reload/i);
      expect(t).not.toContain(MARKER);
      seen.add(t);
    }
    expect(seen.size).toBe(5);
    expect(gitErrorText(new ApiError(409, 'repos_mapped', 'x'), 'remove')).toContain('Unmap them first');
    for (const ctx of [
      'connect',
      'token',
      'rotate',
      'repos',
      'check',
      'hook',
      'backfill',
      'remove',
      'other',
    ] as const)
      expect(gitErrorText(new ApiError(409, 'conflict', 'x'), ctx)).not.toMatch(/reload/i);
    expect(gitErrorText(new ApiError(404, 'not_found', 'x'))).not.toMatch(/reload/i);
  });

  it('removes a connection after a confirm that names it, then goes back to the list', async () => {
    const api = server({
      integrations: [integ()],
      routes: { 'DELETE /api/v1/orgs/:org/integrations/:integration': () => ({ status: 204 }) },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    await user.click(await screen.findByRole('button', { name: 'Remove connection' }));
    const dialog = await screen.findByRole('dialog', { name: 'Remove GitLab: gitlab.com?' });
    expect(dialog).toHaveClass(touchClass!);
    expect(within(dialog).getByText(/must be unmapped first/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Remove connection' }));
    await waitFor(() => expect(api.calls.some((c) => c.method === 'DELETE')).toBe(true));
    expect(api.calls.find((c) => c.method === 'DELETE')!.url.pathname).toBe(
      '/api/v1/orgs/orbit/integrations/i1',
    );
    expect(await screen.findByRole('heading', { name: 'Integrations' })).toBeInTheDocument();
  });

  it('removing with repositories mapped shows the fixed reason and keeps the dialog', async () => {
    server({
      integrations: [integ({ repoCount: 1 })],
      routes: { 'DELETE /api/v1/orgs/:org/integrations/:integration': () => conflict('repos_mapped') },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    await user.click(await screen.findByRole('button', { name: 'Remove connection' }));
    const dialog = await screen.findByRole('dialog');
    await user.click(within(dialog).getByRole('button', { name: 'Remove connection' }));
    const alert = await within(dialog).findByRole('alert');
    expect(alert).toHaveTextContent('still has mapped repositories');
    expect(document.body.innerHTML).not.toContain(MARKER);
  });

  it('a hash-only secret offers to rotate first, and the button starts the rotation', async () => {
    const api = server({
      integrations: [integ({ mode: 'webhook_only', tokenSet: false })],
      routes: {
        'PATCH /api/v1/orgs/:org/integrations/:integration': () => conflict('secret_hash_only'),
        'POST /api/v1/orgs/:org/integrations/:integration/rotate-secret': () => ({
          json: {
            ...integ({ mode: 'webhook_only', secretPending: true }),
            webhookSecret: NEW_SECRET,
            committed: false,
            failedRepos: [],
          },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations/i1');
    await user.click(await screen.findByRole('button', { name: 'Add a token' }));
    const dialog = await screen.findByRole('dialog', { name: 'Add a token' });
    await user.type(within(dialog).getByLabelText('New token'), TOKEN);
    await user.click(within(dialog).getByRole('button', { name: 'Save token' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('Rotate the webhook secret first');
    expect(dialog).not.toHaveTextContent(/reload/i);
    await user.click(within(dialog).getByRole('button', { name: 'Rotate the webhook secret first' }));
    expect(await screen.findByRole('dialog', { name: 'Copy the new secret' })).toBeInTheDocument();
    expect(posts(api, '/rotate-secret')).toHaveLength(1);
  });

  it('a duplicate in the connect dialog points at the existing connection', async () => {
    server({
      integrations: [integ()],
      routes: { 'POST /api/v1/orgs/:org/integrations': () => conflict('integration_exists') },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/integrations');
    await user.click(await screen.findByRole('button', { name: 'Connect' }));
    const dialog = await screen.findByRole('dialog', { name: 'Connect a git host' });
    await user.click(within(dialog).getByRole('button', { name: 'Next' }));
    await user.type(await within(dialog).findByLabelText('Token'), TOKEN);
    await user.click(within(dialog).getByRole('button', { name: 'Connect' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('already exists');
    expect(within(dialog).getByRole('link', { name: 'Open the existing connection' })).toHaveAttribute(
      'href',
      '/orbit/settings/integrations/i1',
    );
    expect(document.body.innerHTML).not.toContain(TOKEN);
  });
});
