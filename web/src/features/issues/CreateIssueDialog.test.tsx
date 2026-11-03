import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Cycle, Team } from '../../api/types';
import { baseRoutes, labels, makeIssue, mockApi, states, team, type Handler } from '../../test/mockApi';
import { renderApp } from '../../test/render';

// The create dialog: a title, a description, and a row of chips. Each chip opens a
// searchable popover. These tests drive the whole app, so the shortcut registry, the dialog's focus
// trap and the toast are the real ones.

const cycle = (n: number, over: Partial<Cycle> = {}): Cycle => ({
  id: `cy${n}`,
  team: { id: 'team1', key: 'ORB', name: 'Orbit' },
  number: n,
  name: null,
  description: '',
  startsAt: `2026-10-${10 + n}T00:00:00Z`,
  endsAt: `2026-10-${20 + n}T00:00:00Z`,
  completedAt: null,
  createdAt: '2026-10-01T00:00:00Z',
  updatedAt: '2026-10-01T00:00:00Z',
  archivedAt: null,
  ...over,
});

interface Opts {
  teams?: Team[];
  cycles?: Cycle[];
  /** Replies to the nth create POST; return undefined for the normal 201. */
  fail?: (n: number) => { status: number; json: unknown } | undefined;
  extra?: Record<string, Handler>;
}

function setup(opts: Opts = {}) {
  const searchable = [
    makeIssue(7, { title: 'Seven the parent', identifier: 'ORB-7' }),
    makeIssue(8, { title: 'Eight', identifier: 'ORB-8' }),
  ];
  let posts = 0;
  const api = mockApi({
    ...baseRoutes([makeIssue(1)]),
    'GET /api/v1/orgs/:org/teams': () => ({
      json: { items: opts.teams ?? [{ ...team, cyclesEnabled: !!opts.cycles }], nextCursor: null },
    }),
    'GET /api/v1/orgs/:org/teams/:team/states': (r) => ({
      json: {
        items:
          r.params.team === 'WEB' ? [{ ...states[0]!, id: 'w1', name: 'Triage', teamId: 'team2' }] : states,
        nextCursor: null,
      },
    }),
    'GET /api/v1/orgs/:org/cycles': () => ({ json: { items: opts.cycles ?? [], nextCursor: null } }),
    'GET /api/v1/orgs/:org/issues': (r) => {
      const q = r.url.searchParams.get('query');
      return {
        json: {
          items: q
            ? searchable.filter((i) => `${i.identifier} ${i.title}`.toLowerCase().includes(q.toLowerCase()))
            : [makeIssue(1)],
          nextCursor: null,
        },
      };
    },
    'POST /api/v1/orgs/:org/issues': (r) => {
      posts++;
      const bad = opts.fail?.(posts);
      if (bad) return bad;
      const b = r.body as { title: string };
      return {
        status: 201,
        json: makeIssue(90 + posts, { title: b.title, identifier: `ORB-${90 + posts}` }),
      };
    },
    'POST /api/v1/orgs/:org/labels': (r) => ({
      status: 201,
      json: { ...labels[0]!, id: 'l9', name: (r.body as { name: string }).name, color: '#00aa00' },
    }),
    ...opts.extra,
  });
  const creates = () =>
    api.calls.filter((c) => c.method === 'POST' && c.url.pathname === '/api/v1/orgs/orbit/issues');
  return { api, creates };
}

async function openDialog() {
  const user = userEvent.setup();
  renderApp('/orbit/issues');
  await screen.findByText('Issue 1');
  await user.keyboard('c');
  const dialog = await screen.findByRole('dialog', { name: 'New issue' });
  // wait for the team and its states: the Status chip then shows the default state
  await within(dialog).findByRole('button', { name: 'Status: Backlog' });
  return { user, dialog };
}
const chip = (dialog: HTMLElement, name: string | RegExp) => within(dialog).getByRole('button', { name });
async function openParent(user: ReturnType<typeof userEvent.setup>, dialog: HTMLElement) {
  await user.click(chip(dialog, /^More options/));
  await user.click(screen.getByRole('button', { name: /^Parent issue/ }));
}
const list = (name: string) => screen.getByRole('listbox', { name });
const submitBtn = (dialog: HTMLElement) => within(dialog).getByRole('button', { name: 'Create issue' });

// jsdom has no layout: `offsetParent` is null for every node, so the dialog's focus trap (which skips
// nodes without an offsetParent) would see no tabbable node. The test gives every attached node one.
// The production filter is not changed.
beforeEach(() => {
  Object.defineProperty(HTMLElement.prototype, 'offsetParent', {
    configurable: true,
    get() {
      return (this as HTMLElement).parentNode;
    },
  });
});
afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  delete (HTMLElement.prototype as { offsetParent?: unknown }).offsetParent;
});

describe('layout', () => {
  it('shows the team key, a big title, a description and one row of chips', async () => {
    const { dialog } = await openDialogWith();
    expect(within(dialog).getByText('ORB · Orbit')).toBeInTheDocument();
    expect(within(dialog).getByText('New issue', { selector: 'span' })).toBeInTheDocument();
    expect(within(dialog).getByRole('textbox', { name: 'Title' })).toHaveFocus();
    expect(within(dialog).getByPlaceholderText('Add description…')).toBeInTheDocument();
    const row = within(dialog).getByRole('group', { name: 'Properties' });
    for (const n of ['Status: Backlog', 'Priority', 'Assignee', 'Project', 'Labels', 'More options']) {
      expect(within(row).getByRole('button', { name: n })).toBeInTheDocument();
    }
    // no Cycle chip when the team has no cycles, and no labelled select boxes any more
    expect(within(row).queryByRole('button', { name: /^Cycle/ })).not.toBeInTheDocument();
    expect(dialog.querySelector('select')).toBeNull();
    expect(within(dialog).queryByRole('button', { name: 'Cancel' })).not.toBeInTheDocument();
    // Parent lives in "..." now (LAUN-19)
    expect(within(row).queryByRole('button', { name: /^Parent issue/ })).not.toBeInTheDocument();
    // no divider lines, attach at the left, Create more next to Create issue at the right
    expect(dialog.className).toMatch(/dialogPlain/);
    const foot = within(dialog).getByRole('button', { name: 'Create issue' }).closest('div')!;
    expect(within(foot).getByRole('switch', { name: 'Create more' })).toBeInTheDocument();
    expect(within(dialog).getByRole('button', { name: 'Attach files' })).toBeInTheDocument();
  });

  it('a single team shows as a plain label, not a button', async () => {
    const { dialog } = await openDialogWith();
    expect(within(dialog).queryByRole('button', { name: /^Team/ })).not.toBeInTheDocument();
  });
});

describe('popover semantics', () => {
  it('opens a combobox over a listbox, with the current item checked, in a portal', async () => {
    const { user, dialog } = await openDialogWith();
    const status = chip(dialog, 'Status: Backlog');
    expect(status).toHaveAttribute('aria-haspopup', 'listbox');
    expect(status).toHaveAttribute('aria-expanded', 'false');
    await user.click(status);
    expect(status).toHaveAttribute('aria-expanded', 'true');
    const box = screen.getByRole('combobox', { name: 'Status' });
    expect(box).toHaveFocus();
    expect(box).toHaveAttribute('aria-controls', list('Status').id);
    const options = within(list('Status')).getAllByRole('option');
    expect(options.map((o) => o.textContent)).toEqual(['Backlog', 'Todo', 'In Progress', 'Done']);
    expect(options.map((o) => o.getAttribute('aria-selected'))).toEqual(['true', 'false', 'false', 'false']);
    expect(options[0]!.querySelector('svg:last-of-type')).not.toBeNull(); // check mark
    // the list is not multi-select for Status
    expect(list('Status')).not.toHaveAttribute('aria-multiselectable');
    // in a portal inside the dialog node: a scrolling dialog body cannot clip it, and aria-modal
    // pruning (which hides everything outside the dialog node) cannot hide it
    expect(dialog.contains(list('Status'))).toBe(true);
    expect(list('Status').closest('[data-floating]')?.parentElement).toBe(dialog);
    // the shortcut letter is shown in the search box
    expect(screen.getByText('S', { selector: 'kbd' })).toBeInTheDocument();
  });

  it('type to filter, arrows move, Enter selects, focus returns to the chip', async () => {
    const { user, dialog } = await openDialogWith();
    await user.click(chip(dialog, 'Status: Backlog'));
    await user.keyboard('o');
    // "Todo", "In Progress", "Done" and "Backlog" all hold an o
    expect(within(list('Status')).getAllByRole('option')).toHaveLength(4);
    await user.keyboard('n'); // "on": Done, and nothing else
    expect(
      within(list('Status'))
        .getAllByRole('option')
        .map((o) => o.textContent),
    ).toEqual(['Done']);
    await user.clear(screen.getByRole('combobox', { name: 'Status' }));
    await user.keyboard('{ArrowDown}{ArrowDown}');
    expect(screen.getByRole('combobox')).toHaveAttribute(
      'aria-activedescendant',
      within(list('Status')).getAllByRole('option')[2]!.id,
    );
    await user.keyboard('{Enter}');
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
    const after = chip(dialog, 'Status: In Progress');
    expect(after).toHaveFocus();
    await user.click(submitBtn(dialog)); // needs a title: the error proves the dialog is still alive
    expect(screen.getByRole('alert')).toHaveTextContent('Add a title.');
  });

  it('shows "No matches" for a search with no result', async () => {
    const { user, dialog } = await openDialogWith();
    await user.click(chip(dialog, 'Status: Backlog'));
    await user.keyboard('zzz');
    expect(screen.queryAllByRole('option')).toHaveLength(0);
    expect(screen.getByText('No matches')).toBeInTheDocument();
  });

  it('Esc closes only the popover; the next Esc closes the dialog', async () => {
    const { user, dialog } = await openDialogWith();
    const status = chip(dialog, 'Status: Backlog');
    await user.click(status);
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
    expect(screen.getByRole('dialog', { name: 'New issue' })).toBeInTheDocument();
    expect(status).toHaveFocus();
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });

  it('Esc with focus on the chip (not in the search box) still closes only the popover', async () => {
    const { user, dialog } = await openDialogWith();
    const priority = chip(dialog, 'Priority');
    await user.click(priority);
    priority.focus(); // a real click leaves focus here until the panel takes it
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
    expect(screen.getByRole('dialog', { name: 'New issue' })).toBeInTheDocument();
    expect(priority).toHaveFocus();
  });

  it('a click outside closes the popover and leaves the dialog open', async () => {
    const { user, dialog } = await openDialogWith();
    await user.click(chip(dialog, 'Priority'));
    expect(list('Priority')).toBeInTheDocument();
    await user.click(within(dialog).getByRole('textbox', { name: 'Title' }));
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
    expect(screen.getByRole('dialog', { name: 'New issue' })).toBeInTheDocument();
  });

  it('a click on the scrim closes the popover first and the dialog stays', async () => {
    const { user, dialog } = await openDialogWith();
    await user.click(chip(dialog, 'Priority'));
    fireEvent.mouseDown(dialog.parentElement!); // the scrim
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
    expect(screen.getByRole('dialog', { name: 'New issue' })).toBeInTheDocument();
    fireEvent.mouseDown(dialog.parentElement!); // now nothing is open: the scrim closes the dialog
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });

  it('opening another chip closes the first; Tab leaves the popover for the next chip', async () => {
    const { user, dialog } = await openDialogWith();
    await user.click(chip(dialog, 'Priority'));
    await user.click(chip(dialog, 'Assignee'));
    expect(screen.queryByRole('listbox', { name: 'Priority' })).not.toBeInTheDocument();
    expect(list('Assignee')).toBeInTheDocument();
    await user.tab();
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
    expect(chip(dialog, 'Project')).toHaveFocus();
  });

  it('flips above a chip near the bottom edge and stays inside the viewport', async () => {
    const { user, dialog } = await openDialogWith();
    const labelsChip = chip(dialog, 'Labels');
    vi.spyOn(labelsChip, 'getBoundingClientRect').mockReturnValue({
      top: 700,
      bottom: 728,
      left: 1000,
      right: 1060,
      width: 60,
      height: 28,
      x: 1000,
      y: 700,
      toJSON: () => ({}),
    });
    vi.spyOn(HTMLElement.prototype, 'scrollHeight', 'get').mockReturnValue(300);
    await user.click(labelsChip);
    const panel = document.body.querySelector<HTMLElement>('[data-floating="Labels"]')!;
    // 1024 wide: 320 wide panel, 8 px margin
    expect(panel.style.left).toBe(`${window.innerWidth - 320 - 8}px`);
    // below the chip there are 28 px, above there are 688: the panel opens above
    expect(parseFloat(panel.style.top)).toBeLessThan(700);
    expect(parseFloat(panel.style.top) + 300).toBeLessThanOrEqual(700);
  });
});

