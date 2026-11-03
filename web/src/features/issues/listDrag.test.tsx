import { act, cleanup, fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Issue } from '../../api/types';
import { dragHandle, handleOf, rowMiddle, shiftListLayout, stubListLayout } from '../../test/listLayout';
import { baseRoutes, makeIssue, mockApi, states } from '../../test/mockApi';
import { renderApp } from '../../test/render';
import { resetDragged } from './cardLink';

// LAUN-111: drag a list row by its handle, sort Manual. jsdom has no layout: test/listLayout.ts stacks the groups
// (Backlog: head 0-30, row 30-66; Todo: head 70-100, rows 100-136, 136-172, 172-208). The real pointer drag in a
// browser was checked by hand (docs/internal/web-decisions.md, LAUN-111); these tests pin the drop rules and bodies.

const ref = (name: string) => {
  const s = states.find((x) => x.name === name)!;
  return { id: s.id, name: s.name, type: s.type, color: s.color };
};
const card = (n: number, state: string, over: Partial<Issue> = {}) =>
  makeIssue(n, { state: ref(state), title: `Card ${n}`, ...over });

/** A fake server that keeps the manual order in `rows`: placeAfter / placeBefore really move the issue. */
function server(
  initial: Issue[],
  nextCursor: string | null = null,
  hooks: { gate?: Promise<void>; fail?: boolean } = {},
) {
  const rows = initial.map((i) => ({ ...i }));
  const api = mockApi({
    ...baseRoutes(),
    'GET /api/v1/orgs/:org/issues': () => ({ json: { items: rows, nextCursor } }),
    'PATCH /api/v1/orgs/:org/issues/:issue': async (r) => {
      if (hooks.gate) await hooks.gate;
      if (hooks.fail) return { status: 500, json: { error: 'boom', code: 'internal' } };
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

const group = (name: string) => document.querySelector(`section[data-group="${name}"]`) as HTMLElement;
const names = (g: string) =>
  within(group(g))
    .queryAllByRole('link')
    .map((a) => a.textContent);
const patches = (api: ReturnType<typeof mockApi>) => api.calls.filter((c) => c.method === 'PATCH');
const settle = () => act(() => new Promise<void>((r) => setTimeout(r, 330)));

// Backlog: 1 | Todo: 2 3 4
const start = () => [card(1, 'Backlog'), card(2, 'Todo'), card(3, 'Todo'), card(4, 'Todo')];
const URL_MANUAL = '/orbit/issues?sort=manual';

let restore: () => void;
beforeEach(() => {
  document.cookie = 'odt_csrf=csrf-test; path=/';
  restore = stubListLayout();
  resetDragged();
});
afterEach(() => restore());

describe('list drag: where a row lands', () => {
  it('inside a group: ONE update with the row below it, and the new order shows', async () => {
    const { api } = server(start());
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    dragHandle('i4', { x: 100, y: 105 }); // above the middle of Card 2 (118)
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ placeBefore: 'i2', version: 1 });
    expect(patches(api)[0]!.url.pathname).toBe('/api/v1/orgs/orbit/issues/ORB-4');
    await waitFor(() => expect(names('Todo')).toEqual(['Card 4', 'Card 2', 'Card 3']));
    expect(names('Backlog')).toEqual(['Card 1']);
    expect(screen.getByTestId('list-live')).toHaveTextContent('ORB-4 moved to position 1 of 3 in Todo');
  });

  it('between two rows: placeAfter the row above', async () => {
    const { api } = server(start());
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    dragHandle('i2', { x: 100, y: 160 }); // between Card 3 (middle 154) and Card 4 (190)
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ placeAfter: 'i3', version: 1 });
  });

  it('into another group: ONE update with the new state and the neighbour', async () => {
    const { api } = server(start());
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    dragHandle('i1', { x: 100, y: 150 }); // Todo, after Card 2 (118), before Card 3 (154)
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ state: 'Todo', placeAfter: 'i2', version: 1 });
    await waitFor(() => expect(names('Todo')).toEqual(['Card 2', 'Card 1', 'Card 3', 'Card 4']));
    expect(group('Backlog')).toBeNull(); // Backlog has no row left: the list shows no empty group
  });

  it('a row dropped on a group head goes to the top of that group', async () => {
    const { api } = server(start());
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    dragHandle('i1', { x: 100, y: 80 }); // the head of Todo
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ state: 'Todo', placeBefore: 'i2', version: 1 });
  });

  it('the place survives a reload', async () => {
    server(start());
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    dragHandle('i4', { x: 100, y: 105 });
    await waitFor(() => expect(names('Todo')).toEqual(['Card 4', 'Card 2', 'Card 3']));
    await settle();
    cleanup();
    renderApp(URL_MANUAL); // a new page load: the rows come from the (fake) server again
    await screen.findByText('Card 3');
    expect(names('Todo')).toEqual(['Card 4', 'Card 2', 'Card 3']);
  });

  it('below the last LOADED row of a group, when more rows are not loaded yet: it names the loaded row', async () => {
    // nextCursor is set: the list has more pages. A drop at the end of a group lands after the last row we have.
    const { api } = server(start(), 'page-2');
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    dragHandle('i2', { x: 100, y: 400 }); // far below the last group: the end of Todo
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ placeAfter: 'i4', version: 1 });
  });

  it('touch: the handle moves the row at once (no hold), a press on the row does nothing', async () => {
    const { api } = server(start());
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    // a touch on the row body (not the handle) never starts a drag
    const row = document.querySelector('[data-issue-id="i4"]') as HTMLElement;
    fireEvent.pointerDown(row, { pointerId: 2, pointerType: 'touch', clientX: 100, clientY: 190 });
    fireEvent.pointerMove(window, { pointerId: 2, pointerType: 'touch', clientX: 100, clientY: 105 });
    fireEvent.pointerUp(window, { pointerId: 2, pointerType: 'touch', clientX: 100, clientY: 105 });
    await settle();
    expect(patches(api)).toHaveLength(0);
    expect(document.body.querySelector('[aria-hidden="true"][class*="dragClone"]')).toBeNull();
    dragHandle('i4', { x: 100, y: 105 }, { pointerType: 'touch' });
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ placeBefore: 'i2', version: 1 });
  });
});

