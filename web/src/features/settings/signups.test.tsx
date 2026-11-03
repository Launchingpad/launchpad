import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { callsTo, joinServer, MARKER, page, platformSettings, signupReq } from '../../test/join';
import { renderApp } from '../../test/render';
import { members } from '../../test/mockApi';

/** Open the approval dialog of a row and confirm it with the default access (All teams). */
async function approveRow(user: ReturnType<typeof userEvent.setup>, name: string) {
  await user.click(await screen.findByRole('button', { name: `Approve ${name}` }));
  const dialog = await screen.findByRole('dialog', { name: `Approve ${name}?` });
  const ok = within(dialog).getByRole('button', { name: 'Approve request' });
  await waitFor(() => expect(ok).toBeEnabled()); // the team list is read first
  await user.click(ok);
}

// Settings, Sign-ups (decisions-T12 #5 to #15).

const list = (items: unknown[]) => ({ 'GET /api/v1/orgs/:org/signups': () => ({ json: page(items) }) });
const err = (status: number, error: string, code: string, headers?: Record<string, string>) => ({
  status,
  json: { error, code },
  headers,
});

describe('sign-ups page', () => {
  it('is in the nav for an org admin, and a member sees the notice only', async () => {
    joinServer({ routes: list([]) });
    const { unmount } = renderApp('/orbit/settings/signups');
    const nav = await screen.findByRole('navigation', { name: 'Settings' });
    expect(within(nav).getByRole('link', { name: 'Sign-ups' })).toHaveAttribute(
      'href',
      '/orbit/settings/signups',
    );
    unmount();
    const api = joinServer({ role: 'member' });
    renderApp('/orbit/settings/signups');
    expect(await screen.findByRole('heading', { name: 'You need admin access.' })).toHaveFocus();
    expect(callsTo(api, 'GET', '/signups')).toHaveLength(0);
  });

  it('lists name, username, email, note, age and the unverified flag', async () => {
    joinServer({
      routes: list([
        signupReq('ann', {
          note: 'I work on iOS',
          createdAt: new Date(Date.now() - 2 * 86400_000).toISOString(),
        }),
        signupReq('bob', { emailVerified: false, emailVerifiedAt: null }),
      ]),
    });
    renderApp('/orbit/settings/signups');
    expect(await screen.findByRole('heading', { name: 'Sign-ups' })).toHaveFocus();
    const table = await screen.findByRole('table', { name: 'Sign-up requests' });
    const rows = within(table).getAllByRole('row').slice(1);
    expect(rows).toHaveLength(2);
    const ann = within(rows[0]!);
    expect(ann.getByText('Name ann')).toBeInTheDocument();
    expect(ann.getByText('ann')).toBeInTheDocument();
    expect(ann.getByText('ann@example.com')).toBeInTheDocument();
    expect(ann.getByText('I work on iOS')).toBeInTheDocument();
    expect(ann.getByText('2 days ago')).toBeInTheDocument();
    expect(ann.queryByText('Email unverified')).not.toBeInTheDocument();
    expect(within(rows[1]!).getByText('Email unverified')).toBeInTheDocument();
    expect(screen.getByText(/means the server could not send a code/)).toBeInTheDocument();
  });

  it('asks only for pending requests', async () => {
    const api = joinServer({ routes: list([]) });
    renderApp('/orbit/settings/signups');
    await screen.findByText('No requests to decide');
    expect(callsTo(api, 'GET', '/signups')[0]!.url.searchParams.get('status')).toBe('pending');
  });

  it('explains the unconfirmed requests in plain text', async () => {
    joinServer({ routes: list([]) });
    renderApp('/orbit/settings/signups');
    expect(await screen.findByText(/must first enter a code from the email/)).toBeInTheDocument();
    expect(screen.getByText(/Unconfirmed requests wait for the email code/)).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'sign-up page' })).toHaveAttribute('href', '/signup');
  });

  it('a platform admin sees the mode notice when the mode is not approval', async () => {
    joinServer({
      platform: true,
      routes: {
        ...list([]),
        'GET /api/v1/platform/settings': () => ({ json: platformSettings({ signupMode: 'invite' }) }),
        'GET /api/v1/platform/admins': () => ({ json: page([]) }),
      },
    });
    renderApp('/orbit/settings/signups');
    expect(await screen.findByTestId('signups-not-approval')).toHaveTextContent(
      'cannot approve requests now',
    );
  });

  describe('approve', () => {
    it('approves with All teams by default, refetches, and says so', async () => {
      let items = [signupReq('ann')];
      const api = joinServer({
        routes: {
          'GET /api/v1/orgs/:org/signups': () => ({ json: page(items) }),
          'POST /api/v1/orgs/:org/signups/:signup/approve': () => {
            items = [];
            return { json: signupReq('ann', { status: 'approved', userId: 'u-ann' }) };
          },
          // The note reads the real access from the member list (LAUN-228).
          'GET /api/v1/orgs/:org/users': () => ({
            json: page([{ ...members[1]!, id: 'u-ann', isAgent: false, teamAccess: 'all', teams: [] }]),
          }),
        },
      });
      const user = userEvent.setup();
      renderApp('/orbit/settings/signups');
      await approveRow(user, 'Name ann');
      await waitFor(() =>
        expect(screen.getByTestId('signups-note')).toHaveTextContent(
          'Name ann is approved with access to all teams.',
        ),
      );
      const post = callsTo(api, 'POST', '/signups/ann/approve');
      expect(post).toHaveLength(1);
      expect(post[0]!.body).toEqual({ teamAccess: 'all' });
      expect(await screen.findByText('No requests to decide')).toBeInTheDocument();
    });

    it.each([
      [
        409,
        'the email of this sign-up request is not confirmed yet; it can be approved after the person enters the code from the email',
        'The person did not confirm the email address yet. Wait for the code, or reject the request.',
      ],
      [
        409,
        'a user with username "ann" exists already; reject the request and ask the person to send it again with another username',
        'This username is taken now. Reject the request and ask the person to ask again with another username.',
      ],
      [
        409,
        'a user with this email exists already; reject the request',
        'An account with this email exists already. Reject the request.',
      ],
      [
        409,
        'the sign-up request is rejected; only a pending request can be approved',
        'This request cannot be approved now. Reload the list.',
      ],
      [403, MARKER, 'You need admin access to approve a request.'],
      [404, MARKER, 'This request is gone already. Reload the list.'],
      [500, MARKER, 'Could not approve the request. Try again.'],
    ])('maps %i "%s" to fixed text', async (status, error, text) => {
      joinServer({
        routes: {
          ...list([signupReq('ann')]),
          'POST /api/v1/orgs/:org/signups/:signup/approve': () => err(status, error, 'conflict'),
        },
      });
      const user = userEvent.setup();
      renderApp('/orbit/settings/signups');
      await approveRow(user, 'Name ann');
      expect(await screen.findByRole('alert')).toHaveTextContent(text);
      expect(document.body.textContent).not.toContain(MARKER);
      expect(document.body.textContent).not.toContain('exists already;');
      // The request stays in the list.
      expect(screen.getByText('Name ann')).toBeInTheDocument();
    });

    it('a 429 shows the wait; a 403 signup_unavailable shows the mode notice', async () => {
      let n = 0;
      joinServer({
        routes: {
          ...list([signupReq('ann')]),
          'POST /api/v1/orgs/:org/signups/:signup/approve': () =>
            n++ === 0
              ? err(429, MARKER, 'rate_limited', { 'Retry-After': '120' })
              : err(403, MARKER, 'signup_unavailable'),
        },
      });
      const user = userEvent.setup();
      renderApp('/orbit/settings/signups');
      await approveRow(user, 'Name ann');
      expect(await screen.findByRole('alert')).toHaveTextContent(
        'Too many requests. Try again in 2 minutes.',
      );
      await approveRow(user, 'Name ann');
      await waitFor(() =>
        expect(screen.getByRole('alert')).toHaveTextContent(
          'Approval is off. A platform admin must set the sign-up mode to Approval',
        ),
      );
      expect(screen.getByTestId('signups-not-approval')).toBeInTheDocument();
      expect(document.body.textContent).not.toContain(MARKER);
    });
  });

  describe('reject', () => {
    it('sends no reason and block false by default', async () => {
      const api = joinServer({
        routes: {
          ...list([signupReq('ann')]),
          'POST /api/v1/orgs/:org/signups/:signup/reject': () => ({
            json: signupReq('ann', { status: 'rejected' }),
          }),
        },
      });
      const user = userEvent.setup();
      renderApp('/orbit/settings/signups');
      await user.click(await screen.findByRole('button', { name: 'Reject Name ann' }));
      const dialog = await screen.findByRole('dialog', { name: 'Reject Name ann?' });
      await user.click(within(dialog).getByRole('button', { name: 'Reject request' }));
      await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
      expect(callsTo(api, 'POST', '/signups/ann/reject')[0]!.body).toEqual({ block: false });
      expect(screen.getByTestId('signups-note')).toHaveTextContent('The request of Name ann is rejected.');
    });

    it('sends the trimmed reason and the block choice', async () => {
      const api = joinServer({
        routes: {
          ...list([signupReq('ann')]),
          'POST /api/v1/orgs/:org/signups/:signup/reject': () => ({
            json: signupReq('ann', { status: 'rejected' }),
          }),
        },
      });
      const user = userEvent.setup();
      renderApp('/orbit/settings/signups');
      await user.click(await screen.findByRole('button', { name: 'Reject Name ann' }));
      const dialog = await screen.findByRole('dialog', { name: 'Reject Name ann?' });
      await user.type(within(dialog).getByLabelText('Reason (optional)'), '  Not someone we know ');
      await user.click(within(dialog).getByLabelText(/Block this email address/));
      await user.click(within(dialog).getByRole('button', { name: 'Reject request' }));
      await waitFor(() => expect(callsTo(api, 'POST', '/signups/ann/reject')).toHaveLength(1));
      expect(callsTo(api, 'POST', '/signups/ann/reject')[0]!.body).toEqual({
        reason: 'Not someone we know',
        block: true,
      });
      await waitFor(() =>
        expect(screen.getByTestId('signups-note')).toHaveTextContent('The email address is blocked.'),
      );
    });

    it('Cancel sends nothing; a failure keeps the dialog with fixed text', async () => {
      const api = joinServer({
        routes: {
          ...list([signupReq('ann')]),
          'POST /api/v1/orgs/:org/signups/:signup/reject': () => err(409, MARKER, 'conflict'),
        },
      });
      const user = userEvent.setup();
      renderApp('/orbit/settings/signups');
      await user.click(await screen.findByRole('button', { name: 'Reject Name ann' }));
      let dialog = await screen.findByRole('dialog', { name: 'Reject Name ann?' });
      await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
      expect(callsTo(api, 'POST', '/reject')).toHaveLength(0);
      await user.click(screen.getByRole('button', { name: 'Reject Name ann' }));
      dialog = await screen.findByRole('dialog', { name: 'Reject Name ann?' });
      await user.click(within(dialog).getByRole('button', { name: 'Reject request' }));
      expect(await within(dialog).findByRole('alert')).toHaveTextContent(
        'This request cannot be rejected now. Reload the list.',
      );
      expect(document.body.textContent).not.toContain(MARKER);
    });
  });

  describe('remove unconfirmed requests', () => {
    it('asks first, then shows the count', async () => {
      const api = joinServer({
        routes: {
          ...list([]),
          'DELETE /api/v1/orgs/:org/signups/unconfirmed': () => ({ json: { deleted: 42 } }),
        },
      });
      const user = userEvent.setup();
      renderApp('/orbit/settings/signups');
      await user.click(await screen.findByRole('button', { name: 'Remove unconfirmed requests' }));
      const dialog = await screen.findByRole('dialog', { name: 'Remove unconfirmed requests?' });
      expect(callsTo(api, 'DELETE', '/signups/unconfirmed')).toHaveLength(0);
      await user.click(within(dialog).getByRole('button', { name: 'Remove requests' }));
      await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
      expect(callsTo(api, 'DELETE', '/signups/unconfirmed')).toHaveLength(1);
      expect(screen.getByTestId('signups-note')).toHaveTextContent('Removed 42 unconfirmed requests.');
    });

    it('Cancel deletes nothing; one request reads in the singular; a failure is fixed text', async () => {
      let status = 200;
      const api = joinServer({
        routes: {
          ...list([]),
          'DELETE /api/v1/orgs/:org/signups/unconfirmed': () =>
            status === 200 ? { json: { deleted: 1 } } : err(status, MARKER, 'x'),
        },
      });
      const user = userEvent.setup();
      renderApp('/orbit/settings/signups');
      await user.click(await screen.findByRole('button', { name: 'Remove unconfirmed requests' }));
      await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Cancel' }));
      expect(callsTo(api, 'DELETE', '/signups/unconfirmed')).toHaveLength(0);
      status = 500;
      await user.click(screen.getByRole('button', { name: 'Remove unconfirmed requests' }));
      const dialog = await screen.findByRole('dialog');
      await user.click(within(dialog).getByRole('button', { name: 'Remove requests' }));
      expect(await within(dialog).findByRole('alert')).toHaveTextContent(
        'Could not remove the requests. Try again.',
      );
      expect(document.body.textContent).not.toContain(MARKER);
      status = 200;
      await user.click(within(dialog).getByRole('button', { name: 'Remove requests' }));
      await waitFor(() =>
        expect(screen.getByTestId('signups-note')).toHaveTextContent('Removed 1 unconfirmed request.'),
      );
    });
  });

  it('shows fixed text and a retry on a load error', async () => {
    joinServer({
      routes: { 'GET /api/v1/orgs/:org/signups': () => err(500, MARKER, 'internal') },
    });
    renderApp('/orbit/settings/signups');
    expect(await screen.findByText('Could not load sign-up requests')).toBeInTheDocument();
    expect(screen.getByText('The server had a problem. Try again.')).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
  });
});