describe('every chip: open, search, select, clear', () => {
  it('Status sets the state', async () => {
    const { user, dialog, ...rest } = await openDialogWith();
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'S');
    await user.click(chip(dialog, 'Status: Backlog'));
    await user.click(within(list('Status')).getByRole('option', { name: 'Todo' }));
    expect(chip(dialog, 'Status: Todo')).toBeInTheDocument();
    await user.click(submitBtn(dialog));
    await waitFor(() => expect(rest.creates()).toHaveLength(1));
    expect(rest.creates()[0]!.body).toMatchObject({ state: 'Todo' });
  });

  it('Priority sets and clears', async () => {
    const { user, dialog, creates } = await openDialogWith();
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'P');
    await user.click(chip(dialog, 'Priority'));
    await user.type(screen.getByRole('combobox', { name: 'Priority' }), 'hi');
    await user.keyboard('{Enter}');
    expect(chip(dialog, 'Priority: High')).toBeInTheDocument();
    // clear = "No priority"
    await user.click(chip(dialog, 'Priority: High'));
    expect(within(list('Priority')).getByRole('option', { name: 'High' })).toHaveAttribute(
      'aria-selected',
      'true',
    );
    await user.click(within(list('Priority')).getByRole('option', { name: 'No priority' }));
    expect(chip(dialog, 'Priority')).toHaveTextContent('Priority');
    await user.click(submitBtn(dialog));
    await waitFor(() => expect(creates()).toHaveLength(1));
    expect(creates()[0]!.body).toMatchObject({ priority: 0 });
  });

  it('Assignee sets (with an avatar) and clears', async () => {
    const { user, dialog, creates } = await openDialogWith();
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'A');
    await user.click(chip(dialog, 'Assignee'));
    await user.type(screen.getByRole('combobox', { name: 'Assignee' }), 'cla');
    expect(within(list('Assignee')).getAllByRole('option')).toHaveLength(1);
    await user.keyboard('{Enter}');
    const set = chip(dialog, 'Assignee: Claude');
    expect(set).toHaveTextContent('Claude');
    expect(set.querySelector('[class*="avatar"]')).not.toBeNull();
    await user.click(set);
    await user.click(within(list('Assignee')).getByRole('option', { name: 'No assignee' }));
    expect(chip(dialog, 'Assignee')).toHaveTextContent('Assignee');
    await user.click(submitBtn(dialog));
    await waitFor(() => expect(creates()).toHaveLength(1));
    expect(creates()[0]!.body).not.toHaveProperty('assignee');
  });

  it('Labels is a multi-select with colour dots; it stays open and toggles; a new label can be made', async () => {
    const { user, dialog, creates, api } = await openDialogWith();
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'L');
    await user.click(chip(dialog, 'Labels'));
    expect(list('Labels')).toHaveAttribute('aria-multiselectable', 'true');
    const bug = within(list('Labels')).getByRole('option', { name: 'bug' });
    expect(bug.querySelector('[class*="dot"]')).toHaveStyle({ background: 'rgb(214, 72, 31)' });
    await user.click(bug);
    expect(list('Labels')).toBeInTheDocument(); // multi: still open
    expect(within(list('Labels')).getByRole('option', { name: 'bug' })).toHaveAttribute(
      'aria-selected',
      'true',
    );
    expect(chip(dialog, 'Labels: bug')).toHaveTextContent('bug');
    await user.type(screen.getByRole('combobox', { name: 'Labels' }), 'fresh');
    await user.click(within(list('Labels')).getByRole('option', { name: 'Create “fresh”' }));
    await waitFor(() => expect(chip(dialog, 'Labels: bug, fresh')).toHaveTextContent('2 labels'));
    expect(
      api.calls.find((c) => c.method === 'POST' && c.url.pathname.endsWith('/labels'))?.body,
    ).toMatchObject({
      name: 'fresh',
      team: 'ORB',
    });
    await user.keyboard('{Escape}');
    await user.click(submitBtn(dialog));
    await waitFor(() => expect(creates()).toHaveLength(1));
    expect(creates()[0]!.body).toMatchObject({ labels: ['bug', 'fresh'] });
  });

  it('Labels toggles off again, and an empty selection sends no labels', async () => {
    const { user, dialog, creates } = await openDialogWith();
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'L');
    await user.click(chip(dialog, 'Labels'));
    await user.keyboard('{Enter}{Enter}'); // on, off
    await user.keyboard('{Escape}');
    expect(chip(dialog, 'Labels')).toBeInTheDocument();
    await user.click(submitBtn(dialog));
    await waitFor(() => expect(creates()).toHaveLength(1));
    expect(creates()[0]!.body).not.toHaveProperty('labels');
  });

  it('Parent searches issues on the server, selects one and clears it', async () => {
    const { user, dialog, creates, api } = await openDialogWith();
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Child');
    await openParent(user, dialog);
    expect(within(list('Parent issue')).getByRole('option', { name: 'No parent' })).toBeInTheDocument();
    await user.type(screen.getByRole('combobox', { name: 'Parent issue' }), 'seven');
    await waitFor(() =>
      expect(within(list('Parent issue')).getByRole('option', { name: /ORB-7 Seven/ })).toBeInTheDocument(),
    );
    expect(api.calls.some((c) => c.url.searchParams.get('query') === 'seven')).toBe(true);
    expect(within(list('Parent issue')).queryByRole('option', { name: /ORB-8/ })).not.toBeInTheDocument();
    await user.keyboard('{Enter}');
    expect(chip(dialog, /^More options: parent ORB-7/)).toBeInTheDocument();
    await user.click(submitBtn(dialog));
    await waitFor(() => expect(creates()).toHaveLength(1));
    expect(creates()[0]!.body).toMatchObject({ parent: 'ORB-7' });
  });

  it('Parent can be cleared with "No parent"', async () => {
    const { user, dialog } = await openDialogWith();
    await openParent(user, dialog);
    await user.type(screen.getByRole('combobox'), 'ORB-8');
    await waitFor(() => screen.getByRole('option', { name: /ORB-8/ }));
    await user.keyboard('{Enter}');
    expect(chip(dialog, /^More options: parent ORB-8/)).toBeInTheDocument();
    await openParent(user, dialog);
    await user.click(screen.getByRole('option', { name: 'No parent' }));
    expect(chip(dialog, 'More options')).toBeInTheDocument();
  });

  it('More holds due date and estimate; they go in the payload and can be cleared', async () => {
    const { user, dialog, creates } = await openDialogWith();
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'More');
    await user.click(chip(dialog, 'More options'));
    const due = screen.getByLabelText('Due date');
    expect(due).toHaveFocus();
    fireEvent.change(due, { target: { value: '2026-11-02' } });
    await user.type(screen.getByLabelText('Estimate (points)'), '3');
    await user.keyboard('{Escape}');
    expect(screen.getByRole('dialog', { name: 'New issue' })).toBeInTheDocument();
    expect(chip(dialog, /^More options: due 2026-11-02, estimate 3/)).toHaveFocus();
    await user.click(submitBtn(dialog));
    await waitFor(() => expect(creates()).toHaveLength(1));
    expect(creates()[0]!.body).toMatchObject({ dueDate: '2026-11-02', estimate: 3 });
  });

  it('More: Clear removes both values', async () => {
    const { user, dialog, creates } = await openDialogWith();
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'More');
    await user.click(chip(dialog, 'More options'));
    await user.type(screen.getByLabelText('Estimate (points)'), '5');
    await user.click(screen.getByRole('button', { name: 'Clear both' }));
    expect(screen.getByLabelText('Estimate (points)')).toHaveValue(null);
    await user.keyboard('{Escape}');
    await user.click(submitBtn(dialog));
    await waitFor(() => expect(creates()).toHaveLength(1));
    expect(creates()[0]!.body).not.toHaveProperty('estimate');
    expect(creates()[0]!.body).not.toHaveProperty('dueDate');
  });

  it('Cycle shows only when the team has cycles, lists recent finished ones and sets cycleId', async () => {
    const { user, dialog, creates } = await openDialogWith({
      cycles: [
        cycle(1, { completedAt: '2026-10-20T00:00:00Z' }),
        cycle(2),
        cycle(3, { name: 'Sprint three' }),
      ],
    });
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Cyc');
    await user.click(await within(dialog).findByRole('button', { name: 'Cycle' }));
    const names = within(list('Cycle'))
      .getAllByRole('option')
      .map((o) => o.textContent);
    expect(names[0]).toBe('No cycle');
    // the last finished cycle stays in the list, then the cycles that are not complete
    expect(names[1]).toMatch(/^Cycle 1/);
    expect(names[2]).toMatch(/^Cycle 2/);
    expect(names[3]).toMatch(/^Sprint three/);
    expect(names).toHaveLength(4);
    await user.click(within(list('Cycle')).getByRole('option', { name: /Sprint three/ }));
    expect(chip(dialog, 'Cycle: Sprint three')).toBeInTheDocument();
    await user.click(submitBtn(dialog));
    await waitFor(() => expect(creates()).toHaveLength(1));
    expect(creates()[0]!.body).toMatchObject({ cycleId: 'cy3' });
  });

  it('Cycle can be cleared with "No cycle"', async () => {
    const { user, dialog, creates } = await openDialogWith({ cycles: [cycle(2)] });
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Cyc');
    await user.click(await within(dialog).findByRole('button', { name: 'Cycle' }));
    await user.click(await screen.findByRole('option', { name: /Cycle 2/ }));
    await user.click(chip(dialog, 'Cycle: Cycle 2'));
    await user.click(screen.getByRole('option', { name: 'No cycle' }));
    await user.click(submitBtn(dialog));
    await waitFor(() => expect(creates()).toHaveLength(1));
    expect(creates()[0]!.body).not.toHaveProperty('cycleId');
  });

  it("Team: with two teams the key chip opens a popover; the choice picks that team's states", async () => {
    const web: Team = { ...team, id: 'team2', key: 'WEB', name: 'Website' };
    const { user, dialog, creates } = await openDialogWith({ teams: [team, web] });
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Site');
    await user.click(chip(dialog, /^Team/));
    expect(
      within(list('Team'))
        .getAllByRole('option')
        .map((o) => o.textContent),
    ).toEqual(['OrbitORB', 'WebsiteWEB']);
    await user.click(within(list('Team')).getByRole('option', { name: /Website/ }));
    expect(await within(dialog).findByRole('button', { name: 'Status: Triage' })).toBeInTheDocument();
    await user.click(submitBtn(dialog));
    await waitFor(() => expect(creates()).toHaveLength(1));
    expect(creates()[0]!.body).toMatchObject({ team: 'WEB', title: 'Site' });
    expect(creates()[0]!.body).not.toHaveProperty('state'); // the old team's state name is dropped
  });

  describe('team labels (LAUN-101)', () => {
    const web: Team = { ...team, id: 'team2', key: 'WEB', name: 'Website' };
    const org = { ...labels[0]!, id: 'l1', name: 'bug' };
    const flaky = { ...labels[0]!, id: 'l2', name: 'flaky', teamId: 'team1', teamKey: 'ORB' };
    const twin = { ...labels[0]!, id: 'l3', name: 'bug', teamId: 'team1', teamKey: 'ORB' };
    const extra = (post?: Handler): Record<string, Handler> => ({
      'GET /api/v1/orgs/:org/labels': (r) => ({
        json: {
          items: r.url.searchParams.get('team') === 'WEB' ? [org] : [org, flaky, twin],
          nextCursor: null,
        },
      }),
      ...(post ? { 'POST /api/v1/orgs/:org/labels': post } : {}),
    });

    it('switching team drops the selected labels, so a team label is never sent for another team', async () => {
      const { user, dialog, creates } = await openDialogWith({ teams: [team, web], extra: extra() });
      await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'T');
      await user.click(chip(dialog, 'Labels'));
      await user.click(await screen.findByRole('option', { name: /flaky/ }));
      await user.keyboard('{Escape}');
      expect(chip(dialog, 'Labels: flaky')).toBeInTheDocument();
      await user.click(chip(dialog, /^Team/));
      await user.click(within(list('Team')).getByRole('option', { name: /Website/ }));
      expect(await within(dialog).findByRole('button', { name: 'Labels' })).toBeInTheDocument();
      await user.click(submitBtn(dialog));
      await waitFor(() => expect(creates()).toHaveLength(1));
      expect(creates()[0]!.body).toMatchObject({ team: 'WEB' });
      expect(creates()[0]!.body).not.toHaveProperty('labels');
    });

    it('an org label and a team label of one name show as one row', async () => {
      const { user, dialog } = await openDialogWith({ extra: extra() });
      await user.click(chip(dialog, 'Labels'));
      await screen.findByRole('option', { name: /flaky/ });
      expect(within(list('Labels')).getAllByRole('option', { name: /^bug/ })).toHaveLength(1);
    });

    it('a refused label create shows the error and adds nothing', async () => {
      const { user, dialog } = await openDialogWith({
        extra: extra(() => ({ status: 403, json: { error: 'Label refused.', code: 'forbidden' } })),
      });
      await user.click(chip(dialog, 'Labels'));
      await user.type(await screen.findByRole('combobox', { name: 'Labels' }), 'fresh');
      await user.click(within(list('Labels')).getByRole('option', { name: 'Create “fresh”' }));
      expect((await screen.findAllByText('Label refused.')).length).toBeGreaterThan(0);
      expect(chip(dialog, 'Labels')).toBeInTheDocument();
    });
  });
});

