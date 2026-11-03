import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Issue } from '../../api/types';
import { at, middle, stubBoardLayout } from '../../test/boardLayout';
import { baseRoutes, makeIssue, mockApi, states } from '../../test/mockApi';
import { currentLocation, renderApp } from '../../test/render';
import { resetDragged } from './cardLink';
import { STORAGE_PREFIX, storageKey } from './viewPrefs';

// LAUN-163: the board's Columns control: empty columns, hidden states, the last-column guard, and the memory per place.

const ref = (name: string) => {
  const s = states.find((x) => x.name === name)!;
  return { id: s.id, name: s.name, type: s.type, color: s.color };
};
const card = (n: number, state: string) => makeIssue(n, { state: ref(state), title: `Card ${n}` });
const NAMES = ['Backlog', 'Todo', 'In Progress', 'Done'];
const cols = () =>
  Array.from(document.querySelectorAll('[data-column]')).map((c) => (c as HTMLElement).dataset.column);
const stored = (): Record<string, Record<string, unknown>> => {
  const k = Object.keys(localStorage).find((x) => x.startsWith(STORAGE_PREFIX));
  return k ? JSON.parse(localStorage.getItem(k)!) : {};
};
const columnsBtn = () => screen.getByRole('button', { name: /^Columns/ });
const openPanel = async (user: ReturnType<typeof userEvent.setup>) => {
  await user.click(columnsBtn());
  return screen.getByRole('group', { name: 'Board columns' });
};
const box = (panel: HTMLElement, name: string) =>
  within(panel).getByRole('checkbox', { name: new RegExp(`^Show ${name},`) });

let restore: () => void;
function serve(issues: Issue[], extra: Record<string, never> | Parameters<typeof mockApi>[0] = {}) {
  return mockApi({
    ...baseRoutes(),
    'GET /api/v1/orgs/:org/issues': () => ({ json: { items: issues, nextCursor: null } }),
    ...extra,
  });
}
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

describe('the Columns control (LAUN-163)', () => {
  it('shows every column, empty ones too, until the person chooses otherwise', async () => {
    serve([card(2, 'Todo')]);
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 2');
    expect(cols()).toEqual(NAMES);
    expect(columnsBtn()).toHaveAccessibleName('Columns');
  });

  it('is not offered on the list', async () => {
    serve([card(2, 'Todo')]);
    renderApp('/orbit/issues');
    await screen.findByText('Card 2');
    expect(screen.queryByRole('button', { name: /^Columns/ })).toBeNull();
  });

  it('"Show empty columns" off hides the columns with no card; on brings them back', async () => {
    const user = userEvent.setup();
    serve([card(2, 'Todo')]);
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 2');
    const panel = await openPanel(user);
    const empty = within(panel).getByRole('checkbox', { name: 'Show empty columns' });
    expect(empty).toBeChecked();
    await user.click(empty);
    expect(cols()).toEqual(['Todo']);
    expect(screen.getByTestId('columns-live')).toHaveTextContent('Empty columns hidden.');
    await user.click(empty);
    expect(cols()).toEqual(NAMES);
  });

  it('a hidden state leaves the board; its cards still count in the note', async () => {
    const user = userEvent.setup();
    serve([card(2, 'Todo'), card(3, 'Done'), card(4, 'Done')]);
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 2');
    const panel = await openPanel(user);
    await user.click(box(panel, 'Done'));
    await user.click(box(panel, 'Backlog'));
    expect(cols()).toEqual(['Todo', 'In Progress']);
    expect(screen.getByTestId('board-hidden')).toHaveTextContent('2 columns hidden, holding 2 issues.');
    expect(screen.getByTestId('columns-live')).toHaveTextContent(
      'Backlog column hidden. 2 of 4 columns chosen.',
    );
    expect(screen.queryByText('Card 3')).toBeNull();
    // Show all columns, from the note
    await user.click(screen.getByRole('button', { name: 'Show all columns' }));
    expect(cols()).toEqual(NAMES);
  });

  it('the last column cannot be hidden: its box is disabled and says why', async () => {
    const user = userEvent.setup();
    serve([card(2, 'Todo')]);
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 2');
    const panel = await openPanel(user);
    for (const n of ['Backlog', 'Todo', 'In Progress']) await user.click(box(panel, n));
    expect(cols()).toEqual(['Done']);
    const last = box(panel, 'Done');
    expect(last).toBeDisabled();
    expect(last).toHaveAccessibleDescription('At least one column must stay.');
    await user.click(last);
    expect(cols()).toEqual(['Done']);
    expect(stored()['all']!.hidden).toEqual(['Backlog', 'Todo', 'In Progress']);
  });

  it('Reset columns restores the defaults and is off while nothing is changed', async () => {
    const user = userEvent.setup();
    serve([card(2, 'Todo')]);
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 2');
    const panel = await openPanel(user);
    const reset = within(panel).getByRole('button', { name: 'Reset columns' });
    expect(reset).toBeDisabled();
    await user.click(box(panel, 'Done'));
    await user.click(within(panel).getByRole('checkbox', { name: 'Show empty columns' }));
    expect(columnsBtn()).toHaveAccessibleName('Columns (changed)');
    await user.click(reset);
    expect(cols()).toEqual(NAMES);
    expect(stored()['all']).toBeUndefined();
  });

  it('works with the keyboard: Space ticks a box, Escape closes and returns focus', async () => {
    const user = userEvent.setup();
    serve([card(2, 'Todo')]);
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 2');
    columnsBtn().focus();
    await user.keyboard('{Enter}');
    const panel = screen.getByRole('group', { name: 'Board columns' });
    await user.tab(); // Show empty columns
    await user.tab(); // Backlog
    expect(box(panel, 'Backlog')).toHaveFocus();
    await user.keyboard(' ');
    expect(cols()).toEqual(['Todo', 'In Progress', 'Done']);
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('group', { name: 'Board columns' })).toBeNull();
    expect(columnsBtn()).toHaveFocus();
  });

  it('a filter that leaves no visible column shows an empty state with Show all columns', async () => {
    const user = userEvent.setup();
    localStorage.setItem(
      storageKey('orbit', 'u1'),
      JSON.stringify({ all: { view: 'list', sort: 'updated', hideEmpty: true, hidden: [], at: 1 } }),
    );
    serve([]);
    renderApp('/orbit/issues?view=board&q=zzz');
    expect(await screen.findByText('No columns to show for these filters')).toBeInTheDocument();
    expect(document.querySelector('[data-column]')).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Show all columns' }));
    await waitFor(() => expect(cols()).toEqual(NAMES));
  });
});

