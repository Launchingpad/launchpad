import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import type { Issue } from '../../api/types';
import { at, middle, stubBoardLayout } from '../../test/boardLayout';
import { baseRoutes, makeIssue, mockApi, states } from '../../test/mockApi';
import { renderApp } from '../../test/render';
import { resetDragged } from './cardLink';

// LAUN-110: "I cannot change the ticket order". A real browser (Brave, headless, a real mouse) moved cards
// in a column and across columns and the order survived a reload, so these tests pin the parts around it:
// the board starts in Manual, a drop sends placeAfter / placeBefore, another sort turns the drag off
// and says why, and a filtered board names the cards that are on screen.

const ref = (name: string) => {
  const s = states.find((x) => x.name === name)!;
  return { id: s.id, name: s.name, type: s.type, color: s.color };
};
const card = (n: number, state: string, over: Partial<Issue> = {}) =>
  makeIssue(n, { state: ref(state), title: `Card ${n}`, ...over });

/** A fake server that keeps the manual order in `rows`. `visible` (a filter) hides some rows from the list. */
function server(initial: Issue[], visible: (i: Issue) => boolean = () => true) {
  const rows = initial.map((i) => ({ ...i }));
  const api = mockApi({
    ...baseRoutes(),
    'GET /api/v1/orgs/:org/issues': () => ({ json: { items: rows.filter(visible), nextCursor: null } }),
    'PATCH /api/v1/orgs/:org/issues/:issue': (r) => {
      const body = r.body as Record<string, unknown>;
      const from = rows.findIndex((x) => x.identifier === r.params.issue);
      const [i] = rows.splice(from, 1);
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
const settle = () => act(() => new Promise<void>((r) => setTimeout(r, 330)));
const ptr = { pointerId: 1, pointerType: 'mouse', isPrimary: true };
function drag(n: number, grab: { x: number; y: number }, to: { x: number; y: number }) {
  fireEvent.pointerDown(cardEl(n), { ...ptr, button: 0, clientX: grab.x, clientY: grab.y });
  fireEvent.pointerMove(window, { ...ptr, clientX: grab.x + 10, clientY: grab.y + 10 });
  fireEvent.pointerMove(window, { ...ptr, clientX: to.x, clientY: to.y });
  fireEvent.pointerUp(window, { ...ptr, clientX: to.x, clientY: to.y });
}

let restore: () => void;
beforeEach(() => {
  document.cookie = 'odt_csrf=csrf-test; path=/';
  restore = stubBoardLayout();
  resetDragged();
});
afterEach(() => restore());

// Todo: 2 3 4 5
const todo = () => [card(2, 'Todo'), card(3, 'Todo'), card(4, 'Todo'), card(5, 'Todo')];

describe('board sort and drag (LAUN-110)', () => {
  it('starts in Manual: the select says so, the hint says how, the request asks for manual', async () => {
    const { api } = server(todo());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 2');
    const select = screen.getByRole('combobox', { name: /sort by/i }) as HTMLSelectElement;
    expect(select.value).toBe('manual');
    expect(select).toBeEnabled();
    expect(screen.getByTestId('board-hint')).toHaveTextContent('Drag a card to change its place');
    const get = api.calls.find((c) => c.method === 'GET' && c.url.pathname.endsWith('/issues'))!;
    expect(get.url.searchParams.get('orderBy')).toBe('manual');
  });

  it('a drop in a column sends placeAfter and the order is still there after a reload', async () => {
    const { api } = server(todo());
    const first = renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 2');
    drag(2, middle(1, 0), at(1, 2)); // Card 2 goes between 4 and 5 (the rest: 3 4 5, place 2)
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ placeAfter: 'i4', version: 1 });
    await waitFor(() => expect(names('Todo')).toEqual(['Card 3', 'Card 4', 'Card 2', 'Card 5']));
    first.unmount();
    renderApp('/orbit/issues?view=board'); // a reload: a new page asks the server again
    await screen.findByText('Card 5');
    expect(names('Todo')).toEqual(['Card 3', 'Card 4', 'Card 2', 'Card 5']);
  });

  it('with sort Priority a drop in the same column moves nothing, sends nothing, and the page says why', async () => {
    const { api } = server(todo());
    renderApp('/orbit/issues?view=board&sort=priority');
    await screen.findByText('Card 2');
    expect(screen.getByTestId('board-hint')).toHaveTextContent(
      'Sort is Priority. Drag a card to another column to change its status; switch to Manual to reorder inside a column.',
    );
    drag(2, middle(1, 0), at(1, 2));
    await settle();
    expect(patches(api)).toHaveLength(0);
    expect(document.querySelector('[data-drag-source]')).toBeNull();
    expect(document.querySelector('[class*="dragClone"]')).toBeNull();
    expect(names('Todo')).toEqual(['Card 2', 'Card 3', 'Card 4', 'Card 5']);
  });

  it('with sort Priority the board sorts each column by priority in the page (urgent first, none last)', async () => {
    // The server sends the newest first; the page puts them in priority order.
    server([
      card(2, 'Todo', { priority: 0 }),
      card(3, 'Todo', { priority: 3 }),
      card(4, 'Todo', { priority: 1 }),
      card(5, 'Todo', { priority: 2 }),
    ]);
    renderApp('/orbit/issues?view=board&sort=priority');
    await screen.findByText('Card 2');
    expect(names('Todo')).toEqual(['Card 4', 'Card 5', 'Card 3', 'Card 2']);
  });

  it('the hint button switches to Manual, and then the same drag works', async () => {
    const { api } = server(todo());
    renderApp('/orbit/issues?view=board&sort=created');
    await screen.findByText('Card 2');
    expect(screen.getByTestId('board-hint')).toHaveTextContent('Sort is Created');
    await userEvent.click(screen.getByRole('button', { name: 'Switch to Manual' }));
    await waitFor(() => expect(screen.getByTestId('board-hint')).toHaveTextContent('Drag a card'));
    expect((screen.getByRole('combobox', { name: /sort by/i }) as HTMLSelectElement).value).toBe('manual');
    await screen.findByText('Card 2');
    drag(2, middle(1, 0), at(1, 2));
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toMatchObject({ placeAfter: 'i4' });
  });

  it('choosing another sort in the select turns the drag off; the board asks for that order', async () => {
    const { api } = server(todo());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 2');
    await userEvent.selectOptions(screen.getByRole('combobox', { name: /sort by/i }), 'created');
    await waitFor(() => expect(screen.getByTestId('board-hint')).toHaveTextContent('Sort is Created'));
    const gets = api.calls.filter((c) => c.method === 'GET' && c.url.pathname.endsWith('/issues'));
    expect(gets[gets.length - 1]!.url.searchParams.get('orderBy')).toBe('createdAt');
    drag(2, middle(1, 0), at(1, 2));
    await settle();
    expect(patches(api)).toHaveLength(0);
  });

  it('the board button from the list starts in Manual, even when the list sort was the default', async () => {
    server(todo());
    renderApp('/orbit/issues');
    await screen.findByText('Card 2');
    await userEvent.click(screen.getByRole('button', { name: 'Board view' }));
    await screen.findByTestId('board-hint');
    expect(screen.getByTestId('board-hint')).toHaveTextContent('Drag a card');
    expect((screen.getByRole('combobox', { name: /sort by/i }) as HTMLSelectElement).value).toBe('manual');
  });

  it('the list in Manual says the rows drag by the handle (LAUN-111); another sort says why not', async () => {
    server(todo());
    renderApp('/orbit/issues?sort=manual');
    await screen.findByText('Card 2');
    expect(screen.getByTestId('list-hint')).toHaveTextContent('Drag the handle');
    await userEvent.selectOptions(screen.getByRole('combobox', { name: /sort by/i }), 'created');
    await waitFor(() => expect(screen.getByTestId('list-hint')).toHaveTextContent('Sort is Created'));
    await userEvent.click(screen.getByRole('button', { name: 'Switch to Manual' }));
    await waitFor(() => expect(screen.getByTestId('list-hint')).toHaveTextContent('Drag the handle'));
  });

  it('a filtered board names a card that is on screen, not a hidden neighbour', async () => {
    // Card 3 is hidden by the filter. Card 5 goes to the top of what is shown: before Card 2.
    const { api } = server(todo(), (i) => i.id !== 'i3');
    renderApp('/orbit/issues?view=board&q=x');
    await screen.findByText('Card 2');
    expect(names('Todo')).toEqual(['Card 2', 'Card 4', 'Card 5']);
    drag(5, middle(1, 2), at(1, 0));
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ placeBefore: 'i2', version: 1 });
  });
});

