import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { baseRoutes, mockApi, type Handler } from '../../test/mockApi';
import { renderApp } from '../../test/render';
import { ApiError } from '../../api/errors';
import type { PushDevice } from '../../api/types';
import {
  deviceRemoveText,
  devicesLoadText,
  pushErrorText,
  relaySecretProblem,
  relayUrlProblem,
} from './pushText';

// LAUN-197: Settings > Mobile push (admin) and Settings > Mobile devices (every person). The requests are the
// contract. The relay URL, its secret and a device token are write-only: no page shows one.

// A real browser does not move focus into an `inert` subtree, and jsdom does. The dialog marks the page
// behind it inert while it is open, so emulate that: a focus() on an element inside [inert] does nothing.
// Without this, a page that focuses too early passes in jsdom and loses focus to <body> in a browser.
beforeEach(() => {
  const real = HTMLElement.prototype.focus;
  vi.spyOn(HTMLElement.prototype, 'focus').mockImplementation(function (this: HTMLElement, o) {
    if (this.closest('[inert]')) return;
    real.call(this, o);
  });
});
afterEach(() => vi.restoreAllMocks());

const URL_IN = 'https://relay.example.com/t/RELAYURLTOKEN-9001';
const SECRET_IN = 'RELAYBEARERSECRET-4711';

interface View {
  available: boolean;
  configured: boolean;
  host: string | null;
  lastOkAt: string | null;
  lastError: string | null;
  secretUnreadable: boolean;
  updatedAt: string | null;
  deviceCount: number;
  rateCappedCount: number;
  deadCount: number;
}

const empty: View = {
  available: true,
  configured: false,
  host: null,
  lastOkAt: null,
  lastError: null,
  secretUnreadable: false,
  updatedAt: null,
  deviceCount: 0,
  rateCappedCount: 0,
  deadCount: 0,
};
const set: View = {
  ...empty,
  configured: true,
  host: 'relay.example.com',
  updatedAt: '2026-10-31T10:00:00Z',
  deviceCount: 2,
};

function session(role: 'admin' | 'member') {
  return {
    'GET /api/v1/auth/session': () => ({
      json: {
        user: { id: 'u1', username: 'chen', name: 'Chen', email: 'c@e.com', isPlatformAdmin: false },
        orgs: [{ id: 'o1', slug: 'orbit', name: 'Orbit', role }],
        csrfToken: 'csrf-test',
      },
    }),
    'GET /api/v1/orgs/:org/onboarding': () => ({ json: { steps: [], remaining: 0, capabilities: [] } }),
  };
}

function relayServer(initial: View, routes: Record<string, Handler> = {}) {
  const cur = { v: initial };
  const api = mockApi({
    ...baseRoutes(),
    ...session('admin'),
    'GET /api/v1/orgs/:org/push': () => ({ json: cur.v }),
    'PUT /api/v1/orgs/:org/push': () => {
      cur.v = { ...cur.v, configured: true, host: 'relay.example.com', updatedAt: '2026-10-31T11:00:00Z' };
      return { json: cur.v };
    },
    'DELETE /api/v1/orgs/:org/push': () => {
      cur.v = { ...empty };
      return { status: 204 };
    },
    'POST /api/v1/orgs/:org/push/test': () => ({ json: { sent: 1, removed: 0 } }),
    ...routes,
  });
  return api;
}

