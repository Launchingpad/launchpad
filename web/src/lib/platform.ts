/** The key a hint names for "also change priority": Command on Apple systems, Ctrl elsewhere (a drop accepts either). */
export function modKeyName(): string {
  return typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.platform) ? '\u2318' : 'Ctrl';
}

/** A touch screen: there is no Ctrl or Command key to hold during a drag, so the hint leaves that line out. */
export function isCoarsePointer(): boolean {
  return typeof window !== 'undefined' && !!window.matchMedia?.('(pointer: coarse)').matches;
}

/** An Apple system: Command is the shortcut key there (LAUN-236: Cmd+Z on macOS, Ctrl+Z elsewhere). */
export function isApple(): boolean {
  return typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.platform);
}
