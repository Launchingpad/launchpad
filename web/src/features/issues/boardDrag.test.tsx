import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import { QueryClient } from '@tanstack/react-query';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { moveIssueInCaches } from '../../api/queries';
import type { Issue } from '../../api/types';
import { at, middle, stubBoardLayout } from '../../test/boardLayout';
import { baseRoutes, makeIssue, mockApi, states } from '../../test/mockApi';
import { renderApp } from '../../test/render';
import { resetDragged } from './cardLink';

// LAUN-29. jsdom has no layout: test/boardLayout.ts gives the board a fake one (columns Backlog, Todo,
// In Progress, Done, cards 46 px apart). The real pointer drag in a browser is checked by hand
// (docs/internal/web-decisions.md); these tests pin the drop rules, the one-update contract, the guard.

let renders = 0;
vi.mock('../../ui/Icons', async (orig) => {
  const m = await orig<typeof import('../../ui/Icons')>();
  return {
    ...m,
    PriorityIcon: (p: Parameters<typeof m.PriorityIcon>[0]) => {
      renders++;
      return m.PriorityIcon(p);
    },
  };
});

const ref = (name: string) => {
  const s = states.find((x) => x.name === name)!;
  return { id: s.id, name: s.name, type: s.type, color: s.color };
};
const card = (n: number, state: string, over: Partial<Issue> = {}) =>
  makeIssue(n, { state: ref(state), title: `Card ${n}`, ...over });

/** A fake server that keeps manual order: placeAfter / placeBefore really move the issue. */
function server(
  initial: Issue[],
  opts: { patch?: (body: Record<string, unknown>) => { status: number; json: unknown } | undefined } = {},
) {
  const rows = initial.map((i) => ({ ...i }));
  const api = mockApi({
    ...baseRoutes(),
    'GET /api/v1/orgs/:org/issues': () => ({ json: { items: rows, nextCursor: null } }),
    'PATCH /api/v1/orgs/:org/issues/:issue': (r) => {
      const body = r.body as Record<string, unknown>;
      const bad = opts.patch?.(body);
      if (bad) return bad;
      const at = rows.findIndex((x) => x.identifier === r.params.issue);
      const [i] = rows.splice(at, 1);
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
function press(el: HTMLElement, p: { x: number; y: number }, extra: object = {}) {
  fireEvent.pointerDown(el, { ...ptr, button: 0, clientX: p.x, clientY: p.y, ...extra });
}
const move = (p: { x: number; y: number }, extra: object = {}) =>
  fireEvent.pointerMove(window, { ...ptr, clientX: p.x, clientY: p.y, ...extra });
const release = (p: { x: number; y: number }, extra: object = {}) =>
  fireEvent.pointerUp(window, { ...ptr, clientX: p.x, clientY: p.y, ...extra });

/** Grab card `n` in the middle and move it, so its centre is at `to`. */
function dragTo(n: number, grab: { x: number; y: number }, to: { x: number; y: number }, drop = true) {
  press(cardEl(n), grab);
  move({ x: grab.x + 10, y: grab.y + 10 }); // past the press threshold
  move(to);
  if (drop) release(to);
}

let restore: () => void;
beforeEach(() => {
  document.cookie = 'odt_csrf=csrf-test; path=/';
  restore = stubBoardLayout();
  resetDragged();
  renders = 0;
});
afterEach(() => restore());

// Backlog: 1 | Todo: 2 3 4 | In Progress: (empty) | Done: (empty)
const start = () => [card(1, 'Backlog'), card(2, 'Todo'), card(3, 'Todo'), card(4, 'Todo')];

describe('board drag: where a card lands', () => {
  it('asks the server for the manual order', async () => {
    const { api } = server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    const get = api.calls.find((c) => c.method === 'GET' && c.url.pathname.endsWith('/issues'))!;
    expect(get.url.searchParams.get('orderBy')).toBe('manual');
  });

  it('between two cards of another column: ONE update with the state and the card above', async () => {
    const { api } = server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), at(1, 1));
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ state: 'Todo', placeAfter: 'i2', version: 1 });
    expect(patches(api)[0]!.url.pathname).toBe('/api/v1/orgs/orbit/issues/ORB-1');
    await waitFor(() => expect(names('Todo')).toEqual(['Card 2', 'Card 1', 'Card 3', 'Card 4']));
    expect(names('Backlog')).toEqual([]);
  });

  it('at the top of a column: placeBefore the first card', async () => {
    const { api } = server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), at(1, 0));
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ state: 'Todo', placeBefore: 'i2', version: 1 });
  });

  it('at the bottom of a column: placeAfter the last card', async () => {
    const { api } = server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), at(1, 3));
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ state: 'Todo', placeAfter: 'i4', version: 1 });
  });

  it('into an empty column: the state only (the server puts it on top)', async () => {
    const { api } = server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), at(3, 0));
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ state: 'Done', version: 1 });
    await waitFor(() => expect(names('Done')).toEqual(['Card 1']));
  });

  it('a drop in the empty area under the cards counts, and is the end of the column', async () => {
    const { api } = server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), { x: 300 + 140, y: 560 });
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toMatchObject({ state: 'Todo', placeAfter: 'i4' });
  });

  it('inside the same column: the place only, no state', async () => {
    const { api } = server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 2');
    // Card 2 (first of Todo) goes between 3 and 4. The remaining cards are 3, 4: place 1.
    dragTo(2, middle(1, 0), at(1, 1));
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ placeAfter: 'i3', version: 1 });
    await waitFor(() => expect(names('Todo')).toEqual(['Card 3', 'Card 2', 'Card 4']));
  });

  it('dropped where it was: nothing is sent', async () => {
    const { api } = server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 3');
    dragTo(3, middle(1, 1), at(1, 1)); // its own slot
    await settle();
    expect(patches(api)).toHaveLength(0);
    expect(names('Todo')).toEqual(['Card 2', 'Card 3', 'Card 4']);
    expect(document.querySelector('[aria-hidden="true"][class*="dragClone"]')).toBeNull();
  });

  it('dropped outside the columns: nothing is sent and the card stays', async () => {
    const { api } = server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), { x: 700, y: 900 }); // below the board
    await settle();
    expect(patches(api)).toHaveLength(0);
    expect(names('Backlog')).toEqual(['Card 1']);
  });

  it('with several teams in a column, names a card of the dragged issue team', async () => {
    const other = card(5, 'Todo', { team: { id: 'team2', key: 'OTH', name: 'Other' } });
    const { api } = server([card(1, 'Backlog'), card(2, 'Todo'), other, card(4, 'Todo')]);
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), at(1, 2)); // after Card 2 and the other team's card
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toEqual({ state: 'Todo', placeAfter: 'i2', version: 1 });
  });
});

