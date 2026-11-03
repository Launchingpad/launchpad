import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it } from 'vitest';
import type { Project } from '../../api/types';
import { callsTo, joinServer, page } from '../../test/join';
import { statusRef, type MockRequest } from '../../test/mockApi';
import { renderApp } from '../../test/render';

// LAUN-177: the New project dialog after Linear: pills, milestones, discard, Cmd+Enter.

const NEW = '01a00000-0000-7000-8000-0000000000aa';
const created: Project = {
  id: NEW,
  name: 'Roadmap',
  description: '',
  content: '',
  contentTruncated: false,
  state: 'planned',
  status: statusRef('planned'),
  lead: null,
  startDate: null,
  targetDate: null,
  teams: [{ id: 't-ORB', key: 'ORB', name: 'Orbit' }],
  labels: [],
  version: 1,
  createdAt: '2026-10-01T00:00:00Z',
  updatedAt: '2026-10-01T00:00:00Z',
  startedAt: null,
  completedAt: null,
  canceledAt: null,
  archivedAt: null,
  updateSchedule: { frequency: 'none', weekday: 5, hour: 9 },
  avatarUrl: null,
  emoji: null,
  issueCounts: { total: 0, scope: 0, done: 0, started: 0, canceled: 0 },
};
const ms = (name: string) => ({
  id: `m-${name}`,
  projectId: NEW,
  name,
  description: '',
  targetDate: null,
  sortOrder: 0,
  createdAt: '',
  updatedAt: '',
});

function server(routes = {}) {
  return joinServer({
    routes: {
      'GET /api/v1/orgs/:org/projects': () => ({ json: page([]) }),
      'GET /api/v1/orgs/:org/projects/:project': () => ({ json: created }),
      'GET /api/v1/orgs/:org/milestones': () => ({ json: page([]) }),
      'GET /api/v1/orgs/:org/issues': () => ({ json: page([]) }),
      'POST /api/v1/orgs/:org/projects': () => ({ status: 201, json: created }),
      ...routes,
    },
  });
}

async function openNew() {
  renderApp('/orbit/projects');
  await userEvent.click(await screen.findByRole('button', { name: 'New project' }));
  return screen.findByRole('dialog');
}
async function choose(dlg: HTMLElement, pill: RegExp, option: RegExp) {
  await userEvent.click(within(dlg).getByRole('button', { name: pill }));
  await userEvent.click(await within(dlg).findByRole('option', { name: option }));
  if (within(dlg).queryByRole('listbox')) await userEvent.keyboard('{Escape}');
}
async function setDate(dlg: HTMLElement, label: string, value: string) {
  await userEvent.click(within(dlg).getByRole('button', { name: new RegExp(`^${label}`) }));
  await userEvent.type(await within(dlg).findByLabelText(label, { selector: 'input' }), value);
  await userEvent.keyboard('{Escape}');
}
const addMilestone = (dlg: HTMLElement) =>
  userEvent.click(within(dlg).getByRole('button', { name: 'Add milestone' }));

beforeEach(() => localStorage.clear());

