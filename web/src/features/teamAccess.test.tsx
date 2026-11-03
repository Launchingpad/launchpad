import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { callsTo, joinServer, page } from '../test/join';
import { FakeEventSource } from '../test/fakes';
import { makeIssue, projectOf, team, type Handler } from '../test/mockApi';
import { renderApp } from '../test/render';

// LAUN-228 (decisions-T20 #71 to #73, #76, #77): what a member who sees only some teams, or none, gets in the web.
// The server lists only the visible teams and answers 404 for a hidden row, the same as for a missing one.

const hid = { ...team, id: 'team2', key: 'HID', name: 'Hidden work' };
const project = projectOf({
  id: '01a00000-0000-7000-8000-000000000001',
  name: 'Alpha launch',
  description: '',
  content: '',
  contentTruncated: false,
  state: 'started',
  lead: null,
  startDate: null,
  targetDate: null,
  teams: [{ id: 'team1', key: 'ORB', name: 'Orbit' }],
  labels: [],
  version: 1,
  createdAt: '2026-10-01T00:00:00Z',
  updatedAt: '2026-10-01T00:00:00Z',
  startedAt: null,
  completedAt: null,
  canceledAt: null,
  archivedAt: null,
  avatarUrl: null,
  issueCounts: { total: 0, scope: 0, done: 0, started: 0, canceled: 0 },
});

function server(
  teams: () => unknown[],
  routes: Record<string, Handler> = {},
  role: 'admin' | 'member' = 'member',
) {
  return joinServer({
    role,
    routes: {
      'GET /api/v1/orgs/:org/teams': () => ({ json: page(teams()) }),
      'GET /api/v1/orgs/:org/products': () => ({ json: page([]) }),
      ...routes,
    },
  });
}

const sidebar = () => screen.getByRole('navigation', { name: 'Primary' });
/** The sidebar's create button (an empty issue list may show one more). */
const createButton = () =>
  within(screen.getByRole('complementary', { name: 'Sidebar' })).getByRole('button', {
    name: 'Create issue',
  });
const noRequestAccess = () => {
  expect(screen.queryByRole('button', { name: /request/i })).not.toBeInTheDocument();
  expect(screen.queryByRole('link', { name: /request/i })).not.toBeInTheDocument();
  expect(document.body.textContent).not.toMatch(/request access/i);
};

