import { Fragment, useEffect, useId, useMemo, useRef, useState, type ReactNode } from 'react';
import { Dialog } from './Dialog';
import { IconCheck, IconChevron } from './Icons';
import styles from '../styles/ui.module.css';

export interface PickerOption {
  id: string;
  label: string;
  icon?: ReactNode;
  hint?: string;
  /** A tooltip and the accessible name of the row (LAUN-100: a product with its full list of teams). */
  title?: string;
  /** A small gray heading above the first row of each run of options with the same section (LAUN-53). */
  section?: string;
}

export interface ListProps {
  options: PickerOption[];
  selected: string[];
  onSelect: (o: PickerOption) => void;
  placeholder?: string;
  /** Local filtering is on unless the options already come filtered (remote search). */
  remote?: boolean;
  onQuery?: (q: string) => void;
  loading?: boolean;
  emptyText?: string;
  /** Offer to create a value that matches no option. */
  onCreate?: (text: string) => void;
  ariaLabel: string;
  /** The list allows several values (sets aria-multiselectable). */
  multi?: boolean;
  /** A shortcut letter shown at the right of the search box. */
  keyHint?: string;
}

export function PickerList({
  options,
  selected,
  onSelect,
  placeholder = 'Search…',
  remote,
  onQuery,
  loading,
  emptyText = 'No matches',
  onCreate,
  ariaLabel,
  multi,
  keyHint,
}: ListProps) {
  const [q, setQ] = useState('');
  const [active, setActive] = useState(0);
  const listId = useId();
  const shown = useMemo(() => {
    if (remote) return options;
    const t = q.trim().toLowerCase();
    return t
      ? options.filter((o) => o.label.toLowerCase().includes(t) || o.hint?.toLowerCase().includes(t))
      : options;
  }, [options, q, remote]);
  // Headings show only when two or more sections have a row left: one section needs no name.
  const sectionCount = new Set(shown.map((o) => o.section).filter(Boolean)).size;
  const canCreate =
    !!onCreate &&
    q.trim().length > 0 &&
    !options.some((o) => o.label.toLowerCase() === q.trim().toLowerCase());
  const total = shown.length + (canCreate ? 1 : 0);
  const activeIdx = Math.min(active, Math.max(0, total - 1));
  const listRef = useRef<HTMLUListElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  // Typing filters at once: the search box owns focus from the moment the list shows.
  useEffect(() => inputRef.current?.focus(), []);

  useEffect(() => {
    listRef.current?.querySelector('[data-active="true"]')?.scrollIntoView?.({ block: 'nearest' });
  }, [activeIdx]);

  const choose = (i: number) => {
    const o = shown[i];
    if (o) onSelect(o);
    else if (canCreate) onCreate!(q.trim());
  };

  return (
    <div>
      <div className={styles.pickerSearchWrap}>
        <input
          ref={inputRef}
          data-autofocus
          className={`${styles.pickerSearch} ${keyHint ? styles.pickerSearchHint : ''}`}
          role="combobox"
          aria-expanded="true"
          aria-autocomplete="list"
          aria-controls={listId}
          aria-activedescendant={total ? `${listId}-${activeIdx}` : undefined}
          aria-label={ariaLabel}
          placeholder={placeholder}
          value={q}
          autoComplete="off"
          onChange={(e) => {
            setQ(e.target.value);
            setActive(0);
            onQuery?.(e.target.value);
          }}
          onKeyDown={(e) => {
            if (e.key === 'ArrowDown') {
              e.preventDefault();
              setActive((activeIdx + 1) % Math.max(total, 1));
            } else if (e.key === 'ArrowUp') {
              e.preventDefault();
              setActive((activeIdx - 1 + Math.max(total, 1)) % Math.max(total, 1));
            } else if (e.key === ' ' && multi && q === '' && total) {
              // Space ticks the row, as in Linear, while the box is empty (a leading space means nothing).
              e.preventDefault();
              choose(activeIdx);
            } else if (e.key === 'Enter') {
              // Cmd/Ctrl+Enter belongs to the dialog (create). It must not also pick the highlighted option.
              if (e.metaKey || e.ctrlKey) return;
              // Enter that confirms an input-method composition is not a choice.
              if (e.nativeEvent.isComposing || e.keyCode === 229) return;
              e.preventDefault();
              if (total) choose(activeIdx);
            }
          }}
        />
        {keyHint && (
          <kbd className={styles.pickerKey} aria-hidden="true">
            {keyHint}
          </kbd>
        )}
      </div>
      <ul
        ref={listRef}
        id={listId}
        role="listbox"
        aria-label={ariaLabel}
        aria-multiselectable={multi ? 'true' : undefined}
        className={styles.options}
      >
        {shown.map((o, i) => {
          const sel = selected.includes(o.id);
          const head = sectionCount > 1 && o.section && o.section !== shown[i - 1]?.section;
          return (
            <Fragment key={o.id}>
              {head && (
                <li className={styles.sectionHead} role="presentation" aria-hidden="true">
                  {o.section}
                </li>
              )}
              <li
                id={`${listId}-${i}`}
                role="option"
                aria-selected={sel}
                title={o.title}
                aria-label={o.title}
                data-active={i === activeIdx}
                className={`${styles.option} ${multi ? styles.optionMulti : ''} ${i === activeIdx ? styles.optionActive : ''}`}
                onMouseMove={() => setActive(i)}
                onMouseDown={(e) => e.preventDefault()}
                onClick={() => onSelect(o)}
              >
                {multi && (
                  <span className={`${styles.checkBox} ${sel ? styles.checkOn : ''}`} aria-hidden="true">
                    {sel && <IconCheck width={12} height={12} />}
                  </span>
                )}
                {o.icon}
                <span className={styles.optionText}>{o.label}</span>
                {o.hint && <span className={o.title ? styles.optionHint : styles.hint}>{o.hint}</span>}
                {sel && !multi && <IconCheck />}
              </li>
            </Fragment>
          );
        })}
        {canCreate && (
          <li
            id={`${listId}-${shown.length}`}
            role="option"
            aria-selected={false}
            data-active={activeIdx === shown.length}
            className={`${styles.option} ${activeIdx === shown.length ? styles.optionActive : ''}`}
            onMouseDown={(e) => e.preventDefault()}
            onClick={() => onCreate!(q.trim())}
          >
            <span className={styles.optionText}>Create “{q.trim()}”</span>
          </li>
        )}
        {total === 0 && (
          <li className={styles.hint} style={{ padding: 8 }} role="presentation">
            {loading ? 'Searching…' : emptyText}
          </li>
        )}
      </ul>
    </div>
  );
}