describe('New project dialog layout', () => {
  it('names the org in the header, focuses the name and labels every pill', async () => {
    server();
    const dlg = await openNew();
    expect(within(dlg).getByText('Orbit')).toBeInTheDocument();
    expect(within(dlg).getByText('New project', { selector: 'span' })).toBeInTheDocument();
    expect(within(dlg).getByRole('textbox', { name: 'Name' })).toHaveFocus();
    for (const n of ['Status: Planned', 'Lead', 'Start date', 'Target date', 'Teams']) {
      const pill = within(dlg).getByRole('button', { name: n });
      expect(pill).toHaveAttribute('aria-expanded', 'false');
    }
    expect(within(dlg).getByRole('button', { name: 'Close' })).toBeInTheDocument();
  });

  it('sends each pill value in the create body', async () => {
    const api = server();
    const dlg = await openNew();
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Name' }), 'Roadmap');
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Description' }), 'Line one');
    await choose(dlg, /^Status/, /In progress/);
    expect(within(dlg).getByRole('button', { name: 'Status: In progress' })).toBeInTheDocument();
    await choose(dlg, /^Lead/, /Chen/);
    expect(within(dlg).getByRole('button', { name: 'Lead: Chen' })).toBeInTheDocument();
    await setDate(dlg, 'Start date', '2026-11-01');
    await setDate(dlg, 'Target date', '2026-12-31');
    await choose(dlg, /^Teams/, /ORB/);
    expect(within(dlg).getByRole('button', { name: 'Teams: ORB' })).toBeInTheDocument();
    await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
    await screen.findByRole('heading', { name: 'Roadmap', level: 1 });
    const post = callsTo(api, 'POST', '/orgs/orbit/projects');
    expect(post).toHaveLength(1);
    expect(post[0]!.body).toEqual({
      name: 'Roadmap',
      description: 'Line one',
      statusId: 'ps-started',
      lead: 'chen',
      startDate: '2026-11-01',
      targetDate: '2026-12-31',
      teams: ['ORB'],
    });
    expect(callsTo(api, 'POST', '/milestones')).toHaveLength(0);
  });

  it('lets Cmd+Enter create from the description', async () => {
    const api = server();
    const dlg = await openNew();
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Name' }), 'Roadmap');
    await choose(dlg, /^Teams/, /ORB/);
    await userEvent.click(within(dlg).getByRole('textbox', { name: 'Description' }));
    await userEvent.keyboard('{Control>}{Enter}{/Control}');
    await screen.findByRole('heading', { name: 'Roadmap', level: 1 });
    expect(callsTo(api, 'POST', '/orgs/orbit/projects')).toHaveLength(1);
  });

  it('lets Cmd+Enter create while a pill list has focus without picking the highlighted row', async () => {
    const api = server();
    const dlg = await openNew();
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Name' }), 'Roadmap');
    await choose(dlg, /^Teams/, /ORB/);
    await userEvent.click(within(dlg).getByRole('button', { name: /^Lead/ }));
    await within(dlg).findByRole('listbox');
    await userEvent.keyboard('{Meta>}{Enter}{/Meta}');
    await screen.findByRole('heading', { name: 'Roadmap', level: 1 });
    expect(callsTo(api, 'POST', '/orgs/orbit/projects')[0]!.body).not.toHaveProperty('lead');
  });
});

describe('Closing the New project dialog', () => {
  it('closes at once with Escape when nothing was typed', async () => {
    server();
    const dlg = await openNew();
    await userEvent.keyboard('{Escape}');
    expect(dlg).not.toBeInTheDocument();
  });

  it('asks before it drops text, keeps it on Keep editing and closes on Discard', async () => {
    const api = server();
    const dlg = await openNew();
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Name' }), 'Roadmap');
    await userEvent.keyboard('{Escape}');
    const keep = within(dlg).getByRole('button', { name: 'Keep editing' });
    expect(keep).toHaveFocus();
    expect(within(dlg).getByText(/Discard this project/)).toBeInTheDocument();
    await userEvent.click(keep);
    expect(within(dlg).getByRole('textbox', { name: 'Name' })).toHaveValue('Roadmap');
    expect(within(dlg).queryByRole('button', { name: 'Keep editing' })).toBeNull();
    await userEvent.click(within(dlg).getByRole('button', { name: 'Cancel' }));
    await userEvent.click(within(dlg).getByRole('button', { name: 'Discard' }));
    expect(dlg).not.toBeInTheDocument();
    expect(callsTo(api, 'POST', '/projects')).toHaveLength(0);
  });

  it('closes only the open pill panel on Escape, not the dialog', async () => {
    server();
    const dlg = await openNew();
    await userEvent.click(within(dlg).getByRole('button', { name: /^Lead/ }));
    await within(dlg).findByRole('listbox');
    await userEvent.keyboard('{Escape}');
    expect(within(dlg).queryByRole('listbox')).toBeNull();
    expect(dlg).toBeInTheDocument();
    expect(within(dlg).getByRole('button', { name: /^Lead/ })).toHaveFocus();
  });
});

