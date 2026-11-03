// LAUN-236, LAUN-237: the undo and redo stacks of the board and the list (docs/internal/decisions-T43-board-undo.md).
// Pure: no React, no API. The hook (useIssueUndo.tsx) makes the writes and tells this module the outcome.

/** The values one undo or one redo writes on one issue. Only the fields the change touched are set. */
export interface FieldSet {
  /** A state ID of the issue's team (the API takes a name or an id; an id survives a rename). */
  state?: string;
  priority?: number;
  /** A username, or null for "no assignee". The API takes no member id here. */
  assignee?: string | null;
  /** Label ids (the API takes a name or an id; an id survives a rename). */
  addLabels?: string[];
  removeLabels?: string[];
  /** The manual rank (decisions-T14): the place in the column goes back exactly, with no neighbour to name. */
  sortOrder?: number;
  /** Names for the toast only, as they were when the change was made. Never sent. */
  words?: { state?: string; add?: string[]; remove?: string[] };
}

/** The body of the write: the FieldSet without its words. */
export function bodyOf(f: FieldSet): Omit<FieldSet, 'words'> {
  const { words: _words, ...rest } = f;
  return rest;
}

/** One issue's part of an action. */
export interface UndoChange {
  issueId: string;
  identifier: string;
  teamId: string;
  /** Write this to go back. For a create it is filled in when the undo runs (the team's canceled state). */
  undo: FieldSet;
  /** Write this to go forward again. */
  redo: FieldSet;
  /** The issue version after the last write of this change: the next write sends it (compare-and-swap). */
  version: number;
  /** The action created the issue: the undo cancels it (a member cannot delete, decisions-T32). */
  created?: boolean;
}

/** One action: what one key press undoes. A group drop or a picker on a selection holds several changes. */
export interface UndoEntry {
  id: number;
  changes: UndoChange[];
}

export interface UndoStacks {
  undo: UndoEntry[];
  redo: UndoEntry[];
}

export type Direction = 'undo' | 'redo';

/** The stack keeps the last 20 actions; the oldest goes first. */
export const UNDO_CAP = 20;

export const emptyStacks = (): UndoStacks => ({ undo: [], redo: [] });

const capped = (list: UndoEntry[], cap: number) => (list.length > cap ? list.slice(list.length - cap) : list);

/** A new action: on top of the undo stack. It starts a new branch, so the redo stack is emptied. */
export function record(s: UndoStacks, e: UndoEntry, cap = UNDO_CAP): UndoStacks {
  if (!e.changes.length) return s;
  return { undo: capped([...s.undo, e], cap), redo: [] };
}

/** Takes the top entry of one stack. The entry is out of both stacks until `settle` puts it somewhere. */
export function take(s: UndoStacks, dir: Direction): [UndoEntry | undefined, UndoStacks] {
  const list = s[dir];
  const e = list[list.length - 1];
  if (!e) return [undefined, s];
  return [e, { ...s, [dir]: list.slice(0, -1) }];
}

/**
 * What became of one change:
 * - `done`: written; `version` is the issue version the server returned.
 * - `drop`: skipped for good (the card changed since, it is gone or hidden, the server refused).
 * - `keep`: not written for a reason that can pass (the network, the server): try again later.
 */
export type ChangeOutcome = { kind: 'done'; version: number } | { kind: 'drop' } | { kind: 'keep' };

/** Puts `e` into `list` with `below` newer entries still above it. */
const insertBelow = (list: UndoEntry[], e: UndoEntry, below: number) => {
  const at = Math.max(0, list.length - below);
  return [...list.slice(0, at), e, ...list.slice(at)];
};

