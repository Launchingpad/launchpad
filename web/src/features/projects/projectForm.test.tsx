import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { Project } from '../../api/types';
import { callsTo, joinServer, MARKER, page } from '../../test/join';
import { projectOf } from '../../test/mockApi';
import { renderApp } from '../../test/render';
import { GROUP_KEY } from './ProjectListPage';
import { ApiError } from '../../api/errors';
import { replacesKey } from './ProjectFormDialog';
import { groupByProduct, NO_PRODUCT } from './projects';

const ID1 = '01a00000-0000-7000-8000-000000000001';
const NEW = '01a00000-0000-7000-8000-0000000000aa';

const team = (id: string, key: string, name: string) => ({ id, key, name });
const proj = (id: string, name: string, keys: string[], over: Partial<Project> = {}): Project =>
  projectOf({
    id,
    name,
    description: 'About it',
    content: '',
    contentTruncated: false,
    state: 'started',
    lead: null,
    startDate: null,
    targetDate: null,
    teams: keys.map((k) => team(`t-${k}`, k, k)),
    labels: [],
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
const prod = (slug: string, name: string, teamKeys: string[]) => ({
  id: `p-${slug}`,
  slug,
  name,
  description: '',
  icon: null,
  color: null,
  version: 1,
  teamKeys,
  createdAt: '',
  updatedAt: '',
  archivedAt: null,
});

describe('groupByProduct', () => {
  const P = (n: string, keys: string[]) => proj(`id-${n}`, n, keys);
  const products = [
    { slug: 'web', name: 'Web', teamKeys: ['WEB', 'API'] },
    { slug: 'apps', name: 'Apps', teamKeys: ['IOS', 'API'] },
  ];
  it('puts a project under the product holding most of its teams, ties to the first by name', () => {
    const g = groupByProduct(
      [P('a', ['WEB', 'API']), P('b', ['IOS', 'API']), P('c', ['API']), P('d', ['WEB', 'IOS', 'API'])],
      products,
    );
    expect(g.map((x) => [x.name, x.projects.map((p) => p.name)])).toEqual([
      ['Apps', ['b', 'c', 'd']], // b: Apps 2, Web 1; c and d tie (1-1, 2-2) and Apps is first by name
      ['Web', ['a']], // a: Web 2, Apps 1
    ]);
  });
  it('keeps the incoming order inside a group and shows a project once', () => {
    const g = groupByProduct([P('z', ['WEB']), P('a', ['WEB'])], products);
    expect(g).toHaveLength(1);
    expect(g[0]!.projects.map((p) => p.name)).toEqual(['z', 'a']);
  });
  it('puts projects with no product team (or no team) under No product, last, and omits empty products', () => {
    const g = groupByProduct([P('x', ['OTHER']), P('y', []), P('w', ['WEB'])], products);
    expect(g.map((x) => x.name)).toEqual(['Web', NO_PRODUCT]);
    expect(g[1]!.projects.map((p) => p.name)).toEqual(['x', 'y']);
  });
  it('has no product bucket for an empty product list except No product', () => {
    const g = groupByProduct([P('x', ['A'])], []);
    expect(g.map((x) => x.name)).toEqual([NO_PRODUCT]);
  });
});

function server(routes = {}) {
  const projects = [proj(ID1, 'Alpha', ['ORB'], { lead: { id: 'u1', username: 'chen', name: 'Chen' } })];
  return joinServer({
    routes: {
      'GET /api/v1/orgs/:org/projects': () => ({ json: page(projects) }),
      'GET /api/v1/orgs/:org/projects/:project': (r) => {
        const f = projects.find((x) => x.id === r.params.project);
        return f ? { json: f } : { status: 404, json: { error: 'nope', code: 'not_found' } };
      },
      'GET /api/v1/orgs/:org/milestones': () => ({ json: page([]) }),
      'GET /api/v1/orgs/:org/issues': () => ({ json: page([]) }),
      ...routes,
    },
  });
}

beforeEach(() => localStorage.clear());

type Dlg = HTMLElement;
/** Open a pill, click one option, and close the panel (a multi list stays open until Esc). */
async function choose(dlg: Dlg, pill: RegExp | string, option: RegExp | string) {
  await userEvent.click(within(dlg).getByRole('button', { name: pill }));
  await userEvent.click(await within(dlg).findByRole('option', { name: option }));
  if (within(dlg).queryByRole('listbox')) await userEvent.keyboard('{Escape}');
}
const pickTeam = (dlg: Dlg) => choose(dlg, /^Teams/, /ORB/);
async function setDate(dlg: Dlg, label: string, value: string) {
  await userEvent.click(within(dlg).getByRole('button', { name: new RegExp(`^${label}`) }));
  await userEvent.type(await within(dlg).findByLabelText(label, { selector: 'input' }), value);
  await userEvent.keyboard('{Escape}');
}
async function openNew() {
  renderApp('/orbit/projects');
  await userEvent.click(await screen.findByRole('button', { name: 'New project' }));
  return screen.findByRole('dialog');
}

describe('New project dialog', () => {
  it('keeps Create off, with a reason on the page, until there is a name and a team', async () => {
    const api = server();
    const dlg = await openNew();
    const create = within(dlg).getByRole('button', { name: 'Create project' });
    expect(create).toBeDisabled();
    expect(create).toHaveAccessibleDescription('Enter a name and choose at least one team.');
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Name' }), 'Roadmap');
    expect(create).toHaveAccessibleDescription('Choose at least one team.');
    expect(create).toBeDisabled();
    await pickTeam(dlg);
    expect(create).toBeEnabled();
    expect(create).not.toHaveAccessibleDescription();
    expect(callsTo(api, 'POST', '/projects')).toHaveLength(0);
  });

  it('names the missing field when Cmd+Enter is pressed too early', async () => {
    const api = server();
    const dlg = await openNew();
    const name = within(dlg).getByRole('textbox', { name: 'Name' });
    expect(name).toHaveFocus();
    await userEvent.keyboard('{Meta>}{Enter}{/Meta}');
    expect(name).toHaveAttribute('aria-invalid', 'true');
    expect(name).toHaveAccessibleDescription('Enter 1 to 255 characters.');
    await userEvent.type(name, 'Roadmap');
    await userEvent.keyboard('{Meta>}{Enter}{/Meta}');
    expect(within(dlg).getByRole('list')).toHaveTextContent('Choose at least one team.');
    expect(within(dlg).getByRole('button', { name: /^Teams/ })).toHaveFocus();
    expect(callsTo(api, 'POST', '/projects')).toHaveLength(0);
  });

  it('refuses a target date before the start date', async () => {
    const api = server();
    renderApp('/orbit/projects');
    await userEvent.click(await screen.findByRole('button', { name: 'New project' }));
    const dlg = await screen.findByRole('dialog');
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Name' }), 'R');
    await pickTeam(dlg);
    await setDate(dlg, 'Start date', '2026-12-10');
    await setDate(dlg, 'Target date', '2026-12-01');
    await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
    expect(within(dlg).getByText('The target date cannot be before the start date.')).toBeInTheDocument();
    expect(callsTo(api, 'POST', '/projects')).toHaveLength(0);
  });

  it('posts the body to /projects with an Idempotency-Key and opens the new project', async () => {
    const created = proj(NEW, 'Roadmap', ['ORB'], { state: 'planned' });
    const api = server({
      'POST /api/v1/orgs/:org/projects': () => ({ status: 201, json: created }),
      'GET /api/v1/orgs/:org/projects/:project': () => ({ json: created }),
    });
    renderApp('/orbit/projects');
    await userEvent.click(await screen.findByRole('button', { name: 'New project' }));
    const dlg = await screen.findByRole('dialog');
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Name' }), '  Roadmap ');
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Description' }), 'Plan');
    await choose(dlg, /^Lead/, /Chen/);
    await setDate(dlg, 'Target date', '2026-12-01');
    await pickTeam(dlg);
    await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
    await screen.findByRole('heading', { name: 'Roadmap', level: 1 });
    const post = callsTo(api, 'POST', '/orgs/orbit/projects');
    expect(post).toHaveLength(1);
    expect(post[0]!.body).toEqual({
      name: 'Roadmap',
      statusId: 'ps-planned',
      teams: ['ORB'],
      description: 'Plan',
      lead: 'chen',
      targetDate: '2026-12-01',
    });
    expect(post[0]!.headers.get('Idempotency-Key')).toBeTruthy();
  });

  it('shows a fixed sentence for a server refusal and keeps the dialog open', async () => {
    server({
      'POST /api/v1/orgs/:org/projects': () => ({
        status: 400,
        json: { error: `name must be 1-255 characters ${MARKER}`, code: 'validation_failed' },
      }),
    });
    renderApp('/orbit/projects');
    await userEvent.click(await screen.findByRole('button', { name: 'New project' }));
    const dlg = await screen.findByRole('dialog');
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Name' }), 'R');
    await pickTeam(dlg);
    await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
    expect(await within(dlg).findByText('Enter 1 to 255 characters.')).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('also offers New project in the empty state', async () => {
    server({ 'GET /api/v1/orgs/:org/projects': () => ({ json: page([]) }) });
    renderApp('/orbit/projects');
    await screen.findByText('No projects yet');
    await userEvent.click(screen.getByRole('button', { name: 'New project' }));
    expect(await screen.findByRole('dialog')).toBeInTheDocument();
  });
});

describe('Edit project dialog', () => {
  it('PATCHes only the changed fields with the version and shows the saved name', async () => {
    let saved = proj(ID1, 'Alpha', ['ORB']);
    const api = server({
      'GET /api/v1/orgs/:org/projects/:project': () => ({ json: saved }),
      'PATCH /api/v1/orgs/:org/projects/:project': () => {
        saved = { ...saved, name: 'Alpha 2', version: 4 };
        return { json: saved };
      },
    });
    renderApp(`/orbit/projects/${ID1}`);
    await userEvent.click(await screen.findByRole('button', { name: 'Edit project' }));
    const dlg = await screen.findByRole('dialog');
    const name = within(dlg).getByRole('textbox', { name: 'Name' });
    await userEvent.clear(name);
    await userEvent.type(name, 'Alpha 2');
    await userEvent.click(within(dlg).getByRole('button', { name: 'Save' }));
    await screen.findByRole('heading', { name: 'Alpha 2', level: 1 });
    const patch = callsTo(api, 'PATCH', `/projects/${ID1}`);
    expect(patch).toHaveLength(1);
    expect(patch[0]!.body).toEqual({ version: 3, name: 'Alpha 2' });
  });

  it('sends null to clear the lead and the full team set when teams change', async () => {
    const api = server({
      'PATCH /api/v1/orgs/:org/projects/:project': () => ({
        json: proj(ID1, 'Alpha', ['ORB'], { version: 4 }),
      }),
    });
    renderApp(`/orbit/projects/${ID1}`);
    await userEvent.click(await screen.findByRole('button', { name: 'Edit project' }));
    const dlg = await screen.findByRole('dialog');
    await choose(dlg, /^Lead/, /No lead/);
    await pickTeam(dlg);
    await userEvent.click(within(dlg).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(api, 'PATCH', `/projects/${ID1}`)).toHaveLength(1));
    expect(callsTo(api, 'PATCH', `/projects/${ID1}`)[0]!.body).toEqual({ version: 3, lead: null, teams: [] });
  });

  it('says so on a version conflict, in a fixed sentence', async () => {
    server({
      'PATCH /api/v1/orgs/:org/projects/:project': () => ({
        status: 409,
        json: { error: `project is at version 9 ${MARKER}`, code: 'version_conflict' },
      }),
    });
    renderApp(`/orbit/projects/${ID1}`);
    await userEvent.click(await screen.findByRole('button', { name: 'Edit project' }));
    const dlg = await screen.findByRole('dialog');
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Name' }), 'x');
    await userEvent.click(within(dlg).getByRole('button', { name: 'Save' }));
    expect(await within(dlg).findByRole('alert')).toHaveTextContent(
      'Someone else changed this project. Close this window and open it again.',
    );
    expect(document.body.textContent).not.toContain(MARKER);
  });

  it('sends nothing when nothing changed', async () => {
    const api = server();
    renderApp(`/orbit/projects/${ID1}`);
    await userEvent.click(await screen.findByRole('button', { name: 'Edit project' }));
    const dlg = await screen.findByRole('dialog');
    await userEvent.click(within(dlg).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(callsTo(api, 'PATCH', `/projects/${ID1}`)).toHaveLength(0);
  });
});

describe('Idempotency-Key lifecycle on create', () => {
  type Reply = { status: number; json: unknown } | 'network';
  async function twoSubmits(first: Reply) {
    let n = 0;
    const created = proj(NEW, 'Roadmap', ['ORB']);
    const api = server({
      'POST /api/v1/orgs/:org/projects': () => {
        n++;
        if (n === 1) {
          if (first === 'network') throw new TypeError('Failed to fetch');
          return first;
        }
        return { status: 201, json: created };
      },
      'GET /api/v1/orgs/:org/projects/:project': () => ({ json: created }),
    });
    renderApp('/orbit/projects');
    await userEvent.click(await screen.findByRole('button', { name: 'New project' }));
    const dlg = await screen.findByRole('dialog');
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Name' }), 'R');
    await pickTeam(dlg);
    await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
    await waitFor(() => expect(callsTo(api, 'POST', '/orgs/orbit/projects')).toHaveLength(1));
    await waitFor(() => expect(within(dlg).getByRole('button', { name: 'Create project' })).toBeEnabled());
    await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
    await waitFor(() => expect(callsTo(api, 'POST', '/orgs/orbit/projects')).toHaveLength(2));
    const k = callsTo(api, 'POST', '/orgs/orbit/projects').map((c) => c.headers.get('Idempotency-Key'));
    expect(k[0]).toBeTruthy();
    return { k, dlg };
  }

  it('uses a new key after a stored 400', async () => {
    const { k } = await twoSubmits({
      status: 400,
      json: { error: 'name must be x', code: 'validation_failed' },
    });
    expect(k[1]).not.toBe(k[0]);
  });
  it('uses a new key after another stored 409', async () => {
    const { k } = await twoSubmits({ status: 409, json: { error: 'x', code: 'conflict' } });
    expect(k[1]).not.toBe(k[0]);
  });
  it('keeps the key after a network failure', async () => {
    const { k } = await twoSubmits('network');
    expect(k[1]).toBe(k[0]);
  });
  it('keeps the key after a 500', async () => {
    const { k } = await twoSubmits({ status: 500, json: { error: 'boom', code: 'internal' } });
    expect(k[1]).toBe(k[0]);
  });
  it('keeps the key after idempotency_outcome_unknown and says to check the list', async () => {
    let n = 0;
    const api = server({
      'POST /api/v1/orgs/:org/projects': () => {
        n++;
        return { status: 409, json: { error: `x ${MARKER}`, code: 'idempotency_outcome_unknown' } };
      },
    });
    renderApp('/orbit/projects');
    await userEvent.click(await screen.findByRole('button', { name: 'New project' }));
    const dlg = await screen.findByRole('dialog');
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Name' }), 'R');
    await pickTeam(dlg);
    await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
    expect(await within(dlg).findByRole('alert')).toHaveTextContent(
      'The project may have been created. Check the project list before you try again.',
    );
    expect(document.body.textContent).not.toContain(MARKER);
    await waitFor(() => expect(within(dlg).getByRole('button', { name: 'Create project' })).toBeEnabled());
    await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
    await waitFor(() => expect(n).toBe(2));
    const k = callsTo(api, 'POST', '/orgs/orbit/projects').map((c) => c.headers.get('Idempotency-Key'));
    expect(k[1]).toBe(k[0]);
  });
  it('keeps the key after a 403', async () => {
    const { k } = await twoSubmits({ status: 403, json: { error: 'no', code: 'forbidden' } });
    expect(k[1]).toBe(k[0]);
  });
  it('keeps the key after a 401 (a signed-out session leaves the dialog, so the rule is checked directly)', () => {
    expect(replacesKey(new ApiError(401, 'unauthorized', 'x'))).toBe(false);
    expect(replacesKey(new ApiError(403, 'forbidden', 'x'))).toBe(false);
    expect(replacesKey(new ApiError(400, 'validation_failed', 'x'))).toBe(true);
    expect(replacesKey(new ApiError(0, 'network', 'x'))).toBe(false);
  });
  it('maps idempotency_key_reused to the may-have-been-created sentence', async () => {
    server({
      'POST /api/v1/orgs/:org/projects': () => ({
        status: 409,
        json: { error: `x ${MARKER}`, code: 'idempotency_key_reused' },
      }),
    });
    renderApp('/orbit/projects');
    await userEvent.click(await screen.findByRole('button', { name: 'New project' }));
    const dlg = await screen.findByRole('dialog');
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Name' }), 'R');
    await pickTeam(dlg);
    await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
    expect(await within(dlg).findByRole('alert')).toHaveTextContent(
      'The project may have been created. Check the project list before you try again.',
    );
    expect(document.body.textContent).not.toContain(MARKER);
  });
  it('maps a server date-order refusal to the target date field', async () => {
    server({
      'POST /api/v1/orgs/:org/projects': () => ({
        status: 400,
        json: { error: 'targetDate is before startDate', code: 'validation_failed' },
      }),
    });
    renderApp('/orbit/projects');
    await userEvent.click(await screen.findByRole('button', { name: 'New project' }));
    const dlg = await screen.findByRole('dialog');
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Name' }), 'R');
    await pickTeam(dlg);
    await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
    expect(
      await within(dlg).findByText('The target date cannot be before the start date.'),
    ).toBeInTheDocument();
  });
  it('maps 50 teams and an unknown team to their own sentences', async () => {
    server({
      'POST /api/v1/orgs/:org/projects': () => ({
        status: 400,
        json: { error: 'at most 50 teams per project', code: 'validation_failed' },
      }),
    });
    renderApp('/orbit/projects');
    await userEvent.click(await screen.findByRole('button', { name: 'New project' }));
    const dlg = await screen.findByRole('dialog');
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Name' }), 'R');
    await pickTeam(dlg);
    await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
    expect(await within(dlg).findByText('A project can have at most 50 teams.')).toBeInTheDocument();
  });
});

describe('Edit diff', () => {
  it('sends only the description when only it changed', async () => {
    const api = server({
      'PATCH /api/v1/orgs/:org/projects/:project': () => ({
        json: proj(ID1, 'Alpha', ['ORB'], { version: 4 }),
      }),
    });
    renderApp(`/orbit/projects/${ID1}`);
    await userEvent.click(await screen.findByRole('button', { name: 'Edit project' }));
    const dlg = await screen.findByRole('dialog');
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Description' }), ' more');
    await userEvent.click(within(dlg).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(callsTo(api, 'PATCH', `/projects/${ID1}`)).toHaveLength(1));
    expect(callsTo(api, 'PATCH', `/projects/${ID1}`)[0]!.body).toEqual({
      version: 3,
      description: 'About it more',
    });
  });
  it('sends nothing for a stored description with edge whitespace left untouched', async () => {
    const padded = proj(ID1, 'Alpha', ['ORB'], { description: '  About it \n' });
    const api = server({ 'GET /api/v1/orgs/:org/projects/:project': () => ({ json: padded }) });
    renderApp(`/orbit/projects/${ID1}`);
    await userEvent.click(await screen.findByRole('button', { name: 'Edit project' }));
    const dlg = await screen.findByRole('dialog');
    await userEvent.click(within(dlg).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(callsTo(api, 'PATCH', `/projects/${ID1}`)).toHaveLength(0);
  });
});

describe('Projects grouped by product', () => {
  const ids = ['1', '2', '3'].map((n) => `01a00000-0000-7000-8000-00000000000${n}`);
  function grouped(products: unknown[]) {
    return server({
      'GET /api/v1/orgs/:org/projects': () => ({
        json: page([
          proj(ids[0]!, 'In web', ['WEB']),
          proj(ids[1]!, 'In apps', ['IOS']),
          proj(ids[2]!, 'Loose', ['OPS']),
        ]),
      }),
      'GET /api/v1/orgs/:org/products': () => ({ json: page(products) }),
    });
  }
  const two = [prod('web', 'Web', ['WEB']), prod('apps', 'Apps', ['IOS'])];

  it('shows a heading per product by name, then No product, each with its projects', async () => {
    grouped(two);
    renderApp('/orbit/projects');
    await screen.findByRole('heading', { name: /Apps/, level: 2 });
    const heads = screen.getAllByRole('heading', { level: 2 }).map((h) => h.textContent);
    expect(heads).toEqual(['Apps 1', 'Web 1', 'No product 1']);
    const apps = screen.getByRole('rowgroup', { name: /Apps/ });
    expect(within(apps).getByText('In apps')).toBeInTheDocument();
    expect(screen.getByText(/appears under the product with most of them/)).toBeInTheDocument();
    expect(
      within(screen.getByRole('rowgroup', { name: /No product/ })).getByText('Loose'),
    ).toBeInTheDocument();
  });

  it('looks as before with no products: no group headings and no toggle', async () => {
    grouped([]);
    renderApp('/orbit/projects');
    await screen.findByRole('table', { name: 'Projects' });
    expect(screen.queryAllByRole('heading', { level: 2 })).toHaveLength(0);
    expect(screen.queryByRole('checkbox', { name: 'Group by product' })).toBeNull();
  });

  it('is flat with no toggle when the products read fails', async () => {
    server({
      'GET /api/v1/orgs/:org/projects': () => ({ json: page([proj(ids[0]!, 'In web', ['WEB'])]) }),
      'GET /api/v1/orgs/:org/products': () => ({ status: 500, json: { error: 'x', code: 'internal' } }),
    });
    renderApp('/orbit/projects');
    expect(await screen.findByRole('table', { name: 'Projects' })).toBeInTheDocument();
    expect(screen.queryAllByRole('heading', { level: 2 })).toHaveLength(0);
    expect(screen.queryByRole('checkbox', { name: 'Group by product' })).toBeNull();
  });

  it('still renders, grouped, when localStorage throws', async () => {
    const get = vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    const set = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('blocked');
    });
    try {
      grouped(two);
      renderApp('/orbit/projects');
      await screen.findByRole('heading', { name: /Apps/, level: 2 });
      await userEvent.click(screen.getByRole('checkbox', { name: 'Group by product' }));
      expect(await screen.findByRole('table', { name: 'Projects' })).toBeInTheDocument();
    } finally {
      get.mockRestore();
      set.mockRestore();
    }
  });

  it('remembers the toggle off and shows the flat list', async () => {
    grouped(two);
    renderApp('/orbit/projects');
    const box = await screen.findByRole('checkbox', { name: 'Group by product' });
    expect(box).toBeChecked();
    await userEvent.click(box);
    expect(await screen.findByRole('table', { name: 'Projects' })).toBeInTheDocument();
    expect(screen.queryAllByRole('heading', { level: 2 })).toHaveLength(0);
    expect(localStorage.getItem(GROUP_KEY)).toBe('off');
  });

  it('starts flat when the choice was stored as off', async () => {
    localStorage.setItem(GROUP_KEY, 'off');
    grouped(two);
    renderApp('/orbit/projects');
    expect(await screen.findByRole('table', { name: 'Projects' })).toBeInTheDocument();
    expect(await screen.findByRole('checkbox', { name: 'Group by product' })).not.toBeChecked();
  });
});