describe('a member who sees some teams (T20 #71)', () => {
  it('the sidebar, the palette and the create dialog list only the visible teams', async () => {
    server(() => [team]);
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByRole('navigation', { name: 'Primary' });
    expect(await within(sidebar()).findByRole('link', { name: /Orbit/ })).toBeInTheDocument();
    expect(within(sidebar()).queryByText(/Hidden work|HID/)).not.toBeInTheDocument();

    await user.click(
      within(screen.getByRole('complementary', { name: 'Sidebar' })).getByRole('button', { name: /Search/ }),
    );
    const palette = await screen.findByRole('dialog');
    expect(within(palette).getByText('Orbit (ORB)')).toBeInTheDocument();
    expect(within(palette).queryByText(/HID/)).not.toBeInTheDocument();
    await user.keyboard('{Escape}');

    await user.click(createButton());
    const dialog = await screen.findByRole('dialog', { name: 'New issue' });
    expect(within(dialog).getByText('ORB · Orbit')).toBeInTheDocument();
    expect(within(dialog).queryByText(/HID/)).not.toBeInTheDocument();
    noRequestAccess();
  });

  it('a create dialog opened on a hidden team link uses a visible team, never the hidden key', async () => {
    const api = server(() => [team], {
      'POST /api/v1/orgs/:org/issues': (r) => ({
        status: 201,
        json: { identifier: 'ORB-9', team: { key: (r.body as { team: string }).team } },
      }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/team/HID');
    expect(await screen.findByRole('heading', { name: 'Team not found' })).toBeInTheDocument();
    await user.keyboard('c');
    const dialog = await screen.findByRole('dialog', { name: 'New issue' });
    expect(await within(dialog).findByText('ORB · Orbit')).toBeInTheDocument();
    await user.type(within(dialog).getByLabelText('Title'), 'A bug{Enter}');
    await waitFor(() => expect(callsTo(api, 'POST', '/issues')).toHaveLength(1));
    expect((callsTo(api, 'POST', '/issues')[0]!.body as { team: string }).team).toBe('ORB');
  });

  it('a remembered project team filter that names a hidden team is dropped', async () => {
    localStorage.setItem('launchpad.projects.view.v1', JSON.stringify({ filters: { teams: ['HID'] } }));
    server(() => [team], { 'GET /api/v1/orgs/:org/projects': () => ({ json: page([project]) }) });
    renderApp('/orbit/projects');
    const bar = await screen.findByRole('toolbar', { name: 'Filters' });
    // The project is listed: the stale filter does not hide it, and the chip names no team.
    expect(await screen.findByRole('link', { name: /Alpha launch/ })).toBeInTheDocument();
    expect(bar.textContent).not.toContain('HID');
  });
});

describe('a hidden link is the not-found page (T20 #72)', () => {
  it.each([
    ['/orbit/issue/HID-1', 'Issue not found'],
    ['/orbit/team/HID', 'Team not found'],
    ['/orbit/team/HID/cycles', 'Team not found'],
    ['/orbit/team/HID/cycle/1', 'Cycle not found'],
    ['/orbit/projects/0194c7e2-0000-7000-8000-000000000001', 'Project not found'],
  ])('%s', async (path, title) => {
    server(() => [team], {
      'GET /api/v1/orgs/:org/projects/:project': () => ({
        status: 404,
        json: { error: 'project not found', code: 'not_found' },
      }),
    });
    renderApp(path);
    expect(await screen.findByRole('heading', { name: title })).toBeInTheDocument();
    expect(document.body.textContent).not.toContain('Hidden work');
    noRequestAccess();
  });
});

describe('a member who sees no team (T20 #73, #77)', () => {
  it('gets the no-access text, a disabled create button and no request control', async () => {
    const api = server(() => []);
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    expect(
      await screen.findByRole('heading', { name: 'You do not have access to any team yet' }),
    ).toBeInTheDocument();
    expect(screen.getByText('Ask an organisation admin.')).toBeInTheDocument();
    noRequestAccess();

    await user.click(createButton());
    const dialog = await screen.findByRole('dialog', { name: 'New issue' });
    expect(dialog.querySelector('#create-issue-no-team')).toHaveTextContent(
      'You do not have access to any team yet. Ask an organisation admin.',
    );
    const create = within(dialog).getByRole('button', { name: 'Create issue' });
    expect(create).toBeDisabled();
    expect(create).toHaveAccessibleDescription(
      'You do not have access to any team yet. Ask an organisation admin.',
    );
    await user.type(within(dialog).getByLabelText('Title'), 'Nowhere{Enter}');
    expect(callsTo(api, 'POST', '/issues')).toHaveLength(0);
    noRequestAccess();
  });

  it('an admin of an org with no team gets the org text in the dialog, not the no-access text', async () => {
    server(() => [], {}, 'admin');
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    expect(await screen.findByText('This organisation has no team yet')).toBeInTheDocument();
    await user.click(createButton());
    const dialog = await screen.findByRole('dialog', { name: 'New issue' });
    expect(dialog.querySelector('#create-issue-no-team')).toHaveTextContent(
      /This organisation has no team yet/,
    );
    expect(within(dialog).getByRole('button', { name: 'Create issue' })).toBeDisabled();
  });
});

describe('the access event (T20 #76)', () => {
  it('a team taken away leaves the sidebar without a reload; one given back returns', async () => {
    let teams: unknown[] = [team, hid];
    const api = server(() => teams);
    renderApp('/orbit/issues');
    await screen.findByRole('navigation', { name: 'Primary' });
    expect(await within(sidebar()).findByText('Hidden work')).toBeInTheDocument();
    const streams = FakeEventSource.eventStreams.length;

    teams = [team];
    FakeEventSource.lastEvents!.emit('access', {});
    await waitFor(() => expect(within(sidebar()).queryByText('Hidden work')).not.toBeInTheDocument());
    expect(within(sidebar()).getByText('Orbit')).toBeInTheDocument();
    // The session (the role) is read again too.
    expect(callsTo(api, 'GET', '/auth/session').length).toBeGreaterThan(1);

    teams = [team, hid];
    FakeEventSource.lastEvents!.emit('access', {});
    expect(await within(sidebar()).findByText('Hidden work')).toBeInTheDocument();
    // The stream was not reopened: an access event cannot start a reconnect loop.
    expect(FakeEventSource.eventStreams).toHaveLength(streams);
  });

  it('the last team taken away shows the no-access text on the open page', async () => {
    let teams: unknown[] = [team];
    server(() => teams);
    renderApp('/orbit/issues');
    expect(await screen.findByText('No issues yet')).toBeInTheDocument();
    teams = [];
    FakeEventSource.lastEvents!.emit('access', {});
    expect(
      await screen.findByRole('heading', { name: 'You do not have access to any team yet' }),
    ).toBeInTheDocument();
  });

  it('the inbox stream refetches the count on access', async () => {
    let n = 0;
    const api = server(() => [team], {
      'GET /api/v1/orgs/:org/notifications/unread-count': () => ({ json: { count: n++ } }),
    });
    renderApp('/orbit/issues');
    await screen.findByText('No issues yet');
    await waitFor(() => expect(FakeEventSource.inboxStreams.length).toBeGreaterThan(0));
    const before = callsTo(api, 'GET', '/unread-count').length;
    FakeEventSource.inboxStreams.at(-1)!.emit('access', {});
    await waitFor(() => expect(callsTo(api, 'GET', '/unread-count').length).toBeGreaterThan(before));
  });
});

// Review fixes (LAUN-228, second commit).

const secret = makeIssue(1, {
  id: 'h1',
  identifier: 'HID-1',
  title: 'Secret merger plan',
  team: { id: 'team2', key: 'HID', name: 'Hidden work' },
});

/** HID-1 is readable until `hide()`; then the team list and the issue answer as for a member without HID. */
function hidingServer(extra: Record<string, Handler> = {}) {
  let hidden = false;
  const api = server(() => (hidden ? [team] : [team, hid]), {
    'GET /api/v1/orgs/:org/issues': () => ({ json: page(hidden ? [] : [secret]) }),
    'GET /api/v1/orgs/:org/issues/:issue': (r) =>
      !hidden && r.params.issue === 'HID-1'
        ? { json: secret }
        : { status: 404, json: { error: 'issue not found', code: 'not_found' } },
    ...extra,
  });
  return {
    api,
    /** Take HID away; with `access` the stream says so, as the server does. */
    hide: (access = true) => {
      hidden = true;
      if (access) FakeEventSource.lastEvents!.emit('access', {});
    },
  };
}

describe('an open page of a team that becomes hidden (T20 #72)', () => {
  it('an issue opened from the list (with its placeholder) becomes Issue not found, and its title goes', async () => {
    const { hide } = hidingServer();
    const user = userEvent.setup();
    renderApp('/orbit/team/HID');
    await user.click(await screen.findByRole('link', { name: /Secret merger plan/ }));
    expect((await screen.findAllByText(/Secret merger plan/)).length).toBeGreaterThan(0);
    await waitFor(() => expect(document.title).toContain('Secret merger plan'));

    hide();
    expect(await screen.findByRole('heading', { name: 'Issue not found' })).toBeInTheDocument();
    await waitFor(() => expect(document.title).toMatch(/Not found$/));
    expect(screen.queryByText('This issue was deleted')).not.toBeInTheDocument();
    for (const text of ['Secret merger plan', 'HID-1', 'Hidden work'])
      expect(document.body.textContent).not.toContain(text);
    expect(document.title).not.toContain('Secret');
  });

  it('a 404 for a cached issue whose team is gone is Not found even without a reset (feed events)', async () => {
    const { hide } = hidingServer();
    renderApp('/orbit/issue/HID-1');
    expect((await screen.findAllByText(/Secret merger plan/)).length).toBeGreaterThan(0);
    hide(false);
    // Plain feed events refetch the teams and the issue but keep the cached copy (no reset).
    FakeEventSource.lastEvents!.emit('team.updated', { seq: 50, type: 'team.updated' });
    FakeEventSource.lastEvents!.emit('issue.updated', { seq: 51, type: 'issue.updated' });
    expect(await screen.findByRole('heading', { name: 'Issue not found' })).toBeInTheDocument();
    expect(screen.queryByText('This issue was deleted')).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain('Secret merger plan');
    await waitFor(() => expect(document.title).toMatch(/Not found$/));
  });

  it('a real delete of a visible issue still says it was deleted', async () => {
    let gone = false;
    const mine = makeIssue(3, { title: 'Visible work' });
    server(() => [team], {
      'GET /api/v1/orgs/:org/issues/:issue': () =>
        gone ? { status: 404, json: { error: 'issue not found', code: 'not_found' } } : { json: mine },
    });
    renderApp('/orbit/issue/ORB-3');
    expect((await screen.findAllByText(/Visible work/)).length).toBeGreaterThan(0);
    gone = true;
    FakeEventSource.lastEvents!.emit('issue.deleted', { seq: 60, type: 'issue.deleted' });
    expect(await screen.findByRole('heading', { name: 'This issue was deleted' })).toBeInTheDocument();
  });

  it('a sub-issue dialog of a hidden issue moves to a visible team and drops the parent', async () => {
    const { api, hide } = hidingServer({
      'POST /api/v1/orgs/:org/issues': (r) => ({
        status: 201,
        json: {
          ...makeIssue(9),
          team: { id: 'team1', key: (r.body as { team: string }).team, name: 'Orbit' },
        },
      }),
    });
    const user = userEvent.setup();
    renderApp('/orbit/issue/HID-1');
    await user.click(await screen.findByRole('button', { name: /Add sub-issues/ }));
    const dialog = await screen.findByRole('dialog', { name: 'New issue' });
    expect(within(dialog).getByRole('button', { name: 'More options: parent HID-1' })).toBeInTheDocument();

    hide();
    expect(await within(dialog).findByText('ORB · Orbit')).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: 'More options' })).toBeInTheDocument();
    expect(dialog.innerHTML).not.toContain('HID-1');
    await user.type(within(dialog).getByLabelText('Title'), 'Child{Enter}');
    await waitFor(() => expect(callsTo(api, 'POST', '/issues')).toHaveLength(1));
    const body = callsTo(api, 'POST', '/issues')[0]!.body as { team: string; parent?: string };
    expect(body.team).toBe('ORB');
    expect(body.parent).toBeUndefined();
  });
});