/**
 * Puts the result of an undo (or a redo) back in the stacks. The done changes move to the other stack as one
 * entry, with their new versions; the kept ones go back on the stack they came from; the dropped ones go.
 * `outcomes` is in the order of `entry.changes`.
 *
 * `newer`: how many new actions were recorded while the undo or redo ran. They stay on top of the undo stack
 * (the next press undoes the newest action). A new action starts a new branch, so the redo stack holds nothing
 * from before it: after an undo the done changes do not go to redo, after a redo the kept ones are dropped.
 */
export function settle(
  s: UndoStacks,
  dir: Direction,
  entry: UndoEntry,
  outcomes: ChangeOutcome[],
  cap = UNDO_CAP,
  newer = 0,
  /** The id of the kept part: a new one, so the done part (which keeps `entry.id`) and it are two entries. */
  keptId = entry.id,
): UndoStacks {
  const done: UndoChange[] = [];
  const kept: UndoChange[] = [];
  entry.changes.forEach((c, i) => {
    const o = outcomes[i] ?? { kind: 'keep' };
    if (o.kind === 'done') done.push({ ...c, version: o.version });
    else if (o.kind === 'keep') kept.push(c);
  });
  let { undo, redo } = s;
  if (dir === 'undo') {
    if (kept.length) undo = capped(insertBelow(undo, { id: keptId, changes: kept }, newer), cap);
    if (done.length && newer === 0) redo = capped([...redo, { ...entry, changes: done }], cap);
  } else {
    if (kept.length && newer === 0) redo = capped([...redo, { id: keptId, changes: kept }], cap);
    if (done.length) undo = capped(insertBelow(undo, { ...entry, changes: done }, newer), cap);
  }
  return { undo, redo };
}

// --- From a write to a change -------------------------------------------------------------------------------

/** The fields of an issue a change compares. `Issue` from the API fits. */
export interface IssueLike {
  id: string;
  identifier: string;
  version: number;
  sortOrder: number;
  priority: number;
  team: { id: string };
  state: { id: string; name: string };
  assignee: { username: string } | null;
  labels: { id: string; name: string }[];
}

/** The part of an update body that board actions send. */
export interface PatchLike {
  state?: string;
  priority?: number;
  assignee?: string | null;
  addLabels?: string[];
  removeLabels?: string[];
  placeAfter?: string;
  placeBefore?: string;
  sortOrder?: number;
}

/** The label of `i` that a patch entry names (by id or, case-insensitive, by name). */
const labelOf = (i: IssueLike, ref: string) =>
  i.labels.find((l) => l.id === ref || l.name.toLowerCase() === ref.toLowerCase());

/**
 * The change one write made, from the issue before (as the page showed it) and the issue the server returned.
 * Only the fields the patch named count, and only when they really changed. Null when nothing changed
 * (nothing to undo).
 */
