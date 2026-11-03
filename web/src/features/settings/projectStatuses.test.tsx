import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import type { ProjectStatus } from '../../api/types';
import { callsTo, joinServer } from '../../test/join';
import { projectStatuses, type Handler } from '../../test/mockApi';
import { renderApp } from '../../test/render';

// LAUN-195 (decisions-T29): Settings, Project statuses.

const NOW = '2026-10-01T00:00:00Z';
const extra: ProjectStatus = {
  id: 'ps-active',
  kind: 'started',
  name: 'Active',
  color: '#00ff00',
  position: 6,
  createdAt: NOW,
  updatedAt: NOW,
};

/** A small stateful statuses API: GET answers from `rows`, the writes change them. */
function setup(opts: { role?: 'admin' | 'member'; routes?: Record<string, Handler> } = {}) {
  const rows: ProjectStatus[] = [...projectStatuses.map((s) => ({ ...s })), { ...extra }];
  const order = () => [...rows].sort((a, b) => a.position - b.position);
  const api = joinServer({
    role: opts.role ?? 'admin',
    routes: {
      'GET /api/v1/orgs/:org/project-statuses': () => ({ json: { items: order(), nextCursor: null } }),
      'POST /api/v1/orgs/:org/project-statuses': (r) => {
        const b = r.body as { kind: ProjectStatus['kind']; name: string; color: string };
        const made = { ...extra, id: `ps-${b.name}`, ...b, position: rows.length };
        rows.push(made);
        return { status: 201, json: made };
      },
      'PATCH /api/v1/orgs/:org/project-statuses/:status': (r) => {
        const s = rows.find((x) => x.id === r.params.status)!;
        Object.assign(s, r.body);
        return { json: s };
      },
      'PUT /api/v1/orgs/:org/project-statuses/order': (r) => {
        (r.body as { ids: string[] }).ids.forEach((id, i) => (rows.find((x) => x.id === id)!.position = i));
        return { json: { items: order(), nextCursor: null } };
      },
      'DELETE /api/v1/orgs/:org/project-statuses/:status': (r) => {
        rows.splice(
          rows.findIndex((x) => x.id === r.params.status),
          1,
        );
        return { status: 204 };
      },
      ...opts.routes,
    },
  });
  return { api, rows };
}