describe('the remembered project team filter before the teams are known (T20 #71)', () => {
  it.each([
    ['while the teams load', () => new Promise<never>(() => {})],
    ['when the teams read fails', () => ({ status: 500, json: { error: 'boom', code: 'internal' } })],
  ])('%s, the stored key is not shown or used', async (_name, teamsReply) => {
    localStorage.setItem('launchpad.projects.view.v1', JSON.stringify({ filters: { teams: ['HID'] } }));
    joinServer({
      role: 'member',
      routes: {
        'GET /api/v1/orgs/:org/teams': teamsReply as Handler,
        'GET /api/v1/orgs/:org/products': () => ({ json: page([]) }),
        'GET /api/v1/orgs/:org/projects': () => ({ json: page([project]) }),
      },
    });
    renderApp('/orbit/projects');
    expect(await screen.findByRole('link', { name: /Alpha launch/ })).toBeInTheDocument();
    expect(screen.getByRole('toolbar', { name: 'Filters' }).textContent).not.toContain('HID');
  });
});

describe('the create dialog with no team (T20 #73)', () => {
  it('announces the no-access text in its live region', async () => {
    server(() => []);
    const user = userEvent.setup();
    renderApp('/orbit/issues');
    await screen.findByRole('heading', { name: 'You do not have access to any team yet' });
    await user.click(createButton());
    const dialog = await screen.findByRole('dialog', { name: 'New issue' });
    const live = within(dialog)
      .getAllByRole('status')
      .find((el) => el.getAttribute('aria-live') === 'polite');
    await waitFor(() =>
      expect(live).toHaveTextContent('You do not have access to any team yet. Ask an organisation admin.'),
    );
  });
});

