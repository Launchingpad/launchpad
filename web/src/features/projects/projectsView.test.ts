import { describe, expect, it } from 'vitest';
import type { Project } from '../../api/types';
import { projectOf } from '../../test/mockApi';
import {
  addDays,
  defaultView,
  filterProjects,
  healthOf,
  parseView,
  sortBy,
  VIEW_KEY,
  writeView,
  emptyProjectFilters,
} from './projectsView';

const P = (name: string, over: Partial<Project> = {}) =>
  projectOf({
    id: name,
    name,
    state: 'started',
    lead: null,
    targetDate: null,
    teams: [],
    ...over,
  }) as unknown as Project;
const names = (l: Project[]) => l.map((p) => p.name);
const TODAY = '2026-10-29';

describe('healthOf', () => {
  it('derives from state and dates', () => {
    expect(healthOf({ state: 'started', targetDate: '2026-10-01' }, TODAY)).toBe('off');
    expect(healthOf({ state: 'completed', targetDate: '2026-10-01' }, TODAY)).toBe('none');
    expect(healthOf({ state: 'paused', targetDate: null }, TODAY)).toBe('risk');
    expect(healthOf({ state: 'planned', targetDate: '2026-11-05' }, TODAY)).toBe('risk');
    expect(healthOf({ state: 'planned', targetDate: '2026-11-13' }, TODAY)).toBe('on');
    expect(healthOf({ state: 'started', targetDate: '2026-11-05' }, TODAY)).toBe('on');
    expect(healthOf({ state: 'started', targetDate: null }, TODAY)).toBe('none');
  });
  it('puts the At-risk line at exactly 14 days, inclusive', () => {
    expect(healthOf({ state: 'planned', targetDate: '2026-11-12' }, TODAY)).toBe('risk');
    expect(healthOf({ state: 'planned', targetDate: '2026-11-13' }, TODAY)).toBe('on');
  });
  it('gives a canceled project No data', () => {
    expect(healthOf({ state: 'canceled', targetDate: '2020-01-01' }, TODAY)).toBe('none');
  });
  it('adds days across a month end', () => {
    expect(addDays('2026-10-29', 14)).toBe('2026-11-12');
  });
});

describe('sortBy', () => {
  const list = [
    P('b', { lead: { id: '1', username: 'z', name: 'Zed' }, targetDate: '2026-12-01' }),
    P('c', { targetDate: '2026-11-01', lead: { id: '2', username: 'a', name: 'Amy' } }),
    P('a'),
  ];
  it('sorts by name both ways', () => {
    expect(names(sortBy(list, 'name', 'asc', TODAY))).toEqual(['a', 'b', 'c']);
    expect(names(sortBy(list, 'name', 'desc', TODAY))).toEqual(['c', 'b', 'a']);
  });
  it('puts a missing target date and a missing lead last in both directions', () => {
    expect(names(sortBy(list, 'target', 'asc', TODAY))).toEqual(['c', 'b', 'a']);
    expect(names(sortBy(list, 'target', 'desc', TODAY))).toEqual(['b', 'c', 'a']);
    expect(names(sortBy(list, 'lead', 'asc', TODAY))).toEqual(['c', 'b', 'a']);
    expect(names(sortBy(list, 'lead', 'desc', TODAY))).toEqual(['b', 'c', 'a']);
  });
  it('sorts by status in live-first order and by health worst first', () => {
    const l = [
      P('x', { state: 'completed' }),
      P('y', { state: 'started', targetDate: '2026-01-01' }),
      P('z'),
    ];
    expect(names(sortBy(l, 'status', 'asc', TODAY))).toEqual(['y', 'z', 'x']);
    expect(names(sortBy(l, 'health', 'asc', TODAY))[0]).toBe('y');
  });
  it('does not change its input', () => {
    const copy = [...list];
    sortBy(list, 'name', 'desc', TODAY);
    expect(list).toEqual(copy);
  });
});

describe('filterProjects', () => {
  const l = [
    P('Alpha', {
      teams: [{ id: '1', key: 'WEB', name: 'Web' }],
      lead: { id: 'u', username: 'chen', name: 'Chen' },
    }),
    P('Beta', { state: 'planned', teams: [{ id: '2', key: 'IOS', name: 'iOS' }] }),
  ];
  const prods = new Map([['apps', ['IOS']]]);
  const f = (o = {}) => names(filterProjects(l, { ...emptyProjectFilters, ...o }, prods));
  it('filters by each field', () => {
    expect(f()).toEqual(['Alpha', 'Beta']);
    expect(f({ q: 'alp' })).toEqual(['Alpha']);
    expect(f({ states: ['ps-planned'] })).toEqual(['Beta']);
    expect(f({ lead: 'chen' })).toEqual(['Alpha']);
    expect(f({ lead: 'none' })).toEqual(['Beta']);
    expect(f({ teams: ['web'] })).toEqual(['Alpha']);
    expect(f({ product: 'apps' })).toEqual(['Beta']);
    expect(f({ product: 'gone' })).toEqual([]);
  });
  it('combines filters with AND', () => {
    expect(f({ states: ['ps-planned'], lead: 'chen' })).toEqual([]);
  });
});

describe('the remembered view', () => {
  it('falls back to the default for junk', () => {
    expect(parseView(null)).toEqual(defaultView());
    expect(parseView('{not json')).toEqual(defaultView());
    expect(parseView('5')).toEqual(defaultView());
    const v = parseView(
      JSON.stringify({
        sort: 'bogus',
        dir: 'x',
        columns: ['nope', 'lead'],
        filters: { states: ['nope', 'ps-paused', 7] },
      }),
    );
    expect(v.sort).toBe('status');
    expect(v.dir).toBe('asc');
    expect(v.columns).toEqual(['lead']);
    // Stored ids are kept as text; the page drops the ones that name no status once the list is loaded.
    expect(v.filters.states).toEqual(['nope', 'ps-paused']);
  });
  it('keeps the default columns when the stored JSON has none', () => {
    const v = parseView(JSON.stringify({ sort: 'name', dir: 'desc' }));
    expect(v.columns).toEqual(defaultView().columns);
    expect(v.sort).toBe('name');
  });
  it('round-trips but does not keep the search text', () => {
    localStorage.clear();
    writeView({
      ...defaultView(),
      sort: 'target',
      dir: 'desc',
      filters: { ...emptyProjectFilters, q: 'secret', lead: 'me' },
    });
    const v = parseView(localStorage.getItem(VIEW_KEY));
    expect(v.sort).toBe('target');
    expect(v.dir).toBe('desc');
    expect(v.filters.lead).toBe('me');
    expect(v.filters.q).toBe('');
  });
});
