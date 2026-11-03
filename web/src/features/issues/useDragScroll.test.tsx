import { createEvent, fireEvent, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { at, middle, stubBoardLayout } from '../../test/boardLayout';
import { baseRoutes, makeIssue, mockApi, states } from '../../test/mockApi';
import { renderApp } from '../../test/render';
import { justDragged, resetDragged } from './cardLink';
import { DRAG_SCROLL_THRESHOLD, MOMENTUM_MAX_PX } from './useDragScroll';

// LAUN-55. jsdom has no layout, so the board's scrollLeft is a plain number here. The real mouse drag in
// a browser is checked by hand (docs/internal/web-decisions.md).

const ref = (name: string) => {
  const s = states.find((x) => x.name === name)!;
  return { id: s.id, name: s.name, type: s.type, color: s.color };
};
const cards = [
  makeIssue(1, { state: ref('Backlog'), title: 'Card 1' }),
  makeIssue(2, { state: ref('Todo'), title: 'Card 2' }),
];
const mouse = { pointerId: 1, pointerType: 'mouse', isPrimary: true };
const board = () => document.querySelector('[role=group][aria-label=Board]') as HTMLElement;
const column = (name: string) => document.querySelector(`[data-column="${name}"]`) as HTMLElement;

let restore: () => void;
let scroll = 0;
let rafs: Array<(t: number) => void> = [];
beforeEach(() => {
  restore = stubBoardLayout();
  resetDragged();
  scroll = 500;
  clock = 0;
  rafs = [];
  vi.stubGlobal('requestAnimationFrame', (f: (t: number) => void) => rafs.push(f));
  vi.stubGlobal('cancelAnimationFrame', () => (rafs = []));
});
afterEach(() => {
  restore();
  vi.unstubAllGlobals();
});

async function open() {
  mockApi({
    ...baseRoutes(),
    'GET /api/v1/orgs/:org/issues': () => ({ json: { items: cards, nextCursor: null } }),
  });
  renderApp('/orbit/issues?view=board');
  await screen.findByText('Card 1');
  Object.defineProperty(board(), 'scrollLeft', {
    configurable: true,
    get: () => scroll,
    set: (v) => (scroll = v),
  });
}
function down(el: Element, x: number, extra: object = {}, t = 0) {
  const e = createEvent.pointerDown(el, { ...mouse, button: 0, clientX: x, clientY: 300, ...extra });
  Object.defineProperty(e, 'timeStamp', { value: t });
  fireEvent(el, e);
}
let clock = 0; // each event is 1 s after the last unless a time is given: no momentum
function move(x: number, t: number = (clock += 1000), extra: object = {}) {
  const e = createEvent.pointerMove(window, { ...mouse, clientX: x, clientY: 300, ...extra });
  Object.defineProperty(e, 'timeStamp', { value: t });
  fireEvent(window, e);
}
function up(x: number, t: number = (clock += 1000)) {
  const e = createEvent.pointerUp(window, { ...mouse, clientX: x, clientY: 300 });
  Object.defineProperty(e, 'timeStamp', { value: t });
  fireEvent(window, e);
}
const grabbing = () => board().className.includes('dragScrolling');
const reduceMotion = () =>
  vi.stubGlobal('matchMedia', (q: string) => ({
    matches: q.includes('reduce'),
    media: q,
    addEventListener() {},
    removeEventListener() {},
    addListener() {},
    removeListener() {},
    onchange: null,
    dispatchEvent: () => false,
  }));

describe('drag on empty space scrolls the board (LAUN-55)', () => {
  it('follows the pointer from the press, on the board itself, a gap or an empty column', async () => {
    await open();
    for (const el of [board(), column('Done')]) {
      scroll = 500;
      down(el, 600);
      move(600 - DRAG_SCROLL_THRESHOLD + 1); // under the threshold: nothing
      expect(scroll).toBe(500);
      expect(grabbing()).toBe(false);
      move(550);
      expect(grabbing()).toBe(true);
      expect(scroll).toBe(550); // the whole distance from the press, not from the threshold
      move(700);
      expect(scroll).toBe(400);
      up(700); // slow events: no momentum
      expect(grabbing()).toBe(false);
      expect(rafs).toHaveLength(0);
    }
  });

  it('a plain click on empty space does nothing', async () => {
    await open();
    down(column('Done'), 600);
    up(600);
    expect(scroll).toBe(500);
    expect(grabbing()).toBe(false);
    expect(justDragged()).toBe(false);
  });

  it('a press on a card is the card drag, not a scroll', async () => {
    await open();
    const card = document.querySelector('[data-issue-id="i1"]') as HTMLElement;
    const p = middle(0, 0);
    down(card, p.x);
    move(p.x + 40);
    move(at(1, 0).x);
    expect(scroll).toBe(500);
    expect(grabbing()).toBe(false);
    up(at(1, 0).x);
  });

  it.each([
    ['the title link', () => screen.getByText('Card 1')],
    ['a checkbox', () => screen.getByLabelText('Select ORB-1')],
  ])('a press on %s does not start it', async (_n, get) => {
    await open();
    down(get(), 600);
    move(500);
    move(400);
    expect(scroll).toBe(500);
    expect(grabbing()).toBe(false);
    up(400);
  });

  it('a press on a scrollbar does not start it', async () => {
    await open();
    const b = board();
    // A 15 px horizontal scrollbar under the content: the press is below clientHeight.
    for (const [k, v] of Object.entries({
      offsetHeight: 600,
      clientHeight: 585,
      offsetWidth: 800,
      clientWidth: 800,
    })) {
      Object.defineProperty(b, k, { configurable: true, value: v });
    }
    b.getBoundingClientRect = () =>
      ({ left: 0, top: 0, right: 800, bottom: 600, width: 800, height: 600 }) as DOMRect;
    fireEvent.pointerDown(b, { ...mouse, button: 0, clientX: 600, clientY: 595 });
    move(500);
    move(400);
    expect(scroll).toBe(500);
    up(400);
    // Above the scrollbar it starts.
    fireEvent.pointerDown(b, { ...mouse, button: 0, clientX: 600, clientY: 300 });
    move(500);
    expect(scroll).toBe(600);
    up(500);
  });

  it('ignores touch, a right button and modifier keys', async () => {
    await open();
    down(column('Done'), 600, { pointerType: 'touch' });
    move(500, undefined, { pointerType: 'touch' });
    expect(grabbing()).toBe(false);
    down(column('Done'), 600, { button: 2 });
    move(500);
    expect(grabbing()).toBe(false);
    down(column('Done'), 600, { shiftKey: true });
    move(500);
    expect(grabbing()).toBe(false);
    expect(scroll).toBe(500);
  });

  it('works with a pen', async () => {
    await open();
    down(column('Done'), 600, { pointerType: 'pen' });
    move(560, undefined, { pointerType: 'pen' });
    expect(scroll).toBe(540);
    up(560);
  });

  it('no text selection: selectstart is cancelled during a press', async () => {
    await open();
    down(column('Done'), 600);
    move(500);
    expect(fireEvent(document.body, new Event('selectstart', { bubbles: true, cancelable: true }))).toBe(
      false,
    );
    up(500);
    expect(fireEvent(document.body, new Event('selectstart', { bubbles: true, cancelable: true }))).toBe(
      true,
    );
  });

  it('a click after the scroll is guarded, and the guard holds until release', async () => {
    await open();
    down(column('Done'), 600);
    move(500);
    expect(justDragged()).toBe(true);
    up(500);
    expect(justDragged()).toBe(true); // 350 ms after the release
    resetDragged();
    expect(justDragged()).toBe(false);
  });

  it('Escape ends it, keeps the place and does not reach the page', async () => {
    await open();
    const page = vi.fn();
    document.addEventListener('keydown', page);
    down(column('Done'), 600);
    move(500);
    fireEvent.keyDown(window, { key: 'Escape' });
    expect(grabbing()).toBe(false);
    expect(page).not.toHaveBeenCalled();
    move(300);
    expect(scroll).toBe(600); // no longer following
    expect(justDragged()).toBe(true); // the button is still down: its click is held back
    up(300);
    document.removeEventListener('keydown', page);
  });

  it.each([
    ['blur', () => fireEvent.blur(window)],
    ['pointercancel', () => fireEvent.pointerCancel(window, { ...mouse })],
    ['lostpointercapture', () => fireEvent(window, new Event('lostpointercapture'))],
  ])('%s ends it cleanly', async (_n, fire) => {
    await open();
    down(column('Done'), 600);
    move(500);
    fire();
    expect(grabbing()).toBe(false);
    move(300);
    expect(scroll).toBe(600);
    up(300);
    expect(justDragged()).toBe(true); // the 350 ms after the release
    resetDragged();
    // A new press works afterwards.
    down(column('Done'), 600);
    move(500);
    expect(grabbing()).toBe(true);
    up(500);
  });

  it('a release outside the window (a pointerup on the window) ends it', async () => {
    await open();
    down(column('Done'), 600);
    move(500);
    up(-50);
    expect(grabbing()).toBe(false);
    move(300);
    expect(scroll).toBe(600); // no longer following
  });

  it('the other pointer is ignored', async () => {
    await open();
    down(column('Done'), 600);
    move(500, undefined, { pointerId: 7 });
    expect(grabbing()).toBe(false);
    up(500);
  });
});

describe('momentum (LAUN-55)', () => {
  const flick = () => {
    down(column('Done'), 600, {}, 950);
    move(560, 1000);
    move(500, 1050);
    move(400, 1100); // 200 px in 100 ms to the left: pointer 2 px/ms
    up(400, 1110);
  };
  const run = (ms: number) => {
    let t = 0;
    for (let i = 0; i < 400 && rafs.length; i++) {
      const f = rafs.shift()!;
      f(t);
      t += ms;
    }
  };

  it('a quick release coasts a little, decays and stops, never past the cap', async () => {
    await open();
    flick();
    const released = scroll;
    expect(released).toBe(700);
    expect(rafs).toHaveLength(1);
    run(16);
    expect(scroll).toBeGreaterThan(released);
    expect(scroll - released).toBeLessThanOrEqual(MOMENTUM_MAX_PX);
    expect(rafs).toHaveLength(0); // it ended by itself
  });

  it('a slow release, or a pointer that stopped before the release, does not coast', async () => {
    await open();
    down(column('Done'), 600, {}, 900);
    move(590, 1000);
    move(580, 1100); // 0.1 px/ms
    up(580, 1110);
    expect(rafs).toHaveLength(0);
    down(column('Done'), 600, {}, 900);
    move(500, 1000);
    move(400, 1100);
    up(400, 1500); // held still for 400 ms
    expect(rafs).toHaveLength(0);
  });

  it('prefers-reduced-motion: no momentum', async () => {
    await open();
    reduceMotion();
    flick();
    expect(rafs).toHaveLength(0);
  });

  it('stops when something else scrolls the board', async () => {
    await open();
    flick();
    rafs.shift()!(0);
    rafs.shift()!(16);
    scroll = 100; // a wheel or a script
    rafs.shift()!(32);
    expect(scroll).toBe(100);
    expect(rafs).toHaveLength(0);
  });

  it('a new press stops the coast', async () => {
    await open();
    flick();
    expect(rafs).toHaveLength(1);
    down(column('Done'), 300);
    expect(rafs).toHaveLength(0);
    up(300);
  });
});
