import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { baseRoutes, mockApi, type Handler, type MockRequest } from '../../test/mockApi';
import { renderApp, testClient } from '../../test/render';
import { badFieldLabels, saveErrorText, testErrorText } from './PlatformPage';
import { ApiError } from '../../api/errors';

// the Platform page (docs/internal/decisions-T9-mail-settings.md).

const TOKEN = 'pm-planted-token-value-9f3a';
const PASSWORD = 'smtp-planted-password-7c1d';
const MARKER = 'RAW-SERVER-TEXT-MARKER';

function settings(over: Record<string, unknown> = {}) {
  return {
    mailProvider: 'env',
    postmarkFrom: '',
    postmarkMessageStream: '',
    postmarkTokenSet: false,
    smtpHost: '',
    smtpPort: 587,
    smtpSecurity: 'starttls',
    smtpUsername: '',
    smtpPasswordSet: false,
    smtpFromAddress: '',
    smtpFromName: '',
    signupMode: 'invite',
    activeMail: { transport: 'dev-log', source: 'environment' },
    loginAvailable: true,
    encryptionKeySet: true,
    secretsUnreadable: false,
    updatedAt: '2026-10-25T10:00:00.000000Z',
    updatedBy: null,
    ...over,
  };
}

function server(
  opts: { current?: Record<string, unknown>; platform?: boolean; routes?: Record<string, Handler> } = {},
) {
  let stored = settings(opts.current);
  let n = 0;
  const api = mockApi({
    ...baseRoutes(),
    'GET /api/v1/auth/session': () => ({
      json: {
        user: {
          id: 'u1',
          username: 'chen',
          name: 'Chen',
          email: 'chen@example.com',
          isPlatformAdmin: opts.platform ?? true,
        },
        orgs: [{ id: 'o1', slug: 'orbit', name: 'Orbit', role: 'admin' }],
        csrfToken: 'csrf-test',
      },
    }),
    'GET /api/v1/platform/admins': () => ({
      json: {
        items: [{ id: 'u1', username: 'chen', name: 'Chen', email: 'chen@example.com', deactivated: false }],
        nextCursor: null,
      },
    }),
    'GET /api/v1/platform/settings': () => ({ json: stored }),
    'PUT /api/v1/platform/settings': (r: MockRequest) => {
      const b = r.body as Record<string, unknown>;
      n += 1;
      stored = settings({
        ...b,
        postmarkToken: undefined,
        smtpPassword: undefined,
        postmarkTokenSet: b.postmarkToken === undefined ? stored.postmarkTokenSet : b.postmarkToken !== '',
        smtpPasswordSet: b.smtpPassword === undefined ? stored.smtpPasswordSet : b.smtpPassword !== '',
        updatedAt: `2026-10-25T10:00:0${n}.000000Z`,
      });
      return { json: stored };
    },
    ...opts.routes,
  });
  return api;
}

const puts = (api: { calls: MockRequest[] }) => api.calls.filter((c) => c.method === 'PUT');
const posts = (api: { calls: MockRequest[] }) => api.calls.filter((c) => c.method === 'POST');
const webStorage = () => JSON.stringify({ ...localStorage }) + JSON.stringify({ ...sessionStorage });

async function open(user = userEvent.setup()) {
  renderApp('/orbit/settings/platform');
  await screen.findByRole('heading', { name: 'Platform' });
  await screen.findByRole('form', { name: 'Platform settings' });
  return user;
}
const pick = (user: ReturnType<typeof userEvent.setup>, value: string) =>
  user.selectOptions(screen.getByLabelText('Mail provider'), value);

