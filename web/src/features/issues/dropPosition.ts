// LAUN-29: the pure part of the board drag. No DOM here: every function takes numbers and ids.
// The client never computes a rank. It names a neighbour (placeAfter / placeBefore) and the server
// does the maths (docs/internal/decisions-T14-manual-order.md).

/** One card of a column, in the column's scroll-content coordinates, WITHOUT the dragged card. */
export interface Slot {
  id: string;
  top: number;
  height: number;
}

/**
 * Where the dragged card goes: the number of cards whose centre is above `y`. `slots` are in column
 * order and leave the dragged card out. Both are measured in the closed layout (no gap open), so
 * opening the gap never moves the thresholds and the gap does not flicker between two places.
 */
export function dropIndex(y: number, slots: readonly Slot[]): number {
  let n = 0;
  for (const s of slots) {
    if (y > s.top + s.height / 2) n++;
    else break;
  }
  return n;
}

export interface PlaceCard {
  id: string;
  teamId: string;
}

export interface Placement {
  placeAfter?: string;
  placeBefore?: string;
}

/**
 * The neighbour to send. `cards` is the destination column without the dragged card, `index` the
 * drop index. A rank only has a meaning next to cards of the same team, so a card of another team
 * (an "all issues" board mixes teams in one column) is skipped: the nearest card of the dragged
 * issue's team above the gap wins (`placeAfter`), else the nearest one below (`placeBefore`).
 * Nothing to name (an empty column, or no card of that team): no placement, and the server puts a
 * moved card on top of its new column.
 */
export function placementAt(cards: readonly PlaceCard[], index: number, teamId: string): Placement {
  for (let i = Math.min(index, cards.length) - 1; i >= 0; i--) {
    if (cards[i]!.teamId === teamId) return { placeAfter: cards[i]!.id };
  }
  for (let i = Math.max(index, 0); i < cards.length; i++) {
    if (cards[i]!.teamId === teamId) return { placeBefore: cards[i]!.id };
  }
  return {};
}

/** Pixels to scroll this frame for a pointer at `pos` in the span [start, end]. 0 away from the edges. */
export function edgeScroll(pos: number, start: number, end: number, zone = 56, max = 22): number {
  if (end - start < zone * 2) zone = (end - start) / 2; // a tiny box: the two zones meet in the middle
  if (zone <= 0) return 0;
  if (pos < start + zone) return -Math.round(max * Math.min(1, (start + zone - pos) / zone));
  if (pos > end - zone) return Math.round(max * Math.min(1, (pos - (end - zone)) / zone));
  return 0;
}

/** The column whose horizontal span holds `x`, from the column rects (left, right) in board order. */
export function columnAt(
  x: number,
  spans: readonly { key: string; left: number; right: number }[],
): string | null {
  for (const s of spans) if (x >= s.left && x < s.right) return s.key;
  return null;
}
