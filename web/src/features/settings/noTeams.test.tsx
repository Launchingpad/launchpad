import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { callsTo, joinServer, MARKER, page } from '../../test/join';
import { team } from '../../test/mockApi';
import { renderApp } from '../../test/render';

// an org with no team (a new org after setup is not like that, but an org made by hand can be).

function server(role: 'admin' | 'member', routes = {}) {
  let teams: unknown[] = [];
  return joinServer({
    role,
    routes: {
      'GET /api/v1/orgs/:org/teams': () => ({ json: page(teams) }),
      'POST /api/v1/orgs/:org/teams': (r) => {
        const b = r.body as { key: string; name: string };
        teams = [{ ...team, ...b }];
        return { status: 201, json: teams[0] };
      },
      ...routes,
    },
  });
}

describe('no team yet', () => {
  it('an admin can create the first team on the issues page', async () => {
    const api = server('admin');
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    expect(await screen.findByText('This organisation has no team yet')).toBeInTheDocument();
    await user.type(screen.getByLabelText('Team key'), 'orb');
    await user.type(screen.getByLabelText('Team name'), 'Orbit');
    await user.click(screen.getByRole('button', { name: 'Create team' }));
    await waitFor(() =>
      expect(screen.queryByText('This organisation has no team yet')).not.toBeInTheDocument(),
    );
    expect(callsTo(api, 'POST', '/teams')[0]!.body).toEqual({ key: 'ORB', name: 'Orbit' });
  });

  // decisions-T20 #73: a member cannot tell an empty org from no access, so one text serves both.
  it('a member is told to ask an admin and sees no form', async () => {
    server('member');
    renderApp('/orbit/issues');
    expect(await screen.findByText('You do not have access to any team yet')).toBeInTheDocument();
    expect(screen.getByText('Ask an organisation admin.')).toBeInTheDocument();
    expect(screen.queryByText('This organisation has no team yet')).not.toBeInTheDocument();
    expect(screen.queryByLabelText('Team key')).not.toBeInTheDocument();
  });

  it('shows fixed text for a failure and never the server text', async () => {
    server('admin', {
      'POST /api/v1/orgs/:org/teams': () => ({ status: 500, json: { error: MARKER, code: 'internal' } }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await user.type(await screen.findByLabelText('Team key'), 'ORB');
    await user.type(screen.getByLabelText('Team name'), 'Orbit');
    await user.click(screen.getByRole('button', { name: 'Create team' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Could not create the team. Try again.');
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('does not show the notice when the org has a team', async () => {
    joinServer({});
    renderApp('/orbit/issues');
    expect(await screen.findByText('No issues yet')).toBeInTheDocument();
    expect(screen.queryByText('This organisation has no team yet')).not.toBeInTheDocument();
  });
});
