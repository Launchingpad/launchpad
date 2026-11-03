import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { mockApi } from '../../test/mockApi';
import { parseFilters } from './filters';
import {
  MAX_ENTRIES,
  applyStored,
  clearStored,
  placeKey,
  RESET_PREFIX,
  readStored,
  reconcile,
  sendViews,
  saveStored,
  storageKey,
} from './viewPrefs';

const K = storageKey('orbit', 'u1');
const apply = (qs: string, stored?: Parameters<typeof applyStored>[2]) => {
  const sp = new URLSearchParams(qs);
  return applyStored(parseFilters(sp), sp, stored);
};
const board = { view: 'board', sort: 'manual', at: 1 } as const;

beforeEach(() => localStorage.clear());
afterEach(() => vi.restoreAllMocks());

describe('stored view (LAUN-109)', () => {
  it('saves and reads per place; places do not share', () => {
    saveStored(K, 'team:orb', 'board', 'manual');
    saveStored(K, 'team:laun', 'list', 'priority');
    expect(readStored(K, 'team:orb')).toMatchObject({ view: 'board', sort: 'manual' });
    expect(readStored(K, 'team:laun')).toMatchObject({ view: 'list', sort: 'priority' });
    expect(readStored(K, 'cycle:orb:3')).toBeUndefined();
    expect(readStored(storageKey('other', 'u1'), 'team:orb')).toBeUndefined();
    expect(readStored(storageKey('orbit', 'u2'), 'team:orb')).toBeUndefined();
  });

  it('an entry equal to the defaults is deleted, not stored (LAUN-142)', () => {
    saveStored(K, 'team:orb', 'board', 'manual', 1);
    saveStored(K, 'team:orb', 'list', 'updated', 2);
    expect(readStored(K, 'team:orb')).toBeUndefined();
    expect(localStorage.getItem(K)).toBeNull();
    saveStored(K, 'team:orb', 'list', 'manual', 3);
    expect(readStored(K, 'team:orb')).toMatchObject({ view: 'list', sort: 'manual' });
  });

  it('clearStored forgets one place only', () => {
    saveStored(K, 'a', 'board', 'manual');
    saveStored(K, 'b', 'board', 'manual');
    clearStored(K, 'a');
    expect(readStored(K, 'a')).toBeUndefined();
    expect(readStored(K, 'b')).toBeDefined();
    clearStored(K, 'b');
    expect(localStorage.getItem(K)).toBeNull();
  });

  it('keeps at most MAX_ENTRIES, dropping the oldest', () => {
    for (let i = 0; i < MAX_ENTRIES + 5; i++) saveStored(K, `p${i}`, 'board', 'manual', 1000 + i);
    expect(Object.keys(JSON.parse(localStorage.getItem(K)!))).toHaveLength(MAX_ENTRIES);
    expect(readStored(K, 'p0')).toBeUndefined();
    expect(readStored(K, `p${MAX_ENTRIES + 4}`)).toBeDefined();
  });

  it('drops unknown or hostile values and survives bad JSON', () => {
    localStorage.setItem(
      K,
      JSON.stringify({
        ok: { view: 'board', sort: 'created', at: 5 },
        badView: { view: 'kanban', sort: 'created', at: 5 },
        badSort: { view: 'list', sort: '<script>', at: 5 },
        wrongType: 'board',
        nul: null,
      }),
    );
    expect(readStored(K, 'ok')).toBeDefined();
    for (const p of ['badView', 'badSort', 'wrongType', 'nul']) expect(readStored(K, p)).toBeUndefined();
    localStorage.setItem(K, '{not json');
    expect(readStored(K, 'ok')).toBeUndefined();
    localStorage.setItem(K, '[1,2]');
    expect(readStored(K, 'ok')).toBeUndefined();
  });

  it('does not throw when storage throws on read or write', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('denied');
    });
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('quota');
    });
    vi.spyOn(Storage.prototype, 'removeItem').mockImplementation(() => {
      throw new Error('denied');
    });
    expect(() => saveStored(K, 'a', 'board', 'manual')).not.toThrow();
    expect(() => clearStored(K, 'a')).not.toThrow();
    expect(readStored(K, 'a')).toBeUndefined();
  });

  it('place keys are bounded and safe', () => {
    expect(placeKey('team', 'ORB')).toBe('team:orb');
    expect(placeKey('cycle', 'orb', 12)).toBe('cycle:orb:12');
    expect(placeKey('x', 'a b/../é')).toBe('x:a_b_..__');
    expect(placeKey('x', 'a'.repeat(500)).length).toBeLessThanOrEqual(2 + 64);
  });
});