describe('list drag: touch capture', () => {
  it("the loss of the touch's implicit capture (on the svg in the handle) does not cancel the drag", async () => {
    // Found in a real browser: setPointerCapture on the handle made the browser release the implicit capture,
    // and its lostpointercapture cancelled every touch drag.
    const { api } = server(start());
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    const ptr = { pointerId: 3, pointerType: 'touch', isPrimary: true };
    const grab = { x: 15, y: rowMiddle('i4').y };
    fireEvent.pointerDown(handleOf('i4').querySelector('svg')!, { ...ptr, clientX: grab.x, clientY: grab.y });
    fireEvent.pointerMove(window, { ...ptr, clientX: grab.x, clientY: grab.y - 12 });
    fireEvent.lostPointerCapture(handleOf('i4').querySelector('svg')!, ptr);
    fireEvent.pointerMove(window, { ...ptr, clientX: 100, clientY: 105 });
    fireEvent.pointerUp(window, { ...ptr, clientX: 100, clientY: 105 });
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ placeBefore: 'i2', version: 1 });
  });
});

describe('list drag: nothing to send', () => {
  it('dropped where it was: no update', async () => {
    const { api } = server(start());
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    dragHandle('i3', rowMiddle('i3'));
    await settle();
    expect(patches(api)).toHaveLength(0);
    expect(document.querySelector('[data-drag-source]')).toBeNull();
  });

  it('dropped outside the list: no update', async () => {
    const { api } = server(start());
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    dragHandle('i4', { x: 900, y: 105 });
    await settle();
    expect(patches(api)).toHaveLength(0);
    expect(names('Todo')).toEqual(['Card 2', 'Card 3', 'Card 4']);
  });

  it('Escape cancels and says so', async () => {
    const { api } = server(start());
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    dragHandle('i4', { x: 100, y: 105 }, { release: false });
    expect(document.querySelector('[data-drag-source]')).not.toBeNull();
    fireEvent.keyDown(window, { key: 'Escape' });
    await settle();
    expect(document.querySelector('[data-drag-source]')).toBeNull();
    fireEvent.pointerUp(window, { pointerId: 1, pointerType: 'mouse', clientX: 100, clientY: 105 });
    expect(patches(api)).toHaveLength(0);
    expect(screen.getByTestId('list-live')).toHaveTextContent('Move of ORB-4 canceled');
  });

  it('a click on the handle (no move) sends nothing and does not open the issue', async () => {
    const { api } = server(start());
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    await userEvent.click(handleOf('i4'));
    await settle();
    expect(patches(api)).toHaveLength(0);
    expect(screen.queryByText('Description 4')).toBeNull();
  });

  it('a press on the row itself does not drag (the row is a link)', async () => {
    const { api } = server(start());
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    const row = document.querySelector('[data-issue-id="i4"]') as HTMLElement;
    fireEvent.pointerDown(row, { pointerId: 1, pointerType: 'mouse', button: 0, clientX: 100, clientY: 190 });
    fireEvent.pointerMove(window, { pointerId: 1, pointerType: 'mouse', clientX: 100, clientY: 105 });
    fireEvent.pointerUp(window, { pointerId: 1, pointerType: 'mouse', clientX: 100, clientY: 105 });
    await settle();
    expect(patches(api)).toHaveLength(0);
  });
});