interface PopoverProps extends Omit<ListProps, 'onSelect' | 'selected'> {
  /** What the trigger shows. */
  display: ReactNode;
  selected: string[];
  onSelect: (o: PickerOption) => void;
  multi?: boolean;
  triggerClassName?: string;
  disabled?: boolean;
  align?: 'left' | 'right';
  /** Read before the shown value by a screen reader ("Status: Backlog"). */
  triggerLabel?: string;
  /** A tooltip on the trigger (LAUN-100: the full list when `display` is cut short). */
  triggerTitle?: string;
}

/** A button that opens a searchable list under it. */
export function PickerPopover({
  display,
  multi,
  triggerClassName,
  disabled,
  align,
  triggerLabel,
  triggerTitle,
  onSelect,
  ...list
}: PopoverProps) {
  const [open, setOpen] = useState(false);
  const wrap = useRef<HTMLDivElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (!wrap.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', onDown);
    return () => document.removeEventListener('mousedown', onDown);
  }, [open]);

  return (
    <div
      className={styles.popWrap}
      ref={wrap}
      onKeyDown={(e) => {
        if (e.key === 'Escape' && open) {
          e.stopPropagation();
          setOpen(false);
          trigger.current?.focus();
        }
      }}
    >
      <button
        ref={trigger}
        type="button"
        className={triggerClassName ?? styles.btn}
        aria-haspopup="listbox"
        aria-expanded={open}
        disabled={disabled}
        title={triggerTitle}
        onClick={() => setOpen((o) => !o)}
      >
        {triggerLabel && (
          <>
            <span className="sr-only">{triggerLabel}:</span>{' '}
          </>
        )}
        {display}
        <IconChevron />
      </button>
      {open && (
        <div className={`${styles.panel} ${align === 'right' ? styles.panelRight : ''}`}>
          <PickerList
            {...list}
            multi={multi}
            onSelect={(o) => {
              onSelect(o);
              if (!multi) {
                setOpen(false);
                trigger.current?.focus();
              }
            }}
          />
        </div>
      )}
    </div>
  );
}

/** The same list in a dialog; the keyboard shortcuts (`s`, `a`, `l`, `p`) use it. */
export function PickerDialog({
  title,
  onClose,
  multi,
  onSelect,
  ...list
}: Omit<ListProps, 'onSelect' | 'ariaLabel'> & {
  title: string;
  onClose: () => void;
  multi?: boolean;
  onSelect: (o: PickerOption) => void;
}) {
  return (
    <Dialog title={title} onClose={onClose} hideHead width={420}>
      <PickerList
        {...list}
        multi={multi}
        ariaLabel={title}
        onSelect={(o) => {
          onSelect(o);
          if (!multi) onClose();
        }}
      />
    </Dialog>
  );
}
