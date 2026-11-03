import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it } from 'vitest';
import type { Project, ProjectLabel } from '../../api/types';
import { callsTo, joinServer, page } from '../../test/join';
import { projectOf } from '../../test/mockApi';
import { currentLocation, renderApp } from '../../test/render';
import { labelValueText } from './ProjectFormDialog';

// LAUN-194: project labels on the project form, the list (chips and filter) and the project page.

const AREA = '01b00000-0000-7000-8000-000000000001';
const API = '01b00000-0000-7000-8000-000000000002';
const RISK = '01b00000-0000-7000-8000-000000000003';
const P1 = '01a00000-0000-7000-8000-000000000001';
const P2 = '01a00000-0000-7000-8000-000000000002';
const P3 = '01a00000-0000-7000-8000-000000000003';

const lab = (id: string, name: string, over: Partial<ProjectLabel> = {}): ProjectLabel => ({
  id,
  name,
  color: '#336699',
  parentId: null,
  projectCount: 0,
  createdAt: '2026-10-01T00:00:00Z',
  ...over,
});
const LABELS = [lab(AREA, 'Area'), lab(API, 'API', { parentId: AREA }), lab(RISK, 'Risk')];
const ref = (l: ProjectLabel) => ({ id: l.id, name: l.name, color: l.color, parentId: l.parentId });

const proj = (id: string, name: string, labels: ProjectLabel[] = [], over: Partial<Project> = {}): Project =>
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
    labels: labels.map(ref),
    version: 3,
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

const alpha = proj(P1, 'Alpha', [LABELS[1]!, LABELS[2]!]); // API (in Area) and Risk
const beta = proj(P2, 'Beta', [LABELS[2]!]);
const gamma = proj(P3, 'Gamma');

function server(routes = {}, list: Project[] = [alpha, beta, gamma]) {
  return joinServer({
    routes: {
      'GET /api/v1/orgs/:org/projects': () => ({ json: page(list) }),
      'GET /api/v1/orgs/:org/projects/:project': (r) => ({
        json: list.find((p) => p.id === r.params.project) ?? alpha,
      }),
      'GET /api/v1/orgs/:org/project-labels': () => ({ json: page(LABELS) }),
      'GET /api/v1/orgs/:org/milestones': () => ({ json: page([]) }),
      'GET /api/v1/orgs/:org/issues': () => ({ json: page([]) }),
      'GET /api/v1/orgs/:org/products': () => ({ json: page([]) }),
      ...routes,
    },
  });
}
const rowNames = () =>
  screen
    .getAllByRole('row')
    .filter((r) => within(r).queryAllByRole('link').length > 0)
    .map((r) => within(r).getAllByRole('link')[0]!.lastElementChild!.textContent);
const bar = () => within(screen.getByRole('toolbar', { name: 'Filters' }));

beforeEach(() => localStorage.clear());

describe('New project dialog, Labels (LAUN-194)', () => {
  async function openNew() {
    renderApp('/orbit/projects');
    await userEvent.click(await screen.findByRole('button', { name: 'New project' }));
    return screen.findByRole('dialog');
  }
  async function pickLabels(dlg: HTMLElement, names: string[]) {
    await userEvent.click(within(dlg).getByRole('button', { name: /^Labels/ }));
    for (const n of names)
      await userEvent.click(await within(dlg).findByRole('option', { name: new RegExp(n) }));
    await userEvent.keyboard('{Escape}');
  }

  it('sends the chosen label ids in labelIds', async () => {
    const api = server({
      'POST /api/v1/orgs/:org/projects': () => ({ status: 201, json: proj(P3, 'Roadmap', [LABELS[2]!]) }),
    });
    const dlg = await openNew();
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Name' }), 'Roadmap');
    await userEvent.click(within(dlg).getByRole('button', { name: /^Teams/ }));
    await userEvent.click(await within(dlg).findByRole('option', { name: /ORB/ }));
    await userEvent.keyboard('{Escape}');
    await pickLabels(dlg, ['Risk', 'API']);
    expect(within(dlg).getByRole('button', { name: 'Labels: Risk, API' })).toBeInTheDocument();
    await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
    await waitFor(() => expect(callsTo(api, 'POST', '/orgs/orbit/projects')).toHaveLength(1));
    expect(callsTo(api, 'POST', '/orgs/orbit/projects')[0]!.body).toEqual({
      name: 'Roadmap',
      statusId: 'ps-planned',
      teams: ['ORB'],
      labelIds: [RISK, API],
    });
  });

  it('sends no labelIds when none is chosen', async () => {
    const api = server({
      'POST /api/v1/orgs/:org/projects': () => ({ status: 201, json: proj(P3, 'Roadmap') }),
    });
    const dlg = await openNew();
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Name' }), 'Roadmap');
    await userEvent.click(within(dlg).getByRole('button', { name: /^Teams/ }));
    await userEvent.click(await within(dlg).findByRole('option', { name: /ORB/ }));
    await userEvent.keyboard('{Escape}');
    await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
    await waitFor(() => expect(callsTo(api, 'POST', '/orgs/orbit/projects')).toHaveLength(1));
    expect(callsTo(api, 'POST', '/orgs/orbit/projects')[0]!.body).not.toHaveProperty('labelIds');
  });
});