describe('shortcuts S, P, A, L', () => {
  it('open their popovers only when no text field has focus', async () => {
    const { user, dialog } = await openDialogWith();
    // the title has focus: letters are text
    await user.keyboard('slap');
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
    expect(within(dialog).getByRole('textbox', { name: 'Title' })).toHaveValue('slap');
    // focus on the dialog itself: they open
    await user.click(within(dialog).getByRole('group', { name: 'Properties' }));
    for (const [key, name] of [
      ['s', 'Status'],
      ['p', 'Priority'],
      ['a', 'Assignee'],
      ['l', 'Labels'],
    ] as const) {
      await user.keyboard(key);
      expect(list(name)).toBeInTheDocument();
      expect(screen.getByRole('combobox', { name })).toHaveFocus();
      await user.keyboard('{Escape}');
      expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
    }
  });

  it('are plain letters inside a popover search box, and need no Ctrl or Cmd', async () => {
    const { user, dialog } = await openDialogWith();
    await user.click(chip(dialog, 'Assignee'));
    await user.keyboard('p');
    expect(screen.getByRole('combobox', { name: 'Assignee' })).toHaveValue('p');
    expect(screen.queryByRole('listbox', { name: 'Priority' })).not.toBeInTheDocument();
    await user.keyboard('{Escape}');
    await user.keyboard('{Control>}s{/Control}');
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
  });

  it('are listed in the shortcut help', async () => {
    const user = userEvent.setup();
    setup();
    renderApp('/orbit/issues');
    await screen.findByText('Issue 1');
    await user.keyboard('?');
    const help = await screen.findByRole('dialog', { name: 'Keyboard shortcuts' });
    expect(within(help).getByText('S P A L')).toBeInTheDocument();
    expect(within(help).getByText('⌘/Ctrl Enter')).toBeInTheDocument();
  });
});

