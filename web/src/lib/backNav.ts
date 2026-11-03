import { useEffect, useRef, type RefObject } from 'react';
import { useLocation, useNavigate, type Location } from 'react-router-dom';

/**
 * True when this page is the first entry of the app in this tab, so history has nothing earlier to go back to
 * (LAUN-89). react-router (browser history) keeps `idx` in `window.history.state`: 0 is the first entry, also
 * after the logged-out redirect and the login `replace`, which give that entry a random `location.key`. Where
 * there is no `idx` (MemoryRouter in tests) the key `default` of the initial entry says the same.
 */
export function hasNoEarlierEntry(loc: Pick<Location, 'key'>): boolean {
  const idx = (window.history.state as { idx?: unknown } | null)?.idx;
  if (typeof idx === 'number') return idx === 0;
  return loc.key === 'default';
}

/** Back one entry, or to `fallback` (the team's issue list) when the app has no earlier entry. */
export function useGoBack(fallback: string): () => void {
  const loc = useLocation();
  const nav = useNavigate();
  return () => (hasNoEarlierEntry(loc) ? nav(fallback) : nav(-1));
}

const saved = new Map<string, { top: number; left: number }>();
const SAVED_MAX = 100;
function remember(id: string, top: number, left: number) {
  saved.delete(id); // newest last
  saved.set(id, { top, left });
  while (saved.size > SAVED_MAX) saved.delete(saved.keys().next().value as string);
}

/**
 * Remember the scroll offset of a scroll container per history entry, and put it back when the entry comes
 * back (Back, Alt+Left). The data may load after the mount, so the offset is applied again each frame for
 * about 2 seconds, until it holds or the person scrolls (LAUN-89).
 *
 * `loadMore` (LAUN-90) is for a list that pages: called each frame while the offset is out of reach, it starts
 * the next page when none is on the way and returns true while it is waiting for one (false: nothing more to
 * load). While it returns true the wait goes on, up to 10 seconds in all; the caller caps the page count.
 */
export function useScrollRestore<T extends HTMLElement>(
  name: string,
  external?: RefObject<T>,
  enabled = true,
  loadMore?: () => boolean,
) {
  const loc = useLocation();
  const more = useRef(loadMore);
  more.current = loadMore;
  const own = useRef<T>(null);
  const ref = external ?? own;
  const id = `${loc.key}|${loc.pathname}${loc.search}|${name}`;
  useEffect(() => {
    const el = ref.current;
    if (!el || !enabled) return;
    const want = saved.get(id);
    let restoring = !!want && (want.top > 0 || want.left > 0);
    let frame = 0;
    let tries = 0;
    const started = Date.now();
    const stop = () => {
      restoring = false;
      cancelAnimationFrame(frame);
    };
    const step = () => {
      if (!want) return;
      el.scrollTop = want.top;
      el.scrollLeft = want.left;
      if (Math.abs(el.scrollTop - want.top) < 2 && Math.abs(el.scrollLeft - want.left) < 2) {
        // Held. Keep watching one more frame in case late rows push it back.
        if (tries++ > 3) return void (restoring = false);
      } else if (more.current?.()) {
        // Out of reach and a page is coming: wait for it, within the overall limit.
        if (Date.now() - started > 10_000) return void (restoring = false);
      } else if (tries++ > 120) return void (restoring = false);
      frame = requestAnimationFrame(step);
    };
    const onScroll = () => {
      if (!restoring) remember(id, el.scrollTop, el.scrollLeft);
    };
    if (restoring) step();
    const user = ['wheel', 'touchstart', 'pointerdown', 'keydown'] as const;
    user.forEach((t) => el.addEventListener(t, stop, { passive: true }));
    el.addEventListener('scroll', onScroll, { passive: true });
    return () => {
      stop();
      user.forEach((t) => el.removeEventListener(t, stop));
      el.removeEventListener('scroll', onScroll);
    };
  }, [id, enabled, ref]);
  return ref;
}

const picked = new Map<string, string>();
/**
 * The selected row of a list, remembered per history entry like the scroll offset (LAUN-90). Returns the value
 * saved for this entry, if any, and a setter. The map is in memory, at most 100 entries.
 */
export function useEntryMemory(name: string): [string | undefined, (v: string | undefined) => void] {
  const loc = useLocation();
  const id = `${loc.key}|${loc.pathname}${loc.search}|${name}`;
  const first = useRef<string | undefined>(picked.get(id));
  return [
    first.current,
    (v) => {
      picked.delete(id);
      if (v !== undefined) picked.set(id, v);
      while (picked.size > SAVED_MAX) picked.delete(picked.keys().next().value as string);
    },
  ];
}

/** Tests only: forget every remembered offset and row, as a reload does. */
export function resetBackNavMemory() {
  saved.clear();
  picked.clear();
}

/**
 * The decision of a paging list's `loadMore` callback for `useScrollRestore` (LAUN-90). `fetch`: ask for the next
 * page now; `wait`: the restore should keep waiting. Nothing more, or a failed page: neither (the list's own error
 * line and Try again take over). It asks once per history entry and loaded page count, because `fetchNextPage`
 * cancels and restarts a fetch that is already on its way. At most 10 pages.
 */
export function nextPageStep(s: {
  hasNextPage: boolean;
  failed: boolean;
  busy: boolean;
  pages: number;
  asked: string;
  entry: string;
}): { fetch: boolean; wait: boolean } {
  if (!s.hasNextPage || s.failed) return { fetch: false, wait: false };
  const fetch = !s.busy && s.pages < 10 && s.asked !== `${s.entry}|${s.pages}`;
  return { fetch, wait: s.busy || s.pages < 10 };
}