describe('applyStored: the URL wins', () => {
  it('no stored choice: the URL parse as it is', () => {
    expect(apply('')).toMatchObject({ view: 'list', sort: 'updated' });
  });
  it('an empty URL takes the stored layout and sort', () => {
    expect(apply('state=Todo', board)).toMatchObject({ view: 'board', sort: 'manual', state: ['Todo'] });
  });
  it('an explicit view and sort are never replaced', () => {
    expect(apply('view=board&sort=created', { view: 'list', sort: 'priority', at: 1 })).toMatchObject({
      view: 'board',
      sort: 'created',
    });
  });
  it('an explicit view keeps the stored sort only if the stored layout is the same', () => {
    expect(apply('view=board', board)).toMatchObject({ view: 'board', sort: 'manual' });
    expect(apply('view=board', { view: 'board', sort: 'priority', at: 1 })).toMatchObject({
      sort: 'priority',
    });
    // the link says list, the stored choice was made on the board: the list's own default
    expect(apply('view=list', board)).toMatchObject({ view: 'list', sort: 'updated' });
  });
  it('an explicit sort keeps the stored layout', () => {
    expect(apply('sort=priority', board)).toMatchObject({ view: 'board', sort: 'priority' });
  });
  it('a junk view or sort in the URL still counts as explicit and falls back to the defaults', () => {
    expect(apply('view=zzz&sort=zzz', board)).toMatchObject({ view: 'list', sort: 'updated' });
  });
  it('filters are not remembered', () => {
    expect(apply('', board).state).toEqual([]);
  });
});

describe('server merge (LAUN-141)', () => {
  const srv = (
    place: string,
    view: 'list' | 'board',
    sort: 'updated' | 'manual' | 'priority',
    at: number,
  ) => ({
    place,
    view,
    sort,
    at,
  });
  const resets = () =>
    JSON.parse(localStorage.getItem(RESET_PREFIX + 'orbit:u1') ?? '{}') as Record<string, number>;

  it('a newer server value replaces the local one and a newer local value is sent back', () => {
    saveStored(K, 'a', 'board', 'manual', 100);
    saveStored(K, 'b', 'board', 'manual', 300);
    const r = reconcile(K, [srv('a', 'list', 'priority', 200), srv('b', 'list', 'priority', 200)]);
    expect(r.changed).toBe(true);
    expect(readStored(K, 'a')).toEqual({
      view: 'list',
      sort: 'priority',
      hideEmpty: false,
      hidden: [],
      groupBy: 'state',
      at: 200,
    });
    expect(readStored(K, 'b')).toMatchObject({ view: 'board', at: 300 });
    expect(r.push).toEqual([
      {
        place: 'b',
        view: 'board',
        sort: 'manual',
        hideEmpty: false,
        hiddenColumns: [],
        groupBy: 'state',
        at: 300,
      },
    ]);
  });

  it('a tie changes nothing and sends nothing', () => {
    saveStored(K, 'a', 'board', 'manual', 100);
    const r = reconcile(K, [srv('a', 'board', 'manual', 100)]);
    expect(r).toEqual({ changed: false, push: [] });
  });

  it('a place only the server has is stored; a place only this device has is sent', () => {
    saveStored(K, 'local', 'board', 'manual', 50);
    const r = reconcile(K, [srv('remote', 'board', 'manual', 10)]);
    expect(readStored(K, 'remote')).toMatchObject({ view: 'board', at: 10 });
    expect(r.push).toEqual([
      {
        place: 'local',
        view: 'board',
        sort: 'manual',
        hideEmpty: false,
        hiddenColumns: [],
        groupBy: 'state',
        at: 50,
      },
    ]);
  });

  it('a reset here wins over an older server choice, and a newer server reset clears the local choice', () => {
    saveStored(K, 'a', 'board', 'manual', 100);
    clearStored(K, 'a', 200); // reset on this device
    expect(readStored(K, 'a')).toBeUndefined();
    const r = reconcile(K, [srv('a', 'board', 'manual', 100)]);
    expect(readStored(K, 'a')).toBeUndefined();
    expect(r.push).toEqual([{ place: 'a', view: 'list', sort: 'updated', at: 200 }]);

    saveStored(K, 'b', 'board', 'manual', 100);
    const r2 = reconcile(K, [srv('b', 'list', 'updated', 300)]);
    expect(r2.changed).toBe(true);
    expect(readStored(K, 'b')).toBeUndefined();
    expect(resets()['b']).toBe(300);
  });

  it('a save of the defaults records the reset time, so an older server choice does not come back', () => {
    saveStored(K, 'a', 'board', 'manual', 100);
    saveStored(K, 'a', 'list', 'updated', 200);
    const r = reconcile(K, [srv('a', 'board', 'manual', 100)]);
    expect(readStored(K, 'a')).toBeUndefined();
    expect(r.push).toEqual([{ place: 'a', view: 'list', sort: 'updated', at: 200 }]);
    expect(resets()['a']).toBe(200);
  });

  it('a tie with different values changes nothing here and sends nothing', () => {
    saveStored(K, 'a', 'board', 'manual', 100);
    const r = reconcile(K, [srv('a', 'list', 'priority', 100)]);
    expect(r).toEqual({ changed: false, push: [] });
    expect(readStored(K, 'a')).toEqual({
      view: 'board',
      sort: 'manual',
      hideEmpty: false,
      hidden: [],
      groupBy: 'state',
      at: 100,
    });
  });

  it('a reset the server has no row for is sent', () => {
    saveStored(K, 'a', 'board', 'manual', 100);
    saveStored(K, 'a', 'list', 'updated', 200); // back to the defaults: a reset time, no entry
    const r = reconcile(K, []);
    expect(r.push).toEqual([{ place: 'a', view: 'list', sort: 'updated', at: 200 }]);
  });

  it('drops a server value outside the allowed sets', () => {
    reconcile(K, [{ place: 'x', view: 'grid', sort: 'updated', at: 5 } as never]);
    expect(readStored(K, 'x')).toBeUndefined();
  });

  it('a new choice removes the reset time of its place', () => {
    saveStored(K, 'a', 'board', 'manual', 100);
    clearStored(K, 'a', 200);
    saveStored(K, 'a', 'board', 'priority', 300);
    expect(resets()['a']).toBeUndefined();
  });
});

