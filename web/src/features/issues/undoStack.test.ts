import { describe, expect, it } from 'vitest';
import {
  bodyOf,
  changeOf,
  createdChange,
  describe as say,
  describeEntry,
  emptyStacks,
  record,
  settle,
  take,
  UNDO_CAP,
  type IssueLike,
  type UndoChange,
  type UndoEntry,
} from './undoStack';
import { asSentence, undoKey } from './useIssueUndo';

// LAUN-236: the pure undo stack of the board.

const ch = (n: number, version = 2): UndoChange => ({
  issueId: `i${n}`,
  identifier: `ORB-${n}`,
  teamId: 'team1',
  undo: { state: 'Backlog' },
  redo: { state: 'Todo' },
  version,
});
const entry = (id: number, ...changes: UndoChange[]): UndoEntry => ({ id, changes });
const ids = (l: UndoEntry[]) => l.map((e) => e.id);

const BACKLOG = { id: 's1', name: 'Backlog' };
const TODO = { id: 's2', name: 'Todo' };
const BUG = { id: 'l-bug', name: 'bug' };
const UI = { id: 'l-ui', name: 'ui' };

const issue = (over: Partial<IssueLike> = {}): IssueLike => ({
  id: 'i1',
  identifier: 'ORB-1',
  version: 1,
  sortOrder: 1024,
  priority: 0,
  team: { id: 'team1' },
  state: BACKLOG,
  assignee: null,
  labels: [],
  ...over,
});

describe('undo stack', () => {
  it('push puts the action on top and empties redo', () => {
    let s = record(emptyStacks(), entry(1, ch(1)));
    s = { ...s, redo: [entry(9, ch(9))] };
    s = record(s, entry(2, ch(2)));
    expect(ids(s.undo)).toEqual([1, 2]);
    expect(s.redo).toEqual([]);
  });

  it('an action with no change is not recorded', () => {
    expect(record(emptyStacks(), entry(1)).undo).toEqual([]);
  });

  it(`keeps the last ${UNDO_CAP} actions`, () => {
    let s = emptyStacks();
    for (let i = 1; i <= 25; i++) s = record(s, entry(i, ch(i)));
    expect(s.undo).toHaveLength(20);
    expect(s.undo[0]!.id).toBe(6);
    expect(s.undo[19]!.id).toBe(25);
  });

  it('undo takes the top; done moves it to redo with the new version; redo moves it back', () => {
    let s = record(record(emptyStacks(), entry(1, ch(1))), entry(2, ch(2)));
    const [e, rest] = take(s, 'undo');
    expect(e!.id).toBe(2);
    expect(ids(rest.undo)).toEqual([1]);
    s = settle(rest, 'undo', e!, [{ kind: 'done', version: 3 }]);
    expect(ids(s.undo)).toEqual([1]);
    expect(ids(s.redo)).toEqual([2]);
    expect(s.redo[0]!.changes[0]!.version).toBe(3);
    const [r, rest2] = take(s, 'redo');
    s = settle(rest2, 'redo', r!, [{ kind: 'done', version: 4 }]);
    expect(ids(s.undo)).toEqual([1, 2]);
    expect(s.redo).toEqual([]);
    expect(s.undo[1]!.changes[0]!.version).toBe(4);
  });

  it('take on an empty stack gives nothing and changes nothing', () => {
    const s = emptyStacks();
    expect(take(s, 'undo')).toEqual([undefined, s]);
    expect(take(s, 'redo')).toEqual([undefined, s]);
  });

  it('a dropped change goes; a kept one goes back where it came from', () => {
    const s0 = record(emptyStacks(), entry(1, ch(1), ch(2), ch(3)));
    const [e, rest] = take(s0, 'undo');
    const s = settle(rest, 'undo', e!, [{ kind: 'done', version: 5 }, { kind: 'drop' }, { kind: 'keep' }]);
    expect(s.redo[0]!.changes.map((c) => c.identifier)).toEqual(['ORB-1']);
    expect(s.undo[0]!.changes.map((c) => c.identifier)).toEqual(['ORB-3']);
    expect(s.undo[0]!.changes[0]!.version).toBe(2); // not written: the old version stays
  });

  it('a split entry: the kept part gets the new id, the done part keeps the old one (two entries, two ids)', () => {
    const s0 = record(emptyStacks(), entry(1, ch(1), ch(2)));
    const [e, rest] = take(s0, 'undo');
    const s = settle(rest, 'undo', e!, [{ kind: 'done', version: 5 }, { kind: 'keep' }], undefined, 0, 9);
    expect(s.redo[0]!.id).toBe(1);
    expect(s.undo[0]!.id).toBe(9);
    const [r, rest2] = take(s, 'redo');
    const t = settle(rest2, 'redo', r!, [{ kind: 'done', version: 6 }, { kind: 'keep' }], undefined, 0, 10);
    expect(t.undo.map((x) => x.id)).toEqual([9, 1]);
  });

  it('an undo that keeps its entry while a new action was made puts it BELOW the new action', () => {
    let s = record(emptyStacks(), entry(1, ch(1)));
    const [e, rest] = take(s, 'undo');
    s = record(rest, entry(2, ch(2))); // made while the undo ran
    s = settle(s, 'undo', e!, [{ kind: 'keep' }], undefined, 1);
    expect(ids(s.undo)).toEqual([1, 2]); // the next press undoes the newest action, 2
    expect(s.redo).toEqual([]);
  });

  it('an undo done while a new action was made: nothing to redo (a new branch)', () => {
    let s = record(emptyStacks(), entry(1, ch(1)));
    const [e, rest] = take(s, 'undo');
    s = record(rest, entry(2, ch(2)));
    s = settle(s, 'undo', e!, [{ kind: 'done', version: 3 }], undefined, 1);
    expect(ids(s.undo)).toEqual([2]);
    expect(s.redo).toEqual([]);
  });

  it('a redo while a new action was made: done goes below the new action, kept is dropped', () => {
    let s = { undo: [], redo: [entry(1, ch(1), ch(3))] } as ReturnType<typeof emptyStacks>;
    const [e, rest] = take(s, 'redo');
    s = record(rest, entry(2, ch(2)));
    s = settle(s, 'redo', e!, [{ kind: 'done', version: 4 }, { kind: 'keep' }], undefined, 1);
    expect(ids(s.undo)).toEqual([1, 2]);
    expect(s.undo[0]!.changes.map((c) => c.identifier)).toEqual(['ORB-1']);
    expect(s.redo).toEqual([]);
  });

  it('all dropped: the entry is gone from both stacks', () => {
    const [e, rest] = take(record(emptyStacks(), entry(1, ch(1))), 'undo');
    const s = settle(rest, 'undo', e!, [{ kind: 'drop' }]);
    expect(s).toEqual(emptyStacks());
  });
});

