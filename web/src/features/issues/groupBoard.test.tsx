import { act, fireEvent, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Issue, Project } from '../../api/types';
import { at, middle, stubBoardLayout } from '../../test/boardLayout';
import { baseRoutes, makeIssue, mockApi, states } from '../../test/mockApi';
import { currentLocation, renderApp } from '../../test/render';
import { resetDragged } from './cardLink';
import { STORAGE_PREFIX } from './viewPrefs';

// LAUN-171: the board groups its columns by status (as before), assignee, priority, label or project.
// Real pointer sequences; the assertions are on the PATCH body the page sends.

const ref = (name: string) => {
  const s = states.find((x) => x.name === name)!;
  return { id: s.id, name: s.name, type: s.type, color: s.color };
};
const chen = { id: 'u1', username: 'chen', name: 'Chen' };
const claude = { id: 'u2', username: 'claude', name: 'Claude' };
const PID = '01a00000-0000-7000-8000-000000000001';
const proj = (id: string, name: string) => ({ id, name, archivedAt: null }) as unknown as Project;
const label = { id: 'l1', name: 'bug', color: '#d6481f' };
const card = (n: number, over: Partial<Issue> = {}) =>
  makeIssue(n, { title: `Card ${n}`, state: ref('Todo'), ...over });

function setup(rows: Issue[], extra: Record<string, () => { json: unknown }> = {}) {
  return mockApi({
    ...baseRoutes(),
    'GET /api/v1/orgs/:org/issues': () => ({ json: { items: rows, nextCursor: null } }),
    'GET /api/v1/orgs/:org/projects': () => ({
      json: { items: [proj(PID, 'Web rebuild'), proj('p2', 'Mobile')], nextCursor: null },
    }),
    'PATCH /api/v1/orgs/:org/issues/:issue': (r) => ({
      json: { ...rows.find((x) => x.identifier === r.params.issue)!, version: 2 },
    }),
    'PUT /api/v1/orgs/:org/me/views/:place': (r) => ({
      json: { place: r.params.place, ...(r.body as object) },
    }),
    ...extra,
  });
}

const cardEl = (n: number) => document.querySelector(`[data-issue-id="i${n}"]`) as HTMLElement;
const patches = (api: ReturnType<typeof mockApi>) => api.calls.filter((c) => c.method === 'PATCH');
const settle = () => act(() => new Promise<void>((r) => setTimeout(r, 330)));
const ptr = { pointerId: 1, pointerType: 'mouse', isPrimary: true };
/** A mouse drag. `mod` holds Ctrl or Command at the release; `stop` ends the test before the release. */
function drag(
  n: number,
  grab: { x: number; y: number },
  to: { x: number; y: number },
  opts: { mod?: 'ctrl' | 'meta'; stop?: boolean } = {},
) {
  fireEvent.pointerDown(cardEl(n), { ...ptr, button: 0, clientX: grab.x, clientY: grab.y });
  fireEvent.pointerMove(window, { ...ptr, clientX: grab.x + 10, clientY: grab.y + 10 });
  fireEvent.pointerMove(window, { ...ptr, clientX: to.x, clientY: to.y });
  if (opts.stop) return;
  fireEvent.pointerUp(window, {
    ...ptr,
    clientX: to.x,
    clientY: to.y,
    ctrlKey: opts.mod === 'ctrl',
    metaKey: opts.mod === 'meta',
  });
}
const storedPlaces = () => {
  const k = Object.keys(localStorage).find((x) => x.startsWith(STORAGE_PREFIX));
  return k ? (JSON.parse(localStorage.getItem(k)!) as Record<string, Record<string, unknown>>) : {};
};

let restore: () => void;
beforeEach(() => {
  localStorage.clear();
  document.cookie = 'odt_csrf=csrf-test; path=/';
  restore = stubBoardLayout();
  resetDragged();
});
afterEach(() => {
  restore();
  vi.restoreAllMocks();
});