describe('hidden columns leave the keyboard walk and the selection (LAUN-163)', () => {
  const checked = (id: string) =>
    (document.querySelector(`[data-issue-id="${id}"] input[type="checkbox"]`) as HTMLInputElement).checked;

  it('j and x never land on a card of a hidden column', async () => {
    const user = userEvent.setup();
    localStorage.setItem(
      storageKey('orbit', 'u1'),
      JSON.stringify({
        all: { view: 'board', sort: 'manual', hideEmpty: false, hidden: ['Backlog'], at: 1 },
      }),
    );
    serve([card(2, 'Backlog'), card(3, 'Todo')]);
    renderApp('/orbit/issues');
    await screen.findByText('Card 3');
    expect(document.querySelector('[data-issue-id="i2"]')).toBeNull();
    await user.keyboard('j');
    await user.keyboard('x');
    expect(checked('i3')).toBe(true);
    await user.keyboard('k'); // already first: stays on the only visible card
    await user.keyboard('x');
    expect(checked('i3')).toBe(false);
  });

  it('a selected card leaves the selection when its column is hidden', async () => {
    const user = userEvent.setup();
    serve([card(2, 'Todo'), card(3, 'Done')]);
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 3');
    await user.click(document.querySelector('[data-issue-id="i3"] input[type="checkbox"]') as HTMLElement);
    expect(screen.getByText(/1 selected/)).toBeInTheDocument();
    const panel = await openPanel(user);
    await user.click(box(panel, 'Done'));
    await waitFor(() => expect(screen.queryByText(/1 selected/)).toBeNull());
  });
});

describe('the panel owns its keys (LAUN-163)', () => {
  it('j, x and Enter with a checkbox focused change nothing behind the panel', async () => {
    const user = userEvent.setup();
    serve([card(2, 'Todo')]);
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 2');
    const panel = await openPanel(user);
    const b = box(panel, 'Backlog');
    b.focus();
    await user.keyboard('jx');
    expect(screen.queryByText(/selected/)).toBeNull();
    await user.keyboard('{Enter}');
    expect(screen.getByRole('group', { name: 'Board columns' })).toBeInTheDocument();
    expect(currentLocation.pathname).not.toContain('/issue/');
  });

  it('a click outside closes the panel', async () => {
    const user = userEvent.setup();
    serve([card(2, 'Todo')]);
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 2');
    await openPanel(user);
    await user.click(screen.getByText('Card 2'));
    expect(screen.queryByRole('group', { name: 'Board columns' })).toBeNull();
  });
});

