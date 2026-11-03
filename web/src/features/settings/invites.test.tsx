import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { callsTo, invite, joinServer, MARKER, page, platformSettings, webStorage } from '../../test/join';
import { renderApp } from '../../test/render';

// Settings, Members, invites (decisions-T12 #16 to #22).

const SECRET = 'AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-planted'; // the invite token: must never leak
const LINK = `https://launchpad.example.com/invite#${SECRET}`;
const unavailable = { status: 403, json: { error: MARKER, code: 'signup_unavailable' } };

const listRoute = (items: unknown[]) => ({ 'GET /api/v1/orgs/:org/invites': () => ({ json: page(items) }) });
const createdMail = (over = {}) => ({
  status: 201,
  json: { invite: invite('new', { email: 'ann@example.com' }), emailed: true, ...over },
});
const createdLink = (url = LINK) => ({
  status: 201,
  json: {
    invite: invite('new', { email: 'ann@example.com', emailed: false }),
    emailed: false,
    secret: SECRET,
    url,
  },
});

async function invitePerson(user: ReturnType<typeof userEvent.setup>, email = 'ann@example.com') {
  await user.type(await screen.findByLabelText('Email'), email);
  await user.click(screen.getByRole('button', { name: 'Send invite' }));
}

describe('invites: mode', () => {
  it('a platform admin sees a plain notice and no form when the mode is closed; the list stays', async () => {
    joinServer({
      platform: true,
      routes: {
        ...listRoute([invite('a')]),
        'GET /api/v1/platform/settings': () => ({ json: platformSettings({ signupMode: 'closed' }) }),
        'GET /api/v1/platform/admins': () => ({ json: page([]) }),
      },
    });
    renderApp('/orbit/settings/members');
    expect(await screen.findByTestId('invites-closed')).toHaveTextContent(
      'Invites are off while the sign-up mode is Closed.',
    );
    expect(screen.getByRole('link', { name: 'Platform page' })).toHaveAttribute(
      'href',
      '/orbit/settings/platform',
    );
    expect(screen.queryByRole('button', { name: 'Send invite' })).not.toBeInTheDocument();
    expect(await screen.findByText('a@example.com')).toBeInTheDocument(); // revoke still works in every mode
  });

  it.each(['invite', 'approval'])('shows the form in %s mode', async (signupMode) => {
    joinServer({
      platform: true,
      routes: {
        ...listRoute([]),
        'GET /api/v1/platform/settings': () => ({ json: platformSettings({ signupMode }) }),
        'GET /api/v1/platform/admins': () => ({ json: page([]) }),
      },
    });
    renderApp('/orbit/settings/members');
    expect(await screen.findByRole('button', { name: 'Send invite' })).toBeInTheDocument();
    expect(screen.queryByTestId('invites-closed')).not.toBeInTheDocument();
  });

  it('an org admin who is not a platform admin never reads the platform settings; a 403 closes the form', async () => {
    const api = joinServer({
      routes: { ...listRoute([]), 'POST /api/v1/orgs/:org/invites': () => unavailable },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/members');
    await invitePerson(user);
    expect(await screen.findByTestId('invites-closed')).toBeInTheDocument();
    expect(screen.getByTestId('invites-closed')).toHaveTextContent('A platform admin can change it.');
    expect(callsTo(api, 'GET', '/platform/settings')).toHaveLength(0);
    expect(document.body.textContent).not.toContain(MARKER);
  });
});

describe('invites: create', () => {
  it('when the server emailed the link, says so, shows no link and keeps no secret', async () => {
    const api = joinServer({
      routes: { ...listRoute([]), 'POST /api/v1/orgs/:org/invites': () => createdMail() },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/members');
    await invitePerson(user, '  ann@example.com ');
    expect(await screen.findByTestId('invite-note')).toHaveTextContent(
      'The invite is sent by email to ann@example.com.',
    );
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(callsTo(api, 'POST', '/invites')[0]!.body).toEqual({
      email: 'ann@example.com',
      role: 'member',
      teamAccess: 'all',
    });
    expect(screen.getByLabelText('Email')).toHaveValue('');
  });

  it('sends the admin role when chosen', async () => {
    const api = joinServer({
      routes: { ...listRoute([]), 'POST /api/v1/orgs/:org/invites': () => createdMail() },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/members');
    await user.selectOptions(await screen.findByLabelText('Role'), 'admin');
    await invitePerson(user);
    await waitFor(() => expect(callsTo(api, 'POST', '/invites')).toHaveLength(1));
    expect(callsTo(api, 'POST', '/invites')[0]!.body).toEqual({ email: 'ann@example.com', role: 'admin' });
  });

  it('checks the email before it sends anything', async () => {
    const api = joinServer({ routes: listRoute([]) });
    const user = userEvent.setup();
    renderApp('/orbit/settings/members');
    await invitePerson(user, 'not-an-email');
    expect(await screen.findByRole('alert')).toHaveTextContent('Enter a valid email address.');
    expect(screen.getByLabelText('Email')).toHaveAttribute('aria-invalid', 'true');
    expect(screen.getByLabelText('Email')).toHaveFocus();
    expect(callsTo(api, 'POST', '/invites')).toHaveLength(0);
  });

  describe('copy once (mail is off)', () => {
    async function open(url?: string) {
      const api = joinServer({
        routes: {
          ...listRoute([]),
          'POST /api/v1/orgs/:org/invites': () => (url ? createdLink(url) : createdLink()),
        },
      });
      const user = userEvent.setup();
      const { client } = renderApp('/orbit/settings/members');
      await invitePerson(user);
      const dialog = await screen.findByRole('dialog', { name: 'Copy the invite link' });
      return { api, user, dialog, client };
    }

    it('warns, copies, and needs the confirm box before Done', async () => {
      const { user, dialog } = await open();
      expect(within(dialog).getByRole('alert')).toHaveTextContent(
        'Copy this link now. It is not shown again.',
      );
      expect(within(dialog).getByTestId('invite-link')).toHaveTextContent(LINK);
      expect(within(dialog).getByRole('button', { name: 'Copy link' })).toHaveFocus();
      const done = within(dialog).getByRole('button', { name: 'Done' });
      expect(done).toBeDisabled();
      await user.click(within(dialog).getByRole('button', { name: 'Copy link' }));
      expect(await navigator.clipboard.readText()).toBe(LINK);
      expect(within(dialog).getByText('Copied.')).toBeInTheDocument();
      await user.click(within(dialog).getByLabelText('I copied the link.'));
      expect(done).toBeEnabled();
      await user.click(done);
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
      expect(document.body.textContent).not.toContain(SECRET);
    });

    it('cannot be closed by Escape or the close button before the box is ticked', async () => {
      const { user, dialog } = await open();
      await user.keyboard('{Escape}');
      expect(screen.getByRole('dialog', { name: 'Copy the invite link' })).toBeInTheDocument();
      expect(within(dialog).getByText(/Tick the box when you copied the link/)).toBeInTheDocument();
      await user.click(within(dialog).getByRole('button', { name: 'Close' }));
      expect(screen.getByRole('dialog', { name: 'Copy the invite link' })).toBeInTheDocument();
      await user.click(within(dialog).getByLabelText('I copied the link.'));
      await user.keyboard('{Escape}');
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    });

    it('asks the browser to confirm before the page closes, only while the link is on screen', async () => {
      const { user, dialog } = await open();
      const during = new Event('beforeunload', { cancelable: true });
      window.dispatchEvent(during);
      expect(during.defaultPrevented).toBe(true);
      await user.click(within(dialog).getByLabelText('I copied the link.'));
      await user.click(within(dialog).getByRole('button', { name: 'Done' }));
      const after = new Event('beforeunload', { cancelable: true });
      window.dispatchEvent(after);
      expect(after.defaultPrevented).toBe(false);
    });

    it('keeps the secret out of storage, the URL, every cache and every later request', async () => {
      const { api, user, dialog, client } = await open();
      expect(webStorage()).not.toContain(SECRET);
      expect(window.location.href).not.toContain(SECRET);
      expect(client.getMutationCache().getAll()).toHaveLength(0);
      expect(
        JSON.stringify(
          client
            .getQueryCache()
            .getAll()
            .map((q) => q.state.data),
        ),
      ).not.toContain(SECRET);
      await user.click(within(dialog).getByLabelText('I copied the link.'));
      await user.click(within(dialog).getByRole('button', { name: 'Done' }));
      const later = api.calls.slice(api.calls.indexOf(callsTo(api, 'POST', '/invites')[0]!) + 1);
      expect(JSON.stringify(later.map((c) => [c.url.href, c.body, [...c.headers.entries()]]))).not.toContain(
        SECRET,
      );
      // The list that reloads after the create does not hold it either.
      expect(
        JSON.stringify(
          client
            .getQueryCache()
            .getAll()
            .map((q) => q.state.data),
        ),
      ).not.toContain(SECRET);
    });

    it('adds this address in front of a bare path (no PUBLIC_BASE_URL on the server)', async () => {
      const { dialog } = await open(`/invite#${SECRET}`);
      expect(within(dialog).getByTestId('invite-link')).toHaveTextContent(
        `${window.location.origin}/invite#${SECRET}`,
      );
    });
  });

  it.each([
    [409, 'that email cannot be invited', 'That email cannot be invited.'],
    [
      409,
      'an open invite for this email exists already; revoke it first',
      'This email has an open invite. Revoke it first.',
    ],
    [409, 'a member of this org has that email already', 'This person is a member already.'],
    [403, MARKER, 'You need admin access to invite a person.'],
    [400, MARKER, 'Enter a valid email address.'],
    [500, MARKER, 'Could not create the invite. Try again.'],
  ])('maps a %i (%s) to fixed text', async (status, error, text) => {
    joinServer({
      routes: {
        ...listRoute([]),
        'POST /api/v1/orgs/:org/invites': () => ({
          status,
          json: { error, code: status === 409 ? 'conflict' : 'x' },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/members');
    await invitePerson(user);
    expect(await screen.findByRole('alert')).toHaveTextContent(text);
    expect(document.body.textContent).not.toContain(MARKER);
    expect(document.body.textContent).not.toContain('revoke it first.');
  });

  it('shows the wait on a 429', async () => {
    joinServer({
      routes: {
        ...listRoute([]),
        'POST /api/v1/orgs/:org/invites': () => ({
          status: 429,
          json: { error: MARKER, code: 'rate_limited' },
          headers: { 'Retry-After': '30' },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/members');
    await invitePerson(user);
    expect(await screen.findByRole('alert')).toHaveTextContent('Too many requests. Try again in 30 seconds.');
  });
});

describe('invites: list and revoke', () => {
  const rows = [
    invite('p1', { emailed: true }),
    invite('p2', { emailed: false, role: 'admin' }),
    invite('acc', { status: 'accepted' }),
    invite('exp', { status: 'expired' }),
    invite('rev', { status: 'revoked' }),
  ];

  it('lists status and how the link went; hides revoked; only a pending invite has Revoke', async () => {
    joinServer({ routes: listRoute(rows) });
    renderApp('/orbit/settings/members');
    const table = await screen.findByRole('table', { name: 'Invites' });
    const body = within(table).getAllByRole('row').slice(1);
    expect(body).toHaveLength(4);
    expect(within(body[0]!).getByText('Sent by email')).toBeInTheDocument();
    expect(within(body[0]!).getByText('3 hours ago')).toBeInTheDocument();
    expect(within(body[1]!).getByText('Link shown once')).toBeInTheDocument();
    expect(within(body[1]!).getByText('Admin')).toBeInTheDocument();
    expect(within(body[2]!).getByText('Accepted')).toBeInTheDocument();
    expect(within(body[3]!).getByText('Expired')).toBeInTheDocument();
    expect(screen.queryByText('rev@example.com')).not.toBeInTheDocument();
    expect(screen.getAllByRole('button', { name: /^Revoke the invite for/ })).toHaveLength(2);
  });

  it('revokes after a confirm, then refetches', async () => {
    let list = [invite('p1')];
    const api = joinServer({
      routes: {
        'GET /api/v1/orgs/:org/invites': () => ({ json: page(list) }),
        'DELETE /api/v1/orgs/:org/invites/:invite': () => {
          list = [invite('p1', { status: 'revoked' })];
          return { status: 204 };
        },
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/members');
    await user.click(await screen.findByRole('button', { name: 'Revoke the invite for p1@example.com' }));
    const dialog = await screen.findByRole('dialog', { name: 'Revoke this invite?' });
    expect(callsTo(api, 'DELETE', '/invites/p1')).toHaveLength(0);
    await user.click(within(dialog).getByRole('button', { name: 'Revoke invite' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(callsTo(api, 'DELETE', '/invites/p1')).toHaveLength(1);
    expect(screen.getByTestId('invite-note')).toHaveTextContent('The invite for p1@example.com is revoked.');
    await waitFor(() => expect(screen.queryByText('p1@example.com')).not.toBeInTheDocument());
  });

  it('keeps the dialog and shows fixed text when the revoke fails', async () => {
    joinServer({
      routes: {
        ...listRoute([invite('p1')]),
        'DELETE /api/v1/orgs/:org/invites/:invite': () => ({
          status: 409,
          json: { error: MARKER, code: 'conflict' },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/members');
    await user.click(await screen.findByRole('button', { name: 'Revoke the invite for p1@example.com' }));
    const dialog = await screen.findByRole('dialog', { name: 'Revoke this invite?' });
    await user.click(within(dialog).getByRole('button', { name: 'Revoke invite' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(
      'This invite was accepted already. It cannot be revoked.',
    );
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('shows an empty line and a retry on a load error', async () => {
    joinServer({ routes: listRoute([]) });
    const { unmount } = renderApp('/orbit/settings/members');
    expect(await screen.findByText('No invites yet.')).toBeInTheDocument();
    unmount();
    joinServer({
      routes: {
        'GET /api/v1/orgs/:org/invites': () => ({ status: 500, json: { error: MARKER, code: 'internal' } }),
      },
    });
    renderApp('/orbit/settings/members');
    expect(await screen.findByText('Could not load invites')).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('a member who opens the address sees the notice only', async () => {
    const api = joinServer({ role: 'member' });
    renderApp('/orbit/settings/members');
    expect(await screen.findByRole('heading', { name: 'You need admin access.' })).toHaveFocus();
    expect(callsTo(api, 'GET', '/invites')).toHaveLength(0);
  });
});