describe('changeOf: the inverse of one write', () => {
  it('a move: the old state (by id) and the old rank; the names only for the words', () => {
    const c = changeOf(issue(), issue({ version: 2, state: TODO, sortOrder: 1536 }), {
      state: 'Todo',
      placeAfter: 'i2',
    });
    expect(c).toMatchObject({
      undo: { state: 's1', sortOrder: 1024, words: { state: 'Backlog' } },
      redo: { state: 's2', sortOrder: 1536, words: { state: 'Todo' } },
      version: 2,
    });
    expect(bodyOf(c!.undo)).toEqual({ state: 's1', sortOrder: 1024 });
  });

  it('a move whose rank stays the same number still records the rank (LAUN-236 review: rank 0)', () => {
    // A card with rank 0 dropped in an empty column keeps rank 0. Without the rank in the undo, the server would
    // put it on top of its old column.
    const c = changeOf(issue({ sortOrder: 0 }), issue({ version: 2, state: TODO, sortOrder: 0 }), {
      state: 'Todo',
    });
    expect(bodyOf(c!.undo)).toEqual({ state: 's1', sortOrder: 0 });
    expect(bodyOf(c!.redo)).toEqual({ state: 's2', sortOrder: 0 });
  });

  it('a state change from a picker records the rank too, equal or not', () => {
    const c = changeOf(issue({ sortOrder: 7 }), issue({ version: 2, state: TODO, sortOrder: 7 }), {
      state: 'Todo',
    });
    expect(c?.undo.sortOrder).toBe(7);
    expect(c?.redo.sortOrder).toBe(7);
  });

  it('a reorder in the same column: the rank only', () => {
    const c = changeOf(issue(), issue({ version: 2, sortOrder: 50 }), { placeBefore: 'i2' });
    expect(bodyOf(c!.undo)).toEqual({ sortOrder: 1024 });
  });

  it('a placement that changed nothing (same column, same rank) is not an action', () => {
    expect(changeOf(issue(), issue({ version: 2 }), { placeAfter: 'i2' })).toBeNull();
  });

  it('priority, assignee and labels (labels by id, names for the words)', () => {
    const c = changeOf(
      issue({ assignee: { username: 'chen' }, labels: [BUG] }),
      issue({ version: 2, priority: 2, assignee: null, labels: [UI] }),
      { priority: 2, assignee: null, addLabels: ['ui'], removeLabels: ['bug'] },
    );
    expect(bodyOf(c!.undo)).toEqual({
      priority: 0,
      assignee: 'chen',
      removeLabels: ['l-ui'],
      addLabels: ['l-bug'],
    });
    expect(bodyOf(c!.redo)).toEqual({
      priority: 2,
      assignee: null,
      addLabels: ['l-ui'],
      removeLabels: ['l-bug'],
    });
    expect(c!.undo.words).toEqual({ remove: ['ui'], add: ['bug'] });
  });

  it('a field the patch did not name is left alone, and a write that changed nothing is null', () => {
    expect(changeOf(issue(), issue({ version: 2, priority: 3 }), { state: 'Backlog' })).toBeNull();
    expect(changeOf(issue({ labels: [BUG] }), issue({ labels: [BUG] }), { addLabels: ['bug'] })).toBeNull();
  });

  it('a create: undo is chosen later, redo is the state (by id) and the rank it was made with', () => {
    expect(createdChange(issue({ state: TODO, sortOrder: -1024 }))).toMatchObject({
      created: true,
      undo: {},
      redo: { state: 's2', sortOrder: -1024, words: { state: 'Todo' } },
      version: 1,
    });
  });
});

