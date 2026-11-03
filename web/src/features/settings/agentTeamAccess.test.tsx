import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { callsTo, joinServer, MARKER, page } from '../../test/join';
import { members, team, type Handler } from '../../test/mockApi';
import { renderApp } from '../../test/render';

// LAUN-228 (decisions-T20 #31, #75): the agent form's team access control.

const hid = { ...team, id: 'team2', key: 'HID', name: 'Hidden work' };

function server(routes: Record<string, Handler> = {}) {
  const list = [...members];
  return joinServer({
    routes: {
      'GET /api/v1/orgs/:org/teams': () => ({ json: page([team, hid]) }),
      'GET /api/v1/orgs/:org/users': () => ({ json: page(list) }),
      'GET /api/v1/orgs/:org/tokens': () => ({ json: page([]) }),
      'POST /api/v1/orgs/:org/agents': (r) => {
        const b = r.body as { username: string; name: string; teamAccess?: string; teams?: string[] };
        const m = {
          ...members[1]!,
          id: 'u9',
          username: b.username,
          name: b.name,
          teamAccess: b.teamAccess ?? 'all',
          teams: b.teams ?? [],
        };
        list.push(m as (typeof list)[number]);
        return { status: 201, json: m };
      },
      ...routes,
    },
  });
}

async function fill(user: ReturnType<typeof userEvent.setup>) {
  await screen.findByRole('table', { name: 'Agents' });
  await user.type(screen.getByLabelText('Username'), 'ci-bot');
  await user.type(screen.getByLabelText('Name'), 'CI bot');
}