describe('Milestones in the New project dialog', () => {
  async function fill(dlg: HTMLElement) {
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Name' }), 'Roadmap');
    await choose(dlg, /^Teams/, /ORB/);
  }

  it('creates the project, then each milestone in order with the project id', async () => {
    const order: string[] = [];
    const api = server({
      'POST /api/v1/orgs/:org/projects': () => {
        order.push('project');
        return { status: 201, json: created };
      },
      'POST /api/v1/orgs/:org/milestones': (r: MockRequest) => {
        order.push(String((r.body as { name: string }).name));
        return { status: 201, json: ms('x') };
      },
    });
    const dlg = await openNew();
    await fill(dlg);
    await addMilestone(dlg);
    const first = within(dlg).getByRole('textbox', { name: 'Milestone 1 name' });
    await waitFor(() => expect(first).toHaveFocus());
    await userEvent.type(first, 'Alpha');
    await userEvent.type(within(dlg).getByLabelText('Milestone 1 target date'), '2026-11-15');
    await userEvent.click(first);
    await userEvent.keyboard('{Enter}'); // Enter in a name adds the next row; it does not send the form
    expect(callsTo(api, 'POST', '/projects')).toHaveLength(0);
    await userEvent.type(await within(dlg).findByRole('textbox', { name: 'Milestone 2 name' }), 'Beta');
    await addMilestone(dlg); // a blank third row is ignored
    await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
    await screen.findByRole('heading', { name: 'Roadmap', level: 1 });
    expect(order).toEqual(['project', 'Alpha', 'Beta']);
    const posts = callsTo(api, 'POST', '/orgs/orbit/milestones');
    expect(posts.map((c) => c.body)).toEqual([
      { projectId: NEW, name: 'Alpha', sortOrder: 0, targetDate: '2026-11-15' },
      { projectId: NEW, name: 'Beta', sortOrder: 1 },
    ]);
  });

  it('asks for a name when a milestone has a date but no name, and sends nothing', async () => {
    const api = server();
    const dlg = await openNew();
    await fill(dlg);
    await addMilestone(dlg);
    await userEvent.type(within(dlg).getByLabelText('Milestone 1 target date'), '2026-11-15');
    await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
    expect(within(dlg).getByRole('textbox', { name: 'Milestone 1 name' })).toHaveAccessibleDescription(
      'Name this milestone or remove it.',
    );
    expect(callsTo(api, 'POST', '/projects')).toHaveLength(0);
  });

  it('keeps the project when a milestone fails, says which one, and retries only that one', async () => {
    let betaTries = 0;
    const api = server({
      'POST /api/v1/orgs/:org/milestones': (r: MockRequest) => {
        const n = (r.body as { name: string }).name;
        if (n === 'Beta' && ++betaTries === 1)
          return { status: 500, json: { error: 'boom', code: 'internal' } };
        return { status: 201, json: ms(n) };
      },
    });
    const dlg = await openNew();
    await fill(dlg);
    await addMilestone(dlg);
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Milestone 1 name' }), 'Alpha');
    await addMilestone(dlg);
    await userEvent.type(await within(dlg).findByRole('textbox', { name: 'Milestone 2 name' }), 'Beta');
    await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
    const alert = await within(dlg).findByRole('alert');
    expect(alert).toHaveTextContent('The project “Roadmap” was created, but this milestone was not: “Beta”.');
    expect(dlg).toBeInTheDocument();
    expect(within(dlg).getByRole('textbox', { name: 'Milestone 2 name' })).toHaveAccessibleDescription(
      'Could not create this milestone.',
    );
    expect(within(dlg).getByRole('textbox', { name: 'Milestone 1 name' })).toBeDisabled(); // made
    await userEvent.click(await within(dlg).findByRole('button', { name: 'Create milestones' }));
    await screen.findByRole('heading', { name: 'Roadmap', level: 1 });
    expect(callsTo(api, 'POST', '/orgs/orbit/projects')).toHaveLength(1);
    expect(
      callsTo(api, 'POST', '/orgs/orbit/milestones').map((c) => (c.body as { name: string }).name),
    ).toEqual(['Alpha', 'Beta', 'Beta']);
  });

  it('opens the project on Cancel after a failed milestone, without asking to discard', async () => {
    const api = server({
      'POST /api/v1/orgs/:org/milestones': () => ({ status: 403, json: { error: 'no', code: 'forbidden' } }),
    });
    const dlg = await openNew();
    await fill(dlg);
    await addMilestone(dlg);
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Milestone 1 name' }), 'Alpha');
    await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
    await within(dlg).findByRole('alert');
    await userEvent.click(within(dlg).getByRole('button', { name: 'Cancel' }));
    await screen.findByRole('heading', { name: 'Roadmap', level: 1 });
    expect(callsTo(api, 'POST', '/orgs/orbit/projects')).toHaveLength(1);
  });

  it('collapses and expands the card and removes a row', async () => {
    server();
    const dlg = await openNew();
    await addMilestone(dlg);
    const toggle = within(dlg).getByRole('button', { name: /^Milestones/ });
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await userEvent.click(toggle);
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    await userEvent.click(toggle);
    await userEvent.click(within(dlg).getByRole('button', { name: 'Remove milestone 1' }));
    expect(within(dlg).queryByRole('textbox', { name: 'Milestone 1 name' })).toBeNull();
  });

  it('has no Milestones card when editing a project', async () => {
    server();
    renderApp(`/orbit/projects/${NEW}`);
    await userEvent.click(await screen.findByRole('button', { name: 'Edit project' }));
    const dlg = await screen.findByRole('dialog');
    expect(within(dlg).queryByRole('button', { name: 'Add milestone' })).toBeNull();
    expect(within(dlg).getByText('Edit project', { selector: 'span' })).toBeInTheDocument();
  });
});