describe('list drag: the keyboard', () => {
  it('Up and Down on the handle move the row one place, with one update each', async () => {
    const { api } = server(start());
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    handleOf('i2').focus();
    fireEvent.keyDown(handleOf('i2'), { key: 'ArrowDown' });
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ placeAfter: 'i3', version: 1 });
    await waitFor(() => expect(names('Todo')).toEqual(['Card 3', 'Card 2', 'Card 4']));
    await waitFor(() => expect(document.activeElement).toBe(handleOf('i2'))); // the focus stays on the handle
    expect(screen.getByTestId('list-live')).toHaveTextContent('ORB-2 moved to position 2 of 3 in Todo');
    fireEvent.keyDown(handleOf('i2'), { key: 'ArrowUp' });
    await waitFor(() => expect(patches(api)).toHaveLength(2));
    expect(patches(api)[1]!.body).toMatchObject({ placeBefore: 'i3' });
  });

  it('at the edge of the group it says so and sends nothing', async () => {
    const { api } = server(start());
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    fireEvent.keyDown(handleOf('i2'), { key: 'ArrowUp' });
    await settle();
    expect(patches(api)).toHaveLength(0);
    expect(screen.getByTestId('list-live')).toHaveTextContent('cannot move further');
  });
});

describe('list drag: only with sort Manual', () => {
  it('with Manual the hint names the handle and every row has one', async () => {
    server(start());
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    expect(screen.getByTestId('list-hint')).toHaveTextContent('Drag the handle');
    expect(document.querySelectorAll('[data-drag-handle]')).toHaveLength(4);
  });

  it('with another sort there is no handle, a press does nothing and the line says why', async () => {
    const { api } = server(start());
    renderApp('/orbit/issues');
    await screen.findByText('Card 4');
    expect(document.querySelectorAll('[data-drag-handle]')).toHaveLength(0);
    expect(screen.getByTestId('list-hint')).toHaveTextContent(
      'Sort is Updated: switch to Manual to reorder.',
    );
    await userEvent.click(screen.getByRole('button', { name: 'Switch to Manual' }));
    await waitFor(() => expect(document.querySelectorAll('[data-drag-handle]')).toHaveLength(4));
    expect(screen.getByTestId('list-hint')).toHaveTextContent('Drag the handle');
    expect(patches(api)).toHaveLength(0);
  });
});

describe('list drag: what the live region says (LAUN-131)', () => {
  it('says "moved" only after the update succeeded', async () => {
    let open!: () => void;
    const gate = new Promise<void>((r) => (open = r));
    const { api } = server(start(), null, { gate });
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    dragHandle('i4', { x: 100, y: 105 });
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    await settle(); // the PATCH is still open
    expect(screen.getByTestId('list-live')).not.toHaveTextContent('moved');
    open();
    await waitFor(() =>
      expect(screen.getByTestId('list-live')).toHaveTextContent('ORB-4 moved to position 1 of 3 in Todo'),
    );
  });

  it('says it could not move when the update fails, never "moved"', async () => {
    const { api } = server(start(), null, { fail: true });
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    dragHandle('i4', { x: 100, y: 105 });
    await waitFor(() =>
      expect(screen.getByTestId('list-live')).toHaveTextContent('ORB-4 could not be moved'),
    );
    expect(patches(api).length).toBeGreaterThan(0);
    expect(screen.getByTestId('list-live')).not.toHaveTextContent('moved to');
  });

  it('a key move that fails says so too', async () => {
    server(start(), null, { fail: true });
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    fireEvent.keyDown(handleOf('i2'), { key: 'ArrowDown' });
    await waitFor(() =>
      expect(screen.getByTestId('list-live')).toHaveTextContent('ORB-2 could not be moved'),
    );
  });

  it('a drop on the same place does not say canceled', async () => {
    server(start());
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    dragHandle('i3', rowMiddle('i3'));
    await settle();
    const t = screen.getByTestId('list-live').textContent ?? '';
    expect(t).toContain('ORB-3 stays in its place');
    expect(t).not.toMatch(/cancel/i);
  });

  it('a drop outside the list does not say canceled either', async () => {
    server(start());
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    dragHandle('i4', { x: 900, y: 105 });
    await settle();
    const t = screen.getByTestId('list-live').textContent ?? '';
    expect(t).toContain('ORB-4 was not moved');
    expect(t).not.toMatch(/cancel/i);
  });
});

