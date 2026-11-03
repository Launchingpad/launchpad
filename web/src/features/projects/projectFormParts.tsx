import { useCallback, useEffect, useRef, useState } from 'react';
import { Floating } from '../../ui/Floating';
import { IconChevron, IconClose, IconPlus } from '../../ui/Icons';
import ui from '../../styles/ui.module.css';
import { formatDay } from './projects';
import styles from './projects.module.css';

/** A pill for a date (LAUN-177). It opens the app's Floating panel with a native date field, as the issue dialog does. */
export function DatePill({
  label,
  value,
  min,
  open,
  onOpenChange,
  onChange,
}: {
  label: string;
  value: string;
  min?: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onChange: (v: string) => void;
}) {
  const [anchor, setAnchor] = useState<HTMLButtonElement | null>(null);
  const trigger = useRef<HTMLButtonElement | null>(null);
  const setTrigger = useCallback((el: HTMLButtonElement | null) => {
    trigger.current = el;
    setAnchor(el);
  }, []);
  const close = (restore: boolean) => {
    if (restore) trigger.current?.focus();
    onOpenChange(false);
  };
  const fieldId = `pill-${label.replace(/\s+/g, '-').toLowerCase()}`;
  return (
    <>
      <button
        ref={setTrigger}
        type="button"
        className={`${ui.chipBtn} ${value ? '' : ui.chipUnset}`}
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-label={value ? `${label}: ${formatDay(value)}` : label}
        onMouseDown={(e) => {
          if (open) e.preventDefault();
        }}
        onClick={() => (open ? close(true) : onOpenChange(true))}
      >
        <span>{value ? formatDay(value) : label}</span>
      </button>
      {open && anchor && (
        <Floating anchor={anchor} label={label} width={240} onClose={close}>
          <DatePanel label={label} fieldId={fieldId} value={value} min={min} onChange={onChange} />
        </Floating>
      )}
    </>
  );
}

function DatePanel({
  label,
  fieldId,
  value,
  min,
  onChange,
}: {
  label: string;
  fieldId: string;
  value: string;
  min?: string;
  onChange: (v: string) => void;
}) {
  const field = useRef<HTMLInputElement>(null);
  // The field takes focus when the panel opens, so typing a date starts at once.
  useEffect(() => field.current?.focus(), []);
  return (
    <div className={styles.datePanel}>
      <label htmlFor={fieldId}>{label}</label>
      <input
        ref={field}
        id={fieldId}
        type="date"
        className={ui.input}
        value={value}
        min={min || undefined}
        onChange={(e) => onChange(e.target.value)}
      />
      <button
        type="button"
        className={`${ui.btn} ${ui.ghost}`}
        disabled={!value}
        onClick={() => {
          onChange('');
          // Focus must not sit on a button that has just disabled itself.
          field.current?.focus();
        }}
      >
        Clear
      </button>
    </div>
  );
}

export interface MilestoneRow {
  key: number;
  name: string;
  date: string;
  /** Created on the server already (the project was made and this one went through). */
  done: boolean;
  error?: string;
}

/**
 * The Milestones card of the New project dialog (LAUN-177): a collapsible card with a "+" button. Rows are made after
 * the project, one by one, by the dialog (there is no route that takes both).
 */
export function MilestonesCard({
  rows,
  onAdd,
  onChange,
  onRemove,
  pending,
}: {
  rows: MilestoneRow[];
  onAdd: () => void;
  onChange: (key: number, patch: Partial<MilestoneRow>) => void;
  onRemove: (key: number) => void;
  /** A save is running: the fields are read-only (not disabled, so focus stays and Esc keeps working). */
  pending?: boolean;
}) {
  const [collapsed, setCollapsed] = useState(false);
  const shown = !collapsed && rows.length > 0;
  // Enter in a name or a date adds the next row. It must not send the form (Cmd/Ctrl+Enter still does).
  const enterAdds = (r: MilestoneRow) => (e: React.KeyboardEvent) => {
    if (e.key === 'Enter' && !e.metaKey && !e.ctrlKey && !e.nativeEvent.isComposing && e.keyCode !== 229) {
      e.preventDefault();
      if (!pending && r.name.trim()) onAdd();
    }
  };
  return (
    <section className={styles.msCard} aria-labelledby="project-ms-h">
      <div className={styles.msCardHead}>
        <button
          type="button"
          className={styles.msToggle}
          aria-expanded={shown}
          aria-controls="project-ms-body"
          disabled={rows.length === 0}
          onClick={() => setCollapsed((c) => !c)}
        >
          <IconChevron className={shown ? styles.msOpen : undefined} aria-hidden="true" />
          <span id="project-ms-h">Milestones{rows.length > 0 ? ` (${rows.length})` : ''}</span>
        </button>
        <button
          type="button"
          className={`${ui.btn} ${ui.ghost} ${ui.icon}`}
          aria-label="Add milestone"
          title="Add milestone"
          aria-disabled={pending || undefined}
          onClick={() => {
            if (pending) return;
            setCollapsed(false);
            onAdd();
          }}
        >
          <IconPlus />
        </button>
      </div>
      <div id="project-ms-body" hidden={!shown}>
        <ul className={styles.msRows}>
          {rows.map((r, i) => (
            <li key={r.key} className={styles.msRow}>
              <div className={styles.msFields}>
                <input
                  className={ui.input}
                  aria-label={`Milestone ${i + 1} name`}
                  placeholder="Milestone name"
                  value={r.name}
                  disabled={r.done}
                  readOnly={pending}
                  maxLength={255}
                  aria-invalid={!!r.error}
                  aria-describedby={r.error ? `ms-${r.key}-error` : undefined}
                  autoComplete="off"
                  data-ms-name={r.key}
                  onChange={(e) => !pending && onChange(r.key, { name: e.target.value, error: undefined })}
                  onKeyDown={enterAdds(r)}
                />
                <input
                  type="date"
                  className={ui.input}
                  aria-label={`Milestone ${i + 1} target date`}
                  value={r.date}
                  disabled={r.done}
                  readOnly={pending}
                  onChange={(e) => !pending && onChange(r.key, { date: e.target.value, error: undefined })}
                  onKeyDown={enterAdds(r)}
                />
                <button
                  type="button"
                  className={`${ui.btn} ${ui.ghost} ${ui.icon}`}
                  aria-label={`Remove milestone ${i + 1}`}
                  aria-disabled={pending || undefined}
                  disabled={r.done}
                  onClick={() => !pending && onRemove(r.key)}
                >
                  <IconClose />
                </button>
              </div>
              {r.done && <span className={ui.hint}>Created</span>}
              {r.error && (
                <span id={`ms-${r.key}-error`} className={styles.fieldError} aria-live="polite">
                  {r.error}
                </span>
              )}
            </li>
          ))}
        </ul>
      </div>
    </section>
  );
}