describe('group by assignee', () => {
  // Columns: Chen (0), Claude (1), Unassigned (2).
  const rows = () => [card(2, { assignee: chen }), card(3), card(4, { assignee: claude })];

  it('shows a column per member and one for Unassigned', async () => {
    setup(rows());
    renderApp('/orbit/issues?view=board&group=assignee');
    await screen.findByText('Card 2');
    expect(
      Array.from(document.querySelectorAll('section[data-column]')).map((s) => s.getAttribute('data-name')),
    ).toEqual(['Chen', 'Claude', 'Unassigned']);
    expect(screen.getByRole('combobox', { name: 'Group by' })).toHaveValue('assignee');
  });

  it('a drop on another member sets the assignee and nothing else', async () => {
    const api = setup(rows());
    renderApp('/orbit/issues?view=board&group=assignee');
    await screen.findByText('Card 2');
    drag(2, middle(0, 0), at(1, 1));
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ assignee: 'claude', version: 1 });
  });

  it('a drop on Unassigned clears the assignee', async () => {
    const api = setup(rows());
    renderApp('/orbit/issues?view=board&group=assignee');
    await screen.findByText('Card 2');
    drag(2, middle(0, 0), at(2, 1));
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ assignee: null, version: 1 });
  });

  it('a drop in the card own column sends nothing', async () => {
    const api = setup([...rows(), card(5)]);
    renderApp('/orbit/issues?view=board&group=assignee');
    await screen.findByText('Card 2');
    drag(3, middle(2, 0), at(2, 2));
    await settle();
    expect(patches(api)).toHaveLength(0);
    expect(screen.getByTestId('board-live')).toHaveTextContent('Reordering inside Unassigned is not saved');
  });

  it('in a sort that is not Manual, Ctrl on the drop also takes the priority of the card it lands next to', async () => {
    // Claude has Card 4 (High, 2) and Card 5 (Low, 4): sorted by priority. Card 2 lands under Card 4.
    const api = setup([
      card(2, { assignee: chen, priority: 1, priorityLabel: 'Urgent' }),
      card(4, { assignee: claude, priority: 2, priorityLabel: 'High' }),
      card(5, { assignee: claude, priority: 4, priorityLabel: 'Low' }),
    ]);
    renderApp('/orbit/issues?view=board&group=assignee&sort=priority');
    await screen.findByText('Card 2');
    drag(2, middle(0, 0), at(1, 1), { mod: 'ctrl' });
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ assignee: 'claude', priority: 2, version: 1 });
  });
});

describe('group by priority', () => {
  // Columns: Urgent (0), High (1), Medium (2), Low (3), No priority (4).
  it('shows the five priorities, including No priority', async () => {
    setup([card(2)]);
    renderApp('/orbit/issues?view=board&group=priority');
    await screen.findByText('Card 2');
    expect(
      Array.from(document.querySelectorAll('section[data-column]')).map((s) => s.getAttribute('data-name')),
    ).toEqual(['Urgent', 'High', 'Medium', 'Low', 'No priority']);
  });

  it('a drop on a priority column sets the priority', async () => {
    const api = setup([card(2), card(3, { priority: 3, priorityLabel: 'Medium' })]);
    renderApp('/orbit/issues?view=board&group=priority');
    await screen.findByText('Card 2');
    drag(2, middle(4, 0), at(1, 1));
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ priority: 2, version: 1 });
  });

  it('a drop on No priority sets 0', async () => {
    const api = setup([card(3, { priority: 3, priorityLabel: 'Medium' })]);
    renderApp('/orbit/issues?view=board&group=priority');
    await screen.findByText('Card 3');
    drag(3, middle(2, 0), at(4, 1));
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ priority: 0, version: 1 });
  });

  it('Ctrl does nothing extra here and the hint does not offer it', async () => {
    const api = setup([card(2), card(3, { priority: 3, priorityLabel: 'Medium' })]);
    renderApp('/orbit/issues?view=board&group=priority&sort=created');
    await screen.findByText('Card 2');
    drag(2, middle(4, 0), at(2, 1), { mod: 'ctrl', stop: true });
    expect(screen.getByTestId('drag-hint')).toHaveTextContent('Board ordered by Created');
    expect(screen.getByTestId('drag-hint')).not.toHaveTextContent('to also change priority');
    fireEvent.pointerUp(window, { ...ptr, clientX: at(2, 1).x, clientY: at(2, 1).y, ctrlKey: true });
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ priority: 3, version: 1 });
  });
});

