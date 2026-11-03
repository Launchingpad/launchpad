import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import type { Member, Team, TeamAccessResult } from '../../api/types';
import { callsTo, joinServer, page } from '../../test/join';
import { members as baseMembers, team, type Handler, type MockRequest } from '../../test/mockApi';
import { renderApp } from '../../test/render';

// Settings, Members: team access (decisions-T20 #74, LAUN-227).

const web: Team = { ...team, id: 'team2', key: 'WEB', name: 'Web' };
const ann: Member = {
  id: 'u3',
  username: 'ann',
  name: 'Ann',
  email: 'ann@example.com',
  isAgent: false,
  role: 'member',
  createdAt: '2026-01-01T00:00:00Z',
  deactivatedAt: null,
  teamAccess: 'selected',
  teams: ['ORB'],
};
const bea: Member = {
  ...ann,
  id: 'u4',
  username: 'bea',
  name: 'Bea',
  email: 'bea@example.com',
  role: 'admin',
  teamAccess: 'all',
  teams: [],
};
const roster = [...baseMembers, ann, bea];

function result(over: Partial<TeamAccessResult> = {}): TeamAccessResult {
  return {
    userId: 'u3',
    teamAccess: 'all',
    teamIds: [],
    dryRun: true,
    changed: true,
    addedTeamIds: [],
    lostTeamIds: [],
    assignedInLostTeams: [],
    unassigned: 0,
    hiddenAssigned: 0,
    removed: { subscriptions: 0, follows: 0, settings: 0 },
    ...over,
  };
}

const isDry = (r: MockRequest) => r.url.searchParams.get('dryRun') === 'true';

function server(
  put: Handler,
  opts: { role?: 'admin' | 'member'; list?: Member[]; routes?: Record<string, Handler> } = {},
) {
  return joinServer({
    role: opts.role,
    routes: {
      'GET /api/v1/orgs/:org/teams': () => ({ json: page([team, web]) }),
      'GET /api/v1/orgs/:org/users': () => ({ json: page(opts.list ?? roster) }),
      'GET /api/v1/orgs/:org/invites': () => ({ json: page([]) }),
      'PUT /api/v1/orgs/:org/users/:user/team-access': put,
      ...opts.routes,
    },
  });
}

const puts = (api: { calls: MockRequest[] }) => callsTo(api, 'PUT', '/team-access');

async function openFor(name: string) {
  const user = userEvent.setup();
  await user.click(await screen.findByRole('button', { name: `Edit team access for ${name}` }));
  const dialog = await screen.findByRole('dialog', { name: `Team access for ${name}` });
  return { user, dialog };
}

