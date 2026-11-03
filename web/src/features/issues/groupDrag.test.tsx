import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { Issue } from '../../api/types';
import { at, middle, stubBoardLayout } from '../../test/boardLayout';
import { baseRoutes, makeIssue, mockApi, states } from '../../test/mockApi';
import { renderApp } from '../../test/render';
import { resetDragged } from './cardLink';

// LAUN-61: select all in a column, then drag the whole selection at once.

const ref = (name: string) => {
  const s = states.find((x) => x.name === name)!;
  return { id: s.id, name: s.name, type: s.type, color: s.color };
};
const card = (n: number, state: string) => makeIssue(n, { state: ref(state), title: `Card ${n}` });

function server(initial: Issue[], failOn?: string, slowRefetch = false) {
  let failed = false;
  const rows = initial.map((i) => ({ ...i }));
  const api = mockApi({
    ...baseRoutes(),
    'GET /api/v1/orgs/:org/issues': async () => {
      // After a failure the refetch is slow, so a test can look at the lists before it lands.
      if (slowRefetch && failed) await new Promise((r) => setTimeout(r, 600));
      return { json: { items: rows, nextCursor: null } };
    },
    'PATCH /api/v1/orgs/:org/issues/:issue': (r) => {
      if (r.params.issue === failOn) {
        failed = true;
        return { status: 500, json: { error: 'database is down', code: 'internal' } };
      }
      const body = r.body as Record<string, unknown>;
      const k0 = rows.findIndex((x) => x.identifier === r.params.issue);
      const [i] = rows.splice(k0, 1);
      if (typeof body.state === 'string') i!.state = ref(body.state);
      const nb = (body.placeAfter ?? body.placeBefore) as string | undefined;
      const k = rows.findIndex((x) => x.id === nb);
      rows.splice(k < 0 ? 0 : body.placeAfter ? k + 1 : k, 0, i!);
      i!.version += 1;
      return { json: i };
    },
  });
  return { api, rows };
}

const column = (name: string) => document.querySelector(`[data-column="${name}"]`) as HTMLElement;
const names = (col: string) =>
  within(column(col))
    .queryAllByRole('link')
    .map((a) => a.textContent);
const cardEl = (n: number) => document.querySelector(`[data-issue-id="i${n}"]`) as HTMLElement;
const patches = (api: ReturnType<typeof mockApi>) => api.calls.filter((c) => c.method === 'PATCH');
const selectCard = (n: number) => fireEvent.click(within(cardEl(n)).getByRole('checkbox'));
const settle = () => act(() => new Promise<void>((r) => setTimeout(r, 330)));

const ptr = { pointerId: 1, pointerType: 'mouse', isPrimary: true };
function drag(n: number, grab: { x: number; y: number }, to: { x: number; y: number }, drop = true) {
  fireEvent.pointerDown(cardEl(n), { ...ptr, button: 0, clientX: grab.x, clientY: grab.y });
  fireEvent.pointerMove(window, { ...ptr, clientX: grab.x + 10, clientY: grab.y + 10 });
  fireEvent.pointerMove(window, { ...ptr, clientX: to.x, clientY: to.y });
  if (drop) fireEvent.pointerUp(window, { ...ptr, clientX: to.x, clientY: to.y });
}

let restore: () => void;
beforeEach(() => {
  document.cookie = 'odt_csrf=csrf-test; path=/';
  restore = stubBoardLayout();
  resetDragged();
});
afterEach(() => restore());

// Backlog: 1 | Todo: 2 3 4 | In Progress: (empty) | Done: (empty)
const start = () => [card(1, 'Backlog'), card(2, 'Todo'), card(3, 'Todo'), card(4, 'Todo')];
const open = async (rows = start(), failOn?: string, slow = false) => {
  const s = server(rows, failOn, slow);
  renderApp('/orbit/issues?view=board');
  await screen.findByText('Card 1');
  return s;
};

describe('select all in a column', () => {
  it('the header checkbox selects every card of its column, mixed when some are', async () => {
    await open();
    const head = within(column('Todo')).getByRole('checkbox', {
      name: 'Select all 3 loaded in Todo',
    }) as HTMLInputElement;
    fireEvent.click(head);
    expect(screen.getByRole('status')).toHaveTextContent('3 selected');
    for (const n of [2, 3, 4]) expect(within(cardEl(n)).getByRole('checkbox')).toBeChecked();
    expect(within(cardEl(1)).getByRole('checkbox')).not.toBeChecked();
    expect(head).toBeChecked();
    fireEvent.click(within(cardEl(3)).getByRole('checkbox'));
    expect(head.indeterminate).toBe(true);
    expect(screen.getByRole('status')).toHaveTextContent('2 selected');
    fireEvent.click(head); // mixed: select the rest
    expect(head).toBeChecked();
    fireEvent.click(within(column('Todo')).getByRole('checkbox', { name: 'Clear all in Todo' }));
    expect(screen.queryByRole('status')).toBeNull();
  });

  it('an empty column has no header checkbox; Backlog selects its one card', async () => {
    await open();
    expect(within(column('Done')).queryByRole('checkbox')).toBeNull();
    fireEvent.click(
      within(column('Backlog')).getByRole('checkbox', { name: 'Select all 1 loaded in Backlog' }),
    );
    expect(within(cardEl(1)).getByRole('checkbox')).toBeChecked();
  });
});