describe('create, errors and Create more', () => {
  it('Create issue closes the dialog and shows the toast', async () => {
    const { user, dialog, creates } = await openDialogWith();
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), '  Plain  ');
    await user.type(within(dialog).getByRole('textbox', { name: 'Description' }), 'Body text');
    await user.click(submitBtn(dialog));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(creates()[0]!.body).toMatchObject({
      team: 'ORB',
      title: 'Plain',
      description: 'Body text',
      priority: 0,
    });
    expect(creates()[0]!.headers.get('Idempotency-Key')).toMatch(/[0-9a-f-]{36}/);
    expect(await screen.findByText('ORB-91 created')).toBeInTheDocument();
  });

  it('Cmd+Enter and Ctrl+Enter create, from the title, from a chip and from an open popover', async () => {
    const { user, dialog, creates } = await openDialogWith();
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Keys');
    await user.click(chip(dialog, 'Priority'));
    await user.keyboard('{Meta>}{Enter}{/Meta}');
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    expect(creates()).toHaveLength(1);
    expect(creates()[0]!.body).toMatchObject({ title: 'Keys' });

    await user.keyboard('c');
    const again = await screen.findByRole('dialog', { name: 'New issue' });
    await user.type(within(again).getByRole('textbox', { name: 'Title' }), 'Ctrl');
    await user.keyboard('{Control>}{Enter}{/Control}');
    await waitFor(() => expect(creates()).toHaveLength(2));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });

  it('Cmd+Enter in an open popover only creates: the marked option is not picked (Create more on, so the chip can be read after)', async () => {
    const { user, dialog, creates } = await openDialogWith();
    await user.click(within(dialog).getByRole('switch', { name: 'Create more' }));
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Mod');
    await user.click(chip(dialog, 'Priority'));
    await user.keyboard('{ArrowDown}{Meta>}{Enter}{/Meta}');
    await waitFor(() => expect(creates()).toHaveLength(1));
    await waitFor(() => expect(within(dialog).getByRole('textbox', { name: 'Title' })).toHaveValue(''));
    expect(chip(dialog, 'Priority')).toHaveTextContent('Priority'); // still unset: High was not picked
  });

  it.each([false, true])(
    'Cmd+Enter with a new label name typed makes no label and creates once (Create more %s)',
    async (createMore) => {
      const { user, dialog, creates, api } = await openDialogWith();
      if (createMore) await user.click(within(dialog).getByRole('switch', { name: 'Create more' }));
      await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Lbl');
      await user.click(chip(dialog, 'Labels'));
      await user.type(screen.getByRole('combobox', { name: 'Labels' }), 'brandnew');
      await user.keyboard('{Control>}{Enter}{/Control}');
      await waitFor(() => expect(creates()).toHaveLength(1));
      expect(creates()[0]!.body).not.toHaveProperty('labels');
      expect(api.calls.some((c) => c.method === 'POST' && c.url.pathname.endsWith('/labels'))).toBe(false);
    },
  );

  it('Cmd+Enter works from the header: the expand button and the team popover', async () => {
    const web: Team = { ...team, id: 'team2', key: 'WEB', name: 'Website' };
    const { user, dialog, creates } = await openDialogWith({ teams: [team, web] });
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Header');
    within(dialog).getByRole('button', { name: 'Make the dialog larger' }).focus();
    await user.keyboard('{Meta>}{Enter}{/Meta}');
    await waitFor(() => expect(creates()).toHaveLength(1));

    await user.keyboard('c');
    const again = await screen.findByRole('dialog', { name: 'New issue' });
    await user.type(within(again).getByRole('textbox', { name: 'Title' }), 'Team popover');
    await user.click(chip(again, /^Team/));
    await user.keyboard('{Meta>}{Enter}{/Meta}');
    await waitFor(() => expect(creates()).toHaveLength(2));
    expect(creates()[1]!.body).toMatchObject({ team: 'ORB' }); // the highlighted team was not picked
  });

  it('the "created" toast is outside the inert root, in a live region, and its link works', async () => {
    const { user, dialog } = await openDialogWith();
    await user.click(within(dialog).getByRole('switch', { name: 'Create more' }));
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Toast');
    await user.click(submitBtn(dialog));
    const toast = await screen.findByText('ORB-91 created');
    const region = toast.closest('[data-live-region]')!;
    expect(region).toHaveAttribute('aria-live', 'polite');
    expect(region.parentElement).toBe(document.body);
    expect(region.closest('[inert]')).toBeNull();
    expect(region.closest('[aria-hidden="true"]')).toBeNull();
    expect(within(region as HTMLElement).getByRole('link', { name: 'Open' })).toBeInTheDocument();
    // and the dialog still hides the rest of the app
    expect(document.getElementById('root')?.hasAttribute('inert') ?? true).toBe(true);
  });

  it('Estimate takes a whole number from 0 to 1000 and says so otherwise', async () => {
    const { user, dialog, creates } = await openDialogWith();
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Est');
    await user.click(chip(dialog, 'More options'));
    for (const bad of ['1.5', '1001']) {
      const est = screen.getByLabelText('Estimate (points)');
      await user.clear(est);
      await user.type(est, bad);
      await user.keyboard('{Escape}');
      await user.click(submitBtn(dialog));
      expect(await screen.findByRole('alert')).toHaveTextContent(
        'Estimate must be a whole number from 0 to 1000.',
      );
      expect(creates()).toHaveLength(0);
      await user.click(chip(dialog, /^More options/));
    }
    await user.clear(screen.getByLabelText('Estimate (points)'));
    await user.type(screen.getByLabelText('Estimate (points)'), '1000');
    await user.keyboard('{Escape}');
    await user.click(submitBtn(dialog));
    await waitFor(() => expect(creates()).toHaveLength(1));
    expect(creates()[0]!.body).toMatchObject({ estimate: 1000 });
  });

  it('Tab moves through the More panel (Due date, Estimate, Clear both) and closes it only after the last', async () => {
    const { user, dialog } = await openDialogWith();
    await user.click(chip(dialog, 'More options'));
    expect(screen.getByLabelText('Due date')).toHaveFocus();
    await user.tab();
    expect(screen.getByLabelText('Estimate (points)')).toHaveFocus();
    await user.keyboard('4');
    await user.tab();
    expect(screen.getByRole('button', { name: /^Parent issue/ })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole('button', { name: 'Clear both' })).toHaveFocus();
    await user.tab({ shift: true });
    expect(screen.getByRole('button', { name: /^Parent issue/ })).toHaveFocus();
    await user.tab();
    await user.tab(); // from the last field: the panel closes
    expect(screen.queryByLabelText('Estimate (points)')).not.toBeInTheDocument();
    expect(screen.getByRole('dialog', { name: 'New issue' })).toBeInTheDocument();
  });

  it('Shift+Tab from the search box closes a list popover and focus lands before the chip', async () => {
    const { user, dialog } = await openDialogWith();
    await user.click(chip(dialog, 'Assignee'));
    await user.tab({ shift: true });
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
    expect(chip(dialog, 'Priority')).toHaveFocus();
  });

  it('a panel that opened above the chip is placed again when the list height changes', async () => {
    let resize: () => void = () => {};
    vi.stubGlobal(
      'ResizeObserver',
      class {
        cb: () => void;
        constructor(cb: () => void) {
          this.cb = cb;
        }
        // The callback exists only for an element the code observes, as in a browser.
        observe() {
          resize = this.cb;
        }
        disconnect() {}
      },
    );
    const { user, dialog } = await openDialogWith();
    const labelsChip = chip(dialog, 'Labels');
    vi.spyOn(labelsChip, 'getBoundingClientRect').mockReturnValue({
      top: 700,
      bottom: 728,
      left: 100,
      right: 160,
      width: 60,
      height: 28,
      x: 100,
      y: 700,
      toJSON: () => ({}),
    });
    const height = vi.spyOn(HTMLElement.prototype, 'scrollHeight', 'get').mockReturnValue(100);
    await user.click(labelsChip);
    const panel = document.body.querySelector<HTMLElement>('[data-floating="Labels"]')!;
    expect(panel.style.top).toBe('596px'); // 700 - 4 - 100
    height.mockReturnValue(200); // the filter changed the list
    act(() => resize());
    expect(panel.style.top).toBe('496px'); // still glued to the chip
    vi.unstubAllGlobals();
  });

  it('Shift+Tab from the Team popover (the first control) stays inside the dialog', async () => {
    const web: Team = { ...team, id: 'team2', key: 'WEB', name: 'Website' };
    const { user, dialog } = await openDialogWith({ teams: [team, web] });
    await user.click(chip(dialog, /^Team/));
    expect(screen.getByRole('combobox', { name: 'Team' })).toHaveFocus();
    await user.tab({ shift: true });
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
    // the focus trap wraps: from the first control to the last one
    expect(dialog.contains(document.activeElement)).toBe(true);
    expect(document.activeElement).toBe(submitBtn(dialog));
  });

  it('Tab from the last chip panel (More) goes on inside the dialog', async () => {
    const { user, dialog } = await openDialogWith();
    await user.click(chip(dialog, 'More options'));
    await user.tab();
    await user.tab(); // Clear both is disabled: Estimate is the last field
    await user.tab();
    expect(screen.queryByLabelText('Estimate (points)')).not.toBeInTheDocument();
    expect(dialog.contains(document.activeElement)).toBe(true);
    expect(document.activeElement).not.toBe(document.body);
  });

  it('Shift+Tab in the Due date field does not close the More panel by itself; leaving the panel does', async () => {
    const { user, dialog } = await openDialogWith();
    await user.click(chip(dialog, 'More options'));
    const due = screen.getByLabelText('Due date');
    fireEvent.keyDown(due, { key: 'Tab', shiftKey: true }); // a browser moves between date segments here
    expect(screen.getByLabelText('Due date')).toBeInTheDocument();
    await user.tab({ shift: true }); // focus now really leaves the panel
    await waitFor(() => expect(screen.queryByLabelText('Due date')).not.toBeInTheDocument());
    expect(dialog.contains(document.activeElement)).toBe(true);
    expect(chip(dialog, 'More options')).toHaveFocus(); // back on the chip, not on the button before it
  });

  it('a press on a label, the padding or the empty-state row keeps the panel open and focus in it', async () => {
    const { user, dialog } = await openDialogWith();
    await user.click(chip(dialog, 'More options'));
    // the label focuses its own field
    await user.click(screen.getByText('Estimate (points)'));
    await new Promise((r) => setTimeout(r, 20)); // the blur check runs on a timer
    expect(screen.getByLabelText('Estimate (points)')).toHaveFocus();
    // the padding of the panel
    await user.click(document.body.querySelector<HTMLElement>('[data-floating]')!);
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.getByLabelText('Due date')).toBeInTheDocument();
    await user.keyboard('{Escape}'); // closes the panel only
    expect(screen.queryByLabelText('Due date')).not.toBeInTheDocument();
    expect(screen.getByRole('dialog', { name: 'New issue' })).toBeInTheDocument();

    // a list: the "No matches" row
    await user.click(chip(dialog, 'Status: Backlog'));
    await user.keyboard('zzz');
    await user.click(screen.getByText('No matches'));
    await new Promise((r) => setTimeout(r, 20));
    expect(list('Status')).toBeInTheDocument();
    expect(screen.getByRole('combobox', { name: 'Status' })).toHaveFocus();
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
    expect(screen.getByRole('dialog', { name: 'New issue' })).toBeInTheDocument();
  });

  it.each([
    ['Status: Backlog', () => screen.getByRole('combobox', { name: 'Status' })],
    ['More options', () => screen.getByLabelText('Due date')],
  ])(
    'a press on the %s chip that closes its own panel leaves focus on the chip (Safari: press moves focus to the dialog node)',
    async (name, field) => {
      const { user, dialog } = await openDialogWith();
      await user.click(chip(dialog, name));
      const c = chip(dialog, name);
      field();
      // Emulate a browser that does not focus a button on press: focus goes to the dialog node
      // unless the page prevents the press.
      if (fireEvent.mouseDown(c)) act(() => dialog.focus());
      await new Promise((r) => setTimeout(r, 20));
      fireEvent.click(c);
      await new Promise((r) => setTimeout(r, 20));
      expect(document.querySelector('[data-floating]')).toBeNull();
      expect(c).toHaveFocus();
      await user.keyboard('{Escape}');
      await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    },
  );

  it('focus that falls to the body while a panel is open goes back into the panel', async () => {
    const { user, dialog } = await openDialogWith();
    await user.click(chip(dialog, 'More options'));
    const panel = document.querySelector<HTMLElement>('[data-floating]')!;
    act(() => (document.activeElement as HTMLElement).blur());
    expect(document.body).toHaveFocus();
    await waitFor(() => expect(panel.contains(document.activeElement)).toBe(true));
  });

  it('Create more with Cmd+Enter from inside a panel leaves focus in the title', async () => {
    const { user, dialog, creates } = await openDialogWith();
    await user.click(within(dialog).getByRole('switch', { name: 'Create more' }));
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Mod');
    await user.click(chip(dialog, 'More options'));
    await user.keyboard('{Meta>}{Enter}{/Meta}');
    await waitFor(() => expect(creates()).toHaveLength(1));
    await waitFor(() => expect(screen.queryByLabelText('Due date')).not.toBeInTheDocument());
    await new Promise((r) => setTimeout(r, 30));
    expect(within(dialog).getByRole('textbox', { name: 'Title' })).toHaveFocus();
  });

  it('Clear both (enabled): Esc then closes only the panel, and focus stays in the dialog', async () => {
    const { user, dialog } = await openDialogWith();
    await user.click(chip(dialog, 'More options'));
    await user.type(screen.getByLabelText('Estimate (points)'), '4');
    await user.click(screen.getByRole('button', { name: 'Clear both' }));
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.getByLabelText('Due date')).toHaveFocus();
    await user.keyboard('{Escape}');
    expect(screen.queryByLabelText('Due date')).not.toBeInTheDocument();
    expect(screen.getByRole('dialog', { name: 'New issue' })).toBeInTheDocument();
    expect(dialog.contains(document.activeElement)).toBe(true);
  });

  it('Clear both (already disabled): a press keeps the panel, and Esc closes only the panel', async () => {
    const { user, dialog } = await openDialogWith();
    await user.click(chip(dialog, 'More options'));
    await user.click(screen.getByRole('button', { name: 'Clear both' }));
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.getByLabelText('Due date')).toBeInTheDocument();
    await user.keyboard('{Escape}');
    expect(screen.queryByLabelText('Due date')).not.toBeInTheDocument();
    expect(screen.getByRole('dialog', { name: 'New issue' })).toBeInTheDocument();
  });

  it('focus that falls to the dialog node goes back into the open panel', async () => {
    const { user, dialog } = await openDialogWith();
    await user.click(chip(dialog, 'More options'));
    const panel = document.querySelector<HTMLElement>('[data-floating]')!;
    act(() => dialog.focus());
    await waitFor(() => expect(panel.contains(document.activeElement)).toBe(true));
    await user.keyboard('{Escape}');
    expect(screen.queryByLabelText('Due date')).not.toBeInTheDocument();
    expect(screen.getByRole('dialog', { name: 'New issue' })).toBeInTheDocument();
  });

  it('Ctrl+Enter that ends an IME composition does not create', async () => {
    const { dialog, creates } = await openDialogWith();
    const title = within(dialog).getByRole('textbox', { name: 'Title' });
    fireEvent.change(title, { target: { value: 'Kanji' } });
    fireEvent.keyDown(title, { key: 'Enter', ctrlKey: true, isComposing: true });
    fireEvent.keyDown(title, { key: 'Enter', ctrlKey: true, keyCode: 229 });
    await new Promise((r) => setTimeout(r, 30));
    expect(creates()).toHaveLength(0);
    fireEvent.keyDown(title, { key: 'Enter', ctrlKey: true });
    await waitFor(() => expect(creates()).toHaveLength(1));
  });

  it('Enter that ends an IME composition in the title does not create (Safari: keyCode 229)', async () => {
    const { user, dialog, creates } = await openDialogWith();
    const title = within(dialog).getByRole('textbox', { name: 'Title' });
    await user.type(title, 'Kanji');
    fireEvent.keyDown(title, { key: 'Enter', isComposing: true });
    fireEvent.keyDown(title, { key: 'Enter', keyCode: 229, isComposing: false });
    await new Promise((r) => setTimeout(r, 30));
    expect(creates()).toHaveLength(0);
    fireEvent.keyDown(title, { key: 'Enter' });
    await waitFor(() => expect(creates()).toHaveLength(1));
  });

  it('Esc that cancels an IME composition in the title or description does not close the dialog', async () => {
    const { dialog } = await openDialogWith();
    for (const name of ['Title', 'Description']) {
      const field = within(dialog).getByRole('textbox', { name });
      fireEvent.keyDown(field, { key: 'Escape', isComposing: true });
      fireEvent.keyDown(field, { key: 'Escape', keyCode: 229, isComposing: false });
      expect(screen.getByRole('dialog', { name: 'New issue' })).toBeInTheDocument();
    }
    fireEvent.keyDown(within(dialog).getByRole('textbox', { name: 'Title' }), { key: 'Escape' });
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });

  it('Enter and Esc that end an IME composition neither choose nor close', async () => {
    const { user, dialog } = await openDialogWith();
    await user.click(chip(dialog, 'Status: Backlog'));
    const box = screen.getByRole('combobox', { name: 'Status' });
    await user.keyboard('{ArrowDown}');
    fireEvent.keyDown(box, { key: 'Enter', isComposing: true });
    fireEvent.keyDown(box, { key: 'Enter', keyCode: 229 });
    expect(list('Status')).toBeInTheDocument();
    expect(chip(dialog, 'Status: Backlog')).toBeInTheDocument();
    fireEvent.keyDown(box, { key: 'Escape', isComposing: true });
    fireEvent.keyDown(box, { key: 'Escape', keyCode: 229 });
    expect(list('Status')).toBeInTheDocument();
    fireEvent.keyDown(box, { key: 'Enter' }); // a plain Enter still chooses
    expect(chip(dialog, 'Status: Todo')).toBeInTheDocument();
  });

  it('announces the success inside the dialog as well as in the toast', async () => {
    const { user, dialog } = await openDialogWith();
    await user.click(within(dialog).getByRole('switch', { name: 'Create more' }));
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Say it');
    await user.click(submitBtn(dialog));
    const live = await within(dialog).findByText(/^Created ORB-91\./);
    expect(live.closest('[aria-live="polite"]')).not.toBeNull();
    expect(dialog.contains(live)).toBe(true);
  });

  it('Enter in the title creates the issue; a pasted line break becomes a space', async () => {
    const { user, dialog, creates } = await openDialogWith();
    const title = within(dialog).getByRole('textbox', { name: 'Title' });
    await user.click(title);
    await user.paste('One\nTwo');
    expect(title).toHaveValue('One Two');
    await user.keyboard('{Enter}');
    await waitFor(() => expect(creates()).toHaveLength(1));
    expect(creates()[0]!.body).toMatchObject({ title: 'One Two' });
  });

  it('Cmd+Enter with an empty title shows the same error as the button', async () => {
    const { user, creates } = await openDialogWith();
    await user.keyboard('{Meta>}{Enter}{/Meta}');
    expect(await screen.findByRole('alert')).toHaveTextContent('Add a title.');
    expect(creates()).toHaveLength(0);
  });

  it('a server error shows in the dialog, keeps it open, and the retry reuses the Idempotency-Key', async () => {
    const { user, dialog, creates } = await openDialogWith({
      fail: (n) =>
        n === 1 ? { status: 500, json: { error: 'database is down', code: 'internal_error' } } : undefined,
    });
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Retry me');
    await user.click(chip(dialog, 'Priority'));
    await user.keyboard('{ArrowDown}{Enter}');
    await user.click(submitBtn(dialog));
    expect(await screen.findByRole('alert')).toHaveTextContent('database is down');
    expect(screen.getByRole('dialog', { name: 'New issue' })).toBeInTheDocument();
    expect(within(dialog).getByRole('textbox', { name: 'Title' })).toHaveValue('Retry me');
    expect(chip(dialog, /^Priority: /)).toBeInTheDocument(); // the chosen property survives the error
    await user.click(submitBtn(dialog));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    const [first, second] = creates();
    expect(second!.headers.get('Idempotency-Key')).toBe(first!.headers.get('Idempotency-Key'));
  });

  it('refuses an empty or blank title', async () => {
    const { user, dialog, creates } = await openDialogWith();
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), '   ');
    await user.click(submitBtn(dialog));
    expect(await screen.findByRole('alert')).toHaveTextContent('Add a title.');
    expect(creates()).toHaveLength(0);
  });

  it('Create more is an off switch; on, it keeps the dialog open, clears the text, keeps the properties', async () => {
    const { user, dialog, creates } = await openDialogWith();
    const more = within(dialog).getByRole('switch', { name: 'Create more' });
    expect(more).toHaveAttribute('aria-checked', 'false');
    await user.click(more);
    expect(more).toHaveAttribute('aria-checked', 'true');

    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'First');
    await user.type(within(dialog).getByRole('textbox', { name: 'Description' }), 'Body one');
    await user.click(chip(dialog, 'Priority'));
    await user.click(within(list('Priority')).getByRole('option', { name: 'Urgent' }));
    await user.click(chip(dialog, 'Labels'));
    await user.click(within(list('Labels')).getByRole('option', { name: 'bug' }));
    await user.keyboard('{Escape}');
    await user.click(submitBtn(dialog));

    await waitFor(() => expect(creates()).toHaveLength(1));
    expect(await screen.findByText('ORB-91 created')).toBeInTheDocument();
    // still open, text cleared, title focused, properties kept
    expect(screen.getByRole('dialog', { name: 'New issue' })).toBeInTheDocument();
    await waitFor(() => expect(within(dialog).getByRole('textbox', { name: 'Title' })).toHaveValue(''));
    expect(within(dialog).getByRole('textbox', { name: 'Description' })).toHaveValue('');
    expect(within(dialog).getByRole('textbox', { name: 'Title' })).toHaveFocus();
    expect(chip(dialog, 'Priority: Urgent')).toBeInTheDocument();
    expect(chip(dialog, 'Labels: bug')).toBeInTheDocument();
    expect(more).toHaveAttribute('aria-checked', 'true');

    // the second create is a NEW issue: new key, new title, same properties
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Second');
    await user.click(submitBtn(dialog));
    await waitFor(() => expect(creates()).toHaveLength(2));
    const [a, b] = creates();
    expect(b!.body).toMatchObject({ title: 'Second', priority: 1, labels: ['bug'] });
    expect(b!.body).not.toHaveProperty('description');
    expect(b!.headers.get('Idempotency-Key')).not.toBe(a!.headers.get('Idempotency-Key'));
    expect(await screen.findByText('ORB-92 created')).toBeInTheDocument();
  });

  it('with Create more on, an error keeps the text so nothing is lost', async () => {
    const { user, dialog } = await openDialogWith({
      fail: () => ({ status: 500, json: { error: 'database is down', code: 'internal_error' } }),
    });
    await user.click(within(dialog).getByRole('switch', { name: 'Create more' }));
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Keep me');
    await user.click(submitBtn(dialog));
    expect(await screen.findByRole('alert')).toHaveTextContent('database is down');
    expect(within(dialog).getByRole('textbox', { name: 'Title' })).toHaveValue('Keep me');
  });

  it('expand makes the dialog wider and back; Close closes it', async () => {
    const { user, dialog } = await openDialogWith();
    const expand = within(dialog).getByRole('button', { name: 'Make the dialog larger' });
    await user.click(expand);
    expect(dialog.style.width).toBe('min(900px, 100%)');
    await user.click(within(dialog).getByRole('button', { name: 'Make the dialog smaller' }));
    expect(dialog.style.width).toBe('min(640px, 100%)');
    await user.click(within(dialog).getByRole('button', { name: 'Close' }));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
  });
});

