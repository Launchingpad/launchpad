import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it } from 'vitest';
import type { Project, ProjectStatus } from '../../api/types';
import { callsTo, joinServer, page } from '../../test/join';
import { projectOf, projectStatuses } from '../../test/mockApi';
import { renderApp } from '../../test/render';

// LAUN-195 (decisions-T29): custom project statuses on the project list, page and dialogs.

const NOW = '2026-10-01T00:00:00Z';
const active: ProjectStatus = {
  id: 'ps-active',
  kind: 'started',
  name: 'Active',
  color: '#00ff00',
  position: 6,
  createdAt: NOW,
  updatedAt: NOW,
};
const statuses = [...projectStatuses, active];
const ref = (s: ProjectStatus) => ({ id: s.id, kind: s.kind, name: s.name, color: s.color });
const ID1 = '01a00000-0000-7000-8000-000000000001';
const ID2 = '01a00000-0000-7000-8000-000000000002';

const proj = (id: string, name: string, over: Partial<Project> = {}): Project =>
  projectOf({
    id,
    name,
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
    createdAt: NOW,
    updatedAt: NOW,
    startedAt: null,
    completedAt: null,
    canceledAt: null,
    archivedAt: null,
    avatarUrl: null,
    emoji: null,
    issueCounts: { total: 0, scope: 0, done: 0, started: 0, canceled: 0 },
    ...over,
  });

const alpha = proj(ID1, 'Alpha', { status: ref(active) }); // kind started, custom name
const beta = proj(ID2, 'Beta', { state: 'planned' });

function server(routes = {}) {
  let saved = alpha;
  return joinServer({
    routes: {
      'GET /api/v1/orgs/:org/project-statuses': () => ({ json: page(statuses) }),
      'GET /api/v1/orgs/:org/projects': () => ({ json: page([saved, beta]) }),
      'GET /api/v1/orgs/:org/projects/:project': () => ({ json: saved }),
      'GET /api/v1/orgs/:org/milestones': () => ({ json: page([]) }),
      'GET /api/v1/orgs/:org/issues': () => ({ json: page([]) }),
      'PATCH /api/v1/orgs/:org/projects/:project': (r) => {
        const b = r.body as { statusId?: string };
        const s = statuses.find((x) => x.id === b.statusId)!;
        saved = { ...saved, status: ref(s), state: s.kind, version: saved.version + 1 };
        return { json: saved };
      },
      'POST /api/v1/orgs/:org/projects': (r) => {
        const b = r.body as { statusId?: string };
        const s = statuses.find((x) => x.id === b.statusId) ?? statuses[1]!;
        return {
          status: 201,
          json: proj('01a00000-0000-7000-8000-0000000000aa', 'New one', { status: ref(s), state: s.kind }),
        };
      },
      ...routes,
    },
  });
}

beforeEach(() => localStorage.clear());

