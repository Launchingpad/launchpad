import { useCallback, useEffect, useRef, useState, type DragEvent } from 'react';
import styles from '../styles/ui.module.css';

/** True for a drag that carries files. Text and links (a selection, a tab) do not. */
export function isFileDrag(e: { dataTransfer: DataTransfer | null }): boolean {
  const types = e.dataTransfer?.types;
  return !!types && Array.from(types).includes('Files');
}

const seen = new WeakSet<object>();

/** The files of ONE drop with exact repeats (name, size, type, modified time) removed. Separate drops are never merged. */
export function uniqueFiles(files: File[]): File[] {
  const keys = new Set<string>();
  return files.filter((f) => {
    const k = `${f.name}\u0000${f.size}\u0000${f.type}\u0000${f.lastModified}`;
    if (keys.has(k)) return false;
    keys.add(k);
    return true;
  });
}

/**
 * A drop zone for files (LAUN-58). Spread `zoneProps` on the element; show <DropOverlay/> while `active`.
 *
 * - dragenter and dragleave fire for every nested child, so they are COUNTED; the overlay clears at zero.
 * - Only file drags count: a dragged text or link leaves the zone alone.
 * - The overlay also clears on drop, Escape and dragend anywhere in the window, so it cannot stick.
 * - While mounted, a file dropped OUTSIDE the zone is cancelled at the window, so the browser never
 *   navigates away to open it.
 * - Pointer events are not used, so the board's pointer drag (LAUN-29) is not touched.
 */
export function useFileDrop(onFiles: (files: File[], e: DragEvent<HTMLElement>) => void, disabled = false) {
  const [active, setActive] = useState(false);
  const depth = useRef(0);
  const handler = useRef(onFiles);
  handler.current = onFiles;
  const off = useRef(disabled);
  off.current = disabled;

  const clear = useCallback(() => {
    depth.current = 0;
    setActive(false);
  }, []);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') clear();
    };
    const guardOver = (e: globalThis.DragEvent) => {
      if (e.defaultPrevented || !isFileDrag(e)) return;
      e.preventDefault(); // without this the browser would open the file in the tab
      if (e.dataTransfer) e.dataTransfer.dropEffect = 'none';
    };
    const guardDrop = (e: globalThis.DragEvent) => {
      if (isFileDrag(e)) e.preventDefault();
      clear();
    };
    window.addEventListener('keydown', onKey, true);
    window.addEventListener('dragend', clear);
    // No mouse event fires while a drag runs, so one proves the drag is over (a missed leave or drop).
    window.addEventListener('mousemove', clear);
    window.addEventListener('pointermove', clear);
    window.addEventListener('blur', clear);
    const onVis = () => clear();
    document.addEventListener('visibilitychange', onVis);
    window.addEventListener('dragover', guardOver);
    window.addEventListener('drop', guardDrop);
    return () => {
      window.removeEventListener('keydown', onKey, true);
      window.removeEventListener('dragend', clear);
      window.removeEventListener('mousemove', clear);
      window.removeEventListener('pointermove', clear);
      window.removeEventListener('blur', clear);
      document.removeEventListener('visibilitychange', onVis);
      window.removeEventListener('dragover', guardOver);
      window.removeEventListener('drop', guardDrop);
    };
  }, [clear]);

  // A zone that becomes disabled (an upload runs) must not keep its overlay.
  useEffect(() => {
    if (disabled) clear();
  }, [disabled, clear]);

  const zoneProps = {
    onDragEnter(e: DragEvent<HTMLElement>) {
      if (off.current || !isFileDrag(e)) return;
      depth.current += 1;
      setActive(true);
    },
    onDragOver(e: DragEvent<HTMLElement>) {
      if (off.current || !isFileDrag(e)) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = 'copy';
      // A dragenter can be missed (the drag began inside the zone's child before this mounted):
      // a dragover proves that a file drag is over the zone.
      if (depth.current === 0) {
        depth.current = 1;
        setActive(true);
      }
    },
    onDragLeave(e: DragEvent<HTMLElement>) {
      if (off.current || !isFileDrag(e)) return;
      depth.current = Math.max(0, depth.current - 1);
      if (depth.current === 0) setActive(false);
    },
    onDrop(e: DragEvent<HTMLElement>) {
      if (!isFileDrag(e)) return;
      e.preventDefault();
      clear();
      if (off.current) return;
      // One drop is one upload per file: a native event is handled once, and the same file listed twice in
      // one drop (some browsers list a dragged screenshot twice) is one file (LAUN-105).
      if (seen.has(e.nativeEvent)) return;
      seen.add(e.nativeEvent);
      const files = uniqueFiles(Array.from(e.dataTransfer.files ?? []));
      if (files.length) handler.current(files, e);
    },
  };
  return { active, zoneProps };
}

/** The soft overlay of a drop zone. It takes no pointer events, so it never disturbs the drag counting. */
export function DropOverlay({ target }: { target?: string }) {
  return (
    <div className={styles.dropOverlay} aria-hidden="true" data-testid="drop-overlay">
      <span className={styles.dropLabel}>
        <strong>{target ? `Drop files to attach to ${target}` : 'Drop files to attach'}</strong>
      </span>
    </div>
  );
}
