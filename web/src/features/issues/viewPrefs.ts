import { useCallback, useEffect, useMemo, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useOrgSlug } from '../../api/org';
import { client, unwrap } from '../../api/client';
import { useAllStates } from '../../api/queries';
import { keys } from '../../api/keys';
import type { Session } from '../../api/types';
import {
  defaultSort,
  GROUP_KEYS,
  type GroupKey,
  type IssueFilters,
  type SortKey,
  type ViewKey,
} from './filters';

/**
 * LAUN-109: the layout (list or board) and the sort of an issue page are remembered per person, per org and per
 * place. localStorage is the fast path (docs/internal/web-decisions.md, LAUN-109); the server copy (LAUN-141,
 * decisions-T23-view-preferences.md) makes the choice follow the person to another device. Filters are not kept.
 * Every read and write can throw (a private window, blocked site data, no network): the page then works as before.
 */

export const STORAGE_PREFIX = 'launchpad.view.v1:';
/** Entries kept per person and org. The oldest by last change goes first. */
export const MAX_ENTRIES = 100;

const VIEWS: readonly ViewKey[] = ['list', 'board'];
const SORTS: readonly SortKey[] = ['updated', 'created', 'priority', 'manual'];

/**
 * LAUN-163: the board's column choices. `hideEmpty` hides columns with no cards; `hidden` holds state names the
 * person hid. The defaults (show everything) are what a place has with no choice made.
 */
export interface ColumnPrefs {
  hideEmpty: boolean;
  /**
   * Hidden columns. A state column is its bare name (LAUN-163); a column of another group-by is
   * `<groupBy>:<column key>` (LAUN-171), so each group kind keeps its own list in the one field.
   */
  hidden: string[];
  /**
   * LAUN-171: the board's group-by of the place. It rides with the column choices because both are board prefs
   * that a change of layout or sort must keep. Absent means `state`. The Columns control never changes it.
   */
  groupBy?: GroupKey;
}
export const defaultColumns: ColumnPrefs = { hideEmpty: false, hidden: [] };
export const MAX_HIDDEN = 30;
export const MAX_COLUMN_NAME = 100;
/** A hidden entry of a column that is not a state column (LAUN-171): `<groupBy>:<key>`. */
export const isKindEntry = (n: string): boolean =>
  GROUP_KEYS.some((g) => g !== 'state' && n.startsWith(`${g}:`));
export const isDefaultColumns = (c: ColumnPrefs): boolean =>
  !c.hideEmpty && c.hidden.length === 0 && (c.groupBy ?? 'state') === 'state';

/** Keep valid, unique names only (the server refuses the rest); a hostile or stale value is dropped. */
const badName = (n: string): boolean =>
  n === '' ||
  [...n].length > MAX_COLUMN_NAME ||
  [...n].some((c) => c === ',' || c < ' ' || (c >= '\u007f' && c <= '\u009f'));

export function cleanHidden(v: unknown): string[] {
  if (!Array.isArray(v)) return [];
  const out: string[] = [];
  for (const n of v) {
    if (typeof n !== 'string' || badName(n)) continue;
    if (!out.includes(n)) out.push(n);
    if (out.length >= MAX_HIDDEN) break;
  }
  return out;
}

export interface StoredView {
  view: ViewKey;
  sort: SortKey;
  /** LAUN-163. An entry written by an older build has neither: it reads as the defaults. */
  hideEmpty: boolean;
  hidden: string[];
  /** LAUN-171. An entry written by an older build reads as `state`. */
  groupBy: GroupKey;
  /** Epoch milliseconds of the last change: the order for the cap. */
  at: number;
}
type Entries = Record<string, StoredView>;

export const storageKey = (org: string, user: string) => `${STORAGE_PREFIX}${org}:${user}`;

/** A place key is built from route parts: bound its length and strip anything odd. */
export const placeKey = (...parts: (string | number)[]): string =>
  parts
    .map((p) =>
      String(p)
        .toLowerCase()
        .replace(/[^a-z0-9._-]/g, '_')
        .slice(0, 64),
    )
    .join(':');