describe('custom project statuses', () => {
  it('the list and the page show the status name, not the kind', async () => {
    server();
    renderApp('/orbit/projects');
    const table = await screen.findByRole('table', { name: 'Projects' });
    expect(within(table).getByText('Active')).toBeInTheDocument();
    expect(within(table).getByText('Planned')).toBeInTheDocument();
    expect(within(table).queryByText('In progress')).toBeNull();
    renderApp(`/orbit/projects/${ID1}`);
    expect(await screen.findAllByText('Active')).not.toHaveLength(0);
  });

  it('the status filter lists a custom Started status with its kind, not after Canceled', async () => {
    server();
    const user = userEvent.setup();
    renderApp('/orbit/projects');
    await screen.findByRole('table', { name: 'Projects' });
    await user.click(
      within(screen.getByRole('toolbar', { name: 'Filters' })).getByRole('button', { name: 'Status' }),
    );
    const opts = (await screen.findAllByRole('option')).map((o) => o.textContent);
    expect(opts.slice(0, 7)).toEqual([
      'Backlog',
      'Planned',
      'In progress',
      'Active',
      'Paused',
      'Completed',
      'Canceled',
    ]);
  });

  it('the list badge shows the status icon in the status colour, with the name', async () => {
    server();
    renderApp('/orbit/projects');
    const table = await screen.findByRole('table', { name: 'Projects' });
    const badge = within(table).getByText('Active').parentElement!;
    expect(badge.querySelector('svg')).not.toBeNull();
    expect(badge.innerHTML).toContain('#00ff00');
  });

  it('filters by a status id, so a custom status is not the same as its kind', async () => {
    server();
    const user = userEvent.setup();
    renderApp('/orbit/projects');
    await screen.findByRole('table', { name: 'Projects' });
    await user.click(
      within(screen.getByRole('toolbar', { name: 'Filters' })).getByRole('button', { name: 'Status' }),
    );
    // The picker lists the custom status next to the default one of the same kind.
    expect(await screen.findByRole('option', { name: /In progress/ })).toBeInTheDocument();
    await user.click(await screen.findByRole('option', { name: /Active/ }));
    await waitFor(() => expect(screen.queryByText('Beta')).toBeNull());
    expect(screen.getByText('Alpha')).toBeInTheDocument();
    expect(JSON.parse(localStorage.getItem('launchpad.projects.view.v1')!).filters.states).toEqual([
      'ps-active',
    ]);
  });

  it('drops a remembered filter that names no status, so the list is not hidden by a stale id', async () => {
    localStorage.setItem(
      'launchpad.projects.view.v1',
      JSON.stringify({
        sort: 'status',
        dir: 'asc',
        columns: ['status'],
        filters: { states: ['started', 'ps-gone'] },
      }),
    );
    server();
    renderApp('/orbit/projects');
    expect(await screen.findByText('Alpha')).toBeInTheDocument();
    expect(screen.getByText('Beta')).toBeInTheDocument();
  });

  it('the Edit dialog lists every status with its name and PATCHes the chosen id', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp(`/orbit/projects/${ID1}`);
    await user.click(await screen.findByRole('button', { name: 'Edit project' }));
    const dlg = await screen.findByRole('dialog');
    await user.click(within(dlg).getByRole('button', { name: 'Status: Active' }));
    const names = (await within(dlg).findAllByRole('option')).map((o) => o.textContent);
    // The server list has Active last; the picker groups it with its kind: Started statuses, then Paused.
    expect(names).toEqual(['Backlog', 'Planned', 'In progress', 'Active', 'Paused', 'Completed', 'Canceled']);
    await user.click(within(dlg).getByRole('option', { name: /Paused/ }));
    if (within(dlg).queryByRole('listbox')) await user.keyboard('{Escape}');
    await user.click(within(dlg).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(api, 'PATCH', `/projects/${ID1}`)).toHaveLength(1));
    expect(callsTo(api, 'PATCH', `/projects/${ID1}`)[0]!.body).toEqual({ version: 1, statusId: 'ps-paused' });
  });

  it('the Edit dialog sends nothing when the status is left alone', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp(`/orbit/projects/${ID1}`);
    await user.click(await screen.findByRole('button', { name: 'Edit project' }));
    const dlg = await screen.findByRole('dialog');
    expect(within(dlg).getByRole('button', { name: 'Status: Active' })).toBeInTheDocument();
    await user.click(within(dlg).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(callsTo(api, 'PATCH', `/projects/${ID1}`)).toHaveLength(0);
  });

  it('the New project dialog starts on the first Planned status and POSTs the picked status id', async () => {
    const api = server();
    const user = userEvent.setup();
    renderApp('/orbit/projects');
    await user.click(await screen.findByRole('button', { name: 'New project' }));
    const dlg = await screen.findByRole('dialog');
    expect(within(dlg).getByRole('button', { name: 'Status: Planned' })).toBeInTheDocument();
    await user.type(within(dlg).getByRole('textbox', { name: 'Name' }), 'New one');
    await user.click(within(dlg).getByRole('button', { name: /^Status/ }));
    await user.click(await within(dlg).findByRole('option', { name: /Active/ }));
    if (within(dlg).queryByRole('listbox')) await user.keyboard('{Escape}');
    expect(within(dlg).getByRole('button', { name: 'Status: Active' })).toBeInTheDocument();
    await user.click(within(dlg).getByRole('button', { name: /^Teams/ }));
    await user.click(await within(dlg).findByRole('option', { name: /ORB/ }));
    await user.keyboard('{Escape}');
    await user.click(within(dlg).getByRole('button', { name: 'Create project' }));
    await waitFor(() => expect(callsTo(api, 'POST', '/orgs/orbit/projects')).toHaveLength(1));
    expect(callsTo(api, 'POST', '/orgs/orbit/projects')[0]!.body).toMatchObject({
      name: 'New one',
      statusId: 'ps-active',
    });
  });
});
