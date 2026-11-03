import { renderHook, screen, within } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { ReactNode } from 'react';
import { OrgProvider } from '../../api/org';
import { keys } from '../../api/keys';
import { useDeleteProject } from './queries';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import type { Project } from '../../api/types';
import { callsTo, joinServer, page } from '../../test/join';
import { makeIssue, projectOf } from '../../test/mockApi';
import { renderApp } from '../../test/render';

const ID1 = '01a00000-0000-7000-8000-000000000001';
const ID2 = '01a00000-0000-7000-8000-000000000002';
const ID3 = '01a00000-0000-7000-8000-000000000003';

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
    createdAt: '2026-10-01T00:00:00Z',
    updatedAt: '2026-10-01T00:00:00Z',
    startedAt: null,
    completedAt: null,
    canceledAt: null,
    archivedAt: null,
    avatarUrl: null,
    emoji: null,
    issueCounts: { total: 0, scope: 0, done: 0, started: 0, canceled: 0 },
    ...over,
  });

const projects = [
  proj(ID3, 'Zeta done', { state: 'completed' }),
  proj(ID1, 'Alpha launch', {
    lead: { id: 'u1', username: 'chen', name: 'Chen' },
    targetDate: '2020-01-15',
    description: 'Ship it',
    // 4 issues, one canceled: 2 done of 3 in scope
    issueCounts: { total: 4, scope: 3, done: 2, started: 1, canceled: 1 },
  }),
  proj(ID2, 'Beta', { state: 'planned' }),
];

function server(routes = {}) {
  return joinServer({
    routes: {
      'GET /api/v1/orgs/:org/projects': () => ({ json: page(projects) }),
      'GET /api/v1/orgs/:org/projects/:project': (r) => {
        const f = projects.find((x) => x.id === r.params.project);
        return f ? { json: f } : { status: 404, json: { error: 'nope', code: 'not_found' } };
      },
      'GET /api/v1/orgs/:org/milestones': () => ({
        json: page([
          {
            id: 'm2',
            projectId: ID1,
            name: 'Beta ready',
            description: '',
            targetDate: '2026-12-01',
            sortOrder: 2,
            createdAt: '',
            updatedAt: '',
            archivedAt: null,
          },
          {
            id: 'm1',
            projectId: ID1,
            name: 'Alpha ready',
            description: '',
            targetDate: null,
            sortOrder: 1,
            createdAt: '',
            updatedAt: '',
            archivedAt: null,
          },
        ]),
      }),
      'GET /api/v1/orgs/:org/issues': () => ({ json: page([makeIssue(1, { projectId: ID1 })]) }),
      ...routes,
    },
  });
}

describe('Projects in the sidebar', () => {
  it('has a Projects link that opens the list and is current there', async () => {
    server();
    renderApp('/orbit/issues');
    const nav = await screen.findByRole('navigation', { name: 'Primary' });
    const link = within(nav).getByRole('link', { name: 'Projects' });
    expect(link).not.toHaveAttribute('aria-current');
    await userEvent.click(link);
    await screen.findByRole('heading', { name: 'Projects' });
    expect(within(nav).getByRole('link', { name: 'Projects' })).toHaveAttribute('aria-current', 'page');
  });

  it('stays current on a project page', async () => {
    server();
    renderApp(`/orbit/projects/${ID1}`);
    const nav = await screen.findByRole('navigation', { name: 'Primary' });
    await screen.findByRole('heading', { name: 'Alpha launch' });
    expect(within(nav).getByRole('link', { name: 'Projects' })).toHaveAttribute('aria-current', 'page');
  });
});