describe('A save in flight', () => {
  function slowServer() {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => (release = r));
    const api = server({
      'POST /api/v1/orgs/:org/projects': async () => {
        await gate;
        return { status: 201, json: created };
      },
      'POST /api/v1/orgs/:org/milestones': () => ({ status: 201, json: ms('x') }),
    });
    return { api, release: () => release() };
  }
  async function ready(dlg: HTMLElement) {
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Name' }), 'Roadmap');
    await choose(dlg, /^Teams/, /ORB/);
  }

  it('ignores Esc, the X, Cancel and a scrim click while saving, then opens the project once', async () => {
    const { api, release } = slowServer();
    const dlg = await openNew();
    await ready(dlg);
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Description' }), 'x');
    await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
    expect(await within(dlg).findByRole('button', { name: 'Saving…' })).toBeDisabled();
    await userEvent.keyboard('{Escape}');
    await userEvent.click(within(dlg).getByRole('button', { name: 'Close' }));
    await userEvent.click(within(dlg).getByRole('button', { name: 'Cancel' }));
    await userEvent.pointer({ keys: '[MouseLeft]', target: dlg.parentElement! });
    expect(dlg).toBeInTheDocument();
    expect(within(dlg).queryByText(/Discard this project/)).toBeNull();
    release();
    await screen.findByRole('heading', { name: 'Roadmap', level: 1 });
    expect(callsTo(api, 'POST', '/orgs/orbit/projects')).toHaveLength(1);
  });

  it('creates once when Cmd+Enter is pressed twice and Create is clicked meanwhile', async () => {
    const { api, release } = slowServer();
    const dlg = await openNew();
    await ready(dlg);
    await userEvent.click(within(dlg).getByRole('textbox', { name: 'Description' }));
    await userEvent.keyboard('{Meta>}{Enter}{Enter}{/Meta}');
    await userEvent.click(within(dlg).getByRole('button', { name: /Saving|Create project/ }));
    release();
    await screen.findByRole('heading', { name: 'Roadmap', level: 1 });
    expect(callsTo(api, 'POST', '/orgs/orbit/projects')).toHaveLength(1);
  });

  it('keeps the milestone fields read-only while saving', async () => {
    const { release } = slowServer();
    const dlg = await openNew();
    await ready(dlg);
    await addMilestone(dlg);
    const m = within(dlg).getByRole('textbox', { name: 'Milestone 1 name' });
    await userEvent.type(m, 'Alpha');
    await userEvent.keyboard('{Meta>}{Enter}{/Meta}');
    await waitFor(() => expect(m).toHaveAttribute('readonly'));
    expect(m).toHaveFocus(); // read-only, not disabled: focus stays, so Esc and Tab keep working
    await userEvent.type(m, 'Z');
    expect(m).toHaveValue('Alpha');
    release();
    await screen.findByRole('heading', { name: 'Roadmap', level: 1 });
  });
});