describe('dragging a selection', () => {
  it('moves the block to an empty column: one PATCH per card, in order, each after the last', async () => {
    const { api } = await open();
    fireEvent.click(within(column('Todo')).getByRole('checkbox', { name: 'Select all 3 loaded in Todo' }));
    drag(3, middle(1, 1), at(3, 0));
    await waitFor(() => expect(patches(api)).toHaveLength(3));
    expect(patches(api).map((p) => [p.url.pathname.split('/').pop(), p.body])).toEqual([
      ['ORB-2', { state: 'Done', version: 1 }],
      ['ORB-3', { state: 'Done', placeAfter: 'i2', version: 1 }],
      ['ORB-4', { state: 'Done', placeAfter: 'i3', version: 1 }],
    ]);
    await waitFor(() => expect(names('Done')).toEqual(['Card 2', 'Card 3', 'Card 4']));
    expect(names('Todo')).toEqual([]);
    expect(screen.queryByRole('status')).toBeNull(); // the selection is spent
  });

  it('cards from two columns land between two cards, in their old relative order', async () => {
    const { api } = await open();
    selectCard(1);
    selectCard(4);
    drag(1, middle(0, 0), at(1, 1)); // between Card 2 and Card 3 (Card 1 held)
    await waitFor(() => expect(patches(api)).toHaveLength(2));
    expect(patches(api)[0]!.body).toEqual({ state: 'Todo', placeAfter: 'i2', version: 1 });
    expect(patches(api)[1]!.body).toEqual({ placeAfter: 'i1', version: 1 });
    await waitFor(() => expect(names('Todo')).toEqual(['Card 2', 'Card 1', 'Card 4', 'Card 3']));
  });

  it('shows a count badge on the copy and dims the other selected cards, then cleans up', async () => {
    await open();
    selectCard(2);
    selectCard(3);
    drag(2, middle(1, 0), at(3, 0), false);
    expect(document.body.querySelector('[class*="dragBadge"]')).toHaveTextContent('2');
    expect(cardEl(3)).toHaveAttribute('data-drag-group');
    fireEvent.keyDown(window, { key: 'Escape' });
    await settle();
    expect(document.querySelector('[class*="dragBadge"]')).toBeNull();
    expect(cardEl(3)).not.toHaveAttribute('data-drag-group');
  });

  it('Escape cancels: nothing is sent, the selection stays, the cancel is announced', async () => {
    const { api } = await open();
    selectCard(2);
    selectCard(3);
    drag(2, middle(1, 0), at(3, 0), false);
    fireEvent.keyDown(window, { key: 'Escape' });
    await settle();
    expect(patches(api)).toHaveLength(0);
    expect(screen.getByRole('status')).toHaveTextContent('2 selected');
    expect(screen.getByTestId('board-live')).toHaveTextContent('Move of 2 issues canceled');
  });

  it('dropping the selected block where it already is sends nothing', async () => {
    const { api } = await open();
    selectCard(2);
    selectCard(3);
    drag(2, middle(1, 0), at(1, 0)); // the top of Todo: 2 3 4 stays 2 3 4
    await settle();
    expect(patches(api)).toHaveLength(0);
  });

  it('a failed card: the lists go back, one toast says how many failed, the rest were tried', async () => {
    const { api } = await open(start(), 'ORB-3');
    fireEvent.click(within(column('Todo')).getByRole('checkbox', { name: 'Select all 3 loaded in Todo' }));
    drag(2, middle(1, 0), at(3, 0));
    expect(await screen.findByRole('alert')).toHaveTextContent('1 of 3 issues could not be moved');
    expect(screen.getByRole('alert')).toHaveTextContent('database is down');
    // ORB-4 followed the card that did work (ORB-2), not the one that failed.
    expect(patches(api).map((p) => p.body)).toEqual([
      { state: 'Done', version: 1 },
      { state: 'Done', placeAfter: 'i2', version: 1 },
      { state: 'Done', placeAfter: 'i2', version: 1 },
    ]);
  });

  it('a failed drop puts the lists back to the pre-drop order before the refetch lands', async () => {
    await open(start(), 'ORB-3', true);
    fireEvent.click(within(column('Todo')).getByRole('checkbox', { name: /Select all 3 loaded in Todo/ }));
    drag(2, middle(1, 0), at(3, 0));
    expect(await screen.findByRole('alert')).toHaveTextContent('1 of 3 issues could not be moved');
    // The refetch is held for 600 ms: this is the cache, rolled back (ORB-2 did move on the server).
    expect(names('Todo')).toEqual(['Card 2', 'Card 3', 'Card 4']);
    expect(names('Done')).toEqual([]);
  });

  it('holding a later member of the group: the block lands at the gap, counted without the group', async () => {
    const { api } = await open([...start(), card(5, 'Todo')]);
    selectCard(2);
    selectCard(4);
    drag(4, middle(1, 2), at(1, 2)); // the gap after Card 3 (cards 2, 3, 5 without the held one)
    await waitFor(() => expect(patches(api)).toHaveLength(2));
    expect(patches(api)[0]!.body).toEqual({ placeAfter: 'i3', version: 1 });
    expect(patches(api)[1]!.body).toEqual({ placeAfter: 'i2', version: 1 });
    await waitFor(() => expect(names('Todo')).toEqual(['Card 3', 'Card 2', 'Card 4', 'Card 5']));
  });

  it('a touch press on a selected card does not drag (the page scrolls)', async () => {
    await open();
    selectCard(2);
    selectCard(3);
    fireEvent.pointerDown(cardEl(2), { ...ptr, pointerType: 'touch', button: 0, clientX: 140, clientY: 60 });
    await act(() => new Promise<void>((r) => setTimeout(r, 300)));
    expect(document.querySelector('[class*="dragClone"]')).toBeNull();
    expect(cardEl(2)).not.toHaveAttribute('data-drag-source');
  });

  it('one selected card drags like any card (the single path)', async () => {
    const { api } = await open();
    selectCard(1);
    drag(1, middle(0, 0), at(1, 1));
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ state: 'Todo', placeAfter: 'i2', version: 1 });
  });
});