// Final review (LAUN-228): input on a page whose data stays valid survives a reconnect and an access event that
// does not take this page's team. Every refetch here is DELAYED (150 ms): an instant mock would hide a reset, since
// the data would be back before the page could unmount.

const slow = <T,>(v: T, ms = 150) => new Promise<T>((r) => setTimeout(() => r(v), ms));

function draftServer(teams: () => unknown[]) {
  const visible = makeIssue(1, { title: 'Visible work' });
  return server(teams, {
    'GET /api/v1/orgs/:org/teams': () => slow({ json: page(teams()) }),
    'GET /api/v1/orgs/:org/issues/:issue': (r) =>
      r.params.issue === 'ORB-1'
        ? slow({ json: visible })
        : slow({ status: 404, json: { error: 'issue not found', code: 'not_found' } }),
    'GET /api/v1/orgs/:org/issues': () => slow({ json: page([visible]) }),
  });
}

async function typeDraft(user: ReturnType<typeof userEvent.setup>) {
  const box = await screen.findByLabelText('Leave a comment');
  await user.type(box, 'half a thought');
  return box;
}

describe('input survives a refresh that keeps its data valid', () => {
  it('a comment draft survives a reconnect after an error', async () => {
    const teams = [team, hid];
    const api = draftServer(() => teams);
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    const box = await typeDraft(user);
    const before = callsTo(api, 'GET', '/teams').length;
    const streams = FakeEventSource.eventStreams.length;
    FakeEventSource.lastEvents!.fail();
    await waitFor(() => expect(FakeEventSource.eventStreams.length).toBe(streams + 1), { timeout: 3000 });
    FakeEventSource.lastEvents!.open();
    // The team list is read again; nothing changed, so nothing else is reset.
    await waitFor(() => expect(callsTo(api, 'GET', '/teams').length).toBeGreaterThan(before));
    await new Promise((r) => setTimeout(r, 400));
    expect(screen.queryByText('Loading issue')).not.toBeInTheDocument();
    expect(screen.getByLabelText('Leave a comment')).toBe(box);
    expect(box).toHaveValue('half a thought');
  });

  it('a comment draft survives an access event that does not take this team', async () => {
    let teams = [team, hid];
    draftServer(() => teams);
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    const box = await typeDraft(user);
    teams = [team]; // HID goes; this issue is ORB-1
    FakeEventSource.lastEvents!.emit('access', {});
    await waitFor(() => expect(within(sidebar()).queryByText('Hidden work')).not.toBeInTheDocument());
    await new Promise((r) => setTimeout(r, 400));
    expect(screen.queryByText('Loading issue')).not.toBeInTheDocument();
    expect(screen.getByLabelText('Leave a comment')).toBe(box);
    expect(box).toHaveValue('half a thought');
  });

  it('with delayed refetches, an open issue of a lost team is Not found and its title goes', async () => {
    let hidden = false;
    server(() => (hidden ? [team] : [team, hid]), {
      'GET /api/v1/orgs/:org/teams': () => slow({ json: page(hidden ? [team] : [team, hid]) }),
      'GET /api/v1/orgs/:org/issues/:issue': () =>
        hidden
          ? slow({ status: 404, json: { error: 'issue not found', code: 'not_found' } })
          : slow({ json: secret }),
    });
    renderApp('/orbit/issue/HID-1');
    expect((await screen.findAllByText(/Secret merger plan/)).length).toBeGreaterThan(0);
    hidden = true;
    FakeEventSource.lastEvents!.emit('access', {});
    expect(await screen.findByRole('heading', { name: 'Issue not found' })).toBeInTheDocument();
    await waitFor(() => expect(document.title).toMatch(/Not found$/));
    expect(document.body.textContent).not.toContain('Secret merger plan');
    expect(document.title).not.toContain('Secret');
  });
});

