import { useEffect, useId, useRef, useState } from 'react';
import type { Group } from '../../lib/group';
import { IconChevron } from '../../ui/Icons';
import { ColumnGlyph } from './ColumnGlyph';
import ui from '../../styles/ui.module.css';
import styles from './issues.module.css';
import { entriesOf, hiddenName, toggleColumn } from './boardColumns';
import type { GroupKey } from './filters';
import { isDefaultColumns, MAX_HIDDEN, type ColumnPrefs } from './viewPrefs';

/**
 * LAUN-163: the board's "Columns" control. A button opens a panel of checkboxes: one per state, "Show empty
 * columns", and Reset. Native checkboxes, so Tab, Space and a screen reader work without extra code; Escape closes
 * and puts focus back on the button.
 */
export function ColumnsControl({
  all,
  prefs,
  onChange,
  kind = 'state',
}: {
  /** Every column of the board, before any is hidden. */
  all: Group[];
  prefs: ColumnPrefs;
  onChange: (p: ColumnPrefs) => void;
  /** LAUN-171: the board's group-by. Each kind keeps its own hidden list. */
  kind?: GroupKey;
}) {
  const [open, setOpen] = useState(false);
  const [say, setSay] = useState('');
  const wrap = useRef<HTMLDivElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const panelId = useId();
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (!wrap.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', onDown);
    return () => document.removeEventListener('mousedown', onDown);
  }, [open]);

  const names = new Set(all.map((g) => hiddenName(kind, g)));
  const isHidden = (g: Group) => prefs.hidden.includes(hiddenName(kind, g));
  const hiddenNow = all.filter(isHidden).length;
  const custom = !isDefaultColumns({
    hideEmpty: prefs.hideEmpty,
    hidden: entriesOf(kind, prefs.hidden).filter((n) => names.has(n)),
  });
  const shownCount = all.length - hiddenNow;
  // The last visible column cannot be hidden: its box is disabled and says why.
  const lastKey = shownCount === 1 ? all.find((g) => !isHidden(g))?.key : undefined;
  const what = {
    state: 'statuses',
    assignee: 'assignees',
    priority: 'priorities',
    label: 'labels',
    project: 'projects',
  }[kind];

  const flip = (col: Group) => {
    const name = col.name;
    const r = toggleColumn(all, prefs, col, kind);
    if (r.refused) {
      setSay(
        r.reason === 'cap' ? `You can hide at most ${MAX_HIDDEN} columns.` : 'At least one column must stay.',
      );
      return;
    }
    onChange(r.prefs);
    const nowHidden = r.prefs.hidden.includes(hiddenName(kind, col));
    const left = all.length - all.filter((g) => r.prefs.hidden.includes(hiddenName(kind, g))).length;
    setSay(`${name} column ${nowHidden ? 'hidden' : 'shown'}. ${left} of ${all.length} columns chosen.`);
  };

  return (
    <div
      className={`${ui.popWrap} ${styles.columnsWrap}`}
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
        className={ui.btn}
        aria-expanded={open}
        aria-controls={open ? panelId : undefined}
        onClick={() => setOpen((o) => !o)}
      >
        Columns{custom ? ' (changed)' : ''}
        <IconChevron />
      </button>
      {open && (
        <div
          id={panelId}
          role="group"
          data-no-hotkeys=""
          aria-label="Board columns"
          className={`${ui.panel} ${ui.panelRight} ${styles.columnsPanel}`}
        >
          <label className={styles.columnsRow}>
            <input
              type="checkbox"
              checked={!prefs.hideEmpty}
              onChange={(e) => {
                onChange({ ...prefs, hideEmpty: !e.target.checked });
                setSay(e.target.checked ? 'Empty columns shown.' : 'Empty columns hidden.');
              }}
            />
            <span>Show empty columns</span>
          </label>
          <div className={styles.columnsHead} aria-hidden="true">
            Show these {what}
          </div>
          {all.map((g) => (
            <label key={g.key} className={styles.columnsRow}>
              <input
                type="checkbox"
                checked={!isHidden(g)}
                disabled={lastKey === g.key}
                aria-label={`Show ${g.name}, ${g.issues.length} ${g.issues.length === 1 ? 'issue' : 'issues'}`}
                aria-describedby={lastKey === g.key ? `${panelId}-last` : undefined}
                onChange={() => flip(g)}
              />
              <ColumnGlyph group={g} />
              <span className={styles.columnsName}>{g.name}</span>
              <span className={styles.count}>{g.issues.length}</span>
            </label>
          ))}
          {lastKey && (
            <p id={`${panelId}-last`} className={ui.hint}>
              At least one column must stay.
            </p>
          )}
          <button
            type="button"
            className={`${ui.btn} ${ui.ghost}`}
            disabled={!custom}
            onClick={() => {
              // Only this group kind's choices go: the other kinds keep theirs.
              onChange({
                ...prefs,
                hideEmpty: false,
                hidden: prefs.hidden.filter((n) => !entriesOf(kind, prefs.hidden).includes(n)),
              });
              setSay('Columns reset. All columns shown.');
            }}
          >
            Reset columns
          </button>
        </div>
      )}
      <div className="sr-only" aria-live="polite" aria-atomic="true" data-testid="columns-live">
        {say}
      </div>
    </div>
  );
}