describe('Edit project dialog, Labels (LAUN-194)', () => {
  async function openEdit() {
    renderApp(`/orbit/projects/${P1}`);
    await userEvent.click(await screen.findByRole('button', { name: 'Edit project' }));
    return screen.findByRole('dialog');
  }

  it('opens with the project labels chosen and PATCHes the whole new set with the version', async () => {
    const api = server({
      'PATCH /api/v1/orgs/:org/projects/:project': () => ({ json: proj(P1, 'Alpha', [LABELS[1]!]) }),
    });
    const dlg = await openEdit();
    await userEvent.click(within(dlg).getByRole('button', { name: /^Labels/ }));
    const risk = await within(dlg).findByRole('option', { name: /Risk/ });
    expect(risk).toHaveAttribute('aria-selected', 'true');
    await userEvent.click(risk); // take Risk off
    await userEvent.keyboard('{Escape}');
    await userEvent.click(within(dlg).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(api, 'PATCH', `/projects/${P1}`)).toHaveLength(1));
    expect(callsTo(api, 'PATCH', `/projects/${P1}`)[0]!.body).toEqual({ version: 3, labelIds: [API] });
  });

  it('sends nothing when the labels are unchanged', async () => {
    const api = server();
    const dlg = await openEdit();
    await userEvent.click(within(dlg).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(callsTo(api, 'PATCH', `/projects/${P1}`)).toHaveLength(0);
  });

  it('drops a label that was deleted since the project was read, instead of sending a dead id', async () => {
    const GONE = '01b00000-0000-7000-8000-0000000000ff';
    const stale = proj(P1, 'Alpha', [LABELS[2]!]);
    stale.labels.push({ id: GONE, name: 'Gone', color: '#000000', parentId: null });
    const api = server(
      { 'PATCH /api/v1/orgs/:org/projects/:project': () => ({ json: proj(P1, 'Alpha', [LABELS[2]!]) }) },
      [stale],
    );
    const dlg = await openEdit();
    await userEvent.click(within(dlg).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(api, 'PATCH', `/projects/${P1}`)).toHaveLength(1));
    expect(callsTo(api, 'PATCH', `/projects/${P1}`)[0]!.body).toEqual({ version: 3, labelIds: [RISK] });
  });
});

describe('Projects list, label chips and filter (LAUN-194)', () => {
  it('shows the labels of each project as chips', async () => {
    server();
    renderApp('/orbit/projects');
    const table = await screen.findByRole('table', { name: 'Projects' });
    const rows = within(table).getAllByRole('row');
    const alphaRow = rows.find((r) => within(r).queryByText('Alpha'))!;
    expect(within(alphaRow).getByText('API')).toBeInTheDocument();
    expect(within(alphaRow).getByText('Risk')).toBeInTheDocument();
    const gammaRow = rows.find((r) => within(r).queryByText('Gamma'))!;
    expect(within(gammaRow).getByText('No labels')).toBeInTheDocument();
  });

  it('filters by label from the bar, keeps it in the address, and Clear removes it', async () => {
    server();
    renderApp('/orbit/projects');
    await screen.findByRole('table', { name: 'Projects' });
    await userEvent.click(await bar().findByRole('button', { name: /^Label/ }));
    await userEvent.click(await screen.findByRole('option', { name: /Risk/ }));
    await waitFor(() => expect(rowNames()).toEqual(['Alpha', 'Beta']));
    expect(currentLocation.search).toBe(`?label=${RISK}`);
    expect(bar().getByRole('button', { name: 'Clear (1)' })).toBeInTheDocument();
    await userEvent.click(bar().getByRole('button', { name: 'Clear (1)' }));
    await waitFor(() => expect(rowNames()).toEqual(['Alpha', 'Beta', 'Gamma']));
    expect(currentLocation.search).toBe('');
  });

  it('reads ?label= from the address, and a group matches the labels in it', async () => {
    server();
    renderApp(`/orbit/projects?label=${AREA}`);
    await screen.findByRole('table', { name: 'Projects' });
    expect(rowNames()).toEqual(['Alpha']); // only Alpha has a label in the Area group
    expect(await bar().findByRole('button', { name: 'Label: Area' })).toBeInTheDocument();
  });

  it('does not remember the label filter in the stored view', async () => {
    server();
    renderApp(`/orbit/projects?label=${RISK}`);
    await screen.findByRole('table', { name: 'Projects' });
    await userEvent.click(bar().getByRole('button', { name: 'Status' }));
    await userEvent.click(await screen.findByRole('option', { name: 'In progress' }));
    expect(localStorage.getItem('launchpad.projects.view.v1')).not.toContain(RISK);
  });

  it('hides the label filter when the org has no project labels', async () => {
    server({ 'GET /api/v1/orgs/:org/project-labels': () => ({ json: page([]) }) });
    renderApp('/orbit/projects');
    await screen.findByRole('table', { name: 'Projects' });
    expect(bar().queryByRole('button', { name: /^Label/ })).toBeNull();
  });
});

