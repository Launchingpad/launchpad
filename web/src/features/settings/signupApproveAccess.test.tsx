import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { callsTo, joinServer, MARKER, page, signupReq } from '../../test/join';
import { members, team, type Handler } from '../../test/mockApi';
import { renderApp } from '../../test/render';

// LAUN-228 part 2 (decisions-T20 #29, #75; LAUN-226 build notes): the approval carries the team access.

const web = { ...team, id: 'team2', key: 'WEB', name: 'Web' };

/** Ann as a member with the access the FIRST approval gave (null: not approved yet). */
const annMember = (access: { teamAccess?: string; teams?: string[] } | null) =>
  access === null
    ? []
    : [
        {
          ...members[1]!,
          id: 'u-ann',
          username: 'ann',
          name: 'Name ann',
          isAgent: false,
          teamAccess: access.teamAccess ?? 'all',
          teams: access.teams ?? [],
        },
      ];

function server(routes: Record<string, Handler> = {}, role: 'admin' | 'member' = 'admin') {
  let items = [signupReq('ann')];
  // The server keeps the first approval's access (LAUN-226): a replay changes nothing.
  let first: { teamAccess?: string; teams?: string[] } | null = null;
  return joinServer({
    role,
    routes: {
      'GET /api/v1/orgs/:org/signups': () => ({ json: page(items) }),
      'GET /api/v1/orgs/:org/teams': () => ({ json: page([team, web]) }),
      'GET /api/v1/orgs/:org/users': () => ({ json: page([...members, ...annMember(first)]) }),
      'POST /api/v1/orgs/:org/signups/:signup/approve': (r) => {
        items = [];
        first ??= (r.body as typeof first) ?? {};
        return { json: signupReq('ann', { status: 'approved', decidedBy: 'u1', userId: 'u-ann' }) };
      },
      ...routes,
    },
  });
}

async function openDialog(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole('button', { name: 'Approve Name ann' }));
  return screen.findByRole('dialog', { name: 'Approve Name ann?' });
}

async function pickSelected(user: ReturnType<typeof userEvent.setup>, dialog: HTMLElement, keys: string[]) {
  await user.click(within(dialog).getByRole('radio', { name: 'Selected teams' }));
  for (const k of keys)
    await user.click(await within(dialog).findByRole('checkbox', { name: new RegExp(k) }));
}

