import { fireEvent } from '@testing-library/react';
import { vi } from 'vitest';

// jsdom has no layout. This gives a board a fake one so the pointer drag (boardDrag.ts) can be tested:
// columns 280 wide every 300 px, a 40 px header, cards 40 high every 46 px (6 px gap), the board 1500 x 600.
// A card's content top is index * 46; its client top is 40 + that - the column's scrollTop.
export const CARD_H = 40;
export const PITCH = 46;
export const HEAD = 40;
export const COL_W = 300;

const colIndex = (section: Element) =>
  Array.from(section.parentElement!.querySelectorAll(':scope > section[data-column]')).indexOf(section);
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

export function stubBoardLayout(): () => void {
  const proto = HTMLElement.prototype;
  const top = Object.getOwnPropertyDescriptor(proto, 'offsetTop');
  const height = Object.getOwnPropertyDescriptor(proto, 'offsetHeight');
  Object.defineProperty(proto, 'offsetTop', {
    configurable: true,
    get(this: HTMLElement) {
      const ul = this.parentElement;
      if (!ul?.matches('[data-cards]') || !this.dataset.issueId) return 0;
      return Array.from(ul.querySelectorAll(':scope > li[data-issue-id]')).indexOf(this) * PITCH;
    },
  });
  Object.defineProperty(proto, 'offsetHeight', {
    configurable: true,
    get(this: HTMLElement) {
      return this.dataset.issueId ? CARD_H : 0;
    },
  });
  const spy = vi.spyOn(proto, 'getBoundingClientRect').mockImplementation(function (this: HTMLElement) {
    if (this.matches('[role="group"][aria-label="Board"]')) return rect(0, 0, 1500, 600);
    const section = this.closest('section[data-column]');
    if (this === section) return rect(colIndex(this) * COL_W, 0, 280, 600);
    if (section && this.matches('[data-cards]')) return rect(colIndex(section) * COL_W, HEAD, 280, 560);
    if (section && this.parentElement?.matches('[data-cards]')) {
      const ul = this.parentElement;
      const x = colIndex(section) * COL_W + 8;
      if (this.dataset.issueId) return rect(x, HEAD + this.offsetTop - ul.scrollTop, 264, CARD_H);
      if (this.dataset.gap !== undefined)
        return rect(x, HEAD + (parseFloat(this.style.top) || 0) - ul.scrollTop, 264, CARD_H);
    }
    return rect(0, 0, 0, 0);
  });
  return () => {
    spy.mockRestore();
    if (top) Object.defineProperty(proto, 'offsetTop', top);
    else delete (proto as unknown as Record<string, unknown>).offsetTop;
    if (height) Object.defineProperty(proto, 'offsetHeight', height);
    else delete (proto as unknown as Record<string, unknown>).offsetHeight;
  };
}

/** Client coordinates for "the dragged card's centre sits at content place `t` of column `col`". */
export const at = (col: number, t: number) => ({ x: col * COL_W + 140, y: HEAD + t * PITCH });
/** The middle of card number `k` of column `col`. */
export const middle = (col: number, k: number) => ({
  x: col * COL_W + 140,
  y: HEAD + k * PITCH + CARD_H / 2,
});

/** A real pointer drag of a card with the mouse: press, a first move past the threshold, the move, the release. */
export function dragCard(card: HTMLElement, grab: { x: number; y: number }, to: { x: number; y: number }) {
  const ptr = { pointerId: 1, pointerType: 'mouse', isPrimary: true };
  fireEvent.pointerDown(card, { ...ptr, button: 0, clientX: grab.x, clientY: grab.y });
  fireEvent.pointerMove(window, { ...ptr, clientX: grab.x + 10, clientY: grab.y + 10 });
  fireEvent.pointerMove(window, { ...ptr, clientX: to.x, clientY: to.y });
  fireEvent.pointerUp(window, { ...ptr, clientX: to.x, clientY: to.y });
}