describe('the words', () => {
  it('a server text in the retry toast ends with a period before the next sentence', () => {
    expect(asSentence('down')).toBe('down.');
    expect(asSentence('The server is down.')).toBe('The server is down.');
    expect(asSentence('Really? ')).toBe('Really?');
  });

  const names = {
    priority: (p: number) => (p === 1 ? 'Urgent' : 'No priority'),
    member: (u: string) => u.toUpperCase(),
  };
  it('one change', () => {
    expect(say({ state: 'Todo', sortOrder: 1 }, 'ORB-12', names)).toBe('moved ORB-12 to Todo');
    expect(say({ assignee: null }, 'ORB-1', names)).toBe('unassigned ORB-1');
    expect(say({ assignee: 'chen' }, 'ORB-1', names)).toBe('assigned ORB-1 to CHEN');
    expect(say({ priority: 1 }, 'ORB-1', names)).toBe('set ORB-1 priority to Urgent');
    expect(say({ addLabels: ['bug'] }, 'ORB-1', names)).toBe('added bug to ORB-1');
    expect(say({ sortOrder: 4 }, 'ORB-1', names)).toBe('moved ORB-1 in its column');
    expect(say({ sortOrder: 4 }, 'ORB-1', { ...names, place: 'group' })).toBe('moved ORB-1 in its group');
  });
  it('several', () => {
    const f = { state: 'Todo' };
    expect(
      describeEntry(
        [
          { f, id: 'ORB-1' },
          { f, id: 'ORB-2' },
        ],
        names,
      ),
    ).toBe('moved 2 issues to Todo');
    expect(
      describeEntry(
        [
          { f, id: 'ORB-1' },
          { f: { priority: 1 }, id: 'ORB-2' },
        ],
        names,
      ),
    ).toBe('changed 2 issues');
  });
});

describe('the keys', () => {
  const key = (o: Partial<KeyboardEventInit>) => new KeyboardEvent('keydown', { key: 'z', ...o });
  it('Command on a Mac, Ctrl elsewhere', () => {
    expect(undoKey(key({ metaKey: true }), true)).toBe('undo');
    expect(undoKey(key({ ctrlKey: true }), true)).toBeNull();
    expect(undoKey(key({ ctrlKey: true }), false)).toBe('undo');
    expect(undoKey(key({ metaKey: true }), false)).toBeNull();
  });
  it('Shift redoes; Ctrl+Y redoes off a Mac only; Alt or a plain z is not ours', () => {
    expect(undoKey(key({ metaKey: true, shiftKey: true, key: 'Z' }), true)).toBe('redo');
    expect(undoKey(key({ ctrlKey: true, shiftKey: true, key: 'Z' }), false)).toBe('redo');
    expect(undoKey(key({ ctrlKey: true, key: 'y' }), false)).toBe('redo');
    expect(undoKey(key({ metaKey: true, key: 'y' }), true)).toBeNull();
    expect(undoKey(key({ ctrlKey: true, altKey: true }), false)).toBeNull();
    expect(undoKey(key({}), false)).toBeNull();
  });
  it('a non-Latin layout uses the physical key; a Latin one keeps its own letter (AZERTY, QWERTZ)', () => {
    expect(undoKey(key({ metaKey: true, key: 'я', code: 'KeyZ' }), true)).toBe('undo'); // Russian
    expect(undoKey(key({ ctrlKey: true, key: 'ζ', code: 'KeyZ' }), false)).toBe('undo'); // Greek
    expect(undoKey(key({ ctrlKey: true, key: 'н', code: 'KeyY' }), false)).toBe('redo');
    // AZERTY: the key labelled Z sends key "z" with code KeyW (undo); the one at KeyZ sends "w" (not ours).
    expect(undoKey(key({ metaKey: true, key: 'z', code: 'KeyW' }), true)).toBe('undo');
    expect(undoKey(key({ metaKey: true, key: 'w', code: 'KeyZ' }), true)).toBeNull();
    // QWERTZ: Z and Y are swapped; the letter wins.
    expect(undoKey(key({ ctrlKey: true, key: 'z', code: 'KeyY' }), false)).toBe('undo');
    expect(undoKey(key({ ctrlKey: true, key: 'y', code: 'KeyZ' }), false)).toBe('redo');
  });
});