describe('a loss found by another read of the team list (final review)', () => {
  it('a team event refetches the list: the lost team leaves the sidebar, a draft on a visible issue stays', async () => {
    let teams = [team, hid];
    draftServer(() => teams);
    const user = userEvent.setup();
    renderApp('/orbit/issue/ORB-1');
    const box = await typeDraft(user);
    await within(sidebar()).findByText('Hidden work');
    teams = [team];
    // Not an access event: a team event (or a window focus) reads the team list again.
    FakeEventSource.lastEvents!.emit('team.updated', { seq: 70, type: 'team.updated' });
    await waitFor(() => expect(within(sidebar()).queryByText('Hidden work')).not.toBeInTheDocument());
    await new Promise((r) => setTimeout(r, 400));
    expect(screen.getByLabelText('Leave a comment')).toBe(box);
    expect(box).toHaveValue('half a thought');
  });

  it('a page entry that carries a copy of a hidden issue (Back, a reload) never shows that copy', async () => {
    server(() => [team], {
      'GET /api/v1/orgs/:org/teams': () => slow({ json: page([team]) }),
      'GET /api/v1/orgs/:org/issues/:issue': () =>
        slow({ status: 404, json: { error: 'issue not found', code: 'not_found' } }),
    });
    // The history entry keeps the list's copy (location.state) after the team was taken away.
    renderApp([{ pathname: '/orbit/issue/HID-1', state: { issue: secret } }] as never);
    const seen: string[] = [];
    const obs = new MutationObserver(() => seen.push(document.body.textContent ?? ''));
    obs.observe(document.body, { subtree: true, childList: true, characterData: true });
    expect(await screen.findByRole('heading', { name: 'Issue not found' })).toBeInTheDocument();
    obs.disconnect();
    expect(seen.some((t) => t.includes('Secret merger plan'))).toBe(false);
    expect(document.title).not.toContain('Secret');
  });
});