describe('board drag: the picture while dragging', () => {
  it('moves a copy of the card, hides the real one, opens a gap and slides the cards under it', async () => {
    server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), at(1, 1), false);
    const clone = document.querySelector('[class*="dragClone"]') as HTMLElement;
    expect(clone).toBeInTheDocument();
    expect(clone).toHaveAttribute('aria-hidden', 'true');
    expect(clone.hasAttribute('data-issue-id')).toBe(false); // not a second card for queries
    expect(cardEl(1)).toHaveAttribute('data-drag-source');
    expect(column('Todo').className).toMatch(/dropTarget/);
    const gap = column('Todo').querySelector<HTMLElement>('[data-gap]')!;
    expect(gap.style.display).toBe('block');
    expect(gap.style.top).toBe('46px'); // after Card 2
    expect(gap.style.height).toBe('40px');
    expect(cardEl(2).style.transform).toBe('');
    expect(cardEl(3).style.transform).toBe('translateY(46px)');
    expect(cardEl(4).style.transform).toBe('translateY(46px)');
    release(at(1, 1));
  });

  it('closes the slot it left in its own column', async () => {
    server([card(1, 'Todo'), card(2, 'Todo'), card(3, 'Todo')]);
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(1, 0), at(1, 2), false);
    // The remaining cards 2 and 3 move up into the slot; the gap is at the end, so nothing opens.
    expect(cardEl(2).style.transform).toBe('translateY(-46px)');
    expect(cardEl(3).style.transform).toBe('translateY(-46px)');
    expect(column('Todo').querySelector<HTMLElement>('[data-gap]')!.style.top).toBe('92px');
    release(at(1, 2));
  });

  it('a press that moves less than the threshold is a click, not a drag', async () => {
    const { api } = server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    press(cardEl(1), middle(0, 0));
    move({ x: middle(0, 0).x + 2, y: middle(0, 0).y + 1 });
    expect(document.querySelector('[class*="dragClone"]')).toBeNull();
    release(middle(0, 0));
    expect(patches(api)).toHaveLength(0);
  });

  it('does not start from a checkbox, a right button or a modified press', async () => {
    server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    press(within(cardEl(1)).getByRole('checkbox'), middle(0, 0));
    move({ x: 200, y: 200 });
    expect(document.querySelector('[class*="dragClone"]')).toBeNull();
    release({ x: 200, y: 200 });
    press(cardEl(1), middle(0, 0), { button: 2 });
    move({ x: 200, y: 200 });
    expect(document.querySelector('[class*="dragClone"]')).toBeNull();
    release({ x: 200, y: 200 });
    press(cardEl(1), middle(0, 0), { metaKey: true });
    move({ x: 200, y: 200 });
    expect(document.querySelector('[class*="dragClone"]')).toBeNull();
    release({ x: 200, y: 200 });
  });

  it('renders no card while it drags (the render-count guard)', async () => {
    const many = Array.from({ length: 120 }, (_, k) => card(k + 1, 'Todo'));
    server(many);
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    const before = renders;
    press(cardEl(1), middle(1, 0));
    for (let k = 1; k <= 40; k++) move({ x: 440, y: 40 + k * 20 });
    expect(document.querySelector('[class*="dragClone"]')).toBeInTheDocument();
    expect(renders).toBe(before); // 40 moves, a gap opened and closed: zero card renders
    fireEvent.keyDown(window, { key: 'Escape' });
    await settle();
    expect(renders).toBe(before);
  });
});