describe('list drag: fast key repeat (LAUN-131)', () => {
  it('two ArrowDown presses before any render give two correct places', async () => {
    const { api } = server(start());
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    const h = handleOf('i2');
    h.focus();
    // Native events, outside act(): no render happens between the two presses, as with a held key.
    const press = () =>
      h.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true, cancelable: true }));
    press();
    press();
    await waitFor(() => expect(patches(api)).toHaveLength(2));
    expect(patches(api)[0]!.body).toMatchObject({ placeAfter: 'i3' });
    expect(patches(api)[1]!.body).toMatchObject({ placeAfter: 'i4' });
    await waitFor(() => expect(names('Todo')).toEqual(['Card 3', 'Card 4', 'Card 2']));
    // a third press is at the edge
    fireEvent.keyDown(handleOf('i2'), { key: 'ArrowDown' });
    await waitFor(() => expect(screen.getByTestId('list-live')).toHaveTextContent('cannot move further'));
    expect(patches(api)).toHaveLength(2);
  });

  it('does not pull focus back once the person moved on', async () => {
    server(start());
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    handleOf('i2').focus();
    fireEvent.keyDown(handleOf('i2'), { key: 'ArrowDown' });
    const other = handleOf('i4');
    other.focus();
    await waitFor(() => expect(names('Todo')).toEqual(['Card 3', 'Card 2', 'Card 4']));
    expect(document.activeElement).toBe(handleOf('i4'));
  });
});

describe('list drag: resize (LAUN-131)', () => {
  it('measures again when the window is resized during a drag', async () => {
    server(start());
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    dragHandle('i4', { x: 100, y: 105 }, { release: false });
    const line = () => document.querySelector<HTMLElement>('[class*="listGap"]')!.style.top;
    const before = line();
    // The rows move 40 px down (a resize reflowed the page), the pointer stays where it is.
    shiftListLayout(40);
    expect(line()).toBe(before); // nothing re-measured yet
    fireEvent(window, new Event('resize'));
    expect(line()).not.toBe(before);
    fireEvent.keyDown(window, { key: 'Escape' });
  });
});

describe('list drag: test-only guards (LAUN-131 review)', () => {
  it('a later key press cancels the refocus timer of the one before', async () => {
    server(start());
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    const real = globalThis.setTimeout;
    const refocusIds: unknown[] = [];
    const set = vi.spyOn(globalThis, 'setTimeout').mockImplementation(((fn: () => void, ms?: number) => {
      const id = real(fn, ms);
      if (ms === 800) refocusIds.push(id); // the 800 ms refocus timer
      return id;
    }) as typeof setTimeout);
    const clear = vi.spyOn(globalThis, 'clearTimeout');
    fireEvent.keyDown(handleOf('i2'), { key: 'ArrowDown' });
    expect(refocusIds).toHaveLength(1);
    fireEvent.keyDown(handleOf('i2'), { key: 'ArrowDown' });
    expect(refocusIds).toHaveLength(2);
    expect(clear.mock.calls.map((c) => c[0])).toContain(refocusIds[0]); // the first timer was cancelled
    set.mockRestore();
    clear.mockRestore();
    await waitFor(() => expect(names('Todo')).toEqual(['Card 3', 'Card 4', 'Card 2']));
  });

  it('the resize listener is removed when the drag ends', async () => {
    server(start());
    renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    const add = vi.spyOn(window, 'addEventListener');
    const remove = vi.spyOn(window, 'removeEventListener');
    const count = (spy: typeof add) => spy.mock.calls.filter((c) => c[0] === 'resize').length;
    dragHandle('i4', { x: 100, y: 105 });
    await settle();
    expect(count(add)).toBeGreaterThan(0);
    expect(count(remove)).toBeGreaterThanOrEqual(1); // removed again when the drag ended
    add.mockRestore();
    remove.mockRestore();
  });

  it('after the moves settle and the data changed, a key press uses the fresh order', async () => {
    const { api, rows } = server(start());
    const { client } = renderApp(URL_MANUAL);
    await screen.findByText('Card 4');
    fireEvent.keyDown(handleOf('i2'), { key: 'ArrowDown' });
    await waitFor(() => expect(names('Todo')).toEqual(['Card 3', 'Card 2', 'Card 4']));
    await settle();
    // someone else reorders: Card 4, Card 2, Card 3
    const byId = (id: string) => rows.find((r) => r.id === id)!;
    rows.splice(0, rows.length, byId('i1'), byId('i4'), byId('i2'), byId('i3'));
    await act(() => client.invalidateQueries());
    await waitFor(() => expect(names('Todo')).toEqual(['Card 4', 'Card 2', 'Card 3']));
    fireEvent.keyDown(handleOf('i2'), { key: 'ArrowDown' });
    await waitFor(() => expect(patches(api)).toHaveLength(2));
    expect(patches(api)[1]!.body).toMatchObject({ placeAfter: 'i3' }); // not i4, the stale order's answer
  });
});