describe('Projects list page', () => {
  it('lists projects live first with state, lead, target date and derived progress', async () => {
    server();
    renderApp('/orbit/projects');
    const list = await screen.findByRole('table', { name: 'Projects' });
    const items = within(list).getAllByRole('row').slice(1);
    expect(items.map((li) => within(li).getAllByRole('link')[0]!.lastElementChild!.textContent)).toEqual([
      'Alpha launch',
      'Beta',
      'Zeta done',
    ]);
    const first = items[0]!;
    expect(within(first).getByText('In progress')).toBeInTheDocument();
    expect(within(first).getByText('Chen')).toBeInTheDocument();
    expect(first).toHaveTextContent('15 Jan 2020');
    expect(first).toHaveTextContent('(overdue)');
    // 2 done of 3 not canceled
    const bar = await within(first).findByRole('progressbar', { name: 'Alpha launch progress' });
    expect(bar).toHaveAttribute('aria-valuenow', '67');
    expect(first).toHaveTextContent('67% (2/3)');
    expect(within(items[1]!).getByText('No lead')).toBeInTheDocument();
    expect(await within(items[1]!).findByText('No issues')).toBeInTheDocument();
    expect(items[2]).not.toHaveTextContent('overdue');
  });

  it('links a row to the project page', async () => {
    server();
    renderApp('/orbit/projects');
    await userEvent.click(await screen.findByRole('link', { name: 'Alpha launch' }));
    await screen.findByRole('heading', { name: 'Alpha launch', level: 1 });
  });

  it('shows an empty state', async () => {
    server({ 'GET /api/v1/orgs/:org/projects': () => ({ json: page([]) }) });
    renderApp('/orbit/projects');
    expect(await screen.findByText('No projects yet')).toBeInTheDocument();
  });

  it('shows an error with a retry', async () => {
    let fail = true;
    server({
      'GET /api/v1/orgs/:org/projects': () =>
        fail ? { status: 500, json: { error: 'boom', code: 'internal' } } : { json: page(projects) },
    });
    renderApp('/orbit/projects');
    expect(await screen.findByText('Could not load projects')).toBeInTheDocument();
    fail = false;
    await userEvent.click(screen.getByRole('button', { name: /retry|try again/i }));
    expect(await screen.findByRole('table', { name: 'Projects' })).toBeInTheDocument();
  });

  it('reads progress from the project list: no issue call per row (LAUN-170)', async () => {
    const api = server();
    renderApp('/orbit/projects');
    const list = await screen.findByRole('table', { name: 'Projects' });
    await within(list).findByRole('progressbar', { name: 'Alpha launch progress' });
    expect(callsTo(api, 'GET', '/issues')).toHaveLength(0);
    expect(callsTo(api, 'GET', '/projects')).toHaveLength(1);
  });

  it('shows the server counts as they come, with no "N+" floor', async () => {
    server({
      'GET /api/v1/orgs/:org/projects': () => ({
        json: page([
          proj(ID1, 'Alpha launch', {
            issueCounts: { total: 900, scope: 800, done: 400, started: 0, canceled: 100 },
          }),
        ]),
      }),
    });
    renderApp('/orbit/projects');
    const bar = await screen.findByRole('progressbar', { name: 'Alpha launch progress' });
    expect(bar).toHaveAttribute('aria-valuenow', '50');
    expect(bar).toHaveAttribute('aria-valuetext', '50%, 400 of 800 issues done');
    expect(screen.getByText('50% (400/800)')).toBeInTheDocument();
  });

  it('does not throw when the server sends no issueCounts (older server)', async () => {
    const old = { ...proj(ID1, 'Alpha launch') } as Partial<Project>;
    delete old.issueCounts;
    server({ 'GET /api/v1/orgs/:org/projects': () => ({ json: page([old]) }) });
    renderApp('/orbit/projects');
    expect(await screen.findByText('No issues')).toBeInTheDocument();
    expect(screen.queryByRole('progressbar')).toBeNull();
  });

  it('a project with only canceled issues has no scope and says No issues', async () => {
    server({
      'GET /api/v1/orgs/:org/projects': () => ({
        json: page([
          proj(ID1, 'Alpha launch', {
            issueCounts: { total: 2, scope: 0, done: 0, started: 0, canceled: 2 },
          }),
        ]),
      }),
    });
    renderApp('/orbit/projects');
    expect(await screen.findByText('No issues')).toBeInTheDocument();
    expect(screen.queryByRole('progressbar')).toBeNull();
  });
});

describe('Project page', () => {
  it('shows facts, milestones in order and the project issues through the project filter', async () => {
    const api = server();
    renderApp(`/orbit/projects/${ID1}`);
    await screen.findByRole('heading', { name: 'Alpha launch', level: 1 });
    expect(screen.getByText('Ship it')).toBeInTheDocument();
    const ms = within(await screen.findByRole('region', { name: 'Milestones' })).getAllByRole('listitem');
    expect(ms.map((m) => m.textContent)).toEqual([
      expect.stringContaining('Alpha ready'),
      expect.stringContaining('Beta ready'),
    ]);
    expect(await screen.findByText('Issue 1')).toBeInTheDocument();
    expect(
      within(screen.getByRole('list', { name: 'Teams in this project' })).getByRole('link', {
        name: /ORB\s*Orbit/,
      }),
    ).toBeInTheDocument();
    const calls = callsTo(api, 'GET', '/issues');
    expect(calls.length).toBeGreaterThan(0);
    expect(calls.every((c) => c.url.searchParams.get('project') === ID1)).toBe(true);
    expect(callsTo(api, 'GET', '/milestones')[0]!.url.searchParams.get('project')).toBe(ID1);
  });

  it('shows the progress of the project from its own answer, without paging issues (LAUN-170)', async () => {
    const api = server();
    renderApp(`/orbit/projects/${ID1}`);
    const bar = await screen.findByRole('progressbar', { name: 'Alpha launch progress' });
    expect(bar).toHaveAttribute('aria-valuenow', '67');
    expect(screen.getByText('67% (2/3)')).toBeInTheDocument();
    // the only issue calls are the project's issue list, none a progress series of 100
    expect(callsTo(api, 'GET', '/issues').every((c) => c.url.searchParams.get('limit') !== '100')).toBe(true);
  });

  it('shows not found for an unknown or malformed id', async () => {
    const api = server();
    renderApp('/orbit/projects/not-an-id');
    expect(await screen.findByText('Project not found')).toBeInTheDocument();
    expect(callsTo(api, 'GET', '/projects/not-an-id')).toHaveLength(0);
  });

  it('shows not found when the server says 404', async () => {
    server();
    renderApp('/orbit/projects/01a00000-0000-7000-8000-0000000000ff');
    expect(await screen.findByText('Project not found')).toBeInTheDocument();
  });

  it('offers Create issue for an open project', async () => {
    server();
    renderApp(`/orbit/projects/${ID1}`);
    await screen.findByRole('heading', { name: 'Alpha launch', level: 1 });
    expect(
      within(screen.getByRole('main')).getByRole('button', { name: 'Create issue' }),
    ).toBeInTheDocument();
  });

  it('hides Create issue for a completed project', async () => {
    server();
    renderApp(`/orbit/projects/${ID3}`);
    await screen.findByRole('heading', { name: 'Zeta done', level: 1 });
    const main = screen.getByRole('main');
    expect(within(main).queryByRole('button', { name: 'Create issue' })).toBeNull();
  });
});

