// LAUN-111: the pure part of the list drag. No DOM here: every function takes numbers and ids.
// The list stacks its groups (one per state) and each row is one line high, so the target is found
// by the pointer's y alone (the board, boardDrag.ts / dropPosition.ts, finds a column by x first).
// The client never computes a rank: it names a loaded neighbour (placeAfter / placeBefore).
import { dropIndex, placementAt, type PlaceCard, type Placement, type Slot } from './dropPosition';

/** One group of the list. All numbers are in the list's own coordinates (the top of the list is 0). */
export interface ListGroupBox {
  key: string;
  top: number;
  bottom: number;
  /** The loaded rows of the group WITHOUT the dragged row, in order. */
  slots: Slot[];
}

export interface ListTarget {
  /** Index into the groups. */
  group: number;
  /** The number of rows (the dragged one left out) that stay above the drop. */
  index: number;
}

/**
 * The group and the place under a pointer at `y`. The space between two groups and a group's own head
 * belong to the group they sit in; a pointer above the first group lands at its top, below the last
 * group at its end. A group is always the one whose bottom is the first one below `y`. No groups: null.
 */
export function listTarget(y: number, groups: readonly ListGroupBox[]): ListTarget | null {
  if (groups.length === 0) return null;
  let g = groups.length - 1;
  for (let k = 0; k < groups.length; k++) {
    if (y < groups[k]!.bottom) {
      g = k;
      break;
    }
  }
  const box = groups[g]!;
  // A defensive duplicate: a pointer above the first row has no centre above it, so dropIndex gives 0 too.
  // Kept so the head rule does not depend on dropIndex's counting.
  const first = box.slots[0];
  if (first && y < first.top) return { group: g, index: 0 };
  return { group: g, index: dropIndex(y, box.slots) };
}

/**
 * Where the drop line is drawn: the top edge of the row that will sit below the drop, else the bottom
 * edge of the last row. A group with no other row (only the dragged one): `fallback`.
 */
export function gapTop(box: ListGroupBox, index: number, fallback: number): number {
  const s = box.slots;
  if (s.length === 0) return fallback;
  if (index < s.length) return s[index]!.top;
  const last = s[s.length - 1]!;
  return last.top + last.height;
}

const same = (a: Placement, b: Placement) => a.placeAfter === b.placeAfter && a.placeBefore === b.placeBefore;

/**
 * The keyboard move: one place up (-1) or down (+1) inside a group. `cards` is the group's loaded rows
 * in order, the held one included. A row of another team is stepped over (a rank only means something
 * next to the same team, see placementAt). null at the edge of the loaded rows, or when there is
 * nothing to name: the keyboard never reaches into rows that are not loaded.
 */
export function stepPlacement(
  cards: readonly PlaceCard[],
  id: string,
  dir: -1 | 1,
): { placement: Placement; index: number } | null {
  const at = cards.findIndex((c) => c.id === id);
  if (at < 0) return null;
  const teamId = cards[at]!.teamId;
  const rest = cards.filter((c) => c.id !== id);
  const now = placementAt(rest, at, teamId);
  for (let t = at + dir; t >= 0 && t <= rest.length; t += dir) {
    const placement = placementAt(rest, t, teamId);
    if (!same(placement, now)) return { placement, index: t };
  }
  return null;
}
