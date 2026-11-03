import { useEffect, useRef } from 'react';

// Dialogs register here so list shortcuts stay quiet while one is open.
let modalCount = 0;
export function registerModal(): () => void {
  modalCount += 1;
  return () => {
    modalCount = Math.max(0, modalCount - 1);
  };
}
export function modalOpen(): boolean {
  return modalCount > 0;
}

export function isTypingTarget(t: EventTarget | null): boolean {
  if (!(t instanceof HTMLElement)) return false;
  if (t.isContentEditable) return true;
  const tag = t.tagName;
  if (tag === 'TEXTAREA' || tag === 'SELECT') return true;
  if (tag === 'INPUT') {
    const type = (t as HTMLInputElement).type;
    return !['checkbox', 'radio', 'button', 'submit', 'reset'].includes(type);
  }
  return false;
}

export type HotkeyMap = Record<string, (e: KeyboardEvent) => void>;

/**
 * `mod+k` works anywhere (even in inputs). Plain keys (`j`, `?`, `Enter`...)
 * never fire while typing in a field or while a dialog is open, and never
 * with Ctrl/Meta/Alt held.
 */
export function useHotkeys(map: HotkeyMap, enabled = true): void {
  const ref = useRef(map);
  ref.current = map;
  useEffect(() => {
    if (!enabled) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.defaultPrevented || e.isComposing) return;
      const mod = e.metaKey || e.ctrlKey;
      if (mod && !e.altKey) {
        const h = ref.current[`mod+${e.key.toLowerCase()}`];
        if (h) {
          e.preventDefault();
          h(e);
        }
        return;
      }
      if (e.altKey) return;
      if (isTypingTarget(e.target) || modalOpen()) return;
      // LAUN-163: a popover panel that marks itself owns its keys (a checkbox in it must not trigger j/x/Enter).
      if (e.target instanceof HTMLElement && e.target.closest('[data-no-hotkeys]')) return;
      // Enter on a focused control belongs to that control.
      if (
        e.key === 'Enter' &&
        e.target instanceof HTMLElement &&
        ['BUTTON', 'A', 'SUMMARY'].includes(e.target.tagName)
      )
        return;
      const h = ref.current[e.key];
      if (h) {
        e.preventDefault();
        h(e);
      }
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, [enabled]);
}