describe('Delete a project (LAUN-191)', () => {
  it('asks first, says issues stay, sends DELETE and goes to the list', async () => {
    const api = server({
      'DELETE /api/v1/orgs/:org/projects/:project': () => ({ status: 204 }),
    });
    const user = userEvent.setup();
    renderApp(`/orbit/projects/${ID2}`);
    await user.click(await screen.findByRole('button', { name: 'Delete project' }));
    const dialog = await screen.findByRole('dialog');
    expect(within(dialog).getByText(/issues and documents are not deleted/)).toBeInTheDocument();
    expect(callsTo(api, 'DELETE', `/projects/${ID2}`)).toHaveLength(0);
    await user.click(within(dialog).getByRole('button', { name: 'Delete project' }));
    await screen.findByRole('heading', { name: 'Projects' });
    expect(callsTo(api, 'DELETE', `/projects/${ID2}`)).toHaveLength(1);
  });

  it('shows a failure and keeps the dialog open', async () => {
    server({
      'DELETE /api/v1/orgs/:org/projects/:project': () => ({
        status: 403,
        json: { error: 'RAW', code: 'insufficient_scope' },
      }),
    });
    const user = userEvent.setup();
    renderApp(`/orbit/projects/${ID2}`);
    await user.click(await screen.findByRole('button', { name: 'Delete project' }));
    await user.click(
      within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete project' }),
    );
    expect(await screen.findByRole('alert')).toHaveTextContent('You need admin access to delete a project.');
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    // A failed delete keeps the page behind the dialog (the cached detail is not dropped).
    expect(screen.getByRole('heading', { name: 'Beta', hidden: true })).toBeInTheDocument();
  });

  it('is hidden from a member', async () => {
    joinServer({
      role: 'member',
      routes: {
        'GET /api/v1/orgs/:org/projects/:project': () => ({ json: projects[2] }),
        'GET /api/v1/orgs/:org/milestones': () => ({ json: page([]) }),
        'GET /api/v1/orgs/:org/issues': () => ({ json: page([]) }),
      },
    });
    renderApp(`/orbit/projects/${ID2}`);
    await screen.findByRole('button', { name: 'Edit project' });
    expect(screen.queryByRole('button', { name: 'Delete project' })).not.toBeInTheDocument();
  });
});

describe('useDeleteProject cache (LAUN-191)', () => {
  const setup = (status: number) => {
    document.cookie = 'odt_csrf=csrf-test; path=/';
    joinServer({
      routes: {
        'DELETE /api/v1/orgs/:org/projects/:project': () =>
          status === 204 ? { status } : { status, json: { error: 'x', code: 'insufficient_scope' } },
      },
    });
    const qc = new QueryClient();
    qc.setQueryData(keys.project('orbit', ID1), projects[1]);
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={qc}>
        <OrgProvider slug="orbit">{children}</OrgProvider>
      </QueryClientProvider>
    );
    return { qc, hook: renderHook(() => useDeleteProject(), { wrapper }) };
  };

  it('a 403 keeps the cached project detail', async () => {
    const { qc, hook } = setup(403);
    await expect(hook.result.current.mutateAsync(ID1)).rejects.toBeTruthy();
    expect(qc.getQueryData(keys.project('orbit', ID1))).toBeTruthy();
  });

  it('a success removes the cached project detail', async () => {
    const { qc, hook } = setup(204);
    await hook.result.current.mutateAsync(ID1);
    expect(qc.getQueryData(keys.project('orbit', ID1))).toBeUndefined();
  });
});