describe('sign-up approval: team access', () => {
  it('All teams is preselected, and the page says a second approval changes nothing', async () => {
    server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/signups');
    const dialog = await openDialog(user);
    expect(within(dialog).getByRole('radio', { name: 'All teams' })).toBeChecked();
    expect(
      within(dialog).getByText(/Approving a request that is approved already changes nothing/),
    ).toBeInTheDocument();
    expect(within(dialog).getByRole('link', { name: 'Members page' })).toHaveAttribute(
      'href',
      '/orbit/settings/members',
    );
  });

  it('sends exactly the selected team keys and says what was set', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/signups');
    const dialog = await openDialog(user);
    await pickSelected(user, dialog, ['WEB', 'ORB']);
    await user.click(within(dialog).getByRole('button', { name: 'Approve request' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(callsTo(api, 'POST', '/signups/ann/approve').map((c) => c.body)).toEqual([
      { teamAccess: 'selected', teams: ['WEB', 'ORB'] },
    ]);
    expect(screen.getByTestId('signups-note')).toHaveTextContent(
      'Name ann is approved with access to these teams: WEB, ORB.',
    );
  });

  it('refuses Selected with no team before any request', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/signups');
    const dialog = await openDialog(user);
    await pickSelected(user, dialog, []);
    await within(dialog).findByRole('group', { name: 'Teams' });
    await user.click(within(dialog).getByRole('button', { name: 'Approve request' }));
    expect(
      await within(dialog).findByText('Pick at least one team, or choose All teams.'),
    ).toBeInTheDocument();
    const first = within(dialog).getByRole('checkbox', { name: /ORB/ });
    expect(first).toHaveFocus();
    expect(first).toHaveAccessibleDescription('Pick at least one team, or choose All teams.');
    expect(callsTo(api, 'POST', '/approve')).toHaveLength(0);
  });

  it('waits for the team list before it can send', async () => {
    const api = server({ 'GET /api/v1/orgs/:org/teams': () => new Promise<never>(() => {}) });
    const user = userEvent.setup();
    renderApp('/orbit/settings/signups');
    const dialog = await openDialog(user);
    const ok = within(dialog).getByRole('button', { name: 'Loading teams…' });
    expect(ok).toBeDisabled();
    await user.click(ok);
    expect(callsTo(api, 'POST', '/approve')).toHaveLength(0);
  });

  it('sends once for a double click', async () => {
    let release: (() => void) | undefined;
    const api = server({
      'POST /api/v1/orgs/:org/signups/:signup/approve': () =>
        new Promise((resolve) => {
          release = () => resolve({ json: signupReq('ann', { status: 'approved', decidedBy: 'u1' }) });
        }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/signups');
    const dialog = await openDialog(user);
    const ok = within(dialog).getByRole('button', { name: 'Approve request' });
    await waitFor(() => expect(ok).toBeEnabled());
    await user.dblClick(ok);
    await waitFor(() => expect(within(dialog).getByRole('button', { name: 'Approving…' })).toBeDisabled());
    expect(callsTo(api, 'POST', '/approve')).toHaveLength(1);
    release!();
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(callsTo(api, 'POST', '/approve')).toHaveLength(1);
  });

  it('the webhook guard (409) shows fixed text in the dialog, never the repository names', async () => {
    server({
      'POST /api/v1/orgs/:org/signups/:signup/approve': () => ({
        status: 409,
        json: {
          error: `some members see only selected teams, so every repository of an enabled integration needs a team; these have none: ${MARKER}`,
          code: 'conflict',
        },
      }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/signups');
    const dialog = await openDialog(user);
    await pickSelected(user, dialog, ['ORB']);
    await user.click(within(dialog).getByRole('button', { name: 'Approve request' }));
    expect(
      await within(dialog).findByText(/Give each repository a team in Integrations/),
    ).toBeInTheDocument();
    expect(screen.getByRole('dialog', { name: 'Approve Name ann?' })).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
    expect(screen.getByTestId('signups-note')).toBeEmptyDOMElement();
  });

  it('an unknown team (400) clears the ticks and reads the teams again', async () => {
    const api = server({
      'POST /api/v1/orgs/:org/signups/:signup/approve': () => ({
        status: 400,
        json: { error: `unknown team "WEB" ${MARKER}`, code: 'validation_failed' },
      }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/signups');
    const dialog = await openDialog(user);
    await pickSelected(user, dialog, ['WEB']);
    const before = callsTo(api, 'GET', '/teams').length;
    await user.click(within(dialog).getByRole('button', { name: 'Approve request' }));
    expect(
      await within(dialog).findByText('A team you picked is gone. Pick the teams again.'),
    ).toBeInTheDocument();
    await waitFor(() => expect(callsTo(api, 'GET', '/teams').length).toBeGreaterThan(before));
    expect(within(dialog).getByRole('checkbox', { name: /WEB/ })).not.toBeChecked();
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('more than 50 teams (400) has its own text', async () => {
    server({
      'POST /api/v1/orgs/:org/signups/:signup/approve': () => ({
        status: 400,
        json: { error: `at most 50 teams ${MARKER}`, code: 'validation_failed' },
      }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/signups');
    const dialog = await openDialog(user);
    await pickSelected(user, dialog, ['ORB']);
    await user.click(within(dialog).getByRole('button', { name: 'Approve request' }));
    expect(
      await within(dialog).findByText('Pick at most 50 teams, or choose All teams.'),
    ).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('a request another admin approved first: names the real access and says the choice was not applied', async () => {
    server({
      'GET /api/v1/orgs/:org/users': () => ({
        json: page([...members, ...annMember({ teamAccess: 'all' })]),
      }),
      'POST /api/v1/orgs/:org/signups/:signup/approve': () => ({
        json: signupReq('ann', { status: 'approved', decidedBy: 'u7', userId: 'u-ann' }),
      }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/signups');
    const dialog = await openDialog(user);
    await pickSelected(user, dialog, ['ORB']);
    await user.click(within(dialog).getByRole('button', { name: 'Approve request' }));
    await waitFor(() =>
      expect(screen.getByTestId('signups-note')).toHaveTextContent(
        'Name ann was approved already by another admin, with access to all teams. The access you chose here was not applied.',
      ),
    );
    expect(screen.getByTestId('signups-note')).not.toHaveTextContent('ORB');
  });

  it('a member cannot see the page or the control', async () => {
    const api = server({}, 'member');
    renderApp('/orbit/settings/signups');
    expect(await screen.findByRole('heading', { name: 'You need admin access.' })).toBeInTheDocument();
    expect(screen.queryByRole('group', { name: 'Team access' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Approve/ })).not.toBeInTheDocument();
    expect(callsTo(api, 'GET', '/signups')).toHaveLength(0);
  });
});

describe('sign-up approval: the note never names an access that was not set (final review)', () => {
  it('the same admin approving again (a retry, a second tab) sees the FIRST access, not the new choice', async () => {
    server({
      // Approved before by this admin with all teams; this answer is the replay.
      'GET /api/v1/orgs/:org/users': () => ({
        json: page([...members, ...annMember({ teamAccess: 'all' })]),
      }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/signups');
    const dialog = await openDialog(user);
    await pickSelected(user, dialog, ['ORB']);
    await user.click(within(dialog).getByRole('button', { name: 'Approve request' }));
    await waitFor(() =>
      expect(screen.getByTestId('signups-note')).toHaveTextContent(
        'Name ann is approved with access to all teams.',
      ),
    );
    expect(screen.getByTestId('signups-note')).not.toHaveTextContent('ORB');
  });

  it('when the member list cannot be read, the note names no access and points to the Members page', async () => {
    server({
      'GET /api/v1/orgs/:org/users': () => ({ status: 500, json: { error: MARKER, code: 'internal' } }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/signups');
    const dialog = await openDialog(user);
    await pickSelected(user, dialog, ['ORB']);
    await user.click(within(dialog).getByRole('button', { name: 'Approve request' }));
    await waitFor(() =>
      expect(screen.getByTestId('signups-note')).toHaveTextContent(
        'Name ann is approved. Check the team access on the Members page.',
      ),
    );
    expect(screen.getByTestId('signups-note')).not.toHaveTextContent('ORB');
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('two clicks in one render send one request (the busy guard, not only the disabled button)', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp('/orbit/settings/signups');
    const dialog = await openDialog(user);
    const ok = within(dialog).getByRole('button', { name: 'Approve request' });
    await waitFor(() => expect(ok).toBeEnabled());
    // Both clicks land before React renders the disabled button.
    act(() => {
      ok.click();
      ok.click();
    });
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(callsTo(api, 'POST', '/approve')).toHaveLength(1);
  });
});
