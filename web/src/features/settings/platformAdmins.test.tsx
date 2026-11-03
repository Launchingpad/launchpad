import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { callsTo, joinServer, MARKER, page, platformSettings } from '../../test/join';
import type { Handler } from '../../test/mockApi';
import { renderApp } from '../../test/render';

// Settings, Platform: the platform admins and the sign-up text (decisions-T12 #23 and #24).

const adm = (id: string, over: Record<string, unknown> = {}) => ({
  id,
  username: id,
  name: `Name ${id}`,
  email: `${id}@example.com`,
  deactivated: false,
  ...over,
});

function server(items: unknown[], routes: Record<string, Handler> = {}) {
  let list = items;
  return joinServer({
    platform: true,
    routes: {
      'GET /api/v1/platform/settings': () => ({ json: platformSettings() }),
      'GET /api/v1/platform/admins': () => ({ json: page(list) }),
      'POST /api/v1/platform/admins': (r) => {
        const u = (r.body as { user: string }).user;
        list = [...list, adm(u)];
        return { status: 201, json: adm(u) };
      },
      'DELETE /api/v1/platform/admins/:user': (r) => {
        list = list.filter((a) => (a as { id: string }).id !== r.params.user);
        return { status: 204 };
      },
      ...routes,
    },
  });
}

describe('platform page: sign-up text', () => {
  it('describes the three modes and links to the pages; no "does not use it yet"', async () => {
    server([adm('chen')]);
    renderApp('/orbit/settings/platform');
    await screen.findByRole('group', { name: 'Sign-up mode' });
    const group = screen.getByRole('group', { name: 'Sign-up mode' });
    expect(group).toHaveTextContent('Invite only: an admin invites each person');
    expect(group).toHaveTextContent('Approval: invites work');
    expect(group).toHaveTextContent('Closed: invites and sign-up requests are both off.');
    expect(within(group).getByRole('link', { name: 'Members page' })).toHaveAttribute(
      'href',
      '/orbit/settings/members',
    );
    expect(within(group).getByRole('link', { name: 'sign-up page' })).toHaveAttribute('href', '/signup');
    expect(within(group).getByRole('link', { name: 'Sign-ups page' })).toHaveAttribute(
      'href',
      '/orbit/settings/signups',
    );
    expect(document.body.textContent).not.toContain('does not use it yet');
    expect(document.body.textContent).not.toContain('ORB-1234');
  });
});