describe('board drag: cancel and the click guard', () => {
  it('Escape puts the card back, sends nothing and leaves nothing behind', async () => {
    const { api } = server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), at(1, 1), false);
    fireEvent.keyDown(window, { key: 'Escape' });
    await settle();
    release(at(1, 1));
    expect(patches(api)).toHaveLength(0);
    expect(document.querySelector('[class*="dragClone"]')).toBeNull();
    expect(cardEl(1)).not.toHaveAttribute('data-drag-source');
    expect(cardEl(3).style.transform).toBe('');
    expect(column('Todo').className).not.toMatch(/dropTarget/);
    expect(document.body.className).not.toMatch(/dragActive/);
    expect(names('Backlog')).toEqual(['Card 1']);
    expect(screen.getByTestId('board-live')).toHaveTextContent('Move of ORB-1 canceled');
  });

  it('pointercancel cancels the drag', async () => {
    const { api } = server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), at(1, 1), false);
    fireEvent.pointerCancel(window, { ...ptr });
    await settle();
    expect(patches(api)).toHaveLength(0);
    expect(document.querySelector('[class*="dragClone"]')).toBeNull();
  });

  it('the click that follows a drop does not open the issue', async () => {
    const { api } = server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), at(1, 1));
    const link = within(cardEl(1)).getByRole('link');
    fireEvent.click(link, { detail: 1 });
    fireEvent.click(cardEl(1), { detail: 1 });
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    await settle();
    // Still on the board: nothing navigated.
    expect(screen.getByRole('group', { name: 'Board' })).toBeInTheDocument();
  });

  it('a plain click, with no drag, still opens the issue', async () => {
    server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    press(cardEl(1), middle(0, 0));
    release(middle(0, 0));
    fireEvent.click(within(cardEl(1)).getByRole('link'), { detail: 1 });
    await waitFor(() => expect(screen.queryByRole('group', { name: 'Board' })).not.toBeInTheDocument());
  });
});