describe('group by label and project: drag is off', () => {
  it('label: columns show, a card does not drag, the page says why, the card still opens', async () => {
    const api = setup([card(2, { labels: [label] }), card(3)]);
    renderApp('/orbit/issues?view=board&group=label');
    await screen.findByText('Card 2');
    expect(
      Array.from(document.querySelectorAll('section[data-column]')).map((s) => s.getAttribute('data-name')),
    ).toEqual(['bug', 'No label']);
    expect(screen.getByTestId('drag-off-hint')).toHaveTextContent(
      'Dragging is off when the board is grouped by label',
    );
    drag(2, middle(0, 0), at(1, 1), { stop: true });
    expect(screen.queryByTestId('drag-hint')).toBeNull();
    expect(document.querySelector('[class*="dragClone"]')).toBeNull();
    fireEvent.pointerUp(window, { ...ptr, clientX: at(1, 1).x, clientY: at(1, 1).y });
    await settle();
    expect(patches(api)).toHaveLength(0);
    await userEvent.click(screen.getByRole('link', { name: 'Card 2' }));
    await waitFor(() => expect(currentLocation.pathname).toBe('/orbit/issue/ORB-2'));
  });

  it('project: the same, with a column for No project', async () => {
    const api = setup([card(2, { projectId: PID, priority: 2, priorityLabel: 'High' }), card(3)]);
    renderApp('/orbit/issues?view=board&group=project');
    await screen.findByText('Card 2');
    await waitFor(() =>
      expect(
        Array.from(document.querySelectorAll('section[data-column]')).map((s) => s.getAttribute('data-name')),
      ).toEqual(['Mobile', 'Web rebuild', 'No project']),
    );
    expect(screen.getByTestId('drag-off-hint')).toHaveTextContent(
      'Dragging is off when the board is grouped by project',
    );
    drag(2, middle(1, 0), at(2, 1), { stop: true });
    expect(document.querySelector('[data-drag-source]')).toBeNull(); // no drag began
    fireEvent.pointerUp(window, { ...ptr, clientX: at(2, 1).x, clientY: at(2, 1).y });
    await settle();
    expect(patches(api)).toHaveLength(0);
  });
});

describe('state columns: Ctrl or Command also changes the priority (non-Manual sorts)', () => {
  // Backlog (0) is empty. Todo (1): 2 (Urgent), 3 (High), 4 (Low); In Progress (2): 5 (No priority).
  const rows = () => [
    card(2, { priority: 1, priorityLabel: 'Urgent' }),
    card(3, { priority: 2, priorityLabel: 'High' }),
    card(4, { priority: 4, priorityLabel: 'Low' }),
    card(5, { state: ref('In Progress') }),
  ];

  it('without the key a drop to another column sends the state only', async () => {
    const api = setup(rows());
    renderApp('/orbit/issues?view=board&sort=priority');
    await screen.findByText('Card 2');
    drag(5, middle(2, 0), at(1, 2));
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ state: 'Todo', version: 1 });
  });

  it.each(['ctrl', 'meta'] as const)(
    'with %s the card also takes the priority of the card above its place',
    async (mod) => {
      const api = setup(rows());
      renderApp('/orbit/issues?view=board&sort=priority');
      await screen.findByText('Card 2');
      drag(5, middle(2, 0), at(1, 2), { mod }); // between Card 3 (High) and Card 4 (Low)
      await waitFor(() => expect(patches(api)).toHaveLength(1));
      expect(patches(api)[0]!.body).toEqual({ state: 'Todo', priority: 2, version: 1 });
    },
  );

  it('at the top of a column the card below gives the priority', async () => {
    const api = setup(rows());
    renderApp('/orbit/issues?view=board&sort=priority');
    await screen.findByText('Card 2');
    drag(5, middle(2, 0), at(1, 0), { mod: 'ctrl' });
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ state: 'Todo', priority: 1, version: 1 });
  });

  it('in the same column the key changes the priority alone; without it nothing is sent', async () => {
    const api = setup(rows());
    renderApp('/orbit/issues?view=board&sort=priority');
    await screen.findByText('Card 2');
    drag(2, middle(1, 0), at(1, 3)); // below Card 4, no key
    await settle();
    expect(patches(api)).toHaveLength(0);
    drag(2, middle(1, 0), at(1, 3), { mod: 'ctrl' });
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ priority: 4, version: 1 });
  });

  it('in Manual sort the key changes nothing: the drop only places the card', async () => {
    const api = setup(rows());
    renderApp('/orbit/issues?view=board&sort=manual');
    await screen.findByText('Card 2');
    drag(5, middle(2, 0), at(1, 2), { mod: 'ctrl' });
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).not.toHaveProperty('priority');
  });
});

