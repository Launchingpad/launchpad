import { describe, expect, it } from 'vitest';
import { activeFilterCount, emptyFilters, parseFilters, toApiQuery, writeFilters } from './filters';
import { groupByState, boardColumns } from '../../lib/group';
import { makeIssue, states } from '../../test/mockApi';

describe('filters <-> URL', () => {
  it('round-trips', () => {
    const f = {
      ...emptyFilters,
      state: ['Todo', 'In Progress'],
      assignee: 'me',
      label: ['bug'],
      priority: 2,
      q: 'crash',
      sort: 'created' as const,
      view: 'board' as const,
    };
    expect(parseFilters(writeFilters(f))).toEqual(f);
  });
  it('round-trips the product through the URL and sends it to the server (LAUN-64)', () => {
    const f = { ...emptyFilters, team: 'ORB', product: 'launchpad' };
    expect(writeFilters(f).get('product')).toBe('launchpad');
    expect(parseFilters(writeFilters(f))).toEqual(f);
    expect(parseFilters(new URLSearchParams('product=')).product).toBeUndefined();
    expect(toApiQuery(f)).toMatchObject({ team: 'ORB', product: 'launchpad' });
    expect(toApiQuery(emptyFilters)).not.toHaveProperty('product');
    expect(activeFilterCount(f)).toBe(1); // the product counts, the team (a route scope) does not
  });
  it('omits defaults from the URL', () => {
    expect(writeFilters(emptyFilters).toString()).toBe('');
  });
  it('ignores a bad priority and an unknown sort', () => {
    const f = parseFilters(new URLSearchParams('priority=9&sort=nope&view=cards'));
    expect(f.priority).toBeUndefined();
    expect(f.sort).toBe('updated');
    expect(f.view).toBe('list');
  });
  it('sends only parameters the server accepts', () => {
    const q = toApiQuery({ ...emptyFilters, state: ['Todo'], q: ' x ', sort: 'priority', view: 'list' });
    expect(Object.keys(q).sort()).toEqual(['orderBy', 'query', 'state']);
    expect(q.orderBy).toBe('updatedAt');
  });
  it('the board starts in Manual and then follows the sort like the list (LAUN-110)', () => {
    expect(parseFilters(new URLSearchParams('view=board')).sort).toBe('manual');
    expect(parseFilters(new URLSearchParams('view=board&sort=priority')).sort).toBe('priority');
    expect(parseFilters(new URLSearchParams('view=board&sort=updated')).sort).toBe('updated');
    expect(parseFilters(new URLSearchParams('')).sort).toBe('updated');
    // The URL round trip keeps what the person chose, and leaves out only the default of the view.
    expect(writeFilters({ ...emptyFilters, view: 'board', sort: 'manual' }).has('sort')).toBe(false);
    expect(writeFilters({ ...emptyFilters, view: 'board', sort: 'updated' }).get('sort')).toBe('updated');
    expect(writeFilters({ ...emptyFilters, sort: 'manual' }).get('sort')).toBe('manual');
    for (const sort of ['manual', 'updated', 'created', 'priority'] as const)
      for (const view of ['list', 'board'] as const) {
        const f = { ...emptyFilters, sort, view };
        expect(parseFilters(writeFilters(f))).toEqual(f);
      }
    expect(toApiQuery({ ...emptyFilters, sort: 'manual', view: 'board' }).orderBy).toBe('manual');
    expect(toApiQuery({ ...emptyFilters, sort: 'created', view: 'board' }).orderBy).toBe('createdAt');
    expect(toApiQuery({ ...emptyFilters, sort: 'created', view: 'list' }).orderBy).toBe('createdAt');
    expect(toApiQuery({ ...emptyFilters, sort: 'manual', view: 'list' }).orderBy).toBe('manual');
  });
});

describe('grouping', () => {
  const issues = [
    makeIssue(1, {
      state: { id: 's3', name: 'In Progress', type: 'started', color: '#f2c94c' },
      priority: 0,
    }),
    makeIssue(2, { priority: 3 }),
    makeIssue(3, { priority: 1 }),
  ];
  it('orders groups by workflow, not by arrival', () => {
    expect(groupByState(issues, states, 'updated').map((g) => g.name)).toEqual(['Backlog', 'In Progress']);
  });
  it('puts Triage first, in the list and on the board (LAUN-20)', () => {
    const triage = { id: 's0', name: 'Triage', type: 'triage' as const, color: '#bec2c8' };
    const withTriage = [{ ...triage, description: '', position: -1, teamId: 'team1' }, ...states];
    const mixed = [...issues, makeIssue(4, { state: triage })];
    expect(groupByState(mixed, withTriage, 'updated').map((g) => g.name)).toEqual([
      'Triage',
      'Backlog',
      'In Progress',
    ]);
    expect(boardColumns(issues, withTriage, 'updated').map((g) => g.name)).toEqual([
      'Triage',
      'Backlog',
      'Todo',
      'In Progress',
      'Done',
    ]);
  });
  it('sorts by priority inside a group, urgent first and none last', () => {
    const g = groupByState(
      [makeIssue(1, { priority: 0 }), makeIssue(2, { priority: 3 }), makeIssue(3, { priority: 1 })],
      states,
      'priority',
    )[0]!;
    expect(g.issues.map((i) => i.number)).toEqual([3, 2, 1]);
  });
  it('board shows empty columns for every state', () => {
    expect(boardColumns(issues, states, 'updated').map((g) => g.name)).toEqual([
      'Backlog',
      'Todo',
      'In Progress',
      'Done',
    ]);
  });
});
