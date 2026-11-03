/** GFM task lists (LAUN-47). The renderer finds each marker's offset in the source; this flips one. */

const MARKER = /^\[([ xX])\]$/;

/** The marker at `offset` is `[ ]`, `[x]` or `[X]`. */
export function isTaskMarkerAt(source: string, offset: number): boolean {
  return MARKER.test(source.slice(offset, offset + 3));
}

/**
 * The source with the task marker at `offset` flipped (`[ ]` <-> `[x]`), or null when no marker is
 * there (the text changed under the click). Exactly three characters change; line endings stay.
 */
export function toggleTaskAt(source: string, offset: number): string | null {
  const m = MARKER.exec(source.slice(offset, offset + 3));
  if (!m) return null;
  return source.slice(0, offset + 1) + (m[1] === ' ' ? 'x' : ' ') + source.slice(offset + 2);
}