describe('members: team access column', () => {
  it('shows each member access to an admin; admin rows have no control', async () => {
    server(() => ({ json: result() }));
    renderApp('/orbit/settings/members');
    expect(await screen.findByRole('columnheader', { name: 'Teams' })).toBeInTheDocument();
    expect(screen.getByTestId('access-chen')).toHaveTextContent('All teams');
    expect(screen.getByTestId('access-claude')).toHaveTextContent('All teams');
    expect(screen.getByTestId('access-ann')).toHaveTextContent('ORB');
    expect(screen.getByTestId('access-bea')).toHaveTextContent('All teams');
    expect(screen.getByRole('button', { name: 'Edit team access for Ann' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Edit team access for Claude' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Edit team access for Chen' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Edit team access for Bea' })).not.toBeInTheDocument();
  });

  it('a selected member with no team reads "No team"', async () => {
    server(() => ({ json: result() }), { list: [...baseMembers, { ...ann, teams: [] }] });
    renderApp('/orbit/settings/members');
    expect(await screen.findByTestId('access-ann')).toHaveTextContent('No team');
  });

  it('shows no column and no control when the server sends no access (not an admin answer)', async () => {
    server(() => ({ json: result() }), {
      list: roster.map((m) => ({ ...m, teamAccess: null, teams: null })),
    });
    renderApp('/orbit/settings/members');
    expect(await screen.findByText('ann@example.com')).toBeInTheDocument();
    expect(screen.queryByRole('columnheader', { name: 'Teams' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Edit team access/ })).not.toBeInTheDocument();
  });

  it('a member session gets no control and sends no setter call', async () => {
    const api = server(() => ({ json: result() }), { role: 'member' });
    renderApp('/orbit/settings/members');
    expect(await screen.findByRole('heading', { name: 'You need admin access.' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Edit team access/ })).not.toBeInTheDocument();
    expect(puts(api)).toHaveLength(0);
  });
});

describe('members: edit team access', () => {
  it('checks with a dry run first, shows the counts, then saves', async () => {
    const api = server((r) =>
      isDry(r)
        ? { json: result({ addedTeamIds: ['team2'] }) }
        : { json: result({ dryRun: false, addedTeamIds: ['team2'] }) },
    );
    renderApp('/orbit/settings/members');
    const { user, dialog } = await openFor('Ann');
    expect(within(dialog).getByRole('radio', { name: 'Selected teams' })).toBeChecked();
    expect(within(dialog).getByRole('checkbox', { name: 'ORB Orbit' })).toBeChecked();
    expect(within(dialog).getByRole('checkbox', { name: 'WEB Web' })).not.toBeChecked();
    expect(within(dialog).queryByRole('button', { name: 'Save access' })).not.toBeInTheDocument();

    await user.click(within(dialog).getByRole('radio', { name: 'All teams' }));
    expect(within(dialog).queryByRole('group', { name: 'Teams' })).not.toBeInTheDocument();
    await user.click(within(dialog).getByRole('button', { name: 'Check changes' }));

    const preview = await within(dialog).findByTestId('access-preview');
    expect(preview).toHaveTextContent('Teams addedWEB');
    expect(preview).toHaveTextContent('Teams removedNone');
    expect(puts(api)).toHaveLength(1);
    expect(puts(api)[0]!.url.searchParams.get('dryRun')).toBe('true');
    expect(puts(api)[0]!.params.user).toBe('u3');
    expect(puts(api)[0]!.body).toEqual({ teamAccess: 'all' });

    await user.click(within(dialog).getByRole('button', { name: 'Save access' }));
    expect(await screen.findByTestId('access-note')).toHaveTextContent('The team access of Ann is saved.');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(puts(api)).toHaveLength(2);
    expect(puts(api)[1]!.url.searchParams.has('dryRun')).toBe(false);
    expect(puts(api)[1]!.body).toEqual({ teamAccess: 'all' });
  });

  it('an edit after the check needs a new check', async () => {
    server(() => ({ json: result() }));
    renderApp('/orbit/settings/members');
    const { user, dialog } = await openFor('Ann');
    await user.click(within(dialog).getByRole('button', { name: 'Check changes' }));
    expect(await within(dialog).findByRole('button', { name: 'Save access' })).toBeInTheDocument();
    await user.click(within(dialog).getByRole('checkbox', { name: 'WEB Web' }));
    expect(within(dialog).queryByRole('button', { name: 'Save access' })).not.toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: 'Check changes' })).toBeInTheDocument();
    expect(within(dialog).queryByTestId('access-preview')).not.toBeInTheDocument();
  });

  it('says so and cannot save when nothing changes', async () => {
    server(() => ({ json: result({ changed: false, teamAccess: 'selected', teamIds: ['team1'] }) }));
    renderApp('/orbit/settings/members');
    const { user, dialog } = await openFor('Ann');
    await user.click(within(dialog).getByRole('button', { name: 'Check changes' }));
    expect(
      await within(dialog).findByText('Nothing changes. This is the access Ann has now.'),
    ).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: 'Save access' })).toBeDisabled();
  });

  it('a product shortcut ticks the teams of the product', async () => {
    const extra = server(() => ({ json: result() }), {
      routes: {
        'GET /api/v1/orgs/:org/products': () => ({
          json: page([{ id: 'p1', name: 'Apps', slug: 'apps', teamKeys: ['ORB', 'WEB'] }]),
        }),
      },
    });
    renderApp('/orbit/settings/members');
    const { user, dialog } = await openFor('Claude');
    await user.click(within(dialog).getByRole('radio', { name: 'Selected teams' }));
    expect(
      within(dialog).getByText('With no team ticked, this person sees no team and no issue.'),
    ).toBeInTheDocument();
    await user.click(await within(dialog).findByRole('button', { name: 'Select all teams of Apps' }));
    expect(within(dialog).getByRole('checkbox', { name: 'ORB Orbit' })).toBeChecked();
    expect(within(dialog).getByRole('checkbox', { name: 'WEB Web' })).toBeChecked();
    await user.click(within(dialog).getByRole('button', { name: 'Check changes' }));
    await waitFor(() => expect(puts(extra)).toHaveLength(1));
    expect(puts(extra)[0]!.body).toEqual({ teamAccess: 'selected', teams: ['ORB', 'WEB'] });
  });

  it('keeps a team of the member that the list does not show, so a save never drops it unseen', async () => {
    const api = server(() => ({ json: result() }), {
      list: [...baseMembers, { ...ann, teams: ['OLD', 'ORB'] }],
    });
    renderApp('/orbit/settings/members');
    const { user, dialog } = await openFor('Ann');
    const old = within(dialog).getByRole('checkbox', { name: 'OLD (not in the team list)' });
    expect(old).toBeChecked();
    // Unticked, it stays in the list and can be ticked again.
    await user.click(old);
    expect(within(dialog).getByRole('checkbox', { name: 'OLD (not in the team list)' })).not.toBeChecked();
    await user.click(within(dialog).getByRole('checkbox', { name: 'OLD (not in the team list)' }));
    expect(within(dialog).getByRole('checkbox', { name: 'OLD (not in the team list)' })).toBeChecked();
    await user.click(within(dialog).getByRole('button', { name: 'Check changes' }));
    await waitFor(() => expect(puts(api)).toHaveLength(1));
    expect(puts(api)[0]!.body).toEqual({ teamAccess: 'selected', teams: ['OLD', 'ORB'] });
  });
});

