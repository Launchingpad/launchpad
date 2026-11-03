import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import type { Team } from '../../api/types';
import { callsTo, invite, joinServer, page } from '../../test/join';
import { team, type Handler } from '../../test/mockApi';
import { renderApp } from '../../test/render';

// Settings, Members, invites: team access (decisions-T20 #75, LAUN-227).

const web: Team = { ...team, id: 'team2', key: 'WEB', name: 'Web' };

function server(post: Handler, opts: { role?: 'admin' | 'member'; list?: unknown[]; teams?: Handler } = {}) {
  return joinServer({
    role: opts.role,
    routes: {
      'GET /api/v1/orgs/:org/teams': opts.teams ?? (() => ({ json: page([team, web]) })),
      'GET /api/v1/orgs/:org/invites': () => ({ json: page(opts.list ?? []) }),
      'POST /api/v1/orgs/:org/invites': post,
    },
  });
}

const created = (over: Record<string, unknown> = {}) => ({
  status: 201,
  json: { invite: invite('new', { email: 'ann@example.com', ...over }), emailed: true },
});
const posts = (api: Parameters<typeof callsTo>[0]) => callsTo(api, 'POST', '/invites');

describe('invites: team access', () => {
  it('sends the selected teams and says which teams the person will see', async () => {
    const api = server(() => created({ teamAccess: 'selected', teams: ['ORB', 'WEB'] }));
    const user = userEvent.setup();
    renderApp('/orbit/settings/members');
    await user.type(await screen.findByLabelText('Email'), 'ann@example.com');
    const access = screen.getByRole('group', { name: 'Team access' });
    expect(within(access).getByRole('radio', { name: 'All teams' })).toBeChecked();
    await user.click(within(access).getByRole('radio', { name: 'Selected teams' }));
    await user.click(await screen.findByRole('checkbox', { name: 'WEB Web' }));
    await user.click(screen.getByRole('checkbox', { name: 'ORB Orbit' }));
    await user.click(screen.getByRole('button', { name: 'Send invite' }));
    await waitFor(() => expect(posts(api)).toHaveLength(1));
    expect(posts(api)[0]!.body).toEqual({
      email: 'ann@example.com',
      role: 'member',
      teamAccess: 'selected',
      teams: ['ORB', 'WEB'],
    });
    expect(await screen.findByTestId('invite-note')).toHaveTextContent(
      'The invite is sent by email to ann@example.com. The person will see only these teams: ORB, WEB.',
    );
    // The form starts again at All teams.
    expect(within(access).getByRole('radio', { name: 'All teams' })).toBeChecked();
  });

  it('hides the control for role Admin and sends no access fields', async () => {
    const api = server(() => created({ role: 'admin' }));
    const user = userEvent.setup();
    renderApp('/orbit/settings/members');
    await user.type(await screen.findByLabelText('Email'), 'ann@example.com');
    await user.click(screen.getByRole('radio', { name: 'Selected teams' }));
    await user.selectOptions(screen.getByLabelText('Role'), 'admin');
    expect(screen.queryByRole('group', { name: 'Team access' })).not.toBeInTheDocument();
    expect(screen.queryByRole('group', { name: 'Teams' })).not.toBeInTheDocument();
    expect(screen.getByText('An admin always sees every team.')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Send invite' }));
    await waitFor(() => expect(posts(api)).toHaveLength(1));
    expect(posts(api)[0]!.body).toEqual({ email: 'ann@example.com', role: 'admin' });
  });

  it('refuses Selected teams with no team ticked, before it sends anything', async () => {
    const api = server(() => created());
    const user = userEvent.setup();
    renderApp('/orbit/settings/members');
    await user.type(await screen.findByLabelText('Email'), 'ann@example.com');
    await user.click(screen.getByRole('radio', { name: 'Selected teams' }));
    await screen.findByRole('checkbox', { name: 'ORB Orbit' });
    await user.click(screen.getByRole('button', { name: 'Send invite' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Pick at least one team, or choose All teams.',
    );
    const first = screen.getByRole('checkbox', { name: 'ORB Orbit' });
    expect(first).toHaveFocus();
    expect(first).toHaveAttribute('aria-invalid', 'true');
    expect(first).toHaveAttribute('aria-describedby', 'invite-teams-error');
    expect(posts(api)).toHaveLength(0);
    await user.click(first);
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
  });

  it('sends once when Send is pressed twice while the create runs', async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => (release = r));
    const api = server(async () => {
      await gate;
      return created();
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/members');
    await user.type(await screen.findByLabelText('Email'), 'ann@example.com');
    await user.click(screen.getByRole('button', { name: 'Send invite' }));
    await user.type(screen.getByLabelText('Email'), '{Enter}');
    release();
    expect(await screen.findByTestId('invite-note')).toHaveTextContent('The invite is sent by email');
    expect(posts(api)).toHaveLength(1);
  });

  it('a failed team list offers a retry and blocks a Selected invite', async () => {
    let fail = true;
    const api = server(() => created(), {
      teams: () =>
        fail ? { status: 500, json: { error: 'boom', code: 'internal' } } : { json: page([team]) },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/members');
    await user.type(await screen.findByLabelText('Email'), 'ann@example.com');
    await user.click(screen.getByRole('radio', { name: 'Selected teams' }));
    expect(await screen.findByText(/Could not load the teams/)).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Send invite' }));
    expect(
      await screen.findByText('Wait for the team list to load, or choose All teams.'),
    ).toBeInTheDocument();
    expect(posts(api)).toHaveLength(0);
    fail = false;
    await user.click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByRole('checkbox', { name: 'ORB Orbit' })).toBeInTheDocument();
  });

  it.each([
    [
      400,
      'validation_failed',
      'unknown team "GONE"',
      'A team you picked does not exist any more. Reload the page and pick the teams again.',
    ],
    [
      409,
      'conflict',
      'some members see only selected teams, so every repository of an enabled integration needs a team; these have none: acme/secret-repo',
      'Selected teams are not possible now: every connected repository needs a team first. Give each repository a team in the git settings, or invite with All teams.',
    ],
  ])('a %i shows a fixed text with no server detail', async (status, code, error, shown) => {
    server(() => ({ status, json: { error, code } }));
    const user = userEvent.setup();
    renderApp('/orbit/settings/members');
    await user.type(await screen.findByLabelText('Email'), 'ann@example.com');
    await user.click(screen.getByRole('radio', { name: 'Selected teams' }));
    await user.click(await screen.findByRole('checkbox', { name: 'ORB Orbit' }));
    await user.click(screen.getByRole('button', { name: 'Send invite' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(shown);
    expect(document.body.textContent).not.toContain('acme/secret-repo');
    expect(document.body.textContent).not.toContain('GONE');
  });

  it('the invites table shows each invite access', async () => {
    server(() => created(), {
      list: [
        invite('a'),
        invite('b', { teamAccess: 'selected', teams: ['ORB', 'WEB'] }),
        invite('c', { teamAccess: 'selected', teams: [] }),
        invite('d', { role: 'admin' }),
      ],
    });
    renderApp('/orbit/settings/members');
    expect(await screen.findByRole('columnheader', { name: 'Teams' })).toBeInTheDocument();
    expect(screen.getByTestId('invite-access-a')).toHaveTextContent('All teams');
    expect(screen.getByTestId('invite-access-b')).toHaveTextContent('ORB, WEB');
    expect(screen.getByTestId('invite-access-c')).toHaveTextContent('No team (its teams were deleted)');
    expect(screen.getByTestId('invite-access-d')).toHaveTextContent('All teams');
  });

  it('a member session cannot see the invites or the control', async () => {
    const api = server(() => created(), { role: 'member' });
    renderApp('/orbit/settings/members');
    expect(await screen.findByRole('heading', { name: 'You need admin access.' })).toBeInTheDocument();
    expect(screen.queryByRole('group', { name: 'Team access' })).not.toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'Invites' })).not.toBeInTheDocument();
    expect(callsTo(api, 'GET', '/invites')).toHaveLength(0);
  });
});