describe('the choice is remembered per place (LAUN-163)', () => {
  it('comes back for the same place, not for another, and is stored beside the layout', async () => {
    const user = userEvent.setup();
    serve([card(2, 'Todo')]);
    const first = renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 2');
    const panel = await openPanel(user);
    await user.click(box(panel, 'Done'));
    await user.click(within(panel).getByRole('checkbox', { name: 'Show empty columns' }));
    // The address said board; the stored layout stays the default (the address is not a choice).
    expect(stored()['all']).toMatchObject({
      view: 'list',
      sort: 'updated',
      hideEmpty: true,
      hidden: ['Done'],
    });
    first.unmount();

    serve([card(2, 'Todo')]);
    const again = renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 2');
    expect(cols()).toEqual(['Todo']);
    again.unmount();

    serve([card(2, 'Todo')]);
    renderApp('/orbit/team/ORB?view=board'); // another place: untouched
    await screen.findByText('Card 2');
    expect(cols()).toEqual(NAMES);
    expect(columnsBtn()).toHaveAccessibleName('Columns');
  });

  it('changing the layout keeps the columns; Reset view clears them', async () => {
    const user = userEvent.setup();
    serve([card(2, 'Todo')]);
    renderApp('/orbit/issues');
    await screen.findByText('Card 2');
    await user.click(screen.getByRole('button', { name: 'Board view' }));
    const panel = await openPanel(user);
    await user.click(box(panel, 'Done'));
    await user.click(screen.getByRole('button', { name: 'List view' }));
    await user.click(screen.getByRole('button', { name: 'Board view' }));
    await waitFor(() => expect(cols()).toEqual(['Backlog', 'Todo', 'In Progress']));
    expect(stored()['all']).toMatchObject({ view: 'board', hidden: ['Done'] });
    await user.click(screen.getByRole('button', { name: 'Reset view' }));
    expect(stored()['all']).toBeUndefined();
    await user.click(screen.getByRole('button', { name: 'Board view' }));
    expect(cols()).toEqual(NAMES);
  });

  it('a stored view from before this change (no column keys) loads with the defaults', async () => {
    localStorage.setItem(
      storageKey('orbit', 'u1'),
      JSON.stringify({ all: { view: 'board', sort: 'manual', at: 1 } }),
    );
    serve([card(2, 'Todo')]);
    renderApp('/orbit/issues');
    await screen.findByText('Card 2');
    expect(cols()).toEqual(NAMES);
    expect(columnsBtn()).toHaveAccessibleName('Columns');
  });

  it('a hostile stored value is ignored', async () => {
    localStorage.setItem(
      storageKey('orbit', 'u1'),
      JSON.stringify({
        all: { view: 'board', sort: 'manual', hideEmpty: 'yes', hidden: [1, null, 'a,b'], at: 1 },
      }),
    );
    serve([card(2, 'Todo')]);
    renderApp('/orbit/issues');
    await screen.findByText('Card 2');
    expect(cols()).toEqual(NAMES);
  });

  it('is sent to the server and a choice made on another device shows here', async () => {
    const user = userEvent.setup();
    const puts: Record<string, unknown>[] = [];
    serve([card(2, 'Todo')], {
      'GET /api/v1/orgs/:org/me/views': () => ({
        json: {
          items: [
            {
              place: 'all',
              view: 'board',
              sort: 'manual',
              hideEmpty: false,
              hiddenColumns: ['Backlog'],
              at: Date.now(),
            },
          ],
        },
      }),
      'PUT /api/v1/orgs/:org/me/views/:place': (req) => {
        puts.push(req.body as Record<string, unknown>);
        return { json: {} };
      },
    });
    renderApp('/orbit/issues');
    await screen.findByText('Card 2');
    await waitFor(() => expect(cols()).toEqual(['Todo', 'In Progress', 'Done']));
    const panel = await openPanel(user);
    await user.click(box(panel, 'Done'));
    await waitFor(() => expect(puts).toHaveLength(1));
    expect(puts[0]).toMatchObject({
      view: 'board',
      sort: 'manual',
      hideEmpty: false,
      hiddenColumns: ['Backlog', 'Done'],
    });
  });

  it('an older server answer without the column keys reads as the defaults', async () => {
    serve([card(2, 'Todo')], {
      'GET /api/v1/orgs/:org/me/views': () => ({
        json: { items: [{ place: 'all', view: 'board', sort: 'manual', at: Date.now() }] },
      }),
    });
    renderApp('/orbit/issues');
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Board view' })).toHaveAttribute('aria-pressed', 'true'),
    );
    await screen.findByText('Card 2');
    expect(cols()).toEqual(NAMES);
  });
});

