import { describe, expect, it } from 'vitest';
import { boardGroups, groupKeyOf, NONE_KEY } from '../../lib/group';
import { makeIssue, states } from '../../test/mockApi';
import { applyColumns, toggleColumn } from './boardColumns';
import { emptyFilters, parseFilters, writeFilters } from './filters';
import { applyStored, readStored, reconcile, saveStored, storageKey } from './viewPrefs';

// LAUN-171: the group-by as data: the URL, the stored view, the columns and the hidden list per kind.

const src = {
  states,
  members: [
    { username: 'claude', name: 'Claude' },
    { username: 'chen', name: 'Chen' },
  ],
  labels: [{ name: 'bug', color: '#f00' }],
  projects: [{ id: 'p1', name: 'Web' }],
};
const K = storageKey('orbit', 'u1');

describe('the group-by in the address', () => {
  it('round-trips, and a state grouping writes nothing', () => {
    expect(writeFilters({ ...emptyFilters, view: 'board', group: 'label' }).get('group')).toBe('label');
    expect(writeFilters({ ...emptyFilters, view: 'board' }).has('group')).toBe(false);
    expect(parseFilters(new URLSearchParams('view=board&group=project')).group).toBe('project');
    expect(parseFilters(new URLSearchParams('group=nonsense')).group).toBe('state');
  });
});

describe('boardGroups', () => {
  it('assignee: members A to Z, Unassigned last, an unknown assignee still gets a column', () => {
    const i = makeIssue(1, { assignee: { id: 'x', username: 'gone', name: 'Gone' } });
    const g = boardGroups('assignee', [i], src, 'manual');
    expect(g.map((c) => c.name)).toEqual(['Chen', 'Claude', 'Gone', 'Unassigned']);
    expect(g.find((c) => c.key === NONE_KEY)!.value).toBeNull();
    expect(g[2]!.issues).toHaveLength(1);
  });
  it('priority: the fixed five, No priority last, with the value a drop sets', () => {
    const g = boardGroups('priority', [], src, 'manual');
    expect(g.map((c) => [c.name, c.value])).toEqual([
      ['Urgent', 1],
      ['High', 2],
      ['Medium', 3],
      ['Low', 4],
      ['No priority', 0],
    ]);
  });
  it('label: a card with two labels shows once, under its first; none goes to No label', () => {
    const two = makeIssue(1, {
      labels: [
        { id: 'l1', name: 'bug', color: '#f00' },
        { id: 'l2', name: 'ui', color: '#00f' },
      ],
    });
    const g = boardGroups('label', [two, makeIssue(2)], src, 'manual');
    expect(g.flatMap((c) => c.issues)).toHaveLength(2);
    expect(g.find((c) => c.name === 'bug')!.issues).toHaveLength(1);
    expect(g.find((c) => c.key === NONE_KEY)!.name).toBe('No label');
    expect(groupKeyOf('label', two)).toBe('bug');
  });
  it('project: an archived project is "Other project", not lost', () => {
    const g = boardGroups('project', [makeIssue(1, { projectId: 'old' })], src, 'manual');
    expect(g.map((c) => c.name)).toEqual(['Web', 'Other project', 'No project']);
  });
});

describe('hidden columns per group kind', () => {
  const assignee = boardGroups('assignee', [makeIssue(1)], src, 'manual');
  const prefs = { hideEmpty: false, hidden: ['Done'] };

  it('an assignee column is stored as assignee:<key> and leaves state names alone', () => {
    const r = toggleColumn(assignee, prefs, assignee[1]!, 'assignee');
    expect(r.prefs.hidden).toEqual(['Done', 'assignee:claude']);
    expect(applyColumns(assignee, r.prefs, 'assignee').shown.map((c) => c.name)).toEqual([
      'Chen',
      'Unassigned',
    ]);
    // The state kind ignores the assignee entry, and the assignee kind ignores the bare state name.
    const stateCols = boardGroups('state', [], src, 'manual');
    expect(applyColumns(stateCols, r.prefs, 'state').shown.map((c) => c.name)).not.toContain('Done');
    expect(applyColumns(stateCols, r.prefs, 'state').shown).toHaveLength(stateCols.length - 1);
  });
  it('showing it again removes only its own entry; pruning keeps the other kind', () => {
    const on = toggleColumn(assignee, prefs, assignee[1]!, 'assignee').prefs;
    expect(toggleColumn(assignee, on, assignee[1]!, 'assignee').prefs.hidden).toEqual(['Done']);
    const more = toggleColumn(
      assignee,
      { ...on, hidden: [...on.hidden, 'assignee:gone'] },
      assignee[0]!,
      'assignee',
    );
    expect(more.prefs.hidden).toEqual(['Done', 'assignee:claude', 'assignee:chen']);
  });
  it('the last column of a kind cannot be hidden', () => {
    const one = [assignee[0]!];
    expect(toggleColumn(one, { hideEmpty: false, hidden: [] }, one[0]!, 'assignee').reason).toBe('last');
  });
});

describe('the stored group-by', () => {
  const board = { view: 'board', sort: 'manual', at: 1 } as const;
  it('is stored with the place, and keeps the columns when only the layout changes', () => {
    saveStored(K, 'team:orb', 'board', 'manual', 10, {
      hideEmpty: true,
      hidden: ['assignee:chen'],
      groupBy: 'assignee',
    });
    expect(readStored(K, 'team:orb')).toMatchObject({ groupBy: 'assignee', hideEmpty: true });
    saveStored(K, 'team:orb', 'board', 'priority', 20); // no columns given: the place keeps its own
    expect(readStored(K, 'team:orb')).toMatchObject({
      groupBy: 'assignee',
      hidden: ['assignee:chen'],
      sort: 'priority',
    });
  });
  it('a place grouped by something else is not the default view, even in list layout', () => {
    saveStored(K, 'a', 'list', 'updated', 10, { hideEmpty: false, hidden: [], groupBy: 'priority' });
    expect(readStored(K, 'a')).toMatchObject({ groupBy: 'priority' });
  });
  it('an entry from an older build reads as state', () => {
    localStorage.setItem(
      K,
      JSON.stringify({ a: { view: 'board', sort: 'manual', hideEmpty: false, hidden: [], at: 1 } }),
    );
    expect(readStored(K, 'a')!.groupBy).toBe('state');
  });
  it('the address wins over the stored group-by; without one the stored value applies', () => {
    const sp = (qs: string) => new URLSearchParams(qs);
    const stored = { ...board, groupBy: 'assignee' } as const;
    expect(applyStored(parseFilters(sp('')), sp(''), stored).group).toBe('assignee');
    expect(applyStored(parseFilters(sp('group=label')), sp('group=label'), stored).group).toBe('label');
    expect(
      applyStored(parseFilters(sp('view=board&sort=manual')), sp('view=board&sort=manual'), stored).group,
    ).toBe('assignee');
  });
  it('the server value arrives through reconcile, and a newer local one is sent back with it', () => {
    localStorage.clear();
    const r = reconcile(K, [
      {
        place: 'a',
        view: 'board',
        sort: 'manual',
        hideEmpty: false,
        hiddenColumns: [],
        groupBy: 'project',
        at: 5,
      },
    ]);
    expect(r.changed).toBe(true);
    expect(readStored(K, 'a')!.groupBy).toBe('project');
    saveStored(K, 'b', 'board', 'manual', 50, { hideEmpty: false, hidden: [], groupBy: 'label' });
    expect(reconcile(K, []).push).toContainEqual(expect.objectContaining({ place: 'b', groupBy: 'label' }));
  });
});