describe('the hint over a drag', () => {
  const rows = () => [card(2), card(3, { state: ref('In Progress') })];
  const platform = (p: string) => vi.spyOn(navigator, 'platform', 'get').mockReturnValue(p);

  it('names the sort and the key, and goes away after the drop', async () => {
    platform('MacIntel');
    setup(rows());
    renderApp('/orbit/issues?view=board&sort=created');
    await screen.findByText('Card 2');
    expect(screen.queryByTestId('drag-hint')).toBeNull();
    drag(2, middle(0, 0), at(1, 1), { stop: true });
    const hint = await screen.findByTestId('drag-hint');
    expect(hint).toHaveTextContent('Board ordered by Created');
    expect(hint).toHaveTextContent('Hold ⌘ to also change priority');
    fireEvent.pointerUp(window, { ...ptr, clientX: at(1, 1).x, clientY: at(1, 1).y });
    await settle();
    await waitFor(() => expect(screen.queryByTestId('drag-hint')).toBeNull());
  });

  it('says Ctrl off Apple systems', async () => {
    platform('Linux x86_64');
    setup(rows());
    renderApp('/orbit/issues?view=board&sort=priority');
    await screen.findByText('Card 2');
    drag(2, middle(0, 0), at(1, 1), { stop: true });
    expect(await screen.findByTestId('drag-hint')).toHaveTextContent('Hold Ctrl to also change priority');
    fireEvent.pointerUp(window, { ...ptr, clientX: 0, clientY: 0 }); // outside: a cancel
    await settle();
  });

  it('is not shown in Manual sort', async () => {
    setup(rows());
    renderApp('/orbit/issues?view=board&sort=manual');
    await screen.findByText('Card 2');
    drag(2, middle(0, 0), at(1, 1), { stop: true });
    expect(screen.queryByTestId('drag-hint')).toBeNull();
    fireEvent.pointerUp(window, { ...ptr, clientX: at(1, 1).x, clientY: at(1, 1).y });
    await settle();
  });
});

describe('the group-by is part of the remembered view', () => {
  it('choosing it writes ?group=, stores it with the place and sends it to the server', async () => {
    const api = setup([card(2, { assignee: chen })]);
    renderApp('/orbit/team/ORB?view=board');
    await screen.findByText('Card 2');
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Group by' }), 'assignee');
    await waitFor(() => expect(currentLocation.search).toContain('group=assignee'));
    await waitFor(() =>
      expect(storedPlaces()['team:orb']).toMatchObject({ view: 'board', groupBy: 'assignee' }),
    );
    await waitFor(() =>
      expect(
        api.calls.find((c) => c.method === 'PUT' && decodeURIComponent(c.url.pathname).endsWith('/team:orb'))
          ?.body,
      ).toMatchObject({
        groupBy: 'assignee',
      }),
    );
    // Back to Status: the address drops the parameter.
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Group by' }), 'state');
    await waitFor(() => expect(currentLocation.search).not.toContain('group='));
  });

  it('comes back when the person returns, and the address wins over it', async () => {
    setup([card(2, { assignee: chen })]);
    const first = renderApp('/orbit/team/ORB?view=board');
    await screen.findByText('Card 2');
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Group by' }), 'priority');
    await waitFor(() => expect(storedPlaces()['team:orb']).toMatchObject({ groupBy: 'priority' }));
    first.unmount();
    renderApp('/orbit/team/ORB');
    await screen.findByText('Card 2');
    expect(screen.getByRole('combobox', { name: 'Group by' })).toHaveValue('priority');
    expect(currentLocation.search).toBe('');
  });

  it('a new device gets it from the server', async () => {
    setup([card(2, { assignee: chen })], {
      'GET /api/v1/orgs/:org/me/views': () => ({
        json: {
          items: [
            {
              place: 'team:orb',
              view: 'board',
              sort: 'manual',
              hideEmpty: false,
              hiddenColumns: [],
              groupBy: 'assignee',
              at: 5,
            },
          ],
        },
      }),
    });
    renderApp('/orbit/team/ORB');
    await waitFor(() => expect(screen.getByRole('combobox', { name: 'Group by' })).toHaveValue('assignee'));
    await waitFor(() => expect(document.querySelector('[data-name="Unassigned"]')).not.toBeNull());
  });

  it('Reset view goes back to Status', async () => {
    setup([card(2)]);
    renderApp('/orbit/team/ORB?view=board&group=assignee');
    await screen.findByText('Card 2');
    await userEvent.click(screen.getByRole('button', { name: 'Reset view' }));
    await waitFor(() => expect(currentLocation.search).toBe(''));
    expect(storedPlaces()['team:orb']).toBeUndefined();
  });
});

