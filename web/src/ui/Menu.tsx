import { useEffect, useId, useRef, useState, type ReactNode } from 'react';
import { IconChevron } from './Icons';
import styles from '../styles/ui.module.css';

export interface MenuItem {
  label: string;
  onSelect: () => void;
  icon?: ReactNode;
  danger?: boolean;
  /** A heading row, not selectable. */
  heading?: boolean;
  current?: boolean;
  /** Small muted text after the label (for example dates). */
  detail?: string;
  /** A shortcut shown at the right, also set as aria-keyshortcuts (for example `Alt+K`). */
  hint?: string;
  /** The row cannot be used now (LAUN-172). It stays in the list, dimmed, with `reason` under the label; choosing it does nothing. */
  disabled?: boolean;
  /** One line that says why the row is disabled. Read as the row's description. */
  reason?: string;
}

export function Menu({
  trigger,
  items,
  label,
  triggerClassName,
  up,
  align = 'left',
  hideChevron,
  wrapClassName,
  panelClassName,
  closeKey,
}: {
  trigger: ReactNode;
  items: MenuItem[];
  label: string;
  triggerClassName?: string;
  up?: boolean;
  align?: 'left' | 'right';
  /** No chevron after the trigger (an icon-only `...` button). */
  hideChevron?: boolean;
  /** Extra class on the wrapper (it then has no inline width) and on the panel. */
  wrapClassName?: string;
  panelClassName?: string;
  /** When this value changes, an open menu closes (for example the page it belongs to changed). */
  closeKey?: unknown;
}) {
  const [open, setOpen] = useState(false);
  const wrap = useRef<HTMLDivElement>(null);
  const btn = useRef<HTMLButtonElement>(null);
  const reasonId = useId();

  useEffect(() => setOpen(false), [closeKey]);

  useEffect(() => {
    if (!open) return;
    wrap.current?.querySelector<HTMLElement>('[role="menuitem"]')?.focus();
    const onDown = (e: MouseEvent) => {
      if (!wrap.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', onDown);
    return () => document.removeEventListener('mousedown', onDown);
  }, [open]);

  const onKey = (e: React.KeyboardEvent) => {
    if (e.key === 'Escape' && open) {
      e.stopPropagation();
      setOpen(false);
      btn.current?.focus();
      return;
    }
    if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return;
    const nodes = [...(wrap.current?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? [])];
    if (!nodes.length) return;
    e.preventDefault();
    const i = nodes.indexOf(document.activeElement as HTMLElement);
    nodes[(i + (e.key === 'ArrowDown' ? 1 : -1) + nodes.length) % nodes.length]!.focus();
  };

  return (
    <div
      className={`${styles.popWrap} ${wrapClassName ?? ''}`}
      ref={wrap}
      onKeyDown={onKey}
      style={wrapClassName ? undefined : { width: '100%' }}
    >
      <button
        ref={btn}
        type="button"
        className={triggerClassName ?? styles.btn}
        aria-haspopup="menu"
        aria-expanded={open}
        aria-label={label}
        onClick={() => setOpen((o) => !o)}
      >
        {trigger}
        {!hideChevron && <IconChevron />}
      </button>
      {open && (
        <div
          className={`${styles.panel} ${up ? styles.panelUp : ''} ${align === 'right' ? styles.panelRight : ''} ${panelClassName ?? ''}`}
          role="menu"
          aria-label={label}
        >
          {items.map((it, i) =>
            it.heading ? (
              <div key={i} className={styles.hint} style={{ padding: '6px 8px' }} role="presentation">
                {it.label}
              </div>
            ) : (
              <button
                key={i}
                type="button"
                role="menuitem"
                className={`${styles.option} ${styles.menuRow} ${it.danger ? styles.danger : ''} ${it.disabled ? styles.menuDisabled : ''}`}
                aria-current={it.current ? 'true' : undefined}
                aria-keyshortcuts={it.hint}
                aria-disabled={it.disabled ? 'true' : undefined}
                aria-describedby={it.disabled && it.reason ? `${reasonId}-${i}` : undefined}
                onClick={() => {
                  if (it.disabled) return; // stays open, so the reason can be read
                  setOpen(false);
                  btn.current?.focus();
                  it.onSelect();
                }}
              >
                {it.icon}
                <span className={styles.menuText} title={it.label}>
                  {it.label}
                  {it.disabled && it.reason && (
                    <span id={`${reasonId}-${i}`} className={`${styles.hint} ${styles.menuReason}`}>
                      {it.reason}
                    </span>
                  )}
                </span>
                {it.detail && <span className={`${styles.hint} ${styles.menuDetail}`}>{it.detail}</span>}
                {it.hint && <kbd className={`${styles.hint} ${styles.menuKey}`}>{it.hint}</kbd>}
                {it.current && <span className={`${styles.hint} ${styles.menuKey}`}>current</span>}
              </button>
            ),
          )}
        </div>
      )}
    </div>
  );
}