describe('Platform page: states', () => {
  it('shows the loaded settings with the env provider and its text', async () => {
    server();
    await open();
    expect(screen.getByLabelText('Mail provider')).toHaveValue('env');
    expect(screen.getByText(/Use the mail setup of the server/)).toBeInTheDocument();
    expect(screen.getByTestId('login-status')).toHaveTextContent('Login is on.');
    expect(screen.getByText(/Login email now goes through the server log/)).toBeInTheDocument();
    expect(screen.getByRole('radio', { name: /Invite only/ })).toBeChecked();
    expect(screen.getByRole('button', { name: 'Save settings' })).toBeDisabled();
  });

  it('shows a spinner, then a fixed load error with a retry, and never the server text', async () => {
    let fail = true;
    server({
      routes: {
        'GET /api/v1/platform/settings': () =>
          fail ? { status: 500, json: { error: MARKER, code: 'internal' } } : { json: settings() },
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/platform');
    expect(await screen.findByText('Loading the platform settings')).toBeInTheDocument();
    const err = await screen.findByTestId('load-error');
    expect(err).toHaveTextContent('The server had a problem. Try again.');
    expect(document.body.textContent).not.toContain(MARKER);
    fail = false;
    await user.click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByRole('form', { name: 'Platform settings' })).toBeInTheDocument();
  });

  it('a 403 on load says to sign in again after a promotion, with no server text', async () => {
    server({
      routes: {
        'GET /api/v1/platform/settings': () => ({ status: 403, json: { error: MARKER, code: 'forbidden' } }),
      },
    });
    renderApp('/orbit/settings/platform');
    const err = await screen.findByTestId('load-error');
    expect(err).toHaveTextContent('sign out and sign in again');
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('refuses a person who is not a platform admin and calls no platform route', async () => {
    const api = server({ platform: false });
    renderApp('/orbit/settings/platform');
    expect(
      await screen.findByRole('heading', { name: 'You need platform admin access.' }),
    ).toBeInTheDocument();
    expect(api.calls.some((c) => c.url.pathname.startsWith('/api/v1/platform'))).toBe(false);
  });
});

describe('Platform page: each provider shows only its fields', () => {
  it('env and none show no provider fields', async () => {
    server();
    const user = await open();
    for (const p of ['env', 'none']) {
      await pick(user, p);
      expect(screen.queryByLabelText('SMTP host')).not.toBeInTheDocument();
      expect(screen.queryByLabelText('Postmark server token')).not.toBeInTheDocument();
    }
    expect(screen.getByText(/Login is off\. Nobody can get a sign-in code\./)).toBeInTheDocument();
  });

  it('postmark shows the token, sender and stream, and no SMTP field', async () => {
    server();
    const user = await open();
    await pick(user, 'postmark');
    expect(screen.getByLabelText('Postmark server token')).toBeInTheDocument();
    expect(screen.getByLabelText('Sender (From)')).toBeInTheDocument();
    expect(screen.getByLabelText('Message stream')).toBeInTheDocument();
    expect(screen.queryByLabelText('SMTP host')).not.toBeInTheDocument();
    expect(screen.queryByLabelText('SMTP password')).not.toBeInTheDocument();
  });

  it('smtp shows host, port, security, user, password and sender, and no Postmark field', async () => {
    server();
    const user = await open();
    await pick(user, 'smtp');
    for (const l of [
      'SMTP host',
      'SMTP port',
      'SMTP security',
      'SMTP username',
      'SMTP password',
      'Sender address',
      'Sender name',
    ])
      expect(screen.getByLabelText(l)).toBeInTheDocument();
    expect(screen.queryByLabelText('Postmark server token')).not.toBeInTheDocument();
    const sec = within(screen.getByLabelText('SMTP security'));
    expect(sec.getAllByRole('option').map((o) => (o as HTMLOptionElement).value)).toEqual([
      'starttls',
      'tls',
      'none',
    ]);
  });
});

describe('Platform page: save payload', () => {
  it('sends the whole settings, and no secret key when none was typed or cleared', async () => {
    const api = server({
      current: { mailProvider: 'smtp', smtpHost: 'old.example.com', smtpPasswordSet: true },
    });
    const user = await open();
    const host = screen.getByLabelText('SMTP host');
    await user.clear(host);
    await user.type(host, ' mail.example.com ');
    await user.clear(screen.getByLabelText('SMTP port'));
    await user.type(screen.getByLabelText('SMTP username'), 'login');
    await user.type(screen.getByLabelText('Sender address'), 'login@example.com');
    await user.click(screen.getByRole('radio', { name: /Approval/ }));
    await user.click(screen.getByRole('button', { name: 'Save settings' }));
    await screen.findByText('Settings saved.');
    expect(puts(api)).toHaveLength(1);
    expect(puts(api)[0]!.body).toStrictEqual({
      mailProvider: 'smtp',
      postmarkFrom: '',
      postmarkMessageStream: '',
      smtpHost: 'mail.example.com',
      smtpPort: 0, // an empty port means the default
      smtpSecurity: 'starttls',
      smtpUsername: 'login',
      smtpFromAddress: 'login@example.com',
      smtpFromName: '',
      signupMode: 'approval',
    });
    expect(puts(api)[0]!.headers.get('Idempotency-Key')).toBeNull();
    expect(puts(api)[0]!.headers.get('X-CSRF-Token')).toBe('csrf-test');
  });

  it('sends the typed secret as the value, and "" for a clear', async () => {
    const api = server({
      current: {
        mailProvider: 'smtp',
        smtpHost: 'h.example.com',
        smtpFromAddress: 'a@example.com',
        smtpPasswordSet: true,
      },
    });
    const user = await open();
    await user.type(screen.getByLabelText('SMTP password'), PASSWORD);
    await user.click(screen.getByRole('button', { name: 'Save settings' }));
    await screen.findByText('Settings saved.');
    expect((puts(api)[0]!.body as Record<string, unknown>).smtpPassword).toBe(PASSWORD);
    expect(puts(api)[0]!.body).not.toHaveProperty('postmarkToken');

    await user.click(screen.getByRole('button', { name: 'Clear' }));
    expect(screen.getByTestId('status-SMTP password')).toHaveTextContent('It will be cleared when you save');
    await user.click(screen.getByRole('button', { name: 'Save settings' }));
    await waitFor(() => expect(puts(api)).toHaveLength(2));
    expect((puts(api)[1]!.body as Record<string, unknown>).smtpPassword).toBe('');
    await waitFor(() => expect(screen.getByTestId('status-SMTP password')).toHaveTextContent('Not set'));
  });

  it('"Keep it" undoes a clear, and a provider change drops a typed secret', async () => {
    const api = server({
      current: { mailProvider: 'postmark', postmarkTokenSet: true, postmarkFrom: 'a@example.com' },
    });
    const user = await open();
    await user.click(screen.getByRole('button', { name: 'Clear' }));
    await user.click(screen.getByRole('button', { name: 'Keep it' }));
    expect(screen.getByTestId('status-Postmark server token')).toHaveTextContent(/^Set$/);
    expect(screen.getByRole('button', { name: 'Save settings' })).toBeDisabled();
    await user.type(screen.getByLabelText('Postmark server token'), TOKEN);
    await pick(user, 'none');
    await user.click(screen.getByRole('button', { name: 'Save settings' }));
    await screen.findByText('Settings saved.');
    expect(puts(api)[0]!.body).not.toHaveProperty('postmarkToken');
    expect(JSON.stringify(puts(api)[0]!.body)).not.toContain(TOKEN);
  });

  it('rejects a bad port in the browser and sends nothing', async () => {
    const api = server({ current: { mailProvider: 'smtp' } });
    const user = await open();
    await user.clear(screen.getByLabelText('SMTP port'));
    await user.type(screen.getByLabelText('SMTP port'), '70000');
    await user.click(screen.getByRole('button', { name: 'Save settings' }));
    expect(await screen.findByText(/Enter a whole number from 0 to 65535/)).toBeInTheDocument();
    expect(screen.getByLabelText('SMTP port')).toHaveAttribute('aria-invalid', 'true');
    expect(puts(api)).toHaveLength(0);
  });
});

describe('Platform page: a bad port does not block another provider', () => {
  it('saves after a bad SMTP port when the provider changes, and sends the stored port', async () => {
    const api = server({ current: { mailProvider: 'smtp', smtpPort: 465, smtpHost: 'h.example.com' } });
    const user = await open();
    await user.clear(screen.getByLabelText('SMTP port'));
    await user.type(screen.getByLabelText('SMTP port'), '99999');
    await pick(user, 'none');
    await user.click(screen.getByRole('button', { name: 'Save settings' }));
    await screen.findByText('Settings saved.');
    expect(puts(api)).toHaveLength(1);
    expect((puts(api)[0]!.body as Record<string, unknown>).smtpPort).toBe(465);
    // Back to SMTP: the box shows the stored port, not the bad one.
    await pick(user, 'smtp');
    expect(screen.getByLabelText('SMTP port')).toHaveValue('465');
  });
});

describe('Platform page: secrets are write-only', () => {
  it('shows only "Set" or "Not set", with the box empty', async () => {
    server({ current: { mailProvider: 'postmark', postmarkTokenSet: true } });
    await open();
    expect(screen.getByTestId('status-Postmark server token')).toHaveTextContent(/^Set$/);
    expect(screen.getByLabelText('Postmark server token')).toHaveValue('');
    expect(screen.getByLabelText('Postmark server token')).toHaveAttribute('type', 'password');
  });

  it('keeps no secret in the query cache, the mutation cache, web storage or the page after a save', async () => {
    const api = server({
      current: { mailProvider: 'smtp', smtpHost: 'h.example.com', smtpFromAddress: 'a@example.com' },
    });
    const client = testClient();
    const user = userEvent.setup();
    const { container } = renderApp('/orbit/settings/platform', client);
    await screen.findByRole('form', { name: 'Platform settings' });
    await user.type(screen.getByLabelText('SMTP username'), 'login');
    await user.type(screen.getByLabelText('SMTP password'), PASSWORD);
    await user.click(screen.getByRole('button', { name: 'Save settings' }));
    await screen.findByText('Settings saved.');
    // The planted secret did travel in the request, so the checks below are not vacuous.
    expect((puts(api)[0]!.body as Record<string, unknown>).smtpPassword).toBe(PASSWORD);
    const queries = JSON.stringify(
      client
        .getQueryCache()
        .getAll()
        .map((q) => [q.queryKey, q.state.data, q.state.error]),
    );
    expect(queries).not.toContain(PASSWORD);
    expect(queries).toContain('smtpPasswordSet');
    expect(client.getMutationCache().getAll()).toHaveLength(0);
    expect(webStorage()).not.toContain(PASSWORD);
    expect(window.location.href).not.toContain(PASSWORD);
    expect(container.innerHTML).not.toContain(PASSWORD);
    expect((screen.getByLabelText('SMTP password') as HTMLInputElement).value).toBe('');
    expect(screen.getByTestId('status-SMTP password')).toHaveTextContent(/^Set$/);
  });

  it('shows no secret on screen after a failed save either', async () => {
    server({
      current: { mailProvider: 'postmark', postmarkFrom: 'a@example.com' },
      routes: {
        'PUT /api/v1/platform/settings': (r) => ({
          status: 400,
          json: {
            error: `postmarkToken ${(r.body as { postmarkToken: string }).postmarkToken} ${MARKER}`,
            code: 'validation_failed',
          },
        }),
      },
    });
    const user = await open();
    await user.type(screen.getByLabelText('Postmark server token'), TOKEN);
    await user.click(screen.getByRole('button', { name: 'Save settings' }));
    const alert = await screen.findByText(/The server did not accept the settings/);
    expect(alert).toHaveTextContent('Check: Postmark token.');
    expect(document.body.innerHTML).not.toContain(MARKER);
    // The masked box keeps what the person typed, so a retry is easy. No text node shows it.
    expect(document.body.textContent).not.toContain(TOKEN);
    expect(screen.getByLabelText('Postmark server token')).toHaveAttribute('type', 'password');
  });

  it('disables the secret fields when the server has no encryption key, and says why', async () => {
    server({ current: { mailProvider: 'postmark', encryptionKeySet: false } });
    await open();
    expect(screen.getByLabelText('Postmark server token')).toBeDisabled();
    expect(screen.getByText(/no SETTINGS_ENCRYPTION_KEY/)).toBeInTheDocument();
  });

  it('warns when the stored secrets cannot be opened', async () => {
    server({ current: { mailProvider: 'postmark', secretsUnreadable: true } });
    await open();
    expect(screen.getByText(/cannot open the stored secrets/)).toBeInTheDocument();
  });
});

describe('Platform page: login availability', () => {
  it('warns, and says how to fix it, when there is no transport', async () => {
    server({
      current: {
        mailProvider: 'none',
        loginAvailable: false,
        activeMail: { transport: 'none', source: 'settings' },
      },
    });
    await open();
    const w = screen.getByTestId('login-warning');
    expect(w).toHaveAttribute('role', 'alert');
    expect(w).toHaveTextContent('Login is off. Nobody can get a sign-in code.');
    expect(w).toHaveTextContent('Choose Postmark or SMTP');
    expect(screen.queryByTestId('login-status')).not.toBeInTheDocument();
  });

  it('names the pepper when mail works but login is still off', async () => {
    server({
      current: {
        mailProvider: 'smtp',
        loginAvailable: false,
        activeMail: { transport: 'smtp', source: 'settings' },
      },
    });
    await open();
    expect(screen.getByTestId('login-warning')).toHaveTextContent('AUTH_OTP_PEPPER');
  });
});

describe('Platform page: save errors map to fixed text', () => {
  it.each([
    [409, 'SETTINGS_ENCRYPTION_KEY'],
    [403, 'You cannot change the platform settings.'],
    [429, 'Too many requests.'],
    [500, 'Could not save the settings. Try again.'],
    [400, 'Check the fields.'],
  ])('status %i', async (status, text) => {
    server({
      current: { mailProvider: 'none' },
      routes: { 'PUT /api/v1/platform/settings': () => ({ status, json: { error: MARKER, code: 'x' } }) },
    });
    const user = await open();
    await user.click(screen.getByRole('radio', { name: /Closed/ }));
    await user.click(screen.getByRole('button', { name: 'Save settings' }));
    const alert = await screen.findByRole('alert', {}, { timeout: 3000 }).catch(() => null);
    expect(alert).not.toBeNull();
    expect(
      await screen.findByText(new RegExp(text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'))),
    ).toBeInTheDocument();
    expect(document.body.innerHTML).not.toContain(MARKER);
  });

  it('maps field names in a 400 to fixed labels', () => {
    expect(badFieldLabels('smtpHost: bad; smtpPort out of range')).toEqual(['SMTP host', 'SMTP port']);
    expect(saveErrorText(new ApiError(400, 'validation_failed', 'smtpFromAddress is bad'))).toBe(
      'The server did not accept the settings. Check: SMTP sender address.',
    );
    expect(saveErrorText(new ApiError(0, 'network_error', 'x'))).toMatch(/Could not reach the server/);
  });
});

describe('Platform page: send test email', () => {
  it('is disabled while the form has unsaved changes', async () => {
    server();
    const user = await open();
    expect(screen.getByRole('button', { name: 'Send test email' })).toBeEnabled();
    await pick(user, 'none');
    expect(screen.getByRole('button', { name: 'Send test email' })).toBeDisabled();
  });

  it('shows the address on success and sends no body', async () => {
    const api = server({
      routes: {
        'POST /api/v1/platform/mail/test': () => ({
          json: { ok: true, to: 'chen@example.com', activeMail: { transport: 'smtp', source: 'settings' } },
        }),
      },
    });
    const user = await open();
    await user.click(screen.getByRole('button', { name: 'Send test email' }));
    expect(
      await screen.findByText('Test email sent to chen@example.com. Check your inbox.'),
    ).toBeInTheDocument();
    expect(posts(api)).toHaveLength(1);
    expect(posts(api)[0]!.url.pathname).toBe('/api/v1/platform/mail/test');
    expect(posts(api)[0]!.body).toBeUndefined();
  });

  it.each([
    [503, 'Login email is off. Choose Postmark or SMTP, save, and try again.'],
    [
      502,
      'The mail server did not accept the test email. Check the host, port, security and sign-in details.',
    ],
    [400, 'Your account has no email address, so the server cannot send a test email.'],
    [429, 'You sent many test emails. Wait a minute and try again.'],
    [403, 'You cannot send a test email.'],
    [500, 'The test email failed. Try again.'],
  ])('maps status %i to fixed text and never shows the server text', async (status, text) => {
    server({
      routes: {
        'POST /api/v1/platform/mail/test': () => ({
          status,
          json: {
            error: `${MARKER} ${PASSWORD} refused the login (reply code 535)`,
            code: 'mail_send_failed',
          },
        }),
      },
    });
    const user = await open();
    await user.click(screen.getByRole('button', { name: 'Send test email' }));
    expect(await screen.findByText(text)).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
    expect(document.body.textContent).not.toContain(PASSWORD);
    expect(document.body.textContent).not.toContain('535');
  });

  it('has fixed text for a network failure', () => {
    expect(testErrorText(new ApiError(0, 'network_error', 'x'))).toMatch(/Could not reach the server/);
  });
});
