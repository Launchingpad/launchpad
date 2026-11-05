import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { baseRoutes, mockApi, type Handler } from '../../test/mockApi';
import { renderApp, testClient } from '../../test/render';

const PASS = 'abcd-efgh-ijkl-mnop-qrst-uvwx-yzab-cdef';
const TICKET = 'ticket-secret-value-123';

function server(platform = true, over: Record<string, Handler> = {}) {
  return mockApi({
    ...baseRoutes(),
    'GET /api/v1/auth/session': () => ({
      json: {
        user: {
          id: 'u1',
          username: 'chen',
          name: 'Chen',
          email: 'chen@example.com',
          isPlatformAdmin: platform,
        },
        orgs: [{ id: 'o1', slug: 'orbit', name: 'Orbit', role: 'admin' }],
        csrfToken: 'csrf-test',
      },
    }),
    'POST /api/v1/platform/move/export': () => ({
      status: 201,
      json: {
        passphrase: PASS,
        ticket: TICKET,
        downloadPath: `/api/v1/platform/move/export/${TICKET}`,
        filename: 'launchpad-move.lpmove',
        expiresAt: '2026-11-04T10:05:00Z',
        includeAttachments: true,
      },
    }),
    ...over,
  });
}

let clicked: HTMLAnchorElement[] = [];
beforeEach(() => {
  clicked = [];
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
    clicked.push(this);
  });
});
afterEach(() => vi.restoreAllMocks());

describe('Settings, Platform, Move server', () => {
  it('refuses a person who is not a platform admin', async () => {
    const api = server(false);
    renderApp('/orbit/settings/move');
    expect(await screen.findByText('You need platform admin access.')).toBeInTheDocument();
    expect(api.calls.some((c) => c.method === 'POST')).toBe(false);
  });

  it('makes the file, shows the passphrase once, starts the download and lists the next steps', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/move');
    expect(await screen.findByLabelText('Include attachments')).toBeChecked();
    await user.click(screen.getByRole('button', { name: 'Make move file' }));
    expect(await screen.findByTestId('move-passphrase')).toHaveTextContent(PASS);
    const post = api.calls.find((c) => c.method === 'POST' && c.url.pathname.endsWith('/move/export'))!;
    expect(post.body).toEqual({ includeAttachments: true });
    expect(post.headers.get('Idempotency-Key')).toBeNull();
    expect(screen.getByText(/shown one time/)).toBeInTheDocument();
    await waitFor(() => expect(clicked).toHaveLength(1));
    expect(clicked[0]!.getAttribute('href')).toContain(`/api/v1/platform/move/export/${TICKET}`);
    expect(clicked[0]!.getAttribute('href')).not.toContain(PASS);
    expect(clicked[0]!.download).toBe('launchpad-move.lpmove');
    expect(screen.getByText(/Anything written here after the export is NOT in the file/)).toBeInTheDocument();
    expect(screen.getByText(/webhooks, MCP clients and mobile clients/)).toBeInTheDocument();
    // Done needs the confirmation, then the passphrase is gone.
    const done = screen.getByRole('button', { name: 'Done' });
    expect(done).toBeDisabled();
    await user.click(screen.getByLabelText('I saved the passphrase in a safe place.'));
    await user.click(done);
    expect(screen.queryByTestId('move-passphrase')).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain(PASS);
  });

  it('sends includeAttachments false when the box is cleared', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/move');
    await user.click(await screen.findByLabelText('Include attachments'));
    await user.click(screen.getByRole('button', { name: 'Make move file' }));
    await screen.findByTestId('move-passphrase');
    const post = api.calls.find((c) => c.method === 'POST' && c.url.pathname.endsWith('/move/export'))!;
    expect(post.body).toEqual({ includeAttachments: false });
  });

  it('shows the passphrase in a modal that Escape and the close button cannot dismiss before the box is ticked', async () => {
    server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/move');
    await user.click(await screen.findByRole('button', { name: 'Make move file' }));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByTestId('move-passphrase')).toHaveTextContent(PASS);
    await user.keyboard('{Escape}');
    expect(screen.getByTestId('move-passphrase')).toBeInTheDocument();
    expect(screen.getByText(/Tick the box when you saved the passphrase/)).toBeInTheDocument();
    await user.click(within(dialog).getByLabelText('I saved the passphrase in a safe place.'));
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByTestId('move-passphrase')).not.toBeInTheDocument());
    // The checklist stays after the dialog.
    expect(screen.getByText('Next steps')).toBeInTheDocument();
  });

  it('keeps the passphrase out of the query cache, the mutation cache and web storage, also after leaving', async () => {
    server();
    const client = testClient();
    const user = userEvent.setup();
    const view = renderApp('/orbit/settings/move', client);
    await user.click(await screen.findByRole('button', { name: 'Make move file' }));
    await screen.findByTestId('move-passphrase');
    const held = () =>
      JSON.stringify([
        client
          .getQueryCache()
          .getAll()
          .map((q) => q.state.data),
        client
          .getMutationCache()
          .getAll()
          .map((m) => [m.state.data, m.state.variables]),
        { ...localStorage },
        { ...sessionStorage },
        window.location.href,
      ]);
    expect(held()).not.toContain(PASS);
    expect(held()).not.toContain(TICKET);
    view.unmount();
    expect(document.body.textContent).not.toContain(PASS);
    expect(held()).not.toContain(PASS);
  });

  it('sends one export request for a double click', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/move');
    const btn = await screen.findByRole('button', { name: 'Make move file' });
    await user.dblClick(btn);
    await screen.findByTestId('move-passphrase');
    expect(
      api.calls.filter((c) => c.method === 'POST' && c.url.pathname.endsWith('/move/export')),
    ).toHaveLength(1);
  });

  it.each([
    '/api/v1/platform/move/export/../settings',
    '/api/v1/platform/move/export/a/b',
    '/api/v1/platform/move/export/t?x=1',
    '/api/v1/platform/move/export/t#f',
    '/api/v1/platform/move/export/',
  ])('refuses the download path %s', async (path) => {
    server(true, {
      'POST /api/v1/platform/move/export': () => ({
        status: 201,
        json: {
          passphrase: PASS,
          ticket: 't',
          downloadPath: path,
          filename: 'f',
          expiresAt: '',
          includeAttachments: true,
        },
      }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/move');
    await user.click(await screen.findByRole('button', { name: 'Make move file' }));
    expect(await screen.findByText(/The download did not start/)).toBeInTheDocument();
    expect(clicked).toHaveLength(0);
  });

  it('shows a fixed text for a blocked export and starts no download', async () => {
    server(true, {
      'POST /api/v1/platform/move/export': () => ({
        status: 409,
        json: { error: 'SERVER-TEXT-MARKER', code: 'move_blocked' },
      }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/move');
    await user.click(await screen.findByRole('button', { name: 'Make move file' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(/clear "Include attachments"/);
    expect(document.body.textContent).not.toContain('SERVER-TEXT-MARKER');
    expect(clicked).toHaveLength(0);
  });

  it('refuses a download path outside the export route', async () => {
    server(true, {
      'POST /api/v1/platform/move/export': () => ({
        status: 201,
        json: {
          passphrase: PASS,
          ticket: 't',
          downloadPath: 'https://evil.example/x',
          filename: 'f',
          expiresAt: '',
          includeAttachments: true,
        },
      }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/move');
    await user.click(await screen.findByRole('button', { name: 'Make move file' }));
    expect(await screen.findByText(/The download did not start/)).toBeInTheDocument();
    expect(clicked).toHaveLength(0);
  });
});