describe('members: assigned issues in a lost team', () => {
  const lost = result({
    userId: 'u2',
    teamAccess: 'selected',
    teamIds: ['team1'],
    lostTeamIds: ['team2', 'team9'],
    assignedInLostTeams: [{ teamId: 'team2', openIssues: 2 }],
    hiddenAssigned: 3, // 2 in the lost team WEB, 1 already hidden before
    removed: { subscriptions: 3, follows: 1, settings: 4 },
  });

  async function checkLoss(saved: TeamAccessResult = { ...lost, dryRun: false }) {
    const api = server((r) => ({ json: isDry(r) ? lost : saved }));
    renderApp('/orbit/settings/members');
    const { user, dialog } = await openFor('Claude');
    await user.click(within(dialog).getByRole('radio', { name: 'Selected teams' }));
    await user.click(within(dialog).getByRole('checkbox', { name: 'ORB Orbit' }));
    await user.click(within(dialog).getByRole('button', { name: 'Check changes' }));
    const preview = await within(dialog).findByTestId('access-preview');
    return { api, user, dialog, preview };
  }

  it('shows the counts and defaults to Unassign', async () => {
    const { api, user, dialog, preview } = await checkLoss();
    expect(puts(api)[0]!.body).toEqual({ teamAccess: 'selected', teams: ['ORB'] });
    expect(preview).toHaveTextContent('Teams removedWEB, 1 team not in the list');
    expect(preview).toHaveTextContent('Issue subscriptions removed3');
    expect(preview).toHaveTextContent('Team follows removed1');
    expect(preview).toHaveTextContent('Team notification settings removed4');
    // Unassign clears the 2 in WEB, so 1 stays hidden after the save.
    const hidden = 'After the save: open issues assigned to them in teams they cannot see';
    expect(preview).toHaveTextContent(`${hidden}1`);
    const group = within(dialog).getByRole('group', {
      name: '2 open issues assigned to Claude in the teams removed (WEB: 2)',
    });
    expect(within(group).getByRole('radio', { name: 'Unassign them (recommended)' })).toBeChecked();
    await user.click(within(dialog).getByRole('button', { name: 'Save access' }));
    await waitFor(() => expect(puts(api)).toHaveLength(2));
    expect(puts(api)[1]!.body).toEqual({
      teamAccess: 'selected',
      teams: ['ORB'],
      assignedIssues: 'unassign',
    });
  });

  it('sends keep when the admin chooses it', async () => {
    const { api, user, dialog } = await checkLoss();
    await user.click(within(dialog).getByRole('radio', { name: /Keep them assigned/ }));
    expect(within(dialog).getByTestId('access-preview')).toHaveTextContent(
      'After the save: open issues assigned to them in teams they cannot see3',
    );
    await user.click(within(dialog).getByRole('button', { name: 'Save access' }));
    await waitFor(() => expect(puts(api)).toHaveLength(2));
    expect(puts(api)[1]!.body).toEqual({ teamAccess: 'selected', teams: ['ORB'], assignedIssues: 'keep' });
  });

  it('a 409 on save shows the server text and asks for a new check', async () => {
    const text = '3 open issues assigned to this member are in teams the member would lose (WEB: 3)';
    const api = server((r) =>
      isDry(r) ? { json: lost } : { status: 409, json: { error: text, code: 'conflict' } },
    );
    renderApp('/orbit/settings/members');
    const { user, dialog } = await openFor('Claude');
    await user.click(within(dialog).getByRole('radio', { name: 'Selected teams' }));
    await user.click(within(dialog).getByRole('checkbox', { name: 'ORB Orbit' }));
    await user.click(within(dialog).getByRole('button', { name: 'Check changes' }));
    await user.click(await within(dialog).findByRole('button', { name: 'Save access' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(
      `The access was not changed: ${text}. Check the changes again.`,
    );
    expect(within(dialog).getByRole('button', { name: 'Check changes' })).toBeInTheDocument();
    expect(within(dialog).queryByTestId('access-preview')).not.toBeInTheDocument();
    expect(puts(api)).toHaveLength(2);
  });
});

describe('members: setter errors', () => {
  it.each([
    [400, 'validation_failed', 'unknown team', 'The server did not accept this access: unknown team.'],
    [403, 'forbidden', 'forbidden', 'Only an organisation admin can change team access.'],
    [
      404,
      'not_found',
      'user not found',
      'This person is no longer a member of this organisation. The member list is refreshed.',
    ],
    [
      409,
      'conflict',
      'repositories without a team: acme/app',
      'The access was not changed: repositories without a team: acme/app. Check the changes again.',
    ],
  ])('a %i on the dry run shows a plain alert', async (status, code, error, shown) => {
    server(() => ({ status, json: { error, code } }));
    renderApp('/orbit/settings/members');
    const { user, dialog } = await openFor('Ann');
    await user.click(within(dialog).getByRole('button', { name: 'Check changes' }));
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(shown);
    expect(within(dialog).queryByRole('button', { name: 'Save access' })).not.toBeInTheDocument();
  });
});

describe('members: review fixes', () => {
  it('a 404 refreshes the member list', async () => {
    const api = server(() => ({ status: 404, json: { error: 'user not found', code: 'not_found' } }));
    renderApp('/orbit/settings/members');
    const { user, dialog } = await openFor('Ann');
    const before = callsTo(api, 'GET', '/users').length;
    await user.click(within(dialog).getByRole('button', { name: 'Check changes' }));
    await within(dialog).findByRole('alert');
    await waitFor(() => expect(callsTo(api, 'GET', '/users').length).toBeGreaterThan(before));
  });

  it('a double-click on Check changes checks once and never saves', async () => {
    const api = server(() => ({ json: result({ addedTeamIds: ['team2'] }) }));
    renderApp('/orbit/settings/members');
    const { user, dialog } = await openFor('Ann');
    await user.dblClick(within(dialog).getByRole('button', { name: 'Check changes' }));
    await within(dialog).findByTestId('access-preview');
    await new Promise((r) => setTimeout(r, 20));
    expect(puts(api)).toHaveLength(1);
    expect(puts(api)[0]!.url.searchParams.get('dryRun')).toBe('true');
    expect(screen.getByRole('dialog', { name: 'Team access for Ann' })).toBeInTheDocument();
  });

  it('a click with detail 2 on Save is ignored', async () => {
    const api = server(() => ({ json: result({ addedTeamIds: ['team2'] }) }));
    renderApp('/orbit/settings/members');
    const { user, dialog } = await openFor('Ann');
    await user.click(within(dialog).getByRole('button', { name: 'Check changes' }));
    const save = await within(dialog).findByRole('button', { name: 'Save access' });
    save.dispatchEvent(new MouseEvent('click', { bubbles: true, detail: 2 }));
    await new Promise((r) => setTimeout(r, 20));
    expect(puts(api)).toHaveLength(1);
  });

  it('announces the counts and moves focus to the Changes heading', async () => {
    server(() => ({
      json: result({
        lostTeamIds: ['team2'],
        assignedInLostTeams: [{ teamId: 'team2', openIssues: 1 }],
      }),
    }));
    renderApp('/orbit/settings/members');
    const { user, dialog } = await openFor('Ann');
    expect(within(dialog).getByRole('radio', { name: 'Selected teams' })).toHaveFocus();
    await user.click(within(dialog).getByRole('button', { name: 'Check changes' }));
    expect(await within(dialog).findByRole('heading', { name: 'Changes' })).toHaveFocus();
    expect(within(dialog).getByTestId('access-announce')).toHaveTextContent(
      'Checked: 0 teams added, 1 team removed, 1 open issue assigned in the teams removed. Review, then save.',
    );
  });

  it('focuses the checked radio: All teams for an all member', async () => {
    server(() => ({ json: result() }));
    renderApp('/orbit/settings/members');
    const { dialog } = await openFor('Claude');
    await waitFor(() => expect(within(dialog).getByRole('radio', { name: 'All teams' })).toHaveFocus());
  });

  it('blocks the check while the teams fail to load, and offers a retry', async () => {
    let fail = true;
    const api = server(() => ({ json: result() }), {
      routes: {
        'GET /api/v1/orgs/:org/teams': () =>
          fail ? { status: 500, json: { error: 'boom', code: 'internal' } } : { json: page([team, web]) },
      },
    });
    renderApp('/orbit/settings/members');
    const { user, dialog } = await openFor('Ann');
    expect(await within(dialog).findByText(/Could not load the teams/)).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: 'Check changes' })).toBeDisabled();
    await user.click(within(dialog).getByRole('radio', { name: 'All teams' }));
    expect(within(dialog).getByRole('button', { name: 'Check changes' })).toBeDisabled();
    expect(puts(api)).toHaveLength(0);
    fail = false;
    await user.click(within(dialog).getByRole('button', { name: 'Try again' }));
    await waitFor(() => expect(within(dialog).getByRole('button', { name: 'Check changes' })).toBeEnabled());
  });

  it('says clearly when the member will see no team', async () => {
    server(() => ({ json: result({ teamAccess: 'selected', lostTeamIds: ['team1'] }) }));
    renderApp('/orbit/settings/members');
    const { user, dialog } = await openFor('Ann');
    await user.click(within(dialog).getByRole('checkbox', { name: 'ORB Orbit' }));
    await user.click(within(dialog).getByRole('button', { name: 'Check changes' }));
    expect(await within(dialog).findByTestId('access-preview')).toHaveTextContent(
      'Ann will see no team and no issue after the save.',
    );
    expect(within(dialog).getByTestId('access-announce')).toHaveTextContent('This person will see no team.');
  });

  it('a save that answers after Cancel does not touch the next dialog', async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => (release = r));
    server(async (r) => {
      if (isDry(r)) return { json: result({ addedTeamIds: ['team2'] }) };
      await gate;
      return { json: result({ dryRun: false, addedTeamIds: ['team2'] }) };
    });
    renderApp('/orbit/settings/members');
    const first = await openFor('Ann');
    await first.user.click(within(first.dialog).getByRole('button', { name: 'Check changes' }));
    await first.user.click(await within(first.dialog).findByRole('button', { name: 'Save access' }));
    await first.user.click(within(first.dialog).getByRole('button', { name: 'Cancel' }));
    const second = await openFor('Claude');
    release();
    await new Promise((r) => setTimeout(r, 30));
    expect(second.dialog).toBeInTheDocument();
    expect(screen.getByRole('dialog', { name: 'Team access for Claude' })).toBeInTheDocument();
    expect(screen.getByTestId('access-note')).toBeEmptyDOMElement();
  });

  it('the note gives the real result, says when it differs from the check, and the cell refreshes', async () => {
    let list = roster;
    const api = server(
      (r) => {
        if (isDry(r)) {
          return {
            json: result({
              lostTeamIds: ['team1'],
              assignedInLostTeams: [{ teamId: 'team1', openIssues: 1 }],
              removed: { subscriptions: 1, follows: 0, settings: 0 },
            }),
          };
        }
        list = roster.map((m) => (m.id === 'u3' ? { ...m, teams: ['WEB'] } : m));
        return {
          json: result({
            dryRun: false,
            lostTeamIds: ['team1'],
            unassigned: 2,
            removed: { subscriptions: 1, follows: 0, settings: 0 },
          }),
        };
      },
      { routes: { 'GET /api/v1/orgs/:org/users': () => ({ json: page(list) }) } },
    );
    renderApp('/orbit/settings/members');
    expect(await screen.findByTestId('access-ann')).toHaveTextContent('ORB');
    const { user, dialog } = await openFor('Ann');
    await user.click(within(dialog).getByRole('checkbox', { name: 'ORB Orbit' }));
    await user.click(within(dialog).getByRole('checkbox', { name: 'WEB Web' }));
    await user.click(within(dialog).getByRole('button', { name: 'Check changes' }));
    await user.click(await within(dialog).findByRole('button', { name: 'Save access' }));
    expect(await screen.findByTestId('access-note')).toHaveTextContent(
      'The team access of Ann is saved. 2 open issues unassigned. Removed: 1 issue subscription, 0 team follows, 0 team notification settings. The result differs from your check, because something changed in between.',
    );
    await waitFor(() => expect(screen.getByTestId('access-ann')).toHaveTextContent('WEB'));
    expect(puts(api)[1]!.body).toEqual({
      teamAccess: 'selected',
      teams: ['WEB'],
      assignedIssues: 'unassign',
    });
  });
});