const isRecord = (v: unknown): v is Record<string, unknown> =>
  typeof v === 'object' && v !== null && !Array.isArray(v);

/** Keep only values from the allowed sets: a stale or hostile value is dropped. */
function clean(v: unknown): StoredView | null {
  if (!isRecord(v)) return null;
  if (!VIEWS.includes(v.view as ViewKey) || !SORTS.includes(v.sort as SortKey)) return null;
  const at = typeof v.at === 'number' && Number.isFinite(v.at) ? v.at : 0;
  return {
    view: v.view as ViewKey,
    sort: v.sort as SortKey,
    hideEmpty: v.hideEmpty === true,
    hidden: cleanHidden(v.hidden ?? v.hiddenColumns),
    groupBy: GROUP_KEYS.find((g) => g === v.groupBy) ?? 'state',
    at,
  };
}
const sameColumns = (a: ColumnPrefs, b: ColumnPrefs) =>
  a.hideEmpty === b.hideEmpty &&
  (a.groupBy ?? 'state') === (b.groupBy ?? 'state') &&
  a.hidden.length === b.hidden.length &&
  a.hidden.every((n, i) => n === b.hidden[i]);

function readAll(key: string): Entries {
  try {
    const raw = localStorage.getItem(key);
    if (!raw) return {};
    const v: unknown = JSON.parse(raw);
    if (!isRecord(v)) return {};
    const out: Entries = {};
    for (const [place, e] of Object.entries(v)) {
      const c = clean(e);
      if (c) out[place] = c;
    }
    return out;
  } catch {
    return {};
  }
}

function writeAll(key: string, entries: Entries): void {
  try {
    if (Object.keys(entries).length === 0) localStorage.removeItem(key);
    else localStorage.setItem(key, JSON.stringify(entries));
  } catch {
    /* storage blocked or full: the choice lasts until the page closes (it is in the URL) */
  }
}

/**
 * LAUN-141: a place the person set back to the defaults has no entry (LAUN-142), but the server must hear about it
 * and must not bring an older choice back. This second blob keeps the time of such a reset: { place: epoch ms }.
 */
export const RESET_PREFIX = 'launchpad.viewsync.v1:';
const resetKey = (key: string) => RESET_PREFIX + key.slice(STORAGE_PREFIX.length);

function readResets(key: string): Record<string, number> {
  try {
    const raw = localStorage.getItem(resetKey(key));
    const v: unknown = raw ? JSON.parse(raw) : {};
    if (!isRecord(v)) return {};
    const out: Record<string, number> = {};
    for (const [place, at] of Object.entries(v))
      if (typeof at === 'number' && Number.isFinite(at)) out[place] = at;
    return out;
  } catch {
    return {};
  }
}

function writeResets(key: string, resets: Record<string, number>): void {
  try {
    const places = Object.keys(resets);
    if (places.length > MAX_ENTRIES) {
      places
        .sort((a, b) => resets[a]! - resets[b]!)
        .slice(0, places.length - MAX_ENTRIES)
        .forEach((p) => delete resets[p]);
    }
    if (places.length === 0) localStorage.removeItem(resetKey(key));
    else localStorage.setItem(resetKey(key), JSON.stringify(resets));
  } catch {
    /* blocked or full: the server copy still gets the reset when the page can send it */
  }
}

function setReset(key: string, place: string, at: number | null): void {
  const r = readResets(key);
  if (at === null) {
    if (!(place in r)) return;
    delete r[place];
  } else r[place] = at;
  writeResets(key, r);
}

export const isDefaultView = (view: ViewKey, sort: SortKey, cols: ColumnPrefs = defaultColumns): boolean =>
  view === 'list' && sort === defaultSort('list') && isDefaultColumns(cols);

export function readStored(key: string, place: string): StoredView | undefined {
  return readAll(key)[place];
}