describe('agent form: team access', () => {
  it('All teams is preselected and sent; no checklist shows', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/agents');
    await fill(user);
    const group = screen.getByRole('group', { name: 'Team access' });
    expect(within(group).getByRole('radio', { name: 'All teams' })).toBeChecked();
    expect(screen.queryByRole('group', { name: 'Teams' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Create agent' }));
    await screen.findByText('Agent CI bot is created.');
    expect(callsTo(api, 'POST', '/agents')[0]!.body).toEqual({
      username: 'ci-bot',
      name: 'CI bot',
      teamAccess: 'all',
    });
  });

  it('Selected teams needs a team before it sends anything', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/agents');
    await fill(user);
    await user.click(screen.getByRole('radio', { name: 'Selected teams' }));
    const checks = await screen.findByRole('group', { name: 'Teams' });
    expect(within(checks).getAllByRole('checkbox')).toHaveLength(2);
    await user.click(screen.getByRole('button', { name: 'Create agent' }));
    expect(await screen.findByText('Pick at least one team, or choose All teams.')).toBeInTheDocument();
    expect(screen.getByRole('group', { name: 'Team access' })).toHaveAttribute(
      'aria-describedby',
      'agent-teams-error',
    );
    // Focus goes to the first team box, which names the error.
    const first = screen.getByRole('checkbox', { name: /ORB/ });
    expect(first).toHaveFocus();
    expect(first).toHaveAccessibleDescription('Pick at least one team, or choose All teams.');
    expect(first).toHaveAttribute('aria-invalid', 'true');
    expect(callsTo(api, 'POST', '/agents')).toHaveLength(0);
  });

  it('sends the ticked team keys and shows them in the table', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/agents');
    await fill(user);
    await user.click(screen.getByRole('radio', { name: 'Selected teams' }));
    await user.click(await screen.findByRole('checkbox', { name: /ORB/ }));
    await user.click(screen.getByRole('button', { name: 'Create agent' }));
    await screen.findByText('Agent CI bot is created.');
    expect(callsTo(api, 'POST', '/agents')[0]!.body).toEqual({
      username: 'ci-bot',
      name: 'CI bot',
      teamAccess: 'selected',
      teams: ['ORB'],
    });
    // The form starts again at All teams.
    expect(screen.getByRole('radio', { name: 'All teams' })).toBeChecked();
    const row = (await screen.findByRole('cell', { name: 'ci-bot' })).closest('tr')!;
    expect(within(row).getByRole('cell', { name: 'ORB' })).toBeInTheDocument();
    const claude = screen.getByRole('cell', { name: 'claude' }).closest('tr')!;
    expect(within(claude).getByRole('cell', { name: 'All teams' })).toBeInTheDocument();
  });

  it('the webhook guard refusal (409) shows fixed text at the team access, never the repositories', async () => {
    server({
      'POST /api/v1/orgs/:org/agents': () => ({
        status: 409,
        json: {
          error: `some members see only selected teams, so every repository of an enabled integration needs a team; these have none: ${MARKER}`,
          code: 'conflict',
        },
      }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/agents');
    await fill(user);
    await user.click(screen.getByRole('radio', { name: 'Selected teams' }));
    await user.click(await screen.findByRole('checkbox', { name: /ORB/ }));
    await user.click(screen.getByRole('button', { name: 'Create agent' }));
    expect(await screen.findByText(/Give each repository a team in Integrations/)).toBeInTheDocument();
    expect(screen.queryByText('This username is in use. Use another username.')).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('an unknown team (deleted meanwhile) clears the ticks and reads the teams again', async () => {
    const api = server({
      'POST /api/v1/orgs/:org/agents': () => ({
        status: 400,
        json: { error: `unknown team "HID" ${MARKER}`, code: 'validation_failed' },
      }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/agents');
    await fill(user);
    await user.click(screen.getByRole('radio', { name: 'Selected teams' }));
    await user.click(await screen.findByRole('checkbox', { name: /HID/ }));
    const before = callsTo(api, 'GET', '/teams').length;
    await user.click(screen.getByRole('button', { name: 'Create agent' }));
    expect(await screen.findByText('A team you picked is gone. Pick the teams again.')).toBeInTheDocument();
    await waitFor(() => expect(callsTo(api, 'GET', '/teams').length).toBeGreaterThan(before));
    expect(screen.getByRole('checkbox', { name: /HID/ })).not.toBeChecked();
    expect(document.body.textContent).not.toContain(MARKER);
  });
});

describe('agent form: server refusals (review fixes)', () => {
  async function submitSelected(reply: { status: number; json: unknown }, username = 'ci-bot') {
    server({ 'POST /api/v1/orgs/:org/agents': () => reply });
    const user = userEvent.setup();
    renderApp('/orbit/settings/agents');
    await screen.findByRole('table', { name: 'Agents' });
    await user.type(screen.getByLabelText('Username'), username);
    await user.type(screen.getByLabelText('Name'), 'Bot');
    await user.click(screen.getByRole('radio', { name: 'Selected teams' }));
    await user.click(await screen.findByRole('checkbox', { name: /ORB/ }));
    await user.click(screen.getByRole('button', { name: 'Create agent' }));
  }

  it('a taken username that contains "repository" is still a taken username', async () => {
    await submitSelected(
      {
        status: 409,
        json: { error: 'a user with username "repository-bot" exists already', code: 'conflict' },
      },
      'repository-bot',
    );
    expect(await screen.findByText('This username is in use. Use another username.')).toBeInTheDocument();
    expect(screen.queryByText(/Give each repository a team/)).not.toBeInTheDocument();
    expect(screen.getByLabelText('Username')).toHaveFocus();
  });

  it('the guard text with no repository names is the guard too', async () => {
    await submitSelected({
      status: 409,
      json: {
        error: `this cannot be done now: an organisation admin must first give a team to every connected repository ${MARKER}`,
        code: 'conflict',
      },
    });
    expect(await screen.findByText(/Give each repository a team in Integrations/)).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('more than 50 teams has its own text', async () => {
    await submitSelected({
      status: 400,
      json: { error: `at most 50 teams ${MARKER}`, code: 'validation_failed' },
    });
    expect(await screen.findByText('Pick at most 50 teams, or choose All teams.')).toBeInTheDocument();
    expect(screen.queryByText('A team you picked is gone. Pick the teams again.')).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('"selected needs at least one team" is the pick-a-team text, not "gone"', async () => {
    await submitSelected({
      status: 400,
      json: { error: 'teamAccess "selected" needs at least one team', code: 'validation_failed' },
    });
    expect(await screen.findByText('Pick at least one team, or choose All teams.')).toBeInTheDocument();
    expect(screen.queryByText('A team you picked is gone. Pick the teams again.')).not.toBeInTheDocument();
  });
});
