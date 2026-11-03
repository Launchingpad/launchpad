import { useEffect, useId, useRef, useState, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { registerModal } from '../lib/hotkeys';
import { IconClose } from './Icons';
import styles from '../styles/ui.module.css';

const FOCUSABLE =
  'a[href],button:not([disabled]),input:not([disabled]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])';

export function Dialog({
  title,
  onClose,
  children,
  footer,
  hideHead,
  header,
  width,
  label,
  onKey,
  plain,
  panelProps,
  overlay,
  className,
}: {
  title: string;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
  /** Visually hidden title (the palette). */
  hideHead?: boolean;
  /** Replaces the title row. The title still names the dialog, but only for assistive technology. */
  header?: ReactNode;
  width?: number;
  label?: string;
  /** Sees every key pressed inside the dialog, popovers in a portal included (Cmd/Ctrl+Enter). */
  onKey?: (e: React.KeyboardEvent) => void;
  /** No divider lines above the footer and under the header (the create dialog, LAUN-19). */
  plain?: boolean;
  /** Extra props for the panel (the create dialog's file drop zone, LAUN-58). */
  panelProps?: React.HTMLAttributes<HTMLDivElement>;
  /** Drawn over the panel (the drop overlay). */
  overlay?: ReactNode;
  /** Added to the panel's classes (a page-scoped rule such as touch targets). */
  className?: string;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const titleId = useId();
  // Read in the render phase: a child's effect (a search box that takes focus) runs before this
  // component's own effect, so reading `activeElement` there would find the child, not the opener.
  const [previous] = useState(() => document.activeElement as HTMLElement | null);
  const onKeyRef = useRef(onKey);
  onKeyRef.current = onKey;
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;

  useEffect(() => {
    const unregister = registerModal();
    // The opener read at render time can be gone: a dialog that closes in the same batch that opens this
    // one (the palette, then "Create issue") leaves a detached input. The closing dialog has already put
    // focus back on its own opener by now (unmount effects run first), so read the live focus then.
    const opener = previous?.isConnected ? previous : (document.activeElement as HTMLElement | null);
    const root = ref.current;
    // Focus the first field, else the dialog itself.
    const first =
      root?.querySelector<HTMLElement>('[data-autofocus]') ?? root?.querySelector<HTMLElement>(FOCUSABLE);
    (first ?? root)?.focus();
    const prevOverflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    // Everything behind the dialog is unreachable by keyboard, pointer and screen reader.
    const scrim = root?.parentElement;
    const hidden: HTMLElement[] = [];
    for (const el of Array.from(document.body.children)) {
      if (
        el === scrim ||
        !(el instanceof HTMLElement) ||
        el.hasAttribute('inert') ||
        el.tagName === 'SCRIPT' ||
        el.hasAttribute('data-live-region')
      )
        continue;
      el.setAttribute('inert', '');
      el.setAttribute('aria-hidden', 'true');
      hidden.push(el);
    }
    return () => {
      for (const el of hidden) {
        el.removeAttribute('inert');
        el.removeAttribute('aria-hidden');
      }
      unregister();
      document.body.style.overflow = prevOverflow;
      // A removed opener (a row the dialog just deleted) is not a focus target; the page moves focus itself.
      if (opener?.isConnected) opener.focus?.();
    };
  }, [previous]);

  const onKeyDown = (e: React.KeyboardEvent) => {
    onKeyRef.current?.(e);
    if (e.key === 'Escape') {
      // Esc that cancels an input-method composition (title, description) is not a request to close.
      if (e.nativeEvent.isComposing || e.keyCode === 229) {
        e.stopPropagation();
        return;
      }
      e.stopPropagation();
      e.preventDefault();
      onCloseRef.current();
      return;
    }
    if (e.key !== 'Tab') return;
    const nodes = [...(ref.current?.querySelectorAll<HTMLElement>(FOCUSABLE) ?? [])].filter(
      // A popover panel lives at the end of the dialog node, but it is not the last stop of the trap.
      (n) => !n.closest('[data-floating]') && (n.offsetParent !== null || n === document.activeElement),
    );
    if (nodes.length === 0) {
      e.preventDefault();
      return;
    }
    const first = nodes[0]!;
    const last = nodes[nodes.length - 1]!;
    if (e.shiftKey && (document.activeElement === first || document.activeElement === ref.current)) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  };

  return createPortal(
    <div
      className={styles.scrim}
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
    >
      <div
        ref={ref}
        className={`${plain ? `${styles.dialog} ${styles.dialogPlain}` : styles.dialog}${className ? ` ${className}` : ''}`}
        style={width ? { width: `min(${width}px, 100%)` } : undefined}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        aria-label={label}
        tabIndex={-1}
        onKeyDown={onKeyDown}
        {...panelProps}
      >
        {header ? (
          <>
            <h2 id={titleId} className="sr-only">
              {title}
            </h2>
            <div className={styles.dialogHead}>{header}</div>
          </>
        ) : hideHead ? (
          <h2 id={titleId} className="sr-only">
            {title}
          </h2>
        ) : (
          <div className={styles.dialogHead}>
            <h2 id={titleId} style={{ fontSize: 15 }}>
              {title}
            </h2>
            <button
              type="button"
              className={`${styles.btn} ${styles.ghost} ${styles.icon}`}
              onClick={onClose}
              aria-label="Close"
            >
              <IconClose />
            </button>
          </div>
        )}
        <div className={styles.dialogBody}>{children}</div>
        {footer && <div className={styles.dialogFoot}>{footer}</div>}
        {overlay}
      </div>
    </div>,
    document.body,
  );
}