describe('platform admins', () => {
  it('lists the admins with the deactivated tag', async () => {
    server([adm('chen'), adm('ann', { deactivated: true, email: null })]);
    renderApp('/orbit/settings/platform');
    const table = await screen.findByRole('table', { name: 'Platform admins' });
    const rows = within(table).getAllByRole('row').slice(1);
    expect(rows).toHaveLength(2);
    expect(within(rows[0]!).getByText('chen@example.com')).toBeInTheDocument();
    expect(within(rows[1]!).getByText('Deactivated')).toBeInTheDocument();
    expect(within(rows[1]!).getByText('—')).toBeInTheDocument();
  });

  it('is not shown to a person who is not a platform admin', async () => {
    const api = joinServer({ platform: false });
    renderApp('/orbit/settings/platform');
    expect(
      await screen.findByRole('heading', { name: 'You need platform admin access.' }),
    ).toBeInTheDocument();
    expect(callsTo(api, 'GET', '/platform/admins')).toHaveLength(0);
  });

  it('adds by username or email, refetches, and says the person must sign in again', async () => {
    const api = server([adm('chen')]);
    const user = userEvent.setup();
    renderApp('/orbit/settings/platform');
    await user.type(await screen.findByLabelText('Username or email'), '  ann ');
    await user.click(screen.getByRole('button', { name: 'Add platform admin' }));
    await waitFor(() =>
      expect(screen.getByTestId('padmin-note')).toHaveTextContent('Name ann is a platform admin.'),
    );
    expect(screen.getByTestId('padmin-note')).toHaveTextContent('must sign in again');
    expect(callsTo(api, 'POST', '/platform/admins')[0]!.body).toEqual({ user: 'ann' });
    expect(await screen.findByText('ann@example.com')).toBeInTheDocument();
    expect(screen.getByLabelText('Username or email')).toHaveValue('');
  });

  it('sends nothing for an empty box', async () => {
    const api = server([adm('chen')]);
    const user = userEvent.setup();
    renderApp('/orbit/settings/platform');
    await user.click(await screen.findByRole('button', { name: 'Add platform admin' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Enter a username or an email address.');
    expect(callsTo(api, 'POST', '/platform/admins')).toHaveLength(0);
  });

  it.each([
    [404, 'No user has this username or email.'],
    [400, 'This account cannot be a platform admin. Use an active person, not an agent.'],
    [403, 'You need platform admin access to do this. Sign out and in again.'],
    [500, 'Could not add the platform admin. Try again.'],
  ])('maps an add failure %i to fixed text', async (status, text) => {
    server([adm('chen')], {
      'POST /api/v1/platform/admins': () => ({ status, json: { error: MARKER, code: 'x' } }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/platform');
    await user.type(await screen.findByLabelText('Username or email'), 'ann');
    await user.click(screen.getByRole('button', { name: 'Add platform admin' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(text);
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('removes one after a confirm', async () => {
    const api = server([adm('chen'), adm('ann')]);
    const user = userEvent.setup();
    renderApp('/orbit/settings/platform');
    await user.click(await screen.findByRole('button', { name: 'Remove Name ann as platform admin' }));
    const dialog = await screen.findByRole('dialog', { name: 'Remove this platform admin?' });
    expect(callsTo(api, 'DELETE', '/platform/admins/ann')).toHaveLength(0);
    await user.click(within(dialog).getByRole('button', { name: 'Remove platform admin' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(callsTo(api, 'DELETE', '/platform/admins/ann')).toHaveLength(1);
    expect(screen.getByTestId('padmin-note')).toHaveTextContent('Name ann is no longer a platform admin.');
    await waitFor(() => expect(screen.queryByText('ann@example.com')).not.toBeInTheDocument());
  });

  it('a platform admin who removes themselves sees a clear notice, not "You do not have access"', async () => {
    const api = server([adm('u1'), adm('ann')]);
    const user = userEvent.setup();
    renderApp('/orbit/settings/platform');
    await user.click(await screen.findByRole('button', { name: 'Remove Name u1 as platform admin' }));
    const dialog = await screen.findByRole('dialog', { name: 'Remove this platform admin?' });
    await user.click(within(dialog).getByRole('button', { name: 'Remove platform admin' }));
    expect(
      await screen.findByRole('heading', { name: 'You are no longer a platform admin.' }),
    ).toBeInTheDocument();
    expect(callsTo(api, 'DELETE', '/platform/admins/u1')).toHaveLength(1);
    expect(document.body.textContent).not.toContain('You do not have access');
  });

  it('removing another admin keeps the page', async () => {
    server([adm('u1'), adm('ann')]);
    const user = userEvent.setup();
    renderApp('/orbit/settings/platform');
    await user.click(await screen.findByRole('button', { name: 'Remove Name ann as platform admin' }));
    const dialog = await screen.findByRole('dialog', { name: 'Remove this platform admin?' });
    await user.click(within(dialog).getByRole('button', { name: 'Remove platform admin' }));
    await waitFor(() => expect(screen.queryByText('ann@example.com')).not.toBeInTheDocument());
    expect(
      screen.queryByRole('heading', { name: 'You are no longer a platform admin.' }),
    ).not.toBeInTheDocument();
  });

  it('shows the 409 for the last admin as fixed text and keeps the person in the list', async () => {
    server([adm('chen')], {
      'DELETE /api/v1/platform/admins/:user': () => ({
        status: 409,
        json: { error: MARKER, code: 'conflict' },
      }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/platform');
    await user.click(await screen.findByRole('button', { name: 'Remove Name chen as platform admin' }));
    const dialog = await screen.findByRole('dialog', { name: 'Remove this platform admin?' });
    await user.click(within(dialog).getByRole('button', { name: 'Remove platform admin' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(
      'You cannot remove the last platform admin. Add another platform admin first.',
    );
    expect(document.body.textContent).not.toContain(MARKER);
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }));
    expect(screen.getByText('chen@example.com')).toBeInTheDocument();
  });

  it('shows fixed text and a retry when the list fails to load', async () => {
    server([], {
      'GET /api/v1/platform/admins': () => ({ status: 500, json: { error: MARKER, code: 'internal' } }),
    });
    renderApp('/orbit/settings/platform');
    expect(await screen.findByText('Could not load the platform admins')).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
  });
});