/** Open the dialog on a fresh mock stack with options. */
async function openDialogWith(opts: Opts = {}) {
  const stack = setup(opts);
  const opened = await openDialog();
  return { ...opened, ...stack };
}

describe('LAUN-19: project, cycle default, attach', () => {
  const nowCycle = (n: number, over: Partial<Cycle> = {}) =>
    cycle(n, {
      startsAt: new Date(Date.now() - 3 * 86400_000).toISOString(),
      endsAt: new Date(Date.now() + 4 * 86400_000).toISOString(),
      ...over,
    });
  const projectsRoute = {
    'GET /api/v1/orgs/:org/projects': () => ({
      json: { items: [{ id: 'pr1', name: 'Alpha', teams: [] }], nextCursor: null },
    }),
  } as Record<string, Handler>;

  it('Project lists the projects and sends projectId; "No project" clears it', async () => {
    const { user, dialog, creates } = await openDialogWith({ extra: projectsRoute });
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'P');
    await user.click(chip(dialog, 'Project'));
    await user.click(await screen.findByRole('option', { name: 'Alpha' }));
    expect(chip(dialog, 'Project: Alpha')).toBeInTheDocument();
    await user.click(chip(dialog, 'Project: Alpha'));
    await user.click(screen.getByRole('option', { name: 'No project' }));
    await user.click(chip(dialog, 'Project'));
    await user.click(await screen.findByRole('option', { name: 'Alpha' }));
    await user.click(submitBtn(dialog));
    await waitFor(() => expect(creates()).toHaveLength(1));
    expect(creates()[0]!.body).toMatchObject({ projectId: 'pr1' });
  });

  it('Project chip shows the project icon until one is chosen, then its logo; picker rows carry logos (LAUN-162)', async () => {
    const logo = '/api/v1/orgs/orbit/projects/pr1/avatar?v=2';
    const { user, dialog } = await openDialogWith({
      extra: {
        'GET /api/v1/orgs/:org/projects': () => ({
          json: { items: [{ id: 'pr1', name: 'Alpha', teams: [], avatarUrl: logo }], nextCursor: null },
        }),
      } as Record<string, Handler>,
    });
    expect(chip(dialog, 'Project').querySelector('img')).toBeNull();
    expect(chip(dialog, 'Project').querySelector('svg')).not.toBeNull();
    await user.click(chip(dialog, 'Project'));
    const row = await screen.findByRole('option', { name: 'Alpha' });
    expect(row.querySelector('img')).toHaveAttribute('src', logo);
    expect(row.querySelector('img')).toHaveAttribute('alt', '');
    await user.click(row);
    const img = chip(dialog, 'Project: Alpha').querySelector('img')!;
    expect(img).toHaveAttribute('src', logo);
    expect(img).toHaveAttribute('alt', '');
  });

  it('picker rows and the chosen chip show the project emoji when there is no logo (LAUN-232)', async () => {
    const { user, dialog } = await openDialogWith({
      extra: {
        'GET /api/v1/orgs/:org/projects': () => ({
          json: {
            items: [{ id: 'pr1', name: 'Alpha', teams: [], avatarUrl: null, emoji: '🎯' }],
            nextCursor: null,
          },
        }),
      } as Record<string, Handler>,
    });
    await user.click(chip(dialog, 'Project'));
    const row = await screen.findByRole('option', { name: 'Alpha' });
    expect(row.querySelector('[data-logo]')).toHaveAttribute('data-logo', 'emoji');
    expect(row).toHaveTextContent('🎯');
    await user.click(row);
    const tile = chip(dialog, 'Project: Alpha').querySelector('[data-logo]')!;
    expect(tile).toHaveAttribute('data-logo', 'emoji');
    expect(tile).toHaveTextContent('🎯');
  });

  it('preselects the running cycle, can pick another, and can clear it', async () => {
    const { user, dialog, creates } = await openDialogWith({
      cycles: [
        nowCycle(2),
        cycle(3, {
          startsAt: new Date(Date.now() + 5 * 86400_000).toISOString(),
          endsAt: new Date(Date.now() + 12 * 86400_000).toISOString(),
        }),
      ],
    });
    await within(dialog).findByRole('button', { name: 'Cycle: Cycle 2' });
    await user.click(chip(dialog, 'Cycle: Cycle 2'));
    await user.click(screen.getByRole('option', { name: /Cycle 3/ }));
    expect(chip(dialog, 'Cycle: Cycle 3')).toBeInTheDocument();
    await user.click(chip(dialog, 'Cycle: Cycle 3'));
    await user.click(screen.getByRole('option', { name: 'No cycle' }));
    // a cleared cycle is not preselected again
    expect(chip(dialog, 'Cycle')).toBeInTheDocument();
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'C');
    await user.click(submitBtn(dialog));
    await waitFor(() => expect(creates()).toHaveLength(1));
    expect(creates()[0]!.body).not.toHaveProperty('cycleId');
  });

  it('sends the preselected cycle when nothing is changed', async () => {
    const { user, dialog, creates } = await openDialogWith({ cycles: [nowCycle(2)] });
    await within(dialog).findByRole('button', { name: 'Cycle: Cycle 2' });
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'D');
    await user.click(submitBtn(dialog));
    await waitFor(() => expect(creates()).toHaveLength(1));
    expect(creates()[0]!.body).toMatchObject({ cycleId: 'cy2' });
  });

  it('has no Cycle chip when the team has cycles off', async () => {
    const { dialog } = await openDialogWith();
    expect(within(dialog).queryByRole('button', { name: /^Cycle/ })).not.toBeInTheDocument();
  });

  it('queues files until Create, then uploads them to the new issue', async () => {
    const { user, dialog, creates, api } = await openDialogWith({
      extra: {
        'PUT /up': () => ({ status: 200 }),
        'POST /api/v1/orgs/:org/attachments/prepare': () => ({
          json: {
            uploadKey: 'org/1/2026/a/note.txt',
            uploadUrl: 'https://silo.test/up',
            method: 'PUT',
            headers: { 'Content-Type': 'text/plain' },
            contentType: 'text/plain',
            sizeBytes: 2,
            expiresAt: '2026-10-26T00:00:00Z',
          },
        }),
        'POST /api/v1/orgs/:org/issues/:issue/attachments': () => ({ status: 201, json: { id: 'a1' } }),
      },
    });
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'With file');
    const input = within(dialog).getByTestId('attach-input');
    await user.upload(input, new File(['hi'], 'note.txt', { type: 'text/plain' }));
    expect(within(dialog).getByText('note.txt')).toBeInTheDocument();
    expect(api.calls.some((c) => c.url.pathname.endsWith('/attachments/prepare'))).toBe(false);
    await user.click(submitBtn(dialog));
    await waitFor(() => expect(creates()).toHaveLength(1));
    await waitFor(() =>
      expect(
        api.calls.some((c) => c.method === 'POST' && c.url.pathname.endsWith('/issues/ORB-91/attachments')),
      ).toBe(true),
    );
    expect(api.calls.filter((c) => c.method === 'PUT').map((c) => c.url.pathname)).toEqual(['/up']);
  });

  it('a file queued can be removed, and a type that is not allowed is refused', async () => {
    const { user, dialog } = await openDialogWith();
    const input = within(dialog).getByTestId('attach-input') as HTMLInputElement;
    await user.upload(input, new File(['x'], 'a.png', { type: 'image/png' }));
    await user.click(within(dialog).getByRole('button', { name: 'Remove a.png' }));
    expect(within(dialog).queryByText('a.png')).not.toBeInTheDocument();
    fireEvent.change(input, {
      target: { files: [new File(['x'], 'run.exe', { type: 'application/x-msdownload' })] },
    });
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('run.exe');
  });

  const attachRoutes = (over: Record<string, Handler> = {}): Record<string, Handler> => ({
    'PUT /up': () => ({ status: 200 }),
    'POST /api/v1/orgs/:org/attachments/prepare': () => ({
      json: {
        uploadKey: 'org/1/2026/a/note.txt',
        uploadUrl: 'https://silo.test/up',
        method: 'PUT',
        headers: { 'Content-Type': 'text/plain' },
        contentType: 'text/plain',
        sizeBytes: 2,
        expiresAt: '2026-10-26T00:00:00Z',
      },
    }),
    'POST /api/v1/orgs/:org/issues/:issue/attachments': () => ({ status: 201, json: { id: 'a1' } }),
    ...over,
  });
  const count = (api: { calls: { method: string; url: URL }[] }, method: string, tail: string) =>
    api.calls.filter((c) => c.method === method && c.url.pathname.endsWith(tail)).length;

  it('one submit at a time: button, Enter and Cmd+Enter during create and upload make one issue and one upload', async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => (release = r));
    const { user, dialog, creates, api } = await openDialogWith({
      extra: attachRoutes({
        'PUT /up': async () => {
          await gate; // the upload is slow: the create call has already answered
          return { status: 200 };
        },
      }),
    });
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Once');
    await user.upload(
      within(dialog).getByTestId('attach-input'),
      new File(['hi'], 'note.txt', { type: 'text/plain' }),
    );
    await user.click(submitBtn(dialog));
    await within(dialog).findByRole('button', { name: 'Uploading…' });
    expect(within(dialog).getByRole('button', { name: 'Uploading…' })).toBeDisabled();
    await user.click(within(dialog).getByRole('button', { name: 'Uploading…' }));
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), '{Enter}');
    await user.keyboard('{Control>}{Enter}{/Control}');
    release();
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'New issue' })).not.toBeInTheDocument());
    expect(creates()).toHaveLength(1);
    expect(count(api, 'POST', '/attachments/prepare')).toBe(1);
    expect(count(api, 'POST', '/issues/ORB-91/attachments')).toBe(1);
  });

  it('a failed upload keeps the issue, keeps the file, and a retry only attaches', async () => {
    let puts = 0;
    const { user, dialog, creates, api } = await openDialogWith({
      extra: attachRoutes({ 'PUT /up': () => ({ status: ++puts === 1 ? 500 : 200 }) }),
    });
    await user.click(within(dialog).getByRole('switch', { name: 'Create more' }));
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Half');
    await user.upload(
      within(dialog).getByTestId('attach-input'),
      new File(['hi'], 'note.txt', { type: 'text/plain' }),
    );
    await user.click(submitBtn(dialog));
    const alert = await within(dialog).findByRole('alert');
    expect(alert).toHaveTextContent('ORB-91 was created');
    expect(alert).toHaveTextContent('note.txt');
    expect(creates()).toHaveLength(1);
    expect(within(dialog).getByText('note.txt')).toBeInTheDocument(); // still queued
    expect(within(dialog).getByRole('textbox', { name: 'Title' })).toHaveValue('Half');
    await user.click(within(dialog).getByRole('button', { name: 'Retry attach' }));
    await waitFor(() => expect(count(api, 'POST', '/issues/ORB-91/attachments')).toBe(1));
    await waitFor(() => expect(within(dialog).queryByText('note.txt')).not.toBeInTheDocument());
    expect(creates()).toHaveLength(1); // no second issue
    expect(within(dialog).getByRole('textbox', { name: 'Title' })).toHaveValue(''); // Create more reset
    expect(within(dialog).getByRole('button', { name: 'Create issue' })).toBeInTheDocument();
  });

  it('closing the dialog during an upload does not close a dialog opened next', async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => (release = r));
    const { user, dialog } = await openDialogWith({
      extra: attachRoutes({
        'PUT /up': async () => {
          await gate;
          return { status: 200 };
        },
      }),
    });
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Gone');
    await user.upload(
      within(dialog).getByTestId('attach-input'),
      new File(['hi'], 'note.txt', { type: 'text/plain' }),
    );
    await user.click(submitBtn(dialog));
    await within(dialog).findByRole('button', { name: 'Uploading…' });
    await user.click(within(dialog).getByRole('button', { name: 'Close' }));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'New issue' })).not.toBeInTheDocument());
    await user.keyboard('c');
    const next = await screen.findByRole('dialog', { name: 'New issue' });
    release();
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.getByRole('dialog', { name: 'New issue' })).toBe(next);
  });

  it('refuses a file over 100 MiB', async () => {
    const { user, dialog } = await openDialogWith();
    const big = new File(['x'], 'big.pdf', { type: 'application/pdf' });
    Object.defineProperty(big, 'size', { value: 100 * 1024 * 1024 + 1 });
    await user.upload(within(dialog).getByTestId('attach-input'), big);
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('big.pdf');
    expect(within(dialog).queryByRole('list', { name: 'Files to attach' })).not.toBeInTheDocument();
  });

  it('a team change clears project and cycle, and preselects the running cycle of the new team', async () => {
    const web: Team = { ...team, id: 'team2', key: 'WEB', name: 'Website', cyclesEnabled: true };
    const mk = (id: string, key: string, n: number): Cycle => ({
      ...cycle(n),
      id,
      team: { id: 't', key, name: key },
      startsAt: new Date(Date.now() - 86400_000).toISOString(),
      endsAt: new Date(Date.now() + 86400_000).toISOString(),
    });
    const { user, dialog, creates } = await openDialogWith({
      teams: [{ ...team, cyclesEnabled: true }, web],
      extra: {
        'GET /api/v1/orgs/:org/cycles': (r) => ({
          json: {
            items: r.url.searchParams.get('team') === 'WEB' ? [mk('cyw', 'WEB', 7)] : [mk('cyo', 'ORB', 2)],
            nextCursor: null,
          },
        }),
        'GET /api/v1/orgs/:org/projects': () => ({
          json: { items: [{ id: 'pr1', name: 'Alpha', state: 'started', teams: [] }], nextCursor: null },
        }),
      },
    });
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Move');
    await within(dialog).findByRole('button', { name: 'Cycle: Cycle 2' });
    await user.click(chip(dialog, 'Project'));
    await user.click(await screen.findByRole('option', { name: 'Alpha' }));
    await user.click(chip(dialog, /^Team/));
    await user.click(within(list('Team')).getByRole('option', { name: /Website/ }));
    await within(dialog).findByRole('button', { name: 'Cycle: Cycle 7' });
    expect(chip(dialog, 'Project')).toBeInTheDocument();
    await user.click(submitBtn(dialog));
    await waitFor(() => expect(creates()).toHaveLength(1));
    expect(creates()[0]!.body).toMatchObject({ team: 'WEB', cycleId: 'cyw' });
    expect(creates()[0]!.body).not.toHaveProperty('projectId');
  });

  it('does not offer completed or canceled projects', async () => {
    const { user, dialog } = await openDialogWith({
      extra: {
        'GET /api/v1/orgs/:org/projects': () => ({
          json: {
            items: [
              { id: 'p1', name: 'Live', state: 'started', teams: [] },
              { id: 'p2', name: 'Shipped', state: 'completed', teams: [] },
              { id: 'p3', name: 'Dropped', state: 'canceled', teams: [] },
            ],
            nextCursor: null,
          },
        }),
      },
    });
    await user.click(chip(dialog, 'Project'));
    await screen.findByRole('option', { name: 'Live' });
    expect(screen.queryByRole('option', { name: 'Shipped' })).not.toBeInTheDocument();
    expect(screen.queryByRole('option', { name: 'Dropped' })).not.toBeInTheDocument();
  });

  it('one submit at a time while Creating…: button, Enter and Cmd+Enter make one create call', async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => (release = r));
    let posts = 0;
    const { user, dialog, creates } = await openDialogWith({
      extra: {
        'POST /api/v1/orgs/:org/issues': async (r) => {
          posts++;
          await gate; // the create call is slow
          return {
            status: 201,
            json: makeIssue(91, { title: (r.body as { title: string }).title, identifier: 'ORB-91' }),
          };
        },
      },
    });
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Slow');
    await user.click(submitBtn(dialog));
    const busyBtn = await within(dialog).findByRole('button', { name: 'Creating…' });
    expect(busyBtn).toBeDisabled();
    await user.click(busyBtn);
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), '{Enter}');
    await user.keyboard('{Control>}{Enter}{/Control}');
    fireEvent.submit(dialog.querySelector('form')!);
    release();
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'New issue' })).not.toBeInTheDocument());
    expect(posts).toBe(1);
    expect(creates()).toHaveLength(1);
  });

  it('files cannot be added or removed while Creating… or Uploading…', async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => (release = r));
    const { user, dialog } = await openDialogWith({
      extra: attachRoutes({
        'POST /api/v1/orgs/:org/issues': async () => {
          await gate;
          return { status: 201, json: makeIssue(91, { title: 'Busy', identifier: 'ORB-91' }) };
        },
      }),
    });
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Busy');
    const input = within(dialog).getByTestId('attach-input');
    await user.upload(input, new File(['hi'], 'one.txt', { type: 'text/plain' }));
    await user.click(submitBtn(dialog));
    await within(dialog).findByRole('button', { name: 'Creating…' });
    expect(within(dialog).getByRole('button', { name: 'Attach files' })).toBeDisabled();
    expect(within(dialog).getByRole('button', { name: 'Remove one.txt' })).toBeDisabled();
    // a file chosen anyway (the input is hidden) is ignored
    fireEvent.change(input, { target: { files: [new File(['x'], 'late.txt', { type: 'text/plain' })] } });
    release();
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'New issue' })).not.toBeInTheDocument());
  });

  it('two files with one name: only the failed one stays; the issue is locked; a close names the file', async () => {
    let puts = 0;
    const { user, dialog, creates } = await openDialogWith({
      extra: attachRoutes({ 'PUT /up': () => ({ status: ++puts === 1 ? 500 : 200 }) }),
    });
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Twins');
    const input = within(dialog).getByTestId('attach-input');
    await user.upload(input, [
      new File(['aa'], 'same.txt', { type: 'text/plain' }),
      new File(['bb'], 'same.txt', { type: 'text/plain' }),
    ]);
    await user.click(submitBtn(dialog));
    await within(dialog).findByRole('alert');
    // exactly one of the two stays (the first one failed)
    expect(within(dialog).getAllByText('same.txt')).toHaveLength(1);
    expect(creates()).toHaveLength(1);
    // locked: title read-only, chips inert
    expect(within(dialog).getByRole('textbox', { name: 'Title' })).toHaveAttribute('readonly');
    expect(within(dialog).getByRole('textbox', { name: 'Description' })).toHaveAttribute('readonly');
    expect(within(dialog).getByRole('group', { name: 'Properties' })).toHaveAttribute('inert');
    await user.click(within(dialog).getByRole('button', { name: 'Close' }));
    expect(await screen.findByText(/ORB-91 was created\. Not attached: same\.txt/)).toBeInTheDocument();
  });

  const keyOf = (c: { headers: Headers }) => c.headers.get('idempotency-key');

  it('after a 400 the corrected form is sent with a NEW Idempotency-Key', async () => {
    const { user, dialog, creates } = await openDialogWith({
      fail: (n) =>
        n === 1 ? { status: 400, json: { error: 'bad title', code: 'validation_failed' } } : undefined,
    });
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'First');
    await user.click(submitBtn(dialog));
    await waitFor(() => expect(creates()).toHaveLength(1));
    await within(dialog).findByRole('alert');
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), ' fixed');
    await user.click(submitBtn(dialog));
    await waitFor(() => expect(creates()).toHaveLength(2));
    expect(keyOf(creates()[1]!)).not.toBe(keyOf(creates()[0]!));
  });

  it('after a 500 the retry keeps the same Idempotency-Key', async () => {
    const { user, dialog, creates } = await openDialogWith({
      fail: (n) => (n === 1 ? { status: 500, json: { error: 'boom', code: 'internal' } } : undefined),
    });
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Again');
    await user.click(submitBtn(dialog));
    await within(dialog).findByRole('alert');
    await user.click(submitBtn(dialog));
    await waitFor(() => expect(creates()).toHaveLength(2));
    expect(keyOf(creates()[1]!)).toBe(keyOf(creates()[0]!));
  });

  it('a create that fails after the dialog was closed shows a toast', async () => {
    let release: () => void = () => {};
    const gate = new Promise<void>((r) => (release = r));
    const { user, dialog } = await openDialogWith({
      extra: {
        'POST /api/v1/orgs/:org/issues': async () => {
          await gate;
          return { status: 400, json: { error: 'Title is not allowed', code: 'validation_failed' } };
        },
      },
    });
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Lost');
    await user.click(submitBtn(dialog));
    await within(dialog).findByRole('button', { name: 'Creating…' });
    await user.click(within(dialog).getByRole('button', { name: 'Close' }));
    release();
    expect(await screen.findByText(/Could not create "Lost": Title is not allowed/)).toBeInTheDocument();
  });

  it('locking the form moves focus out of the chip row; Esc still closes the dialog', async () => {
    const { user, dialog } = await openDialogWith({
      extra: attachRoutes({ 'PUT /up': () => ({ status: 500 }) }),
    });
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Focus');
    await user.upload(
      within(dialog).getByTestId('attach-input'),
      new File(['hi'], 'f.txt', { type: 'text/plain' }),
    );
    const pri = chip(dialog, 'Priority');
    act(() => pri.focus());
    expect(pri).toHaveFocus();
    await user.keyboard('{Control>}{Enter}{/Control}');
    await within(dialog).findByRole('alert');
    // jsdom has no inert: the proof is that the code moved focus before it locked the row
    expect(within(dialog).getByRole('group', { name: 'Properties' })).toHaveAttribute('inert');
    expect(document.activeElement).not.toBe(document.body);
    expect(dialog.contains(document.activeElement)).toBe(true);
    expect(within(dialog).getByRole('textbox', { name: 'Title' })).toHaveFocus();
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'New issue' })).not.toBeInTheDocument());
  });
});

