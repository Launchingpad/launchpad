import { useCallback, useEffect, useLayoutEffect, useRef, useState, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import styles from '../styles/ui.module.css';

const GAP = 4;
// A date or time input has segments: Tab and Shift+Tab move between them inside ONE element.
const SEGMENTED = new Set(['date', 'time', 'datetime-local', 'month', 'week']);
const MARGIN = 8;

interface Place {
  top: number;
  left: number;
  maxHeight: number;
}

/**
 * A panel under (or over) an anchor button. It renders in a portal inside the dialog node that holds the
 * anchor (`document.body` when there is none), so a dialog body that scrolls or clips cannot cut it off. It flips above the anchor when there is more room
 * there, and it stays inside the viewport. Esc and Tab close it; so does a click outside it.
 *
 * `onClose(true)` asks the caller to put focus back on the anchor (Esc, Tab, a choice).
 * `onClose(false)` is a click outside: the pointer already chose a new focus.
 */
export function Floating({
  anchor,
  onClose,
  children,
  label,
  width = 320,
}: {
  anchor: HTMLElement | null;
  onClose: (restoreFocus: boolean) => void;
  children: ReactNode;
  label: string;
  width?: number;
}) {
  const panel = useRef<HTMLDivElement>(null);
  const [place, setPlace] = useState<Place | null>(null);
  const closeRef = useRef(onClose);
  closeRef.current = onClose;

  const measure = useCallback(() => {
    const p = panel.current;
    if (!anchor || !p) return;
    const a = anchor.getBoundingClientRect();
    const vw = window.innerWidth;
    const vh = window.innerHeight;
    const w = Math.min(width, vw - 2 * MARGIN);
    const left = Math.max(MARGIN, Math.min(a.left, vw - w - MARGIN));
    const below = vh - a.bottom - GAP - MARGIN;
    const above = a.top - GAP - MARGIN;
    const natural = p.scrollHeight;
    // jsdom has no layout: every rect is zero, so the panel simply opens below.
    const down = natural <= below || below >= above;
    const room = Math.max(120, down ? below : above);
    const h = Math.min(natural, room);
    setPlace({ left, top: down ? a.bottom + GAP : a.top - GAP - h, maxHeight: room });
  }, [anchor, width]);

  useLayoutEffect(() => {
    measure();
  }, [measure, children]);

  useEffect(() => {
    const onMove = () => measure();
    window.addEventListener('resize', onMove);
    window.addEventListener('scroll', onMove, true);
    const onDown = (e: MouseEvent) => {
      const t = e.target as Node;
      if (panel.current?.contains(t) || anchor?.contains(t)) return;
      // The click only closes the panel. It must not also close the dialog under it.
      e.stopPropagation();
      closeRef.current(false);
    };
    document.addEventListener('mousedown', onDown, true);
    // A filter can change the list height: place the panel again, so one that opened above does not drift.
    const ro = typeof ResizeObserver === 'undefined' ? null : new ResizeObserver(onMove);
    if (panel.current) ro?.observe(panel.current);
    return () => {
      ro?.disconnect();
      window.removeEventListener('resize', onMove);
      window.removeEventListener('scroll', onMove, true);
      document.removeEventListener('mousedown', onDown, true);
    };
  }, [anchor, measure]);

  // Inside the dialog node when there is one: a modal dialog (aria-modal) can hide everything outside
  // its own node from assistive technology, and a panel on <body> would be hidden with it.
  const host = anchor?.closest<HTMLElement>('[role="dialog"]') ?? document.body;

  return createPortal(
    <div
      ref={panel}
      className={styles.floatPanel}
      role="presentation"
      data-floating={label}
      tabIndex={-1}
      style={{
        width: Math.min(width, window.innerWidth - 2 * MARGIN),
        top: place?.top ?? 0,
        left: place?.left ?? 0,
        maxHeight: place?.maxHeight,
        // Not `visibility: hidden`: a hidden element cannot take focus, and the list focuses its search box at once.
        opacity: place ? 1 : 0,
      }}
      onMouseDown={(e) => {
        // A press on text, padding or an empty-state row must not move focus to the dialog node (it has
        // tabindex -1): the search box keeps focus, so Esc still closes only this panel. A label still
        // focuses its field, because its click does that.
        // Buttons are not excluded: Safari and Firefox on Mac do not focus a button on press, and a button
        // that disables itself loses focus. The click still fires after a prevented mousedown.
        if (!(e.target as HTMLElement).closest('input,select,textarea,[role="option"]')) e.preventDefault();
      }}
      onBlur={() => {
        // Focus moved out of the panel by a key the panel did not see (Shift+Tab out of a date field).
        setTimeout(() => {
          const p = panel.current;
          const a = document.activeElement;
          if (!p || !a || !p.isConnected || p.contains(a) || anchor?.contains(a)) return;
          if (a === document.body || a.contains(p)) {
            // Focus fell to the body or to the dialog node (for example from a button that disabled itself).
            // Put it back in the panel, so Esc still has the panel in its key path and closes only the panel.
            const first = p.querySelector<HTMLElement>(
              'input:not([disabled]),button:not([disabled]),select:not([disabled]),textarea:not([disabled])',
            );
            (first ?? p).focus();
            return;
          }
          closeRef.current(true); // by keyboard: focus goes back to the chip
        }, 0);
      }}
      onKeyDown={(e) => {
        if (e.key === 'Escape') {
          // Esc while an input method is composing text cancels the composition, not the panel.
          if (e.nativeEvent.isComposing || e.keyCode === 229) {
            e.stopPropagation(); // and the dialog under it must not close either
            return;
          }
          // Only the panel closes. The dialog under it stays.
          e.stopPropagation();
          e.preventDefault();
          onClose(true);
        } else if (e.key === 'Tab') {
          const active = document.activeElement;
          if (active instanceof HTMLInputElement && SEGMENTED.has(active.type)) {
            // The browser decides if this Tab stays in the field; the blur handler closes the panel if not.
            e.stopPropagation();
            return;
          }
          // Tab moves between the fields of the panel. It closes the panel only when focus would leave it
          // (Tab from the last field, Shift+Tab from the first).
          const nodes = [
            ...(panel.current?.querySelectorAll<HTMLElement>(
              'input:not([disabled]),button:not([disabled]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])',
            ) ?? []),
          ];
          const at = nodes.indexOf(active as HTMLElement);
          const leaving = e.shiftKey ? at <= 0 : at === -1 || at === nodes.length - 1;
          if (leaving) {
            // Close, and let the key go on to the dialog: its focus trap needs to see a Tab that now
            // starts on the anchor, or Shift+Tab from the first chip would leave the modal.
            onClose(true);
          } else {
            e.stopPropagation();
          }
        }
      }}
    >
      {children}
    </div>,
    host,
  );
}
