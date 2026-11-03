import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import type { ProjectLabel } from '../../api/types';
import { baseRoutes, mockApi, type Handler, type MockRequest } from '../../test/mockApi';
import { renderApp } from '../../test/render';
import { COLOR_RULE, NAME_RULE, NAME_TAKEN } from './projectLabelText';

// LAUN-194: Settings, Project labels.

const AREA = '01b00000-0000-7000-8000-000000000001';
const API = '01b00000-0000-7000-8000-000000000002';
const RISK = '01b00000-0000-7000-8000-000000000003';

function label(id: string, name: string, over: Partial<ProjectLabel> = {}): ProjectLabel {
  return {
    id,
    name,
    color: '#95a2b3',
    parentId: null,
    projectCount: 0,
    createdAt: '2026-10-01T00:00:00Z',
    ...over,
  };
}
const seed = () => [
  label(RISK, 'Risk', { color: '#ff0000', projectCount: 1 }),
  label(API, 'API', { parentId: AREA, projectCount: 2 }),
  label(AREA, 'Area'),
];

function setup(opts: { role?: 'admin' | 'member'; routes?: Record<string, Handler> } = {}) {
  const rows = seed();
  const base = baseRoutes();
  const api = mockApi({
    ...base,
    'GET /api/v1/auth/session': (r) => {
      const s = base['GET /api/v1/auth/session']!(r) as { json: { orgs: { role: string }[] } };
      s.json.orgs[0]!.role = opts.role ?? 'admin';
      return s;
    },
    'GET /api/v1/orgs/:org/project-labels': () => ({ json: { items: rows, nextCursor: null } }),
    'POST /api/v1/orgs/:org/project-labels': (r) => {
      const b = r.body as { name: string; color?: string; parentId?: string };
      const l = label(`01b00000-0000-7000-8000-0000000000${rows.length + 10}`, b.name, {
        color: b.color ?? '#95a2b3',
        parentId: b.parentId ?? null,
      });
      rows.push(l);
      return { status: 201, json: l };
    },
    'PATCH /api/v1/orgs/:org/project-labels/:label': (r) => {
      const l = rows.find((x) => x.id === r.params.label)!;
      Object.assign(l, r.body);
      return { json: l };
    },
    'DELETE /api/v1/orgs/:org/project-labels/:label': (r) => {
      rows.splice(
        rows.findIndex((x) => x.id === r.params.label),
        1,
      );
      return { status: 204 };
    },
    ...opts.routes,
  });
  return { api, rows };
}
const sent = (api: ReturnType<typeof mockApi>, method: string): MockRequest[] =>
  api.calls.filter((c) => c.method === method && /\/project-labels(\/|$)/.test(c.url.pathname));