describe('the label picker in the create dialog (LAUN-53)', () => {
  it('has the box, ticks several labels and keeps the dialog list open', async () => {
    const { user, dialog } = await openDialogWith();
    await user.click(chip(dialog, 'Labels'));
    const box = await screen.findByRole('combobox', { name: 'Labels' });
    expect(box).toHaveFocus();
    await user.keyboard('{Enter}');
    expect(chip(dialog, 'Labels: bug')).toBeInTheDocument();
    expect(screen.getByRole('listbox', { name: 'Labels' })).toBeInTheDocument();
    await user.keyboard('{Escape}');
    expect(chip(dialog, 'Labels: bug')).toHaveFocus();
  });
});

describe('Suggestions in the create dialog (LAUN-53)', () => {
  it('shows Suggestions once a project is chosen, from the labels used in that project', async () => {
    const bug = { id: 'l1', name: 'bug', color: '#d6481f' };
    const { user, dialog } = await openDialogWith({
      extra: {
        'GET /api/v1/orgs/:org/projects': () => ({
          json: { items: [{ id: 'pr1', name: 'Alpha', teams: [] }], nextCursor: null },
        }),
        'GET /api/v1/orgs/:org/labels': () => ({
          json: {
            items: [
              { ...labels[0]!, ...bug },
              { ...labels[0]!, id: 'l2', name: 'ios' },
            ],
            nextCursor: null,
          },
        }),
        'GET /api/v1/orgs/:org/issues': (r) => ({
          json: {
            items:
              r.url.searchParams.get('project') === 'pr1'
                ? [makeIssue(5, { projectId: 'pr1', labels: [bug] })]
                : [makeIssue(1)],
            nextCursor: null,
          },
        }),
      },
    });
    const heads = () =>
      [...document.querySelectorAll('[role="listbox"] > li[aria-hidden="true"]')].map((n) => n.textContent);
    await user.click(chip(dialog, 'Labels'));
    await screen.findByRole('option', { name: 'ios' });
    expect(heads()).toEqual([]); // no project: one group, no heading
    await user.keyboard('{Escape}');
    await user.click(chip(dialog, 'Project'));
    await user.click(await screen.findByRole('option', { name: 'Alpha' }));
    await user.click(chip(dialog, 'Labels'));
    await waitFor(() => expect(heads()).toEqual(['Suggestions', 'Org labels']));
  });
});