export function changeOf(before: IssueLike, after: IssueLike, patch: PatchLike): UndoChange | null {
  const undo: FieldSet = {};
  const redo: FieldSet = {};
  let changed = false;
  const stateMoved = patch.state !== undefined && before.state.id !== after.state.id;
  if (stateMoved) {
    undo.state = before.state.id;
    redo.state = after.state.id;
    undo.words = { state: before.state.name };
    redo.words = { state: after.state.name };
    changed = true;
  }
  // A move to another column, or a placement, may give the card a new rank. The rank is ALWAYS recorded then,
  // even when it is the same number: without it the server puts a card that changes column on top (T14), so an
  // undo would not bring it back to its place (a card with rank 0 dropped in an empty column keeps rank 0).
  const placed =
    patch.placeAfter !== undefined || patch.placeBefore !== undefined || patch.sortOrder !== undefined;
  if (stateMoved || placed) {
    undo.sortOrder = before.sortOrder;
    redo.sortOrder = after.sortOrder;
    if (before.sortOrder !== after.sortOrder) changed = true;
  }
  if (patch.priority !== undefined && before.priority !== after.priority) {
    undo.priority = before.priority;
    redo.priority = after.priority;
    changed = true;
  }
  const was = before.assignee?.username ?? null;
  const is = after.assignee?.username ?? null;
  if (patch.assignee !== undefined && was !== is) {
    undo.assignee = was;
    redo.assignee = is;
    changed = true;
  }
  const added = (patch.addLabels ?? [])
    .filter((n) => !labelOf(before, n))
    .map((n) => labelOf(after, n))
    .filter((l): l is { id: string; name: string } => !!l);
  const removed = (patch.removeLabels ?? [])
    .filter((n) => !labelOf(after, n))
    .map((n) => labelOf(before, n))
    .filter((l): l is { id: string; name: string } => !!l);
  if (added.length) {
    undo.removeLabels = added.map((l) => l.id);
    redo.addLabels = added.map((l) => l.id);
    undo.words = { ...undo.words, remove: added.map((l) => l.name) };
    redo.words = { ...redo.words, add: added.map((l) => l.name) };
    changed = true;
  }
  if (removed.length) {
    undo.addLabels = removed.map((l) => l.id);
    redo.removeLabels = removed.map((l) => l.id);
    undo.words = { ...undo.words, add: removed.map((l) => l.name) };
    redo.words = { ...redo.words, remove: removed.map((l) => l.name) };
    changed = true;
  }
  if (!changed) return null;
  return {
    issueId: after.id,
    identifier: after.identifier,
    teamId: after.team.id,
    undo,
    redo,
    version: after.version,
  };
}

/**
 * The change a create made: undo cancels the issue, redo puts it back in the state and at the rank it was
 * created with (without the rank the server would put it on top of its column, T14).
 */
export function createdChange(issue: IssueLike): UndoChange {
  return {
    issueId: issue.id,
    identifier: issue.identifier,
    teamId: issue.team.id,
    undo: {}, // the team's canceled state, chosen when the undo runs
    redo: { state: issue.state.id, sortOrder: issue.sortOrder, words: { state: issue.state.name } },
    version: issue.version,
    created: true,
  };
}

// --- Words for the toast --------------------------------------------------------------------------------------

/** What writing `f` on issue `id` does, in a few words ("moved ORB-12 to Todo"). */
export function describe(
  f: FieldSet,
  id: string,
  names: {
    priority: (p: number) => string;
    member: (username: string) => string;
    /** What holds the rows: "column" on the board (the default), "group" on the list. */
    place?: string;
  },
): string {
  if (f.state !== undefined) return `moved ${id} to ${f.words?.state ?? f.state}`;
  if (f.assignee !== undefined)
    return f.assignee === null ? `unassigned ${id}` : `assigned ${id} to ${names.member(f.assignee)}`;
  if (f.priority !== undefined) return `set ${id} priority to ${names.priority(f.priority)}`;
  if (f.addLabels?.length || f.removeLabels?.length) {
    const add = f.words?.add ?? f.addLabels ?? [];
    const remove = f.words?.remove ?? f.removeLabels ?? [];
    const parts = [
      ...(add.length ? [`added ${add.join(', ')} to ${id}`] : []),
      ...(remove.length ? [`removed ${remove.join(', ')} from ${id}`] : []),
    ];
    return parts.join(' and ');
  }
  if (f.sortOrder !== undefined) return `moved ${id} in its ${names.place ?? 'column'}`;
  return `changed ${id}`;
}

/** The words for a whole entry: one change in full, several as a count ("moved 3 issues to Todo"). */
export function describeEntry(
  fields: { f: FieldSet; id: string }[],
  names: Parameters<typeof describe>[2],
): string {
  if (fields.length === 1) return describe(fields[0]!.f, fields[0]!.id, names);
  const states = new Set(fields.map((x) => x.f.words?.state ?? x.f.state));
  const first = fields[0]!.f;
  if (states.size === 1 && first.state !== undefined)
    return `moved ${fields.length} issues to ${first.words?.state ?? first.state}`;
  return `changed ${fields.length} issues`;
}