/** `cols` absent keeps the columns the place already has (a change of layout or sort must not lose them). */
export function saveStored(
  key: string,
  place: string,
  view: ViewKey,
  sort: SortKey,
  now = Date.now(),
  cols?: ColumnPrefs,
): void {
  const all = readAll(key);
  const c: ColumnPrefs = cols ?? {
    hideEmpty: all[place]?.hideEmpty ?? false,
    hidden: all[place]?.hidden ?? [],
    groupBy: all[place]?.groupBy ?? 'state',
  };
  // LAUN-142: the defaults are what a place shows with no entry, so an entry equal to them is deleted.
  if (isDefaultView(view, sort, c)) {
    delete all[place];
    writeAll(key, all);
    setReset(key, place, now);
    return;
  }
  setReset(key, place, null);
  all[place] = {
    view,
    sort,
    hideEmpty: c.hideEmpty,
    hidden: c.hidden,
    groupBy: c.groupBy ?? 'state',
    at: now,
  };
  const places = Object.keys(all);
  if (places.length > MAX_ENTRIES) {
    places
      .sort((a, b) => all[a]!.at - all[b]!.at)
      .slice(0, places.length - MAX_ENTRIES)
      .forEach((p) => delete all[p]);
  }
  writeAll(key, all);
}

export function clearStored(key: string, place: string, now = Date.now()): void {
  const all = readAll(key);
  if (!(place in all)) return;
  delete all[place];
  writeAll(key, all);
  setReset(key, place, now);
}

/** One place as the server holds it (`at` is epoch milliseconds). */
export interface ServerView {
  place: string;
  view: ViewKey;
  sort: SortKey;
  /** LAUN-163: absent from an older server, then the defaults. */
  hideEmpty?: boolean;
  hiddenColumns?: string[];
  /** LAUN-171: absent from an older server, then `state`. */
  groupBy?: GroupKey;
  at: number;
}

/**
 * LAUN-141: merge the server's list into the local store. The newer `at` wins per place; a tie changes nothing.
 * What the server lacks, or has older, is returned in `push` so the caller can send it. A server value equal to the
 * defaults is a reset: it removes the local entry and keeps the time as a reset. Pure apart from localStorage.
 */
export function reconcile(key: string, server: ServerView[]): { changed: boolean; push: ServerView[] } {
  const all = readAll(key);
  const resets = readResets(key);
  const seen = new Set<string>();
  const push: ServerView[] = [];
  let changed = false;
  for (const s of server) {
    const cv = clean(s);
    if (!cv) continue;
    seen.add(s.place);
    const local = all[s.place];
    const localAt = local ? local.at : (resets[s.place] ?? -1);
    if (s.at > localAt) {
      if (
        isDefaultView(cv.view, cv.sort, { hideEmpty: cv.hideEmpty, hidden: cv.hidden, groupBy: cv.groupBy })
      ) {
        if (local) changed = true;
        delete all[s.place];
        resets[s.place] = s.at;
      } else {
        if (!local || local.view !== cv.view || local.sort !== cv.sort || !sameColumns(local, cv))
          changed = true;
        all[s.place] = {
          view: cv.view,
          sort: cv.sort,
          hideEmpty: cv.hideEmpty,
          hidden: cv.hidden,
          groupBy: cv.groupBy,
          at: s.at,
        };
        delete resets[s.place];
      }
    } else if (s.at < localAt) {
      push.push(
        local
          ? toServer(s.place, local)
          : { place: s.place, view: 'list', sort: defaultSort('list'), at: localAt },
      );
    }
  }
  for (const [place, e] of Object.entries(all)) if (!seen.has(place)) push.push(toServer(place, e));
  for (const [place, at] of Object.entries(resets))
    if (!seen.has(place) && !(place in all))
      push.push({ place, view: 'list', sort: defaultSort('list'), at });
  writeAll(key, all);
  writeResets(key, resets);
  return { changed, push };
}

function toServer(place: string, v: StoredView): ServerView {
  return {
    place,
    view: v.view,
    sort: v.sort,
    hideEmpty: v.hideEmpty,
    hiddenColumns: v.hidden,
    groupBy: v.groupBy,
    at: v.at,
  };
}