describe('sending the push-back (LAUN-141)', () => {
  it('attempts every entry, in groups, and a failing one does not stop the rest or throw', async () => {
    document.cookie = 'odt_csrf=csrf-test; path=/';
    const urls: string[] = [];
    mockApi({
      'PUT /api/v1/orgs/:org/me/views/:place': (req) => {
        urls.push(req.url.pathname);
        return req.url.pathname.endsWith('p7')
          ? { status: 503, json: { error: 'x', code: 'internal' } }
          : { json: {} };
      },
    });
    const views = Array.from({ length: 12 }, (_, i) => ({
      place: `p${i}`,
      view: 'board' as const,
      sort: 'manual' as const,
      at: i + 1,
    }));
    await expect(sendViews('orbit', views)).resolves.toBeUndefined();
    expect(urls.sort()).toEqual(views.map((v) => `/api/v1/orgs/orbit/me/views/${v.place}`).sort());
  });
});

describe('saveStored keeps the columns (LAUN-163)', () => {
  it('a change of layout or sort alone keeps hideEmpty and hidden', () => {
    localStorage.clear();
    saveStored(K, 'p', 'board', 'manual', 1, { hideEmpty: true, hidden: ['Done'] });
    saveStored(K, 'p', 'board', 'priority', 2);
    expect(readStored(K, 'p')).toMatchObject({ sort: 'priority', hideEmpty: true, hidden: ['Done'] });
  });
});

describe('reconcile and the columns (LAUN-163)', () => {
  it('equal layout and sort but different columns counts as a change', () => {
    localStorage.clear();
    saveStored(K, 'p', 'board', 'manual', 1);
    const r = reconcile(K, [
      { place: 'p', view: 'board', sort: 'manual', hideEmpty: true, hiddenColumns: ['Done'], at: 5 },
    ]);
    expect(r.changed).toBe(true);
    expect(readStored(K, 'p')).toMatchObject({ hideEmpty: true, hidden: ['Done'], at: 5 });
  });
});