// LAUN-58: files dropped on the dialog queue exactly as the paperclip does.
describe('LAUN-58: drop files on the create dialog', () => {
  const dt = (files: File[], types: string[] = ['Files']) => ({
    types,
    files,
    dropEffect: 'none',
  });
  const txt = (name: string) => new File(['hi'], name, { type: 'text/plain' });
  const routes = (over: Record<string, Handler> = {}): Record<string, Handler> => ({
    'PUT /up': () => ({ status: 200 }),
    'POST /api/v1/orgs/:org/attachments/prepare': () => ({
      json: {
        uploadKey: 'org/1/2026/a/note.txt',
        uploadUrl: 'https://silo.test/up',
        method: 'PUT',
        headers: { 'Content-Type': 'text/plain' },
        contentType: 'text/plain',
        sizeBytes: 2,
        expiresAt: '2026-10-26T00:00:00Z',
      },
    }),
    'POST /api/v1/orgs/:org/issues/:issue/attachments': () => ({ status: 201, json: { id: 'a1' } }),
    ...over,
  });

  it('shows the overlay for a file drag, keeps it over nested children, clears when the drag leaves', async () => {
    const { dialog } = await openDialogWith({ extra: routes() });
    const title = within(dialog).getByRole('textbox', { name: 'Title' });
    expect(screen.queryByTestId('drop-overlay')).not.toBeInTheDocument();
    fireEvent.dragEnter(dialog, { dataTransfer: dt([]) });
    expect(screen.getByTestId('drop-overlay')).toHaveTextContent('Drop files to attach');
    fireEvent.dragEnter(title, { dataTransfer: dt([]) }); // a child: the parent leaves, the child enters
    fireEvent.dragLeave(dialog, { dataTransfer: dt([]) });
    expect(screen.getByTestId('drop-overlay')).toBeInTheDocument();
    fireEvent.dragLeave(title, { dataTransfer: dt([]) });
    expect(screen.queryByTestId('drop-overlay')).not.toBeInTheDocument();
  });

  it('shows the overlay on a dragover when the dragenter was missed', async () => {
    const { dialog } = await openDialogWith({ extra: routes() });
    fireEvent.dragOver(dialog, { dataTransfer: dt([]) });
    expect(screen.getByTestId('drop-overlay')).toBeInTheDocument();
    fireEvent.dragLeave(dialog, { dataTransfer: dt([]) });
    expect(screen.queryByTestId('drop-overlay')).not.toBeInTheDocument();
  });

  it('ignores a drag of text or a link: no overlay, no preventDefault', async () => {
    const { dialog } = await openDialogWith({ extra: routes() });
    fireEvent.dragEnter(dialog, { dataTransfer: dt([], ['text/plain', 'text/uri-list']) });
    expect(screen.queryByTestId('drop-overlay')).not.toBeInTheDocument();
    const over = fireEvent.dragOver(dialog, { dataTransfer: dt([], ['text/plain']) });
    expect(over).toBe(true); // not cancelled
    fireEvent.drop(dialog, { dataTransfer: dt([], ['text/plain']) });
    expect(within(dialog).queryByRole('list', { name: 'Files to attach' })).not.toBeInTheDocument();
  });

  it('clears on Escape, on dragend and on a drop elsewhere', async () => {
    const { dialog } = await openDialogWith({ extra: routes() });
    for (const end of [
      () => fireEvent.keyDown(window, { key: 'Escape' }),
      () => fireEvent.dragEnd(window),
      () => fireEvent.drop(document.body, { dataTransfer: dt([txt('x.txt')]) }),
    ]) {
      fireEvent.dragEnter(dialog, { dataTransfer: dt([]) });
      expect(screen.getByTestId('drop-overlay')).toBeInTheDocument();
      end();
      expect(screen.queryByTestId('drop-overlay')).not.toBeInTheDocument();
    }
    // a file dropped outside any zone is cancelled, so the browser does not open it
    expect(fireEvent.drop(document.body, { dataTransfer: dt([txt('x.txt')]) })).toBe(false);
    expect(within(dialog).queryByRole('list', { name: 'Files to attach' })).not.toBeInTheDocument();
  });

  it('a drop queues several files like the paperclip, refused ones are named, and create uploads them', async () => {
    const { user, dialog, api } = await openDialogWith({ extra: routes() });
    fireEvent.dragEnter(dialog, { dataTransfer: dt([]) });
    const over = fireEvent.dragOver(dialog, { dataTransfer: dt([]) });
    expect(over).toBe(false); // cancelled: the drop is allowed
    fireEvent.drop(dialog, {
      dataTransfer: dt([
        txt('a.txt'),
        txt('b.txt'),
        new File(['x'], 'run.exe', { type: 'application/x-msdownload' }),
      ]),
    });
    expect(screen.queryByTestId('drop-overlay')).not.toBeInTheDocument();
    const queue = within(dialog).getByRole('list', { name: 'Files to attach' });
    expect(within(queue).getByText('a.txt')).toBeInTheDocument();
    expect(within(queue).getByText('b.txt')).toBeInTheDocument();
    expect(within(queue).queryByText('run.exe')).not.toBeInTheDocument();
    expect(within(dialog).getByRole('alert')).toHaveTextContent('Not attached: run.exe (type not allowed');
    await user.click(within(queue).getByRole('button', { name: 'Remove b.txt' }));
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Dropped');
    await user.click(submitBtn(dialog));
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'New issue' })).not.toBeInTheDocument());
    expect(
      api.calls.filter((c) => c.method === 'POST' && c.url.pathname.endsWith('/issues/ORB-91/attachments')),
    ).toHaveLength(1);
  });

  it('a dropped file over 100 MiB is refused by name and reason', async () => {
    const { dialog } = await openDialogWith({ extra: routes() });
    const big = new File(['x'], 'big.pdf', { type: 'application/pdf' });
    Object.defineProperty(big, 'size', { value: 100 * 1024 * 1024 + 1 });
    fireEvent.drop(dialog, { dataTransfer: dt([big]) });
    expect(await within(dialog).findByRole('alert')).toHaveTextContent('big.pdf (larger than 100 MiB)');
    expect(within(dialog).queryByRole('list', { name: 'Files to attach' })).not.toBeInTheDocument();
  });

  it('when attachments are off the retry banner says so, with a fixed message', async () => {
    const { user, dialog } = await openDialogWith({
      extra: routes({
        'POST /api/v1/orgs/:org/attachments/prepare': () => ({
          status: 503,
          json: { error: 'attachments are off', code: 'attachments_unavailable' },
        }),
      }),
    });
    fireEvent.drop(dialog, { dataTransfer: dt([txt('a.txt')]) });
    await user.type(within(dialog).getByRole('textbox', { name: 'Title' }), 'Off');
    await user.click(submitBtn(dialog));
    expect(
      await within(dialog).findByText(/Attachments are turned off on this server\./),
    ).toBeInTheDocument();
  });
});
