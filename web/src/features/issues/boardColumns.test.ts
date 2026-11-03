import { describe, expect, it } from 'vitest';
import type { Group } from '../../lib/group';
import { makeIssue } from '../../test/mockApi';
import { applyColumns, toggleColumn } from './boardColumns';
import { cleanHidden, defaultColumns } from './viewPrefs';

const g = (name: string, n: number): Group => ({
  key: name,
  name,
  type: 'unstarted',
  color: '#000',
  issues: Array.from({ length: n }, (_, i) => makeIssue(i + 1)),
});
const all = [g('Backlog', 0), g('Todo', 2), g('Done', 3)];

describe('applyColumns (LAUN-163)', () => {
  it('shows every column by default, empty ones too', () => {
    expect(applyColumns(all, defaultColumns).shown.map((c) => c.name)).toEqual(['Backlog', 'Todo', 'Done']);
  });
  it('hideEmpty drops columns with no card', () => {
    const r = applyColumns(all, { hideEmpty: true, hidden: [] });
    expect(r.shown.map((c) => c.name)).toEqual(['Todo', 'Done']);
    expect(r.hiddenColumns).toBe(1);
  });
  it('a hidden state leaves the board but its cards are counted', () => {
    const r = applyColumns(all, { hideEmpty: false, hidden: ['Done'] });
    expect(r.shown.map((c) => c.name)).toEqual(['Backlog', 'Todo']);
    expect(r.hiddenColumns).toBe(1);
    expect(r.hiddenIssues).toBe(3);
  });
  it('a hidden name that is not a column is ignored', () => {
    expect(applyColumns(all, { hideEmpty: false, hidden: ['Gone'] }).shown).toHaveLength(3);
  });
});

describe('toggleColumn (LAUN-163)', () => {
  it('hides and shows a state', () => {
    const a = toggleColumn(all, defaultColumns, 'Done');
    expect(a).toEqual({ prefs: { hideEmpty: false, hidden: ['Done'] }, refused: false });
    expect(toggleColumn(all, a.prefs, 'Done').prefs.hidden).toEqual([]);
  });
  it('refuses to hide the last column that is not hidden', () => {
    const r = toggleColumn(all, { hideEmpty: false, hidden: ['Backlog', 'Todo'] }, 'Done');
    expect(r.refused).toBe(true);
    expect(r.prefs.hidden).toEqual(['Backlog', 'Todo']);
  });
  it('drops names of states that no longer exist', () => {
    expect(toggleColumn(all, { hideEmpty: false, hidden: ['Gone'] }, 'Done').prefs.hidden).toEqual(['Done']);
  });
});

describe('cleanHidden', () => {
  it('keeps valid unique names only, at most 30', () => {
    expect(cleanHidden(['A', 'A', '', 'x,y', 'ok\n', 5, 'B'])).toEqual(['A', 'B']);
    expect(cleanHidden(Array.from({ length: 40 }, (_, i) => `s${i}`))).toHaveLength(30);
    expect(cleanHidden('nope')).toEqual([]);
    expect(cleanHidden(['x'.repeat(101)])).toEqual([]);
    expect(cleanHidden(['a\u0085b', 'a\u009fb', 'a\u007fb', 'ok\u00a0'])).toEqual(['ok\u00a0']); // C1 controls too, as the server
  });
});

describe('toggleColumn at the cap (LAUN-163)', () => {
  it('names its own reason', () => {
    const many = Array.from({ length: 32 }, (_, i) => g(`S${i}`, 1));
    const hidden = many.slice(0, 30).map((x) => x.name);
    const r = toggleColumn(many, { hideEmpty: false, hidden }, 'S30');
    expect(r).toMatchObject({ refused: true, reason: 'cap' });
    expect(toggleColumn(all, { hideEmpty: false, hidden: ['Backlog', 'Todo'] }, 'Done').reason).toBe('last');
  });
});