describe('board drag: the update goes wrong', () => {
  it('rolls the card back to its old column and place, and says why', async () => {
    const { api } = server(start(), {
      patch: () => ({ status: 500, json: { error: 'database is down', code: 'internal_error' } }),
    });
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), at(1, 1));
    expect(await screen.findByRole('alert')).toHaveTextContent('database is down');
    await waitFor(() => expect(names('Backlog')).toEqual(['Card 1']));
    expect(names('Todo')).toEqual(['Card 2', 'Card 3', 'Card 4']);
    expect(patches(api)).toHaveLength(1);
  });

  it('shows the move at once, before the server answers', async () => {
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    const rows = start();
    mockApi({
      ...baseRoutes(),
      'GET /api/v1/orgs/:org/issues': () => ({ json: { items: rows, nextCursor: null } }),
      'PATCH /api/v1/orgs/:org/issues/:issue': async () => {
        await gate;
        return { status: 500, json: { error: 'late', code: 'internal_error' } };
      },
    });
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), at(1, 1));
    await waitFor(() => expect(names('Todo')).toEqual(['Card 2', 'Card 1', 'Card 3', 'Card 4']));
    expect(names('Backlog')).toEqual([]);
    release();
    await screen.findByRole('alert');
    await waitFor(() => expect(names('Backlog')).toEqual(['Card 1']));
  });

  it('stale_placement: says the column changed and fetches the lists again', async () => {
    const { api } = server(start(), {
      patch: () => ({ status: 409, json: { error: 'neighbour moved', code: 'stale_placement' } }),
    });
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    const gets = () =>
      api.calls.filter((c) => c.method === 'GET' && c.url.pathname.endsWith('/issues')).length;
    const before = gets();
    dragTo(1, middle(0, 0), at(1, 1));
    expect(await screen.findByRole('alert')).toHaveTextContent('The column changed');
    await waitFor(() => expect(gets()).toBeGreaterThan(before));
    await waitFor(() => expect(names('Backlog')).toEqual(['Card 1']));
  });

  it('announces a successful drop for a screen reader', async () => {
    server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), at(1, 1));
    await waitFor(() =>
      expect(screen.getByTestId('board-live')).toHaveTextContent('ORB-1 moved to Todo, position 2 of 4'),
    );
  });
});

describe('board drag: touch and the edges', () => {
  const touch = { pointerType: 'touch' };
  it('a touch picks a card up after a short hold', async () => {
    const { api } = server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    press(cardEl(1), middle(0, 0), touch);
    expect(document.querySelector('[class*="dragClone"]')).toBeNull(); // not yet: the page may scroll
    await act(() => new Promise<void>((r) => setTimeout(r, 300)));
    expect(document.querySelector('[class*="dragClone"]')).toBeInTheDocument();
    move(at(1, 1), touch);
    release(at(1, 1), touch);
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    expect(patches(api)[0]!.body).toMatchObject({ state: 'Todo', placeAfter: 'i2' });
  });

  it('a touch that moves before the hold is a scroll, not a drag', async () => {
    const { api } = server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    press(cardEl(1), middle(0, 0), touch);
    move({ x: middle(0, 0).x, y: middle(0, 0).y + 30 }, touch);
    await act(() => new Promise<void>((r) => setTimeout(r, 300)));
    expect(document.querySelector('[class*="dragClone"]')).toBeNull();
    release(at(1, 1), touch);
    expect(patches(api)).toHaveLength(0);
  });

  it('a held touch blocks the page scroll (touchmove is cancelled)', async () => {
    server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    press(cardEl(1), middle(0, 0), touch);
    await act(() => new Promise<void>((r) => setTimeout(r, 300)));
    const ev = new Event('touchmove', { cancelable: true, bubbles: true });
    window.dispatchEvent(ev);
    expect(ev.defaultPrevented).toBe(true);
    fireEvent.keyDown(window, { key: 'Escape' });
    await settle();
  });

  it('scrolls the board sideways near its right edge and a column down near its bottom', async () => {
    server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    const board = screen.getByRole('group', { name: 'Board' });
    const ul = column('Todo').querySelector<HTMLElement>('[data-cards]')!;
    press(cardEl(1), middle(0, 0));
    move({ x: 1495, y: 300 });
    await waitFor(() => expect(board.scrollLeft).toBeGreaterThan(0));
    move({ x: 440, y: 595 }); // the bottom edge of the Todo column
    await waitFor(() => expect(ul.scrollTop).toBeGreaterThan(0));
    fireEvent.keyDown(window, { key: 'Escape' });
    await settle();
  });
});

