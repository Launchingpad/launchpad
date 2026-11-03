import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { baseRoutes, mockApi, type Handler } from '../../test/mockApi';
import { renderApp } from '../../test/render';
import { ApiError } from '../../api/errors';
import { personalErrorText } from './personalWebhookText';
import { searchSettings, settingsPages } from './settingsPages';

// LAUN-229 (decisions-T40): Settings > Personal webhooks. A member's own webhooks: the requests are the
// contract (GET/POST /me/webhooks, PATCH/DELETE /me/webhooks/{id}, POST .../test). The URL is write-only.

const SLACK = 'https://hooks.slack.com/services/T0001/B0001/SECRETTOKEN123';

interface Hook {
  id: string;
  name: string;
  kind: 'slack' | 'discord' | 'fluxer' | 'custom';
  host: string;
  events: string[];
  enabled: boolean;
  lastOkAt: string | null;
  lastError: string | null;
  secretUnreadable: boolean;
  createdAt: string;
  updatedAt: string;
}
interface List {
  available: boolean;
  linksAvailable: boolean;
  limit: number;
  items: Hook[];
}

const hook = (over: Partial<Hook> = {}): Hook => ({
  id: 'h1',
  name: 'My Slack',
  kind: 'slack',
  host: 'hooks.slack.com',
  events: ['assigned', 'my_issues'],
  enabled: true,
  lastOkAt: null,
  lastError: null,
  secretUnreadable: false,
  createdAt: '2026-11-01T10:00:00Z',
  updatedAt: '2026-11-01T10:00:00Z',
  ...over,
});
const base: List = { available: true, linksAvailable: true, limit: 3, items: [] };

function server(initial: List, routes: Record<string, Handler> = {}, role: 'admin' | 'member' = 'member') {
  const cur = { v: structuredClone(initial) };
  let n = 100;
  return mockApi({
    ...baseRoutes(),
    'GET /api/v1/auth/session': () => ({
      json: {
        user: { id: 'u1', username: 'ann', name: 'Ann', email: 'ann@example.com', isPlatformAdmin: false },
        orgs: [{ id: 'o1', slug: 'orbit', name: 'Orbit', role }],
        csrfToken: 'csrf-test',
      },
    }),
    'GET /api/v1/orgs/:org/onboarding': () => ({ json: { steps: [], remaining: 0, capabilities: [] } }),
    'GET /api/v1/orgs/:org/me/webhooks': () => ({ json: cur.v }),
    'POST /api/v1/orgs/:org/me/webhooks': (r) => {
      const b = r.body as Partial<Hook> & { webhookUrl: string };
      const created = hook({ id: `h${++n}`, name: b.name!, kind: b.kind ?? 'slack', events: b.events ?? [] });
      cur.v = { ...cur.v, items: [...cur.v.items, created] };
      return { status: 201, json: created };
    },
    'PATCH /api/v1/orgs/:org/me/webhooks/:id': (r) => {
      const b = r.body as Partial<Hook>;
      cur.v = { ...cur.v, items: cur.v.items.map((s) => (s.id === r.params.id ? { ...s, ...b } : s)) };
      return { json: cur.v.items.find((s) => s.id === r.params.id) };
    },
    'DELETE /api/v1/orgs/:org/me/webhooks/:id': (r) => {
      cur.v = { ...cur.v, items: cur.v.items.filter((s) => s.id !== r.params.id) };
      return { status: 204 };
    },
    'POST /api/v1/orgs/:org/me/webhooks/:id/test': () => ({ json: { ok: true } }),
    ...routes,
  });
}

const PATH = '/orbit/settings/personal-webhooks';

