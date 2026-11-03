import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { baseRoutes, mockApi, team, type Handler } from '../../test/mockApi';
import { renderApp } from '../../test/render';
import { keys } from '../../api/keys';
import { chatErrorText, CHAT_KINDS, CHAT_MAX_NAME } from './chatText';
import { ApiError } from '../../api/errors';

// LAUN-204: Settings > Chat services. The requests are the contract: POST/PATCH/DELETE on
// /chat-services and POST .../test. The webhook URL is write-only: the page never shows it.

const SLACK = 'https://hooks.slack.com/services/T0001/B0001/SECRETTOKEN123';
const DISCORD = 'https://discord.com/api/webhooks/123456789/SECRETTOKEN123';
const FLUXER = 'https://fluxer.example.com/api/webhooks/123456789/SECRETTOKEN123';
const CUSTOM = 'https://chat.example.com/hooks/SECRETTOKEN123';

interface Svc {
  id: string;
  name: string;
  kind: 'slack' | 'discord' | 'fluxer' | 'custom';
  host: string;
  teamId: string;
  events: string[];
  enabled: boolean;
  lastOkAt: string | null;
  lastError: string | null;
  secretUnreadable: boolean;
  createdAt: string;
  updatedAt: string;
  rateCappedCount: number;
  lastRateCappedAt: string | null;
}
interface List {
  available: boolean;
  linksAvailable: boolean;
  limit: number;
  items: Svc[];
}

const svc = (over: Partial<Svc> = {}): Svc => ({
  id: 's1',
  name: 'Engineering',
  kind: 'slack',
  host: 'hooks.slack.com',
  teamId: 'team1',
  events: ['assigned', 'comment'],
  enabled: true,
  lastOkAt: null,
  lastError: null,
  secretUnreadable: false,
  createdAt: '2026-10-31T10:00:00Z',
  updatedAt: '2026-10-31T10:00:00Z',
  rateCappedCount: 0,
  lastRateCappedAt: null,
  ...over,
});
const base: List = { available: true, linksAvailable: true, limit: 10, items: [] };

function server(initial: List, routes: Record<string, Handler> = {}, role: 'admin' | 'member' = 'admin') {
  const cur = { v: structuredClone(initial) };
  let n = 100;
  const api = mockApi({
    ...baseRoutes(),
    'GET /api/v1/auth/session': () => ({
      json: {
        user: { id: 'u1', username: 'chen', name: 'Chen', email: 'c@e.com', isPlatformAdmin: false },
        orgs: [{ id: 'o1', slug: 'orbit', name: 'Orbit', role }],
        csrfToken: 'csrf-test',
      },
    }),
    'GET /api/v1/orgs/:org/onboarding': () => ({ json: { steps: [], remaining: 0, capabilities: [] } }),
    'GET /api/v1/orgs/:org/chat-services': () => ({ json: cur.v }),
    'POST /api/v1/orgs/:org/chat-services': (r) => {
      const b = r.body as Partial<Svc> & { webhookUrl: string };
      const kind = b.kind ?? 'slack';
      const created = svc({
        id: `s${++n}`,
        name: b.name!,
        kind,
        host: kind === 'slack' ? 'hooks.slack.com' : kind === 'discord' ? 'discord.com' : 'chat.example.com',
        teamId: b.teamId!,
        events: b.events ?? [],
        enabled: b.enabled ?? true,
      });
      cur.v = { ...cur.v, items: [...cur.v.items, created] };
      return { status: 201, json: created };
    },
    'PATCH /api/v1/orgs/:org/chat-services/:service': (r) => {
      const b = r.body as Partial<Svc>;
      cur.v = {
        ...cur.v,
        items: cur.v.items.map((s) => (s.id === r.params.service ? { ...s, ...b } : s)),
      };
      return { json: cur.v.items.find((s) => s.id === r.params.service) };
    },
    'DELETE /api/v1/orgs/:org/chat-services/:service': (r) => {
      cur.v = { ...cur.v, items: cur.v.items.filter((s) => s.id !== r.params.service) };
      return { status: 204 };
    },
    'POST /api/v1/orgs/:org/chat-services/:service/test': () => ({ json: { ok: true } }),
    ...routes,
  });
  return api;
}

const PATH = '/orbit/settings/chat-services';