describe('After a partial failure', () => {
  const failing = (names: string[]) =>
    server({
      'POST /api/v1/orgs/:org/milestones': (r: MockRequest) =>
        names.includes((r.body as { name: string }).name)
          ? { status: 500, json: { error: 'boom', code: 'internal' } }
          : { status: 201, json: ms('x') },
    });
  async function fillMs(dlg: HTMLElement, list: string[]) {
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Name' }), 'Roadmap');
    await choose(dlg, /^Teams/, /ORB/);
    for (const [i, n] of list.entries()) {
      await addMilestone(dlg);
      await userEvent.type(await within(dlg).findByRole('textbox', { name: `Milestone ${i + 1} name` }), n);
    }
  }

  it('puts focus on the first failed milestone, locks the top block, and Esc still opens the project', async () => {
    const api = failing(['Alpha']); // the FIRST row fails
    const dlg = await openNew();
    await fillMs(dlg, ['Alpha', 'Beta']);
    await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
    await within(dlg).findByRole('alert');
    const first = within(dlg).getByRole('textbox', { name: 'Milestone 1 name' });
    await waitFor(() => expect(first).toHaveFocus());
    expect(within(dlg).getByRole('textbox', { name: 'Name' }).closest('[inert]')).not.toBeNull();
    expect(within(dlg).getByRole('textbox', { name: 'Milestone 2 name' })).toBeDisabled(); // made
    await userEvent.keyboard('{Escape}');
    await screen.findByRole('heading', { name: 'Roadmap', level: 1 });
    expect(callsTo(api, 'POST', '/orgs/orbit/projects')).toHaveLength(1);
  });

  it('keeps Tab inside the dialog after a failure', async () => {
    failing(['Alpha']);
    const dlg = await openNew();
    await fillMs(dlg, ['Alpha']);
    await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
    await within(dlg).findByRole('alert');
    await waitFor(() => expect(dlg.contains(document.activeElement)).toBe(true));
    for (let i = 0; i < 8; i++) {
      await userEvent.tab();
      expect(dlg.contains(document.activeElement)).toBe(true);
    }
  });

  it('keeps the dialog and the typed text when the project call fails with the milestone card filled', async () => {
    for (const status of [400, 500]) {
      const api = server({
        'POST /api/v1/orgs/:org/projects': () => ({
          status,
          json: {
            error: 'name must be 1-255 characters',
            code: status === 400 ? 'validation_failed' : 'internal',
          },
        }),
      });
      const dlg = await openNew();
      await fillMs(dlg, ['Alpha']);
      await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
      if (status === 400)
        expect(await within(dlg).findByText('Enter 1 to 255 characters.')).toBeInTheDocument();
      else expect(await within(dlg).findByRole('alert')).toHaveTextContent('Could not create the project');
      expect(callsTo(api, 'POST', '/milestones')).toHaveLength(0);
      expect(within(dlg).getByRole('textbox', { name: 'Milestone 1 name' })).toHaveValue('Alpha');
      expect(within(dlg).getByRole('button', { name: 'Create project' })).toBeEnabled();
      // focus is back inside the dialog, so Esc works (it asks to discard)
      await waitFor(() => expect(dlg.contains(document.activeElement)).toBe(true));
      await userEvent.keyboard('{Escape}');
      expect(within(dlg).getByText(/Discard this project/)).toBeInTheDocument();
      await userEvent.click(within(dlg).getByRole('button', { name: 'Discard' }));
      expect(dlg).not.toBeInTheDocument();
    }
  });

  it('wraps a very long failed name in the alert', async () => {
    failing(['W'.repeat(200)]);
    const dlg = await openNew();
    await fillMs(dlg, ['W'.repeat(200)]);
    await userEvent.click(within(dlg).getByRole('button', { name: 'Create project' }));
    expect((await within(dlg).findByRole('alert')).className).toMatch(/wrapAny/);
  });
});