describe('Project page, labels (LAUN-194)', () => {
  it('lists the project labels', async () => {
    server();
    renderApp(`/orbit/projects/${P1}`);
    const list = await screen.findByRole('list', { name: 'Project labels' });
    expect(
      within(list)
        .getAllByRole('listitem')
        .map((li) => li.textContent),
    ).toEqual(['API', 'Risk']);
  });

  it('shows no label list for a project without labels', async () => {
    server();
    renderApp(`/orbit/projects/${P3}`);
    await screen.findByRole('heading', { name: 'Gamma', level: 1 });
    expect(screen.queryByRole('list', { name: 'Project labels' })).toBeNull();
  });
});

describe('Long names, many labels and the 50 limit (LAUN-194)', () => {
  const many = Array.from({ length: 51 }, (_, i) =>
    lab(
      `01b00000-0000-7000-8000-0000000001${String(i).padStart(2, '0')}`,
      `Label ${String(i).padStart(2, '0')}`,
    ),
  );
  const LONG = 'x'.repeat(100);

  it('puts the full name in a title on the pill, in the list cell and on the project page', async () => {
    const long = proj(P1, 'Alpha', [lab(RISK, LONG)]);
    server({}, [long]);
    renderApp('/orbit/projects');
    const table = await screen.findByRole('table', { name: 'Projects' });
    expect(within(table).getByTitle(LONG)).toBeInTheDocument();
    expect(within(table).getByTitle(LONG).closest('td')?.className).toMatch(/labelsCell/);
  });

  it('names the Labels chip by a count and the first two names when there are many', () => {
    expect(labelValueText([])).toBeUndefined();
    expect(labelValueText(['A', 'B'])).toBe('A, B');
    expect(labelValueText(['A', 'B', 'C', 'D'])).toBe('4 labels, A, B and 2 more');
  });

  it('stops at 50 labels in the picker and says so', async () => {
    const fifty = proj(P1, 'Alpha', many.slice(0, 50));
    const api = server({ 'GET /api/v1/orgs/:org/project-labels': () => ({ json: page(many) }) }, [fifty]);
    renderApp(`/orbit/projects/${P1}`);
    await userEvent.click(await screen.findByRole('button', { name: 'Edit project' }));
    const dlg = await screen.findByRole('dialog');
    expect(within(dlg).getByText('A project can have at most 50 labels.')).toBeInTheDocument();
    await userEvent.click(within(dlg).getByRole('button', { name: /^Labels/ }));
    await userEvent.click(await within(dlg).findByRole('option', { name: /Label 50/ }));
    await userEvent.keyboard('{Escape}');
    await userEvent.click(within(dlg).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(callsTo(api, 'PATCH', `/projects/${P1}`)).toHaveLength(0); // 51st not added: nothing changed
  });

  it('shows the server sentence when a label 400 comes back', async () => {
    server({
      'PATCH /api/v1/orgs/:org/projects/:project': () => ({
        status: 400,
        json: { error: 'unknown project label in labelIds', code: 'validation_failed' },
      }),
    });
    renderApp(`/orbit/projects/${P1}`);
    await userEvent.click(await screen.findByRole('button', { name: 'Edit project' }));
    const dlg = await screen.findByRole('dialog');
    await userEvent.click(within(dlg).getByRole('button', { name: /^Labels/ }));
    await userEvent.click(await within(dlg).findByRole('option', { name: /Risk/ }));
    await userEvent.keyboard('{Escape}');
    await userEvent.click(within(dlg).getByRole('button', { name: 'Save' }));
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('unknown project label in labelIds');
  });
});