/** The push-back of a first load goes out 5 at a time, not up to 100 at once. */
export async function sendViews(org: string, views: ServerView[]): Promise<void> {
  for (let i = 0; i < views.length; i += 5)
    await Promise.all(views.slice(i, i + 5).map((v) => sendView(org, v)));
}

/** Best effort: a failed send is repaired by the next reconcile (the local value is newer than the server's). */
async function sendView(org: string, v: ServerView): Promise<void> {
  try {
    await unwrap(
      client.PUT('/api/v1/orgs/{org}/me/views/{place}', {
        params: { path: { org, place: v.place } },
        body: {
          view: v.view,
          sort: v.sort,
          hideEmpty: v.hideEmpty ?? false,
          hiddenColumns: v.hiddenColumns ?? [],
          groupBy: v.groupBy ?? 'state',
          at: v.at,
        },
      }),
    );
  } catch {
    /* offline, or the server is older than this build */
  }
}

/**
 * The URL wins. `view` or `sort` in the address is explicit and is never replaced; what the address leaves out
 * comes from the stored choice, and then from the defaults. A stored sort goes with the layout it was chosen in:
 * if the address names the other layout, the sort is that layout's default.
 */
export function applyStored(
  parsed: IssueFilters,
  sp: URLSearchParams,
  stored?: Pick<StoredView, 'view' | 'sort'> & Partial<StoredView>,
): IssueFilters {
  if (!stored) return parsed;
  const urlView = sp.has('view');
  const urlSort = sp.has('sort');
  // LAUN-171: the group-by is the address's when it names one, else the stored one (it applies to the board only).
  const group: GroupKey = sp.has('group') ? parsed.group : (stored.groupBy ?? 'state');
  if (urlView && urlSort) return { ...parsed, group };
  const view: ViewKey = urlView ? parsed.view : stored.view;
  if (urlSort) return { ...parsed, view, group };
  const sort = view === stored.view ? stored.sort : defaultSort(view);
  return { ...parsed, view, sort, group };
}

/**
 * Wraps a page's filters: applies the stored view, and remembers a change of layout or sort.
 * `place` is stable per page ("team:orb", "cycle:orb:3", "product:web", "all", "mine").
 */
