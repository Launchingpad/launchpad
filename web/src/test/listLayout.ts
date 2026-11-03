import { fireEvent } from '@testing-library/react';
import { vi } from 'vitest';

// jsdom has no layout. This gives the issue LIST a fake one so the handle drag (listDrag.ts) can be tested:
// the scroller is 800 x 600 at the origin; groups are stacked in order, each a 30 px head, rows 36 px high,
// and 4 px between two groups. The list wrapper (`[data-list]`) sits at the top of the scroller.
export const ROW_H = 36;
export const HEAD_H = 30;
export const GROUP_GAP = 4;

const rect = (left: number, top: number, width: number, height: number): DOMRect =>
  ({
    left,
    top,
    width,
    height,
    right: left + width,
    bottom: top + height,
    x: left,
    y: top,
    toJSON: () => ({}),
  }) as DOMRect;

/** The y of the top of every group and row, from the DOM, as the browser would stack them. */
function layout(list: Element) {
  const tops = new Map<Element, { top: number; height: number }>();
  let y = 0;
  for (const sec of Array.from(list.querySelectorAll('section[data-group]'))) {
    const start = y;
    y += HEAD_H;
    for (const li of Array.from(sec.querySelectorAll('li[data-issue-id]'))) {
      tops.set(li, { top: y, height: ROW_H });
      y += ROW_H;
    }
    tops.set(sec, { top: start, height: y - start });
    y += GROUP_GAP;
  }
  return tops;
}

/** Move every group and row down by `dy` px (a reflow after a window resize); 0 puts them back. */
let shiftY = 0;
export const shiftListLayout = (dy: number) => (shiftY = dy);

export function stubListLayout(): () => void {
  const spy = vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(function (
    this: HTMLElement,
  ) {
    if (this.matches('[data-testid="issues-scroll"]')) return rect(0, 0, 800, 600);
    if (this.matches('[data-list]')) return rect(0, 0, 800, 2000);
    const list = this.closest('[data-list]');
    if (list) {
      const box = layout(list).get(this);
      if (box) return rect(0, box.top + shiftY, 800, box.height);
    }
    return rect(0, 0, 0, 0);
  });
  shiftY = 0;
  return () => spy.mockRestore();
}

/** Client y of the middle of a row. */
export const rowMiddle = (id: string) => {
  const li = document.querySelector(`[data-issue-id="${id}"]`)!;
  const box = layout(li.closest('[data-list]')!).get(li)!;
  return { x: 100, y: box.top + ROW_H / 2 };
};

const handleOf = (id: string) =>
  document.querySelector(`[data-issue-id="${id}"] [data-drag-handle]`) as HTMLElement;

/** A real pointer drag of a row's handle: press, a first move past the threshold, the move, the release. */
export function dragHandle(
  id: string,
  to: { x: number; y: number },
  opts: { pointerType?: string; release?: boolean } = {},
) {
  const grab = { x: 15, y: rowMiddle(id).y };
  const ptr = { pointerId: 1, pointerType: opts.pointerType ?? 'mouse', isPrimary: true };
  fireEvent.pointerDown(handleOf(id), { ...ptr, button: 0, clientX: grab.x, clientY: grab.y });
  fireEvent.pointerMove(window, { ...ptr, clientX: grab.x + 2, clientY: grab.y + 10 });
  fireEvent.pointerMove(window, { ...ptr, clientX: to.x, clientY: to.y });
  if (opts.release !== false) fireEvent.pointerUp(window, { ...ptr, clientX: to.x, clientY: to.y });
}
export { handleOf };