describe('prefers-reduced-motion', () => {
  const orig = window.matchMedia;
  afterEach(() => {
    window.matchMedia = orig;
  });
  it('lands at once and still drops', async () => {
    window.matchMedia = ((q: string) => ({
      matches: q.includes('reduce'),
      media: q,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
      onchange: null,
    })) as typeof window.matchMedia;
    const { api } = server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), at(1, 1));
    await waitFor(() => expect(patches(api)).toHaveLength(1));
    await waitFor(() => expect(names('Todo')).toEqual(['Card 2', 'Card 1', 'Card 3', 'Card 4']));
  });
});

describe('board drag: review fixes', () => {
  const live = () => screen.getByTestId('board-live');
  const noClone = () => expect(document.querySelector('[class*="dragClone"]')).toBeNull();

  it('lost pointer capture cancels: the card goes back, nothing is sent, it is announced', async () => {
    const { api } = server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), at(1, 1), false);
    fireEvent.lostPointerCapture(window, { ...ptr });
    await settle();
    expect(patches(api)).toHaveLength(0);
    noClone();
    expect(live()).toHaveTextContent('Move of ORB-1 canceled');
  });

  it('the window losing focus cancels the drag', async () => {
    const { api } = server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), at(1, 1), false);
    fireEvent.blur(window);
    await settle();
    expect(patches(api)).toHaveLength(0);
    noClone();
    expect(live()).toHaveTextContent('Move of ORB-1 canceled');
  });

  it('a drop outside the board is announced as canceled', async () => {
    server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), { x: 700, y: 900 });
    await settle();
    expect(live()).toHaveTextContent('Move of ORB-1 canceled');
  });

  it('a drop back at the same spot is announced as canceled', async () => {
    const { api } = server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 3');
    dragTo(3, middle(1, 1), at(1, 1));
    await settle();
    expect(patches(api)).toHaveLength(0);
    expect(live()).toHaveTextContent('Move of ORB-3 canceled');
  });

  it('the source card stays hidden when React re-renders it mid-drag', async () => {
    server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), at(1, 1), false);
    fireEvent.mouseDown(cardEl(1)); // focuses the card: React rewrites its className
    expect(cardEl(1).className).toMatch(/focused/);
    expect(cardEl(1)).toHaveAttribute('data-drag-source');
    fireEvent.keyDown(window, { key: 'Escape' });
    await settle();
    release(at(1, 1));
  });

  it('a click long after Escape (the button let go late) does not open the issue', async () => {
    server(start());
    renderApp('/orbit/issues?view=board');
    await screen.findByText('Card 1');
    dragTo(1, middle(0, 0), at(1, 1), false);
    fireEvent.keyDown(window, { key: 'Escape' });
    await act(() => new Promise<void>((r) => setTimeout(r, 450))); // longer than the 350 ms timer
    release(at(1, 1));
    fireEvent.click(within(cardEl(1)).getByRole('link'), { detail: 1 });
    await settle();
    expect(screen.getByRole('group', { name: 'Board' })).toBeInTheDocument();
    // and after the guard ran out, a click works again
    await act(() => new Promise<void>((r) => setTimeout(r, 400)));
    fireEvent.click(within(cardEl(1)).getByRole('link'), { detail: 1 });
    await waitFor(() => expect(screen.queryByRole('group', { name: 'Board' })).not.toBeInTheDocument());
  });

  it('with no neighbour the optimistic move puts the card on TOP of its new column', () => {
    const qc = new QueryClient();
    const a = card(2, 'Todo');
    const b = card(3, 'Todo');
    const moved = card(1, 'Todo'); // its state is already patched to Todo
    const key = ['org', 'orbit', 'issues', 'list', { orderBy: 'manual' }];
    qc.setQueryData(key, { pages: [{ items: [a, b, moved], nextCursor: null }], pageParams: [undefined] });
    moveIssueInCaches(qc, 'orbit', moved.id, {});
    const d = qc.getQueryData<{ pages: { items: Issue[] }[] }>(key)!;
    expect(d.pages[0]!.items.map((i) => i.id)).toEqual(['i1', 'i2', 'i3']);
  });
});