export function useRememberedView(
  place: string,
  sp: URLSearchParams,
  parsed: IssueFilters,
  write: (f: IssueFilters) => void,
) {
  const org = useOrgSlug();
  const qc = useQueryClient();
  const user = qc.getQueryData<Session | null>(keys.session)?.user.id ?? 'anon';
  const key = storageKey(org, user);
  const [tick, setTick] = useState(0);
  // A change in another tab shows here too.
  useEffect(() => {
    const on = (e: StorageEvent) => {
      if (e.key === key || e.key === null) setTick((t) => t + 1);
    };
    window.addEventListener('storage', on);
    return () => window.removeEventListener('storage', on);
  }, [key]);
  // LAUN-141: once per page load and org, ask the server what it holds and merge. A human session only.
  const signedIn = user !== 'anon';
  const remote = useQuery({
    queryKey: [...keys.org(org), 'view-preferences', user],
    queryFn: () => unwrap(client.GET('/api/v1/orgs/{org}/me/views', { params: { path: { org } } })),
    enabled: signedIn,
    staleTime: Infinity,
    retry: false,
    refetchOnWindowFocus: false,
  });
  const remoteItems = remote.data?.items;
  useEffect(() => {
    if (!remoteItems) return;
    const { changed, push } = reconcile(key, remoteItems as ServerView[]);
    void sendViews(org, push);
    if (changed) setTick((t) => t + 1);
  }, [remoteItems, key, org]);
  const spKey = sp.toString();
  const stored = useMemo(
    () => readStored(key, place),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [key, place, tick],
  );
  const filters = useMemo(
    () => applyStored(parsed, new URLSearchParams(spKey), stored),
    [parsed, spKey, stored],
  );
  /** The page's setter, with the memory: saves only when the layout or the sort changed. */
  const setFilters = useCallback(
    (next: IssueFilters) => {
      // LAUN-142: a sort that is only the old layout's default was not chosen. When the layout changes, it becomes the
      // new layout's default (Manual from the board button does not stay on the list). An explicit sort is kept.
      if (next.view !== filters.view && filters.sort === defaultSort(filters.view)) {
        next = { ...next, sort: defaultSort(next.view) };
      }
      if (next.view !== filters.view || next.sort !== filters.sort || next.group !== filters.group) {
        const at = Date.now();
        const cur = readStored(key, place);
        // Keeps the place's columns; the group-by is the new one (LAUN-171).
        saveStored(key, place, next.view, next.sort, at, {
          hideEmpty: cur?.hideEmpty ?? false,
          hidden: cur?.hidden ?? [],
          groupBy: next.group,
        });
        if (signedIn) {
          void sendView(org, {
            place,
            view: next.view,
            sort: next.sort,
            hideEmpty: cur?.hideEmpty ?? false,
            hiddenColumns: cur?.hidden ?? [],
            groupBy: next.group,
            at,
          });
        }
        setTick((t) => t + 1);
      }
      write(next);
    },
    [key, place, org, signedIn, filters.view, filters.sort, filters.group, write],
  );
  /** LAUN-163: the board's column choices of this place. */
  const columns: ColumnPrefs = useMemo(
    () => ({
      hideEmpty: stored?.hideEmpty ?? false,
      hidden: stored?.hidden ?? [],
      groupBy: stored?.groupBy ?? 'state',
    }),
    [stored],
  );
  /**
   * Remember new column choices. The layout and the sort stored with them are the stored ones (or the defaults),
   * not what the address shows: choosing columns must not also remember an address's `?view=board`.
   */
  const setColumns = useCallback(
    (next: ColumnPrefs) => {
      const at = Date.now();
      const cur = readStored(key, place);
      // The group-by is not the Columns control's to change: keep the stored one.
      const c: ColumnPrefs = {
        hideEmpty: next.hideEmpty,
        hidden: cleanHidden(next.hidden),
        groupBy: cur?.groupBy ?? 'state',
      };
      const view = cur?.view ?? 'list';
      const sort = cur?.sort ?? defaultSort('list');
      saveStored(key, place, view, sort, at, c);
      if (signedIn)
        void sendView(org, {
          place,
          view,
          sort,
          hideEmpty: c.hideEmpty,
          hiddenColumns: c.hidden,
          groupBy: c.groupBy,
          at,
        });
      setTick((t) => t + 1);
    },
    [key, place, org, signedIn],
  );
  /** "Reset view": forget this place, and show the defaults (the filters stay). */
  const resetView = useCallback(() => {
    const at = Date.now();
    clearStored(key, place, at);
    if (signedIn)
      void sendView(org, {
        place,
        view: 'list',
        sort: defaultSort('list'),
        hideEmpty: false,
        hiddenColumns: [],
        groupBy: 'state',
        at,
      });
    setTick((t) => t + 1);
    write({ ...filters, view: 'list', sort: defaultSort('list'), group: 'state' });
  }, [key, place, org, signedIn, filters, write]);
  // A hidden name that is no state any more changes nothing on the board; the Columns button ignores it, so
  // "Reset view" must too. Until the states have loaded nothing is filtered.
  const { states } = useAllStates();
  const known = useMemo(() => new Set(states.map((s) => s.name)), [states]);
  const liveColumns: ColumnPrefs = useMemo(
    () =>
      known.size === 0
        ? columns
        : { ...columns, hidden: columns.hidden.filter((n) => known.has(n) || isKindEntry(n)) },
    [columns, known],
  );
  const customised =
    (stored !== undefined && !isDefaultView(stored.view, stored.sort, liveColumns)) ||
    filters.view !== 'list' ||
    filters.sort !== defaultSort(filters.view) ||
    filters.group !== 'state';
  return { filters, setFilters, resetView, customised, columns, setColumns };
}