describe('Keys and limits', () => {
  it('Enter in a milestone date adds a row and does not send the form', async () => {
    const api = server();
    const dlg = await openNew();
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Name' }), 'Roadmap');
    await choose(dlg, /^Teams/, /ORB/);
    await addMilestone(dlg);
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Milestone 1 name' }), 'Alpha');
    await userEvent.click(within(dlg).getByLabelText('Milestone 1 target date'));
    await userEvent.keyboard('{Enter}');
    expect(await within(dlg).findByRole('textbox', { name: 'Milestone 2 name' })).toBeInTheDocument();
    expect(callsTo(api, 'POST', '/projects')).toHaveLength(0);
  });

  it('Esc on the discard strip returns to editing and keeps the dialog', async () => {
    server();
    const dlg = await openNew();
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Name' }), 'Roadmap');
    await userEvent.keyboard('{Escape}');
    expect(within(dlg).getByRole('button', { name: 'Keep editing' })).toHaveFocus();
    await userEvent.keyboard('{Escape}');
    expect(dlg).toBeInTheDocument();
    expect(within(dlg).queryByRole('button', { name: 'Keep editing' })).toBeNull();
    expect(within(dlg).getByRole('textbox', { name: 'Name' })).toHaveValue('Roadmap');
  });

  it('counts a form with only a milestone as changed', async () => {
    server();
    const dlg = await openNew();
    await addMilestone(dlg);
    await userEvent.type(within(dlg).getByRole('textbox', { name: 'Milestone 1 name' }), 'Alpha');
    await userEvent.keyboard('{Escape}');
    expect(within(dlg).getByText(/Discard this project/)).toBeInTheDocument();
  });

  it('limits the names to 255 characters and keeps the name on one line', async () => {
    server();
    const dlg = await openNew();
    const name = within(dlg).getByRole('textbox', { name: 'Name' });
    expect(name).toHaveAttribute('maxlength', '255');
    await userEvent.type(name, 'Road{Enter}map');
    expect(name).toHaveValue('Road');
    expect(within(dlg).getByRole('textbox', { name: 'Description' })).toHaveFocus();
    await addMilestone(dlg);
    expect(within(dlg).getByRole('textbox', { name: 'Milestone 1 name' })).toHaveAttribute(
      'maxlength',
      '255',
    );
  });

  it('shows the pills before the description', async () => {
    server();
    const dlg = await openNew();
    const pills = within(dlg).getByRole('group', { name: 'Project properties' });
    const desc = within(dlg).getByRole('textbox', { name: 'Description' });
    expect(pills.compareDocumentPosition(desc) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });
});