// LAUN-167: "click and drag a ticket is broken". With a sort that is not Manual the whole board was dead.
// Now a card can always change column (the state only; no place is sent); the order inside a column
// stays a Manual feature.
describe('board drag in every sort (LAUN-167)', () => {
  // Todo: 2 3 4, In Progress: 5 6
  const rows = () => [
    card(2, 'Todo'),
    card(3, 'Todo'),
    card(4, 'Todo'),
    card(5, 'In Progress'),
    card(6, 'In Progress'),
  ];
  const live = () => screen.getByTestId('board-live');

  it.each(['updated', 'created', 'priority'])(
    'sort=%s: a drop in another column sends the state and no place, and the card moves there',
    async (sort) => {
      const { api } = server(rows());
      renderApp(`/orbit/issues?view=board&sort=${sort}`);
      await screen.findByText('Card 2');
      drag(2, middle(1, 0), at(2, 1));
      await waitFor(() => expect(patches(api)).toHaveLength(1));
      expect(patches(api)[0]!.body).toEqual({ state: 'In Progress', version: 1 });
      await waitFor(() => expect(names('Todo')).toEqual(['Card 3', 'Card 4']));
      expect(names('In Progress')).toContain('Card 2');
      expect(live()).toHaveTextContent('moved to In Progress');
    },
  );

  it('sort=updated: until the server answers, the card is on top of its new column (the newest)', async () => {
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    const rowsNow = rows();
    mockApi({
      ...baseRoutes(),
      'GET /api/v1/orgs/:org/issues': () => ({ json: { items: rowsNow, nextCursor: null } }),
      'PATCH /api/v1/orgs/:org/issues/:issue': async () => {
        await gate;
        return { status: 500, json: { error: 'boom', code: 'internal' } };
      },
    });
    renderApp('/orbit/issues?view=board&sort=updated');
    await screen.findByText('Card 2');
    drag(6, middle(2, 1), at(1, 2)); // pointer low in the column: a manual drop would put it last
    await waitFor(() => expect(names('Todo')).toEqual(['Card 6', 'Card 2', 'Card 3', 'Card 4']));
    release();
    // The update failed: the card is back where it was.
    await waitFor(() => expect(names('Todo')).toEqual(['Card 2', 'Card 3', 'Card 4']));
    expect(names('In Progress')).toEqual(['Card 5', 'Card 6']);
  });

  it('sort=updated: a drop in the same column sends nothing and says reordering needs Manual', async () => {
    const { api } = server(rows());
    renderApp('/orbit/issues?view=board&sort=updated');
    await screen.findByText('Card 2');
    drag(2, middle(1, 0), at(1, 2));
    await settle();
    expect(patches(api)).toHaveLength(0);
    expect(names('Todo')).toEqual(['Card 2', 'Card 3', 'Card 4']);
    expect(live()).toHaveTextContent(/reordering inside Todo needs the Manual sort/i);
  });

  it('sort=updated: the selected cards go to another column together, state only', async () => {
    const { api } = server(rows());
    renderApp('/orbit/issues?view=board&sort=updated');
    await screen.findByText('Card 2');
    await userEvent.click(screen.getByRole('checkbox', { name: 'Select ORB-2' }));
    await userEvent.click(screen.getByRole('checkbox', { name: 'Select ORB-3' }));
    drag(2, middle(1, 0), at(2, 0));
    await waitFor(() => expect(patches(api)).toHaveLength(2));
    for (const p of patches(api)) expect(p.body).toMatchObject({ state: 'In Progress' });
    for (const p of patches(api)) expect(p.body).not.toHaveProperty('placeAfter');
    for (const p of patches(api)) expect(p.body).not.toHaveProperty('placeBefore');
    expect(live()).toHaveTextContent('2 issues moved to In Progress');
  });

  it('Manual is unchanged: a cross-column drop still sends the place', async () => {
    const { api } = server(rows());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 2');
    drag(2, middle(1, 0), at(2, 1));
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toMatchObject({ state: 'In Progress', placeAfter: 'i5' });
  });

  it('sort=updated: a group with one card already in the target column moves only the others (1 issue)', async () => {
    const { api } = server(rows());
    renderApp('/orbit/issues?view=board&sort=updated');
    await screen.findByText('Card 2');
    await userEvent.click(screen.getByRole('checkbox', { name: 'Select ORB-2' }));
    await userEvent.click(screen.getByRole('checkbox', { name: 'Select ORB-5' }));
    drag(2, middle(1, 0), at(2, 0));
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.url.pathname).toMatch(/ORB-2$/);
    expect(patches(api)[0]!.body).toEqual({ state: 'In Progress', version: 1 });
    expect(live()).toHaveTextContent('1 issue moved to In Progress');
    expect(live()).not.toHaveTextContent('1 issues');
  });

  it('sort=updated: a group dropped inside one column sends nothing and says reordering needs Manual', async () => {
    const { api } = server(rows());
    renderApp('/orbit/issues?view=board&sort=updated');
    await screen.findByText('Card 2');
    await userEvent.click(screen.getByRole('checkbox', { name: 'Select ORB-2' }));
    await userEvent.click(screen.getByRole('checkbox', { name: 'Select ORB-3' }));
    drag(2, middle(1, 0), at(1, 2));
    await settle();
    expect(patches(api)).toHaveLength(0);
    expect(live()).toHaveTextContent(/reordering inside Todo needs the Manual sort/i);
    expect(names('Todo')).toEqual(['Card 2', 'Card 3', 'Card 4']);
  });

  it('a failed move is rolled back from the snapshot even when the refetch never answers', async () => {
    const rowsNow = rows();
    let gets = 0;
    mockApi({
      ...baseRoutes(),
      'GET /api/v1/orgs/:org/issues': async () => {
        if (++gets > 1) await new Promise(() => {}); // every refetch hangs
        return { json: { items: rowsNow, nextCursor: null } };
      },
      'PATCH /api/v1/orgs/:org/issues/:issue': () => ({
        status: 500,
        json: { error: 'boom', code: 'internal' },
      }),
    });
    renderApp('/orbit/issues?view=board&sort=updated');
    await screen.findByText('Card 2');
    drag(6, middle(2, 1), at(1, 2));
    await waitFor(() => expect(gets).toBeGreaterThan(1)); // the failed update has settled and asked again
    await waitFor(() => expect(names('Todo')).toEqual(['Card 2', 'Card 3', 'Card 4']));
    expect(names('In Progress')).toEqual(['Card 5', 'Card 6']);
  });

  it('sort=updated: a mixed group dropped on the lane of the card you hold still moves the others', async () => {
    const { api } = server(rows());
    renderApp('/orbit/issues?view=board&sort=updated');
    await screen.findByText('Card 2');
    await userEvent.click(screen.getByRole('checkbox', { name: 'Select ORB-2' }));
    await userEvent.click(screen.getByRole('checkbox', { name: 'Select ORB-5' }));
    drag(5, middle(2, 0), at(2, 1)); // hold ORB-5 (already in In Progress); ORB-2 comes from Todo
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.url.pathname).toMatch(/ORB-2$/);
    expect(live()).toHaveTextContent('1 issue moved to In Progress');
  });

  it('the cards are not marked as undraggable in another sort', async () => {
    server(rows());
    renderApp('/orbit/issues?view=board&sort=created');
    await screen.findByText('Card 2');
    expect(document.querySelector('[data-drag-off]')).toBeNull();
  });
});