describe('Settings, Project statuses (LAUN-195)', () => {
  it('a member gets the admin notice', async () => {
    setup({ role: 'member' });
    renderApp('/orbit/settings/project-statuses');
    expect(await screen.findByRole('heading', { name: 'You need admin access.' })).toBeInTheDocument();
  });

  it('lists the statuses under their kind', async () => {
    setup();
    renderApp('/orbit/settings/project-statuses');
    const started = await screen.findByRole('list', { name: 'Started statuses' });
    expect(
      within(started)
        .getAllByRole('listitem')
        .map((li) => li.textContent?.replace(/UpDownEditDelete/, '')),
    ).toEqual(['In progress', 'Active']);
    expect(
      within(screen.getByRole('list', { name: 'Backlog statuses' })).getAllByRole('listitem'),
    ).toHaveLength(1);
  });

  it('adds a status to a kind and sends the kind, the name and the colour', async () => {
    const { api } = setup();
    const user = userEvent.setup();
    renderApp('/orbit/settings/project-statuses');
    await user.click(await screen.findByRole('button', { name: 'Add a Paused status' }));
    const dlg = await screen.findByRole('dialog');
    await user.type(within(dlg).getByLabelText('Name'), '  On hold ');
    await user.clear(within(dlg).getByLabelText('Colour'));
    await user.type(within(dlg).getByLabelText('Colour'), '#112233');
    await user.click(within(dlg).getByRole('button', { name: 'Add status' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    const post = callsTo(api, 'POST', '/project-statuses');
    expect(post).toHaveLength(1);
    expect(post[0]!.body).toEqual({ kind: 'paused', name: 'On hold', color: '#112233' });
    expect(
      within(await screen.findByRole('list', { name: 'Paused statuses' })).getByText('On hold'),
    ).toBeInTheDocument();
  });

  it('checks the name and the colour before it asks the server', async () => {
    const { api } = setup();
    const user = userEvent.setup();
    renderApp('/orbit/settings/project-statuses');
    await user.click(await screen.findByRole('button', { name: 'Add a Started status' }));
    const dlg = await screen.findByRole('dialog');
    await user.clear(within(dlg).getByLabelText('Colour'));
    await user.type(within(dlg).getByLabelText('Colour'), 'red');
    await user.click(within(dlg).getByRole('button', { name: 'Add status' }));
    expect(within(dlg).getByText(/Use 1 to 50 characters/)).toBeInTheDocument();
    expect(within(dlg).getByText('Use a colour like #5e6ad2.')).toBeInTheDocument();
    expect(callsTo(api, 'POST', '/project-statuses')).toHaveLength(0);
  });

  it('refuses a comma in the name before it asks the server', async () => {
    const { api } = setup();
    const user = userEvent.setup();
    renderApp('/orbit/settings/project-statuses');
    await user.click(await screen.findByRole('button', { name: 'Add a Started status' }));
    const dlg = await screen.findByRole('dialog');
    await user.type(within(dlg).getByLabelText('Name'), 'On, hold');
    await user.click(within(dlg).getByRole('button', { name: 'Add status' }));
    expect(within(dlg).getByText(/with no comma/)).toBeInTheDocument();
    expect(callsTo(api, 'POST', '/project-statuses')).toHaveLength(0);
  });

  it('shows a server 409 on the name field and keeps the dialog open', async () => {
    setup({
      routes: {
        'POST /api/v1/orgs/:org/project-statuses': () => ({
          status: 409,
          json: { error: 'a project status named "Active" exists', code: 'conflict' },
        }),
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/project-statuses');
    await user.click(await screen.findByRole('button', { name: 'Add a Started status' }));
    const dlg = await screen.findByRole('dialog');
    await user.type(within(dlg).getByLabelText('Name'), 'active');
    await user.click(within(dlg).getByRole('button', { name: 'Add status' }));
    expect(await within(dlg).findByText('a project status named "Active" exists')).toBeInTheDocument();
    expect(screen.getByRole('dialog')).toBeInTheDocument();
  });

  it('edits a status and sends only what changed', async () => {
    const { api } = setup();
    const user = userEvent.setup();
    renderApp('/orbit/settings/project-statuses');
    await user.click(await screen.findByRole('button', { name: 'Edit Active' }));
    const dlg = await screen.findByRole('dialog');
    await user.clear(within(dlg).getByLabelText('Name'));
    await user.type(within(dlg).getByLabelText('Name'), 'Doing');
    await user.click(within(dlg).getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    const patch = callsTo(api, 'PATCH', '/project-statuses/ps-active');
    expect(patch).toHaveLength(1);
    expect(patch[0]!.body).toEqual({ name: 'Doing' });
  });

  it('moves a status up inside its kind and puts the whole order with PUT', async () => {
    const { api } = setup();
    const user = userEvent.setup();
    renderApp('/orbit/settings/project-statuses');
    // "In progress" is first in Started: it cannot go up, and the last one cannot go down.
    expect(await screen.findByRole('button', { name: 'Move In progress up' })).toHaveAttribute(
      'aria-disabled',
      'true',
    );
    expect(screen.getByRole('button', { name: 'Move Active down' })).toHaveAttribute('aria-disabled', 'true');
    await user.click(screen.getByRole('button', { name: 'Move Active up' }));
    await waitFor(() => expect(callsTo(api, 'PUT', '/project-statuses/order')).toHaveLength(1));
    expect(callsTo(api, 'PUT', '/project-statuses/order')[0]!.body).toEqual({
      ids: [
        'ps-backlog',
        'ps-planned',
        'ps-active',
        'ps-started',
        'ps-paused',
        'ps-completed',
        'ps-canceled',
      ],
    });
    const started = await screen.findByRole('list', { name: 'Started statuses' });
    await waitFor(() => expect(within(started).getAllByRole('listitem')[0]!.textContent).toContain('Active'));
  });

  it("keeps focus on the moved row's button after a reorder, even when it reaches the end", async () => {
    setup();
    const user = userEvent.setup();
    renderApp('/orbit/settings/project-statuses');
    await user.click(await screen.findByRole('button', { name: 'Move Active up' }));
    // Active is now first in Started: its Up button is aria-disabled but still holds focus.
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Move Active up' })).toHaveAttribute('aria-disabled', 'true'),
    );
    expect(screen.getByRole('button', { name: 'Move Active up' })).toHaveFocus();
  });

  it('moves focus to the next row after a delete, and to the Add button when none is left', async () => {
    setup();
    const user = userEvent.setup();
    renderApp('/orbit/settings/project-statuses');
    await user.click(await screen.findByRole('button', { name: 'Delete In progress' }));
    await user.click(
      within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete status' }),
    );
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    await waitFor(() => expect(screen.getByRole('button', { name: 'Edit Active' })).toHaveFocus());
    await user.click(screen.getByRole('button', { name: 'Delete Active' }));
    await user.click(
      within(await screen.findByRole('dialog')).getByRole('button', { name: 'Delete status' }),
    );
    await waitFor(() => expect(screen.getByRole('button', { name: 'Add a Started status' })).toHaveFocus());
  });

  it('says why a status in use is not deleted, and deletes a free one', async () => {
    let inUse = true;
    const { api } = setup({
      routes: {
        'DELETE /api/v1/orgs/:org/project-statuses/:status': () =>
          inUse
            ? {
                status: 409,
                json: {
                  error: '2 project(s) have the status "Active"; move them to another status first',
                  code: 'status_in_use',
                },
              }
            : { status: 204 },
      },
    });
    const user = userEvent.setup();
    renderApp('/orbit/settings/project-statuses');
    await user.click(await screen.findByRole('button', { name: 'Delete Active' }));
    const dlg = await screen.findByRole('dialog');
    await user.click(within(dlg).getByRole('button', { name: 'Delete status' }));
    expect(await within(dlg).findByRole('alert')).toHaveTextContent('2 project(s) have the status "Active"');
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    inUse = false;
    await user.click(within(dlg).getByRole('button', { name: 'Delete status' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull());
    expect(callsTo(api, 'DELETE', '/project-statuses/ps-active')).toHaveLength(2);
  });
});
