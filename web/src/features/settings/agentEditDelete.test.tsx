import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { callsTo, joinServer, page } from '../../test/join';
import { members, team, type Handler } from '../../test/mockApi';
import { renderApp } from '../../test/render';

// LAUN-334 (decisions-T47): edit an agent's team access and delete an agent on Settings, Agents.

const web = { ...team, id: 'team2', key: 'WEB', name: 'Web' };
const dryResult = {
  userId: 'u2',
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
};

function server(del?: Handler) {
  let list = [...members];
  const api = joinServer({
    routes: {
      'GET /api/v1/orgs/:org/teams': () => ({ json: page([team, web]) }),
      'GET /api/v1/orgs/:org/users': () => ({ json: page(list) }),
      'GET /api/v1/orgs/:org/tokens': () => ({ json: page([]) }),
      'PUT /api/v1/orgs/:org/users/:user/team-access': (r) => ({
        json: { ...dryResult, dryRun: r.url.searchParams.get('dryRun') === 'true' },
      }),
      'DELETE /api/v1/orgs/:org/agents/:user':
        del ??
        ((r) => {
          list = list.filter((m) => m.id !== r.params.user);
          return { status: 204 };
        }),
    },
  });
  return api;
}

describe('agents page: edit access', () => {
  it('opens the team access dialog for the agent, saves and shows the note', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/agents');
    await user.click(await screen.findByRole('button', { name: 'Edit team access for Claude' }));
    const dialog = await screen.findByRole('dialog', { name: 'Team access for Claude' });
    await user.click(within(dialog).getByRole('radio', { name: 'Selected teams' }));
    await user.click(within(dialog).getByRole('checkbox', { name: 'WEB Web' }));
    await user.click(within(dialog).getByRole('button', { name: 'Check changes' }));
    await user.click(await within(dialog).findByRole('button', { name: 'Save access' }));
    expect(await screen.findByTestId('access-note')).toHaveTextContent('The team access of Claude is saved.');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    const puts = callsTo(api, 'PUT', '/team-access');
    expect(puts).toHaveLength(2);
    expect(puts[1]!.params.user).toBe('u2');
    expect(puts[1]!.body).toEqual({ teamAccess: 'selected', teams: ['WEB'] });
  });
});

describe('agents page: delete', () => {
  it('asks first, says what happens, sends DELETE and the row leaves', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/agents');
    await user.click(await screen.findByRole('button', { name: 'Delete agent Claude' }));
    const dialog = await screen.findByRole('dialog');
    expect(dialog).toHaveTextContent('tokens are removed');
    expect(dialog).toHaveTextContent('cannot be undone');
    expect(within(dialog).getByRole('button', { name: 'Cancel' })).toHaveFocus();
    expect(callsTo(api, 'DELETE', '/agents/u2')).toHaveLength(0);
    await user.click(within(dialog).getByRole('button', { name: 'Delete agent' }));
    await waitFor(() =>
      expect(screen.queryByRole('button', { name: 'Delete agent Claude' })).not.toBeInTheDocument(),
    );
    const calls = callsTo(api, 'DELETE', '/agents/u2');
    expect(calls).toHaveLength(1);
    expect(calls[0]!.params.user).toBe('u2');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
  });

  it('Cancel sends nothing and keeps the row', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/agents');
    await user.click(await screen.findByRole('button', { name: 'Delete agent Claude' }));
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Delete agent Claude' })).toBeInTheDocument();
    expect(callsTo(api, 'DELETE', '/agents/u2')).toHaveLength(0);
  });

  it.each([
    [409, 'this agent is a member of another org and cannot be deleted here'],
    [403, 'You need admin access to delete an agent.'],
    [404, 'The server cannot find this agent.'],
  ])('shows a fixed text for %i and keeps the dialog', async (status, text) => {
    const api = server(() => ({
      status,
      json: {
        error: status === 409 ? text : 'x',
        code: status === 403 ? 'forbidden' : status === 409 ? 'conflict' : 'not_found',
      },
    }));
    const user = userEvent.setup();
    renderApp('/orbit/settings/agents');
    await user.click(await screen.findByRole('button', { name: 'Delete agent Claude' }));
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete agent' }));
    expect(await screen.findByRole('alert')).toHaveTextContent(text);
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    expect(callsTo(api, 'DELETE', '/agents/u2')).toHaveLength(1);
  });
});

describe('agents page: edge cases', () => {
  it('shows the network text when the server cannot be reached', async () => {
    server(() => {
      throw new TypeError('Failed to fetch');
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/agents');
    await user.click(await screen.findByRole('button', { name: 'Delete agent Claude' }));
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete agent' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not reach the server.');
    expect(screen.getByRole('dialog')).toBeInTheDocument();
  });

  it('has no Edit access button when the server sends no team access', async () => {
    joinServer({
      routes: {
        'GET /api/v1/orgs/:org/teams': () => ({ json: page([team]) }),
        'GET /api/v1/orgs/:org/users': () => ({
          json: page([members[0]!, { ...members[1]!, teamAccess: null, teams: null }]),
        }),
        'GET /api/v1/orgs/:org/tokens': () => ({ json: page([]) }),
      },
    });
    renderApp('/orbit/settings/agents');
    await screen.findByRole('button', { name: 'Delete agent Claude' });
    expect(screen.queryByRole('button', { name: /Edit team access/ })).not.toBeInTheDocument();
  });
});