describe('a hidden column and the drag (LAUN-163, LAUN-167)', () => {
  const ptr = { pointerId: 1, pointerType: 'mouse', isPrimary: true };
  const settle = () => act(() => new Promise<void>((r) => setTimeout(r, 330)));

  it('a hidden column is not a drop target; a visible one still takes a card in any sort', async () => {
    localStorage.setItem(
      storageKey('orbit', 'u1'),
      JSON.stringify({
        all: { view: 'board', sort: 'created', hideEmpty: false, hidden: ['Backlog', 'Done'], at: 1 },
      }),
    );
    const api = serve([card(2, 'Todo')], {
      'PATCH /api/v1/orgs/:org/issues/:issue': () => ({ json: card(2, 'In Progress') }),
    });
    renderApp('/orbit/issues');
    await screen.findByText('Card 2');
    expect(cols()).toEqual(['Todo', 'In Progress']);
    const el = document.querySelector('[data-issue-id="i2"]') as HTMLElement;
    const from = middle(0, 0);
    const to = at(1, 0);
    fireEvent.pointerDown(el, { ...ptr, button: 0, clientX: from.x, clientY: from.y });
    fireEvent.pointerMove(window, { ...ptr, clientX: from.x + 10, clientY: from.y + 10 });
    fireEvent.pointerMove(window, { ...ptr, clientX: to.x, clientY: to.y });
    fireEvent.pointerUp(window, { ...ptr, clientX: to.x, clientY: to.y });
    await settle();
    const patches = api.calls.filter((c) => c.method === 'PATCH');
    expect(patches).toHaveLength(1);
    expect(patches[0]!.body).toMatchObject({ state: 'In Progress' });
  });
});

describe('panel messages and Escape (LAUN-163)', () => {
  it('Escape inside the panel keeps the card selection', async () => {
    const user = userEvent.setup();
    serve([card(2, 'Todo')]);
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 2');
    await user.click(document.querySelector('[data-issue-id="i2"] input[type="checkbox"]') as HTMLElement);
    expect(screen.getByText(/1 selected/)).toBeInTheDocument();
    const panel = await openPanel(user);
    box(panel, 'Backlog').focus();
    await user.keyboard('{Escape}');
    expect(screen.queryByRole('group', { name: 'Board columns' })).toBeNull();
    expect(screen.getByText(/1 selected/)).toBeInTheDocument();
  });

  it('at 30 hidden names the live region gives the cap message', async () => {
    const user = userEvent.setup();
    const extra = Array.from({ length: 32 }, (_, i) => ({
      ...states[0]!,
      id: `x${i}`,
      name: `Extra${i}`,
      position: 10 + i,
    }));
    mockApi({
      ...baseRoutes(),
      'GET /api/v1/orgs/:org/teams/:team/states': () => ({
        json: { items: [...states, ...extra], nextCursor: null },
      }),
      'GET /api/v1/orgs/:org/issues': () => ({ json: { items: [card(2, 'Todo')], nextCursor: null } }),
    });
    localStorage.setItem(
      storageKey('orbit', 'u1'),
      JSON.stringify({
        all: {
          view: 'board',
          sort: 'manual',
          hideEmpty: false,
          hidden: extra.slice(0, 30).map((x) => x.name),
          at: 1,
        },
      }),
    );
    renderApp('/orbit/issues');
    await screen.findByText('Card 2');
    const panel = await openPanel(user);
    await user.click(box(panel, 'Extra30'));
    expect(screen.getByTestId('columns-live')).toHaveTextContent('You can hide at most 30 columns.');
  });
});