describe('Personal webhooks page (LAUN-229)', () => {
  it('is open to a plain member and listed in the Account group and the search', async () => {
    server({ ...base, items: [hook({ lastOkAt: '2026-11-01T10:00:00Z' })] });
    renderApp(PATH);
    expect(await screen.findByRole('heading', { name: 'Personal webhooks' })).toBeInTheDocument();
    expect((await screen.findAllByTestId('pwh-status')).map((s) => s.textContent)).toEqual(['Working']);
    expect(screen.queryByText('You need admin access.')).toBeNull();
    const page = settingsPages.find((p) => p.path === 'personal-webhooks')!;
    expect(page.access).toBe('member');
    expect(page.group).toBe('Account');
    expect(searchSettings(settingsPages, 'assigned to me').map((h) => h.page.path)).toContain(
      'personal-webhooks',
    );
    // The rules are stated: own issues, not my own change, not a hidden team.
    expect(screen.getByText(/one you are assigned to or one you created/)).toBeInTheDocument();
    expect(screen.getByText(/for a change you make yourself/)).toBeInTheDocument();
    expect(screen.getByText(/a team you cannot see/)).toBeInTheDocument();
    expect(screen.getByText(/Only you see these webhooks/)).toBeInTheDocument();
  });

  it('adds a webhook with both choices on by default, and never shows the URL again', async () => {
    const api = server(base);
    renderApp(PATH);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Add webhook' }));
    const dlg = await screen.findByRole('dialog');
    expect(within(dlg).getByLabelText(/Assigned to me/)).toBeChecked();
    expect(within(dlg).getByLabelText(/Changes on my issues/)).toBeChecked();
    expect(within(dlg).getByLabelText('Webhook URL')).toHaveAttribute('type', 'password');
    await user.type(within(dlg).getByLabelText('Name'), 'Mine');
    await user.type(within(dlg).getByLabelText('Webhook URL'), SLACK);
    await user.click(within(dlg).getByRole('button', { name: 'Add webhook' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    const post = api.calls.find((c) => c.method === 'POST' && c.url.pathname.endsWith('/me/webhooks'))!;
    expect(post.body).toEqual({
      name: 'Mine',
      kind: 'slack',
      webhookUrl: SLACK,
      events: ['assigned', 'my_issues'],
    });
    expect(await screen.findByText('Mine')).toBeInTheDocument();
    expect(document.body.textContent).not.toContain('SECRETTOKEN123');
  });

  it('refuses a wrong URL before any request', async () => {
    const api = server(base);
    renderApp(PATH);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Add webhook' }));
    const dlg = await screen.findByRole('dialog');
    await user.type(within(dlg).getByLabelText('Name'), 'Mine');
    await user.type(within(dlg).getByLabelText('Webhook URL'), 'https://evil.example.com/x');
    await user.click(within(dlg).getByRole('button', { name: 'Add webhook' }));
    expect(await within(dlg).findByRole('alert')).toHaveTextContent(/not a Slack Incoming Webhook URL/);
    expect(api.calls.filter((c) => c.method === 'POST')).toHaveLength(0);
  });

  it('edits the choices and the switch; an empty URL keeps the stored one', async () => {
    const api = server({ ...base, items: [hook()] });
    renderApp(PATH);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Edit My Slack' }));
    const dlg = await screen.findByRole('dialog');
    expect(within(dlg).getByText(/Saved \(host hooks.slack.com\)/)).toBeInTheDocument();
    await user.click(within(dlg).getByLabelText(/Changes on my issues/));
    await user.click(within(dlg).getByLabelText(/Post to this webhook/));
    await user.click(within(dlg).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    const patch = api.calls.find((c) => c.method === 'PATCH')!;
    expect(patch.url.pathname).toContain('/me/webhooks/h1');
    expect(patch.body).toEqual({ enabled: false, events: ['assigned'] });
  });

  it('sends a test and removes a webhook', async () => {
    const api = server({ ...base, items: [hook()] });
    renderApp(PATH);
    const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Send test message to My Slack' }));
    expect(await screen.findByText('Test message sent. Check the channel.')).toBeInTheDocument();
    expect(
      api.calls.some((c) => c.method === 'POST' && c.url.pathname.endsWith('/me/webhooks/h1/test')),
    ).toBe(true);
    await user.click(screen.getByRole('button', { name: 'Remove My Slack' }));
    const dlg = await screen.findByRole('dialog');
    await user.click(within(dlg).getByRole('button', { name: 'Remove webhook' }));
    await waitFor(() => expect(screen.queryByText('My Slack')).toBeNull());
    expect(api.calls.some((c) => c.method === 'DELETE' && c.url.pathname.endsWith('/me/webhooks/h1'))).toBe(
      true,
    );
  });

  it('turns Add off at the limit and without a key', async () => {
    server({ ...base, items: [hook(), hook({ id: 'h2', name: 'B' }), hook({ id: 'h3', name: 'C' })] });
    renderApp(PATH);
    expect(await screen.findByRole('button', { name: 'Add webhook' })).toHaveAttribute(
      'aria-disabled',
      'true',
    );
    expect(screen.getByText(/You have 3 webhooks/)).toBeInTheDocument();
  });

  it('shows only the caller own list: an admin sees the same own-rows route, no other member', async () => {
    const api = server(base, {}, 'admin');
    renderApp(PATH);
    expect(await screen.findByText(/No personal webhook yet/)).toBeInTheDocument();
    // The page asks only for the caller's own rows; there is no admin route for others.
    const gets = api.calls.filter((c) => c.method === 'GET' && c.url.pathname.includes('webhooks'));
    expect(gets.every((c) => c.url.pathname.includes('/me/webhooks'))).toBe(true);
  });

  it('says a refused account cannot have webhooks (an agent token is 403)', async () => {
    server(base, {
      'GET /api/v1/orgs/:org/me/webhooks': () => ({
        status: 403,
        json: { error: 'RAW-SERVER-TEXT', code: 'forbidden' },
      }),
    });
    renderApp(PATH);
    expect(await screen.findByText('No personal webhooks for this account')).toBeInTheDocument();
    expect(document.body.textContent).not.toContain('RAW-SERVER-TEXT');
  });

  it('maps errors to fixed sentences', () => {
    const e = (status: number, code: string, message = 'x') => new ApiError(status, code, message);
    expect(personalErrorText(e(409, 'integration_limit'), 'create')).toMatch(/at most 3/);
    expect(personalErrorText(e(409, 'conflict'), 'create')).toMatch(/this name/);
    expect(
      personalErrorText(e(502, 'chat_send_failed', 'the test message was not sent: chat_gone'), 'test'),
    ).toMatch(/does not exist any more/);
    expect(personalErrorText(e(400, 'validation_failed'), 'save', 'slack')).toMatch(/Slack/);
  });
});
