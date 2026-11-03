import { act, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { pollConfig } from '../../../api/backups';
import { RunBox } from './StepList';
import type { BackupRun } from '../../../api/types';
import { renderApp } from '../../../test/render';
import { ApiError } from '../../../api/errors';
import { backupErrorText, changeText, codeText, safeCode } from './backupText';
import { backupServer, callsTo, destination, FP, MARKER, recipient, run, status } from './fixtures';

// LAUN-78: Settings, Platform, Backups. The rules are the server's (decisions-T16-backups.md #44, build notes of
// tickets 2 and 8); the server stays the judge. These tests keep the page's own promises: platform admin only, fixed
// sentences only, secrets write-only and short lived, the probe and confirm flow, the key gate.

const P = '/api/v1/platform/backups';
const URL = '/orbit/settings/backups';
const err = (status: number, code: string, error = MARKER) => ({ status, json: { error, code } });

async function open(opts: Parameters<typeof backupServer>[0] = {}) {
  const s = backupServer(opts);
  const view = renderApp(URL);
  await screen.findByRole('heading', { name: 'Backups' });
  await screen.findByRole('list', { name: 'Destinations' });
  return { ...s, ...view, user: userEvent.setup() };
}

describe('gating', () => {
  it('refuses a person who is not a platform admin, and calls nothing', async () => {
    const s = backupServer({ platform: false });
    renderApp(URL);
    expect(
      await screen.findByRole('heading', { name: 'You need platform admin access.' }),
    ).toBeInTheDocument();
    expect(s.api.calls.filter((c) => c.url.pathname.includes('/platform/backups'))).toHaveLength(0);
  });

  it('shows the Backups link in the settings nav to a platform admin only', async () => {
    backupServer({ platform: true });
    const { unmount } = renderApp(URL);
    const nav = await screen.findByRole('navigation', { name: 'Settings' });
    expect(within(nav).getByRole('link', { name: 'Backups' })).toHaveAttribute(
      'href',
      '/orbit/settings/backups',
    );
    unmount();
    backupServer({ platform: false });
    renderApp('/orbit/settings/notifications');
    const nav2 = await screen.findByRole('navigation', { name: 'Settings' });
    expect(within(nav2).queryByRole('link', { name: 'Backups' })).not.toBeInTheDocument();
  });
});

describe('banners and status', () => {
  it('shows a fixed sentence per banner and never the server text of a failed run', async () => {
    await open({
      state: {
        status: status({
          banners: [
            'encryption_key_missing',
            'key_mismatch',
            'signing_key_missing',
            'signing_key_unreadable',
            'no_active_recipient',
            'no_recent_dump',
            'backup_stale',
            'backup_run_failed',
            'mail_off',
            'files_not_backed_up',
          ],
          mailReady: false,
          staleDestinations: [
            {
              id: 'dest1',
              name: 'Offsite S3',
              lastSuccessAt: null,
              since: '2026-10-20T00:00:00Z',
              thresholdHours: 48,
              lastAlertAt: null,
            },
          ],
          failedDestinations: [
            {
              id: 'dest1',
              name: 'Offsite S3',
              runId: 'r9',
              finishedAt: '2026-10-28T04:00:00Z',
              errorCode: 'auth_failed',
              errorText: MARKER,
            },
          ],
          destinationsUnreadable: 2,
          filesStored: true,
          filesNotBackedUp: [{ id: 'dest1', name: 'Offsite S3', lastBackupAt: null }],
        }),
      },
    });
    const region = screen.getByRole('region', { name: 'Backup warnings' });
    expect(region.querySelectorAll('[data-banner]')).toHaveLength(10);
    expect(region).toHaveTextContent('no BACKUP_ENCRYPTION_KEY');
    expect(region).toHaveTextContent('not the key that sealed the stored secrets');
    expect(region).toHaveTextContent('no good backup yet');
    expect(region).toHaveTextContent('no files backup yet');
    expect(region).toHaveTextContent('The sign-in details were refused');
    expect(region).toHaveTextContent('2 destinations have secrets that this server cannot open');
    expect(within(region).getByRole('link', { name: 'Open the Platform page' })).toHaveAttribute(
      'href',
      '/orbit/settings/platform',
    );
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('shows no banner area when there is no banner, and the newest dump', async () => {
    await open();
    expect(screen.queryByRole('region', { name: 'Backup warnings' })).not.toBeInTheDocument();
    expect(screen.getByTestId('newest-dump')).toHaveTextContent('4.8 MiB');
  });

  it('shows a fixed sentence when the status cannot load', async () => {
    backupServer({ routes: { [`GET ${P}/status`]: () => err(500, 'internal') } });
    renderApp(URL);
    expect(await screen.findByTestId('load-error')).toHaveTextContent('The server had a problem');
    expect(document.body.textContent).not.toContain(MARKER);
  });
});

describe('the key gate', () => {
  it('keeps Enable off until the box is ticked, and off while there is no signing key', async () => {
    const { user } = await open();
    const enable = screen.getByRole('button', { name: 'Enable Offsite S3' });
    expect(enable).toBeDisabled();
    expect(screen.getByTestId('enable-gate')).toHaveTextContent('tick');
    await user.click(
      screen.getByRole('checkbox', { name: /I have saved the private key and the signing public key/ }),
    );
    expect(enable).toBeEnabled();
  });

  it('keeps Enable off with the box ticked when the signing key does not exist', async () => {
    const { user } = await open({ state: { status: status({ signingPublic: '' }) } });
    await user.click(screen.getByRole('checkbox', { name: /I have saved the private key/ }));
    expect(screen.getByRole('button', { name: 'Enable Offsite S3' })).toBeDisabled();
    expect(screen.getByTestId('enable-gate')).toHaveTextContent('signing public key must exist');
  });

  it('enables with a PATCH and maps a 409 to a fixed sentence', async () => {
    let fail = true;
    const { api, user } = await open({
      routes: {
        [`PATCH ${P}/destinations/:id`]: () =>
          fail ? err(409, 'conflict') : { json: destination({ enabled: true }) },
      },
    });
    await user.click(screen.getByRole('checkbox', { name: /I have saved the private key/ }));
    await user.click(screen.getByRole('button', { name: 'Enable Offsite S3' }));
    expect(await screen.findByText(/cannot be enabled yet/)).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
    fail = false;
    await user.click(screen.getByRole('button', { name: 'Enable Offsite S3' }));
    await waitFor(() => expect(callsTo(api, 'PATCH', '/destinations/dest1')).toHaveLength(2));
    expect(callsTo(api, 'PATCH', '/destinations/dest1')[1]!.body).toEqual({ enabled: true });
  });
});

describe('destinations: create', () => {
  async function fillS3(user: ReturnType<typeof userEvent.setup>) {
    await user.click(screen.getByRole('button', { name: 'Add a destination' }));
    const dlg = await screen.findByRole('dialog', { name: 'Add a destination' });
    await user.type(within(dlg).getByLabelText('Name'), 'New bucket');
    await user.type(within(dlg).getByLabelText('Endpoint'), 'https://s3.example.org');
    await user.type(within(dlg).getByLabelText('Bucket'), 'my-bucket');
    return dlg;
  }

  it('offers only the supported types', async () => {
    const { user } = await open({ state: { status: status({ supportedTypes: ['webdav'] }) } });
    await user.click(screen.getByRole('button', { name: 'Add a destination' }));
    const dlg = await screen.findByRole('dialog');
    const opts = within(within(dlg).getByLabelText('Type')).getAllByRole('option');
    expect(opts.map((o) => o.textContent)).toEqual(['WebDAV or Nextcloud']);
  });

  it('disables Add when the server has no driver', async () => {
    await open({ state: { status: status({ supportedTypes: [] }) } });
    expect(screen.getByRole('button', { name: 'Add a destination' })).toBeDisabled();
  });

  it('sends the config and secrets once, then the secrets leave the page and every cache', async () => {
    const SECRET = 'SUPER-SECRET-VALUE-123';
    const { api, user, client } = await open({
      routes: {
        [`POST ${P}/destinations`]: (r) => ({
          status: 201,
          json: destination({ id: 'new1', name: (r.body as { name: string }).name }),
        }),
      },
    });
    const dlg = await fillS3(user);
    await user.type(within(dlg).getByLabelText('Access key'), 'AKIA-ACCESS');
    const secretBox = within(dlg).getByLabelText('Secret key');
    expect(secretBox).toHaveAttribute('type', 'password');
    await user.type(secretBox, SECRET);
    await user.click(within(dlg).getByRole('button', { name: 'Save destination' }));
    await waitFor(() => expect(callsTo(api, 'POST', '/platform/backups/destinations')).toHaveLength(1));
    const body = callsTo(api, 'POST', '/platform/backups/destinations')[0]!.body as Record<string, unknown>;
    expect(body).toMatchObject({
      name: 'New bucket',
      type: 's3',
      config: {
        endpoint: 'https://s3.example.org',
        bucket: 'my-bucket',
        region: 'us-east-1',
        addressing: 'path',
      },
      secrets: { accessKey: 'AKIA-ACCESS', secretKey: SECRET },
      schedule: 'daily',
      keepLast: 14,
    });
    expect(await screen.findByTestId('dest-note')).toHaveTextContent('New bucket is saved and disabled');
    // Gone: the dialog, the DOM, the storage, the query cache and the mutation cache.
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(document.body.innerHTML).not.toContain(SECRET);
    expect(JSON.stringify({ ...localStorage }) + JSON.stringify({ ...sessionStorage })).not.toContain(SECRET);
    expect(
      JSON.stringify(
        client
          .getQueryCache()
          .getAll()
          .map((q) => q.state.data),
      ),
    ).not.toContain(SECRET);
    expect(client.getMutationCache().getAll()).toHaveLength(0);
  });

  it('checks the fields before it sends', async () => {
    const { api, user } = await open();
    await user.click(screen.getByRole('button', { name: 'Add a destination' }));
    const dlg = await screen.findByRole('dialog');
    await user.click(within(dlg).getByRole('button', { name: 'Save destination' }));
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('Name is required.');
    await user.type(within(dlg).getByLabelText('Name'), 'x');
    await user.click(within(dlg).getByRole('button', { name: 'Save destination' }));
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('Endpoint is required.');
    expect(callsTo(api, 'POST', '/platform/backups/destinations')).toHaveLength(0);
  });

  it('asks for a confirm when only 1 backup is kept', async () => {
    const { api, user } = await open({
      routes: { [`POST ${P}/destinations`]: () => ({ status: 201, json: destination() }) },
    });
    const dlg = await fillS3(user);
    await user.type(within(dlg).getByLabelText('Access key'), 'a');
    await user.type(within(dlg).getByLabelText('Secret key'), 'b');
    await user.clear(within(dlg).getByLabelText('Keep the newest'));
    await user.type(within(dlg).getByLabelText('Keep the newest'), '1');
    await user.click(within(dlg).getByRole('button', { name: 'Save destination' }));
    expect(await within(dlg).findByText(/Keeping only 1 backup is risky/)).toBeInTheDocument();
    await user.click(within(dlg).getByRole('checkbox', { name: /keeping only 1 backup is risky/ }));
    await user.click(within(dlg).getByRole('button', { name: 'Save destination' }));
    await waitFor(() => expect(callsTo(api, 'POST', '/platform/backups/destinations')).toHaveLength(1));
    expect(callsTo(api, 'POST', '/platform/backups/destinations')[0]!.body).toMatchObject({
      keepLast: 1,
      confirmLowKeep: true,
    });
  });

  it('WebDAV: says to include the full WebDAV path and refuses http://', async () => {
    const { api, user } = await open();
    await user.click(screen.getByRole('button', { name: 'Add a destination' }));
    const dlg = await screen.findByRole('dialog');
    await user.selectOptions(within(dlg).getByLabelText('Type'), 'webdav');
    expect(dlg).toHaveTextContent(
      'Include the full WebDAV path, for example https://cloud.example.com/remote.php/dav/files/<user>/',
    );
    await user.type(within(dlg).getByLabelText('Name'), 'Cloud');
    await user.type(
      within(dlg).getByLabelText('WebDAV base URL'),
      'http://cloud.example.com/remote.php/dav/files/me/',
    );
    await user.type(within(dlg).getByLabelText('Username'), 'me');
    await user.type(within(dlg).getByLabelText('Password (app password)'), 'pw');
    await user.click(within(dlg).getByRole('button', { name: 'Save destination' }));
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('must start with https://');
    expect(callsTo(api, 'POST', '/platform/backups/destinations')).toHaveLength(0);
  });

  it('maps a failed create to a fixed sentence', async () => {
    const { user } = await open({
      routes: { [`POST ${P}/destinations`]: () => err(409, 'conflict') },
    });
    const dlg = await fillS3(user);
    await user.type(within(dlg).getByLabelText('Access key'), 'a');
    await user.type(within(dlg).getByLabelText('Secret key'), 'b');
    await user.click(within(dlg).getByRole('button', { name: 'Save destination' }));
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('The server refused this change');
    expect(document.body.textContent).not.toContain(MARKER);
  });
});

describe('destinations: edit and write-only secrets', () => {
  async function openEdit(opts: Parameters<typeof backupServer>[0] = {}) {
    const ctx = await open(opts);
    await ctx.user.click(screen.getByRole('button', { name: 'Edit Offsite S3' }));
    const dlg = await screen.findByRole('dialog', { name: 'Edit Offsite S3' });
    return { ...ctx, dlg };
  }

  it('shows Set for a stored secret and never a value', async () => {
    const { dlg } = await openEdit();
    expect(within(dlg).getByLabelText('Access key')).toHaveValue('');
    expect(within(dlg).getByLabelText('Secret key')).toHaveValue('');
    expect(within(dlg).getByTestId('secret-status-Access key')).toHaveTextContent('Set');
    expect(within(dlg).getByTestId('secret-status-Secret key')).toHaveTextContent('Set');
  });

  it('a change that keeps no secret sends only that change', async () => {
    const { api, user, dlg } = await openEdit({
      routes: { [`PATCH ${P}/destinations/:id`]: () => ({ json: destination({ keepLast: 20 }) }) },
    });
    const keep = within(dlg).getByLabelText('Keep the newest');
    await user.clear(keep);
    await user.type(keep, '20');
    await user.click(within(dlg).getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(callsTo(api, 'PATCH', '/destinations/dest1')).toHaveLength(1));
    expect(callsTo(api, 'PATCH', '/destinations/dest1')[0]!.body).toEqual({ keepLast: 20 });
  });

  it('a changed connection needs every stored secret again, and says so', async () => {
    const { api, user, dlg } = await openEdit({
      routes: { [`PATCH ${P}/destinations/:id`]: () => ({ json: destination() }) },
    });
    const save = within(dlg).getByRole('button', { name: 'Save changes' });
    await user.clear(within(dlg).getByLabelText('Bucket'));
    await user.type(within(dlg).getByLabelText('Bucket'), 'other-bucket');
    expect(await within(dlg).findByTestId('reenter-warning')).toHaveTextContent('Access key, Secret key');
    expect(save).toBeDisabled();
    expect(within(dlg).getByTestId('secret-status-Access key')).toHaveTextContent('Enter it again');
    await user.type(within(dlg).getByLabelText('Access key'), 'NEWKEY');
    expect(save).toBeDisabled();
    await user.click(within(dlg).getByRole('button', { name: 'Clear Secret key' }));
    expect(save).toBeEnabled();
    await user.click(save);
    await waitFor(() => expect(callsTo(api, 'PATCH', '/destinations/dest1')).toHaveLength(1));
    const body = callsTo(api, 'PATCH', '/destinations/dest1')[0]!.body as Record<string, unknown>;
    expect((body.config as Record<string, unknown>).bucket).toBe('other-bucket');
    expect(body.secrets).toEqual({ accessKey: 'NEWKEY', secretKey: '' });
  });

  it('Clear sends an empty string and Keep it undoes it', async () => {
    const { api, user, dlg } = await openEdit({
      routes: { [`PATCH ${P}/destinations/:id`]: () => ({ json: destination() }) },
    });
    await user.click(within(dlg).getByRole('button', { name: 'Clear Access key' }));
    expect(within(dlg).getByTestId('secret-status-Access key')).toHaveTextContent('It will be cleared');
    await user.click(within(dlg).getByRole('button', { name: 'Keep Access key' }));
    expect(within(dlg).getByTestId('secret-status-Access key')).toHaveTextContent(/^Set$/);
    await user.click(within(dlg).getByRole('button', { name: 'Clear Access key' }));
    await user.click(within(dlg).getByRole('button', { name: 'Save changes' }));
    await waitFor(() => expect(callsTo(api, 'PATCH', '/destinations/dest1')).toHaveLength(1));
    expect(callsTo(api, 'PATCH', '/destinations/dest1')[0]!.body).toEqual({ secrets: { accessKey: '' } });
  });

  it('shows a destination whose secrets cannot be opened', async () => {
    await open({ state: { destinations: [destination({ secretsUnreadable: true })] } });
    expect(screen.getByText('Secrets unreadable')).toBeInTheDocument();
    expect(screen.getByText(/cannot open the stored secrets/)).toBeInTheDocument();
  });

  it('never renders a secret even if a reply wrongly carried one', async () => {
    await open({
      state: { destinations: [destination({ secrets: { accessKey: 'LEAKED-1' }, accessKey: 'LEAKED-2' })] },
    });
    expect(document.body.textContent).not.toContain('LEAKED');
    expect(document.body.innerHTML).not.toContain('LEAKED');
  });
});

describe('probe, confirm and test', () => {
  const probeRun = (step: Record<string, unknown>, over: Record<string, unknown> = {}) =>
    run({
      id: 'p1',
      trigger: 'probe',
      destinationId: null,
      steps: [{ name: 'probe', ok: true, ...step }],
      ...over,
    });

  async function openCreate(opts: Parameters<typeof backupServer>[0] = {}) {
    const ctx = await open(opts);
    await ctx.user.click(screen.getByRole('button', { name: 'Add a destination' }));
    const dlg = await screen.findByRole('dialog');
    await ctx.user.type(within(dlg).getByLabelText('Name'), 'N');
    await ctx.user.type(within(dlg).getByLabelText('Endpoint'), 'https://s3.example.org');
    await ctx.user.type(within(dlg).getByLabelText('Bucket'), 'bk');
    await ctx.user.type(within(dlg).getByLabelText('Access key'), 'AK');
    await ctx.user.type(within(dlg).getByLabelText('Secret key'), 'SK');
    return { ...ctx, dlg };
  }

  it('a trusted certificate needs no pin', async () => {
    const r = probeRun({ kind: 'tls', fingerprint: FP, chainVerified: true });
    const { user, dlg, api } = await openCreate({
      state: { runById: { p1: r } },
      routes: { [`POST ${P}/destinations/probe`]: () => ({ status: 202, json: r }) },
    });
    await user.click(within(dlg).getByRole('button', { name: 'Probe the server' }));
    expect(await within(dlg).findByTestId('probe-trusted')).toHaveTextContent('valid for this host');
    expect(within(dlg).getByRole('button', { name: 'Test the connection' })).toBeEnabled();
    expect(callsTo(api, 'POST', '/destinations/probe')[0]!.body).toMatchObject({
      type: 's3',
      config: { endpoint: 'https://s3.example.org', bucket: 'bk' },
    });
    // The probe sends no credentials.
    expect(JSON.stringify(callsTo(api, 'POST', '/destinations/probe')[0]!.body)).not.toContain('SK');
  });

  it('an untrusted certificate shows the fingerprint and needs an explicit confirm before Test', async () => {
    const r = probeRun({ kind: 'tls', fingerprint: FP, chainVerified: false });
    const t = run({
      id: 't1',
      trigger: 'test',
      destinationId: null,
      steps: [
        { name: 'connect', ok: true },
        { name: 'auth', ok: true },
        { name: 'write', ok: false, code: 'forbidden' },
      ],
      status: 'failed',
      errorCode: 'forbidden',
      errorText: MARKER,
    });
    const { user, dlg, api } = await openCreate({
      state: { runById: { p1: r, t1: t } },
      routes: {
        [`POST ${P}/destinations/probe`]: () => ({ status: 202, json: r }),
        [`POST ${P}/destinations/test`]: () => ({ status: 202, json: t }),
      },
    });
    await user.click(within(dlg).getByRole('button', { name: 'Probe the server' }));
    expect(await within(dlg).findByTestId('probe-fingerprint')).toHaveTextContent(FP);
    const test = within(dlg).getByRole('button', { name: 'Test the connection' });
    expect(test).toBeDisabled();
    await user.click(within(dlg).getByRole('checkbox', { name: /I compared this fingerprint/ }));
    expect(test).toBeEnabled();
    await user.click(test);
    const steps = await within(dlg).findByRole('list', { name: 'Test steps' });
    expect(within(steps).getAllByRole('listitem')).toHaveLength(3);
    expect(steps).toHaveTextContent('Write a test file');
    expect(steps).toHaveTextContent('The account has no right to do this');
    expect(document.body.textContent).not.toContain(MARKER);
    const body = callsTo(api, 'POST', '/destinations/test')[0]!.body as Record<string, unknown>;
    expect(body.confirmedFingerprint).toBe(FP);
    expect((body.config as Record<string, unknown>).tlsPin).toBe(FP);
    expect(body.secrets).toEqual({ accessKey: 'AK', secretKey: 'SK' });
    // The pin is now in the form, so Save will keep it.
    expect(within(dlg).getByLabelText('TLS pin')).toHaveValue(FP);
  });

  it('a plain http kind is shown as not encrypted in transit and needs a confirm', async () => {
    const r = probeRun({ kind: 'plain', fingerprint: '', chainVerified: false });
    const { user, dlg } = await openCreate({
      state: { runById: { p1: r } },
      routes: { [`POST ${P}/destinations/probe`]: () => ({ status: 202, json: r }) },
    });
    await user.click(within(dlg).getByRole('button', { name: 'Probe the server' }));
    const warn = await within(dlg).findByTestId('probe-plain');
    expect(warn).toHaveTextContent('Not encrypted in transit.');
    expect(warn).toHaveTextContent('http://');
    const test = within(dlg).getByRole('button', { name: 'Test the connection' });
    expect(test).toBeDisabled();
    await user.click(within(dlg).getByRole('checkbox', { name: /connection is not encrypted in transit/ }));
    expect(test).toBeEnabled();
  });

  it('an SSH host key needs a confirm', async () => {
    const r = probeRun({ kind: 'ssh', fingerprint: 'SHA256:abcDEF123' });
    const ctx = await open({
      state: { status: status({ supportedTypes: ['sftp'] }), runById: { p1: r } },
      routes: { [`POST ${P}/destinations/probe`]: () => ({ status: 202, json: r }) },
    });
    await ctx.user.click(screen.getByRole('button', { name: 'Add a destination' }));
    const dlg = await screen.findByRole('dialog');
    await ctx.user.type(within(dlg).getByLabelText('Name'), 'N');
    await ctx.user.type(within(dlg).getByLabelText('Host'), 'sftp.example.org');
    await ctx.user.type(within(dlg).getByLabelText('Username'), 'u');
    await ctx.user.type(within(dlg).getByLabelText('Password'), 'p');
    await ctx.user.click(within(dlg).getByRole('button', { name: 'Probe the server' }));
    expect(await within(dlg).findByTestId('probe-fingerprint')).toHaveTextContent('SHA256:abcDEF123');
    expect(dlg).toHaveTextContent('SSH host key');
    expect(within(dlg).getByRole('button', { name: 'Test the connection' })).toBeDisabled();
  });

  it('a failed probe shows a fixed sentence for the code, not the remote words', async () => {
    const r = run({
      id: 'p1',
      trigger: 'probe',
      destinationId: null,
      status: 'failed',
      steps: [{ name: 'probe', ok: false, code: 'dns_failed' }],
      errorCode: 'dns_failed',
      errorText: MARKER,
    });
    const { user, dlg } = await openCreate({
      state: { runById: { p1: r } },
      routes: { [`POST ${P}/destinations/probe`]: () => ({ status: 202, json: r }) },
    });
    await user.click(within(dlg).getByRole('button', { name: 'Probe the server' }));
    expect(await within(dlg).findByText('The host name was not found.')).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('a saved destination with no pin: the confirmed fingerprint goes to the saved test route', async () => {
    const r = probeRun({ kind: 'tls', fingerprint: FP, chainVerified: false });
    const t = run({ id: 't1', trigger: 'test', status: 'succeeded', steps: [{ name: 'connect', ok: true }] });
    const ctx = await open({
      state: { runById: { p1: r, t1: t } },
      routes: {
        [`POST ${P}/destinations/:id/probe`]: () => ({ status: 202, json: r }),
        [`POST ${P}/destinations/:id/test`]: () => ({ status: 202, json: t }),
      },
    });
    await ctx.user.click(screen.getByRole('button', { name: 'Edit Offsite S3' }));
    const dlg = await screen.findByRole('dialog');
    await ctx.user.click(within(dlg).getByRole('button', { name: 'Probe the server' }));
    await within(dlg).findByTestId('probe-fingerprint');
    await ctx.user.click(within(dlg).getByRole('checkbox', { name: /I compared this fingerprint/ }));
    await ctx.user.click(within(dlg).getByRole('button', { name: 'Test the connection' }));
    await within(dlg).findByText('It worked.');
    const c = callsTo(ctx.api, 'POST', '/destinations/dest1/test');
    expect(c).toHaveLength(1);
    expect(c[0]!.body).toEqual({ confirmedFingerprint: FP });
    expect(callsTo(ctx.api, 'POST', '/destinations/test')).toHaveLength(0);
  });

  it('a rate limit and a missing key map to fixed sentences', async () => {
    const { user, dlg } = await openCreate({
      routes: { [`POST ${P}/destinations/probe`]: () => err(503, 'backup_unavailable') },
    });
    await user.click(within(dlg).getByRole('button', { name: 'Probe the server' }));
    expect(
      await within(dlg).findByText(/cannot do that\. It has no BACKUP_ENCRYPTION_KEY/),
    ).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('Dropbox cannot be tested before it is saved', async () => {
    const ctx = await open({ state: { status: status({ supportedTypes: ['dropbox'] }) } });
    await ctx.user.click(screen.getByRole('button', { name: 'Add a destination' }));
    const dlg = await screen.findByRole('dialog');
    expect(within(dlg).getByRole('button', { name: 'Test the connection' })).toBeDisabled();
    expect(dlg).toHaveTextContent('can be tested after it is saved and authorised');
  });
});

describe('Dropbox authorisation', () => {
  it('sends the code once, clears the box and polls the run', async () => {
    const CODE = 'ONE-TIME-CODE-XYZ';
    const x = run({
      id: 'x1',
      trigger: 'exchange',
      status: 'succeeded',
      steps: [{ name: 'exchange', ok: true }],
    });
    const ctx = await open({
      state: {
        status: status({ supportedTypes: ['dropbox'] }),
        destinations: [
          destination({
            id: 'db1',
            name: 'My Dropbox',
            type: 'dropbox',
            config: {},
            secretsSet: ['appKey', 'appSecret'],
          }),
        ],
        runById: { x1: x },
      },
      routes: { [`POST ${P}/destinations/:id/exchange`]: () => ({ status: 202, json: x }) },
    });
    await ctx.user.click(screen.getByRole('button', { name: 'Edit My Dropbox' }));
    const dlg = await screen.findByRole('dialog');
    expect(dlg).toHaveTextContent('Not authorised yet.');
    const box = within(dlg).getByLabelText('Authorisation code');
    expect(box).toHaveAttribute('type', 'password');
    await ctx.user.type(box, CODE);
    await ctx.user.click(within(dlg).getByRole('button', { name: 'Authorise' }));
    await within(dlg).findByText('It worked.');
    expect(callsTo(ctx.api, 'POST', '/destinations/db1/exchange')[0]!.body).toEqual({ code: CODE });
    expect(box).toHaveValue('');
    expect(document.body.innerHTML).not.toContain(CODE);
  });
});

describe('run now, delete and error mapping', () => {
  it('Back up now queues a run; destination_busy is a fixed sentence', async () => {
    let busy = false;
    const { api, user } = await open({
      state: { destinations: [destination({ enabled: true })] },
      routes: {
        [`POST ${P}/destinations/:id/run`]: () =>
          busy
            ? err(409, 'destination_busy')
            : { status: 202, json: run({ trigger: 'manual', status: 'queued' }) },
      },
    });
    await user.click(screen.getByRole('button', { name: 'Back up Offsite S3 now' }));
    expect(await screen.findByTestId('dest-note')).toHaveTextContent('A backup of Offsite S3 is queued');
    expect(callsTo(api, 'POST', '/destinations/dest1/run')).toHaveLength(1);
    busy = true;
    await user.click(screen.getByRole('button', { name: 'Back up Offsite S3 now' }));
    expect(await screen.findByText(/already queued or running/)).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('deletes after a confirm', async () => {
    const { api, user } = await open({
      routes: { [`DELETE ${P}/destinations/:id`]: () => ({ status: 204 }) },
    });
    await user.click(screen.getByRole('button', { name: 'Delete Offsite S3' }));
    const dlg = await screen.findByRole('dialog', { name: 'Delete this destination?' });
    expect(callsTo(api, 'DELETE', '/destinations/dest1')).toHaveLength(0);
    await user.click(within(dlg).getByRole('button', { name: 'Delete destination' }));
    await waitFor(() => expect(callsTo(api, 'DELETE', '/destinations/dest1')).toHaveLength(1));
    expect(await screen.findByTestId('dest-note')).toHaveTextContent('is deleted');
  });

  it('backupErrorText maps every status and code to a fixed sentence and ignores the server message', () => {
    const e = (s: number, c: string) => new ApiError(s, c, MARKER);
    const cases: [ApiError, Parameters<typeof backupErrorText>[1], RegExp][] = [
      [e(0, 'network_error'), 'check', /Could not reach the server/],
      [e(401, 'unauthorized'), 'check', /signed out/],
      [e(403, 'forbidden'), 'recipientCancel', /Another platform admin must cancel/],
      [e(403, 'forbidden'), 'destination', /cannot change the backup settings/],
      [e(404, 'not_found'), 'destination', /does not exist any more/],
      [e(409, 'destination_busy'), 'run', /already queued or running/],
      [e(409, 'conflict'), 'enable', /cannot be enabled yet/],
      [e(409, 'conflict'), 'recipient', /limit of 5 keys/],
      [e(409, 'conflict'), 'destination', /server refused this change/],
      [e(400, 'validation_failed'), 'destination', /enter every stored secret again/],
      [e(429, 'rate_limited'), 'check', /Too many requests/],
      [e(503, 'backup_unavailable'), 'check', /BACKUP_ENCRYPTION_KEY/],
      [e(500, 'internal'), 'load', /server had a problem/],
    ];
    for (const [error, ctx, re] of cases) {
      const text = backupErrorText(error, ctx);
      expect(text).toMatch(re);
      expect(text).not.toContain(MARKER);
    }
    expect(backupErrorText(new Error(MARKER), 'check')).not.toContain(MARKER);
  });

  it('codeText and changeText only produce our own words', () => {
    expect(codeText('tls_pin_mismatch')).toMatch(/certificate key changed/);
    expect(codeText('weird_new_code')).toBe('The run failed (code weird_new_code).');
    expect(codeText('<script>alert(1)</script>')).toBe('');
    expect(safeCode('A B')).toBe('');
    expect(changeText({ kind: 'destination_added' })).toBe('A destination was added.');
    expect(changeText({ kind: 'x'.repeat(100) })).toBe('A backup setting changed (unknown).');
  });
});

describe('recipients', () => {
  const pending = recipient({
    fingerprint: 'b'.repeat(64),
    state: 'pending',
    effectiveAt: '2099-01-02T00:00:00Z',
  });
  const removing = recipient({
    fingerprint: 'c'.repeat(64),
    state: 'removing',
    removeAt: '2099-01-02T00:00:00Z',
    removeBy: 'u2',
  });

  it('lists state, hold time and fingerprints, and cancels a pending add', async () => {
    const { api, user } = await open({
      state: { recipients: [recipient(), pending, removing] },
      routes: {
        [`POST ${P}/recipients/:fp/cancel`]: () => ({
          json: { items: [recipient()], everHadRecipient: true, limit: 5 },
        }),
      },
    });
    const list = screen.getByRole('list', { name: 'Encryption keys' });
    expect(within(list).getAllByRole('listitem')).toHaveLength(3);
    expect(list).toHaveTextContent('Pending: held until');
    expect(list).toHaveTextContent('Removing: active until');
    expect(list).toHaveTextContent('b'.repeat(12));
    await user.click(screen.getByRole('button', { name: `Cancel the add of key ${'b'.repeat(12)}` }));
    await waitFor(() => expect(callsTo(api, 'POST', `/recipients/${'b'.repeat(64)}/cancel`)).toHaveLength(1));
    expect(await screen.findByTestId('recipient-note')).toHaveTextContent('is cancelled');
  });

  it('a removal that you scheduled cannot be cancelled by you: a fixed 403 sentence', async () => {
    const { user } = await open({
      state: { recipients: [recipient(), removing] },
      routes: { [`POST ${P}/recipients/:fp/cancel`]: () => err(403, 'forbidden') },
    });
    await user.click(screen.getByRole('button', { name: `Cancel the removal of key ${'c'.repeat(12)}` }));
    expect(await screen.findByText(/Another platform admin must cancel/)).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('schedules a removal after a confirm that names the host command', async () => {
    const { api, user } = await open({
      routes: {
        [`DELETE ${P}/recipients/:fp`]: () => ({
          json: { items: [recipient({ state: 'removing' })], everHadRecipient: true, limit: 5 },
        }),
      },
    });
    await user.click(screen.getByRole('button', { name: `Remove key ${FP.slice(0, 12)}` }));
    const dlg = await screen.findByRole('dialog', { name: 'Remove this key?' });
    expect(dlg).toHaveTextContent('server backup-recipient-remove --fingerprint');
    await user.click(within(dlg).getByRole('button', { name: 'Schedule the removal' }));
    await waitFor(() => expect(callsTo(api, 'DELETE', `/recipients/${FP}`)).toHaveLength(1));
    expect(await screen.findByTestId('recipient-note')).toHaveTextContent('in 24 hours');
  });

  it('adds a key, tells about the hold, and maps a 409', async () => {
    const KEY = 'age1' + 'q'.repeat(58);
    let conflict = false;
    const { api, user } = await open({
      routes: {
        [`POST ${P}/recipients`]: () =>
          conflict
            ? err(409, 'conflict')
            : {
                status: 201,
                json: recipient({
                  state: 'pending',
                  effectiveAt: '2099-01-02T00:00:00Z',
                  fingerprint: 'd'.repeat(64),
                }),
              },
      },
    });
    const form = screen.getByRole('form', { name: 'Add an encryption key' });
    expect(form).toHaveTextContent('held for 24 hours');
    await user.type(within(form).getByLabelText('Public key (age1…)'), 'not a key');
    await user.click(within(form).getByRole('button', { name: 'Add key' }));
    expect(await screen.findByText(/Enter an age public key/)).toBeInTheDocument();
    expect(callsTo(api, 'POST', '/recipients')).toHaveLength(0);
    await user.clear(within(form).getByLabelText('Public key (age1…)'));
    await user.type(within(form).getByLabelText('Public key (age1…)'), KEY);
    await user.click(within(form).getByRole('button', { name: 'Add key' }));
    expect(await screen.findByTestId('recipient-note')).toHaveTextContent('Backups are encrypted to it from');
    expect(callsTo(api, 'POST', '/recipients')[0]!.body).toEqual({ publicKey: KEY });
    conflict = true;
    await user.type(within(form).getByLabelText('Public key (age1…)'), KEY);
    await user.click(within(form).getByRole('button', { name: 'Add key' }));
    expect(await screen.findByText(/limit of 5 keys/)).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
  });
});

describe('notices', () => {
  it('saves the whole notice object', async () => {
    const { api, user } = await open();
    const form = screen.getByRole('form', { name: 'Backup notices' });
    const save = within(form).getByRole('button', { name: 'Save notices' });
    expect(save).toBeDisabled();
    await user.click(within(form).getByRole('checkbox', { name: 'Mail when a backup succeeds' }));
    await user.clear(within(form).getByLabelText('Stale alert after (hours)'));
    await user.type(within(form).getByLabelText('Stale alert after (hours)'), '0');
    await user.click(save);
    await waitFor(() => expect(callsTo(api, 'PUT', '/platform/backups/settings')).toHaveLength(1));
    expect(callsTo(api, 'PUT', '/platform/backups/settings')[0]!.body).toEqual({
      notifyOnFailure: true,
      notifyOnSuccess: true,
      staleAlertHours: 0,
    });
    expect(await within(form).findByText('Notices saved.')).toBeInTheDocument();
  });

  it('refuses a bad number and maps a server refusal', async () => {
    const { api, user } = await open({
      routes: { [`PUT ${P}/settings`]: () => err(400, 'validation_failed') },
    });
    const form = screen.getByRole('form', { name: 'Backup notices' });
    const hours = within(form).getByLabelText('Stale alert after (hours)');
    await user.clear(hours);
    await user.type(hours, 'abc');
    await user.click(within(form).getByRole('button', { name: 'Save notices' }));
    expect(await within(form).findByRole('alert')).toHaveTextContent('whole number');
    expect(callsTo(api, 'PUT', '/platform/backups/settings')).toHaveLength(0);
    await user.clear(hours);
    await user.type(hours, '5');
    await user.click(within(form).getByRole('button', { name: 'Save notices' }));
    expect(await within(form).findByRole('alert')).toHaveTextContent('did not accept these values');
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('warns when mail is not ready', async () => {
    await open({ state: { status: status({ mailReady: false }) } });
    expect(screen.getByRole('form', { name: 'Backup notices' })).toHaveTextContent(
      'none of these mails is sent now',
    );
  });
});

describe('runs and the change log', () => {
  it('lists runs with fixed result text, opens a detail and never shows errorText', async () => {
    const failed = run({
      id: 'r2',
      status: 'failed',
      errorCode: 'quota_exceeded',
      errorText: MARKER,
      steps: [{ name: 'write', ok: false, code: 'quota_exceeded' }],
    });
    const { user } = await open({ state: { runs: [run(), failed] } });
    const table = await screen.findByRole('table', { name: 'Backup runs' });
    expect(within(table).getAllByRole('row')).toHaveLength(3);
    expect(table).toHaveTextContent('The destination is full or over its quota.');
    expect(table).toHaveTextContent('Succeeded');
    await user.click(within(table).getAllByRole('button', { name: /Details of the run/ })[1]!);
    expect(table).toHaveTextContent('Write a test file: Failed.');
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('filters runs by destination and pages with the cursor', async () => {
    let calls = 0;
    const { api, user } = await open({
      routes: {
        [`GET ${P}/runs`]: (r) => {
          calls++;
          const cursor = r.url.searchParams.get('cursor');
          return {
            json: cursor
              ? { items: [run({ id: 'r-b', destinationName: 'Second page' })], nextCursor: null }
              : { items: [run({ id: 'r-a' })], nextCursor: 'CUR1' },
          };
        },
      },
    });
    await screen.findByRole('table', { name: 'Backup runs' });
    await user.click(screen.getByRole('button', { name: 'Show more runs' }));
    expect(await screen.findByText('Second page')).toBeInTheDocument();
    expect(calls).toBeGreaterThanOrEqual(2);
    await user.selectOptions(screen.getByLabelText('Destination'), 'dest1');
    await waitFor(() =>
      expect(
        callsTo(api, 'GET', '/platform/backups/runs').some(
          (c) => c.url.searchParams.get('destination') === 'dest1',
        ),
      ).toBe(true),
    );
  });

  it('shows the change log in fixed sentences, with the host command as the actor', async () => {
    await open({
      state: {
        changes: [
          {
            id: 'c1',
            at: '2026-10-28T10:00:00Z',
            actorId: 'u1',
            actorUsername: 'chen',
            kind: 'destination_enabled',
            destinationId: 'dest1',
            destinationName: 'Offsite S3',
            detail: { identity: 'secret-host.example.com' },
          },
          {
            id: 'c2',
            at: '2026-10-28T09:00:00Z',
            actorId: null,
            actorUsername: null,
            kind: 'recipient_removed_by_host',
            destinationId: null,
            destinationName: '',
            detail: { fingerprint: 'abcdef012345' + '0'.repeat(52) },
          },
          {
            id: 'c3',
            at: '2026-10-28T08:00:00Z',
            actorId: null,
            actorUsername: null,
            kind: 'some_future_kind',
            destinationId: null,
            destinationName: '',
            detail: { text: MARKER },
          },
        ],
      },
    });
    const table = await screen.findByRole('table', { name: 'Backup change log' });
    expect(table).toHaveTextContent('A destination was enabled. Destination: Offsite S3.');
    expect(table).toHaveTextContent('A command on the host');
    expect(table).toHaveTextContent('Key abcdef012345.');
    expect(table).toHaveTextContent('A backup setting changed (some_future_kind).');
    expect(document.body.textContent).not.toContain('secret-host');
    expect(document.body.textContent).not.toContain(MARKER);
  });
});

// ---------- review round 1 ----------

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** Every place in the page where `text` is visible or readable, except the value of an input. */
function whereVisible(text: string): string[] {
  const hits: string[] = [];
  for (const el of Array.from(document.body.querySelectorAll('*'))) {
    const tag = el.tagName;
    for (const a of Array.from(el.attributes)) {
      if (a.name === 'value' && (tag === 'INPUT' || tag === 'TEXTAREA')) continue;
      if (a.value.includes(text)) hits.push(`${tag}[${a.name}]`);
    }
    if (tag === 'TEXTAREA') continue;
    for (const n of Array.from(el.childNodes))
      if (n.nodeType === Node.TEXT_NODE && (n.textContent ?? '').includes(text)) hits.push(`${tag} text`);
  }
  return hits;
}

function deferred() {
  let release!: () => void;
  const gate = new Promise<void>((r) => (release = r));
  return { gate, release };
}

afterEach(() => {
  pollConfig.ms = 1500;
});

describe('schedule and limits', () => {
  async function openAdd(opts: Parameters<typeof backupServer>[0] = {}) {
    const ctx = await open(opts);
    await ctx.user.click(screen.getByRole('button', { name: 'Add a destination' }));
    const dlg = await screen.findByRole('dialog');
    await ctx.user.type(within(dlg).getByLabelText('Name'), 'N');
    await ctx.user.type(within(dlg).getByLabelText('Endpoint'), 'https://s3.example.org');
    await ctx.user.type(within(dlg).getByLabelText('Bucket'), 'bk');
    await ctx.user.type(within(dlg).getByLabelText('Access key'), 'AK');
    await ctx.user.type(within(dlg).getByLabelText('Secret key'), 'SK');
    return { ...ctx, dlg };
  }

  it('verify every is 0 to 90', async () => {
    const { user, dlg, api } = await openAdd({
      routes: { [`POST ${P}/destinations`]: () => ({ status: 201, json: destination() }) },
    });
    const v = within(dlg).getByLabelText('Verify every (days)');
    await user.clear(v);
    await user.type(v, '91');
    await user.click(within(dlg).getByRole('button', { name: 'Save destination' }));
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('from 0 to 90');
    expect(callsTo(api, 'POST', '/platform/backups/destinations')).toHaveLength(0);
    await user.clear(v);
    await user.type(v, '90');
    await user.click(within(dlg).getByRole('button', { name: 'Save destination' }));
    await waitFor(() => expect(callsTo(api, 'POST', '/platform/backups/destinations')).toHaveLength(1));
    expect(callsTo(api, 'POST', '/platform/backups/destinations')[0]!.body).toMatchObject({
      verifyEveryDays: 90,
    });
  });

  it('a weekly destination sends the weekday', async () => {
    const { user, dlg, api } = await openAdd({
      routes: { [`POST ${P}/destinations`]: () => ({ status: 201, json: destination() }) },
    });
    await user.selectOptions(within(dlg).getByLabelText('Schedule'), 'weekly');
    await user.selectOptions(within(dlg).getByLabelText('Day of the week'), 'Friday');
    await user.click(within(dlg).getByRole('button', { name: 'Save destination' }));
    await waitFor(() => expect(callsTo(api, 'POST', '/platform/backups/destinations')).toHaveLength(1));
    expect(callsTo(api, 'POST', '/platform/backups/destinations')[0]!.body).toMatchObject({
      schedule: 'weekly',
      weekday: 5,
    });
  });

  it('refuses a long name and a folder with a slash at the start or the end', async () => {
    const { user, dlg, api } = await openAdd();
    const name = within(dlg).getByLabelText('Name');
    await user.type(name, 'x'.repeat(60));
    await user.click(within(dlg).getByRole('button', { name: 'Save destination' }));
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('at most 60 characters');
    await user.clear(name);
    await user.type(name, 'ok');
    await user.type(within(dlg).getByLabelText('Folder'), '/lead');
    await user.click(within(dlg).getByRole('button', { name: 'Save destination' }));
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('no slash at the start or at the end');
    expect(callsTo(api, 'POST', '/platform/backups/destinations')).toHaveLength(0);
  });

  it('SFTP may start the folder with one slash; the notice hours stop at 8760', async () => {
    const ctx = await open({ state: { status: status({ supportedTypes: ['sftp'] }) } });
    await ctx.user.click(screen.getByRole('button', { name: 'Add a destination' }));
    const dlg = await screen.findByRole('dialog');
    await ctx.user.type(within(dlg).getByLabelText('Name'), 'N');
    await ctx.user.type(within(dlg).getByLabelText('Host'), 'h.example.org');
    await ctx.user.type(within(dlg).getByLabelText('Username'), 'u');
    await ctx.user.type(within(dlg).getByLabelText('Password'), 'p');
    await ctx.user.type(within(dlg).getByLabelText('Folder'), '//two');
    await ctx.user.click(within(dlg).getByRole('button', { name: 'Save destination' }));
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('at most one slash');
    await ctx.user.click(within(dlg).getByRole('button', { name: 'Cancel' }));
    const form = screen.getByRole('form', { name: 'Backup notices' });
    const hours = within(form).getByLabelText('Stale alert after (hours)');
    await ctx.user.clear(hours);
    await ctx.user.type(hours, '9000');
    await ctx.user.click(within(form).getByRole('button', { name: 'Save notices' }));
    expect(await within(form).findByRole('alert')).toHaveTextContent('0 to 8760');
  });
});

describe('notices saved against a server that stamps a new updatedAt', () => {
  it('shows "Notices saved." after the save', async () => {
    const { user } = await open();
    const form = screen.getByRole('form', { name: 'Backup notices' });
    await user.click(within(form).getByRole('checkbox', { name: 'Mail when a backup succeeds' }));
    await user.click(within(form).getByRole('button', { name: 'Save notices' }));
    expect(await screen.findByText('Notices saved.')).toBeInTheDocument();
  });
});

describe('a typed secret is never readable in the page', () => {
  it('is only in the input value, not in a status text, a title or any attribute', async () => {
    const SECRET = 'TYPED-SECRET-ABC-987';
    const { user } = await open();
    await user.click(screen.getByRole('button', { name: 'Edit Offsite S3' }));
    const dlg = await screen.findByRole('dialog');
    await user.type(within(dlg).getByLabelText('Access key'), SECRET);
    expect(within(dlg).getByLabelText('Access key')).toHaveValue(SECRET);
    expect(whereVisible(SECRET)).toEqual([]);
  });

  it('a private key loaded from a file reaches the request and is not shown', async () => {
    const KEY = '-----BEGIN KEY-----\nFILE-KEY-DATA\n-----END KEY-----';
    const ctx = await open({
      state: { status: status({ supportedTypes: ['sftp'] }) },
      routes: { [`POST ${P}/destinations`]: () => ({ status: 201, json: destination() }) },
    });
    await ctx.user.click(screen.getByRole('button', { name: 'Add a destination' }));
    const dlg = await screen.findByRole('dialog');
    expect(dlg).toHaveTextContent('shows as plain text in some browsers');
    await ctx.user.type(within(dlg).getByLabelText('Name'), 'N');
    await ctx.user.type(within(dlg).getByLabelText('Host'), 'h.example.org');
    await ctx.user.type(within(dlg).getByLabelText('Username'), 'u');
    await ctx.user.upload(
      within(dlg).getByLabelText(/load the key from a file/),
      new File([KEY], 'id.pem', { type: 'text/plain' }),
    );
    await waitFor(() =>
      expect(within(dlg).getByTestId('secret-status-Private key')).toHaveTextContent(
        'Not set. It will be saved.',
      ),
    );
    expect(whereVisible('FILE-KEY-DATA')).toEqual([]);
    await ctx.user.click(within(dlg).getByRole('button', { name: 'Save destination' }));
    await waitFor(() => expect(callsTo(ctx.api, 'POST', '/platform/backups/destinations')).toHaveLength(1));
    expect(
      (
        callsTo(ctx.api, 'POST', '/platform/backups/destinations')[0]!.body as {
          secrets: Record<string, string>;
        }
      ).secrets,
    ).toEqual({ privateKey: KEY });
  });
});

describe('the probe is forgotten when the connection changes', () => {
  it('drops the fingerprint and the ticks when a connection field changes', async () => {
    const r = run({
      id: 'p1',
      trigger: 'probe',
      destinationId: null,
      steps: [{ name: 'probe', ok: true, kind: 'tls', fingerprint: FP, chainVerified: false }],
    });
    const ctx = await open({
      state: { runById: { p1: r } },
      routes: { [`POST ${P}/destinations/probe`]: () => ({ status: 202, json: r }) },
    });
    await ctx.user.click(screen.getByRole('button', { name: 'Add a destination' }));
    const dlg = await screen.findByRole('dialog');
    await ctx.user.type(within(dlg).getByLabelText('Endpoint'), 'https://s3.example.org');
    await ctx.user.type(within(dlg).getByLabelText('Bucket'), 'bk');
    await ctx.user.click(within(dlg).getByRole('button', { name: 'Probe the server' }));
    await within(dlg).findByTestId('probe-fingerprint');
    await ctx.user.click(within(dlg).getByRole('checkbox', { name: /I compared this fingerprint/ }));
    await ctx.user.type(within(dlg).getByLabelText('Endpoint'), 'x');
    expect(within(dlg).queryByTestId('probe-fingerprint')).not.toBeInTheDocument();
    expect(
      within(dlg).queryByRole('checkbox', { name: /I compared this fingerprint/ }),
    ).not.toBeInTheDocument();
    // A new probe starts with the tick off.
    await ctx.user.click(within(dlg).getByRole('button', { name: 'Probe the server' }));
    await within(dlg).findByTestId('probe-fingerprint');
    expect(within(dlg).getByRole('checkbox', { name: /I compared this fingerprint/ })).not.toBeChecked();
  });
});

describe('in-flight guards', () => {
  it('Delete destination sends one request for two clicks', async () => {
    const d = deferred();
    const { api, user } = await open({
      routes: {
        [`DELETE ${P}/destinations/:id`]: async () => {
          await d.gate;
          return { status: 204 };
        },
      },
    });
    await user.click(screen.getByRole('button', { name: 'Delete Offsite S3' }));
    const dlg = await screen.findByRole('dialog');
    const btn = within(dlg).getByRole('button', { name: 'Delete destination' });
    await user.click(btn);
    await user.click(within(dlg).getByRole('button', { name: /Deleting|Delete destination/ }));
    d.release();
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(callsTo(api, 'DELETE', '/destinations/dest1')).toHaveLength(1);
  });

  it('Schedule the removal sends one request for two clicks', async () => {
    const d = deferred();
    const { api, user } = await open({
      routes: {
        [`DELETE ${P}/recipients/:fp`]: async () => {
          await d.gate;
          return { json: { items: [recipient({ state: 'removing' })], everHadRecipient: true, limit: 5 } };
        },
      },
    });
    await user.click(screen.getByRole('button', { name: `Remove key ${FP.slice(0, 12)}` }));
    const dlg = await screen.findByRole('dialog');
    await user.click(within(dlg).getByRole('button', { name: 'Schedule the removal' }));
    await user.click(within(dlg).getByRole('button', { name: /Scheduling|Schedule the removal/ }));
    d.release();
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(callsTo(api, 'DELETE', `/recipients/${FP}`)).toHaveLength(1);
  });

  it('Authorise sends the code once for two clicks', async () => {
    const d = deferred();
    const x = run({ id: 'x1', trigger: 'exchange', steps: [{ name: 'exchange', ok: true }] });
    const ctx = await open({
      state: {
        status: status({ supportedTypes: ['dropbox'] }),
        destinations: [
          destination({
            id: 'db1',
            name: 'My Dropbox',
            type: 'dropbox',
            config: {},
            secretsSet: ['appKey', 'appSecret'],
          }),
        ],
        runById: { x1: x },
      },
      routes: {
        [`POST ${P}/destinations/:id/exchange`]: async () => {
          await d.gate;
          return { status: 202, json: x };
        },
      },
    });
    await ctx.user.click(screen.getByRole('button', { name: 'Edit My Dropbox' }));
    const dlg = await screen.findByRole('dialog');
    await ctx.user.type(within(dlg).getByLabelText('Authorisation code'), 'CODE-1');
    await ctx.user.click(within(dlg).getByRole('button', { name: 'Authorise' }));
    await ctx.user.click(within(dlg).getByRole('button', { name: /Sending|Authorise/ }));
    d.release();
    await within(dlg).findByText('It worked.');
    expect(callsTo(ctx.api, 'POST', '/destinations/db1/exchange')).toHaveLength(1);
  });

  it('a failed Authorise gives the code back so it can be sent again', async () => {
    const ctx = await open({
      state: {
        status: status({ supportedTypes: ['dropbox'] }),
        destinations: [
          destination({
            id: 'db1',
            name: 'My Dropbox',
            type: 'dropbox',
            config: {},
            secretsSet: ['appKey', 'appSecret'],
          }),
        ],
      },
      routes: { [`POST ${P}/destinations/:id/exchange`]: () => err(409, 'conflict') },
    });
    await ctx.user.click(screen.getByRole('button', { name: 'Edit My Dropbox' }));
    const dlg = await screen.findByRole('dialog');
    await ctx.user.type(within(dlg).getByLabelText('Authorisation code'), 'CODE-2');
    await ctx.user.click(within(dlg).getByRole('button', { name: 'Authorise' }));
    await within(dlg).findByText(/app key and app secret are not saved/);
    expect(within(dlg).getByLabelText('Authorisation code')).toHaveValue('CODE-2');
  });
});

describe('Dropbox access', () => {
  it('Full Dropbox needs a folder, and the request carries the access mode', async () => {
    const ctx = await open({
      state: { status: status({ supportedTypes: ['dropbox'] }) },
      routes: { [`POST ${P}/destinations`]: () => ({ status: 201, json: destination({ type: 'dropbox' }) }) },
    });
    await ctx.user.click(screen.getByRole('button', { name: 'Add a destination' }));
    const dlg = await screen.findByRole('dialog');
    expect(within(dlg).getByLabelText('App access')).toHaveValue('app_folder');
    await ctx.user.type(within(dlg).getByLabelText('Name'), 'DB');
    await ctx.user.type(within(dlg).getByLabelText('App key'), 'k');
    await ctx.user.type(within(dlg).getByLabelText('App secret'), 's');
    await ctx.user.selectOptions(within(dlg).getByLabelText('App access'), 'full_dropbox');
    await ctx.user.click(within(dlg).getByRole('button', { name: 'Save destination' }));
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('Folder is required with Full Dropbox');
    await ctx.user.type(within(dlg).getByLabelText('Folder'), 'launchpad');
    await ctx.user.click(within(dlg).getByRole('button', { name: 'Save destination' }));
    await waitFor(() => expect(callsTo(ctx.api, 'POST', '/platform/backups/destinations')).toHaveLength(1));
    expect(callsTo(ctx.api, 'POST', '/platform/backups/destinations')[0]!.body).toMatchObject({
      type: 'dropbox',
      config: { access: 'full_dropbox', folder: 'launchpad' },
    });
  });

  it('a saved Dropbox destination with no access in its config is not "changed"; a change blocks Test', async () => {
    const ctx = await open({
      state: {
        status: status({ supportedTypes: ['dropbox'] }),
        destinations: [
          destination({
            id: 'db1',
            name: 'My Dropbox',
            type: 'dropbox',
            config: {},
            secretsSet: ['appKey', 'appSecret', 'refreshToken'],
          }),
        ],
      },
    });
    await ctx.user.click(screen.getByRole('button', { name: 'Edit My Dropbox' }));
    const dlg = await screen.findByRole('dialog');
    expect(within(dlg).queryByTestId('reenter-warning')).not.toBeInTheDocument();
    expect(within(dlg).getByRole('button', { name: 'Test the connection' })).toBeEnabled();
    await ctx.user.type(within(dlg).getByLabelText('Folder'), 'sub');
    expect(within(dlg).getByRole('button', { name: 'Test the connection' })).toBeDisabled();
    expect(dlg).toHaveTextContent('Save your changes first');
  });
});

describe('recipient removal errors', () => {
  it('Remove has its own 403 and 409 sentences', async () => {
    let code = 403;
    const { user } = await open({
      routes: { [`DELETE ${P}/recipients/:fp`]: () => err(code, code === 403 ? 'forbidden' : 'conflict') },
    });
    await user.click(screen.getByRole('button', { name: `Remove key ${FP.slice(0, 12)}` }));
    await user.click(
      within(await screen.findByRole('dialog')).getByRole('button', { name: 'Schedule the removal' }),
    );
    expect(await screen.findByText('You cannot remove an encryption key.')).toBeInTheDocument();
    expect(document.body.textContent).not.toContain('Another platform admin must cancel');
    code = 409;
    await user.click(screen.getByRole('button', { name: `Remove key ${FP.slice(0, 12)}` }));
    await user.click(
      within(await screen.findByRole('dialog')).getByRole('button', { name: 'Schedule the removal' }),
    );
    expect(await screen.findByText(/a removal is already planned for this key/)).toBeInTheDocument();
  });
});

describe('waiting for a run', () => {
  it('says the worker may not run after 45 s without a start', () => {
    vi.useFakeTimers();
    render(<RunBox run={run({ status: 'queued', steps: [] }) as unknown as BackupRun} label="Test" />);
    expect(screen.queryByText(/has not started this yet/)).not.toBeInTheDocument();
    act(() => {
      vi.advanceTimersByTime(46_000);
    });
    expect(screen.getByText(/has not started this yet/)).toBeInTheDocument();
  });

  async function startProbe(routes: Parameters<typeof backupServer>[0]) {
    const ctx = await open(routes);
    await ctx.user.click(screen.getByRole('button', { name: 'Add a destination' }));
    const dlg = await screen.findByRole('dialog');
    await ctx.user.type(within(dlg).getByLabelText('Endpoint'), 'https://s3.example.org');
    await ctx.user.type(within(dlg).getByLabelText('Bucket'), 'bk');
    await ctx.user.click(within(dlg).getByRole('button', { name: 'Probe the server' }));
    return { ...ctx, dlg };
  }

  it('stops polling when the run has ended', async () => {
    pollConfig.ms = 20;
    let reads = 0;
    const { dlg } = await startProbe({
      routes: {
        [`POST ${P}/destinations/probe`]: () => ({
          status: 202,
          json: run({ id: 'p1', trigger: 'probe', status: 'queued', steps: [] }),
        }),
        [`GET ${P}/runs/:id`]: () => {
          reads++;
          return {
            json: run({
              id: 'p1',
              trigger: 'probe',
              status: reads < 3 ? 'queued' : 'succeeded',
              steps:
                reads < 3
                  ? []
                  : [{ name: 'probe', ok: true, kind: 'tls', fingerprint: FP, chainVerified: true }],
            }),
          };
        },
      },
    });
    await within(dlg).findByTestId('probe-trusted');
    const n = reads;
    await sleep(250);
    expect(reads).toBe(n);
  });

  it('stops after five failed reads in a row and says so', async () => {
    pollConfig.ms = 10;
    let reads = 0;
    const { dlg } = await startProbe({
      routes: {
        [`POST ${P}/destinations/probe`]: () => ({
          status: 202,
          json: run({ id: 'p1', trigger: 'probe', status: 'queued', steps: [] }),
        }),
        [`GET ${P}/runs/:id`]: () => {
          reads++;
          return err(500, 'internal');
        },
      },
    });
    await within(dlg).findByText(/stopped asking for the result/);
    await sleep(200);
    expect(reads).toBe(5);
    expect(document.body.textContent).not.toContain(MARKER);
  });
});

describe('the runs filter', () => {
  it('goes back to all destinations when the chosen one is deleted', async () => {
    const d1 = destination();
    const d2 = destination({ id: 'dest2', name: 'Second' });
    const s = backupServer({
      state: { destinations: [d1, d2] },
      routes: {
        [`DELETE ${P}/destinations/:id`]: () => ({ status: 204 }),
        [`GET ${P}/runs`]: (r) => ({
          json: {
            items: [
              run({ destinationName: r.url.searchParams.get('destination') ? 'Filtered row' : 'All row' }),
            ],
            nextCursor: null,
          },
        }),
      },
    });
    s.st.destinations = [d1, d2];
    const user = userEvent.setup();
    renderApp(URL);
    await screen.findByRole('list', { name: 'Destinations' });
    s.api.calls.length = 0;
    await user.selectOptions(await screen.findByLabelText('Destination'), 'dest1');
    expect(screen.getByLabelText('Destination')).toHaveValue('dest1');
    // The destination vanishes from the server; the list reads again after the delete.
    const del = screen.getByRole('button', { name: 'Delete Offsite S3' });
    s.st.destinations = [d2];
    await user.click(del);
    await user.click(
      within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete destination' }),
    );
    await waitFor(() => expect(screen.getByLabelText('Destination')).toHaveValue(''));
    expect(await screen.findByText('All row')).toBeInTheDocument();
    expect(screen.queryByText('Filtered row')).not.toBeInTheDocument();
  });
});

// LAUN-146: small review follow-ups.
describe('follow-ups (LAUN-146)', () => {
  const sftpStatus = () => status({ supportedTypes: ['sftp'] });

  async function openSftp() {
    const ctx = await open({ state: { status: sftpStatus() } });
    await ctx.user.click(screen.getByRole('button', { name: 'Add a destination' }));
    const dlg = await screen.findByRole('dialog');
    return { ...ctx, dlg };
  }
  const fileBox = (dlg: HTMLElement) =>
    within(dlg).getByLabelText('Or load the key from a file. The page does not show it.');

  it('1. a key file over 64 KiB is refused with a fixed sentence and is not read', async () => {
    const read = vi.spyOn(FileReader.prototype, 'readAsText');
    const { user, dlg } = await openSftp();
    const big = new File(['k'.repeat(64 * 1024 + 1)], 'id_big');
    await user.upload(fileBox(dlg), big);
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('too big for a key');
    expect(within(dlg).getByLabelText('Private key')).toHaveValue('');
    expect(read).not.toHaveBeenCalled();
    read.mockRestore();
  });

  it('1. a key file of exactly 64 KiB loads and the box stays masked', async () => {
    const { user, dlg } = await openSftp();
    const ok = new File(['k'.repeat(64 * 1024)], 'id_ok');
    await user.upload(fileBox(dlg), ok);
    await waitFor(() => expect(within(dlg).getByLabelText('Private key')).toHaveValue('k'.repeat(64 * 1024)));
    expect(within(dlg).queryByRole('alert')).not.toBeInTheDocument();
    expect(within(dlg).getByLabelText('Private key').className).toMatch(/masked/);
    expect(within(dlg).getByLabelText('Private key').tagName).toBe('TEXTAREA');
  });

  it('1. a failed read shows a fixed sentence and loads nothing', async () => {
    const spy = vi.spyOn(FileReader.prototype, 'readAsText').mockImplementation(function (this: FileReader) {
      queueMicrotask(() => this.onerror?.(new ProgressEvent('error') as ProgressEvent<FileReader>));
    });
    const { user, dlg } = await openSftp();
    await user.upload(fileBox(dlg), new File(['small'], 'id_small'));
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('could not be read');
    expect(within(dlg).getByLabelText('Private key')).toHaveValue('');
    spy.mockRestore();
  });

  it('2. the enable sentence does not count the things', () => {
    const text = backupErrorText(new ApiError(409, 'conflict', 'x'), 'enable');
    expect(text).not.toMatch(/four|five|\b[0-9]\b things/i);
    expect(text).toContain('Check these things.');
  });

  it('3. a disabled Enable button points at the gate hint; an enabled one does not', async () => {
    const { user } = await open();
    const enable = screen.getByRole('button', { name: 'Enable Offsite S3' });
    const hint = screen.getByTestId('enable-gate');
    expect(hint.id).not.toBe('');
    expect(enable).toHaveAttribute('aria-describedby', hint.id);
    await user.click(screen.getByRole('checkbox', { name: /I have saved the private key/ }));
    expect(enable).not.toHaveAttribute('aria-describedby');
  });

  it('4. Cancel the add and Cancel the removal send one request for two clicks', async () => {
    const pending = recipient({
      fingerprint: 'b'.repeat(64),
      state: 'pending',
      effectiveAt: '2099-01-02T00:00:00Z',
    });
    const removing = recipient({
      fingerprint: 'c'.repeat(64),
      state: 'removing',
      removeAt: '2099-01-02T00:00:00Z',
      removeBy: 'u2',
    });
    const d = deferred();
    const { api, user } = await open({
      state: { recipients: [recipient(), pending, removing] },
      routes: {
        [`POST ${P}/recipients/:fp/cancel`]: async () => {
          await d.gate;
          return { json: { items: [recipient()], everHadRecipient: true, limit: 5 } };
        },
      },
    });
    const add = screen.getByRole('button', { name: `Cancel the add of key ${'b'.repeat(12)}` });
    const rem = screen.getByRole('button', { name: `Cancel the removal of key ${'c'.repeat(12)}` });
    await user.click(add);
    await user.click(add);
    await user.click(rem);
    await user.click(rem);
    d.release();
    await waitFor(() => expect(callsTo(api, 'POST', '/cancel')).toHaveLength(2));
    expect(callsTo(api, 'POST', `/recipients/${'b'.repeat(64)}/cancel`)).toHaveLength(1);
    expect(callsTo(api, 'POST', `/recipients/${'c'.repeat(64)}/cancel`)).toHaveLength(1);
  });

  const untrusted = () =>
    run({
      id: 'p1',
      trigger: 'probe',
      destinationId: null,
      steps: [{ name: 'probe', ok: true, kind: 'tls', fingerprint: FP, chainVerified: false }],
    });

  async function openSavedProbed(extra: Record<string, unknown> = {}) {
    const r = untrusted();
    const t = run({ id: 't1', trigger: 'test', status: 'succeeded', steps: [{ name: 'connect', ok: true }] });
    const ctx = await open({
      state: { runById: { p1: r, t1: t } },
      routes: {
        [`POST ${P}/destinations/:id/probe`]: () => ({ status: 202, json: r }),
        [`POST ${P}/destinations/:id/test`]: () => ({ status: 202, json: t }),
        [`POST ${P}/destinations/test`]: () => ({ status: 202, json: t }),
        ...extra,
      },
    });
    await ctx.user.click(screen.getByRole('button', { name: 'Edit Offsite S3' }));
    const dlg = await screen.findByRole('dialog');
    await ctx.user.click(within(dlg).getByRole('button', { name: 'Probe the server' }));
    await within(dlg).findByTestId('probe-fingerprint');
    return { ...ctx, dlg };
  }
  const tick = (dlg: HTMLElement) =>
    within(dlg).getByRole('checkbox', { name: /I compared this fingerprint/ });

  it('5. a first pin with one retyped secret asks for the other saved secret and sends nothing', async () => {
    const { api, user, dlg } = await openSavedProbed();
    await user.type(within(dlg).getByLabelText('Access key'), 'AK2');
    await user.click(tick(dlg));
    await user.click(within(dlg).getByRole('button', { name: 'Test the connection' }));
    expect(await within(dlg).findByText(/Enter all of them again/)).toBeInTheDocument();
    expect(callsTo(api, 'POST', '/destinations/test')).toHaveLength(0);
    expect(callsTo(api, 'POST', '/destinations/dest1/test')).toHaveLength(0);
    expect(document.body.textContent).not.toContain('AK2');
  });

  it('5. a first pin with every stored secret retyped goes to the unsaved test with the pin', async () => {
    const { api, user, dlg } = await openSavedProbed();
    await user.type(within(dlg).getByLabelText('Access key'), 'AK2');
    await user.type(within(dlg).getByLabelText('Secret key'), 'SK2');
    await user.click(tick(dlg));
    await user.click(within(dlg).getByRole('button', { name: 'Test the connection' }));
    await within(dlg).findByText('It worked.');
    const c = callsTo(api, 'POST', '/destinations/test');
    expect(c).toHaveLength(1);
    const body = c[0]!.body as Record<string, unknown>;
    expect(body.secrets).toEqual({ accessKey: 'AK2', secretKey: 'SK2' });
    expect(body.confirmedFingerprint).toBe(FP);
    expect((body.config as Record<string, unknown>).tlsPin).toBe(FP);
    expect(body).not.toHaveProperty('useSavedSecretsFrom');
  });

  it('6. Notices saved. does not come back when an edit is undone', async () => {
    const { user } = await open();
    const form = screen.getByRole('form', { name: 'Backup notices' });
    const cb = within(form).getByRole('checkbox', { name: 'Mail when a backup succeeds' });
    await user.click(cb);
    await user.click(within(form).getByRole('button', { name: 'Save notices' }));
    expect(await within(form).findByText('Notices saved.')).toBeInTheDocument();
    await user.click(cb);
    expect(within(form).queryByText('Notices saved.')).not.toBeInTheDocument();
    await user.click(cb);
    expect(within(form).queryByText('Notices saved.')).not.toBeInTheDocument();
    const hours = within(form).getByLabelText('Stale alert after (hours)');
    await user.type(hours, '1');
    await user.type(hours, '{Backspace}');
    expect(within(form).queryByText('Notices saved.')).not.toBeInTheDocument();
  });

  it('7. a hand-edited pin drops the fingerprint tick; the probe fingerprint restores it', async () => {
    const { user, dlg } = await openSavedProbed();
    await user.click(tick(dlg));
    const test = within(dlg).getByRole('button', { name: 'Test the connection' });
    expect(test).toBeEnabled();
    const pin = within(dlg).getByLabelText('TLS pin');
    await user.type(pin, 'deadbeef');
    expect(tick(dlg)).not.toBeChecked();
    expect(test).toBeDisabled();
    expect(within(dlg).getByTestId('pin-differs')).toBeInTheDocument();
    await user.clear(pin);
    expect(tick(dlg)).toBeChecked();
    await user.type(pin, FP);
    expect(tick(dlg)).toBeChecked();
    expect(within(dlg).queryByTestId('pin-differs')).not.toBeInTheDocument();
  });

  it('8. an edited secret clears the old Test result but keeps the probe', async () => {
    const { user, dlg } = await openSavedProbed();
    await user.click(tick(dlg));
    await user.click(within(dlg).getByRole('button', { name: 'Test the connection' }));
    await within(dlg).findByText('It worked.');
    await user.type(within(dlg).getByLabelText('Access key'), 'x');
    expect(within(dlg).queryByText('It worked.')).not.toBeInTheDocument();
    expect(within(dlg).getByTestId('probe-fingerprint')).toBeInTheDocument();
  });

  it('1. a good file after a bad one clears the file error', async () => {
    const { user, dlg } = await openSftp();
    await user.upload(fileBox(dlg), new File(['k'.repeat(64 * 1024 + 1)], 'big'));
    expect(await within(dlg).findByRole('alert')).toBeInTheDocument();
    await user.upload(fileBox(dlg), new File(['good'], 'good'));
    await waitFor(() => expect(within(dlg).getByLabelText('Private key')).toHaveValue('good'));
    expect(within(dlg).queryByRole('alert')).not.toBeInTheDocument();
  });

  it('4. a cancel button in flight is aria-disabled (not disabled) and keeps focus', async () => {
    const pending = recipient({
      fingerprint: 'b'.repeat(64),
      state: 'pending',
      effectiveAt: '2099-01-02T00:00:00Z',
    });
    const d = deferred();
    const { user } = await open({
      state: { recipients: [recipient(), pending] },
      routes: {
        [`POST ${P}/recipients/:fp/cancel`]: async () => {
          await d.gate;
          return { json: { items: [recipient()], everHadRecipient: true, limit: 5 } };
        },
      },
    });
    const add = screen.getByRole('button', { name: `Cancel the add of key ${'b'.repeat(12)}` });
    add.focus();
    await user.click(add);
    expect(add).toHaveAttribute('aria-disabled', 'true');
    expect(add).not.toBeDisabled();
    expect(add).toHaveFocus();
    d.release();
    expect(await screen.findByTestId('recipient-note')).toHaveTextContent('is cancelled');
  });

  it('6. Notices saved. hides after editing only the hours', async () => {
    const { user } = await open();
    const form = screen.getByRole('form', { name: 'Backup notices' });
    const hours = within(form).getByLabelText('Stale alert after (hours)');
    await user.clear(hours);
    await user.type(hours, '10');
    await user.click(within(form).getByRole('button', { name: 'Save notices' }));
    expect(await within(form).findByText('Notices saved.')).toBeInTheDocument();
    await user.type(hours, '1');
    await user.type(hours, '{Backspace}');
    expect(within(form).queryByText('Notices saved.')).not.toBeInTheDocument();
  });

  it('7. a pin with a trailing space still counts as the probed fingerprint; the hint is one status', async () => {
    const { user, dlg } = await openSavedProbed();
    await user.click(tick(dlg));
    await user.type(within(dlg).getByLabelText('TLS pin'), `${FP} `);
    expect(tick(dlg)).toBeChecked();
    expect(within(dlg).queryByTestId('pin-differs')).not.toBeInTheDocument();
    await user.type(within(dlg).getByLabelText('TLS pin'), 'z');
    expect(within(dlg).getAllByTestId('pin-differs')).toHaveLength(1);
    expect(within(dlg).getByTestId('pin-differs')).toHaveAttribute('role', 'status');
  });

  it('8. a test error disappears when a secret is edited', async () => {
    const { user, dlg } = await openSavedProbed({
      [`POST ${P}/destinations/:id/test`]: () => err(503, 'backup_unavailable'),
    });
    await user.click(tick(dlg));
    await user.click(within(dlg).getByRole('button', { name: 'Test the connection' }));
    expect(
      await within(dlg).findByText(/cannot do that\. It has no BACKUP_ENCRYPTION_KEY/),
    ).toBeInTheDocument();
    await user.type(within(dlg).getByLabelText('Access key'), 'x');
    expect(
      within(dlg).queryByText(/cannot do that\. It has no BACKUP_ENCRYPTION_KEY/),
    ).not.toBeInTheDocument();
  });

  it('8. a Test response that lands after a secret edit is not shown', async () => {
    const d = deferred();
    const t = run({ id: 't1', trigger: 'test', status: 'succeeded', steps: [{ name: 'connect', ok: true }] });
    const { user, dlg } = await openSavedProbed({
      [`POST ${P}/destinations/:id/test`]: async () => {
        await d.gate;
        return { status: 202, json: t };
      },
    });
    await user.click(tick(dlg));
    await user.click(within(dlg).getByRole('button', { name: 'Test the connection' }));
    await user.type(within(dlg).getByLabelText('Access key'), 'x');
    d.release();
    await waitFor(() => expect(within(dlg).getByLabelText('TLS pin')).toHaveValue(FP)); // the response was handled
    await expect(within(dlg).findByText('It worked.', {}, { timeout: 400 })).rejects.toThrow();
  });
});
