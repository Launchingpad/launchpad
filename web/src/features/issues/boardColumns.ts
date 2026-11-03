import type { Group } from '../../lib/group';
import type { GroupKey } from './filters';
import { cleanHidden, isKindEntry, type ColumnPrefs } from './viewPrefs';

/**
 * The name a column is stored under in `hidden`. A state column keeps its bare name (LAUN-163); a column of
 * another group-by is `<groupBy>:<key>` (LAUN-171), so each group kind has its own list in the one field.
 */
export const hiddenName = (kind: GroupKey, g: Group): string =>
  kind === 'state' ? g.name : `${kind}:${g.key}`;

/** The stored entries that belong to this group kind. */
export const entriesOf = (kind: GroupKey, hidden: string[]): string[] =>
  hidden.filter((n) => (kind === 'state' ? !isKindEntry(n) : n.startsWith(`${kind}:`)));

/**
 * LAUN-163: which board columns show. A column is hidden when the person hid it, or when "hide empty" is on
 * and it holds no card. The cards of a hidden column are not lost: `hiddenIssues` counts them, so a total can still
 * name them. `kind` is the board's group-by (LAUN-171); each kind has its own hidden list.
 */
export function applyColumns(
  all: Group[],
  prefs: ColumnPrefs,
  kind: GroupKey = 'state',
): { shown: Group[]; hiddenColumns: number; hiddenIssues: number } {
  const hidden = new Set(prefs.hidden);
  const shown: Group[] = [];
  let hiddenColumns = 0;
  let hiddenIssues = 0;
  for (const g of all) {
    if (hidden.has(hiddenName(kind, g)) || (prefs.hideEmpty && g.issues.length === 0)) {
      hiddenColumns += 1;
      hiddenIssues += g.issues.length;
    } else shown.push(g);
  }
  return { shown, hiddenColumns, hiddenIssues };
}

/**
 * Hide or show one column. The last column that is not hidden by choice cannot be hidden: a board needs
 * one column. Returns the same prefs when the change is refused.
 */
export function toggleColumn(
  all: Group[],
  prefs: ColumnPrefs,
  column: string | Group,
  kind: GroupKey = 'state',
): { prefs: ColumnPrefs; refused: boolean; reason?: 'last' | 'cap' } {
  // A name (a state column, as before) or the column itself (any group-by: two columns may share a name).
  const stored = (g: Group) => hiddenName(kind, g);
  const target = typeof column === 'string' ? all.find((g) => g.name === column) : column;
  const entry = target ? stored(target) : String(column);
  if (prefs.hidden.includes(entry))
    return { prefs: { ...prefs, hidden: prefs.hidden.filter((n) => n !== entry) }, refused: false };
  const left = all.filter((g) => stored(g) !== entry && !prefs.hidden.includes(stored(g))).length;
  if (left === 0) return { prefs, refused: true, reason: 'last' };
  // Entries of this kind for columns that no longer exist are dropped; other kinds' entries stay.
  const live = new Set(all.map(stored));
  const mine = new Set(entriesOf(kind, prefs.hidden));
  const hidden = cleanHidden([...prefs.hidden.filter((n) => !mine.has(n) || live.has(n)), entry]);
  const refused = !hidden.includes(entry);
  return refused ? { prefs, refused, reason: 'cap' } : { prefs: { ...prefs, hidden }, refused };
}