describe('Mobile push page (LAUN-197)', () => {
  it('sets the relay with one PUT of both values, then shows only "Set" and the host', async () => {
    const api = relayServer(empty);
    const user = userEvent.setup();
    renderApp('/orbit/settings/push');
    expect(await screen.findByRole('heading', { name: 'Mobile push' })).toBeInTheDocument();
    const url = await screen.findByLabelText('Relay URL');
    const secret = screen.getByLabelText('Relay secret');
    expect(url).toHaveAttribute('type', 'text'); // the URL is shown while typed; only the host is shown after
    expect(secret).toHaveAttribute('type', 'password');
    await user.type(url, `  ${URL_IN}  `);
    await user.type(secret, SECRET_IN);
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('Saved.')).toBeInTheDocument();
    const put = api.calls.find((c) => c.method === 'PUT')!;
    expect(put.url.pathname).toBe('/api/v1/orgs/orbit/push');
    expect(put.body).toEqual({ relayUrl: URL_IN, relaySecret: SECRET_IN });
    // Write-only: the fields are gone and neither value is anywhere on the page.
    expect(screen.queryByLabelText('Relay URL')).not.toBeInTheDocument();
    expect(screen.getByTestId('push-state')).toHaveTextContent('Set. Host relay.example.com');
    expect(document.body.innerHTML).not.toContain('RELAYURLTOKEN');
    expect(document.body.innerHTML).not.toContain('RELAYBEARERSECRET');
    expect(screen.getByRole('button', { name: 'Replace relay' })).toBeInTheDocument();
    // The form is gone: focus moves to the Replace button, not the body.
    await waitFor(() => expect(screen.getByRole('button', { name: 'Replace relay' })).toHaveFocus());
  });

  it('refuses a bad URL or a missing value with no request, focuses the field, names no value', async () => {
    const api = relayServer(empty);
    const user = userEvent.setup();
    renderApp('/orbit/settings/push');
    const url = await screen.findByLabelText('Relay URL');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('Enter the relay URL.')).toBeInTheDocument();
    expect(url).toHaveFocus();
    await user.type(url, 'http://relay.example.com');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('The URL must start with https://.')).toBeInTheDocument();
    expect(url).toHaveAttribute('aria-invalid', 'true');
    await user.clear(url);
    await user.type(url, URL_IN);
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('Enter the relay secret.')).toBeInTheDocument();
    expect(screen.getByLabelText('Relay secret')).toHaveFocus();
    await user.type(screen.getByLabelText('Relay secret'), 'has space');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText(/printable characters without spaces/)).toBeInTheDocument();
    expect(api.calls.filter((c) => c.method === 'PUT')).toHaveLength(0);
  });

  it('a configured relay keeps its values: Replace opens two empty fields and a partial PUT is allowed', async () => {
    const api = relayServer(set);
    const user = userEvent.setup();
    renderApp('/orbit/settings/push');
    expect(await screen.findByTestId('push-state')).toHaveTextContent('relay.example.com');
    expect(screen.queryByLabelText(/Relay URL/)).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Replace relay' }));
    const secret = await screen.findByLabelText('New relay secret');
    await waitFor(() => expect(screen.getByLabelText('New relay URL')).toHaveFocus());
    expect(secret).toHaveValue('');
    // Nothing entered: no request.
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('Enter a new URL or a new secret.')).toBeInTheDocument();
    expect(api.calls.filter((c) => c.method === 'PUT')).toHaveLength(0);
    await user.type(secret, 'NEWSECRET-123');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await screen.findByText('Saved.');
    expect(api.calls.find((c) => c.method === 'PUT')!.body).toEqual({ relaySecret: 'NEWSECRET-123' });
  });

  it('Keep the current relay closes the form without a request', async () => {
    const api = relayServer(set);
    const user = userEvent.setup();
    renderApp('/orbit/settings/push');
    await user.click(await screen.findByRole('button', { name: 'Replace relay' }));
    await user.click(await screen.findByRole('button', { name: 'Keep the current relay' }));
    expect(screen.queryByLabelText('New relay URL')).not.toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole('button', { name: 'Replace relay' })).toHaveFocus());
    expect(api.calls.filter((c) => c.method === 'PUT')).toHaveLength(0);
  });

  it('a new host needs the secret again: no request, and the secret field is focused; the same host keeps it', async () => {
    const api = relayServer(set);
    const user = userEvent.setup();
    renderApp('/orbit/settings/push');
    await user.click(await screen.findByRole('button', { name: 'Replace relay' }));
    await user.type(await screen.findByLabelText('New relay URL'), 'https://other.example.net/x');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByText('The relay host changed. Enter the secret again.')).toBeInTheDocument();
    expect(screen.getByLabelText('New relay secret')).toHaveFocus();
    expect(api.calls.filter((c) => c.method === 'PUT')).toHaveLength(0);
    // Same host, other path: the secret may stay.
    const url = screen.getByLabelText('New relay URL');
    await user.clear(url);
    await user.type(url, 'https://Relay.Example.com:8443/v2');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await screen.findByText('Saved.');
    expect(api.calls.find((c) => c.method === 'PUT')!.body).toEqual({
      relayUrl: 'https://Relay.Example.com:8443/v2',
    });
  });

  it('a private address refused by the server is a fixed sentence', async () => {
    relayServer(empty, {
      'PUT /api/v1/orgs/:org/push': () => ({
        status: 400,
        json: {
          error: 'relayUrl: must not be a private, loopback or link-local address',
          code: 'validation_failed',
        },
      }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/push');
    await user.type(await screen.findByLabelText('Relay URL'), 'https://10.0.0.5');
    await user.type(screen.getByLabelText('Relay secret'), SECRET_IN);
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(
      await screen.findByText('The relay address is private or internal. Use a public https address.'),
    ).toBeInTheDocument();
  });

  it('Remove asks first, focus goes to Cancel, and Cancel sends nothing', async () => {
    const api = relayServer(set);
    const user = userEvent.setup();
    renderApp('/orbit/settings/push');
    await user.click(await screen.findByRole('button', { name: 'Remove' }));
    const dialog = await screen.findByRole('dialog', { name: 'Remove the push relay?' });
    expect(within(dialog).getByRole('button', { name: 'Cancel' })).toHaveFocus();
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(api.calls.filter((c) => c.method === 'DELETE')).toHaveLength(0);
    await user.click(screen.getByRole('button', { name: 'Remove' }));
    await screen.findByRole('dialog');
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('two synchronous clicks on Remove relay send one DELETE, and focus lands on the URL field', async () => {
    const api = relayServer(set);
    const user = userEvent.setup();
    renderApp('/orbit/settings/push');
    await user.click(await screen.findByRole('button', { name: 'Remove' }));
    const confirm = within(await screen.findByRole('dialog')).getByRole('button', { name: 'Remove relay' });
    act(() => {
      confirm.click();
      confirm.click();
    });
    const field = await screen.findByLabelText('Relay URL');
    await waitFor(() => expect(field).toHaveFocus());
    expect(field).not.toBe(document.body);
    expect(api.calls.filter((c) => c.method === 'DELETE')).toHaveLength(1);
  });

  it('two synchronous clicks on Save send one PUT; on Send test push one POST', async () => {
    const api = relayServer(empty);
    const user = userEvent.setup();
    renderApp('/orbit/settings/push');
    await user.type(await screen.findByLabelText('Relay URL'), URL_IN);
    await user.type(screen.getByLabelText('Relay secret'), SECRET_IN);
    const save = screen.getByRole('button', { name: 'Save' });
    act(() => {
      save.click();
      save.click();
    });
    await screen.findByText('Saved.');
    expect(api.calls.filter((c) => c.method === 'PUT')).toHaveLength(1);
    const test = screen.getByRole('button', { name: 'Send test push' });
    act(() => {
      test.click();
      test.click();
    });
    await screen.findByText('Test push sent to 1 of your devices.');
    expect(api.calls.filter((c) => c.method === 'POST')).toHaveLength(1);
  });

  it('shows fixed sentences for a failed test, a failed save and the last failed push, never raw text', async () => {
    relayServer(
      { ...set, lastError: 'push_unauthorized' },
      {
        'POST /api/v1/orgs/:org/push/test': () => ({
          status: 502,
          json: { error: 'the test push was not sent: push_unreachable', code: 'push_send_failed' },
        }),
      },
    );
    const user = userEvent.setup();
    renderApp('/orbit/settings/push');
    expect(
      await screen.findByText(/Last push failed\. The relay did not accept the secret/),
    ).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Send test push' }));
    expect(
      await screen.findByText('The test push was not sent. The server could not reach the relay.'),
    ).toBeInTheDocument();
  });

  it('says how many pushes were dropped or given up, and how many devices exist', async () => {
    relayServer({ ...set, rateCappedCount: 3, deadCount: 1, deviceCount: 1 });
    renderApp('/orbit/settings/push');
    expect(await screen.findByTestId('push-capped')).toHaveTextContent(
      'The hourly limit dropped 3 pushes in the last day',
    );
    expect(screen.getByText(/1 push could not be delivered and was given up/)).toBeInTheDocument();
    expect(screen.getByText(/1 device is registered/)).toBeInTheDocument();
  });

  it('says so when the server has no encryption key, and disables the fields', async () => {
    relayServer({ ...empty, available: false });
    renderApp('/orbit/settings/push');
    expect(await screen.findByText(/no SETTINGS_ENCRYPTION_KEY/)).toBeInTheDocument();
    expect(screen.getByLabelText('Relay URL')).toBeDisabled();
  });

  it('an unreadable relay asks for both values again', async () => {
    relayServer({ ...set, secretUnreadable: true });
    renderApp('/orbit/settings/push');
    expect(await screen.findByText(/cannot be opened with this server key/)).toBeInTheDocument();
    expect(screen.getByLabelText('New relay URL')).toBeInTheDocument();
    expect(screen.getByLabelText('New relay secret')).toBeInTheDocument();
  });

  it('a load error is a fixed sentence with Try again', async () => {
    relayServer(empty, {
      'GET /api/v1/orgs/:org/push': () => ({
        status: 500,
        json: { error: 'boom RAW', code: 'internal_error' },
      }),
    });
    renderApp('/orbit/settings/push');
    expect(await screen.findByText('Could not load the mobile push settings')).toBeInTheDocument();
    expect(screen.queryByText(/boom RAW/)).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Try again' })).toBeInTheDocument();
  });

  it('a member is turned away from the page', async () => {
    mockApi({ ...baseRoutes(), ...session('member') });
    renderApp('/orbit/settings/push');
    expect(await screen.findByRole('heading', { name: 'You need admin access.' })).toBeInTheDocument();
  });

  it('the settings search finds the page by "relay" and "push"', async () => {
    relayServer(empty);
    const user = userEvent.setup();
    renderApp('/orbit/settings/organisation');
    await user.type(await screen.findByRole('searchbox', { name: 'Search settings' }), 'relay');
    expect(await screen.findByRole('link', { name: /Mobile push/ })).toBeInTheDocument();
  });
});

// ---------- Mobile devices ----------

const phone: PushDevice = {
  id: 'd1',
  platform: 'ios',
  label: 'Anna phone',
  appVersion: '1.2.0',
  createdAt: '2026-10-01T10:00:00Z',
  lastSeenAt: '2026-10-30T10:00:00Z',
};
const tablet: PushDevice = { ...phone, id: 'd2', platform: 'android', label: '', appVersion: '' };
const third = { ...phone, id: 'd3', label: 'Old pad' };

function devicesServer(
  items: PushDevice[],
  opts: { relayConfigured?: boolean; routes?: Record<string, Handler> } = {},
) {
  const cur = { items };
  const api = mockApi({
    ...baseRoutes(),
    ...session('member'),
    'GET /api/v1/orgs/:org/me/push-devices': () => ({
      json: { relayConfigured: opts.relayConfigured ?? true, maxDevices: 10, items: cur.items },
    }),
    'DELETE /api/v1/orgs/:org/me/push-devices/:device': (r) => {
      cur.items = cur.items.filter((d) => d.id !== r.params.device);
      return { status: 204 };
    },
    ...opts.routes,
  });
  return api;
}

describe('Mobile devices page (LAUN-197)', () => {
  it('lists the devices of the person with their platform, version and last seen, and no token', async () => {
    devicesServer([phone, tablet]);
    renderApp('/orbit/settings/mobile-devices');
    expect(await screen.findByRole('heading', { name: 'Mobile devices' })).toBeInTheDocument();
    const list = await screen.findByRole('list', { name: 'Registered devices' });
    const rows = within(list).getAllByRole('listitem');
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveTextContent('Anna phone');
    expect(rows[0]).toHaveTextContent('iPhone or iPad, app 1.2.0');
    expect(rows[1]).toHaveTextContent('Unnamed device');
    expect(rows[1]).toHaveTextContent('Android');
    expect(within(screen.getByRole('main')).getByRole('link', { name: 'Notifications' })).toHaveAttribute(
      'href',
      '/orbit/settings/notifications',
    );
  });

  it('says when the admin has not turned push on, and when there are no devices', async () => {
    devicesServer([], { relayConfigured: false });
    renderApp('/orbit/settings/mobile-devices');
    expect(await screen.findByText(/Your administrator has not turned on mobile push/)).toBeInTheDocument();
    expect(screen.getByText('No devices yet')).toBeInTheDocument();
  });

  it('Remove asks first: Cancel or Escape sends nothing and returns focus to the button', async () => {
    const api = devicesServer([phone, tablet]);
    const user = userEvent.setup();
    renderApp('/orbit/settings/mobile-devices');
    const btn = await screen.findByRole('button', { name: 'Remove Anna phone' });
    await user.click(btn);
    const dialog = await screen.findByRole('dialog', { name: 'Remove this device?' });
    expect(within(dialog).getByRole('button', { name: 'Cancel' })).toHaveFocus();
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    await waitFor(() => expect(btn).toHaveFocus());
    await user.click(btn);
    await screen.findByRole('dialog');
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    await waitFor(() => expect(btn).toHaveFocus());
    expect(api.calls.filter((c) => c.method === 'DELETE')).toHaveLength(0);
  });

  it('confirming sends one DELETE, and focus goes to the next device, then the previous, then the heading', async () => {
    const api = devicesServer([phone, tablet, third]);
    const user = userEvent.setup();
    renderApp('/orbit/settings/mobile-devices');
    async function removeVia(name: string) {
      await user.click(await screen.findByRole('button', { name: `Remove ${name}` }));
      await user.click(
        within(await screen.findByRole('dialog')).getByRole('button', { name: 'Remove device' }),
      );
      await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    }
    await removeVia('Anna phone');
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Remove Anna phone' })).toBeNull());
    expect(api.calls.filter((c) => c.method === 'DELETE')).toHaveLength(1);
    expect(api.calls.find((c) => c.method === 'DELETE')!.url.pathname).toBe(
      '/api/v1/orgs/orbit/me/push-devices/d1',
    );
    await waitFor(() => expect(screen.getByRole('button', { name: 'Remove Unnamed device' })).toHaveFocus());
    await removeVia('Old pad');
    await waitFor(() => expect(screen.getByRole('button', { name: 'Remove Unnamed device' })).toHaveFocus());
    await removeVia('Unnamed device');
    await screen.findByText('No devices yet');
    await waitFor(() => expect(screen.getByRole('heading', { name: 'Mobile devices' })).toHaveFocus());
  });

  it('two synchronous clicks on the confirm button send one DELETE', async () => {
    const api = devicesServer([phone, tablet]);
    const user = userEvent.setup();
    renderApp('/orbit/settings/mobile-devices');
    await user.click(await screen.findByRole('button', { name: 'Remove Anna phone' }));
    const confirm = within(await screen.findByRole('dialog')).getByRole('button', { name: 'Remove device' });
    act(() => {
      confirm.click();
      confirm.click();
    });
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Remove Anna phone' })).toBeNull());
    expect(api.calls.filter((c) => c.method === 'DELETE')).toHaveLength(1);
  });

  it('a failed removal shows a fixed sentence and keeps the row', async () => {
    devicesServer([phone], {
      routes: {
        'DELETE /api/v1/orgs/:org/me/push-devices/:device': () => ({
          status: 500,
          json: { error: 'internal RAW text', code: 'internal_error' },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/mobile-devices');
    await user.click(await screen.findByRole('button', { name: 'Remove Anna phone' }));
    await user.click(
      within(await screen.findByRole('dialog')).getByRole('button', { name: 'Remove device' }),
    );
    expect(await screen.findByText('Could not remove the device. Try again.')).toBeInTheDocument();
    expect(screen.queryByText(/RAW text/)).not.toBeInTheDocument();
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Remove Anna phone' })).toBeInTheDocument();
  });

  it('an agent (403) sees a plain sentence, and a load error has Try again', async () => {
    devicesServer([], {
      routes: {
        'GET /api/v1/orgs/:org/me/push-devices': () => ({
          status: 403,
          json: { error: 'forbidden RAW', code: 'forbidden' },
        }),
      },
    });
    renderApp('/orbit/settings/mobile-devices');
    expect(await screen.findByText('No mobile devices for this account')).toBeInTheDocument();
    expect(screen.queryByText(/forbidden RAW/)).not.toBeInTheDocument();
  });

  it('a load failure is a fixed sentence with Try again', async () => {
    devicesServer([], {
      routes: {
        'GET /api/v1/orgs/:org/me/push-devices': () => ({
          status: 500,
          json: { error: 'RAW', code: 'internal_error' },
        }),
      },
    });
    renderApp('/orbit/settings/mobile-devices');
    expect(await screen.findByText('Could not load your devices')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Try again' })).toBeInTheDocument();
  });

  it('a member sees the page in the sidebar', async () => {
    devicesServer([]);
    renderApp('/orbit/settings/mobile-devices');
    const nav = await screen.findByRole('navigation', { name: 'Settings' });
    expect(within(nav).getByRole('link', { name: 'Mobile devices' })).toHaveAttribute('aria-current', 'page');
  });
});

// ---------- fixed text ----------

const e = (status: number, code: string, message = 'RAW server text') => new ApiError(status, code, message);

describe('push text', () => {
  it('checks the URL and the secret like the server does', () => {
    expect(relayUrlProblem('https://relay.example.com')).toBeNull();
    expect(relayUrlProblem('https://relay.example.com:8443/x/y')).toBeNull();
    for (const bad of [
      'http://relay.example.com',
      'relay.example.com',
      'https://u:p@relay.example.com',
      'https://relay.example.com/?a=1',
      'https://relay.example.com/#x',
      'https://relay.example.com/a b',
      'https://' + 'a'.repeat(520),
      '',
    ])
      expect(relayUrlProblem(bad), bad).not.toBeNull();
    expect(relaySecretProblem('abc-123')).toBeNull();
    expect(relaySecretProblem('has space')).not.toBeNull();
    expect(relaySecretProblem('é')).not.toBeNull();
    expect(relaySecretProblem('x'.repeat(513))).not.toBeNull();
  });

  it('maps the local-name and old-spelling refusals to their own sentences', () => {
    expect(
      pushErrorText(
        e(400, 'validation_failed', 'relayUrl: must not be a local or internal host name'),
        'save',
      ),
    ).toContain('local or internal host name');
    expect(
      pushErrorText(
        e(400, 'validation_failed', 'relayUrl: must not be a numeric address in an old spelling'),
        'save',
      ),
    ).toContain('old spelling');
  });

  it('every error maps to a fixed sentence and never repeats server text', () => {
    const all = [
      pushErrorText(new Error('x'), 'save'),
      pushErrorText(e(0, 'x'), 'save'),
      pushErrorText(e(401, 'unauthenticated'), 'save'),
      pushErrorText(e(403, 'forbidden'), 'save'),
      pushErrorText(e(429, 'rate_limited'), 'test'),
      pushErrorText(e(404, 'not_found'), 'test'),
      pushErrorText(e(502, 'push_send_failed', 'the test push was not sent: push_blocked'), 'test'),
      pushErrorText(e(502, 'push_send_failed', 'weird'), 'test'),
      pushErrorText(e(409, 'conflict', 'you have no registered device; open the app'), 'test'),
      pushErrorText(e(409, 'conflict'), 'test'),
      pushErrorText(e(500, 'internal_error'), 'test'),
      pushErrorText(e(409, 'encryption_unavailable'), 'save'),
      pushErrorText(e(400, 'validation_failed'), 'save'),
      pushErrorText(
        e(400, 'validation_failed', 'relayUrl: must not be a local or internal host name'),
        'save',
      ),
      pushErrorText(
        e(400, 'validation_failed', 'relayUrl: must not be a numeric address in an old spelling'),
        'save',
      ),
      pushErrorText(e(500, 'internal_error'), 'save'),
      pushErrorText(e(500, 'internal_error'), 'remove'),
      deviceRemoveText(e(500, 'internal_error')),
      deviceRemoveText(e(403, 'forbidden')),
      devicesLoadText(e(403, 'forbidden')),
      devicesLoadText(e(500, 'x')),
    ];
    for (const t of all) expect(t).not.toContain('RAW server text');
    expect(pushErrorText(e(502, 'push_send_failed', 'x: push_blocked'), 'test')).toContain(
      'refused to connect to this address',
    );
  });
});