describe('Settings, Project labels (LAUN-194)', () => {
  it('a member gets the admin notice and no list request', async () => {
    const { api } = setup({ role: 'member' });
    renderApp('/orbit/settings/project-labels');
    expect(await screen.findByRole('heading', { name: 'You need admin access.' })).toBeInTheDocument();
    expect(sent(api, 'GET')).toHaveLength(0);
  });

  it('is in the sidebar under Administration and lists groups first, labels in a group below them', async () => {
    setup();
    renderApp('/orbit/settings/project-labels');
    await screen.findByRole('heading', { name: 'Project labels', level: 1 });
    const nav = screen.getByRole('navigation', { name: 'Settings' });
    expect(
      within(within(nav).getByRole('group', { name: 'Administration' })).getByRole('link', {
        name: 'Project labels',
      }),
    ).toHaveAttribute('href', '/orbit/settings/project-labels');
    const list = await screen.findByRole('list', { name: 'Project labels' });
    const items = within(list).getAllByRole('listitem');
    expect(
      items.map((li) => li.querySelector('span')?.nextElementSibling?.firstElementChild?.textContent),
    ).toEqual(['AreaGroup', 'API', 'Risk']);
    expect(within(items[1]!).getByText('2 projects')).toBeInTheDocument();
    // A group is marked by a tag, not by indent alone; a label in it has no inline indent style.
    expect(within(items[0]!).getByText('Group')).toBeInTheDocument();
    expect(within(items[1]!).queryByText('Group')).toBeNull();
    expect(within(items[2]!).queryByText('Group')).toBeNull();
    expect(items[1]).not.toHaveAttribute('style');
    expect(within(items[2]!).getByText('1 project')).toBeInTheDocument();
  });

  it('creates a label with a colour and a group: the body holds exactly those', async () => {
    const { api } = setup();
    const user = userEvent.setup();
    renderApp('/orbit/settings/project-labels');
    await user.click(await screen.findByRole('button', { name: 'Create a label' }));
    const dlg = await screen.findByRole('dialog');
    await user.type(within(dlg).getByRole('textbox', { name: 'Name' }), '  Web ');
    const colour = within(dlg).getByRole('textbox', { name: 'Colour' });
    await user.clear(colour);
    await user.type(colour, '#112233');
    await user.selectOptions(within(dlg).getByRole('combobox', { name: 'Group' }), 'Area');
    await user.click(within(dlg).getByRole('button', { name: 'Create label' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(sent(api, 'POST')).toHaveLength(1);
    expect(sent(api, 'POST')[0]!.body).toEqual({ name: 'Web', color: '#112233', parentId: AREA });
    // The list refetched and shows the new label.
    expect(await screen.findByRole('button', { name: 'Edit Web' })).toBeInTheDocument();
  });

  it('refuses an empty name and a bad colour before any request, and says why', async () => {
    const { api } = setup();
    const user = userEvent.setup();
    renderApp('/orbit/settings/project-labels');
    await user.click(await screen.findByRole('button', { name: 'Create a label' }));
    const dlg = await screen.findByRole('dialog');
    await user.click(within(dlg).getByRole('button', { name: 'Create label' }));
    expect(await within(dlg).findByText(NAME_RULE)).toBeInTheDocument();
    await user.type(within(dlg).getByRole('textbox', { name: 'Name' }), 'X');
    const colour = within(dlg).getByRole('textbox', { name: 'Colour' });
    await user.clear(colour);
    await user.type(colour, 'red');
    await user.click(within(dlg).getByRole('button', { name: 'Create label' }));
    expect(await within(dlg).findByText(COLOR_RULE)).toBeInTheDocument();
    expect(sent(api, 'POST')).toHaveLength(0);
  });

  it('shows a fixed sentence under Name for a 409, never the server text', async () => {
    setup({
      routes: {
        'POST /api/v1/orgs/:org/project-labels': () => ({
          status: 409,
          json: { error: 'RAW-SERVER-TEXT-MARKER', code: 'conflict' },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/project-labels');
    await user.click(await screen.findByRole('button', { name: 'Create a label' }));
    const dlg = await screen.findByRole('dialog');
    await user.type(within(dlg).getByRole('textbox', { name: 'Name' }), 'risk');
    await user.click(within(dlg).getByRole('button', { name: 'Create label' }));
    expect(await within(dlg).findByText(NAME_TAKEN)).toBeInTheDocument();
    expect(screen.queryByText(/RAW-SERVER-TEXT-MARKER/)).toBeNull();
  });

  it('edit sends only what changed; no change sends nothing; ungrouping sends parentId null', async () => {
    const { api } = setup();
    const user = userEvent.setup();
    renderApp('/orbit/settings/project-labels');
    await user.click(await screen.findByRole('button', { name: 'Edit Risk' }));
    let dlg = await screen.findByRole('dialog');
    await user.click(within(dlg).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(sent(api, 'PATCH')).toHaveLength(0);

    await user.click(screen.getByRole('button', { name: 'Edit Risk' }));
    dlg = await screen.findByRole('dialog');
    const colour = within(dlg).getByRole('textbox', { name: 'Colour' });
    await user.clear(colour);
    await user.type(colour, '#00ff00');
    await user.click(within(dlg).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(sent(api, 'PATCH')).toHaveLength(1));
    expect(sent(api, 'PATCH')[0]!.url.pathname).toMatch(new RegExp(`/project-labels/${RISK}$`));
    expect(sent(api, 'PATCH')[0]!.body).toEqual({ color: '#00ff00' });

    await user.click(await screen.findByRole('button', { name: 'Edit API' }));
    dlg = await screen.findByRole('dialog');
    await user.selectOptions(within(dlg).getByRole('combobox', { name: 'Group' }), 'No group');
    await user.click(within(dlg).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(sent(api, 'PATCH')).toHaveLength(2));
    expect(sent(api, 'PATCH')[1]!.body).toEqual({ parentId: null });
  });

  it('a group cannot be put in a group: its Group select is locked', async () => {
    setup();
    const user = userEvent.setup();
    renderApp('/orbit/settings/project-labels');
    await user.click(await screen.findByRole('button', { name: 'Edit Area' }));
    const dlg = await screen.findByRole('dialog');
    expect(within(dlg).getByRole('combobox', { name: 'Group' })).toBeDisabled();
  });

  it('delete asks first, names the number of projects, and calls DELETE only on confirm', async () => {
    const { api } = setup();
    const user = userEvent.setup();
    renderApp('/orbit/settings/project-labels');
    await user.click(await screen.findByRole('button', { name: 'Delete API' }));
    let dlg = await screen.findByRole('dialog');
    expect(within(dlg).getByText(/takes it off 2 projects/)).toBeInTheDocument();
    await user.click(within(dlg).getByRole('button', { name: 'Cancel' }));
    expect(sent(api, 'DELETE')).toHaveLength(0);

    await user.click(screen.getByRole('button', { name: 'Delete API' }));
    dlg = await screen.findByRole('dialog');
    await user.click(within(dlg).getByRole('button', { name: 'Delete label' }));
    await waitFor(() => expect(sent(api, 'DELETE')).toHaveLength(1));
    expect(sent(api, 'DELETE')[0]!.url.pathname).toMatch(new RegExp(`/project-labels/${API}$`));
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Edit API' })).toBeNull());
  });
});