describe('Chat services page (LAUN-204)', () => {
  it('says who gets a post: every ticked event, a person or an agent, and a mute silences it (LAUN-208)', async () => {
    server({ ...base, items: [svc()] });
    renderApp(PATH);
    await screen.findAllByTestId('chat-status');
    expect(
      screen.getByText(/whoever the issue belongs to and whoever made the change, a person or an agent/),
    ).toBeInTheDocument();
    // LAUN-209: the team rule is stated.
    expect(screen.getByText(/A team with no service gets no post/)).toBeInTheDocument();
    expect(screen.getByText(/add a service per team to get the others/)).toBeInTheDocument();
    expect(screen.getByText(/An event that belongs to no team .* posts nowhere/)).toBeInTheDocument();
    expect(
      screen.getByText(/If any active member has muted an issue, that issue posts nothing/),
    ).toBeInTheDocument();
    expect(screen.queryByText(/someone who is not an agent/)).toBeNull();
  });

  it('lists the services with name, kind, host and a status chip, and never a URL', async () => {
    server({
      ...base,
      items: [
        svc({ lastOkAt: '2026-10-31T10:00:00Z' }),
        svc({ id: 's2', name: 'Alerts', kind: 'discord', host: 'discord.com', enabled: false }),
        svc({ id: 's3', name: 'Fluxer', kind: 'custom', host: 'chat.example.com', lastError: 'chat_gone' }),
      ],
    });
    renderApp(PATH);
    expect(await screen.findByRole('heading', { name: 'Chat services' })).toBeInTheDocument();
    const rows = await screen.findAllByTestId('chat-status');
    expect(rows.map((r) => r.textContent)).toEqual(['Working', 'Off', 'Last post failed']);
    expect(screen.getByText('hooks.slack.com')).toBeInTheDocument();
    expect(screen.getByText('Custom webhook')).toBeInTheDocument();
    expect(screen.getByText(/The service says the webhook does not exist any more/)).toBeInTheDocument();
    expect(screen.getByText('3 of 10 services.')).toBeInTheDocument();
    expect(document.body.innerHTML).not.toContain('SECRETTOKEN');
  });

  it('adds a Slack service with one POST, then the URL is nowhere on the page', async () => {
    const api = server(base);
    const user = userEvent.setup();
    renderApp(PATH);
    await user.click(await screen.findByRole('button', { name: 'Add service' }));
    const dlg = await screen.findByRole('dialog', { name: 'Add a chat service' });
    const url = within(dlg).getByLabelText('Webhook URL');
    expect(url).toHaveAttribute('type', 'password'); // the URL is a secret
    await user.type(within(dlg).getByLabelText('Name'), '  Engineering ');
    await user.type(url, `  ${SLACK}  `);
    await user.click(within(dlg).getByLabelText(/^Assigned/));
    await user.click(within(dlg).getByLabelText(/^Comments/));
    await user.click(within(dlg).getByRole('button', { name: 'Add service' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    const post = api.calls.find((c) => c.method === 'POST')!;
    expect(post.url.pathname).toBe('/api/v1/orgs/orbit/chat-services');
    expect(post.body).toEqual({
      name: 'Engineering',
      kind: 'slack',
      teamId: 'team1',
      webhookUrl: SLACK,
      events: ['assigned', 'comment'],
    });
    expect(await screen.findByText('Engineering')).toBeInTheDocument();
    expect(document.body.innerHTML).not.toContain('SECRETTOKEN123');
  });

  it('adds a Discord, a Fluxer and a custom service with their kind', async () => {
    const api = server(base);
    const user = userEvent.setup();
    renderApp(PATH);
    for (const [kind, label, url] of [
      ['discord', 'Discord', DISCORD],
      ['fluxer', 'Fluxer', FLUXER],
      ['custom', 'Custom webhook', CUSTOM],
    ] as const) {
      await user.click(await screen.findByRole('button', { name: 'Add service' }));
      const dlg = await screen.findByRole('dialog');
      await user.click(within(dlg).getByLabelText(label));
      await user.type(within(dlg).getByLabelText('Name'), `Svc ${kind}`);
      await user.type(within(dlg).getByLabelText('Webhook URL'), url);
      await user.click(within(dlg).getByRole('button', { name: 'Add service' }));
      await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
      expect(api.calls.filter((c) => c.method === 'POST').at(-1)!.body).toMatchObject({
        kind,
        webhookUrl: url,
      });
    }
  });

  it('the custom and Fluxer kinds say that private addresses are refused', async () => {
    server(base);
    const user = userEvent.setup();
    renderApp(PATH);
    await user.click(await screen.findByRole('button', { name: 'Add service' }));
    const dlg = await screen.findByRole('dialog');
    await user.click(within(dlg).getByLabelText('Custom webhook'));
    expect(
      within(dlg).getByText(/refuses private addresses, localhost, single-label names/),
    ).toBeInTheDocument();
    expect(within(dlg).queryByText(/not been verified/)).not.toBeInTheDocument();
    await user.click(within(dlg).getByLabelText('Fluxer'));
    expect(
      within(dlg).getByText(/refuses private addresses, localhost, single-label names/),
    ).toBeInTheDocument();
  });

  it('refuses a wrong URL for the kind with no request, focuses the field and names no value', async () => {
    const api = server(base);
    const user = userEvent.setup();
    renderApp(PATH);
    await user.click(await screen.findByRole('button', { name: 'Add service' }));
    const dlg = await screen.findByRole('dialog');
    await user.type(within(dlg).getByLabelText('Name'), 'X');
    const field = within(dlg).getByLabelText('Webhook URL');
    await user.click(within(dlg).getByRole('button', { name: 'Add service' }));
    expect(await within(dlg).findByText('Paste the webhook URL.')).toBeInTheDocument();
    await user.type(field, DISCORD); // a Discord URL for the Slack kind
    await user.click(within(dlg).getByRole('button', { name: 'Add service' }));
    expect(await within(dlg).findByText(/This is not a Slack Incoming Webhook URL/)).toBeInTheDocument();
    expect(field).toHaveAttribute('aria-invalid', 'true');
    expect(field).toHaveFocus();
    // The custom kind refuses http and a URL with a password.
    await user.click(within(dlg).getByLabelText('Custom webhook'));
    await user.clear(field);
    await user.type(field, 'http://chat.example.com/h');
    await user.click(within(dlg).getByRole('button', { name: 'Add service' }));
    expect(
      await within(dlg).findByText(/must start with https:\/\/ and name a public host/),
    ).toBeInTheDocument();
    expect(api.calls.filter((c) => c.method === 'POST')).toHaveLength(0);
  });

  describe('a service removed by another session while a dialog is open', () => {
    async function setup() {
      let items = [svc(), svc({ id: 's2', name: 'Alerts', kind: 'custom', host: 'chat.example.com' })];
      server(base, { 'GET /api/v1/orgs/:org/chat-services': () => ({ json: { ...base, items } }) });
      const user = userEvent.setup();
      const { client } = renderApp(PATH);
      await screen.findByText('Engineering');
      const drop = async (id: string) => {
        items = items.filter((x) => x.id !== id);
        await act(async () => {
          await client.invalidateQueries({ queryKey: keys.chat('orbit') });
        });
      };
      return { user, drop };
    }

    it('the Edit dialog closes and focus lands on Add service, not on the body', async () => {
      const { user, drop } = await setup();
      await user.click(await screen.findByRole('button', { name: 'Edit Engineering' }));
      await screen.findByRole('dialog');
      await drop('s1');
      await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
      await waitFor(() => expect(screen.getByRole('button', { name: 'Add service' })).toHaveFocus());
    });

    it('the Remove confirm closes and focus lands on Add service', async () => {
      const { user, drop } = await setup();
      await user.click(await screen.findByRole('button', { name: 'Remove Engineering' }));
      await screen.findByRole('dialog');
      await drop('s1');
      await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
      await waitFor(() => expect(screen.getByRole('button', { name: 'Add service' })).toHaveFocus());
    });

    it('a dialog of a service that is still there stays open', async () => {
      const { user, drop } = await setup();
      await user.click(await screen.findByRole('button', { name: 'Edit Engineering' }));
      await screen.findByRole('dialog');
      await drop('s2');
      expect(screen.getByRole('dialog')).toBeInTheDocument();
    });
  });

  it('the new-host sentence shows for a custom and a Fluxer service only', async () => {
    server({
      ...base,
      items: [
        svc(),
        svc({ id: 's2', name: 'Alerts', kind: 'custom', host: 'chat.example.com' }),
        svc({ id: 's4', name: 'Chatty', kind: 'fluxer', host: 'chat.example.com' }),
      ],
    });
    const user = userEvent.setup();
    renderApp(PATH);
    await user.click(await screen.findByRole('button', { name: 'Edit Engineering' }));
    let dlg = await screen.findByRole('dialog');
    expect(within(dlg).getByText(/host hooks\.slack\.com/)).toBeInTheDocument();
    expect(within(dlg).queryByText(/needs the whole new URL/)).not.toBeInTheDocument();
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    await user.click(screen.getByRole('button', { name: 'Edit Alerts' }));
    dlg = await screen.findByRole('dialog');
    expect(within(dlg).getByText(/needs the whole new URL/)).toBeInTheDocument();
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    await user.click(screen.getByRole('button', { name: 'Edit Chatty' }));
    dlg = await screen.findByRole('dialog');
    expect(
      within(dlg).getByText(/A Fluxer service that moves to another host needs the whole new URL/),
    ).toBeInTheDocument();
  });

  it('a name is required (1 to 40 characters)', async () => {
    const api = server(base);
    const user = userEvent.setup();
    renderApp(PATH);
    await user.click(await screen.findByRole('button', { name: 'Add service' }));
    const dlg = await screen.findByRole('dialog');
    await user.type(within(dlg).getByLabelText('Webhook URL'), SLACK);
    await user.click(within(dlg).getByRole('button', { name: 'Add service' }));
    expect(
      await within(dlg).findByText('Give the service a name of 1 to 40 characters.'),
    ).toBeInTheDocument();
    expect(within(dlg).getByLabelText('Name')).toHaveFocus();
    // The field stops at the server limit, so a longer name cannot be typed.
    expect(CHAT_MAX_NAME).toBe(40);
    expect(within(dlg).getByLabelText('Name')).toHaveAttribute('maxlength', '40');
    expect(api.calls.filter((c) => c.method === 'POST')).toHaveLength(0);
  });

  it('edit sends only what changed; the stored URL is kept; focus returns to the Edit button', async () => {
    const api = server({ ...base, items: [svc()] });
    const user = userEvent.setup();
    renderApp(PATH);
    await user.click(await screen.findByRole('button', { name: 'Edit Engineering' }));
    const dlg = await screen.findByRole('dialog', { name: 'Edit Engineering' });
    expect(within(dlg).queryByLabelText(/^Slack$/)).not.toBeInTheDocument(); // the kind cannot change
    expect(within(dlg).getByLabelText('New webhook URL')).toHaveValue('');
    expect(within(dlg).getByLabelText(/^Assigned/)).toBeChecked();
    await user.clear(within(dlg).getByLabelText('Name'));
    await user.type(within(dlg).getByLabelText('Name'), 'Eng');
    await user.click(within(dlg).getByLabelText(/^State changes/));
    await user.click(within(dlg).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    const patch = api.calls.find((c) => c.method === 'PATCH')!;
    expect(patch.url.pathname).toBe('/api/v1/orgs/orbit/chat-services/s1');
    expect(patch.body).toEqual({ name: 'Eng', events: ['assigned', 'comment', 'state_change'] });
    await waitFor(() => expect(screen.getByRole('button', { name: 'Edit Eng' })).toHaveFocus());
  });

  it('edit with a new URL and the switch off sends them; no change sends nothing', async () => {
    const api = server({ ...base, items: [svc({ kind: 'custom', host: 'chat.example.com' })] });
    const user = userEvent.setup();
    renderApp(PATH);
    await user.click(await screen.findByRole('button', { name: 'Edit Engineering' }));
    let dlg = await screen.findByRole('dialog');
    await user.click(within(dlg).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(api.calls.filter((c) => c.method === 'PATCH')).toHaveLength(0);
    await user.click(screen.getByRole('button', { name: 'Edit Engineering' }));
    dlg = await screen.findByRole('dialog');
    expect(within(dlg).getByText(/needs the whole new URL/)).toBeInTheDocument();
    await user.type(within(dlg).getByLabelText('New webhook URL'), 'https://other.example.org/in/AAA');
    await user.click(within(dlg).getByLabelText(/Post events to this service/));
    await user.click(within(dlg).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(api.calls.filter((c) => c.method === 'PATCH')).toHaveLength(1));
    expect(api.calls.find((c) => c.method === 'PATCH')!.body).toEqual({
      enabled: false,
      webhookUrl: 'https://other.example.org/in/AAA',
    });
  });

  it('Remove asks first, focus goes to Cancel, Cancel and Escape send nothing and focus comes back', async () => {
    const api = server({ ...base, items: [svc()] });
    const user = userEvent.setup();
    renderApp(PATH);
    const remove = await screen.findByRole('button', { name: 'Remove Engineering' });
    await user.click(remove);
    const dialog = await screen.findByRole('dialog', { name: 'Remove Engineering?' });
    expect(within(dialog).getByRole('button', { name: 'Cancel' })).toHaveFocus();
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(api.calls.filter((c) => c.method === 'DELETE')).toHaveLength(0);
    await waitFor(() => expect(remove).toHaveFocus());
    await user.click(remove);
    await screen.findByRole('dialog');
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    await waitFor(() => expect(remove).toHaveFocus());
  });

  it('Remove confirms with one DELETE for two synchronous clicks, and focus lands on a real element', async () => {
    const api = server({ ...base, items: [svc(), svc({ id: 's2', name: 'Alerts' })] });
    const user = userEvent.setup();
    renderApp(PATH);
    await user.click(await screen.findByRole('button', { name: 'Remove Engineering' }));
    const confirm = within(await screen.findByRole('dialog')).getByRole('button', { name: 'Remove service' });
    act(() => {
      confirm.click();
      confirm.click();
    });
    await waitFor(() => expect(screen.queryByText('Engineering')).not.toBeInTheDocument());
    expect(api.calls.filter((c) => c.method === 'DELETE')).toHaveLength(1);
    // The next row's Edit button, never <body>.
    await waitFor(() => expect(screen.getByRole('button', { name: 'Edit Alerts' })).toHaveFocus());
  });

  it('removing the last service focuses Add service', async () => {
    server({ ...base, items: [svc()] });
    const user = userEvent.setup();
    renderApp(PATH);
    await user.click(await screen.findByRole('button', { name: 'Remove Engineering' }));
    await user.click(
      within(await screen.findByRole('dialog')).getByRole('button', { name: 'Remove service' }),
    );
    await screen.findByText(/No chat service yet/);
    await waitFor(() => expect(screen.getByRole('button', { name: 'Add service' })).toHaveFocus());
  });

  it('two synchronous clicks on Add service save one POST; on Send test one POST', async () => {
    const api = server({ ...base, items: [svc()] });
    const user = userEvent.setup();
    renderApp(PATH);
    const test = await screen.findByRole('button', { name: 'Send test message to Engineering' });
    act(() => {
      test.click();
      test.click();
    });
    await screen.findByText('Test message sent. Check the channel.');
    expect(api.calls.filter((c) => c.url.pathname.endsWith('/test'))).toHaveLength(1);
    await user.click(screen.getByRole('button', { name: 'Add service' }));
    const dlg = await screen.findByRole('dialog');
    await user.type(within(dlg).getByLabelText('Name'), 'Two');
    await user.type(within(dlg).getByLabelText('Webhook URL'), SLACK);
    const add = within(dlg).getByRole('button', { name: 'Add service' });
    act(() => {
      add.click();
      add.click();
    });
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(api.calls.filter((c) => c.method === 'POST' && !c.url.pathname.endsWith('/test'))).toHaveLength(1);
  });

  it('a failed post shows the fixed sentence; a rate cap and an unreadable secret are shown', async () => {
    server({
      ...base,
      items: [
        svc({ lastError: 'chat_rejected', rateCappedCount: 3, lastRateCappedAt: '2026-10-31T10:00:00Z' }),
        svc({ id: 's2', name: 'Broken', secretUnreadable: true }),
      ],
    });
    renderApp(PATH);
    expect(await screen.findByText(/Last post failed\. The service refused the post/)).toBeInTheDocument();
    expect(screen.getByTestId('chat-capped')).toHaveTextContent(
      'The hourly limit dropped 3 posts in the last day',
    );
    expect(screen.getByText(/cannot be opened with this server key/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Send test message to Broken' })).toHaveAttribute(
      'aria-disabled',
      'true',
    );
  });

  it('a failed test shows a fixed sentence and not the server text; a 429 says to wait', async () => {
    server(
      { ...base, items: [svc()] },
      {
        'POST /api/v1/orgs/:org/chat-services/:service/test': () => ({
          status: 502,
          json: { error: 'the test message was not sent: chat_rejected', code: 'chat_send_failed' },
        }),
      },
    );
    const user = userEvent.setup();
    renderApp(PATH);
    await user.click(await screen.findByRole('button', { name: 'Send test message to Engineering' }));
    const out = await screen.findByTestId('chat-test-result');
    await waitFor(() =>
      expect(out).toHaveTextContent('The test message was not sent. The service refused the post.'),
    );
    expect(out).not.toHaveTextContent('chat_rejected');
  });

  it('a 429 on the test says to wait', async () => {
    server(
      { ...base, items: [svc()] },
      {
        'POST /api/v1/orgs/:org/chat-services/:service/test': () => ({
          status: 429,
          json: { error: 'x', code: 'rate_limited' },
        }),
      },
    );
    const user = userEvent.setup();
    renderApp(PATH);
    await user.click(await screen.findByRole('button', { name: 'Send test message to Engineering' }));
    expect(await screen.findByText(/You sent many test messages. Wait a moment/)).toBeInTheDocument();
  });

  it('a server without a key cannot store: Add service is off and the page says why', async () => {
    server({ ...base, available: false, items: [svc()] });
    const user = userEvent.setup();
    renderApp(PATH);
    expect(await screen.findByText(/no SETTINGS_ENCRYPTION_KEY/)).toBeInTheDocument();
    const add = screen.getByRole('button', { name: 'Add service' });
    expect(add).toHaveAttribute('aria-disabled', 'true');
    await user.click(add);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('at ten services Add service is off and says so', async () => {
    server({ ...base, items: Array.from({ length: 10 }, (_, i) => svc({ id: `s${i}`, name: `S${i}` })) });
    renderApp(PATH);
    expect(await screen.findByText(/You have 10 services, the most an org may have/)).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Add service' })).toHaveAttribute('aria-disabled', 'true');
  });

  it('warns when PUBLIC_BASE_URL is not set', async () => {
    server({ ...base, linksAvailable: false });
    renderApp(PATH);
    expect(await screen.findByText(/PUBLIC_BASE_URL is not set/)).toBeInTheDocument();
  });

  it('a failed load shows a retry; a failed add shows a fixed sentence in the dialog', async () => {
    server(base, {
      'GET /api/v1/orgs/:org/chat-services': () => ({ status: 500, json: { error: 'raw', code: 'x' } }),
    });
    renderApp(PATH);
    expect(await screen.findByText('Could not load the chat services')).toBeInTheDocument();
    expect(screen.queryByText('raw')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Try again' })).toBeInTheDocument();
  });

  it('a failed add keeps the dialog and shows a fixed sentence, not the server text', async () => {
    server(base, {
      'POST /api/v1/orgs/:org/chat-services': () => ({
        status: 500,
        json: { error: 'raw server words', code: 'x' },
      }),
    });
    const user = userEvent.setup();
    renderApp(PATH);
    await user.click(await screen.findByRole('button', { name: 'Add service' }));
    const dlg = await screen.findByRole('dialog');
    await user.type(within(dlg).getByLabelText('Name'), 'A');
    await user.type(within(dlg).getByLabelText('Webhook URL'), SLACK);
    await user.click(within(dlg).getByRole('button', { name: 'Add service' }));
    const alert = await within(dlg).findByRole('alert');
    expect(alert).toHaveTextContent('Could not add the service. Try again.');
    expect(alert).not.toHaveTextContent('raw server words');
  });

  it('a duplicate name (409) is a fixed sentence', async () => {
    server(base, {
      'POST /api/v1/orgs/:org/chat-services': () => ({ status: 409, json: { error: 'x', code: 'conflict' } }),
    });
    const user = userEvent.setup();
    renderApp(PATH);
    await user.click(await screen.findByRole('button', { name: 'Add service' }));
    const dlg = await screen.findByRole('dialog');
    await user.type(within(dlg).getByLabelText('Name'), 'A');
    await user.type(within(dlg).getByLabelText('Webhook URL'), SLACK);
    await user.click(within(dlg).getByRole('button', { name: 'Add service' }));
    expect(
      await within(dlg).findByText('A service with this name exists. Choose another name.'),
    ).toBeInTheDocument();
  });

  it('the old /settings/slack address goes to Chat services', async () => {
    server(base);
    renderApp('/orbit/settings/slack');
    expect(await screen.findByRole('heading', { name: 'Chat services' })).toBeInTheDocument();
  });

  it('a plain member is not offered the page; Slack and Discord find it for an admin', async () => {
    server(base, {}, 'member');
    const user = userEvent.setup();
    renderApp('/orbit/settings/notifications');
    await user.type(await screen.findByRole('searchbox', { name: 'Search settings' }), 'discord');
    expect(screen.getByText('No settings match.')).toBeInTheDocument();
  });

  it('settings search finds Chat services by Slack, Discord and Fluxer for an admin', async () => {
    server(base);
    const user = userEvent.setup();
    renderApp('/orbit/settings/notifications');
    const search = await screen.findByRole('searchbox', { name: 'Search settings' });
    for (const q of ['slack', 'discord', 'fluxer']) {
      await user.clear(search);
      await user.type(search, q);
      expect(await screen.findByRole('link', { name: /Chat services/ })).toBeInTheDocument();
    }
  });
});

describe('Chat services teams (LAUN-209)', () => {
  it('shows the team name on each card', async () => {
    server({ ...base, items: [svc()] });
    renderApp(PATH);
    expect(await screen.findByTestId('chat-team')).toHaveTextContent('Team Orbit');
  });

  it('the Add dialog has a required team picker and sends teamId', async () => {
    const api = server(base);
    const user = userEvent.setup();
    renderApp(PATH);
    await user.click(await screen.findByRole('button', { name: 'Add service' }));
    const dlg = await screen.findByRole('dialog', { name: 'Add a chat service' });
    const pick = within(dlg).getByLabelText('Team');
    expect(pick).toBeRequired();
    expect(within(dlg).getByRole('option', { name: 'Orbit (ORB)' })).toBeInTheDocument();
    expect(pick).toHaveValue('team1');
    await user.type(within(dlg).getByLabelText('Name'), 'Eng');
    await user.type(within(dlg).getByLabelText('Webhook URL'), SLACK);
    await user.click(within(dlg).getByRole('button', { name: 'Add service' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(api.calls.find((c) => c.method === 'POST')!.body).toMatchObject({ teamId: 'team1' });
  });

  it('the Edit dialog moves a service to another team with a PATCH of teamId alone', async () => {
    const api = server({ ...base, items: [svc({ teamId: 'gone-team' })] });
    const user = userEvent.setup();
    renderApp(PATH);
    await user.click(await screen.findByRole('button', { name: 'Edit Engineering' }));
    const dlg = await screen.findByRole('dialog', { name: 'Edit Engineering' });
    await user.selectOptions(within(dlg).getByLabelText('Team'), 'team1');
    await user.click(within(dlg).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(api.calls.find((c) => c.method === 'PATCH')!.body).toEqual({ teamId: 'team1' });
  });

  it('a 400 on the team is a fixed sentence', () => {
    expect(chatErrorText(new ApiError(400, 'validation_failed', 'teamId: unknown team'), 'save')).toBe(
      'Choose a team of this org.',
    );
  });
});

// LAUN-231 (decisions-T20 "Not done" chat item): team access does not apply to a channel, so the
// dialog always warns the admin who can read the posts. Nothing posted changes.
describe('Chat services channel warning (LAUN-231)', () => {
  const OPS = { ...team, id: 'team2', key: 'OPS', name: 'Operations' };
  const twoTeams: Record<string, Handler> = {
    'GET /api/v1/orgs/:org/teams': () => ({ json: { items: [team, OPS], nextCursor: null } }),
  };
  const warningFor = (key: string) =>
    `Everyone in this channel sees each post this service sends for ${key}: what happened, a link, the issue identifier, and for a state change the names of the old and new state. They see it whether or not they have access to ${key}. Launchpad cannot see who is in the channel.`;

  it('the Add dialog warns for the chosen team, follows a team change, and the select names it', async () => {
    server(base, twoTeams);
    const user = userEvent.setup();
    renderApp(PATH);
    await user.click(await screen.findByRole('button', { name: 'Add service' }));
    const dlg = await screen.findByRole('dialog', { name: 'Add a chat service' });
    const note = within(dlg).getByRole('note');
    expect(note).toHaveTextContent(warningFor('team ORB'));
    const pick = within(dlg).getByLabelText('Team');
    expect(pick.getAttribute('aria-describedby')?.split(' ')).toContain(note.id);
    expect(pick).toHaveAccessibleDescription(
      expect.stringContaining('Launchpad cannot see who is in the channel'),
    );
    await user.selectOptions(pick, 'team2');
    expect(within(dlg).getByRole('note')).toHaveTextContent(warningFor('team OPS'));
    expect(within(dlg).getByRole('note')).not.toHaveTextContent('team ORB');
  });

  it('the Edit dialog warns for the service team, and follows a team change', async () => {
    server({ ...base, items: [svc({ teamId: 'team2' })] }, twoTeams);
    const user = userEvent.setup();
    renderApp(PATH);
    await user.click(await screen.findByRole('button', { name: 'Edit Engineering' }));
    const dlg = await screen.findByRole('dialog', { name: 'Edit Engineering' });
    expect(within(dlg).getByRole('note')).toHaveTextContent(warningFor('team OPS'));
    await user.selectOptions(within(dlg).getByLabelText('Team'), 'team1');
    expect(within(dlg).getByRole('note')).toHaveTextContent(warningFor('team ORB'));
  });

  it('a service whose team is not in the list (archived) says "this team"', async () => {
    server({ ...base, items: [svc({ teamId: 'archived-team' })] }, twoTeams);
    const user = userEvent.setup();
    renderApp(PATH);
    await user.click(await screen.findByRole('button', { name: 'Edit Engineering' }));
    const dlg = await screen.findByRole('dialog', { name: 'Edit Engineering' });
    expect(within(dlg).getByRole('note')).toHaveTextContent(warningFor('this team'));
  });

  it('the page says that Launchpad cannot see who is in a channel', async () => {
    server({ ...base, items: [svc()] });
    renderApp(PATH);
    await screen.findAllByTestId('chat-status');
    expect(
      screen.getByText(/Launchpad cannot see who is in a channel: everyone in it sees the posts/),
    ).toBeInTheDocument();
  });

  it('a member who opens the address gets no page and no warning', async () => {
    const api = server({ ...base, items: [svc()] }, {}, 'member');
    renderApp(PATH);
    expect(await screen.findByText('You need admin access.')).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Add service' })).toBeNull();
    expect(screen.queryByRole('note')).toBeNull();
    expect(api.calls.some((c) => c.url.pathname.includes('/chat-services'))).toBe(false);
  });
});

describe('chatErrorText', () => {
  it('maps every status to a fixed sentence', () => {
    const e = (status: number, code = 'x', message = 'raw') => new ApiError(status, code, message);
    expect(chatErrorText(e(0), 'create')).toMatch(/Could not reach the server/);
    expect(chatErrorText(e(401), 'save')).toMatch(/signed out/);
    expect(chatErrorText(e(403), 'save')).toMatch(/admin access/);
    expect(chatErrorText(e(409, 'encryption_unavailable'), 'create')).toMatch(/SETTINGS_ENCRYPTION_KEY/);
    expect(chatErrorText(e(409, 'integration_limit'), 'create')).toMatch(/at most 10/);
    expect(chatErrorText(e(400), 'create', 'discord')).toMatch(/not a Discord webhook/);
    expect(chatErrorText(e(400), 'save', 'custom')).toMatch(/public host/);
    expect(chatErrorText(e(404), 'test')).toMatch(/no longer exists/);
    expect(
      chatErrorText(e(502, 'chat_send_failed', 'the test message was not sent: chat_unreachable'), 'test'),
    ).toMatch(/could not reach the service/);
    expect(chatErrorText(e(502, 'chat_send_failed', 'weird'), 'test')).toMatch(/did not accept it/);
    expect(chatErrorText(e(500), 'remove')).toMatch(/Could not remove/);
    expect(chatErrorText(new Error('boom'), 'save')).toBe('Something went wrong. Try again.');
  });

  it('the quick URL checks agree with the server rules', () => {
    const t = (k: string, u: string) => CHAT_KINDS.find((c) => c.value === k)!.test(u);
    expect(t('slack', SLACK)).toBe(true);
    expect(t('slack', DISCORD)).toBe(false);
    expect(t('discord', DISCORD)).toBe(true);
    expect(t('discord', 'https://discord.com.evil.example/api/webhooks/1/a')).toBe(false);
    expect(t('discord', 'https://ptb.discord.com/api/webhooks/1/a')).toBe(false);
    expect(t('fluxer', FLUXER)).toBe(true);
    expect(t('fluxer', 'https://fluxer.example.com/api/v1/webhooks/1/a')).toBe(true);
    expect(t('fluxer', CUSTOM)).toBe(false);
    expect(t('fluxer', FLUXER + '?x=1')).toBe(false);
    expect(t('fluxer', 'http://fluxer.example.com/api/webhooks/1/a')).toBe(false);
    expect(t('custom', CUSTOM)).toBe(true);
    expect(t('custom', 'http://chat.example.com/h')).toBe(false);
    expect(t('custom', 'https://u:p@chat.example.com/h')).toBe(false);
    expect(t('custom', 'not a url')).toBe(false);
  });
});