describe('hidden columns are kept per group kind', () => {
  it('hiding an assignee column does not hide a state column, and each kind restores on its own', async () => {
    setup([card(2, { assignee: chen }), card(4, { assignee: claude })]);
    renderApp('/orbit/team/ORB?view=board&group=assignee');
    await screen.findByText('Card 2');
    await userEvent.click(screen.getByRole('button', { name: /^Columns/ }));
    await userEvent.click(screen.getByRole('checkbox', { name: /^Show Claude/ }));
    await waitFor(() => expect(document.querySelector('[data-name="Claude"]')).toBeNull());
    expect(storedPlaces()['team:orb']).toMatchObject({ hidden: ['assignee:claude'] });
    // Status again: every state column is there.
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Group by' }), 'state');
    await waitFor(() => expect(document.querySelector('[data-name="Todo"]')).not.toBeNull());
    expect(document.querySelector('[data-name="Done"]')).not.toBeNull();
    // And back to Assignee: Claude is still hidden.
    await userEvent.selectOptions(screen.getByRole('combobox', { name: 'Group by' }), 'assignee');
    await waitFor(() => expect(document.querySelector('[data-name="Chen"]')).not.toBeNull());
    expect(document.querySelector('[data-name="Claude"]')).toBeNull();
    expect(screen.getByTestId('board-hidden')).toHaveTextContent('1 column hidden');
  });
});

describe('polish (LAUN-171)', () => {
  const gaps = () => Array.from(document.querySelectorAll<HTMLElement>('[data-gap]'));

  it('Manual sort: an assignee column draws no drop gap and the cards do not make room; a state column does', async () => {
    setup([card(2, { assignee: chen }), card(3, { assignee: claude }), card(4, { assignee: claude })]);
    renderApp('/orbit/issues?view=board&group=assignee&sort=manual');
    await screen.findByText('Card 2');
    expect(gaps()).toHaveLength(0);
    drag(2, middle(0, 0), at(1, 1), { stop: true });
    expect(document.querySelector<HTMLElement>('[data-issue-id="i3"]')!.style.transform).toBe('');
    fireEvent.pointerUp(window, { ...ptr, clientX: 0, clientY: 0 });
    await settle();
  });

  it('Manual sort: a state column still draws the gap', async () => {
    setup([card(2), card(3), card(4)]);
    renderApp('/orbit/issues?view=board&sort=manual');
    await screen.findByText('Card 2');
    drag(2, middle(1, 0), at(1, 2), { stop: true });
    expect(gaps().some((g) => g.style.display === 'block')).toBe(true);
    fireEvent.pointerUp(window, { ...ptr, clientX: 0, clientY: 0 });
    await settle();
  });

  it('cards of a label or project board are marked drag-off (no grab cursor); others are not', async () => {
    setup([card(2, { labels: [label] })]);
    const first = renderApp('/orbit/issues?view=board&group=label');
    await screen.findByText('Card 2');
    expect(cardEl(2)).toHaveAttribute('data-drag-off');
    first.unmount();
    renderApp('/orbit/issues?view=board&group=assignee');
    await screen.findByText('Card 2');
    expect(cardEl(2)).not.toHaveAttribute('data-drag-off');
  });

  it('a coarse pointer keeps the first hint line and drops the key line', async () => {
    vi.stubGlobal('matchMedia', (q: string) => ({
      matches: q.includes('coarse'),
      media: q,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
    }));
    setup([card(2), card(3, { state: ref('In Progress') })]);
    renderApp('/orbit/issues?view=board&sort=priority');
    await screen.findByText('Card 2');
    drag(2, middle(1, 0), at(2, 1), { stop: true });
    const hint = await screen.findByTestId('drag-hint');
    expect(hint).toHaveTextContent('Board ordered by Priority');
    expect(hint).not.toHaveTextContent('to also change priority');
    fireEvent.pointerUp(window, { ...ptr, clientX: 0, clientY: 0 });
    await settle();
    vi.unstubAllGlobals();
  });

  it('the label hint tells the person to open a card', async () => {
    setup([card(2, { labels: [label] })]);
    renderApp('/orbit/issues?view=board&group=label');
    await screen.findByText('Card 2');
    expect(screen.getByTestId('drag-off-hint')).toHaveTextContent('Open a card to change its labels.');
    expect(screen.getByTestId('drag-off-hint')).not.toHaveTextContent('L key');
  });
});
