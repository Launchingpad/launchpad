import type { MouseEvent } from 'react';

// LAUN-29: a board drag ends with a pointerup, and the browser may then send a click to the card
// (the press and the release are on the same card or its list). That click must not open the issue.
// The drag calls markDragged() when it ends; for a short time after, a click is ignored.
const JUST_DRAGGED_MS = 350;
let draggedAt = Number.NEGATIVE_INFINITY;
// True from the start of a drag until its pointer is released. An Escape ends a drag while the button
// is still down; the click comes when it is let go, however late (so a timer alone is not enough).
let dragActive = false;
export function setDragActive(on: boolean) {
  dragActive = on;
}
export function markDragged(now: number = performance.now()) {
  draggedAt = now;
}
export function justDragged(now: number = performance.now()): boolean {
  return dragActive || now - draggedAt < JUST_DRAGGED_MS;
}
/** For tests. */
export function resetDragged() {
  draggedAt = Number.NEGATIVE_INFINITY;
  dragActive = false;
}

// LAUN-50: the title link is stretched over its whole card or row. This is its click rule.
// Modified and non-primary clicks keep the browser's behaviour (new tab, new window).
// A mouse click that ends a text selection made inside the same card or row is not a click
// (a keyboard Enter has detail 0 and is never held back). On a phone in select mode a tap
// toggles the selection instead of opening the issue.
export function onCardLinkClick(e: MouseEvent, selecting: boolean, toggle: () => void) {
  if (e.detail > 0 && justDragged()) {
    e.preventDefault(); // the click that ends a drag is not a click (LAUN-29)
    return;
  }
  if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
  if (e.detail > 0 && selectionInside(e.currentTarget.closest('[data-issue-id]'))) {
    e.preventDefault();
    return;
  }
  if (
    selecting &&
    typeof window.matchMedia === 'function' &&
    window.matchMedia('(max-width: 760px)').matches
  ) {
    e.preventDefault();
    toggle();
  }
}

function selectionInside(item: Element | null): boolean {
  const sel = typeof window.getSelection === 'function' ? window.getSelection() : null;
  if (!item || !sel || sel.isCollapsed || sel.toString().trim() === '') return false;
  return !!sel.anchorNode && item.contains(sel.anchorNode);
}

// The tooltip-bearing spans and the identifier sit above the overlay (so the title attribute shows and
// the id can be copied). A plain click on one of them is forwarded to the title link, so it still opens
// the issue. Controls and the link itself are left alone.
export function onCardClick(e: MouseEvent<HTMLElement>) {
  if (justDragged()) return;
  if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.defaultPrevented) return;
  const t = e.target as Element;
  // A click on the item itself is a press and a release on different children (a drag): not a click.
  if (t === e.currentTarget) return;
  if (t.closest('a, button, input, select, textarea, label')) return;
  if (e.detail > 0 && selectionInside(e.currentTarget)) return; // the end of a text selection
  e.currentTarget.querySelector<HTMLAnchorElement>('a[href]')?.click();
}
