import { act, fireEvent, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { baseRoutes, mockApi } from '../../test/mockApi';
import { currentLocation, renderApp } from '../../test/render';
import { currentCsrf, setMemoryCsrf } from '../../api/http';
import { FakeXhr } from '../../test/fakeXhr';
import { forgetSetupToken } from './SetupPage';

const TOKEN = 'AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-AbCde';
const SECRET = 'odt_restored-admin-token-secret';
const PASS = 'abcd efgh ijkl mnop qrst uvwx yzab cdef';
const unauth = { status: 401, json: { error: 'unauthenticated', code: 'unauthenticated' } };
const OK = {
  user: { id: 'u1', username: 'chen', name: 'Chen', email: 'chen@example.com', isPlatformAdmin: true },
  orgs: [{ id: 'o1', slug: 'orbit', name: 'Orbit', role: 'admin' }],
  csrfToken: 'csrf-1',
  token: { token: { id: 't1' }, secret: SECRET },
  summary: {
    migrationVersion: 1,
    appVersion: 'x',
    exportedAt: '',
    tables: 12,
    rows: 345,
    objects: 2,
    attachmentsIncluded: true,
  },
};

let view: ReturnType<typeof renderApp>;
async function open() {
  mockApi({
    ...baseRoutes(),
    'GET /api/v1/auth/session': () => unauth,
    'GET /api/v1/setup': () => ({ json: { setupRequired: true } }),
  });
  FakeXhr.install();
  window.history.replaceState(null, '', `/setup#${TOKEN}`);
  view = renderApp('/setup');
  const user = userEvent.setup();
  await user.click(await screen.findByRole('button', { name: 'Restore from a move file' }));
  return user;
}

async function fillAndSend(user: ReturnType<typeof userEvent.setup>, pass = PASS) {
  const file = new File(['bundle-bytes'], 'x.lpmove', { type: 'application/octet-stream' });
  await user.upload(screen.getByLabelText('Move file'), file);
  await user.type(screen.getByLabelText('Passphrase'), pass);
  await user.click(screen.getByRole('button', { name: 'Restore' }));
  return file;
}

beforeEach(() => forgetSetupToken());
afterEach(() => {
  forgetSetupToken();
  window.history.replaceState(null, '', '/');
});

describe('setup: restore from a move file', () => {
  it('streams the File with the token and passphrase in headers only, shows progress, then the token once', async () => {
    const user = await open();
    expect(window.location.href).not.toContain(TOKEN);
    const file = await fillAndSend(user);
    const x = FakeXhr.last();
    expect(x.method).toBe('POST');
    expect(x.url).toContain('/api/v1/setup/restore');
    expect(x.url).not.toContain(TOKEN);
    expect(x.url).not.toContain('abcd');
    expect(x.headers['X-Launchpad-Setup-Token']).toBe(TOKEN);
    expect(x.headers['X-Launchpad-Passphrase']).toBe(PASS);
    expect(x.body).toBe(file); // the File itself, not read into memory
    act(() => x.progress(50, 100));
    expect(await screen.findByText('Uploading: 50%')).toBeInTheDocument();
    expect(screen.getByRole('progressbar', { name: 'Upload progress' })).toHaveValue(50);
    act(() => x.progress(100, 100));
    expect(await screen.findByText(/The server is loading the data/)).toBeInTheDocument();
    act(() => x.respond(201, OK));
    expect(await screen.findByTestId('first-token')).toHaveTextContent(SECRET);
    expect(screen.getByRole('heading', { name: 'Restore is done' })).toBeInTheDocument();
    expect(screen.getByText(/345 rows in 12 tables/)).toBeInTheDocument();
    expect(screen.queryByLabelText('Passphrase')).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain(PASS);
  });

  it('needs a file and a passphrase before it sends', async () => {
    const user = await open();
    await user.click(screen.getByRole('button', { name: 'Restore' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Choose the move file and enter the passphrase.',
    );
    expect(FakeXhr.all).toHaveLength(0);
  });

  it.each([
    [400, 'bad_passphrase', /passphrase is wrong/],
    [400, 'bundle_invalid', /damaged/],
    [409, 'bundle_version_mismatch', /same release/],
    [409, 'move_blocked', /cannot load the file/],
  ])('shows a fixed text for %i %s and keeps the form', async (status, code, text) => {
    const user = await open();
    await fillAndSend(user);
    act(() => FakeXhr.last().respond(status, { error: 'SERVER-TEXT-MARKER', code }));
    expect(await screen.findByRole('alert')).toHaveTextContent(text);
    expect(document.body.textContent).not.toContain('SERVER-TEXT-MARKER');
    expect(screen.getByLabelText('Passphrase')).toBeEnabled();
    expect(screen.queryByTestId('first-token')).not.toBeInTheDocument();
  });

  it('shows the page for a link that is wrong, and for an install that has a user', async () => {
    const user = await open();
    await fillAndSend(user);
    act(() => FakeXhr.last().respond(401, { error: 'x', code: 'invalid_setup_token' }));
    expect(await screen.findByRole('heading', { name: 'This setup link does not work' })).toBeInTheDocument();
  });

  it('shows setup is done when the install has a user', async () => {
    const user = await open();
    await fillAndSend(user);
    act(() => FakeXhr.last().respond(409, { error: 'x', code: 'setup_complete' }));
    expect(await screen.findByRole('heading', { name: 'Setup is already done' })).toBeInTheDocument();
  });

  it('cancels a running upload', async () => {
    const user = await open();
    await fillAndSend(user);
    act(() => FakeXhr.last().progress(10, 100));
    await user.click(await screen.findByRole('button', { name: 'Cancel' }));
    expect(FakeXhr.last().aborted).toBe(true);
    await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('The upload was cancelled'));
  });

  it('can go back to the new install form', async () => {
    const user = await open();
    await user.click(screen.getByRole('button', { name: 'New install' }));
    expect(await screen.findByLabelText('Username')).toBeInTheDocument();
  });

  it('keeps the toggle and the token on the page while a restore runs', async () => {
    const user = await open();
    await fillAndSend(user);
    act(() => FakeXhr.last().progress(100, 100));
    expect(screen.getByRole('button', { name: 'New install' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Restore from a move file' })).toBeDisabled();
    // A new link in the address bar does not replace the held link or the form during the upload.
    window.history.replaceState(null, '', '/setup#' + 'Z'.repeat(43));
    act(() => {
      window.dispatchEvent(new HashChangeEvent('hashchange'));
    });
    expect(screen.getByLabelText('Passphrase')).toBeInTheDocument();
    act(() => FakeXhr.last().respond(201, OK));
    expect(await screen.findByTestId('first-token')).toHaveTextContent(SECRET);
  });

  it('aborts on unmount only while the file is still going out; after the last byte it waits and starts no session', async () => {
    setMemoryCsrf(undefined);
    let user = await open();
    await fillAndSend(user);
    act(() => FakeXhr.last().progress(40, 100));
    view.unmount();
    expect(FakeXhr.last().aborted).toBe(true);

    user = await open();
    await fillAndSend(user);
    const x = FakeXhr.last();
    act(() => x.progress(100, 100));
    view.unmount();
    expect(x.aborted).toBe(false);
    act(() => x.respond(201, OK));
    // Nobody is on screen to show the token, so no session is started in this tab.
    expect(currentCsrf()).toBeUndefined();
  });

  it('says the result is unknown (not "nothing was loaded") when the answer is lost after the upload', async () => {
    let user = await open();
    await fillAndSend(user);
    act(() => {
      FakeXhr.last().progress(100, 100);
      FakeXhr.last().respond(502, { error: 'bad gateway', code: 'error' });
    });
    expect(await screen.findByRole('alert')).toHaveTextContent(/The result is unknown/);
    expect(screen.getByRole('alert')).not.toHaveTextContent(/Nothing was loaded/);

    user = await open();
    await fillAndSend(user);
    act(() => {
      FakeXhr.last().progress(100, 100);
      FakeXhr.last().onerror?.();
    });
    expect(await screen.findByRole('alert')).toHaveTextContent(/The result is unknown/);
  });

  it('a 500 before the whole file is sent keeps the plain restore-failed text', async () => {
    const user = await open();
    await fillAndSend(user);
    act(() => FakeXhr.last().respond(500, { error: 'x', code: 'error' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(/The restore failed/);
  });

  it('asks the browser to confirm leaving while a restore runs, and not otherwise', async () => {
    const user = await open();
    const idle = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(idle);
    expect(idle.defaultPrevented).toBe(false);
    await fillAndSend(user);
    const busy = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(busy);
    expect(busy.defaultPrevented).toBe(true);
  });

  it('sends one request for a double submit', async () => {
    const user = await open();
    await fillAndSend(user);
    fireEvent.submit(screen.getByLabelText('Passphrase').closest('form')!);
    expect(FakeXhr.all).toHaveLength(1);
  });

  it('shows no counts when the server sends no summary, and goes to the org (not the checklist) afterwards', async () => {
    const user = await open();
    await fillAndSend(user);
    const { summary: _s, ...noSummary } = OK;
    void _s;
    act(() => FakeXhr.last().respond(201, noSummary));
    expect(await screen.findByText(/The move file is loaded\./)).toBeInTheDocument();
    expect(screen.queryByText(/rows in/)).not.toBeInTheDocument();
    await user.click(screen.getByLabelText('I saved the token in a safe place.'));
    await user.click(screen.getByRole('button', { name: 'Continue to Launchpad' }));
    // `/orbit` itself redirects to the issue list; the first-run checklist is the path a restore must NOT use.
    await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/issues'));
    expect(currentLocation.pathname).not.toContain('/settings/setup');
  });

  it('uses new-password autocomplete on the passphrase field', async () => {
    await open();
    expect(screen.getByLabelText('Passphrase')).toHaveAttribute('autocomplete', 'new-password');
  });
});
