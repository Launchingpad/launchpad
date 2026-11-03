import { useCallback, useEffect, useSyncExternalStore } from 'react';

export type ThemePref = 'system' | 'light' | 'dark';
const KEY = 'devops-theme';

// LAUN-235: the choice lives in localStorage and every useTheme() reads it from there, so the shell menu, the
// command palette and the Appearance page show the same value (before, each kept its own copy in React state).
// When storage cannot be written (blocked, or full) the choice lasts for this page only (memoryPref): it wins over
// what storage holds until a later write succeeds.
let memoryPref: ThemePref | null = null;
const prefListeners = new Set<() => void>();

function read(): ThemePref {
  if (memoryPref !== null) return memoryPref;
  try {
    const v = localStorage.getItem(KEY);
    return v === 'light' || v === 'dark' ? v : 'system';
  } catch {
    return 'system';
  }
}

/** Test hook: forget a page-only variant. */
export function resetThemeMemory(): void {
  memoryPref = null;
}

export function applyTheme(pref: ThemePref): void {
  const el = document.documentElement;
  if (pref === 'system') el.removeAttribute('data-theme');
  else el.setAttribute('data-theme', pref);
}

function subscribePref(f: () => void): () => void {
  listenToOtherTabs();
  prefListeners.add(f);
  return () => prefListeners.delete(f);
}

export function useTheme() {
  const pref = useSyncExternalStore(subscribePref, read, () => 'system' as ThemePref);
  useEffect(() => applyTheme(pref), [pref]);
  const setPref = useCallback((p: ThemePref) => {
    try {
      if (p === 'system') localStorage.removeItem(KEY);
      else localStorage.setItem(KEY, p);
      memoryPref = null;
    } catch {
      memoryPref = p; /* storage blocked or full: the choice lasts for this page only */
    }
    prefListeners.forEach((f) => f());
  }, []);
  const cycle = useCallback(
    () => setPref(pref === 'system' ? 'light' : pref === 'light' ? 'dark' : 'system'),
    [pref, setPref],
  );
  return { pref, setPref, cycle };
}

// ---------- LAUN-235: the theme (look): Linear, Paper or Terminal ----------
// decisions-T42-themes.md. The look is a second axis next to the light/dark choice above: <html data-look>
// carries it (none for Linear, so the Linear CSS is untouched) and data-theme keeps the variant. The server
// holds the person's choice (GET/PUT /me/preferences); localStorage mirrors it so public/theme-init.js can
// set the attribute before the first paint (no flash). The mirror is per browser, not per person.

export type Look = 'linear' | 'paper' | 'terminal';
export const LOOKS: readonly Look[] = ['linear', 'paper', 'terminal'];
export const LOOK_KEY = 'launchpad-look';

export function isLook(v: unknown): v is Look {
  return v === 'linear' || v === 'paper' || v === 'terminal';
}

/** The look on the page now (the attribute is the truth: theme-init.js set it before React ran). */
export function currentLook(): Look {
  const v = document.documentElement.getAttribute('data-look');
  return isLook(v) ? v : 'linear';
}

const lookListeners = new Set<() => void>();

/** Show a look at once and remember it in this browser for the next first paint. */
export function applyLook(look: Look): void {
  const el = document.documentElement;
  const before = el.getAttribute('data-look');
  if (look === 'linear') el.removeAttribute('data-look');
  else el.setAttribute('data-look', look);
  try {
    if (look === 'linear') localStorage.removeItem(LOOK_KEY);
    else localStorage.setItem(LOOK_KEY, look);
  } catch {
    /* storage blocked: the next load starts in Linear until the server answers */
  }
  if (before !== el.getAttribute('data-look')) lookListeners.forEach((f) => f());
}

function subscribeLook(f: () => void): () => void {
  listenToOtherTabs();
  lookListeners.add(f);
  return () => lookListeners.delete(f);
}

/** The look on the page; re-renders when applyLook changes it. */
export function useLook(): Look {
  return useSyncExternalStore(subscribeLook, currentLook, () => 'linear' as Look);
}

// LAUN-235 review: another tab of this browser changed the theme or the variant (a `storage` event fires in every
// OTHER tab): follow it. The look is shown without writing storage again; the variant is read from storage.
let listening = false;
function listenToOtherTabs(): void {
  if (listening || typeof window === 'undefined') return;
  listening = true;
  window.addEventListener('storage', (e) => {
    if (e.key === LOOK_KEY || e.key === null) {
      const el = document.documentElement;
      const before = el.getAttribute('data-look');
      if (isLook(e.newValue) && e.newValue !== 'linear') el.setAttribute('data-look', e.newValue);
      else el.removeAttribute('data-look');
      if (before !== el.getAttribute('data-look')) lookListeners.forEach((f) => f());
    }
    if (e.key === KEY || e.key === null) prefListeners.forEach((f) => f());
  });
}
